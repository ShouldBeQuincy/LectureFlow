// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Path grants of the desktop path uploads (pathgrant.go; security audit FB1
// L-7, T-SEC-08). Nothing here opens a network path: the network cases are
// refused before the file system is asked, or they meet a stand-in file
// system (the hosts are reserved names and TEST-NET addresses).

func TestNetworkPath(t *testing.T) {
	for p, want := range map[string]bool{
		`\\fileserver.example\share\week1.mp4`:       true,
		`//fileserver.example/share/week1.mp4`:       true,
		`\\192.0.2.10\share\week1.mp4`:               true,
		`/\192.0.2.10\share\week1.mp4`:               true,
		` \\fileserver.example\share\week1.mp4`:      true,
		`\\?\UNC\fileserver.example\share\week1.mp4`: true,
		`//?/UNC/fileserver.example/share/week1.mp4`: true,
		`\\.\UNC\fileserver.example\share\week1.mp4`: true,
		`\\.\pipe\lf`:                                    true,
		`\\?\C:\Users\alice\week1.mp4`:                   true, // extended-length: a device form
		`\??\C:\Users\alice\week1.mp4`:                   true,
		`\\fileserver.example`:                           true,
		`C:\Users\alice\Videos\week1.mp4`:                false,
		`C:/Users/alice/Videos/week1.mp4`:                false,
		`C:\Users\alice\..\bob\week1.mp4`:                false,
		`/home/alice/week1.mp4`:                          false,
		`/home/alice/../bob/week1.mp4`:                   false,
		`week1.mp4`:                                      false,
		`..\week1.mp4`:                                   false,
		`\Users\alice\week1.mp4`:                         false, // rooted on the current drive: local
		``:                                               false,
		`D:\lectures\\fileserver.example\share\x.mp4`:    false, // a doubled separator inside a local path
		`file://fileserver.example/share/week1.mp4`:      false, // not absolute: path_invalid before any grant
		`\\fileserver.example\share\..\..\week1.mp4`:     true,
		`//fileserver.example/share/./week1.mp4`:         true,
		`\\fileserver.example\share\week1.mp4\`:          true,
		`\\FILESERVER.EXAMPLE\SHARE\WEEK1.MP4`:           true,
		`\\192.0.2.10@SSL@443\DavWWWRoot\week1.mp4`:      true, // WebDAV redirector
		`\\fileserver.example@8080\share\week1.mp4`:      true,
		`\\[2001:db8::1]\share\week1.mp4`:                true,
		`\\2001-db8--1.ipv6-literal.net\share\week1.mp4`: true,
	} {
		if got := networkPath(p); got != want {
			t.Errorf("networkPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestPathGrantKey(t *testing.T) {
	same := func(a, b string, windows bool) {
		t.Helper()
		if ka, kb := grantKeyFor(a, windows), grantKeyFor(b, windows); ka != kb {
			t.Errorf("%q and %q: keys %q and %q differ", a, b, ka, kb)
		}
	}
	differ := func(a, b string, windows bool) {
		t.Helper()
		if ka, kb := grantKeyFor(a, windows), grantKeyFor(b, windows); ka == kb {
			t.Errorf("%q and %q: same key %q", a, b, ka)
		}
	}
	if runtime.GOOS == "windows" {
		// filepath.Clean is the Windows one here
		same(`C:\Users\Alice\Videos\Week 1.MP4`, `c:/users/alice/videos/week 1.mp4`, true)
		same(`C:\Users\alice\Videos\week1.mp4`, `C:\Users\alice\Videos\.\x\..\week1.mp4`, true)
		same(`C:\Users\alice\Videos\week1.mp4`, `C:\Users\alice\\Videos\week1.mp4\`, true)
		same(`\\FileServer.example\Share\week1.mp4`, `//fileserver.example/share/week1.mp4`, true)
		differ(`C:\Users\alice\week1.mp4`, `C:\Users\alice\week2.mp4`, true)
		differ(`C:\Progra~1\week1.mp4`, `C:\Program Files\week1.mp4`, true) // 8.3 names are not expanded
		differ(`C:\Übung\week1.mp4`, `C:\übung\week1.mp4`, true)            // only ASCII letters fold
		differ(`C:\granted\..\secret.txt`, `C:\granted\secret.txt`, true)
		differ(`\\fileserver.example\share\week1.mp4`, `C:\share\week1.mp4`, true)
	}
	if runtime.GOOS != "windows" {
		same("/home/alice/week1.mp4", "/home/alice//./x/../week1.mp4/", false)
		differ("/home/Alice/week1.mp4", "/home/alice/week1.mp4", false) // case matters here
		differ("/home/alice/granted/../secret.txt", "/home/alice/granted/secret.txt", false)
	}
	if pathGrantKey("week1.mp4") != grantKeyFor("week1.mp4", runtime.GOOS == "windows") {
		t.Error("pathGrantKey is grantKeyFor for this system")
	}
}

