// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/relocate"
)

// Security review SEC24 R1: the data directory endpoints (move, its check,
// the data import) refuse a network path the folder dialog did not pick
// before any file system call touches it (os.Stat of \\server\… sends the
// user's NTLM credentials). Nothing here opens a network path: the file
// system calls are stand-ins that record what they were asked (the hosts
// are reserved names and TEST-NET addresses).

// ddSpy replaces the data directory endpoints' file system calls.
type ddSpy struct {
	mu    sync.Mutex
	calls []string
}

func (d *ddSpy) rec(s string) {
	d.mu.Lock()
	d.calls = append(d.calls, s)
	d.mu.Unlock()
}

func (d *ddSpy) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.calls
	d.calls = nil
	return c
}

// install: a network path is never really asked about (not found); a local
// one is (the existing behaviour). checkMove and checkImportSource refuse
// with a code of their own unless plan is set.
func (d *ddSpy) install(s *Server, plan func(dst string) relocate.MovePlan) {
	s.statDir = func(p string) (fs.FileInfo, error) {
		d.rec("stat " + p)
		if networkPath(p) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
		}
		return os.Stat(p)
	}
	s.checkMove = func(_ context.Context, src, dst, _ string) (relocate.MovePlan, error) {
		d.rec("checkMove " + dst)
		if plan != nil {
			return plan(dst), nil
		}
		return relocate.MovePlan{}, model.Errorf(relocate.CodeTargetNotWritable, map[string]any{"path": dst}, nil)
	}
	s.checkImportSource = func(_ context.Context, dir string) (relocate.SourceInfo, error) {
		d.rec("checkImportSource " + dir)
		return relocate.SourceInfo{}, model.Errorf(relocate.CodeNotLectureFlow, map[string]any{"path": dir}, nil)
	}
}

// netFolders are the network and device spellings a page could send.
func netFolders() []string {
	return []string{
		`\\fileserver.example\share\LectureFlow`,
		`//fileserver.example/share/LectureFlow`,
		`\\192.0.2.10\share\LectureFlow`,
		`/\192.0.2.10\share\LectureFlow`,
		` \\fileserver.example\share\LectureFlow`,
		`\\?\UNC\fileserver.example\share\LectureFlow`,
		`//?/UNC/192.0.2.10/share/LectureFlow`,
		`\\.\UNC\fileserver.example\share\LectureFlow`,
		`\\192.0.2.10@SSL@443\DavWWWRoot\LectureFlow`,
		`\\fileserver.example`,
	}
}

func TestDataDirEndpointsRefuseUngrantedNetworkPathBeforeFS(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	run := e.srv.dataDirRun()
	run.move = func(context.Context, relocate.MoveOptions) error {
		t.Error("a refused path was moved to")
		return nil
	}
	// a folder grant for another share, and a file grant for this one, do not help
	e.srv.grants.addFolders([]string{`\\fileserver.example\share\Other`})
	e.grant(`\\fileserver.example\share\LectureFlow`, `//fileserver.example/share/LectureFlow`)
	for _, p := range netFolders() {
		c := e.check(p)
		if c.OK || c.Error == nil || c.Error.Code != codeDataDirPathNotAllowed || c.FreeBytes != -1 {
			t.Errorf("check %q: %+v / %+v", p, c, c.Error)
		}
		for _, ep := range []string{"/api/app/data-dir", "/api/app/import"} {
			res, b := e.req("POST", ep, map[string]string{"path": p})
			if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotAllowed {
				t.Errorf("POST %s %q: %d %s", ep, p, res.StatusCode, b)
			}
		}
		// statDir is asked about the current data directory only (the status
		// of a refusal): never about the page's path
		for _, c := range spy.take() {
			if strings.Contains(strings.ToLower(c), "fileserver.example") || strings.Contains(c, "192.0.2.10") || !strings.HasPrefix(c, "stat ") {
				t.Errorf("%q: the file system was asked: %s", p, c)
			}
		}
	}
	if st := e.status(); st.State != ddIdle || e.restarts.Load() != 0 || pathExists(e.loc) {
		t.Fatalf("a refusal changed something: %+v", st)
	}

	// local paths: picked in the folder dialog, they reach the checks (an
	// ungranted local path is refused too: datadir_localgrant_test.go)
	local := filepath.Join(t.TempDir(), "Big Disk", "LectureFlow")
	e.pick(local)
	c := e.check(local)
	if c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
		t.Fatalf("local check: %+v", c)
	}
	res, b := e.req("POST", "/api/app/data-dir", map[string]string{"path": local})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeTargetNotWritable {
		t.Fatalf("local move: %d %s", res.StatusCode, b)
	}
	res, b = e.req("POST", "/api/app/import", map[string]string{"path": local})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeNotLectureFlow {
		t.Fatalf("local import: %d %s", res.StatusCode, b)
	}
	calls := strings.Join(spy.take(), "\n")
	for _, want := range []string{"checkMove " + local, "checkImportSource " + local} {
		if !strings.Contains(calls, want) {
			t.Errorf("local path: %q not asked; calls:\n%s", want, calls)
		}
	}

	// web mode: no such endpoints, nothing asked
	e.asWeb(func() {
		res, _ := e.req("POST", "/api/app/data-dir/check", map[string]string{"path": netFolders()[0]})
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("web mode: %d", res.StatusCode)
		}
	})
	if calls := spy.take(); len(calls) != 0 {
		t.Errorf("web mode asked the file system: %v", calls)
	}
}

