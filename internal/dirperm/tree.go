// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// What lies below a directory (security audit M-3). Check and Restrict look
// at one directory; what was created below it while other accounts could
// write there keeps its own owner and its own entries: a folder another
// account made stays theirs (an owner may always change the permissions),
// and an entry that names another account outright is not replaced when the
// parent's inheritable entries change. CheckTree looks at every entry of a
// tree, RestrictTree resets the ones CheckTree would report.
//
// The walk is bounded (TreeOptions) and never follows a link:
//
//   - Windows: every entry is opened by its exact name (\\?\ paths: a name
//     ending in a dot or a space is that entry, not another one) without
//     following reparse points; its owner and DACL are read from that
//     handle and judged like Check judges a directory (an owner that is
//     not the user, SYSTEM, the administrators or TrustedInstaller; an
//     entry of any kind but the deny kinds, inherited or not, that gives
//     another account a write right; no DACL at all). A reparse point
//     (symbolic link, junction, any other) is never entered and makes the
//     tree unconfirmed even when nobody else may change the link itself: an
//     account with write access can turn an empty folder of the user into a
//     junction without becoming its owner, so the link proves nothing about
//     where it leads. LectureFlow creates none below its data directory on
//     Windows.
//   - macOS, Linux: an entry owned by another account (not root) is
//     reported, wherever it lies; group and others' write bits count where
//     those accounts can get to the entry (every directory on the way lets
//     them pass), as in Check. A symbolic link is judged by its own owner
//     (only its owner can have made it) and not followed.
//
// A tree the walk cannot vouch for as a whole — more entries or levels than
// the bounds, an entry whose permissions cannot be read, a link (Windows),
// a root that is itself a link — is "unconfirmed": never OK.
//
// What this cannot see: a file of the user that another account changed in
// place while it could (the file is still the user's), and entries of the
// user moved around inside the tree. RestrictTree makes the entries the
// user's; it does not vouch for what they contain.

// Bounds of CheckTree and RestrictTree when TreeOptions leaves them zero.
const (
	DefaultMaxEntries = 200000
	DefaultMaxDepth   = 64
)

// maxFindings bounds TreeReport.Findings (and RestrictTree's lists).
const maxFindings = 8

// NotConfirmed starts what TreeReport.Problem says in the place of the
// accounts when the walk could not vouch for the whole tree.
const NotConfirmed = "not confirmed: "

// Scope is what TreeOptions.Scope says about one entry.
type Scope int

const (
	// ScopeAll: the entry is checked and, when it is a directory, what it
	// holds as well.
	ScopeAll Scope = iota
	// ScopeSelf: the entry is checked; what a directory holds is not.
	ScopeSelf
	// ScopeSkip: the entry is not looked at.
	ScopeSkip
)

// TreeOptions bound and narrow a walk.
type TreeOptions struct {
	// MaxEntries is how many entries are looked at (0 = DefaultMaxEntries);
	// a tree with more is unconfirmed.
	MaxEntries int
	// MaxDepth is how many levels below root are entered (0 =
	// DefaultMaxDepth); a deeper tree is unconfirmed.
	MaxDepth int
	// Scope, when set, is asked for every entry (rel: its "/"-separated
	// path below root; "" is root itself, which is always checked: only
	// ScopeSelf matters for it). nil = everything.
	Scope func(rel string) Scope
}

func (o TreeOptions) maxEntries() int {
	if o.MaxEntries > 0 {
		return o.MaxEntries
	}
	return DefaultMaxEntries
}

func (o TreeOptions) maxDepth() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return DefaultMaxDepth
}

// TreeFinding is one entry another account may change.
type TreeFinding struct {
	Path string   // absolute
	Who  []string // as Exposure.Who
}

// TreeReport is what a walk found.
type TreeReport struct {
	// Entries is how many entries were looked at.
	Entries int
	// Findings are the first entries other accounts may change (at most a
	// few); More counts the rest.
	Findings []TreeFinding
	More     int
	// Unconfirmed is why the walk cannot vouch for the whole tree ("" when
	// it can): short English naming the path.
	Unconfirmed string
}

// OK reports whether every entry was looked at and none lets another
// account change it.
func (r TreeReport) OK() bool {
	return len(r.Findings) == 0 && r.More == 0 && r.Unconfirmed == ""
}

// Problem describes the first thing that keeps the tree from being OK: the
// path it is about and who (the accounts, or why it is unconfirmed). Both
// are "" for an OK report.
func (r TreeReport) Problem() (path, who string) {
	if len(r.Findings) > 0 {
		f := r.Findings[0]
		return f.Path, strings.Join(f.Who, ", ")
	}
	if r.Unconfirmed != "" {
		return "", NotConfirmed + r.Unconfirmed
	}
	return "", ""
}

