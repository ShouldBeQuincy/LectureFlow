// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package stages implements every automatic task of SPEC §4.1/§4.3/§6 as a
// sched.Stage: prep, asr, summary, repair, translate, export (lecture),
// glossary_seed and glossary_review (course), material_extract and
// material_terms (material).
//
// Rules shared by all stages (internal/sched/scheduler.go file header):
//   - no LLM call and no waiting on database work inside Store.Tx: results are
//     collected in memory, the transaction only writes them;
//   - the progress callback is non-blocking and may be called from any goroutine;
//   - job.stats of a running job is written only through sched.UpdateStats
//     (token usage is reported by the llm client via sched.ReportUsage);
//   - files are written to *.tmp first and renamed (SPEC §4.7).
package stages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// Deps are the collaborators of every stage.
type Deps struct {
	Store   db.Store
	Cfg     *config.Config
	LLM     llm.Client
	DataDir string
	Events  sched.Events // nil = sched.NopEvents
	// Invalidate runs the course-wide invalidation check (SPEC §4.4) outside
	// any transaction, telling it why (v1.1 A2.2: the stale_reason of the
	// jobs it marks); wired to Scheduler.InvalidateCourseWith. nil = no-op.
	Invalidate func(ctx context.Context, courseID string, cause state.StaleCause) error
	// GPUTotalMB returns the total VRAM for compute_type=auto (SPEC §17.2);
	// wired to Scheduler.GPUStatus. nil or <= 0 = unknown.
	GPUTotalMB func(ctx context.Context) int
	// Concurrency is the number of requests one API stage keeps in flight
	// (translate sentences, repair and material_terms chunks).
	// 0 = the max_concurrency of the stage's provider (the job snapshot's
	// provider for lecture stages, the role's current provider for course
	// stages). The API pool still enforces the provider limits across all stages.
	Concurrency int
	// Settings is the source of the current settings (v1.1 A5):
	// keep_original_media and the providers' max_concurrency. Lecture stages
	// never read the current model settings: they use their job's
	// config_snapshot (A4.1). nil = the legacy fields of Cfg (tests of the
	// old path).
	Settings *settings.Service
	// Now is the clock of generated_at in the citation JSON (A11). nil = time.Now.
	Now func() time.Time
}

// All returns the nine stages of SPEC §4.1 and §4.3 and the course task
// glossary_review of SPEC-v2 G3.1, sharing d.
func All(d Deps) []sched.Stage {
	if d.Events == nil {
		d.Events = sched.NopEvents{}
	}
	return []sched.Stage{
		&prepStage{d: d},
		newASRStage(d),
		&summaryStage{apiStage{d: d, role: config.RoleSummary}},
		&repairStage{apiStage{d: d, role: config.RoleRepair}},
		&translateStage{apiStage{d: d, role: config.RoleTranslate}},
		&exportStage{d: d},
		&glossaryStage{apiStage{d: d, role: config.RoleGlossary}},
		&materialExtractStage{d: d},
		&materialTermsStage{apiStage{d: d, role: config.RoleGlossary}},        // SPEC §2: material_terms uses the glossary role
		&glossaryReviewStage{apiStage{d: d, role: model.RoleGlossaryJanitor}}, // SPEC-v2 G3: the glossary janitor
		// SPEC-v2 auxiliary tasks (not pipeline stages, phase 2 lead ruling 1)
		&titleStage{apiStage{d: d, role: model.RoleOrganizer}},        // P13.5
		&chaptersStage{apiStage{d: d, role: model.RoleChapters}},      // P7
		&filenameRuleStage{apiStage{d: d, role: model.RoleOrganizer}}, // P13.3.2
		&streamStage{d: d}, // P8.1: stream.mp4 for lectures prepared before v2 (CPU)
	}
}

// ByName returns All(d) keyed by stage name (for sched.Options.Stages).
func ByName(d Deps) map[string]sched.Stage {
	m := map[string]sched.Stage{}
	for _, s := range All(d) {
		m[s.Name()] = s
	}
	return m
}

