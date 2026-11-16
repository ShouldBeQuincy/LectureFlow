// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
)

// v3.2.x P1-c: the fake Anthropic endpoint answers "stream": true with the
// events of the message it would send whole; other requests are unchanged.
func TestFakeServerStreams(t *testing.T) {
	s := llmtest.New(t)
	post := func(stream bool) (*http.Response, string) {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5-5", "max_tokens": 1000, "stream": stream,
			"system":   []any{map[string]any{"type": "text", "text": ctxbuild.SummarySystemTemplate}},
			"messages": []any{map[string]any{"role": "user", "content": "t"}},
			"thinking": map[string]any{"type": "adaptive", "display": "summarized"},
		})
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/messages", bytes.NewReader(body))
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, whole := post(false)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || !json.Valid([]byte(whole)) {
		t.Fatalf("not streamed: %s %s", resp.Header.Get("Content-Type"), whole)
	}
	resp, sse := post(true)
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"event: message_start\n", "event: ping\n", `"thinking_delta"`, `"signature_delta"`, `"text_delta"`,
		"event: message_delta\n", `"stop_reason":"end_turn"`, "event: message_stop\n"} {
		if !strings.Contains(sse, want) {
			t.Errorf("stream lacks %q:\n%s", want, sse)
		}
	}
	if !strings.Contains(sse, `"thinking":"llmtest: w`) {
		t.Errorf("no thinking summary:\n%s", sse)
	}
	reqs := s.Requests(llmtest.RoleSummary)
	if len(reqs) != 2 || reqs[0].Stream || !reqs[1].Stream || reqs[1].Display != "summarized" {
		t.Errorf("requests %+v", reqs)
	}
	// MessageSSE of the whole answer is the same stream, without the summary
	// (the whole answer's thinking is empty).
	conv, err := llmtest.MessageSSE([]byte(whole))
	if err != nil || !strings.Contains(string(conv), "event: message_stop") || strings.Contains(string(conv), "llmtest: w") {
		t.Errorf("MessageSSE %v:\n%s", err, conv)
	}
}
