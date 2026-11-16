// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// ssh-mvp, security review 3 L6: the SSH servers are off unless
// LECTUREFLOW_EXPERIMENTAL_SSH=1; the agent builds never come from the
// working directory.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

func sshWiring(t *testing.T, env string) (*sched.Scheduler, *devices.Registry) {
	t.Helper()
	t.Setenv(SSHEnv, env)
	dir := t.TempDir()
	st, err := db.Open(filepath.Join(dir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// An ssh row and its server left by an earlier run with the switch on.
	if err := st.InsertASRDevice(context.Background(), model.ASRDevice{ID: "ssh:mac", Backend: model.BackendMLX, Label: "Mac",
		Enabled: true, Model: "small", Precision: model.PrecisionFP16, MaxParallel: 1, Priority: 900}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ASR.FakeResult = "fake"
	reg := devices.NewRegistry(st, cfg, dir, devices.RegistryOptions{})
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := sched.New(sched.Options{Store: st, Config: cfg, Devices: reg, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	wireSSH(context.Background(), cfg, st, reg, s, api.NewHub(), "dev")
	t.Cleanup(func() { closeSSH(s) })
	return s, reg
}

func TestSSHOffByDefault(t *testing.T) {
	for _, env := range []string{"", "0", "true", "yes"} {
		s, reg := sshWiring(t, env)
		if s.SSH() != nil {
			t.Fatalf("%q: the SSH servers are wired", env)
		}
		if slices.ContainsFunc(reg.List(), func(d model.ASRDevice) bool { return model.IsSSHDevice(d.ID) }) {
			t.Fatalf("%q: an ssh row is in the device table", env)
		}
	}
}

func TestSSHOnWithTheSwitch(t *testing.T) {
	s, reg := sshWiring(t, "1")
	if s.SSH() == nil {
		t.Fatal("not wired with the switch on")
	}
	if !slices.ContainsFunc(reg.List(), func(d model.ASRDevice) bool { return d.ID == "ssh:mac" }) {
		t.Fatal("the ssh row is not in the device table")
	}
	// ssh-1b: the resource bar gets the servers' cards (none added yet)
	cards := sshCards(s)
	if cards == nil {
		t.Fatal("no card source with the switch on")
	}
	if got := cards(context.Background()); len(got) != 0 {
		t.Fatalf("cards %+v", got)
	}
}

// ssh-1b: with the switch off the resource service has no card source.
func TestSSHCardsOffByDefault(t *testing.T) {
	s, _ := sshWiring(t, "")
	if sshCards(s) != nil {
		t.Fatal("a card source with the SSH servers off")
	}
}

func TestAgentDirNeverFromTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "tools", "lf-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	cfg := config.Default()
	exeDir := t.TempDir()
	exe := filepath.Join(exeDir, "lectureflow.exe")
	if d := agentDir(cfg, "dev", exe); d != "" {
		t.Fatalf("found %s (the working directory's tools/lf-agent must not count)", d)
	}
	// A development build: tools/lf-agent next to the executable.
	_ = os.MkdirAll(filepath.Join(exeDir, "tools", "lf-agent"), 0o755)
	if d := agentDir(cfg, "dev", exe); d != filepath.Join(exeDir, "tools", "lf-agent") {
		t.Fatalf("development build: %q", d)
	}
	// A release build: only the packaged resources.
	if d := agentDir(cfg, "3.2.0", exe); d != "" {
		t.Fatalf("release build used %s", d)
	}
	res := t.TempDir()
	_ = os.MkdirAll(filepath.Join(res, "agent"), 0o755)
	cfg.ResourcesDir = res
	if d := agentDir(cfg, "3.2.0", exe); d != filepath.Join(res, "agent") {
		t.Fatalf("resources: %q", d)
	}
}
