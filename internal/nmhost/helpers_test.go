// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

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
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// Test values that must never reach a log, a file or stderr.
const (
	secretCookie = "SECRETCOOKIEVALUE-7f3a9c"
	secretQuery  = "sig=SECRETSIGNATURE-d41e"
)

// fakeHandler is a scriptable bridge.Handler.
type fakeHandler struct {
	mu      sync.Mutex
	version string
	// block makes Check wait until the channel is closed when the request
	// names the session "slow".
	block chan struct{}
	// pushes go to every subscriber.
	subs     []chan bridge.Status
	subCount int
	initial  []bridge.Status
	enqueued []*bridge.EnqueueRequest
	panicOn  string
}

func newFakeHandler() *fakeHandler { return &fakeHandler{version: "9.9.9", block: make(chan struct{})} }

func (f *fakeHandler) Hello(ctx context.Context, req *bridge.HelloRequest) (*bridge.HelloReply, error) {
	return &bridge.HelloReply{App: bridge.AppInfo{Version: f.version, Mode: "desktop"}, Courses: []bridge.Course{{ID: "01HCOURSE", Name: "CSC1003"}}}, nil
}

func (f *fakeHandler) Check(ctx context.Context, req *bridge.CheckRequest) (*bridge.CheckReply, error) {
	if f.panicOn == bridge.TypeCheck {
		panic("boom " + secretCookie)
	}
	for _, s := range req.SessionIDs {
		if s == "slow" {
			select {
			case <-f.block:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	items := []bridge.CheckItem{}
	for _, s := range req.SessionIDs {
		items = append(items, bridge.CheckItem{SessionID: s, State: bridge.StateQueued, TaskID: 1})
	}
	return &bridge.CheckReply{Items: items}, nil
}

func (f *fakeHandler) Enqueue(ctx context.Context, req *bridge.EnqueueRequest) (*bridge.EnqueueReply, error) {
	f.mu.Lock()
	f.enqueued = append(f.enqueued, req)
	f.mu.Unlock()
	switch req.SessionID {
	case "coded":
		return nil, bridge.NewError(bridge.CodeCourseGone, map[string]any{"courseId": req.CourseID})
	case "fail":
		// A careless error that quotes the signed URL: the log must still
		// not show its query.
		return nil, fmt.Errorf("download %s refused", req.Tracks[0].URL)
	}
	return &bridge.EnqueueReply{TaskID: 42, Position: 1}, nil
}

func (f *fakeHandler) Refresh(ctx context.Context, req *bridge.RefreshRequest) (*bridge.RefreshReply, error) {
	return &bridge.RefreshReply{TaskID: 42, State: bridge.StateQueued}, nil
}

func (f *fakeHandler) Open(ctx context.Context, req *bridge.OpenRequest) (*bridge.OpenReply, error) {
	if req.LectureID == "huge" {
		return nil, errors.New("x")
	}
	return &bridge.OpenReply{}, nil
}

func (f *fakeHandler) Subscribe(ctx context.Context) <-chan bridge.Status {
	ch := make(chan bridge.Status, 1024)
	f.mu.Lock()
	f.subCount++
	for _, st := range f.initial {
		ch <- st
	}
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, c := range f.subs {
			if c == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(ch)
	}()
	return ch
}

func (f *fakeHandler) push(st bridge.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs {
		c <- st
	}
}

func (f *fakeHandler) subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subCount
}

func (f *fakeHandler) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// logBuffer is a concurrency-safe log sink.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestLogger() (*slog.Logger, *logBuffer) {
	buf := &logBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// client speaks frames over a connection.
type client struct {
	t  *testing.T
	nc net.Conn
}

func (c *client) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.sendRaw(b)
}

func (c *client) sendRaw(b []byte) {
	c.t.Helper()
	c.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := bridge.WriteFrame(c.nc, b, bridge.MaxRequest); err != nil {
		c.t.Fatal(err)
	}
}

// recv reads one frame as a generic map.
func (c *client) recv() map[string]any {
	c.t.Helper()
	m, err := c.tryRecv(5 * time.Second)
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *client) tryRecv(d time.Duration) (map[string]any, error) {
	c.nc.SetReadDeadline(time.Now().Add(d))
	b, err := bridge.ReadFrame(c.nc, bridge.MaxToBrowser)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func hello(id string) bridge.HelloRequest {
	return bridge.HelloRequest{Envelope: bridge.Envelope{Type: bridge.TypeHello, ID: id}, Protocol: bridge.ProtocolVersion,
		Extension: bridge.ExtensionInfo{Version: "0.1.0", Browser: "chrome", Locale: "zh-CN"}}
}

func enqueueReq(id, session string) bridge.EnqueueRequest {
	return bridge.EnqueueRequest{
		Envelope:  bridge.Envelope{Type: bridge.TypeEnqueue, ID: id},
		Profile:   bridge.Profile{ID: "panopto", Version: "1.0.0", Reviewed: true},
		Platform:  "panopto",
		SessionID: session,
		Lecture:   bridge.Lecture{Title: "Week 1", RecordedAt: "2026-10-01"},
		CourseID:  "01HCOURSE",
		Tracks:    []bridge.Track{{URL: "https://media.example.edu/v/1/audio.mp4?" + secretQuery, Kind: bridge.KindAudio, Format: bridge.FormatFile, SizeBytes: -1}},
		Referer:   "https://example.edu/Panopto/Pages/Viewer.aspx?id=1&" + secretQuery,
		Cookies:   []bridge.Cookie{{Name: "session", Value: secretCookie, Domain: "media.example.edu", Path: "/"}},
	}
}

// uniquePipe is a named pipe name no other test or app uses.
func uniquePipe(t *testing.T) string {
	var b [8]byte
	rand.Read(b[:])
	return pipePrefix + "lectureflow-test-" + hex.EncodeToString(b[:])
}

// uniqueSocket is a socket path in a private directory. A Unix socket path
// holds at most 104 bytes on macOS and 108 on Linux and Windows (sun_path,
// counting the terminating NUL). The temporary directory plus a long test
// name can go past that (macOS: /var/folders/…/T/, also on the CI runners;
// Windows: a long user name or TEMP pointed somewhere deep), so such a path
// moves to a short private directory: /tmp, or on Windows the public
// profile or the root of the temporary directory's drive. Every call gets
// its own directory, so parallel tests never share a socket.
func uniqueSocket(t *testing.T) string {
	t.Helper()
	const maxLen = 100
	p := filepath.Join(t.TempDir(), "nm.sock")
	if len(p) < maxLen {
		return p
	}
	var bases []string
	if runtime.GOOS == "windows" {
		if pub := os.Getenv("PUBLIC"); pub != "" {
			bases = append(bases, pub)
		}
		if vol := filepath.VolumeName(os.TempDir()); vol != "" {
			bases = append(bases, vol+`\`)
		}
	} else {
		bases = append(bases, "/tmp")
	}
	var errs []error
	for _, base := range bases {
		d, err := os.MkdirTemp(base, "lfnm-")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		if p := filepath.Join(d, "nm.sock"); len(p) < maxLen {
			return p
		}
		errs = append(errs, fmt.Errorf("%s: path still too long", d))
	}
	t.Fatalf("no directory short enough for a Unix socket (temporary directory %s): %v", os.TempDir(), errors.Join(errs...))
	return ""
}

// serveFake serves a fake handler on endpoint and returns the server.
func serveFake(t *testing.T, endpoint string, h bridge.Handler, log *slog.Logger) *Server {
	t.Helper()
	ln, err := Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerOptions{Handler: h, Log: log})
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return s
}

// assertNoSecrets fails when text holds a test cookie or signature.
func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{secretCookie, "SECRETSIGNATURE"} {
		if bytes.Contains([]byte(text), []byte(s)) {
			t.Errorf("%s contains %q:\n%s", what, s, text)
		}
	}
}

var _ io.Writer = (*logBuffer)(nil)
