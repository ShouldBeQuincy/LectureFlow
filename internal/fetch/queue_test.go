// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// The queue implements its part of the bridge protocol with the frozen
// signatures.
var _ interface {
	Check(context.Context, *bridge.CheckRequest) (*bridge.CheckReply, error)
	Enqueue(context.Context, *bridge.EnqueueRequest) (*bridge.EnqueueReply, error)
	Refresh(context.Context, *bridge.RefreshRequest) (*bridge.RefreshReply, error)
	Subscribe(context.Context) <-chan bridge.Status
} = (*Queue)(nil)

func bridgeCode(err error) string {
	var be *bridge.Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func codedOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

func TestFileDownloadToLecture(t *testing.T) {
	e := newQEnv(t, nil)
	audio := pattern(7, 300_000)
	e.media.put("/files/a.m4a", audio)
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateDone)
	if info.LectureID == "" || info.Error != nil || len(info.AllowedActions) != 1 || info.AllowedActions[0] != ActionRemove {
		t.Fatalf("done %+v", info)
	}
	if info.RecordedAt != "2026-10-21T09:35" || info.Title != "Lecture 3: Graphs / Trees" || info.BytesDone != int64(len(audio)) {
		t.Fatalf("info %+v", info)
	}
	l := e.lecture(info.LectureID)
	if !bytes.Equal(readAll(t, l.MediaPath), audio) {
		t.Fatal("lecture media differs from the download")
	}
	imp := e.importsSnapshot()
	if len(imp) != 1 || imp[0].Filename != "Lecture 3 Graphs Trees.m4a" || imp[0].Title != "Lecture 3: Graphs / Trees" ||
		imp[0].RecordedAt == nil || *imp[0].RecordedAt != "2026-10-21T09:35" || imp[0].CourseID != e.course {
		t.Fatalf("import %+v", imp)
	}
	// the request carried the cookie of its domain only, the page and the
	// signed URL; aria2c got them as headers of addUri
	reqs := e.media.requests("/files/a.m4a")
	if len(reqs) == 0 || reqs[0].Cookie != testCookie || reqs[0].Referer != testRefOrigin || !strings.Contains(reqs[0].Query, testSig) {
		t.Fatalf("media requests %+v", reqs)
	}
	adds, args, conf, _, _ := e.aria.snapshot()
	if len(adds) != 1 || adds[0].Split != "4" || adds[0].MaxConn != "4" ||
		!slices.Contains(adds[0].Headers, "Cookie: sid="+testCookie) || !slices.Contains(adds[0].Headers, "Referer: "+testRefOrigin) {
		t.Fatalf("addUri %+v", adds)
	}
	for _, h := range adds[0].Headers {
		if strings.Contains(h, "elsewhere") {
			t.Fatalf("a cookie of another domain was sent: %v", adds[0].Headers)
		}
	}
	for _, want := range []string{"--enable-rpc=true", "--rpc-listen-all=false", "--continue=true", "--auto-file-renaming=false",
		"--file-allocation=none", "--check-certificate=true", "--console-log-level=warn", "--summary-interval=0"} {
		if !slices.Contains(args, want) {
			t.Errorf("aria2c args lack %s: %v", want, args)
		}
	}
	if argValue(args, "stop-with-process") != itoa(int64(os.Getpid())) {
		t.Errorf("--stop-with-process: %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "rpc-secret") || strings.Contains(a, testCookie) || strings.Contains(a, "log=") || strings.Contains(a, "save-session") {
			t.Errorf("aria2c argument %q", a)
		}
	}
	if !strings.HasPrefix(conf, "rpc-secret=") || strings.Count(conf, "\n") != 1 {
		t.Errorf("conf file %q", conf)
	}
	if _, err := os.Stat(argValue(args, "conf-path")); !os.IsNotExist(err) {
		t.Errorf("the conf file is still there: %v", err)
	}
	// the task directory is gone, the row has no credential
	e.waitFor("task directory removed", func() bool {
		_, err := os.Stat(e.q.taskDir(id))
		return os.IsNotExist(err)
	})
	rec := e.row(id)
	// ruling 3: the host and the hash of the path, never the path; the
	// page's host only (security review L3; page_path: db TestMigration013PagePath)
	if rec.PageHost != "lms.example.edu" || strings.Contains(rec.TracksJSON, "Signature") ||
		strings.Contains(rec.TracksJSON, "/files/a.m4a") || strings.Contains(rec.TracksJSON, `"path"`) ||
		!strings.Contains(rec.TracksJSON, `"path_hash":"`+pathHash("/files/a.m4a")+`"`) ||
		!strings.Contains(rec.TracksJSON, `"host":"`+strings.TrimPrefix(e.media.srv.URL, "http://")+`"`) ||
		rec.LectureID == nil || rec.State != "done" {
		t.Fatalf("row %+v", rec)
	}
	if e.changedCount(id) == 0 {
		t.Fatal("no change notice")
	}
}

func (e *qenv) changedCount(id int64) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.changed[id]
}

// Panopto on CloudFront (M0): no cookie at all, the Referer only.
func TestNoCookies(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.mu.Lock()
	e.media.cookie = ""
	e.media.mu.Unlock()
	e.media.put("/files/v.mp4", pattern(1, 50_000))
	req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"))
	req.Cookies = []bridge.Cookie{}
	id := e.enqueue(req)
	e.wait(id, bridge.StateDone)
	adds, _, _, _, _ := e.aria.snapshot()
	if len(adds) != 1 || len(adds[0].Headers) != 1 || adds[0].Headers[0] != "Referer: "+testRefOrigin {
		t.Fatalf("headers %+v", adds)
	}
}

