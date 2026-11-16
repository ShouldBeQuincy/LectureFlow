// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// asrStage runs the job's transcription device (SPEC §6.2, SPEC-v2 I6/I7,
// SPEC-v3 V5): faster-whisper through worker/asr.py, OpenVINO through
// worker/asr_openvino.py or MLX through worker/asr_mlx.py, chosen by the
// backend of the job's ASR snapshot (interpreters and models: asrCall); in test mode it reads asr.fake_result (SPEC §14;
// asr.fake_result_openvino for openvino snapshots when that is set).
type asrStage struct {
	d Deps

	mu   sync.Mutex
	wavs map[string]wavHash // abs path → cached SHA-256
}

type wavHash struct {
	size int64
	mod  time.Time
	sum  string
}

func newASRStage(d Deps) *asrStage { return &asrStage{d: d, wavs: map[string]wavHash{}} }

func (s *asrStage) Name() string                 { return model.StageASR }
func (s *asrStage) Resource() sched.ResourceKind { return sched.ResourceGPU }

// asrJob is job.json of worker/asr.py.
type asrJob struct {
	Audio         string `json:"audio"`
	Model         string `json:"model"`
	ComputeType   string `json:"compute_type"`
	Language      string `json:"language"`
	InitialPrompt string `json:"initial_prompt"`
	Device        string `json:"device"`
	CPUThreads    int    `json:"cpu_threads,omitempty"`
	// Timing asks asr.py for the result's timing block (phase 3b worker
	// protocol: asr.py writes it only when asked, asr_openvino.py always).
	Timing bool `json:"timing,omitempty"`
	// Checkpoint (power management, power-brief §4): pipeline runs only;
	// nil for the self-test, so its job.json is unchanged.
	Checkpoint *devices.CheckpointJob `json:"checkpoint,omitempty"`
	// Redecode is the switch "修复退化的转写窗口" (kv asr.redecode,
	// Deps.redecodeOn): whether asr.py decodes degenerate windows again.
	Redecode bool `json:"redecode"`
}

// ovJob is job.json of worker/asr_openvino.py (SPEC-v2 I7; brief section
// 4). Every path is absolute; CacheDir is "" for the CPU.
type ovJob struct {
	Audio          string `json:"audio"`
	Model          string `json:"model"`
	Precision      string `json:"precision"`
	ModelDir       string `json:"model_dir"`
	Device         string `json:"device"` // GPU | NPU | CPU (model.OVDeviceName)
	Language       string `json:"language"`
	InitialPrompt  string `json:"initial_prompt"`
	CacheDir       string `json:"cache_dir"`
	HFID           string `json:"hf_id"`
	HFHome         string `json:"hf_home"`
	WordTimestamps bool   `json:"word_timestamps"`
	MinFreeBytes   int64  `json:"min_free_bytes"`
	// Checkpoint: as asrJob.Checkpoint (chunk_s = ovCheckpointChunkS).
	Checkpoint *devices.CheckpointJob `json:"checkpoint,omitempty"`
}

// mlxJob is job.json of worker/asr_mlx.py (SPEC-v3 V5): the OpenVINO
// worker's fields and checkpoint contract; Device is model.DeviceMLXGPU.
type mlxJob struct {
	Audio          string                 `json:"audio"`
	Model          string                 `json:"model"`
	Precision      string                 `json:"precision"`
	ModelDir       string                 `json:"model_dir"`
	Device         string                 `json:"device"`
	Language       string                 `json:"language"`
	InitialPrompt  string                 `json:"initial_prompt"`
	WordTimestamps bool                   `json:"word_timestamps"`
	Checkpoint     *devices.CheckpointJob `json:"checkpoint,omitempty"`
}

// ovCheckpointChunkS is devices.OVCheckpointChunkS, the checkpoint chunk_s
// of the OpenVINO worker; tests change it.
var ovCheckpointChunkS = devices.OVCheckpointChunkS

// asrResult is result.json of both workers (and the fake_result files).
type asrResult struct {
	PromptTermsUsed int `json:"prompt_terms_used"`
	Sentences       []struct {
		StartMS      int64               `json:"start_ms"`
		EndMS        int64               `json:"end_ms"`
		Text         string              `json:"text"`
		LowConfWords []model.LowConfWord `json:"low_conf_words"`
		// Words are the word timestamps of SPEC-v2 P4.2; absent in v1
		// results, in old fake files and from the OpenVINO worker when the
		// model gives none.
		Words []model.Word `json:"words"`
	} `json:"sentences"`
	// Timing is the workers' optional timing block (phase 3b); nil when
	// absent.
	Timing *workerTiming `json:"timing"`
	// Parts (power management, power-brief §4): written by the workers
	// whenever job.json has a checkpoint object; two or more = the run was
	// resumed from a checkpoint (stored in the snapshot, §3.8).
	Parts []model.ASRPart `json:"parts"`
}

// workerTiming is result.json's "timing" (seconds and MB).
type workerTiming struct {
	LoadS            float64 `json:"load_s"`
	TranscribeS      float64 `json:"transcribe_s"`
	AudioS           float64 `json:"audio_s"`
	FirstCompile     bool    `json:"first_compile"`
	PeakWorkingSetMB float64 `json:"peak_working_set_mb"`
	// Postprocess (redecode, 2026-10-27): the counters of the worker's steps
	// after decoding; absent when it ran none.
	Postprocess *workerPostprocess `json:"postprocess"`
}

