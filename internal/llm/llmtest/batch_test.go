// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
)

// v3.2.x P2: batch translate requests against the fake server.

type batchAnswer struct {
	Items []struct {
		Idx int    `json:"idx"`
		Zh  string `json:"zh"`
	} `json:"items"`
}

func batchOnce(t *testing.T, c *llm.HTTPClient, first, n int) (ctxbuild.TranslateBatchRequest, llm.Result, error) {
	t.Helper()
	lec := ctxbuild.LectureSnapshot{Sentences: sample(t)}
	req := ctxbuild.BuildTranslateBatchRequest(lec, first, n, 3)
	res, err := c.Complete(context.Background(), config.RoleTranslate, req.System, req.User, true)
	return req, res, err
}

func idxsOf(t *testing.T, text string) []int {
	t.Helper()
	var a batchAnswer
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		t.Fatalf("answer %q: %v", text, err)
	}
	var out []int
	for _, it := range a.Items {
		out = append(out, it.Idx)
	}
	return out
}

func TestRoleOfBatch(t *testing.T) {
	lec := ctxbuild.LectureSnapshot{Sentences: sample(t)}
	if llmtest.RoleOf(ctxbuild.TranslateBatchSystemTemplate) != llmtest.RoleTranslate ||
		llmtest.RoleOf(ctxbuild.TranslateBatchSystem(lec)) != llmtest.RoleTranslate {
		t.Error("the batch system prompt is not recognised as translate")
	}
	if !llmtest.IsBatchSystem(ctxbuild.TranslateBatchSystem(lec)) || llmtest.IsBatchSystem(ctxbuild.TranslateSystem(lec)) {
		t.Error("IsBatchSystem")
	}
	u := ctxbuild.BuildTranslateBatchRequest(lec, 2, 3, 3).User
	if got := llmtest.TargetIdxs(u); !slices.Equal(got, []int{2, 3, 4}) {
		t.Errorf("TargetIdxs %v", got)
	}
	if got := llmtest.TargetTexts(u); len(got) != 3 || got[0] != lec.Sentences[2].Text || got[2] != lec.Sentences[4].Text {
		t.Errorf("TargetTexts %q", got)
	}
}

func TestBatchAnswer(t *testing.T) {
	s, c := setup(t)
	s.SetTranslation(4, "覆盖的译文。")
	req, res, err := batchOnce(t, c, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	var a batchAnswer
	if err := llm.ParseJSON(res.Text, &a); err != nil {
		t.Fatalf("%q: %v", res.Text, err)
	}
	if got := idxsOf(t, res.Text); !slices.Equal(got, req.Idxs) {
		t.Fatalf("items %v, want %v", got, req.Idxs)
	}
	if a.Items[1].Zh != "覆盖的译文。" || a.Items[0].Zh == "" {
		t.Errorf("items %+v", a.Items)
	}
	// each item is what the single request of that sentence answers
	lec := ctxbuild.LectureSnapshot{Sentences: sample(t)}
	for _, it := range a.Items {
		r := ctxbuild.BuildTranslateRequest(lec, it.Idx, 3)
		one, err := c.Complete(context.Background(), config.RoleTranslate, r.System, r.User, true)
		if err != nil {
			t.Fatal(err)
		}
		var single struct {
			Idx int    `json:"idx"`
			Zh  string `json:"zh"`
		}
		if err := json.Unmarshal([]byte(one.Text), &single); err != nil || single.Idx != it.Idx || single.Zh != it.Zh {
			t.Errorf("idx %d: batch %q, single %q", it.Idx, it.Zh, one.Text)
		}
	}
	rs := s.Requests(llmtest.RoleTranslate)
	if len(rs) != 5 || !rs[0].Batch || !slices.Equal(rs[0].TargetIdxs, []int{3, 4, 5, 6}) || rs[0].TargetIdx != 3 {
		t.Fatalf("requests %+v", rs)
	}
	for _, r := range rs[1:] {
		if r.Batch || len(r.TargetIdxs) != 1 || r.TargetIdxs[0] != r.TargetIdx {
			t.Errorf("single request %+v", r)
		}
	}
	if b := s.BatchRequests(); len(b) != 1 || b[0].User != req.User {
		t.Errorf("BatchRequests %d", len(b))
	}
	if got := s.TranslateIdxs(); !slices.Equal(got, []int{3, 3, 4, 5, 6}) {
		t.Errorf("TranslateIdxs %v", got)
	}
	// a Handle answer is used for batch requests too
	s.Handle(llmtest.RoleTranslate, func(system, user string) string { return `{"items": []}` })
	if _, res, _ := batchOnce(t, c, 0, 2); res.Text != `{"items": []}` {
		t.Errorf("Handle: %q", res.Text)
	}
}

func TestBatchFailureModes(t *testing.T) {
	s, c := setup(t)
	want := []int{5, 6, 7}
	for _, tc := range []struct {
		mode llmtest.Failure
		want []int
		bare bool
	}{
		{llmtest.FailBatchMissing, []int{5, 6}, false},
		{llmtest.FailBatchExtra, []int{5, 6, 7, 1007}, false},
		{llmtest.FailBatchDuplicate, []int{5, 6, 7, 5}, false},
		{llmtest.FailBatchWrongIds, []int{1005, 1006, 1007}, false},
		{llmtest.FailWrongIdx, []int{1005, 1006, 1007}, false},
		{llmtest.FailBatchShuffled, []int{7, 6, 5}, false},
		{llmtest.FailBatchBareArray, want, true},
	} {
		s.FailNext(llmtest.RoleTranslate, 1, tc.mode)
		_, res, err := batchOnce(t, c, 5, 3)
		if err != nil {
			t.Fatalf("mode %d: %v", tc.mode, err)
		}
		text := res.Text
		if tc.bare {
			if !strings.HasPrefix(text, "[") {
				t.Fatalf("bare array: %q", text)
			}
			text = `{"items": ` + text + `}`
		}
		if got := idxsOf(t, text); !slices.Equal(got, tc.want) {
			t.Errorf("mode %d: items %v, want %v", tc.mode, got, tc.want)
		}
	}
	// a batch mode leaves a single-sentence answer alone
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailBatchMissing)
	if res, err := translateOnce(t, c, 2); err != nil || !strings.Contains(res.Text, `"idx":2`) {
		t.Errorf("single with a batch mode: %q %v", res.Text, err)
	}
	// not JSON and second thoughts work on batches as on single requests
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailNotJSON)
	if _, res, _ := batchOnce(t, c, 5, 3); json.Valid([]byte(res.Text)) {
		t.Errorf("not JSON: %q", res.Text)
	}
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailSecondJSON)
	var a batchAnswer
	if _, res, _ := batchOnce(t, c, 5, 3); !errors.Is(llm.ParseJSON(res.Text, &a), llm.ErrSecondJSON) {
		t.Errorf("second JSON: %q", res.Text)
	}
}

