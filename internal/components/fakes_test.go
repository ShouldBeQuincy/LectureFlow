// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/pyenv"
)

// The test binary doubles as a fake uv and a fake python: TestMain copies it
// to {fakeBin}/uv(.exe); the fake "uv venv" copies it into the venv as
// python(.exe). Behaviour switches (environment variables, set with
// t.Setenv and inherited by the child processes):
//
//	LF_FAKE_UV_FAIL_INDEX=sub   uv pip sync fails like a network error when --index-url contains sub
//	LF_FAKE_UV_FAIL=sync        uv pip sync fails
//	LF_FAKE_UV_SLEEP_MS=n       uv pip sync sleeps first (pause / cancel while uv runs)
//	LF_FAKE_UV_LOG=file         each uv call appends its arguments as one JSON line
//	LF_FAKE_UV_HASH_FAIL=file   file holds a count n: the next n uv pip sync calls fail with uv's
//	                            "Hash mismatch for `pkg-a==1.0.0`" (decremented per call)
//	LF_FAKE_UV_SDIST_ONLY=file  the index offers source distributions only: uv pip sync with
//	                            --no-build fails like uv 0.13.1 ("has no usable wheels"); without
//	                            it the fake "runs the build backend" (writes file) and installs
//	LF_FAKE_PY_FAIL=module      python -c "import ..." fails when it imports module
//	LF_FAKE_PY_LOG=file         each python call appends {"args", "telemetry_off": pyenv.Has(its environment)}
var fakeBin string

