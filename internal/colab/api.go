// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The Colab session backend ("tunnel" API) as google-colab-cli 0.7.5 uses
// it (colab_cli/client.py: Client._issue_request, assign, _get_assignment,
// _post_assignment, unassign, list_assignments, get_consumption_user_info;
// colab_cli/consumption.py: CcuInfo). See UPSTREAM.md.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultColabURL is upstream's Prod().domain.
const DefaultColabURL = "https://colab.research.google.com"

const (
	tunPrefix  = "/tun/m"
	xssiPrefix = ")]}'\n"
	// defaultClientAgent is our X-Colab-Client-Agent. Upstream sends
	// "colab-cli"; LectureFlow names itself instead of posing as the
	// official CLI (NOTES: to be confirmed by the first real recording).
	defaultClientAgent = "lectureflow"
	tunnelTimeout      = 30 * time.Second
	// assignTimeout: "Slow allocations may outlive AuthorizedSession's
	// default read timeout" (upstream _issue_request).
	assignTimeout = 120 * time.Second
	maxBodyBytes  = 1 << 20
)

// apiChanged is CodeAPIChanged: the response does not look like what
// upstream expects; the lecture goes back to the local devices (V14.6).
func apiChanged(endpoint, detail string) error {
	return fallbackErr(CodeAPIChanged, map[string]any{"endpoint": endpoint, "detail": detail},
		fmt.Errorf("Colab 接口可能已变更（%s：%s）", endpoint, detail))
}

// httpError is a non-2xx answer the caller interprets (412, 400, 5xx).
type httpError struct {
	endpoint string
	status   int
}

func (e *httpError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.endpoint, e.status) }

func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.status
	}
	return 0
}

// transient: worth retrying later (upstream retries 502/503/504 and read
// timeouts; 429 and 500 are treated the same here).
func transient(err error) bool {
	switch statusOf(err) {
	case 429, 500, 502, 503, 504:
		return true
	}
	var ne netError
	if errors.As(err, &ne) {
		return true
	}
	var pe *netError // the WebSocket handshake's (ws.go)
	return errors.As(err, &pe)
}

// netError marks a transport failure (no HTTP answer).
type netError struct{ text string }

func (e netError) Error() string { return e.text }

// tunnel talks to {colab}/tun/m with the user's access token.
type tunnel struct {
	base    string
	hc      *http.Client
	auth    *authManager
	agent   string
	secrets *secretSet
	log     *slog.Logger
	// unknownGPU: accelerator names of ccu-info already logged as unknown
	unknownGPU sync.Map
}

// obj is a decoded JSON object with strict accessors: a missing or mistyped
// field is CodeAPIChanged.
type obj struct {
	endpoint string
	m        map[string]json.RawMessage
}

func (o obj) raw(key string) (json.RawMessage, error) {
	v, ok := o.m[key]
	if !ok || string(v) == "null" {
		return nil, apiChanged(o.endpoint, "field "+key+" missing")
	}
	return v, nil
}

func (o obj) str(key string) (string, error) {
	v, err := o.raw(key)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", apiChanged(o.endpoint, "field "+key+" is not a string")
	}
	return s, nil
}

func (o obj) num(key string) (float64, error) {
	v, err := o.raw(key)
	if err != nil {
		return 0, err
	}
	var f float64
	if json.Unmarshal(v, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		// pydantic accepts numeric strings for int/float fields
		var s string
		if json.Unmarshal(v, &s) == nil {
			if g, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(g) && !math.IsInf(g, 0) {
				return g, nil
			}
		}
		return 0, apiChanged(o.endpoint, "field "+key+" is not a number")
	}
	return f, nil
}

func (o obj) integer(key string) (int, error) {
	f, err := o.num(key)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) || math.Abs(f) > 1e12 {
		return 0, apiChanged(o.endpoint, "field "+key+" is not an integer")
	}
	return int(f), nil
}

