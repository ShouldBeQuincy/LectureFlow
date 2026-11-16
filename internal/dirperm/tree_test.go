// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateTree makes a private directory holding
//
//	updates/3.2.0/update.json
//	updates/backup/3.1.0/app/LectureFlow.exe
//	components/whisper/1.0/venv/pyvenv.cfg
//	lectureflow.db
//
// (14 entries: the root, 9 directories, 4 files).
func privateTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	if _, err := MkdirPrivate(root); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"updates/3.2.0/update.json",
		"updates/backup/3.1.0/app/LectureFlow.exe",
		"components/whisper/1.0/venv/pyvenv.cfg",
		"lectureflow.db",
	} {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const privateTreeEntries = 14

// A tree made inside a private directory is OK, and every entry is counted.
func TestCheckTreePrivate(t *testing.T) {
	root := privateTree(t)
	rep, err := CheckTree(root, TreeOptions{})
	if err != nil || !rep.OK() || rep.Entries != privateTreeEntries {
		t.Fatalf("private tree: %+v %v", rep, err)
	}
	if p, who := rep.Problem(); p != "" || who != "" {
		t.Fatalf("Problem of an OK report: %q %q", p, who)
	}
	// a single file is a tree of one entry
	rep, err = CheckTree(filepath.Join(root, "lectureflow.db"), TreeOptions{})
	if err != nil || !rep.OK() || rep.Entries != 1 {
		t.Fatalf("one file: %+v %v", rep, err)
	}
	if fixed, err := RestrictTree(root, TreeOptions{}); err != nil || len(fixed) != 0 {
		t.Fatalf("RestrictTree of a clean tree: %v %v", fixed, err)
	}
	if _, err := CheckTree(filepath.Join(root, "missing"), TreeOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root: %v", err)
	}
	if _, err := RestrictTree(filepath.Join(root, "missing"), TreeOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RestrictTree of a missing root: %v", err)
	}
	if _, err := CheckTree("", TreeOptions{}); err == nil {
		t.Fatal("empty path accepted")
	}
}

// Beyond the bounds the tree is unconfirmed, never OK; RestrictTree says so
// instead of reporting success.
func TestCheckTreeBounds(t *testing.T) {
	root := privateTree(t)
	rep, err := CheckTree(root, TreeOptions{MaxEntries: 4})
	if err != nil || rep.OK() || rep.Entries != 4 || !strings.Contains(rep.Unconfirmed, "more than 4 entries") {
		t.Fatalf("MaxEntries: %+v %v", rep, err)
	}
	if _, who := rep.Problem(); !strings.HasPrefix(who, "not confirmed: ") {
		t.Fatalf("Problem: %q", who)
	}
	// exactly as many entries as the tree has is fine
	if rep, err := CheckTree(root, TreeOptions{MaxEntries: privateTreeEntries}); err != nil || !rep.OK() {
		t.Fatalf("MaxEntries = size: %+v %v", rep, err)
	}
	// updates/backup/3.1.0/app/LectureFlow.exe lies 5 levels below the root
	rep, err = CheckTree(root, TreeOptions{MaxDepth: 4})
	if err != nil || rep.OK() || !strings.Contains(rep.Unconfirmed, "deeper than 4 levels") {
		t.Fatalf("MaxDepth: %+v %v", rep, err)
	}
	if rep, err := CheckTree(root, TreeOptions{MaxDepth: 5}); err != nil || !rep.OK() {
		t.Fatalf("MaxDepth = depth: %+v %v", rep, err)
	}
	if _, err := RestrictTree(root, TreeOptions{MaxEntries: 4}); err == nil || !strings.Contains(err.Error(), "more than 4 entries") {
		t.Fatalf("RestrictTree beyond the bounds: %v", err)
	}
	// one folder with more entries than the bound: its names are not even
	// read past the bound
	wide := filepath.Join(root, "components", "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(wide, fmt.Sprintf("f%02d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names, more, err := readNames(wide, 10)
	if err != nil || !more || len(names) != 10 {
		t.Fatalf("readNames: %v %v %v", names, more, err)
	}
	if names, more, err := readNames(wide, 30); err != nil || more || len(names) != 30 || names[0] != "f00" || names[29] != "f29" {
		t.Fatalf("readNames at the bound: %v %v %v", names, more, err)
	}
	rep, err = CheckTree(wide, TreeOptions{MaxEntries: 10})
	if err != nil || rep.OK() || rep.Entries > 10 || !strings.Contains(rep.Unconfirmed, "holds more than 10 entries") {
		t.Fatalf("a wide folder: %+v %v", rep, err)
	}
}

// Scope narrows the walk: ScopeSelf checks a directory without what it
// holds, ScopeSkip leaves an entry alone.
func TestCheckTreeScope(t *testing.T) {
	root := privateTree(t)
	var asked []string
	rep, err := CheckTree(root, TreeOptions{Scope: func(rel string) Scope {
		asked = append(asked, rel)
		switch {
		case rel == "" || rel == "updates" || strings.HasPrefix(rel, "updates/"):
			return ScopeAll
		case rel == "components":
			return ScopeSelf
		}
		return ScopeSkip
	}})
	// the root, updates and the 6 entries below it, components itself
	if err != nil || !rep.OK() || rep.Entries != 9 {
		t.Fatalf("scoped: %+v %v (asked %v)", rep, err, asked)
	}
	for _, rel := range asked {
		if strings.HasPrefix(rel, "components/") {
			t.Fatalf("asked about %q below a ScopeSelf directory", rel)
		}
	}
	// the root alone
	rep, err = CheckTree(root, TreeOptions{Scope: func(string) Scope { return ScopeSelf }})
	if err != nil || !rep.OK() || rep.Entries != 1 {
		t.Fatalf("root only: %+v %v", rep, err)
	}
}

// The rules of macOS and Linux for one entry (they are plain arithmetic,
// so every system runs them).
func TestPosixEntry(t *testing.T) {
	const me = 501
	for _, c := range []struct {
		name               string
		mode               os.FileMode
		uid                uint32
		group              string
		reach              bool
		write, owner, bits bool
		childReach         bool
		who                string
	}{
		{name: "my file", mode: 0o644, uid: me, group: "staff", reach: true},
		{name: "root's file", mode: 0o644, uid: 0, group: "wheel", reach: true},
		{name: "another account's file", mode: 0o600, uid: 502, group: "staff", write: true, owner: true, who: "owner 502"},
		{name: "another account's file, out of reach", mode: 0o600, uid: 502, group: "staff", reach: false, write: true, owner: true, who: "owner 502"},
		{name: "group may write", mode: 0o664, uid: me, group: "staff", reach: true, write: true, bits: true, who: "group staff"},
		{name: "group may write, out of reach", mode: 0o664, uid: me, group: "staff"},
		{name: "the user's private group", mode: 0o664, uid: me, group: "alice", reach: true},
		{name: "others may write", mode: 0o646, uid: me, group: "staff", reach: true, write: true, bits: true, who: "others"},
		{name: "both", mode: 0o666, uid: me, group: "staff", reach: true, write: true, bits: true, who: "group staff, others"},
		{name: "my directory, others pass", mode: os.ModeDir | 0o755, uid: me, group: "staff", reach: true, childReach: true},
		{name: "my directory, closed", mode: os.ModeDir | 0o700, uid: me, group: "staff", reach: true},
		{name: "open directory out of reach", mode: os.ModeDir | 0o777, uid: me, group: "staff"},
		{name: "open directory", mode: os.ModeDir | 0o777, uid: me, group: "staff", reach: true, write: true, bits: true, childReach: true, who: "group staff, others"},
		{name: "my link", mode: os.ModeSymlink | 0o777, uid: me, group: "staff", reach: true},
		{name: "another account's link", mode: os.ModeSymlink | 0o777, uid: 502, group: "staff", reach: true, write: true, owner: true, who: "owner 502"},
	} {
		info := posixEntry(c.mode, c.uid, c.group, me, "alice", c.reach)
		if info.exp.Write != c.write || info.badOwner != c.owner || info.badACL != c.bits || info.reach != c.childReach ||
			strings.Join(info.exp.Who, ", ") != c.who || info.doubt != "" ||
			info.link != (c.mode&os.ModeSymlink != 0) || info.dir != c.mode.IsDir() {
			t.Errorf("%s: %+v", c.name, info)
		}
	}
}

// The report keeps the first findings and counts the rest.
func TestTreeReportFindings(t *testing.T) {
	var r TreeReport
	for i := range maxFindings + 3 {
		r.find(fmt.Sprintf("/data/%d", i), []string{"others"})
	}
	r.doubt("first")
	r.doubt("second")
	if len(r.Findings) != maxFindings || r.More != 3 || r.Unconfirmed != "first" || r.OK() {
		t.Fatalf("%+v", r)
	}
	if p, who := r.Problem(); p != "/data/0" || who != "others" {
		t.Fatalf("Problem: %q %q", p, who)
	}
}

// A root that ends in a separator, as the path of a drive (E:\) or of a
// share (\\nas\share\) does: the entries below it are opened by clean
// paths, not by ones with a doubled separator (which the exact-name form
// refuses, so that the whole tree was "not confirmed": SEC17, 低-6). The
// walk is started here with such a path, no drive is touched.
func TestTreeRootEndingInSeparator(t *testing.T) {
	root := privateTree(t)
	sep := string(os.PathSeparator)
	for _, c := range []struct{ dir, name, want string }{
		{root, "updates", root + sep + "updates"},
		{root + sep, "updates", root + sep + "updates"},
		{sep, "data", sep + "data"},
		{"E:" + sep, "LectureFlow", "E:" + sep + "LectureFlow"},
		{sep + sep + "nas" + sep + "share" + sep, "x", sep + sep + "nas" + sep + "share" + sep + "x"},
	} {
		if got := childPath(c.dir, c.name); got != c.want {
			t.Errorf("childPath(%q, %q) = %q, want %q", c.dir, c.name, got, c.want)
		}
	}
	st, err := newTreeState(root)
	if err != nil {
		t.Fatal(err)
	}
	w := &treeWalk{st: st}
	w.visit(root+sep, "", 0, ScopeAll, st.rootReach)
	if !w.rep.OK() || w.rep.Entries != privateTreeEntries {
		t.Fatalf("a root ending in a separator: %+v", w.rep)
	}
	// and what it finds is named by a clean path
	w = &treeWalk{st: st, o: TreeOptions{MaxDepth: 1}}
	w.visit(root+sep, "", 0, ScopeAll, st.rootReach)
	if want := root + sep + "components" + sep + "whisper lies deeper than 1 levels"; w.rep.Unconfirmed != want {
		t.Fatalf("unconfirmed %q, want %q", w.rep.Unconfirmed, want)
	}
}

// How long a walk takes (go test -bench CheckTree ./internal/dirperm): the
// tree has 20 directories of 100 files each.
func BenchmarkCheckTree(b *testing.B) {
	root := filepath.Join(b.TempDir(), "data")
	if _, err := MkdirPrivate(root); err != nil {
		b.Fatal(err)
	}
	for d := range 20 {
		dir := filepath.Join(root, "components", fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := range 100 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.py", f)), []byte("x"), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for range b.N {
		rep, err := CheckTree(root, TreeOptions{})
		if err != nil || !rep.OK() || rep.Entries != 2022 {
			b.Fatalf("%+v %v", rep, err)
		}
	}
}
