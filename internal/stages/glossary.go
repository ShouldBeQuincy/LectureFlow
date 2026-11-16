// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/glossary"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// FoundTerm is one term extracted by the LLM (SPEC §13.1 output).
type FoundTerm struct {
	En string `json:"en"`
	Zh string `json:"zh"`
}

type termsAnswer struct {
	Terms *[]FoundTerm `json:"terms"`
}

func checkTermsAnswer(a *termsAnswer) error {
	if a.Terms == nil {
		return errors.New(`answer has no "terms" array`)
	}
	return nil
}

// MergeResult tells what MergeTerms changed.
type MergeResult struct {
	Inserted, FilledZh, FreqChanged int
	// Fallback are the changes of the G2 fallback rule, applied in the merge
	// transaction when no glossary janitor is configured (SPEC-v2 G2).
	Fallback []model.JanitorItem
	Version  int // glossary_version after the merge (unchanged when nothing changed)
}

// Changed reports whether anything that bumps glossary_version was written
// (a common_word mark alone does not: it is a pre-screen flag, G2).
func (r MergeResult) Changed() bool {
	return r.Inserted+r.FilledZh+r.FreqChanged+len(r.Fallback) > 0
}

// errTermSource is MergeTerms' refusal of a source other than the syllabus
// or the slides (SPEC-v2 G6 rule 1).
func errTermSource(source string) error {
	return model.Errorf(CodeGlossaryBadSource, map[string]any{"source": source},
		fmt.Errorf("术语只能从大纲或课件中提取，来源 %q 不能写入术语表", source))
}

