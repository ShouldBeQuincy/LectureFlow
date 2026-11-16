// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// mac-opti M3: the diagnostics follow the platform and the installed
// components; M5: macOS has no fw:cpu.

func init() {
	// The tests of this package describe the Windows diagnostics and device
	// rules (devices.GOOS: fw:cpu, faster-whisper, CUDA); they hold on every
	// host, a Mac included. The macOS ones are tested below (pinGOOS).
	devices.GOOS = "windows"
}

// pinGOOS makes the servers and registries the test creates follow goos.
func pinGOOS(t *testing.T, goos string) {
	t.Helper()
	old := devices.GOOS
	devices.GOOS = goos
	t.Cleanup(func() { devices.GOOS = old })
}

func checkNames(cs []SystemCheck) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// diagNow runs the checks (withCUDA: as "重新检查" does) and returns the
// page's rows, without HTTP (desktop mode would want the token).
func diagNow(t *testing.T, e *env, withCUDA bool) ([]SystemCheck, map[string]SystemCheck) {
	t.Helper()
	e.srv.diag.waitStartup()
	e.srv.diag.run(context.Background(), withCUDA)
	cs := e.srv.diag.snapshot()
	by := map[string]SystemCheck{}
	for _, c := range cs {
		by[c.Name] = c
	}
	return cs, by
}

// cmdLog records the programs the checks run (and fakes them like fakeCmd).
type cmdLog struct {
	mu   sync.Mutex
	runs []string // "program arg0 arg1 ..."
}

func (l *cmdLog) run(ctx context.Context, name string, args ...string) (string, string, error) {
	l.mu.Lock()
	l.runs = append(l.runs, strings.Join(append([]string{name}, args...), " "))
	l.mu.Unlock()
	return fakeCmd(ctx, name, args...)
}

func (l *cmdLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.runs...)
}

