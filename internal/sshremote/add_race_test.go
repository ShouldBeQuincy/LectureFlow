// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC7 L6: two Adds at the same time that end at the same id. The id was
// checked before the key and the row were written and outside the lock, so
// both passed; the second one's key replaced the first one's, and the one
// whose row lost then emptied the key row of the one that won: a server
// without its private key.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
)

// gateStore holds one SetKV of a key (a non-empty value) until released.
type gateStore struct {
	Store
	mu      sync.Mutex
	hold    bool
	entered chan struct{}
	release chan struct{}
}

func (g *gateStore) SetKV(ctx context.Context, key, value string) error {
	g.mu.Lock()
	hold := g.hold && value != ""
	if hold {
		g.hold = false
	}
	g.mu.Unlock()
	if hold {
		close(g.entered)
		select {
		case <-g.release:
		case <-time.After(20 * time.Second):
			return errors.New("gateStore: never released")
		}
	}
	return g.Store.SetKV(ctx, key, value)
}

type addResult struct {
	v   ServerView
	err error
}

// addDuring runs Add(first) up to the point where it stores its key, then
// Add(second) to its end, then lets the first finish.
func addDuring(t *testing.T, first, second AddRequest) (*testEnv, addResult, addResult) {
	t.Helper()
	g := &gateStore{hold: true, entered: make(chan struct{}), release: make(chan struct{})}
	e := newTestEnv(t, func(o *Options) {
		g.Store = o.Store
		o.Store = g
	})
	ctx := context.Background()
	done := make(chan addResult, 1)
	go func() {
		v, err := e.m.Add(ctx, first)
		done <- addResult{v, err}
	}()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first Add never stored its key")
	}
	var b addResult
	b.v, b.err = e.m.Add(ctx, second)
	close(g.release)
	select {
	case a := <-done:
		return e, a, b
	case <-time.After(30 * time.Second):
		t.Fatal("the first Add never ended")
	}
	return nil, addResult{}, addResult{}
}

// keyBelongs: the server's stored private key opens and is the one of the
// public key the API shows for it.
func keyBelongs(t *testing.T, e *testEnv, v ServerView) {
	t.Helper()
	signer, err := loadSigner(context.Background(), e.m.o.Store, v.ID, KeyRef(v.ID))
	if err != nil {
		t.Fatalf("%s: its private key: %v", v.ID, err)
	}
	got, _ := e.m.Get(v.ID)
	if fp := PublicFingerprint(got.PublicKey); fp == "" || fp != PublicFingerprint(authorizedLine(signer.PublicKey(), "x")) {
		t.Fatalf("%s: the stored private key is not the one of its public key", v.ID)
	}
}

func TestConcurrentAddsWithTheSameIDKeepTheWinnersKey(t *testing.T) {
	a := AddRequest{ID: "lab", Name: "Lab A", Host: "a.example.com", Username: "alice"}
	b := AddRequest{ID: "lab", Name: "Lab B", Host: "b.example.com", Username: "bob"}
	e, ra, rb := addDuring(t, a, b)
	if !errors.Is(rb.err, ErrExists) {
		t.Fatalf("the second Add with an id being added: %v (%+v)", rb.err, rb.v)
	}
	if ra.err != nil {
		t.Fatalf("the first Add: %v", ra.err)
	}
	if list := e.m.List(); len(list) != 1 || list[0].ID != "lab" || list[0].Name != "Lab A" {
		t.Fatalf("servers: %+v", list)
	}
	keyBelongs(t, e, ra.v)
	// taken while the server exists, free again once it is removed
	if _, err := e.m.Add(context.Background(), b); !errors.Is(err, ErrExists) {
		t.Fatalf("an Add with an existing id: %v", err)
	}
	if err := e.m.Remove(context.Background(), "lab", false); err != nil {
		t.Fatal(err)
	}
	v, err := e.m.Add(context.Background(), b)
	if err != nil || v.ID != "lab" {
		t.Fatalf("an Add with a freed id: %v %+v", err, v)
	}
	keyBelongs(t, e, v)
}

func TestConcurrentAddsWithTheSameNameGetTheirOwnIDs(t *testing.T) {
	a := AddRequest{Name: "Lab", Host: "a.example.com", Username: "alice"}
	b := AddRequest{Name: "Lab", Host: "b.example.com", Username: "bob"}
	e, ra, rb := addDuring(t, a, b)
	if ra.err != nil || rb.err != nil {
		t.Fatalf("adds: %v, %v", ra.err, rb.err)
	}
	if ra.v.ID != "lab" || rb.v.ID != "lab-2" {
		t.Fatalf("ids %q and %q, want lab and lab-2", ra.v.ID, rb.v.ID)
	}
	if len(e.m.List()) != 2 {
		t.Fatalf("servers: %+v", e.m.List())
	}
	keyBelongs(t, e, ra.v)
	keyBelongs(t, e, rb.v)
}

// failingInsert fails InsertRemoteServer while *fail.
type failingInsert struct {
	Store
	fail *bool
}

func (f *failingInsert) InsertRemoteServer(ctx context.Context, r db.RemoteServer) error {
	if *f.fail {
		return errors.New("failingInsert: no row")
	}
	return f.Store.InsertRemoteServer(ctx, r)
}

// A failed Add frees its id.
func TestFailedAddFreesItsID(t *testing.T) {
	fail := true
	e := newTestEnv(t, func(o *Options) { o.Store = &failingInsert{Store: o.Store, fail: &fail} })
	req := AddRequest{ID: "lab", Name: "Lab", Host: "a.example.com", Username: "alice"}
	if _, err := e.m.Add(context.Background(), req); err == nil {
		t.Fatal("the insert did not fail")
	}
	fail = false
	v, err := e.m.Add(context.Background(), req)
	if err != nil || v.ID != "lab" {
		t.Fatalf("an Add after a failed one: %v %+v", err, v)
	}
	keyBelongs(t, e, v)
}
