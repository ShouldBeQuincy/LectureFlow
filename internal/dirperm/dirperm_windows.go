// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Access rights that let another account change a directory or what it
// holds (including its permissions), and those that let it read.
// FILE_WRITE_ATTRIBUTES is a write right of its own: a handle with nothing
// else can turn an empty folder into a junction (security review SEC17).
// The read and execute rights folders usually pass down (FILE_GENERIC_READ,
// FILE_GENERIC_EXECUTE) hold none of these.
const (
	rightsWrite = 0x2 | 0x4 | 0x40 | // FILE_WRITE_DATA / ADD_FILE, FILE_APPEND_DATA / ADD_SUBDIRECTORY, FILE_DELETE_CHILD
		0x10 | 0x100 | // FILE_WRITE_EA, FILE_WRITE_ATTRIBUTES
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL
	rightsRead = 0x1 | // FILE_READ_DATA / LIST_DIRECTORY
		windows.GENERIC_READ | windows.GENERIC_ALL
)

// The kinds of entries a DACL holds (ACE_HEADER.AceType). The deny kinds
// can only take away; every other kind is taken to allow (SEC17: an
// "object" allow entry gives a file's rights like a plain one).
const (
	aceAllowed               = 0  // ACCESS_ALLOWED_ACE_TYPE: header, mask, SID
	aceDenied                = 1  // ACCESS_DENIED_ACE_TYPE
	aceAllowedObject         = 5  // ACCESS_ALLOWED_OBJECT_ACE_TYPE: header, mask, flags, GUIDs that are present, SID
	aceDeniedObject          = 6  // ACCESS_DENIED_OBJECT_ACE_TYPE
	aceAllowedCallback       = 9  // ACCESS_ALLOWED_CALLBACK_ACE_TYPE: as aceAllowed, a condition after the SID
	aceDeniedCallback        = 10 // ACCESS_DENIED_CALLBACK_ACE_TYPE
	aceAllowedCallbackObject = 11 // ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE: as aceAllowedObject, a condition after the SID
	aceDeniedCallbackObject  = 12 // ACCESS_DENIED_CALLBACK_OBJECT_ACE_TYPE

	aceObjectTypePresent          = 0x1 // ACE_OBJECT_TYPE_PRESENT
	aceInheritedObjectTypePresent = 0x2 // ACE_INHERITED_OBJECT_TYPE_PRESENT
)

// aceSID is the account an allow entry names, found where its kind keeps
// it: nil for a kind this program does not know and for an entry too short
// to hold it (the caller takes such an entry to allow).
func aceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	size := uintptr(ace.Header.AceSize)
	off := uintptr(8) // the header and the mask
	switch ace.Header.AceType {
	case aceAllowed, aceAllowedCallback:
	case aceAllowedObject, aceAllowedCallbackObject:
		if size < 12 {
			return nil
		}
		flags := *(*uint32)(unsafe.Add(unsafe.Pointer(ace), 8))
		off = 12
		if flags&aceObjectTypePresent != 0 {
			off += 16
		}
		if flags&aceInheritedObjectTypePresent != 0 {
			off += 16
		}
	default:
		return nil
	}
	// a SID: revision, the number of sub-authorities, the authority (6
	// bytes), then the sub-authorities (4 bytes each)
	if off+8 > size {
		return nil
	}
	p := unsafe.Add(unsafe.Pointer(ace), off)
	if *(*byte)(p) != 1 { // SID_REVISION
		return nil
	}
	if n := uintptr(*(*byte)(unsafe.Add(p, 1))); n > 15 || off+8+4*n > size {
		return nil
	}
	return (*windows.SID)(p)
}

// Accounts that are not "other accounts": besides the user, SYSTEM, the
// administrators and TrustedInstaller (who can take any file anyway), and
// the placeholders CREATOR OWNER / OWNER RIGHTS (they stand for whoever
// creates or owns an entry, who already needs access of their own).
var trustedSIDs = []string{
	"S-1-5-18",     // SYSTEM
	"S-1-5-32-544", // Administrators
	"S-1-3-0",      // CREATOR OWNER
	"S-1-3-4",      // OWNER RIGHTS
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", // NT SERVICE\TrustedInstaller
}

func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

// privateSDDL is the security descriptor of a private directory.
func privateSDDL(user *windows.SID) string {
	return "D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
}

func privateDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(privateSDDL(sid))
}

