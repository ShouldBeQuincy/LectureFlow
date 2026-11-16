// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// The user's own SSH inference servers in the mock (ssh-1b): the bodies of
// internal/api/handlers_remote.go (internal/sshremote.ServerView), the
// "add a server" wizard's steps (add, the public key, the host key to
// confirm, trust, install with its progress in time, test), pause, edit,
// remove, "改用“服务器”处理", the servers' device rows (in the scenario's
// own store, so the device list, its order and the model change work as
// for any device) and their resource cards (Snapshot.Remotes, kind ssh).
// Nothing is contacted: the hardware, the host key and the timings are
// made up.
//
// Scenarios (?mock=…; every other scenario has the SSH servers off: the
// endpoints answer 404 api.not_found, as the real server does without
// LECTUREFLOW_EXPERIMENTAL_SSH=1):
//
//	ssh_none   switched on, no server yet (the wizard from the start; a new
//	           server reports an RTX 3090 24 GB on Ubuntu 24.04)
//	ssh_ready  "gpu-box" (Linux, RTX 4090 24 GB) connected and used first,
//	           transcribing the first running lecture, with its machine,
//	           readings and 60 s history; "mac-mini" (macOS, M4 16 GB,
//	           MLX) offline after a lost connection; "lab-server" whose host
//	           key is not checked yet (继续设置)
//	ssh_key_changed
//	           "gpu-box" set up and confirmed, but it now presents another
//	           host key (SEC7 M1): host_key_changed with the error naming the
//	           new fingerprint; 继续设置 → the "主机密钥已变化" step (connect
//	           answers changed and previous_fingerprint; trust needs replace
//	           and the old fingerprint)

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sshremote"
)

// sshInstallScale multiplies the simulated install (tests make it small; 1
// = about 9 seconds).
var sshInstallScale = 1.0

const (
	sshHostKeyFP = "SHA256:k3mOckH0stKeyF1ngerprintOnlyF0rPreview00"
	sshHostKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMockHostKeyForThePreviewOnly0000000000000"
	// ssh_key_changed: the key gpu-box presents instead (SEC7 M1)
	sshNewHostKeyFP = "SHA256:n3wM0ckH0stKeyAfterAReinstallPreviewOnly0"
	sshNewHostKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINewMockHostKeyForThePreviewOnly00000000000"
	sshMockApp      = "3.2.0"
	sshNotFoundOn   = "api.not_found"
)

var sshScenarios = map[string]func(*store, *sshMock, time.Time){"ssh_none": func(*store, *sshMock, time.Time) {}, "ssh_ready": setupSSHReady,
	"ssh_key_changed": setupSSHKeyChanged}

