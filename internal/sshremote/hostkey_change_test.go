// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC7 M1 / L1: a server whose confirmed host key changed. Connect shows the
// presented key next to the confirmed one's fingerprint and pins nothing;
// Trust replaces a confirmed key only with replace and the previous
// fingerprint; the errors name the presented key; a key the user's own
// known_hosts marks @revoked is a mismatch.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/remote"
)

// changedKey sets up a confirmed server whose host key then changes; it
// returns the view before the change and the new key's fingerprint.
func changedKey(t *testing.T, e *testEnv, f *fakeRemote) (ServerView, string) {
	t.Helper()
	v := addTrusted(t, e, f)
	if v.HostKeyFP == "" {
		t.Fatalf("no confirmed fingerprint: %+v", v)
	}
	e.m.server(v.ID).close()
	f.newHostKey()
	f.mu.Lock()
	fp := ssh.FingerprintSHA256(f.host.PublicKey())
	f.mu.Unlock()
	if fp == v.HostKeyFP {
		t.Fatal("the new host key has the old fingerprint")
	}
	return v, fp
}

func hasSession(s *Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c != nil
}

// Connect after the key changed: the presented key, changed and the
// confirmed key's fingerprint; nothing pinned, no session, no command; the
// card's error names the presented key.
func TestConnectShowsAChangedHostKey(t *testing.T) {
	f := newFakeRemote(t, "Linux x86_64")
	e := newTestEnv(t, nil)
	v, newFP := changedKey(t, e, f)
	before := len(f.sentCommands())
	res, err := e.m.Connect(context.Background(), v.ID)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !res.Changed || res.HostKey == nil || res.HostKey.Fingerprint != newFP || res.PreviousFingerprint != v.HostKeyFP || res.HostKeyHop != 0 {
		t.Fatalf("connect after a changed key: %+v (host key %+v)", res, res.HostKey)
	}
	if res.HostKey.Algorithm != "ssh-ed25519" || !strings.Contains(res.HostKey.Check, "ssh_host_ed25519_key.pub") {
		t.Fatalf("presented key: %+v", res.HostKey)
	}
	got, _ := e.m.Get(v.ID)
	if got.HostKeyFP != v.HostKeyFP || !got.Trusted {
		t.Fatalf("the presented key was pinned: %+v", got)
	}
	if got.State != StateKeyChanged || res.Server.State != StateKeyChanged {
		t.Fatalf("state %q / %q", got.State, res.Server.State)
	}
	if got.LastError == nil || got.LastError.Code != remote.CodeHostKeyMismatch || got.LastError.Params["fingerprint"] != newFP {
		t.Fatalf("last error: %+v", got.LastError)
	}
	if hasSession(e.m.server(v.ID)) {
		t.Fatal("a session was opened to a server with a changed key")
	}
	if len(f.sentCommands()) != before {
		t.Fatal("commands ran on a server with a changed host key")
	}
	// The same through Test (the card's 测试连接) and a lecture.
	if _, err := e.m.Test(context.Background(), v.ID); PresentedFingerprint(err) != newFP {
		t.Fatalf("test: %v", err)
	}
	dev, _ := e.m.Device(v.DeviceID)
	_, err = dev.Transcribe(context.Background(), testJob(t, "01JOBTEST0000000000000000K", remote.EngineFasterWhisper), nil)
	var re *remote.Error
	if !errors.As(err, &re) || re.Code != remote.CodeHostKeyMismatch || re.Params["fingerprint"] != newFP || re.Params["device"] != v.Name {
		t.Fatalf("a lecture on it: %v %+v", err, re)
	}
}

