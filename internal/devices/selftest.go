// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package devices

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/wav"
)

// Self-test helpers (SPEC-v2 I9; lead ruling 7). FROZEN signatures;
// bodies: task D (with wer.go). The run itself is stages.Deps.DeviceTest,
// scheduled by sched.Scheduler.StartDeviceTest.

// SelfTestClip is the clip length the self-test takes (I9: "中间 2 分钟").
const SelfTestClip = 2 * time.Minute

// SelfTestCompileLimit is how long one candidate may load (compile) before
// it fails with selftest.compile_timeout (lead ruling 3). Pipeline jobs
// have no such limit.
const SelfTestCompileLimit = 15 * time.Minute

// ClipSource is the lecture a self-test takes its clip from.
type ClipSource struct {
	LectureID string
	CourseID  string
	Title     string
	Audio     string // absolute path of its audio.wav (16 kHz mono PCM, prep's output)
}

// FindClipSource picks the lecture (I9, lead ruling 7: the user's own
// audio, no bundled clip yet): a lecture whose current repair job is done
// and whose audio.wav exists, the most recently finished repair first (ties
// by lecture id). ok is false when there is none: the self-test is not
// available ("先完成一节讲座的转写与修复", stages.CodeSelfTestNoAudio).
// Cheap enough for every GET /api/asr/devices (no sentences are read).
//
// The audio must also be longer than SelfTestClip (read from the WAV
// header; task D): a shorter lecture cannot give a clip of "中间 2 分钟",
// so it is skipped rather than making the test fail.
func FindClipSource(ctx context.Context, st db.Store, dataDir string) (src ClipSource, ok bool, err error) {
	cs, err := clipCandidates(ctx, st)
	if err != nil {
		return ClipSource{}, false, err
	}
	for _, c := range cs {
		l, err := st.GetLecture(ctx, c.ScopeID)
		if err != nil {
			continue // deleted meanwhile
		}
		audio, err := filepath.Abs(filepath.Join(model.LectureDir(dataDir, l.CourseID, l.ID), model.AudioFile))
		if err != nil {
			continue
		}
		if fi, err := os.Stat(audio); err != nil || fi.IsDir() {
			continue
		}
		if ms, ok := wav.DurationMS(audio); !ok || ms <= SelfTestClip.Milliseconds() {
			continue
		}
		return ClipSource{LectureID: l.ID, CourseID: l.CourseID, Title: l.Title, Audio: audio}, true, nil
	}
	return ClipSource{}, false, nil
}

// clipCandidates returns the lectures whose current repair job is done, in
// the order FindClipSource tries them: the most recently finished first
// (no finish time counts as ""), ties by lecture id. The SQLite store reads
// just those rows (db.JobReader, r7 perf: about 0.5 ms instead of 15 ms on
// a 400-lecture library); another store is read in full, with the same
// result.
func clipCandidates(ctx context.Context, st db.Store) ([]db.FinishedJob, error) {
	if jr, ok := st.(db.JobReader); ok {
		return jr.DoneLectureJobsByFinish(ctx, model.StageRepair)
	}
	jobs, err := st.ListCurrentJobsByStatus(ctx, model.StatusDone)
	if err != nil {
		return nil, err
	}
	var cs []db.FinishedJob
	for _, j := range jobs {
		if j.Scope != model.ScopeLecture || j.Stage != model.StageRepair {
			continue
		}
		f := ""
		if j.FinishedAt != nil {
			f = *j.FinishedAt
		}
		cs = append(cs, db.FinishedJob{ScopeID: j.ScopeID, FinishedAt: f})
	}
	sort.SliceStable(cs, func(a, b int) bool {
		if cs[a].FinishedAt != cs[b].FinishedAt {
			return cs[a].FinishedAt > cs[b].FinishedAt
		}
		return cs[a].ScopeID < cs[b].ScopeID
	})
	return cs, nil
}

// Clip planning (PlanClip).
const (
	// clipMinPauseMS: a gap between two sentences of at least this length
	// is a pause; other sentence boundaries are used only when no pause
	// lies near the wanted cut.
	clipMinPauseMS = 300
	// clipPadMS: a cut keeps at most this much of the pause next to the
	// clip's first / last sentence (half the pause when it is shorter), so
	// a long silence does not pad the clip.
	clipPadMS = 1000
)

