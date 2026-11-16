// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/colab/loginfake"
)

// 3.1.0 (.claude/v31-brief.md §2.2): the in-app Google sign-in runs a fake
// gcloud (internal/colab/loginfake) — never the real one, never Google.

// sleeperArg makes the test binary a long-running child of the fake gcloud
// ("python" for ModeHang, "browser" for ModeBrowser): it records its pid in
// {LECTUREFLOW_FAKE_GCLOUD_OUT}/{role}.pid and sleeps (bounded, so a failed
// test leaves nothing running for long).
const sleeperArg = "sleeper"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == sleeperArg && os.Getenv(loginfake.EnvOut) != "" {
		role := os.Args[2]
		p := filepath.Join(os.Getenv(loginfake.EnvOut), role+".pid")
		if err := os.WriteFile(p+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil || os.Rename(p+".tmp", p) != nil {
			os.Exit(3)
		}
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const loginTestADC = `{"type":"authorized_user","client_id":"` + testClientID + `","client_secret":"` + testClientSecret +
	`","refresh_token":"` + testRefreshToken + `"}`

// loginEnv is one test's sign-in: a fake SDK, a configuration directory, the
// recorded status changes.
type loginTestEnv struct {
	t      *testing.T
	dir    string
	gcloud string
	conf   string
	out    string
	env    []string

	mu      sync.Mutex
	changes []LoginStatus
	changed chan struct{} // signalled (never blocking) after every recorded change
}

func newLoginTestEnv(t *testing.T, mode string) *loginTestEnv {
	t.Helper()
	// Spaces and parentheses, as in C:\Program Files (x86)\Google\Cloud SDK
	// or ~/Library/Application Support.
	e := &loginTestEnv{t: t, dir: filepath.Join(t.TempDir(), "Program Files (x86)", "Cloud SDK"), changed: make(chan struct{}, 1)}
	var err error
	if e.gcloud, err = loginfake.Install(e.dir, loginTestADC); err != nil {
		t.Fatal(err)
	}
	e.conf = filepath.Join(e.dir, "data", "colab", "gcloud")
	e.out = filepath.Join(e.dir, "out")
	if err := os.MkdirAll(e.out, 0o755); err != nil {
		t.Fatal(err)
	}
	e.env = append(os.Environ(), loginfake.EnvMode+"="+mode, loginfake.EnvOut+"="+e.out,
		// the user's own gcloud settings must not reach the sign-in
		"GOOGLE_APPLICATION_CREDENTIALS="+filepath.Join(e.dir, "users-own-adc.json"),
		"CLOUDSDK_CONFIG="+filepath.Join(e.dir, "users-own-config"), "CLOUDSDK_CORE_PROJECT=users-project")
	return e
}

// withChild makes ModeHang run a copy of the test binary inside the fake
// SDK, the way gcloud runs its bundled Python.
func (e *loginTestEnv) withChild() {
	e.t.Helper()
	e.env = append(e.env, loginfake.EnvChild+"="+copyTestBinary(e.t, filepath.Join(e.dir, "google-cloud-sdk", "platform", "bundledpython"), "python"))
}

func copyTestBinary(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, name+filepath.Ext(self))
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, src); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

func (e *loginTestEnv) options() LoginOptions {
	return LoginOptions{
		ConfigDir: e.conf, Gcloud: func() (string, bool) { return e.gcloud, true }, Env: func() []string { return e.env },
		OnChange: func(s LoginStatus) {
			e.mu.Lock()
			e.changes = append(e.changes, s)
			e.mu.Unlock()
			select {
			case e.changed <- struct{}{}:
			default:
			}
		},
	}
}

func (e *loginTestEnv) states() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.changes {
		if len(out) == 0 || out[len(out)-1] != c.State {
			out = append(out, c.State)
		}
	}
	return out
}

// waitChange waits until OnChange has reported a status for which cond
// holds. The Login publishes a status (Status) before it calls OnChange,
// outside its lock (LoginOptions.OnChange), so a test that saw the final
// status through Status must wait for its notification before reading the
// recorded changes.
func (e *loginTestEnv) waitChange(what string, cond func(LoginStatus) bool) {
	e.t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		seen := slices.ContainsFunc(e.changes, cond)
		e.mu.Unlock()
		if seen {
			return
		}
		select {
		case <-e.changed:
		case <-deadline.C:
			e.t.Fatalf("%s: not reported; changes %v", what, e.states())
		}
	}
}

