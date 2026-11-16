// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// SPEC-v3 step 5 (v5-integration-b1): the workers run with the component
// environments and models when installed, else with the configured paths.

// compLocator is a component manager with some components installed.
type compLocator struct {
	py     map[string]string // backend → interpreter
	models map[string]string // backend/name/precision → dir
}

func (c compLocator) Python(backend string) (string, bool) {
	p, ok := c.py[backend]
	return p, ok
}

func (c compLocator) ModelDir(backend, name, precision string) (string, bool) {
	d, ok := c.models[backend+"/"+name+"/"+precision]
	return d, ok
}

// fakeExe is the fake worker binary the env configured (useFakeWorker).
func (e *env) fakeExe() string {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	return exe
}

// noPython makes every configured interpreter a missing file, so a run
// succeeds only with a component's interpreter.
func (e *env) noPython() {
	e.cfg.Python = filepath.Join(e.root, "no-venv", "python.exe")
	e.cfg.PythonOpenVINO = filepath.Join(e.root, "no-venv-ov", "python.exe")
	e.cfg.PythonMLX = filepath.Join(e.root, "no-venv-mlx", "python")
}

func TestASRComponentsFasterWhisper(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	modelDir := filepath.Join(e.root, "components", "model__faster-whisper__large-v3__float16", "1.0.0", "model")
	e.cfg.Components = compLocator{
		py:     map[string]string{model.BackendFasterWhisper: exe, config.ComponentBase: filepath.Join(e.root, "nope")},
		models: map[string]string{"faster-whisper/large-v3/float16": modelDir},
	}
	l := e.lecture("L fw", "m.mp4")
	e.writeWAV(l, "wav fw")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "float16", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	jobs := fakeJobs(t, log)
	got := jobs[len(jobs)-1]
	if got.Script != "asr.py" || got.Job["model"] != modelDir {
		t.Fatalf("ran %s with model %v, want the component directory %s", got.Script, got.Job["model"], modelDir)
	}
	// the checkpoint keeps the model name of the snapshot (I6)
	if ck, _ := got.Job["checkpoint"].(map[string]any); ck == nil || ck["model"] != "large-v3" {
		t.Errorf("checkpoint %v", got.Job["checkpoint"])
	}
	// the snapshot keeps the name too
	if s, ok := model.ParseASRSnapshot(r.job.ConfigSnapshot); !ok || s.Model != "large-v3" {
		t.Errorf("snapshot %v", r.job.ConfigSnapshot)
	}

	// another model, not installed: a permanent error, never the worker's
	// own download into the global cache (V12.2 acceptance #3)
	l2 := e.lecture("L fw2", "m.mp4")
	e.writeWAV(l2, "wav fw2")
	e.setSnapshot(l2.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "medium", "float16", "en"))
	nJobs := len(fakeJobs(t, log))
	r = e.run(e.stage(e.deps(nil), model.StageASR), l2.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || errCode(r.err) != "devices.model_not_installed" {
		t.Fatalf("model not installed: err %v (code %q)", r.err, errCode(r.err))
	}
	if n := len(fakeJobs(t, log)); n != nJobs {
		t.Errorf("model not installed: the worker ran (%d -> %d jobs)", nJobs, n)
	}

	// the asr-cuda component is not installed: config python (missing here)
	e.cfg.Components = compLocator{}
	l3 := e.lecture("L fw3", "m.mp4")
	e.writeWAV(l3, "wav fw3")
	e.setSnapshot(l3.ID, model.StageASR, deviceSnapshot(model.DeviceCUDA0, "large-v3", "float16", "en"))
	r = e.run(e.stage(e.deps(nil), model.StageASR), l3.ID)
	info, _ := state.ParseJobError(deref(r.job.Error))
	if r.err == nil || !sched.IsPermanent(r.err) || codeOf(r.err) != CodeWorkerPythonMissing ||
		!paramsMatch(info, "path", e.cfg.Python) || !strings.Contains(detailOf(r.err), ".venv") {
		t.Fatalf("without the component: err %v, job.error %+v", r.err, info)
	}
}

// writeModelFiles creates files (non-empty) in dir: an installed model as
// devices.ModelAvailable checks it.
func (e *env) writeModelFiles(dir string, files ...string) {
	e.t.Helper()
	e.must(os.MkdirAll(dir, 0o755))
	for _, f := range files {
		e.must(os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644))
	}
}

