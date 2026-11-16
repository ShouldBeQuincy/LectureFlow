// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"lectureflow/internal/bridge"
)

// retryRule says whether a failed task with a code may be retried, and why.
type retryRule struct {
	retry bool
	why   string
}

// failedRetry is the one table of which failures a retry can fix (T18 N2
// and M1). A retry runs the same task again: the same URLs, the same
// downloaded files. A failure the user's environment caused (network,
// disk, aria2c, a proxy, an inspecting antivirus) can be fixed before it; a
// failure of the recording itself (DRM, a live stream, its addresses, a
// file that is not a recording) comes back as it was: such a task offers
// "remove" only, and the extension sending the recording again (Enqueue,
// Refresh) starts it anew. SPEC-ext leaves the actions after a failure
// open. A code missing here stays retryable (Retryable).
var failedRetry = map[string]retryRule{
	// needs_refresh codes: the state offers cancel only (AllowedActions);
	// after a refresh the task resumes
	CodeRestarted:           {true, "needs_refresh: the extension gives new credentials"},
	CodeCredentialsRejected: {true, "needs_refresh: the extension gives new credentials"},
	CodeCredentialsExpired:  {true, "needs_refresh: the extension gives new credentials"},
	CodeCredentialsCleared:  {true, "needs_refresh: the extension gives new credentials"},

	CodeHTTPStatus:  {true, "a 5xx, 429 or a passing 403/404 of the platform may be gone later"},
	CodeNetwork:     {true, "the network or a timeout: fixed when the connection is back"},
	CodeNoSpace:     {true, "the disk: fixed by freeing space"},
	CodeNoSpaceKept: {true, "the disk: fixed by freeing space; the retry goes on with the files kept"},
	CodeWriteFailed: {true, "the data directory's disk or permissions: fixable"},
	// the text says to reinstall: a retry after that goes on with what is
	// downloaded
	CodeEngineMissing: {true, "aria2c missing: fixed by reinstalling"},
	CodeEngineFailed:  {true, "aria2c crashed or stopped: transient"},
	CodeTLSFailed:     {true, "an HTTPS inspection of the network or antivirus: fixed by another network"},
	// a playlist cut short or a server's passing error page; the text says
	// to retry and that the platform may be unsupported when it keeps
	// happening
	CodePlaylistInvalid: {true, "often a truncated or passing answer of the server"},
	// the environment variables are read again when LectureFlow restarts
	CodeProxyUnsupported: {true, "the user's proxy settings: fixed by changing them"},
	CodeMergeFailed:      {true, "ffmpeg or a missing part: a retry downloads the part again"},
	CodeImportFailed:     {true, "the lecture import's own failure (disk, database): transient"},

	CodePlaylistUnsupported: {false, "a live stream or a feature not supported (multi-period, byte ranges, xlink): the same playlist again"},
	CodeDRMProtected:        {false, "protected content: never handled (SPEC-ext 不碰 DRM)"},
	CodeLocalAddress:        {false, "the recording's addresses lead to the local network: refused again"},
	CodeLoopbackAddress:     {false, "the recording's addresses lead to this computer: refused again"},
	CodeSiteBlocked:         {false, "a site LectureFlow does not handle: refused again"},
	CodeUnusualHost:         {false, "an address that cannot be checked safely: refused again"},
	CodeAngleNoVideo:        {false, "the second angle's file has no video: the same file again (send without it)"},
	CodeTooLarge:            {false, "over the size one task may hold (a live stream, a server that never stops): the same download again"},
	// ImportRefused (fetch.RefusedFile): a retry imports the same file; the
	// extension sending it again downloads it anew
	"upload.media_empty":    {false, "the fetched file is empty: the same file again"},
	"upload.media_html":     {false, "the fetched file is a web page: the same file again"},
	"upload.media_playlist": {false, "the fetched file is a playlist: the same file again"},
	"upload.media_format":   {false, "the fetched file's format is refused: the same file again"},
}

// Retryable reports whether a failed task with this error code offers a
// retry (failedRetry). An unknown code is retryable, but a refused file's.
func Retryable(code string) bool {
	if r, ok := failedRetry[code]; ok {
		return r.retry
	}
	return !RefusedFile(code)
}

// AllowedActions are the actions of a task in state whose error code is
// code (TaskInfo.AllowedActions; also mockserve's). Never nil.
func AllowedActions(state, code string) []string {
	switch state {
	case bridge.StateQueued, bridge.StateDownloading, bridge.StateMerging, bridge.StateNeedsRefresh:
		return []string{ActionCancel}
	case bridge.StateFailed:
		if !Retryable(code) {
			return []string{ActionRemove}
		}
		return []string{ActionRetry, ActionRemove}
	case bridge.StateDone, bridge.StateCancelled:
		return []string{ActionRemove}
	}
	return []string{} // importing: a few seconds, nothing to do
}
