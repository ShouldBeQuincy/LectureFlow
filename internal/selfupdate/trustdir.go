// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lectureflow/internal/dirperm"
)

// The shell's record outside the data directory (security review SEC17,
// 中-B). Everything a rollback believes rests on the desktop shell's record
// of the installation it started and of the backups' hashes. While that
// record lay in the shell's profile, and the profile in the data directory,
// an account that could once write there could prepare it together with
// the rest of the rollback state and remove every trace of itself before
// the user looked: tightening the permissions afterwards cannot tell. On
// Windows the shell therefore keeps the record in a directory of its own
// outside the data directory, %LOCALAPPDATA%\LectureFlow\selfupdate
// (desktop/src/main/lib/updatefiles.js trustDirFor), names that directory
// to -selfupdate-verify (-selfupdate-trust-dir) and believes the record
// only while CheckTrustDir vouches for it. Elsewhere (macOS: no Mac to
// test on) the record stays in the profile and CheckTrust looks at it
// there.

// trustDirTree bounds the walk of the trust directory: a few small files.
var trustDirTree = dirperm.TreeOptions{MaxEntries: 1000, MaxDepth: 4}

// CheckTrustDir reports selfupdate.untrusted_dir {path, who} when the
// directory trustDir, where the desktop shell keeps its record, cannot
// vouch for it:
//
//   - it is not an absolute path;
//   - it lies in the data directory dataDir, or dataDir lies in it —
//     compared as given and once more with links resolved (the record must
//     never be something the data directory's permissions decide);
//   - it is a link (a junction, a symbolic link, another reparse point);
//   - another account of this computer can change it or anything in it
//     (dirperm.Check, and a bounded dirperm.CheckTree that also refuses
//     links below it).
//
// A missing trustDir is made first, with private permissions
// (dirperm.MkdirPrivate; its parent with the usual ones) — never inside
// the data directory, which is looked at before. Other errors: CodeIO; the
// caller treats any error as "not vouched for".
func CheckTrustDir(trustDir, dataDir string) error {
	untrusted := func(p, who string) error {
		return cerr(CodeUntrustedDir, map[string]any{"path": p, "who": who},
			fmt.Errorf("the update's trust directory %s cannot vouch for the record (%s)", p, who))
	}
	if trustDir == "" || !filepath.IsAbs(trustDir) {
		return untrusted(trustDir, "not an absolute path")
	}
	trustDir = filepath.Clean(trustDir)
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return ioErr(err)
	}
	// The desktop shell recognises this reason by its words, at the end of
	// the -selfupdate-verify output (desktop/src/main/lib/verifyupdate.js,
	// OVERLAP_RE and OVERLAP_DIR_RE, which also read "trust directory <path>
	// cannot vouch for the record ("): it then says to move the data folder
	// instead of tightening its permissions. Changing the wording here means
	// changing those too (cmd/lectureflow TestSelfUpdateVerifyTrustDir pins
	// it; R47 S2).
	const inData = "it lies in the data directory, or the data directory lies in it"
	if overlaps(trustDir, data) {
		return untrusted(trustDir, inData)
	}
	if _, err := dirperm.MkdirPrivate(trustDir); err != nil {
		return ioErr(err)
	}
	fi, err := os.Lstat(trustDir)
	if err != nil {
		return ioErr(err)
	}
	if !isPlainDir(fi) {
		return untrusted(trustDir, "not confirmed: it is a link (a junction, a symbolic link or another reparse point)")
	}
	realTrust, err1 := filepath.EvalSymlinks(trustDir)
	realData, err2 := filepath.EvalSymlinks(data)
	if err := errors.Join(err1, err2); err != nil {
		return ioErr(err)
	}
	if overlaps(realTrust, realData) {
		return untrusted(trustDir, inData)
	}
	e, err := dirperm.Check(trustDir)
	if err != nil {
		return ioErr(err)
	}
	if e.Write {
		return untrusted(trustDir, strings.Join(e.Who, ", "))
	}
	rep, err := dirperm.CheckTree(trustDir, trustDirTree)
	if err != nil {
		return ioErr(err)
	}
	if !rep.OK() {
		p, who := rep.Problem()
		if p == "" {
			p = trustDir
		}
		return untrusted(p, who)
	}
	return nil
}

// overlaps: a and b (absolute, clean) are the same directory or one lies
// in the other; without regard to case on Windows and macOS, whose file
// systems usually ignore it (the safe side: more is refused).
func overlaps(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	return within(a, b) || within(b, a)
}

// within: p is dir or lies in it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false // another volume
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
