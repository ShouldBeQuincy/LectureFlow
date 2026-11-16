// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The v2 glossary janitor of the mock (SPEC-v2 G1–G3, G6; lead rulings 4,
// 5, 8, 11): term tiers and locks on the example glossaries, "整理术语表"
// (a simulated glossary_review run when the glossary_janitor role is
// configured, the G2 common-word rule at once when it is not), the report
// and its undo. Behaviour and messages follow internal/api/handlers_glossary.go
// and internal/glossary (reasons of the rule: "Common English word
// (automatic rule)." / "Plural of 'x' (automatic rule).").
//
// The simulated janitor applies the same rule with model-style reasons,
// disables a few generic words, merges plurals and drops actions on locked
// terms into the report's rejected list. Under ?mock=unset the role is not
// configured (settings.go), so "整理术语表" uses the rule.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/glossary"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/stages"
)

// whyLockedText is the real report's Chinese text of janitor.rejected.locked
// (stages' whyText; the front end translates WhyCode, P14.3).
const whyLockedText = "条目已被用户锁定"

func (m *mock) registerJanitor(mux *http.ServeMux) {
	a := m.api
	mux.HandleFunc("POST /api/courses/{id}/glossary/review", a(m.reviewGlossary))
	mux.HandleFunc("GET /api/courses/{id}/glossary/report", a(m.glossaryReport))
	mux.HandleFunc("POST /api/courses/{id}/glossary/report/undo", a(m.undoJanitorItem))
}

// ---------- example data ----------

// mockCommonWords stands in for internal/glossary/commonwords.txt (G2).
var mockCommonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`key public private block state world round real zero algorithm process thread
		join transaction page file memory cache table index order model system data analysis practical example
		lock signature message function hash code number line time way thing mode`) {
		mockCommonWords[w] = true
	}
}

// mockIsCommon is the G2 pre-screen: a single word, not all capitals,
// without digits or hyphens, whose lower-case form (or that form without a
// trailing s / es) is a common word.
func mockIsCommon(en string) bool {
	if en == "" || strings.ContainsAny(en, " -0123456789") || strings.ToUpper(en) == en {
		return false
	}
	l := strings.ToLower(en)
	return mockCommonWords[l] || mockCommonWords[strings.TrimSuffix(l, "s")] || mockCommonWords[strings.TrimSuffix(l, "es")]
}

// termNote holds the v2 state of an example term that differs from what an
// extractor writes (enforce, unlocked, zh from the extractor).
type termNote struct {
	reference bool
	locked    bool
	zhSource  string
	note      string // janitor_note
}

// termNotes: who changed what in the example glossaries. CSC1003 went
// through one janitor run (v2_janitor_reports.json, built by
// buildJanitorReports); several terms were edited by the user (locked).
var termNotes = map[string]map[string]termNote{
	"CSC1003": {
		"key":                {reference: true, note: "Common word; technical sense mostly in compounds such as 'secret key'."},
		"public":             {reference: true, note: "Common word; 'public knowledge' is not 'public key'."},
		"block":              {reference: true, note: "Common word; enforce 'block cipher' instead."},
		"state":              {reference: true, note: "Common word; technical only as 'cipher state'."},
		"world":              {note: "Generic word with no technical meaning here."},
		"algorithms":         {note: "Plural of 'algorithm'."},
		"IV":                 {zhSource: model.ZhSourceJanitor, note: "Align with 'initialization vector'."},
		"nonce":              {locked: true, zhSource: model.ZhSourceUser},
		"Caesar cipher":      {locked: true, zhSource: model.ZhSourceUser},
		"frequency analysis": {locked: true},
		"S-box":              {locked: true, zhSource: model.ZhSourceUser},
	},
	"COMP2120": {
		"thrashing": {locked: true, zhSource: model.ZhSourceUser},
	},
}

// annotateTerms fills the v2 fields of a fixture glossary (manual terms are
// locked with the user's zh, G1/G6).
func annotateTerms(code string, ts []api.Term) {
	notes := termNotes[code]
	for i := range ts {
		t := &ts[i]
		t.Tier, t.ZhSource, t.CommonWord = model.TierEnforce, model.ZhSourceExtractor, mockIsCommon(t.En)
		if t.Source == model.SourceManual {
			t.Locked, t.ZhSource = true, model.ZhSourceUser
		}
		n, ok := notes[t.En]
		if !ok {
			continue
		}
		if n.reference {
			t.Tier = model.TierReference
		}
		t.Locked = t.Locked || n.locked
		if n.zhSource != "" {
			t.ZhSource = n.zhSource
		}
		t.JanitorNote = n.note
	}
}