func TestAudioVideoMerge(t *testing.T) {
	e := newQEnv(t, nil)
	video, audio := pattern(1, 120_000), pattern(2, 40_000)
	e.media.put("/files/v.mp4", video)
	e.media.put("/files/a.m4a", audio)
	req := e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	req.MaxConnections = 2
	id := e.enqueue(req)
	info := e.wait(id, bridge.StateDone)
	l := e.lecture(info.LectureID)
	if filepath.Ext(l.MediaPath) != ".mp4" || !bytes.Equal(readAll(t, l.MediaPath), append(append([]byte{}, video...), audio...)) {
		t.Fatalf("merged media %s", l.MediaPath)
	}
	calls := e.ffmpegCalls()
	if len(calls) != 1 {
		t.Fatalf("ffmpeg calls %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	for _, want := range []string{"-map 0:v:0? -map 1:a:0", "-c copy", "-shortest", "-f mp4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mux lacks %q: %s", want, joined)
		}
	}
	if strings.Index(joined, "t0.mp4") > strings.Index(joined, "t1.m4a") {
		t.Errorf("video must be the first input: %s", joined)
	}
	adds, _, _, _, _ := e.aria.snapshot()
	for _, a := range adds {
		if a.Split != "2" {
			t.Errorf("the profile's maxConnections 2 is not applied: %+v", a)
		}
	}
}

func hlsSegments(e *qenv, dir string, n int, key bool) [][]byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	if key {
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/" + dir + "/key.bin?kt=" + testSig + "\",IV=0x0000000000000000000000000000000A\n")
		e.media.put("/"+dir+"/key.bin", []byte("0123456789abcdef"))
	}
	var segs [][]byte
	for i := 1; i <= n; i++ {
		seg := pattern(byte(i), 10_000+i)
		segs = append(segs, seg)
		name := "seg" + itoa(int64(i)) + ".ts"
		e.media.put("/"+dir+"/"+name, seg)
		b.WriteString("#EXTINF:6.0,\n" + name + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	e.media.put("/"+dir+"/index.m3u8", []byte(b.String()))
	return segs
}

func TestHLSSegmentsWithKey(t *testing.T) {
	e := newQEnv(t, nil)
	segs := hlsSegments(e, "v720", 7, true)
	hlsSegments(e, "v360", 2, false)
	e.media.put("/hls/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n/v360/index.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2400000,RESOLUTION=1280x720\n/v720/index.m3u8\n"))
	req := e.request("s1", e.hlsTrack(bridge.KindVideo, "/hls/master.m3u8"))
	req.MaxConnections = 3
	id := e.enqueue(req)
	info := e.wait(id, bridge.StateDone)
	var want []byte
	for _, s := range segs {
		want = append(want, s...)
	}
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, want) {
		t.Fatalf("joined media: %d bytes, want %d", len(got), len(want))
	}
	adds, _, _, maxActive, _ := e.aria.snapshot()
	if len(adds) != 8 { // 7 segments + the key
		t.Fatalf("%d downloads: %+v", len(adds), adds)
	}
	if maxActive > 3 {
		t.Fatalf("%d parts at once, the limit is 3", maxActive)
	}
	keyFetched := false
	for _, a := range adds {
		if a.Split != "1" || !slices.Contains(a.Headers, "Cookie: sid="+testCookie) {
			t.Errorf("part %+v", a)
		}
		keyFetched = keyFetched || strings.Contains(a.URI, "/v720/key.bin")
	}
	if !keyFetched {
		t.Fatal("the AES-128 key was not downloaded")
	}
	calls := e.ffmpegCalls()
	if len(calls) != 1 {
		t.Fatalf("ffmpeg %v", calls)
	}
	joined := strings.Join(calls[0], " ")
	for _, w := range []string{"-allowed_extensions ALL", "-allowed_segment_extensions ALL", "-extension_picky 0", "-protocol_whitelist file,crypto", "-c copy"} {
		if !strings.Contains(joined, w) {
			t.Errorf("join lacks %q: %s", w, joined)
		}
	}
	// the playlists were read with the session and the page too
	for _, p := range []string{"/hls/master.m3u8", "/v720/index.m3u8"} {
		r := e.media.requests(p)
		if len(r) != 1 || r[0].Cookie != testCookie || r[0].Referer != testRefOrigin {
			t.Fatalf("%s: %+v", p, r)
		}
	}
	if len(e.media.requests("/v360/index.m3u8")) != 0 {
		t.Fatal("the smaller variant was read")
	}
}

// An older ffmpeg without the 7.1 options: the join is repeated without them.
func TestHLSOldFFmpeg(t *testing.T) {
	e := newQEnv(t, nil)
	t.Setenv("LF_FAKE_FFMPEG", "oldopts")
	hlsSegments(e, "v1", 2, false)
	id := e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/v1/index.m3u8")))
	e.wait(id, bridge.StateDone)
	calls := e.ffmpegCalls()
	if len(calls) != 2 || strings.Contains(strings.Join(calls[1], " "), "extension_picky") {
		t.Fatalf("calls %v", calls)
	}
}

