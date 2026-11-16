// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"os"
	"path/filepath"
	"testing"
)

// T27 P2: DataDirLimit tells the diagnostics page the longest data
// directory under which a component can be installed, by the rule Plan
// applies (checkPathLen): a data directory of exactly that length plans,
// one character more is refused with path_too_long.
func TestDataDirLimitMatchesPlan(t *testing.T) {
	e := newTestEnv(t)
	man := e.manifest(defaultSpec())
	man.Component(IDPython).Platforms[PlatformWindowsX64].MaxPath = 113 // as the shipped manifest measures it
	if err := os.WriteFile(filepath.Join(e.manDir, "components.json"), marshalManifest(t, man), 0o644); err != nil {
		t.Fatal(err)
	}
	need := RelPathNeed(man.Component(IDPython), man.Component(IDPython).Build(PlatformWindowsX64))

	m := e.open()
	limit, ok := m.DataDirLimit(IDPython)
	if !ok || limit != MaxPathLen-1-need {
		t.Fatalf("DataDirLimit(python) = %d, %v; want %d", limit, ok, MaxPathLen-1-need)
	}
	// unknown: a component without a measurement, an id the manifest lacks
	if _, ok := m.DataDirLimit(IDASRCUDA); ok {
		t.Error("asr-cuda has no measurement in this manifest, but a limit is reported")
	}
	if _, ok := m.DataDirLimit("no-such-component"); ok {
		t.Error("a limit for a component the manifest lacks")
	}
	m.Close()

	// the same rule as Plan, on both sides of the limit
	e.data = longDataDir(t, limit)
	m2 := e.open()
	if _, err := m2.Plan([]string{IDPython}); err != nil {
		t.Fatalf("a data directory of %d characters (the limit): %v", limit, err)
	}
	if got, _ := m2.DataDirLimit(IDPython); got != limit {
		t.Errorf("the limit depends on the data directory: %d, want %d", got, limit)
	}
	m2.Close()
	e.data = longDataDir(t, limit+1)
	m3 := e.open()
	if _, err := m3.Plan([]string{IDPython}); CodeOf(err) != CodePathTooLong {
		t.Fatalf("a data directory of %d characters: %v, want %s", limit+1, err, CodePathTooLong)
	}
	m3.Close()

	// no MAX_PATH limit elsewhere
	o := e.options()
	o.Platform = PlatformDarwinARM64
	m4, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer m4.Close()
	if _, ok := m4.DataDirLimit(IDPython); ok {
		t.Error("a limit on macOS")
	}
}
