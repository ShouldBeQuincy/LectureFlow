// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/dirperm"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/resources"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// env is a real SQLite store, the v1.1 settings service pointing at the
// fake LLM server (never a real API: llmtest.Configure runs before the
// settings import), a scheduler that is not started unless a test asks
// (jobs only move when a test moves them) and the HTTP server under httptest.
type env struct {
	t   *testing.T
	dir string
	cfg *config.Config
	st  db.Store
	llm *llmtest.Server
	set *settings.Service
	sc  *sched.Scheduler
	res *resources.Service
	hub *Hub
	srv *Server
	ts  *httptest.Server
}

type envOpt func(*Server)

// envConfig tunes newEnvWith.
type envConfig struct {
	server func(*Server)        // injected fields of the server
	sched  func(*sched.Options) // scheduler options
	// resources builds a resource watcher (fake nvidia-smi, 50 ms interval);
	// resDeps may adjust its dependencies.
	resources bool
	resDeps   func(*resources.Deps)
	start     bool // start the scheduler (stopped on cleanup)
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	return newEnvWith(t, envConfig{server: func(s *Server) {
		for _, o := range opts {
			o(s)
		}
	}})
}

// fakeSMI is a recorded nvidia-smi line (testdata/resources/nvidia_smi_laptop.txt).
func fakeSMI(context.Context, string, ...string) (string, error) {
	return "NVIDIA GeForce RTX 5070 Ti Laptop GPU, 7, 3000, 12227, 45, 5.88, 90.00\n", nil
}

func newEnvWith(t *testing.T, ec envConfig) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := llmtest.New(t)
	cfg := config.Default()
	cfg.DataDir = filepath.Join(dir, "data")
	// Never the machine's own aria2c: CI runners have one on PATH
	// (Chocolatey, Homebrew), and the diagnostics would run it. Tests of
	// the aria2c check set their own.
	cfg.Aria2c = filepath.Join(dir, "no-aria2c", "aria2c")
	// as the application creates it (cmd: private from the start; the
	// system's temporary directory may let other accounts in, which the
	// data_dir row would report)
	if _, err := dirperm.MkdirPrivate(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	fake.Configure(cfg) // BEFORE settings.Open: the import must never see a real endpoint or key
	set, err := settings.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	hub.MinInterval = 50 * time.Millisecond
	hub.PingInterval = 200 * time.Millisecond
	opts := sched.Options{
		Store: st, Config: cfg, Settings: set, Events: hub, DataDir: cfg.DataDir,
		GPUQuery: func(context.Context) (sched.GPUMem, error) {
			return sched.GPUMem{FreeMB: 9000, TotalMB: 12000, UsedMB: 3000}, nil
		},
	}
	if ec.sched != nil {
		ec.sched(&opts)
	}
	sc, err := sched.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, cfg: cfg, st: st, llm: fake, set: set, sc: sc, hub: hub}
	if ec.resources {
		d := resources.Deps{
			Store: st, Providers: set.Providers, ASR: set.ASR, Slots: sc.GPUSlots,
			Inflight: func(id string) (int, int) {
				in, mx, _ := sc.Pool().ProviderStatus(id)
				return in, mx
			},
			RunCmd: fakeSMI, Interval: 50 * time.Millisecond, OnSample: hub.Resources,
		}
		if ec.resDeps != nil {
			ec.resDeps(&d)
		}
		e.res = resources.New(d)
		e.res.Start(ctx)
		t.Cleanup(e.res.Stop)
	}
	client := llm.NewWithSettings(set, sc.Pool(), sc.Pool(), filepath.Join(dir, "llmlogs"), nil)
	e.srv = newServer(Deps{Store: st, Sched: sc, Cfg: cfg, DataDir: cfg.DataDir, Hub: hub, Settings: set,
		Resources: e.res, LLM: client}, func(s *Server) {
		// Diagnostics never run real programs in tests.
		s.runCmd = fakeCmd
		s.freeSpace = func(string) (int64, error) { return 100 << 30, nil }
		s.extractPDF = func(context.Context, string, string) ([]string, error) {
			return nil, errors.New("no PDF worker in tests")
		}
		if ec.server != nil {
			ec.server(s)
		}
	})
	// Every error response of every test must be a coded body (P14.3).
	e.ts = httptest.NewServer(checkErrorBodies(t, e.srv.Handler()))
	if ec.start {
		if err := sc.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if ec.start {
			sc.Stop()
		}
		e.hub.Close() // end open event streams, or ts.Close would wait for them
		e.ts.Close()
		e.srv.WaitInvalidations()
		e.srv.diag.waitStartup()
	})
	return e
}