// testGrants is a store on a clock the test moves.
func testGrants(now *time.Time) *pathGrants {
	g := newPathGrants()
	g.now = func() time.Time { return *now }
	g.wait = 0
	return g
}

func tryTake(g *pathGrants, p string) (func(), bool) {
	return g.take(context.Background(), p)
}

func grantCount(g *pathGrants) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// use takes a grant for a request that succeeds (grantScope renews then).
func use(g *pathGrants, p string) bool {
	_, ok := tryTake(g, p)
	if ok {
		g.renew()
	}
	return ok
}

func TestPathGrantsOneTimeExpiryAndLimit(t *testing.T) {
	if pathGrantTTL != 12*time.Hour || pathGrantPerKey != 8 {
		t.Fatalf("pathGrantTTL = %v, pathGrantPerKey = %d (SEC24 M1: 12 hours; SEC24b: 8)", pathGrantTTL, pathGrantPerKey)
	}
	now := time.Unix(1_000_000, 0)
	g := testGrants(&now)
	a, b, c := abs("alice", "a.mp4"), abs("alice", "b.mp4"), abs("alice", "c.mp4")

	// used once, then gone; another path is no match
	g.add([]string{a})
	if _, ok := tryTake(g, b); ok {
		t.Fatal("a grant for another path was used")
	}
	if _, ok := tryTake(g, a); !ok {
		t.Fatal("granted path refused")
	}
	if _, ok := tryTake(g, a); ok {
		t.Fatal("a grant was used twice")
	}
	// registered twice before its first use (the same PDF dropped on two
	// lectures): usable twice (SEC24b); a path named twice in one
	// registration counts once
	g.add([]string{a, a})
	g.add([]string{a})
	if n := grantCount(g); n != 1 {
		t.Fatalf("one path takes %d places", n)
	}
	for i := 0; i < 2; i++ {
		if _, ok := tryTake(g, a); !ok {
			t.Fatalf("second registration, use %d refused", i+1)
		}
	}
	if _, ok := tryTake(g, a); ok {
		t.Fatal("third use of two grants")
	}
	// registered a thousand times: one place, at most pathGrantPerKey uses
	for i := 0; i < 1000; i++ {
		g.add([]string{a})
	}
	if n := grantCount(g); n != 1 {
		t.Fatalf("a thousand registrations of one path take %d places", n)
	}
	for i := 0; i < pathGrantPerKey; i++ {
		if _, ok := tryTake(g, a); !ok {
			t.Fatalf("use %d of %d refused", i+1, pathGrantPerKey)
		}
	}
	if _, ok := tryTake(g, a); ok {
		t.Fatalf("more than %d uses of one path", pathGrantPerKey)
	}

	// expiry: valid until pathGrantTTL after the registration
	g.add([]string{a, b, c})
	now = now.Add(pathGrantTTL - time.Second)
	restore, ok := tryTake(g, a)
	if !ok {
		t.Fatal("refused before the expiry")
	}
	// a request that succeeded renews every unexpired grant
	g.renew()
	now = now.Add(pathGrantTTL - time.Second)
	if !use(g, b) {
		t.Fatal("the successful request did not renew the others")
	}
	// a failed request gives its grant back
	restore()
	if _, ok := tryTake(g, a); !ok {
		t.Fatal("returned grant refused")
	}
	now = now.Add(pathGrantTTL)
	if _, ok := tryTake(g, c); ok {
		t.Fatal("expired grant used")
	}
	g.mu.Lock()
	g.sweepLocked(now)
	g.mu.Unlock()
	if n := grantCount(g); n != 0 {
		t.Fatalf("expired grants kept: %d", n)
	}
	// an expired grant is not brought back by a late failure either
	g.add([]string{a})
	restore, _ = tryTake(g, a)
	now = now.Add(2 * pathGrantTTL)
	restore()
	if _, ok := tryTake(g, a); ok {
		t.Fatal("an expired grant came back")
	}
	// a refund next to a new registration: both grants count, one place
	g.add([]string{a})
	restore, _ = tryTake(g, a)
	g.add([]string{a})
	restore()
	if n := grantCount(g); n != 1 {
		t.Fatalf("one path takes %d places after a refund", n)
	}
	for i := 0; i < 2; i++ {
		if _, ok := tryTake(g, a); !ok {
			t.Fatalf("refunded and new grant: use %d refused", i+1)
		}
	}

	// at most g.max paths: the oldest registration goes first
	g.max = 3
	old1, old2 := abs("alice", "old1.mp4"), abs("alice", "old2.mp4")
	g.add([]string{old1, old2})
	held, _ := tryTake(g, old2) // in use while its batch is dropped
	g.add([]string{a})
	g.add([]string{b, c})
	if _, ok := tryTake(g, old1); ok {
		t.Fatal("the oldest registration was kept beyond the limit")
	}
	held()
	if _, ok := tryTake(g, old2); ok {
		t.Fatal("a dropped registration's grant came back")
	}
	for _, p := range []string{a, b, c} {
		if _, ok := tryTake(g, p); !ok {
			t.Fatalf("%s: newer grant dropped", p)
		}
	}
	if n := grantCount(g); n != 0 {
		t.Fatalf("count %d after all were used", n)
	}
	// re-registering a path the store holds takes no room: the other picks stay
	g.add([]string{old1})
	g.add([]string{a, b})
	for i := 0; i < 1000; i++ {
		g.add([]string{old1})
	}
	for _, p := range []string{a, b, old1} {
		if _, ok := tryTake(g, p); !ok {
			t.Fatalf("%s: pushed out by re-registrations", p)
		}
	}
	// one registration larger than the limit keeps no more than the limit
	g.add([]string{a, b, c, old1, old2})
	if n := grantCount(g); n > g.max {
		t.Fatalf("%d grants kept, limit %d", n, g.max)
	}
}

