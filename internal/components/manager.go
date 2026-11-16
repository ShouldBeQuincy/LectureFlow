// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// Options configure a Manager.
type Options struct {
	// DataDir is the application data directory; components live in
	// DataDir/components.
	DataDir string
	// AppVersion is the application version ("" = "dev"; a version that is
	// not semver is compatible with every manifest).
	AppVersion string
	// Platform overrides CurrentPlatform (tests).
	Platform string
	// BuiltinManifest is the manifest shipped with the application
	// (resources/components.json); lock paths are relative to its directory.
	BuiltinManifest string
	// ManifestURL is where CheckManifest fetches a newer manifest; "" = the
	// manifest's update_url.
	ManifestURL string
	// UV is the bundled uv executable (resources/bin/uv(.exe)).
	UV string
	// HTTPClient downloads; nil = a client with connection timeouts (a
	// transfer has no overall timeout; StallTimeout bounds silence).
	HTTPClient *http.Client
	// DiskFree returns the bytes available on the volume of a path; nil =
	// the operating system's figure.
	DiskFree func(path string) (int64, error)
	// SelfTest is the update self-test (V8): start the worker with the new
	// version and process one second of silence. nil = updates switch
	// after the install check only (a notice says so).
	SelfTest func(ctx context.Context, req SelfTestRequest) error
	// InUse reports whether something outside the manager uses an installed
	// component right now (its interpreter or its model directory), with a
	// short reason key for the message; nil = nothing ever does. Uninstall
	// and the switch of an update are refused while it returns true
	// (components.in_use_by_task). It is called without the manager's lock
	// held, so it may call Python or ModelDir. See the package doc for what
	// callers must count.
	InUse  func(componentID string) (inUse bool, reason string)
	Logger *slog.Logger
	Now    func() time.Time
	// RetryBackoff is the first delay before resuming a broken transfer
	// (doubles per attempt); default 2s.
	RetryBackoff time.Duration
	// StallTimeout: a transfer without a byte for this long is broken;
	// default 60s.
	StallTimeout time.Duration
	// EventInterval limits progress events per task; default 250ms.
	EventInterval time.Duration
	// CheckTimeout bounds the post-install import check; default 5 minutes.
	CheckTimeout time.Duration
}

// SelfTestRequest describes the new version an update installed. The
// self-test runs the matching worker on one second of silence.
type SelfTestRequest struct {
	ID      string
	Version string
	Kind    string
	Backend string
	// Dir is the new version's directory (not yet active).
	Dir string
	// Python is the interpreter to run the worker with: the new venv
	// (venv and python components) or the backend's active venv (models;
	// "" when it is not installed).
	Python string
	// ModelDir is the new model (model components) or an installed model of
	// the backend (venv components; "" when there is none).
	ModelDir string
}

// Manager manages the components of one data directory. Its methods are
// safe for concurrent use.
type Manager struct {
	root       string
	dataDir    string
	platform   string
	appVersion string
	uvPath     string
	updateURL  string
	log        *slog.Logger
	now        func() time.Time
	diskFree   func(string) (int64, error)
	selfTest   func(context.Context, SelfTestRequest) error
	inUse      func(string) (bool, string)
	client     *http.Client
	dl         *downloader

	eventInterval time.Duration
	checkTimeout  time.Duration

	builtin *Manifest

	// testHook, when set by a test before tasks start, receives internal
	// events ("import.copying", "import.copied") on the task's goroutine.
	testHook func(event string, task TaskID)

	mu        sync.Mutex
	man       *Manifest
	st        *state
	recovered bool
	tasks     map[TaskID]*task
	order     []TaskID
	seq       int
	subs      map[int]*subscriber
	subSeq    int
	closed    bool

	wake       chan struct{}
	stopCh     chan struct{}
	runnerDone chan struct{}
}

