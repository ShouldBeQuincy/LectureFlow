// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

import (
	"errors"
	"fmt"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// remoteErr is a classified error: a *remote.Error wrapping the coded
// error the job keeps (stats.remote_fallback / job.error).
func remoteErr(kind remote.ErrKind, code string, params map[string]any, waitingFor string, cause error) error {
	if cause == nil {
		cause = errors.New(code)
	}
	return &remote.Error{Kind: kind, Code: code, Params: params, WaitingFor: waitingFor,
		Err: &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params}, Err: cause}}
}

func fallback(code string, params map[string]any, cause error) error {
	return remoteErr(remote.ErrFallback, code, params, "", cause)
}

func busy(code string, params map[string]any, waitingFor string, cause error) error {
	return remoteErr(remote.ErrBusy, code, params, waitingFor, cause)
}

// connError maps a connection error to its card state, code and
// waiting_for.
func connError(err error) (state, code, waitingFor string) {
	switch {
	case errors.Is(err, errHostKeyUnknown):
		return StateUntrusted, remote.CodeHostKeyUnknown, model.WaitRemoteAuth
	case errors.Is(err, errHostKeyMismatch):
		return StateKeyChanged, remote.CodeHostKeyMismatch, model.WaitRemoteAuth
	case errors.Is(err, errAuth):
		return StateAuth, remote.CodeAuthFailed, model.WaitRemoteAuth
	case errors.Is(err, errUnsupported):
		return StateUnsupported, remote.CodeUnsupported, model.WaitRemoteSetup
	case errors.Is(err, errInstall):
		return StateSetup, remote.CodeInstallFailed, model.WaitRemoteSetup
	case errors.Is(err, errSetup):
		return StateSetup, remote.CodeSetupNeeded, model.WaitRemoteSetup
	case errors.Is(err, ErrNoDataKey):
		return StateAuth, remote.CodeAuthFailed, model.WaitRemoteAuth
	case errors.Is(err, remote.ErrContract):
		return StateError, remote.CodeProtocol, model.WaitRemoteOffline
	case errors.Is(err, errAgentMissing): // before errAgent: a missing build is wrapped in it
		return StateError, remote.CodeAgentMissing, model.WaitRemoteSetup
	case errors.Is(err, errAgent):
		return StateError, remote.CodeAgentFailed, model.WaitRemoteSetup
	}
	return StateOffline, remote.CodeOffline, model.WaitRemoteOffline
}

var (
	errSetup = errors.New("the server's environment is not installed")
	errAgent = errors.New("lf-agent could not be installed or started")
	// errAgentMissing: this installation has no lf-agent build for the
	// server's platform (the installers leave lf-agent out while the SSH
	// servers are experimental; desktop/scripts/stage.js).
	errAgentMissing = errors.New("this installation carries no lf-agent build")
)

// agentRunError maps a run's error event to the lecture's error.
func agentRunError(label string, ev remote.AgentEvent) error {
	p := map[string]any{"device": label}
	cause := fmt.Errorf("remote worker: %s (exit %d)", ev.Code, ev.Exit)
	switch ev.Code {
	case remote.AgentDeviceUnusable:
		return fallback(remote.CodeDeviceUnusable, p, cause)
	case remote.AgentOutOfMemory:
		return fallback(remote.CodeOutOfMemory, p, cause)
	case remote.AgentNoSpace:
		p["free_mb"], p["need_mb"] = ev.FreeMB, ev.NeedMB
		return fallback(remote.CodeNoSpace, p, cause)
	case remote.AgentAudioMissing, remote.AgentAudioMismatch:
		return fallback(remote.CodeUploadFailed, p, cause)
	case remote.AgentKitMissing:
		return fallback(remote.CodeSetupNeeded, p, cause)
	}
	p["code"] = ev.Code
	return fallback(remote.CodeWorkerFailed, p, cause)
}

// ConnErrorCode is the error code of a failed Connect / Trust / Test /
// Install (remote.Code*): the API's answer and the card's last error.
func ConnErrorCode(err error) string {
	_, code, _ := connError(err)
	return code
}