func termState(t api.Term) model.TermState {
	return model.TermState{Tier: t.Tier, Enabled: t.Enabled, Zh: t.Zh, ZhSource: t.ZhSource}
}

// buildJanitorReports is the example report of CSC1003 (glossary v4 → v5,
// Anthropic / claude-opus-5-5), consistent with termNotes; used by the
// fixture generator (v2_janitor_reports.json) and checked by the tests.
func buildJanitorReports(courses []api.Course) []model.JanitorReport {
	out := []model.JanitorReport{}
	for i := range courses {
		c := &courses[i]
		if deref(c.Code) != "CSC1003" {
			continue
		}
		ts := fixtureTerms(c)
		byEn := map[string]api.Term{}
		for _, t := range ts {
			byEn[t.En] = t
		}
		item := func(en, action string, before func(*model.TermState)) model.JanitorItem {
			t := byEn[en]
			it := model.JanitorItem{TermID: t.ID, En: t.En, Action: action, Reason: t.JanitorNote, After: termState(t)}
			it.Before = it.After
			before(&it.Before)
			return it
		}
		wasEnforce := func(s *model.TermState) { s.Tier = model.TierEnforce }
		wasEnabled := func(s *model.TermState) { s.Enabled = true }
		merge := item("algorithms", model.JanitorMerge, wasEnabled)
		merge.Into, merge.IntoEn = byEn["algorithm"].ID, "algorithm"
		rep := model.JanitorReport{
			CourseID: c.ID, FromVersion: c.GlossaryVersion - 1, Version: c.GlossaryVersion,
			CreatedAt: "2026-10-22T19:47:30Z", ProviderName: "Anthropic", Model: "claude-opus-5-5",
			Items: []model.JanitorItem{
				item("key", model.JanitorReference, wasEnforce),
				item("public", model.JanitorReference, wasEnforce),
				item("block", model.JanitorReference, wasEnforce),
				item("state", model.JanitorReference, wasEnforce),
				item("world", model.JanitorDisable, wasEnabled),
				merge,
				item("IV", model.JanitorFixZh, func(s *model.TermState) { s.Zh, s.ZhSource = "初始向量", model.ZhSourceExtractor }),
			},
			Rejected: []model.JanitorRejected{
				{TermID: byEn["Kerckhoffs's principle"].ID, Action: model.JanitorReference, Reason: "Phrase also used outside cryptography.", Why: whyLockedText, WhyCode: stages.WhyLocked},
				{TermID: "t_01JUNKNOWN000000000000000", Action: model.JanitorEnforce, Reason: "Proper noun: Kasiski examination.", Why: "条目不存在（清道夫只能整理已有条目，不能新增）", WhyCode: stages.WhyUnknownTerm},
			},
		}
		out = append(out, rep)
	}
	return out
}

// The later run of CSC1003: nothing to change, one suggestion rejected.
const (
	lastRunStart = "2026-10-23T08:40:20Z"
	lastRunTook  = 50 * time.Second
)

// buildJanitorLastRuns is the last run of each example course whose last
// run changed nothing (the others' last run is their latest report).
func buildJanitorLastRuns(courses []api.Course) []model.JanitorReport {
	out := []model.JanitorReport{}
	for _, rep := range buildJanitorReports(courses) {
		start, _ := time.Parse(time.RFC3339, lastRunStart)
		run := model.JanitorReport{CourseID: rep.CourseID, FromVersion: rep.Version, Version: rep.Version,
			CreatedAt: start.Add(lastRunTook).UTC().Format(time.RFC3339), ProviderName: rep.ProviderName, Model: rep.Model,
			Items: []model.JanitorItem{}, Rejected: rep.Rejected[:1]}
		out = append(out, run)
	}
	return out
}

// ---------- reports ----------

