// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/pyenv"
	"lectureflow/internal/sched"
	"lectureflow/internal/wav"
)

// SPEC-v2 I9 device self-test (phase 3b freeze, .claude/v3b-brief.md).
// FROZEN signature; body: task D.

// selfTestCompileLimit is devices.SelfTestCompileLimit; tests shorten it.
var selfTestCompileLimit = devices.SelfTestCompileLimit

// selfTestDir is the work directory of a device's self-test:
// {data_dir}/selftest/{device id with every character outside
// [A-Za-z0-9._-] replaced by "_"} (cuda:0 → cuda_0, ov:GPU → ov_GPU).
func selfTestDir(dataDir, deviceID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, deviceID)
	return filepath.Join(dataDir, "selftest", safe)
}

// DeviceTest runs the self-test of dev (sched.DeviceTestFunc; cmd wires it
// into sched.Options.DeviceTest): it picks the clip (devices.FindClipSource,
// devices.PlanClip; when no lecture gives one, the whole bundled sample of
// SPEC-v3 V4.3, devices.BundledClip, decoded by ffmpeg into a 16 kHz mono
// WAV, language en, no prompt, audio source "bundled"; neither → a
// benchmark with status failed and error CodeSelfTestNoAudio), cuts it
// into a work directory under
// {data_dir}/selftest/{device}/, runs the device's worker once per
// devices.InstalledCandidates (with the component manager only installed
// models; none installed → status failed with devices.CodeSelfTestNoModel,
// nothing downloaded) (env.BeforeCandidate before each; a fallback
// candidate only when no earlier one reached RTF <= 0.5; a load longer
// than devices.SelfTestCompileLimit fails that candidate with
// CodeSelfTestCompileTimeout), measures RTF and WER (devices.WER against
// the clip's repaired text) and the worker's peak memory, and returns the
// benchmark with devices.PickFor. Cancel (ctx) kills the worker within
// seconds; the partial benchmark comes back with status cancelled. It
// never writes the database: the scheduler stores the result.
//
// Details (task D): the clip is transcribed with the language of the
// source lecture's asr snapshot and the initial_prompt its last asr run
// actually used (job.stats.asr_prompt), so the self-test sees what the
// pipeline sees. The candidate interrupted by a cancel is not part of the
// partial results. A test whose every candidate failed has status failed
// with the first candidate's error. The load limit counts from the start
// of the worker, or from its STATUS compiling_model line, to its first
// PROGRESS line; time spent preparing the model (STATUS preparing_model /
// downloading_model) does not count. The clip file is removed at the end.
func (d Deps) DeviceTest(ctx context.Context, dev model.ASRDevice, env sched.DeviceTestEnv) (model.DeviceBenchmark, error) {
	report := env.Report
	if report == nil {
		report = func(sched.DeviceTestProgress) {}
	}
	before := env.BeforeCandidate
	if before == nil {
		before = func(ctx context.Context) error { return ctx.Err() }
	}
	b := model.DeviceBenchmark{Version: model.BenchmarkVersion, StartedAt: db.FormatTime(d.now()), Results: []model.BenchmarkResult{}}
	finish := func(status string, info *model.ErrorInfo) (model.DeviceBenchmark, error) {
		b.Status, b.Error, b.FinishedAt = status, info, db.FormatTime(d.now())
		// The tie rule keeps the device's recommended combination (power
		// management freeze, lead addition after I12).
		if p, ok := devices.PickFor(dev.ID, b.Audio.ReferenceWords, b.Results); ok {
			b.Pick = &p
		}
		return b, nil
	}
	if dev.Backend == "" {
		dev.Backend = model.DeviceBackend(dev.ID)
	}
	if len(devices.Candidates(dev.ID)) == 0 || !runnableDevice(dev.ID, dev.Backend) {
		return finish(model.BenchmarkFailed, &model.ErrorInfo{Code: CodeASRDeviceMissing, Params: map[string]any{"device": d.deviceLabel(ctx, dev.ID)},
			Detail: fmt.Sprintf("设备 %q 没有可以自测的组合", dev.ID)})
	}
	dataDir, err := filepath.Abs(d.DataDir)
	if err != nil {
		return model.DeviceBenchmark{}, err
	}
	// With the component manager only the installed models are tested
	// (devices.InstalledCandidates): never a download outside it.
	cands := devices.InstalledCandidates(d.Cfg, dataDir, dev)
	if len(cands) == 0 {
		models := strings.Join(devices.CandidateModels(dev.ID), " / ")
		return finish(model.BenchmarkFailed, &model.ErrorInfo{Code: devices.CodeSelfTestNoModel,
			Params: map[string]any{"device": d.deviceLabel(ctx, dev.ID), "models": models},
			Detail: fmt.Sprintf("设备 %q 的自测组合（%s）都没有作为组件安装，不会自行下载模型", dev.ID, models)})
	}
	report(sched.DeviceTestProgress{Phase: sched.DeviceTestPhaseClip, Steps: len(cands)})

	noAudio := func(detail string) (model.DeviceBenchmark, error) {
		return finish(model.BenchmarkFailed, &model.ErrorInfo{Code: CodeSelfTestNoAudio, Detail: detail})
	}
	dir := selfTestDir(dataDir, dev.ID)
	clip := filepath.Join(dir, model.AudioFile)

	// The user's own lecture first (I9); without one that gives a clip,
	// the bundled sample (SPEC-v3 V4.3), whole.
	var (
		clipMS           int64
		ref              string
		language, prompt string
		noLecture        string // why no lecture clip ("" = one was planned)
		start, end       int64
	)
	src, ok, err := devices.FindClipSource(ctx, d.Store, d.DataDir)
	if err != nil {
		return model.DeviceBenchmark{}, err
	}
	if !ok {
		noLecture = "没有已完成修复、且录音长于 2 分钟的讲座"
	} else {
		ss, err := d.Store.ListSentences(ctx, src.LectureID, 0, 0)
		if err != nil {
			return model.DeviceBenchmark{}, err
		}
		var planned bool
		start, end, ref, planned = devices.PlanClip(ss, devices.SelfTestClip)
		if !planned {
			noLecture = fmt.Sprintf("讲座 %q 中找不到句间停顿处的 2 分钟片段", src.Title)
		}
	}
	var sample string
	if noLecture != "" {
		var bundled bool
		sample, ref, bundled = devices.BundledClip(d.Cfg.SampleDir)
		if !bundled {
			return noAudio(noLecture + "，也没有随程序附带的示例音频")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return model.DeviceBenchmark{}, err
	}
	defer os.Remove(clip)
	if noLecture == "" {
		clipMS, err = wav.Cut(src.Audio, clip, start, end)
		if err != nil {
			return model.DeviceBenchmark{}, fmt.Errorf("self-test clip of lecture %s: %w", src.LectureID, err)
		}
		_, refWords := devices.WER(ref, "")
		b.Audio = model.BenchmarkAudio{Source: model.BenchmarkSourceLec, LectureID: src.LectureID, StartMS: start,
			EndMS: start + clipMS, ReferenceWords: refWords}
		language, prompt = d.selfTestRunParams(ctx, src.LectureID)
	} else {
		clipMS, err = d.decodeSample(ctx, sample, clip)
		if ctx.Err() != nil {
			return finish(model.BenchmarkCancelled, nil)
		}
		if err != nil {
			return model.DeviceBenchmark{}, err
		}
		_, refWords := devices.WER(ref, "")
		b.Audio = model.BenchmarkAudio{Source: model.BenchmarkSourceBundled, StartMS: 0, EndMS: clipMS, ReferenceWords: refWords}
		language, prompt = BundledLanguage, "" // the sample is an English reading
	}
	refWords := b.Audio.ReferenceWords

	reached := false // an earlier candidate ran OK with RTF <= PickMaxRTF
	for i, c := range cands {
		if c.Fallback && reached {
			continue
		}
		if err := before(ctx); err != nil || ctx.Err() != nil {
			return finish(model.BenchmarkCancelled, nil)
		}
		step := i + 1
		r := d.selfTestCandidate(ctx, dev, c, candidateRun{dir: dir, clip: clip, clipMS: clipMS, language: language,
			prompt: prompt, ref: ref, refWords: refWords}, func(phase string, p float64) {
			report(sched.DeviceTestProgress{Phase: phase, Step: step, Steps: len(cands), Candidate: c, Progress: p})
		})
		if ctx.Err() != nil {
			return finish(model.BenchmarkCancelled, nil)
		}
		b.Results = append(b.Results, r)
		if r.OK && r.RTF <= model.PickMaxRTF {
			reached = true
		}
	}
	for _, r := range b.Results {
		if r.OK {
			return finish(model.BenchmarkDone, nil)
		}
	}
	var first *model.ErrorInfo
	if len(b.Results) > 0 {
		first = b.Results[0].Error
	}
	return finish(model.BenchmarkFailed, first)
}

// checkASRModel fails permanently (devices.CodeModelNotInstalled {model,
// precision}) when a run of the normalized snapshot a would have to fetch
// its model outside the component manager (devices.ModelAvailable; V12.2
// acceptance issue 3). The asr stage calls it before asrCall (workerRun);
// the self-test filters its candidates the same way.
func (d Deps) checkASRModel(a model.ASRSnapshot) error {
	dataDir, err := filepath.Abs(d.DataDir)
	if err != nil {
		return err
	}
	if devices.ModelAvailable(d.Cfg, dataDir, a.Backend, a.Model, a.Precision) {
		return nil
	}
	return sched.Permanent(model.Errorf(devices.CodeModelNotInstalled, map[string]any{"model": a.Model, "precision": a.Precision},
		fmt.Errorf("转写模型 %s（%s，后端 %s）没有作为组件安装：请在“组件”中安装它，或在转写设备设置中改用已安装的模型", a.Model, a.Precision, a.Backend)))
}

// BundledLanguage is the language of the bundled sample (SPEC-v3 V4.3:
// an English public-domain reading).
const BundledLanguage = "en"

// decodeSample decodes the bundled sample (FLAC) into clip, a 16 kHz mono
// PCM WAV like prep's audio.wav, with ffmpeg (config ffmpeg), and returns
// its duration in milliseconds. A failed decode removes the partial file.
func (d Deps) decodeSample(ctx context.Context, sample, clip string) (int64, error) {
	tools, err := d.tools(nil)
	if err != nil {
		return 0, model.Errorf(CodePrepFFmpegMissing, map[string]any{"path": d.Cfg.FFmpeg},
			fmt.Errorf("找不到 ffmpeg（%q），无法解码示例音频：%v", d.Cfg.FFmpeg, err))
	}
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error",
		"-y", "-i", sample, "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", clip}
	msg, err := tools.FFmpeg(ctx, filepath.Dir(clip), args, func(float64) {})
	if err != nil {
		os.Remove(clip)
		return 0, fmt.Errorf("decode the bundled sample %s: %w (%s)", sample, err, strings.TrimSpace(msg))
	}
	ms, ok := wav.DurationMS(clip)
	if !ok || ms <= 0 {
		os.Remove(clip)
		return 0, fmt.Errorf("decode the bundled sample %s: no audio in the result (%s)", sample, strings.TrimSpace(msg))
	}
	return ms, nil
}

