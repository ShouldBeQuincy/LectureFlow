// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"lectureflow/internal/cite"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/pyenv"
	"lectureflow/internal/stages"
	"lectureflow/internal/state"
)

// v1.1 A8 (syllabus wizard), A3 / A2.3 course operations and A11 (course
// citation JSON).

const syllabusBusy = "syllabus.busy"

// syllabusNameFile remembers the file name of syllabus.raw.txt for a course
// that has no syllabus row yet (see rawSyllabus).
const syllabusNameFile = "syllabus.raw.name"

// maxSyllabusBytes bounds pasted or uploaded syllabus text.
const maxSyllabusBytes = 8 << 20

func (s *Server) glossarySeedRunning(ctx context.Context, courseID string) bool {
	j, err := s.st.GetCurrentJob(ctx, model.ScopeCourse, courseID, model.StageGlossarySeed)
	return err == nil && j.Status == model.StatusRunning
}

// writeTemp writes data next to path as path+".tmp" and returns that name;
// the caller renames it into place (atomic replace) or removes it.
func writeTemp(path, data string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o644); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// writeFileAtomic writes path through a temporary file and a rename.
func writeFileAtomic(path, data string) error {
	tmp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeRaw stores the raw syllabus text (A8.2 syllabus.raw.txt) and its
// source file name.
func (s *Server) writeRaw(courseID, filename, raw string) error {
	if err := checkRecordIDs(courseID); err != nil { // record_dir.go
		return err
	}
	if err := writeFileAtomic(s.syllabusRawPath(courseID), raw); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(model.CourseDir(s.dataDir, courseID), syllabusNameFile), filename)
}

// receiveSyllabusFile streams a .txt/.md/.pdf upload into dir (as
// "upload.{ext}") and returns its text: .txt/.md read as UTF-8, .pdf
// extracted with worker/extract_pdf.py, pages joined by "\n\n" (SPEC
// §6.0.1). The stored file is removed on error.
func (s *Server) receiveSyllabusFile(w http.ResponseWriter, r *http.Request, dir string) (uploadResult, string, error) {
	up, err := s.receiveUpload(w, r, dir, "upload", acceptExt([]string{".txt", ".md", ".pdf"}, "syllabus.bad_type"))
	if err != nil {
		return up, "", err
	}
	var text string
	if strings.ToLower(filepath.Ext(up.filename)) == ".pdf" {
		pages, err := s.extractPDF(r.Context(), dir, up.path)
		if err != nil {
			os.Remove(up.path)
			if r.Context().Err() != nil {
				return up, "", r.Context().Err()
			}
			var ce *model.CodedError
			if errors.As(err, &ce) {
				switch ce.Info.Code {
				case stages.CodeWorkerPythonNotInstalled:
					// 3.1.0: the python component is not installed (desktop)
					return up, "", apiErrDetail(http.StatusUnprocessableEntity, ce.Info.Code, nil, err.Error())
				case stages.CodePDFDecodeBudget, stages.CodePDFMemoryLimit, stages.CodePDFTimeout:
					// over the extraction's limits (security review 2, M2)
					return up, "", apiErrDetail(http.StatusUnprocessableEntity, ce.Info.Code, ce.Info.Params, err.Error())
				}
			}
			return up, "", apiErrDetail(http.StatusUnprocessableEntity, "syllabus.pdf_failed", nil, err.Error())
		}
		text = strings.Join(pages, "\n\n")
	} else if text, err = readText(up.path, maxSyllabusBytes); err != nil {
		os.Remove(up.path)
		return up, "", err
	}
	return up, text, nil
}

// rawSyllabus returns the raw text and the file name of a course syllabus
// (A8.2): syllabus.raw.txt; for a v1 course that has a syllabus but neither
// a raw file nor SYLLABUS.md, the extracted text of the syllabus row.
func (s *Server) rawSyllabus(ctx context.Context, courseID string) (raw, filename string, err error) {
	sy, err := s.st.GetSyllabus(ctx, courseID)
	hasRow := err == nil
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return "", "", err
	}
	if b, err := os.ReadFile(s.syllabusRawPath(courseID)); err == nil {
		raw = string(b)
		if nb, err := os.ReadFile(filepath.Join(model.CourseDir(s.dataDir, courseID), syllabusNameFile)); err == nil {
			filename = strings.TrimSpace(string(nb))
		}
	} else if hasRow && sy.Text != nil && !fileExists(s.syllabusMDPath(courseID)) {
		raw = *sy.Text
	}
	if hasRow {
		filename = sy.Filename
	}
	return raw, filename, nil
}

