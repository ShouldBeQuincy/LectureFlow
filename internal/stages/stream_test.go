// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// ---------- fake media tools (SPEC-v2 P8.1, appendix D6) ----------

// fakeMedia is a mediaTools that records ffmpeg calls and writes their
// output file, without running any program.
type fakeMedia struct {
	mu          sync.Mutex
	probe       string // ffprobe JSON
	probeStderr string
	probeErr    error
	free        int64 // bytes free (0 = plenty)
	freeErr     error
	calls       [][]string
	// fail decides the result of an ffmpeg call: a non-nil error fails it
	// with the returned stderr.
	fail func(args []string) (string, error)
	// block, when set, holds stream.mp4 calls until it is closed or ctx ends.
	block chan struct{}
	// encoders is the result of H264Encoders (nil = NVENC usable, the rest
	// not listed); encErr fails it.
	encoders []encoderCheck
	encErr   error
	encCalls int
}

func (f *fakeMedia) H264Encoders(ctx context.Context) ([]encoderCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.encCalls++
	if f.encErr != nil {
		return nil, f.encErr
	}
	if f.encoders == nil {
		return []encoderCheck{{Name: encNVENC, Listed: true}, {Name: encQSV}, {Name: encVideoToolbox}, {Name: encOpenH264}}, nil
	}
	return f.encoders, nil
}

func (f *fakeMedia) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	fail, block := f.fail, f.block
	f.mu.Unlock()
	if !filepath.IsAbs(dir) || !filepath.IsAbs(args[len(args)-1]) {
		return "relative path", errors.New("exit status 1")
	}
	if fail != nil {
		if stderr, err := fail(args); err != nil {
			return stderr, err
		}
	}
	progress(0.5)
	if block != nil && isStreamCall(args) {
		select {
		case <-block:
		case <-ctx.Done():
			return "killed", ctx.Err()
		}
	}
	if err := os.WriteFile(args[len(args)-1], make([]byte, 1000), 0o644); err != nil {
		return err.Error(), err
	}
	progress(1)
	return "", nil
}

func (f *fakeMedia) Probe(ctx context.Context, media string) ([]byte, string, error) {
	if f.probeErr != nil {
		return nil, f.probeStderr, f.probeErr
	}
	return []byte(f.probe), "", nil
}

func (f *fakeMedia) FreeSpace(string) (int64, error) {
	if f.freeErr != nil {
		return 0, f.freeErr
	}
	if f.free == 0 {
		return 500 << 30, nil
	}
	return f.free, nil
}

func (f *fakeMedia) ffmpegCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// streamCalls are the recorded calls that write stream.mp4.
func (f *fakeMedia) streamCalls() [][]string {
	var out [][]string
	for _, c := range f.ffmpegCalls() {
		if isStreamCall(c) {
			out = append(out, c)
		}
	}
	return out
}

func isStreamCall(args []string) bool {
	return strings.HasSuffix(args[len(args)-1], model.StreamFile+".tmp")
}

// ffprobe JSON of typical inputs.
const (
	probeMP4 = `{"streams": [
	 {"index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv420p", "disposition": {"default": 1, "attached_pic": 0}, "tags": {"creation_time": "2026-09-30T13:05:12.000000Z"}},
	 {"index": 1, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 1, "attached_pic": 0}},
	 {"index": 2, "codec_type": "data", "codec_name": "bin_data"}],
	 "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "3012.5", "tags": {"major_brand": "mp42", "creation_time": "2026-09-30T13:05:12.000000Z"}}}`
	probeHEVC = `{"streams": [
	 {"index": 0, "codec_type": "audio", "codec_name": "aac"},
	 {"index": 1, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p"}],
	 "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "tags": {"creation_time": "1904-01-01T00:00:00.000000Z"}}}`
	probeM4A = `{"streams": [
	 {"index": 0, "codec_type": "audio", "codec_name": "aac"},
	 {"index": 1, "codec_type": "video", "codec_name": "mjpeg", "pix_fmt": "yuvj420p", "disposition": {"attached_pic": 1}}],
	 "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}}`
)

// mediaLecture creates a launched lecture whose original upload m.mp4 is
// in its directory.
func (e *env) mediaLecture(title string) (model.Lecture, string) {
	e.t.Helper()
	l := e.lecture(title, "m.mp4")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	media := filepath.Join(dir, "m.mp4")
	e.must(os.WriteFile(media, []byte("original media"), 0o644))
	return l, dir
}

// streamState reads kv stream:{lecture}.
func (e *env) streamState(lectureID string) model.StreamState {
	e.t.Helper()
	st, ok, err := StreamStateOf(e.ctx, e.st, lectureID)
	if err != nil || !ok {
		e.t.Fatalf("stream state of %s: ok=%v err=%v", lectureID, ok, err)
	}
	return st
}