// SEC24 M1: the page copies one file per course at a time. A second drop
// waits behind the first one's copies; those may take hours on a slow
// source, and the second drop's grants stay valid as long as copies
// succeed.
func TestPathGrantsLongUploadQueue(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	g := testGrants(&now)
	first := []string{abs("alice", "w1.mp4"), abs("alice", "w2.mp4"), abs("alice", "w3.mp4")}
	second := []string{abs("alice", "w4.mp4"), abs("alice", "w5.mp4")}
	g.add(first)
	if !use(g, first[0]) {
		t.Fatal("first copy refused")
	}
	now = now.Add(time.Minute)
	g.add(second) // dropped while the first copy runs
	// each copy of the first drop takes 20 minutes
	for _, p := range first[1:] {
		now = now.Add(20 * time.Minute)
		if !use(g, p) {
			t.Fatalf("%s refused after %v", p, now.Sub(time.Unix(2_000_000, 0)))
		}
	}
	now = now.Add(20 * time.Minute)
	if !use(g, second[0]) {
		t.Fatal("the second drop expired behind 20-minute copies")
	}
	// a queue that runs for most of a day: each successful use renews the rest
	now = now.Add(11 * time.Hour)
	g.add([]string{abs("alice", "w6.mp4")}) // a pick elsewhere, used at once
	if !use(g, abs("alice", "w6.mp4")) {
		t.Fatal("fresh grant refused")
	}
	now = now.Add(11 * time.Hour) // 22 hours after w5's renewal by the w4 copy, 11 after the w6 use
	if !use(g, second[1]) {
		t.Fatal("the w6 use did not renew w5")
	}
	// nothing used for 12 hours: everything expires
	g.add([]string{abs("alice", "w7.mp4")})
	now = now.Add(pathGrantTTL)
	if _, ok := tryTake(g, abs("alice", "w7.mp4")); ok {
		t.Fatal("a grant outlived 12 hours without any use")
	}
}

