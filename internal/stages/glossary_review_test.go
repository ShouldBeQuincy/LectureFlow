// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// ---------- helpers (pipeline-A, SPEC-v2 G1–G6) ----------

const janitorModel = "janitor-model"

// janitorDeps returns Deps on the settings path (v1.1 A5) with the
// glossary_janitor role assigned to the imported fake provider, so
// state.JanitorConfigured is true.
func (e *env) janitorDeps() Deps {
	e.t.Helper()
	set, err := settings.Open(e.ctx, e.st, e.cfg)
	e.must(err)
	e.must(set.SetRole(e.ctx, model.RoleGlossaryJanitor, "deepseek", janitorModel, model.RoleParams{}))
	c := llm.NewWithSettings(set, nil, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	d := e.deps(c)
	d.Settings = set
	return d
}

// term2 inserts an enabled extractor term (v2 columns set); mod adjusts it.
func (e *env) term2(en, zh, source string, mod func(*model.Term)) model.Term {
	e.t.Helper()
	t := model.Term{ID: db.NewID(), CourseID: e.course.ID, En: en, EnKey: db.EnKey(en), Zh: zh, Source: source,
		Enabled: true, Tier: model.TierEnforce, ZhSource: model.ZhSourceExtractor}
	if mod != nil {
		mod(&t)
	}
	e.must(e.st.InsertTerm(e.ctx, t))
	return t
}

func (e *env) getTerm(id string) model.Term {
	e.t.Helper()
	t, err := e.st.GetTerm(e.ctx, id)
	e.must(err)
	return t
}

func (e *env) glossaryVersion() int {
	e.t.Helper()
	c, err := e.st.GetCourse(e.ctx, e.course.ID)
	e.must(err)
	return c.GlossaryVersion
}

// enqueueReview queues the course's glossary_review (the janitor must be configured).
func (e *env) enqueueReview() {
	e.t.Helper()
	queued, err := state.EnqueueGlossaryReview(e.ctx, e.st, e.course.ID)
	e.must(err)
	if !queued {
		e.t.Fatal("EnqueueGlossaryReview did not queue (janitor not configured?)")
	}
}

func lockedTerm(t *model.Term) { t.Locked = true; t.ZhSource = model.ZhSourceUser }

// janitorEntries parses the user message of a janitor request.
func janitorEntries(t *testing.T, user string) (open, locked []map[string]any) {
	t.Helper()
	for _, l := range strings.Split(user, "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("janitor user line %q: %v", l, err)
		}
		if m["locked"] == true {
			locked = append(locked, m)
		} else {
			open = append(open, m)
		}
	}
	return open, locked
}

// ---------- MergeTerms (G2, G6) ----------

// TestMergeTermsRejectsSources: G6 rule 1 — only syllabus and material may
// write terms through MergeTerms; transcript, repair, summary, translation,
// chapters, review, manual and unknown sources are refused and nothing is written.
func TestMergeTermsRejectsSources(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	e.material(nil, "a.pdf", []string{"zorblax appears in the slides"})
	v0 := e.glossaryVersion()
	for _, src := range []string{"transcript", "asr", "repair", "summary", "translation", "translate", "chapters",
		model.SourceReview, model.SourceManual, "", "Syllabus", "unknown"} {
		_, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{"zorblax", "佐布拉克斯"}}, src)
		if err == nil {
			t.Errorf("MergeTerms with source %q must be refused (G6)", src)
		}
	}
	if n := len(e.terms()); n != 0 {
		t.Errorf("%d terms written by refused merges", n)
	}
	if e.glossaryVersion() != v0 || len(e.invalidations()) != 0 || e.ev.has("terms", e.course.ID) {
		t.Error("a refused merge must not bump the version, invalidate or notify")
	}
	for _, src := range []string{model.SourceSyllabus, model.SourceMaterial} {
		if _, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{"zorblax " + src, ""}}, src); err != nil {
			t.Errorf("source %s refused: %v", src, err)
		}
	}
}

// TestMergeTermsCommonWordAndFallback: without a janitor the G2 fallback
// runs in the merge transaction (one version bump in all), common_word is
// computed, locked terms are not touched, new terms come from the extractor.
func TestMergeTermsCommonWordAndFallback(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil) // legacy config: no glossary_janitor role → not configured
	public := e.term2("public", "公钥", model.SourceMaterial, nil)
	private := e.term2("private", "", model.SourceSyllabus, lockedTerm) // locked: zh not filled, tier kept
	keepEn := e.term2("Feistel", "", model.SourceMaterial, func(t *model.Term) { t.ZhSource = model.ZhSourceJanitor })
	algo := e.term2("algorithm", "", model.SourceSyllabus, nil)
	e.material(nil, "a.pdf", []string{"The public key and the private key. AES is a block cipher."})
	v0 := e.glossaryVersion()

	res, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{
		{"public", "公开"}, {"private", "私钥"}, {"Feistel", "费斯妥"}, {"algorithm", "算法"},
		{"algorithms", "算法"}, {"world", "世界"}, {"AES", "高级加密标准"}, {"Block Cipher", "分组密码"},
	}, model.SourceMaterial)
	if err != nil {
		t.Fatal(err)
	}
	if v := e.glossaryVersion(); v != v0+1 || res.Version != v {
		t.Errorf("version %d (result %d), want exactly %d", v, res.Version, v0+1)
	}
	m := termMap(e.terms())
	check := func(key, tier string, enabled, common bool, zh, zhSource string) {
		t.Helper()
		x := m[key]
		if x.Tier != tier || x.Enabled != enabled || x.CommonWord != common || x.Zh != zh || x.ZhSource != zhSource {
			t.Errorf("%s: tier %s enabled %v common %v zh %q zh_source %s; want %s %v %v %q %s",
				key, x.Tier, x.Enabled, x.CommonWord, x.Zh, x.ZhSource, tier, enabled, common, zh, zhSource)
		}
	}
	check("public", model.TierReference, true, true, "公钥", model.ZhSourceExtractor)
	check("private", model.TierEnforce, true, true, "", model.ZhSourceUser)             // locked
	check("feistel", model.TierEnforce, true, false, "", model.ZhSourceJanitor)         // the janitor kept it in English
	check("algorithm", model.TierEnforce, true, false, "算法", model.ZhSourceExtractor)   // empty zh filled
	check("algorithms", model.TierEnforce, false, false, "算法", model.ZhSourceExtractor) // plural disabled
	check("world", model.TierReference, true, true, "世界", model.ZhSourceExtractor)      // new common word
	check("aes", model.TierEnforce, true, false, "高级加密标准", model.ZhSourceExtractor)
	check("block cipher", model.TierEnforce, true, false, "分组密码", model.ZhSourceExtractor)
	if x := m["algorithms"]; x.Source != model.SourceMaterial || x.JanitorNote != "Plural of 'algorithm' (automatic rule)." {
		t.Errorf("algorithms %+v", x)
	}
	if x := e.getTerm(private.ID); x.JanitorNote != "" || !x.Locked {
		t.Errorf("locked private %+v", x)
	}
	_, _, _ = public, keepEn, algo

	rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
	if err != nil {
		t.Fatalf("fallback report: %v", err)
	}
	var ens []string
	for _, it := range rep.Items {
		ens = append(ens, it.En+":"+it.Action)
	}
	sort.Strings(ens)
	if !rep.Fallback || infoCode(rep.NoteInfo) != glossary.CodeNoteFallbackMerge || rep.FromVersion != v0 || rep.Version != v0+1 ||
		strings.Join(ens, ",") != "algorithms:disable,public:reference,world:reference" {
		t.Errorf("report %+v items %v", rep, ens)
	}
	if len(e.invalCauses()) != 1 || e.invalCauses()[0].Kind != state.StaleGlossary {
		t.Errorf("invalidations %+v", e.invalCauses())
	}

	// nothing new: no bump, no new report
	res, err = MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{"public", "公开"}}, model.SourceSyllabus)
	if err != nil || res.Changed() || e.glossaryVersion() != v0+1 || len(e.invalidations()) != 1 {
		t.Errorf("unchanged merge %+v %v version %d", res, err, e.glossaryVersion())
	}

	// a user who moves a term back to enforce locks it: the next merge leaves it alone
	x := e.getTerm(public.ID)
	x.Tier, x.Locked = model.TierEnforce, true
	e.must(e.st.UpdateTerm(e.ctx, x))
	if _, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{"zero", "零"}}, model.SourceMaterial); err != nil {
		t.Fatal(err)
	}
	if g := e.getTerm(public.ID); g.Tier != model.TierEnforce {
		t.Errorf("locked public moved by the fallback: %+v", g)
	}
}