// notInstalled runs the asr stage on a new lecture with the snapshot and
// requires the permanent devices.model_not_installed {model, precision}
// without the worker running (V12.2 acceptance #3).
func (e *env) notInstalled(log, name, device, mdl, prec string) {
	e.t.Helper()
	l := e.lecture(name, "m.mp4")
	e.writeWAV(l, "wav "+name)
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(device, mdl, prec, "en"))
	nJobs := len(fakeJobs(e.t, log))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || !sched.IsPermanent(r.err) || errCode(r.err) != devices.CodeModelNotInstalled {
		e.t.Fatalf("%s/%s not installed: err %v (code %q)", mdl, prec, r.err, errCode(r.err))
	}
	info, _ := state.ParseJobError(deref(r.job.Error))
	if !paramsMatch(info, "model", mdl) || !paramsMatch(info, "precision", prec) {
		e.t.Errorf("%s/%s not installed: job.error %+v", mdl, prec, info)
	}
	if n := len(fakeJobs(e.t, log)); n != nJobs {
		e.t.Errorf("%s/%s not installed: the worker ran (%d -> %d jobs)", mdl, prec, nJobs, n)
	}
}

func TestASRComponentsOpenVINO(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ov_ok", fakeASROV)
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	data := e.absDataDir()
	modelDir := filepath.Join(e.root, "components", "model__openvino__small__int8", "1.0.0", "model")
	e.writeModelFiles(modelDir, devices.ModelFiles...)
	e.cfg.Components = compLocator{
		py:     map[string]string{model.BackendOpenVINO: exe},
		models: map[string]string{"openvino/small/int8": modelDir},
	}
	run := func(mdl, prec string) fakeJob {
		t.Helper()
		l := e.lecture("L ov "+mdl+prec, "m.mp4")
		e.writeWAV(l, "wav "+mdl+prec)
		e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, mdl, prec, "en"))
		if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		jobs := fakeJobs(t, log)
		return jobs[len(jobs)-1]
	}
	if got := run("small", "int8"); got.Script != "asr_openvino.py" || got.Job["model_dir"] != modelDir ||
		got.Job["cache_dir"] != devices.CacheDir(data, "GPU", "small", "int8") {
		t.Fatalf("component model: %s %v", got.Script, got.Job)
	}
	// a precision that is not installed: a permanent error, never the
	// worker's own download and conversion (V12.2 acceptance #3)
	e.notInstalled(log, "L ov fp16", model.DeviceOVGPU, "small", "fp16")
	// another model, not installed
	e.notInstalled(log, "L ov medium", model.DeviceOVGPU, "medium", "int8")
	// a model already converted into the data directory needs no download
	e.writeModelFiles(devices.ModelDir(data, "small", "fp16"), devices.ModelFiles...)
	if got := run("small", "fp16"); got.Script != "asr_openvino.py" || got.Job["model_dir"] != devices.ModelDir(data, "small", "fp16") {
		t.Fatalf("converted model in the data directory: %s %v", got.Script, got.Job["model_dir"])
	}
	// no asr-openvino component: python_openvino (missing) → devices.ov_python_missing, .venv-ov
	e.cfg.Components = nil
	l := e.lecture("L ov none", "m.mp4")
	e.writeWAV(l, "wav none")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceOVGPU, "small", "int8", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if r.err == nil || codeOf(r.err) != devices.CodeOVPythonMissing || !strings.Contains(detailOf(r.err), ".venv-ov") ||
		!strings.Contains(detailOf(r.err), "python_openvino") {
		t.Fatalf("without the component: %v", r.err)
	}
}

