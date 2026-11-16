// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// dirLink makes link a junction to target (mklink /J needs no privilege;
// in Go 1.23 and later os.Lstat reports it as ModeIrregular, not as
// ModeSymlink). See void_links_test.go.
func dirLink(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	// (registered after the temporary directories: runs before they go)
	t.Cleanup(func() { os.Remove(link) })
}

// The data directory itself a junction (security review SEC30, 低-2: the
// folder "收紧权限" works on, replaced by a link while it runs): nothing
// where it leads is removed or changed, and VoidRollback says so with an
// error (the state is not reported as removed).
func TestVoidRollbackRootJunctionWindows(t *testing.T) {
	elsewhere := filepath.Join(privateDataDir(t), "elsewhere")
	writeTree(t, elsewhere, map[string]string{
		"electron/selfupdate-trust.json":    `{"schema":1,"backups":{}}`,
		"updates/applying.json":             `{"schema":1,"version":"3.2.0","from_version":"3.1.0","phase":"installing"}`,
		"updates/previous.json":             `{"schema":1,"version":"3.1.0","backup":"backup/3.1.0"}`,
		"updates/backup/3.1.0/app/keep.txt": "the user's own",
	})
	before := snapshot(t, elsewhere)
	data := filepath.Join(privateDataDir(t), "data")
	dirLink(t, data, elsewhere)
	v, err := VoidRollback(data)
	if err == nil || v.Any() {
		t.Fatalf("VoidRollback through a junction: %+v %v", v, err)
	}
	sameSnapshot(t, "where the junction led", before, snapshot(t, elsewhere))
	if !pathThere(data) {
		t.Error("the junction itself was removed")
	}
}

// previous.json, applying.json and the shell's record as junctions (to a
// folder elsewhere): the links go, never what they lead to.
func TestVoidRollbackFileNamesJunctionWindows(t *testing.T) {
	data := privateDataDir(t)
	victim := newVictim(t)
	for _, d := range []string{"updates", ShellProfileDir} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := []string{
		filepath.Join(data, "updates", previousFile),
		filepath.Join(data, "updates", applyingFile),
		filepath.Join(data, ShellProfileDir, TrustRecordFile),
	}
	for _, link := range links {
		dirLink(t, link, victim)
	}
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Record: true, Applying: true, Previous: true}) {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "where the junctions led", before, snapshot(t, victim))
	for _, link := range links {
		if pathThere(link) {
			t.Errorf("%s is still there", link)
		}
	}
}