func (r *TreeReport) find(path string, who []string) {
	if len(r.Findings) < maxFindings {
		r.Findings = append(r.Findings, TreeFinding{Path: path, Who: who})
		return
	}
	r.More++
}

func (r *TreeReport) doubt(why string) {
	if r.Unconfirmed == "" {
		r.Unconfirmed = why
	}
}

// entryInfo is what the system says about one entry (inspectEntry).
type entryInfo struct {
	dir  bool // a directory that may be entered (never a link)
	link bool // a symbolic link or another reparse point
	// exp.Write: another account may change the entry (Who names it).
	exp Exposure
	// badOwner, badACL: what makes exp.Write true (repairEntry resets
	// these).
	badOwner, badACL bool
	// doubt: why the entry cannot be vouched for although nobody else may
	// change the entry itself ("" = no doubt).
	doubt string
	// reach (macOS, Linux): other accounts can get to what this directory
	// holds.
	reach bool
}

// treeWalk is one walk: CheckTree, or RestrictTree with repair set.
type treeWalk struct {
	o      TreeOptions
	st     *treeState
	rep    TreeReport
	repair bool
	fixed  []string // entries reset
	nfixed int
	failed []string // "path: why" of entries that could not be reset
	full   bool     // MaxEntries reached: nothing more is looked at
}

func (w *treeWalk) scope(rel string) Scope {
	if w.o.Scope == nil {
		return ScopeAll
	}
	return w.o.Scope(rel)
}

func (w *treeWalk) fail(path string, why error) {
	if len(w.failed) < maxFindings {
		w.failed = append(w.failed, fmt.Sprintf("%s: %v", path, why))
	}
}

// visit looks at one entry and, for a directory in scope, at what it holds.
func (w *treeWalk) visit(path, rel string, depth int, sc Scope, parentReach bool) {
	if w.full {
		return
	}
	if w.rep.Entries >= w.o.maxEntries() {
		w.full = true
		w.rep.doubt(fmt.Sprintf("more than %d entries", w.o.maxEntries()))
		return
	}
	info, err := inspectEntry(w.st, path, parentReach)
	if err != nil {
		if rel != "" && errors.Is(err, os.ErrNotExist) {
			if _, lerr := os.Lstat(treePath(path)); lerr != nil {
				return // gone since its folder was listed: nothing to change
			}
			// listed and still there, but not to be opened by its name:
			// never passed over
		}
		w.rep.Entries++
		if w.repair {
			w.fail(path, err)
		}
		w.rep.doubt(fmt.Sprintf("%s cannot be examined: %v", path, err))
		return
	}
	w.rep.Entries++
	if rel == "" && info.link {
		// what a link leads to is not this tree
		w.rep.doubt(path + " is a link")
		if w.repair {
			w.fail(path, errors.New("a link is not reset"))
		}
		return
	}
	if w.repair && (info.exp.Write || info.doubt != "") {
		// root's own entries are Restrict's (resetting them would take the
		// permissions of the folder above it): only its owner is reset
		if rerr := repairEntry(w.st, path, info, rel == ""); rerr != nil {
			w.fail(path, rerr)
		} else if again, ierr := inspectEntry(w.st, path, parentReach); ierr != nil {
			w.fail(path, ierr)
		} else {
			if again.exp.Write || again.doubt != "" {
				w.fail(path, errors.New("still open to other accounts after the reset"))
			} else {
				w.nfixed++
				if len(w.fixed) < maxFindings {
					w.fixed = append(w.fixed, path)
				}
			}
			info = again
		}
	}
	if info.exp.Write {
		w.rep.find(path, info.exp.Who)
	}
	if info.doubt != "" {
		w.rep.doubt(path + " " + info.doubt)
	}
	if !info.dir || sc == ScopeSelf {
		return
	}
	names, more, err := readNames(path, w.o.maxEntries())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		w.rep.doubt(fmt.Sprintf("%s cannot be listed: %v", path, err))
	}
	if more {
		w.rep.doubt(fmt.Sprintf("%s holds more than %d entries", path, w.o.maxEntries()))
	}
	for _, name := range names {
		crel := name
		if rel != "" {
			crel = rel + "/" + name
		}
		csc := w.scope(crel)
		if csc == ScopeSkip {
			continue
		}
		if depth >= w.o.maxDepth() {
			w.rep.doubt(fmt.Sprintf("%s lies deeper than %d levels", childPath(path, name), w.o.maxDepth()))
			return
		}
		w.visit(childPath(path, name), crel, depth+1, csc, info.reach)
		if w.full {
			return
		}
	}
}