// selfTestRunParams are the language and initial_prompt of the self-test:
// the source lecture's asr snapshot language and the prompt its asr run
// used; without an asr job, the current (or legacy) settings' language and
// no prompt.
func (d Deps) selfTestRunParams(ctx context.Context, lectureID string) (language, prompt string) {
	if j, err := d.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR); err == nil {
		prompt = j.Stats.ASRPrompt
		if s, ok := model.ParseASRSnapshot(j.ConfigSnapshot); ok && s.Language != "" {
			return s.Language, prompt
		}
	}
	if d.Settings != nil {
		return d.Settings.ASR().Language, prompt
	}
	return d.Cfg.ASR.Language, prompt
}

// candidateRun is what every candidate of one self-test shares.
type candidateRun struct {
	dir, clip        string
	clipMS           int64
	language, prompt string
	ref              string
	refWords         int
}

// selfTestCandidate transcribes the clip once with candidate c on dev. A
// failure is the result's Error; when ctx is cancelled the result is
// meaningless (the caller drops it).
func (d Deps) selfTestCandidate(ctx context.Context, dev model.ASRDevice, c devices.Candidate, cr candidateRun,
	report func(phase string, p float64)) model.BenchmarkResult {
	res := model.BenchmarkResult{Model: c.Model, Precision: c.Precision}
	snap := model.ASRDeviceSnapshot(model.ASRDevice{ID: dev.ID, Backend: dev.Backend, Model: c.Model, Precision: c.Precision}, cr.language, false)
	w, err := d.asrCall(ctx, snap, cr.dir, cr.clip, cr.prompt, nil) // no checkpoint: a clip is short
	if err != nil {
		res.Error = errorInfo(err, workerCode(dev.Backend))
		return res
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu                 sync.Mutex
		loaded, timedOut   bool
		prepared, compiled bool
		started            = time.Now()
		firstProgress      time.Time
		limit              = selfTestCompileLimit
	)
	timer := time.AfterFunc(limit, func() {
		mu.Lock()
		if loaded {
			mu.Unlock()
			return
		}
		timedOut = true
		mu.Unlock()
		cancel()
	})
	defer timer.Stop()
	report(sched.DeviceTestPhaseTranscribing, 0)
	w.status = func(v string) {
		mu.Lock()
		defer mu.Unlock()
		if loaded {
			return
		}
		switch v {
		case model.ASRStatusDownloadingModel, model.ASRStatusPreparingModel:
			prepared = true
			timer.Stop() // preparing is not loading
			report(sched.DeviceTestPhasePreparing, 0)
		case model.ASRStatusCompilingModel:
			compiled = true
			timer.Reset(limit)
			report(sched.DeviceTestPhaseCompiling, 0)
		}
	}
	w.progress = func(p float64) {
		mu.Lock()
		if !loaded {
			loaded = true
			firstProgress = time.Now()
			timer.Stop()
		}
		mu.Unlock()
		report(sched.DeviceTestPhaseTranscribing, p)
	}
	raw, err := d.runWorker(cctx, w)
	finished := time.Now()
	mu.Lock()
	res.Prepared, res.FirstCompile = prepared, compiled
	to := timedOut
	mu.Unlock()
	if err != nil {
		switch {
		case ctx.Err() != nil:
		case to:
			mins := int(math.Ceil(limit.Minutes()))
			res.Error = &model.ErrorInfo{Code: CodeSelfTestCompileTimeout, Params: map[string]any{"minutes": mins},
				Detail: fmt.Sprintf("加载模型超过 %d 分钟，已停止这一组合", mins)}
		default:
			res.Error = errorInfo(err, workerCode(dev.Backend))
		}
		return res
	}
	var out asrResult
	if err := json.Unmarshal(raw, &out); err != nil {
		res.Error = &model.ErrorInfo{Code: CodeASRResultUnparsable, Detail: err.Error()}
		return res
	}
	var hyp []string
	for _, s := range out.Sentences {
		if t := strings.TrimSpace(s.Text); t != "" {
			hyp = append(hyp, t)
		}
		if len(s.Words) > 0 {
			res.Words = true
		}
	}
	if firstProgress.IsZero() {
		firstProgress = finished
	}
	res.LoadS, res.TranscribeS = firstProgress.Sub(started).Seconds(), finished.Sub(firstProgress).Seconds()
	if t := out.Timing; t != nil {
		res.LoadS, res.TranscribeS = t.LoadS, t.TranscribeS
		res.FirstCompile = res.FirstCompile || t.FirstCompile
		res.PeakMemMB = int(math.Round(max(t.PeakWorkingSetMB, 0)))
	}
	if cr.clipMS > 0 {
		res.RTF = res.TranscribeS / (float64(cr.clipMS) / 1000)
	}
	if cr.refWords > 0 {
		v, _ := devices.WER(cr.ref, strings.Join(hyp, " "))
		res.WER = &v
	}
	res.OK = true
	return res
}

