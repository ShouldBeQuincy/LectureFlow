// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"encoding/json"
	"testing"
	"time"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
)

// v3.2.x: the Colab card's retry in place, the runtime that is held but
// does not answer (paid: still billed; free: still uses the window) and a
// session being released, as mock scenarios the front end can be looked at
// with.

func TestMockColabReconnect(t *testing.T) {
	srv := testServer(t)
	r := obsResources(t, srv.URL, "colab_reconnect")
	st := r.Remote
	if st.State != colab.StateRunning || len(st.Jobs) != 1 || st.Jobs[0].Reconnect == nil {
		t.Fatalf("colab_reconnect status: %+v", st)
	}
	rc := st.Jobs[0].Reconnect
	if rc.Attempt < 1 || rc.Attempt > rc.Of || rc.Of != 3 || !rc.NextAt.After(time.Now().Add(-time.Second)) || rc.NextAt.After(time.Now().Add(16*time.Second)) {
		t.Errorf("reconnect: %+v", rc)
	}
	card := r.Remotes[0]
	if len(card.Jobs) != 1 || card.Jobs[0].Reconnect == nil || *card.Jobs[0].Reconnect != *rc || card.Jobs[0].LectureTitle == "" {
		t.Errorf("card jobs: %+v", card.Jobs)
	}
	// the error of before the session: older than session_started_at
	if st.SessionStartedAt == nil || st.LastError == nil || st.LastErrorAt == nil || !st.LastErrorAt.Before(*st.SessionStartedAt) {
		t.Errorf("stale error: %+v at %v, session %v", st.LastError, st.LastErrorAt, st.SessionStartedAt)
	}
	if card.LastError == nil || !card.LastError.At.Equal(*st.LastErrorAt) {
		t.Errorf("card error: %+v", card.LastError)
	}
	if rt := st.Runtime; rt == nil || !rt.Held || rt.Reachable != colab.ReachUnreachable || rt.UnreachableSince == nil {
		t.Errorf("runtime: %+v", rt)
	}
}

func TestMockColabUnreachable(t *testing.T) {
	srv := testServer(t)
	for sc, paid := range map[string]bool{"colab_unreachable": true, "colab_unreachable_free": false} {
		r := obsResources(t, srv.URL, sc)
		st := r.Remote
		if st.State != colab.StateRunning || len(st.Jobs) != 0 || len(r.Remotes[0].Jobs) != 0 {
			t.Errorf("%s: state %s, jobs %+v", sc, st.State, st.Jobs)
		}
		rt := st.Runtime
		if rt == nil || !rt.Held || rt.Reachable != colab.ReachUnreachable || rt.UnreachableSince == nil || rt.Billing != paid {
			t.Errorf("%s: runtime %+v", sc, rt)
		}
		if paid && (st.SessionSpentCU <= 0 || st.CUPerHour <= 0 || st.Tier != colab.TierPaid) {
			t.Errorf("%s: spent %v rate %v tier %s", sc, st.SessionSpentCU, st.CUPerHour, st.Tier)
		}
		if !paid && (st.Tier != colab.TierFree || st.Accelerator != "T4") {
			t.Errorf("%s: tier %s accelerator %s", sc, st.Tier, st.Accelerator)
		}
		if st.LastError != nil || st.SessionStartedAt == nil {
			t.Errorf("%s: error %+v, session %v", sc, st.LastError, st.SessionStartedAt)
		}
	}
	// colab_running: the runtime answers
	if rt := obsResources(t, srv.URL, "colab_running").Remote.Runtime; rt == nil || !rt.Held || rt.Reachable != colab.ReachOK {
		t.Errorf("colab_running: runtime %+v", rt)
	}
	// "立即关机" stays available: the runtime is released
	var v struct {
		Status colab.Status `json:"status"`
	}
	if code, b, _ := mockDo(t, "POST", srv.URL+"/api/colab/shutdown?mock=colab_unreachable", `{"mode":"now"}`); code != 200 || json.Unmarshal(b, &v) != nil || v.Status.State != colab.StateStopping {
		t.Fatalf("shutdown now: %d %s", code, b)
	}
}

func TestMockColabReleasing(t *testing.T) {
	srv := testServer(t)
	r := obsResources(t, srv.URL, "colab_releasing")
	st := r.Remote
	if st.State != colab.StateStopping || len(st.Jobs) != 0 || st.Current != nil || len(r.Remotes[0].Jobs) != 0 {
		t.Fatalf("colab_releasing: %+v", st)
	}
	if rt := st.Runtime; rt == nil || !rt.Held || rt.Reachable != colab.ReachUnreachable || !rt.Billing {
		t.Errorf("runtime: %+v", rt)
	}
	if lo := st.Leftover; lo == nil || lo.Code != colab.CodeReleaseFailed || lo.Accelerator != "" {
		t.Errorf("leftover: %+v", lo)
	}
	// the running lecture of the store is not on the card (it is back in the local queue)
	if running := colabLecturesByASR(t, srv.URL, "colab_releasing")[model.StatusRunning]; len(running) == 0 {
		t.Error("the fixtures have no running asr stage")
	}
	// it stays stopping (the release keeps failing)
	if obsResources(t, srv.URL, "colab_releasing").Remote.State != colab.StateStopping {
		t.Error("colab_releasing ended")
	}
}