// childPath is the path of the entry name in the folder dir. Not
// filepath.Join: the name is kept exactly as the folder lists it (one
// ending in a dot or a space is that entry). dir may end in a separator —
// the root of a drive (E:\) or of a share (\\nas\share\) does —, which is
// not doubled: the exact-name form of a path takes no empty element.
func childPath(dir, name string) string {
	sep := string(os.PathSeparator)
	return strings.TrimSuffix(dir, sep) + sep + name
}

// readNames lists the directory dir in name order, at most limit names
// (more: it holds more than that; they are not read, so that a folder
// filled with millions of entries costs neither time nor memory). What
// could be read before an error is returned with it.
func readNames(dir string, limit int) (names []string, more bool, err error) {
	f, err := os.Open(treePath(dir))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	for {
		batch, rerr := f.Readdirnames(1024)
		names = append(names, batch...)
		if len(names) > limit {
			names, more = names[:limit], true
			break
		}
		if rerr != nil {
			if rerr != io.EOF {
				err = rerr
			}
			break
		}
		if len(batch) == 0 {
			break
		}
	}
	sort.Strings(names)
	return names, more, err
}

func walk(root string, o TreeOptions, repair bool) (*treeWalk, error) {
	if root == "" {
		return nil, errors.New("dirperm: empty path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil, err
	}
	st, err := newTreeState(abs)
	if err != nil {
		return nil, err
	}
	w := &treeWalk{o: o, st: st, repair: repair}
	sc := ScopeAll
	if w.scope("") == ScopeSelf {
		sc = ScopeSelf
	}
	w.visit(abs, "", 0, sc, st.rootReach)
	return w, nil
}

// CheckTree reports which entries of the tree at root (a directory, or a
// single file) other accounts may change. It only reads, enters no link
// and stops at the bounds of o; see the top of this file for the rules. A
// root that does not exist is an error (os.ErrNotExist); everything else
// the walk could not read makes the report unconfirmed.
func CheckTree(root string, o TreeOptions) (TreeReport, error) {
	w, err := walk(root, o, false)
	if err != nil {
		return TreeReport{}, err
	}
	return w.rep, nil
}

// RestrictTree walks like CheckTree and resets every entry it would report:
// the owner becomes the current user and, below root, the entry's own
// permissions are dropped so that it has its folder's again (Windows: an
// empty, unprotected DACL; elsewhere the group's and others' write bits are
// cleared). root's own permissions are Restrict's business: call it first.
// What it cannot reset — a link, an entry it may not take over (on macOS
// and Linux: any entry of another account), a tree beyond the bounds — is
// an error naming the first few; nothing is ever reported as reset that
// still lets another account in. fixed lists the first entries reset.
func RestrictTree(root string, o TreeOptions) (fixed []string, err error) {
	w, err := walk(root, o, true)
	if err != nil {
		return nil, err
	}
	switch {
	case len(w.failed) > 0:
		return w.fixed, fmt.Errorf("dirperm: could not reset %s", strings.Join(w.failed, "; "))
	case !w.rep.OK():
		p, who := w.rep.Problem()
		return w.fixed, fmt.Errorf("dirperm: %s", strings.TrimSpace(p+" "+who))
	}
	return w.fixed, nil
}

// othersOf is what a POSIX mode gives accounts other than the owner: the
// permission bits of the group (unless it is named like the user: a "user
// private group") and of everyone else, and how to name them.
func othersOf(mode os.FileMode, group, myName string) (bits os.FileMode, who []string) {
	perm := mode.Perm()
	if g := (perm >> 3) & 0o7; g != 0 && (myName == "" || group != myName) {
		bits |= g
		who = append(who, "group "+group)
	}
	if o := perm & 0o7; o != 0 {
		bits |= o
		who = append(who, "others")
	}
	return bits, who
}

// posixEntry judges one entry below a checked root on macOS and Linux:
// another account (not root) owning it can always change it, however it is
// reached (an open file stays open); the group's and others' write bits
// count where those accounts can get to the entry (reach), and never for a
// symbolic link (its mode means nothing).
func posixEntry(mode os.FileMode, uid uint32, group string, me uint32, myName string, reach bool) entryInfo {
	info := entryInfo{link: mode&os.ModeSymlink != 0, dir: mode.IsDir()}
	if uid != me && uid != 0 {
		info.exp.Write, info.badOwner = true, true
		info.exp.add(fmt.Sprintf("owner %d", uid))
	}
	if info.link {
		return info
	}
	perm := mode.Perm()
	if g := (perm >> 3) & 0o7; reach && g&0o2 != 0 && (myName == "" || group != myName) {
		info.exp.Write, info.badACL = true, true
		info.exp.add("group " + group)
	}
	if reach && perm&0o2 != 0 {
		info.exp.Write, info.badACL = true, true
		info.exp.add("others")
	}
	if info.dir {
		bits, _ := othersOf(mode, group, myName)
		info.reach = reach && bits&0o1 != 0
	}
	return info
}
