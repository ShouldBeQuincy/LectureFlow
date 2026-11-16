// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Memory mode (SPEC-ext "Colab 内存运行"; on by default since 2026-10-27):
// replay tests of the probe, the RAM disk paths, the fallbacks, the order
// "verify the result → delete the remote files → release", that with the
// switch off nothing changes at all, and the switch itself.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// remoteWatch is the backend's HTTP transport in these tests: it records
// what job.json said, the remote deletions, and checks that result.json
// was verified and written locally before the first deletion and that
// every deletion came before the release. Its dial (the kernel channels)
// records the same for the cell that removes the job directory (3.2.0:
// the last kernel channel of a successful lecture).
type remoteWatch struct {
	t      *testing.T
	outDir string
	want   string // result.json's expected content

	mu            sync.Mutex
	jobJSON       map[string]any
	deletes       []string
	verifiedFirst bool
	deletesAtRel  int
	released      bool
	// dials: per kernel channel opened, whether result.json was written
	// locally and whether the runtime had been released by then
	dials []dialAt
}

type dialAt struct{ verified, released bool }

func (w *remoteWatch) written() bool {
	got, err := os.ReadFile(filepath.Join(w.outDir, "result.json"))
	return err == nil && sameResult(got, w.want)
}

func (w *remoteWatch) dial(base Dialer) Dialer {
	return func(ctx context.Context, rawURL string, header http.Header) (MessageConn, error) {
		w.mu.Lock()
		w.dials = append(w.dials, dialAt{verified: w.written(), released: w.released})
		w.mu.Unlock()
		return base(ctx, rawURL, header)
	}
}

func (w *remoteWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	switch {
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/job.json"):
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var put struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &put)
		raw, _ := base64.StdEncoding.DecodeString(put.Content)
		w.jobJSON = map[string]any{}
		if err := json.Unmarshal(raw, &w.jobJSON); err != nil {
			w.t.Errorf("job.json is not JSON: %v", err)
		}
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/contents/"):
		if len(w.deletes) == 0 {
			w.verifiedFirst = w.written()
		}
		w.deletes = append(w.deletes, r.URL.Path[strings.Index(r.URL.Path, "/api/contents/")+len("/api/contents/"):])
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/tun/m/unassign/"):
		w.released = true
		w.deletesAtRel = len(w.deletes)
	}
	w.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

// newDiskKV is the kv of the replay tests (newEnv): memory mode switched
// off. The samples from before memory mode, and those extending them, were
// recorded on the disk without the probe; memory mode is on by default, so
// their kv says "off" as the kv of a user who switched it off would. The
// memory tests set the mode they test, or remove the key (newMemoryEnv).
//
// The samples ask for a T4 (recorded before the paid tier's default became
// the L4, colab-batch): the kv holds a user's explicit choice of the T4.
func newDiskKV() *MemKV {
	kv := NewMemKV()
	kv.m[KVMemoryMode] = MemoryModeOff
	set := DefaultSettings()
	set.Accelerator, set.AcceleratorChosen = "T4", true
	raw, _ := json.Marshal(set)
	kv.m[KVSettings] = string(raw)
	return kv
}

// memoryUnset makes newMemoryEnv remove kv colab.memory_mode: never set,
// as in a new installation or for a user who never touched the switch.
const memoryUnset = "(unset)"

func newMemoryEnv(t *testing.T, sample, mode string, mods ...func(*Options)) (*testEnv, *remoteWatch) {
	t.Helper()
	w := &remoteWatch{t: t}
	mods = append([]func(*Options){func(o *Options) {
		o.HTTPClient = &http.Client{Transport: w}
		o.Dial = w.dial(DialWebSocket(nil))
	}}, mods...)
	e := newEnv(t, sample, mods...).withCredential(t).startup(t)
	w.outDir, w.want = e.dir, e.rs.vars["result_json"]
	if mode == memoryUnset {
		e.kv.mu.Lock()
		delete(e.kv.m, KVMemoryMode)
		e.kv.mu.Unlock()
	} else if err := e.kv.SetKV(context.Background(), KVMemoryMode, mode); err != nil {
		t.Fatal(err)
	}
	return e, w
}

