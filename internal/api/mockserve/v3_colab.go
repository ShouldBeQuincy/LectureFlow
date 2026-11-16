// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// Colab remote transcription in the mock (SPEC-v3 V14.2, V14.4, V14.5;
// .claude/v3-brief.md §8): GET /api/colab, its settings, importing,
// checking and disconnecting the authorization, the two shutdown buttons
// and "改用 Colab 处理" (POST /api/lectures/{id}/use-colab), with the bodies
// of internal/api/handlers_colab.go.
//
// colab-obs (2026-10-27; colab-obs-web): the account (Status.Account: the
// level follows the paid balance, eligibleGpus unlock the GPUs, the free
// quota; "重新查询" is POST /api/colab/account/refresh), the accelerators
// it allows (selectable_accelerators, effective_accelerator, locked_reason:
// a locked pick is kept, a T4 is borrowed meanwhile), the paid / free tier
// following the account (a PUT tier is ignored), and with a session the
// machine and its readings (Status.Machine / Usage, the 2-second history)
// plus the resource snapshot's remotes[] (internal/remote.Card, its jobs
// with the lecture titles). The rules are the ones colab-obs-core's
// internal/colab/account.go states.
//
// Scenarios (?mock=…; every other scenario starts with Colab switched off
// and not authorized, and can be set up from the settings page):
//
//	colab_off        switched off, not authorized; gcloud's credential file
//	                 is "found" after the first 检测授权 (the guide's flow);
//	                 the account is read at once after authorizing (paid)
//	colab_none       on and authorized (by the in-app sign-in: source app), no
//	                 session ("远端 GPU：未使用"); the first queued asr stage
//	                 carries remote.back_to_local; a paid account (87.42 CU)
//	colab_queued     a session waiting for an L4 (排队等卡); the first running
//	                 asr stage runs remotely at the step wait_accelerator
//	colab_running    a session 1 h 47 min into its usage window on a T4 (the
//	                 user's pick); the first running asr stage runs remotely
//	                 (upload → transcribe → download → verify follow its
//	                 progress); the machine's readings (Tesla T4, 12.7 GB)
//	colab_exhausted  as colab_running, 3 h 56 min into the window:
//	                 use-colab answers 409 remote.unavailable {remote_window}
//	colab_active     an L4 session transcribing two lectures at once (the
//	                 first running and the first queued asr stage), three
//	                 more waiting with one uploaded ahead; the machine's
//	                 readings (NVIDIA L4, 53 GB); a paid account of 286.40
//	                 CU; the first done lecture was transcribed in speed mode
//	                 (asr_timing.postprocess.split: 4 parts)
//	colab_idle       no session, a paid account read 2 minutes ago (286.40
//	                 CU, every measured GPU, the H100 not offered); L4 picked
//	colab_unknown    the account query fails (colab.unavailable): level free,
//	                 only the T4; the L4 picked stays the choice (locked:
//	                 account_unknown); "重新查询" keeps failing
//	colab_free       paid balance 0 with a free quota of 3.2 CU (refill in
//	                 9 h): the T4 is used as usual, nothing locked
//	colab_locked     the paid balance used up (0, no free quota): the A100
//	                 picked is kept, a T4 is borrowed (locked: no_balance)
//
// v3.2.x (retry in place, still billed, releasing; internal/colab retry.go):
//
//	colab_reconnect        as colab_running, but the runtime does not answer:
//	                       the running lecture retries in place
//	                       (reconnect attempt 1–3 of 3, the next try every
//	                       15 s); an error of before the session (hidden by
//	                       the front end: last_error_at < session_started_at)
//	colab_unreachable      a paid L4 session whose runtime does not answer,
//	                       no lecture on it: "仍在计费" with the CU spent
//	colab_unreachable_free the same on a free account (T4): "仍在占用本次使用窗口"
//	colab_releasing        a paid L4 session being released (stopping, the
//	                       release keeps failing: the runtime does not
//	                       answer), plus a runtime lost earlier with no
//	                       accelerator known (leftover colab.release_failed)
//
// The resource snapshot ("remote", "remotes"), the lecture views
// (stats.remote and the Colab snapshot of the remote asr stage), the device
// list (the colab:remote row) and the SSE "resources" events get the Colab
// parts through overlays registered for the loopback host names: the
// routing tree prefers patterns with a host, so the overlay runs the
// request through main.go's own handler (with the host rewritten) and edits
// its JSON, without changing main.go (v3-brief §7.5). Without Colab
// switched on the overlays pass everything through unchanged.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

const (
	colabADCPath   = `C:\Users\Student\AppData\Roaming\gcloud\application_default_credentials.json`
	colabInnerHost = "mock-inner.invalid"
)

// compute units per hour by accelerator (colab-prof's measurements; the
// H100 never measured)
var colabCUPerHour = map[string]float64{"T4": 1.07, "L4": 1.54, "G4": 8.9, "A100": 5.3, "H100": 20}

// the accelerators besides the T4 an account may unlock, in the settings' order
var colabUnlockable = []string{"L4", "A100", "G4", "H100"}

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		a := m.api
		mux.HandleFunc("GET /api/colab", a(m.colabGet))
		mux.HandleFunc("PUT /api/colab/settings", a(m.colabPutSettings))
		mux.HandleFunc("POST /api/colab/account/refresh", a(m.colabRefreshAccount))
		mux.HandleFunc("POST /api/colab/auth/import", a(m.colabImport))
		mux.HandleFunc("POST /api/colab/auth/check", a(m.colabCheck))
		mux.HandleFunc("DELETE /api/colab/auth", a(m.colabDisconnect))
		mux.HandleFunc("POST /api/colab/shutdown", a(m.colabShutdown))
		mux.HandleFunc("POST /api/lectures/{id}/use-colab", a(m.colabUse))
		for _, host := range []string{"127.0.0.1", "localhost"} {
			for _, p := range []string{"/api/resources", "/api/lectures/{id}", "/api/courses/{id}/lectures", "/api/dashboard", "/api/settings", "/api/asr/devices"} {
				mux.HandleFunc("GET "+host+p, m.colabOverlay(mux))
			}
			mux.HandleFunc("GET "+host+"/api/events", m.colabEvents(mux))
		}
	})
}

// ---------- state ----------

type colabSession struct {
	start       time.Time // requested (the window counts from here)
	queuedUntil time.Time
	bootUntil   time.Time
	stopAt      time.Time // zero = not stopping
	stopReason  string
}