// streamError decodes StreamState.Error.
func streamError(t *testing.T, st model.StreamState) model.ErrorInfo {
	t.Helper()
	var info model.ErrorInfo
	if err := json.Unmarshal([]byte(st.Error), &info); err != nil {
		t.Fatalf("StreamState.Error %q is not an ErrorInfo: %v", st.Error, err)
	}
	return info
}

func monotone(vals []float64) bool {
	for i := 1; i < len(vals); i++ {
		if vals[i] < vals[i-1] {
			return false
		}
	}
	return true
}

func argsContain(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// ---------- pure parts ----------

func TestParseProbe(t *testing.T) {
	info, err := parseProbe([]byte(probeMP4))
	if err != nil {
		t.Fatal(err)
	}
	if info.Video == nil || info.Video.Index != 0 || info.Video.Codec != "h264" || info.Audio == nil || info.Audio.Index != 1 || info.Audio.Codec != "aac" {
		t.Errorf("streams %+v %+v", info.Video, info.Audio)
	}
	if info.CreationTime == nil || !info.CreationTime.Equal(time.Date(2026, 9, 30, 13, 5, 12, 0, time.UTC)) {
		t.Errorf("creation_time %v", info.CreationTime)
	}
	// the MP4 epoch default is no recording time; audio before video is fine
	info, _ = parseProbe([]byte(probeHEVC))
	if info.CreationTime != nil || info.Video == nil || info.Video.Index != 1 || info.Audio.Index != 0 {
		t.Errorf("hevc %+v", info)
	}
	// a cover picture is not a video: m4a with artwork is audio-only
	info, _ = parseProbe([]byte(probeM4A))
	if info.Video != nil || info.Audio == nil || planOf(info) != planNone {
		t.Errorf("m4a with cover art: video %+v", info.Video)
	}
	// creation_time on a stream only, other letter case, older ffprobe layout
	info, _ = parseProbe([]byte(`{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "mp3", "tags": {"CREATION_TIME": "2026-10-06 09:05:00"}}], "format": {"format_name": "mp3"}}`))
	if info.CreationTime == nil || info.CreationTime.Format(time.RFC3339) != "2026-10-06T09:05:00Z" {
		t.Errorf("stream creation_time %v", info.CreationTime)
	}
	if _, err := parseProbe([]byte("not json")); err == nil {
		t.Error("unparsable ffprobe output must be an error")
	}
}

func TestBrowserReady(t *testing.T) {
	v := func(codec, pix string) *mediaStream { return &mediaStream{Index: 0, Codec: codec, PixFmt: pix} }
	a := func(codec string) *mediaStream { return &mediaStream{Index: 1, Codec: codec} }
	mp4 := "mov,mp4,m4a,3gp,3g2,mj2"
	cases := []struct {
		info mediaInfo
		want streamPlan
	}{
		{mediaInfo{Format: mp4, Video: v("h264", "yuv420p"), Audio: a("aac")}, planRemux},
		{mediaInfo{Format: mp4, Video: v("h264", "yuvj420p"), Audio: a("aac")}, planRemux},
		{mediaInfo{Format: mp4, Video: v("h264", "yuv420p10le"), Audio: a("aac")}, planTranscode}, // High 10: browsers cannot decode
		{mediaInfo{Format: mp4, Video: v("hevc", "yuv420p"), Audio: a("aac")}, planTranscode},
		{mediaInfo{Format: mp4, Video: v("h264", "yuv420p"), Audio: a("mp3")}, planTranscode},
		{mediaInfo{Format: "matroska,webm", Video: v("h264", "yuv420p"), Audio: a("aac")}, planRemux}, // test-media I5: the container does not matter
		{mediaInfo{Format: "matroska,webm", Video: v("h264", "yuv420p"), Audio: a("opus")}, planTranscode},
		{mediaInfo{Format: "avi", Video: v("mpeg4", "yuv420p"), Audio: a("mp3")}, planTranscode},
		{mediaInfo{Format: "wav", Audio: a("pcm_s16le")}, planNone},
		{mediaInfo{Format: mp4, Video: v("h264", "yuv420p")}, planNone}, // no audio: not generated
	}
	for i, c := range cases {
		if got := planOf(c.info); got != c.want {
			t.Errorf("case %d (%s): plan %d, want %d", i, c.info.Format, got, c.want)
		}
	}
}

// TestStreamArgs: P8.1 remux (-c copy -movflags +faststart) and transcode
// (H.264 with the chosen encoder of SPEC-v3 V7 + AAC), mapping the probed
// streams; the input read with the whitelists of media.InputArgs (security
// review 2, M3) right before its -i.
func TestStreamArgs(t *testing.T) {
	info := mediaInfo{Video: &mediaStream{Index: 2, Codec: "h264"}, Audio: &mediaStream{Index: 0, Codec: "aac"}}
	wl := "-format_whitelist " + lfmedia.FormatWhitelist + " -protocol_whitelist file"
	remux := strings.Join(streamArgs(info, `C:\in.mp4`, `C:\out.tmp`, ""), " ")
	if remux != `-hide_banner -nostdin -nostats -progress pipe:1 -y -threads 2 `+wl+` -i C:\in.mp4 -map 0:2 -map 0:0 -c copy -movflags +faststart -f mp4 C:\out.tmp` {
		t.Errorf("remux args: %s", remux)
	}
	tr := strings.Join(streamArgs(info, `C:\in.mkv`, `C:\out.tmp`, encNVENC), " ")
	if tr != `-hide_banner -nostdin -nostats -progress pipe:1 -y -threads 2 `+wl+` -i C:\in.mkv -map 0:2 -map 0:0 -c:v h264_nvenc -preset p4 -tune hq -rc vbr -cq 23 -b:v 0 -profile:v high -pix_fmt yuv420p -vf scale=trunc(iw/2)*2:trunc(ih/2)*2 -c:a aac -b:a 128k -threads 2 -movflags +faststart -f mp4 C:\out.tmp` {
		t.Errorf("transcode args: %s", tr)
	}
}

// TestMediaErrorClasses (appendix D6): file problems get their own codes
// (permanent), everything else media.failed (retryable); the raw ffmpeg
// output is the detail.
func TestMediaErrorClasses(t *testing.T) {
	exit := errors.New("exit status 1")
	cases := []struct {
		stderr    string
		err       error
		code      string
		permanent bool
	}{
		{"[in#0 @ 0000] Error opening input: Invalid data found when processing input\nx.mp4: Invalid data found when processing input", exit, MediaCorrupt, true},
		{"[mov,mp4 @ 0000] moov atom not found\nx.mp4: Invalid data found when processing input", exit, MediaCorrupt, true},
		{"[out#0/mp4 @ 0000] Output file does not contain any stream", exit, MediaNoAudio, true},
		{"[vist#0:0] Decoder (codec av1) not found for input stream #0:0", exit, MediaUnsupported, true},
		{"[mp4 @ 0000] Could not find tag for codec pcm_alaw in stream #1, codec not currently supported in container", exit, MediaUnsupported, true},
		{"[aac @ 0000] Error while decoding stream #0:1: Invalid data found when processing input\nav_interleaved_write_frame(): No space left on device", exit, MediaFailed, false},
		{"[out#0/mp4 @ 0000] Error opening output stream.mp4.tmp: Permission denied", exit, MediaFailed, false},
		{"", &ffmpegStartError{exe: `C:\ffmpeg\ffprobe.exe`, err: errors.New("access denied")}, MediaFailed, false},
	}
	for i, c := range cases {
		err := mediaError(c.stderr, c.err)
		var ce *model.CodedError
		if !errors.As(err, &ce) || ce.Info.Code != c.code || sched.IsPermanent(err) != c.permanent {
			t.Errorf("case %d: %v (permanent=%v), want %s permanent=%v", i, err, sched.IsPermanent(err), c.code, c.permanent)
			continue
		}
		info := model.ErrorInfo{}
		if e := json.Unmarshal([]byte(errorInfoJSON(err)), &info); e != nil || info.Code != c.code {
			t.Errorf("case %d: errorInfoJSON %q", i, errorInfoJSON(err))
		}
		want := c.stderr
		if want == "" {
			want = "无法启动 ffprobe"
		}
		if !strings.Contains(info.Detail, want) {
			t.Errorf("case %d: detail %q lacks the raw output", i, info.Detail)
		}
	}
}

// TestFFprobeNextToFFmpeg: the ffprobe of the configured ffmpeg's directory
// is preferred over PATH.
func TestFFprobeNextToFFmpeg(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg.exe")
	fp := filepath.Join(dir, "ffprobe.exe")
	for _, p := range []string{ff, fp} {
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ffprobeFor(ff); err != nil || got != fp {
		t.Errorf("ffprobeFor = %q, %v; want %q", got, err, fp)
	}
}

// ---------- prep: the playback file ----------

// TestPrepRemux: an MP4 with H.264 + AAC is remuxed (P8.1), atomically,
// after audio.wav; the progress counts both steps; the state is recorded.
func TestPrepRemux(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("L")
	fm := &fakeMedia{probe: probeMP4}
	r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	calls := fm.ffmpegCalls()
	if len(calls) != 2 || isStreamCall(calls[0]) || !isStreamCall(calls[1]) {
		t.Fatalf("ffmpeg calls %v: audio.wav first, then stream.mp4", calls)
	}
	media, _ := filepath.Abs(filepath.Join(dir, "m.mp4"))
	if !argsContain(calls[1], "-i", media) || !argsContain(calls[1], "-c", "copy") || !argsContain(calls[1], "-movflags", "+faststart") ||
		!argsContain(calls[1], "-map", "0:0", "-map", "0:1") || slices.Contains(calls[1], "libx264") {
		t.Errorf("remux args %v", calls[1])
	}
	if fi, err := os.Stat(filepath.Join(dir, model.StreamFile)); err != nil || fi.Size() != 1000 {
		t.Errorf("stream.mp4: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile+".tmp")); !os.IsNotExist(err) {
		t.Error("stream.mp4.tmp left behind")
	}
	st := e.streamState(l.ID)
	if st.Status != StreamReady || st.Transcoded || st.SizeBytes != 1000 || st.Error != "" || st.UpdatedAt == "" {
		t.Errorf("stream state %+v", st)
	}
	// progress: audio.wav takes 80% before a remux, stream.mp4 the rest
	vals, a := r.progress.vals, audioShareRemux
	if !monotone(vals) || r.progress.last() != 1 || !slices.Contains(vals, a) || !slices.Contains(vals, 0.5*a) || !slices.Contains(vals, a+0.5*(1-a)) {
		t.Errorf("progress %v", vals)
	}
	if !e.ev.has("lecture", l.ID) {
		t.Error("no lecture event for the stream state")
	}
}

// TestPrepTranscode: incompatible codecs → H.264 + AAC, recorded as
// transcoded. stream-parallel: prep only writes audio.wav (all of its
// progress) and queues the stream task, which transcodes after prep.
func TestPrepTranscode(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	fm := &fakeMedia{probe: probeHEVC}
	p, r := e.prepThenStream(fm, l.ID)
	if vals := p.progress.vals; !monotone(vals) || !slices.Contains(vals, 0.5) || p.progress.last() != 1 || len(fm.ffmpegCalls()) != 2 {
		t.Errorf("prep progress %v (audio.wav is all of prep)", vals)
	}
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("stream task %v %s", r.err, r.job.Status)
	}
	sc := fm.streamCalls()
	if len(sc) != 1 || !argsContain(sc[0], "-c:v", encNVENC) || slices.Contains(sc[0], "libx264") || !argsContain(sc[0], "-c:a", "aac") ||
		!argsContain(sc[0], "-map", "0:1", "-map", "0:0") || !argsContain(sc[0], "-movflags", "+faststart") {
		t.Errorf("transcode args %v", sc)
	}
	if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
		t.Errorf("stream state %+v", st)
	}
	if vals := r.progress.vals; !monotone(vals) || !slices.Contains(vals, 0.5) || r.progress.last() != 1 {
		t.Errorf("stream task progress %v", vals)
	}
}

// TestPrepAudioOnly: no video (cover art does not count) → no stream.mp4,
// state audio_only; a stale stream.mp4 of an earlier run is removed.
func TestPrepAudioOnly(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("L")
	e.must(os.WriteFile(filepath.Join(dir, model.StreamFile), []byte("old"), 0o644))
	fm := &fakeMedia{probe: probeM4A}
	r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(fm.streamCalls()) != 0 || len(fm.ffmpegCalls()) != 1 {
		t.Errorf("ffmpeg calls %v", fm.ffmpegCalls())
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
		t.Error("stream.mp4 must not exist for an audio-only source")
	}
	if st := e.streamState(l.ID); st.Status != StreamAudioOnly {
		t.Errorf("stream state %+v", st)
	}
	if vals := r.progress.vals; !monotone(vals) || !slices.Contains(vals, 0.5) || r.progress.last() != 1 {
		t.Errorf("progress %v (audio.wav is all of prep)", vals)
	}
}

// TestPrepNoSpace (appendix D6): under 10 GB free → not generated, state
// no_space, prep done.
func TestPrepNoSpace(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("L")
	fm := &fakeMedia{probe: probeMP4, free: StreamMinFreeBytes - 1}
	if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("prep %v %s", r.err, r.job.Status)
	}
	if len(fm.streamCalls()) != 0 {
		t.Error("stream.mp4 generated with too little free space")
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
		t.Error("stream.mp4 written")
	}
	if st := e.streamState(l.ID); st.Status != StreamNoSpace {
		t.Errorf("stream state %+v", st)
	}
	// exactly 10 GB is enough; an unknown free space does not block
	for _, fm := range []*fakeMedia{{probe: probeMP4, free: StreamMinFreeBytes}, {probe: probeMP4, freeErr: errors.New("no volume")}} {
		l, _ := e.mediaLecture("L2")
		if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady {
			t.Errorf("free %d err %v: state %+v", fm.free, fm.freeErr, st)
		}
	}
}

