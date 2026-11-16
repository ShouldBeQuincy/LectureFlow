// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// test-media I3 (several audio tracks), I5 (H.264 + AAC in another
// container) and I9 (truncated files).

// probeTwoAudio: video, then a first audio track that is not marked default
// and a second one that is (test-media m02).
const probeTwoAudio = `{"streams": [
 {"index": 0, "codec_type": "video", "codec_name": "h264", "pix_fmt": "yuv420p", "disposition": {"default": 1}},
 {"index": 1, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 0}},
 {"index": 2, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 1}}],
 "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "60.0"}}`

func TestParseProbeAudioTrack(t *testing.T) {
	cases := []struct {
		name, json string
		want       int
	}{
		{"default second", probeTwoAudio, 2},
		{"none default", `{"streams": [
		 {"index": 0, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 0}},
		 {"index": 1, "codec_type": "audio", "codec_name": "opus", "disposition": {"default": 0}}], "format": {"format_name": "matroska,webm"}}`, 0},
		{"both default", `{"streams": [
		 {"index": 0, "codec_type": "video", "codec_name": "h264"},
		 {"index": 1, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 1}},
		 {"index": 2, "codec_type": "audio", "codec_name": "aac", "disposition": {"default": 1}}], "format": {"format_name": "mov,mp4"}}`, 1},
		{"no disposition", `{"streams": [
		 {"index": 3, "codec_type": "audio", "codec_name": "mp3"},
		 {"index": 4, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mpegts"}}`, 3},
	}
	for _, c := range cases {
		info, err := parseProbe([]byte(c.json))
		if err != nil || info.Audio == nil || info.Audio.Index != c.want {
			t.Errorf("%s: audio %+v (%v), want index %d", c.name, info.Audio, err, c.want)
		}
	}
	if info, _ := parseProbe([]byte(probeTwoAudio)); info.Duration != 60 {
		t.Errorf("duration %v", info.Duration)
	}
}

// I3: prep maps the same audio track into audio.wav and stream.mp4.
func TestPrepSameAudioTrack(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	fm := &fakeMedia{probe: probeTwoAudio}
	if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	calls := fm.ffmpegCalls()
	if len(calls) != 2 || !isStreamCall(calls[1]) {
		t.Fatalf("ffmpeg calls %v", calls)
	}
	if !argsContain(calls[0], "-map", "0:2") || argsContain(calls[0], "-map", "0:1") {
		t.Errorf("audio.wav args %v: want -map 0:2", calls[0])
	}
	if !argsContain(calls[1], "-map", "0:0", "-map", "0:2") {
		t.Errorf("stream.mp4 args %v: want -map 0:0 -map 0:2", calls[1])
	}
	// the -map is an output option: after the input's -i
	if i, m := slices.Index(calls[0], "-i"), slices.Index(calls[0], "-map"); m < i {
		t.Errorf("-map before -i: %v", calls[0])
	}
}

// I3: without a probe, ffmpeg picks the track of audio.wav as before.
func TestPrepAudioTrackProbeFailed(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	fm := &fakeMedia{probeErr: errors.New("exit status 1"), probeStderr: "something odd"}
	if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	if calls := fm.ffmpegCalls(); len(calls) != 1 || slices.Contains(calls[0], "-map") {
		t.Errorf("ffmpeg calls %v", calls)
	}
}

