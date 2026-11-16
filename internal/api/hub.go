// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/resources"
)

// Hub fans scheduler and API events out to SSE clients (SPEC §8.2).
// It implements sched.Events: Job and Changed never block.
//
// Each client keeps only the latest event per job and a set of pending
// "changed" notices, so a slow client loses intermediate progress but never
// a final state. Progress-only updates of one job go out at most once per
// MinInterval; a status change goes out immediately.
type Hub struct {
	// MinInterval is the per-job throttle for progress events (default 500ms).
	MinInterval time.Duration
	// PingInterval is the keep-alive comment interval (default 15s).
	PingInterval time.Duration
	// WriteTimeout drops a client whose connection does not accept a write
	// within this time (default 30s); the browser reconnects and refreshes.
	WriteTimeout time.Duration

	mu      sync.Mutex
	clients map[*sseClient]struct{}
	closed  chan struct{}
	once    sync.Once

	// hookMu serialises the client-count hook so the last call always
	// reports the current count (v1.1 A10: sampling stops without clients).
	hookMu    sync.Mutex
	onClients func(n int)
	jobHook   func(model.Job) // set by the server; must not block
	// jobStats (v3.2.x P1-c) adds the run's statistics to a job event; set
	// by the server (Server.jobEventStats), must not block.
	jobStats func(*JobEvent, model.Job)
}

// setJobStats registers fn (see Hub.jobStats).
func (h *Hub) setJobStats(fn func(*JobEvent, model.Job)) {
	h.mu.Lock()
	h.jobStats = fn
	h.mu.Unlock()
}

// setClientsHook registers fn, called with the number of visible SSE
// clients (VisibleClients) after every connect, disconnect and visibility
// change (resources.Service.SetClients), and once right away.
func (h *Hub) setClientsHook(fn func(n int)) {
	h.hookMu.Lock()
	h.onClients = fn
	h.hookMu.Unlock()
	h.notifyClients()
}

// notifyClients reports the current visible client count to the hook. The
// count is read under hookMu, so concurrent connects/disconnects cannot
// report an older count last.
func (h *Hub) notifyClients() {
	h.hookMu.Lock()
	defer h.hookMu.Unlock()
	if h.onClients != nil {
		h.onClients(h.VisibleClients())
	}
}

// setJobHook registers fn, called (synchronously, so it must not block)
// with every job passed to Job.
func (h *Hub) setJobHook(fn func(model.Job)) {
	h.mu.Lock()
	h.jobHook = fn
	h.mu.Unlock()
}

// Resources broadcasts a resource snapshot as SSE "event: resources"
// (v1.1 A10). Wire resources.Deps.OnSample to it. Each client keeps only
// the latest snapshot. Never blocks.
func (h *Hub) Resources(snap resources.Snapshot) {
	h.each(func(c *sseClient) {
		c.mu.Lock()
		s := snap
		c.res = &s
		c.mu.Unlock()
	})
}

// Components broadcasts the component tasks as SSE "event: components"
// (SPEC-v3 V3, .claude/v3-brief.md §7.2). The caller throttles (at most once
// a second); each client keeps only the latest payload. Never blocks.
func (h *Hub) Components(data any) {
	h.each(func(c *sseClient) {
		c.mu.Lock()
		c.comps = data
		c.hasComps = true
		c.mu.Unlock()
	})
}

// Close ends every open event stream (graceful shutdown: http.Server.Shutdown
// does not cancel running handlers). Later connections end immediately.
func (h *Hub) Close() {
	h.init()
	h.once.Do(func() { close(h.closed) })
}

func (h *Hub) init() {
	h.mu.Lock()
	if h.closed == nil {
		h.closed = make(chan struct{})
	}
	if h.clients == nil {
		h.clients = map[*sseClient]struct{}{}
	}
	h.mu.Unlock()
}

// NewHub returns a hub with the SPEC defaults. Create it before the
// scheduler and pass it to both sched.Options.Events and api.Deps.Hub.
func NewHub() *Hub {
	return &Hub{
		MinInterval:  500 * time.Millisecond,
		PingInterval: 15 * time.Second,
		WriteTimeout: 30 * time.Second,
		clients:      map[*sseClient]struct{}{},
		closed:       make(chan struct{}),
	}
}

type changedKey struct{ kind, id string }

type sseClient struct {
	wake chan struct{}

	mu       sync.Mutex
	jobs     map[string]JobEvent // latest unsent event per job id
	jobOrder []string
	changed  map[changedKey]bool
	chOrder  []changedKey
	lastSent map[string]sentInfo // per job id
	res      *resources.Snapshot // latest unsent resources snapshot
	comps    any                 // latest unsent components payload (hasComps)
	hasComps bool

	// perf-fix (visibility.go): id is the front end's client id (?client=,
	// "" = none), visible whether its page is shown (?visible=0 = hidden;
	// POST /api/events/visibility changes it) and visSeq the sequence
	// number of the report in effect. Guarded by Hub.mu.
	id      string
	visible bool
	visSeq  int64
}

type sentInfo struct {
	at     time.Time
	status string
}

