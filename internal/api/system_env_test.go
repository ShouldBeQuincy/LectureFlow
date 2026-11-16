// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// ext-step1 (SPEC-ext "主程序配套改动 1", .claude/ext-brief.md §2): the
// diagnostics by hardware and transcription mode — NVIDIA, Apple silicon,
// Intel / CPU only and only Colab — and the endpoints behind them
// (GET/PUT /api/asr-mode, kv hardware.last).

func init() {
	// The startup hardware detection runs the system's own tools
	// (PowerShell, nvidia-smi); the tests of this package never do.
	startupHardwareDetect = false
}

// gpuQueries counts the scheduler's nvidia-smi readings (sched.GPUQuery).
type gpuQueries struct{ n atomic.Int64 }

func (g *gpuQueries) opt(o *sched.Options) {
	o.GPUQuery = func(context.Context) (sched.GPUMem, error) {
		g.n.Add(1)
		return sched.GPUMem{FreeMB: 9000, TotalMB: 12000, UsedMB: 3000}, nil
	}
}

// envCmd is fakeCmd whose self-tests answer for the device they were
// given (--device …; the MLX worker has none: mlx:gpu).
func envCmd(ctx context.Context, name string, args ...string) (string, string, error) {
	if isScriptRun(args, "--selftest") {
		device := "mlx:gpu"
		for i, a := range args {
			if a == "--device" && i+1 < len(args) {
				device = args[i+1]
			}
		}
		return "SELFTEST ok device=" + device + " load_s=0.4 transcribe_s=0.20 segments=0\n", "", nil
	}
	return fakeCmd(ctx, name, args...)
}

// envLog records the programs the checks run, answering like envCmd.
type envLog struct{ cmdLog }

func (l *envLog) run(ctx context.Context, name string, args ...string) (string, string, error) {
	l.mu.Lock()
	l.runs = append(l.runs, strings.Join(append([]string{name}, args...), " "))
	l.mu.Unlock()
	return envCmd(ctx, name, args...)
}

// noNVIDIAWords fails when v (as the page receives it) names CUDA, cuDNN
// or NVIDIA.
func noNVIDIAWords(t *testing.T, what string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(string(b))
	for _, w := range []string{"cuda", "cudnn", "nvidia"} {
		if i := strings.Index(low, w); i >= 0 {
			from, to := max(0, i-80), min(len(b), i+80)
			t.Errorf("%s names %q: …%s…", what, w, b[from:to])
		}
	}
}

// noNVIDIARuns fails when a check ran nvidia-smi or a CUDA self-test.
func noNVIDIARuns(t *testing.T, log *envLog, gq *gpuQueries) {
	t.Helper()
	for _, r := range log.list() {
		if strings.Contains(r, "nvidia-smi") || strings.Contains(r, "--device cuda") || strings.Contains(r, "--check-cudnn") {
			t.Errorf("ran %q without an NVIDIA GPU", r)
		}
	}
	if n := gq.n.Load(); n != 0 {
		t.Errorf("read nvidia-smi %d times without an NVIDIA GPU", n)
	}
}

// systemNow is GET /api/system after the checks ran (withSelftest: as
// "重新检查"), without HTTP (desktop mode would want the token).
func systemNow(t *testing.T, de *devEnv, withSelftest bool) System {
	t.Helper()
	de.srv.diag.waitStartup()
	de.srv.diag.run(context.Background(), withSelftest)
	return de.srv.systemView(context.Background())
}

// writeFiles creates non-empty files in dir.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range names {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- NVIDIA ----

