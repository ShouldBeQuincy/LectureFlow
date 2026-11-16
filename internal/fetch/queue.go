// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package fetch is the download queue of the browser extension (SPEC-ext
// "Native Messaging 协议与下载队列", .claude/ext-brief.md §6.2): the
// extension hands over the media tracks of a recording with the cookies
// and the page they need; the built-in aria2c downloads them; HLS and DASH
// tracks are joined and audio + video combined by the built-in ffmpeg
// offline; the finished file becomes a lecture draft that is launched at
// once.
//
// Credentials (cookies, the page as Referer, the full track URLs with their
// signed query strings, and their paths, which may be credentials too)
// live only in this process's memory: never in the database, on disk, in a
// log or on a command line (the database and the log name a track by its
// host and a hash of its path). A task that loses them (a restart, a
// 401/403, a failure left without a retry for FailedCredentialTTL) waits in
// needs_refresh until the extension sends them again. Cookies reach only
// the host they belong to, after the redirects Go followed (redirect.go).
package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/bridge"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// Options configure a Queue.
type Options struct {
	Store   Store
	DataDir string
	// Aria2c and FFmpeg are the executables ("" = from PATH).
	Aria2c string
	FFmpeg string
	// Import creates and launches the lecture of a finished download
	// (Server.ImportDownload); set it with SetImport when the server is
	// built after the queue.
	Import ImportFunc
	// OnChange is called when a task changed, at most every
	// StatusInterval per task (SSE changed {kind: "downloads", id}).
	OnChange func(taskID int64)

	// Tests.
	HTTPClient     *http.Client  // playlists (default: 60 s timeout)
	Launcher       Launcher      // aria2c (default: run it, hidden window)
	IdleTimeout    time.Duration // aria2c stops after this long unused (default 5 min)
	PollInterval   time.Duration // aria2c status polling (default 500 ms)
	StatusInterval time.Duration // throttle of status pushes and OnChange (default 500 ms)
	MinFree        int64         // free space kept on the data directory's volume (default 2 GB; <0 = no check)
	FreeSpace      func(dir string) (int64, error)
	Logger         *slog.Logger
	Now            func() time.Time
	// FailedCredentialTTL is how long a failed task keeps its credentials
	// for a retry (lead's ruling 4; default DefaultFailedCredentialTTL);
	// CredentialSweep how often that is checked (default 1 min).
	FailedCredentialTTL time.Duration
	CredentialSweep     time.Duration
	// SpaceInterval is how often a running download looks at the free
	// space and at the size of the task's directory (default
	// DefaultSpaceInterval); MaxTaskBytes the most a task's files may hold
	// (default DefaultMaxTaskBytes; <0 = no limit). AUD3 low-2,
	// disklimit.go.
	SpaceInterval time.Duration
	MaxTaskBytes  int64

	// allowLoopback lets downloads reach this computer (local.go): the
	// tests' media servers listen on 127.0.0.1. Only this package's tests
	// set it; the product has DevLoopbackEnv alone.
	allowLoopback bool
	// ariaVersion stands in for "aria2c --version" (engine.features; this
	// package's tests, whose aria2c is the test binary).
	ariaVersion func(exe string) (string, error)
}

// Queue is the download queue. It implements the Check, Enqueue, Refresh
// and Subscribe methods of bridge.Handler.
type Queue struct {
	o       Options
	st      Store
	eng     *engine
	hc      *http.Client
	local   *addrPolicy // which hosts downloads may reach (local.go)
	log     *slog.Logger
	changed *keyedThrottle

	ctx    context.Context // ends with Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	tasks   map[int64]*task // tasks of this process: waiting, running, or failed with credentials
	seq     int64           // queue order
	running int
	conc    int // tasks at once
	conns   int // connections per file
	subs    map[*subscriber]context.CancelFunc
	started bool
	closed  bool
	imp     ImportFunc
}

