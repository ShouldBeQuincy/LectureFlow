// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/pyenv"
)

// Detection helpers of the Registry (task B): the default nvidia-smi name
// query, the default OpenVINO probe (worker/asr_openvino.py --probe, brief
// section 4) and the classification of probe failures into the three
// Detection.OVErr codes.

// ProbeTimeout bounds one run of the OpenVINO probe (importing openvino and
// listing the devices takes a few seconds; the NPU driver can be slow on
// the first call after boot).
const ProbeTimeout = 60 * time.Second

// cudaNameTimeout bounds one nvidia-smi run (it normally answers in ~100 ms).
const cudaNameTimeout = 10 * time.Second

// FakeCUDAName is the NVIDIA name the default CUDAName reports in test mode
// (cfg.ASR.FakeResult set; no process is started).
const FakeCUDAName = "Fake NVIDIA GPU (test mode)"

// Fake OpenVINO devices of the default probe in test mode with
// cfg.ASR.FakeResultOpenVINO set.
const (
	FakeIntelGPUName = "Fake Intel GPU (test mode)"
	FakeIntelNPUName = "Fake Intel NPU (test mode)"
)

func (r *Registry) testMode() bool { return r.cfg.ASR.FakeResult != "" }

// cudaName runs o.CUDAName or the default.
func (r *Registry) cudaName(ctx context.Context) (string, error) {
	if r.o.CUDAName != nil {
		return r.o.CUDAName(ctx)
	}
	if r.testMode() {
		return FakeCUDAName, nil
	}
	return nvidiaName(ctx, r.cfg.NvidiaSMI)
}

// nvidiaName is `nvidia-smi --query-gpu=name --format=csv,noheader`, first
// non-empty line. It runs in nvidia-smi's own directory, not the service's
// current one (procutil.CommandInExeDir; SEC52 info-7).
func nvidiaName(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cudaNameTimeout)
	defer cancel()
	out, err := procutil.CommandInExeDir(ctx, exe, "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return "", fmt.Errorf("nvidia-smi: %w", err)
	}
	return ParseCUDAName(string(out))
}

// ParseCUDAName returns the first non-empty line of nvidia-smi's name query.
func ParseCUDAName(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", errors.New("nvidia-smi: empty output")
}

// probe runs o.Probe or the default.
func (r *Registry) probe(ctx context.Context) (*OVProbe, error) {
	if r.o.Probe != nil {
		return r.o.Probe(ctx)
	}
	if r.testMode() {
		return fakeProbe(r.cfg.ASR.FakeResultOpenVINO != ""), nil
	}
	return r.workerProbe(ctx)
}

// fakeProbe is the probe of test mode: the Intel GPU and the NPU when the
// fake OpenVINO result is configured, otherwise no OpenVINO device.
func fakeProbe(withDevices bool) *OVProbe {
	p := &OVProbe{OpenVINO: "test", GenAI: "test", Devices: []OVDevice{}, Ignored: []OVDevice{}, CanConvert: true}
	if withDevices {
		p.Devices = []OVDevice{
			{Name: "GPU.0", Kind: model.DeviceKindIGPU, FullName: FakeIntelGPUName, Vendor: "0x8086"},
			{Name: "NPU", Kind: model.DeviceKindNPU, FullName: FakeIntelNPUName, Vendor: "0x8086"},
		}
	}
	return p
}

// workerProbe runs `{python_openvino} -s {worker_dir}/asr_openvino.py --probe`.
// Errors are coded: CodeOVNotInstalled (no interpreter at all: the desktop
// application without asr-openvino), CodeOVPythonMissing (the configured
// interpreter does not exist), CodeOVImportFailed (exit 2: openvino cannot
// be imported), CodeProbeFailed (anything else).
func (r *Registry) workerProbe(ctx context.Context) (*OVProbe, error) {
	py, _, _ := r.cfg.WorkerPython(model.BackendOpenVINO)
	if py == "" { // the desktop application without asr-openvino (3.1.0)
		return nil, model.Errorf(CodeOVNotInstalled, nil, errors.New("the asr-openvino component is not installed"))
	}
	if !executableExists(py) {
		return nil, model.Errorf(CodeOVPythonMissing, map[string]any{"path": py},
			fmt.Errorf("python_openvino not found: %s", py))
	}
	// A path with a directory part is started by its absolute path, the
	// file executableExists just found: the probe runs in the worker
	// directory (below), where the system would look for a relative one
	// (SEC52 info-1). A bare name stays a PATH look-up.
	if filepath.Base(py) != py {
		if abs, err := filepath.Abs(py); err == nil {
			py = abs
		}
	}
	script := filepath.Join(r.cfg.WorkerDir, "asr_openvino.py")
	// absolute: the probe runs in the script's directory (below), so a
	// relative path would no longer name it there
	if abs, err := filepath.Abs(script); err == nil {
		script = abs
	}
	if fi, err := os.Stat(script); err != nil || fi.IsDir() {
		msg := "asr_openvino.py not found: " + script
		return nil, model.Errorf(CodeProbeFailed, map[string]any{"error": msg}, errors.New(msg))
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	defer r.trackPython(py)()
	var stdout, stderr bytes.Buffer
	// -s and pyenv's environment: no PYTHONPATH and the like (pyenv
	// "Isolation"), no usage reporting (the probe imports openvino)
	cmd := exec.CommandContext(ctx, py, pyenv.ScriptArgs(script, "--probe")...)
	// The worker directory (the application's own resources/worker, or the
	// repository's worker/ in development), not the service's current
	// directory: the Windows DLL search looks in the current directory when
	// openvino loads its native libraries (SEC47 info-5, SEC50 info-2).
	// sys.path is unaffected: in script mode it starts with this directory
	// anyway, and -s with the filtered environment stays as it was.
	cmd.Dir = filepath.Dir(script)
	cmd.Env = pyenv.Environ()
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := procutil.Run(cmd, procutil.Tied) // ends with LectureFlow, also on a crash (test-chaos 1)
	if err != nil {
		var ee *exec.ExitError
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			msg := fmt.Sprintf("timeout after %s", ProbeTimeout)
			return nil, model.Errorf(CodeProbeFailed, map[string]any{"error": msg}, errors.New(msg))
		case errors.As(err, &ee) && ee.ExitCode() == 2:
			msg := lastErrorLine(stderr.String(), err)
			return nil, model.Errorf(CodeOVImportFailed, map[string]any{"error": msg}, errors.New(msg))
		default:
			msg := lastErrorLine(stderr.String(), err)
			return nil, model.Errorf(CodeProbeFailed, map[string]any{"error": msg}, errors.New(msg))
		}
	}
	p, err := ParseProbe(stdout.String())
	if err != nil {
		return nil, model.Errorf(CodeProbeFailed, map[string]any{"error": err.Error()}, err)
	}
	return p, nil
}