// The reference laptop (NVIDIA + Intel GPU and NPU): faster-whisper, GPU,
// cuDNN and the CUDA self-test, then the OpenVINO rows; no MLX, CPU or
// Colab row. "重新检查" runs the CUDA self-test and reads nvidia-smi.
func TestExtDiagNVIDIA(t *testing.T) {
	log, gq := &envLog{}, &gpuQueries{}
	de := newDevEnv(t, envConfig{sched: gq.opt, server: func(s *Server) { s.runCmd = log.run }})
	sys := systemNow(t, de, true)
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "faster-whisper", "pypdf", "gpu", "cudnn", "cuda",
		"openvino_venv", "openvino", "openvino_devices", "npu_driver", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	if got := checkNames(sys.Checks); !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
	by := checksByName(sys)
	wantCheck(t, by[checkCUDNN], codeCUDNNOK, "")
	if v := by[checkCUDNN].DetailInfo.Params["version"]; v != "9.1.0" || by[checkCUDNN].Status != statusOK {
		t.Errorf("cudnn %+v", by[checkCUDNN])
	}
	if c := by[checkCUDA]; c.Status != statusOK || !strings.HasPrefix(c.Detail, "SELFTEST ok") {
		t.Errorf("cuda %+v", c)
	}
	runs := strings.Join(log.list(), "\n")
	asr := " -s " + filepath.Join(de.cfg.WorkerDir, "asr.py")
	for _, w := range []string{asr + " --check-cudnn", asr + " --selftest --device cuda"} {
		if !strings.Contains(runs, w) {
			t.Errorf("did not run %q; runs:\n%s", w, runs)
		}
	}
	if gq.n.Load() == 0 || sys.GPU.TotalMB != 12000 {
		t.Errorf("nvidia-smi not read: %d, gpu %+v", gq.n.Load(), sys.GPU)
	}
	cfg, _ := sys.Config.(map[string]any)
	if _, ok := cfg["nvidia_smi"]; !ok {
		t.Errorf("no nvidia_smi with an NVIDIA GPU: %v", cfg)
	}
	if len(sys.Devices) != 4 || sys.OpenVINO == nil || sys.Power == nil {
		t.Errorf("devices %d openvino %v power %v", len(sys.Devices), sys.OpenVINO, sys.Power)
	}

	// cuDNN does not load: rated like CUDA (cuda:0 is enabled: error), with
	// the worker's reason without the "ERROR: " prefix.
	de.srv.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
		if len(args) == 3 && isScriptRun(args, "--check-cudnn") {
			return "", "Traceback ...\nERROR: cuDNN not found: cudnn64_9.dll", errUnknownCmd
		}
		return fakeCmd(ctx, name, args...)
	}
	sys = systemNow(t, de, false)
	c := checksByName(sys)[checkCUDNN]
	wantCheck(t, c, codeCUDNNFailed, codeHintCUDNN)
	if c.Status != statusError || c.DetailInfo.Params["error"] != "cuDNN not found: cudnn64_9.dll" {
		t.Errorf("cudnn failed %+v", c)
	}
	// The CUDA self-test keeps its last result without "重新检查".
	if c := checksByName(sys)[checkCUDA]; !strings.HasPrefix(c.Detail, "SELFTEST ok") {
		t.Errorf("cuda after a run without self-tests: %+v", c)
	}
}

// ---- Apple silicon ----

// A Mac with MLX and its model: the MLX rows and the MLX self-test
// (asr_mlx.py --selftest --model DIR); nothing about NVIDIA anywhere, no
// nvidia-smi, no CUDA self-test.
func TestExtDiagAppleSilicon(t *testing.T) {
	pinGOOS(t, "darwin")
	const basePy = "/Users/u/Library/Application Support/LectureFlow/components/python/3.12.10/venv/bin/python"
	const mlxPy = "/Users/u/Library/Application Support/LectureFlow/components/asr-mlx/1.0.0/venv/bin/python"
	modelDir := t.TempDir()
	writeFiles(t, modelDir, "config.json", "weights.safetensors")
	log, gq := &envLog{}, &gpuQueries{}
	loc := &mapLocator{py: map[string]string{config.ComponentBase: basePy, model.BackendMLX: mlxPy},
		models: map[string]string{"large-v3-turbo/fp16": modelDir}}
	de := newDevEnv(t, envConfig{sched: gq.opt, server: func(s *Server) {
		s.runCmd = log.run
		desktopInterpreters(s)
		s.cfg.Components = loc
	}})
	macDevices(de, true, false)
	sys := systemNow(t, de, true)
	if got := checkNames(sys.Checks); !slices.Equal(got, macChecks) {
		t.Fatalf("checks %v, want %v", got, macChecks)
	}
	by := checksByName(sys)
	if c := by[checkMLXSelftest]; c.Status != statusOK || !strings.HasPrefix(c.Detail, "SELFTEST ok") {
		t.Errorf("mlx self-test %+v", c)
	}
	if runs := strings.Join(log.list(), "\n"); !strings.Contains(runs, mlxPy+" -s "+filepath.Join(de.cfg.WorkerDir, "asr_mlx.py")+" --selftest --model "+modelDir) {
		t.Errorf("the MLX self-test did not run; runs:\n%s", runs)
	}
	noNVIDIARuns(t, log, gq)
	noNVIDIAWords(t, "checks", sys.Checks)
	noNVIDIAWords(t, "configuration", sys.Config)
	noNVIDIAWords(t, "devices", sys.Devices)
	if sys.OpenVINO != nil {
		t.Errorf("an OpenVINO block on a Mac: %+v", sys.OpenVINO)
	}

	// The model is missing: the self-test does not run (mlx_model says what
	// to install).
	loc.mu.Lock()
	loc.models = map[string]string{}
	loc.mu.Unlock()
	before := len(log.list())
	sys = systemNow(t, de, true)
	c := checksByName(sys)[checkMLXSelftest]
	wantCheck(t, c, codeSelftestNoModel, "")
	if c.Status != statusWarn || c.DetailInfo.Params["model"] != "large-v3-turbo" {
		t.Errorf("no model: %+v", c)
	}
	for _, r := range log.list()[before:] {
		if strings.Contains(r, "--selftest") {
			t.Errorf("ran %q without the model", r)
		}
	}
	// The self-test fails: the worker's reason, the component hint.
	loc.install(model.BackendMLX, "", "large-v3-turbo/fp16", modelDir)
	de.srv.runCmd = func(ctx context.Context, name string, args ...string) (string, string, error) {
		if isScriptRun(args, "--selftest") {
			return "ERROR_CODE device_unavailable\n", "ERROR: MLX reports no Metal GPU", errUnknownCmd
		}
		return fakeCmd(ctx, name, args...)
	}
	c = checksByName(systemNow(t, de, true))[checkMLXSelftest]
	wantCheck(t, c, "check.selftest_failed", codeHintInstallMLX)
	if c.Status != statusError || c.DetailInfo.Params["error"] != "MLX reports no Metal GPU" {
		t.Errorf("failed self-test %+v", c)
	}
}

