// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"lectureflow/internal/calendar"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/state"
)

// filenameRuleStage is the auxiliary task filename_rule (SPEC-v2 P13.3.2,
// P13.3.3): the organizer model writes one regular expression for the
// course's lecture file names, which is then checked deterministically
// (calendar.ValidateRule) and, from then on, applied by the program without
// the model. Scope course, role organizer (current setting).
//
// Lecture kinds (lead freeze .claude/kinds-brief.md 1.1–1.3): the prompt
// (version ctxbuild.FilenameRulePromptVersion) also asks for an optional
// (?P<kind>…) group and kind_map (captured code → kind id), checked against
// the kind catalog by cleanKindMap (bad entries are dropped, the rule is
// not rejected for them); a sample dated more than 30 days before the term
// start is a past lecture, not a failure (rule 4 as amended).
//
// Outcome of a valid rule:
//   - 首次例外: no lecture of the course has a recording time yet, every
//     sample parses cleanly (Status ok: no unmatched, invalid or suspicious
//     file), the course has no user-written rule and no citation export
//     whose numbers would change → applied at once (ApplyFilenameRule's
//     work, in the same transaction), the state before saved for “撤销”
//     (kv filename_rule_undo);
//   - otherwise → kv filename_rule_proposal (model.FilenameProposal) for the
//     user to apply, edit or ignore.
//
// A rejected answer is asked again once, with the reasons appended to the
// request (askJSONRetry, ctxbuild.FilenameRuleProblems); after a second
// rejection the built-in Panopto pattern is checked the same way
// (fallbackRule, filename_fallback.go) and, if it passes, takes the
// model's place; otherwise the task fails and the course keeps its rule
// (or none). Without a model for the organizer role (3.1.0 "只转写") the
// model is not asked: only the built-in pattern is tried, with the same
// check and outcome, and when it does not fit the task fails with
// filename_rule.not_configured. Every file name sent to the model
// is added to kv filename_rule_samples, so the same names never trigger a
// generation again (P13.3.2 trigger).
type filenameRuleStage struct{ apiStage }

func (s *filenameRuleStage) Name() string { return model.StageFilenameRule }

func (s *filenameRuleStage) InputHash(context.Context, string) (string, error) { return "", nil }

// filenameOldRuleSamples: when the course already has a rule, at least this
// many of the file names it matches (if it matches that many) are among the
// samples, so the new rule stays compatible with the old files (P13.3.3).
const filenameOldRuleSamples = 10

type filenameRuleAnswer struct {
	Regex string `json:"regex"`
	// KindMap is kept raw: a malformed kind_map is dropped (cleanKindMap),
	// it does not make the whole answer unparsable.
	KindMap     json.RawMessage `json:"kind_map"`
	Explanation string          `json:"explanation"`
}

// ruleRejected is a regex that calendar.ValidateRule rejected.
type ruleRejected struct {
	regex    string
	matched  int
	total    int
	problems []string
	feedback []string // for the retry (ctxbuild.FilenameRuleProblems)
}

func (e *ruleRejected) Error() string {
	return fmt.Sprintf("regex %s rejected (%d/%d file names matched): %s", e.regex, e.matched, e.total, strings.Join(e.problems, "; "))
}

// Problems is the feedback of the retry (askJSONRetry).
func (e *ruleRejected) Problems() []string { return e.feedback }

