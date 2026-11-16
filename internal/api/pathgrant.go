// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Path grants (security audit FB1 L-7, T-SEC-08). A desktop path upload
// (upload_path.go) reads a file the page names by its local path. The page
// only learns such paths from the system file dialog or a drop, both of
// which the desktop main process sees first; the main process registers
// each path here as a one-time grant (POST /api/app/path-grants), and
// checkSourcePath accepts only a path that carries one. A page that runs
// someone else's script can then no longer make the service read an
// arbitrary local file, or open \\server\share\… and send the user's NTLM
// credentials to that server: such a path is refused before anything
// touches the file system.
//
// The channel is the main process's own API client (the access token, as
// for its other calls) plus the X-LF-Path-Grant header: an HMAC of the
// access token that only the main process computes. The page's requests
// carry the token too (the main process adds it), but the page never sees
// it, so it cannot make the grant header.
//
// Binding: a grant is for one spelling of one path, compared after
// pathGrantKey (purely lexical, nothing is resolved on disk): separators
// unified, filepath.Clean (repeated separators, "." and ".." components, a
// trailing separator), and on Windows ASCII letters without case (like
// media.UnderRoot; Unicode case folding is not what Windows does). Not
// expanded: 8.3 short names (C:\PROGRA~1\… is another spelling, refused)
// and links (a link the user picked is granted as the link; the checks of
// checkSourcePath then follow it as before). \\?\, \??\ and \\.\ paths
// never get that far (weirdPathPrefix refuses them first).
//
// A grant is used up by the request that uses it, and returned when that
// request fails (an error status), so that "retry" after freeing disk
// space still works. A file grant expires pathGrantTTL (12 hours) after its
// registration, and every request that used a file grant and succeeded
// renews all unexpired file grants to 12 hours from then (security review
// SEC24 M1):
// the page copies one file per course at a time, so files dropped behind a
// long queue (a slow NAS, a USB 2 card reader, large recordings) wait as
// long as the copies before them take; a short expiry broke those uploads,
// and "retry" could not help. A long expiry widens nothing an attacker can
// read: a grant is only ever for a path the user picked or dropped, and it
// is used once. A request that fails returns its grant without renewing
// anything (SEC24b), so failing on purpose cannot keep grants alive.
//
// A path registered again before it is used gets another grant (the same
// PDF dropped on two lectures while the queue is busy), up to
// pathGrantPerKey; all grants of one path take one place of the
// pathGrantMax, so a page re-registering a file the user dropped cannot
// push out the user's other picks (SEC24 L2). The oldest registration is
// dropped first when room is needed. Nothing is written to disk; a restart
// forgets them all.
//
// Folder grants (the folder dialog's pick, for the data directory endpoints;
// handlers_datadir.go) follow the same rules but one: a folder grant expires
// folderGrantTTL (10 minutes) after its registration and takes no part in
// renewal. No successful request extends it, and the request that uses it
// extends nothing (security review SEC26 low-1). The reason for the 12
// hours does not apply to it: nothing queues behind other work. Between the
// folder dialog and the request that uses the pick there is only the page's
// own confirmation: in the settings and for the import one dialog of a few
// lines; in the setup guide the check first (it reads the current data
// directory once, before the dialog shows), then the dialog. Ten minutes is
// that many times over. With 12 hours and renewal, a pick the user then
// declined (cancel in that dialog, or another folder chosen after a failed
// check) stayed usable by any script in the page for as long as uploads
// kept succeeding; now it is for ten minutes at most. A user who leaves the
// confirmation open for longer gets data_dir.path_not_granted on
// confirming, whose text says to choose the folder again; a request that
// fails returns the grant with the expiry it had. The page also gives a
// pick up at once when it will not be used (DELETE /api/app/folder-pick,
// handlers_datadir.go; web/api.js forgetFolderPick: the user declined the
// confirmation, the check refused the folder, or the move or import was
// refused and returned its grant, SEC38 low-1: every page asks for the
// folder again then); that needs no secret, since losing a grant only ever
// means choosing the folder again.

