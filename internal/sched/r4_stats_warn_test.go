// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// failStatsStore is a store whose SetJobStats fails with err (nil = works).
type failStatsStore struct {
	db.Store
	mu    sync.Mutex
	err   error
	calls int
}

func (f *failStatsStore) SetJobStats(context.Context, string, model.JobStats) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

func (f *failStatsStore) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// failProgressStore is a store whose SetJobProgress fails with err (nil =
// works) and records the values written; SetJobStats works.
type failProgressStore struct {
	db.Store
	mu      sync.Mutex
	err     error
	calls   int
	written []float64
}

func (f *failProgressStore) SetJobProgress(_ context.Context, _ string, p float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, p)
	return nil
}

func (f *failProgressStore) SetJobStats(context.Context, string, model.JobStats) error { return nil }

func (f *failProgressStore) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Review R51 (after r4 D14): a progress write that keeps failing is logged
// like the stats (once a minute for the same reason, with the number not
// logged), and the value is tried again on the next flush although the
// stage reports nothing new; a success says how many failures were not
// logged; the run's end tells failures still unlogged and ends the count.
func TestProgressWriteFailureIsRetriedAndThrottled(t *testing.T) {
	buf := &logBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	st := &failProgressStore{err: errors.New("database is locked")}
	s := &Scheduler{st: st, ev: NopEvents{}}
	r := &run{job: model.Job{ID: "r4-progress-job"}, live: true, now: func() time.Time { return now }}
	flush := func(n int) {
		for range n {
			s.flushRun(r)
			now = now.Add(500 * time.Millisecond)
		}
	}
	warns := func() []string { return buf.lines("progress write") }

	r.pmu.Lock()
	r.latest, r.dirty = 0.25, true // reported once
	r.pmu.Unlock()
	flush(121) // one minute of failures, nothing new reported
	if st.calls != 121 {
		t.Fatalf("%d progress writes, want 121 (tried again on every flush)", st.calls)
	}
	if w := warns(); len(w) != 2 || !strings.Contains(w[1], "unlogged=119") {
		t.Fatalf("warnings:\n%s", strings.Join(w, "\n"))
	}
	flush(3)
	st.setErr(nil)
	flush(1)
	if len(st.written) != 1 || st.written[0] != 0.25 || r.lastFlush != 0.25 {
		t.Fatalf("written %v, last flush %v", st.written, r.lastFlush)
	}
	if ok := buf.lines("progress written again"); len(ok) != 1 || !strings.Contains(ok[0], "unlogged_failures=3") {
		t.Fatalf("recovery: %q", ok)
	}
	flush(5) // nothing new: no more writes
	if st.calls != 125 {
		t.Errorf("%d progress writes after the value was written, want 125", st.calls)
	}

	// failing again, then the run ends
	st.setErr(errors.New("database is locked"))
	r.pmu.Lock()
	r.latest, r.dirty = 0.5, true
	r.pmu.Unlock()
	flush(3)
	endProgressWriteLog(r)
	if w := warns(); len(w) != 4 || !strings.Contains(w[3], "unlogged=2") || !strings.Contains(w[3], "run_finished=true") {
		t.Fatalf("end of the run:\n%s", strings.Join(w, "\n"))
	}
	statsWarns.mu.Lock()
	_, kept := statsWarns.m[r.job.ID+progressWriteLog.suffix]
	statsWarns.mu.Unlock()
	if kept {
		t.Error("the progress count outlives the run")
	}
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) lines(msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(s, `msg="`+msg+`"`) || strings.Contains(s, "msg="+msg+" ") {
			out = append(out, s)
		}
	}
	return out
}

// Review R2 item 9 (r4 D14): a stats write that keeps failing is tried on
// every flush as before, but logged once per statsWarnEvery for the same
// reason, with the number of failures not logged; another reason is logged
// at once; a success after unlogged failures says how many; the run's last
// write logs its failure always.
func TestStatsWriteFailureLogIsThrottled(t *testing.T) {
	buf := &logBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	st := &failStatsStore{err: errors.New("database is locked")}
	s := &Scheduler{st: st}
	r := &run{job: model.Job{ID: "r4-d14-job"}, now: func() time.Time { return now }}
	flush := func(n int) {
		for range n {
			r.Update(func(st *model.JobStats) { st.Status = "x" }) // dirty
			s.persistStats(context.Background(), r, nil)
			now = now.Add(500 * time.Millisecond)
		}
	}
	warns := func() []string { return buf.lines("job stats") }

	flush(100) // 50 s of failures
	if st.calls != 100 {
		t.Fatalf("%d writes, want 100 (every flush still tries)", st.calls)
	}
	if w := warns(); len(w) != 1 {
		t.Fatalf("%d warnings in 50 s, want 1:\n%s", len(w), strings.Join(w, "\n"))
	}
	flush(21) // to one minute after the first warning
	w := warns()
	if len(w) != 2 || !strings.Contains(w[1], "unlogged=119") {
		t.Fatalf("after a minute:\n%s", strings.Join(w, "\n"))
	}
	if !r.statsDirty {
		t.Error("a failed write is not tried again")
	}

	st.setErr(errors.New("disk I/O error")) // another reason: at once
	flush(1)
	if w := warns(); len(w) != 3 || !strings.Contains(w[2], "disk I/O error") || !strings.Contains(w[2], "unlogged=0") {
		t.Fatalf("another reason:\n%s", strings.Join(w, "\n"))
	}
	flush(4)
	st.setErr(nil)
	flush(1)
	if ok := buf.lines("job stats written again"); len(ok) != 1 || !strings.Contains(ok[0], "unlogged_failures=4") {
		t.Fatalf("recovery: %q", ok)
	}
	flush(3) // written: nothing more
	if len(warns()) != 3 || len(buf.lines("job stats written again")) != 1 {
		t.Fatalf("log after recovery:\n%s", buf.b.String())
	}

	// failing again, then the run's last write: logged although within the minute
	st.setErr(errors.New("database is locked"))
	flush(2)
	s.persistStats(context.Background(), r, func(st *model.JobStats) { st.DurationMS = 1 })
	if w := warns(); len(w) != 5 || !strings.Contains(w[4], "unlogged=1") {
		t.Fatalf("last write:\n%s", strings.Join(w, "\n"))
	}
	statsWarns.mu.Lock()
	_, kept := statsWarns.m[r.job.ID]
	statsWarns.mu.Unlock()
	if kept {
		t.Error("the run's count outlives its last write")
	}
}