// storeRun records a run like glossary.SaveRunReport: every run becomes
// the last run (janitor_last_run); a run that changed something is also
// stored by version and becomes the latest report (the change list that can
// be undone). Runs without changes leave the reports alone.
func (sc *sylCourse) storeRun(rep model.JanitorReport) {
	if rep.Items == nil {
		rep.Items = []model.JanitorItem{}
	}
	run := rep
	sc.lastRun = &run
	if len(rep.Items) == 0 {
		return
	}
	if sc.reports == nil {
		sc.reports = map[int]model.JanitorReport{}
	}
	sc.reports[rep.Version] = rep
	sc.latest = rep.Version
}

// lastRunOr is glossary.LastRun: the last run, else the latest report.
func (sc *sylCourse) lastRunOr() (model.JanitorReport, bool) {
	if sc.lastRun != nil {
		return *sc.lastRun, true
	}
	rep, ok := sc.reports[sc.latest]
	return rep, ok && sc.latest > 0
}

func runModel(rep model.JanitorReport) string {
	if rep.Fallback {
		return "常用词规则"
	}
	return rep.ProviderName + " / " + rep.Model
}

// janitorTarget is the glossary_janitor role of the store's settings when it
// is usable (a provider that exists and has a key): state.JanitorConfigured.
// Must hold s.mu.
func (s *store) janitorTarget() (provider, modelName string, ok bool) {
	for _, rv := range s.settings.Roles {
		if rv.Role != model.RoleGlossaryJanitor || rv.ProviderID == nil || rv.Model == nil || strings.TrimSpace(*rv.Model) == "" {
			continue
		}
		if p, found := s.providerByID(*rv.ProviderID); found && p.KeyHint != "" {
			return p.Name, strings.TrimSpace(*rv.Model), true
		}
	}
	return "", "", false
}

// janitorConfigured is janitorTarget for a request: under ?mock=unset the
// role comes from that scenario's settings. Must hold s.mu.
func (m *mock) janitorConfigured(r *http.Request, s *store) bool {
	st, err := m.settingsStore(r, s)
	if err != nil {
		return false
	}
	if st != s {
		st.mu.Lock()
		defer st.mu.Unlock()
	}
	_, _, ok := st.janitorTarget()
	return ok
}

// refreshJanitor recomputes Course.Janitor (G3.5). Must hold s.mu.
func (s *store) refreshJanitor(c *api.Course, sc *sylCourse) {
	_, _, ok := s.janitorTarget()
	j := api.JanitorStatus{Configured: ok}
	switch stageOf(c.Stages, model.StageGlossaryReview) {
	case model.StatusQueued, model.StatusRunning:
		j.Running = true
	}
	// Last*: the last run (also one without changes); LastChanges: the
	// changes not undone in the latest report that changed something.
	if run, found := sc.lastRunOr(); found {
		j.LastAt, j.LastVersion, j.LastFallback, j.LastModel = run.CreatedAt, run.Version, run.Fallback, runModel(run)
	}
	if rep, found := sc.reports[sc.latest]; found && sc.latest > 0 {
		for _, it := range rep.Items {
			if !it.Undone {
				j.LastChanges++
			}
		}
	}
	c.Janitor = j
}

// courseOut is the course as a request sees it (Configured follows the
// request's settings scenario).
func (m *mock) courseOut(r *http.Request, s *store, c api.Course) api.Course {
	c.Janitor.Configured = m.janitorConfigured(r, s)
	if ns := s.renumNotices[c.ID]; len(ns) > 0 { // kinds.go
		c.RenumberNotices = append([]api.RenumberNotice(nil), ns...)
	}
	return c
}

// ---------- the rule ----------

var genericWords = map[string]bool{"world": true, "analysis": true, "practical": true, "data": true, "example": true, "thing": true, "way": true}

