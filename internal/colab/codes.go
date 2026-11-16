// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Error codes of this package (SPEC-v2 P14.3, SPEC-v3 V14). The zh-CN / en
// texts are in web/i18n/_parts/colab-core.{zh-CN,en}.json (errors.{code});
// the comments list the params. No param ever carries a credential, a
// token or text derived from the audio.
const (
	// The Colab service answered in a way this build does not expect
	// (V14.6): a missing or mistyped field, an unknown enum value, an
	// unexpected HTTP status. The lecture goes back to the local devices.
	CodeAPIChanged = "colab.api_changed" // endpoint (e.g. "tun/m/assign"), detail (what did not match)
	// The remote worker's standard output broke the event contract (V14.3
	// step 3): a line that is not JSON, an unknown event type or field, a
	// value that could carry text. The lecture goes back to the local devices.
	CodeContractViolation = "colab.contract_violation" // reason

	// Credentials (V14.2).
	CodeAuthMissing      = "colab.auth_missing"       // no credential imported
	CodeAuthExpired      = "colab.auth_expired"       // the lifecycle the user chose ended; the local copy was deleted
	CodeAuthRejected     = "colab.auth_rejected"      // Google rejected the credential (revoked, expired, missing scope)
	CodeADCNotFound      = "colab.adc_not_found"      // path
	CodeADCInvalid       = "colab.adc_invalid"        // field
	CodeADCUnsupported   = "colab.adc_unsupported"    // type (only "authorized_user" from gcloud is accepted)
	CodeADCMissingScope  = "colab.adc_missing_scope"  // scopes (space separated)
	CodeLifecycleInvalid = "colab.lifecycle_invalid"  // value
	CodeRevokeFailed     = "colab.revoke_failed"      // status (HTTP status or "network"); the local copy is deleted anyway
	CodeAuthEndpoint     = "colab.auth_endpoint_down" // status: the token endpoint failed (network, 5xx)

	// Session and accelerator (V14.3 step 1).
	CodeQueueTimeout           = "colab.queue_timeout"           // accelerator, minutes
	CodeAcceleratorUnavailable = "colab.accelerator_unavailable" // accelerator (HTTP 400: no entitlement / quota)
	CodeUnavailable            = "colab.unavailable"             // endpoint: transient failures did not stop
	CodeBootTimeout            = "colab.boot_timeout"            // minutes
	CodeSessionReclaimed       = "colab.session_reclaimed"       // Colab took the runtime back
	CodeConnectionLost         = "colab.connection_lost"         // the kernel connection broke and could not be resumed
	CodeLeftoverSession        = "colab.leftover_session"        // accelerator: an earlier session could not be released (no valid credential)
	CodeReleaseFailed          = "colab.release_failed"          // accelerator

	// Dispatch decisions (V14.3 step 4) and the run of one lecture.
	CodeWindowExhausted = "colab.window_exhausted" // remaining_min, need_min
	CodeWindowCooldown  = "colab.window_cooldown"  // minutes: the last session used its window up; no new one yet
	CodeBalanceLow      = "colab.balance_low"      // need (compute units, 2 decimals)
	CodeAuthTooShort    = "colab.auth_too_short"   // need_min: the credential ends before the lecture could finish
	CodeShutdownRequeue = "colab.shutdown_requeue" // "立即关机": the lecture goes back to the queue
	CodeBusy            = "colab.busy"             // another lecture is running on the remote runtime
	CodeRemoteFailed    = "colab.remote_failed"    // reason (the worker's error code, e.g. no_gpu, out_of_memory)
	CodeHashMismatch    = "colab.hash_mismatch"    // file (result.json): the download does not match the worker's sha256
	CodeAudioMissing    = "colab.audio_missing"    // file
	CodeSettingsInvalid = "colab.settings_invalid" // field
	CodeWorkerMissing   = "colab.worker_missing"   // file: a worker script to upload is missing locally
	CodeCancelled       = "colab.cancelled"        // the caller cancelled the lecture; the runtime's release has begun (or the runtime stays for other lectures)
	CodeUploadFailed    = "colab.upload_failed"    // file
	CodeDownloadFailed  = "colab.download_failed"  // file
	CodeLocalWrite      = "colab.local_write"      // file: the downloaded result could not be written locally
	CodeWorkerExited    = "colab.worker_exited"    // the remote worker ended without a done or error event

	// The in-app Google sign-in (3.1.0, login.go): gcloud runs in a
	// configuration directory of its own and the ADC file it writes is
	// imported like "导入授权".
	CodeGcloudMissing     = "colab.gcloud_missing"      // neither the gcloud component nor a gcloud on PATH
	CodeGcloudStartFailed = "colab.gcloud_start_failed" // gcloud could not be started (detail: the system's error)
	CodeLoginRunning      = "colab.login_running"       // a sign-in is already waiting for the browser
	CodeLoginTimeout      = "colab.login_timeout"       // minutes: the sign-in did not finish in time; gcloud was stopped
	CodeLoginFailed       = "colab.login_failed"        // exit_code: gcloud ended with an error (detail: its last ERROR line)

	// The account (colab-obs-core, account.go): Status.Account.ReadError
	// when ccu-info could not be read (a credential problem keeps its own
	// colab.auth_* code). reason: unavailable (network, Colab's 5xx),
	// api_changed (the answer does not look like ccu-info), other. Never a
	// lecture's error: nothing is handed back for it.
	CodeAccountUnavailable = "colab.account_unavailable" // reason
)

