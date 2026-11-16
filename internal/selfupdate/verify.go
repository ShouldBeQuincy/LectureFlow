// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"lectureflow/internal/dirperm"
)

// Verifying an update package again just before the desktop shell runs it
// (the command line's -selfupdate-verify; README.md "What Electron does at
// start"): the files under the updates directory could have been changed
// since Go accepted them, so the signature of the version's update.json,
// the package and the JSON file's own claims are checked once more against
// the public key built into the application.
//
//   - VerifyPending: the update to install (pending.json).
//   - VerifyPrevious: the package of the version a failed update returns
//     to (previous.json "file"; security review sec-update M2): the
//     rollback runs it, so it is held to the same standard.
//
// With VerifyOptions.CopyDir the package is copied, while it is hashed,
// into a new directory only this account can use (dirperm), and the shell
// runs that copy (M3: nothing can swap the file between the check and the
// start). Nothing else is written, downloaded or removed.

// ErrNotConfigured: the application was built without a public key, so
// nothing can be verified (VerifyPending, VerifyPrevious).
var ErrNotConfigured = errors.New("selfupdate: no public key built in")

// CopyPrefix starts the name of the private directory a copy goes to
// (VerifyOptions.CopyDir/<CopyPrefix><random>/<package name>).
const CopyPrefix = "lectureflow-update-"

// VerifyOptions configure VerifyPending and VerifyPrevious.
type VerifyOptions struct {
	// PublicKey is the minisign public key ("" = ErrNotConfigured);
	// BuiltinPublicKey is the application's.
	PublicKey string
	// Current is the running version: a pending update must be newer, a
	// previous version older. "" or not a version (a development build)
	// skips this check.
	Current string
	// Platform is the installation's platform ("" = this system's).
	Platform string
	// CopyDir, when set, is where a new private directory receives a copy
	// of the package (Vouched.Copy); usually the system's temporary
	// directory.
	CopyDir string
}

// Vouched is the package VerifyPending or VerifyPrevious vouches for.
type Vouched struct {
	Version   string
	Kind      string
	File      string // relative to the updates directory, "/"-separated
	Size      int64
	SHA256    string
	Manifest  string // the signed update.json (relative)
	Signature string // its signature (relative)
	// Copy is the absolute path of the private copy (VerifyOptions.CopyDir),
	// "" without one.
	Copy string
}

// BuiltinPublicKey is the public key compiled into the application
// (PublicKey; "" = updates are not configured).
func BuiltinPublicKey() string { return builtinPublicKey }

// CurrentPlatform is this system's platform name in update.json
// (windows-x64, darwin-arm64; "" = none of them).
func CurrentPlatform() string { return currentPlatform() }

func (o VerifyOptions) key() (Key, error) {
	if o.PublicKey == "" {
		return Key{}, cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoPublicKey}, ErrNotConfigured)
	}
	pk, err := ParsePublicKey(o.PublicKey)
	if err != nil {
		return Key{}, cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoPublicKey}, fmt.Errorf("the built-in public key: %w", err))
	}
	return pk, nil
}

func (o VerifyOptions) platform() string {
	if o.Platform != "" {
		return o.Platform
	}
	return currentPlatform()
}

