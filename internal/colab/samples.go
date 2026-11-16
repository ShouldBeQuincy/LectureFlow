// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// The runtime's own readings (colab-obs-core; colab-telemetry.md §3, the
// lead's ruling of 2026-10-27 14:30: plan A + B):
//
//	A: the attaching process of a lecture (worker/colab_remote.py) sends a
//	   machine event and then a sample every 2 s on the lecture's event
//	   stream (one attach per runtime at a time: BASE/sample.lock);
//	B: while nothing came from A for noWorkerSample, a front end watches
//	   (SetViewers) and the session runs, the runtime's own
//	   GET /api/colab/resources every pollEvery (total and free memory, the
//	   GPU's memory and use; no CPU, no segments). A failure is only
//	   logged: never CodeAPIChanged, never a hand-back.
//
// Both go into Backend.samples (remote.Samples: the history by the time
// this machine received them); the release forgets them, the account
// stays.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"lectureflow/internal/remote"
)

const (
	pollEvery      = 5 * time.Second
	noWorkerSample = 6 * time.Second
	pollTimeout    = 10 * time.Second
	// resourcesMaxBytes bounds the resources answer (about 300 bytes on a
	// T4; security review 3 L5: not the 512 MiB of a download, every 5 s);
	// a longer one is no reading this time (logged).
	resourcesMaxBytes = 64 << 10
	// resourcesMaxGPUs: only the first gpus entries are read (remote.MaxGPUs).
	resourcesMaxGPUs = remote.MaxGPUs
)

// SetViewers tells the backend how many front ends watch the resource bar
// (resources.Service.Clients): plan B polls only while there is one.
func (b *Backend) SetViewers(n func() int) {
	b.mu.Lock()
	b.viewers = n
	b.mu.Unlock()
}

// noteReading takes a machine or sample event of a lecture of session s.
func (b *Backend) noteReading(s *liveSession, ev remoteEvent) {
	b.mu.Lock()
	live := b.sess == s
	b.mu.Unlock()
	if !live {
		return
	}
	b.noteReach(s, true) // its events arrive: the runtime answers
	switch ev.Type {
	case remote.EventMachine:
		b.samples.SetMachine(ev.Machine.Machine(remote.SourceNvidiaSMI))
	case remote.EventSample:
		b.samples.Add(b.now(), ev.Sample.Usage())
	}
}

// machineLocked is Status.Machine of session s: the reported machine with
// the accelerator asked for and where the files are (the memory probe's
// decision once the runtime is prepared).
func (b *Backend) machineLocked(s *liveSession) *remote.Machine {
	m := b.samples.Machine()
	if m == nil {
		return nil
	}
	m.Requested = s.rec.Accelerator
	if p := s.prep; p != nil && isClosed(p.done) && p.err == nil {
		switch {
		case p.memory == nil:
			m.Storage = remote.StorageDisk
		case p.memory.memory():
			m.Storage = remote.StorageMemory
		default:
			m.Storage = remote.StorageDiskFallback
		}
	}
	return m
}

