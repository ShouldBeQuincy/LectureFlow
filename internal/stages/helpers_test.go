// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// ---------- fake Python worker ----------
//
// Worker tests set config.python to this test binary and LF_FAKE_WORKER to a
// mode; the binary then behaves like worker/*.py (SPEC §6.0) instead of
// running the tests. No Python, GPU or network is involved.

const fakeWorkerEnv = "LF_FAKE_WORKER"

func TestMain(m *testing.M) {
	// A test may set both fakes: worker calls are recognised by --in.
	if mode := os.Getenv(fakeWorkerEnv); mode != "" && slices.Contains(os.Args, "--in") {
		os.Exit(fakeWorker(mode))
	}
	if mode := os.Getenv(fakeFFmpegEnv); mode != "" {
		os.Exit(fakeFFmpeg(mode))
	}
	if mode := os.Getenv(fakeWorkerEnv); mode != "" {
		os.Exit(fakeWorker(mode))
	}
	os.Exit(m.Run())
}

// longChineseStderr is > 2 KB of multi-byte text ending in a known line.
func longChineseStderr() string {
	return strings.Repeat("转码过程中出现了一个错误，", 120) + "\n最后一行：磁盘已满\n"
}

func fakeWorker(mode string) int {
	in, out := "", ""
	for i, a := range os.Args {
		if a == "--out" && i+1 < len(os.Args) {
			out = os.Args[i+1]
		}
		if a == "--in" && i+1 < len(os.Args) {
			in = os.Args[i+1]
		}
	}
	if !filepath.IsAbs(in) || !filepath.IsAbs(out) {
		fmt.Fprintf(os.Stderr, "ERROR: --in %q / --out %q must be absolute paths\n", in, out)
		return 2
	}
	jb, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 2
	}
	var job map[string]any
	_ = json.Unmarshal(jb, &job)
	logFakeJob(job, in, out)
	for _, k := range []string{"audio", "pdf"} {
		if p, ok := job[k].(string); ok && !filepath.IsAbs(p) {
			fmt.Fprintf(os.Stderr, "ERROR: job.%s %q is not absolute\n", k, p)
			return 2
		}
	}
	writeResult := func() int {
		src := os.Getenv("LF_FAKE_RESULT")
		b, err := os.ReadFile(src)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		return 0
	}
	runOK := func() int {
		fmt.Println("PROGRESS 0.10")
		fmt.Println("model large-v3 loaded in 0.1s")
		fmt.Println("PROGRESS 0.50")
		if rc := writeResult(); rc != 0 {
			return rc
		}
		fmt.Println("PROGRESS 1.00")
		return 0
	}
	switch mode {
	case "ok":
		return runOK()
	case "download":
		fmt.Println("STATUS downloading_model")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat("go.flag"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Println("PROGRESS 0.50")
		if rc := writeResult(); rc != 0 {
			return rc
		}
		fmt.Println("PROGRESS 1.00")
		return 0
	case "exit2":
		fmt.Fprintln(os.Stderr, "Traceback: ...")
		fmt.Fprintln(os.Stderr, "ERROR: audio file cannot be decoded")
		return 2
	case "exit1":
		fmt.Fprintln(os.Stderr, "ERROR: CUDA out of memory")
		return 1
	case "noout":
		return 0
	case "env": // pyenv_test.go: --out gets this process's environment
		b, _ := json.Marshal(os.Environ())
		if err := os.WriteFile(out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		return 0
	case "argsenv": // worker_isolation_test.go: --out gets the arguments and the environment
		b, _ := json.Marshal(map[string][]string{"args": os.Args[1:], "env": os.Environ()})
		if err := os.WriteFile(out, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1
		}
		return 0
	case "chinese":
		fmt.Fprint(os.Stderr, longChineseStderr())
		return 1
	case "hang":
		fmt.Println("PROGRESS 0.10")
		time.Sleep(60 * time.Second)
		return 0
	}
	if rc, ok := fakeOVWorker(mode, job, out, writeResult); ok {
		return rc
	}
	if rc, ok := fakePDFWorker(mode, writeResult); ok { // pdf_limits_test.go
		return rc
	}
	if mode == "ckpt" {
		if job["checkpoint"] == nil {
			return runOK() // no checkpoint object: exactly as mode "ok"
		}
		return fakeCheckpointWorker(jb, out)
	}
	fmt.Fprintln(os.Stderr, "ERROR: unknown fake mode", mode)
	return 1
}

// ---------- fake OpenVINO worker and self-test modes (phase 3b task D) ----------
//
// The same binary plays worker/asr_openvino.py (brief section 4): STATUS
// preparing_model / compiling_model, ERROR_CODE lines, a partial --out file
// at the moment of a kill. LF_FAKE_JOBLOG, when set, gets one JSON line per
// invocation: {"script", "job", "cwd", "in", "out"}.

const fakeJobLogEnv = "LF_FAKE_JOBLOG"

func logFakeJob(job map[string]any, in, out string) {
	p := os.Getenv(fakeJobLogEnv)
	if p == "" {
		return
	}
	script := filepath.Base(fakeScriptArg())
	cwd, _ := os.Getwd()
	b, _ := json.Marshal(map[string]any{"script": script, "job": job, "cwd": cwd, "in": in, "out": out})
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// fakeScriptArg is the worker script the fake was started with: the first
// argument after the interpreter's options ("-s"), "" without one.
func fakeScriptArg() string {
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// waitFlag waits (up to 10 s) for the file name in the working directory.
func waitFlag(name string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(name); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeSelfTestEnv holds the self-test script: a JSON object keyed by
// "{model}/{precision}" (compute_type for faster-whisper) of fakeCandidate.
const fakeSelfTestEnv = "LF_FAKE_SELFTEST"

// fakeCandidate is how the fake worker transcribes one self-test candidate.
type fakeCandidate struct {
	Fail        string  `json:"fail"`         // "" | "exit1" | "device" (ERROR_CODE device_unavailable, exit 2)
	Prepare     bool    `json:"prepare"`      // STATUS preparing_model first, then sleeps PrepareMS
	PrepareMS   int     `json:"prepare_ms"`   //
	Compile     bool    `json:"compile"`      // STATUS compiling_model before loading
	LoadMS      int     `json:"load_ms"`      // sleep before the first PROGRESS
	Hang        bool    `json:"hang"`         // after the first PROGRESS, sleep 60 s
	Text        string  `json:"text"`         // the hypothesis (one sentence)
	Words       bool    `json:"words"`        // with word timestamps
	TranscribeS float64 `json:"transcribe_s"` // the timing block's transcribe_s
	NoTiming    bool    `json:"no_timing"`    // no timing block (Go measures)
	PeakMB      float64 `json:"peak_mb"`      //
}

func fakeOVWorker(mode string, job map[string]any, out string, writeResult func() int) (int, bool) {
	switch mode {
	case "ov_ok": // STATUS lines, then a normal run
		fmt.Println("STATUS preparing_model")
		fmt.Println("STATUS compiling_model")
		fmt.Println("PROGRESS 0.10")
		fmt.Println("PROGRESS 0.50")
		if rc := writeResult(); rc != 0 {
			return rc, true
		}
		fmt.Println("PROGRESS 1.00")
		return 0, true
	case "ov_status": // waits for compile.flag and go.flag so that a test can watch stats.status
		fmt.Println("STATUS preparing_model")
		waitFlag("compile.flag")
		fmt.Println("STATUS compiling_model")
		waitFlag("go.flag")
		fmt.Println("PROGRESS 0.50")
		if rc := writeResult(); rc != 0 {
			return rc, true
		}
		fmt.Println("PROGRESS 1.00")
		return 0, true
	case "ov_err_device":
		fmt.Println("ERROR_CODE device_unavailable")
		fmt.Fprintln(os.Stderr, "ERROR: no Intel GPU (vendor 0x8086) among GPU.0, GPU.1")
		return 2, true
	case "ov_err_prepare_net":
		fmt.Println("STATUS preparing_model")
		fmt.Println("ERROR_CODE model_prepare_failed")
		fmt.Fprintln(os.Stderr, "ERROR: download failed: connection reset")
		return 1, true
	case "ov_err_prepare_env":
		fmt.Println("STATUS preparing_model")
		fmt.Println("ERROR_CODE model_prepare_failed")
		fmt.Fprintln(os.Stderr, "ERROR: optimum.intel cannot be imported")
		return 2, true
	case "ov_err_space":
		fmt.Println("STATUS preparing_model")
		fmt.Println("ERROR_CODE no_space {\"free_bytes\": 5368709120, \"need_bytes\": 10737418240}")
		fmt.Fprintln(os.Stderr, "ERROR: not enough free space")
		return 2, true
	case "ov_err_unknown":
		fmt.Println("ERROR_CODE something_new {}")
		fmt.Fprintln(os.Stderr, "ERROR: a new failure")
		return 1, true
	case "hang_tmp": // a partial result.json.tmp exists when the process is killed
		fmt.Println("PROGRESS 0.10")
		if err := os.WriteFile(out, []byte("{\"sentences\": ["), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			return 1, true
		}
		fmt.Println("PROGRESS 0.20")
		time.Sleep(60 * time.Second)
		return 0, true
	case "selftest":
		return fakeSelfTestWorker(job, out), true
	}
	return 0, false
}

func fakeSelfTestWorker(job map[string]any, out string) int {
	var script map[string]fakeCandidate
	if err := json.Unmarshal([]byte(os.Getenv(fakeSelfTestEnv)), &script); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: LF_FAKE_SELFTEST:", err)
		return 1
	}
	prec, _ := job["precision"].(string)
	if prec == "" {
		prec, _ = job["compute_type"].(string)
	}
	key := fmt.Sprint(job["model"]) + "/" + prec
	c, ok := script[key]
	if !ok {
		fmt.Fprintln(os.Stderr, "ERROR: no fake candidate", key)
		return 1
	}
	if c.Prepare {
		fmt.Println("STATUS preparing_model")
		time.Sleep(time.Duration(c.PrepareMS) * time.Millisecond)
	}
	if c.Compile {
		fmt.Println("STATUS compiling_model")
	}
	time.Sleep(time.Duration(c.LoadMS) * time.Millisecond)
	switch c.Fail {
	case "exit1":
		fmt.Fprintln(os.Stderr, "ERROR: out of memory")
		return 1
	case "device":
		fmt.Println("ERROR_CODE device_unavailable")
		fmt.Fprintln(os.Stderr, "ERROR: device lost")
		return 2
	}
	fmt.Println("PROGRESS 0.10")
	if c.Hang {
		time.Sleep(60 * time.Second)
	}
	sent := map[string]any{"start_ms": 0, "end_ms": 1000, "text": c.Text, "low_conf_words": []any{}}
	if c.Words {
		var ws []map[string]any
		for i, w := range strings.Fields(c.Text) {
			ws = append(ws, map[string]any{"w": w, "s": i * 100, "e": i*100 + 90})
		}
		sent["words"] = ws
	}
	res := map[string]any{"prompt_terms_used": 0, "sentences": []any{sent}}
	if !c.NoTiming {
		res["timing"] = map[string]any{"load_s": 2.5, "transcribe_s": c.TranscribeS, "audio_s": 120.0,
			"first_compile": c.Compile, "peak_working_set_mb": c.PeakMB}
	}
	b, _ := json.Marshal(res)
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	fmt.Println("PROGRESS 1.00")
	return 0
}

// ---------- fake checkpointing worker (power management, task P3) ----------
//
// Mode "ckpt" plays both workers with the checkpoint protocol of
// power-brief §4 (the real one is task P2's): without a "checkpoint" object
// in job.json it is mode "ok". With one, the sentences of LF_FAKE_RESULT
// are its decoding windows. It continues from the file at checkpoint.path
// when version, job_id, input_hash and its own backend (asr_openvino.py:
// openvino, otherwise faster-whisper) all match, and deletes the file and
// starts over otherwise; the OpenVINO worker with chunk_s 0 never
// checkpoints (it deletes a file it cannot continue and writes none). After
// every window it writes the checkpoint (.tmp + rename): the kept segments
// with their words, resume_ms = the window's end, the parts so far.
// LF_FAKE_CKPT_STOP=n makes it stop after its n-th window of this run:
// it writes "ckpt.flag" into its working directory and sleeps 60 s, so a
// test can kill it there. The result has the source's sentences assembled
// from the checkpoint's segments and the new ones, and "parts" (the
// checkpoint's parts plus this run's, from resume_ms to the last end).
// LF_FAKE_CKPT_TIMING (a JSON object: load_s, transcribe_s, first_compile,
// peak_working_set_mb) adds the result's "timing" block, with audio_s = the
// audio this run transcribed (from resume_ms to the last end), as both
// workers report it; without it the result has no timing.

const (
	fakeCkptStopEnv   = "LF_FAKE_CKPT_STOP"
	fakeCkptTimingEnv = "LF_FAKE_CKPT_TIMING"
)

// fakeCkptSentence is a sentence of LF_FAKE_RESULT as the fake reads and
// writes it (round trip without loss).
type fakeCkptSentence struct {
	StartMS      int64               `json:"start_ms"`
	EndMS        int64               `json:"end_ms"`
	Text         string              `json:"text"`
	LowConfWords []model.LowConfWord `json:"low_conf_words"`
	Words        []model.Word        `json:"words,omitempty"`
}

func fakeCheckpointWorker(jobJSON []byte, out string) int {
	fail := func(err any) int {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		return 1
	}
	var job struct {
		Checkpoint devices.CheckpointJob `json:"checkpoint"`
	}
	if err := json.Unmarshal(jobJSON, &job); err != nil {
		return fail(err)
	}
	cj := job.Checkpoint
	backend := model.BackendFasterWhisper
	if filepath.Base(fakeScriptArg()) == "asr_openvino.py" {
		backend = model.BackendOpenVINO
	}
	writes := backend == model.BackendFasterWhisper || cj.ChunkS > 0
	var src struct {
		PromptTermsUsed int                `json:"prompt_terms_used"`
		Sentences       []fakeCkptSentence `json:"sentences"`
	}
	b, err := os.ReadFile(os.Getenv("LF_FAKE_RESULT"))
	if err != nil {
		return fail(err)
	}
	if err := json.Unmarshal(b, &src); err != nil {
		return fail(err)
	}
	if !filepath.IsAbs(cj.Path) {
		return fail(fmt.Sprintf("checkpoint.path %q is not absolute", cj.Path))
	}
	// the checkpoint to continue, if any
	ck := devices.Checkpoint{Version: devices.CheckpointVersion, JobID: cj.JobID, InputHash: cj.InputHash, Backend: backend,
		PromptTermsUsed: src.PromptTermsUsed, Parts: []model.ASRPart{}, Segments: []devices.CheckpointSegment{}}
	if b, err := os.ReadFile(cj.Path); err == nil {
		var old devices.Checkpoint
		if json.Unmarshal(b, &old) == nil && writes && old.Version == devices.CheckpointVersion && old.JobID == cj.JobID &&
			old.InputHash == cj.InputHash && old.Backend == backend {
			ck = old
		} else if err := os.Remove(cj.Path); err != nil {
			return fail(err)
		}
	}
	from := ck.ResumeMS
	fmt.Println("PROGRESS 0.05")
	stop := 0
	fmt.Sscan(os.Getenv(fakeCkptStopEnv), &stop)
	windows := 0
	part := model.ASRPart{FromMS: from, ToMS: from, DeviceID: cj.DeviceID, Model: cj.Model, Precision: cj.Precision}
	for _, s := range src.Sentences {
		if s.StartMS < from {
			continue // transcribed by an earlier run
		}
		seg := devices.CheckpointSegment{StartMS: s.StartMS, EndMS: s.EndMS, Words: []devices.CheckpointWord{}}
		for _, w := range strings.Fields(s.Text) {
			seg.Words = append(seg.Words, devices.CheckpointWord{Word: " " + w, Start: float64(s.StartMS) / 1000, End: float64(s.EndMS) / 1000, Probability: 0.9})
		}
		ck.Segments = append(ck.Segments, seg)
		part.ToMS = s.EndMS
		windows++
		if writes {
			ck.ResumeMS = s.EndMS
			c := ck
			c.Parts = append(append([]model.ASRPart(nil), ck.Parts...), part)
			cb, _ := json.MarshalIndent(c, "", " ")
			if err := os.WriteFile(cj.Path+".tmp", cb, 0o644); err != nil {
				return fail(err)
			}
			if err := os.Rename(cj.Path+".tmp", cj.Path); err != nil {
				return fail(err)
			}
		}
		if stop > 0 && windows == stop {
			if err := os.WriteFile("ckpt.flag", nil, 0o644); err != nil {
				return fail(err)
			}
			fmt.Println("PROGRESS 0.50")
			time.Sleep(60 * time.Second)
			return 0
		}
	}
	// assemble the result from all kept segments, like remove_repeats →
	// split_sentences over checkpoint + new segments
	byStart := map[int64]fakeCkptSentence{}
	for _, s := range src.Sentences {
		byStart[s.StartMS] = s
	}
	res := struct {
		PromptTermsUsed int                `json:"prompt_terms_used"`
		Sentences       []fakeCkptSentence `json:"sentences"`
		Parts           []model.ASRPart    `json:"parts"`
		Timing          map[string]any     `json:"timing,omitempty"`
	}{PromptTermsUsed: ck.PromptTermsUsed, Sentences: []fakeCkptSentence{}, Parts: append(ck.Parts, part)}
	if tj := os.Getenv(fakeCkptTimingEnv); tj != "" {
		if err := json.Unmarshal([]byte(tj), &res.Timing); err != nil {
			return fail(err)
		}
		res.Timing["audio_s"] = float64(part.ToMS-from) / 1000
	}
	for _, seg := range ck.Segments {
		s, ok := byStart[seg.StartMS]
		if !ok {
			return fail(fmt.Sprintf("checkpoint segment at %d ms is not in the source", seg.StartMS))
		}
		res.Sentences = append(res.Sentences, s)
	}
	rb, _ := json.Marshal(res)
	if err := os.WriteFile(out, rb, 0o644); err != nil {
		return fail(err)
	}
	fmt.Println("PROGRESS 1.00")
	return 0
}

// readCheckpoint reads a lecture's checkpoint file; ok is false when there
// is none.
func (e *env) readCheckpoint(l model.Lecture) (devices.Checkpoint, bool) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), model.ASRCheckpointFile))
	if os.IsNotExist(err) {
		return devices.Checkpoint{}, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var c devices.Checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		e.t.Fatalf("checkpoint: %v", err)
	}
	return c, true
}

