// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Link chains in an archive (security review SEC13 low-3): each symbolic
// link's target is checked from the entry's own place, but links are made
// one after another, so a link whose path or target runs through another
// link is resolved somewhere else. With "a/b/d -> ../../q", the link
// "a/b/d/e -> ../../../up2" reads "up2" in the archive but lands in q/ and
// leads two levels above the destination; a third link below it is then
// made there. The unpacker must refuse such an archive before it makes any
// link, so these tests hold where links cannot be made (Windows without the
// privilege) as well as where they can.

// chainEntry is one archive entry: kind 'f' file, 'd' directory, 'l'
// symbolic link (link = target), 'h' hard link (link = the other entry).
type chainEntry struct {
	kind       byte
	name, link string
}

// chainBox is root/up1/up2/dst with a file "victim" in up1 and in up2: an
// escape of one to three levels lands in the box.
type chainBox struct{ root, dst string }

func newChainBox(t *testing.T) chainBox {
	t.Helper()
	root := t.TempDir()
	b := chainBox{root: root, dst: filepath.Join(root, "up1", "up2", "dst")}
	if err := os.MkdirAll(filepath.Join(root, "up1", "up2"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"up1/victim", "up1/up2/victim"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(v)), []byte("victim"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// assertNoEscape fails when anything outside dst was added, removed or
// changed, or when a symbolic link was made in dst.
func (b chainBox) assertNoEscape(t *testing.T) {
	t.Helper()
	known := map[string]bool{"up1": true, "up1/up2": true, "up1/victim": true, "up1/up2/victim": true}
	filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("walk %s: %v", p, err)
			return nil
		}
		rel, _ := filepath.Rel(b.root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
		case rel == "up1/up2/dst" || strings.HasPrefix(rel, "up1/up2/dst/"):
			if d.Type()&fs.ModeSymlink != 0 {
				t.Errorf("a link was made before the archive was refused: %s", rel)
			}
		case !known[rel]:
			t.Errorf("written outside the destination: %s", rel)
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

func chainTarGz(t *testing.T, entries []chainEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644}
		switch e.kind {
		case 'f':
			h.Typeflag, h.Size = tar.TypeReg, int64(len("data"))
		case 'd':
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case 'l':
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, e.link, 0o777
		case 'h':
			h.Typeflag, h.Linkname = tar.TypeLink, e.link
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == 'f' {
			tw.Write([]byte("data"))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// chainZip writes the entries as a zip; ok is false when the zip format
// cannot hold them (hard links).
func chainZip(t *testing.T, entries []chainEntry) (string, bool) {
	t.Helper()
	var zs []zipEntry
	for _, e := range entries {
		switch e.kind {
		case 'f':
			zs = append(zs, zipEntry{name: e.name, body: "data"})
		case 'd':
			zs = append(zs, zipEntry{name: strings.TrimSuffix(e.name, "/") + "/"})
		case 'l':
			zs = append(zs, zipEntry{name: e.name, body: e.link, mode: os.ModeSymlink | 0o777})
		default:
			return "", false
		}
	}
	p := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(p, makeModeZip(t, zs), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, true
}

func TestUnpackRefusesLinkChains(t *testing.T) {
	ctx := context.Background()
	cases := map[string][]chainEntry{
		// link -> link -> a third link made two levels above dst (over up2/victim)
		"link below a link": {
			{'d', "q/", ""},
			{'l', "a/b/d", "../../q"},
			{'l', "a/b/d/e", "../../../up2"},
			{'l', "a/b/d/e/victim", "zzz"},
		},
		"link below a link, backslashes": {
			{'d', `q\`, ""},
			{'l', `a\b\d`, `..\..\q`},
			{'l', `a\b\d\e`, `..\..\..\up2`},
			{'l', `a\b\d\e\victim`, "zzz"},
		},
		"link below a link, mixed separators": {
			{'d', "q/", ""},
			{'l', `a/b\d`, `../..\q`},
			{'l', `a\b/d/e`, `..\../..\up2`},
		},
		"link below a link of another case": {
			{'d', "q/", ""},
			{'l', "A/B/D", "../../q"},
			{'l', "a/b/d/e", "../../../up2"},
		},
		"file below a link": {
			{'d', "q/", ""},
			{'l', "a/d", "../q"},
			{'f', "a/d/f", ""},
		},
		"directory below a link": {
			{'d', "q/", ""},
			{'l', "a/d", "../q"},
			{'d', "a/d/sub/", ""},
		},
		"file below a link of another case": {
			{'d', "q/", ""},
			{'l', "a/D", "../q"},
			{'f', `A\d\f`, ""},
		},
		"link where a file was below it": {
			{'d', "q/", ""},
			{'f', "a/d/f", ""},
			{'l', "a/d", "../q"},
		},
		"link where a directory was": {
			{'d', "a/d/", ""},
			{'l', "a/D", "../q"},
		},
		// the target runs through a link and then up: "up2" in the archive,
		// up1/up2 once a/b/d leads to q
		"target through a link": {
			{'d', "q/", ""},
			{'l', "a/b/d", "../../q"},
			{'l', "r", "a/b/d/../../../up2"},
		},
		"target through a link, backslashes": {
			{'d', "q/", ""},
			{'l', `a\b\d`, `..\..\q`},
			{'l', "r", `a\b\d\..\..\..\up2`},
		},
		"target with .. after a name": {
			{'f', "x/f", ""},
			{'l', "r", "x/../x/f"},
		},
		"hard link to a link": {
			{'f', "f", ""},
			{'l', "s", "f"},
			{'h', "h", "s"},
		},
		"hard link to a link of another case": {
			{'f', "f", ""},
			{'l', "S", "f"},
			{'h', "h", "s"},
		},
		"hard link below a link": {
			{'f', "q/f", ""},
			{'l', "d", "q"},
			{'h', "h", "d/f"},
		},
		"hard link at a path below a link": {
			{'f', "f", ""},
			{'d', "q/", ""},
			{'l', "d", "q"},
			{'h', "d/h", "f"},
		},
		"link over a hard link's directory": {
			{'f', "q/f", ""},
			{'h', "h", "q/f"},
			{'l', "Q", "x"},
		},
	}
	for name, entries := range cases {
		for _, kind := range []string{"tar.gz", "zip"} {
			var archive string
			if kind == "zip" {
				p, ok := chainZip(t, entries)
				if !ok {
					continue // hard links exist in tar archives only
				}
				archive = p
			} else {
				archive = chainTarGz(t, entries)
			}
			t.Run(name+"/"+kind, func(t *testing.T) {
				box := newChainBox(t)
				if err := unpack(ctx, archive, kind, layout{}, box.dst); CodeOf(err) != CodeUnpackFailed {
					t.Errorf("err %v, want %s", err, CodeUnpackFailed)
				}
				box.assertNoEscape(t)
			})
		}
	}
}

// A hard link to a symbolic link entry or below one is refused by its own
// check, not only by the copy failing later because the link is not made
// yet: the error says so.
func TestUnpackHardLinkToALinkSaysWhy(t *testing.T) {
	ctx := context.Background()
	for name, entries := range map[string][]chainEntry{
		"to a link":                {{'f', "f", ""}, {'l', "s", "f"}, {'h', "h", "s"}},
		"to a link, another case":  {{'f', "f", ""}, {'l', "S", "f"}, {'h', "h", "s"}},
		"below a link":             {{'f', "q/f", ""}, {'l', "d", "q"}, {'h', "h", "d/f"}},
		"below a link, other case": {{'f', "q/f", ""}, {'l', "D", "q"}, {'h', "h", "d/f"}},
	} {
		t.Run(name, func(t *testing.T) {
			box := newChainBox(t)
			err := unpack(ctx, chainTarGz(t, entries), "tar.gz", layout{}, box.dst)
			if CodeOf(err) != CodeUnpackFailed {
				t.Errorf("err %v, want %s", err, CodeUnpackFailed)
			}
			if d := ErrorInfoOf(err).Detail; !strings.Contains(d, `unsafe hard link "h" ->`) {
				t.Errorf("detail %q", d)
			}
			box.assertNoEscape(t)
		})
	}
}

// Ordinary links still unpack: a chain of library names, a link to a
// directory, targets that go up first and then down.
func TestUnpackKeepsOrdinaryLinks(t *testing.T) {
	ctx := context.Background()
	entries := []chainEntry{
		{'f', "lib/libx.so.1.2", ""},
		{'l', "lib/libx.so.1", "libx.so.1.2"},
		{'l', "lib/libx.so", "libx.so.1"},
		{'l', "lib64", "lib"},
		{'l', "bin/x", "../lib/libx.so"},
		{'l', "bin/y", "./../lib/./libx.so.1.2"},
		{'l', "share/doc/README", "../../lib64/libx.so.1.2"},
	}
	for _, kind := range []string{"tar.gz", "zip"} {
		t.Run(kind, func(t *testing.T) {
			var archive string
			if kind == "zip" {
				archive, _ = chainZip(t, entries)
			} else {
				archive = chainTarGz(t, entries)
			}
			box := newChainBox(t)
			if err := unpack(ctx, archive, kind, layout{}, box.dst); err != nil {
				t.Fatal(err)
			}
			for _, rel := range []string{"lib/libx.so.1", "lib/libx.so", "bin/x", "bin/y"} {
				if b, err := os.ReadFile(filepath.Join(box.dst, filepath.FromSlash(rel))); err != nil || string(b) != "data" {
					t.Errorf("%s = %q %v", rel, b, err)
				}
			}
			if runtime.GOOS != "windows" {
				// a link through a directory link: copied only where links can be made
				if b, err := os.ReadFile(filepath.Join(box.dst, "share", "doc", "README")); err != nil || string(b) != "data" {
					t.Errorf("share/doc/README = %q %v", b, err)
				}
			}
		})
	}
}

func TestLinkTargetDotDotOnlyLeads(t *testing.T) {
	for target, ok := range map[string]bool{
		"f":           true,
		"../f":        true,
		"./../f":      true,
		`..\..\a\f`:   true,
		"a//b":        true,
		"a/./b":       true,
		"a/../b":      false,
		`a\..\b`:      false,
		"../a/../b":   false,
		"a/b/../../c": false,
	} {
		_, err := linkTarget("x/y/z/l", "l", target)
		if (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", target, err, ok)
		}
	}
}

// A link or junction already in the destination (left there, or named by
// a short 8.3 name or a trailing dot the archive check cannot see): no link
// is made through it.
func TestMakeLinksRefusesLinkedDirectory(t *testing.T) {
	ctx := context.Background()
	box := newChainBox(t)
	if err := os.MkdirAll(box.dst, 0o755); err != nil {
		t.Fatal(err)
	}
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
	archive := chainTarGz(t, []chainEntry{{'f', "other", ""}, {'l', "j/victim", "other"}})
	err := unpack(ctx, archive, "tar.gz", layout{}, box.dst)
	if CodeOf(err) != CodeUnpackFailed {
		t.Errorf("err %v, want %s", err, CodeUnpackFailed)
	}
	if d := ErrorInfoOf(err).Detail; !strings.Contains(d, `"j" on its way is a link, a junction or a file`) {
		t.Errorf("detail %q does not name the junction", d)
	}
	if err := os.Remove(j); err != nil {
		t.Fatal(err)
	}
	box.assertNoEscape(t)
}
