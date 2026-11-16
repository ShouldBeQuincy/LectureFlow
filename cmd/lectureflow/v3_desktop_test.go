// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// v3 step 3 (v1-go): the command-line contract, desktop mode and the
// graceful stop (SPEC-v3 V1.1, V1.2; .claude/v3-brief.md §1). Fake ASR, no
// LLM request, no GPU.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/selfupdate"
	"lectureflow/internal/state"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var readyRe = regexp.MustCompile(`^LECTUREFLOW_READY http://127\.0\.0\.1:([1-9][0-9]*)$`)

// writeTestConfig writes a config.toml in test mode (fake ASR, no
// nvidia-smi, a fake DeepSeek key) and returns its path and the data dir.
func writeTestConfig(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	fakeASR, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fake_asr.json"))
	if err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "config.toml")
	body := fmt.Sprintf(`data_dir = %q
nvidia_smi = %q
[asr]
fake_result = %q
[llm.providers.deepseek]
api_key = "sk-test-not-a-real-key"
`, filepath.ToSlash(dataDir), filepath.ToSlash(filepath.Join(dir, "no-nvidia-smi")), filepath.ToSlash(fakeASR))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

func TestParseArgs(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "1.2.3-build"
	var errb bytes.Buffer
	o, err := parseArgs(nil, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if o.Config != "config.toml" || o.Listen != "" || o.DataDir != "" || o.Desktop || o.Resources != "" || o.LocationFile != "" || o.AppVersion != "1.2.3-build" {
		t.Fatalf("defaults: %+v", o)
	}
	o, err = parseArgs([]string{"-config", "c.toml", "-listen", "127.0.0.1:0", "-data-dir", "D", "-desktop", "-resources", "R", "-app-version", "3.0.0", "-location-file", "L"}, &errb)
	if err != nil {
		t.Fatal(err)
	}
	want := cliOptions{Config: "c.toml", Listen: "127.0.0.1:0", DataDir: "D", Desktop: true, Resources: "R", AppVersion: "3.0.0", LocationFile: "L"}
	if o != want {
		t.Fatalf("got %+v want %+v", o, want)
	}
	version = ""
	if o, _ := parseArgs(nil, &errb); o.AppVersion != "dev" {
		t.Fatalf("version fallback %q", o.AppVersion)
	}
	if _, err := parseArgs([]string{"-nope"}, &errb); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err := parseArgs([]string{"extra"}, &errb); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestReadToken(t *testing.T) {
	tok, _, err := readToken(strings.NewReader("  " + testToken + "  \r\nmore"))
	if err != nil || tok != testToken {
		t.Fatalf("token %q err %v", tok, err)
	}
	if tok, _, err := readToken(strings.NewReader(testToken[:32])); err != nil || tok != testToken[:32] {
		t.Fatalf("32 characters without newline: %q %v", tok, err)
	}
	for _, in := range []string{"", "\n", "short\n", testToken[:31] + "\n"} {
		if _, _, err := readToken(strings.NewReader(in)); err == nil {
			t.Fatalf("token %q accepted", in)
		}
	}
	old := tokenReadTimeout
	tokenReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { tokenReadTimeout = old })
	pr, pw := io.Pipe()
	defer pw.Close()
	if _, _, err := readToken(pr); err == nil {
		t.Fatal("missing token must fail after the timeout")
	}
}

func TestWatchEOFStops(t *testing.T) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	watchEOF(pr, func() { close(done) })
	pw.Write([]byte("ignored\n"))
	select {
	case <-done:
		t.Fatal("stopped before EOF")
	case <-time.After(30 * time.Millisecond):
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EOF did not stop")
	}
}