// ---- Intel / CPU only ----

// intelDevices makes the fake registry an Intel laptop without NVIDIA: the
// Intel GPU, NPU and CPU, fw:cpu (Detect's fallback row) and the cuda:0
// placeholder an old setting migrated (not present, "NVIDIA GPU").
func intelDevices(reg *fakeDevReg) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.rows = slices.DeleteFunc(reg.rows, func(d model.ASRDevice) bool { return d.ID == model.DeviceCUDA0 })
	reg.rows = append(reg.rows,
		model.ASRDevice{ID: model.DeviceFWCPU, Backend: model.BackendFasterWhisper, Label: devices.LabelFWCPU, Enabled: false,
			Model: "small", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 10},
		model.ASRDevice{ID: model.DeviceCUDA0, Backend: model.BackendFasterWhisper, Label: devices.LabelCUDAUnknown, Enabled: true,
			Model: "large-v3", Precision: model.PrecisionAuto, MaxParallel: 1, Priority: 300})
	reg.present = map[string]bool{model.DeviceOVGPU: true, model.DeviceOVNPU: true, model.DeviceOVCPU: true, model.DeviceFWCPU: true}
	reg.det.CUDAName = ""
	reg.det.OV.Ignored = []devices.OVDevice{}
	det := reg.det
	reg.detectDet = &det
}

