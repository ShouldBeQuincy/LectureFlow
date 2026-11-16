// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// ssh-1b: the wizard's "how to use it" moves the server's device row in the
// device order; the diagnostics zip lists the servers without host and user
// names, and the redaction of every file replaces them; the server's own
// model recommendation on its device row.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/secrets"
	"lectureflow/internal/sshremote"
)

// orderRegistry is a device table that records the order it is given
// (and fails with err when set).
type orderRegistry struct {
	deviceRegistry
	rows  []model.ASRDevice
	order []string
	calls int
	err   error
}

func (o *orderRegistry) List() []model.ASRDevice { return slices.Clone(o.rows) }

func (o *orderRegistry) Reorder(_ context.Context, ids []string) ([]model.ASRDevice, error) {
	o.calls++
	if o.err != nil {
		return nil, o.err
	}
	o.order = slices.Clone(ids)
	return nil, nil
}

// liveRegistry is an orderRegistry the rest of a running server can read
// too (diagnostics, the device views): no devices detected.
type liveRegistry struct{ *orderRegistry }

func (liveRegistry) Get(string) (model.ASRDevice, bool)          { return model.ASRDevice{}, false }
func (liveRegistry) Present(string) bool                         { return false }
func (liveRegistry) Detected() bool                              { return false }
func (liveRegistry) LastDetection() devices.Detection            { return devices.Detection{} }
func (liveRegistry) Prompts() []model.ASRDevice                  { return nil }
func (liveRegistry) DismissPrompt(context.Context, string) error { return nil }
func (liveRegistry) Detect(context.Context) (devices.Detection, []string, error) {
	return devices.Detection{}, nil, nil
}

// groupServers is one SSH server; a patch changes its group.
type groupServers struct {
	remoteServers
	v sshremote.ServerView
}

func (g *groupServers) Get(id string) (sshremote.ServerView, bool) { return g.v, id == g.v.ID }

func (g *groupServers) Update(_ context.Context, id string, p sshremote.Patch) (sshremote.ServerView, error) {
	if id != g.v.ID {
		return sshremote.ServerView{}, sshremote.ErrNotFound
	}
	if p.Group != nil {
		g.v.Settings.Group = *p.Group
	}
	return g.v, nil
}

func (g *groupServers) Device(string) (remote.Device, bool) { return nil, false }

// PATCH {group}: only a new group moves the device row (R13 S2: the wizard
// sends the group every time it ends, and a row the user dragged keeps its
// place); the group is saved and "devices" changes even when the row
// cannot be moved (R13 S3).
func TestPatchRemoteGroupMovesTheRowOnlyWhenItChanges(t *testing.T) {
	reg := &orderRegistry{rows: []model.ASRDevice{{ID: "cuda:0"}, {ID: "ssh:box"}}}
	e := newEnv(t, func(s *Server) { s.dev = liveRegistry{reg} })
	g := &groupServers{v: sshremote.ServerView{ID: "box", DeviceID: "ssh:box", Settings: sshremote.Settings{Group: sshremote.GroupSupplement}}}
	e.sc.SetSSH(g)
	c := e.hub.add(clientVisibility{visible: true})
	defer e.hub.remove(c)
	devicesChanged := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		k := changedKey{"devices", ""}
		was := c.changed[k]
		delete(c.changed, k)
		return was
	}
	patch := func(group string) sshremote.ServerView {
		t.Helper()
		res, body := e.do(http.MethodPatch, "/api/remote/servers/box", map[string]any{"group": group})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("PATCH group %s: %d %s", group, res.StatusCode, body)
		}
		var v sshremote.ServerView
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// the same group: the row stays where it is
	if v := patch(sshremote.GroupSupplement); v.Settings.Group != sshremote.GroupSupplement || reg.calls != 0 {
		t.Fatalf("unchanged group: %+v, %d reorders", v.Settings, reg.calls)
	}
	if !devicesChanged() {
		t.Error("unchanged group: no devices event")
	}
	// a new group: the row moves
	if v := patch(sshremote.GroupPrefer); v.Settings.Group != sshremote.GroupPrefer || reg.calls != 1 ||
		!slices.Equal(reg.order, []string{"ssh:box", "cuda:0"}) {
		t.Fatalf("new group: %+v, %d reorders, order %v", v.Settings, reg.calls, reg.order)
	}
	if !devicesChanged() {
		t.Error("new group: no devices event")
	}
	// the row cannot be moved: the group is still saved and announced
	reg.err = errors.New("devices: order_invalid")
	if v := patch(sshremote.GroupSupplement); v.Settings.Group != sshremote.GroupSupplement || reg.calls != 2 {
		t.Fatalf("failed move: %+v, %d reorders", v.Settings, reg.calls)
	}
	if g.v.Settings.Group != sshremote.GroupSupplement {
		t.Fatalf("failed move: the saved group is %q", g.v.Settings.Group)
	}
	if !devicesChanged() {
		t.Error("failed move: no devices event")
	}
}

