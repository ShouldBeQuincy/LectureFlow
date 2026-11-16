// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package llmtest is the fake LLM server of SPEC §14 (OpenAI-compatible)
// and of v1.1 A6 (Anthropic Messages API).
//
// It identifies the role of each request by the first line of its system
// prompt (ctxbuild's §13 prompts) and answers with the canned responses of
// testdata/llm/{role}.json:
//
//	glossary_seed.json, material_terms.json, repair.json  the JSON object the
//	                                                      model returns
//	summary.json    {"summary": "..."}  → returned as plain text
//	translate.json  {"default": "...", "by_idx": {"0": "..."}, "by_text": {"target text": "..."}}
//	                → {"idx": <target idx>, "zh": ...}; lookup order:
//	                  SetTranslation override, by_text (target text), by_idx, default;
//	                → a batch request (v3.2.x P2, ctxbuild.TranslateBatchSystemTemplate,
//	                  role translate, Request.Batch) gets {"items": [{"idx", "zh"}, …]},
//	                  one item per [TARGET #idx] in order, each looked up the same way
//	syllabus_format.md  (optional) the Markdown of the A8.3 syllabus_format
//	                    role; a built-in document when the file is absent
//	glossary_janitor.json  (optional) the JSON object of the SPEC-v2 G3
//	                    glossary janitor; {"actions": []} when absent
//	chapters.json       (optional) the SPEC-v2 P7 chapters answer;
//	                    DefaultChaptersAnswer when absent
//	title.json          (optional) the P13.5 answer of the organizer role
//	                    (title task); DefaultTitleAnswer when absent
//	filename_rule.json  (optional) the P13.3.2 answer of the organizer role
//	                    (filename_rule task); DefaultFilenameRuleAnswer when absent
//
// The organizer role has two prompts (P13.5, P13.3.2); the server tells
// them apart by their first lines and reports them as the roles title and
// filename_rule (Count, Requests, Handle, FailNext use these names).
//
// Endpoints:
//
//	POST {URL}/chat/completions   OpenAI-compatible (SPEC §12)
//	POST {URL}/v1/messages        Anthropic (A6); system blocks, cache_control,
//	                              "Respond with JSON only." in JSON mode
//	GET  {URL}/models, {URL}/v1/models   model lists for "探测模型" (A5.3);
//	                              limit/after_id paginate like Anthropic's API
//
// A request without system prompt whose user message is "ping" is the
// "测试连接" request (A5.3, role RolePing) and is answered with "pong".
//
// Every chat response carries usage. OpenAI: prompt_cache_hit_tokens (the
// system prompt counts as a cache hit when it was seen before). Anthropic:
// cache_creation_input_tokens the first time a cache_control system prompt
// is seen, cache_read_input_tokens afterwards.
//
// Programmable failures: FailNext (per role, including RolePing), SetError
// (every request until cleared), SetDelay (latency before every answer),
// RequireKey (401 echoing the wrong key, to test that callers redact it),
// SetConcurrencyLimit (429 above n chat requests in flight, like DeepSeek's
// account-level concurrency limit; perf-llm).
//
// SetCacheAfterAnswer models DeepSeek's context cache more closely
// (perf-llm): a system prompt is a cache hit only for requests that arrive
// after a request with it was answered.
//
// Structured output and thinking of the Anthropic endpoint (v3.2.x P1): a
// request's output_config.format is checked like the API checks it
// (CheckSchema) and recorded (Request.Schema); RejectFormat makes the server
// refuse the field with HTTP 400, like a model that does not know it;
// thinking {"type": "between_tools"} is refused for every model but
// claude-sonnet-5-5 and with any other field; {"type": "disabled"} by the
// models whose thinking cannot be switched off (claude-opus-5-5,
// claude-sonnet-5-5, Fable, Mythos), with any other field and with effort
// xhigh or max (C91). FailRefusal answers with
// stop_reason "refusal" (OpenAI endpoint: finish_reason "content_filter"). FailSecondJSON and FailTrailingText append to the
// normal answer what a model's second thoughts look like. FailTruncated
// sends the first half of the normal answer as cut off at the output limit.
//
// Batch translate answers (v3.2.x P2): FailBatchMissing, FailBatchExtra,
// FailBatchDuplicate, FailBatchWrongIds, FailBatchShuffled and
// FailBatchBareArray change the items of a batch answer (no effect on a
// single-sentence request; FailWrongIdx on a batch is FailBatchWrongIds).
// FailNextBatch queues failures that only batch requests take, ahead of the
// translate role's FailNext queue. FailHang sends nothing until the client
// hangs up (its attempt timeout).
//
// Streaming (v3.2.x P1-c, stream.go of this package): a request with
// "stream": true is answered with the server-sent events of the Messages
// API, made from the same message the server would send whole
// (MessageSSE), so every canned answer, Handle, FailNext and Truncated work
// for streamed requests too. With "display": "summarized" the thinking block
// streams ThinkingSummary. StreamNext scripts other streams (pings, a hang,
// an error event, a dropped connection, max_tokens without text);
// SetStreamHook watches (and may pause) the events as they are sent.
//
// Programmable answers: Handle(role, fn) answers every request of role with
// fn(system, user) (e.g. janitor actions naming term ids created at run time);
// injected failures still apply. An answer wrapped with Truncated is sent as
// cut off at the output limit (finish_reason "length" / stop_reason "max_tokens").
package llmtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Roles as identified by the fake server (also the fixture file names).
const (
	RoleGlossarySeed    = "glossary_seed"
	RoleMaterialTerms   = "material_terms"
	RoleSummary         = "summary"
	RoleRepair          = "repair"
	RoleTranslate       = "translate"
	RoleSyllabusFormat  = "syllabus_format"  // v1.1 A8.3; fixture optional
	RoleGlossaryJanitor = "glossary_janitor" // SPEC-v2 G3; fixture optional
	RoleChapters        = "chapters"         // SPEC-v2 P7; fixture optional
	RoleTitle           = "title"            // SPEC-v2 P13.5, organizer role; fixture optional
	RoleFilenameRule    = "filename_rule"    // SPEC-v2 P13.3.2, organizer role; fixture optional
	RolePing            = "ping"             // "测试连接" (A5.3)
)

