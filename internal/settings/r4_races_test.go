// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package settings

import (
	"errors"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// r4 D3: a probe result is stored only while the provider still has the
// type, base_url and key the probe was made with; a result that arrives
// after the user moved the provider elsewhere is dropped, so the cache
// UpdateProvider cleared stays clear.
func TestSetProbedModelsForDropsStaleResults(t *testing.T) {
	s, _ := newService(t)
	p := create(t, s, "P", model.ProviderOpenAI, "https://api.example.com/v1", fakeKey)
	cached := func() (model.ProbedModels, bool) {
		t.Helper()
		pm, ok, err := s.ProbedModels(bg(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return pm, ok
	}
	store := func(from model.Provider, models ...string) bool {
		t.Helper()
		stored, err := s.SetProbedModelsFor(bg(), from, model.ProbedModels{Models: models})
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}

	// Unchanged provider: stored.
	if !store(p, "m-1") {
		t.Fatal("a result for the current provider was dropped")
	}
	if pm, ok := cached(); !ok || len(pm.Models) != 1 || pm.Models[0] != "m-1" {
		t.Fatalf("cache %+v %v", pm, ok)
	}
	// A rename or a limit change does not touch what the probe talked to.
	conc := 3
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{Name: sp("Renamed"), MaxConcurrency: &conc}); err != nil {
		t.Fatal(err)
	}
	if !store(p, "m-2") {
		t.Fatal("a rename dropped the probe result")
	}

	// The probe started on the old address; the address changed while it ran.
	started := p
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{BaseURL: sp("https://other.example.org/v1")}); err != nil {
		t.Fatal(err)
	}
	if store(started, "old-address-model") {
		t.Fatal("a result of the old address was stored")
	}
	if pm, ok := cached(); ok {
		t.Fatalf("the old address's list came back: %+v", pm)
	}

	// The same for a new key, and for a new API type.
	cur, _ := s.Provider(p.ID)
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{APIKey: sp("sk-test-another-key-0000")}); err != nil {
		t.Fatal(err)
	}
	if store(cur, "old-key-model") {
		t.Fatal("a result made with the old key was stored")
	}
	cur, _ = s.Provider(p.ID)
	if _, err := s.UpdateProvider(bg(), p.ID, ProviderInput{Type: sp(model.ProviderAnthropic)}); err != nil {
		t.Fatal(err)
	}
	if store(cur, "old-type-model") {
		t.Fatal("a result of the old API type was stored")
	}
	if pm, ok := cached(); ok {
		t.Fatalf("a dropped result reached the cache: %+v", pm)
	}

	// Deleted meanwhile: not found, and no cache row for a provider that is gone.
	cur, _ = s.Provider(p.ID)
	if _, err := s.DeleteProvider(bg(), p.ID); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.SetProbedModelsFor(bg(), cur, model.ProbedModels{Models: []string{"x"}}); stored ||
		!errors.Is(err, db.ErrNotFound) || codeOf(err) != CodeProviderNotFound {
		t.Fatalf("deleted provider: stored %v, err %v", stored, err)
	}
	if pm, ok := cached(); ok {
		t.Fatalf("a deleted provider got a cache row: %+v", pm)
	}
}

// r4 D9: UpdateGeneral and UpdateASR read, edit and write under the write
// lock. While one update's edit runs, another update's edit must not start
// (it would see the settings without the first change and store them over
// it); after both, both changes are there.
func TestUpdateSettingsSerialisesReadModifyWrite(t *testing.T) {
	type update struct {
		name   string
		first  func(s *Service, entered chan<- struct{}, release <-chan struct{}) error
		second func(s *Service, ran chan<- struct{}) error
		check  func(t *testing.T, s *Service)
	}
	cases := []update{{
		name: "general",
		first: func(s *Service, entered chan<- struct{}, release <-chan struct{}) error {
			return s.UpdateGeneral(bg(), func(g *model.GeneralSettings) error {
				close(entered)
				<-release
				g.DailyBudget = 7
				return nil
			})
		},
		second: func(s *Service, ran chan<- struct{}) error {
			return s.UpdateGeneral(bg(), func(g *model.GeneralSettings) error {
				close(ran)
				off := false
				g.AutoChapters = &off
				return nil
			})
		},
		check: func(t *testing.T, s *Service) {
			if g := s.General(); g.DailyBudget != 7 || g.AutoChaptersOn() {
				t.Fatalf("general %+v: a change was lost", g)
			}
		},
	}, {
		name: "asr",
		first: func(s *Service, entered chan<- struct{}, release <-chan struct{}) error {
			return s.UpdateASR(bg(), func(a *model.ASRSettings) error {
				close(entered)
				<-release
				a.MaxParallel = 3
				return nil
			})
		},
		second: func(s *Service, ran chan<- struct{}) error {
			return s.UpdateASR(bg(), func(a *model.ASRSettings) error {
				close(ran)
				a.Language = "de"
				return nil
			})
		},
		check: func(t *testing.T, s *Service) {
			if a := s.ASR(); a.MaxParallel != 3 || a.Language != "de" {
				t.Fatalf("asr %+v: a change was lost", a)
			}
		},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, st := newService(t)
			entered, release, ran := make(chan struct{}), make(chan struct{}), make(chan struct{})
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			go func() { firstDone <- c.first(s, entered, release) }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the first update did not start")
			}
			go func() { secondDone <- c.second(s, ran) }()
			// Correct code never lets the second edit run here; the wait
			// only bounds how long a broken lock gets to show itself.
			select {
			case <-ran:
				close(release)
				<-firstDone
				t.Fatal("the second update read the settings while the first was editing them")
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			for i, ch := range []chan error{firstDone, secondDone} {
				select {
				case err := <-ch:
					if err != nil {
						t.Fatalf("update %d: %v", i+1, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("update %d did not finish", i+1)
				}
			}
			c.check(t, s)
			s2, err := Open(bg(), st, nil) // and stored so
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, s2)
		})
	}
}

// An edit error is returned as it is and nothing is written; validation
// still applies to the edited value.
func TestUpdateGeneralEditErrorAndValidation(t *testing.T) {
	s, _ := newService(t)
	before := s.General()
	sentinel := errors.New("bad body")
	if err := s.UpdateGeneral(bg(), func(g *model.GeneralSettings) error {
		g.DailyBudget = 9
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("edit error: %v", err)
	}
	if err := s.UpdateGeneral(bg(), func(g *model.GeneralSettings) error {
		g.DailyBudget = -1
		return nil
	}); !errors.Is(err, ErrInvalid) || codeOf(err) != CodeBudgetInvalid {
		t.Fatalf("negative budget: %v", err)
	}
	if err := s.UpdateASR(bg(), func(a *model.ASRSettings) error {
		a.MaxParallel = 0
		return nil
	}); !errors.Is(err, ErrInvalid) || codeOf(err) != CodeMaxParallelInvalid {
		t.Fatalf("asr max_parallel 0: %v", err)
	}
	if g := s.General(); g.DailyBudget != before.DailyBudget || g.AutoChaptersOn() != before.AutoChaptersOn() {
		t.Fatalf("a failed update changed the settings: %+v, was %+v", g, before)
	}
	if a := s.ASR(); a.MaxParallel != DefaultASR().MaxParallel {
		t.Fatalf("a failed ASR update changed max_parallel: %d", a.MaxParallel)
	}
}