// MergeTerms merges extracted terms into the course glossary (SPEC §6.0.1),
// shared by glossary_seed and material_terms. In one transaction:
//   - terms are identified by en_key; en is trimmed with whitespace runs
//     collapsed, zh is trimmed;
//   - dropped: fewer than 2 characters, more than 6 words, only digits or
//     punctuation (ctxbuild.DropTerm);
//   - dropped whole, never cut (AUD5 low-1): an en or zh over the length
//     limits (ctxbuild.TermTooLong): it is text rather than a term, and the
//     glossary goes into every later request of the course; such entries
//     are counted in one service-log line that does not quote them;
//   - existing source=manual or locked (v2 G1): untouched; existing with
//     empty zh and a new non-empty zh: zh filled in, unless the zh was set by
//     the janitor or the user (zh_source, G6 rule 3: "" from fix_zh means
//     "keep in English"); existing with a zh: kept;
//   - new terms are inserted enabled with the given source, tier enforce,
//     zh_source extractor (a later duplicate in the same batch can still
//     fill an empty zh);
//   - freq of every course term is recomputed: case-insensitive whole-word
//     (whole-phrase) matches over the text of all the course's materials;
//   - common_word (G2) is recomputed for every term;
//   - without a configured glossary janitor (state.JanitorConfigured), the
//     G2 fallback rule (glossary.Fallback) runs in the same transaction and
//     a fallback report is stored when it changed something;
//   - any actual change bumps glossary_version, once.
//
// SPEC-v2 G6 rule 1: source must be syllabus or material; anything else is
// refused before anything is written.
//
// After the commit, when something changed, Deps.Invalidate(courseID) runs
// and Events.Changed("terms", courseID) is sent.
func MergeTerms(ctx context.Context, d Deps, courseID string, found []FoundTerm, source string) (MergeResult, error) {
	if source != model.SourceSyllabus && source != model.SourceMaterial {
		return MergeResult{}, errTermSource(source)
	}
	var res MergeResult
	overlong := 0
	err := d.Store.Tx(ctx, func(tx db.Store) error {
		res, overlong = MergeResult{}, 0
		c, err := tx.GetCourse(ctx, courseID)
		if err != nil {
			return err
		}
		existing, err := tx.ListTerms(ctx, courseID)
		if err != nil {
			return err
		}
		byKey := make(map[string]*model.Term, len(existing))
		terms := make([]*model.Term, 0, len(existing)+len(found))
		for i := range existing {
			t := &existing[i]
			key := t.EnKey
			if key == "" {
				key = ctxbuild.EnKey(t.En)
			}
			byKey[key] = t
			terms = append(terms, t)
		}
		for _, f := range found {
			en := strings.Join(strings.Fields(f.En), " ")
			zh := strings.TrimSpace(f.Zh)
			if ctxbuild.DropTerm(en) {
				continue
			}
			if ctxbuild.TermTooLong(en, zh) {
				overlong++
				continue
			}
			key := ctxbuild.EnKey(en)
			if t, ok := byKey[key]; ok {
				if t.Source == model.SourceManual || t.Locked || t.Zh != "" || zh == "" ||
					(t.ZhSource != "" && t.ZhSource != model.ZhSourceExtractor) {
					continue
				}
				t.Zh = zh
				if err := tx.UpdateTerm(ctx, *t); err != nil {
					return err
				}
				res.FilledZh++
				continue
			}
			t := &model.Term{ID: db.NewID(), CourseID: courseID, En: en, EnKey: key, Zh: zh, Source: source, Enabled: true,
				Tier: model.TierEnforce, ZhSource: model.ZhSourceExtractor, CommonWord: glossary.IsCommonWord(en)}
			if err := tx.InsertTerm(ctx, *t); err != nil {
				return err
			}
			byKey[key] = t
			terms = append(terms, t)
			res.Inserted++
		}
		mats, err := tx.ListMaterials(ctx, courseID)
		if err != nil {
			return err
		}
		var all strings.Builder
		for _, m := range mats {
			for _, p := range m.PagesMD {
				all.WriteString(p)
				all.WriteByte('\n')
			}
		}
		text := ctxbuild.PrepareTermText(all.String())
		for _, t := range terms {
			if f := text.Count(t.En); f != t.Freq {
				if err := tx.SetTermFreq(ctx, t.ID, f); err != nil {
					return err
				}
				t.Freq = f
				res.FreqChanged++
			}
			if cw := glossary.IsCommonWord(t.En); cw != t.CommonWord {
				t.CommonWord = cw
				if err := tx.UpdateTerm(ctx, *t); err != nil {
					return err
				}
			}
		}
		configured, err := state.JanitorConfigured(ctx, tx)
		if err != nil {
			return err
		}
		if !configured {
			if res.Fallback, err = glossary.Fallback(ctx, tx, courseID); err != nil {
				return err
			}
		}
		res.Version = c.GlossaryVersion
		if res.Changed() {
			if res.Version, err = tx.BumpGlossaryVersion(ctx, courseID); err != nil {
				return err
			}
		}
		if len(res.Fallback) > 0 {
			return glossary.SaveRunReport(ctx, tx, model.JanitorReport{
				CourseID: courseID, FromVersion: c.GlossaryVersion, Version: res.Version,
				CreatedAt: glossary.Timestamp(d.now()), Fallback: true, Note: glossary.NoteFallbackMerge,
				NoteInfo: glossary.NoteInfo(glossary.CodeNoteFallbackMerge, nil), Items: res.Fallback,
			})
		}
		return nil
	})
	if err != nil {
		return MergeResult{}, err
	}
	if overlong > 0 {
		// counts only: the entries are course material (or injected text)
		slog.Warn("glossary: over-long extracted terms dropped", "course", courseID, "source", source,
			"count", overlong, "max_en", ctxbuild.TermEnMaxChars, "max_zh", ctxbuild.TermZhMaxChars)
	}
	if res.Changed() {
		d.events().Changed("terms", courseID)
		d.invalidate(ctx, courseID, state.StaleCause{Kind: state.StaleGlossary})
	}
	return res, nil
}