func TestASRComponentsMLX(t *testing.T) {
	e := newEnv(t)
	e.useFakeMLX("ov_ok", fakeASROV)
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	data := e.absDataDir()
	modelDir := filepath.Join(e.root, "components", "model__mlx__large-v3-turbo__fp16", "1.0.0", "model")
	e.writeModelFiles(modelDir, "config.json", "weights.safetensors")
	e.cfg.Components = compLocator{
		py:     map[string]string{model.BackendMLX: exe},
		models: map[string]string{"mlx/large-v3-turbo/fp16": modelDir},
	}
	l := e.lecture("L mlx", "m.mp4")
	e.writeWAV(l, "wav mlx")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "large-v3-turbo", "fp16", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	jobs := fakeJobs(t, log)
	if got := jobs[len(jobs)-1]; got.Script != "asr_mlx.py" || got.Job["model_dir"] != modelDir {
		t.Fatalf("component: %s %v", got.Script, got.Job["model_dir"])
	}
	// model not installed (nothing in the data directory either): a
	// permanent error without the worker running (V12.2 acceptance #3)
	e.notInstalled(log, "L mlx2", model.DeviceMLXGPU, "small", "fp16")
	// a model placed by hand in the data directory is used
	e.writeModelFiles(devices.MLXModelDir(data, "small"), "config.json", "weights.npz")
	l3 := e.lecture("L mlx3", "m.mp4")
	e.writeWAV(l3, "wav mlx3")
	e.setSnapshot(l3.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "small", "fp16", "en"))
	if r := e.run(e.stage(e.deps(nil), model.StageASR), l3.ID); r.err != nil {
		t.Fatal(r.err)
	}
	jobs = fakeJobs(t, log)
	if got := jobs[len(jobs)-1]; got.Job["model_dir"] != devices.MLXModelDir(data, "small") {
		t.Fatalf("hand-placed model: %v", got.Job["model_dir"])
	}
}

// worker.go: the missing-interpreter text names the venv of the key.
func TestWorkerPythonMissingDetail(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ok", fakeASR)
	for _, c := range []struct{ key, code, venv string }{
		{"", CodeWorkerPythonMissing, ".venv"},
		{"python", CodeWorkerPythonMissing, ".venv"},
		{"python_openvino", devices.CodeOVPythonMissing, ".venv-ov"},
		{"python_mlx", devices.CodeMLXPythonMissing, ".venv-mlx"},
	} {
		w := workerCall{name: "asr", label: "x", code: workerASR, dir: t.TempDir(), job: map[string]any{}, out: "result.json"}
		missing := filepath.Join(e.root, "missing", "python.exe")
		if c.key == "" {
			e.cfg.Python = missing
		} else {
			w.python, w.pythonKey = missing, c.key
			if c.code != CodeWorkerPythonMissing {
				w.pythonCode = c.code
			}
		}
		_, err := e.deps(nil).runWorker(e.ctx, w)
		d := detailOf(err)
		if codeOf(err) != c.code || !strings.Contains(d, "（config.toml 的 "+orDefault(c.key, "python")+"）") ||
			!strings.Contains(d, "建立 "+c.venv+" ") {
			t.Errorf("key %q: code %s detail %s", c.key, codeOf(err), d)
		}
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// SPEC-v3 V5: the MLX worker's errors have their own codes.
func TestASRMLXCodes(t *testing.T) {
	e := newEnv(t)
	e.useFakeMLX("exit1", "")
	l := e.lecture("L", "m.mp4")
	e.writeWAV(l, "wav")
	e.setSnapshot(l.ID, model.StageASR, deviceSnapshot(model.DeviceMLXGPU, "large-v3-turbo", "fp16", "en"))
	r := e.run(e.stage(e.deps(nil), model.StageASR), l.ID)
	if codeOf(r.err) != workerASRMLX+workerFailed {
		t.Fatalf("worker failure: %v (%s)", r.err, codeOf(r.err))
	}
	if workerCode(model.BackendMLX) != "asr_mlx" || workerCode(model.BackendOpenVINO) != "asr_ov" || workerCode(model.BackendFasterWhisper) != "asr" {
		t.Error("workerCode")
	}
	for _, c := range []string{CodeASRMLXDeviceUnavailable, "asr_mlx.worker_start_failed", "asr_mlx.worker_input_error",
		"asr_mlx.worker_failed", "asr_mlx.worker_error", "asr_mlx.worker_no_output"} {
		found := false
		for _, x := range ErrorCodes {
			found = found || x == c
		}
		if !found {
			t.Errorf("%s not in ErrorCodes", c)
		}
	}
}

// The PDF extraction runs with the python component's base environment
// when installed, else with config python.
func TestMaterialExtractComponentPython(t *testing.T) {
	e := newEnv(t)
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["one"]}`), 0o644))
	e.useFakeWorker("ok", pages)
	exe := e.fakeExe()
	e.noPython()
	newMaterial := func(name string) string {
		m := e.material(nil, name, nil)
		mdir := model.MaterialDir(e.cfg.DataDir, m.CourseID, m.ID)
		e.must(os.MkdirAll(mdir, 0o755))
		e.must(os.WriteFile(filepath.Join(mdir, name), []byte("%PDF-1.4"), 0o644))
		return m.ID
	}
	// asr-cuda's interpreter must not be used for PDFs
	e.cfg.Components = compLocator{py: map[string]string{model.BackendFasterWhisper: exe}}
	r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), newMaterial("a.pdf"))
	if codeOf(r.err) != CodeWorkerPythonMissing {
		t.Fatalf("without the python component: %v", r.err)
	}
	e.cfg.Components = compLocator{py: map[string]string{config.ComponentBase: exe}}
	if r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), newMaterial("b.pdf")); r.err != nil {
		t.Fatalf("with the python component: %v", r.err)
	}
	e.cfg.Components = nil
	e.cfg.Python = exe
	if r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), newMaterial("c.pdf")); r.err != nil {
		t.Fatalf("config python: %v", r.err)
	}
}

// ---------- the component update self-test (V8) ----------

func TestComponentSelfTestBase(t *testing.T) {
	e := newEnv(t)
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["LectureFlow self-test"]}`), 0o644))
	e.useFakeWorker("ok", pages)
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	d := e.deps(nil)
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "python", Backend: config.ComponentBase, Python: exe}); err != nil {
		t.Fatal(err)
	}
	jobs := fakeJobs(t, log)
	if len(jobs) != 1 || jobs[0].Script != "extract_pdf.py" {
		t.Fatalf("jobs %+v", jobs)
	}
	pdf, _ := jobs[0].Job["pdf"].(string)
	if !strings.HasPrefix(pdf, filepath.Join(e.absDataDir(), "selftest", "components")) {
		t.Errorf("work file %s", pdf)
	}
	if _, err := os.Stat(filepath.Dir(pdf)); !os.IsNotExist(err) {
		t.Errorf("work directory left behind: %v", err)
	}
	// two pages for a one-page PDF fail
	e.must(os.WriteFile(pages, []byte(`{"pages": ["a", "b"]}`), 0o644))
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "python", Backend: config.ComponentBase, Python: exe}); err == nil {
		t.Error("wrong page count accepted")
	}
	// no interpreter
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "python", Backend: config.ComponentBase}); err == nil {
		t.Error("no interpreter accepted")
	}
}