// SEC24b: a request that takes a grant and then fails gives it back
// without renewing anything, so failing on purpose keeps no grant alive;
// one success does renew.
func TestPathGrantsFailuresDoNotRenew(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	g := testGrants(&now)
	held, other := abs("alice", "held.mp4"), abs("alice", "other.mp4")
	g.add([]string{held, other})
	start := now
	deadline := func(p string) time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		if e := g.m[pathGrantKey(p)]; e != nil {
			return e.batch.deadline
		}
		return time.Time{}
	}
	want := start.Add(pathGrantTTL)
	for i := 0; i < 5; i++ { // every 2 hours a request takes held and fails
		now = now.Add(2 * time.Hour)
		restore, ok := tryTake(g, held)
		if !ok {
			t.Fatalf("round %d: held refused", i+1)
		}
		restore()
		if d := deadline(other); !d.Equal(want) {
			t.Fatalf("round %d: a failed request moved the expiry to %v (want %v)", i+1, d, want)
		}
	}
	now = start.Add(pathGrantTTL)
	if _, ok := tryTake(g, other); ok {
		t.Fatal("failed requests kept a grant alive past 12 hours")
	}
	if _, ok := tryTake(g, held); ok {
		t.Fatal("the grant that failed on purpose outlived 12 hours")
	}

	// the same through grantScope's ledger: refund on failure, renew on success
	now = time.Unix(4_000_000, 0)
	g.add([]string{held, other})
	want = now.Add(pathGrantTTL)
	now = now.Add(time.Hour)
	l := &grantLedger{}
	ctx := context.WithValue(context.Background(), grantLedgerKey{}, l)
	restore, ok := g.take(ctx, held)
	if !keepRestore(ctx, g, restore, ok) {
		t.Fatal("held refused")
	}
	l.refund()
	if d := deadline(other); !d.Equal(want) {
		t.Fatalf("a refunded request renewed: %v", d)
	}
	l = &grantLedger{}
	ctx = context.WithValue(context.Background(), grantLedgerKey{}, l)
	restore, ok = g.take(ctx, held)
	if !keepRestore(ctx, g, restore, ok) {
		t.Fatal("refunded grant refused")
	}
	if d := deadline(other); !d.Equal(want) {
		t.Fatalf("renewed before the request ended: %v", d)
	}
	l.succeeded()
	if d := deadline(other); !d.Equal(now.Add(pathGrantTTL)) {
		t.Fatalf("a successful request did not renew: %v, want %v", d, now.Add(pathGrantTTL))
	}
}

func abs(parts ...string) string {
	root := "/home"
	if runtime.GOOS == "windows" {
		root = `C:\Users`
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// A drop's grant may arrive just after the upload: take waits for it, but
// not beyond g.wait, and not after the request went away.
func TestPathGrantsWaitForALateGrant(t *testing.T) {
	g := newPathGrants()
	g.wait = 5 * time.Second
	p := abs("alice", "dropped.mp4")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(30 * time.Millisecond)
		g.add([]string{abs("alice", "other.mp4")}) // wakes the waiter, no match
		time.Sleep(30 * time.Millisecond)
		g.add([]string{p})
	}()
	start := time.Now()
	if _, ok := g.take(context.Background(), p); !ok {
		t.Fatal("late grant not seen")
	}
	wg.Wait()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}

	g.wait = 50 * time.Millisecond
	start = time.Now()
	if _, ok := g.take(context.Background(), abs("alice", "never.mp4")); ok {
		t.Fatal("no grant, but taken")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waited %v for a grant that never came", d)
	}

	g.wait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	if _, ok := g.take(ctx, abs("alice", "never.mp4")); ok {
		t.Fatal("no grant, but taken")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a gone request waited %v", d)
	}
}

// fsSpy replaces checkSourcePath's file system calls and records them.
type fsSpy struct {
	mu    sync.Mutex
	calls []string
}