// filenameRejection describes in English why calendar.ValidateRule rejected
// regex over samples (parsed is its preview table), for the retry feedback:
// the compile error or missing groups, the names that do not match, the
// matched names without a usable date with their captured date and time
// groups, and kindNotes (cleanKindMap: what was dropped from kind_map). The
// reason of such a name is told apart with the exported calendar functions
// only: a date it parses once the upload time is moved far ahead (with a
// year group: the date does not depend on it) was after the upload (rule
// 3); otherwise the groups do not form a date. (A date long before the
// term start is no longer a failure: lecture kinds number it as a past
// lecture, so the feedback never names the term start.)
func filenameRejection(regex string, samples []calendar.Sample, parsed []calendar.Parsed, matched int, kindNotes []string) ctxbuild.FilenameRuleRejection {
	rj := ctxbuild.FilenameRuleRejection{Regex: regex, Matched: matched, Total: len(samples), MinMatchPercent: calendar.MinMatchPercent,
		KindMap: kindNotes}
	re, err := regexp.Compile(regex)
	if err != nil {
		rj.CompileError = err.Error()
		return rj
	}
	names := re.SubexpNames()
	has := func(g string) bool { return slices.Contains(names, g) }
	for _, g := range []string{calendar.GroupMonth, calendar.GroupDay} {
		if !has(g) {
			rj.MissingGroups = append(rj.MissingGroups, g)
		}
	}
	if len(rj.MissingGroups) > 0 {
		return rj
	}
	rule := calendar.FilenameRule{Regex: regex}
	for i, s := range samples {
		if i >= len(parsed) {
			break
		}
		p := parsed[i]
		switch {
		case !p.Matched:
			rj.Unmatched = append(rj.Unmatched, s.Filename)
			continue
		case p.RecordedAt != nil:
			continue
		}
		d := ctxbuild.FilenameRuleInvalidDate{Filename: s.Filename, Reason: "the captured groups do not form a valid date and time"}
		if m := re.FindStringSubmatch(s.Filename); m != nil {
			for _, g := range []string{calendar.GroupYear, calendar.GroupMonth, calendar.GroupDay, calendar.GroupHour, calendar.GroupMinute, calendar.GroupAMPM} {
				for k, n := range names {
					if n == g && m[k] != "" {
						d.Groups = append(d.Groups, g+"="+m[k])
						break
					}
				}
			}
		}
		if has(calendar.GroupYear) {
			late := s
			late.UploadedAt = s.UploadedAt.AddDate(100, 0, 0)
			if q := calendar.Parse(rule, late); q.RecordedAt != nil {
				d.Reason = fmt.Sprintf("%s is after the upload date %s", calendar.FormatDate(*q.RecordedAt), calendar.FormatDate(s.UploadedAt))
			}
		}
		rj.Invalid = append(rj.Invalid, d)
	}
	return rj
}

// filenameRuleData is the input of one run.
type filenameRuleData struct {
	in        ctxbuild.FilenameRuleInput
	samples   []calendar.Sample
	termStart *time.Time
	kinds     kindCatalog
}

