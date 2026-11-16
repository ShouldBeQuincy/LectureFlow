// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC14 N30-I4, the tests the review of the changed-host-key flow asked
// for: LectureFlow's key is never offered to a server whose host key is not
// the confirmed one (the handshake stops at the key exchange, before the
// user name and the public key go out), and a Trust that races with an
// edit of the address or with another Trust pins nothing.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"lectureflow/internal/remote"
)

// No public key is offered before the host key is confirmed, nor to a
// server that presents another key than the confirmed one: by Connect,
// Test, a lecture, or a Trust that is refused.
func TestNoAuthenticationWithoutTheConfirmedHostKey(t *testing.T) {
	ctx := context.Background()
	f := newFakeRemote(t, "Linux x86_64")
	e := newTestEnv(t, nil)
	host, port := f.addr()
	v, err := e.m.Add(ctx, AddRequest{Name: "Fresh", Host: host, Port: port, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.authorize(v.PublicKey)
	res, err := e.m.Connect(ctx, v.ID)
	if err != nil || res.HostKey == nil {
		t.Fatalf("first connect: %v %+v", err, res)
	}
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: "SHA256:wrong"}); err == nil {
		t.Fatal("a wrong fingerprint was accepted")
	}
	if n := f.authAttempts(); n != 0 {
		t.Fatalf("%d public keys offered before the host key was confirmed", n)
	}
	firstFP := res.HostKey.Fingerprint
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: firstFP}); err != nil {
		t.Fatal(err)
	}
	confirmed := f.authAttempts()
	if confirmed == 0 {
		t.Fatal("the fake server counts no authentication: this test would prove nothing")
	}

	e.m.server(v.ID).close()
	f.newHostKey()
	newFP := hostFP(f)
	if res, err = e.m.Connect(ctx, v.ID); err != nil || !res.Changed {
		t.Fatalf("connect after the key changed: %v %+v", err, res)
	}
	if _, err := e.m.Test(ctx, v.ID); PresentedFingerprint(err) != newFP {
		t.Fatalf("test: %v", err)
	}
	dev, _ := e.m.Device(v.DeviceID)
	var re *remote.Error
	if _, err := dev.Transcribe(ctx, testJob(t, "01JOBTEST0000000000000000N", remote.EngineFasterWhisper), nil); !errors.As(err, &re) || re.Code != remote.CodeHostKeyMismatch {
		t.Fatalf("a lecture on it: %v", err)
	}
	for name, req := range map[string]TrustRequest{
		"no replace":         {Fingerprint: newFP},
		"wrong previous":     {Fingerprint: newFP, Replace: true, Previous: newFP},
		"wrong fingerprint":  {Fingerprint: firstFP, Replace: true, Previous: firstFP},
		"a hop out of range": {Hop: 1, Fingerprint: newFP, Replace: true, Previous: firstFP},
	} {
		if _, err := e.m.Trust(ctx, v.ID, req); err == nil {
			t.Errorf("trust, %s: accepted", name)
		}
	}
	if n := f.authAttempts(); n != confirmed {
		t.Fatalf("%d public keys offered to a server with a changed host key", n-confirmed)
	}
	// replaced knowingly: only now is the key offered again
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: newFP, Replace: true, Previous: firstFP}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if f.authAttempts() == confirmed {
		t.Fatal("no authentication after the new key was confirmed")
	}
}

// hookDialer dials as usual; the next dial first runs the hook set with
// next (once): what happens "meanwhile", between Trust reading the server
// and Trust writing the key.
type hookDialer struct {
	mu   sync.Mutex
	hook func()
}

func (d *hookDialer) next(fn func()) {
	d.mu.Lock()
	d.hook = fn
	d.mu.Unlock()
}

func (d *hookDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	fn := d.hook
	d.hook = nil
	d.mu.Unlock()
	if fn != nil {
		fn()
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

// Trust and an edit of the address at the same time: the key read from the
// old address is not pinned to the new one (409, ErrHostKeyPinned), on the
// first confirmation and when replacing a changed key.
func TestTrustPinsNothingWhenTheAddressChangedMeanwhile(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		changed bool // the confirmed key changed first (Trust replaces)
		patch   func(other *fakeRemote) Patch
	}{
		{"first trust, user name edited", false, func(*fakeRemote) Patch { u := "bob"; return Patch{Username: &u} }},
		{"first trust, address edited", false, func(o *fakeRemote) Patch { h, p := o.addr(); return Patch{Host: &h, Port: &p} }},
		{"replace, address edited", true, func(o *fakeRemote) Patch { h, p := o.addr(); return Patch{Host: &h, Port: &p} }},
		{"first trust, jump host added", false, func(o *fakeRemote) Patch {
			h, p := o.addr()
			return Patch{Jumps: &[]Jump{{Host: h, Port: p, Username: "alice"}}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &hookDialer{}
			e := newTestEnv(t, func(o *Options) { o.Dial = d.dial })
			f, other := newFakeRemote(t, "Linux x86_64"), newFakeRemote(t, "Linux x86_64")
			var v ServerView
			req := TrustRequest{}
			if c.changed {
				var newFP string
				v, newFP = changedKey(t, e, f)
				req = TrustRequest{Fingerprint: newFP, Replace: true, Previous: v.HostKeyFP}
			} else {
				host, port := f.addr()
				var err error
				if v, err = e.m.Add(ctx, AddRequest{Name: "Racy", Host: host, Port: port, Username: "alice"}); err != nil {
					t.Fatal(err)
				}
				f.authorize(v.PublicKey)
				req = TrustRequest{Fingerprint: hostFP(f)}
			}
			var patchErr error
			d.next(func() { _, patchErr = e.m.Update(ctx, v.ID, c.patch(other)) })
			_, err := e.m.Trust(ctx, v.ID, req)
			if patchErr != nil {
				t.Fatalf("the edit meanwhile: %v", patchErr)
			}
			if !errors.Is(err, ErrHostKeyPinned) {
				t.Fatalf("trust while the address was edited: %v", err)
			}
			got, _ := e.m.Get(v.ID)
			if got.Trusted || got.HostKeyFP != "" || got.State != StateUntrusted {
				t.Fatalf("a key was pinned to the edited address: %+v", got)
			}
			for _, j := range got.Jumps {
				if j.HostKey != "" {
					t.Fatalf("a key was pinned to the added jump host: %+v", got.Jumps)
				}
			}
		})
	}
}

// Two Trusts at the same time: the second finds the hop confirmed
// meanwhile and pins nothing (409); the first one's key stays.
func TestTrustPinsNothingWhenTheHopWasConfirmedMeanwhile(t *testing.T) {
	ctx := context.Background()
	d := &hookDialer{}
	e := newTestEnv(t, func(o *Options) { o.Dial = d.dial })
	f := newFakeRemote(t, "Linux x86_64")
	host, port := f.addr()
	v, err := e.m.Add(ctx, AddRequest{Name: "Racy", Host: host, Port: port, Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.authorize(v.PublicKey)
	fp := hostFP(f)
	var innerErr error
	d.next(func() { _, innerErr = e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: fp}) })
	_, err = e.m.Trust(ctx, v.ID, TrustRequest{Fingerprint: fp})
	if innerErr != nil {
		t.Fatalf("the trust meanwhile: %v", innerErr)
	}
	if !errors.Is(err, ErrHostKeyPinned) {
		t.Fatalf("the second trust: %v", err)
	}
	if got, _ := e.m.Get(v.ID); !got.Trusted || got.HostKeyFP != fp {
		t.Fatalf("after two trusts: %+v", got)
	}
}
