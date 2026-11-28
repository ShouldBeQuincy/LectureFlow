// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package llm

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"

	"lectureflow/internal/settings"
)

// OpenAI's own API, and the parameters its current models refuse (v3.3.1).
//
// api.openai.com refuses max_tokens for its current models ("Unsupported
// parameter: 'max_tokens' is not supported with this model. Use
// 'max_completion_tokens' instead.") and takes max_completion_tokens for
// its chat models, so a request to it names the output limit
// max_completion_tokens. Every other OpenAI-compatible provider keeps
// max_tokens: DeepSeek and most others know only that name, and their
// requests stay byte for byte what they were.
//
// An HTTP 400 in OpenAI's error format that names the sent name of the
// output limit as an unsupported parameter (a relay in front of OpenAI's
// models, or a model that wants the other name) is answered once by the same
// request with the other name; one that names the temperature as
// unsupported (models that take only their default) once by the request
// without it. No backoff wait: the server is fine. What was learned holds
// for that provider, base URL and model until the app restarts, like a
// refused output_config.format (HTTPClient.noFormat). Any other 400 is the
// call's error, as before (SPEC §12).
//
// DeepSeek's request fields (the thinking switch, reasoning_effort and the
// janitor's output limit; appendix D2) never go to OpenAI's own API, even
// for a provider whose balance query says DeepSeek (deepSeekRequests).

const (
	fieldMaxTokens           = "max_tokens"
	fieldMaxCompletionTokens = "max_completion_tokens"
	fieldTemperature         = "temperature"

	// error.code of OpenAI's answer to a parameter the model does not take
	// at all, or not with the value sent.
	codeUnsupportedParameter = "unsupported_parameter"
	codeUnsupportedValue     = "unsupported_value"

	// probeCompletionTokens: the max_completion_tokens of "测试连接". Not 1:
	// a margin for models that spend tokens before they answer, or refuse a
	// very small limit. The probe only needs an HTTP 200 and costs at most
	// these few output tokens.
	probeCompletionTokens = 16
)

// isOpenAIOfficial reports whether baseURL is OpenAI's own API:
// api.openai.com, or one of its regional hosts (eu.api.openai.com, ...).
// Proxies and relays, whatever models they serve, are not.
func isOpenAIOfficial(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return h == "api.openai.com" || strings.HasSuffix(h, ".api.openai.com")
}

// deepSeekRequests reports whether requests to a provider are shaped for
// DeepSeek's API (the thinking switch, reasoning_effort, the janitor's
// output limit): settings.IsDeepSeek (its host, or its balance query), but
// never OpenAI's own API. A DeepSeek provider edited to OpenAI's address
// keeps its balance query (the settings dialog fills that in for a new
// provider only), and its requests are not DeepSeek's for that.
func deepSeekRequests(baseURL, balanceAPI string) bool {
	return settings.IsDeepSeek(baseURL, balanceAPI) && !isOpenAIOfficial(baseURL)
}

// defaultTokenField is the name the output limit of a request to baseURL
// goes by before any refusal was learned.
func defaultTokenField(baseURL string) string {
	if isOpenAIOfficial(baseURL) {
		return fieldMaxCompletionTokens
	}
	return fieldMaxTokens
}

func otherTokenField(field string) string {
	if field == fieldMaxCompletionTokens {
		return fieldMaxTokens
	}
	return fieldMaxCompletionTokens
}

// compatKey is what a learned refusal is remembered by: the provider, its
// base URL (a provider edited to another address asks again) and the model.
func compatKey(tg target) string {
	return tg.providerID + "\x00" + tg.baseURL + "\x00" + tg.model
}

// tokenField is the name the output limit of a request to tg goes by: the
// one learned for its provider and model, else defaultTokenField.
func (c *HTTPClient) tokenField(tg target) string {
	if f, ok := c.tokenFields.Load(compatKey(tg)); ok {
		return f.(string)
	}
	return defaultTokenField(tg.baseURL)
}

// learnTokenField remembers that tg's provider and model refused the name
// refused: their requests go by field from now on. Logged the first time.
func (c *HTTPClient) learnTokenField(tg target, role, refused, field string) {
	if _, known := c.tokenFields.Swap(compatKey(tg), field); !known {
		slog.Warn("llm: the provider refused the name of the output limit; requests to this model use the other name until the app restarts",
			"provider", tg.providerName, "model", tg.model, "role", role, "refused", refused, "now", field)
	}
}

// temperatureRefused reports that tg's provider and model refused a
// temperature: their requests go without one (the model's default).
func (c *HTTPClient) temperatureRefused(tg target) bool {
	_, refused := c.noTemperature.Load(compatKey(tg))
	return refused
}

// learnNoTemperature remembers that tg's provider and model refused the
// temperature. Logged the first time.
func (c *HTTPClient) learnNoTemperature(tg target, role string) {
	if _, known := c.noTemperature.LoadOrStore(compatKey(tg), true); !known {
		slog.Warn("llm: the model refused the temperature; requests to it go without one (its default applies) until the app restarts",
			"provider", tg.providerName, "model", tg.model, "role", role, "temperature", tg.temperature)
	}
}

// openAIErrorFields returns error.param and error.code of an answer in
// OpenAI's error format ({"error": {"param": ..., "code": ...}}): "" for one
// that is absent, null or not a string, and for a body in any other format.
func openAIErrorFields(body []byte) (param, code string) {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return "", ""
	}
	var e struct {
		Param json.RawMessage `json:"param"`
		Code  json.RawMessage `json:"code"`
	}
	if json.Unmarshal(env.Error, &e) != nil {
		return "", ""
	}
	return rawString(e.Param), rawString(e.Code) // stream.go
}

// refusesField reports an error that names field as a parameter the model
// does not take.
func refusesField(param, code, field string) bool {
	return code == codeUnsupportedParameter && param == field
}

// refusesTemperature reports an error that names the temperature as not
// taken at all, or not with the value sent ("Only the default (1) value is
// supported"). A value out of range is another code and stays the call's
// error.
func refusesTemperature(param, code string) bool {
	return param == fieldTemperature && (code == codeUnsupportedParameter || code == codeUnsupportedValue)
}

// probeTokenLimit is the output limit "测试连接" sends under field.
func probeTokenLimit(field string) int {
	if field == fieldMaxCompletionTokens {
		return probeCompletionTokens
	}
	return 1
}