// firstTerms keeps the first n distinct usable terms of one model answer
// (the §13.1 / §13.2 prompts ask for "at most n"; NOTES lead 第四批 1).
// Entries the §6.0.1 rules drop and repeated en_keys do not count toward n;
// a repeat of a kept term is still passed on (it may fill an empty zh in
// MergeTerms, which applies every rule again). total is the number of
// distinct usable terms the model returned.
//
// An entry over the length limits (ctxbuild.TermTooLong) is not usable and
// never counts toward n or total, so it cannot push real terms out; it is
// still passed on so that MergeTerms, which drops it, logs it with the
// course.
func firstTerms(found []FoundTerm, n int) (kept []FoundTerm, total int) {
	seen := map[string]bool{}
	for _, f := range found {
		en := strings.Join(strings.Fields(f.En), " ")
		if ctxbuild.DropTerm(en) {
			continue
		}
		if ctxbuild.TermTooLong(en, f.Zh) {
			kept = append(kept, f)
			continue
		}
		k := ctxbuild.EnKey(en)
		if !seen[k] {
			seen[k] = true
			total++
			if total > n {
				continue
			}
		} else if !keptKey(kept, k) {
			continue
		}
		kept = append(kept, f)
	}
	return kept, total
}

// distinctTerms counts the distinct usable en_keys of found (entries over
// the length limits are not usable).
func distinctTerms(found []FoundTerm) int {
	seen := map[string]bool{}
	for _, f := range found {
		en := strings.Join(strings.Fields(f.En), " ")
		if !ctxbuild.DropTerm(en) && !ctxbuild.TermTooLong(en, f.Zh) {
			seen[ctxbuild.EnKey(en)] = true
		}
	}
	return len(seen)
}

// keptKey: some kept entry has en_key k.
func keptKey(kept []FoundTerm, k string) bool {
	for _, f := range kept {
		if ctxbuild.EnKey(strings.Join(strings.Fields(f.En), " ")) == k {
			return true
		}
	}
	return false
}

// glossaryStage is glossary_seed: terms from the syllabus (SPEC §4.3,
// §6.0.1, §13.1; v1.1 A8.2: from SYLLABUS.md). A course stage: it has no
// snapshot and the llm client uses the glossary role's current setting.
type glossaryStage struct{ apiStage }

func (s *glossaryStage) Name() string { return model.StageGlossarySeed }

// InputHash: none (SPEC §4.4); a new syllabus resets the job instead.
func (s *glossaryStage) InputHash(context.Context, string) (string, error) { return "", nil }

