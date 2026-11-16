// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realcolab

package colab

// The memory-mode experiment on real Colab runtimes (SPEC-ext "Colab 内存
//运行（实验）", 实验验收). Not part of the default build or test run: it
// borrows real runtimes with the account owner's gcloud credentials, uploads
// a lecture to Google and costs compute units.
//
//	LF_COLAB_CONSENT=1 \
//	LECTUREFLOW_DATA_DIR=<a new, empty directory> \
//	LF_COLAB_AUDIO=<16 kHz mono WAV of one lecture> LF_COLAB_SHORT_AUDIO=<a few minutes of it> \
//	LF_COLAB_REPORT=<report.json> LF_COLAB_RUNS=disk,memory,disk,memory,disk,memory,fallback-short,tmpfs-short \
//	go test -tags realcolab -run TestRealColabMemoryExperiment -timeout 8h -v ./internal/colab/
//
// The credential is imported like the app's "从自己的 gcloud 导入" (POST
// /api/colab/auth/import → ImportADC) into the database of
// LECTUREFLOW_DATA_DIR with the "session" lifecycle: it is deleted when the
// test ends (Close; secure delete), and never revoked (revoking would also
// sign the user's own gcloud out). Every run borrows a fresh runtime
// (cold start), transcribes, checks the runtime's persistent disk BEFORE
// the release, releases the runtime and checks that it is gone.
//
// Runs: disk (memory mode off, today's behaviour), memory (auto),
// fallback (auto with the estimate raised by 1 TB: must fall back to the
// disk), tmpfs (auto, /dev/shm skipped: the probe mounts its own tmpfs);
// "-short" uses LF_COLAB_SHORT_AUDIO.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/db"
)

type expStage struct {
	Name string  `json:"name"`
	AtS  float64 `json:"at_s"` // seconds after Transcribe was called
}

type expRun struct {
	Label    string  `json:"label"`
	Mode     string  `json:"mode"` // kv colab.memory_mode
	Audio    string  `json:"audio"`
	AudioSec float64 `json:"audio_sec"`
	Started  string  `json:"started"`

	// ColdStartS: Transcribe called → the remote transcribe stage began
	// (assign, boot, probe, uploads, install, model download, model load).
	ColdStartS float64    `json:"cold_start_s"`
	ReadyS     float64    `json:"runtime_ready_s"` // assign + boot
	Stages     []expStage `json:"stages"`
	WallS      float64    `json:"wall_s"` // until result.json was written
	// TranscribeS: the worker's own transcription time (done.transcribe_s).
	TranscribeS float64 `json:"transcribe_s"`
	LoadS       float64 `json:"setup_s"` // install + model download (event times)
	Sentences   int     `json:"sentences"`
	// MemPeakMB: the runtime's peak used memory (MemTotal - MemAvailable,
	// the RAM disk counted as used) while the worker ran; BaselineUsedMB
	// right after boot.
	MemPeakMB      int64 `json:"mem_peak_mb"`
	MemTotalMB     int64 `json:"mem_total_mb"`
	BaselineUsedMB int64 `json:"baseline_used_mb"`

	Storage  string          `json:"storage"`
	Decision *memoryDecision `json:"decision,omitempty"`
	Baseline json.RawMessage `json:"baseline,omitempty"`
	Residue  json.RawMessage `json:"residue,omitempty"`

	ReleaseReason    string  `json:"release_reason"`
	EndpointGone     bool    `json:"endpoint_gone"`
	AssignmentsAfter int     `json:"assignments_after"`
	BalanceBefore    float64 `json:"balance_before"`
	BalanceAfter     float64 `json:"balance_after"`
	Err              string  `json:"error,omitempty"`
}