// wait polls the login until cond holds.
func waitLogin(t *testing.T, l *Login, what string, cond func(LoginStatus) bool) LoginStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := l.Status()
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: status stayed %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ended(s LoginStatus) bool {
	return s.State != LoginWaiting && s.State != LoginImporting && s.State != LoginIdle
}

// waitPID waits for {out}/{role}.pid.
func waitPID(t *testing.T, out, role string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(out, role+".pid")); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s child did not start", role)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitGone waits for pid to end.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readOut(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
}

// The whole sign-in: gcloud runs with the scopes of GcloudLoginCommand in
// the sign-in's own configuration directory, its link is parsed, its ADC
// file is imported like "导入授权" (lifecycle kept) and then wiped.
func TestLoginSuccess(t *testing.T) {
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, kv, clk, lb := newCredBackend(t, oauth, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeSuccess)
	l := b.Login(e.options())
	if b.Login(LoginOptions{}) != l {
		t.Fatal("Login made a second sign-in")
	}
	if st := l.Status(); st.State != LoginIdle || st.StartedAt != nil || st.Error != nil {
		t.Fatalf("initial status %+v", st)
	}
	st, err := l.Start(Lifecycle{Kind: LifecycleDays, Days: 3})
	if err != nil || st.State != LoginWaiting || st.StartedAt == nil || !st.StartedAt.Equal(clk.Now()) {
		t.Fatalf("Start: %+v %v", st, err)
	}
	st = waitLogin(t, l, "sign-in", ended)
	if st.State != LoginDone || st.Error != nil || st.URL != loginfake.URL {
		t.Fatalf("final status %+v", st)
	}
	e.waitChange("sign-in", ended)
	if got := e.states(); !slices.Equal(got, []string{LoginWaiting, LoginImporting, LoginDone}) {
		t.Errorf("status changes %v", got)
	}
	info := b.Credential(context.Background())
	if !info.Present || info.Lifecycle != "3d" || info.Source != SourceApp {
		t.Fatalf("credential %+v", info)
	}
	// The stored record says where it came from (disconnect: no gcloud note).
	if v, _, _ := kv.GetKV(context.Background(), KVCredential); !strings.Contains(v, `"source":"app"`) {
		t.Error("the sign-in's credential is not stored with the source app")
	}
	if oauth.calls.Load() != 1 {
		t.Errorf("token endpoint calls: %d", oauth.calls.Load())
	}
	if _, err := os.Stat(e.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sign-in directory is still there: %v", err)
	}
	if got, want := readOut(t, filepath.Join(e.out, "args.txt")), strings.Join(LoginArgs(), " "); got != want {
		t.Errorf("gcloud args %q, want %q", got, want)
	}
	if !strings.Contains(strings.Join(LoginArgs(), " "), "--scopes="+strings.Join(GcloudLoginScopes, ",")) {
		t.Error("LoginArgs do not carry GcloudLoginScopes")
	}
	env := readOut(t, filepath.Join(e.out, "env.txt"))
	for _, want := range []string{"config=" + e.conf, "prompts=1", "usage=true", "gac=", "project="} {
		if !slices.Contains(strings.Split(env, "\n"), want) {
			t.Errorf("gcloud's environment lacks %q:\n%s", want, env)
		}
	}
	// Secrets: not in the status, the changes or the log.
	sb, _ := json.Marshal(st)
	cb, _ := json.Marshal(e.changes)
	assertNoSecrets(t, "login status", string(sb), string(cb), lb.String())
	if v, _, _ := kv.GetKV(context.Background(), KVCredential); !strings.Contains(v, testRefreshToken) {
		t.Error("the credential was not stored")
	}

	// Another sign-in after the first one ended is allowed (a fresh run).
	st, err = l.Start(Lifecycle{Kind: LifecycleManual})
	if err != nil || st.State != LoginWaiting || st.URL != "" {
		t.Fatalf("second Start: %+v %v", st, err)
	}
	if st = waitLogin(t, l, "second sign-in", ended); st.State != LoginDone {
		t.Fatalf("second sign-in %+v", st)
	}
	if info := b.Credential(context.Background()); info.Lifecycle != "manual" {
		t.Fatalf("credential after the second sign-in %+v", info)
	}
}

