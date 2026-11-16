// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// Batch translation and DeepSeek's prompt cache (acceptance T2: the batches
// sent after the warm-up all hit 0 cached tokens). On DeepSeek a batch run
// warms up in two steps: the first chunk alone, then the second chunk
// alone, then the rest. Other providers, and the single path on DeepSeek,
// keep the one-step warm-up.

// cacheLecture is a lecture of n generated sentences with batch (nil: no
// batch in the snapshot), whose providers point at DeepSeek's API when
// deepSeek (the script client sends nothing anywhere).
func cacheLecture(t *testing.T, n int, batch int, deepSeek bool) (*env, model.Lecture) {
	t.Helper()
	e := newEnv(t)
	return e, e.cacheLecture(n, batch, deepSeek)
}

// cacheLecture makes the lecture of cacheLecture in e.
func (e *env) cacheLecture(n int, batch int, deepSeek bool) model.Lecture {
	e.t.Helper()
	if deepSeek {
		for name, p := range e.cfg.LLM.Providers {
			p.BaseURL = "https://api.deepseek.com"
			e.cfg.LLM.Providers[name] = p
		}
	}
	l := e.lecture("L1", "m.mp4")
	ss := make([]model.Sentence, n)
	for i := range ss {
		text := fmt.Sprintf("Now we look at item %d of the list.", i)
		ss[i] = model.Sentence{LectureID: l.ID, Idx: i, StartMS: int64(i) * 1000, EndMS: int64(i)*1000 + 900, ASRText: text,
			RepairedText: text, ZhStatus: model.ZhNone}
	}
	e.insertSentences(l.ID, ss)
	e.setSummary(l.ID, "S")
	if batch > 0 {
		e.setTranslateBatch(l.ID, batch)
	}
	return l
}

func cacheZh(idx int, _ bool) string {
	return fmt.Sprintf("现在我们来看列表中的第 %d 项。", idx)
}

// On DeepSeek: the second batch request is sent only when the first has
// answered, every other one only when the second has answered; every batch
// request carries the same system prompt, byte for byte (the prefix the
// cache can share), and nothing else of the requests is shared.
func TestTranslateBatchDeepSeekTwoStepWarmup(t *testing.T) {
	e, l := cacheLecture(t, 60, 10, true)
	var r spans
	fake := batchScript(cacheZh, func(ctx context.Context, n int, c llmCall) (bool, string, error) {
		r.run(n, func() { time.Sleep(20 * time.Millisecond) })
		return false, "", nil
	})
	d := e.deps(fake)
	d.Concurrency = 4
	snap, ok := model.ParseLLMSnapshot(e.job(model.ScopeLecture, l.ID, model.StageTranslate).ConfigSnapshot)
	if !ok || !d.deepSeek(snap.ProviderID) {
		t.Fatalf("the translate provider %q is not taken for DeepSeek", snap.ProviderID)
	}
	res := e.run(e.stage(d, model.StageTranslate), l.ID)
	if res.err != nil {
		t.Fatal(res.err)
	}
	batch, single := splitCalls(fake)
	if len(batch) != 6 || len(single) != 0 {
		t.Fatalf("%d batch, %d single requests", len(batch), len(single))
	}
	for k := 1; k < 6; k++ {
		if r.s[k].start.Before(r.s[0].end) {
			t.Errorf("request %d sent before the first answered", k)
		}
		if k >= 2 && r.s[k].start.Before(r.s[1].end) {
			t.Errorf("request %d sent before the second answered", k)
		}
	}
	if r.peak < 2 {
		t.Errorf("peak %d: the rest did not run in parallel", r.peak)
	}
	ld, err := e.deps(nil).loadLecture(e.ctx, l.ID)
	e.must(err)
	want := ctxbuild.TranslateBatchSystem(ld.snap)
	users := map[string]bool{}
	for k, c := range batch {
		if c.System != want {
			t.Errorf("batch request %d: system prompt differs from TranslateBatchSystem", k)
		}
		users[c.User] = true
	}
	if len(users) != len(batch) {
		t.Errorf("%d distinct user messages for %d batches", len(users), len(batch))
	}
	for i, s := range e.sentences(l.ID) {
		if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != cacheZh(i, true) {
			t.Fatalf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
		}
	}
}

