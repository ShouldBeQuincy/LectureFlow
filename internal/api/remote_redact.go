// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The words of the SSH servers the diagnostics redact (SEC7 M2): the host
// names, user names, names and slugs of every server ever configured, not
// only of those that exist when the zip is made. A removed server, an
// edited host or a log written while the SSH servers were on stay redacted
// after the server is gone or the switch is off.
//
// The history is kept in the kv row kvRemoteRedactHistory of this
// machine's database. It is written when a server is added, edited or
// removed through the API, and when the diagnostics read the servers'
// rows (so servers configured before the history existed join it). Nothing
// removes it. It is sensitive itself: it never goes into the diagnostics
// zip (database.json has row counts only) or into a log.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
	"lectureflow/internal/sshremote"
)

const kvRemoteRedactHistory = "remote.ssh.redact_history"

// remoteRedactEntry is one server of the history (by its id, the slug).
type remoteRedactEntry struct {
	ID    string   `json:"id"`
	Names []string `json:"names,omitempty"`
	Hosts []string `json:"hosts,omitempty"`
	Users []string `json:"users,omitempty"`

	label string // server-N in one zip (remoteRedactPlan)
}

// remoteRedactMu serializes the history's read-modify-write.
var remoteRedactMu sync.Mutex

// remoteServerLister reads the servers' rows (db's sqlStore; not part of
// db.Store): the diagnostics read them with the SSH servers off too.
type remoteServerLister interface {
	ListRemoteServers(ctx context.Context) ([]db.RemoteServer, error)
}

func remoteEntryOfView(v sshremote.ServerView) remoteRedactEntry {
	e := remoteRedactEntry{ID: v.ID, Names: []string{v.Name}, Hosts: []string{v.Host}, Users: []string{v.Username}}
	for _, j := range v.Jumps {
		e.Hosts = append(e.Hosts, j.Host)
		e.Users = append(e.Users, j.Username)
	}
	return e
}

func remoteEntryOfRow(r db.RemoteServer) remoteRedactEntry {
	e := remoteRedactEntry{ID: r.ID, Names: []string{r.Name}, Hosts: []string{r.Host}, Users: []string{r.Username}}
	var jumps []sshremote.Jump
	_ = json.Unmarshal([]byte(r.JumpJSON), &jumps)
	for _, j := range jumps {
		e.Hosts = append(e.Hosts, j.Host)
		e.Users = append(e.Users, j.Username)
	}
	return e
}

// mergeRemoteEntry adds e's words to hist (true: something new).
func mergeRemoteEntry(hist map[string]*remoteRedactEntry, e remoteRedactEntry) bool {
	if e.ID == "" {
		return false
	}
	cur, ok := hist[e.ID]
	if !ok {
		cur = &remoteRedactEntry{ID: e.ID}
		hist[e.ID] = cur
	}
	changed := !ok
	add := func(list *[]string, vals []string) {
		for _, v := range vals {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(*list, v) {
				*list = append(*list, v)
				changed = true
			}
		}
	}
	add(&cur.Names, e.Names)
	add(&cur.Hosts, e.Hosts)
	add(&cur.Users, e.Users)
	return changed
}

// loadRemoteHistory reads the history (empty when there is none or it
// cannot be read). The caller holds remoteRedactMu.
func (s *Server) loadRemoteHistory(ctx context.Context) map[string]*remoteRedactEntry {
	hist := map[string]*remoteRedactEntry{}
	if s.st == nil {
		return hist
	}
	v, ok, err := s.st.GetKV(ctx, kvRemoteRedactHistory)
	if err != nil || !ok || v == "" {
		return hist
	}
	var stored struct {
		Servers []remoteRedactEntry `json:"servers"`
	}
	if json.Unmarshal([]byte(v), &stored) != nil {
		return hist
	}
	for _, e := range stored.Servers {
		mergeRemoteEntry(hist, e)
	}
	return hist
}

