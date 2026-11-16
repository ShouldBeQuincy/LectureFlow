// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/qa"
	"lectureflow/internal/resources"
)

// fixtureEpoch is the reference time of testdata/api/v11_*.json. When the
// mock server loads them it shifts every timestamp by (start time − epoch),
// so "3 hours ago" in the fixtures is 3 hours before the server started.
const fixtureEpoch = "2026-10-23T14:00:00Z"

// store is one in-memory world: the default scenario shares one store; the
// "empty" scenario has its own store without courses. Mutations follow the
// real rules closely enough for the front end to be exercised end to end;
// they never touch the disk.
type store struct {
	mu        sync.Mutex
	courses   []api.Course
	lectures  []api.Lecture
	settings  api.Settings
	usage     []api.ProviderUsage
	res       resources.Snapshot
	terms     []api.Term
	reports   []model.JanitorReport // v2: v2_janitor_reports.json (janitor.go)
	lastRuns  []model.JanitorReport // v2: v2_janitor_last_runs.json
	sentences []api.Sentence
	patches   []api.Patch
	paused    bool
	// redecodeOff: the switch "修复退化的转写窗口" was turned off
	// (api.Settings.ASRRedecode; on by default, like kv asr.redecode).
	redecodeOff bool
	started     time.Time
	// progress0 is the progress of each running stage at load time; the
	// current progress grows slowly with the time since the server started.
	progress0 map[string]float64
	// noWords: lectures whose transcript has no word timestamps (v2 P4.2,
	// transcribed before v2) until asr runs again; see finalize.
	noWords map[string]bool
	subs    map[chan api.ChangedEvent]struct{}
	idSeq   int
	// lecture kinds (kinds.go): the global kind catalog and the kind_map of each
	// course's saved file-name rule.
	kinds    []api.LectureKind
	kindMaps map[string]map[string]string
	// renumNotices: Course.renumber_notices by course id (kinds.go; only
	// the scenario "renumbered" has some).
	renumNotices map[string][]api.RenumberNotice
	// dev: the transcription devices, their self-tests and the power state
	// (devices.go, SPEC-v2 I6-I10).
	dev *devState
	// history: per lecture, the usage of runs that are no longer current
	// (Lecture.usage_total, r7.go).
	history map[string]api.UsageStats
}

// shiftJSON parses b and moves every RFC3339 UTC timestamp by d.
func shiftJSON(b []byte, d time.Duration) ([]byte, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, e := range t {
				t[k] = walk(e)
			}
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
		case string:
			if len(t) == 20 && t[10] == 'T' && t[19] == 'Z' {
				if tm, err := time.Parse(time.RFC3339, t); err == nil {
					return tm.Add(d).UTC().Format(time.RFC3339)
				}
			}
		}
		return x
	}
	return json.Marshal(walk(v))
}

