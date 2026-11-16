// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
)

// KV is the part of db.Store the updater uses.
type KV interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
	SetKV(ctx context.Context, key, value string) error
}

// kv keys (the update channel is the existing app.update_channel).
const (
	KVAutoCheck = "selfupdate.auto_check" // "1" | "0" (default on)
	KVSources   = "selfupdate.sources"    // JSON array of the extra sources
	// JSON {"at": RFC 3339, "result": "up_to_date" | "available" | "error",
	// "failures": failed checks in a row, "error": the last failure}
	KVLastCheck = "selfupdate.last_check"
	// JSON failedRecord: the version whose installation did not work, until
	// a strictly newer one appears ("" = none)
	KVFailedInstall = "selfupdate.failed_install"
)

// When automatic checks run (README "Privacy"): a day after a check that
// worked; after a failed one sooner, backing off: 1 h, 3 h, 6 h, then a day
// again. A release without update.json or signature (no_manifest: the
// maintainer has not uploaded the signature yet) is asked again every 2 h
// for about a day, so the update arrives within hours of the upload, then
// every 6 h. A check made by hand counts as well.
const (
	checkDaily      = 24 * time.Hour
	noManifestRetry = 2 * time.Hour
	noManifestLong  = 6 * time.Hour
	noManifestTries = 12 // 2 h apart: about a day
)

var retryLadder = []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour}

// CheckInterval is the time from a check to the next automatic one after
// failures failed checks in a row (0: the last one worked), the last of
// them with code.
func CheckInterval(failures int, code string) time.Duration {
	switch {
	case failures <= 0:
		return checkDaily
	case code == CodeNoManifest && failures <= noManifestTries:
		return noManifestRetry
	case code == CodeNoManifest:
		return noManifestLong
	case failures <= len(retryLadder):
		return retryLadder[failures-1]
	}
	return checkDaily
}

// States of Status.State.
const (
	StateNotConfigured = "not_configured" // Status.Reason says why; nothing is checked
	StateIdle          = "idle"           // not checked since the start
	StateChecking      = "checking"
	StateUpToDate      = "up_to_date"
	StateAvailable     = "available" // Status.Available is newer than this version
	StateDownloading   = "downloading"
	StateImporting     = "importing"
	StatePending       = "pending" // Status.Pending is installed at the next start
	StateError         = "error"   // the last check or download failed (Status.Error)
	// Status.InstallFailed: installing that version did not work; it is not
	// offered until a newer one appears. Status.Available is that version
	// when a check found it: downloading it is trying it again.
	StateInstallFailed = "install_failed"
)

// Reasons of StateNotConfigured.
const (
	ReasonNoPublicKey = "no_public_key"
	ReasonWebMode     = "web_mode"
	ReasonDevBuild    = "dev_build"
	ReasonPlatform    = "unsupported_platform"
	ReasonNoSource    = "no_source"
)

const (
	opCheck    = "check"
	opDownload = "download"
	opImport   = "import"
)

// Options configure an Updater.
type Options struct {
	DataDir string // updates go to {DataDir}/updates
	Version string // the running version
	KV      KV
	// Desktop: the desktop app (Electron installs pending updates). In web
	// mode nothing could install them: not configured.
	Desktop  bool
	Portable bool   // the Windows portable build
	Platform string // "" = this computer's (windows-x64, darwin-arm64)
	// NoInstall: the desktop shell found that this copy of the app cannot
	// replace itself (an unpacked or copied folder; -no-self-update). No
	// package is this copy's: a newer version is found and shown without
	// one (the page says to install it by hand), a download or an offline
	// update is refused with selfupdate.no_package, and any pending update
	// in the data directory is another copy's (othercopy.go).
	NoInstall bool
	// InstallDir is the installation directory the desktop shell names
	// (-install-dir: the Windows installation set up by the installer;
	// "" = none, nothing is checked): while other accounts may change it,
	// nothing is downloaded or imported (CheckInstallDir, installdir.go).
	InstallDir string

	// Overrides for tests ("" = the built-in values).
	PublicKey string
	Repo      string
	GitHubWeb string // https://github.com
	GitHubAPI string // https://api.github.com

	Client     *http.Client // checks; nil = NewHTTPClient(60 s)
	Downloader Downloader   // nil = HTTPDownloader
	Now        func() time.Time
	Log        *slog.Logger

	// Automatic checks: the first one this long after Run starts (default
	// 2 min), then Run looks every CheckEvery (default 1 h) whether a day
	// has passed since the last check.
	FirstCheckDelay time.Duration
	CheckEvery      time.Duration
	RequestTimeout  time.Duration // per request of a check (default 30 s)
}

// Status is GET /api/selfupdate.
type Status struct {
	State          string           `json:"state"`
	Reason         string           `json:"reason,omitempty"`
	CurrentVersion string           `json:"current_version"`
	Channel        string           `json:"channel"`
	Platform       string           `json:"platform,omitempty"`
	Kind           string           `json:"kind,omitempty"`
	AutoCheck      bool             `json:"auto_check"`
	LastCheck      string           `json:"last_check,omitempty"`
	NextCheck      string           `json:"next_check,omitempty"` // the next automatic check (auto_check on, after a first check; not before now)
	Available      *Release         `json:"available,omitempty"`
	Progress       *Progress        `json:"progress,omitempty"`
	Pending        *PendingInfo     `json:"pending,omitempty"`
	Rollback       *RollbackInfo    `json:"rollback,omitempty"`
	LastResult     *Result          `json:"last_result,omitempty"`
	InstallFailed  *FailedInstall   `json:"install_failed,omitempty"`
	Error          *model.ErrorInfo `json:"error,omitempty"`
}

// Release is a newer version found by a check.
type Release struct {
	Version    string `json:"version"`
	Channel    string `json:"channel"`
	ReleasedAt string `json:"released_at"`
	NotesURL   string `json:"notes_url,omitempty"`
	MinVersion string `json:"min_version,omitempty"`
	// Direct is false when this version is older than MinVersion: no
	// download here, the user installs the new version by hand.
	Direct bool   `json:"direct"`
	File   string `json:"file,omitempty"` // this installation's package ("" = none in the release)
	Size   int64  `json:"size,omitempty"`
	Source string `json:"source,omitempty"` // "github", "github-api" or the extra source that answered
}

// Progress of a download.
type Progress struct {
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
}

