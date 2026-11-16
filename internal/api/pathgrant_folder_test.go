// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/relocate"
)

// r6 path-grants (security review SEC26 low-1): a folder grant (the folder
// dialog's pick, for the data directory endpoints) lives folderGrantTTL from
// its registration and takes no part in renewal: no successful request
// extends it, and the request that uses it extends nothing. File grants keep
// their 12 hours and their renewal. All on a clock the tests move.

// grantDeadline is the expiry of the grants of key (zero: none).
func grantDeadline(g *pathGrants, key string) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e := g.m[key]; e != nil {
		return e.batch.deadline
	}
	return time.Time{}
}

// ledgerCtx is the context of a request inside grantScope.
func ledgerCtx() (context.Context, *grantLedger) {
	l := &grantLedger{}
	return context.WithValue(context.Background(), grantLedgerKey{}, l), l
}

func TestFolderGrantsShortFixedExpiry(t *testing.T) {
	if folderGrantTTL != 10*time.Minute || pathGrantTTL != 12*time.Hour {
		t.Fatalf("folderGrantTTL = %v, pathGrantTTL = %v (SEC26 low-1: 10 minutes for a folder; SEC24 M1: 12 hours for a file)", folderGrantTTL, pathGrantTTL)
	}
	start := time.Unix(6_000_000, 0)
	now := start
	g := testGrants(&now)
	s := &Server{grants: g}
	dir, file, file2 := abs("alice", "LectureFlow"), abs("alice", "week1.mp4"), abs("alice", "week2.mp4")
	dirKey, fileKey := folderGrantKey(dir), pathGrantKey(file)
	g.addFolders([]string{dir})
	g.add([]string{file, file2})
	folderEnd := start.Add(folderGrantTTL)
	if d := grantDeadline(g, dirKey); !d.Equal(folderEnd) {
		t.Fatalf("a folder grant expires %v after its registration, want %v", d.Sub(start), folderGrantTTL)
	}
	if d := grantDeadline(g, fileKey); !d.Equal(start.Add(pathGrantTTL)) {
		t.Fatalf("a file grant expires %v after its registration, want %v", d.Sub(start), pathGrantTTL)
	}

	// a request that used a file grant and succeeded renews the file grants
	// as before, and leaves the folder grant its expiry
	now = start.Add(4 * time.Minute)
	ctx, l := ledgerCtx()
	if !s.useGrant(ctx, file2) {
		t.Fatal("file grant refused")
	}
	l.succeeded()
	fileEnd := now.Add(pathGrantTTL)
	if d := grantDeadline(g, fileKey); !d.Equal(fileEnd) {
		t.Fatalf("a successful upload no longer renews the file grants: %v, want %v", d, fileEnd)
	}
	if d := grantDeadline(g, dirKey); !d.Equal(folderEnd) {
		t.Fatalf("a successful upload renewed the folder grant: it now expires %v after its registration", d.Sub(start))
	}
	g.renew() // however renewal is reached
	if d := grantDeadline(g, dirKey); !d.Equal(folderEnd) {
		t.Fatalf("renew moved the folder grant's expiry to %v after its registration", d.Sub(start))
	}
	fileEnd = now.Add(pathGrantTTL)

	// a request that used the folder grant and failed returns it with the
	// expiry it had
	now = folderEnd.Add(-time.Second)
	ctx, l = ledgerCtx()
	if !s.useFolderGrant(ctx, dir) {
		t.Fatal("the folder grant was refused before its expiry")
	}
	if g.hasFolder(dir) {
		t.Fatal("a folder grant is used once")
	}
	l.refund()
	if !g.hasFolder(dir) {
		t.Fatal("a failed request did not return the folder grant")
	}
	if d := grantDeadline(g, dirKey); !d.Equal(folderEnd) {
		t.Fatalf("a returned folder grant expires %v after its registration", d.Sub(start))
	}
	// expired: not there, not usable, the file grant untouched
	now = folderEnd
	if g.hasFolder(dir) {
		t.Fatal("a folder grant outlived folderGrantTTL")
	}
	if _, ok := g.takeFolder(context.Background(), dir); ok {
		t.Fatal("an expired folder grant was used")
	}
	if d := grantDeadline(g, fileKey); !d.Equal(fileEnd) {
		t.Fatalf("the file grant's expiry moved: %v, want %v", d, fileEnd)
	}

	// a failure that ends after the expiry does not bring the grant back
	g.addFolders([]string{dir})
	restore, ok := g.takeFolder(context.Background(), dir)
	if !ok {
		t.Fatal("picked again: refused")
	}
	now = now.Add(folderGrantTTL)
	restore()
	if g.hasFolder(dir) {
		t.Fatal("an expired folder grant came back with a late failure")
	}

	// the request that uses a folder grant and succeeds renews nothing: not
	// the file grants, not another folder's
	other := abs("alice", "Other")
	g.addFolders([]string{dir, other})
	picked := now
	now = now.Add(5 * time.Minute)
	ctx, l = ledgerCtx()
	if !s.useFolderGrant(ctx, dir) {
		t.Fatal("picked again: refused")
	}
	l.succeeded()
	if d := grantDeadline(g, fileKey); !d.Equal(fileEnd) {
		t.Fatalf("using a folder grant renewed the file grants: %v, want %v", d, fileEnd)
	}
	if d := grantDeadline(g, folderGrantKey(other)); !d.Equal(picked.Add(folderGrantTTL)) {
		t.Fatalf("using a folder grant renewed another folder's: it now expires %v after its registration", d.Sub(picked))
	}

	// picking a folder again starts its folderGrantTTL anew (a new
	// registration, as for a file)
	now = picked.Add(folderGrantTTL - time.Second)
	g.addFolders([]string{other})
	now = now.Add(folderGrantTTL - time.Second)
	if !g.hasFolder(other) {
		t.Fatal("a folder picked again expired with its first pick")
	}
	now = now.Add(time.Second)
	if g.hasFolder(other) {
		t.Fatal("a folder picked twice outlived folderGrantTTL after the second pick")
	}

	// the file grant lives on for its 12 hours
	now = fileEnd.Add(-time.Second)
	if _, ok := tryTake(g, file); !ok {
		t.Fatal("the file grant expired early")
	}
}