type colabMock struct {
	mu            sync.Mutex
	enabled       bool
	batteryPrefer bool
	memoryOff     bool // memory mode switched off (on by default)
	settings      colab.Settings
	auth          *colab.CredentialInfo
	adcFound      bool
	held          bool
	afterCurrent  bool
	sess          *colabSession
	lastRelease   *colab.ReleaseInfo
	planned       int
	ahead         int // of the planned lectures, audio already on the runtime
	exhausted     bool
	pinned        map[string]bool // lectures sent to Colab with use-colab
	backToLocal   bool            // colab_none: the first queued asr carries remote.back_to_local
	login         *colabLoginRun  // the Google sign-in of 3.1.0 (v31_onboarding.go)
	// colab-obs: the account as Colab last answered; acctFail makes every
	// query fail with it (the reading shown, if any, stays)
	acct     colab.Account
	acctFail *model.ErrorInfo
	two      bool // colab_active: the first queued asr lecture runs remotely too
	split    bool // colab_active: the first done lecture was transcribed in speed mode
	// v3.2.x: the runtime does not answer (Status.Runtime unreachable);
	// reconnect: the running lecture retries in place; noJobs: no lecture
	// on the runtime; stuckStop: a stopping session stays stopping;
	// staleErr: an error of before the session; leftover: Status.Leftover
	unreachable bool
	reconnect   bool
	noJobs      bool
	stuckStop   bool
	staleErr    bool
	leftover    *colab.LeftoverInfo
}

var colabStates sync.Map // *mock + scenario → *colabMock

type colabKey struct {
	m  *mock
	sc string
}

func (m *mock) colabState(r *http.Request) *colabMock {
	k := colabKey{m, scenario(r)}
	if v, ok := colabStates.Load(k); ok {
		return v.(*colabMock)
	}
	v, _ := colabStates.LoadOrStore(k, newColabMock(k.sc, time.Now()))
	return v.(*colabMock)
}

