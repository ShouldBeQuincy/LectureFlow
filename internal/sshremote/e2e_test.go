// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// End to end over a real SSH connection to the in-process server, with the
// real lf-agent and the fake Python: adding a server, confirming its host
// key, installing agent and kit from the fake sources, a lecture, a cancel,
// a lost connection resumed from the checkpoint, and nothing left behind.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// addTrusted adds the fake server, authorizes its key and confirms its host key.
func addTrusted(t *testing.T, e *testEnv, f *fakeRemote) ServerView {
	t.Helper()
	ctx := context.Background()
	host, port := f.addr()
	v, err := e.m.Add(ctx, AddRequest{Name: "Test Mac", Host: host, Port: port, Username: "alice", Group: GroupPrefer})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.PublicKey, "restrict ssh-ed25519 ") || v.State != StateUntrusted {
		t.Fatalf("new server: %+v", v)
	}
	f.authorize(v.PublicKey)
	res, err := e.m.Connect(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.HostKey == nil || !strings.HasPrefix(res.HostKey.Fingerprint, "SHA256:") {
		t.Fatalf("connect without a confirmed key shows it: %+v", res)
	}
	if len(f.sentCommands()) != 0 {
		t.Fatalf("commands ran before the host key was confirmed: %v", f.sentCommands())
	}
	if _, err := e.m.Trust(ctx, v.ID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: "SHA256:wrong"}); err == nil {
		t.Fatal("a wrong fingerprint was accepted")
	}
	res, err = e.m.Trust(ctx, v.ID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: res.HostKey.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Server.Trusted || res.Server.State != StateSetup || res.Server.Platform != f.platform {
		t.Fatalf("after trust: %+v", res.Server)
	}
	return res.Server
}

func waitState(t *testing.T, e *testEnv, id string, want ...string) ServerView {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		v, _ := e.m.Get(id)
		if slices.Contains(want, v.State) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %s (last error %+v), want %v", v.State, v.LastError, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func install(t *testing.T, e *testEnv, id string) {
	t.Helper()
	if err := e.m.Install(id); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, e, id, StateReady, StateSetup, StateError)
	if v.State != StateReady || !v.KitReady {
		t.Fatalf("install: %+v", v)
	}
}

