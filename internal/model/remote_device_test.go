// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"strings"
	"testing"
)

// SEC7 L7: a new SSH server's slug starts with a letter or a digit. The
// slugs of the servers that exist already (SSHSlugPattern) are still
// recognised, a leading "-" included: their device rows stay SSH devices.
func TestSSHSlugPatterns(t *testing.T) {
	long := strings.Repeat("a", 32)
	for slug, want := range map[string][2]bool{ // existing, new
		"a":         {true, true},
		"0":         {true, true},
		"gpu-box-2": {true, true},
		"a-":        {true, true},
		long:        {true, true},
		"-x":        {true, false},
		"-":         {true, false},
		"--":        {true, false},
		"":          {false, false},
		long + "a":  {false, false},
		"Lab":       {false, false},
		"a_b":       {false, false},
		"a b":       {false, false},
		"a\n":       {false, false},
	} {
		if got := SSHSlugPattern.MatchString(slug); got != want[0] {
			t.Errorf("SSHSlugPattern %q: %v, want %v", slug, got, want[0])
		}
		if got := SSHNewSlugPattern.MatchString(slug); got != want[1] {
			t.Errorf("SSHNewSlugPattern %q: %v, want %v", slug, got, want[1])
		}
		if got := IsSSHDevice(SSHDevicePrefix + slug); got != want[0] {
			t.Errorf("IsSSHDevice ssh:%q: %v, want %v", slug, got, want[0])
		}
	}
	if !IsRemoteDevice("ssh:-x") || SSHSlug("ssh:-x") != "-x" {
		t.Error("an existing server with a leading \"-\" is no SSH device any more")
	}
}