// colabAuth is a credential imported at now; source is colab.SourceApp (the
// in-app sign-in) or colab.SourceGcloud (导入授权 of the user's own gcloud).
func colabAuth(lifecycle string, days int, now time.Time, source string) *colab.CredentialInfo {
	imported := now
	info := &colab.CredentialInfo{Present: true, Lifecycle: lifecycle, ImportedAt: &imported, RemainingSec: -1,
		Scopes: []string{"openid", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/colaboratory"},
		Source: source}
	if days > 0 {
		exp := imported.Add(time.Duration(days) * 24 * time.Hour)
		info.ExpiresAt = &exp
	}
	return info
}

// colabAccount is a reading of ccu-info taken at: the paid balance, the
// GPUs it lists for the account and the free quota (nil: none sent).
func colabAccount(at time.Time, balance float64, eligible, ineligible []string, quota *float64, refill *time.Time) colab.Account {
	rate, n := 0.0, 0
	a := colab.Account{ReadAt: &at, ReadOK: true, Level: colab.LevelFree, PaidBalanceCU: &balance, AccountRateCUPerHour: &rate, AccountRuntimes: &n,
		FreeQuotaCU: quota, FreeRefillAt: refill, EligibleGPUs: append([]string{}, eligible...), IneligibleGPUs: append([]string{}, ineligible...),
		EligibilityKnown: true}
	if balance > 0 {
		a.Level = colab.LevelPaid
	}
	return a
}

// colabPaid: a paid account that may borrow every measured GPU (not the H100).
func colabPaid(at time.Time, balance float64) colab.Account {
	return colabAccount(at, balance, []string{"G4", "A100", "L4", "T4"}, []string{"H100"}, nil, nil)
}

// colabUnread: never read (level free, only the T4).
func colabUnread() colab.Account {
	return colab.Account{Level: colab.LevelFree, EligibleGPUs: []string{}, IneligibleGPUs: []string{}}
}

func newColabMock(sc string, now time.Time) *colabMock {
	c := &colabMock{settings: colab.DefaultSettings(), pinned: map[string]bool{}, acct: colabUnread()}
	switch sc {
	case "colab_none", "colab_queued", "colab_running", "colab_exhausted", "colab_active", "colab_idle", "colab_unknown", "colab_free", "colab_locked",
		"colab_reconnect", "colab_unreachable", "colab_unreachable_free", "colab_releasing":
		c.enabled, c.adcFound = true, true
		// signed in in the app a few hours ago with the default 7 days
		c.auth = colabAuth("7d", 7, now.Add(-4*time.Hour-12*time.Minute), colab.SourceApp)
		c.acct = colabPaid(now.Add(-2*time.Minute), 87.42)
	}
	pick := func(a string) { c.settings.Accelerator, c.settings.AcceleratorChosen = a, true }
	switch sc {
	case "colab_none":
		c.backToLocal = true
	case "colab_queued":
		c.sess = &colabSession{start: now.Add(-3 * time.Minute), queuedUntil: now.Add(2 * time.Hour), bootUntil: now.Add(2 * time.Hour)}
		c.planned = 2
	case "colab_running":
		pick("T4")
		c.sess = &colabSession{start: now.Add(-107 * time.Minute), queuedUntil: now.Add(-105 * time.Minute), bootUntil: now.Add(-103 * time.Minute)}
		c.planned, c.ahead = 2, 1
	case "colab_exhausted":
		pick("T4")
		c.sess = &colabSession{start: now.Add(-236 * time.Minute), queuedUntil: now.Add(-230 * time.Minute), bootUntil: now.Add(-228 * time.Minute)}
		c.planned, c.exhausted = 2, true
	case "colab_active":
		pick("L4")
		c.acct = colabPaid(now.Add(-40*time.Second), 286.40)
		rate, n := 1.54, 1
		c.acct.AccountRateCUPerHour, c.acct.AccountRuntimes = &rate, &n
		c.sess = &colabSession{start: now.Add(-26 * time.Minute), queuedUntil: now.Add(-25 * time.Minute), bootUntil: now.Add(-24 * time.Minute)}
		c.planned, c.ahead, c.two, c.split = 3, 1, true, true
	case "colab_idle":
		pick("L4")
		c.acct = colabPaid(now.Add(-2*time.Minute), 286.40)
	case "colab_unknown":
		pick("L4")
		c.acct = colabUnread()
		c.acctFail = &model.ErrorInfo{Code: colab.CodeUnavailable, Params: map[string]any{"endpoint": "tun/m/ccu-info"}}
		c.acct.ReadError = c.acctFail
	case "colab_free":
		quota, refill := 3.2, now.Add(9*time.Hour).Truncate(time.Hour)
		c.acct = colabAccount(now.Add(-3*time.Minute), 0, []string{"T4"}, []string{"L4", "A100", "G4", "H100"}, &quota, &refill)
	case "colab_locked":
		pick("A100")
		c.acct = colabAccount(now.Add(-1*time.Minute), 0, []string{"G4", "A100", "L4", "T4"}, []string{"H100"}, nil, nil)
	case "colab_reconnect":
		pick("T4")
		c.sess = &colabSession{start: now.Add(-38 * time.Minute), queuedUntil: now.Add(-36 * time.Minute), bootUntil: now.Add(-35 * time.Minute)}
		c.planned, c.unreachable, c.reconnect, c.staleErr = 1, true, true, true
	case "colab_unreachable":
		pick("L4")
		c.acct = colabPaid(now.Add(-3*time.Minute), 286.40)
		c.sess = &colabSession{start: now.Add(-52 * time.Minute), queuedUntil: now.Add(-51 * time.Minute), bootUntil: now.Add(-50 * time.Minute)}
		c.unreachable, c.noJobs = true, true
	case "colab_unreachable_free":
		quota, refill := 2.1, now.Add(7*time.Hour).Truncate(time.Hour)
		c.acct = colabAccount(now.Add(-3*time.Minute), 0, []string{"T4"}, []string{"L4", "A100", "G4", "H100"}, &quota, &refill)
		c.sess = &colabSession{start: now.Add(-44 * time.Minute), queuedUntil: now.Add(-43 * time.Minute), bootUntil: now.Add(-42 * time.Minute)}
		c.unreachable, c.noJobs = true, true
	case "colab_releasing":
		pick("L4")
		c.acct = colabPaid(now.Add(-3*time.Minute), 286.40)
		c.sess = &colabSession{start: now.Add(-61 * time.Minute), queuedUntil: now.Add(-60 * time.Minute), bootUntil: now.Add(-59 * time.Minute),
			stopAt: now.Add(-40 * time.Second), stopReason: colab.ReleaseFailed}
		c.unreachable, c.stuckStop, c.held = true, true, true
		c.leftover = &colab.LeftoverInfo{Since: now.Add(-3 * time.Hour), Code: colab.CodeReleaseFailed}
	}
	return c
}

// runtimeInfo is Status.Runtime of a session (nil without one): held from
// the boot on, unreachable in the v3.2.x scenarios, billed on a paid account.
func (c *colabMock) runtimeInfo(now time.Time) *colab.RuntimeInfo {
	st := c.sessionState(now)
	if c.sess == nil || st == colab.StateQueued || st == colab.StateBooting {
		if c.sess == nil {
			return nil
		}
		return &colab.RuntimeInfo{Reachable: colab.ReachUnknown}
	}
	ri := &colab.RuntimeInfo{Held: true, Reachable: colab.ReachOK, Billing: c.tier() == colab.TierPaid}
	if c.unreachable {
		since := now.Add(-25 * time.Second).Truncate(time.Second)
		ri.Reachable, ri.UnreachableSince = colab.ReachUnreachable, &since
	}
	return ri
}

// reconnectOf is a retrying lecture's reconnect: attempt 1–3 of 3, the
// next try at the next quarter minute.
func reconnectOf(now time.Time) *remote.Reconnect {
	next := now.Truncate(15 * time.Second).Add(15 * time.Second)
	return &remote.Reconnect{Attempt: 1 + int(now.Unix()/15%3), Of: 3, NextAt: next.UTC()}
}

// on: the overlays add Colab (the real server: enabled; the resource
// snapshot has "remote" from then on, also without a credential).
func (c *colabMock) on() bool { return c.enabled }

// advance ends a stopping session (3 s after the button).
func (c *colabMock) advance(now time.Time) {
	if s := c.sess; s != nil && !s.stopAt.IsZero() && !c.stuckStop && now.After(s.stopAt.Add(3*time.Second)) {
		c.lastRelease = &colab.ReleaseInfo{Reason: s.stopReason, At: s.stopAt.Add(3 * time.Second)}
		c.sess = nil
		c.afterCurrent = false
	}
}

func (c *colabMock) sessionState(now time.Time) string {
	s := c.sess
	switch {
	case s == nil && c.lastRelease != nil:
		return colab.StateReleased
	case s == nil:
		return colab.StateNone
	case !s.stopAt.IsZero():
		return colab.StateStopping
	case now.Before(s.queuedUntil):
		return colab.StateQueued
	case now.Before(s.bootUntil):
		return colab.StateBooting
	}
	return colab.StateRunning
}

// ---------- the account (colab-obs; the rules of internal/colab/account.go) ----------

// account is Status.Account. A credential (an import or the in-app sign-in)
// is followed by a reading at once, as on the real server. c.mu is held.
func (c *colabMock) account(now time.Time) colab.Account {
	if c.auth != nil && c.acct.ReadAt == nil && c.acctFail == nil {
		c.acct = colabPaid(now, 87.42)
	}
	return c.acct
}

func (c *colabMock) paidBalance() float64 {
	if c.acct.PaidBalanceCU == nil {
		return 0
	}
	return *c.acct.PaidBalanceCU
}

// selectable: the T4 always, the others listed in eligibleGpus with a paid balance.
func (c *colabMock) selectable() []string {
	out := []string{"T4"}
	if !c.acct.EligibilityKnown || c.paidBalance() <= 0 {
		return out
	}
	for _, a := range colabUnlockable {
		if slices.Contains(c.acct.EligibleGPUs, a) {
			out = append(out, a)
		}
	}
	return out
}

// preferred: the user's pick, else the level's default (paid L4, free T4).
func (c *colabMock) preferred() string {
	if c.settings.AcceleratorChosen {
		return c.settings.Accelerator
	}
	if c.acct.Level == colab.LevelPaid {
		return colab.DefaultAccelerator(colab.TierPaid)
	}
	return colab.DefaultAccelerator(colab.TierFree)
}

// effective is the accelerator the next borrowing asks for and, when that
// is not the preferred one, why.
func (c *colabMock) effective() (string, string) {
	p := c.preferred()
	if slices.Contains(c.selectable(), p) {
		return p, ""
	}
	switch {
	case !c.acct.EligibilityKnown:
		return "T4", colab.LockAccountUnknown
	case c.paidBalance() <= 0:
		return "T4", colab.LockNoBalance
	}
	return "T4", colab.LockIneligible
}

func (c *colabMock) tier() string {
	if c.acct.Level == colab.LevelPaid {
		return colab.TierPaid
	}
	return colab.TierFree
}

// userSettings: the settings the page shows (the preferred accelerator, the
// account's tier).
func (c *colabMock) userSettings() colab.Settings {
	s := c.settings
	s.Accelerator, s.Tier = c.preferred(), c.tier()
	return s
}

// ---------- the remote lectures, the machine and its readings ----------

// remoteLecture is a lecture whose asr runs on Colab: the first running asr
// stage of the store while a session is queued, booting or running (and in
// colab_active the first queued one as well, two at once).
type remoteLecture struct {
	id       string
	title    string
	progress float64
	started  time.Time
	second   bool // its asr stage is queued in the store; the overlay shows it running
}

func (c *colabMock) remoteLectures(s *store, now time.Time) []remoteLecture {
	st := c.sessionState(now)
	if st != colab.StateQueued && st != colab.StateBooting && st != colab.StateRunning || c.noJobs {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var first, second *remoteLecture
	for _, l := range s.lectures {
		j := stageIndex(&l, model.StageASR)
		if j < 0 {
			continue
		}
		switch l.Stages[j].Status {
		case model.StatusRunning:
			if first != nil {
				continue
			}
			rl := &remoteLecture{id: l.ID, title: l.Title, progress: l.Stages[j].Progress, started: now.Add(-6 * time.Minute)}
			if p := l.Stages[j].StartedAt; p != nil {
				if t, err := time.Parse(time.RFC3339, *p); err == nil {
					rl.started = t
				}
			}
			first = rl
		case model.StatusQueued:
			if c.two && second == nil && st == colab.StateRunning {
				second = &remoteLecture{id: l.ID, title: l.Title, progress: 0.18, started: now.Add(-2 * time.Minute), second: true}
			}
		}
	}
	var out []remoteLecture
	for _, rl := range []*remoteLecture{first, second} {
		if rl != nil {
			out = append(out, *rl)
		}
	}
	return out
}

// step of a remote lecture: its progress mapped on the five steps.
func (c *colabMock) step(rl remoteLecture, now time.Time) model.RemoteStats {
	acc, _ := c.effective()
	rs := model.RemoteStats{Steps: append([]string{}, model.RemoteSteps...), Accelerator: acc, EtaSec: -1}
	switch st := c.sessionState(now); {
	case st == colab.StateQueued || st == colab.StateBooting:
		rs.Step = model.RemoteStepWaitAccelerator
		if st == colab.StateQueued {
			rs.WaitingFor = model.WaitRemoteQueue
		}
		return rs
	case rl.progress < 0.03:
		rs.Step = model.RemoteStepUploadAudio
	case rl.progress < 0.93:
		rs.Step, rs.Stage = model.RemoteStepRemoteTranscribe, "transcribe"
	case rl.progress < 0.97:
		rs.Step = model.RemoteStepDownloadResult
	default:
		rs.Step = model.RemoteStepVerify
	}
	rs.EtaSec = int64((1 - rl.progress) * 26 * 60)
	if acc == "L4" { // faster than the T4 the five minutes count for
		rs.EtaSec = int64((1 - rl.progress) * 3 * 60)
	}
	return rs
}

func (c *colabMock) cuPerHour() float64 {
	acc, _ := c.effective()
	if v, ok := colabCUPerHour[acc]; ok {
		return v
	}
	return 2
}

// colabMachines: what nvidia-smi and /proc report on each runtime (the
// measured specs of colab-setup-time.md §5.1; mock driver versions).
var colabMachines = map[string]remote.Machine{
	"T4":   {GPUName: "Tesla T4", GPUArch: "Turing", ComputeCap: "7.5", VRAMTotalMB: ptr(15360), VCPUs: ptr(2), MemTotalMB: ptr(13002)},
	"L4":   {GPUName: "NVIDIA L4", GPUArch: "Ada Lovelace", ComputeCap: "8.9", VRAMTotalMB: ptr(23034), VCPUs: ptr(12), MemTotalMB: ptr(54272)},
	"A100": {GPUName: "NVIDIA A100-SXM4-40GB", GPUArch: "Ampere", ComputeCap: "8.0", VRAMTotalMB: ptr(40960), VCPUs: ptr(12), MemTotalMB: ptr(87040)},
	"G4":   {GPUName: "NVIDIA RTX PRO 6000 Blackwell Server Edition", GPUArch: "Blackwell", ComputeCap: "12.0", VRAMTotalMB: ptr(97887), VCPUs: ptr(24), MemTotalMB: ptr(185344)},
	"H100": {GPUName: "NVIDIA H100 80GB HBM3", GPUArch: "Hopper", ComputeCap: "9.0", VRAMTotalMB: ptr(81559), VCPUs: ptr(26), MemTotalMB: ptr(240640)},
}

func ptr[T any](v T) *T { return &v }

// machine is Status.Machine while the session runs (nil before).
func (c *colabMock) machine(now time.Time) *remote.Machine {
	if c.sessionState(now) != colab.StateRunning && c.sessionState(now) != colab.StateStopping {
		return nil
	}
	acc, _ := c.effective()
	m, ok := colabMachines[acc]
	if !ok {
		return nil
	}
	m.Source, m.Driver, m.CUDA, m.GPUCount, m.Storage, m.Requested = remote.SourceNvidiaSMI, "550.54.15", "12.4", ptr(1), remote.StorageMemory, acc
	if c.memoryOff {
		m.Storage = remote.StorageDisk
	}
	return &m
}

// wave is a reading of the history slot i (one per 2 s): base with a slow
// swing, kept within 0–100.
func wave(base, amp float64, i int64) int {
	v := base + amp*math.Sin(float64(i)*0.7) + amp*0.4*math.Sin(float64(i)*2.3)
	return int(math.Round(math.Max(0, math.Min(100, v))))
}

// usage is Status.Usage while the session runs: the worker's readings with
// jobs running (memory split into memory disk, Whisper, system and data),
// else the runtime's own report (used / free only, no CPU).
func (c *colabMock) usage(now time.Time, jobs int) *remote.Usage {
	m := c.machine(now)
	if m == nil {
		return nil
	}
	slot := now.Unix() / 2
	at := now.Truncate(2 * time.Second)
	total := *m.MemTotalMB
	u := &remote.Usage{At: at, AgeSec: int(now.Sub(at).Seconds()), Source: remote.SourceWorker}
	cpuBase, gpuBase := 38.0, 90.0
	if m.Requested == "T4" {
		cpuBase, gpuBase = 57, 88
	}
	hist := func(base, amp float64) []*int {
		out := make([]*int, remote.HistoryLen)
		for k := range out {
			out[k] = ptr(wave(base, amp, slot-int64(remote.HistoryLen-1-k)))
		}
		return out
	}
	if jobs == 0 { // between lectures: the runtime's resources endpoint only
		used := 4100
		u.Source = remote.SourceColabResources
		u.RAM = []remote.RAMSegment{{Kind: remote.SegUsed, MB: used}, {Kind: remote.SegFree, MB: total - used}}
		u.GPUUtilPct, u.VRAMUsedMB = ptr(0), ptr(310)
		u.CPUHistory, u.GPUHistory, u.VRAMHistory = make([]*int, remote.HistoryLen), hist(0, 0), make([]*int, remote.HistoryLen)
		return u
	}
	ramdisk, whisper, system, data := 3441, 4731, 2365, 491
	vram, vramW := 8090, 7612
	if m.Requested == "T4" {
		whisper, system, data, vram, vramW = 3390, 2437, 276, 4710, 4280
	}
	if c.memoryOff { // disk mode: no memory disk, the job files on the disk
		u.DataDiskMB, ramdisk, data = ptr(data), 0, 0
	}
	free := total - ramdisk - whisper - system - data
	for _, s := range []remote.RAMSegment{{Kind: remote.SegRAMDisk, MB: ramdisk}, {Kind: remote.SegWhisper, MB: whisper}, {Kind: remote.SegSystem, MB: system}, {Kind: remote.SegData, MB: data}, {Kind: remote.SegFree, MB: free}} {
		if s.MB > 0 || s.Kind == remote.SegFree {
			u.RAM = append(u.RAM, s)
		}
	}
	u.CPUHistory, u.GPUHistory = hist(cpuBase, 4), hist(gpuBase, 3)
	u.VRAMHistory = make([]*int, remote.HistoryLen)
	for k := range u.VRAMHistory {
		u.VRAMHistory[k] = ptr(vram)
	}
	u.CPUPct, u.GPUUtilPct = u.CPUHistory[remote.HistoryLen-1], u.GPUHistory[remote.HistoryLen-1]
	u.VRAMUsedMB, u.VRAMWhisperMB, u.VRAMReservedMB = ptr(vram), ptr(vramW), ptr(392)
	u.GPUTempC, u.GPUPowerW = ptr(61), ptr(58.5)
	return u
}

func (c *colabMock) status(rls []remoteLecture, now time.Time) colab.Status {
	acct := c.account(now) // first: a credential without a reading is read now
	acc, locked := c.effective()
	st := colab.Status{
		Service: "colab", Configured: c.auth != nil, Accelerator: acc, Tier: c.tier(),
		State: c.sessionState(now), WindowTotalSec: int64(colab.FairWindow.Seconds()), CUPerHour: c.cuPerHour(),
		ShutdownAfter: c.afterCurrent, LastRelease: c.lastRelease, Planned: c.planned, Slots: 1,
		Account: acct, SelectableAccelerators: c.selectable(), EffectiveAccelerator: acc, LockedReason: locked,
	}
	if acc == "L4" || acc == "A100" || acc == "G4" { // two lectures at once on the larger GPUs
		st.Slots = 2
	}
	if c.auth != nil {
		st.Auth = *c.authInfo(now)
	}
	if c.acct.ReadAt != nil && c.acct.PaidBalanceCU != nil && c.acct.Level == colab.LevelPaid {
		b := *c.acct.PaidBalanceCU
		st.BalanceCU = &b
	}
	left := 4
	if s := c.sess; s != nil {
		used := now.Sub(s.start)
		if used > colab.FairWindow {
			used = colab.FairWindow
		}
		st.WindowUsedSec = int64(used.Seconds())
		if now.After(s.queuedUntil) {
			st.SessionSpentCU = round2(now.Sub(s.queuedUntil).Hours() * c.cuPerHour())
		}
		left = int((colab.FairWindow - used).Minutes() / 50)
		st.UploadedAhead = c.ahead
		st.Machine, st.Usage = c.machine(now), c.usage(now, len(rls))
		started := s.start.UTC().Truncate(time.Second)
		st.SessionStartedAt = &started
		st.Runtime = c.runtimeInfo(now)
		if c.staleErr { // an error of the session before (the front end hides it)
			at := started.Add(-12 * time.Minute)
			st.LastError, st.LastErrorAt = &model.ErrorInfo{Code: colab.CodeConnectionLost}, &at
		}
	}
	st.Leftover = c.leftover
	if c.exhausted {
		left = 0
	}
	st.LecturesLeft = left
	if c.planned > 0 {
		st.PlannedFit = min(c.planned, left)
		st.LecturesLeft = st.PlannedFit
		st.PlanCostCU = round2(float64(st.PlannedFit) * 0.45 * c.cuPerHour())
	}
	for _, rl := range rls {
		rs := c.step(rl, now)
		overall := rl.progress
		if rs.Step == model.RemoteStepWaitAccelerator {
			overall = 0
		}
		j := colab.JobInfo{ID: asrJobID(rl.id), Step: rs.Step, Stage: rs.Stage, Fraction: -1, Overall: overall,
			ETASec: float64(rs.EtaSec), StartedAt: rl.started}
		if c.reconnect && st.State == colab.StateRunning {
			j.Reconnect = reconnectOf(now)
		}
		st.Jobs = append(st.Jobs, j)
	}
	if len(st.Jobs) > 0 {
		st.Current = &st.Jobs[0]
	}
	return st
}

// card is the snapshot's remotes[] entry (internal/remote.Card) of the
// status; its jobs carry the lecture titles (lecture_title, as the device
// cards' tasks do).
func (c *colabMock) card(st colab.Status, rls []remoteLecture) any {
	card := remote.Card{ID: model.DeviceColab, Kind: remote.KindColab, Label: "Google Colab", State: st.State,
		Machine: st.Machine, Usage: st.Usage, Slots: st.Slots, Queued: st.Planned, UploadedAhead: st.UploadedAhead, Colab: st}
	for i, rl := range rls {
		j := st.Jobs[i]
		p := j.Overall
		eta := int(j.ETASec)
		card.Jobs = append(card.Jobs, remote.CardJob{LectureID: rl.id, Stage: colabCardStage(j), Progress: &p, ETASec: &eta, Reconnect: j.Reconnect})
	}
	if st.LastError != nil {
		card.LastError = &remote.CardError{Code: st.LastError.Code, Params: st.LastError.Params, At: time.Now()}
		if st.LastErrorAt != nil {
			card.LastError.At = *st.LastErrorAt
		}
	}
	v := toJSON(card)
	if obj, ok := v.(map[string]any); ok {
		if jobs, ok := obj["jobs"].([]any); ok {
			for i, j := range jobs {
				if jm, ok := j.(map[string]any); ok && i < len(rls) {
					jm["lecture_title"] = rls[i].title
				}
			}
		}
	}
	return v
}

// colabCardStage is a card job's stage as the real server writes it
// (internal/resources/remotes.go): the worker's own while it transcribes,
// else the step (wait, upload, download, verify).
func colabCardStage(j colab.JobInfo) string {
	switch j.Step {
	case model.RemoteStepRemoteTranscribe:
		if j.Stage != "" {
			return j.Stage
		}
		return "check"
	case model.RemoteStepUploadAudio:
		return "upload"
	case model.RemoteStepDownloadResult:
		return "download"
	case model.RemoteStepVerify:
		return "verify"
	}
	return "wait"
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

func (c *colabMock) authInfo(now time.Time) *colab.CredentialInfo {
	if c.auth == nil {
		return &colab.CredentialInfo{RemainingSec: -1}
	}
	a := *c.auth
	a.RemainingSec = -1
	if a.ExpiresAt != nil {
		a.RemainingSec = max(0, int64(a.ExpiresAt.Sub(now).Seconds()))
	}
	return &a
}

func (c *colabMock) adc() api.ColabADCView {
	v := api.ColabADCView{Detected: c.adcFound, Path: colabADCPath, Found: c.adcFound, LoginCommand: colab.GcloudLoginCommand()}
	if !c.adcFound {
		v.Problem = colab.CodeADCNotFound
	}
	return v
}

func (m *mock) colabView(r *http.Request, c *colabMock) api.ColabView {
	now := time.Now()
	c.advance(now)
	rls := c.remoteLectures(m.storeFor(r), now)
	return api.ColabView{
		Status:   c.status(rls, now),
		Settings: api.ColabSettingsView{Settings: c.userSettings(), MemoryMode: !c.memoryOff, Enabled: c.enabled, BatteryPrefer: c.batteryPrefer},
		ADC:      c.adc(),
		Held:     c.held,
	}
}

// ---------- endpoints ----------

func (m *mock) colabGet(w http.ResponseWriter, r *http.Request, _ *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	writeJSON(w, http.StatusOK, m.colabView(r, c))
}

// "重新查询": the account is read now (colab_unknown: the query fails again,
// the reading shown stays).
func (m *mock) colabRefreshAccount(w http.ResponseWriter, r *http.Request, _ *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	switch {
	case c.acctFail != nil:
		c.acct.ReadOK, c.acct.ReadError = false, c.acctFail
	case c.auth != nil:
		if c.acct.ReadAt == nil {
			c.acct = colabPaid(now, 87.42)
		}
		c.acct.ReadAt, c.acct.ReadOK, c.acct.ReadError = &now, true, nil
	}
	writeJSON(w, http.StatusOK, m.colabView(r, c))
}

func (m *mock) colabPutSettings(w http.ResponseWriter, r *http.Request, _ *store) {
	var p api.ColabSettingsPatch
	if err := decodeBody(r, &p); err != nil {
		writeErr(w, err)
		return
	}
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.settings
	if p.Accelerator != nil {
		set.Accelerator, set.AcceleratorChosen = *p.Accelerator, true
	}
	// p.Tier is ignored: the tier follows the account (colab-obs)
	if p.FastMode != nil {
		set.FastMode = *p.FastMode
	}
	if p.MaxQueueWaitMin != nil {
		set.MaxQueueWaitMin = *p.MaxQueueWaitMin
	}
	if p.Model != nil {
		set.Model = *p.Model
	}
	if p.ComputeType != nil {
		set.ComputeType = *p.ComputeType
	}
	if p.IdleReleaseMin != nil {
		set.IdleReleaseMin = *p.IdleReleaseMin
	}
	if p.WindowCooldownMin != nil {
		set.WindowCooldownMin = *p.WindowCooldownMin
	}
	if p.HighMem != nil {
		set.HighMem = *p.HighMem
	}
	if err := set.Validate(); err != nil {
		var info model.ErrorInfo
		var ce *model.CodedError
		if errors.As(err, &ce) {
			info = model.ErrorInfo{Code: ce.Info.Code, Params: ce.Info.Params}
		} else {
			info = model.ErrorInfo{Code: colab.CodeSettingsInvalid, Params: map[string]any{"field": "settings"}}
		}
		writeJSON(w, http.StatusBadRequest, api.ErrorBody{Error: info})
		return
	}
	c.settings = set
	if p.MemoryMode != nil {
		c.memoryOff = !*p.MemoryMode
	}
	if p.Enabled != nil {
		c.enabled = *p.Enabled
		if c.enabled {
			c.held = false
		}
	}
	if p.BatteryPrefer != nil {
		c.batteryPrefer = *p.BatteryPrefer
	}
	m.storeFor(r).notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, m.colabView(r, c))
}

func (m *mock) colabImport(w http.ResponseWriter, r *http.Request, _ *store) {
	var req api.ColabImportRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.adcFound {
		codedError(w, http.StatusBadRequest, colab.CodeADCNotFound, map[string]any{"path": colabADCPath})
		return
	}
	now := time.Now()
	switch req.Lifecycle {
	case "", colab.LifecycleDays:
		days := req.Days
		if days == 0 {
			days = 7
		}
		if days < 1 || days > 90 {
			codedError(w, http.StatusBadRequest, colab.CodeLifecycleInvalid, map[string]any{"value": req.Lifecycle})
			return
		}
		c.auth = colabAuth(itoa(days)+"d", days, now, colab.SourceGcloud)
	case colab.LifecycleSession, colab.LifecycleManual:
		c.auth = colabAuth(req.Lifecycle, 0, now, colab.SourceGcloud)
	default:
		codedError(w, http.StatusBadRequest, colab.CodeLifecycleInvalid, map[string]any{"value": req.Lifecycle})
		return
	}
	m.storeFor(r).notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, c.authInfo(now))
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 检测授权: the guide's flow — after the first check the file is "there".
func (m *mock) colabCheck(w http.ResponseWriter, r *http.Request, _ *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.adcFound = true
	writeJSON(w, http.StatusOK, api.ColabCheckView{ADC: c.adc(), Auth: *c.authInfo(time.Now())})
}

func (m *mock) colabDisconnect(w http.ResponseWriter, r *http.Request, _ *store) {
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	v := api.ColabDisconnectView{Warnings: []model.ErrorInfo{}, Notes: []model.ErrorInfo{}}
	if c.sess != nil {
		c.sess = nil
		c.lastRelease = &colab.ReleaseInfo{Reason: "disconnect", At: now}
		v.Released = true
	}
	if c.auth != nil {
		// as the real server: only a credential of the user's own gcloud
		// signs gcloud's application-default login out (the same token);
		// the in-app sign-in's was never in the user's gcloud
		if c.auth.Source != colab.SourceApp {
			v.Notes = append(v.Notes, model.ErrorInfo{Code: "remote.gcloud_signed_out"})
			c.adcFound = false
		}
		c.auth = nil
		v.Revoked = true
	}
	c.login = nil
	m.storeFor(r).notify(api.ChangedEvent{Kind: "settings"})
	writeJSON(w, http.StatusOK, v)
}

func (m *mock) colabShutdown(w http.ResponseWriter, r *http.Request, _ *store) {
	var req api.ColabShutdownRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	c := m.colabState(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	switch req.Mode {
	case "after_current":
		c.afterCurrent = true
		if len(c.remoteLectures(m.storeFor(r), now)) == 0 && c.sess != nil {
			c.sess.stopAt, c.sess.stopReason = now, "after_current"
		}
	case "now":
		if c.sess != nil {
			c.sess.stopAt, c.sess.stopReason = now, "shutdown_now"
		}
	default:
		codedError(w, http.StatusBadRequest, "request.bad_param", map[string]any{"name": "mode"})
		return
	}
	c.held = true
	c.planned, c.ahead = 0, 0
	clear(c.pinned)
	writeJSON(w, http.StatusOK, m.colabView(r, c))
}

func (m *mock) colabUse(w http.ResponseWriter, r *http.Request, s *store) {
	id := r.PathValue("id")
	c := m.colabState(r)
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		codedError(w, http.StatusConflict, "remote.disabled", nil)
		return
	}
	if c.auth == nil {
		c.mu.Unlock()
		codedError(w, http.StatusConflict, "remote.unavailable", map[string]any{"waiting_for": model.WaitRemoteAuth})
		return
	}
	if c.exhausted && c.sessionState(time.Now()) == colab.StateRunning {
		c.mu.Unlock()
		codedError(w, http.StatusConflict, "remote.unavailable", map[string]any{"waiting_for": model.WaitRemoteWindow})
		return
	}
	c.mu.Unlock()
	s.mu.Lock()
	l, err := s.findLecture(id)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, err)
		return
	}
	j := stageIndex(l, model.StageASR)
	status := ""
	if j >= 0 {
		status = l.Stages[j].Status
	}
	s.mu.Unlock()
	switch status {
	case model.StatusQueued, model.StatusRunning, model.StatusFailed, model.StatusCancelled:
	default:
		codedError(w, http.StatusConflict, "state.conflict", map[string]any{"stage": model.StageASR, "status": status})
		return
	}
	c.mu.Lock()
	c.pinned[id] = true
	c.held = false
	c.mu.Unlock()
	s.notify(api.ChangedEvent{Kind: "lecture", ID: id})
	// the lecture view through the overlay (the pinned snapshot)
	r2 := r.Clone(r.Context())
	r2.Method, r2.Host, r2.Body = http.MethodGet, colabInnerHost, http.NoBody
	r2.URL.Path = "/api/lectures/" + id
	rec := httptest.NewRecorder()
	m.colabOverlayServe(rec, r2, s, c)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// ---------- overlays ----------

func innerRequest(r *http.Request) *http.Request {
	r2 := r.Clone(r.Context())
	r2.Host = colabInnerHost
	return r2
}

func (m *mock) colabOverlay(mux *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := m.colabState(r)
		c.mu.Lock()
		on := c.on() || len(c.pinned) > 0 || m.sshOn(r) // ssh-1b: the SSH servers' cards (ssh.go)
		c.mu.Unlock()
		if !on {
			mux.ServeHTTP(w, innerRequest(r))
			return
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, innerRequest(r))
		c.mu.Lock()
		defer c.mu.Unlock()
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			body = m.colabFix(r, c, r.URL.Path, body)
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	}
}

