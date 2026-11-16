// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The SSH inference servers in the service (ssh-mvp): internal/sshremote's
// manager over the database and the device registry, attached to the
// scheduler (sched.Scheduler.SetSSH; the API reaches it through the
// scheduler).
//
// Off unless LECTUREFLOW_EXPERIMENTAL_SSH=1 (security review 3, L6: not in
// 3.2.0): then nothing is wired, the device table leaves ssh:* rows out,
// /api/remote/* and use-remote answer 404, and migration 014's table stays
// unused. The lf-agent builds come only from the packaged resources
// (resources/agent/<platform>/lf-agent, when the installer carries them)
// or, in a development build (no version set), from tools/lf-agent next to
// the executable (make.ps1 agent); never from the working directory. When
// neither has a build for a server's platform, the servers can still be
// added, connected and trusted, but installing the agent there fails
// (remote.agent_missing) and the server cannot transcribe. The installers
// leave lf-agent out while the feature is experimental (desktop/scripts/
// stage.js, LECTUREFLOW_BUNDLE_LF_AGENT).
// ssh-1b: the servers' cards go to the resource bar (sshCards).

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
	"lectureflow/internal/sshremote"
)

// SSHEnv switches the SSH servers on ("1").
const SSHEnv = "LECTUREFLOW_EXPERIMENTAL_SSH"

// sshEnabled reports whether the SSH servers are switched on.
func sshEnabled(getenv func(string) string) bool { return getenv(SSHEnv) == "1" }

// devBuild: a build without a version (go run, make.ps1 build).
func devBuild(appVersion string) bool { return appVersion == "" || appVersion == "dev" }

// agentDir is where the lf-agent builds are ("" = none found): the
// packaged resources, else (development builds only) tools/lf-agent next
// to the executable.
func agentDir(cfg *config.Config, appVersion, exe string) string {
	var dirs []string
	if cfg.ResourcesDir != "" { // the configured resources (-resources, the desktop shell)
		if abs, err := filepath.Abs(cfg.ResourcesDir); err == nil {
			dirs = append(dirs, filepath.Join(abs, "agent"))
		}
	}
	if devBuild(appVersion) && exe != "" && filepath.IsAbs(exe) {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "tools", "lf-agent"))
	}
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}

// wireSSH builds the manager and attaches it to the scheduler when the SSH
// servers are switched on; logged and skipped when it cannot be built (then
// everything stays as with the switch off).
func wireSSH(ctx context.Context, cfg *config.Config, st db.Store, reg *devices.Registry, s *sched.Scheduler,
	hub *api.Hub, appVersion string) {
	if !sshEnabled(os.Getenv) {
		return // off (the default): no manager, no ssh rows, the endpoints answer 404
	}
	slog.Warn("ssh servers: experimental feature switched on", "env", SSHEnv)
	exe, _ := os.Executable()
	store, ok := st.(sshremote.Store)
	if !ok {
		slog.Warn("ssh servers: the store has no remote_server table")
		return
	}
	agents := agentDir(cfg, appVersion, exe)
	if agents == "" {
		slog.Warn("ssh servers: this installation has no lf-agent builds; servers can be added but not used (remote.agent_missing)")
	}
	m, err := sshremote.New(ctx, sshremote.Options{
		Store: store, Devices: reg, AgentDir: agents, Manifest: cfg.ComponentsManifest,
		WorkerDir: cfg.WorkerDir, FFmpeg: cfg.FFmpeg, AgentVersion: appVersion,
		OnChange: func() {
			hub.Changed("devices", "")
			s.Kick()
		},
	})
	if err != nil {
		slog.Warn("ssh servers: not available", "err", err)
		return
	}
	reg.EnableSSH() // the ssh:* rows are in the table from now on
	s.SetSSH(m)
}

// sshCards is the resource service's RemoteCards (ssh-1b): the servers'
// cards, or nil while the SSH servers are off (wireSSH built no manager).
func sshCards(s *sched.Scheduler) func(context.Context) []remote.Card {
	m, ok := s.SSH().(*sshremote.Manager)
	if !ok || m == nil {
		return nil
	}
	return m.Cards
}

// closeSSH ends the servers' connections (graceful stop; the agents keep
// what an interrupted lecture needs for their retain time).
func closeSSH(s *sched.Scheduler) {
	if m, ok := s.SSH().(*sshremote.Manager); ok && m != nil {
		_ = m.Close()
	}
}
