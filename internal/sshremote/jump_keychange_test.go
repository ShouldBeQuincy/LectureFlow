// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC14 N30-L1 / N30-I4: a jump host whose confirmed key changed. Connect
// answers "changed" for that hop (the presented key next to the confirmed
// one's fingerprint, the state host_key_changed) whether the hops behind it
// are confirmed or not; nothing is pinned, nothing reaches the hops behind
// it, and Trust with replace and the previous fingerprint is the way on.

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/remote"
)

func hostFP(f *fakeRemote) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ssh.FingerprintSHA256(f.host.PublicKey())
}

// addBehindJumps adds a server reached through jumps (in order) and
// confirms the host keys of the first confirm hops, as the wizard does:
// Connect shows the next unconfirmed hop's key, Trust pins it.
func addBehindJumps(t *testing.T, e *testEnv, target *fakeRemote, jumps []*fakeRemote, confirm int) ServerView {
	t.Helper()
	ctx := context.Background()
	host, port := target.addr()
	req := AddRequest{Name: "Behind", Host: host, Port: port, Username: "alice"}
	for _, j := range jumps {
		j.setForward(true)
		h, p := j.addr()
		req.Jumps = append(req.Jumps, Jump{Host: h, Port: p, Username: "alice"})
	}
	v, err := e.m.Add(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	hops := append(append([]*fakeRemote(nil), jumps...), target)
	for _, f := range hops {
		f.authorize(v.PublicKey)
	}
	for hop := 0; hop < confirm; hop++ {
		res, err := e.m.Connect(ctx, v.ID)
		if err != nil {
			t.Fatalf("connect before hop %d is confirmed: %v", hop, err)
		}
		if res.HostKey == nil || res.Changed || res.HostKeyHop != hop || res.HostKey.Fingerprint != hostFP(hops[hop]) {
			t.Fatalf("connect before hop %d is confirmed: %+v (host key %+v)", hop, res, res.HostKey)
		}
		if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: hop, Fingerprint: res.HostKey.Fingerprint}); err != nil {
			t.Fatalf("trust hop %d: %v", hop, err)
		}
	}
	got, _ := e.m.Get(v.ID)
	return got
}

// wantChanged checks Connect's answer for a hop whose confirmed key changed.
func wantChanged(t *testing.T, e *testEnv, id string, res ConnectResult, err error, hop int, oldFP, newFP string) {
	t.Helper()
	if err != nil {
		t.Fatalf("connect with hop %d's key changed: %v", hop, err)
	}
	if !res.Changed || res.HostKey == nil || res.HostKeyHop != hop || res.HostKey.Fingerprint != newFP || res.PreviousFingerprint != oldFP {
		t.Fatalf("connect with hop %d's key changed: %+v (host key %+v)", hop, res, res.HostKey)
	}
	got, _ := e.m.Get(id)
	if got.State != StateKeyChanged || res.Server.State != StateKeyChanged {
		t.Fatalf("state %q / %q, want %q", got.State, res.Server.State, StateKeyChanged)
	}
	if got.LastError == nil || got.LastError.Code != remote.CodeHostKeyMismatch || got.LastError.Params["fingerprint"] != newFP {
		t.Fatalf("last error: %+v", got.LastError)
	}
	if hasSession(e.m.server(id)) {
		t.Fatal("a session was opened through a hop with a changed key")
	}
}