// Trust on a confirmed hop: 409 (ErrHostKeyPinned) without replace, with a
// wrong or missing previous; the presented fingerprint must still match;
// with both it replaces the key and logs a warning without the host, the
// user name or the fingerprints.
func TestTrustReplacesAConfirmedKeyOnlyKnowingly(t *testing.T) {
	f := newFakeRemote(t, "Linux x86_64")
	e := newTestEnv(t, nil)
	v, newFP := changedKey(t, e, f)
	ctx := context.Background()
	unchanged := func(what string) {
		t.Helper()
		got, _ := e.m.Get(v.ID)
		if got.HostKeyFP != v.HostKeyFP {
			t.Fatalf("%s: the key was replaced: %+v", what, got)
		}
	}
	for _, c := range []struct {
		name string
		req  TrustRequest
	}{
		{"no replace", TrustRequest{Fingerprint: newFP}},
		{"no replace, previous", TrustRequest{Fingerprint: newFP, Previous: v.HostKeyFP}},
		{"replace without previous", TrustRequest{Fingerprint: newFP, Replace: true}},
		{"replace, wrong previous", TrustRequest{Fingerprint: newFP, Replace: true, Previous: newFP}},
		{"replace, made-up previous", TrustRequest{Fingerprint: newFP, Replace: true, Previous: "SHA256:wrong"}},
	} {
		if _, err := e.m.Trust(ctx, v.ID, c.req); !errors.Is(err, ErrHostKeyPinned) {
			t.Errorf("%s: %v", c.name, err)
		}
		unchanged(c.name)
	}
	// the right previous but another fingerprint than the presented one
	var fe *FieldError
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: v.HostKeyFP, Replace: true, Previous: v.HostKeyFP}); !errors.As(err, &fe) || fe.Field != "fingerprint" {
		t.Fatalf("old fingerprint as the new one: %v", err)
	}
	unchanged("wrong fingerprint")

	logs := captureSlog(t)
	// previous as typed or copied: another case for the prefix, padding, spaces
	prev := " sha256:" + strings.TrimPrefix(v.HostKeyFP, "SHA256:") + "= "
	res, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: newFP, Replace: true, Previous: prev})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	// reached again with the new key (the environment is not installed in
	// this test: setup)
	if res.Server.HostKeyFP != newFP || res.Changed || res.HostKey != nil || res.Server.State != StateSetup {
		t.Fatalf("after replacing: %+v", res)
	}
	text := logs.String()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "replaced") || !strings.Contains(text, "device="+v.DeviceID) {
		t.Fatalf("no warning about the replaced key:\n%s", text)
	}
	host, port := f.addr()
	for _, leak := range []string{host, fmt.Sprint(port), "alice", v.Name, v.HostKeyFP, newFP, strings.TrimPrefix(newFP, "SHA256:")} {
		if strings.Contains(text, leak) {
			t.Errorf("%q in the log:\n%s", leak, text)
		}
	}
	// now pinned: the same replace again needs the new key as previous
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: newFP, Replace: true, Previous: v.HostKeyFP}); !errors.Is(err, ErrHostKeyPinned) {
		t.Fatalf("replace with a stale previous: %v", err)
	}
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: newFP}); !errors.Is(err, ErrHostKeyPinned) {
		t.Fatalf("trust on a confirmed key: %v", err)
	}
}

