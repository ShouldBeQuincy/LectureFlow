// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// SEC16 R2: with aria2c on the system's resolver (its features read:
// --async-dns=false given, or no Async DNS built in), a name Go's look-up
// fails on is refused (downloads.network) rather than handed to aria2c,
// which would look it up again and could get any answer; unknown features
// keep the old behaviour (left to aria2c); a proxied URL is not looked up.
func TestLookupFailureRefusedOnSystemResolver(t *testing.T) {
	ctx := context.Background()
	engineWith := func(version string, err error) *engine {
		e := &engine{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			versionOf: func(string) (string, error) { return version, err }}
		e.features("aria2c")
		return e
	}
	for _, c := range []struct {
		name   string
		eng    *engine
		proxy  string
		code   string
		lookup int
	}{
		{"windows build", engineWith(fakeVersion(featuresWindows), nil), "", CodeNetwork, 1},
		{"macos build", engineWith(fakeVersion(featuresMac), nil), "", CodeNetwork, 1},
		{"version unreadable", engineWith("", errors.New("exit status 1")), "", "", 1},
		{"no features line", engineWith("aria2 version 1.37.0\n", nil), "", "", 1},
		{"not started", nil, "", "", 1},
		{"proxied", engineWith(fakeVersion(featuresWindows), nil), "http://proxy.school.example:3128", "", 0},
	} {
		lookups := map[string]int{}
		p := testPolicy(false, lookups)
		p.proxy = func(*url.URL) (*url.URL, error) {
			if c.proxy == "" {
				return nil, nil
			}
			return url.Parse(c.proxy)
		}
		r := (&Queue{eng: c.eng}).newResolver(runSnapshot{net: &taskNet{addr: p.forTask("lms.publicuni.example")}})
		for i := 0; i < 2; i++ { // (the answer is kept per host)
			err := r.checkHost(ctx, "https://cdn.unknown.example/seg0.ts")
			got := ""
			var f *failure
			if errors.As(err, &f) {
				got = f.code
				if !strings.Contains(f.detail, "cdn.unknown.example") {
					t.Errorf("%s: detail %q", c.name, f.detail)
				}
			} else if err != nil {
				got = err.Error()
			}
			if got != c.code {
				t.Errorf("%s: %q, want %q", c.name, got, c.code)
			}
		}
		if lookups["cdn.unknown.example"] != c.lookup {
			t.Errorf("%s: lookups %v", c.name, lookups)
		}
		// a name that resolves is judged by its addresses as before
		if err := r.checkHost(ctx, "https://cdn.example.com/seg0.ts"); err != nil {
			t.Errorf("%s: a public name: %v", c.name, err)
		}
	}
	if (*engine)(nil).systemResolver() {
		t.Error("no engine counts as the system's resolver")
	}
}

// The whole queue (the Windows build's features): a segment on a name the
// look-up cannot answer fails the task with downloads.network and is never
// given to aria2c. (The task has no cookies and the segment is on another
// origin than the page, so Go does not request it itself; the tests' look-up
// knows no name, and nothing is resolved for real.)
func TestQueueRefusesUnresolvedSegmentHost(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.mu.Lock()
	e.media.cookie = "" // a platform without a session cookie
	e.media.mu.Unlock()
	// should the check let it through, the fake aria2c dials this
	// computer, not a name looked up for real
	e.aria.mu.Lock()
	e.aria.hostsAt["cdn.unknown.example"] = "127.0.0.1"
	e.aria.mu.Unlock()
	e.media.put("/seg/index.m3u8", []byte("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6,\n"+
		"http://cdn.unknown.example/files/seg0.ts\n#EXT-X-ENDLIST\n"))
	req := e.request("nx", e.hlsTrack(bridge.KindVideo, "/seg/index.m3u8"))
	req.Cookies = nil
	info := e.wait(e.enqueue(req), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeNetwork {
		t.Fatalf("%+v", info.Error)
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("aria2c was given %+v", adds)
	}
}

// SEC16 info-3: classifyHost takes a Hostname(); a Host with a port, user
// info or brackets is refused as unusual instead of read as a public name
// (looked up, a failure let through).
func TestClassifyHostRefusesHostForms(t *testing.T) {
	for _, h := range []string{"127.0.0.1:80", "10.0.0.1:443", "[::1]:80", "[::1]", "localhost:80", "0x7f.1:80",
		"127.0.0.1:x;y", "user@127.0.0.1", "a:b@cdn.example.com", "cdn.example.com:443", "[fe80::1%25eth0]"} {
		if class, final := classifyHost(h); class != classUnusual || !final {
			t.Errorf("classifyHost(%q) = %d %v, want unusual, final", h, class, final)
		}
	}
	// a Hostname() as before, IPv6 with its zone included
	for h, want := range map[string]addrClass{"::1": classLoopback, "fe80::1%eth0": classLocalNet, "::ffff:10.0.0.1": classLocalNet,
		"2001:db8::1": classPublic, "cdn.example.com": classPublic, "127.0.0.1": classLoopback} {
		if class, _ := classifyHost(h); class != want {
			t.Errorf("classifyHost(%q) = %d, want %d", h, class, want)
		}
	}
}

// SEC16 info-7: the local and loopback codes carry the host in safeHost's
// form too, bounded in length, like site_blocked and unusual_host.
func TestLocalCodesHostIsSafe(t *testing.T) {
	long := strings.Repeat("a", 400) + ".example"
	for _, c := range []struct {
		err  error
		code string
		want string
	}{
		{&localAddrError{host: "fe80::1%en0"}, CodeLocalAddress, "fe80::1%25en0"},
		{&localAddrError{host: "x‮.campus.example"}, CodeLocalAddress, "x%E2%80%AE.campus.example"},
		{&localAddrError{host: "localhost", loopback: true}, CodeLoopbackAddress, "localhost"},
		{&url.Error{Op: "Get", URL: "http://ｌｏｃａｌｈｏｓｔ/x", Err: &localAddrError{host: "y", loopback: true}}, CodeLoopbackAddress,
			"%EF%BD%8C%EF%BD%8F%EF%BD%83%EF%BD%81%EF%BD%8C%EF%BD%88%EF%BD%8F%EF%BD%93%EF%BD%94"},
	} {
		f := localFailure(c.err)
		if f == nil || f.code != c.code || f.params["host"] != c.want || !strings.HasSuffix(f.detail, ": "+c.want) {
			t.Errorf("%v: %+v, want host %q", c.err, f, c.want)
		}
	}
	f := localFailure(&localAddrError{host: long})
	if h, _ := f.params["host"].(string); len(h) > 260 || !strings.HasPrefix(long, h) {
		t.Errorf("a long host: %d bytes", len(h))
	}
}
