// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package dirperm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// junction: see Junction. The link is opened once, by its exact name and
// without following it; its kind, its own security descriptor and the path
// it holds are read from that handle.
func junction(dir string) (JunctionInfo, bool, error) {
	var info JunctionInfo
	abs, err := filepath.Abs(dir)
	if err != nil {
		return info, false, err
	}
	abs = filepath.Clean(abs)
	h, err := openEntry(abs, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return info, false, err
	}
	defer windows.CloseHandle(h)
	var tag struct{ Attributes, ReparseTag uint32 } // FILE_ATTRIBUTE_TAG_INFO
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&tag)), uint32(unsafe.Sizeof(tag))); err != nil {
		return info, false, &os.PathError{Op: "stat", Path: abs, Err: err}
	}
	if tag.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 || tag.ReparseTag != windows.IO_REPARSE_TAG_MOUNT_POINT {
		return info, false, nil
	}
	// a junction from here on: every error says it cannot be vouched for

	// the link's own owner and DACL (whoever may change them, or write
	// the link, may point it elsewhere)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return info, true, &os.PathError{Op: "read permissions", Path: abs, Err: err}
	}
	me, err := currentUserSID()
	if err != nil {
		return info, true, err
	}
	j, err := judge(sd, me, accountName)
	if err != nil {
		return info, true, &os.PathError{Op: "read permissions", Path: abs, Err: err}
	}
	if j.exp.Write {
		info.Link = Exposure{Write: true, Who: j.exp.Writers, Writers: j.exp.Writers}
	}
	// where the link itself really lies (a subst or mapped drive letter
	// hides the folders above it)
	at, err := finalPathOf(h, abs)
	if err != nil {
		return info, true, err
	}
	info.LinkPath = filepath.Clean(at)

	raw, err := mountPointTarget(h)
	if err != nil {
		return info, true, &os.PathError{Op: "read link", Path: abs, Err: err}
	}
	named, err := substitutePath(raw)
	if err != nil {
		return info, true, err
	}
	named = filepath.Clean(longPath(named))
	final, err := finalPath(abs)
	if err != nil {
		return info, true, fmt.Errorf("where it leads (%s) cannot be opened: %w", named, err)
	}
	final = filepath.Clean(final)
	if !sameNamedPath(final, named) {
		return info, true, fmt.Errorf("it leads to %s, which is really %s: reached through another link or a drive letter of another place", named, final)
	}
	// and no folder on that path is a link, each looked at by its exact
	// name (security review SEC30, 低-1: the comparison above is the
	// system's view only as far as its case rules go)
	if err := linkOnTheWay(named); err != nil {
		return info, true, err
	}
	// what the link leads to, opened through it, is the folder at that
	// path itself, opened without following anything: the same file, a
	// folder, not a link
	if err := samePlainDir(abs, final); err != nil {
		return info, true, err
	}
	info.Target = final
	return info, true, nil
}

