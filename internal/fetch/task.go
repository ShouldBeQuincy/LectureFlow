// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/bridge"
	lfmedia "lectureflow/internal/media"
)

// A task's directory, {data_dir}/downloads/{id}/:
//
//	t{i}{ext}            track i, finished (a file track, or a joined HLS track)
//	t{i}{ext}.part       a file track while aria2c downloads it (+ .part.aria2)
//	t{i}/                an HLS or DASH track's parts: src{ext}(.part) for
//	                     a single file, else index.m3u8 with s*/k*/m* files
//	                     (HLS) or init{ext} with s*.seg files (DASH), which
//	                     the join turns into joined{ext}; a/ the same for a
//	                     separate audio rendition or adaptation set;
//	                     plan.id names what the parts belong to;
//	                     ready.json once every part is there
//	angle2.mp4           the second camera angle remuxed (angle.go)
//	media{ext}           the file handed to the import (made last)
//
// Every stage leaves its result under a final name only when it is
// complete, so a retry or a refresh starts where the last run stopped.

// runSnapshot is what a runner needs, copied under the queue's lock.
type runSnapshot struct {
	id         int64
	records    []trackRecord
	urls       []string // nil = no credentials
	cookies    []bridge.Cookie
	referer    string
	conns      int
	courseID   string
	title      string
	recordedAt *string
	// net is the run's address policy and HTTP client (local.go; nil
	// outside a run: Queue.netOf).
	net *taskNet
	// guard is the run's session of the guard proxy, through which aria2c
	// makes every connection of the run's downloads (guard.go; set by
	// download for as long as aria2c works for the run).
	guard *guardSession
	// watch looks at the free space and the task directory's size while
	// aria2c writes (disklimit.go; set by download).
	watch *diskWatch
}

// trackPlan is t{i}/ready.json: how to join an HLS or DASH track.
type trackPlan struct {
	Main  trackPart  `json:"main"`
	Audio *trackPart `json:"audio,omitempty"` // a separate audio rendition (HLS) or adaptation set (DASH)
}

type trackPart struct {
	Dir      string   `json:"dir"`                // relative to the task directory
	Source   string   `json:"source,omitempty"`   // one file: the downloaded file
	Playlist string   `json:"playlist,omitempty"` // HLS segments: the local playlist
	Parts    []string `json:"parts,omitempty"`    // DASH: init section and segments, in order
	Ext      string   `json:"ext,omitempty"`      // DASH: the container (.mp4, .m4a, .webm)
}

func trackName(i int) string { return "t" + strconv.Itoa(i) }

// findFinal returns the finished file stem{ext} in dir ("" = none): not a
// partial download (.part, .aria2), not a temporary file (.tmp).
func findFinal(dir, stem string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, stem+".*"))
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".aria2") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			return m
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// trackDownloaded: track i needs nothing more from the network (the second
// angle also once it is remuxed: its track file is gone then).
func trackDownloaded(dir string, i int, lay trackLayout) bool {
	return findFinal(dir, trackName(i)) != "" || fileExists(filepath.Join(dir, trackName(i), "ready.json")) ||
		(i == lay.angle && angleDone(dir))
}

// downloadsComplete: the task can go on without credentials. media{ext} is
// made last, so it means every other result (the second angle) is there.
func downloadsComplete(dir string, records []trackRecord) bool {
	if findFinal(dir, "media") != "" {
		return true
	}
	if len(records) == 0 {
		return false
	}
	lay := layoutOf(records)
	for i := range records {
		if !trackDownloaded(dir, i, lay) {
			return false
		}
	}
	return true
}

