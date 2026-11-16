// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

// Remote transcription devices (ssh-mvp; .claude/research/remote-ssh-plan.md
// §2.1): Colab (colab:remote, SPEC-v3 V14) and the user's own SSH inference
// servers (ssh:<slug>). An ssh row's backend is the engine the server runs
// (faster-whisper or mlx), so the asr_device CHECK is unchanged; every
// place that would take "backend is faster-whisper" for "a local CUDA
// device" asks IsRemoteDevice first.

import (
	"regexp"
	"strings"
)

// SSHDevicePrefix starts the id of an SSH inference server's device row.
const SSHDevicePrefix = "ssh:"

// SSHSlugPattern is the slug of an SSH server (its device id is
// "ssh:"+slug and its remote_server.id the slug). It recognises every
// server that exists; a new one's slug is an SSHNewSlugPattern.
var SSHSlugPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// SSHNewSlugPattern is the slug a new SSH server may get (SEC7 L7): an
// SSHSlugPattern that starts with a letter or a digit, so that nothing made
// of it ("lectureflow-<slug>", "ssh:<slug>") can ever read as an option.
// Servers added before keep their slug (SSHSlugPattern).
var SSHNewSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// IsSSHDevice reports whether id is an SSH inference server's device id.
func IsSSHDevice(id string) bool {
	return strings.HasPrefix(id, SSHDevicePrefix) && SSHSlugPattern.MatchString(id[len(SSHDevicePrefix):])
}

// IsRemoteDevice reports whether id is a remote transcription device
// (colab:* or ssh:*): it uses neither the machine's power nor its cooling,
// so it is outside the power-aware policy and multi_device, and no local
// worker or self-test ever runs for it.
func IsRemoteDevice(id string) bool {
	return strings.HasPrefix(id, "colab:") || IsSSHDevice(id)
}

// SSHDeviceID is the device id of the SSH server slug.
func SSHDeviceID(slug string) string { return SSHDevicePrefix + slug }

// SSHSlug is the slug of an ssh:* device id ("" for other ids).
func SSHSlug(id string) string {
	if !IsSSHDevice(id) {
		return ""
	}
	return id[len(SSHDevicePrefix):]
}

// waiting_for values of the SSH servers (ssh-mvp; user decision 2026-10-27,
// plan §10 item 4). remote_busy, remote_auth and vram are shared with Colab
// and the local devices.
const (
	// WaitRemoteOffline: the server the lecture may run on is not reachable
	// (connecting, reconnecting, or down).
	WaitRemoteOffline = "remote_offline"
	// WaitRemoteSetup: the server is reachable but its agent, Python
	// environment or model is not installed (or is being installed).
	WaitRemoteSetup = "remote_setup"
	// WaitRemoteFallbackSSH: an SSH server handed the lecture back
	// (stats.remote_fallback with a remote.* code: it is never sent to a
	// remote device automatically again) and no local device can take it
	// (WaitRemoteFallback is Colab's, which offers "改用 Colab 处理").
	WaitRemoteFallbackSSH = "remote_fallback_ssh"
)

// ActionUseRemote is "改用 <服务器> 处理" (POST /api/lectures/{id}/use-remote
// {"device": "ssh:<slug>"}).
const ActionUseRemote = "use_remote"
