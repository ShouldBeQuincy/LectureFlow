// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"lectureflow/internal/blocklist"
	"lectureflow/internal/bridge"
	"lectureflow/internal/calendar"
	"lectureflow/internal/db"
)

// The queue's part of bridge.Handler (ext-brief §3): check, enqueue,
// refresh and the status pushes. cmd composes hello and open.

// Limits of a request (the extension never comes near them).
const (
	maxSessionIDs = 500
	maxCookies    = 300
	maxIDLen      = 256
)

func badField(field string) error {
	return bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": field})
}

// localRequestError answers a track URL whose host is this computer or
// the local network (local.go): the protocol's request.invalid (no new
// bridge code), with the reason.
func localRequestError() error {
	return bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": "tracks.url", "reason": "local_address"})
}

// unusualRequestError answers a page or track URL whose host name has
// unusual characters (blocklist.UnusualHost; the extension sends ASCII,
// punycode, hosts only): request.invalid with the reason, like
// localRequestError (no new bridge code).
func unusualRequestError(field string) error {
	return bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": field, "reason": "unusual_host"})
}

// refuseBlocked answers a request whose page (the Referer the extension
// sends: the page address, or only its origin when the tracks are on
// another) or any track is on a site LectureFlow does not handle
// (internal/blocklist): downloads.site_blocked {category}; a host the list
// cannot compare safely is unusualRequestError. The extension keeps such
// pages out on its own; this is the second line.
func refuseBlocked(referer string, tracks []liveTrack) error {
	category, field := blocklist.URLCategory(referer), "referer"
	for _, t := range tracks {
		if category != "" {
			break
		}
		category, field = blocklist.URLCategory(t.url), "tracks.url"
	}
	switch category {
	case "":
		return nil
	case blocklist.CategoryUnusualHost:
		return unusualRequestError(field)
	}
	return bridge.NewError(CodeSiteBlocked, map[string]any{"category": category})
}

func header(typ, id string) bridge.ReplyHeader {
	return bridge.ReplyHeader{Type: typ, ID: id, OK: true}
}

// Check reports the sessions the queue knows. A session's own download
// task comes first (MatchedBy "session"): one that has not ended with its
// state, a done one whose lecture still exists as imported, a failed or
// cancelled one with its state (it may be sent again); a done task whose
// lecture was deleted does not count. Without one, a session whose title
// (req.Titles, same index) matches a lecture of req.CourseID (every
// course when "") counts as imported (MatchedBy "title"): the lecture's
// title or its original file name without the extension, compared by
// titleKey. Two queries for the whole request, however many sessions.
func (q *Queue) Check(ctx context.Context, req *bridge.CheckRequest) (*bridge.CheckReply, error) {
	if req.Platform == "" || len(req.Platform) > maxIDLen {
		return nil, badField("platform")
	}
	if len(req.SessionIDs) > maxSessionIDs {
		return nil, badField("sessionIds")
	}
	if len(req.Titles) > len(req.SessionIDs) {
		return nil, badField("titles")
	}
	if len(req.CourseID) > maxIDLen {
		return nil, badField("courseId")
	}
	out := &bridge.CheckReply{ReplyHeader: header(bridge.TypeCheck, req.ID), Items: []bridge.CheckItem{}}
	if len(req.SessionIDs) == 0 {
		return out, nil
	}
	rows, err := q.st.ListDownloadTasks(ctx)
	if err != nil {
		return nil, err
	}
	bySession := map[string]db.DownloadTask{}
	for _, rec := range rows {
		if rec.Platform == req.Platform {
			bySession[rec.SessionID] = rec
		}
	}
	items := make([]*bridge.CheckItem, len(req.SessionIDs))
	var byTitle []int // sessions left for the title match
	seen := map[string]bool{}
	q.mu.Lock()
	for i, sid := range req.SessionIDs {
		if sid == "" || seen[sid] {
			continue
		}
		seen[sid] = true
		if rec, ok := bySession[sid]; ok {
			if t := q.tasks[rec.ID]; t != nil {
				rec = t.rec
			}
			item := bridge.CheckItem{SessionID: sid, State: rec.State, TaskID: rec.ID, CourseID: rec.CourseID,
				MatchedBy: matchedBySession}
			if rec.State == bridge.StateDone {
				item.State = bridge.StateImported
				if rec.LectureID != nil {
					item.LectureID = *rec.LectureID
				}
			}
			if rec.State != bridge.StateDone || rec.LectureID != nil {
				items[i] = &item
				continue
			}
		}
		if i < len(req.Titles) && titleKey(req.Titles[i]) != "" {
			byTitle = append(byTitle, i)
		}
	}
	q.mu.Unlock()
	if len(byTitle) > 0 {
		names, err := q.st.ListLectureNames(ctx, req.CourseID)
		if err != nil {
			return nil, err
		}
		lectures := map[string]db.LectureName{} // titleKey → the oldest lecture with it
		for _, n := range names {
			for _, k := range []string{titleKey(n.Title), titleKey(stripExt(n.OriginalFilename))} {
				if _, dup := lectures[k]; k != "" && !dup {
					lectures[k] = n
				}
			}
		}
		for _, i := range byTitle {
			if n, ok := lectures[titleKey(req.Titles[i])]; ok {
				items[i] = &bridge.CheckItem{SessionID: req.SessionIDs[i], State: bridge.StateImported, LectureID: n.ID,
					CourseID: n.CourseID, MatchedBy: matchedByTitle}
			}
		}
	}
	for _, it := range items {
		if it != nil {
			out.Items = append(out.Items, *it)
		}
	}
	return out, nil
}