// TestPrepStreamFailureKeepsPrepDone: the playback file is optional — a
// failed remux/transcode or probe is recorded with its D6 code and the raw
// output, and prep still succeeds (the transcript does not wait for it). A
// transcode fails in the stream task that prep queued (stream-parallel):
// the task fails, prep is done.
func TestPrepStreamFailureKeepsPrepDone(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		fm   *fakeMedia
		code string
	}{
		{"unsupported codec", &fakeMedia{probe: probeHEVC, fail: func(args []string) (string, error) {
			if isStreamCall(args) {
				return "[vist#0:1] Decoder (codec hevc) not found for input stream #0:1", errors.New("exit status 1")
			}
			return "", nil
		}}, MediaUnsupported},
		{"disk full", &fakeMedia{probe: probeMP4, fail: func(args []string) (string, error) {
			if isStreamCall(args) {
				return "av_interleaved_write_frame(): No space left on device", errors.New("exit status 1")
			}
			return "", nil
		}}, MediaFailed},
		{"probe: damaged file", &fakeMedia{probeErr: errors.New("exit status 1"),
			probeStderr: "[mov,mp4 @ 0000] moov atom not found\nm.mp4: Invalid data found when processing input"}, MediaCorrupt},
		{"no ffprobe", &fakeMedia{probeErr: errors.New("找不到 ffprobe")}, MediaFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, dir := e.mediaLecture(c.name)
			var r runResult
			if c.fm.probe == probeHEVC {
				var task runResult
				r, task = e.prepThenStream(c.fm, l.ID)
				var ce *model.CodedError
				if task.job.Status != model.StatusFailed || !errors.As(task.err, &ce) || ce.Info.Code != c.code {
					t.Errorf("stream task %v %s, want %s", task.err, task.job.Status, c.code)
				}
			} else {
				r = e.run(&prepStage{d: e.deps(nil), media: c.fm}, l.ID)
			}
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep %v %s", r.err, r.job.Status)
			}
			if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); err != nil {
				t.Error("audio.wav missing")
			}
			for _, n := range []string{model.StreamFile, model.StreamFile + ".tmp"} {
				if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
					t.Errorf("%s exists after a failure", n)
				}
			}
			st := e.streamState(l.ID)
			info := streamError(t, st)
			if st.Status != StreamFailed || info.Code != c.code || info.Detail == "" {
				t.Errorf("stream state %+v / %+v, want code %s", st, info, c.code)
			}
			if r.progress.last() != 1 {
				t.Errorf("progress %v", r.progress.vals)
			}
		})
	}
}

