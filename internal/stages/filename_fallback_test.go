// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"lectureflow/internal/calendar"
	"lectureflow/internal/llm"
	"lectureflow/internal/llm/llmtest"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// weekdayMissedRegex is the kind of answer seen in the v3 acceptance: the
// model ignored the weekday (and put day before month), so the name
// "… Wed Oct 07 09-35-00 BST 2026.mp4" does not match at all.
const weekdayMissedRegex = `CSC1003-(?P<kind>[A-Z]+)\d+-\d+ (?P<day>\d{2}) (?P<month>[A-Za-z]{3}) (?P<year>\d{4}) (?P<hour>\d{2})-(?P<minute>\d{2})-\d{2} [A-Z]+ \d{4}\.mp4`

// TestPanoptoFallbackParse: the built-in pattern on Panopto names and
// close variants; other formats do not match.
func TestPanoptoFallbackParse(t *testing.T) {
	up := lt(2026, 10, 24, 18, 0)
	cases := map[string]string{ // name → recorded_at ("" = no match)
		"CSC1003 Lecture Wed Oct 07 09-35-00 BST 2026.mp4":           "2026-10-07T09:35",
		"CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4":            "2026-10-07T09:35",
		"CSC1003-T01-02 Fri Oct 16 12-35-00 BST 2026.mp4":            "2026-10-16T12:35",
		"MTH2001 Tue Sep 1 9-05-00 GMT 2026.mp4":                     "2026-09-01T09:05",
		"Week 3 Tutorial Thursday October 15 14:00:00 CEST 2026.mkv": "2026-10-15T14:00",
		"Lecture Mon Oct 05 10.00 2026.mp4":                          "2026-10-05T10:00",
		"PHY1010 Wed Oct 07 09-35-00 GMT+1 2026.mp4":                 "2026-10-07T09:35",
		"CS101_2026-10-06_1000.mp4":                                  "",
		"Wednesday notes Oct 07.mp4":                                 "",
	}
	for name, want := range cases {
		p := calendar.Parse(calendar.FilenameRule{Regex: panoptoFallbackRegex}, calendar.Sample{Filename: name, UploadedAt: up})
		got := ""
		if p.RecordedAt != nil {
			got = p.RecordedAt.Format("2006-01-02T15:04")
			if !p.HasTime {
				t.Errorf("%s: no time", name)
			}
		}
		if got != want {
			t.Errorf("%s: %q, want %q (matched %v, %s)", name, got, want, p.Matched, p.Error)
		}
	}
}

// TestFilenameRuleWeekdayFallback: the model's rule misses the weekday
// twice (v3 acceptance item 8) → the built-in Panopto pattern is checked
// and applied (first exception), the lecture gets its recording time.
func TestFilenameRuleWeekdayFallback(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	l := e.upload("CSC1003 Lecture Wed Oct 07 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 5))
	answer := llmtest.FilenameRuleAnswer(weekdayMissedRegex, map[string]string{"L": "lecture"})
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return answer })

	r := e.runFilenameRule(d)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n := e.fake.Count(llmtest.RoleFilenameRule); n != 2 {
		t.Errorf("%d requests, want 2 (the model is still asked first, with its retry)", n)
	}
	if r.job.Status != model.StatusDone {
		t.Errorf("job %s", r.job.Status)
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if rule.Regex != panoptoFallbackRegex || rule.Source != calendar.RuleSourceLLM || rule.SamplesMatched != 1 || rule.SamplesTotal != 1 || rule.KindMap != nil {
		t.Errorf("rule %+v", rule)
	}
	got := e.lec(l.ID)
	if ptrStr(got.RecordedAt) != "2026-10-07T09:35" || got.RecordedAtSource != model.RecordedAtFilename {
		t.Errorf("recorded_at %s (%s)", ptrStr(got.RecordedAt), got.RecordedAtSource)
	}
	if e.kv(model.KVFilenameUndo+e.course.ID) == "" {
		t.Error("no undo record")
	}
	if s := e.samplesUsed(); len(s) != 1 {
		t.Errorf("samples used %v", s)
	}
}

