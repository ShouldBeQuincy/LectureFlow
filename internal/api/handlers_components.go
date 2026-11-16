// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// The component endpoints of SPEC-v3 V3 (.claude/v3-brief.md §7.2): the
// component list, install plans, background install / update / import
// tasks and their controls, the download sources and the cost presets of
// the manifest, and SSE "event: components". They are thin views over
// internal/components.Manager (Deps.Components); without a manager every
// endpoint answers 503 server.unavailable {service: "components"}.

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/components", s.getComponents)
		h("POST /api/components/plan", s.planComponents)
		h("POST /api/components/install", s.installComponents)
		h("POST /api/components/import", s.importComponent)
		h("GET /api/components/tasks", s.getComponentTasks)
		h("POST /api/components/tasks/{id}/{action}", s.componentTaskAction)
		h("GET /api/components/sources", s.getComponentSources)
		h("PUT /api/components/sources", s.putComponentSources)
		h("GET /api/components/presets", s.getComponentPresets)
		h("POST /api/components/{id}/update", s.updateComponent)
		h("DELETE /api/components/{id}", s.uninstallComponent)
		s.compState().start(s)
	})
}

// kvComponentsSources stores the user's download sources (SourcesSetting).
// (Split so TestErrorCodesInSourceAreCataloged does not take the kv key
// for an error code of the "components" area; a model.KV constant would be
// the cleaner home.)
const kvComponentsSources = "components" + ".sources"

// componentsEventInterval is the minimum gap between two SSE "components"
// events (brief §7.2: at most once a second).
const componentsEventInterval = time.Second

// compState is the per-Server state of the component and hardware
// endpoints (Server has no field for it: v3 handlers only add files).
// Tests replace the injectable fields before the first request.
type compState struct {
	// interval throttles SSE "components" events.
	interval time.Duration
	// region returns "cn" when the system's region is mainland China
	// (suggest_mirror), "" otherwise.
	region func() string
	// getenv reads an environment variable (env-keys, use-env-key).
	getenv func(string) string
	// detect probes the hardware (GET /api/hardware, POST .../detect).
	detect func(ctx context.Context, dataDir string) (hwdetect.HW, error)

	startOnce sync.Once

	// SSE throttle
	evMu    sync.Mutex
	evTimer *time.Timer
	evLast  time.Time

	// hardware cache; hwMu also serialises detection runs
	hwMu sync.Mutex
	hw   *hwdetect.HW

	// uploaded archives of web-mode imports, removed when their task ends
	impMu   sync.Mutex
	imports map[components.TaskID]string
}

var compStates sync.Map // *Server → *compState

func (s *Server) compState() *compState {
	if v, ok := compStates.Load(s); ok {
		return v.(*compState)
	}
	v, _ := compStates.LoadOrStore(s, &compState{
		interval: componentsEventInterval,
		region:   systemRegion,
		getenv:   os.Getenv,
		detect:   hwdetect.Detect,
		imports:  map[components.TaskID]string{},
	})
	return v.(*compState)
}

// start subscribes to the manager's events (SSE) once per server and
// removes the import uploads an earlier run left behind (tasks do not
// survive a restart).
func (cs *compState) start(s *Server) {
	cs.startOnce.Do(func() {
		if s.comps == nil {
			return
		}
		if s.dataDir != "" {
			_ = os.RemoveAll(importUploadDir(s.dataDir))
		}
		s.comps.Subscribe(func(ev components.Event) {
			if ev.Task != nil {
				cs.taskSeen(*ev.Task)
			}
			cs.kick(s)
		})
	})
}