// The reference Windows machine (NVIDIA + Intel GPU and NPU): the same rows
// in the same order as before mac-opti, startup and recheck, web mode.
func TestDiagnosticsWindowsChecksUnchanged(t *testing.T) {
	// ext-step1: plus cudnn after gpu
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "faster-whisper", "pypdf", "gpu", "cudnn", "cuda",
		"openvino_venv", "openvino", "openvino_devices", "npu_driver", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	de := newDevEnv(t, envConfig{})
	de.srv.diag.waitStartup()
	var sys System
	de.call("GET", "/api/system", nil, 200, &sys)
	if got := checkNames(sys.Checks); !slices.Equal(got, want) {
		t.Fatalf("startup checks %v, want %v", got, want)
	}
	if sys.OpenVINO == nil {
		t.Fatal("no OpenVINO block on the reference machine")
	}
	de.call("POST", "/api/system/recheck", nil, 200, &sys)
	if got := checkNames(sys.Checks); !slices.Equal(got, want) {
		t.Fatalf("checks after recheck %v, want %v", got, want)
	}
	by := checksByName(sys)
	for _, n := range []string{"data_dir", "ffmpeg", "python", "faster-whisper", "pypdf", "gpu", "cudnn", "cuda"} {
		if by[n].Status != statusOK {
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	// web mode: the developer's hint of a missing .venv-ov, as before
	wantCheck(t, by["openvino_venv"], "check.openvino_venv_missing", "check.hint_openvino_venv")
}

// Windows without an NVIDIA GPU: no GPU and CUDA rows and no CUDA
// self-test; they come back when the hardware detection of the setup guide
// found an NVIDIA card (a missing driver). ext-step1: no faster-whisper row
// either (its text names NVIDIA) but the CPU self-test; a cuda:0 row no
// longer counts; the rows of the new environment come from a rerun of the
// checks.
func TestDiagnosticsWindowsWithoutNVIDIA(t *testing.T) {
	log := &cmdLog{}
	de := newDevEnv(t, envConfig{server: func(s *Server) { s.runCmd = log.run }})
	de.reg.mu.Lock()
	de.reg.rows = slices.DeleteFunc(de.reg.rows, func(d model.ASRDevice) bool { return d.ID == model.DeviceCUDA0 })
	de.reg.det.CUDAName = ""
	det := de.reg.det
	de.reg.detectDet = &det
	de.reg.mu.Unlock()
	cs, _ := diagNow(t, de.env, true)
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf",
		"openvino_venv", "openvino", "openvino_devices", "npu_driver", "cpu_selftest", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	if got := checkNames(cs); !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
	for _, r := range log.list() {
		if strings.Contains(r, "--selftest") {
			t.Errorf("the CUDA self-test ran without an NVIDIA GPU: %s", r)
		}
	}
	// The setup guide's hardware detection saw an NVIDIA card (through the
	// system's device list: nvidia-smi does not run).
	cs2 := de.srv.compState()
	cs2.hwMu.Lock()
	cs2.hw = &hwdetect.HW{OS: "windows", NVIDIA: []hwdetect.GPU{{Name: "NVIDIA GeForce RTX 4060", Source: "wmi"}}}
	cs2.hwMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = checkNames(de.srv.diag.snapshot()); slices.Contains(got, "gpu") && slices.Contains(got, "cuda") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(got, "gpu") || !slices.Contains(got, "cuda") || slices.Contains(got, "cpu_selftest") {
		t.Fatalf("with the detected NVIDIA card: %v", got)
	}
}

// Windows without an Intel GPU / NPU and without an OpenVINO environment:
// no OpenVINO rows; an installed environment shows them.
func TestDiagnosticsWindowsWithoutIntel(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	missing := filepath.Join(t.TempDir(), "missing", "python.exe")
	de.reg.mu.Lock()
	de.reg.rows = de.reg.rows[:1] // cuda:0
	de.reg.det = devices.Detection{At: time.Now(), CUDAName: nvidiaName, Python: missing,
		OVErr: &model.ErrorInfo{Code: devices.CodeOVPythonMissing, Params: map[string]any{"path": missing}}}
	det := de.reg.det
	de.reg.detectDet = &det
	de.reg.mu.Unlock()
	cs, _ := diagNow(t, de.env, false)
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "faster-whisper", "pypdf", "gpu", "cudnn", "cuda", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	if got := checkNames(cs); !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
	// An OpenVINO environment (the asr-openvino component or .venv-ov).
	python := filepath.Join(t.TempDir(), "python.exe")
	if err := os.WriteFile(python, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	de.reg.mu.Lock()
	de.reg.det.Python = python
	de.reg.mu.Unlock()
	if got := checkNames(de.srv.diag.snapshot()); !slices.Contains(got, "openvino_venv") || !slices.Contains(got, "npu_driver") {
		t.Fatalf("with an OpenVINO environment: %v", got)
	}
}

// Windows desktop mode without components: no .venv under Resources is
// run; the rows say which component is missing and point to the
// Components page. The rows themselves are the same as in web mode.
func TestDiagnosticsWindowsDesktopWithoutComponents(t *testing.T) {
	log := &cmdLog{}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.runCmd = log.run
		s.desktop = &DesktopOptions{Token: "t", Port: 1}
	}})
	cs, by := diagNow(t, de.env, true)
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "faster-whisper", "pypdf", "gpu", "cudnn", "cuda",
		"openvino_venv", "openvino", "openvino_devices", "npu_driver", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	if got := checkNames(cs); !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
	wantCheck(t, by["python"], codePythonNotInstalled, codeHintInstallPython)
	wantCheck(t, by["faster-whisper"], codeFWNotInstalled, codeHintInstallFW)
	wantCheck(t, by["pypdf"], codePythonNotInstalled, codeHintInstallPython)
	wantCheck(t, by["cuda"], codeFWNotInstalled, codeHintInstallFW)
	wantCheck(t, by[checkCUDNN], codeFWNotInstalled, codeHintInstallFW)
	wantCheck(t, by["openvino_venv"], codeOpenVINONotInstalled, codeHintInstallOpenVINO)
	for _, n := range []string{"python", "faster-whisper", "pypdf", "cuda", checkCUDNN} {
		if by[n].Status != statusError { // cuda:0 is enabled: it is what transcribes
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	for _, r := range log.list() {
		if strings.Contains(r, "python") {
			t.Errorf("ran a python that is not installed: %s", r)
		}
	}
	// Only the Intel devices transcribe (cuda:0 off): a missing asr-cuda
	// is a warning.
	de.reg.mu.Lock()
	de.reg.rows[0].Enabled = false
	de.reg.mu.Unlock()
	_, by = diagNow(t, de.env, false)
	if c := by["faster-whisper"]; c.Status != statusWarn {
		t.Errorf("faster-whisper with the Intel devices only: %+v", c)
	}
}

// Windows desktop mode with the components: their interpreters run; the
// python check falls back to the python component without asr-cuda.
func TestDiagnosticsWindowsDesktopComponents(t *testing.T) {
	log := &cmdLog{}
	loc := &mapLocator{py: map[string]string{config.ComponentBase: compBasePython}}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.runCmd = log.run
		s.desktop = &DesktopOptions{Token: "t", Port: 1}
		s.cfg.Components = loc
	}})
	_, by := diagNow(t, de.env, false)
	if by["python"].Status != statusOK || by["pypdf"].Status != statusOK {
		t.Fatalf("python %+v pypdf %+v", by["python"], by["pypdf"])
	}
	wantCheck(t, by["faster-whisper"], codeFWNotInstalled, codeHintInstallFW)
	if runs := strings.Join(log.list(), "\n"); !strings.Contains(runs, compBasePython+" --version") || !strings.Contains(runs, compBasePython+" -I -c import pypdf") {
		t.Fatalf("runs:\n%s", runs)
	}
}

