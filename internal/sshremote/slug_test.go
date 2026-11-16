// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC7 L7: Add takes no id that starts with "-" (the slug ends up in the
// key's comment and the device id; no option-like word is ever made of
// it). A server added before, with such an id, stays usable.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

func TestAddRefusesAnIDStartingWithADash(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	for _, id := range []string{"-x", "-", "--lab", "-oProxyCommand"} {
		var fe *FieldError
		if v, err := e.m.Add(ctx, AddRequest{ID: id, Name: "Lab", Host: "h.example.com", Username: "u"}); !errors.As(err, &fe) || fe.Field != "id" {
			t.Errorf("id %q: %v (%+v)", id, err, v)
		}
	}
	if len(e.m.List()) != 0 {
		t.Fatalf("servers: %+v", e.m.List())
	}
	for _, id := range []string{"x", "0-lab", "lab-", strings.Repeat("a", 32)} {
		if v, err := e.m.Add(ctx, AddRequest{ID: id, Name: "Lab", Host: "h.example.com", Username: "u"}); err != nil || v.ID != id {
			t.Errorf("id %q: %v (%+v)", id, err, v)
		}
	}
	// names never make such a slug
	for _, name := range []string{"-x", "--- lab ---", "-", "- -", "（实验室）", "-" + strings.Repeat("b", 40)} {
		v, err := e.m.Add(ctx, AddRequest{Name: name, Host: "h.example.com", Username: "u"})
		if err != nil || !model.SSHNewSlugPattern.MatchString(v.ID) {
			t.Errorf("name %q: id %q (%v)", name, v.ID, err)
		}
	}
}

func TestAServerAddedWithALeadingDashStaysUsable(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	// as an earlier build stored it
	const id = "-old"
	ref := KeyRef(id)
	sealed, pub, err := newKey("lectureflow-"+id, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.(Store).SetKV(ctx, ref, sealed); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := e.st.(Store).InsertRemoteServer(ctx, db.RemoteServer{ID: id, Name: "Old", Host: "old.example.com", Port: 22, Username: "u",
		JumpJSON: "[]", KeyRef: ref, PublicKey: pub, SettingsJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, Options{Store: e.st.(Store), Devices: e.devs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	v, ok := m.Get(id)
	if !ok || v.DeviceID != "ssh:-old" || !slices.Contains(m.DeviceIDs(), "ssh:-old") {
		t.Fatalf("the server is gone: %+v %v", v, m.DeviceIDs())
	}
	if _, ok := m.Device("ssh:-old"); !ok {
		t.Fatal("its device is gone")
	}
	name := "Old lab"
	if v, err = m.Update(ctx, id, Patch{Name: &name}); err != nil || v.Name != name {
		t.Fatalf("update: %v %+v", err, v)
	}
	if k, err := m.PublicKeyOf(id); err != nil || k.Line != pub {
		t.Fatalf("public key: %v %+v", err, k)
	}
	if _, err := m.Add(ctx, AddRequest{ID: id, Name: "Again", Host: "h.example.com", Username: "u"}); err == nil {
		t.Fatal("a second server with the old id was added")
	}
	if err := m.Remove(ctx, id, false); err != nil {
		t.Fatal(err)
	}
}
