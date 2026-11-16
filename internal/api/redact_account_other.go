// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build !windows

package api

// systemDomainsOfSIDs: only Windows has the system's domains.
func systemDomainsOfSIDs() []string { return nil }