// winPath is an absolute path that CreateDirectory accepts at any length.
func winPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if len(abs) < 248 || strings.HasPrefix(abs, `\\?\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}

// treePath is the path a walk opens and lists an entry by: always in the
// form the system takes as it is (\\?\…), so that a name ending in a dot or
// a space names that entry and not, after the usual trimming, another one
// or none. path is absolute and clean (the walk builds it from the root and
// the names a folder lists).
func treePath(path string) string {
	switch {
	case strings.HasPrefix(path, `\\?\`), strings.HasPrefix(path, `\\.\`):
		return path
	case strings.HasPrefix(path, `\\`):
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

func createPrivate(dir string, sd *windows.SECURITY_DESCRIPTOR) error {
	p, err := windows.UTF16PtrFromString(winPath(dir))
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	if err := windows.CreateDirectory(p, sa); err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// mkdirPrivate creates dir with the private DACL already in place (no
// moment in which it has the parent's permissions).
func mkdirPrivate(dir string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	return createPrivate(dir, sd)
}

func mkdirTempPrivate(parent, pattern string) (string, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return "", errors.New("dirperm: pattern contains a path separator")
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	sd, err := privateDescriptor()
	if err != nil {
		return "", err
	}
	for range 10000 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := prefix + strconv.FormatUint(uint64(binary.LittleEndian.Uint32(b[:])), 10) + suffix
		dir := filepath.Join(parent, name)
		err := createPrivate(dir, sd)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", &os.PathError{Op: "mkdirtemp", Path: filepath.Join(parent, pattern), Err: os.ErrExist}
}

func restrict(dir string) error {
	sd, err := privateDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// Setting a protected DACL drops the inherited entries; Windows then
	// passes the new inheritable ones down to everything below dir.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return &os.PathError{Op: "set permissions", Path: dir, Err: err}
	}
	return nil
}

func check(dir string) (Exposure, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return Exposure{}, &os.PathError{Op: "read permissions", Path: dir, Err: err}
	}
	me, err := currentUserSID()
	if err != nil {
		return Exposure{}, err
	}
	j, err := judge(sd, me, accountName)
	return j.exp, err
}

// trustedSID: sid is the user me or one of trustedSIDs, not "another
// account".
func trustedSID(sid, me *windows.SID) bool {
	if sid.Equals(me) {
		return true
	}
	s := sid.String()
	// app container and capability SIDs (S-1-15-…) only narrow what a
	// sandboxed program of an account with access may do
	if strings.HasPrefix(s, "S-1-15-") {
		return true
	}
	for _, t := range trustedSIDs {
		if s == t {
			return true
		}
	}
	return false
}

// What lets an account take a folder out of its place (CheckAbove): remove
// or rename the folder itself (DELETE), or any entry of it
// (FILE_DELETE_CHILD), or give itself either (WRITE_DAC, WRITE_OWNER;
// GENERIC_ALL holds them all).
const (
	rightsGrant       = windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL
	rightsRemoveSelf  = windows.DELETE | rightsGrant
	rightsRemoveChild = 0x40 | rightsGrant // FILE_DELETE_CHILD
	aceInheritOnly    = 0x08               // INHERIT_ONLY_ACE: passed down, not for the folder itself
)

// checkAbove: see CheckAbove. dir is absolute and clean.
func checkAbove(dir, top string) (string, Exposure, error) {
	me, err := currentUserSID()
	if err != nil {
		return "", Exposure{}, err
	}
	for child := dir; ; {
		parent := filepath.Dir(child)
		if parent == child || (top != "" && strings.EqualFold(parent, top)) {
			return "", Exposure{}, nil
		}
		root := filepath.Dir(parent) == parent
		if !root {
			// a junction or a symbolic link on the way: its own entry is
			// what the system reads below, not the folders it leads into
			// (security review SEC20): not vouched for
			p, err := windows.UTF16PtrFromString(parent)
			if err != nil {
				return parent, Exposure{}, &os.PathError{Op: "check", Path: parent, Err: err}
			}
			attrs, err := windows.GetFileAttributes(p)
			if err != nil {
				return parent, Exposure{}, &os.PathError{Op: "read attributes", Path: parent, Err: err}
			}
			if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return parent, Exposure{}, &os.PathError{Op: "check", Path: parent, Err: ErrLink}
			}
		}
		sd, err := windows.GetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return parent, Exposure{}, &os.PathError{Op: "read permissions", Path: parent, Err: err}
		}
		// the folder's own entries: its entries (child among them) may be
		// removed; unless it is the root of the drive, the folder itself too
		mask := uint32(rightsRemoveChild)
		if !root {
			mask |= rightsRemoveSelf
		}
		e, err := judgeAbove(sd, me, accountName, mask)
		if err != nil {
			return parent, Exposure{}, &os.PathError{Op: "read permissions", Path: parent, Err: err}
		}
		if e.Write {
			return parent, e, nil
		}
		child = parent
	}
}

// finalPath: the path the system opens for p (GetFinalPathNameByHandle:
// junctions, symbolic links, subst and mapped drives resolved), in the
// usual form (D:\…, \\server\share\…).
func finalPath(p string) (string, error) {
	h, err := openEntryFollow(p)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return finalPathOf(h, p)
}

// finalPathOf: finalPath of the entry open as h (opened as p).
func finalPathOf(h windows.Handle, p string) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", &os.PathError{Op: "final path", Path: p, Err: err}
		}
		if int(n) < len(buf) {
			return tidyFinalPath(windows.UTF16ToString(buf[:n])), nil
		}
		if n > 1<<16 {
			return "", &os.PathError{Op: "final path", Path: p, Err: errors.New("too long")}
		}
		buf = make([]uint16, n+1)
	}
}

// tidyFinalPath: GetFinalPathNameByHandle's \\?\D:\… and \\?\UNC\srv\share\…
// in the usual form; anything else (a volume without a drive letter,
// \\?\Volume{…}\…) is kept whole: an absolute path the system opens as it
// is, not a relative one (security review SEC20 低-B).
func tidyFinalPath(s string) string {
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		return `\\` + s[len(`\\?\UNC\`):]
	case strings.HasPrefix(s, `\\?\`) && len(s) >= 7 && s[5] == ':' && s[6] == '\\':
		return s[len(`\\?\`):]
	}
	return s
}

// openEntryFollow opens p itself or what it leads to, only to ask about it.
func openEntryFollow(p string) (windows.Handle, error) {
	u, err := windows.UTF16PtrFromString(winPath(p))
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: p, Err: err}
	}
	h, err := windows.CreateFile(u, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return h, nil
}

// judgeAbove: whether another account owns the folder of sd, or an entry
// that applies to the folder itself gives another account one of mask's
// rights (Exposure.Write; Read is never set).
func judgeAbove(sd *windows.SECURITY_DESCRIPTOR, me *windows.SID, name func(*windows.SID) string, mask uint32) (Exposure, error) {
	var e Exposure
	if owner, _, err := sd.Owner(); err == nil && owner != nil && !trustedSID(owner, me) {
		e.Write = true
		e.addWriter(name(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return e, err
	}
	if dacl == nil {
		e.Write = true
		e.addWriter("Everyone")
		return e, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return e, err
		}
		switch ace.Header.AceType {
		case aceDenied, aceDeniedObject, aceDeniedCallback, aceDeniedCallbackObject:
			continue
		}
		if ace.Header.AceFlags&aceInheritOnly != 0 {
			continue // for what lies below (judged there, if at all)
		}
		sid := aceSID(ace)
		if sid == nil {
			e.Write = true
			e.addWriter(fmt.Sprintf("an entry of kind %d", ace.Header.AceType))
			continue
		}
		if !trustedSID(sid, me) && uint32(ace.Mask)&mask != 0 {
			e.Write = true
			e.addWriter(name(sid))
		}
	}
	return e, nil
}

// judgement is what a security descriptor lets other accounts do.
type judgement struct {
	exp Exposure
	// badOwner: the owner is another account; badACL: an allow entry gives
	// another account a write right, or there is no DACL.
	badOwner, badACL bool
}

// judge reads the owner and the DACL of sd for the user me (name: how an
// account is called in Exposure.Who).
func judge(sd *windows.SECURITY_DESCRIPTOR, me *windows.SID, name func(*windows.SID) string) (judgement, error) {
	var j judgement
	e := &j.exp
	trusted := func(sid *windows.SID) bool { return trustedSID(sid, me) }
	if owner, _, err := sd.Owner(); err == nil && owner != nil && !trusted(owner) {
		// the owner may always change the permissions
		e.Write, j.badOwner = true, true
		e.addWriter(name(owner))
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return j, err
	}
	if dacl == nil {
		// no DACL (e.g. FAT or exFAT): everyone may do everything
		e.Write, e.Read, j.badACL = true, true, true
		e.addWriter("Everyone")
		return j, nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return j, err
		}
		switch ace.Header.AceType {
		case aceDenied, aceDeniedObject, aceDeniedCallback, aceDeniedCallbackObject:
			continue // deny entries can only take away (counting without them errs on the safe side)
		}
		// Every other kind is taken to allow: the plain and the object
		// kinds do, a callback entry may (its condition is not evaluated
		// here).
		sid := aceSID(ace)
		if sid == nil {
			// a kind this program cannot read (or a damaged entry): it may
			// allow anyone anything
			e.Write, j.badACL = true, true
			e.addWriter(fmt.Sprintf("an entry of kind %d", ace.Header.AceType))
			continue
		}
		if trusted(sid) {
			continue
		}
		// Inherit-only entries count too: they apply to what lies below.
		m := uint32(ace.Mask)
		if m&rightsWrite != 0 {
			e.Write, j.badACL = true, true
			e.addWriter(name(sid))
		}
		if m&rightsRead != 0 {
			e.Read = true
			e.add(name(sid))
		}
	}
	return j, nil
}

// ---- trees (tree.go) ----

// treeSelf is the user a walk judges entries for and makes their owner
// (tests replace it: one account cannot make a file another account owns).
var treeSelf = currentUserSID

// treeState is what a walk keeps between entries.
type treeState struct {
	me        *windows.SID
	names     map[string]string // SID → account name
	rootReach bool              // (macOS, Linux)
}

func newTreeState(string) (*treeState, error) {
	me, err := treeSelf()
	if err != nil {
		return nil, err
	}
	return &treeState{me: me, names: map[string]string{}}, nil
}

func (st *treeState) name(sid *windows.SID) string {
	s := sid.String()
	if n, ok := st.names[s]; ok {
		return n
	}
	n := accountName(sid)
	st.names[s] = n
	return n
}

// openEntry opens the entry at path itself (a link is not followed) for
// reading or changing its security descriptor.
func openEntry(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(treePath(path))
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// inspectEntry reads the kind, the owner and the DACL of the entry at path
// from one handle.
func inspectEntry(st *treeState, path string, _ bool) (entryInfo, error) {
	var info entryInfo
	h, err := openEntry(path, windows.READ_CONTROL)
	if err != nil {
		return info, err
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return info, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return info, &os.PathError{Op: "read permissions", Path: path, Err: err}
	}
	j, err := judge(sd, st.me, st.name)
	if err != nil {
		return info, &os.PathError{Op: "read permissions", Path: path, Err: err}
	}
	info.exp = Exposure{Write: j.exp.Write}
	if j.exp.Write {
		// only those that may change it (an entry is reported for them)
		info.exp.Who, info.exp.Writers = j.exp.Writers, j.exp.Writers
	}
	info.badOwner, info.badACL = j.badOwner, j.badACL
	info.link = fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	info.dir = fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && !info.link
	if info.link {
		info.doubt = "is a link (a reparse point): where it leads is not checked"
	}
	return info, nil
}

// emptyACL is an ACL without entries (never a missing DACL, which would
// admit everyone): with it and no protection an entry has exactly what its
// folder passes down.
func emptyACL() *windows.ACL {
	b := new([8]byte)                       // an ACL header
	b[0] = 2                                // ACL_REVISION
	binary.LittleEndian.PutUint16(b[2:], 8) // AclSize
	return (*windows.ACL)(unsafe.Pointer(b))
}

// repairEntry makes the user the owner of the entry at path and, unless it
// is the root of the walk, drops its own entries so that it inherits its
// folder's again. A link is never reset: taking it over would vouch for
// where it leads.
func repairEntry(st *treeState, path string, info entryInfo, root bool) error {
	if info.link {
		return errors.New("a link is not reset (remove it if it is not yours)")
	}
	set := func(access uint32, what windows.SECURITY_INFORMATION, owner *windows.SID, dacl *windows.ACL) error {
		h, err := openEntry(path, access)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, what, owner, nil, dacl, nil); err != nil {
			return &os.PathError{Op: "set permissions", Path: path, Err: err}
		}
		return nil
	}
	if info.badOwner {
		if err := set(windows.READ_CONTROL|windows.WRITE_OWNER, windows.OWNER_SECURITY_INFORMATION, st.me, nil); err != nil {
			return err
		}
	}
	if info.badACL && !root {
		return set(windows.READ_CONTROL|windows.WRITE_DAC, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, emptyACL())
	}
	return nil
}

// accountName is DOMAIN\name of a SID (localised by Windows), or the SID
// when it cannot be looked up within half a second (a domain account while
// the domain controller is unreachable).
func accountName(sid *windows.SID) string {
	s := sid.String()
	c, err := sid.Copy()
	if err != nil {
		return s
	}
	done := make(chan string, 1)
	go func() {
		acc, dom, _, err := c.LookupAccount("")
		switch {
		case err != nil || acc == "":
			done <- s
		case dom == "" || strings.EqualFold(dom, "BUILTIN") || strings.EqualFold(dom, "NT AUTHORITY") || dom == acc:
			done <- acc
		default:
			done <- dom + `\` + acc
		}
	}()
	select {
	case n := <-done:
		return n
	case <-time.After(500 * time.Millisecond):
		return s
	}
}

func save(dir string) (func() error, error) {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, &os.PathError{Op: "read permissions", Path: dir, Err: err}
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return nil, err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return func() error {
		// sd (Go memory) holds dacl until this runs
		_ = sd
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
			return &os.PathError{Op: "set permissions", Path: dir, Err: err}
		}
		return nil
	}, nil
}