// Answers of the optional SPEC-v2 phase 2 roles when their fixture is absent.
// The chapters answer needs a transcript of at least 10 sentences.
const (
	DefaultChaptersAnswer     = `{"chapters": [{"start_idx": 0, "title_en": "Introduction", "title_zh": "引言"}, {"start_idx": 4, "title_en": "Main Topic", "title_zh": "主要内容"}, {"start_idx": 9, "title_en": "Summary", "title_zh": "总结"}]}`
	DefaultTitleAnswer        = `{"title_en": "Introduction to Testing", "title_zh": "测试导论", "blurb_en": "What testing is and why it matters.", "blurb_zh": "什么是测试以及测试为什么重要。"}`
	DefaultFilenameRuleAnswer = `{"regex": "(?P<year>\\d{4})-(?P<month>\\d{2})-(?P<day>\\d{2})", "explanation": "The file names contain the date as YYYY-MM-DD."}`
)

// optionalFixtures are the optional JSON fixtures and their defaults.
var optionalFixtures = map[string]string{
	RoleGlossaryJanitor: DefaultJanitorAnswer,
	RoleChapters:        DefaultChaptersAnswer,
	RoleTitle:           DefaultTitleAnswer,
	RoleFilenameRule:    DefaultFilenameRuleAnswer,
}

// Roles lists every role whose fixture file is required.
var Roles = []string{RoleGlossarySeed, RoleMaterialTerms, RoleSummary, RoleRepair, RoleTranslate}

// TestKey is the API key Configure writes into the config. Not a real key.
const TestKey = "sk-test-fake-llm"

// DefaultModels is the model list of GET /models until SetModels.
var DefaultModels = []string{"deepseek-chat", "deepseek-reasoner"}

// DefaultJanitorAnswer is the glossary_janitor answer without a fixture.
const DefaultJanitorAnswer = `{"actions": []}`

// TruncatedPrefix marks a Handle (or SetResponse) answer that the server
// sends as cut off at the output limit: OpenAI finish_reason "length",
// Anthropic stop_reason "max_tokens"; the content is the rest of the answer.
const TruncatedPrefix = "\x00llmtest:truncated\x00"

// Truncated returns content marked with TruncatedPrefix.
func Truncated(content string) string { return TruncatedPrefix + content }

// cutTruncated removes TruncatedPrefix and reports whether it was there.
func cutTruncated(content string) (string, bool) {
	return strings.CutPrefix(content, TruncatedPrefix)
}

// DefaultSyllabusMarkdown is the syllabus_format answer without a fixture.
const DefaultSyllabusMarkdown = "# CS101 Introduction to Testing\n\n## Weekly Topics\n\n| Week | Topic | Notes |\n|---|---|---|\n| 1 | Caches | |\n\n## Key Terms\n\ncache, write-back\n"

// Failure is an injected failure mode (FailNext).
type Failure int

const (
	FailHTTP500  Failure = iota // HTTP 500
	FailHTTP429                 // HTTP 429 with Retry-After: 0
	FailHTTP401                 // HTTP 401
	FailNotJSON                 // HTTP 200 whose content is not JSON
	FailWrongIdx                // translate: HTTP 200 with idx = target+1000
	FailFenced                  // HTTP 200 with the normal content wrapped in ```json fences
	FailHTTP529                 // HTTP 529 overloaded_error (Anthropic, A6)
	FailHTTP403                 // HTTP 403
	// FailSecondJSON: HTTP 200 with the normal content, then text, then a
	// second JSON object that differs from it ("Wait, …" and a corrected
	// answer: SecondJSONTail).
	FailSecondJSON
	// FailTrailingText: HTTP 200 with the normal content followed by text
	// without any JSON (TrailingText).
	FailTrailingText
	// FailRefusal: Anthropic: HTTP 200 with stop_reason "refusal" and
	// stop_details.category RefusalCategory, no text. OpenAI: HTTP 200
	// with the first half of the normal answer and finish_reason
	// "content_filter" (no category).
	FailRefusal
	// FailTruncated: HTTP 200 with the first half of the normal answer, cut
	// off at the output limit (finish_reason "length" / stop_reason
	// "max_tokens"), like Truncated.
	FailTruncated
	// Batch translate answers (v3.2.x P2); no effect on a single-sentence
	// translate request or another role.
	FailBatchMissing   // the last item is left out
	FailBatchExtra     // an item for idx = last target + 1000 is appended
	FailBatchDuplicate // the first item is appended again
	FailBatchWrongIds  // every idx is target + 1000
	FailBatchShuffled  // the items in reverse order (every idx present once)
	FailBatchBareArray // the items array alone, without {"items": …}
	// FailHang: no response (not even its headers) until the client hangs
	// up, e.g. when its attempt timeout runs out (any request, streamed or
	// not; review R14).
	FailHang
)

// WrongIdxOffset is what FailWrongIdx, FailBatchExtra and FailBatchWrongIds
// add to a target idx.
const WrongIdxOffset = 1000

// What FailSecondJSON and FailTrailingText append, and the category of
// FailRefusal.
const (
	SecondJSONTail  = "\n\nWait, that is not right. Let me correct it:\n" + `{"llmtest": "corrected answer"}`
	TrailingText    = "\n\nNote: I kept the technical terms as they are."
	RefusalCategory = "cyber"
)

