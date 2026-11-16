// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// ext-step1 (SPEC-ext "主程序配套改动 1. 诊断页按实际硬件显示",
// .claude/ext-brief.md §2): the diagnostics first decide what this computer
// is, then which checks exist at all. The server picks the rows (the page
// hides nothing), so a machine without NVIDIA never sees CUDA, cuDNN or
// NVIDIA in a check, a hint or the configuration, and neither nvidia-smi
// nor a CUDA self-test runs there.
//
//	NVIDIA (local)        faster-whisper, gpu, cudnn, cuda
//	Apple silicon (local) mlx_python, mlx, mlx_model, mlx_selftest
//	otherwise (local)     the OpenVINO rows (Intel GPU / NPU or an
//	                      OpenVINO environment), cpu_selftest
//	only Colab (remote)   none of these; gcloud, colab_auth, colab_reach
//
// The common rows (data_dir, ffmpeg, python, pypdf, the providers, disk)
// stay; the Colab rows also follow a local mode while Colab is switched on.

// Names of the checks added by ext-step1.
const (
	checkCUDNN       = "cudnn"        // the cuDNN library loads (asr.py --check-cudnn)
	checkCUDA        = "cuda"         // the CUDA self-test (recheck only)
	checkMLXSelftest = "mlx_selftest" // the MLX self-test (recheck only)
	checkCPUSelftest = "cpu_selftest" // the CPU self-test: ov:CPU, else fw:cpu (recheck only)
	checkGcloud      = "gcloud"       // the Google Cloud CLI is there
	checkColabAuth   = "colab_auth"   // a usable Colab credential
	checkColabReach  = "colab_reach"  // a free read-only call to Colab
)

// selftestChecks run only on "重新检查" (they may take tens of seconds and
// download the tiny model); otherwise their last result is kept.
var selftestChecks = []string{checkCUDA, checkMLXSelftest, checkCPUSelftest}

func isSelftestCheck(name string) bool {
	for _, n := range selftestChecks {
		if n == name {
			return true
		}
	}
	return false
}

// diagEnv is what this computer is, for the diagnostics.
type diagEnv struct {
	mac    bool // macOS: the MLX checks
	nvidia bool // an NVIDIA GPU (hasNVIDIA; never on macOS)
	remote bool // asr.mode = remote: no local inference rows
	cloud  bool // the Colab rows: remote, or Colab switched on
}

// local reports whether local inference rows of the kind exist.
func (e diagEnv) nvidiaRows() bool { return !e.remote && !e.mac && e.nvidia }
func (e diagEnv) macRows() bool    { return !e.remote && e.mac }
func (e diagEnv) cpuRows() bool    { return !e.remote && !e.mac && !e.nvidia }

// environment reads the environment now.
func (d *diagnostics) environment(ctx context.Context) diagEnv {
	s := d.s
	e := diagEnv{mac: d.mac()}
	e.remote = s.asrMode(ctx).Mode == ASRModeRemote
	e.nvidia = !e.mac && s.hasNVIDIA(ctx)
	e.cloud = e.remote || s.colabOn(ctx)
	return e
}

// checkAllowed reports whether a row of the checks belongs to env (the
// snapshot drops the others at once, before the checks run again).
func checkAllowed(name string, env diagEnv) bool {
	switch name {
	case "faster-whisper", "gpu", checkCUDNN, checkCUDA:
		return env.nvidiaRows()
	case checkMLXPython, checkMLX, checkMLXModel, checkMLXSelftest:
		return env.macRows()
	case checkCPUSelftest, checkOpenVINOVenv, checkOpenVINO, checkOpenVINODevices, checkNPUDriver:
		return env.cpuRows() || (name != checkCPUSelftest && env.nvidiaRows())
	case checkGcloud, checkColabAuth, checkColabReach:
		return env.cloud
	}
	return true
}

// hasNVIDIA reports whether this computer has an NVIDIA GPU (ext-brief §2):
// the remembered hardware detection (kv hardware.last) or this run's named
// one, or the device detection's nvidia-smi answered. A cuda:0 row in the
// device table no longer counts (rows migrated from old settings exist on
// computers without one). Never on macOS; always without the device
// registry (the legacy single-device builds, as before).
func (s *Server) hasNVIDIA(ctx context.Context) bool {
	if s.diag != nil && s.diag.mac() {
		return false
	}
	if s.dev == nil {
		return true
	}
	if s.dev.LastDetection().CUDAName != "" {
		return true
	}
	if h, ok := s.hardwareLast(ctx); ok && len(h.NVIDIA) > 0 {
		return true
	}
	hw, ok := s.cachedHW()
	return ok && len(hw.NVIDIA) > 0
}

