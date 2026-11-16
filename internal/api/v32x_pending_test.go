// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"lectureflow/internal/model"
)

// v3.2.x P1: job.stats.pending_sentences of a translate job is the stage
// view's stats.pending_sentences (also once the run is done) and the job
// event's; the lecture's usage sum, the other stages and a job without
// pending sentences show no such key.
func TestStageViewPendingSentences(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	for _, s := range []string{"prep", "asr", "summary", "repair"} {
		e.moveJob(model.ScopeLecture, l.ID, s, "queued", "running", "done")
	}
	e.moveJob(model.ScopeLecture, l.ID, "translate", "queued", "running")

	// No pending sentences: no key anywhere.
	e.setStats(l.ID, "translate", model.JobStats{PromptTokens: 500, CompletionTokens: 100})
	_, b := e.do("GET", "/api/lectures/"+l.ID, nil)
	if strings.Contains(string(b), "pending_sentences") {
		t.Fatalf("a run without pending sentences shows the key: %s", b)
	}

	// Running with pending sentences: the stage view and the job event.
	st := model.JobStats{PromptTokens: 500, CompletionTokens: 100, PendingSentences: 3}
	e.setStats(l.ID, "translate", st)
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if v := stageOf(lv.Stages, "translate"); v.Stats.PendingSentences != 3 {
		t.Fatalf("running translate: %+v", v.Stats)
	}
	j := e.moveJob(model.ScopeLecture, l.ID, "translate")
	var ev JobEvent
	e.srv.jobEventStats(&ev, j)
	if ev.Stats == nil || ev.Stats.PendingSentences != 3 {
		t.Fatalf("job event: %+v", ev.Stats)
	}
	if want := e.srv.stageUsage(j); !reflect.DeepEqual(*ev.Stats, want) {
		t.Fatalf("job event stats %+v, stage view %+v", *ev.Stats, want)
	}

	// Done: the stage view keeps them; nothing else has the key.
	e.moveJob(model.ScopeLecture, l.ID, "translate", "done")
	e.setStats(l.ID, "translate", st)
	_, b = e.do("GET", "/api/lectures/"+l.ID, nil)
	var raw struct {
		Stages []struct {
			Stage string                     `json:"stage"`
			Stats map[string]json.RawMessage `json:"stats"`
		} `json:"stages"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Usage["pending_sentences"]; ok {
		t.Fatalf("lecture usage has pending_sentences: %s", b)
	}
	seen := false
	for _, s := range raw.Stages {
		v, ok := s.Stats["pending_sentences"]
		if s.Stage == "translate" {
			seen = ok && string(v) == "3"
		} else if ok {
			t.Errorf("stage %s has pending_sentences", s.Stage)
		}
	}
	if !seen {
		t.Fatalf("done translate without pending_sentences: %s", b)
	}
}

// v3.2.x P1-b: job.stats.mixed_models of a translate job is the stage
// view's stats.mixed_models (running and done) and the job event's; a job
// without it (every job written before v3.2.x) shows no such key, and the
// lecture's usage sum never has it.
func TestStageViewMixedModels(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	for _, s := range []string{"prep", "asr", "summary", "repair"} {
		e.moveJob(model.ScopeLecture, l.ID, s, "queued", "running", "done")
	}
	e.moveJob(model.ScopeLecture, l.ID, "translate", "queued", "running")

	// One model (an old job's stats): the response has no key.
	e.setStats(l.ID, "translate", model.JobStats{PromptTokens: 500, CompletionTokens: 100})
	_, b := e.do("GET", "/api/lectures/"+l.ID, nil)
	if strings.Contains(string(b), "mixed_models") {
		t.Fatalf("a run with one model shows the key: %s", b)
	}
	j := e.moveJob(model.ScopeLecture, l.ID, "translate")
	var ev JobEvent
	e.srv.jobEventStats(&ev, j)
	if eb, _ := json.Marshal(ev); ev.Stats == nil || strings.Contains(string(eb), "mixed_models") {
		t.Fatalf("job event of a run with one model: %s", eb)
	}

	mixed := []string{"DeepSeek / deepseek-chat", "Anthropic / claude-sonnet-4-5"}
	st := model.JobStats{PromptTokens: 500, CompletionTokens: 100, MixedModels: mixed}
	e.setStats(l.ID, "translate", st)
	var lv Lecture
	e.call("GET", "/api/lectures/"+l.ID, nil, http.StatusOK, &lv)
	if v := stageOf(lv.Stages, "translate"); !reflect.DeepEqual(v.Stats.MixedModels, mixed) {
		t.Fatalf("running translate: %+v", v.Stats)
	}
	j = e.moveJob(model.ScopeLecture, l.ID, "translate")
	ev = JobEvent{}
	e.srv.jobEventStats(&ev, j)
	if ev.Stats == nil || !reflect.DeepEqual(ev.Stats.MixedModels, mixed) {
		t.Fatalf("job event: %+v", ev.Stats)
	}
	if want := e.srv.stageUsage(j); !reflect.DeepEqual(*ev.Stats, want) {
		t.Fatalf("job event stats %+v, stage view %+v", *ev.Stats, want)
	}
	// The event holds its own copy of the list.
	ev.Stats.MixedModels[0] = "x"
	if j.Stats.MixedModels[0] != mixed[0] {
		t.Fatal("job event stats share the job's slice")
	}

	// Done: the stage view keeps them; nothing else has the key.
	e.moveJob(model.ScopeLecture, l.ID, "translate", "done")
	e.setStats(l.ID, "translate", st)
	_, b = e.do("GET", "/api/lectures/"+l.ID, nil)
	var raw struct {
		Stages []struct {
			Stage string                     `json:"stage"`
			Stats map[string]json.RawMessage `json:"stats"`
		} `json:"stages"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Usage["mixed_models"]; ok {
		t.Fatalf("lecture usage has mixed_models: %s", b)
	}
	seen := false
	for _, s := range raw.Stages {
		v, ok := s.Stats["mixed_models"]
		if s.Stage == "translate" {
			seen = ok && string(v) == `["DeepSeek / deepseek-chat","Anthropic / claude-sonnet-4-5"]`
		} else if ok {
			t.Errorf("stage %s has mixed_models", s.Stage)
		}
	}
	if !seen {
		t.Fatalf("done translate without mixed_models: %s", b)
	}
}