// colabOverlayServe answers r2 (a GET of the lecture view) with the overlay.
func (m *mock) colabOverlayServe(w http.ResponseWriter, r2 *http.Request, s *store, c *colabMock) {
	rec := httptest.NewRecorder()
	m.api(m.getLecture)(rec, withPath(r2, "id", strings.TrimPrefix(r2.URL.Path, "/api/lectures/")))
	c.mu.Lock()
	defer c.mu.Unlock()
	body := rec.Body.Bytes()
	if rec.Code == http.StatusOK {
		body = m.colabFix(r2, c, r2.URL.Path, body)
	}
	writeRaw(w, rec.Code, body)
}

func withPath(r *http.Request, k, v string) *http.Request {
	r.SetPathValue(k, v)
	return r
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// colabFix edits one JSON response of path. c.mu is held.
func (m *mock) colabFix(r *http.Request, c *colabMock, path string, body []byte) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body
	}
	now := time.Now()
	c.advance(now)
	s := m.storeFor(r)
	rls := c.remoteLectures(s, now)
	switch {
	case path == "/api/resources":
		v = c.fixResources(v, rls, now)
	case path == "/api/settings":
		if obj, ok := v.(map[string]any); ok {
			if list, ok := obj["devices"].([]any); ok {
				obj["devices"] = c.fixDevices(list)
			}
		}
	case path == "/api/asr/devices":
		if list, ok := v.([]any); ok {
			v = c.fixDevices(list)
		}
	default:
		v = c.fixLectures(v, rls, c.backToLocalLecture(s), c.splitLecture(s), now)
	}
	v = m.sshFix(r, path, v) // ssh-1b (ssh.go)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return body
	}
	return buf.Bytes()
}