// TestMergeTermsWithJanitor: with a configured janitor the merge only marks
// common_word; tiers are left to glossary_review.
func TestMergeTermsWithJanitor(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	v0 := e.glossaryVersion()
	if _, err := MergeTerms(e.ctx, d, e.course.ID, []FoundTerm{{"public", "公钥"}, {"algorithm", ""}, {"algorithms", ""}}, model.SourceSyllabus); err != nil {
		t.Fatal(err)
	}
	m := termMap(e.terms())
	if x := m["public"]; !x.CommonWord || x.Tier != model.TierEnforce || x.JanitorNote != "" || x.ZhSource != model.ZhSourceExtractor {
		t.Errorf("public %+v", x)
	}
	if !m["algorithms"].Enabled {
		t.Error("the fallback must not run when the janitor is configured")
	}
	if e.glossaryVersion() != v0+1 {
		t.Errorf("version %d", e.glossaryVersion())
	}
	if _, err := glossary.LatestReport(e.ctx, e.st, e.course.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("no report expected: %v", err)
	}
}

// TestTermStagesEnqueueReview: glossary_seed and material_terms queue the
// glossary_review when the janitor is configured (ruling 4), not otherwise.
func TestTermStagesEnqueueReview(t *testing.T) {
	for _, configured := range []bool{false, true} {
		e := newEnv(t)
		d := e.deps(nil)
		if configured {
			d = e.janitorDeps()
		}
		md := "# Course\nCaches and write-back.\n"
		e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
		e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
		if r := e.run(e.stage(d, model.StageGlossarySeed), e.course.ID); r.err != nil {
			t.Fatal(r.err)
		}
		j, err := e.st.GetCurrentJob(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview)
		switch {
		case configured && (err != nil || j.Status != model.StatusPending):
			t.Errorf("configured: glossary_review after glossary_seed %+v %v", j, err)
		case !configured && !errors.Is(err, db.ErrNotFound):
			t.Errorf("not configured: no glossary_review expected, got %+v %v", j, err)
		}
		if configured { // a done review is queued again by material_terms
			e.markDone(model.ScopeCourse, e.course.ID, model.StageGlossaryReview)
			m := e.material(nil, "a.pdf", []string{"Cache hierarchy"})
			if r := e.run(e.stage(d, model.StageMaterialTerms), m.ID); r.err != nil {
				t.Fatal(r.err)
			}
			if j, err := e.st.GetCurrentJob(e.ctx, model.ScopeCourse, e.course.ID, model.StageGlossaryReview); err != nil || j.Status != model.StatusPending {
				t.Errorf("glossary_review after material_terms %+v %v", j, err)
			}
		}
	}
}

// ---------- glossary_review (G3) ----------

