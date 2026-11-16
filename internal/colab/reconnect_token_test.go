// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// A kernel channel that breaks after the runtime proxy token it was opened
// with has ended reconnects with a current token (review R65 M1): before,
// the reconnection reused the cell's first token, the runtime refused it
// and the lecture was handed back as if the runtime were gone.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/remote"
)

// wsRelay stands between the backend and the fake's kernel channels: it
// records the proxy token of every connection, refuses one token (401, as
// the real proxy is expected to refuse an expired one), and can break a
// cell's connection without the fake noticing: a reconnection with the same
// session id goes on with the same channel, as Jupyter's replay would.
type wsRelay struct {
	real Dialer
	done chan struct{}

	mu     sync.Mutex
	chans  map[string]*relayChan // by session id
	last   string                // the session id of the latest new channel
	tokens []string              // of every connection attempt, in order
	refuse string                // connections with this token get 401
}

type relayChan struct {
	under MessageConn
	msgs  chan relayMsg
	brk   chan struct{}
}

type relayMsg struct {
	bin  bool
	data []byte
	err  error
}

func newWSRelay(t *testing.T) *wsRelay {
	w := &wsRelay{real: DialWebSocket(nil), done: make(chan struct{}), chans: map[string]*relayChan{}}
	t.Cleanup(func() { close(w.done) })
	return w
}