func (s *Server) syllabusDoc(ctx context.Context, courseID string) (SyllabusDoc, error) {
	raw, name, err := s.rawSyllabus(ctx, courseID)
	if err != nil {
		return SyllabusDoc{}, err
	}
	doc := SyllabusDoc{Filename: name, Raw: raw}
	if b, err := os.ReadFile(s.syllabusMDPath(courseID)); err == nil {
		doc.Markdown = string(b)
	}
	return doc, nil
}

func (s *Server) courseExists(w http.ResponseWriter, r *http.Request, id string) (model.Course, bool) {
	c, err := s.st.GetCourse(r.Context(), id)
	if err != nil {
		s.fail(w, r, notFoundAs(err, "course.not_found"))
		return c, false
	}
	return c, true
}

// GET /api/courses/{id}/syllabus → SyllabusDoc.
func (s *Server) getSyllabus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	doc, err := s.syllabusDoc(r.Context(), c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// POST /api/courses/{id}/syllabus/raw: multipart "file" (.txt/.md/.pdf)
// or JSON {text} (pasted). Saves syllabus.raw.txt (A8.2). When the course
// already has a syllabus row, its file name and path are updated (text
// unchanged); a new course gets its row only when SYLLABUS.md is saved
// (PUT), because a syllabus row without a glossary_seed job would make
// every launched lecture's asr wait for glossary_seed forever.
func (s *Server) syllabusRaw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := checkRecordIDs(c.ID); err != nil { // record_dir.go
		s.fail(w, r, err)
		return
	}
	var name, raw string
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	// SPEC-v3 V1.4: in desktop mode JSON {"path"} is a local file (copied
	// like an upload); JSON {text} stays pasted text.
	native := false
	if s.nativeUpload(r) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSyllabusBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "syllabus.bad_body", nil)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		native = jsonHasPath(b)
	}
	if ct == "multipart/form-data" || native {
		if err := s.checkSpace(r); err != nil {
			s.fail(w, r, err)
			return
		}
		up, text, err := s.receiveSyllabusFile(w, r, model.SyllabusDir(s.dataDir, c.ID))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		os.Remove(up.path) // only the text is kept (syllabus.raw.txt)
		name, raw = up.filename, text
		if strings.TrimSpace(raw) == "" {
			writeError(w, http.StatusUnprocessableEntity, "syllabus.no_text", nil)
			return
		}
	} else {
		var in SyllabusRawInput
		dec := json.NewDecoder(io.LimitReader(r.Body, maxSyllabusBytes+1))
		if err := dec.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "syllabus.bad_body", nil)
			return
		}
		name, raw = "pasted.txt", in.Text
		if strings.TrimSpace(raw) == "" {
			writeError(w, http.StatusBadRequest, "syllabus.empty", nil)
			return
		}
	}
	if err := s.writeRaw(c.ID, name, raw); err != nil {
		s.fail(w, r, err)
		return
	}
	rawAbs, _ := filepath.Abs(s.syllabusRawPath(c.ID))
	err := s.st.Tx(ctx, func(tx db.Store) error {
		sy, err := tx.GetSyllabus(ctx, c.ID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: name, Path: rawAbs, Text: sy.Text})
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", c.ID)
	doc, err := s.syllabusDoc(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// stripFence removes a ``` fence the model may wrap the Markdown in.
func stripFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	lines := strings.Split(t, "\n")
	lines = lines[1:]
	if n := len(lines); n > 0 && strings.HasPrefix(strings.TrimSpace(lines[n-1]), "```") {
		lines = lines[:n-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// POST /api/courses/{id}/syllabus/format: the syllabus_format role turns
// the raw text into SYLLABUS.md (A8.3). Nothing is saved: the wizard shows
// the result for editing and saves it with PUT.
func (s *Server) formatSyllabus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if s.llmc == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "llm"})
		return
	}
	raw, _, err := s.rawSyllabus(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeError(w, http.StatusConflict, "syllabus.no_raw", nil)
		return
	}
	provider := ""
	if s.set != nil {
		t, err := s.set.RoleTarget(model.RoleSyllabusFormat)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		provider = t.ProviderID
	}
	system, user := ctxbuild.BuildSyllabusFormatRequest(raw)
	res, err := s.llmc.Complete(ctx, model.RoleSyllabusFormat, system, user, false)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// The provider declined (R25 B-3): its own code says so and that
		// asking again will not help, with the role, provider and model
		// (llm.CodeRefusal), instead of the generic format_failed.
		var ce *model.CodedError
		if errors.Is(err, llm.ErrRefusal) && errors.As(err, &ce) && ce.Info.Code == llm.CodeRefusal {
			info := ce.Info
			if info.Detail == "" && ce.Err != nil {
				info.Detail = ce.Err.Error()
			}
			writeErrorInfo(w, http.StatusBadGateway, info)
			return
		}
		writeErrorInfo(w, http.StatusBadGateway, model.ErrorInfo{Code: "syllabus.format_failed", Detail: err.Error()})
		return
	}
	md := stripFence(res.Text)
	if md == "" {
		writeError(w, http.StatusBadGateway, "syllabus.format_empty", nil)
		return
	}
	out := SyllabusFormatResult{Markdown: md, Usage: UsageStats{
		PromptTokens: res.PromptTokens, CacheHitTokens: res.CacheHitTokens, CompletionTokens: res.CompletionTokens}}
	if p, ok := s.providerByID(provider); ok {
		if cost, ok := p.EstimateCost(res.PromptTokens, res.CacheHitTokens, res.CompletionTokens); ok {
			out.Usage.EstimatedCost = &cost
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) providerByID(id string) (model.Provider, bool) {
	if s.set == nil || id == "" {
		return model.Provider{}, false
	}
	return s.set.Provider(id)
}

// PUT /api/courses/{id}/syllabus {markdown}: save SYLLABUS.md and
// syllabus.text (A8.2), then run glossary_seed again (409 while it runs).
func (s *Server) saveSyllabus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := checkRecordIDs(c.ID); err != nil { // record_dir.go
		s.fail(w, r, err)
		return
	}
	var in SyllabusSaveInput
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSyllabusBytes+1))
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "request.bad_json", nil)
		return
	}
	if strings.TrimSpace(in.Markdown) == "" {
		writeError(w, http.StatusBadRequest, "syllabus.empty", nil)
		return
	}
	if s.glossarySeedRunning(ctx, c.ID) {
		writeError(w, http.StatusConflict, syllabusBusy, nil)
		return
	}
	mdPath := s.syllabusMDPath(c.ID)
	tmp, err := writeTemp(mdPath, in.Markdown)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_, rawName, err := s.rawSyllabus(ctx, c.ID)
	if err != nil {
		os.Remove(tmp)
		s.fail(w, r, err)
		return
	}
	err = s.st.Tx(ctx, func(tx db.Store) error {
		_, err := tx.GetSyllabus(ctx, c.ID)
		switch {
		case errors.Is(err, db.ErrNotFound):
			name, path := rawName, s.syllabusRawPath(c.ID)
			if !fileExists(path) {
				path = mdPath
			}
			if name == "" {
				name = "SYLLABUS.md"
			}
			abs, _ := filepath.Abs(path)
			md := in.Markdown
			if err := tx.UpsertSyllabus(ctx, model.Syllabus{CourseID: c.ID, Filename: name, Path: abs, Text: &md}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := tx.SetSyllabusText(ctx, c.ID, in.Markdown); err != nil {
				return err
			}
		}
		return state.ResetGlossarySeed(ctx, tx, c.ID)
	})
	if err == nil {
		err = os.Rename(tmp, mdPath)
	}
	if err != nil {
		os.Remove(tmp)
		s.failBusy(w, r, err, syllabusBusy, nil)
		return
	}
	s.kick()
	s.hub.Changed("course", c.ID)
	s.respondCourse(w, r, c.ID, http.StatusOK)
}