// New builds a queue; Start must run before it downloads anything.
func New(o Options) *Queue {
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = 5 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.StatusInterval <= 0 {
		o.StatusInterval = 500 * time.Millisecond
	}
	if o.MinFree == 0 {
		o.MinFree = 2 << 30
	}
	if o.SpaceInterval <= 0 {
		o.SpaceInterval = DefaultSpaceInterval
	}
	if o.MaxTaskBytes == 0 {
		o.MaxTaskBytes = DefaultMaxTaskBytes
	}
	if o.FailedCredentialTTL <= 0 {
		o.FailedCredentialTTL = DefaultFailedCredentialTTL
	}
	if o.CredentialSweep <= 0 {
		o.CredentialSweep = time.Minute
	}
	if o.FreeSpace == nil {
		o.FreeSpace = diskFree
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Launcher == nil {
		o.Launcher = execLauncher
	}
	if o.FFmpeg == "" {
		o.FFmpeg = "ffmpeg"
	}
	local := &addrPolicy{allowLoopback: o.allowLoopback}
	if os.Getenv(DevLoopbackEnv) == "1" && !local.allowLoopback {
		local.allowLoopback = true
		o.Logger.Warn("downloads: development switch " + DevLoopbackEnv + " is on: downloads may reach this computer's loopback addresses")
	}
	// hc gives the timeout of Go's own requests; each task run has its own
	// transport (taskNet), unless a test gave the client
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	q := &Queue{
		o: o, st: o.Store, hc: hc, local: local, log: o.Logger,
		tasks: map[int64]*task{}, subs: map[*subscriber]context.CancelFunc{},
		conc: DefaultConcurrency, conns: DefaultConnections, imp: o.Import,
	}
	q.ctx, q.cancel = context.WithCancel(context.Background())
	// The RPC client has no proxy: aria2c listens on the loopback
	// interface (127.0.0.1, which the client uses, and [::1]). Each aria2c
	// gets its own transport, whose connections go to its port only once
	// it is aria2c's (engine.rpcTransport); this client gives the timeout.
	rpcHC := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4}}
	q.eng = &engine{exe: o.Aria2c, launch: o.Launcher, idle: o.IdleTimeout, hc: rpcHC, log: o.Logger, versionOf: o.ariaVersion}
	if o.OnChange != nil {
		q.changed = newKeyedThrottle(o.StatusInterval, o.OnChange)
	}
	return q
}

// SetImport sets the lecture import (cmd: the HTTP server is built after
// the queue). Call it before Start.
func (q *Queue) SetImport(fn ImportFunc) {
	q.mu.Lock()
	q.imp = fn
	q.mu.Unlock()
}

// Start is the queue's start-up: every task that had not ended when the
// last process stopped becomes needs_refresh (its credentials were in that
// process only), except those whose every track is already on disk: they
// go on locally (merge, import) without credentials (lead 2026-10-27; the
// user would otherwise have to go back to the browser for nothing). The
// settings are read, and leftover task directories are removed in the
// background.
func (q *Queue) Start(ctx context.Context) error {
	n, err := q.st.ResetUnfinishedDownloads(ctx, CodeRestarted, q.nowStr())
	if err != nil {
		return err
	}
	q.scrubPaths(ctx)
	q.loadLimits(ctx)
	var local []db.DownloadTask
	if rows, err := q.st.ListDownloadTasks(ctx); err == nil {
		for _, rec := range rows {
			if rec.State == bridge.StateNeedsRefresh && rec.ErrorCode == CodeRestarted &&
				downloadsComplete(q.taskDir(rec.ID), recordsOf(rec)) {
				local = append(local, rec)
			}
		}
	} else {
		q.log.Warn("downloads: tasks not readable after the restart", "err", err)
	}
	if n > 0 {
		q.log.Info("downloads: unfinished tasks after the restart", "tasks", n, "continued_locally", len(local))
	}
	q.mu.Lock()
	q.started = true
	for _, rec := range local {
		if err := q.queueLocked(&task{rec: rec}); err != nil && !errors.Is(err, errGone) {
			q.log.Warn("downloads: a task with its files complete could not go on", "task", rec.ID, "err", err)
		}
	}
	q.dispatchLocked()
	q.mu.Unlock()
	q.wg.Add(2)
	go func() {
		defer q.wg.Done()
		q.sweepDirs(q.ctx)
	}()
	go func() {
		defer q.wg.Done()
		q.sweepCredentials(q.ctx)
	}()
	return nil
}