func (s *filenameRuleStage) Run(ctx context.Context, courseID string, progress func(float64)) error {
	d := s.d
	data, err := d.filenameRuleInput(ctx, courseID)
	if err != nil {
		return err
	}
	if len(data.samples) == 0 {
		return model.Errorf("filename_rule.no_samples", nil, errors.New("no lecture of the course has an original file name"))
	}
	noModel := !d.roleConfigured(s.role)
	if !noModel {
		d.recordPromptVersion(ctx, s.role, ctxbuild.FilenameRulePromptVersion)
	}
	req := ctxbuild.FilenameRuleRequest(data.in)
	progress(0.1)

	var (
		parsed    []calendar.Parsed
		matched   int
		kindMap   map[string]string
		kindNotes []string
	)
	check := func(a *filenameRuleAnswer) error {
		a.Regex = strings.TrimSpace(a.Regex)
		a.Explanation = oneLineTrim(a.Explanation)
		if a.Regex == "" {
			return errors.New(`the answer has no "regex"`)
		}
		var km map[string]string
		var notes []string
		if compiles, group := hasKindGroup(a.Regex); compiles {
			km, notes = cleanKindMap(a.KindMap, group, data.kinds)
		}
		p, m, ok, problems := calendar.ValidateRule(calendar.FilenameRule{Regex: a.Regex, KindMap: km}, data.samples, data.termStart)
		// codes no sample has (lead after the v3 check): the rows' kinds do
		// not depend on them, so p stays valid
		var dropped []string
		km, dropped = dropUncapturedCodes(km, p)
		notes = append(notes, dropped...)
		if codes := unmappedKindCodes(p); len(codes) > 0 {
			notes = append(notes, ctxbuild.FilenameKindMapUnmapped(codes))
		}
		if !ok {
			fb := ctxbuild.FilenameRuleProblems(filenameRejection(a.Regex, data.samples, p, m, notes))
			texts := make([]string, len(problems))
			for i, pr := range problems {
				texts[i] = pr.Text
			}
			return &ruleRejected{regex: a.Regex, matched: m, total: len(data.samples), problems: texts, feedback: fb}
		}
		parsed, matched, kindMap, kindNotes = p, m, km, notes
		return nil
	}
	var (
		a        filenameRuleAnswer
		unusable bool
	)
	if noModel {
		err = errNoFilenameModel
	} else {
		a, unusable, err = askJSONRetry(ctx, d.LLM, s.role, req.System, req.User, check)
		// A refusal (llm.ErrRefusal: a refusal stop reason or a content
		// filter) is a model that gave no usable rule, like an unusable
		// answer: the built-in pattern is tried and the names are recorded,
		// so they do not trigger (and get refused) again (review R25 B-1).
		if err != nil && !unusable && !errors.Is(err, llm.ErrRefusal) {
			return err
		}
	}
	// The model was asked about these names (without a model: the built-in
	// pattern was tried on them): they do not trigger again, whatever the
	// outcome (P13.3.2 “此前没有被用于生成过规则”).
	names := make([]string, len(data.samples))
	for i, sm := range data.samples {
		names[i] = sm.Filename
	}
	if err != nil {
		// both answers rejected or unusable: the built-in Panopto pattern
		// (filename_fallback.go), checked the same way
		if fa, p, m, ok := fallbackRule(data.samples, data.termStart); ok {
			if noModel {
				fa.Explanation = panoptoNoModelExplanation
				slog.Info("filename rule: no model configured, built-in Panopto pattern used", "course", courseID)
			} else {
				// what was wrong, not the error: it quotes the regex and file names (SEC1 L4)
				problem := unusableKind(err)
				if errors.Is(err, llm.ErrRefusal) {
					problem = "refused"
				}
				slog.Info("filename rule: model's rule rejected, built-in Panopto pattern used", "course", courseID, "problem", problem)
			}
			a, parsed, matched, kindMap, kindNotes, err = fa, p, m, nil, nil, nil
		}
	}
	if err != nil {
		if mErr := d.Store.Tx(context.WithoutCancel(ctx), func(tx db.Store) error {
			return addFilenameSamples(ctx, tx, courseID, names)
		}); mErr != nil {
			return mErr
		}
		d.events().Changed("course", courseID)
		if errors.Is(err, llm.ErrRefusal) {
			return err // coded llm.refusal, permanent: says to use another model
		}
		var rj *ruleRejected
		if errors.As(err, &rj) {
			return model.Errorf("filename_rule.rejected", map[string]any{"matched": rj.matched, "total": rj.total}, err)
		}
		if errors.Is(err, errNoFilenameModel) {
			// the names do not fit the built-in pattern and there is no
			// model to ask: say how to get the recording times
			return model.Errorf("filename_rule.not_configured", nil, err)
		}
		return model.Errorf("filename_rule.invalid_output", map[string]any{"attempts": auxAttempts}, withoutAnswerText(err))
	}
	progress(0.9)
	if len(kindNotes) > 0 {
		slog.Info("filename rule: kind_map entries dropped", "course", courseID, "notes", kindNotes)
	}

	rule := calendar.FilenameRule{
		Regex:          a.Regex,
		Source:         calendar.RuleSourceLLM,
		CreatedAt:      d.now().UTC().Format(time.RFC3339),
		SamplesMatched: matched,
		SamplesTotal:   len(data.samples),
		KindMap:        kindMap,
	}
	allOK := true
	for _, p := range parsed {
		if p.Status() != calendar.StatusOK {
			allOK = false
			break
		}
	}
	var (
		changedIDs []string
		dated      bool
	)
	err = d.Store.Tx(context.WithoutCancel(ctx), func(tx db.Store) error {
		changedIDs, dated = nil, false
		if err := addFilenameSamples(ctx, tx, courseID, names); err != nil {
			return err
		}
		auto, err := firstException(ctx, tx, courseID, allOK)
		if err != nil {
			return err
		}
		if !auto {
			raw, err := json.Marshal(rule)
			if err != nil {
				return err
			}
			p, err := json.Marshal(model.FilenameProposal{Rule: raw, Explanation: a.Explanation, CreatedAt: rule.CreatedAt})
			if err != nil {
				return err
			}
			return tx.SetKV(ctx, model.KVFilenameProposal+courseID, string(p))
		}
		undo, err := filenameUndoRecord(ctx, tx, courseID, rule.CreatedAt)
		if err != nil {
			return err
		}
		if changedIDs, dated, err = applyRule(ctx, tx, d, courseID, rule); err != nil {
			return err
		}
		if err := tx.SetKV(ctx, model.KVFilenameProposal+courseID, ""); err != nil {
			return err
		}
		return tx.SetKV(ctx, model.KVFilenameUndo+courseID, undo)
	})
	if err != nil {
		return err
	}
	for _, id := range changedIDs {
		d.events().Changed("lecture", id)
	}
	d.events().Changed("course", courseID)
	if dated {
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleMaterials})
	}
	progress(1)
	return nil
}

