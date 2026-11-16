// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// colab-obs-core: the runtime's own readings — the machine / sample events
// (plan A) and the runtime's resources endpoint (plan B).

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/remote"
)

func TestParseMachineAndSampleEvents(t *testing.T) {
	m := `{"type":"machine","ts":1.5,"gpu_name":"Tesla T4","gpu_arch":"Turing","compute_cap":"7.5","driver":"580.82.07",` +
		`"cuda":"13.0","gpu_count":1,"vram_total_mb":15360,"vcpus":2,"mem_total_mb":12975,"storage":"memory"}`
	ev, err := parseEvent(m)
	if err != nil || ev.Type != remote.EventMachine || ev.Machine.GPUName != "Tesla T4" || ev.Machine.VCPUs != 2 {
		t.Fatalf("%+v %v", ev, err)
	}
	s := `{"type":"sample","ts":2,"cpu_pct":37.5,"mem_total_mb":12975,"mem_available_mb":6100,"ramdisk_mb":3400,` +
		`"data_mb":95,"data_disk_mb":-1,"whisper_mb":3000,"gpu_util_pct":88,"vram_used_mb":4100,"vram_reserved_mb":448,` +
		`"vram_whisper_mb":3650,"gpu_temp_c":61,"gpu_power_w":65.2}`
	ev, err = parseEvent(s)
	if err != nil || ev.Type != remote.EventSample || ev.Sample.WhisperMB != 3000 || ev.Sample.CPUPct != 37.5 {
		t.Fatalf("%+v %v", ev, err)
	}
	for name, line := range map[string]string{
		"a comma in the name (text?)": strings.Replace(m, `"Tesla T4"`, `"Welcome, students"`, 1),
		"a field missing":             strings.Replace(m, `,"storage":"memory"`, ``, 1),
		"an extra field":              strings.Replace(s, `"gpu_power_w":65.2`, `"gpu_power_w":65.2,"text":"x"`, 1),
		"out of range":                strings.Replace(s, `"cpu_pct":37.5`, `"cpu_pct":250`, 1),
		"a fraction of a MB":          strings.Replace(s, `"data_mb":95`, `"data_mb":95.5`, 1),
		"a string for a number":       strings.Replace(s, `"whisper_mb":3000`, `"whisper_mb":"3000"`, 1),
	} {
		if _, err := parseEvent(line); Code(err) != CodeContractViolation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r, _ := codedParam(mustErr(parseEvent(strings.Replace(m, `"Tesla T4"`, `"Welcome, students"`, 1))), "reason"); r != "bad value of gpu_name" {
		t.Errorf("reason %q", r)
	}
}

func mustErr(_ remoteEvent, err error) error { return err }

// Plan A on the replay: the readings are kept (not progress, not a stage),
// Status shows the machine and the memory bar, the release forgets them.
func TestReplaySampleEvents(t *testing.T) {
	e := newEnv(t, "sample_events.json").withCredential(t).startup(t)
	ctx := context.Background()
	var evs eventLog
	if _, err := e.b.Transcribe(ctx, e.job(t), evs.add); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	var steps []string
	for _, ev := range evs.all() {
		if len(steps) == 0 || steps[len(steps)-1] != ev.Step {
			steps = append(steps, ev.Step)
		}
		if ev.Stage != "" && !slices.Contains(remoteStages, ev.Stage) {
			t.Errorf("stage %q", ev.Stage)
		}
	}
	if !slices.Equal(steps, []string{StepWaitAccelerator, StepUploadAudio, StepRemoteTranscribe, StepDownloadResult, StepVerify}) {
		t.Errorf("steps %v", steps)
	}
	st := e.b.Status(ctx)
	m, u := st.Machine, st.Usage
	if m == nil || m.Source != remote.SourceNvidiaSMI || m.GPUName != "Tesla T4" || m.Requested != "T4" || m.Storage != remote.StorageDisk ||
		*m.VRAMTotalMB != 15360 || *m.VCPUs != 2 || m.CUDA != "13.0" {
		t.Fatalf("machine %+v", m)
	}
	if u == nil || u.Source != remote.SourceWorker || *u.CPUPct != 41 || *u.GPUUtilPct != 88 || *u.VRAMWhisperMB != 3650 ||
		u.DataDiskMB == nil || *u.DataDiskMB != 2 || len(u.CPUHistory) != remote.HistoryLen {
		t.Fatalf("usage %+v", u)
	}
	want := []remote.RAMSegment{{Kind: remote.SegData, MB: 0}, {Kind: remote.SegWhisper, MB: 3412},
		{Kind: remote.SegSystem, MB: 12975 - 6100 - 3412}, {Kind: remote.SegFree, MB: 6100}}
	if !slices.Equal(u.RAM, want) {
		t.Errorf("segments %+v", u.RAM)
	}
	js, _ := json.Marshal(st)
	for _, w := range []string{`"machine":{"source":"nvidia-smi","gpu_name":"Tesla T4"`, `"usage":{"at":`, `"cpu_history":[`} {
		if !strings.Contains(string(js), w) {
			t.Errorf("JSON lacks %s: %s", w, js)
		}
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.b.Status(ctx); st.Machine != nil || st.Usage != nil || st.Account.ReadAt == nil {
		t.Errorf("after the release: machine %+v usage %+v account %+v", st.Machine, st.Usage, st.Account)
	}
	e.rs.finish()
	e.checkClean(t)
}

// A malformed reading is a contract violation like any other line.
func TestReplaySampleViolation(t *testing.T) {
	e := newEnv(t, "sample_violation.json").withCredential(t).startup(t)
	ctx := context.Background()
	_, err := e.b.Transcribe(ctx, e.job(t), nil)
	if Code(err) != CodeContractViolation || !IsFallback(err) || strings.Contains(err.Error(), "Welcome") {
		t.Fatalf("err %v", err)
	}
	if r, _ := codedParam(err, "reason"); r != "bad value of gpu_name" {
		t.Errorf("reason %q", r)
	}
	e.b.handbacksSettled() // v3.2.x: the files and the runtime go in the background
	e.rs.finish()
	e.checkClean(t, err)
}

// Plan B: the runtime's resources endpoint only while a front end watches,
// the session runs and nothing came from the worker for 6 s; a failure is
// only logged.
func TestResourcesPollFillsIn(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	var viewers atomic.Int32
	e.b.SetViewers(func() int { return int(viewers.Load()) })
	e.f.resources = map[string]any{
		"memory": map[string]any{"totalBytes": 13605818368, "freeBytes": "12584570880"},
		"disks":  []any{map[string]any{"filesystem": map[string]any{"label": "kernel", "totalBytes": 1, "usedBytes": 1}}},
		"gpus": []any{map[string]any{"name": "Tesla T4", "memoryTotalBytes": 16106127360, "memoryUsedBytes": 1183842304,
			"gpuUtilization": 0.87, "memoryUtilization": 0.1, "everUsed": true}},
	}
	reads := func() int { return e.f.count(func() int { return e.f.resourceReads }) }
	// No session: nothing.
	viewers.Store(1)
	e.b.pollResources(ctx)
	if reads() != 0 {
		t.Fatal("polled without a session")
	}
	a := e.lecture(t, "a")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	// Nobody watches: nothing.
	viewers.Store(0)
	e.b.pollResources(ctx)
	if reads() != 0 {
		t.Fatal("polled without a viewer")
	}
	viewers.Store(1)
	e.b.pollResources(ctx)
	st := e.b.Status(ctx)
	if reads() != 1 || st.Usage == nil || st.Usage.Source != remote.SourceColabResources || st.Usage.CPUPct != nil ||
		*st.Usage.GPUUtilPct != 87 || *st.Usage.VRAMUsedMB != 1129 {
		t.Fatalf("usage %+v (%d reads)", st.Usage, reads())
	}
	if !slices.Equal(st.Usage.RAM, []remote.RAMSegment{{Kind: remote.SegUsed, MB: 12975 - 12001}, {Kind: remote.SegFree, MB: 12001}}) {
		t.Errorf("segments %+v", st.Usage.RAM)
	}
	if m := st.Machine; m == nil || m.Source != remote.SourceColabResources || m.GPUName != "Tesla T4" || *m.VRAMTotalMB != 15360 || *m.MemTotalMB != 12975 {
		t.Errorf("machine %+v", m)
	}
	// The worker's readings arrive: plan B pauses for 6 s, its machine wins.
	e.b.mu.Lock()
	s := e.b.sess
	e.b.mu.Unlock()
	mr, _ := remote.ReadMachine(func(k string) (any, bool) {
		v := map[string]any{"gpu_name": "Tesla T4", "gpu_arch": "Turing", "compute_cap": "7.5", "driver": "580.82.07", "cuda": "13.0",
			"gpu_count": 1.0, "vram_total_mb": 15360.0, "vcpus": 2.0, "mem_total_mb": 12975.0, "storage": "disk"}[k]
		return v, true
	})
	e.b.noteReading(s, remoteEvent{Type: remote.EventMachine, Machine: mr})
	e.b.noteReading(s, remoteEvent{Type: remote.EventSample, Sample: remote.SampleReading{CPUPct: 20, MemTotalMB: 12975, MemAvailableMB: 9000,
		RAMDiskMB: -1, DataDiskMB: 1, WhisperMB: 3000, GPUUtilPct: 80, VRAMUsedMB: 4000, VRAMReservedMB: 448, VRAMWhisperMB: 3600,
		GPUTempC: 60, GPUPowerW: 50}})
	e.b.pollResources(ctx)
	if reads() != 1 {
		t.Errorf("polled while the worker reports")
	}
	e.clk.Advance(7 * time.Second)
	e.f.mu.Lock()
	e.f.resources = nil // the endpoint fails: logged once, nothing else
	e.f.mu.Unlock()
	e.b.pollResources(ctx)
	e.b.pollResources(ctx)
	st = e.b.Status(ctx)
	if reads() != 3 || st.LastError != nil || st.Machine.Source != remote.SourceNvidiaSMI || st.Usage.Source != remote.SourceWorker {
		t.Errorf("after failures: %d reads, %+v", reads(), st)
	}
	if n := strings.Count(e.log.String(), "reading the runtime's resources failed"); n != 1 {
		t.Errorf("logged %d times", n)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
	if st := e.b.Status(ctx); st.Usage != nil || st.Machine != nil {
		t.Errorf("readings kept after the release")
	}
	e.checkSecrets(t)
}

// Security review 3 L5: the resources answer is read up to 64 KiB (a longer
// one is no reading this time, logged) and only its first 64 gpus entries.
func TestResourcesAnswerBounded(t *testing.T) {
	ctx := context.Background()
	e := newFakeEnv(t, t4())
	e.b.SetViewers(func() int { return 1 })
	a := e.lecture(t, "a")
	e.f.hold("a")
	da := startLecture(e, ctx, a)
	e.transcribingNow(t)
	gpu := map[string]any{"name": "Tesla T4", "memoryTotalBytes": 16106127360, "memoryUsedBytes": 0, "gpuUtilization": 0.5}
	many := make([]any, 100)
	for i := range many {
		many[i] = gpu
	}
	e.f.mu.Lock()
	e.f.resources = map[string]any{"memory": map[string]any{"totalBytes": 13605818368, "freeBytes": 12584570880}, "gpus": many}
	e.f.mu.Unlock()
	e.b.pollResources(ctx)
	if m := e.b.Status(ctx).Machine; m == nil || *m.GPUCount != 64 {
		t.Fatalf("machine %+v", m)
	}
	// Too large: no reading, logged once, nothing else.
	e.b.samples.Reset()
	e.f.mu.Lock()
	e.f.resources = map[string]any{"memory": map[string]any{"totalBytes": 1, "freeBytes": 1}, "padding": strings.Repeat("x", resourcesMaxBytes)}
	e.f.mu.Unlock()
	e.clk.Advance(10 * time.Second)
	e.b.pollResources(ctx)
	e.b.pollResources(ctx)
	st := e.b.Status(ctx)
	if st.Usage != nil || st.Machine != nil || st.LastError != nil {
		t.Errorf("a too large answer was used: %+v", st)
	}
	if n := strings.Count(e.log.String(), "reading the runtime's resources failed"); n != 1 {
		t.Errorf("logged %d times", n)
	}
	e.f.release("a")
	if err := waitErr(t, da); err != nil {
		t.Fatal(err)
	}
	_ = e.b.QueueDrained(ctx)
}