// CheckItem.MatchedBy values.
const (
	matchedBySession = "session"
	matchedByTitle   = "title"
)

// titleKey is how check compares titles: lower case, every run of
// whitespace and of the characters / \ : _ - one space, no space at
// either end. Panopto's downloads name a file after the session with "/"
// and ":" made "-" ("CSC1003-L01-01 Wed Oct 21 09-35-00 BST 2026.mp4" is
// the session "CSC1003/L01/01 Wed Oct 21 09:35:00 BST 2026"). Titles are
// cut at 1,000 characters.
func titleKey(s string) string {
	s = truncateRunes(s, 1000)
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || strings.ContainsRune(`/\:_-`, r) {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte(' ')
		}
		gap = false
		b.WriteRune(r)
	}
	return b.String()
}

// stripExt drops a file name's extension: a final dot with 1–5 letters or
// digits ("Lecture 3.5 notes" keeps its ".5 notes").
func stripExt(name string) string {
	if e := filepath.Ext(name); extRe.MatchString(strings.ToLower(e)) {
		return strings.TrimSuffix(name, e)
	}
	return name
}

// checkTracks validates tracks and builds their records: http(s) URLs,
// file, HLS or DASH (with an optional selector), at most one audio track
// and two others (the main picture and the second camera angle, in this
// order; angle.go). The records keep the URL's host and the hash of its
// path, in the order sent.
func checkTracks(tracks []bridge.Track) ([]liveTrack, error) {
	if len(tracks) == 0 {
		return nil, bridge.NewError(bridge.CodeNoTracks, nil)
	}
	audio, other := 0, 0
	out := make([]liveTrack, 0, len(tracks))
	for _, tr := range tracks {
		switch tr.Kind {
		case bridge.KindAudio:
			audio++
		case bridge.KindVideo, bridge.KindUnknown:
			other++
		default:
			return nil, badField("tracks.kind")
		}
		if audio > 1 || other > maxOtherTracks {
			return nil, bridge.NewError(bridge.CodeTooManyTracks, nil)
		}
		if tr.Format != bridge.FormatFile && tr.Format != bridge.FormatHLS && tr.Format != bridge.FormatDASH {
			return nil, badField("tracks.format")
		}
		if !validHTTPURL(tr.URL) {
			return nil, badField("tracks.url")
		}
		if len(tr.Selector) > maxIDLen || hasCTL(tr.Selector) {
			return nil, badField("tracks.selector")
		}
		u, _ := url.Parse(tr.URL)
		rec := trackRecord{Kind: tr.Kind, Format: tr.Format, Role: truncateRunes(tr.Role, 64), Label: cleanTitle(truncateRunes(tr.Label, 200)),
			Quality: truncateRunes(tr.Quality, 64), SizeBytes: boundSize(tr.SizeBytes), Host: u.Host, PathHash: pathHash(u.EscapedPath())}
		if tr.Format == bridge.FormatDASH {
			rec.Selector = tr.Selector
		}
		out = append(out, liveTrack{rec: rec, url: tr.URL})
	}
	return out, nil
}