func TestComponentSelfTestASR(t *testing.T) {
	e := newEnv(t)
	e.useFakeWorker("ov_ok", fakeASROV)
	log := e.jobLog()
	exe := e.fakeExe()
	e.noPython()
	d := e.deps(nil)
	ovModel := filepath.Join(e.root, "c", "model__openvino__small__int8", "1.0.0", "model")
	e.must(os.MkdirAll(ovModel, 0o755))
	for _, f := range devices.ModelFiles {
		e.must(os.WriteFile(filepath.Join(ovModel, f), []byte("x"), 0o644))
	}
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-openvino", Backend: model.BackendOpenVINO, Python: exe,
		ModelDir: ovModel, Model: "small", Precision: "int8"}); err != nil {
		t.Fatal(err)
	}
	jobs := fakeJobs(t, log)
	got := jobs[len(jobs)-1]
	if got.Script != "asr_openvino.py" || got.Job["model_dir"] != ovModel || got.Job["device"] != "CPU" ||
		got.Job["language"] != "en" || got.Job["model"] != "small" || got.Job["precision"] != "int8" || got.Job["checkpoint"] != nil {
		t.Fatalf("job %v", got.Job)
	}
	audio, _ := got.Job["audio"].(string)
	if !strings.HasPrefix(audio, filepath.Join(e.absDataDir(), "selftest", "components")) {
		t.Errorf("audio %s", audio)
	}
	// unknown model / precision: placeholders the worker accepts
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-openvino", Backend: model.BackendOpenVINO, Python: exe, ModelDir: ovModel}); err != nil {
		t.Fatal(err)
	}
	jobs = fakeJobs(t, log)
	if j := jobs[len(jobs)-1].Job; j["model"] != "small" || j["precision"] != "int8" {
		t.Errorf("placeholders %v", j)
	}
	// incomplete model files: fails before any worker runs
	e.must(os.Remove(filepath.Join(ovModel, "config.json")))
	n := len(fakeJobs(t, log))
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "model:openvino:small:int8", Backend: model.BackendOpenVINO, Python: exe,
		ModelDir: ovModel, Model: "small", Precision: "int8"}); err == nil || len(fakeJobs(t, log)) != n {
		t.Fatalf("incomplete model: %v", err)
	}

	// faster-whisper: the model directory is job.model, cuda or the CPU
	e.useFakeWorker("ok", fakeASR)
	fwModel := filepath.Join(e.root, "c", "model__faster-whisper__small__float16", "1.0.0", "model")
	e.must(os.MkdirAll(fwModel, 0o755))
	e.must(os.WriteFile(filepath.Join(fwModel, "model.bin"), []byte("x"), 0o644))
	e.noPython()
	for _, cuda := range []bool{true, false} {
		if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-cuda", Backend: model.BackendFasterWhisper, Python: exe,
			ModelDir: fwModel, Model: "small", Precision: "float16", CUDA: cuda}); err != nil {
			t.Fatal(err)
		}
		jobs = fakeJobs(t, log)
		j := jobs[len(jobs)-1]
		wantDev := map[bool]string{true: "cuda", false: "cpu"}[cuda]
		if j.Script != "asr.py" || j.Job["model"] != fwModel || j.Job["device"] != wantDev {
			t.Fatalf("faster-whisper cuda=%v: %s %v", cuda, j.Script, j.Job)
		}
	}
	// a failing worker fails the self-test with its code
	e.useFakeWorker("exit1", "")
	e.noPython()
	err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-cuda", Backend: model.BackendFasterWhisper, Python: exe, ModelDir: fwModel})
	if codeOf(err) != workerASR+workerFailed {
		t.Fatalf("failing worker: %v", err)
	}
}