// New opens the components directory of opts.DataDir: loads the built-in
// manifest (and a newer compatible one fetched earlier), the state (rebuilt
// from the receipts when state.json is damaged), removes what interrupted
// runs left behind, and starts the background runner.
func New(opts Options) (*Manager, error) {
	if opts.DataDir == "" {
		return nil, errors.New("components: DataDir is required")
	}
	if opts.BuiltinManifest == "" {
		return nil, errors.New("components: BuiltinManifest is required")
	}
	m := &Manager{
		dataDir:       opts.DataDir,
		root:          filepath.Join(opts.DataDir, "components"),
		platform:      opts.Platform,
		appVersion:    opts.AppVersion,
		uvPath:        opts.UV,
		updateURL:     opts.ManifestURL,
		log:           opts.Logger,
		now:           opts.Now,
		diskFree:      opts.DiskFree,
		selfTest:      opts.SelfTest,
		inUse:         opts.InUse,
		client:        opts.HTTPClient,
		eventInterval: opts.EventInterval,
		checkTimeout:  opts.CheckTimeout,
		tasks:         map[TaskID]*task{},
		subs:          map[int]*subscriber{},
		wake:          make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		runnerDone:    make(chan struct{}),
	}
	if m.platform == "" {
		m.platform = CurrentPlatform()
	}
	if m.appVersion == "" {
		m.appVersion = "dev"
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.diskFree == nil {
		m.diskFree = diskFree
	}
	if m.client == nil {
		m.client = defaultHTTPClient()
	}
	if m.eventInterval <= 0 {
		m.eventInterval = 250 * time.Millisecond
	}
	if m.checkTimeout <= 0 {
		m.checkTimeout = 5 * time.Minute
	}
	backoff, stall := opts.RetryBackoff, opts.StallTimeout
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	if stall <= 0 {
		stall = 60 * time.Second
	}
	m.dl = &downloader{client: m.client, dir: filepath.Join(m.root, "downloads"), backoff: backoff, stall: stall, log: m.log}

	builtin, err := LoadManifest(opts.BuiltinManifest)
	if err != nil {
		return nil, fmt.Errorf("components: built-in manifest: %w", err)
	}
	if !builtin.Compatible(m.appVersion) {
		m.log.Warn("components: the built-in manifest does not match the application version",
			"app_version", m.appVersion, "compat", builtin.Compat)
	}
	m.builtin, m.man = builtin, builtin
	if cached := m.loadCachedManifest(); cached != nil {
		m.man = cached
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, cerr(CodeIO, nil, err)
	}
	st, recovered, err := m.loadState()
	if err != nil {
		return nil, err
	}
	m.st, m.recovered = st, recovered
	m.cleanOrphans(st)
	removeAllRetry(m.uvCache())
	removeAllRetry(filepath.Join(m.root, legacyUVCache))
	removeAllRetry(m.importsDir())
	m.cleanStaleDownloads()
	go m.runner()
	return m, nil
}

// Close stops the runner: a running task stops where it is (its partial
// downloads stay for the next start), queued tasks are dropped.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	for _, t := range m.tasks {
		if t.cancel != nil {
			t.cancel(errClosing)
		}
	}
	for id, s := range m.subs {
		close(s.done)
		delete(m.subs, id)
	}
	close(m.stopCh)
	m.mu.Unlock()
	select {
	case <-m.runnerDone:
	case <-time.After(10 * time.Second):
		return errors.New("components: the runner did not stop in time")
	}
	return nil
}

// Root is the components directory.
func (m *Manager) Root() string { return m.root }

// Platform is the platform the manager installs for.
func (m *Manager) Platform() string { return m.platform }

// StateRecovered reports that state.json was damaged at start and rebuilt
// from the installation receipts.
func (m *Manager) StateRecovered() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recovered
}

// uvCache is uv's cache while a task runs. The short name keeps uv's
// unpacked wheels (.uvc/archive-v0/{id}/{package files}) about as deep as
// the venv's site-packages; ".uv-cache" is the name before V12.2, removed
// at start like the cache itself.
func (m *Manager) uvCache() string { return filepath.Join(m.root, ".uvc") }

const legacyUVCache = ".uv-cache"

func (m *Manager) testEvent(event string, id TaskID) {
	if m.testHook != nil {
		m.testHook(event, id)
	}
}

// checkInUse asks Options.InUse about each id (without m.mu held).
func (m *Manager) checkInUse(ids ...string) error {
	if m.inUse == nil {
		return nil
	}
	for _, id := range ids {
		if busy, reason := m.inUse(id); busy {
			if reason == "" {
				reason = "task"
			}
			return cerr(CodeInUseByTask, map[string]any{"id": id, "reason": reason},
				fmt.Errorf("%s is in use (%s)", id, reason))
		}
	}
	return nil
}

func (m *Manager) abs(rel string) string { return filepath.Join(m.root, filepath.FromSlash(rel)) }

// manifest returns the current manifest.
func (m *Manager) manifest() *Manifest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.man
}

// Presets returns the cost tiers of the current manifest.
func (m *Manager) Presets() Presets { return m.manifest().Presets }

// SourceGroups returns the download sources of the current manifest.
func (m *Manager) SourceGroups() map[string]SourceGroup { return m.manifest().Sources }

// SuggestSources returns the sources onboarding preselects for a region
// ("cn": the first mirror of each group marked for it; V3.3).
func (m *Manager) SuggestSources(region string) Sources {
	var s Sources
	man := m.manifest()
	pick := func(g string) string {
		for _, mr := range man.Sources[g].Mirrors {
			if region != "" && mr.Region == region {
				return mr.ID
			}
		}
		return ""
	}
	s.PyPI, s.Models = pick(GroupPyPI), pick(GroupModels)
	return s
}

// ManifestInfo describes the current manifest.
type ManifestInfo struct {
	Serial     int64  `json:"serial"`
	Published  string `json:"published,omitempty"`
	AppVersion string `json:"app_version"`
	Builtin    bool   `json:"builtin"`
}

// ManifestInfo returns the current manifest's identity.
func (m *Manager) ManifestInfo() ManifestInfo {
	man := m.manifest()
	return ManifestInfo{Serial: man.Serial, Published: man.Published, AppVersion: man.AppVersion, Builtin: man == m.builtin}
}

// ---------------------------------------------------------------------------
// List, Python, ModelDir
// ---------------------------------------------------------------------------

