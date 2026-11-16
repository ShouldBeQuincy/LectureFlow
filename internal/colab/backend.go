// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// Options configure a Backend. Zero values take the defaults noted.
type Options struct {
	// KV stores the credential, the session record, settings and rates
	// (required; db.Store in the app, MemKV in tests).
	KV KV
	// HTTPClient for the OAuth, tunnel and runtime requests (nil = a client
	// without a global timeout; every request has its own).
	HTTPClient *http.Client
	// UploadClient carries the audio uploads to the runtime (nil: a client
	// with a transport of its own when HTTPClient is nil too, else
	// HTTPClient): small requests do not wait behind an upload.
	UploadClient *http.Client
	// Dial opens the kernel channels (nil = DialWebSocket(nil)).
	Dial Dialer
	// ColabURL, TokenURL, RevokeURL: the services (defaults: upstream's
	// production URLs; tests point them at the replay server).
	ColabURL, TokenURL, RevokeURL string
	// ADCPath: where ImportADC reads gcloud's credentials ("" = ADCPath()).
	ADCPath string
	// WorkerDir holds colab_remote.py, asr.py and asr_common.py (required
	// for Transcribe).
	WorkerDir string
	// FFmpeg encodes the upload to FLAC ("" = upload the WAV as it is).
	FFmpeg    string
	RunFFmpeg FFmpegRunner // nil = run the ffmpeg binary
	Logger    *slog.Logger
	Now       func() time.Time
	// Sleep waits d or until ctx ends (tests advance a fake clock).
	Sleep func(ctx context.Context, d time.Duration) error
	// RunID identifies this run of the app ("" = random): a credential with
	// the session lifecycle belongs to one run.
	RunID string
	// ClientAgent is X-Colab-Client-Agent ("" = "lectureflow").
	ClientAgent string
	// AllowHTTP lets the runtime proxy URL be http:// (tests only).
	AllowHTTP bool
	// ChunkSize of uploads (0 = 4 MiB).
	ChunkSize int
	// Backoff while waiting for an accelerator (0 = 5 s doubling to 2 min:
	// colab-prof R3, the user's "retrying is cheaper than waiting"; 412
	// still backs off exponentially and the wait limit still holds).
	Backoff Backoff
	// BootTimeout for the runtime to answer after assignment (0 = 10 min).
	BootTimeout time.Duration
	// Defaults of the estimate (zero = DefaultEstimateDefaults()).
	Defaults *EstimateDefaults
	// RemoteBase is the runtime directory LectureFlow uses (Contents API
	// path; "" = "content/lectureflow", i.e. /content/lectureflow). In
	// memory mode (kv colab.memory_mode) the memory probe picks a RAM disk
	// instead (memory.go).
	RemoteBase string
	// MemoryExtraMB is added to the memory probe's estimate (experiments and
	// tests: forces the fallback to the disk). 0 in the app.
	MemoryExtraMB int
	// MemoryNoShm makes the memory probe skip /dev/shm and mount its own
	// tmpfs (experiments and tests). false in the app.
	MemoryNoShm bool
	// OnChange (may be nil) is told when what Status shows changed without
	// a request (a background reading of the account: SSE changed colab).
	OnChange func()
	// Enabled (nil = always) says whether Colab is switched on: Run reads
	// the account every five minutes only then.
	Enabled func(ctx context.Context) bool
	// Retry: probing and retrying in place after a transient failure on the
	// runtime (retry.go; zero = 3 probes after 5 s, 15 s and 30 s, each up
	// to 20 % longer, 10 s per probe, 60 s in all). Its clock and waits are
	// Now and Sleep. Not a setting.
	Retry RetryOptions
	// RuntimeTimeout of a small request to the runtime (0 = 60 s; an audio
	// chunk and the result's download get three times as long). Tests
	// shorten it.
	RuntimeTimeout time.Duration
}

// Backoff is the exponential back-off of the accelerator queue.
type Backoff struct{ Initial, Max time.Duration }

// RemoteRequirements are pip-installed on the runtime: the versions of this
// repository's .venv (faster-whisper 1.2.1 needs PyAV < 19, see
// worker/requirements.txt; ctranslate2 4.x is the CUDA 12 / cuDNN 9 build).
var RemoteRequirements = []string{"faster-whisper==1.2.1", "av==15.1.0", "ctranslate2==4.8.3"}

// workerFiles are uploaded to RemoteBase (colab_remote.py imports asr.py,
// which imports asr_common.py and textrepair.py: the same transcription,
// text repair and sentence splitting as the local faster-whisper worker).
var workerFiles = []string{"colab_remote.py", "asr.py", "asr_common.py", "textrepair.py"}

// Steps of one lecture (V14.5 hover card: 等卡 → 上传音频 → 远端转写 →
// 下载结果 → 校验).
const (
	StepWaitAccelerator  = "wait_accelerator"
	StepUploadAudio      = "upload_audio"
	StepRemoteTranscribe = "remote_transcribe"
	StepDownloadResult   = "download_result"
	StepVerify           = "verify"
)

// Event is a progress report of Transcribe.
type Event struct {
	Step string `json:"step"`
	// Stage is the remote worker's stage during remote_transcribe (check,
	// install, download_model, load_model, transcribe, write).
	Stage string `json:"stage,omitempty"`
	// Fraction of the step (0..1; -1 unknown); Overall of the lecture.
	Fraction float64 `json:"fraction"`
	Overall  float64 `json:"overall"`
	// WaitingFor is model.WaitRemoteQueue while Colab has no accelerator.
	WaitingFor   string  `json:"waiting_for,omitempty"`
	ETASec       float64 `json:"eta_sec"` // -1 unknown
	SessionState string  `json:"session_state"`
	// Reconnect: the runtime did not answer and the lecture is trying again
	// in place (retry.go); nil otherwise. An event with it repeats the
	// lecture's last step; one without it follows when the runtime answers
	// again.
	Reconnect *remote.Reconnect `json:"reconnect,omitempty"`
}

// Job is one lecture to transcribe remotely.
type Job struct {
	// ID names the remote job directory ([A-Za-z0-9_-], ≤ 64; other
	// characters are dropped).
	ID string
	// AudioPath is the lecture's audio.wav.
	AudioPath string
	// DurationSec of the audio (estimate and rate; required > 0).
	DurationSec   float64
	Language      string
	InitialPrompt string
	// OutDir receives result.json (written atomically).
	OutDir string
	// WorkDir for the temporary FLAC ("" = OutDir).
	WorkDir string
	// NoRedecode: the switch "修复退化的转写窗口" is off (kv asr.redecode),
	// so worker/asr.py does not decode degenerate windows again (job.json
	// "redecode": false). The zero value keeps the default, on.
	NoRedecode bool
}

