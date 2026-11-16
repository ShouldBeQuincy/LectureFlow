// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lectureflow/internal/model"
)

// TaskID identifies a background task.
type TaskID string

// Task states.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskPaused    = "paused"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

// Task kinds.
const (
	TaskInstall = "install"
	TaskUpdate  = "update"
	TaskImport  = "import"
)

// Item phases (ItemStatus.Phase).
const (
	PhasePending     = "pending"
	PhaseDownloading = "downloading"
	PhaseVerifying   = "verifying" // hashing an imported archive
	PhaseUnpacking   = "unpacking"
	PhaseInstalling  = "installing" // uv creates and fills the environment
	PhaseChecking    = "checking"   // the post-install import check
	PhaseSelfTest    = "selftest"   // the update's self-test (V8)
	PhaseDone        = "done"
	PhaseSkipped     = "skipped" // already installed
	PhaseFailed      = "failed"
)

// TaskStatus is the state of a task (Tasks, events).
type TaskStatus struct {
	ID    TaskID `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Components are the requested ids (Items add their dependencies).
	Components []string     `json:"components"`
	Items      []ItemStatus `json:"items"`
	// Downloaded / Total are bytes over all items; SpeedBps the transfer rate
	// over the last seconds; ETASeconds -1 when unknown.
	Downloaded int64             `json:"downloaded"`
	Total      int64             `json:"total"`
	SpeedBps   int64             `json:"speed_bps"`
	ETASeconds int64             `json:"eta_seconds"`
	Error      *model.ErrorInfo  `json:"error,omitempty"`
	Notices    []model.ErrorInfo `json:"notices,omitempty"`
	Sources    Sources           `json:"sources"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}