// Status is a component of the current platform with its install state.
type Status struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	NameKey  string `json:"name_key,omitempty"`
	DescKey  string `json:"desc_key,omitempty"`
	Version  string `json:"version"` // the manifest's version
	Upstream string `json:"upstream,omitempty"`
	Bundled  bool   `json:"bundled"`

	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version,omitempty"`
	UpdateAvailable  bool   `json:"update_available"`
	// RebuildNeeded: a venv built with a python runtime that is no longer
	// the active one (Update rebuilds it).
	RebuildNeeded bool `json:"rebuild_needed"`
	// Broken: state.json lists it but its interpreter or files are missing.
	Broken bool `json:"broken"`
	// Orphaned: installed, but the current manifest no longer has it.
	Orphaned bool `json:"orphaned"`

	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	Depends     []string `json:"depends,omitempty"`
	Backend     string   `json:"backend,omitempty"`
	Model       string   `json:"model,omitempty"`
	Precision   string   `json:"precision,omitempty"`
	License     string   `json:"license,omitempty"`
	LicenseNote string   `json:"license_note,omitempty"`
	Source      string   `json:"source,omitempty"`
	Revision    string   `json:"revision,omitempty"`

	DownloadSize  int64 `json:"download_size"`
	InstalledSize int64 `json:"installed_size"`
	// DiskUsage is the installed version's size on disk.
	DiskUsage int64 `json:"disk_usage"`
	// Task is the active task that installs or updates it.
	Task TaskID `json:"task,omitempty"`
}

// List returns the components of the current platform (manifest order),
// then installed components the manifest no longer lists.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := m.busyIDs()
	var out []Status
	seen := map[string]bool{}
	for i := range m.man.Components {
		c := &m.man.Components[i]
		b := c.Build(m.platform)
		if b == nil && !c.Bundled {
			continue
		}
		seen[c.ID] = true
		s := Status{
			ID: c.ID, Kind: c.Kind, NameKey: c.NameKey, DescKey: c.DescKey, Version: c.Version,
			Upstream: c.Upstream, Bundled: c.Bundled, Depends: c.DependsOn(m.platform), Backend: c.Backend,
			Model: c.Model, Precision: c.Precision, License: c.License, LicenseNote: c.LicenseNote,
			Source: c.Source, Revision: c.Revision, Task: busy[c.ID],
		}
		reason := c.availableFor(m.platform, m.appVersion)
		s.Available, s.UnavailableReason = reason == "", reason
		if b != nil {
			s.DownloadSize, s.InstalledSize = b.DownloadSize, b.InstalledSize
		}
		if c.Bundled {
			s.Installed, s.InstalledVersion, s.Version = true, m.appVersion, m.appVersion
		} else if e, ok := m.st.Active[c.ID]; ok {
			m.fillInstalled(&s, e)
			s.UpdateAvailable = CompareVersions(c.Version, e.Version) > 0
		}
		out = append(out, s)
	}
	for _, id := range m.st.sortedIDs() {
		if seen[id] {
			continue
		}
		e := m.st.Active[id]
		s := Status{ID: id, Kind: e.Kind, Version: e.Version, Orphaned: true, Task: busy[id]}
		m.fillInstalled(&s, e)
		out = append(out, s)
	}
	return out
}

func (m *Manager) fillInstalled(s *Status, e Installed) {
	s.Installed, s.InstalledVersion, s.DiskUsage = true, e.Version, e.SizeBytes
	if e.Kind == KindVenv {
		if py, ok := m.st.Active[IDPython]; ok && e.Python != "" && py.Version != e.Python {
			s.RebuildNeeded = true
		}
	}
	s.Broken = !m.intact(e)
}

// intact checks the entry's essential files (cheap stats).
func (m *Manager) intact(e Installed) bool {
	dir := m.abs(e.Dir)
	switch e.Kind {
	case KindVenv:
		return venvOK(filepath.Join(dir, "venv"))
	case KindPythonRuntime:
		exe, ok := m.exePath(e)
		return ok && venvOK(filepath.Join(dir, "venv")) && fileExists(exe)
	case KindTool:
		exe, ok := m.exePath(e)
		return ok && fileExists(exe)
	case KindModel, KindBinary:
		for _, f := range e.Files {
			p, ok := m.filePath(e, f)
			if !ok {
				return false
			}
			if fi, err := os.Stat(p); err != nil || fi.Size() != f.Size {
				return false
			}
		}
		return true
	}
	return true
}

// venvOK: the venv's interpreter exists and pyvenv.cfg's home (the base
// runtime, an absolute path) is there; a moved data directory without
// Relocated fails here.
func venvOK(venv string) bool {
	if !fileExists(venvPython(venv)) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "home" {
			fi, err := os.Stat(strings.TrimSpace(v))
			return err == nil && fi.IsDir()
		}
	}
	return false
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// busyIDs maps component ids to the active task that has them. Caller holds m.mu.
func (m *Manager) busyIDs() map[string]TaskID {
	out := map[string]TaskID{}
	for _, id := range m.order {
		t := m.tasks[id]
		if !t.active() {
			continue
		}
		for _, it := range t.items {
			if _, ok := out[it.id]; !ok && it.id != "" && it.phase != PhaseDone && it.phase != PhaseSkipped {
				out[it.id] = t.id
			}
		}
	}
	return out
}