func TestLoginRefusals(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeSuccess)
	o := e.options()
	o.Gcloud = func() (string, bool) { return "", false }
	l := b.Login(o)
	if _, err := l.Start(Lifecycle{Kind: LifecycleDays, Days: 7}); Code(err) != CodeGcloudMissing {
		t.Fatalf("without gcloud: %v", err)
	}
	if _, err := l.Start(Lifecycle{Kind: "forever"}); Code(err) != CodeLifecycleInvalid {
		t.Fatalf("bad lifecycle: %v", err)
	}
	if st := l.Status(); st.State != LoginIdle || len(e.changes) != 0 {
		t.Fatalf("a refused start changed the status: %+v %v", st, e.changes)
	}
	if err := os.MkdirAll(filepath.Join(e.dir, "data", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", "relative/gcloud", filepath.Join(e.dir, "data")} {
		bad := newLogin(LoginOptions{ConfigDir: dir, Gcloud: func() (string, bool) { return e.gcloud, true }})
		if _, err := bad.Start(Lifecycle{Kind: LifecycleManual}); err == nil {
			t.Errorf("ConfigDir %q accepted", dir)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "data", "keep")); err != nil {
		t.Fatal("a refused ConfigDir was touched")
	}
}

// gcloud that cannot be started: a failed status (shown like any other
// outcome), not an HTTP error.
func TestLoginStartFailed(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeSuccess)
	o := e.options()
	o.Gcloud = func() (string, bool) { return filepath.Join(e.dir, "missing", "gcloud.cmd"), true }
	l := b.Login(o)
	st, err := l.Start(Lifecycle{Kind: LifecycleManual})
	if err != nil || st.State != LoginFailed || st.Error == nil || st.Error.Code != CodeGcloudStartFailed || st.Error.Detail == "" {
		t.Fatalf("Start: %+v %v", st, err)
	}
	if got := e.states(); !slices.Equal(got, []string{LoginFailed}) {
		t.Errorf("changes %v", got)
	}
	// The Login is free again.
	o.Gcloud = func() (string, bool) { return e.gcloud, true }
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); Code(err) == CodeLoginRunning {
		t.Fatal("a failed start kept the sign-in busy")
	}
}

func TestLoginGcloudFails(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeFail)
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	st := waitLogin(t, l, "failing gcloud", ended)
	if st.State != LoginFailed || st.Error == nil || st.Error.Code != CodeLoginFailed || st.Error.Params["exit_code"] != 1 ||
		st.Error.Detail != loginfake.ErrorLine || st.URL != loginfake.URL {
		t.Fatalf("status %+v / %+v", st, st.Error)
	}
	if b.Credential(context.Background()).Present {
		t.Fatal("a credential after a failed sign-in")
	}
}

// gcloud succeeded but wrote no credential: adc_not_found from the import.
func TestLoginWithoutADC(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeNoADC)
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	st := waitLogin(t, l, "no ADC", ended)
	if st.State != LoginFailed || st.Error == nil || st.Error.Code != CodeADCNotFound {
		t.Fatalf("status %+v / %+v", st, st.Error)
	}
	e.waitChange("no ADC", ended)
	if got := e.states(); !slices.Equal(got, []string{LoginWaiting, LoginImporting, LoginFailed}) {
		t.Errorf("changes %v", got)
	}
}

// The account was signed in without the Colab permission (Google's consent
// screen lets the user untick it): the import refuses, nothing is stored
// and the file is wiped all the same.
func TestLoginImportRejected(t *testing.T) {
	oauth := newFakeOAuth(t, okToken("openid https://www.googleapis.com/auth/userinfo.email"))
	b, kv, _, lb := newCredBackend(t, oauth, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeSuccess)
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	st := waitLogin(t, l, "rejected import", ended)
	if st.State != LoginFailed || st.Error == nil || st.Error.Code != CodeADCMissingScope {
		t.Fatalf("status %+v / %+v", st, st.Error)
	}
	if v, ok, _ := kv.GetKV(context.Background(), KVCredential); ok && v != "" {
		t.Fatal("a credential was stored")
	}
	if _, err := os.Stat(e.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sign-in directory is still there: %v", err)
	}
	sb, _ := json.Marshal(st)
	assertNoSecrets(t, "status", string(sb), lb.String())
}

