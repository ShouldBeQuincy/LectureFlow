// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// One SSH inference server as a remote.Device: its connection (opened on
// demand, closed after IdleTimeout without work), its agent and kit, and
// the state the card, the scheduler's admission (Ready) and the API show.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// Card states of an SSH server (remote.Card.State).
const (
	StateOffline     = "offline"          // cannot be reached (or not tried yet)
	StateIdle        = "idle"             // was ready; disconnected after IdleTimeout without work
	StateConnecting  = "connecting"       // connecting and checking agent and environment
	StateUntrusted   = "untrusted"        // its host key is not confirmed yet
	StateKeyChanged  = "host_key_changed" // it presented another host key than the confirmed one
	StateAuth        = "auth_failed"      // it refused the key (authorized_keys line missing?)
	StateUnsupported = "unsupported"      // not linux-x64 or darwin-arm64
	StateSetup       = "setup"            // the environment is not installed
	StateInstalling  = "installing"       // the environment is being installed
	StateReady       = "ready"            // connected, ready for lectures
	StateBusy        = "busy"             // ready, every slot in use
	StatePaused      = "paused"           // the user paused it
	StateError       = "error"            // the agent misbehaved or could not be installed
)

// Settings are a server's own settings (remote_server.settings_json).
type Settings struct {
	// Group: GroupPrefer (before the local devices) or GroupSupplement
	// (when the local devices are busy). The device list's order decides;
	// this is the wizard's answer, applied as the row's priority.
	Group string `json:"group,omitempty"`
	// Paused: no lecture goes to it.
	Paused bool `json:"paused,omitempty"`
	// Installed: the user installed the environment once; a changed kit
	// (new worker scripts after an update) is installed again on its own.
	Installed bool `json:"installed,omitempty"`
	// RetainMin: how long a job's files survive a lost connection (≤ 120).
	RetainMin int `json:"retain_min,omitempty"`
}

// Groups.
const (
	GroupPrefer     = "prefer"
	GroupSupplement = "supplement"
)

// Server is one SSH inference server.
type Server struct {
	m     *Manager
	id    string // the slug
	devID string

	mu         sync.Mutex
	rec        db.RemoteServer
	set        Settings
	state      string
	lastErr    *remote.CardError
	c          *conn
	ctl        *control
	connecting bool
	nextTry    time.Time
	backoff    time.Duration
	lastUse    time.Time
	lastSeen   time.Time
	presented  []ssh.PublicKey // the host key of each hop seen last
	probe      remote.Probe
	samples    remote.Samples // the card's machine and readings (shared with Colab)
	runs       map[string]*remote.CardJob
	installing *installState
}

type installState struct {
	step     string
	fraction float64
	err      error
	done     bool
}

func newServer(m *Manager, rec db.RemoteServer) *Server {
	s := &Server{m: m, id: rec.ID, devID: model.SSHDeviceID(rec.ID), rec: rec, state: StateOffline,
		runs: map[string]*remote.CardJob{}}
	_ = json.Unmarshal([]byte(rec.SettingsJSON), &s.set)
	if rec.HostKey == "" {
		s.state = StateUntrusted
	}
	_ = json.Unmarshal([]byte(rec.ProbeJSON), &s.probe)
	return s
}

// ID is the device id.
func (s *Server) ID() string { return s.devID }

// label is the display name.
func (s *Server) label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Name
}

// row is the server's device row (ok false before the first probe).
func (s *Server) row() (model.ASRDevice, bool) {
	if s.m.o.Devices == nil {
		return model.ASRDevice{}, false
	}
	return s.m.o.Devices.Get(s.devID)
}

// Slots is the row's max_parallel (at least 1).
func (s *Server) Slots() int {
	if d, ok := s.row(); ok && d.MaxParallel > 0 {
		return d.MaxParallel
	}
	return 1
}

func (s *Server) retain() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set.RetainMin > 0 {
		return min(time.Duration(s.set.RetainMin)*time.Minute, remote.MaxRetain)
	}
	return s.m.o.Retain
}

// Ready: may a lecture start on it now (remote.Device).
func (s *Server) Ready(ctx context.Context, audioSec float64) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set.Paused {
		return false, model.WaitDevicePaused(s.devID)
	}
	switch s.state {
	case StateReady, StateIdle, StateBusy:
		if len(s.runs) >= s.slotsLocked() {
			return false, ""
		}
		if s.state == StateIdle {
			s.kickLocked() // reconnect now; Transcribe waits for it
		}
		return true, ""
	case StateUntrusted, StateKeyChanged, StateAuth:
		return false, model.WaitRemoteAuth
	case StateSetup, StateInstalling, StateUnsupported:
		return false, model.WaitRemoteSetup
	case StateError:
		s.kickLocked()
		return false, model.WaitRemoteSetup
	}
	s.kickLocked()
	return false, model.WaitRemoteOffline
}

