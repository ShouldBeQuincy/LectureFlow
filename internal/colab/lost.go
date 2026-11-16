// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// Lost runtimes (security review SEC2 H1): runtimes no session of the
// backend stands for any more, which could not be released when that was
// found out. A runtime assigned to a session that was released while the
// assignment was on its way, whose release then failed; a session whose
// "was a runtime assigned?" question Colab could not answer; the record of
// a session not released yet when a new session is borrowed (one record
// fits in colab.session). They are kept in kv colab.lost, apart from the
// session record, so that neither overwrites the other; Tick tries them
// again, Close and ImportADC too, the next start does, and the card shows
// them as a leftover meanwhile (Status.Leftover).

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"lectureflow/internal/model"
)

// lostRuntime is one runtime to release. Endpoint "" = not known: Colab is
// asked with the notebook hash whether one was assigned.
type lostRuntime struct {
	Endpoint     string    `json:"endpoint,omitempty"`
	NotebookHash string    `json:"nbh,omitempty"`
	Accelerator  string    `json:"accelerator"`
	Shape        string    `json:"shape,omitempty"`
	Since        time.Time `json:"since"`
}

func lostOf(rec sessionRecord) lostRuntime {
	return lostRuntime{Endpoint: rec.Endpoint, NotebookHash: rec.NotebookHash, Accelerator: rec.Accelerator,
		Shape: rec.Shape, Since: rec.RequestedAt}
}

func (l lostRuntime) same(o lostRuntime) bool {
	if l.Endpoint != "" || o.Endpoint != "" {
		return l.Endpoint == o.Endpoint
	}
	return l.NotebookHash == o.NotebookHash
}

// lostCodeOf is the leftover's code for a failed release: no usable
// credential, or the release failed.
func lostCodeOf(err error) string {
	if WaitingFor(err) == model.WaitRemoteAuth {
		return CodeLeftoverSession
	}
	return CodeReleaseFailed
}

// addLostLocked adds l (b.mu held); err (may be nil) is why it could not
// be released.
func (b *Backend) addLostLocked(l lostRuntime, err error) {
	if l.Endpoint == "" && l.NotebookHash == "" {
		return
	}
	for i, x := range b.lost {
		if x.same(l) || x.Endpoint == "" && l.Endpoint != "" && x.NotebookHash == l.NotebookHash {
			b.lost[i] = l // the endpoint is known now
			if err != nil {
				b.lostCode = lostCodeOf(err)
			}
			return
		}
	}
	b.lost = append(b.lost, l)
	if err != nil {
		b.lostCode = lostCodeOf(err)
	}
}

// addLost keeps l for a later release and tells the card.
func (b *Backend) addLost(ctx context.Context, l lostRuntime, err error) {
	b.mu.Lock()
	b.addLostLocked(l, err)
	b.mu.Unlock()
	b.saveLost(ctx)
	b.changed()
}

// saveLost writes the lost runtimes (kv colab.lost; "" when none).
func (b *Backend) saveLost(ctx context.Context) {
	b.recMu.Lock()
	defer b.recMu.Unlock()
	b.saveLostRecLocked(ctx)
}

// saveLostRecLocked is saveLost with recMu held: the list as it is now, so
// the last write is always the latest list.
func (b *Backend) saveLostRecLocked(ctx context.Context) {
	b.mu.Lock()
	v := ""
	if len(b.lost) > 0 {
		raw, _ := json.Marshal(b.lost)
		v = string(raw)
	}
	b.mu.Unlock()
	if err := b.o.KV.SetKV(context.WithoutCancel(ctx), KVLost, v); err != nil {
		b.log.Warn("colab: saving the runtimes to release", "err", err)
	}
}

// loadLost reads the lost runtimes an earlier run left (Startup).
func (b *Backend) loadLost(ctx context.Context) {
	v, ok, err := getKV(ctx, b.o.KV, KVLost)
	if err != nil || !ok {
		return
	}
	var list []lostRuntime
	if json.Unmarshal([]byte(v), &list) != nil {
		b.log.Warn("colab: unreadable list of runtimes to release dropped")
		_ = deleteKV(ctx, b.o.KV, KVLost)
		return
	}
	b.mu.Lock()
	for _, l := range list {
		b.addLostLocked(l, nil)
	}
	b.mu.Unlock()
}