// The same over HTTP: the registration of POST /api/app/path-grants, and
// the data directory endpoints refusing an expired pick before anything
// touches the folder.
func TestDataDirFolderGrantExpires(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	start := time.Unix(7_000_000, 0)
	var passed atomic.Int64 // the handlers read the clock on other goroutines
	e.srv.grants.now = func() time.Time { return start.Add(time.Duration(passed.Load())) }
	at := func(d time.Duration) { passed.Store(int64(d)) }

	dst, dst2 := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow"), filepath.Join(t.TempDir(), "Other")
	file := filepath.Join(t.TempDir(), "week1.mp4")
	secret := pathGrantSecret(testToken)
	for _, body := range []any{
		map[string]any{"paths": []string{dst, dst2}, "kind": "folder"},
		map[string]any{"paths": []string{file}},
	} {
		b, _ := json.Marshal(body)
		if res, rb := e.grantPost(testToken, secret, b); res.StatusCode != http.StatusNoContent {
			t.Fatalf("register %s: %d %s", b, res.StatusCode, rb)
		}
	}
	g := e.srv.grants
	folderEnd, fileEnd := start.Add(folderGrantTTL), start.Add(pathGrantTTL)
	if d := grantDeadline(g, folderGrantKey(dst)); !d.Equal(folderEnd) {
		t.Fatalf("kind folder: expires %v after the registration, want %v", d.Sub(start), folderGrantTTL)
	}
	if d := grantDeadline(g, pathGrantKey(file)); !d.Equal(fileEnd) {
		t.Fatalf("kind file: expires %v after the registration, want %v", d.Sub(start), pathGrantTTL)
	}

	// a move that starts (202) uses its folder grant and renews nothing
	at(5 * time.Minute)
	e.srv.checkMove = func(_ context.Context, src, d, _ string) (relocate.MovePlan, error) {
		return relocate.MovePlan{Src: src, Dst: d, TotalBytes: 1}, nil
	}
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error {
		return errors.New("stand-in move: stopped")
	}
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": dst2})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("move: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if d := grantDeadline(g, pathGrantKey(file)); !d.Equal(fileEnd) {
		t.Fatalf("a move that started renewed the file grant: %v, want %v", d, fileEnd)
	}
	if d := grantDeadline(g, folderGrantKey(dst)); !d.Equal(folderEnd) {
		t.Fatalf("a move that started renewed the other folder's grant: expires %v after the registration", d.Sub(start))
	}
	spy.install(e.srv, nil) // the checks refuse again

	// one second before the expiry the pick still reaches the checks, and a
	// refused move returns it with the expiry it had
	at(folderGrantTTL - time.Second)
	if c := e.check(dst); c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
		t.Fatalf("check before the expiry: %+v", c)
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": dst})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeTargetNotWritable {
		t.Fatalf("move before the expiry: %d %s", res.StatusCode, b)
	}
	if d := grantDeadline(g, folderGrantKey(dst)); !d.Equal(folderEnd) {
		t.Fatalf("the refused move returned the grant with another expiry: %v after the registration", d.Sub(start))
	}

	// expired: refused like a folder that was never picked, nothing asked
	// of the file system
	spy.take()
	at(folderGrantTTL)
	if c := e.check(dst); c.OK || c.Error == nil || c.Error.Code != codeDataDirPathNotGranted || c.FreeBytes != -1 {
		t.Fatalf("check after the expiry: %+v / %+v", c, c.Error)
	}
	for _, ep := range []string{"/api/app/data-dir", "/api/app/import"} {
		res, b = e.req("POST", ep, map[string]string{"path": dst})
		if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotGranted {
			t.Fatalf("POST %s after the expiry: %d %s", ep, res.StatusCode, b)
		}
	}
	if calls := spy.take(); len(calls) != 0 {
		t.Fatalf("an expired pick reached the file system: %v", calls)
	}

	// picked again, it is good for another folderGrantTTL
	e.pick(dst)
	at(2*folderGrantTTL - time.Second)
	if c := e.check(dst); c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
		t.Fatalf("check of a folder picked again: %+v", c)
	}
	// and the file grant is as it was
	at(pathGrantTTL - time.Second)
	if _, ok := g.take(context.Background(), file); !ok {
		t.Fatal("the file grant expired early")
	}
}