// workerCode is the worker error prefix of a backend.
func workerCode(backend string) string {
	switch backend {
	case model.BackendOpenVINO:
		return workerASROV
	case model.BackendMLX:
		return workerASRMLX
	}
	return workerASR
}

// errorInfo is the coded form of a candidate's error (Detail = its text
// without the code); an uncoded error becomes {prefix}.worker_error.
func errorInfo(err error, prefix string) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) && ce.Info.Code != "" {
		info := ce.Info
		info.Detail = detailOf(err)
		return &info
	}
	return &model.ErrorInfo{Code: prefix + workerError, Detail: err.Error()}
}

// ---------- the component update self-test (SPEC-v3 V8) ----------

// ComponentCheck describes the self-test of a component update: cmd builds
// it from components.SelfTestRequest (the new version is installed but not
// active yet) and runs ComponentSelfTest.
type ComponentCheck struct {
	ID string // the component id (messages only)
	// Backend: config.ComponentBase (the python component: pypdf, the PDF
	// worker), model.BackendFasterWhisper, BackendOpenVINO or BackendMLX.
	// Anything else has nothing to test.
	Backend string
	// Python is the interpreter to run the worker with: the new
	// environment, or for a model component the backend's active one; ""
	// = the backend's environment is not installed.
	Python string
	// ModelDir is the model to load (the new model, or for an environment
	// an installed model of its backend); "" = none installed.
	ModelDir string
	// Model and Precision name ModelDir in job.json ("" = unknown: a
	// placeholder valid for the worker is used; the files are what count).
	Model, Precision string
	// CUDA: faster-whisper runs on the NVIDIA GPU (the asr-cuda component
	// exists for it); false = the CPU.
	CUDA bool
}

