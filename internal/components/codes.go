// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"

	"lectureflow/internal/model"
)

// Error codes of this package (SPEC-v2 P14.3): every error the Manager
// returns or stores in a task is a *model.CodedError with one of these
// codes; the front end shows t("errors."+code, params). Proposed zh-CN / en
// texts: packaging/components/i18n/.
const (
	// Download and install failures (TaskStatus.Error, ItemStatus.Error).
	CodeNetwork        = "components.network"         // the connection failed or broke off
	CodeDiskFull       = "components.disk_full"       // required_mb, free_mb (free_mb -1 = unknown)
	CodeChecksum       = "components.checksum"        // file: SHA-256 mismatch twice
	CodeHTTPStatus     = "components.http_status"     // status
	CodeCancelled      = "components.cancelled"       //
	CodeUVFailed       = "components.uv_failed"       // step (venv | sync); Detail = uv's last output lines
	CodeUVMissing      = "components.uv_missing"      // the bundled uv is missing (Options.UV)
	CodeVerifyFailed   = "components.verify_failed"   // id: the installed environment cannot import its packages
	CodeSelfTestFailed = "components.selftest_failed" // id: the update's self-test failed; the old version stays
	CodeUnpackFailed   = "components.unpack_failed"   // file
	CodeIO             = "components.io"              // a local file operation failed
	CodeLockMissing    = "components.lock_missing"    // id: the lock file is missing or does not match the manifest
	// CodeRedirectRefused: the source redirected to an address the
	// downloads do not follow (download.go "Redirects": this computer, the
	// local network, plain http, more than five hops). Not transient: the
	// same source answers the same, so the next source is tried at once,
	// and the last one fails the task (SEC47 low-3; a mirror's falls back
	// to the default, mirror_fallback's reason). Params: host (the
	// redirect's, printable ASCII), file for a download's.
	CodeRedirectRefused = "components.redirect_refused"

	// Requests (HTTP 4xx in wave 2).
	CodeUnknown             = "components.unknown"              // id
	CodeUnavailable         = "components.unavailable"          // id, reason
	CodeUnsupportedPlatform = "components.unsupported_platform" // id, platform
	CodeBusy                = "components.busy"                 // id: a task is installing or updating it
	CodeInUse               = "components.in_use"               // id, dependents
	CodeInUseByTask         = "components.in_use_by_task"       // id, reason: Options.InUse says a task uses it (reason: asr | device_test | self_check | material | task)
	CodeNotInstalled        = "components.not_installed"        // id
	CodeBundled             = "components.bundled"              // id: part of the application
	CodeUpToDate            = "components.up_to_date"           // id, version
	CodeTaskNotFound        = "components.task_not_found"       // task
	CodeTaskState           = "components.task_state"           // task, state: the action does not apply in this state
	// CodePathTooLong: on Windows the component's deepest file would get a
	// path longer than MaxPathLen under this data directory (pathlen.go).
	CodePathTooLong = "components.path_too_long" // id, path_len, max, data_dir

	// Offline import (ImportFile).
	CodeImportInvalid     = "components.import_invalid"      // the file is not a component archive
	CodeImportUnknown     = "components.import_unknown"      // id, version: not a component of this application version
	CodeImportChecksum    = "components.import_checksum"     // id: the archive's SHA-256 differs from the manifest
	CodeImportMissingDeps = "components.import_missing_deps" // id, dependents: install these first

	// Manifests.
	CodeManifestInvalid      = "components.manifest_invalid"      //
	CodeManifestIncompatible = "components.manifest_incompatible" // app_version, compat
	CodeManifestSchema       = "components.manifest_schema"       // schema
	CodeNoUpdateURL          = "components.no_update_url"         //

	// Notices (TaskStatus.Notices).
	CodeMirrorFallback  = "components.mirror_fallback"  // group (pypi | models), mirror, reason (an error code)
	CodeSelfTestSkipped = "components.selftest_skipped" // id: no self-test function was configured
)

// ErrorCodes lists the codes of this package (for the catalog check of
// wave 2).
var ErrorCodes = []string{
	CodeNetwork, CodeDiskFull, CodeChecksum, CodeHTTPStatus, CodeCancelled, CodeUVFailed,
	CodeUVMissing, CodeVerifyFailed, CodeSelfTestFailed, CodeUnpackFailed, CodeIO, CodeLockMissing, CodeRedirectRefused,
	CodeUnknown, CodeUnavailable, CodeUnsupportedPlatform, CodeBusy, CodeInUse, CodeInUseByTask, CodeNotInstalled,
	CodeBundled, CodeUpToDate, CodeTaskNotFound, CodeTaskState, CodePathTooLong,
	CodeImportInvalid, CodeImportUnknown, CodeImportChecksum, CodeImportMissingDeps,
	CodeManifestInvalid, CodeManifestIncompatible, CodeManifestSchema, CodeNoUpdateURL,
	CodeMirrorFallback, CodeSelfTestSkipped,
}

func cerr(code string, params map[string]any, err error) error {
	return model.Errorf(code, params, err)
}

// ErrorInfoOf returns the ErrorInfo of err (Detail = the error text); a
// plain error becomes components.io. A user name and password in a URL of
// the text are removed (redactText): Detail reaches the log, the task's
// status and the page.
func ErrorInfoOf(err error) model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		if info.Detail == "" && ce.Err != nil {
			info.Detail = ce.Err.Error()
		}
		info.Detail = redactText(info.Detail)
		return info
	}
	return model.ErrorInfo{Code: CodeIO, Detail: redactText(err.Error())}
}

// CodeOf returns the code of a coded error, "" otherwise.
func CodeOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

// Control causes of a task's context.
var (
	errPaused    = errors.New("paused")
	errCancelled = errors.New("cancelled")
	errClosing   = errors.New("manager closing")
)

// classify maps a download or file error to an error code.
func classify(err error) string {
	if err == nil {
		return ""
	}
	if c := CodeOf(err); c != "" {
		return c
	}
	var rr *redirectRefusedError
	switch {
	case errors.As(err, &rr):
		return CodeRedirectRefused
	case errors.Is(err, errCancelled), errors.Is(err, context.Canceled):
		return CodeCancelled
	case isDiskFull(err):
		return CodeDiskFull
	}
	var ne net.Error
	var ue *url.Error
	if errors.As(err, &ne) || errors.As(err, &ue) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "connection reset") {
		return CodeNetwork
	}
	return CodeIO
}

// asCoded wraps a plain error with its classified code; a refused redirect
// gets its host among the params.
func asCoded(err error, params map[string]any) error {
	if err == nil || CodeOf(err) != "" {
		return err
	}
	var rr *redirectRefusedError
	if errors.As(err, &rr) {
		p := map[string]any{"host": printableHost(rr.host)}
		for k, v := range params {
			p[k] = v
		}
		return cerr(CodeRedirectRefused, p, err)
	}
	return cerr(classify(err), params, err)
}
