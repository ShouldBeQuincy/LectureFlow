// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Security review SEC13: the manager's own check of custom addresses
// (low-1), no credentials in logs and notices (low-2), and files kept
// across a pause are hashed again (info-4).

func TestCustomURLOK(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"https://pypi.example.com/simple", true},
		{"HTTPS://pypi.example.com/simple", true},
		{"http://127.0.0.1:3141/simple", true},
		{"https://alice:s3cret@pypi.example.com/simple", true},
		{`http://evil\@127.0.0.1/simple`, false},
		{"http://ev\til/simple", false},
		{"http://evil/%zz", false},
		{"http://evil/a b", false},
		{"http://evil/\x7f", false},
		{"http://evil/ ", false},
		{"ftp://example.com/", false},
		{"http:///simple", false},
		{"http://", false},
		{"", false},
		{"https://example.com/" + strings.Repeat("a", MaxCustomURLLen), false},
	} {
		if got := CustomURLOK(c.s); got != c.want {
			t.Errorf("CustomURLOK(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// What Go cannot parse is refused by the manager itself, whichever caller
// hands it over.
func TestManagerRefusesUnparsableCustomURL(t *testing.T) {
	e := newTestEnv(t)
	man := e.manifest(defaultSpec())
	for _, g := range []string{GroupPyPI, GroupModels} {
		for _, bad := range []string{`http://evil\@127.0.0.1/simple`, "http://ev\til/simple", "http://evil/%zz", "HTTP://evil/%zz"} {
			s := Sources{}
			if g == GroupPyPI {
				s.PyPI = bad
			} else {
				s.Models = bad
			}
			err := man.ValidateSources(s)
			if err == nil {
				t.Errorf("%s %q accepted", g, bad)
				continue
			}
			info := ErrorInfoOf(err)
			if info.Code != CodeSourcesInvalid || info.Params["group"] != g || info.Params["field"] != "custom_url" {
				t.Errorf("%s %q: %+v", g, bad, info)
			}
			if g == GroupPyPI {
				if _, err := man.indexURLs(s); CodeOf(err) != CodeSourcesInvalid {
					t.Errorf("indexURLs %q: %v", bad, err)
				}
			}
		}
	}
	if !InsecureCustomSource(GroupPyPI, `http://evil\@127.0.0.1/simple`) {
		t.Error("an unparsable http index counts as safe")
	}
	// an upper-case scheme is a custom address, not an unknown mirror id
	s := Sources{PyPI: "HTTPS://pypi.example.com/simple"}
	if err := man.ValidateSources(s); err != nil {
		t.Errorf("upper-case scheme: %v", err)
	}
	// uv gets the scheme in lower case (uv 0.13.1 reads "HTTPS://…" as a
	// relative path); the host is left as given
	urls, err := man.indexURLs(s)
	if err != nil || len(urls) == 0 || urls[0].Mirror != s.PyPI || urls[0].URL != "https://pypi.example.com/simple" {
		t.Errorf("upper-case scheme: %+v %v", urls, err)
	}
	urls, _ = man.indexURLs(Sources{PyPI: "Http://LocalHost:3141/Simple"})
	if len(urls) == 0 || urls[0].URL != "http://LocalHost:3141/Simple" {
		t.Errorf("mixed case: %+v", urls)
	}
}

// The upper-case custom index reaches uv in lower case.
func TestUpperCaseSchemeToUV(t *testing.T) {
	e := newTestEnv(t)
	uvlog := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", uvlog)
	m := e.open()
	custom := "HTTP://" + strings.TrimPrefix(e.url("/default/simple"), "http://")
	mustInstall(t, m, Sources{PyPI: custom}, IDPython)
	calls := syncCalls(t, uvlog)
	if len(calls) == 0 {
		t.Fatal("no uv sync")
	}
	for _, args := range calls {
		if got := flagValue(args, "--index-url"); got != e.url("/default/simple") {
			t.Errorf("uv --index-url %q, want %q", got, e.url("/default/simple"))
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://alice:s3cret@pypi.example.com/simple?x=1":                        "https://pypi.example.com/simple?x=1",
		"http://alice@127.0.0.1:3141/simple":                                      "http://127.0.0.1:3141/simple",
		"https://pypi.example.com/simple":                                         "https://pypi.example.com/simple",
		`http://evil\:s3cret@127.0.0.1/simple`:                                    "http://127.0.0.1/simple",
		"tuna":                                                                    "tuna",
		"https://a@b:pass@pypi.example.com/simple":                                "https://pypi.example.com/simple",
		"https://hf.example.com/x?token=abc&rev=main":                             "https://hf.example.com/x?token=***&rev=main",
		"https://s3.example.com/o?X-Amz-Signature=f00&X-Amz-Credential=c1&part=2": "https://s3.example.com/o?X-Amz-Signature=***&X-Amz-Credential=***&part=2",
		"https://example.com/p?API_KEY=k&Password=p&page=3":                       "https://example.com/p?API_KEY=***&Password=***&page=3",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
	text := `Get "https://alice:s3cret@hf.example.com/x": EOF; also http://bob:pw@192.0.2.1/y and mailto:a@b; ` +
		`http://a@carol:pw2@192.0.2.2/z?secret=pw3&n=1`
	got := redactText(text)
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") || strings.Contains(got, "bob:pw") ||
		strings.Contains(got, "carol") || strings.Contains(got, "pw2") || strings.Contains(got, "pw3") ||
		!strings.Contains(got, "http://192.0.2.2/z?secret=***&n=1") ||
		!strings.Contains(got, `"https://hf.example.com/x"`) || !strings.Contains(got, "http://192.0.2.1/y") {
		t.Errorf("redactText: %s", got)
	}
}

// syncBuffer is a log sink for the manager's goroutines.
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

// Custom sources with a user name and password: uv and the downloader get
// them, the log, the notices and the errors do not.
func TestCredentialsStayOutOfLogs(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("LF_FAKE_UV_FAIL_INDEX", "/mirror/")
	uvlog := filepath.Join(t.TempDir(), "uv.log")
	t.Setenv("LF_FAKE_UV_LOG", uvlog)
	var logs syncBuffer
	opts := e.options()
	opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	host := strings.TrimPrefix(e.url(""), "http://")
	src := Sources{
		PyPI:   "http://alice:s3cret@" + host + "/mirror/simple",
		Models: "http://alice:s3cret@" + host + "/nowhere",
	}
	st := mustInstall(t, m, src, IDASRCUDA, ovModel)
	calls, _ := os.ReadFile(uvlog)
	if !strings.Contains(string(calls), "alice:s3cret@") {
		t.Errorf("uv did not get the index as configured: %s", calls)
	}
	out := logs.String()
	if !strings.Contains(out, "trying the next one") {
		t.Fatalf("no fallback was logged: %s", out)
	}
	if strings.Contains(out, "s3cret") || strings.Contains(out, "alice") {
		t.Errorf("credentials in the log:\n%s", out)
	}
	if len(st.Notices) == 0 {
		t.Fatal("no fallback notice")
	}
	for _, n := range st.Notices {
		if s := fmt.Sprint(n.Params, n.Detail); strings.Contains(s, "s3cret") || strings.Contains(s, "alice") {
			t.Errorf("credentials in a notice: %+v", n)
		}
	}
}

// A file moved into staging before a pause is hashed again on resume; one
// that changed meanwhile is obtained again.
func TestStagedFileRehashedOnResume(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	mustInstall(t, m, Sources{}, IDASRCUDA) // the model's environment first
	seed := defaultSpec().modelSeed
	binPath := fmt.Sprintf("/default/hf/tiny/%d/model.bin", seed)
	hit, release := e.srv.gate(fmt.Sprintf("/default/hf/tiny/%d/config.json", seed))
	id, err := m.Install([]string{fwFloat16}, Sources{})
	if err != nil {
		t.Fatal(err)
	}
	<-hit // model.bin is in staging now
	if err := m.Pause(id); err != nil {
		t.Fatal(err)
	}
	wait(t, m, id, TaskPaused)
	release()
	staged, _ := filepath.Glob(filepath.Join(e.data, "components", DirName(fwFloat16), ".s*", "model", "model.bin"))
	if len(staged) != 1 {
		t.Fatalf("staged model.bin: %v", staged)
	}
	fi, err := os.Stat(staged[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged[0], bytes.Repeat([]byte{'x'}, int(fi.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(id); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, m, id, TaskDone, TaskFailed); st.State != TaskDone {
		t.Fatalf("after resume: %+v", st.Error)
	}
	dir, ok := m.ModelDir(BackendFasterWhisper, "tiny", "float16")
	if !ok {
		t.Fatal("model not installed")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "model.bin"))
	if !bytes.Equal(got, e.content["model.bin"]) {
		t.Error("the changed staged file was kept")
	}
	if n := e.srv.count(binPath); n != 2 {
		t.Errorf("model.bin downloaded %d times, want 2", n)
	}
}
