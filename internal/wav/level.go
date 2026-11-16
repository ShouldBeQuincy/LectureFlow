// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package wav

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
)

// ErrNoSamples: the WAV file has a data chunk but no samples in it.
var ErrNoSamples = errors.New("WAV file has no samples")

// levelBuf is the read size of Level: 256 KiB, about 8 s of 16 kHz mono
// 16-bit audio between two checks of ctx; memory use does not grow with the
// file.
const levelBuf = 256 << 10

// Speech level (round 2 N06, review R26 C-1). The level of the whole file
// is an energy mean: a few seconds of a loud noise (a cough, a door, a
// bell) pull quiet speech far up, so it misses the recordings it is meant
// to catch. Level instead measures the RMS of every LevelBlockSeconds block
// and takes the LevelPercentile-th percentile of them:
//
//   - LevelBlockSeconds = 1: about a word or two, long enough for a stable
//     RMS of speech and short enough that a short noise stays in one or two
//     blocks; one value per second is 36 000 for 10 hours (a few hundred
//     KiB), so the blocks are kept and sorted rather than binned.
//   - LevelPercentile = 90: as long as speech covers at least 10% of the
//     blocks that are not digital silence, the 90th percentile lies among
//     the speech blocks, so long pauses do not pull it down; noises louder
//     than the speech covering less than 10% do not pull it up.
//   - Blocks below DigitalSilenceDBFS (−90 dBFS: an RMS under about one
//     sample step, so zeros or ±1 dither) are left out: zeros from padding (prep's
//     aresample fills gaps with them) or a muted input are no part of the
//     recording's level. Room noise is far above it and still counts.
const (
	LevelBlockSeconds  = 1
	LevelPercentile    = 90
	DigitalSilenceDBFS = -90.0
)

// Level is the speech level of the 16-bit PCM WAV file at path in dBFS:
// the LevelPercentile-th percentile (nearest rank) of the RMS levels of its
// LevelBlockSeconds blocks, blocks below DigitalSilenceDBFS left out. RMS
// is 20·log10(rms / 32768) over all channels' samples, so a full-scale
// square wave is 0 dBFS and a full-scale sine about −3 dBFS.
//
// The last block, when shorter than a full one, counts as a block of its
// own from half a block on and is added to the block before it otherwise; a
// file shorter than one block is one block. A file in which every block is
// digital silence (all zeros included) is -Inf.
//
// The file is read once, sequentially, in fixed-size blocks, and never
// written; ctx is checked between reads. A file that is not a 16-bit PCM
// WAV is ErrNotPCM, one without samples ErrNoSamples.
func Level(ctx context.Context, path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	h, err := readPCMHeader(f, fi.Size())
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if bits := binary.LittleEndian.Uint16(h.fmt[14:16]); bits != 16 {
		return 0, fmt.Errorf("%s: %w: %d bits per sample", path, ErrNotPCM, bits)
	}
	n := h.dataLen - h.dataLen%2
	if n <= 0 {
		return 0, fmt.Errorf("%s: %w", path, ErrNoSamples)
	}
	if _, err := f.Seek(h.dataOff, io.SeekStart); err != nil {
		return 0, err
	}
	// Samples per block: one second of every channel (byte rate / 2).
	per := int64(h.byteRate) / 2 * LevelBlockSeconds
	per = max(per, 1)

	var (
		blocks    []float64 // dB of each finished block
		lastSum   float64   // the last finished block, for a short tail
		lastCount int64
		sum       float64 // the block being read
		count     int64
		total     int64 // samples read
		r         = io.LimitReader(f, n)
		buf       = make([]byte, levelBuf)
	)
	finish := func() {
		blocks = append(blocks, blockDB(sum, count))
		lastSum, lastCount = sum, count
		sum, count = 0, 0
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		k, err := io.ReadFull(r, buf)
		k -= k % 2
		for i := 0; i < k; {
			// The samples of buf up to the end of the current block.
			m := min(int64(k-i)/2, per-count)
			var acc uint64 // at most levelBuf/2 squares of at most 2^30: no overflow
			seg := buf[i : i+int(m)*2]
			for j := 0; j+1 < len(seg); j += 2 {
				v := int64(int16(uint16(seg[j]) | uint16(seg[j+1])<<8))
				acc += uint64(v * v)
			}
			sum += float64(acc)
			count += m
			i += int(m) * 2
			if count == per {
				finish()
			}
		}
		total += int64(k / 2)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("%s: %w", path, ErrNoSamples)
	}
	switch {
	case count == 0:
	case len(blocks) == 0 || count*2 >= per:
		finish() // the whole (short) file, or a tail of half a block or more
	default:
		blocks[len(blocks)-1] = blockDB(lastSum+sum, lastCount+count) // a short tail joins the block before
	}
	return speechLevel(blocks), nil
}

// blockDB is the RMS level in dBFS of count samples whose squares add up to
// sum; -Inf for silence.
func blockDB(sum float64, count int64) float64 {
	if sum == 0 || count == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(math.Sqrt(sum/float64(count))/32768)
}

// speechLevel is the LevelPercentile-th percentile (nearest rank) of the
// block levels at or above DigitalSilenceDBFS; -Inf when there are none.
func speechLevel(blocks []float64) float64 {
	var sound []float64
	for _, b := range blocks {
		if b >= DigitalSilenceDBFS {
			sound = append(sound, b)
		}
	}
	if len(sound) == 0 {
		return math.Inf(-1)
	}
	slices.Sort(sound)
	rank := (LevelPercentile*len(sound) + 99) / 100 // ceil(p/100 · n), 1-based
	return sound[max(rank, 1)-1]
}
