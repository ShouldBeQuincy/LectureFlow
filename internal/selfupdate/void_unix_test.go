// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package selfupdate

import (
	"os"
	"testing"
)

// dirLink makes link a symbolic link to the directory target. See
// void_links_test.go.
func dirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// (registered after the temporary directories: runs before they go)
	t.Cleanup(func() { os.Remove(link) })
}