// runAndRelease transcribes the test lecture and releases the runtime as
// the scheduler does when the queue is empty.
func runAndRelease(t *testing.T, e *testEnv) string {
	t.Helper()
	ctx := context.Background()
	path, err := e.b.Transcribe(ctx, e.job(t), nil)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got, _ := os.ReadFile(path); !sameResult(got, e.rs.vars["result_json"]) {
		t.Fatal("result.json differs from the download")
	}
	if st := e.b.Status(ctx); st.State != StateRunning {
		t.Fatalf("state after the lecture %s", st.State)
	}
	if err := e.b.QueueDrained(ctx); err != nil {
		t.Fatalf("QueueDrained: %v", err)
	}
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseQueueEmpty {
		t.Fatalf("status after release %+v", st)
	}
	e.rs.finish()
	e.checkClean(t)
	return path
}

// checkOrder: the job directory was removed (3.2.0: by one cell in the
// lecture's kernel, the last kernel channel; the sample checks the path
// under base) after result.json was verified and written, and before the
// release; no file went through the Contents API one by one.
func (w *remoteWatch) checkOrder(t *testing.T, base string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.dials) == 0 {
		t.Fatal("no kernel channel opened")
	}
	if last := w.dials[len(w.dials)-1]; !last.verified || last.released {
		t.Errorf("the removal cell ran before result.json was written (%v) or after the release (%v)", last.verified, last.released)
	}
	for _, d := range w.dials[:len(w.dials)-1] {
		if d.verified {
			t.Error("a kernel channel after result.json was written, besides the removal cell")
		}
	}
	if len(w.deletes) != 0 || !w.released {
		t.Errorf("deletes %v (released %v)", w.deletes, w.released)
	}
	_ = base
}

// With the switch off ("off", as the settings page writes it, in any case)
// the backend does exactly what it did before memory mode existed: the
// replay of borrow_run_release.json (no probe cell, /content/lectureflow)
// passes, and job.json has no memory field.
func TestMemoryModeOffIsUnchanged(t *testing.T) {
	for _, mode := range []string{MemoryModeOff, " OFF\n", "Off"} {
		t.Run("mode="+mode, func(t *testing.T) {
			e, w := newMemoryEnv(t, "borrow_run_release.json", mode)
			runAndRelease(t, e)
			w.checkOrder(t, "content/lectureflow")
			if _, ok := w.jobJSON["memory"]; ok || len(w.jobJSON) != 8 {
				t.Errorf("job.json %v", w.jobJSON)
			}
			if strings.Contains(e.log.String(), "memory mode") {
				t.Error("memory mode logged with the switch off")
			}
			if m := e.b.lastMetrics; m == nil || m.memory != nil || m.storage() != "disk" {
				t.Errorf("metrics %+v", m)
			}
			if !strings.Contains(e.log.String(), "storage=disk ") {
				t.Errorf("log: %s", e.log.String())
			}
		})
	}
}

// remoteJob without memory marshals to the same bytes as before the field
// existed.
func TestRemoteJobWithoutMemoryField(t *testing.T) {
	b, _ := json.Marshal(remoteJob{Audio: "audio.flac", AudioSHA256: strings.Repeat("a", 64), Model: "large-v3",
		ComputeType: "float16", Language: "en", InitialPrompt: "", Result: "result.json", Requirements: RemoteRequirements})
	want := `{"audio":"audio.flac","audio_sha256":"` + strings.Repeat("a", 64) + `","model":"large-v3","compute_type":"float16",` +
		`"language":"en","initial_prompt":"","result":"result.json","requirements":["faster-whisper==1.2.1","av==15.1.0","ctranslate2==4.8.3"]}`
	if string(b) != want {
		t.Errorf("job.json\n%s\nwant\n%s", b, want)
	}
}