func TestComponentSelfTestFallbacks(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	// an environment without any installed model: the import check
	var gotPy string
	var gotMods []string
	prev := componentImportCheck
	t.Cleanup(func() { componentImportCheck = prev })
	componentImportCheck = func(_ context.Context, python, _ string, modules []string) error {
		gotPy, gotMods = python, modules
		return nil
	}
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-mlx", Backend: model.BackendMLX, Python: "/c/mlx/python"}); err != nil {
		t.Fatal(err)
	}
	if gotPy != "/c/mlx/python" || strings.Join(gotMods, ",") != "mlx_whisper,mlx.core" {
		t.Fatalf("import check %s %v", gotPy, gotMods)
	}
	componentImportCheck = func(context.Context, string, string, []string) error { return errors.New("No module named mlx") }
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-mlx", Backend: model.BackendMLX, Python: "/c/mlx/python"}); err == nil {
		t.Error("failed import accepted")
	}
	// a model without its backend's environment: the files only
	mlxModel := filepath.Join(e.root, "m")
	e.must(os.MkdirAll(mlxModel, 0o755))
	check := ComponentCheck{ID: "model:mlx:small:fp16", Backend: model.BackendMLX, ModelDir: mlxModel, Model: "small", Precision: "fp16"}
	if err := d.ComponentSelfTest(e.ctx, check); err == nil {
		t.Error("model without files accepted")
	}
	e.must(os.WriteFile(filepath.Join(mlxModel, "config.json"), []byte("{}"), 0o644))
	e.must(os.WriteFile(filepath.Join(mlxModel, "weights.npz"), []byte("x"), 0o644))
	if err := d.ComponentSelfTest(e.ctx, check); err != nil {
		t.Fatalf("model files present: %v", err)
	}
	// nothing at all; and components without a worker (binary) pass
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "asr-cuda", Backend: model.BackendFasterWhisper}); err == nil {
		t.Error("nothing to test accepted")
	}
	if err := d.ComponentSelfTest(e.ctx, ComponentCheck{ID: "core", Backend: ""}); err != nil {
		t.Errorf("binary component: %v", err)
	}
}

// The real import check: a missing interpreter fails at once.
func TestComponentImportCheckMissingPython(t *testing.T) {
	err := componentImportCheck(context.Background(), filepath.Join(t.TempDir(), "python.exe"), "", []string{"os"})
	if err == nil {
		t.Fatal("missing interpreter accepted")
	}
}