// ReleaseInfo is the last release.
type ReleaseInfo struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// File: with ReleaseCleanupFailed, the remote file (a fixed name such
	// as result.json) that could not be deleted.
	File string `json:"file,omitempty"`
}

// LeftoverInfo: a session of an earlier run that could not be released.
type LeftoverInfo struct {
	Accelerator string    `json:"accelerator"`
	Since       time.Time `json:"since"`
	Code        string    `json:"code"`
}

// Backend is the Colab remote transcription backend (V14): one runtime at
// a time, one lecture at a time on it, or two on the larger GPUs, with the
// next planned lectures uploaded ahead (batch.go). See doc.go for the API
// overview.
type Backend struct {
	o       Options
	auth    *authManager
	tun     *tunnel
	secrets *secretSet
	log     *slog.Logger

	relMu sync.Mutex // one release at a time
	// recMu orders the writes of the session record and of the lost
	// runtimes (persist, dropSession, saveLost); lostMu: one retryLost at a
	// time. Order: relMu, recMu, mu.
	recMu  sync.Mutex
	lostMu sync.Mutex

	mu           sync.Mutex
	settings     Settings
	sess         *liveSession
	jobs         []*jobState // running lectures, oldest first
	queue        []float64
	drained      bool
	afterCurrent bool
	lastRelease  *ReleaseInfo
	leftover     *LeftoverInfo
	// lost: runtimes to release that no session stands for (lost.go);
	// lostCode the leftover's code while one is there.
	lost          []lostRuntime
	lostCode      string
	ccu           *ccuSnapshot
	rates         map[string]Rate
	cooldownUntil time.Time
	lastErr       *model.ErrorInfo
	lastErrAt     time.Time
	// lastErrAcct: lastErr came from reading the account while no session
	// existed; the next reading that works clears it (v3.2.x).
	lastErrAcct bool
	// reach: whether the session's runtime answered lately (retry.go;
	// forgotten with the session). handbackWG counts the hand-backs being
	// released in the background.
	reach      reachState
	handbackWG sync.WaitGroup
	// handBackHook (tests) runs in a background hand-back right before its
	// release: a test holds it there to order the release against others.
	handBackHook func()
	// cancelWait: how long a cancelled lecture's Transcribe waits for the
	// release of its runtime (releaseCancelled; cancelReleaseWait, shorter
	// in tests).
	cancelWait time.Duration

	// colab-obs-core (account.go, samples.go): the last attempt to read the
	// account and its error; a background reading runs; the runtime's
	// machine and readings (reset at every release); the number of front
	// ends watching (SetViewers); a failing resources poll was logged.
	ccuTry   time.Time
	ccuErr   error
	acctBusy bool
	samples  remote.Samples
	viewers  func() int
	pollFail bool
	// removalWG counts the background removals of finished lectures'
	// files (removal.go; tests wait for them)
	removalWG sync.WaitGroup

	loginMu sync.Mutex
	login   *Login // the in-app Google sign-in (login.go); nil until Login is first called

	// lastMetrics: the measurements of the last lecture that finished
	// (tests and the realcolab experiment).
	lastMetrics *jobMetrics
	// hookReady, when set (tests and the realcolab experiment only), runs
	// once the runtime is ready for a lecture, before anything is uploaded.
	hookReady func(ctx context.Context, s *liveSession)

	// colab-batch (batch.go): the scheduler's plan, the planner's state,
	// and the context of the background work (uploads ahead, deletions),
	// cancelled by Close.
	plan      []PlanEntry
	planning  bool
	planAgain bool
	planIdle  chan struct{}
	bg        context.Context
	bgCancel  context.CancelFunc
}

type ccuSnapshot struct {
	info ccuInfo
	at   time.Time
}

const ccuMaxAge = time.Minute

var errShutdownNow = errors.New("colab: shut down now")
var errAuthEnding = errors.New("colab: the credential is about to end")

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// New builds a Backend. Call Startup once before use.
func New(o Options) (*Backend, error) {
	if o.KV == nil {
		return nil, errors.New("colab: Options.KV is required")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
		if o.UploadClient == nil {
			if tr, ok := http.DefaultTransport.(*http.Transport); ok {
				o.UploadClient = &http.Client{Transport: tr.Clone()}
			}
		}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	// The redirect policy (redirect.go) on every client: copies, so a
	// caller's client is left as it is.
	shared := o.UploadClient == nil || o.UploadClient == o.HTTPClient
	o.HTTPClient = withRedirectPolicy(o.HTTPClient, o.Logger)
	if shared {
		o.UploadClient = o.HTTPClient
	} else {
		o.UploadClient = withRedirectPolicy(o.UploadClient, o.Logger)
	}
	if o.Dial == nil {
		o.Dial = DialWebSocket(nil)
	}
	if o.ColabURL == "" {
		o.ColabURL = DefaultColabURL
	}
	if o.TokenURL == "" {
		o.TokenURL = DefaultTokenURL
	}
	if o.RevokeURL == "" {
		o.RevokeURL = DefaultRevokeURL
	}
	if o.ADCPath == "" {
		o.ADCPath = ADCPath()
	}
	if o.RunFFmpeg == nil {
		o.RunFFmpeg = runFFmpeg
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = defaultSleep
	}
	if o.RunID == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		o.RunID = hex.EncodeToString(b[:])
	}
	if o.ClientAgent == "" {
		o.ClientAgent = defaultClientAgent
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = defaultChunkSize
	}
	if o.Backoff.Initial <= 0 {
		o.Backoff.Initial = 5 * time.Second
	}
	if o.Backoff.Max < o.Backoff.Initial {
		o.Backoff.Max = max(o.Backoff.Initial, 2*time.Minute)
	}
	if o.BootTimeout <= 0 {
		o.BootTimeout = 10 * time.Minute
	}
	if o.Defaults == nil {
		d := DefaultEstimateDefaults()
		o.Defaults = &d
	}
	if o.RemoteBase == "" {
		o.RemoteBase = "content/lectureflow"
	}
	o.Retry = o.Retry.normalized()
	if o.RuntimeTimeout <= 0 {
		o.RuntimeTimeout = runtimeTimeout
	}
	b := &Backend{o: o, secrets: &secretSet{}, log: o.Logger, settings: DefaultSettings(), rates: map[string]Rate{},
		cancelWait: cancelReleaseWait}
	b.bg, b.bgCancel = context.WithCancel(context.Background())
	b.auth = &authManager{kv: o.KV, hc: o.HTTPClient, tokenURL: o.TokenURL, revokeURL: o.RevokeURL, now: o.Now,
		runID: o.RunID, secrets: b.secrets, log: o.Logger}
	b.tun = &tunnel{base: o.ColabURL, hc: o.HTTPClient, auth: b.auth, agent: o.ClientAgent, secrets: b.secrets, log: o.Logger}
	return b, nil
}