const (
	// PathGrantHeader carries pathGrantSecret(token) on POST /api/app/path-grants.
	PathGrantHeader = "X-LF-Path-Grant"
	// pathGrantLabel is the HMAC message of the grant header.
	pathGrantLabel = "lectureflow path grant v1"
	// pathGrantTTL is how long a file grant stays valid after its
	// registration or after the last successful request that used a file
	// grant (see above).
	pathGrantTTL = 12 * time.Hour
	// folderGrantTTL is how long a folder grant stays valid after its
	// registration; nothing renews it (see "Folder grants" above).
	folderGrantTTL = 10 * time.Minute
	// pathGrantMax bounds the paths with grants (all grants of one path
	// count once); pathGrantPerPost one registration; pathGrantPerKey the
	// unused grants of one path.
	pathGrantMax     = 2000
	pathGrantPerPost = 1000
	pathGrantPerKey  = 8
	// pathGrantMaxBytes bounds one registered path.
	pathGrantMaxBytes = 128 << 10
	// pathGrantWait is how long a path upload waits for its grant: a drop's
	// grant goes page → preload → main process → service while the page
	// posts the upload directly, so it may arrive a moment later.
	pathGrantWait = 3 * time.Second
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("POST /api/app/path-grants", s.postPathGrants)
	})
}

// pathGrantSecret is the value of PathGrantHeader for token (hex HMAC-SHA256
// of pathGrantLabel keyed with the token; desktop/src/main/lib/pathgrant.js).
func pathGrantSecret(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(pathGrantLabel))
	return hex.EncodeToString(m.Sum(nil))
}

// pathGrantOK compares the grant header in constant time; an empty token
// accepts nothing.
func (d *DesktopOptions) pathGrantOK(got string) bool {
	if d.Token == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(pathGrantSecret(d.Token)))
}

// pathGrantKey is the comparison form of a path (see the binding rule
// above). Nothing is resolved or touched on disk.
func pathGrantKey(p string) string { return grantKeyFor(p, runtime.GOOS == "windows") }