// readLimited reads a small file named by a JSON file of dir.
func readLimited(dir, rel, what string, limit int64) ([]byte, error) {
	f, ok := absPath(dir, rel)
	if !ok {
		return nil, cerr(CodeManifestInvalid, map[string]any{"field": what}, fmt.Errorf("%s %q leaves the updates directory", what, rel))
	}
	fh, err := os.Open(f)
	if err != nil {
		return nil, ioErr(err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err != nil {
		return nil, ioErr(err)
	}
	if int64(len(b)) > limit {
		return nil, cerr(CodeManifestInvalid, map[string]any{"field": what}, fmt.Errorf("%s is larger than %d bytes", what, limit))
	}
	return b, nil
}

// signedAsset checks the signature of version's update.json (manifest,
// signature: relative paths) and returns its asset for platform and kind.
func signedAsset(dir string, pk Key, version, manifest, signature, platform, kind string) (Asset, error) {
	raw, err := readLimited(dir, manifest, "manifest", maxManifestBytes)
	if err != nil {
		return Asset{}, err
	}
	sig, err := readLimited(dir, signature, "signature", maxSignatureBytes)
	if err != nil {
		return Asset{}, err
	}
	m, err := verifyManifest(pk, raw, sig)
	if err != nil {
		return Asset{}, err
	}
	if m.Version != version {
		return Asset{}, cerr(CodeVersionMismatch, map[string]any{"signed": m.Version, "manifest": version},
			fmt.Errorf("the JSON file names %s, the signed update.json %s", version, m.Version))
	}
	a, ok := m.Asset(platform, kind)
	if !ok {
		return Asset{}, cerr(CodeNoPackage, map[string]any{"platform": platform, "kind": kind},
			errors.New("the signed update.json has no such package"))
	}
	return a, nil
}

// checkPackage compares the claims of a JSON file (file, size, SHA-256)
// with the signed asset, then hashes the file (copying it with copyDir)
// and fills v.
func checkPackage(dir string, a Asset, file string, size int64, sum string, copyDir string, v *Vouched) error {
	if a.Size != size || a.SHA256 != sum {
		return cerr(CodeChecksum, map[string]any{"file": a.Name}, errors.New("the JSON file differs from the signed update.json"))
	}
	// The package lives in its version's directory under its signed name
	// (as the updater stores it).
	if path.Clean(file) != relPath(v.Version, a.Name) {
		return cerr(CodeManifestInvalid, map[string]any{"field": "file"}, fmt.Errorf("file %q is not %s", file, relPath(v.Version, a.Name)))
	}
	f, ok := absPath(dir, file)
	if !ok {
		return cerr(CodeManifestInvalid, map[string]any{"field": "file"}, fmt.Errorf("file %q leaves the updates directory", file))
	}
	if copyDir == "" {
		got, n, err := hashFile(f)
		if err != nil {
			return ioErr(err)
		}
		if n != a.Size || got != a.SHA256 {
			return cerr(CodeChecksum, map[string]any{"file": a.Name}, fmt.Errorf("the package has %d bytes, SHA-256 %s", n, got))
		}
	} else {
		cp, err := copyPackage(f, a, copyDir)
		if err != nil {
			return err
		}
		v.Copy = cp
	}
	v.Kind, v.File, v.Size, v.SHA256 = a.Kind, relPath(v.Version, a.Name), a.Size, a.SHA256
	return nil
}

// copyPackage copies src into a new private directory under parent while
// hashing the bytes it writes; the copy is kept only when they are the
// asset's. Returns the copy's path.
func copyPackage(src string, a Asset, parent string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", ioErr(err)
	}
	defer in.Close()
	dir, err := dirperm.MkdirTempPrivate(parent, CopyPrefix+"*")
	if err != nil {
		return "", ioErr(err)
	}
	dst := filepath.Join(dir, a.Name)
	fail := func(err error) (string, error) {
		os.RemoveAll(dir)
		return "", err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fail(ioErr(err))
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), io.LimitReader(in, a.Size+1), make([]byte, 1<<20))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(ioErr(err))
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != a.Size || got != a.SHA256 {
		return fail(cerr(CodeChecksum, map[string]any{"file": a.Name}, fmt.Errorf("the package has %d bytes, SHA-256 %s", n, got)))
	}
	return dst, nil
}

// VerifyPending checks pending.json of the updates directory dir: its
// schema; the signature of the update.json it names (trusted comment =
// update.json's version); the same version; newer than the running one;
// the platform; an asset of that platform and kind with pending.json's
// size and SHA-256, stored as <version>/<name>; and the package's bytes.
// Errors are coded like the updater's (CodeOf): CodeNotConfigured
// (ErrNotConfigured), CodeNotAvailable (no pending update),
// CodeManifestInvalid, CodeSignatureInvalid, CodeVersionMismatch,
// CodeNotNewer, CodeNoPackage, CodeChecksum, CodeIO.
func VerifyPending(dir string, o VerifyOptions) (Vouched, error) {
	var v Vouched
	pk, err := o.key()
	if err != nil {
		return v, err
	}
	var p Pending
	ok, err := readJSON(filepath.Join(dir, pendingFile), &p)
	switch {
	case !ok && err == nil:
		return v, cerr(CodeNotAvailable, nil, errors.New("no pending update (pending.json)"))
	case err != nil && !ok:
		return v, ioErr(err)
	case err != nil:
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "pending.json"}, err)
	}
	if p.Schema != fileSchema {
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "pending.json schema"}, fmt.Errorf("pending.json schema %d", p.Schema))
	}
	if _, err := parseVersion(p.Version); err != nil {
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "pending.json version"}, err)
	}
	v.Version, v.Manifest, v.Signature = p.Version, p.Manifest, p.Signature
	a, err := signedAsset(dir, pk, p.Version, p.Manifest, p.Signature, p.Platform, p.Kind)
	if err != nil {
		return v, err
	}
	if o.Current != "" {
		if _, err := parseVersion(o.Current); err == nil && compareVersions(p.Version, o.Current) <= 0 {
			return v, cerr(CodeNotNewer, map[string]any{"version": p.Version, "current": o.Current},
				fmt.Errorf("%s is not newer than %s", p.Version, o.Current))
		}
	}
	if platform := o.platform(); p.Platform != platform {
		return v, cerr(CodeNoPackage, map[string]any{"platform": p.Platform, "kind": p.Kind},
			fmt.Errorf("pending.json is for %s, this installation is %s", p.Platform, platform))
	}
	if err := checkPackage(dir, a, p.File, p.Size, p.SHA256, o.CopyDir, &v); err != nil {
		return v, err
	}
	return v, nil
}