// applyJanitorRule changes the unlocked enabled terms of sc: plurals of
// another enabled term are disabled (fallback) or merged into it (janitor),
// common words in the enforce tier become reference, and (janitor only)
// generic words are disabled. Locked terms are left alone; the janitor
// reports what it would have done to them as rejected. Changed terms get
// the reason as janitor_note. Must hold s.mu.
func applyJanitorRule(sc *sylCourse, fallback bool) ([]model.JanitorItem, []model.JanitorRejected) {
	items := []model.JanitorItem{}
	var rejected []model.JanitorRejected
	byKey := map[string]int{}
	for i, t := range sc.terms {
		byKey[strings.ToLower(t.En)] = i
	}
	for i := range sc.terms {
		t := &sc.terms[i]
		if !t.Enabled {
			continue
		}
		l := strings.ToLower(t.En)
		action, reason, into := "", "", -1
		for _, suf := range []string{"es", "s"} {
			if j, ok := byKey[strings.TrimSuffix(l, suf)]; ok && strings.HasSuffix(l, suf) && j != i && sc.terms[j].Enabled {
				into = j
				break
			}
		}
		switch {
		case into >= 0 && fallback:
			action, reason = model.JanitorDisable, "Plural of '"+sc.terms[into].En+"' (automatic rule)."
		case into >= 0:
			action, reason = model.JanitorMerge, "Plural of '"+sc.terms[into].En+"'."
		case !fallback && genericWords[l]:
			action, reason = model.JanitorDisable, "Generic word with no technical meaning here."
		case t.CommonWord && t.Tier != model.TierReference && fallback:
			action, reason = model.JanitorReference, "Common English word (automatic rule)."
		case t.CommonWord && t.Tier != model.TierReference:
			action, reason = model.JanitorReference, "Common word; the technical sense is only one of its uses here."
		}
		if action == "" {
			continue
		}
		if t.Locked {
			if !fallback {
				rejected = append(rejected, model.JanitorRejected{TermID: t.ID, Action: action, Reason: reason, Why: whyLockedText, WhyCode: stages.WhyLocked})
			}
			continue
		}
		it := model.JanitorItem{TermID: t.ID, En: t.En, Action: action, Reason: reason, Before: termState(*t)}
		switch action {
		case model.JanitorReference:
			t.Tier = model.TierReference
		default: // disable, merge
			t.Enabled = false
			if action == model.JanitorMerge {
				it.Into, it.IntoEn = sc.terms[into].ID, sc.terms[into].En
			}
		}
		t.JanitorNote = reason
		it.After = termState(*t)
		items = append(items, it)
		// G3.3 merge: an empty zh of the target gets the merged term's zh,
		// recorded as its own fix_zh item whose Into points back (undone
		// together with the merge).
		if action == model.JanitorMerge {
			if dst := &sc.terms[into]; dst.Zh == "" && t.Zh != "" && !dst.Locked && dst.ZhSource == model.ZhSourceExtractor {
				fill := model.JanitorItem{TermID: dst.ID, En: dst.En, Action: model.JanitorFixZh, Into: t.ID, IntoEn: t.En,
					Reason: "Filled from merged '" + t.En + "'.", Before: termState(*dst)}
				dst.Zh, dst.ZhSource, dst.JanitorNote = t.Zh, model.ZhSourceJanitor, fill.Reason
				fill.After = termState(*dst)
				items = append(items, fill)
			}
		}
	}
	return items, rejected
}

// runRule applies the rule to the course now and stores its report (the
// glossary version + 1 when something changed); provider "" = fallback.
// note and noteInfo are the report's note and its coded form (v2 P14.3),
// as internal/glossary and internal/stages write them (the page shows
// note_info in the interface language). Must hold s.mu.
func (s *store) runRule(c *api.Course, sc *sylCourse, provider, modelName, note string, noteInfo *model.ErrorInfo) model.JanitorReport {
	fallback := provider == ""
	items, rejected := applyJanitorRule(sc, fallback)
	from := c.GlossaryVersion
	rep := model.JanitorReport{CourseID: c.ID, FromVersion: from, Version: from, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ProviderName: provider, Model: modelName, Fallback: fallback, Note: note, NoteInfo: noteInfo, Items: items, Rejected: rejected}
	if len(items) > 0 {
		rep.Version = from + 1
		c.GlossaryVersion = rep.Version
	}
	sc.storeRun(rep)
	s.refreshJanitor(c, sc)
	finalizeCourse(c, s.lectures, s.kctx())
	s.notify(api.ChangedEvent{Kind: "terms", ID: c.ID}, api.ChangedEvent{Kind: "course", ID: c.ID})
	return rep
}

// ---------- the simulated glossary_review run ----------