// N30-L1: the jump host's confirmed key changes while a hop behind it is
// not confirmed yet (the middle of the first setup, or after the target's
// address was edited). Before, Connect answered an error and "offline",
// with no way to replace the jump host's key.
func TestConnectShowsAChangedJumpKeyBeforeLaterHopsAreConfirmed(t *testing.T) {
	for _, c := range []struct {
		name  string
		jumps int // the first is confirmed and changes; the rest and the target are not confirmed
	}{
		{"the target is not confirmed", 1},
		{"a later jump host is not confirmed", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newTestEnv(t, nil)
			target := newFakeRemote(t, "Linux x86_64")
			var jumps []*fakeRemote
			for i := 0; i < c.jumps; i++ {
				jumps = append(jumps, newFakeRemote(t, "Linux x86_64"))
			}
			v := addBehindJumps(t, e, target, jumps, 1)
			oldFP := hostFP(jumps[0])
			pinned := v.Jumps[0].HostKey
			if keyFingerprint(pinned) != oldFP || v.Trusted {
				t.Fatalf("after confirming the first jump host: %+v", v)
			}
			jumps[0].newHostKey()
			newFP := hostFP(jumps[0])
			behind := append(append([]*fakeRemote(nil), jumps[1:]...), target)
			tries := make([]int, len(behind))
			for i, f := range behind {
				tries[i] = f.authAttempts()
			}
			jumpTries := jumps[0].authAttempts()

			res, err := e.m.Connect(ctx, v.ID)
			wantChanged(t, e, v.ID, res, err, 0, oldFP, newFP)
			got, _ := e.m.Get(v.ID)
			if got.Jumps[0].HostKey != pinned || got.Trusted {
				t.Fatalf("the presented key was pinned: %+v", got)
			}
			if jumps[0].authAttempts() != jumpTries {
				t.Fatal("LectureFlow's key was offered to a jump host with a changed key")
			}
			for i, f := range behind {
				if f.authAttempts() != tries[i] || len(f.sentCommands()) != 0 {
					t.Fatalf("hop %d behind the changed jump host was contacted", i+1)
				}
			}

			// the confirmed key stays without replace and the previous fingerprint
			if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: 0, Fingerprint: newFP}); !errors.Is(err, ErrHostKeyPinned) {
				t.Fatalf("trust without replace: %v", err)
			}
			// and the hop behind it cannot be confirmed past it
			if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: 1, Fingerprint: hostFP(behind[0])}); !errors.Is(err, errHostKeyMismatch) {
				t.Fatalf("trust of the hop behind the changed jump host: %v", err)
			}
			// replaced knowingly: the wizard goes on with the next unconfirmed hop
			res, err = e.m.Trust(ctx, v.ID, TrustRequest{Hop: 0, Fingerprint: newFP, Replace: true, Previous: oldFP})
			if err != nil {
				t.Fatalf("replace: %v", err)
			}
			if res.Changed || res.HostKey == nil || res.HostKeyHop != 1 || res.HostKey.Fingerprint != hostFP(behind[0]) {
				t.Fatalf("after replacing the jump host's key: %+v (host key %+v)", res, res.HostKey)
			}
			// back to "a hop is not confirmed yet", whichever hop is next
			got, _ = e.m.Get(v.ID)
			if res.Server.State != StateUntrusted || got.State != StateUntrusted {
				t.Fatalf("state %q / %q after replacing the jump host's key, want %q", res.Server.State, got.State, StateUntrusted)
			}
			if keyFingerprint(got.Jumps[0].HostKey) != newFP {
				t.Fatalf("the jump host's key was not replaced: %+v", got.Jumps)
			}
			// SEC39 L-B: the card no longer says "host key mismatch" about
			// the very key that was just confirmed
			if got.LastError != nil || res.Server.LastError != nil {
				t.Fatalf("a stale error after replacing the jump host's key: %+v", got.LastError)
			}
			for hop := 1; hop <= len(behind); hop++ {
				if res, err = e.m.Trust(ctx, v.ID, TrustRequest{Hop: hop, Fingerprint: hostFP(behind[hop-1])}); err != nil {
					t.Fatalf("trust hop %d: %v", hop, err)
				}
			}
			if !res.Server.Trusted || res.Server.State != StateSetup {
				t.Fatalf("after confirming every hop: %+v", res.Server)
			}
		})
	}
}