// venvComponent returns the venv component id of a backend.
func (m *Manager) venvComponent(backend string) string {
	if backend == BackendBase {
		return IDPython
	}
	for _, c := range m.man.Components {
		if c.Kind == KindVenv && c.Backend == backend {
			return c.ID
		}
	}
	// installed but no longer in the manifest
	for _, id := range m.st.sortedIDs() {
		if e := m.st.Active[id]; e.Kind == KindVenv && e.Backend == backend {
			return id
		}
	}
	return ""
}

// Python returns the interpreter of a backend's environment: "base" (the
// python component's venv: pypdf), "faster-whisper", "openvino", "mlx".
// false when the component is not installed (or its interpreter is missing).
func (m *Manager) Python(backend string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.venvComponent(backend)
	if id == "" {
		return "", false
	}
	e, ok := m.st.Active[id]
	if !ok {
		return "", false
	}
	p := venvPython(filepath.Join(m.abs(e.Dir), "venv"))
	if !fileExists(p) {
		return "", false
	}
	return p, true
}

// ModelDir returns the directory of an installed model. faster-whisper
// models are the same CTranslate2 files for every compute type, so any
// installed precision of the model serves when the exact one is missing.
func (m *Manager) ModelDir(backend, name, precision string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.st.Active[ModelID(backend, name, precision)]; ok {
		return filepath.Join(m.abs(e.Dir), "model"), true
	}
	if backend != BackendFasterWhisper {
		return "", false
	}
	var cands []string
	for _, id := range m.st.sortedIDs() {
		if b, n, _, ok := ParseModelID(id); ok && b == backend && n == name {
			cands = append(cands, id)
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return strings.HasSuffix(cands[i], ":float16") && !strings.HasSuffix(cands[j], ":float16")
	})
	return filepath.Join(m.abs(m.st.Active[cands[0]].Dir), "model"), true
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

// Plan is what an install needs.
type Plan struct {
	Items []PlanItem `json:"items"`
	// DownloadBytes still to download (partial downloads and files other
	// components already have are not counted again).
	DownloadBytes  int64 `json:"download_bytes"`
	InstalledBytes int64 `json:"installed_bytes"`
	// RequiredBytes is the free space the install needs at its peak;
	// FreeBytes what the volume has (-1 = unknown).
	RequiredBytes int64 `json:"required_bytes"`
	FreeBytes     int64 `json:"free_bytes"`
	Enough        bool  `json:"enough"`
}

// PlanItem is one component of a plan, in install order.
type PlanItem struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Kind       string `json:"kind"`
	Dependency bool   `json:"dependency"` // added because a requested component needs it
	Installed  bool   `json:"installed"`  // nothing to do
	Update     bool   `json:"update"`     // replaces the installed version

	DownloadBytes  int64 `json:"download_bytes"`
	InstalledBytes int64 `json:"installed_bytes"`
}

// planMargin is free space kept on top of the computed need.
const planMargin = 256 << 20

// Plan completes the dependencies of ids and computes sizes and the disk
// space check. On Windows it refuses with components.path_too_long a
// component that is not installed yet and whose deepest file would get a
// path longer than MaxPathLen under the data directory (pathlen.go).
func (m *Manager) Plan(ids []string) (Plan, error) {
	m.mu.Lock()
	man, st := m.man, m.st.clone()
	m.mu.Unlock()
	return m.plan(man, st, ids, nil)
}

// plan builds a plan; update lists the ids that replace their installed
// version.
func (m *Manager) plan(man *Manifest, st *state, ids []string, update map[string]bool) (Plan, error) {
	var p Plan
	if len(ids) == 0 {
		return p, cerr(CodeUnknown, map[string]any{"id": ""}, errors.New("no component requested"))
	}
	requested := map[string]bool{}
	for _, id := range ids {
		requested[id] = true
	}
	var order []string
	visited := map[string]bool{}
	var visit func(id string) error
	visit = func(id string) error {
		if visited[id] {
			return nil
		}
		visited[id] = true
		c := man.Component(id)
		if c == nil {
			return cerr(CodeUnknown, map[string]any{"id": id}, fmt.Errorf("unknown component %q", id))
		}
		if c.Bundled {
			return nil
		}
		if r := c.availableFor(m.platform, m.appVersion); r != "" {
			if r == "platform" {
				return cerr(CodeUnsupportedPlatform, map[string]any{"id": id, "platform": m.platform},
					fmt.Errorf("%s is not available for %s", id, m.platform))
			}
			return cerr(CodeUnavailable, map[string]any{"id": id, "reason": r}, fmt.Errorf("%s is unavailable: %s", id, r))
		}
		for _, d := range c.DependsOn(m.platform) {
			if err := visit(d); err != nil {
				return err
			}
		}
		order = append(order, id)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return p, err
		}
	}
	reusable := m.installedFiles(st)
	for _, id := range order {
		c := man.Component(id)
		b := c.Build(m.platform)
		it := PlanItem{ID: id, Version: c.Version, Kind: c.Kind, Dependency: !requested[id], Update: update[id]}
		if _, ok := st.Active[id]; ok && !it.Update {
			it.Installed = true
		}
		if !it.Installed {
			if err := m.checkPathLen(c, b); err != nil {
				return Plan{}, err
			}
			it.InstalledBytes = b.InstalledSize
			it.DownloadBytes = m.remainingDownload(b, reusable)
			need := it.DownloadBytes
			if c.Kind == KindVenv || c.Kind == KindPythonRuntime {
				need += b.InstalledSize // the archive or the uv cache next to the unpacked files
			} else {
				for _, f := range b.Files {
					if f.Unpack != "" {
						need += b.InstalledSize
						break
					}
				}
			}
			p.DownloadBytes += it.DownloadBytes
			p.InstalledBytes += it.InstalledBytes
			p.RequiredBytes += need
		}
		p.Items = append(p.Items, it)
	}
	p.FreeBytes = -1
	p.Enough = true
	if p.RequiredBytes > 0 {
		p.RequiredBytes += planMargin
		os.MkdirAll(m.root, 0o755)
		if free, err := m.diskFree(m.root); err == nil {
			p.FreeBytes = free
			p.Enough = free >= p.RequiredBytes
		} else {
			m.log.Warn("components: cannot read the free disk space", "error", err)
		}
	}
	return p, nil
}

