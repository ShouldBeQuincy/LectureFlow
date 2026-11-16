// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// Error codes of the SSH inference servers (ssh-mvp; job.error and
// stats.remote_fallback; Chinese texts in testdata/api/error_codes_backend.json,
// listed through stages.ErrorCodes). device is the server's display name.
const (
	CodeOffline           = "remote.offline"            // device
	CodeAuthFailed        = "remote.auth_failed"        // device
	CodeHostKeyUnknown    = "remote.host_key_unknown"   // device
	CodeHostKeyMismatch   = "remote.host_key_mismatch"  // device, fingerprint
	CodeSetupNeeded       = "remote.setup_needed"       // device
	CodeInstallFailed     = "remote.install_failed"     // device, step
	CodeAgentFailed       = "remote.agent_failed"       // device
	CodeAgentMissing      = "remote.agent_missing"      // device (this installation has no lf-agent build)
	CodeProtocol          = "remote.protocol"           // device, reason
	CodeUploadFailed      = "remote.upload_failed"      // device
	CodeDownloadFailed    = "remote.download_failed"    // device
	CodeResultInvalid     = "remote.result_invalid"     // device, reason
	CodeWorkerFailed      = "remote.worker_failed"      // device, code
	CodeDeviceUnusable    = "remote.device_unusable"    // device
	CodeOutOfMemory       = "remote.out_of_memory"      // device
	CodeNoSpace           = "remote.no_space"           // device, free_mb, need_mb
	CodeDisconnected      = "remote.disconnected"       // device, minutes
	CodeBusy              = "remote.busy"               // device
	CodeUnsupported       = "remote.unsupported"        // device, platform
	CodeNotConfigured     = "remote.not_configured"     // device
	CodeAudioUnreadable   = "remote.audio_unreadable"   //
	CodeCleanupIncomplete = "remote.cleanup_incomplete" // device (warning: remote files left; reaped later)
	// CodeSelfTestSSH: the device self-test of an SSH server's row (its
	// speed shows in every lecture; Settings → Remote servers tests the
	// connection). Colab's is sched.CodeSelfTestRemote.
	CodeSelfTestSSH = "selftest.remote_ssh" // device
)

// ErrorCodes lists them (stages.ErrorCodes includes them).
var ErrorCodes = []string{
	CodeOffline, CodeAuthFailed, CodeHostKeyUnknown, CodeHostKeyMismatch, CodeSetupNeeded, CodeInstallFailed,
	CodeAgentFailed, CodeAgentMissing, CodeProtocol, CodeUploadFailed, CodeDownloadFailed, CodeResultInvalid, CodeWorkerFailed,
	CodeDeviceUnusable, CodeOutOfMemory, CodeNoSpace, CodeDisconnected, CodeBusy, CodeUnsupported,
	CodeNotConfigured, CodeAudioUnreadable, CodeCleanupIncomplete, CodeSelfTestSSH,
}