func (f *fsSpy) install(s *Server, eval func(string) (string, error)) {
	s.evalSymlinks = func(p string) (string, error) {
		f.mu.Lock()
		f.calls = append(f.calls, "eval "+p)
		f.mu.Unlock()
		return eval(p)
	}
	s.statSource = func(p string) (fs.FileInfo, error) {
		f.mu.Lock()
		f.calls = append(f.calls, "stat "+p)
		f.mu.Unlock()
		return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
}

func (f *fsSpy) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// Without a grant a network path (and any other path) is refused before
// the file system is asked about it; nothing is created.
func TestPathUploadRefusesUngrantedPathBeforeFS(t *testing.T) {
	quietLog(t)
	spy := &fsSpy{}
	e := newEnv(t, func(s *Server) {
		spy.install(s, func(p string) (string, error) {
			return "", &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
		})
		s.grants.wait = 20 * time.Millisecond
	})
	c := e.course("C")
	d := e.draft(c.ID, "L") // (the fixture helpers send no token)
	e.desktopOn()
	local := writeSrc(t, t.TempDir(), "week1.mp4", []byte("x"))
	// the exact refusal of the plain UNC form on this system
	plain := `//fileserver.example/share/week1.mp4` // absolute on POSIX
	if runtime.GOOS == "windows" {
		plain = `\\fileserver.example\share\week1.mp4`
	}
	paths := []string{
		plain,
		`\\fileserver.example\share\week1.mp4`,
		`//fileserver.example/share/week1.mp4`,
		`\\192.0.2.10\share\week1.mp4`,
		`/\192.0.2.10\share\week1.mp4`,
		`\\?\UNC\fileserver.example\share\week1.mp4`,
		`//?/UNC/192.0.2.10/share/week1.mp4`,
		`\\.\UNC\fileserver.example\share\week1.mp4`,
		`\\192.0.2.10@SSL@443\DavWWWRoot\week1.mp4`,
		local, // a local file the user did not pick
	}
	// another path was granted: no help for these
	e.grant(`\\fileserver.example\share\other.mp4`, `//fileserver.example/share/other.mp4`, filepath.Join(filepath.Dir(local), "other.mp4"))
	for _, ep := range []string{"/api/courses/" + c.ID + "/lectures", "/api/courses/" + c.ID + "/materials", "/api/courses/" + c.ID + "/syllabus/raw"} {
		for i, p := range paths {
			res, b := e.ungrantedUpload(ep, map[string]any{"path": p})
			code := errCode(t, b)
			ok := (res.StatusCode == http.StatusForbidden && code == "upload.path_not_allowed") ||
				(res.StatusCode == http.StatusBadRequest && code == "upload.path_invalid")
			if !ok || ((i == 0 || p == local) && code != "upload.path_not_allowed") {
				t.Errorf("%s %q: %d %s", ep, p, res.StatusCode, b)
			}
			if calls := spy.take(); len(calls) != 0 {
				t.Errorf("%s %q: the file system was asked: %v", ep, p, calls)
			}
		}
	}
	var list []Lecture
	res, b := e.doWith("GET", "/api/courses/"+c.ID+"/lectures", nil, testToken, "")
	decodeInto(t, b, &list)
	if res.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("refused uploads created lectures: %s", b)
	}
	// the other endpoints that take a user's path the same way
	for _, call := range []struct{ method, ep string }{
		{"POST", "/api/lectures/" + d.ID + "/angles"},
		{"PUT", "/api/lectures/" + d.ID + "/media"},
	} {
		body, _ := json.Marshal(map[string]any{"path": plain})
		res, b := e.doWith(call.method, call.ep, body, testToken, "")
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != "upload.path_not_allowed" {
			t.Errorf("%s %s: %d %s", call.method, call.ep, res.StatusCode, b)
		}
		if calls := spy.take(); len(calls) != 0 {
			t.Errorf("%s %s: the file system was asked: %v", call.method, call.ep, calls)
		}
	}
}

// A network path the user did pick passes the grant and reaches the file
// system (here a stand-in: nothing leaves the machine).
func TestPathUploadGrantedNetworkPath(t *testing.T) {
	spy := &fsSpy{}
	e := newEnv(t, func(s *Server) {
		spy.install(s, func(p string) (string, error) {
			return "", &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
		})
	})
	c := e.course("C")
	e.desktopOn()
	p := `//fileserver.example/share/week1.mp4`
	if runtime.GOOS == "windows" {
		p = `\\fileserver.example\share\week1.mp4`
	}
	e.grant(p)
	res, b := e.ungrantedUpload("/api/courses/"+c.ID+"/lectures", map[string]any{"path": p})
	if res.StatusCode != http.StatusNotFound || errCode(t, b) != "upload.path_not_found" {
		t.Fatalf("granted network path: %d %s", res.StatusCode, b)
	}
	if calls := spy.take(); len(calls) != 1 || calls[0] != "eval "+filepath.Clean(p) {
		t.Fatalf("file system calls %v", calls)
	}
}

