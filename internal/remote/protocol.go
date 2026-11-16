// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package remote

// The protocol between LectureFlow (the controller) and lf-agent on an SSH
// server (ssh-mvp; plan §5). Everything runs over SSH channels of one
// connection, nothing listens on a port:
//
//   - Control channel: "lf-agent serve". stdin carries requests, stdout
//     events, one JSON object per line. A request line is at most
//     MaxRequestLine bytes, an event line at most MaxEventLine; objects are
//     flat (scalars only, Params of "run" excepted) and strict: unknown or
//     duplicate keys, wrong types or values out of range break the
//     contract (the controller hands the lecture back, the agent answers
//     "fail" bad_request).
//   - File channels: "lf-agent put|get|stat ...", one exec channel per
//     file; put/stat answer with one event line, get streams the file.
//   - Set-up: "lf-agent ensure" reads a KitSpec from stdin and reports
//     "step" events; "probe", "version", "reap", "uninstall" answer with
//     one event line.
//
// Requests: hello {version, retain_s} → hello; ping → pong; job {job,
// audio_kind, audio_size, audio_sha256} → ok {have, resume}; run {job,
// engine, kit, params} → progress / status ... then done or error; cancel
// {job} → ok (the run ends with error "cancelled"); delete {job} → ok;
// sample {on} → ok, then machine once and sample every 2 s.
//
// The agent ends a session (kills its workers, keeps their job
// directories for retain_s and exits) when no request arrives for
// HeartbeatTimeout or stdin closes; the controller pings every
// HeartbeatEvery.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits of the protocol.
const (
	MaxEventLine   = 4096
	MaxRequestLine = 64 << 10
	MaxKitSpec     = 4 << 20
	// MaxResultBytes is the largest result.json the controller takes.
	MaxResultBytes = 32 << 20
	// MaxProbeString is the longest string of a probe (display only).
	MaxProbeString = 128
)

// Timing of the protocol.
const (
	HeartbeatEvery   = 5 * time.Second
	HeartbeatTimeout = 30 * time.Second
	// CancelWithin: a cancelled run returns within this.
	CancelWithin = 3 * time.Second
	// KillGrace: SIGTERM, then SIGKILL after this.
	KillGrace = 3 * time.Second
	// DefaultRetain: how long a job directory survives a lost connection
	// (user decision 2026-10-27, plan §10 item 5: at most 2 hours).
	DefaultRetain = 2 * time.Hour
	MaxRetain     = 2 * time.Hour
)

// JobIDPattern is a remote job id (an asr job id: a ULID).
var JobIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// KitPattern is a kit hash (SHA-256, hex).
var KitPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SHA256Pattern is a SHA-256 in hex.
var SHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// VersionStringPattern is an application / agent version.
var VersionStringPattern = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}([-+][0-9A-Za-z.-]{1,32})?$`)

// Platforms of a remote (the component manifest's names).
const (
	PlatformLinuxX64    = "linux-x64"
	PlatformDarwinArm64 = "darwin-arm64"
	PlatformWindowsX64  = "windows-x64"
)

// Platforms lists them.
var Platforms = []string{PlatformLinuxX64, PlatformDarwinArm64, PlatformWindowsX64}

// Audio kinds of a job (plan §5: FLAC for CUDA, WAV for MLX).
const (
	AudioWAV  = "wav"
	AudioFLAC = "flac"
)

// File kinds of put / get / stat.
const (
	FileAudio  = "audio"
	FileResult = "result"
)

// Request ops.
const (
	OpHello  = "hello"
	OpPing   = "ping"
	OpJob    = "job"
	OpRun    = "run"
	OpCancel = "cancel"
	OpDelete = "delete"
	OpSample = "sample"
)

// Request is one control-channel request.
type Request struct {
	Op  string `json:"op"`
	Seq int64  `json:"seq"`
	Job string `json:"job,omitempty"`
	// hello
	Version string `json:"version,omitempty"`
	RetainS int    `json:"retain_s,omitempty"`
	// job
	AudioKind   string `json:"audio_kind,omitempty"`
	AudioSize   int64  `json:"audio_size,omitempty"`
	AudioSHA256 string `json:"audio_sha256,omitempty"`
	// run
	Engine string          `json:"engine,omitempty"`
	Kit    string          `json:"kit,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	// sample
	On bool `json:"on,omitempty"`
}