// installedFiles maps sha256 → an installed file with that content.
func (m *Manager) installedFiles(st *state) map[string]string {
	out := map[string]string{}
	for _, e := range st.Active {
		for _, f := range e.Files {
			if p, ok := m.filePath(e, f); ok {
				out[f.SHA256] = p
			}
		}
	}
	return out
}

// remainingDownload: the build's download size minus what is on disk.
func (m *Manager) remainingDownload(b *Build, reusable map[string]string) int64 {
	if len(b.Files) == 0 {
		return b.DownloadSize // venv: the wheels (uv caches what it got)
	}
	var n int64
	for _, f := range b.Files {
		spec := fileSpec{Name: f.Name, SHA256: f.SHA256, Size: f.Size}
		if _, ok := reusable[f.SHA256]; ok && f.Unpack == "" {
			continue
		}
		if m.dl.have(spec) {
			continue
		}
		n += f.Size - m.dl.partial(spec)
	}
	if b.Lock != nil { // python: the base venv's wheels
		n += b.DownloadSize - sumSizes(b.Files)
	}
	return n
}

func sumSizes(fs []File) int64 {
	var n int64
	for _, f := range fs {
		n += f.Size
	}
	return n
}

func diskFullError(p Plan) error {
	return cerr(CodeDiskFull, map[string]any{
		"required_mb": mb(p.RequiredBytes), "free_mb": mb(p.FreeBytes),
	}, fmt.Errorf("not enough disk space: %d bytes needed, %d free", p.RequiredBytes, p.FreeBytes))
}

func mb(n int64) int64 {
	if n < 0 {
		return -1
	}
	return (n + (1 << 20) - 1) >> 20
}

// ---------------------------------------------------------------------------
// Install, Update, ImportFile
// ---------------------------------------------------------------------------

// Install starts a background task that installs ids and their missing
// dependencies (already installed components are left as they are; Update
// replaces them). It fails at once when the disk lacks space.
func (m *Manager) Install(ids []string, src Sources) (TaskID, error) {
	m.mu.Lock()
	man, st, closed := m.man, m.st.clone(), m.closed
	m.mu.Unlock()
	if closed {
		return "", errors.New("components: manager closed")
	}
	if err := man.ValidateSources(src); err != nil {
		return "", err
	}
	p, err := m.plan(man, st, ids, nil)
	if err != nil {
		return "", err
	}
	if !p.Enough {
		return "", diskFullError(p)
	}
	var items []*item
	for _, pi := range p.Items {
		if pi.Installed {
			continue
		}
		items = append(items, &item{id: pi.ID, version: pi.Version, dependency: pi.Dependency,
			phase: PhasePending, total: man.Component(pi.ID).Build(m.platform).DownloadSize})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.newTask(TaskInstall, ids, src, man, items)
	m.emitComponents()
	return t.id, nil
}

// Update replaces an installed component with the manifest's version (or
// rebuilds a venv made with an older python runtime): the old version stays
// active until the new one passed the install check and the self-test, a
// failure leaves the old one in place (V8). Updating python also rebuilds
// the installed venvs on the new runtime.
func (m *Manager) Update(id string) (TaskID, error) {
	return m.UpdateFrom(id, Sources{})
}

// UpdateFrom is Update with download sources.
func (m *Manager) UpdateFrom(id string, src Sources) (TaskID, error) {
	m.mu.Lock()
	man, st, closed := m.man, m.st.clone(), m.closed
	busy := m.busyIDs()
	m.mu.Unlock()
	if closed {
		return "", errors.New("components: manager closed")
	}
	if err := man.ValidateSources(src); err != nil {
		return "", err
	}
	c := man.Component(id)
	if c == nil {
		return "", cerr(CodeUnknown, map[string]any{"id": id}, fmt.Errorf("unknown component %q", id))
	}
	if c.Bundled {
		return "", cerr(CodeBundled, map[string]any{"id": id}, errors.New("bundled with the application"))
	}
	cur, ok := st.Active[id]
	if !ok {
		return "", cerr(CodeNotInstalled, map[string]any{"id": id}, fmt.Errorf("%s is not installed", id))
	}
	if t, ok := busy[id]; ok {
		return "", cerr(CodeBusy, map[string]any{"id": id, "task": string(t)}, fmt.Errorf("%s is busy", id))
	}
	newer := CompareVersions(c.Version, cur.Version) > 0
	rebuild := c.Kind == KindVenv && cur.Python != "" && st.Active[IDPython].Version != cur.Python
	if !newer && !rebuild {
		return "", cerr(CodeUpToDate, map[string]any{"id": id, "version": cur.Version}, fmt.Errorf("%s is up to date", id))
	}
	ids := []string{id}
	update := map[string]bool{id: true}
	if id == IDPython {
		for _, oid := range st.sortedIDs() {
			if e := st.Active[oid]; e.Kind == KindVenv && man.Component(oid) != nil {
				ids = append(ids, oid)
				update[oid] = true
			}
		}
	}
	// refused at once while in use (the switch checks again: the work
	// before it can take a while)
	if err := m.checkInUse(ids...); err != nil {
		return "", err
	}
	p, err := m.plan(man, st, ids, update)
	if err != nil {
		return "", err
	}
	if !p.Enough {
		return "", diskFullError(p)
	}
	var items []*item
	for _, pi := range p.Items {
		if pi.Installed {
			continue
		}
		items = append(items, &item{id: pi.ID, version: man.Component(pi.ID).Version, update: pi.Update,
			dependency: pi.Dependency, phase: PhasePending, total: man.Component(pi.ID).Build(m.platform).DownloadSize})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.newTask(TaskUpdate, []string{id}, src, man, items)
	m.emitComponents()
	return t.id, nil
}

// ImportFile starts a task that installs a component archive (V3.3 offline
// install): the archive's SHA-256 must equal the offline entry of a
// component in the current manifest.
//
// The task works on a private copy of the file (see importfile.go). It
// fails at once when the disk lacks the space for the copy, the extracted
// entries and the installed component.
func (m *Manager) ImportFile(path string) (TaskID, error) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return "", cerr(CodeImportInvalid, nil, fmt.Errorf("cannot read %s: %v", path, err))
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return "", errors.New("components: manager closed")
	}
	if err := m.importPathCheck(path); err != nil {
		return "", err
	}
	if p := m.importPlan(path, fi.Size()); !p.Enough {
		return "", diskFullError(p)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("components: manager closed")
	}
	it := &item{id: "", phase: PhasePending, total: fi.Size(), offline: &offlineSource{orig: path}}
	t := m.newTask(TaskImport, nil, Sources{}, m.man, []*item{it})
	t.importPath = path
	return t.id, nil
}

