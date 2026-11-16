// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/db"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// Security review 2, M3 (media-integ): prep and the stream task read a
// lecture's media with the whitelists of media.InputArgs, refuse a text
// playlist before ffmpeg sees it (media.format_rejected), report a
// whitelist refusal of ffmpeg / ffprobe with that code, and never open a
// stored path that names another computer.

// inputGuarded reports whether the ffmpeg arguments read input with the
// whitelists of media.InputArgs: among the options of that input (after the
// previous input's "-i x", before its own "-i") are -format_whitelist
// media.FormatWhitelist and -protocol_whitelist file. After its "-i" they
// would be output options, which ffmpeg applies to the output and so
// silently do nothing for the input: not counted.
func inputGuarded(args []string, input string) bool {
	start := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-i" {
			continue
		}
		if args[i+1] == input {
			return hasOptions(args[start:i], lfmedia.InputArgs())
		}
		start = i + 2
	}
	return false
}

// hasOptions reports whether opts holds each option of want ("-name",
// "value" pairs) with its value.
func hasOptions(opts, want []string) bool {
	for k := 0; k+1 < len(want); k += 2 {
		found := false
		for i := 0; i+1 < len(opts); i++ {
			if opts[i] == want[k] && opts[i+1] == want[k+1] {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Every ffmpeg / ffprobe argument list that reads a lecture's media has
// the whitelists before the input; the check itself tells a misplaced
// option (after -i) from a right one.
func TestMediaInputArgsPosition(t *testing.T) {
	media, out := filepath.Join(t.TempDir(), "in.mp4"), filepath.Join(t.TempDir(), "out.tmp")
	if a := audioArgs(media, out, nil); !inputGuarded(a, media) || a[len(a)-1] != out {
		t.Errorf("audio.wav args %v", a)
	}
	info := mediaInfo{Video: &mediaStream{Index: 0, Codec: "h264"}, Audio: &mediaStream{Index: 1, Codec: "aac"}}
	for _, enc := range []string{"", encQSV, encOpenH264} {
		if a := streamArgs(info, media, out, enc); !inputGuarded(a, media) {
			t.Errorf("stream.mp4 args (%q) %v", enc, a)
		}
	}
	p := probeArgs(media)
	if p[len(p)-1] != media || !hasOptions(p[:len(p)-1], lfmedia.InputArgs()) {
		t.Errorf("ffprobe args %v", p)
	}
	misplaced := append(append([]string{"-y", "-i", media}, lfmedia.InputArgs()...), out)
	if inputGuarded(misplaced, media) {
		t.Error("whitelists after -i counted as input options")
	}
	other := append(append(append([]string{"-y"}, lfmedia.InputArgs()...), "-i", "first.mp4", "-i", media), out)
	if inputGuarded(other, media) {
		t.Error("the whitelists of another input counted")
	}
}

// prep and the stream task pass the whitelists on every ffmpeg call that
// reads the media: remux, transcode, audio.wav.
func TestPrepStreamCallsGuarded(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, probe string
	}{{"remux", probeMP4}, {"transcode", probeHEVC}} {
		t.Run(c.name, func(t *testing.T) {
			l, dir := e.mediaLecture(c.name)
			media, _ := filepath.Abs(filepath.Join(dir, "m.mp4"))
			fm := &fakeMedia{probe: c.probe}
			if c.probe == probeHEVC {
				if _, r := e.prepThenStream(fm, l.ID); r.err != nil {
					t.Fatal(r.err)
				}
			} else if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
				t.Fatal(r.err)
			}
			calls := fm.ffmpegCalls()
			if len(calls) != 2 {
				t.Fatalf("ffmpeg calls %v", calls)
			}
			for _, a := range calls {
				if !inputGuarded(a, media) {
					t.Errorf("unguarded call %v", a)
				}
			}
		})
	}
}

// A media file that is a text playlist is refused by prep and by the
// stream task before any program runs (media.format_rejected, permanent).
func TestPrepRefusesPlaylistMedia(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		content, kind string
	}{
		{"#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nC:\\Users\\someone\\other.ts\n#EXT-X-ENDLIST\n", "m3u8"},
		{"\xef\xbb\xbf  ffconcat version 1.0\nfile 'x.wav'\n", "ffconcat"},
		{"<?xml version=\"1.0\"?><MPD/>", "xml"},
	} {
		l, dir := e.mediaLecture(c.kind)
		e.must(os.WriteFile(filepath.Join(dir, "m.mp4"), []byte(c.content), 0o644))
		fm := &fakeMedia{probe: probeMP4}
		r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
		var ce *model.CodedError
		if !errors.As(r.err, &ce) || ce.Info.Code != MediaFormatRejected || !paramsMatch(&ce.Info, "format", c.kind) || !sched.IsPermanent(r.err) ||
			r.job.Status != model.StatusFailed || jobErrorCode(r.job) != MediaFormatRejected {
			t.Errorf("%s: prep err %v job %s", c.kind, r.err, r.job.Status)
		}
		if calls := fm.ffmpegCalls(); len(calls) != 0 {
			t.Errorf("%s: ffmpeg ran on a playlist: %v", c.kind, calls)
		}
		if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); !os.IsNotExist(err) {
			t.Errorf("%s: audio.wav written", c.kind)
		}
		// the stream task (an old lecture's playback file) refuses it too
		e.enqueueStream(l.ID)
		r = e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		if codeOf(r.err) != MediaFormatRejected || r.job.Status != model.StatusFailed {
			t.Errorf("%s: stream task err %v job %s", c.kind, r.err, r.job.Status)
		}
		if info := streamError(t, e.streamState(l.ID)); info.Code != MediaFormatRejected || fmt.Sprint(info.Params["format"]) != c.kind {
			t.Errorf("%s: kv %+v", c.kind, info)
		}
		if calls := fm.ffmpegCalls(); len(calls) != 0 {
			t.Errorf("%s: ffmpeg ran on a playlist: %v", c.kind, calls)
		}
	}
}

