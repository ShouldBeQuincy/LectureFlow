// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
)

// raceWait bounds every wait of these tests: code that deadlocks or waits
// on the wrong lock fails quickly instead of hanging until go test's
// timeout (safety rule 5).
const raceWait = 10 * time.Second

// raceClient never waits longer than the tests do.
var raceClient = &http.Client{Timeout: raceWait + 5*time.Second}

type raceResult struct {
	status int
	body   []byte
	err    error
}

// raceDo sends one request in the background; the result arrives on the
// channel (bounded by raceClient's timeout).
func raceDo(method, url string, body any) <-chan raceResult {
	out := make(chan raceResult, 1)
	go func() {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				out <- raceResult{err: err}
				return
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, url, rd)
		if err != nil {
			out <- raceResult{err: err}
			return
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := raceClient.Do(req)
		if err != nil {
			out <- raceResult{err: err}
			return
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		out <- raceResult{status: res.StatusCode, body: b, err: err}
	}()
	return out
}

// raceGet waits (bounded) for a result; ok false when none came in time.
func raceGet(ch <-chan raceResult) (raceResult, bool) {
	select {
	case r := <-ch:
		return r, true
	case <-time.After(raceWait):
		return raceResult{}, false
	}
}

// releaser closes a channel once, however many paths do it.
type releaser struct {
	ch   chan struct{}
	once sync.Once
}

func newReleaser() *releaser { return &releaser{ch: make(chan struct{})} }
func (r *releaser) free()    { r.once.Do(func() { close(r.ch) }) }

// blockingProbe is an env whose model probe reports each call on started
// and then waits for release (or the request's end); its answer names the
// address it was made against.
func blockingProbe(t *testing.T) (*env, <-chan model.Provider, *releaser) {
	t.Helper()
	started, release := make(chan model.Provider, 4), newReleaser()
	e := newEnv(t, func(s *Server) {
		s.probe = func(ctx context.Context, p model.Provider) ([]string, error) {
			started <- p
			select {
			case <-release.ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []string{"model-of-" + p.BaseURL}, nil
		}
	})
	t.Cleanup(release.free) // before the env's cleanup closes the server (LIFO)
	return e, started, release
}

// probeStarted waits (bounded) for the fake probe to be called.
func probeStarted(t *testing.T, started <-chan model.Provider, release *releaser) model.Provider {
	t.Helper()
	select {
	case p := <-started:
		return p
	case <-time.After(raceWait):
		release.free()
		t.Fatal("the probe did not start")
	}
	return model.Provider{}
}

// r4 D3: the model probe runs while the user moves the provider to another
// address; when its result arrives it belongs to the old address and must
// not be written back (the page would show the old list again). The fake
// probe blocks until the test releases it, so "late" is exact, not a sleep.
func TestProbeResultOfOldAddressIsDropped(t *testing.T) {
	e, started, release := blockingProbe(t)
	wait := func(ch <-chan raceResult) ProviderModels {
		t.Helper()
		r, ok := raceGet(ch)
		if !ok {
			release.free()
			t.Fatal("probe request did not finish")
		}
		if r.err != nil || r.status != http.StatusOK {
			t.Fatalf("probe: %d %v %s", r.status, r.err, r.body)
		}
		var pm ProviderModels
		decodeStrict(t, r.body, &pm)
		return pm
	}
	deepseek := func() ProviderView {
		t.Helper()
		var list []ProviderView
		e.call("GET", "/api/providers", nil, http.StatusOK, &list)
		for _, p := range list {
			if p.ID == "deepseek" {
				return p
			}
		}
		t.Fatal("provider deepseek missing")
		return ProviderView{}
	}
	probeURL := e.ts.URL + "/api/providers/deepseek/models"

	pending := raceDo("POST", probeURL, nil)
	old := probeStarted(t, started, release)
	const newBase = "https://api.example.org/v1"
	// Bounded: a probe that held the settings' write lock would block it.
	patch, ok := raceGet(raceDo("PATCH", e.ts.URL+"/api/providers/deepseek", map[string]any{"base_url": newBase}))
	if !ok || patch.err != nil || patch.status != http.StatusOK {
		release.free()
		t.Fatalf("PATCH while the probe runs: ok %v, %d %v %s", ok, patch.status, patch.err, patch.body)
	}
	release.free() // the old address answers now
	pm := wait(pending)
	stale := "model-of-" + old.BaseURL
	if slices.Contains(pm.Models, stale) {
		t.Fatalf("the response carries the old address's list: %+v", pm)
	}
	if v := deepseek(); slices.Contains(v.Models, stale) || v.ModelsProbedAt != nil {
		t.Fatalf("the old address's list was written back: models %v, probed_at %v", v.Models, v.ModelsProbedAt)
	}

	// A probe of the current address is stored as before.
	pm = wait(raceDo("POST", probeURL, nil))
	probeStarted(t, started, release)
	if want := "model-of-" + newBase; !slices.Equal(pm.Models, []string{want}) || pm.ProbedAt == "" {
		t.Fatalf("probe of the current address: %+v", pm)
	}
	if v := deepseek(); !slices.Equal(v.Models, []string{"model-of-" + newBase}) || v.ModelsProbedAt == nil {
		t.Fatalf("current probe not stored: %+v", v)
	}
}

// r4 D3: a provider deleted while its probe runs answers 404
// provider.not_found, and no cache row is left for it.
func TestProbeOfDeletedProviderIsNotFound(t *testing.T) {
	e, started, release := blockingProbe(t)
	var p ProviderView
	e.call("POST", "/api/providers", map[string]any{"name": "Spare", "type": model.ProviderOpenAI,
		"base_url": "https://spare.example.test/v1", "api_key": "sk-test-spare-0000"}, http.StatusCreated, &p)
	pending := raceDo("POST", e.ts.URL+"/api/providers/"+p.ID+"/models", nil)
	probeStarted(t, started, release)
	del, ok := raceGet(raceDo("DELETE", e.ts.URL+"/api/providers/"+p.ID, nil))
	if !ok || del.err != nil || del.status != http.StatusOK {
		release.free()
		t.Fatalf("DELETE while the probe runs: ok %v, %d %v %s", ok, del.status, del.err, del.body)
	}
	release.free()
	r, ok := raceGet(pending)
	if !ok {
		t.Fatal("probe request did not finish")
	}
	var eb ErrorBody
	if r.err != nil || r.status != http.StatusNotFound {
		t.Fatalf("probe of a deleted provider: %d %v %s", r.status, r.err, r.body)
	}
	decodeStrict(t, r.body, &eb)
	if eb.Error.Code != "provider.not_found" {
		t.Fatalf("probe of a deleted provider: %+v", eb.Error)
	}
	if _, cached, err := e.set.ProbedModels(context.Background(), p.ID); cached || err != nil {
		t.Fatalf("a deleted provider got a probe cache: %v %v", cached, err)
	}
}

// gatedBody holds the first read of a request body until release is
// closed, after telling the test (reading) that the handler got there.
type gatedBody struct {
	io.ReadCloser
	once    sync.Once
	reading chan<- struct{}
	release <-chan struct{}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	g.once.Do(func() {
		close(g.reading)
		<-g.release
	})
	return g.ReadCloser.Read(p)
}

// putWhileHeld sends PUT path first and holds it where its handler reads
// the body; meanwhile PUT path second runs to completion; then the first
// one goes on. Every wait is bounded: a second PUT that waits for the
// first (a handler reading its body under the settings' write lock) fails
// the test at once, releasing the first.
func putWhileHeld(t *testing.T, e *env, path string, first, second any) {
	t.Helper()
	reading, release := make(chan struct{}), newReleaser()
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &gatedBody{ReadCloser: r.Body, reading: reading, release: release.ch}
		e.ts.Config.Handler.ServeHTTP(w, r)
	}))
	defer gated.Close()
	defer release.free() // runs before gated.Close, which waits for the held request

	firstDone := raceDo("PUT", gated.URL+path, first)
	select {
	case <-reading:
	case <-time.After(raceWait):
		t.Fatalf("the first PUT %s did not reach its body", path)
	}
	r, ok := raceGet(raceDo("PUT", e.ts.URL+path, second))
	if !ok {
		release.free()
		t.Fatalf("the second PUT %s kept waiting for the first: its handler holds the settings' lock while it reads the body", path)
	}
	if r.err != nil || r.status != http.StatusOK {
		t.Fatalf("second PUT %s: %d %v %s", path, r.status, r.err, r.body)
	}
	release.free()
	r, ok = raceGet(firstDone)
	if !ok {
		t.Fatalf("the first PUT %s did not finish", path)
	}
	if r.err != nil || r.status != http.StatusOK {
		t.Fatalf("first PUT %s: %d %v %s", path, r.status, r.err, r.body)
	}
}