// I3 with the real ffmpeg: two audio tracks, neither marked default, the
// first a mono tone and the second stereo silence (test-media m04). ffmpeg
// on its own takes the stereo track (more channels) for audio.wav; prep
// must take the first, the one stream.mp4 plays.
func TestRealPrepAudioTrack(t *testing.T) {
	ff := guardFFmpeg(t)
	e := newEnv(t)
	tools := softwareTools{&execMedia{ffmpeg: ff}}
	l := e.lecture("tracks", "tracks.mka")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	genMedia(t, ff, filepath.Join(dir, "tracks.mka"),
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=44100",
		"-map", "0:a", "-map", "1:a", "-t", "3", "-ac:a:0", "1", "-c:a", "flac",
		"-disposition:a:0", "0", "-disposition:a:1", "0")
	if r := e.run(&prepStage{d: e.deps(nil), media: tools}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	got := readPCM16(t, filepath.Join(dir, model.AudioFile))
	if r := rms(got, [2]float64{0.5, 2.5}); r < 1000 {
		t.Errorf("audio.wav rms %.0f: the silent second track was transcribed, not the first", r)
	}
}

// I5: H.264 + AAC in a container other than MP4 is remuxed, not transcoded.
func TestPrepRemuxOtherContainer(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	fm := &fakeMedia{probe: strings.Replace(probeMP4, "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm", 1)}
	if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
		t.Fatal(r.err)
	}
	calls := fm.streamCalls()
	if len(calls) != 1 || !argsContain(calls[0], "-c", "copy") || !argsContain(calls[0], "-f", "mp4") {
		t.Fatalf("stream calls %v: want one remux in prep", calls)
	}
	if e.hasCurrentJob(l.ID, model.StageStream) {
		t.Error("the stream task was queued for a remux")
	}
	if st := e.streamState(l.ID); st.Status != StreamReady || st.Transcoded {
		t.Errorf("state %+v", st)
	}
	if fm.encCalls != 0 {
		t.Errorf("encoders checked %d times for a remux that worked", fm.encCalls)
	}
}

// I5: when the remux out of another container fails in prep, prep does not
// transcode (review R5): it is done, with audio.wav for asr, and leaves the
// transcode to the stream task, which tries the remux once more and then
// transcodes; the state says transcoded. Out of an MP4 a failed remux stays
// the error it was.
func TestRemuxFallbackToTranscode(t *testing.T) {
	failRemux := func(args []string) (string, error) {
		if isStreamCall(args) && argsContain(args, "-c", "copy") {
			return "[mp4 @ 0x1] Could not write header (incorrect codec parameters ?): Invalid argument", errors.New("exit status 1")
		}
		return "", nil
	}
	t.Run("mkv", func(t *testing.T) {
		e := newEnv(t)
		e.cfg.Storage.KeepOriginalMedia = boolp(false)
		l, dir := e.mediaLecture("L")
		fm := &fakeMedia{probe: strings.Replace(probeMP4, "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm", 1), fail: failRemux}
		r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("prep %v %s", r.err, r.job.Status)
		}
		if calls := fm.streamCalls(); len(calls) != 1 || !argsContain(calls[0], "-c", "copy") {
			t.Fatalf("prep stream calls %v: want the remux only, no transcode in prep", calls)
		}
		if fm.encCalls != 0 {
			t.Errorf("prep checked the encoders %d times: the transcode is the stream task's", fm.encCalls)
		}
		if _, err := os.Stat(filepath.Join(dir, model.AudioFile)); err != nil {
			t.Errorf("audio.wav for asr: %v", err)
		}
		if st, ok, _ := StreamStateOf(e.ctx, e.st, l.ID); ok {
			t.Errorf("prep recorded a playback file state %+v; the stream task makes the file", st)
		}
		if j := e.job(model.ScopeLecture, l.ID, model.StageStream); j.Status != model.StatusPending {
			t.Fatalf("stream task %s after prep, want pending", j.Status)
		}
		if _, err := os.Stat(filepath.Join(dir, "m.mp4")); err != nil {
			t.Errorf("keep_original_media=false: prep deleted the original the stream task needs: %v", err)
		}

		if r := e.run(&streamStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		calls := fm.streamCalls()
		if len(calls) != 3 || !argsContain(calls[1], "-c", "copy") || !argsContain(calls[2], "-c:v", encNVENC) {
			t.Fatalf("stream calls %v: want prep's remux, then the stream task's remux and transcode", calls)
		}
		if st := e.streamState(l.ID); st.Status != StreamReady || !st.Transcoded {
			t.Errorf("state %+v", st)
		}
	})
	t.Run("mp4", func(t *testing.T) {
		e := newEnv(t)
		l, _ := e.mediaLecture("L")
		fm := &fakeMedia{probe: probeMP4, fail: failRemux}
		if r := e.run(&prepStage{d: e.deps(nil), media: fm}, l.ID); r.err != nil {
			t.Fatal(r.err)
		}
		if calls := fm.streamCalls(); len(calls) != 1 {
			t.Errorf("stream calls %v: an MP4 remux has no fallback", calls)
		}
		if st := e.streamState(l.ID); st.Status != StreamFailed {
			t.Errorf("state %+v", st)
		}
		if e.hasCurrentJob(l.ID, model.StageStream) {
			t.Error("the stream task was queued for a failed MP4 remux")
		}
	})
}

