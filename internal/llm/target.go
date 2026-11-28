// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"context"
	"time"

	"lectureflow/internal/model"
)

// v1.1 additions (SPEC-v1.1 A4, A5, A6). FROZEN by the lead; bodies:
// settings-builder.
//
// Target selection of Client.Complete (the interface is unchanged):
//  1. a target attached with WithTarget (lecture stages attach their own
//     job's snapshot, A4.1: "阶段运行时只读取自己的快照");
//  2. otherwise the current setting of the role (settings.RoleTarget):
//     course-level stages and syllabus_format.
// The provider is looked up by Target.ProviderID in the settings cache; a
// missing provider is a permanent error ("提供商「X」已删除，请重跑并选择使用当前设置").
// provider.type selects the OpenAI-compatible or the Anthropic adapter (A6).
// Temperature comes from Target.Params.Temperature (resolved), else
// model.DefaultTemperature(role). The API pool is acquired per provider id.
// After every response the usage is reported to the job's stats sink
// (sched.ReportUsage) AND recorded in usage_daily through the UsageRecorder.

type targetKey struct{}

// WithTarget makes every Complete under ctx use t instead of the current
// role setting.
func WithTarget(ctx context.Context, t model.LLMSnapshot) context.Context {
	return context.WithValue(ctx, targetKey{}, t)
}

// TargetFrom returns the target attached with WithTarget.
func TargetFrom(ctx context.Context) (model.LLMSnapshot, bool) {
	t, ok := ctx.Value(targetKey{}).(model.LLMSnapshot)
	return t, ok
}

// UsageRecorder receives the usage of every successful response (A10.3:
// usage_daily is updated after each LLM request). Implemented by sched.Pool
// (writes usage_daily for the local day and refreshes the budget cache).
type UsageRecorder interface {
	RecordUsage(ctx context.Context, providerID string, promptTokens, cacheHitTokens, completionTokens int64)
}

// TestResult is the result of "测试连接" (A5.3).
type TestResult struct {
	OK      bool
	Latency time.Duration
	Error   string // the server's error text, verbatim (key never included)
	// ErrorInfo is the coded form of Error (v2 P14.3, llm.probe.* codes);
	// its Detail is Error.
	ErrorInfo *model.ErrorInfo
}

// TestConnection sends one minimal request (max_tokens 1, user "ping") to
// provider p with modelName ("" = the first model a role uses with p, else a
// probed model) and reports latency or the error text (A5.3). It does not go
// through the API pool and records no usage. To OpenAI's own API the limit
// is max_completion_tokens 16; an HTTP 400 refusing the name of the limit is
// answered once by the request with the other name (openai_compat.go).
// OpenAI type: POST {base_url}/chat/completions; Anthropic: POST {base_url}/v1/messages.
func TestConnection(ctx context.Context, p model.Provider, modelName string) TestResult {
	return testConnection(ctx, p, modelName)
}

// ProbeModels lists the provider's models (A5.3): OpenAI type GET
// {base_url}/models, Anthropic GET {base_url}/v1/models (headers x-api-key,
// anthropic-version). Sorted, de-duplicated ids.
func ProbeModels(ctx context.Context, p model.Provider) ([]string, error) {
	return probeModels(ctx, p)
}