func checkCreds(cookies []bridge.Cookie, referer string) error {
	if len(cookies) > maxCookies {
		return badField("cookies")
	}
	for _, c := range cookies {
		if !validCookie(c) {
			return badField("cookies")
		}
	}
	if referer != "" && !validHTTPURL(referer) {
		return badField("referer")
	}
	return nil
}

// cookiesForTracks keeps the cookies a browser would send to the host of
// at least one track (domainMatch). The extension reads the cookies of the
// track URLs (cookies.getAll), so it sends no other; one of another host
// (a hand-made message) is left out, so that no redirect, playlist entry
// or segment that leads elsewhere ever carries it: with the cookies a
// request carries only for its own host (cookieHeader), those of a task go
// to the hosts the task named and nowhere else (AUD3 medium-1, the app's
// side). It returns the cookies kept and how many were left out.
func cookiesForTracks(cs []bridge.Cookie, tracks []liveTrack) ([]bridge.Cookie, int) {
	hosts := make([]string, 0, len(tracks))
	for _, t := range tracks {
		if u, err := url.Parse(t.url); err == nil {
			hosts = append(hosts, u.Hostname())
		}
	}
	out := make([]bridge.Cookie, 0, len(cs))
	for _, c := range cs {
		for _, h := range hosts {
			if domainMatch(h, c) {
				out = append(out, c)
				break
			}
		}
	}
	return out, len(cs) - len(out)
}

func (q *Queue) trackCookies(cs []bridge.Cookie, tracks []liveTrack) []bridge.Cookie {
	kept, dropped := cookiesForTracks(cs, tracks)
	if dropped > 0 {
		q.log.Warn("downloads: cookies of no track's host left out", "count", dropped)
	}
	return kept
}

// hostOnly is the host of a "host[:port]" (download_task.page_host, a
// track record's host), without the brackets of an IPv6 address.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// sameSource reports whether a refresh may give a task's tracks the fresh
// URLs matched to them (AUD3 low-1, the app's side): every fresh URL on
// the host of the track it replaces, or the refresh from the page the task
// was sent from (its host: the platform may have moved its media since).
// A page that embeds a player frame of a platform (so that its own media
// passes as that platform's session) is neither: it cannot swap its media
// into a task waiting for a refresh. Sent from another page that embeds
// the player (an LMS) the media is on the same hosts and the refresh goes
// on. An unknown page (no Referer) matches no page.
func sameSource(rec db.DownloadTask, matched []liveTrack, referer string) bool {
	if rec.PageHost != "" && referer != "" {
		if u, err := url.Parse(referer); err == nil && u.Hostname() != "" && sameHost(u.Hostname(), hostOnly(rec.PageHost)) {
			return true
		}
	}
	for _, m := range matched {
		u, err := url.Parse(m.url)
		if err != nil || !sameHost(u.Hostname(), hostOnly(m.rec.Host)) {
			return false
		}
	}
	return true
}

// otherSourceError answers a refresh that fails sameSource: request.invalid
// with the reason, like localRequestError (no new bridge code).
func otherSourceError() error {
	return bridge.NewError(bridge.CodeBadRequest, map[string]any{"field": "tracks.url", "reason": "other_host"})
}

func tracksJSON(ts []liveTrack) string {
	recs := make([]trackRecord, len(ts))
	for i, t := range ts {
		recs[i] = t.rec
	}
	return encodeRecords(recs)
}