// I5 with the real ffmpeg: an MKV with H.264 + AAC becomes a remuxed
// stream.mp4 that is itself browser-ready.
func TestRealRemuxMKV(t *testing.T) {
	e := newEnv(t)
	ff := realMediaOrSkip(t, e)
	l := e.lecture("mkv", "in.mkv")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	genMedia(t, ff, filepath.Join(dir, "in.mkv"), "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-shortest",
		"-c:v", realH264OrSkip(t, e, ff), "-pix_fmt", "yuv420p", "-c:a", "aac")
	r := e.run(&prepStage{d: e.deps(nil), media: &execMedia{ffmpeg: ff}}, l.ID)
	if r.err != nil {
		t.Fatal(r.err)
	}
	st := e.streamState(l.ID)
	if free, err := diskFree(e.root); err == nil && free < StreamMinFreeBytes {
		if st.Status != StreamNoSpace {
			t.Errorf("state %+v, want no_space", st)
		}
		return
	}
	if st.Status != StreamReady || st.Transcoded || e.hasCurrentJob(l.ID, model.StageStream) {
		t.Fatalf("state %+v: want a remux in prep", st)
	}
	out, _, err := (&execMedia{ffmpeg: ff}).Probe(e.ctx, filepath.Join(dir, model.StreamFile))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := parseProbe(out); err != nil || !mp4Family(info.Format) || !browserReady(info) {
		t.Errorf("stream.mp4 %+v %v", info, err)
	}
}

// ---------- I9: truncated files ----------

func TestTruncated(t *testing.T) {
	cases := []struct {
		wav, declared float64
		want          bool
	}{
		{9.77, 20, true},      // x02b_trunc_faststart.mp4
		{18.5, 20, false},     // 7.5 % short
		{17.9, 20, true},      // 10.5 % short
		{0.5, 1.4, false},     // under a second short
		{3000, 3012.5, false}, // a lecture whose audio ends a little early
		{2700, 3012.5, true},
		{10, 0, false}, // duration unknown
	}
	for _, c := range cases {
		if got := truncated(c.wav, c.declared); got != c.want {
			t.Errorf("truncated(%v, %v) = %v", c.wav, c.declared, got)
		}
	}
	if clock(9.77) != "0:09" || clock(3012.5) != "50:12" || clock(3725) != "1:02:05" {
		t.Errorf("clock %s %s %s", clock(9.77), clock(3012.5), clock(3725))
	}
}

// writeWAV writes a 16 kHz mono 16-bit WAV of sec seconds of a square wave
// at about −20.8 dBFS (round 2 N06: it was silence, which prep now flags
// as prep.low_volume), with a LIST chunk before the data as ffmpeg writes
// it.
func writeWAV(t *testing.T, path string, sec float64) {
	t.Helper()
	writeLevelWAV(t, path, sec, 3000)
}

// writeLevelWAV is writeWAV with a square wave of ±amp (RMS amp, so
// 20·log10(amp/32768) dBFS; 0 = silence).
func writeLevelWAV(t *testing.T, path string, sec float64, amp int16) {
	t.Helper()
	writeSegmentsWAV(t, path, wavSegment{sec, amp})
}

// wavSegment is sec seconds of a square wave of ±amp.
type wavSegment struct {
	sec float64
	amp int16
}

