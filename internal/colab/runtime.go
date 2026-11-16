// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The Jupyter server of an assigned runtime, reached through the runtime
// proxy URL with the runtime proxy token (never the Google access token):
//   - Contents API (upstream colab_cli/contents.py ContentsClient): files;
//     the token goes in the colab-runtime-proxy-token query parameter.
//   - Kernels API (jupyter-kernel-client 0.9.0 manager.py fetch /
//     KernelHttpManager): start, list and delete kernels; the token goes in
//     Authorization and X-Colab-Runtime-Proxy-Token (upstream runtime.py
//     ColabRuntime.kernel_client).
//   - Kernel channels over a WebSocket (jupyter-kernel-client wsclient.py,
//     default Jupyter protocol: JSON text messages) to execute code.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	runtimeTimeout = 60 * time.Second
	// uploadChunk: bytes per Contents API PUT (JupyterLab's chunked
	// upload protocol: chunk 1, 2, …, -1 for the last).
	defaultChunkSize = 4 << 20
	// smallAnswerMax bounds the answer of the small requests (requestWith:
	// the kernel list and start, mkdir, delete, the Contents API's answer
	// to an upload's PUT, which is the file's model without its content).
	// Real answers are a few hundred bytes to a few KB; 4 MiB is far above
	// any of them and keeps a runtime from making each such request read
	// hundreds of megabytes (SEC41 L1). The result's download has its own
	// limit (maxResultAnswer), the resource readings theirs
	// (resourcesMaxBytes).
	smallAnswerMax = 4 << 20
)

// runtimeClient talks to one runtime's Jupyter server.
type runtimeClient struct {
	hc *http.Client
	// up carries the audio uploads (nil = hc): a connection of its own, so
	// that the megabytes queued for the uplink do not hold up the small
	// requests behind them (deletions, job.json, the result; colab-batch
	// real run: deletions stalled while an upload ran).
	up    *http.Client
	base  *url.URL
	token string
	agent string
	dial  Dialer
	// slow (may be nil) is told about a request that took slowRequest or
	// longer: the method and the endpoint name only (colab-batch
	// diagnostics: the runtime proxy sometimes stalls for tens of seconds).
	slow func(method, endpoint string, took time.Duration)
	// timeout of a small request (0 = runtimeTimeout; Options.RuntimeTimeout)
	timeout time.Duration
}

// to is the time limit of a small request (an audio chunk and a download
// get three times as long).
func (r *runtimeClient) to() time.Duration {
	if r.timeout > 0 {
		return r.timeout
	}
	return runtimeTimeout
}

// slowRequest: a runtime request this long is logged.
const slowRequest = 3 * time.Second