func (b *Backend) now() time.Time { return b.o.Now() }

// Startup runs once when the app starts: deletes a credential its
// lifecycle no longer allows (a "session" one of an earlier run, an
// expired one), loads settings and measured rates, and releases a session
// an earlier run left behind (V14.3 step 5).
func (b *Backend) Startup(ctx context.Context) error {
	b.auth.startup(ctx)
	set, err := LoadSettings(ctx, b.o.KV)
	if err != nil {
		return err
	}
	rates := map[string]Rate{}
	if v, ok, err := getKV(ctx, b.o.KV, KVRates); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &rates)
	}
	var cooldown time.Time
	if v, ok, err := getKV(ctx, b.o.KV, KVCooldown); err == nil && ok {
		cooldown, _ = time.Parse(time.RFC3339, v)
	}
	b.mu.Lock()
	b.settings, b.rates, b.cooldownUntil = set, rates, cooldown
	b.mu.Unlock()
	b.loadLost(ctx)
	b.retryLost(ctx) // runtimes an earlier run could not release (lost.go)
	rec, err := b.loadRecord(ctx)
	if err != nil {
		return err
	}
	if rec != nil {
		return b.secrets.scrubErr(b.cleanupLeftover(ctx, rec))
	}
	return nil
}

// Settings returns the settings in effect: Accelerator is what the next
// borrowing asks for (the user's pick or the account level's default; T4
// when the account does not allow it, Status.LockedReason) and Tier the
// account's (account.go). The settings page shows UserSettings.
func (b *Backend) Settings() Settings { return b.liveSettings() }

// SetSettings validates, stores and applies settings (a running session
// keeps its accelerator).
func (b *Backend) SetSettings(ctx context.Context, s Settings) error {
	s = s.normalized()
	if err := SaveSettings(ctx, b.o.KV, s); err != nil {
		return err
	}
	b.mu.Lock()
	b.settings = s
	b.mu.Unlock()
	return nil
}

// DetectADC is "检测授权": is gcloud's credential file there and importable?
func (b *Backend) DetectADC() ADCInfo { return DetectADC(b.o.ADCPath) }

// ImportADC imports the user's own gcloud application default credentials
// with the user's lifecycle (V14.2), checking them with one token refresh;
// the credential's source is SourceGcloud. A leftover session that waited
// for a credential is released afterwards.
func (b *Backend) ImportADC(ctx context.Context, lc Lifecycle) (CredentialInfo, error) {
	return b.ImportADCFile(ctx, b.o.ADCPath, lc, SourceGcloud)
}

// ImportADCFile is ImportADC for the ADC file at path, recording source
// (SourceApp: the in-app sign-in imports the file gcloud wrote into its own
// configuration directory; anything but SourceApp is stored as
// SourceGcloud).
func (b *Backend) ImportADCFile(ctx context.Context, path string, lc Lifecycle, source string) (CredentialInfo, error) {
	info, err := b.auth.importADC(ctx, path, lc, source)
	if err != nil {
		return info, b.secrets.scrubErr(err)
	}
	b.forgetAccount() // perhaps another Google account: read it again (RefreshAccountSoon)
	if rec, err := b.loadRecord(ctx); err == nil && rec != nil {
		b.mu.Lock()
		live := b.sess != nil
		b.mu.Unlock()
		if !live {
			_ = b.cleanupLeftover(ctx, rec)
		}
	}
	b.retryLost(ctx)
	return info, nil
}

// Credential describes the stored credential (no secret).
func (b *Backend) Credential(ctx context.Context) CredentialInfo { return b.auth.info(ctx) }

// DisconnectResult is the outcome of "断开连接".
type DisconnectResult struct {
	// Revoked: Google confirmed the revocation. The local copy is deleted
	// whatever this says.
	Revoked bool `json:"revoked"`
	// Released: a runtime was given back first.
	Released bool              `json:"released"`
	Warnings []model.ErrorInfo `json:"warnings,omitempty"`
	// Source of the credential that was stored (SourceApp, SourceGcloud;
	// "" when there was none).
	Source string `json:"source,omitempty"`
}

// Disconnect ends the running lecture (back to the queue), releases the
// runtime, revokes the credential at Google and deletes the local copy
// (V14.2). A failed revocation still deletes the local copy and is
// reported as a warning. Note: a SourceGcloud credential is the user's own
// gcloud ADC file's refresh token, so revoking it also signs gcloud's
// application-default login out; a SourceApp one was only ever in the
// app's sign-in directory (wiped after the import).
func (b *Backend) Disconnect(ctx context.Context) (DisconnectResult, error) {
	var res DisconnectResult
	b.stopJobs(errShutdownNow, 30*time.Second)
	b.mu.Lock()
	had := b.sess != nil
	b.mu.Unlock()
	if had {
		if err := b.release(ctx, ReleaseDisconnect); err != nil {
			res.Warnings = append(res.Warnings, *infoOf(b.secrets.scrubErr(err)))
		} else {
			res.Released = true
		}
	}
	revoked, source, err := b.auth.revokeAndDelete(ctx)
	res.Revoked, res.Source = revoked, source
	b.forgetAccount()
	if err != nil {
		if Code(err) == "" {
			return res, err // the database failed: the local copy may still be there
		}
		res.Warnings = append(res.Warnings, *infoOf(b.secrets.scrubErr(err)))
	}
	return res, nil
}

// Close is called when the app quits: a running in-app sign-in is
// cancelled, the runtime is released, and a credential with the "session"
// lifecycle is deleted (its run ends).
//
// A hand-back being released in the background (handBack) is covered: the
// release here waits for it (relMu) and is then a no-op, or runs first and
// releases the runtime itself. When neither succeeds before the app is
// gone, the session record (state stopping) stays and the next Startup
// releases the runtime (cleanupLeftover).
func (b *Backend) Close(ctx context.Context) error {
	b.closeLogin() // a running gcloud sign-in ends with the app (login.go)
	b.stopJobs(errShutdownNow, 10*time.Second)
	err := b.release(ctx, ReleaseAppExit)
	defer b.bgCancel()
	b.retryLost(ctx) // what fails stays in kv colab.lost for the next start
	if v, ok, _ := getKV(ctx, b.o.KV, KVCredential); ok {
		var c storedCredential
		if json.Unmarshal([]byte(v), &c) == nil && c.Lifecycle.Kind == LifecycleSession {
			_ = b.auth.deleteLocal(ctx)
		}
	}
	return b.secrets.scrubErr(err)
}

