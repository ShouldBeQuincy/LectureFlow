// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// layout is how an archive's entries map to the destination: Strip
// (File.Strip) is a top-level directory whose contents land in the
// destination, an entry outside it is refused; Exclude (File.Exclude) are
// paths below the destination that are not unpacked.
type layout struct {
	Strip   string
	Exclude []string
}

func fileLayout(f File) layout { return layout{Strip: f.Strip, Exclude: f.Exclude} }

// excluded reports whether rel (relative to the destination) is left out:
// an Exclude entry ending in "/" is a directory, else one file.
func (l layout) excluded(rel string) bool {
	for _, e := range l.Exclude {
		if dir, ok := strings.CutSuffix(e, "/"); ok {
			if rel == dir || strings.HasPrefix(rel, e) {
				return true
			}
		} else if rel == e {
			return true
		}
	}
	return false
}

// unpack extracts a tar.gz or zip archive into dst. Entries that would land
// outside dst (absolute paths, "..", links pointing out) are refused; see
// layout for strip and exclude.
func unpack(ctx context.Context, archive, kind string, lay layout, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return asCoded(err, nil)
	}
	var err error
	switch kind {
	case "tar.gz":
		err = untarGz(ctx, archive, lay, dst)
	case "zip":
		err = unzipAll(ctx, archive, lay, dst)
	default:
		err = fmt.Errorf("unknown archive type %q", kind)
	}
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		if CodeOf(err) != "" {
			return err
		}
		if isDiskFull(err) {
			return cerr(CodeDiskFull, map[string]any{"required_mb": -1, "free_mb": -1}, err)
		}
		return cerr(CodeUnpackFailed, map[string]any{"file": filepath.Base(archive)}, err)
	}
	return nil
}

// entryPath maps an archive entry name to a path under dst.
func entryPath(dst, name string) (string, error) {
	p, _, err := entryRel(dst, name, layout{})
	return p, err
}

// entryRel maps an archive entry name to a path under dst and returns its
// "/"-separated path relative to dst too; with Strip, the name must be
// inside that top-level directory, which is dropped. "" = nothing to write
// (the root, the strip directory itself, an excluded path).
func entryRel(dst, name string, lay layout) (string, string, error) {
	strip := lay.Strip
	name = strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean("/" + name)
	if clean == "/" {
		return "", "", nil
	}
	rel := strings.TrimPrefix(clean, "/")
	if !safeRelPath(rel) || strings.HasPrefix(name, "/") || strings.Contains(name, ":") {
		return "", "", fmt.Errorf("unsafe archive entry %q", name)
	}
	if strip != "" {
		if rel == strip {
			return "", "", nil
		}
		r, ok := strings.CutPrefix(rel, strip+"/")
		if !ok {
			return "", "", fmt.Errorf("archive entry %q is outside %s/", name, strip)
		}
		rel = r
	}
	if lay.excluded(rel) {
		return "", "", nil
	}
	return filepath.Join(dst, filepath.FromSlash(rel)), rel, nil
}

type deferredLink struct{ target, at, rel string }