// TestFilenameRuleFallbackProposal: with a name the built-in pattern does
// not match (but at least 60 % that it does), the fallback is only
// proposed, like a model's rule with unmatched names (P13.3.3).
func TestFilenameRuleFallbackProposal(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	e.upload("CSC1003-L01-01 Wed Oct 07 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 0))
	e.upload("CSC1003-L01-02 Wed Oct 14 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 1))
	e.upload("revision session.mp4", lt(2026, 10, 24, 18, 2))
	e.fake.FailNext(llmtest.RoleFilenameRule, 2, llmtest.FailNotJSON)

	r := e.runFilenameRule(d)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if e.courseRow().FilenameRule != nil {
		t.Error("rule applied although a name does not match")
	}
	p := e.proposal()
	if p == nil {
		t.Fatal("no proposal")
	}
	var rule calendar.FilenameRule
	e.must(json.Unmarshal(p.Rule, &rule))
	if rule.Regex != panoptoFallbackRegex || rule.SamplesMatched != 2 || rule.SamplesTotal != 3 || p.Explanation != panoptoFallbackExplanation {
		t.Errorf("proposal %+v / %+v", rule, p)
	}
}

// TestFilenameRuleFallbackNotUsedOnAcceptedRule: an accepted model rule is
// kept as it is, even for Panopto names.
func TestFilenameRuleFallbackNotUsedOnAcceptedRule(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	d := e.deps(nil)
	for i, n := range llmtest.PanoptoNames {
		e.upload(n, lt(2026, 10, 24, 18, i))
	}
	e.fake.Handle(llmtest.RoleFilenameRule, func(string, string) string { return llmtest.PanoptoFilenameRuleAnswer })
	if r := e.runFilenameRule(d); r.err != nil {
		t.Fatal(r.err)
	}
	var rule calendar.FilenameRule
	if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil {
		t.Fatal("rule not applied")
	}
	if rule.Regex != llmtest.PanoptoRegex {
		t.Errorf("regex %s", rule.Regex)
	}
}

// A refused filename-rule request (Claude's stop_reason refusal, an
// OpenAI-compatible content filter) is handled like an unusable answer:
// asked once, the built-in Panopto pattern applied, the names recorded so
// they do not trigger again (review R25 B-1).
func TestFilenameRuleRefusalFallsBack(t *testing.T) {
	for _, name := range []string{"anthropic", "openai"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.organizerRoles()
			var d Deps
			var count func() int
			if name == "anthropic" {
				fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusedErr() }}
				d = e.deps(fake)
				count = func() int { return len(fake.calls) }
			} else {
				d = e.deps(nil)
				e.fake.FailNext(llmtest.RoleFilenameRule, 1, llmtest.FailRefusal)
				count = func() int { return e.fake.Count(llmtest.RoleFilenameRule) }
			}
			l := e.upload("CSC1003 Lecture Wed Oct 07 09-35-00 BST 2026.mp4", lt(2026, 10, 24, 18, 5))
			r := e.runFilenameRule(d)
			if r.err != nil || r.job.Status != model.StatusDone {
				t.Fatalf("err %v job %s", r.err, r.job.Status)
			}
			if n := count(); n != 1 {
				t.Errorf("%d requests, want 1 (a refusal is not asked again)", n)
			}
			var rule calendar.FilenameRule
			if raw := e.courseRow().FilenameRule; raw == nil || json.Unmarshal([]byte(*raw), &rule) != nil || rule.Regex != panoptoFallbackRegex {
				t.Fatalf("fallback rule not applied: %+v", rule)
			}
			if got := e.lec(l.ID); ptrStr(got.RecordedAt) != "2026-10-07T09:35" {
				t.Errorf("recorded_at %s", ptrStr(got.RecordedAt))
			}
			if s := e.samplesUsed(); len(s) != 1 {
				t.Errorf("samples used %v", s)
			}
		})
	}
}

// A refusal on names the built-in pattern does not fit: the names are
// recorded all the same and the job fails with the refusal (llm.refusal,
// permanent), not as an unusable answer.
func TestFilenameRuleRefusalWithoutFallback(t *testing.T) {
	e := newEnv(t)
	e.organizerRoles()
	fake := &scriptLLM{fn: func(context.Context, int, llmCall) (string, error) { return "", refusedErr() }}
	d := e.deps(fake)
	e.upload("CS101_2026-10-06_1000.mp4", lt(2026, 10, 24, 18, 0))
	r := e.runFilenameRule(d)
	if !errors.Is(r.err, llm.ErrRefusal) || !sched.IsPermanent(r.err) || jobErrorCode(r.job) != llm.CodeRefusal {
		t.Fatalf("err %v job %s %v", r.err, r.job.Status, r.job.Error)
	}
	if len(fake.calls) != 1 {
		t.Errorf("%d requests", len(fake.calls))
	}
	if s := e.samplesUsed(); len(s) != 1 {
		t.Errorf("samples used %v", s)
	}
}