func (s *glossaryStage) Run(ctx context.Context, courseID string, progress func(float64)) error {
	d := s.d
	sy, err := d.Store.GetSyllabus(ctx, courseID)
	if errors.Is(err, db.ErrNotFound) {
		return sched.Permanent(model.Errorf(CodeGlossaryNoSyllabus, nil, errors.New("课程没有大纲，无法提取术语")))
	}
	if err != nil {
		return err
	}
	// v1.1 A8.2: the input is syllabus.text, which holds SYLLABUS.md (the
	// formatted, user-confirmed syllabus). The file is not read again here.
	// Syllabi uploaded before v1.1 may have no text yet (a PDF that was never
	// extracted): extract it from the uploaded file as v1 did.
	text := ""
	if sy.Text != nil {
		text = *sy.Text
	}
	if strings.TrimSpace(text) == "" {
		text, err = s.syllabusText(ctx, courseID, sy, progress)
		if err != nil {
			return err
		}
		if err := d.Store.SetSyllabusText(context.WithoutCancel(ctx), courseID, text); err != nil {
			return err
		}
		d.events().Changed("course", courseID)
	}
	if strings.TrimSpace(text) == "" {
		sched.UpdateStats(ctx, func(st *model.JobStats) {
			st.Note = "大纲中没有可提取的文字（可能是扫描版 PDF），未提取到术语；可以改传 .txt 或 .md 版本的大纲"
			st.NoteInfo = noteInfo(part(NoteGlossarySeedNoText, nil))
		})
		progress(1)
		return nil
	}
	progress(0.3)
	req := ctxbuild.GlossarySeedRequest(text)
	a, err := askJSON(llm.WithJSONSchema(ctx, ctxbuild.SchemaTerms), d.LLM, config.RoleGlossary, req.System, req.User, checkTermsAnswer)
	if err != nil {
		if isUnusable(err) {
			return fmt.Errorf("术语提取失败：%w", err)
		}
		return err
	}
	progress(0.9)
	found, total := firstTerms(*a.Terms, ctxbuild.GlossarySeedMaxTerms)
	kept := ""
	var keptPart []model.ErrorInfo
	if total > ctxbuild.GlossarySeedMaxTerms {
		kept = fmt.Sprintf("（模型返回 %d 个，超过提示词的上限，只保留前 %d 个）", total, ctxbuild.GlossarySeedMaxTerms)
		keptPart = []model.ErrorInfo{part(NoteGlossarySeedCapped, map[string]any{"returned": total, "max": ctxbuild.GlossarySeedMaxTerms})}
	}
	res, err := MergeTerms(context.WithoutCancel(ctx), d, courseID, found, model.SourceSyllabus)
	if err != nil {
		return err
	}
	if err := d.enqueueGlossaryReview(ctx, courseID); err != nil {
		return err
	}
	n := min(total, ctxbuild.GlossarySeedMaxTerms)
	parts := []model.ErrorInfo{part(NoteGlossarySeedDone, map[string]any{"n": n, "inserted": res.Inserted, "filled_zh": res.FilledZh})}
	parts = append(append(parts, fallbackParts(res)...), keptPart...)
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Note = fmt.Sprintf("大纲提取 %d 个术语：新增 %d，补全译名 %d%s%s", n, res.Inserted, res.FilledZh, fallbackNote(res), kept)
		st.NoteInfo = noteInfo(parts...)
	})
	progress(1)
	return nil
}

// syllabusText extracts the syllabus text when the upload did not: PDFs via
// worker/extract_pdf.py (pages joined by "\n\n", SPEC §6.0.1), other files
// read as UTF-8 text.
func (s *glossaryStage) syllabusText(ctx context.Context, courseID string, sy model.Syllabus, progress func(float64)) (string, error) {
	d := s.d
	dir, err := filepath.Abs(model.SyllabusDir(d.DataDir, courseID))
	if err != nil {
		return "", err
	}
	// a path naming another computer is never opened (review 2, M3)
	path := ""
	if !state.StoredPathUnsafe(state.PathSyllabus, courseID, sy.Path) {
		path = resolvePath(sy.Path, dir)
		if path == "" && !state.StoredPathUnsafe(state.PathSyllabus, courseID, sy.Filename) {
			path = resolvePath(sy.Filename, dir)
		}
	}
	if _, err := os.Stat(path); path == "" || err != nil {
		return "", sched.Permanent(model.Errorf(CodeGlossarySyllabusMissing, map[string]any{"filename": sy.Filename},
			fmt.Errorf("找不到大纲文件 %s，请重新上传大纲", sy.Filename)))
	}
	if strings.EqualFold(filepath.Ext(path), ".pdf") || strings.EqualFold(filepath.Ext(sy.Filename), ".pdf") {
		pages, err := d.extractPDF(ctx, dir, path, func(p float64) { progress(p * 0.3) })
		if err != nil {
			return "", err
		}
		return strings.Join(pages, "\n\n"), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if !utf8.Valid(b) {
		return "", sched.Permanent(model.Errorf(CodeGlossarySyllabusNotUTF8, map[string]any{"filename": sy.Filename},
			fmt.Errorf("大纲文件 %s 不是 UTF-8 文本，请另存为 UTF-8 编码后重新上传", sy.Filename)))
	}
	return string(b), nil
}
