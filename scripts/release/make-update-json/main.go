// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command make-update-json writes the (unsigned) update.json of a release
// from the release files: size and SHA-256 of each package, GitHub
// Releases download URLs (format: internal/selfupdate/README.md). CI runs
// it; the maintainer signs the result with sign-update.ps1 / sign-update.sh.
//
//	go run ./scripts/release/make-update-json -dir out -version 3.2.0 -repo OWNER/REPO
//	    [-tag v3.2.0] [-channel stable|beta] [-released-at 2026-11-02T10:00:00Z]
//	    [-min-version 3.0.0] [-notes-url URL] [-mirror PREFIX=REPLACE ...]
//	    [-allow-missing] [-out DIR/update.json]
//	go run ./scripts/release/make-update-json -verify update.json -dir DIR -tag v3.2.0 [-channel stable|beta] [-allow-mirrors]
//
// -verify checks an update.json against local packages before it is signed
// (verify.go); sign-update.sh / sign-update.ps1 run it.
//
// The packages are found by their names:
//
//	windows-x64  installer  LectureFlow-Setup-<version>-x64.exe
//	windows-x64  portable   LectureFlow-<version>-x64-portable.exe
//	darwin-arm64 dmg        LectureFlow-<version>-arm64.dmg
//	darwin-arm64 zip        LectureFlow-<version>-arm64-mac.zip (or -arm64.zip)
//
// All four must be there unless -allow-missing (then at least one).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lectureflow/internal/selfupdate"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type mirrorFlags []selfupdate.MirrorRule

func (m *mirrorFlags) String() string { return fmt.Sprint(*m) }

func (m *mirrorFlags) Set(v string) error {
	prefix, replace, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("want PREFIX=REPLACE")
	}
	*m = append(*m, selfupdate.MirrorRule{Prefix: prefix, Replace: replace})
	return nil
}