// TestPrepKeepOriginalFalseKeepsStream (P8.1 / §17.5, lead ruling): with
// keep_original_media = false the original upload is deleted after prep
// once stream.mp4 exists (stream.mp4 stays) or the source is audio-only;
// when the playback file failed or there was no space, the original is
// kept (job note) so the stream task can still make it — and that task
// deletes it after succeeding.
func TestPrepKeepOriginalFalseKeepsStream(t *testing.T) {
	e := newEnv(t)
	e.cfg.Storage.KeepOriginalMedia = boolp(false)
	failStream := func(args []string) (string, error) {
		if isStreamCall(args) {
			return "[vist#0:1] Decoder (codec hevc) not found for input stream #0:1", errors.New("exit status 1")
		}
		return "", nil
	}
	cases := []struct {
		name   string
		fm     *fakeMedia
		status string
		kept   bool
	}{
		{"ready", &fakeMedia{probe: probeMP4}, StreamReady, false},
		{"audio only", &fakeMedia{probe: probeM4A}, StreamAudioOnly, false},
		{"failed", &fakeMedia{probe: probeHEVC, fail: failStream}, StreamFailed, true},
		{"no space", &fakeMedia{probe: probeMP4, free: 1 << 30}, StreamNoSpace, true},
		{"probe failed", &fakeMedia{probeErr: errors.New("找不到 ffprobe")}, StreamFailed, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, dir := e.mediaLecture(c.name)
			r := e.run(&prepStage{d: e.deps(nil), media: c.fm}, l.ID)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep %v %s", r.err, r.job.Status)
			}
			if c.fm.probe == probeHEVC {
				// stream-parallel: a transcode is the stream task's; prep keeps
				// the original for it without a note, and the task's own
				// outcome decides (here: it fails, so the original stays and
				// the task's job carries the note)
				if _, err := os.Stat(filepath.Join(dir, "m.mp4")); err != nil || r.job.Stats.NoteInfo != nil {
					t.Fatalf("after prep: original %v, prep note %+v", err, r.job.Stats.NoteInfo)
				}
				if r = e.run(&streamStage{d: e.deps(nil), media: c.fm}, l.ID); r.job.Status != model.StatusFailed {
					t.Fatalf("stream task %v %s", r.err, r.job.Status)
				}
			}
			if st := e.streamState(l.ID); st.Status != c.status {
				t.Fatalf("stream state %+v, want %s", st, c.status)
			}
			_, err := os.Stat(filepath.Join(dir, "m.mp4"))
			if kept := err == nil; kept != c.kept {
				t.Errorf("original kept = %v, want %v", kept, c.kept)
			}
			if c.kept != (infoCode(r.job.Stats.NoteInfo) == NotePrepKeptOriginal) ||
				(c.kept && !paramsMatch(r.job.Stats.NoteInfo, "stream_status", c.status)) {
				t.Errorf("job note %+v", r.job.Stats.NoteInfo)
			}
			if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); err != nil {
				t.Error("audio.wav missing")
			}
			if c.status == StreamReady {
				if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); err != nil {
					t.Errorf("stream.mp4 must be kept: %v", err)
				}
			}
			if !c.kept {
				return
			}
			// later the stream task makes the playback file and then removes the original
			e.enqueueStream(l.ID)
			if r := e.run(&streamStage{d: e.deps(nil), media: &fakeMedia{probe: probeMP4}}, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			if st := e.streamState(l.ID); st.Status != StreamReady {
				t.Errorf("stream task state %+v", st)
			}
			if _, err := os.Stat(filepath.Join(dir, "m.mp4")); !os.IsNotExist(err) {
				t.Error("the stream task must delete the kept original once stream.mp4 exists")
			}
			if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); err != nil {
				t.Errorf("stream.mp4: %v", err)
			}
		})
	}
	// keep_original_media = true: the stream task never deletes anything
	e.cfg.Storage.KeepOriginalMedia = boolp(true)
	l, dir := e.mediaLecture("keep")
	e.enqueueStream(l.ID)
	if r := e.run(&streamStage{d: e.deps(nil), media: &fakeMedia{probe: probeMP4}}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.mp4")); err != nil {
		t.Error("keep_original_media=true: the stream task deleted the original")
	}
}

