// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Windows path length (V12.2 acceptance, issue 1). Python on Windows loads
// extension modules and their DLLs with LoadLibrary, which fails with "The
// filename or extension is too long" once the absolute path exceeds
// MAX_PATH (260 UTF-16 units including the terminating NUL) unless the
// system enables long paths, which LectureFlow must not change. The deepest
// file of the CUDA environment is about 150 characters below the data
// directory, so a data directory longer than about 100 characters breaks
// it. Plan / Install / Update / ImportFile refuse such an install at once
// with components.path_too_long, and relocate.CheckMove refuses to move the
// data directory to a place too long for the installed components
// (data_dir.path_too_long).
const (
	// MaxPathLen is the longest absolute path (UTF-16 units) a component
	// file may get: MAX_PATH 260 − the NUL − a margin of 9 for what the
	// measurement in Build.MaxPath cannot see (files Python writes later,
	// a slightly deeper file after a lock refresh).
	MaxPathLen = 250

	// rebuildSuffix is the longest suffix finalRel adds to a version
	// directory ("-r99", a venv rebuilt with the same version). Staging
	// directories (".s{n}", n < 100) are never longer than {version}-r99.
	rebuildSuffix = "-r99"
)

// PathLen is the length of p as Windows counts it (UTF-16 code units).
func PathLen(p string) int {
	n := 0
	for _, r := range p {
		n += utf16.RuneLen(r)
	}
	return n
}

// buildMaxRel is the longest path inside an installed build relative to
// its version directory; 0 = unknown.
func buildMaxRel(c *Component, b *Build) int {
	if b == nil {
		return 0
	}
	if b.MaxPath > 0 {
		return b.MaxPath
	}
	sub := ""
	switch c.Kind {
	case KindModel:
		sub = "model/"
	case KindBinary:
		sub = "bin/"
	default:
		return 0 // a venv or runtime: known only from a measurement
	}
	longest := 0
	for _, f := range b.Files {
		if f.Unpack != "" {
			return 0 // the archive's entries are not listed
		}
		longest = max(longest, PathLen(sub+f.Name))
	}
	return longest
}

// RelPathNeed is the longest path, relative to the data directory, that
// installing the build creates:
// components/{dir}/{version}-r99/{longest file}. 0 = unknown.
func RelPathNeed(c *Component, b *Build) int {
	rel := buildMaxRel(c, b)
	if rel == 0 {
		return 0
	}
	return PathLen("components/"+DirName(c.ID)+"/"+c.Version+rebuildSuffix+"/") + rel
}

// limitsPaths: the platform has the MAX_PATH limit.
func limitsPaths(platform string) bool { return strings.HasPrefix(platform, "windows") }

// DataDirLimit is the longest data directory (an absolute path without a
// trailing separator, in UTF-16 units as PathLen counts) under which the
// current manifest's build of component id for this platform can be
// installed: checkPathLen's rule, MaxPathLen − 1 − RelPathNeed. ok is
// false when no such limit is known: the platform has no MAX_PATH limit,
// the manifest has no such component or no build of it for this platform,
// or the build's depth is unknown (RelPathNeed 0, which checkPathLen lets
// through too). It only reads the manifest (the diagnostics page).
func (m *Manager) DataDirLimit(id string) (max int, ok bool) {
	if !limitsPaths(m.platform) {
		return 0, false
	}
	c := m.manifest().Component(id)
	if c == nil {
		return 0, false
	}
	need := RelPathNeed(c, c.Build(m.platform))
	if need == 0 {
		return 0, false
	}
	return MaxPathLen - 1 - need, true
}

// checkPathLen refuses a build whose deepest file would get a path longer
// than MaxPathLen under this data directory (Windows platforms only).
func (m *Manager) checkPathLen(c *Component, b *Build) error {
	if !limitsPaths(m.platform) {
		return nil
	}
	need := RelPathNeed(c, b)
	if need == 0 {
		return nil
	}
	dataDir := m.dataDir
	if a, err := filepath.Abs(dataDir); err == nil {
		dataDir = a
	}
	dataDir = strings.TrimRight(dataDir, `\/`)
	total := PathLen(dataDir) + 1 + need
	if total <= MaxPathLen {
		return nil
	}
	return cerr(CodePathTooLong, map[string]any{
		"id": c.ID, "path_len": total, "max": MaxPathLen, "data_dir": dataDir,
	}, fmt.Errorf("%s: its deepest file would have a %d-character path under %s (Windows allows %d)",
		c.ID, total, dataDir, MaxPathLen))
}
