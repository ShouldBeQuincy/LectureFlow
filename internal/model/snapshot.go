// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package model

import (
	"encoding/json"
	"strings"
)

// Model configuration snapshots of v1.1 A4. FROZEN by the lead.
//
// job.config_snapshot holds, per lecture stage:
//   - summary, repair, translate: LLMSnapshot
//   - asr: ASRSnapshot
//   - prep, export: "{}"
//
// Snapshots are written when a lecture is launched (A4.1) and replaced only
// when the user picks "使用当前设置" in the rerun decision (A4.3). A stage
// reads its own job's snapshot, never the current settings. Model and
// provider changes never make a stage stale.

// EmptySnapshot is the snapshot of prep/export, drafts, and course/material jobs.
const EmptySnapshot = "{}"

// LLMSnapshot is the snapshot of an LLM stage. Params are resolved
// (temperature always present, window present for translate).
type LLMSnapshot struct {
	ProviderID   string     `json:"provider_id"`
	ProviderName string     `json:"provider_name"` // display only; not compared
	Model        string     `json:"model"`
	Params       RoleParams `json:"params"`
	// PromptVersion is the prompt version of the stage (v2 G5.5); 0 (absent,
	// snapshots written before v2) means version 1. Compared by SameSnapshot.
	PromptVersion int `json:"prompt_version,omitempty"`
	// Source (v3.2.x P1-b, lead freeze): SnapshotSourceManual when the user
	// named provider, model and params for one retry instead of taking the
	// role's setting; "" otherwise. Display only ("临时指定"); not compared.
	Source string `json:"source,omitempty"`
}

// SnapshotSourceManual is LLMSnapshot.Source of a snapshot the user wrote
// by hand for one retry (model choice manual).
const SnapshotSourceManual = "manual"

// SnapshotPromptVersion returns the prompt version of an LLM snapshot
// (1 when it has none).
func SnapshotPromptVersion(s LLMSnapshot) int {
	if s.PromptVersion <= 0 {
		return 1
	}
	return s.PromptVersion
}

// ASRSnapshot is the snapshot of the asr stage.
//
// SPEC-v2 I6 (phase 3b freeze): a snapshot records {device_id, backend,
// model, precision, language}; Auto marks one written in auto mode, whose
// device fields the scheduler rewrites every time the job starts
// (state.BindASRDevice, lead ruling 1). Snapshots written by v1.1/v2 have
// only model, compute_type, device and language: they are never rewritten
// in the database, every reader goes through NormalizeASRSnapshot. The
// field order keeps the bytes of a v1.1-style snapshot unchanged (the new
// fields are omitted when empty). ComputeType and Device are the v1.1
// fields: ComputeType is the configured value (e.g. "auto"); the per-run
// resolution is not part of the snapshot.
type ASRSnapshot struct {
	DeviceID    string `json:"device_id,omitempty"` // model.Device* (I6)
	Backend     string `json:"backend,omitempty"`   // model.Backend* (I6)
	Model       string `json:"model"`
	Precision   string `json:"precision,omitempty"`    // I6; ValidPrecision(Backend, ·)
	ComputeType string `json:"compute_type,omitempty"` // v1.1 only (read, never written by 3b code)
	Device      string `json:"device,omitempty"`       // v1.1 only: cuda | cpu
	Language    string `json:"language"`
	Auto        bool   `json:"auto,omitempty"`
	// Parts (power management, v2-step20 freeze): the parts of a run that
	// was resumed from a checkpoint, in audio order, each with its device,
	// model and precision; written by the asr stage when the run finished
	// with two parts or more (state.RecordASRParts). The top-level device
	// fields are the last part's (the device bound at the last start).
	// Omitted when empty: every snapshot written before stays byte for byte
	// the same. Not compared by SameSnapshot (A4.3 compares the top-level
	// effective configuration only).
	Parts []ASRPart `json:"parts,omitempty"`
}

// NormalizeASRSnapshot returns s in the I6 form. A v1.1 snapshot (no
// device_id) becomes device cuda:0 (device cuda or empty) or fw:cpu (device
// cpu), backend faster-whisper, precision = its compute_type verbatim; a
// snapshot with a device_id but no backend gets DeviceBackend(device_id).
// The v1.1 fields are kept as they are (the asr input hash of
// hash_version 2 reads compute_type); Auto is unchanged.
func NormalizeASRSnapshot(s ASRSnapshot) ASRSnapshot {
	if s.DeviceID == "" {
		if s.Device == "cpu" {
			s.DeviceID = DeviceFWCPU
		} else {
			s.DeviceID = DeviceCUDA0
		}
		if s.Backend == "" {
			s.Backend = BackendFasterWhisper
		}
		if s.Precision == "" {
			s.Precision = s.ComputeType
		}
	}
	if s.Backend == "" {
		s.Backend = DeviceBackend(s.DeviceID)
	}
	return s
}