// FailNextBatch is taken by batch requests only, ahead of FailNext.
func TestFailNextBatchQueue(t *testing.T) {
	s, c := setup(t)
	s.FailNextBatch(1, llmtest.FailBatchMissing)
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailWrongIdx)
	// the single request takes the role queue, not the batch queue
	if res, err := translateOnce(t, c, 2); err != nil || !strings.Contains(res.Text, `"idx": 1002`) {
		t.Fatalf("single: %q %v", res.Text, err)
	}
	if _, res, err := batchOnce(t, c, 5, 3); err != nil || !slices.Equal(idxsOf(t, res.Text), []int{5, 6}) {
		t.Fatalf("batch: %q %v", res.Text, err)
	}
	if _, res, err := batchOnce(t, c, 5, 3); err != nil || !slices.Equal(idxsOf(t, res.Text), []int{5, 6, 7}) {
		t.Fatalf("batch after the queues: %q %v", res.Text, err)
	}
	// with the batch queue empty a batch request takes the role queue
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailBatchDuplicate)
	if _, res, _ := batchOnce(t, c, 5, 3); !slices.Equal(idxsOf(t, res.Text), []int{5, 6, 7, 5}) {
		t.Errorf("role queue: %q", res.Text)
	}
	s.FailNextBatch(2, llmtest.FailHTTP500)
	s.Reset()
	if _, res, err := batchOnce(t, c, 5, 3); err != nil || !slices.Equal(idxsOf(t, res.Text), []int{5, 6, 7}) {
		t.Errorf("Reset kept batch failures: %q %v", res.Text, err)
	}
}

// FailTruncated: cut off at the output limit; the client gives up with
// ErrTruncated (OpenAI-compatible without max_tokens: at once).
func TestBatchTruncatedAndRefused(t *testing.T) {
	s, c := setup(t)
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailTruncated)
	if _, _, err := batchOnce(t, c, 0, 5); !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("truncated: %v", err)
	}
	if n := s.Count(llmtest.RoleTranslate); n != 1 {
		t.Errorf("%d requests", n)
	}

	// Claude: truncation retried with a doubled limit; refusal permanent
	s2 := llmtest.New(t)
	ten := 10
	cc, _, _ := claudeClient(t, s2, "claude-sonnet-5-5", model.RoleParams{Batch: &ten})
	lec := ctxbuild.LectureSnapshot{Sentences: sample(t)}
	req := ctxbuild.BuildTranslateBatchRequest(lec, 0, 4, 3)
	ctx := llm.WithJSONSchema(context.Background(), ctxbuild.SchemaTranslateBatch)
	s2.FailNext(llmtest.RoleTranslate, 1, llmtest.FailTruncated)
	res, err := cc.Complete(ctx, model.RoleTranslate, req.System, req.User, true)
	if err != nil || !slices.Equal(idxsOf(t, res.Text), req.Idxs) {
		t.Fatalf("truncated once: %q %v", res.Text, err)
	}
	rs := s2.Requests(llmtest.RoleTranslate)
	if len(rs) != 2 || rs[0].MaxTokens != llm.BaseMaxTokensTranslateBatch || rs[1].MaxTokens != 2*llm.BaseMaxTokensTranslateBatch ||
		rs[0].Schema != compact(t, ctxbuild.SchemaTranslateBatch) || !rs[0].Batch {
		t.Errorf("requests %+v", rs)
	}
	s2.FailNextBatch(1, llmtest.FailRefusal)
	if _, err := cc.Complete(ctx, model.RoleTranslate, req.System, req.User, true); !errors.Is(err, llm.ErrRefusal) {
		t.Errorf("refusal: %v", err)
	}
}