type options struct {
	dir, version, repo, tag, channel, releasedAt, minVersion, notesURL, out string
	mirrors                                                                 []selfupdate.MirrorRule
	allowMissing                                                            bool
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("make-update-json", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	var mirrors mirrorFlags
	fs.StringVar(&o.dir, "dir", "", "directory with the release files (required)")
	fs.StringVar(&o.version, "version", "", "release version without v, e.g. 3.2.0 (required)")
	fs.StringVar(&o.repo, "repo", "", "GitHub repository OWNER/NAME (required)")
	fs.StringVar(&o.tag, "tag", "", "release tag (default v<version>)")
	fs.StringVar(&o.channel, "channel", "", "stable or beta (default: beta for a pre-release version)")
	fs.StringVar(&o.releasedAt, "released-at", "", "RFC 3339 time (default now)")
	fs.StringVar(&o.minVersion, "min-version", "", "oldest version that may update directly (optional)")
	fs.StringVar(&o.notesURL, "notes-url", "", "release notes URL (default the release page)")
	fs.Var(&mirrors, "mirror", "mirror rule PREFIX=REPLACE (repeatable)")
	fs.BoolVar(&o.allowMissing, "allow-missing", false, "accept a release without every package")
	fs.StringVar(&o.out, "out", "", "output file (default <dir>/update.json)")
	allowMirrors := fs.Bool("allow-mirrors", false, "with -verify: accept mirror rules in update.json (they are printed)")
	verify := fs.String("verify", "", "check this update.json against the packages in -dir and -tag instead of writing one (before signing)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *verify != "" {
		if o.version != "" || o.repo != "" || fs.NArg() > 0 {
			fmt.Fprintln(stderr, "make-update-json: -verify takes -dir, -tag and -channel only (the repository is selfupdate.Repo)")
			return 2
		}
		return runVerify(*verify, o.dir, o.tag, o.channel, *allowMirrors, stdout, stderr)
	}
	if o.dir == "" || o.version == "" || o.repo == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "make-update-json: -dir, -version and -repo are required")
		return 2
	}
	o.mirrors = mirrors
	m, b, err := build(o, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "make-update-json:", err)
		return 1
	}
	out := o.out
	if out == "" {
		out = filepath.Join(o.dir, "update.json")
	}
	if err := writeAtomic(out, b); err != nil {
		fmt.Fprintln(stderr, "make-update-json:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: LectureFlow %s (%s), unsigned\n", out, m.Version, m.Channel)
	for _, p := range []string{selfupdate.PlatformWindowsX64, selfupdate.PlatformDarwinArm64} {
		for _, a := range m.Platforms[p] {
			fmt.Fprintf(stdout, "  %-12s %-9s %s  %d bytes  sha256 %s\n", p, a.Kind, a.Name, a.Size, a.SHA256)
		}
	}
	return 0
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// packageNames are the candidate file names of each package.
func packageNames(v string) []struct {
	platform, kind string
	names          []string
} {
	return []struct {
		platform, kind string
		names          []string
	}{
		{selfupdate.PlatformWindowsX64, selfupdate.KindInstaller, []string{"LectureFlow-Setup-" + v + "-x64.exe"}},
		{selfupdate.PlatformWindowsX64, selfupdate.KindPortable, []string{"LectureFlow-" + v + "-x64-portable.exe"}},
		{selfupdate.PlatformDarwinArm64, selfupdate.KindDMG, []string{"LectureFlow-" + v + "-arm64.dmg"}},
		{selfupdate.PlatformDarwinArm64, selfupdate.KindZip, []string{"LectureFlow-" + v + "-arm64-mac.zip", "LectureFlow-" + v + "-arm64.zip"}},
	}
}

// build makes update.json; it is checked with the application's own parser.
func build(o options, now time.Time) (*selfupdate.Manifest, []byte, error) {
	if !repoRe.MatchString(o.repo) {
		return nil, nil, fmt.Errorf("-repo %q: want OWNER/NAME", o.repo)
	}
	if o.tag == "" {
		o.tag = "v" + o.version
	}
	if o.channel == "" {
		o.channel = selfupdate.ChannelStable
		if strings.Contains(strings.SplitN(o.version, "+", 2)[0], "-") {
			o.channel = selfupdate.ChannelBeta
		}
	}
	if o.releasedAt == "" {
		o.releasedAt = now.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	if o.notesURL == "" {
		o.notesURL = "https://github.com/" + o.repo + "/releases/tag/" + url.PathEscape(o.tag)
	}
	m := &selfupdate.Manifest{Schema: selfupdate.SchemaVersion, App: selfupdate.AppName, Version: o.version,
		Channel: o.channel, ReleasedAt: o.releasedAt, MinVersion: o.minVersion, NotesURL: o.notesURL,
		Platforms: map[string][]selfupdate.Asset{}, Mirrors: o.mirrors}
	var missing []string
	for _, p := range packageNames(o.version) {
		found := ""
		for _, n := range p.names {
			if fi, err := os.Stat(filepath.Join(o.dir, n)); err == nil && fi.Mode().IsRegular() {
				found = n
				break
			}
		}
		if found == "" {
			missing = append(missing, p.names[0])
			continue
		}
		sum, size, err := hashFile(filepath.Join(o.dir, found))
		if err != nil {
			return nil, nil, err
		}
		m.Platforms[p.platform] = append(m.Platforms[p.platform], selfupdate.Asset{Kind: p.kind, Name: found,
			URL:  "https://github.com/" + o.repo + "/releases/download/" + url.PathEscape(o.tag) + "/" + url.PathEscape(found),
			Size: size, SHA256: sum})
	}
	if len(missing) > 0 && (!o.allowMissing || len(m.Platforms) == 0) {
		return nil, nil, fmt.Errorf("missing in %s: %s", o.dir, strings.Join(missing, ", "))
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	b = append(b, '\n')
	if _, err := selfupdate.ParseManifest(b); err != nil {
		return nil, nil, fmt.Errorf("the result is not a valid update.json: %w", err)
	}
	return m, b, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