// What ffprobe and ffmpeg print when the whitelists refuse an input is
// media.format_rejected (permanent), with the demuxer or protocol when the
// output names it; prep stops at a refused probe (the conversion would be
// refused as well) and runs no ffmpeg.
func TestFormatRejectedClasses(t *testing.T) {
	exit := errors.New("exit status 1")
	wl := "'" + lfmedia.FormatWhitelist + "'"
	for _, c := range []struct{ stderr, format string }{
		{"[hls @ 000001] Format not on whitelist " + wl + "\nx.m3u8: Invalid argument", "hls"},
		{"[concat @ 000002374c0a90c0] Format not on whitelist " + wl, "concat"},
		{"[in#0 @ 0000027b863a8540] Format not on whitelist " + wl + "\n[in#0 @ 0000027b863a81c0] Error opening input: Invalid argument\nError opening input files: Invalid argument", "?"},
		{"[http @ 0000] Protocol 'http' not on whitelist 'file'!\n[mov,mp4 @ 0000] Invalid data found when processing input", "http"},
	} {
		err := mediaError(c.stderr, exit)
		var ce *model.CodedError
		if !errors.As(err, &ce) || ce.Info.Code != MediaFormatRejected || !paramsMatch(&ce.Info, "format", c.format) || !sched.IsPermanent(err) {
			t.Errorf("%q: %v", c.stderr, err)
		}
		if !strings.Contains(errorInfoJSON(err), "not on whitelist") {
			t.Errorf("%q: the detail lacks ffmpeg's output: %s", c.stderr, errorInfoJSON(err))
		}
	}

	e := newEnv(t)
	l, dir := e.mediaLecture("probe refused")
	fm := &fakeMedia{probeErr: exit, probeStderr: "[nut @ 0000] Format not on whitelist " + wl}
	r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
	if codeOf(r.err) != MediaFormatRejected || !paramsMatch(errInfo(r.err), "format", "nut") || !sched.IsPermanent(r.err) || r.job.Status != model.StatusFailed {
		t.Errorf("prep after a refused probe: %v %s", r.err, r.job.Status)
	}
	if calls := fm.ffmpegCalls(); len(calls) != 0 {
		t.Errorf("ffmpeg ran after the probe was refused: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); !os.IsNotExist(err) {
		t.Error("audio.wav written")
	}
	// a probe that fails otherwise still leaves the verdict to the conversion
	l2, _ := e.mediaLecture("conversion refused")
	fm = &fakeMedia{probeErr: exit, probeStderr: "x.mp4: Invalid data found when processing input",
		fail: func([]string) (string, error) { return "[in#0 @ 0000] Format not on whitelist " + wl, exit }}
	r = e.run(&prepStage{d: e.deps(nil), media: fm}, l2.ID)
	if codeOf(r.err) != MediaFormatRejected || !paramsMatch(errInfo(r.err), "format", "?") || !sched.IsPermanent(r.err) || len(fm.ffmpegCalls()) != 1 {
		t.Errorf("prep with a refused conversion: %v, calls %v", r.err, fm.ffmpegCalls())
	}
}

func errInfo(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return &ce.Info
	}
	return nil
}