func readShifted(dir, name string, d time.Duration, v any) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if d != 0 {
		if b, err = shiftJSON(b, d); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func epoch() time.Time {
	t, _ := time.Parse(time.RFC3339, fixtureEpoch)
	return t
}

// loadStore reads the v1.1 fixtures. resFile picks the resources snapshot
// (v11_resources.json or the "nogpu" variant). empty drops all courses.
func loadStore(dir string, now time.Time, empty bool) (*store, error) {
	shift := now.Sub(epoch()).Truncate(time.Second)
	s := &store{started: now, progress0: map[string]float64{}, subs: map[chan api.ChangedEvent]struct{}{},
		noWords: map[string]bool{mockNoWordsLecture: true}}
	files := []struct {
		name string
		v    any
	}{
		{"v11_courses.json", &s.courses},
		{"v11_lectures.json", &s.lectures},
		{"v11_settings.json", &s.settings},
		{"v11_usage_today.json", &s.usage},
		{"v11_resources.json", &s.res},
		{"terms.json", &s.terms},
		{"v2_janitor_reports.json", &s.reports},
		{"v2_janitor_last_runs.json", &s.lastRuns},
	}
	for _, f := range files {
		if err := readShifted(dir, f.name, shift, f.v); err != nil {
			return nil, err
		}
	}
	var page api.SentencePage
	if err := readShifted(dir, "mock_sentences.json", 0, &page); err != nil {
		return nil, err
	}
	s.sentences = page.Sentences
	for i := range s.sentences { // coded as the real views.go does (P14.3)
		if q := s.sentences[i].QAError; q != nil && *q != "" {
			s.sentences[i].QAErrorInfo = qa.ErrorInfo(*q)
		}
	}
	if err := readShifted(dir, "mock_patches.json", 0, &s.patches); err != nil {
		return nil, err
	}
	if empty {
		s.courses, s.lectures, s.reports, s.lastRuns = []api.Course{}, []api.Lecture{}, nil, nil
		for i := range s.usage {
			s.usage[i] = api.ProviderUsage{ProviderID: s.usage[i].ProviderID, Name: s.usage[i].Name}
		}
		for i := range s.res.Providers {
			p := &s.res.Providers[i]
			p.Today, p.Total, p.Inflight = resources.Tokens{}, resources.Tokens{}, 0
			p.CacheHitRateToday, p.CacheHitRateTotal, p.CostToday = nil, nil, nil
		}
	}
	for _, l := range s.lectures {
		for _, st := range l.Stages {
			if st.Status == model.StatusRunning {
				s.progress0[l.ID+"/"+st.Stage] = st.Progress
			}
		}
	}
	s.initKinds() // lecture kinds (kinds.go)
	s.refreshAll()
	seedHistory(s)      // Lecture.usage_total (r7.go)
	initDevices(s, now) // devices.go (phase 3b)
	return s, nil
}

// ---------- change notifications (SSE "changed") ----------

func (s *store) subscribe() (chan api.ChangedEvent, func()) {
	ch := make(chan api.ChangedEvent, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// notify must be called with s.mu held.
func (s *store) notify(evs ...api.ChangedEvent) {
	for ch := range s.subs {
		for _, ev := range evs {
			select {
			case ch <- ev:
			default: // slow client: drop, it refetches on the next change
			}
		}
	}
}

// ---------- reads ----------

// advance moves running stages forward with the wall clock: the remaining
// part takes about 30 minutes, capped at 99% (the mock never finishes a stage).
func (s *store) advance(now time.Time) {
	el := now.Sub(s.started).Seconds()
	for i := range s.lectures {
		l := &s.lectures[i]
		for j := range l.Stages {
			st := &l.Stages[j]
			p0, ok := s.progress0[l.ID+"/"+st.Stage]
			if !ok || st.Status != model.StatusRunning {
				continue
			}
			st.Progress = math.Min(0.99, p0+(1-p0)*el/1800)
			st.Progress = math.Round(st.Progress*1000) / 1000
		}
	}
}

func (s *store) refreshAll() {
	for i := range s.lectures {
		s.finalize(&s.lectures[i])
	}
	for i := range s.courses {
		finalizeCourse(&s.courses[i], s.lectures, s.kctx())
	}
}

func (s *store) findLecture(id string) (*api.Lecture, error) {
	for i := range s.lectures {
		if s.lectures[i].ID == id {
			return &s.lectures[i], nil
		}
	}
	return nil, errNotFound("讲座")
}

func (s *store) findCourse(id string) (*api.Course, error) {
	for i := range s.courses {
		if s.courses[i].ID == id {
			return &s.courses[i], nil
		}
	}
	return nil, errNotFound("课程")
}

func (s *store) courseLectures(id string) []api.Lecture {
	out := []api.Lecture{}
	for _, l := range s.lectures {
		if l.CourseID == id {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// snapshot returns GET /api/resources for the given base snapshot, with
// slowly varying GPU numbers and the live ASR slot usage: used is the
// number of asr jobs running on the NVIDIA card (power management: a job
// on an Intel device leaves the card idle).
func (s *store) resources(base resources.Snapshot, now time.Time, used int) resources.Snapshot {
	r := base
	r.SampledAt = now.UTC().Format(time.RFC3339)
	r.GPU.Paused = s.paused
	r.GPU.ASRSlotsUsed = used
	r.Providers = append([]resources.Provider{}, base.Providers...)
	if !r.GPU.Available {
		return r
	}
	t := float64(now.Unix())
	util := func(t float64) int {
		if used == 0 {
			return int(4 + 3*math.Sin(t/11))
		}
		return int(math.Round(89 + 5*math.Sin(t/13) + 3*math.Sin(t/3.7)))
	}
	temp := func(t float64) int {
		if used == 0 {
			return int(math.Round(48 + 2*math.Sin(t/29)))
		}
		return int(math.Round(73 + 2*math.Sin(t/41) + math.Sin(t/7)))
	}
	r.GPU.UtilHistory, r.GPU.TempHistory = make([]int, 30), make([]int, 30)
	for k := 0; k < 30; k++ {
		ts := t - float64(2*(29-k))
		r.GPU.UtilHistory[k], r.GPU.TempHistory[k] = util(ts), temp(ts)
	}
	u, tc := util(t), temp(t)
	r.GPU.UtilPct, r.GPU.TempC = &u, &tc
	r.GPU.TempWarn = tc >= 85
	mem := 7820 + int(40*math.Sin(t/17))
	if used == 0 {
		mem = 1260
	}
	r.GPU.MemUsedMB = &mem
	pw := math.Round((104+9*math.Sin(t/13))*10) / 10
	if used == 0 {
		pw = 14.2
	}
	r.GPU.PowerW = &pw
	return r
}

// ---------- model choice (A4.3) ----------

func (s *store) providerByID(id string) (api.ProviderView, bool) {
	for _, p := range s.settings.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return api.ProviderView{}, false
}

func (s *store) role(stage string) (api.RoleView, bool) {
	r := model.RoleOfStage(stage)
	for _, rv := range s.settings.Roles {
		if rv.Role == r {
			return rv, true
		}
	}
	return api.RoleView{}, false
}

// currentModel is what launching or "使用当前设置" would write now.
func (s *store) currentModel(stage string) *api.StageModel {
	switch stage {
	case model.StagePrep, model.StageExport:
		return nil
	case model.StageASR:
		// SPEC-v2 I6: the device a launch would name (devices.go); the
		// fixture generator has no devices: cuda:0 as migrated from the
		// legacy settings
		a := s.settings.ASR
		if s.dev == nil {
			return &api.StageModel{Label: asrLabelOf(labelCUDA, a.Model, a.ComputeType, a.Language), Model: a.Model}
		}
		d := s.preferredDevice()
		if d == nil {
			return nil
		}
		return &api.StageModel{Label: asrLabelOf(d.row.Label, d.row.Model, d.row.Precision, a.Language), Model: d.row.Model}
	}
	rv, ok := s.role(stage)
	if !ok || rv.ProviderID == nil || rv.Model == nil {
		return nil
	}
	p, _ := s.providerByID(*rv.ProviderID)
	temp := rv.Temperature
	params := &model.RoleParams{Temperature: &temp, Window: rv.Window, MaxTokens: rv.MaxTokens, Batch: rv.Batch}
	if th := rv.Thinking; th != "" && th != model.ThinkingOff {
		params.Thinking = &th
	}
	return &api.StageModel{Label: mockLLMLabel(p.Name, *rv.Model, *params), ProviderID: p.ID, ProviderName: p.Name, Model: *rv.Model, Params: params}
}

// mockLLMLabel is internal/api's llmLabel: "DeepSeek / deepseek-chat · 温度
// 0.3 · 窗口 3 · 深度思考 高" (off is not shown).
func mockLLMLabel(provider, name string, p model.RoleParams) string {
	label := fmt.Sprintf("%s / %s", provider, name)
	if p.Temperature != nil && !model.ThinkingOn(p) {
		label += fmt.Sprintf(" · 温度 %g", *p.Temperature)
	}
	if p.Window != nil {
		label += fmt.Sprintf(" · 窗口 %d", *p.Window)
	}
	if lv, ok := map[string]string{model.ThinkingLow: "低", model.ThinkingHigh: "高", model.ThinkingMax: "最高"}[model.ThinkingLevel(p)]; ok {
		label += " · 深度思考 " + lv
	}
	return label
}

func snapshotView(m *api.StageModel) api.SnapshotView {
	if m == nil {
		return api.SnapshotView{Label: "（无）"}
	}
	return api.SnapshotView{Label: m.Label, ProviderID: m.ProviderID, ProviderName: m.ProviderName, Model: m.Model, Params: m.Params}
}

// diffs lists the snapshot/current differences of stage `from` and its
// downstream stages; onlyUnusable keeps just the snapshots that cannot be
// used any more (retry, lead decision 7).
func (s *store) diffs(l *api.Lecture, from int, onlyUnusable bool) []api.ModelDiffView {
	var out []api.ModelDiffView
	for _, st := range l.Stages[from:] {
		if st.Model == nil {
			continue
		}
		cur := s.currentModel(st.Stage)
		usable, reason := true, ""
		if st.Model.ProviderID != "" {
			p, ok := s.providerByID(st.Model.ProviderID)
			switch {
			case !ok:
				usable, reason = false, fmt.Sprintf("提供商 %s 已删除", st.Model.ProviderName)
			case p.KeyHint == "":
				usable, reason = false, fmt.Sprintf("提供商 %s 没有设置 API key", p.Name)
			}
		}
		if onlyUnusable && usable {
			continue
		}
		if usable && cur != nil && cur.Label == st.Model.Label {
			continue
		}
		d := api.ModelDiffView{
			LectureID: l.ID, LectureTitle: l.Title, Stage: st.Stage,
			Snapshot: snapshotView(st.Model), Current: snapshotView(cur),
			SnapshotUsable: usable, Reason: reason,
		}
		// v2 G5.5: the example lectures were launched before v2 (their
		// snapshots have no prompt version, i.e. 1); the current settings
		// carry the current one. (The mock still asks only when the model
		// differs; the real server also asks for a prompt version alone.)
		if v := model.PromptVersionOf(st.Stage); v > 0 {
			d.Snapshot.PromptVersion, d.Current.PromptVersion = 1, v
		}
		out = append(out, d)
	}
	return out
}

func earliestStale(l *api.Lecture) int {
	for i, st := range l.Stages {
		if st.Status == model.StatusStale {
			return i
		}
	}
	return -1
}

// diffsFor is used by the fixture generator (v11_needs_model_choice.json).
func diffsFor(lectures []api.Lecture, settings api.Settings, ids []string, stage string) []api.ModelDiffView {
	s := &store{lectures: lectures, settings: settings}
	var out []api.ModelDiffView
	for _, id := range ids {
		l, err := s.findLecture(id)
		if err != nil {
			continue
		}
		from := earliestStale(l)
		if stage != "" {
			from = stageIndex(l, stage)
		}
		if from >= 0 {
			out = append(out, s.diffs(l, from, false)...)
		}
	}
	return out
}

// ---------- mutations (called with s.mu held) ----------

type httpError struct {
	status int
	msg    string
	body   any
}

func (e *httpError) Error() string { return e.msg }

func errNotFound(what string) error { return &httpError{status: 404, msg: "找不到这个" + what} }
func errConflict(msg string) error  { return &httpError{status: 409, msg: msg} }
func errBad(msg string) error       { return &httpError{status: 400, msg: msg} }

func needsChoice(diffs []api.ModelDiffView) error {
	return &httpError{status: 409, msg: model.ErrNeedsModelChoice, body: api.NeedsModelChoice{Error: model.ErrorInfo{Code: model.ErrNeedsModelChoice}, Diff: diffs}}
}

func waitFor(stage string) string {
	switch stage {
	case model.StagePrep, model.StageExport:
		return model.WaitCPUSlot
	case model.StageASR:
		return model.WaitGPUSlot
	}
	return model.WaitAPISlot
}

func resetStage(st *api.StageView, status string) {
	st.Status = status
	st.Progress = 0
	st.Error, st.Note, st.StaleReason = nil, nil, nil
	st.ErrorInfo, st.NoteInfo, st.WarningInfo, st.StaleInfo = nil, nil, nil, nil
	st.StartedAt, st.FinishedAt, st.DurationMS = nil, nil, nil
	st.Stats = api.UsageStats{}
	st.ASRTiming = nil  // the timing of the last run (the real asr stage clears it per attempt)
	st.Checkpoint = nil // a new job: an older checkpoint is void
	st.WaitingFor = nil
	st.LLM, st.LastActivityAt = nil, nil // v3.2.x: the requests of the last run
	if status == model.StatusQueued {
		w := waitFor(st.Stage)
		st.WaitingFor = &w
	}
}

// restartFrom queues stage i and resets its downstream stages to pending
// (a rerun, SPEC §4.6; done stages downstream keep nothing but their snapshot).
func (s *store) restartFrom(l *api.Lecture, i int, choice string) {
	if choice == model.ModelChoiceCurrent {
		for j := i; j < len(l.Stages); j++ {
			l.Stages[j].Model = s.currentModel(l.Stages[j].Stage)
		}
	}
	for j := i; j < len(l.Stages); j++ {
		s.retireUsage(l.ID, l.Stages[j].Stats) // usage_total keeps what the old jobs used (r7.go)
	}
	resetStage(&l.Stages[i], model.StatusQueued)
	for j := i + 1; j < len(l.Stages); j++ {
		resetStage(&l.Stages[j], model.StatusPending)
	}
	delete(s.progress0, l.ID+"/"+l.Stages[i].Stage)
	if a := stageIndex(l, model.StageASR); a >= 0 && i <= a {
		delete(s.noWords, l.ID) // a new transcription has word timestamps (P4.2)
	}
}

func (s *store) changed(l *api.Lecture) {
	s.finalize(l)
	for i := range s.courses {
		if s.courses[i].ID == l.CourseID {
			finalizeCourse(&s.courses[i], s.lectures, s.kctx())
		}
	}
	s.notify(api.ChangedEvent{Kind: "lecture", ID: l.ID}, api.ChangedEvent{Kind: "course", ID: l.CourseID})
}

func (s *store) launch(id string, now time.Time) (*api.Lecture, error) {
	l, err := s.findLecture(id)
	if err != nil {
		return nil, err
	}
	if !l.Draft {
		return nil, errConflict("这节讲座已经启动")
	}
	t := now.UTC().Format(time.RFC3339)
	l.LaunchedAt = &t
	for i := range l.Stages {
		l.Stages[i].Model = s.currentModel(l.Stages[i].Stage)
		resetStage(&l.Stages[i], model.StatusPending)
	}
	resetStage(&l.Stages[0], model.StatusQueued)
	s.changed(l)
	return l, nil
}

func (s *store) update(id, choice string) (*api.Lecture, error) {
	l, err := s.findLecture(id)
	if err != nil {
		return nil, err
	}
	i := earliestStale(l)
	if i < 0 {
		return nil, errConflict("这节讲座没有需要更新的阶段")
	}
	if choice == "" {
		if d := s.diffs(l, i, false); len(d) > 0 {
			return nil, needsChoice(d)
		}
	} else if err := s.checkChoice(l, i, choice); err != nil {
		return nil, err
	}
	s.restartFrom(l, i, choice)
	s.changed(l)
	return l, nil
}

func (s *store) checkChoice(l *api.Lecture, from int, choice string) error {
	switch choice {
	case model.ModelChoiceCurrent:
		return nil
	case model.ModelChoiceSnapshot:
		if d := s.diffs(l, from, true); len(d) > 0 {
			return errConflict("“沿用原模型”不可用：" + d[0].Reason)
		}
		return nil
	}
	return errBad("model_choice 只能是 snapshot 或 current")
}

func (s *store) cancel(id string, now time.Time) (*api.Lecture, error) {
	l, err := s.findLecture(id)
	if err != nil {
		return nil, err
	}
	n := 0
	for i := range l.Stages {
		st := &l.Stages[i]
		if st.Status == model.StatusQueued || st.Status == model.StatusRunning {
			if st.Status == model.StatusRunning {
				t := now.UTC().Format(time.RFC3339)
				st.FinishedAt = &t
			}
			st.Status = model.StatusCancelled
			st.WaitingFor = nil
			settleLLM(st)
			delete(s.progress0, l.ID+"/"+st.Stage)
			n++
		}
	}
	if n == 0 {
		return nil, errConflict("这节讲座没有排队或运行中的阶段")
	}
	s.changed(l)
	return l, nil
}

func (s *store) stageAction(id, stage, action, choice string, now time.Time) (*api.Lecture, error) {
	return s.stageActionWith(id, stage, action, api.ModelChoiceRequest{ModelChoice: choice}, now)
}

// stageActionWith is stageAction with the whole request body (v3.2.x: the
// manual model of a retry, manual.go).
func (s *store) stageActionWith(id, stage, action string, req api.ModelChoiceRequest, now time.Time) (*api.Lecture, error) {
	choice := req.ModelChoice
	if err := checkManual(stage, action, req); err != nil {
		return nil, err
	}
	l, err := s.findLecture(id)
	if err != nil {
		return nil, err
	}
	if model.IsAuxStage(stage) { // v2: title (course_p2.go)
		if err := s.auxAction(l, stage, action); err != nil {
			return nil, err
		}
		return l, nil
	}
	i := stageIndex(l, stage)
	if i < 0 {
		return nil, errBad("未知的阶段：" + stage)
	}
	st := &l.Stages[i]
	allowed := false
	for _, a := range st.AllowedActions {
		allowed = allowed || a == action
	}
	if !allowed {
		return nil, errConflict(fmt.Sprintf("%s当前为 %s，不能执行 %s", model.StageLabel(stage), st.Status, action))
	}
	switch action {
	case model.ActionCancel:
		if st.Status == model.StatusRunning {
			t := now.UTC().Format(time.RFC3339)
			st.FinishedAt = &t
		}
		st.Status, st.WaitingFor = model.StatusCancelled, nil
		settleLLM(st)
		delete(s.progress0, l.ID+"/"+st.Stage)
	case model.ActionRetry:
		switch {
		case choice == model.ModelChoiceManual:
			m, err := s.manualModel(stage, *req.Manual, st.Model)
			if err != nil {
				return nil, err
			}
			st.Model = m // this stage only; downstream and the role untouched
		case choice == "":
			// v3.2.x P1-b (Q9a): the retry of an LLM stage also asks when
			// the settings changed since the run; any other only when a
			// snapshot cannot be used
			if d := s.diffs(l, i, !manualStage(stage)); len(d) > 0 {
				return nil, needsChoice(d)
			}
		default:
			if err := s.checkChoice(l, i, choice); err != nil {
				return nil, err
			}
		}
		if choice == model.ModelChoiceCurrent {
			for j := i; j < len(l.Stages); j++ {
				l.Stages[j].Model = s.currentModel(l.Stages[j].Stage)
			}
		}
		st.Status, st.Attempt = model.StatusQueued, st.Attempt+1
		w := waitFor(stage)
		st.WaitingFor = &w
	case model.ActionRerun:
		if choice == "" {
			if d := s.diffs(l, i, false); len(d) > 0 {
				return nil, needsChoice(d)
			}
		} else if err := s.checkChoice(l, i, choice); err != nil {
			return nil, err
		}
		s.restartFrom(l, i, choice)
	default:
		return nil, errBad("未知的操作：" + action)
	}
	s.changed(l)
	return l, nil
}

func (s *store) deleteLecture(id string, force bool) error {
	l, err := s.findLecture(id)
	if err != nil {
		return err
	}
	if !l.CanDelete && !force {
		// the real back end's code (handlers_lecture.go deleteBusy, P14.3);
		// the v1.1 text as detail
		return codedErr(409, "lecture.running", map[string]any{"title": l.Title}, "这节讲座有阶段正在运行，不能直接删除；可以先取消，或确认“先取消再删除”")
	}
	courseID := l.CourseID
	for i := range s.lectures {
		if s.lectures[i].ID == id {
			s.lectures = append(s.lectures[:i], s.lectures[i+1:]...)
			break
		}
	}
	for i := range s.courses {
		if s.courses[i].ID == courseID {
			finalizeCourse(&s.courses[i], s.lectures, s.kctx())
		}
	}
	s.notify(api.ChangedEvent{Kind: "course", ID: courseID})
	s.notify(api.ChangedEvent{Kind: "lecture", ID: id}) // as the real back end (F-real)
	return nil
}

func (s *store) patchLecture(id string, p api.LecturePatch) (*api.Lecture, error) {
	l, err := s.findLecture(id)
	if err != nil {
		return nil, err
	}
	// v2 P13: titles, the recording time (409 RenumberConflict), seq read-only
	if err := s.patchLectureP2(l, p); err != nil {
		return nil, err
	}
	if p.RecordedAt != nil {
		if c, err := s.findCourse(l.CourseID); err == nil {
			s.finalize(l)
			s.fnChanged(c) // other lectures' numbers may move too
			return l, nil
		}
	}
	s.changed(l)
	return l, nil
}

func (s *store) newID(prefix string) string {
	s.idSeq++
	return fmt.Sprintf("%s%0*d", prefix, 26-len(prefix), s.idSeq)
}

// addDraft creates a lecture from an upload (v1.1 A3: uploads are drafts).
func (s *store) addDraft(courseID, title string, now time.Time) api.Lecture {
	seq := 0
	for _, l := range s.lectures {
		if l.CourseID == courseID && l.Seq > seq {
			seq = l.Seq
		}
	}
	l := api.Lecture{
		ID: s.newID("01JMOCK"), CourseID: courseID, Title: title,
		CreatedAt: now.UTC().Format(time.RFC3339), Seq: seq + 1,
		Exports: []api.ExportFile{},
	}
	for _, st := range model.LectureStages {
		l.Stages = append(l.Stages, api.StageView{Stage: st, Status: model.StatusPending, AllowedActions: []string{}})
	}
	s.lectures = append(s.lectures, l)
	p := &s.lectures[len(s.lectures)-1]
	s.changed(p)
	return *p
}

func (s *store) createCourse(in api.CourseCreate, now time.Time) (*api.Course, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errBad("课程名称不能为空")
	}
	c := api.Course{ID: s.newID("01JCMOCK"), Name: name, CreatedAt: now.UTC().Format(time.RFC3339), GlossaryVersion: 0}
	if in.Code != nil && strings.TrimSpace(*in.Code) != "" {
		code := strings.TrimSpace(*in.Code)
		c.Code = &code
	}
	finalizeCourse(&c, s.lectures, s.kctx())
	s.courses = append(s.courses, c)
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
	return &s.courses[len(s.courses)-1], nil
}

func (s *store) patchCourse(id string, p api.CoursePatch) (*api.Course, error) {
	c, err := s.findCourse(id)
	if err != nil {
		return nil, err
	}
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" {
			return nil, errBad("课程名称不能为空")
		}
		c.Name = n
	}
	if p.Code != nil {
		code := strings.TrimSpace(*p.Code)
		if code == "" {
			c.Code = nil
		} else {
			c.Code = &code
		}
	}
	s.notify(api.ChangedEvent{Kind: "course", ID: c.ID})
	return c, nil
}

// batch runs fn for every id and collects BatchResults (A7: one by one).
func (s *store) batch(ids []string, fn func(id string) (*api.Lecture, error)) api.BatchResponse {
	out := api.BatchResponse{Results: []api.BatchResult{}}
	for _, id := range ids {
		l, err := fn(id)
		r := api.BatchResult{ID: id, OK: err == nil}
		if err != nil {
			r.Error = err.Error()
			var he *httpError
			if errors.As(err, &he) && he.body != nil {
				if b, ok := he.body.(api.ErrorBody); ok { // coded (P14.3): code for the front end, text as before
					info := b.Error
					r.ErrorInfo, r.Error = &info, info.Detail
				} else if nm, ok := he.body.(api.NeedsModelChoice); ok { // as the real batchError
					info := nm.Error
					r.ErrorInfo, r.Error = &info, "模型设置已变化，需要选择沿用原模型还是使用当前设置"
				} else {
					r.Error = "需要选择模型"
				}
			}
		} else if l != nil {
			cp := *l
			r.Lecture = &cp
		}
		out.Results = append(out.Results, r)
	}
	return out
}