// PendingInfo summarizes pending.json.
type PendingInfo struct {
	Version    string `json:"version"`
	File       string `json:"file"` // base name
	Size       int64  `json:"size"`
	Origin     string `json:"origin"` // download | file
	VerifiedAt string `json:"verified_at"`
}

// RollbackInfo summarizes previous.json.
type RollbackInfo struct {
	Version string `json:"version"`
	// Package: previous.json names a verified package of that version
	// (otherwise Electron backs up the installation when it installs).
	Package bool `json:"package"`
}

// Settings is GET /api/selfupdate/settings.
type Settings struct {
	AutoCheck bool     `json:"auto_check"`
	Sources   []string `json:"sources"`
}

// SettingsPatch is the body of PATCH /api/selfupdate/settings.
type SettingsPatch struct {
	AutoCheck *bool     `json:"auto_check"`
	Sources   *[]string `json:"sources"`
}

// Updater checks, downloads and verifies application updates.
type Updater struct {
	opt        Options
	log        *slog.Logger
	now        func() time.Time
	reason     string
	pk         Key
	cur        components.Version
	platform   string
	kind       string
	repo       string
	web, api   string
	client     *http.Client
	dl         Downloader
	dir        string
	reqTimeout time.Duration

	root     context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
	changeMu sync.Mutex
	onChange []func()

	mu        sync.Mutex
	op        string
	cancelDL  context.CancelCauseFunc
	dlDone    chan struct{} // closed when the running download has ended
	avail     *Verified
	checked   bool
	lastCheck time.Time
	failures  int    // failed checks in a row (the automatic checks back off)
	failCode  string // the code of the last failed check
	pending   *Pending
	other     *Pending // pending.json when it is another copy's (othercopy.go)
	previous  *Previous
	result    *Result
	failed    *failedRecord // kv selfupdate.failed_install (activeFailedLocked: in effect)
	progress  *Progress
	lastEmit  time.Time
	lastErr   *model.ErrorInfo
}

// New builds an Updater. It touches nothing on disk; call Startup, then
// run Run in a goroutine, and Close at shutdown.
func New(o Options) *Updater {
	u := &Updater{opt: o, log: o.Log, now: o.Now, client: o.Client, dl: o.Downloader,
		repo: o.Repo, web: o.GitHubWeb, api: o.GitHubAPI, reqTimeout: o.RequestTimeout}
	if u.log == nil {
		u.log = slog.Default()
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.client == nil {
		u.client = NewHTTPClient(60 * time.Second)
	}
	if u.dl == nil {
		u.dl = &HTTPDownloader{}
	}
	if u.repo == "" {
		u.repo = Repo
	}
	if u.web == "" {
		u.web = "https://github.com"
	}
	if u.api == "" {
		u.api = "https://api.github.com"
	}
	u.web = strings.TrimRight(u.web, "/")
	u.api = strings.TrimRight(u.api, "/")
	if u.reqTimeout <= 0 {
		u.reqTimeout = 30 * time.Second
	}
	if o.DataDir != "" {
		u.dir = filepath.Join(o.DataDir, "updates")
	}
	u.root, u.stop = context.WithCancel(context.Background())

	u.platform = o.Platform
	if u.platform == "" {
		u.platform = currentPlatform()
	}
	switch u.platform {
	case PlatformWindowsX64:
		u.kind = KindInstaller
		if o.Portable {
			u.kind = KindPortable
		}
	case PlatformDarwinArm64:
		u.kind = KindZip
	}

	key := o.PublicKey
	if key == "" {
		key = builtinPublicKey
	}
	var err error
	switch {
	case key == "":
		u.reason = ReasonNoPublicKey
	case !o.Desktop:
		u.reason = ReasonWebMode
	case u.kind == "":
		u.reason = ReasonPlatform
	case u.dir == "" || o.KV == nil:
		u.reason = ReasonNoSource
	}
	if u.reason == "" {
		if u.pk, err = ParsePublicKey(key); err != nil {
			u.log.Error("selfupdate: the built-in public key is unusable; updates are off", "error", err)
			u.reason = ReasonNoPublicKey
		}
	}
	if u.reason == "" {
		if u.cur, err = parseVersion(o.Version); err != nil {
			u.reason = ReasonDevBuild
		}
	}
	return u
}

func currentPlatform() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return PlatformWindowsX64
	case "darwin/arm64":
		return PlatformDarwinArm64
	}
	return ""
}

// asset is the package of m this copy downloads and installs: none for a
// copy that cannot update itself (Options.NoInstall).
func (u *Updater) asset(m *Manifest) (Asset, bool) {
	if u.opt.NoInstall {
		return Asset{}, false
	}
	return m.Asset(u.platform, u.kind)
}

// importKinds are the package kinds an offline update may bring (none for
// a copy that cannot update itself: matchPackage answers no_package).
func (u *Updater) importKinds() []string {
	if u.opt.NoInstall {
		return nil
	}
	if u.platform == PlatformDarwinArm64 {
		return []string{KindZip, KindDMG}
	}
	return []string{u.kind}
}

// Enabled reports whether updates are configured.
func (u *Updater) Enabled() bool { return u.reason == "" }

// Dir is the updates directory.
func (u *Updater) Dir() string { return u.dir }

// OnChange registers fn, called (outside any lock) whenever the status
// changes; download progress at most once a second.
func (u *Updater) OnChange(fn func()) {
	u.changeMu.Lock()
	u.onChange = append(u.onChange, fn)
	u.changeMu.Unlock()
}