func TestGlossaryReviewActions(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	S, M := model.SourceSyllabus, model.SourceMaterial
	public := e.term2("public", "公钥", M, func(t *model.Term) { t.CommonWord = true })
	world := e.term2("world", "世界", M, nil)
	worlds := e.term2("worlds", "", M, nil)
	algo := e.term2("algorithm", "", S, nil)
	algos := e.term2("algorithms", "算法", M, nil)
	iv := e.term2("IV", "初始向量", M, nil)
	e.term2("initialization vector", "初始化向量", S, nil)
	vig := e.term2("Vigenère cipher", "维吉尼亚密码", S, func(t *model.Term) { t.Tier = model.TierReference })
	aes := e.term2("AES", "高级加密标准", S, nil)
	zero := e.term2("zero", "零", M, lockedTerm)
	data := e.term2("data", "数据", M, func(t *model.Term) { t.Enabled = false })
	datum := e.term2("datum", "", M, nil)
	key := e.term2("key", "密钥", M, nil)
	kasiski := e.term2("Kasiski examination", "卡西斯基试验", M, nil)
	block := e.term2("block", "分组", M, nil)
	secret := e.term2("secret", "秘密", M, func(t *model.Term) { t.ZhSource = model.ZhSourceUser }) // unlocked, but a user zh
	e.material(nil, "w1.pdf", []string{"The algorithms are public knowledge. The public key is published.", "The IV is random."})
	md := "# CSC1003 Cryptography\nWeek 1: the Vigenère cipher.\n"
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
	code := "CSC1003"
	e.must(e.st.SetCourseCode(e.ctx, e.course.ID, &code))

	actions := fmt.Sprintf(`{"actions": [
		{"id": %q, "action": "reference", "reason": "Common word; technical sense only in 'public key'."},
		{"id": %q, "action": "disable", "reason": "Generic word with no technical meaning here."},
		{"id": %q, "action": "merge", "into": %q, "reason": "Plural of 'algorithm'."},
		{"id": %q, "action": "fix_zh", "zh": "初始化向量", "reason": "Align with 'initialization vector'."},
		{"id": %q, "action": "enforce", "reason": "Proper noun."},
		{"id": %q, "action": "fix_zh", "zh": "", "reason": "Usually kept in English."},
		{"id": %q, "action": "disable", "reason": "Locked entry."},
		{"id": %q, "action": "merge", "into": %q, "reason": "Singular of data."},
		{"id": %q, "action": "merge", "into": %q, "reason": "Itself."},
		{"id": "t_ghost", "action": "enforce", "reason": "New term."},
		{"id": %q, "action": "disable", "reason": "Second action."},
		{"id": %q, "action": "promote", "reason": "Unknown action."},
		{"id": %q, "action": "fix_zh", "reason": "No zh given."},
		{"id": %q, "action": "fix_zh", "zh": "机密", "reason": "Better."},
		{"id": %q, "action": "merge", "into": %q, "reason": "Plural of 'world'."}
	]}`, public.ID, world.ID, algos.ID, algo.ID, iv.ID, vig.ID, aes.ID,
		zero.ID, datum.ID, data.ID, key.ID, key.ID, public.ID, kasiski.ID, block.ID, secret.ID, worlds.ID, world.ID)
	var mu sync.Mutex
	var users []string
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		mu.Lock()
		users = append(users, user)
		mu.Unlock()
		return actions
	})
	e.enqueueReview()
	v0 := e.glossaryVersion()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}

	// the request
	reqs := e.fake.Requests(llmtest.RoleGlossaryJanitor)
	if len(reqs) != 1 || !reqs[0].JSONMode || reqs[0].Model != janitorModel {
		t.Fatalf("requests %+v", reqs)
	}
	if !strings.HasPrefix(reqs[0].System, ctxbuild.JanitorSystem+"\n\nCOURSE: Computer Architecture (CSC1003)\n\nSYLLABUS.md:\n"+strings.TrimSpace(md)) {
		t.Errorf("system:\n%s", reqs[0].System)
	}
	open, locked := janitorEntries(t, users[0])
	if len(locked) != 1 || locked[0]["id"] != zero.ID || len(open) != 14 {
		t.Errorf("entries: %d open, locked %v (disabled data is not sent)", len(open), locked)
	}

	// the applied actions
	type st struct {
		tier            string
		enabled         bool
		zh, zhSource    string
		note            string
		locked, changed bool
	}
	want := map[string]st{
		public.ID:  {model.TierReference, true, "公钥", model.ZhSourceExtractor, "Common word; technical sense only in 'public key'.", false, true},
		world.ID:   {model.TierEnforce, false, "世界", model.ZhSourceExtractor, "Generic word with no technical meaning here.", false, true},
		algos.ID:   {model.TierEnforce, false, "算法", model.ZhSourceExtractor, "Plural of 'algorithm'.", false, true},
		algo.ID:    {model.TierEnforce, true, "算法", model.ZhSourceExtractor, "Filled from merged 'algorithms'.", false, true},
		iv.ID:      {model.TierEnforce, true, "初始化向量", model.ZhSourceJanitor, "Align with 'initialization vector'.", false, true},
		vig.ID:     {model.TierEnforce, true, "维吉尼亚密码", model.ZhSourceExtractor, "Proper noun.", false, true},
		aes.ID:     {model.TierEnforce, true, "", model.ZhSourceJanitor, "Usually kept in English.", false, true},
		zero.ID:    {model.TierEnforce, true, "零", model.ZhSourceUser, "", true, false},
		datum.ID:   {model.TierEnforce, true, "", model.ZhSourceExtractor, "", false, false},
		data.ID:    {model.TierEnforce, false, "数据", model.ZhSourceExtractor, "", false, false},
		key.ID:     {model.TierEnforce, true, "密钥", model.ZhSourceExtractor, "", false, false},
		kasiski.ID: {model.TierEnforce, true, "卡西斯基试验", model.ZhSourceExtractor, "", false, false},
		block.ID:   {model.TierEnforce, true, "分组", model.ZhSourceExtractor, "", false, false},
		secret.ID:  {model.TierEnforce, true, "秘密", model.ZhSourceUser, "", false, false},
		worlds.ID:  {model.TierEnforce, true, "", model.ZhSourceExtractor, "", false, false},
	}
	for id, w := range want {
		g := e.getTerm(id)
		got := st{g.Tier, g.Enabled, g.Zh, g.ZhSource, g.JanitorNote, g.Locked, w.changed}
		if got != w {
			t.Errorf("%s: %+v, want %+v", g.En, got, w)
		}
	}
	if v := e.glossaryVersion(); v != v0+1 {
		t.Errorf("version %d, want exactly %d (one transaction)", v, v0+1)
	}

	// the report
	rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fallback || rep.ProviderName != "deepseek" || rep.Model != janitorModel || rep.FromVersion != v0 || rep.Version != v0+1 || rep.CreatedAt == "" {
		t.Errorf("report header %+v", rep)
	}
	acts := map[string]model.JanitorItem{}
	for _, it := range rep.Items {
		acts[it.TermID] = it
	}
	if len(rep.Items) != 7 {
		t.Errorf("%d items, want 7 (6 actions + the merge target): %+v", len(rep.Items), rep.Items)
	}
	if it := acts[algos.ID]; it.Action != model.JanitorMerge || it.Into != algo.ID || it.IntoEn != "algorithm" || !it.Before.Enabled || it.After.Enabled {
		t.Errorf("merge item %+v", it)
	}
	if it := acts[algo.ID]; it.Action != model.JanitorFixZh || it.Into != algos.ID || it.IntoEn != "algorithms" ||
		it.Reason != "Filled from merged 'algorithms'." || it.Before.Zh != "" || it.After.Zh != "算法" || it.After.ZhSource != model.ZhSourceExtractor {
		t.Errorf("merge target item %+v", it)
	}
	if it := acts[iv.ID]; it.Action != model.JanitorFixZh || it.Before.Zh != "初始向量" || it.After.Zh != "初始化向量" || it.After.ZhSource != model.ZhSourceJanitor || it.Reason == "" {
		t.Errorf("fix_zh item %+v", it)
	}
	if it := acts[vig.ID]; it.Action != model.JanitorEnforce || it.Before.Tier != model.TierReference || it.After.Tier != model.TierEnforce {
		t.Errorf("enforce item %+v", it)
	}
	rejected := map[string]int{}
	for _, x := range rep.Rejected {
		rejected[x.TermID]++
		if x.Why == "" || x.Action == "" || x.WhyCode == "" || whyText[x.WhyCode] != x.Why {
			t.Errorf("rejected without why/action: %+v", x)
		}
		if x.TermID == "t_ghost" && x.WhyCode != WhyUnknownTerm {
			t.Errorf("unknown id rejected as %s", x.WhyCode)
		}
	}
	for _, id := range []string{zero.ID, datum.ID, key.ID, "t_ghost", public.ID, kasiski.ID, block.ID, secret.ID, worlds.ID} {
		if rejected[id] != 1 {
			t.Errorf("term %s rejected %d times, want 1 (%+v)", id, rejected[id], rep.Rejected)
		}
	}
	if len(rep.Rejected) != 9 {
		t.Errorf("%d rejected, want 9", len(rep.Rejected))
	}
	if n := len(e.terms()); n != 16 {
		t.Errorf("%d terms: the janitor must not add terms", n)
	}

	// invalidation, events, stats
	if c := e.invalCauses(); len(c) != 1 || c[0].Kind != state.StaleJanitor {
		t.Errorf("invalidations %+v, want one StaleJanitor", c)
	}
	if !e.ev.has("terms", e.course.ID) || !e.ev.has("course", e.course.ID) {
		t.Errorf("events %v", e.ev.changed)
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteJanitorDone || len(extraCodes(n)) != 0 ||
		!paramsMatch(n, "n", 7, "enforce", 1, "reference", 1, "disable", 1, "merge", 1, "fix_zh", 3, "rejected", 9) {
		t.Errorf("note %+v", n)
	}
	if r.job.Status != model.StatusDone || r.stats.Requests != 1 {
		t.Errorf("job %s stats %+v", r.job.Status, r.stats)
	}
	if p, ok := e.stage(d, model.StageGlossaryReview).(sched.ProviderStage); !ok || p.Provider() != "deepseek" {
		t.Error("glossary_review must report the janitor role's provider")
	}
	if h, err := e.stage(d, model.StageGlossaryReview).InputHash(e.ctx, e.course.ID); h != "" || err != nil {
		t.Errorf("InputHash %q %v, want none", h, err)
	}

	// undo (G3.5): back to the state before, locked, version + 1
	v1 := e.glossaryVersion()
	if _, err := glossary.Undo(e.ctx, e.st, e.course.ID, rep.Version, iv.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g := e.getTerm(iv.ID); g.Zh != "初始向量" || g.ZhSource != model.ZhSourceExtractor || !g.Locked || g.JanitorNote != "" {
		t.Errorf("undone IV %+v", g)
	}
	if e.glossaryVersion() != v1+1 {
		t.Error("undo must bump the version by 1")
	}
	if rep2, _ := glossary.Report(e.ctx, e.st, e.course.ID, rep.Version); func() bool {
		for _, it := range rep2.Items {
			if it.TermID == iv.ID {
				return !it.Undone
			}
		}
		return true
	}() {
		t.Error("report item not marked undone")
	}
	if _, err := glossary.Undo(e.ctx, e.st, e.course.ID, rep.Version, iv.ID, time.Now()); !errors.Is(err, glossary.ErrAlreadyUndone) {
		t.Errorf("second undo: %v", err)
	}
	// undoing the merge restores the merged term and the target it filled
	v2 := e.glossaryVersion()
	if _, err := glossary.Undo(e.ctx, e.st, e.course.ID, rep.Version, algos.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g := e.getTerm(algos.ID); !g.Enabled || !g.Locked {
		t.Errorf("undone merge: %+v", g)
	}
	if g := e.getTerm(algo.ID); g.Zh != "" || !g.Locked || g.JanitorNote != "" {
		t.Errorf("merge target after undo: %+v", g)
	}
	if e.glossaryVersion() != v2+1 {
		t.Errorf("version %d, want %d", e.glossaryVersion(), v2+1)
	}
	if lr, err := glossary.LastRun(e.ctx, e.st, e.course.ID); err != nil || lr.CreatedAt != rep.CreatedAt || lr.Fallback || lr.Model != janitorModel {
		t.Errorf("LastRun %+v %v", lr, err)
	}
}

// TestGlossaryReviewMergeFill: where the zh of a merged term goes (lead,
// Gate review round 2): a target with a no-op action of its own still gets
// it as a separate fix_zh item; a target this run changed gets it in its own
// item; locked targets and user translations are never filled.
func TestGlossaryReviewMergeFill(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	M := model.SourceMaterial
	algo := e.term2("algorithm", "", M, nil) // gets "enforce", which changes nothing
	algos := e.term2("algorithms", "算法", M, nil)
	cipher := e.term2("cipher", "", M, nil) // gets "reference": a real change
	ciphers := e.term2("ciphers", "密码", M, nil)
	lockedT := e.term2("mode", "", M, func(t *model.Term) { t.Locked = true })
	modes := e.term2("modes", "模式", M, nil)
	userT := e.term2("round", "", M, func(t *model.Term) { t.ZhSource = model.ZhSourceUser }) // the user keeps it in English
	rounds := e.term2("rounds", "轮", M, nil)
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		return fmt.Sprintf(`{"actions": [
			{"id": %q, "action": "enforce", "reason": "Named concept."},
			{"id": %q, "action": "merge", "into": %q, "reason": "Plural."},
			{"id": %q, "action": "reference", "reason": "Also everyday."},
			{"id": %q, "action": "merge", "into": %q, "reason": "Plural."},
			{"id": %q, "action": "merge", "into": %q, "reason": "Plural."},
			{"id": %q, "action": "merge", "into": %q, "reason": "Plural."}
		]}`, algo.ID, algos.ID, algo.ID, cipher.ID, ciphers.ID, cipher.ID, modes.ID, lockedT.ID, rounds.ID, userT.ID)
	})
	e.enqueueReview()
	if r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}
	rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	byTerm := map[string][]model.JanitorItem{}
	for _, it := range rep.Items {
		byTerm[it.TermID] = append(byTerm[it.TermID], it)
	}
	for id, its := range byTerm {
		if len(its) != 1 {
			t.Errorf("term %s has %d items", id, len(its))
		}
	}

	// algorithm: the no-op "enforce" is no item; the fill is its own fix_zh item
	if g := e.getTerm(algo.ID); g.Zh != "算法" || g.Tier != model.TierEnforce || g.JanitorNote != "Filled from merged 'algorithms'." {
		t.Errorf("algorithm %+v", g)
	}
	if its := byTerm[algo.ID]; len(its) != 1 || its[0].Action != model.JanitorFixZh || its[0].Into != algos.ID || its[0].After.Zh != "算法" {
		t.Errorf("algorithm items %+v", its)
	}
	// cipher: the fill joins its own "reference" item
	if g := e.getTerm(cipher.ID); g.Zh != "密码" || g.Tier != model.TierReference || g.JanitorNote != "Also everyday; zh filled from merged 'ciphers'." {
		t.Errorf("cipher %+v", g)
	}
	if its := byTerm[cipher.ID]; len(its) != 1 || its[0].Action != model.JanitorReference || its[0].Into != "" ||
		its[0].Reason != "Also everyday; zh filled from merged 'ciphers'." || its[0].Before.Zh != "" ||
		its[0].After != (model.TermState{Tier: model.TierReference, Enabled: true, Zh: "密码", ZhSource: model.ZhSourceExtractor}) {
		t.Errorf("cipher items %+v", its)
	}
	// locked and user-kept targets are never filled; the merges themselves apply
	if g := e.getTerm(lockedT.ID); g.Zh != "" || g.JanitorNote != "" {
		t.Errorf("locked target filled: %+v", g)
	}
	if g := e.getTerm(userT.ID); g.Zh != "" || g.ZhSource != model.ZhSourceUser {
		t.Errorf("user target filled: %+v", g)
	}
	if len(byTerm[lockedT.ID])+len(byTerm[userT.ID]) != 0 {
		t.Error("unfilled targets must have no item")
	}
	for _, id := range []string{algos.ID, ciphers.ID, modes.ID, rounds.ID} {
		if g := e.getTerm(id); g.Enabled {
			t.Errorf("%s not merged", g.En)
		}
	}
	if len(rep.Items) != 6 {
		t.Errorf("%d items, want 6 (4 merges, algorithm's fill, cipher's reference)", len(rep.Items))
	}

	// undoing the cipher merge leaves cipher's own item (not a fill item) alone
	if _, err := glossary.Undo(e.ctx, e.st, e.course.ID, rep.Version, ciphers.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g := e.getTerm(cipher.ID); g.Zh != "密码" || g.Tier != model.TierReference {
		t.Errorf("cipher after undoing the merge %+v", g)
	}
}