// firstException is P13.3.3 首次例外: apply without asking when no lecture of
// the course has a recording time, every sample parsed cleanly (allOK), the
// course has no rule written by the user (P13.3.1: never replaced
// automatically) and no citation export exists (its numbers would change
// without the P13.6 confirmation).
func firstException(ctx context.Context, tx db.Store, courseID string, allOK bool) (bool, error) {
	if !allOK {
		return false, nil
	}
	c, err := tx.GetCourse(ctx, courseID)
	if err != nil {
		return false, err
	}
	if r := courseRule(c); r != nil && r.Source == calendar.RuleSourceUser {
		return false, nil
	}
	if v, ok, err := tx.GetKV(ctx, model.KVCiteExportPrefix+courseID); err != nil {
		return false, err
	} else if ok && strings.TrimSpace(v) != "" {
		return false, nil
	}
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return false, err
	}
	for _, l := range ls {
		if l.RecordedAt != nil && strings.TrimSpace(*l.RecordedAt) != "" {
			return false, nil
		}
	}
	return true, nil
}

// filenameUndoRecord is the JSON of FilenameUndo for the course as it is now.
func filenameUndoRecord(ctx context.Context, tx db.Store, courseID, at string) (string, error) {
	c, err := tx.GetCourse(ctx, courseID)
	if err != nil {
		return "", err
	}
	ls, err := tx.ListLectures(ctx, courseID)
	if err != nil {
		return "", err
	}
	u := FilenameUndo{Rule: c.FilenameRule, Lectures: []FilenameUndoLecture{}, CreatedAt: at}
	for _, l := range ls {
		u.Lectures = append(u.Lectures, FilenameUndoLecture{
			ID: l.ID, RecordedAt: l.RecordedAt, RecordedAtSource: recordedSource(l.RecordedAtSource),
			Title: l.Title, TitleZh: l.TitleZh, TitleSource: titleSource(l.TitleSource),
			Kind: l.Kind, KindSource: kindSource(l.KindSource),
		})
	}
	b, err := json.Marshal(u)
	return string(b), err
}