// ItemStatus is one component of a task.
type ItemStatus struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Update     bool   `json:"update"`
	Dependency bool   `json:"dependency"`
	Phase      string `json:"phase"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	// Source is where the item comes from: "default", "mirror" (the chosen
	// mirror or custom URL), "offline" (ImportFile); "" before it starts.
	Source string           `json:"source,omitempty"`
	Error  *model.ErrorInfo `json:"error,omitempty"`
}

// Item sources (ItemStatus.Source).
const (
	SourceMirror  = "mirror"
	SourceOffline = "offline"
)

type task struct {
	id        TaskID
	kind      string
	requested []string
	src       Sources
	man       *Manifest
	created   time.Time
	updated   time.Time
	state     string
	err       *model.ErrorInfo
	notices   []model.ErrorInfo
	items     []*item
	cancel    context.CancelCauseFunc // set while running

	importPath string // TaskImport: the archive

	// transfer rate: network bytes over time
	net      int64
	samples  []rateSample
	lastEmit time.Time
}

type rateSample struct {
	at  time.Time
	net int64
}

type item struct {
	id         string
	version    string
	update     bool // replaces the active version: self-test, keep the old one on failure
	dependency bool
	phase      string
	total      int64
	doneBytes  int64 // finished files
	curBytes   int64 // the file in progress
	staging    string
	err        *model.ErrorInfo
	offline    *offlineSource // TaskImport
	source     string         // ItemStatus.Source
	// pythonRel: the python runtime's version directory once the runtime
	// is unpacked there (resume after a pause).
	pythonRel string
}

func (it *item) downloaded() int64 { return it.doneBytes + it.curBytes }

func (t *task) active() bool {
	switch t.state {
	case TaskQueued, TaskRunning, TaskPaused:
		return true
	}
	return false
}

// status snapshots the task; the caller holds m.mu.
func (t *task) status(now time.Time) TaskStatus {
	s := TaskStatus{
		ID: t.id, Kind: t.kind, State: t.state,
		Components: append([]string(nil), t.requested...),
		Sources:    t.src,
		CreatedAt:  t.created.UTC().Format(time.RFC3339),
		UpdatedAt:  t.updated.UTC().Format(time.RFC3339),
		ETASeconds: -1,
	}
	if t.err != nil {
		e := *t.err
		s.Error = &e
	}
	s.Notices = append([]model.ErrorInfo(nil), t.notices...)
	for _, it := range t.items {
		is := ItemStatus{ID: it.id, Version: it.version, Update: it.update, Dependency: it.dependency,
			Phase: it.phase, Downloaded: it.downloaded(), Total: it.total, Source: it.source}
		if it.err != nil {
			e := *it.err
			is.Error = &e
		}
		if is.Downloaded > is.Total {
			is.Downloaded = is.Total
		}
		s.Items = append(s.Items, is)
		s.Downloaded += is.Downloaded
		s.Total += is.Total
	}
	if t.state == TaskRunning {
		s.SpeedBps = t.speed(now)
		if s.SpeedBps > 0 {
			s.ETASeconds = (s.Total - s.Downloaded + s.SpeedBps - 1) / s.SpeedBps
		}
	}
	if t.state == TaskDone {
		s.ETASeconds = 0
	}
	return s
}

const rateWindow = 5 * time.Second

func (t *task) addNet(now time.Time, delta int64) {
	t.net += delta
	t.samples = append(t.samples, rateSample{now, t.net})
	cut := 0
	for cut < len(t.samples)-1 && now.Sub(t.samples[cut].at) > rateWindow {
		cut++
	}
	t.samples = t.samples[cut:]
}

func (t *task) speed(now time.Time) int64 {
	if len(t.samples) < 2 {
		return 0
	}
	first, last := t.samples[0], t.samples[len(t.samples)-1]
	if now.Sub(last.at) > rateWindow { // stalled
		return 0
	}
	dt := last.at.Sub(first.at).Seconds()
	if dt < 0.2 {
		return 0
	}
	return int64(float64(last.net-first.net) / dt)
}

// Event is published to subscribers (SSE in wave 2).
type Event struct {
	// Type: "task" (Task set), "components" (the installed set or a
	// component's state changed: call List), "manifest" (a newer manifest
	// was adopted).
	Type string      `json:"type"`
	Task *TaskStatus `json:"task,omitempty"`
}

// Event types.
const (
	EventTask       = "task"
	EventComponents = "components"
	EventManifest   = "manifest"
)

// subscriber delivers events in order on its own goroutine; progress
// events of the same task coalesce when the callback is slow.
type subscriber struct {
	fn      func(Event)
	mu      sync.Mutex
	pending map[string]Event
	order   []string
	signal  chan struct{}
	done    chan struct{}
}

func newSubscriber(fn func(Event)) *subscriber {
	s := &subscriber{fn: fn, pending: map[string]Event{}, signal: make(chan struct{}, 1), done: make(chan struct{})}
	go s.loop()
	return s
}

func (s *subscriber) publish(key string, ev Event) {
	s.mu.Lock()
	if _, ok := s.pending[key]; !ok {
		s.order = append(s.order, key)
	}
	s.pending[key] = ev
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *subscriber) loop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.signal:
		}
		s.mu.Lock()
		order, pending := s.order, s.pending
		s.order, s.pending = nil, map[string]Event{}
		s.mu.Unlock()
		for _, k := range order {
			select {
			case <-s.done:
				return
			default:
			}
			s.fn(pending[k])
		}
	}
}

// emit publishes an event to every subscriber; the caller holds m.mu.
func (m *Manager) emit(key string, ev Event) {
	for _, s := range m.subs {
		s.publish(key, ev)
	}
}

// emitTask publishes the task's status; progress (force = false) at most
// every EventInterval. The caller holds m.mu.
func (m *Manager) emitTask(t *task, force bool) {
	now := m.now()
	t.updated = now
	if !force && now.Sub(t.lastEmit) < m.eventInterval {
		return
	}
	t.lastEmit = now
	st := t.status(now)
	m.emit("task:"+string(t.id), Event{Type: EventTask, Task: &st})
}

func (m *Manager) emitComponents() {
	m.emit(EventComponents, Event{Type: EventComponents})
}

// ---------------------------------------------------------------------------
// Runner: one task at a time, in creation order.
// ---------------------------------------------------------------------------

func (m *Manager) wakeRunner() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) runner() {
	defer close(m.runnerDone)
	for {
		m.mu.Lock()
		var t *task
		for _, id := range m.order {
			if x := m.tasks[id]; x.state == TaskQueued {
				t = x
				break
			}
		}
		closed := m.closed
		var ctx context.Context
		if t != nil && !closed {
			var cancel context.CancelCauseFunc
			ctx, cancel = context.WithCancelCause(context.Background())
			t.cancel = cancel
			t.state = TaskRunning
			t.err = nil
			m.emitTask(t, true)
		}
		m.mu.Unlock()
		if closed {
			return
		}
		if t == nil {
			select {
			case <-m.wake:
			case <-m.stopCh:
				return
			}
			continue
		}
		m.runTask(ctx, t)
	}
}

// runTask runs the items in order; a pause or cancel stops at the current
// step.
func (m *Manager) runTask(ctx context.Context, t *task) {
	var failed error
	var failedItem *item
	for _, it := range t.items {
		if it.phase == PhaseDone || it.phase == PhaseSkipped {
			continue
		}
		if err := m.runItem(ctx, t, it); err != nil {
			failed, failedItem = err, it
			break
		}
	}
	cause := context.Cause(ctx)
	final := TaskDone
	var info *model.ErrorInfo
	switch {
	case failed == nil: // every item finished (a late pause or cancel changes nothing)
	case errors.Is(cause, errPaused) || errors.Is(cause, errClosing):
		final = TaskPaused
	case errors.Is(cause, errCancelled):
		final = TaskCancelled
		info = &model.ErrorInfo{Code: CodeCancelled}
	default:
		final = TaskFailed
		i := m.withDiskFree(ErrorInfoOf(failed))
		info = &i
		m.log.Warn("components: task failed", "task", t.id, "code", i.Code, "error", redactText(failed.Error()))
	}
	// clean up before the final state is visible: a finished task leaves
	// nothing behind
	switch final {
	case TaskCancelled:
		m.cleanupTask(t, true)
	case TaskFailed, TaskDone:
		m.cleanupTask(t, false)
	}
	m.mu.Lock()
	t.cancel = nil
	t.state = final
	t.err = info
	if final == TaskFailed && failedItem != nil {
		failedItem.phase = PhaseFailed
		failedItem.err = info
	}
	m.emitTask(t, true)
	if final != TaskPaused {
		m.emitComponents()
	}
	drop := m.gcRetained()
	m.trimFinished()
	m.mu.Unlock()
	m.removeDirs(drop)
}

// withDiskFree completes a disk_full error that happened while writing
// (not in the plan's check) with the free space now.
func (m *Manager) withDiskFree(info model.ErrorInfo) model.ErrorInfo {
	if info.Code != CodeDiskFull {
		return info
	}
	params := map[string]any{"required_mb": int64(-1), "free_mb": int64(-1)}
	for k, v := range info.Params {
		params[k] = v
	}
	if fmt.Sprint(params["free_mb"]) == "-1" {
		if free, err := m.diskFree(m.root); err == nil {
			params["free_mb"] = mb(free)
		}
	}
	info.Params = params
	return info
}

// cleanupTask removes the task's staging directories; with downloads also its
// downloaded and partial files that no other active task needs (cancel:
// nothing of the task stays on disk). The uv cache goes when no task is
// active.
func (m *Manager) cleanupTask(t *task, downloads bool) {
	m.mu.Lock()
	var stagings []string
	for _, it := range t.items {
		if it.staging != "" {
			stagings = append(stagings, it.staging)
			it.staging = ""
		}
	}
	needed := map[string]bool{}
	anyActive := false
	for _, o := range m.tasks {
		if o == t || !o.active() {
			continue
		}
		anyActive = true
		for _, it := range o.items {
			for _, f := range m.itemFiles(o.man, it.id) {
				needed[f.SHA256] = true
			}
		}
	}
	var shas []string
	if downloads {
		for _, it := range t.items {
			if it.phase == PhaseDone || it.phase == PhaseSkipped {
				continue
			}
			for _, f := range m.itemFiles(t.man, it.id) {
				if !needed[f.SHA256] {
					shas = append(shas, f.SHA256)
				}
			}
		}
	}
	m.mu.Unlock()
	for _, s := range stagings {
		if err := removeAllRetry(s); err != nil {
			m.log.Warn("components: cannot remove a staging directory", "dir", s, "error", err)
		}
		removeIfEmpty(filepath.Dir(s))
	}
	for _, s := range shas {
		m.dl.remove(s)
	}
	if downloads {
		removeIfEmpty(m.dl.dir)
	}
	if !anyActive {
		removeAllRetry(m.uvCache())
	}
	if t.kind == TaskImport {
		m.removeImportCopy(t) // a retry copies the user's file again
	}
}

func removeIfEmpty(dir string) {
	if es, err := os.ReadDir(dir); err == nil && len(es) == 0 {
		os.Remove(dir)
	}
}

// itemFiles are the downloadable files of a component on this platform.
func (m *Manager) itemFiles(man *Manifest, id string) []File {
	c := man.Component(id)
	b := c.Build(m.platform)
	if b == nil {
		return nil
	}
	return b.Files
}

// trimFinished keeps the last finished tasks only. Caller holds m.mu.
func (m *Manager) trimFinished() {
	const keep = 20
	var finished []TaskID
	for _, id := range m.order {
		if !m.tasks[id].active() {
			finished = append(finished, id)
		}
	}
	if len(finished) <= keep {
		return
	}
	drop := map[TaskID]bool{}
	for _, id := range finished[:len(finished)-keep] {
		drop[id] = true
		delete(m.tasks, id)
	}
	order := m.order[:0]
	for _, id := range m.order {
		if !drop[id] {
			order = append(order, id)
		}
	}
	m.order = order
}

// newTask registers a queued task. Caller holds m.mu.
func (m *Manager) newTask(kind string, requested []string, src Sources, man *Manifest, items []*item) *task {
	m.seq++
	now := m.now()
	t := &task{
		id:        TaskID(fmt.Sprintf("t%d-%d", now.UnixMilli(), m.seq)),
		kind:      kind,
		requested: append([]string(nil), requested...),
		src:       src,
		man:       man,
		created:   now,
		updated:   now,
		state:     TaskQueued,
		items:     items,
	}
	m.tasks[t.id] = t
	m.order = append(m.order, t.id)
	m.emitTask(t, true)
	m.wakeRunner()
	return t
}

// setSource records where an item's data comes from.
func (m *Manager) setSource(t *task, it *item, u sourceURL) {
	m.mu.Lock()
	it.source = SourceDefault
	if u.Mirror != "" {
		it.source = SourceMirror
	}
	m.emitTask(t, true)
	m.mu.Unlock()
}

// setPhase updates an item's phase and publishes it.
func (m *Manager) setPhase(t *task, it *item, phase string) {
	m.mu.Lock()
	it.phase = phase
	m.emitTask(t, true)
	m.mu.Unlock()
}

// progress returns the onBytes callback of one file of an item: network
// bytes feed the rate, the first report (an existing partial file) does not.
func (m *Manager) progress(t *task, it *item) func(int64) {
	first := true
	var last int64
	return func(n int64) {
		m.mu.Lock()
		defer m.mu.Unlock()
		now := m.now()
		if !first && n > last {
			t.addNet(now, n-last)
		}
		first = false
		last = n
		it.curBytes = n
		m.emitTask(t, false)
	}
}

// fileDone moves a finished file's bytes into the item's total.
func (m *Manager) fileDone(t *task, it *item, size int64) {
	m.mu.Lock()
	it.curBytes = 0
	it.doneBytes += size
	m.emitTask(t, false)
	m.mu.Unlock()
}

// notice records a notice on the task (mirror fallback, skipped self-test).
func (m *Manager) notice(t *task, info model.ErrorInfo) {
	m.mu.Lock()
	for _, n := range t.notices {
		if n.Code == info.Code && fmt.Sprint(n.Params) == fmt.Sprint(info.Params) {
			m.mu.Unlock()
			return
		}
	}
	t.notices = append(t.notices, info)
	m.emitTask(t, true)
	m.mu.Unlock()
}