// r4 D9: PUT /api/settings/general merges the body into the current
// settings. Two pages saving different fields at the same time must both
// keep their change. Before the fix the first handler had already copied
// the settings (without the second page's field) and stored that copy
// over it.
func TestPutGeneralConcurrentFieldsBothKept(t *testing.T) {
	e := newEnv(t)
	putWhileHeld(t, e, "/api/settings/general", map[string]any{"daily_budget": 7}, map[string]any{"auto_chapters": false})
	g := e.set.General()
	if g.DailyBudget != 7 || g.AutoChaptersOn() {
		t.Fatalf("general %+v: want daily_budget 7 and auto_chapters off (both changes kept)", g)
	}
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if sv.General.DailyBudget != 7 || sv.General.AutoChapters == nil || *sv.General.AutoChapters {
		t.Fatalf("GET general %+v", sv.General)
	}
	// A bad body is still request.bad_json and changes nothing.
	if info := e.callErrInfo("PUT", "/api/settings/general", "not an object", http.StatusBadRequest); info.Code != "request.bad_json" {
		t.Fatalf("bad body: %+v", info)
	}
	if g2 := e.set.General(); g2.DailyBudget != 7 || g2.AutoChaptersOn() {
		t.Fatalf("a bad body changed the settings: %+v", g2)
	}
}

// r4 D9: the same for PUT /api/settings/asr (merged under the settings'
// write lock as well).
func TestPutASRConcurrentFieldsBothKept(t *testing.T) {
	e := newEnv(t)
	if a := e.set.ASR(); a.MaxParallel == 3 || a.Language == "de" {
		t.Fatalf("the test needs other start values: %+v", a)
	}
	putWhileHeld(t, e, "/api/settings/asr", map[string]any{"max_parallel": 3}, map[string]any{"language": "de"})
	if a := e.set.ASR(); a.MaxParallel != 3 || a.Language != "de" {
		t.Fatalf("asr %+v: want max_parallel 3 and language de (both changes kept)", a)
	}
	var sv Settings
	e.call("GET", "/api/settings", nil, http.StatusOK, &sv)
	if sv.ASR.MaxParallel != 3 || sv.ASR.Language != "de" {
		t.Fatalf("GET asr %+v", sv.ASR)
	}
	if info := e.callErrInfo("PUT", "/api/settings/asr", "not an object", http.StatusBadRequest); info.Code != "request.bad_json" {
		t.Fatalf("bad body: %+v", info)
	}
	if a := e.set.ASR(); a.MaxParallel != 3 || a.Language != "de" {
		t.Fatalf("a bad body changed the settings: %+v", a)
	}
}