// isScriptRun: args run a worker script with flag, as pyenv.ScriptArgs
// puts it: "-s", the script (*.py), flag, ...
func isScriptRun(args []string, flag string) bool {
	return len(args) > 2 && args[0] == "-s" && strings.HasSuffix(args[1], ".py") && args[2] == flag
}

// fakeCmd stands in for ffmpeg / python in the diagnostics.
func fakeCmd(_ context.Context, name string, args ...string) (string, string, error) {
	switch {
	case len(args) == 1 && args[0] == "-version":
		return "ffmpeg version 9.0.1-test Copyright\nmore", "", nil
	case len(args) == 1 && args[0] == "--version":
		return "Python 3.13.1\n", "", nil
	case len(args) == 3 && args[0] == "-I" && args[1] == "-c":
		return "", "FutureWarning: something\n", nil
	case isScriptRun(args, "--selftest"):
		return "SELFTEST ok device=cuda compute_type=float16 load_s=1.0 transcribe_s=0.10 segments=0\n", "", nil
	case len(args) == 3 && isScriptRun(args, "--check-cudnn"): // ext-step1
		return "CUDNN ok version=9.1.0\n", "", nil
	}
	return "", "unknown", errUnknownCmd
}

var errUnknownCmd = &httpError{status: 500, info: model.ErrorInfo{Code: "server.internal", Detail: "unknown command"}}

func (e *env) do(method, path string, body any) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

// call expects the status and decodes the JSON body into out (strictly).
func (e *env) call(method, path string, body any, want int, out any) {
	e.t.Helper()
	res, b := e.do(method, path, body)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, res.StatusCode, want, b)
	}
	if out != nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode %T: %v; body %s", method, path, out, err, b)
		}
	}
}

// callErr expects an error status with a coded body {"error": {"code",
// "params", "detail"}} (v2 P14.3) and returns "code text": the code and its
// Chinese catalog text with the params filled in, so tests can check either.
func (e *env) callErr(method, path string, body any, want int) string {
	e.t.Helper()
	info := e.callErrInfo(method, path, body, want)
	return info.Code + " " + zhText(info)
}

// callErrInfo is callErr returning the decoded ErrorInfo.
func (e *env) callErrInfo(method, path string, body any, want int) model.ErrorInfo {
	e.t.Helper()
	res, b := e.do(method, path, body)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, res.StatusCode, want, b)
	}
	var er ErrorBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&er); err != nil || er.Error.Code == "" {
		e.t.Fatalf("%s %s: want {\"error\":{\"code\":...}}, got %s", method, path, b)
	}
	return er.Error
}

func multipartBody(t *testing.T, fields [][2]string, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(content)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *env) upload(path string, fields [][2]string, filename string, content []byte) (*http.Response, []byte) {
	e.t.Helper()
	body, ctype := multipartBody(e.t, fields, filename, content)
	res, err := http.Post(e.ts.URL+path, ctype, body)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (e *env) course(name string) Course {
	e.t.Helper()
	var c Course
	e.call("POST", "/api/courses", map[string]string{"name": name}, http.StatusCreated, &c)
	return c
}

// draft uploads a lecture; v1.1 A3: it stays a draft until launched.
func (e *env) draft(courseID, title string) Lecture {
	e.t.Helper()
	res, b := e.upload("/api/courses/"+courseID+"/lectures", [][2]string{{"title", title}}, "rec.mp4", []byte("fake media"))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("upload lecture: %d %s", res.StatusCode, b)
	}
	var l Lecture
	if err := json.Unmarshal(b, &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

// lecture uploads and launches a lecture (like a v1 lecture: uploaded and started).
func (e *env) lecture(courseID, title string) Lecture {
	e.t.Helper()
	l := e.draft(courseID, title)
	e.call("POST", "/api/lectures/"+l.ID+"/launch", nil, http.StatusOK, &l)
	return l
}

// moveJob walks the current job of a stage through legal transitions.
func (e *env) moveJob(scope, scopeID, stage string, path ...string) model.Job {
	e.t.Helper()
	ctx := context.Background()
	for _, to := range path {
		j, err := e.st.GetCurrentJob(ctx, scope, scopeID, stage)
		if err != nil {
			e.t.Fatal(err)
		}
		if to == model.StatusFailed {
			if _, err := state.FailJob(ctx, e.st, j.ID, "boom: worker exited with code 1", false); err != nil {
				e.t.Fatal(err)
			}
			continue
		}
		if err := state.TransitionJob(ctx, e.st, j.ID, j.Status, to, db.JobFinish{}); err != nil {
			e.t.Fatalf("%s %s→%s: %v", stage, j.Status, to, err)
		}
	}
	j, err := e.st.GetCurrentJob(ctx, scope, scopeID, stage)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func stageOf(stages []StageView, name string) StageView {
	for _, s := range stages {
		if s.Stage == name {
			return s
		}
	}
	return StageView{}
}

func eqStrings(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}
