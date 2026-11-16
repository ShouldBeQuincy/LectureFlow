// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package procutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// exeDirChildEnv set to a file: the test binary writes its working
// directory to that file and exits (init runs before TestMain).
const exeDirChildEnv = "PROCUTIL_TEST_EXEDIR_OUT"

func init() {
	out := os.Getenv(exeDirChildEnv)
	if out == "" {
		return
	}
	wd, err := os.Getwd()
	if err != nil || os.WriteFile(out, []byte(wd), 0o644) != nil {
		os.Exit(8)
	}
	os.Exit(0)
}

// samePath compares two paths as the file system does.
func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SEC52 info-7: CommandInExeDir runs a helper program (nvidia-smi, the
// hardware probe's PowerShell) in its own directory, not in this process's
// current directory, which in web mode is the folder LectureFlow was
// started from and which Windows searches for a DLL loaded by name. The
// executable is resolved first: a bare name in PATH, a relative path
// against the current directory (the file it named before, not one below
// the new working directory), an absolute path as it is. One that cannot
// be resolved is left as exec makes it and fails as before.
func TestCommandInExeDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	args := []string{"--query-gpu=name", "--format=csv,noheader"}
	for _, c := range []struct {
		name string
		arg  func(t *testing.T) string
	}{
		{"absolute path", func(t *testing.T) string {
			t.Chdir(t.TempDir())
			return exe
		}},
		{"bare name in PATH", func(t *testing.T) string {
			t.Setenv("PATH", dir)
			t.Chdir(t.TempDir()) // the service's current directory: elsewhere
			return filepath.Base(exe)
		}},
		{"relative path", func(t *testing.T) string {
			t.Chdir(filepath.Dir(dir))
			return filepath.Join(filepath.Base(dir), filepath.Base(exe))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			arg := c.arg(t)
			cmd := CommandInExeDir(context.Background(), arg, args...)
			if cmd.Err != nil || !filepath.IsAbs(cmd.Path) || !samePath(cmd.Path, exe) || !samePath(cmd.Dir, dir) {
				t.Fatalf("%q: path %q, dir %q, err %v; want %q in %q", arg, cmd.Path, cmd.Dir, cmd.Err, exe, dir)
			}
			if !slices.Equal(cmd.Args[1:], args) {
				t.Fatalf("args %q", cmd.Args)
			}
			out := filepath.Join(t.TempDir(), "cwd.txt")
			cmd.Env = append(os.Environ(), exeDirChildEnv+"="+out)
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !samePath(string(b), dir) {
				wd, _ := os.Getwd()
				t.Fatalf("ran in %s, want %s (the current directory is %s)", b, dir, wd)
			}
		})
	}

	// Not found: no directory, the same error as before.
	t.Run("not found", func(t *testing.T) {
		cmd := CommandInExeDir(context.Background(), "no-such-program-lectureflow-r10", args...)
		if cmd.Dir != "" || !errors.Is(cmd.Err, exec.ErrNotFound) {
			t.Fatalf("bare name: dir %q, err %v", cmd.Dir, cmd.Err)
		}
		missing := filepath.Join(t.TempDir(), "missing", "nvidia-smi.exe")
		cmd = CommandInExeDir(context.Background(), missing, args...)
		if cmd.Dir != "" {
			t.Fatalf("missing path: dir %q", cmd.Dir)
		}
		got := cmd.Run()
		want := exec.CommandContext(context.Background(), missing, args...).Run()
		if got == nil || want == nil || got.Error() != want.Error() {
			t.Fatalf("missing path: error %v, want %v", got, want)
		}
	})
}

// copyExe copies the test binary to dst (executable).
func copyExe(t *testing.T, dst string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// SEC53: a program of the same name in the current directory never runs.
// Windows' look-up finds it there too, but exec.LookPath answers
// exec.ErrDot for it (also when PATH names a different file of that name),
// and for a file found through a relative PATH entry on every system; the
// helper keeps that error, sets no directory, and Start refuses to run the
// file. Elsewhere the current directory is not searched at all.
func TestCommandInExeDirCurrentDirProgram(t *testing.T) {
	// When it exists, Windows' look-up skips the current directory; the
	// cases below are about the default.
	const noDot = "NoDefaultCurrentDirectoryInExePath"
	t.Setenv(noDot, "")
	if err := os.Unsetenv(noDot); err != nil {
		t.Fatal(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	const name = "lf-r10-same-name"
	cwd := t.TempDir()
	copyExe(t, filepath.Join(cwd, name+suffix))
	t.Chdir(cwd)
	// run runs cmd as a child that records its working directory: ran
	// reports whether it started at all, wd where.
	run := func(t *testing.T, cmd *exec.Cmd) (ran bool, wd string, err error) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "cwd.txt")
		cmd.Env = append(os.Environ(), exeDirChildEnv+"="+out)
		err = cmd.Run()
		b, rerr := os.ReadFile(out)
		return rerr == nil, string(b), err
	}
	refused := func(t *testing.T, cmd *exec.Cmd, want error) {
		t.Helper()
		if !errors.Is(cmd.Err, want) || cmd.Dir != "" {
			t.Fatalf("path %q, dir %q, err %v; want %v and no directory", cmd.Path, cmd.Dir, cmd.Err, want)
		}
		if ran, _, err := run(t, cmd); ran || !errors.Is(err, want) {
			t.Fatalf("ran %v, error %v; want not run, %v", ran, err, want)
		}
	}

	t.Run("only in the current directory", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		want := exec.ErrDot
		if runtime.GOOS != "windows" {
			want = exec.ErrNotFound
		}
		refused(t, CommandInExeDir(context.Background(), name), want)
	})
	t.Run("a different one in PATH", func(t *testing.T) {
		pdir := t.TempDir()
		copyExe(t, filepath.Join(pdir, name+suffix))
		t.Setenv("PATH", pdir)
		cmd := CommandInExeDir(context.Background(), name)
		if runtime.GOOS == "windows" {
			refused(t, cmd, exec.ErrDot)
			return
		}
		// not searched: PATH's runs, in its own directory
		if cmd.Err != nil || !samePath(cmd.Dir, pdir) {
			t.Fatalf("path %q, dir %q, err %v; want %s", cmd.Path, cmd.Dir, cmd.Err, pdir)
		}
		if ran, wd, err := run(t, cmd); !ran || err != nil || !samePath(wd, pdir) {
			t.Fatalf("ran %v in %q, error %v; want %s", ran, wd, err, pdir)
		}
	})
	t.Run("a relative PATH entry", func(t *testing.T) {
		const other = "lf-r10-other-name"
		if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		copyExe(t, filepath.Join(cwd, "sub", other+suffix))
		t.Setenv("PATH", "sub")
		refused(t, CommandInExeDir(context.Background(), other), exec.ErrDot)
	})
}
