// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Offline update ("从文件更新", README.md "离线更新"): the user gives
// update.json, update.json.minisig and the package, or one zip holding the
// three. Everything works without a network. The files are first copied
// into a private staging directory under updates/ (the user's files are
// read once), identified by content rather than by name (a browser may
// have renamed "update.json" to "update (1).json"), and accepted only
// after the same checks as a download: signature, version binding, newer
// than this version, size and SHA-256 of the package.

// MaxImportFiles is the most files one offline update may consist of.
const MaxImportFiles = 3

// ImportTooMany is the error for a file beyond MaxImportFiles.
func ImportTooMany(name string) error {
	return cerr(CodeImportInvalid, map[string]any{"file": filepath.Base(name)}, fmt.Errorf("at most %d files", MaxImportFiles))
}

// ImportPaths imports an offline update from local files (the desktop
// page's file picker or drop). The paths were checked by the caller
// (absolute, regular files).
func (u *Updater) ImportPaths(ctx context.Context, paths []string) (Status, error) {
	return u.ImportWith(ctx, func(dir string) error {
		if len(paths) == 0 {
			return cerr(CodeImportIncomplete, map[string]any{"missing": "manifest"}, errors.New("no file"))
		}
		if len(paths) > MaxImportFiles {
			return cerr(CodeImportInvalid, map[string]any{"file": filepath.Base(paths[MaxImportFiles])},
				fmt.Errorf("at most %d files", MaxImportFiles))
		}
		for i, p := range paths {
			if err := copyInto(ctx, dir, i, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// copyInto copies the user's file p into the staging directory.
func copyInto(ctx context.Context, dir string, i int, p string) error {
	in, err := os.Open(p)
	if err != nil {
		return cerr(CodeImportInvalid, map[string]any{"file": filepath.Base(p)}, err)
	}
	defer in.Close()
	dst := filepath.Join(dir, StagingName(i, filepath.Base(p)))
	_, _, err = copyHash(dst, ctxReader{ctx, in}, maxPackageBytes)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return cerr(CodeCancelled, nil, ctx.Err())
	case errors.Is(err, errTooLarge):
		return cerr(CodeImportInvalid, map[string]any{"file": filepath.Base(p)}, err)
	}
	var le *localError
	if errors.As(err, &le) {
		return ioErr(le.err)
	}
	return cerr(CodeImportInvalid, map[string]any{"file": filepath.Base(p)}, err)
}

// StagingName is the name file i of an offline update gets in the staging
// directory (its own base name, made safe and unique).
func StagingName(i int, name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, path.Base(strings.ReplaceAll(name, "\\", "/")))
	name = strings.TrimLeft(name, ". ")
	if len(name) > 120 {
		// the last 120 bytes, from the start of a character
		cut := len(name) - 120
		for cut < len(name) && !utf8.RuneStart(name[cut]) {
			cut++
		}
		name = name[cut:]
	}
	if name == "" {
		name = "file"
	}
	return fmt.Sprintf("%d-%s", i, name)
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// ImportWith runs an offline update: fill puts the user's files (at most
// MaxImportFiles, named with StagingName) into a fresh staging directory,
// then they are verified and the package becomes the pending update. The
// staging directory is removed afterwards. Errors: not_configured, busy,
// untrusted_dir, untrusted_install_dir, other_copy_pending (before
// anything is staged), and the import's own (signature_invalid, version_mismatch,
// manifest_invalid, not_newer, too_old, no_package, checksum,
// import_incomplete, import_invalid, io_failed); fill's errors as they are.
func (u *Updater) ImportWith(ctx context.Context, fill func(dir string) error) (Status, error) {
	if err := u.begin(opImport); err != nil {
		return u.Status(ctx), err
	}
	err := func() error {
		if err := u.untrusted(); err != nil {
			return err
		}
		if err := u.otherCopyPending(); err != nil {
			return err
		}
		if u.opt.NoInstall {
			// (a copy that cannot update itself: before anything is staged)
			return cerr(CodeNoPackage, map[string]any{"platform": u.platform, "kind": u.kind}, nil)
		}
		if err := os.MkdirAll(u.dir, 0o755); err != nil {
			return ioErr(err)
		}
		dir, err := os.MkdirTemp(u.dir, importPrefix)
		if err != nil {
			return ioErr(err)
		}
		defer os.RemoveAll(dir)
		if err := fill(dir); err != nil {
			return err
		}
		return u.importDir(ctx, dir)
	}()
	if err != nil {
		u.log.Info("selfupdate: offline update refused", "error", err)
	}
	u.mu.Lock()
	u.op = ""
	if err == nil {
		// like end(): the update is ready, an earlier failed check or
		// download no longer describes the state (its outcome is the error
		// returned; the status of a refused import is unchanged)
		u.lastErr = nil
	}
	u.mu.Unlock()
	u.changed()
	return u.Status(ctx), err
}

// staged is one file of the staging directory.
type staged struct {
	path string
	name string // the user's name (staging prefix removed)
	size int64
}

func (u *Updater) importDir(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ioErr(err)
	}
	var files []staged
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return ioErr(err)
		}
		name := e.Name()
		if _, rest, ok := strings.Cut(name, "-"); ok {
			name = rest
		}
		files = append(files, staged{path: filepath.Join(dir, e.Name()), name: name, size: fi.Size()})
	}
	switch {
	case len(files) == 0:
		return cerr(CodeImportIncomplete, map[string]any{"missing": "manifest"}, errors.New("no file"))
	case len(files) > MaxImportFiles:
		return cerr(CodeImportInvalid, map[string]any{"file": files[MaxImportFiles].name}, fmt.Errorf("at most %d files", MaxImportFiles))
	}
	if len(files) == 1 {
		if handled, err := u.importBundle(ctx, dir, files[0]); handled {
			return err
		}
	}
	var man, sig *staged
	var pkgs []staged
	for i := range files {
		f := &files[i]
		switch sniff(f.path, f.name, f.size) {
		case "signature":
			if sig != nil {
				return cerr(CodeImportInvalid, map[string]any{"file": f.name}, errors.New("two signature files"))
			}
			sig = f
		case "manifest":
			if man != nil {
				return cerr(CodeImportInvalid, map[string]any{"file": f.name}, errors.New("two update.json files"))
			}
			man = f
		default:
			pkgs = append(pkgs, *f)
		}
	}
	switch {
	case len(pkgs) > 1:
		return cerr(CodeImportInvalid, map[string]any{"file": pkgs[1].name}, errors.New("two packages"))
	case man == nil:
		return cerr(CodeImportIncomplete, map[string]any{"missing": "manifest"}, errors.New("no update.json"))
	case sig == nil:
		return cerr(CodeImportIncomplete, map[string]any{"missing": "signature"}, errors.New("no update.json.minisig"))
	case len(pkgs) == 0:
		return cerr(CodeImportIncomplete, map[string]any{"missing": "package"}, errors.New("no package"))
	}
	raw, err := os.ReadFile(man.path)
	if err != nil {
		return ioErr(err)
	}
	sigData, err := os.ReadFile(sig.path)
	if err != nil {
		return ioErr(err)
	}
	m, err := u.verifyImport(raw, sigData)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return cerr(CodeCancelled, nil, err)
	}
	sum, n, err := hashFile(pkgs[0].path)
	if err != nil {
		return ioErr(err)
	}
	a, err := u.matchPackage(m, pkgs[0].name, sum, n)
	if err != nil {
		return err
	}
	return u.acceptImport(m, raw, sigData, pkgs[0].path, a)
}