// Memory mode on /dev/shm: the probe runs before any upload; scripts,
// audio, job.json (memory: true), the result and the cleanup are on the RAM
// disk; the result is verified before the RAM disk's files are deleted,
// and the runtime is released after that. This is the default: a kv that
// never had the key (a new installation, a user who never touched the
// switch), the "" db.Store writes for a deleted key, "auto" (the switch
// turned on again) and values the app never writes all mean on.
func TestMemoryModeRunsOnTheRAMDisk(t *testing.T) {
	for _, mode := range []string{memoryUnset, "", MemoryModeAuto, "on", "AUTO", "auto-ish"} {
		t.Run("mode="+mode, func(t *testing.T) {
			e, w := newMemoryEnv(t, "memory_run_release.json", mode)
			runAndRelease(t, e)
			w.checkOrder(t, "dev/shm/lectureflow")
			if w.jobJSON["memory"] != true {
				t.Errorf("job.json %v", w.jobJSON)
			}
			logs := e.log.String()
			for _, s := range []string{"memory mode:", "ram_disk=dev_shm", "need_mb=11798", "storage=memory", "mem_peak_mb=7310", "the machine is still Google's"} {
				if !strings.Contains(logs, s) {
					t.Errorf("log lacks %q:\n%s", s, logs)
				}
			}
			m := e.b.lastMetrics
			if m == nil || m.memory == nil || m.memory.Base != "/dev/shm/lectureflow" || m.memPeakMB != 7310 || m.memTotalMB != 12985 {
				t.Errorf("metrics %+v", m)
			}
		})
	}
}

// /dev/shm too small: the probe mounted its own tmpfs at /mnt/lectureflow.
func TestMemoryModeOwnTmpfs(t *testing.T) {
	e, w := newMemoryEnv(t, "memory_tmpfs.json", memoryUnset, func(o *Options) { o.MemoryNoShm = true })
	runAndRelease(t, e)
	w.checkOrder(t, "mnt/lectureflow")
	if !strings.Contains(e.log.String(), "ram_disk=tmpfs") || w.jobJSON["memory"] != true {
		t.Errorf("log %s, job.json %v", e.log.String(), w.jobJSON)
	}
}

// Not enough memory (here forced by the experiment knob): the lecture runs
// on the disk exactly as with the switch off, and the log says why; with
// the default (never set) as with the switch turned on again.
func TestMemoryModeFallsBackToTheDisk(t *testing.T) {
	for _, mode := range []string{memoryUnset, MemoryModeAuto} {
		t.Run("mode="+mode, func(t *testing.T) {
			e, w := newMemoryEnv(t, "memory_fallback.json", mode, func(o *Options) { o.MemoryExtraMB = 1000000 })
			runAndRelease(t, e)
			w.checkOrder(t, "content/lectureflow")
			if _, ok := w.jobJSON["memory"]; ok {
				t.Errorf("job.json %v", w.jobJSON)
			}
			logs := e.log.String()
			if !strings.Contains(logs, "memory mode not possible") || !strings.Contains(logs, "reason=low_memory") ||
				!strings.Contains(logs, "storage=disk_fallback") {
				t.Errorf("log: %s", logs)
			}
		})
	}
}

// A probe cell that fails without output: disk mode (probe_failed).
func TestMemoryProbeFailedUsesTheDisk(t *testing.T) {
	e, w := newMemoryEnv(t, "memory_probe_failed.json", memoryUnset)
	runAndRelease(t, e)
	w.checkOrder(t, "content/lectureflow")
	if !strings.Contains(e.log.String(), "reason=probe_failed") {
		t.Errorf("log: %s", e.log.String())
	}
}

// A probe answer outside the contract: nothing is uploaded, the lecture
// goes back to the local devices, the runtime is released.
func TestMemoryProbeViolation(t *testing.T) {
	e, w := newMemoryEnv(t, "memory_probe_violation.json", memoryUnset)
	ctx := context.Background()
	_, err := e.b.Transcribe(ctx, e.job(t), nil)
	if Code(err) != CodeContractViolation || !IsFallback(err) {
		t.Fatalf("err %v", err)
	}
	e.b.handbacksSettled() // v3.2.x: the runtime goes in the background
	if st := e.b.Status(ctx); st.State != StateReleased || st.LastRelease.Reason != ReleaseFailed {
		t.Errorf("status %+v", st)
	}
	if w.jobJSON != nil || len(w.deletes) != 0 {
		t.Errorf("uploaded %v, deleted %v", w.jobJSON, w.deletes)
	}
	e.rs.finish()
	e.checkClean(t, err)
}