// colabOn reports whether Colab is set up and switched on.
func (s *Server) colabOn(ctx context.Context) bool {
	if s.colab == nil || s.sc == nil {
		return false
	}
	p, err := s.sc.RemotePrefs(ctx)
	return err == nil && p.Enabled
}

// stale reports whether the environment changed since the last run.
func (d *diagnostics) stale(ctx context.Context) bool {
	env := d.environment(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ran && env != d.ranEnv
}

// envRerunDelay is how long the checks wait after a change of the
// environment (several changes make one run).
var envRerunDelay = 200 * time.Millisecond

// envChanged runs the checks again soon (without the self-tests): the
// transcription mode, the hardware detection or Colab's switch changed.
func (d *diagnostics) envChanged() { d.rerunAfter(envRerunDelay, false) }

// ---- codes ----

// Codes of the ext-step1 checks (part of the API's catalog like
// platformCheckCodes; init adds them).
const (
	codeCUDNNOK              = "check.cudnn_ok"
	codeCUDNNFailed          = "check.cudnn_failed"
	codeHintCUDNN            = "check.hint_cudnn"
	codeFakeSkipped          = "check.fake_skipped"
	codeSelftestNoModel      = "check.selftest_no_model"
	codeSelftestNotInstalled = "check.selftest_not_installed"
	codeHintInstallLocal     = "check.hint_install_local"
	codeHintLocalDev         = "check.hint_local_dev"
	codeHintRepairLocal      = "check.hint_repair_local"
	codeHintRepairOpenVINO   = "check.hint_repair_openvino"
	codeHintOVModelDev       = "check.hint_ov_model_dev"
	codeOpenVINOVenvAbsent   = "check.openvino_venv_absent"
	codeGcloudOK             = "check.gcloud_ok"
	codeGcloudMissing        = "check.gcloud_missing"
	codeHintInstallGcloud    = "check.hint_install_gcloud"
	codeHintGcloudDev        = "check.hint_gcloud_dev"
	codeColabOff             = "check.colab_off"
	codeColabAuthOK          = "check.colab_auth_ok"
	codeColabAuthMissing     = "check.colab_auth_missing"
	codeHintColabAuth        = "check.hint_colab_auth"
	codeColabReachOK         = "check.colab_reach_ok"
	codeColabReachFailed     = "check.colab_reach_failed"
	codeColabReachSkipped    = "check.colab_reach_skipped"
	codeHintColabReach       = "check.hint_colab_reach"
)

var envCheckCodes = map[string]errDef{
	codeCUDNNOK:              {"cuDNN {version} 可以加载", []string{"version"}},
	codeCUDNNFailed:          {"无法加载 cuDNN：{error}", []string{"error"}},
	codeHintCUDNN:            {"在项目的 .venv 中执行 pip install -r worker/requirements.txt（含 nvidia-cudnn-cu12），然后点击“重新检查”", nil},
	codeFakeSkipped:          {"测试模式（asr.fake_result），跳过这项检查", nil},
	codeSelftestNoModel:      {"未运行：没有 Whisper {model}（{precision}）的模型文件", []string{"model", "precision"}},
	codeSelftestNotInstalled: {"未运行：还没有安装本地转写组件", nil},
	codeHintInstallLocal:     {"按推荐安装本地转写组件，装好后点击“重新检查”", nil},
	codeHintLocalDev:         {"按 worker/requirements-openvino.txt 在项目目录建立 .venv-ov（或按 worker/requirements.txt 建立 .venv），然后点击“重新检查”", nil},
	codeHintRepairLocal:      {"在设置的“组件”中修复本地转写组件，然后点击“重新检查”", nil},
	codeHintRepairOpenVINO:   {"在设置的“组件”中修复“Intel 转写（OpenVINO）”，然后点击“重新检查”", nil},
	codeHintOVModelDev:       {"把转换好的 OpenVINO 模型放到 {path}，或先在这台电脑上转写一节课（会自动准备模型），然后点击“重新检查”", []string{"path"}},
	codeOpenVINOVenvAbsent:   {"没有找到 OpenVINO 环境（{path}）。Intel 核显与 NPU 不可用", []string{"path"}},
	codeGcloudOK:             {"已安装：{path}", []string{"path"}},
	codeGcloudMissing:        {"没有找到 Google Cloud CLI（gcloud）", nil},
	codeHintInstallGcloud:    {"在设置的“组件”中安装“Google Cloud CLI（gcloud）”，然后点击“重新检查”", nil},
	codeHintGcloudDev:        {"安装 Google Cloud CLI，确认 gcloud 在 PATH 中，然后点击“重新检查”", nil},
	codeColabOff:             {"Colab 远端转写没有启动", nil},
	codeColabAuthOK:          {"已授权", nil},
	codeColabAuthMissing:     {"还没有授权 Google Colab，或授权已过期", nil},
	codeHintColabAuth:        {"在设置的“Colab 远端转写”中登录 Google，然后点击“重新检查”", nil},
	codeColabReachOK:         {"可以连接 Google Colab（{ms} ms），没有分配运行时", []string{"ms"}},
	codeColabReachFailed:     {"无法连接 Google Colab：{error}", []string{"error"}},
	codeColabReachSkipped:    {"未检查：还没有授权", nil},
	codeHintColabReach:       {"检查网络连接；仍然失败时在设置的“Colab 远端转写”中重新登录 Google，然后点击“重新检查”", nil},
}

func init() {
	for code, def := range envCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// ---- helpers ----

// installedInterp is diagInterp of a backend that is really there: its
// component, or a configured interpreter whose file exists (web mode's
// diagInterp also returns a configured path that does not exist, for a
// check to report). The self-tests choose by it.
func (s *Server) installedInterp(backend string) diagPython {
	p := s.diagInterp(backend)
	if !p.ok {
		return p
	}
	_, _, component := s.cfg.WorkerPython(backend)
	if component || pythonExists(p.path) {
		return p
	}
	return diagPython{path: p.path}
}

// workerErr is the reason of a failed worker run: its last stderr line
// without the "ERROR: " prefix of the worker protocol, else err.
func workerErr(errOut string, err error) string {
	detail := lastLine(errOut)
	if rest, ok := strings.CutPrefix(detail, "ERROR:"); ok {
		detail = strings.TrimSpace(rest)
	}
	if detail == "" && err != nil {
		detail = err.Error()
	}
	return detail
}

// selftestLine sets c's detail to the worker's own SELFTEST line, else the
// generic "自检通过".
func selftestLine(c *SystemCheck, out string) {
	if m := selftestRe.FindString(out); m != "" {
		c.Detail = strings.TrimSpace(m)
		c.DetailInfo = nil
		return
	}
	c.setDetail("check.selftest_ok", nil)
}

func fakeSkipped(name string) SystemCheck {
	return checkWith(SystemCheck{Name: name, Status: statusWarn}, codeFakeSkipped, nil)
}

// ---- NVIDIA: cuDNN ----

var cudnnRe = regexp.MustCompile(`CUDNN ok version=(\S+)`)

// checkCUDNN loads cuDNN in the faster-whisper worker's interpreter
// (worker/asr.py --check-cudnn: "CUDNN ok version=…"). Rated like the
// CUDA rows (cudaRule).
func (s *Server) checkCUDNN(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkCUDNN, Status: statusOK}
	if s.cfg.ASR.FakeResult != "" {
		return fakeSkipped(checkCUDNN)
	}
	failStatus, _ := s.cudaRule()
	python := s.diagInterp(model.BackendFasterWhisper)
	if !python.ok {
		c.Status = failStatus
		c.setDetail(codeFWNotInstalled, nil)
		c.setHint(codeHintInstallFW, nil)
		return c
	}
	script := filepath.Join(s.cfg.WorkerDir, "asr.py")
	out, errOut, err := s.pyScript(ctx, cmdTimeout, python.path, script, "--check-cudnn")
	m := cudnnRe.FindStringSubmatch(out)
	if err != nil || m == nil {
		if err == nil {
			err = errors.New("no CUDNN line")
		}
		c.Status = failStatus
		c.setDetail(codeCUDNNFailed, map[string]any{"error": workerErr(errOut, err)})
		c.setHint(s.devHint(codeHintCUDNN, codeHintSelftestDesktop), nil)
		return c
	}
	c.setDetail(codeCUDNNOK, map[string]any{"version": m[1]})
	return c
}

// ---- Apple silicon: the MLX self-test ----

// mlxModel is the model and precision of mlx:gpu, else the recommended ones
// (what the setup guide installs), with the directory a run loads.
func (s *Server) mlxModel() (mdl, precision, dir string) {
	cand, ok := devices.Recommended(model.DeviceMLXGPU)
	if !ok {
		cand = devices.Fallback(model.BackendMLX)
	}
	mdl, precision = cand.Model, cand.Precision
	if s.dev != nil {
		if d, ok := s.dev.Get(model.DeviceMLXGPU); ok && d.Model != "" {
			mdl, precision = d.Model, d.Precision
		}
	}
	return mdl, precision, devices.MLXModelPath(s.cfg, s.absDataDir(), mdl, precision)
}

// checkMLXSelftest transcribes one second of silence with the MLX model
// (worker/asr_mlx.py --selftest --model DIR). Not run while MLX or the
// model is missing (their own rows say what to install).
func (s *Server) checkMLXSelftest(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkMLXSelftest, Status: statusOK}
	if s.cfg.ASR.FakeResult != "" {
		return fakeSkipped(checkMLXSelftest)
	}
	python := s.diagInterp(model.BackendMLX)
	if !python.ok {
		c.Status = statusWarn
		c.setDetail(codeSelftestNotInstalled, nil)
		return c
	}
	mdl, precision, dir := s.mlxModel()
	if !devices.MLXModelFilesReady(dir) {
		c.Status = statusWarn
		c.setDetail(codeSelftestNoModel, map[string]any{"model": mdl, "precision": precision})
		return c
	}
	script := filepath.Join(s.cfg.WorkerDir, "asr_mlx.py")
	out, errOut, err := s.pyScript(ctx, selftestTimeout, python.path, script, "--selftest", "--model", dir)
	if err != nil {
		c.Status = s.backendFail(model.BackendMLX)
		c.setDetail("check.selftest_failed", map[string]any{"error": workerErr(errOut, err)})
		c.setHint(s.devHint(codeHintMLXVenv, codeHintInstallMLX), nil)
		return c
	}
	selftestLine(&c, out)
	return c
}