// SetQueue tells the backend the audio lengths (seconds) of the lectures
// planned for it, in order ("本次计划的剩余讲座", "还能再处理约 N 节").
// A non-empty queue clears an earlier QueueDrained.
func (b *Backend) SetQueue(audioSecs []float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queue = append([]float64(nil), audioSecs...)
	if len(audioSecs) > 0 {
		b.drained = false
	}
}

// QueueDrained says no more lectures are coming: the runtime is released
// now, or after the running lecture (V14.3 step 5: "队列清空…立即释放").
// The caller must not call it while it has a lecture on its way to
// Transcribe (the scheduler waits until its Colab run has ended,
// colabfix); a lecture that arrives while the release runs gets IsBusy.
func (b *Backend) QueueDrained(ctx context.Context) error {
	b.mu.Lock()
	b.drained, b.queue, b.plan = true, nil, nil
	busy := len(b.jobs) > 0
	b.mu.Unlock()
	b.kickPlan() // lectures uploaded ahead go
	if busy {
		return nil
	}
	return b.secrets.scrubErr(b.release(ctx, ReleaseQueueEmpty))
}

// ShutdownAfterCurrent is "处理完当前这节后关机": release after the
// running lecture, or now when none runs.
func (b *Backend) ShutdownAfterCurrent(ctx context.Context) error {
	b.mu.Lock()
	b.afterCurrent = true
	busy := len(b.jobs) > 0
	b.mu.Unlock()
	b.kickPlan() // lectures uploaded ahead go
	if busy {
		return nil
	}
	return b.secrets.scrubErr(b.release(ctx, ReleaseAfterCurrent))
}

// ShutdownNow is "立即关机": the running lecture stops (Transcribe returns
// an error with IsRequeue true: it goes back to the queue; a lecture whose
// result was already written succeeds) and the runtime is released.
// Transcribe releases it when it notices the stop, but that is not assumed
// (Gate round 2: it may not have returned within the wait): the release
// here is a no-op when the runtime is already gone.
func (b *Backend) ShutdownNow(ctx context.Context) error {
	b.stopJobs(errShutdownNow, 30*time.Second)
	return b.secrets.scrubErr(b.release(ctx, ReleaseNow))
}

// stopJobs cancels the running lectures with cause and waits up to wait
// for their Transcribe to return; false when no lecture was running.
func (b *Backend) stopJobs(cause error, wait time.Duration) bool {
	b.mu.Lock()
	js := append([]*jobState(nil), b.jobs...)
	b.mu.Unlock()
	if len(js) == 0 {
		return false
	}
	for _, j := range js {
		j.cancel(cause)
	}
	deadline := time.After(wait)
	for _, j := range js {
		select {
		case <-j.done:
		case <-deadline:
			return true
		}
	}
	return true
}

// Run calls Tick every 30 seconds until ctx ends; with it the account is
// read every five minutes while Colab is on (account.go), and every 5 s the
// runtime's own resources fill in for the worker's readings while a front
// end watches (samples.go).
func (b *Backend) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	p := time.NewTicker(pollEvery)
	defer p.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick(ctx)
			b.accountTick(ctx)
		case <-p.C:
			b.pollResources(ctx)
		}
	}
}

// Tick does the time-driven work: releases an idle runtime (queue drained,
// after-current, idle limit, window used up, credential about to end, a
// finished lecture's file left on it),
// stops a lecture whose credential is about to end (back to the queue),
// and retries a failed release.
func (b *Backend) Tick(ctx context.Context) {
	b.adoptRecord(ctx) // a leftover session record joins them
	b.retryLost(ctx)   // runtimes no session stands for (lost.go)
	b.mu.Lock()
	s, js := b.sess, append([]*jobState(nil), b.jobs...)
	set := b.settings
	drained, after := b.drained && len(b.queue) == 0, b.afterCurrent
	var state, left string
	var idleSince, requested time.Time
	if s != nil {
		state, idleSince, requested, left = s.rec.State, s.lastJobEnd, s.rec.RequestedAt, s.leftFile
		if idleSince.IsZero() {
			idleSince = s.rec.ReadyAt
		}
	}
	b.mu.Unlock()
	if s == nil {
		return
	}
	authLeft, authErr := b.auth.remaining(ctx)
	authEnding := authErr != nil || authLeft >= 0 && authLeft < time.Duration(b.o.Defaults.AuthMarginSec)*time.Second
	if len(js) > 0 {
		if authEnding {
			b.log.Info("colab: the credential is about to end; stopping the lecture and releasing the runtime")
			for _, j := range js {
				j.cancel(errAuthEnding)
			}
		}
		return
	}
	reason, file := "", ""
	now := b.now()
	windowUp := now.Sub(requested) >= FairWindow-time.Duration(b.o.Defaults.OverheadSec)*time.Second
	switch {
	case state == StateStopping:
		reason = ReleaseFailed // retry
	case state != StateRunning:
		return
	case authEnding:
		reason = ReleaseAuthEnding
	case left != "" && !windowUp:
		// A finished lecture's file stayed and the lectures that kept the
		// runtime are over (one that left before its end never released
		// it: review R52 L1). The window's own release keeps its cooldown.
		reason, file = ReleaseCleanupFailed, left
	case after:
		reason = ReleaseAfterCurrent
	case drained:
		reason = ReleaseQueueEmpty
	case windowUp:
		reason = ReleaseWindow
	case !idleSince.IsZero() && now.Sub(idleSince) >= time.Duration(set.IdleReleaseMin)*time.Minute:
		reason = ReleaseIdle
	}
	if reason != "" {
		// A lecture dispatched to the runtime since the snapshot above keeps
		// it (SEC2 T3): the idle-type reasons release only while no lecture
		// is on it (ifIdle), and the next Tick asks again once none is. The
		// retry of a failed release (stopping: nothing is dispatched to it)
		// and the credential's end (later it could not be released at all)
		// go as before.
		idle := state != StateStopping && reason != ReleaseAuthEnding
		_ = b.releaseWith(ctx, reason, releaseOpts{file: file, expect: s, ifIdle: idle}) // a stopping hand-back's own reason wins (releaseWith)
	}
}

// ---- dispatch decision ----

func rateKey(accelerator, modelName, computeType string) string {
	return accelerator + "|" + modelName + "|" + computeType
}