func init() {
	mockV3Routes = append(mockV3Routes, func(m *mock, mux *http.ServeMux) {
		h := func(fn func(http.ResponseWriter, *http.Request, *store, *sshMock)) http.HandlerFunc {
			return m.api(func(w http.ResponseWriter, r *http.Request, s *store) {
				x := m.sshState(r)
				if x == nil {
					codedError(w, http.StatusNotFound, sshNotFoundOn, map[string]any{"method": r.Method, "path": r.URL.Path})
					return
				}
				fn(w, r, s, x)
			})
		}
		mux.HandleFunc("GET /api/remote/servers", h(sshList))
		mux.HandleFunc("POST /api/remote/servers", h(sshAdd))
		mux.HandleFunc("GET /api/remote/servers/{id}", h(sshGet))
		mux.HandleFunc("PATCH /api/remote/servers/{id}", h(sshPatch))
		mux.HandleFunc("DELETE /api/remote/servers/{id}", h(sshRemove))
		mux.HandleFunc("POST /api/remote/servers/{id}/connect", h(sshConnect))
		mux.HandleFunc("POST /api/remote/servers/{id}/trust", h(sshTrust))
		mux.HandleFunc("POST /api/remote/servers/{id}/install", h(sshInstall))
		mux.HandleFunc("POST /api/remote/servers/{id}/test", h(sshTest))
		mux.HandleFunc("GET /api/remote/servers/{id}/pubkey", h(sshPubKey))
		mux.HandleFunc("POST /api/lectures/{id}/use-remote", h(func(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
			var in api.UseRemoteRequest
			if err := decodeBody(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			x.mu.Lock()
			srv := x.byDevice(in.Device)
			x.mu.Unlock()
			if srv == nil || !srv.kitReady {
				codedError(w, http.StatusConflict, remote.CodeNotConfigured, map[string]any{"device": in.Device})
				return
			}
			m.getLecture(w, r, s)
		}))
	})
}

// ---------- state ----------

type sshSrv struct {
	id, name, host, username string
	port                     int
	group                    string
	paused                   bool
	trusted                  bool
	presented                bool // connect showed the host key
	state                    string
	platform                 string
	probe                    *sshremote.ProbeView
	kitReady                 bool
	agent                    string
	installAt                time.Time // zero: not installing
	latency                  int
	lastErr                  *remote.CardError
	hw                       sshremote.ProbeView // what a probe reports
	running                  bool                // a lecture runs on it (ssh_ready)
	pinned                   string              // the confirmed key's fingerprint ("": sshHostKeyFP)
	keyChanged               bool                // it presents sshNewHostKey instead (ssh_key_changed)
}

// pinnedFP is the confirmed host key's fingerprint.
func (srv *sshSrv) pinnedFP() string {
	if srv.pinned != "" {
		return srv.pinned
	}
	return sshHostKeyFP
}

type sshMock struct {
	mu      sync.Mutex
	servers []*sshSrv
	store   *lazyStore
	setup   func(*store, *sshMock, time.Time)
	ready   bool
}

var sshStates sync.Map // mock + scenario → *sshMock

type sshKey struct {
	m  *mock
	sc string
}

// sshState is the scenario's SSH world (nil: the SSH servers are off).
func (m *mock) sshState(r *http.Request) *sshMock {
	sc := scenario(r)
	setup, ok := sshScenarios[sc]
	if !ok {
		return nil
	}
	v, _ := sshStates.LoadOrStore(sshKey{m, sc}, &sshMock{store: &lazyStore{}, setup: setup})
	x := v.(*sshMock)
	m.sshStoreOf(x) // the store and the servers are set up together
	return x
}

// sshStore is storeFor's hook: the SSH scenarios have their own store (the
// servers' device rows live there).
func (m *mock) sshStore(r *http.Request) (*store, bool) {
	x := m.sshState(r)
	if x == nil {
		return nil, false
	}
	return m.sshStoreOf(x), true
}

func (m *mock) sshStoreOf(x *sshMock) *store {
	return x.store.get(m, func(s *store) {
		x.mu.Lock()
		defer x.mu.Unlock()
		if !x.ready {
			x.ready = true
			x.setup(s, x, time.Now())
		}
	})
}

func (x *sshMock) byID(id string) *sshSrv {
	for _, s := range x.servers {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (x *sshMock) byDevice(dev string) *sshSrv {
	return x.byID(model.SSHSlug(dev))
}

func defaultHW() sshremote.ProbeView {
	return sshremote.ProbeView{OSVersion: "Ubuntu 24.04.3 LTS", CPUs: 16, MemTotalMB: 64215, DiskFreeMB: 421888, GPUName: "NVIDIA GeForce RTX 3090", VRAMTotalMB: 24576, Driver: "580.95.05"}
}

func setupSSHReady(s *store, x *sshMock, now time.Time) {
	box := &sshSrv{id: "gpu-box", name: "gpu-box", host: "gpu-box.example.net", username: "student", port: 22, group: sshremote.GroupPrefer,
		trusted: true, state: sshremote.StateReady, platform: remote.PlatformLinuxX64, kitReady: true, agent: sshMockApp, latency: 18,
		hw: sshremote.ProbeView{OSVersion: "Ubuntu 24.04.3 LTS", CPUs: 24, MemTotalMB: 64215, DiskFreeMB: 386210, GPUName: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564, Driver: "580.95.05"}}
	box.probe = &box.hw
	mac := &sshSrv{id: "mac-mini", name: "mac-mini", host: "192.0.2.42", username: "student", port: 22, group: sshremote.GroupSupplement,
		trusted: true, state: sshremote.StateOffline, platform: remote.PlatformDarwinArm64, kitReady: true, agent: "3.1.0",
		hw:      sshremote.ProbeView{OSVersion: "macOS 26.1", CPUs: 10, MemTotalMB: 16384, DiskFreeMB: 182400, GPUName: "Apple M4", VRAMTotalMB: -1, Metal: true},
		lastErr: &remote.CardError{Code: remote.CodeOffline, Params: map[string]any{"device": "mac-mini"}, At: now.Add(-7 * time.Minute).UTC()}}
	mac.probe = &mac.hw
	lab := &sshSrv{id: "lab-server", name: "lab-server", host: "lab.cs.example.org", username: "labuser", port: 2222, group: sshremote.GroupSupplement,
		state: sshremote.StateUntrusted, hw: defaultHW()}
	x.servers = []*sshSrv{box, lab, mac}
	sshAddRow(s, box, "large-v3", model.PrecisionAuto)
	sshAddRow(s, mac, "large-v3-turbo", model.PrecisionFP16)
	// the first running lecture transcribes on gpu-box
	for _, l := range s.lectures {
		if j := stageIndex(&l, model.StageASR); j >= 0 && l.Stages[j].Status == model.StatusRunning {
			s.dev.assigned[l.ID] = model.SSHDeviceID(box.id)
			box.running = true
			break
		}
	}
}

// setupSSHKeyChanged: gpu-box (set up and confirmed) presents another host
// key; nothing runs on it.
func setupSSHKeyChanged(s *store, x *sshMock, now time.Time) {
	box := &sshSrv{id: "gpu-box", name: "gpu-box", host: "gpu-box.example.net", username: "student", port: 22, group: sshremote.GroupPrefer,
		trusted: true, keyChanged: true, state: sshremote.StateKeyChanged, platform: remote.PlatformLinuxX64, kitReady: true, agent: sshMockApp, latency: 18,
		hw:      sshremote.ProbeView{OSVersion: "Ubuntu 24.04.3 LTS", CPUs: 24, MemTotalMB: 64215, DiskFreeMB: 386210, GPUName: "NVIDIA GeForce RTX 4090", VRAMTotalMB: 24564, Driver: "580.95.05"},
		lastErr: &remote.CardError{Code: remote.CodeHostKeyMismatch, Params: map[string]any{"device": "gpu-box", "fingerprint": sshNewHostKeyFP}, At: now.Add(-3 * time.Minute).UTC()}}
	box.probe = &box.hw
	x.servers = []*sshSrv{box}
	sshAddRow(s, box, "large-v3", model.PrecisionAuto)
}

// sshAddRow adds (or updates) the server's device row: before the local
// devices when it is used first, else after them. s.mu held or not shared yet.
func sshAddRow(s *store, srv *sshSrv, mdl, precision string) {
	id := model.SSHDeviceID(srv.id)
	backend := model.BackendFasterWhisper
	if srv.platform == remote.PlatformDarwinArm64 {
		backend = model.BackendMLX
	}
	for _, d := range s.dev.devs {
		if d.row.ID == id {
			d.row.Label, d.present = srv.name, srv.present()
			return
		}
	}
	d := &mockDevice{row: model.ASRDevice{ID: id, Backend: backend, Label: srv.name, Enabled: true, Model: mdl, Precision: precision, MaxParallel: 1},
		present: srv.present(), migrated: true}
	s.dev.devs = append(s.dev.devs, d)
	sshPlace(s, id, srv.group)
}

// sshPlace gives the row its group's priority (handlers_remote.go applySSHGroup).
func sshPlace(s *store, id, group string) {
	hi, lo := math.MinInt, math.MaxInt
	for _, d := range s.dev.devs {
		if model.IsRemoteDevice(d.row.ID) {
			continue
		}
		hi, lo = max(hi, d.row.Priority), min(lo, d.row.Priority)
	}
	if hi == math.MinInt {
		hi, lo = 0, 0
	}
	for _, d := range s.dev.devs {
		if d.row.ID == id {
			if group == sshremote.GroupPrefer {
				d.row.Priority = hi + 5
			} else {
				d.row.Priority = lo - 5
			}
		}
	}
}

func (srv *sshSrv) present() bool {
	return srv.state == sshremote.StateReady || srv.state == sshremote.StateBusy || srv.state == sshremote.StateIdle
}

// advance moves an install on with the clock.
func (srv *sshSrv) advance(now time.Time) {
	if srv.installAt.IsZero() {
		return
	}
	if now.Sub(srv.installAt).Seconds() >= 9*sshInstallScale {
		srv.installAt = time.Time{}
		srv.kitReady, srv.agent, srv.state, srv.lastErr = true, sshMockApp, sshremote.StateReady, nil
	}
}

func (srv *sshSrv) installView(now time.Time) *sshremote.InstallView {
	if srv.installAt.IsZero() {
		return nil
	}
	e := now.Sub(srv.installAt).Seconds() / sshInstallScale
	switch {
	case e < 1:
		return &sshremote.InstallView{Step: "python", Fraction: round2(e)}
	case e < 4:
		return &sshremote.InstallView{Step: "packages", Fraction: round2((e - 1) / 3)}
	default:
		return &sshremote.InstallView{Step: "model", Fraction: round2(math.Min(1, (e-4)/5))}
	}
}

// pubKey is the server's (made-up) public key line.
func (srv *sshSrv) pubKey() string {
	return "restrict ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPreviewOnlyNotARealKey" + strings.Repeat("0", 8) + " lectureflow-" + srv.id
}

// view is the server's sshremote.ServerView. s.mu held (the device row).
func (srv *sshSrv) view(s *store, now time.Time) sshremote.ServerView {
	srv.advance(now)
	v := sshremote.ServerView{ID: srv.id, DeviceID: model.SSHDeviceID(srv.id), Name: srv.name, Host: srv.host, Port: srv.port,
		Username: srv.username, Jumps: []sshremote.Jump{}, State: srv.state, Platform: srv.platform, Trusted: srv.trusted,
		PublicKey: srv.pubKey(), KeyFP: "SHA256:PreviewOnlyPublicKeyFingerprint" + srv.id, Settings: sshremote.Settings{Group: srv.group, Paused: srv.paused, Installed: srv.kitReady},
		Probe: srv.probe, Install: srv.installView(now), LastError: srv.lastErr, AgentVersion: srv.agent, KitReady: srv.kitReady,
		AgentCurrent: srv.agent == sshMockApp}
	if srv.trusted {
		v.HostKeyFP = srv.pinnedFP()
	}
	if srv.platform != "" {
		v.Engine = remote.EngineFasterWhisper
		v.Recommended = &sshremote.ModelChoice{Model: "large-v3", Precision: model.PrecisionAuto}
		v.KitSize = &sshremote.KitSize{DownloadBytes: 39752201 + 1569598543 + 3090835702, InstalledBytes: 146561138 + 2589837596 + 3090835702}
		if srv.platform == remote.PlatformDarwinArm64 {
			v.Engine = remote.EngineMLX
			v.Recommended = &sshremote.ModelChoice{Model: "large-v3-turbo", Precision: model.PrecisionFP16}
			v.KitSize = &sshremote.KitSize{DownloadBytes: 29622607 + 221212593 + 1613977880, InstalledBytes: 113920439 + 663637779 + 1613977880}
		}
	}
	for _, d := range s.dev.devs {
		if d.row.ID == v.DeviceID {
			v.Model, v.Precision = d.row.Model, d.row.Precision
			d.present = srv.present() && !srv.paused
		}
	}
	if srv.paused {
		v.State = sshremote.StatePaused
	} else if srv.running && srv.state == sshremote.StateReady {
		v.State = sshremote.StateBusy
	}
	if srv.present() && srv.latency > 0 {
		lat := srv.latency
		v.LatencyMS = &lat
	}
	return v
}

func (x *sshMock) views(s *store, now time.Time) []sshremote.ServerView {
	out := make([]sshremote.ServerView, 0, len(x.servers))
	for _, srv := range x.servers {
		out = append(out, srv.view(s, now))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// ---------- handlers ----------

// sshDo runs fn with the store and the SSH world locked; it answers fn's
// server view (status) or error.
func sshDo(w http.ResponseWriter, s *store, x *sshMock, fn func(now time.Time) (int, any, error)) {
	s.mu.Lock()
	x.mu.Lock()
	status, v, err := fn(time.Now())
	x.mu.Unlock()
	s.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, v)
}

func sshNotFound() error {
	return codedErr(http.StatusNotFound, "remote_server.not_found", nil, "找不到这台远程服务器")
}

func sshList(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		return http.StatusOK, map[string]any{"servers": x.views(s, now)}, nil
	})
}

func sshGet(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		return http.StatusOK, srv.view(s, now), nil
	})
}