// The first confirmation is unchanged: replace and previous are ignored
// while nothing is pinned.
func TestFirstTrustIgnoresReplace(t *testing.T) {
	f := newFakeRemote(t, "Linux x86_64")
	e := newTestEnv(t, nil)
	host, port := f.addr()
	v, err := e.m.Add(context.Background(), AddRequest{Name: "Fresh", Host: host, Port: port, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.authorize(v.PublicKey)
	res, err := e.m.Connect(context.Background(), v.ID)
	if err != nil || res.HostKey == nil || res.Changed || res.PreviousFingerprint != "" {
		t.Fatalf("first connect: %v %+v", err, res)
	}
	res, err = e.m.Trust(context.Background(), v.ID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: res.HostKey.Fingerprint, Replace: true, Previous: "SHA256:x"})
	if err != nil || !res.Server.Trusted {
		t.Fatalf("first trust: %v %+v", err, res.Server)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	for in, want := range map[string]string{
		"SHA256:abcDEF+/0":     "SHA256:abcDEF+/0",
		"  sha256:abcDEF+/0= ": "SHA256:abcDEF+/0",
		"Sha256: abcDEF+/0":    "SHA256:abcDEF+/0",
		"abcDEF+/0==":          "SHA256:abcDEF+/0",
		"SHA256:":              "",
		"":                     "",
		"   ":                  "",
	} {
		if got := NormalizeFingerprint(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if NormalizeFingerprint("SHA256:ABC") == NormalizeFingerprint("SHA256:abc") {
		t.Error("the base64 is case-sensitive")
	}
}

// PresentedFingerprint finds the presented key through the handshake's
// wrapping; other errors have none.
func TestPresentedFingerprint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(priv2)
	pinned := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	err := hostKeyCallback(pinned, nil)("h", nil, other.PublicKey())
	wrapped := fmt.Errorf("ssh: handshake failed: %w", err)
	if !errors.Is(wrapped, errHostKeyMismatch) || PresentedFingerprint(wrapped) != ssh.FingerprintSHA256(other.PublicKey()) {
		t.Fatalf("%v: %q", wrapped, PresentedFingerprint(wrapped))
	}
	if p := connErrParams("box", wrapped); p["fingerprint"] != ssh.FingerprintSHA256(other.PublicKey()) || p["device"] != "box" {
		t.Fatalf("params %v", p)
	}
	for _, e := range []error{nil, errHostKeyMismatch, errHostKeyUnknown, errors.New("x")} {
		if fp := PresentedFingerprint(e); fp != "" {
			t.Errorf("%v: %q", e, fp)
		}
		if p := connErrParams("box", e); len(p) != 1 {
			t.Errorf("%v: params %v", e, p)
		}
	}
	if keyFingerprint(pinned) != ssh.FingerprintSHA256(s.PublicKey()) || keyFingerprint("") != "" || keyFingerprint("garbage") != "" {
		t.Error("keyFingerprint")
	}
}

// L1: the user's own known_hosts — the same key is a match, another key
// for the host a mismatch, a key it marks @revoked a mismatch too (not
// "no entry", which the wizard trusts in one click), no entry "".
func TestKnownHostsVerdict(t *testing.T) {
	newKey := func() ssh.PublicKey {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		s, _ := ssh.NewSignerFromKey(priv)
		return s.PublicKey()
	}
	key, other, third := newKey(), newKey(), newKey()
	line := func(k ssh.PublicKey) string { return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) }
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	same := write("same", "gpu.example.com "+line(key)+"\n")
	another := write("another", "gpu.example.com "+line(other)+"\n")
	revoked := write("revoked", "@revoked * "+line(key)+"\n")
	revokedHost := write("revoked-host", "gpu.example.com "+line(key)+"\n@revoked gpu.example.com "+line(key)+"\n")
	elsewhere := write("elsewhere", "other.example.com "+line(other)+"\n")
	port := write("port", "[gpu.example.com]:2222 "+line(other)+"\n")
	for _, c := range []struct {
		file string
		port int
		want string
	}{
		{same, 22, "match"},
		{another, 22, "mismatch"},
		{revoked, 22, "mismatch"},
		{revokedHost, 22, "mismatch"},
		{elsewhere, 22, ""},
		{port, 2222, "mismatch"},
		{port, 22, ""},
		{filepath.Join(dir, "missing"), 22, ""},
		// N30-L2: a @cert-authority line names the key that signs host
		// certificates, not a host's key, so it says nothing about a plain
		// key: no entry. The host's own lines and @revoked count as before.
		{write("ca", "@cert-authority *.example.com "+line(other)+"\n"), 22, ""},
		{write("ca-tabs", "# lab\n\n  @cert-authority\t*.example.com\t"+line(other)+"\n"), 22, ""},
		{write("ca-port", "@cert-authority [gpu.example.com]:2222 "+line(other)+"\n"), 2222, ""},
		{write("ca-same", "@cert-authority *.example.com "+line(other)+"\ngpu.example.com "+line(key)+"\n"), 22, "match"},
		{write("ca-another", "@cert-authority *.example.com "+line(other)+"\n# x\ngpu.example.com "+line(third)+"\n"), 22, "mismatch"},
		{write("another-ca", "gpu.example.com "+line(third)+"\n@cert-authority * "+line(other)+"\n"), 22, "mismatch"},
		{write("ca-revoked", "@cert-authority * "+line(other)+"\n@revoked * "+line(key)+"\n"), 22, "mismatch"},
		{write("ca-elsewhere", "@cert-authority *.other.example "+line(other)+"\n"), 22, ""},
	} {
		if got := knownHostsVerdictIn(c.file, key, "gpu.example.com", c.port); got != c.want {
			t.Errorf("%s:%d: %q, want %q", filepath.Base(c.file), c.port, got, c.want)
		}
	}
}