func (s *Server) slotsLocked() int {
	if d, ok := s.row(); ok && d.MaxParallel > 0 {
		return d.MaxParallel
	}
	return 1
}

// kickLocked starts a background connection attempt (with back-off).
func (s *Server) kickLocked() {
	if s.connecting || s.rec.HostKey == "" || s.m.closed() || s.m.now().Before(s.nextTry) {
		return
	}
	switch s.state {
	case StateUntrusted, StateKeyChanged, StateUnsupported, StateInstalling:
		return
	}
	s.connecting = true
	go func() {
		ctx, cancel := context.WithTimeout(s.m.ctx, 2*time.Minute)
		defer cancel()
		_, _, err := s.session(ctx, false)
		s.mu.Lock()
		s.connecting = false
		if err != nil {
			s.backoff = min(max(2*s.backoff, time.Second), 30*time.Second)
			s.nextTry = s.m.now().Add(s.backoff)
		} else {
			s.backoff, s.nextTry = 0, time.Time{}
		}
		s.mu.Unlock()
	}()
}

// setState records a state (and the card's last error) and keeps the
// device table's presence in step.
func (s *Server) setState(state string, err error) {
	s.mu.Lock()
	s.state = state
	if err != nil {
		_, code, _ := connError(err)
		s.lastErr = &remote.CardError{Code: code, Params: connErrParams(s.rec.Name, err), At: s.m.now().UTC()}
	} else if state == StateReady {
		s.lastErr = nil
	}
	present := state == StateReady || state == StateBusy
	s.mu.Unlock()
	if s.m.o.Devices != nil {
		s.m.o.Devices.SetSSHPresent(s.devID, present || state == StateIdle)
	}
	s.m.changed()
}

// target is the connection target of the current row.
func (s *Server) target(ctx context.Context) (target, error) {
	s.mu.Lock()
	rec := s.rec
	s.mu.Unlock()
	signer, err := loadSigner(ctx, s.m.o.Store, rec.ID, rec.KeyRef)
	if err != nil {
		return target{}, err
	}
	var jumps []Jump
	_ = json.Unmarshal([]byte(rec.JumpJSON), &jumps)
	return target{host: rec.Host, port: rec.Port, user: rec.Username, hostKey: rec.HostKey, jumps: jumps, signer: signer,
		dial: s.m.dial, clientID: "SSH-2.0-LectureFlow",
		seenKey: func(hop int, k ssh.PublicKey) {
			s.mu.Lock()
			for len(s.presented) <= hop {
				s.presented = append(s.presented, nil)
			}
			s.presented[hop] = k
			s.mu.Unlock()
		}}, nil
}

// session returns a live connection and control channel, connecting,
// installing or updating the agent and checking (with install: installing)
// the kit as needed.
func (s *Server) session(ctx context.Context, install bool) (*conn, *control, error) {
	s.mu.Lock()
	if s.c != nil && s.c.alive() && s.ctl != nil && s.ctl.alive() {
		c, ctl := s.c, s.ctl
		s.lastUse = s.m.now()
		s.mu.Unlock()
		return c, ctl, nil
	}
	s.mu.Unlock()
	s.m.sessMu(s.id).Lock()
	defer s.m.sessMu(s.id).Unlock()
	s.mu.Lock()
	if s.c != nil && s.c.alive() && s.ctl != nil && s.ctl.alive() {
		c, ctl := s.c, s.ctl
		s.mu.Unlock()
		return c, ctl, nil
	}
	prev := s.state
	s.mu.Unlock()
	if prev != StateInstalling {
		s.setState(StateConnecting, nil)
	}
	c, ctl, err := s.open(ctx, install)
	if err != nil {
		state, _, _ := connError(err)
		if prev == StateInstalling && ctx.Err() == nil {
			state = StateInstalling
		}
		if ctx.Err() != nil && !errors.Is(err, errSetup) {
			state = StateOffline
		}
		s.setState(state, err)
		slog.Info("ssh server: not ready", "device", s.devID, "state", state, "code", ConnErrorCode(err), "class", ErrorClass(err))
		return nil, nil, err
	}
	s.mu.Lock()
	s.c, s.ctl, s.lastUse, s.lastSeen = c, ctl, s.m.now(), s.m.now()
	s.mu.Unlock()
	s.setState(StateReady, nil)
	go s.watchConn(c, ctl)
	return c, ctl, nil
}

