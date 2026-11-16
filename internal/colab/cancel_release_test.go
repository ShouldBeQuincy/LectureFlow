// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// A cancelled lecture's Transcribe returns within seconds even when Colab
// hangs while its runtime is released (review R65 O1): the release goes on
// in the background as a hand-back's does, and every guarantee of that one
// holds — the session is stopping at once, its record says so, the next
// start releases what a quit left behind, a failed release stays for Tick.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// cancelHeldLecture runs lecture l1, holds it in its transcription, and
// cancels it; it returns Transcribe's error, failing when that takes more
// than 8 s.
func cancelHeldLecture(t *testing.T, e *fakeEnv) error {
	t.Helper()
	e.f.hold("l1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startLecture(e, ctx, e.lecture(t, "l1"))
	e.transcribingNow(t)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || Code(err) != CodeCancelled {
			t.Fatalf("err %v (code %s), want %s", err, Code(err), CodeCancelled)
		}
		return err
	case <-time.After(8 * time.Second):
		st := e.b.Status(context.Background())
		t.Fatalf("Transcribe has not returned %s after the cancel; state %s", time.Since(start).Round(time.Second), st.State)
		return nil
	}
}

func sessionRecordOf(t *testing.T, e *fakeEnv) (sessionRecord, bool) {
	t.Helper()
	v, ok, err := e.kv.GetKV(context.Background(), KVSession)
	if err != nil || !ok || v == "" {
		return sessionRecord{}, false
	}
	var rec sessionRecord
	if json.Unmarshal([]byte(v), &rec) != nil {
		t.Fatalf("session record %q", v)
	}
	return rec, true
}

// The release hangs (POST unassign held): the cancel returns at once, the
// session is stopping and recorded so, nothing is dispatched to it; once
// Colab answers the release ends as before, with the reason cancelled.
// (Red before: Transcribe waited for the release, about 30 s here.)
func TestCancelReturnsWithinSecondsWhileTheReleaseHangs(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.b.cancelWait = 300 * time.Millisecond
	ctx := context.Background()
	arrived, letGo := heldRelease(t, e.f)
	cancelHeldLecture(t, e)
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the release never reached Colab")
	}
	if st := e.b.Status(ctx); st.State != StateStopping {
		t.Errorf("state %s while the release hangs, want %s", st.State, StateStopping)
	}
	if ok, _ := e.b.CanDispatch(ctx, 600); ok {
		t.Error("a lecture may be dispatched to the runtime being released")
	}
	if rec, ok := sessionRecordOf(t, e); !ok || rec.State != StateStopping || rec.Endpoint == "" {
		t.Errorf("session record %+v (present %v), want one in state stopping with its endpoint", rec, ok)
	}
	if e.f.wasReleased() {
		t.Fatal("released although Colab holds the request")
	}
	letGo()
	e.b.handbacksSettled()
	st := e.b.Status(ctx)
	if !e.f.wasReleased() || st.State != StateReleased || st.LastRelease == nil || st.LastRelease.Reason != ReleaseCancelled {
		t.Errorf("after Colab answered: released %v, state %s, last release %+v", e.f.wasReleased(), st.State, st.LastRelease)
	}
	if _, ok := sessionRecordOf(t, e); ok {
		t.Error("the session record is left after the release")
	}
}

// The app quits while the cancelled lecture's release still hangs: the
// record lets the next start release the runtime.
func TestCancelledLecturesRecordLetsTheNextStartRelease(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.b.cancelWait = 300 * time.Millisecond
	ctx := context.Background()
	arrived, letGo := heldRelease(t, e.f)
	cancelHeldLecture(t, e)
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the release never reached Colab")
	}
	lg, _ := newTestLogger()
	o := e.b.o
	o.Logger, o.RunID = lg, "run-2"
	b2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.bgCancel)
	e.f.mu.Lock()
	e.f.unassignGate = nil
	e.f.mu.Unlock()
	if err := b2.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if !e.f.wasReleased() {
		t.Error("the next start did not release the runtime")
	}
	letGo()
	e.b.handbacksSettled()
}

// The release fails (unassign answers 500 every time): the session stays
// stopping with its record for Tick and the next start, and the failure is
// the last error.
func TestCancelWhoseReleaseFailsKeepsTheRecord(t *testing.T) {
	e := newFakeEnv(t, t4())
	e.b.cancelWait = 300 * time.Millisecond
	ctx := context.Background()
	e.f.fault(fakeFault{method: "POST", suffix: "/tun/m/unassign/gpu-fake-1", times: -1, status: 500})
	cancelHeldLecture(t, e)
	e.b.handbacksSettled()
	st := e.b.Status(ctx)
	if e.f.wasReleased() || st.State != StateStopping {
		t.Errorf("released %v, state %s, want the session stopping", e.f.wasReleased(), st.State)
	}
	if st.LastError == nil || st.LastError.Code != CodeReleaseFailed {
		t.Errorf("last error %+v, want %s", st.LastError, CodeReleaseFailed)
	}
	if rec, ok := sessionRecordOf(t, e); !ok || rec.State != StateStopping || rec.Endpoint == "" {
		t.Errorf("session record %+v (present %v), want one in state stopping with its endpoint", rec, ok)
	}
}
