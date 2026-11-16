// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Link chains in the runtime archive (review R50 low-3; the rules of
// internal/components/unpack.go for SEC13 low-3, copied: the agent does not
// import the components package). Each symbolic link's target is checked
// from the entry's own place, which is where the link ends up only when no
// directory on the way is a link itself. So:
//   - an entry below a symbolic link entry is refused, and so is a link
//     where an entry used the path as a directory, whichever comes first;
//     names compared case-insensitively ("\" taken as "/"), as Windows and
//     macOS file systems do;
//   - a hard link to a symbolic link entry or below one is refused;
//   - ".." may only lead a link target (linkTargetOK);
//   - nothing is written where an existing directory on the way from dst is
//     a link or a junction (plainDirs);
//   - untarGz checks every entry before writing anything and makes the
//     symbolic links last.

var (
	errUnsafePath = errors.New("unsafe path in archive")
	errUnsafeLink = errors.New("unsafe link in archive")
)

// linkGuard collects an archive's links during the checking pass.
type linkGuard struct {
	link map[string]bool // keys of symbolic link entries
	dir  map[string]bool // keys of paths used as directories
}

func newLinkGuard() *linkGuard {
	return &linkGuard{link: map[string]bool{}, dir: map[string]bool{}}
}

// linkKey folds rel (relative to dst) for comparing names.
func linkKey(rel string) string {
	return strings.ToLower(path.Clean(strings.ReplaceAll(rel, `\`, "/")))
}

// entry records a file or directory at rel: refused below a link.
func (g *linkGuard) entry(rel string, isDir bool) error {
	d := linkKey(rel)
	if !isDir {
		d = path.Dir(d)
	}
	for ; d != "." && d != "/"; d = path.Dir(d) {
		if g.link[d] {
			return errUnsafePath
		}
		g.dir[d] = true
	}
	return nil
}

// symlink records a symbolic link at rel: refused below another link and
// where an entry used rel as a directory.
func (g *linkGuard) symlink(rel string) error {
	if err := g.entry(rel, false); err != nil {
		return errUnsafeLink
	}
	k := linkKey(rel)
	if g.dir[k] {
		return errUnsafeLink
	}
	g.link[k] = true
	return nil
}

// hardLink records a hard link at rel to src (relative to dst too):
// refused below a link, and to a link or a path below one.
func (g *linkGuard) hardLink(rel, src string) error {
	if err := g.entry(rel, false); err != nil {
		return err
	}
	if g.link[linkKey(src)] || g.entry(src, false) != nil {
		return errUnsafeLink
	}
	return nil
}

// linkTargetOK reports whether a symbolic link target is relative, names
// no drive or root and has ".." only at its start: after a name, ".." is
// resolved from wherever that name leads.
func linkTargetOK(t string) bool {
	if t == "" || filepath.IsAbs(t) || strings.HasPrefix(t, "/") || strings.HasPrefix(t, `\`) || strings.Contains(t, ":") {
		return false
	}
	named := false
	for _, c := range strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch c {
		case ".":
		case "..":
			if named {
				return false
			}
		default:
			named = true
		}
	}
	return true
}

// plainDirs checks the directories from dst to p's parent that exist (dst
// itself not included): each must be a plain directory. A symbolic link or
// a junction is not one on Lstat (Go 1.23 and later); MkdirAll makes the
// missing ones plain. plain caches the ones found.
func plainDirs(dst, p string, plain map[string]bool) error {
	rel, err := filepath.Rel(dst, filepath.Dir(p))
	if err != nil {
		return errUnsafePath
	}
	if rel == "." {
		return nil
	}
	cur := dst
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
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
			return fmt.Errorf("%w: %q on the way is a link, a junction or a file", errUnsafePath, c)
		}
		plain[cur] = true
	}
	return nil
}