// Cancelling while the browser is open: gcloud's whole process tree ends
// within seconds; a second start meanwhile is refused.
func TestLoginCancel(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeHang)
	e.withChild()
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	pid := waitPID(t, e.out, "python")
	waitLogin(t, l, "link", func(s LoginStatus) bool { return s.URL == loginfake.URL })
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); Code(err) != CodeLoginRunning {
		t.Fatalf("second start: %v", err)
	}
	t0 := time.Now()
	st := l.Cancel(10 * time.Second)
	if st.State != LoginCancelled || st.Error != nil || st.URL != loginfake.URL {
		t.Fatalf("after cancel %+v", st)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
	waitGone(t, pid, "gcloud's python")
	if _, err := os.Stat(e.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sign-in directory is still there: %v", err)
	}
	if got := e.states(); !slices.Equal(got, []string{LoginWaiting, LoginCancelled}) {
		t.Errorf("changes %v", got)
	}
	// Cancelling again: nothing runs, the status stays.
	if st := l.Cancel(time.Second); st.State != LoginCancelled {
		t.Fatalf("second cancel %+v", st)
	}
}

func TestLoginTimeout(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeHang)
	e.withChild()
	o := e.options()
	// The timeout must outlast the start of the fake gcloud and its child:
	// macOS checks every freshly written executable on its first launch
	// (measured 1.0–2.7 s for the script and the copied test binary on the
	// M6 Mac), so 1.5 s ended gcloud before its child had started.
	o.Timeout = 6 * time.Second
	l := b.Login(o)
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	pid := waitPID(t, e.out, "python")
	st := waitLogin(t, l, "timeout", ended)
	if st.State != LoginFailed || st.Error == nil || st.Error.Code != CodeLoginTimeout {
		t.Fatalf("status %+v / %+v", st, st.Error)
	}
	if m, ok := st.Error.Params["minutes"]; !ok || m != 0 { // 6 s rounds to 0 minutes
		t.Errorf("minutes %v", st.Error.Params)
	}
	waitGone(t, pid, "gcloud's python")
	if DefaultLoginTimeout != 10*time.Minute || newLogin(LoginOptions{ConfigDir: filepath.Join(t.TempDir(), "gcloud")}).o.Timeout != DefaultLoginTimeout {
		t.Error("the default timeout is not 10 minutes")
	}
}

// Cancelling during the import (the token request) ends the run as
// cancelled; Close refuses new sign-ins.
func TestLoginCancelDuringImportAndClose(t *testing.T) {
	e := newLoginTestEnv(t, loginfake.ModeSuccess)
	o := e.options()
	importing := make(chan struct{})
	o.importADC = func(ctx context.Context, path string, lc Lifecycle) error {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the ADC file to import: %v", err)
		}
		close(importing)
		<-ctx.Done()
		return ctx.Err()
	}
	l := newLogin(o)
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	<-importing
	if st := l.Status(); st.State != LoginImporting {
		t.Fatalf("status while importing %+v", st)
	}
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); Code(err) != CodeLoginRunning {
		t.Fatalf("start while importing: %v", err)
	}
	if st := l.Cancel(5 * time.Second); st.State != LoginCancelled {
		t.Fatalf("after cancel %+v", st)
	}
	if _, err := os.Stat(e.conf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the ADC file was not wiped: %v", err)
	}
	l.Close()
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err == nil {
		t.Fatal("a sign-in started after Close")
	}
}

// The app quitting (Backend.Close) stops a running sign-in.
func TestLoginStoppedByBackendClose(t *testing.T) {
	b, _, _, _ := newCredBackend(t, nil, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeHang)
	e.withChild()
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleManual}); err != nil {
		t.Fatal(err)
	}
	pid := waitPID(t, e.out, "python")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if st := l.Status(); st.State != LoginCancelled {
		t.Fatalf("after Close %+v", st)
	}
	waitGone(t, pid, "gcloud's python")
}

