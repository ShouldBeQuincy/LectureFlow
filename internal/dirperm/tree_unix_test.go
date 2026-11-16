// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package dirperm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symbolic link of the user inside the tree is not followed and is no
// finding (a Python environment's bin/python is one); as the root of the
// walk it makes the tree unconfirmed.
func TestCheckTreeSymlink(t *testing.T) {
	root := privateTree(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.py"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "components", "whisper", "1.0", "site")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || !rep.OK() || rep.Entries != privateTreeEntries+1 {
		t.Fatalf("a link inside: %+v %v", rep, err)
	}
	if _, err := RestrictTree(root, TreeOptions{}); err != nil {
		t.Fatalf("RestrictTree: %v", err)
	}
	if got := modeOf(t, outside); got != 0o777 {
		t.Fatalf("what the link leads to was changed: %o", got)
	}
	rep, err = CheckTree(link, TreeOptions{})
	if err != nil || rep.OK() || rep.Entries != 1 || !strings.Contains(rep.Unconfirmed, "is a link") {
		t.Fatalf("a link as the root: %+v %v", rep, err)
	}
	if _, err := RestrictTree(link, TreeOptions{}); err == nil {
		t.Fatal("RestrictTree of a link passed")
	}
}

// Inside a directory others cannot enter, the group's and others' write
// bits of an entry reach nobody: no finding, and nothing is changed.
func TestCheckTreeOutOfReach(t *testing.T) {
	root := privateTree(t)
	f := filepath.Join(root, "lectureflow.db")
	if err := os.Chmod(f, 0o666); err != nil {
		t.Fatal(err)
	}
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || !rep.OK() {
		t.Fatalf("out of reach: %+v %v", rep, err)
	}
	if _, err := RestrictTree(root, TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, f); got != 0o666 {
		t.Fatalf("mode changed to %o", got)
	}
}