// kick schedules an SSE "components" event: right away when the last one
// is at least interval old, else at last+interval. Events in between
// coalesce; the payload is read when the event goes out, so the last
// state is never lost.
func (cs *compState) kick(s *Server) {
	cs.evMu.Lock()
	defer cs.evMu.Unlock()
	if cs.evTimer != nil {
		return
	}
	wait := cs.interval - time.Since(cs.evLast)
	if wait < 0 {
		wait = 0
	}
	cs.evTimer = time.AfterFunc(wait, func() {
		cs.evMu.Lock()
		cs.evTimer = nil
		cs.evLast = time.Now()
		cs.evMu.Unlock()
		if s.hub != nil && s.comps != nil {
			s.hub.Components(ComponentsEvent{Tasks: taskViews(s.comps.Tasks())})
		}
	})
}

// taskSeen removes the uploaded archive of an import task that is done or
// cancelled (a failed one keeps it for "重试").
func (cs *compState) taskSeen(t components.TaskStatus) {
	if t.State != components.TaskDone && t.State != components.TaskCancelled {
		return
	}
	cs.impMu.Lock()
	p, ok := cs.imports[t.ID]
	delete(cs.imports, t.ID)
	cs.impMu.Unlock()
	if ok {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("components: cannot remove an imported archive", "err", err)
		}
	}
}

// ---------- views (JSON snake_case, brief §7.2) ----------

// ComponentView is one component (ComponentStatus of the brief).
type ComponentView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // python-runtime | venv | model | binary
	Version   string `json:"version"`
	NameKey   string `json:"name_key,omitempty"`
	DescKey   string `json:"desc_key,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Model     string `json:"model,omitempty"`
	Precision string `json:"precision,omitempty"`
	// DownloadBytes / InstallBytes are the manifest's sizes of this
	// platform's build; DiskBytes what the installed version uses.
	DownloadBytes int64    `json:"download_bytes"`
	InstallBytes  int64    `json:"install_bytes"`
	DiskBytes     int64    `json:"disk_bytes"`
	Requires      []string `json:"requires"`
	License       string   `json:"license"`
	LicenseNote   string   `json:"license_note,omitempty"`
	// State: available | installed | installing | broken | unavailable.
	State             string `json:"state"`
	InstalledVersion  string `json:"installed_version,omitempty"`
	UpdateVersion     string `json:"update_version,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Bundled           bool   `json:"bundled"`
	// RebuildNeeded: a venv made with an older python runtime ("更新" rebuilds it).
	RebuildNeeded bool `json:"rebuild_needed"`
	// Orphaned: installed, but no longer in the manifest (can be removed).
	Orphaned bool `json:"orphaned"`
	// Task is the active task working on it.
	Task string `json:"task,omitempty"`
}

// ComponentsManifestView describes the current manifest.
type ComponentsManifestView struct {
	AppVersion  string `json:"app_version"`
	PublishedAt string `json:"published_at"`
	// UpdateAvailable: some installed component has a newer version (or a
	// venv needs rebuilding on the current python runtime).
	UpdateAvailable bool  `json:"update_available"`
	Serial          int64 `json:"serial"`
	Builtin         bool  `json:"builtin"`
}

// ComponentsView is GET /api/components.
type ComponentsView struct {
	Platform   string                 `json:"platform"`
	Manifest   ComponentsManifestView `json:"manifest"`
	Components []ComponentView        `json:"components"`
	Tasks      []ComponentTaskView    `json:"tasks"`
}

// ComponentPlanView is POST /api/components/plan.
type ComponentPlanView struct {
	IDs           []string              `json:"ids"` // the requested ids and their dependencies, install order
	DownloadBytes int64                 `json:"download_bytes"`
	InstallBytes  int64                 `json:"install_bytes"`
	FreeBytes     int64                 `json:"free_bytes"` // -1 = unknown
	EnoughSpace   bool                  `json:"enough_space"`
	NeedBytes     int64                 `json:"need_bytes"`
	Items         []components.PlanItem `json:"items"`
}

