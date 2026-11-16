// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// ssh-1b: the SSH servers in the mock — off (404) outside the ssh_*
// scenarios; the wizard's steps from adding to "how to use it"; the device
// rows and their order; the resource cards; removing.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/remote"
	"lectureflow/internal/sshremote"
)

func sshView(t *testing.T, code int, b []byte, want int) sshremote.ServerView {
	t.Helper()
	var v sshremote.ServerView
	if code != want || json.Unmarshal(b, &v) != nil {
		t.Fatalf("%d %s (want %d)", code, b, want)
	}
	return v
}

func sshDevices(t *testing.T, u, sc string) []api.ASRDeviceView {
	t.Helper()
	code, b, _ := mockDo(t, "GET", u+"/api/asr/devices?mock="+sc, "")
	var list []api.ASRDeviceView
	if code != 200 || json.Unmarshal(b, &list) != nil {
		t.Fatalf("devices: %d %s", code, b)
	}
	return list
}

func TestMockSSHOffOutsideItsScenarios(t *testing.T) {
	u := testServer(t).URL
	for _, p := range []string{"GET /api/remote/servers", "POST /api/remote/servers", "POST /api/remote/servers/x/connect", "POST /api/lectures/x/use-remote"} {
		m, path, _ := strings.Cut(p, " ")
		code, b, _ := mockDo(t, m, u+path+"?mock=1", "{}")
		if code != 404 || !strings.Contains(string(b), `"api.not_found"`) {
			t.Errorf("%s: %d %s", p, code, b)
		}
	}
	code, b, _ := mockDo(t, "GET", u+"/api/resources?mock=1", "")
	if code != 200 || strings.Contains(string(b), `"kind":"ssh"`) {
		t.Fatalf("resources without SSH: %d", code)
	}
}