// The single-file path (M0 on Panopto): one download of the whole file,
// split, then a local remux.
func TestHLSSingleFile(t *testing.T) {
	e := newQEnv(t, nil)
	pl, end := panoptoMedia(30, 1000, false)
	file := pattern(9, int(end)+100)
	e.media.put("/sessions/x.hls/fragmented.mp4", file)
	e.media.put("/sessions/x.hls/index.m3u8", []byte(pl))
	e.media.put("/sessions/x.hls/master.m3u8", []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,CODECS=\"avc1.64001f,mp4a.40.2\"\nindex.m3u8\n"))
	id := e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/sessions/x.hls/master.m3u8")))
	info := e.wait(id, bridge.StateDone)
	if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Equal(got, file) {
		t.Fatalf("remuxed media %d bytes", len(got))
	}
	adds, _, _, _, _ := e.aria.snapshot()
	if len(adds) != 1 || !strings.HasSuffix(adds[0].URI, "/sessions/x.hls/fragmented.mp4?Policy="+testSig) || adds[0].Split != "4" {
		t.Fatalf("downloads %+v", adds)
	}
	calls := e.ffmpegCalls()
	if len(calls) != 1 || strings.Contains(strings.Join(calls[0], " "), "allowed_extensions") || !strings.Contains(strings.Join(calls[0], " "), "src.mp4") {
		t.Fatalf("remux %v", calls)
	}
	if info.BytesDone != int64(len(file)) {
		t.Fatalf("bytes %d", info.BytesDone)
	}
}

// A file shorter than its playlist says is refused.
func TestHLSSingleFileShort(t *testing.T) {
	e := newQEnv(t, nil)
	pl, end := panoptoMedia(5, 1000, true)
	e.media.put("/p/fragmented.mp4", pattern(9, int(end)-1))
	e.media.put("/p/index.m3u8", []byte(pl))
	id := e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/p/index.m3u8")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodePlaylistInvalid {
		t.Fatalf("%+v", info)
	}
}

// A video variant whose audio is a separate rendition: with no audio track
// in the task the rendition is fetched and muxed in.
func TestHLSSeparateAudioRendition(t *testing.T) {
	e := newQEnv(t, nil)
	hlsSegments(e, "v", 2, false)
	hlsSegments(e, "a", 2, false)
	e.media.put("/m/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"/a/index.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.4d401e\",AUDIO=\"g\"\n/v/index.m3u8\n"))
	id := e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/m/master.m3u8")))
	e.wait(id, bridge.StateDone)
	calls := e.ffmpegCalls()
	if len(calls) != 1 || strings.Count(strings.Join(calls[0], " "), "-i ") != 2 || !strings.Contains(strings.Join(calls[0], " "), "-map 1:a:0") {
		t.Fatalf("join %v", calls)
	}
	// with an audio track of its own the rendition is not fetched
	e2 := newQEnv(t, nil)
	hlsSegments(e2, "v", 2, false)
	hlsSegments(e2, "a", 2, false)
	e2.media.put("/m/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"g\",NAME=\"en\",DEFAULT=YES,URI=\"/a/index.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.4d401e\",AUDIO=\"g\"\n/v/index.m3u8\n"))
	e2.media.put("/files/a.m4a", pattern(3, 1000))
	id = e2.enqueue(e2.request("s1", e2.hlsTrack(bridge.KindVideo, "/m/master.m3u8"), e2.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e2.wait(id, bridge.StateDone)
	if n := len(e2.media.requests("/a/index.m3u8")); n != 0 {
		t.Fatalf("the rendition was read %d times", n)
	}
}

func TestEnqueueValidation(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	a := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
	v := e.fileTrack(bridge.KindVideo, "/files/v.mp4")
	rtmp := v
	rtmp.Format = "rtmp"
	badSelector := v
	badSelector.Format, badSelector.Selector = bridge.FormatDASH, "video:-:0\nX: 1"
	badURL := a
	badURL.URL = "file:///etc/passwd"
	unknown := v
	unknown.Kind = bridge.KindUnknown
	cases := []struct {
		name  string
		mod   func(r *bridge.EnqueueRequest)
		code  string
		field string
	}{
		{"no tracks", func(r *bridge.EnqueueRequest) { r.Tracks = nil }, bridge.CodeNoTracks, ""},
		{"two audio", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{a, a} }, bridge.CodeTooManyTracks, ""},
		{"three videos", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{v, unknown, v} }, bridge.CodeTooManyTracks, ""},
		{"two audio with two videos", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{a, v, v, a} }, bridge.CodeTooManyTracks, ""},
		{"unknown format", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{rtmp} }, bridge.CodeBadRequest, "tracks.format"},
		{"bad selector", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{badSelector} }, bridge.CodeBadRequest, "tracks.selector"},
		{"bad url", func(r *bridge.EnqueueRequest) { r.Tracks = []bridge.Track{badURL} }, bridge.CodeBadRequest, "tracks.url"},
		{"no session", func(r *bridge.EnqueueRequest) { r.SessionID = "" }, bridge.CodeBadRequest, "sessionId"},
		{"no platform", func(r *bridge.EnqueueRequest) { r.Platform = "" }, bridge.CodeBadRequest, "platform"},
		{"course gone", func(r *bridge.EnqueueRequest) { r.CourseID = "01NOSUCHCOURSE" }, bridge.CodeCourseGone, ""},
		{"header injection", func(r *bridge.EnqueueRequest) {
			r.Cookies = []bridge.Cookie{{Name: "sid", Value: "x\r\nX-Evil: 1", Domain: "127.0.0.1"}}
		}, bridge.CodeBadRequest, "cookies"},
		{"bad referer", func(r *bridge.EnqueueRequest) { r.Referer = "javascript:alert(1)" }, bridge.CodeBadRequest, "referer"},
		{"negative connections", func(r *bridge.EnqueueRequest) { r.MaxConnections = -1 }, bridge.CodeBadRequest, "maxConnections"},
	}
	for _, c := range cases {
		r := e.request("s-"+c.name, a)
		c.mod(r)
		_, err := e.q.Enqueue(ctx, r)
		var be *bridge.Error
		if !errors.As(err, &be) || be.Code != c.code || (c.field != "" && be.Params["field"] != c.field) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// audio + unknown is one audio and one other track: accepted
	e.media.put("/files/a.m4a", pattern(1, 100))
	e.media.put("/files/v.mp4", pattern(2, 100))
	id := e.enqueue(e.request("ok", a, unknown))
	e.wait(id, bridge.StateDone)
	rows, _ := e.opts.Store.ListDownloadTasks(ctx)
	if len(rows) != 1 {
		t.Fatalf("refused requests left rows: %d", len(rows))
	}
}

