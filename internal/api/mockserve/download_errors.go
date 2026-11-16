// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The scenario "download_errors" (?mock=download_errors): one ended task
// for every task error code internal/fetch stores (fetch.ErrorCodes less
// the HTTP answers, plus the lecture import's upload.media_* refusals that
// fetch.ImportRefused passes on), so that the download list's text of each
// reason can be seen; the scenario "downloads" keeps its ten tasks (its
// tests and scripts/shots/ext_dl_shots.py count them). Code, params and
// detail are shaped as internal/fetch builds them (codes.go, task.go,
// local.go, queue.go, merge.go, angle.go, drm.go); host names are reserved
// ones. needs_refresh tasks can be cancelled, failed ones removed and,
// when a retry can help (fetch.Retryable), retried (dlActions), as in
// "downloads".

import (
	"fmt"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/fetch"
	"lectureflow/internal/model"
)

const dlErrScenario = "download_errors"

// dlErrSample is one sample failure.
type dlErrSample struct {
	course, title, state string
	err                  model.ErrorInfo
}

func dlErrSamples() []dlErrSample {
	const crypto, osc = dlCourseCrypto, dlCourseOS
	refresh, failed := "needs_refresh", "failed"
	e := func(code string, params map[string]any, detail string) model.ErrorInfo {
		return model.ErrorInfo{Code: code, Params: params, Detail: detail}
	}
	p := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	return []dlErrSample{
		// needs_refresh (task.go, queue.go)
		{crypto, "Week 10 · Restarted", refresh, e(fetch.CodeRestarted, nil, "")},
		{crypto, "Week 10 · Credentials rejected", refresh, e(fetch.CodeCredentialsRejected, p("status", 403), "")},
		{crypto, "Week 10 · Credentials expired", refresh, e(fetch.CodeCredentialsExpired, nil, "")},
		{crypto, "Week 10 · Credentials cleared", refresh, e(fetch.CodeCredentialsCleared, p("minutes", 30), "")},
		// failed: network and engine (task.go ariaFailure, netFailure)
		{crypto, "Week 11 · HTTP status", failed, e(fetch.CodeHTTPStatus, p("status", 404), "aria2 error 3: Resource not found")},
		{crypto, "Week 11 · Network", failed, e(fetch.CodeNetwork, nil, "aria2 error 2: Timeout.")},
		{crypto, "Week 11 · TLS", failed, e(fetch.CodeTLSFailed, nil, "aria2 error 1: SSL/TLS handshake failure: unable to get local issuer certificate")},
		{crypto, "Week 11 · No space", failed, e(fetch.CodeNoSpace, p("free_bytes", int64(734_003_200), "keep_bytes", int64(2<<30)), "aria2 error 9: There is not enough space on the disk.")},
		{crypto, "Week 11 · Write failed", failed, e(fetch.CodeWriteFailed, nil, "aria2 error 16: Failed to open the file, cause: Access is denied.")},
		{crypto, "Week 11 · Engine missing", failed, e(fetch.CodeEngineMissing, nil, "")},
		{crypto, "Week 11 · Engine failed", failed, e(fetch.CodeEngineFailed, nil, "aria2c exited")},
		// failed: the stream (task.go playlistFailure, drm.go)
		{crypto, "Week 12 · Playlist invalid", failed, e(fetch.CodePlaylistInvalid, nil, "the file is shorter than its playlist (1048576 bytes expected)")},
		{crypto, "Week 12 · Live stream", failed, e(fetch.CodePlaylistUnsupported, p("feature", "live"), "")},
		{crypto, "Week 12 · DRM", failed, e(fetch.CodeDRMProtected, nil, "protected stream: sample-aes")},
		// failed: refused addresses (local.go localFailure, proxyFailure)
		{crypto, "Week 13 · Local network", failed, e(fetch.CodeLocalAddress, p("host", "nas.intranet.test"),
			"refused an address of the local network for a recording whose page is not on it: nas.intranet.test")},
		{crypto, "Week 13 · This computer", failed, e(fetch.CodeLoopbackAddress, p("host", "localhost"),
			"refused an address of this computer: localhost")},
		{crypto, "Week 13 · Site blocked", failed, e(fetch.CodeSiteBlocked, p("category", "courses", "host", "courses.example"),
			"refused a site LectureFlow does not handle: courses.example")},
		{crypto, "Week 13 · Unusual host name", failed, e(fetch.CodeUnusualHost, p("host", "vid%C3%A9o.example"),
			"refused a host name with unusual characters: vid%C3%A9o.example")},
		{crypto, "Week 13 · FTP redirect", failed, e(fetch.CodeUnusualHost, p("host", "ftp://files.example"),
			"refused an address that is neither http nor https (a redirect led there): ftp://files.example")},
		{crypto, "Week 13 · SOCKS proxy", failed, e(fetch.CodeProxyUnsupported, p("scheme", "socks5h"),
			"the proxy set for this address is a socks5h:// one, which aria2c cannot use; nothing was handed to aria2c")},
		{crypto, "Week 13 · HTTPS proxy", failed, e(fetch.CodeProxyUnsupported, p("scheme", "https"),
			"the proxy set for this address is a https:// one, which aria2c cannot use; nothing was handed to aria2c")},
		// failed: after the download (merge.go, angle.go)
		{osc, "Lecture 8 · Merge failed", failed, e(fetch.CodeMergeFailed, nil, "a downloaded part is missing: 00042.ts")},
		{osc, "Lecture 8 · Second angle", failed, e(fetch.CodeAngleNoVideo, nil, "Stream map '0:v:0' matches no streams.")},
		{osc, "Lecture 8 · Import failed", failed, e(fetch.CodeImportFailed, nil, "no lecture import is configured")},
		// failed: the lecture import refused the file (merge.go importMedia → internal/api upload_media.go)
		{osc, "Lecture 9 · Empty file", failed, e("upload.media_empty", p("name", "Lecture 9 · Empty file.mp4"), "")},
		{osc, "Lecture 9 · Web page", failed, e("upload.media_html", p("name", "Lecture 9 · Web page.mp4"), "")},
		{osc, "Lecture 9 · Playlist", failed, e("upload.media_playlist", p("name", "Lecture 9 · Playlist.mp4", "kind", "m3u8"), "")},
		{osc, "Lecture 9 · Format", failed, e("upload.media_format", p("name", "Lecture 9 · Format.mp4", "format", "tty"), "")},
		// failed: over the size one task may hold (disklimit.go; last, so
		// that the ids above stay)
		{crypto, "Week 12 · Too large", failed, e(fetch.CodeTooLarge, p("max_bytes", fetch.DefaultMaxTaskBytes),
			"the recording would hold 68719542272 bytes or more, over the 68719476736 bytes one task may fetch")},
		// failed: no space, with the parts fetched so far kept for the
		// resume (disklimit.go discardAfter; last, so that the ids above stay)
		{crypto, "Week 12 · No space, parts kept", failed, e(fetch.CodeNoSpaceKept,
			p("free_bytes", int64(1_610_612_736), "keep_bytes", int64(2<<30), "task_bytes", int64(3_221_225_472)),
			"1610612736 bytes free on the data directory's volume, 2147483648 to keep")},
	}
}

func newDLErrorState(now time.Time) *dlState {
	const mb = int64(1_000_000)
	av := []api.DownloadTrack{{Kind: "audio", Format: "hls", Role: "primary"}, {Kind: "video", Format: "hls", Role: "screen"}}
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	st := &dlState{}
	for i, x := range dlErrSamples() {
		id := int64(60 + i)
		created := time.Duration(i+1) * 7 * time.Minute // newest first = the order above
		info := x.err
		t := &dlTask{mockDownload: mockDownload{
			ID: id, Platform: "panopto", SessionID: fmt.Sprintf("dec0de%02d-0000-4000-8000-%012d", id, id), CourseID: x.course,
			Title: x.title, RecordedAt: now.AddDate(0, 0, -(i+1)).Format("2006-01-02") + "T09:35", State: x.state,
			BytesDone: int64(i%5) * 40 * mb, BytesTotal: 600 * mb, Tracks: av, Error: &info,
			CreatedAt: at(created), UpdatedAt: at(created / 2),
		}, last: now}
		st.seq++
		t.seq = st.seq
		st.tasks = append(st.tasks, t)
	}
	st.settle()
	return st
}
