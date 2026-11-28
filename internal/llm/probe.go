// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// "测试连接" and "探测模型" of the settings page (A5.3). Both talk to the
// provider directly: no API pool, no usage, no retries. One exception
// (v3.3.1, openai_compat.go): an HTTP 400 to "测试连接" that refuses the
// name of its output limit is answered once by the request with the other
// name, as the roles' requests are.

var (
	// ProbeTimeout bounds one TestConnection or ProbeModels call.
	ProbeTimeout = 30 * time.Second
	// probeHTTP is the HTTP client of TestConnection and ProbeModels.
	probeHTTP = &http.Client{CheckRedirect: checkRedirect}
)

// maxModelPages bounds the pagination of Anthropic's GET /v1/models.
const maxModelPages = 20

func testConnection(ctx context.Context, p model.Provider, modelName string) TestResult {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if p.APIKey == "" && !settings.IsLocal(p.BaseURL) {
		return failedTest(probeErr(CodeProbeNoAPIKey, map[string]any{"provider": p.Name}, fmt.Sprintf("提供商「%s」没有设置 API key", p.Name)))
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		models, err := probeModels(ctx, p)
		if err != nil {
			return failedTest(probeErr(CodeProbeNoModelNoList, nil, "没有可用于测试的模型名，探测模型也失败了："+err.Error()))
		}
		if len(models) == 0 {
			return failedTest(probeErr(CodeProbeNoModel, nil, "没有可用于测试的模型名：请先为某个角色选择模型，或手动填写模型名"))
		}
		modelName = models[0]
	}
	var body []byte
	var endpoint string
	// again returns the request to send once more after the answer
	// (status, resp) of the first (nil = none).
	again := func(int, []byte) []byte { return nil }
	switch p.Type {
	case model.ProviderAnthropic:
		body, _ = json.Marshal(anthropicRequest{Model: modelName, MaxTokens: 1, Messages: []anthropicMessage{{Role: "user", Content: "ping"}}})
		endpoint = strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
	default:
		// The output limit goes by the name the roles' requests start with:
		// max_completion_tokens to OpenAI's own API, max_tokens elsewhere
		// (openai_compat.go).
		field := defaultTokenField(p.BaseURL)
		build := func() []byte {
			req := map[string]any{
				"model": modelName, "messages": []chatMessage{{Role: "user", Content: "ping"}}, field: probeTokenLimit(field), "stream": false,
			}
			// Same thinking switch as the roles' requests (appendix D2), so the
			// test also shows whether the provider accepts it.
			if th := thinkingDisabled(deepSeekRequests(p.BaseURL, p.BalanceAPI)); th != nil {
				req["thinking"] = th
			}
			b, _ := json.Marshal(req)
			return b
		}
		body = build()
		endpoint = strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
		again = func(status int, resp []byte) []byte {
			if status != http.StatusBadRequest {
				return nil
			}
			if param, code := openAIErrorFields(resp); !refusesField(param, code, field) {
				return nil
			}
			field = otherTokenField(field)
			return build()
		}
	}
	start := time.Now()
	status, resp, err := doProbe(ctx, http.MethodPost, endpoint, p, body)
	if err == nil {
		if body2 := again(status, resp); body2 != nil {
			// The latency reported is the last round trip's.
			start = time.Now()
			status, resp, err = doProbe(ctx, http.MethodPost, endpoint, p, body2)
		}
	}
	latency := time.Since(start)
	// A round trip takes time, but a coarse clock (some Windows machines)
	// can measure a fast local one as zero.
	latency = max(latency, time.Microsecond)
	if err == nil && status != http.StatusOK {
		err = probeErr(CodeProbeHTTP, map[string]any{"status": status}, httpErrorText(status, resp, p.APIKey))
	}
	if err == nil && !json.Valid(resp) {
		err = probeErr(CodeProbeNotJSON, nil, "响应不是 JSON（base_url 是否指向了 API 地址？）："+redactSnippet(resp, p.APIKey))
	}
	if err != nil {
		res := failedTest(err) // doProbe's errors are coded and redacted
		res.Latency = latency
		return res
	}
	return TestResult{OK: true, Latency: latency}
}

func probeModels(ctx context.Context, p model.Provider) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	seen := map[string]bool{}
	base := strings.TrimRight(p.BaseURL, "/")
	if p.Type != model.ProviderAnthropic {
		ids, _, _, err := listModels(ctx, base+"/models", p)
		if err != nil {
			return nil, err
		}
		return sortedUnique(ids, seen), nil
	}
	var all []string
	after := ""
	for page := 0; page < maxModelPages; page++ {
		q := url.Values{"limit": {"1000"}}
		if after != "" {
			q.Set("after_id", after)
		}
		ids, more, last, err := listModels(ctx, base+"/v1/models?"+q.Encode(), p)
		if err != nil {
			return nil, err
		}
		all = append(all, ids...)
		if !more || last == "" || last == after {
			break
		}
		after = last
	}
	return sortedUnique(all, seen), nil
}

// listModels GETs one page of a models list ({"data":[{"id":...}], ...}).
func listModels(ctx context.Context, endpoint string, p model.Provider) (ids []string, hasMore bool, lastID string, err error) {
	status, body, err := doProbe(ctx, http.MethodGet, endpoint, p, nil)
	if err != nil {
		return nil, false, "", err
	}
	if status != http.StatusOK {
		return nil, false, "", probeErr(CodeProbeHTTP, map[string]any{"status": status}, httpErrorText(status, body, p.APIKey))
	}
	var page struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, false, "", probeErr(CodeProbeBadModelList, nil, "模型列表无法解析："+redactSnippet(body, p.APIKey))
	}
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	return ids, page.HasMore, page.LastID, nil
}

// doProbe sends one request. Its errors are coded (llm.probe.*) and
// redacted.
func doProbe(ctx context.Context, method, endpoint string, p model.Provider, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return 0, nil, probeErr(CodeProbeBadBaseURL, nil, redact(fmt.Sprintf("base_url 无效：%v", err), p.APIKey))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	setAuth(req.Header, p.Type, p.APIKey)
	resp, err := probeHTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, probeErr(CodeProbeTimeout, map[string]any{"seconds": ProbeTimeout.Seconds()},
				redact(fmt.Sprintf("请求超时（超过 %s）：%v", ProbeTimeout, err), p.APIKey))
		}
		return 0, nil, probeErr(CodeProbeNetwork, nil, redact(err.Error(), p.APIKey))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, b, probeErr(CodeProbeNetwork, nil, redact(err.Error(), p.APIKey))
	}
	return resp.StatusCode, b, nil
}

// httpErrorText is "HTTP 401：{the server's body}" with any echoed key redacted.
func httpErrorText(status int, body []byte, key string) string {
	s := redactSnippet(body, key)
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d：%s", status, s)
}

func sortedUnique(ids []string, seen map[string]bool) []string {
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