// A network folder the folder dialog picked is used like a local one: the
// check leaves the grant to the move, a failed request returns it, the
// move that starts uses it up.
func TestDataDirGrantedNetworkFolder(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	var planned bool
	spy.install(e.srv, nil)
	share := `\\fileserver.example\share\LectureFlow`
	if runtime.GOOS != "windows" {
		share = `//fileserver.example/share/LectureFlow`
	}
	e.srv.grants.addFolders([]string{share})

	for i := 0; i < 2; i++ { // the check does not use the grant up
		c := e.check(share)
		if c.Error == nil || c.Error.Code != relocate.CodeTargetNotWritable {
			t.Fatalf("check %d of a granted share: %+v", i+1, c)
		}
	}
	// a failed import and a failed move return the grant
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": share})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeNotLectureFlow {
		t.Fatalf("import of a granted share: %d %s", res.StatusCode, b)
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": share})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != relocate.CodeTargetNotWritable {
		t.Fatalf("move to a granted share: %d %s", res.StatusCode, b)
	}
	calls := strings.Join(spy.take(), "\n")
	for _, want := range []string{"checkMove " + share, "checkImportSource " + share} {
		if !strings.Contains(calls, want) {
			t.Errorf("granted share: %q not asked; calls:\n%s", want, calls)
		}
	}

	// the move that starts uses the grant up
	e.srv.checkMove = func(_ context.Context, src, dst, _ string) (relocate.MovePlan, error) {
		planned = true
		return relocate.MovePlan{Src: src, Dst: dst, TotalBytes: 1}, nil
	}
	e.srv.dataDirRun().move = func(context.Context, relocate.MoveOptions) error {
		return errors.New("stand-in move: stopped")
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": share})
	if res.StatusCode != http.StatusAccepted || !planned {
		t.Fatalf("move: %d %s", res.StatusCode, b)
	}
	e.waitRun()
	if e.restarts.Load() != 0 {
		t.Fatal("a failed move restarted")
	}
	res, b = e.req("POST", "/api/app/data-dir", map[string]string{"path": share})
	if res.StatusCode != http.StatusForbidden || errCode(t, b) != codeDataDirPathNotAllowed {
		t.Fatalf("second move with one grant: %d %s", res.StatusCode, b)
	}
}

// SEC24c: decodeJSON reads a body of any Content-Type, so a page could post
// the path as text/plain. Such a request is in grantScope too: a failure
// returns the folder grant and renews nothing.
func TestDataDirTextPlainKeepsGrantRules(t *testing.T) {
	quietLog(t)
	e := newDDEnv(t)
	spy := &ddSpy{}
	spy.install(e.srv, nil)
	now := time.Unix(5_000_000, 0)
	e.srv.grants.now = func() time.Time { return now }
	share := `\\fileserver.example\share\LectureFlow`
	if runtime.GOOS != "windows" {
		share = `//fileserver.example/share/LectureFlow`
	}
	e.srv.grants.addFolders([]string{share})
	want := now.Add(folderGrantTTL) // a folder grant: fixed (r6 path-grants)
	deadline := func() time.Time {
		g := e.srv.grants
		g.mu.Lock()
		defer g.mu.Unlock()
		if en := g.m[folderGrantKey(share)]; en != nil {
			return en.batch.deadline
		}
		return time.Time{}
	}
	body, _ := json.Marshal(map[string]string{"path": share})
	for _, tc := range []struct{ ep, code string }{
		{"/api/app/data-dir", relocate.CodeTargetNotWritable},
		{"/api/app/import", relocate.CodeNotLectureFlow},
	} {
		now = now.Add(folderGrantTTL / 3)
		req, err := http.NewRequest("POST", e.ts.URL+tc.ep, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set(TokenHeader, testToken)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rb, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest || errCode(t, rb) != tc.code {
			t.Fatalf("%s as text/plain: %d %s", tc.ep, res.StatusCode, rb)
		}
		if !e.srv.grants.hasFolder(share) {
			t.Fatalf("%s as text/plain: the failed request did not return the grant", tc.ep)
		}
		if d := deadline(); !d.Equal(want) {
			t.Fatalf("%s as text/plain: the failed request renewed the grant to %v (want %v)", tc.ep, d, want)
		}
	}
	if calls := strings.Join(spy.take(), "\n"); !strings.Contains(calls, "checkMove "+share) || !strings.Contains(calls, "checkImportSource "+share) {
		t.Fatalf("the granted share did not reach the checks:\n%s", calls)
	}
}