func setBehaviour(t *testing.T, f *fakeRemote, v map[string]any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(filepath.Join(f.home, "fakepython.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mlxParams() map[string]any {
	return map[string]any{"model": "large-v3-turbo", "precision": "fp16", "device": "mlx:gpu", "language": "en",
		"initial_prompt": "LectureFlow, CSC3631", "word_timestamps": true,
		"checkpoint": map[string]any{"job_id": "01JOBTEST0000000000000000A", "input_hash": strings.Repeat("ab", 32),
			"device_id": "ssh:test-mac", "model": "large-v3-turbo", "precision": "fp16", "chunk_s": 600}}
}

func testJob(t *testing.T, id string, engine string) remote.Job {
	t.Helper()
	dir := t.TempDir()
	audio := filepath.Join(dir, "audio.wav")
	writeWAV(t, audio, 3)
	p := mlxParams()
	if engine == remote.EngineFasterWhisper {
		p = map[string]any{"compute_type": "float16", "device": "cuda", "language": "en", "initial_prompt": "x",
			"cpu_threads": 4, "timing": true, "redecode": true}
	}
	m, prec := "large-v3-turbo", model.PrecisionFP16
	if engine == remote.EngineFasterWhisper {
		m, prec = "large-v3", model.PrecisionAuto
	}
	return remote.Job{ID: id, LectureID: "lec1", AudioPath: audio, DurationSec: 3, Engine: engine, Model: m, Precision: prec,
		Params: p, OutDir: dir}
}

// jobsLeft lists the job directories on the remote.
func jobsLeft(t *testing.T, f *fakeRemote) []string {
	t.Helper()
	ents, err := os.ReadDir(f.jobsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func waitNoJobs(t *testing.T, f *fakeRemote) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(jobsLeft(t, f)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("remote job directories left: %v", jobsLeft(t, f))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type eventLog struct {
	mu  sync.Mutex
	evs []remote.Event
}

func (l *eventLog) add(ev remote.Event) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.mu.Unlock()
}

func (l *eventLog) steps() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.evs {
		if len(out) == 0 || out[len(out)-1] != e.Step {
			out = append(out, e.Step)
		}
	}
	return out
}

func TestEndToEndMLXLecture(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v := addTrusted(t, e, f)
	// The device row exists with the engine of the platform.
	d, ok := e.devs.Get(v.DeviceID)
	if !ok || d.Backend != model.BackendMLX || d.Model == "" || d.Precision != model.PrecisionFP16 {
		t.Fatalf("device row: %+v %v", d, ok)
	}
	if e.devs.Present(v.DeviceID) {
		t.Fatal("present before the environment is installed")
	}
	dev, _ := e.m.Device(v.DeviceID)
	if ok, wf := dev.Ready(context.Background(), 60); ok || wf != model.WaitRemoteSetup {
		t.Fatalf("ready before install: %v %q", ok, wf)
	}
	install(t, e, v.ID)
	if !e.devs.Present(v.DeviceID) {
		t.Fatal("not present after install")
	}
	if ok, wf := dev.Ready(context.Background(), 60); !ok {
		t.Fatalf("not ready after install: %q", wf)
	}
	setBehaviour(t, f, map[string]any{"mode": "ok", "steps": 5, "step_ms": 20, "sentences": 4})
	job := testJob(t, "01JOBTEST0000000000000000A", remote.EngineMLX)
	var log eventLog
	path, err := dev.Transcribe(context.Background(), job, log.add)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	ss, _ := res["sentences"].([]any)
	if len(ss) != 4 || strings.Contains(string(raw), "extra_key") || strings.Contains(string(raw), "resumed_from") {
		t.Fatalf("result not checked and cleaned: %s", raw)
	}
	if got := log.steps(); !slices.Equal(got, []string{remote.StepConnect, remote.StepUpload, remote.StepTranscribe,
		remote.StepDownload, remote.StepVerify}) {
		t.Fatalf("steps %v", got)
	}
	waitNoJobs(t, f)
	// Only the allowed commands ran: uname, the install script, the agent.
	for _, c := range f.sentCommands() {
		if c != unameCommand && !strings.HasPrefix(c, "sh -c '") {
			t.Fatalf("unexpected command %q", c)
		}
	}
	// Card while idle.
	c := dev.Card(context.Background())
	if c.Kind != remote.KindSSH || c.ID != v.DeviceID || c.SSH == nil || c.Slots != 1 {
		t.Fatalf("card %+v", c)
	}
	// Removing with uninstall leaves nothing of the agent.
	if err := e.m.Remove(context.Background(), v.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.agentRoot()); !os.IsNotExist(err) {
		t.Fatalf("agent directory left after uninstall: %v", err)
	}
	if _, err := os.Stat(f.jobsDir()); !os.IsNotExist(err) {
		t.Fatalf("job directory left after uninstall: %v", err)
	}
	if _, ok, _ := e.st.GetKV(context.Background(), KVKeyPrefix+v.ID); ok {
		t.Fatal("the private key is left in kv")
	}
	if _, ok := e.devs.Get(v.DeviceID); ok {
		t.Fatal("the device row is left")
	}
}

func TestEndToEndCancelWithin3s(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v := addTrusted(t, e, f)
	install(t, e, v.ID)
	setBehaviour(t, f, map[string]any{"mode": "ok", "steps": 1000, "step_ms": 50})
	dev, _ := e.m.Device(v.DeviceID)
	ctx, cancel := context.WithCancel(context.Background())
	job := testJob(t, "01JOBTEST0000000000000000B", remote.EngineMLX)
	running := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		_, err := dev.Transcribe(ctx, job, func(ev remote.Event) {
			if ev.Step == remote.StepTranscribe && ev.Stage == remote.StageTranscribe {
				once.Do(func() { close(running) })
			}
		})
		done <- err
	}()
	select {
	case <-running:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not start")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if took := time.Since(start); took > remote.CancelWithin {
			t.Fatalf("cancel took %v", took)
		}
	case <-time.After(remote.CancelWithin + time.Second):
		t.Fatal("the cancel did not return within 3 s")
	}
	waitNoJobs(t, f)
}

func TestEndToEndReconnectResumes(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v := addTrusted(t, e, f)
	install(t, e, v.ID)
	setBehaviour(t, f, map[string]any{"mode": "ok", "steps": 40, "step_ms": 50, "sentences": 2})
	dev, _ := e.m.Device(v.DeviceID)
	job := testJob(t, "01JOBTEST0000000000000000C", remote.EngineMLX)
	var dropped sync.Once
	var log eventLog
	path, err := dev.Transcribe(context.Background(), job, func(ev remote.Event) {
		log.add(ev)
		if ev.Stage == remote.StageTranscribe && ev.Fraction >= 0.25 {
			dropped.Do(func() { go f.dropAll() })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	reconnected := false
	for _, ev := range log.evs {
		reconnected = reconnected || ev.Stage == remote.StageReconnect
	}
	if !reconnected {
		t.Fatal("no reconnect was reported")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"parts"`) {
		t.Fatalf("result: %s", raw)
	}
	waitNoJobs(t, f)
}

func TestEndToEndResultRefused(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v := addTrusted(t, e, f)
	install(t, e, v.ID)
	dev, _ := e.m.Device(v.DeviceID)
	for _, mode := range []string{"bad_result", "huge"} {
		setBehaviour(t, f, map[string]any{"mode": mode, "steps": 2, "step_ms": 10})
		job := testJob(t, "01JOBTEST000000000000000D"+map[string]string{"bad_result": "1", "huge": "2"}[mode], remote.EngineMLX)
		_, err := dev.Transcribe(context.Background(), job, nil)
		if !remote.IsFallback(err) {
			t.Fatalf("%s: %v is not a fallback", mode, err)
		}
		if _, err := os.Stat(filepath.Join(job.OutDir, "result.json")); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused result was written", mode)
		}
		waitNoJobs(t, f)
	}
	// A worker error is a fallback with its code.
	setBehaviour(t, f, map[string]any{"mode": "device"})
	_, err := dev.Transcribe(context.Background(), testJob(t, "01JOBTEST0000000000000000E", remote.EngineMLX), nil)
	if remote.CodeOf(err) != remote.CodeDeviceUnusable || !remote.IsFallback(err) {
		t.Fatalf("device error: %v (%s)", err, remote.CodeOf(err))
	}
	waitNoJobs(t, f)
}

func TestEndToEndFasterWhisperUsesFLAC(t *testing.T) {
	f := newFakeRemote(t, "Linux x86_64")
	var encoded int
	e := newTestEnv(t, func(o *Options) {
		o.EncodeFLAC = func(ctx context.Context, in, out string) error {
			encoded++
			b, err := os.ReadFile(in)
			if err != nil {
				return err
			}
			return os.WriteFile(out, append([]byte("fLaC"), b...), 0o600)
		}
	})
	v := addTrusted(t, e, f)
	if d, _ := e.devs.Get(v.DeviceID); d.Backend != model.BackendFasterWhisper {
		t.Fatalf("linux row backend %q", d.Backend)
	}
	install(t, e, v.ID)
	setBehaviour(t, f, map[string]any{"mode": "ok", "steps": 2, "step_ms": 10, "sentences": 1})
	dev, _ := e.m.Device(v.DeviceID)
	job := testJob(t, "01JOBTEST0000000000000000F", remote.EngineFasterWhisper)
	if _, err := dev.Transcribe(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	if encoded != 1 {
		t.Fatalf("FLAC encodings: %d", encoded)
	}
	if _, err := os.Stat(filepath.Join(job.OutDir, "ssh-upload-"+job.ID+".flac")); !os.IsNotExist(err) {
		t.Fatal("the local FLAC was not removed")
	}
	waitNoJobs(t, f)
}

func TestHostKeyChangedIsRefused(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	v := addTrusted(t, e, f)
	install(t, e, v.ID)
	e.m.server(v.ID).close()
	before := len(f.sentCommands())
	f.newHostKey()
	_, err := e.m.Test(context.Background(), v.ID)
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	got, _ := e.m.Get(v.ID)
	if got.State != StateKeyChanged || got.LastError == nil || got.LastError.Code != remote.CodeHostKeyMismatch {
		t.Fatalf("after a changed key: %+v", got)
	}
	if len(f.sentCommands()) != before {
		t.Fatal("commands ran on a server with a changed host key")
	}
	dev, _ := e.m.Device(v.DeviceID)
	if ok, wf := dev.Ready(context.Background(), 1); ok || wf != model.WaitRemoteAuth {
		t.Fatalf("ready with a changed key: %v %q", ok, wf)
	}
}

func TestUnauthorizedKey(t *testing.T) {
	f := newFakeRemote(t, "Darwin arm64")
	e := newTestEnv(t, nil)
	host, port := f.addr()
	v, err := e.m.Add(context.Background(), AddRequest{Name: "No key", Host: host, Port: port, Username: "u"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.m.Connect(context.Background(), v.ID)
	if err != nil || res.HostKey == nil {
		t.Fatalf("connect: %v %+v", err, res)
	}
	res, err = e.m.Trust(context.Background(), v.ID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: res.HostKey.Fingerprint})
	if err == nil {
		t.Fatal("connected without an authorized key")
	}
	if res.Server.State != StateAuth {
		t.Fatalf("state %q", res.Server.State)
	}
}