// sniff tells the two small files apart, by content first, then by name
// (a damaged update.json is still recognized, and then refused by the
// signature check rather than taken for a package).
func sniff(p, name string, size int64) string {
	if size > maxManifestBytes {
		return "package"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "package"
	}
	lower := strings.ToLower(name)
	if size <= maxSignatureBytes && (bytes.HasPrefix(b, []byte(untrustedPrefix)) || strings.HasSuffix(lower, ".minisig")) {
		return "signature"
	}
	t := bytes.TrimLeft(b, " \t\r\n\xef\xbb\xbf")
	if (len(t) > 0 && t[0] == '{' && bytes.Contains(b, []byte(`"schema"`))) || strings.HasSuffix(lower, ".json") {
		return "manifest"
	}
	return "package"
}

// verifyImport checks the signature and that the version is acceptable.
func (u *Updater) verifyImport(raw, sigData []byte) (*Manifest, error) {
	m, err := verifyManifest(u.pk, raw, sigData)
	if err != nil {
		return nil, err
	}
	if compareVersions(m.Version, u.opt.Version) <= 0 {
		return nil, cerr(CodeNotNewer, map[string]any{"version": m.Version, "current": u.opt.Version},
			fmt.Errorf("version %s is not newer than %s", m.Version, u.opt.Version))
	}
	if !u.direct(m) {
		return nil, cerr(CodeTooOld, map[string]any{"min_version": m.MinVersion, "current": u.opt.Version}, nil)
	}
	return m, nil
}

// matchPackage finds this installation's asset with the package's size and
// SHA-256.
func (u *Updater) matchPackage(m *Manifest, name, sum string, size int64) (Asset, error) {
	var found bool
	for _, k := range u.importKinds() {
		a, ok := m.Asset(u.platform, k)
		if !ok {
			continue
		}
		found = true
		if a.Size == size && a.SHA256 == sum {
			return a, nil
		}
	}
	if !found {
		return Asset{}, cerr(CodeNoPackage, map[string]any{"platform": u.platform, "kind": u.kind}, nil)
	}
	return Asset{}, cerr(CodeChecksum, map[string]any{"file": name},
		fmt.Errorf("%s: %d bytes, SHA-256 %s: no %s package of update.json has them", name, size, sum, u.platform))
}