// saveRemoteHistory writes the history. The caller holds remoteRedactMu.
func (s *Server) saveRemoteHistory(ctx context.Context, hist map[string]*remoteRedactEntry) {
	if s.st == nil {
		return
	}
	ids := make([]string, 0, len(hist))
	for id := range hist {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	list := make([]remoteRedactEntry, 0, len(ids))
	for _, id := range ids {
		list = append(list, *hist[id])
	}
	b, _ := json.Marshal(map[string]any{"servers": list})
	if err := s.st.SetKV(context.WithoutCancel(ctx), kvRemoteRedactHistory, string(b)); err != nil {
		// the error is the database's: it holds none of the words
		slog.Warn("remote server: the diagnostics' redaction words could not be saved", "err", err)
	}
}

// rememberRemoteServers adds the servers' words to the history.
func (s *Server) rememberRemoteServers(ctx context.Context, views ...sshremote.ServerView) {
	if s.st == nil {
		return
	}
	remoteRedactMu.Lock()
	defer remoteRedactMu.Unlock()
	hist := s.loadRemoteHistory(ctx)
	changed := false
	for _, v := range views {
		if mergeRemoteEntry(hist, remoteEntryOfView(v)) {
			changed = true
		}
	}
	if changed {
		s.saveRemoteHistory(ctx, hist)
	}
}

// remoteRedactPlan is every server ever configured, numbered for one zip:
// the servers that exist (their rows, read with the SSH servers off too)
// by id, then the removed ones by id; label is server-1, server-2, … The
// existing servers join the history.
func (s *Server) remoteRedactPlan(ctx context.Context) []remoteRedactEntry {
	remoteRedactMu.Lock()
	defer remoteRedactMu.Unlock()
	hist := s.loadRemoteHistory(ctx)
	current := map[string]bool{}
	changed := false
	note := func(e remoteRedactEntry) {
		if mergeRemoteEntry(hist, e) {
			changed = true
		}
		current[e.ID] = true
	}
	if ls, ok := s.st.(remoteServerLister); ok {
		if rows, err := ls.ListRemoteServers(ctx); err == nil {
			for _, r := range rows {
				note(remoteEntryOfRow(r))
			}
		}
	}
	if rs, ok := s.sshServers(); ok {
		for _, v := range rs.List() {
			note(remoteEntryOfView(v))
		}
	}
	if changed {
		s.saveRemoteHistory(ctx, hist)
	}
	ids := make([]string, 0, len(hist))
	for id := range hist {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if current[a] != current[b] {
			if current[a] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	out := make([]remoteRedactEntry, 0, len(ids))
	for i, id := range ids {
		e := *hist[id]
		e.label = "server-" + strconv.Itoa(i+1)
		out = append(out, e)
	}
	return out
}

// genericRemoteWords are names and slugs replaced only as "ssh:<slug>":
// as whole words they are the application's own vocabulary (a server
// named "GPU" must not hide every "gpu" of the logs), and they say
// nothing about the server.
var genericRemoteWords = map[string]bool{
	"cpu": true, "gpu": true, "npu": true, "igpu": true, "dgpu": true, "cuda": true, "mlx": true,
	"auto": true, "local": true, "localhost": true, "remote": true, "server": true, "servers": true,
	"ssh": true, "colab": true, "mac": true, "macos": true, "linux": true, "windows": true,
	"default": true, "test": true, "host": true, "device": true,
}

// genericRemoteUsers are user names that name no one (an image's default
// account, a placeholder): replaced only as the user of "user@host"
// (N31-L2) and in a path ("/root/…", "/scratch/root/…"; formUser), so
// "root cause" or "the user" stay readable. Compared in lower
// case. No word that may be a person's name (SEC32 L-2: not "rocky" or
// "dev", and not genericRemoteWords, which has "mac").
var genericRemoteUsers = map[string]bool{
	"root": true, "admin": true, "administrator": true, "user": true, "ubuntu": true, "debian": true,
	"centos": true, "fedora": true, "almalinux": true, "ec2-user": true, "azureuser": true,
	"opc": true, "pi": true, "git": true, "guest": true, "student": true, "lab": true,
	// SEC32b: not only a server word; as a user name everywhere it hid the
	// slow-request log's ".../{id}/test" routes and every "test" key
	"test": true,
}

// genericRemoteHosts are single-word host names that name no one (a
// short name on the local network): replaced only in an address
// ("user@gpu", "ssh://gpu", "[gpu]", "lookup gpu", "gpu:22"; N31-L2). A host name with a
// dot and an address are never generic: they are replaced everywhere.
var genericRemoteHosts = map[string]bool{
	"lab": true, "nas": true, "box": true, "node": true, "pc": true, "desktop": true, "laptop": true,
	"workstation": true, "gateway": true, "jump": true, "jumphost": true, "bastion": true, "router": true,
	"vm": true, "cloud": true, "cluster": true, "compute": true, "login": true, "head": true, "master": true,
	"main": true, "worker": true, "work": true, "home": true, "office": true, "dev": true, "prod": true,
	"build": true, "ubuntu": true, "debian": true, "raspberrypi": true, "pi": true,
}

// keptHost: a host that is this machine (a tunnel's end) is not redacted.
func keptHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && (a.Unmap().IsLoopback() || a.IsUnspecified())
}

// remoteRedactWords are the plan's words: names (any case) and slugs
// become the server's label, "ssh:<slug>" becomes "ssh:<label>", host
// names (any case) <host>, user names <user>. Host and user names come
// last: a name that is also a host name is replaced as the host. Generic
// host and user names are replaced in addresses only (wordForm).
func remoteRedactWords(plan []remoteRedactEntry) []redactWord {
	var out []redactWord
	for _, e := range plan {
		out = append(out, redactWord{word: model.SSHDeviceID(e.ID), repl: model.SSHDeviceID(e.label)})
		if !genericRemoteWords[strings.ToLower(e.ID)] {
			out = append(out, redactWord{word: e.ID, repl: e.label, fold: true})
		}
		for _, n := range e.Names {
			if !genericRemoteWords[strings.ToLower(n)] {
				out = append(out, redactWord{word: n, repl: e.label, fold: true})
			}
		}
	}
	for _, e := range plan {
		for _, h := range e.Hosts {
			if keptHost(h) {
				continue
			}
			w := redactWord{word: h, repl: redactedHost, fold: true}
			if l := strings.ToLower(h); genericRemoteHosts[l] || genericRemoteWords[l] {
				w.form = formHost
			}
			out = append(out, w)
		}
		for _, u := range e.Users {
			w := redactWord{word: u, repl: redactedUser}
			if genericRemoteUsers[strings.ToLower(u)] {
				w.form = formUser
			}
			out = append(out, w)
		}
	}
	return out
}
