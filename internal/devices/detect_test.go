// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/model"
)

// The default OpenVINO probe runs `{python_openvino} asr_openvino.py
// --probe`. These tests use the test binary itself as python_openvino: with
// fakeProbeEnv set, init below plays the worker before any test runs (no
// TestMain, which task D's tests may need in this package).
const fakeProbeEnv = "LF_DEVICES_FAKE_PROBE"

// fakeProbeEnvOut is the file the fake probe of mode "env" writes its
// environment to (a JSON array of KEY=VALUE).
const fakeProbeEnvOut = "LF_DEVICES_FAKE_PROBE_ENV_OUT"

const fakeProbeJSON = `{"openvino":"2026.4.1","genai":"2026.4.1.0","devices":[{"name":"GPU.0","kind":"igpu","full_name":"Intel(R) Arc(TM) 140T GPU","vendor":"0x8086","luid":"0a0b","driver_version":"32.0.101.6881"}],"ignored":[{"name":"GPU.1","kind":"","full_name":"NVIDIA GeForce RTX","vendor":"0x10de","luid":"","driver_version":"","reason":"not_intel"}],"can_convert":false}`

func init() {
	mode := os.Getenv(fakeProbeEnv)
	if mode == "" {
		return
	}
	// python -s asr_openvino.py --probe (pyenv.ScriptArgs)
	if len(os.Args) != 4 || os.Args[1] != "-s" || filepath.Base(os.Args[2]) != "asr_openvino.py" || os.Args[3] != "--probe" {
		fmt.Fprintln(os.Stderr, "ERROR: fake probe: unexpected arguments", os.Args[1:])
		os.Exit(7)
	}
	switch mode {
	case "ok":
		fmt.Println("openvino loaded")
		fmt.Println("PROBE " + fakeProbeJSON)
		os.Exit(0)
	case "import":
		fmt.Fprintln(os.Stderr, "Traceback (most recent call last):")
		fmt.Fprintln(os.Stderr, "ModuleNotFoundError: No module named 'openvino'")
		fmt.Fprintln(os.Stderr, "ERROR: cannot import openvino: No module named 'openvino'")
		os.Exit(2)
	case "crash":
		fmt.Fprintln(os.Stderr, "something went wrong")
		os.Exit(3)
	case "noline":
		fmt.Println("hello")
		os.Exit(0)
	case "badjson":
		fmt.Println("PROBE {not json")
		os.Exit(0)
	case "env": // pyenv_test.go: the probe's environment goes to the file of fakeProbeEnvOut
		b, _ := json.Marshal(os.Environ())
		if os.WriteFile(os.Getenv(fakeProbeEnvOut), b, 0o644) != nil {
			os.Exit(8)
		}
		fmt.Println("PROBE " + fakeProbeJSON)
		os.Exit(0)
	case "argsenv": // probe_isolation_test.go: the arguments and the environment
		b, _ := json.Marshal(map[string][]string{"args": os.Args[1:], "env": os.Environ()})
		if os.WriteFile(os.Getenv(fakeProbeEnvOut), b, 0o644) != nil {
			os.Exit(8)
		}
		fmt.Println("PROBE " + fakeProbeJSON)
		os.Exit(0)
	case "cwd": // r9_probe_dir_test.go: the working directory
		// as a real interpreter: the script is opened from this process's
		// own working directory (SEC52 L-3: a relative path would fail here)
		if fi, err := os.Stat(os.Args[2]); err != nil || fi.IsDir() {
			fmt.Fprintln(os.Stderr, "ERROR: fake probe: cannot open the script", os.Args[2])
			os.Exit(6)
		}
		wd, err := os.Getwd()
		if err != nil || os.WriteFile(os.Getenv(fakeProbeEnvOut), []byte(wd), 0o644) != nil {
			os.Exit(8)
		}
		fmt.Println("PROBE " + fakeProbeJSON)
		os.Exit(0)
	}
	os.Exit(9)
}

// probeConfig points python_openvino at the test binary and worker_dir at
// a directory with an (empty) asr_openvino.py.
func probeConfig(t *testing.T) *config.Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "asr_openvino.py"), []byte("# fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.PythonOpenVINO, cfg.WorkerDir = exe, dir
	return cfg
}