// "Cancel" in the page's confirmation of a picked folder: the page gives
// the pick up (DELETE /api/app/folder-pick), and it is gone at once, not
// after folderGrantTTL.
func TestFolderPickGivenUp(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	now := time.Unix(8_000_000, 0) // never moved: nothing here waits for an expiry
	e.srv.grants.now = func() time.Time { return now }
	g := e.srv.grants
	const ep = "/api/app/folder-pick"
	giveUp := func(p string) {
		t.Helper()
		res, b := e.req("DELETE", ep, map[string]string{"path": p})
		if res.StatusCode != http.StatusNoContent || len(b) != 0 {
			t.Fatalf("DELETE %s %q: %d %s", ep, p, res.StatusCode, b)
		}
	}
	reaches := func(p string) {
		t.Helper()
		if c := e.check(p); c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
			t.Fatalf("check of the picked folder %q: %+v / %+v", p, c, c.Error)
		}
	}
	a, b := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow"), filepath.Join(t.TempDir(), "Other")
	e.pick(a, b)
	e.grant(a) // a file grant of the same path is another grant
	reaches(a)

	spy.take()
	giveUp(a)
	if g.hasFolder(a) {
		t.Fatal("the pick the user declined is still granted")
	}
	if c := e.check(a); c.OK || c.Error == nil || c.Error.Code != codeDataDirPathNotGranted || c.FreeBytes != -1 {
		t.Fatalf("check after the pick was given up: %+v / %+v", c, c.Error)
	}
	for _, p := range []string{"/api/app/data-dir", "/api/app/import"} {
		res, rb := e.req("POST", p, map[string]string{"path": a})
		if res.StatusCode != http.StatusForbidden || errCode(t, rb) != codeDataDirPathNotGranted {
			t.Fatalf("POST %s after the pick was given up: %d %s", p, res.StatusCode, rb)
		}
	}
	if calls := spy.take(); len(calls) != 0 {
		t.Fatalf("giving a pick up, or using it afterwards, reached the file system: %v", calls)
	}
	// only that folder's pick: the other folder and the file grant stay
	if !g.hasFolder(b) {
		t.Fatal("giving up one pick dropped another folder's")
	}
	if grantDeadline(g, pathGrantKey(a)).IsZero() {
		t.Fatal("giving up a folder pick dropped the file grant of the same path")
	}
	reaches(b)

	// picked again, the folder is usable again
	e.pick(a)
	reaches(a)
	// every unused pick of the folder goes, in whatever spelling the page
	// names it (the grant's own comparison)
	e.pick(a)
	giveUp(a + string(filepath.Separator))
	if g.hasFolder(a) {
		t.Fatal("a folder picked twice kept a grant after the user declined")
	}
	if filepath.Separator == '\\' {
		e.pick(a)
		giveUp(strings.ToUpper(filepath.ToSlash(a)))
		if g.hasFolder(a) {
			t.Fatal("another spelling of the folder did not give the pick up")
		}
	}

	// nothing to give up: the same answer, nothing asked, nothing else lost
	e.pick(a)
	spy.take()
	for _, p := range []string{"", "relative", a + "x", filepath.Dir(a), filepath.Join(a, "sub"), netFolders()[0], "C:\\x\x00y"} {
		giveUp(p)
	}
	if calls := spy.take(); len(calls) != 0 {
		t.Fatalf("giving up a folder that was not picked reached the file system: %v", calls)
	}
	if !g.hasFolder(a) || !g.hasFolder(b) {
		t.Fatalf("giving up other paths dropped a pick: a %v, b %v", g.hasFolder(a), g.hasFolder(b))
	}

	// a JSON object (or null) is answered 204, any other body 400, with or
	// without a pick: the answer says nothing about the picks (SEC38 info-4)
	for _, raw := range []string{`{}`, `null`, `{"path":null}`, `{"other":1}`} {
		if res, rb := e.doWith("DELETE", ep, []byte(raw), testToken, ""); res.StatusCode != http.StatusNoContent || len(rb) != 0 {
			t.Fatalf("body %s: %d %s", raw, res.StatusCode, rb)
		}
	}
	// refused requests give nothing up
	picked, _ := json.Marshal(a)
	for _, raw := range []string{`{`, `[]`, `"x"`, `1`, `{"path":1}`, `[` + string(picked) + `]`, string(picked)} {
		if res, rb := e.doWith("DELETE", ep, []byte(raw), testToken, ""); res.StatusCode != http.StatusBadRequest || errCode(t, rb) != "request.bad_json" {
			t.Fatalf("body %s: %d %s", raw, res.StatusCode, rb)
		}
	}
	body, _ := json.Marshal(map[string]string{"path": a})
	if res, rb := e.doWith("DELETE", ep, body, "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", res.StatusCode, rb)
	}
	e.asWeb(func() {
		res, rb := e.req("DELETE", ep, map[string]string{"path": a})
		if res.StatusCode != http.StatusNotFound || errCode(t, rb) != "api.not_found" {
			t.Fatalf("web mode: %d %s", res.StatusCode, rb)
		}
	})
	if !g.hasFolder(a) {
		t.Fatal("a refused request gave the pick up")
	}
}