// process runs a task to done; any error is how it stopped. A failure
// that leaves nothing worth keeping takes the task's files with it
// (discardAfter).
func (q *Queue) process(ctx context.Context, t *task, s runSnapshot) (err error) {
	dir, err := filepath.Abs(q.taskDir(s.id))
	if err != nil {
		return writeFailure(err)
	}
	defer func() { q.discardAfter(ctx, err, s.id, dir) }()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return writeFailure(err)
	}
	media := findFinal(dir, "media")
	if media == "" {
		if !downloadsComplete(dir, s.records) {
			if s.urls == nil {
				return &failure{refresh: true, code: CodeRestarted}
			}
			if allExpired(s.cookies, q.o.Now()) {
				return &failure{refresh: true, code: CodeCredentialsExpired}
			}
			if err := q.checkSpace(s, dir); err != nil {
				return err
			}
			if err := q.download(ctx, t, s, dir); err != nil {
				return err
			}
		}
		if media, err = q.merge(ctx, t, s, dir); err != nil {
			return err
		}
	}
	if err := q.setState(t, bridge.StateImporting); err != nil {
		return err
	}
	return q.importMedia(ctx, t, s, media)
}

// checkSpace is the check before a download (disklimit.go): the reserve
// and what the reported sizes still lack beyond the bytes in the task's
// directory dir.
func (q *Queue) checkSpace(s runSnapshot, dir string) error {
	if q.o.MinFree < 0 {
		return nil
	}
	if err := os.MkdirAll(q.o.DataDir, 0o755); err != nil {
		return writeFailure(err)
	}
	free, err := q.o.FreeSpace(q.o.DataDir)
	if err != nil {
		return nil // unknown: do not block the user
	}
	need := q.o.MinFree + missingBytes(s.records, dir)
	if free < need {
		return &failure{code: CodeNoSpace, params: map[string]any{"free_bytes": free, "keep_bytes": q.o.MinFree}}
	}
	return nil
}

