// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llmtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// v3.2.x P1: structured output, the level off of the two models that think
// without a thinking field, refusals, and answers with second thoughts —
// the real HTTP client against the fake server.

func translateReq() ctxbuild.Request {
	lec := ctxbuild.LectureSnapshot{LectureID: "L", Sentences: []ctxbuild.Sentence{{Idx: 0, Text: "A cache line.", ASRText: "A cache line."}}}
	r := ctxbuild.BuildTranslateRequest(lec, 0, 3)
	return ctxbuild.Request{System: r.System, User: r.User}
}

// claudeClient is settingsClient with every role on modelName.
func claudeClient(t *testing.T, s *llmtest.Server, modelName string, params model.RoleParams) (*llm.HTTPClient, *settings.Service, model.Provider) {
	t.Helper()
	c, set, p := settingsClient(t, s, model.ProviderAnthropic)
	for _, role := range model.Roles {
		if err := set.SetRole(context.Background(), role, p.ID, modelName, params); err != nil {
			t.Fatal(err)
		}
	}
	return c, set, p
}

func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return b.String()
}

// Every schema of ctxbuild passes the rules of structured output.
func TestAnswerSchemasAreValid(t *testing.T) {
	if len(ctxbuild.AnswerSchemas) != 6 {
		t.Fatalf("%d schemas", len(ctxbuild.AnswerSchemas))
	}
	for name, schema := range ctxbuild.AnswerSchemas {
		if msg := llmtest.CheckSchema([]byte(schema)); msg != "" {
			t.Errorf("%s: %s", name, msg)
		}
	}
	// the checker itself
	for _, bad := range []string{
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,                                                                    // no additionalProperties
		`{"type":"object","properties":{"a":{"type":"string"}},"required":[],"additionalProperties":false}`,                                          // a not required
		`{"type":"object","properties":{"a":{"type":"string","maxLength":3}},"required":["a"],"additionalProperties":false}`,                         // length constraint
		`{"type":"array","items":{"type":"object","properties":{}}}`,                                                                                 // nested object without the rule
		`{"type":"object","properties":{"a":{"type":"integer","minimum":0}},"required":["a"],"additionalProperties":false}`,                          // range constraint
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":{"type":"string"}}`,                           // not false
		`{"type":"object","properties":{"a":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["a"],"additionalProperties":false}`, // array size
	} {
		if llmtest.CheckSchema([]byte(bad)) == "" {
			t.Errorf("accepted: %s", bad)
		}
	}
}