// dispatchInput assembles the pure function's input from the backend's
// state. balance is the cached ccu-info (nil = unknown).
func (b *Backend) dispatchInput(audioSec float64, authLeft time.Duration, authOK bool) DispatchInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.liveSettingsLocked()
	in := DispatchInput{
		AudioSec: audioSec, WindowSec: FairWindow.Seconds(), Paid: set.Tier == TierPaid,
		AuthRemainingSec: -1,
	}
	acc := set.Accelerator
	if s := b.sess; s != nil {
		acc = s.rec.Accelerator
		in.UsedSec = max(0, b.now().Sub(s.rec.RequestedAt).Seconds())
		in.SetupDone = s.setupDone
	}
	in.Defaults = b.o.Defaults.For(acc)
	in.Rate = b.rates[rateKey(acc, set.Model, set.ComputeType)]
	in.CUPerHour = b.o.Defaults.CUPerHour[acc]
	if b.ccu != nil {
		in.BalanceKnown, in.BalanceCU = true, b.ccu.info.Balance
		if q := b.ccu.info.FreeQuotaCU; q != nil && acc == "T4" {
			in.BalanceCU += *q // the free quota is for a T4 (whichever Colab charges first)
		}
		if b.sess != nil && b.ccu.info.Assignments > 0 && b.ccu.info.RatePerHour > 0 {
			in.CUPerHour = b.ccu.info.RatePerHour // the account's real rate while our runtime runs
		}
	}
	if !authOK {
		in.AuthRemainingSec = 0
	} else if authLeft >= 0 {
		in.AuthRemainingSec = authLeft.Seconds()
	}
	return in
}

// CanDispatch reports whether a lecture of lectureDurationSec should go to
// Colab now, and if not, the waiting_for value (model.WaitRemote*; "" when
// the backend is simply busy or Colab cannot be asked). The account is
// read from Colab at most once a minute (every level: it says which
// accelerator and, when paid, the balance); a transient failure to read it
// does not stop the dispatch (account.go blockingRead).
func (b *Backend) CanDispatch(ctx context.Context, lectureDurationSec float64) (bool, string) {
	authLeft, err := b.auth.remaining(ctx)
	if err != nil {
		return false, model.WaitRemoteAuth
	}
	b.mu.Lock()
	busy := len(b.jobs) >= b.slotsLocked() || len(b.jobs) > 0 && b.afterCurrent ||
		b.sess != nil && b.sess.rec.State != StateRunning
	cooling := b.sess == nil && b.now().Before(b.cooldownUntil)
	b.mu.Unlock()
	if busy {
		return false, ""
	}
	if cooling {
		return false, model.WaitRemoteWindow
	}
	if err := b.refreshCCU(ctx); blockingRead(err) {
		b.mu.Lock()
		b.noteErrLocked(infoOf(b.secrets.scrubErr(err)))
		b.lastErrAcct = b.sess == nil
		b.mu.Unlock()
		if WaitingFor(err) != "" {
			return false, WaitingFor(err)
		}
		return false, ""
	}
	d := Decide(b.dispatchInput(lectureDurationSec, authLeft, true))
	return d.Dispatch, d.WaitingFor
}

// ---- status ----

// Status is what the resource bar and the 工作台 line show (V14.5).
type Status struct {
	Service     string         `json:"service"` // "colab"
	Configured  bool           `json:"configured"`
	Auth        CredentialInfo `json:"auth"`
	Accelerator string         `json:"accelerator"`
	Tier        string         `json:"tier"`
	// State: none, queued, booting, running, stopping, released (the last
	// release is in LastRelease).
	State string `json:"state"`
	// Window: fair-use window of the current session.
	WindowUsedSec  int64 `json:"window_used_sec"`
	WindowTotalSec int64 `json:"window_total_sec"`
	// LecturesLeft: "本次还能再处理约 N 节" (planned queue when known, else
	// lectures of TypicalLectureSec).
	LecturesLeft int `json:"lectures_left"`
	// Planned lectures (SetQueue) and how many of them fit.
	Planned    int      `json:"planned"`
	PlannedFit int      `json:"planned_fit"`
	Current    *JobInfo `json:"current,omitempty"`
	// colab-batch: every running lecture (Current is the first), how many
	// may run at once, and how many lectures' audio is on the runtime
	// ahead of their turn.
	Jobs           []JobInfo `json:"jobs,omitempty"`
	Slots          int       `json:"slots"`
	UploadedAhead  int       `json:"uploaded_ahead"`
	BalanceCU      *float64  `json:"balance_cu,omitempty"`
	CUPerHour      float64   `json:"cu_per_hour"`
	PlanCostCU     float64   `json:"plan_cost_cu"`     // "本次预计消耗"
	SessionSpentCU float64   `json:"session_spent_cu"` // since assignment, at CUPerHour
	ShutdownAfter  bool      `json:"shutdown_after_current"`
	// CooldownUntil: no new session before this (after a window release).
	CooldownUntil *time.Time       `json:"cooldown_until,omitempty"`
	LastRelease   *ReleaseInfo     `json:"last_release,omitempty"`
	Leftover      *LeftoverInfo    `json:"leftover,omitempty"`
	LastError     *model.ErrorInfo `json:"last_error,omitempty"`
	LastErrorAt   *time.Time       `json:"last_error_at,omitempty"`
	// SessionStartedAt: when the current session was requested (absent
	// without one). LastError is cleared when a session starts; a front end
	// may also hide an error older than this.
	SessionStartedAt *time.Time `json:"session_started_at,omitempty"`
	// Runtime: whether the session's runtime is held, answers and is billed
	// (retry.go; absent without a session). A hint only: nothing is released
	// because of it.
	Runtime *RuntimeInfo `json:"runtime,omitempty"`

	// colab-obs-core (account.go): the account (always present; level free
	// until it was read), the accelerators it allows (T4 first), the one
	// the next borrowing asks for and, when that is not the user's pick
	// (or the level's default), why (LockAccountUnknown, LockNoBalance,
	// LockIneligible). Accelerator is the session's while one runs, else
	// EffectiveAccelerator.
	Account                Account  `json:"account"`
	SelectableAccelerators []string `json:"selectable_accelerators"`
	// PreferredAccelerator: the user's pick on the settings page (or,
	// never picked, the level's default); EffectiveAccelerator differs from
	// it while LockedReason says why.
	PreferredAccelerator string `json:"preferred_accelerator"`
	EffectiveAccelerator string `json:"effective_accelerator"`
	LockedReason         string `json:"locked_reason"` // "" = the pick (or default) is borrowed
	// Machine and Usage: what the runtime reports about itself while a
	// session runs (samples.go); absent otherwise, and before its first
	// reading.
	Machine *remote.Machine `json:"machine,omitempty"`
	Usage   *remote.Usage   `json:"usage,omitempty"`
}

