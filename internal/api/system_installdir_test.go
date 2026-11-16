// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
	"lectureflow/internal/selfupdate"
)

// The install_dir row (security audit M-4). selfupdate.CheckInstallDir is
// injected here (internal/selfupdate tests it with real permissions).

const testInstallDir = `D:\LectureFlow`

// fakeInstallCheck makes the row's check answer *err (and count its
// calls) for the test.
func fakeInstallCheck(t *testing.T, err *error) *[]string {
	t.Helper()
	orig := checkInstallDirTrust
	var asked []string
	checkInstallDirTrust = func(dir string) error {
		asked = append(asked, dir)
		return *err
	}
	resetInstallDirRow := func() {
		installDirRow.mu.Lock()
		installDirRow.dir = ""
		installDirRow.mu.Unlock()
	}
	resetInstallDirRow()
	t.Cleanup(func() {
		checkInstallDirTrust = orig
		resetInstallDirRow()
	})
	return &asked
}

// systemChecks is GET /api/system's rows of a desktop env.
func systemChecks(t *testing.T, e *env) []SystemCheck {
	t.Helper()
	resp, body := e.doWith("GET", "/api/system", nil, testToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/system: %d %s", resp.StatusCode, body)
	}
	var sys System
	if err := json.Unmarshal(body, &sys); err != nil {
		t.Fatal(err)
	}
	return sys.Checks
}

func installRow(t *testing.T, e *env) (SystemCheck, int) {
	t.Helper()
	rows := systemChecks(t, e)
	for i, c := range rows {
		if c.Name == checkInstallDir {
			return c, i
		}
	}
	return SystemCheck{}, -1
}

// noProfile makes the user's profile and %LOCALAPPDATA% the given folders
// for the hints, and every path lie where it is written.
func noProfile(t *testing.T, local, profile string) {
	t.Helper()
	ol, op, of := installLocalAppData, installProfile, installFinalPath
	installLocalAppData = func() string { return local }
	installProfile = func() string { return profile }
	installFinalPath = func(p string) (string, error) { return p, nil }
	t.Cleanup(func() { installLocalAppData, installProfile, installFinalPath = ol, op, of })
}