// componentSelfTestTimeout bounds one component self-test (a large model
// loads in well under a minute on any device the components serve).
var componentSelfTestTimeout = 10 * time.Minute

// componentImportModules are what the import check of an environment
// without any installed model imports (the components' check_imports).
var componentImportModules = map[string][]string{
	model.BackendFasterWhisper: {"faster_whisper", "ctranslate2"},
	model.BackendOpenVINO:      {"openvino", "openvino_genai"},
	model.BackendMLX:           {"mlx_whisper", "mlx.core"},
}

// importCheckCmd is the command of componentImportCheck: `{py} -I -c` with
// the telemetry packages blocked before the imports (`import openvino`
// alone used to report to Google Analytics) and pyenv's environment, run
// in dir: the worker directory (config worker_dir: the application's own
// resources/worker, or the repository's worker/ in development), or with
// dir "" the interpreter's own directory (the environment's Scripts or bin
// folder, which the component manager or the user set up). Never the
// service's current directory: the Windows DLL search looks there when the
// imports load native extensions (SEC47 info-5, SEC50 info-2). -I keeps the
// directory off sys.path either way.
func importCheckCmd(ctx context.Context, py, dir string, modules []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, py, "-I", "-c", pyenv.ImportCheck(modules))
	if dir == "" {
		dir = filepath.Dir(py)
	}
	cmd.Dir = dir
	cmd.Env = pyenv.Environ("PYTHONIOENCODING=utf-8")
	return cmd
}

