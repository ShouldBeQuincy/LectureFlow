// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// extractPDF runs worker/extract_pdf.py in dir (SPEC §6.0.1) and returns
// the pages. The interpreter is the python component's base environment
// (pypdf, SPEC-v3 V6) when installed, else the config python
// (config.Config.PDFPython).
func (d Deps) extractPDF(ctx context.Context, dir, pdf string, progress func(float64)) ([]string, error) {
	return d.extractPDFWith(ctx, d.Cfg.PDFPython(), dir, pdf, progress)
}

// extractPDFWith is extractPDF with the interpreter python (the component
// update self-test runs the new base environment before it is active).
// The run has the limits of pdf_limits.go: memory cap, decode budget and a
// time limit by file size and page count; over any of them is an input
// error (not retried).
func (d Deps) extractPDFWith(ctx context.Context, python, dir, pdf string, progress func(float64)) ([]string, error) {
	abs, err := filepath.Abs(pdf)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, err := os.Stat(abs); err == nil {
		size = fi.Size()
	}
	deadline := NewPDFDeadline(ctx, size, pdfTimeoutMax)
	defer deadline.Stop()
	raw, err := d.runWorker(deadline.Context(), workerCall{
		name: "extract_pdf", label: "PDF 抽取进程（extract_pdf.py）", code: workerPDF, dir: dir,
		job: PDFJob(abs), out: "result.json", progress: progress, status: deadline.Status,
		python: python, pythonKey: "python",
		errorCode: PDFErrorCode, limits: PDFLimits(), memoryErr: PDFMemoryError,
	})
	if err != nil {
		if terr := deadline.Err(ctx); terr != nil {
			return nil, terr
		}
		return nil, err
	}
	var res struct {
		Pages *[]string `json:"pages"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Pages == nil {
		return nil, model.Errorf(CodePDFResultUnparsable, nil, fmt.Errorf("PDF 抽取结果无法解析：%v", err))
	}
	return *res.Pages, nil
}

// materialExtractStage extracts the pages of a slide PDF (SPEC §3.1, §6.0.1).
type materialExtractStage struct{ d Deps }

func (s *materialExtractStage) Name() string                 { return model.StageMaterialExtract }
func (s *materialExtractStage) Resource() sched.ResourceKind { return sched.ResourceCPU }

// InputHash: none (SPEC §4.4).
func (s *materialExtractStage) InputHash(context.Context, string) (string, error) { return "", nil }

func (s *materialExtractStage) Run(ctx context.Context, materialID string, progress func(float64)) error {
	d := s.d
	m, err := d.Store.GetMaterial(ctx, materialID)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(model.MaterialDir(d.DataDir, m.CourseID, m.ID))
	if err != nil {
		return err
	}
	path := ""
	if !state.StoredPathUnsafe(state.PathMaterial, materialID, m.Path) {
		path = resolvePath(m.Path, dir) // a path naming another computer is never opened (review 2, M3)
	}
	if _, err := os.Stat(path); path == "" || err != nil {
		return sched.Permanent(model.Errorf(CodeMaterialFileMissing, map[string]any{"filename": m.Filename},
			fmt.Errorf("找不到课件文件 %s，请重新上传", m.Filename)))
	}
	pages, err := d.extractPDF(ctx, dir, path, progress)
	if err != nil {
		return err
	}
	if err := d.Store.SetMaterialPages(context.WithoutCancel(ctx), materialID, pages); err != nil {
		return err
	}
	d.events().Changed("course", m.CourseID)
	// The slides text of lectures using this material changed (SPEC §4.4).
	d.invalidate(ctx, m.CourseID, state.StaleCause{Kind: state.StaleMaterial, Material: m.Filename})
	progress(1)
	return nil
}

// materialTermsStage extracts terms from the slides (SPEC §4.3, §6.0.1, §13.2).
type materialTermsStage struct{ apiStage }

func (s *materialTermsStage) Name() string { return model.StageMaterialTerms }

// InputHash: none (SPEC §4.4).
func (s *materialTermsStage) InputHash(context.Context, string) (string, error) { return "", nil }

func (s *materialTermsStage) Run(ctx context.Context, materialID string, progress func(float64)) error {
	d := s.d
	m, err := d.Store.GetMaterial(ctx, materialID)
	if err != nil {
		return err
	}
	if m.PagesMD == nil {
		return model.Errorf(CodeMaterialNotExtracted, map[string]any{"filename": m.Filename},
			fmt.Errorf("课件 %s 还没有抽取文字（material_extract 未完成）", m.Filename))
	}
	reqs := ctxbuild.MaterialTermsRequests(m.PagesMD)
	answers := make([][]FoundTerm, len(reqs))
	var (
		mu     sync.Mutex
		failed []int
		// refused: chunks the provider declined (llm.ErrRefusal), skipped
		// like unusable ones (review fix of v3.2.x P1).
		refused      []int
		firstRefusal error
		capped       = map[int]int{} // chunk → distinct terms returned, when over the limit
	)
	pc := &progressCounter{total: len(reqs), report: func(p float64) { progress(p * 0.9) }}
	err = parallel(ctx, len(reqs), d.concurrency(config.RoleGlossary, ""), func(ctx context.Context, i int) (bool, error) {
		a, err := askJSON(llm.WithJSONSchema(ctx, ctxbuild.SchemaTerms), d.LLM, config.RoleGlossary, reqs[i].System, reqs[i].User, checkTermsAnswer)
		if err != nil {
			if isUnusable(err) {
				// No error text: it quotes the model's answer (course content).
				slog.Warn("material_terms chunk unusable", "material", materialID, "chunk", i, "problem", unusableKind(err))
				mu.Lock()
				failed = append(failed, i)
				mu.Unlock()
				pc.inc()
				return false, nil
			}
			if errors.Is(err, llm.ErrRefusal) && ctx.Err() == nil {
				slog.Warn("material_terms chunk refused by the provider", "material", materialID, "chunk", i, "category", refusalCategory(err))
				mu.Lock()
				refused = append(refused, i)
				if firstRefusal == nil {
					firstRefusal = err
				}
				mu.Unlock()
				pc.inc()
				return false, nil
			}
			return true, err
		}
		ts, total := firstTerms(*a.Terms, ctxbuild.MaterialTermsMaxTerms) // the §13.2 prompt's limit
		if total > ctxbuild.MaterialTermsMaxTerms {
			mu.Lock()
			capped[i] = total
			mu.Unlock()
		}
		answers[i] = ts
		pc.inc()
		return false, nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if len(reqs) > 0 && len(refused) == len(reqs) {
		return firstRefusal // llm.refusal, permanent: every chunk was declined
	}
	if len(reqs) > 0 && len(failed) > 0 && len(failed)+len(refused) == len(reqs) {
		return model.Errorf(CodeMaterialTermsUnparsable, map[string]any{"filename": m.Filename, "n": len(reqs), "attempts": businessRetries + 1},
			fmt.Errorf("课件 %s 的 %d 块术语提取结果都无法解析（模型输出连续 %d 次无法使用）", m.Filename, len(reqs), businessRetries+1))
	}
	var found []FoundTerm
	for _, a := range answers { // chunk order: deterministic merge
		found = append(found, a...)
	}
	res, err := MergeTerms(context.WithoutCancel(ctx), d, m.CourseID, found, model.SourceMaterial)
	if err != nil {
		return err
	}
	if err := d.enqueueGlossaryReview(ctx, m.CourseID); err != nil {
		return err
	}
	note := fmt.Sprintf("课件提取 %d 个术语：新增 %d，补全译名 %d%s", distinctTerms(found), res.Inserted, res.FilledZh, fallbackNote(res))
	parts := []model.ErrorInfo{part(NoteMaterialTermsDone, map[string]any{"n": distinctTerms(found), "inserted": res.Inserted, "filled_zh": res.FilledZh})}
	parts = append(parts, fallbackParts(res)...)
	if len(reqs) == 0 {
		note = "课件没有可提取的文字（可能是扫描版），未提取到术语"
		parts = []model.ErrorInfo{part(NoteMaterialNoText, nil)}
	}
	if len(capped) > 0 {
		ks := make([]int, 0, len(capped))
		for i := range capped {
			ks = append(ks, i)
		}
		sort.Ints(ks)
		texts := make([]string, len(ks))
		chunks, returned := make([]int, len(ks)), make([]int, len(ks))
		for k, i := range ks {
			texts[k] = fmt.Sprintf("第 %d 块模型返回 %d 个", i+1, capped[i])
			chunks[k], returned[k] = i+1, capped[i]
		}
		note += fmt.Sprintf("（%s，超过提示词的上限，每块只保留前 %d 个）", strings.Join(texts, "、"), ctxbuild.MaterialTermsMaxTerms)
		parts = append(parts, part(NoteMaterialCapped, map[string]any{
			"chunks": numList(chunks), "returned": numList(returned), "max": ctxbuild.MaterialTermsMaxTerms}))
	}
	warning := ""
	var warningInfo *model.ErrorInfo
	if len(failed) > 0 {
		sort.Ints(failed)
		texts := make([]string, len(failed))
		chunks := make([]int, len(failed))
		for k, i := range failed {
			texts[k] = fmt.Sprintf("第 %d 块", i+1)
			chunks[k] = i + 1
		}
		note += fmt.Sprintf("；共 %d 块，其中%s的模型输出无法解析，已跳过", len(reqs), strings.Join(texts, "、"))
		parts = append(parts, part(NoteMaterialSkipped, map[string]any{"total": len(reqs), "chunks": numList(chunks)}))
		// a partial result is reported like repair's (Gate B ruling, NOTES.md)
		warning = fmt.Sprintf("课件 %s 共 %d 块，其中%s的术语提取结果 %d 次都无法解析，这些页的术语没有加入术语表",
			m.Filename, len(reqs), strings.Join(texts, "、"), businessRetries+1)
		warningInfo = &model.ErrorInfo{Code: WarnMaterialTermsLost, Params: map[string]any{
			"filename": m.Filename, "total": len(reqs), "chunks": numList(chunks), "attempts": businessRetries + 1}}
	}
	if len(refused) > 0 {
		sort.Ints(refused)
		texts := make([]string, len(refused))
		chunks := make([]int, len(refused))
		for k, i := range refused {
			texts[k] = fmt.Sprintf("第 %d 块", i+1)
			chunks[k] = i + 1
		}
		note += fmt.Sprintf("；共 %d 块，其中%s被服务商的安全策略拒绝，已跳过", len(reqs), strings.Join(texts, "、"))
		parts = append(parts, part(NoteMaterialRefused, map[string]any{"total": len(reqs), "chunks": numList(chunks)}))
		rwarn := fmt.Sprintf("课件 %s 共 %d 块，其中%s的术语提取请求被服务商的安全策略拒绝，这些页的术语没有加入术语表",
			m.Filename, len(reqs), strings.Join(texts, "、"))
		rinfo := model.ErrorInfo{Code: WarnMaterialTermsRefused, Params: map[string]any{
			"filename": m.Filename, "total": len(reqs), "chunks": numList(chunks)}}
		if warningInfo == nil {
			warning, warningInfo = rwarn, noteInfo(rinfo)
		} else {
			warning, warningInfo = warning+"；"+rwarn, noteInfo(*warningInfo, rinfo)
		}
	}
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.Note, st.NoteInfo = note, noteInfo(parts...)
		st.Warning, st.WarningInfo = warning, warningInfo
	})
	progress(1)
	return nil
}
