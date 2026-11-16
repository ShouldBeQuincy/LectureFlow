// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The SSH inference servers (ssh-mvp; .claude/research/remote-ssh-plan.md
// §6): add, edit, remove, connect (shows the host key), trust (pins it),
// install the environment, test, the public key to authorize, and "改用
// <服务器> 处理" for a lecture. The servers come from the scheduler
// (sched.Scheduler.SSH, an *sshremote.Manager wired by cmd only with
// LECTUREFLOW_EXPERIMENTAL_SSH=1; security review 3, L6); without them every
// endpoint answers 404 api.not_found, as if it did not exist. No body holds
// a private key or any secret: a server's view has the public key and
// fingerprints only.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"lectureflow/internal/model"
	"lectureflow/internal/sched"
	"lectureflow/internal/sshremote"
)

// remoteServers is what the endpoints need from the SSH servers.
type remoteServers interface {
	List() []sshremote.ServerView
	Get(id string) (sshremote.ServerView, bool)
	Add(ctx context.Context, req sshremote.AddRequest) (sshremote.ServerView, error)
	Update(ctx context.Context, id string, p sshremote.Patch) (sshremote.ServerView, error)
	Remove(ctx context.Context, id string, uninstall bool) error
	Connect(ctx context.Context, id string) (sshremote.ConnectResult, error)
	Trust(ctx context.Context, id string, req sshremote.TrustRequest) (sshremote.ConnectResult, error)
	Install(id string) error
	Test(ctx context.Context, id string) (sshremote.ServerView, error)
	PublicKeyOf(id string) (sshremote.PublicKey, error)
}

// API codes of these endpoints (the servers' own codes, remote.*, are the
// back end's: error_codes_backend.json).
const (
	codeRemoteServerNotFound  = "remote_server.not_found"
	codeRemoteServerInvalid   = "remote_server.invalid"
	codeRemoteServerExists    = "remote_server.exists"
	codeRemoteServerBusy      = "remote_server.busy"
	codeRemoteServerNoKey     = "remote_server.credentials_unavailable"
	codeRemoteServerKeyPinned = "remote_server.host_key_pinned"
)

var remoteServerCodes = map[string]errDef{
	codeRemoteServerNotFound: {"找不到这台远程服务器（可能已被移除），请刷新页面", nil},
	codeRemoteServerInvalid:  {"远程服务器的设置无效（{field}）", []string{"field"}},
	codeRemoteServerExists:   {"已经有一台同名（{id}）的远程服务器", []string{"id"}},
	codeRemoteServerBusy:     {"这台远程服务器正在转写，请等它结束或先取消再改", nil},
	codeRemoteServerNoKey:    {"本机的凭据存储不可用，无法为远程服务器生成或读取密钥。请查看“诊断”页", nil},
	// SEC7 M1: trust for a host key that is confirmed already
	codeRemoteServerKeyPinned: {"这台远程服务器的主机密钥已经确认过，没有替换。请重新连接，在“主机密钥已变化”一步核对新旧指纹后再确认", nil},
}

func init() {
	for code, def := range remoteServerCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("remote server code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/remote/servers", s.listRemoteServers)
		h("POST /api/remote/servers", s.addRemoteServer)
		h("GET /api/remote/servers/{id}", s.getRemoteServer)
		h("PATCH /api/remote/servers/{id}", s.patchRemoteServer)
		h("DELETE /api/remote/servers/{id}", s.deleteRemoteServer)
		h("POST /api/remote/servers/{id}/connect", s.connectRemoteServer)
		h("POST /api/remote/servers/{id}/trust", s.trustRemoteServer)
		h("POST /api/remote/servers/{id}/install", s.installRemoteServer)
		h("POST /api/remote/servers/{id}/test", s.testRemoteServer)
		h("GET /api/remote/servers/{id}/pubkey", s.remoteServerKey)
		h("POST /api/lectures/{id}/use-remote", s.useRemoteServer)
	})
	// ssh-1b: the diagnostics zip lists the servers without their host
	// names and user names (remote-servers.json), and the redaction of every
	// file of the zip (the logs too) replaces them. SEC7 M2: the servers'
	// names, ids and device ids become numbers (server-1, …; the same in
	// every file of the zip), and the words come from every server ever
	// configured (remoteRedactHistory), also after it was removed and while
	// the SSH servers are off.
	diagSections = append(diagSections, DiagSection{Name: "remote-servers.json", Collect: func(ctx context.Context, s *Server) (any, bool) {
		rs, ok := s.sshServers()
		if !ok {
			return nil, false
		}
		labels := map[string]string{}
		for _, e := range s.diagRemotePlan(ctx) {
			labels[e.ID] = e.label
		}
		list := rs.List()
		out := make([]sshremote.ServerView, 0, len(list))
		for _, v := range list {
			out = append(out, redactServerView(v, labels[v.ID]))
		}
		return map[string]any{"servers": out}, true
	}})
	redactWordSources = append(redactWordSources, func(ctx context.Context, s *Server) []redactWord {
		return remoteRedactWords(s.diagRemotePlan(ctx))
	})
}