// A JSON request with a schema to a Claude model carries
// output_config.format, in the same object as the effort; the server checks
// the schema. Nothing else gets one: no schema in ctx, not JSON mode, a
// model name that is not Claude's, an OpenAI-compatible provider.
func TestStructuredOutputRequest(t *testing.T) {
	s := llmtest.New(t)
	high := model.ThinkingHigh
	c, set, p := claudeClient(t, s, "claude-sonnet-5-5", model.RoleParams{Thinking: &high})
	ctx := context.Background()
	sctx := llm.WithJSONSchema(ctx, ctxbuild.SchemaTranslate)
	req := translateReq()

	res, err := c.Complete(sctx, model.RoleTranslate, req.System, req.User, true)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Idx *int    `json:"idx"`
		Zh  *string `json:"zh"`
	}
	if err := llm.ParseJSON(res.Text, &out); err != nil || out.Idx == nil || out.Zh == nil {
		t.Fatalf("answer %q: %v", res.Text, err)
	}
	got := s.Requests(llmtest.RoleTranslate)
	if len(got) != 1 || got[0].Schema != compact(t, ctxbuild.SchemaTranslate) || got[0].Effort != "high" || got[0].Thinking != "adaptive" ||
		!got[0].JSONMode || got[0].RawUser != req.User+llm.JSONInstruction {
		t.Fatalf("request %+v", got)
	}

	// thinking off on this model: between_tools, the schema alone in output_config
	off := model.ThinkingOff
	if err := set.SetRole(ctx, model.RoleTranslate, p.ID, "claude-sonnet-5-5", model.RoleParams{Thinking: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(sctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if r := s.Requests(llmtest.RoleTranslate)[1]; r.Schema == "" || r.Effort != "" || r.Thinking != "between_tools" {
		t.Fatalf("off request %+v", r)
	}

	s.Reset()
	// no schema in ctx; not JSON mode
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	sum := ctxbuild.SummaryRequest(ctxbuild.LectureSnapshot{LectureID: "L", Sentences: []ctxbuild.Sentence{{Idx: 0, Text: "A cache line."}}})
	if _, err := c.Complete(sctx, model.RoleSummary, sum.System, sum.User, false); err != nil {
		t.Fatal(err)
	}
	// a model name that is not Claude's (an Anthropic-compatible endpoint)
	if err := set.SetRole(ctx, model.RoleTranslate, p.ID, "deepseek-flash", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(sctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	for i, r := range s.Requests("") {
		if r.Schema != "" {
			t.Errorf("request %d (%s, %s) has a schema", i, r.Role, r.Model)
		}
	}
	if n := len(s.Requests("")); n != 3 {
		t.Fatalf("%d requests", n)
	}

	// an OpenAI-compatible provider: response_format json_object as before
	s2 := llmtest.New(t)
	c2, _, _ := settingsClient(t, s2, model.ProviderOpenAI)
	if _, err := c2.Complete(sctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if r := s2.Requests(llmtest.RoleTranslate)[0]; r.Schema != "" || !r.JSONMode || r.API != model.ProviderOpenAI {
		t.Fatalf("openai request %+v", r)
	}
}

// A model that does not know output_config.format answers HTTP 400: the
// call is sent once more without the schema, and the client remembers it
// for that provider and model (other models still get the schema).
func TestStructuredOutputFallback(t *testing.T) {
	s := llmtest.New(t)
	c, set, p := claudeClient(t, s, "claude-opus-4-7", model.RoleParams{})
	ctx := llm.WithJSONSchema(context.Background(), ctxbuild.SchemaTranslate)
	req := translateReq()
	s.RejectFormat(true)

	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	// The refused request never reached the role handler: one recorded
	// request, without a schema.
	got := s.Requests(llmtest.RoleTranslate)
	if len(got) != 1 || got[0].Schema != "" || !got[0].JSONMode {
		t.Fatalf("after the fallback: %+v", got)
	}
	// remembered: the next call goes without the schema at once (the server
	// would accept it now, and still gets none)
	s.RejectFormat(false)
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Requests(llmtest.RoleTranslate); len(got) != 2 || got[1].Schema != "" {
		t.Fatalf("remembered: %+v", got)
	}
	// another model of the same provider is asked with the schema
	if err := set.SetRole(context.Background(), model.RoleTranslate, p.ID, "claude-opus-5-5", model.RoleParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Requests(llmtest.RoleTranslate); len(got) != 3 || got[2].Schema == "" || got[2].Model != "claude-opus-5-5" {
		t.Fatalf("other model: %+v", got)
	}
}

// A 400 that is not about the schema: the call fails after its one resend
// (two requests, not more) and nothing is remembered.
func TestStructuredOutputFallbackOnlyOnce(t *testing.T) {
	s := llmtest.New(t)
	c, _, _ := claudeClient(t, s, "claude-sonnet-5-5", model.RoleParams{})
	ctx := llm.WithJSONSchema(context.Background(), ctxbuild.SchemaTranslate)
	req := translateReq()
	s.SetError(400, "messages: something else is wrong")
	_, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true)
	var ce *model.CodedError
	if err == nil || !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != llm.CodeRejected {
		t.Fatalf("err %v", err)
	}
	s.SetError(0, "")
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Requests(llmtest.RoleTranslate); len(got) != 1 || got[0].Schema == "" {
		t.Fatalf("the schema was dropped after an unrelated 400: %+v", got)
	}
}

// Thinking off: claude-sonnet-5-5 gets thinking between_tools and no
// effort; claude-opus-5-5, Fable and Mythos effort low and no thinking field
// (dated ids too); claude-opus-5, claude-sonnet-5 and claude-haiku-5-5
// thinking disabled and no effort (C91); every other model gets neither, as
// before. The fake server refuses "disabled" where the API does.
func TestThinkingOffOnAlwaysThinkingModels(t *testing.T) {
	sum := ctxbuild.SummaryRequest(ctxbuild.LectureSnapshot{LectureID: "L", Sentences: []ctxbuild.Sentence{{Idx: 0, Text: "A cache line."}}})
	for _, tc := range []struct{ model, thinking, effort string }{
		{"claude-sonnet-5-5", "between_tools", ""},
		{"claude-sonnet-5-5-20260901", "between_tools", ""},
		{"claude-opus-5-5", "", "low"},
		{"claude-opus-5-5-20260901", "", "low"},
		{"claude-fable-5-2", "", "low"},
		{"claude-mythos-5-2", "", "low"},
		{"claude-opus-5", "disabled", ""},
		{"claude-opus-5-20260301", "disabled", ""},
		{"claude-sonnet-5", "disabled", ""},
		{"claude-sonnet-5-50", "", ""}, // another id that only starts alike
		{"claude-haiku-5-5", "disabled", ""},
		{"claude-opus-4-8", "", ""},
		{"claude-haiku-4-5", "", ""},
		{"deepseek-flash", "", ""},
	} {
		for _, params := range []model.RoleParams{{}, {Thinking: ptr(model.ThinkingOff)}} {
			s := llmtest.New(t)
			c, _, _ := claudeClient(t, s, tc.model, params)
			if _, err := c.Complete(context.Background(), model.RoleSummary, sum.System, sum.User, false); err != nil {
				t.Fatalf("%s: %v", tc.model, err)
			}
			if r := s.Requests(llmtest.RoleSummary)[0]; r.Thinking != tc.thinking || r.Effort != tc.effort || r.Schema != "" {
				t.Errorf("%s (%+v): thinking %q effort %q", tc.model, params, r.Thinking, r.Effort)
			}
		}
	}
	// With a level, both models think adaptively with that effort.
	for _, m := range []string{"claude-sonnet-5-5", "claude-opus-5-5"} {
		s := llmtest.New(t)
		c, _, _ := claudeClient(t, s, m, model.RoleParams{Thinking: ptr(model.ThinkingLow)})
		if _, err := c.Complete(context.Background(), model.RoleSummary, sum.System, sum.User, false); err != nil {
			t.Fatal(err)
		}
		if r := s.Requests(llmtest.RoleSummary)[0]; r.Thinking != "adaptive" || r.Effort != "low" {
			t.Errorf("%s low: thinking %q effort %q", m, r.Thinking, r.Effort)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// stop_reason refusal: a permanent, coded error with the category after
// one request; never handed to the stage as an answer.
func TestRefusalIsPermanent(t *testing.T) {
	s := llmtest.New(t)
	c, _, _ := claudeClient(t, s, "claude-sonnet-5-5", model.RoleParams{})
	ctx := llm.WithJSONSchema(context.Background(), ctxbuild.SchemaTranslate)
	req := translateReq()
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailRefusal)
	u := &llm.Usage{}
	_, err := c.Complete(llm.WithUsage(ctx, u), model.RoleTranslate, req.System, req.User, true)
	var ce *model.CodedError
	if !errors.Is(err, llm.ErrRefusal) || !sched.IsPermanent(err) || !errors.As(err, &ce) || ce.Info.Code != llm.CodeRefusal ||
		ce.Info.Params["category"] != llmtest.RefusalCategory || ce.Info.Params["model"] != "claude-sonnet-5-5" || errors.Is(err, llm.ErrBadJSON) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(err.Error(), "拒绝") || !strings.Contains(err.Error(), llmtest.RefusalCategory) {
		t.Errorf("text %q", err)
	}
	if n := s.Count(llmtest.RoleTranslate); n != 1 {
		t.Fatalf("%d requests for a refusal", n)
	}
	if tot := u.Totals(); tot.Requests != 1 || tot.PromptTokens == 0 {
		t.Error("the usage of the refused request is not counted")
	}
	// the next request is answered as usual
	if _, err := c.Complete(ctx, model.RoleTranslate, req.System, req.User, true); err != nil {
		t.Fatal(err)
	}
}

// An answer with second thoughts through the real client: ParseJSON refuses
// two different JSON values (ErrSecondJSON) and accepts text after one.
func TestSecondThoughtsAnswers(t *testing.T) {
	s := llmtest.New(t)
	c, _, _ := settingsClient(t, s, model.ProviderOpenAI)
	req := translateReq()
	var out struct {
		Idx *int    `json:"idx"`
		Zh  *string `json:"zh"`
	}
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailSecondJSON)
	res, err := c.Complete(context.Background(), model.RoleTranslate, req.System, req.User, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ParseJSON(res.Text, &out); !errors.Is(err, llm.ErrSecondJSON) || !errors.Is(err, llm.ErrBadJSON) {
		t.Fatalf("second JSON: %v (%q)", err, res.Text)
	}
	s.FailNext(llmtest.RoleTranslate, 1, llmtest.FailTrailingText)
	res, err = c.Complete(context.Background(), model.RoleTranslate, req.System, req.User, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ParseJSON(res.Text, &out); err != nil || out.Zh == nil || !strings.HasSuffix(res.Text, llmtest.TrailingText) {
		t.Fatalf("trailing text: %v (%q)", err, res.Text)
	}
}
