// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"
	"io"
	"runtime"
	"time"
)

// stackDumpMaxBytes caps the goroutine dump written when the graceful stop
// exceeds its limit (T063, test-upgrade F8).
const stackDumpMaxBytes = 1 << 20

// writeStackDump writes the stacks of all goroutines to w as one Write
// (the rotating log puts a single Write whole into one file), at most
// max bytes of stacks; a longer dump is cut and says so.
func writeStackDump(w io.Writer, limit time.Duration, max int) {
	if w == nil {
		return
	}
	buf := make([]byte, max+1)
	n := runtime.Stack(buf, true)
	truncated := n > max
	if truncated {
		n = max
	}
	var out []byte
	out = fmt.Appendf(out, "=== goroutine dump: graceful stop exceeded its time limit (%s) ===\n", limit)
	out = append(out, buf[:n]...)
	if truncated || (n > 0 && buf[n-1] != '\n') {
		out = append(out, '\n')
	}
	if truncated {
		out = fmt.Appendf(out, "=== truncated at %d bytes ===\n", max)
	}
	out = append(out, "=== end of goroutine dump ===\n"...)
	_, _ = w.Write(out)
}
