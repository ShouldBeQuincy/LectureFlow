// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

import (
	"archive/tar"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Link chains in the runtime archive (review R50 low-3, the rules of
// components/unpack.go for SEC13 low-3): with "a/b/d -> ../../q", the link
// "a/b/d/e -> ../../.." reads as dst but lands in q/ and leads two levels
// above it, and a file "a/b/d/e/evil" would be written there. untarGz must
// refuse such an archive before it writes anything, so these tests hold
// where links cannot be made (Windows without the privilege) too: the
// error must be the unpacker's own ("unsafe ..."), not the system refusing
// a link, and dst must still be empty.

func hDir(n string) tar.Header { return tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: 0o755} }
func hFile(n string) tar.Header {
	return tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644}
}
func hSym(n, t string) tar.Header {
	return tar.Header{Name: n, Typeflag: tar.TypeSymlink, Linkname: t, Mode: 0o777}
}
func hHard(n, t string) tar.Header { return tar.Header{Name: n, Typeflag: tar.TypeLink, Linkname: t} }

// untarBox is root/up1/up2/dst (empty) with a file "victim" in up1 and up2.
type untarBox struct{ root, dst string }

func newUntarBox(t *testing.T) untarBox {
	t.Helper()
	root := t.TempDir()
	b := untarBox{root: root, dst: filepath.Join(root, "up1", "up2", "dst")}
	if err := os.MkdirAll(b.dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"up1/victim", "up1/up2/victim"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(v)), []byte("victim"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// assertUnchanged fails when anything in the box changed: dst must be
// empty, nothing added outside it, the victims intact.
func (b untarBox) assertUnchanged(t *testing.T) {
	t.Helper()
	known := map[string]bool{"up1": true, "up1/up2": true, "up1/victim": true, "up1/up2/victim": true, "up1/up2/dst": true}
	filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		rel, _ := filepath.Rel(b.root, p)
		if rel = filepath.ToSlash(rel); rel != "." && !known[rel] {
			t.Errorf("written while refusing the archive: %s", rel)
		}
		return nil
	})
	for _, v := range []string{"up1/victim", "up1/up2/victim"} {
		p := filepath.Join(b.root, filepath.FromSlash(v))
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s was removed or replaced: %v %v", v, fi, err)
			continue
		}
		if got, _ := os.ReadFile(p); string(got) != "victim" {
			t.Errorf("%s = %q", v, got)
		}
	}
}

func (b untarBox) untar(t *testing.T, entries ...tar.Header) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "python.tar.gz")
	if err := os.WriteFile(p, tarGz(t, entries...), 0o600); err != nil {
		t.Fatal(err)
	}
	return untarGz(p, b.dst)
}

func TestUntarRefusesLinkChains(t *testing.T) {
	for name, entries := range map[string][]tar.Header{
		// R50: a file written two levels above dst
		"file through a chain":             {hDir("q/"), hSym("a/b/d", "../../q"), hSym("a/b/d/e", "../../.."), hFile("a/b/d/e/evil")},
		"file over a victim":               {hDir("q/"), hSym("a/b/d", "../../q"), hSym("a/b/d/e", "../../../up2"), hFile("a/b/d/e/victim")},
		"link below a link":                {hDir("q/"), hSym("a/b/d", "../../q"), hSym("a/b/d/e", "../../../up2")},
		"backslashes":                      {hDir(`q\`), hSym(`a\b\d`, `..\..\q`), hSym(`a\b\d\e`, `..\..\..\up2`)},
		"another case":                     {hDir("q/"), hSym("A/B/D", "../../q"), hFile("a/b/d/f")},
		"file below a link":                {hDir("q/"), hSym("a/d", "../q"), hFile("a/d/f")},
		"dir below a link":                 {hDir("q/"), hSym("a/d", "../q"), hDir("a/d/sub/")},
		"link over a file's directory":     {hFile("a/d/f"), hSym("a/d", "../q")},
		"link over a directory":            {hDir("a/d/"), hSym("a/D", "../q")},
		"target through a link":            {hDir("q/"), hSym("a/b/d", "../../q"), hSym("r", "a/b/d/../../../up2")},
		"target with .. after a name":      {hFile("x/f"), hSym("r", "x/../x/f")},
		"target from the drive root":       {hFile("f"), hSym("r", `\f`)},
		"hard link to a link":              {hFile("f"), hSym("s", "f"), hHard("h", "s")},
		"hard link below a link":           {hFile("q/f"), hSym("d", "q"), hHard("h", "d/f")},
		"hard link at a path below a link": {hFile("f"), hDir("q/"), hSym("d", "q"), hHard("d/h", "f")},
	} {
		t.Run(name, func(t *testing.T) {
			box := newUntarBox(t)
			if err := box.untar(t, entries...); err == nil || !strings.Contains(err.Error(), "unsafe") {
				t.Errorf("err %v, want the archive refused as unsafe", err)
			}
			box.assertUnchanged(t)
		})
	}
}

// Ordinary links still unpack (where the system makes links at all).
func TestUntarKeepsOrdinaryLinks(t *testing.T) {
	box := newUntarBox(t)
	err := box.untar(t,
		hFile("lib/libx.so.1.2"),
		hSym("lib/libx.so.1", "libx.so.1.2"),
		hSym("lib/libx.so", "libx.so.1"),
		hSym("lib64", "lib"),
		hSym("bin/x", "../lib/libx.so"),
		hSym("bin/y", "./../lib/./libx.so.1.2"),
		hSym("share/doc/README", "../../lib64/libx.so.1.2"),
		hHard("lib/copy", "lib/libx.so.1.2"),
	)
	if err != nil {
		if runtime.GOOS != "windows" || strings.Contains(err.Error(), "unsafe") {
			t.Fatal(err)
		}
		return // Windows without the privilege: the system refused a link
	}
	for _, rel := range []string{"lib/copy", "lib/libx.so", "bin/x", "bin/y", "share/doc/README"} {
		if b, err := os.ReadFile(filepath.Join(box.dst, filepath.FromSlash(rel))); err != nil || string(b) != "data" {
			t.Errorf("%s = %q %v", rel, b, err)
		}
	}
}

// A link or junction already in dst: nothing is written or linked through it.
func TestUntarRefusesLinkedDirectory(t *testing.T) {
	for name, entries := range map[string][]tar.Header{
		"file": {hFile("other"), hFile("j/victim")},
		"link": {hFile("other"), hSym("j/x", "../other")},
		"dir":  {hDir("j/sub/")},
		"hard": {hFile("other"), hHard("j/victim", "other")},
	} {
		t.Run(name, func(t *testing.T) {
			box := newUntarBox(t)
			j := filepath.Join(box.dst, "j")
			up2 := filepath.Join(box.root, "up1", "up2")
			if runtime.GOOS == "windows" {
				// a junction needs no privilege
				if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, up2).CombinedOutput(); err != nil {
					t.Fatalf("mklink /J: %v %s", err, out)
				}
			} else if err := os.Symlink(up2, j); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Remove(j) }) // the junction itself, never what it leads to
			err := box.untar(t, entries...)
			if err == nil || !strings.Contains(err.Error(), "unsafe") {
				t.Errorf("err %v, want the archive refused as unsafe", err)
			}
			if err := os.Remove(j); err != nil {
				t.Fatal(err)
			}
			// "other" may be written before the refusal: inside dst, harmless
			os.Remove(filepath.Join(box.dst, "other"))
			box.assertUnchanged(t)
		})
	}
}