// ---- no NVIDIA, no Apple silicon: the CPU self-test ----

// ovSelftestModel is the OpenVINO model the CPU self-test loads: the first
// of the OpenVINO devices' models (ov:CPU first), the recommended one of
// ov:CPU and the fallback that is converted; else the first of them, not
// ready.
func (s *Server) ovSelftestModel() (mdl, precision, dir string, ready bool) {
	var cands []devices.Candidate
	if s.dev != nil {
		for _, id := range []string{model.DeviceOVCPU, model.DeviceOVGPU, model.DeviceOVNPU} {
			if d, ok := s.dev.Get(id); ok && d.Model != "" {
				cands = append(cands, devices.Candidate{Model: d.Model, Precision: d.Precision})
			}
		}
	}
	if c, ok := devices.Recommended(model.DeviceOVCPU); ok {
		cands = append(cands, c)
	}
	cands = append(cands, devices.Fallback(model.BackendOpenVINO))
	data := s.absDataDir()
	for _, c := range cands {
		if d := devices.OVModelPath(s.cfg, data, c.Model, c.Precision); devices.ModelFilesReady(d) {
			return c.Model, c.Precision, d, true
		}
	}
	c := cands[0]
	return c.Model, c.Precision, devices.OVModelPath(s.cfg, data, c.Model, c.Precision), false
}

