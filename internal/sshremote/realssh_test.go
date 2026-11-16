// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realssh

package sshremote

// The real SSH test (opt-in: go test -tags realssh -run RealSSH; make.ps1
// realssh). It talks to the machine named by the environment, with the real
// lf-agent builds (tools/lf-agent, make.ps1 agent), the real component
// manifest and a real model; never part of the normal test run.
//
//	LF_SSH_TEST_STATE        a directory kept between runs (database, data key, logs)
//	LF_SSH_TEST_HOST / _PORT / _USER
//	LF_SSH_TEST_FINGERPRINT  the server's host key fingerprint, checked by the user beforehand
//	LF_SSH_TEST_AUDIO        a 16 kHz mono WAV (a lecture or a part of one)
//	LF_SSH_TEST_MODEL        the model (default large-v3-turbo)
//	LF_SSH_TEST_STEP         add | run | cancel | remove
//
// "add" creates the server and writes its authorized_keys line to
// STATE/pubkey.txt (the user appends it on the server); "run" confirms the
// host key, installs agent and kit and transcribes the audio
// (STATE/result.json, STATE/report.json); "cancel" starts a transcription
// and cancels it once the worker reports progress; "remove" uninstalls the
// agent (environment, model and jobs) and removes the server.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/secrets"
)