// watchConn notes a lost connection.
func (s *Server) watchConn(c *conn, ctl *control) {
	select {
	case <-c.done:
	case <-ctl.dead:
		c.close()
	}
	s.mu.Lock()
	mine := s.c == c
	if mine {
		s.c, s.ctl = nil, nil
	}
	idle := len(s.runs) == 0
	state := s.state
	s.mu.Unlock()
	if !mine {
		return
	}
	if state == StateReady || state == StateBusy {
		if idle {
			s.setState(StateIdle, nil)
		} else {
			s.setState(StateOffline, errLost)
		}
	}
}

// open does the work of session.
func (s *Server) open(ctx context.Context, install bool) (*conn, *control, error) {
	t, err := s.target(ctx)
	if err != nil {
		return nil, nil, err
	}
	c, err := connect(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			c.close()
		}
	}()
	platform, err := s.platform(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	build, err := findAgent(s.m.o.AgentDir, platform)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errAgent, err) // errAgentMissing stays visible to connError
	}
	ver, current, err := agentVersion(ctx, c, platform, build)
	if err != nil {
		return nil, nil, err
	}
	if !current {
		if ver, err = installAgent(ctx, c, platform, build); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errAgent, err)
		}
	}
	probe, err := agentEvent(ctx, c, platform, build, nil, "probe")
	if err != nil {
		return nil, nil, fmt.Errorf("%w: probe: %v", errAgent, err)
	}
	if err := s.noteProbe(ctx, platform, ver.Version, probe.Probe); err != nil {
		return nil, nil, err
	}
	kit, err := s.kit()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errSetup, err)
	}
	hash := kit.Hash()
	if !slices.Contains(probe.Probe.Kits, hash) {
		s.mu.Lock()
		auto := s.set.Installed
		s.mu.Unlock()
		if !install && !auto {
			return nil, nil, errSetup
		}
		if err := s.ensure(ctx, c, platform, build, kit); err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		s.probe.Kits = append(s.probe.Kits, hash)
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.rec.KitHash != hash {
		s.rec.KitHash = hash
		s.persistLocked(ctx)
	}
	s.mu.Unlock()
	cmd, err := agentCommand(platform, build.id(), "serve")
	if err != nil {
		return nil, nil, err
	}
	ctl, err := openControl(ctx, c, cmd, s.retain(), s.onMachine, s.onSample)
	if err != nil {
		return nil, nil, err
	}
	ok = true
	s.mu.Lock()
	s.m.builds[s.id] = build
	s.mu.Unlock()
	return c, ctl, nil
}

// platform detects (once) and stores the server's platform.
func (s *Server) platform(ctx context.Context, c *conn) (string, error) {
	p, err := detectPlatform(ctx, c)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.rec.Platform != p {
		s.rec.Platform, s.rec.Engine = p, engineOf(p)
		s.persistLocked(ctx)
	}
	s.mu.Unlock()
	return p, nil
}

// noteProbe stores the probe and creates or updates the device row.
func (s *Server) noteProbe(ctx context.Context, platform, agentVersion string, p remote.Probe) error {
	b, _ := json.Marshal(p)
	s.mu.Lock()
	s.probe = p
	s.rec.ProbeJSON, s.rec.AgentVersion = string(b), agentVersion
	s.persistLocked(ctx)
	name, group := s.rec.Name, s.set.Group
	s.mu.Unlock()
	if s.m.o.Devices == nil {
		return nil
	}
	engine := engineOf(platform)
	d, ok := s.m.o.Devices.Get(s.devID)
	row := model.ASRDevice{ID: s.devID, Backend: engine, Label: name, MaxParallel: 1}
	if ok && d.Backend == engine {
		row.Model, row.Precision = d.Model, d.Precision
	} else {
		row.Model, row.Precision = recommendModel(engine, p)
		row.Priority = s.m.priorityFor(group)
	}
	return s.m.o.Devices.UpsertSSH(ctx, row)
}

// recommendModel is the model of a new row by the remote's memory.
func recommendModel(engine string, p remote.Probe) (string, string) {
	if engine == remote.EngineMLX {
		if p.MemTotalMB > 0 && p.MemTotalMB < 12000 {
			return "medium", model.PrecisionFP16
		}
		return "large-v3-turbo", model.PrecisionFP16
	}
	if p.VRAMTotalMB > 0 && p.VRAMTotalMB < 6000 {
		return "medium", model.PrecisionAuto
	}
	return "large-v3", model.PrecisionAuto
}