func TestWithLimitExitsOnTimeout(t *testing.T) {
	var got []int
	var mu sync.Mutex
	exit := func(c int) { mu.Lock(); got = append(got, c); mu.Unlock() }
	release := make(chan struct{})
	t0 := time.Now()
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(release)
	}()
	withLimit(100*time.Millisecond, exitRestart, exit, nil, func(deadline time.Time) {
		if d := time.Until(deadline); d <= 0 || d > 100*time.Millisecond {
			t.Errorf("deadline in %v", d)
		}
		<-release
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != exitRestart {
		t.Fatalf("exit calls %v", got)
	}
	if time.Since(t0) < 100*time.Millisecond {
		t.Fatal("returned too early")
	}
	got = nil
	withLimit(time.Second, exitOK, exit, nil, func(time.Time) {})
	time.Sleep(10 * time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("exit called after a stop within the limit: %v", got)
	}
}

// proc runs runMain in this process.
type proc struct {
	t      *testing.T
	stdinW *io.PipeWriter
	sigc   chan os.Signal
	lines  chan string
	stderr *syncBuf
	exits  chan int
	code   chan int
	app    chan *App
	desk   chan *api.DesktopOptions
	mu     sync.Mutex
	out    []string
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// offlineSelfUpdate gives the in-app update a client that refuses every
// request. Since the maintainer's key is built in (2026-10-27), a desktop
// instance with a release version ("-app-version 3.2.0-test") has updates
// configured and would ask GitHub two minutes after its start; tests never
// reach the network.
func offlineSelfUpdate(o *selfupdate.Options) {
	o.Client = &http.Client{Transport: refuseTransport{}}
}

type refuseTransport struct{}

func (refuseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("no network in tests: %s", r.URL.Host)
}

func startProc(t *testing.T, args []string, stdin string, opts AppOptions) *proc {
	t.Helper()
	if opts.SelfUpdate == nil {
		opts.SelfUpdate = offlineSelfUpdate
	}
	pr, pw := io.Pipe()
	outR, outW := io.Pipe()
	p := &proc{t: t, stdinW: pw, sigc: make(chan os.Signal, 1), lines: make(chan string, 1000), stderr: &syncBuf{},
		exits: make(chan int, 4), code: make(chan int, 1), app: make(chan *App, 1), desk: make(chan *api.DesktopOptions, 1)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			p.mu.Lock()
			p.out = append(p.out, sc.Text())
			p.mu.Unlock()
			select {
			case p.lines <- sc.Text():
			default:
			}
		}
	}()
	if stdin != "" {
		go pw.Write([]byte(stdin))
	}
	go func() {
		c := runMain(runEnv{args: args, stdin: pr, stdout: outW, stderr: p.stderr, signals: p.sigc,
			exit: func(c int) { p.exits <- c }, appOpts: opts,
			started: func(a *App, d *api.DesktopOptions) { p.app <- a; p.desk <- d }})
		p.code <- c
		outW.Close()
	}()
	t.Cleanup(func() {
		pw.Close()
		select {
		case p.sigc <- os.Interrupt:
		default:
		}
		select {
		case <-p.code:
		case <-time.After(15 * time.Second):
		}
	})
	return p
}

// ready waits for the ready line and returns the base URL.
func (p *proc) ready() string {
	p.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case l := <-p.lines:
			if strings.HasPrefix(l, ReadyPrefix) {
				if !readyRe.MatchString(l) {
					p.t.Fatalf("ready line %q", l)
				}
				return strings.TrimPrefix(l, ReadyPrefix)
			}
		case c := <-p.code:
			p.code <- c
			p.t.Fatalf("exited with %d before ready; stderr:\n%s", c, p.stderr.String())
		case <-deadline:
			p.t.Fatal("no ready line")
		}
	}
}

func (p *proc) wait(limit time.Duration) int {
	p.t.Helper()
	select {
	case c := <-p.code:
		p.code <- c
		return c
	case <-time.After(limit):
		p.t.Fatalf("did not exit within %v", limit)
		return -1
	}
}

func (p *proc) stdout() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.out, "\n")
}