func TestMockSSHWizard(t *testing.T) {
	old := sshInstallScale
	sshInstallScale = 0.02
	t.Cleanup(func() { sshInstallScale = old })
	u := testServer(t).URL
	q := "?mock=ssh_none"
	code, b, _ := mockDo(t, "GET", u+"/api/remote/servers"+q, "")
	if code != 200 || !strings.Contains(string(b), `"servers":[]`) {
		t.Fatalf("list: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers"+q, `{"name":"x","host":"a b","username":"q"}`)
	if code != 400 || !strings.Contains(string(b), `"remote_server.invalid"`) || !strings.Contains(string(b), `"field":"host"`) {
		t.Fatalf("invalid host: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers"+q, `{"name":"GPU Box","host":"gpu-box.local","username":"student"}`)
	v := sshView(t, code, b, 201)
	if v.ID != "gpu-box" || v.State != sshremote.StateUntrusted || v.Port != 22 || !strings.HasPrefix(v.PublicKey, "restrict ssh-ed25519 ") {
		t.Fatalf("added: %+v", v)
	}
	code, b, _ = mockDo(t, "GET", u+"/api/remote/servers/gpu-box/pubkey"+q, "")
	var k sshremote.PublicKey
	if code != 200 || json.Unmarshal(b, &k) != nil || !strings.Contains(k.Command, k.Line) || !strings.Contains(k.CommandWindows, "authorized_keys") {
		t.Fatalf("pubkey: %d %s", code, b)
	}
	// the preview shows the commands the application gives (sshremote.PublicKeyOf)
	if !strings.Contains(k.Command, `printf '\n%s\n' '`+k.Line+`' >> ~/.ssh/authorized_keys`) ||
		!strings.Contains(k.CommandWindows, `-Value '', '`+k.Line+`'`) || !strings.Contains(k.CommandWindowsAdmin, `-Value '', '`+k.Line+`'`) {
		t.Fatalf("pubkey commands: %s", b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/connect"+q, "")
	var cr sshremote.ConnectResult
	if code != 200 || json.Unmarshal(b, &cr) != nil || cr.HostKey == nil || !strings.HasPrefix(cr.HostKey.Fingerprint, "SHA256:") || cr.HostKey.Check == "" {
		t.Fatalf("connect: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/trust"+q, `{"hop":0,"fingerprint":"SHA256:wrong"}`)
	if code != 400 {
		t.Fatalf("a wrong fingerprint: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/trust"+q, `{"hop":0,"fingerprint":"`+cr.HostKey.Fingerprint+`"}`)
	cr = sshremote.ConnectResult{}
	if code != 200 || json.Unmarshal(b, &cr) != nil || cr.HostKey != nil {
		t.Fatalf("trust: %d %s", code, b)
	}
	v = cr.Server
	if !v.Trusted || v.State != sshremote.StateSetup || v.Probe == nil || v.Probe.GPUName == "" || v.Model != "large-v3" || v.KitSize == nil ||
		v.Recommended == nil || v.KitReady {
		t.Fatalf("after trust: %+v", v)
	}
	// the device row exists, after the local devices (the default group)
	devs := sshDevices(t, u, "ssh_none")
	if devs[len(devs)-1].ID != "ssh:gpu-box" {
		t.Fatalf("row order %v", ids(devs))
	}
	// the model can be changed on the row
	code, b, _ = mockDo(t, "PATCH", u+"/api/asr/devices/ssh:gpu-box"+q, `{"model":"medium"}`)
	if code != 200 || !strings.Contains(string(b), `"model":"medium"`) {
		t.Fatalf("model: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/install"+q, "")
	v = sshView(t, code, b, 202)
	if v.State != sshremote.StateInstalling || v.Install == nil {
		t.Fatalf("installing: %+v", v)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, b, _ = mockDo(t, "GET", u+"/api/remote/servers/gpu-box"+q, "")
		v = sshView(t, code, b, 200)
		if v.KitReady || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !v.KitReady || v.State != sshremote.StateReady || v.Install != nil || !v.AgentCurrent {
		t.Fatalf("installed: %+v", v)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/test"+q, "")
	v = sshView(t, code, b, 200)
	if v.State != sshremote.StateReady || v.LatencyMS == nil {
		t.Fatalf("test: %+v", v)
	}
	// "use it first": the row goes above the local devices
	code, b, _ = mockDo(t, "PATCH", u+"/api/remote/servers/gpu-box"+q, `{"group":"prefer"}`)
	if v = sshView(t, code, b, 200); v.Settings.Group != sshremote.GroupPrefer {
		t.Fatalf("group: %+v", v)
	}
	if devs = sshDevices(t, u, "ssh_none"); devs[0].ID != "ssh:gpu-box" {
		t.Fatalf("row order %v", ids(devs))
	}
	code, b, _ = mockDo(t, "PATCH", u+"/api/remote/servers/gpu-box"+q, `{"paused":true}`)
	if v = sshView(t, code, b, 200); v.State != sshremote.StatePaused {
		t.Fatalf("paused: %+v", v)
	}
	code, _, _ = mockDo(t, "DELETE", u+"/api/remote/servers/gpu-box"+q+"&uninstall=1", "")
	if code != 204 {
		t.Fatalf("remove: %d", code)
	}
	for _, d := range sshDevices(t, u, "ssh_none") {
		if d.ID == "ssh:gpu-box" {
			t.Fatal("the row is left after removing the server")
		}
	}
}

func TestMockSSHReadyCards(t *testing.T) {
	u := testServer(t).URL
	code, b, _ := mockDo(t, "GET", u+"/api/remote/servers?mock=ssh_ready", "")
	var list struct {
		Servers []sshremote.ServerView `json:"servers"`
	}
	if code != 200 || json.Unmarshal(b, &list) != nil || len(list.Servers) != 3 {
		t.Fatalf("list: %d %s", code, b)
	}
	states := map[string]string{}
	for _, s := range list.Servers {
		states[s.ID] = s.State
	}
	if states["gpu-box"] != sshremote.StateBusy || states["mac-mini"] != sshremote.StateOffline || states["lab-server"] != sshremote.StateUntrusted {
		t.Fatalf("states %v", states)
	}
	code, b, _ = mockDo(t, "GET", u+"/api/resources?mock=ssh_ready", "")
	var res struct {
		Remotes []remote.Card `json:"remotes"`
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	if code != 200 || json.Unmarshal(b, &res) != nil {
		t.Fatalf("resources: %d %s", code, b)
	}
	byID := map[string]remote.Card{}
	for _, c := range res.Remotes {
		byID[c.ID] = c
	}
	box := byID["ssh:gpu-box"]
	if box.Kind != remote.KindSSH || box.Machine == nil || box.Usage == nil || len(box.Usage.GPUHistory) != remote.HistoryLen ||
		len(box.Jobs) != 1 || box.Jobs[0].LectureTitle == "" || box.SSH == nil || box.SSH.LatencyMS == nil {
		t.Fatalf("gpu-box card %+v", box)
	}
	if mac := byID["ssh:mac-mini"]; mac.Machine != nil || mac.LastError == nil || mac.LastError.Code != remote.CodeOffline {
		t.Fatalf("mac-mini card %+v", mac)
	}
	for _, d := range res.Devices {
		if strings.HasPrefix(d.ID, "ssh:") {
			t.Fatalf("an SSH row among the local device cards: %s", d.ID)
		}
	}
	// the rows: gpu-box first (used first), mac-mini after the local devices
	devs := sshDevices(t, u, "ssh_ready")
	if devs[0].ID != "ssh:gpu-box" || devs[len(devs)-1].ID != "ssh:mac-mini" {
		t.Fatalf("rows %v", ids(devs))
	}
	// a running server cannot be removed
	if code, _, _ := mockDo(t, "DELETE", u+"/api/remote/servers/gpu-box?mock=ssh_ready", ""); code != 409 {
		t.Fatalf("remove while transcribing: %d", code)
	}
}

func ids(list []api.ASRDeviceView) []string {
	out := make([]string, len(list))
	for i, d := range list {
		out[i] = d.ID
	}
	return out
}
