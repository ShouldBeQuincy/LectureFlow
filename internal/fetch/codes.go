// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"errors"

	"lectureflow/internal/model"
)

// Codes of the download queue (testdata/api/error_codes_backend.json). The
// task codes are stored with a failed or needs_refresh task and shown in
// the download list (Download.error); the request codes are the HTTP
// layer's answers to /api/downloads and the download settings
// (ext-brief §6.2 "接口（冻结…）").
const (
	// needs_refresh: the credentials are gone or no longer accepted.
	CodeRestarted           = "downloads.restarted"            // the app restarted before the task ended
	CodeCredentialsRejected = "downloads.credentials_rejected" // params: status (401 or 403)
	CodeCredentialsExpired  = "downloads.credentials_expired"  // every cookie sent has expired
	// A failed task kept its credentials for a retry no longer than
	// Options.FailedCredentialTTL (lead's ruling 4). Params: minutes.
	CodeCredentialsCleared = "downloads.credentials_cleared"

	// failed (retryable).
	CodeHTTPStatus = "downloads.http_status" // params: status
	CodeNetwork    = "downloads.network"
	CodeNoSpace    = "downloads.no_space" // params: free_bytes, keep_bytes
	// no_space with the task's own files kept for the resume (disklimit.go:
	// they stay so that the retry goes on; SEC43b low-5): the text says how
	// much they hold, which removing the task frees. Params: free_bytes,
	// keep_bytes, task_bytes (lead's interface F5; the extension knows it
	// as a disk failure).
	CodeNoSpaceKept   = "downloads.no_space_kept"
	CodeWriteFailed   = "downloads.write_failed"
	CodeEngineMissing = "downloads.engine_missing"
	CodeEngineFailed  = "downloads.engine_failed"
	// The server's certificate was refused (an invalid chain, often a
	// school proxy's or an antivirus program's HTTPS inspection).
	CodeTLSFailed           = "downloads.tls_failed"
	CodePlaylistInvalid     = "downloads.playlist_invalid"
	CodePlaylistUnsupported = "downloads.playlist_unsupported" // params: feature (live | multi_period | byte_range | xlink)
	// The stream is protected by DRM (DASH ContentProtection, an HLS key
	// with a KEYFORMAT other than identity, SAMPLE-AES, an MP4 file with
	// encrypted samples: drm.go): LectureFlow does not handle protected
	// content. The detail names the feature.
	CodeDRMProtected = "downloads.drm_protected"
	// A URL of the task (a playlist's entry, a redirect, a name's address)
	// leads to the local network (a private or link-local address, *.local)
	// while the recording's page is public and on another site: refused
	// (security review L1, local.go). Params: host.
	CodeLocalAddress = "downloads.local_address"
	// A URL of the task leads to this computer (loopback), which downloads
	// never reach (local.go). Params: host.
	CodeLoopbackAddress = "downloads.loopback_address"
	// The recording's page or a URL of the task is on a site LectureFlow
	// does not handle (internal/blocklist: YouTube, MOOCs and online / paid
	// course platforms; user 2026-10-27): the bridge's answer to enqueue
	// and refresh, and a task's failure when a playlist, an MPD or a
	// redirect leads there later. Params: category (youtube | courses);
	// host for a task.
	CodeSiteBlocked = "downloads.site_blocked"
	// A URL of the task (a playlist's entry, an MPD's, a redirect's
	// Location) has a host name with characters outside plain ASCII, a
	// control character or a percent sign (blocklist.UnusualHost): it can
	// be neither matched against the do-not-handle list nor judged for
	// local addresses as written (net/http maps it by IDNA), so it is
	// refused (SEC10). Params: host (non-ASCII bytes percent-encoded).
	// Also a redirect that leads to an address that is neither http nor
	// https (ftp://, which aria2c follows; the guard proxy refuses it,
	// guard.go): host is then the scheme and the host, "ftp://files.example".
	// Also an address of no single server (multicast, the limited
	// broadcast, an IPv4-compatible address of a public one, an IPv6
	// address with 0.0.0.0/8 inside: local.go classNotUnicast), and a URL
	// with user info ("user:pass@", which aria2c would send as Basic
	// authentication; local.go userInfoError): host is the URL's host
	// alone, never the user info.
	CodeUnusualHost = "downloads.unusual_host"
	// The proxy set for a URL of the task (HTTP_PROXY / HTTPS_PROXY, as
	// Go's own requests read them) is one aria2c cannot use: a SOCKS proxy
	// (aria2c has none: it would connect directly, past the proxy, from
	// the user's own address) or one written https:// (aria2c speaks plain
	// HTTP to it: the proxy's password would go out in the clear). Nothing
	// is handed to aria2c (SEC16 low-1 and low-2; user 2026-11-11).
	// Params: scheme (socks5, socks5h, https, ...).
	CodeProxyUnsupported = "downloads.proxy_unsupported"
	CodeMergeFailed      = "downloads.merge_failed"
	CodeImportFailed     = "downloads.import_failed"
	// The second camera angle (angle.go) has no video stream to keep.
	CodeAngleNoVideo = "downloads.angle_no_video"
	// The task would hold more than one task may (Options.MaxTaskBytes,
	// 64 GiB; disklimit.go): a live stream or a server that never stops
	// sending, rarely a real recording. Not retryable; the files go.
	// Params: max_bytes.
	CodeTooLarge = "downloads.too_large"

	// HTTP answers.
	CodeNotFound           = "downloads.not_found"           // 404
	CodeActive             = "downloads.active"              // 409 DELETE of an unfinished task; params: state
	CodeNeedsRefresh       = "downloads.needs_refresh"       // 409 retry of a needs_refresh task
	CodeNotRetryable       = "downloads.not_retryable"       // 409 retry of a task that did not fail; params: state
	CodeNotCancellable     = "downloads.not_cancellable"     // 409 cancel of an ended or importing task; params: state
	CodeConcurrencyInvalid = "downloads.concurrency_invalid" // 400; params: value, min, max
	CodeConnectionsInvalid = "downloads.connections_invalid" // 400; params: value, min, max
)