func grantKeyFor(p string, windows bool) string {
	if !windows {
		return filepath.Clean(p)
	}
	q := filepath.Clean(strings.ReplaceAll(p, "/", `\`))
	b := []byte(q)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// networkPath reports a path that names another computer or a device: a
// UNC path (\\server\share\…, //server/share/…), \\?\UNC\…, a Win32
// device or extended-length path (\\.\…, \\?\…, \??\…). Purely lexical and
// the same on every system (the Windows forms are what matter; elsewhere
// //host/… is a local path, but nothing the dialog returns looks like it).
// Such a path is refused without a grant before any system call.
func networkPath(p string) bool {
	q := strings.ReplaceAll(strings.TrimSpace(p), "/", `\`)
	return strings.HasPrefix(q, `\\`) || strings.HasPrefix(q, `\??\`)
}

// grantBatch is the paths of one registration.
type grantBatch struct {
	seq      uint64
	deadline time.Time
	fixed    bool // folder grants: renew leaves the deadline alone
	dropped  bool // evicted for room: a failed request does not bring it back
}

// grantEntry is the unused grants of one path: n of them (1 to
// pathGrantPerKey), valid until the batch of its latest registration
// expires.
type grantEntry struct {
	batch *grantBatch
	n     int
}

// pathGrants is the in-memory grant store.
type pathGrants struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration // file grants; renewed
	dirTTL  time.Duration // folder grants; fixed
	max     int           // paths with grants
	perKey  int           // unused grants of one path
	wait    time.Duration
	seq     uint64
	m       map[string]*grantEntry // key → its unused grants
	changed chan struct{}          // closed (and replaced) on every add
}

func newPathGrants() *pathGrants {
	return &pathGrants{now: time.Now, ttl: pathGrantTTL, dirTTL: folderGrantTTL, max: pathGrantMax, perKey: pathGrantPerKey, wait: pathGrantWait,
		m: map[string]*grantEntry{}, changed: make(chan struct{})}
}

// folderGrantKey is the key of a folder grant (the folder dialog's pick,
// for the data directory endpoints; handlers_datadir.go). The NUL keeps it
// apart from every file key: a path with NUL is never accepted.
func folderGrantKey(p string) string { return "\x00folder\x00" + pathGrantKey(p) }

// add registers a file grant for each path as one batch.
func (g *pathGrants) add(paths []string) { g.addKeys(paths, pathGrantKey, false) }

// addFolders registers a folder grant for each path as one batch (the same
// counting as files: picking the same folder again before it is used is as
// legitimate as dropping the same file twice, and one rule is less to get
// wrong). The batch expires after g.dirTTL and is never renewed.
func (g *pathGrants) addFolders(paths []string) { g.addKeys(paths, folderGrantKey, true) }

// addKeys registers a grant for each path (keyOf; a path named twice in one
// registration counts once) as one batch: of folder grants (folder: valid
// for g.dirTTL, fixed) or of file grants (g.ttl, renewed). A path that
// already has unused grants gets one more, up to g.perKey, and moves to
// this batch (dropped last); with no room for the new paths, the oldest
// batches are dropped first.
func (g *pathGrants) addKeys(paths []string, keyOf func(string) string, folder bool) {
	if len(paths) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweepLocked(now)
	keys := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if k := keyOf(p); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for len(g.m) > 0 {
		need := 0
		for _, k := range keys {
			if g.m[k] == nil {
				need++
			}
		}
		if len(g.m)+need <= g.max {
			break
		}
		g.dropOldestLocked()
	}
	g.seq++
	b := &grantBatch{seq: g.seq, deadline: now.Add(g.ttl)}
	if folder {
		b.deadline, b.fixed = now.Add(g.dirTTL), true
	}
	for _, k := range keys {
		e := g.m[k]
		if e == nil {
			if len(g.m) >= g.max {
				break // one registration larger than the limit
			}
			g.m[k] = &grantEntry{batch: b, n: 1}
			continue
		}
		e.batch = b
		if e.n < g.perKey {
			e.n++
		}
	}
	close(g.changed)
	g.changed = make(chan struct{})
}

// sweepLocked removes expired grants.
func (g *pathGrants) sweepLocked(now time.Time) {
	for k, e := range g.m {
		if !now.Before(e.batch.deadline) {
			delete(g.m, k)
		}
	}
}

// dropOldestLocked removes every grant of the oldest batch.
func (g *pathGrants) dropOldestLocked() {
	var oldest *grantBatch
	for _, e := range g.m {
		if oldest == nil || e.batch.seq < oldest.seq {
			oldest = e.batch
		}
	}
	if oldest == nil {
		return
	}
	oldest.dropped = true
	for k, e := range g.m {
		if e.batch == oldest {
			delete(g.m, k)
		}
	}
}

// takeLocked uses up one grant of key; restore gives it back (a failed
// request). Nothing is renewed here: renew runs once the request succeeded.
func (g *pathGrants) takeLocked(key string, now time.Time) (restore func(), ok bool) {
	e := g.m[key]
	if e == nil || !now.Before(e.batch.deadline) {
		return nil, false
	}
	b := e.batch
	if e.n--; e.n <= 0 {
		delete(g.m, key)
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if b.dropped || !g.now().Before(b.deadline) {
			return
		}
		if cur := g.m[key]; cur != nil {
			if cur.n < g.perKey {
				cur.n++
			}
			return
		}
		if len(g.m) >= g.max {
			return
		}
		g.m[key] = &grantEntry{batch: b, n: 1}
	}, true
}

// renew makes every unexpired file grant valid for g.ttl from now on: a
// request that used a file grant succeeded (a queue of copies that runs for
// hours keeps the grants added behind it). Folder grants keep the expiry of
// their registration.
func (g *pathGrants) renew() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	until := now.Add(g.ttl)
	for _, e := range g.m {
		if !e.batch.fixed && now.Before(e.batch.deadline) {
			e.batch.deadline = until
		}
	}
}

// take uses up a grant of p, waiting up to g.wait for one to arrive (a
// drop's). False when there is none, or ctx is done.
func (g *pathGrants) take(ctx context.Context, p string) (restore func(), ok bool) {
	return g.takeKey(ctx, pathGrantKey(p), g.wait)
}

// takeFolder uses up a folder grant of p. No wait: the main process
// registers the folder dialog's pick before the page has the path.
func (g *pathGrants) takeFolder(ctx context.Context, p string) (restore func(), ok bool) {
	return g.takeKey(ctx, folderGrantKey(p), 0)
}

// hasFolder reports a valid folder grant of p without using it (the data
// directory check: the move that follows uses it).
func (g *pathGrants) hasFolder(p string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.m[folderGrantKey(p)]
	return e != nil && g.now().Before(e.batch.deadline)
}

// forgetFolder drops every unused folder grant of p: the user declined the
// page's confirmation of the pick (DELETE /api/app/folder-pick). A grant
// that a request is using at that moment is not here to drop; if that
// request fails it comes back with the expiry it had. The page never gives
// a pick up while it uses it.
func (g *pathGrants) forgetFolder(p string) {
	g.mu.Lock()
	delete(g.m, folderGrantKey(p))
	g.mu.Unlock()
}

func (g *pathGrants) takeKey(ctx context.Context, key string, wait time.Duration) (restore func(), ok bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		g.mu.Lock()
		restore, ok := g.takeLocked(key, g.now())
		ch := g.changed
		g.mu.Unlock()
		if ok {
			return restore, true
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// grantLedger collects the grants one request used, to give them back
// when the request fails, or to renew the store when it succeeds
// (grantScope).
type grantLedger struct {
	mu       sync.Mutex
	restores []func()
	renew    func() // set once a file grant was used
}

// succeeded renews the store if the request used a file grant.
func (l *grantLedger) succeeded() {
	l.mu.Lock()
	renew := l.renew
	l.renew, l.restores = nil, nil
	l.mu.Unlock()
	if renew != nil {
		renew()
	}
}

type grantLedgerKey struct{}

func (l *grantLedger) refund() {
	l.mu.Lock()
	rs := l.restores
	l.restores = nil
	l.mu.Unlock()
	for _, r := range rs {
		r()
	}
}

// useGrant uses up the grant of p for the request of ctx; false = none.
func (s *Server) useGrant(ctx context.Context, p string) bool {
	if s.grants == nil {
		return false
	}
	restore, ok := s.grants.take(ctx, p)
	return keepRestore(ctx, s.grants, restore, ok)
}

// useFolderGrant uses up the folder grant of p for the request of ctx. It
// earns no renewal (nil). That is on purpose, and a change from r4, where
// any used grant renewed the store: renewal is there for the upload queue
// (a copy that succeeded shows the queue is alive, so the files waiting
// behind it keep their grants), and a move or an import that starts says
// nothing about that queue. Nothing is lost: a move or import that starts
// ends with a restart, which forgets every grant; the renewal was only ever
// felt when the run then failed in the background. And it closes a way to
// keep file grants alive without the user: a script in the page holding a
// pick the user declined could have spent it on a move just for the
// renewal.
func (s *Server) useFolderGrant(ctx context.Context, p string) bool {
	if s.grants == nil {
		return false
	}
	restore, ok := s.grants.takeFolder(ctx, p)
	return keepRestore(ctx, nil, restore, ok)
}

// keepRestore records a used grant in the request's ledger: given back if
// the request fails, the store g renewed if it succeeds (g nil: a folder
// grant, which earns no renewal). Without a ledger (a direct call outside
// grantScope) nothing is renewed: renewal is earned by a request that
// answered below 400 (SEC24c; tests call renew themselves).
func keepRestore(ctx context.Context, g *pathGrants, restore func(), ok bool) bool {
	if !ok {
		return false
	}
	if l, _ := ctx.Value(grantLedgerKey{}).(*grantLedger); l != nil {
		l.mu.Lock()
		l.restores = append(l.restores, restore)
		if g != nil {
			l.renew = g.renew
		}
		l.mu.Unlock()
	}
	return true
}

// grantScope gives every desktop request that may use a grant (any method
// but GET, HEAD and OPTIONS, whatever its Content-Type: decodeJSON does not
// check it, so a text/plain body reaches the same handlers; SEC24c) a
// ledger, returns the grants it used when it answers with an error status,
// and renews the store when one that used a file grant succeeds (below 400).
// The status recorder keeps Flush, Unwrap and ReadFrom (statusWriter, the
// slow-request log wraps every request with the same), so multipart
// uploads are unaffected; event streams are GET and not wrapped.
func (s *Server) grantScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.desktop == nil || safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		l := &grantLedger{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), grantLedgerKey{}, l)))
		if sw.statusCode() >= 400 {
			l.refund() // given back, nothing renewed (SEC24b)
		} else {
			l.succeeded()
		}
	})
}

// POST /api/app/path-grants {"paths": [...], "kind": "file" | "folder"}
// (desktop main process only: PathGrantHeader) → 204. kind defaults to
// file: a path the page may copy by path (the file dialog, a drop);
// folder: the folder dialog's pick, which the data directory endpoints
// need for every path they take (handlers_datadir.go folderRefused); it
// lasts folderGrantTTL and is not renewed.
func (s *Server) postPathGrants(w http.ResponseWriter, r *http.Request) {
	d := s.desktop
	if d == nil {
		writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
		return
	}
	if !d.pathGrantOK(r.Header.Get(PathGrantHeader)) {
		writeError(w, http.StatusUnauthorized, "auth.token_required", nil)
		return
	}
	var in struct {
		Paths []string `json:"paths"`
		Kind  string   `json:"kind"`
	}
	if err := decodeJSON(r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if len(in.Paths) == 0 || len(in.Paths) > pathGrantPerPost || (in.Kind != "" && in.Kind != "file" && in.Kind != "folder") {
		writeError(w, http.StatusBadRequest, "request.invalid", nil)
		return
	}
	for _, p := range in.Paths {
		if p == "" || len(p) > pathGrantMaxBytes || strings.ContainsRune(p, 0) {
			writeError(w, http.StatusBadRequest, "request.invalid", nil)
			return
		}
	}
	if in.Kind == "folder" {
		s.grants.addFolders(in.Paths)
	} else {
		s.grants.add(in.Paths)
	}
	w.WriteHeader(http.StatusNoContent)
}
