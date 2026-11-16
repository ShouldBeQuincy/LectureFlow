// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// janitorClient is a legacy-mode client whose glossary_janitor role uses
// the glossary role's provider and model.
func janitorClient(t *testing.T, s *llmtest.Server) *llm.HTTPClient {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	s.Configure(cfg)
	cfg.LLM.Roles[model.RoleGlossaryJanitor] = cfg.LLM.Roles[config.RoleGlossary]
	c := llm.New(cfg, nil, "", nil)
	c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	return c
}

// TestJanitorRoleAndHandle: the G3.4 prompt is recognised as
// glossary_janitor; without a handler the fixture (or {"actions": []})
// answers; Handle answers with a function of the request; failures still apply.
func TestJanitorRoleAndHandle(t *testing.T) {
	s := llmtest.New(t)
	c := janitorClient(t, s)
	ctx := context.Background()
	req := ctxbuild.JanitorRequests(ctxbuild.JanitorInput{CourseName: "C", Terms: []model.Term{{ID: "t1", En: "public", EnKey: "public", Enabled: true}}})[0]
	if got := llmtest.RoleOf(req.System); got != llmtest.RoleGlossaryJanitor {
		t.Fatalf("RoleOf = %q", got)
	}

	res, err := c.Complete(ctx, model.RoleGlossaryJanitor, req.System, req.User, true)
	if err != nil || strings.TrimSpace(res.Text) != `{"actions": []}` {
		t.Fatalf("default answer %q %v", res.Text, err)
	}

	var calls atomic.Int32
	s.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string {
		calls.Add(1)
		if !strings.Contains(user, `"id":"t1"`) {
			return `{"actions": []}`
		}
		return `{"actions": [{"id": "t1", "action": "reference", "reason": "Common word."}]}`
	})
	res, err = c.Complete(ctx, model.RoleGlossaryJanitor, req.System, req.User, true)
	if err != nil || !strings.Contains(res.Text, `"action": "reference"`) || calls.Load() != 1 {
		t.Fatalf("handled answer %q %v (calls %d)", res.Text, err, calls.Load())
	}

	s.FailNext(llmtest.RoleGlossaryJanitor, 1, llmtest.FailHTTP401)
	if _, err := c.Complete(ctx, model.RoleGlossaryJanitor, req.System, req.User, true); err == nil || !sched.IsPermanent(err) {
		t.Errorf("injected 401: %v, want a permanent error", err)
	}

	s.Handle(llmtest.RoleGlossaryJanitor, nil)
	if res, err := c.Complete(ctx, model.RoleGlossaryJanitor, req.System, req.User, true); err != nil || strings.Contains(res.Text, "reference") {
		t.Errorf("after removing the handler: %q %v", res.Text, err)
	}
	if n := s.Count(llmtest.RoleGlossaryJanitor); n != 4 {
		t.Errorf("Count = %d, want 4", n)
	}
}

// TestTargetLabels: TargetIdx reads the SPEC-v2 G5.3 label and the v1 one.
func TestTargetLabels(t *testing.T) {
	for _, u := range []string{"[BEFORE]\nA.\n[TARGET #12]\nThe target.\n[AFTER]\n(none)", "[上文]\nA.\n[目标 #12]\nThe target.\n[下文]\n（无）"} {
		if llmtest.TargetIdx(u) != 12 || llmtest.TargetText(u) != "The target." {
			t.Errorf("TargetIdx/TargetText of %q = %d %q", u, llmtest.TargetIdx(u), llmtest.TargetText(u))
		}
	}
}

// TestTruncatedAnswer: an answer wrapped with Truncated is sent as cut off
// at the output limit; the Anthropic adapter reports llm.ErrTruncated after
// its retry with a doubled max_tokens.
func TestTruncatedAnswer(t *testing.T) {
	s := llmtest.New(t)
	c, _, _ := settingsClient(t, s, model.ProviderAnthropic)
	req := ctxbuild.JanitorRequests(ctxbuild.JanitorInput{CourseName: "C", Terms: []model.Term{{ID: "t1", En: "public", EnKey: "public", Enabled: true}}})[0]
	s.Handle(llmtest.RoleGlossaryJanitor, func(system, user string) string { return llmtest.Truncated(`{"actions": [`) })
	_, err := c.Complete(context.Background(), model.RoleGlossaryJanitor, req.System, req.User, true)
	if !errors.Is(err, llm.ErrTruncated) || !sched.IsPermanent(err) {
		t.Fatalf("err %v, want a permanent llm.ErrTruncated", err)
	}
	if n := s.Count(llmtest.RoleGlossaryJanitor); n != 3 {
		t.Errorf("%d requests, want 3 (v3.2.x P1-c: two retries, each with a doubled limit)", n)
	}
	s.Handle(llmtest.RoleGlossaryJanitor, nil)
	if res, err := c.Complete(context.Background(), model.RoleGlossaryJanitor, req.System, req.User, true); err != nil || strings.TrimSpace(res.Text) != `{"actions": []}` {
		t.Errorf("after the handler is removed: %q %v", res.Text, err)
	}
}