func (o obj) object(key string) (obj, error) {
	v, err := o.raw(key)
	if err != nil {
		return obj{}, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil || m == nil {
		return obj{}, apiChanged(o.endpoint, "field "+key+" is not an object")
	}
	return obj{endpoint: o.endpoint, m: m}, nil
}

func (o obj) has(key string) bool {
	v, ok := o.m[key]
	return ok && string(v) != "null"
}

// do performs one tunnel request (upstream Client._issue_request): Accept
// and X-Colab-Client-Agent headers, authuser=0, the access token; strips
// the XSSI prefix. A 401 drops the access token and retries once with a
// fresh one. Non-2xx answers are *httpError (412, 400, 429, 5xx) or, for
// statuses upstream never sees in normal operation, CodeAPIChanged. An
// empty body returns ok false.
func (t *tunnel) do(ctx context.Context, method, path, endpoint string, q url.Values, hdr map[string]string, timeout time.Duration) (obj, bool, error) {
	for attempt := 0; ; attempt++ {
		o, ok, err := t.once(ctx, method, path, endpoint, q, hdr, timeout)
		if s := statusOf(err); (s == 401 || s == 403) && attempt == 0 {
			t.auth.forget()
			continue
		} else if s == 401 || s == 403 {
			// A fresh token is refused as well: missing scope or account
			// problem; the user has to authorise again.
			return obj{}, false, authErr(CodeAuthRejected, map[string]any{"reason": "http_" + strconv.Itoa(s)},
				fmt.Sprintf("%s: HTTP %d", endpoint, s))
		}
		return o, ok, err
	}
}

func (t *tunnel) once(ctx context.Context, method, path, endpoint string, q url.Values, hdr map[string]string, timeout time.Duration) (obj, bool, error) {
	tok, err := t.auth.token(ctx)
	if err != nil {
		return obj{}, false, err
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("authuser", "0")
	u := strings.TrimRight(t.base, "/") + path + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return obj{}, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Colab-Client-Agent", t.agent)
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		if pctx := context.Cause(ctx); errors.Is(pctx, context.Canceled) {
			return obj{}, false, pctx
		}
		var rr *redirectRefused
		if errors.As(err, &rr) {
			return obj{}, false, apiChanged(endpoint, rr.Error())
		}
		return obj{}, false, netError{text: fmt.Sprintf("%s %s: %s", method, endpoint, netErrText(err))}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return obj{}, false, netError{text: fmt.Sprintf("%s %s: reading the answer: %s", method, endpoint, netErrText(err))}
	}
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
	case s == 400, s == 401, s == 403, s == 412, s == 429, s >= 500:
		return obj{}, false, &httpError{endpoint: endpoint, status: s}
	default:
		return obj{}, false, apiChanged(endpoint, "HTTP "+strconv.Itoa(s))
	}
	if len(body) > maxBodyBytes {
		return obj{}, false, apiChanged(endpoint, "answer too large")
	}
	body = bytes.TrimPrefix(body, []byte(xssiPrefix))
	if len(bytes.TrimSpace(body)) == 0 {
		return obj{endpoint: endpoint}, false, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return obj{}, false, apiChanged(endpoint, "answer is not a JSON object")
	}
	return obj{endpoint: endpoint, m: m}, true, nil
}

// ---- endpoints ----

// ccuInfo is GET /tun/m/ccu-info (upstream consumption.py CcuInfo): the
// three fields upstream requires, strictly; the newer ones leniently
// (account.go: a missing or unknown value never makes it CodeAPIChanged).
type ccuInfo struct {
	Balance     float64 // currentBalance (paid compute units)
	RatePerHour float64 // consumptionRateHourly (the whole account)
	Assignments int     // assignmentsCount
	// EligibleGPUs / IneligibleGPUs: the supported accelerators of
	// eligibleGpus / ineligibleGpus; EligibilityKnown: eligibleGpus was a
	// list (proto3 JSON leaves an empty list out: absent = unknown).
	EligibleGPUs, IneligibleGPUs []string
	EligibilityKnown             bool
	// FreeQuotaCU, FreeRefillAt: freeCcuQuotaInfo (nil when absent).
	FreeQuotaCU  *float64
	FreeRefillAt *time.Time
}

func (t *tunnel) ccuInfo(ctx context.Context) (ccuInfo, error) {
	const ep = "tun/m/ccu-info"
	o, ok, err := t.do(ctx, http.MethodGet, tunPrefix+"/ccu-info", ep, nil, nil, tunnelTimeout)
	if err != nil {
		return ccuInfo{}, t.unexpected(ep, err)
	}
	if !ok {
		return ccuInfo{}, apiChanged(ep, "empty answer")
	}
	var c ccuInfo
	if c.Balance, err = o.num("currentBalance"); err != nil {
		return ccuInfo{}, err
	}
	if c.RatePerHour, err = o.num("consumptionRateHourly"); err != nil {
		return ccuInfo{}, err
	}
	if c.Assignments, err = o.integer("assignmentsCount"); err != nil {
		return ccuInfo{}, err
	}
	readCCUExtras(o, &c, func(name string) {
		if _, seen := t.unknownGPU.LoadOrStore(name, true); !seen && t.log != nil {
			t.log.Info("colab: ccu-info lists an accelerator LectureFlow does not know; ignored", "name", name)
		}
	})
	return c, nil
}