// fixResources adds "remote" and "remotes" (while switched on) and takes
// the remote lectures off the local device cards.
func (c *colabMock) fixResources(v any, rls []remoteLecture, now time.Time) any {
	obj, ok := v.(map[string]any)
	if !ok || !c.on() {
		return v
	}
	st := c.status(rls, now)
	obj["remote"] = toJSON(st)
	obj["remotes"] = []any{c.card(st, rls)}
	if len(rls) == 0 {
		return v
	}
	remoteIDs := map[string]bool{}
	for _, rl := range rls {
		remoteIDs[rl.id] = true
	}
	if devs, ok := obj["devices"].([]any); ok {
		for _, d := range devs {
			dm, ok := d.(map[string]any)
			if !ok {
				continue
			}
			tasks, _ := dm["tasks"].([]any)
			keep := []any{}
			for _, tk := range tasks {
				if tm, ok := tk.(map[string]any); ok {
					if id, _ := tm["lecture_id"].(string); remoteIDs[id] {
						if num, ok := dm["slots_used"].(json.Number); ok {
							if n, err := num.Int64(); err == nil && n > 0 {
								dm["slots_used"] = n - 1
							}
						}
						continue
					}
				}
				keep = append(keep, tk)
			}
			if tasks != nil {
				dm["tasks"] = keep
			}
		}
	}
	return v
}