// Request is one chat request received by the server.
type Request struct {
	Role        string // "" when not recognised
	API         string // model.ProviderOpenAI | model.ProviderAnthropic
	Model       string
	System      string
	User        string // Anthropic: without the appended JSON instruction
	Temperature float64
	HasTemp     bool   // the request carried a temperature
	JSONMode    bool   // OpenAI: response_format json_object; Anthropic: JSON instruction appended
	TargetIdx   int    // translate: the [TARGET #idx] (v1: [目标 #idx]), the first one of a batch; -1 otherwise
	Batch       bool   // translate: a batch request (ctxbuild.TranslateBatchSystemTemplate)
	TargetIdxs  []int  // translate: every [TARGET #idx] in order (one for a single-sentence request)
	Auth        string // Authorization header
	Key         string // the key sent (Bearer token or x-api-key)

	// Anthropic only.
	MaxTokens    int
	Version      string // anthropic-version header
	CacheControl bool   // the system block carried cache_control {"type":"ephemeral"}
	RawUser      string // the user content as received
	Thinking     string // thinking.type ("" = no thinking field)
	Display      string // thinking.display ("" = not sent)
	Effort       string // output_config.effort ("" = not sent)
	Schema       string // output_config.format.schema, compact ("" = no format)
	Stream       bool   // "stream": true (answered with server-sent events)
}

// Hit is one chat request as it arrived (Hits), whatever it was answered
// with: unlike Requests it includes the ones SetError, RejectFormat,
// RequireKey and SetConcurrencyLimit refused.
type Hit struct {
	Path   string // URL path
	Format bool   // the body holds output_config.format
	Status int    // HTTP status of the answer
}

// Server is the fake LLM. Its URL is the provider base_url.
type Server struct {
	URL string

	srv *httptest.Server

	mu         sync.Mutex
	fixtures   map[string][]byte
	syllabus   string
	translate  translateFixture
	overrides  map[int]string
	responses  map[string]string                           // SetResponse
	handlers   map[string]func(system, user string) string // Handle
	fails      map[string][]Failure
	batchFails []Failure // FailNextBatch
	requests   []Request
	seen       map[string]bool // system prompts seen (cache-hit simulation)
	models     []string
	modelCalls int
	errStatus  int
	errMsg     string
	delay      time.Duration
	requireKey string
	// respHeaders: added to every answer (SetResponseHeaders).
	respHeaders http.Header
	// rejectFormat: output_config.format is answered with HTTP 400 (RejectFormat).
	rejectFormat bool
	// hits: every chat request that arrived, answered with an error or not (Hits).
	hits []Hit
	// streams: scripted streams per role (StreamNext); streamHook: SetStreamHook.
	streams    map[string][]StreamMode
	streamHook func(role, event string)

	// perf-llm: concurrency limit and cache timing
	concLimit   int
	inflight    int
	maxInflight int
	throttled   int
	cacheAfter  bool
	built       map[string]bool // SetCacheAfterAnswer: system prompts answered once
	cacheMisses int             // SetCacheAfterAnswer: chat requests whose system prompt missed
}

type translateFixture struct {
	Default string            `json:"default"`
	ByIdx   map[string]string `json:"by_idx"`
	ByText  map[string]string `json:"by_text"`
}

// FixtureDir returns the repository's testdata/llm directory.
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "llm")
}

// New starts a server with the fixtures of testdata/llm; it is closed by
// tb.Cleanup.
func New(tb testing.TB) *Server { return NewFromDir(tb, FixtureDir()) }

// NewFromDir starts a server with the fixtures of dir.
func NewFromDir(tb testing.TB, dir string) *Server {
	tb.Helper()
	s := &Server{
		fixtures:  map[string][]byte{},
		overrides: map[int]string{},
		responses: map[string]string{},
		handlers:  map[string]func(system, user string) string{},
		fails:     map[string][]Failure{},
		streams:   map[string][]StreamMode{},
		seen:      map[string]bool{},
		built:     map[string]bool{},
		models:    append([]string(nil), DefaultModels...),
		syllabus:  DefaultSyllabusMarkdown,
	}
	for _, role := range Roles {
		b, err := os.ReadFile(filepath.Join(dir, role+".json"))
		if err != nil {
			tb.Fatalf("llmtest: %v", err)
		}
		if !json.Valid(b) {
			tb.Fatalf("llmtest: %s.json is not valid JSON", role)
		}
		s.fixtures[role] = b
	}
	if b, err := os.ReadFile(filepath.Join(dir, RoleSyllabusFormat+".md")); err == nil {
		s.syllabus = string(b)
	}
	for role, def := range optionalFixtures {
		s.fixtures[role] = []byte(def)
		if b, err := os.ReadFile(filepath.Join(dir, role+".json")); err == nil {
			if !json.Valid(b) {
				tb.Fatalf("llmtest: %s.json is not valid JSON", role)
			}
			s.fixtures[role] = b
		}
	}
	if err := json.Unmarshal(s.fixtures[RoleTranslate], &s.translate); err != nil {
		tb.Fatalf("llmtest: translate.json: %v", err)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	tb.Cleanup(s.Close)
	return s
}

// Close stops the server (idempotent). The clients' connections are closed
// first (review R18 suggestion 5): a handler hanging on a request (FailHang,
// a hanging stream) returns at once, instead of when the client's attempt
// timeout (two minutes or more) ends it.
func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// Configure points every provider of cfg at the server with TestKey.
func (s *Server) Configure(cfg *config.Config) {
	for name, p := range cfg.LLM.Providers {
		p.BaseURL = s.URL
		p.APIKey = TestKey
		p.APIKeyEnv = ""
		cfg.LLM.Providers[name] = p
	}
}

// ConfigureSettings is Configure for the v1.1 settings service: it creates
// a provider named name of type typ (model.ProviderOpenAI or
// model.ProviderAnthropic) pointing at the server with TestKey — or updates
// the existing provider of that name — and assigns every role to it with
// modelName (role params left at their defaults).
func (s *Server) ConfigureSettings(ctx context.Context, set *settings.Service, name, typ, modelName string) (model.Provider, error) {
	key, url := TestKey, s.URL
	in := settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &url, APIKey: &key}
	var p model.Provider
	var err error
	found := false
	for _, ex := range set.Providers() {
		if ex.Name == name {
			p, err = set.UpdateProvider(ctx, ex.ID, in)
			found = true
			break
		}
	}
	if !found {
		p, err = set.CreateProvider(ctx, in)
	}
	if err != nil {
		return model.Provider{}, err
	}
	for _, role := range model.Roles {
		if err := set.SetRole(ctx, role, p.ID, modelName, model.RoleParams{}); err != nil {
			return model.Provider{}, err
		}
	}
	return p, nil
}

