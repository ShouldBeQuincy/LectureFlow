// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package wav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// squareWAV is a 16 kHz 16-bit WAV (with a LIST chunk) of frames frames of
// a square wave ±amp: RMS amp.
func squareWAV(frames int, channels uint16, amp int16) []byte {
	b, data := pcmFile(formatPCM, 16000, channels, 16, frames, true)
	samples := data[:0]
	for i := range len(data) / 2 {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		samples = binary.LittleEndian.AppendUint16(samples, uint16(v))
	}
	off := len(b) - len(data)
	copy(b[off:], samples)
	return b
}

func TestLevel(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		b        []byte
		want     float64
		tolerate float64
	}{
		{"full scale", squareWAV(16000, 1, 32767), 0, 0.001},
		{"-20.8 dBFS", squareWAV(16000, 1, 3000), 20 * math.Log10(3000.0/32768), 1e-9},
		{"103", squareWAV(16000, 1, 103), 20 * math.Log10(103.0/32768), 1e-9},
		{"stereo", squareWAV(16000, 2, 1000), 20 * math.Log10(1000.0/32768), 1e-9},
		{"several blocks", squareWAV(levelBuf, 1, 500), 20 * math.Log10(500.0/32768), 1e-9},
	} {
		p := writeTemp(t, "a.wav", c.b)
		before := sha256.Sum256(c.b)
		got, err := Level(ctx, p)
		if err != nil || math.Abs(got-c.want) > c.tolerate {
			t.Errorf("%s: Level = %v, %v; want %v", c.name, got, err, c.want)
		}
		after, _ := os.ReadFile(p)
		if sha256.Sum256(after) != before {
			t.Errorf("%s: Level changed the file", c.name)
		}
	}
	if got, err := Level(ctx, writeTemp(t, "s.wav", squareWAV(8000, 1, 0))); err != nil || !math.IsInf(got, -1) {
		t.Errorf("silence: %v, %v; want -Inf", got, err)
	}
}

// segWAV is a 16 kHz mono 16-bit WAV of the segments one after the other,
// each n samples of a square wave ±amp.
func segWAV(segs ...[2]int) []byte {
	frames := 0
	for _, s := range segs {
		frames += s[0]
	}
	b, data := pcmFile(formatPCM, 16000, 1, 16, frames, true)
	samples := data[:0]
	for _, s := range segs {
		for i := range s[0] {
			v := int16(s[1])
			if i%2 == 1 {
				v = -v
			}
			samples = binary.LittleEndian.AppendUint16(samples, uint16(v))
		}
	}
	copy(b[len(b)-len(data):], samples)
	return b
}

func dbfs(amp float64) float64 { return 20 * math.Log10(amp/32768) }