// A credential an interrupted sign-in left behind is wiped when the
// sign-in is set up (the app starting again).
func TestLoginStartupWipesLeftovers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "colab", "gcloud")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	adc := writeADC(t, dir, "authorized_user")
	newLogin(LoginOptions{ConfigDir: dir})
	if _, err := os.Stat(adc); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover ADC: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover directory: %v", err)
	}
}

// A gcloud that printed no link: the status keeps url "" and still
// completes.
func TestLoginWithoutLink(t *testing.T) {
	oauth := newFakeOAuth(t, okToken(fullScope))
	b, _, _, _ := newCredBackend(t, oauth, "run-1")
	e := newLoginTestEnv(t, loginfake.ModeNoURL)
	l := b.Login(e.options())
	if _, err := l.Start(Lifecycle{Kind: LifecycleSession}); err != nil {
		t.Fatal(err)
	}
	if st := waitLogin(t, l, "no link", ended); st.State != LoginDone || st.URL != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestSignInURL(t *testing.T) {
	for line, want := range map[string]string{
		"    " + loginfake.URL: loginfake.URL,
		loginfake.URL + "\r":   loginfake.URL,
		"Go to the following link in your browser:  https://accounts.google.com/o/oauth2/auth?x=1 ": "https://accounts.google.com/o/oauth2/auth?x=1",
		"http://accounts.google.com/o/oauth2/auth?x=1":                                              "",
		"https://accounts.google.com.example.com/o/oauth2/auth":                                     "",
		"https://example.com/?next=https%3A%2F%2Faccounts.google":                                   "",
		"https://evil.example/https://accounts.google.com/x":                                        "https://accounts.google.com/x",
		"Credentials saved to file: [C:\\x\\adc.json]":                                              "",
	} {
		if got := signInURL(line); got != want {
			t.Errorf("signInURL(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestLoginEnvFilter(t *testing.T) {
	base := []string{"PATH=/bin", "cloudsdk_config=/home/u/.config/gcloud", "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE=/tmp/t",
		"GOOGLE_APPLICATION_CREDENTIALS=/k.json", "CLOUDSDK_PYTHON=/usr/bin/python3", "CLOUDSDK_CORE_DISABLE_PROMPTS=0",
		"HTTPS_PROXY=http://proxy:3128"}
	got := loginEnv(base, "/data/colab/gcloud")
	want := []string{"PATH=/bin", "CLOUDSDK_PYTHON=/usr/bin/python3", "HTTPS_PROXY=http://proxy:3128",
		"CLOUDSDK_CONFIG=/data/colab/gcloud", "CLOUDSDK_CORE_DISABLE_PROMPTS=1", "CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true", "CLOUDSDK_SURVEY_DISABLE_PROMPTS=true"}
	if !slices.Equal(got, want) {
		t.Errorf("loginEnv:\n got %q\nwant %q", got, want)
	}
}

// gcloud's output arrives in arbitrary pieces: lines are reassembled, an
// overlong line is cut, the last ERROR line is the detail (without a link).
func TestLoginOutputLines(t *testing.T) {
	var lines []string
	o := &loginOutput{onLine: func(s string) { lines = append(lines, s) }}
	w := o.writer()
	for _, chunk := range []string{"Your browser", " has been opened to visit:\r\n\r\n    https://accounts.goo", "gle.com/a?b=1\r\n",
		"ERROR: first\n", "ERROR: see https://accounts.google.com/x again\n", "tail without newline"} {
		_, _ = w.Write([]byte(chunk))
	}
	o.flush()
	_, _ = w.Write([]byte(strings.Repeat("x", 3*loginLineMax) + "\n"))
	if len(lines) != 6 || lines[1] != "https://accounts.google.com/a?b=1" || lines[4] != "tail without newline" ||
		len(lines[5]) != loginLineMax {
		t.Fatalf("lines %q", lines[:min(len(lines), 5)])
	}
	if got := o.lastError(); got != "ERROR: see [link] again" {
		t.Errorf("lastError %q", got)
	}
	_, _ = w.Write([]byte("ERROR: " + strings.Repeat("é", loginDetailMax) + "\n"))
	if got := o.lastError(); len(got) > loginDetailMax+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("long ERROR line kept as %d bytes", len(got))
	}
}
