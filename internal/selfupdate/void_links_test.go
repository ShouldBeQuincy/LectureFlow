// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Security review SEC17, 高-A: an account that could once write to the data
// directory leaves a link in place of updates/ or electron/ — on Windows a
// junction, which needs no right on its target and no privilege. Removing
// the rollback state, or the update's own clean-up, must not remove what
// the link leads to. dirLink makes a junction on Windows, a symbolic link
// elsewhere. Everything here lies in t.TempDir(); links are removed (the
// link itself) before the temporary directories are.

// writeTree writes files (rel, "/"-separated → content) below dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot lists every entry below dir (rel → content, "/" for a folder),
// links not followed.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			out[rel] = "/"
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		default:
			out[rel] = "link"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("%s: %d entries before, %d after: %v", what, len(before), len(after), after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %s was %q, is %q", what, k, v, after[k])
		}
	}
}

// victimFiles is what lies where a link leads: a folder of the user's that
// happens to look like the updates directory or the shell's profile.
var victimFiles = map[string]string{
	"backup/photos/important.txt": "the user's photos",
	"applying.json":               `{"schema":1,"version":"3.2.0","from_version":"3.1.0","phase":"installing"}`,
	"previous.json":               `{"schema":1,"version":"3.1.0","backup":"backup/3.1.0","note":"not the program's"}`,
	"selfupdate-trust.json":       `{"schema":1,"backups":{}}`,
	"Preferences":                 "{}",
	"3.0.0/keep.txt":              "a folder named like a version",
	".import-1/keep.txt":          "a folder named like an offline import",
}

func newVictim(t *testing.T) string {
	t.Helper()
	victim := filepath.Join(privateDataDir(t), "victim")
	writeTree(t, victim, victimFiles)
	return victim
}

func TestVoidRollbackUpdatesLink(t *testing.T) {
	data := privateDataDir(t)
	victim := newVictim(t)
	writeTree(t, data, map[string]string{
		"electron/selfupdate-trust.json": `{"schema":1}`,
		"electron/Preferences":           "{}",
	})
	link := filepath.Join(data, "updates")
	dirLink(t, link, victim)
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Record: true, Links: true}) || !v.Any() {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "where updates led", before, snapshot(t, victim))
	if pathThere(link) {
		t.Fatal("the link is still there")
	}
	if pathThere(filepath.Join(data, "electron", TrustRecordFile)) || !pathThere(filepath.Join(data, "electron", "Preferences")) {
		t.Fatal("the shell's record was not removed, or more than it was")
	}
	// nothing left: the rollback state is gone, not hidden
	if v, err := VoidRollback(data); err != nil || v.Any() {
		t.Fatalf("again: %+v %v", v, err)
	}
}

func TestVoidRollbackProfileLink(t *testing.T) {
	data := preparedRollback(t, false)
	victim := newVictim(t)
	link := filepath.Join(data, ShellProfileDir)
	dirLink(t, link, victim)
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Links: true, Applying: true, Backups: true, Previous: true}) {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "where electron led", before, snapshot(t, victim))
	if pathThere(link) {
		t.Fatal("the link is still there")
	}
	for _, gone := range []string{"updates/applying.json", "updates/backup"} {
		if pathThere(filepath.Join(data, filepath.FromSlash(gone))) {
			t.Errorf("%s is still there", gone)
		}
	}
}

// A link below updates/backup (SEC17 probe fa4/b) is removed, not entered.
func TestVoidRollbackLinkInBackup(t *testing.T) {
	data := preparedRollback(t, true)
	victim := newVictim(t)
	dirLink(t, filepath.Join(data, "updates", "backup", "3.0.0"), victim)
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Record: true, Applying: true, Backups: true, Previous: true}) {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "where backup/3.0.0 led", before, snapshot(t, victim))
	if pathThere(filepath.Join(data, "updates", "backup")) {
		t.Fatal("updates/backup is still there")
	}
}

// previous.json as a hard link to a file elsewhere: rewritten by replacing
// the entry, never through it.
func TestVoidRollbackPreviousHardLink(t *testing.T) {
	data := preparedRollback(t, true)
	victim := newVictim(t)
	prev := filepath.Join(data, "updates", previousFile)
	if err := os.Remove(prev); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(victim, previousFile), prev); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || !v.Previous {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "the hard link's other name", before, snapshot(t, victim))
	if b, err := os.ReadFile(prev); err != nil || string(b) == victimFiles[previousFile] {
		t.Fatalf("previous.json: %s %v", b, err)
	}
}

