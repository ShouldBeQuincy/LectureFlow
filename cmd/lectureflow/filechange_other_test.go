// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package main

import "time"

// testCreationTime: not read here (ok false).
func testCreationTime(string) (time.Time, bool) { return time.Time{}, false }
