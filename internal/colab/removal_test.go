// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// 3.2.0 (accept-320: the first lecture's cleanup took about 42 s of slow
// DELETEs): a finished lecture's directory is removed in the background by
// one cell in its kernel; the lecture does not wait for it, a release
// waits for it (at most removalReleaseWait), and the deletions one by one
// with their retries stay the fallback.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRemoveCellCode(t *testing.T) {
	code := removeCellCode("mnt/lectureflow", "mnt/lectureflow/jobs/lecture-1")
	for _, want := range []string{`_lf_base = "/mnt/lectureflow"`, `_lf_dir = "/mnt/lectureflow/jobs/lecture-1"`,
		"os.path.dirname(_lf_dir) == os.path.join(_lf_base, 'jobs')", "shutil.rmtree(_lf_dir, ignore_errors=True)",
		`"LECTUREFLOW_REMOVED" if not os.path.lexists(_lf_dir) else "LECTUREFLOW_LEFT"`} {
		if !strings.Contains(code, want) {
			t.Errorf("code lacks %q:\n%s", want, code)
		}
	}
	// Nothing else is printed (the cell's stdout is the mark).
	if strings.Count(code, "print(") != 1 {
		t.Errorf("code:\n%s", code)
	}
}

// The lecture is done while its directory is still being removed; the
// release waits for the removal, which goes before the runtime.
func TestRemovalDoesNotDelayTheLecture(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	gate, started := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.removeGate, e.f.removing = gate, started
	e.f.mu.Unlock()
	a := e.lecture(t, "a")
	if _, err := e.b.Transcribe(ctx, a, nil); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no removal cell")
	}
	dir := "content/lectureflow/jobs/a"
	if !e.f.has(dir + "/result.json") {
		t.Fatal("the lecture waited for its removal")
	}
	released := make(chan error, 1)
	go func() { released <- e.b.QueueDrained(ctx) }()
	select {
	case <-released:
		t.Fatal("released while the lecture's files were being removed")
	case <-time.After(100 * time.Millisecond):
	}
	if e.f.count(func() int {
		if e.f.released {
			return 1
		}
		return 0
	}) != 0 {
		t.Fatal("the runtime went before the removal")
	}
	close(gate)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if e.f.has(dir+"/result.json") || e.f.hasDir(dir) || !e.f.released {
		t.Errorf("after the release: files %v, released %v", e.f.files, e.f.released)
	}
	e.f.mu.Lock()
	removed, deleted := slices.Clone(e.f.removed), slices.Clone(e.f.deleted)
	e.f.mu.Unlock()
	if !slices.Equal(removed, []string{dir}) || len(deleted) != 0 {
		t.Errorf("removed %v, deleted one by one %v", removed, deleted)
	}
	if !strings.Contains(e.log.String(), "how=cell") {
		t.Errorf("log: %s", e.log.String())
	}
	e.checkSecrets(t)
}

// The cell cannot confirm and every DELETE is slow: the lecture is done at
// once all the same; the deletions one by one (the fallback) finish in the
// background, and the runtime stays usable for the next lecture meanwhile.
func TestSlowDeletionsDoNotDelayTheLecture(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	e.f.mu.Lock()
	e.f.removeLeft, e.f.deleteDelay = true, 300*time.Millisecond
	e.f.mu.Unlock()
	a := e.lecture(t, "a")
	start := time.Now()
	if _, err := e.b.Transcribe(ctx, a, nil); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the lecture took %s (it waited for the deletions)", took)
	}
	e.b.removalsSettled()
	dir := "content/lectureflow/jobs/a"
	e.f.mu.Lock()
	deleted := slices.Clone(e.f.deleted)
	e.f.mu.Unlock()
	if e.f.has(dir+"/result.json") || !slices.Contains(deleted, dir+"/result.json") || !slices.Contains(deleted, dir) {
		t.Errorf("deleted %v", deleted)
	}
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Errorf("the runtime went: %+v", st)
	}
	if !strings.Contains(e.log.String(), "how=contents") {
		t.Errorf("log: %s", e.log.String())
	}
	_ = e.b.QueueDrained(ctx)
}

// A removal that does not end in time does not hold the release longer
// than removalReleaseWait; the runtime (and its storage) goes.
func TestReleaseDoesNotWaitForeverForARemoval(t *testing.T) {
	old := removalReleaseWait
	removalReleaseWait = 200 * time.Millisecond
	t.Cleanup(func() { removalReleaseWait = old })
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	gate, started := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.removeGate, e.f.removing = gate, started
	e.f.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatal(err)
	}
	<-started
	start := time.Now()
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second || !e.f.released {
		t.Errorf("release took %s, released %v", took, e.f.released)
	}
	if !strings.Contains(e.log.String(), "before a lecture's files were removed") {
		t.Errorf("log: %s", e.log.String())
	}
}