// diagRemotePlan is remoteRedactPlan, computed once per diagnostics export
// (N31-I1): the words of the redaction and the labels of
// remote-servers.json number the servers alike, also when one is added or
// removed while the zip is made. A server added since has no label there
// (redactServerView: "server") and its device id becomes ssh:<server>.
func (s *Server) diagRemotePlan(ctx context.Context) []remoteRedactEntry {
	return diagOnce(ctx, "remote-redact-plan", func() []remoteRedactEntry { return s.remoteRedactPlan(ctx) })
}

// What a server's host and user names become in the diagnostics.
const (
	redactedHost = "<host>"
	redactedUser = "<user>"
)

// redactServerView is a server for the diagnostics zip: no host or user
// name (its own or a jump host's), no public key line (its fingerprint
// stays), no pinned host keys; its name, id and device id are label
// (server-N; SEC7 M2), and so is the device of its last error.
func redactServerView(v sshremote.ServerView, label string) sshremote.ServerView {
	if label == "" {
		label = "server"
	}
	v.Name, v.ID, v.DeviceID = label, label, model.SSHDeviceID(label)
	if v.LastError != nil {
		le := *v.LastError
		params := make(map[string]any, len(le.Params))
		for k, p := range le.Params {
			params[k] = p
		}
		if _, ok := params["device"]; ok {
			params["device"] = label
		}
		le.Params = params
		v.LastError = &le
	}
	v.Host, v.Username, v.PublicKey = redactedHost, redactedUser, ""
	jumps := make([]sshremote.Jump, len(v.Jumps))
	for i, j := range v.Jumps {
		j.Host, j.Username, j.HostKey = redactedHost, redactedUser, ""
		jumps[i] = j
	}
	v.Jumps = jumps
	return v
}

// sshServers returns the SSH servers (ok false while they are off).
func (s *Server) sshServers() (remoteServers, bool) {
	if s.sc == nil {
		return nil, false
	}
	rs, ok := s.sc.SSH().(remoteServers)
	return rs, ok && rs != nil
}

// sshDeviceOptions gives an SSH server's device row the model its memory
// suits as the recommendation (ssh-1b; the local recommendation table
// knows only this machine's devices).
func (s *Server) sshDeviceOptions(v *ASRDeviceView) {
	rs, ok := s.sshServers()
	if !ok {
		return
	}
	if sv, found := rs.Get(model.SSHSlug(v.ID)); found && sv.Recommended != nil {
		v.Options.Recommended = &ModelPrecision{Model: sv.Recommended.Model, Precision: sv.Recommended.Precision}
	}
}