// recordedAt normalises the extension's recording time to
// lecture.recorded_at ("" or an unreadable value: unknown). It is the wall
// time the page states, in the zone the page states it in (normally the
// university's), never converted to this computer's zone: a file-name rule
// reads "… 09-35-00 BST 2026.mp4" as 09:35 and the term calendar counts
// weeks in that same wall time, so the lecture gets the same date and
// teaching week whether it was sent by the extension or uploaded as a file,
// wherever the computer is (tz-fix). An RFC 3339 time keeps the clock of
// its own offset ("2026-10-21T09:35:00+01:00" is 09:35).
func recordedAt(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		v := calendar.FormatRecordedAt(t) // t's own offset: the stated clock
		return &v
	}
	// a wall time: the location only carries it, it does not change it
	if t, err := calendar.ParseRecordedAt(s, time.UTC); err == nil {
		v := calendar.FormatRecordedAt(t)
		return &v
	}
	return nil
}

// Enqueue adds a lecture's download. The session is the dedup key: a task
// that has not ended, or a done task whose lecture exists, answers
// queue.duplicate {state, taskId[, lectureId]}; a failed or cancelled task,
// or a done one whose lecture was deleted, is reused.
func (q *Queue) Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	if req.Platform == "" || len(req.Platform) > maxIDLen {
		return nil, badField("platform")
	}
	if req.SessionID == "" || len(req.SessionID) > maxIDLen {
		return nil, badField("sessionId")
	}
	if req.CourseID == "" {
		return nil, badField("courseId")
	}
	if req.MaxConnections < 0 {
		return nil, badField("maxConnections")
	}
	tracks, err := checkTracks(req.Tracks)
	if err != nil {
		return nil, err
	}
	if err := refuseBlocked(req.Referer, tracks); err != nil {
		return nil, err
	}
	if err := q.local.checkTrackHosts(tracks); err != nil {
		return nil, err
	}
	if err := checkCreds(req.Cookies, req.Referer); err != nil {
		return nil, err
	}
	if _, err := q.st.GetCourse(ctx, req.CourseID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, bridge.NewError(bridge.CodeCourseGone, nil)
		}
		return nil, err
	}
	profile := req.Profile.ID
	if profile == "" {
		profile = req.Platform
	}
	rec := db.DownloadTask{
		Platform: req.Platform, SessionID: req.SessionID, ProfileID: truncateRunes(profile, maxIDLen),
		ProfileVersion: truncateRunes(req.Profile.Version, 64), OverrideDisabled: req.OverrideDisabled,
		CourseID: req.CourseID, Title: cleanTitle(req.Lecture.Title), RecordedAt: recordedAt(req.Lecture.RecordedAt),
		DurationSec: max(req.Lecture.DurationSec, 0), TracksJSON: tracksJSON(tracks),
		State: bridge.StateQueued, BytesTotal: -1, MaxConnections: req.MaxConnections,
	}
	if rec.Title == "" {
		rec.Title = "Recording"
		if rec.RecordedAt != nil {
			rec.Title += " " + strings.SplitN(*rec.RecordedAt, "T", 2)[0]
		}
	}
	if u, err := url.Parse(req.Referer); err == nil && req.Referer != "" {
		// the page's host only: its path may name the recording or carry
		// a token, and nothing reads it (security review L3)
		rec.PageHost = u.Host
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if !q.started {
		return nil, ErrNotStarted
	}
	old, err := q.st.FindDownloadTask(ctx, req.Platform, req.SessionID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		rec.CreatedAt = q.nowStr()
		rec.UpdatedAt = rec.CreatedAt
		id, err := q.st.InsertDownloadTask(ctx, rec)
		if err != nil {
			return nil, err
		}
		rec.ID = id
	case err != nil:
		return nil, err
	default:
		if t := q.tasks[old.ID]; t != nil {
			old = t.rec
		}
		switch {
		case old.State == bridge.StateDone && old.LectureID != nil:
			return nil, bridge.NewError(bridge.CodeDuplicate, map[string]any{
				"state": bridge.StateImported, "taskId": old.ID, "lectureId": *old.LectureID})
		case !ended(old.State):
			return nil, bridge.NewError(bridge.CodeDuplicate, map[string]any{"state": old.State, "taskId": old.ID})
		}
		// sent again: the row is reused; partial files are kept only for
		// the very same tracks, and not when the import refused the file
		// (ImportRefused: a sign-in page, say; the same file fails again)
		if t := q.tasks[old.ID]; t != nil {
			t.forget()
			delete(q.tasks, old.ID)
		}
		if !sameTracks(old.TracksJSON, rec.TracksJSON) || (old.State == bridge.StateFailed && RefusedFile(old.ErrorCode)) {
			removeAllRetry(q.taskDir(old.ID))
		} else if old.State == bridge.StateFailed {
			// the partial files stay (a cancelled or done task has none
			// left): so does the progress they hold
			rec.BytesDone, rec.BytesTotal = old.BytesDone, old.BytesTotal
		}
		rec.ID, rec.CreatedAt = old.ID, old.CreatedAt
	}
	t := &task{rec: rec, tracks: tracks, cookies: q.trackCookies(req.Cookies, tracks), referer: req.Referer}
	if err := q.queueLocked(t); err != nil {
		if errors.Is(err, errGone) {
			return nil, bridge.NewError(bridge.CodeCourseGone, nil)
		}
		return nil, err
	}
	q.log.Info("downloads: task queued", "task", rec.ID, "platform", rec.Platform, "tracks", len(tracks),
		"first", logURL(tracks[0].url))
	return &bridge.EnqueueReply{ReplyHeader: header(bridge.TypeEnqueue, req.ID), TaskID: rec.ID,
		Position: q.positionLocked(t)}, nil
}