// acceptImport moves a verified offline update into updates/<version>/ and
// makes it pending.
func (u *Updater) acceptImport(m *Manifest, raw, sigData []byte, pkg string, a Asset) error {
	dir := filepath.Join(u.dir, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, manifestFile), raw); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, signatureFile), sigData); err != nil {
		return ioErr(err)
	}
	if err := renameRetry(pkg, filepath.Join(dir, a.Name)); err != nil {
		return ioErr(err)
	}
	os.Remove(filepath.Join(dir, a.Name+".part"))
	return u.makePending(m, a, OriginFile)
}

// importBundle handles a single zip holding update.json,
// update.json.minisig and the package. handled is false when the file is
// not such a zip (then it is treated as a lone package).
func (u *Updater) importBundle(ctx context.Context, dir string, f staged) (bool, error) {
	zr, err := zip.OpenReader(f.path)
	if err != nil {
		return false, nil
	}
	defer zr.Close()
	var man, sig *zip.File
	var others []*zip.File
	for _, e := range zr.File {
		if e.FileInfo().IsDir() {
			continue
		}
		base := path.Base(strings.ReplaceAll(e.Name, "\\", "/"))
		switch strings.ToLower(base) {
		case manifestFile:
			man = e
		case signatureFile:
			sig = e
		default:
			if !strings.HasPrefix(base, ".") && !strings.HasPrefix(e.Name, "__MACOSX/") {
				others = append(others, e)
			}
		}
	}
	if man == nil && sig == nil {
		return false, nil // a package that happens to be a zip (the macOS one)
	}
	if man == nil {
		return true, cerr(CodeImportIncomplete, map[string]any{"missing": "manifest"}, errors.New("the zip has no update.json"))
	}
	if sig == nil {
		return true, cerr(CodeImportIncomplete, map[string]any{"missing": "signature"}, errors.New("the zip has no update.json.minisig"))
	}
	raw, err := readZipEntry(man, maxManifestBytes)
	if err != nil {
		return true, cerr(CodeImportInvalid, map[string]any{"file": f.name}, err)
	}
	sigData, err := readZipEntry(sig, maxSignatureBytes)
	if err != nil {
		return true, cerr(CodeImportInvalid, map[string]any{"file": f.name}, err)
	}
	m, err := u.verifyImport(raw, sigData)
	if err != nil {
		return true, err
	}
	// the package: the entry named like one of this installation's assets,
	// else the only other file
	var pick *zip.File
	var want Asset
	for _, k := range u.importKinds() {
		a, ok := m.Asset(u.platform, k)
		if !ok {
			continue
		}
		for _, e := range others {
			if path.Base(strings.ReplaceAll(e.Name, "\\", "/")) == a.Name {
				pick, want = e, a
				break
			}
		}
		if pick != nil {
			break
		}
	}
	if pick == nil {
		if len(others) != 1 {
			if len(others) == 0 {
				return true, cerr(CodeImportIncomplete, map[string]any{"missing": "package"}, errors.New("the zip has no package"))
			}
			return true, cerr(CodeImportInvalid, map[string]any{"file": f.name}, errors.New("the zip has several files besides update.json"))
		}
		pick = others[0]
	}
	limit := want.Size
	if limit == 0 {
		limit = maxPackageBytes
	}
	rc, err := pick.Open()
	if err != nil {
		return true, cerr(CodeImportInvalid, map[string]any{"file": f.name}, err)
	}
	name := path.Base(strings.ReplaceAll(pick.Name, "\\", "/"))
	out := filepath.Join(dir, "pkg-"+StagingName(0, name))
	sum, n, err := copyHash(out, ctxReader{ctx, rc}, limit)
	rc.Close()
	if err != nil {
		if ctx.Err() != nil {
			return true, cerr(CodeCancelled, nil, ctx.Err())
		}
		if errors.Is(err, errTooLarge) {
			return true, cerr(CodeChecksum, map[string]any{"file": name}, err)
		}
		var le *localError
		if errors.As(err, &le) {
			return true, ioErr(le.err)
		}
		return true, cerr(CodeImportInvalid, map[string]any{"file": f.name}, err)
	}
	a, err := u.matchPackage(m, name, sum, n)
	if err != nil {
		os.Remove(out)
		return true, err
	}
	return true, u.acceptImport(m, raw, sigData, out, a)
}

func readZipEntry(e *zip.File, limit int64) ([]byte, error) {
	rc, err := e.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", e.Name, limit)
	}
	return b, nil
}
