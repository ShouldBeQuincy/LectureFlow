// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileBasicInfo is FILE_BASIC_INFO (GetFileInformationByHandleEx, class
// FileBasicInfo); times are FILETIMEs (100 ns since 1601).
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// lastChange is the latest of the file's last write, creation and change
// times (path is followed like os.Stat). The change time moves with every
// write, rename, attribute or time stamp change; the creation time alone
// is not enough: a file deleted and created again under the same name
// within seconds gets the old one's creation time back (file name
// tunneling), and one copied over another keeps it.
func lastChange(path string) (time.Time, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return time.Time{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return time.Time{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var bi fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&bi)), uint32(unsafe.Sizeof(bi))); err != nil {
		return time.Time{}, &os.PathError{Op: "GetFileInformationByHandleEx", Path: path, Err: err}
	}
	latest := bi.LastWriteTime
	for _, t := range []int64{bi.CreationTime, bi.ChangeTime} {
		if t > latest {
			latest = t
		}
	}
	ft := windows.Filetime{LowDateTime: uint32(latest), HighDateTime: uint32(uint64(latest) >> 32)}
	return time.Unix(0, ft.Nanoseconds()), nil
}