// TestStreamNotInASRHash (P8.1): stream.mp4 is not part of asr's input_hash.
func TestStreamNotInASRHash(t *testing.T) {
	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l, dir := e.mediaLecture("L")
	d := e.deps(nil)
	if r := e.run(&prepStage{d: d, media: &fakeMedia{probe: probeMP4}}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	asr := e.stage(d, model.StageASR)
	if r := e.run(asr, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	before, err := asr.InputHash(e.ctx, l.ID)
	if err != nil || before == "" {
		t.Fatalf("asr hash %q %v", before, err)
	}
	e.must(os.WriteFile(filepath.Join(dir, model.StreamFile), []byte("a different playback file"), 0o644))
	if after, _ := asr.InputHash(e.ctx, l.ID); after != before {
		t.Error("changing stream.mp4 changed the asr input hash")
	}
	os.Remove(filepath.Join(dir, model.StreamFile))
	if after, _ := asr.InputHash(e.ctx, l.ID); after != before {
		t.Error("removing stream.mp4 changed the asr input hash")
	}
}

// TestPrepCreationTime (P13.3.4, lead ruling 3): the media's creation_time
// goes to ResolveRecordedAt; a muxer default (1904) does not.
func TestPrepCreationTime(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("with creation_time")
	if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeMP4}}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	raw, ok, err := e.st.GetKV(e.ctx, KVCreationTimePrefix+l.ID)
	if err != nil || !ok || raw != "2026-09-30T13:05:12Z" {
		t.Errorf("creation_time handed over: %q %v %v", raw, ok, err)
	}
	got, err := e.st.GetLecture(e.ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RecordedAtSource != model.RecordedAtMetadata || got.RecordedAt == nil || !strings.HasPrefix(*got.RecordedAt, "2026-09-30") {
		t.Errorf("recorded_at %v (%s), want the creation_time as metadata", got.RecordedAt, got.RecordedAtSource)
	}

	l2, _ := e.mediaLecture("1904")
	if r := e.run(&prepStage{d: e.deps(nil), media: &fakeMedia{probe: probeHEVC}}, l2.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if _, ok, _ := e.st.GetKV(e.ctx, KVCreationTimePrefix+l2.ID); ok {
		t.Error("a 1904 creation_time is not a recording time")
	}
	if got, _ := e.st.GetLecture(e.ctx, l2.ID); got.RecordedAt != nil {
		t.Errorf("recorded_at %v", *got.RecordedAt)
	}
}

// TestPrepCancelDuringStream: cancelling while stream.mp4 is written stops
// prep at once and leaves no temporary file.
func TestPrepCancelDuringStream(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("L")
	fm := &fakeMedia{probe: probeMP4, block: make(chan struct{})}
	ctx, cancel := context.WithCancel(e.ctx)
	time.AfterFunc(200*time.Millisecond, cancel)
	r := e.runCtx(ctx, &prepStage{d: e.deps(nil), media: fm}, l.ID)
	if !errors.Is(r.err, context.Canceled) || r.job.Status != model.StatusCancelled {
		t.Fatalf("err %v status %s", r.err, r.job.Status)
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile+".tmp")); !os.IsNotExist(err) {
		t.Error("stream.mp4.tmp left behind")
	}
	if _, ok, _ := StreamStateOf(e.ctx, e.st, l.ID); ok {
		t.Error("a cancelled prep must not record a stream state")
	}
}

// ---------- the auxiliary task "stream" ----------

// enqueueStream creates the pending stream job of a lecture (as GET /media
// does through state.EnqueueAux).
func (e *env) enqueueStream(lectureID string) {
	e.t.Helper()
	if _, err := state.EnqueueAux(e.ctx, e.st, model.ScopeLecture, lectureID, model.StageStream); err != nil {
		e.t.Fatal(err)
	}
}

func TestStreamStageRegistered(t *testing.T) {
	e := newEnv(t)
	s, ok := ByName(e.deps(nil))[model.StageStream]
	if !ok || s.Resource() != sched.ResourceCPU {
		t.Fatalf("stream stage %v registered=%v", s, ok)
	}
	if h, err := s.InputHash(e.ctx, "x"); h != "" || err != nil {
		t.Errorf("InputHash %q %v", h, err)
	}
}

// TestStreamTask: an old lecture (prep done before v2, original kept) gets
// stream.mp4 by the rules of prep, without touching audio.wav.
func TestStreamTask(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("old")
	e.writeWAV(l, "RIFF old audio")
	e.enqueueStream(l.ID)
	fm := &fakeMedia{probe: probeHEVC}
	r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("stream %v %s", r.err, r.job.Status)
	}
	if calls := fm.ffmpegCalls(); len(calls) != 1 || !isStreamCall(calls[0]) || !argsContain(calls[0], "-c:v", encNVENC) {
		t.Errorf("ffmpeg calls %v", calls)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, model.AudioFile)); string(b) != "RIFF old audio" {
		t.Error("the stream task rewrote audio.wav")
	}
	if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
		t.Errorf("stream state %+v", st)
	}
	if r.progress.last() != 1 || !monotone(r.progress.vals) {
		t.Errorf("progress %v", r.progress.vals)
	}
	// no creation_time handling for old lectures (ruling 3 names prep only)
	if _, ok, _ := e.st.GetKV(e.ctx, KVCreationTimePrefix+l.ID); ok {
		t.Error("the stream task must not resolve recorded_at")
	}
}

