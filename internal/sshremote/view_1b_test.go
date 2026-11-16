// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// ssh-1b: what the settings' wizard, the server cards and the diagnostics
// read from a server's view (the device row's model, the recommendation,
// the agent's version against this build, the kit's size, the latency),
// the cards of every server for the resource bar, and the authorized_keys
// command per system.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

func TestViewAfterProbe(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v0, err := e.m.Add(context.Background(), AddRequest{Name: "Zed Mac", Host: "127.0.0.1", Port: 22, Username: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if v0.Model != "" || v0.Recommended != nil || v0.KitSize != nil || v0.LatencyMS != nil || v0.AgentCurrent {
		t.Fatalf("before a probe: %+v", v0)
	}
	v := addTrusted(t, e, f)
	d, _ := e.devs.Get(v.DeviceID)
	if v.Model == "" || v.Model != d.Model || v.Precision != d.Precision {
		t.Fatalf("model of the row: view %q/%q, row %q/%q", v.Model, v.Precision, d.Model, d.Precision)
	}
	if v.Recommended == nil || v.Recommended.Model != d.Model || v.Recommended.Precision != model.PrecisionFP16 {
		t.Fatalf("recommendation: %+v (row %s)", v.Recommended, d.Model)
	}
	// the test build's agent says 0.0.0-dev, as the manager does without a version
	if !v.AgentCurrent || v.AgentVersion != "0.0.0-dev" {
		t.Fatalf("agent current: %v %q", v.AgentCurrent, v.AgentVersion)
	}
	if v.KitSize != nil {
		t.Fatalf("a size without a manifest: %+v", v.KitSize)
	}
	// The cards of every server, by name: "Test Mac" (connected) before "Zed Mac".
	cards := e.m.Cards(context.Background())
	if len(cards) != 2 || cards[0].Label != "Test Mac" || cards[1].Label != "Zed Mac" {
		t.Fatalf("cards %+v", cards)
	}
	for _, c := range cards {
		if c.Kind != remote.KindSSH || c.SSH == nil || !model.IsSSHDevice(c.ID) {
			t.Fatalf("card %+v", c)
		}
	}
	// A newer build: the agent on the server is not current any more.
	e.m.o.AgentVersion = "9.9.9"
	if v, _ := e.m.Get(v.ID); v.AgentCurrent {
		t.Fatal("an agent of another version counts as current")
	}
}

func TestKitSizeFromTheManifest(t *testing.T) {
	manifest := filepath.Join("..", "..", "packaging", "components", "components.json")
	ks, err := kitSizeFromManifest(manifest, remote.PlatformLinuxX64, remote.EngineFasterWhisper, "large-v3", "float16")
	if err != nil {
		t.Fatal(err)
	}
	// python + asr-cuda + the model: about 4.7 GB to download
	if ks.DownloadBytes < 3_000_000_000 || ks.DownloadBytes > 8_000_000_000 || ks.InstalledBytes < ks.DownloadBytes {
		t.Fatalf("linux large-v3: %+v", ks)
	}
	mac, err := kitSizeFromManifest(manifest, remote.PlatformDarwinArm64, remote.EngineMLX, "large-v3-turbo", model.PrecisionFP16)
	if err != nil || mac.DownloadBytes <= 0 || mac.DownloadBytes >= ks.DownloadBytes {
		t.Fatalf("mac turbo: %+v %v", mac, err)
	}
	if _, err := kitSizeFromManifest(manifest, remote.PlatformLinuxX64, remote.EngineFasterWhisper, "no-such-model", "float16"); err == nil {
		t.Fatal("a model the manifest lacks has a size")
	}
	// The manager caches it per kit and answers nil without a manifest.
	m := &Manager{o: Options{Manifest: manifest}}
	a := m.kitSize(remote.PlatformLinuxX64, remote.EngineFasterWhisper, "large-v3", "float16")
	if a == nil || *a != ks || m.kitSize(remote.PlatformLinuxX64, remote.EngineFasterWhisper, "large-v3", "float16") != a {
		t.Fatalf("cached size: %+v", a)
	}
	if (&Manager{}).kitSize(remote.PlatformLinuxX64, remote.EngineFasterWhisper, "large-v3", "float16") != nil {
		t.Fatal("a size without a manifest")
	}
}

func TestPublicKeyCommands(t *testing.T) {
	e := newTestEnv(t, nil)
	v, err := e.m.Add(context.Background(), AddRequest{Name: "Win box", Host: "198.51.100.5", Username: "q"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := e.m.PublicKeyOf(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.Command, "~/.ssh/authorized_keys") || !strings.Contains(k.Command, k.Line) {
		t.Fatalf("posix: %s", k.Command)
	}
	if !strings.Contains(k.CommandWindows, `$env:USERPROFILE\.ssh\authorized_keys`) || !strings.Contains(k.CommandWindows, "'"+k.Line+"'") ||
		strings.Contains(k.CommandWindows, "administrators_authorized_keys") {
		t.Fatalf("windows: %s", k.CommandWindows)
	}
	if !strings.Contains(k.CommandWindowsAdmin, "administrators_authorized_keys") {
		t.Fatalf("windows admin: %s", k.CommandWindowsAdmin)
	}
}
