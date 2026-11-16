// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// gitRepo makes a git repository in a temporary directory with the given
// files, adds them all (with -f, so that ignored ones are tracked as well,
// like the process files of the development history) and returns its root.
// The user's and the system's git configuration are kept out, so that a
// global excludes file cannot change the result (isolateGit).
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("no git on PATH: licaudit lists the files with git ls-files")
	}
	isolateGit(t)
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-f", "--", ".")
	return root
}

// isolateGit keeps the developer's git configuration out of this test's
// git runs: no global or system config (GIT_CONFIG_GLOBAL is an empty file,
// GIT_CONFIG_NOSYSTEM), and no default global excludes file either, which
// git reads without any config from $XDG_CONFIG_HOME/git/ignore or
// ~/.config/git/ignore (SEC52 info-8): XDG_CONFIG_HOME and HOME point to
// an empty directory.
func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	empty := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("HOME", home)
}

// T27 P1: the process files that the history tracks but .gitignore excludes
// (the ones make-release.sh leaves out of the published tree) are not
// checked; every other tracked source still is.
func TestTrackedFilesLeaveOutIgnoredProcessFiles(t *testing.T) {
	hdr := "// " + spdxTag + "\n"
	root := gitRepo(t, map[string]string{
		".gitignore":     "/proc/\n",
		"proc/x.sh":      "#!/bin/sh\necho process file\n",
		"a.go":           "package a\n",
		"b.go":           hdr + "package b\n",
		"worker/w.py":    "# " + spdxTag + "\n",
		"keep/proc/y.sh": "#!/bin/sh\n", // not under the anchored /proc/
	})
	files, err := gitTrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "a.go", "b.go", "keep/proc/y.sh", "worker/w.py"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	missing, err := CheckSPDX(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "keep/proc/y.sh"}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	noNotice, err := CheckExceptionNotice(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"worker/w.py"}; !reflect.DeepEqual(noNotice, want) {
		t.Fatalf("worker sources without the notice = %v, want %v", noNotice, want)
	}

	// The command reports the published source without the header and
	// nothing under proc/.
	var out bytes.Buffer
	code := auditMain([]string{"-root", root, "-check", "-only", "spdx"}, &out, &bytes.Buffer{}, nil, gitTrackedFiles)
	if code != 1 || !strings.Contains(out.String(), "FAIL a.go") || strings.Contains(out.String(), "proc/x.sh") {
		t.Fatalf("exit %d, output %s", code, out.String())
	}
}

// Without the ignore rule both files are published, so both are reported.
func TestTrackedFilesWithoutGitignoreKeepEverything(t *testing.T) {
	root := gitRepo(t, map[string]string{
		"proc/x.sh": "#!/bin/sh\necho process file\n",
		"a.go":      "package a\n",
	})
	files, err := gitTrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "proc/x.sh"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	missing, err := CheckSPDX(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "proc/x.sh"}; !reflect.DeepEqual(missing, want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
}

// A developer's own global excludes file (here a made-up home whose
// XDG and ~/.config git/ignore list *.go) does not reach these tests.
func TestTrackedFilesTestsIgnoreDevelopersGlobalExcludes(t *testing.T) {
	dev := t.TempDir()
	for _, d := range []string{filepath.Join(dev, "xdg", "git"), filepath.Join(dev, ".config", "git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "ignore"), []byte("*.go\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", dev)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dev, "xdg"))
	root := gitRepo(t, map[string]string{"a.go": "package a\n"})
	files, err := gitTrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, []string{"a.go"}) {
		t.Fatalf("files = %v: the developer's global excludes reached the test", files)
	}
}

// Outside a git work tree the listing fails and the audit exits with
// status 2, as before.
func TestTrackedFilesOutsideRepositoryFail(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	// Stop git from finding a repository above the temporary directory.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if _, err := gitTrackedFiles(dir); err == nil {
		t.Fatal("listing outside a repository succeeded")
	}
	var errOut bytes.Buffer
	if code := auditMain([]string{"-root", dir, "-check", "-only", "spdx"}, &bytes.Buffer{}, &errOut, nil, gitTrackedFiles); code != 2 ||
		!strings.Contains(errOut.String(), "listing tracked files") {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
}