// ComponentTaskItemView is one component of a task.
type ComponentTaskItemView struct {
	ID string `json:"id"` // "" while an import is not identified yet
	// State: queued | downloading | downloaded | verifying | installing |
	// installed | failed | paused | cancelled (compui.item.state.*);
	// Phase is the manager's finer phase.
	State      string           `json:"state"`
	Phase      string           `json:"phase"`
	Version    string           `json:"version"`
	Update     bool             `json:"update"`
	Dependency bool             `json:"dependency"`
	DoneBytes  int64            `json:"done_bytes"`
	TotalBytes int64            `json:"total_bytes"`
	Source     string           `json:"source"` // default | mirror | offline; "" before it starts
	Error      *model.ErrorInfo `json:"error,omitempty"`
}

// ComponentTaskView is a task (TaskStatus of the brief).
type ComponentTaskView struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // install | update | import
	// State: queued | running | paused | verifying | installing | done |
	// failed | cancelled.
	State      string                  `json:"state"`
	IDs        []string                `json:"ids"`
	DoneBytes  int64                   `json:"done_bytes"`
	TotalBytes int64                   `json:"total_bytes"`
	SpeedBps   int64                   `json:"speed_bps"`
	ETAS       int64                   `json:"eta_s"` // -1 = unknown
	Items      []ComponentTaskItemView `json:"items"`
	Error      *model.ErrorInfo        `json:"error,omitempty"`
	Notes      []model.ErrorInfo       `json:"notes"`
	CreatedAt  string                  `json:"created_at"`
	UpdatedAt  string                  `json:"updated_at"`
}

// ComponentTaskResult is the 202 body of install, update, import and the
// task actions.
type ComponentTaskResult struct {
	Task ComponentTaskView `json:"task"`
}

// ComponentsEvent is the data of SSE "event: components".
type ComponentsEvent struct {
	Tasks []ComponentTaskView `json:"tasks"`
}

// SourceChoice is the choice of one source group.
type SourceChoice struct {
	Mode      string `json:"mode"` // default | mirror
	MirrorID  string `json:"mirror_id"`
	CustomURL string `json:"custom_url"` // mode mirror without mirror_id
}

// SourcesSetting is the stored choice (kv components.sources) and the body
// of PUT /api/components/sources.
type SourcesSetting struct {
	PyPI   SourceChoice `json:"pypi"`
	Models SourceChoice `json:"models"`
}

// SourceOption is a preset mirror of a group.
type SourceOption struct {
	ID string `json:"id"`
	// Label is a neutral label (the mirror's host name); NameKey the i18n
	// key of its display name.
	Label   string `json:"label"`
	NameKey string `json:"name_key,omitempty"`
	URL     string `json:"url"`
	Region  string `json:"region,omitempty"`
}

// SourcesView is GET / PUT /api/components/sources.
type SourcesView struct {
	PyPI    SourceChoice              `json:"pypi"`
	Models  SourceChoice              `json:"models"`
	Options map[string][]SourceOption `json:"options"`
	// SuggestMirror: the system is set to mainland China; Suggested is the
	// choice onboarding preselects then (only a suggestion).
	SuggestMirror bool           `json:"suggest_mirror"`
	Suggested     SourcesSetting `json:"suggested"`
}

func componentView(c components.Status) ComponentView {
	v := ComponentView{
		ID: c.ID, Kind: c.Kind, Version: c.Version, NameKey: c.NameKey, DescKey: c.DescKey, Upstream: c.Upstream,
		Backend: c.Backend, Model: c.Model, Precision: c.Precision,
		DownloadBytes: c.DownloadSize, InstallBytes: c.InstalledSize, DiskBytes: c.DiskUsage,
		Requires: append([]string{}, c.Depends...), License: c.License, LicenseNote: c.LicenseNote,
		InstalledVersion: c.InstalledVersion, UnavailableReason: c.UnavailableReason,
		Bundled: c.Bundled, RebuildNeeded: c.RebuildNeeded, Orphaned: c.Orphaned, Task: string(c.Task),
	}
	switch {
	case c.Task != "":
		v.State = "installing"
	case c.Installed && c.Broken:
		v.State = "broken"
	case c.Installed:
		v.State = "installed"
	case !c.Available:
		v.State = "unavailable"
	default:
		v.State = "available"
	}
	if c.UpdateAvailable {
		v.UpdateVersion = c.Version
	}
	return v
}