// TestGlossaryReviewBatches: more than 400 entries are sent in batches of
// 200 in en_key order; the whole run still bumps the version once.
func TestGlossaryReviewBatches(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	for i := 449; i >= 0; i-- {
		e.term2(fmt.Sprintf("term%03d", i), "", model.SourceMaterial, nil)
	}
	e.term2("zero", "零", model.SourceMaterial, lockedTerm)
	var mu sync.Mutex
	var batches [][]string
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		open, locked := janitorEntries(t, user)
		ens := make([]string, len(open))
		for i, m := range open {
			ens[i] = m["en"].(string)
		}
		mu.Lock()
		batches = append(batches, ens)
		mu.Unlock()
		if len(locked) != 1 {
			t.Errorf("batch without the locked line: %v", locked)
		}
		return fmt.Sprintf(`{"actions": [{"id": %q, "action": "reference", "reason": "First of its batch."}]}`, open[0]["id"])
	})
	e.enqueueReview()
	v0 := e.glossaryVersion()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	sort.Slice(batches, func(i, j int) bool { return batches[i][0] < batches[j][0] })
	if len(batches) != 3 {
		t.Fatalf("%d batches, want 3", len(batches))
	}
	for k, w := range []struct {
		n           int
		first, last string
	}{{200, "term000", "term199"}, {200, "term200", "term399"}, {50, "term400", "term449"}} {
		b := batches[k]
		if len(b) != w.n || b[0] != w.first || b[len(b)-1] != w.last {
			t.Errorf("batch %d: %d %s..%s, want %d %s..%s", k, len(b), b[0], b[len(b)-1], w.n, w.first, w.last)
		}
	}
	if v := e.glossaryVersion(); v != v0+1 {
		t.Errorf("version %d, want exactly %d", v, v0+1)
	}
	m := termMap(e.terms())
	for _, k := range []string{"term000", "term200", "term400"} {
		if m[k].Tier != model.TierReference {
			t.Errorf("%s not applied: %+v", k, m[k])
		}
	}
	if rep, _ := glossary.LatestReport(e.ctx, e.st, e.course.ID); len(rep.Items) != 3 || rep.Version != v0+1 {
		t.Errorf("report %+v", rep)
	}
	if !paramsMatch(r.stats.NoteInfo, "n", 3) || infoCode(r.stats.NoteInfo) != NoteJanitorDone || r.stats.Requests != 3 {
		t.Errorf("stats %+v", r.stats)
	}
}

