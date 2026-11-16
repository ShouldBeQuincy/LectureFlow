// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// The server's resource card (remote.Card; the resource bar draws it in
// plan step 2). Readings come from the agent's machine / sample events
// ("远端资源采样"); times are this machine's (remote clocks are not
// trusted).

import (
	"context"
	"encoding/json"
	"sort"

	"lectureflow/internal/remote"
)

// Card is the server's card (remote.Device).
func (s *Server) Card(context.Context) remote.Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	if s.set.Paused {
		state = StatePaused
	} else if state == StateReady && len(s.runs) >= s.slotsLocked() {
		state = StateBusy
	}
	c := remote.Card{ID: s.devID, Kind: remote.KindSSH, Label: s.rec.Name, State: state, Slots: s.slotsLocked(),
		LastError: s.lastErr, SSH: &remote.SSHInfo{AgentVersion: s.rec.AgentVersion, Storage: remote.StorageDisk}}
	if s.c != nil && s.c.alive() {
		if ms := int(s.c.latency.Load()); ms > 0 {
			c.SSH.LatencyMS = &ms
		}
	}
	if !s.lastSeen.IsZero() {
		t := s.lastSeen.UTC()
		c.SSH.LastSeen = &t
	}
	for _, j := range s.runs {
		c.Jobs = append(c.Jobs, *j)
	}
	sort.Slice(c.Jobs, func(a, b int) bool { return c.Jobs[a].JobID < c.Jobs[b].JobID }) // job ids are ULIDs: by start
	if s.c != nil {
		c.Machine = s.samples.Machine()
		c.Usage = s.samples.Usage(s.m.now())
	}
	return c
}

func jsonString(v any, def string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return def
	}
	return string(b)
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