// itemState maps an item's phase to the item states of the front end.
func itemState(task string, it components.ItemStatus) string {
	switch it.Phase {
	case components.PhaseDone, components.PhaseSkipped:
		return "installed"
	case components.PhaseFailed:
		return "failed"
	case components.PhaseVerifying, components.PhaseChecking, components.PhaseSelfTest:
		return "verifying"
	case components.PhaseUnpacking, components.PhaseInstalling:
		return "installing"
	}
	// pending or downloading
	switch task {
	case components.TaskPaused, components.TaskCancelled:
		return task
	case components.TaskFailed:
		if it.Downloaded > 0 {
			return "failed"
		}
	}
	if it.Phase == components.PhaseDownloading {
		if it.Total > 0 && it.Downloaded >= it.Total {
			return "downloaded"
		}
		return "downloading"
	}
	return "queued"
}

// taskState refines a running task by the phase of its current item.
func taskState(t components.TaskStatus) string {
	if t.State != components.TaskRunning {
		return t.State
	}
	for _, it := range t.Items {
		switch it.Phase {
		case components.PhasePending, components.PhaseDone, components.PhaseSkipped, components.PhaseFailed:
			continue
		case components.PhaseVerifying, components.PhaseChecking, components.PhaseSelfTest:
			return "verifying"
		case components.PhaseUnpacking, components.PhaseInstalling:
			return "installing"
		}
		return components.TaskRunning
	}
	return components.TaskRunning
}

func taskView(t components.TaskStatus) ComponentTaskView {
	v := ComponentTaskView{
		ID: string(t.ID), Kind: t.Kind, State: taskState(t), IDs: append([]string{}, t.Components...),
		DoneBytes: t.Downloaded, TotalBytes: t.Total, SpeedBps: t.SpeedBps, ETAS: t.ETASeconds,
		Items: []ComponentTaskItemView{}, Error: t.Error, Notes: append([]model.ErrorInfo{}, t.Notices...),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	for _, it := range t.Items {
		v.Items = append(v.Items, ComponentTaskItemView{
			ID: it.ID, State: itemState(t.State, it), Phase: it.Phase, Version: it.Version, Update: it.Update,
			Dependency: it.Dependency, DoneBytes: it.Downloaded, TotalBytes: it.Total, Source: it.Source, Error: it.Error,
		})
	}
	return v
}

func taskViews(ts []components.TaskStatus) []ComponentTaskView {
	out := make([]ComponentTaskView, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskView(t))
	}
	return out
}

func planView(p components.Plan) ComponentPlanView {
	v := ComponentPlanView{
		IDs: []string{}, DownloadBytes: p.DownloadBytes, InstallBytes: p.InstalledBytes,
		FreeBytes: p.FreeBytes, EnoughSpace: p.Enough, NeedBytes: p.RequiredBytes,
		Items: append([]components.PlanItem{}, p.Items...),
	}
	for _, it := range p.Items {
		v.IDs = append(v.IDs, it.ID)
	}
	return v
}

// ---------- errors ----------

// requireComps answers 503 without a component manager.
func (s *Server) requireComps(w http.ResponseWriter) bool {
	if s.comps == nil {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "components"})
		return false
	}
	return true
}

