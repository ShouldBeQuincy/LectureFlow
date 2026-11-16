// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"strings"

	"lectureflow/internal/model"
)

// Error codes of this package (SPEC-v2 P14.3): every error the Updater
// returns or shows in Status.Error is a *model.CodedError with one of these
// codes; the front end shows t("errors."+code, params). Catalog:
// testdata/api/error_codes_backend.json; texts: web/i18n/_parts/ext-update.*.
const (
	// Preconditions (HTTP 409).
	CodeNotConfigured = "selfupdate.not_configured" // reason: no_public_key | dev_build | unsupported_platform | web_mode | no_source
	CodeBusy          = "selfupdate.busy"           // op: check | download | import
	CodeNotAvailable  = "selfupdate.not_available"  // no newer version is known (download)
	CodeTooOld        = "selfupdate.too_old"        // min_version, current: this version cannot update directly
	CodeNoPackage     = "selfupdate.no_package"     // platform, kind: the release has no package for this installation

	// Checking (Status.Error after a check; Detail lists every source's answer).
	CodeUnreachable      = "selfupdate.unreachable"       // no source answered
	CodeNoManifest       = "selfupdate.no_manifest"       // a source answered, but its latest release has no update.json or no signature yet (HTTP 404)
	CodeSignatureInvalid = "selfupdate.signature_invalid" // the signature does not verify with the built-in key
	CodeVersionMismatch  = "selfupdate.version_mismatch"  // signed, manifest: the trusted comment names another version
	CodeManifestInvalid  = "selfupdate.manifest_invalid"  // field: update.json is malformed or of a newer schema

	// Downloading.
	CodeDownloadFailed = "selfupdate.download_failed" // every download URL failed
	CodeChecksum       = "selfupdate.checksum"        // file: size or SHA-256 differs from the signed update.json
	CodeCancelled      = "selfupdate.cancelled"       // the download was cancelled
	CodeIO             = "selfupdate.io_failed"       // a local file operation failed (Detail = the error)

	// Offline update ("从文件更新").
	CodeNotNewer         = "selfupdate.not_newer"         // version, current: the package is not newer than this version
	CodeImportIncomplete = "selfupdate.import_incomplete" // missing: manifest | signature | package
	CodeImportInvalid    = "selfupdate.import_invalid"    // file: unreadable, or one file too many

	// Settings (HTTP 400).
	CodeSourcesInvalid = "selfupdate.sources_invalid" // index: the extra source at this position (0-based) is not a usable URL

	// The data directory (HTTP 409; sec-update M2): another account of this
	// computer may change it, so no update is downloaded, imported,
	// installed or rolled back from it (CheckTrust).
	CodeUntrustedDir = "selfupdate.untrusted_dir" // path, who

	// The installation directory (HTTP 409; security audit M-4): another
	// account of this computer may change LectureFlow's own program files
	// there (or move the folder away), so no update is downloaded,
	// imported, installed or rolled back in the app; the user installs the
	// new version by hand (CheckInstallDir).
	CodeUntrustedInstallDir = "selfupdate.untrusted_install_dir" // path, who

	// Another copy of LectureFlow that shares the data directory (the
	// installed and the portable one do by default) has an update waiting
	// to be installed there (HTTP 409; othercopy.go): this copy downloads
	// or imports none until that one is installed, since it would replace
	// the other copy's pending.json and previous.json.
	// kind is itself a coded text, {"code": CodeCopyInstaller | CodeCopyPortable}
	// (copyParam): the page shows it inside the sentence, in its language.
	CodeOtherCopyPending = "selfupdate.other_copy_pending" // version, kind: the waiting update and the copy it belongs to

	// Names of the two Windows copies, used as the kind of
	// CodeOtherCopyPending; never an answer of their own.
	CodeCopyInstaller = "selfupdate.copy_installer" // the copy set up by the installer
	CodeCopyPortable  = "selfupdate.copy_portable"  // the portable copy
)

// PartCodes are the codes of ErrorCodes that only occur inside another
// code's text (they have a text and a catalogue entry, no HTTP status).
var PartCodes = []string{CodeCopyInstaller, CodeCopyPortable}

// copyParam names the copy a package of kind belongs to as a coded text
// for a param: {"code": …}; a kind without a name stays as it is.
func copyParam(kind string) any {
	switch kind {
	case KindInstaller:
		return map[string]any{"code": CodeCopyInstaller}
	case KindPortable:
		return map[string]any{"code": CodeCopyPortable}
	}
	return kind
}

// ErrorCodes lists every code above (cmd's catalog test).
var ErrorCodes = []string{
	CodeNotConfigured, CodeBusy, CodeNotAvailable, CodeTooOld, CodeNoPackage,
	CodeUnreachable, CodeNoManifest, CodeSignatureInvalid, CodeVersionMismatch, CodeManifestInvalid,
	CodeDownloadFailed, CodeChecksum, CodeCancelled, CodeIO,
	CodeNotNewer, CodeImportIncomplete, CodeImportInvalid,
	CodeSourcesInvalid, CodeUntrustedDir, CodeUntrustedInstallDir, CodeOtherCopyPending,
	CodeCopyInstaller, CodeCopyPortable,
}

// downloadCodes are the errors of a failed download; the others in
// Status.Error are those of the last check.
var downloadCodes = map[string]bool{CodeDownloadFailed: true, CodeChecksum: true, CodeIO: true}

// IsDownloadError reports whether code is that of a failed download (else
// of a check): a failed download of the available version is the state
// "error"; a failed check only a note next to an update that can still be
// installed (the mock shares the rule).
func IsDownloadError(code string) bool { return downloadCodes[code] }

func cerr(code string, params map[string]any, err error) error {
	return model.Errorf(code, params, err)
}

// CodeOf returns the code of a coded error, "" otherwise.
func CodeOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

// InfoOf returns the ErrorInfo of err (Detail = the wrapped error's text);
// a plain error becomes selfupdate.io_failed.
func InfoOf(err error) model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		if info.Detail == "" && ce.Err != nil {
			info.Detail = ce.Err.Error()
		}
		info.Detail = tidyDetail(info.Detail)
		return info
	}
	if errors.Is(err, context.Canceled) {
		return model.ErrorInfo{Code: CodeCancelled}
	}
	return model.ErrorInfo{Code: CodeIO, Detail: tidyDetail(err.Error())}
}

// tidyDetail normalizes the text shown under "查看详情": aria2c's messages
// (and other Windows sources) end lines with "\r\n"; lines are trimmed and
// empty ones dropped.
func tidyDetail(s string) string {
	if !strings.ContainsAny(s, "\r\n") && strings.TrimSpace(s) == s {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimRight(ln, " \t"); strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// ioErr wraps a local file error.
func ioErr(err error) error {
	if err == nil || CodeOf(err) != "" {
		return err
	}
	return cerr(CodeIO, nil, err)
}

// isNetErr reports a connection-level failure (worth trying the next URL).
func isNetErr(err error) bool {
	var ne net.Error
	var ue *url.Error
	return errors.As(err, &ne) || errors.As(err, &ue) ||
		strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "unexpected EOF")
}

// isNotExist reports a missing file.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