// The files themselves as symbolic links to files elsewhere: the links go,
// the files they name stay as they were. On Windows creating a symbolic
// link needs a privilege (or Developer Mode); without it this case cannot
// be laid out there and is skipped — the junction cases need none
// (TestVoidRollbackFileNamesJunctionWindows covers the same names).
func TestVoidRollbackFileSymlinks(t *testing.T) {
	data := privateDataDir(t)
	victim := newVictim(t)
	for _, d := range []string{"updates", ShellProfileDir} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		filepath.Join(data, "updates", previousFile):          filepath.Join(victim, previousFile),
		filepath.Join(data, "updates", applyingFile):          filepath.Join(victim, applyingFile),
		filepath.Join(data, ShellProfileDir, TrustRecordFile): filepath.Join(victim, TrustRecordFile),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("no symbolic links without the privilege here: %v", err)
		}
		t.Cleanup(func() { os.Remove(link) })
	}
	before := snapshot(t, victim)

	v, err := VoidRollback(data)
	if err != nil || v != (Voided{Record: true, Applying: true, Previous: true}) {
		t.Fatalf("VoidRollback: %+v %v", v, err)
	}
	sameSnapshot(t, "where the links led", before, snapshot(t, victim))
	for link := range links {
		if pathThere(link) {
			t.Errorf("%s is still there", link)
		}
	}
}

// The update's own clean-up (old version folders, offline-import staging)
// removes nothing through an updates directory that is a link.
func TestUpdaterCleanupUpdatesLink(t *testing.T) {
	data := privateDataDir(t)
	victim := newVictim(t)
	dirLink(t, filepath.Join(data, "updates"), victim)
	before := snapshot(t, victim)

	u := newTestUpdater(t, nil, newTestKey(t), func(o *Options) { o.DataDir = data }) // Startup: cleanup(true)
	u.removeVersion("3.0.0")
	u.cleanup(false)
	sameSnapshot(t, "where updates led", before, snapshot(t, victim))
}

// Nor does it write or remove single files there (SEC17 低-F): latest.json
// and its signature, pending.json; the error says why.
func TestUpdaterFilesThroughUpdatesLink(t *testing.T) {
	data := privateDataDir(t)
	victim := newVictim(t)
	writeTree(t, victim, map[string]string{
		"latest.json":         "the user's latest.json",
		"latest.json.minisig": "the user's signature",
		"pending.json":        `{"schema":1,"version":"not a version"}`,
	})
	dirLink(t, filepath.Join(data, "updates"), victim)
	before := snapshot(t, victim)

	// Startup: an unverifiable latest.json and an invalid pending.json
	// would be removed
	u := newTestUpdater(t, nil, newTestKey(t), func(o *Options) { o.DataDir = data })
	if err := u.writeUpdatesFile(latestFile, []byte("{}")); !errors.Is(err, errNotPlainDir) {
		t.Fatalf("writing through the link: %v", err)
	}
	if err := u.removeUpdatesFile(pendingFile); !errors.Is(err, errNotPlainDir) {
		t.Fatalf("removing through the link: %v", err)
	}
	u.removeLatest()
	sameSnapshot(t, "where updates led", before, snapshot(t, victim))

	// a plain updates directory: written and removed as before
	plain := privateDataDir(t)
	p := newTestUpdater(t, nil, newTestKey(t), func(o *Options) { o.DataDir = plain })
	if err := p.writeUpdatesFile(latestFile, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(plain, "updates", latestFile)); err != nil || string(b) != "{}" {
		t.Fatalf("latest.json: %q %v", b, err)
	}
	p.removeLatest()
	if pathThere(filepath.Join(plain, "updates", latestFile)) {
		t.Fatal("latest.json is still there")
	}
	if err := p.removeUpdatesFile(pendingFile); err != nil {
		t.Fatalf("removing what is not there: %v", err)
	}
	if _, err := openPlainDir(filepath.Join(plain, "missing")); !isNotExist(err) {
		t.Fatalf("a missing directory: %v", err)
	}
}

// ... while in a plain updates directory it removes what it always did.
func TestUpdaterCleanupPlainUpdates(t *testing.T) {
	data := privateDataDir(t)
	writeTree(t, filepath.Join(data, "updates"), map[string]string{
		"3.0.0/old.exe":      "an old package",
		"2.9.0/old.exe":      "an older one",
		".import-1/part":     "an offline import left behind",
		"backup/3.0.0/x":     "the shell's",
		"notaversion/keep":   "another name",
		"3.1.0/keep.txt":     "the running version's",
		"previous.json.keep": "a file",
	})
	link := filepath.Join(data, "updates", "2.8.0")
	victim := newVictim(t)
	dirLink(t, link, victim) // a link named like a version: removed, not entered
	before := snapshot(t, victim)

	u := newTestUpdater(t, nil, newTestKey(t), func(o *Options) { o.DataDir = data }) // Startup: cleanup(true)
	for _, gone := range []string{"3.0.0", "2.9.0", ".import-1"} {
		if pathThere(filepath.Join(data, "updates", gone)) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"backup/3.0.0/x", "notaversion/keep", "3.1.0/keep.txt", "previous.json.keep"} {
		if !pathThere(filepath.Join(data, "updates", filepath.FromSlash(kept))) {
			t.Errorf("%s was removed", kept)
		}
	}
	u.removeVersion("2.8.0")
	sameSnapshot(t, "where updates/2.8.0 led", before, snapshot(t, victim))
}
