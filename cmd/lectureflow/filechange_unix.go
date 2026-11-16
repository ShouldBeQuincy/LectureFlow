// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build linux || darwin

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lastChange is the later of the file's modification and status change
// (ctime) times (path is followed like os.Stat). The ctime moves with every
// write, rename, permission or time stamp change, so a copy put in place
// with an older modification time still counts as changed now.
func lastChange(path string) (time.Time, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return time.Time{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	m := time.Unix(st.Mtim.Unix())
	if c := time.Unix(st.Ctim.Unix()); c.After(m) {
		return c, nil
	}
	return m, nil
}
