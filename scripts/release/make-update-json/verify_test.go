// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
)

var testPackages = []string{"LectureFlow-Setup-3.2.0-x64.exe", "LectureFlow-3.2.0-x64-portable.exe",
	"LectureFlow-3.2.0-arm64.dmg", "LectureFlow-3.2.0-arm64-mac.zip"}

// signFixture is a release as the maintainer has it before signing: the four
// packages in dir and the update.json CI made from them (in its own dir).
func signFixture(t *testing.T) (dir, manifest string) {
	t.Helper()
	dir, _ = releaseDir(t, "3.2.0", testPackages...)
	_, b, err := build(options{dir: dir, version: "3.2.0", repo: selfupdate.Repo}, time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	manifest = filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(manifest, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, manifest
}

// editManifest rewrites update.json through f (as an attacker with write
// access to the draft release could).
func editManifest(t *testing.T, manifest string, f func(m *selfupdate.Manifest)) {
	t.Helper()
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m selfupdate.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(&m)
	b, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findAsset(m *selfupdate.Manifest, name string) *selfupdate.Asset {
	for p := range m.Platforms {
		for i := range m.Platforms[p] {
			if m.Platforms[p][i].Name == name {
				return &m.Platforms[p][i]
			}
		}
	}
	return nil
}

// signCase is one tampering the check must refuse; want is a fragment of
// the refusal that names the item.
type signCase struct {
	name    string
	tag     string
	channel string
	mutate  func(t *testing.T, dir, manifest string)
	want    string
}

var mirrorRule = selfupdate.MirrorRule{Prefix: "https://github.com/", Replace: "https://mirror.example.com/gh/"}

func signCases() []signCase {
	exe := testPackages[0]
	return []signCase{
		{name: "update.json sha256 edited", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { findAsset(m, exe).SHA256 = strings.Repeat("ab", 32) })
		}, want: "sha256"},
		{name: "update.json size edited", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { findAsset(m, exe).Size++ })
		}, want: "size"},
		{name: "package replaced with a longer file", mutate: func(t *testing.T, dir, _ string) {
			if err := os.WriteFile(filepath.Join(dir, exe), bytes.Repeat([]byte{9}, 5000), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "size"},
		{name: "package changed, same size", mutate: func(t *testing.T, dir, _ string) {
			p := filepath.Join(dir, testPackages[2])
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 0xff
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "sha256"},
		{name: "url on another repository", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.URL = strings.Replace(a.URL, selfupdate.Repo, "attacker/LectureFlow", 1)
			})
		}, want: "url"},
		{name: "url of another tag", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.URL = strings.Replace(a.URL, "/v3.2.0/", "/v3.1.0/", 1)
			})
		}, want: "url"},
		{name: "plain http url", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.URL = strings.Replace(a.URL, "https://", "http://", 1)
			})
		}, want: "url"},
		{name: "url host in capitals", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.URL = strings.Replace(a.URL, "github.com", "GitHub.com", 1)
			})
		}, want: "url"},
		{name: "url with a port", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.URL = strings.Replace(a.URL, "github.com/", "github.com:443/", 1)
			})
		}, want: "url"},
		{name: "url with a query", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { findAsset(m, exe).URL += "?x=1" })
		}, want: "url"},
		{name: "mirror rule", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { m.Mirrors = []selfupdate.MirrorRule{mirrorRule} })
		}, want: "mirrors"},
		{name: "notes_url elsewhere", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { m.NotesURL = "https://example.com/notes" })
		}, want: "notes_url"},
		{name: "version is not the tag's", tag: "v3.2.1", want: "version"},
		{name: "beta tag, stable update.json", tag: "v3.2.0-beta.1", want: "version"},
		{name: "channel edited", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) { m.Channel = selfupdate.ChannelBeta })
		}, want: "channel"},
		{name: "channel not the one asked for", channel: "beta", want: "channel"},
		{name: "extra package in update.json", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := *findAsset(m, exe)
				a.Kind = "msi"
				m.Platforms[selfupdate.PlatformWindowsX64] = append(m.Platforms[selfupdate.PlatformWindowsX64], a)
			})
		}, want: "unexpected package"},
		{name: "extra platform in update.json", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				m.Platforms["linux-x64"] = []selfupdate.Asset{*findAsset(m, exe)}
			})
		}, want: "unexpected package"},
		{name: "package missing from update.json", mutate: func(t *testing.T, _, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				m.Platforms[selfupdate.PlatformDarwinArm64] = m.Platforms[selfupdate.PlatformDarwinArm64][:1]
			})
		}, want: "lists no"},
		{name: "listed package missing locally", mutate: func(t *testing.T, dir, _ string) {
			if err := os.Remove(filepath.Join(dir, testPackages[1])); err != nil {
				t.Fatal(err)
			}
		}, want: "not in the package directory"},
		{name: "asset renamed", mutate: func(t *testing.T, dir, mf string) {
			editManifest(t, mf, func(m *selfupdate.Manifest) {
				a := findAsset(m, exe)
				a.Name = "Evil-Setup.exe"
				a.URL = releaseDownloadPrefix("v3.2.0") + a.Name
			})
		}, want: "name"},
		{name: "not an update.json", mutate: func(t *testing.T, _, mf string) {
			os.WriteFile(mf, []byte(`{"schema":1,"app":"Other"}`), 0o644)
		}, want: "does not parse"},
	}
}