// TestStreamTaskOriginalMissing: the original was deleted after prep
// (keep_original_media = false) → media.original_missing, permanent.
func TestStreamTaskOriginalMissing(t *testing.T) {
	e := newEnv(t)
	l, dir := e.mediaLecture("old")
	e.must(os.Remove(filepath.Join(dir, "m.mp4")))
	e.enqueueStream(l.ID)
	fm := &fakeMedia{probe: probeMP4}
	r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
	var ce *model.CodedError
	if r.err == nil || !sched.IsPermanent(r.err) || !errors.As(r.err, &ce) || ce.Info.Code != MediaOriginalMissing {
		t.Fatalf("err %v", r.err)
	}
	if r.job.Status != model.StatusFailed || len(fm.ffmpegCalls()) != 0 {
		t.Errorf("status %s, ffmpeg calls %v", r.job.Status, fm.ffmpegCalls())
	}
	if info := streamError(t, e.streamState(l.ID)); info.Code != MediaOriginalMissing || !strings.Contains(info.Detail, "m.mp4") {
		t.Errorf("stream error %+v", info)
	}
}

// TestStreamTaskOutcomes: audio-only, no space, and D6 error classes of the
// task (the job fails with the CodedError; the kv records it).
func TestStreamTaskOutcomes(t *testing.T) {
	e := newEnv(t)
	fail := func(stderr string) func([]string) (string, error) {
		return func([]string) (string, error) { return stderr, errors.New("exit status 1") }
	}
	cases := []struct {
		name   string
		fm     *fakeMedia
		status string
		code   string // "" = the job is done
	}{
		{"audio only", &fakeMedia{probe: probeM4A}, StreamAudioOnly, ""},
		{"no space", &fakeMedia{probe: probeMP4, free: 1 << 30}, StreamNoSpace, ""},
		{"corrupt", &fakeMedia{probe: probeMP4, fail: fail("[mov,mp4 @ 0] moov atom not found\nm.mp4: Invalid data found when processing input")}, StreamFailed, MediaCorrupt},
		{"unsupported", &fakeMedia{probe: probeHEVC, fail: fail("[vist#0:1] Decoder (codec hevc) not found for input stream #0:1")}, StreamFailed, MediaUnsupported},
		{"no audio", &fakeMedia{probe: `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv420p"}], "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}}`}, StreamFailed, MediaNoAudio},
		{"other", &fakeMedia{probe: probeMP4, fail: fail("Conversion failed!")}, StreamFailed, MediaFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, _ := e.mediaLecture(c.name)
			e.enqueueStream(l.ID)
			r := e.run(&streamStage{d: e.deps(nil), media: c.fm}, l.ID)
			st := e.streamState(l.ID)
			if st.Status != c.status {
				t.Errorf("state %+v, want %s", st, c.status)
			}
			if c.code == "" {
				if r.err != nil || r.job.Status != model.StatusDone {
					t.Errorf("job %v %s", r.err, r.job.Status)
				}
				return
			}
			var ce *model.CodedError
			if r.err == nil || !errors.As(r.err, &ce) || ce.Info.Code != c.code || r.job.Status != model.StatusFailed {
				t.Errorf("job err %v status %s, want %s", r.err, r.job.Status, c.code)
			}
			if info := streamError(t, st); info.Code != c.code {
				t.Errorf("kv error %+v", info)
			}
		})
	}
}