// A file that stays even after the retries releases the runtime once no
// lecture runs on it (cleanup_failed, unchanged); with another lecture
// running, that lecture's end releases it.
func TestRemovalLeftoverReleasesWhenIdle(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	e.f.mu.Lock()
	e.f.removeLeft, e.f.failDelete = true, "jobs/a/result.json"
	e.f.mu.Unlock()
	if _, err := e.b.Transcribe(ctx, e.lecture(t, "a"), nil); err != nil {
		t.Fatal(err)
	}
	e.b.removalsSettled()
	st := e.b.Status(ctx)
	if st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != ReleaseCleanupFailed || st.LastRelease.File != "result.json" {
		t.Fatalf("status %+v (last release %+v)", st, st.LastRelease)
	}
}

// T051: a directory stays busy until its deletion ends with every file
// gone; meanwhile each attempt takes a suffixed directory of its own.
func TestJobDirForBusyDirectory(t *testing.T) {
	b := &Backend{}
	s := &liveSession{}
	j := &jobState{id: "a", key: "a"}
	if d := b.jobDirFor(s, "base", j); d != "base/jobs/a" || j.id != "a" {
		t.Fatalf("free: %q (id %q)", d, j.id)
	}
	b.markBusyDir(s, "base/jobs/a")
	d := b.jobDirFor(s, "base", j)
	if !strings.HasPrefix(d, "base/jobs/a-") || d != "base/jobs/"+j.id || len(j.id) != len("a-")+6 {
		t.Fatalf("busy: %q (id %q)", d, j.id)
	}
	b.settleBusyDir(s, "base/jobs/a", false) // a file was left: still busy
	if j2 := (&jobState{id: "a", key: "a"}); b.jobDirFor(s, "base", j2) == "base/jobs/a" {
		t.Fatal("a directory that kept a file was reused")
	}
	b.settleBusyDir(s, "base/jobs/a", true)
	if j3 := (&jobState{id: "a", key: "a"}); b.jobDirFor(s, "base", j3) != "base/jobs/a" {
		t.Fatal("a removed directory was not reused")
	}
}

// T051 (R3): the same lecture dispatched again to the runtime while its
// previous directory is still being removed in the background. The new
// attempt gets a directory of its own: the removal still under way cannot
// delete its upload or its job.json, the transcription succeeds, and the
// previous directory is removed in the end.
func TestRetryWhileRemovalRunsUsesItsOwnDirectory(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	gate, started := make(chan struct{}), make(chan struct{})
	e.f.mu.Lock()
	e.f.removeGate, e.f.removing = gate, started
	e.f.mu.Unlock()
	a := e.lecture(t, "a")
	if _, err := e.b.Transcribe(ctx, a, nil); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	first := "content/lectureflow/jobs/" + e.transcribingNow(t)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no removal cell")
	}
	e.f.mu.Lock()
	e.f.removing = nil // the next removal cells do not report
	e.f.mu.Unlock()

	// The second attempt is held in its transcription (its job.json and
	// directory in place) while the first removal goes on.
	e.f.hold("*")
	done := make(chan error, 1)
	go func() { _, err := e.b.Transcribe(ctx, a, nil); done <- err }()
	var name string
	select {
	case name = <-e.f.transcribing:
	case err := <-done:
		t.Fatalf("second attempt ended before its transcription: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the second attempt did not start transcribing")
	}
	second := "content/lectureflow/jobs/" + name
	if second == first || !strings.HasPrefix(name, "a-") {
		t.Errorf("the second attempt uses %q (the first %q)", second, first)
	}
	close(gate)
	e.waitFor(t, "the first removal", func() bool {
		e.f.mu.Lock()
		defer e.f.mu.Unlock()
		return slices.Contains(e.f.removed, first)
	})
	if !e.f.has(second + "/job.json") {
		t.Error("the first removal deleted the second attempt's job.json")
	}
	e.f.release("*")
	if err := <-done; err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	e.b.removalsSettled()
	if e.f.hasDir(first) || e.f.hasDir(second) {
		t.Errorf("left on the runtime: dirs %v, files %v", e.f.dirs, e.f.files)
	}
	e.f.mu.Lock()
	removed := slices.Clone(e.f.removed)
	e.f.mu.Unlock()
	if !slices.Contains(removed, first) || !slices.Contains(removed, second) {
		t.Errorf("removed %v", removed)
	}
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Errorf("the runtime went: %+v", st)
	}
	_ = e.b.QueueDrained(ctx)
	e.checkSecrets(t)
}