func TestMain(m *testing.M) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	switch base {
	case "uv":
		os.Exit(fakeUV(os.Args[1:]))
	case "python", "python3":
		os.Exit(fakePython(os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "lfcomp-fakebin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	self, _ := os.Executable()
	if err := copyFile(self, filepath.Join(dir, "uv"+exeSuffix)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBin = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

func fakeUVPath() string { return filepath.Join(fakeBin, "uv"+exeSuffix) }

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func fakeUV(args []string) int {
	if p := os.Getenv("LF_FAKE_UV_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			b, _ := json.Marshal(args)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "cache":
		if len(args) < 2 || args[1] != "clean" {
			return 2
		}
		if cache := os.Getenv("UV_CACHE_DIR"); cache != "" {
			os.Remove(filepath.Join(cache, "fake-wheel.bin"))
		}
		return 0
	case "venv":
		py := flagValue(args, "--python")
		dir := args[len(args)-1]
		if fi, err := os.Stat(py); err != nil || fi.IsDir() {
			fmt.Fprintf(os.Stderr, "error: No interpreter found at %s\n", py)
			return 2
		}
		self, _ := os.Executable()
		vp := venvPython(dir)
		if err := os.MkdirAll(filepath.Dir(vp), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := copyFile(self, vp); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		cfg := "home = " + filepath.Dir(py) + "\nimplementation = CPython\nuv = fake\nversion_info = 3.12.15\n"
		if err := os.WriteFile(filepath.Join(dir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
			return 2
		}
		return 0
	case "pip":
		if len(args) < 2 || args[1] != "sync" {
			return 2
		}
		if ms, _ := strconv.Atoi(os.Getenv("LF_FAKE_UV_SLEEP_MS")); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		index := flagValue(args, "--index-url")
		if sub := os.Getenv("LF_FAKE_UV_FAIL_INDEX"); sub != "" && index != "" && strings.Contains(index, sub) {
			fmt.Fprintf(os.Stderr, "error: Failed to fetch: `%s/pkg-a/`\n  Caused by: error sending request for url (%s)\n", index, index)
			return 2
		}
		if p := os.Getenv("LF_FAKE_UV_HASH_FAIL"); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
					os.WriteFile(p, []byte(strconv.Itoa(n-1)), 0o644)
					// uv 0.13.1's message (checked with a local wheel)
					fmt.Fprintf(os.Stderr, "error: Failed to download `pkg-a==1.0.0`\n  cause: Hash mismatch for `pkg-a==1.0.0`\n\n"+
						"         Expected:\n           sha256:%s\n\n         Computed:\n           sha256:%s\n",
						strings.Repeat("a", 64), strings.Repeat("f", 64))
					return 1
				}
			}
		}
		if marker := os.Getenv("LF_FAKE_UV_SDIST_ONLY"); marker != "" {
			if hasFlag(args, "--no-build") {
				// uv 0.13.1's message (checked with a local sdist-only find-links directory)
				fmt.Fprint(os.Stderr, "error: No solution found when resolving dependencies\n"+
					"  cause: Because pkg-a==1.0.0 has no usable wheels and you require pkg-a==1.0.0, "+
					"we can conclude that your requirements are unsatisfiable.\n\n"+
					"hint: Wheels are required for `pkg-a` because building from source is disabled "+
					"for all packages (i.e., with `--no-build`)\n")
				return 1
			}
			os.WriteFile(marker, []byte("build backend ran"), 0o644)
		}
		if os.Getenv("LF_FAKE_UV_FAIL") == "sync" {
			fmt.Fprintln(os.Stderr, "error: something unexpected broke")
			return 1
		}
		lock := args[len(args)-1]
		reqs, err := parseLock(lock)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		if hasFlag(args, "--require-hashes") {
			for _, r := range reqs {
				if r.hashes == 0 {
					fmt.Fprintf(os.Stderr, "error: In `--require-hashes` mode, all requirements must have a hash, but none were provided for: %s\n", r.name)
					return 2
				}
			}
		}
		if links := flagValue(args, "--find-links"); links != "" {
			for _, r := range reqs {
				ms, _ := filepath.Glob(filepath.Join(links, strings.ReplaceAll(r.name, "-", "_")+"-"+r.version+"-*.whl"))
				if len(ms) == 0 {
					fmt.Fprintf(os.Stderr, "error: Because %s was not found in the provided package locations\n", r.name)
					return 1
				}
			}
			index = "find-links"
		}
		py := flagValue(args, "--python")
		venv := filepath.Dir(filepath.Dir(py))
		site := filepath.Join(venv, "Lib", "site-packages")
		if runtime.GOOS != "windows" {
			site = filepath.Join(venv, "lib", "python3.12", "site-packages")
		}
		os.MkdirAll(site, 0o755)
		var b strings.Builder
		for _, r := range reqs {
			fmt.Fprintf(&b, "%s==%s\n", r.name, r.version)
		}
		fmt.Fprintf(&b, "index=%s\n", index)
		if err := os.WriteFile(filepath.Join(site, "fake_installed.txt"), []byte(b.String()), 0o644); err != nil {
			return 2
		}
		if cache := os.Getenv("UV_CACHE_DIR"); cache != "" {
			os.MkdirAll(cache, 0o755)
			os.WriteFile(filepath.Join(cache, "fake-wheel.bin"), []byte("cached"), 0o644)
		}
		return 0
	}
	return 2
}

type lockReq struct {
	name, version string
	hashes        int
}

func parseLock(p string) ([]lockReq, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var out []lockReq
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
		case strings.HasPrefix(t, "--hash=sha256:"):
			if len(out) == 0 {
				return nil, fmt.Errorf("hash without requirement")
			}
			out[len(out)-1].hashes++
		default:
			name, ver, ok := strings.Cut(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(t, `\`)), " "), "==")
			if !ok {
				return nil, fmt.Errorf("unpinned requirement %q", t)
			}
			out = append(out, lockReq{name: strings.TrimSpace(name), version: strings.TrimSpace(ver)})
		}
	}
	return out, nil
}

func fakePython(args []string) int {
	if p := os.Getenv("LF_FAKE_PY_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			b, _ := json.Marshal(map[string]any{"args": args, "telemetry_off": pyenv.Has(os.Environ())})
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if len(args) >= 3 && args[0] == "-I" && args[1] == "-c" {
		if bad := os.Getenv("LF_FAKE_PY_FAIL"); bad != "" && strings.Contains(args[2], bad) {
			fmt.Fprintf(os.Stderr, "ModuleNotFoundError: No module named '%s'\n", bad)
			return 1
		}
		return 0
	}
	return 0
}

// ---------------------------------------------------------------------------
// Fake HTTP server
// ---------------------------------------------------------------------------

type reqLog struct {
	Path  string
	Range string
}

type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	files    map[string][]byte // path → content
	status   map[string]int    // path prefix → status for every request
	drop     map[string]int64  // path → abort after this many body bytes (once)
	corrupt  map[string]int    // path → serve damaged content this many times
	gates    map[string]chan struct{}
	gateHit  map[string]chan struct{} // closed when a gated request has sent its first chunk
	noRange  bool
	requests []reqLog
}

func newFakeServer(t *testing.T) *fakeServer {
	fs := &fakeServer{files: map[string][]byte{}, status: map[string]int{}, drop: map[string]int64{},
		corrupt: map[string]int{}, gates: map[string]chan struct{}{}, gateHit: map[string]chan struct{}{}}
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

// put serves content under /default{p} and /mirror{p}.
func (fs *fakeServer) put(p string, content []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files["/default"+p] = content
	fs.files["/mirror"+p] = content
}

func (fs *fakeServer) putExact(p string, content []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files[p] = content
}

// gate makes the next request for path stop after its first chunk until
// release is called; hit is closed once that chunk was sent.
func (fs *fakeServer) gate(path string) (hit <-chan struct{}, release func()) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	g := make(chan struct{})
	h := make(chan struct{})
	fs.gates[path] = g
	fs.gateHit[path] = h
	var once sync.Once
	return h, func() {
		once.Do(func() {
			fs.mu.Lock()
			if fs.gates[path] == g {
				delete(fs.gates, path)
			}
			fs.mu.Unlock()
			close(g)
		})
	}
}

func (fs *fakeServer) log() []reqLog {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]reqLog(nil), fs.requests...)
}

func (fs *fakeServer) count(prefix string) int {
	n := 0
	for _, r := range fs.log() {
		if strings.HasPrefix(r.Path, prefix) {
			n++
		}
	}
	return n
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	fs.requests = append(fs.requests, reqLog{Path: r.URL.Path, Range: r.Header.Get("Range")})
	for pre, st := range fs.status {
		if strings.HasPrefix(r.URL.Path, pre) {
			fs.mu.Unlock()
			http.Error(w, "unavailable", st)
			return
		}
	}
	content, ok := fs.files[r.URL.Path]
	if !ok {
		fs.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if fs.corrupt[r.URL.Path] > 0 {
		fs.corrupt[r.URL.Path]--
		content = append([]byte(nil), content...)
		content[len(content)/2] ^= 0xff
	}
	dropAt, dropping := fs.drop[r.URL.Path]
	if dropping {
		delete(fs.drop, r.URL.Path)
	}
	gate, gated := fs.gates[r.URL.Path]
	hit := fs.gateHit[r.URL.Path]
	if gated {
		delete(fs.gates, r.URL.Path)
	}
	noRange := fs.noRange
	fs.mu.Unlock()

	start := int64(0)
	if rg := r.Header.Get("Range"); rg != "" && !noRange {
		v := strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-")
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n > int64(len(content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = n
	}
	body := content[start:]
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	fl := w.(http.Flusher)
	if dropping {
		n := dropAt - start
		if n > int64(len(body)) {
			n = int64(len(body))
		}
		if n > 0 {
			w.Write(body[:n])
		}
		fl.Flush()
		panic(http.ErrAbortHandler) // the connection breaks off
	}
	if gated {
		first := int64(64 << 10)
		if first > int64(len(body)) {
			first = int64(len(body)) / 2
		}
		w.Write(body[:first])
		fl.Flush()
		close(hit)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		body = body[first:]
	}
	w.Write(body)
}

// ---------------------------------------------------------------------------
// Test environment: a manifest served by the fake server
// ---------------------------------------------------------------------------

type testEnv struct {
	t        *testing.T
	srv      *fakeServer
	data     string // data dir
	manDir   string // built-in manifest directory
	content  map[string][]byte
	free     int64
	freeMu   sync.Mutex
	selfTest func(context.Context, SelfTestRequest) error
	inUse    func(string) (bool, string)
	stMu     sync.Mutex
	opts     Options
}

func randBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// makeTarGz builds a python-build-standalone-like archive.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func makeZip(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedKeys(files) {
		body := files[name]
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	zw.Close()
	return buf.Bytes()
}

const testLock = `# fake lock
pkg-a==1.0.0 \
    --hash=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
pkg-b==2.0.0 \
    --hash=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
    --hash=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
`

const testPyLock = `pypdf==6.21.2 \
    --hash=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
`

// manifestSpec varies the generated manifest.
type manifestSpec struct {
	serial       int64
	compat       string
	modelVersion string
	modelSeed    int64 // content of the tiny model
	pyVersion    string
	pySeed       int64
	cudaVersion  string
	offline      map[string]*Offline // id → offline entry
}

func defaultSpec() manifestSpec {
	return manifestSpec{serial: 1, compat: ">=3.0.0 <4.0.0", modelVersion: "1.0.0", modelSeed: 1,
		pyVersion: "1.0.0", pySeed: 1, cudaVersion: "1.0.0"}
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, srv: newFakeServer(t), data: t.TempDir(), manDir: t.TempDir(),
		content: map[string][]byte{}, free: 1 << 40}
	e.writeBuiltin(defaultSpec())
	return e
}

func (e *testEnv) url(p string) string { return e.srv.URL + p }

// files builds and serves the content of a spec and returns the manifest.
func (e *testEnv) manifest(s manifestSpec) *Manifest {
	t := e.t
	py := makeTarGz(t, map[string]string{
		"python/python.exe":  fmt.Sprintf("fake interpreter %d", s.pySeed),
		"python/bin/python3": "fake interpreter",
		"python/Lib/os.py":   "# os",
		"python/LICENSE.txt": "PSF",
	})
	model := randBytes(s.modelSeed, 700<<10+123)
	cfg := []byte(fmt.Sprintf(`{"model": "tiny", "seed": %d}`, s.modelSeed))
	ovModel := randBytes(77, 300<<10)
	tool := makeZip(t, map[string][]byte{"tool.exe": []byte("tool binary"), "LICENSE": []byte("MIT")})

	pyName := fmt.Sprintf("cpython-%d.tar.gz", s.pySeed)
	e.srv.put("/python/"+pyName, py)
	e.srv.put(fmt.Sprintf("/hf/tiny/%d/model.bin", s.modelSeed), model)
	e.srv.put(fmt.Sprintf("/hf/tiny/%d/config.json", s.modelSeed), cfg)
	e.srv.put("/hf/ov-tiny/model.xml", ovModel)
	e.srv.put("/files/tool.zip", tool)
	e.content["model.bin"] = model
	e.content["config.json"] = cfg
	e.content["python"] = py

	lockFile := func(name, body string) *Lock {
		os.MkdirAll(filepath.Join(e.manDir, "locks"), 0o755)
		os.WriteFile(filepath.Join(e.manDir, "locks", name), []byte(body), 0o644)
		e.srv.put("/files/locks/"+name, []byte(body))
		return &Lock{Path: "locks/" + name, URL: "{files}/locks/" + name, SHA256: shaHex([]byte(body)), Packages: 2}
	}
	plat := func(b *Build) map[string]*Build { return map[string]*Build{PlatformWindowsX64: b} }
	modelFiles := []File{
		{Name: "model.bin", URL: fmt.Sprintf("{models}/tiny/%d/model.bin", s.modelSeed), SHA256: shaHex(model), Size: int64(len(model))},
		{Name: "config.json", URL: fmt.Sprintf("{models}/tiny/%d/config.json", s.modelSeed), SHA256: shaHex(cfg), Size: int64(len(cfg))},
	}
	msize := int64(len(model) + len(cfg))
	m := &Manifest{
		Schema: 1, Serial: s.serial, AppVersion: "3.0.0", Compat: s.compat,
		Sources: map[string]SourceGroup{
			GroupPyPI: {Primary: "pypi",
				Default: SourceDef{ID: "default", Values: map[string]string{"pypi": e.url("/default/simple"), "python": e.url("/default/python")}},
				Mirrors: []SourceDef{{ID: "mirror", Region: "cn", Values: map[string]string{"pypi": e.url("/mirror/simple"), "python": e.url("/mirror/python")}}}},
			GroupModels: {Primary: "models",
				Default: SourceDef{ID: "default", Values: map[string]string{"models": e.url("/default/hf")}},
				Mirrors: []SourceDef{{ID: "hf-mirror", Region: "cn", Values: map[string]string{"models": e.url("/mirror/hf")}}}},
			"files": {Primary: "files", Default: SourceDef{ID: "default", Values: map[string]string{"files": e.url("/default/files")}}},
		},
		Components: []Component{
			{ID: IDCore, Version: "3.0.0", Kind: KindBinary, Bundled: true},
			{ID: IDPython, Version: s.pyVersion, Kind: KindPythonRuntime, CheckImports: []string{"pypdf"},
				Platforms: plat(&Build{DownloadSize: int64(len(py)) + 1000, InstalledSize: 5 << 20, InstalledEstimated: true,
					Files: []File{{Name: pyName, URL: "{python}/" + pyName, SHA256: shaHex(py), Size: int64(len(py)), Unpack: "tar.gz"}},
					Exe:   "python/python.exe", Lock: lockFile("python-windows-x64.txt", testPyLock)})},
			{ID: IDASRCUDA, Version: s.cudaVersion, Kind: KindVenv, Backend: BackendFasterWhisper, Depends: []string{IDPython},
				CheckImports: []string{"faster_whisper", "ctranslate2"},
				Platforms:    plat(&Build{DownloadSize: 3 << 20, InstalledSize: 6 << 20, Lock: lockFile("asr-cuda-windows-x64.txt", testLock)})},
			{ID: IDASRMLX, Version: "1.0.0", Kind: KindVenv, Backend: BackendMLX, Depends: []string{IDPython},
				Platforms: map[string]*Build{PlatformDarwinARM64: {DownloadSize: 1, InstalledSize: 1, Lock: lockFile("asr-mlx-darwin-arm64.txt", testLock)}}},
			{ID: "model:faster-whisper:tiny:float16", Version: s.modelVersion, Kind: KindModel, Backend: BackendFasterWhisper,
				Model: "tiny", Precision: "float16", License: "MIT", Depends: []string{IDASRCUDA},
				Platforms: plat(&Build{DownloadSize: msize, InstalledSize: msize, Files: modelFiles})},
			{ID: "model:faster-whisper:tiny:int8_float16", Version: s.modelVersion, Kind: KindModel, Backend: BackendFasterWhisper,
				Model: "tiny", Precision: "int8_float16", License: "MIT", Depends: []string{IDASRCUDA},
				Platforms: plat(&Build{DownloadSize: msize, InstalledSize: msize, Files: modelFiles})},
			{ID: "model:openvino:tiny:int8", Version: "1.0.0", Kind: KindModel, Backend: BackendOpenVINO,
				Model: "tiny", Precision: "int8", License: "MIT",
				Platforms: plat(&Build{DownloadSize: int64(len(ovModel)), InstalledSize: int64(len(ovModel)),
					Files: []File{{Name: "openvino_encoder_model.xml", URL: "{models}/ov-tiny/model.xml", SHA256: shaHex(ovModel), Size: int64(len(ovModel))}}})},
			{ID: "model:openvino:huge:fp16", Version: "1.0.0", Kind: KindModel, Backend: BackendOpenVINO,
				Model: "huge", Precision: "fp16", Unavailable: "no_hash",
				Platforms: plat(&Build{})},
			{ID: "tool", Version: "1.0.0", Kind: KindBinary,
				Platforms: plat(&Build{DownloadSize: int64(len(tool)), InstalledSize: 1 << 20,
					Files: []File{{Name: "tool.zip", URL: "{files}/tool.zip", SHA256: shaHex(tool), Size: int64(len(tool)), Unpack: "zip"}}})},
		},
		Presets: testPresets(),
	}
	for id, off := range s.offline {
		m.Component(id).Platforms[PlatformWindowsX64].Offline = off
	}
	return m
}

func testPresets() Presets {
	return Presets{
		Default: "standard",
		Tiers: []PresetTier{
			{ID: "standard", NameKey: "n", DescKey: "d", CostKey: "c", Roles: map[string]PresetRole{"translate": {Provider: "deepseek"}}},
			{ID: "free", NameKey: "n", DescKey: "d", CostKey: "c", NightProcessing: true, Roles: map[string]PresetRole{"translate": {Provider: FreeProvider}}},
			{ID: "transcribe_only", NameKey: "n", DescKey: "d", CostKey: "c", TranscribeOnly: true},
		},
		Providers: []PresetProvider{
			{ID: "deepseek", Name: "DeepSeek", Type: "openai", BaseURL: "https://api.deepseek.com", DefaultModel: "deepseek-flash",
				SignupURL: "https://example.invalid/s", KeysURL: "https://example.invalid/k", StepsKeys: []string{"s1"}, CostKey: "c"},
			{ID: "freebie", Name: "Free", Type: "openai", BaseURL: "https://free.example.invalid/v1", DefaultModel: "m", Free: true,
				TermsNotice: true, TermsURL: "https://free.example.invalid/terms",
				SignupURL: "https://example.invalid/s", KeysURL: "https://example.invalid/k", StepsKeys: []string{"s1"}, CostKey: "c"},
		},
		Free: FreeChoice{Default: "freebie"},
	}
}

func marshalManifest(t *testing.T, m *Manifest) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *testEnv) writeBuiltin(s manifestSpec) {
	b := marshalManifest(e.t, e.manifest(s))
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// serveManifest publishes a manifest at /manifest.json for CheckManifest.
func (e *testEnv) serveManifest(s manifestSpec) {
	e.srv.putExact("/manifest.json", marshalManifest(e.t, e.manifest(s)))
}

func (e *testEnv) setFree(n int64) {
	e.freeMu.Lock()
	e.free = n
	e.freeMu.Unlock()
}

func (e *testEnv) setSelfTest(fn func(context.Context, SelfTestRequest) error) {
	e.stMu.Lock()
	e.selfTest = fn
	e.stMu.Unlock()
}

func (e *testEnv) setInUse(fn func(string) (bool, string)) {
	e.stMu.Lock()
	e.inUse = fn
	e.stMu.Unlock()
}

func (e *testEnv) options() Options {
	return Options{
		DataDir:         e.data,
		AppVersion:      "3.0.0",
		Platform:        PlatformWindowsX64,
		BuiltinManifest: filepath.Join(e.manDir, "components.json"),
		ManifestURL:     e.url("/manifest.json"),
		UV:              fakeUVPath(),
		DiskFree: func(string) (int64, error) {
			e.freeMu.Lock()
			defer e.freeMu.Unlock()
			return e.free, nil
		},
		SelfTest: func(ctx context.Context, req SelfTestRequest) error {
			e.stMu.Lock()
			fn := e.selfTest
			e.stMu.Unlock()
			if fn == nil {
				return nil
			}
			return fn(ctx, req)
		},
		InUse: func(id string) (bool, string) {
			e.stMu.Lock()
			fn := e.inUse
			e.stMu.Unlock()
			if fn == nil {
				return false, ""
			}
			return fn(id)
		},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBackoff:  5 * time.Millisecond,
		StallTimeout:  5 * time.Second,
		EventInterval: time.Millisecond,
	}
}

func (e *testEnv) open() *Manager {
	e.t.Helper()
	m, err := New(e.options())
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { m.Close() })
	return m
}

// wait waits until the task reaches one of the states.
func wait(t *testing.T, m *Manager, id TaskID, states ...string) TaskStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, ok := m.Task(id)
		if !ok {
			t.Fatalf("task %s vanished", id)
		}
		for _, s := range states {
			if st.State == s {
				return st
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	st, _ := m.Task(id)
	t.Fatalf("task %s: state %s, want %v (error %+v)", id, st.State, states, st.Error)
	return st
}

// waitFor polls cond.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustInstall(t *testing.T, m *Manager, src Sources, ids ...string) TaskStatus {
	t.Helper()
	id, err := m.Install(ids, src)
	if err != nil {
		t.Fatal(err)
	}
	st := wait(t, m, id, TaskDone, TaskFailed, TaskCancelled)
	if st.State != TaskDone {
		t.Fatalf("install %v: %s %+v", ids, st.State, st.Error)
	}
	return st
}

func statusOf(m *Manager, id string) (Status, bool) {
	for _, s := range m.List() {
		if s.ID == id {
			return s, true
		}
	}
	return Status{}, false
}

// listFiles returns the files under dir (relative, "/"-separated).
func listFiles(t *testing.T, dir string) []string {
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}