// The one-step warm-up stays where the second step does not apply: batches
// on a provider other than DeepSeek, the single path on DeepSeek (batch 1
// and no batch at all). After the first request, the second and third are
// in flight together.
func TestTranslateWarmupOneStepElsewhere(t *testing.T) {
	for _, c := range []struct {
		name     string
		batch    int
		deepSeek bool
		isBatch  bool
	}{
		{"batch 10, not DeepSeek", 10, false, true},
		{"batch 1, DeepSeek", 1, true, false},
		{"no batch, DeepSeek", 0, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, l := cacheLecture(t, 60, c.batch, c.deepSeek)
			third := make(chan struct{})
			var once sync.Once
			var mu sync.Mutex
			overlap := false
			fake := batchScript(cacheZh, func(ctx context.Context, n int, call llmCall) (bool, string, error) {
				switch n {
				case 1: // waits for the third request, which comes only if both may be in flight
					select {
					case <-third:
						mu.Lock()
						overlap = true
						mu.Unlock()
					case <-time.After(2 * time.Second):
					}
				case 2:
					once.Do(func() { close(third) })
				}
				return false, "", nil
			})
			d := e.deps(fake)
			d.Concurrency = 4
			if res := e.run(e.stage(d, model.StageTranslate), l.ID); res.err != nil {
				t.Fatal(res.err)
			}
			batch, single := splitCalls(fake)
			if c.isBatch && (len(batch) != 6 || len(single) != 0) || !c.isBatch && (len(batch) != 0 || len(single) != 60) {
				t.Fatalf("%d batch, %d single requests", len(batch), len(single))
			}
			mu.Lock()
			defer mu.Unlock()
			if !overlap {
				t.Error("the second request held the third back: a second warm-up step")
			}
		})
	}
}

// What two batch requests of a run share is the system prompt and the
// [BEFORE] label, nothing more, and neither input is a prefix of the other.
// So a cache unit that ends where a batch's input ends (DeepSeek stores one
// there, per its caching guide) cannot be matched completely by another
// batch, whatever order they are sent in; a unit the batches can share must
// end within the system prompt and the label. That is what this test shows.
// Whether DeepSeek stores such a unit after the second warm-up step is for
// a real run to show (warmup.go, translateBatchDeepSeekWarmup).
func TestTranslateBatchSharedPrefixIsTheSystemPrompt(t *testing.T) {
	e, l := cacheLecture(t, 30, 10, false)
	ld, err := e.deps(nil).loadLecture(e.ctx, l.ID)
	e.must(err)
	x := model.SnapshotWindow(e.job(model.ScopeLecture, l.ID, model.StageTranslate).ConfigSnapshot)
	a := ctxbuild.BuildTranslateBatchRequest(ld.snap, 0, 10, x)
	b := ctxbuild.BuildTranslateBatchRequest(ld.snap, 10, 10, x)
	if a.System != b.System || !llmtest.IsBatchSystem(a.System) {
		t.Fatal("batch system prompts differ between batches")
	}
	// The user messages part right after the [BEFORE] label: the shared
	// prefix of two batch requests is the system prompt plus that label.
	common := 0
	for common < len(a.User) && common < len(b.User) && a.User[common] == b.User[common] {
		common++
	}
	if want := len(ctxbuild.LabelBefore) + 1; common != want {
		t.Errorf("user messages share %d bytes, want %d (the [BEFORE] label)", common, want)
	}
	if strings.HasPrefix(b.User, a.User) || strings.HasPrefix(a.User, b.User) {
		t.Error("one batch's input is a prefix of the other's")
	}
}