// One grant, one upload; a failed request returns it (retry after freeing
// space works), a successful one uses it up.
func TestPathGrantUsedOnceReturnedOnFailure(t *testing.T) {
	var free int64
	e := newEnv(t, func(s *Server) {
		s.freeSpace = func(string) (int64, error) { return free, nil }
		s.grants.wait = 20 * time.Millisecond
	})
	free = e.srv.minFree + 100<<30
	c := e.course("C")
	e.desktopOn()
	ep := "/api/courses/" + c.ID + "/lectures"
	// Both failures come after checkSourcePath has used the grant (SEC24
	// T1): the request-level checkSpace passes (the JSON body is small);
	// importLocal's check of the file's own size, and the size limit, fail.
	for _, tc := range []struct {
		name         string
		fail, unfail func()
		status       int
		code         string
	}{
		{"no room for the file", func() { free = e.srv.minFree + 1000 }, func() { free = e.srv.minFree + 100<<30 }, http.StatusInsufficientStorage, "upload.no_space"},
		{"over the size limit", func() { e.srv.maxUpload = 3 }, func() { e.srv.maxUpload = 4 << 30 }, http.StatusRequestEntityTooLarge, "upload.too_large"},
	} {
		src := writeSrc(t, t.TempDir(), "week1.mp4", make([]byte, 4000))
		e.grant(src)
		tc.fail()
		res, b := e.ungrantedUpload(ep, map[string]any{"path": src})
		if res.StatusCode != tc.status || errCode(t, b) != tc.code {
			t.Fatalf("%s: %d %s", tc.name, res.StatusCode, b)
		}
		tc.unfail()
		res, b = e.ungrantedUpload(ep, map[string]any{"path": src})
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%s: retry with the returned grant: %d %s", tc.name, res.StatusCode, b)
		}
		res, b = e.ungrantedUpload(ep, map[string]any{"path": src})
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != "upload.path_not_allowed" {
			t.Fatalf("%s: second use of one grant: %d %s", tc.name, res.StatusCode, b)
		}
	}
	// the path as the dialog spelled it, in another spelling of the same name
	if runtime.GOOS == "windows" {
		src := writeSrc(t, t.TempDir(), "week2.mp4", []byte("media"))
		e.grant(strings.ToUpper(src))
		if res, b := e.ungrantedUpload(ep, map[string]any{"path": src}); res.StatusCode != http.StatusCreated {
			t.Fatalf("case-insensitive match on Windows: %d %s", res.StatusCode, b)
		}
	}
}

