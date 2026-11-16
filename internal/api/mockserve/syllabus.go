// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Course materials of the mock (SPEC §8, §9.3; v1.1 A8 wizard and the
// course page): the syllabus (raw text, syllabus_format, SYLLABUS.md), the
// per-course glossary (list, add, edit, enable, delete), slides (upload,
// binding, delete, stage actions), course create/patch with the server's
// validation, and a simulated background term extraction (glossary_seed
// after saving SYLLABUS.md, material_extract → material_terms after a slide
// upload) so the wizard's last step and the course page show it progressing.
//
// Behaviour and messages follow internal/api/handlers_syllabus.go and
// handlers_course.go. Nothing is written to disk; restart the server to get
// the fixtures back. Simulations: a syllabus file name containing "fail"
// answers 500, "scan" a PDF without text (422); raw text containing
// "mock-format-fail" makes syllabus/format fail (502); a slide whose file
// name contains "corrupt" fails material_extract once (the retry works);
// under ?mock=error the glossary and SYLLABUS.md reads answer 500 (the
// course page's in-card error states); under ?mock=slow the glossary comes
// after the rest of the course page (its own skeleton).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/api"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
)

// sylTiming is how long the simulated work takes (tests shorten it).
var sylTiming = struct {
	format        time.Duration // POST syllabus/format (A8.1 step 3 "正在整理…")
	queued, run   time.Duration // glossary_seed: queued → running → done
	matQueued     time.Duration // a slide's stage waits this long in the queue
	extract, term time.Duration // material_extract, then material_terms
	slowTerms     time.Duration // ?mock=slow: the glossary arrives this much after the rest
	reviewQueued  time.Duration // v2 glossary_review: queued → running
	review        time.Duration // v2 glossary_review: running → done (janitor.go)
}{2 * time.Second, 1500 * time.Millisecond, 5 * time.Second, 1500 * time.Millisecond, 2 * time.Second, 4 * time.Second, 1500 * time.Millisecond,
	1500 * time.Millisecond, 4 * time.Second}

const (
	maxSyllabusBytes = 8 << 20
	syllabusBusy     = "正在从当前大纲提取术语，请等它结束后再替换大纲"
	courseNotFound   = "找不到这门课程（可能已被删除）"
	termNotFound     = "找不到这个术语（可能已被删除）"
	materialNotFound = "找不到这份课件（可能已被删除）"
)

// sylCourse is the per-course state the fixtures do not hold.
type sylCourse struct {
	filename string // name of the raw text's source (syllabus.raw.name)
	raw      string // syllabus.raw.txt
	markdown string // SYLLABUS.md
	terms    []api.Term
	gen      int             // bumps on every re-run, so stale simulation steps are dropped
	failed   map[string]bool // slides whose simulated extraction already failed once
	// v2 glossary janitor (janitor.go)
	reports     map[int]model.JanitorReport // reports that changed something, by glossary version
	latest      int                         // version of the latest of them (0 = none)
	lastRun     *model.JanitorReport        // the latest run, also one without changes
	reviewGen   int                         // bumps when a review run is replaced or cancelled
	reviewDirty bool                        // terms arrived during a run: run once more
}

type sylWorld struct {
	courses map[string]*sylCourse
	seq     int
}

var sylWorlds sync.Map // *store → *sylWorld

// syl returns the store's syllabus world; must be called with s.mu held.
// On first use every fixture course gets its syllabus text and glossary.
// The fixtures' term_count already equals the glossary's length
// (TestFixtureTermCounts); syllabus.chars is set to the length of the
// generated text, as the server reports the length of what it stores.
func (s *store) syl() *sylWorld {
	if v, ok := sylWorlds.Load(s); ok {
		return v.(*sylWorld)
	}
	w := &sylWorld{courses: map[string]*sylCourse{}}
	for i := range s.courses {
		c := &s.courses[i]
		sc := &sylCourse{terms: fixtureTerms(c)}
		if c.Syllabus != nil {
			sc.filename = c.Syllabus.Filename
			sc.raw = fixtureRaw(*c, s.lectures)
			if c.Syllabus.Formatted {
				sc.markdown = formatSyllabusMD(*c, sc.raw)
				c.Syllabus.Chars = utf8.RuneCountInString(sc.markdown)
			} else {
				c.Syllabus.Chars = utf8.RuneCountInString(sc.raw)
			}
		}
		w.courses[c.ID] = sc
		for _, rep := range s.reports {
			if rep.CourseID == c.ID {
				sc.storeRun(rep)
			}
		}
		for _, run := range s.lastRuns {
			if run.CourseID == c.ID {
				r := run
				sc.lastRun = &r
			}
		}
		s.refreshJanitor(c, sc)
	}
	sylWorlds.Store(s, w)
	return w
}

func (s *store) sylCourse(id string) (*api.Course, *sylCourse, error) {
	c, err := s.findCourse(id)
	if err != nil {
		return nil, nil, &httpError{status: 404, msg: courseNotFound}
	}
	w := s.syl()
	sc := w.courses[id]
	if sc == nil {
		sc = &sylCourse{terms: []api.Term{}}
		w.courses[id] = sc
	}
	return c, sc, nil
}

func (m *mock) registerSyllabus(mux *http.ServeMux) {
	a := m.api
	for _, s := range []*store{m.main, m.empty} {
		if s != nil {
			s.mu.Lock()
			s.syl()
			s.mu.Unlock()
		}
	}
	mux.HandleFunc("POST /api/courses", a(m.createCourseChecked))
	mux.HandleFunc("PATCH /api/courses/{id}", a(m.patchCourseChecked))
	mux.HandleFunc("GET /api/courses/{id}/syllabus", a(m.getSyllabus))
	mux.HandleFunc("POST /api/courses/{id}/syllabus/raw", a(m.syllabusRaw))
	mux.HandleFunc("POST /api/courses/{id}/syllabus/format", a(m.formatSyllabus))
	mux.HandleFunc("PUT /api/courses/{id}/syllabus", a(m.saveSyllabus))
	mux.HandleFunc("POST /api/courses/{id}/syllabus", a(m.legacySyllabus))
	mux.HandleFunc("POST /api/courses/{id}/stages/{stage}/{action}", a(m.courseStageAction))
	mux.HandleFunc("GET /api/courses/{id}/terms", a(m.listTerms))
	mux.HandleFunc("POST /api/courses/{id}/terms", a(m.createTerm))
	mux.HandleFunc("PATCH /api/terms/{id}", a(m.patchTerm))
	mux.HandleFunc("DELETE /api/terms/{id}", a(m.deleteTerm))
	mux.HandleFunc("POST /api/courses/{id}/materials", a(m.uploadMaterial))
	mux.HandleFunc("PATCH /api/materials/{id}", a(m.patchMaterial))
	mux.HandleFunc("DELETE /api/materials/{id}", a(m.deleteMaterial))
	mux.HandleFunc("POST /api/materials/{id}/stages/{stage}/{action}", a(m.materialStageAction))
}

// ---------- courses (A8.1 step 1) ----------