func TestDedupAndReuse(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 20_000))
	e.media.hold("/files/a.m4a")
	req := e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	id := e.enqueue(req)
	e.wait(id, bridge.StateDownloading)
	_, err := e.q.Enqueue(ctx, req)
	var be *bridge.Error
	if !errors.As(err, &be) || be.Code != bridge.CodeDuplicate || be.Params["state"] != bridge.StateDownloading || be.Params["taskId"] != id {
		t.Fatalf("duplicate while downloading: %v", err)
	}
	chk, err := e.q.Check(ctx, &bridge.CheckRequest{Envelope: bridge.Envelope{Type: bridge.TypeCheck, ID: "c"}, Platform: "panopto", SessionIDs: []string{"s1", "s2", "s1"}})
	if err != nil || !chk.OK || chk.ID != "c" || len(chk.Items) != 1 || chk.Items[0].State != bridge.StateDownloading || chk.Items[0].TaskID != id || chk.Items[0].CourseID != e.course {
		t.Fatalf("check %+v %v", chk, err)
	}
	e.media.release()
	info := e.wait(id, bridge.StateDone)
	_, err = e.q.Enqueue(ctx, req)
	if !errors.As(err, &be) || be.Code != bridge.CodeDuplicate || be.Params["state"] != bridge.StateImported || be.Params["lectureId"] != info.LectureID {
		t.Fatalf("duplicate after done: %v", err)
	}
	chk, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s1"}})
	if len(chk.Items) != 1 || chk.Items[0].State != bridge.StateImported || chk.Items[0].LectureID != info.LectureID {
		t.Fatalf("check after done %+v", chk.Items)
	}
	// another platform's session of the same id is another lecture
	chk, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: bridge.GenericProfile, SessionIDs: []string{"s1"}})
	if len(chk.Items) != 0 {
		t.Fatalf("generic %+v", chk.Items)
	}
	// the lecture deleted: new again, the same row is reused
	if err := e.st.DeleteLecture(ctx, info.LectureID); err != nil {
		t.Fatal(err)
	}
	chk, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s1"}})
	if len(chk.Items) != 0 {
		t.Fatalf("check after the lecture was deleted %+v", chk.Items)
	}
	other := e.mkCourse("CSC2001")
	req2 := e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	req2.CourseID = other
	if id2 := e.enqueue(req2); id2 != id {
		t.Fatalf("reused row %d, want %d", id2, id)
	}
	info = e.wait(id, bridge.StateDone)
	if info.CourseID != other || e.lecture(info.LectureID).CourseID != other {
		t.Fatalf("again %+v", info)
	}
	// failed: sent again → reused
	e.media.setStatus("/files/b.m4a", 500)
	idb := e.enqueue(e.request("s2", e.fileTrack(bridge.KindAudio, "/files/b.m4a")))
	e.wait(idb, bridge.StateFailed)
	chk, _ = e.q.Check(ctx, &bridge.CheckRequest{Platform: "panopto", SessionIDs: []string{"s2"}})
	if len(chk.Items) != 1 || chk.Items[0].State != bridge.StateFailed {
		t.Fatalf("check failed %+v", chk.Items)
	}
	e.media.setStatus("/files/b.m4a", 0)
	e.media.put("/files/b.m4a", pattern(3, 100))
	if again := e.enqueue(e.request("s2", e.fileTrack(bridge.KindAudio, "/files/b.m4a"))); again != idb {
		t.Fatalf("failed task not reused: %d vs %d", again, idb)
	}
	e.wait(idb, bridge.StateDone)
}