// substitutePath is the folder a junction's substitute name (\??\D:\…)
// names, clean: only a folder on a drive letter — not a volume
// (\??\Volume{…}\: a mounted drive), a share (\??\UNC\…) or a device — and
// never the root of a drive, which "收紧权限" would make private as a
// whole (security review SEC30, 信息-4).
func substitutePath(raw string) (string, error) {
	rest, ok := strings.CutPrefix(raw, `\??\`)
	if !ok || len(rest) < 3 || rest[1] != ':' || rest[2] != '\\' || !isDriveLetter(rest[0]) {
		return "", fmt.Errorf("it leads to %s: not a folder on a drive", raw)
	}
	p := filepath.Clean(rest)
	if isRoot(p) {
		return "", fmt.Errorf("it leads to %s: the root of a drive", p)
	}
	return p, nil
}

func isDriveLetter(c byte) bool { return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' }

// sameNamedPath: a and b are the same path, ignoring the case of ASCII
// letters only. Any other character must be the same: Unicode case folding
// (strings.EqualFold) takes KELVIN SIGN for k, LONG S for s, OHM SIGN for
// omega, which NTFS keeps apart as different names (security review SEC30,
// 低-1). A path whose only difference is the case of a letter outside ASCII
// is therefore not confirmed (the safe side).
func sameNamedPath(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y || x < 'a' || x > 'z' {
			return false
		}
	}
	return true
}

// linkOnTheWay reports the first folder of the absolute path p (D:\…),
// from below the root of its drive down to p itself, that is a reparse
// point, each looked at by its exact name without following it.
func linkOnTheWay(p string) error {
	vol := filepath.VolumeName(p)
	cur := vol + `\`
	for _, name := range strings.Split(strings.Trim(p[len(vol):], `\`), `\`) {
		if name == "" {
			continue
		}
		cur = childPath(cur, name)
		u, err := windows.UTF16PtrFromString(treePath(cur))
		if err != nil {
			return &os.PathError{Op: "check", Path: cur, Err: err}
		}
		attrs, err := windows.GetFileAttributes(u)
		if err != nil {
			return fmt.Errorf("%s cannot be examined: %w", cur, err)
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("it leads through %s, itself a link", cur)
		}
	}
	return nil
}

// longPath is p with its short (8.3) names spelled out
// (GetLongPathName: each name as its folder lists it; a link on the way is
// not followed), or p itself when that cannot be done. mklink keeps a path
// as it was typed, the final path has the long names.
func longPath(p string) string {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, windows.MAX_PATH)
	for range 3 {
		n, err := windows.GetLongPathName(u, &buf[0], uint32(len(buf)))
		switch {
		case err != nil || n == 0:
			return p
		case int(n) < len(buf):
			return windows.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n+1)
	}
	return p
}

// mountPointTarget is the path a junction holds (its substitute name, as
// \??\D:\…), read from a handle of the link itself.
func mountPointTarget(h windows.Handle) (string, error) {
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return "", err
	}
	// REPARSE_DATA_BUFFER: tag, data length, reserved; then the mount
	// point's offsets and lengths (bytes, from the start of its path
	// buffer at 16)
	if n < 16 || int(n) > len(buf) {
		return "", errors.New("a reparse point too short to name a folder")
	}
	b := buf[:n]
	if binary.LittleEndian.Uint32(b) != windows.IO_REPARSE_TAG_MOUNT_POINT {
		return "", errors.New("not a junction")
	}
	end := 8 + int(binary.LittleEndian.Uint16(b[4:]))
	off, size := int(binary.LittleEndian.Uint16(b[8:])), int(binary.LittleEndian.Uint16(b[10:]))
	if end > len(b) || size == 0 || size%2 != 0 || off%2 != 0 || 16+off+size > end {
		return "", errors.New("a damaged junction")
	}
	name := make([]uint16, size/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(b[16+off+2*i:])
	}
	return windows.UTF16ToString(name), nil
}

// samePlainDir: link, opened through it, and target, opened without
// following a link, are the same file, and that is a folder that is not a
// reparse point.
func samePlainDir(link, target string) error {
	hf, err := openEntryFollow(link)
	if err != nil {
		return fmt.Errorf("where it leads cannot be opened: %w", err)
	}
	defer windows.CloseHandle(hf)
	ht, err := openEntry(target, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return fmt.Errorf("%s cannot be opened: %w", target, err)
	}
	defer windows.CloseHandle(ht)
	var a, b windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(hf, &a); err != nil {
		return &os.PathError{Op: "stat", Path: link, Err: err}
	}
	if err := windows.GetFileInformationByHandle(ht, &b); err != nil {
		return &os.PathError{Op: "stat", Path: target, Err: err}
	}
	return sameDirInfo(target, a, b)
}

// sameDirInfo: through (what the link leads to, opened through it) and
// named (the folder at target, opened by its name without following it)
// are the same file — the same volume and file index: the folder that was
// there when the path was resolved, not one put in its place since (a
// guard against a race, hard to stage for real) — and a folder that is not
// a reparse point.
func sameDirInfo(target string, through, named windows.ByHandleFileInformation) error {
	switch {
	case named.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return fmt.Errorf("it leads to %s, itself a link", target)
	case named.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return fmt.Errorf("it leads to %s, not a folder", target)
	case through.VolumeSerialNumber != named.VolumeSerialNumber || through.FileIndexHigh != named.FileIndexHigh ||
		through.FileIndexLow != named.FileIndexLow:
		return fmt.Errorf("it does not lead to %s itself", target)
	}
	return nil
}