// checkCPUSelftest transcribes one second of silence on the CPU: with
// OpenVINO when its environment is installed (worker/asr_openvino.py
// --selftest --device CPU --model DIR), else with faster-whisper on the
// CPU (worker/asr.py --selftest --device cpu); neither installed is a
// warning that points to the recommended local install. Nothing here
// names a GPU vendor (ext-brief §2).
func (s *Server) checkCPUSelftest(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkCPUSelftest, Status: statusOK}
	if s.cfg.ASR.FakeResult != "" {
		return fakeSkipped(checkCPUSelftest)
	}
	if ov := s.installedInterp(model.BackendOpenVINO); ov.ok {
		mdl, precision, dir, ready := s.ovSelftestModel()
		if !ready {
			c.Status = statusWarn
			params := map[string]any{"model": mdl, "precision": precision}
			c.setDetail(codeSelftestNoModel, params)
			if s.desktopDiag() {
				c.setHint(codeHintInstallMLXModel, params) // "在设置的“组件”中安装“Whisper {model} 模型（{precision}）”"
			} else {
				c.setHint(codeHintOVModelDev, map[string]any{"path": dir})
			}
			return c
		}
		script := filepath.Join(s.cfg.WorkerDir, "asr_openvino.py")
		out, errOut, err := s.pyScript(ctx, selftestTimeout, ov.path, script, "--selftest", "--device", "CPU", "--model", dir)
		if err != nil {
			c.Status = s.backendFail(model.BackendOpenVINO)
			c.setDetail("check.selftest_failed", map[string]any{"error": workerErr(errOut, err)})
			c.setHint(s.devHint("check.hint_openvino_venv", codeHintRepairOpenVINO), nil)
			return c
		}
		selftestLine(&c, out)
		return c
	}
	if fw := s.installedInterp(model.BackendFasterWhisper); fw.ok {
		script := filepath.Join(s.cfg.WorkerDir, "asr.py")
		out, errOut, err := s.pyScript(ctx, selftestTimeout, fw.path, script, "--selftest",
			"--device", "cpu", "--compute-type", config.ResolveComputeType(model.PrecisionAuto, "cpu", 0))
		if err != nil {
			c.Status = s.backendFail(model.BackendFasterWhisper)
			c.setDetail("check.selftest_failed", map[string]any{"error": workerErr(errOut, err)})
			c.setHint(s.devHint("check.hint_pip", codeHintRepairLocal), nil)
			return c
		}
		selftestLine(&c, out)
		return c
	}
	c.Status = statusWarn
	c.setDetail(codeSelftestNotInstalled, nil)
	c.setHint(s.devHint(codeHintLocalDev, codeHintInstallLocal), nil)
	return c
}