func TestParseMemoryProbe(t *testing.T) {
	line := func(fields string) string {
		return `{"type":"memory","ts":1793260799.0,` + fields + `}` + "\n"
	}
	ok := `"mode":"memory","base":"/dev/shm/lectureflow","reason":"ok","mem_total_mb":12985,"mem_available_mb":11800,"need_mb":7108,"free_mb":6492`
	d, err := parseMemoryProbe(line(ok))
	if err != nil || !d.memory() || d.Base != "/dev/shm/lectureflow" || d.NeedMB != 7108 || d.FreeMB != 6492 || d.ramDisk() != "dev_shm" {
		t.Fatalf("%+v %v", d, err)
	}
	d, err = parseMemoryProbe(line(`"mode":"disk","base":"","reason":"swap_enabled","mem_total_mb":12985,"mem_available_mb":11800,"need_mb":7108,"free_mb":0`))
	if err != nil || d.memory() || d.Reason != "swap_enabled" {
		t.Fatalf("%+v %v", d, err)
	}
	bad := map[string]string{
		"no newline":         strings.TrimSuffix(line(ok), "\n"),
		"two lines":          line(ok) + line(ok),
		"other base":         line(strings.Replace(ok, "/dev/shm/lectureflow", "/content/lectureflow", 1)),
		"base with dots":     line(strings.Replace(ok, "/dev/shm/lectureflow", "/dev/shm/../etc", 1)),
		"memory without ok":  line(strings.Replace(ok, `"ok"`, `"low_memory"`, 1)),
		"disk with base":     line(strings.Replace(strings.Replace(ok, `"memory"`, `"disk"`, 1), `"ok"`, `"low_memory"`, 1)),
		"disk with ok":       line(`"mode":"disk","base":"","reason":"ok","mem_total_mb":1,"mem_available_mb":1,"need_mb":1,"free_mb":0`),
		"unknown mode":       line(strings.Replace(ok, `"memory"`, `"ram"`, 1)),
		"unknown reason":     line(`"mode":"disk","base":"","reason":"because","mem_total_mb":1,"mem_available_mb":1,"need_mb":1,"free_mb":0`),
		"fraction":           line(strings.Replace(ok, "7108", "7108.5", 1)),
		"negative":           line(strings.Replace(ok, "6492", "-1", 1)),
		"string number":      line(strings.Replace(ok, "6492", `"6492"`, 1)),
		"extra field":        line(ok + `,"text":"Zebrafish"`),
		"missing field":      line(strings.Replace(ok, `,"free_mb":6492`, "", 1)),
		"not a memory event": `{"type":"stage","ts":1.0,"stage":"check"}` + "\n",
		"not JSON":           "Zebrafish\n",
	}
	for name, out := range bad {
		if _, err := parseMemoryProbe(out); Code(err) != CodeContractViolation {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "Zebrafish") {
			t.Errorf("%s: the error quotes the output", name)
		}
	}
}