// mapLocator is a component manager with the interpreters of py and the
// MLX models of models ("model/precision" → directory).
type mapLocator struct {
	mu     sync.Mutex
	py     map[string]string
	models map[string]string
}

func (l *mapLocator) Python(backend string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.py[backend]
	return p, ok
}

func (l *mapLocator) ModelDir(backend, name, precision string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if backend != model.BackendMLX {
		return "", false
	}
	d, ok := l.models[name+"/"+precision]
	return d, ok
}

// install adds a component's interpreter (or a model directory).
func (l *mapLocator) install(backend, python string, model, dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if python != "" {
		l.py[backend] = python
	}
	if model != "" {
		l.models[model] = dir
	}
}

// macDevices makes the fake registry an Apple Silicon Mac: no NVIDIA, no
// OpenVINO; mlx:gpu when mlx, colab:remote when colab.
func macDevices(de *devEnv, mlx, colab bool) {
	de.reg.mu.Lock()
	defer de.reg.mu.Unlock()
	at := "2026-10-26T08:00:00Z"
	de.reg.rows = nil
	de.reg.present = map[string]bool{}
	de.reg.det = devices.Detection{At: time.Now(), CUDAName: "", Python: filepath.Join(os.TempDir(), "lf-none", ".venv-ov", "bin", "python"),
		OVErr: &model.ErrorInfo{Code: devices.CodeOVPythonMissing, Params: map[string]any{"path": ".venv-ov/bin/python"}}}
	if mlx {
		de.reg.rows = append(de.reg.rows, model.ASRDevice{ID: model.DeviceMLXGPU, Backend: model.BackendMLX, Label: "Apple M6", Enabled: true,
			Model: "large-v3-turbo", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 250, DetectedAt: &at})
		de.reg.present[model.DeviceMLXGPU] = true
		de.reg.det.MLX = "Apple M6"
	}
	if colab {
		de.reg.rows = append(de.reg.rows, model.ASRDevice{ID: model.DeviceColab, Backend: model.BackendColab, Label: "Google Colab", Enabled: true,
			Model: "large-v3", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 5, DetectedAt: &at})
		de.reg.present[model.DeviceColab] = true
	}
	det := de.reg.det
	de.reg.detectDet = &det
}

// ext-step1: plus the MLX self-test after the model
var macChecks = []string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "mlx_python", "mlx", "mlx_model", "mlx_selftest", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}

