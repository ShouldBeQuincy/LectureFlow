// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"lectureflow/internal/ctxbuild"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// The repair warm-up on Anthropic (v3.2.x r2 N46): the other chunks follow
// when chunk 0's streamed answer begins (llm.WithFirstEventHook).

// The warm-up values: repair elsewhere is unchanged (2 s after sending, at
// most 15 s); on Anthropic the cap is the same 15 s.
func TestRepairWarmupValues(t *testing.T) {
	if repairWarmup != (warmup{afterSend: 2 * time.Second, max: 15 * time.Second}) {
		t.Errorf("repairWarmup %+v", repairWarmup)
	}
	if repairAnthropicWarmup != (warmup{afterFirstEvent: true, max: 15 * time.Second}) {
		t.Errorf("repairAnthropicWarmup %+v", repairAnthropicWarmup)
	}
}

// Which repair runs wait for the first event: Anthropic-type providers
// other than DeepSeek's Anthropic-compatible endpoint (by base_url or by
// balance API, as deepSeek decides), with a model they stream. Models they
// do not stream, OpenAI-compatible providers, DeepSeek and config.toml
// providers keep repairWarmup.
func TestRepairWarmupFor(t *testing.T) {
	e := newEnv(t)
	set, err := settings.Open(e.ctx, e.st, e.cfg)
	e.must(err)
	str := func(s string) *string { return &s }
	d := e.deps(nil)
	if got := d.repairWarmupFor(model.LLMSnapshot{ProviderID: "deepseek", Model: "claude-opus-5-5"}); got != repairWarmup {
		t.Errorf("config.toml provider: %+v", got)
	}
	d.Settings = set
	const streamedModel = "claude-opus-5-5"
	for _, c := range []struct {
		name, typ, baseURL, balance string
		anthropic                   bool
	}{
		{"Claude", model.ProviderAnthropic, "https://api.anthropic.com", model.BalanceNone, true},
		{"Claude gateway", model.ProviderAnthropic, "https://llm.example.com", model.BalanceNone, true},
		{"DeepSeek Anthropic", model.ProviderAnthropic, "https://api.deepseek.com/anthropic", model.BalanceNone, false},
		{"DeepSeek balance", model.ProviderAnthropic, "https://gateway.example.com", model.BalanceDeepSeek, false},
		{"DeepSeek", model.ProviderOpenAI, "https://api.deepseek.com", model.BalanceDeepSeek, false},
		{"OpenAI-compatible", model.ProviderOpenAI, "https://llm.example.com/v1", model.BalanceNone, false},
	} {
		p, err := set.CreateProvider(e.ctx, settings.ProviderInput{Name: str(c.name), Type: str(c.typ),
			BaseURL: str(c.baseURL), APIKey: str(llmtest.TestKey), BalanceAPI: str(c.balance)})
		e.must(err)
		want := repairWarmup
		if c.anthropic {
			want = repairAnthropicWarmup
		}
		if got := d.repairWarmupFor(model.LLMSnapshot{ProviderID: p.ID, Model: streamedModel}); got != want || d.anthropic(p.ID) != c.anthropic {
			t.Errorf("%s: %+v, anthropic %v", c.name, got, d.anthropic(p.ID))
		}
		// Models an Anthropic provider does not stream keep repairWarmup.
		for _, m := range []string{"claude-haiku-4-5", "claude-sonnet-4-5-20250929", "kimi-k2", "glm-4.6"} {
			if got := d.repairWarmupFor(model.LLMSnapshot{ProviderID: p.ID, Model: m}); got != repairWarmup {
				t.Errorf("%s, model %s: %+v", c.name, m, got)
			}
		}
	}
	if got := d.repairWarmupFor(model.LLMSnapshot{ProviderID: "no-such-provider", Model: streamedModel}); got != repairWarmup {
		t.Errorf("unknown provider: %+v", got)
	}
}