func get(t *testing.T, url string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// Web mode as before: no token, the notice and the ready line, SIGINT stops
// with exit code 0.
func TestRunWebMode(t *testing.T) {
	cfgPath, dataDir := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	url := p.ready()
	if !strings.Contains(p.stdout(), webModeNotice) {
		t.Fatal("web mode notice missing")
	}
	if c := get(t, url+"/api/courses", nil); c != http.StatusOK {
		t.Fatalf("GET /api/courses without token in web mode: %d", c)
	}
	if d := <-p.desk; d != nil {
		t.Fatal("desktop options in web mode")
	}
	a := <-p.app
	// The legacy config.toml settings are imported in web mode (A5.1).
	if provs, err := a.Store.ListProviders(context.Background()); err != nil || len(provs) != 1 || provs[0].ID != "deepseek" {
		t.Fatalf("web mode providers %+v %v", provs, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "lectureflow.db")); err != nil {
		t.Fatal(err)
	}
	p.sigc <- os.Interrupt
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
	if len(p.exits) != 0 {
		t.Fatal("hard exit in a normal stop")
	}
}

// Desktop mode: token from stdin, no provider import, -data-dir overrides
// the config, stdin EOF stops with code 0.
func TestRunDesktopModeStdinEOF(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	dataDir := filepath.Join(t.TempDir(), "desk")
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-data-dir", dataDir, "-desktop", "-app-version", "3.0.0-test"}, testToken+"\n", AppOptions{})
	url := p.ready()
	if strings.Contains(p.stdout(), webModeNotice) {
		t.Fatal("web mode notice in desktop mode")
	}
	d := <-p.desk
	if d == nil || d.Token != testToken || d.Shutdown == nil || d.Restart == nil || !strings.HasSuffix(url, fmt.Sprintf(":%d", d.Port)) {
		t.Fatalf("desktop options %+v (url %s)", d, url)
	}
	a := <-p.app
	if a.Cfg.DataDir != dataDir {
		t.Fatalf("data dir %q", a.Cfg.DataDir)
	}
	if provs, err := a.Store.ListProviders(context.Background()); err != nil || len(provs) != 0 {
		t.Fatalf("desktop mode must not import config.toml providers: %+v %v", provs, err)
	}
	if c := get(t, url+"/api/courses", map[string]string{api.TokenHeader: testToken}); c != http.StatusOK {
		t.Fatalf("with token: %d", c)
	}
	if c := get(t, url+"/api/courses", nil); c != http.StatusUnauthorized {
		t.Fatalf("without token: %d", c)
	}
	t0 := time.Now()
	p.stdinW.Close()
	if c := p.wait(10 * time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("stop took %v", d)
	}
}

// DesktopOptions.Restart: graceful stop, then exit code 3.
func TestRunDesktopRestartExitCode3(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	p.ready()
	d := <-p.desk
	d.Restart()  // returns at once
	d.Shutdown() // a later request does not change the code
	if c := p.wait(10 * time.Second); c != exitRestart {
		t.Fatalf("exit code %d, want 3", c)
	}
}

// A short token fails the start: exit code 1, "ERROR: ..." last on stderr.
func TestRunDesktopShortToken(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, "tooshort\n", AppOptions{})
	if c := p.wait(10 * time.Second); c != exitFailure {
		t.Fatalf("exit code %d", c)
	}
	lines := strings.Split(strings.TrimRight(p.stderr.String(), "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "ERROR: ") || !strings.Contains(last, "token") {
		t.Fatalf("stderr last line %q", last)
	}
	if strings.Contains(p.stdout(), ReadyPrefix) {
		t.Fatal("ready line after a failed start")
	}
}

// A port in use fails the start the same way.
func TestRunListenFailure(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	p1 := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	url := p1.ready()
	addr := strings.TrimPrefix(url, "http://")
	cfg2, _ := writeTestConfig(t)
	p2 := startProc(t, []string{"-config", cfg2, "-listen", addr}, "", AppOptions{})
	if c := p2.wait(10 * time.Second); c != exitFailure {
		t.Fatalf("exit code %d", c)
	}
	lines := strings.Split(strings.TrimRight(p2.stderr.String(), "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "ERROR: ") {
		t.Fatalf("stderr last line %q", last)
	}
}

// blockingStage replaces prep: it runs until cancelled (or, with ignore,
// until released).
type blockingStage struct {
	started chan string
	release chan struct{}
	ignore  bool
}

func (b *blockingStage) Name() string                                      { return model.StagePrep }
func (b *blockingStage) Resource() sched.ResourceKind                      { return sched.ResourceCPU }
func (b *blockingStage) Provider() string                                  { return "" }
func (b *blockingStage) InputHash(context.Context, string) (string, error) { return "", nil }
func (b *blockingStage) Run(ctx context.Context, id string, _ func(float64)) error {
	select {
	case b.started <- id:
	default:
	}
	if b.ignore {
		<-b.release
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return nil
	}
}

type instantStage struct{}

func (instantStage) Name() string                                      { return model.StagePrep }
func (instantStage) Resource() sched.ResourceKind                      { return sched.ResourceCPU }
func (instantStage) Provider() string                                  { return "" }
func (instantStage) InputHash(context.Context, string) (string, error) { return "", nil }
func (instantStage) Run(context.Context, string, func(float64)) error  { return nil }

func fastSched(o *sched.Options) {
	o.ScanInterval = 10 * time.Millisecond
	o.ProgressMinInterval = 10 * time.Millisecond
}

// launchLecture creates a course and a launched lecture with the current
// settings' snapshots; returns the lecture id.
func launchLecture(t *testing.T, a *App) string {
	t.Helper()
	ctx := context.Background()
	c := model.Course{ID: db.NewID(), Name: "CS"}
	if err := a.Store.CreateCourse(ctx, c); err != nil {
		t.Fatal(err)
	}
	l := model.Lecture{ID: db.NewID(), CourseID: c.ID, Title: "L", MediaPath: "m.mp4"}
	if err := a.Store.CreateLecture(ctx, l); err != nil {
		t.Fatal(err)
	}
	snaps, err := a.Settings.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Launch(ctx, a.Store, l.ID, snaps); err != nil {
		t.Fatal(err)
	}
	a.Sched.Kick()
	return l.ID
}

func prepJob(t *testing.T, st db.Store, lectureID string) model.Job {
	t.Helper()
	j, err := st.GetCurrentJob(context.Background(), model.ScopeLecture, lectureID, model.StagePrep)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// The graceful stop cancels a running job; the next start's recovery (SPEC
// §4.7) requeues it with attempt + 1, and it then completes.
func TestGracefulStopThenStartupRecovery(t *testing.T) {
	cfgPath, _ := writeTestConfig(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	blk := &blockingStage{started: make(chan string, 1), release: make(chan struct{})}
	ctx := context.Background()
	a, err := buildApp(ctx, cfg, AppOptions{Sched: fastSched, Stages: []sched.Stage{blk}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id := launchLecture(t, a)
	select {
	case <-blk.started:
	case <-time.After(10 * time.Second):
		a.Close()
		t.Fatal("prep did not start")
	}
	before := prepJob(t, a.Store, id)
	if before.Status != model.StatusRunning {
		t.Fatalf("status %s", before.Status)
	}
	t0 := time.Now()
	a.Shutdown(time.Now().Add(9*time.Second), nil)
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("graceful stop took %v", d)
	}
	a.Close() // no-op after Shutdown

	inst := instantStage{}
	b, err := buildApp(ctx, cfg, AppOptions{Sched: fastSched, Stages: []sched.Stage{inst}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	j := prepJob(t, b.Store, id)
	if j.Status != model.StatusQueued || j.Attempt != before.Attempt+1 {
		t.Fatalf("after recovery: status %s attempt %d (before %d) error %q", j.Status, j.Attempt, before.Attempt, deref(j.Error))
	}
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for prepJob(t, b.Store, id).Status != model.StatusDone {
		if time.Now().After(deadline) {
			t.Fatalf("recovered job not done: %s", prepJob(t, b.Store, id).Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A job that ignores cancellation does not hold the stop past its limit:
// runMain returns within the limit (the scheduler wait is bounded) and the
// hard exit is not needed.
func TestRunStopWithinLimitDespiteStuckJob(t *testing.T) {
	old := shutdownLimit
	shutdownLimit = 3 * time.Second
	t.Cleanup(func() { shutdownLimit = old })
	cfgPath, _ := writeTestConfig(t)
	blk := &blockingStage{started: make(chan string, 1), release: make(chan struct{}), ignore: true}
	t.Cleanup(func() { close(blk.release) })
	p := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{Sched: fastSched, Stages: []sched.Stage{blk}})
	p.ready()
	a := <-p.app
	launchLecture(t, a)
	select {
	case <-blk.started:
	case <-time.After(10 * time.Second):
		t.Fatal("prep did not start")
	}
	t0 := time.Now()
	p.sigc <- os.Interrupt
	if c := p.wait(shutdownLimit + time.Second); c != exitOK {
		t.Fatalf("exit code %d", c)
	}
	if d := time.Since(t0); d > shutdownLimit {
		t.Fatalf("stop took %v (limit %v)", d, shutdownLimit)
	}
}

func TestPreventSleepSetting(t *testing.T) {
	st, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if !preventSleep(ctx, st) {
		t.Fatal("default must be on")
	}
	for v, want := range map[string]bool{"0": false, "false": false, "1": true, "true": true, "": true} {
		if err := st.SetKV(ctx, model.KVAppPreventSleep, v); err != nil {
			t.Fatal(err)
		}
		if got := preventSleep(ctx, st); got != want {
			t.Errorf("prevent_sleep %q: %v", v, got)
		}
	}
}

// Process level: the built binary in desktop mode reads the token, prints
// the ready line, and exits with 0 within 10 s of stdin closing.
func TestDesktopBinaryProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "lectureflow")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9-test", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cfgPath, _ := writeTestConfig(t)
	cmd := exec.Command(bin, "-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop")
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuf
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() { exited <- cmd.Wait(); close(waitDone) }()
	defer func() {
		select {
		case <-waitDone:
		default:
			cmd.Process.Kill()
			select {
			case <-waitDone:
			case <-time.After(10 * time.Second):
				t.Error("process did not end after Kill")
			}
		}
	}()
	if _, err := io.WriteString(stdin, testToken+"\n"); err != nil {
		t.Fatal(err)
	}
	readyc := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, ReadyPrefix) {
				readyc <- l
			}
		}
	}()
	var url string
	select {
	case l := <-readyc:
		if !readyRe.MatchString(l) {
			t.Fatalf("ready line %q", l)
		}
		url = strings.TrimPrefix(l, ReadyPrefix)
	case err := <-exited:
		t.Fatalf("exited before ready: %v\n%s", err, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("no ready line")
	}
	if c := get(t, url+"/api/courses", map[string]string{api.TokenHeader: testToken}); c != http.StatusOK {
		t.Fatalf("GET with token: %d", c)
	}
	t0 := time.Now()
	stdin.Close()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if errors.As(err, &ee) || err != nil {
			t.Fatalf("exit: %v\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not exit within 10 s of stdin closing")
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("stop took %v", d)
	}
}