// workerPostprocess is timing.postprocess of result.json.
type workerPostprocess struct {
	// Redecode: worker/asr.py RedecodeStats (counts and seconds; skipped
	// when job.redecode was false).
	Redecode *struct {
		Skipped      bool    `json:"skipped"`
		Windows      int     `json:"windows"`
		Unpunctuated int     `json:"unpunctuated"`
		Uppercase    int     `json:"uppercase"`
		Redecoded    int     `json:"redecoded"`
		Accepted     int     `json:"accepted"`
		RedecodeS    float64 `json:"redecode_s"`
	} `json:"redecode"`
	// TextRepair (textrepair-int): the statistical repair before the
	// sentence split (worker/textrepair.py repair_transcript; every worker).
	TextRepair *workerTextRepair `json:"textrepair"`
	// Split: "极速模式" cut the lecture at silences (colab_remote.py; Colab only).
	Split *model.ASRSplit `json:"split"`
}

// workerTextRepair is the stats of worker/textrepair.py: timing.postprocess.
// textrepair of a transcription, result.json "textrepair" of a "修复断句" run.
type workerTextRepair struct {
	Skipped              string `json:"skipped"`
	Version              int    `json:"version"`
	Words                int    `json:"words"`
	Segments             int    `json:"segments"`
	UnpunctuatedSegments int    `json:"unpunctuated_segments"`
	CapsSegments         int    `json:"caps_segments"`
	SentenceEndsAdded    int    `json:"sentence_ends_added"`
	CommasAdded          int    `json:"commas_added"`
	WordsRecased         int    `json:"words_recased"`
	TrainSentences       int    `json:"train_sentences"`
	KeptSentences        int    `json:"kept_sentences"`
	SentencesIn          int    `json:"sentences_in"`
	SentencesOut         int    `json:"sentences_out"`
	MS                   int64  `json:"ms"`
}

// stats is the repair's counters as job.stats (negative values from a broken
// worker count as 0); nil for nil.
func (r *workerTextRepair) stats() *model.ASRTextRepair {
	if r == nil {
		return nil
	}
	return &model.ASRTextRepair{
		Skipped: r.Skipped, Version: max(r.Version, 0), Words: max(r.Words, 0), Segments: max(r.Segments, 0),
		UnpunctuatedSegments: max(r.UnpunctuatedSegments, 0), CapsSegments: max(r.CapsSegments, 0),
		SentenceEndsAdded: max(r.SentenceEndsAdded, 0), CommasAdded: max(r.CommasAdded, 0),
		WordsRecased: max(r.WordsRecased, 0), TrainSentences: max(r.TrainSentences, 0),
		KeptSentences: max(r.KeptSentences, 0), SentencesIn: max(r.SentencesIn, 0), SentencesOut: max(r.SentencesOut, 0),
		MS: max(r.MS, 0),
	}
}

// stats is the timing as job.stats.asr_timing (ms and MB); nil for nil.
func (t *workerTiming) stats() *model.ASRTiming {
	if t == nil {
		return nil
	}
	ms := func(s float64) int64 { return int64(math.Round(max(s, 0) * 1000)) }
	out := &model.ASRTiming{LoadMS: ms(t.LoadS), TranscribeMS: ms(t.TranscribeS), AudioMS: ms(t.AudioS),
		FirstCompile: t.FirstCompile, PeakMemMB: int(math.Round(max(t.PeakWorkingSetMB, 0)))}
	if p := t.Postprocess; p != nil && (p.Redecode != nil || p.TextRepair != nil || p.Split != nil) {
		out.Postprocess = &model.ASRPostprocess{TextRepair: p.TextRepair.stats(), Split: p.Split.Checked()}
		if r := p.Redecode; r != nil {
			out.Postprocess.Redecode = &model.ASRRedecode{
				Skipped: r.Skipped, Windows: max(r.Windows, 0), Unpunctuated: max(r.Unpunctuated, 0), Uppercase: max(r.Uppercase, 0),
				Redecoded: max(r.Redecoded, 0), Accepted: max(r.Accepted, 0), RedecodeMS: ms(r.RedecodeS),
			}
		}
	}
	return out
}

// ASRHashVersion is stats.hash_version of new asr runs: input_hash = wav +
// language (SPEC-v2 I6 revising A1; ctxbuild.ASRInputHashV3). Jobs of
// version 2 (SPEC-v1.1 A1: wav + model, compute_type, language) keep their
// formula; older ones are never compared.
const ASRHashVersion = 3

// asrHashVersionV11 is the version of the v1.1 formula (SPEC-v1.1 A1, lead
// ruling 3).
const asrHashVersionV11 = 2