func TestVerifyManifest(t *testing.T) {
	dir, mf := signFixture(t)
	raw, _ := os.ReadFile(mf)
	res, err := verifyManifest(raw, dir, "v3.2.0", "", false)
	if err != nil || len(res.assets) != 4 {
		t.Fatalf("a consistent release refused: %v", err)
	}
	if _, err := verifyManifest(raw, dir, "v3.2.0", "stable", false); err != nil {
		t.Fatalf("-channel stable: %v", err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-verify", mf, "-dir", dir, "-tag", "v3.2.0"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "verified: 4 packages") {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	for _, args := range [][]string{
		{"-verify", mf, "-dir", dir},
		{"-verify", mf, "-tag", "v3.2.0"},
		{"-verify", mf, "-dir", dir, "-tag", "v3.2.0", "-repo", "attacker/x"},
	} {
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	for _, tag := range []string{"3.2.0", "v3.2", "release-3.2.0"} {
		if _, err := verifyManifest(raw, dir, tag, "", false); err == nil {
			t.Errorf("tag %q accepted", tag)
		}
	}
}

func TestVerifyManifestRefuses(t *testing.T) {
	for _, c := range signCases() {
		t.Run(c.name, func(t *testing.T) {
			dir, mf := signFixture(t)
			if c.mutate != nil {
				c.mutate(t, dir, mf)
			}
			tag := c.tag
			if tag == "" {
				tag = "v3.2.0"
			}
			raw, _ := os.ReadFile(mf)
			_, err := verifyManifest(raw, dir, tag, c.channel, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
		})
	}
	// both macOS zip names in the directory: which one is meant is unclear
	dir, mf := signFixture(t)
	os.WriteFile(filepath.Join(dir, "LectureFlow-3.2.0-arm64.zip"), []byte("other"), 0o644)
	raw, _ := os.ReadFile(mf)
	if _, err := verifyManifest(raw, dir, "v3.2.0", "", false); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("two zips: %v", err)
	}
}

func TestVerifyManifestMirrorsOnlyWhenAllowed(t *testing.T) {
	dir, mf := signFixture(t)
	editManifest(t, mf, func(m *selfupdate.Manifest) { m.Mirrors = []selfupdate.MirrorRule{mirrorRule} })
	raw, _ := os.ReadFile(mf)
	if _, err := verifyManifest(raw, dir, "v3.2.0", "", false); err == nil || !strings.Contains(err.Error(), "mirrors") {
		t.Fatalf("mirrors without -allow-mirrors: %v", err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-verify", mf, "-dir", dir, "-tag", "v3.2.0", "-allow-mirrors"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "MIRROR RULE") || !strings.Contains(out.String(), mirrorRule.Replace) {
		t.Fatalf("-allow-mirrors: exit %d: %s %s", code, out.String(), errb.String())
	}
	sum := sha256.Sum256(raw)
	if !strings.Contains(out.String(), "checked update.json sha256 "+hex.EncodeToString(sum[:])+"\n") {
		t.Fatalf("no checked sha256 line: %s", out.String())
	}
}

func TestVerifyManifestRefusesSymlinkedPackage(t *testing.T) {
	dir, mf := signFixture(t)
	p := filepath.Join(dir, testPackages[0])
	real := filepath.Join(t.TempDir(), "real.exe")
	b, _ := os.ReadFile(p)
	os.WriteFile(real, b, 0o644)
	os.Remove(p)
	if err := os.Symlink(real, p); err != nil {
		// Windows without the symlink privilege: nothing to test here,
		// the same bytes as a plain file are covered by the other cases.
		t.Logf("no symlink (%v); checked that the plain file is accepted instead", err)
		os.WriteFile(p, b, 0o644)
		raw, _ := os.ReadFile(mf)
		if _, err := verifyManifest(raw, dir, "v3.2.0", "", false); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, _ := os.ReadFile(mf)
	if _, err := verifyManifest(raw, dir, "v3.2.0", "", false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked package: %v", err)
	}
}