// ---------------------------------------------------------------------------
// Tasks, Pause, Resume, Cancel
// ---------------------------------------------------------------------------

// Tasks returns the tasks of this session (active ones and the last
// finished ones), oldest first.
func (m *Manager) Tasks() []TaskStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	out := make([]TaskStatus, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.tasks[id].status(now))
	}
	return out
}

// Task returns one task.
func (m *Manager) Task(id TaskID) (TaskStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return TaskStatus{}, false
	}
	return t.status(m.now()), true
}

func (m *Manager) getTask(id TaskID) (*task, error) {
	t, ok := m.tasks[id]
	if !ok {
		return nil, cerr(CodeTaskNotFound, map[string]any{"task": string(id)}, fmt.Errorf("no task %s", id))
	}
	return t, nil
}

func taskStateErr(t *task) error {
	return cerr(CodeTaskState, map[string]any{"task": string(t.id), "state": t.state},
		fmt.Errorf("task %s is %s", t.id, t.state))
}

// Pause stops a queued or running task where it is; Resume continues it
// (downloads resume from the partial files).
func (m *Manager) Pause(id TaskID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.getTask(id)
	if err != nil {
		return err
	}
	switch t.state {
	case TaskQueued:
		t.state = TaskPaused
		m.emitTask(t, true)
	case TaskRunning:
		if t.cancel != nil {
			t.cancel(errPaused)
		}
	default:
		return taskStateErr(t)
	}
	return nil
}

// Resume queues a paused task again.
func (m *Manager) Resume(id TaskID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.getTask(id)
	if err != nil {
		return err
	}
	if t.state != TaskPaused {
		return taskStateErr(t)
	}
	t.state = TaskQueued
	m.emitTask(t, true)
	m.wakeRunner()
	return nil
}