// SEC39 L-B: confirming a hop's key clears the card's last error only when
// that error is about a host key; any other error stays until the server
// is reached.
func TestTrustClearsOnlyHostKeyErrors(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		code  string
		stays bool
	}{
		{remote.CodeHostKeyMismatch, false},
		{remote.CodeHostKeyUnknown, false},
		{remote.CodeInstallFailed, true},
		{remote.CodeAuthFailed, true},
		{remote.CodeOffline, true},
	} {
		t.Run(c.code, func(t *testing.T) {
			e := newTestEnv(t, nil)
			target, jump := newFakeRemote(t, "Linux x86_64"), newFakeRemote(t, "Linux x86_64")
			v := addBehindJumps(t, e, target, []*fakeRemote{jump}, 0)
			s := e.m.server(v.ID)
			s.mu.Lock()
			s.lastErr = &remote.CardError{Code: c.code, Params: map[string]any{"device": v.Name}, At: time.Now().UTC()}
			s.mu.Unlock()
			// a refused Trust leaves the error alone
			if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: 0, Fingerprint: "SHA256:wrong"}); err == nil {
				t.Fatal("a wrong fingerprint was accepted")
			}
			if got, _ := e.m.Get(v.ID); got.LastError == nil || got.LastError.Code != c.code {
				t.Fatalf("after a refused trust: %+v", got.LastError)
			}
			// the jump host's key is confirmed; the target's is shown next
			res, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: 0, Fingerprint: hostFP(jump)})
			if err != nil || res.HostKey == nil || res.HostKeyHop != 1 {
				t.Fatalf("trust: %v %+v", err, res)
			}
			got, _ := e.m.Get(v.ID)
			if c.stays && (got.LastError == nil || got.LastError.Code != c.code) {
				t.Fatalf("an error that is not about a host key was cleared: %+v", got.LastError)
			}
			if !c.stays && got.LastError != nil {
				t.Fatalf("the host key error stays after the key was confirmed: %+v", got.LastError)
			}
		})
	}
}

// N30-I4: every hop is confirmed, then a hop's key changes. A changed jump
// host: its hop, and the target is not contacted. A changed target behind a
// jump host: the jump host is passed as usual, the target's hop is shown
// and LectureFlow's key is not offered to it.
func TestConnectShowsWhichHopChanged(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t, nil)
	target, jump := newFakeRemote(t, "Linux x86_64"), newFakeRemote(t, "Linux x86_64")
	v := addBehindJumps(t, e, target, []*fakeRemote{jump}, 2)
	if !v.Trusted || v.State != StateSetup {
		t.Fatalf("after confirming both hops: %+v", v)
	}
	e.m.server(v.ID).close()

	oldJump := hostFP(jump)
	jump.newHostKey()
	tries, commands := target.authAttempts(), len(target.sentCommands())
	res, err := e.m.Connect(ctx, v.ID)
	wantChanged(t, e, v.ID, res, err, 0, oldJump, hostFP(jump))
	if target.authAttempts() != tries || len(target.sentCommands()) != commands {
		t.Fatal("the target behind the changed jump host was contacted")
	}
	res, err = e.m.Trust(ctx, v.ID, TrustRequest{Hop: 0, Fingerprint: hostFP(jump), Replace: true, Previous: oldJump})
	if err != nil || res.Changed || res.Server.State != StateSetup {
		t.Fatalf("after replacing the jump host's key: %v %+v", err, res)
	}
	e.m.server(v.ID).close()

	oldTarget := hostFP(target)
	target.newHostKey()
	tries, commands = target.authAttempts(), len(target.sentCommands())
	jumpTries := jump.authAttempts()
	res, err = e.m.Connect(ctx, v.ID)
	wantChanged(t, e, v.ID, res, err, 1, oldTarget, hostFP(target))
	if jump.authAttempts() == jumpTries {
		t.Fatal("the jump host was not passed")
	}
	if target.authAttempts() != tries || len(target.sentCommands()) != commands {
		t.Fatal("LectureFlow's key or a command was sent to a target with a changed key")
	}
	got, _ := e.m.Get(v.ID)
	if got.HostKeyFP != oldTarget || keyFingerprint(got.Jumps[0].HostKey) != hostFP(jump) {
		t.Fatalf("the confirmed keys moved: %+v", got)
	}
}