// The repair stage's wiring, on the fake clock of a synctest bubble: a
// lecture of four chunks, four at a time, every answer 5 s after its
// request arrives. On Anthropic with a streamed model the other chunks are
// sent when chunk 0's answer begins (at 5 s: the in-process transport
// delivers the stream whole, so message_start comes with the rest); with a
// model that is not streamed, on DeepSeek's Anthropic-compatible API and on
// an OpenAI-compatible provider, 2 s after chunk 0 was sent, as before.
func TestRepairStageWarmup(t *testing.T) {
	const answer = 5 * time.Second
	for _, c := range []struct {
		name, typ, balance, model string
		gap                       time.Duration
		stream                    bool // the requests are streamed
	}{
		{"Claude, streamed", model.ProviderAnthropic, model.BalanceNone, "claude-opus-5-5", answer, true},
		{"Claude, not streamed", model.ProviderAnthropic, model.BalanceNone, "claude-haiku-4-5", 2 * time.Second, false},
		{"Anthropic gateway, other model", model.ProviderAnthropic, model.BalanceNone, "kimi-k2", 2 * time.Second, false},
		// streamed, but DeepSeek: the timing of every DeepSeek run stays
		{"DeepSeek, Anthropic API", model.ProviderAnthropic, model.BalanceDeepSeek, "claude-opus-5-5", 2 * time.Second, true},
		{"OpenAI-compatible", model.ProviderOpenAI, model.BalanceNone, "gpt-test", 2 * time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := llmtest.New(t) // outside the bubble (runTranslatePerf)
			var (
				mu       sync.Mutex
				arrivals []time.Duration
			)
			synctest.Test(t, func(t *testing.T) {
				e := newPerfEnv(t, fake)
				begin := time.Now()
				fake.SetDelay(answer)
				fake.Handle(llmtest.RoleRepair, func(_, _ string) string {
					mu.Lock()
					arrivals = append(arrivals, time.Since(begin)-answer) // the handler runs after the delay
					mu.Unlock()
					return `{"patches": []}`
				})
				l := synthLecture(e, 4*ctxbuild.RepairChunkSize)
				set, err := settings.Open(e.ctx, e.st, e.cfg)
				e.must(err)
				str := func(s string) *string { return &s }
				p, err := set.CreateProvider(e.ctx, settings.ProviderInput{Name: str("P"), Type: str(c.typ),
					BaseURL: str(fake.URL), APIKey: str(llmtest.TestKey), BalanceAPI: str(c.balance)})
				e.must(err)
				snap := model.LLMSnapshot{ProviderID: p.ID, ProviderName: p.Name, Model: c.model,
					Params: settings.ResolveParams(model.RoleRepair, model.RoleParams{})}
				e.setSnapshot(l.ID, model.StageRepair, model.MarshalSnapshot(snap))
				d := e.deps(llm.NewWithSettings(set, nil, nil, "", &http.Client{Transport: fake.Transport()}))
				d.Settings = set
				d.Concurrency = 4
				if r := e.run(e.stage(d, model.StageRepair), l.ID); r.err != nil {
					t.Fatal(r.err)
				}
			})
			if len(arrivals) != 4 {
				t.Fatalf("%d repair requests, want 4", len(arrivals))
			}
			slices.Sort(arrivals)
			for k, at := range arrivals[1:] {
				if gap := at - arrivals[0]; gap != c.gap {
					t.Errorf("chunk request %d sent %v after chunk 0, want %v", k+1, gap, c.gap)
				}
			}
			reqs := fake.Requests(llmtest.RoleRepair)
			if len(reqs) != 4 || reqs[0].Stream != c.stream {
				t.Errorf("%d requests, streamed %v", len(reqs), len(reqs) > 0 && reqs[0].Stream)
			}
		})
	}
}

// claudeClient is a settings-mode client whose every role streams from the
// fake server on a current Claude model.
func (e *env) claudeClient() *llm.HTTPClient {
	e.t.Helper()
	set, err := settings.Open(e.ctx, e.st, e.cfg)
	e.must(err)
	_, err = e.fake.ConfigureSettings(e.ctx, set, "Claude", model.ProviderAnthropic, "claude-opus-5-5")
	e.must(err)
	return llm.NewWithSettings(set, nil, nil, "", nil)
}