func TestConcurrencyAndConnections(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	for k, v := range map[string]string{KVConcurrency: "2", KVConnections: "3"} {
		if err := e.st.SetKV(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	e.q.SettingsChanged()
	var ids []int64
	for i := 1; i <= 4; i++ {
		p := "/files/f" + itoa(int64(i)) + ".mp4"
		e.media.put(p, pattern(byte(i), 40_000))
		e.media.hold(p)
		ids = append(ids, e.enqueue(e.request("s"+itoa(int64(i)), e.fileTrack(bridge.KindVideo, p))))
	}
	e.wait(ids[0], bridge.StateDownloading)
	e.wait(ids[1], bridge.StateDownloading)
	time.Sleep(50 * time.Millisecond)
	for k, want := range []int{0, 0, 1, 2} {
		info, _ := e.q.Get(ctx, ids[k])
		if want > 0 && (info.State != bridge.StateQueued || info.Position != want) {
			t.Fatalf("task %d: %+v, want queued at %d", k, info, want)
		}
	}
	if a := e.q.Activity(); a.Active != 2 || a.Queued != 2 {
		t.Fatalf("activity %+v", a)
	}
	adds, _, _, _, _ := e.aria.snapshot()
	if len(adds) != 2 || adds[0].Split != "3" {
		t.Fatalf("downloads %+v", adds)
	}
	// a third slot
	if err := e.st.SetKV(ctx, KVConcurrency, "3"); err != nil {
		t.Fatal(err)
	}
	e.q.SettingsChanged()
	e.wait(ids[2], bridge.StateDownloading)
	if info, _ := e.q.Get(ctx, ids[3]); info.Position != 1 {
		t.Fatalf("last task position %d", info.Position)
	}
	e.media.release()
	for _, id := range ids {
		e.wait(id, bridge.StateDone)
	}
	if a := e.q.Activity(); a.Active != 0 || a.Queued != 0 {
		t.Fatalf("activity after %+v", a)
	}
	// invalid stored values read as the defaults
	_ = e.st.SetKV(ctx, KVConcurrency, "99")
	_ = e.st.SetKV(ctx, KVConnections, "x")
	if c, n, err := ReadLimits(ctx, e.opts.Store); err != nil || c != DefaultConcurrency || n != DefaultConnections {
		t.Fatalf("limits %d %d %v", c, n, err)
	}
}

func TestRejectedNeedsRefresh(t *testing.T) {
	for _, status := range []int{401, 403} {
		e := newQEnv(t, nil)
		ctx := context.Background()
		e.media.put("/files/a.m4a", pattern(1, 5000))
		e.media.setStatus("/files/a.m4a", status)
		id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
		info := e.wait(id, bridge.StateNeedsRefresh)
		if info.Error == nil || info.Error.Code != CodeCredentialsRejected || info.Error.Params["status"] != float64(status) {
			t.Fatalf("%d: %+v", status, info.Error)
		}
		if strings.Contains(info.Error.Detail, testSig) {
			t.Fatalf("detail keeps the signature: %s", info.Error.Detail)
		}
		if !slices.Equal(info.AllowedActions, []string{ActionCancel}) {
			t.Fatalf("actions %v", info.AllowedActions)
		}
		e.q.mu.Lock()
		_, inMemory := e.q.tasks[id]
		e.q.mu.Unlock()
		if inMemory {
			t.Fatal("the credentials of a needs_refresh task are kept")
		}
		if _, err := e.q.Retry(ctx, id); codedOf(err) != CodeNeedsRefresh {
			t.Fatalf("retry of needs_refresh: %v", err)
		}
		// the extension sends new credentials: the download resumes
		e.media.setStatus("/files/a.m4a", 0)
		tr := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
		tr.Role = "primary-audio" // matched by kind
		rep, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Envelope: bridge.Envelope{Type: bridge.TypeRefresh, ID: "f"}, Platform: "panopto",
			SessionID: "s1", Tracks: []bridge.Track{tr}, Referer: testReferer, Cookies: e.cookies()})
		if err != nil || !rep.OK || rep.TaskID != id {
			t.Fatalf("refresh %+v %v", rep, err)
		}
		e.wait(id, bridge.StateDone)
		// refreshing a done task answers its state; an unknown session fails
		rep, err = e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1", Tracks: []bridge.Track{tr}})
		if err != nil || rep.State != bridge.StateDone {
			t.Fatalf("refresh done %+v %v", rep, err)
		}
		if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "nope", Tracks: []bridge.Track{tr}}); bridgeCode(err) != bridge.CodeTaskNotFound {
			t.Fatalf("refresh unknown %v", err)
		}
	}
}

// A restart in the middle of a download: the task needs a refresh, then
// resumes from the partial file with a Range request.
func TestRestartAndResume(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	video := pattern(4, 200_000)
	e.media.put("/files/v.mp4", video)
	e.media.hold("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.waitFor("half of the file", func() bool { return e.aria.completed("/files/v.mp4") >= int64(len(video)/4) })
	e.restart()
	info := e.wait(id, bridge.StateNeedsRefresh)
	if info.Error == nil || info.Error.Code != CodeRestarted {
		t.Fatalf("after restart %+v", info)
	}
	e.media.release()
	// the extension is opened again on that page
	_, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}, Referer: testReferer, Cookies: e.cookies()})
	if err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateDone)
	if !bytes.Equal(readAll(t, e.lecture(info.LectureID).MediaPath), video) {
		t.Fatal("resumed file differs")
	}
	reqs := e.media.requests("/files/v.mp4")
	last := reqs[len(reqs)-1]
	if last.Range == "" || last.Range == "bytes=0-" || last.Cookie != testCookie {
		t.Fatalf("resume request %+v", last)
	}
}

func TestStartMarksUnfinishedNeedsRefresh(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	ids := map[string]int64{}
	for _, s := range db.DownloadStates {
		id, err := e.opts.Store.InsertDownloadTask(ctx, db.DownloadTask{Platform: "panopto", SessionID: "s-" + s, ProfileID: "panopto",
			CourseID: e.course, Title: "x", TracksJSON: `[{"kind":"audio","format":"file"}]`, State: s, BytesTotal: -1})
		if err != nil {
			t.Fatal(err)
		}
		ids[s] = id
	}
	e.restart()
	for s, id := range ids {
		info, _ := e.q.Get(ctx, id)
		want := s
		if s == "queued" || s == "downloading" || s == "merging" || s == "importing" {
			want = bridge.StateNeedsRefresh
		}
		if info.State != want {
			t.Errorf("%s → %s", s, info.State)
		}
	}
	// failed after a restart: retry has no credentials and no files
	info, err := e.q.Retry(ctx, ids["failed"])
	if err != nil || info.State != bridge.StateNeedsRefresh || info.Error.Code != CodeRestarted {
		t.Fatalf("retry without credentials %+v %v", info, err)
	}
	// status pushes after hello: every task that has not ended
	sctx, cancel := context.WithCancel(ctx)
	ch := e.q.Subscribe(sctx)
	got := map[int64]string{}
	timeout := time.After(5 * time.Second)
	for len(got) < 6 { // the four reset, needs_refresh, and the failed one retried
		select {
		case st := <-ch:
			got[st.TaskID] = st.State
			if st.Type != bridge.TypeStatus || st.CourseID != e.course {
				t.Fatalf("status %+v", st)
			}
		case <-timeout:
			t.Fatalf("initial statuses %v", got)
		}
	}
	for s := range ids {
		_, seen := got[ids[s]]
		if seen != (s == "queued" || s == "downloading" || s == "merging" || s == "importing" || s == "needs_refresh" || s == "failed") {
			t.Errorf("%s: pushed %v", s, seen)
		}
	}
	cancel()
	for range ch {
	}
}