// kit is the server's kit for its row's model and precision.
func (s *Server) kit() (remote.KitSpec, error) {
	s.mu.Lock()
	platform, vram := s.rec.Platform, int(s.probe.VRAMTotalMB)
	s.mu.Unlock()
	d, ok := s.row()
	if !ok {
		return remote.KitSpec{}, errors.New("no device row")
	}
	engine := engineOf(platform)
	return s.m.kitFor(platform, engine, d.Model, modelPrecision(engine, d.Precision, vram))
}

// ensure installs the kit, reporting its steps on the card.
func (s *Server) ensure(ctx context.Context, c *conn, platform string, b agentBuild, kit remote.KitSpec) error {
	spec, err := json.Marshal(kit)
	if err != nil {
		return err
	}
	cmd, err := agentCommand(platform, b.id(), "ensure")
	if err != nil {
		return err
	}
	st := &installState{step: "python"}
	s.mu.Lock()
	s.installing = st
	s.mu.Unlock()
	s.setState(StateInstalling, nil)
	lw := &lineWriter{fn: func(line []byte) {
		ev, err := remote.ParseEvent(line)
		if err != nil {
			return
		}
		s.mu.Lock()
		switch ev.Type {
		case remote.EvStep:
			st.step, st.fraction = ev.Step, ev.Fraction
		case remote.EvFail:
			st.err = &agentFail{code: ev.Code, freeMB: ev.FreeMB, needMB: ev.NeedMB}
		case remote.EvReady:
			st.done = true
		}
		s.mu.Unlock()
		s.m.changed()
	}}
	stderr, err := c.run(ctx, cmd, bytes.NewReader(spec), lw)
	lw.flush()
	s.mu.Lock()
	s.installing = nil
	failed := st.err
	done := st.done
	if err == nil && done {
		s.set.Installed = true
		s.persistLocked(ctx)
	}
	s.mu.Unlock()
	if err != nil || !done {
		if failed == nil {
			failed = err
		}
		if strings.TrimSpace(stderr) != "" {
			slog.Warn("ssh server: installing the environment failed", "device", s.devID, "code", failCode(failed),
				"detail", remote.CleanText(stderr, 512))
		}
		return fmt.Errorf("%w: %v", errInstall, failed)
	}
	return nil
}

var errInstall = errors.New("installing the environment failed")

// lineWriter calls fn for every complete line written to it.
type lineWriter struct {
	buf []byte
	fn  func([]byte)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimRight(w.buf[:i], "\r")
		if len(line) <= remote.MaxEventLine {
			w.fn(line)
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > remote.MaxEventLine {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.fn(bytes.TrimSpace(w.buf))
		w.buf = nil
	}
}

// persistLocked writes the row (caller holds s.mu).
func (s *Server) persistLocked(ctx context.Context) {
	b, _ := json.Marshal(s.set)
	s.rec.SettingsJSON = string(b)
	s.rec.UpdatedAt = s.m.now().UTC().Format(time.RFC3339)
	if err := s.m.o.Store.UpdateRemoteServer(context.WithoutCancel(ctx), s.rec); err != nil {
		slog.Warn("ssh server: saving its row", "device", s.devID, "err", err)
	}
}

// onMachine / onSample keep the card's readings (remote.ReadMachine /
// ReadSample: the checks and conversions Colab uses too).
func (s *Server) onMachine(v map[string]any) {
	m, bad := remote.ReadMachine(getter(v))
	if bad != "" {
		return // ParseEvent checked it already; never half a reading
	}
	source := remote.SourceNvidiaSMI
	if m.Driver == "" {
		source = remote.SourceSystemProfiler // an Apple GPU: no nvidia-smi
	}
	s.samples.SetMachine(m.Machine(source))
	s.mu.Lock()
	s.lastSeen = s.m.now()
	s.mu.Unlock()
	s.m.changed()
}

func (s *Server) onSample(v map[string]any) {
	r, bad := remote.ReadSample(getter(v))
	if bad != "" {
		return
	}
	now := s.m.now()
	s.samples.Add(now, r.Usage())
	s.mu.Lock()
	s.lastSeen = now
	s.mu.Unlock()
}

// getter reads a parsed machine / sample event (numbers as float64).
func getter(v map[string]any) remote.Getter {
	return func(k string) (any, bool) {
		x, ok := v[k]
		if n, isInt := x.(int64); isInt {
			return float64(n), ok
		}
		return x, ok
	}
}

// close ends the connection (the agent stops and keeps nothing it ran).
func (s *Server) close() {
	s.mu.Lock()
	c := s.c
	s.c, s.ctl = nil, nil
	s.mu.Unlock()
	if c != nil {
		c.close()
	}
}
