// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// -verify: the maintainer's check before signing (sign-update.sh / .ps1 run
// it). update.json comes from CI and the draft release; a signature vouches
// for it, so before the key is touched every claim in it is checked against
// files the maintainer downloaded and against the tag being signed:
//
//	go run ./scripts/release/make-update-json -verify update.json -dir PACKAGES -tag v3.2.0 [-channel stable|beta]
//
// update.json is read with the application's own parser
// (selfupdate.ParseManifest), so the check sees exactly what installations
// will see. It refuses, naming the item:
//   - a version that is not the tag's, or a channel that is not the expected
//     one (-channel, default: beta for a pre-release version, else stable);
//   - an asset of an unknown platform or kind, or under another name than
//     the one make-update-json gives that package;
//   - an asset URL other than https://github.com/<selfupdate.Repo>/releases/download/<tag>/<name>;
//   - a notes_url outside https://github.com/<selfupdate.Repo>/;
//   - a package listed but missing in -dir, or in -dir but not listed (all
//     four packages must be in both);
//   - a size or SHA-256 that differs from the local file.
//
//   - mirror rules, unless -allow-mirrors: the application checks every
//     download against the signed SHA-256 whichever URL it comes from, but a
//     signed rule sends retries (and the user's IP) to another host, and CI
//     makes none. Allowed rules are printed.
//
// On success it prints "checked update.json sha256 <hex>" for the bytes it
// checked; the sign scripts compare that with the file before and after
// minisign reads it.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"lectureflow/internal/selfupdate"
)

var tagRe = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// verifyResult is what a passed check printed for the maintainer.
type verifyResult struct {
	manifest *selfupdate.Manifest
	assets   []verifiedAsset
}

type verifiedAsset struct {
	platform string
	selfupdate.Asset
}

// releaseDownloadPrefix is the fixed URL prefix of this repository's
// release assets for a tag.
func releaseDownloadPrefix(tag string) string {
	return "https://github.com/" + selfupdate.Repo + "/releases/download/" + url.PathEscape(tag) + "/"
}