func TestCancel(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/v.mp4", pattern(1, 100_000))
	e.media.hold("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.waitFor("a partial file", func() bool {
		_, err := os.Stat(filepath.Join(e.q.taskDir(id), "t0.mp4.part"))
		return err == nil
	})
	info, err := e.q.Cancel(ctx, id)
	if err != nil || info.State != bridge.StateCancelled || !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
		t.Fatalf("cancel %+v %v", info, err)
	}
	if _, err := os.Stat(e.q.taskDir(id)); !os.IsNotExist(err) {
		t.Fatalf("task directory left: %v", err)
	}
	e.aria.mu.Lock()
	removed := len(e.aria.removed)
	e.aria.mu.Unlock()
	if removed == 0 {
		t.Fatal("aria2c's download was not removed")
	}
	if _, err := e.q.Cancel(ctx, id); codedOf(err) != CodeNotCancellable {
		t.Fatalf("cancel twice: %v", err)
	}
	if _, err := e.q.Retry(ctx, id); codedOf(err) != CodeNotRetryable {
		t.Fatalf("retry cancelled: %v", err)
	}
	if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: "s1",
		Tracks: []bridge.Track{e.fileTrack(bridge.KindVideo, "/files/v.mp4")}}); bridgeCode(err) != bridge.CodeTaskNotFound {
		t.Fatalf("refresh cancelled: %v", err)
	}
	// queued tasks can be cancelled too
	_ = e.st.SetKV(ctx, KVConcurrency, "1")
	e.q.SettingsChanged()
	e.media.put("/files/w.mp4", pattern(1, 100_000))
	e.media.hold("/files/w.mp4")
	first := e.enqueue(e.request("s2", e.fileTrack(bridge.KindVideo, "/files/w.mp4")))
	second := e.enqueue(e.request("s3", e.fileTrack(bridge.KindVideo, "/files/w.mp4")))
	e.wait(first, bridge.StateDownloading)
	if info, err := e.q.Cancel(ctx, second); err != nil || info.State != bridge.StateCancelled {
		t.Fatalf("cancel queued %+v %v", info, err)
	}
	// cancelled → removable, then gone
	if err := e.q.Remove(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.Get(ctx, second); codedOf(err) != CodeNotFound {
		t.Fatalf("after remove: %v", err)
	}
	if err := e.q.Remove(ctx, first); codedOf(err) != CodeActive {
		t.Fatalf("remove active: %v", err)
	}
	if _, err := e.q.Cancel(ctx, 9999); codedOf(err) != CodeNotFound {
		t.Fatalf("cancel unknown: %v", err)
	}
}

func TestFailedRetryAndRemove(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 3000))
	e.media.setStatus("/files/a.m4a", 503)
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeHTTPStatus || info.Error.Params["status"] != float64(503) ||
		!slices.Equal(info.AllowedActions, []string{ActionRetry, ActionRemove}) {
		t.Fatalf("failed %+v", info)
	}
	e.media.setStatus("/files/a.m4a", 0)
	info, err := e.q.Retry(ctx, id)
	if err != nil || (info.State != bridge.StateQueued && info.State != bridge.StateDownloading) {
		t.Fatalf("retry %+v %v", info, err)
	}
	info = e.wait(id, bridge.StateDone)
	if _, err := e.q.Retry(ctx, id); codedOf(err) != CodeNotRetryable {
		t.Fatalf("retry done: %v", err)
	}
	if err := e.q.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	// the lecture stays
	e.lecture(info.LectureID)
	list, _ := e.q.List(ctx, "")
	if len(list) != 0 {
		t.Fatalf("list %+v", list)
	}
}

// Merging or importing failed: the retry needs no credentials (the files
// are there), even after a restart.
func TestImportFailureAndLaunchRefused(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/v.mp4", pattern(1, 3000))
	e.media.put("/files/a.m4a", pattern(2, 1000))
	t.Setenv("LF_FAKE_FFMPEG", "fail")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeMergeFailed || !strings.Contains(info.Error.Detail, "Conversion failed") {
		t.Fatalf("merge failure %+v", info.Error)
	}
	t.Setenv("LF_FAKE_FFMPEG", "ok")
	e.mu.Lock()
	e.importErr = errors.New("disk on fire")
	e.mu.Unlock()
	if _, err := e.q.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	info = e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeImportFailed || info.Error.Detail != "disk on fire" {
		t.Fatalf("import failure %+v", info.Error)
	}
	if _, err := os.Stat(filepath.Join(e.q.taskDir(id), "media.mp4")); err != nil {
		t.Fatalf("the merged file is gone: %v", err)
	}
	e.restart()
	e.mu.Lock()
	e.importErr = nil
	e.launchErr = &model.ErrorInfo{Code: "settings.role_not_configured"}
	e.mu.Unlock()
	adds, _, _, _, _ := e.aria.snapshot()
	before := len(adds)
	if info, err := e.q.Retry(ctx, id); err != nil || info.State == bridge.StateNeedsRefresh {
		t.Fatalf("retry after restart with the files there: %+v %v", info, err)
	}
	info = e.wait(id, bridge.StateDone)
	if adds, _, _, _, _ = e.aria.snapshot(); info.LectureID == "" || len(adds) != before {
		t.Fatalf("done %+v, downloads %d → %d", info, before, len(adds))
	}
	if !strings.Contains(e.logs.String(), "stays a draft") {
		t.Fatal("the refused launch is not logged")
	}
}

