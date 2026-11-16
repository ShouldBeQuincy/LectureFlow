// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
)

// probeAudio5s: an audio-only MP4 declaring 5 s (not truncated against a
// 5 s audio.wav).
const probeAudio5s = `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "5.0"}}`

// levelMedia is fakeMedia whose audio.wav is a square wave of ±amp for sec
// seconds (writeLevelWAV), or the bytes raw when set.
type levelMedia struct {
	*fakeMedia
	sec float64
	amp int16
	raw []byte
	t   *testing.T
}

func (m levelMedia) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	stderr, err := m.fakeMedia.FFmpeg(ctx, dir, args, progress)
	out := args[len(args)-1]
	if err == nil && strings.Contains(filepath.Base(out), model.AudioFile) {
		if m.raw != nil {
			if werr := os.WriteFile(out, m.raw, 0o644); werr != nil {
				m.t.Fatal(werr)
			}
		} else {
			writeLevelWAV(m.t, out, m.sec, m.amp)
		}
	}
	return stderr, err
}

// N06: the threshold is strict and silence counts as too quiet.
func TestLowVolumeThreshold(t *testing.T) {
	if lowVolumeDBFS != -50 {
		t.Errorf("lowVolumeDBFS = %v", lowVolumeDBFS)
	}
	for _, c := range []struct {
		level float64
		want  bool
	}{{-30, false}, {-50, false}, {-49.999, false}, {-50.001, true}, {-80, true}, {math.Inf(-1), true}} {
		if got := lowVolume(c.level); got != c.want {
			t.Errorf("lowVolume(%v) = %v", c.level, got)
		}
	}
}

// N06: prep measures audio.wav and warns below −50 dBFS; normal audio, a
// level just above the threshold and an unreadable WAV have no warning, and
// prep is done in every case.
func TestPrepLowVolumeWarning(t *testing.T) {
	cases := []struct {
		name string
		amp  int16 // RMS = amp: 104 ≈ −49.96 dBFS, 103 ≈ −50.04 dBFS
		raw  []byte
		warn bool
	}{
		{"normal", 3000, nil, false},
		{"just above the threshold", 104, nil, false},
		{"just below the threshold", 103, nil, true},
		{"very quiet", 10, nil, true},
		{"silence", 0, nil, true},
		{"header damaged", 0, append([]byte("RIFX"), make([]byte, 2000)...), false},
		{"no data chunk", 0, append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00"), make([]byte, 2000)...), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l, _ := e.mediaLecture("L")
			r := e.run(&prepStage{d: e.deps(nil), media: levelMedia{&fakeMedia{probe: probeAudio5s}, 5, c.amp, c.raw, t}}, l.ID)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep %v %s", r.err, r.job.Status)
			}
			w := r.stats.WarningInfo
			if !c.warn {
				if w != nil || r.stats.Warning != "" {
					t.Errorf("warning %+v %q", w, r.stats.Warning)
				}
				return
			}
			if infoCode(w) != WarnPrepLowVolume || len(extraCodes(w)) != 0 || r.stats.Warning != LowVolumeWarning {
				t.Errorf("warning %+v %q", w, r.stats.Warning)
			}
		})
	}
}

// N06 review C-1: the level is the speech level (90th percentile of the
// 1-second blocks), not the whole-file mean. Long silence or room noise
// around normal speech is not warned; quiet speech with a few short loud
// noises (a cough, a door; the mean of these is about −26 dBFS) is.
func TestPrepLowVolumeSpeechLevel(t *testing.T) {
	burst := func(quiet float64) []wavSegment {
		var s []wavSegment
		for range 3 {
			s = append(s, wavSegment{quiet, 10}, wavSegment{0.5, 10000})
		}
		return s
	}
	cases := []struct {
		name string
		segs []wavSegment
		warn bool
	}{
		{"90 s of digital silence, 6 s of speech at −30 dBFS", []wavSegment{{90, 0}, {6, 1000}}, false},
		{"50 s of room noise at −76 dBFS, 10 s of speech at −30 dBFS", []wavSegment{{50, 5}, {10, 1000}}, false},
		{"speech at −70 dBFS with three short loud noises", burst(19.5), true},
		{"speech at −70 dBFS throughout", []wavSegment{{60, 10}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "a.wav")
			writeSegmentsWAV(t, src, c.segs...)
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			e := newEnv(t)
			l, _ := e.mediaLecture("L")
			r := e.run(&prepStage{d: e.deps(nil), media: levelMedia{&fakeMedia{probe: probeAudio5s}, 0, 0, raw, t}}, l.ID)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("prep %v %s", r.err, r.job.Status)
			}
			if got := infoCode(r.stats.WarningInfo) == WarnPrepLowVolume; got != c.warn {
				t.Errorf("warned %v, want %v (%+v %q)", got, c.warn, r.stats.WarningInfo, r.stats.Warning)
			}
		})
	}
}