// The others start when item 0's answer begins: not before (its answer is
// delayed), and without waiting for the rest of it (the fake holds item 0's
// stream after message_start until all of them started; if they waited for
// its answer, they would start only after that hold timed out).
func TestParallelWarmAfterFirstEvent(t *testing.T) {
	e := newEnv(t)
	c := e.claudeClient()
	const delay = 60 * time.Millisecond
	e.fake.SetDelay(delay)
	others := make(chan struct{})
	var started atomic.Int32
	var holds atomic.Int32
	e.fake.SetStreamHook(func(role, event string) {
		if event == "message_start" && holds.Add(1) == 1 {
			select {
			case <-others:
			case <-time.After(5 * time.Second):
			}
		}
	})
	var r spans
	err := parallelWarm(context.Background(), 4, 4, repairAnthropicWarmup, func(ctx context.Context, i int) (bool, error) {
		var err error
		r.run(i, func() {
			if i == 0 {
				_, err = c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
				return
			}
			if started.Add(1) == 3 {
				close(others)
			}
		})
		return err != nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.s) != 4 || e.fake.Count(llmtest.RoleSummary) != 1 {
		t.Fatalf("%d items ran, %d requests", len(r.s), e.fake.Count(llmtest.RoleSummary))
	}
	for i := 1; i < 4; i++ {
		if !r.s[i].start.Before(r.s[0].end) {
			t.Errorf("item %d waited for item 0's answer", i)
		}
		if d := r.s[i].start.Sub(r.s[0].start); d < delay-5*time.Millisecond {
			t.Errorf("item %d started %v after item 0, before its answer began (%v)", i, d, delay)
		}
	}
}

// Item 0 fails for good before its answer began (HTTP 401): no other item
// runs.
func TestParallelWarmAfterFirstEventFirstItemFails(t *testing.T) {
	e := newEnv(t)
	c := e.claudeClient()
	e.fake.FailNext(llmtest.RoleSummary, 1, llmtest.FailHTTP401)
	var r spans
	err := parallelWarm(context.Background(), 10, 4, repairAnthropicWarmup, func(ctx context.Context, i int) (bool, error) {
		var err error
		r.run(i, func() {
			if i == 0 {
				_, err = c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
			}
		})
		return err != nil, err
	})
	if err == nil || len(r.s) != 1 || e.fake.Count(llmtest.RoleSummary) != 1 {
		t.Fatalf("err %v, %d items ran, %d requests", err, len(r.s), e.fake.Count(llmtest.RoleSummary))
	}
}

// Cancelled after the warm-up ended on item 0's first event, while item 0's
// stream still runs: the run returns at once and starts nothing more (two
// workers: items 2..5 were waiting for one).
func TestParallelWarmAfterFirstEventCancelled(t *testing.T) {
	e := newEnv(t)
	c := e.claudeClient()
	e.fake.StreamNext(llmtest.RoleSummary, 1, llmtest.StreamHang) // message_start, then nothing
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var r spans
	done := make(chan error, 1)
	go func() {
		done <- parallelWarm(ctx, 6, 2, repairAnthropicWarmup, func(ctx context.Context, i int) (bool, error) {
			var err error
			r.run(i, func() {
				if i == 0 {
					_, err = c.Complete(ctx, model.RoleSummary, ctxbuild.SummarySystemTemplate, "u", false)
					return
				}
				cancel()
				<-ctx.Done()
				err = ctx.Err()
			})
			return err != nil, err
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not return after the cancel")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.s[1]; len(r.s) != 2 || !ok || e.fake.Count(llmtest.RoleSummary) != 1 {
		t.Fatalf("%d items ran, %d requests", len(r.s), e.fake.Count(llmtest.RoleSummary))
	}
}

// Without a first event (item 0 not streamed, failing or retried before
// its answer began) the warm-up ends as it does without the hook, on the
// fake clock of a synctest bubble: when item 0 finishes, or at the cap.
func TestParallelWarmAfterFirstEventWithoutEvent(t *testing.T) {
	w := repairAnthropicWarmup
	// item 0 takes 3 s: the others start when it finished
	synctest.Test(t, func(t *testing.T) {
		var r spans
		if err := parallelWarm(context.Background(), 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(3 * time.Second)
				}
			})
			return false, nil
		}); err != nil || len(r.s) != 6 {
			t.Fatalf("err %v, %d items ran", err, len(r.s))
		}
		for i := 1; i < 6; i++ {
			if !r.s[i].start.Equal(r.s[0].end) {
				t.Errorf("item %d started %v after item 0's end", i, r.s[i].start.Sub(r.s[0].end))
			}
		}
	})
	// item 0 takes longer than the cap: the others start at the cap
	synctest.Test(t, func(t *testing.T) {
		var r spans
		if err := parallelWarm(context.Background(), 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(time.Minute)
				}
			})
			return false, nil
		}); err != nil || len(r.s) != 6 {
			t.Fatalf("err %v, %d items ran", err, len(r.s))
		}
		for i := 1; i < 6; i++ {
			if d := r.s[i].start.Sub(r.s[0].start); d != w.max {
				t.Errorf("item %d started %v after item 0, want the cap %v", i, d, w.max)
			}
		}
	})
	// item 0 fails for good: nothing else runs
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		var r spans
		err := parallelWarm(context.Background(), 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					time.Sleep(5 * time.Second)
				}
			})
			if i == 0 {
				return true, boom
			}
			return false, nil
		})
		if !errors.Is(err, boom) || len(r.s) != 1 {
			t.Fatalf("err %v, %d items ran", err, len(r.s))
		}
	})
	// item 0 fails without stopping the run (an unusable answer): the rest runs
	synctest.Test(t, func(t *testing.T) {
		var r spans
		err := parallelWarm(context.Background(), 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {})
			if i == 0 {
				return false, errors.New("unusable")
			}
			return false, nil
		})
		if err != nil || len(r.s) != 6 {
			t.Fatalf("err %v, %d items ran", err, len(r.s))
		}
	})
	// cancelled while the others wait: the run returns at the cancel, and
	// none of them runs
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		begin := time.Now()
		var r spans
		err := parallelWarm(ctx, 6, 6, w, func(ctx context.Context, i int) (bool, error) {
			r.run(i, func() {
				if i == 0 {
					<-ctx.Done()
				}
			})
			if i == 0 {
				return true, ctx.Err()
			}
			return false, nil
		})
		if !errors.Is(err, context.Canceled) || len(r.s) != 1 || time.Since(begin) != time.Second {
			t.Fatalf("err %v, %d items ran, after %v", err, len(r.s), time.Since(begin))
		}
	})
}