// linkTarget checks a symbolic link's target: relative, and inside the
// destination when resolved from the entry's place (rel: the entry's path
// relative to the destination, after strip). ".." may only lead the target:
// after a name ("d/../x") it is resolved from wherever that name leads,
// another link's target when the name is a link (security review SEC13
// low-3).
func linkTarget(rel, entry, linkname string) (string, error) {
	linkname = strings.ReplaceAll(linkname, `\`, "/")
	target := path.Join(path.Dir(rel), linkname)
	if linkname == "" || strings.HasPrefix(linkname, "/") || strings.Contains(linkname, ":") ||
		target == ".." || strings.HasPrefix(target, "../") || target == "." {
		return "", fmt.Errorf("unsafe link %q -> %q", entry, linkname)
	}
	named := false
	for _, c := range strings.Split(linkname, "/") {
		switch c {
		case "", ".":
		case "..":
			if named {
				return "", fmt.Errorf("unsafe link %q -> %q", entry, linkname)
			}
		default:
			named = true
		}
	}
	return filepath.FromSlash(linkname), nil
}

// linkSet collects an archive's symbolic links, which are made after all
// its files, and refuses link chains (security review SEC13 low-3): an
// entry below a symbolic link entry, whichever comes first in the archive,
// would be written wherever that link leads, and a link there would be
// resolved from that place rather than from the one linkTarget checked.
// Names are compared case-insensitively, as Windows and macOS file systems
// do; makeLinks checks the directories on disk as well.
type linkSet struct {
	links []deferredLink
	link  map[string]string // folded rel -> rel of each symbolic link entry
	dir   map[string]bool   // folded rels used as directories
}

func newLinkSet() *linkSet {
	return &linkSet{link: map[string]string{}, dir: map[string]bool{}}
}

// entry records a file or a directory at rel: refused below a link.
func (s *linkSet) entry(name, rel string, isDir bool) error {
	d := rel
	if !isDir {
		d = path.Dir(rel)
	}
	for ; d != "."; d = path.Dir(d) {
		k := strings.ToLower(d)
		if l, ok := s.link[k]; ok {
			return fmt.Errorf("unsafe archive entry %q: below the link %q", name, l)
		}
		s.dir[k] = true
	}
	return nil
}

// hardLink records a hard link at rel to src (relative to the destination
// too): refused below a link, and to a link or a path below one. Within
// one archive the last two would also fail later, as no symbolic link
// exists yet when a hard link is made; these checks say why, and do not
// depend on links being made last.
func (s *linkSet) hardLink(name, rel, src string) error {
	if err := s.entry(name, rel, false); err != nil {
		return err
	}
	if _, ok := s.link[strings.ToLower(src)]; ok {
		return fmt.Errorf("unsafe hard link %q -> %q", name, src)
	}
	if err := s.entry(name, src, false); err != nil {
		return fmt.Errorf("unsafe hard link %q -> %q", name, src)
	}
	return nil
}

// symlink records a symbolic link at rel (target checked by linkTarget, at
// its place in dst): refused below another link, and where an entry used
// rel as a directory.
func (s *linkSet) symlink(name, rel, target, at string) error {
	if err := s.entry(name, rel, false); err != nil {
		return err
	}
	k := strings.ToLower(rel)
	if s.dir[k] {
		return fmt.Errorf("unsafe link %q: the archive has entries below it", name)
	}
	s.link[k] = rel
	s.links = append(s.links, deferredLink{target: target, at: at, rel: rel})
	return nil
}

func untarGz(ctx context.Context, archive string, lay layout, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	links := newLinkSet()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p, rel, err := entryRel(dst, h.Name, lay)
		if err != nil {
			return err
		}
		if p == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := links.entry(h.Name, rel, true); err != nil {
				return err
			}
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := links.entry(h.Name, rel, false); err != nil {
				return err
			}
			if err := writeEntry(p, tr, h.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// the target must stay inside dst
			target, err := linkTarget(rel, h.Name, h.Linkname)
			if err != nil {
				return err
			}
			if err := links.symlink(h.Name, rel, target, p); err != nil {
				return err
			}
		case tar.TypeLink:
			src, srcRel, err := entryRel(dst, h.Linkname, layout{Strip: lay.Strip})
			if err != nil || src == "" {
				return fmt.Errorf("unsafe hard link %q -> %q", h.Name, h.Linkname)
			}
			if lay.excluded(srcRel) {
				continue // its file was left out
			}
			if err := links.hardLink(h.Name, rel, srcRel); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := linkOrCopy(src, p); err != nil {
				return err
			}
		default:
			// devices, fifos: not part of a runtime archive
		}
	}
	return makeLinks(dst, links.links)
}

// makeLinks creates symbolic links after the files exist; where the system
// refuses symlinks (Windows without the privilege) the target file is
// copied, a link to a directory or to nothing is left out. A link is made
// only where the directories on the way from dst are plain directories: one
// that is a link or a junction (a link entry under its short 8.3 name or
// with a trailing dot, something left in dst) would put it elsewhere.
func makeLinks(dst string, links []deferredLink) error {
	plain := map[string]bool{}
	for _, l := range links {
		if err := plainDirs(dst, l.rel, plain); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(l.at), 0o755); err != nil {
			return err
		}
		os.Remove(l.at)
		if err := os.Symlink(l.target, l.at); err == nil {
			continue
		} else if runtime.GOOS != "windows" {
			return err
		}
		src := filepath.Join(filepath.Dir(l.at), l.target)
		fi, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a dangling link: nothing can use it
		}
		if err != nil {
			return fmt.Errorf("link target %s: %w", l.target, err)
		}
		if fi.IsDir() {
			continue // directory links are not needed by a runtime or tool on Windows
		}
		if err := linkOrCopy(src, l.at); err != nil {
			return err
		}
	}
	return nil
}

// plainDirs checks the directories from dst to rel's parent that exist (dst
// itself not included): each must be a directory, not a symbolic link or a
// junction (since Go 1.23 neither reports as a directory on Lstat; a cloud
// files directory still does); MkdirAll makes the missing ones plain. plain
// caches the ones found.
//
// Only links are checked, not the files and directories written before
// them, which assumes dst holds no link or junction an entry could be
// written through. installPython and installTool empty dst first
// (removeAllRetry). installFiles does not: staging/<sub> is kept across a
// pause and shared by the archives of one build. It then holds only what
// earlier archives of this install unpacked through these same checks
// (unpacking never makes a junction), so each link in it resolves inside
// dst, a file written through one stays inside, and a link made through
// one is refused here.
func plainDirs(dst, rel string, plain map[string]bool) error {
	cur := dst
	for _, c := range strings.Split(path.Dir(rel), "/") {
		if c == "." {
			break
		}
		cur = filepath.Join(cur, c)
		if plain[cur] {
			continue
		}
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("unsafe link %q: %q on its way is a link, a junction or a file, not a directory", rel, c)
		}
		plain[cur] = true
	}
	return nil
}

func writeEntry(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// unzipAll extracts a zip archive. Symbolic links (Unix mode in the
// external attributes; the entry's data is the target) are checked like the
// ones of a tar archive and made after the files.
func unzipAll(ctx context.Context, archive string, lay layout, dst string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	links := newLinkSet()
	for _, zf := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, rel, err := entryRel(dst, zf.Name, lay)
		if err != nil {
			return err
		}
		if p == "" {
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			target, err := readZipLink(zf)
			if err != nil {
				return err
			}
			t, err := linkTarget(rel, zf.Name, target)
			if err != nil {
				return err
			}
			if err := links.symlink(zf.Name, rel, t, p); err != nil {
				return err
			}
			continue
		}
		if err := links.entry(zf.Name, rel, zf.FileInfo().IsDir()); err != nil {
			return err
		}
		if err := writeZipEntry(zf, p); err != nil {
			return err
		}
	}
	return makeLinks(dst, links.links)
}

// readZipLink reads the target of a symbolic link entry.
func readZipLink(zf *zip.File) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("link %q: target too long", zf.Name)
	}
	return string(b), nil
}

// extractZipEntry writes one zip entry to dst/rel (rel "" = the entry name).
func extractZipEntry(zf *zip.File, dst, name string) error {
	p, err := entryPath(dst, name)
	if err != nil {
		return err
	}
	if p == "" {
		return nil
	}
	if zf.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not supported in zip archives")
	}
	return writeZipEntry(zf, p)
}

// writeZipEntry writes a directory or file entry to p.
func writeZipEntry(zf *zip.File, p string) error {
	if zf.FileInfo().IsDir() {
		return os.MkdirAll(p, 0o755)
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	return writeEntry(p, rc, zf.Mode())
}