// scrubPaths rewrites the rows that still hold track paths (written before
// ruling 3) with their hashes, so that no path stays in the database even
// for tasks that are never written again (done ones). updated_at is kept.
func (q *Queue) scrubPaths(ctx context.Context) {
	rows, err := q.st.ListDownloadTasks(ctx)
	if err != nil {
		q.log.Warn("downloads: tasks not readable for the path scrub", "err", err)
		return
	}
	n := 0
	for _, rec := range rows {
		s, changed := scrubTracksJSON(rec.TracksJSON)
		if !changed {
			continue
		}
		rec.TracksJSON = s
		if err := q.st.UpdateDownloadTask(ctx, rec); err != nil && !errors.Is(err, db.ErrNotFound) {
			q.log.Warn("downloads: a task's track paths could not be removed", "task", rec.ID, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		q.log.Info("downloads: track paths replaced by their hashes", "tasks", n)
	}
}

// sweepCredentials ends the grace period of failed tasks (lead's ruling
// 4): their credentials stay in memory for a retry, at most
// FailedCredentialTTL; then they are forgotten and the task needs a
// refresh, unless every track is already on disk (merging or importing
// failed), which a retry can finish without credentials.
func (q *Queue) sweepCredentials(ctx context.Context) {
	tick := time.NewTicker(q.o.CredentialSweep)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		q.mu.Lock()
		for _, t := range q.tasks {
			q.expireLocked(t)
		}
		q.mu.Unlock()
	}
}

// credentialsDue: t failed long enough ago that its credentials must go.
func (q *Queue) credentialsDue(t *task) bool {
	return t.rec.State == bridge.StateFailed && t.hasCreds() && !t.failedAt.IsZero() &&
		q.o.Now().Sub(t.failedAt) >= q.o.FailedCredentialTTL
}

// expireLocked forgets the credentials of a failed task whose grace
// period is over; it reports whether it did.
func (q *Queue) expireLocked(t *task) bool {
	if !q.credentialsDue(t) {
		return false
	}
	t.forget()
	t.failedAt = time.Time{}
	id := t.rec.ID
	if downloadsComplete(q.taskDir(id), t.records()) {
		q.log.Info("downloads: credentials of a failed task cleared; its files are complete", "task", id)
		return true
	}
	if err := q.saveLocked(t, bridge.StateNeedsRefresh, q.clearedFailure()); err == nil {
		delete(q.tasks, id)
	}
	q.log.Info("downloads: credentials of a failed task cleared; it needs a refresh", "task", id)
	return true
}

func (q *Queue) clearedFailure() *failure {
	return &failure{refresh: true, code: CodeCredentialsCleared,
		params: map[string]any{"minutes": int((q.o.FailedCredentialTTL + time.Minute - 1) / time.Minute)}}
}

// Close stops every running task where it is (the next start marks them
// needs_refresh), forgets every credential, stops aria2c and ends the
// status subscriptions. It waits at most timeout for the running tasks.
func (q *Queue) Close(timeout time.Duration) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	for _, cancel := range q.subs {
		cancel()
	}
	q.mu.Unlock()
	q.cancel()
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		q.log.Warn("downloads: running tasks did not stop in time")
	}
	q.eng.close()
	q.changed.stop()
	q.mu.Lock()
	for id, t := range q.tasks {
		t.forget()
		delete(q.tasks, id)
	}
	q.mu.Unlock()
}

// SettingsChanged rereads the download settings (PATCH /api/app/settings)
// and starts waiting tasks when the limit went up.
func (q *Queue) SettingsChanged() {
	q.loadLimits(context.Background())
	q.mu.Lock()
	q.dispatchLocked()
	q.mu.Unlock()
}

// ReadLimits returns the stored download settings (defaults for missing or
// invalid values).
func ReadLimits(ctx context.Context, kv interface {
	GetKV(ctx context.Context, key string) (string, bool, error)
}) (concurrency, connections int, err error) {
	read := func(key string, lo, hi, def int) (int, error) {
		v, ok, err := kv.GetKV(ctx, key)
		if err != nil || !ok {
			return def, err
		}
		n, perr := strconv.Atoi(strings.TrimSpace(v))
		if perr != nil || n < lo || n > hi {
			return def, nil
		}
		return n, nil
	}
	if concurrency, err = read(KVConcurrency, MinConcurrency, MaxConcurrency, DefaultConcurrency); err != nil {
		return
	}
	connections, err = read(KVConnections, MinConnections, MaxConnections, DefaultConnections)
	return
}