// Retry queues a failed or cancelled task again, optionally with other
// download sources ("重试" / "换一个下载源", V3.3): finished items stay
// finished, downloads resume from what is on disk. The disk space check
// runs again first.
func (m *Manager) Retry(id TaskID, src *Sources) error {
	m.mu.Lock()
	t, err := m.getTask(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if t.state != TaskFailed && t.state != TaskCancelled {
		m.mu.Unlock()
		return taskStateErr(t)
	}
	man, st := t.man, m.st.clone()
	var pending []string
	for _, it := range t.items {
		if it.id != "" && it.phase != PhaseDone && it.phase != PhaseSkipped {
			pending = append(pending, it.id)
		}
	}
	m.mu.Unlock()
	if src != nil {
		if err := man.ValidateSources(*src); err != nil {
			return err
		}
	}
	if t.kind == TaskImport {
		if fi, err := os.Stat(t.importPath); err != nil || fi.IsDir() {
			return cerr(CodeImportInvalid, nil, fmt.Errorf("cannot read %s: %v", t.importPath, err))
		} else if p := m.importPlan(t.importPath, fi.Size()); !p.Enough {
			return diskFullError(p)
		}
	} else if len(pending) > 0 {
		update := map[string]bool{}
		for _, it := range t.items {
			if it.update {
				update[it.id] = true
			}
		}
		p, err := m.plan(man, st, pending, update)
		if err != nil {
			return err
		}
		if !p.Enough {
			return diskFullError(p)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.state != TaskFailed && t.state != TaskCancelled {
		return taskStateErr(t)
	}
	if src != nil {
		t.src = *src
	}
	t.err, t.notices = nil, nil
	for _, it := range t.items {
		if it.phase != PhaseDone && it.phase != PhaseSkipped {
			it.phase, it.err, it.curBytes, it.doneBytes = PhasePending, nil, 0, 0
		}
	}
	t.state = TaskQueued
	m.emitTask(t, true)
	m.emitComponents()
	m.wakeRunner()
	return nil
}

// Cancel stops a task and deletes what it downloaded and unpacked (files
// another active task needs stay). Allowed for queued, running, paused and
// failed tasks.
func (m *Manager) Cancel(id TaskID) error {
	m.mu.Lock()
	t, err := m.getTask(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	switch t.state {
	case TaskRunning:
		if t.cancel != nil {
			t.cancel(errCancelled)
		}
		m.mu.Unlock()
		return nil
	case TaskQueued, TaskPaused, TaskFailed:
		t.state = TaskCancelled // the runner no longer picks it
		info := model.ErrorInfo{Code: CodeCancelled}
		t.err = &info
		m.mu.Unlock()
		m.cleanupTask(t, true)
		m.mu.Lock()
		m.emitTask(t, true)
		m.emitComponents()
		m.mu.Unlock()
		return nil
	default:
		m.mu.Unlock()
		return taskStateErr(t)
	}
}

// Subscribe registers fn for events; fn runs on its own goroutine, in
// order, and may be slow (progress events of a task then coalesce). The
// returned function unsubscribes.
func (m *Manager) Subscribe(fn func(Event)) (cancel func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subSeq++
	id := m.subSeq
	s := newSubscriber(fn)
	if m.closed {
		close(s.done)
		return func() {}
	}
	m.subs[id] = s
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if _, ok := m.subs[id]; ok {
				delete(m.subs, id)
				close(s.done)
			}
			m.mu.Unlock()
		})
	}
}

// ---------------------------------------------------------------------------
// Uninstall
// ---------------------------------------------------------------------------

// Uninstall removes an installed component. Bundled components cannot be
// removed, a component other installed components depend on neither, nor
// one an active task is working on, nor one Options.InUse reports in use.
func (m *Manager) Uninstall(id string) error {
	m.mu.Lock()
	_, err := m.uninstallCheck(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	// InUse runs without the lock (it may call Python or ModelDir); the
	// checks run again afterwards
	if err := m.checkInUse(id); err != nil {
		return err
	}
	m.mu.Lock()
	e, err := m.uninstallCheck(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	ns := m.st.clone()
	delete(ns.Active, id)
	if err := m.writeState(ns); err != nil {
		m.mu.Unlock()
		return err
	}
	m.st = ns
	drop := m.gcRetained()
	m.emitComponents()
	m.mu.Unlock()
	m.removeDirs(drop)
	if err := removeAllRetry(m.abs(e.Dir)); err != nil {
		// not installed any more; the next start removes the rest
		m.log.Warn("components: cannot remove all files", "id", id, "error", err)
	}
	removeIfEmpty(filepath.Dir(m.abs(e.Dir)))
	return nil
}

// uninstallCheck returns the entry to remove or why it cannot go. Caller
// holds m.mu.
func (m *Manager) uninstallCheck(id string) (Installed, error) {
	c := m.man.Component(id)
	if c != nil && c.Bundled {
		return Installed{}, cerr(CodeBundled, map[string]any{"id": id}, errors.New("bundled with the application"))
	}
	e, ok := m.st.Active[id]
	if !ok {
		return e, cerr(CodeNotInstalled, map[string]any{"id": id}, fmt.Errorf("%s is not installed", id))
	}
	busy := m.busyIDs()
	if t, ok := busy[id]; ok {
		return e, cerr(CodeBusy, map[string]any{"id": id, "task": string(t)}, fmt.Errorf("%s is busy", id))
	}
	// an active task still has to install something that needs it
	for bid, t := range busy {
		if bc := m.man.Component(bid); bc != nil && contains(bc.DependsOn(m.platform), id) {
			return e, cerr(CodeBusy, map[string]any{"id": id, "task": string(t)}, fmt.Errorf("%s is needed by %s being installed", id, bid))
		}
	}
	var dependents []string
	for _, oid := range m.st.sortedIDs() {
		if oc := m.man.Component(oid); oc != nil && contains(oc.DependsOn(m.platform), id) {
			dependents = append(dependents, oid)
		} else if oc == nil && ((id == IDPython && m.st.Active[oid].Kind == KindVenv) || envUses(m.st.Active[oid].Env, id)) {
			dependents = append(dependents, oid)
		}
	}
	if len(dependents) > 0 {
		return e, cerr(CodeInUse, map[string]any{"id": id, "dependents": strings.Join(dependents, ", ")},
			fmt.Errorf("%s is needed by %s", id, strings.Join(dependents, ", ")))
	}
	return e, nil
}

// gcRetained drops retained python runtimes no installed venv uses and
// returns their directories for removeDirs (after unlocking). Caller holds
// m.mu.
func (m *Manager) gcRetained() []string {
	if len(m.st.Retained) == 0 {
		return nil
	}
	used := map[string]bool{}
	for _, e := range m.st.Active {
		if e.Kind == KindVenv && e.Python != "" {
			used[e.Python] = true
		}
	}
	ns := m.st.clone()
	ns.Retained = nil
	var drop []string
	for _, r := range m.st.Retained {
		if used[r.Version] {
			ns.Retained = append(ns.Retained, r)
		} else {
			drop = append(drop, m.abs(r.Dir))
		}
	}
	if len(drop) == 0 {
		return nil
	}
	if err := m.writeState(ns); err != nil {
		m.log.Warn("components: cannot update state.json", "error", err)
		return nil
	}
	m.st = ns
	return drop
}

// removeDirs deletes directories that are no longer referenced.
func (m *Manager) removeDirs(dirs []string) {
	for _, d := range dirs {
		if err := removeAllRetry(d); err != nil {
			m.log.Warn("components: cannot remove an unused directory", "dir", d, "error", err)
		}
		removeIfEmpty(filepath.Dir(d))
	}
}

// ---------------------------------------------------------------------------
// Manifests from the network
// ---------------------------------------------------------------------------

const cachedManifestName = "manifest.json"

func (m *Manager) loadCachedManifest() *Manifest {
	p := filepath.Join(m.root, cachedManifestName)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	man, err := ParseManifest(data, filepath.Join(m.root, "manifest.d"))
	if err != nil {
		m.log.Warn("components: ignoring the downloaded manifest", "error", err)
		return nil
	}
	if !man.Compatible(m.appVersion) || man.Serial <= m.builtin.Serial {
		return nil
	}
	return man
}

// CheckManifest fetches the manifest at Options.ManifestURL (or the
// manifest's update_url) and adopts it when it is valid, compatible with
// the application version and newer (a larger serial). Incompatible
// manifests are rejected with components.manifest_incompatible.
func (m *Manager) CheckManifest(ctx context.Context) (bool, error) {
	cur := m.manifest()
	u := m.updateURL
	if u == "" {
		u = cur.UpdateURL
	}
	if u == "" {
		return false, cerr(CodeNoUpdateURL, nil, errors.New("no manifest update URL"))
	}
	// (with the redirect chain's state, download.go)
	req, err := http.NewRequestWithContext(withRedirectChain(ctx), http.MethodGet, u, nil)
	if err != nil {
		return false, cerr(CodeNetwork, nil, err)
	}
	req.Header.Set("User-Agent", "LectureFlow-components")
	resp, err := m.client.Do(req)
	if err != nil {
		return false, asCoded(err, nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, cerr(CodeHTTPStatus, map[string]any{"status": resp.StatusCode}, &httpStatusError{resp.StatusCode})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return false, asCoded(err, nil)
	}
	man, err := ParseManifest(data, filepath.Join(m.root, "manifest.d"))
	if err != nil {
		return false, err
	}
	if !man.Compatible(m.appVersion) {
		return false, cerr(CodeManifestIncompatible, map[string]any{"app_version": m.appVersion, "compat": man.Compat},
			fmt.Errorf("the manifest needs application versions %s", man.Compat))
	}
	for _, c := range man.Components {
		for _, b := range c.Platforms {
			if b != nil && b.Lock != nil && b.Lock.URL == "" && c.Unavailable == "" {
				if !m.builtinHasLock(b.Lock) {
					return false, cerr(CodeManifestInvalid, map[string]any{"id": c.ID},
						fmt.Errorf("%s: a downloaded manifest needs lock URLs", c.ID))
				}
			}
		}
	}
	if man.Serial <= cur.Serial {
		return false, nil
	}
	if err := writeFileAtomic(filepath.Join(m.root, cachedManifestName), data); err != nil {
		return false, cerr(CodeIO, nil, err)
	}
	m.mu.Lock()
	m.man = man
	m.emit(EventManifest, Event{Type: EventManifest})
	m.emitComponents()
	m.mu.Unlock()
	return true, nil
}

func (m *Manager) builtinHasLock(l *Lock) bool {
	sum, _, err := sha256File(filepath.Join(m.builtin.dir, filepath.FromSlash(l.Path)), nil)
	return err == nil && sum == l.SHA256
}

// cleanStaleDownloads removes downloads no component of the current
// manifest has (partial downloads of current files stay and resume).
func (m *Manager) cleanStaleDownloads() {
	known := map[string]bool{}
	for _, c := range m.man.Components {
		for _, b := range c.Platforms {
			if b == nil {
				continue
			}
			for _, f := range b.Files {
				known[f.SHA256] = true
			}
			if b.Lock != nil {
				known[b.Lock.SHA256] = true
			}
		}
	}
	es, err := os.ReadDir(m.dl.dir)
	if err != nil {
		return
	}
	for _, e := range es {
		sha := strings.TrimSuffix(e.Name(), ".part")
		if !known[sha] {
			os.RemoveAll(filepath.Join(m.dl.dir, e.Name()))
		}
	}
	removeIfEmpty(m.dl.dir)
}