func toJSON(x any) any {
	b, _ := json.Marshal(x)
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&v)
	return v
}

// fixDevices appends the colab:remote row while Colab is on and authorized
// (the device table of the real server, NOTES v14-colab-integrate 3).
func (c *colabMock) fixDevices(list []any) []any {
	if !c.on() || c.auth == nil {
		return list
	}
	acc, _ := c.effective()
	row := api.ASRDeviceView{
		ID: model.DeviceColab, Backend: "colab", Kind: model.DeviceKindRemote, Label: "Google Colab · " + acc,
		Enabled: true, Model: c.settings.Model, Precision: c.settings.ComputeType, MaxParallel: 1, Priority: 5, Present: true,
		SelfTest: api.SelfTestView{DeviceID: model.DeviceColab, State: "idle"}, Running: []api.DeviceTaskView{},
		Options: api.DeviceOptions{Models: []string{c.settings.Model}, Precisions: []string{c.settings.ComputeType}},
	}
	return append(list, toJSON(row))
}

// fixLectures walks a lecture view, a lecture list or the dashboard: the
// remote lectures' asr stages get stats.remote and the Colab snapshot (the
// second one of colab_active shows running); a pinned queued one the Colab
// snapshot; in colab_none the first queued asr stage the note
// remote.back_to_local; in colab_active the first done one the speed mode's
// parts (asr_timing.postprocess.split).
func (c *colabMock) fixLectures(v any, rls []remoteLecture, backID, splitID string, now time.Time) any {
	byID := map[string]remoteLecture{}
	for _, rl := range rls {
		byID[rl.id] = rl
	}
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			if stages, ok := t["stages"].([]any); ok {
				id, _ := t["id"].(string)
				for _, s := range stages {
					sm, ok := s.(map[string]any)
					if !ok || sm["stage"] != model.StageASR {
						continue
					}
					status, _ := sm["status"].(string)
					rl, remoteNow := byID[id]
					if remoteNow && rl.second && status == model.StatusQueued {
						sm["status"], sm["progress"], status = model.StatusRunning, json.Number("0.18"), model.StatusRunning
						sm["started_at"] = rl.started.UTC().Format(time.RFC3339)
					}
					switch {
					case remoteNow && status == model.StatusRunning:
						rs := c.step(rl, now)
						stats, _ := sm["stats"].(map[string]any)
						if stats == nil {
							stats = map[string]any{}
							sm["stats"] = stats
						}
						stats["remote"] = toJSON(rs)
						sm["model"] = c.snapshot()
					case c.pinned[id] && (status == model.StatusQueued || status == model.StatusRunning):
						sm["model"] = c.snapshot()
					case id == backID && status == model.StatusQueued && sm["note_info"] == nil:
						sm["note_info"] = toJSON(model.ErrorInfo{Code: "remote.back_to_local", Params: map[string]any{"cause": colab.CodeSessionReclaimed}})
					case id == splitID && status == model.StatusDone:
						tm, _ := sm["asr_timing"].(map[string]any)
						if tm == nil {
							tm = toJSON(model.ASRTiming{LoadMS: 5200, TranscribeMS: 61000, AudioMS: 3300000}).(map[string]any)
							sm["asr_timing"] = tm
						}
						pp, _ := tm["postprocess"].(map[string]any)
						if pp == nil {
							pp = map[string]any{}
							tm["postprocess"] = pp
						}
						pp["split"] = map[string]any{"parts": 4, "cuts_ms": []int{812400, 1650300, 2471800}}
					}
				}
			}
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return v
}