func (q *Queue) loadLimits(ctx context.Context) {
	c, n, err := ReadLimits(ctx, q.st)
	if err != nil {
		q.log.Warn("downloads: settings not readable; using the defaults", "err", err)
	}
	q.mu.Lock()
	q.conc, q.conns = c, n
	q.mu.Unlock()
}

func (q *Queue) nowStr() string { return db.FormatTime(q.o.Now()) }

func (q *Queue) downloadsDir() string { return filepath.Join(q.o.DataDir, "downloads") }

func (q *Queue) taskDir(id int64) string {
	return filepath.Join(q.downloadsDir(), strconv.FormatInt(id, 10))
}

// sweepDirs removes task directories no task needs any more (deleted,
// done or cancelled tasks whose removal was interrupted).
func (q *Queue) sweepDirs(ctx context.Context) {
	entries, err := os.ReadDir(q.downloadsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || !e.IsDir() {
			continue
		}
		rec, err := q.st.GetDownloadTask(ctx, id)
		if err == nil && rec.State != bridge.StateDone && rec.State != bridge.StateCancelled {
			continue
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			continue
		}
		q.mu.Lock()
		_, live := q.tasks[id]
		q.mu.Unlock()
		if !live {
			removeAllRetry(filepath.Join(q.downloadsDir(), e.Name()))
		}
	}
}

// removeAllRetry removes a directory, retrying for a moment: on Windows a
// file aria2c or ffmpeg just closed may still be locked.
func removeAllRetry(dir string) {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	slog.Warn("downloads: could not remove a task directory", "dir", dir, "err", err)
}

// ---------- tasks in memory ----------

// task is the in-memory side of a download_task row.
type task struct {
	rec db.DownloadTask
	// The credentials: nil tracks = none (after a restart or a refresh
	// request, or once the task ended).
	tracks  []liveTrack
	cookies []bridge.Cookie
	referer string
	// (the profile's maxConnectionsPerFile is rec.MaxConnections: it
	// outlives a restart and a refresh)

	queuedSeq int64
	cancel    context.CancelFunc // non-nil while running
	done      chan struct{}      // closed when the runner returned
	stopAs    string             // set by Cancel: the runner leaves the state alone
	prog      []trackProgress
	persistAt time.Time
	detail    string    // detail of the last failure (查看详情), memory only
	failedAt  time.Time // when it failed with its credentials kept (ruling 4)
}

type liveTrack struct {
	rec trackRecord
	url string
}

type trackProgress struct {
	done, total int64 // total -1 = unknown
	// seen: the track reported something real (bytes, or a known total).
	// Until every track has, a resumed task shows its stored progress:
	// aria2c reports 0 of unknown until it has read its control file
	// (M1 acceptance: the bar went back to 0 at every resume).
	seen bool
}

func (t *task) hasCreds() bool { return t.tracks != nil }

// forget drops the credentials.
func (t *task) forget() {
	for i := range t.tracks {
		t.tracks[i].url = ""
	}
	t.tracks, t.cookies, t.referer = nil, nil, ""
}

func (t *task) records() []trackRecord { return parseRecords(t.rec.TracksJSON) }

func recordsOf(rec db.DownloadTask) []trackRecord { return parseRecords(rec.TracksJSON) }

func ended(state string) bool {
	return state == bridge.StateDone || state == bridge.StateFailed || state == bridge.StateCancelled
}

func runningState(state string) bool {
	return state == bridge.StateDownloading || state == bridge.StateMerging || state == bridge.StateImporting
}

// failure is how a task stopped short of done.
type failure struct {
	refresh bool // needs_refresh rather than failed
	code    string
	params  map[string]any
	detail  string // for 查看详情 and the log; never a URL with its query
	// discard: every file of the task goes with the failure, and its
	// progress (disklimit.go: too large); discardAfter removes them before
	// the runner saves the task.
	discard bool
}

func (f *failure) Error() string {
	if f.detail != "" {
		return f.code + ": " + f.detail
	}
	return f.code
}

// errGone: the task's row disappeared (its course was deleted).
var errGone = errors.New("download task row is gone")