// JobInfo is the running lecture.
type JobInfo struct {
	ID        string    `json:"id"`
	Step      string    `json:"step"`
	Stage     string    `json:"stage,omitempty"`
	Fraction  float64   `json:"fraction"`
	Overall   float64   `json:"overall"`
	ETASec    float64   `json:"eta_sec"`
	StartedAt time.Time `json:"started_at"`
	// JobID is the scheduler's job id (Job.ID; ID is the remote
	// directory's name); Split the parts of "极速模式" (0 = whole).
	JobID string `json:"job_id,omitempty"`
	Split int    `json:"split,omitempty"`
	// Reconnect: the runtime does not answer and the lecture is trying
	// again in place (retry.go); nil otherwise.
	Reconnect *remote.Reconnect `json:"reconnect,omitempty"`
}

// Status returns the backend's state without contacting Colab (the balance
// is the last value read).
func (b *Backend) Status(ctx context.Context) Status {
	auth := b.auth.info(ctx)
	authLeft, authErr := b.auth.remaining(ctx)
	in := b.dispatchInput(TypicalLectureSec, authLeft, authErr == nil)
	b.mu.Lock()
	defer b.mu.Unlock()
	sel := b.selectionLocked()
	// Before the balance was read once, "还能再处理约 N 节" counts the window
	// and the credential only (the balance appears when known). A T4
	// counts Colab's free quota too, at every level (account.go).
	acc := sel.effective
	if b.sess != nil {
		acc = b.sess.rec.Accelerator
	}
	in.Paid = in.BalanceKnown && (in.Paid || acc == "T4" && sel.quotaCU >= 0)
	st := Status{
		Service: "colab", Configured: auth.Present, Auth: auth, Accelerator: sel.effective, Tier: sel.tier(),
		State: StateNone, WindowTotalSec: int64(FairWindow.Seconds()), CUPerHour: in.CUPerHour, ShutdownAfter: b.afterCurrent,
		LastRelease: b.lastRelease, Leftover: b.leftoverLocked(), LastError: b.lastErr, Planned: len(b.queue),
		Slots: b.slotsLocked(), Account: b.accountLocked(), SelectableAccelerators: sel.selectable,
		EffectiveAccelerator: sel.effective, LockedReason: sel.locked, PreferredAccelerator: sel.preferred,
	}
	if b.lastErr != nil && !b.lastErrAt.IsZero() {
		t := b.lastErrAt.UTC()
		st.LastErrorAt = &t
	}
	if b.lastRelease != nil {
		st.State = StateReleased
	}
	if s := b.sess; s != nil {
		st.State, st.Accelerator = s.rec.State, s.rec.Accelerator
		st.WindowUsedSec = int64(in.UsedSec)
		for _, u := range s.uploads {
			if u.ok {
				st.UploadedAhead++
			}
		}
		if !s.rec.AssignedAt.IsZero() {
			st.SessionSpentCU = round2(b.now().Sub(s.rec.AssignedAt).Hours() * in.CUPerHour)
		}
		st.Machine, st.Usage = b.machineLocked(s), b.samples.Usage(b.now())
		started := s.rec.RequestedAt.UTC()
		st.SessionStartedAt = &started
		st.Runtime = b.runtimeInfoLocked(s, sel.tier() == TierPaid)
	}
	if b.ccu != nil {
		v := round2(b.ccu.info.Balance)
		st.BalanceCU = &v
	}
	if b.now().Before(b.cooldownUntil) {
		t := b.cooldownUntil
		st.CooldownUntil = &t
	}
	if len(b.queue) > 0 {
		p := PlanQueue(in, b.queue)
		st.LecturesLeft, st.PlannedFit, st.PlanCostCU = p.Lectures, p.Lectures, p.CostCU
	} else {
		st.LecturesLeft = ApproxLectures(in, TypicalLectureSec)
	}
	for _, j := range b.jobs {
		st.Jobs = append(st.Jobs, JobInfo{ID: j.id, Step: j.ev.Step, Stage: j.ev.Stage, Fraction: j.ev.Fraction, Overall: j.ev.Overall,
			ETASec: j.ev.ETASec, StartedAt: j.started, JobID: j.key, Split: j.split, Reconnect: j.reconnect})
	}
	if len(st.Jobs) > 0 {
		c := st.Jobs[0]
		st.Current = &c
	}
	return st
}

// noteErrLocked records the last error (Status.LastError, LastErrorAt).
func (b *Backend) noteErrLocked(info *model.ErrorInfo) {
	b.lastErr, b.lastErrAt, b.lastErrAcct = info, b.now(), false
}

// clearErrLocked forgets the last error (a new session begins; the account
// can be read again).
func (b *Backend) clearErrLocked() {
	b.lastErr, b.lastErrAt, b.lastErrAcct = nil, time.Time{}, false
}

// accountReadOK: the account was read. An error recorded when it could not
// be read while no session existed is over (v3.2.x: it used to stay on the
// card until the app quit).
func (b *Backend) accountReadOK() {
	b.mu.Lock()
	cleared := b.lastErrAcct
	if cleared {
		b.clearErrLocked()
	}
	b.mu.Unlock()
	if cleared {
		b.changed()
	}
}

func infoOf(err error) *model.ErrorInfo {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info := ce.Info
		return &info
	}
	return &model.ErrorInfo{Code: CodeUnavailable, Detail: err.Error()}
}

// ---- one lecture ----

var jobIDClean = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func remoteJobID(id string) string {
	id = jobIDClean.ReplaceAllString(id, "")
	if len(id) > 64 {
		id = id[:64]
	}
	if id == "" {
		id = randomHex(8)
	}
	return id
}

// Transcribe runs one lecture on Colab: wait for the accelerator → upload
// the audio → remote transcription → download the result → verify
// (V14.3). It returns the path of result.json in job.OutDir, the same
// structure the local faster-whisper worker writes.
//
// Errors: IsFallback (hand the lecture to the local devices; WaitingFor
// tells why when it is a dispatch condition), IsRequeue ("立即关机": back
// to the queue), IsBusy (the runtime is being released or started, or
// another lecture still runs on it: back to the queue, Colab may take it
// later; nothing is released), a cancelled ctx (the runtime is released, the error wraps
// context.Canceled), or a local problem (missing audio). A transient
// failure on the runtime is probed and retried in place first (retry.go);
// any other failure, or the probe window used up, hands the lecture back at
// once and releases the runtime in the background (V14.3 step 5; v3.2.x:
// Transcribe no longer waits for the release).
func (b *Backend) Transcribe(ctx context.Context, job Job, progress func(Event)) (string, error) {
	path, err := b.transcribe(ctx, job, progress)
	return path, b.secrets.scrubErr(err)
}