// startReview is state.EnqueueGlossaryReview + the scheduler for the mock:
// waiting or queued → nothing (several triggers merge); running → once more
// afterwards; otherwise a new run: queued → running → done. Must hold s.mu.
func (s *store) startReview(c *api.Course, sc *sylCourse) {
	switch stageOf(c.Stages, model.StageGlossaryReview) {
	case model.StatusPending, model.StatusQueued:
		return
	case model.StatusRunning:
		sc.reviewDirty = true
		return
	}
	sc.reviewGen++
	gen, id := sc.reviewGen, c.ID
	c.Stages = setStage(c.Stages, model.StageGlossaryReview, model.StatusQueued, 0, time.Now())
	bumpAttempt(c.Stages, model.StageGlossaryReview, model.ActionRerun)
	s.refreshJanitor(c, sc)
	s.courseChanged(c)
	step := func(d time.Duration, fn func(c *api.Course, now time.Time)) {
		time.AfterFunc(d, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if sc.reviewGen != gen {
				return
			}
			if c, err := s.findCourse(id); err == nil {
				fn(c, time.Now())
			}
		})
	}
	q, run := sylTiming.reviewQueued, sylTiming.review
	step(q, func(c *api.Course, now time.Time) {
		c.Stages = setStage(c.Stages, model.StageGlossaryReview, model.StatusRunning, 0.1, now)
		s.refreshJanitor(c, sc)
		s.courseChanged(c)
	})
	for _, f := range []float64{0.35, 0.6, 0.85} {
		p := f
		step(q+time.Duration(float64(run)*p), func(c *api.Course, now time.Time) {
			for i := range c.Stages {
				if c.Stages[i].Stage == model.StageGlossaryReview && c.Stages[i].Status == model.StatusRunning {
					c.Stages[i].Progress = p
					s.notify(api.ChangedEvent{Kind: "course", ID: c.ID}) // the mock streams no course job events
				}
			}
		})
	}
	step(q+run, func(c *api.Course, now time.Time) {
		if stageOf(c.Stages, model.StageGlossaryReview) != model.StatusRunning {
			return
		}
		provider, modelName, ok := s.janitorTarget()
		if !ok { // appendix D2: the janitor is not usable → the rule, then a failure
			// as glossary_review's fallback (internal/stages): the note and
			// its coded form carry the cause
			reason := "没有可用的提供商"
			s.runRule(c, sc, "", "", glossary.NoteFallbackFailed(reason),
				glossary.NoteInfoFallbackFailed(settings.CodeRoleNotConfigured, reason))
			c.Stages = setStage(c.Stages, model.StageGlossaryReview, model.StatusFailed, 0.1, now)
			msg := "没有可用的提供商：请在设置中为「术语表清道夫」选择提供商和模型（已按常用词规则整理）"
			// coded as the real D2 fallback (codes-p2: the cause's code in params)
			info := model.ErrorInfo{Code: "janitor.failed_fallback", Params: map[string]any{"cause": settings.CodeRoleNotConfigured}, Detail: msg}
			for i := range c.Stages {
				if c.Stages[i].Stage == model.StageGlossaryReview {
					c.Stages[i].Error, c.Stages[i].ErrorInfo = &msg, &info
				}
			}
		} else {
			c.Stages = setStage(c.Stages, model.StageGlossaryReview, model.StatusDone, 1, now)
			s.runRule(c, sc, provider, modelName, "", nil)
		}
		s.refreshJanitor(c, sc)
		s.courseChanged(c)
		if sc.reviewDirty {
			sc.reviewDirty = false
			s.startReview(c, sc)
		}
	})
}

// reviewStageAction: cancel / retry / rerun of the course's glossary_review
// (checked by checkAction). Must hold s.mu.
func (s *store) reviewStageAction(c *api.Course, sc *sylCourse, action string) {
	if action == model.ActionCancel {
		sc.reviewGen++
		sc.reviewDirty = false
		c.Stages = setStage(c.Stages, model.StageGlossaryReview, model.StatusCancelled, 0, time.Now())
		s.refreshJanitor(c, sc)
		s.courseChanged(c)
		return
	}
	attempt := 0
	for _, st := range c.Stages {
		if st.Stage == model.StageGlossaryReview {
			attempt = st.Attempt
		}
	}
	s.startReview(c, sc) // done, failed or cancelled: a new run
	if action == model.ActionRetry {
		for i := range c.Stages {
			if c.Stages[i].Stage == model.StageGlossaryReview {
				c.Stages[i].Attempt = attempt + 1
			}
		}
	}
}

// ---------- handlers ----------