// saveLocked writes the task's state. On errGone the task is dropped.
func (q *Queue) saveLocked(t *task, state string, f *failure) error {
	rec := t.rec
	rec.State = state
	rec.ErrorCode, rec.ErrorParams = "", ""
	if f != nil {
		rec.ErrorCode = f.code
		if len(f.params) > 0 {
			b, _ := json.Marshal(f.params)
			rec.ErrorParams = string(b)
		}
		t.detail = f.detail
	} else {
		t.detail = ""
	}
	rec.BytesDone, rec.BytesTotal = t.bytes()
	rec.UpdatedAt = q.nowStr()
	rec.TracksJSON, _ = scrubTracksJSON(rec.TracksJSON) // a row from before ruling 3
	if err := q.st.UpdateDownloadTask(context.Background(), rec); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			q.dropLocked(t)
			return errGone
		}
		return err
	}
	t.rec = rec
	t.persistAt = q.o.Now()
	q.notifyLocked(t)
	return nil
}

// stopGone is task.stopAs of a task whose row disappeared.
const stopGone = "gone"

// dropLocked forgets a task whose row is gone (its course was deleted):
// a running task is stopped and its runner removes the files.
func (q *Queue) dropLocked(t *task) {
	t.forget()
	delete(q.tasks, t.rec.ID)
	q.kick(t.rec.ID)
	if t.cancel != nil {
		t.stopAs = stopGone
		t.cancel()
		return
	}
	go removeAllRetry(q.taskDir(t.rec.ID))
}

// bytes is the progress of all tracks (total -1 while any is unknown);
// the stored values without live progress, or while a track has not
// reported yet (trackProgress.seen).
func (t *task) bytes() (done, total int64) {
	if len(t.prog) == 0 {
		return t.rec.BytesDone, t.rec.BytesTotal
	}
	for _, p := range t.prog {
		if !p.seen {
			return t.rec.BytesDone, t.rec.BytesTotal
		}
	}
	total = 0
	for _, p := range t.prog {
		done += p.done
		if p.total < 0 || total < 0 {
			total = -1
		} else {
			total += p.total
		}
	}
	return done, total
}

// positionLocked is the 1-based place among waiting tasks (0 = not waiting).
func (q *Queue) positionLocked(t *task) int {
	if t.rec.State != bridge.StateQueued {
		return 0
	}
	n := 1
	for _, o := range q.tasks {
		if o != t && o.rec.State == bridge.StateQueued && o.queuedSeq < t.queuedSeq {
			n++
		}
	}
	return n
}

func (q *Queue) kick(id int64) { q.changed.kick(id) }

// notifyLocked tells the bridge subscribers and the SSE clients.
func (q *Queue) notifyLocked(t *task) {
	st := q.statusLocked(t.rec, t)
	for s := range q.subs {
		s.push(st)
	}
	q.kick(t.rec.ID)
}

func errorInfo(rec db.DownloadTask, detail string) *model.ErrorInfo {
	if rec.ErrorCode == "" || (rec.State != bridge.StateFailed && rec.State != bridge.StateNeedsRefresh) {
		return nil
	}
	info := &model.ErrorInfo{Code: rec.ErrorCode, Detail: detail}
	if rec.ErrorParams != "" {
		_ = json.Unmarshal([]byte(rec.ErrorParams), &info.Params)
	}
	return info
}

func (q *Queue) statusLocked(rec db.DownloadTask, t *task) bridge.Status {
	st := bridge.Status{Type: bridge.TypeStatus, TaskID: rec.ID, Platform: rec.Platform, SessionID: rec.SessionID,
		CourseID: rec.CourseID, State: rec.State, BytesDone: rec.BytesDone, BytesTotal: rec.BytesTotal}
	if t != nil {
		st.Position = q.positionLocked(t)
		st.BytesDone, st.BytesTotal = t.bytes()
	}
	if rec.LectureID != nil {
		st.LectureID = *rec.LectureID
	}
	if info := errorInfo(rec, ""); info != nil {
		st.Error = &bridge.Error{Code: info.Code, Params: info.Params}
	}
	return st
}

// ---------- scheduling ----------

// queueLocked makes t wait for a slot.
func (q *Queue) queueLocked(t *task) error {
	q.seq++
	t.queuedSeq = q.seq
	t.prog = nil
	t.failedAt = time.Time{}
	q.tasks[t.rec.ID] = t
	if err := q.saveLocked(t, bridge.StateQueued, nil); err != nil {
		return err
	}
	q.dispatchLocked()
	return nil
}

