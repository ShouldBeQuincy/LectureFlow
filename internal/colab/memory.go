// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Memory mode (SPEC-ext "Colab 内存运行"; on by default since the user's
// decision of 2026-10-27 after the experiment, a switch on the settings
// page turns it off): the recording, the result, the model and the extra
// Python packages stay on a RAM disk of the runtime instead of its
// persistent disk. Before anything
// is uploaded, worker/colab_remote.py runs as a memory probe (its source in
// the cell, `python -I -c`, so nothing is written before the decision): it
// reads /proc/meminfo and /proc/swaps, estimates model x 2 + environment +
// recording x 2 + 3 GB (the transcription process) + 2 GB (lead's ruling
// after the Colab experiment; SPEC-ext's formula left the process out), and answers with one "memory" event: memory (the
// RAM disk to use: /dev/shm/lectureflow, or a tmpfs it mounted at
// /mnt/lectureflow) or disk (and why; the lecture then runs exactly as
// without memory mode). The decision is made for every lecture; what an
// earlier lecture of the runtime put on the RAM disk is reused.
//
// Memory mode keeps the data off the runtime's persistent disk and leaves
// less behind. The machine is still Google's: it does not make anything
// invisible to Google (SPEC-ext; log lines and documents say so too).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Values of kv colab.memory_mode.
const (
	MemoryModeOff  = "off"
	MemoryModeAuto = "auto"
)

// Memory probe tables (worker/colab_remote.py MEMORY_BASES, MEMORY_MODES,
// MEMORY_REASONS; test_colab_remote.py compares them with this file).
var (
	memoryBases   = []string{"/dev/shm/lectureflow", "/mnt/lectureflow"}
	memoryModes   = []string{"memory", "disk"}
	memoryReasons = []string{"ok", "low_memory", "swap_enabled", "no_proc", "tmpfs_failed", "error"}
)

// memoryReasonProbeFailed: the probe printed nothing (the cell failed
// before it could answer); the lecture uses the disk. Go only.
const memoryReasonProbeFailed = "probe_failed"

// memoryDecision is the probe's answer.
type memoryDecision struct {
	Mode           string // "memory" or "disk"
	Base           string // the RAM disk (memoryBases) in memory mode, else ""
	Reason         string // memoryReasons, or memoryReasonProbeFailed
	MemTotalMB     int64
	MemAvailableMB int64
	NeedMB         int64
	FreeMB         int64
}

func (d memoryDecision) memory() bool { return d.Mode == "memory" }

// ramDisk names the RAM disk for logs: dev_shm or tmpfs.
func (d memoryDecision) ramDisk() string {
	if d.Base == memoryBases[0] {
		return "dev_shm"
	}
	return "tmpfs"
}

// MemoryMode reports whether memory mode is on (the settings page's
// switch): true unless the user switched it off (kv colab.memory_mode =
// off). Absent and unknown values follow the default, on. "On" means "try":
// the probe still falls back to the disk for a lecture that does not fit.
func (b *Backend) MemoryMode(ctx context.Context) (bool, error) {
	v, ok, err := getKV(ctx, b.o.KV, KVMemoryMode)
	if err != nil {
		return false, err
	}
	return !ok || !strings.EqualFold(strings.TrimSpace(v), MemoryModeOff), nil
}

// SetMemoryMode stores the user's choice (MemoryModeAuto or
// MemoryModeOff); the next lecture reads it.
func (b *Backend) SetMemoryMode(ctx context.Context, on bool) error {
	v := MemoryModeOff
	if on {
		v = MemoryModeAuto
	}
	return b.o.KV.SetKV(ctx, KVMemoryMode, v)
}

// memoryMode is the mode of a lecture: MemoryModeAuto unless the user
// switched memory mode off. A kv that cannot be read means MemoryModeOff
// (the disk, as before memory mode existed): it cannot tell whether the
// user switched it off, and an explicit "off" must never be overridden.
func (b *Backend) memoryMode(ctx context.Context) string {
	if on, err := b.MemoryMode(ctx); err != nil || !on {
		return MemoryModeOff
	}
	return MemoryModeAuto
}