// usedPrompt is the part of an initial_prompt the worker actually used: its
// first n items, split exactly like the worker does (", " separated, blank
// items dropped). n <= 0 = nothing was used.
func usedPrompt(prompt string, n int) string {
	if n <= 0 {
		return ""
	}
	var items []string
	for _, it := range strings.Split(prompt, ", ") {
		if it = strings.TrimSpace(it); it != "" {
			items = append(items, it)
		}
	}
	if n >= len(items) {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ")
}

// hashParams are the ASR settings of the v1.1 input hash (hash_version 2),
// from the job's ASR snapshot as stored (A1: model, compute_type,
// language). compute_type is the configured value (e.g. "auto"), not the
// per-run resolution: a transient nvidia-smi failure must not make every
// lecture look stale.
func hashParams(snap model.ASRSnapshot) ctxbuild.ASRParams {
	return ctxbuild.ASRParams{Model: snap.Model, ComputeType: snap.ComputeType, Language: snap.Language}
}

// InputHash uses the formula the current asr job was recorded with:
// version 3 (SPEC-v2 I6) = wav hash + the snapshot's language, so neither a
// device nor a model change ever makes asr stale (a rerun on another device
// goes through the A4.3 decision instead); version 2 (SPEC-v1.1 A1) = wav
// hash + the snapshot's model, compute_type and language. The prompt is
// part of neither, so a glossary change never makes asr stale, and the
// settings are never read. A job recorded with an older formula
// (stats.hash_version < 2, v1 runs) or without a snapshot reports "" and is
// never compared.
func (s *asrStage) InputHash(ctx context.Context, lectureID string) (string, error) {
	j, err := s.d.Store.GetCurrentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if errors.Is(err, db.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	snap, ok := model.ParseASRSnapshot(j.ConfigSnapshot)
	if j.Stats.HashVersion < asrHashVersionV11 || !ok {
		return "", nil
	}
	l, err := s.d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return "", err
	}
	wav, err := s.wavSHA256(filepath.Join(s.d.lectureDir(l), model.AudioFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil // no audio (prep rerun): nothing to compare
	}
	if err != nil {
		return "", err
	}
	if j.Stats.HashVersion >= ASRHashVersion {
		return ctxbuild.ASRInputHashV3(wav, snap.Language), nil
	}
	return ctxbuild.ASRInputHash(wav, hashParams(snap)), nil
}

// wavSHA256 hashes the file, cached by (path, size, mtime).
func (s *asrStage) wavSHA256(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(path)
	s.mu.Lock()
	c, ok := s.wavs[abs]
	s.mu.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	s.wavs[abs] = wavHash{size: fi.Size(), mod: fi.ModTime(), sum: sum}
	s.mu.Unlock()
	return sum, nil
}

// Run transcribes the lecture (run). A run whose context the scheduler
// cancelled with a power-policy cause (sched.ErrPowerMigrate /
// ErrPowerSuspend, power-brief §3.4) returns an error wrapping that cause,
// so sched.IsPowerInterrupt holds whatever step the cancel interrupted; the
// checkpoint is kept for the next run of the same job, like after any other
// cancel.
func (s *asrStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	err := s.run(ctx, lectureID, progress)
	if err != nil && ctx.Err() != nil && !sched.IsPowerInterrupt(err) {
		if cause := context.Cause(ctx); sched.IsPowerInterrupt(cause) {
			return fmt.Errorf("%w: asr stopped (%v)", cause, err)
		}
	}
	return err
}

func (s *asrStage) run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	// The run parameters come from the job's ASR snapshot (v1.1 A4.1, lead
	// ruling 4); GPU limits stay with the scheduler's current settings.
	job, err := d.currentJob(ctx, model.ScopeLecture, lectureID, model.StageASR)
	if err != nil {
		return err
	}
	if model.IsTextRepairJob(job) {
		// "修复断句": the stored transcript is repaired, nothing is
		// transcribed (asr_textrepair.go; the scheduler runs it on the CPU).
		return s.textRepairRun(ctx, job, lectureID, progress)
	}
	snap, ok := model.ParseASRSnapshot(job.ConfigSnapshot)
	if !ok {
		return sched.Permanent(model.Errorf(CodeSnapshotMissing, nil, errors.New(MissingSnapshot)))
	}
	// A v1.1 snapshot runs on cuda:0 / fw:cpu (I6, lead ruling 2); the
	// device of an auto snapshot was bound by the scheduler when the job
	// started (lead ruling 1).
	run := model.NormalizeASRSnapshot(snap)
	// SPEC-v3 V14: a colab snapshot runs on the remote backend (asr_colab.go).
	remote := run.Backend == model.BackendColab && run.DeviceID == model.DeviceColab
	ssh := model.IsSSHDevice(run.DeviceID) // ssh-mvp: an SSH inference server (asr_remote.go)
	if !remote && !ssh {
		if err := d.checkASRDevice(ctx, run); err != nil {
			return err
		}
	}
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(d.lectureDir(l))
	if err != nil {
		return err
	}
	audio := filepath.Join(dir, model.AudioFile)
	if _, err := os.Stat(audio); err != nil {
		return sched.Permanent(model.Errorf(CodeASRAudioMissing, map[string]any{"file": model.AudioFile},
			fmt.Errorf("找不到 %s（预处理的产物），请重新运行预处理阶段", audio)))
	}
	prompt, glossaryVersion, err := s.asrPrompt(ctx, l)
	if err != nil {
		return err
	}

	sched.UpdateStats(ctx, func(st *model.JobStats) { st.ASRTiming = nil }) // set again when this attempt succeeds
	var raw []byte
	fromWorker := false
	// prior: the transcription time of the earlier attempts this run builds
	// on (stats.asr_prior_transcribe_ms) when it continues the checkpoint
	// at resumeMS; both 0 when the worker starts over (asrTimingResume).
	var prior, resumeMS int64
	if remote {
		// Colab (V14.3): the same result.json as the local faster-whisper
		// worker; the local checkpoint is not continued remotely.
		raw, err = s.remoteRun(ctx, job, run, dir, audio, prompt, progress)
	} else if ssh {
		raw, err = s.sshRun(ctx, job, run, dir, audio, prompt, progress)
	} else if fake := d.Cfg.ASR.FakeResult; fake != "" {
		if run.Backend == model.BackendOpenVINO && d.Cfg.ASR.FakeResultOpenVINO != "" {
			fake = d.Cfg.ASR.FakeResultOpenVINO
		}
		raw, err = s.fakeRun(ctx, dir, fake, progress)
	} else {
		kept, ok := discardForeignCheckpoint(job, run, dir)
		var ckpt *devices.CheckpointJob
		if ckpt, err = s.checkpointJob(job, run, dir, audio); err != nil {
			return err
		}
		if ok {
			resumeMS = continuedFrom(kept, *ckpt, run.Backend)
		}
		if resumeMS > 0 {
			prior = job.Stats.ASRPriorTranscribeMS
		}
		clock := newAttemptClock(ctx, prior, job.Stats.ASRPriorTranscribeMS)
		raw, err = s.workerRun(ctx, dir, audio, prompt, run, ckpt, clock.wrap(progress))
		clock.exited()
		fromWorker = true
	}
	if err != nil {
		return err
	}
	var res asrResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return model.Errorf(CodeASRResultUnparsable, nil, fmt.Errorf("转写结果 result.json 无法解析：%w", err))
	}
	ss := make([]model.Sentence, 0, len(res.Sentences))
	for _, r := range res.Sentences {
		text := strings.TrimSpace(r.Text)
		if text == "" {
			continue
		}
		var words []model.Word // nil = no word timestamps: sentence.words stays NULL (P4.2)
		if len(r.Words) > 0 {
			words = r.Words
		}
		ss = append(ss, model.Sentence{
			LectureID: lectureID, Idx: len(ss), StartMS: r.StartMS, EndMS: max(r.EndMS, r.StartMS),
			ASRText: text, LowConfWords: r.LowConfWords, RepairedText: text, ZhStatus: model.ZhNone,
			Words: words,
		})
	}
	if len(ss) == 0 {
		return sched.Permanent(model.Errorf(CodeASRNoSpeech, nil,
			errors.New("未识别到任何语音：音频可能是静音、没有讲话或音量过低。请确认上传的是正确的录音，然后重新运行预处理")))
	}
	// One transaction for all sentences (SPEC §6.2). Leftovers of an attempt
	// interrupted after a previous commit are removed first. A run resumed
	// from a checkpoint (two parts or more) records its parts in the
	// snapshot in the same transaction (power-brief §3.8); with fewer the
	// snapshot stays byte for byte.
	err = d.Store.Tx(ctx, func(tx db.Store) error {
		if err := tx.DeletePatches(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.DeleteSummary(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.DeleteSentences(ctx, lectureID); err != nil {
			return err
		}
		if err := tx.InsertSentences(ctx, lectureID, ss); err != nil { // runs RecomputeText in the same transaction
			return err
		}
		if fromWorker {
			return state.RecordASRParts(ctx, tx, job.ID, res.Parts)
		}
		return nil
	})
	if err != nil {
		return err
	}
	d.events().Changed("sentences", lectureID)
	// The transcription is stored: its checkpoint has served (power-brief
	// §4: only a successful run removes it; cancels and power interrupts
	// keep it for the next run of the same job).
	if fromWorker || remote || ssh {
		removeCheckpoint(dir)
	}

	// Record the prompt the worker actually used (A1) and its timing (phase
	// 3b; a run resumed from the checkpoint reports the whole lecture,
	// asrTimingResume), and write the v3 input hash: the scheduler computed
	// none at the start, because a new job has no hash_version yet.
	n := res.PromptTermsUsed
	used := usedPrompt(prompt, n)
	timing := res.Timing.stats()
	if fromWorker && timing != nil && prior > 0 {
		if whole, ok := resumedAudioMS(res.Parts, timing.AudioMS, resumeMS); ok {
			timing.TranscribeMS += prior
			timing.AudioMS = whole
		}
	}
	sched.UpdateStats(ctx, func(st *model.JobStats) {
		st.PromptTermsUsed = n
		st.ASRPrompt = used
		st.GlossaryVersion = &glossaryVersion
		st.HashVersion = ASRHashVersion
		st.Status = ""
		st.ASRTiming = timing
		st.ASRPriorTranscribeMS = 0 // folded into the timing above, or void
	})
	if id := jobID(ctx); id != "" {
		wav, err := s.wavSHA256(audio)
		if err != nil {
			return err
		}
		h := ctxbuild.ASRInputHashV3(wav, run.Language)
		if err := d.Store.SetJobInputHash(context.WithoutCancel(ctx), id, h); err != nil {
			return err
		}
	}
	// The observed memory peak calibrates the admission (I8, lead ruling 8;
	// since power management also the faster-whisper workers', for the
	// worker memory budget, power-brief §3.7); only real worker runs count,
	// not the test fakes (fake_result).
	if fromWorker && timing != nil && timing.PeakMemMB > 0 {
		if err := devices.RecordObservedMem(context.WithoutCancel(ctx), d.Store, run.DeviceID, run.Model, run.Precision, timing.PeakMemMB); err != nil {
			slog.Warn("asr: recording the observed memory peak", "device", run.DeviceID, "model", run.Model,
				"precision", run.Precision, "err", err)
		}
	}
	progress(1)
	return nil
}

// asrPrompt is the initial_prompt of lecture l's transcription (the course's
// terms, ", " joined in priority order) and the course's glossary version.
// The prompt still comes from the glossary of the moment (A1); terms and
// glossary version are read together. The order of the prompt depends on
// the text of the materials bound to this lecture (SPEC-v2 G4) and, since
// lecture kinds, to its teaching week (lead ruling for K2: the lecture's
// material set without the course-wide part). The prompt is not part of
// the input hash, so this never makes asr stale. A "修复断句" run takes its
// glossary from it too (textrepair-int).
func (s *asrStage) asrPrompt(ctx context.Context, l model.Lecture) (string, int, error) {
	var (
		terms           []model.Term
		mats            []model.Material
		week            int
		glossaryVersion int
	)
	err := s.d.Store.Tx(ctx, func(tx db.Store) error {
		c, err := tx.GetCourse(ctx, l.CourseID)
		if err != nil {
			return err
		}
		glossaryVersion = c.GlossaryVersion
		cur, err := tx.GetLecture(ctx, l.ID) // recorded_at as of now
		if err != nil {
			return err
		}
		week = state.LectureWeek(c, cur)
		if terms, err = tx.ListTerms(ctx, l.CourseID); err != nil {
			return err
		}
		mats, err = tx.ListMaterials(ctx, l.CourseID)
		return err
	})
	if err != nil {
		return "", 0, err
	}
	return ctxbuild.BuildASRPrompt(terms, ctxbuild.BoundMaterialTextWeek(mats, l.ID, week)), glossaryVersion, nil
}

// fakeRun copies asr.fake_result through result.json.tmp → result.json (SPEC §14).
func (s *asrStage) fakeRun(ctx context.Context, dir, fake string, progress func(float64)) ([]byte, error) {
	b, err := os.ReadFile(fake)
	if err != nil {
		return nil, sched.Permanent(model.Errorf(CodeASRFakeUnreadable, map[string]any{"path": fake}, fmt.Errorf("asr.fake_result %q 无法读取：%w", fake, err)))
	}
	for _, p := range []float64{0.25, 0.5, 0.75} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		progress(p)
	}
	out := filepath.Join(dir, "result.json")
	if err := writeFileAtomic(out, b); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// workerRun runs the snapshot's worker (asrCall) with stats.status following
// its STATUS lines (asrStatusHooks).
func (s *asrStage) workerRun(ctx context.Context, dir, audio, prompt string, a model.ASRSnapshot, ckpt *devices.CheckpointJob,
	progress func(float64)) ([]byte, error) {
	// With the component environment, a model that is not installed is an
	// error, never a download into the global cache (V12.2 acceptance #3).
	if err := s.d.checkASRModel(a); err != nil {
		return nil, err
	}
	w, err := s.d.asrCall(ctx, a, dir, audio, prompt, ckpt)
	if err != nil {
		return nil, err
	}
	w.progress, w.status = asrStatusHooks(ctx, progress)
	return s.d.runWorker(ctx, w)
}

// checkpointJob is the job.json "checkpoint" object of a pipeline run of
// job (power-brief §4): the lecture's {dir}/asr.ckpt.json, the job's id
// (a retry after a cancel and the §4.7 recovery run the same job, so the
// worker may continue; a rerun is a new job and starts over), the v3 input
// hash (a prep rerun changes the wav), the run's device, model and
// precision (as in the snapshot), and chunk_s (ovCheckpointChunkS for
// OpenVINO, 0 for faster-whisper). Whether a file the stage kept
// (discardForeignCheckpoint) is continued is the worker's decision.
func (s *asrStage) checkpointJob(job model.Job, a model.ASRSnapshot, dir, audio string) (*devices.CheckpointJob, error) {
	wav, err := s.wavSHA256(audio)
	if err != nil {
		return nil, err
	}
	chunk := 0
	if a.Backend == model.BackendOpenVINO || a.Backend == model.BackendMLX {
		chunk = ovCheckpointChunkS // the MLX worker chunks like the OpenVINO one
	}
	return &devices.CheckpointJob{
		Path: filepath.Join(dir, model.ASRCheckpointFile), JobID: job.ID, InputHash: ctxbuild.ASRInputHashV3(wav, a.Language),
		DeviceID: a.DeviceID, Model: a.Model, Precision: a.Precision, ChunkS: chunk,
	}, nil
}

// checkpointResumable reports whether the asr run with the (normalized,
// bound) snapshot run may continue from checkpoint ck, given the job's
// stats st (power management backend Gate round 1, item 4). The worker only
// compares job id, input hash and backend, and a retry after a cancel is
// the same job, so without this a lecture would mix two configurations
// although the user changed the device row's model or precision (an auto
// snapshot binds the row's current values at every start). A configuration
// change inside one lecture is allowed only for a power migration (lead
// ruling on Gate round 1 item 4). The checkpoint is continued when
//   - st.ASRResume is set (the last run was a power interrupt) and this run
//     is bound to another device than the checkpoint's last part: the
//     policy moved the lecture, so model and precision may differ too (e.g.
//     Intel GPU int8 → NPU fp16, power requirement 4a; every migration
//     targets another device than the one interrupted, so a chain GPU →
//     NPU → GPU continues as well); or
//   - the checkpoint's last part ran with exactly this run's device id,
//     model and precision (a retry after a cancel on the same
//     configuration, §4.7 recovery, a suspended CUDA job resumed on AC).
//
// On the same device another model or precision can only come from the
// user (the device row edited, a self-test recommendation applied), so it
// starts over even after a power interrupt (asr_resume survives a user
// cancel). A checkpoint without parts names no device and is not
// continued.
func checkpointResumable(st model.JobStats, run model.ASRSnapshot, ck devices.Checkpoint) bool {
	if len(ck.Parts) == 0 {
		return false
	}
	last := ck.Parts[len(ck.Parts)-1]
	if st.ASRResume != nil && last.DeviceID != run.DeviceID {
		return true
	}
	return last.DeviceID == run.DeviceID && last.Model == run.Model && last.Precision == run.Precision
}

// discardForeignCheckpoint runs before the worker of a pipeline run starts:
// when the lecture directory dir holds a checkpoint that checkpointResumable
// does not allow job to continue with run, it removes the file and a partial
// .tmp (removeCheckpoint) and logs it, so the worker starts over. A missing
// file, or one that cannot be read or parsed as a devices.Checkpoint, is left
// to the worker's own rules (it deletes what it cannot continue); a kept file
// is still subject to them (version, job id, input hash, backend). It
// returns the checkpoint it kept, ok false when there is none (missing,
// unreadable or removed).
func discardForeignCheckpoint(job model.Job, run model.ASRSnapshot, dir string) (devices.Checkpoint, bool) {
	b, err := os.ReadFile(filepath.Join(dir, model.ASRCheckpointFile))
	if err != nil {
		return devices.Checkpoint{}, false
	}
	var ck devices.Checkpoint
	if json.Unmarshal(b, &ck) != nil {
		return devices.Checkpoint{}, false
	}
	if checkpointResumable(job.Stats, run, ck) {
		return ck, true
	}
	var last model.ASRPart
	if n := len(ck.Parts); n > 0 {
		last = ck.Parts[n-1]
	}
	slog.Info("asr: checkpoint not continued (its last part ran with another model or precision on this device, or on another device without a power interrupt); starting over",
		"job", job.ID, "lecture", job.ScopeID, "checkpoint_job", ck.JobID, "power_interrupt", job.Stats.ASRResume != nil,
		"checkpoint_device", last.DeviceID, "checkpoint_model", last.Model, "checkpoint_precision", last.Precision,
		"device", run.DeviceID, "model", run.Model, "precision", run.Precision)
	removeCheckpoint(dir)
	return devices.Checkpoint{}, false
}

// asrTimingResume (power management real-machine verification,
// docs/benchmarks/power-checkpoint.md §6.8 item 1): a worker reports the
// timing of its own run only, so after a resume from the checkpoint
// job.stats.asr_timing would show the last attempt's transcription time and
// audio (e.g. 2:00 of 33:31 for a 54:47 lecture). The stage therefore
//   - measures every pipeline worker run (attemptClock) and keeps
//     job.stats.asr_prior_transcribe_ms = the time of the attempts so far
//     whose checkpoint the next attempt continues; an attempt that starts
//     over (continuedFrom = 0: no checkpoint it can continue) begins the
//     sum again at 0;
//   - when a run that continued the checkpoint succeeds (resumedAudioMS),
//     adds that sum to asr_timing.transcribe_ms and reports the whole
//     lecture (the span of the result's parts) as asr_timing.audio_ms.
//     load_ms, first_compile and peak_mem_mb stay the last attempt's: they
//     describe the device that finished (the hover card's "首次在这台设备上
//     使用" and the observed memory of run.DeviceID), and a killed worker
//     reports no load time (Go only sees its first PROGRESS line, which for
//     faster-whisper also comes after Python start-up, audio decoding, VAD
//     and the first window).
// An uninterrupted run's asr_timing is the worker's, byte for byte, and the
// successful run removes asr_prior_transcribe_ms from the stats. A run
// whose earlier time is unknown (prior 0: an attempt interrupted before
// this fix, or a server that died before any flush) keeps its own pair, so
// the RTF stays right.

// asrNow is the clock of attemptClock (tests replace it).
var asrNow = time.Now

// priorSaveEvery is how often a running attempt's measured time goes into
// the in-memory stats (the scheduler flushes them), so a server that dies
// keeps most of it; the worker's exit always writes it.
const priorSaveEvery = 5 * time.Second

// attemptClock measures the transcription time of one pipeline worker run
// on the Go side: from its first PROGRESS line (the workers print it once
// the model is loaded) to its exit. It writes base + that time into
// stats.asr_prior_transcribe_ms (at most every priorSaveEvery while the
// worker runs, and when it exits); a successful run replaces the value.
type attemptClock struct {
	ctx  context.Context
	base int64

	mu    sync.Mutex
	first time.Time // zero until the first PROGRESS line
	saved time.Time
}

// newAttemptClock starts the measurement of an attempt that builds on base
// ms of earlier attempts; stored is the stats' current value (rewritten when
// it differs, i.e. the sum begins again at 0).
func newAttemptClock(ctx context.Context, base, stored int64) *attemptClock {
	if base != stored {
		sched.UpdateStats(ctx, func(st *model.JobStats) { st.ASRPriorTranscribeMS = base })
	}
	return &attemptClock{ctx: ctx, base: base}
}

// wrap returns progress with the clock ticking on every PROGRESS line.
func (c *attemptClock) wrap(progress func(float64)) func(float64) {
	return func(p float64) {
		c.tick(false)
		progress(p)
	}
}

// exited records the worker's exit (whatever its outcome).
func (c *attemptClock) exited() { c.tick(true) }

func (c *attemptClock) tick(exited bool) {
	now := asrNow()
	c.mu.Lock()
	if c.first.IsZero() {
		if !exited {
			c.first, c.saved = now, now
		}
		c.mu.Unlock()
		return // nothing transcribed yet: the stats keep base
	}
	if !exited && now.Sub(c.saved) < priorSaveEvery {
		c.mu.Unlock()
		return
	}
	c.saved = now
	v := c.base + max(now.Sub(c.first), 0).Milliseconds()
	c.mu.Unlock()
	sched.UpdateStats(c.ctx, func(st *model.JobStats) { st.ASRPriorTranscribeMS = v })
}

// continuedFrom is where the worker will continue the kept checkpoint ck
// (its resume_ms) for the job.json checkpoint object cj and the run's
// backend, or 0 when it starts over: the worker's rules Go can check
// (version, job id, input hash, backend) plus a part and a resume point. The
// faster-whisper worker also compares its version and VAD options; a run
// that started over for that reason is caught by resumedAudioMS.
func continuedFrom(ck devices.Checkpoint, cj devices.CheckpointJob, backend string) int64 {
	if ck.Version != devices.CheckpointVersion || ck.JobID != cj.JobID || ck.InputHash != cj.InputHash ||
		ck.Backend != backend || len(ck.Parts) == 0 || ck.ResumeMS <= 0 {
		return 0
	}
	return ck.ResumeMS
}

// resumedAudioMS is the lecture's audio as a result covers it (the span of
// its parts) when the worker continued from the checkpoint at resumeMS:
// then its timing.audio_s is only what follows (span - resumeMS, within the
// 10 ms rounding of audio_s), while a run that started over reports about
// the span; half the resume point tells them apart. ok is false without
// parts or resume point, or when the run started over.
func resumedAudioMS(parts []model.ASRPart, audioMS, resumeMS int64) (int64, bool) {
	if len(parts) == 0 || resumeMS <= 0 {
		return 0, false
	}
	span := parts[len(parts)-1].ToMS - parts[0].FromMS
	if span-audioMS < resumeMS/2 {
		return 0, false
	}
	return span, true
}

// removeCheckpoint deletes the checkpoint of the lecture directory dir and
// a partial one a killed worker may have left (a failure is only logged: a
// stale file is void for the next job anyway, its job id differs).
func removeCheckpoint(dir string) {
	for _, name := range []string{model.ASRCheckpointFile, model.ASRCheckpointFile + ".tmp"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("asr: removing the checkpoint", "file", filepath.Join(dir, name), "err", err)
		}
	}
}

// asrStatusHooks returns the progress and status callbacks of an asr worker
// run: a transient status (model.ASRStatus*: downloading_model of
// faster-whisper, preparing_model / compiling_model of OpenVINO) is shown in
// stats.status until the next PROGRESS line, which the workers print only
// once the model is loaded.
func asrStatusHooks(ctx context.Context, progress func(float64)) (func(float64), func(string)) {
	var mu sync.Mutex
	transient := false
	onProgress := func(p float64) {
		mu.Lock()
		was := transient
		transient = false
		mu.Unlock()
		if was {
			sched.UpdateStats(ctx, func(st *model.JobStats) { st.Status = "" })
		}
		progress(p)
	}
	onStatus := func(v string) {
		switch v {
		case model.ASRStatusDownloadingModel, model.ASRStatusPreparingModel, model.ASRStatusCompilingModel:
		default:
			return
		}
		mu.Lock()
		transient = true
		mu.Unlock()
		sched.UpdateStats(ctx, func(st *model.JobStats) { st.Status = v })
	}
	return onProgress, onStatus
}

// checkASRDevice fails permanently (CodeASRDeviceMissing {device}) when a
// normalized snapshot names a device this build cannot run: an unknown id,
// or a backend that is not the id's.
func (d Deps) checkASRDevice(ctx context.Context, a model.ASRSnapshot) error {
	if runnableDevice(a.DeviceID, a.Backend) {
		return nil
	}
	return sched.Permanent(model.Errorf(CodeASRDeviceMissing, map[string]any{"device": d.deviceLabel(ctx, a.DeviceID)},
		fmt.Errorf("快照中的转写设备 %q（后端 %q）无法在本程序中运行，请重跑并选择使用当前设置", a.DeviceID, a.Backend)))
}

// runnableDevice reports whether this build runs device id with backend:
// a known id of that backend (model.FWDevice / model.OVDeviceName).
func runnableDevice(id, backend string) bool {
	if model.DeviceBackend(id) != backend {
		return false
	}
	switch backend {
	case model.BackendFasterWhisper:
		return model.FWDevice(id) != ""
	case model.BackendOpenVINO:
		return model.OVDeviceName(id) != ""
	case model.BackendMLX:
		return id == model.DeviceMLXGPU
	}
	return false // colab: the remote backend (asr_colab.go), never a local worker or a self-test
}

// deviceLabel is the device param of the asr errors: the row's label, or
// the id when there is no row.
func (d Deps) deviceLabel(ctx context.Context, id string) string {
	if dev, err := d.Store.GetASRDevice(context.WithoutCancel(ctx), id); err == nil && dev.Label != "" {
		return dev.Label
	}
	return id
}

// asrCall is the worker invocation that transcribes audio with the
// (normalized, checked) snapshot a in dir: worker/asr.py for
// faster-whisper, worker/asr_openvino.py for OpenVINO (brief section 4),
// worker/asr_mlx.py for MLX (SPEC-v3 V5). The interpreter of each is the
// backend's component environment when installed, else its configured
// path (config.Config.WorkerPython: python, python_openvino, python_mlx);
// the model is the model component when installed (devices.FWModel,
// OVModelPath, MLXModelPath), else the earlier location (SPEC-v3 step 5).
// The snapshot keeps the model name: the asr input hash never includes
// model or device (SPEC-v2 I6). ckpt is job.json's checkpoint object (nil
// = none: the self-test). Progress and status are the caller's.
func (d Deps) asrCall(ctx context.Context, a model.ASRSnapshot, dir, audio, prompt string, ckpt *devices.CheckpointJob) (workerCall, error) {
	return d.asrCallAt(ctx, a, dir, audio, prompt, ckpt, nil)
}

// workerLoc overrides where an asr worker call finds its interpreter and
// model: the component update self-test (SPEC-v3 V8) runs the new version
// before it is active. modelDir "" keeps the default model location.
type workerLoc struct {
	python   string
	modelDir string
}

// asrCallAt is asrCall with loc (nil = the active components, else the
// configured paths).
func (d Deps) asrCallAt(ctx context.Context, a model.ASRSnapshot, dir, audio, prompt string, ckpt *devices.CheckpointJob, loc *workerLoc) (workerCall, error) {
	python, key, _ := d.Cfg.WorkerPython(a.Backend)
	if loc != nil {
		python = loc.python
	}
	modelDir := func(def string) string {
		if loc != nil && loc.modelDir != "" {
			return loc.modelDir
		}
		return def
	}
	if a.Backend == model.BackendOpenVINO {
		dataDir, err := filepath.Abs(d.DataDir)
		if err != nil {
			return workerCall{}, err
		}
		dev := model.OVDeviceName(a.DeviceID)
		cache := ""
		if dev != "CPU" {
			cache = devices.CacheDir(dataDir, dev, a.Model, a.Precision)
		}
		job := ovJob{
			Audio: audio, Model: a.Model, Precision: a.Precision,
			ModelDir: modelDir(devices.OVModelPath(d.Cfg, dataDir, a.Model, a.Precision)),
			Device:   dev, Language: a.Language, InitialPrompt: prompt, CacheDir: cache,
			HFID: devices.HFID(a.Model), HFHome: devices.HFHome(dataDir), WordTimestamps: true,
			MinFreeBytes: devices.PrepareMinFreeBytes, Checkpoint: ckpt,
		}
		return workerCall{
			name: "asr_openvino", label: "转写进程（asr_openvino.py）", code: workerASROV, dir: dir, job: job, out: "result.json",
			python: python, pythonCode: devices.CodeOVPythonMissing, pythonKey: key,
			errorCode: d.ovErrorCode(ctx, a),
		}, nil
	}
	if a.Backend == model.BackendMLX {
		dataDir, err := filepath.Abs(d.DataDir)
		if err != nil {
			return workerCall{}, err
		}
		job := mlxJob{
			Audio: audio, Model: a.Model, Precision: a.Precision,
			ModelDir: modelDir(devices.MLXModelPath(d.Cfg, dataDir, a.Model, a.Precision)),
			Device:   a.DeviceID, Language: a.Language, InitialPrompt: prompt, WordTimestamps: true, Checkpoint: ckpt,
		}
		return workerCall{
			name: "asr_mlx", label: "转写进程（asr_mlx.py）", code: workerASRMLX, dir: dir, job: job, out: "result.json",
			python: python, pythonCode: devices.CodeMLXPythonMissing, pythonKey: key,
			errorCode: d.mlxErrorCode(ctx, a),
		}, nil
	}
	total := 0
	if d.GPUTotalMB != nil {
		total = d.GPUTotalMB(ctx)
	}
	dev := model.FWDevice(a.DeviceID)
	job := asrJob{
		Audio: audio, Model: modelDir(devices.FWModel(d.Cfg, a.Model, a.Precision)),
		ComputeType: config.ResolveComputeType(a.Precision, dev, total),
		Language:    a.Language, InitialPrompt: prompt, Device: dev, Timing: true, Checkpoint: ckpt,
		Redecode: d.redecodeOn(ctx),
	}
	if dev == "cuda" {
		job.CPUThreads = 4 // SPEC §17.3; cpu mode: the worker uses physical cores - 1
	}
	return workerCall{name: "asr", label: "转写进程（asr.py）", code: workerASR, dir: dir, job: job, out: "result.json",
		python: python, pythonKey: key}, nil
}

// redecodeOn reads the switch "修复退化的转写窗口" (model.KVASRRedecode:
// on unless the user switched it off). It is read for every faster-whisper
// run (pipeline, self-test, Colab), so a change applies to the next
// transcription; it is no part of the input hash. A kv that cannot be read
// keeps the default.
func (d Deps) redecodeOn(ctx context.Context) bool {
	if d.Store == nil {
		return true
	}
	v, ok, err := d.Store.GetKV(context.WithoutCancel(ctx), model.KVASRRedecode)
	if err != nil {
		slog.Warn("asr: reading the redecode switch; it stays on", "err", err)
		return true
	}
	return model.RedecodeEnabled(v, ok)
}

// mlxErrorCode maps the MLX worker's ERROR_CODE names (NOTES "决定（v3
// v5-mlx-worker）" 14: only device_unavailable) to the asr.mlx_* job
// errors.
func (d Deps) mlxErrorCode(ctx context.Context, a model.ASRSnapshot) func(name, raw string) (string, map[string]any) {
	return func(name, _ string) (string, map[string]any) {
		if name == "device_unavailable" {
			return CodeASRMLXDeviceUnavailable, map[string]any{"device": d.deviceLabel(ctx, a.DeviceID)}
		}
		return "", nil
	}
}

// ovErrorCode maps the OpenVINO worker's ERROR_CODE names (brief section
// 4) to the asr.ov_* job errors.
func (d Deps) ovErrorCode(ctx context.Context, a model.ASRSnapshot) func(name, raw string) (string, map[string]any) {
	return func(name, raw string) (string, map[string]any) {
		switch name {
		case "device_unavailable":
			return CodeASROVDeviceUnavailable, map[string]any{"device": d.deviceLabel(ctx, a.DeviceID)}
		case "model_prepare_failed":
			return CodeASROVModelPrepareFailed, map[string]any{"model": a.Model, "precision": a.Precision}
		case "no_space":
			var v struct {
				Free int64 `json:"free_bytes"`
				Need int64 `json:"need_bytes"`
			}
			_ = json.Unmarshal([]byte(raw), &v)
			if v.Need <= 0 {
				v.Need = devices.PrepareMinFreeBytes
			}
			return CodeASROVNoSpace, map[string]any{"free_bytes": v.Free, "need_bytes": v.Need}
		}
		return "", nil
	}
}
