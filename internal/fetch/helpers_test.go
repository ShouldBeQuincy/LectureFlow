// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The secrets of the tests: none may reach the disk, the database or a log.
const (
	testCookie  = "COOKIE_SECRET_7f3a9c"
	testSig     = "SIG_SECRET_b81d24"
	testRefTok  = "REF_SECRET_c55e10"
	testReferer = "https://lms.example.edu/Panopto/Pages/Viewer.aspx?id=abc&token=" + testRefTok
	// testRefOrigin is the Referer the media hosts get: another origin
	// than the page's sees its origin only (refererFor).
	testRefOrigin = "https://lms.example.edu/"
)

func TestMain(m *testing.M) {
	if os.Getenv("LF_FAKE_ARIA2C") != "" {
		os.Exit(runFakeAriaProcess())
	}
	if mode := os.Getenv("LF_FAKE_FFMPEG"); mode != "" {
		os.Exit(fakeFFmpeg(mode))
	}
	os.Exit(m.Run())
}

// ---------- fake ffmpeg ----------

// fakeFFmpeg stands in for the bundled ffmpeg: it checks that every input
// is a local file (an HLS playlist: with the local-only options, every
// segment, key and init section present, no URL), writes the inputs'
// bytes concatenated to the output (last argument) and logs its arguments
// (LF_FAKE_FFMPEG_LOG). Like ffmpeg it reads an input with the options
// given before its own -i (security review 2, M3): every input needs
// -format_whitelist and -protocol_whitelist there, and a file that starts
// like an HLS playlist is refused as ffmpeg refuses it ("Format not on
// whitelist") unless hls is in its format whitelist. Modes: ok, fail,
// oldopts (an ffmpeg before 7.1), failcombine (fails with two inputs),
// novideo (an angle remux finds no video stream).
func fakeFFmpeg(mode string) int {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		// an aria2c --version probe of this binary (Options.ariaVersion
		// not set): answer it rather than write a file named --version
		fmt.Print(fakeVersion(featuresWindows))
		return 0
	}
	if p := os.Getenv("LF_FAKE_FFMPEG_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			b, _ := json.Marshal(args)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "[mp4 @ 0000] Could not find tag for codec\nConversion failed!")
		return 1
	}
	if mode == "failurl" { // a URL in the output (a file's metadata may hold one)
		fmt.Fprintln(os.Stderr, "[mov,mp4 @ 0000] source: HTTPS://cdn.example.edu/s/abc/v.mp4?Signature="+testSig)
		fmt.Fprintln(os.Stderr, "Conversion failed!")
		return 1
	}
	if mode == "failcombine" && strings.Count(strings.Join(args, " "), "-i ") >= 2 {
		fmt.Fprintln(os.Stderr, "[aost#0:1 @ 0000] Error muxing\nConversion failed!")
		return 1
	}
	if mode == "novideo" && slices.Contains(args, "-an") {
		fmt.Fprintln(os.Stderr, "Stream map '0:v:0' matches no streams.\nTo ignore this, add a trailing '?' to the map.")
		return 1
	}
	if mode == "oldopts" && strings.Contains(strings.Join(args, " "), "-extension_picky") {
		fmt.Fprintln(os.Stderr, "Option extension_picky not found.")
		return 1
	}
	var out bytes.Buffer
	start := 0 // where the options of the next input begin
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-i" {
			continue
		}
		in, opts := args[i+1], args[start:i]
		start = i + 2
		if !filepath.IsAbs(in) {
			fmt.Fprintln(os.Stderr, "input not absolute:", in)
			return 1
		}
		formats, protocols := optionValue(opts, "-format_whitelist"), optionValue(opts, "-protocol_whitelist")
		if formats == "" || protocols == "" {
			fmt.Fprintln(os.Stderr, "input without the whitelists before its -i:", in)
			return 1
		}
		b, err := os.ReadFile(in)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		playlist, hls := bytes.HasPrefix(b, []byte("#EXTM3U")), slices.Contains(strings.Split(formats, ","), "hls")
		switch {
		case playlist && !hls:
			fmt.Fprintf(os.Stderr, "[in#0 @ 0000] Format not on whitelist '%s'\n[in#0 @ 0000] Error opening input: Invalid argument\nError opening input file %s.\n", formats, in)
			return 1
		case hls && !playlist:
			fmt.Fprintln(os.Stderr, "the hls demuxer allowed for an input that is no local playlist:", in)
			return 1
		case !playlist:
			if protocols != "file" {
				fmt.Fprintln(os.Stderr, "protocol whitelist", protocols, "for a file input:", in)
				return 1
			}
			out.Write(b)
			continue
		}
		before := strings.Join(opts, " ")
		if !strings.Contains(before, "-allowed_extensions ALL") || protocols != "file,crypto" {
			fmt.Fprintln(os.Stderr, "playlist input without the local-only options")
			return 1
		}
		pl := b
		dir := filepath.Dir(in)
		for _, l := range strings.Split(string(pl), "\n") {
			l = strings.TrimSpace(l)
			if strings.Contains(l, "http") {
				fmt.Fprintln(os.Stderr, "URL in the local playlist:", l)
				return 1
			}
			ref := ""
			switch {
			case l == "":
			case strings.HasPrefix(l, "#EXT-X-KEY"), strings.HasPrefix(l, "#EXT-X-MAP"):
				if s := strings.Index(l, `URI="`); s >= 0 {
					ref = l[s+5:]
					ref = ref[:strings.IndexByte(ref, '"')]
				}
			case strings.HasPrefix(l, "#"):
			default:
				ref = l
			}
			if ref == "" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, ref))
			if err != nil {
				fmt.Fprintln(os.Stderr, "missing part:", err)
				return 1
			}
			if !strings.HasPrefix(l, "#EXT-X-KEY") {
				out.Write(b)
			}
		}
	}
	if err := os.WriteFile(args[len(args)-1], out.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// optionValue is the value of the last name in opts ("" when absent).
func optionValue(opts []string, name string) string {
	v := ""
	for i := 0; i+1 < len(opts); i++ {
		if opts[i] == name {
			v = opts[i+1]
		}
	}
	return v
}

// ---------- fake media server ----------

type seenReq struct {
	Method, Path, Query, Cookie, Referer, Range, UA string
	CookieHeader                                    string // the whole Cookie header
}

// mediaServer serves the test media. It checks the cookie, the referer and
// the signed query on every media request (403 otherwise), can force a
// status per path, and can hold a file after its first half (gate).
type mediaServer struct {
	srv *httptest.Server

	mu      sync.Mutex
	files   map[string][]byte
	status  map[string]int
	gated   map[string]bool
	stalled map[string]bool   // every GET (ranged too) stops after 64 KB until release
	redir   map[string]string // path → Location (302)
	noHead  map[string]int    // path → status answered to HEAD
	gate    chan struct{}
	seen    []seenReq
	cookie  string // required Cookie value of sid ("" = none required)
	sigFor  string // path prefix that needs ?Signature=testSig
	// refPrefix is how the required Referer starts (the page's origin).
	refPrefix string
	started   chan string
}

func newMediaServer(t *testing.T) *mediaServer {
	m := &mediaServer{files: map[string][]byte{}, status: map[string]int{}, gated: map[string]bool{}, stalled: map[string]bool{},
		redir: map[string]string{}, noHead: map[string]int{},
		cookie: testCookie, sigFor: "/files/", refPrefix: testRefOrigin, started: make(chan string, 64)}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(func() {
		m.release()
		m.srv.Close()
	})
	return m
}

func (m *mediaServer) url(p string) string { return m.srv.URL + p }

func (m *mediaServer) put(p string, b []byte) {
	m.mu.Lock()
	m.files[p] = b
	m.mu.Unlock()
}

func (m *mediaServer) setStatus(p string, code int) {
	m.mu.Lock()
	if code == 0 {
		delete(m.status, p)
	} else {
		m.status[p] = code
	}
	m.mu.Unlock()
}

// stall makes every GET of path (ranged too: aria2c's split connections)
// stop after 64 KB until release.
func (m *mediaServer) stall(p string) {
	m.mu.Lock()
	m.stalled[p] = true
	if m.gate == nil {
		m.gate = make(chan struct{})
	}
	m.mu.Unlock()
}

// hold makes path stop after half of its bytes until release.
func (m *mediaServer) hold(p string) {
	m.mu.Lock()
	m.gated[p] = true
	if m.gate == nil {
		m.gate = make(chan struct{})
	}
	m.mu.Unlock()
}

func (m *mediaServer) release() {
	m.mu.Lock()
	if m.gate != nil {
		close(m.gate)
		m.gate = nil
	}
	m.gated = map[string]bool{}
	m.stalled = map[string]bool{}
	m.mu.Unlock()
}

func (m *mediaServer) requests(p string) []seenReq {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []seenReq
	for _, s := range m.seen {
		if p == "" || s.Path == p {
			out = append(out, s)
		}
	}
	return out
}

func (m *mediaServer) serve(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("sid")
	cookie := ""
	if c != nil {
		cookie = c.Value
	}
	m.mu.Lock()
	m.seen = append(m.seen, seenReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Cookie: cookie, Referer: r.Referer(),
		Range: r.Header.Get("Range"), UA: r.Header.Get("User-Agent"), CookieHeader: r.Header.Get("Cookie")})
	code := m.status[r.URL.Path]
	location := m.redir[r.URL.Path]
	if r.Method == http.MethodHead && m.noHead[r.URL.Path] != 0 {
		code = m.noHead[r.URL.Path]
	}
	body, ok := m.files[r.URL.Path]
	gated, gate := m.gated[r.URL.Path], m.gate
	stalled := m.stalled[r.URL.Path]
	need, sigFor, refPrefix := m.cookie, m.sigFor, m.refPrefix
	m.mu.Unlock()
	select {
	case m.started <- r.URL.Path:
	default:
	}
	switch {
	case code != 0:
		http.Error(w, "forced", code)
		return
	case need != "" && cookie != need:
		http.Error(w, "no session", http.StatusForbidden)
		return
	case !strings.HasPrefix(r.Referer(), refPrefix):
		http.Error(w, "bad referer", http.StatusForbidden)
		return
	case sigFor != "" && strings.HasPrefix(r.URL.Path, sigFor) && r.URL.Query().Get("Signature") != testSig:
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	case location != "":
		http.Redirect(w, r, location, http.StatusFound)
		return
	case !ok:
		http.NotFound(w, r)
		return
	}
	if stalled && r.Method == http.MethodGet {
		// whatever the range: 64 KB of it, then nothing until release
		w.Header().Set("Accept-Ranges", "bytes")
		start, end := int64(0), int64(len(body))
		if rg := r.Header.Get("Range"); rg != "" {
			var a, b int64 = 0, -1
			if n, _ := fmt.Sscanf(rg, "bytes=%d-%d", &a, &b); n >= 1 {
				start = a
				if n == 2 && b+1 < end {
					end = b + 1
				}
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(body)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(http.StatusOK)
		}
		first := min(start+64<<10, end)
		w.Write(body[start:first])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(body[first:end])
		return
	}
	if gated && r.Method == http.MethodGet && r.Header.Get("Range") == "" {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		half := len(body) / 2
		w.Write(body[:half])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(body[half:])
		return
	}
	http.ServeContent(w, r, filepath.Base(r.URL.Path), time.Time{}, bytes.NewReader(body))
}

// pattern returns n deterministic bytes.
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// ---------- the queue under test ----------

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type qenv struct {
	t       *testing.T
	root    string
	dataDir string
	st      db.Store
	course  string
	aria    *fakeAria
	media   *mediaServer
	q       *Queue
	logs    *syncBuffer
	ffLog   string
	opts    Options

	mu        sync.Mutex
	imports   []ImportRequest
	importErr error
	launchErr *model.ErrorInfo
	changed   map[int64]int
}

// newQEnv builds a queue on a real database with the fake aria2c (in this
// process), the fake ffmpeg (this test binary) and a fake lecture import,
// and starts it.
func newQEnv(t *testing.T, tweak func(*Options)) *qenv {
	t.Helper()
	root := t.TempDir()
	e := &qenv{t: t, root: root, dataDir: filepath.Join(root, "data"), aria: newFakeAria(), media: newMediaServer(t),
		logs: &syncBuffer{}, ffLog: filepath.Join(root, "ffmpeg.log"), changed: map[int64]int{}}
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := db.Open(filepath.Join(e.dataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.st = st
	t.Cleanup(func() { st.Close() })
	e.course = e.mkCourse("CSC1003")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LF_FAKE_FFMPEG", "ok")
	t.Setenv("LF_FAKE_FFMPEG_LOG", e.ffLog)
	e.opts = Options{
		Store: st.(Store), DataDir: e.dataDir, Aria2c: exe, FFmpeg: exe, Import: e.importFn,
		OnChange: func(id int64) {
			e.mu.Lock()
			e.changed[id]++
			e.mu.Unlock()
		},
		Launcher: e.aria.launcher(), IdleTimeout: time.Minute, PollInterval: 10 * time.Millisecond,
		StatusInterval: 20 * time.Millisecond, MinFree: -1,
		Logger:        slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		allowLoopback: true, // the media servers listen on 127.0.0.1 (local.go)
		// aria2c is the fake in this process: the official Windows build's
		// features unless a test says otherwise
		ariaVersion: func(string) (string, error) { return fakeVersion(featuresWindows), nil },
	}
	if tweak != nil {
		tweak(&e.opts)
	}
	e.start()
	t.Cleanup(func() { e.q.Close(5 * time.Second) })
	return e
}

func (e *qenv) start() {
	e.t.Helper()
	e.q = New(e.opts)
	// no DNS and no proxy from the environment in the tests: a name is
	// unknown unless a test gives it an address (local.go)
	e.q.local.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host (test)") }
	e.q.local.proxy = noProxy
	// (the guard proxy so connects to addresses written in a URL only: the
	// tests' servers on 127.0.0.1; a test that gives a name another
	// address sets loopbackWire)
	if err := e.q.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// loopbackWire is the network of a test whose look-up gives names
// addresses that are not this computer's (addrPolicy.dial): a connection
// to any address goes to the same port of 127.0.0.1, so the test's
// "public" and "private" addresses are servers of the test, and nothing
// leaves the computer. The address was judged before, as the address it
// claims to be.
func loopbackWire(ctx context.Context, network, address string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", port))
}

// restart closes the queue and starts a new one on the same data (a new
// process: nothing in memory survives).
func (e *qenv) restart() {
	e.t.Helper()
	e.q.Close(5 * time.Second)
	e.start()
}

func (e *qenv) mkCourse(name string) string {
	c := model.Course{ID: db.NewID(), Name: name}
	if err := e.st.CreateCourse(context.Background(), c); err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

func (e *qenv) importFn(ctx context.Context, req ImportRequest) (ImportResult, error) {
	e.mu.Lock()
	e.imports = append(e.imports, req)
	ierr, lerr := e.importErr, e.launchErr
	e.mu.Unlock()
	if ierr != nil {
		return ImportResult{}, ierr
	}
	l := model.Lecture{ID: db.NewID(), CourseID: req.CourseID, Title: req.Title, OriginalFilename: req.Filename,
		TitleSource: model.TitleSourceFilename}
	if req.RecordedAt != nil {
		l.RecordedAt, l.RecordedAtSource = req.RecordedAt, model.RecordedAtUser
	}
	dir := model.LectureDir(e.dataDir, l.CourseID, l.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ImportResult{}, err
	}
	l.MediaPath = filepath.Join(dir, "media"+filepath.Ext(req.Path))
	if err := os.Rename(req.Path, l.MediaPath); err != nil {
		return ImportResult{}, err
	}
	for _, a := range req.Angles {
		if err := os.Rename(a.Path, filepath.Join(dir, a.Name+".mp4")); err != nil {
			return ImportResult{}, err
		}
	}
	if err := e.st.CreateLecture(ctx, l); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{LectureID: l.ID, LaunchError: lerr}, nil
}

func (e *qenv) importsSnapshot() []ImportRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ImportRequest(nil), e.imports...)
}

func (e *qenv) cookies() []bridge.Cookie {
	return []bridge.Cookie{
		{Name: "sid", Value: testCookie, Domain: "127.0.0.1", Path: "/", HostOnly: true},
		{Name: "other", Value: "elsewhere", Domain: "example.org", Path: "/"},
	}
}

func (e *qenv) fileTrack(kind, p string) bridge.Track {
	return bridge.Track{URL: e.media.url(p) + "?Signature=" + testSig + "&Expires=2000000000", Kind: kind,
		Format: bridge.FormatFile, Role: kind, Label: kind, SizeBytes: -1}
}

func (e *qenv) hlsTrack(kind, p string) bridge.Track {
	return bridge.Track{URL: e.media.url(p) + "?Policy=" + testSig, Kind: kind, Format: bridge.FormatHLS,
		Role: "hls-" + kind, Label: kind, SizeBytes: -1}
}

func (e *qenv) request(session string, tracks ...bridge.Track) *bridge.EnqueueRequest {
	return &bridge.EnqueueRequest{
		Envelope: bridge.Envelope{Type: bridge.TypeEnqueue, ID: "r-" + session},
		Profile:  bridge.Profile{ID: "panopto", Version: "1.0.0", Reviewed: true},
		Platform: "panopto", SessionID: session,
		Lecture:  bridge.Lecture{Title: "Lecture 3: Graphs / Trees", RecordedAt: "2026-10-21T09:35", DurationSec: 3300},
		CourseID: e.course, Tracks: tracks, Referer: testReferer, Cookies: e.cookies(),
	}
}

func (e *qenv) enqueue(req *bridge.EnqueueRequest) int64 {
	e.t.Helper()
	rep, err := e.q.Enqueue(context.Background(), req)
	if err != nil {
		e.t.Fatalf("enqueue %s: %v", req.SessionID, err)
	}
	if !rep.OK || rep.TaskID == 0 || rep.Type != bridge.TypeEnqueue || rep.ID != req.ID {
		e.t.Fatalf("enqueue reply %+v", rep)
	}
	return rep.TaskID
}

// wait polls until the task is in state (fails after 15 s).
func (e *qenv) wait(id int64, state string) TaskInfo {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		info, err := e.q.Get(context.Background(), id)
		if err == nil && info.State == state {
			return info
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("task %d: state %q (err %v, info %+v), want %s\nlog:\n%s", id, info.State, err, info, state, e.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitFor polls cond (fails after 15 s).
func (e *qenv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, e.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *qenv) ffmpegCalls() [][]string {
	b, err := os.ReadFile(e.ffLog)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var a []string
		if json.Unmarshal([]byte(l), &a) == nil {
			out = append(out, a)
		}
	}
	return out
}

func (e *qenv) lecture(id string) model.Lecture {
	e.t.Helper()
	l, err := e.st.GetLecture(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *qenv) row(id int64) db.DownloadTask {
	e.t.Helper()
	rec, err := e.opts.Store.GetDownloadTask(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func readAll(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var _ = io.Discard