// review R10 M1: the import refusing the downloaded file (a sign-in page,
// an empty file) fails the task with that code and its params, not
// downloads.import_failed, in the list and in the extension's status. The
// file goes back to the task's directory, but sending the recording again
// downloads it anew: the same file would be refused again. Another import
// failure keeps the file for the next try.
func TestImportRefusedKeepsReason(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	ch := e.q.Subscribe(ctx)
	e.media.put("/files/a.m4a", pattern(1, 1000))
	track := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
	e.mu.Lock()
	e.importErr = fmt.Errorf("download import: %w", &ImportRefused{Code: "upload.media_html", Params: map[string]any{"name": "Lecture 3.m4a"}})
	e.mu.Unlock()
	id := e.enqueue(e.request("s1", track))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != "upload.media_html" || info.Error.Params["name"] != "Lecture 3.m4a" || len(info.Error.Params) != 1 || info.Error.Detail != "" {
		t.Fatalf("refused import %+v", info.Error)
	}
	// a retry would import the same file (T18 N2): remove only
	if !slices.Equal(info.AllowedActions, []string{ActionRemove}) {
		t.Fatalf("actions %v", info.AllowedActions)
	}
	rec, err := e.opts.Store.GetDownloadTask(ctx, id)
	if err != nil || rec.ErrorCode != "upload.media_html" || rec.ErrorParams != `{"name":"Lecture 3.m4a"}` {
		t.Fatalf("stored %q %q %v", rec.ErrorCode, rec.ErrorParams, err)
	}
	var st bridge.Status
	for st.State != bridge.StateFailed {
		select {
		case st = <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("no failed status")
		}
	}
	if st.Error == nil || st.Error.Code != "upload.media_html" || st.Error.Params["name"] != "Lecture 3.m4a" {
		t.Fatalf("status %+v", st.Error)
	}
	if _, err := os.Stat(filepath.Join(e.q.taskDir(id), "media.m4a")); err != nil {
		t.Fatalf("the file was not given back: %v", err)
	}

	// a retry would import the same file and be refused again: it is
	// refused with the task's reason (T18 N2), nothing runs
	adds, _, _, _, _ := e.aria.snapshot()
	before := len(adds)
	if _, err := e.q.Retry(ctx, id); codedOf(err) != "upload.media_html" || !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a refused file: %v", err)
	}
	if info, _ = e.q.Get(ctx, id); info.State != bridge.StateFailed || info.Error == nil || info.Error.Code != "upload.media_html" {
		t.Fatalf("after the refused retry %+v", info)
	}
	if adds, _, _, _, _ = e.aria.snapshot(); len(adds) != before {
		t.Fatalf("a retry downloaded again: %d → %d", before, len(adds))
	}

	// sent again after the refusal: downloaded anew; another import failure
	e.mu.Lock()
	e.importErr = errors.New("disk on fire")
	e.mu.Unlock()
	if again := e.enqueue(e.request("s1", track)); again != id {
		t.Fatalf("a new task %d", again)
	}
	info = e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeImportFailed || info.Error.Detail != "disk on fire" {
		t.Fatalf("import failure %+v", info.Error)
	}
	if adds, _, _, _, _ = e.aria.snapshot(); len(adds) != before+1 {
		t.Fatalf("not downloaded anew after a refusal: %d → %d", before, len(adds))
	}

	// sent again after that failure: the file there is imported
	e.mu.Lock()
	e.importErr = nil
	e.mu.Unlock()
	if again := e.enqueue(e.request("s1", track)); again != id {
		t.Fatalf("a new task %d", again)
	}
	info = e.wait(id, bridge.StateDone)
	if adds, _, _, _, _ = e.aria.snapshot(); info.LectureID == "" || len(adds) != before+1 {
		t.Fatalf("done %+v, downloads %d → %d", info, before, len(adds))
	}
}