// syllabusPDFTimeout caps the syllabus extraction's time limit
// (stages.PDFTimeout; the user waits for the request).
var syllabusPDFTimeout = 5 * time.Minute

// runExtractPDF runs {python} {worker_dir}/extract_pdf.py --in job.json
// --out result.json.tmp in dir (SPEC §6.0, §6.0.1) and returns the pages.
// The request context kills the process. While it runs the interpreter is
// reported by RunningPythons with reason material, so the python
// component is not uninstalled or switched under it (v3 Gate round 2).
// It has the material stage's limits (stages/pdf_limits.go: memory cap,
// decode budget, a time limit by size and page count capped at
// syllabusPDFTimeout); over one of them is a coded error.
func (s *Server) runExtractPDF(ctx context.Context, dir, pdf string) ([]string, error) {
	if s.cfg == nil {
		return nil, errors.New("没有配置 python 与 worker_dir")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	absPDF, err := filepath.Abs(pdf)
	if err != nil {
		return nil, err
	}
	py := s.cfg.PDFPython() // the python component's base venv when installed (v3)
	if py == "" {
		// The desktop application without the python component (3.1.0):
		// nothing to run, the component to install.
		return nil, model.Errorf(stages.CodeWorkerPythonNotInstalled, nil, errors.New("没有安装“Python 运行环境”组件，无法从 PDF 中提取文字"))
	}
	if strings.ContainsAny(py, `/\`) {
		if py, err = filepath.Abs(py); err != nil {
			return nil, err
		}
	}
	script, err := filepath.Abs(filepath.Join(s.cfg.WorkerDir, "extract_pdf.py"))
	if err != nil {
		return nil, err
	}
	defer s.trackPython(py, PythonUseMaterial)()
	job, _ := json.Marshal(stages.PDFJob(absPDF))
	jobPath := filepath.Join(absDir, "job.json")
	if err := writeFileAtomic(jobPath, string(job)); err != nil {
		return nil, err
	}
	out := filepath.Join(absDir, "result.json")
	tmp := out + ".tmp"
	os.Remove(tmp)
	var size int64
	if fi, err := os.Stat(absPDF); err == nil {
		size = fi.Size()
	}
	deadline := stages.NewPDFDeadline(ctx, size, syllabusPDFTimeout)
	defer deadline.Stop()
	cmd := exec.CommandContext(deadline.Context(), py, pyenv.ScriptArgs(script, "--in", jobPath, "--out", tmp)...) // -s (pyenv "Isolation")
	cmd.Dir = absDir
	cmd.Env = pyenv.Environ("PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1") // no usage reporting, no PYTHONPATH and the like (pyenv)
	cmd.WaitDelay = 3 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	proc, err := procutil.Start(cmd, stages.PDFLimits())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("无法启动 PDF 抽取进程：%v", err)
	}
	defer proc.Close()
	var errName, errRaw string // the last ERROR_CODE line
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if rest, ok := strings.CutPrefix(line, "STATUS "); ok {
			deadline.Status(rest) // "pages n": the time limit grows with the page count
		} else if rest, ok := strings.CutPrefix(line, "ERROR_CODE "); ok {
			errName, errRaw, _ = strings.Cut(strings.TrimSpace(rest), " ")
		}
	}
	_, _ = io.Copy(io.Discard, stdout) // a line longer than the buffer
	werr := cmd.Wait()
	if werr != nil || ctx.Err() != nil {
		os.Remove(tmp)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := deadline.Err(ctx); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := stages.PDFMemoryError(proc.Usage(), werr != nil); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if werr != nil {
		msg := lastLine(strings.ToValidUTF8(stderr.String(), "�"))
		if msg == "" {
			msg = werr.Error()
		}
		if code, params := stages.PDFErrorCode(errName, strings.TrimSpace(errRaw)); code != "" {
			return nil, model.Errorf(code, params, errors.New(msg))
		}
		return nil, errors.New(msg)
	}
	b, err := os.ReadFile(tmp)
	os.Remove(tmp)
	if err != nil {
		return nil, fmt.Errorf("PDF 抽取进程没有写出结果：%v", err)
	}
	var res struct {
		Pages *[]string `json:"pages"`
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Pages == nil {
		return nil, fmt.Errorf("PDF 抽取结果无法解析：%v", err)
	}
	return *res.Pages, nil
}

// ---- course operations (A3.2 启动全部草稿, A2.3 更新全部过期讲座) ----

// courseLectures returns the course's lectures in the list order of P13.4
// (by recorded_at) and their numbering.
func (s *Server) courseLectures(ctx context.Context, c model.Course) ([]model.Lecture, numbering, error) {
	nums, lecs, err := s.courseNumbering(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	return orderLectures(lecs, nums), nums, nil
}

func (s *Server) respondCourseOp(w http.ResponseWriter, r *http.Request, c model.Course, results []BatchResult) {
	v, err := s.courseView(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, CourseOpResult{Results: results, Course: v})
}

// POST /api/courses/{id}/launch-drafts: launch every draft of the course.
func (s *Server) launchDrafts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	lecs, _, err := s.courseLectures(ctx, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var ids []string
	for _, l := range lecs {
		if state.IsDraft(l) {
			ids = append(ids, l.ID)
		}
	}
	if len(ids) > 0 {
		if err := s.settingsReady(); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.respondCourseOp(w, r, c, s.launchAll(ctx, ids))
}

// POST /api/courses/{id}/update-stale {model_choice?}: A2.3 "更新" for every
// lecture of the course with a stale stage, with ONE model decision.
func (s *Server) updateStale(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req ModelChoiceRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	lecs, _, err := s.courseLectures(ctx, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ix, err := s.scopeJobsOf(ctx, nil, nil, lecs) // this course's lectures only (course_reads.go)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var ids []string
	for _, l := range lecs {
		if !state.IsDraft(l) && anyStatus(ix.of(model.ScopeLecture, l.ID), model.StatusStale) {
			ids = append(ids, l.ID)
		}
	}
	results := []BatchResult{}
	if len(ids) > 0 {
		res, err := s.sc.UpdateLectures(ctx, ids, req.ModelChoice)
		if err != nil {
			s.failChoice(w, r, err)
			return
		}
		for _, x := range res {
			results = append(results, s.batchResult(ctx, x.LectureID, x.Err, true, ""))
		}
	}
	s.respondCourseOp(w, r, c, results)
}

// ---- A11 course citation JSON ----

// asciiFileName replaces what cannot appear in a quoted ASCII filename.
func asciiFileName(name string) string {
	return strings.Map(func(r rune) rune {
		if r > unicode.MaxASCII || r < 0x20 || r == '"' || r == '\\' || r == '/' {
			return '_'
		}
		return r
	}, name)
}

// GET /api/courses/{id}/export/cite.json: every finished lecture (current
// export job done, lead ruling 12), built by cite.Build with the calendar
// numbers of all the course's lectures (v2 P13.6: Build orders them by
// recorded_at and makes the W?? labels unique course-wide). The export is
// recorded (kv cite_exported:{course}) so later renumbering asks first.
func (s *Server) courseCite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := s.courseExists(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	lecs, nums, err := s.courseLectures(ctx, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ix, err := s.scopeJobsOf(ctx, nil, nil, lecs) // this course's lectures only (course_reads.go)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var inputs []cite.LectureInput
	for _, l := range lecs {
		if e, ok := findJob(ix.of(model.ScopeLecture, l.ID), model.StageExport); !ok || e.Status != model.StatusDone {
			continue
		}
		in := cite.LectureInput{Lecture: l}
		in.MediaMS, _ = s.mediaDurationMS(l, ix.of(model.ScopeLecture, l.ID)) // = Lecture.duration_ms
		if sum, err := s.st.GetSummary(ctx, l.ID); err == nil {
			in.Summary = sum.Text
		} else if !errors.Is(err, db.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
		if in.Sentences, err = s.st.ListSentences(ctx, l.ID, 0, 0); err != nil {
			s.fail(w, r, err)
			return
		}
		inputs = append(inputs, in)
	}
	terms, err := s.st.ListTerms(ctx, c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	kinds, err := s.st.ListLectureKinds(ctx) // the "kinds" table (cite schema 3)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	doc := cite.Build(c, kinds, nums.cite(), inputs, terms, s.now())
	var buf bytes.Buffer
	if err := cite.Encode(&buf, doc); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := stages.MarkCiteExported(ctx, s.st, c.ID, s.now()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.hub.Changed("course", c.ID)
	name := cite.CoursePart(c) + ".cite.json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFileName(name), url.PathEscape(name)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf.Bytes()); err != nil {
		slog.Debug("write cite.json", "err", err)
	}
}