func (d Deps) events() sched.Events {
	if d.Events == nil {
		return sched.NopEvents{}
	}
	return d.Events
}

func (d Deps) invalidate(ctx context.Context, courseID string, cause state.StaleCause) {
	if d.Invalidate == nil {
		return
	}
	if err := d.Invalidate(context.WithoutCancel(ctx), courseID, cause); err != nil {
		slog.Warn("course invalidation", "course", courseID, "cause", cause.Kind, "err", err)
	}
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// keepOriginalMedia is SPEC §17.5 keep_original_media (v1.1: a general setting).
func (d Deps) keepOriginalMedia() bool {
	if d.Settings != nil {
		return d.Settings.General().KeepOriginalMedia
	}
	return d.Cfg.Storage.KeepMedia()
}

// provider returns the id of the provider currently set for role (course
// stages, and the scheduler's api_slot hint). Legacy: the config key.
func (d Deps) provider(role string) string {
	if d.Settings != nil {
		if t, err := d.Settings.RoleTarget(role); err == nil {
			return t.ProviderID
		}
		return ""
	}
	p, _ := llm.RoleProvider(d.Cfg, role)
	return p
}

// concurrency is the number of parallel requests of one stage run: the
// max_concurrency of providerID ("" = the role's current provider).
func (d Deps) concurrency(role, providerID string) int {
	if d.Concurrency > 0 {
		return d.Concurrency
	}
	if providerID == "" {
		providerID = d.provider(role)
	}
	if d.Settings != nil {
		if p, ok := d.Settings.Provider(providerID); ok && p.MaxConcurrency > 0 {
			return p.MaxConcurrency
		}
		return 1
	}
	if p, ok := d.Cfg.LLM.Providers[providerID]; ok && p.MaxConcurrency > 0 {
		return p.MaxConcurrency
	}
	if p, ok := d.Cfg.LLM.Providers[d.provider(role)]; ok && p.MaxConcurrency > 0 {
		return p.MaxConcurrency
	}
	return 1
}

// deepSeek reports whether providerID is DeepSeek's API (settings.IsDeepSeek:
// its base_url host or its balance API).
func (d Deps) deepSeek(providerID string) bool {
	if d.Settings != nil {
		p, ok := d.Settings.Provider(providerID)
		return ok && settings.IsDeepSeek(p.BaseURL, p.BalanceAPI)
	}
	if d.Cfg == nil {
		return false
	}
	p, ok := d.Cfg.LLM.Providers[providerID]
	return ok && settings.IsDeepSeek(p.BaseURL, "")
}

// anthropic reports whether providerID is an Anthropic provider: of type
// anthropic and not DeepSeek's Anthropic-compatible endpoint (deepSeek), the
// rule settings uses for the glossary_janitor default. config.toml
// providers are OpenAI-compatible (llm resolveLegacy).
func (d Deps) anthropic(providerID string) bool {
	if d.Settings == nil {
		return false
	}
	p, ok := d.Settings.Provider(providerID)
	return ok && p.Type == model.ProviderAnthropic && !d.deepSeek(providerID)
}

// repairWarmupFor is the warm-up of a repair run on target:
// repairAnthropicWarmup when its provider is Anthropic and its requests are
// streamed (llm.Streams: a current Claude model), the only case in which
// chunk 0 reports the start of its answer; repairWarmup otherwise —
// DeepSeek whatever its endpoint, other providers, and models an Anthropic
// provider does not stream (older Claude models, other models behind an
// Anthropic-compatible gateway), which keep their timing.
func (d Deps) repairWarmupFor(target model.LLMSnapshot) warmup {
	if d.anthropic(target.ProviderID) && llm.Streams(model.ProviderAnthropic, target.Model) {
		return repairAnthropicWarmup
	}
	return repairWarmup
}

// ---------- model configuration snapshots (v1.1 A4) ----------

// MissingSnapshot is the error of a lecture stage whose job has no usable
// config_snapshot (lead ruling 4, NOTES.md). The stage fails permanently:
// running it with the current settings would silently change the model.
const MissingSnapshot = "该阶段缺少模型配置快照，请重跑并选择使用当前设置"

// currentJob returns the job a stage runs for: the one the scheduler put in
// ctx, else the current job of (scope, scopeID, stage).
func (d Deps) currentJob(ctx context.Context, scope, scopeID, stage string) (model.Job, error) {
	if j, ok := sched.JobFrom(ctx); ok && j.Stage == stage && j.ScopeID == scopeID {
		return j, nil
	}
	return d.Store.GetCurrentJob(ctx, scope, scopeID, stage)
}

// llmTarget reads the LLM snapshot of the running lecture job and attaches
// it to ctx with llm.WithTarget, so every request of the run goes to the
// snapshot's provider and model (A4.1: "阶段运行时只读取自己的快照"). The
// job is returned for its other snapshot parameters (translate's window).
func (a apiStage) llmTarget(ctx context.Context, lectureID, stage string) (context.Context, model.Job, model.LLMSnapshot, error) {
	j, err := a.d.currentJob(ctx, model.ScopeLecture, lectureID, stage)
	if err != nil {
		return ctx, j, model.LLMSnapshot{}, err
	}
	snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot)
	if !ok {
		return ctx, j, snap, sched.Permanent(model.Errorf(CodeSnapshotMissing, nil, errors.New(MissingSnapshot)))
	}
	return llm.WithTarget(ctx, snap), j, snap, nil
}

