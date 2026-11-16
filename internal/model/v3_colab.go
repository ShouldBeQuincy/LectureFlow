// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import "lectureflow/internal/remote"

// SPEC-v3 V14: the Colab backend inside the scheduler, the asr stage and the
// HTTP API (v14-colab-integrate; .claude/v3-brief.md §8). The Colab
// package's own kv keys (credential, session, settings, rates) are in
// internal/colab/kv.go.

// kv keys of the scheduler's use of Colab ("1" | "0"; missing or anything
// else = off).
const (
	// KVColabEnabled: lectures may be sent to colab:remote (default off).
	// With it off colab:remote is not in the device table and nothing goes
	// to Colab, not even a lecture the user pinned with "改用 Colab 处理".
	KVColabEnabled = "colab.enabled"
	// KVColabBatteryPrefer is V14.4 "电池模式下优先使用 Colab" (default
	// off): on battery, a queued lecture the power-aware policy keeps off
	// the local devices may go to Colab.
	KVColabBatteryPrefer = "colab.battery_prefer"
)

// Remote steps of one lecture (V14.5 hover card: 等卡 → 上传音频 → 远端转写
// → 下载结果 → 校验); the same values as internal/colab's Step* constants
// (a test keeps them in step).
const (
	RemoteStepWaitAccelerator  = "wait_accelerator"
	RemoteStepUploadAudio      = "upload_audio"
	RemoteStepRemoteTranscribe = "remote_transcribe"
	RemoteStepDownloadResult   = "download_result"
	RemoteStepVerify           = "verify"
)

// RemoteSteps are the five steps in order (RemoteStats.Steps).
var RemoteSteps = []string{RemoteStepWaitAccelerator, RemoteStepUploadAudio, RemoteStepRemoteTranscribe,
	RemoteStepDownloadResult, RemoteStepVerify}

// RemoteStats is job.stats.remote of an asr job running on colab:remote
// (the stage view's stats.remote, V14.5).
type RemoteStats struct {
	Step  string   `json:"step"`  // RemoteStep*
	Steps []string `json:"steps"` // RemoteSteps
	// EtaSec is the estimated time left of the lecture in seconds; -1 =
	// unknown.
	EtaSec int64 `json:"eta_sec"`
	// Accelerator is the runtime's GPU, e.g. "T4".
	Accelerator string `json:"accelerator"`
	// Stage is the remote worker's stage during remote_transcribe (check,
	// install, download_model, load_model, transcribe, write).
	Stage string `json:"stage,omitempty"`
	// WaitingFor is WaitRemoteQueue while Colab has no accelerator for us.
	WaitingFor string `json:"waiting_for,omitempty"`
	// Reconnect (v3.2.x, lead freeze 2026-11-11): the runtime does not
	// answer and the lecture is trying again in place (colab.Event's
	// reconnect: attempt, of, next_at); absent otherwise, cleared once the
	// runtime answers again.
	Reconnect *remote.Reconnect `json:"reconnect,omitempty"`
}

// ParseKVFlag reads a "1" | "0" kv flag: "1" and "true" are on, anything
// else (missing included) is off.
func ParseKVFlag(v string, ok bool) bool { return ok && (v == "1" || v == "true") }

// KVFlag is the stored form of a flag.
func KVFlag(on bool) string {
	if on {
		return "1"
	}
	return "0"
}
