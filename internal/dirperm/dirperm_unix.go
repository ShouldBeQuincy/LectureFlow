// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package dirperm

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// mkdirPrivate creates dir with mode 0700 (the umask can only take bits
// away).
func mkdirPrivate(dir string) error { return os.Mkdir(dir, 0o700) }

// mkdirTempPrivate: os.MkdirTemp already creates 0700 directories.
func mkdirTempPrivate(parent, pattern string) (string, error) { return os.MkdirTemp(parent, pattern) }

func restrict(dir string) error { return os.Chmod(dir, 0o700) }

func save(dir string) (func() error, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	mode := fi.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetgid | os.ModeSetuid)
	return func() error { return os.Chmod(dir, mode) }, nil
}

// checkAbove: check (chainOf, exposureOf) already judges every directory
// from / down to the checked one, renaming included.
func checkAbove(string, string) (string, Exposure, error) { return "", Exposure{}, nil }

func finalPath(p string) (string, error) { return filepath.EvalSymlinks(p) }

// junction: there are no junctions here (a symbolic link is not one).
func junction(string) (JunctionInfo, bool, error) { return JunctionInfo{}, false, nil }

// node is one directory on the way from / to the checked one.
type node struct {
	path  string
	mode  os.FileMode
	uid   uint32
	group string // the group's name (or number)
}

func check(dir string) (Exposure, error) {
	chain, err := chainOf(dir)
	if err != nil {
		return Exposure{}, err
	}
	me, myName := self()
	return exposureOf(chain, me, myName), nil
}

// self is the user's id and name ("" when it cannot be looked up).
func self() (uint32, string) {
	me := uint32(os.Geteuid())
	if u, err := user.LookupId(strconv.Itoa(int(me))); err == nil {
		return me, u.Username
	}
	return me, ""
}

func nodeOf(p string, fi os.FileInfo) node {
	n := node{path: p, mode: fi.Mode()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		n.uid = st.Uid
		n.group = strconv.Itoa(int(st.Gid))
		if gr, err := user.LookupGroupId(n.group); err == nil {
			n.group = gr.Name
		}
	} else {
		n.uid = uint32(os.Geteuid())
	}
	return n
}

// chainOf is every directory from / down to dir (symbolic links resolved).
func chainOf(dir string) ([]node, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if real, err = filepath.Abs(real); err != nil {
		return nil, err
	}
	var chain []node
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		chain = append([]node{nodeOf(p, fi)}, chain...)
		if filepath.Dir(p) == p {
			break
		}
	}
	return chain, nil
}

// ---- trees (tree.go) ----

// treePath is the path a walk opens and lists an entry by (Windows needs
// another form than the one it shows).
func treePath(path string) string { return path }

// treeState is what a walk keeps between entries.
type treeState struct {
	me     uint32
	myName string
	groups map[uint32]string // gid → name
	// rootReach: other accounts can pass every directory above the root.
	rootReach bool
}

func newTreeState(root string) (*treeState, error) {
	st := &treeState{groups: map[uint32]string{}}
	st.me, st.myName = self()
	chain, err := chainOf(filepath.Dir(root))
	if err != nil {
		return nil, err
	}
	st.rootReach = true
	for _, n := range chain {
		if bits, _ := othersOf(n.mode, n.group, st.myName); bits&0o1 == 0 {
			st.rootReach = false
		}
	}
	return st, nil
}

func (st *treeState) group(gid uint32) string {
	if g, ok := st.groups[gid]; ok {
		return g
	}
	g := strconv.Itoa(int(gid))
	if gr, err := user.LookupGroupId(g); err == nil {
		g = gr.Name
	}
	st.groups[gid] = g
	return g
}

// inspectEntry judges the entry at path itself (a link is not followed).
func inspectEntry(st *treeState, path string, parentReach bool) (entryInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return entryInfo{}, err
	}
	uid, group := st.me, ""
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid, group = s.Uid, st.group(s.Gid)
	}
	return posixEntry(fi.Mode(), uid, group, st.me, st.myName, parentReach), nil
}

// repairEntry clears the group's and others' write bits of the entry at
// path (not of the root of the walk: Restrict sets its mode). An entry of
// another account cannot be taken over without root's rights.
func repairEntry(_ *treeState, path string, info entryInfo, root bool) error {
	if info.badOwner {
		return errors.New("it belongs to another account and cannot be taken over (remove it if it is not yours)")
	}
	if info.link || !info.badACL || root {
		return nil
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("it became a link")
	}
	keep := fi.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetgid | os.ModeSetuid)
	return os.Chmod(path, keep&^0o022)
}

// exposureOf walks from / down to the last node: another account reaches
// it only through directories it may traverse; on the way, a directory it
// may write without the sticky bit (or owns) lets it rename what comes
// next; the last one counts when its group or others may write (read) it,
// or another account owns it. A group named like the user (a "user private
// group") is not another account; root is not another account.
func exposureOf(chain []node, me uint32, myName string) Exposure {
	var e Exposure
	foreign := func(uid uint32) bool { return uid != me && uid != 0 }
	others := func(n node) (os.FileMode, []string) { return othersOf(n.mode, n.group, myName) }
	reach := true
	for i, n := range chain {
		bits, who := others(n)
		if i < len(chain)-1 {
			if reach && foreign(n.uid) {
				e.Write = true
				e.addWriter(fmt.Sprintf("owner %d of %s", n.uid, n.path))
			}
			if reach && bits&0o2 != 0 && n.mode&os.ModeSticky == 0 {
				e.Write = true
				e.addWriter(n.path)
			}
			if bits&0o1 == 0 {
				reach = false
			}
			continue
		}
		if !reach {
			break
		}
		if foreign(n.uid) {
			e.Write = true
			e.addWriter(fmt.Sprintf("owner %d", n.uid))
		}
		if bits&0o2 != 0 {
			e.Write = true
			for _, w := range who {
				e.addWriter(w)
			}
		}
		if bits&0o4 != 0 {
			e.Read = true
			for _, w := range who {
				e.add(w)
			}
		}
	}
	return e
}
