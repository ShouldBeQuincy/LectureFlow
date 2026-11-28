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
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/settings"
)

// Transport defaults of SPEC §12.
var (
	// DefaultBackoff are the waits before retry 1..4 (429, 5xx, network
	// errors, timeouts). Retry-After overrides a wait when present.
	DefaultBackoff = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	// DefaultTimeout is the timeout of one HTTP attempt.
	DefaultTimeout = 120 * time.Second
	// MaxRetryAfter caps a server-provided Retry-After.
	MaxRetryAfter = 5 * time.Minute
)

// Temperature returns the sampling temperature of a role (SPEC §12).
func Temperature(role string) float64 { return model.DefaultTemperature(role) }

// RoleProvider returns the provider name and model configured for role
// in the legacy config (syllabus_format falls back to the glossary role).
func RoleProvider(cfg *config.Config, role string) (provider, model string) {
	r, ok := cfg.LLM.Roles[role]
	if !ok && role == RoleSyllabusFormat {
		r = cfg.LLM.Roles[config.RoleGlossary]
	}
	return r.Provider, r.Model
}

// RoleSyllabusFormat is the v1.1 A8.3 role (alias of the model constant).
const RoleSyllabusFormat = model.RoleSyllabusFormat

// HTTPClient implements Client for OpenAI-compatible providers (SPEC §12)
// and Anthropic providers (v1.1 A6). Fields other than those set by the
// constructors may be changed before first use.
//
// Two modes:
//   - New (legacy): providers and roles come from config.toml ([llm]);
//     provider ids are the config keys. Every provider is OpenAI-compatible.
//   - NewWithSettings (v1.1): providers and roles come from the settings
//     service; provider.type selects the adapter.
//
// In both modes a target attached with WithTarget wins over the role's
// current setting (target.go); in legacy mode its ProviderID is looked up
// among the config keys.
type HTTPClient struct {
	cfg    *config.Config    // legacy mode; in settings mode the startup config (log level, data dir)
	set    *settings.Service // settings mode; nil in legacy mode
	pool   sched.APIPool
	usage  UsageRecorder
	http   *http.Client
	logDir string

	// Backoff are the waits before each retry; len(Backoff) is the maximum
	// number of retries (default DefaultBackoff: 4 retries, 5 attempts).
	Backoff []time.Duration
	// Timeout of one HTTP attempt (default DefaultTimeout).
	Timeout time.Duration
	// Sleep waits d or until ctx is done (tests inject a fast one).
	Sleep func(ctx context.Context, d time.Duration) error
	// AnthropicMaxTokens, when > 0, is the base for the answer of every
	// Anthropic request (the thinking headroom is added; a role's own
	// max_tokens still wins); 0 (the default) uses the per-role bases
	// (maxtokens.go: BaseMaxTokens, AnthropicMaxTokensFor).
	AnthropicMaxTokens int

	logMu sync.Mutex
	// calls is the call log (calllog.go): always written, never a prompt or
	// an answer.
	calls *callLog
	// thinkingWarned: the thinking levels already logged as sent off
	// (thinking.go, thinkingFor).
	thinkingWarned sync.Map
	// limitWarned: the notes about output limits already logged
	// (maxtokens.go, warnLimit).
	limitWarned sync.Map
	// newIdleTimer starts the idle timeout of a streamed attempt (stream.go;
	// time.AfterFunc, tests inject a timer they fire themselves).
	newIdleTimer func(d time.Duration, f func()) idleTimer
	// drained, when set (tests), is called when drainBody is done with a
	// stream's response; eof reports that it was read to its end.
	drained func(eof bool)
	// noFormat: the (provider id, model, schema) triples that answered HTTP
	// 400 to a request with output_config.format and then took the same
	// request without it (anthropic.go, structured output). Not persisted: a
	// new process asks again.
	noFormat sync.Map
	// tokenFields: the name of the output limit an OpenAI-compatible
	// provider and model took after an HTTP 400 refused the other one;
	// noTemperature: the ones that refused a temperature (openai_compat.go,
	// v3.3.1). Not persisted: a new process asks again.
	tokenFields   sync.Map
	noTemperature sync.Map
	// limits: the account limits Anthropic answers reported, per provider
	// and model (ratelimits.go, T-PERF-03). Not persisted.
	limits rateLimitStore
}

var _ Client = (*HTTPClient)(nil)

// New returns a legacy-mode client for cfg. pool is acquired around every
// HTTP attempt (nil = no limit, tests only); when pool also implements
// UsageRecorder (sched.Pool does) every response is recorded there too.
// logDir is where debug logs go ("" = {data_dir}/logs/llm). hc nil = a
// default client with a connection pool sized for parallel requests.
func New(cfg *config.Config, pool sched.APIPool, logDir string, hc *http.Client) *HTTPClient {
	c := baseClient(cfg, pool, logDir, hc)
	if ur, ok := pool.(UsageRecorder); ok {
		c.usage = ur
	}
	return c
}

// NewWithSettings returns a v1.1 client: providers, keys and role settings
// come from set (read from its cache on every request, so edits on the
// settings page apply to the next request). pool is acquired per provider
// id around every HTTP attempt (nil = no limit, tests only); usage receives
// every successful response for usage_daily (nil = not recorded). logDir ""
// = {data_dir}/logs/llm of the startup config.
func NewWithSettings(set *settings.Service, pool sched.APIPool, usage UsageRecorder, logDir string, hc *http.Client) *HTTPClient {
	c := baseClient(set.Config(), pool, logDir, hc)
	c.set = set
	c.usage = usage
	return c
}

func baseClient(cfg *config.Config, pool sched.APIPool, logDir string, hc *http.Client) *HTTPClient {
	if cfg == nil {
		cfg = config.Default()
	}
	if logDir == "" {
		logDir = filepath.Join(cfg.DataDir, "logs", "llm")
	}
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		// Room for the connections of a provider's whole concurrency
		// (perf-llm: DeepSeek 64 by default, up to settings.MaxConcurrencyLimit),
		// so a burst does not close and reopen TLS connections.
		tr.MaxIdleConns = 0
		tr.MaxIdleConnsPerHost = 256
		hc = &http.Client{Transport: tr, CheckRedirect: checkRedirect}
	} else if hc.CheckRedirect == nil {
		cp := *hc // the caller's client is left as it is
		cp.CheckRedirect = checkRedirect
		hc = &cp
	}
	return &HTTPClient{
		cfg:     cfg,
		pool:    pool,
		http:    hc,
		logDir:  logDir,
		calls:   newCallLog(logDir),
		Backoff: DefaultBackoff,
		Timeout: DefaultTimeout,
		Sleep:   sleepCtx,

		newIdleTimer: afterFunc,
	}
}

// checkRedirect is the redirect policy of the LLM clients (sec2-app): a
// redirect is followed only to the same host (scheme://host:port of the
// previous request; http → https on the same host name is allowed), so the
// API key never goes to another host — net/http keeps custom headers such
// as x-api-key on every redirect and Authorization on redirects within the
// same domain. Any other redirect is not followed: the 3xx response comes
// back as the answer (a failed request that names the status).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return http.ErrUseLastResponse
	}
	if !sameOrigin(via[len(via)-1].URL, req.URL) {
		return http.ErrUseLastResponse
	}
	return nil
}

// CheckRedirect is checkRedirect for the other clients that send a
// provider's key: the balance query (internal/resources; AUD1 I-2). Kept
// here rather than in a package of its own: the rule and its origin
// comparison (db.HostKey) stay one, and resources already depends on
// this package's dependencies.
func CheckRedirect(req *http.Request, via []*http.Request) error { return checkRedirect(req, via) }