// An Intel laptop, desktop mode, asr-openvino installed with a converted
// model: the OpenVINO rows and the CPU self-test on ov:CPU; no GPU, cuDNN,
// CUDA or faster-whisper row, no CUDA device (not even the migrated cuda:0
// placeholder), nothing about NVIDIA; neither nvidia-smi nor a CUDA
// self-test runs. Then fw:cpu (only asr-cuda) and nothing installed.
func TestExtDiagIntelCPU(t *testing.T) {
	const ovPy = `C:\Users\u\AppData\Local\LectureFlow\components\asr-openvino\2026.4-1\venv\Scripts\python.exe`
	log, gq := &envLog{}, &gpuQueries{}
	loc := &mapLocator{py: map[string]string{config.ComponentBase: compBasePython, model.BackendOpenVINO: ovPy}, models: map[string]string{}}
	de := newDevEnv(t, envConfig{sched: gq.opt, server: func(s *Server) {
		s.runCmd = log.run
		desktopInterpreters(s)
		s.cfg.Components = loc
		intelDevices(s.dev.(*fakeDevReg))
	}})
	ovModel := devices.ModelDir(de.srv.absDataDir(), "large-v3-turbo", "int8") // ov:CPU's model
	writeFiles(t, ovModel, devices.ModelFiles...)
	sys := systemNow(t, de, true)
	want := []string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "openvino_venv", "openvino", "openvino_devices", "npu_driver",
		"cpu_selftest", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}
	if got := checkNames(sys.Checks); !slices.Equal(got, want) {
		t.Fatalf("checks %v, want %v", got, want)
	}
	by := checksByName(sys)
	if c := by[checkCPUSelftest]; c.Status != statusOK || !strings.HasPrefix(c.Detail, "SELFTEST ok") {
		t.Errorf("cpu self-test %+v", c)
	}
	runs := strings.Join(log.list(), "\n")
	if w := ovPy + " -s " + filepath.Join(de.cfg.WorkerDir, "asr_openvino.py") + " --selftest --device CPU --model " + ovModel; !strings.Contains(runs, w) {
		t.Errorf("did not run %q; runs:\n%s", w, runs)
	}
	noNVIDIARuns(t, log, gq)
	noNVIDIAWords(t, "checks", sys.Checks)
	noNVIDIAWords(t, "configuration", sys.Config)
	noNVIDIAWords(t, "devices", sys.Devices)
	noNVIDIAWords(t, "openvino", sys.OpenVINO)
	if sys.GPU.TotalMB != 0 || len(sys.Devices) != 4 || sys.OpenVINO == nil {
		t.Errorf("gpu %+v devices %+v openvino %v", sys.GPU, sys.Devices, sys.OpenVINO)
	}
	for _, d := range sys.Devices {
		if d.ID == model.DeviceCUDA0 {
			t.Errorf("the CUDA placeholder row in the devices: %+v", d)
		}
	}
	// the configuration: no faster-whisper interpreter (not installed), no
	// CUDA device, no NVIDIA setting
	cfg, _ := sys.Config.(map[string]any)
	wantPy := []ConfigPython{
		{Backend: model.BackendOpenVINO, Path: ovPy, Source: config.PythonFromComponent, Exists: true},
		{Backend: config.ComponentBase, Path: compBasePython, Source: config.PythonFromComponent, Exists: true},
	}
	if got := pythonRows(t, cfg); !slices.Equal(got, wantPy) {
		t.Errorf("python %+v, want %+v", got, wantPy)
	}
	if devs := deviceRows(t, cfg); len(devs) != 4 {
		t.Errorf("asr_devices %+v", devs)
	}

	// Only asr-cuda (fw:cpu): the self-test runs faster-whisper on the CPU;
	// the configuration lists its interpreter (it is there), still without
	// a word about NVIDIA.
	loc.mu.Lock()
	loc.py = map[string]string{config.ComponentBase: compBasePython, model.BackendFasterWhisper: compFWPython}
	loc.mu.Unlock()
	before := len(log.list())
	sys = systemNow(t, de, true)
	if c := checksByName(sys)[checkCPUSelftest]; c.Status != statusOK {
		t.Errorf("fw:cpu self-test %+v", c)
	}
	if runs := strings.Join(log.list()[before:], "\n"); !strings.Contains(runs, compFWPython+" -s "+filepath.Join(de.cfg.WorkerDir, "asr.py")+" --selftest --device cpu --compute-type int8") {
		t.Errorf("fw:cpu self-test did not run; runs:\n%s", runs)
	}
	if rows := pythonRows(t, sys.Config.(map[string]any)); len(rows) != 3 || rows[0].Backend != model.BackendFasterWhisper || rows[1].Source != config.PythonNotInstalled {
		t.Errorf("python with asr-cuda %+v", rows)
	}
	noNVIDIAWords(t, "checks (fw:cpu)", sys.Checks)
	noNVIDIARuns(t, log, gq)

	// Nothing installed: a warning that points to the recommended install;
	// no interpreter runs.
	loc.mu.Lock()
	loc.py = map[string]string{config.ComponentBase: compBasePython}
	loc.mu.Unlock()
	before = len(log.list())
	sys = systemNow(t, de, true)
	c := checksByName(sys)[checkCPUSelftest]
	wantCheck(t, c, codeSelftestNotInstalled, codeHintInstallLocal)
	if c.Status != statusWarn {
		t.Errorf("nothing installed %+v", c)
	}
	for _, r := range log.list()[before:] {
		if strings.Contains(r, "--selftest") {
			t.Errorf("ran %q with nothing installed", r)
		}
	}
	noNVIDIAWords(t, "checks (nothing installed)", sys.Checks)
}

// The same laptop in web mode (development) without .venv-ov: the
// OpenVINO environment row does not mention CUDA; the CPU self-test points
// to the development environments.
func TestExtDiagIntelWeb(t *testing.T) {
	log, gq := &envLog{}, &gpuQueries{}
	de := newDevEnv(t, envConfig{sched: gq.opt, server: func(s *Server) {
		s.runCmd = log.run
		intelDevices(s.dev.(*fakeDevReg))
		r := s.dev.(*fakeDevReg)
		missing := filepath.Join(os.TempDir(), "lf-none", ".venv-ov", "Scripts", "python.exe")
		r.det.Python, r.det.OV = missing, nil
		r.det.OVErr = &model.ErrorInfo{Code: devices.CodeOVPythonMissing, Params: map[string]any{"path": missing}}
	}})
	sys := systemNow(t, de, true)
	by := checksByName(sys)
	wantCheck(t, by["openvino_venv"], codeOpenVINOVenvAbsent, "check.hint_openvino_venv")
	wantCheck(t, by[checkCPUSelftest], codeSelftestNotInstalled, codeHintLocalDev)
	noNVIDIAWords(t, "checks", sys.Checks)
	noNVIDIAWords(t, "configuration", sys.Config)
	noNVIDIARuns(t, log, gq)
}

// ---- only Colab ----

// colabTestEnv is a desktop-mode laptop with an NVIDIA GPU and a Colab
// backend against a fake Google (the credential imported or not).
type colabTestEnv struct {
	*devEnv
	b   *colab.Backend
	log *envLog
	gq  *gpuQueries
	// reach counts the reachability calls of this env's server; reachFn
	// is what they answer (nil: reachable; setReach).
	reach   atomic.Int64
	reachMu sync.Mutex
	reachFn func() (bool, error)
}