// filenameRuleInput collects the samples (P13.3.2 输入): the original file
// names of the course's lectures, at most the newest
// ctxbuild.FilenameMaxSamples (one per distinct name), with the upload time
// and the stored media creation_time. When the course already has a rule,
// up to filenameOldRuleSamples of the names it matches are kept among them.
func (d Deps) filenameRuleInput(ctx context.Context, courseID string) (filenameRuleData, error) {
	var data filenameRuleData
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		data = filenameRuleData{}
		c, err := tx.GetCourse(ctx, courseID)
		if err != nil {
			return err
		}
		ls, err := tx.ListLectures(ctx, courseID) // oldest first
		if err != nil {
			return err
		}
		// newest first, one per distinct file name
		var cands []model.Lecture
		seen := map[string]bool{}
		for i := len(ls) - 1; i >= 0; i-- {
			if n := ls[i].OriginalFilename; n != "" && !seen[n] {
				seen[n] = true
				cands = append(cands, ls[i])
			}
		}
		chosen := selectSamples(cands, courseRule(c), ctxbuild.FilenameMaxSamples, filenameOldRuleSamples)
		termStart, _ := courseCalendar(c)
		data.termStart = termStart
		if data.kinds, err = listKinds(ctx, tx); err != nil {
			return err
		}
		data.in.Kinds = data.kinds.kinds
		if c.Code != nil {
			data.in.CourseCode = *c.Code
		}
		if termStart != nil {
			data.in.TermStart = calendar.FormatDate(calendar.AlignMonday(*termStart))
		}
		// oldest first, as uploaded
		for i := len(chosen) - 1; i >= 0; i-- {
			l := chosen[i]
			ct, err := storedCreationTime(ctx, tx, l.ID)
			if err != nil {
				return err
			}
			if ct != nil {
				t := ct.In(time.Local)
				ct = &t
			}
			up := uploadedAtLocal(l)
			data.samples = append(data.samples, calendar.Sample{Filename: l.OriginalFilename, UploadedAt: up, CreationTime: ct})
			fs := ctxbuild.FilenameSample{Filename: l.OriginalFilename, UploadedAt: up.Format(ctxbuild.SampleTimeLayout)}
			if ct != nil {
				fs.CreationTime = ct.Format(ctxbuild.SampleTimeLayout)
			}
			data.in.Samples = append(data.in.Samples, fs)
		}
		return nil
	})
	return data, err
}

// selectSamples picks at most max lectures from cands (newest first): the
// newest ones, except that when old is set and matches some candidates, at
// least min(keepOld, those) of the matched ones are included, replacing the
// oldest unmatched picks. The result is newest first.
func selectSamples(cands []model.Lecture, old *calendar.FilenameRule, max, keepOld int) []model.Lecture {
	if len(cands) <= max {
		return cands
	}
	chosen := append([]model.Lecture(nil), cands[:max]...)
	if old == nil {
		return chosen
	}
	r, err := calendar.Compile(*old)
	if err != nil {
		return chosen
	}
	matches := func(l model.Lecture) bool {
		return r.Parse(calendar.Sample{Filename: l.OriginalFilename, UploadedAt: uploadedAtLocal(l)}).Matched
	}
	have := 0
	for _, l := range chosen {
		if matches(l) {
			have++
		}
	}
	var extra []model.Lecture // matched, not chosen, newest first
	for _, l := range cands[max:] {
		if matches(l) {
			extra = append(extra, l)
		}
	}
	want := min(keepOld, have+len(extra))
	for j := len(chosen) - 1; j >= 0 && have < want && len(extra) > 0; j-- {
		if matches(chosen[j]) {
			continue
		}
		chosen[j] = extra[0]
		extra = extra[1:]
		have++
	}
	// keep newest first: the swapped-in lectures are older than every pick
	// they replaced, so a stable re-sort by created_at restores the order
	sortNewestFirst(chosen)
	return chosen
}

// sortNewestFirst orders lectures by created_at, newest first (insertion sort:
// at most FilenameMaxSamples items).
func sortNewestFirst(ls []model.Lecture) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j].CreatedAt > ls[j-1].CreatedAt; j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// addFilenameSamples adds names to kv filename_rule_samples:{course}.
func addFilenameSamples(ctx context.Context, tx db.Store, courseID string, names []string) error {
	cur, err := FilenameSamples(ctx, tx, courseID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, n := range cur {
		seen[n] = true
	}
	added := false
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			cur = append(cur, n)
			added = true
		}
	}
	if !added {
		return nil
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return tx.SetKV(ctx, model.KVFilenameSamples+courseID, string(b))
}

// FilenameSamples returns kv filename_rule_samples:{course}: the original
// file names already sent to the model for a rule (P13.3.2 trigger:
// lectures without a recording time whose names are not in it).
func FilenameSamples(ctx context.Context, st db.Store, courseID string) ([]string, error) {
	v, ok, err := st.GetKV(ctx, model.KVFilenameSamples+courseID)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return []string{}, err
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return []string{}, nil
	}
	return out, nil
}