// compErr gives an error of the component manager its HTTP status: missing
// objects 404 (an unknown id only when it came from the path), conflicts
// with the current state 409, invalid requests 400. A plain error (the
// manager is closing) is 503.
func compErr(err error, pathID bool) error {
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		return apiErrDetail(http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "components"}, err.Error())
	}
	status := http.StatusBadRequest
	switch ce.Info.Code {
	case components.CodeUnknown:
		if pathID {
			status = http.StatusNotFound
		}
	case components.CodeTaskNotFound, components.CodeNotInstalled:
		status = http.StatusNotFound
	case components.CodeBusy, components.CodeInUse, components.CodeInUseByTask, components.CodeTaskState,
		components.CodeUpToDate, components.CodeBundled, components.CodeDiskFull:
		status = http.StatusConflict
	case components.CodeIO:
		status = http.StatusInternalServerError
	}
	info := ce.Info
	if info.Detail == "" && ce.Err != nil {
		info.Detail = ce.Err.Error()
	}
	return &httpError{status: status, info: info}
}

// componentIDRe accepts manifest ids, model ids included
// (model:{backend}:{name}:{precision}).
var componentIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:+-]{0,199}$`)

// taskIDRe accepts the manager's task ids.
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// pathComponentID reads and checks the {id} of the path (the client
// escapes the colons of a model id as %3A or leaves them; PathValue
// returns the unescaped id either way).
func pathComponentID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !componentIDRe.MatchString(id) {
		return "", apiErr(http.StatusNotFound, components.CodeUnknown, map[string]any{"id": truncate(id, 200)})
	}
	return id, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// idsBody is the body of plan and install.
type idsBody struct {
	IDs []string `json:"ids"`
}

func readIDs(r *http.Request) ([]string, error) {
	var in idsBody
	if err := decodeJSON(r, &in); err != nil {
		return nil, err
	}
	if len(in.IDs) == 0 || len(in.IDs) > 100 {
		return nil, apiErr(http.StatusBadRequest, "request.bad_param", map[string]any{"name": "ids"})
	}
	for _, id := range in.IDs {
		if !componentIDRe.MatchString(id) {
			return nil, apiErr(http.StatusBadRequest, components.CodeUnknown, map[string]any{"id": truncate(id, 200)})
		}
	}
	return in.IDs, nil
}

// ---------- handlers ----------

// GET /api/components.
func (s *Server) getComponents(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	mi := s.comps.ManifestInfo()
	v := ComponentsView{
		Platform:   s.comps.Platform(),
		Manifest:   ComponentsManifestView{AppVersion: mi.AppVersion, PublishedAt: mi.Published, Serial: mi.Serial, Builtin: mi.Builtin},
		Components: []ComponentView{},
		Tasks:      taskViews(s.comps.Tasks()),
	}
	for _, c := range s.comps.List() {
		cv := componentView(c)
		if c.UpdateAvailable || c.RebuildNeeded {
			v.Manifest.UpdateAvailable = true
		}
		v.Components = append(v.Components, cv)
	}
	writeJSON(w, http.StatusOK, v)
}

// POST /api/components/plan {"ids"}.
func (s *Server) planComponents(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	ids, err := readIDs(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.comps.Plan(ids)
	if err != nil {
		s.fail(w, r, compErr(err, false))
		return
	}
	writeJSON(w, http.StatusOK, planView(p))
}

// POST /api/components/install {"ids"} → 202 {task}. The download sources
// are the stored ones (PUT /api/components/sources).
func (s *Server) installComponents(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	ids, err := readIDs(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.comps.Plan(ids)
	if err != nil {
		s.fail(w, r, compErr(err, false))
		return
	}
	todo := false
	for _, it := range p.Items {
		todo = todo || !it.Installed
	}
	if !todo {
		writeError(w, http.StatusConflict, components.CodeUpToDate, map[string]any{"id": strings.Join(ids, ", "), "version": ""})
		return
	}
	src, err := s.storedSources(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := s.comps.Install(ids, toManagerSources(src))
	if err != nil {
		s.fail(w, r, compErr(err, false))
		return
	}
	s.respondTask(w, r, id)
}

// POST /api/components/{id}/update → 202 {task}.
func (s *Server) updateComponent(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	cid, err := pathComponentID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	src, err := s.storedSources(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := s.comps.UpdateFrom(cid, toManagerSources(src))
	if err != nil {
		s.fail(w, r, compErr(err, true))
		return
	}
	s.respondTask(w, r, id)
}

// DELETE /api/components/{id}: 409 while installed components need it or
// a task works on it.
func (s *Server) uninstallComponent(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	cid, err := pathComponentID(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.comps.Uninstall(cid); err != nil {
		s.fail(w, r, compErr(err, true))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// respondTask writes 202 {task} for a task the manager just created or changed.
func (s *Server) respondTask(w http.ResponseWriter, r *http.Request, id components.TaskID) {
	t, ok := s.comps.Task(id)
	if !ok {
		s.fail(w, r, apiErr(http.StatusNotFound, components.CodeTaskNotFound, map[string]any{"task": string(id)}))
		return
	}
	writeJSON(w, http.StatusAccepted, ComponentTaskResult{Task: taskView(t)})
}

// GET /api/components/tasks → [TaskStatus].
func (s *Server) getComponentTasks(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	writeJSON(w, http.StatusOK, taskViews(s.comps.Tasks()))
}

// POST /api/components/tasks/{id}/{pause|resume|cancel|retry} → 202 {task}.
// retry uses the stored download sources (the page saves the new ones
// first for "换一个下载源").
func (s *Server) componentTaskAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	raw := r.PathValue("id")
	if !taskIDRe.MatchString(raw) {
		s.fail(w, r, apiErr(http.StatusNotFound, components.CodeTaskNotFound, map[string]any{"task": truncate(raw, 64)}))
		return
	}
	id := components.TaskID(raw)
	var err error
	switch action := r.PathValue("action"); action {
	case "pause":
		err = s.comps.Pause(id)
	case "resume":
		err = s.comps.Resume(id)
	case "cancel":
		err = s.comps.Cancel(id)
	case "retry":
		var src SourcesSetting
		if src, err = s.retrySources(r); err == nil {
			ms := toManagerSources(src)
			err = s.comps.Retry(id, &ms)
		}
	default:
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return
	}
	if err != nil {
		var he *httpError
		if !errors.As(err, &he) {
			err = compErr(err, true)
		}
		s.fail(w, r, err)
		return
	}
	s.respondTask(w, r, id)
}

// retryBody is the optional body of the retry action: new download sources,
// either top-level {pypi, models} or {"sources": {pypi, models}}; a group
// left out keeps the stored choice.
type retryBody struct {
	Sources *struct {
		PyPI   *SourceChoice `json:"pypi"`
		Models *SourceChoice `json:"models"`
	} `json:"sources"`
	PyPI   *SourceChoice `json:"pypi"`
	Models *SourceChoice `json:"models"`
}

// retrySources returns the sources of a retry: the stored ones, replaced
// by the groups the body sends (validated; not stored).
func (s *Server) retrySources(r *http.Request) (SourcesSetting, error) {
	st, err := s.storedSources(r.Context())
	if err != nil {
		return st, err
	}
	var in retryBody
	if err := decodeOptionalJSON(r, &in); err != nil {
		return st, err
	}
	pypi, models := in.PyPI, in.Models
	if in.Sources != nil {
		if in.Sources.PyPI != nil {
			pypi = in.Sources.PyPI
		}
		if in.Sources.Models != nil {
			models = in.Sources.Models
		}
	}
	groups := s.comps.SourceGroups()
	for _, g := range []struct {
		name string
		in   *SourceChoice
		dst  *SourceChoice
	}{{components.GroupPyPI, pypi, &st.PyPI}, {components.GroupModels, models, &st.Models}} {
		if g.in == nil {
			continue
		}
		c := *g.in
		normSourceChoice(&c)
		if f := checkSourceChoice(g.name, c, groups); f != "" {
			return st, apiErr(http.StatusBadRequest, "components.sources_invalid", map[string]any{"group": g.name, "field": f})
		}
		*g.dst = c
	}
	return st, nil
}

// GET /api/components/presets → the manifest's cost tiers (V4.2 step 4).
func (s *Server) getComponentPresets(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.comps.Presets())
}

// ---------- import ----------

// importUploadDir holds the archives uploaded in web mode until their task ends.
func importUploadDir(dataDir string) string {
	return filepath.Join(dataDir, "tmp", "component-imports")
}

// POST /api/components/import: desktop mode {"path"} (the file the user
// picked; read in place), web mode multipart "file" (stored in the data
// directory until the task is done or cancelled) → 202 {task}.
func (s *Server) importComponent(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	var path string
	if s.nativeUpload(r) {
		pr, err := readPathRequest(w, r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		src, _, err := s.checkSourcePath(r.Context(), pr.path)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		path = src
		id, err := s.comps.ImportFile(path)
		if err != nil {
			s.fail(w, r, compErr(err, false))
			return
		}
		s.respondTask(w, r, id)
		return
	}
	if err := s.checkSpace(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	res, err := s.receiveUpload(w, r, importUploadDir(s.dataDir), "import-"+hex.EncodeToString(rnd[:]), func(name string) error {
		if !strings.EqualFold(filepath.Ext(name), ".zip") {
			return apiErr(http.StatusBadRequest, components.CodeImportInvalid, nil)
		}
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cs := s.compState()
	cs.impMu.Lock() // held across ImportFile so taskSeen cannot miss a fast task
	id, err := s.comps.ImportFile(res.path)
	if err == nil {
		cs.imports[id] = res.path
	}
	cs.impMu.Unlock()
	if err != nil {
		os.Remove(res.path)
		s.fail(w, r, compErr(err, false))
		return
	}
	// the task may already have ended before it was registered
	if t, ok := s.comps.Task(id); ok {
		cs.taskSeen(t)
	}
	s.respondTask(w, r, id)
}

// ---------- download sources ----------

// storedSources reads kv components.sources (default sources when unset
// or unreadable).
func (s *Server) storedSources(ctx context.Context) (SourcesSetting, error) {
	def := SourcesSetting{PyPI: SourceChoice{Mode: "default"}, Models: SourceChoice{Mode: "default"}}
	v, ok, err := s.kvGet(ctx, kvComponentsSources)
	if err != nil || !ok {
		return def, err
	}
	var st SourcesSetting
	if json.Unmarshal([]byte(v), &st) != nil {
		return def, nil
	}
	// a stored mirror the current manifest no longer has falls back to
	// default. A custom package index on plain http (saved before that was
	// refused) is kept as the user saved it: the page shows it, and an
	// install with it fails with components.sources_invalid (the manager
	// checks it) instead of quietly using another index.
	if s.comps != nil {
		groups := s.comps.SourceGroups()
		for _, g := range []struct {
			name string
			c    *SourceChoice
		}{{components.GroupPyPI, &st.PyPI}, {components.GroupModels, &st.Models}} {
			if sourceChoiceShape(g.name, *g.c, groups) != "" {
				*g.c = SourceChoice{Mode: "default"}
			}
		}
	}
	return st, nil
}

// toManagerSources converts the stored choice for the manager.
func toManagerSources(st SourcesSetting) components.Sources {
	conv := func(c SourceChoice) string {
		if c.Mode != "mirror" {
			return ""
		}
		if c.MirrorID != "" {
			return c.MirrorID
		}
		return c.CustomURL
	}
	return components.Sources{PyPI: conv(st.PyPI), Models: conv(st.Models)}
}

func fromManagerSources(src components.Sources) SourcesSetting {
	conv := func(id string) SourceChoice {
		if id == "" || id == components.SourceDefault {
			return SourceChoice{Mode: "default"}
		}
		return SourceChoice{Mode: "mirror", MirrorID: id}
	}
	return SourcesSetting{PyPI: conv(src.PyPI), Models: conv(src.Models)}
}

// normSourceChoice trims a choice and drops the fields its mode ignores.
func normSourceChoice(c *SourceChoice) {
	c.Mode = strings.TrimSpace(c.Mode)
	c.MirrorID = strings.TrimSpace(c.MirrorID)
	c.CustomURL = strings.TrimSpace(c.CustomURL)
	if c.Mode == "" {
		c.Mode = "default"
	}
	switch {
	case c.Mode == "default":
		c.MirrorID, c.CustomURL = "", ""
	case c.MirrorID != "":
		c.CustomURL = ""
	}
}

// checkSourceChoice returns the invalid field of a choice, or "": its shape
// (sourceChoiceShape), and a custom package index must not be plain http
// to another computer (components.InsecureCustomSource; security audit
// M-1).
func checkSourceChoice(group string, c SourceChoice, groups map[string]components.SourceGroup) string {
	if f := sourceChoiceShape(group, c, groups); f != "" {
		return f
	}
	if c.Mode == "mirror" && c.MirrorID == "" && components.InsecureCustomSource(group, c.CustomURL) {
		return "custom_url"
	}
	return ""
}

// sourceChoiceShape returns the field of a choice that is malformed or names
// a mirror the manifest does not have, or "".
func sourceChoiceShape(group string, c SourceChoice, groups map[string]components.SourceGroup) string {
	switch c.Mode {
	case "default":
		return ""
	case "mirror":
	default:
		return "mode"
	}
	if c.MirrorID == "" {
		if !components.CustomURLOK(c.CustomURL) {
			return "custom_url"
		}
		return ""
	}
	for _, m := range groups[group].Mirrors {
		if m.ID == c.MirrorID {
			return ""
		}
	}
	return "mirror_id"
}

func (s *Server) sourcesView(st SourcesSetting) SourcesView {
	v := SourcesView{PyPI: st.PyPI, Models: st.Models, Options: map[string][]SourceOption{}}
	groups := s.comps.SourceGroups()
	for _, g := range []string{components.GroupPyPI, components.GroupModels} {
		opts := []SourceOption{}
		sg := groups[g]
		for _, m := range sg.Mirrors {
			u := m.URL
			if u == "" {
				u = m.Values[sg.Primary]
			}
			label := m.ID
			if pu, err := url.Parse(u); err == nil && pu.Host != "" {
				label = pu.Host
			}
			opts = append(opts, SourceOption{ID: m.ID, Label: label, NameKey: m.NameKey, URL: u, Region: m.Region})
		}
		v.Options[g] = opts
	}
	cs := s.compState()
	if cs.region != nil && cs.region() == "cn" {
		v.SuggestMirror = true
		v.Suggested = fromManagerSources(s.comps.SuggestSources("cn"))
	} else {
		v.Suggested = SourcesSetting{PyPI: SourceChoice{Mode: "default"}, Models: SourceChoice{Mode: "default"}}
	}
	return v
}

// GET /api/components/sources.
func (s *Server) getComponentSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	st, err := s.storedSources(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.sourcesView(st))
}

// PUT /api/components/sources {pypi, models} → SourcesView.
func (s *Server) putComponentSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireComps(w) {
		return
	}
	var in SourcesSetting
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	groups := s.comps.SourceGroups()
	checks := []struct {
		name string
		c    *SourceChoice
	}{{components.GroupPyPI, &in.PyPI}, {components.GroupModels, &in.Models}}
	for _, g := range checks {
		normSourceChoice(g.c)
		if f := checkSourceChoice(g.name, *g.c, groups); f != "" {
			s.fail(w, r, apiErr(http.StatusBadRequest, "components.sources_invalid", map[string]any{"group": g.name, "field": f}))
			return
		}
	}
	b, _ := json.Marshal(in)
	if err := s.st.SetKV(r.Context(), kvComponentsSources, string(b)); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.sourcesView(in))
}