// releaseLost releases l: the endpoint when it is still the account's, or
// whatever Colab assigned to its notebook hash.
func (b *Backend) releaseLost(ctx context.Context, l lostRuntime) error {
	if l.Endpoint != "" {
		return b.unassignIfListed(ctx, l.Endpoint)
	}
	if l.NotebookHash == "" {
		return nil
	}
	a, _, ok, err := b.tun.getAssign(ctx, l.NotebookHash, l.Accelerator, l.Shape, b.o.AllowHTTP)
	if err != nil {
		return codedHTTP("tun/m/assign", err)
	}
	if !ok {
		return nil
	}
	return b.unassignIfListed(ctx, a.Endpoint)
}

// retryLost tries to release every lost runtime once (Tick, Startup,
// ImportADC, Close). One run at a time; runtimes added meanwhile wait for
// the next.
func (b *Backend) retryLost(ctx context.Context) {
	b.lostMu.Lock()
	defer b.lostMu.Unlock()
	b.mu.Lock()
	todo := slices.Clone(b.lost)
	b.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	var done []lostRuntime
	var lastErr error
	if _, err := b.auth.remaining(ctx); err != nil {
		lastErr = err // nothing can be released without a credential
	} else {
		for _, l := range todo {
			if err := b.releaseLost(ctx, l); err != nil {
				lastErr = err
				continue
			}
			done = append(done, l)
		}
	}
	b.mu.Lock()
	b.lost = slices.DeleteFunc(b.lost, func(x lostRuntime) bool {
		return slices.ContainsFunc(done, func(d lostRuntime) bool { return d == x })
	})
	if lastErr != nil {
		b.lostCode = lostCodeOf(lastErr)
	}
	left := len(b.lost)
	if left == 0 {
		b.lostCode = ""
	}
	b.mu.Unlock()
	if len(done) > 0 {
		b.saveLost(ctx)
		b.log.Info("colab: runtimes of sessions already released were given back", "count", len(done), "left", left)
		b.changed()
	}
	if lastErr != nil {
		b.log.Warn("colab: releasing a runtime of a session already released failed; will retry", "code", Code(lastErr),
			"left", left)
	}
}

// adoptRecord moves a session record no live session stands for (a
// release that waits for a credential, or a start that could not release
// an earlier run's session) to the lost runtimes, so that Tick tries it
// again instead of only the next import or start (Tick).
func (b *Backend) adoptRecord(ctx context.Context) {
	b.mu.Lock()
	idle := b.sess == nil && b.leftover != nil
	b.mu.Unlock()
	if !idle {
		return
	}
	b.recMu.Lock()
	defer b.recMu.Unlock()
	rec, err := b.loadRecord(ctx)
	if err != nil || rec == nil {
		return
	}
	b.mu.Lock()
	if b.sess != nil {
		b.mu.Unlock()
		return
	}
	b.addLostLocked(lostOf(*rec), nil)
	if b.leftover != nil {
		b.lostCode = b.leftover.Code
	}
	b.leftover = nil // the lost runtime stands for it on the card now (leftoverLocked)
	b.mu.Unlock()
	b.saveLostRecLocked(ctx)
	if err := deleteKV(context.WithoutCancel(ctx), b.o.KV, KVSession); err != nil {
		b.log.Warn("colab: deleting the session record", "err", err)
	}
}

// leftoverLocked is Status.Leftover: a session of an earlier run that
// could not be released, else the first lost runtime.
func (b *Backend) leftoverLocked() *LeftoverInfo {
	if b.leftover != nil || len(b.lost) == 0 {
		return b.leftover
	}
	code := b.lostCode
	if code == "" {
		code = CodeReleaseFailed
	}
	l := b.lost[0]
	return &LeftoverInfo{Accelerator: l.Accelerator, Since: l.Since, Code: code}
}