// fakeJob is one line of the LF_FAKE_JOBLOG file.
type fakeJob struct {
	Script string         `json:"script"`
	Job    map[string]any `json:"job"`
	// Cwd is the worker's working directory; In and Out its --in / --out.
	Cwd string `json:"cwd"`
	In  string `json:"in"`
	Out string `json:"out"`
}

// fakeJobs reads the LF_FAKE_JOBLOG file: one fakeJob per worker run.
func fakeJobs(t *testing.T, path string) []fakeJob {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []fakeJob
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r fakeJob
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("job log line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// ---------- fake ffmpeg ----------

const fakeFFmpegEnv = "LF_FAKE_FFMPEG"

// fakeFFmpeg imitates ffmpeg's stderr for the failure classes prep must tell
// apart; in mode "ok" it writes a WAV-sized file to the (absolute) output.
func fakeFFmpeg(mode string) int {
	args := os.Args[1:]
	out := args[len(args)-1]
	media := ""
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			media = args[i+1]
		}
	}
	fmt.Fprintf(os.Stderr, "Input #0, wav, from '%s':\n  Duration: 00:00:04.00, bitrate: 1411 kb/s\n", media)
	if !filepath.IsAbs(out) || !filepath.IsAbs(media) {
		fmt.Fprintf(os.Stderr, "[out#0/wav @ 000001] Error opening output %s: No such file or directory\n", out)
		return 1
	}
	switch mode {
	case "ok":
		fmt.Println("out_time_us=1000000")
		fmt.Println("progress=continue")
		fmt.Println("out_time_us=4000000")
		if err := os.WriteFile(out, make([]byte, 1000), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("progress=end")
		return 0
	case "outfail":
		fmt.Fprintf(os.Stderr, "[out#0/wav @ 000001] Error opening output %s: No such file or directory\nError opening output file %s.\nError opening output files: No such file or directory\n", out, out)
		return 1
	case "diskfull":
		fmt.Fprintln(os.Stderr, "[aac @ 0000] Error while decoding stream #0:0: Invalid data found when processing input")
		fmt.Fprintln(os.Stderr, "av_interleaved_write_frame(): No space left on device")
		return 1
	case "corrupt":
		fmt.Fprintf(os.Stderr, "[in#0 @ 0000] Error opening input: Invalid data found when processing input\nError opening input file %s.\n%s: Invalid data found when processing input\n", media, media)
		return 1
	case "noaudio":
		fmt.Fprintln(os.Stderr, "Output #0, wav, to 'audio.wav.tmp':\n[out#0/wav @ 0000] Output file does not contain any stream")
		return 1
	case "chinese":
		fmt.Fprint(os.Stderr, longChineseStderr())
		return 1
	}
	return 1
}

// useFakeFFmpeg points config.ffmpeg at this binary in mode.
func (e *env) useFakeFFmpeg(mode string) {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg.FFmpeg = exe
	e.t.Setenv(fakeFFmpegEnv, mode)
}

// useFakeWorker points config.python at this binary and selects a mode;
// result is the file the fake copies to --out.
func (e *env) useFakeWorker(mode, result string) {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	wd := filepath.Join(e.root, "worker")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for _, n := range []string{"asr.py", "asr_openvino.py", "extract_pdf.py"} {
		if err := os.WriteFile(filepath.Join(wd, n), []byte("# fake\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	e.cfg.Python = exe
	e.cfg.PythonOpenVINO = exe // the same fake plays worker/asr_openvino.py (phase 3b)
	e.cfg.WorkerDir = wd
	e.t.Setenv(fakeWorkerEnv, mode)
	if result != "" {
		abs, _ := filepath.Abs(result)
		e.t.Setenv("LF_FAKE_RESULT", abs)
	}
}

// ---------- events ----------

type recEvents struct {
	mu      sync.Mutex
	changed []string    // "kind:id"
	jobs    []model.Job // Job events, in order
}

func (r *recEvents) Job(j model.Job) {
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
}

// jobEvents are the recorded Job events of stage for scopeID.
func (r *recEvents) jobEvents(scopeID, stage string) []model.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.Job
	for _, j := range r.jobs {
		if j.ScopeID == scopeID && j.Stage == stage {
			out = append(out, j)
		}
	}
	return out
}
func (r *recEvents) Changed(kind, id string) {
	r.mu.Lock()
	r.changed = append(r.changed, kind+":"+id)
	r.mu.Unlock()
}
func (r *recEvents) has(kind, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.changed {
		if c == kind+":"+id {
			return true
		}
	}
	return false
}

// ---------- scripted LLM ----------

type llmCall struct {
	Role, System, User string
	JSONMode           bool
}

// scriptLLM is an llm.Client whose answers come from a function.
type scriptLLM struct {
	mu    sync.Mutex
	calls []llmCall
	fn    func(ctx context.Context, n int, c llmCall) (string, error) // n: 0-based call number
}

func (s *scriptLLM) Complete(ctx context.Context, role, system, user string, jsonMode bool) (llm.Result, error) {
	c := llmCall{Role: role, System: system, User: user, JSONMode: jsonMode}
	s.mu.Lock()
	n := len(s.calls)
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return llm.Result{}, err
	}
	text, err := s.fn(ctx, n, c)
	if err != nil {
		return llm.Result{}, err
	}
	sched.ReportUsage(ctx, 10, 5, 2)
	return llm.Result{Text: text, PromptTokens: 10, CompletionTokens: 5, CacheHitTokens: 2}, nil
}

func (s *scriptLLM) Calls() []llmCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llmCall(nil), s.calls...)
}

