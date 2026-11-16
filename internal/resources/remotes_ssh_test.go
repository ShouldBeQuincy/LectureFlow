// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"testing"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
)

// ssh-1b: the SSH servers' cards follow Colab's in Snapshot.Remotes; a
// running lecture gets its title from the store when the card has none;
// the SSH rows stay out of the local device cards.
func TestSnapshotHasTheSSHCards(t *testing.T) {
	f := newDevFixture(t)
	f.rows = append(f.rows, model.ASRDevice{ID: "ssh:gpu-box", Backend: model.BackendFasterWhisper, Label: "gpu-box", Enabled: true,
		Model: "large-v3", Precision: "auto", MaxParallel: 1, Priority: 900})
	clk := newClock()
	d := f.deps(clk)
	p := 0.4
	d.RemoteCards = func(context.Context) []remote.Card {
		return []remote.Card{
			{ID: "ssh:gpu-box", Kind: remote.KindSSH, Label: "gpu-box", State: "busy", Slots: 1,
				Jobs: []remote.CardJob{{LectureID: f.lec.ID, JobID: "01JOB", Stage: remote.StepTranscribe, Progress: &p}}},
			{ID: "ssh:mac", Kind: remote.KindSSH, Label: "mac", State: "offline", Slots: 1,
				Jobs: []remote.CardJob{{LectureID: "gone", JobID: "01JOB2", Stage: remote.StepConnect}, {LectureID: f.lec.ID, LectureTitle: "kept", JobID: "01JOB3"}}},
		}
	}
	s := New(d)
	s.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	snap := s.Snapshot(context.Background())
	if len(snap.Remotes) != 2 || snap.Remotes[0].ID != "ssh:gpu-box" || snap.Remotes[1].ID != "ssh:mac" {
		t.Fatalf("remotes %+v", snap.Remotes)
	}
	if j := snap.Remotes[0].Jobs[0]; j.LectureTitle != "Week 3 Ciphers" || *j.Progress != 0.4 {
		t.Fatalf("job %+v", j)
	}
	if j := snap.Remotes[1].Jobs; j[0].LectureTitle != "" || j[1].LectureTitle != "kept" {
		t.Fatalf("titles: an unknown lecture stays untitled, a given title stays: %+v", j)
	}
	for _, c := range snap.Devices {
		if model.IsSSHDevice(c.ID) {
			t.Fatalf("an SSH server among the local device cards: %+v", c)
		}
	}
	// No SSH servers (off): nothing added.
	d.RemoteCards = nil
	s2 := New(d)
	s2.newEngines = (&fakeEngines{reads: [][]EngineSample{nil}}).open
	if r := s2.Snapshot(context.Background()).Remotes; r != nil {
		t.Fatalf("remotes without servers: %+v", r)
	}
}