// TestGlossaryReviewUnavailable: appendix D2 — a janitor that cannot be
// used (401, or no provider for the role) fails permanently after the G2
// fallback tidied the glossary; retryable errors are retried without it.
func TestGlossaryReviewUnavailable(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		e := newEnv(t)
		d := e.janitorDeps()
		public := e.term2("public", "公钥", model.SourceMaterial, nil)
		e.enqueueReview()
		v0 := e.glossaryVersion()
		e.fake.FailNext(llmtest.RoleGlossaryJanitor, 1, llmtest.FailHTTP401)
		r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
		if r.err == nil || !sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed {
			t.Fatalf("err %v (permanent %v), job %s", r.err, sched.IsPermanent(r.err), r.job.Status)
		}
		// the job error says the fallback rule was used; the janitor's own
		// error is its cause (v2 P14.3)
		if jobErrorCode(r.job) != CodeJanitorFailedFallback || codeOf(r.err) != CodeJanitorFailedFallback {
			t.Errorf("job error %q must say the fallback rule was used", r.err)
		}
		var ce *model.CodedError
		if !errors.As(r.err, &ce) || !paramsMatch(&ce.Info, "cause", llm.CodeRejected) {
			t.Errorf("job error %+v: cause", ce)
		}
		if r.stats.NoteInfo == nil || r.stats.NoteInfo.Code != NoteJanitorUnavailable || !paramsMatch(r.stats.NoteInfo, "n", 1) {
			t.Errorf("job note %+v", r.stats.NoteInfo)
		}
		rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
		if err != nil || !rep.Fallback || !strings.Contains(rep.Note, "HTTP 401") || len(rep.Items) != 1 ||
			infoCode(rep.NoteInfo) != glossary.CodeNoteFallbackFailed || !paramsMatch(rep.NoteInfo, "cause", llm.CodeRejected) ||
			!strings.Contains(rep.NoteInfo.Detail, "HTTP 401") {
			t.Errorf("fallback report %+v %v", rep, err)
		}
		if lr, err := glossary.LastRun(e.ctx, e.st, e.course.ID); err != nil || lr.CreatedAt != rep.CreatedAt || !lr.Fallback {
			t.Errorf("last run %+v %v", lr, err)
		}
		if g := e.getTerm(public.ID); g.Tier != model.TierReference {
			t.Errorf("public %+v: the fallback rule must have run", g)
		}
		if e.glossaryVersion() != v0+1 {
			t.Errorf("version %d", e.glossaryVersion())
		}
		if c := e.invalCauses(); len(c) != 1 || c[0].Kind != state.StaleJanitor {
			t.Errorf("invalidations %+v", c)
		}
		if !e.ev.has("terms", e.course.ID) {
			t.Error("no terms event")
		}
	})
	t.Run("no provider for the role", func(t *testing.T) {
		e := newEnv(t)
		e.janitorDeps() // configured, so the job is queued …
		e.enqueueReview()
		e.term2("world", "世界", model.SourceMaterial, nil)
		r := e.run(e.stage(e.deps(nil), model.StageGlossaryReview), e.course.ID) // … but this client has no janitor role
		if r.err == nil || !sched.IsPermanent(r.err) {
			t.Fatalf("err %v", r.err)
		}
		if rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID); err != nil || !rep.Fallback {
			t.Errorf("fallback report %+v %v", rep, err)
		}
	})
	t.Run("retryable", func(t *testing.T) {
		e := newEnv(t)
		d := e.janitorDeps()
		public := e.term2("public", "公钥", model.SourceMaterial, nil)
		e.enqueueReview()
		e.fake.FailNext(llmtest.RoleGlossaryJanitor, 5, llmtest.FailHTTP500)
		r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
		if r.err == nil || sched.IsPermanent(r.err) {
			t.Fatalf("err %v: a 5xx must stay retryable", r.err)
		}
		if _, err := glossary.LastRun(e.ctx, e.st, e.course.ID); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("no fallback for a retryable error: %v", err)
		}
		if g := e.getTerm(public.ID); g.Tier != model.TierEnforce {
			t.Errorf("public %+v changed", g)
		}
	})
}

