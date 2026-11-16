// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// hitsOf renders the requests the server received, refused ones included.
func hitsOf(s *llmtest.Server) string {
	var out string
	for _, h := range s.Hits() {
		out += fmt.Sprintf("[format=%v status=%d]", h.Format, h.Status)
	}
	return out
}

// Review fix of v3.2.x P1: a structured-output request refused with HTTP
// 400 is sent exactly once more, without the schema — counted on the wire,
// the refused request included.
func TestStructuredOutputResentExactlyOnce(t *testing.T) {
	ctx := llm.WithJSONSchema(context.Background(), ctxbuild.SchemaTranslate)
	req := translateReq()

	// The schema is what the server refuses: one 400, one resend, and the
	// next call goes without the schema at once.
	s := llmtest.New(t)
	c, _, _ := claudeClient(t, s, "claude-opus-4-7", model.RoleParams{})
	s.RejectFormat(true)
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("[format=true status=%d][format=false status=%d]", http.StatusBadRequest, http.StatusOK)
	if got := hitsOf(s); got != want {
		t.Fatalf("first call: %s, want %s", got, want)
	}
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if got := hitsOf(s); got != want+"[format=false status=200]" {
		t.Fatalf("second call: %s", got)
	}
	// What is remembered is that schema: another one on the same model is
	// still sent (review suggestion 4).
	s.RejectFormat(false)
	sum := ctxbuild.SummaryRequest(ctxbuild.LectureSnapshot{LectureID: "L", Sentences: []ctxbuild.Sentence{{Idx: 0, Text: "A cache line."}}})
	if _, err := c.Complete(llm.WithJSONSchema(context.Background(), ctxbuild.SchemaRepair), model.RoleRepair, sum.System, sum.User, true); err != nil {
		t.Fatal(err)
	}
	if got := hitsOf(s); got != want+"[format=false status=200][format=true status=200]" {
		t.Fatalf("another schema: %s", got)
	}

	// Every request is refused: two requests, not more, and the next call
	// tries the schema again.
	s = llmtest.New(t)
	c, _, _ = claudeClient(t, s, "claude-sonnet-5-5", model.RoleParams{})
	s.SetError(http.StatusBadRequest, "messages: something else is wrong")
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err == nil {
		t.Fatal("no error")
	}
	want = "[format=true status=400][format=false status=400]"
	if got := hitsOf(s); got != want {
		t.Fatalf("refused: %s, want %s", got, want)
	}
	s.SetError(0, "")
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if got := hitsOf(s); got != want+"[format=true status=200]" {
		t.Fatalf("after: %s", got)
	}
}