// dispatchLocked starts waiting tasks, oldest first, while slots are free.
func (q *Queue) dispatchLocked() {
	if !q.started || q.closed {
		return
	}
	startedAny := false
	for q.running < q.conc {
		var next *task
		for _, t := range q.tasks {
			if t.rec.State == bridge.StateQueued && t.cancel == nil && (next == nil || t.queuedSeq < next.queuedSeq) {
				next = t
			}
		}
		if next == nil || !q.startLocked(next) {
			break // (a task that could not start waits for the next event)
		}
		startedAny = true
	}
	if startedAny {
		q.notifyQueuedLocked() // the others moved up
	}
}

// notifyQueuedLocked announces the waiting tasks (their positions changed).
func (q *Queue) notifyQueuedLocked() {
	for _, t := range q.tasks {
		if t.rec.State == bridge.StateQueued {
			q.notifyLocked(t)
		}
	}
}

func (q *Queue) startLocked(t *task) bool {
	ctx, cancel := context.WithCancel(q.ctx)
	t.cancel, t.done, t.stopAs = cancel, make(chan struct{}), ""
	t.prog = make([]trackProgress, len(t.records()))
	for i := range t.prog {
		t.prog[i].total = -1
	}
	q.running++
	if err := q.saveLocked(t, bridge.StateDownloading, nil); err != nil {
		q.running--
		cancel()
		t.cancel = nil
		close(t.done)
		t.prog = nil
		if !errors.Is(err, errGone) {
			q.log.Error("downloads: a task could not start", "task", t.rec.ID, "err", err)
		}
		return false
	}
	snap := runSnapshot{
		id: t.rec.ID, records: t.records(), courseID: t.rec.CourseID, title: t.rec.Title, recordedAt: t.rec.RecordedAt,
		conns: q.conns, referer: t.referer,
		cookies: append([]bridge.Cookie(nil), t.cookies...),
		net:     q.newTaskNet(pageHostOf(t.referer, t.rec.PageHost)),
	}
	if m := t.rec.MaxConnections; m > 0 && m < snap.conns {
		snap.conns = m
	}
	if t.hasCreds() {
		snap.urls = make([]string, len(t.tracks))
		for i, lt := range t.tracks {
			snap.urls[i] = lt.url
		}
	}
	q.wg.Add(1)
	go q.run(ctx, t, snap)
	return true
}

// run is a task's runner goroutine.
func (q *Queue) run(ctx context.Context, t *task, snap runSnapshot) {
	defer q.wg.Done()
	err := q.process(ctx, t, snap)
	snap.net.close()
	q.mu.Lock()
	defer q.mu.Unlock()
	q.running--
	t.cancel = nil
	close(t.done)
	id := t.rec.ID
	switch {
	case t.stopAs == stopGone:
		go removeAllRetry(q.taskDir(id))
	case t.stopAs != "":
		// Cancel decides what happens next.
	case errors.Is(err, errGone):
	case err == nil:
		// done (process saved it)
	case q.ctx.Err() != nil:
		// Close: the row stays as it is but for the bytes reached (the
		// database copy is up to 5 s old); the next start marks it
		// needs_refresh and the resume starts from there.
		if d, tot := t.bytes(); d != t.rec.BytesDone || tot != t.rec.BytesTotal {
			if err := q.st.SetDownloadProgress(context.Background(), id, d, tot, q.nowStr()); err == nil {
				t.rec.BytesDone, t.rec.BytesTotal = d, tot
			}
		}
		t.forget()
		t.prog = nil
		delete(q.tasks, id)
	default:
		f := asFailure(err)
		if f.discard {
			// its files went (discardAfter): so does the progress they held
			t.prog = nil
			t.rec.BytesDone, t.rec.BytesTotal = 0, -1
		}
		q.fillNoSpace(f)
		state := bridge.StateFailed
		if f.refresh {
			state = bridge.StateNeedsRefresh
			t.forget()
		} else if !Retryable(f.code) {
			// no retry is offered (failedRetry): nothing to keep the
			// credentials for; the task stays in memory for its detail
			t.forget()
		} else if t.hasCreds() {
			t.failedAt = q.o.Now() // kept for a retry, at most FailedCredentialTTL
		}
		q.log.Warn("downloads: task stopped", "task", id, "state", state, "code", f.code, "detail", f.detail)
		saved := q.saveLocked(t, state, f) == nil // with the bytes reached
		t.prog = nil
		if saved && state == bridge.StateNeedsRefresh {
			delete(q.tasks, id) // nothing in memory is needed any more
		}
	}
	q.dispatchLocked()
}