// Refresh gives a task new credentials (the tracks again, matched to the
// task's by role, then by kind; the format must stay; each on the host of
// the track it replaces unless sent from the task's own page: sameSource).
// A needs_refresh or
// failed task waits for a slot again and resumes where it stopped (a
// failed task whose file the import refused starts over); a
// waiting task takes the new credentials; a running or done task is left
// alone and its state answered. A cancelled task is refresh.not_found.
func (q *Queue) Refresh(ctx context.Context, req *bridge.RefreshRequest) (*bridge.RefreshReply, error) {
	if req.Platform == "" || req.SessionID == "" {
		return nil, badField("sessionId")
	}
	fresh, err := checkTracks(req.Tracks)
	if err != nil {
		return nil, err
	}
	if err := refuseBlocked(req.Referer, fresh); err != nil {
		return nil, err
	}
	if err := q.local.checkTrackHosts(fresh); err != nil {
		return nil, err
	}
	if err := checkCreds(req.Cookies, req.Referer); err != nil {
		return nil, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if !q.started {
		return nil, ErrNotStarted
	}
	rec, err := q.st.FindDownloadTask(ctx, req.Platform, req.SessionID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, bridge.NewError(bridge.CodeTaskNotFound, nil)
	}
	if err != nil {
		return nil, err
	}
	t := q.tasks[rec.ID]
	if t == nil {
		t = &task{rec: rec}
	}
	reply := func() *bridge.RefreshReply {
		return &bridge.RefreshReply{ReplyHeader: header(bridge.TypeRefresh, req.ID), TaskID: t.rec.ID, State: t.rec.State}
	}
	switch t.rec.State {
	case bridge.StateCancelled:
		return nil, bridge.NewError(bridge.CodeTaskNotFound, nil)
	case bridge.StateDone, bridge.StateDownloading, bridge.StateMerging, bridge.StateImporting:
		return reply(), nil
	}
	matched, err := matchTracks(recordsOf(t.rec), fresh)
	if err != nil {
		return nil, err
	}
	if !sameSource(t.rec, matched, req.Referer) {
		q.log.Warn("downloads: a refresh from another page with tracks on other hosts refused", "task", t.rec.ID)
		return nil, otherSourceError()
	}
	t.tracks, t.cookies, t.referer = matched, q.trackCookies(req.Cookies, matched), req.Referer
	if t.rec.State == bridge.StateQueued {
		return reply(), nil
	}
	if t.rec.State == bridge.StateFailed && RefusedFile(t.rec.ErrorCode) {
		// the import refused the file (ImportRefused: a sign-in page, say):
		// it is downloaded anew with the new credentials, as Enqueue does,
		// not imported again to be refused again (R12 D-3). The directory
		// is this task's alone (taskDir: the row's id), and no runner holds
		// it: a task is saved failed by its runner as it ends (run).
		removeAllRetry(q.taskDir(t.rec.ID))
		t.rec.BytesDone, t.rec.BytesTotal = 0, -1
	}
	if err := q.queueLocked(t); err != nil {
		if errors.Is(err, errGone) {
			return nil, bridge.NewError(bridge.CodeTaskNotFound, nil)
		}
		return nil, err
	}
	q.log.Info("downloads: task refreshed", "task", t.rec.ID)
	return reply(), nil
}

// matchTracks gives each stored track the URL of a fresh one: same role
// first, then same kind.
func matchTracks(stored []trackRecord, fresh []liveTrack) ([]liveTrack, error) {
	used := make([]bool, len(fresh))
	find := func(same func(liveTrack) bool) int {
		for k, f := range fresh {
			if !used[k] && same(f) {
				return k
			}
		}
		return -1
	}
	out := make([]liveTrack, len(stored))
	for i, s := range stored {
		k := -1
		if s.Role != "" {
			k = find(func(f liveTrack) bool { return f.rec.Role == s.Role })
		}
		if k < 0 {
			k = find(func(f liveTrack) bool { return f.rec.Kind == s.Kind })
		}
		if k < 0 {
			return nil, badField("tracks")
		}
		used[k] = true
		if fresh[k].rec.Format != s.Format {
			return nil, badField("tracks.format")
		}
		out[i] = liveTrack{rec: s, url: fresh[k].url}
	}
	return out, nil
}

// Subscribe delivers a status for every task that has not ended (also
// those waiting for a refresh), then one per change, until ctx ends.
func (q *Queue) Subscribe(ctx context.Context) <-chan bridge.Status {
	s := newSubscriber(q.o.StatusInterval)
	sctx, cancel := context.WithCancel(ctx)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		cancel()
		ch := make(chan bridge.Status)
		close(ch)
		return ch
	}
	q.subs[s] = cancel
	q.mu.Unlock()
	if rows, err := q.st.ListDownloadTasks(sctx); err == nil {
		q.mu.Lock()
		for _, rec := range rows {
			t := q.tasks[rec.ID]
			if t != nil {
				rec = t.rec
			}
			if ended(rec.State) {
				continue
			}
			s.push(q.statusLocked(rec, t))
		}
		q.mu.Unlock()
	} else if sctx.Err() == nil {
		q.log.Warn("downloads: status subscription without the initial list", "err", err)
	}
	go s.run(sctx, func() {
		cancel()
		q.mu.Lock()
		delete(q.subs, s)
		q.mu.Unlock()
	})
	return s.out
}

// LocateTask answers the bridge's "open" by task id (cmd's TaskLocator):
// the task's course and, once imported, its lecture; open.not_found for
// an unknown task.
func (q *Queue) LocateTask(ctx context.Context, taskID int64) (courseID, lectureID string, err error) {
	q.mu.Lock()
	if t := q.tasks[taskID]; t != nil {
		courseID = t.rec.CourseID
		if t.rec.LectureID != nil {
			lectureID = *t.rec.LectureID
		}
		q.mu.Unlock()
		return courseID, lectureID, nil
	}
	q.mu.Unlock()
	rec, err := q.st.GetDownloadTask(ctx, taskID)
	if errors.Is(err, db.ErrNotFound) {
		return "", "", bridge.NewError(bridge.CodeNotFound, nil)
	}
	if err != nil {
		return "", "", err
	}
	if rec.LectureID != nil {
		lectureID = *rec.LectureID
	}
	return rec.CourseID, lectureID, nil
}