var (
	sshHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})?$|^[0-9a-fA-F:]{2,45}$`)
	sshUserRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	sshSlugRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

func sshInvalid(field string) error {
	return codedErr(http.StatusBadRequest, "remote_server.invalid", map[string]any{"field": field}, "远程服务器的设置无效（"+field+"）")
}

func checkSSHEndpoint(name, host string, port int, user string) error {
	switch {
	case name == "" || len([]rune(name)) > 64:
		return sshInvalid("name")
	case !sshHostRe.MatchString(host) || strings.Contains(host, ".."):
		return sshInvalid("host")
	case port < 1 || port > 65535:
		return sshInvalid("port")
	case !sshUserRe.MatchString(user):
		return sshInvalid("username")
	}
	return nil
}

func sshAdd(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	var in sshremote.AddRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		in.Name, in.Host = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host)
		if in.Port == 0 {
			in.Port = 22
		}
		if err := checkSSHEndpoint(in.Name, in.Host, in.Port, in.Username); err != nil {
			return 0, nil, err
		}
		id := strings.Trim(sshSlugRe.ReplaceAllString(strings.ToLower(in.Name), "-"), "-")
		if id == "" {
			id = "server"
		}
		if len(id) > 29 {
			id = strings.Trim(id[:29], "-")
		}
		for n, base := 2, id; x.byID(id) != nil; n++ {
			id = base + "-" + itoa(n)
		}
		group := in.Group
		if group == "" {
			group = sshremote.GroupSupplement
		}
		srv := &sshSrv{id: id, name: in.Name, host: in.Host, port: in.Port, username: in.Username, group: group, state: sshremote.StateUntrusted, hw: defaultHW()}
		x.servers = append(x.servers, srv)
		s.devicesChanged()
		return http.StatusCreated, srv.view(s, now), nil
	})
}

func sshPatch(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	var p sshremote.Patch
	if err := decodeBody(r, &p); err != nil {
		writeErr(w, err)
		return
	}
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		name, host, port, user := srv.name, srv.host, srv.port, srv.username
		if p.Name != nil {
			name = strings.TrimSpace(*p.Name)
		}
		if p.Host != nil {
			host = strings.TrimSpace(*p.Host)
		}
		if p.Port != nil {
			port = *p.Port
		}
		if p.Username != nil {
			user = *p.Username
		}
		if err := checkSSHEndpoint(name, host, port, user); err != nil {
			return 0, nil, err
		}
		if p.Group != nil && *p.Group != sshremote.GroupPrefer && *p.Group != sshremote.GroupSupplement {
			return 0, nil, sshInvalid("group")
		}
		endpoint := host != srv.host || port != srv.port || user != srv.username
		if endpoint && srv.running {
			return 0, nil, codedErr(http.StatusConflict, "remote_server.busy", nil, "这台远程服务器正在转写")
		}
		srv.name, srv.host, srv.port, srv.username = name, host, port, user
		if endpoint {
			srv.trusted, srv.presented, srv.state = false, false, sshremote.StateUntrusted
		}
		if p.Paused != nil {
			srv.paused = *p.Paused
		}
		if p.Group != nil {
			srv.group = *p.Group
			sshPlace(s, model.SSHDeviceID(srv.id), srv.group)
		}
		for _, d := range s.dev.devs {
			if d.row.ID == model.SSHDeviceID(srv.id) {
				d.row.Label = srv.name
			}
		}
		s.devicesChanged()
		return http.StatusOK, srv.view(s, now), nil
	})
}

func sshRemove(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		if srv.running {
			return 0, nil, codedErr(http.StatusConflict, "remote_server.busy", nil, "这台远程服务器正在转写")
		}
		out := x.servers[:0]
		for _, o := range x.servers {
			if o != srv {
				out = append(out, o)
			}
		}
		x.servers = out
		devs := s.dev.devs[:0]
		for _, d := range s.dev.devs {
			if d.row.ID != model.SSHDeviceID(srv.id) {
				devs = append(devs, d)
			}
		}
		s.dev.devs = devs
		s.devicesChanged()
		return http.StatusNoContent, nil, nil
	})
}

// probed: the connection after a confirmed host key — the probe, the
// device row with the model the hardware suits.
func (srv *sshSrv) probed(s *store) {
	if srv.platform == "" {
		srv.platform = remote.PlatformLinuxX64
		if srv.hw.Metal {
			srv.platform = remote.PlatformDarwinArm64
		}
	}
	hw := srv.hw
	srv.probe = &hw
	if srv.kitReady {
		srv.state, srv.lastErr = sshremote.StateReady, nil
	} else {
		srv.state = sshremote.StateSetup
	}
	if srv.latency == 0 {
		srv.latency = 23
	}
	mdl, prec := "large-v3", model.PrecisionAuto
	if srv.platform == remote.PlatformDarwinArm64 {
		mdl, prec = "large-v3-turbo", model.PrecisionFP16
	}
	sshAddRow(s, srv, mdl, prec)
	s.devicesChanged()
}

func sshConnect(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		if !srv.trusted {
			srv.presented = true
			return http.StatusOK, sshremote.ConnectResult{Server: srv.view(s, now), HostKey: &sshremote.HostKeyInfo{Algorithm: "ssh-ed25519",
				Fingerprint: sshHostKeyFP, Key: sshHostKey, Check: "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"}}, nil
		}
		if srv.keyChanged {
			// SEC7 M1: the presented key next to the confirmed one; nothing pinned
			srv.state = sshremote.StateKeyChanged
			srv.lastErr = &remote.CardError{Code: remote.CodeHostKeyMismatch, Params: map[string]any{"device": srv.name, "fingerprint": sshNewHostKeyFP}, At: now.UTC()}
			s.devicesChanged()
			return http.StatusOK, sshremote.ConnectResult{Server: srv.view(s, now), HostKey: &sshremote.HostKeyInfo{Algorithm: "ssh-ed25519",
				Fingerprint: sshNewHostKeyFP, Key: sshNewHostKey, Check: "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"},
				PreviousFingerprint: srv.pinnedFP(), Changed: true}, nil
		}
		srv.probed(s)
		return http.StatusOK, sshremote.ConnectResult{Server: srv.view(s, now)}, nil
	})
}

func sshTrust(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	var in api.TrustRequest
	if err := decodeBody(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		// a confirmed key is replaced only knowingly (SEC7 M1)
		if srv.trusted && (!in.Replace || sshremote.NormalizeFingerprint(in.Previous) != srv.pinnedFP()) {
			return 0, nil, codedErr(http.StatusConflict, "remote_server.host_key_pinned", nil, "主机密钥已经确认过")
		}
		want := sshHostKeyFP
		if srv.keyChanged {
			want = sshNewHostKeyFP
		}
		if sshremote.NormalizeFingerprint(in.Fingerprint) != want {
			return 0, nil, sshInvalid("fingerprint")
		}
		srv.trusted, srv.pinned, srv.keyChanged = true, want, false
		srv.probed(s)
		return http.StatusOK, sshremote.ConnectResult{Server: srv.view(s, now)}, nil
	})
}

func sshInstall(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		if !srv.trusted {
			return 0, nil, codedErr(http.StatusBadGateway, remote.CodeHostKeyUnknown, map[string]any{"device": srv.name}, "主机密钥还没有核对")
		}
		if srv.installAt.IsZero() {
			srv.installAt, srv.state, srv.lastErr = now, sshremote.StateInstalling, nil
		}
		s.devicesChanged()
		return http.StatusAccepted, srv.view(s, now), nil
	})
}

func sshTest(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		if !srv.trusted {
			return 0, nil, codedErr(http.StatusBadGateway, remote.CodeHostKeyUnknown, map[string]any{"device": srv.name}, "主机密钥还没有核对")
		}
		if srv.keyChanged {
			return 0, nil, codedErr(http.StatusBadGateway, remote.CodeHostKeyMismatch, map[string]any{"device": srv.name, "fingerprint": sshNewHostKeyFP}, "主机密钥变了")
		}
		if srv.id == "mac-mini" { // ssh_ready: switched off
			srv.lastErr = &remote.CardError{Code: remote.CodeOffline, Params: map[string]any{"device": srv.name}, At: now.UTC()}
			return 0, nil, codedErr(http.StatusBadGateway, remote.CodeOffline, map[string]any{"device": srv.name}, "无法连接远程服务器")
		}
		srv.probed(s)
		return http.StatusOK, srv.view(s, now), nil
	})
}

func sshPubKey(w http.ResponseWriter, r *http.Request, s *store, x *sshMock) {
	sshDo(w, s, x, func(now time.Time) (int, any, error) {
		srv := x.byID(r.PathValue("id"))
		if srv == nil {
			return 0, nil, sshNotFound()
		}
		line := srv.pubKey()
		return http.StatusOK, sshremote.PublicKey{Line: line, Fingerprint: "SHA256:PreviewOnlyPublicKeyFingerprint" + srv.id,
			// as sshremote.PublicKeyOf writes them (a new line before the key)
			Command: "mkdir -p ~/.ssh && chmod 700 ~/.ssh && printf '\\n%s\\n' '" + line + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys",
			CommandWindows: `New-Item -ItemType Directory -Force "$env:USERPROFILE\.ssh" | Out-Null; ` +
				`Add-Content -Path "$env:USERPROFILE\.ssh\authorized_keys" -Value '', '` + line + `'`,
			CommandWindowsAdmin: `Add-Content -Path "$env:ProgramData\ssh\administrators_authorized_keys" -Value '', '` + line + `'`}, nil
	})
}

// ---------- the resource snapshot ----------

// sshOn: the request's scenario has the SSH servers switched on.
func (m *mock) sshOn(r *http.Request) bool {
	_, ok := sshScenarios[scenario(r)]
	return ok
}

// sshFix adds the servers' cards to a resource snapshot (Snapshot.Remotes,
// after Colab's) and takes their rows off the local device cards (the real
// back end lists remote devices only as remotes). The colab overlay calls it.
func (m *mock) sshFix(r *http.Request, path string, v any) any {
	if path != "/api/resources" || !m.sshOn(r) {
		return v
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	x := m.sshState(r)
	s := m.storeFor(r)
	now := time.Now()
	s.mu.Lock()
	x.mu.Lock()
	var cards []any
	titles := map[string]string{}
	progress := map[string]float64{}
	for _, l := range s.lectures {
		if dev := s.dev.assigned[l.ID]; model.IsSSHDevice(dev) {
			titles[dev] = l.Title
			progress[dev] = 0
			if j := stageIndex(&l, model.StageASR); j >= 0 {
				progress[dev] = l.Stages[j].Progress
			}
			titles[dev+"|id"] = l.ID
		}
	}
	for _, srv := range x.servers {
		cards = append(cards, toJSON(srv.card(s, now, titles, progress)))
	}
	x.mu.Unlock()
	s.mu.Unlock()
	if list, ok := obj["remotes"].([]any); ok {
		obj["remotes"] = append(list, cards...)
	} else if len(cards) > 0 {
		obj["remotes"] = cards
	}
	if devs, ok := obj["devices"].([]any); ok {
		keep := []any{}
		for _, d := range devs {
			if dm, ok := d.(map[string]any); ok {
				if id, _ := dm["id"].(string); model.IsSSHDevice(id) {
					continue
				}
			}
			keep = append(keep, d)
		}
		obj["devices"] = keep
	}
	return obj
}

// card is the server's remote.Card (sshremote.Server.Card's shape).
func (srv *sshSrv) card(s *store, now time.Time, titles map[string]string, progress map[string]float64) remote.Card {
	v := srv.view(s, now)
	c := remote.Card{ID: v.DeviceID, Kind: remote.KindSSH, Label: srv.name, State: v.State, Slots: 1, LastError: srv.lastErr,
		SSH: &remote.SSHInfo{AgentVersion: srv.agent, Storage: remote.StorageDisk, LatencyMS: v.LatencyMS}}
	if srv.present() {
		seen := now.Add(-2 * time.Second).UTC()
		c.SSH.LastSeen = &seen
	}
	if srv.state != sshremote.StateReady && srv.state != sshremote.StateBusy {
		return c
	}
	hw := srv.hw
	vram, vcpu, mem, one := int(hw.VRAMTotalMB), int(hw.CPUs), int(hw.MemTotalMB), 1
	c.Machine = &remote.Machine{Source: remote.SourceNvidiaSMI, GPUName: hw.GPUName, Driver: hw.Driver, CUDA: "13.0", GPUCount: &one,
		VRAMTotalMB: &vram, VCPUs: &vcpu, MemTotalMB: &mem, Storage: remote.StorageDisk}
	if hw.Metal {
		c.Machine.Source, c.Machine.Driver, c.Machine.CUDA = remote.SourceSystemProfiler, "", ""
	}
	busy := srv.running
	c.Usage = sshUsage(now, mem, busy)
	if busy {
		dev := v.DeviceID
		p := progress[dev]
		eta := int(math.Round((1 - p) * 420))
		c.Jobs = []remote.CardJob{{LectureID: titles[dev+"|id"], LectureTitle: titles[dev], JobID: "01JMOCKSSHJOB00000000000000", Stage: remote.StepTranscribe, Progress: &p, ETASec: &eta}}
	}
	return c
}

// sshUsage is a made-up reading with its 60 s history (the GPU busy while
// a lecture transcribes).
func sshUsage(now time.Time, memMB int, busy bool) *remote.Usage {
	i := now.Unix() / 2
	gpuBase, cpuBase := 4.0, 6.0
	if busy {
		gpuBase, cpuBase = 88, 21
	}
	hist := func(base, amp float64) []*int {
		out := make([]*int, remote.HistoryLen)
		for k := range out {
			v := wave(base, amp, i-int64(remote.HistoryLen-1-k))
			out[k] = &v
		}
		return out
	}
	gpu, cpu := hist(gpuBase, 6), hist(cpuBase, 4)
	whisper, system, data := 0, 3120, 0
	vramUsed, vramWhisper := 610, 0
	if busy {
		whisper, data, vramUsed, vramWhisper = 4380, 61, 5240, 4630
	}
	free := memMB - whisper - system - data
	temp, power := 41, 38.5
	if busy {
		temp, power = 67, 291.4
	}
	reserved := 312
	disk := data
	return &remote.Usage{At: now.UTC(), Source: remote.SourceWorker, CPUPct: cpu[len(cpu)-1], GPUUtilPct: gpu[len(gpu)-1],
		RAM:        []remote.RAMSegment{{Kind: remote.SegWhisper, MB: whisper}, {Kind: remote.SegSystem, MB: system}, {Kind: remote.SegData, MB: data}, {Kind: remote.SegFree, MB: free}},
		DataDiskMB: &disk, VRAMUsedMB: &vramUsed, VRAMReservedMB: &reserved, VRAMWhisperMB: &vramWhisper, GPUTempC: &temp, GPUPowerW: &power,
		CPUHistory: cpu, GPUHistory: gpu, VRAMHistory: hist(float64(vramUsed)/245.6, 1)}
}
