// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/db"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
)

// stallingKV is a database a long transaction holds: every GetKV waits for
// the caller's context to end, or for release (the test's clean-up lets a
// reader without a deadline go).
type stallingKV struct {
	db.Store
	release chan struct{}
}

func (s *stallingKV) GetKV(ctx context.Context, key string) (string, bool, error) {
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case <-s.release:
	}
	return s.Store.GetKV(ctx, key)
}

// SEC52b info-9: the data folder row reads kv (the asr mode, Colab's switch,
// the first read of the remembered hardware) under credentialRows.mu, like
// the credentials row; those reads are bounded (unreadableCredentials,
// scrubPending, keyNotice: "a long transaction must not stall the page"),
// and so are these. The row still says what it can without them: python,
// which counts whatever the mode and the hardware.
func TestDataDirLocationRowBoundedKVReads(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	e.srv.comps = componentsWith(t, false)
	e.srv.cfg.Python = "" // the desktop application: no interpreter of its own
	oldLimited, oldFS := dataDirPathLimited, dataDirFSType
	dataDirPathLimited = true
	dataDirFSType = func(string) (string, bool) { return "NTFS", false }
	t.Cleanup(func() { dataDirPathLimited, dataDirFSType = oldLimited, oldFS })
	// the hardware not read yet: its first read goes to the database too
	d := e.srv.diag
	d.hwMu.Lock()
	d.hwLast, d.hwRead = nil, false
	d.hwMu.Unlock()
	stall := &stallingKV{Store: e.srv.st, release: make(chan struct{})}
	t.Cleanup(func() { close(stall.release) })
	e.srv.st = stall
	dir := shortBaseDir(t, 110)
	e.srv.dataDir = dir

	type result struct {
		c       SystemCheck
		partial bool
	}
	done := make(chan result, 1)
	go func() {
		c, _, partial := e.srv.dataDirLocationCheck()
		done <- result{c, partial}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the data folder row still waits for a stalled database after 5 s (its kv reads have no deadline)")
	}
	c := r.c
	py := components.IDPython
	if c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathComponents || !r.partial {
		t.Fatalf("row %+v, partial %v", c, r.partial)
	}
	p := c.DetailInfo.Params
	extra, _ := p["extra"].([]model.ErrorInfo)
	if p["path"] != dir || p["len"] != 110 || p["max"] != shippedLimit(t, py) ||
		len(extra) != 1 || extra[0].Code != dataDirComponentClause[py] {
		t.Fatalf("params %v, want python alone", p)
	}
}

// SEC53: once the shared deadline ran out, the values not read are not
// known: the asr mode taken as "not set" (local) would name the
// transcription components of a computer that only transcribes on Colab.
// The row then names only what holds whatever they are (python), and the
// result is not kept in the rows' cache: the next request reads again.
func TestDataDirLocationRowPartialReads(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	e.srv.comps = componentsWith(t, false)
	e.srv.cfg.Python = "" // the desktop application: no interpreter of its own
	oldLimited, oldFS := dataDirPathLimited, dataDirFSType
	dataDirPathLimited = true
	dataDirFSType = func(string) (string, bool) { return "NTFS", false }
	t.Cleanup(func() {
		dataDirPathLimited, dataDirFSType = oldLimited, oldFS
		clearCredentialRows()
	})
	ctx := context.Background()
	if err := e.st.SetKV(ctx, kvASRMode, ASRModeRemote); err != nil {
		t.Fatal(err)
	}
	// the hardware known without the database: an NVIDIA GPU (asr-cuda's
	// limit, 96, is below the 110 characters)
	d := e.srv.diag
	d.hwMu.Lock()
	d.hwLast, d.hwRead = &HardwareLast{NVIDIA: []string{"NVIDIA GeForce RTX 4060 Laptop GPU"}}, true
	d.hwMu.Unlock()
	store := e.srv.st
	stall := &stallingKV{Store: store, release: make(chan struct{})}
	t.Cleanup(func() { close(stall.release) })
	e.srv.dataDir = shortBaseDir(t, 110)
	clearCredentialRows()

	// clauses are the components the data folder row names, through the
	// rows' cache
	clauses := func(name string) []string {
		t.Helper()
		done := make(chan []SystemCheck, 1)
		go func() { done <- e.srv.credentialChecks(false) }()
		var rows []SystemCheck
		select {
		case rows = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the rows still wait after 5 s", name)
		}
		for _, c := range rows {
			if c.Name != checkDataDirLocation {
				continue
			}
			if c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathComponents {
				t.Fatalf("%s: row %+v", name, c)
			}
			extra, _ := c.DetailInfo.Params["extra"].([]model.ErrorInfo)
			var out []string
			for _, x := range extra {
				out = append(out, x.Code)
			}
			return out
		}
		t.Fatalf("%s: no data folder row in %+v", name, rows)
		return nil
	}
	py := dataDirComponentClause[components.IDPython]
	cuda := dataDirComponentClause[hwdetect.ComponentASRCUDA]

	// stalled: the asr mode (remote) is not read; asr-cuda is not named
	e.srv.st = stall
	if got := clauses("stalled"); !slices.Equal(got, []string{py}) {
		t.Fatalf("stalled: %v, want %v (python alone)", got, []string{py})
	}
	// read again at the next request, within the cache's 5 s: with the
	// database answering and the mode local, asr-cuda is named
	if err := store.SetKV(ctx, kvASRMode, ASRModeLocal); err != nil {
		t.Fatal(err)
	}
	e.srv.st = store
	if got := clauses("answering"); !slices.Equal(got, []string{py, cuda}) {
		t.Fatalf("answering: %v, want %v (not the stalled result from the cache)", got, []string{py, cuda})
	}
}