// ---------- real ffmpeg (skipped without ffmpeg/ffprobe, SPEC §14) ----------

// realMediaOrSkip returns ffmpeg after checking that ffprobe is next to it
// or in PATH.
func realMediaOrSkip(t *testing.T, e *env) string {
	t.Helper()
	ff := ffmpegOrSkip(t, e.cfg)
	if _, err := ffprobeFor(ff); err != nil {
		t.Skipf("ffprobe not found (%v): the real playback-file test is skipped as allowed by SPEC §14", err)
	}
	return ff
}

// realH264OrSkip is an H.264 encoder of the real ffmpeg for making a test
// input: libx264 when listed, else the first usable encoder of V7.
func realH264OrSkip(t *testing.T, e *env, ff string) string {
	t.Helper()
	out, err := runCombined(e.ctx, ff, []string{"-hide_banner", "-encoders"})
	if err != nil {
		t.Skipf("ffmpeg -encoders: %v", err)
	}
	if parseEncoderList(out)["libx264"] {
		return "libx264"
	}
	checks, err := defaultEncoderProber.check(e.ctx, ff)
	if err != nil {
		t.Skipf("encoder check: %v", err)
	}
	if u := usableEncoders(checks); len(u) > 0 {
		return u[0]
	}
	t.Skip("this ffmpeg has no usable H.264 encoder: the remux test input cannot be made (SPEC §14)")
	return ""
}