// probeUse counts, per registry, the interpreters its detection runs right
// now (the OpenVINO probe, the MLX check). It is kept outside Registry so
// the detection files own it.
var probeUse = struct {
	sync.Mutex
	m map[*Registry]map[string]int
}{m: map[*Registry]map[string]int{}}

// trackPython registers python as run by r's detection until the returned
// function is called.
func (r *Registry) trackPython(python string) (done func()) {
	if python == "" {
		return func() {}
	}
	probeUse.Lock()
	if probeUse.m[r] == nil {
		probeUse.m[r] = map[string]int{}
	}
	probeUse.m[r][python]++
	probeUse.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			probeUse.Lock()
			defer probeUse.Unlock()
			u := probeUse.m[r]
			if u[python]--; u[python] <= 0 {
				delete(u, python)
			}
			if len(u) == 0 {
				delete(probeUse.m, r)
			}
		})
	}
}

// RunningPythons returns the interpreters the device detection runs right
// now (the OpenVINO probe with the asr-openvino interpreter, the MLX check
// with the asr-mlx one), sorted. The component manager must not remove
// them meanwhile (components.Options.InUse, reason "self_check"; v3 Gate
// round 2).
func (r *Registry) RunningPythons() []string {
	probeUse.Lock()
	out := make([]string, 0, len(probeUse.m[r]))
	for p := range probeUse.m[r] {
		out = append(out, p)
	}
	probeUse.Unlock()
	sort.Strings(out)
	return out
}

// executableExists: a path with a directory part must be a file; a bare
// name is looked up in PATH.
func executableExists(p string) bool {
	if p == "" {
		return false
	}
	if filepath.Base(p) == p {
		_, err := exec.LookPath(p)
		return err == nil
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// maxErrorText bounds the error text kept from a worker's stderr.
const maxErrorText = 300

// lastErrorLine is the reason of a failed worker run: the text of its last
// "ERROR: " line on stderr (brief section 4), else its last non-empty
// stderr line, else err.
func lastErrorLine(stderr string, err error) string {
	var last, lastErr string
	sc := bufio.NewScanner(strings.NewReader(stderr))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		last = line
		if rest, ok := strings.CutPrefix(line, "ERROR:"); ok {
			lastErr = strings.TrimSpace(rest)
		}
	}
	msg := lastErr
	if msg == "" {
		msg = last
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if r := []rune(msg); len(r) > maxErrorText {
		msg = string(r[:maxErrorText]) + "…"
	}
	return msg
}

// ParseProbe reads the "PROBE {json}" line of the worker's stdout (other
// lines are ignored); slices are never nil.
func ParseProbe(stdout string) (*OVProbe, error) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "PROBE ")
		if !ok {
			continue
		}
		var p OVProbe
		if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &p); err != nil {
			return nil, fmt.Errorf("invalid PROBE line: %w", err)
		}
		return normalizeProbe(&p), nil
	}
	return nil, errors.New("the probe printed no PROBE line")
}

// normalizeProbe returns a copy whose slices are never nil.
func normalizeProbe(p *OVProbe) *OVProbe {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Devices = append([]OVDevice{}, p.Devices...)
	cp.Ignored = append([]OVDevice{}, p.Ignored...)
	return &cp
}

// probeErrorInfo classifies a probe error (Detection.OVErr): a coded error
// with one of the four probe codes keeps its code and params; anything
// else is CodeProbeFailed {error}. Detail is the raw text.
func probeErrorInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		switch ce.Info.Code {
		case CodeOVNotInstalled, CodeOVPythonMissing, CodeOVImportFailed, CodeProbeFailed:
			info := ce.Info
			if info.Detail == "" && ce.Err != nil {
				info.Detail = ce.Err.Error()
			}
			return &info
		}
	}
	msg := err.Error()
	return &model.ErrorInfo{Code: CodeProbeFailed, Params: map[string]any{"error": msg}, Detail: msg}
}