// setState saves a state reached by the runner (merging, importing).
func (q *Queue) setState(t *task, state string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t.stopAs != "" {
		return context.Canceled
	}
	return q.saveLocked(t, state, nil)
}

// progress records a track's bytes; the database copy every 5 s.
func (q *Queue) progress(t *task, i int, done, total int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if i >= len(t.prog) || t.stopAs != "" {
		return
	}
	p := trackProgress{done: done, total: total, seen: t.prog[i].seen || done > 0 || total >= 0}
	if t.prog[i] == p {
		return
	}
	t.prog[i] = p
	if now := q.o.Now(); now.Sub(t.persistAt) >= 5*time.Second {
		d, tot := t.bytes()
		if err := q.st.SetDownloadProgress(context.Background(), t.rec.ID, d, tot, db.FormatTime(now)); err == nil {
			t.rec.BytesDone, t.rec.BytesTotal = d, tot
			t.persistAt = now
		}
	}
	q.notifyLocked(t)
}

// ---------- the HTTP layer ----------

func (q *Queue) infoLocked(rec db.DownloadTask, t *task) TaskInfo {
	info := TaskInfo{
		ID: rec.ID, Platform: rec.Platform, SessionID: rec.SessionID, CourseID: rec.CourseID, Title: rec.Title,
		State: rec.State, BytesDone: rec.BytesDone, BytesTotal: rec.BytesTotal, OverrideDisabled: rec.OverrideDisabled,
		AllowedActions: AllowedActions(rec.State, rec.ErrorCode), CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, Tracks: []TrackInfo{},
	}
	if rec.RecordedAt != nil {
		info.RecordedAt = *rec.RecordedAt
	}
	if rec.LectureID != nil {
		info.LectureID = *rec.LectureID
	}
	detail := ""
	if t != nil {
		info.Position = q.positionLocked(t)
		info.BytesDone, info.BytesTotal = t.bytes()
		detail = t.detail
	}
	info.Error = errorInfo(rec, detail)
	for _, r := range recordsOf(rec) {
		info.Tracks = append(info.Tracks, TrackInfo{Kind: r.Kind, Format: r.Format, Role: r.Role, Label: r.Label})
	}
	return info
}