// applySSHGroup moves a server's device row to its group's place in the
// device order (ssh-1b: the wizard asks "优先使用 / 本机忙时补充" after
// the first probe created the row): GroupPrefer right above the first
// local device, GroupSupplement right below the last one (above Colab).
// Without a row yet the manager places it when it creates it.
func (s *Server) applySSHGroup(ctx context.Context, devID, group string) error {
	if s.dev == nil {
		return nil
	}
	rows := s.dev.List()
	ids := make([]string, 0, len(rows))
	found := false
	for _, d := range rows {
		if d.ID == devID {
			found = true
			continue
		}
		ids = append(ids, d.ID)
	}
	if !found {
		return nil
	}
	first, last := -1, -1
	for i, id := range ids {
		if !model.IsRemoteDevice(id) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	at := 0
	switch {
	case group == sshremote.GroupPrefer && first >= 0:
		at = first
	case group == sshremote.GroupPrefer:
		at = 0
	case last >= 0:
		at = last + 1
	default: // no local device: before Colab
		at = len(ids)
		for i, id := range ids {
			if id == model.DeviceColab {
				at = i
				break
			}
		}
	}
	ids = append(ids[:at], append([]string{devID}, ids[at:]...)...)
	_, err := s.dev.Reorder(ctx, ids)
	return err
}

// remoteSrv returns the servers, or answers 404 api.not_found (the SSH
// servers are off: the endpoints do not exist).
func (s *Server) remoteSrv(w http.ResponseWriter, r *http.Request) (remoteServers, bool) {
	if s.sc != nil {
		if rs, ok := s.sc.SSH().(remoteServers); ok && rs != nil {
			return rs, true
		}
	}
	writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
	return nil, false
}

// failRemote maps the servers' errors.
func (s *Server) failRemote(w http.ResponseWriter, r *http.Request, id string, err error) {
	var fe *sshremote.FieldError
	switch {
	case errors.Is(err, sshremote.ErrNotFound):
		writeError(w, http.StatusNotFound, codeRemoteServerNotFound, nil)
	case errors.As(err, &fe):
		writeError(w, http.StatusBadRequest, codeRemoteServerInvalid, map[string]any{"field": fe.Field})
	case errors.Is(err, sshremote.ErrExists):
		writeError(w, http.StatusConflict, codeRemoteServerExists, map[string]any{"id": id})
	case errors.Is(err, sshremote.ErrBusy):
		writeError(w, http.StatusConflict, codeRemoteServerBusy, nil)
	case errors.Is(err, sshremote.ErrNoDataKey):
		writeError(w, http.StatusServiceUnavailable, codeRemoteServerNoKey, nil)
	case errors.Is(err, sshremote.ErrHostKeyPinned):
		writeError(w, http.StatusConflict, codeRemoteServerKeyPinned, nil)
	case r.Context().Err() != nil:
	default:
		// A connection's failure: the server's own code (remote.*), 502;
		// a changed host key names the presented key (SEC7 M1).
		name := id
		if rs, ok := s.sc.SSH().(remoteServers); ok {
			if v, ok := rs.Get(id); ok {
				name = v.Name
			}
		}
		params := map[string]any{"device": name}
		if fp := sshremote.PresentedFingerprint(err); fp != "" {
			params["fingerprint"] = fp
		}
		writeError(w, http.StatusBadGateway, sshremote.ConnErrorCode(err), params)
	}
}

func (s *Server) listRemoteServers(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": rs.List()})
}

func (s *Server) addRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var req sshremote.AddRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := rs.Add(r.Context(), req)
	if err != nil {
		s.failRemote(w, r, req.ID, err)
		return
	}
	s.rememberRemoteServers(r.Context(), v)
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	v, found := rs.Get(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, codeRemoteServerNotFound, nil)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var p sshremote.Patch
	if err := decodeJSON(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	before, _ := rs.Get(id)
	v, err := rs.Update(r.Context(), id, p)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.rememberRemoteServers(r.Context(), before, v) // the old host and names stay redacted
	// Only a new group moves the device row: the wizard sends its group
	// every time it ends, and an unchanged one must keep the place the
	// user dragged the row to. The group is saved either way; when the row
	// cannot be moved (the device list changed meanwhile) the answer is
	// still the saved server, and the row stays where it was.
	if p.Group != nil && v.Settings.Group != before.Settings.Group {
		if err := s.applySSHGroup(r.Context(), v.DeviceID, v.Settings.Group); err != nil {
			slog.Warn("remote server: the device row keeps its place", "device", v.DeviceID, "group", v.Settings.Group, "err", err)
		}
	}
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusOK, v)
}

// DELETE /api/remote/servers/{id}?uninstall=1: with uninstall, lf-agent
// removes its directories (environment, models, jobs) on the server first.
func (s *Server) deleteRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	uninstall := r.URL.Query().Get("uninstall") == "1"
	if v, found := rs.Get(id); found {
		s.rememberRemoteServers(r.Context(), v) // its words stay redacted after it is gone
	}
	if err := rs.Remove(r.Context(), id, uninstall); err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.hub.Changed("devices", "")
	w.WriteHeader(http.StatusNoContent)
}

// POST …/connect: without a confirmed host key the answer holds the
// presented key (host_key: algorithm, fingerprint, the command that shows
// it on the server, the user's known_hosts verdict); nothing else is sent.
// A confirmed key the server no longer presents (SEC7 M1): 200 with the
// presented key, changed: true and previous_fingerprint; nothing is pinned
// and the server's state is host_key_changed.
func (s *Server) connectRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	res, err := rs.Connect(r.Context(), id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// TrustRequest confirms a presented host key (sshremote.TrustRequest): a
// hop whose key is confirmed already answers 409
// remote_server.host_key_pinned unless replace is true and previous is the
// confirmed key's fingerprint (SEC7 M1).
type TrustRequest = sshremote.TrustRequest

func (s *Server) trustRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	var req TrustRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	res, err := rs.Trust(r.Context(), id, req)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	s.hub.Changed("devices", "")
	writeJSON(w, http.StatusOK, res)
}

// POST …/install: installs the environment in the background (202); the
// server's view shows the steps (install) and its state.
func (s *Server) installRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := rs.Install(id); err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	v, _ := rs.Get(id)
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) testRemoteServer(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	v, err := rs.Test(r.Context(), id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) remoteServerKey(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteSrv(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	k, err := rs.PublicKeyOf(id)
	if err != nil {
		s.failRemote(w, r, id, err)
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// UseRemoteRequest is "改用 <服务器> 处理".
type UseRemoteRequest struct {
	Device string `json:"device"` // "ssh:<id>"
}

func (s *Server) useRemoteServer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.remoteSrv(w, r); !ok {
		return
	}
	var req UseRemoteRequest
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	if _, err := s.getLectureRow(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sc.UseSSH(r.Context(), id, req.Device); err != nil {
		if errors.Is(err, sched.ErrSSHNotConfigured) {
			writeError(w, http.StatusNotFound, "api.not_found", map[string]any{"method": r.Method, "path": r.URL.Path})
			return
		}
		s.fail(w, r, err)
		return
	}
	s.respondLecture(w, r, id, http.StatusOK)
}