// writeSegmentsWAV is writeWAV with the segments one after the other.
func writeSegmentsWAV(t *testing.T, path string, segs ...wavSegment) {
	t.Helper()
	var amps []int16
	for _, s := range segs {
		for range int(s.sec * 16000) {
			amps = append(amps, s.amp)
		}
	}
	n := len(amps)
	list := []byte("INFOISFT\x0e\x00\x00\x00Lavf62.3.100\x00\x00")
	var b []byte
	le32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	le16 := func(v uint16) { b = binary.LittleEndian.AppendUint16(b, v) }
	b = append(b, "RIFF"...)
	le32(uint32(4 + 8 + 16 + 8 + len(list) + 8 + 2*n))
	b = append(b, "WAVEfmt "...)
	le32(16)
	le16(1)
	le16(1)
	le32(16000)
	le32(32000)
	le16(2)
	le16(16)
	b = append(b, "LIST"...)
	le32(uint32(len(list)))
	b = append(b, list...)
	b = append(b, "data"...)
	le32(uint32(2 * n))
	for i, amp := range amps {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		le16(uint16(v))
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWAVSeconds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.wav")
	writeWAV(t, p, 2.5)
	if d, err := wavSeconds(p); err != nil || math.Abs(d-2.5) > 1e-9 {
		t.Errorf("wavSeconds = %v, %v", d, err)
	}
	if err := os.WriteFile(p, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wavSeconds(p); err == nil {
		t.Error("a file of zeros is no WAV")
	}
}

// wavMedia is fakeMedia whose audio.wav is a real WAV of wav seconds.
type wavMedia struct {
	*fakeMedia
	wav float64
	t   *testing.T
}

func (m wavMedia) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	stderr, err := m.fakeMedia.FFmpeg(ctx, dir, args, progress)
	if err == nil && strings.Contains(filepath.Base(args[len(args)-1]), model.AudioFile) {
		writeWAV(m.t, args[len(args)-1], m.wav)
	}
	return stderr, err
}

// I9: audio.wav much shorter than the container says → prep is done, with
// the warning prep.media_truncated; a complete file has none.
func TestPrepTruncatedWarning(t *testing.T) {
	probe := `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "20.0"}}`
	for _, c := range []struct {
		wav  float64
		warn bool
	}{{9.77, true}, {19.98, false}} {
		e := newEnv(t)
		l, _ := e.mediaLecture("L")
		r := e.run(&prepStage{d: e.deps(nil), media: wavMedia{&fakeMedia{probe: probe}, c.wav, t}}, l.ID)
		if r.err != nil || r.job.Status != model.StatusDone {
			t.Fatalf("prep %v %s", r.err, r.job.Status)
		}
		w := r.stats.WarningInfo
		if !c.warn {
			if w != nil || r.stats.Warning != "" {
				t.Errorf("complete file: warning %+v %q", w, r.stats.Warning)
			}
			continue
		}
		if w == nil || w.Code != WarnPrepMediaTruncated || w.Params["processed"] != "0:09" || w.Params["declared"] != "0:20" {
			t.Errorf("warning %+v", w)
		}
		if !strings.Contains(r.stats.Warning, "0:09") || !strings.Contains(r.stats.Warning, "0:20") {
			t.Errorf("warning text %q", r.stats.Warning)
		}
	}
}

// I9 with the real ffmpeg: a 20 s MKV cut in half still declares 20 s;
// prep transcribes the first part and says so.
func TestRealPrepTruncated(t *testing.T) {
	ff := guardFFmpeg(t)
	e := newEnv(t)
	l := e.lecture("trunc", "trunc.mka")
	dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
	media := filepath.Join(dir, "trunc.mka")
	genMedia(t, ff, media, "-f", "lavfi", "-i", "sine=frequency=440:duration=20", "-c:a", "flac")
	b, err := os.ReadFile(media)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.run(&prepStage{d: e.deps(nil), media: softwareTools{&execMedia{ffmpeg: ff}}}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("prep %v %s", r.err, r.job.Status)
	}
	if w := r.stats.WarningInfo; w == nil || w.Code != WarnPrepMediaTruncated || w.Params["declared"] != "0:20" {
		t.Errorf("warning %+v (%q)", w, r.stats.Warning)
	}
}

// I9 / review R5: the declared duration is only trusted from a container
// whose header or index declares it, and the audio stream's own duration
// comes before the file's (the longest stream's).
func TestParseProbeDeclaredDuration(t *testing.T) {
	cases := []struct {
		name, json string
		want       float64
	}{
		{"raw ADTS, estimated from the bitrate", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac", "duration": "188.47"}],
		 "format": {"format_name": "aac", "duration": "188.47"}}`, 0},
		{"MP3, estimated from the bitrate", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "mp3", "duration": "44.9"}],
		 "format": {"format_name": "mp3", "duration": "44.9"}}`, 0},
		{"MPEG-TS", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mpegts", "duration": "30"}}`, 0},
		{"MP4, video longer than the audio", `{"streams": [
		 {"index": 0, "codec_type": "video", "codec_name": "h264", "duration": "10.0"},
		 {"index": 1, "codec_type": "audio", "codec_name": "aac", "duration": "4.0"}],
		 "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "10.0"}}`, 4},
		{"Matroska, no stream duration", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "flac"}],
		 "format": {"format_name": "matroska,webm", "duration": "20.0"}}`, 20},
		{"WAV", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "pcm_s16le", "duration": "7.5"}],
		 "format": {"format_name": "wav", "duration": "7.5"}}`, 7.5},
		{"no duration", `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mov,mp4"}}`, 0},
	}
	for _, c := range cases {
		info, err := parseProbe([]byte(c.json))
		if err != nil || info.Duration != c.want {
			t.Errorf("%s: duration %v (%v), want %v", c.name, info.Duration, err, c.want)
		}
	}
}