// download fetches every track that is not there yet, in parallel.
func (q *Queue) download(ctx context.Context, t *task, s runSnapshot, dir string) error {
	// a proxy aria2c cannot use: the task fails before any request of its
	// own and before aria2c is started (local.go upstream); the URLs a
	// playlist leads to are judged one by one later (resolver.checkHost)
	for _, raw := range s.urls {
		if u, err := url.Parse(raw); err == nil {
			if _, err := q.netOf(s).addr.p.upstream(u); err != nil {
				return asFailure(err)
			}
		}
	}
	inst, err := q.eng.acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errEngineMissing) {
			return &failure{code: CodeEngineMissing}
		}
		return &failure{code: CodeEngineFailed, detail: err.Error()}
	}
	defer q.eng.release()
	// aria2c reaches the servers through the guard proxy alone, judged by
	// the task's own rules; the session ends with the downloads, and its
	// connections with it
	sess, err := inst.guard.open(q.netOf(s).addr)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &failure{code: CodeEngineFailed, detail: err.Error()}
	}
	defer sess.close()
	s.guard = sess
	s.watch = q.newDiskWatch(dir)
	lay := layoutOf(s.records)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(s.records))
	for i, r := range s.records {
		if trackDownloaded(dir, i, lay) {
			q.progressDone(t, i, dir, lay)
			continue
		}
		// without an audio track the main picture brings the audio of its
		// manifest; the second angle never has audio
		withAudio := lay.audio < 0 && i == lay.main
		wg.Add(1)
		go func(i int, r trackRecord) {
			defer wg.Done()
			var err error
			switch r.Format {
			case bridge.FormatHLS:
				err = q.downloadHLS(gctx, inst, t, s, dir, i, r, withAudio)
			case bridge.FormatDASH:
				err = q.downloadDASH(gctx, inst, t, s, dir, i, r, withAudio)
			default:
				err = q.downloadFile(gctx, inst, t, s, dir, i, r)
			}
			if err != nil {
				errs[i] = err
				cancel()
			}
		}(i, r)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// the first real failure (the others were cancelled because of it)
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// progressDone reports a track found complete on disk.
func (q *Queue) progressDone(t *task, i int, dir string, lay trackLayout) {
	var n int64
	f := findFinal(dir, trackName(i))
	if f == "" && i == lay.angle && angleDone(dir) {
		f = filepath.Join(dir, AngleFile)
	}
	if f != "" {
		if fi, err := os.Stat(f); err == nil {
			n = fi.Size()
		}
	} else {
		n = dirSize(filepath.Join(dir, trackName(i)))
	}
	q.progress(t, i, n, n)
}

// dirSize is the bytes of the regular files under dir (not following
// links or junctions; a file gone meanwhile, or a directory that cannot be
// read, counts nothing).
//
// A partial download aria2c is writing (name.part: fetchItems) is measured
// on its own (Lstat): on Windows the directory listing's size
// (DirEntry.Info) lags behind for a file another handle still writes, so
// aria2c's partial files counted as nearly empty (SEC43 low-2, the size
// limit's check). Every other file is complete, and the listing's size,
// which Windows gives without a system call per file, is right: one Lstat
// per file made a look at 20,000 segments take about half a second, in
// diskWatch's lock, every Options.SpaceInterval (SEC43b low-6).
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		var fi os.FileInfo
		if strings.HasSuffix(d.Name(), ".part") {
			fi, err = lstatFile(p)
		} else {
			fi, err = d.Info()
		}
		if err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// lstatFile is os.Lstat (a test counts dirSize's calls).
var lstatFile = os.Lstat

func defaultExt(kind string) string {
	if kind == bridge.KindAudio {
		return ".m4a"
	}
	return ".mp4"
}

// fileTrackExt is the extension of a file track's local file, t{i}{ext},
// which becomes the lecture's media{ext} and the end of its
// original_filename: the URL's when it is a lecture media extension
// (media.Exts, the check of an upload's name), else the kind's default
// (AUD3 low-3). Never .part, .tmp or .aria2, which findFinal takes for an
// unfinished download (AUD3 info-1), nor .exe, .html or a playlist's. The
// content decides how it is read (media.Check, ffmpeg's probe with the
// whitelists), not the name.
func fileTrackExt(rawURL, kind string) string {
	def := defaultExt(kind)
	if e := extOf(rawURL, def); lfmedia.AllowedName("t" + e) {
		return e
	}
	return def
}

func (q *Queue) downloadFile(ctx context.Context, inst *instance, t *task, s runSnapshot, dir string, i int, r trackRecord) error {
	u := s.urls[i]
	name := trackName(i) + fileTrackExt(u, r.Kind)
	items := []fetchItem{{url: u, name: name}}
	return q.fetchItems(ctx, inst, s, dir, items, s.conns, 1, func(done, total int64) { q.progress(t, i, done, total) })
}

// downloadHLS reads the playlists (with the task's cookies) and fetches
// the parts; withAudio: the task has no audio track, so a separate audio
// rendition of the chosen video variant is fetched too.
func (q *Queue) downloadHLS(ctx context.Context, inst *instance, t *task, s runSnapshot, dir string, i int, r trackRecord, withAudio bool) error {
	body, base, err := q.getPlaylist(ctx, s.urls[i], s)
	if err != nil {
		return err
	}
	extra := ""
	if isMasterPlaylist(body) {
		m, err := parseMaster(body, base)
		if err != nil {
			return playlistFailure(err)
		}
		var media string
		media, extra = m.pick(r.Kind)
		if !withAudio || r.Kind == bridge.KindAudio {
			extra = ""
		}
		if body, base, err = q.getPlaylist(ctx, media, s); err != nil {
			return err
		}
	}
	main, err := parseMedia(body, base)
	if err != nil {
		return playlistFailure(err)
	}
	var audio *mediaPlaylist
	if extra != "" {
		ab, abase, err := q.getPlaylist(ctx, extra, s)
		if err != nil {
			return err
		}
		if audio, err = parseMedia(ab, abase); err != nil {
			return playlistFailure(err)
		}
	}
	sub := filepath.Join(dir, trackName(i))
	plan := trackPlan{}
	if plan.Main, err = q.fetchPart(ctx, inst, s, sub, "", main, q.trackReport(t, i, 0)); err != nil {
		return err
	}
	if audio != nil {
		part, err := q.fetchPart(ctx, inst, s, sub, "a", audio, q.trackReport(t, i, dirSize(sub)))
		if err != nil {
			return err
		}
		plan.Audio = &part
	}
	return writeReady(sub, plan)
}

// trackReport reports a part's progress as track i's, after base bytes of
// earlier parts.
func (q *Queue) trackReport(t *task, i int, base int64) func(done, total int64) {
	return func(done, total int64) {
		if total >= 0 {
			total += base
		}
		q.progress(t, i, base+done, total)
	}
}

func writeReady(sub string, plan trackPlan) error {
	b, _ := json.Marshal(plan)
	if err := writeFileAtomic(filepath.Join(sub, "ready.json"), b); err != nil {
		return writeFailure(err)
	}
	return nil
}

// preparePartDir makes dir the home of the parts of urls: plan.id names
// them (a hash of their file names, no URL); when it names other parts
// (the extension sent another variant or adaptation set since) the
// directory is emptied first, so a resume never mixes two of them.
func preparePartDir(dir string, urls []string) error {
	h := sha256.New()
	for _, u := range urls {
		name := u
		if p, err := url.Parse(u); err == nil {
			name = path.Base(p.Path)
		}
		io.WriteString(h, name)
		h.Write([]byte{0})
	}
	id := hex.EncodeToString(h.Sum(nil))[:16]
	idFile := filepath.Join(dir, "plan.id")
	if b, err := os.ReadFile(idFile); err == nil && string(b) == id {
		return nil
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(idFile, []byte(id))
}

// downloadDASH reads the MPD (with the task's cookies), plans the track's
// AdaptationSet (dash.go) and fetches its parts; withAudio: the task has
// no audio track, so the MPD's audio is fetched too for a video track.
func (q *Queue) downloadDASH(ctx context.Context, inst *instance, t *task, s runSnapshot, dir string, i int, r trackRecord, withAudio bool) error {
	body, base, err := q.getPlaylist(ctx, s.urls[i], s)
	if err != nil {
		return err
	}
	m, err := parseMPD(body, base)
	if err != nil {
		return playlistFailure(err)
	}
	set, err := m.find(r.Kind, r.Selector)
	if err != nil {
		return playlistFailure(err)
	}
	if r.Selector != "" && set.selector() != r.Selector {
		q.log.Warn("downloads: the DASH selector names no adaptation set; the first of its kind is used",
			"task", s.id, "track", i, "selector", r.Selector, "used", set.selector())
	}
	main, err := m.source(set)
	if err != nil {
		return playlistFailure(err)
	}
	var audio *dashSource
	if withAudio && set.kind == bridge.KindVideo {
		if as := m.audioFor(); as != nil {
			if audio, err = m.source(as); err != nil {
				return playlistFailure(err)
			}
		}
	}
	sub := filepath.Join(dir, trackName(i))
	plan := trackPlan{}
	if plan.Main, err = q.fetchDASHPart(ctx, inst, s, sub, "", main, q.trackReport(t, i, 0)); err != nil {
		return err
	}
	if audio != nil {
		part, err := q.fetchDASHPart(ctx, inst, s, sub, "a", audio, q.trackReport(t, i, dirSize(sub)))
		if err != nil {
			return err
		}
		plan.Audio = &part
	}
	return writeReady(sub, plan)
}

// fetchDASHPart downloads one Representation into sub/rel: its one file
// (split into conns connections), or its init section and segments (up to
// conns at a time), which the join concatenates.
func (q *Queue) fetchDASHPart(ctx context.Context, inst *instance, s runSnapshot, sub, rel string, d *dashSource, report func(done, total int64)) (trackPart, error) {
	dir := filepath.Join(sub, rel)
	if err := preparePartDir(dir, d.resources()); err != nil {
		return trackPart{}, writeFailure(err)
	}
	part := trackPart{Dir: filepath.ToSlash(filepath.Join(trackDirRel(sub), rel)), Ext: d.ext}
	if d.single != "" {
		name := singleFileName(d.single, d.ext)
		if err := q.fetchItems(ctx, inst, s, dir, []fetchItem{{url: d.single, name: name}}, s.conns, 1, report); err != nil {
			return part, err
		}
		part.Source = name
		return part, nil
	}
	var items []fetchItem
	if d.init != "" {
		items = append(items, fetchItem{url: d.init, name: "init" + d.ext})
	}
	for k, u := range d.media {
		items = append(items, fetchItem{url: u, name: fmt.Sprintf("s%06d.seg", k+1)})
	}
	if err := q.fetchItems(ctx, inst, s, dir, items, 1, s.conns, report); err != nil {
		return part, err
	}
	for _, it := range items {
		part.Parts = append(part.Parts, it.name)
	}
	return part, nil
}

// fetchPart downloads one media playlist's resources into sub/rel: as one
// file when the playlist is a single file in consecutive byte ranges (the
// fast path: aria2c splits the file), else every segment, key and init
// section with up to conns at a time plus a local playlist.
func (q *Queue) fetchPart(ctx context.Context, inst *instance, s runSnapshot, sub, rel string, m *mediaPlaylist, report func(done, total int64)) (trackPart, error) {
	dir := filepath.Join(sub, rel)
	if err := preparePartDir(dir, m.resources); err != nil {
		return trackPart{}, writeFailure(err)
	}
	part := trackPart{Dir: filepath.ToSlash(filepath.Join(trackDirRel(sub), rel))}
	if uri, minSize, ok := m.singleFile(); ok {
		name := singleFileName(uri, ".mp4")
		if err := q.fetchItems(ctx, inst, s, dir, []fetchItem{{url: uri, name: name}}, s.conns, 1, report); err != nil {
			return part, err
		}
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() < minSize {
			os.Remove(filepath.Join(dir, name))
			return part, &failure{code: CodePlaylistInvalid, detail: fmt.Sprintf("the file is shorter than its playlist (%d bytes expected)", minSize)}
		}
		part.Source = name
		return part, nil
	}
	if err := writeFileAtomic(filepath.Join(dir, "index.m3u8"), m.local()); err != nil {
		return part, writeFailure(err)
	}
	items := make([]fetchItem, len(m.resources))
	for k, u := range m.resources {
		items[k] = fetchItem{url: u, name: m.names[u]}
	}
	if err := q.fetchItems(ctx, inst, s, dir, items, 1, s.conns, report); err != nil {
		return part, err
	}
	part.Playlist = "index.m3u8"
	return part, nil
}

func trackDirRel(sub string) string { return filepath.Base(sub) }

// singleFileName is the local name of the one file of an HLS byte-range
// playlist or of a DASH SegmentBase Representation: "src" and the URL's
// extension when it is a media file's, def without one, else the neutral
// .seg (hlsPartExt, the rule of the segments' names). Never .tmp, which a
// failure for want of space takes for a temporary file and deletes
// (removeTempFiles) while ready.json still names it, so that every retry's
// merge failed (SEC43b info-8); nor .part or .aria2 (aria2c's), nor a
// playlist's. The "*.tmp is temporary" rule stays one for every writer
// (writeFileAtomic, the joins, ffmpeg's outputs, the angle) rather than a
// list of their names to keep in step.
func singleFileName(rawURL, def string) string { return "src" + hlsPartExt(rawURL, def) }

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// getPlaylist reads a playlist (or an MPD) with the task's cookies and
// referer; each redirect hop gets the cookies of its own host (client).
// The base for relative URIs is the URL after redirects.
func (q *Queue) getPlaylist(ctx context.Context, rawURL string, s runSnapshot) ([]byte, *url.URL, error) {
	if err := q.netOf(s).addr.checkURL(ctx, rawURL, false); err != nil {
		return nil, nil, localFailure(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, &failure{code: CodePlaylistInvalid, detail: "bad playlist URL"}
	}
	for _, h := range requestHeaders(rawURL, s.cookies, s.referer, q.o.Now()) {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Set(k, v)
	}
	res, err := q.client(s).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, netFailure(err)
	}
	defer res.Body.Close()
	if f := statusFailure(res.StatusCode); f != nil {
		return nil, nil, f
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, &failure{code: CodeNetwork, detail: redactText(err.Error())}
	}
	return b, res.Request.URL, nil
}

// statusFailure maps an HTTP status (nil for success).
func statusFailure(status int) *failure {
	switch {
	case status < 400:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &failure{refresh: true, code: CodeCredentialsRejected, params: map[string]any{"status": status}}
	}
	return &failure{code: CodeHTTPStatus, params: map[string]any{"status": status}}
}

// drmFeatures are the errPlaylist features that mean DRM (CodeDRMProtected).
var drmFeatures = map[string]bool{"drm": true, "sample-aes": true}

func playlistFailure(err error) error {
	var pe *errPlaylist
	if errors.As(err, &pe) && drmFeatures[pe.feature] {
		return &failure{code: CodeDRMProtected, detail: "protected stream: " + pe.feature}
	}
	if errors.As(err, &pe) && pe.feature != "" {
		return &failure{code: CodePlaylistUnsupported, params: map[string]any{"feature": pe.feature}}
	}
	return &failure{code: CodePlaylistInvalid, detail: err.Error()}
}

var urlInText = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// redactText keeps track URLs out of a message (an aria2 error, a Go
// network error quoting the URL): each becomes its host and path hash
// (logURL), as the query may be signed and the path a credential. The
// scheme may be in any case ("HTTPS://" as the extension sent it; fuzz).
func redactText(s string) string {
	return urlInText.ReplaceAllStringFunc(s, logURL)
}

func writeFailure(err error) error {
	if isDiskFull(err) {
		return &failure{code: CodeNoSpace, detail: err.Error()} // the queue fills in the free space
	}
	return &failure{code: CodeWriteFailed, detail: err.Error()}
}

// asFailure turns any runner error into a failure.
func asFailure(err error) *failure {
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	if f := localFailure(err); f != nil {
		return f
	}
	if f := proxyFailure(err); f != nil {
		return f
	}
	var ne net.Error
	if errors.As(err, &ne) || isTLSError(err) {
		return netFailure(err)
	}
	return &failure{code: CodeEngineFailed, detail: redactText(err.Error())}
}

// netFailure is a network error of Go's own requests (playlists, redirect
// probes): CodeLocalAddress when a hop or an address was refused (local.go),
// CodeTLSFailed when the server's certificate was refused, else
// CodeNetwork.
func netFailure(err error) *failure {
	if f := localFailure(err); f != nil {
		return f
	}
	if isTLSError(err) {
		return &failure{code: CodeTLSFailed, detail: redactText(err.Error())}
	}
	return &failure{code: CodeNetwork, detail: redactText(err.Error())}
}

// isTLSError reports a refused server certificate.
func isTLSError(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci)
}

// ariaTLSMessage: aria2c's words for a refused certificate ("SSL/TLS
// handshake failure: Invalid certificate chain" on macOS, OpenSSL's and
// Windows' "certificate verify failed" and the like).
var ariaTLSMessage = regexp.MustCompile(`(?i)SSL/TLS handshake failure|certificate`)

// ---------- aria2c downloads ----------

type fetchItem struct {
	url  string
	name string // final file name in the directory
}

// fetchItems downloads items into dir with aria2c, at most window at a
// time, each with split connections; a finished item is renamed from
// name.part to name, and items already there are skipped. report gets the
// bytes so far and the expected total (-1 = unknown).
func (q *Queue) fetchItems(ctx context.Context, inst *instance, s runSnapshot, dir string, items []fetchItem, split, window int, report func(done, total int64)) error {
	if window < 1 {
		window = 1
	}
	if split < 1 {
		split = 1
	}
	var pending []fetchItem
	var doneBytes int64
	completed := 0
	for _, it := range items {
		if fi, err := os.Stat(filepath.Join(dir, it.name)); err == nil && fi.Mode().IsRegular() {
			doneBytes += fi.Size()
			completed++
			continue
		}
		pending = append(pending, it)
	}
	active := map[string]fetchItem{}
	var order []string
	// stopAll stops the downloads in flight; one that completed meanwhile
	// (between two polls) still gets its final name, or the next run would
	// ask the server for the bytes after its end (416)
	stopAll := func() {
		gids := append([]string(nil), order...)
		for _, g := range q.stopGIDs(inst, gids) {
			if it, ok := active[g]; ok {
				_ = renameRetry(filepath.Join(dir, it.name+".part"), filepath.Join(dir, it.name))
			}
		}
	}
	estimate := func(activeDone, activeTotal int64, nActive int) int64 {
		if len(items) == 1 {
			if completed == 1 {
				return doneBytes
			}
			if activeTotal > 0 {
				return activeTotal
			}
			return -1
		}
		if completed == 0 {
			return -1
		}
		est := doneBytes / int64(completed) * int64(len(items))
		if min := doneBytes + activeDone; est < min {
			est = min
		}
		return est
	}
	report(doneBytes, estimate(0, 0, 0))
	res := q.newResolver(s)
	// every download is given the guard proxy with the run's session: no
	// session, no download (a caller outside download: one for this call)
	sess := s.guard
	if sess == nil {
		var err error
		if sess, err = inst.guard.open(q.netOf(s).addr); err != nil {
			return engineFailure(inst, err)
		}
		defer sess.close()
	}
	tick := time.NewTicker(q.o.PollInterval)
	defer tick.Stop()
	for len(pending) > 0 || len(active) > 0 {
		if free := window - len(active); free > 0 && len(pending) > 0 {
			batch := pending[:min(free, len(pending))]
			pending = pending[len(batch):]
			urls := make([]string, len(batch))
			for k, it := range batch {
				urls[k] = it.url
			}
			// ruling 5: with cookies, aria2c gets the URL after the
			// redirects and the cookies of its host only
			finals, err := res.finals(ctx, urls)
			if err != nil {
				stopAll()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			for k, it := range batch {
				if ctx.Err() != nil {
					break // the select below stops what was added
				}
				opts := map[string]any{
					"dir": dir, "out": it.name + ".part",
					"split": strconv.Itoa(split), "max-connection-per-server": strconv.Itoa(split), "min-split-size": "1M",
				}
				if h := requestHeaders(finals[k], s.cookies, s.referer, q.o.Now()); len(h) > 0 {
					opts["header"] = h
				}
				// aria2c's proxy is the guard, whatever the URL (guard.go)
				for name, v := range sess.options() {
					opts[name] = v
				}
				gid, err := inst.rpc.addURIDetached(ctx, finals[k], opts)
				if err != nil {
					stopAll()
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return engineFailure(inst, err)
				}
				active[gid] = it
				order = append(order, gid)
			}
		}
		select {
		case <-ctx.Done():
			stopAll()
			return ctx.Err()
		case <-tick.C:
		}
		sts, err := inst.rpc.tellStatuses(ctx, order)
		if err != nil {
			stopAll()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return engineFailure(inst, err)
		}
		// the guard refused a connection (a redirect to this computer, the
		// local network or a site LectureFlow does not handle): the task
		// fails with the rule's code, whatever aria2c makes of the refusal
		if err := sess.refused(); err != nil {
			stopAll()
			return asFailure(err)
		}
		var activeDone, activeTotal int64
		for _, st := range sts {
			it, ok := active[st.GID]
			if !ok {
				continue
			}
			if len(st.FollowedBy) > 0 {
				// a Metalink or a .torrent that aria2c followed despite
				// --follow-metalink=false / --follow-torrent=false: stop
				// what it started, the content is not the media
				q.stopGIDs(inst, st.FollowedBy)
				stopAll()
				return &failure{code: CodeEngineFailed, detail: "aria2c followed a Metalink or a torrent instead of downloading the file"}
			}
			switch st.Status {
			case "complete":
				if err := renameRetry(filepath.Join(dir, it.name+".part"), filepath.Join(dir, it.name)); err != nil {
					stopAll()
					return writeFailure(err)
				}
				doneBytes += num(st.CompletedLength)
				completed++
				delete(active, st.GID)
				order = remove(order, st.GID)
				_ = inst.rpc.removeResult(ctx, st.GID)
			case "error":
				stopAll()
				return sess.failureOf(st)
			case "removed", "":
				stopAll()
				return &failure{code: CodeEngineFailed, detail: "aria2c dropped a download"}
			default:
				activeDone += num(st.CompletedLength)
				activeTotal += num(st.TotalLength)
			}
		}
		report(doneBytes+activeDone, estimate(activeDone, activeTotal, len(active)))
		// the disk (disklimit.go): what these files will hold, the free
		// space and the task's directory (after the report: a task stopped
		// here keeps the progress its files hold)
		if err := q.overDeclared(doneBytes, activeDone, activeTotal); err != nil {
			stopAll()
			return err
		}
		if err := s.watch.check(); err != nil {
			stopAll()
			return err
		}
	}
	return nil
}

func remove(list []string, v string) []string {
	for i, x := range list {
		if x == v {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func renameRetry(from, to string) error {
	var err error
	for k := 0; k < 20; k++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// stopGIDs removes downloads from aria2c and waits until it no longer
// writes their files (the partial files stay for a later resume). It
// returns the downloads that had completed before they could be removed
// (their .part files are whole).
func (q *Queue) stopGIDs(inst *instance, gids []string) (complete []string) {
	if len(gids) == 0 || !inst.alive() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, g := range gids {
		_ = inst.rpc.forceRemove(ctx, g)
	}
	for ctx.Err() == nil {
		sts, err := inst.rpc.tellStatuses(ctx, gids)
		if err != nil {
			break
		}
		busy := false
		complete = complete[:0]
		for _, st := range sts {
			switch st.Status {
			case "active", "waiting", "paused":
				busy = true
			case "complete":
				complete = append(complete, st.GID)
			}
		}
		if !busy {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, g := range gids {
		_ = inst.rpc.removeResult(ctx, g)
	}
	return complete
}

func engineFailure(inst *instance, err error) error {
	if !inst.alive() {
		return &failure{code: CodeEngineFailed, detail: "aria2c exited"}
	}
	return &failure{code: CodeEngineFailed, detail: redactText(err.Error())}
}

var statusInMessage = regexp.MustCompile(`status=(\d{3})`)

// ariaFailure maps a failed aria2 download (aria2's exit status codes).
func ariaFailure(st ariaStatus) error {
	detail := "aria2 error " + st.ErrorCode
	if st.ErrorMessage != "" {
		detail += ": " + redactText(st.ErrorMessage)
	}
	if ariaTLSMessage.MatchString(st.ErrorMessage) {
		return &failure{code: CodeTLSFailed, detail: detail}
	}
	switch st.ErrorCode {
	case "24": // HTTP authorization failed
		return &failure{refresh: true, code: CodeCredentialsRejected, params: map[string]any{"status": 401}, detail: detail}
	case "22": // bad HTTP status
		if m := statusInMessage.FindStringSubmatch(st.ErrorMessage); m != nil {
			n, _ := strconv.Atoi(m[1])
			if f := statusFailure(n); f != nil {
				f.detail = detail
				return f
			}
		}
		return &failure{code: CodeNetwork, detail: detail}
	case "3": // resource not found
		return &failure{code: CodeHTTPStatus, params: map[string]any{"status": 404}, detail: detail}
	case "2", "5", "6", "19", "29": // timeout, too slow, network, name resolution, server overloaded
		return &failure{code: CodeNetwork, detail: detail}
	case "9": // not enough disk space (the queue fills in the free space)
		return &failure{code: CodeNoSpace, detail: detail}
	case "13", "14", "15", "16", "17", "18":
		return &failure{code: CodeWriteFailed, detail: detail}
	}
	return &failure{code: CodeEngineFailed, detail: detail}
}