// PlanClip chooses the clip inside a lecture of sentences ss (ordered by
// time): about target long, centred on the middle of the lecture, starting
// and ending in a pause between two sentences so that no sentence crosses
// a boundary (the 3a method). ref is the repaired text (sentence.text) of
// the sentences inside, joined with spaces; ok is false when the lecture is
// shorter than target or has no usable pause.
//
// Details (task D): the lecture spans 0 … the latest sentence end; the
// start cut is the sentence boundary nearest to middle − target/2 and the
// end cut the one nearest to start + target, both preferring pauses of
// clipMinPauseMS or more and accepting a clip of 3/4 to 5/4 of target.
func PlanClip(ss []model.Sentence, target time.Duration) (startMS, endMS int64, ref string, ok bool) {
	n := len(ss)
	tgt := target.Milliseconds()
	if n == 0 || tgt <= 0 {
		return 0, 0, "", false
	}
	var total int64
	for _, s := range ss {
		total = max(total, s.EndMS, s.StartMS)
	}
	if total < tgt {
		return 0, 0, "", false
	}
	// endUpTo[i]: the latest end of ss[0..i]; startFrom[i]: the earliest
	// start of ss[i..]. The boundary between i-1 and i is the gap
	// startFrom[i] − endUpTo[i-1]; overlapping sentences (negative gap)
	// have no boundary between them.
	endUpTo := make([]int64, n)
	startFrom := make([]int64, n)
	for i, s := range ss {
		endUpTo[i] = max(s.EndMS, s.StartMS)
		if i > 0 {
			endUpTo[i] = max(endUpTo[i], endUpTo[i-1])
		}
	}
	for i := n - 1; i >= 0; i-- {
		startFrom[i] = ss[i].StartMS
		if i < n-1 {
			startFrom[i] = min(startFrom[i], startFrom[i+1])
		}
	}
	type cut struct {
		at  int64 // ms
		i   int   // first (start cut) or last (end cut) sentence inside
		gap int64
	}
	var starts, ends []cut
	for i := range ss {
		prevEnd := int64(0) // the lecture starts at 0
		if i > 0 {
			prevEnd = endUpTo[i-1]
		}
		if gap := startFrom[i] - prevEnd; gap >= 0 {
			starts = append(starts, cut{startFrom[i] - min(gap/2, clipPadMS), i, gap})
		}
		if i == n-1 { // the lecture ends here
			ends = append(ends, cut{endUpTo[i], i, clipMinPauseMS})
		} else if gap := startFrom[i+1] - endUpTo[i]; gap >= 0 {
			ends = append(ends, cut{endUpTo[i] + min(gap/2, clipPadMS), i, gap})
		}
	}
	// nearest picks the cut nearest to want inside [lo, hi] (pauses first;
	// equal distance: the longer gap, then the earlier cut).
	nearest := func(cs []cut, want, lo, hi int64, ok func(cut) bool) (cut, bool) {
		for _, minGap := range []int64{clipMinPauseMS, 0} {
			best, found := cut{}, false
			for _, c := range cs {
				if c.gap < minGap || c.at < lo || c.at > hi || !ok(c) {
					continue
				}
				d, bd := abs64(c.at-want), abs64(best.at-want)
				if !found || d < bd || (d == bd && c.gap > best.gap) {
					best, found = c, true
				}
			}
			if found {
				return best, true
			}
		}
		return cut{}, false
	}
	wantStart := total/2 - tgt/2
	sc, found := nearest(starts, wantStart, max(0, wantStart-tgt/4), wantStart+tgt/4, func(cut) bool { return true })
	if !found {
		return 0, 0, "", false
	}
	ec, found := nearest(ends, sc.at+tgt, sc.at+tgt*3/4, sc.at+tgt*5/4, func(c cut) bool { return c.i >= sc.i })
	if !found {
		return 0, 0, "", false
	}
	var parts []string
	for _, s := range ss[sc.i : ec.i+1] {
		if t := strings.Join(strings.Fields(s.Text), " "); t != "" {
			parts = append(parts, t)
		}
	}
	return sc.at, ec.at, strings.Join(parts, " "), true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Pick chooses the recommended result (I9, the I5 rule): among OK results
// with RTF <= model.PickMaxRTF the lowest WER (equal WER: the lower RTF;
// results without WER, i.e. no reference: the first in candidate order),
// rule model.PickRuleBest; when no OK result reaches the RTF limit, the
// lowest RTF, rule model.PickRuleLowestRTF. ok is false without any OK
// result.
func Pick(rs []model.BenchmarkResult) (model.BenchmarkPick, bool) {
	best := -1
	for i, r := range rs {
		if !r.OK || r.RTF > model.PickMaxRTF {
			continue
		}
		if best < 0 || betterWER(r, rs[best]) {
			best = i
		}
	}
	if best >= 0 {
		return model.BenchmarkPick{Model: rs[best].Model, Precision: rs[best].Precision, Rule: model.PickRuleBest}, true
	}
	for i, r := range rs {
		if r.OK && (best < 0 || r.RTF < rs[best].RTF) {
			best = i
		}
	}
	if best < 0 {
		return model.BenchmarkPick{}, false
	}
	return model.BenchmarkPick{Model: rs[best].Model, Precision: rs[best].Precision, Rule: model.PickRuleLowestRTF}, true
}

// Tie rule of PickFor (power management freeze, lead addition after I12):
// two results tie when their WERs differ by at most PickTieWords words of
// the clip's reference (|ΔWER| × reference words ≤ PickTieWords) and their
// RTFs by less than PickTieRTF (relative to the larger one).
const (
	PickTieWords = 1
	PickTieRTF   = 0.10
)

// PickFor is Pick for device id with the clip's reference word count
// (DeviceBenchmark.Audio.ReferenceWords): when the PickRuleBest winner ties
// (PickTieWords, PickTieRTF) with the OK result of Recommended(id) that also
// reaches RTF <= model.PickMaxRTF, the recommended combination is kept
// (accuracy first, §0.1: in I12 NPU fp16 and int8 had the same WER on a
// 280-word clip and int8 won on speed, while 3a's 10-minute clips show int8
// worse, 6.8 % against 6.1 %). Rule stays model.PickRuleBest. Without a
// recommendation, a reference (refWords <= 0) or a tie it is Pick. The
// self-test (stages DeviceTest) and the mock use it instead of Pick.
//
// Details (task P3): the recommended result is the first OK result with
// Recommended(id)'s model and precision; both it and the winner need a
// measured WER. The RTF difference is relative to the larger of the two
// RTFs (two zero RTFs tie). A small tolerance absorbs the floating-point
// error of |ΔWER| × refWords (an exact count of words).
//
// FROZEN signature; body: task P3.
func PickFor(id string, refWords int, rs []model.BenchmarkResult) (model.BenchmarkPick, bool) {
	p, ok := Pick(rs)
	if !ok || p.Rule != model.PickRuleBest || refWords <= 0 {
		return p, ok
	}
	rec, found := Recommended(id)
	if !found || (p.Model == rec.Model && p.Precision == rec.Precision) {
		return p, ok
	}
	var win, keep *model.BenchmarkResult
	for i := range rs {
		r := &rs[i]
		if !r.OK {
			continue
		}
		if win == nil && r.Model == p.Model && r.Precision == p.Precision && r.RTF <= model.PickMaxRTF {
			win = r
		}
		if keep == nil && r.Model == rec.Model && r.Precision == rec.Precision {
			keep = r
		}
	}
	if win == nil || keep == nil || keep.RTF > model.PickMaxRTF || !pickTie(*win, *keep, refWords) {
		return p, ok
	}
	return model.BenchmarkPick{Model: keep.Model, Precision: keep.Precision, Rule: model.PickRuleBest}, true
}

// pickTie reports whether a and b tie under PickTieWords and PickTieRTF.
func pickTie(a, b model.BenchmarkResult, refWords int) bool {
	if a.WER == nil || b.WER == nil {
		return false
	}
	if math.Abs(*a.WER-*b.WER)*float64(refWords) > PickTieWords+1e-9 {
		return false
	}
	hi := max(a.RTF, b.RTF)
	if hi <= 0 {
		return a.RTF == b.RTF
	}
	return math.Abs(a.RTF-b.RTF)/hi < PickTieRTF
}

// betterWER reports whether a beats the earlier result b under the
// PickRuleBest order: a measured WER beats none; lower WER; equal WER, lower
// RTF; otherwise the earlier one stays.
func betterWER(a, b model.BenchmarkResult) bool {
	switch {
	case a.WER == nil:
		return false
	case b.WER == nil:
		return true
	case *a.WER != *b.WER:
		return *a.WER < *b.WER
	}
	return a.RTF < b.RTF
}