// recordGlossaryVersion writes stats.glossary_version: the course glossary
// the run used (v1.1 A2.2 "术语表已更新（v3 → v4）").
func recordGlossaryVersion(ctx context.Context, v int) {
	sched.UpdateStats(ctx, func(st *model.JobStats) { st.GlossaryVersion = &v })
}

func (d Deps) lectureDir(l model.Lecture) string {
	return model.LectureDir(d.DataDir, l.CourseID, l.ID)
}

// mediaDisplayName is the name a message gives a lecture's media file: the
// name the user imported it under (original_filename), else the stored
// file's own name ("media.mp4"; lectures created before v2 have no
// original name). test-media I6.
func mediaDisplayName(l model.Lecture) string {
	if n := strings.TrimSpace(l.OriginalFilename); n != "" {
		return n
	}
	return filepath.Base(l.MediaPath)
}

// resolvePath resolves a stored path: absolute, relative to the working
// directory when it exists there, otherwise relative to base (the lecture,
// material or syllabus directory). Same rule as state.MediaExists.
func resolvePath(p, base string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	if _, err := os.Stat(p); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return filepath.Join(base, p)
}

// within reports whether path lies inside dir.
func within(path, dir string) bool {
	ap, err1 := filepath.Abs(path)
	ad, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(ad, ap)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// lectureData is a lecture with its sentences and prompt snapshot.
type lectureData struct {
	lec       model.Lecture
	sentences []model.Sentence
	snap      ctxbuild.LectureSnapshot
	hasSum    bool
	// glossaryVersion is course.glossary_version read together with the
	// terms of snap (v1.1: recorded in stats.glossary_version).
	glossaryVersion int
}

// loadLecture loads everything the lecture-level prompts and hashes need,
// in one read transaction so that the terms and the glossary version agree.
// The snapshot comes from state.LoadSnapshot, the same loader the
// sentence-level invalidation of review operations uses, so the zh_hash
// written by translate and the hash recomputed on review always agree.
func (d Deps) loadLecture(ctx context.Context, lectureID string) (lectureData, error) {
	var ld lectureData
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		ld = lectureData{}
		l, err := tx.GetLecture(ctx, lectureID)
		if err != nil {
			return fmt.Errorf("讲座 %s：%w", lectureID, err)
		}
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		snap, err := state.LoadSnapshot(ctx, tx, lectureID)
		if err != nil {
			return err
		}
		ss, err := tx.ListSentences(ctx, lectureID, 0, 0)
		if err != nil {
			return err
		}
		if len(ss) != len(snap.Sentences) {
			return model.Errorf(CodeSentencesChanged, nil, fmt.Errorf("讲座 %s 的句子在读取期间发生了变化，请重试", lectureID))
		}
		switch _, err := tx.GetSummary(ctx, lectureID); {
		case err == nil:
			ld.hasSum = true
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		ld.lec, ld.sentences, ld.snap, ld.glossaryVersion = l, ss, snap, c.GlossaryVersion
		return nil
	})
	return ld, err
}