// FailNext makes the next n requests of role fail with mode (queued after
// failures injected earlier).
func (s *Server) FailNext(role string, n int, mode Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.fails[role] = append(s.fails[role], mode)
	}
}

// FailNextBatch makes the next n batch translate requests fail with mode
// (queued after batch failures injected earlier). A batch request takes a
// failure from this queue first and from the translate role's FailNext
// queue only when this one is empty; single-sentence requests never take
// from it.
func (s *Server) FailNextBatch(n int, mode Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.batchFails = append(s.batchFails, mode)
	}
}

// SetError makes every request (chat and model lists) answer HTTP status
// with message until SetError(0, "").
func (s *Server) SetError(status int, message string) {
	s.mu.Lock()
	s.errStatus, s.errMsg = status, message
	s.mu.Unlock()
}

// SetDelay delays every answer by d (the request's cancellation is honoured).
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

// SetResponseHeaders adds h to every answer from now on, errors included
// (e.g. Anthropic's anthropic-ratelimit-* headers); nil = none.
func (s *Server) SetResponseHeaders(h http.Header) {
	s.mu.Lock()
	s.respHeaders = h.Clone()
	s.mu.Unlock()
}

// RequireKey makes requests that do not send key fail with 401; the error
// message quotes the key that was sent, like real servers do ("" = off).
func (s *Server) RequireKey(key string) {
	s.mu.Lock()
	s.requireKey = key
	s.mu.Unlock()
}

// RejectFormat makes the Anthropic endpoint answer HTTP 400 to every request
// with output_config.format, like a model without structured output.
func (s *Server) RejectFormat(on bool) {
	s.mu.Lock()
	s.rejectFormat = on
	s.mu.Unlock()
}

// CheckSchema applies the rules of Claude's structured output to a JSON
// Schema: every object has "additionalProperties": false and lists all its
// properties as required; no length, range or array-size constraints. It
// returns what is wrong ("" = nothing).
func CheckSchema(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "schema is not JSON: " + err.Error()
	}
	return checkSchemaNode(v, "schema")
}

func checkSchemaNode(v any, path string) string {
	m, ok := v.(map[string]any)
	if !ok {
		return path + ": not an object"
	}
	for _, k := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "pattern"} {
		if _, has := m[k]; has {
			return path + ": " + k + " is not supported"
		}
	}
	switch m["type"] {
	case "object":
		if ap, has := m["additionalProperties"]; !has || ap != false {
			return path + ": additionalProperties must be false"
		}
		props, _ := m["properties"].(map[string]any)
		req := map[string]bool{}
		if list, ok := m["required"].([]any); ok {
			for _, r := range list {
				if name, ok := r.(string); ok {
					req[name] = true
				}
			}
		}
		for name, p := range props {
			if !req[name] {
				return path + ": property " + name + " is not required"
			}
			if msg := checkSchemaNode(p, path+"."+name); msg != "" {
				return msg
			}
		}
		if len(req) != len(props) {
			return path + ": required lists a name that is not a property"
		}
	case "array":
		if msg := checkSchemaNode(m["items"], path+".items"); msg != "" {
			return msg
		}
	case "string", "integer", "number", "boolean", "null":
	default:
		return path + ": type missing or unknown"
	}
	return ""
}

// SetModels sets the list returned by GET /models and /v1/models.
func (s *Server) SetModels(ids []string) {
	s.mu.Lock()
	s.models = append([]string(nil), ids...)
	s.mu.Unlock()
}

// ModelListCalls returns the number of GET /models requests received.
func (s *Server) ModelListCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelCalls
}

// SetTranslation overrides the translation returned for sentence idx.
func (s *Server) SetTranslation(idx int, zh string) {
	s.mu.Lock()
	s.overrides[idx] = zh
	s.mu.Unlock()
}

// Handle answers every later request of role with fn(system, user) (nil
// removes the handler). fn runs outside the server lock, possibly on several
// goroutines at once. It wins over SetResponse and the fixtures; FailNext,
// SetError and RequireKey still apply.
func (s *Server) Handle(role string, fn func(system, user string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn == nil {
		delete(s.handlers, role)
		return
	}
	s.handlers[role] = fn
}

// SetResponse overrides the raw content returned for a non-translate role.
func (s *Server) SetResponse(role, content string) {
	s.mu.Lock()
	s.responses[role] = content
	s.mu.Unlock()
}

// Count returns the number of requests received for role (failed ones included).
func (s *Server) Count(role string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Role == role {
			n++
		}
	}
	return n
}

// Requests returns the requests received for role ("" = all), in arrival order.
func (s *Server) Requests(role string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.requests {
		if role == "" || r.Role == role {
			out = append(out, r)
		}
	}
	return out
}

// TranslateIdxs returns the target idx of every translate request, in
// arrival order (duplicates kept: a QA retry appears twice). A batch request
// contributes its first target; BatchRequests lists them with all targets.
func (s *Server) TranslateIdxs() []int {
	var out []int
	for _, r := range s.Requests(RoleTranslate) {
		out = append(out, r.TargetIdx)
	}
	return out
}

// BatchRequests returns the batch translate requests, in arrival order.
func (s *Server) BatchRequests() []Request {
	var out []Request
	for _, r := range s.Requests(RoleTranslate) {
		if r.Batch {
			out = append(out, r)
		}
	}
	return out
}

// Reset forgets the recorded requests and pending failures (fixtures and
// overrides are kept).
func (s *Server) Reset() {
	s.mu.Lock()
	s.requests = nil
	s.hits = nil
	s.fails = map[string][]Failure{}
	s.streams = map[string][]StreamMode{}
	s.batchFails = nil
	s.modelCalls = 0
	s.mu.Unlock()
}