func (r *runtimeClient) url(path string, q url.Values) string {
	u := *r.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawPath = ""
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// request sends one request; status codes are the caller's business except
// that transport failures become netError.
func (r *runtimeClient) request(ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte, timeout time.Duration) (int, []byte, error) {
	return r.requestWith(r.hc, ctx, method, path, q, hdr, body, timeout)
}

// bulkBody: a request body this large is an audio chunk (the upload client).
const bulkBody = 1 << 20

// requestWith is request on hc; the answer is read up to smallAnswerMax
// (a longer one is errAnswerTooLarge). A request whose answer may be
// larger uses requestLimit with its own limit.
func (r *runtimeClient) requestWith(hc *http.Client, ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte, timeout time.Duration) (int, []byte, error) {
	return r.requestLimit(hc, ctx, method, path, q, hdr, body, timeout, smallAnswerMax)
}

// errAnswerTooLarge: an answer longer than requestLimit's limit (nothing of
// it is used).
var errAnswerTooLarge = errors.New("the answer is larger than allowed")

// requestLimit is requestWith reading at most limit bytes of the answer;
// a longer one is errAnswerTooLarge.
func (r *runtimeClient) requestLimit(hc *http.Client, ctx context.Context, method, path string, q url.Values, hdr http.Header, body []byte,
	timeout time.Duration, limit int64) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(runtimeRequestContext(ctx), timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.url(path, q), rd)
	if err != nil {
		return 0, nil, err
	}
	if r.slow != nil && len(body) < 64<<10 { // an audio chunk is as slow as the uplink
		start := time.Now()
		defer func() {
			if took := time.Since(start); took >= slowRequest {
				r.slow(method, endpointName(path), took)
			}
		}()
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := hc.Do(req)
	if err != nil {
		if c := context.Cause(ctx); errors.Is(c, context.Canceled) {
			return 0, nil, c
		}
		var rr *redirectRefused
		if errors.As(err, &rr) {
			// not transient: the same request is redirected the same way
			return 0, nil, apiChanged(endpointName(path), rr.Error())
		}
		return 0, nil, netError{text: fmt.Sprintf("%s %s: %s", method, endpointName(path), netErrText(err))}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, netError{text: fmt.Sprintf("%s %s: reading the answer: %s", method, endpointName(path), netErrText(err))}
	}
	if int64(len(b)) > limit {
		// the default limit too: a truncated answer is never passed on
		// (security audit AUD4 L-1)
		return resp.StatusCode, nil, fmt.Errorf("%s %s: %w (%d bytes)", method, endpointName(path), errAnswerTooLarge, limit)
	}
	return resp.StatusCode, b, nil
}

// endpointName is a path for messages: api/contents or api/kernels, never
// file names or ids.
func endpointName(path string) string {
	p := strings.TrimLeft(path, "/")
	for _, pre := range []string{"api/contents", "api/kernels"} {
		if strings.HasPrefix(p, pre) {
			return pre
		}
	}
	return "runtime"
}

func runtimeStatusErr(ep string, status int) error {
	switch {
	case status == 401 || status == 403:
		return &httpError{endpoint: ep, status: status}
	case status == 404 || status == 410:
		return &httpError{endpoint: ep, status: status}
	case status == 429 || status >= 500:
		return &httpError{endpoint: ep, status: status}
	}
	return apiChanged(ep, "HTTP "+strconv.Itoa(status))
}

// ---- kernels ----

func (r *runtimeClient) kernelHeaders() http.Header {
	return http.Header{
		"Accept":                      {"application/json"},
		"Content-Type":                {"application/json"},
		"Authorization":               {"Bearer " + r.token},
		"X-Colab-Client-Agent":        {r.agent},
		"X-Colab-Runtime-Proxy-Token": {r.token},
	}
}

// listKernels is GET /api/kernels: the readiness probe while booting.
func (r *runtimeClient) listKernels(ctx context.Context) ([]string, error) {
	const ep = "api/kernels"
	st, b, err := r.request(ctx, http.MethodGet, "/api/kernels", nil, r.kernelHeaders(), nil, r.to())
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, runtimeStatusErr(ep, st)
	}
	var list []map[string]json.RawMessage
	if json.Unmarshal(b, &list) != nil {
		return nil, apiChanged(ep, "answer is not a list of kernels")
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		id, err := (obj{endpoint: ep, m: m}).str("id")
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// startKernel is POST /api/kernels {"name":"python3","path":null}
// (jupyter-kernel-client KernelClient.start default name).
func (r *runtimeClient) startKernel(ctx context.Context) (string, error) {
	const ep = "api/kernels"
	st, b, err := r.request(ctx, http.MethodPost, "/api/kernels", nil, r.kernelHeaders(), []byte(`{"name":"python3","path":null}`), r.to())
	if err != nil {
		return "", err
	}
	if st != http.StatusOK && st != http.StatusCreated {
		return "", runtimeStatusErr(ep, st)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return "", apiChanged(ep, "answer is not a kernel model")
	}
	id, err := (obj{endpoint: ep, m: m}).str("id")
	if err != nil {
		return "", err
	}
	if id == "" || strings.ContainsAny(id, "/?#") {
		return "", apiChanged(ep, "bad kernel id")
	}
	return id, nil
}

// deleteKernel is DELETE /api/kernels/{id}; 404/410 count as done.
func (r *runtimeClient) deleteKernel(ctx context.Context, id string) error {
	st, _, err := r.request(ctx, http.MethodDelete, "/api/kernels/"+url.PathEscape(id), nil, r.kernelHeaders(), nil, r.to())
	if err != nil {
		return err
	}
	if st == http.StatusNoContent || st == http.StatusOK || st == 404 || st == 410 {
		return nil
	}
	return runtimeStatusErr("api/kernels", st)
}

// ---- contents ----

func (r *runtimeClient) contentsQuery(extra url.Values) url.Values {
	q := url.Values{"authuser": {"0"}, "colab-runtime-proxy-token": {r.token}}
	for k, v := range extra {
		q[k] = v
	}
	return q
}

func contentsPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/api/contents/" + strings.Join(parts, "/")
}

var jsonHeader = http.Header{"Content-Type": {"application/json"}}

// mkdir creates a directory (PUT {"type":"directory"}); an existing one is
// fine.
func (r *runtimeClient) mkdir(ctx context.Context, path string) error {
	const ep = "api/contents"
	body, _ := json.Marshal(map[string]any{"type": "directory"})
	st, _, err := r.request(ctx, http.MethodPut, contentsPath(path), r.contentsQuery(nil), jsonHeader, body, r.to())
	if err != nil {
		return err
	}
	if st == http.StatusOK || st == http.StatusCreated {
		return nil
	}
	return runtimeStatusErr(ep, st)
}

// upload writes data to path in chunks (upstream ContentsClient.upload
// sends the whole file as base64 with "chunk": 1; files larger than one
// chunk continue with chunk 2, 3, … and -1 for the last, JupyterLab's
// protocol). progress gets the bytes sent so far.
func (r *runtimeClient) upload(ctx context.Context, path string, data []byte, chunkSize int, progress func(done, total int64)) error {
	const ep = "api/contents"
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	name := path[strings.LastIndex(path, "/")+1:]
	total := int64(len(data))
	n := max(1, (len(data)+chunkSize-1)/chunkSize)
	for i := 0; i < n; i++ {
		part := data[i*chunkSize : min(len(data), (i+1)*chunkSize)]
		chunk := i + 1
		if n > 1 && i == n-1 {
			chunk = -1
		}
		body, _ := json.Marshal(map[string]any{
			"name": name, "path": path, "type": "file", "format": "base64",
			"content": base64.StdEncoding.EncodeToString(part), "chunk": chunk,
		})
		hc := r.hc
		if r.up != nil && len(body) >= bulkBody {
			hc = r.up
		}
		st, _, err := r.requestWith(hc, ctx, http.MethodPut, contentsPath(path), r.contentsQuery(nil), jsonHeader, body, 3*r.to())
		if err != nil {
			return err
		}
		if st != http.StatusOK && st != http.StatusCreated {
			return runtimeStatusErr(ep, st)
		}
		if progress != nil {
			progress(min(total, int64((i+1)*chunkSize)), total)
		}
	}
	return nil
}

// download reads a file (GET ?content=1, as upstream ContentsClient.download;
// format=base64&type=file asks for the exact bytes, a text answer is
// accepted too). It is only used for a result.json: an answer longer than
// maxResultAnswer is errAnswerTooLarge.
func (r *runtimeClient) download(ctx context.Context, path string) ([]byte, error) {
	const ep = "api/contents"
	q := r.contentsQuery(url.Values{"content": {"1"}, "format": {"base64"}, "type": {"file"}})
	st, b, err := r.requestLimit(r.hc, ctx, http.MethodGet, contentsPath(path), q, nil, nil, 3*r.to(), maxResultAnswer)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, runtimeStatusErr(ep, st)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		return nil, apiChanged(ep, "answer is not a contents model")
	}
	o := obj{endpoint: ep, m: m}
	typ, err := o.str("type")
	if err != nil {
		return nil, err
	}
	if typ != "file" {
		return nil, apiChanged(ep, "not a file")
	}
	format, err := o.str("format")
	if err != nil {
		return nil, err
	}
	content, err := o.str("content")
	if err != nil {
		return nil, err
	}
	switch format {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
		if err != nil {
			return nil, apiChanged(ep, "content is not base64")
		}
		return data, nil
	case "text":
		return []byte(content), nil
	}
	return nil, apiChanged(ep, "unknown format "+safeToken(format))
}