func (b *Backend) transcribe(ctx context.Context, job Job, progress func(Event)) (string, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	if _, err := os.Stat(job.AudioPath); err != nil {
		return "", newErr(CodeAudioMissing, map[string]any{"file": filepath.Base(job.AudioPath)}, kindLocal, "",
			errors.New("the lecture's audio file is missing"))
	}
	if job.OutDir == "" {
		return "", errors.New("colab: Job.OutDir is required")
	}
	if job.WorkDir == "" {
		job.WorkDir = job.OutDir
	}
	files, hash, err := b.readWorkerFiles()
	if err != nil {
		return "", err
	}
	authLeft, err := b.auth.remaining(ctx)
	if err != nil {
		return "", err
	}

	jctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jp := &jobProgress{b: b, out: progress, est: -1}
	b.mu.Lock()
	if len(b.jobs) >= b.slotsLocked() {
		b.mu.Unlock()
		b.log.Info("colab: other lectures still run on the remote runtime; the lecture goes back to the queue")
		return "", busyErr(errors.New("another lecture is running on the remote runtime"))
	}
	set := b.liveSettingsLocked()
	j := &jobState{id: remoteJobID(job.ID), key: job.ID, audio: job.AudioPath, durSec: job.DurationSec, started: b.now(),
		cancel: cancel, done: make(chan struct{})}
	b.jobs = append(b.jobs, j)
	jp.j = j
	ahead := false
	if b.sess != nil {
		for _, u := range b.sess.uploads {
			ahead = ahead || u.key == job.ID || u.audio == job.AudioPath
		}
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		for i, x := range b.jobs {
			if x == j {
				b.jobs = append(b.jobs[:i:i], b.jobs[i+1:]...)
				break
			}
		}
		b.mu.Unlock()
		close(j.done)
		b.kickPlan()
	}()
	// R3: the FLAC encoding overlaps the wait for the accelerator (not
	// needed when the audio went up ahead of the lecture's turn).
	var af *audioFuture
	if !ahead {
		af = b.startAudio(jctx, job)
		defer af.discard()
	}

	// The account before borrowing: which accelerator it allows and, when
	// paid, the balance (every level; a transient failure goes on, account.go).
	b.mu.Lock()
	hasSess := b.sess != nil
	b.mu.Unlock()
	if !hasSess {
		if err := b.refreshCCU(jctx); blockingRead(err) {
			b.mu.Lock()
			prev := b.lastErr
			b.mu.Unlock()
			err = b.jobErr(ctx, jctx, nil, err) // no session of this lecture's
			b.mu.Lock()
			if b.lastErr != prev {
				// recorded just now (a cancel records nothing: an earlier
				// lecture's error stays a lecture's error, review R3)
				b.lastErrAcct = b.sess == nil // over once the account can be read
			}
			b.mu.Unlock()
			return "", err
		}
	}
	set = b.liveSettings()
	if d := Decide(b.dispatchInput(job.DurationSec, authLeft, true)); !d.Dispatch {
		return "", decisionErr(d)
	}
	jp.est = EstimateSec(job.DurationSec, b.currentRate(set), b.defaultsFor(set), b.setupDone())
	jp.step(StepWaitAccelerator, -1)
	s, err := b.acquire(jctx, set, jp)
	if IsBusy(err) {
		// The runtime is being released (or started) by someone else: not
		// a failure of this lecture, nothing to release or report
		// (colabfix). The scheduler requeues it.
		b.log.Info("colab: the remote runtime is being released or started; the lecture goes back to the queue")
		return "", err
	}
	if err != nil {
		return "", b.jobErr(ctx, jctx, s, err) // s: the session whose boot failed, or nil
	}
	// With the real session: check again (queue wait and boot count).
	if set.Tier == TierPaid {
		if err := b.refreshCCU(jctx); blockingRead(err) {
			return "", b.jobErr(ctx, jctx, s, err)
		}
	}
	if authLeft, err = b.auth.remaining(jctx); err != nil {
		return "", b.jobErr(ctx, jctx, s, err)
	}
	if d := Decide(b.dispatchInput(job.DurationSec, authLeft, true)); !d.Dispatch {
		return "", decisionErr(d) // the runtime stays for a lecture that fits; Tick releases it when idle
	}
	if b.hookReady != nil {
		b.hookReady(jctx, s)
	}
	path, m, out, err := b.runJob(jctx, s, set, job, j, files, hash, af, jp)
	if err != nil {
		return "", b.lectureFailed(ctx, jctx, s, j, out, err)
	}
	b.finishJob(ctx, jctx, s, set, job, j, m)
	return path, nil
}

// jobOnlyFailure: the failure is the lecture's own (its audio or result,
// a local problem), not the runtime's: other lectures may go on there.
func jobOnlyFailure(err error) bool {
	switch Code(err) {
	case CodeHashMismatch, CodeDownloadFailed, CodeUploadFailed, CodeLocalWrite, CodeAudioMissing:
		return true
	case CodeRemoteFailed:
		r, _ := codedParam(err, "reason")
		switch r {
		case "bad_job", "audio_missing", "audio_hash_mismatch", "audio_unreadable":
			return true
		}
	}
	return false
}

// keepFor reports whether the runtime stays after lecture j stopped or
// failed on its own: other lectures run there, are uploaded ahead or are
// planned to be (colab-batch: "卡租到了就该打满"). Without any, a failure or
// a cancel releases it at once as before (V14.3 step 5).
func (b *Backend) keepFor(s *liveSession, j *jobState) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess == s && s.rec.State == StateRunning && !b.afterCurrent && b.othersLocked(s, j) > 0
}

// lectureFailed handles a failure on the runtime (runJob): 立即关机 and an
// ending credential release the runtime; a cancel or the lecture's own
// failure deletes its remote files and keeps the runtime for the other
// lectures (keepFor), else releases it; any other failure releases it.
func (b *Backend) lectureFailed(ctx, jctx context.Context, s *liveSession, j *jobState, out runOutcome, err error) error {
	cause := context.Cause(jctx)
	stopped := errors.Is(cause, errShutdownNow) || errors.Is(cause, errAuthEnding)
	if !stopped && (ctx.Err() != nil || jobOnlyFailure(err)) && b.keepFor(s, j) {
		switch {
		case out.jobDir == "":
		case ctx.Err() != nil:
			// retried (the runtime stays): a file that still cannot go
			// releases the runtime once no other lecture runs on it
			b.cleanupKept(context.WithoutCancel(jctx), s, out.jobDir)
		default:
			// a failure: the same, in the background (the lecture goes back
			// now, v3.2.x)
			b.markBusyDir(s, out.jobDir) // before the lecture can come back (T051)
			b.handbackWG.Add(1)
			go func() {
				defer b.handbackWG.Done()
				b.cleanupKept(b.bg, s, out.jobDir) // ends with the app (Close)
			}()
		}
		if ctx.Err() != nil {
			b.log.Info("colab: lecture cancelled; its files on the runtime were deleted, the runtime stays for the other lectures")
			return newErr(CodeCancelled, nil, kindFallback, "", ctx.Err())
		}
		if Code(err) == "" {
			err = fallbackErr(CodeUnavailable, map[string]any{"endpoint": "runtime"}, err)
		}
		b.mu.Lock()
		b.noteErrLocked(infoOf(b.secrets.scrubErr(err)))
		b.mu.Unlock()
		b.log.Info("colab: lecture failed on Colab; back to the local devices (the runtime stays for the other lectures)",
			"code", Code(err))
		return err
	}
	if stopped || ctx.Err() != nil {
		// 立即关机, an ending credential: released here, as before (their
		// callers rely on the runtime being gone). The user's cancel: the
		// release begins here and is waited for a few seconds at most
		// (releaseCancelled).
		return b.jobErr(ctx, jctx, s, err)
	}
	// A failure: the lecture goes back now; its files (one attempt each)
	// and the runtime go in the background (handBack).
	return b.failedErr(s, out.jobDir, err)
}