// baselineCell: the remote clock (the residue check's marker) and the
// memory right after boot.
const baselineCell = `import json, os, time
def _mi():
    d = {}
    for l in open('/proc/meminfo'):
        k, _, r = l.partition(':')
        if r.split():
            d[k] = int(r.split()[0])
    return d
_m = _mi()
_st = os.statvfs('/dev/shm')
print(json.dumps({'marker': time.time(), 'mem_total_mb': _m['MemTotal'] // 1024, 'mem_available_mb': _m['MemAvailable'] // 1024,
    'shmem_mb': _m.get('Shmem', 0) // 1024, 'swaps': open('/proc/swaps').read().splitlines(),
    'dev_shm_size_mb': _st.f_blocks * _st.f_frsize >> 20, 'dev_shm_free_mb': _st.f_bavail * _st.f_frsize >> 20,
    'mounts': [l.split()[1] + ' ' + l.split()[2] for l in open('/proc/mounts')]}))
`

// residueFind is the find command of the residue check (shown in the
// report as it ran): every file on a disk-backed file system changed after
// the marker that looks like a recording, a result, a job file or a model.
const residueFind = `for mp in $(findmnt -rn -o TARGET,FSTYPE | awk '$2 !~ /^(tmpfs|ramfs|devtmpfs|proc|sysfs|devpts|cgroup2?|mqueue|securityfs|debugfs|tracefs|pstore|bpf|autofs|hugetlbfs|configfs|fusectl|binfmt_misc|nsfs|rpc_pipefs|efivarfs)$/ {print $1}'); do find "$mp" -xdev -type f -newermt "@MARKER" \( -name '*.flac' -o -name '*.wav' -o -name 'result.json*' -o -name 'job.json' -o -name 'stderr.log' -o -name 'model.bin' -o -name 'vocabulary.*' -o -path '*models--*' \) -printf '%s %p\n'; done | sort -u`

// residueCell walks every disk-backed file system for files changed after
// the marker (ctime or mtime), classifies them, runs residueFind, and
// describes the RAM disks.
const residueCell = `import json, os, stat, subprocess
M = MARKER
RAMV = {'tmpfs','ramfs','devtmpfs','proc','sysfs','devpts','cgroup','cgroup2','mqueue','securityfs','debugfs','tracefs','pstore','bpf','autofs','hugetlbfs','configfs','fusectl','binfmt_misc','nsfs','rpc_pipefs','efivarfs'}
mounts = [(l.split()[1].replace('\\040', ' '), l.split()[2]) for l in open('/proc/mounts')]
mps = set(m for m, _ in mounts)
disk = sorted(set(m for m, fs in mounts if fs not in RAMV))
cats = {'recording': [], 'result_or_job': [], 'model': [], 'packages': [], 'lectureflow': []}
total, by_dir, big = 0, {}, []
for mp in disk:
    try:
        dev = os.lstat(mp).st_dev
    except OSError:
        continue
    for root, dirs, files in os.walk(mp):
        dirs[:] = [d for d in dirs if os.path.join(root, d) not in mps]
        for f in files:
            p = os.path.join(root, f)
            try:
                st = os.lstat(p)
            except OSError:
                continue
            if st.st_dev != dev or not stat.S_ISREG(st.st_mode) or max(st.st_mtime, st.st_ctime) < M:
                continue
            total += 1
            k = '/'.join(p.split('/')[:4])
            n, s, ex = by_dir.get(k, (0, 0, []))
            if len(ex) < 4:
                ex.append(p)
            by_dir[k] = (n + 1, s + st.st_size, ex)
            if st.st_size >= 1 << 20:
                big.append((st.st_size, p))
            low = p.lower()
            if f in ('audio.flac', 'audio.wav') or low.endswith(('.flac', '.wav', '.mp3', '.m4a', '.mp4')):
                cats['recording'].append(p)
            if f.startswith('result.json') or f in ('job.json', 'stderr.log'):
                cats['result_or_job'].append(p)
            if f == 'model.bin' or 'models--' in p or f.startswith('vocabulary.'):
                cats['model'].append(p)
            if any(s in p for s in ('/faster_whisper', '/ctranslate2', '/onnxruntime', '/av/', '/tokenizers/')):
                cats['packages'].append(p)
            if '/lectureflow' in p:
                cats['lectureflow'].append(p)
def du(p):
    t = 0
    for r, _, fs in os.walk(p):
        for f in fs:
            try:
                t += os.lstat(os.path.join(r, f)).st_size
            except OSError:
                pass
    return t >> 20
ram = {}
for b in ('/dev/shm/lectureflow', '/mnt/lectureflow', '/content/lectureflow'):
    if os.path.isdir(b):
        ram[b] = {e: du(os.path.join(b, e)) if os.path.isdir(os.path.join(b, e)) else os.path.getsize(os.path.join(b, e)) >> 20 for e in sorted(os.listdir(b))}
        # every file outside the package and model directories (bytes)
        fl = []
        for r, ds, fs in os.walk(b):
            ds[:] = [d for d in ds if not (r == b and d in ('env', 'hf'))]
            fl += [[os.path.relpath(os.path.join(r, f), b), os.lstat(os.path.join(r, f)).st_size] for f in fs]
        ram[b + ' (files outside env/ and hf/)'] = sorted(fl)[:60]
trash = {}
for t in ('/root/.local/share/Trash', '/.Trash-0', '/content/.Trash-0', '/mnt/lectureflow/.Trash-0', '/dev/shm/.Trash-0'):
    if os.path.exists(t):
        trash[t] = sum(len(fs) for _, _, fs in os.walk(t))
mi = {}
for l in open('/proc/meminfo'):
    k, _, r = l.partition(':')
    if r.split():
        mi[k] = int(r.split()[0]) >> 10
fo = subprocess.run(['bash', '-c', FIND], stdout=subprocess.PIPE, stderr=subprocess.STDOUT).stdout.decode('utf-8', 'replace')
print(json.dumps({'disk_filesystems': disk, 'changed_files': total,
    'top_dirs': sorted(([k, n, s >> 20, ex] for k, (n, s, ex) in by_dir.items()), key=lambda x: -x[1])[:25],
    'trash_files': trash,
    'big_files_mb': [[s >> 20, p] for s, p in sorted(big, reverse=True)[:30]],
    'categories': {k: {'count': len(v), 'examples': v[:15]} for k, v in cats.items()},
    'hf_cache_on_disk': os.path.isdir('/root/.cache/huggingface'), 'pip_cache_on_disk': os.path.isdir('/root/.cache/pip'),
    'ram_disks_mb': ram, 'swaps': open('/proc/swaps').read().splitlines(),
    'meminfo_mb': {k: mi.get(k) for k in ('MemTotal', 'MemAvailable', 'Shmem')},
    'find_command': FIND, 'find_output': fo.splitlines()[:200]}))
`