// TestPrepRealStream runs prep with the real ffmpeg/ffprobe on a 2-second
// H.264/AAC MP4 (remux) and an MPEG-4/MP3 AVI (transcode; prep queues the
// stream task, which is run next: stream-parallel). With less than 10 GB
// free on the temp volume the state must be no_space instead.
func TestPrepRealStream(t *testing.T) {
	e := newEnv(t)
	ff := realMediaOrSkip(t, e)
	free, ferr := diskFree(e.root)
	cases := []struct {
		name, file string
		args       []string
		transcoded bool
	}{
		{"remux", "in.mp4", []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac"}, false},
		{"transcode", "in.avi", []string{"-c:v", "mpeg4", "-c:a", "mp2"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := e.lecture(c.name, c.file)
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			args := c.args
			if !c.transcoded {
				// The H.264 test input: libx264 when this ffmpeg has it (a test
				// input only; the playback file never uses it), else the
				// encoder the playback file would use (an LGPL build).
				args = slices.Clone(args)
				args[1] = realH264OrSkip(t, e, ff)
			}
			gen := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-shortest"}, args...)
			if out, err := exec.Command(ff, append(gen, filepath.Join(dir, c.file))...).CombinedOutput(); err != nil {
				t.Fatalf("generating the test input: %v\n%s", err, out)
			}
			r := e.run(e.stage(e.deps(nil), model.StagePrep), l.ID)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if c.transcoded {
				// stream-parallel: prep wrote audio.wav and queued the stream
				// task; the transcode is that task's
				if _, ok, _ := StreamStateOf(e.ctx, e.st, l.ID); ok {
					t.Error("prep recorded a playback file state for a transcode")
				}
				if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
					t.Errorf("prep made stream.mp4 itself: %v", err)
				}
				if j := e.job(model.ScopeLecture, l.ID, model.StageStream); j.Status != model.StatusPending {
					t.Fatalf("stream task %s, want pending (queued by prep)", j.Status)
				}
				if r := e.run(e.stage(e.deps(nil), model.StageStream), l.ID); r.err != nil {
					t.Fatal(r.err)
				}
			} else if e.hasCurrentJob(l.ID, model.StageStream) {
				t.Error("prep queued the stream task for a remux")
			}
			st := e.streamState(l.ID)
			if ferr == nil && free < StreamMinFreeBytes {
				if st.Status != StreamNoSpace {
					t.Errorf("only %d bytes free: state %+v, want no_space", free, st)
				}
				return
			}
			if c.transcoded && st.Status == StreamFailed {
				// SPEC-v3 V7: no usable H.264 encoder on this machine → audio mode
				if info := streamError(t, st); info.Code == MediaNoH264Encoder {
					t.Logf("no usable H.264 encoder here: %s", info.Detail)
					return
				}
			}
			fi, err := os.Stat(filepath.Join(dir, model.StreamFile))
			if err != nil || st.Status != StreamReady || st.Transcoded != c.transcoded || st.SizeBytes != fi.Size() {
				t.Fatalf("state %+v, stream.mp4 %v", st, err)
			}
			tools := &execMedia{ffmpeg: ff}
			out, _, err := tools.Probe(e.ctx, filepath.Join(dir, model.StreamFile))
			if err != nil {
				t.Fatal(err)
			}
			info, err := parseProbe(out)
			if err != nil || planOf(info) != planRemux {
				t.Errorf("stream.mp4 is not browser-ready H.264/AAC MP4: %+v %v", info, err)
			}
		})
	}
}