// I9 / review R5 with the real ffmpeg: complete VBR audio whose duration
// ffprobe estimates from the bitrate (raw ADTS AAC; MP3 without a Xing
// header) declares far more than it holds — 10 s of silence, then 20 s of
// noise — and is no truncated file; a truncated MP4 is still reported.
func TestRealPrepTruncationTrustedDurationOnly(t *testing.T) {
	ff := guardFFmpeg(t)
	src := []string{"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=10", "-f", "lavfi", "-i", "anoisesrc=d=20:a=0.5:r=44100",
		"-filter_complex", "[0][1]concat=n=2:v=0:a=1"}
	encoders, err := runCombined(context.Background(), ff, []string{"-hide_banner", "-encoders"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, file string
		enc        []string
		cut        bool
		warn       bool
	}{
		{"raw ADTS AAC", "vbr.aac", []string{"-c:a", "aac", "-q:a", "0.5", "-f", "adts"}, false, false},
		{"MP3 without a Xing header", "vbr.mp3", []string{"-c:a", "libmp3lame", "-q:a", "9", "-write_xing", "0", "-id3v2_version", "0"}, false, false},
		{"truncated MP4", "cut.mp4", []string{"-c:a", "aac", "-movflags", "+faststart"}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.enc[1] == "libmp3lame" && !parseEncoderList(encoders)["libmp3lame"] {
				t.Skip("this ffmpeg has no MP3 encoder: the VBR MP3 input cannot be made (SPEC §14)")
			}
			e := newEnv(t)
			l := e.lecture("L", c.file)
			dir := model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID)
			media := filepath.Join(dir, c.file)
			genMedia(t, ff, media, append(append([]string{}, src...), c.enc...)...)
			if c.cut {
				b, err := os.ReadFile(media)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(media, b[:len(b)/2], 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				// the input is what the check must not trust: ffprobe declares
				// more than 10 % beyond its 30 s
				raw, _, err := (&execMedia{ffmpeg: ff}).Probe(e.ctx, media)
				if err != nil {
					t.Fatal(err)
				}
				var p probeJSON
				if err := json.Unmarshal(raw, &p); err != nil {
					t.Fatal(err)
				}
				if d, _ := strconv.ParseFloat(p.Format.Duration, 64); !truncated(30, d) {
					t.Skipf("ffprobe declares %v s for 30 s: this ffmpeg does not overestimate this input", d)
				}
			}
			r := e.run(&prepStage{d: e.deps(nil), media: softwareTools{&execMedia{ffmpeg: ff}}}, l.ID)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep %v %s", r.err, r.job.Status)
			}
			w := r.stats.WarningInfo
			if c.warn && (w == nil || w.Code != WarnPrepMediaTruncated || w.Params["declared"] != "0:30") {
				t.Errorf("truncated file: warning %+v (%q)", w, r.stats.Warning)
			}
			if !c.warn && (w != nil || r.stats.Warning != "") {
				t.Errorf("complete file: warning %+v (%q)", w, r.stats.Warning)
			}
		})
	}
}