// The done event's optional memory fields.
func TestDoneEventMemoryFields(t *testing.T) {
	base := `{"type":"done","ts":1.0,"result":"result.json","sha256":"` + strings.Repeat("0", 64) +
		`","bytes":10,"audio_s":1.0,"load_s":1.0,"transcribe_s":1.0,"sentences":1`
	ev, err := parseEvent(base + `}`)
	if err != nil || ev.MemPeakMB != -1 || ev.MemTotalMB != -1 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = parseEvent(base + `,"mem_peak_mb":7310,"mem_total_mb":12985}`)
	if err != nil || ev.MemPeakMB != 7310 || ev.MemTotalMB != 12985 {
		t.Fatalf("%+v %v", ev, err)
	}
	for _, v := range []string{`,"mem_peak_mb":1.5}`, `,"mem_peak_mb":-2}`, `,"mem_total_mb":"big"}`, `,"mem_used_mb":1}`} {
		if _, err := parseEvent(base + v); Code(err) != CodeContractViolation {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func TestProbeArgsAndCode(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "audio.wav")
	if err := os.WriteFile(wav, make([]byte, 12345), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{KV: NewMemKV()})
	if err != nil {
		t.Fatal(err)
	}
	set := DefaultSettings()
	args := b.probeArgs(set, Job{AudioPath: wav})
	want := []string{"--memory-probe", "--model", "large-v3", "--audio-bytes", "12345",
		"--requirements", "faster-whisper==1.2.1,av==15.1.0,ctranslate2==4.8.3"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args %v", args)
	}
	b.o.MemoryExtraMB, b.o.MemoryNoShm = 500, true
	args = b.probeArgs(set, Job{AudioPath: wav})
	if strings.Join(args[len(want):], " ") != "--extra-mb 500 --no-shm" {
		t.Errorf("args %v", args)
	}
	src := []byte("print('a \"quoted\" <tag> & line')\nimport sys\n")
	code := probeCode(src, args)
	q, _ := json.Marshal(string(src))
	if !strings.Contains(code, "[sys.executable, '-I', '-c', "+string(q)+"] + [") || !strings.Contains(code, `"--no-shm"]`) ||
		!strings.Contains(code, "stderr=subprocess.DEVNULL") {
		t.Errorf("code:\n%s", code)
	}
}

// errKV fails every read and write.
type errKV struct{}

func (errKV) GetKV(context.Context, string) (string, bool, error) {
	return "", false, errors.New("disk I/O error")
}
func (errKV) SetKV(context.Context, string, string) error { return errors.New("disk I/O error") }

// The switch: on unless the user switched it off (the default since
// 2026-10-27); the settings page writes "off" and "auto", and a kv that
// never had the key stays without it until the user touches the switch. A
// kv that cannot be read runs the lecture on the disk (it cannot tell
// whether the user switched memory mode off) and the settings view gets
// the error.
func TestMemoryModeSwitch(t *testing.T) {
	ctx := context.Background()
	kv := NewMemKV()
	b, err := New(Options{KV: kv})
	if err != nil {
		t.Fatal(err)
	}
	check := func(what string, wantOn bool) {
		t.Helper()
		on, err := b.MemoryMode(ctx)
		if err != nil || on != wantOn {
			t.Errorf("%s: MemoryMode %v %v, want %v", what, on, err, wantOn)
		}
		if mode := b.memoryMode(ctx); (mode == MemoryModeAuto) != wantOn {
			t.Errorf("%s: mode %s", what, mode)
		}
	}
	check("never set", true)
	if _, ok := kv.Snapshot()[KVMemoryMode]; ok {
		t.Error("reading the switch wrote it")
	}
	for v, want := range map[string]bool{"": true, "auto": true, " auto\n": true, "AUTO": true, "on": true, "yes": true,
		"off": false, " off\n": false, "OFF": false} {
		_ = kv.SetKV(ctx, KVMemoryMode, v)
		check(fmt.Sprintf("kv %q", v), want)
	}
	for _, on := range []bool{false, true, false} {
		if err := b.SetMemoryMode(ctx, on); err != nil {
			t.Fatal(err)
		}
		want := MemoryModeOff
		if on {
			want = MemoryModeAuto
		}
		if v, _, _ := kv.GetKV(ctx, KVMemoryMode); v != want {
			t.Errorf("SetMemoryMode(%v) wrote %q", on, v)
		}
		check(fmt.Sprintf("SetMemoryMode(%v)", on), on)
	}
	bad, err := New(Options{KV: errKV{}})
	if err != nil {
		t.Fatal(err)
	}
	if on, err := bad.MemoryMode(ctx); err == nil || on {
		t.Errorf("unreadable kv: MemoryMode %v %v", on, err)
	}
	if mode := bad.memoryMode(ctx); mode != MemoryModeOff {
		t.Errorf("unreadable kv: mode %s", mode)
	}
	if err := bad.SetMemoryMode(ctx, false); err == nil {
		t.Error("SetMemoryMode on a failing kv: no error")
	}
}