// N06: an empty audio.wav is no audio (media.no_audio, as before), not a
// low-volume warning.
func TestPrepLowVolumeEmptyWAV(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	r := e.run(&prepStage{d: e.deps(nil), media: levelMedia{&fakeMedia{probe: probeAudio5s}, 0, 0, []byte{}, t}}, l.ID)
	if r.err == nil || codeOf(r.err) != MediaNoAudio {
		t.Fatalf("empty audio.wav: %v", r.err)
	}
	if r.stats.WarningInfo != nil {
		t.Errorf("warning %+v", r.stats.WarningInfo)
	}
}

// N06: with a truncated file both warnings are kept: truncation is the main
// part, the low volume follows in params.extra and after "；" in the text.
func TestPrepLowVolumeWithTruncation(t *testing.T) {
	probe := `{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}], "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "20.0"}}`
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	r := e.run(&prepStage{d: e.deps(nil), media: levelMedia{&fakeMedia{probe: probe}, 9.77, 0, nil, t}}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone {
		t.Fatalf("prep %v %s", r.err, r.job.Status)
	}
	w := r.stats.WarningInfo
	if infoCode(w) != WarnPrepMediaTruncated || w.Params["declared"] != "0:20" || !slices.Equal(extraCodes(w), []string{WarnPrepLowVolume}) {
		t.Errorf("warning %+v", w)
	}
	if !strings.HasPrefix(r.stats.Warning, "媒体文件可能不完整") || !strings.HasSuffix(r.stats.Warning, "；"+LowVolumeWarning) {
		t.Errorf("warning text %q", r.stats.Warning)
	}
}

// N06: the measurement only reads audio.wav. The file prep leaves is byte
// for byte what ffmpeg wrote, and asr's input_hash is the hash of those
// bytes — what it was before the measurement existed — so no transcribed
// lecture becomes stale.
func TestPrepLowVolumeKeepsHashes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "quiet.wav")
	writeLevelWAV(t, src, 5, 20) // ≈ −64 dBFS: warned
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	wantWAV := hex.EncodeToString(sum[:])

	e := newEnv(t)
	e.cfg.ASR.FakeResult = fakeASR
	l, dir := e.mediaLecture("L")
	r := e.run(&prepStage{d: e.deps(nil), media: levelMedia{&fakeMedia{probe: probeAudio5s}, 0, 0, raw, t}}, l.ID)
	if r.err != nil || r.job.Status != model.StatusDone || infoCode(r.stats.WarningInfo) != WarnPrepLowVolume {
		t.Fatalf("prep %v %s %+v", r.err, r.job.Status, r.stats.WarningInfo)
	}
	got, err := os.ReadFile(filepath.Join(dir, model.AudioFile))
	if err != nil {
		t.Fatal(err)
	}
	if s := sha256.Sum256(got); hex.EncodeToString(s[:]) != wantWAV {
		t.Fatal("prep changed audio.wav")
	}

	d := e.deps(nil)
	stg := e.stage(d, model.StageASR)
	ra := e.run(stg, l.ID)
	if ra.err != nil || ra.job.Status != model.StatusDone {
		t.Fatalf("asr %v %s", ra.err, ra.job.Status)
	}
	snap, ok := model.ParseASRSnapshot(ra.job.ConfigSnapshot)
	if !ok {
		t.Fatalf("snapshot %q", ra.job.ConfigSnapshot)
	}
	want := ctxbuild.ASRInputHashV3(wantWAV, snap.Language)
	if ra.job.InputHash == nil || *ra.job.InputHash != want {
		t.Errorf("asr input_hash %v, want %s", ra.job.InputHash, want)
	}
	if h, _ := stg.InputHash(e.ctx, l.ID); h != want {
		t.Errorf("InputHash %q, want %q", h, want)
	}
}

// N06: a cancel as the conversion ends stops prep without a warning (the
// cancel in the middle of a long measurement is wav.TestLevelCancel).
func TestPrepLowVolumeCancel(t *testing.T) {
	e := newEnv(t)
	l, _ := e.mediaLecture("L")
	ctx, cancel := context.WithCancel(e.ctx)
	m := cancelAfterAudio{levelMedia{&fakeMedia{probe: probeAudio5s}, 5, 10, nil, t}, cancel}
	r := e.runCtx(ctx, &prepStage{d: e.deps(nil), media: m}, l.ID)
	if r.err == nil || r.job.Status == model.StatusDone {
		t.Fatalf("cancelled prep: %v %s", r.err, r.job.Status)
	}
	if r.stats.WarningInfo != nil {
		t.Errorf("warning %+v", r.stats.WarningInfo)
	}
}

// cancelAfterAudio writes audio.wav and cancels the run as the conversion
// ends, still reporting success.
type cancelAfterAudio struct {
	levelMedia
	cancel context.CancelFunc
}

func (m cancelAfterAudio) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	stderr, err := m.levelMedia.FFmpeg(ctx, dir, args, progress)
	if strings.Contains(filepath.Base(args[len(args)-1]), model.AudioFile) {
		m.cancel()
	}
	return stderr, err
}