func (u *Updater) changed() {
	u.changeMu.Lock()
	fns := append([]func(){}, u.onChange...)
	u.changeMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Close cancels a running download and waits for it.
func (u *Updater) Close() {
	u.stop()
	u.wg.Wait()
}

// ---- settings ----

func (u *Updater) kvGet(ctx context.Context, key string) (string, bool) {
	if u.opt.KV == nil {
		return "", false
	}
	v, ok, err := u.opt.KV.GetKV(ctx, key)
	if err != nil {
		u.log.Warn("selfupdate: reading a setting", "key", key, "error", err)
		return "", false
	}
	return v, ok
}

// Settings reads the settings (defaults for missing or unreadable values).
func (u *Updater) Settings(ctx context.Context) Settings {
	s := Settings{AutoCheck: true, Sources: []string{}}
	if v, ok := u.kvGet(ctx, KVAutoCheck); ok && v == "0" {
		s.AutoCheck = false
	}
	if v, ok := u.kvGet(ctx, KVSources); ok {
		var list []string
		if json.Unmarshal([]byte(v), &list) == nil {
			// an entry this version no longer accepts (e.g. plain http) is
			// left out, the others stay
			usable := list[:0]
			for _, raw := range list {
				if _, err := parseSource(raw, u.githubLatest()); err == nil {
					usable = append(usable, raw)
				}
			}
			if norm, err := normalizeSources(usable, u.githubLatest()); err == nil {
				s.Sources = norm
			}
		}
	}
	return s
}

// UpdateSettings validates and stores a patch (omitted fields unchanged).
func (u *Updater) UpdateSettings(ctx context.Context, p SettingsPatch) (Settings, error) {
	if u.opt.KV == nil {
		return Settings{}, cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoSource}, errors.New("no settings store"))
	}
	var srcJSON string
	if p.Sources != nil {
		norm, err := normalizeSources(*p.Sources, u.githubLatest())
		if err != nil {
			return u.Settings(ctx), err
		}
		b, _ := json.Marshal(norm)
		srcJSON = string(b)
	}
	if p.AutoCheck != nil {
		v := "0"
		if *p.AutoCheck {
			v = "1"
		}
		if err := u.opt.KV.SetKV(ctx, KVAutoCheck, v); err != nil {
			return u.Settings(ctx), err
		}
	}
	if p.Sources != nil {
		if err := u.opt.KV.SetKV(ctx, KVSources, srcJSON); err != nil {
			return u.Settings(ctx), err
		}
	}
	u.changed()
	return u.Settings(ctx), nil
}

// channel is the user's update channel (app.update_channel).
func (u *Updater) channel(ctx context.Context) string {
	if v, ok := u.kvGet(ctx, model.KVAppUpdateChannel); ok && v == ChannelBeta {
		return ChannelBeta
	}
	return ChannelStable
}

type lastCheckRecord struct {
	At       string           `json:"at"`
	Result   string           `json:"result"`
	Failures int              `json:"failures,omitempty"`
	Error    *model.ErrorInfo `json:"error,omitempty"`
}

// recordCheck keeps the outcome of a check (err nil: it worked) for the
// next start: when it was, the failures in a row and the last failure,
// which the status shows again after a restart.
func (u *Updater) recordCheck(ctx context.Context, at time.Time, result string, err error) {
	rec := lastCheckRecord{At: at.UTC().Format(time.RFC3339), Result: result}
	u.mu.Lock()
	if err == nil {
		u.failures, u.failCode = 0, ""
	} else {
		info := InfoOf(err)
		u.failures++
		u.failCode = info.Code
		rec.Failures, rec.Error = u.failures, &info
	}
	u.mu.Unlock()
	b, _ := json.Marshal(rec)
	if err := u.opt.KV.SetKV(ctx, KVLastCheck, string(b)); err != nil {
		u.log.Warn("selfupdate: saving the check time", "error", err)
	}
}

// lastCheckLocked is the time of the last check, never after now: a check
// recorded "in the future" (the clock was set back since) counts as made
// now, so automatic checks are not held up until that time (u.mu held).
func (u *Updater) lastCheckLocked() time.Time {
	if now := u.now(); u.lastCheck.After(now) {
		u.lastCheck = now
	}
	return u.lastCheck
}

// nextCheckLocked is when the next automatic check is due (zero: no check
// made yet; u.mu held).
func (u *Updater) nextCheckLocked() time.Time {
	last := u.lastCheckLocked()
	if last.IsZero() {
		return time.Time{}
	}
	return last.Add(CheckInterval(u.failures, u.failCode))
}

// ---- status ----

// offered reports whether v may be offered on channel and is newer than
// this version and than the pending update.
func (u *Updater) offeredLocked(v *Verified, channel string) bool {
	if v == nil {
		return false
	}
	m := v.Manifest
	if m.Channel == ChannelBeta && channel != ChannelBeta {
		return false
	}
	if compareVersions(m.Version, u.opt.Version) <= 0 {
		return false
	}
	return u.pending == nil || compareVersions(m.Version, u.pending.Version) > 0
}

func (u *Updater) direct(m *Manifest) bool {
	return m.MinVersion == "" || compareVersions(u.opt.Version, m.MinVersion) >= 0
}

