// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// v3.2.x: stats.remote of a Colab transcription carries the reconnect state
// (stage view and job event); without one the key is absent.
func TestStageViewRemoteReconnect(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	e.moveJob(model.ScopeLecture, l.ID, "prep", "queued", "running", "done")
	e.moveJob(model.ScopeLecture, l.ID, "asr", "queued", "running")
	rs := model.RemoteStats{Step: model.RemoteStepRemoteTranscribe, Steps: model.RemoteSteps, EtaSec: 300, Accelerator: "T4", Stage: "transcribe"}
	e.setStats(l.ID, "asr", model.JobStats{Remote: &rs})
	_, b := e.do("GET", "/api/lectures/"+l.ID, nil)
	if !strings.Contains(string(b), `"remote":`) || strings.Contains(string(b), "reconnect") {
		t.Fatalf("without a reconnect: %s", b)
	}

	next := time.Date(2026, 11, 11, 9, 0, 5, 0, time.UTC)
	rs.Reconnect = &remote.Reconnect{Attempt: 2, Of: 4, NextAt: next}
	e.setStats(l.ID, "asr", model.JobStats{Remote: &rs})
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	st := stageOf(lv.Stages, "asr")
	if st.Stats.Remote == nil || st.Stats.Remote.Reconnect == nil || st.Stats.Remote.Reconnect.Attempt != 2 ||
		st.Stats.Remote.Reconnect.Of != 4 || !st.Stats.Remote.Reconnect.NextAt.Equal(next) {
		t.Fatalf("stage view stats.remote: %+v", st.Stats.Remote)
	}
	_, b = e.do("GET", "/api/lectures/"+l.ID, nil)
	if !strings.Contains(string(b), `"reconnect":{"attempt":2,"of":4,"next_at":"2026-11-11T09:00:05Z"}`) {
		t.Fatalf("reconnect JSON: %s", b)
	}

	j := e.moveJob(model.ScopeLecture, l.ID, "asr")
	var ev JobEvent
	e.srv.jobEventStats(&ev, j)
	if ev.Stats == nil || ev.Stats.Remote == nil || ev.Stats.Remote.Reconnect == nil || ev.Stats.Remote.Reconnect.Attempt != 2 {
		t.Fatalf("job event: %+v", ev.Stats)
	}
	if ev.Stats.Remote.Reconnect == j.Stats.Remote.Reconnect {
		t.Fatal("the event shares the job's reconnect value")
	}
}
