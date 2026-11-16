// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// SEC7 M1 in the mock (ssh_key_changed): gpu-box presents another host key;
// connect answers changed with both fingerprints, trust needs replace and
// the old fingerprint, then the new key is the confirmed one.

import (
	"encoding/json"
	"strings"
	"testing"

	"lectureflow/internal/remote"
	"lectureflow/internal/sshremote"
)

func TestMockSSHKeyChanged(t *testing.T) {
	u := testServer(t).URL
	q := "?mock=ssh_key_changed"
	code, b, _ := mockDo(t, "GET", u+"/api/remote/servers/gpu-box"+q, "")
	v := sshView(t, code, b, 200)
	if v.State != sshremote.StateKeyChanged || !v.Trusted || v.HostKeyFP != sshHostKeyFP || v.LastError == nil ||
		v.LastError.Code != remote.CodeHostKeyMismatch || v.LastError.Params["fingerprint"] != sshNewHostKeyFP {
		t.Fatalf("server: %+v", v)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/test"+q, "")
	if code != 502 || !strings.Contains(string(b), `"fingerprint":"`+sshNewHostKeyFP+`"`) {
		t.Fatalf("test: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/connect"+q, "")
	var cr sshremote.ConnectResult
	if code != 200 || json.Unmarshal(b, &cr) != nil || !cr.Changed || cr.HostKey == nil || cr.HostKey.Fingerprint != sshNewHostKeyFP || cr.PreviousFingerprint != sshHostKeyFP {
		t.Fatalf("connect: %d %s", code, b)
	}
	for _, body := range []string{
		`{"hop":0,"fingerprint":"` + sshNewHostKeyFP + `"}`,
		`{"hop":0,"fingerprint":"` + sshNewHostKeyFP + `","replace":true}`,
		`{"hop":0,"fingerprint":"` + sshNewHostKeyFP + `","replace":true,"previous":"` + sshNewHostKeyFP + `"}`,
	} {
		if code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/trust"+q, body); code != 409 || !strings.Contains(string(b), `"remote_server.host_key_pinned"`) {
			t.Fatalf("trust %s: %d %s", body, code, b)
		}
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/trust"+q, `{"hop":0,"fingerprint":"`+sshNewHostKeyFP+`","replace":true,"previous":"`+sshHostKeyFP+`"}`)
	cr = sshremote.ConnectResult{}
	if code != 200 || json.Unmarshal(b, &cr) != nil || cr.Changed || cr.Server.HostKeyFP != sshNewHostKeyFP || cr.Server.State != sshremote.StateReady {
		t.Fatalf("replace: %d %s", code, b)
	}
	code, b, _ = mockDo(t, "POST", u+"/api/remote/servers/gpu-box/connect"+q, "")
	cr = sshremote.ConnectResult{}
	if code != 200 || json.Unmarshal(b, &cr) != nil || cr.Changed || cr.HostKey != nil {
		t.Fatalf("connect after replacing: %d %s", code, b)
	}
}
