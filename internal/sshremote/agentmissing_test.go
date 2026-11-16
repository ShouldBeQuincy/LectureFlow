// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// The installers leave lf-agent out while the SSH servers are experimental
// (desktop/scripts/stage.js): with the switch on, a server can still be
// added and trusted, but using it says plainly that this installation has
// no agent (remote.agent_missing), not that the server failed to start it.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

func TestFindAgentMissingBuild(t *testing.T) {
	for name, dir := range map[string]string{
		"no agent directory":      "",
		"no build for a platform": t.TempDir(),
		"directory gone":          filepath.Join(t.TempDir(), "agent"),
	} {
		_, err := findAgent(dir, remote.PlatformLinuxX64)
		if !errors.Is(err, errAgentMissing) {
			t.Errorf("%s: %v is not errAgentMissing", name, err)
		}
		// as the session wraps it (server.go)
		wrapped := fmt.Errorf("%w: %w", errAgent, err)
		if got := ConnErrorCode(wrapped); got != remote.CodeAgentMissing {
			t.Errorf("%s: code %q, want %q", name, got, remote.CodeAgentMissing)
		}
	}
	// an agent that fails to install or start keeps its own code
	if got := ConnErrorCode(fmt.Errorf("%w: exit 1", errAgent)); got != remote.CodeAgentFailed {
		t.Errorf("agent failure: code %q, want %q", got, remote.CodeAgentFailed)
	}
}

func TestServerWithoutAgentBuildsSaysSo(t *testing.T) {
	ctx := context.Background()
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, func(o *Options) { o.AgentDir = "" }) // an installer without lf-agent
	host, port := f.addr()
	v, err := e.m.Add(ctx, AddRequest{Name: "Test Mac", Host: host, Port: port, Username: "alice", Group: GroupPrefer})
	if err != nil {
		t.Fatal(err)
	}
	f.authorize(v.PublicKey)
	res, err := e.m.Connect(ctx, v.ID)
	if err != nil || res.HostKey == nil {
		t.Fatalf("connect: %v %+v", err, res)
	}
	// confirming the host key opens a session, which needs the agent
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: res.HostKey.Fingerprint}); !errors.Is(err, errAgentMissing) {
		t.Fatalf("trust without lf-agent: %v", err)
	}
	if _, err := e.m.Test(ctx, v.ID); !errors.Is(err, errAgentMissing) {
		t.Fatalf("test without lf-agent: %v", err)
	}
	got, _ := e.m.Get(v.ID)
	if !got.Trusted || got.State != StateError || got.LastError == nil || got.LastError.Code != remote.CodeAgentMissing {
		t.Fatalf("without lf-agent: %+v (last error %+v)", got, got.LastError)
	}
	if got.LastError.Params["device"] != v.Name {
		t.Fatalf("params %+v", got.LastError.Params)
	}
	for _, c := range f.sentCommands() {
		if strings.Contains(c, "lf-agent.part") {
			t.Fatalf("an agent was uploaded: %q", c)
		}
	}
	dev, _ := e.m.Device(v.DeviceID)
	if ok, wf := dev.Ready(ctx, 1); ok || wf != model.WaitRemoteSetup {
		t.Fatalf("ready without an agent: %v %q", ok, wf)
	}
}
