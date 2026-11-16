// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/model"
)

// test-media I2: audio.wav keeps the media's timeline. A WAV has no
// timestamps, so an audio track that starts after the file, or one with a
// hole, used to be closed up and the sentences after it came early against
// the playback file.

// The filter is an output option of audio.wav (after the input's -i).
func TestAudioArgsKeepTimeline(t *testing.T) {
	a := audioArgs("in.mp4", "out.tmp", nil)
	in, af := slices.Index(a, "-i"), slices.Index(a, "-af")
	if in < 0 || af < in+2 || a[af+1] != "aresample=async=1:first_pts=0" || a[len(a)-1] != "out.tmp" {
		t.Fatalf("audio.wav args %v", a)
	}
	for _, w := range [][2]string{{"-ac", "1"}, {"-ar", "16000"}, {"-c:a", "pcm_s16le"}, {"-f", "wav"}} {
		if i := slices.Index(a, w[0]); i < af || a[i+1] != w[1] {
			t.Errorf("%s %s missing or before the filter: %v", w[0], w[1], a)
		}
	}
}

// With the real ffmpeg: audio that starts 1 s after the picture, and a
// 1 s hole in the audio, keep their place; continuous audio comes out
// sample for sample as without the filter.
func TestRealAudioTimeline(t *testing.T) {
	ff := guardFFmpeg(t)
	tools := softwareTools{&execMedia{ffmpeg: ff}}
	ctx := context.Background()
	src := t.TempDir()
	const tone = "sine=frequency=440:duration=4"
	video := func(sec string) []string {
		return []string{"-f", "lavfi", "-i", "testsrc=d=" + sec + ":s=160x120:r=10"}
	}
	cases := []struct {
		name   string
		args   []string
		dur    float64      // seconds of audio.wav
		silent [][2]float64 // stretches (s) that must be silence
		loud   [][2]float64 // and some that must not
	}{
		{"continuous", append(video("4"), "-f", "lavfi", "-i", tone, "-c:v", "mpeg4", "-c:a", "aac", "-shortest"),
			4, nil, [][2]float64{{0.1, 3.9}}},
		{"audio late", append(video("5"), "-itsoffset", "1", "-f", "lavfi", "-i", tone, "-map", "0:v", "-map", "1:a", "-c:v", "mpeg4", "-c:a", "aac"),
			5, [][2]float64{{0, 0.9}}, [][2]float64{{1.1, 4.9}}},
		{"hole", []string{"-f", "lavfi", "-i", tone, "-af", "asetpts='PTS+gte(T,2)/TB'", "-c:a", "aac"},
			5, [][2]float64{{2.1, 2.9}}, [][2]float64{{0.1, 1.9}, {3.1, 4.9}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			media := filepath.Join(src, c.name+".mkv")
			genMedia(t, ff, media, c.args...)
			dir := t.TempDir()
			ps := &prepStage{}
			if err := ps.convertAudio(ctx, tools, model.Lecture{MediaPath: media}, dir, media, nil, func(float64) {}); err != nil {
				t.Fatal(err)
			}
			got := readPCM16(t, filepath.Join(dir, model.AudioFile))
			if d := float64(len(got)) / 16000; math.Abs(d-c.dur) > 0.06 {
				t.Errorf("audio.wav %.3f s, want %.1f s", d, c.dur)
			}
			for _, s := range c.silent {
				if r := rms(got, s); r > 30 {
					t.Errorf("%.1f–%.1f s: rms %.0f, want silence", s[0], s[1], r)
				}
			}
			for _, s := range c.loud {
				if r := rms(got, s); r < 1000 {
					t.Errorf("%.1f–%.1f s: rms %.0f, want the tone", s[0], s[1], r)
				}
			}
			if c.name != "continuous" {
				return
			}
			// the same samples as the conversion without the filter
			plain := filepath.Join(dir, "plain.wav")
			args := audioArgs(media, plain, nil)
			i := slices.Index(args, "-af")
			args = slices.Delete(args, i, i+2)
			if out, err := tools.FFmpeg(ctx, dir, args, func(float64) {}); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if want := readPCM16(t, plain); !slices.Equal(got, want) {
				t.Errorf("continuous audio changed by the filter: %d samples, %d without", len(got), len(want))
			}
		})
	}
}

// readPCM16 returns the samples of a 16 kHz mono 16-bit WAV.
func readPCM16(t *testing.T, path string) []int16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("%s: not a WAV", path)
	}
	for p := 12; p+8 <= len(b); {
		id, n := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		p += 8
		switch id {
		case "fmt ":
			if ch, rate, bits := binary.LittleEndian.Uint16(b[p+2:]), binary.LittleEndian.Uint32(b[p+4:]), binary.LittleEndian.Uint16(b[p+14:]); ch != 1 || rate != 16000 || bits != 16 {
				t.Fatalf("%s: %d ch %d Hz %d bit", path, ch, rate, bits)
			}
		case "data":
			n = min(n, len(b)-p)
			out := make([]int16, n/2)
			if err := binary.Read(bytes.NewReader(b[p:p+n/2*2]), binary.LittleEndian, out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		p += n + n%2
	}
	t.Fatalf("%s: no data chunk", path)
	return nil
}

// rms of the samples in the stretch s (seconds).
func rms(x []int16, s [2]float64) float64 {
	a, b := min(int(s[0]*16000), len(x)), min(int(s[1]*16000), len(x))
	if b <= a {
		return 0
	}
	var sum float64
	for _, v := range x[a:b] {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(b-a))
}
