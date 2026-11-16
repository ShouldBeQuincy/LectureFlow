// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// SEC7 M1: the endpoints when a server's confirmed host key changed, over
// a real sshremote.Manager and an in-process SSH server that only does the
// handshake (it refuses every login, so nothing runs on it): connect shows
// the presented key with changed and previous_fingerprint and pins
// nothing; trust answers 409 remote_server.host_key_pinned unless replace
// and the right previous come with it; the mismatch error names the
// presented key (no "{fingerprint}" left in the text).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/remote"
	"lectureflow/internal/secrets"
	"lectureflow/internal/sshremote"
)

// handshakeServer is an SSH server on 127.0.0.1 that presents its host key
// and refuses every login.
type handshakeServer struct {
	ln   net.Listener
	mu   sync.Mutex
	host ssh.Signer
}

func newHandshakeServer(t *testing.T) *handshakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &handshakeServer{ln: ln}
	hs.newKey()
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				hs.mu.Lock()
				cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
					return nil, fmt.Errorf("refused")
				}}
				cfg.AddHostKey(hs.host)
				hs.mu.Unlock()
				_, _, _, _ = ssh.NewServerConn(c, cfg)
			}()
		}
	}()
	return hs
}

// newKey gives the server another host key (reinstalled, or an impostor);
// it returns the key's fingerprint.
func (hs *handshakeServer) newKey() string {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	hs.mu.Lock()
	hs.host = s
	hs.mu.Unlock()
	return ssh.FingerprintSHA256(s.PublicKey())
}

func (hs *handshakeServer) port() int { return hs.ln.Addr().(*net.TCPAddr).Port }