// ErrorCodes lists every code above (cmd's catalog test).
var ErrorCodes = []string{
	CodeRestarted, CodeCredentialsRejected, CodeCredentialsExpired, CodeCredentialsCleared,
	CodeHTTPStatus, CodeNetwork, CodeNoSpace, CodeNoSpaceKept, CodeWriteFailed, CodeEngineMissing, CodeEngineFailed, CodeTLSFailed,
	CodePlaylistInvalid, CodePlaylistUnsupported, CodeDRMProtected, CodeLocalAddress, CodeLoopbackAddress, CodeSiteBlocked, CodeUnusualHost, CodeProxyUnsupported, CodeMergeFailed, CodeImportFailed, CodeAngleNoVideo, CodeTooLarge,
	CodeNotFound, CodeActive, CodeNeedsRefresh, CodeNotRetryable, CodeNotCancellable,
	CodeConcurrencyInvalid, CodeConnectionsInvalid,
}

// Sentinels wrapped by the coded errors of Get, Cancel, Retry and Remove
// (model.CodedError): the HTTP layer answers 404 for ErrNotFound and 409
// for ErrConflict.
var (
	ErrNotFound = errors.New("download task not found")
	ErrConflict = errors.New("download task state does not allow this")
	// ErrClosed is returned once the queue is closed, ErrNotStarted
	// before Start.
	ErrClosed     = errors.New("download queue closed")
	ErrNotStarted = errors.New("download queue not started")
)

func codedNotFound() error { return model.Errorf(CodeNotFound, nil, ErrNotFound) }

func codedConflict(code, state string) error {
	var params map[string]any
	if state != "" {
		params = map[string]any{"state": state}
	}
	return model.Errorf(code, params, ErrConflict)
}