// ---------- environment ----------

type env struct {
	t       *testing.T
	ctx     context.Context
	root    string
	st      db.Store
	cfg     *config.Config
	fake    *llmtest.Server
	ev      *recEvents
	course  model.Course
	invalMu sync.Mutex
	inval   []string
	causes  []state.StaleCause
	gpuMB   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	st, err := db.Open(filepath.Join(root, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DataDir = filepath.Join(root, "data")
	e := &env{t: t, ctx: context.Background(), root: root, st: st, cfg: cfg, ev: &recEvents{}, gpuMB: 12282}
	e.fake = llmtest.New(t)
	e.fake.Configure(cfg)
	e.course = model.Course{ID: db.NewID(), Name: "Computer Architecture", CreatedAt: db.Now()}
	if err := st.CreateCourse(e.ctx, e.course); err != nil {
		t.Fatal(err)
	}
	return e
}

// deps builds Deps with client (nil = an HTTP client on the fake LLM server).
func (e *env) deps(client llm.Client) Deps {
	if client == nil {
		c := llm.New(e.cfg, nil, "", nil)
		c.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
		client = c
	}
	return Deps{
		Store: e.st, Cfg: e.cfg, LLM: client, DataDir: e.cfg.DataDir, Events: e.ev,
		Invalidate: func(ctx context.Context, courseID string, cause state.StaleCause) error {
			e.invalMu.Lock()
			e.inval = append(e.inval, courseID)
			e.causes = append(e.causes, cause)
			e.invalMu.Unlock()
			return nil
		},
		GPUTotalMB: func(context.Context) int { return e.gpuMB },
	}
}

// invalCauses returns the causes passed to Deps.Invalidate, in call order.
func (e *env) invalCauses() []state.StaleCause {
	e.invalMu.Lock()
	defer e.invalMu.Unlock()
	return append([]state.StaleCause(nil), e.causes...)
}

func (e *env) stage(d Deps, name string) sched.Stage {
	e.t.Helper()
	s, ok := ByName(d)[name]
	if !ok {
		e.t.Fatalf("no stage %s", name)
	}
	return s
}

func (e *env) invalidations() []string {
	e.invalMu.Lock()
	defer e.invalMu.Unlock()
	return append([]string(nil), e.inval...)
}

// lecture creates a launched lecture whose six jobs carry the snapshots of
// the current config (what v1.1 "启动" writes, A3.1/A4.1).
func (e *env) lecture(title, media string) model.Lecture {
	e.t.Helper()
	now := db.Now()
	l := e.draft(title, media)
	if _, err := e.st.SetLectureLaunched(e.ctx, l.ID, now); err != nil {
		e.t.Fatal(err)
	}
	for stage, snap := range e.snapshots() {
		e.setSnapshot(l.ID, stage, snap)
	}
	l, err := e.st.GetLecture(e.ctx, l.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// draft creates a lecture that was never launched: its jobs have no snapshot.
func (e *env) draft(title, media string) model.Lecture {
	e.t.Helper()
	l := model.Lecture{ID: db.NewID(), CourseID: e.course.ID, Title: title, MediaPath: media, CreatedAt: db.Now()}
	if err := e.st.CreateLecture(e.ctx, l); err != nil {
		e.t.Fatal(err)
	}
	if err := os.MkdirAll(model.LectureDir(e.cfg.DataDir, l.CourseID, l.ID), 0o755); err != nil {
		e.t.Fatal(err)
	}
	l, err := e.st.GetLecture(e.ctx, l.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// snapshots are the config_snapshot values the current config gives the
// lecture stages (provider_id = the config key, as the legacy import does).
func (e *env) snapshots() map[string]string {
	a := e.cfg.ASR
	out := map[string]string{
		model.StagePrep:   model.EmptySnapshot,
		model.StageExport: model.EmptySnapshot,
		model.StageASR:    model.MarshalSnapshot(model.ASRSnapshot{Model: a.Model, ComputeType: a.ComputeType, Device: a.Device, Language: a.Language}),
	}
	for _, stage := range []string{model.StageSummary, model.StageRepair, model.StageTranslate} {
		out[stage] = model.MarshalSnapshot(e.llmSnapshot(stage, ""))
	}
	return out
}

// llmSnapshot is the LLM snapshot of stage from the config; modelName ""
// keeps the configured model.
func (e *env) llmSnapshot(stage, modelName string) model.LLMSnapshot {
	role := model.RoleOfStage(stage)
	r := e.cfg.LLM.Roles[role]
	if modelName == "" {
		modelName = r.Model
	}
	temp := model.DefaultTemperature(role)
	s := model.LLMSnapshot{ProviderID: r.Provider, ProviderName: r.Provider, Model: modelName, Params: model.RoleParams{Temperature: &temp}}
	if stage == model.StageTranslate {
		w := r.Window
		s.Params.Window = &w
	}
	return s
}

// setSnapshot writes the config_snapshot of the lecture's current job of stage.
func (e *env) setSnapshot(lectureID, stage, raw string) {
	e.t.Helper()
	j := e.job(model.ScopeLecture, lectureID, stage)
	e.must(e.st.SetJobSnapshot(e.ctx, j.ID, raw))
}

type sampleRow struct {
	Idx          int                 `json:"idx"`
	StartMS      int64               `json:"start_ms"`
	EndMS        int64               `json:"end_ms"`
	ASRText      string              `json:"asr_text"`
	LowConfWords []model.LowConfWord `json:"low_conf_words"`
}

// insertSample writes testdata/sample_sentences.json as the lecture's
// sentences (as asr would) and returns them.
func (e *env) insertSample(lectureID string) []model.Sentence {
	e.t.Helper()
	b, err := os.ReadFile("../../testdata/sample_sentences.json")
	if err != nil {
		e.t.Fatal(err)
	}
	var rows []sampleRow
	if err := json.Unmarshal(b, &rows); err != nil {
		e.t.Fatal(err)
	}
	ss := make([]model.Sentence, len(rows))
	for i, r := range rows {
		ss[i] = model.Sentence{LectureID: lectureID, Idx: r.Idx, StartMS: r.StartMS, EndMS: r.EndMS, ASRText: r.ASRText,
			LowConfWords: r.LowConfWords, RepairedText: r.ASRText, ZhStatus: model.ZhNone}
	}
	e.insertSentences(lectureID, ss)
	return e.sentences(lectureID)
}

func (e *env) insertSentences(lectureID string, ss []model.Sentence) {
	e.t.Helper()
	if err := e.st.Tx(e.ctx, func(tx db.Store) error { return tx.InsertSentences(e.ctx, lectureID, ss) }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sentences(lectureID string) []model.Sentence {
	e.t.Helper()
	ss, err := e.st.ListSentences(e.ctx, lectureID, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return ss
}

func (e *env) setSummary(lectureID, text string) {
	e.t.Helper()
	if err := e.st.UpsertSummary(e.ctx, model.Summary{LectureID: lectureID, Text: text, InputHash: "x"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addTerm(en, zh, source string, freq int) model.Term {
	e.t.Helper()
	t := model.Term{ID: db.NewID(), CourseID: e.course.ID, En: en, EnKey: db.EnKey(en), Zh: zh, Source: source, Enabled: true, Freq: freq}
	if err := e.st.InsertTerm(e.ctx, t); err != nil {
		e.t.Fatal(err)
	}
	return t
}

func (e *env) terms() []model.Term {
	e.t.Helper()
	ts, err := e.st.ListTerms(e.ctx, e.course.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

func (e *env) job(scope, id, stage string) model.Job {
	e.t.Helper()
	j, err := e.st.GetCurrentJob(e.ctx, scope, id, stage)
	if err != nil {
		e.t.Fatalf("job %s %s: %v", id, stage, err)
	}
	return j
}

// progressRec records progress values.
type progressRec struct {
	mu   sync.Mutex
	vals []float64
}

func (p *progressRec) fn(v float64) {
	p.mu.Lock()
	p.vals = append(p.vals, v)
	p.mu.Unlock()
}

func (p *progressRec) last() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.vals) == 0 {
		return -1
	}
	return p.vals[len(p.vals)-1]
}

// runResult is what run returns.
type runResult struct {
	err      error
	stats    model.JobStats
	progress *progressRec
	job      model.Job // after the final transition
}

// run executes a stage the way the scheduler does: current job →
// queued → running, InputHash stored, Run with the job and a stats sink in
// ctx, stats persisted, then running → done / failed.
func (e *env) run(stg sched.Stage, scopeID string) runResult {
	return e.runCtx(e.ctx, stg, scopeID)
}

func (e *env) runCtx(ctx context.Context, stg sched.Stage, scopeID string) runResult {
	e.t.Helper()
	scope := model.ScopeOfStage(stg.Name())
	j := e.job(scope, scopeID, stg.Name())
	switch j.Status {
	case model.StatusPending:
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusPending, model.StatusQueued, db.JobFinish{}))
	case model.StatusFailed, model.StatusCancelled:
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, j.Status, model.StatusQueued, db.JobFinish{}))
	case model.StatusQueued:
	default:
		e.t.Fatalf("run %s: job is %s", stg.Name(), j.Status)
	}
	e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusQueued, model.StatusRunning, db.JobFinish{}))
	h, err := stg.InputHash(e.ctx, scopeID)
	if err != nil {
		e.t.Fatalf("InputHash %s: %v", stg.Name(), err)
	}
	if h != "" {
		e.must(e.st.SetJobInputHash(e.ctx, j.ID, h))
	}
	j, _ = e.st.GetJob(e.ctx, j.ID)
	rec := &sched.StatsRecorder{Stats: j.Stats}
	pr := &progressRec{}
	rctx := sched.WithStatsSink(sched.WithJob(ctx, j), rec)
	runErr := stg.Run(rctx, scopeID, pr.fn)
	stats := rec.Snapshot()
	e.must(e.st.SetJobStats(e.ctx, j.ID, stats))
	switch {
	case runErr == nil:
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusRunning, model.StatusDone, db.JobFinish{}))
	case errors.Is(runErr, context.Canceled):
		e.must(state.TransitionJob(e.ctx, e.st, j.ID, model.StatusRunning, model.StatusCancelled, db.JobFinish{}))
	default:
		// as the scheduler stores it (v2 P14.3): a coded error as its ErrorInfo JSON
		_, err := state.FailJob(e.ctx, e.st, j.ID, state.EncodeJobError(runErr), false)
		e.must(err)
	}
	j, _ = e.st.GetJob(e.ctx, j.ID)
	return runResult{err: runErr, stats: stats, progress: pr, job: j}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// markDone moves a lecture/material/course job to done without running it.
func (e *env) markDone(scope, scopeID, stage string) {
	e.t.Helper()
	j := e.job(scope, scopeID, stage)
	for _, step := range [][2]string{{model.StatusPending, model.StatusQueued}, {model.StatusQueued, model.StatusRunning}, {model.StatusRunning, model.StatusDone}} {
		if j.Status == step[0] {
			e.must(state.TransitionJob(e.ctx, e.st, j.ID, step[0], step[1], db.JobFinish{}))
			j.Status = step[1]
		}
	}
}

// ffmpegOrSkip returns the ffmpeg executable or skips (SPEC §14).
func ffmpegOrSkip(t *testing.T, cfg *config.Config) string {
	t.Helper()
	p, err := exec.LookPath(cfg.FFmpeg)
	if err != nil {
		t.Skipf("ffmpeg not found (%v): prep is skipped as allowed by SPEC §14", err)
	}
	return p
}

// silentMedia generates seconds of silence with ffmpeg at path.
func silentMedia(t *testing.T, ffmpeg, path string, seconds int) {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", fmt.Sprint(seconds), "-c:a", "pcm_s16le", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
}
