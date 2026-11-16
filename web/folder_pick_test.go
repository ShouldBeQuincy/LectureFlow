// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// r6 path-grants (security review SEC26 low-1): a folder the user picked
// and then declined, or that the check refused, is given up at once
// (DELETE /api/app/folder-pick, internal/api/handlers_datadir.go) instead of
// staying granted until it expires. The wrapper is api.forgetFolderPick;
// the three pages that pick a folder call it where the pick is dropped.
func TestFolderPickGivenUpSources(t *testing.T) {
	api := readWeb(t, "api.js")
	if !strings.Contains(api, "forgetFolderPick: (path) => request('DELETE', '/api/app/folder-pick', { path }, { quiet: true })") {
		t.Error("api.js: forgetFolderPick is not the quiet DELETE /api/app/folder-pick {path}")
	}
	// every page that shows the folder dialog gives a dropped pick up: on
	// cancel, and when the service refuses the confirmed move or import at
	// once (it returns the grant, and the page asks for the folder again;
	// security review SEC38 low-1); the storage step also when the check
	// refuses the folder or fails
	for name, want := range map[string]int{"settings_app.js": 2, "onboarding_storage.js": 4, "onboarding.js": 2} {
		src := readWeb(t, name)
		if !strings.Contains(src, "pickFolder()") {
			t.Errorf("%s no longer shows the folder dialog: this test is out of date", name)
		}
		if n := strings.Count(src, "api.forgetFolderPick(path)"); n != want {
			t.Errorf("%s gives a pick up in %d places, want %d", name, n, want)
		}
		// never awaited: the page does not wait for the answer
		if strings.Contains(src, "await api.forgetFolderPick") {
			t.Errorf("%s waits for the give-up request", name)
		}
	}
	// no other file shows the folder dialog without this test knowing
	files, err := filepath.Glob("*.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		switch f {
		case "settings_app.js", "onboarding_storage.js", "onboarding.js":
			continue
		}
		if strings.Contains(readWeb(t, f), ".pickFolder()") {
			t.Errorf("%s shows the folder dialog: it must give a dropped pick up too (api.forgetFolderPick), and be listed here", f)
		}
	}
}

// The translators' notes of the two "choose the folder again" errors say
// how long a pick lasts now (ten minutes, never renewed), not the 12 hours
// of a file.
func TestFolderPickErrorNotes(t *testing.T) {
	var ctx map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readWeb(t, filepath.Join("i18n", "context.json"))), &ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"errors.data_dir.path_not_allowed", "errors.data_dir.path_not_granted"} {
		var e struct {
			Notes string `json:"notes"`
		}
		if err := json.Unmarshal(ctx[k], &e); err != nil {
			t.Errorf("%s: %v", k, err)
			continue
		}
		n := e.Notes
		if n == "" {
			t.Errorf("%s: no notes", k)
			continue
		}
		if strings.Contains(n, "12 hours") {
			t.Errorf("%s: the notes still say 12 hours: %s", k, n)
		}
		if !strings.Contains(n, "10 minutes") {
			t.Errorf("%s: the notes do not say how long a pick lasts (10 minutes): %s", k, n)
		}
	}
}

// The behaviour (testdata/folder_pick_test.mjs): each page sends the
// give-up when the confirmation is cancelled (the setup guide's storage
// step also when the check refuses the folder), does not send it when the
// user confirms, and a give-up that fails shows nothing and blocks nothing.
func TestFolderPickGivenUpLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/folder_pick_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "folder_pick_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "folder pick checks: ok") {
		t.Fatalf("folder_pick_test.mjs: %v\n%s", err, out)
	}
}