// A stored path naming another computer (UNC, device path, URL; only a
// database edited elsewhere has one) is a missing file for prep, the stream
// task, the slides and the syllabus: nothing is opened (no ffmpeg run), and
// it is logged once.
func TestNetworkPathsNotOpened(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEnv(t)
	// 192.0.2.0/24 is TEST-NET-1 (RFC 5737): no such computer. The paths are
	// unique per run, as each is logged once per process.
	u := e.course.ID
	for i, p := range []string{`\\192.0.2.10\share\` + u + `.mp4`, `//192.0.2.11/share/` + u + `.mp4`, `\\?\UNC\192.0.2.12\share\` + u + `.mp4`, `file://192.0.2.13/share/` + u + `.mp4`} {
		l := e.lecture(fmt.Sprintf("net%d", i), p)
		fm := &fakeMedia{probe: probeMP4}
		r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
		if codeOf(r.err) != CodePrepMediaMissing || !sched.IsPermanent(r.err) {
			t.Errorf("%s: prep %v", p, r.err)
		}
		e.enqueueStream(l.ID)
		r = e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID)
		if codeOf(r.err) != MediaOriginalMissing || r.job.Status != model.StatusFailed {
			t.Errorf("%s: stream task %v", p, r.err)
		}
		if calls := fm.ffmpegCalls(); len(calls) != 0 {
			t.Errorf("%s: ffmpeg ran: %v", p, calls)
		}
		if n := strings.Count(logBuf.String(), "path="+quoteLog(p)); n != 1 {
			t.Errorf("%s logged %d times, want once:\n%s", p, n, logBuf.String())
		}
	}

	slides, syllabus := `\\192.0.2.20\share\`+u+`.pdf`, `\\192.0.2.21\share\`+u+`.txt`
	m := model.Material{ID: db.NewID(), CourseID: e.course.ID, Filename: "slides.pdf", Path: slides, CreatedAt: db.Now()}
	e.must(e.st.CreateMaterial(e.ctx, m))
	e.useFakeWorker("exit2", "") // would fail differently if it ran
	if r := e.run(e.stage(e.deps(nil), model.StageMaterialExtract), m.ID); codeOf(r.err) != CodeMaterialFileMissing || !sched.IsPermanent(r.err) {
		t.Errorf("material: %v", r.err)
	}
	e.must(e.st.UpsertSyllabus(e.ctx, model.Syllabus{CourseID: e.course.ID, Filename: "syllabus.txt", Path: syllabus}))
	e.must(state.ResetGlossarySeed(e.ctx, e.st, e.course.ID))
	if r := e.run(e.stage(e.deps(nil), model.StageGlossarySeed), e.course.ID); codeOf(r.err) != CodeGlossarySyllabusMissing {
		t.Errorf("syllabus: %v", r.err)
	}
	for _, p := range []string{slides, syllabus} {
		if !strings.Contains(logBuf.String(), "path="+quoteLog(p)) {
			t.Errorf("%s not logged:\n%s", p, logBuf.String())
		}
	}
}

// quoteLog is how slog's text handler writes a string value.
func quoteLog(s string) string {
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.MessageKey {
			return slog.Attr{}
		}
		return a
	}})).Info("", "v", s)
	return strings.TrimSuffix(strings.TrimPrefix(b.String(), "v="), "\n")
}

// ---------- with the real ffmpeg (skipped without one, SPEC §14) ----------

// guardFFmpeg is the bundled ffmpeg when the desktop build has staged it
// (desktop/build/resources/bin), else the one in PATH, with its ffprobe.
func guardFFmpeg(t *testing.T) string {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	ff, _ := filepath.Abs(filepath.Join("..", "..", "desktop", "build", "resources", "bin", "ffmpeg"+exe))
	if fi, err := os.Stat(ff); err != nil || fi.IsDir() {
		p, err := exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("ffmpeg not found: the real whitelist tests are skipped (SPEC §14)")
		}
		ff = p
	}
	if _, err := ffprobeFor(ff); err != nil {
		t.Skipf("ffprobe not found (%v): the real whitelist tests are skipped (SPEC §14)", err)
	}
	return ff
}

// softwareTools is execMedia offering only the software H.264 encoder
// (OpenH264): these tests never open a hardware encoder.
type softwareTools struct{ *execMedia }

func (m softwareTools) H264Encoders(ctx context.Context) ([]encoderCheck, error) {
	out, err := runCombined(ctx, m.ffmpeg, []string{"-hide_banner", "-encoders"})
	if err != nil {
		return nil, err
	}
	c := encoderCheck{Name: encOpenH264, Listed: parseEncoderList(out)[encOpenH264]}
	if c.Listed {
		if out, err := runCombined(ctx, m.ffmpeg, trialArgs(encOpenH264)); err != nil {
			c.Err = fmt.Sprintf("%v: %s", err, out)
		}
	}
	return []encoderCheck{c}, nil
}

func genMedia(t *testing.T, ff, out string, args ...string) {
	t.Helper()
	all := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	if b, err := exec.Command(ff, append(all, out)...).CombinedOutput(); err != nil {
		t.Fatalf("generating %s: %v\n%s", filepath.Base(out), err, b)
	}
}

const guardSine = "sine=frequency=440:duration=1"

// The playlists of review M3 reach a file outside the lecture directory
// when ffmpeg reads them without the whitelists; prep and the stream task
// refuse them — the ones that look like a playlist by their first bytes, a
// container outside the whitelist by ffprobe — and the ffmpeg calls of
// audio.wav and stream.mp4 refuse a playlist by themselves too (the
// whitelists are effective where they stand: right before -i).
func TestRealPrepRefusesPlaylists(t *testing.T) {
	ff := guardFFmpeg(t)
	e := newEnv(t)
	tools := softwareTools{&execMedia{ffmpeg: ff}}
	outside := filepath.Join(e.root, "outside")
	e.must(os.MkdirAll(outside, 0o755))
	seg := filepath.Join(outside, "seg.ts")
	genMedia(t, ff, seg, "-f", "lavfi", "-i", guardSine, "-c:a", "mp2", "-f", "mpegts")
	hls := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\n%s\n#EXT-X-ENDLIST\n", seg)

	// the attack works without the whitelists (so the refusals below mean something)
	plain := filepath.Join(e.root, "plain.m3u8")
	e.must(os.WriteFile(plain, []byte(hls), 0o644))
	if b, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", plain, "-vn", "-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le", "-f", "wav", filepath.Join(e.root, "leak.wav")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg without the whitelists did not follow the playlist: %v\n%s", err, b)
	}

	cases := []struct {
		name, file, format string
		write              func(path string)
	}{
		{"hls", "media.m3u8", "m3u8", func(p string) { e.must(os.WriteFile(p, []byte(hls), 0o644)) }},
		{"hls as mp4", "media.mp4", "m3u8", func(p string) { e.must(os.WriteFile(p, []byte(hls), 0o644)) }},
		{"ffconcat as mp3", "media.mp3", "ffconcat", func(p string) {
			e.must(os.WriteFile(p, []byte("ffconcat version 1.0\nfile '"+filepath.ToSlash(seg)+"'\n"), 0o644))
		}},
		// a real container outside the whitelist: only ffprobe can tell
		{"nut as wav", "media.wav", "nut", func(p string) { genMedia(t, ff, p, "-f", "lavfi", "-i", guardSine, "-c:a", "pcm_s16le", "-f", "nut") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := e.lecture(c.name, c.file)
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			c.write(filepath.Join(dir, c.file))
			r := e.run(&prepStage{d: e.deps(nil), media: tools}, l.ID)
			if codeOf(r.err) != MediaFormatRejected || !paramsMatch(errInfo(r.err), "format", c.format) || !sched.IsPermanent(r.err) ||
				r.job.Status != model.StatusFailed || r.job.Attempt != 0 {
				t.Fatalf("prep: %v (job %s attempt %d), want %s format %s", r.err, r.job.Status, r.job.Attempt, MediaFormatRejected, c.format)
			}
			for _, f := range []string{model.AudioFile, model.AudioFile + ".tmp"} {
				if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
					t.Errorf("%s exists", f)
				}
			}
			e.enqueueStream(l.ID)
			r = e.run(&streamStage{d: e.deps(nil), media: tools}, l.ID)
			if codeOf(r.err) != MediaFormatRejected || r.job.Status != model.StatusFailed {
				t.Errorf("stream task: %v", r.err)
			}
			if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
				t.Error("stream.mp4 made from a playlist")
			}
		})
	}

	// The ffmpeg calls themselves, past the first-bytes check and the probe.
	dir := filepath.Join(e.root, "direct")
	e.must(os.MkdirAll(dir, 0o755))
	pl := filepath.Join(dir, "media.m3u8")
	e.must(os.WriteFile(pl, []byte(hls), 0o644))
	ps := &prepStage{d: e.deps(nil), media: tools}
	err := ps.convertAudio(e.ctx, tools, model.Lecture{MediaPath: pl}, dir, pl, nil, func(float64) {})
	if codeOf(err) != MediaFormatRejected || !sched.IsPermanent(err) {
		t.Errorf("audio.wav from a playlist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); !os.IsNotExist(err) {
		t.Error("audio.wav made from a playlist")
	}
	remux := mediaInfo{Format: "mov,mp4,m4a,3gp,3g2,mj2", Video: &mediaStream{Index: 0, Codec: "h264", PixFmt: "yuv420p"}, Audio: &mediaStream{Index: 0, Codec: "aac"}}
	if free, err := diskFree(dir); err == nil && free < StreamMinFreeBytes {
		t.Logf("only %d bytes free: stream.mp4 is not attempted (no_space)", free)
	} else if _, err := makeStream(e.ctx, tools, dir, pl, remux, func(float64) {}); codeOf(err) != MediaFormatRejected {
		t.Errorf("stream.mp4 from a playlist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, model.StreamFile)); !os.IsNotExist(err) {
		t.Error("stream.mp4 made from a playlist")
	}
	if _, err := probeMedia(e.ctx, tools, pl); codeOf(err) != MediaFormatRejected || !paramsMatch(errInfo(err), "format", "hls") {
		t.Errorf("ffprobe on a playlist: %v", err)
	}
}

// The recordings of the accepted types (the 13 of internal/media's real
// test) go through prep as before: audio.wav, and a playback file — the
// stream task's transcode for the video ones (OpenH264 only), audio mode
// for the audio ones; an H.264/AAC MP4 is remuxed.
func TestRealPrepAcceptsFormats(t *testing.T) {
	ff := guardFFmpeg(t)
	e := newEnv(t)
	tools := softwareTools{&execMedia{ffmpeg: ff}}
	free, ferr := diskFree(e.root)
	lowSpace := ferr == nil && free < StreamMinFreeBytes
	video := []string{"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1", "-f", "lavfi", "-i", guardSine}
	v := func(args ...string) []string { return append(slices.Clone(video), args...) }
	cases := []struct {
		file  string
		args  []string
		video bool
	}{
		{"a.wav", []string{"-f", "lavfi", "-i", guardSine}, false},
		{"a.flac", []string{"-f", "lavfi", "-i", guardSine}, false},
		{"a.m4a", []string{"-f", "lavfi", "-i", guardSine, "-c:a", "aac"}, false},
		{"a.aiff", []string{"-f", "lavfi", "-i", guardSine}, false},
		{"a.ogg", []string{"-f", "lavfi", "-i", guardSine, "-c:a", "flac"}, false},
		{"a.wma", []string{"-f", "lavfi", "-i", guardSine, "-c:a", "wmav2"}, false},
		{"v.mp4", v("-c:v", "mpeg4", "-c:a", "aac"), true},
		{"v.mkv", v("-c:v", "mpeg4", "-c:a", "flac"), true},
		{"v.avi", v("-c:v", "mpeg4", "-c:a", "pcm_s16le"), true},
		{"v.ts", v("-c:v", "mpeg2video", "-c:a", "mp2"), true},
		{"v.mpg", v("-c:v", "mpeg2video", "-c:a", "mp2", "-f", "mpeg"), true},
		{"v.wmv", v("-c:v", "wmv2", "-c:a", "wmav2"), true},
		{"v.flv", v("-c:v", "flv1", "-c:a", "aac", "-ar", "44100"), true},
	}
	out, err := runCombined(e.ctx, ff, []string{"-hide_banner", "-encoders"})
	if err != nil {
		t.Fatal(err)
	}
	enc := parseEncoderList(out)
	h264 := ""
	for _, n := range []string{"libx264", encOpenH264} { // a test input only
		if enc[n] {
			h264 = n
			break
		}
	}
	if h264 != "" {
		cases = append(cases, struct {
			file  string
			args  []string
			video bool
		}{"remux.mp4", v("-c:v", h264, "-pix_fmt", "yuv420p", "-c:a", "aac"), true})
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			l := e.lecture(c.file, c.file)
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			genMedia(t, ff, filepath.Join(dir, c.file), c.args...)
			r := e.run(&prepStage{d: e.deps(nil), media: tools}, l.ID)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep: %v %s", r.err, r.job.Status)
			}
			if fi, err := os.Stat(filepath.Join(dir, model.AudioFile)); err != nil || fi.Size() <= 44 {
				t.Fatalf("audio.wav: %v", err)
			}
			if e.hasCurrentJob(l.ID, model.StageStream) {
				if r := e.run(&streamStage{d: e.deps(nil), media: tools}, l.ID); r.err != nil && codeOf(r.err) == MediaFormatRejected {
					t.Fatalf("stream task refused an accepted format: %v", r.err)
				}
			}
			st := e.streamState(l.ID)
			switch {
			case lowSpace && c.video:
				if st.Status != StreamNoSpace {
					t.Errorf("state %+v, want no_space (%d bytes free)", st, free)
				}
			case !c.video:
				if st.Status != StreamAudioOnly {
					t.Errorf("state %+v, want audio_only", st)
				}
			case st.Status == StreamFailed && streamError(t, st).Code == MediaNoH264Encoder:
				t.Logf("no OpenH264 in this ffmpeg: audio mode (%s)", streamError(t, st).Detail)
			case st.Status != StreamReady || st.Transcoded != (c.file != "remux.mp4"):
				t.Errorf("state %+v", st)
			}
		})
	}
}
