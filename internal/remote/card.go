// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package remote holds what every remote transcription device shares: the
// resource card the resource bar draws (Colab today, SSH servers next) and
// the machine / sample readings a remote worker reports
// (.claude/research/colab-telemetry.md, remote-ssh-plan.md §6).
//
// The types in this file and sample.go are frozen by the lead
// (2026-10-27): fields may be added, never renamed or removed. A leaf
// package: it imports nothing of LectureFlow's, so colab, resources and
// the SSH backend can all use it.
package remote

import "time"

// Card kinds.
const (
	KindColab = "colab"
	KindSSH   = "ssh"
)

// Card is one remote device in the resource bar (resources.Snapshot.Remotes).
type Card struct {
	ID    string `json:"id"`    // the device id: "colab:remote", "ssh:<slug>"
	Kind  string `json:"kind"`  // KindColab | KindSSH
	Label string `json:"label"` // the device's display name
	// State is the device's own state enum (colab.Status.State for Colab).
	State string `json:"state"`

	// Machine and Usage are present while a session is up and the remote
	// has reported them; absent otherwise (the bar then shows the
	// device's inactive view).
	Machine *Machine `json:"machine,omitempty"`
	Usage   *Usage   `json:"usage,omitempty"`

	Jobs          []CardJob `json:"jobs,omitempty"` // lectures running on it now
	Slots         int       `json:"slots"`          // lectures it may run at once
	Queued        int       `json:"queued"`         // lectures waiting for it
	UploadedAhead int       `json:"uploaded_ahead"` // of those, audio already on it

	LastError *CardError `json:"last_error,omitempty"`

	// Exactly one of these, by Kind: the device's own details (for Colab
	// the account, window, CU and lock state; see internal/colab).
	Colab any      `json:"colab,omitempty"`
	SSH   *SSHInfo `json:"ssh,omitempty"`
}

// CardJob is a lecture running on a remote device.
type CardJob struct {
	LectureID    string   `json:"lecture_id"`
	LectureTitle string   `json:"lecture_title,omitempty"` // as the device cards show it
	JobID        string   `json:"job_id"`                  // the job id (a ULID)
	Stage        string   `json:"stage"`                   // the remote stage (upload, install, …, transcribe)
	Progress     *float64 `json:"progress,omitempty"`      // 0–1
	ETASec       *int     `json:"eta_sec,omitempty"`
	Split        int      `json:"split,omitempty"` // speed mode: parts the lecture is cut into (0 = whole)
	// Reconnect: the device does not answer and the lecture is trying
	// again in place (v3.2.x); absent otherwise.
	Reconnect *Reconnect `json:"reconnect,omitempty"`
}

// Reconnect tells that a lecture on a remote device waits for the device to
// answer again: the attempt under way (from 1), how many there are before
// the lecture is handed back, and when the device is probed next.
type Reconnect struct {
	Attempt int       `json:"attempt"`
	Of      int       `json:"of"`
	NextAt  time.Time `json:"next_at"`
}

// CardError is the device's last error, as an error code (§9.6).
type CardError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
	At     time.Time      `json:"at"`
}

// SSHInfo is an SSH server's own part of the card.
type SSHInfo struct {
	LatencyMS    *int       `json:"latency_ms,omitempty"`
	AgentVersion string     `json:"agent_version,omitempty"`
	LastSeen     *time.Time `json:"last_seen,omitempty"`
	Storage      string     `json:"storage,omitempty"` // StorageMemory | StorageDisk
}

// Where a device keeps its environment, model and job files.
const (
	StorageMemory       = "memory"
	StorageDisk         = "disk"
	StorageDiskFallback = "disk_fallback"
)

// Machine is what the remote reports about itself, once per session. The
// card's identity comes from here (nvidia-smi on the remote), never from
// the requested accelerator or a name in the settings.
type Machine struct {
	Source      string `json:"source"`   // SourceNvidiaSMI | SourceSystemProfiler | SourceColabResources
	GPUName     string `json:"gpu_name"` // as the remote reports it ("Tesla T4")
	GPUArch     string `json:"gpu_arch,omitempty"`
	ComputeCap  string `json:"compute_cap,omitempty"`
	Driver      string `json:"driver,omitempty"`
	CUDA        string `json:"cuda,omitempty"`
	GPUCount    *int   `json:"gpu_count"`
	VRAMTotalMB *int   `json:"vram_total_mb"`
	VCPUs       *int   `json:"vcpus"`
	MemTotalMB  *int   `json:"mem_total_mb"`
	Storage     string `json:"storage,omitempty"`
	// Requested is the accelerator asked for (Colab: "L4"), a label only.
	Requested string `json:"requested,omitempty"`
}

// Machine sources.
const (
	SourceNvidiaSMI      = "nvidia-smi"
	SourceSystemProfiler = "system_profiler"
	SourceColabResources = "colab_resources"
	SourceWorker         = "worker" // Usage.Source: the remote worker's sample events
)

// RAM segment kinds, left to right in the resource bar. The segments of
// one Usage add up to the machine's memory; SegFree is what is left.
const (
	SegRAMDisk = "ramdisk" // the memory disk minus job files: environment, model, scripts
	SegData    = "data"    // job files: uploaded audio (also uploaded ahead), job.json, results, logs
	SegWhisper = "whisper" // the worker and its children, anonymous memory (RssAnon)
	SegSystem  = "system"  // everything else in use
	SegUsed    = "used"    // only total-and-free readings (Colab's resources endpoint): used, unsplit
	SegFree    = "free"    // MemAvailable
)

// RAMSegment is one coloured part of the memory bar.
type RAMSegment struct {
	Kind string `json:"kind"`
	MB   int    `json:"mb"`
}

// HistoryLen is how many points a history holds: 60 s, one every 2 s.
const (
	HistoryLen  = 30
	HistoryStep = 2 * time.Second
)

// Usage is the latest reading plus its history. Times are when this
// machine received the reading (remote clocks are not trusted).
type Usage struct {
	At     time.Time `json:"at"`
	AgeSec int       `json:"age_sec"`
	Source string    `json:"source"` // SourceWorker | SourceColabResources

	CPUPct     *int         `json:"cpu_pct"`
	RAM        []RAMSegment `json:"ram"`
	DataDiskMB *int         `json:"data_disk_mb,omitempty"` // job files on disk (disk storage)

	GPUUtilPct     *int     `json:"gpu_util_pct"`
	VRAMUsedMB     *int     `json:"vram_used_mb"`
	VRAMReservedMB *int     `json:"vram_reserved_mb"`
	VRAMWhisperMB  *int     `json:"vram_whisper_mb"`
	GPUTempC       *int     `json:"gpu_temp_c"`
	GPUPowerW      *float64 `json:"gpu_power_w"`

	// HistoryLen points, oldest first, one per HistoryStep; null where
	// there was no reading (the time axis is not compressed).
	CPUHistory  []*int `json:"cpu_history"`
	GPUHistory  []*int `json:"gpu_history"`
	VRAMHistory []*int `json:"vram_history"`
}