// sameOrigin reports whether a redirect from prev to next stays on the same
// origin; an upgrade from http to https on the same host name counts. Host
// names are compared by db.HostKey, the rule of the origin a key is bound
// to (SEC10): ASCII letters lower-cased only, a host with a byte outside
// ASCII equal only to the same bytes (strings.EqualFold took "ς" for "σ",
// two hosts once net/http maps them by IDNA).
func sameOrigin(prev, next *url.URL) bool {
	ph, _ := db.HostKey(prev.Hostname())
	nh, _ := db.HostKey(next.Hostname())
	if ph != nh {
		return false
	}
	ps, ns := strings.ToLower(prev.Scheme), strings.ToLower(next.Scheme)
	switch {
	case ps == ns:
		return portOf(prev) == portOf(next)
	case ps == "http" && ns == "https":
		return true
	}
	return false
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// ErrTruncated: an answer was cut off at its output limit (Anthropic
// stop_reason "max_tokens", OpenAI-compatible finish_reason "length") and
// could not be completed: still truncated after the retries of the
// truncation ladder (TruncationRetries), or truncated at a limit the request
// did not set (so it cannot be raised). Complete returns it wrapped with sched.Permanent and
// a Chinese message ("输出被截断"); its usage is recorded as usual.
var ErrTruncated = errors.New("output truncated at max_tokens")

// ErrRefusal: the provider declined the request (Anthropic stop_reason
// "refusal": its safety classifiers stopped the answer; OpenAI-compatible
// finish_reason "content_filter": its content filter did). Sending the same
// request again gives the same result, so Complete returns it at once,
// wrapped with sched.Permanent and coded CodeRefusal; a stage must not count
// it as an unparsable answer. Its usage is recorded as usual.
var ErrRefusal = errors.New("the provider declined the request")

// TruncationRetries is how often a truncated answer is retried (v3.2.x
// P1-c, the user's Q5a, amending appendix D2's single retry). Each retry
// takes the next rung of the truncation ladder:
//
//   - the answer has no text and thinking is on above the lowest level: the
//     thinking level goes one down (max -> high -> low), max_tokens stays
//     (the output limit went to thinking; a lower level leaves room for the
//     answer);
//   - otherwise (text was written, or the level is already low or off):
//     max_tokens doubles, at most to the model's output limit.
//
// The level off is never a rung: claude-opus-5-5 (Fable, Mythos) cannot switch thinking off
// (its off is effort low, the same effort as low) and claude-sonnet-5-5's
// off (between_tools) is no extended thinking at all; the user's choice to
// think is kept. A ladder that cannot go on (no max_tokens sent, the output
// limit reached) ends at once. The final error lists every rung. A caller
// that asks for less itself skips the ladder (WithNoTruncationRetry: the
// glossary_review stage, which splits its batch; T-PERF-07).
const TruncationRetries = 2

// MaxAttemptTimeout caps the timeout of one HTTP attempt, whatever
// max_tokens asks for (SEC1 L3): a non-streamed request that runs longer is
// most likely stuck, and it holds an API pool slot meanwhile. Streamed
// requests (v3.2.x P1-c, stream.go) add an idle timeout; this stays as the
// cap of the whole attempt. Cancellation is not affected.
var MaxAttemptTimeout = 30 * time.Minute

// LongAttemptTimeout: a request that is not streamed and would need more
// than this for one attempt, and whose timeout MaxAttemptTimeout cut short,
// is not sent again when that attempt times out (review D7): the same
// request would most likely time out again, each time holding an API pool
// slot for the whole cap (4 retries of 30 minutes). It is the default of
// MaxAttemptTimeout; tests that lower the cap to make attempts short keep
// their retries.
const LongAttemptTimeout = 30 * time.Minute

// lowerThinking returns the thinking level one rung below level on the
// truncation ladder (max -> high -> low); false at low, off and any other
// value.
func lowerThinking(level string) (string, bool) {
	switch level {
	case model.ThinkingMax:
		return model.ThinkingHigh, true
	case model.ThinkingHigh:
		return model.ThinkingLow, true
	}
	return "", false
}

// truncStep is one rung of the truncation ladder: the level and the
// max_tokens an attempt was sent with.
type truncStep struct {
	level     string
	maxTokens int
}

// DeepSeekJanitorMaxTokens is max_tokens of glossary_janitor requests to
// DeepSeek. Without it DeepSeek stops at its default of 8K output tokens in
// non-thinking mode; the documented upper limit is 384K (NOTES.md, v2
// llm-builder). Other roles and other OpenAI-compatible providers send no
// max_tokens.
const DeepSeekJanitorMaxTokens = 16000

// openAIMaxTokens returns max_tokens of an OpenAI-compatible request (0 =
// the field is not sent).
func openAIMaxTokens(role string, deepseek bool) int {
	if deepseek && role == model.RoleGlossaryJanitor {
		return DeepSeekJanitorMaxTokens
	}
	return 0
}

// thinkingParam is DeepSeek's thinking switch of the OpenAI-compatible
// API: {"thinking": {"type": "enabled" | "disabled"}} (thinking is on by
// default). Appendix D2: a role runs with thinking off unless the user
// turns its "深度思考" on (thinking.go); settings.IsDeepSeek decides which
// providers are DeepSeek.
type thinkingParam struct {
	Type string `json:"type"`
}

// thinkingDisabled returns the thinking field of a request to the provider:
// disabled for DeepSeek, absent (nil) for every other provider.
func thinkingDisabled(deepseek bool) *thinkingParam {
	if !deepseek {
		return nil
	}
	return &thinkingParam{Type: "disabled"}
}

// deepSeekThinking returns the thinking switch and reasoning_effort of an
// OpenAI-compatible request with thinking level (thinking.go): enabled with
// the level when thinking is on (DeepSeek only), else thinkingDisabled.
func deepSeekThinking(deepseek bool, level string) (*thinkingParam, string) {
	if deepseek && ThinkingBudget(level) > 0 {
		return &thinkingParam{Type: "enabled"}, level
	}
	return thinkingDisabled(deepseek), ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Temperature is always sent, except with DeepSeek thinking on (ignored
	// in thinking mode) and to a model that refused it (openai_compat.go);
	// the field order is unchanged, so a request without thinking is byte
	// for byte what it was.
	Temperature    *float64          `json:"temperature,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Stream         bool              `json:"stream"`
	Thinking       *thinkingParam    `json:"thinking,omitempty"`   // DeepSeek only (appendix D2, thinking.go)
	MaxTokens      int               `json:"max_tokens,omitempty"` // 0 = not sent (openAIMaxTokens)
	// ReasoningEffort is DeepSeek's thinking level (thinking.go); "" = not sent.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// MaxCompletionTokens is the output limit under the name OpenAI's own
	// API takes (openai_compat.go; 0 = not sent). A request carries this or
	// MaxTokens, never both. Last, so a request with max_tokens is byte for
	// byte what it was.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int64  `json:"prompt_tokens"`
		CompletionTokens     int64  `json:"completion_tokens"`
		PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// failure kinds of one attempt
type failKind int

const (
	failNetwork   failKind = iota // transport error (retry)
	failTimeout                   // attempt timeout (retry)
	failStatus                    // HTTP status (retry for 429 / 5xx, incl. Anthropic 529)
	failEnvelope                  // 200 with an unreadable body (retry)
	failPool                      // sched.APIPool refused (returned as is)
	failTruncated                 // stop_reason max_tokens / finish_reason length (see Complete)
	failRefusal                   // stop_reason refusal / finish_reason content_filter (never retried, see ErrRefusal)
	failIdle                      // streamed: no event within the idle timeout (retry; stream.go)
	failTimeLimit                 // streamed: the attempt timeout ran out while events kept coming (never retried; SEC4 M4)
	failResource                  // finish_reason insufficient_system_resource (DeepSeek; retry, billed)
)

type attemptErr struct {
	kind       failKind
	status     int
	body       string
	retryAfter time.Duration // > 0 when the server sent Retry-After
	hasRA      bool
	err        error
	// res: failTruncated, failRefusal: the answer with its usage; any other
	// failure with usage set: the usage a stream reported before it failed
	// (tokens only, SEC4 M1)
	res      Result
	usage    bool
	category string // failRefusal: stop_details.category
	// permanent: not retried whatever the kind (a stream too long to read,
	// SEC4 L2 / L3; failTimeLimit)
	permanent bool
	// inStream: an error event of a stream (HTTP 200), not a status of the
	// response: it says nothing about the request's schema
	inStream bool
	// deadline: failTimeout because the attempt's own timeout ran out (not
	// a connect or TLS handshake timeout before it; WithNoDeadlineRetry)
	deadline bool
	// errParam, errCode: error.param and error.code of an HTTP 400 to an
	// OpenAI-compatible request, read from the whole body
	// (openai_compat.go; "" when absent)
	errParam, errCode string
}

// billed reports a failed attempt whose usage counts (Complete records it):
// a truncated or declined answer, or a stream that reported usage before it
// failed.
func (e *attemptErr) billed() bool {
	return e.kind == failTruncated || e.kind == failRefusal || e.usage
}

// throttled reports a throttling answer (perf-llm): 429 Too Many Requests,
// 503 Service Unavailable (DeepSeek's "server overloaded") and Anthropic's
// 529 Overloaded. The API pool lowers the provider's concurrency on it.
func (e *attemptErr) throttled() bool {
	return e.kind == failStatus && (e.status == http.StatusTooManyRequests || e.status == http.StatusServiceUnavailable || e.status == StatusOverloaded)
}

func (e *attemptErr) retryable() bool {
	if e.permanent || e.kind == failTimeLimit {
		return false
	}
	if e.kind == failStatus {
		return e.status == http.StatusTooManyRequests || e.status >= 500
	}
	return true
}

// target is a resolved request target: the provider (with its key) and
// the model, temperature and thinking level to use.
type target struct {
	providerID   string // API pool and usage_daily key
	providerName string // for messages
	ptype        string // model.ProviderOpenAI | model.ProviderAnthropic
	baseURL      string
	key          string
	model        string
	temperature  float64
	deepseek     bool   // DeepSeek's API: thinking is switched off unless the role turns it on (appendix D2)
	thinking     string // model.Thinking*: the level sent (off when the provider does not support it)
	maxTokens    int    // RoleParams.MaxTokens, the user's output limit (0 = automatic; maxtokens.go)
	batch        int    // RoleParams.Batch (translate; 0 = not set)
}

// limitsOf returns the user's output limit and the batch size of params
// (0 when unset or not positive).
func limitsOf(p model.RoleParams) (maxTokens, batch int) {
	if p.MaxTokens != nil && *p.MaxTokens > 0 {
		maxTokens = *p.MaxTokens
	}
	if p.Batch != nil && *p.Batch > 0 {
		batch = *p.Batch
	}
	return maxTokens, batch
}

// resolve picks the target of a request (target.go: WithTarget first,
// then the role's current setting).
func (c *HTTPClient) resolve(ctx context.Context, role string) (target, error) {
	if c.set != nil {
		return c.resolveSettings(ctx, role)
	}
	return c.resolveLegacy(ctx, role)
}

func temperatureOf(role string, p model.RoleParams) float64 {
	if p.Temperature != nil {
		return *p.Temperature
	}
	return model.DefaultTemperature(role)
}

func deletedProvider(t model.LLMSnapshot) error {
	name := t.ProviderName
	if name == "" {
		name = t.ProviderID
	}
	return sched.Permanent(model.Errorf(CodeSnapshotProviderDeleted, map[string]any{"provider": name},
		fmt.Errorf("提供商「%s」已删除，请重跑并选择使用当前设置", name)))
}

func (c *HTTPClient) resolveSettings(ctx context.Context, role string) (target, error) {
	t, ok := TargetFrom(ctx)
	if !ok {
		var err error
		if t, err = c.set.RoleTarget(role); err != nil {
			return target{}, sched.Permanent(err)
		}
	}
	p, ok := c.set.Provider(t.ProviderID)
	if !ok {
		return target{}, deletedProvider(t)
	}
	if strings.TrimSpace(t.Model) == "" {
		return target{}, sched.Permanent(model.Errorf(CodeNoModel, map[string]any{"role": role},
			fmt.Errorf("LLM 角色 %s 没有指定模型，请在设置中选择模型", role)))
	}
	if p.APIKey == "" && !settings.IsLocal(p.BaseURL) {
		return target{}, sched.Permanent(model.Errorf(CodeNoAPIKey, map[string]any{"provider": p.Name},
			fmt.Errorf("提供商「%s」没有设置 API key，请在设置页填写后点击重试", p.Name)))
	}
	deepseek := deepSeekRequests(p.BaseURL, p.BalanceAPI) // openai_compat.go
	mt, batch := limitsOf(t.Params)
	return target{
		providerID: p.ID, providerName: p.Name, ptype: p.Type, baseURL: p.BaseURL, key: p.APIKey,
		model: t.Model, temperature: temperatureOf(role, t.Params),
		deepseek: deepseek, thinking: c.thinkingFor(role, p.Name, p.Type, deepseek, t.Params),
		maxTokens: mt, batch: batch,
	}, nil
}

func (c *HTTPClient) resolveLegacy(ctx context.Context, role string) (target, error) {
	name, mod := "", ""
	temp := Temperature(role)
	var params model.RoleParams // config.toml roles have no thinking
	if t, ok := TargetFrom(ctx); ok {
		if _, found := c.cfg.LLM.Providers[t.ProviderID]; !found {
			return target{}, deletedProvider(t)
		}
		name, mod, temp, params = t.ProviderID, t.Model, temperatureOf(role, t.Params), t.Params
	} else {
		r, ok := c.cfg.LLM.Roles[role]
		if !ok && role == RoleSyllabusFormat {
			r, ok = c.cfg.LLM.Roles[config.RoleGlossary]
		}
		if !ok {
			return target{}, sched.Permanent(model.Errorf(CodeUnknownRole, map[string]any{"role": role}, fmt.Errorf("未知的 LLM 角色 %q", role)))
		}
		name, mod = r.Provider, r.Model
	}
	p, ok := c.cfg.LLM.Providers[name]
	if !ok {
		return target{}, sched.Permanent(model.Errorf(CodeProviderNotConfigured, map[string]any{"role": role, "provider": name},
			fmt.Errorf("LLM 角色 %s 使用了未配置的 provider %q，请检查 API key 或模型名", role, name)))
	}
	key := p.Key()
	if key == "" && !p.Local {
		where := "config.toml 的 api_key"
		if p.APIKeyEnv != "" {
			where = "环境变量 " + p.APIKeyEnv
		}
		return target{}, sched.Permanent(model.Errorf(CodeNoAPIKey, map[string]any{"provider": name},
			fmt.Errorf("LLM provider %s 没有 API key（%s 为空），请检查 API key 或模型名", name, where)))
	}
	deepseek := settings.IsDeepSeek(p.BaseURL, "")
	mt, batch := limitsOf(params)
	return target{
		providerID: name, providerName: name, ptype: model.ProviderOpenAI, baseURL: p.BaseURL, key: key,
		model: mod, temperature: temp, deepseek: deepseek,
		thinking:  c.thinkingFor(role, name, model.ProviderOpenAI, deepseek, params),
		maxTokens: mt, batch: batch,
	}, nil
}

// Complete implements Client. See the Client doc for the error contract.
func (c *HTTPClient) Complete(ctx context.Context, role, system, user string, jsonMode bool) (Result, error) {
	tg, err := c.resolve(ctx, role)
	if err != nil {
		return Result{}, err
	}
	// maxTokens is the max_tokens of the request; 0 = not sent (most
	// OpenAI-compatible requests: the server's default limit applies).
	// budget: the tokens it holds for thinking (maxtokens.go: the level's
	// budget, or the headroom of a current Claude model).
	maxTokens, budget := c.outputLimit(tg, role)
	// limit: the model's output limit (0 = none known); a truncated answer
	// is not retried with more.
	limit := truncationCap(tg)
	firstMaxTokens := maxTokens
	// schema: the JSON Schema of the answer, sent to Claude models only
	// (anthropic.go, structured output); nil = the request is as before.
	var schema json.RawMessage
	// formatKey: what a refused schema is remembered by — the provider, the
	// model and the schema itself (review fix of v3.2.x P1: a 400 caused by
	// one role's schema does not turn structured output off for the others).
	formatKey := ""
	if jsonMode && tg.ptype == model.ProviderAnthropic && structuredModel(tg.model) {
		if sc := jsonSchemaFrom(ctx); len(sc) > 0 {
			formatKey = tg.providerID + "\x00" + tg.model + "\x00" + string(sc)
			if _, off := c.noFormat.Load(formatKey); !off {
				schema = sc
			}
		}
	}
	// OpenAI-compatible requests (openai_compat.go): field is the name the
	// output limit goes by; sendTemp, whether the role's temperature may be
	// sent; tempInBody, whether the body built last carries it. triedField,
	// triedTemp: each refusal is answered once per call.
	field, sendTemp, tempInBody := c.tokenField(tg), !c.temperatureRefused(tg), false
	triedField, triedTemp := false, false
	build := func() ([]byte, error) {
		if tg.ptype == model.ProviderAnthropic {
			return anthropicBody(tg, maxTokens, system, user, jsonMode, schema)
		}
		thinking, effort := deepSeekThinking(tg.deepseek, tg.thinking)
		req := chatRequest{
			Model: tg.model,
			Messages: []chatMessage{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
			Thinking:        thinking,
			ReasoningEffort: effort,
		}
		if field == fieldMaxCompletionTokens {
			req.MaxCompletionTokens = maxTokens
		} else {
			req.MaxTokens = maxTokens
		}
		if effort == "" && sendTemp { // DeepSeek ignores temperature in thinking mode
			temp := tg.temperature
			req.Temperature = &temp
		}
		tempInBody = req.Temperature != nil
		if jsonMode {
			req.ResponseFormat = map[string]string{"type": "json_object"}
		}
		return json.Marshal(req)
	}
	endpoint := strings.TrimRight(tg.baseURL, "/") + "/chat/completions"
	if tg.ptype == model.ProviderAnthropic {
		endpoint = strings.TrimRight(tg.baseURL, "/") + "/v1/messages"
	}
	body, err := build()
	if err != nil {
		return Result{}, sched.Permanent(err)
	}
	timeout, long := c.capTimeout(tg, role, c.requestTimeout(tg, role, maxTokens, budget))
	// firstTimeout, firstLong: the first attempt's, kept by a retry that is
	// only one thinking level down (review P3).
	firstTimeout, firstLong := timeout, long
	call := callInfo{role: role, provider: tg.providerID, providerName: tg.providerName, api: tg.ptype,
		model: tg.model, url: endpoint, key: tg.key, timeout: timeout, long: long,
		id: newCallID(), effort: tg.thinking, budget: budget, jsonMode: jsonMode, maxTokens: maxTokens,
		structured: len(schema) > 0, stream: streamed(tg)}
	if call.stream {
		call.idle = c.idleTimeout()
	}
	call.thinkingType, call.effortSent = sentThinkingFields(tg)
	call.try = traceFrom(ctx).begin(c.calls, call)

	// retries counts the §12 transport retries (they use Backoff);
	// truncations the retries of a truncated answer (TruncationRetries).
	retries, truncations := 0, 0
	// steps: the level and max_tokens of each rung of the truncation ladder
	// (the first attempt, then one per truncation retry), for the error.
	steps := []truncStep{{tg.thinking, maxTokens}}
	fb, _ := c.pool.(RateFeedback)
	// reason: why the next attempt is sent again (call log).
	reason := ""
	// withoutFormat: the attempt is the one sent again without the schema
	// after an HTTP 400 (once per call).
	withoutFormat, triedWithoutFormat := false, false
	for attempt := 0; ; attempt++ {
		// giveUp records the error the call ends with.
		giveUp := func(err error) (Result, error) {
			c.calls.failed(ctx, call, attempt, err)
			return Result{}, err
		}
		res, start, aerr := c.attempt(ctx, call, body, attempt, reason)
		if withoutFormat {
			switch {
			case aerr == nil || aerr.kind == failTruncated || aerr.kind == failRefusal:
				// The server took the request without the schema: the schema
				// was what it refused. This model gets none for this schema
				// from now on.
				withoutFormat = false
				if _, known := c.noFormat.LoadOrStore(formatKey, true); !known {
					slog.Warn("llm: the model refused output_config.format with this schema; such requests go without it until the app restarts",
						"provider", call.providerName, "model", call.model, "role", role)
				}
			case aerr.kind == failStatus && aerr.status == http.StatusBadRequest:
				withoutFormat = false // refused for another reason: nothing learned
			}
			// Any other failure (429, 5xx, network) says nothing yet: the
			// transport retry is sent without the schema too.
		}
		if fb != nil && !start.IsZero() {
			switch {
			case aerr == nil || aerr.kind == failTruncated || aerr.kind == failRefusal:
				fb.Succeeded(call.provider)
			case aerr.throttled():
				fb.Throttled(call.provider, start)
			}
		}
		if aerr == nil {
			c.reportUsage(ctx, tg, res, start)
			return res, nil
		}
		if aerr.billed() {
			// The truncated or declined answer was billed: it counts like any
			// other; so does the input (and any output reported) of a stream
			// that failed after message_start, cancelled ones included (SEC4 M1).
			c.reportUsage(ctx, tg, aerr.res, start)
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if aerr.kind == failPool {
			return giveUp(aerr.err)
		}
		if aerr.kind == failTruncated {
			// A failure, retried on the truncation ladder (TruncationRetries).
			// The server is fine, so there is no backoff wait. Retrying the
			// job with the same limits would most likely be cut off again
			// (and billed again), so the final error is permanent. A request
			// without max_tokens hit the server's default limit, which this
			// client cannot raise: permanent at once, no transport retry. So
			// is every truncation of a request whose caller asks for less
			// itself (WithNoTruncationRetry).
			if maxTokens <= 0 || truncations >= TruncationRetries || noTruncationRetry(ctx) {
				return giveUp(sched.Permanent(truncatedError(call, firstMaxTokens, maxTokens, truncations, attempt, steps)))
			}
			if lower, ok := lowerThinking(tg.thinking); ok && strings.TrimSpace(aerr.res.Text) == "" {
				// No text: the limit went to thinking. One level down, the
				// same max_tokens (Q5a).
				reason = fmt.Sprintf("truncated without text: thinking %s -> %s, max_tokens %d", tg.thinking, lower, maxTokens)
				tg.thinking = lower // build() reads tg
				budget, _ = thinkingHeadroom(tg.ptype, tg.model, tg.thinking, streamed(tg))
			} else {
				if limit > 0 && maxTokens >= limit {
					return giveUp(sched.Permanent(truncatedError(call, firstMaxTokens, maxTokens, truncations, attempt, steps)))
				}
				prev := maxTokens
				maxTokens *= 2
				if limit > 0 && maxTokens > limit {
					maxTokens = limit // the model's output limit (v3.2.x D1)
				}
				reason = fmt.Sprintf("truncated: max_tokens %d -> %d", prev, maxTokens)
			}
			truncations++
			steps = append(steps, truncStep{tg.thinking, maxTokens})
			if body, err = build(); err != nil {
				return giveUp(sched.Permanent(err))
			}
			call.maxTokens, call.effort, call.budget = maxTokens, tg.thinking, budget
			call.thinkingType, call.effortSent = sentThinkingFields(tg)
			// The retry gets the time all of max_tokens needs (review fix of
			// D1), within MaxAttemptTimeout; a lower level is recomputed too.
			// A translate batch keeps at least its batchTimeout (review R14).
			if maxTokens == firstMaxTokens && !timeoutScaled(tg) {
				// Only lowered, never doubled, where the first attempt's time
				// follows the thinking budget only (DeepSeek, older Claude
				// models): the first attempt's time as it was (review P3, R56).
				// Recomputed from max_tokens it grew past the first attempt's;
				// from the lower level's budget it shrank, although the level
				// is lowered because thinking filled max_tokens, which it may
				// do again (DeepSeek's effort is no hard limit). Where the time
				// follows max_tokens (current Claude models, a user's
				// max_tokens) it is recomputed as before.
				call.timeout, call.long = firstTimeout, firstLong
				continue
			}
			call.timeout, call.long = c.capTimeout(tg, role, max(attemptTimeout(c.baseTimeout(), maxTokens, budget, true), c.batchTimeout(tg, role, budget)))
			continue
		}
		if aerr.kind == failRefusal {
			return giveUp(sched.Permanent(refusalError(call, aerr.category, attempt)))
		}
		if aerr.kind == failStatus && aerr.status == http.StatusBadRequest && !aerr.inStream && tg.ptype != model.ProviderAnthropic {
			// An OpenAI-compatible model that does not take a parameter of the
			// request as sent (openai_compat.go): the same request once more
			// with the output limit's other name, or without the temperature;
			// no backoff wait, the server is fine. Any other 400, or the same
			// refusal again, is the call's error below.
			again := ""
			switch {
			case maxTokens > 0 && !triedField && refusesField(aerr.errParam, aerr.errCode, field):
				refused := field
				field, triedField = otherTokenField(field), true
				c.learnTokenField(tg, role, refused, field)
				again = fmt.Sprintf("http 400: %s not supported, sent again with %s", refused, field)
			case tempInBody && !triedTemp && refusesTemperature(aerr.errParam, aerr.errCode):
				sendTemp, triedTemp = false, true
				c.learnNoTemperature(tg, role)
				again = "http 400: temperature not supported, sent again without it"
			}
			if again != "" {
				if body, err = build(); err != nil {
					return giveUp(sched.Permanent(err))
				}
				reason = again
				continue
			}
		}
		if aerr.kind == failStatus && aerr.status == http.StatusBadRequest && !aerr.inStream && len(schema) > 0 && !triedWithoutFormat {
			// Structured output: a model that does not know the field refuses
			// the request. Sent once more as it was before the field existed;
			// no backoff wait, the server is fine. A second 400 is the call's
			// error (the request is wrong in some other way).
			schema, triedWithoutFormat, withoutFormat = nil, true, true
			if body, err = build(); err != nil {
				return giveUp(sched.Permanent(err))
			}
			call.structured = false
			reason = "http 400 with output_config.format: sent again without the schema"
			slog.Warn("llm: HTTP 400 for a request with output_config.format, sending it again without the schema",
				"call", call.id, "provider", call.providerName, "model", call.model, "role", role, "error", aerr.body)
			continue
		}
		if aerr.kind == failTimeout && aerr.deadline && noDeadlineRetry(ctx) {
			// The answer took longer than one attempt may (review R14): the
			// caller asks for less instead of the same again (the translate
			// stage splits the batch). Not permanent: still CodeTimeout.
			return giveUp(finalError(call, aerr, attempt))
		}
		if aerr.kind == failTimeout && aerr.deadline && call.long && !call.stream {
			// Review D7: the attempt needed more than LongAttemptTimeout and
			// ran out of the cap; sent again it would most likely do the
			// same. Permanent, like a stream cut at its time limit.
			return giveUp(sched.Permanent(longTimeoutError(call, attempt)))
		}
		if !aerr.retryable() {
			return giveUp(sched.Permanent(finalError(call, aerr, attempt)))
		}
		if retries >= len(c.Backoff) {
			return giveUp(finalError(call, aerr, attempt))
		}
		wait := c.Backoff[retries]
		if aerr.hasRA {
			wait = aerr.retryAfter
		}
		retries++
		reason = retryReason(aerr, wait)
		reportBackoff(ctx, call, attempt, wait)
		if err := c.Sleep(ctx, wait); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return giveUp(err)
		}
	}
}

// sentThinkingFields returns thinking.type and output_config.effort of an
// Anthropic request to tg as sent ("" = absent; always "" elsewhere).
func sentThinkingFields(tg target) (thinkingType, effort string) {
	if tg.ptype != model.ProviderAnthropic {
		return "", ""
	}
	th, oc := anthropicThinkingFields(tg.model, tg.thinking)
	if th != nil {
		thinkingType = th.Type
	}
	if oc != nil {
		effort = oc.Effort
	}
	return thinkingType, effort
}

// capTimeout returns d, at most MaxAttemptTimeout; reaching the cap is
// logged once per process, provider, model and role (SEC1 L3). long
// reports a d that was cut and is over LongAttemptTimeout (review D7).
func (c *HTTPClient) capTimeout(tg target, role string, d time.Duration) (_ time.Duration, long bool) {
	if MaxAttemptTimeout <= 0 || d <= MaxAttemptTimeout {
		return d, false
	}
	c.warnLimit("timeout_cap", tg, role, "llm: the timeout of one attempt is capped",
		"timeout", d.String(), "cap", MaxAttemptTimeout.String())
	return MaxAttemptTimeout, d > LongAttemptTimeout
}

// longTimeoutError is the job error of a request that is not streamed
// whose attempt, cut to MaxAttemptTimeout, timed out (review D7): CodeTimeout
// (its params as always), not sent again; the detail says why.
func longTimeoutError(call callInfo, retries int) error {
	tried := ""
	if retries > 0 {
		tried = fmt.Sprintf("，已重试 %d 次", retries)
	}
	params := map[string]any{"role": call.role, "retries": retries, "seconds": call.timeout.Seconds()}
	return model.Errorf(CodeTimeout, params, fmt.Errorf("LLM 请求超时（%s，单次请求达到 %s 的上限%s）：这次请求需要的时间超过上限，原样重试多半会再次超时，所以没有重试；可以换用更快的模型，开了深度思考的话可以降低思考档位，然后点击重试",
		call.role, call.timeout, tried))
}

// baseTimeout is the timeout of one attempt without thinking (SPEC §12).
func (c *HTTPClient) baseTimeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// requestTimeout is the timeout of one attempt of a request to tg with
// maxTokens, budget of them held for thinking (attemptTimeout). It follows
// all of max_tokens when that is this client's choice for the answer (a
// current Claude model's base + headroom) or the user's value; elsewhere
// only the budget counts, as before (the default limits of older Claude
// models and DeepSeek). A translate batch gets at least batchTimeout.
func (c *HTTPClient) requestTimeout(tg target, role string, maxTokens, budget int) time.Duration {
	return max(attemptTimeout(c.baseTimeout(), maxTokens, budget, timeoutScaled(tg)), c.batchTimeout(tg, role, budget))
}

// timeoutScaled reports whether the timeout of a request to tg follows all
// of max_tokens (a current Claude model's base + headroom, or the user's
// value) rather than the thinking budget only (requestTimeout).
func timeoutScaled(tg target) bool {
	return tg.maxTokens > 0 || tg.ptype == model.ProviderAnthropic && anthropicAdaptive(tg.model)
}

// reportUsage reports the usage of one response, sent at start, to
// usage_daily (priced by the period start falls in, v2 P12.5, when the
// recorder is a CostRecorder), the job's stats sink (with that cost) and the
// WithUsage totals.
func (c *HTTPClient) reportUsage(ctx context.Context, tg target, res Result, start time.Time) {
	cost, priced := 0.0, false
	if c.usage != nil {
		// Spent tokens count even when the job is cancelled right now.
		uctx := context.WithoutCancel(ctx)
		if cr, ok := c.usage.(CostRecorder); ok {
			cost, priced = cr.RecordRequest(uctx, tg.providerID, tg.model, start, res.PromptTokens, res.CacheHitTokens, res.CompletionTokens)
		} else {
			c.usage.RecordUsage(uctx, tg.providerID, res.PromptTokens, res.CacheHitTokens, res.CompletionTokens)
		}
	}
	// The scheduler's stats sink owns job.stats (NOTES.md, Gate A round 2).
	// Without a priced estimate it prices the tokens itself (sched.ReportUsageCost).
	sched.ReportUsageCost(ctx, res.PromptTokens, res.CompletionTokens, res.CacheHitTokens, cost, priced)
	if u := UsageFrom(ctx); u != nil {
		u.Add(res)
	}
}

// truncatedError is the job error of an answer still truncated after the
// truncation ladder; first is the limit of the first attempt, maxTokens of
// the last (0 = the request set none and the server's default output limit
// was reached; a doubled limit stops at the model's output limit), steps
// the level and limit of every attempt. Coded CodeTruncated (v2 P14.3) with
// the attempts in the param "tried" ("high/64000, low/64000, low/128000");
// errors.Is(err, ErrTruncated) holds.
func truncatedError(call callInfo, first, maxTokens, truncations, retries int, steps []truncStep) error {
	limits := fmt.Sprintf("max_tokens %d 仍不够", maxTokens)
	switch {
	case maxTokens <= 0:
		limits = "回答达到了服务端默认的输出长度上限"
	case truncations > 0 && first != maxTokens:
		limits = fmt.Sprintf("max_tokens 已从 %d 翻倍到 %d 仍不够", first, maxTokens)
	}
	zh := make([]string, len(steps))
	plain := make([]string, len(steps))
	for i, st := range steps {
		lvl := st.level
		if lvl == "" {
			lvl = model.ThinkingOff
		}
		zh[i] = fmt.Sprintf("思考 %s、max_tokens %d", lvl, max(st.maxTokens, 0))
		plain[i] = fmt.Sprintf("%s/%d", lvl, max(st.maxTokens, 0))
	}
	if len(steps) > 1 {
		limits += "；各次尝试：" + strings.Join(zh, " → ")
	}
	tried := ""
	if retries > 0 {
		tried = fmt.Sprintf("，已重试 %d 次", retries)
	}
	params := map[string]any{"role": call.role, "retries": retries, "provider": call.providerName, "model": call.model,
		"max_tokens": max(maxTokens, 0), "tried": strings.Join(plain, ", ")}
	return model.Errorf(CodeTruncated, params, fmt.Errorf("LLM 输出被截断（%s，%s%s；provider %s，model %s），可以点击重试，或在设置中为该角色换用其他模型：%w",
		call.role, limits, tried, call.providerName, call.model, ErrTruncated))
}

// refusalError is the job error of a request the provider declined
// (ErrRefusal). category is stop_details.category ("" when none came).
func refusalError(call callInfo, category string, retries int) error {
	what := ""
	if category != "" {
		what = "，类别 " + category
	}
	params := map[string]any{"role": call.role, "retries": retries, "provider": call.providerName, "model": call.model, "category": category}
	return model.Errorf(CodeRefusal, params, fmt.Errorf("LLM 拒绝了这次请求（%s%s；provider %s，model %s）：服务商的安全策略中止了回答，重试同一请求不会有不同结果，可以在设置中为该角色换用其他模型：%w",
		call.role, what, call.providerName, call.model, ErrRefusal))
}

type callInfo struct {
	role, provider, providerName, api, model, url, key string
	timeout                                            time.Duration
	// long: timeout was cut to MaxAttemptTimeout from more than
	// LongAttemptTimeout (capTimeout; review D7)
	long bool

	// For the call log (calllog.go).
	id        string // one Complete call
	try       int    // the stage's request for the same answer (WithCallTrace), 0 = no trace
	effort    string // thinking level sent
	budget    int    // the tokens max_tokens holds for thinking (maxtokens.go)
	jsonMode  bool
	maxTokens int // max_tokens of the next attempt, 0 = not sent
	// structured: the next attempt carries a JSON Schema; thinkingType and
	// effortSent: thinking.type and output_config.effort as sent (Anthropic).
	structured               bool
	thinkingType, effortSent string
	// stream: the request is streamed (stream.go); idle its idle timeout
	// (0 = none).
	stream bool
	idle   time.Duration
}

// setAuth sets the authentication headers of the provider type.
func setAuth(h http.Header, api, key string) {
	if api == model.ProviderAnthropic {
		if key != "" {
			h.Set("x-api-key", key)
		}
		h.Set("anthropic-version", AnthropicVersion)
		return
	}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

// attempt performs one HTTP request holding an API pool slot. start is when
// the request was sent (after the pool granted it): the moment that decides
// its peak or off-peak price (v2 P12.5).
func (c *HTTPClient) attempt(ctx context.Context, call callInfo, body []byte, n int, reason string) (_ Result, start time.Time, _ *attemptErr) {
	queued := time.Now()
	if c.pool != nil {
		release, err := c.pool.Acquire(ctx, call.provider)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, start, &attemptErr{kind: failNetwork, err: ctx.Err()}
			}
			return Result{}, start, &attemptErr{kind: failPool, err: fmt.Errorf("api pool: %w", err)}
		}
		defer release()
	}
	if fn := sentHookFrom(ctx); fn != nil {
		fn()
	}
	actx, cancel := context.WithTimeout(ctx, call.timeout)
	// rctx: the request's context; with an idle timeout (streamed requests)
	// it is also cancelled with errIdle when no event arrives in time.
	rctx := actx
	var idleCtx context.Context
	var idleCancel context.CancelCauseFunc
	// endRequest ends the request's contexts when attempt returns, unless
	// the rest of a finished stream is still being read then (drainBody,
	// which ends them itself).
	endRequest := cancel
	draining := false
	defer func() {
		if !draining {
			endRequest()
		}
	}()
	if call.stream && call.idle > 0 {
		idleCtx, idleCancel = context.WithCancelCause(actx)
		endRequest = func() { idleCancel(nil); cancel() }
		rctx = idleCtx
	}

	start = time.Now()
	hreq, err := http.NewRequestWithContext(rctx, http.MethodPost, call.url, bytes.NewReader(body))
	if err != nil {
		// Malformed base_url: not retryable (status 0 → permanent, generic message).
		return Result{}, start, &attemptErr{kind: failStatus, status: 0, body: err.Error(), err: err}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if call.stream {
		hreq.Header.Set("Accept", "text/event-stream")
	} else {
		hreq.Header.Set("Accept", "application/json")
	}
	setAuth(hreq.Header, call.api, call.key)

	c.calls.start(ctx, call, n, reason, len(body), start.Sub(queued))
	// ended: the end record below was written. A panic before it does not
	// leave the attempt counted in flight, which kept the call log's files
	// open (review D16); the panic goes on.
	ended := false
	defer func() {
		if !ended {
			c.calls.abandon()
		}
	}()
	reportStart(ctx, call, n, start)
	// touch restarts the idle timeout: it runs from the moment the request
	// is sent, through the wait for the response headers, to the last event.
	var touch func()
	if idleCtx != nil {
		newTimer := c.newIdleTimer
		if newTimer == nil {
			newTimer = afterFunc
		}
		t := newTimer(call.idle, func() { idleCancel(errIdle) })
		defer t.Stop()
		touch = func() { t.Reset(call.idle) }
	}
	var out attemptOutcome
	resp, err := c.http.Do(hreq)
	if err == nil {
		out.status, out.header, out.headersAt = resp.StatusCode, resp.Header, time.Since(start)
		if touch != nil {
			touch()
		}
		if call.stream && resp.StatusCode == http.StatusOK && isEventStream(resp.Header) {
			// alive: the job's last activity follows the events, at most
			// every streamActivityEvery (SEC4 L7).
			lastAlive := start
			alive := func() {
				if now := time.Now(); now.Sub(lastAlive) >= streamActivityEvery {
					lastAlive = now
					reportActivity(ctx, call, n, now)
				}
			}
			hooks := streamHooks{since: func() time.Duration { return time.Since(start) }, touch: touch, alive: alive,
				phase: func(p string) { reportPhase(ctx, call, n, p) }, start: firstEventHookFrom(ctx)}
			if c.cfg.LogLevel == "debug" {
				hooks.keep = &bytes.Buffer{} // the raw events go to the debug log
			}
			var so streamOutcome
			so, err = readAnthropicStream(resp.Body, call.key, hooks)
			out.stream = &so
			if hooks.keep != nil {
				out.respBody = hooks.keep.Bytes()
			}
			// The stream was read to its end (message_stop) or given up on as
			// unusable: what the server still sends is read in the
			// background, so the connection can be used again (SEC4 L5).
			draining = err == nil
		} else {
			// Not streamed, an error status, or a server that ignored
			// "stream" and sent the whole message.
			var r io.Reader = io.LimitReader(resp.Body, maxResponseBytes)
			if touch != nil {
				r = &touchReader{r: r, touch: touch}
			}
			out.respBody, err = io.ReadAll(r)
		}
		if draining {
			go drainBody(resp.Body, endRequest, c.drained)
		} else {
			resp.Body.Close()
		}
	}
	out.duration = time.Since(start)
	if err != nil && idleCtx != nil && errors.Is(context.Cause(idleCtx), errIdle) {
		out.idle = true
	}
	c.debugLog(ctx, call, hreq.Header, body, n, out.status, out.respBody, err, out.duration)

	res, aerr := c.answer(ctx, call, out, err)
	if aerr != nil && aerr.kind == failTimeout && ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
		aerr.deadline = true
	}
	c.observeLimits(call, out, res, aerr) // streamed or not, any status (T-PERF-03)
	rec := c.calls.end(ctx, call, n, out, aerr)
	ended = true
	reportEnd(ctx, call, n, start.Add(out.duration), rec, res, aerr)
	return res, start, aerr
}

// A streamed response read up to message_stop is closed by drainBody, which
// first reads what is left of it — at most drainMaxBytes, for at most
// drainWait — so that an HTTP/1.1 connection goes back to the pool instead
// of being closed (SEC4 L5). It runs in the background: the answer does not
// wait for it, and a server that does not end the response is given up on
// after drainWait (the request is cancelled, the connection closed).
const (
	drainMaxBytes = 4 << 10
	drainWait     = time.Second
)

// drainBody reads the rest of body (drainMaxBytes, drainWait), closes it and
// then calls end, which ends the request's contexts; done (nil = none) is
// told whether body was read to its end. At drainWait the request is
// cancelled and body closed (once: closeBody), so a read blocked in a
// transport that does not follow the request's context ends too (R56).
func drainBody(body io.ReadCloser, end func(), done func(eof bool)) {
	closeBody := sync.OnceFunc(func() { body.Close() })
	stop := time.AfterFunc(drainWait, func() { end(); closeBody() })
	_, err := io.CopyN(io.Discard, body, drainMaxBytes)
	stop.Stop()
	closeBody()
	end()
	if done != nil {
		done(err == io.EOF)
	}
}

// answer turns the HTTP exchange of an attempt into its Result or failure.
func (c *HTTPClient) answer(ctx context.Context, call callInfo, out attemptOutcome, err error) (Result, *attemptErr) {
	status, respBody := out.status, out.respBody
	if err != nil {
		// so: what a stream gave before the read failed (nil when the
		// response was not a stream); its usage goes with the failure.
		so := out.stream
		if ctx.Err() != nil {
			return Result{}, so.failWith(&attemptErr{kind: failNetwork, err: ctx.Err()})
		}
		if so != nil && so.aerr != nil && (so.aerr.kind == failTruncated || so.aerr.kind == failRefusal) {
			// The stop reason came before the stream broke: the answer is
			// over, truncated or declined (SEC4 L9).
			return Result{}, so.aerr
		}
		if out.idle {
			return Result{}, so.failWith(&attemptErr{kind: failIdle, err: fmt.Errorf("%w (%s)", errIdle, call.idle)})
		}
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			if stillStreaming(call, out) {
				// Cut while the answer was still coming: it is longer than one
				// attempt may take. The same request would be cut again and
				// billed again, so it is not sent again (SEC4 M4).
				return Result{}, so.failWith(&attemptErr{kind: failTimeLimit, permanent: true,
					err: fmt.Errorf("the attempt timeout of %s ran out while the answer was still streaming", call.timeout)})
			}
			return Result{}, so.failWith(&attemptErr{kind: failTimeout, err: err})
		}
		return Result{}, so.failWith(&attemptErr{kind: failNetwork, err: err})
	}
	if status != http.StatusOK {
		ae := &attemptErr{kind: failStatus, status: status, body: redactSnippet(respBody, call.key)}
		ae.retryAfter, ae.hasRA = parseRetryAfter(out.header.Get("Retry-After"))
		if status == http.StatusBadRequest && call.api != model.ProviderAnthropic {
			ae.errParam, ae.errCode = openAIErrorFields(respBody)
		}
		return Result{}, ae
	}
	if out.stream != nil {
		return out.stream.res, out.stream.aerr
	}
	if call.api == model.ProviderAnthropic {
		res, aerr := parseAnthropic(respBody, call.key)
		return res, aerr
	}
	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(respBody, call.key), err: err}
	}
	if len(cr.Choices) == 0 {
		return Result{}, &attemptErr{kind: failEnvelope, body: redactSnippet(respBody, call.key), err: errors.New("no choices in response")}
	}
	res := Result{Text: cr.Choices[0].Message.Content}
	if u := cr.Usage; u != nil {
		res.PromptTokens = u.PromptTokens
		res.CompletionTokens = u.CompletionTokens
		switch {
		case u.PromptCacheHitTokens != nil:
			res.CacheHitTokens = *u.PromptCacheHitTokens
		case u.PromptTokensDetails != nil:
			res.CacheHitTokens = u.PromptTokensDetails.CachedTokens
		}
	}
	switch cr.Choices[0].FinishReason {
	case finishLength:
		// Cut off at max_tokens (or the context length): the same failure
		// as Anthropic's stop_reason max_tokens (appendix D2).
		return Result{}, &attemptErr{kind: failTruncated, res: res, err: errors.New("finish_reason length")}
	case finishContentFilter:
		// Stopped by the provider's content filter: the same failure as
		// Anthropic's stop_reason refusal (ErrRefusal; never retried, its
		// usage counts). Content that came before the filter is cut short
		// and is not an answer. No category is given.
		return Result{}, &attemptErr{kind: failRefusal, res: res, err: errors.New("finish_reason content_filter")}
	case finishInsufficientResource:
		// DeepSeek: the inference service ran out of resources and cut the
		// answer short. A temporary server failure like a 5xx or an
		// overload: retried with the backoff; the cut-off content is not
		// an answer; the billed usage counts (N50b).
		return Result{}, &attemptErr{kind: failResource, res: res, usage: true, err: errors.New("finish_reason insufficient_system_resource")}
	}
	return res, nil
}

// OpenAI-compatible finish_reason values: finishLength, an answer cut off
// at max_tokens or at the context length; finishContentFilter, an answer
// stopped by the provider's content filter; finishInsufficientResource
// (DeepSeek), an answer interrupted because the inference service ran out
// of resources.
const (
	finishLength               = "length"
	finishContentFilter        = "content_filter"
	finishInsufficientResource = "insufficient_system_resource"
)

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parseRetryAfter reads delay-seconds or an HTTP date, capped at MaxRetryAfter.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			secs = 0
		}
		d = time.Duration(secs * float64(time.Second))
	} else if t, err := http.ParseTime(v); err == nil {
		d = max(time.Until(t), 0)
	} else {
		return 0, false
	}
	return min(d, MaxRetryAfter), true
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	const n = 300
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// redactSnippet is the excerpt of a server's body for an error or a log:
// the key is redacted BEFORE the body is cut to its excerpt, so a key that
// straddles the cut cannot leak in part (sec2-app L1).
func redactSnippet(b []byte, key string) string {
	return snippet([]byte(redact(string(b), key)))
}

// redact replaces a key echoed by a server (some error bodies quote the
// key they rejected) with its hint, so it never reaches job.error or the UI.
func redact(s, key string) string {
	if key == "" || !strings.Contains(s, key) {
		return s
	}
	return strings.ReplaceAll(s, key, model.KeyHint(key))
}

// ErrConnectTimeout is in the chain of a CodeTimeout error whose last
// attempt timed out before its own timeout ran out: connecting or the TLS
// handshake took too long (review D8). The server never got the request,
// so a smaller request would not help (the translate stage does not split
// a batch for it). The error's text and params are the same as for any
// CodeTimeout.
var ErrConnectTimeout = errors.New("llm: timed out connecting to the server")

// connectTimeoutErr is the transport's error with ErrConnectTimeout in its
// chain; its text is the transport's.
type connectTimeoutErr struct{ error }

func (e connectTimeoutErr) Unwrap() []error { return []error{e.error, ErrConnectTimeout} }

// finalError turns the last attempt failure into the user-facing job error
// (SPEC §12, §17.6). retries is the number of retries already made. The
// error is coded (v2 P14.3, codes.go); its Chinese text is the detail.
func finalError(call callInfo, e *attemptErr, retries int) error {
	tried := ""
	if retries > 0 {
		tried = fmt.Sprintf("，已重试 %d 次", retries)
	}
	params := func(kv ...any) map[string]any {
		p := map[string]any{"role": call.role, "retries": retries}
		for i := 0; i+1 < len(kv); i += 2 {
			p[kv[i].(string)] = kv[i+1]
		}
		return p
	}
	switch e.kind {
	case failNetwork:
		return model.Errorf(CodeNetwork, params(),
			fmt.Errorf("LLM 请求失败（%s%s）：网络不可用，请检查网络连接后点击重试：%w", call.role, tried, e.err))
	case failTimeout:
		err := e.err
		if !e.deadline {
			err = connectTimeoutErr{err} // review D8
		}
		return model.Errorf(CodeTimeout, params("seconds", call.timeout.Seconds()),
			fmt.Errorf("LLM 请求超时（%s，单次请求超过 %s%s）：网络不可用或服务无响应，请稍后点击重试：%w",
				call.role, call.timeout, tried, err))
	case failIdle:
		// A code of its own (SEC4 M5): the hint for gateways and proxies that
		// buffer a streamed answer, which no retry gets through.
		return model.Errorf(CodeIdleTimeout, params("seconds", call.idle.Seconds()),
			fmt.Errorf("LLM 请求超时（%s，流式回答超过 %s 没有任何数据%s）：服务无响应或网络中断，请稍后点击重试；若使用的网关或代理会缓冲流式响应，可以在设置里把空闲超时设为 0（关闭）：%w",
				call.role, call.idle, tried, e.err))
	case failTimeLimit:
		return model.Errorf(CodeTimeLimit, params("seconds", call.timeout.Seconds(), "provider", call.providerName, "model", call.model),
			fmt.Errorf("LLM 在单次请求的时限内没有输出完（%s，单次请求上限 %s%s，provider %s，model %s）：服务仍在输出时被中止，原样重试多半会再次超时并重复计费；可以换用更快的模型，开了深度思考的话可以降低思考档位，然后点击重试：%w",
				call.role, call.timeout, tried, call.providerName, call.model, e.err))
	case failResource:
		return model.Errorf(CodeOverloaded, params(),
			fmt.Errorf("LLM 服务过载（%s，服务端资源不足，回答被中断%s），请稍后点击重试：%w", call.role, tried, e.err))
	case failEnvelope:
		return model.Errorf(CodeBadResponse, params(),
			fmt.Errorf("LLM 响应无法解析（%s%s）：%v：%s", call.role, tried, e.err, e.body))
	}
	switch {
	case e.status == http.StatusBadRequest || e.status == http.StatusUnauthorized || e.status == http.StatusForbidden:
		return model.Errorf(CodeRejected, params("status", e.status, "provider", call.providerName, "model", call.model),
			fmt.Errorf("LLM 请求被拒绝（%s，HTTP %d），请检查 API key 或模型名（provider %s，model %s）：%s",
				call.role, e.status, call.providerName, call.model, e.body))
	case e.status == http.StatusPaymentRequired:
		return model.Errorf(CodeNoBalance, params("provider", call.providerName),
			fmt.Errorf("LLM 账户余额不足（%s，HTTP 402，provider %s），请充值后点击重试：%s", call.role, call.providerName, e.body))
	case e.status == http.StatusTooManyRequests:
		return model.Errorf(CodeRateLimited, params(),
			fmt.Errorf("LLM 请求被限流（%s，HTTP 429%s），请稍后点击重试：%s", call.role, tried, e.body))
	case e.status == StatusOverloaded:
		return model.Errorf(CodeOverloaded, params(),
			fmt.Errorf("LLM 服务过载（%s，HTTP 529%s），请稍后点击重试：%s", call.role, tried, e.body))
	case e.status >= 500:
		return model.Errorf(CodeServerError, params("status", e.status),
			fmt.Errorf("LLM 服务端错误（%s，HTTP %d%s），请稍后点击重试：%s", call.role, e.status, tried, e.body))
	}
	return model.Errorf(CodeRequestFailed, params("status", e.status, "provider", call.providerName, "model", call.model),
		fmt.Errorf("LLM 请求失败（%s，HTTP %d），请检查 base_url、API key 或模型名（provider %s，model %s）：%s",
			call.role, e.status, call.providerName, call.model, e.body))
}