// TestGlossaryReviewNothingToDo: only locked terms — no request, a report
// without changes, no version bump.
func TestGlossaryReviewNothingToDo(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	e.term2("zero", "零", model.SourceMaterial, lockedTerm)
	e.enqueueReview()
	v0 := e.glossaryVersion()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if e.fake.Count(llmtest.RoleGlossaryJanitor) != 0 || e.glossaryVersion() != v0 || len(e.invalidations()) != 0 {
		t.Errorf("requests %d, version %d, invalidations %v", e.fake.Count(llmtest.RoleGlossaryJanitor), e.glossaryVersion(), e.invalidations())
	}
	if _, err := glossary.LatestReport(e.ctx, e.st, e.course.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a run without changes stores no versioned report: %v", err)
	}
	if lr, err := glossary.LastRun(e.ctx, e.st, e.course.ID); err != nil || len(lr.Items) != 0 || lr.Version != v0 || lr.Model != janitorModel {
		t.Errorf("last run %+v %v", lr, err)
	}
	if infoCode(r.stats.NoteInfo) != NoteJanitorNothing {
		t.Errorf("note %+v", r.stats.NoteInfo)
	}
}

// ---------- G6: source principle (SPEC-v2 implementation order, step 2) ----------

var wordRE = regexp.MustCompile(`[A-Za-zÀ-ÿ][A-Za-zÀ-ÿ-]{3,}`)

// extractWords answers a term request with every word of the user message
// except its label: a fake extractor that would put any transcript word it
// is shown into the glossary.
func extractWords(label string) func(system, user string) string {
	return func(system, user string) string {
		seen := map[string]bool{}
		var ts []FoundTerm
		for _, w := range wordRE.FindAllString(strings.TrimPrefix(user, label), -1) {
			if !seen[strings.ToLower(w)] {
				seen[strings.ToLower(w)] = true
				ts = append(ts, FoundTerm{En: w})
			}
		}
		b, _ := json.Marshal(map[string]any{"terms": ts})
		return string(b)
	}
}

// TestSourcePrinciple: a translated lecture whose transcript, repair,
// human edit, summary and translation contain words the slides and the
// syllabus do not; after glossary_seed, material_terms and glossary_review
// no such word is in the glossary, and every janitor context comes from the
// slides or SYLLABUS.md.
func TestSourcePrinciple(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	md := "# CSC1003 Cryptography\n## Weekly Topics\n| Week | Topic |\n|---|---|\n| 1 | Classical ciphers: the Vigenère cipher |\n| 2 | Public key cryptography |\n"
	pages := []string{
		"Week 1: Classical ciphers\nThe Vigenère cipher repeats a keyword; Kasiski examination finds its length.",
		"Public key cryptography: the public key is published and the private key is kept secret.",
	}

	// a lecture that went through every stage, full of words that exist only in AI output or the transcript
	l := e.lecture("Week 1", "m.mp4")
	only := []string{"zorblax", "quantifrag", "blorptastic", "snerdwick", "glimmerwyrm"}
	ss := []model.Sentence{
		{LectureID: l.ID, Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "Today the zorblax protocol meets the quantifrag engine.", RepairedText: "Today the zorblax protocol meets the quantifrag engine.", ZhStatus: model.ZhNone},
		{LectureID: l.ID, Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "The Vigenère cipher is blorptastic.", RepairedText: "The Vigenère cipher is snerdwick.", ZhStatus: model.ZhNone},
	}
	e.insertSentences(l.ID, ss)
	human := "A glimmerwyrm cipher, repaired by hand."
	e.must(e.st.Tx(e.ctx, func(tx db.Store) error {
		if err := tx.SetHumanText(e.ctx, l.ID, 1, &human); err != nil {
			return err
		}
		return tx.RecomputeText(e.ctx, l.ID, nil)
	}))
	for i, zh := range []string{"今天 zorblax 协议遇到 quantifrag 引擎。", "维吉尼亚密码是 glimmerwyrm。"} {
		z := zh
		e.must(e.st.SetTranslation(e.ctx, l.ID, i, db.SentenceTr{Zh: &z, ZhStatus: model.ZhOK}))
	}
	e.setSummary(l.ID, "The lecture covers the zorblax protocol and the snerdwick engine.")
	for _, stage := range []string{model.StagePrep, model.StageASR, model.StageSummary, model.StageRepair, model.StageTranslate, model.StageExport} {
		e.markDone(model.ScopeLecture, l.ID, stage)
	}

	e.fake.Handle(llmtest.RoleGlossarySeed, extractWords("SYLLABUS:\n"))
	e.fake.Handle(llmtest.RoleMaterialTerms, extractWords("SLIDES:\n"))
	var mu sync.Mutex
	var janitorUsers []string
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		mu.Lock()
		janitorUsers = append(janitorUsers, user)
		mu.Unlock()
		// the janitor cannot add terms either (G6 rule 2)
		return `{"actions": [{"id": "zorblax", "action": "enforce", "reason": "New."}]}`
	})

	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "SYLLABUS.md", Path: "SYLLABUS.md", Text: &md}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	if r := e.run(e.stage(d, model.StageGlossarySeed), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}
	m := e.material(nil, "w1.pdf", pages)
	if r := e.run(e.stage(d, model.StageMaterialTerms), m.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID); r.err != nil {
		t.Fatal(r.err)
	}

	human1 := strings.ToLower(md + "\n" + strings.Join(pages, "\n"))
	terms := e.terms()
	if len(terms) == 0 {
		t.Fatal("no terms extracted")
	}
	for _, x := range terms {
		if !strings.Contains(human1, strings.ToLower(x.En)) {
			t.Errorf("term %q is not in the syllabus or the slides", x.En)
		}
		for _, w := range only {
			if strings.Contains(strings.ToLower(x.En), w) {
				t.Errorf("term %q comes from the lecture's transcript or AI output (G6 rule 1)", x.En)
			}
		}
	}
	for _, req := range append(e.fake.Requests(llmtest.RoleGlossarySeed), e.fake.Requests(llmtest.RoleMaterialTerms)...) {
		for _, w := range only {
			if strings.Contains(req.System+req.User, w) {
				t.Errorf("a term extraction request contains %q", w)
			}
		}
	}

	if len(janitorUsers) != 1 {
		t.Fatalf("%d janitor requests", len(janitorUsers))
	}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	sources := []string{flat(md)}
	for _, p := range pages {
		sources = append(sources, flat(p))
	}
	open, _ := janitorEntries(t, janitorUsers[0])
	nctx := 0
	for _, entry := range open {
		for _, c := range entry["contexts"].([]any) {
			nctx++
			frag := strings.Trim(c.(string), "…")
			ok := false
			for _, s := range sources {
				if strings.Contains(s, frag) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("context %q is not cut from the slides or SYLLABUS.md (G6 rule 7)", c)
			}
		}
	}
	if nctx == 0 {
		t.Error("no contexts at all: the check above proves nothing")
	}
	for _, frag := range append(only, "meets the", "repaired by hand", "The lecture covers") {
		if strings.Contains(janitorUsers[0], frag) {
			t.Errorf("the janitor request contains %q from the lecture", frag)
		}
	}
	if rep, err := glossary.LastRun(e.ctx, e.st, e.course.ID); err != nil || len(rep.Rejected) != 1 || rep.Rejected[0].TermID != "zorblax" || len(rep.Items) != 0 {
		t.Errorf("last run %+v %v: the unknown id must be rejected", rep, err)
	}
}