// ParseRequest decodes and checks one request line (the agent's side).
func ParseRequest(line []byte) (Request, error) {
	var r Request
	if len(line) > MaxRequestLine {
		return r, errors.New("request line too long")
	}
	if !utf8.Valid(line) {
		return r, errors.New("request is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("request is not a valid object: %w", err)
	}
	if dec.More() {
		return r, errors.New("trailing data after the request")
	}
	if r.Seq < 0 || r.Seq > 1<<53 {
		return r, errors.New("bad seq")
	}
	needJob := func() error {
		if !JobIDPattern.MatchString(r.Job) {
			return errors.New("bad job id")
		}
		return nil
	}
	switch r.Op {
	case OpHello:
		if r.Version != "" && !VersionStringPattern.MatchString(r.Version) {
			return r, errors.New("bad version")
		}
		if r.RetainS < 0 || r.RetainS > int(MaxRetain/time.Second) {
			return r, errors.New("bad retain_s")
		}
	case OpPing:
	case OpJob:
		if err := needJob(); err != nil {
			return r, err
		}
		if r.AudioKind != AudioWAV && r.AudioKind != AudioFLAC {
			return r, errors.New("bad audio_kind")
		}
		if r.AudioSize <= 0 || r.AudioSize > MaxAudioBytes {
			return r, errors.New("bad audio_size")
		}
		if !SHA256Pattern.MatchString(r.AudioSHA256) {
			return r, errors.New("bad audio_sha256")
		}
	case OpRun:
		if err := needJob(); err != nil {
			return r, err
		}
		if r.Engine != EngineMLX && r.Engine != EngineFasterWhisper {
			return r, errors.New("bad engine")
		}
		if !KitPattern.MatchString(r.Kit) {
			return r, errors.New("bad kit")
		}
		if _, err := CheckParams(r.Engine, r.Params); err != nil {
			return r, err
		}
	case OpCancel, OpDelete:
		if err := needJob(); err != nil {
			return r, err
		}
	case OpSample:
	default:
		return r, errors.New("unknown op")
	}
	return r, nil
}

// MaxAudioBytes bounds an uploaded audio file (a 6-hour 16 kHz mono WAV is
// about 0.7 GB).
const MaxAudioBytes = 4 << 30

// Event types (agent → controller).
const (
	EvHello    = "hello"
	EvOK       = "ok"
	EvFail     = "fail"
	EvPong     = "pong"
	EvProgress = "progress"
	EvStatus   = "status"
	EvDone     = "done"
	EvError    = "error"
	EvStat     = "stat"
	EvStep     = "step"
	EvReady    = "ready"
	EvProbe    = "probe"
	EvVersion  = "version"
	// EventMachine, EventSample (sample.go)
)

// Agent error codes (fail.code, error.code).
const (
	AgentBadRequest       = "bad_request"
	AgentUnknownJob       = "unknown_job"
	AgentBusy             = "busy"
	AgentKitMissing       = "kit_missing"
	AgentAudioMissing     = "audio_missing"
	AgentAudioMismatch    = "audio_hash_mismatch"
	AgentNoSpace          = "no_space"
	AgentUnsafePath       = "unsafe_path"
	AgentWorkerStart      = "worker_start_failed"
	AgentWorkerInput      = "worker_input_error"
	AgentWorkerFailed     = "worker_failed"
	AgentDeviceUnusable   = "device_unavailable"
	AgentOutOfMemory      = "out_of_memory"
	AgentCancelled        = "cancelled"
	AgentInterrupted      = "interrupted"
	AgentResultMissing    = "result_missing"
	AgentResultTooLarge   = "result_too_large"
	AgentDownloadFailed   = "download_failed"
	AgentHashMismatch     = "hash_mismatch"
	AgentUnpackFailed     = "unpack_failed"
	AgentVenvFailed       = "venv_failed"
	AgentPackagesFailed   = "packages_failed"
	AgentImportsFailed    = "imports_failed"
	AgentVersionMismatch  = "version_mismatch"
	AgentUnsupported      = "unsupported_platform"
	AgentInternal         = "internal"
	AgentIncompleteUpload = "incomplete"
)

// AgentCodes lists them (an event with another code breaks the contract).
var AgentCodes = []string{
	AgentBadRequest, AgentUnknownJob, AgentBusy, AgentKitMissing, AgentAudioMissing, AgentAudioMismatch,
	AgentNoSpace, AgentUnsafePath, AgentWorkerStart, AgentWorkerInput, AgentWorkerFailed, AgentDeviceUnusable,
	AgentOutOfMemory, AgentCancelled, AgentInterrupted, AgentResultMissing, AgentResultTooLarge,
	AgentDownloadFailed, AgentHashMismatch, AgentUnpackFailed, AgentVenvFailed, AgentPackagesFailed,
	AgentImportsFailed, AgentVersionMismatch, AgentUnsupported, AgentInternal, AgentIncompleteUpload,
}

// Worker statuses (status.status; the workers' STATUS lines).
var WorkerStatuses = []string{"downloading_model", "preparing_model", "compiling_model", "loading_model"}

// Set-up steps (step.step).
var SetupSteps = []string{"python", "venv", "packages", "imports", "worker", "model", "done"}

// AgentEvent is one parsed event line. Unset numbers are 0 unless noted.
type AgentEvent struct {
	Type string
	Seq  int64
	Job  string
	// hello
	Version, Platform string
	Jobs              []string // kept job directories
	// ok (job): bytes already there; whether a checkpoint exists
	Have   int64
	Resume bool
	// fail / error
	Code      string
	Retryable bool
	Exit      int
	FreeMB    int64 // no_space; -1 unknown
	NeedMB    int64
	// progress, step
	Fraction float64
	// status
	Status string
	// done: result.json's size and hash; the worker's peak memory (-1 unknown)
	Bytes  int64
	SHA256 string
	PeakMB int64
	// stat
	Complete bool
	// step
	Step string
	// ready
	Kit string
	// probe
	Probe Probe
	// machine, sample: the raw numbers / strings (sample.go keys)
	Machine map[string]any
	Sample  map[string]any
}

// Probe is what "lf-agent probe" reports (display only, plus the
// numbers the controller decides on).
type Probe struct {
	Platform    string
	OSVersion   string
	Hostname    string
	CPUs        int64
	MemTotalMB  int64
	DiskFreeMB  int64 // on the agent's data directory
	GPUName     string
	VRAMTotalMB int64 // -1 unknown; unified memory on Apple Silicon
	Driver      string
	CUDA        string
	Metal       bool
	Kits        []string // installed kit hashes
}

// value kinds of the flat object parser.
type scalar struct {
	kind string // string | number | bool
	str  string
	num  float64
	b    bool
}

type keySpec struct {
	required, optional []string
}

var eventKeys = map[string]keySpec{
	EvHello:    {required: []string{"type", "seq", "version", "platform"}, optional: []string{"jobs", "ts"}},
	EvOK:       {required: []string{"type"}, optional: []string{"seq", "job", "have", "resume", "bytes", "ts"}},
	EvFail:     {required: []string{"type", "code"}, optional: []string{"seq", "job", "free_mb", "need_mb", "ts"}},
	EvPong:     {required: []string{"type", "seq"}, optional: []string{"ts"}},
	EvProgress: {required: []string{"type", "job", "fraction"}, optional: []string{"ts"}},
	EvStatus:   {required: []string{"type", "job", "status"}, optional: []string{"ts"}},
	EvDone:     {required: []string{"type", "job", "bytes", "sha256"}, optional: []string{"peak_mb", "ts"}},
	EvError:    {required: []string{"type", "job", "code", "retryable"}, optional: []string{"exit", "free_mb", "need_mb", "ts"}},
	EvStat:     {required: []string{"type", "have", "complete"}, optional: []string{"ts"}},
	EvStep:     {required: []string{"type", "step", "fraction"}, optional: []string{"ts"}},
	EvReady:    {required: []string{"type", "kit"}, optional: []string{"ts"}},
	EvProbe: {required: []string{"type", "platform", "cpus", "mem_total_mb", "disk_free_mb", "vram_total_mb"},
		optional: []string{"os_version", "hostname", "gpu_name", "driver", "cuda", "metal", "kits", "ts"}},
	EvVersion:    {required: []string{"type", "version", "platform"}, optional: []string{"sha256", "ts"}},
	EventMachine: {required: append([]string{"type"}, MachineEventKeys...), optional: []string{"ts"}},
	EventSample:  {required: append([]string{"type"}, SampleEventKeys...), optional: []string{"ts"}},
}

// ErrContract is wrapped by every violation of the event contract.
var ErrContract = errors.New("remote agent broke the protocol")

func violation(reason string) error { return fmt.Errorf("%w: %s", ErrContract, reason) }

// ParseEvent checks and decodes one event line (the controller's side).
// The reason of a violation never quotes the line.
func ParseEvent(line []byte) (AgentEvent, error) {
	var ev AgentEvent
	if len(line) > MaxEventLine {
		return ev, violation("line too long")
	}
	if !utf8.Valid(line) {
		return ev, violation("line is not UTF-8")
	}
	vals, err := flatObject(line)
	if err != nil {
		return ev, err
	}
	t, ok := vals["type"]
	if !ok || t.kind != "string" {
		return ev, violation("no type")
	}
	spec, ok := eventKeys[t.str]
	if !ok {
		return ev, violation("unknown event type")
	}
	for _, k := range spec.required {
		if _, ok := vals[k]; !ok {
			return ev, violation("missing key " + k)
		}
	}
	for k := range vals {
		if !slices.Contains(spec.required, k) && !slices.Contains(spec.optional, k) {
			return ev, violation("unknown key")
		}
	}
	ev.Type = t.str
	p := parser{vals: vals}
	if _, ok := vals["ts"]; ok {
		p.num("ts", 0, 1e12) // remote clocks are not trusted: checked, not kept
	}
	switch ev.Type {
	case EvHello:
		ev.Seq = p.int("seq", 0, 1<<53)
		ev.Version = p.str("version", VersionStringPattern)
		ev.Platform = p.enum("platform", Platforms)
		if _, ok := vals["jobs"]; ok {
			ev.Jobs = p.list("jobs", JobIDPattern, 1000)
		}
	case EvOK:
		ev.Seq = p.optInt("seq", 0, 1<<53, 0)
		ev.Job = p.optStr("job", JobIDPattern)
		ev.Have = p.optInt("have", 0, MaxAudioBytes, 0)
		ev.Resume = p.optBool("resume")
		ev.Bytes = p.optInt("bytes", 0, MaxAudioBytes, 0)
	case EvFail:
		ev.Seq = p.optInt("seq", 0, 1<<53, 0)
		ev.Job = p.optStr("job", JobIDPattern)
		ev.Code = p.enum("code", AgentCodes)
		ev.FreeMB = p.optInt("free_mb", -1, MaxMB, -1)
		ev.NeedMB = p.optInt("need_mb", -1, MaxMB, -1)
	case EvPong:
		ev.Seq = p.int("seq", 0, 1<<53)
	case EvProgress:
		ev.Job = p.str("job", JobIDPattern)
		ev.Fraction = p.num("fraction", 0, 1)
	case EvStatus:
		ev.Job = p.str("job", JobIDPattern)
		ev.Status = p.enum("status", WorkerStatuses)
	case EvDone:
		ev.Job = p.str("job", JobIDPattern)
		ev.Bytes = p.int("bytes", 1, MaxResultBytes)
		ev.SHA256 = p.str("sha256", SHA256Pattern)
		ev.PeakMB = p.optInt("peak_mb", -1, MaxMB, -1)
	case EvError:
		ev.Job = p.str("job", JobIDPattern)
		ev.Code = p.enum("code", AgentCodes)
		ev.Retryable = p.bool("retryable")
		ev.Exit = int(p.optInt("exit", -1, 255, -1))
		ev.FreeMB = p.optInt("free_mb", -1, MaxMB, -1)
		ev.NeedMB = p.optInt("need_mb", -1, MaxMB, -1)
	case EvStat:
		ev.Have = p.int("have", 0, MaxAudioBytes)
		ev.Complete = p.bool("complete")
	case EvStep:
		ev.Step = p.enum("step", SetupSteps)
		ev.Fraction = p.num("fraction", -1, 1)
	case EvReady:
		ev.Kit = p.str("kit", KitPattern)
	case EvProbe:
		ev.Probe = Probe{
			Platform: p.enum("platform", Platforms), OSVersion: p.optText("os_version"), Hostname: p.optText("hostname"),
			CPUs: p.int("cpus", -1, MaxVCPUs), MemTotalMB: p.int("mem_total_mb", -1, MaxMB),
			DiskFreeMB: p.int("disk_free_mb", -1, MaxMB), GPUName: p.optStr("gpu_name", GPUNamePattern),
			VRAMTotalMB: p.int("vram_total_mb", -1, MaxMB), Driver: p.optStr("driver", DriverPattern),
			CUDA: p.optStr("cuda", VersionPattern), Metal: p.optBool("metal"),
		}
		if _, ok := vals["kits"]; ok {
			ev.Probe.Kits = p.list("kits", KitPattern, 64)
		}
	case EvVersion:
		ev.Version = p.str("version", VersionStringPattern)
		ev.Platform = p.enum("platform", Platforms)
		ev.SHA256 = p.optStr("sha256", SHA256Pattern)
	case EventMachine, EventSample:
		m := map[string]any{}
		keys := MachineEventKeys
		if ev.Type == EventSample {
			keys = SampleEventKeys
		}
		for _, k := range keys {
			m[k] = p.machineValue(k)
		}
		if ev.Type == EventMachine {
			ev.Machine = m
		} else {
			ev.Sample = m
		}
	}
	if p.err != nil {
		return AgentEvent{}, p.err
	}
	return ev, nil
}

// machineValue checks one key of a machine / sample event (sample.go).
func (p *parser) machineValue(k string) any {
	switch k {
	case "gpu_name":
		return p.str(k, GPUNamePattern)
	case "gpu_arch":
		return p.str(k, GPUArchPattern)
	case "driver":
		return p.str(k, DriverPattern)
	case "cuda", "compute_cap":
		return p.str(k, VersionPattern)
	case "storage":
		return p.str(k, StorageValuePattern)
	case "gpu_count":
		return p.int(k, -1, MaxGPUs)
	case "vcpus":
		return p.int(k, -1, MaxVCPUs)
	case "cpu_pct", "gpu_util_pct":
		return p.int(k, -1, MaxPct)
	case "gpu_temp_c":
		return p.int(k, -1, 200)
	case "gpu_power_w":
		return p.num(k, -1, MaxPowerW)
	}
	return p.int(k, -1, MaxMB)
}

// parser reads typed values of a flat object, keeping the first error.
type parser struct {
	vals map[string]scalar
	err  error
}

func (p *parser) fail(k, why string) {
	if p.err == nil {
		p.err = violation(k + ": " + why)
	}
}

func (p *parser) get(k, kind string) (scalar, bool) {
	v, ok := p.vals[k]
	if !ok {
		return v, false
	}
	if v.kind != kind {
		p.fail(k, "wrong type")
		return v, false
	}
	return v, true
}

func (p *parser) num(k string, lo, hi float64) float64 {
	v, ok := p.get(k, "number")
	if !ok {
		if _, present := p.vals[k]; !present {
			p.fail(k, "missing")
		}
		return 0
	}
	if math.IsNaN(v.num) || v.num < lo || v.num > hi {
		p.fail(k, "out of range")
		return 0
	}
	return v.num
}

func (p *parser) int(k string, lo, hi int64) int64 {
	f := p.num(k, float64(lo), float64(hi))
	if f != math.Trunc(f) {
		p.fail(k, "not an integer")
		return 0
	}
	return int64(f)
}

func (p *parser) optInt(k string, lo, hi, def int64) int64 {
	if _, ok := p.vals[k]; !ok {
		return def
	}
	return p.int(k, lo, hi)
}

func (p *parser) bool(k string) bool {
	v, ok := p.get(k, "bool")
	if !ok {
		if _, present := p.vals[k]; !present {
			p.fail(k, "missing")
		}
		return false
	}
	return v.b
}

func (p *parser) optBool(k string) bool {
	if _, ok := p.vals[k]; !ok {
		return false
	}
	return p.bool(k)
}

func (p *parser) str(k string, re *regexp.Regexp) string {
	v, ok := p.get(k, "string")
	if !ok {
		if _, present := p.vals[k]; !present {
			p.fail(k, "missing")
		}
		return ""
	}
	if re != nil && !re.MatchString(v.str) {
		p.fail(k, "bad value")
		return ""
	}
	return v.str
}

func (p *parser) optStr(k string, re *regexp.Regexp) string {
	if _, ok := p.vals[k]; !ok {
		return ""
	}
	return p.str(k, re)
}

// optText is a display-only string: printable, at most MaxProbeString runes.
func (p *parser) optText(k string) string {
	if _, ok := p.vals[k]; !ok {
		return ""
	}
	s := p.str(k, nil)
	return CleanText(s, MaxProbeString)
}

func (p *parser) enum(k string, allowed []string) string {
	s := p.str(k, nil)
	if p.err == nil && !slices.Contains(allowed, s) {
		p.fail(k, "unknown value")
		return ""
	}
	return s
}

// list is a comma-separated list of values matching re ("" = none).
func (p *parser) list(k string, re *regexp.Regexp, max int) []string {
	s := p.str(k, nil)
	if p.err != nil || s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	if len(parts) > max {
		p.fail(k, "too many values")
		return nil
	}
	for _, x := range parts {
		if !re.MatchString(x) {
			p.fail(k, "bad value")
			return nil
		}
	}
	return parts
}

// flatObject parses a JSON object whose values are all scalars, rejecting
// duplicate keys.
func flatObject(line []byte) (map[string]scalar, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, violation("line is not a JSON object")
	}
	vals := map[string]scalar{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, violation("line is not a JSON object")
		}
		key, ok := kt.(string)
		if !ok || len(key) > 64 {
			return nil, violation("bad key")
		}
		if _, dup := vals[key]; dup {
			return nil, violation("duplicate key")
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, violation("line is not a JSON object")
		}
		switch v := vt.(type) {
		case string:
			vals[key] = scalar{kind: "string", str: v}
		case json.Number:
			f, err := v.Float64()
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, violation("bad number")
			}
			vals[key] = scalar{kind: "number", num: f}
		case bool:
			vals[key] = scalar{kind: "bool", b: v}
		default:
			return nil, violation("value is not a scalar")
		}
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') {
		return nil, violation("line is not a JSON object")
	}
	if rest := bytes.TrimSpace(line[dec.InputOffset():]); len(rest) > 0 {
		return nil, violation("trailing data")
	}
	return vals, nil
}

// CleanText keeps the printable characters of s (no control or format
// characters), at most max runes, trimmed.
func CleanText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			break
		}
		if r == utf8.RuneError || r < 0x20 || (r >= 0x7f && r < 0xa0) || isFormatRune(r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// isFormatRune: bidi controls and other invisible format characters. The
// bidi controls are those of Unicode's Bidi_Control property: U+061C
// (ARABIC LETTER MARK), U+200E–U+200F, U+202A–U+202E and U+2066–U+2069
// (within U+2060–U+206F). The text of sentences and words keeps ZWNJ and
// ZWJ (isTextFormatRune); identifier-like strings (CleanText) drop them.
func isFormatRune(r rune) bool {
	switch {
	case r == 0x061c, r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2060 && r <= 0x206f, r == 0xfeff:
		return true
	}
	return false
}

// Encode writes v as one compact JSON line (no HTML escaping) with a
// trailing newline.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