// ---- Colab ----

// checkGcloud: the Google Cloud CLI the in-app sign-in runs (the gcloud
// component, else gcloud on PATH). Only its presence: running it takes
// seconds. A missing one is a warning (a stored credential works without
// it).
func (s *Server) checkGcloud(context.Context) SystemCheck {
	c := SystemCheck{Name: checkGcloud, Status: statusOK}
	if p, ok := gcloudLookup(s); ok {
		c.setDetail(codeGcloudOK, map[string]any{"path": p})
		return c
	}
	c.Status = statusWarn
	c.setDetail(codeGcloudMissing, nil)
	c.setHint(s.devHint(codeHintGcloudDev, codeHintInstallGcloud), nil)
	return c
}

// checkColabAuthRow: a stored Colab credential within its lifecycle (no
// network). Missing: an error when only Colab transcribes (remote), a
// warning otherwise.
func (s *Server) checkColabAuthRow(ctx context.Context, remote bool) SystemCheck {
	c := SystemCheck{Name: checkColabAuth, Status: statusOK}
	fail := statusWarn
	if remote {
		fail = statusError
	}
	if s.colab == nil {
		c.Status = fail
		c.setDetail(codeColabOff, nil)
		return c
	}
	if info := s.colab.Credential(ctx); !info.Present || info.RemainingSec == 0 {
		c.Status = fail
		c.setDetail(codeColabAuthMissing, nil)
		c.setHint(codeHintColabAuth, nil)
		return c
	}
	c.setDetail(codeColabAuthOK, nil)
	return c
}

// colabReacher is the Colab backend's free reachability call: refresh the
// access token and make one read-only request; no runtime is assigned and
// nothing is spent (ext-brief §2). colab.Backend gets it from the colab
// package's owner (ext-step1 report); until then the row says "未检查".
type colabReacher interface {
	Reach(ctx context.Context) error
}

// colabReachTimeout bounds the reachability call.
const colabReachTimeout = 30 * time.Second

// reachColab makes the call; supported false when the backend has none.
// It is the default of Server.colabReach (tests inject their own per
// server).
func (s *Server) reachColab(ctx context.Context) (supported bool, err error) {
	if s.colab == nil {
		return false, nil
	}
	r, ok := any(s.colab).(colabReacher)
	if !ok {
		return false, nil
	}
	return true, r.Reach(ctx)
}

// checkColabReach: Colab answers a read-only request with the stored
// credential (a warning when it does not: offline, the credential was
// revoked). Not checked without a credential (colab_auth says why).
func (s *Server) checkColabReach(ctx context.Context) SystemCheck {
	c := SystemCheck{Name: checkColabReach, Status: statusWarn}
	if s.colab == nil {
		c.setDetail(codeColabOff, nil)
		return c
	}
	if info := s.colab.Credential(ctx); !info.Present || info.RemainingSec == 0 {
		c.setDetail(codeColabReachSkipped, nil)
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, colabReachTimeout)
	defer cancel()
	reach := s.colabReach
	if reach == nil { // a Server not built by newServer
		reach = s.reachColab
	}
	t0 := time.Now()
	supported, err := reach(ctx)
	switch {
	case !supported:
		c.setDetail("check.api_skipped", nil) // "未检查"
	case err != nil:
		c.setDetail(codeColabReachFailed, map[string]any{"error": err.Error()})
		c.setHint(codeHintColabReach, nil)
	default:
		c.Status = statusOK
		c.setDetail(codeColabReachOK, map[string]any{"ms": time.Since(t0).Milliseconds()})
	}
	return c
}