// Status returns the current status.
func (u *Updater) Status(ctx context.Context) Status {
	set := u.Settings(ctx)
	channel := u.channel(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	st := Status{CurrentVersion: u.opt.Version, Channel: channel, Platform: u.platform, Kind: u.kind, AutoCheck: set.AutoCheck}
	if u.reason != "" {
		st.State, st.Reason = StateNotConfigured, u.reason
		return st
	}
	if last := u.lastCheckLocked(); !last.IsZero() {
		st.LastCheck = last.UTC().Format(time.RFC3339)
		if set.AutoCheck {
			next := u.nextCheckLocked()
			if now := u.now(); next.Before(now) {
				next = now
			}
			st.NextCheck = next.UTC().Format(time.RFC3339)
		}
	}
	if u.offeredLocked(u.avail, channel) {
		m := u.avail.Manifest
		r := &Release{Version: m.Version, Channel: m.Channel, ReleasedAt: m.ReleasedAt, NotesURL: m.NotesURL,
			MinVersion: m.MinVersion, Direct: u.direct(m), Source: u.avail.Source}
		if a, ok := u.asset(m); ok {
			r.File, r.Size = a.Name, a.Size
		}
		st.Available = r
	}
	if p := u.pending; p != nil {
		st.Pending = &PendingInfo{Version: p.Version, File: filepath.Base(filepath.FromSlash(p.File)), Size: p.Size,
			Origin: p.Origin, VerifiedAt: p.VerifiedAt}
	}
	if p := u.previous; p != nil {
		st.Rollback = &RollbackInfo{Version: p.Version, Package: p.File != "" || p.Backup != ""}
	}
	if u.result != nil {
		r := *u.result
		st.LastResult = &r
	}
	if f := u.activeFailedLocked(channel); f != nil {
		fi := f.FailedInstall
		st.InstallFailed = &fi
	}
	if u.progress != nil {
		pr := *u.progress
		st.Progress = &pr
	}
	if u.lastErr != nil {
		e := *u.lastErr
		st.Error = &e
	}
	// An update that can be installed comes first: a failed check (the
	// network was not up yet, the latest release has no signature yet)
	// leaves the version found before, or the one downloaded, as it is,
	// with the failure as a note (Status.Error). A failed download of the
	// available version is the state error (download again). A version
	// whose installation did not work is not "available" again: the state
	// install_failed (with a failed check as a note) until a newer version
	// appears or the user tries it again.
	switch {
	case u.op == opCheck:
		st.State = StateChecking
	case u.op == opDownload:
		st.State = StateDownloading
	case u.op == opImport:
		st.State = StateImporting
	case u.lastErr != nil && IsDownloadError(u.lastErr.Code) && st.Available != nil:
		st.State = StateError
	case st.InstallFailed != nil && st.Pending == nil && (st.Available == nil || st.Available.Version == st.InstallFailed.Version):
		st.State = StateInstallFailed
	case u.lastErr != nil && st.Available == nil && st.Pending == nil:
		st.State = StateError
	case st.Available != nil:
		st.State = StateAvailable
	case st.Pending != nil:
		st.State = StatePending
	case u.checked:
		st.State = StateUpToDate
	default:
		st.State = StateIdle
	}
	return st
}

// begin claims the single operation slot.
func (u *Updater) beginLocked(op string) error {
	if u.reason != "" {
		return cerr(CodeNotConfigured, map[string]any{"reason": u.reason}, nil)
	}
	if u.op != "" {
		return cerr(CodeBusy, map[string]any{"op": u.op}, nil)
	}
	if u.root.Err() != nil {
		return cerr(CodeBusy, map[string]any{"op": "shutdown"}, nil)
	}
	u.op = op
	return nil
}

func (u *Updater) begin(op string) error {
	u.mu.Lock()
	err := u.beginLocked(op)
	u.mu.Unlock()
	if err == nil {
		u.changed()
	}
	return err
}

// end releases the slot; err (nil = success, cancellation = no error) is
// what the status shows.
func (u *Updater) end(op string, err error) {
	u.mu.Lock()
	if u.op == op {
		u.op = ""
	}
	u.progress = nil
	if op == opDownload {
		u.cancelDL, u.dlDone = nil, nil
	}
	if err == nil || CodeOf(err) == CodeCancelled || errors.Is(err, context.Canceled) {
		u.lastErr = nil
	} else {
		info := InfoOf(err)
		u.lastErr = &info
	}
	u.mu.Unlock()
	u.changed()
}

// ---- checking ----

// Check asks the sources for update.json now. The outcome is in the
// returned status (an unreachable source or a bad signature is
// Status.Error); the error is only for a check that could not start
// (selfupdate.not_configured, selfupdate.busy).
func (u *Updater) Check(ctx context.Context) (Status, error) {
	if err := u.begin(opCheck); err != nil {
		return u.Status(ctx), err
	}
	err := u.check(ctx)
	u.end(opCheck, err)
	return u.Status(ctx), nil
}

func (u *Updater) check(ctx context.Context) error {
	set := u.Settings(ctx)
	channel := u.channel(ctx)
	now := u.now()
	u.mu.Lock()
	u.lastCheck = now
	u.mu.Unlock()
	srcs := u.sources(set.Sources, channel)
	if len(srcs) == 0 {
		err := cerr(CodeNotConfigured, map[string]any{"reason": ReasonNoSource}, errors.New("no repository and no extra source"))
		u.recordCheck(ctx, now, StateError, err)
		return err
	}
	v, err := u.checkSources(ctx, srcs, channel)
	if err != nil {
		if context.Cause(ctx) != nil {
			return err // stopped (shutdown, the request went away): not a failed check
		}
		u.recordCheck(ctx, now, StateError, err)
		return err
	}
	m := v.Manifest
	newer := compareVersions(m.Version, u.opt.Version) > 0 && (m.Channel == ChannelStable || channel == ChannelBeta)
	u.mu.Lock()
	u.checked = true
	if newer {
		u.avail = v
	} else {
		u.avail = nil
	}
	forget := u.failed
	if forget != nil && !(newer && compareVersions(m.Version, forget.Version) > 0) {
		forget = nil
	}
	if forget != nil {
		u.failed = nil // a strictly newer version appeared
	}
	u.mu.Unlock()
	if forget != nil {
		u.saveFailed(ctx, nil)
		u.log.Info("selfupdate: a version newer than the one that failed to install is available", "failed", forget.Version, "version", m.Version)
	}
	if newer {
		u.recordCheck(ctx, now, StateAvailable, nil)
		err := u.writeUpdatesFile(latestFile, v.Raw)
		if err == nil {
			err = u.writeUpdatesFile(latestSigFile, v.Signature)
		}
		if err != nil {
			u.log.Warn("selfupdate: keeping the check's result for the next start", "error", err)
		}
		u.log.Info("selfupdate: a newer version is available", "version", m.Version, "channel", m.Channel, "source", redactURL(v.Source))
	} else {
		u.recordCheck(ctx, now, StateUpToDate, nil)
		u.removeLatest()
	}
	return nil
}

// Run checks once a day while automatic checks are on (sooner after a
// failed check, CheckInterval), until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	if u.reason != "" {
		return
	}
	delay := u.opt.FirstCheckDelay
	if delay <= 0 {
		delay = 2 * time.Minute
	}
	every := u.opt.CheckEvery
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.root.Done():
			return
		case <-t.C:
		}
		if u.due(ctx) {
			if _, err := u.Check(ctx); err != nil && CodeOf(err) != CodeBusy {
				u.log.Warn("selfupdate: automatic check", "error", err)
			}
		}
		t.Reset(u.untilDue(every))
	}
}