// macOS, desktop mode, nothing installed (the friend's Mac mini): no
// NVIDIA, CUDA, OpenVINO or .venv rows; MLX rows that say what to install;
// no process for a missing interpreter.
func TestDiagnosticsMacDesktopNothingInstalled(t *testing.T) {
	pinGOOS(t, "darwin")
	log := &cmdLog{}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.runCmd = log.run
		s.desktop = &DesktopOptions{Token: "t", Port: 1}
	}})
	macDevices(de, false, false)
	cs, by := diagNow(t, de.env, true)
	if got := checkNames(cs); !slices.Equal(got, macChecks) {
		t.Fatalf("checks %v, want %v", got, macChecks)
	}
	wantCheck(t, by["python"], codePythonNotInstalled, codeHintInstallPython)
	wantCheck(t, by["pypdf"], codePythonNotInstalled, codeHintInstallPython)
	wantCheck(t, by[checkMLXPython], codeMLXNotInstalled, codeHintInstallMLX)
	wantCheck(t, by[checkMLX], "check.api_skipped", "")
	wantCheck(t, by[checkMLXModel], codeMLXModelMissing, codeHintInstallMLXModel)
	if p := by[checkMLXModel].DetailInfo.Params; p["model"] != "large-v3-turbo" || p["precision"] != model.PrecisionFP16 {
		t.Errorf("model params %v", p)
	}
	for _, n := range []string{"python", "pypdf", checkMLXPython, checkMLXModel} {
		if by[n].Status != statusError { // nothing can transcribe
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	if by[checkMLX].Status != statusWarn {
		t.Errorf("mlx: %+v", by[checkMLX])
	}
	for _, r := range log.list() {
		if !strings.HasSuffix(r, " -version") {
			t.Errorf("ran %q; want ffmpeg -version only", r)
		}
	}
	// With Colab authorised the missing MLX is a warning (Colab transcribes).
	macDevices(de, false, true)
	_, by = diagNow(t, de.env, false)
	for _, n := range []string{checkMLXPython, checkMLXModel} {
		if by[n].Status != statusWarn {
			t.Errorf("%s with Colab: %+v", n, by[n])
		}
	}
}

// macOS, desktop mode, python, asr-mlx and the model installed: every
// check runs the component's interpreter and passes.
func TestDiagnosticsMacDesktopMLX(t *testing.T) {
	pinGOOS(t, "darwin")
	const basePy = "/Users/u/Library/Application Support/LectureFlow/components/python/3.12.10/venv/bin/python"
	const mlxPy = "/Users/u/Library/Application Support/LectureFlow/components/asr-mlx/1.0.0/venv/bin/python"
	modelDir := t.TempDir()
	for _, f := range []string{"config.json", "weights.safetensors"} {
		if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := &cmdLog{}
	loc := &mapLocator{py: map[string]string{config.ComponentBase: basePy, model.BackendMLX: mlxPy},
		models: map[string]string{"large-v3-turbo/fp16": modelDir}}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.runCmd = log.run
		s.desktop = &DesktopOptions{Token: "t", Port: 1}
		s.cfg.Components = loc
	}})
	macDevices(de, true, false)
	cs, by := diagNow(t, de.env, true)
	if got := checkNames(cs); !slices.Equal(got, macChecks) {
		t.Fatalf("checks %v, want %v", got, macChecks)
	}
	for _, n := range []string{"python", "pypdf", checkMLXPython, checkMLX, checkMLXModel} {
		if by[n].Status != statusOK {
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	wantCheck(t, by[checkMLXModel], codeMLXModelOK, "")
	runs := strings.Join(log.list(), "\n")
	for _, w := range []string{basePy + " --version", basePy + " -I -c import pypdf", mlxPy + " --version", mlxPy + " -I -c " + mlxImport} {
		if !strings.Contains(runs, w) {
			t.Errorf("did not run %q; runs:\n%s", w, runs)
		}
	}
	// The device's own model: small is not installed.
	de.reg.mu.Lock()
	de.reg.rows[0].Model = "small"
	de.reg.mu.Unlock()
	_, by = diagNow(t, de.env, false)
	wantCheck(t, by[checkMLXModel], codeMLXModelMissing, codeHintInstallMLXModel)
	if p := by[checkMLXModel].HintInfo.Params; p["model"] != "small" || p["precision"] != model.PrecisionFP16 || by[checkMLXModel].Status != statusError {
		t.Errorf("small: %+v", by[checkMLXModel])
	}
}

// macOS in web mode (development): the configured interpreters run
// (.venv, .venv-mlx) with the developer's hints; a model placed in
// {data_dir}/models/mlx passes.
func TestDiagnosticsMacWeb(t *testing.T) {
	pinGOOS(t, "darwin")
	log := &cmdLog{}
	de := newDevEnv(t, envConfig{server: func(s *Server) { s.runCmd = log.run }})
	macDevices(de, true, false)
	var sys System
	de.call("POST", "/api/system/recheck", nil, 200, &sys)
	if got := checkNames(sys.Checks); !slices.Equal(got, macChecks) {
		t.Fatalf("checks %v, want %v", got, macChecks)
	}
	if sys.OpenVINO != nil || len(sys.Devices) != 1 || sys.Devices[0].ID != model.DeviceMLXGPU {
		t.Fatalf("openvino %+v devices %+v (no OpenVINO block on a Mac)", sys.OpenVINO, sys.Devices)
	}
	by := checksByName(sys)
	dir := devices.MLXModelDir(de.srv.absDataDir(), "large-v3-turbo")
	wantCheck(t, by[checkMLXModel], codeMLXModelMissing, codeHintMLXModelDev)
	if p := by[checkMLXModel].HintInfo.Params; p["path"] != dir {
		t.Errorf("hint path %v, want %s", p["path"], dir)
	}
	runs := strings.Join(log.list(), "\n")
	if !strings.Contains(runs, de.cfg.Python+" --version") || !strings.Contains(runs, de.cfg.PythonMLX+" -I -c "+mlxImport) || strings.Contains(runs, "--selftest") {
		t.Errorf("runs:\n%s", runs)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.json", "weights.npz"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var sys2 System
	de.call("GET", "/api/system", nil, 200, &sys2)
	wantCheck(t, checksByName(sys2)[checkMLXModel], codeMLXModelOK, "")

	// A failing import: the developer's hint.
	de.srv.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
		if len(args) == 3 && args[2] == mlxImport {
			return "", "ModuleNotFoundError: No module named 'mlx'", errUnknownCmd
		}
		return fakeCmd(ctx, name, args...)
	}
	_, by = diagNow(t, de.env, false)
	wantCheck(t, by[checkMLX], "check.import_failed", codeHintMLXVenv)
	if by[checkMLX].Status != statusError || by[checkMLX].DetailInfo.Params["error"] != "ModuleNotFoundError: No module named 'mlx'" {
		t.Errorf("mlx import failed: %+v", by[checkMLX])
	}
}

// The checks run again when the installed components change (the setup
// guide installs python, asr-mlx and the model after the service started):
// the rows follow without "重新检查"; several events make one run.
func TestDiagnosticsRerunAfterComponentChange(t *testing.T) {
	pinGOOS(t, "darwin")
	old := rerunDelay
	rerunDelay = 20 * time.Millisecond
	t.Cleanup(func() { rerunDelay = old })
	const basePy = "/Users/u/Library/Application Support/LectureFlow/components/python/1.0.0/venv/bin/python"
	const mlxPy = "/Users/u/Library/Application Support/LectureFlow/components/asr-mlx/1.0.0/venv/bin/python"
	modelDir := t.TempDir()
	for _, f := range []string{"config.json", "weights.safetensors"} {
		if err := os.WriteFile(filepath.Join(modelDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := &cmdLog{}
	loc := &mapLocator{py: map[string]string{}, models: map[string]string{}}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.runCmd = log.run
		s.desktop = &DesktopOptions{Token: "t", Port: 1}
		s.cfg.Components = loc
	}})
	macDevices(de, false, false)
	de.srv.diag.waitStartup()
	if c := checksByName(System{Checks: de.srv.diag.snapshot()})["pypdf"]; c.DetailInfo == nil || c.DetailInfo.Code != codePythonNotInstalled {
		t.Fatalf("pypdf before the install: %+v", c)
	}
	loc.install(config.ComponentBase, basePy, "", "")
	de.srv.diag.componentsChanged()
	loc.install(model.BackendMLX, mlxPy, "large-v3-turbo/fp16", modelDir)
	de.srv.diag.componentsChanged()
	macDevices(de, true, false)
	deadline := time.Now().Add(5 * time.Second)
	var by map[string]SystemCheck
	for time.Now().Before(deadline) {
		by = checksByName(System{Checks: de.srv.diag.snapshot()})
		if by[checkMLX].Status == statusOK && by["pypdf"].Status == statusOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, n := range []string{"python", "pypdf", checkMLXPython, checkMLX, checkMLXModel} {
		if by[n].Status != statusOK {
			t.Errorf("%s after the install: %+v", n, by[n])
		}
	}
	time.Sleep(100 * time.Millisecond)
	n := 0
	for _, r := range log.list() {
		if strings.HasSuffix(r, " -version") {
			n++ // ffmpeg: once per run
		}
	}
	if n != 2 {
		t.Errorf("%d runs (startup + one rerun expected)", n)
	}
	// Stopped (shutdown): no run any more.
	before := len(log.list())
	de.srv.diag.stop()
	de.srv.diag.componentsChanged()
	time.Sleep(100 * time.Millisecond)
	if after := log.list(); len(after) != before {
		t.Errorf("ran after stop: %v", after[before:])
	}
}