// RoleOf identifies the role of a system prompt by its first line.
func RoleOf(system string) string {
	first := ctxbuild.FirstLine(system)
	switch first {
	case ctxbuild.FirstLine(ctxbuild.GlossarySeedSystem):
		return RoleGlossarySeed
	case ctxbuild.FirstLine(ctxbuild.MaterialTermsSystem):
		return RoleMaterialTerms
	case ctxbuild.FirstLine(ctxbuild.SummarySystemTemplate):
		return RoleSummary
	case ctxbuild.FirstLine(ctxbuild.RepairSystemTemplate):
		return RoleRepair
	case ctxbuild.FirstLine(ctxbuild.TranslateSystemTemplate), ctxbuild.FirstLine(ctxbuild.TranslateBatchSystemTemplate):
		return RoleTranslate
	case ctxbuild.FirstLine(ctxbuild.SyllabusFormatSystem):
		return RoleSyllabusFormat
	case ctxbuild.FirstLine(ctxbuild.JanitorSystem):
		return RoleGlossaryJanitor
	case ctxbuild.FirstLine(ctxbuild.ChaptersSystem):
		return RoleChapters
	case ctxbuild.FirstLine(ctxbuild.TitleSystem):
		return RoleTitle
	case ctxbuild.FirstLine(ctxbuild.FilenameRuleSystem):
		return RoleFilenameRule
	}
	return ""
}

// IsBatchSystem reports whether a system prompt is the one of a batch
// translate request (ctxbuild.TranslateBatchSystemTemplate).
func IsBatchSystem(system string) bool {
	return ctxbuild.FirstLine(system) == ctxbuild.FirstLine(ctxbuild.TranslateBatchSystemTemplate)
}

// targetRE accepts the SPEC-v2 G5.3 label and the v1 one.
var targetRE = regexp.MustCompile(`(?m)^\[(?:TARGET|目标) #(\d+)\]$`)

// TargetIdx returns the idx of the "[TARGET #idx]" line of a translate user message, or -1.
func TargetIdx(user string) int {
	m := targetRE.FindStringSubmatch(user)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// TargetText returns the line after "[TARGET #idx]".
func TargetText(user string) string {
	loc := targetRE.FindStringIndex(user)
	if loc == nil {
		return ""
	}
	rest := strings.TrimPrefix(user[loc[1]:], "\n")
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// TargetIdxs returns the idx of every "[TARGET #idx]" line of a translate
// user message, in order (nil when there is none).
func TargetIdxs(user string) []int {
	var out []int
	for _, m := range targetRE.FindAllStringSubmatch(user, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// TargetTexts returns the line after every "[TARGET #idx]" line, in the
// order of TargetIdxs.
func TargetTexts(user string) []string {
	var out []string
	for _, loc := range targetRE.FindAllStringIndex(user, -1) {
		rest := strings.TrimPrefix(user[loc[1]:], "\n")
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[:i]
		}
		out = append(out, rest)
	}
	return out
}

type wireRequest struct {
	Model          string `json:"model"`
	Messages       []struct{ Role, Content string }
	Temperature    *float64          `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format"`
}

// SetConcurrencyLimit makes the server answer HTTP 429 (without
// Retry-After) to a chat request that arrives while n chat requests are being
// answered, like DeepSeek's account-level concurrency limit (perf-llm); 0 =
// no limit. Throttled counts these answers, MaxInflight the peak.
func (s *Server) SetConcurrencyLimit(n int) {
	s.mu.Lock()
	s.concLimit = n
	s.mu.Unlock()
}

// Throttled returns the number of 429 answers of SetConcurrencyLimit.
func (s *Server) Throttled() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.throttled
}

// MaxInflight returns the most chat requests answered at the same time
// (requests refused by SetConcurrencyLimit do not count).
func (s *Server) MaxInflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInflight
}

// SetCacheAfterAnswer switches the OpenAI-compatible cache simulation: on, a
// system prompt counts as a cache hit only for requests that arrived after a
// request with the same system prompt was answered (DeepSeek builds the
// cache from a request it processed; requests sent together all miss); off
// (the default), once a request with it was seen.
func (s *Server) SetCacheAfterAnswer(on bool) {
	s.mu.Lock()
	s.cacheAfter = on
	s.mu.Unlock()
}

type arrivalHitKey struct{}

// beginChat counts a chat request in flight; ok is false when the
// concurrency limit refuses it. hit is the arrival-time cache state of the
// request's system prompt (SetCacheAfterAnswer; nil when off).
func (s *Server) beginChat(body []byte) (ok bool, hit *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.concLimit > 0 && s.inflight >= s.concLimit {
		s.throttled++
		return false, nil
	}
	s.inflight++
	s.maxInflight = max(s.maxInflight, s.inflight)
	if s.cacheAfter {
		var wr wireRequest
		if json.Unmarshal(body, &wr) == nil && len(wr.Messages) == 2 && wr.Messages[0].Role == "system" {
			h := s.built[wr.Messages[0].Content]
			hit = &h
			if !h {
				s.cacheMisses++
			}
		}
	}
	return true, hit
}

// CacheMisses returns the chat requests whose system prompt was not cached
// when they arrived (SetCacheAfterAnswer only).
func (s *Server) CacheMisses() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cacheMisses
}

func (s *Server) endChat() {
	s.mu.Lock()
	s.inflight--
	s.mu.Unlock()
}

// anthropicJSONInstruction mirrors llm.JSONInstruction (A6).
const anthropicJSONInstruction = "\n\nRespond with JSON only."

// Hits returns every chat request received (refused ones included), in
// arrival order.
func (s *Server) Hits() []Hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Hit(nil), s.hits...)
}

// statusWriter writes the status of an answer into its Hit before the
// client can see the answer (Hits).
type statusWriter struct {
	http.ResponseWriter
	set  func(status int)
	done bool
}

func (w *statusWriter) mark(code int) {
	if !w.done {
		w.done = true
		w.set(code)
	}
}