// setReach changes what the env's reachability call answers (the server's
// diagnostics may run in the background, hence the lock).
func (ce *colabTestEnv) setReach(fn func() (supported bool, err error)) {
	ce.reachMu.Lock()
	ce.reachFn = fn
	ce.reachMu.Unlock()
}

func newColabTestEnv(t *testing.T, authorised bool) *colabTestEnv {
	t.Helper()
	ce := &colabTestEnv{log: &envLog{}, gq: &gpuQueries{}}
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+colabTestAccess+`","expires_in":3599,"token_type":"Bearer",`+
			`"scope":"openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/colaboratory"}`)
	}))
	t.Cleanup(google.Close)
	adc := filepath.Join(t.TempDir(), "application_default_credentials.json")
	raw, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "client-id.apps.example",
		"client_secret": colabTestClient, "refresh_token": colabTestRefresh})
	if err := os.WriteFile(adc, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prevGcloud := gcloudLookup
	gcloudLookup = func(*Server) (string, bool) {
		return `C:\LectureFlow\components\gcloud\1.0.0\tool\bin\gcloud.cmd`, true
	}
	t.Cleanup(func() { gcloudLookup = prevGcloud })
	ce.devEnv = newDevEnv(t, envConfig{
		sched: func(o *sched.Options) {
			ce.gq.opt(o)
			b, err := colab.New(colab.Options{KV: o.Store, TokenURL: google.URL + "/token", RevokeURL: google.URL + "/revoke",
				ColabURL: google.URL + "/colab", ADCPath: adc, AllowHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Startup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if authorised {
				if _, err := b.ImportADC(context.Background(), colab.Lifecycle{Kind: colab.LifecycleManual}); err != nil {
					t.Fatal(err)
				}
			}
			ce.b = b
		},
		server: func(s *Server) {
			s.colab = ce.b
			s.runCmd = ce.log.run
			s.desktop = &DesktopOptions{Token: "t", Port: 1} // the desktop checks and hints
			// this server's own fake: the checks of another env's server
			// (a rerun landing late) never count here
			s.colabReach = func(context.Context) (bool, error) {
				ce.reach.Add(1)
				ce.reachMu.Lock()
				fn := ce.reachFn
				ce.reachMu.Unlock()
				if fn != nil {
					return fn()
				}
				return true, nil
			}
		},
	})
	ce.srv.desktop = nil // HTTP without the token; the checks stay the desktop mode's
	ce.srv.diag.waitStartup()
	return ce
}

var remoteChecks = []string{"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "gcloud", "colab_auth", "colab_reach", "browser_ext", "api_key:deepseek", "api:deepseek", "disk"}

// Only Colab on a laptop that has an NVIDIA GPU: none of the local
// inference rows, no device, power or OpenVINO block, no transcription
// interpreter in the configuration, no NVIDIA setting; gcloud, the
// authorisation and the reachability instead (the read-only call: no
// runtime). No nvidia-smi, no self-test, no device detection on recheck.
func TestExtDiagRemoteOnly(t *testing.T) {
	ce := newColabTestEnv(t, true)
	ce.call("PUT", "/api/asr-mode", map[string]any{"mode": "remote"}, http.StatusOK, nil)
	ce.gq.n.Store(0) // the startup checks ran in the local mode
	before := len(ce.log.list())
	ce.reg.mu.Lock()
	detects := ce.reg.detects
	ce.reg.mu.Unlock()
	ce.srv.diag.run(context.Background(), true) // as "重新检查"
	sys := ce.srv.systemView(context.Background())
	if got := checkNames(sys.Checks); !slices.Equal(got, remoteChecks) {
		t.Fatalf("checks %v, want %v", got, remoteChecks)
	}
	by := checksByName(sys)
	wantCheck(t, by[checkGcloud], codeGcloudOK, "")
	wantCheck(t, by[checkColabAuth], codeColabAuthOK, "")
	wantCheck(t, by[checkColabReach], codeColabReachOK, "")
	for _, n := range []string{checkGcloud, checkColabAuth, checkColabReach} {
		if by[n].Status != statusOK {
			t.Errorf("%s: %+v", n, by[n])
		}
	}
	if ce.reach.Load() == 0 {
		t.Error("the reachability call was not made")
	}
	if sys.Devices != nil || sys.Power != nil || sys.OpenVINO != nil || sys.GPU.TotalMB != 0 {
		t.Errorf("local blocks with only Colab: devices %v power %v openvino %v gpu %+v", sys.Devices, sys.Power, sys.OpenVINO, sys.GPU)
	}
	cfg, _ := sys.Config.(map[string]any)
	if rows := pythonRows(t, cfg); len(rows) != 1 || rows[0].Backend != config.ComponentBase {
		t.Errorf("python with only Colab %+v", rows)
	}
	for _, d := range deviceRows(t, cfg) {
		if d.Backend != model.BackendColab {
			t.Errorf("a local device in the configuration: %+v", d)
		}
	}
	noNVIDIAWords(t, "checks", sys.Checks)
	noNVIDIAWords(t, "configuration", sys.Config)
	for _, r := range ce.log.list()[before:] {
		if strings.Contains(r, "--selftest") || strings.Contains(r, "--check-cudnn") || strings.Contains(r, "nvidia-smi") ||
			strings.Contains(r, "import faster_whisper") {
			t.Errorf("ran %q with only Colab", r)
		}
	}
	if n := ce.gq.n.Load(); n != 0 {
		t.Errorf("read nvidia-smi %d times with only Colab", n)
	}
	// POST /api/system/recheck: no local device detection.
	ce.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	ce.reg.mu.Lock()
	if ce.reg.detects != detects {
		t.Errorf("recheck detected the local devices with only Colab (%d → %d)", detects, ce.reg.detects)
	}
	ce.reg.mu.Unlock()

	// Not authorised: only Colab transcribes, so an error; the
	// reachability is not checked.
	ce2 := newColabTestEnv(t, false)
	ce2.call("PUT", "/api/asr-mode", map[string]any{"mode": "remote"}, http.StatusOK, nil)
	ce2.srv.diag.run(context.Background(), true)
	by = checksByName(ce2.srv.systemView(context.Background()))
	wantCheck(t, by[checkColabAuth], codeColabAuthMissing, codeHintColabAuth)
	wantCheck(t, by[checkColabReach], codeColabReachSkipped, "")
	if by[checkColabAuth].Status != statusError || by[checkColabReach].Status != statusWarn || ce2.reach.Load() != 0 {
		t.Errorf("not authorised: %+v %+v (reach %d)", by[checkColabAuth], by[checkColabReach], ce2.reach.Load())
	}
	// The first server's checks running now (the envChanged rerun its
	// PUT /api/asr-mode scheduled can land this late) make its own
	// reachability call, never the second server's (flaky2: the hook was
	// a package variable that the second env had replaced).
	n := ce.reach.Load()
	ce.srv.diag.run(context.Background(), false)
	if ce.reach.Load() == n || ce2.reach.Load() != 0 {
		t.Errorf("the first server's checks: its reach %d → %d, the second's %d", n, ce.reach.Load(), ce2.reach.Load())
	}
}

// Local mode with Colab switched on: the local rows and the Colab rows; a
// missing authorisation is only a warning then. gcloud missing: a warning
// with the component hint; a failing reachability call: a warning.
func TestExtDiagLocalWithColab(t *testing.T) {
	ce := newColabTestEnv(t, true)
	if err := sched.WriteRemotePrefs(context.Background(), ce.st, sched.RemotePrefs{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	gcloudLookup = func(*Server) (string, bool) { return "", false }
	ce.setReach(func() (bool, error) { return true, errors.New("dial tcp: no route to host") })
	sys := systemNow(t, ce.devEnv, false)
	names := checkNames(sys.Checks)
	for _, n := range []string{"gpu", checkCUDNN, checkCUDA, checkGcloud, checkColabAuth, checkColabReach} {
		if !slices.Contains(names, n) {
			t.Errorf("no %s in %v", n, names)
		}
	}
	if i, j := slices.Index(names, checkCUDA), slices.Index(names, checkGcloud); i > j {
		t.Errorf("the Colab rows before the local ones: %v", names)
	}
	by := checksByName(sys)
	wantCheck(t, by[checkGcloud], codeGcloudMissing, codeHintInstallGcloud)
	wantCheck(t, by[checkColabReach], codeColabReachFailed, codeHintColabReach)
	if by[checkGcloud].Status != statusWarn || by[checkColabReach].Status != statusWarn {
		t.Errorf("gcloud %+v reach %+v", by[checkGcloud], by[checkColabReach])
	}
	// Without the reachability call in the backend: not checked.
	ce.setReach(func() (bool, error) { return false, nil })
	by = checksByName(systemNow(t, ce.devEnv, false))
	wantCheck(t, by[checkColabReach], "check.api_skipped", "")
}

// ---- GET / PUT /api/asr-mode ----

func TestASRModeEndpoints(t *testing.T) {
	de := newDevEnv(t, envConfig{})
	var v ASRModeView
	de.call("GET", "/api/asr-mode", nil, http.StatusOK, &v)
	if v != (ASRModeView{Mode: ASRModeLocal, Source: ASRModeFromDefault}) {
		t.Fatalf("default %+v", v)
	}
	// the guide of 3.1 saved the mode only in its state
	de.call("PUT", "/api/onboarding", map[string]any{"step": "storage", "done": false, "data": map[string]any{"asr_mode": "remote"}}, http.StatusOK, nil)
	de.call("GET", "/api/asr-mode", nil, http.StatusOK, &v)
	if v != (ASRModeView{Mode: ASRModeRemote, Source: ASRModeFromOnboarding}) {
		t.Fatalf("from the guide %+v", v)
	}
	for _, bad := range []any{map[string]any{"mode": "cloud"}, map[string]any{}, map[string]any{"mode": ""}} {
		if info := de.callErrInfo("PUT", "/api/asr-mode", bad, http.StatusBadRequest); info.Code != "request.bad_param" || info.Params["name"] != "mode" {
			t.Errorf("%v: %+v", bad, info)
		}
	}
	de.srv.diag.waitStartup()
	de.call("PUT", "/api/asr-mode", map[string]any{"mode": "local"}, http.StatusOK, &v)
	if v != (ASRModeView{Mode: ASRModeLocal, Source: ASRModeFromSettings}) {
		t.Fatalf("put %+v", v)
	}
	de.call("GET", "/api/asr-mode", nil, http.StatusOK, &v)
	if v.Source != ASRModeFromSettings || v.Mode != ASRModeLocal {
		t.Fatalf("after put %+v", v)
	}
	// Switching to Colab only: GET /api/system leaves the local rows out at
	// once; the Colab rows follow from the checks' next run.
	old := envRerunDelay
	envRerunDelay = 10 * time.Millisecond
	t.Cleanup(func() { envRerunDelay = old })
	prevGcloud := gcloudLookup
	gcloudLookup = func(*Server) (string, bool) { return "", false }
	t.Cleanup(func() { gcloudLookup = prevGcloud })
	de.call("PUT", "/api/asr-mode", map[string]any{"mode": "remote"}, http.StatusOK, &v)
	var sys System
	de.call("GET", "/api/system", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if !checkAllowed(c.Name, diagEnv{remote: true, cloud: true}) {
			t.Errorf("local row %s right after switching to Colab only", c.Name)
		}
	}
	// The rows of the new run are all there as soon as it starts, each one
	// check.checking until its check has answered: wait for the run to end
	// (under load colab_auth was read while still checking).
	settled := func(sys System) bool {
		if !slices.Equal(checkNames(sys.Checks), remoteChecks) {
			return false
		}
		for _, c := range sys.Checks {
			if c.DetailInfo != nil && c.DetailInfo.Code == checkPending {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sys = System{} // a fresh value: decoding into the old one would merge the params
		de.call("GET", "/api/system", nil, http.StatusOK, &sys)
		if settled(sys) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := checkNames(sys.Checks); !slices.Equal(got, remoteChecks) {
		t.Fatalf("checks after switching %v, want %v", got, remoteChecks)
	}
	by := checksByName(sys)
	wantCheck(t, by[checkColabAuth], codeColabOff, "") // no Colab backend in this test server
	if by[checkColabAuth].Status != statusError {
		t.Errorf("colab_auth without a backend, only Colab: %+v", by[checkColabAuth])
	}
}

// ---- kv hardware.last ----

// Every hardware detection is remembered (names and kinds only); it makes
// the NVIDIA rows appear without nvidia-smi answering, while a cuda:0 row
// alone no longer does.
func TestHardwareLastRemembered(t *testing.T) {
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.compState().detect = func(context.Context, string) (hwdetect.HW, error) {
			return hwdetect.HW{OS: "windows", CPU: "Intel(R) Core(TM) Ultra 9 386H", RAMMB: 32000,
				NVIDIA:   []hwdetect.GPU{{Name: "NVIDIA GeForce RTX 5070 Ti Laptop GPU", VRAMMB: 12227, Driver: "592.00", Source: "nvidia-smi"}},
				IntelGPU: true, IntelGPUName: "Intel(R) Arc(TM) 140T GPU", IntelNPU: true, IntelNPUName: "Intel(R) AI Boost"}, nil
		}
		r := s.dev.(*fakeDevReg)
		r.det.CUDAName = "" // nvidia-smi does not answer (a broken driver); cuda:0 still has its row
	}})
	if de.srv.hasNVIDIA(context.Background()) {
		t.Fatal("a cuda:0 row alone counted as an NVIDIA GPU")
	}
	de.call("GET", "/api/hardware", nil, http.StatusOK, nil)
	raw, ok, err := de.st.GetKV(context.Background(), kvHardwareLast)
	if err != nil || !ok {
		t.Fatalf("hardware.last %q %v %v", raw, ok, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"apple_silicon", "detected_at", "intel_gpu", "intel_npu", "nvidia"}; !slices.Equal(keys, want) {
		t.Errorf("hardware.last keys %v, want %v (%s)", keys, want, raw)
	}
	var h HardwareLast
	_ = json.Unmarshal([]byte(raw), &h)
	if !slices.Equal(h.NVIDIA, []string{"NVIDIA GeForce RTX 5070 Ti Laptop GPU"}) || !h.IntelGPU || !h.IntelNPU || h.AppleSilicon {
		t.Errorf("hardware.last %+v", h)
	}
	if _, err := time.Parse(time.RFC3339, h.DetectedAt); err != nil {
		t.Errorf("detected_at %q", h.DetectedAt)
	}
	// A new server on the same database (a restart): the NVIDIA rows
	// without any detection in this run.
	if !de.srv.hasNVIDIA(context.Background()) {
		t.Fatal("hardware.last did not count")
	}
	fresh := &Server{st: de.st, dev: de.reg}
	fresh.diag = &diagnostics{s: fresh, goos: "windows"}
	if !fresh.hasNVIDIA(context.Background()) {
		t.Error("hardware.last was not read back")
	}
}

// Without a remembered detection the server detects once in the
// background at startup (not in test mode); the rows follow.
func TestHardwareDetectedAtStartup(t *testing.T) {
	old, oldDelay := startupHardwareDetect, envRerunDelay
	startupHardwareDetect, envRerunDelay = true, 10*time.Millisecond
	t.Cleanup(func() { startupHardwareDetect, envRerunDelay = old, oldDelay })
	var calls atomic.Int64
	detect := func(context.Context, string) (hwdetect.HW, error) {
		calls.Add(1)
		return hwdetect.HW{OS: "windows", NVIDIA: []hwdetect.GPU{{Name: "NVIDIA GeForce RTX 4060", Source: "wmi"}}}, nil
	}
	de := newDevEnv(t, envConfig{server: func(s *Server) {
		s.compState().detect = detect
		intelDevices(s.dev.(*fakeDevReg)) // nvidia-smi does not answer
	}})
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = checkNames(de.srv.diag.snapshot()); slices.Contains(got, "gpu") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(got, "gpu") || !slices.Contains(got, checkCUDNN) || calls.Load() != 1 {
		t.Fatalf("after the startup detection (%d runs): %v", calls.Load(), got)
	}
	// Remembered: the next start does not detect.
	var mu sync.Mutex
	again := 0
	raw, _, _ := de.st.GetKV(context.Background(), kvHardwareLast)
	newDevEnv(t, envConfig{sched: func(o *sched.Options) {
		if err := o.Store.SetKV(context.Background(), kvHardwareLast, raw); err != nil {
			t.Fatal(err)
		}
	}, server: func(s *Server) {
		s.compState().detect = func(context.Context, string) (hwdetect.HW, error) {
			mu.Lock()
			again++
			mu.Unlock()
			return hwdetect.HW{}, nil
		}
	}})
	// Test mode (asr.fake_result) never detects.
	newDevEnv(t, envConfig{server: func(s *Server) {
		s.cfg.ASR.FakeResult = "fake.json"
		s.compState().detect = func(context.Context, string) (hwdetect.HW, error) {
			mu.Lock()
			again++
			mu.Unlock()
			return hwdetect.HW{}, nil
		}
	}})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if again != 0 {
		t.Errorf("detected %d more times", again)
	}
}

// ---- the examples testdata/api/system_{intel,mac,remote}.json ----

// The examples of the three environments without NVIDIA (the mock server's
// diag_intel / diag_mac / diag_cloud) are System bodies (no unknown field),
// have the rows of the real checks in the real order, and name neither
// CUDA, cuDNN nor NVIDIA.
func TestExtDiagExamples(t *testing.T) {
	providers := []string{"api_key:DeepSeek", "api:DeepSeek", "api_key:Anthropic", "api:Anthropic", "api_key:OpenRouter", "api:OpenRouter", "disk"}
	for name, rows := range map[string][]string{
		"system_intel.json":  {"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "openvino_venv", "openvino", "openvino_devices", "npu_driver", "cpu_selftest", "browser_ext"},
		"system_mac.json":    {"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "mlx_python", "mlx", "mlx_model", "mlx_selftest", "browser_ext"},
		"system_remote.json": {"data_dir", "ffmpeg", "aria2c", "python", "pypdf", "gcloud", "colab_auth", "colab_reach", "browser_ext"},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", name))
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		var sys System
		if err := dec.Decode(&sys); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := checkNames(sys.Checks)
		var wantProviders []string
		for _, p := range providers {
			if slices.Contains(got, p) {
				wantProviders = append(wantProviders, p)
			}
		}
		if want := append(append([]string{}, rows...), wantProviders...); !slices.Equal(got, want) || got[len(got)-1] != "disk" {
			t.Errorf("%s: checks %v, want %v", name, got, want)
		}
		for _, c := range sys.Checks {
			if c.DetailInfo != nil {
				wantCataloged(t, name+" "+c.Name, c.DetailInfo)
			}
			if c.HintInfo != nil {
				wantCataloged(t, name+" "+c.Name+" hint", c.HintInfo)
			}
		}
		noNVIDIAWords(t, name, sys)
	}
}
