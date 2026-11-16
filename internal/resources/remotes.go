// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

// Snapshot.Remotes (colab-obs-core): the remote devices as remote.Card.
// Colab's card is built from its Status (Deps.Remote) and the scheduler's
// colab:remote row (which lecture each running job is, its title as the
// device cards show it). The Service's build adds them next to
// Snapshot.Remote: snap.Remotes = remoteCards(remote, devIn). ssh-1b: the
// SSH servers' cards (Deps.RemoteCards, sshremote's) follow it, their
// running lectures titled as the device cards title them.

import (
	"context"
	"math"

	"lectureflow/internal/colab"
	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/sched"
)

// remoteCards is Snapshot.Remotes: nil without a remote device (Colab not
// configured or switched off).
func remoteCards(st *colab.Status, in deviceInput) []remote.Card {
	if st == nil {
		return nil
	}
	return []remote.Card{ColabCard(*st, in.statuses[model.DeviceColab].Running, in.titles)}
}

// ColabCard is Colab's card: its Status in Card.Colab, the runtime's
// Machine and Usage, the running lectures (running: the scheduler's jobs on
// colab:remote, for their lecture ids; titles: lecture id → lecture, as for
// the device cards' tasks), slots, the queue and the lectures uploaded
// ahead.
func ColabCard(st colab.Status, running []sched.RunningASR, titles map[string]model.Lecture) remote.Card {
	c := remote.Card{
		ID: model.DeviceColab, Kind: remote.KindColab, Label: sched.RemoteLabel(st.Accelerator), State: st.State,
		Machine: st.Machine, Usage: st.Usage, Slots: st.Slots, Queued: st.Planned, UploadedAhead: st.UploadedAhead,
		Colab: st,
	}
	lecture := map[string]string{}
	for _, r := range running {
		lecture[r.JobID] = r.LectureID
	}
	for _, j := range st.Jobs {
		cj := remote.CardJob{LectureID: lecture[j.JobID], JobID: j.JobID, Stage: cardStage(j), Split: j.Split, Reconnect: j.Reconnect}
		cj.LectureTitle = titles[cj.LectureID].Title
		p := math.Max(0, math.Min(1, j.Overall))
		cj.Progress = &p
		if j.ETASec >= 0 && !math.IsInf(j.ETASec, 0) && !math.IsNaN(j.ETASec) {
			eta := int(math.Round(j.ETASec))
			cj.ETASec = &eta
		}
		c.Jobs = append(c.Jobs, cj)
	}
	if e := st.LastError; e != nil {
		ce := &remote.CardError{Code: e.Code, Params: e.Params}
		if st.LastErrorAt != nil {
			ce.At = *st.LastErrorAt
		}
		c.LastError = ce
	}
	return c
}

// cardStage is a lecture's remote stage: the worker's own while it runs
// there (check, install, download_model, load_model, transcribe, write),
// else the step (wait, upload, download, verify).
func cardStage(j colab.JobInfo) string {
	switch j.Step {
	case colab.StepRemoteTranscribe:
		if j.Stage != "" {
			return j.Stage
		}
		return "check"
	case colab.StepUploadAudio:
		return "upload"
	case colab.StepDownloadResult:
		return "download"
	case colab.StepVerify:
		return "verify"
	}
	return "wait"
}

// remoteCard names remote.Card where build's local variable "remote" (the
// Colab status) hides the package.
type remoteCard = remote.Card

// titledCards gives the SSH servers' running lectures their titles (ssh-1b):
// the ones the device cards show (titles), else read from the store.
func (s *Service) titledCards(ctx context.Context, cards []remote.Card, titles map[string]model.Lecture) []remote.Card {
	for i := range cards {
		for j := range cards[i].Jobs {
			cj := &cards[i].Jobs[j]
			if cj.LectureTitle != "" || cj.LectureID == "" {
				continue
			}
			l, ok := titles[cj.LectureID]
			if !ok && s.d.Store != nil {
				if got, err := s.d.Store.GetLecture(ctx, cj.LectureID); err == nil {
					l, ok = got, true
				}
			}
			if ok {
				cj.LectureTitle = l.Title
			}
		}
	}
	return cards
}

// Clients is the number of connected SSE clients (SetClients): Colab reads
// its runtime's resources itself only while a front end watches
// (colab.Backend.SetViewers).
func (s *Service) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients
}