func (w *wsRelay) dial(ctx context.Context, rawURL string, h http.Header) (MessageConn, error) {
	tok := h.Get("X-Colab-Runtime-Proxy-Token")
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	sid := u.Query().Get("session_id")
	w.mu.Lock()
	w.tokens = append(w.tokens, tok)
	refused := w.refuse != "" && tok == w.refuse
	ch := w.chans[sid]
	w.mu.Unlock()
	if refused {
		return nil, &httpError{endpoint: "api/kernels/channels", status: 401}
	}
	if ch != nil {
		return &relayConn{ch: ch}, nil // a reconnection: the same channel
	}
	under, err := w.real(ctx, rawURL, h)
	if err != nil {
		return nil, err
	}
	ch = &relayChan{under: under, msgs: make(chan relayMsg), brk: make(chan struct{}, 1)}
	go func() {
		for {
			bin, data, err := under.ReadMessage(context.Background())
			select {
			case ch.msgs <- relayMsg{bin, data, err}:
			case <-w.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	w.mu.Lock()
	w.chans[sid], w.last = ch, sid
	w.mu.Unlock()
	return &relayConn{ch: ch}, nil
}

// breakLast breaks the latest channel's connection once (its next read).
func (w *wsRelay) breakLast() {
	w.mu.Lock()
	ch := w.chans[w.last]
	w.mu.Unlock()
	ch.brk <- struct{}{}
}

func (w *wsRelay) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.tokens)
}

type relayConn struct {
	ch     *relayChan
	broken bool
}

func (c *relayConn) ReadMessage(ctx context.Context) (bool, []byte, error) {
	select {
	case <-c.ch.brk:
		c.broken = true
		return false, nil, errWSClosed
	case m := <-c.ch.msgs:
		return m.bin, m.data, m.err
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

func (c *relayConn) WriteText(ctx context.Context, b []byte) error {
	return c.ch.under.WriteText(ctx, b)
}

func (c *relayConn) Close() error {
	if c.broken {
		return nil // the channel goes on for the reconnection
	}
	return c.ch.under.Close()
}

// The lecture's cell was opened with the first proxy token; the clock
// passes the token's end, the account hands out a new token and the old one
// is refused; then the cell's connection breaks. The reconnection fetches
// and uses the new token and the lecture ends on Colab. (Red before: the
// old token was refused with 401 and the lecture handed back as
// session_reclaimed, the runtime released.)
func TestKernelReconnectionAfterTheTokensEndUsesANewToken(t *testing.T) {
	w := newWSRelay(t)
	e := newFakeEnv(t, t4(), func(o *Options) { o.Dial = w.dial })
	ctx := context.Background()
	e.f.hold("l1")
	done := startLecture(e, ctx, e.lecture(t, "l1"))
	e.transcribingNow(t)
	e.clk.Advance(61 * time.Minute) // the proxy token (3600 s) has ended
	newTok := "fake-proxy-token-2"
	e.f.proxyTok.Store(&newTok)
	w.mu.Lock()
	w.refuse = "fake-proxy-token"
	w.mu.Unlock()
	before := len(w.seen())
	w.breakLast()
	e.waitFor(t, "the reconnection", func() bool { return len(w.seen()) > before })
	e.f.release("l1")
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Transcribe after a reconnection past the token's end: %v (code %s)", err, Code(err))
	}
	if got := w.seen()[before]; got != newTok {
		t.Errorf("the reconnection used token %q, want the new %q", got, newTok)
	}
	if got := e.sleeps(); !slices.Equal(got, []time.Duration{5 * time.Second}) {
		t.Errorf("waits %v, want the 5 s before the reconnection", got)
	}
	e.b.removalsSettled()
	if st := e.b.Status(ctx); e.f.wasReleased() || st.State != StateRunning || st.LastError != nil {
		t.Errorf("released %v, state %s, last error %+v", e.f.wasReleased(), st.State, st.LastError)
	}
}

// execCell asks its source for the client of every connection: a
// transient failure of the token's refresh is one failed connection (the
// same waits, the same attempts); any other failure of it ends the cell at
// once — a runtime that is gone or a credential that is refused is never
// waited out.
func TestKernelChannelsTakeTheirTokenFromTheSource(t *testing.T) {
	clk := newFakeClock()
	newRC := func() *reconnector {
		return &reconnector{o: RetryOptions{Jitter: -1}.normalized(), now: clk.Now, sleep: clk.Sleep,
			notify: func(*remote.Reconnect) {}}
	}
	base, _ := url.Parse("http://runtime.invalid/rt")
	var tokens []string
	wrote := make(chan []byte, 1)
	var parent string
	dials := 0
	dial := func(_ context.Context, _ string, h http.Header) (MessageConn, error) {
		dials++
		tokens = append(tokens, h.Get("X-Colab-Runtime-Proxy-Token"))
		if dials == 1 { // the request goes out, then the connection breaks
			return &scriptConn{wrote: wrote}, nil
		}
		var m kernelMsg
		if parent == "" {
			select {
			case req := <-wrote:
				_ = json.Unmarshal(req, &m)
				parent, _ = m.Header["msg_id"].(string)
			case <-time.After(5 * time.Second):
				return nil, errors.New("test: no request was sent before the reconnection")
			}
		}
		return &scriptConn{msgs: [][]byte{ // the cell ends
			kernelReply(parent, "iopub", "status", map[string]any{"execution_state": "idle"}),
			kernelReply(parent, "shell", "execute_reply", map[string]any{"status": "ok"})}}, nil
	}
	client := func(tok string) *runtimeClient {
		return &runtimeClient{base: base, token: tok, agent: "t", dial: dial}
	}
	unavailable := fallbackErr(CodeUnavailable, map[string]any{"endpoint": "tun/m/assignments"}, &httpError{endpoint: "tun/m/assignments", status: 503})
	noOut := func(string) error { return nil }
	ctx := context.Background()

	// t1, then the refresh fails once (transient), then t2
	calls := 0
	src := func(context.Context) (*runtimeClient, error) {
		calls++
		switch calls {
		case 1:
			return client("t1"), nil
		case 2:
			return nil, unavailable
		}
		return client("t2"), nil
	}
	res, err := execCell(ctx, src, "k1", "pass", noOut, newRC(), nil)
	if err != nil || res.Status != "ok" {
		t.Fatalf("execCell: %+v, %v", res, err)
	}
	if want := []string{"t1", "t2"}; !slices.Equal(tokens, want) {
		t.Errorf("connections with tokens %v, want %v", tokens, want)
	}
	if want := []time.Duration{5 * time.Second, 15 * time.Second}; !slices.Equal(clk.sleeps, want) {
		t.Errorf("waits %v, want %v (the failed refresh used up an attempt)", clk.sleeps, want)
	}

	// decisions of the refresh: no further wait, the error as it came
	for _, decision := range []error{
		fallbackErr(CodeSessionReclaimed, nil, errors.New("the runtime is no longer among the account's assignments")),
		authErr(CodeAuthRejected, map[string]any{"reason": "http_401"}, "tun/m/assignments: HTTP 401"),
	} {
		clk.sleeps, tokens, dials, parent, calls = nil, nil, 0, "", 0
		src := func(context.Context) (*runtimeClient, error) {
			calls++
			if calls == 1 {
				return client("t1"), nil
			}
			return nil, decision
		}
		_, err := execCell(ctx, src, "k1", "pass", noOut, newRC(), nil)
		if Code(err) != Code(decision) {
			t.Errorf("err %v, want %s", err, Code(decision))
		}
		if want := []time.Duration{5 * time.Second}; !slices.Equal(clk.sleeps, want) || calls != 2 {
			t.Errorf("%s: waits %v with %d calls of the source, want %v with 2", Code(decision), clk.sleeps, calls, want)
		}
		select { // the request of this round, unless a redial took it
		case <-wrote:
		default:
		}
	}

	// the first connection: a transient refresh failure is retried as a
	// refused connection would be; a decision is not
	clk.sleeps, tokens, dials, parent, calls = nil, nil, 0, "", 0
	src = func(context.Context) (*runtimeClient, error) {
		calls++
		if calls == 1 {
			return nil, unavailable
		}
		return client("t1"), nil
	}
	if _, err := execCell(ctx, src, "k1", "pass", noOut, newRC(), nil); calls < 2 || len(clk.sleeps) == 0 || clk.sleeps[0] != 5*time.Second {
		t.Errorf("err %v after waits %v and %d calls, want a 5 s wait and a second call", err, clk.sleeps, calls)
	}
	select {
	case <-wrote:
	default:
	}
	clk.sleeps, calls = nil, 0
	src = func(context.Context) (*runtimeClient, error) {
		calls++
		return nil, fallbackErr(CodeSessionReclaimed, nil, nil)
	}
	if _, err := execCell(ctx, src, "k1", "pass", noOut, newRC(), nil); Code(err) != CodeSessionReclaimed || len(clk.sleeps) != 0 || calls != 1 {
		t.Errorf("err %v after waits %v and %d calls, want session_reclaimed at once", err, clk.sleeps, calls)
	}
}