// ---------- lead ruling after the real acceptance ----------

// janitorDepsAnthropic is janitorDeps with the janitor on an Anthropic-type
// provider of the fake server (whose truncated answers the client reports
// as llm.ErrTruncated after its own retry with a doubled max_tokens).
func (e *env) janitorDepsAnthropic() Deps {
	e.t.Helper()
	d := e.janitorDeps()
	str := func(s string) *string { return &s }
	p, err := d.Settings.CreateProvider(e.ctx, settings.ProviderInput{
		Name: str("Claude"), Type: str(model.ProviderAnthropic), BaseURL: str(e.fake.URL), APIKey: str(llmtest.TestKey),
	})
	e.must(err)
	e.must(d.Settings.SetRole(e.ctx, model.RoleGlossaryJanitor, p.ID, janitorModel, model.RoleParams{}))
	return d
}

// TestGlossaryReviewSplitsTruncated: an answer cut off at the output limit
// is asked again in halves (en_key order, recursively); all actions are
// applied in one transaction (version + 1); actions that change nothing are
// dropped and counted.
func TestGlossaryReviewSplitsTruncated(t *testing.T) {
	t.Run("openai", func(t *testing.T) { testSplitsTruncated(t, false) })
	t.Run("anthropic", func(t *testing.T) { testSplitsTruncated(t, true) })
}

func testSplitsTruncated(t *testing.T, anthropic bool) {
	e := newEnv(t)
	d := e.janitorDeps()
	if anthropic {
		d = e.janitorDepsAnthropic()
	}
	for i := 59; i >= 0; i-- {
		e.term2(fmt.Sprintf("term%03d", i), "", model.SourceMaterial, nil)
	}
	var mu sync.Mutex
	var answered [][]string
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		open, _ := janitorEntries(t, user)
		if len(open) > 25 {
			return llmtest.Truncated(`{"actions": [{"id": "`)
		}
		ens := make([]string, len(open))
		var acts []string
		for i, m := range open {
			en := m["en"].(string)
			ens[i] = en
			var n int
			fmt.Sscanf(en, "term%03d", &n)
			action := "enforce" // already enforce: no change
			if n%2 == 0 {
				action = "reference"
			}
			acts = append(acts, fmt.Sprintf(`{"id": %q, "action": %q, "reason": "r"}`, m["id"], action))
		}
		mu.Lock()
		answered = append(answered, ens)
		mu.Unlock()
		return `{"actions": [` + strings.Join(acts, ",") + `]}`
	})
	e.enqueueReview()
	v0 := e.glossaryVersion()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	sort.Slice(answered, func(i, j int) bool { return answered[i][0] < answered[j][0] })
	if len(answered) != 4 {
		t.Fatalf("answered batches %v, want 4 of 15 (60 → 30 → 15)", answered)
	}
	// T-PERF-07: the first truncation splits at once, without the client's
	// truncation ladder: 60 once, 30 twice, 15 four times, every request
	// with the same max_tokens (none doubled).
	reqs := e.fake.Requests(llmtest.RoleGlossaryJanitor)
	if len(reqs) != 7 {
		t.Errorf("%d requests, want 7 (60 → 30 ×2 → 15 ×4, each asked once)", len(reqs))
	}
	for i, q := range reqs {
		if q.MaxTokens != reqs[0].MaxTokens {
			t.Errorf("request %d: max_tokens %d, the first had %d", i, q.MaxTokens, reqs[0].MaxTokens)
		}
	}
	for k, b := range answered {
		if len(b) != 15 || b[0] != fmt.Sprintf("term%03d", 15*k) || b[14] != fmt.Sprintf("term%03d", 15*k+14) {
			t.Errorf("batch %d: %v", k, b)
		}
	}
	if v := e.glossaryVersion(); v != v0+1 {
		t.Errorf("version %d, want exactly %d", v, v0+1)
	}
	m := termMap(e.terms())
	for i := 0; i < 60; i++ {
		k := fmt.Sprintf("term%03d", i)
		want := model.TierEnforce
		if i%2 == 0 {
			want = model.TierReference
		}
		if m[k].Tier != want {
			t.Errorf("%s tier %s, want %s", k, m[k].Tier, want)
		}
	}
	rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Items) != 30 || len(rep.Rejected) != 0 {
		t.Errorf("report: %d items, %d rejected; want 30 and 0 (no-op actions are neither)", len(rep.Items), len(rep.Rejected))
	}
	if n := rep.NoteInfo; infoCode(n) != NoteJanitorIgnored || !paramsMatch(n, "n", 30) ||
		fmt.Sprint(extraCodes(n)) != "["+NoteJanitorSplit+"]" || !paramsMatch(&extras(n)[0], "batches", 4) {
		t.Errorf("report note %+v", n)
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteJanitorDone || !paramsMatch(n, "n", 30, "reference", 30, "rejected", 0) ||
		fmt.Sprint(extraCodes(n)) != "["+NoteJanitorNoops+" "+NoteJanitorSplit+"]" {
		t.Errorf("note %+v", n)
	}
}