// backToLocalLecture is the lecture of colab_none's remote.back_to_local
// note: the first one (store order) whose asr stage is queued and not pinned.
func (c *colabMock) backToLocalLecture(s *store) string {
	if !c.backToLocal {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lectures {
		if j := stageIndex(&l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusQueued && !c.pinned[l.ID] {
			return l.ID
		}
	}
	return ""
}

// splitLecture is the lecture colab_active says was transcribed in speed
// mode: the first one (store order) whose asr stage is done.
func (c *colabMock) splitLecture(s *store) string {
	if !c.split {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lectures {
		if j := stageIndex(&l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusDone {
			return l.ID
		}
	}
	return ""
}

func (c *colabMock) snapshot() any {
	s := c.settings
	acc, _ := c.effective()
	return toJSON(api.StageModel{
		Label: "Google Colab · " + acc + " · " + s.Model + " · " + s.ComputeType + " · en", Model: s.Model,
		ASR: &model.ASRSnapshot{DeviceID: model.DeviceColab, Backend: "colab", Model: s.Model, Precision: s.ComputeType, Language: "en"},
	})
}

// ---------- SSE ----------

// sseFix rewrites the "resources" events of the stream.
type sseFix struct {
	http.ResponseWriter
	fl  http.Flusher
	fix func([]byte) []byte
}

func (f *sseFix) Write(p []byte) (int, error) {
	const head = "event: resources\ndata: "
	if bytes.HasPrefix(p, []byte(head)) {
		data := bytes.TrimSuffix(p[len(head):], []byte("\n\n"))
		out := append([]byte(head), bytes.TrimRight(f.fix(data), "\n")...)
		out = append(out, '\n', '\n')
		if _, err := f.ResponseWriter.Write(out); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return f.ResponseWriter.Write(p)
}

func (f *sseFix) Flush() { f.fl.Flush() }

func (m *mock) colabEvents(mux *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := m.colabState(r)
		fl, ok := w.(http.Flusher)
		if !ok {
			mux.ServeHTTP(w, innerRequest(r))
			return
		}
		fw := &sseFix{ResponseWriter: w, fl: fl, fix: func(b []byte) []byte {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !c.on() && !m.sshOn(r) {
				return b
			}
			return m.colabFix(r, c, "/api/resources", b)
		}}
		mux.ServeHTTP(fw, innerRequest(r))
	}
}