// R12 D-3 (P6 D4): the extension's refresh of a task whose file the
// import refused downloads it anew, as sending it again does, instead of
// importing the same file to be refused again. Only that task's directory
// goes: another failed task's file stays, and a refresh of a task that
// failed otherwise still resumes from its file.
func TestRefreshAfterRefusedImportDownloadsAnew(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 1000))
	e.media.put("/files/b.m4a", pattern(2, 1000))
	trackA := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
	trackB := e.fileTrack(bridge.KindAudio, "/files/b.m4a")
	setImportErr := func(err error) {
		e.mu.Lock()
		e.importErr = err
		e.mu.Unlock()
	}
	refresh := func(session string, tr bridge.Track) {
		t.Helper()
		if _, err := e.q.Refresh(ctx, &bridge.RefreshRequest{Platform: "panopto", SessionID: session,
			Tracks: []bridge.Track{tr}, Referer: testReferer, Cookies: e.cookies()}); err != nil {
			t.Fatal(err)
		}
	}
	// another task: its import failed otherwise, its file is kept
	setImportErr(errors.New("disk on fire"))
	other := e.enqueue(e.request("other", trackB))
	if info := e.wait(other, bridge.StateFailed); info.Error == nil || info.Error.Code != CodeImportFailed {
		t.Fatalf("other %+v", info.Error)
	}
	otherFile := filepath.Join(e.q.taskDir(other), "media.m4a")
	if _, err := os.Stat(otherFile); err != nil {
		t.Fatal(err)
	}
	// this task: the file refused
	setImportErr(&ImportRefused{Code: "upload.media_html", Params: map[string]any{"name": "Lecture 3.m4a"}})
	id := e.enqueue(e.request("s1", trackA))
	if info := e.wait(id, bridge.StateFailed); info.Error == nil || info.Error.Code != "upload.media_html" {
		t.Fatalf("refused %+v", info.Error)
	}
	if _, err := os.Stat(filepath.Join(e.q.taskDir(id), "media.m4a")); err != nil {
		t.Fatalf("the refused file was not given back: %v", err)
	}
	adds, _, _, _, _ := e.aria.snapshot()
	before := len(adds)
	imports := len(e.importsSnapshot())

	if rec, err := e.opts.Store.GetDownloadTask(ctx, id); err != nil || rec.BytesDone != 1000 {
		t.Fatalf("stored bytes before the refresh %d/%d %v", rec.BytesDone, rec.BytesTotal, err)
	}

	// refreshed: the progress starts over (the download held at half, so
	// nothing has reported yet), then downloaded anew (one more download),
	// imported once, done
	setImportErr(nil)
	e.media.hold("/files/a.m4a")
	refresh("s1", trackA)
	if rec, err := e.opts.Store.GetDownloadTask(ctx, id); err != nil || rec.BytesDone != 0 || rec.BytesTotal != -1 {
		t.Fatalf("stored bytes right after the refresh %d/%d %v, want 0/-1", rec.BytesDone, rec.BytesTotal, err)
	}
	e.media.release()
	info := e.wait(id, bridge.StateDone)
	if adds, _, _, _, _ = e.aria.snapshot(); len(adds) != before+1 {
		t.Fatalf("not downloaded anew on refresh: %d → %d", before, len(adds))
	}
	if got := len(e.importsSnapshot()); got != imports+1 || info.LectureID == "" {
		t.Fatalf("imports %d → %d, %+v", imports, got, info)
	}
	if rec, err := e.opts.Store.GetDownloadTask(ctx, id); err != nil || rec.BytesDone != 1000 || rec.BytesTotal != 1000 {
		t.Fatalf("stored bytes %d/%d %v", rec.BytesDone, rec.BytesTotal, err)
	}
	// the other task's file is untouched; its refresh imports that file
	// without downloading it again
	if _, err := os.Stat(otherFile); err != nil {
		t.Fatalf("another task's file went: %v", err)
	}
	refresh("other", trackB)
	e.wait(other, bridge.StateDone)
	if adds, _, _, _, _ = e.aria.snapshot(); len(adds) != before+1 {
		t.Fatalf("a refresh after another import failure downloaded again: %d → %d", before, len(adds))
	}
}

// An import error that is not a refusal of the file keeps
// downloads.import_failed, also a coded one of another kind.
func TestRefusedFileCodes(t *testing.T) {
	for code, want := range map[string]bool{
		"upload.media_empty": true, "upload.media_html": true, "upload.media_playlist": true, "upload.media_format": true,
		CodeImportFailed: false, "upload.not_media": false, "server.internal": false, "": false,
	} {
		if RefusedFile(code) != want {
			t.Errorf("RefusedFile(%q) = %v", code, !want)
		}
	}
}

func TestStatusPushes(t *testing.T) {
	e := newQEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := e.q.Subscribe(ctx)
	e.media.put("/files/a.m4a", pattern(1, 50_000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	var states []string
	var last bridge.Status
	timeout := time.After(10 * time.Second)
	for last.State != bridge.StateDone {
		select {
		case st := <-ch:
			if st.TaskID != id || st.SessionID != "s1" || st.Platform != "panopto" {
				t.Fatalf("status %+v", st)
			}
			if len(states) == 0 || states[len(states)-1] != st.State {
				states = append(states, st.State)
			}
			last = st
		case <-timeout:
			t.Fatalf("states so far %v", states)
		}
	}
	if last.LectureID == "" || last.BytesDone != 50_000 || states[len(states)-1] != bridge.StateDone {
		t.Fatalf("last %+v, states %v", last, states)
	}
	cancel()
	for range ch {
	}
}

// The course is deleted while its download waits: the row goes with it and
// the task is dropped quietly.
func TestCourseDeletedWhileDownloading(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/v.mp4", pattern(1, 100_000))
	e.media.hold("/files/v.mp4")
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4")))
	e.wait(id, bridge.StateDownloading)
	raw, err := db.OpenDB(filepath.Join(e.dataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM course WHERE id = ?`, e.course); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	e.media.release()
	e.waitFor("the task dropped", func() bool {
		_, err := e.q.Get(context.Background(), id)
		e.q.mu.Lock()
		_, live := e.q.tasks[id]
		e.q.mu.Unlock()
		return codedOf(err) == CodeNotFound && !live
	})
	e.waitFor("the directory removed", func() bool {
		_, err := os.Stat(e.q.taskDir(id))
		return os.IsNotExist(err)
	})
}

func TestLocateTask(t *testing.T) {
	e := newQEnv(t, nil)
	ctx := context.Background()
	e.media.put("/files/a.m4a", pattern(1, 100))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateDone)
	c, l, err := e.q.LocateTask(ctx, id)
	if err != nil || c != e.course || l != info.LectureID {
		t.Fatalf("%q %q %v", c, l, err)
	}
	if _, _, err := e.q.LocateTask(ctx, 999); bridgeCode(err) != bridge.CodeNotFound {
		t.Fatalf("unknown: %v", err)
	}
}