// List returns the tasks (of one course when courseID is not ""), newest
// first.
func (q *Queue) List(ctx context.Context, courseID string) ([]TaskInfo, error) {
	rows, err := q.st.ListDownloadTasks(ctx)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	out := make([]TaskInfo, 0, len(rows))
	for _, rec := range rows {
		if courseID != "" && rec.CourseID != courseID {
			continue
		}
		t := q.tasks[rec.ID]
		if t != nil {
			rec = t.rec
		}
		out = append(out, q.infoLocked(rec, t))
	}
	q.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Get returns one task (CodeNotFound).
func (q *Queue) Get(ctx context.Context, id int64) (TaskInfo, error) {
	q.mu.Lock()
	if t := q.tasks[id]; t != nil {
		defer q.mu.Unlock()
		return q.infoLocked(t.rec, t), nil
	}
	q.mu.Unlock()
	rec, err := q.st.GetDownloadTask(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return TaskInfo{}, codedNotFound()
	}
	if err != nil {
		return TaskInfo{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.infoLocked(rec, q.tasks[id]), nil
}

// taskLocked returns the in-memory task of id, loading its row when it is
// not in memory (then without credentials, not registered).
func (q *Queue) taskLocked(ctx context.Context, id int64) (*task, error) {
	if t := q.tasks[id]; t != nil {
		return t, nil
	}
	rec, err := q.st.GetDownloadTask(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil, codedNotFound()
	}
	if err != nil {
		return nil, err
	}
	return &task{rec: rec}, nil
}

// stopLocked stops t's runner (if any) for good and waits for it; q.mu is
// released while waiting.
func (q *Queue) stopLocked(t *task, as string) {
	if t.cancel == nil {
		return
	}
	t.stopAs = as
	t.cancel()
	done := t.done
	q.mu.Unlock()
	<-done
	q.mu.Lock()
}

// Cancel stops a task that has not ended (CodeNotCancellable otherwise, and
// while it is importing): its files are deleted and its credentials
// forgotten.
func (q *Queue) Cancel(ctx context.Context, id int64) (TaskInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.taskLocked(ctx, id)
	if err != nil {
		return TaskInfo{}, err
	}
	if ended(t.rec.State) || t.rec.State == bridge.StateImporting {
		return TaskInfo{}, codedConflict(CodeNotCancellable, t.rec.State)
	}
	q.stopLocked(t, bridge.StateCancelled)
	if t.rec.State == bridge.StateImporting || ended(t.rec.State) {
		// it moved on while stopping
		return TaskInfo{}, codedConflict(CodeNotCancellable, t.rec.State)
	}
	t.forget()
	t.prog = nil
	if err := q.saveLocked(t, bridge.StateCancelled, nil); err != nil {
		if errors.Is(err, errGone) {
			return TaskInfo{}, codedNotFound()
		}
		return TaskInfo{}, err
	}
	delete(q.tasks, id)
	removeAllRetry(q.taskDir(id))
	q.log.Info("downloads: task cancelled", "task", id)
	q.dispatchLocked()
	q.notifyQueuedLocked() // positions behind it moved up
	return q.infoLocked(t.rec, nil), nil
}

// Retry queues a failed task again. With its credentials still in memory,
// or with every track already downloaded, it waits for a slot; otherwise
// (the app restarted since) it becomes needs_refresh. A needs_refresh task
// answers CodeNeedsRefresh: only the extension can bring new credentials.
// A failure a retry cannot fix (Retryable) answers its own code, 409 (the
// task offers remove only).
func (q *Queue) Retry(ctx context.Context, id int64) (TaskInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.taskLocked(ctx, id)
	if err != nil {
		return TaskInfo{}, err
	}
	switch t.rec.State {
	case bridge.StateFailed:
		if !Retryable(t.rec.ErrorCode) {
			// the same failure again (failedRetry): its own reason, 409
			info := errorInfo(t.rec, "")
			return TaskInfo{}, model.Errorf(info.Code, info.Params, ErrConflict)
		}
	case bridge.StateNeedsRefresh:
		return TaskInfo{}, codedConflict(CodeNeedsRefresh, "")
	default:
		return TaskInfo{}, codedConflict(CodeNotRetryable, t.rec.State)
	}
	cleared := false
	if q.credentialsDue(t) {
		// the grace period ended before the sweep came by
		t.forget()
		t.failedAt = time.Time{}
		cleared = true
	}
	if !t.hasCreds() && !downloadsComplete(q.taskDir(id), t.records()) {
		f := &failure{refresh: true, code: CodeRestarted}
		if cleared {
			f = q.clearedFailure()
		}
		if err := q.saveLocked(t, bridge.StateNeedsRefresh, f); err != nil {
			if errors.Is(err, errGone) {
				return TaskInfo{}, codedNotFound()
			}
			return TaskInfo{}, err
		}
		delete(q.tasks, id)
		return q.infoLocked(t.rec, nil), nil
	}
	if err := q.queueLocked(t); err != nil {
		if errors.Is(err, errGone) {
			return TaskInfo{}, codedNotFound()
		}
		return TaskInfo{}, err
	}
	q.log.Info("downloads: task retried", "task", id)
	return q.infoLocked(t.rec, t), nil
}

// Remove deletes an ended task (done, failed, cancelled; CodeActive
// otherwise) with its leftover files. A done task's lecture stays.
func (q *Queue) Remove(ctx context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, err := q.taskLocked(ctx, id)
	if err != nil {
		return err
	}
	if !ended(t.rec.State) {
		return codedConflict(CodeActive, t.rec.State)
	}
	if err := q.st.DeleteDownloadTask(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return codedNotFound()
		}
		return err
	}
	t.forget()
	delete(q.tasks, id)
	removeAllRetry(q.taskDir(id))
	q.kick(id)
	return nil
}

// Activity counts the tasks of this process for /api/app/activity.
func (q *Queue) Activity() Activity {
	q.mu.Lock()
	defer q.mu.Unlock()
	var a Activity
	for _, t := range q.tasks {
		switch {
		case runningState(t.rec.State):
			a.Active++
		case t.rec.State == bridge.StateQueued:
			a.Queued++
		}
	}
	return a
}