// proxyInfo is runtimeProxyInfo (upstream RuntimeProxyInfo).
type proxyInfo struct {
	Token     string
	ExpiresIn time.Duration
	URL       *url.URL
}

// assignment is an assigned runtime (upstream PostAssignmentResponse,
// Assignment, ListedAssignment).
type assignment struct {
	Accelerator string // "" when the answer does not say (Assignment)
	Endpoint    string
	Variant     int // AssignmentVariant: 0 DEFAULT, 1 GPU, 2 TPU; -1 unknown
	Shape       int // Shape: 0 STANDARD, 1 HIGH_RAM; -1 unknown
	Proxy       proxyInfo
}

func (t *tunnel) proxyInfo(o obj, allowHTTP bool) (proxyInfo, error) {
	p, err := o.object("runtimeProxyInfo")
	if err != nil {
		return proxyInfo{}, err
	}
	tok, err := p.str("token")
	if err != nil {
		return proxyInfo{}, err
	}
	if tok == "" {
		return proxyInfo{}, apiChanged(o.endpoint, "field runtimeProxyInfo.token empty")
	}
	t.secrets.add(tok)
	exp, err := p.integer("tokenExpiresInSeconds")
	if err != nil {
		return proxyInfo{}, err
	}
	raw, err := p.str("url")
	if err != nil {
		return proxyInfo{}, err
	}
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" || !(u.Scheme == "https" || allowHTTP && u.Scheme == "http") {
		return proxyInfo{}, apiChanged(o.endpoint, "field runtimeProxyInfo.url is not an https URL")
	}
	return proxyInfo{Token: tok, ExpiresIn: time.Duration(exp) * time.Second, URL: u}, nil
}

func (t *tunnel) endpointField(o obj) (string, error) {
	ep, err := o.str("endpoint")
	if err != nil {
		return "", err
	}
	if ep == "" || len(ep) > 512 {
		return "", apiChanged(o.endpoint, "field endpoint empty or too long")
	}
	return ep, nil
}

func (t *tunnel) acceleratorField(o obj, key string) (string, error) {
	a, err := o.str(key)
	if err != nil {
		return "", err
	}
	if !slices.Contains(knownAccelerators, a) {
		return "", apiChanged(o.endpoint, "unknown accelerator "+safeToken(a))
	}
	return a, nil
}

func (t *tunnel) enumInt(o obj, key string, allowed ...int) (int, error) {
	v, err := o.integer(key)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(allowed, v) {
		return 0, apiChanged(o.endpoint, fmt.Sprintf("unknown %s %d", key, v))
	}
	return v, nil
}

// listAssignments is GET /tun/m/assignments (upstream list_assignments).
func (t *tunnel) listAssignments(ctx context.Context, allowHTTP bool) ([]assignment, error) {
	const ep = "tun/m/assignments"
	o, ok, err := t.do(ctx, http.MethodGet, tunPrefix+"/assignments", ep, nil, nil, tunnelTimeout)
	if err != nil {
		return nil, t.unexpected(ep, err)
	}
	if !ok {
		return nil, apiChanged(ep, "empty answer")
	}
	raw, err := o.raw("assignments")
	if err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, apiChanged(ep, "field assignments is not a list of objects")
	}
	out := make([]assignment, 0, len(items))
	for _, m := range items {
		it := obj{endpoint: ep, m: m}
		var a assignment
		if a.Accelerator, err = t.acceleratorField(it, "accelerator"); err != nil {
			return nil, err
		}
		if a.Endpoint, err = t.endpointField(it); err != nil {
			return nil, err
		}
		if a.Variant, err = t.enumInt(it, "variant", 0, 1, 2); err != nil {
			return nil, err
		}
		if a.Shape, err = t.enumInt(it, "machineShape", 0, 1); err != nil {
			return nil, err
		}
		if a.Proxy, err = t.proxyInfo(it, allowHTTP); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// newNotebookHash is upstream's uuid_to_web_safe_base64(uuid.uuid4()): the
// UUID's text with "-" → "_", padded with "." to 44 characters.
func newNotebookHash() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	s := h[0:8] + "_" + h[8:12] + "_" + h[12:16] + "_" + h[16:20] + "_" + h[20:32]
	return s + strings.Repeat(".", 44-len(s))
}