// failedErr classifies a failure of a lecture on Colab, records it as the
// last error and hands the lecture's runtime back in the background when
// it held one (V14.3 step 5: released at once — begun at once, not waited
// for; v3.2.x). s nil: no session of this lecture's — nothing is released
// (the session the backend has now is another lecture's: SEC2 H1, L3);
// jobDir ("" = none) has its files deleted before the release.
func (b *Backend) failedErr(s *liveSession, jobDir string, err error) error {
	if st := statusOf(err); Code(err) == "" && (st == 401 || st == 403 || st == 404 || st == 410) && isRuntimeErr(err) {
		err = fallbackErr(CodeSessionReclaimed, nil, err) // the runtime's Jupyter server is gone or refuses us
	}
	if Code(err) == "" {
		err = codedHTTP("colab", err)
		if Code(err) == "" {
			err = fallbackErr(CodeUnavailable, map[string]any{"endpoint": "runtime"}, err)
		}
	}
	reason := ReleaseFailed
	if Code(err) == CodeSessionReclaimed {
		reason = ReleaseReclaimed
	}
	b.mu.Lock()
	b.noteErrLocked(infoOf(b.secrets.scrubErr(err)))
	b.mu.Unlock()
	b.log.Info("colab: lecture failed on Colab; back to the local devices", "code", Code(err), "release", s != nil)
	if s != nil {
		b.handBack(s, jobDir, reason)
	}
	return err
}

// defaultsFor is the estimate's defaults with the accelerator's own
// measured speed (EstimateDefaults.SecPerAudioMinBy).
func (b *Backend) defaultsFor(set Settings) EstimateDefaults {
	b.mu.Lock()
	acc := set.Accelerator
	if b.sess != nil {
		acc = b.sess.rec.Accelerator
	}
	b.mu.Unlock()
	return b.o.Defaults.For(acc)
}

// decisionErr turns a negative decision into the error Transcribe returns.
func decisionErr(d Decision) error {
	switch d.WaitingFor {
	case model.WaitRemoteAuth:
		return newErr(CodeAuthTooShort, map[string]any{"need_min": int(d.EstimateSec/60 + 0.5)}, kindFallback, d.WaitingFor,
			errors.New("the credential ends before the lecture could finish"))
	case model.WaitRemoteBalance:
		return newErr(CodeBalanceLow, map[string]any{"need": fmt.Sprintf("%.2f", d.CostCU)}, kindFallback, d.WaitingFor,
			errors.New("not enough compute units for the lecture"))
	}
	return newErr(CodeWindowExhausted, map[string]any{"remaining_min": int(d.RemainingSec / 60), "need_min": int(d.EstimateSec/60 + 0.5)},
		kindFallback, model.WaitRemoteWindow, errors.New("the lecture does not fit the remaining fair-use window"))
}

// jobErr classifies a failure of the lecture and releases the runtime
// (V14.3 step 5: "任务失败、用户取消时立即释放"): after 立即关机 or an ending
// credential the backend's runtime, here and now (they end every lecture);
// after the user's cancel the lecture's own session s, begun now and waited
// for a few seconds at most (releaseCancelled); after a failure s in the
// background (failedErr). s nil: the lecture had no
// session of its own (before or while borrowing): a cancel or a failure
// releases nothing, since the session there may be another lecture's,
// being borrowed (SEC2 L3).
func (b *Backend) jobErr(ctx, jctx context.Context, s *liveSession, err error) error {
	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errShutdownNow), errors.Is(cause, errAuthEnding):
		_ = b.release(ctx, stopReason(jctx))
		return newErr(CodeShutdownRequeue, nil, kindRequeue, "", errors.New("the remote runtime was shut down; the lecture goes back to the queue"))
	case ctx.Err() != nil:
		if s != nil {
			b.releaseCancelled(s) // returns within seconds (retry.go)
		}
		return newErr(CodeCancelled, nil, kindFallback, "", ctx.Err())
	}
	return b.failedErr(s, "", err)
}

// stopReason is the release reason of a lecture whose context jctx was
// cancelled: 立即关机 (also disconnect and quitting), the credential about
// to end, else the caller's cancel.
func stopReason(jctx context.Context) string {
	cause := context.Cause(jctx)
	switch {
	case errors.Is(cause, errShutdownNow):
		return ReleaseNow
	case errors.Is(cause, errAuthEnding):
		return ReleaseAuthEnding
	}
	return ReleaseCancelled
}

// isRuntimeErr: an *httpError of the runtime's Jupyter server.
func isRuntimeErr(err error) bool {
	var he *httpError
	return errors.As(err, &he) && strings.HasPrefix(he.endpoint, "api/")
}

func (b *Backend) currentRate(set Settings) Rate {
	b.mu.Lock()
	defer b.mu.Unlock()
	acc := set.Accelerator
	if b.sess != nil {
		acc = b.sess.rec.Accelerator
	}
	return b.rates[rateKey(acc, set.Model, set.ComputeType)]
}

func (b *Backend) setupDone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess != nil && b.sess.setupDone
}

// readWorkerFiles reads the scripts to upload and their combined hash.
func (b *Backend) readWorkerFiles() (map[string][]byte, string, error) {
	files := map[string][]byte{}
	var all []byte
	for _, name := range workerFiles {
		data, err := os.ReadFile(filepath.Join(b.o.WorkerDir, name))
		if err != nil {
			return nil, "", newErr(CodeWorkerMissing, map[string]any{"file": name}, kindFallback, "",
				fmt.Errorf("worker script %s not found", name))
		}
		files[name] = data
		all = append(all, []byte(name+"\x00")...)
		all = append(all, data...)
	}
	return files, sha256Hex(all), nil
}