// verifyManifest checks the update.json bytes against the packages in dir
// and the tag being signed; channel "" means the default for the version.
func verifyManifest(data []byte, dir, tag, channel string, allowMirrors bool) (*verifyResult, error) {
	if !tagRe.MatchString(tag) {
		return nil, fmt.Errorf("tag %q: want v<version>, e.g. v3.2.0", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	if channel == "" {
		channel = selfupdate.ChannelStable
		if strings.Contains(strings.SplitN(version, "+", 2)[0], "-") {
			channel = selfupdate.ChannelBeta
		}
	}
	if channel != selfupdate.ChannelStable && channel != selfupdate.ChannelBeta {
		return nil, fmt.Errorf("-channel %q: want stable or beta", channel)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("package directory %s does not exist", dir)
	}
	m, err := selfupdate.ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("update.json does not parse: %w", err)
	}
	if m.Version != version {
		return nil, fmt.Errorf("version: update.json says %q, the tag %s means %q", m.Version, tag, version)
	}
	if m.Channel != channel {
		return nil, fmt.Errorf("channel: update.json says %q, expected %q for %s", m.Channel, channel, tag)
	}
	if m.NotesURL != "" && !strings.HasPrefix(m.NotesURL, "https://github.com/"+selfupdate.Repo+"/") {
		return nil, fmt.Errorf("notes_url %q is not on https://github.com/%s/", m.NotesURL, selfupdate.Repo)
	}
	if len(m.Mirrors) > 0 && !allowMirrors {
		return nil, fmt.Errorf("mirrors: update.json has %d mirror rule(s) (first %s -> %s); CI makes none, sign them only on purpose (-allow-mirrors)",
			len(m.Mirrors), m.Mirrors[0].Prefix, m.Mirrors[0].Replace)
	}

	// The packages expected for this version, by platform and kind.
	type slot struct{ platform, kind string }
	names := map[slot][]string{}
	for _, p := range packageNames(version) {
		names[slot{p.platform, p.kind}] = p.names
	}
	prefix := releaseDownloadPrefix(tag)
	res := &verifyResult{manifest: m}
	listed := map[slot]bool{}
	platforms := make([]string, 0, len(m.Platforms))
	for p := range m.Platforms {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	for _, p := range platforms {
		for i, a := range m.Platforms[p] {
			field := fmt.Sprintf("platforms.%s[%d] (%s)", p, i, a.Name)
			s := slot{p, a.Kind}
			cands, ok := names[s]
			if !ok {
				return nil, fmt.Errorf("%s: unexpected package %s/%s", field, p, a.Kind)
			}
			known := false
			for _, n := range cands {
				known = known || a.Name == n
			}
			if !known {
				return nil, fmt.Errorf("%s: name %q, expected %s", field, a.Name, strings.Join(cands, " or "))
			}
			if want := prefix + url.PathEscape(a.Name); a.URL != want {
				return nil, fmt.Errorf("%s: url %q, expected %q", field, a.URL, want)
			}
			sum, size, err := localPackage(filepath.Join(dir, a.Name))
			if err != nil {
				return nil, fmt.Errorf("%s: listed in update.json but %v", field, err)
			}
			if size != a.Size {
				return nil, fmt.Errorf("%s: size %d in update.json, the local file has %d bytes", field, a.Size, size)
			}
			if sum != a.SHA256 {
				return nil, fmt.Errorf("%s: sha256 %s in update.json, the local file has %s", field, a.SHA256, sum)
			}
			listed[s] = true
			res.assets = append(res.assets, verifiedAsset{p, a})
		}
	}
	// Every package must be listed, and nothing in dir may be left out.
	for _, p := range packageNames(version) {
		s := slot{p.platform, p.kind}
		var present []string
		for _, n := range p.names {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil && fi.Mode().IsRegular() {
				present = append(present, n)
			}
		}
		if !listed[s] {
			if len(present) > 0 {
				return nil, fmt.Errorf("%s is in %s but update.json lists no %s/%s package", present[0], dir, p.platform, p.kind)
			}
			return nil, fmt.Errorf("update.json lists no %s/%s package (%s)", p.platform, p.kind, p.names[0])
		}
		if len(present) > 1 {
			return nil, fmt.Errorf("%s holds both %s: keep only the one update.json lists", dir, strings.Join(present, " and "))
		}
	}
	return res, nil
}

// localPackage → SHA-256 and size of a regular file.
func localPackage(p string) (string, int64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return "", 0, fmt.Errorf("%s is not in the package directory", filepath.Base(p))
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", p)
	}
	return hashFile(p)
}

func runVerify(manifestPath, dir, tag, channel string, allowMirrors bool, stdout, stderr io.Writer) int {
	if manifestPath == "" || dir == "" || tag == "" {
		fmt.Fprintln(stderr, "make-update-json: -verify needs -dir and -tag")
		return 2
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "make-update-json: REFUSED:", err)
		return 1
	}
	res, err := verifyManifest(data, dir, tag, channel, allowMirrors)
	if err != nil {
		fmt.Fprintf(stderr, "make-update-json: REFUSED: %s: %v\n", manifestPath, err)
		return 1
	}
	m := res.manifest
	fmt.Fprintf(stdout, "%s: LectureFlow %s (%s) for tag %s, released_at %s", manifestPath, m.Version, m.Channel, tag, m.ReleasedAt)
	if m.MinVersion != "" {
		fmt.Fprintf(stdout, ", min_version %s", m.MinVersion)
	}
	fmt.Fprintln(stdout)
	for _, a := range res.assets {
		fmt.Fprintf(stdout, "  %-12s %-9s %s  %d bytes  sha256 %s  matches %s\n", a.platform, a.Kind, a.Name, a.Size, a.SHA256, filepath.Join(dir, a.Name))
	}
	for _, r := range m.Mirrors {
		fmt.Fprintf(stdout, "  MIRROR RULE (allowed by -allow-mirrors; signed, downloads are still checked by SHA-256): %s -> %s\n", r.Prefix, r.Replace)
	}
	sum := sha256.Sum256(data)
	fmt.Fprintf(stdout, "checked update.json sha256 %s\n", hex.EncodeToString(sum[:]))
	fmt.Fprintf(stdout, "verified: %d packages match update.json; every URL is under %s\n", len(res.assets), releaseDownloadPrefix(tag))
	return 0
}