// assignQuery is upstream _build_assign_url: nbh, variant=GPU,
// accelerator, shape=hm when asked.
func assignQuery(nbh, accelerator, shape string) url.Values {
	q := url.Values{"nbh": {nbh}, "variant": {"GPU"}, "accelerator": {accelerator}}
	if shape != "" {
		q.Set("shape", shape)
	}
	return q
}

// assign asks for a runtime (upstream Client.assign without its retry
// loop, which the caller runs with its own back-off): GET
// /tun/m/assign returns either the existing assignment of this notebook
// hash or an XSRF token; with the token, POST the same URL. Errors:
// *httpError 412 (refused: too many sessions or no capacity — the
// "queue"), 400 (no entitlement for the accelerator), transient ones;
// CodeAPIChanged for answers of an unexpected shape.
func (t *tunnel) assign(ctx context.Context, nbh, accelerator, shape string, allowHTTP bool) (assignment, error) {
	const ep = "tun/m/assign"
	a, xsrf, assigned, err := t.getAssign(ctx, nbh, accelerator, shape, allowHTTP)
	if err != nil || assigned {
		// assigned: this notebook hash already has a runtime (an earlier
		// POST went through although its answer was lost).
		return a, err
	}
	o, ok, err := t.do(ctx, http.MethodPost, tunPrefix+"/assign", ep, assignQuery(nbh, accelerator, shape),
		map[string]string{"X-Goog-Colab-Token": xsrf}, assignTimeout)
	if err != nil {
		return assignment{}, err
	}
	if !ok {
		return assignment{}, apiChanged(ep, "empty answer")
	}
	if a.Accelerator, err = t.acceleratorField(o, "accelerator"); err != nil {
		return assignment{}, err
	}
	if a.Endpoint, err = t.endpointField(o); err != nil {
		return assignment{}, err
	}
	if a.Variant, err = t.enumInt(o, "variant", 0, 1, 2); err != nil {
		return assignment{}, err
	}
	a.Shape = -1
	if a.Proxy, err = t.proxyInfo(o, allowHTTP); err != nil {
		return assignment{}, err
	}
	return a, nil
}

// unassign releases a runtime (upstream Client.unassign): GET
// /tun/m/unassign/{endpoint} for the XSRF token, then POST with it.
func (t *tunnel) unassign(ctx context.Context, endpoint string) error {
	const ep = "tun/m/unassign"
	path := tunPrefix + "/unassign/" + url.PathEscape(endpoint)
	o, ok, err := t.do(ctx, http.MethodGet, path, ep, nil, nil, tunnelTimeout)
	if err != nil {
		return err
	}
	if !ok {
		return apiChanged(ep, "empty answer")
	}
	xsrf, err := o.str("token")
	if err != nil {
		return err
	}
	t.secrets.add(xsrf)
	if _, _, err := t.do(ctx, http.MethodPost, path, ep, nil, map[string]string{"X-Goog-Colab-Token": xsrf}, tunnelTimeout); err != nil {
		return err
	}
	return nil
}

// unexpected turns errors of endpoints without special statuses into
// coded errors: transient ones CodeUnavailable, other HTTP statuses
// CodeAPIChanged.
func (t *tunnel) unexpected(ep string, err error) error {
	return codedHTTP(ep, err)
}

// codedHTTP converts an *httpError / netError into the package's coded
// errors (fallback): transient → CodeUnavailable, other → CodeAPIChanged.
func codedHTTP(ep string, err error) error {
	if err == nil {
		return nil
	}
	if Code(err) != "" || errors.Is(err, context.Canceled) {
		return err
	}
	if transient(err) {
		return fallbackErr(CodeUnavailable, map[string]any{"endpoint": ep}, err)
	}
	if s := statusOf(err); s != 0 {
		return apiChanged(ep, "HTTP "+strconv.Itoa(s))
	}
	return err
}
