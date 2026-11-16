// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
	"lectureflow/internal/selfupdate"
)

func dataDirRow(t *testing.T, e *env) SystemCheck {
	t.Helper()
	var sys System
	e.call("GET", "/api/system", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if c.Name == checkDataDir {
			return c
		}
	}
	t.Fatal("no data_dir row")
	return SystemCheck{}
}

// fakeDataDirAccess makes dirperm.Check of the data directory answer *e
// (and Restrict record its calls) for the test.
func fakeDataDirAccess(t *testing.T, e *dirperm.Exposure, restrictErr *error) *int {
	t.Helper()
	oc, or := checkDataDirAccess, restrictDataDir
	calls := 0
	checkDataDirAccess = func(string) (dirperm.Exposure, error) { return *e, nil }
	restrictDataDir = func(string) error {
		calls++
		return *restrictErr
	}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	t.Cleanup(func() {
		checkDataDirAccess, restrictDataDir = oc, or
		dataDirAccess.mu.Lock()
		dataDirAccess.dir = ""
		dataDirAccess.mu.Unlock()
	})
	return &calls
}

// The data_dir row warns while other accounts can change or read the data
// directory; "收紧权限" (POST /api/system/data-dir/restrict) makes it
// private, or says why it could not.
func TestDataDirAccessRowAndRestrict(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	exp := dirperm.Exposure{}
	var rerr error
	calls := fakeDataDirAccess(t, &exp, &rerr)
	abs, _ := filepath.Abs(e.cfg.DataDir)

	if c := dataDirRow(t, e); c.Status != statusOK || c.DetailInfo == nil || c.DetailInfo.Code != "check.dir_writable" || c.HintInfo != nil {
		t.Fatalf("private: %+v", c)
	}

	exp = dirperm.Exposure{Write: true, Read: true, Who: []string{"Authenticated Users", "Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = "" // past the cache
	dataDirAccess.mu.Unlock()
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.DetailInfo.Params["path"] != abs ||
		c.DetailInfo.Params["who"] != "Authenticated Users, Users" || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirRestrict {
		t.Fatalf("shared: %+v %+v %+v", c, c.DetailInfo, c.HintInfo)
	}
	exp = dirperm.Exposure{Read: true, Who: []string{"Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if c := dataDirRow(t, e); c.Status != statusWarn || c.DetailInfo.Code != codeDataDirReadable {
		t.Fatalf("readable: %+v", c)
	}

	// the permissions cannot be set
	rerr = errors.New("access denied")
	info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusInternalServerError)
	if info.Code != codeDataDirRestrictFailed || info.Params["path"] != abs || info.Params["error"] != "access denied" {
		t.Fatalf("failed: %+v", info)
	}
	// set, but others still get in (FAT, another owner)
	rerr = nil
	info = e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed {
		t.Fatalf("still exposed: %+v", info)
	}
	// done
	checkDataDirAccess = func(string) (dirperm.Exposure, error) { return dirperm.Exposure{}, nil }
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Name != checkDataDir || out.Check.Status != statusOK || *calls != 3 {
		t.Fatalf("restricted: %+v (%d calls)", out.Check, *calls)
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v", c)
	}
}

// What the data directory holds (security audit M-3; the walk is injected
// here, system_datadir_windows_test.go does it for real): an entry other
// accounts may change, or a tree the walk cannot vouch for, turns the row
// into a warning although the data directory itself is private; "收紧权限"
// answers 200 only when a new walk finds nothing.
func TestDataDirEntriesRowAndRestrict(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	exp := dirperm.Exposure{}
	var rerr error
	fakeDataDirAccess(t, &exp, &rerr)
	abs, _ := filepath.Abs(e.cfg.DataDir)
	entry := filepath.Join(abs, "components", "whisper", "venv")

	oc, or := checkDataDirTree, restrictDataDirTree
	t.Cleanup(func() { checkDataDirTree, restrictDataDirTree = oc, or })
	rep := dirperm.TreeReport{}
	var terr error
	resets := 0
	checkDataDirTree = func(dir string) (dirperm.TreeReport, error) {
		if dir != e.cfg.DataDir {
			t.Errorf("walk of %q", dir)
		}
		return rep, nil
	}
	restrictDataDirTree = func(string) ([]string, error) {
		resets++
		return nil, terr
	}

	// the row uses the last walk; GET /api/system starts none
	rep = dirperm.TreeReport{Entries: 7, Findings: []dirperm.TreeFinding{{Path: entry, Who: []string{"owner S-1-5-21-1-2-3-1002"}}}}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("before the checks ran again: %+v", c)
	}
	e.srv.checkDataDirEntries()
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.DetailInfo.Params["path"] != entry ||
		c.DetailInfo.Params["who"] != "owner S-1-5-21-1-2-3-1002" || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirRestrict {
		t.Fatalf("an entry of another account: %+v %+v", c, c.DetailInfo)
	}
	// the data directory itself readable by others: the entry is the
	// graver finding
	exp = dirperm.Exposure{Read: true, Who: []string{"Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if c := dataDirRow(t, e); c.DetailInfo.Code != codeDataDirShared || c.DetailInfo.Params["path"] != entry {
		t.Fatalf("readable and an entry: %+v %+v", c, c.DetailInfo)
	}
	exp = dirperm.Exposure{}

	// the reset fails: 409 with its reason, the row keeps warning
	terr = errors.New("dirperm: could not reset " + entry + ": Access is denied.")
	info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed || info.Params["path"] != abs || info.Params["error"] != terr.Error() || resets != 1 {
		t.Fatalf("reset failed: %+v (%d resets)", info, resets)
	}
	if c := dataDirRow(t, e); c.Status != statusWarn {
		t.Fatalf("after a failed reset: %+v", c)
	}
	// the reset reports no error, but the walk still finds the entry
	terr = nil
	info = e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed || info.Params["error"] != "other accounts can still change "+entry+" (owner S-1-5-21-1-2-3-1002)" {
		t.Fatalf("still found: %+v", info)
	}
	// a tree that cannot be vouched for
	rep = dirperm.TreeReport{Entries: 300000, Unconfirmed: "more than 300000 entries"}
	info = e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed {
		t.Fatalf("unconfirmed: %+v", info)
	}
	// the row says so in its own words and offers no "收紧权限" (which
	// could not repair it)
	c = dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirUnconfirmed || c.DetailInfo.Params["path"] != abs ||
		c.DetailInfo.Params["reason"] != "more than 300000 entries" || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirUnconfirm {
		t.Fatalf("unconfirmed row: %+v %+v %+v", c, c.DetailInfo, c.HintInfo)
	}
	// an entry found as well: that is what "收紧权限" is for
	rep = dirperm.TreeReport{Entries: 300000, Unconfirmed: "more than 300000 entries",
		Findings: []dirperm.TreeFinding{{Path: entry, Who: []string{"Users"}}}}
	e.srv.checkDataDirEntries()
	if c := dataDirRow(t, e); c.DetailInfo.Code != codeDataDirShared || c.HintInfo.Code != codeHintDataDirRestrict {
		t.Fatalf("an entry and an unconfirmed tree: %+v %+v", c, c.DetailInfo)
	}
	// the data directory itself readable by others: its own row first
	rep = dirperm.TreeReport{Entries: 300000, Unconfirmed: "more than 300000 entries"}
	e.srv.checkDataDirEntries()
	exp = dirperm.Exposure{Read: true, Who: []string{"Users"}}
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if c := dataDirRow(t, e); c.DetailInfo.Code != codeDataDirReadable {
		t.Fatalf("readable and an unconfirmed tree: %+v %+v", c, c.DetailInfo)
	}
	exp = dirperm.Exposure{}
	// clean
	rep = dirperm.TreeReport{Entries: 7}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK || resets != 4 {
		t.Fatalf("restricted: %+v (%d resets)", out.Check, resets)
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v %+v", c, c.DetailInfo)
	}
}

// Which entries of the data directory the walk looks at.
func TestDataDirScope(t *testing.T) {
	for rel, want := range map[string]dirperm.Scope{
		"":                                   dirperm.ScopeAll,
		"updates":                            dirperm.ScopeAll,
		"updates/backup/3.1.0/app/x.exe":     dirperm.ScopeAll,
		"Updates/pending.json":               dirperm.ScopeAll,
		"components":                         dirperm.ScopeAll,
		"components/whisper/1.0/venv/a.py":   dirperm.ScopeAll,
		"nmhost/launch.json":                 dirperm.ScopeAll,
		"electron":                           dirperm.ScopeAll,
		"electron/selfupdate-trust.json":     dirperm.ScopeAll,
		"electron/Preferences":               dirperm.ScopeAll,
		"Electron/Code Cache/js/index":       dirperm.ScopeAll,
		"lectureflow.db":                     dirperm.ScopeSelf,
		"courses":                            dirperm.ScopeSelf,
		"courses/c1/lectures/l1/audio.wav":   dirperm.ScopeSkip,
		"models":                             dirperm.ScopeSelf,
		"updatesX":                           dirperm.ScopeSelf,
		"componentsX/whisper":                dirperm.ScopeSkip,
		"logs/llm/2026-11-11/request-1.json": dirperm.ScopeSkip,
	} {
		if got := dataDirScope(rel); got != want {
			t.Errorf("dataDirScope(%q) = %v, want %v", rel, got, want)
		}
	}
}

// "收紧权限" and the update's rollback state (security review SEC17, 中-3;
// the permissions and the removal are injected here,
// system_datadir_windows_test.go does it for real): the state is removed
// exactly when the data directory was open to change before the click, and
// the answer says so.
func TestDataDirRestrictVoidsRollbackState(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	or, oc, ot, ort, ov := restrictDataDir, checkDataDirAccess, checkDataDirTree, restrictDataDirTree, voidRollbackState
	t.Cleanup(func() {
		restrictDataDir, checkDataDirAccess, checkDataDirTree, restrictDataDirTree, voidRollbackState = or, oc, ot, ort, ov
		dataDirAccess.mu.Lock()
		dataDirAccess.dir = ""
		dataDirAccess.mu.Unlock()
	})
	restricted := false
	restrictDataDir = func(string) error {
		restricted = true
		return nil
	}
	// the data directory as it is before the click, private once restricted
	before := dirperm.Exposure{}
	var beforeErr error
	checkDataDirAccess = func(string) (dirperm.Exposure, error) {
		if restricted {
			return dirperm.Exposure{}, nil
		}
		return before, beforeErr
	}
	walk := dirperm.TreeReport{Entries: 9}
	checkDataDirTree = func(string) (dirperm.TreeReport, error) { return walk, nil }
	var reset []string
	var resetErr error
	restrictDataDirTree = func(string) ([]string, error) { return reset, resetErr }
	voids := 0
	found := selfupdate.Voided{Record: true, Applying: true, Backups: true, Previous: true}
	var voidErr error
	voidRollbackState = func(dir string) (selfupdate.Voided, error) {
		if !restricted {
			t.Error("the rollback state was removed before the permissions were set")
		}
		if want, _ := filepath.EvalSymlinks(e.cfg.DataDir); dir != want {
			t.Errorf("removed in %q, want %q", dir, want)
		}
		voids++
		return found, voidErr
	}
	post := func(status int) map[string]any {
		t.Helper()
		restricted = false
		dataDirAccess.mu.Lock()
		dataDirAccess.dir = ""
		dataDirAccess.mu.Unlock()
		var out map[string]any
		if status == http.StatusOK {
			e.call("POST", "/api/system/data-dir/restrict", nil, status, &out)
			return out
		}
		// an error body: {"error": {...}} and, when the state went, "rollback_reset"
		res, b := e.do("POST", "/api/system/data-dir/restrict", nil)
		if res.StatusCode != status {
			t.Fatalf("status %d, want %d; body %s", res.StatusCode, status, b)
		}
		var body struct {
			Error         model.ErrorInfo `json:"error"`
			RollbackReset *bool           `json:"rollback_reset"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil || body.Error.Code == "" {
			t.Fatalf("want {\"error\":{\"code\":...}}, got %s (%v)", b, err)
		}
		out = map[string]any{"code": body.Error.Code, "error": body.Error.Params["error"]}
		if body.RollbackReset != nil {
			out["rollback_reset"] = *body.RollbackReset
		}
		return out
	}
	e.srv.checkDataDirEntries() // the last walk: nothing found

	// private all along: an update on probation keeps its way back
	if out := post(http.StatusOK); voids != 0 || out["rollback_reset"] != nil || out["check"] == nil {
		t.Fatalf("private all along: %d removals, %+v", voids, out)
	}
	// other accounts could read it, not change it: nothing to remove either
	before = dirperm.Exposure{Read: true, Who: []string{"Users"}}
	if out := post(http.StatusOK); voids != 0 || out["rollback_reset"] != nil {
		t.Fatalf("readable: %d removals, %+v", voids, out)
	}
	// other accounts could change it
	before = dirperm.Exposure{Write: true, Who: []string{"Authenticated Users"}}
	if out := post(http.StatusOK); voids != 1 || out["rollback_reset"] != true || out["check"] == nil {
		t.Fatalf("was writable: %d removals, %+v", voids, out)
	}
	// ... but there was no such state (or the shell's profile is elsewhere)
	found = selfupdate.Voided{}
	if out := post(http.StatusOK); voids != 2 || out["rollback_reset"] != nil {
		t.Fatalf("nothing to remove: %d removals, %+v", voids, out)
	}
	found = selfupdate.Voided{Backups: true}
	// what it was like could not be told
	before, beforeErr = dirperm.Exposure{}, errors.New("cannot read the permissions")
	if out := post(http.StatusOK); voids != 3 || out["rollback_reset"] != true {
		t.Fatalf("unknown before: %d removals, %+v", voids, out)
	}
	beforeErr = nil
	// an entry below it had to be reset
	reset = []string{filepath.Join(e.cfg.DataDir, "updates", "backup")}
	if out := post(http.StatusOK); voids != 4 || out["rollback_reset"] != true {
		t.Fatalf("an entry was reset: %d removals, %+v", voids, out)
	}
	reset = nil
	// an entry could not be reset: the state goes although the answer is
	// 409, which says so (the page shows the notice next to the error)
	resetErr = errors.New("dirperm: could not reset x: Access is denied.")
	if out := post(http.StatusConflict); voids != 5 || out["code"] != codeDataDirRestrictFailed || out["error"] != resetErr.Error() || out["rollback_reset"] != true {
		t.Fatalf("an entry could not be reset: %d removals, %+v", voids, out)
	}
	// ... with nothing to remove: a plain error
	found = selfupdate.Voided{}
	if out := post(http.StatusConflict); voids != 6 || out["code"] != codeDataDirRestrictFailed || out["rollback_reset"] != nil {
		t.Fatalf("an entry could not be reset, nothing to remove: %d removals, %+v", voids, out)
	}
	found = selfupdate.Voided{Links: true} // a link in place of updates/ went
	resetErr = nil
	// the last walk had not vouched for it
	walk = dirperm.TreeReport{Entries: 9, Unconfirmed: "x is a link"}
	e.srv.checkDataDirEntries()
	if out := post(http.StatusConflict); voids != 7 || out["rollback_reset"] != true {
		t.Fatalf("the last walk was not OK: %d removals, %+v", voids, out)
	}
	found = selfupdate.Voided{Backups: true}
	walk = dirperm.TreeReport{Entries: 9}
	e.srv.checkDataDirEntries()
	if post(http.StatusOK); voids != 7 {
		t.Fatalf("private again: %d removals", voids)
	}
	// the state cannot be removed: no 200, and not "reset" (part of it
	// may be left)
	before = dirperm.Exposure{Write: true, Who: []string{"Users"}}
	voidErr = errors.New("the update's rollback state could not be removed: Access is denied.")
	if out := post(http.StatusInternalServerError); voids != 8 || out["code"] != codeDataDirRestrictFailed || out["error"] != voidErr.Error() || out["rollback_reset"] != nil {
		t.Fatalf("cannot remove: %d removals, %+v", voids, out)
	}
	// the permissions cannot be set: nothing is removed
	voidErr = nil
	restrictDataDir = func(string) error { return errors.New("access denied") }
	if out := post(http.StatusInternalServerError); voids != 8 {
		t.Fatalf("permissions not set: %d removals, %+v", voids, out)
	}
}