func realEnv(t *testing.T) (*Manager, *devices.Registry, string) {
	t.Helper()
	state := os.Getenv("LF_SSH_TEST_STATE")
	if state == "" {
		t.Fatal("LF_SSH_TEST_STATE is not set")
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(state, "data.key")
	key, err := os.ReadFile(keyFile)
	if err != nil {
		key = make([]byte, secrets.KeySize)
		_, _ = rand.Read(key)
		if err := os.WriteFile(keyFile, key, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := secrets.SetKey(key); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.OpenDB(filepath.Join(state, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	st := db.New(sqlDB)
	cfg := config.Default()
	cfg.DataDir = state
	cfg.ASR.FakeResult = "none" // the registry starts no local detection process
	reg := devices.NewRegistry(st, cfg, state, devices.RegistryOptions{})
	if err := reg.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg.EnableSSH() // as cmd does with LECTUREFLOW_EXPERIMENTAL_SSH=1
	agents := os.Getenv("LF_SSH_TEST_AGENTS")
	if agents == "" {
		agents = filepath.Join("..", "..", "tools", "lf-agent")
	}
	m, err := New(context.Background(), Options{Store: st.(Store), Devices: reg, AgentDir: agents,
		Manifest: filepath.Join("..", "..", "packaging", "components", "components.json"), WorkerDir: filepath.Join("..", "..", "worker"),
		AgentVersion: "3.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, reg, state
}

const realID = "acceptance"

func TestRealSSH(t *testing.T) {
	m, reg, state := realEnv(t)
	ctx := context.Background()
	step := os.Getenv("LF_SSH_TEST_STEP")
	report := map[string]any{"step": step, "at": time.Now().UTC().Format(time.RFC3339)}
	defer func() {
		b, _ := json.MarshalIndent(report, "", "  ")
		_ = os.WriteFile(filepath.Join(state, "report-"+step+".json"), b, 0o600)
		t.Logf("report: %s", b)
	}()
	switch step {
	case "add":
		if _, ok := m.Get(realID); !ok {
			port, _ := strconv.Atoi(os.Getenv("LF_SSH_TEST_PORT"))
			if _, err := m.Add(ctx, AddRequest{ID: realID, Name: "Acceptance Mac", Host: os.Getenv("LF_SSH_TEST_HOST"), Port: port,
				Username: os.Getenv("LF_SSH_TEST_USER"), Group: GroupPrefer}); err != nil {
				t.Fatal(err)
			}
		}
		pk, err := m.PublicKeyOf(realID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "pubkey.txt"), []byte(pk.Line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		report["fingerprint"] = pk.Fingerprint
	case "run":
		start := time.Now()
		res, err := m.Connect(ctx, realID)
		if err != nil {
			t.Fatal(err)
		}
		if res.HostKey != nil {
			want := os.Getenv("LF_SSH_TEST_FINGERPRINT")
			report["presented"] = res.HostKey.Fingerprint
			if want == "" || res.HostKey.Fingerprint != want {
				t.Fatalf("host key %s, expected %q", res.HostKey.Fingerprint, want)
			}
			if res, err = m.Trust(ctx, realID, TrustRequest{Hop: res.HostKeyHop, Fingerprint: want}); err != nil {
				t.Fatal(err)
			}
		}
		report["connect_s"] = time.Since(start).Seconds()
		report["probe"] = res.Server.Probe
		if d, ok := reg.Get(model.SSHDeviceID(realID)); ok {
			if mdl := os.Getenv("LF_SSH_TEST_MODEL"); mdl != "" && mdl != d.Model {
				d.Model = mdl
				if err := reg.UpsertSSH(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !res.Server.KitReady {
			start := time.Now()
			if err := m.Install(realID); err != nil {
				t.Fatal(err)
			}
			for {
				v, _ := m.Get(realID)
				if v.State == StateReady {
					break
				}
				if v.State != StateInstalling && v.State != StateConnecting {
					t.Fatalf("install ended in %s: %+v", v.State, v.LastError)
				}
				time.Sleep(2 * time.Second)
			}
			report["install_s"] = time.Since(start).Seconds()
		}
		path, took, n := realTranscribe(t, m, reg, "01REALSSHACCEPTANCE000000A", nil)
		report["transcribe_s"], report["sentences"] = took.Seconds(), n
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(filepath.Join(state, "result.json"), b, 0o600)
		report["card"] = m.server(realID).Card(ctx)
	case "cancel":
		ctx2, cancel := context.WithCancel(ctx)
		var at time.Time
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = realTranscribe(t, m, reg, "01REALSSHCANCEL00000000000", func(ev remote.Event) {
				if ev.Stage == remote.StageTranscribe && ev.Fraction > 0.02 && at.IsZero() {
					at = time.Now()
					cancel()
				}
			}, ctx2)
		}()
		<-done
		if at.IsZero() {
			t.Fatal("the transcription never reported progress")
		}
		report["cancel_return_s"] = time.Since(at).Seconds()
		if s := time.Since(at); s > remote.CancelWithin {
			t.Fatalf("the cancel took %v", s)
		}
		time.Sleep(5 * time.Second) // the remote cancel and delete run in the background
	case "remove":
		if err := m.Remove(ctx, realID, true); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("LF_SSH_TEST_STEP %q: add | run | cancel | remove", step)
	}
}

// realTranscribe sends the test audio with the parameters an MLX run of the
// asr stage would have.
func realTranscribe(t *testing.T, m *Manager, reg *devices.Registry, id string, progress func(remote.Event),
	ctxs ...context.Context) (string, time.Duration, int) {
	t.Helper()
	ctx := context.Background()
	if len(ctxs) > 0 {
		ctx = ctxs[0]
	}
	audio := os.Getenv("LF_SSH_TEST_AUDIO")
	fi, err := os.Stat(audio)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := reg.Get(model.SSHDeviceID(realID))
	if !ok {
		t.Fatal("no device row")
	}
	sum, err := fileSHA256(audio)
	if err != nil {
		t.Fatal(err)
	}
	sec := float64(fi.Size()-44) / 32000
	params := map[string]any{"model": d.Model, "precision": d.Precision, "device": "mlx:gpu", "language": "en",
		"initial_prompt": "", "word_timestamps": true,
		"checkpoint": map[string]any{"job_id": id, "input_hash": ctxbuild.ASRInputHashV3(sum, "en"), "device_id": d.ID,
			"model": d.Model, "precision": d.Precision, "chunk_s": devices.OVCheckpointChunkS}}
	out := t.TempDir()
	job := remote.Job{ID: id, LectureID: "acceptance", AudioPath: audio, AudioSHA256: sum, DurationSec: sec, Engine: d.Backend,
		Model: d.Model, Precision: d.Precision, Params: params, OutDir: out}
	dev, _ := m.Device(d.ID)
	start := time.Now()
	last := ""
	path, err := dev.Transcribe(ctx, job, func(ev remote.Event) {
		if s := fmt.Sprintf("%s/%s", ev.Step, ev.Stage); s != last {
			last = s
			t.Logf("%6.1fs %s %.2f", time.Since(start).Seconds(), s, ev.Overall)
		}
		if progress != nil {
			progress(ev)
		}
	})
	took := time.Since(start)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", took, 0
		}
		t.Fatal(err)
	}
	var res struct {
		Sentences []json.RawMessage `json:"sentences"`
	}
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &res)
	if len(res.Sentences) == 0 || !strings.Contains(string(b), "start_ms") {
		t.Fatalf("result %s", b[:min(len(b), 300)])
	}
	return path, took, len(res.Sentences)
}