// ---------- LLM JSON requests with business retries (SPEC §12) ----------

// businessRetries is the number of extra attempts after an unparsable or
// invalid answer (repair: 2 per chunk, translate: 2 per sentence, SPEC §12).
const businessRetries = 2

// errUnusable marks an answer that stayed unparsable/invalid after the
// business retries. Transport errors (llm client) are returned unwrapped.
type errUnusable struct{ last error }

func (e *errUnusable) Error() string {
	return fmt.Sprintf("模型输出连续 %d 次无法使用：%v", businessRetries+1, withoutAnswerText(e.last))
}
func (e *errUnusable) Unwrap() error { return e.last }

// As lets errors.As find the coded form (v2 P14.3): CodeOutputUnusable with
// the text above as the detail.
func (e *errUnusable) As(target any) bool {
	if p, ok := target.(**model.CodedError); ok {
		*p = &model.CodedError{Info: model.ErrorInfo{Code: CodeOutputUnusable, Params: map[string]any{"attempts": businessRetries + 1}},
			Err: errors.New(e.Error())}
		return true
	}
	return false
}

func isUnusable(err error) bool {
	var u *errUnusable
	return errors.As(err, &u)
}

// unusableKind names what was wrong with the last answer of an unusable
// request, for the service log: the error text itself quotes the model's
// answer (lecture content), which the log must not hold.
func unusableKind(err error) string {
	switch {
	case errors.Is(err, llm.ErrSecondJSON):
		return "second_json"
	case errors.Is(err, llm.ErrBadJSON):
		return "not_json"
	}
	return "check"
}

// refusalCategory is the provider's refusal category of an llm.refusal
// error ("" when none came).
func refusalCategory(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		c, _ := ce.Info.Params["category"].(string)
		return c
	}
	return ""
}

// askJSON sends one JSON-mode request and decodes the answer into a fresh T,
// asking again up to businessRetries times when the answer cannot be parsed
// or check rejects it. From the second request on the model is told why its
// answer was rejected (askJSONTries; v3.2.x P1 amends SPEC §12, which sent
// the same request again). Transport and context errors — and a request the
// provider declined, llm.ErrRefusal — are returned at once.
func askJSON[T any](ctx context.Context, c llm.Client, role, system, user string, check func(*T) error) (T, error) {
	v, last, err := askJSONTries(ctx, c, role, system, user, businessRetries+1, check)
	if err != nil {
		return v, err
	}
	if last != nil {
		var zero T
		return zero, &errUnusable{last: last}
	}
	return v, nil
}

// retryProblems is a check error that lists its problems for the retry.
type retryProblems interface{ Problems() []string }

// unparsedAnswer is askJSONTries' reason for an answer that did not parse
// (llm.ParseAnswer's error, kept as is: its text is what the retry feedback
// and a pending sentence's qa_error say). That text may quote the start of
// the answer — lecture content — so where it becomes a job error's detail
// or a log line (the scheduler's "job failed"), withoutAnswerText replaces
// it (review R51, as r4 D1 did for checked answers).
type unparsedAnswer struct {
	err   error
	chars int // length of the answer, in runes
}

func (e *unparsedAnswer) Error() string { return e.err.Error() }
func (e *unparsedAnswer) Unwrap() error { return e.err }

// withoutAnswerText is err for a job error's detail and the service log: an
// unparsedAnswer becomes a fixed text with the answer's length (the one of
// two different JSON values, llm.ErrSecondJSON, quotes nothing and stays);
// any other error is returned as it is. The rejected answer itself is in
// the call log's rejected-*.jsonl, as before.
func withoutAnswerText(err error) error {
	ua, ok := err.(*unparsedAnswer)
	if !ok || errors.Is(ua.err, llm.ErrSecondJSON) {
		return err
	}
	return &answerNotQuoted{
		text: fmt.Sprintf("%s (an answer of %d characters, not quoted here)", llm.ErrBadJSON.Error(), ua.chars),
		err:  err,
	}
}