func TestApplySSHGroupPlacesTheRow(t *testing.T) {
	rows := func(ids ...string) []model.ASRDevice {
		out := make([]model.ASRDevice, len(ids))
		for i, id := range ids {
			out[i] = model.ASRDevice{ID: id}
		}
		return out
	}
	for _, c := range []struct {
		name, group string
		rows        []string
		want        []string
	}{
		{"prefer: above the first local device", sshremote.GroupPrefer,
			[]string{"ssh:other", "cuda:0", "ssh:box", "ov:GPU", "colab:remote"}, []string{"ssh:other", "ssh:box", "cuda:0", "ov:GPU", "colab:remote"}},
		{"supplement: below the last local device", sshremote.GroupSupplement,
			[]string{"ssh:box", "cuda:0", "ov:GPU", "fw:cpu", "colab:remote"}, []string{"cuda:0", "ov:GPU", "fw:cpu", "ssh:box", "colab:remote"}},
		{"supplement without a local device: before Colab", sshremote.GroupSupplement,
			[]string{"ssh:box", "ssh:mac", "colab:remote"}, []string{"ssh:mac", "ssh:box", "colab:remote"}},
		{"prefer without a local device: first", sshremote.GroupPrefer,
			[]string{"ssh:mac", "ssh:box"}, []string{"ssh:box", "ssh:mac"}},
	} {
		reg := &orderRegistry{rows: rows(c.rows...)}
		s := &Server{dev: reg}
		if err := s.applySSHGroup(context.Background(), "ssh:box", c.group); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(reg.order, c.want) {
			t.Errorf("%s: %v, want %v", c.name, reg.order, c.want)
		}
	}
	// no row yet (before the first probe): nothing to move
	reg := &orderRegistry{rows: rows("cuda:0")}
	if err := (&Server{dev: reg}).applySSHGroup(context.Background(), "ssh:box", sshremote.GroupPrefer); err != nil || reg.order != nil {
		t.Fatalf("without a row: %v %v", reg.order, err)
	}
}

func TestRemoteServersInDiagnosticsWithoutHostOrUser(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	if err := secrets.SetKey(bytes.Repeat([]byte{7}, secrets.KeySize)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	withSSH(t, e)
	res, body := e.do(http.MethodPost, "/api/remote/servers", map[string]any{"name": "Lab GPU", "host": "gpu7.cs.example.org", "port": 2222,
		"username": "alice"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add: %d %s", res.StatusCode, body)
	}
	res, body = e.do(http.MethodPost, "/api/app/diagnostics", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	raw, ok := files["remote-servers.json"]
	if !ok {
		t.Fatalf("no remote-servers.json in %v", keysOf(files))
	}
	var got struct {
		Servers []sshremote.ServerView `json:"servers"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil || len(got.Servers) != 1 {
		t.Fatalf("%v %s", err, raw)
	}
	v := got.Servers[0]
	// SEC7 M2: the name, id and device id are numbered (the name "Lab GPU"
	// and the slug lab-gpu name the server too)
	if v.Host != "<host>" || v.Username != "<user>" || v.PublicKey != "" || v.Port != 2222 || v.Name != "server-1" ||
		v.ID != "server-1" || v.DeviceID != "ssh:server-1" || v.KeyFP == "" {
		t.Fatalf("redacted view: %+v", v)
	}
	for name, f := range files {
		if strings.Contains(f, "gpu7.cs.example.org") || strings.Contains(f, "alice") {
			t.Errorf("%s names the server's host or user", name)
		}
	}
}

func TestRemoteServersNotInDiagnosticsWhileOff(t *testing.T) {
	e := newEnv(t)
	res, body := e.do(http.MethodPost, "/api/app/diagnostics", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if _, ok := unzipAll(t, body)["remote-servers.json"]; ok {
		t.Fatal("remote-servers.json while the SSH servers are off")
	}
}

func TestRedactHostAndUserWords(t *testing.T) {
	r := &redactor{words: []redactWord{{word: "gpu-box.example.net", repl: "<host>"}, {word: "alice", repl: "<user>"}, {word: "user", repl: "<user>"}}}
	cases := map[string]string{
		`dial tcp gpu-box.example.net:22: connection refused`:                  `dial tcp <host>:22: connection refused`,
		`"host":"gpu-box.example.net","username":"alice"`:                      `"host":"<host>","username":"<user>"`,
		`ssh alice@gpu-box.example.net.`:                                       `ssh <user>@<host>.`,
		`alice alice`:                                                          `<user> <user>`,
		`my-gpu-box.example.net x.gpu-box.example.net gpu-box.example.network`: `my-gpu-box.example.net x.gpu-box.example.net gpu-box.example.network`,
		`alice_old alice2 Alice`:                                               `alice_old alice2 Alice`,
		`user <user> users`:                                                    `<user> <user> users`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