// VerifyPrevious checks the package previous.json names, the version a
// failed update returns to: the signature of <version>/update.json (the
// updater keeps it next to the package), the version binding, older than
// the running version, the platform, an asset of that platform and kind
// with previous.json's size and SHA-256, stored as <version>/<name>, and
// the package's bytes. previous.json without "file" (only a backup of the
// installation) is CodeNotAvailable. Codes as VerifyPending.
func VerifyPrevious(dir string, o VerifyOptions) (Vouched, error) {
	var v Vouched
	pk, err := o.key()
	if err != nil {
		return v, err
	}
	var p Previous
	ok, err := readJSON(filepath.Join(dir, previousFile), &p)
	switch {
	case !ok && err == nil:
		return v, cerr(CodeNotAvailable, nil, errors.New("no previous version (previous.json)"))
	case err != nil && !ok:
		return v, ioErr(err)
	case err != nil:
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "previous.json"}, err)
	}
	if p.Schema != fileSchema {
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "previous.json schema"}, fmt.Errorf("previous.json schema %d", p.Schema))
	}
	if _, err := parseVersion(p.Version); err != nil {
		return v, cerr(CodeManifestInvalid, map[string]any{"field": "previous.json version"}, err)
	}
	if p.File == "" {
		return v, cerr(CodeNotAvailable, nil, fmt.Errorf("previous.json names no package of %s", p.Version))
	}
	v.Version = p.Version
	v.Manifest, v.Signature = relPath(p.Version, manifestFile), relPath(p.Version, signatureFile)
	a, err := signedAsset(dir, pk, p.Version, v.Manifest, v.Signature, p.Platform, p.Kind)
	if err != nil {
		return v, err
	}
	if o.Current != "" {
		if _, err := parseVersion(o.Current); err == nil && compareVersions(p.Version, o.Current) >= 0 {
			return v, cerr(CodeManifestInvalid, map[string]any{"field": "previous.json version"},
				fmt.Errorf("previous.json names %s, which is not older than %s", p.Version, o.Current))
		}
	}
	if platform := o.platform(); p.Platform != platform {
		return v, cerr(CodeNoPackage, map[string]any{"platform": p.Platform, "kind": p.Kind},
			fmt.Errorf("previous.json is for %s, this installation is %s", p.Platform, platform))
	}
	if err := checkPackage(dir, a, p.File, p.Size, p.SHA256, o.CopyDir, &v); err != nil {
		return v, err
	}
	return v, nil
}

// PrivateDir makes a new, empty directory in parent that only this account
// can use (named like the directory of a private copy: CopyPrefix and
// digits) and returns its absolute path. The desktop shell copies a backup
// of the installation into it and checks the copy's hash there before a
// rollback uses it (security audit M-3). The directory is created with its
// permissions (dirperm) and looked at once more: one that is not this
// account's alone is removed and an error. Errors: CodeIO.
func PrivateDir(parent string) (string, error) {
	if parent == "" {
		return "", ioErr(errors.New("no directory to make the private directory in"))
	}
	abs, err := filepath.Abs(parent)
	if err != nil {
		return "", ioErr(err)
	}
	dir, err := dirperm.MkdirTempPrivate(abs, CopyPrefix+"*")
	if err != nil {
		return "", ioErr(err)
	}
	rep, err := dirperm.CheckTree(dir, dirperm.TreeOptions{})
	if err == nil && !rep.OK() {
		p, who := rep.Problem()
		err = fmt.Errorf("the new directory %s is not this account's alone (%s)", strings.TrimSpace(dir+" "+p), who)
	}
	if err != nil {
		os.Remove(dir)
		return "", ioErr(err)
	}
	return dir, nil
}