// POST /api/courses/{id}/glossary/review → 202 {queued} | 200 {report}.
func (m *mock) reviewGlossary(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if m.janitorConfigured(r, s) {
		s.startReview(c, sc)
		writeJSON(w, http.StatusAccepted, api.GlossaryReviewResult{Queued: true})
		return
	}
	rep := s.runRule(c, sc, "", "", glossary.NoteFallbackManual, glossary.NoteInfo(glossary.CodeNoteFallbackManual, nil))
	writeJSON(w, http.StatusOK, api.GlossaryReviewResult{Report: &rep})
}

// GET /api/courses/{id}/glossary/report[?version=N | ?run=last]: the
// latest report that changed something, the one of version N, or the last
// run (also one without changes).
func (m *mock) glossaryReport(w http.ResponseWriter, r *http.Request, s *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	raw, run := strings.TrimSpace(r.URL.Query().Get("version")), strings.TrimSpace(r.URL.Query().Get("run"))
	switch {
	case run != "" && (run != "last" || raw != ""):
		writeError(w, http.StatusBadRequest, "run 只能是 last，且不能与 version 同时使用")
		return
	case run == "last":
		rep, ok := sc.lastRunOr()
		if !ok {
			writeError(w, http.StatusNotFound, "这门课程还没有整理过术语表")
			return
		}
		writeJSON(w, http.StatusOK, rep)
		return
	}
	v := sc.latest
	msg := "这门课程还没有改动过术语表的整理报告"
	if raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "version 必须是术语表版本号（非负整数）")
			return
		}
		v, msg = n, "找不到术语表 v"+raw+" 的整理报告"
	}
	rep, ok := sc.reports[v]
	if !ok || v == 0 {
		writeError(w, http.StatusNotFound, msg)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// POST /api/courses/{id}/glossary/report/undo {version, term_id} (G3.5):
// back to Before, locked, janitor_note cleared, version + 1.
func (m *mock) undoJanitorItem(w http.ResponseWriter, r *http.Request, s *store) {
	var in api.JanitorUndoInput
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	in.TermID = strings.TrimSpace(in.TermID)
	if in.Version <= 0 || in.TermID == "" {
		writeError(w, http.StatusBadRequest, "缺少要撤销的改动（version 与 term_id）")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, sc, err := s.sylCourse(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	const gone = "找不到要撤销的改动（报告或术语可能已被删除），请刷新"
	rep, ok := sc.reports[in.Version]
	if !ok {
		writeError(w, http.StatusNotFound, gone)
		return
	}
	k := -1
	for i, it := range rep.Items {
		if it.TermID == in.TermID {
			k = i
		}
	}
	ti := -1
	for i := range sc.terms {
		if sc.terms[i].ID == in.TermID {
			ti = i
		}
	}
	switch {
	case k < 0 || ti < 0:
		writeError(w, http.StatusNotFound, gone)
		return
	case rep.Items[k].Undone:
		writeError(w, http.StatusConflict, "这一项已经撤销过了")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	items := append([]model.JanitorItem(nil), rep.Items...)
	restore := func(i, ti int) {
		t := &sc.terms[ti]
		b := items[i].Before
		t.Tier, t.Enabled, t.Zh, t.ZhSource = b.Tier, b.Enabled, b.Zh, b.ZhSource
		t.Locked, t.JanitorNote = true, ""
		items[i].Undone, items[i].UndoneAt = true, now
	}
	restore(k, ti)
	// undoing a merge also restores the zh it filled into the target (its
	// own fix_zh item, Into = the merged term), unless that was undone first
	if items[k].Action == model.JanitorMerge {
		for i, it := range items {
			if it.Action == model.JanitorFixZh && it.Into == in.TermID && !it.Undone {
				for tj := range sc.terms {
					if sc.terms[tj].ID == it.TermID {
						restore(i, tj)
					}
				}
			}
		}
	}
	if sc.lastRun != nil && sc.lastRun.Version == rep.Version && sc.lastRun.CreatedAt == rep.CreatedAt {
		sc.lastRun.Items = items
	}
	rep.Items = items
	sc.reports[in.Version] = rep
	s.termsChanged(c, sc) // version + 1, changed terms + course
	s.refreshJanitor(c, sc)
	writeJSON(w, http.StatusOK, rep)
}