func TestRemoteHostKeyChangedEndpoints(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{7}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	withSSH(t, e)
	hs := newHandshakeServer(t)
	res, body := e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": "Lab box", "host": "127.0.0.1", "port": hs.port(), "username": "alice"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add: %d %s", res.StatusCode, body)
	}
	connect := func() (int, sshremote.ConnectResult, []byte) {
		t.Helper()
		res, body := e.do(http.MethodPost, "/api/remote/servers/lab-box/connect", nil)
		var r sshremote.ConnectResult
		_ = json.Unmarshal(body, &r)
		return res.StatusCode, r, body
	}
	trust := func(req map[string]any) (int, []byte) {
		t.Helper()
		res, body := e.do(http.MethodPost, "/api/remote/servers/lab-box/trust", req)
		return res.StatusCode, body
	}
	pinnedFP := func() string {
		t.Helper()
		_, body := e.do(http.MethodGet, "/api/remote/servers/lab-box", nil)
		var v sshremote.ServerView
		_ = json.Unmarshal(body, &v)
		return v.HostKeyFP
	}

	// First confirmation (unchanged): the presented key, then trust pins it;
	// the login is refused afterwards (auth_failed), the key stays pinned.
	code, r, body := connect()
	if code != http.StatusOK || r.HostKey == nil || r.Changed || r.PreviousFingerprint != "" {
		t.Fatalf("first connect: %d %s", code, body)
	}
	first := r.HostKey.Fingerprint
	if code, body := trust(map[string]any{"hop": 0, "fingerprint": first}); code != http.StatusBadGateway || !strings.Contains(string(body), `"remote.auth_failed"`) {
		t.Fatalf("first trust: %d %s", code, body)
	}
	if pinnedFP() != first {
		t.Fatalf("not pinned: %q", pinnedFP())
	}
	// trust again for a confirmed key: 409
	if code, body := trust(map[string]any{"hop": 0, "fingerprint": first}); code != http.StatusConflict || !strings.Contains(string(body), `"`+codeRemoteServerKeyPinned+`"`) {
		t.Fatalf("trust on a confirmed key: %d %s", code, body)
	}

	// The key changes: test names the presented key in the error.
	newFP := hs.newKey()
	res, body = e.do(http.MethodPost, "/api/remote/servers/lab-box/test", nil)
	var eb ErrorBody
	_ = json.Unmarshal(body, &eb)
	if res.StatusCode != http.StatusBadGateway || eb.Error.Code != "remote.host_key_mismatch" || eb.Error.Params["fingerprint"] != newFP || eb.Error.Params["device"] != "Lab box" {
		t.Fatalf("test after the change: %d %s", res.StatusCode, body)
	}
	// connect: 200, the presented key with changed and the previous fingerprint; nothing pinned
	code, r, body = connect()
	if code != http.StatusOK || !r.Changed || r.HostKey == nil || r.HostKey.Fingerprint != newFP || r.PreviousFingerprint != first || r.HostKeyHop != 0 {
		t.Fatalf("connect after the change: %d %s", code, body)
	}
	if r.Server.State != sshremote.StateKeyChanged || r.Server.LastError == nil || r.Server.LastError.Params["fingerprint"] != newFP {
		t.Fatalf("server after the change: %s", body)
	}
	if pinnedFP() != first {
		t.Fatal("connect pinned the presented key")
	}
	// trust without replace, with a wrong previous: 409, nothing replaced
	for _, req := range []map[string]any{
		{"hop": 0, "fingerprint": newFP},
		{"hop": 0, "fingerprint": newFP, "previous": first},
		{"hop": 0, "fingerprint": newFP, "replace": true},
		{"hop": 0, "fingerprint": newFP, "replace": true, "previous": newFP},
	} {
		if code, body := trust(req); code != http.StatusConflict || !strings.Contains(string(body), `"`+codeRemoteServerKeyPinned+`"`) {
			t.Errorf("trust %v: %d %s", req, code, body)
		}
		if pinnedFP() != first {
			t.Fatalf("trust %v replaced the key", req)
		}
	}

	// replace with the right previous: the new key is pinned (the login is
	// still refused), one warning without host or user name
	var logs bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	defer slog.SetDefault(prev)
	code, body = trust(map[string]any{"hop": 0, "fingerprint": newFP, "replace": true, "previous": first})
	if code != http.StatusBadGateway || !strings.Contains(string(body), `"remote.auth_failed"`) {
		t.Fatalf("replace: %d %s", code, body)
	}
	if pinnedFP() != newFP {
		t.Fatalf("not replaced: %q", pinnedFP())
	}
	mu.Lock()
	text := logs.String()
	mu.Unlock()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "replaced") || !strings.Contains(text, "device=ssh:lab-box") {
		t.Fatalf("no warning:\n%s", text)
	}
	for _, leak := range []string{"127.0.0.1", fmt.Sprint(hs.port()), "alice", first, newFP} {
		if strings.Contains(text, leak) {
			t.Errorf("%q in the log:\n%s", leak, text)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The trust body reaches the servers as it was sent.
type trustRecorder struct {
	remoteServers
	got sshremote.TrustRequest
}

func (r *trustRecorder) Trust(_ context.Context, _ string, req sshremote.TrustRequest) (sshremote.ConnectResult, error) {
	r.got = req
	return sshremote.ConnectResult{}, sshremote.ErrHostKeyPinned
}

func (r *trustRecorder) Device(string) (remote.Device, bool) { return nil, false }

func TestRemoteTrustBody(t *testing.T) {
	e := newEnv(t)
	rec := &trustRecorder{}
	e.sc.SetSSH(rec)
	res, body := e.do(http.MethodPost, "/api/remote/servers/box/trust", map[string]any{"hop": 1, "fingerprint": "SHA256:new", "replace": true, "previous": "SHA256:old"})
	if res.StatusCode != http.StatusConflict || !strings.Contains(string(body), codeRemoteServerKeyPinned) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if rec.got != (sshremote.TrustRequest{Hop: 1, Fingerprint: "SHA256:new", Replace: true, Previous: "SHA256:old"}) {
		t.Fatalf("trust got %+v", rec.got)
	}
}