// componentImportCheck runs `{python} -I -c "import …"` in dir
// (importCheckCmd; tests replace it).
var componentImportCheck = func(ctx context.Context, python, dir string, modules []string) error {
	py, err := executable(python)
	if err != nil {
		return fmt.Errorf("interpreter %s: %w", python, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := importCheckCmd(ctx, py, dir, modules)
	cmd.WaitDelay = 3 * time.Second
	tail := &tailBuffer{n: stderrTailBytes}
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("import %s: %v: %s", strings.Join(modules, ", "), err, tail.String())
	}
	return nil
}

// ComponentSelfTest is the self-test of a component update (SPEC-v3 V8:
// the worker starts and processes one second of silence) before the new
// version becomes active:
//   - the python component (base): worker/extract_pdf.py on a generated
//     one-page PDF must return one page;
//   - an ASR environment or model: the backend's worker transcribes 1 s of
//     digital silence (16 kHz mono WAV, language en) with the model —
//     faster-whisper on cuda (CUDA set) or the CPU, OpenVINO on its CPU
//     device (always present, no long GPU / NPU compile), MLX on mlx:gpu —
//     and must exit 0 with a parsable result (no sentences is fine);
//   - an ASR environment without any installed model of its backend
//     degrades to an import check of the backend's modules;
//   - a model whose backend environment is not installed (Python "")
//     degrades to checking its files (the worker's own requirements).
//
// The model files are checked first in every case with a model, so a
// broken model never makes the OpenVINO worker download and convert one.
// Work files live under {data_dir}/selftest/components/ and are removed.
// A failure is the returned error (coded when the worker's was).
func (d Deps) ComponentSelfTest(ctx context.Context, c ComponentCheck) error {
	ctx, cancel := context.WithTimeout(ctx, componentSelfTestTimeout)
	defer cancel()
	switch c.Backend {
	case config.ComponentBase:
		if c.Python == "" {
			return errors.New("the base environment has no interpreter")
		}
		dir, err := d.componentTestDir(c.ID)
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		pdf := filepath.Join(dir, "selftest.pdf")
		if err := os.WriteFile(pdf, selfTestPDF(), 0o644); err != nil {
			return err
		}
		pages, err := d.extractPDFWith(ctx, c.Python, dir, pdf, nil)
		if err != nil {
			return err
		}
		if len(pages) != 1 {
			return fmt.Errorf("extract_pdf.py returned %d pages for a one-page PDF", len(pages))
		}
		return nil
	case model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX:
	default:
		return nil
	}
	if c.ModelDir != "" {
		if err := checkModelFiles(c.Backend, c.ModelDir); err != nil {
			return err
		}
	}
	switch {
	case c.Python == "" && c.ModelDir == "":
		return errors.New("neither an interpreter nor a model to test")
	case c.Python == "":
		return nil // the files were checked above
	case c.ModelDir == "":
		return componentImportCheck(ctx, c.Python, d.Cfg.WorkerDir, componentImportModules[c.Backend])
	}
	dir, err := d.componentTestDir(c.ID)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	audio := filepath.Join(dir, model.AudioFile)
	if err := os.WriteFile(audio, silenceWAV(1000), 0o644); err != nil {
		return err
	}
	snap := model.ASRSnapshot{Backend: c.Backend, Model: c.Model, Precision: c.Precision, Language: BundledLanguage}
	switch c.Backend {
	case model.BackendFasterWhisper:
		snap.DeviceID, snap.Precision = model.DeviceFWCPU, "auto"
		if c.CUDA {
			snap.DeviceID = model.DeviceCUDA0
		}
	case model.BackendOpenVINO:
		snap.DeviceID = model.DeviceOVCPU
	case model.BackendMLX:
		snap.DeviceID = model.DeviceMLXGPU
	}
	if snap.Model == "" {
		snap.Model = "small" // a placeholder the workers accept; model_dir decides
	}
	if c.Backend != model.BackendFasterWhisper && !model.ValidPrecision(c.Backend, snap.Precision) {
		snap.Precision = "int8"
	}
	w, err := d.asrCallAt(ctx, snap, dir, audio, "", nil, &workerLoc{python: c.Python, modelDir: c.ModelDir})
	if err != nil {
		return err
	}
	raw, err := d.runWorker(ctx, w)
	if err != nil {
		return err
	}
	var out asrResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return model.Errorf(CodeASRResultUnparsable, nil, fmt.Errorf("self-test result: %w", err))
	}
	return nil
}

