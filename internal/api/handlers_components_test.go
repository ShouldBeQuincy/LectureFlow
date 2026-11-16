// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/hwdetect"
)

// A real components.Manager over a local fake download server: only model
// and binary components (plain file downloads), so no uv or python is
// involved. The manifest is written to a temporary directory.

const (
	tcTool  = "tool"
	tcTiny  = "model:faster-whisper:tiny:int8"
	tcBig   = "model:faster-whisper:big:int8"
	tcOV    = "model:openvino:small:int8"
	tcGone  = "model:openvino:huge:fp16"
	tcBlock = 16 << 10
)

// fileServer serves files under /default/... and /mirror/... with Range
// support. A gated path stops after its first block until released (or the
// client goes away); status makes a path answer an HTTP error; slow makes
// every block of a path wait.
type fileServer struct {
	*httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	status map[string]int
	gates  map[string]chan struct{}
	hits   map[string]chan struct{}
	slow   map[string]time.Duration
}

func newFileServer(t *testing.T) *fileServer {
	fs := &fileServer{files: map[string][]byte{}, status: map[string]int{}, gates: map[string]chan struct{}{},
		hits: map[string]chan struct{}{}, slow: map[string]time.Duration{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(fs.handle))
	t.Cleanup(func() {
		fs.mu.Lock()
		for k, g := range fs.gates {
			close(g)
			delete(fs.gates, k)
		}
		fs.mu.Unlock()
		fs.Close()
	})
	return fs
}

func (fs *fileServer) put(p string, b []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files["/default"+p] = b
	fs.files["/mirror"+p] = b
}

func (fs *fileServer) setStatus(prefix string, code int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if code == 0 {
		delete(fs.status, prefix)
	} else {
		fs.status[prefix] = code
	}
}

// gate blocks the next request of the path after its first block.
func (fs *fileServer) gate(p string) (hit <-chan struct{}, release func()) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	g, h := make(chan struct{}), make(chan struct{})
	fs.gates["/default"+p], fs.hits["/default"+p] = g, h
	var once sync.Once
	return h, func() {
		once.Do(func() {
			fs.mu.Lock()
			if fs.gates["/default"+p] == g {
				delete(fs.gates, "/default"+p)
			}
			fs.mu.Unlock()
			close(g)
		})
	}
}

func (fs *fileServer) handle(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	for pre, code := range fs.status {
		if strings.HasPrefix(r.URL.Path, pre) {
			fs.mu.Unlock()
			w.WriteHeader(code)
			return
		}
	}
	b, ok := fs.files[r.URL.Path]
	gate, hit := fs.gates[r.URL.Path], fs.hits[r.URL.Path]
	delete(fs.gates, r.URL.Path) // one request only
	delete(fs.hits, r.URL.Path)
	slow := fs.slow[r.URL.Path]
	fs.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	start := int64(0)
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") && strings.HasSuffix(rg, "-") {
		if n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64); err == nil && n < int64(len(b)) {
			start = n
		}
	}
	body := b[start:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(b)-1, len(b)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
	}
	fl := w.(http.Flusher)
	for i := 0; i < len(body); i += tcBlock {
		end := min(i+tcBlock, len(body))
		if _, err := w.Write(body[i:end]); err != nil {
			return
		}
		fl.Flush()
		if gate != nil {
			close(hit)
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			gate = nil
		}
		if slow > 0 {
			time.Sleep(slow)
		}
	}
}

// compEnv is an API env with a component manager.
type compEnv struct {
	*env
	fs      *fileServer
	m       *components.Manager
	data    string
	content map[string][]byte
	free    atomic.Int64
	toolSHA string
}

func randBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// writeTestManifest writes components.json for the fake server and returns its path.
func writeTestManifest(t *testing.T, ce *compEnv) string {
	t.Helper()
	tool := []byte("tool binary v1")
	tiny := randBytes(1, 200<<10+17)
	big := randBytes(2, 600<<10+5)
	ov := randBytes(3, 90<<10)
	ce.fs.put("/files/tool.bin", tool)
	ce.fs.put("/hf/tiny/model.bin", tiny)
	ce.fs.put("/hf/big/model.bin", big)
	ce.fs.put("/hf/ov-small/model.xml", ov)
	ce.content = map[string][]byte{tcTool: tool, tcTiny: tiny, tcBig: big, tcOV: ov}
	ce.toolSHA = shaOf(tool)
	u := func(p string) string { return ce.fs.URL + p }
	plat := func(b *components.Build) map[string]*components.Build {
		return map[string]*components.Build{components.PlatformWindowsX64: b}
	}
	file := func(name, tmpl string, b []byte) components.File {
		return components.File{Name: name, URL: tmpl, SHA256: shaOf(b), Size: int64(len(b))}
	}
	man := components.Manifest{
		Schema: 1, Serial: 1, AppVersion: "3.0.0", Compat: ">=3.0.0 <4.0.0", Published: "2026-10-20T08:00:00Z",
		Sources: map[string]components.SourceGroup{
			components.GroupPyPI: {Primary: "pypi",
				Default: components.SourceDef{ID: "default", Values: map[string]string{"pypi": u("/default/simple")}},
				Mirrors: []components.SourceDef{{ID: "tuna", NameKey: "compui.source.name.tuna", URL: "https://pypi.tuna.example.invalid/", Region: "cn",
					Values: map[string]string{"pypi": u("/mirror/simple")}}}},
			components.GroupModels: {Primary: "models",
				Default: components.SourceDef{ID: "default", Values: map[string]string{"models": u("/default/hf")}},
				Mirrors: []components.SourceDef{{ID: "hf-mirror", NameKey: "compui.source.name.hf_mirror", URL: "https://hf-mirror.example.invalid/", Region: "cn",
					Values: map[string]string{"models": u("/mirror/hf")}}}},
			"files": {Primary: "files", Default: components.SourceDef{ID: "default", Values: map[string]string{"files": u("/default/files")}}},
		},
		Components: []components.Component{
			{ID: components.IDCore, Version: "3.0.0", Kind: components.KindBinary, Bundled: true},
			{ID: tcTool, Version: "1.0.0", Kind: components.KindBinary, License: "MIT",
				Platforms: plat(&components.Build{DownloadSize: int64(len(tool)), InstalledSize: int64(len(tool)),
					Files:   []components.File{file("tool.bin", "{files}/tool.bin", tool)},
					Offline: &components.Offline{SHA256: strings.Repeat("0", 64), Size: 1}})},
			{ID: tcTiny, Version: "1.0.0", Kind: components.KindModel, Backend: "faster-whisper", Model: "tiny", Precision: "int8",
				License: "MIT", Depends: []string{tcTool},
				Platforms: plat(&components.Build{DownloadSize: int64(len(tiny)), InstalledSize: int64(len(tiny)),
					Files: []components.File{file("model.bin", "{models}/tiny/model.bin", tiny)}})},
			{ID: tcBig, Version: "1.0.0", Kind: components.KindModel, Backend: "faster-whisper", Model: "big", Precision: "int8",
				License: "MIT",
				Platforms: plat(&components.Build{DownloadSize: int64(len(big)), InstalledSize: int64(len(big)),
					Files: []components.File{file("model.bin", "{models}/big/model.bin", big)}})},
			{ID: tcOV, Version: "1.0.0", Kind: components.KindModel, Backend: "openvino", Model: "small", Precision: "int8",
				License: "MIT",
				Platforms: plat(&components.Build{DownloadSize: int64(len(ov)), InstalledSize: int64(len(ov)),
					Files: []components.File{file("model.xml", "{models}/ov-small/model.xml", ov)}})},
			{ID: tcGone, Version: "1.0.0", Kind: components.KindModel, Backend: "openvino", Model: "huge", Precision: "fp16",
				Unavailable: "no_hash", Platforms: plat(&components.Build{})},
		},
		Presets: components.Presets{
			Default: "standard",
			Tiers: []components.PresetTier{
				{ID: "standard", NameKey: "n", DescKey: "d", CostKey: "c", Roles: map[string]components.PresetRole{"translate": {Provider: "deepseek"}}},
				{ID: "transcribe_only", NameKey: "n", DescKey: "d", CostKey: "c", TranscribeOnly: true},
			},
			Providers: []components.PresetProvider{
				{ID: "deepseek", Name: "DeepSeek", Type: "openai", BaseURL: "https://api.deepseek.com", DefaultModel: "deepseek-flash",
					SignupURL: "https://example.invalid/s", KeysURL: "https://example.invalid/k", StepsKeys: []string{"s1"}, CostKey: "c",
					BalanceAPI: "deepseek"},
				{ID: "gemini", Name: "Google Gemini", Type: "openai", BaseURL: "https://gemini.example.invalid/v1beta/openai", DefaultModel: "g",
					Free: true, TermsNotice: true, TermsURL: "https://example.invalid/terms", MaxConcurrency: 1, RPM: 5,
					SignupURL: "https://example.invalid/s", KeysURL: "https://example.invalid/k", StepsKeys: []string{"s1"}, CostKey: "c"},
			},
			Free: components.FreeChoice{Default: "gemini"},
		},
	}
	b, err := json.MarshalIndent(&man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "components.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newCompEnv builds the env; tweak adjusts the server (desktop mode, state).
func newCompEnv(t *testing.T, tweak func(*Server, *compState)) *compEnv {
	t.Helper()
	ce := &compEnv{fs: newFileServer(t), data: t.TempDir()}
	ce.free.Store(1 << 40)
	manPath := writeTestManifest(t, ce)
	m, err := components.New(components.Options{
		DataDir: ce.data, AppVersion: "3.0.0", Platform: components.PlatformWindowsX64, BuiltinManifest: manPath,
		DiskFree:     func(string) (int64, error) { return ce.free.Load(), nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBackoff: 5 * time.Millisecond, StallTimeout: 5 * time.Second, EventInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	ce.m = m
	ce.env = newEnvWith(t, envConfig{server: func(s *Server) {
		s.comps = m
		cs := s.compState()
		cs.region = func() string { return "" }
		cs.getenv = func(string) string { return "" }
		if tweak != nil {
			tweak(s, cs)
		}
	}})
	return ce
}

func (ce *compEnv) tasks() []ComponentTaskView {
	var out []ComponentTaskView
	ce.call("GET", "/api/components/tasks", nil, http.StatusOK, &out)
	return out
}

func (ce *compEnv) task(id string) ComponentTaskView {
	for _, t := range ce.tasks() {
		if t.ID == id {
			return t
		}
	}
	ce.t.Fatalf("task %s not listed", id)
	return ComponentTaskView{}
}

// waitTask polls the task until it is in one of the states.
func (ce *compEnv) waitTask(id string, states ...string) ComponentTaskView {
	ce.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var t ComponentTaskView
	for time.Now().Before(deadline) {
		t = ce.task(id)
		for _, s := range states {
			if t.State == s {
				return t
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	ce.t.Fatalf("task %s: state %s, want %v (error %+v)", id, t.State, states, t.Error)
	return t
}

func (ce *compEnv) install(ids ...string) ComponentTaskView {
	ce.t.Helper()
	var res ComponentTaskResult
	ce.call("POST", "/api/components/install", map[string]any{"ids": ids}, http.StatusAccepted, &res)
	return res.Task
}

func (ce *compEnv) list() ComponentsView {
	var v ComponentsView
	ce.call("GET", "/api/components", nil, http.StatusOK, &v)
	return v
}

func (ce *compEnv) comp(id string) ComponentView {
	for _, c := range ce.list().Components {
		if c.ID == id {
			return c
		}
	}
	ce.t.Fatalf("component %s not listed", id)
	return ComponentView{}
}

func TestComponentsNilManager503(t *testing.T) {
	e := newEnv(t)
	for _, c := range [][2]string{
		{"GET", "/api/components"}, {"POST", "/api/components/plan"}, {"POST", "/api/components/install"},
		{"POST", "/api/components/import"}, {"GET", "/api/components/tasks"}, {"POST", "/api/components/tasks/t1/pause"},
		{"GET", "/api/components/sources"}, {"PUT", "/api/components/sources"}, {"GET", "/api/components/presets"},
		{"POST", "/api/components/tool/update"}, {"DELETE", "/api/components/tool"},
	} {
		info := e.callErrInfo(c[0], c[1], map[string]any{}, http.StatusServiceUnavailable)
		if info.Code != "server.unavailable" || info.Params["service"] != "components" {
			t.Errorf("%s %s: %+v", c[0], c[1], info)
		}
	}
}

func TestComponentsListAndPresets(t *testing.T) {
	ce := newCompEnv(t, nil)
	v := ce.list()
	if v.Platform != "windows-x64" || v.Manifest.AppVersion != "3.0.0" || v.Manifest.PublishedAt == "" || v.Manifest.UpdateAvailable {
		t.Fatalf("header: %+v %+v", v.Platform, v.Manifest)
	}
	want := map[string]string{components.IDCore: "installed", tcTool: "available", tcTiny: "available", tcGone: "unavailable"}
	for _, c := range v.Components {
		if s, ok := want[c.ID]; ok && c.State != s {
			t.Errorf("%s: state %s, want %s", c.ID, c.State, s)
		}
		if c.Requires == nil {
			t.Errorf("%s: requires is null", c.ID)
		}
		if c.ID == tcTiny && (c.Kind != "model" || c.Backend != "faster-whisper" || c.Precision != "int8" ||
			c.DownloadBytes != int64(len(ce.content[tcTiny])) || len(c.Requires) != 1 || c.Requires[0] != tcTool) {
			t.Errorf("tiny: %+v", c)
		}
		if c.ID == tcGone && c.UnavailableReason != "no_hash" {
			t.Errorf("gone: %+v", c)
		}
	}
	if v.Tasks == nil {
		t.Error("tasks is null")
	}
	var p components.Presets
	ce.call("GET", "/api/components/presets", nil, http.StatusOK, &p)
	if p.Default != "standard" || len(p.Tiers) != 2 || len(p.Providers) != 2 {
		t.Fatalf("presets: %+v", p)
	}
}

func TestComponentsPlan(t *testing.T) {
	ce := newCompEnv(t, nil)
	var p ComponentPlanView
	ce.call("POST", "/api/components/plan", map[string]any{"ids": []string{tcTiny}}, http.StatusOK, &p)
	if len(p.IDs) != 2 || p.IDs[0] != tcTool || p.IDs[1] != tcTiny || !p.EnoughSpace ||
		p.DownloadBytes != int64(len(ce.content[tcTool])+len(ce.content[tcTiny])) || p.FreeBytes != 1<<40 || p.NeedBytes <= p.DownloadBytes {
		t.Fatalf("plan: %+v", p)
	}
	// not enough space: the plan says so, install refuses with 409
	ce.free.Store(1 << 20)
	ce.call("POST", "/api/components/plan", map[string]any{"ids": []string{tcTiny}}, http.StatusOK, &p)
	if p.EnoughSpace || p.FreeBytes != 1<<20 {
		t.Fatalf("plan without space: %+v", p)
	}
	info := ce.callErrInfo("POST", "/api/components/install", map[string]any{"ids": []string{tcTiny}}, http.StatusConflict)
	if info.Code != components.CodeDiskFull || info.Params["free_mb"] != float64(1) {
		t.Fatalf("install without space: %+v", info)
	}
	// unknown, unavailable, malformed
	if c := ce.callErrInfo("POST", "/api/components/plan", map[string]any{"ids": []string{"nope"}}, http.StatusBadRequest).Code; c != components.CodeUnknown {
		t.Errorf("unknown: %s", c)
	}
	if c := ce.callErrInfo("POST", "/api/components/plan", map[string]any{"ids": []string{tcGone}}, http.StatusBadRequest).Code; c != components.CodeUnavailable {
		t.Errorf("unavailable: %s", c)
	}
	if c := ce.callErrInfo("POST", "/api/components/plan", map[string]any{"ids": []string{}}, http.StatusBadRequest).Code; c != "request.bad_param" {
		t.Errorf("empty: %s", c)
	}
	if c := ce.callErrInfo("POST", "/api/components/plan", map[string]any{"ids": []string{"../x"}}, http.StatusBadRequest).Code; c != components.CodeUnknown {
		t.Errorf("bad id: %s", c)
	}
}

func TestComponentsInstallProgressDone(t *testing.T) {
	ce := newCompEnv(t, nil)
	ce.fs.mu.Lock()
	ce.fs.slow["/default/hf/tiny/model.bin"] = 5 * time.Millisecond
	ce.fs.mu.Unlock()
	task := ce.install(tcTiny)
	if task.Kind != "install" || len(task.IDs) != 1 || task.IDs[0] != tcTiny || len(task.Items) != 2 || task.Notes == nil {
		t.Fatalf("task: %+v", task)
	}
	// progress: some bytes while the model downloads
	sawProgress := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		tk := ce.task(task.ID)
		if tk.DoneBytes > 0 && tk.DoneBytes < tk.TotalBytes {
			sawProgress = true
		}
		if tk.State == "done" || tk.State == "failed" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	done := ce.waitTask(task.ID, "done")
	if !sawProgress {
		t.Error("never saw partial progress")
	}
	if done.DoneBytes != done.TotalBytes || done.TotalBytes != int64(len(ce.content[tcTool])+len(ce.content[tcTiny])) {
		t.Errorf("bytes: %+v", done)
	}
	for _, it := range done.Items {
		if it.State != "installed" || it.Source != "default" {
			t.Errorf("item: %+v", it)
		}
	}
	if c := ce.comp(tcTiny); c.State != "installed" || c.InstalledVersion != "1.0.0" || c.DiskBytes == 0 {
		t.Errorf("tiny after install: %+v", c)
	}
	// again: nothing to do
	if c := ce.callErrInfo("POST", "/api/components/install", map[string]any{"ids": []string{tcTiny}}, http.StatusConflict).Code; c != components.CodeUpToDate {
		t.Errorf("reinstall: %s", c)
	}
	// update of an up-to-date component, of one not installed
	if c := ce.callErrInfo("POST", "/api/components/"+tcTiny+"/update", nil, http.StatusConflict).Code; c != components.CodeUpToDate {
		t.Errorf("update: %s", c)
	}
	if c := ce.callErrInfo("POST", "/api/components/"+url.PathEscape(tcBig)+"/update", nil, http.StatusNotFound).Code; c != components.CodeNotInstalled {
		t.Errorf("update not installed: %s", c)
	}
}

func TestComponentsUninstallInUse(t *testing.T) {
	ce := newCompEnv(t, nil)
	ce.waitTask(ce.install(tcTiny).ID, "done")
	info := ce.callErrInfo("DELETE", "/api/components/"+tcTool, nil, http.StatusConflict)
	if info.Code != components.CodeInUse || info.Params["dependents"] != tcTiny {
		t.Fatalf("in use: %+v", info)
	}
	if c := ce.callErrInfo("DELETE", "/api/components/core", nil, http.StatusConflict).Code; c != components.CodeBundled {
		t.Errorf("core: %s", c)
	}
	if c := ce.callErrInfo("DELETE", "/api/components/"+url.PathEscape("Bad Id"), nil, http.StatusNotFound).Code; c != components.CodeUnknown {
		t.Errorf("bad id: %s", c)
	}
	// the model id with escaped colons, then the tool
	ce.call("DELETE", "/api/components/model%3Afaster-whisper%3Atiny%3Aint8", nil, http.StatusNoContent, nil)
	ce.call("DELETE", "/api/components/"+tcTool, nil, http.StatusNoContent, nil)
	if c := ce.comp(tcTool); c.State != "available" {
		t.Errorf("tool after uninstall: %+v", c)
	}
	if c := ce.callErrInfo("DELETE", "/api/components/"+tcTool, nil, http.StatusNotFound).Code; c != components.CodeNotInstalled {
		t.Errorf("twice: %s", c)
	}
}

func TestComponentsPauseResumeCancelRetry(t *testing.T) {
	ce := newCompEnv(t, nil)
	hit, release := ce.fs.gate("/hf/big/model.bin")
	defer release()
	task := ce.install(tcBig)
	<-hit
	ce.waitTask(task.ID, "running")
	var res ComponentTaskResult
	ce.call("POST", "/api/components/tasks/"+task.ID+"/pause", nil, http.StatusAccepted, &res)
	p := ce.waitTask(task.ID, "paused")
	if p.DoneBytes == 0 {
		t.Errorf("paused without the first block: %+v", p)
	}
	if p.Items[0].State != "paused" {
		t.Errorf("item while paused: %+v", p.Items[0])
	}
	// pause again: wrong state
	if c := ce.callErrInfo("POST", "/api/components/tasks/"+task.ID+"/pause", nil, http.StatusConflict).Code; c != components.CodeTaskState {
		t.Errorf("pause twice: %s", c)
	}
	ce.call("POST", "/api/components/tasks/"+task.ID+"/resume", nil, http.StatusAccepted, &res)
	ce.waitTask(task.ID, "done")

	// cancel a running task
	ce.call("DELETE", "/api/components/"+url.PathEscape(tcBig), nil, http.StatusNoContent, nil)
	hit2, release2 := ce.fs.gate("/hf/big/model.bin")
	defer release2()
	task2 := ce.install(tcBig)
	<-hit2
	ce.call("POST", "/api/components/tasks/"+task2.ID+"/cancel", nil, http.StatusAccepted, &res)
	c := ce.waitTask(task2.ID, "cancelled")
	if c.Error == nil || c.Error.Code != components.CodeCancelled {
		t.Errorf("cancelled: %+v", c)
	}
	// a failed task (HTTP 404 from the server), retried with new sources in the body
	ce.fs.setStatus("/default/hf/tiny", http.StatusNotFound)
	task3 := ce.install(tcTiny)
	f := ce.waitTask(task3.ID, "failed")
	if f.Error == nil || f.Error.Code != components.CodeHTTPStatus {
		t.Fatalf("failed: %+v", f.Error)
	}
	ce.fs.setStatus("/default/hf/tiny", 0)
	if code := ce.callErrInfo("POST", "/api/components/tasks/"+task3.ID+"/retry",
		map[string]any{"models": map[string]any{"mode": "mirror", "mirror_id": "nope"}}, http.StatusBadRequest).Code; code != "components.sources_invalid" {
		t.Errorf("retry with a bad source: %s", code)
	}
	ce.call("POST", "/api/components/tasks/"+task3.ID+"/retry",
		map[string]any{"models": map[string]any{"mode": "mirror", "mirror_id": "hf-mirror"}}, http.StatusAccepted, &res)
	d := ce.waitTask(task3.ID, "done")
	for _, it := range d.Items {
		if it.ID == tcTiny && it.Source != "mirror" {
			t.Errorf("retried item source: %+v", it)
		}
	}
	// the retry did not store the sources
	var sv SourcesView
	ce.call("GET", "/api/components/sources", nil, http.StatusOK, &sv)
	if sv.Models.Mode != "default" {
		t.Errorf("sources after retry: %+v", sv.Models)
	}
	// unknown task, unknown action
	if code := ce.callErrInfo("POST", "/api/components/tasks/t999/resume", nil, http.StatusNotFound).Code; code != components.CodeTaskNotFound {
		t.Errorf("unknown task: %s", code)
	}
	if code := ce.callErrInfo("POST", "/api/components/tasks/"+task3.ID+"/explode", nil, http.StatusNotFound).Code; code != "api.not_found" {
		t.Errorf("unknown action: %s", code)
	}
}

// offlineZip builds an archive whose component.json names id/version.
func offlineZip(t *testing.T, id, version string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("component.json")
	fmt.Fprintf(w, `{"schema":1,"id":%q,"version":%q,"platform":"windows-x64"}`, id, version)
	w, _ = zw.Create("files/tool.bin")
	w.Write([]byte("tool binary v1"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestComponentsImportChecksumWeb(t *testing.T) {
	ce := newCompEnv(t, nil)
	res, b := ce.upload("/api/components/import", nil, "tool.zip", offlineZip(t, tcTool, "1.0.0"))
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	var r ComponentTaskResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	f := ce.waitTask(r.Task.ID, "failed")
	if f.Kind != "import" || f.Error == nil || f.Error.Code != components.CodeImportChecksum || f.Error.Params["id"] != tcTool {
		t.Fatalf("import task: %+v", f)
	}
	dir := importUploadDir(ce.srv.dataDir)
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("a failed import keeps its upload for retry: %v", ents)
	}
	// cancelling the failed task removes the upload
	ce.call("POST", "/api/components/tasks/"+r.Task.ID+"/cancel", nil, http.StatusAccepted, nil)
	waitUntil(t, "upload removed", func() bool { ents, _ := os.ReadDir(dir); return len(ents) == 0 })
	// not a zip by name
	if res, b := ce.upload("/api/components/import", nil, "tool.exe", []byte("x")); res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), components.CodeImportInvalid) {
		t.Errorf("not a zip: %d %s", res.StatusCode, b)
	}
	// web mode: a JSON path is refused
	if c := ce.callErrInfo("POST", "/api/components/import", map[string]any{"path": `C:\x.zip`}, http.StatusBadRequest).Code; c != "upload.not_multipart" {
		t.Errorf("path in web mode: %s", c)
	}
}

func TestComponentsImportDesktopPath(t *testing.T) {
	d := &DesktopOptions{Token: testToken}
	ce := newCompEnv(t, func(s *Server, _ *compState) { s.desktop = d })
	u, _ := url.Parse(ce.ts.URL)
	d.Port, _ = strconv.Atoi(u.Port())
	zipPath := filepath.Join(t.TempDir(), "tool.zip")
	if err := os.WriteFile(zipPath, offlineZip(t, tcTool, "1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": zipPath})
	ce.grant(zipPath) // picked in the main process's dialog (T-SEC-08)
	res, b := ce.doWith("POST", "/api/components/import", body, testToken, "")
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	var r ComponentTaskResult
	json.Unmarshal(b, &r)
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, ok := ce.m.Task(components.TaskID(r.Task.ID))
		if ok && st.State == components.TaskFailed {
			if st.Error.Code != components.CodeImportChecksum {
				t.Fatalf("error: %+v", st.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Errorf("the picked file must stay: %v", err)
	}
	body, _ = json.Marshal(map[string]string{"path": "relative.zip"})
	if res, b := ce.doWith("POST", "/api/components/import", body, testToken, ""); res.StatusCode != http.StatusBadRequest || errCode(t, b) != "upload.path_invalid" {
		t.Errorf("relative path: %d %s", res.StatusCode, b)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestComponentsSources(t *testing.T) {
	region := "cn"
	ce := newCompEnv(t, func(_ *Server, cs *compState) { cs.region = func() string { return region } })
	var v SourcesView
	ce.call("GET", "/api/components/sources", nil, http.StatusOK, &v)
	if v.PyPI.Mode != "default" || v.Models.Mode != "default" || !v.SuggestMirror ||
		v.Suggested.PyPI.MirrorID != "tuna" || v.Suggested.Models.MirrorID != "hf-mirror" {
		t.Fatalf("sources: %+v", v)
	}
	if len(v.Options["pypi"]) != 1 || v.Options["pypi"][0].ID != "tuna" || v.Options["pypi"][0].Label != "pypi.tuna.example.invalid" ||
		v.Options["models"][0].NameKey != "compui.source.name.hf_mirror" {
		t.Fatalf("options: %+v", v.Options)
	}
	in := SourcesSetting{PyPI: SourceChoice{Mode: "mirror", MirrorID: "tuna"}, Models: SourceChoice{Mode: "mirror", CustomURL: ce.fs.URL + "/broken"}}
	ce.call("PUT", "/api/components/sources", in, http.StatusOK, &v)
	if v.PyPI != in.PyPI || v.Models != in.Models {
		t.Fatalf("put: %+v", v)
	}
	region = ""
	ce.call("GET", "/api/components/sources", nil, http.StatusOK, &v)
	if v.PyPI != in.PyPI || v.Models != in.Models || v.SuggestMirror || v.Suggested.PyPI.Mode != "default" {
		t.Fatalf("stored: %+v", v)
	}
	if got := toManagerSources(SourcesSetting{PyPI: v.PyPI, Models: v.Models}); got.PyPI != "tuna" || got.Models != ce.fs.URL+"/broken" {
		t.Errorf("manager sources: %+v", got)
	}
	for _, bad := range []struct {
		body  SourcesSetting
		field string
	}{
		{SourcesSetting{PyPI: SourceChoice{Mode: "fast"}}, "mode"},
		{SourcesSetting{PyPI: SourceChoice{Mode: "mirror", MirrorID: "hf-mirror"}}, "mirror_id"},
		{SourcesSetting{Models: SourceChoice{Mode: "mirror", CustomURL: "ftp://x"}}, "custom_url"},
		{SourcesSetting{Models: SourceChoice{Mode: "mirror"}}, "custom_url"},
	} {
		info := ce.callErrInfo("PUT", "/api/components/sources", bad.body, http.StatusBadRequest)
		if info.Code != "components.sources_invalid" || info.Params["field"] != bad.field {
			t.Errorf("%+v: %+v", bad.body, info)
		}
	}
	// installs use the stored sources: the custom address does not
	// answer, so the task falls back to the default source with a note
	tk := ce.waitTask(ce.install(tcOV).ID, "done")
	if tk.Items[0].Source != "default" || len(tk.Notes) != 1 || tk.Notes[0].Code != components.CodeMirrorFallback ||
		tk.Notes[0].Params["group"] != "models" {
		t.Errorf("install with a stored custom URL: %+v %+v", tk.Items[0], tk.Notes)
	}
}

// Security audit M-1: a custom package index must be https, plain http
// only for this computer; the models group keeps http.
func TestComponentsSourcesInsecureIndex(t *testing.T) {
	ce := newCompEnv(t, nil)
	custom := func(u string) SourceChoice { return SourceChoice{Mode: "mirror", CustomURL: u} }
	for _, u := range []string{"http://pypi.example.com/simple", "HTTP://pypi.example.com/simple",
		"http://192.0.2.10:8080/simple", "http://127.0.0.1@pypi.example.com/simple", "http://localhost.example.com/simple"} {
		info := ce.callErrInfo("PUT", "/api/components/sources", SourcesSetting{PyPI: custom(u)}, http.StatusBadRequest)
		if info.Code != "components.sources_invalid" || info.Params["group"] != "pypi" || info.Params["field"] != "custom_url" {
			t.Errorf("%s: %+v", u, info)
		}
	}
	for _, u := range []string{"https://pypi.example.com/simple", "http://127.0.0.1/simple", "http://127.0.0.1:3141/simple",
		"http://[::1]:3141/simple", "http://LocalHost/simple"} {
		var v SourcesView
		ce.call("PUT", "/api/components/sources", SourcesSetting{PyPI: custom(u)}, http.StatusOK, &v)
		if v.PyPI.CustomURL != u {
			t.Errorf("%s: %+v", u, v.PyPI)
		}
	}
	var v SourcesView
	ce.call("PUT", "/api/components/sources", SourcesSetting{Models: custom("http://hf.example.com")}, http.StatusOK, &v)
	// a retry body may not switch to such an index either
	tk := ce.install(tcOV)
	ce.waitTask(tk.ID, "done", "failed")
	info := ce.callErrInfo("POST", "/api/components/tasks/"+tk.ID+"/retry",
		map[string]any{"pypi": custom("http://pypi.example.com/simple")}, http.StatusBadRequest)
	if info.Code != "components.sources_invalid" || info.Params["field"] != "custom_url" {
		t.Errorf("retry: %+v", info)
	}

	// an http index saved before the rule: read as it is (the page shows
	// it, the setting is not rewritten), refused when an install uses it
	legacy := `{"pypi":{"mode":"mirror","custom_url":"http://pypi.example.com/simple"},"models":{"mode":"default"}}`
	if err := ce.st.SetKV(context.Background(), kvComponentsSources, legacy); err != nil {
		t.Fatal(err)
	}
	ce.call("GET", "/api/components/sources", nil, http.StatusOK, &v)
	if v.PyPI.Mode != "mirror" || v.PyPI.CustomURL != "http://pypi.example.com/simple" {
		t.Errorf("stored legacy index: %+v", v.PyPI)
	}
	before := len(ce.tasks())
	info = ce.callErrInfo("POST", "/api/components/install", map[string]any{"ids": []string{tcTiny}}, http.StatusBadRequest)
	if info.Code != "components.sources_invalid" || info.Params["group"] != "pypi" || info.Params["field"] != "custom_url" {
		t.Errorf("install with a stored http index: %+v", info)
	}
	if len(ce.tasks()) != before {
		t.Error("a task started with an http package index")
	}
	if got, _, _ := ce.st.GetKV(context.Background(), kvComponentsSources); got != legacy {
		t.Errorf("the stored setting changed: %s", got)
	}
}

func TestRegionFrom(t *testing.T) {
	for _, c := range []struct{ locale, tz, want string }{
		{"zh-CN", "", "cn"}, {"zh_CN.UTF-8", "", "cn"}, {"zh-HK", "China Standard Time", ""}, {"zh-TW", "Asia/Shanghai", ""},
		{"en-US", "", ""},
	} {
		if got := regionFrom(c.locale, c.tz); got != c.want {
			t.Errorf("regionFrom(%q, %q) = %q, want %q", c.locale, c.tz, got, c.want)
		}
	}
	_ = systemRegion() // runs on this machine without failing
}

// sseEvents reads "event: components" payloads with their arrival times.
func sseComponents(t *testing.T, ctx context.Context, base string, out chan<- ComponentsEvent, at chan<- time.Time) {
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ev := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && ev == "components":
			var p ComponentsEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &p); err != nil {
				t.Errorf("components event: %v", err)
				return
			}
			at <- time.Now()
			out <- p
		}
	}
}

func TestComponentsSSEThrottled(t *testing.T) {
	const interval = 200 * time.Millisecond
	ce := newCompEnv(t, func(_ *Server, cs *compState) { cs.interval = interval })
	ce.fs.mu.Lock()
	ce.fs.slow["/default/hf/big/model.bin"] = 15 * time.Millisecond // ~40 blocks: ~0.6 s of progress
	ce.fs.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, ats := make(chan ComponentsEvent, 1000), make(chan time.Time, 1000)
	go sseComponents(t, ctx, ce.ts.URL, evs, ats)
	waitUntil(t, "SSE client", func() bool { return ce.hub.Clients() > 0 })
	task := ce.install(tcBig)
	var times []time.Time
	var last ComponentsEvent
	deadline := time.After(20 * time.Second)
	for done := false; !done; {
		select {
		case at := <-ats:
			times = append(times, at)
			last = <-evs
			for _, tk := range last.Tasks {
				if tk.ID == task.ID && tk.State == "done" {
					done = true
				}
			}
		case <-deadline:
			t.Fatalf("no final components event; %d events", len(times))
		}
	}
	if len(times) < 2 {
		t.Fatalf("only %d events", len(times))
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < interval-50*time.Millisecond {
			t.Errorf("events %d and %d only %v apart", i-1, i, gap)
		}
	}
}

func TestHardwareCacheAndRedetect(t *testing.T) {
	var calls atomic.Int32
	hw := hwdetect.HW{OS: "windows", Arch: "amd64", CPU: "Test CPU", RAMMB: 16384}
	ce := newCompEnv(t, func(_ *Server, cs *compState) {
		cs.detect = func(ctx context.Context, dataDir string) (hwdetect.HW, error) {
			calls.Add(1)
			return hw, nil
		}
	})
	var v HardwareView
	ce.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	ce.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	if calls.Load() != 1 {
		t.Fatalf("detections: %d, want 1 (cached)", calls.Load())
	}
	if v.HW.CPU != "Test CPU" || v.HW.NVIDIA == nil {
		t.Fatalf("hw: %+v", v.HW)
	}
	// CPU only on Windows: python, asr-openvino, model:openvino:small:int8;
	// only the model is in the test manifest, so the download is its size.
	if len(v.Recommendation.Components) != 3 || v.Recommendation.Components[2] != tcOV ||
		v.Recommendation.DownloadBytes != int64(len(ce.content[tcOV])) || len(v.Recommendation.Notes) == 0 {
		t.Fatalf("recommendation: %+v", v.Recommendation)
	}
	hw.CPU = "Other CPU"
	ce.call("POST", "/api/hardware/detect", nil, http.StatusOK, &v)
	if calls.Load() != 2 || v.HW.CPU != "Other CPU" {
		t.Fatalf("redetect: %d %+v", calls.Load(), v.HW)
	}
	ce.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	if calls.Load() != 2 || v.HW.CPU != "Other CPU" {
		t.Fatalf("after redetect: %d %+v", calls.Load(), v.HW)
	}
	// installed: nothing left to download
	ce.waitTask(ce.install(tcOV).ID, "done")
	ce.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	if v.Recommendation.DownloadBytes != 0 {
		t.Errorf("download after install: %d", v.Recommendation.DownloadBytes)
	}
}

func TestHardwareWithoutComponents(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.compState().detect = func(context.Context, string) (hwdetect.HW, error) {
			return hwdetect.HW{OS: "linux", Arch: "amd64"}, nil
		}
	})
	var v HardwareView
	e.call("GET", "/api/hardware", nil, http.StatusOK, &v)
	if v.HW.OS != "linux" || v.Recommendation.Reasons == nil || v.Recommendation.Notes == nil || v.Recommendation.DownloadBytes != 0 {
		t.Fatalf("%+v", v)
	}
}