// sameASR compares the effective configuration (lead ruling 2): device,
// backend, model, precision and language of the normalized snapshots; the
// auto flag and the v1.1 fields are not compared, so a v1.1 snapshot equals
// the cuda:0 row its settings were migrated to.
func sameASR(a, b ASRSnapshot) bool {
	a, b = NormalizeASRSnapshot(a), NormalizeASRSnapshot(b)
	return a.DeviceID == b.DeviceID && a.Backend == b.Backend && a.Model == b.Model &&
		a.Precision == b.Precision && a.Language == b.Language
}

// PinASRSnapshot is the asr snapshot "沿用原模型" keeps (A4.3, lead ruling
// 2): an auto snapshot is fixed to the device it last ran on (auto = false,
// every other field as stored). "沿用原模型" keeps the configuration, never
// the parts of a run that already happened (power management, backend Gate
// round 1, fix 3): Parts are dropped, of a fixed snapshot too. Anything
// else — a fixed or v1.1 snapshot without parts, an empty or unparsable
// one — is returned unchanged, byte for byte.
func PinASRSnapshot(raw string) string {
	s, ok := ParseASRSnapshot(raw)
	if !ok || (!s.Auto && len(s.Parts) == 0) {
		return raw
	}
	s.Auto, s.Parts = false, nil
	return MarshalSnapshot(s)
}

// SnapshotStages are the lecture stages whose snapshot is not empty.
var SnapshotStages = []string{StageASR, StageSummary, StageRepair, StageTranslate}

// IsEmptySnapshot reports whether a raw snapshot is empty ("", "{}" or "null").
func IsEmptySnapshot(raw string) bool {
	t := strings.TrimSpace(raw)
	return t == "" || t == "{}" || t == "null"
}

// ParseLLMSnapshot decodes an LLM stage snapshot. ok is false when raw is
// empty, invalid, or has no provider_id/model.
func ParseLLMSnapshot(raw string) (s LLMSnapshot, ok bool) {
	if IsEmptySnapshot(raw) {
		return s, false
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return LLMSnapshot{}, false
	}
	return s, s.ProviderID != "" && s.Model != ""
}

// ParseASRSnapshot decodes the asr snapshot. ok is false when raw is empty,
// invalid, or has no model.
func ParseASRSnapshot(raw string) (s ASRSnapshot, ok bool) {
	if IsEmptySnapshot(raw) {
		return s, false
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return ASRSnapshot{}, false
	}
	return s, s.Model != ""
}

// MarshalSnapshot encodes a snapshot value (LLMSnapshot, ASRSnapshot) as
// stored in job.config_snapshot. nil encodes as EmptySnapshot.
func MarshalSnapshot(v any) string {
	if v == nil {
		return EmptySnapshot
	}
	b, err := json.Marshal(v)
	if err != nil {
		return EmptySnapshot
	}
	return string(b)
}

// SameSnapshot compares two raw snapshots of the same stage for the rerun
// decision (A4.3): LLM snapshots by provider_id, model, params and prompt
// version (v2 G5.5; provider_name is display only); ASR snapshots by their
// effective configuration (sameASR: device, backend, model, precision,
// language after NormalizeASRSnapshot; v2 I6, lead ruling 2); otherwise
// both must be empty. An unparsable non-empty snapshot never matches.
func SameSnapshot(stage, a, b string) bool {
	switch stage {
	case StageSummary, StageRepair, StageTranslate:
		sa, okA := ParseLLMSnapshot(a)
		sb, okB := ParseLLMSnapshot(b)
		if !okA || !okB {
			return IsEmptySnapshot(a) && IsEmptySnapshot(b)
		}
		return sa.ProviderID == sb.ProviderID && sa.Model == sb.Model && sameParams(sa.Params, sb.Params) &&
			SnapshotPromptVersion(sa) == SnapshotPromptVersion(sb)
	case StageASR:
		sa, okA := ParseASRSnapshot(a)
		sb, okB := ParseASRSnapshot(b)
		if !okA || !okB {
			return IsEmptySnapshot(a) && IsEmptySnapshot(b)
		}
		return sameASR(sa, sb)
	}
	return IsEmptySnapshot(a) == IsEmptySnapshot(b)
}

// sameParams compares resolved params. Thinking is compared by its
// effective level (ThinkingLevel: absent, "" and "off" are the same), so a
// snapshot written before the option existed equals a current setting with
// thinking off, and changing the level makes the rerun decision ask (A4.3).
// Thinking is never part of an input hash: it changes how a stage runs,
// not what it reads.
func sameParams(a, b RoleParams) bool {
	eqF := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	eqI := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eqF(a.Temperature, b.Temperature) && eqI(a.Window, b.Window) && ThinkingLevel(a) == ThinkingLevel(b) &&
		eqI(a.MaxTokens, b.MaxTokens) // Batch is not compared (see RoleParams)
}

// SnapshotWindow returns the translate context window x of a translate
// snapshot, or DefaultTranslateWindow when it is missing.
func SnapshotWindow(raw string) int {
	if s, ok := ParseLLMSnapshot(raw); ok && s.Params.Window != nil && *s.Params.Window >= 0 {
		return *s.Params.Window
	}
	return DefaultTranslateWindow
}