// checkModelFiles checks a model directory as the backend's worker needs it.
func checkModelFiles(backend, dir string) error {
	ok := false
	switch backend {
	case model.BackendFasterWhisper:
		ok = devices.FWModelFilesReady(dir)
	case model.BackendOpenVINO:
		ok = devices.ModelFilesReady(dir)
	case model.BackendMLX:
		ok = devices.MLXModelFilesReady(dir)
	}
	if !ok {
		return fmt.Errorf("the model directory %s lacks the files the %s worker needs", dir, backend)
	}
	return nil
}

// componentTestDir creates a fresh work directory for a component
// self-test: {data_dir}/selftest/components/{h}-{n}, h the 8 hex digits of
// the FNV-1a hash of the component id and n the random number os.MkdirTemp
// picks, so two self-tests (of the same component, too) never share a
// directory. The worker runs in it, so its depth below the data directory,
// files included, is kept within the material folder's (internal/api
// workDirDepth), which keeps it within both budgets of the diagnostics'
// data directory length check, the working directory's and the deepest
// worker file's (internal/api deepestFileDepth, R6-E): with the component
// id in the name (model ids reach 48 characters; before 3.2.x the name was
// {safe id}-{UnixNano base36}) it did not.
func (d Deps) componentTestDir(id string) (string, error) {
	parent, err := filepath.Abs(selfTestDir(d.DataDir, "components"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	h := fnv.New32a()
	h.Write([]byte(id))
	dir, err := os.MkdirTemp(parent, fmt.Sprintf("%08x-*", h.Sum32()))
	if err == nil {
		// the name has only the hash: this line ties the worker's "dir" to the component
		slog.Info("component self-test", "component", id, "dir", dir)
	}
	return dir, err
}

// silenceWAV is ms milliseconds of digital silence as a 16 kHz mono 16-bit
// PCM WAV file (the format prep writes).
func silenceWAV(ms int) []byte {
	const rate = 16000
	data := rate * ms / 1000 * 2
	le32 := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	le16 := func(v int) []byte { return []byte{byte(v), byte(v >> 8)} }
	b := make([]byte, 0, 44+data)
	b = append(b, "RIFF"...)
	b = append(b, le32(36+data)...)
	b = append(b, "WAVEfmt "...)
	b = append(b, le32(16)...)
	b = append(b, le16(1)...) // PCM
	b = append(b, le16(1)...) // mono
	b = append(b, le32(rate)...)
	b = append(b, le32(rate*2)...) // byte rate
	b = append(b, le16(2)...)      // block align
	b = append(b, le16(16)...)     // bits per sample
	b = append(b, "data"...)
	b = append(b, le32(data)...)
	return append(b, make([]byte, data)...)
}

// selfTestPDF is a valid one-page PDF with one line of text (Helvetica).
func selfTestPDF() []byte {
	content := "BT /F1 12 Tf 10 30 Td (LectureFlow self-test) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 60] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return []byte(b.String())
}