// The hint follows where the problem lies (security review SEC20 低-1,
// SEC20b 低-C): the default folder written out; a folder above where the
// installation really lies (a subst drive) is "above"; a link on the way
// gets the hint that claims nothing about who may change what.
func TestInstallDirHints(t *testing.T) {
	base := t.TempDir()
	local := filepath.Join(base, "Users", "alice", "AppData", "Local")
	noProfile(t, local, filepath.Join(base, "Users", "alice"))
	def := filepath.Join(local, "Programs", "LectureFlow")
	apps := filepath.Join(base, "D", "Apps")
	dir := filepath.Join(apps, "lectureflow")
	// X:\lectureflow on a subst drive standing for D\Apps
	subst := filepath.Join(base, "X", "lectureflow")
	// the root of the drive dir is on (C:\ or /): others can delete or
	// replace its entries, not rename or move it (SEC20 recheck 低-E)
	root := filepath.VolumeName(base) + string(filepath.Separator)
	installFinalPath = func(p string) (string, error) {
		if p == subst {
			return dir, nil
		}
		return p, nil
	}
	for _, c := range []struct {
		what, dir, p string
		unconfirmed  bool
		code         string
		params       map[string]any
	}{
		{"the folder itself", dir, dir, false, codeHintInstallDir, map[string]any{"dir": dir, "default": def}},
		{"an entry in it", dir, filepath.Join(dir, "resources", "app.asar"), false, codeHintInstallDir, map[string]any{"dir": dir, "default": def}},
		{"a folder above", dir, apps, false, codeHintInstallDirAbove, map[string]any{"path": apps, "default": def}},
		{"the root above", dir, filepath.Join(base, "D"), false, codeHintInstallDirAbove, map[string]any{"path": filepath.Join(base, "D"), "default": def}},
		{"the drive's root above", dir, root, false, codeHintInstallDirRoot, map[string]any{"path": root, "default": def}},
		{"a sibling is not above", dir, filepath.Join(apps, "lectureflow2"), false, codeHintInstallDir, map[string]any{"dir": dir, "default": def}},
		{"above the real path of a subst drive", subst, apps, false, codeHintInstallDirAbove, map[string]any{"path": apps, "default": def}},
		{"a link on the way", dir, apps, true, codeHintInstallDirUnconfirmed, map[string]any{"path": apps, "default": def}},
		{"cannot be read", dir, dir, true, codeHintInstallDirUnconfirmed, map[string]any{"path": dir, "default": def}},
		// the default place: installing again there does not help
		{"the default place", def, filepath.Join(local, "Programs"), false, codeHintInstallDirProfile,
			map[string]any{"path": filepath.Join(local, "Programs")}},
		{"the default place, unconfirmed", def, def, true, codeHintInstallDirProfile, map[string]any{"path": def}},
		{"the profile", filepath.Join(base, "Users", "alice", "Apps", "lf"), filepath.Join(base, "Users", "alice", "Apps", "lf"), false, codeHintInstallDirProfile,
			map[string]any{"path": filepath.Join(base, "Users", "alice", "Apps", "lf")}},
	} {
		var sc SystemCheck
		setInstallDirHint(&sc, c.dir, c.p, c.unconfirmed)
		if sc.HintInfo == nil || sc.HintInfo.Code != c.code || len(sc.HintInfo.Params) != len(c.params) || sc.Hint == "" {
			t.Errorf("%s: %+v", c.what, sc.HintInfo)
			continue
		}
		for k, v := range c.params {
			if sc.HintInfo.Params[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.what, k, sc.HintInfo.Params[k], v)
			}
		}
		if strings.Contains(sc.Hint, "%LOCALAPPDATA%") {
			t.Errorf("%s: the default folder is not written out: %s", c.what, sc.Hint)
		}
	}
	// a share's root without the trailing separator is a root too (Windows
	// paths only)
	if runtime.GOOS == "windows" {
		for _, p := range []string{`\\fs\apps`, `\\fs\apps\`, `D:\`} {
			if !volumeRoot(p) {
				t.Errorf("volumeRoot(%s) = false", p)
			}
		}
		for _, p := range []string{`\\fs\apps\lf`, `D:\Apps`} {
			if volumeRoot(p) {
				t.Errorf("volumeRoot(%s) = true", p)
			}
		}
		var sc SystemCheck
		setInstallDirHint(&sc, `\\fs\apps\lectureflow`, `\\fs\apps`, false)
		if sc.HintInfo == nil || sc.HintInfo.Code != codeHintInstallDirRoot {
			t.Errorf("a share's root without the separator: %+v", sc.HintInfo)
		}
	}
	// without %LOCALAPPDATA% the name stays (nothing better to say)
	noProfile(t, "", "")
	if d := defaultInstallDir(); d != `%LOCALAPPDATA%\Programs\LectureFlow` {
		t.Fatalf("default: %s", d)
	}
}

func TestInstallDirRow(t *testing.T) {
	var cerr error
	asked := fakeInstallCheck(t, &cerr)
	noProfile(t, "", "")
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken, InstallDir: testInstallDir})
	e.srv.diag.waitStartup()

	// private: ok, after the data folder's rows
	c, at := installRow(t, e)
	if at < 0 || c.Status != statusOK || c.DetailInfo == nil || c.DetailInfo.Code != codeInstallDirPrivate ||
		c.DetailInfo.Params["path"] != testInstallDir || c.HintInfo != nil {
		t.Fatalf("private: %d %+v", at, c)
	}
	rows := systemChecks(t, e)
	for i, r := range rows {
		if (r.Name == checkDataDir || r.Name == checkCredentials || r.Name == checkDataDirLocation) && i > at {
			t.Fatalf("%s after install_dir: %v", r.Name, rows)
		}
	}
	if (*asked)[0] != testInstallDir {
		t.Fatalf("asked about %v", *asked)
	}

	// another account may change it: a warning naming the entry and who,
	// with the hint (no button: nothing else in the row)
	n := len(*asked)
	entry := testInstallDir + `\resources\app.asar`
	cerr = model.Errorf(selfupdate.CodeUntrustedInstallDir, map[string]any{"path": entry, "who": `NT AUTHORITY\Authenticated Users`}, errors.New("test"))
	if c, _ := installRow(t, e); c.Status != statusOK || len(*asked) != n {
		t.Fatalf("not looked at again within the cache: %+v %d", c, len(*asked))
	}
	e.srv.checkDataDirEntries() // "重新检查" (and the start) look again
	c, _ = installRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeInstallDirShared || c.DetailInfo.Params["path"] != entry ||
		c.DetailInfo.Params["who"] != `NT AUTHORITY\Authenticated Users` || c.HintInfo == nil || c.HintInfo.Code != codeHintInstallDir ||
		c.HintInfo.Params["dir"] != testInstallDir || c.Detail == "" || c.Hint == "" {
		t.Fatalf("shared: %+v %+v %+v", c, c.DetailInfo, c.HintInfo)
	}

	// not confirmed (a link, too many entries): its own text with the reason
	cerr = model.Errorf(selfupdate.CodeUntrustedInstallDir, map[string]any{"path": testInstallDir, "who": dirperm.NotConfirmed + "more than 20000 entries"}, errors.New("test"))
	e.srv.checkDataDirEntries()
	c, _ = installRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeInstallDirUnconfirmed || c.DetailInfo.Params["reason"] != "more than 20000 entries" ||
		c.HintInfo == nil || c.HintInfo.Code != codeHintInstallDirUnconfirmed {
		t.Fatalf("unconfirmed: %+v %+v", c, c.DetailInfo)
	}
	// the check fails otherwise (the folder is gone): not vouched for either
	cerr = model.Errorf(selfupdate.CodeIO, nil, errors.New("CreateFile D:\\LectureFlow: The system cannot find the file specified."))
	e.srv.checkDataDirEntries()
	c, _ = installRow(t, e)
	if c.Status != statusWarn || c.DetailInfo.Code != codeInstallDirUnconfirmed || c.DetailInfo.Params["reason"] == "" {
		t.Fatalf("failed: %+v %+v", c, c.DetailInfo)
	}
	cerr = nil
	e.srv.checkDataDirEntries()
	if c, _ := installRow(t, e); c.Status != statusOK {
		t.Fatalf("fixed: %+v", c)
	}
}

// Without an installation directory (development, the portable build,
// macOS, web mode) there is no row and nothing is checked.
func TestInstallDirRowAbsent(t *testing.T) {
	var cerr error
	asked := fakeInstallCheck(t, &cerr)
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	e.srv.diag.waitStartup()
	e.srv.checkDataDirEntries()
	if _, at := installRow(t, e); at >= 0 {
		t.Fatal("a row without an installation directory")
	}
	web := newEnv(t)
	web.srv.diag.waitStartup()
	var sys System
	web.call("GET", "/api/system", nil, http.StatusOK, &sys)
	for _, c := range sys.Checks {
		if c.Name == checkInstallDir {
			t.Fatal("a row in web mode")
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("checked: %v", *asked)
	}
}

// The update's refusal is a 409 like the data folder's.
func TestUntrustedInstallDirHTTP(t *testing.T) {
	if selfUpdateHTTP[selfupdate.CodeUntrustedInstallDir] != http.StatusConflict {
		t.Fatalf("status: %d", selfUpdateHTTP[selfupdate.CodeUntrustedInstallDir])
	}
}