func courseName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	switch {
	case name == "":
		return "", errBad("课程名称不能为空")
	case utf8.RuneCountInString(name) > 200:
		return "", errBad("课程名称太长（最多 200 个字符）")
	}
	return name, nil
}

func courseCode(raw string) error {
	code := strings.TrimSpace(raw)
	switch {
	case utf8.RuneCountInString(code) > 32:
		return errBad("课程代码太长（最多 32 个字符）")
	case strings.IndexFunc(code, unicode.IsSpace) >= 0:
		return errBad("课程代码不能包含空格（它会出现在引用标记中，例如 CSC1003 L03@12:34）")
	}
	return nil
}

func (m *mock) createCourseChecked(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.CourseCreate
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := courseName(in.Name); err != nil {
		writeErr(w, err)
		return
	}
	if in.Code != nil {
		if err := courseCode(*in.Code); err != nil {
			writeErr(w, err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.createCourse(in, time.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	s.syl().courses[c.ID] = &sylCourse{terms: []api.Term{}}
	writeJSON(w, http.StatusCreated, c)
}

func (m *mock) patchCourseChecked(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.CoursePatch
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Name == nil && in.Code == nil && in.TermStart == nil && in.SkipWeeks == nil && in.Numbering == nil {
		writeError(w, http.StatusBadRequest, "没有要修改的内容（name、code、term_start 或 skip_weeks）")
		return
	}
	if in.Name != nil {
		if _, err := courseName(*in.Name); err != nil {
			writeErr(w, err)
			return
		}
	}
	if in.Code != nil {
		if err := courseCode(*in.Code); err != nil {
			writeErr(w, err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.findCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// v2 P13.4: term start and skipped weeks (409 RenumberConflict first,
	// so nothing is saved without the confirmation; course_p2.go)
	if err := s.patchCalendar(c, in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Name != nil || in.Code != nil {
		if c, err = s.patchCourse(c.ID, in); err != nil {
			writeErr(w, err)
			return
		}
	}
	// lecture kinds (kinds.go): the numbering mode only changes what is shown
	if err := patchNumbering(c, in.Numbering); err != nil {
		writeErr(w, err)
		return
	}
	if in.TermStart != nil || in.SkipWeeks != nil || in.Numbering != nil {
		s.fnChanged(c) // every lecture number may have changed
	}
	writeJSON(w, http.StatusOK, c)
}

// ---------- syllabus (A8.1 steps 2–3, A8.2) ----------

// errorScenario answers the course page's optional reads with 500 under
// ?mock=error, so their in-card error states can be previewed.
func errorScenario(w http.ResponseWriter, r *http.Request) bool {
	if scenario(r) != "error" {
		return false
	}
	writeError(w, http.StatusInternalServerError, "服务器内部错误：mock 模拟的失败（?mock=error）")
	return true
}

func (m *mock) getSyllabus(w http.ResponseWriter, r *http.Request, s *store) {
	if errorScenario(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SyllabusDoc{Filename: sc.filename, Raw: sc.raw, Markdown: sc.markdown})
}

// readSyllabusUpload reads the multipart "file" (.txt/.md/.pdf). A PDF
// cannot be extracted in the mock: it yields the course's sample text.
func readSyllabusUpload(r *http.Request) (name string, data []byte, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, errBad("无法解析上传内容：" + err.Error())
	}
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, errBad("无法解析上传内容：" + err.Error())
		}
		if p.FormName() != "file" || p.FileName() == "" {
			_, _ = io.Copy(io.Discard, p)
			continue
		}
		name = p.FileName()
		ext := strings.ToLower(path.Ext(name))
		if ext != ".txt" && ext != ".md" && ext != ".pdf" {
			return "", nil, errBad(fmt.Sprintf("大纲只支持 .txt、.md、.pdf 文件（收到：%s）", name))
		}
		data, err = io.ReadAll(io.LimitReader(p, maxSyllabusBytes+1))
		if err != nil {
			return "", nil, errBad("上传中断：" + err.Error())
		}
		if len(data) > maxSyllabusBytes {
			return "", nil, &httpError{status: http.StatusRequestEntityTooLarge, msg: "大纲文件太大（最多 8 MB 文字）"}
		}
	}
	if name == "" {
		return "", nil, errBad("缺少文件")
	}
	return name, data, nil
}

// POST /api/courses/{id}/syllabus/raw: multipart file or JSON {text} → SyllabusDoc.
func (m *mock) syllabusRaw(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	c, _, err := s.sylCourse(r.PathValue("id"))
	var sample string // what "extracting" a PDF yields in the mock
	if err == nil {
		sample = fixtureRaw(*c, s.lectures)
	}
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	var name, raw string
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		fn, data, err := readSyllabusUpload(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if simulatedFailure(fn) {
			writeError(w, http.StatusInternalServerError, "mock：模拟的上传失败（文件名包含 fail）")
			return
		}
		name, raw = fn, strings.ToValidUTF8(string(data), "�")
		if strings.HasSuffix(strings.ToLower(fn), ".pdf") {
			raw = sample
		}
		if strings.Contains(strings.ToLower(fn), "scan") || strings.TrimSpace(raw) == "" {
			writeError(w, http.StatusUnprocessableEntity, "文件中没有可提取的文字（扫描版 PDF 没有文字层），请改传 .txt/.md 或直接粘贴文本")
			return
		}
	} else {
		var in struct {
			api.SyllabusRawInput
			Path string `json:"path"` // desktop: a local file (SPEC-v3 V1.4)
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, maxSyllabusBytes+1))
		if err != nil || json.Unmarshal(b, &in) != nil {
			writeError(w, http.StatusBadRequest, "请求格式错误：需要 multipart 文件或 JSON {text}")
			return
		}
		name, raw = "pasted.txt", in.Text
		if in.Path != "" {
			// The mock reads no local file: every path "contains" the sample.
			name, raw = pathBase(in.Path), sample
			if simulatedFailure(name) {
				writeError(w, http.StatusInternalServerError, "mock：模拟的复制失败（文件名包含 fail）")
				return
			}
		}
		if strings.TrimSpace(raw) == "" {
			writeError(w, http.StatusBadRequest, "大纲内容为空")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	sc.filename, sc.raw = name, raw
	if c.Syllabus != nil { // like the server: the row only changes its file name until PUT
		c.Syllabus.Filename = name
	}
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
	writeJSON(w, http.StatusOK, api.SyllabusDoc{Filename: sc.filename, Raw: sc.raw, Markdown: sc.markdown})
}

// POST /api/courses/{id}/syllabus/format → SyllabusFormatResult (not saved).
func (m *mock) formatSyllabus(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	var course api.Course
	var raw string
	if err == nil {
		course, raw = *c, sc.raw
	}
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeError(w, http.StatusConflict, "还没有大纲原文：请先上传文件或粘贴文本")
		return
	}
	prov, err := m.syllabusRole(r, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	select {
	case <-time.After(sylTiming.format):
	case <-r.Context().Done():
		return
	}
	if strings.Contains(raw, "mock-format-fail") {
		writeError(w, http.StatusBadGateway, "大纲整理失败：mock 模拟的模型错误（原文包含 mock-format-fail）")
		return
	}
	md := formatSyllabusMD(course, raw)
	u := api.UsageStats{PromptTokens: int64(utf8.RuneCountInString(raw)/4 + 190), CompletionTokens: int64(utf8.RuneCountInString(md) / 4)}
	if prov.PriceInput != nil && prov.PriceOutput != nil {
		cost := round4((float64(u.PromptTokens)**prov.PriceInput + float64(u.CompletionTokens)**prov.PriceOutput) / 1e6)
		u.EstimatedCost = &cost
	}
	writeJSON(w, http.StatusOK, api.SyllabusFormatResult{Markdown: md, Usage: u})
}

// syllabusRole is the provider of the syllabus_format role, or the
// server's 409 when the role is not configured (scenario "unset").
func (m *mock) syllabusRole(r *http.Request, s *store) (api.ProviderView, error) {
	st, err := m.settingsStore(r, s)
	if err != nil {
		return api.ProviderView{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, rv := range st.settings.Roles {
		if rv.Role != model.RoleSyllabusFormat {
			continue
		}
		if rv.ProviderID == nil || rv.Model == nil {
			return api.ProviderView{}, errConflict("请先在设置中为「大纲整理」选择提供商和模型")
		}
		p, ok := st.providerByID(*rv.ProviderID)
		if !ok {
			return api.ProviderView{}, errConflict("「大纲整理」使用的提供商已删除，请先在设置中为它重新选择提供商和模型")
		}
		return p, nil
	}
	return api.ProviderView{}, nil
}

// PUT /api/courses/{id}/syllabus {markdown} → Course; glossary_seed runs again.
func (m *mock) saveSyllabus(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.SyllabusSaveInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Markdown) == "" {
		writeError(w, http.StatusBadRequest, "大纲内容不能为空")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if stageOf(c.Stages, model.StageGlossarySeed) == model.StatusRunning {
		writeError(w, http.StatusConflict, syllabusBusy)
		return
	}
	sc.markdown = in.Markdown
	name := sc.filename
	if name == "" {
		name = "SYLLABUS.md"
	}
	c.Syllabus = &api.Syllabus{Filename: name, Chars: utf8.RuneCountInString(in.Markdown), Formatted: true}
	s.startGlossarySeed(c, sc)
	writeJSON(w, http.StatusOK, c)
}

// POST /api/courses/{id}/syllabus (v1, kept): the file's text becomes the
// raw text and, unformatted, the syllabus; glossary_seed runs again. An
// unknown course answers with the v1 example (as before).
func (m *mock) legacySyllabus(w http.ResponseWriter, r *http.Request, s *store) {
	fn, data, err := readSyllabusUpload(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if simulatedFailure(fn) {
		writeError(w, http.StatusInternalServerError, "mock：模拟的上传失败（文件名包含 fail）")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		var out api.Course
		if err := m.readExample("course.json", &out); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if stageOf(c.Stages, model.StageGlossarySeed) == model.StatusRunning {
		writeError(w, http.StatusConflict, syllabusBusy)
		return
	}
	raw := strings.ToValidUTF8(string(data), "�")
	if strings.HasSuffix(strings.ToLower(fn), ".pdf") {
		raw = fixtureRaw(*c, s.lectures)
	}
	sc.filename, sc.raw, sc.markdown = fn, raw, ""
	c.Syllabus = &api.Syllabus{Filename: fn, Chars: utf8.RuneCountInString(raw)}
	s.startGlossarySeed(c, sc)
	writeJSON(w, http.StatusOK, c)
}

// ---------- simulated background work ----------

func stageOf(stages []api.StageView, stage string) string {
	for _, st := range stages {
		if st.Stage == stage {
			return st.Status
		}
	}
	return ""
}

func setStage(stages []api.StageView, stage, status string, progress float64, now time.Time) []api.StageView {
	i := -1
	for k := range stages {
		if stages[k].Stage == stage {
			i = k
		}
	}
	if i < 0 {
		stages = append(stages, api.StageView{Stage: stage})
		i = len(stages) - 1
	}
	st := &stages[i]
	t := now.UTC().Format(time.RFC3339)
	switch status {
	case model.StatusQueued, model.StatusPending:
		resetStage(st, status)
		if status == model.StatusQueued {
			wf := model.WaitAPISlot
			if stage == model.StageMaterialExtract {
				wf = model.WaitCPUSlot
			}
			st.WaitingFor = &wf
		}
	case model.StatusRunning:
		st.Status, st.WaitingFor, st.Error = status, nil, nil
		if st.StartedAt == nil {
			st.StartedAt = &t
		}
	default:
		st.Status, st.WaitingFor = status, nil
		st.FinishedAt = &t
		if st.StartedAt != nil {
			if t0, err := time.Parse(time.RFC3339, *st.StartedAt); err == nil {
				d := now.Sub(t0).Milliseconds()
				st.DurationMS = &d
			}
		}
	}
	st.Progress = progress
	st.DisplayStatus = st.Status
	st.AllowedActions = stageActions(st.Status)
	return stages
}

// after runs fn under s.mu after d unless the course's generation changed.
func (s *store) after(d time.Duration, sc *sylCourse, gen int, fn func(now time.Time)) {
	time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sc.gen == gen {
			fn(time.Now())
		}
	})
}

// startGlossarySeed queues glossary_seed and simulates it: queued →
// running → done, merging the "## Key Terms" of SYLLABUS.md (or the raw
// text's topics) into the glossary with source syllabus. Must hold s.mu.
func (s *store) startGlossarySeed(c *api.Course, sc *sylCourse) {
	sc.gen++
	gen, id := sc.gen, c.ID
	now := time.Now()
	c.Stages = setStage(c.Stages, model.StageGlossarySeed, model.StatusQueued, 0, now)
	s.courseChanged(c)
	s.after(sylTiming.queued, sc, gen, func(now time.Time) {
		if c, err := s.findCourse(id); err == nil && stageOf(c.Stages, model.StageGlossarySeed) == model.StatusQueued {
			c.Stages = setStage(c.Stages, model.StageGlossarySeed, model.StatusRunning, 0.15, now)
			s.courseChanged(c)
		}
	})
	s.after(sylTiming.queued+sylTiming.run, sc, gen, func(now time.Time) {
		c, err := s.findCourse(id)
		if err != nil || stageOf(c.Stages, model.StageGlossarySeed) != model.StatusRunning {
			return
		}
		src := sc.markdown
		if src == "" {
			src = sc.raw
		}
		c.Stages = setStage(c.Stages, model.StageGlossarySeed, model.StatusDone, 1, now)
		s.mergeTerms(c, sc, syllabusKeyTerms(src), model.SourceSyllabus)
		s.courseChanged(c)
	})
}

// startMaterial simulates a slide's stages from `from` on: material_extract
// → material_terms for a new upload, or material_terms alone on its retry.
func (s *store) startMaterial(c *api.Course, sc *sylCourse, mid, from string) {
	gen, id := sc.gen, c.ID
	step := func(d time.Duration, fn func(c *api.Course, m *api.Material, now time.Time)) {
		s.after(d, sc, gen, func(now time.Time) {
			c, err := s.findCourse(id)
			if err != nil {
				return
			}
			for i := range c.Materials {
				if c.Materials[i].ID == mid {
					fn(c, &c.Materials[i], now)
					c.Materials[i].CanDelete = !materialRunning(c.Materials[i])
					s.courseChanged(c)
				}
			}
		})
	}
	termsDone := func(c *api.Course, m *api.Material, now time.Time) {
		if stageOf(m.Stages, model.StageMaterialTerms) != model.StatusRunning {
			return
		}
		m.Stages = setStage(m.Stages, model.StageMaterialTerms, model.StatusDone, 1, now)
		s.mergeTerms(c, sc, materialTerms(*c, m.Filename), model.SourceMaterial)
	}
	q := sylTiming.matQueued
	if from == model.StageMaterialTerms {
		step(q, func(c *api.Course, m *api.Material, now time.Time) {
			if stageOf(m.Stages, model.StageMaterialTerms) == model.StatusQueued {
				m.Stages = setStage(m.Stages, model.StageMaterialTerms, model.StatusRunning, 0.2, now)
			}
		})
		step(q+sylTiming.term, termsDone)
		return
	}
	step(q, func(c *api.Course, m *api.Material, now time.Time) {
		if stageOf(m.Stages, model.StageMaterialExtract) == model.StatusQueued {
			m.Stages = setStage(m.Stages, model.StageMaterialExtract, model.StatusRunning, 0.3, now)
		}
	})
	step(q+sylTiming.extract, func(c *api.Course, m *api.Material, now time.Time) {
		if stageOf(m.Stages, model.StageMaterialExtract) != model.StatusRunning {
			return
		}
		if strings.Contains(strings.ToLower(m.Filename), "corrupt") && !sc.failed[mid] {
			if sc.failed == nil {
				sc.failed = map[string]bool{}
			}
			sc.failed[mid] = true
			m.Stages = setStage(m.Stages, model.StageMaterialExtract, model.StatusFailed, 0.3, now)
			msg := "exit status 1: pdfminer.pdfparser.PDFSyntaxError: No /Root object! - Is this really a PDF? (mock: the file name contains corrupt; a retry succeeds)"
			m.Stages[0].Error = &msg
			m.Stages[0].ErrorInfo = &model.ErrorInfo{Code: "pdf.worker_failed", Params: map[string]any{"exit_code": 1}, Detail: msg}
			return
		}
		m.Pages = 12 + int(hashOf(m.Filename)%29)
		m.Stages = setStage(m.Stages, model.StageMaterialExtract, model.StatusDone, 1, now)
		m.Stages = setStage(m.Stages, model.StageMaterialTerms, model.StatusRunning, 0.2, now)
	})
	step(400*time.Millisecond+sylTiming.extract+sylTiming.term, termsDone)
}

func materialRunning(m api.Material) bool {
	for _, st := range m.Stages {
		if st.Status == model.StatusRunning {
			return true
		}
	}
	return false
}

// courseChanged recomputes the derived fields and tells the front end.
func (s *store) courseChanged(c *api.Course) {
	finalizeCourse(c, s.lectures, s.kctx())
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
}

// mergeTerms is SPEC §6.0.1 MergeTerms, simplified: new terms are inserted,
// an existing term only gets a missing zh, manual terms are never touched.
func (s *store) mergeTerms(c *api.Course, sc *sylCourse, found []api.Term, source string) {
	changed := false
	for _, t := range found {
		key := strings.ToLower(t.En)
		i := -1
		for k := range sc.terms {
			if strings.ToLower(sc.terms[k].En) == key {
				i = k
			}
		}
		switch {
		case i < 0:
			t.ID, t.Source, t.Enabled = s.newID("01JTMOCK"), source, true
			t.Tier, t.ZhSource, t.CommonWord = model.TierEnforce, model.ZhSourceExtractor, mockIsCommon(t.En)
			sc.terms = append(sc.terms, t)
			changed = true
		case sc.terms[i].Source != model.SourceManual && sc.terms[i].Zh == "" && t.Zh != "":
			sc.terms[i].Zh = t.Zh
			changed = true
		}
	}
	// v2 ruling 5: without a janitor the G2 rule runs in the same
	// transaction (one version bump for both); with one, ruling 4: a
	// glossary_review is queued whether or not the merge changed anything.
	_, _, configured := s.janitorTarget()
	var items []model.JanitorItem
	if !configured {
		from := c.GlossaryVersion
		items, _ = applyJanitorRule(sc, true)
		if len(items) > 0 {
			changed = true
			sc.storeRun(model.JanitorReport{CourseID: c.ID, FromVersion: from, Version: from + 1, CreatedAt: time.Now().UTC().Format(time.RFC3339),
				Fallback: true, Note: glossary.NoteFallbackMerge, NoteInfo: glossary.NoteInfo(glossary.CodeNoteFallbackMerge, nil), Items: items})
		}
	}
	if changed {
		sortTerms(sc.terms)
		c.GlossaryVersion++
		c.TermCount = len(sc.terms)
		s.notify(api.ChangedEvent{Kind: "terms", ID: c.ID})
	}
	s.refreshJanitor(c, sc)
	if configured {
		s.startReview(c, sc)
	}
}

func sortTerms(ts []api.Term) {
	sort.SliceStable(ts, func(i, j int) bool { return strings.ToLower(ts[i].En) < strings.ToLower(ts[j].En) })
}

// ---------- course and slide stage actions ----------

func checkAction(st []api.StageView, stage, action string) (string, error) {
	status := stageOf(st, stage)
	if status == "" {
		return "", errBad("未知的阶段或操作")
	}
	for _, a := range stageActions(status) {
		if a == action {
			return status, nil
		}
	}
	return "", errConflict(fmt.Sprintf("%s当前为 %s，不能执行 %s", model.StageLabel(stage), status, action))
}

// POST /api/courses/{id}/stages/{glossary_seed|glossary_review}/{action} → Course.
func (m *mock) courseStageAction(w http.ResponseWriter, r *http.Request, s *store) {
	stage, action := r.PathValue("stage"), r.PathValue("action")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if stage != model.StageGlossarySeed && stage != model.StageGlossaryReview {
		writeError(w, http.StatusBadRequest, "未知的阶段或操作")
		return
	}
	if _, err := checkAction(c.Stages, stage, action); err != nil {
		writeErr(w, err)
		return
	}
	if stage == model.StageGlossaryReview { // janitor.go
		s.reviewStageAction(c, sc, action)
		writeJSON(w, http.StatusOK, c)
		return
	}
	if action == model.ActionCancel {
		sc.gen++
		c.Stages = setStage(c.Stages, stage, model.StatusCancelled, 0, time.Now())
		s.courseChanged(c)
	} else {
		s.startGlossarySeed(c, sc)
		bumpAttempt(c.Stages, stage, action)
	}
	writeJSON(w, http.StatusOK, c)
}

// bumpAttempt: a retry continues the same job (attempt + 1, SPEC §4.6), a
// rerun starts a new one.
func bumpAttempt(stages []api.StageView, stage, action string) {
	for i := range stages {
		if stages[i].Stage == stage {
			if action == model.ActionRetry {
				stages[i].Attempt++
			} else {
				stages[i].Attempt = 0
			}
		}
	}
}

func (s *store) findMaterial(id string) (*api.Course, *api.Material, error) {
	for i := range s.courses {
		for j := range s.courses[i].Materials {
			if s.courses[i].Materials[j].ID == id {
				return &s.courses[i], &s.courses[i].Materials[j], nil
			}
		}
	}
	return nil, nil, &httpError{status: 404, msg: materialNotFound}
}

// POST /api/materials/{id}/stages/{stage}/{action} → Material.
func (m *mock) materialStageAction(w http.ResponseWriter, r *http.Request, s *store) {
	stage, action := r.PathValue("stage"), r.PathValue("action")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, mat, err := s.findMaterial(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if stage != model.StageMaterialExtract && stage != model.StageMaterialTerms {
		writeError(w, http.StatusBadRequest, "未知的阶段或操作")
		return
	}
	if _, err := checkAction(mat.Stages, stage, action); err != nil {
		writeErr(w, err)
		return
	}
	_, sc, _ := s.sylCourse(c.ID)
	now := time.Now()
	if action == model.ActionCancel {
		mat.Stages = setStage(mat.Stages, stage, model.StatusCancelled, 0, now)
	} else {
		if stage == model.StageMaterialExtract {
			mat.Stages = setStage(mat.Stages, model.StageMaterialExtract, model.StatusQueued, 0, now)
			mat.Stages = setStage(mat.Stages, model.StageMaterialTerms, model.StatusPending, 0, now)
			wf := model.WaitUpstream(model.StageMaterialExtract)
			mat.Stages[1].WaitingFor = &wf
		} else {
			mat.Stages = setStage(mat.Stages, model.StageMaterialTerms, model.StatusQueued, 0, now)
		}
		bumpAttempt(mat.Stages, stage, action)
		s.startMaterial(c, sc, mat.ID, stage)
	}
	mat.CanDelete = !materialRunning(*mat)
	out := *mat
	s.courseChanged(c)
	writeJSON(w, http.StatusOK, out)
}

// ---------- slides (§9.3; NOTES.md B, E) ----------

// POST /api/courses/{id}/materials: multipart file (PDF), optional lecture_id → Course.
func (m *mock) uploadMaterial(w http.ResponseWriter, r *http.Request, s *store) {
	fields, filename, _, err := drain(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无法解析上传内容："+err.Error())
		return
	}
	if simulatedFailure(filename) {
		writeError(w, http.StatusInternalServerError, "mock：模拟的上传失败（文件名包含 fail）")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		// unknown course: answer with the v1 example, like before
		var out api.Course
		if err := m.readExample("course.json", &out); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if !strings.EqualFold(path.Ext(filename), ".pdf") {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("课件只支持 PDF 文件（收到：%s）", filename))
		return
	}
	var lid *string
	if v := strings.TrimSpace(fields["lecture_id"]); v != "" {
		if err := s.checkLectureInCourse(v, c.ID); err != nil {
			writeErr(w, err)
			return
		}
		lid = &v
	}
	now := time.Now()
	mat := api.Material{ID: s.newID("01JMMOCK"), CourseID: c.ID, LectureID: lid, Filename: filename, CreatedAt: now.UTC().Format(time.RFC3339), Stages: []api.StageView{}}
	mat.Bind = uploadBind(c, lid, filename) // lecture kinds (kinds.go): the week in the file name, with a term start
	mat.Stages = setStage(mat.Stages, model.StageMaterialExtract, model.StatusQueued, 0, now)
	mat.Stages = setStage(mat.Stages, model.StageMaterialTerms, model.StatusPending, 0, now)
	wf := model.WaitUpstream(model.StageMaterialExtract)
	mat.Stages[1].WaitingFor = &wf
	mat.CanDelete = true
	c.Materials = append(c.Materials, mat)
	s.startMaterial(c, sc, mat.ID, model.StageMaterialExtract)
	s.courseChanged(c)
	writeJSON(w, http.StatusCreated, c)
}

func (s *store) checkLectureInCourse(lectureID, courseID string) error {
	l, err := s.findLecture(lectureID)
	if err != nil || l.CourseID != courseID {
		return errBad("要绑定的讲座不属于这门课程")
	}
	return nil
}

// PATCH /api/materials/{id} {lecture_id: string|null} → Material.
// lecture kinds (kinds.go): or {bind: {scope, week, lecture_id}}.
func (m *mock) patchMaterial(w http.ResponseWriter, r *http.Request, s *store) {
	var req map[string]json.RawMessage
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if rawBind, ok := req["bind"]; ok {
		var b api.MaterialBind
		if err := json.Unmarshal(rawBind, &b); err != nil {
			writeErr(w, codedErr(http.StatusBadRequest, "material.bind_invalid", nil, err.Error()))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		c, mat, err := s.findMaterial(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		bind, err := s.parseBind(c, b)
		if err != nil {
			writeErr(w, err)
			return
		}
		mat.Bind = bind
		s.courseChanged(c)
		for i := range s.lectures {
			if s.lectures[i].CourseID == c.ID {
				s.notify(api.ChangedEvent{Kind: "lecture", ID: s.lectures[i].ID}) // their slides changed
			}
		}
		writeJSON(w, http.StatusOK, *mat)
		return
	}
	raw, ok := req["lecture_id"]
	if !ok {
		writeErr(w, codedErr(http.StatusBadRequest, "material.bind_missing", nil, "缺少适用范围：需要 bind，或 v1 的 lecture_id（整门课通用时传 null）"))
		return
	}
	var lid *string
	if err := json.Unmarshal(raw, &lid); err != nil {
		writeError(w, http.StatusBadRequest, "lecture_id 必须是字符串或 null")
		return
	}
	if lid != nil && strings.TrimSpace(*lid) == "" {
		lid = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, mat, err := s.findMaterial(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if lid != nil {
		if err := s.checkLectureInCourse(*lid, c.ID); err != nil {
			writeErr(w, err)
			return
		}
	}
	mat.LectureID = lid
	mat.Bind = defaultBind(lid, "") // the v1 body: one lecture or the whole course
	s.courseChanged(c)
	out := *mat
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/materials/{id} → 204 (409 while a stage runs).
func (m *mock) deleteMaterial(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, mat, err := s.findMaterial(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if materialRunning(*mat) {
		writeError(w, http.StatusConflict, "这份课件正在处理，不能删除；可以先取消，或等它结束")
		return
	}
	for i := range c.Materials {
		if c.Materials[i].ID == mat.ID {
			c.Materials = append(c.Materials[:i], c.Materials[i+1:]...)
			break
		}
	}
	s.courseChanged(c)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- glossary (SPEC §8; every change bumps glossary_version) ----------

func (m *mock) listTerms(w http.ResponseWriter, r *http.Request, s *store) {
	if errorScenario(w, r) {
		return
	}
	if scenario(r) == "slow" {
		select {
		case <-time.After(sylTiming.slowTerms):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc.terms)
}

func (s *store) termsChanged(c *api.Course, sc *sylCourse) {
	c.GlossaryVersion++
	c.TermCount = len(sc.terms)
	finalizeCourse(c, s.lectures, s.kctx())
	s.notify(api.ChangedEvent{Kind: "terms", ID: c.ID}, api.ChangedEvent{Kind: "course", ID: c.ID})
}

func (m *mock) createTerm(w http.ResponseWriter, r *http.Request, s *store) {
	var req struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	en := strings.Join(strings.Fields(req.En), " ")
	if en == "" {
		writeError(w, http.StatusBadRequest, "英文术语不能为空")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, t := range sc.terms {
		if strings.EqualFold(t.En, en) {
			writeError(w, http.StatusConflict, fmt.Sprintf("术语“%s”已经在术语表中", t.En))
			return
		}
	}
	// v2 G1, G6: a manual term is locked from the start with the user's zh
	t := api.Term{ID: s.newID("01JTMOCK"), En: en, Zh: strings.TrimSpace(req.Zh), Source: model.SourceManual, Enabled: true,
		Tier: model.TierEnforce, Locked: true, ZhSource: model.ZhSourceUser, CommonWord: mockIsCommon(en)}
	sc.terms = append(sc.terms, t)
	sortTerms(sc.terms)
	s.termsChanged(c, sc)
	writeJSON(w, http.StatusCreated, t)
}

func (s *store) findTerm(id string) (*api.Course, *sylCourse, int, error) {
	w := s.syl()
	for cid, sc := range w.courses {
		for i := range sc.terms {
			if sc.terms[i].ID == id {
				c, err := s.findCourse(cid)
				if err != nil {
					break
				}
				return c, sc, i, nil
			}
		}
	}
	return nil, nil, -1, &httpError{status: 404, msg: termNotFound}
}

// patchTerm mirrors internal/api patchTerm (v2 lead ruling 8): a change of
// zh, enabled or tier locks the term (zh also makes zh_source user) and
// bumps the version; {"locked": false} only unlocks. (An unknown term is
// 404 before the body is checked, as in the v1 mock.)
func (m *mock) patchTerm(w http.ResponseWriter, r *http.Request, s *store) {
	var req api.TermPatch
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, i, err := s.findTerm(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	edit := req.Zh != nil || req.Enabled != nil || req.Tier != nil
	switch {
	case !edit && req.Locked == nil:
		writeError(w, http.StatusBadRequest, "没有要修改的内容（zh、enabled、tier 或 locked）")
		return
	case req.Locked != nil && *req.Locked:
		writeError(w, http.StatusBadRequest, "只能解锁（locked: false）；修改译法、级别或启用状态时会自动锁定")
		return
	case req.Locked != nil && edit:
		writeError(w, http.StatusBadRequest, "解锁不能和其他修改一起提交")
		return
	case req.Tier != nil && *req.Tier != model.TierEnforce && *req.Tier != model.TierReference:
		writeError(w, http.StatusBadRequest, "级别只能是 enforce（强制）或 reference（参考）")
		return
	}
	t := &sc.terms[i]
	if req.Locked != nil {
		if t.Locked {
			t.Locked = false
			s.notify(api.ChangedEvent{Kind: "terms", ID: c.ID})
		}
		writeJSON(w, http.StatusOK, *t)
		return
	}
	changed := false
	if req.Zh != nil {
		if zh := strings.TrimSpace(*req.Zh); zh != t.Zh {
			t.Zh, t.ZhSource, changed = zh, model.ZhSourceUser, true
		}
	}
	if req.Enabled != nil && *req.Enabled != t.Enabled {
		t.Enabled, changed = *req.Enabled, true
	}
	if req.Tier != nil && *req.Tier != t.Tier {
		t.Tier, changed = *req.Tier, true
	}
	if changed {
		t.Locked = true
	}
	out := *t
	if changed {
		s.termsChanged(c, sc)
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) deleteTerm(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, i, err := s.findTerm(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	sc.terms = append(sc.terms[:i], sc.terms[i+1:]...)
	s.termsChanged(c, sc)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- the mock's syllabus_format (A8.3 structure) ----------

var (
	weekRe    = regexp.MustCompile(`(?i)^\s*(?:week|wk)\.?\s*(\d{1,2})\s*[:\-–—.)]?\s*(.+)$`)
	bulletRe  = regexp.MustCompile(`^\s*(?:[-*•·]|\d{1,2}[.)])\s+(.+)$`)
	keyValRe  = regexp.MustCompile(`^([A-Za-z][A-Za-z /&-]{1,30}):\s+(.+)$`)
	headingRe = regexp.MustCompile(`^#{1,6}\s+`)
	// "Week 3 - Modes of Operation" (fixture lecture titles)
	lectureWeekRe = regexp.MustCompile(`(?i)^week\s*(\d+)\s*-\s*(.+)$`)
)

// sectionOf maps a raw heading line to an A8.3 section.
func sectionOf(line string) string {
	l := strings.ToLower(strings.TrimSpace(headingRe.ReplaceAllString(line, "")))
	l = strings.TrimSuffix(l, ":")
	if len(l) > 40 || strings.Contains(l, ":") {
		return "" // "Assessment: Coursework 30%" is a key-value line, not a heading
	}
	switch {
	case strings.Contains(l, "outcome") || strings.Contains(l, "objective"):
		return "outcomes"
	case strings.Contains(l, "schedule") || strings.Contains(l, "weekly") || strings.Contains(l, "topics") || strings.Contains(l, "syllabus outline"):
		return "weeks"
	case strings.HasPrefix(l, "assessment") || strings.HasPrefix(l, "assessments"):
		return "assessment"
	case strings.Contains(l, "reading") || strings.Contains(l, "textbook") || strings.Contains(l, "bibliography"):
		return "reading"
	case l == "aims" || l == "aim" || l == "overview" || l == "description" || l == "module description":
		return "info"
	}
	return ""
}

// formatSyllabusMD turns raw syllabus text into the fixed SYLLABUS.md
// structure of A8.3; sections without information are omitted.
func formatSyllabusMD(c api.Course, raw string) string {
	var info, infoText, outcomes, assessment, reading []string
	type week struct{ n, topic, notes string }
	var weeks []week
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if sec := sectionOf(line); sec != "" && !weekRe.MatchString(line) {
			section = sec
			continue
		}
		if m := weekRe.FindStringSubmatch(line); m != nil {
			topic, notes, _ := strings.Cut(m[2], ";")
			weeks = append(weeks, week{m[1], strings.TrimSpace(topic), strings.TrimSpace(notes)})
			continue
		}
		item := line
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			item = m[1]
		}
		item = strings.TrimSuffix(strings.TrimSpace(item), ";")
		if m := keyValRe.FindStringSubmatch(line); m != nil && (section == "" || section == "info") {
			k := strings.TrimSpace(m[1])
			if strings.HasPrefix(strings.ToLower(k), "assessment") {
				for _, part := range strings.Split(m[2], ",") {
					assessment = append(assessment, strings.TrimSpace(part))
				}
			} else {
				info = append(info, fmt.Sprintf("**%s:** %s", k, strings.TrimSpace(m[2])))
			}
			continue
		}
		switch section {
		case "outcomes":
			if bulletRe.MatchString(line) || len(outcomes) > 0 || !strings.HasSuffix(line, ":") {
				if !strings.HasSuffix(line, ":") {
					outcomes = append(outcomes, item)
				}
			}
		case "assessment":
			assessment = append(assessment, item)
		case "reading":
			reading = append(reading, item)
		case "info":
			infoText = append(infoText, line)
		default:
			if len(info) == 0 && len(infoText) == 0 && strings.Contains(line, c.Name) {
				continue // the title line
			}
			infoText = append(infoText, line)
		}
	}
	var b strings.Builder
	title := strings.TrimSpace(deref(c.Code) + " " + c.Name)
	fmt.Fprintf(&b, "# %s\n", title)
	if len(info) > 0 || len(infoText) > 0 {
		b.WriteString("\n## Course Information\n\n")
		for _, x := range info {
			fmt.Fprintf(&b, "- %s\n", x)
		}
		if len(infoText) > 0 {
			if len(info) > 0 {
				b.WriteString("\n")
			}
			b.WriteString(strings.Join(infoText, " ") + "\n")
		}
	}
	writeList := func(h string, xs []string) {
		if len(xs) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s\n\n", h)
		for _, x := range xs {
			fmt.Fprintf(&b, "- %s\n", x)
		}
	}
	writeList("Learning Outcomes", outcomes)
	if len(weeks) > 0 {
		b.WriteString("\n## Weekly Topics\n\n| Week | Topic | Notes |\n| ---: | --- | --- |\n")
		for _, w := range weeks {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", w.n, strings.ReplaceAll(w.topic, "|", `\|`), strings.ReplaceAll(w.notes, "|", `\|`))
		}
	}
	writeList("Assessment", assessment)
	writeList("Reading List", reading)
	var topics []string
	for _, w := range weeks {
		topics = append(topics, w.topic, w.notes)
	}
	if keys := keyPhrases(topics); len(keys) > 0 {
		b.WriteString("\n## Key Terms\n\n" + strings.Join(keys, ", ") + "\n")
	}
	return b.String()
}

var phraseSplit = regexp.MustCompile(`\s*(?:;|,|&|\band\b|\(|\)|:|/)\s*`)

// keyPhrases picks short noun phrases out of the weekly topics.
func keyPhrases(topics []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range topics {
		for _, p := range phraseSplit.Split(t, -1) {
			p = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(p, "the "), "The "))
			n := len(strings.Fields(p))
			if n == 0 || n > 4 || len(p) < 3 {
				continue
			}
			l := strings.ToLower(p)
			if seen[l] || l == "revision" || l == "introduction" || strings.HasPrefix(l, "introduction to") {
				continue
			}
			seen[l] = true
			out = append(out, p)
			if len(out) >= 24 {
				return out
			}
		}
	}
	return out
}

// syllabusKeyTerms reads the comma-separated "## Key Terms" section (or
// the weekly topics of a raw text) as glossary_seed's result.
func syllabusKeyTerms(src string) []api.Term {
	var names []string
	if i := strings.Index(src, "## Key Terms"); i >= 0 {
		rest := src[i+len("## Key Terms"):]
		if j := strings.Index(rest, "\n## "); j >= 0 {
			rest = rest[:j]
		}
		for _, p := range strings.Split(rest, ",") {
			if p = strings.TrimSpace(p); p != "" {
				names = append(names, p)
			}
		}
	} else {
		var topics []string
		for _, line := range strings.Split(src, "\n") {
			if m := weekRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				topics = append(topics, m[2])
			}
		}
		names = keyPhrases(topics)
	}
	out := make([]api.Term, 0, len(names))
	for _, n := range names {
		if len([]rune(n)) < 2 || len(strings.Fields(n)) > 6 {
			continue // SPEC §6.0.1 discard rules
		}
		out = append(out, api.Term{En: n, Zh: zhOf(n), Freq: 1 + int(hashOf(n)%12)})
	}
	return out
}

// ---------- fixture content ----------

// fixtureRaw is the raw syllabus text of a fixture course: the CSC1003
// module outline, or an outline built from the course's lecture titles.
func fixtureRaw(c api.Course, lectures []api.Lecture) string {
	if deref(c.Code) == "CSC1003" {
		return csc3631Raw
	}
	var b strings.Builder
	title := strings.TrimSpace(deref(c.Code) + " " + c.Name)
	fmt.Fprintf(&b, "%s — Module Handbook 2026-27\n\n", title)
	if c.Code != nil {
		fmt.Fprintf(&b, "Module code: %s\n", *c.Code)
	}
	b.WriteString("Credits: 20 (Semester 1)\nTeaching: two lectures and one lab per week\nAssessment: Coursework 40%, Exam 60%\n\n")
	fmt.Fprintf(&b, "Aims\nThis module covers the core ideas of %s, from first principles to current practice.\n\n", c.Name)
	weeks := map[string]string{}
	var order []string
	for _, l := range lectures {
		if l.CourseID != c.ID {
			continue
		}
		if m := lectureWeekRe.FindStringSubmatch(l.Title); m != nil {
			if _, ok := weeks[m[1]]; !ok {
				order = append(order, m[1])
				weeks[m[1]] = m[2]
			} else {
				weeks[m[1]] += "; " + m[2]
			}
		}
	}
	if len(order) > 0 {
		b.WriteString("Weekly schedule\n")
		for _, n := range order {
			fmt.Fprintf(&b, "Week %s: %s\n", n, weeks[n])
		}
		b.WriteString("\n")
	}
	b.WriteString("Reading\nLecture notes and slides on the module page\n")
	return b.String()
}

const csc3631Raw = `CSC1003 Cryptography — Module Outline 2026-27

Module code: CSC1003
Credits: 10 (Semester 1)
Teaching: two lectures per week; a practical every other week
Assessment: Coursework 30% (two problem sets), Exam 70% (2 hours, January)

Aims
This module introduces the principles of modern cryptography: how security is defined, why classical schemes fail, and how block ciphers, hash functions, MACs and public-key primitives are built and used in practice.

Learning outcomes
On completion students should be able to:
- explain perfect secrecy and computational security;
- analyse stream ciphers built from LFSRs and identify their weaknesses;
- describe the structure of DES and AES and the common modes of operation;
- use hash functions and MACs correctly and recognise length-extension pitfalls;
- explain RSA, Diffie-Hellman key exchange and digital signatures.

Weekly schedule
Week 1: Classical ciphers & frequency analysis; perfect secrecy and the one-time pad
Week 2: Stream ciphers & LFSRs; block ciphers and DES
Week 3: Advanced Encryption Standard; modes of operation (ECB, CBC, CTR, GCM)
Week 4: Hash functions & SHA-2; message authentication codes (HMAC)
Week 5: Number theory refresher; RSA
Week 6: Diffie-Hellman key exchange; discrete logarithm problem
Week 7: Digital signatures; certificates and PKI
Week 8: Elliptic-curve cryptography
Week 9: TLS 1.3 handshake
Week 10: Revision

Reading
Katz & Lindell, Introduction to Modern Cryptography (3rd ed.)
Paar & Pelzl, Understanding Cryptography
`

// term banks: "en|zh|source|freq[|off]"
var termBanks = map[string]string{
	"CSC1003": `block cipher|分组密码|syllabus|42
stream cipher|流密码|syllabus|31
one-time pad|一次一密|syllabus|12
perfect secrecy|完善保密性|syllabus|9
LFSR|线性反馈移位寄存器|material|17
Feistel network|Feistel 网络|material|14
DES||syllabus|26
AES||syllabus|38
S-box|S 盒|material|22
key schedule|密钥编排|material|11
mode of operation|工作模式|syllabus|15
ECB||material|8
CBC||material|13
CTR mode|计数器模式|material|7
GCM||material|5
initialization vector|初始化向量|material|16
nonce|一次性随机数|review|10
padding oracle attack|填充预言攻击|material|3
hash function|哈希函数|syllabus|29
collision resistance|抗碰撞性|material|11
preimage resistance|抗原像性|material|6
SHA-2||syllabus|9
Merkle–Damgård construction|Merkle–Damgård 结构|material|4
length extension attack|长度扩展攻击|material|3
message authentication code|消息认证码|syllabus|18
HMAC||syllabus|12
RSA||syllabus|20
Diffie-Hellman key exchange|Diffie-Hellman 密钥交换|syllabus|9
discrete logarithm|离散对数|syllabus|7
digital signature|数字签名|syllabus|11
public key infrastructure|公钥基础设施|syllabus|4
elliptic curve|椭圆曲线|syllabus|6
Kerckhoffs's principle|柯克霍夫原则|manual|5
chosen-plaintext attack|选择明文攻击|material|8
IND-CPA||manual|6
semantic security|语义安全|syllabus|5
Vigenère cipher|维吉尼亚密码|material|7
Caesar cipher|凯撒密码|review|4
frequency analysis|频率分析|material|3|off
avalanche effect|雪崩效应|material|4
key|密钥|syllabus|120
public|公钥|material|41
world|世界|material|6|off
algorithm|算法|syllabus|30
algorithms|算法|material|9|off
IV|初始化向量|material|10
block|分组|material|35
state|状态|material|22`,
	"COMP2120": `process|进程|syllabus|64
thread|线程|syllabus|41
context switch|上下文切换|material|12
scheduler|调度器|syllabus|19
round robin|时间片轮转|material|9
priority inversion|优先级反转|material|4
mutex|互斥锁|syllabus|15
semaphore|信号量|syllabus|13
race condition|竞态条件|material|8
deadlock|死锁|syllabus|11
virtual memory|虚拟内存|syllabus|22
page table|页表|material|17
TLB||material|9
page fault|缺页异常|material|10
thrashing|抖动|review|3
system call|系统调用|syllabus|14
kernel mode|内核态|material|7
file descriptor||manual|5`,
	"CSC1002": `instruction set architecture|指令集架构|syllabus|21
pipeline hazard|流水线冒险|syllabus|14
data forwarding|数据前递|material|8
branch prediction|分支预测|syllabus|12
out-of-order execution|乱序执行|material|9
reorder buffer|重排序缓冲区|material|6
cache line|缓存行|material|11
write-back|写回|material|5
cache coherence|缓存一致性|syllabus|7
MESI protocol|MESI 协议|material|4
Amdahl's law|阿姆达尔定律|syllabus|6
CPI||syllabus|10
SIMD||material|5
RISC-V||manual|8
TLB||material|4
memory hierarchy|存储层次结构|syllabus|9`,
	"COMP3311": `relational model|关系模型|syllabus|18
primary key|主键|syllabus|22
foreign key|外键|syllabus|15
functional dependency|函数依赖|syllabus|12
normal form|范式|syllabus|10
BCNF||material|6
relational algebra|关系代数|syllabus|9
SQL||syllabus|40
join|连接|material|25
B+ tree|B+ 树|material|8
query optimiser|查询优化器|material|5
transaction|事务|syllabus|16
ACID||syllabus|7
serialisability|可串行化|material|4
two-phase locking|两阶段锁|material|5
write-ahead logging|预写日志|material|3`,
}

// material term banks: what material_terms "finds" in a new slide deck.
var materialBanks = map[string]string{
	"CSC1003": `birthday attack|生日攻击|material|3
authenticated encryption|认证加密|material|5
key derivation function|密钥派生函数|material|2`,
	"COMP2120": `copy-on-write|写时复制|material|3
working set|工作集|material|2`,
	"CSC1002": `branch target buffer|分支目标缓冲器|material|2
superscalar|超标量|material|3`,
	"COMP3311": `index scan|索引扫描|material|3
hash join|哈希连接|material|2`,
}

func parseBank(bank string, prefix string) []api.Term {
	out := []api.Term{}
	for i, line := range strings.Split(strings.TrimSpace(bank), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 4 {
			continue
		}
		var freq int
		fmt.Sscanf(f[3], "%d", &freq)
		out = append(out, api.Term{ID: fmt.Sprintf("%s%0*d", prefix, 26-len(prefix), i+1), En: f[0], Zh: f[1], Source: f[2], Freq: freq, Enabled: len(f) < 5 || f[4] != "off"})
	}
	return out
}

func fixtureTerms(c *api.Course) []api.Term {
	code := deref(c.Code)
	bank, ok := termBanks[code]
	if !ok {
		return []api.Term{}
	}
	ts := parseBank(bank, "01JT"+strings.ToUpper(code))
	annotateTerms(code, ts) // v2 fields (janitor.go)
	sortTerms(ts)
	return ts
}

func materialTerms(c api.Course, filename string) []api.Term {
	return parseBank(materialBanks[deref(c.Code)], "01JTMAT")
}

// zhOf is the mock's "translation" of a syllabus key term ("" = keep English).
func zhOf(en string) string {
	l := strings.ToLower(en)
	for _, bank := range termBanks {
		for _, t := range parseBank(bank, "x") {
			if strings.ToLower(t.En) == l {
				return t.Zh
			}
		}
	}
	known := map[string]string{
		"classical ciphers": "古典密码", "frequency analysis": "频率分析", "perfect secrecy": "完善保密性",
		"one-time pad": "一次一密", "stream ciphers": "流密码", "lfsrs": "线性反馈移位寄存器", "block ciphers": "分组密码",
		"advanced encryption standard": "高级加密标准", "modes of operation": "工作模式", "hash functions": "哈希函数",
		"message authentication codes": "消息认证码", "number theory refresher": "数论复习", "discrete logarithm problem": "离散对数问题",
		"digital signatures": "数字签名", "certificates": "证书", "pki": "公钥基础设施", "elliptic-curve cryptography": "椭圆曲线密码学",
		"tls 1.3 handshake": "TLS 1.3 握手",
	}
	return known[l]
}