// The production path (review R20 M1): with Deps.Settings, whether the run
// is on DeepSeek comes from the snapshot provider in the settings (its
// base_url host or its balance API), not from Cfg, whose providers here say
// the opposite. On DeepSeek a batch run warms up in two steps; elsewhere,
// and for batch 1 on DeepSeek, the one-step warm-up stays.
func TestTranslateWarmupSettingsPath(t *testing.T) {
	for _, c := range []struct {
		name     string
		baseURL  string
		balance  string
		batch    int
		deepSeek bool // d.deepSeek of the snapshot provider
		twoStep  bool
		isBatch  bool
	}{
		{"base_url api.deepseek.com, batch 10", "https://api.deepseek.com", model.BalanceNone, 10, true, true, true},
		{"balance API DeepSeek only, batch 10", "https://gateway.invalid/v1", model.BalanceDeepSeek, 10, true, true, true},
		{"not DeepSeek, batch 10", "https://llm.invalid/v1", model.BalanceNone, 10, false, false, true},
		{"base_url api.deepseek.com, batch 1", "https://api.deepseek.com", model.BalanceDeepSeek, 1, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, l := cacheLecture(t, 60, c.batch, !c.deepSeek)
			set, err := settings.Open(e.ctx, e.st, e.cfg)
			e.must(err)
			str := func(s string) *string { return &s }
			p, err := set.CreateProvider(e.ctx, settings.ProviderInput{
				Name: str("Settings provider"), Type: str(model.ProviderOpenAI), BaseURL: str(c.baseURL),
				APIKey: str(llmtest.TestKey), BalanceAPI: str(c.balance),
			})
			e.must(err)
			snap, ok := model.ParseLLMSnapshot(e.job(model.ScopeLecture, l.ID, model.StageTranslate).ConfigSnapshot)
			if !ok {
				t.Fatal("no translate snapshot")
			}
			if cfgSays := e.deps(nil).deepSeek(snap.ProviderID); cfgSays == c.deepSeek {
				t.Fatalf("Cfg's provider %q: deepSeek %v, want the opposite of the settings", snap.ProviderID, cfgSays)
			}
			snap.ProviderID, snap.ProviderName = p.ID, p.Name
			e.setSnapshot(l.ID, model.StageTranslate, model.MarshalSnapshot(snap))

			var (
				r       spans
				third   = make(chan struct{})
				once    sync.Once
				mu      sync.Mutex
				overlap bool
			)
			fake := batchScript(cacheZh, func(ctx context.Context, n int, call llmCall) (bool, string, error) {
				if c.twoStep {
					r.run(n, func() { time.Sleep(20 * time.Millisecond) })
					return false, "", nil
				}
				switch n {
				case 1: // waits for the third request, which comes only if both may be in flight
					select {
					case <-third:
						mu.Lock()
						overlap = true
						mu.Unlock()
					case <-time.After(2 * time.Second):
					}
				case 2:
					once.Do(func() { close(third) })
				}
				return false, "", nil
			})
			d := e.deps(fake)
			d.Settings = set
			d.Concurrency = 4
			if got := d.deepSeek(p.ID); got != c.deepSeek {
				t.Fatalf("settings provider (base_url %s, balance %s): deepSeek %v, want %v", c.baseURL, c.balance, got, c.deepSeek)
			}
			if res := e.run(e.stage(d, model.StageTranslate), l.ID); res.err != nil {
				t.Fatal(res.err)
			}
			batch, single := splitCalls(fake)
			if c.isBatch && (len(batch) != 6 || len(single) != 0) || !c.isBatch && (len(batch) != 0 || len(single) != 60) {
				t.Fatalf("%d batch, %d single requests", len(batch), len(single))
			}
			if c.twoStep {
				for k := 1; k < 6; k++ {
					if r.s[k].start.Before(r.s[0].end) {
						t.Errorf("request %d sent before the first answered", k)
					}
					if k >= 2 && r.s[k].start.Before(r.s[1].end) {
						t.Errorf("request %d sent before the second answered", k)
					}
				}
				if r.peak < 2 {
					t.Errorf("peak %d: the rest did not run in parallel", r.peak)
				}
			} else {
				mu.Lock()
				defer mu.Unlock()
				if !overlap {
					t.Error("the second request held the third back: a second warm-up step")
				}
			}
			for i, s := range e.sentences(l.ID) {
				if s.ZhStatus != model.ZhOK || s.Zh == nil || *s.Zh != cacheZh(i, c.isBatch) {
					t.Fatalf("sentence %d: %s %v", i, s.ZhStatus, s.Zh)
				}
			}
		})
	}
}