// grantPost posts to /api/app/path-grants with the token and grant header.
func (e *env) grantPost(token, grant string, body []byte) (*http.Response, []byte) {
	e.t.Helper()
	req, err := http.NewRequest("POST", e.ts.URL+"/api/app/path-grants", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	if grant != "" {
		req.Header.Set(PathGrantHeader, grant)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestPostPathGrants(t *testing.T) {
	// the desktop main process computes the same header
	// (desktop/test/unit/pathgrant.test.js has this vector too)
	if got := pathGrantSecret(testToken); got != "a0b9b3e44527fa6409a10ee492828354479d7daf3722999ef862b2410c99d264" {
		t.Fatalf("pathGrantSecret = %s", got)
	}
	e := newEnv(t, func(s *Server) { s.grants.wait = 20 * time.Millisecond })
	c := e.course("C")
	// web mode: no such endpoint
	body, _ := json.Marshal(map[string]any{"paths": []string{"/x"}})
	if res, b := e.grantPost("", pathGrantSecret(testToken), body); res.StatusCode != http.StatusNotFound {
		t.Fatalf("web mode: %d %s", res.StatusCode, b)
	}
	e.desktopOn()
	src := writeSrc(t, t.TempDir(), "week1.mp4", []byte("media"))
	body, _ = json.Marshal(map[string]any{"paths": []string{src}})
	secret := pathGrantSecret(testToken)
	for _, tc := range []struct {
		name, token, grant string
		status             int
	}{
		{"no token", "", secret, http.StatusUnauthorized},
		{"the page's request: token only", testToken, "", http.StatusUnauthorized},
		{"the token as grant", testToken, testToken, http.StatusUnauthorized},
		{"another token's grant", testToken, pathGrantSecret(strings.Repeat("f", 64)), http.StatusUnauthorized},
	} {
		if res, b := e.grantPost(tc.token, tc.grant, body); res.StatusCode != tc.status {
			t.Errorf("%s: %d %s", tc.name, res.StatusCode, b)
		}
	}
	ep := "/api/courses/" + c.ID + "/lectures"
	if res, b := e.ungrantedUpload(ep, map[string]any{"path": src}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("refused registrations granted the path: %d %s", res.StatusCode, b)
	}
	for _, bad := range []any{
		map[string]any{"paths": []string{}},
		map[string]any{"paths": []string{""}},
		map[string]any{"paths": []string{"a\x00b"}},
		map[string]any{"paths": []string{strings.Repeat("a", pathGrantMaxBytes+1)}},
		map[string]any{"paths": make([]string, pathGrantPerPost+1)},
		map[string]any{"paths": "x"},
		map[string]any{"paths": []string{src}, "kind": "dir"},
	} {
		b, _ := json.Marshal(bad)
		if res, rb := e.grantPost(testToken, secret, b); res.StatusCode != http.StatusBadRequest {
			t.Errorf("bad body %.60s: %d %s", b, res.StatusCode, rb)
		}
	}
	// a folder grant (the folder dialog, SEC24 R1) is no file grant
	fb, _ := json.Marshal(map[string]any{"paths": []string{src}, "kind": "folder"})
	if res, b := e.grantPost(testToken, secret, fb); res.StatusCode != http.StatusNoContent {
		t.Fatalf("register a folder: %d %s", res.StatusCode, b)
	}
	if !e.srv.grants.hasFolder(src) {
		t.Fatal("folder grant missing")
	}
	if res, b := e.ungrantedUpload(ep, map[string]any{"path": src}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a folder grant let a file be copied: %d %s", res.StatusCode, b)
	}
	if res, b := e.grantPost(testToken, secret, body); res.StatusCode != http.StatusNoContent {
		t.Fatalf("register: %d %s", res.StatusCode, b)
	}
	if !e.srv.grants.hasFolder(src) {
		t.Fatal("a file grant replaced the folder grant")
	}
	if res, b := e.ungrantedUpload(ep, map[string]any{"path": src}); res.StatusCode != http.StatusCreated {
		t.Fatalf("granted upload: %d %s", res.StatusCode, b)
	}
	if res, b := e.ungrantedUpload(ep, map[string]any{"path": src}); res.StatusCode != http.StatusForbidden || errCode(t, b) != "upload.path_not_allowed" {
		t.Fatalf("used grant: %d %s", res.StatusCode, b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("the user's file must stay: %v", err)
	}
}

// The offline update and the component import take a user's path as well
// (SEC24 L4): without a grant it is refused before the file system is asked.
func TestOfflineImportsRefuseUngrantedPathBeforeFS(t *testing.T) {
	quietLog(t)
	share := func(name string) string {
		if runtime.GOOS == "windows" {
			return `\\fileserver.example\share\` + name
		}
		return `//fileserver.example/share/` + name
	}
	refused := func(t *testing.T, e *env, spy *fsSpy, ep string, body map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		res, rb := e.doWith("POST", ep, b, testToken, "")
		if res.StatusCode != http.StatusForbidden || errCode(t, rb) != "upload.path_not_allowed" {
			t.Errorf("%s %v: %d %s", ep, body, res.StatusCode, rb)
		}
		if calls := spy.take(); len(calls) != 0 {
			t.Errorf("%s %v: the file system was asked: %v", ep, body, calls)
		}
	}
	notFound := func(p string) (string, error) {
		return "", &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
	}

	t.Run("selfupdate", func(t *testing.T) {
		r := newSURelease(t)
		e, _ := newSUEnv(t, r, &DesktopOptions{Token: testToken})
		spy := &fsSpy{}
		spy.install(e.srv, notFound)
		e.srv.grants.wait = 20 * time.Millisecond
		local := filepath.Join(t.TempDir(), "update.json")
		refused(t, e, spy, "/api/selfupdate/import", map[string]any{"path": share("update.json")})
		refused(t, e, spy, "/api/selfupdate/import", map[string]any{"paths": []string{share("update.json"), share("update.json.minisig")}})
		refused(t, e, spy, "/api/selfupdate/import", map[string]any{"paths": []string{local}})
	})

	t.Run("components", func(t *testing.T) {
		d := &DesktopOptions{Token: testToken}
		spy := &fsSpy{}
		ce := newCompEnv(t, func(s *Server, _ *compState) {
			s.desktop = d
			spy.install(s, notFound)
			s.grants.wait = 20 * time.Millisecond
		})
		u, _ := url.Parse(ce.ts.URL)
		d.Port, _ = strconv.Atoi(u.Port())
		refused(t, ce.env, spy, "/api/components/import", map[string]any{"path": share("tool.zip")})
		refused(t, ce.env, spy, "/api/components/import", map[string]any{"path": filepath.Join(t.TempDir(), "tool.zip")})
	})
}