// remove deletes a file or an empty directory; a missing one is fine.
func (r *runtimeClient) remove(ctx context.Context, path string) error {
	st, _, err := r.request(ctx, http.MethodDelete, contentsPath(path), r.contentsQuery(nil), nil, nil, r.to())
	if err != nil {
		return err
	}
	if st == http.StatusNoContent || st == http.StatusOK || st == 404 {
		return nil
	}
	return runtimeStatusErr("api/contents", st)
}

// ---- kernel channels ----

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

// kernelMsg is a Jupyter message in the default WebSocket protocol.
type kernelMsg struct {
	Header       map[string]any  `json:"header"`
	ParentHeader map[string]any  `json:"parent_header"`
	Metadata     map[string]any  `json:"metadata"`
	Content      json.RawMessage `json:"content"`
	Channel      string          `json:"channel"`
	Buffers      []any           `json:"buffers"`
}

func (m kernelMsg) msgType() string {
	s, _ := m.Header["msg_type"].(string)
	return s
}

func (m kernelMsg) parentID() string {
	s, _ := m.ParentHeader["msg_id"].(string)
	return s
}

// decodeKernelMsg reads a text message (JSON) or a binary one (default
// protocol with buffers: big-endian count and offsets, the JSON first).
func decodeKernelMsg(binaryMsg bool, data []byte) (kernelMsg, error) {
	var m kernelMsg
	if binaryMsg {
		if len(data) < 12 {
			return m, errors.New("short binary message")
		}
		n := int(binary.BigEndian.Uint32(data[:4]))
		if n < 2 || len(data) < 4*(n+1) {
			return m, errors.New("bad binary message")
		}
		start, stop := int(binary.BigEndian.Uint32(data[4:8])), int(binary.BigEndian.Uint32(data[8:12]))
		if start < 0 || start > stop || stop > len(data) {
			return m, errors.New("bad binary offsets")
		}
		data = data[start:stop]
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// execOutcome is the end of one execute_request.
type execOutcome struct {
	Status string // execute_reply status: ok, error, aborted
	EName  string // the error's exception name (identifier only), "" when none
	Stderr int    // bytes of stderr the kernel streamed (discarded unread)
}

// kernelExec runs code in kernel kid and passes every stdout stream chunk
// to onStdout. started (may be nil) is called once the request has been
// sent (colab-batch R1: the audio uploads while the cell runs). It returns
// when the execute_reply and the idle status for the request have arrived.
// A connection that breaks is reopened with the same session id (the
// Jupyter server replays what it buffered for the session) after the waits
// of rc (retry.go: 5 s, 15 s, 30 s, within 60 s of the break — the same
// parameters as a failed request; it was 2 s, 4 s, 8 s before v3.2.x); a
// message of the request arriving again ends that sequence, so a later
// break gets its own. The first connection is retried the same way after a
// transient failure (nothing was sent yet). Outputs other than stdout and
// stderr streams (display data, execute results) are CodeContractViolation:
// the cell LectureFlow runs prints only through the worker's stdout.
//
// Every connection uses r's proxy token; execCell takes a new one for each.
func (r *runtimeClient) kernelExec(ctx context.Context, kid, code string, onStdout func(string) error, rc *reconnector, started func()) (execOutcome, error) {
	return execCell(ctx, func(context.Context) (*runtimeClient, error) { return r, nil }, kid, code, onStdout, rc, started)
}

// clientSource returns the runtime client to connect with, its proxy token
// valid now (Backend.clientOf: Backend.runtime of the session). Its error
// is fetching that token's.
type clientSource func(ctx context.Context) (*runtimeClient, error)

// execCell is kernelExec with the client of every connection from src, so
// a connection that breaks after the token it was opened with has ended
// reconnects with a current one (review R65 M1: a lecture runs longer than
// a token lasts, and the runtime refuses an ended token as if it were
// gone). A transient failure of the token's refresh (retryable) counts as
// one failed connection: the same waits, attempts and window. Any other
// failure of it (the runtime no longer assigned, the credential refused or
// gone) ends the cell at once, as a refused connection does.
func execCell(ctx context.Context, src clientSource, kid, code string, onStdout func(string) error, rc *reconnector, started func()) (execOutcome, error) {
	session := newUUID()
	// refresh: the error is src's, not the connection's
	dialOnce := func() (conn MessageConn, refresh bool, err error) {
		r, err := src(ctx)
		if err != nil {
			return nil, true, err
		}
		q := url.Values{"session_id": {session}, "token": {r.token}}
		u := r.url("/api/kernels/"+url.PathEscape(kid)+"/channels", q)
		u = "ws" + strings.TrimPrefix(u, "http")
		conn, err = r.dial(ctx, u, http.Header{
			"X-Colab-Client-Agent":        {r.agent},
			"X-Colab-Runtime-Proxy-Token": {r.token},
		})
		return conn, false, err
	}
	conn, refresh, err := dialOnce()
	for err != nil {
		if ctx.Err() != nil {
			return execOutcome{}, ctx.Err()
		}
		if !retryable(err, refresh) || !rc.next(ctx) {
			rc.reset()
			if ctx.Err() != nil {
				return execOutcome{}, ctx.Err()
			}
			return execOutcome{}, err
		}
		conn, refresh, err = dialOnce()
	}
	rc.reset()
	defer func() { conn.Close() }()
	msgID := newUUID()
	content, _ := json.Marshal(map[string]any{
		"code": code, "silent": false, "store_history": false, "user_expressions": map[string]any{},
		"allow_stdin": false, "stop_on_error": true,
	})
	req, _ := json.Marshal(kernelMsg{
		Header: map[string]any{
			"msg_id": msgID, "username": "lectureflow", "session": session, "msg_type": "execute_request",
			"version": "5.3", "date": time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		},
		ParentHeader: map[string]any{}, Metadata: map[string]any{}, Content: content, Channel: "shell", Buffers: []any{},
	})
	if err := conn.WriteText(ctx, req); err != nil {
		if ctx.Err() != nil {
			return execOutcome{}, ctx.Err()
		}
		return execOutcome{}, netError{text: "kernel channels: sending the request: " + netErrText(err)}
	}
	if started != nil {
		started()
	}
	var out execOutcome
	gotReply, gotIdle := false, false
	defer rc.reset()
	for !(gotReply && gotIdle) {
		bin, data, err := conn.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			conn.Close()
			conn = closedConn{}
			// The wait before the next connection; none left (or the window
			// used up): the connection is lost.
			if !rc.next(ctx) {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				return out, fallbackErr(CodeConnectionLost, nil, fmt.Errorf("kernel channels: %s", netErrText(err)))
			}
			if c2, refresh, derr := dialOnce(); derr == nil {
				conn = c2
			} else if ctx.Err() != nil {
				return out, ctx.Err()
			} else if !retryable(derr, refresh) {
				// The runtime is gone or refuses us (401, 403, 404, 410, ...),
				// or so does the token's refresh: as for the first connection
				// and every other request, no waiting it out (SEC2 L2);
				// failedErr maps the runtime's 401/403/404/410 to
				// session_reclaimed.
				return out, derr
			}
			// a failed connection: the next read fails at once and the
			// loop waits again (or gives up)
			continue
		}
		m, err := decodeKernelMsg(bin, data)
		if err != nil {
			return out, apiChanged("api/kernels/channels", "message is not a Jupyter message")
		}
		if m.parentID() != msgID {
			continue
		}
		rc.reset() // the request's messages arrive (again): the break is over
		var c map[string]json.RawMessage
		_ = json.Unmarshal(m.Content, &c)
		co := obj{endpoint: "api/kernels/channels", m: c}
		switch m.Channel {
		case "iopub":
			switch m.msgType() {
			case "status":
				if st, _ := co.str("execution_state"); st == "idle" {
					gotIdle = true
				}
			case "stream":
				name, err1 := co.str("name")
				text, err2 := co.str("text")
				if err1 != nil || err2 != nil {
					return out, apiChanged("api/kernels/channels", "stream message without name or text")
				}
				if name == "stdout" {
					if err := onStdout(text); err != nil {
						return out, err
					}
				} else {
					out.Stderr += len(text)
				}
			case "error":
				if en, _ := co.str("ename"); excNameRe.MatchString(en) {
					out.EName = en
				} else {
					out.EName = "?"
				}
			case "display_data", "execute_result", "update_display_data":
				return out, violation("rich output from the cell")
			}
		case "shell":
			if m.msgType() == "execute_reply" {
				st, err := co.str("status")
				if err != nil {
					return out, err
				}
				out.Status = st
				gotReply = true
			}
		}
	}
	return out, nil
}

// closedConn stands in after a failed redial: its next read fails at once,
// so the loop tries again (or gives up).
type closedConn struct{}

func (closedConn) ReadMessage(context.Context) (bool, []byte, error) { return false, nil, errWSClosed }
func (closedConn) WriteText(context.Context, []byte) error           { return errWSClosed }
func (closedConn) Close() error                                      { return nil }