// Where the desktop shell keeps its record of the installation it started
// and of the hashes of the backups it made (desktop/src/main/lib:
// datadir.js SHELL_PROFILE_DIR, updatefiles.js TRUST_FILE): in its profile,
// which lies in the data directory unless the data was moved elsewhere
// (then it stays under the user's own profile folder). On Windows the
// shell keeps it outside the data directory instead (trustdir.go,
// CheckTrustDir) and reads the profile's file only once, when it takes a
// record over from an older version; CheckTrust still looks at the
// profile's file, which that one look relies on.
const (
	ShellProfileDir = "electron"
	TrustRecordFile = "selfupdate-trust.json"
)

// updatesTree bounds the walk of the updates directory (CheckTrust): a few
// versions' packages and one backup of the installation, some thousand
// entries at most. A variable for the tests.
var updatesTree = dirperm.TreeOptions{MaxEntries: 50000, MaxDepth: 32}

// trustRecordScope narrows the walk of the shell's profile to the folder
// itself and the record in it (the browser's own files are not the
// update's business).
func trustRecordScope(rel string) dirperm.Scope {
	switch {
	case rel == "":
		return dirperm.ScopeAll
	case strings.EqualFold(rel, TrustRecordFile):
		return dirperm.ScopeSelf
	}
	return dirperm.ScopeSkip
}

// junctionAboveTop is where CheckTrust stops looking at the folders above
// a data directory that is a junction and above the folder it leads to (""
// = the root of the drive; tests stop at a folder of their own).
var junctionAboveTop = ""

// CheckTrust reports selfupdate.untrusted_dir {path, who} when another
// account of this computer may change what the in-app update relies on in
// the data directory: then nothing in it is installed or rolled back
// (sec-update M2, security audit M-3). Looked at:
//
//   - the data directory holding the updates directory dir, and dir itself
//     (dirperm.Check); a data directory that is a junction is the folder
//     it leads to, and one nobody can vouch for is refused
//     (dirperm.ResolveJunction, who "not confirmed: …");
//   - every entry below dir (dirperm.CheckTree: the packages, the JSON
//     files, the backups of the installation): a folder another account
//     made there while it could keeps that account as its owner, whatever
//     the data directory's permissions are now;
//   - the shell's profile folder and its record of the backups' hashes,
//     when they lie in the data directory.
//
// path is the first entry found, who the accounts — or, when the walk
// could not vouch for everything (a link, more entries than expected, an
// entry that cannot be examined), "not confirmed: …". A missing updates
// directory counts as its data directory. Other errors: CodeIO.
func CheckTrust(dir string) error {
	dir = filepath.Clean(dir)
	// A data directory that is a junction (security review SEC30, 低-3):
	// the link's own permissions are not the folder's. The folder it
	// leads to is checked instead when nobody else can have chosen it;
	// otherwise nothing is vouched for — as the diagnostics' row "cannot
	// be confirmed" (dirperm.ResolveJunction). Anything else: as it is.
	data := filepath.Dir(dir)
	root, _, doubt := dirperm.ResolveJunction(data, junctionAboveTop)
	if doubt != "" {
		who := dirperm.NotConfirmed + doubt
		return cerr(CodeUntrustedDir, map[string]any{"path": data, "who": who},
			fmt.Errorf("%s cannot be confirmed to be this account's alone (%s)", data, who))
	}
	for _, d := range []string{root, dir} {
		e, err := dirperm.Check(d)
		if err != nil {
			if d == dir && isNotExist(err) {
				continue
			}
			return ioErr(err)
		}
		if e.Write {
			who := strings.Join(e.Who, ", ")
			return cerr(CodeUntrustedDir, map[string]any{"path": d, "who": who},
				fmt.Errorf("other accounts of this computer can change %s (%s)", d, who))
		}
	}
	for _, t := range []struct {
		root string
		o    dirperm.TreeOptions
	}{
		{dir, updatesTree},
		{filepath.Join(filepath.Dir(dir), ShellProfileDir), dirperm.TreeOptions{Scope: trustRecordScope}},
	} {
		rep, err := dirperm.CheckTree(t.root, t.o)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return ioErr(err)
		}
		if rep.OK() {
			continue
		}
		p, who := rep.Problem()
		if p == "" {
			// unconfirmed: who says why and names the entry
			return cerr(CodeUntrustedDir, map[string]any{"path": t.root, "who": who},
				fmt.Errorf("%s cannot be confirmed to be this account's alone (%s)", t.root, who))
		}
		return cerr(CodeUntrustedDir, map[string]any{"path": p, "who": who},
			fmt.Errorf("other accounts of this computer can change %s (%s)", p, who))
	}
	return nil
}