// ErrorCodes lists the codes this package defines (for cmd's
// TestBackendErrorCodesCatalog once the lead adds the package there).
var ErrorCodes = []string{
	CodeAPIChanged, CodeContractViolation,
	CodeAuthMissing, CodeAuthExpired, CodeAuthRejected, CodeADCNotFound, CodeADCInvalid, CodeADCUnsupported,
	CodeADCMissingScope, CodeLifecycleInvalid, CodeRevokeFailed, CodeAuthEndpoint,
	CodeQueueTimeout, CodeAcceleratorUnavailable, CodeUnavailable, CodeBootTimeout, CodeSessionReclaimed,
	CodeConnectionLost, CodeLeftoverSession, CodeReleaseFailed,
	CodeWindowExhausted, CodeWindowCooldown, CodeBalanceLow, CodeAuthTooShort, CodeShutdownRequeue, CodeBusy,
	CodeRemoteFailed, CodeHashMismatch, CodeAudioMissing, CodeSettingsInvalid, CodeWorkerMissing, CodeCancelled,
	CodeUploadFailed, CodeDownloadFailed, CodeLocalWrite, CodeWorkerExited,
	CodeGcloudMissing, CodeGcloudStartFailed, CodeLoginRunning, CodeLoginTimeout, CodeLoginFailed,
	CodeAccountUnavailable,
}

// ErrorParams lists each code's params (for the lead's
// testdata/api/error_codes_backend.json entries; TestErrorParams checks
// that the code sends exactly these).
var ErrorParams = map[string][]string{
	CodeAPIChanged: {"endpoint", "detail"}, CodeContractViolation: {"reason"},
	CodeAuthMissing: {}, CodeAuthExpired: {}, CodeAuthRejected: {"reason"}, CodeADCNotFound: {"path"},
	CodeADCInvalid: {"field"}, CodeADCUnsupported: {"type"}, CodeADCMissingScope: {"scopes"},
	CodeLifecycleInvalid: {"value"}, CodeRevokeFailed: {"status"}, CodeAuthEndpoint: {"status"},
	CodeQueueTimeout: {"accelerator", "minutes"}, CodeAcceleratorUnavailable: {"accelerator"}, CodeUnavailable: {"endpoint"},
	CodeBootTimeout: {"minutes"}, CodeSessionReclaimed: {}, CodeConnectionLost: {}, CodeLeftoverSession: {"accelerator"},
	CodeReleaseFailed:   {"accelerator"},
	CodeWindowExhausted: {"remaining_min", "need_min"}, CodeWindowCooldown: {"minutes"}, CodeBalanceLow: {"need"},
	CodeAuthTooShort: {"need_min"}, CodeShutdownRequeue: {}, CodeBusy: {}, CodeRemoteFailed: {"reason"},
	CodeHashMismatch: {"file"}, CodeAudioMissing: {"file"}, CodeSettingsInvalid: {"field"}, CodeWorkerMissing: {"file"},
	CodeCancelled: {}, CodeUploadFailed: {"file"}, CodeDownloadFailed: {"file"}, CodeLocalWrite: {"file"},
	CodeWorkerExited:  {},
	CodeGcloudMissing: {}, CodeGcloudStartFailed: {}, CodeLoginRunning: {}, CodeLoginTimeout: {"minutes"},
	CodeLoginFailed: {"exit_code"}, CodeAccountUnavailable: {"reason"},
}
