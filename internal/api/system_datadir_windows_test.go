// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"lectureflow/internal/selfupdate"
)

// For real: a data directory with the permissions of a folder made under
// D:\ (Authenticated Users may modify) is reported; "收紧权限" makes it and
// what it holds private.
func TestDataDirRestrictWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(e.cfg.DataDir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(e.cfg.DataDir, "components")
	os.MkdirAll(inner, 0o755)
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || !strings.Contains(c.DetailInfo.Params["who"].(string), "Authenticated Users") && !strings.Contains(c.DetailInfo.Params["who"].(string), "S-1-5-11") {
		t.Fatalf("loose: %+v %+v", c, c.DetailInfo)
	}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK {
		t.Fatalf("restrict: %+v", out.Check)
	}
	sd2, err := windows.GetNamedSecurityInfo(inner, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if s := sd2.String(); strings.Contains(s, ";AU)") {
		t.Fatalf("what the data directory holds still lets others in: %s", s)
	}
}

// For real (security audit M-3): the data directory itself is private, but
// a folder of a Python environment keeps entries of its own for other
// accounts (as one made while they could write there). The data_dir row
// names it; "收紧权限" resets it; an entry that cannot be reset is a 409
// naming it, and the row keeps warning.
func TestDataDirRestrictSubItemsWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("private: %+v %+v", c, c.DetailInfo)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	if err := os.MkdirAll(filepath.Join(e.cfg.DataDir, "components"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(e.cfg.DataDir, "components", "whisper")
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(env)
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, sa); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env, "sitecustomize.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.checkDataDirEntries() // as the diagnostics do when they run again
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirRestrict ||
		!strings.HasSuffix(c.DetailInfo.Params["path"].(string), filepath.Join("components", "whisper")) ||
		!strings.Contains(c.DetailInfo.Params["who"].(string), "Authenticated Users") && !strings.Contains(c.DetailInfo.Params["who"].(string), "S-1-5-11") {
		t.Fatalf("a folder other accounts may change: %+v %+v", c, c.DetailInfo)
	}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK {
		t.Fatalf("restrict: %+v", out.Check)
	}
	for _, f := range []string{env, filepath.Join(env, "sitecustomize.py")} {
		sd2, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if s := sd2.String(); strings.Contains(s, ";AU)") {
			t.Fatalf("%s still lets others in: %s", f, s)
		}
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v %+v", c, c.DetailInfo)
	}

	// an entry nobody here may change the permissions of (the owner may
	// only read it, everyone may modify it)
	stuck := filepath.Join(e.cfg.DataDir, "updates", "previous.json")
	if err := os.MkdirAll(filepath.Dir(stuck), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stuck, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	sd, err = windows.SecurityDescriptorFromString("D:P(A;;0x1200a9;;;OW)(A;;0x1301bf;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(stuck, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed || !strings.Contains(info.Params["error"].(string), filepath.Join("updates", "previous.json")) {
		t.Fatalf("an entry that cannot be reset: %+v", info)
	}
	c = dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || !strings.HasSuffix(c.DetailInfo.Params["path"].(string), filepath.Join("updates", "previous.json")) {
		t.Fatalf("after a failed reset: %+v %+v", c, c.DetailInfo)
	}
}

// loosenWindows gives p an entry of its own for Users (full control).
func loosenWindows(t *testing.T, p string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// For real (security review SEC17, 中-2): the desktop shell's profile in
// the data directory is looked at as a whole. A file of the browser's that
// other accounts may change (its settings, a cache of compiled scripts) is
// reported and reset like the update's files.
func TestDataDirRestrictProfileWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	cache := filepath.Join(e.cfg.DataDir, "electron", "Code Cache", "js")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(e.cfg.DataDir, "electron", "Preferences")
	script := filepath.Join(cache, "index")
	for _, f := range []string{prefs, script} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.checkDataDirEntries()
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("a private profile: %+v %+v", c, c.DetailInfo)
	}
	loosenWindows(t, script)
	loosenWindows(t, prefs)
	e.srv.checkDataDirEntries()
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.HintInfo.Code != codeHintDataDirRestrict ||
		!strings.HasSuffix(c.DetailInfo.Params["path"].(string), filepath.Join("electron", "Code Cache", "js", "index")) {
		t.Fatalf("a file of the profile other accounts may change: %+v %+v", c, c.DetailInfo)
	}
	var out struct {
		Check SystemCheck `json:"check"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	for _, f := range []string{prefs, script} {
		sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if s := sd.String(); strings.Contains(s, ";BU)") {
			t.Fatalf("%s still lets others in: %s", f, s)
		}
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v %+v", c, c.DetailInfo)
	}
}

// The row never says "private" about a data directory the in-app update
// refuses (SEC17, 低-3): what selfupdate.CheckTrust says about updates/ is
// part of the walk — an entry with its accounts, or why it cannot be
// vouched for. The errors here are CheckTrust's own, about other folders.
func TestDataDirRowFollowsUpdateTrustWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	abs, _ := filepath.Abs(e.cfg.DataDir)
	orig := checkUpdateTrust
	t.Cleanup(func() { checkUpdateTrust = orig })

	// an updates directory with a file Users may change
	other := filepath.Join(e.cfg.DataDir, "courses", "other")
	if err := os.MkdirAll(filepath.Join(other, "updates"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(other, "updates", "pending.json")
	if err := os.WriteFile(pkg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	loosenWindows(t, pkg)
	asked := ""
	checkUpdateTrust = func(dir string) error {
		asked = dir
		return selfupdate.CheckTrust(filepath.Join(other, "updates"))
	}
	e.srv.checkDataDirEntries()
	if !strings.HasSuffix(asked, filepath.Join(filepath.Base(abs), "updates")) {
		t.Fatalf("asked about %q", asked)
	}
	c := dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared || c.DetailInfo.Params["path"] != pkg {
		t.Fatalf("an entry the update refuses: %+v %+v", c, c.DetailInfo)
	}
	// a tree the update cannot vouch for: a junction in it
	link := filepath.Join(other, "updates", "backup")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, other).CombinedOutput(); err != nil {
		t.Skipf("no junction on this system: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(link) })
	if err := os.Remove(pkg); err != nil {
		t.Fatal(err)
	}
	e.srv.checkDataDirEntries()
	c = dataDirRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeDataDirUnconfirmed || c.DetailInfo.Params["path"] != abs ||
		!strings.Contains(c.DetailInfo.Params["reason"].(string), link) || strings.HasPrefix(c.DetailInfo.Params["reason"].(string), "not confirmed") ||
		c.HintInfo.Code != codeHintDataDirUnconfirm {
		t.Fatalf("a tree the update cannot vouch for: %+v %+v", c, c.DetailInfo)
	}
	// and "收紧权限" does not answer 200 then
	info := e.callErrInfo("POST", "/api/system/data-dir/restrict", nil, http.StatusConflict)
	if info.Code != codeDataDirRestrictFailed || !strings.Contains(info.Params["error"].(string), "not confirmed") {
		t.Fatalf("restrict: %+v", info)
	}
	// the update has nothing to say: the row is the walk's
	checkUpdateTrust = orig
	e.srv.checkDataDirEntries()
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("private: %+v %+v", c, c.DetailInfo)
	}
}

// forgedRollback lays out in the data directory what an account could
// prepare while it could write there (SEC17, 中-3): the shell's record of
// an "installation on probation" and of a backup's hash, applying.json,
// the backup with a program and previous.json naming it.
func forgedRollback(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"electron/selfupdate-trust.json":           `{"schema":1,"install":{"version":"3.2.0","from_version":"3.1.0","target":"C:\\Programs\\LectureFlow","kind":"installer"},"backups":{"3.1.0":{"dir":"backup/3.1.0","kind":"installer","item":".","tree":"ab"}}}`,
		"updates/applying.json":                    `{"schema":1,"version":"3.2.0","from_version":"3.1.0","phase":"installing","tries":0}`,
		"updates/backup/3.1.0/app/LectureFlow.exe": "another program",
		"updates/backup/3.1.0/backup.json":         `{"schema":1,"version":"3.1.0","kind":"installer","item":"."}`,
		"updates/previous.json":                    `{"schema":1,"version":"3.1.0","platform":"windows-x64","kind":"installer","backup":"backup/3.1.0","created_at":"2026-11-02T12:00:00Z"}`,
	}
	out := map[string]string{}
	for rel, body := range files {
		p := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		out[rel] = p
	}
	return out
}

// For real (SEC17, 中-3): a data directory other accounts could write to
// holds a prepared rollback state. "收紧权限" makes the directory private
// and removes that state; nothing of it is left for a later start of the
// desktop shell to believe.
func TestDataDirRestrictVoidsForgedRollbackWindows(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	dataDirAccess.mu.Lock()
	dataDirAccess.dir = ""
	dataDirAccess.mu.Unlock()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;0x1301bf;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(e.cfg.DataDir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	files := forgedRollback(t, e.cfg.DataDir)
	if c := dataDirRow(t, e); c.Status != statusWarn || c.DetailInfo.Code != codeDataDirShared {
		t.Fatalf("loose: %+v %+v", c, c.DetailInfo)
	}
	// while it is loose the update refuses everything in it anyway
	if err := selfupdate.CheckTrust(filepath.Join(e.cfg.DataDir, "updates")); selfupdate.CodeOf(err) != selfupdate.CodeUntrustedDir {
		t.Fatalf("CheckTrust of a loose data directory: %v", err)
	}
	var out struct {
		Check         SystemCheck `json:"check"`
		RollbackReset bool        `json:"rollback_reset"`
	}
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.Check.Status != statusOK || !out.RollbackReset {
		t.Fatalf("restrict: %+v", out)
	}
	for _, rel := range []string{"electron/selfupdate-trust.json", "updates/applying.json", "updates/backup/3.1.0/app/LectureFlow.exe", "updates/backup/3.1.0/backup.json"} {
		if _, err := os.Lstat(files[rel]); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(e.cfg.DataDir, "updates", "backup")); !os.IsNotExist(err) {
		t.Errorf("updates/backup is still there (%v)", err)
	}
	if b, err := os.ReadFile(files["updates/previous.json"]); err != nil || strings.Contains(string(b), "backup") || !strings.Contains(string(b), `"version": "3.1.0"`) {
		t.Fatalf("previous.json: %s %v", b, err)
	}
	if err := selfupdate.CheckTrust(filepath.Join(e.cfg.DataDir, "updates")); err != nil {
		t.Fatalf("CheckTrust afterwards: %v", err)
	}
	if c := dataDirRow(t, e); c.Status != statusOK {
		t.Fatalf("after restricting: %+v %+v", c, c.DetailInfo)
	}

	// The same state in a data directory that was private all along (an
	// update really on probation): "收紧权限" leaves it alone.
	files = forgedRollback(t, e.cfg.DataDir)
	e.srv.checkDataDirEntries()
	out.RollbackReset = false
	e.call("POST", "/api/system/data-dir/restrict", nil, http.StatusOK, &out)
	if out.RollbackReset {
		t.Fatal("the rollback state of a private data directory was reported as removed")
	}
	for rel, p := range files {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed from a data directory that was private all along: %v", rel, err)
		}
	}
	if b, _ := os.ReadFile(files["updates/previous.json"]); !strings.Contains(string(b), `"backup":"backup/3.1.0"`) {
		t.Fatalf("previous.json was changed: %s", b)
	}
}
