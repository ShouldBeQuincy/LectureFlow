// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
)

// T18 N2 / M1: a failed task whose retry can only fail again (DRM, a live
// stream or a format not supported, an address of the local network or of
// this computer, a site not handled, an unusual host name, a file the
// lecture import refused, a second angle without video) offers "remove"
// only; a failure the user's environment caused (network, timeout, disk,
// aria2c, proxy, certificate inspection) keeps "retry". Every task code is
// pinned here: a new code must be put in failedRetry.
func TestRetryableTable(t *testing.T) {
	want := map[string]bool{
		CodeRestarted: true, CodeCredentialsRejected: true, CodeCredentialsExpired: true, CodeCredentialsCleared: true,
		CodeHTTPStatus: true, CodeNetwork: true, CodeNoSpace: true, CodeNoSpaceKept: true, CodeWriteFailed: true,
		CodeEngineMissing: true, CodeEngineFailed: true, CodeTLSFailed: true, CodePlaylistInvalid: true,
		CodeProxyUnsupported: true, CodeMergeFailed: true, CodeImportFailed: true,

		CodePlaylistUnsupported: false, CodeDRMProtected: false, CodeLocalAddress: false, CodeLoopbackAddress: false,
		CodeSiteBlocked: false, CodeUnusualHost: false, CodeAngleNoVideo: false, CodeTooLarge: false,
		"upload.media_empty": false, "upload.media_html": false, "upload.media_playlist": false, "upload.media_format": false,
	}
	httpAnswers := []string{CodeNotFound, CodeActive, CodeNeedsRefresh, CodeNotRetryable, CodeNotCancellable, CodeConcurrencyInvalid, CodeConnectionsInvalid}
	for _, c := range ErrorCodes {
		if slices.Contains(httpAnswers, c) {
			continue
		}
		if _, ok := want[c]; !ok {
			t.Errorf("task code %s is not pinned by this test", c)
		}
		if _, ok := failedRetry[c]; !ok {
			t.Errorf("task code %s is not in failedRetry", c)
		}
	}
	for c, w := range want {
		r, ok := failedRetry[c]
		if !ok {
			t.Errorf("%s: not in failedRetry", c)
			continue
		}
		if r.why == "" {
			t.Errorf("%s: no reason given", c)
		}
		if got := Retryable(c); got != w {
			t.Errorf("Retryable(%s) = %v, want %v", c, got, w)
		}
		acts := AllowedActions(bridge.StateFailed, c)
		wantActs := []string{ActionRemove}
		if w {
			wantActs = []string{ActionRetry, ActionRemove}
		}
		if !slices.Equal(acts, wantActs) {
			t.Errorf("AllowedActions(failed, %s) = %v, want %v", c, acts, wantActs)
		}
	}
	for c := range failedRetry {
		if _, ok := want[c]; !ok {
			t.Errorf("failedRetry has %s, which this test does not pin", c)
		}
	}
	// a code nobody classified (none, an older version's, a future
	// upload.media_* refusal) stays retryable but a refused file
	if !Retryable("") || !Retryable("downloads.something_new") || Retryable("upload.media_other") {
		t.Error("fallbacks")
	}
	// the other states do not look at the code
	for _, c := range []string{"", CodeDRMProtected, CodeNetwork} {
		for state, w := range map[string][]string{
			bridge.StateQueued: {ActionCancel}, bridge.StateDownloading: {ActionCancel}, bridge.StateMerging: {ActionCancel},
			bridge.StateNeedsRefresh: {ActionCancel}, bridge.StateImporting: {},
			bridge.StateDone: {ActionRemove}, bridge.StateCancelled: {ActionRemove},
		} {
			if got := AllowedActions(state, c); !slices.Equal(got, w) || got == nil {
				t.Errorf("AllowedActions(%s, %q) = %v", state, c, got)
			}
		}
	}
}

// The queue answers what the table says (List, Get), and refuses a retry it
// does not offer with the task's own reason (409).
func TestFailedPermanentOffersRemoveOnly(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	ins := func(session, code, params string) int64 {
		id, err := e.opts.Store.InsertDownloadTask(ctx, db.DownloadTask{Platform: "panopto", SessionID: session, ProfileID: "panopto",
			CourseID: e.course, Title: "x", TracksJSON: `[{"kind":"audio","format":"file"}]`, State: bridge.StateFailed,
			ErrorCode: code, ErrorParams: params, BytesTotal: -1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	drm := ins("s-drm", CodeDRMProtected, "")
	refused := ins("s-html", "upload.media_html", `{"name":"x.mp4"}`)
	network := ins("s-net", CodeNetwork, "")
	e.restart()
	for id, w := range map[int64][]string{drm: {ActionRemove}, refused: {ActionRemove}, network: {ActionRetry, ActionRemove}} {
		info, err := e.q.Get(ctx, id)
		if err != nil || !slices.Equal(info.AllowedActions, w) {
			t.Errorf("task %d: %v %v", id, info.AllowedActions, err)
		}
	}
	list, err := e.q.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range list {
		if info.ID == drm && !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
			t.Errorf("list: %v", info.AllowedActions)
		}
	}
	for id, code := range map[int64]string{drm: CodeDRMProtected, refused: "upload.media_html"} {
		_, err := e.q.Retry(ctx, id)
		if codedOf(err) != code || !errors.Is(err, ErrConflict) {
			t.Errorf("retry of task %d: %v", id, err)
		}
		if info, _ := e.q.Get(ctx, id); info.State != bridge.StateFailed {
			t.Errorf("task %d after a refused retry: %s", id, info.State)
		}
	}
	// a retryable one still goes on (no credentials after the restart:
	// needs_refresh, as before)
	if info, err := e.q.Retry(ctx, network); err != nil || info.State != bridge.StateNeedsRefresh {
		t.Fatalf("retry of a network failure: %+v %v", info, err)
	}
	// removing is allowed
	if err := e.q.Remove(ctx, drm); err != nil {
		t.Fatal(err)
	}
}

// R61 F1: a failure no retry can fix does not keep the credentials for a
// retry (ruling 4's grace period is for retries alone): they are dropped at
// once, and after the grace period the task is still failed for its own
// reason, offering remove only — not needs_refresh / credentials_cleared,
// whose text sends the user to the extension for the same failure. A
// retryable failure keeps them (TestFailedCredentialsExpire).
func TestFailedPermanentDropsCredentials(t *testing.T) {
	clk := &fakeClock{}
	e := newQEnv(t, func(o *Options) {
		o.Now = clk.now
		o.CredentialSweep = 5 * time.Millisecond
	})
	ctx := context.Background()
	dashFixture(e, "p", `<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`)
	id := e.enqueue(e.request("s1", e.dashTrack(bridge.KindVideo, "/p/manifest.mpd", "video:-:0")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeDRMProtected || !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
		t.Fatalf("failed %+v", info)
	}
	if e.q.hasCredsOf(id) {
		t.Fatal("a failure no retry can fix kept its credentials")
	}
	clk.advance(31 * time.Minute)
	time.Sleep(50 * time.Millisecond) // a few sweeps
	info, err := e.q.Get(ctx, id)
	if err != nil || info.State != bridge.StateFailed || info.Error == nil || info.Error.Code != CodeDRMProtected ||
		!strings.Contains(info.Error.Detail, "drm") || !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
		t.Fatalf("after 31 minutes %+v %v", info, err)
	}
	if e.q.hasCredsOf(id) {
		t.Fatal("credentials in memory after 31 minutes")
	}
}