// TestGlossaryReviewTruncatedSmall: a batch still cut off below the split
// size fails the run on the appendix D2 path (fallback, permanent error).
func TestGlossaryReviewTruncatedSmall(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDepsAnthropic()
	for i := 0; i < 30; i++ {
		e.term2(fmt.Sprintf("term%03d", i), "", model.SourceMaterial, nil)
	}
	public := e.term2("public", "公钥", model.SourceMaterial, nil)
	// The two halves run in parallel and the half that fails first cancels
	// the other. To make the count deterministic, a half's third (last)
	// request is answered only after all six requests of the halves have
	// reached the server: then both halves are asked three times (v3.2.x
	// P1-c: two truncation retries; a batch below janitorMinSplit cannot be
	// split and keeps the client's ladder, T-PERF-07) whatever the
	// scheduling (the timeout only keeps a broken stage from hanging the test).
	var (
		mu      sync.Mutex
		perHalf = map[int]int{} // entries → requests seen
		halves  int
		allSent = make(chan struct{})
	)
	entriesOf := func(user string) int { return strings.Count("\n"+user, "\n{\"id\":") }
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		if n := entriesOf(user); n < 31 {
			mu.Lock()
			perHalf[n]++
			last := perHalf[n] == 3
			if halves++; halves == 6 {
				close(allSent)
			}
			mu.Unlock()
			if last {
				select {
				case <-allSent:
				case <-time.After(10 * time.Second):
				}
			}
		}
		return llmtest.Truncated(`{"actions": [`)
	})
	e.enqueueReview()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || !errors.Is(r.err, llm.ErrTruncated) {
		t.Fatalf("err %v", r.err)
	}
	// 31 → 15 + 16 at its first truncation (T-PERF-07: no ladder for a
	// batch that can be split), both still cut off (each asked three times
	// by the client): no further split — exactly 7 requests: 31 ×1, 15 ×3,
	// 16 ×3.
	reqs := e.fake.Requests(llmtest.RoleGlossaryJanitor)
	if n := len(reqs); n != 7 {
		t.Fatalf("%d requests, want 7", n)
	}
	var sizes []int
	for i, q := range reqs {
		if n := entriesOf(q.User); i < 1 && n != 31 {
			t.Errorf("request %d has %d entries, want 31", i, n)
		} else if i >= 1 {
			sizes = append(sizes, n)
		}
	}
	sort.Ints(sizes)
	if fmt.Sprint(sizes) != "[15 15 15 16 16 16]" {
		t.Errorf("requests of the halves have %v entries, want [15 15 15 16 16 16]", sizes)
	}
	if rep, err := glossary.LastRun(e.ctx, e.st, e.course.ID); err != nil || !rep.Fallback {
		t.Errorf("fallback run %+v %v", rep, err)
	}
	if g := e.getTerm(public.ID); g.Tier != model.TierReference {
		t.Errorf("public %+v: the fallback rule must have run", g)
	}
}

// TestGlossaryReviewNoOpsAndFixZh: actions that change nothing are dropped
// (not items, not rejected, counted in the note); a fix_zh equal to the
// English term means "keep in English" ("").
func TestGlossaryReviewNoOpsAndFixZh(t *testing.T) {
	e := newEnv(t)
	d := e.janitorDeps()
	M := model.SourceMaterial
	aes := e.term2("AES", "高级加密标准", M, nil)
	iv := e.term2("IV", "", M, nil)
	rsa := e.term2("RSA", "", M, nil)
	public := e.term2("public", "公钥", M, func(t *model.Term) { t.Tier = model.TierReference })
	world := e.term2("world", "世界", M, func(t *model.Term) { t.Enabled = false })
	key := e.term2("key", "密钥", M, nil)
	e.fake.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		return fmt.Sprintf(`{"actions": [
			{"id": %q, "action": "fix_zh", "zh": "AES", "reason": "Kept in English."},
			{"id": %q, "action": "fix_zh", "zh": "iv", "reason": "Kept in English."},
			{"id": %q, "action": "fix_zh", "zh": "  rsa ", "reason": "Kept in English."},
			{"id": %q, "action": "reference", "reason": "Common word."},
			{"id": %q, "action": "disable", "reason": "Generic."},
			{"id": %q, "action": "fix_zh", "zh": "密钥", "reason": "Standard."},
			{"id": %q, "action": "enforce", "reason": "Technical here."}
		]}`, aes.ID, iv.ID, rsa.ID, public.ID, world.ID, key.ID, iv.ID)
	})
	e.enqueueReview()
	v0 := e.glossaryVersion()
	r := e.run(e.stage(d, model.StageGlossaryReview), e.course.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if g := e.getTerm(aes.ID); g.Zh != "" || g.ZhSource != model.ZhSourceJanitor || g.JanitorNote != "Kept in English." {
		t.Errorf("AES %+v: fix_zh \"AES\" means keep in English", g)
	}
	for _, id := range []string{iv.ID, rsa.ID, public.ID, world.ID, key.ID} {
		before := map[string]model.Term{iv.ID: iv, rsa.ID: rsa, public.ID: public, world.ID: world, key.ID: key}[id]
		if g := e.getTerm(id); g.Zh != before.Zh || g.ZhSource != before.ZhSource || g.Tier != before.Tier || g.Enabled != before.Enabled || g.JanitorNote != "" {
			t.Errorf("%s changed by a no-op action: %+v", g.En, g)
		}
	}
	rep, err := glossary.LatestReport(e.ctx, e.st, e.course.ID)
	if err != nil || len(rep.Items) != 1 || rep.Items[0].TermID != aes.ID || rep.Items[0].After.Zh != "" {
		t.Errorf("report %+v %v", rep, err)
	}
	// the second action for IV is still a duplicate (the first one counted, even as a no-op)
	if len(rep.Rejected) != 1 || rep.Rejected[0].TermID != iv.ID {
		t.Errorf("rejected %+v", rep.Rejected)
	}
	if e.glossaryVersion() != v0+1 {
		t.Errorf("version %d", e.glossaryVersion())
	}
	if n := r.stats.NoteInfo; infoCode(n) != NoteJanitorDone || !paramsMatch(n, "n", 1, "fix_zh", 1, "rejected", 1) ||
		fmt.Sprint(extraCodes(n)) != "["+NoteJanitorNoops+"]" || !paramsMatch(&extras(n)[0], "n", 5) {
		t.Errorf("note %+v", n)
	}
}