// Job implements sched.Events.
func (h *Hub) Job(j model.Job) {
	ev := JobEvent{Scope: j.Scope, ScopeID: j.ScopeID, Stage: j.Stage, Status: j.Status, Progress: j.Progress}
	h.mu.Lock()
	hook, decorate := h.jobHook, h.jobStats
	h.mu.Unlock()
	if decorate != nil {
		// v3.2.x P1-c: stats, llm and last_activity_at of a running job.
		// The event replaces the client's unsent one of the job like any
		// other (latest wins), so an event that changes only the stats is
		// neither dropped nor sent sooner than MinInterval after the last.
		decorate(&ev, j)
	}
	if hook != nil {
		hook(j)
	}
	h.each(func(c *sseClient) {
		c.mu.Lock()
		if _, ok := c.jobs[j.ID]; !ok {
			c.jobOrder = append(c.jobOrder, j.ID)
		}
		c.jobs[j.ID] = ev
		c.mu.Unlock()
	})
}

// Changed implements sched.Events. kind: lecture|course|sentences|patches|terms|system|settings|devices
// (SPEC-v2 I8-I10: a device row, its pause switch, a self-test or the power
// state changed; id "").
func (h *Hub) Changed(kind, id string) {
	k := changedKey{kind, id}
	h.each(func(c *sseClient) {
		c.mu.Lock()
		if !c.changed[k] {
			c.changed[k] = true
			c.chOrder = append(c.chOrder, k)
		}
		c.mu.Unlock()
	})
}

func (h *Hub) each(fn func(c *sseClient)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		fn(c)
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// Clients returns the number of connected SSE clients.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// add registers a client; v is the visibility its request reported
// (visibility.go).
func (h *Hub) add(v clientVisibility) *sseClient {
	c := &sseClient{
		wake:     make(chan struct{}, 1),
		jobs:     map[string]JobEvent{},
		changed:  map[changedKey]bool{},
		lastSent: map[string]sentInfo{},
		id:       v.id,
		visible:  v.visible,
		visSeq:   v.seq,
	}
	h.init()
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	// Registered first, so the immediate sample the resource watcher takes
	// on a rising count reaches this client too.
	h.notifyClients()
	return c
}

func (h *Hub) remove(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	h.notifyClients()
}

type sseItem struct {
	event string
	data  any
}

// take returns what can be sent now and when the next deferred job event
// becomes due (zero if none).
func (c *sseClient) take(now time.Time, min time.Duration) (items []sseItem, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keep := c.jobOrder[:0]
	for _, id := range c.jobOrder {
		ev := c.jobs[id]
		last, seen := c.lastSent[id]
		if seen && last.status == ev.Status && now.Sub(last.at) < min {
			due := last.at.Add(min)
			if next.IsZero() || due.Before(next) {
				next = due
			}
			keep = append(keep, id)
			continue
		}
		items = append(items, sseItem{"job", ev})
		c.lastSent[id] = sentInfo{at: now, status: ev.Status}
		delete(c.jobs, id)
	}
	c.jobOrder = keep
	for _, k := range c.chOrder {
		items = append(items, sseItem{"changed", ChangedEvent{Kind: k.kind, ID: k.id}})
	}
	c.chOrder = c.chOrder[:0]
	clear(c.changed)
	if c.res != nil {
		items = append(items, sseItem{"resources", *c.res})
		c.res = nil
	}
	if c.hasComps {
		items = append(items, sseItem{"components", c.comps})
		c.comps, c.hasComps = nil, false
	}
	// Forget throttle state of finished jobs so the map does not grow forever.
	if len(c.lastSent) > 4096 {
		for id, s := range c.lastSent {
			if now.Sub(s.at) > time.Minute {
				delete(c.lastSent, id)
			}
		}
	}
	return items, next
}

// ServeHTTP is GET /api/events.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	c := h.add(visibilityOf(r))
	defer h.remove(c)

	min, ping, wto := h.MinInterval, h.PingInterval, h.WriteTimeout
	if min <= 0 {
		min = 500 * time.Millisecond
	}
	if ping <= 0 {
		ping = 15 * time.Second
	}
	if wto <= 0 {
		wto = 30 * time.Second
	}
	write := func(fn func(io.Writer) error) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(wto))
		if err := fn(w); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// An initial comment makes the browser's EventSource fire "open" at once.
	if !write(func(w io.Writer) error { _, err := io.WriteString(w, ": connected\n\n"); return err }) {
		return
	}

	pinger := time.NewTicker(ping)
	defer pinger.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closed:
			return
		case <-pinger.C:
			if !write(func(w io.Writer) error { _, err := io.WriteString(w, ": ping\n\n"); return err }) {
				return
			}
			continue
		case <-c.wake:
		case <-timer.C:
		}
		items, next := c.take(time.Now(), min)
		if len(items) > 0 {
			ok := write(func(w io.Writer) error {
				for _, it := range items {
					b, err := json.Marshal(it.data)
					if err != nil {
						slog.Error("sse marshal", "err", err)
						continue
					}
					if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", it.event, b); err != nil {
						return err
					}
				}
				return nil
			})
			if !ok {
				return
			}
		}
		if !next.IsZero() {
			timer.Reset(time.Until(next))
		}
	}
}