func (w *statusWriter) WriteHeader(code int) {
	w.mark(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.mark(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and Hijack (streams).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Read the body first: the server notices a client hanging up (e.g. a
	// timed-out probe during SetDelay) only once the body has been consumed.
	body, _ := io.ReadAll(r.Body)
	if r.Method == http.MethodPost {
		var probe struct {
			OutputConfig *struct {
				Format json.RawMessage `json:"format"`
			} `json:"output_config"`
		}
		_ = json.Unmarshal(body, &probe)
		s.mu.Lock()
		i := len(s.hits)
		s.hits = append(s.hits, Hit{Path: r.URL.Path, Format: probe.OutputConfig != nil && len(probe.OutputConfig.Format) > 0})
		s.mu.Unlock()
		w = &statusWriter{ResponseWriter: w, set: func(status int) {
			s.mu.Lock()
			if i < len(s.hits) { // Reset may have dropped it
				s.hits[i].Status = status
			}
			s.mu.Unlock()
		}}
	}
	s.mu.Lock()
	delay, errStatus, errMsg, requireKey := s.delay, s.errStatus, s.errMsg, s.requireKey
	for k, v := range s.respHeaders {
		w.Header()[k] = append([]string(nil), v...)
	}
	s.mu.Unlock()
	anthropic := strings.HasSuffix(r.URL.Path, "/v1/messages") || r.Header.Get("anthropic-version") != ""
	if r.Method == http.MethodPost {
		ok, hit := s.beginChat(body)
		if !ok {
			writeAPIErr(w, anthropic, http.StatusTooManyRequests, "Too many concurrent requests (llmtest concurrency limit)")
			return
		}
		defer s.endChat()
		if hit != nil {
			r = r.WithContext(context.WithValue(r.Context(), arrivalHitKey{}, *hit))
		}
	}
	if delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-r.Context().Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
	if errStatus != 0 {
		writeAPIErr(w, anthropic, errStatus, errMsg)
		return
	}
	if requireKey != "" {
		if got := keyOf(r); got != requireKey {
			writeAPIErr(w, anthropic, http.StatusUnauthorized, "Incorrect API key provided: "+got)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		s.handleModels(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages"):
		s.handleAnthropic(w, r, body)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
		s.handleOpenAI(w, r, body)
	default:
		writeAPIErr(w, anthropic, http.StatusNotFound, "not found")
	}
}

func keyOf(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.modelCalls++
	ids := append([]string(nil), s.models...)
	s.mu.Unlock()
	sort.Strings(ids)
	start := 0
	if after := r.URL.Query().Get("after_id"); after != "" {
		for i, id := range ids {
			if id == after {
				start = i + 1
			}
		}
	}
	end := len(ids)
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && start+n < end {
		end = start + n
	}
	page := ids[start:end]
	data := []any{}
	for _, id := range page {
		data = append(data, map[string]any{"id": id, "type": "model", "object": "model", "display_name": id})
	}
	resp := map[string]any{"object": "list", "data": data, "has_more": end < len(ids)}
	if len(page) > 0 {
		resp["first_id"], resp["last_id"] = page[0], page[len(page)-1]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleOpenAI(w http.ResponseWriter, r *http.Request, body []byte) {
	var wr wireRequest
	if err := json.Unmarshal(body, &wr); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req := Request{API: model.ProviderOpenAI, Model: wr.Model, TargetIdx: -1,
		JSONMode: wr.ResponseFormat["type"] == "json_object",
		Auth:     r.Header.Get("Authorization"), Key: keyOf(r)}
	if wr.Temperature != nil {
		req.Temperature, req.HasTemp = *wr.Temperature, true
	}
	switch {
	case len(wr.Messages) == 1 && wr.Messages[0].Role == "user" && wr.Messages[0].Content == "ping":
		req.User, req.Role = "ping", RolePing
	case len(wr.Messages) == 2 && wr.Messages[0].Role == "system" && wr.Messages[1].Role == "user":
		req.System, req.User = wr.Messages[0].Content, wr.Messages[1].Content
		req.Role = RoleOf(req.System)
	default:
		writeErr(w, http.StatusBadRequest, "expected exactly two messages: system, user")
		return
	}
	content, fail, cacheHit, status := s.answer(&req)
	if status != 0 {
		writeFailure(w, false, status, content, fail)
		return
	}
	if fail != nil && *fail == FailHang {
		<-r.Context().Done()
		return
	}
	if h, ok := r.Context().Value(arrivalHitKey{}).(bool); ok {
		cacheHit = h // SetCacheAfterAnswer: the cache state when the request arrived
		s.mu.Lock()
		s.built[req.System] = true
		s.mu.Unlock()
	}
	finish := "stop"
	if c, cut := cutTruncated(content); cut {
		content, finish = c, "length"
	}
	if fail != nil && *fail == FailRefusal {
		// The content filter stopped the answer halfway (FailRefusal).
		runes := []rune(content)
		content, finish = string(runes[:len(runes)/2]), "content_filter"
	}
	prompt := int64(utf8.RuneCountInString(req.System)+utf8.RuneCountInString(req.User))/4 + 1
	var hit int64
	if cacheHit {
		hit = int64(utf8.RuneCountInString(req.System)) / 4
	}
	completion := int64(utf8.RuneCountInString(content))/4 + 1
	resp := map[string]any{
		"id":      "fake",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]any{
			"prompt_tokens":            prompt,
			"completion_tokens":        completion,
			"total_tokens":             prompt + completion,
			"prompt_cache_hit_tokens":  hit,
			"prompt_cache_miss_tokens": prompt - hit,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type anthropicWire struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       json.RawMessage `json:"system"`
	Temperature  *float64        `json:"temperature"`
	Stream       bool            `json:"stream"`
	Thinking     map[string]any  `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"output_config"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type anthropicBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control"`
}

// blocksText reads a string or an array of text blocks.
func blocksText(raw json.RawMessage) (text string, cache bool, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false, true
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false, false
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" {
			return "", false, false
		}
		sb.WriteString(b.Text)
		if b.CacheControl != nil && b.CacheControl.Type == "ephemeral" {
			cache = true
		}
	}
	return sb.String(), cache, true
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request, body []byte) {
	var wr anthropicWire
	if err := json.Unmarshal(body, &wr); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	if r.Header.Get("anthropic-version") == "" {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	if wr.MaxTokens <= 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "max_tokens: Field required")
		return
	}
	system, cache, ok := blocksText(wr.System)
	if !ok || len(wr.Messages) != 1 || wr.Messages[0].Role != "user" {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "expected a system prompt and exactly one user message")
		return
	}
	user, _, ok := blocksText(wr.Messages[0].Content)
	if !ok {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "invalid user content")
		return
	}
	req := Request{API: model.ProviderAnthropic, Model: wr.Model, System: system, RawUser: user, User: user,
		TargetIdx: -1, Auth: r.Header.Get("Authorization"), Key: keyOf(r),
		MaxTokens: wr.MaxTokens, Version: r.Header.Get("anthropic-version"), CacheControl: cache, Stream: wr.Stream}
	if wr.Temperature != nil {
		req.Temperature, req.HasTemp = *wr.Temperature, true
	}
	if wr.Thinking != nil {
		req.Thinking, _ = wr.Thinking["type"].(string)
		if d, has := wr.Thinking["display"]; has {
			req.Display, _ = d.(string)
			if req.Display != "summarized" && req.Display != "omitted" {
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "thinking.display: unknown value")
				return
			}
		}
		if req.Thinking == "between_tools" {
			id := strings.ToLower(wr.Model)
			switch {
			case id != "claude-sonnet-5-5" && !strings.HasPrefix(id, "claude-sonnet-5-5-"):
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "thinking.type: between_tools is not supported for this model")
				return
			case len(wr.Thinking) != 1:
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "thinking: between_tools takes no other field")
				return
			}
		}
		if req.Thinking == "disabled" {
			// The models whose thinking cannot be switched off refuse it
			// (Anthropic's thinking page: Opus 5.5, Sonnet 5.5, Fable, Mythos).
			id := strings.ToLower(wr.Model)
			for _, p := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-", "claude-mythos-"} {
				if strings.HasPrefix(id, p) {
					writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", `"thinking.type.disabled" is not supported for this model`)
					return
				}
			}
			if len(wr.Thinking) != 1 {
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "thinking: disabled takes no other field")
				return
			}
		}
	}
	if oc := wr.OutputConfig; oc != nil {
		req.Effort = oc.Effort
		if (req.Thinking == "between_tools" || req.Thinking == "disabled") && (oc.Effort == "xhigh" || oc.Effort == "max") {
			writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "thinking.type: "+req.Thinking+" needs effort high or below")
			return
		}
		if f := oc.Format; f != nil {
			s.mu.Lock()
			reject := s.rejectFormat
			s.mu.Unlock()
			if reject {
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "output_config.format: Extra inputs are not permitted")
				return
			}
			if f.Type != "json_schema" {
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "output_config.format.type: expected json_schema")
				return
			}
			if msg := CheckSchema(f.Schema); msg != "" {
				writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "output_config.format.schema: "+msg)
				return
			}
			var compact bytes.Buffer
			_ = json.Compact(&compact, f.Schema)
			req.Schema = compact.String()
		}
	}
	if strings.HasSuffix(user, anthropicJSONInstruction) {
		req.JSONMode = true
		req.User = strings.TrimSuffix(user, anthropicJSONInstruction)
	}
	if system == "" && req.User == "ping" {
		req.Role = RolePing
	} else {
		req.Role = RoleOf(system)
	}
	content, fail, cacheHit, status := s.answer(&req)
	if status != 0 {
		writeFailure(w, true, status, content, fail)
		return
	}
	if fail != nil && *fail == FailHang {
		<-r.Context().Done()
		return
	}
	stop := "end_turn"
	if c, cut := cutTruncated(content); cut {
		content, stop = c, "max_tokens"
	}
	refused := fail != nil && *fail == FailRefusal
	if refused {
		content, stop = "", "refusal"
	}
	sysTok := int64(utf8.RuneCountInString(system)) / 4
	input := int64(utf8.RuneCountInString(req.RawUser))/4 + 1
	var read, creation int64
	switch {
	case cache && cacheHit:
		read = sysTok
	case cache:
		creation = sysTok
	default:
		input += sysTok
	}
	resp := map[string]any{
		"id":          "msg_fake",
		"type":        "message",
		"role":        "assistant",
		"model":       req.Model,
		"stop_reason": stop,
		"content": []any{
			map[string]any{"type": "thinking", "thinking": ""}, // always-thinking models: skipped by the client
			map[string]any{"type": "text", "text": content},
		},
		"usage": map[string]any{
			"input_tokens":                input,
			"output_tokens":               int64(utf8.RuneCountInString(content))/4 + 1,
			"cache_read_input_tokens":     read,
			"cache_creation_input_tokens": creation,
		},
	}
	if refused {
		resp["content"] = []any{}
		resp["stop_details"] = map[string]any{"type": "refusal", "category": RefusalCategory, "explanation": "llmtest: injected refusal"}
	}
	if wr.Stream {
		s.writeStream(w, r, req, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// answer records req and returns its content, applying injected failures.
// status != 0 means "answer with this error" (content is the message).
func (s *Server) answer(req *Request) (content string, fail *Failure, cacheHit bool, status int) {
	if req.Role == RoleTranslate {
		req.TargetIdx = TargetIdx(req.User)
		req.TargetIdxs = TargetIdxs(req.User)
		req.Batch = IsBatchSystem(req.System)
	}
	s.mu.Lock()
	s.requests = append(s.requests, *req)
	if req.Batch && len(s.batchFails) > 0 {
		f := s.batchFails[0]
		fail = &f
		s.batchFails = s.batchFails[1:]
	} else if q := s.fails[req.Role]; len(q) > 0 {
		f := q[0]
		fail = &f
		s.fails[req.Role] = q[1:]
	}
	cacheHit = s.seen[req.System]
	s.seen[req.System] = true
	h := s.handlers[req.Role]
	var cerr error
	if h == nil || req.Role == "" {
		content, cerr = s.content(*req)
	}
	s.mu.Unlock()
	if h != nil && req.Role != "" {
		content = h(req.System, req.User)
	}

	if req.Role == "" {
		return "llmtest: unknown role, first line: " + ctxbuild.FirstLine(req.System), nil, false, http.StatusBadRequest
	}
	if cerr != nil {
		return cerr.Error(), nil, false, http.StatusInternalServerError
	}
	if fail != nil {
		switch *fail {
		case FailHTTP500:
			return "injected failure", fail, false, http.StatusInternalServerError
		case FailHTTP429:
			return "injected rate limit", fail, false, http.StatusTooManyRequests
		case FailHTTP401:
			return "injected authentication failure", fail, false, http.StatusUnauthorized
		case FailHTTP403:
			return "injected permission failure", fail, false, http.StatusForbidden
		case FailHTTP529:
			return "Overloaded", fail, false, 529
		case FailNotJSON:
			content = "Sorry, I cannot produce JSON right now."
		case FailWrongIdx:
			switch {
			case req.Batch:
				content = mangleBatch(content, FailBatchWrongIds)
			case req.Role == RoleTranslate:
				content = fmt.Sprintf(`{"idx": %d, "zh": "错误的句子"}`, req.TargetIdx+WrongIdxOffset)
			}
		case FailBatchMissing, FailBatchExtra, FailBatchDuplicate, FailBatchWrongIds, FailBatchShuffled, FailBatchBareArray:
			if req.Batch {
				content = mangleBatch(content, *fail)
			}
		case FailTruncated:
			r := []rune(content)
			content = Truncated(string(r[:len(r)/2]))
		case FailFenced:
			content = "```json\n" + content + "\n```"
		case FailSecondJSON:
			content += SecondJSONTail
		case FailTrailingText:
			content += TrailingText
		}
	}
	return content, fail, cacheHit, 0
}

// batchItem is one item of a batch translate answer.
type batchItem struct {
	Idx int    `json:"idx"`
	Zh  string `json:"zh"`
}

// mangleBatch applies a FailBatch* mode to a batch answer {"items": […]}
// (content that does not parse as one is returned unchanged).
func mangleBatch(content string, mode Failure) string {
	var a struct {
		Items []batchItem `json:"items"`
	}
	if json.Unmarshal([]byte(content), &a) != nil || len(a.Items) == 0 {
		return content
	}
	items := a.Items
	switch mode {
	case FailBatchMissing:
		items = items[:len(items)-1]
	case FailBatchExtra:
		items = append(items, batchItem{Idx: items[len(items)-1].Idx + WrongIdxOffset, Zh: "多余的句子"})
	case FailBatchDuplicate:
		items = append(items, items[0])
	case FailBatchWrongIds:
		for i := range items {
			items[i].Idx += WrongIdxOffset
		}
	case FailBatchShuffled:
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	case FailBatchBareArray:
		b, _ := json.Marshal(items)
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

// translationOf returns the canned translation of target idx with text
// (s.mu held): SetTranslation override, by_text, by_idx, default.
func (s *Server) translationOf(idx int, text string) string {
	zh, ok := s.overrides[idx]
	if !ok {
		zh, ok = s.translate.ByText[text]
	}
	if !ok {
		zh, ok = s.translate.ByIdx[strconv.Itoa(idx)]
	}
	if !ok {
		zh = s.translate.Default
	}
	return zh
}

// writeFailure writes an injected or validation error in the API's format.
func writeFailure(w http.ResponseWriter, anthropic bool, status int, msg string, fail *Failure) {
	if fail != nil && *fail == FailHTTP429 {
		w.Header().Set("Retry-After", "0")
	}
	writeAPIErr(w, anthropic, status, msg)
}

// content returns the normal answer for req (s.mu held).
func (s *Server) content(req Request) (string, error) {
	switch req.Role {
	case "":
		return "", nil
	case RolePing:
		return "pong", nil
	case RoleTranslate:
		if req.TargetIdx < 0 {
			return "", fmt.Errorf("llmtest: translate request without [TARGET #idx]")
		}
		if req.Batch {
			texts := TargetTexts(req.User)
			items := make([]batchItem, len(req.TargetIdxs))
			for i, idx := range req.TargetIdxs {
				items[i] = batchItem{Idx: idx, Zh: s.translationOf(idx, texts[i])}
			}
			b, _ := json.Marshal(map[string]any{"items": items})
			return string(b), nil
		}
		b, _ := json.Marshal(map[string]any{"idx": req.TargetIdx, "zh": s.translationOf(req.TargetIdx, TargetText(req.User))})
		return string(b), nil
	}
	if c, ok := s.responses[req.Role]; ok {
		return c, nil
	}
	switch req.Role {
	case RoleSummary:
		var f struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(s.fixtures[RoleSummary], &f); err != nil {
			return "", err
		}
		return f.Summary, nil
	case RoleSyllabusFormat:
		return s.syllabus, nil
	}
	return string(s.fixtures[req.Role]), nil
}

func writeAPIErr(w http.ResponseWriter, anthropic bool, code int, msg string) {
	if anthropic {
		typ := "api_error"
		switch code {
		case http.StatusBadRequest:
			typ = "invalid_request_error"
		case http.StatusUnauthorized:
			typ = "authentication_error"
		case http.StatusForbidden:
			typ = "permission_error"
		case http.StatusNotFound:
			typ = "not_found_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		case 529:
			typ = "overloaded_error"
		}
		writeAnthropicErr(w, code, typ, msg)
		return
	}
	writeErr(w, code, msg)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "llmtest"}})
	_, _ = w.Write(b)
}

func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
	_, _ = w.Write(b)
}