// Review R26 C-1: the level is the 90th percentile of the 1-second blocks.
func TestLevelSpeech(t *testing.T) {
	const s = 16000 // samples per second
	// rms of a block holding a samples of ±x and b samples of ±y
	mix := func(a, x, b, y float64) float64 { return dbfs(math.Sqrt((a*x*x + b*y*y) / (a + b))) }
	for _, c := range []struct {
		name string
		b    []byte
		want float64
	}{
		{"90% digital silence, 10% speech at 1000", segWAV([2]int{90 * s, 0}, [2]int{10 * s, 1000}), dbfs(1000)},
		{"99% at 58, 1% at 3277", segWAV([2]int{99 * s, 58}, [2]int{1 * s, 3277}), dbfs(58)},
		{"85% room noise at 5, 15% speech at 1000", segWAV([2]int{85 * s, 5}, [2]int{15 * s, 1000}), dbfs(1000)},
		{"95% room noise at 5, 5% speech: the noise", segWAV([2]int{95 * s, 5}, [2]int{5 * s, 1000}), dbfs(5)},
		{"±1 dither (−90.3 dBFS) is digital silence", segWAV([2]int{10 * s, 1}), math.Inf(-1)},
		{"±2 (−84.3 dBFS) is not", segWAV([2]int{10 * s, 2}), dbfs(2)},
		// tails: 2 s at 100, then a short loud tail
		{"tail of 0.3 s joins the last block", segWAV([2]int{2 * s, 100}, [2]int{3 * s / 10, 1000}), mix(1, 100, 0.3, 1000)},
		{"tail of 0.5 s is a block", segWAV([2]int{2 * s, 100}, [2]int{s / 2, 1000}), dbfs(1000)},
		{"file of 0.3 s is one block", segWAV([2]int{3 * s / 10, 300}), dbfs(300)},
		{"file of 0.2 s then 0.1 s", segWAV([2]int{2 * s / 10, 300}, [2]int{s / 10, 600}), mix(0.2, 300, 0.1, 600)},
		{"silence with one block of sound", segWAV([2]int{30 * s, 0}, [2]int{s, 200}), dbfs(200)},
	} {
		got, err := Level(context.Background(), writeTemp(t, "a.wav", c.b))
		if err != nil || math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: Level = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	// nearest rank: of 10 blocks at 1..10 the 90th percentile is the 9th
	var segs [][2]int
	for a := 1; a <= 10; a++ {
		segs = append(segs, [2]int{s, a * 100})
	}
	if got, _ := Level(context.Background(), writeTemp(t, "r.wav", segWAV(segs...))); math.Abs(got-dbfs(900)) > 1e-6 {
		t.Errorf("rank: %v, want %v", got, dbfs(900))
	}
}

func TestLevelErrors(t *testing.T) {
	ctx := context.Background()
	b8, _ := pcmFile(formatPCM, 16000, 1, 8, 100, false)
	float, _ := pcmFile(3, 16000, 1, 32, 100, false)
	empty, _ := pcmFile(formatPCM, 16000, 1, 16, 0, false)
	for _, c := range []struct {
		name string
		b    []byte
		want error
	}{
		{"empty file", nil, ErrNotPCM},
		{"zeros", make([]byte, 1000), ErrNotPCM},
		{"8 bit", b8, ErrNotPCM},
		{"float", float, ErrNotPCM},
		{"no samples", empty, ErrNoSamples},
		{"header cut short", squareWAV(100, 1, 1000)[:30], ErrNotPCM},
	} {
		if _, err := Level(ctx, writeTemp(t, "e.wav", c.b)); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := Level(ctx, filepath.Join(t.TempDir(), "missing.wav")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
}

// countCtx is a context whose Err turns to Canceled after n calls.
type countCtx struct {
	context.Context
	n int
}

func (c *countCtx) Err() error {
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	return nil
}

// A cancel in the middle of a long file stops the read at the next block.
func TestLevelCancel(t *testing.T) {
	p := writeTemp(t, "long.wav", squareWAV(4*levelBuf, 1, 1000)) // 8 blocks
	ctx := &countCtx{Context: context.Background(), n: 2}
	if _, err := Level(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("Level = %v, want Canceled", err)
	}
	if ctx.n != 0 {
		t.Errorf("ctx checked %d times fewer than expected", ctx.n)
	}
}

// BenchmarkLevelHour: an hour of 16 kHz mono 16-bit audio (115 MB).
func BenchmarkLevelHour(b *testing.B) {
	frames := 3600 * 16000
	data := bytes.Repeat([]byte{0xE8, 0x03, 0x18, 0xFC}, frames/2) // ±1000
	hdr, _ := pcmFile(formatPCM, 16000, 1, 16, 0, false)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(len(hdr)-8+len(data)))
	binary.LittleEndian.PutUint32(hdr[len(hdr)-4:], uint32(len(data)))
	p := filepath.Join(b.TempDir(), "hour.wav")
	f, err := os.Create(p)
	if err != nil {
		b.Fatal(err)
	}
	f.Write(hdr)
	f.Write(data)
	f.Close()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		if _, err := Level(context.Background(), p); err != nil {
			b.Fatal(err)
		}
	}
}
