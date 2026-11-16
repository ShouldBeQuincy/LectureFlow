// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
)

// What the app checks on its own of where a task's credentials go and
// where its tracks come from (AUD3 medium-1 and low-1, the app's side).

// The extension reads the cookies of the track URLs (cookies.getAll), so a
// task's cookies are those of its tracks' hosts. One for another host (a
// hand-made message) is left out: a redirect or a playlist entry that
// leads to that host gets no cookie, nor does aria2c for it.
func TestCookiesOnlyForTrackHosts(t *testing.T) {
	e := newQEnv(t, nil)
	other, otherURL := otherHost(t)
	video := pattern(7, 50_000)
	other.put("/cdn/v.mp4", video)
	e.media.mu.Lock()
	e.media.redir["/files/r/v.mp4"] = otherURL + "/cdn/v.mp4?Expires=1&Sig=cdn"
	e.media.mu.Unlock()
	const otherSecret = "OTHER_HOST_COOKIE_9d41"
	req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/r/v.mp4"))
	req.Cookies = append(req.Cookies, bridge.Cookie{Name: "cdn", Value: otherSecret, Domain: "localhost", Path: "/", HostOnly: true})
	info := e.wait(e.enqueue(req), bridge.StateDone)
	if !bytes.Equal(readAll(t, e.lecture(info.LectureID).MediaPath), video) {
		t.Fatal("media differs")
	}
	seen := other.requests("")
	if len(seen) < 2 { // Go's HEAD and aria2c's GET
		t.Fatalf("the other host saw %+v", seen)
	}
	for _, r := range seen {
		if r.CookieHeader != "" {
			t.Fatalf("a cookie reached the other host: %+v", r)
		}
	}
	adds, _, _, _, _ := e.aria.snapshot()
	for _, a := range adds {
		if slices.ContainsFunc(a.Headers, func(h string) bool { return strings.Contains(h, otherSecret) }) {
			t.Fatalf("aria2c got the other host's cookie: %+v", a.URI)
		}
	}
	// the track's own host still gets its session (Go's redirect probe)
	if r := e.media.requests("/files/r/v.mp4"); len(r) == 0 || r[0].Cookie != testCookie {
		t.Fatalf("probe %+v", r)
	}
}

// A refresh gives a task's tracks new URLs. Each must stay on the host of
// the track it replaces, unless the refresh comes from the task's own page
// (the platform may have moved its media to another host since): a page
// that embeds a player frame of the platform can no longer swap its own
// media into a task waiting for a refresh (AUD3 low-1). From another page
// (an LMS embedding the player) with the media on the same host, as
// before.
func TestRefreshKeepsTheTaskHosts(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	other, otherURL := otherHost(t)
	for _, m := range []*mediaServer{e.media, other} {
		m.mu.Lock()
		m.refPrefix = "" // the pages differ here; what the media servers get is not the point
		m.mu.Unlock()
	}
	video := pattern(8, 30_000)
	e.media.put("/files/v.mp4", video)
	other.put("/cdn/v.mp4", video)
	e.media.setStatus("/files/v.mp4", 403)
	needsRefresh := func(session string) int64 {
		t.Helper()
		id := e.enqueue(e.request(session, e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
		e.wait(id, bridge.StateNeedsRefresh)
		return id
	}
	refresh := func(session, referer, trackURL string) error {
		_, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "r-" + session},
			Platform: "panopto", SessionID: session, Referer: referer, Cookies: e.cookies(),
			Tracks: []bridge.Track{{URL: trackURL, Kind: bridge.KindVideo, Format: bridge.FormatFile, Role: bridge.KindVideo, SizeBytes: -1}}})
		return err
	}
	const elsewhere = "https://attacker.example/embeds-the-player"
	a, b, c := needsRefresh("a"), needsRefresh("b"), needsRefresh("c")
	e.media.setStatus("/files/v.mp4", 0)

	// another page, another host: refused, the task still waits
	err := refresh("a", elsewhere, otherURL+"/cdn/v.mp4?Sig=x")
	var be *bridge.Error
	if !errors.As(err, &be) || be.Code != bridge.CodeBadRequest || be.Params["field"] != "tracks.url" || be.Params["reason"] != "other_host" {
		t.Fatalf("refresh from another page and host: %v", err)
	}
	if info, _ := e.q.Get(ctx, a); info.State != bridge.StateNeedsRefresh {
		t.Fatalf("state %s", info.State)
	}
	// no referer at all (a profile that sends none): the hosts decide
	if err := refresh("a", "", otherURL+"/cdn/v.mp4?Sig=x"); bridgeCode(err) != bridge.CodeBadRequest {
		t.Fatalf("refresh without a page, another host: %v", err)
	}

	// another page (an LMS), the same host: resumes
	if err := refresh("b", elsewhere, e.fileTrack(bridge.KindVideo, "/files/v.mp4").URL); err != nil {
		t.Fatal(err)
	}
	e.wait(b, bridge.StateDone)

	// the task's own page, another host: resumes
	if err := refresh("c", testReferer, otherURL+"/cdn/v.mp4?Sig=x"); err != nil {
		t.Fatal(err)
	}
	e.wait(c, bridge.StateDone)
}
