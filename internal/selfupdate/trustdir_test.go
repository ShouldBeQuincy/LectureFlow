// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/dirperm"
)

// The shell's trust directory (SEC17 中-B): made privately where it is
// missing, refused inside the data directory (or around it) and as a link.
// Everything lies in t.TempDir(); never the real %LOCALAPPDATA%.
func TestCheckTrustDir(t *testing.T) {
	data := privateDataDir(t)
	base := privateDataDir(t) // stands for %LOCALAPPDATA%
	trust := filepath.Join(base, "LectureFlow", "selfupdate")

	// missing: made, private, vouched for
	if err := CheckTrustDir(trust, data); err != nil {
		t.Fatalf("a new trust directory: %v", err)
	}
	if e, err := dirperm.Check(trust); err != nil || e.Exposed() {
		t.Fatalf("the new trust directory is not private: %+v %v", e, err)
	}
	// again, with the record in it
	if err := os.WriteFile(filepath.Join(trust, "trust.json"), []byte(`{"schema":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrustDir(trust, data); err != nil {
		t.Fatalf("again: %v", err)
	}

	refused := func(what, dir, dataDir string) {
		t.Helper()
		if err := CheckTrustDir(dir, dataDir); CodeOf(err) != CodeUntrustedDir {
			t.Errorf("%s: %v", what, err)
		}
	}
	// in the data directory (not made there), the data directory itself,
	// the data directory in it; names that only differ in case where the
	// file system ignores it
	in := filepath.Join(data, "electron", "selfupdate")
	refused("in the data directory", in, data)
	if pathThere(filepath.Join(data, "electron")) {
		t.Error("a trust directory was made in the data directory")
	}
	refused("the data directory", data, data)
	refused("around the data directory", filepath.Dir(data), data)
	refused("not absolute", filepath.Join("LectureFlow", "selfupdate"), data)
	refused("empty", "", data)
	// a data directory next to it, whose name begins like it, is no overlap
	sibling := trust + "-data"
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckTrustDir(trust, sibling); err != nil {
		t.Errorf("a sibling data directory: %v", err)
	}
}

// A trust directory that is a link — to a private folder elsewhere, or
// into the data directory — is refused; nothing is made through it.
func TestCheckTrustDirLink(t *testing.T) {
	data := privateDataDir(t)
	base := privateDataDir(t)
	elsewhere := filepath.Join(privateDataDir(t), "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "LectureFlow"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "LectureFlow", "selfupdate")
	dirLink(t, link, elsewhere)
	if err := CheckTrustDir(link, data); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("a link to a private folder: %v", err)
	}
	os.Remove(link)
	inData := filepath.Join(data, "record")
	if err := os.Mkdir(inData, 0o700); err != nil {
		t.Fatal(err)
	}
	dirLink(t, link, inData)
	if err := CheckTrustDir(link, data); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("a link into the data directory: %v", err)
	}
	// the data directory reached through a link to it: overlaps once resolved
	os.Remove(link)
	if err := os.Mkdir(link, 0o700); err != nil {
		t.Fatal(err)
	}
	dataLink := filepath.Join(link, "data")
	dirLink(t, dataLink, data)
	if err := CheckTrustDir(link, dataLink); CodeOf(err) != CodeUntrustedDir {
		t.Fatalf("a data directory inside it through a link: %v", err)
	}
}