// answerNotQuoted is an unparsedAnswer under its quote-free text.
type answerNotQuoted struct {
	text string
	err  error
}

func (e *answerNotQuoted) Error() string { return e.text }
func (e *answerNotQuoted) Unwrap() error { return e.err }

// askJSONTries sends a JSON-mode request up to tries times until an answer
// parses (llm.ParseAnswer) and passes check. A retry is the same system
// prompt (the cached prefix is untouched) and the same user message,
// followed by ctxbuild.WithRetryFeedback with what was wrong with the last
// answer: the error's Problems() when it has them (a retryProblems), else
// its text; ctxbuild.SecondJSONProblem for an answer of two different JSON
// values; ctxbuild.NotJSONProblem for any other unparsable answer. Every
// rejected answer is reported (llm.ReportRejected).
//
// last is nil when v is usable, else the reason the last answer was
// rejected (every try was used). err is a transport or context error,
// returned at once.
func askJSONTries[T any](ctx context.Context, c llm.Client, role, system, user string, tries int, check func(*T) error) (v T, last, err error) {
	u := user
	reject := func(problems []string) { u = ctxbuild.WithRetryFeedback(user, problems) }
	ctx = llm.WithCallTrace(ctx) // the call log numbers the tries and names the refused answers
	for range tries {
		var a T
		res, err := c.Complete(ctx, role, system, u, true)
		if err != nil {
			return a, nil, err
		}
		if err := llm.ParseAnswer(ctx, role, res.Text, &a); err != nil {
			llm.ReportRejected(ctx, role, res.Text, err)
			last = &unparsedAnswer{err: err, chars: utf8.RuneCountInString(res.Text)}
			if errors.Is(err, llm.ErrSecondJSON) {
				reject([]string{ctxbuild.SecondJSONProblem})
			} else {
				reject([]string{ctxbuild.NotJSONProblem(err.Error())})
			}
			continue
		}
		if check != nil {
			if err := check(&a); err != nil {
				llm.ReportRejected(ctx, role, res.Text, err)
				last = err
				var rp retryProblems
				if errors.As(err, &rp) {
					reject(rp.Problems())
				} else {
					reject([]string{err.Error()})
				}
				continue
			}
		}
		return a, nil, nil
	}
	var zero T
	return zero, last, nil
}

// parallel runs fn(i) for i in [0, n) with at most workers goroutines. When
// fn returns a fatal error (stop == true) no new item is started; the first
// fatal error is returned after the running ones finish.
func parallel(ctx context.Context, n, workers int, fn func(ctx context.Context, i int) (stop bool, err error)) error {
	if n == 0 {
		return nil
	}
	workers = max(1, min(workers, n))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		next  int
		first error
		wg    sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if first != nil || next >= n || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				stop, err := fn(ctx, i)
				if stop {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return first
}

// progressCounter reports done/total through the progress callback.
type progressCounter struct {
	mu          sync.Mutex
	done, total int
	report      func(float64)
}

func (p *progressCounter) inc() { p.add(1) }

// add counts n more items done (a batch of translated sentences).
func (p *progressCounter) add(n int) {
	p.mu.Lock()
	p.done += n
	v := float64(p.done) / float64(max(p.total, 1))
	p.mu.Unlock()
	p.report(v)
}

// writeFileAtomic writes data to path via path+".tmp" and a rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func jobID(ctx context.Context) string {
	if j, ok := sched.JobFrom(ctx); ok {
		return j.ID
	}
	return ""
}

// apiStage holds what every API stage shares.
type apiStage struct {
	d    Deps
	role string
}

func (a apiStage) Resource() sched.ResourceKind { return sched.ResourceAPI }

// Provider is the provider currently set for the stage's role. For lecture
// jobs the scheduler uses the provider of the job's snapshot (v1.1 sched).
func (a apiStage) Provider() string { return a.d.provider(a.role) }