// pollResources is plan B, once (Run calls it every pollEvery).
func (b *Backend) pollResources(ctx context.Context) {
	b.mu.Lock()
	s, viewers := b.sess, b.viewers
	b.mu.Unlock()
	if viewers == nil || viewers() <= 0 || s == nil {
		return
	}
	b.mu.Lock()
	running := b.sess == s && s.rec.State == StateRunning && s.rt != nil
	b.mu.Unlock()
	if !running || b.now().Sub(b.samples.LastFrom(remote.SourceWorker)) < noWorkerSample {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	rt, err := b.runtime(pctx, s)
	var r runtimeResources
	if err == nil {
		r, err = rt.colabResources(pctx)
	}
	b.mu.Lock()
	live, logged := b.sess == s, b.pollFail
	b.pollFail = err != nil
	b.mu.Unlock()
	// Status.Runtime (retry.go): the runtime answered, or failed in the way
	// a lost connection does. Any other failure (the endpoint is not
	// public: it may be gone) says nothing.
	if err == nil {
		b.noteReach(s, true)
	} else if ctx.Err() == nil && Code(err) == "" && transient(err) {
		b.noteReach(s, false)
	}
	if err != nil {
		if !logged && ctx.Err() == nil {
			b.log.Info("colab: reading the runtime's resources failed (the resource bar shows the last reading)",
				"code", Code(codedHTTP("api/colab/resources", err)))
		}
		return
	}
	if !live {
		return
	}
	if r.machine != nil {
		b.samples.SetMachine(*r.machine)
	}
	b.samples.Add(b.now(), r.usage)
}

// runtimeResources is one answer of the runtime's resources endpoint.
type runtimeResources struct {
	usage   remote.Usage
	machine *remote.Machine
}

// colabResources is GET /api/colab/resources: Colab's own RAM / disk
// widget's endpoint (colab-vscode reads it too; not public): memory
// {totalBytes, freeBytes ≈ MemAvailable}, gpus [{name, memoryTotalBytes,
// memoryUsedBytes (no driver reserve), gpuUtilization 0–1}]. Read
// leniently: numbers may be strings, a missing value is unknown.
func (r *runtimeClient) colabResources(ctx context.Context) (runtimeResources, error) {
	const ep = "api/colab/resources"
	st, body, err := r.requestLimit(r.hc, ctx, http.MethodGet, "/api/colab/resources", nil, r.kernelHeaders(), nil, pollTimeout, resourcesMaxBytes)
	if err != nil {
		return runtimeResources{}, err
	}
	if st != http.StatusOK {
		return runtimeResources{}, runtimeStatusErr(ep, st)
	}
	var doc struct {
		Memory map[string]json.RawMessage   `json:"memory"`
		GPUs   []map[string]json.RawMessage `json:"gpus"`
	}
	body = bytes.TrimPrefix(bytes.TrimSpace(body), []byte(strings.TrimSpace(xssiPrefix))) // Colab's XSSI guard, if any
	if json.Unmarshal(body, &doc) != nil {
		return runtimeResources{}, fmt.Errorf("%s: the answer is not a resources object", ep)
	}
	mb := func(m map[string]json.RawMessage, k string) int64 {
		f, ok := lenientNumber(m[k])
		if !ok || f < 0 || f/(1<<20) > remote.MaxMB {
			return -1
		}
		return int64(f / (1 << 20))
	}
	out := runtimeResources{usage: remote.Usage{Source: remote.SourceColabResources, RAM: []remote.RAMSegment{}}}
	total, free := mb(doc.Memory, "totalBytes"), mb(doc.Memory, "freeBytes")
	if total > 0 && free >= 0 {
		out.usage.RAM = remote.UsedFree(total, free)
	}
	m := remote.Machine{Source: remote.SourceColabResources}
	if total > 0 {
		t := int(total)
		m.MemTotalMB = &t
	}
	if len(doc.GPUs) > resourcesMaxGPUs {
		doc.GPUs = doc.GPUs[:resourcesMaxGPUs]
	}
	n := len(doc.GPUs)
	m.GPUCount = &n
	if n > 0 {
		g := doc.GPUs[0]
		var name string
		if json.Unmarshal(g["name"], &name) == nil && remote.GPUNamePattern.MatchString(name) {
			m.GPUName = name
		}
		if v := mb(g, "memoryTotalBytes"); v >= 0 {
			t := int(v)
			m.VRAMTotalMB = &t
		}
		if v := mb(g, "memoryUsedBytes"); v >= 0 {
			u := int(v)
			out.usage.VRAMUsedMB = &u
		}
		if f, ok := lenientNumber(g["gpuUtilization"]); ok && f >= 0 && f <= 1 {
			p := int(math.Round(f * 100))
			out.usage.GPUUtilPct = &p
		}
	}
	out.machine = &m
	return out, nil
}