// execCell runs code in the session's kernel and returns its stdout.
func execCell(ctx context.Context, b *Backend, code string) (string, error) {
	b.mu.Lock()
	s := b.sess
	var kid string
	if s != nil {
		kid = s.kernelID
	}
	b.mu.Unlock()
	if s == nil || kid == "" {
		return "", errors.New("no runtime or kernel")
	}
	rt, err := b.runtime(ctx, s)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	out, err := rt.kernelExec(ctx, kid, code, func(x string) error { buf.WriteString(x); return nil }, b.newReconnector(nil), nil)
	if err != nil {
		return buf.String(), err
	}
	if out.Status != "ok" {
		return buf.String(), fmt.Errorf("cell status %s (%s)", out.Status, out.EName)
	}
	return buf.String(), nil
}

func wavSeconds(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(fi.Size()-44) / 32000 // 16 kHz mono s16 (ffmpeg -ar 16000 -ac 1)
}

// TestRealColabCellsCompile: the experiment's cells are valid Python (no
// runtime needed; uses LF_PYTHON or python on PATH).
func TestRealColabCellsCompile(t *testing.T) {
	py := os.Getenv("LF_PYTHON")
	if py == "" {
		py = "python"
	}
	find, _ := json.Marshal(strings.ReplaceAll(residueFind, "MARKER", "1.000"))
	for name, code := range map[string]string{
		"baseline": baselineCell,
		"residue":  "FIND = " + string(find) + "\n" + strings.Replace(residueCell, "M = MARKER", "M = 1.000", 1),
	} {
		f := filepath.Join(t.TempDir(), name+".py")
		if err := os.WriteFile(f, []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(py, "-c", "import sys; compile(open(sys.argv[1], encoding='utf-8').read(), sys.argv[1], 'exec')", f).CombinedOutput(); err != nil {
			t.Errorf("%s cell: %v\n%s", name, err, out)
		}
	}
}

// TestRealColabDebug borrows a runtime, uploads the worker scripts to
// LF_COLAB_DEBUG_BASE (a Contents API path, default dev/shm/lectureflow),
// runs the cells of LF_COLAB_DEBUG_CELLS (a Python file, cells separated by
// lines "#%%") in one kernel, logs their output and releases the runtime.
// For diagnosing the experiment; uploads no audio.
func TestRealColabDebug(t *testing.T) {
	if os.Getenv("LF_COLAB_CONSENT") != "1" || os.Getenv("LF_COLAB_DEBUG_CELLS") == "" {
		t.Skip("set LF_COLAB_CONSENT=1 and LF_COLAB_DEBUG_CELLS")
	}
	src, err := os.ReadFile(os.Getenv("LF_COLAB_DEBUG_CELLS"))
	if err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("LF_COLAB_DEBUG_BASE")
	if base == "" {
		base = "dev/shm/lectureflow"
	}
	ctx := context.Background()
	dataDir := os.Getenv("LECTUREFLOW_DATA_DIR")
	store, err := db.Open(filepath.Join(dataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := filepath.Abs(filepath.Join("..", "..", "worker"))
	b, err := New(Options{KV: store, WorkerDir: wd, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Startup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleSession}); err != nil {
		t.Fatalf("import: %s", Code(err))
	}
	defer func() {
		cctx := context.WithoutCancel(ctx)
		if err := b.release(cctx, ReleaseQueueEmpty); err != nil {
			t.Errorf("release: %v", err)
		}
		list, err := b.tun.listAssignments(cctx, false)
		t.Logf("assignments after release: %d (%v)", len(list), Code(err))
		_ = b.Close(cctx)
	}()
	jp := &jobProgress{b: b, out: func(Event) {}, est: -1, j: &jobState{started: time.Now()}}
	s, err := b.acquire(ctx, b.Settings(), jp)
	if err != nil {
		t.Fatalf("acquire: %s %v", Code(err), err)
	}
	rt, err := b.runtime(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := rt.startKernel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	s.kernelID = kid
	b.mu.Unlock()
	files, _, err := b.readWorkerFiles()
	if err != nil {
		t.Fatal(err)
	}
	for i, cell := range strings.Split(string(src), "\n#%%") {
		if i == 1 { // the first cell may prepare the directory; upload after it
			for _, name := range workerFiles {
				if err := rt.upload(ctx, base+"/"+name, files[name], len(files[name])+1, nil); err != nil {
					t.Fatalf("upload %s: %v", name, err)
				}
			}
		}
		out, err := execCell(ctx, b, cell)
		t.Logf("---- cell %d (err %v) ----\n%s", i+1, err, out)
	}
}

func TestRealColabMemoryExperiment(t *testing.T) {
	if os.Getenv("LF_COLAB_CONSENT") != "1" {
		t.Skip("set LF_COLAB_CONSENT=1: borrows real Colab runtimes with your gcloud credentials")
	}
	dataDir := os.Getenv("LECTUREFLOW_DATA_DIR")
	audio, short := os.Getenv("LF_COLAB_AUDIO"), os.Getenv("LF_COLAB_SHORT_AUDIO")
	reportPath := os.Getenv("LF_COLAB_REPORT")
	runs := strings.Split(os.Getenv("LF_COLAB_RUNS"), ",")
	if dataDir == "" || audio == "" || reportPath == "" || len(runs) == 0 || runs[0] == "" {
		t.Fatal("LECTUREFLOW_DATA_DIR, LF_COLAB_AUDIO, LF_COLAB_REPORT and LF_COLAB_RUNS are required")
	}
	ctx := context.Background()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(filepath.Join(dataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	logf, err := os.OpenFile(filepath.Join(dataDir, "colab.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()
	wd, _ := filepath.Abs(filepath.Join("..", "..", "worker"))
	ffmpeg := os.Getenv("LF_COLAB_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	b, err := New(Options{KV: store, WorkerDir: wd, FFmpeg: ffmpeg,
		Logger: slog.New(slog.NewTextHandler(logf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Startup(ctx); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	set := b.Settings()
	set.MaxQueueWaitMin, set.IdleReleaseMin = 20, 60 // the harness releases each runtime itself
	if err := b.SetSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	info, err := b.ImportADC(ctx, Lifecycle{Kind: LifecycleSession})
	if err != nil {
		t.Fatalf("importing the gcloud credential (code %s): %v", Code(err), err)
	}
	t.Logf("credential imported: lifecycle %s, source %s, %d scopes", info.Lifecycle, info.Source, len(info.Scopes))
	var endpoints []string
	defer func() {
		cctx := context.WithoutCancel(ctx)
		_ = b.QueueDrained(cctx)
		list, err := b.tun.listAssignments(cctx, false)
		_ = b.Close(cctx) // releases a runtime still held; deletes the session credential (not revoked)
		if err == nil {
			for _, a := range list {
				for _, e := range endpoints {
					if a.Endpoint == e {
						t.Errorf("runtime %s still assigned after the experiment", e)
					}
				}
			}
			t.Logf("assignments of the account after the experiment: %d", len(list))
		} else {
			t.Logf("listing assignments after Close: %v", Code(err))
		}
		if v, _, _ := store.GetKV(cctx, KVCredential); v != "" {
			t.Error("the session credential is still stored")
		}
	}()

	var report []expRun
	if old, err := os.ReadFile(reportPath); err == nil {
		_ = json.Unmarshal(old, &report)
	}
	save := func() {
		data, _ := json.MarshalIndent(report, "", " ")
		_ = os.WriteFile(reportPath, data, 0o644)
	}
	outDir := filepath.Join(dataDir, "out")
	_ = os.MkdirAll(outDir, 0o700)

	for i, spec := range runs {
		label, file := spec, audio
		if base, ok := strings.CutSuffix(spec, "-short"); ok {
			label, file = base, short
		}
		r := expRun{Label: spec, Audio: filepath.Base(file), AudioSec: wavSeconds(file), Started: time.Now().Format(time.RFC3339)}
		b.o.MemoryExtraMB, b.o.MemoryNoShm = 0, false
		switch label {
		case "disk":
			r.Mode = MemoryModeOff
		case "memory":
			r.Mode = MemoryModeAuto
		case "fallback":
			r.Mode, b.o.MemoryExtraMB = MemoryModeAuto, 1000000
		case "tmpfs":
			r.Mode, b.o.MemoryNoShm = MemoryModeAuto, true
		default:
			t.Fatalf("unknown run %q", spec)
		}
		if err := store.SetKV(ctx, KVMemoryMode, r.Mode); err != nil {
			t.Fatal(err)
		}
		if c, err := b.tun.ccuInfo(ctx); err == nil {
			r.BalanceBefore = c.Balance
		}
		t0 := time.Now()
		since := func() float64 { return time.Since(t0).Seconds() }
		var mu sync.Mutex
		var marker float64
		b.hookReady = func(hctx context.Context, s *liveSession) {
			mu.Lock()
			defer mu.Unlock()
			r.ReadyS = since()
			b.mu.Lock()
			endpoints = append(endpoints, s.rec.Endpoint)
			b.mu.Unlock()
			rt, err := b.runtime(hctx, s)
			if err == nil && s.kernelID == "" {
				var kid string
				if kid, err = rt.startKernel(hctx); err == nil {
					b.mu.Lock()
					s.kernelID = kid
					b.mu.Unlock()
				}
			}
			if err != nil {
				r.Err = "hook: " + Code(err)
				return
			}
			out, err := execCell(hctx, b, baselineCell)
			if err != nil {
				r.Err = "baseline: " + err.Error()
				return
			}
			r.Baseline = json.RawMessage(strings.TrimSpace(out))
			var bl struct {
				Marker    float64 `json:"marker"`
				Total     int64   `json:"mem_total_mb"`
				Available int64   `json:"mem_available_mb"`
			}
			_ = json.Unmarshal(r.Baseline, &bl)
			marker, r.BaselineUsedMB = bl.Marker, bl.Total-bl.Available
		}
		seen := map[string]bool{}
		onEvent := func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			for _, name := range []string{ev.Step, "stage:" + ev.Stage} {
				if name != "stage:" && !seen[name] {
					seen[name] = true
					r.Stages = append(r.Stages, expStage{Name: name, AtS: since()})
					if name == "stage:transcribe" {
						r.ColdStartS = since()
					}
					t.Logf("[%s] %6.0fs %s", spec, since(), name)
				}
			}
		}
		jobOut := filepath.Join(outDir, fmt.Sprintf("run-%d", i+1))
		_ = os.MkdirAll(jobOut, 0o700)
		path, terr := b.Transcribe(ctx, Job{ID: fmt.Sprintf("exp-%s-%d", label, i+1), AudioPath: file, DurationSec: r.AudioSec,
			Language: "en", OutDir: jobOut}, onEvent)
		r.WallS = since()
		if terr != nil {
			r.Err = strings.TrimSpace(r.Err + " transcribe: " + Code(terr))
			t.Errorf("[%s] Transcribe: %v", spec, terr)
		} else {
			var res struct {
				Sentences []json.RawMessage `json:"sentences"`
			}
			data, _ := os.ReadFile(path)
			_ = json.Unmarshal(data, &res)
			r.Sentences = len(res.Sentences)
			_ = os.Remove(path) // the transcript is not kept
		}
		if m := b.lastMetrics; m != nil && terr == nil {
			r.TranscribeS, r.LoadS = m.transcribeSec, m.setupSec
			r.MemPeakMB, r.MemTotalMB, r.Storage, r.Decision = m.memPeakMB, m.memTotalMB, m.storage(), m.memory
		}
		// The residue check runs on the runtime BEFORE it is released.
		if marker > 0 && terr == nil {
			code := strings.Replace(residueCell, "M = MARKER", "M = "+strconv.FormatFloat(marker, 'f', 3, 64), 1)
			find := strings.ReplaceAll(residueFind, "MARKER", strconv.FormatFloat(marker, 'f', 3, 64))
			fq, _ := json.Marshal(find)
			code = "FIND = " + string(fq) + "\n" + code
			out, err := execCell(ctx, b, code)
			if err != nil {
				t.Errorf("[%s] residue check: %v", spec, err)
				r.Err = strings.TrimSpace(r.Err + " residue: " + err.Error())
			} else {
				r.Residue = json.RawMessage(strings.TrimSpace(out))
			}
		}
		b.mu.Lock()
		var ep string
		if b.sess != nil {
			ep = b.sess.rec.Endpoint
		}
		b.mu.Unlock()
		if err := b.QueueDrained(ctx); err != nil {
			t.Errorf("[%s] release: %v", spec, err)
		}
		st := b.Status(ctx)
		if st.LastRelease != nil {
			r.ReleaseReason = st.LastRelease.Reason
		}
		if list, err := b.tun.listAssignments(ctx, false); err == nil {
			r.AssignmentsAfter = len(list)
			r.EndpointGone = true
			for _, a := range list {
				if ep != "" && a.Endpoint == ep {
					r.EndpointGone = false
				}
			}
		}
		if !r.EndpointGone || st.State != StateReleased {
			t.Errorf("[%s] runtime not released: state %s, endpoint gone %v", spec, st.State, r.EndpointGone)
		}
		if c, err := b.tun.ccuInfo(ctx); err == nil {
			r.BalanceAfter = c.Balance
		}
		b.hookReady = nil
		t.Logf("[%s] cold start %.0fs, transcribe %.0fs, peak %d MB, storage %s, release %s, gone %v, CU %.2f → %.2f",
			spec, r.ColdStartS, r.TranscribeS, r.MemPeakMB, r.Storage, r.ReleaseReason, r.EndpointGone, r.BalanceBefore, r.BalanceAfter)
		report = append(report, r)
		save()
		if terr != nil && os.Getenv("LF_COLAB_KEEP_GOING") != "1" {
			t.Fatalf("[%s] stopping after a failed run", spec)
		}
	}
}