func TestWorkerProbe(t *testing.T) {
	cfg := probeConfig(t)
	r := NewRegistry(regStore(t), cfg, t.TempDir(), RegistryOptions{CUDAName: func(context.Context) (string, error) { return "", errors.New("none") }})
	r.Load(tctx())

	t.Setenv(fakeProbeEnv, "ok")
	p, err := r.workerProbe(tctx())
	if err != nil {
		t.Fatal(err)
	}
	if p.OpenVINO != "2026.4.1" || len(p.Devices) != 1 || p.Devices[0].Kind != model.DeviceKindIGPU || p.Devices[0].LUID != "0a0b" ||
		len(p.Ignored) != 1 || p.Ignored[0].Reason != ProbeReasonNotIntel || p.CanConvert {
		t.Fatalf("probe %+v", p)
	}
	d, newIDs, err := r.Detect(tctx())
	if err != nil || d.OVErr != nil || !reflect.DeepEqual(newIDs, []string{model.DeviceOVGPU}) || d.Python != cfg.PythonOpenVINO {
		t.Fatalf("detect: %+v %v %v", d, newIDs, err)
	}

	for _, c := range []struct {
		mode, code, text string
	}{
		{"import", CodeOVImportFailed, "cannot import openvino: No module named 'openvino'"},
		{"crash", CodeProbeFailed, "something went wrong"},
		{"noline", CodeProbeFailed, "the probe printed no PROBE line"},
		{"badjson", CodeProbeFailed, "invalid PROBE line"},
	} {
		t.Setenv(fakeProbeEnv, c.mode)
		d, _, err := r.Detect(tctx())
		if err != nil {
			t.Fatal(err)
		}
		e := d.OVErr
		if d.OV != nil || e == nil || e.Code != c.code || !strings.Contains(fmt.Sprint(e.Params["error"]), c.text) || !strings.Contains(e.Detail, c.text) {
			t.Errorf("%s: OVErr %+v", c.mode, e)
		}
		if r.Present(model.DeviceOVGPU) {
			t.Errorf("%s: ov:GPU still present", c.mode)
		}
	}

	// No interpreter: devices.ov_python_missing {path}, no process.
	cfg.PythonOpenVINO = filepath.Join(t.TempDir(), ".venv-ov", "Scripts", "python.exe")
	d, _, _ = r.Detect(tctx())
	if e := d.OVErr; e == nil || e.Code != CodeOVPythonMissing || e.Params["path"] != cfg.PythonOpenVINO || d.Python != cfg.PythonOpenVINO {
		t.Fatalf("python missing: %+v", e)
	}
	// 3.1.0: no interpreter at all (the desktop application without
	// asr-openvino): devices.ov_not_installed without a path, no process.
	cfg.PythonOpenVINO = ""
	d, _, _ = r.Detect(tctx())
	if e := d.OVErr; e == nil || e.Code != CodeOVNotInstalled || len(e.Params) != 0 || d.Python != "" || d.OV != nil {
		t.Fatalf("not installed: %+v (python %q)", e, d.Python)
	}
	if got := r.RunningPythons(); len(got) != 0 {
		t.Fatalf("running %v", got)
	}
	// No worker script: probe_failed (python would exit 2 for a missing
	// script, which is not an import failure).
	cfg.PythonOpenVINO = probeConfig(t).PythonOpenVINO
	cfg.WorkerDir = t.TempDir()
	d, _, _ = r.Detect(tctx())
	if e := d.OVErr; e == nil || e.Code != CodeProbeFailed || !strings.Contains(e.Detail, "asr_openvino.py") {
		t.Fatalf("script missing: %+v", e)
	}
}

func TestProbeErrorInfoOfInjectedErrors(t *testing.T) {
	// An injected probe returning a plain error: probe_failed {error}.
	e := probeErrorInfo(errors.New("boom"))
	if e.Code != CodeProbeFailed || e.Params["error"] != "boom" || e.Detail != "boom" {
		t.Fatalf("plain: %+v", e)
	}
	// A coded probe error keeps its code; another code becomes probe_failed.
	e = probeErrorInfo(fmt.Errorf("wrapped: %w", model.Errorf(CodeOVPythonMissing, map[string]any{"path": "p"}, errors.New("missing p"))))
	if e.Code != CodeOVPythonMissing || e.Params["path"] != "p" || e.Detail != "missing p" {
		t.Fatalf("coded: %+v", e)
	}
	e = probeErrorInfo(model.Errorf("something.else", nil, errors.New("x")))
	if e.Code != CodeProbeFailed {
		t.Fatalf("other code: %+v", e)
	}
}

func TestParseProbe(t *testing.T) {
	p, err := ParseProbe("loading\r\nPROBE {\"openvino\":\"1\",\"genai\":\"2\",\"devices\":null,\"can_convert\":true}\r\nbye\n")
	if err != nil || p.OpenVINO != "1" || p.GenAI != "2" || !p.CanConvert || p.Devices == nil || p.Ignored == nil {
		t.Fatalf("probe %+v %v", p, err)
	}
	if _, err := ParseProbe("no probe here\n"); err == nil {
		t.Fatal("missing PROBE line accepted")
	}
	if _, err := ParseProbe("PROBE [1,2]\n"); err == nil {
		t.Fatal("bad JSON accepted")
	}
	// The JSON round-trips (the worker's field names).
	p, err = ParseProbe("PROBE " + fakeProbeJSON)
	if err != nil || p.Devices[0].FullName != "Intel(R) Arc(TM) 140T GPU" || p.Devices[0].DriverVersion != "32.0.101.6881" || p.Ignored[0].Vendor != "0x10de" {
		t.Fatalf("probe %+v %v", p, err)
	}
}

func TestParseCUDANameAndErrorLine(t *testing.T) {
	if n, err := ParseCUDAName("\r\n  NVIDIA GeForce RTX 5070 Ti Laptop GPU \r\nNVIDIA second\n"); err != nil || n != "NVIDIA GeForce RTX 5070 Ti Laptop GPU" {
		t.Fatalf("name %q %v", n, err)
	}
	if _, err := ParseCUDAName(" \n"); err == nil {
		t.Fatal("empty output accepted")
	}
	if got := lastErrorLine("a\nERROR: first\nb\nERROR:  second \nc\n", nil); got != "second" {
		t.Fatalf("error line %q", got)
	}
	if got := lastErrorLine("a\nlast line\n\n", nil); got != "last line" {
		t.Fatalf("last line %q", got)
	}
	if got := lastErrorLine("", errors.New("exit status 5")); got != "exit status 5" {
		t.Fatalf("fallback %q", got)
	}
	long := lastErrorLine("ERROR: "+strings.Repeat("长", 400), nil)
	if r := []rune(long); len(r) != maxErrorText+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("truncation: %d runes", len(r))
	}
}

func TestExecutableExists(t *testing.T) {
	exe, _ := os.Executable()
	if !executableExists(exe) || executableExists(filepath.Dir(exe)) || executableExists("") ||
		executableExists(filepath.Join(t.TempDir(), "nope.exe")) || executableExists("no-such-binary-lectureflow-xyz") {
		t.Fatal("executableExists")
	}
}