// due: automatic checks are on, nothing runs, and the interval since the
// last check (also one made by hand) has passed: a day, sooner after a
// failure (CheckInterval).
func (u *Updater) due(ctx context.Context) bool {
	if !u.Settings(ctx).AutoCheck {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	next := u.nextCheckLocked()
	return u.op == "" && (next.IsZero() || !u.now().Before(next))
}

// untilDue is how long Run sleeps: every, or less when the next check is
// due earlier (a retry an hour after a failure is not put off to the tick
// after it).
func (u *Updater) untilDue(every time.Duration) time.Duration {
	u.mu.Lock()
	defer u.mu.Unlock()
	next := u.nextCheckLocked()
	if next.IsZero() {
		return every
	}
	if d := next.Sub(u.now()); d > 0 && d+time.Second < every {
		return d + time.Second
	}
	return every
}

// ---- downloading ----

// Download starts downloading the available version's package (202: the
// status is downloading). Errors: not_configured, busy, not_available,
// too_old, no_package, untrusted_dir, untrusted_install_dir,
// other_copy_pending.
func (u *Updater) Download(ctx context.Context) (Status, error) {
	channel := u.channel(ctx)
	if err := u.untrusted(); err != nil {
		return u.Status(ctx), err
	}
	if err := u.otherCopyPending(); err != nil {
		return u.Status(ctx), err
	}
	u.mu.Lock()
	err := u.beginLocked(opDownload)
	if err != nil {
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	v := u.avail
	var a Asset
	switch {
	case !u.offeredLocked(v, channel):
		err = cerr(CodeNotAvailable, nil, nil)
	case !u.direct(v.Manifest):
		err = cerr(CodeTooOld, map[string]any{"min_version": v.Manifest.MinVersion, "current": u.opt.Version}, nil)
	default:
		var ok bool
		if a, ok = u.asset(v.Manifest); !ok {
			err = cerr(CodeNoPackage, map[string]any{"platform": u.platform, "kind": u.kind}, nil)
		}
	}
	if err != nil {
		u.op = ""
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	// Downloading the version whose installation failed is trying it again
	// (the user's "try again"): the failure no longer applies; a new one is
	// recorded again from result.json.
	var retry *failedRecord
	if f := u.failed; f != nil && !f.Retry && f.Version == v.Manifest.Version {
		f.Retry = true
		r := *f
		retry = &r
	}
	dctx, cancel := context.WithCancelCause(u.root)
	done := make(chan struct{})
	u.cancelDL, u.dlDone = cancel, done
	u.progress = &Progress{BytesTotal: a.Size}
	u.lastErr = nil
	u.wg.Add(1)
	u.mu.Unlock()
	if retry != nil {
		u.saveFailed(ctx, retry)
		u.log.Info("selfupdate: trying again the version whose installation failed", "version", retry.Version)
	}
	u.changed()
	go func() {
		defer u.wg.Done()
		defer close(done)
		defer cancel(nil)
		err := u.download(dctx, v, a)
		if err != nil && CodeOf(err) != CodeCancelled {
			u.log.Warn("selfupdate: download failed", "version", v.Manifest.Version, "error", err)
		}
		u.end(opDownload, err)
	}()
	return u.Status(ctx), nil
}

// CancelDownload stops a running download (the partial file stays for the
// next attempt) and waits for it to end, at most 10 seconds. It reports
// whether one was running.
func (u *Updater) CancelDownload() bool {
	u.mu.Lock()
	c, done := u.cancelDL, u.dlDone
	u.mu.Unlock()
	if c == nil {
		return false
	}
	c(errCancelledByUser)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		u.log.Warn("selfupdate: the download did not stop within 10 s")
	}
	return true
}

var errCancelledByUser = errors.New("download cancelled")

// checkTrust is CheckTrust, checkInstall CheckInstallDir (tests replace
// them).
var (
	checkTrust   = CheckTrust
	checkInstall = CheckInstallDir
)

// untrusted returns selfupdate.untrusted_dir when other accounts may
// change the data directory, selfupdate.untrusted_install_dir when they
// may change the installation directory (Options.InstallDir): nothing is
// downloaded or imported (the desktop shell would not install it either).
// A check that fails for another reason does not stop anything here; the
// shell checks again (-selfupdate-verify) and refuses then.
func (u *Updater) untrusted() error {
	if u.reason != "" || u.dir == "" {
		return nil
	}
	err := checkTrust(u.dir)
	if CodeOf(err) == CodeUntrustedDir {
		u.log.Warn("selfupdate: the data directory can be changed by other accounts; no update is downloaded or imported", "error", err)
		return err
	}
	if err != nil {
		u.log.Warn("selfupdate: checking the data directory's permissions", "error", err)
	}
	if u.opt.InstallDir == "" {
		return nil
	}
	// Any failure of this check refuses too (security review SEC20): the
	// installation folder cannot be vouched for, and the shell's check
	// would refuse the package after the download anyway; the diagnostics
	// show the same folder as "cannot be confirmed".
	err = checkInstall(u.opt.InstallDir)
	if err != nil && CodeOf(err) != CodeUntrustedInstallDir {
		err = cerr(CodeUntrustedInstallDir, map[string]any{"path": u.opt.InstallDir, "who": dirperm.NotConfirmed + InfoOf(err).Detail}, err)
	}
	if err != nil {
		u.log.Warn("selfupdate: the installation directory cannot be vouched for; no update is downloaded or imported (install the new version by hand)", "error", err)
		return err
	}
	return nil
}

func (u *Updater) download(ctx context.Context, v *Verified, a Asset) error {
	m := v.Manifest
	dir := filepath.Join(u.dir, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, manifestFile), v.Raw); err != nil {
		return ioErr(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, signatureFile), v.Signature); err != nil {
		return ioErr(err)
	}
	final := filepath.Join(dir, a.Name)
	if !fileMatches(final, a) {
		os.Remove(final)
		part := final + ".part"
		err := u.dl.Download(ctx, DownloadRequest{URLs: u.assetURLs(ctx, v, a), UserURLs: u.userAssetURLs(ctx, v, a), Path: part,
			Size: a.Size, SHA256: a.SHA256, Progress: u.setProgress})
		if c := context.Cause(ctx); c != nil {
			return cerr(CodeCancelled, nil, c)
		}
		if err != nil {
			var le *localError
			if errors.As(err, &le) || CodeOf(err) == CodeIO {
				return ioErr(err)
			}
			return cerr(CodeDownloadFailed, nil, err)
		}
		sum, n, err := hashFile(part)
		if err != nil {
			return ioErr(err)
		}
		if n != a.Size || sum != a.SHA256 {
			os.Remove(part)
			return cerr(CodeChecksum, map[string]any{"file": a.Name},
				fmt.Errorf("%s: %d bytes, SHA-256 %s; update.json says %d bytes, %s", a.Name, n, sum, a.Size, a.SHA256))
		}
		if err := renameRetry(part, final); err != nil {
			return ioErr(err)
		}
	}
	return u.makePending(m, a, OriginDownload)
}

func (u *Updater) setProgress(done int64) {
	now := u.now()
	u.mu.Lock()
	emit := false
	if u.progress != nil {
		u.progress.BytesDone = done
		if now.Sub(u.lastEmit) >= time.Second {
			u.lastEmit = now
			emit = true
		}
	}
	u.mu.Unlock()
	if emit {
		u.changed()
	}
}

// assetURLs are the URLs of a package, best first.
func (u *Updater) assetURLs(ctx context.Context, v *Verified, a Asset) []string {
	var out []string
	add := func(s string) {
		if s == "" || !secureURL(s) { // https, or http to this computer
			return
		}
		for _, x := range out {
			if x == s {
				return
			}
		}
		out = append(out, s)
	}
	if v.source != nil {
		add(v.source.assetURL(a))
	}
	add(a.URL)
	for _, s := range u.sources(u.Settings(ctx).Sources, ChannelStable) {
		if s.kind == srcTemplate || s.kind == srcDir {
			add(s.assetURL(a))
		}
	}
	for _, mu := range v.Manifest.mirrorURLs(a.URL) {
		add(mu)
	}
	return out
}

// userAssetURLs are those of assetURLs that come from the user's own
// sources (a {url} template or a directory of the settings, the source
// that delivered update.json included): DownloadRequest.UserURLs.
func (u *Updater) userAssetURLs(ctx context.Context, v *Verified, a Asset) []string {
	var out []string
	add := func(s *source) {
		if s == nil || (s.kind != srcTemplate && s.kind != srcDir) {
			return
		}
		if x := s.assetURL(a); x != "" && secureURL(x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	add(v.source)
	for _, s := range u.sources(u.Settings(ctx).Sources, ChannelStable) {
		add(s)
	}
	return out
}

// ---- pending and previous ----

// makePending records a verified package as the update to install at the
// next start: previous.json first, then pending.json.
func (u *Updater) makePending(m *Manifest, a Asset, origin string) error {
	// never over another copy's update (both files have one slot; Download
	// and ImportWith refuse before they begin, this is the last look)
	if err := u.otherCopyPending(); err != nil {
		return err
	}
	now := u.now().UTC().Format(time.RFC3339)
	prev := u.previousFor(now)
	if err := u.writeUpdatesJSON(previousFile, prev); err != nil {
		return ioErr(err)
	}
	p := Pending{Schema: fileSchema, Version: m.Version, FromVersion: u.opt.Version, Channel: m.Channel,
		Platform: u.platform, Kind: a.Kind, File: relPath(m.Version, a.Name), Size: a.Size, SHA256: a.SHA256,
		Manifest: relPath(m.Version, manifestFile), Signature: relPath(m.Version, signatureFile),
		KeyID: hex.EncodeToString(keyIDDisplay(u.pk.KeyID)), Origin: origin, VerifiedAt: now}
	p.KeyID = strings.ToUpper(p.KeyID)
	if err := u.writeUpdatesJSON(pendingFile, p); err != nil {
		return ioErr(err)
	}
	u.mu.Lock()
	u.pending = &p
	u.other = nil // (one given up for its age: the slot is this copy's now)
	u.previous = &prev
	// the version whose installation failed: a newer one supersedes it; the
	// same one (an offline update of it) is trying it again
	var failedChange bool
	var failedNow *failedRecord
	if f := u.failed; f != nil {
		switch {
		case compareVersions(p.Version, f.Version) > 0:
			u.failed, failedChange = nil, true
		case p.Version == f.Version && !f.Retry:
			f.Retry, failedChange = true, true
			r := *f
			failedNow = &r
		}
	}
	u.mu.Unlock()
	if failedChange {
		u.saveFailed(context.Background(), failedNow)
	}
	u.log.Info("selfupdate: update ready; it is installed at the next start", "version", p.Version, "origin", origin)
	u.cleanup(false)
	return nil
}

// previousFor describes the running version for previous.json: its
// verified package when updates/<version>/ has one (it came through the
// updater), else none (Electron backs up the installation). A backup
// Electron recorded for the same version is kept.
func (u *Updater) previousFor(now string) Previous {
	prev := Previous{Schema: fileSchema, Version: u.opt.Version, Platform: u.platform, Kind: u.kind, CreatedAt: now}
	var old Previous
	if ok, err := readJSON(filepath.Join(u.dir, previousFile), &old); ok && err == nil && old.Version == prev.Version {
		if _, ok := absPath(u.dir, old.Backup); ok {
			prev.Backup = old.Backup
		}
	}
	dir := filepath.Join(u.dir, u.opt.Version)
	raw, err1 := os.ReadFile(filepath.Join(dir, manifestFile))
	sig, err2 := os.ReadFile(filepath.Join(dir, signatureFile))
	if err1 != nil || err2 != nil {
		return prev
	}
	m, err := verifyManifest(u.pk, raw, sig)
	if err != nil || m.Version != u.opt.Version {
		return prev
	}
	for _, k := range append([]string{u.kind}, u.importKinds()...) {
		a, ok := m.Asset(u.platform, k)
		if ok && fileMatches(filepath.Join(dir, a.Name), a) {
			prev.Kind, prev.File, prev.Size, prev.SHA256 = a.Kind, relPath(u.opt.Version, a.Name), a.Size, a.SHA256
			break
		}
	}
	return prev
}

// DiscardPending removes the pending update: pending.json, then the
// downloaded or imported files of that version (updates/<version>/; the
// running version's files and Electron's backups stay). Downloading it
// again starts from the beginning.
func (u *Updater) DiscardPending(ctx context.Context) (Status, error) {
	u.mu.Lock()
	if err := u.beginLocked(opImport); err != nil {
		u.mu.Unlock()
		return u.Status(ctx), err
	}
	p := u.pending
	u.mu.Unlock()
	var err error
	if p != nil {
		if rerr := u.removeUpdatesFile(pendingFile); rerr != nil {
			err = ioErr(rerr)
		} else {
			u.mu.Lock()
			u.pending = nil
			u.mu.Unlock()
			u.removeVersion(p.Version)
			u.cleanup(false)
			u.log.Info("selfupdate: the pending update was discarded", "version", p.Version)
		}
	}
	u.mu.Lock()
	u.op = ""
	u.mu.Unlock()
	u.changed()
	return u.Status(ctx), err
}

// removeVersion deletes updates/<version>/ unless it belongs to the
// running version or to previous.json (what a failed update returns to).
func (u *Updater) removeVersion(version string) {
	if _, err := parseVersion(version); err != nil || version != filepath.Base(version) || version == u.opt.Version {
		return
	}
	u.mu.Lock()
	keep := u.previous != nil && u.previous.Version == version
	u.mu.Unlock()
	if keep {
		return
	}
	r, err := u.updatesRoot()
	if err != nil {
		if !isNotExist(err) {
			u.log.Warn("selfupdate: removing the discarded update's files", "version", version, "error", err)
		}
		return
	}
	defer r.Close()
	if err := r.RemoveAll(version); err != nil {
		u.log.Warn("selfupdate: removing the discarded update's files", "version", version, "error", err)
	}
}

// updatesRoot opens the updates directory for removing entries of it, when
// it is a plain directory (security review SEC17: one that is a link —
// left by whoever could once write to the data directory — is not
// followed; what lies where it leads is not this program's to remove).
// What is removed is then named relative to that handle, and os.Root's
// RemoveAll removes a link below it without entering it.
func (u *Updater) updatesRoot() (*os.Root, error) {
	r, err := openPlainDir(u.dir)
	if errors.Is(err, errNotPlainDir) {
		u.log.Warn("selfupdate: the updates directory is a link; nothing is written or removed through it", "dir", u.dir)
	}
	return r, err
}

// writeUpdatesFile writes the file name of the updates directory (a
// temporary file renamed over it) through updatesRoot: when the updates
// directory is a link nothing is written (security review SEC17, 低-F: the
// link could lead to any folder the user can write) and the error says
// so. A missing updates directory is made first.
func (u *Updater) writeUpdatesFile(name string, data []byte) error {
	r, err := u.updatesRoot()
	if isNotExist(err) {
		if err = os.MkdirAll(u.dir, 0o755); err == nil {
			r, err = u.updatesRoot()
		}
	}
	if err != nil {
		return err
	}
	defer r.Close()
	return writeFileAtomicIn(r, name, data)
}

// removeLatest removes the last check's verified result (latest.json and
// its signature) through removeUpdatesFile.
func (u *Updater) removeLatest() {
	for _, name := range []string{latestFile, latestSigFile} {
		if err := u.removeUpdatesFile(name); err != nil {
			u.log.Warn("selfupdate: removing the last check's result", "file", name, "error", err)
		}
	}
}

// writeUpdatesJSON is writeUpdatesFile of v as indented JSON.
func (u *Updater) writeUpdatesJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return u.writeUpdatesFile(name, append(b, '\n'))
}

// removeUpdatesFile removes the entry name of the updates directory (a
// link there is removed itself) through updatesRoot: nothing when the
// updates directory is a link (an error) or missing (nil).
func (u *Updater) removeUpdatesFile(name string) error {
	r, err := u.updatesRoot()
	if isNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(name); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

// ---- startup and cleanup ----

// Startup reads what earlier runs left: the pending update (dropped once
// this version is not older than it: it was installed), previous.json,
// result.json, the last verified check result; then removes stale files.
func (u *Updater) Startup(ctx context.Context) {
	if u.reason != "" {
		return
	}
	if v, ok := u.kvGet(ctx, KVLastCheck); ok {
		var rec lastCheckRecord
		if json.Unmarshal([]byte(v), &rec) == nil {
			if t, err := time.Parse(time.RFC3339, rec.At); err == nil {
				u.lastCheck = t
				u.checked = rec.Result == StateUpToDate
				if rec.Result == StateError && rec.Failures > 0 && rec.Error != nil && rec.Error.Code != "" {
					// the last check failed: shown again, and the
					// automatic checks keep backing off
					e := *rec.Error
					u.lastErr, u.failures, u.failCode = &e, rec.Failures, e.Code
				}
			}
		}
	}
	u.loadPending()
	var prev Previous
	if ok, err := readJSON(filepath.Join(u.dir, previousFile), &prev); ok && err == nil && prev.Version != "" {
		u.previous = &prev
	}
	var res Result
	if ok, err := readJSON(filepath.Join(u.dir, resultFile), &res); ok && err == nil && res.Version != "" {
		u.result = &res
	}
	raw, err1 := os.ReadFile(filepath.Join(u.dir, latestFile))
	sig, err2 := os.ReadFile(filepath.Join(u.dir, latestSigFile))
	if err1 == nil && err2 == nil {
		if m, err := verifyManifest(u.pk, raw, sig); err == nil && compareVersions(m.Version, u.opt.Version) > 0 {
			u.avail = &Verified{Manifest: m, Raw: raw, Signature: sig}
		} else {
			u.removeLatest()
		}
	}
	u.loadFailed(ctx)
	// (removes the package of a version whose installation failed)
	u.cleanup(true)
}

// ---- a failed installation ----

// loadFailed (Startup) records a newer version whose installation did not
// work: result.json rolled_back or failed for a version newer than this
// one, with no pending update of it (Electron keeps pending.json when it
// only could not check the package this time: that is no failure). A
// result already recorded (same version and time) is not recorded again,
// so that the user's "try again" stays. The record is dropped once this
// version is not older than it, or a strictly newer version is offered or
// pending.
func (u *Updater) loadFailed(ctx context.Context) {
	channel := u.channel(ctx)
	rec := u.readFailed(ctx)
	if r := u.result; r != nil && (r.Outcome == OutcomeRolledBack || r.Outcome == OutcomeFailed) && validVersion(r.Version) &&
		compareVersions(r.Version, u.opt.Version) > 0 && (u.pending == nil || u.pending.Version != r.Version) {
		if rec == nil || rec.Version != r.Version || rec.At != r.At {
			// only an installation this version started of a version Go
			// verified (SEC12 L-3): a planted or stale result.json cannot
			// hold back an update
			if why := u.unprovenFailure(r); why != "" {
				u.log.Info("selfupdate: result.json is not about an update of this version; not remembered as failed",
					"version", r.Version, "reason", why)
			} else {
				rec = &failedRecord{FailedInstall: FailedInstall{Version: r.Version, At: r.At, Outcome: r.Outcome, Error: r.Error}}
				u.saveFailed(ctx, rec)
				u.log.Warn("selfupdate: installing the update did not work; it is not offered again until a newer version appears",
					"version", r.Version, "outcome", r.Outcome)
			}
		}
	}
	if rec == nil {
		return
	}
	u.mu.Lock()
	u.failed = rec
	superseded := u.supersededLocked(rec, channel)
	retried := !superseded && !rec.Retry && u.pending != nil && u.pending.Version == rec.Version
	if superseded {
		u.failed = nil
	}
	if retried {
		rec.Retry = true
	}
	u.mu.Unlock()
	switch {
	case superseded:
		u.saveFailed(ctx, nil)
		u.log.Info("selfupdate: forgetting the failed installation: a newer version is here", "version", rec.Version)
	case retried:
		u.saveFailed(ctx, rec)
	}
}

// unprovenFailure says why result.json r is not a failed update of this
// version ("" when it is): its "from" must be the running version (the
// shell writes the version it went back to, or left running), and
// updates/<version>/ must hold that version's update.json with a signature
// that verifies with the built-in key (Go wrote both when it downloaded or
// imported it; the start's clean-up removes them only afterwards). The
// path comes from r.Version only after it parsed as a version, and the
// files are only read. Nothing is proven in a data directory other
// accounts may change (selfupdate.untrusted_dir): result.json and
// update.json may be theirs (SEC12 review residual). Only the data
// directory counts here, not the installation folder (untrusted): a
// rollback can leave that one open, and the failure it rolled back is
// then the very one to remember.
func (u *Updater) unprovenFailure(r *Result) string {
	if r.From != u.opt.Version {
		return "it is about an update from " + r.From
	}
	if u.dir != "" && CodeOf(checkTrust(u.dir)) == CodeUntrustedDir {
		return "the data directory can be changed by other accounts"
	}
	if !validVersion(r.Version) || r.Version != filepath.Base(r.Version) {
		return "not a version"
	}
	mf, ok1 := absPath(u.dir, relPath(r.Version, manifestFile))
	sf, ok2 := absPath(u.dir, relPath(r.Version, signatureFile))
	if !ok1 || !ok2 {
		return "not a version"
	}
	raw, err1 := os.ReadFile(mf)
	sig, err2 := os.ReadFile(sf)
	if err1 != nil || err2 != nil {
		return "no update.json of that version here"
	}
	m, err := verifyManifest(u.pk, raw, sig)
	if err != nil || m.Version != r.Version {
		return "its update.json does not verify"
	}
	return ""
}

func validVersion(s string) bool {
	_, err := parseVersion(s)
	return err == nil
}

// readFailed is kv selfupdate.failed_install (nil: none or unreadable).
func (u *Updater) readFailed(ctx context.Context) *failedRecord {
	v, ok := u.kvGet(ctx, KVFailedInstall)
	if !ok || v == "" {
		return nil
	}
	var r failedRecord
	if json.Unmarshal([]byte(v), &r) != nil || !validVersion(r.Version) {
		return nil
	}
	return &r
}

// saveFailed stores r (nil: forgets the record). A failure is only logged:
// the record is a convenience, never a reason for anything else to fail.
func (u *Updater) saveFailed(ctx context.Context, r *failedRecord) {
	if u.opt.KV == nil {
		return
	}
	v := ""
	if r != nil {
		b, _ := json.Marshal(r)
		v = string(b)
	}
	if err := u.opt.KV.SetKV(ctx, KVFailedInstall, v); err != nil {
		u.log.Warn("selfupdate: saving the failed installation", "error", err)
	}
}

// supersededLocked: the failed version r no longer matters: this version
// is not older than it, or a strictly newer version is offered on channel
// or pending (u.mu held).
func (u *Updater) supersededLocked(r *failedRecord, channel string) bool {
	if compareVersions(r.Version, u.opt.Version) <= 0 {
		return true
	}
	if v := u.avail; v != nil && u.offeredLocked(v, channel) && compareVersions(v.Manifest.Version, r.Version) > 0 {
		return true
	}
	return u.pending != nil && compareVersions(u.pending.Version, r.Version) > 0
}

// activeFailedLocked is the failed installation in effect, or nil: none,
// the user is trying it again (Retry, or it is pending), or superseded
// (u.mu held).
func (u *Updater) activeFailedLocked(channel string) *failedRecord {
	r := u.failed
	if r == nil || r.Retry || (u.pending != nil && u.pending.Version == r.Version) || u.supersededLocked(r, channel) {
		return nil
	}
	return r
}

func (u *Updater) loadPending() {
	path := filepath.Join(u.dir, pendingFile)
	var p Pending
	ok, err := readJSON(path, &p)
	if !ok {
		return
	}
	drop := func(why string) {
		u.log.Info("selfupdate: removing pending.json", "reason", why, "version", p.Version)
		if err := u.removeUpdatesFile(pendingFile); err != nil {
			u.log.Warn("selfupdate: removing pending.json", "error", err)
		}
	}
	if err != nil || p.Schema != fileSchema {
		drop("unreadable")
		return
	}
	if _, err := parseVersion(p.Version); err != nil {
		drop("invalid")
		return
	}
	// Another copy's update (othercopy.go) is that copy's whatever its
	// version: the other copy may be older than this one.
	other := u.otherCopys(&p)
	if !other && p.Platform != u.platform {
		drop("invalid")
		return
	}
	if !other && compareVersions(p.Version, u.opt.Version) <= 0 {
		drop("installed or older than this version")
		return
	}
	f, ok := absPath(u.dir, p.File)
	if fi, err := os.Stat(f); !ok || err != nil || fi.Size() != p.Size {
		drop("the package is missing")
		return
	}
	if other {
		u.other = &p
		u.log.Info("selfupdate: pending.json is another copy's (a copy of LectureFlow of another kind shares this data directory); left alone, and this copy starts no update until that one is installed",
			"version", p.Version, "kind", p.Kind, "this", u.kind)
		return
	}
	u.pending = &p
}

// cleanup removes version directories nothing refers to (not this version,
// the pending one, another copy's pending one, previous.json's, nor the
// available one being
// downloaded, unless its installation failed and the user has not asked to
// try it again: its package goes); atStart also removes offline-update
// staging directories. Only entries of the updates directory whose names
// are versions (or staging names) are removed, never a path built from a
// JSON file; other names (Electron's backup/) are never touched. A
// directory that cannot be removed is logged and left for the next start.
// Nothing is removed when the updates directory is a link (updatesRoot).
func (u *Updater) cleanup(atStart bool) {
	r, err := u.updatesRoot()
	if err != nil {
		return
	}
	defer r.Close()
	d, err := r.Open(".")
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return
	}
	keep := map[string]bool{u.opt.Version: true}
	u.mu.Lock()
	if u.pending != nil {
		keep[u.pending.Version] = true
	}
	if u.other != nil {
		keep[u.other.Version] = true // another copy's package (othercopy.go)
	}
	if u.previous != nil {
		keep[u.previous.Version] = true
	}
	if u.avail != nil {
		if f := u.failed; f == nil || f.Retry || f.Version != u.avail.Manifest.Version {
			keep[u.avail.Manifest.Version] = true
		}
	}
	u.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(name, importPrefix):
			if atStart {
				r.RemoveAll(name)
			}
		case !keep[name]:
			if _, err := parseVersion(name); err == nil {
				if err := r.RemoveAll(name); err != nil {
					u.log.Warn("selfupdate: removing an old update", "version", name, "error", err)
				}
			}
		}
	}
}