// probeCode is the cell of the memory probe: colab_remote.py's source run
// with `python -I -c` (isolated: no user site, no PYTHON* variables), its
// standard output copied to the cell's, its standard error discarded.
func probeCode(src []byte, args []string) string {
	q := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return fmt.Sprintf(`import subprocess, sys
_r = subprocess.run([sys.executable, '-I', '-c', %s] + %s, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL, timeout=300)
sys.stdout.write(_r.stdout.decode('utf-8', 'replace'))
sys.stdout.flush()
`, q(string(src)), q(args))
}

// probeArgs are the probe's arguments: the model, the size of the lecture's
// audio file (the WAV, an upper bound of the upload; x 2 in the estimate
// also covers the decoded samples), the pins, and the experiment knobs.
func (b *Backend) probeArgs(set Settings, job Job) []string {
	var size int64
	if fi, err := os.Stat(job.AudioPath); err == nil {
		size = fi.Size()
	}
	args := []string{"--memory-probe", "--model", set.Model, "--audio-bytes", strconv.FormatInt(size, 10),
		"--requirements", strings.Join(RemoteRequirements, ",")}
	if b.o.MemoryExtraMB > 0 {
		args = append(args, "--extra-mb", strconv.Itoa(b.o.MemoryExtraMB))
	}
	if b.o.MemoryNoShm {
		args = append(args, "--no-shm")
	}
	return args
}

// probeMemory runs the memory probe in kernel kid and logs the decision;
// its connections take their proxy token from client (execCell). A probe
// that prints nothing means disk mode (memoryReasonProbeFailed); anything
// but exactly one valid memory event is CodeContractViolation.
func (b *Backend) probeMemory(ctx context.Context, client clientSource, kid string, set Settings, job Job, src []byte) (memoryDecision, error) {
	var buf strings.Builder
	_, err := execCell(ctx, client, kid, probeCode(src, b.probeArgs(set, job)), func(s string) error {
		if buf.Len()+len(s) > 2*maxEventLine {
			return violation("memory probe output too long")
		}
		buf.WriteString(s)
		return nil
	}, b.newReconnector(nil), nil)
	if err != nil {
		return memoryDecision{}, err
	}
	var d memoryDecision
	if buf.Len() == 0 {
		d = memoryDecision{Mode: "disk", Reason: memoryReasonProbeFailed}
	} else if d, err = parseMemoryProbe(buf.String()); err != nil {
		return memoryDecision{}, err
	}
	if d.memory() {
		b.log.Info("colab: memory mode: the recording, the result, the model and the Python packages stay in the runtime's memory, off its persistent disk (the machine is still Google's)",
			"ram_disk", d.ramDisk(), "mem_total_mb", d.MemTotalMB, "mem_available_mb", d.MemAvailableMB, "need_mb", d.NeedMB, "free_mb", d.FreeMB)
	} else {
		b.log.Info("colab: memory mode not possible for this lecture; using the runtime's disk",
			"reason", d.Reason, "mem_total_mb", d.MemTotalMB, "mem_available_mb", d.MemAvailableMB, "need_mb", d.NeedMB)
	}
	return d, nil
}

// parseMemoryProbe checks the probe's whole output: one line, a memory
// event (events.go parseEvent checks its fields and values).
func parseMemoryProbe(out string) (memoryDecision, error) {
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		return memoryDecision{}, violation("memory probe output is not one line")
	}
	ev, err := parseEvent(strings.TrimRight(out[:len(out)-1], "\r"))
	if err != nil {
		return memoryDecision{}, err
	}
	if ev.Type != "memory" {
		return memoryDecision{}, violation("memory probe output is not a memory event")
	}
	return ev.Memory, nil
}

// checkMemoryEvent validates the decision's enumerations and consistency.
func checkMemoryEvent(d memoryDecision) error {
	switch {
	case !slices.Contains(memoryModes, d.Mode):
		return violation("bad value of mode")
	case !slices.Contains(memoryReasons, d.Reason):
		return violation("bad value of reason")
	case d.Mode == "memory" && (!slices.Contains(memoryBases, d.Base) || d.Reason != "ok"):
		return violation("bad value of base")
	case d.Mode == "disk" && (d.Base != "" || d.Reason == "ok"):
		return violation("bad value of base")
	}
	return nil
}
