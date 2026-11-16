// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Round 9, the source-copy line (r9-copy, TX-01…TX-09): texts of zh-CN.json
// and en.json said again where two texts said the same thing twice, said it
// in two shapes, or promised more than the code does. The other languages
// follow with their translators.

// r9Catalogs are the source catalogs this line writes.
func r9Catalogs(t *testing.T) map[string]catalog {
	t.Helper()
	return map[string]catalog{
		"zh-CN": loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json")),
		"en":    loadCatalog(t, filepath.Join(catalogDir, "en.json")),
	}
}

// TX-01 (T28 suggestion 1): the glossary question of the course page, a
// course card and the selection (actions.js, one course) is the dashboard's
// sentence (several courses) with the one course in the same place.
func TestR9GlossaryQuestionSameShape(t *testing.T) {
	for loc, c := range r9Catalogs(t) {
		course := text(t, c, "common.glossary_check.message_course")
		dash := text(t, c, "dashboard.glossary_pending.message")
		if got := strings.ReplaceAll(course, "{course}", "{courses}"); got != dash {
			t.Errorf("%s common.glossary_check.message_course = %q: not dashboard.glossary_pending.message (%q) with {course} for {courses}", loc, course, dash)
		}
	}
}

// longestCommonRun is the length in runes of the longest stretch of text
// a and b share.
func longestCommonRun(a, b string) int {
	x, y := []rune(a), []rune(b)
	prev := make([]int, len(y)+1)
	best := 0
	for i := 1; i <= len(x); i++ {
		cur := make([]int, len(y)+1)
		for j := 1; j <= len(y); j++ {
			if x[i-1] == y[j-1] {
				cur[j] = prev[j-1] + 1
				best = max(best, cur[j])
			}
		}
		prev = cur
	}
	return best
}

// TX-02 (T28 suggestion 4, the copy half): while the translation is held,
// the edit box shows a hint_held (player dialog, lecture page) and then
// player.edit.zh_note_held (view_player.js:675-677, view_transcript.js:
// 770-773). The note no longer says the hint's sentence again; it keeps
// what is its own: the translation is not edited itself, until the
// translation runs again an edit or "translate again" only marks the
// sentence, and “Update” runs the out-of-date stages again and bills them.
func TestR9HeldEditNoteDoesNotRepeatHint(t *testing.T) {
	said := map[string]string{
		"zh-CN": "这节讲座的翻译要先重新运行（“更新”整节，或重试翻译）",
		"en":    "This lecture's translation must run again first (“Update” of the whole lecture, or a retry of the translation)",
	}
	own := map[string][]string{
		"zh-CN": {"不能直接修改", "重翻此句", "待重翻", "重新计费"},
		"en":    {"cannot be edited", "translating it again", "only marks", "bills them again"},
	}
	// the longest stretch the note may share with a hint: a few words
	// (“要先重新运行”, “Update” of the whole lecture), not a sentence
	limit := map[string]int{"zh-CN": 12, "en": 40}
	for loc, c := range r9Catalogs(t) {
		note := text(t, c, "player.edit.zh_note_held")
		if strings.Contains(note, said[loc]) {
			t.Errorf("%s player.edit.zh_note_held says the hint's sentence again: %s", loc, note)
		}
		for _, k := range []string{"player.edit.hint_held", "transcript.edit.hint_held"} {
			hint := text(t, c, k)
			if !strings.Contains(hint, said[loc]) {
				t.Errorf("%s %s lacks %q (the note relies on it): %s", loc, k, said[loc], hint)
			}
			if n := longestCommonRun(note, hint); n > limit[loc] {
				t.Errorf("%s player.edit.zh_note_held shares %d characters in a row with %s (at most %d)", loc, n, k, limit[loc])
			}
		}
		for _, w := range own[loc] {
			if !strings.Contains(note, w) {
				t.Errorf("%s player.edit.zh_note_held lacks %q: %s", loc, w, note)
			}
		}
	}
}

// TX-03 (needs-human 一·11 a): the privacy texts promise no more than the
// code does.
//   - The setup guide's title no longer says all data stays here (lecture
//     text goes to model services, recordings to Colab or a remote server).
//   - A remote server's leftover files: their time is up about 2 hours after
//     the connection ends (remote.DefaultRetain), but they are deleted only
//     when the server's agent starts again, i.e. at LectureFlow's next
//     connection (internal/agent agent.go reap at every start, jobs.go
//     keepJob / reap): not "kept for at most 2 hours".
//   - Deleting a lecture leaves the excerpts of unusable model answers in
//     logs/llm (rejected-*.jsonl); their files are removed after 14 days,
//     at the first call-log write of a later day (internal/llm calllog.go
//     CallLogKeepDays, pruneLocked): not "at most 14 days" either.
func TestR9PrivacyPromisesNoMore(t *testing.T) {
	// R89: also that the files stay when LectureFlow never connects again
	// (low 4); that continuing the transcription is not tied to the 2 hours
	// (medium 1: a lecture waits about 10 minutes for a lost server,
	// sshremote ReconnectGrace, then goes back to this computer); that an
	// answer of up to 16 KB is kept whole, not as an excerpt (low 1,
	// calllog.go RejectedExcerptBytes); and the debug log of log_level =
	// debug (low 2, debuglog.go: same folder, same pruning).
	type want struct {
		titleNot  string
		noMax     []string
		tied      []string
		remote    []string
		deleteHas []string
		delNot    []string
	}
	ws := map[string]want{
		"zh-CN": {
			titleNot:  "数据留在",
			noMax:     []string{"最多保留", "最多 2 小时"},
			tied:      []string{"在此之前连接恢复"},
			remote:    []string{"约 2 小时", "到期后在 LectureFlow 下次连接这台服务器时删除", "一直不再连接的话会留在服务器上"},
			deleteHas: []string{"14 天", "不随讲座删除", "下次向模型服务发送请求时删除", "较长的回答只留开头和结尾", "log_level 设为 debug"},
			delNot:    []string{"最多保留 14 天", "最多 14 天", "以摘录的形式"},
		},
		"en": {
			titleNot:  "data stays",
			noMax:     []string{"at most 2 hours", "for at most"},
			tied:      []string{"comes back before then"},
			remote:    []string{"about 2 hours", "the next time LectureFlow connects to the server", "if it never connects again, they stay there"},
			deleteHas: []string{"14 days", "not deleted with the lecture", "the next time LectureFlow sends a request to a model service", "only the beginning and end of a long answer", "log_level"},
			delNot:    []string{"at most 14 days", "as excerpts"},
		},
	}
	for loc, c := range r9Catalogs(t) {
		w := ws[loc]
		if s := text(t, c, "onboarding.privacy.title"); strings.Contains(s, w.titleNot) {
			t.Errorf("%s onboarding.privacy.title = %q: says all data stays here", loc, s)
		}
		for _, k := range []string{"privacy.local.2", "settings.remote.privacy"} {
			s := text(t, c, k)
			for _, x := range w.noMax {
				if strings.Contains(s, x) {
					t.Errorf("%s %s says %q: the files stay until the next connection: %s", loc, k, x, s)
				}
			}
			for _, x := range w.tied {
				if strings.Contains(s, x) {
					t.Errorf("%s %s says %q: a lecture waits only about 10 minutes for a lost server: %s", loc, k, x, s)
				}
			}
			for _, x := range w.remote {
				if !strings.Contains(s, x) {
					t.Errorf("%s %s lacks %q: %s", loc, k, x, s)
				}
			}
		}
		s := text(t, c, "privacy.delete.1")
		for _, x := range w.deleteHas {
			if !strings.Contains(s, x) {
				t.Errorf("%s privacy.delete.1 lacks %q: %s", loc, x, s)
			}
		}
		for _, x := range w.delNot {
			if strings.Contains(s, x) {
				t.Errorf("%s privacy.delete.1 says %q: not what the logs keep or how long: %s", loc, x, s)
			}
		}
	}
}

// contextEntry is a key's entry in context.json.
func contextEntry(t *testing.T, key string) map[string]any {
	t.Helper()
	e, ok := loadCatalog(t, filepath.Join(catalogDir, "context.json"))[key].(map[string]any)
	if !ok {
		t.Fatalf("context.json has no entry for %s", key)
	}
	return e
}

// TX-04 (needs-human 一·16 b): the time row of a stage card (pipeline.js
// timeText: duration_ms when ended, common.pipe.elapsed while running)
// counts only the stage's latest run: started_at is set at every start
// (state.TransitionJob) and duration_ms is that run's (sched). The label
// says so, within its max (a narrow card label).
func TestR9StageTimeIsThisRun(t *testing.T) {
	e := contextEntry(t, "common.pipe.k_time")
	limit, _ := e["max"].(float64)
	if limit <= 0 {
		t.Fatalf("common.pipe.k_time has no max: %v", e)
	}
	if notes, _ := e["notes"].(string); !strings.Contains(notes, "latest run") {
		t.Errorf("common.pipe.k_time context does not say it is the latest run: %s", notes)
	}
	for loc, c := range r9Catalogs(t) {
		s := text(t, c, "common.pipe.k_time")
		if w := map[string]string{"zh-CN": "本次", "en": "this run"}[loc]; !strings.Contains(strings.ToLower(s), w) {
			t.Errorf("%s common.pipe.k_time = %q: does not say it is this run", loc, s)
		}
		if n := len([]rune(s)); n > int(limit) {
			t.Errorf("%s common.pipe.k_time = %q: %d characters, max %d", loc, s, n, int(limit))
		}
	}
}

// TX-05 (I74, I75, I76): errors.downloads.no_space_kept says what its
// context said: removing the task frees its space, but the task is gone
// (fetch.Queue.Remove deletes its record and files), so the lecture has to
// be sent again from the browser extension.
func TestR9NoSpaceKeptSaysSendAgain(t *testing.T) {
	for loc, w := range map[string]string{"zh-CN": "要用 LectureFlow 扩展重新发送这节讲座", "en": "sent again with the LectureFlow extension"} {
		if s := text(t, r9Catalogs(t)[loc], "errors.downloads.no_space_kept"); !strings.Contains(s, w) {
			t.Errorf("%s errors.downloads.no_space_kept lacks %q: %s", loc, w, s)
		}
	}
	if notes, _ := contextEntry(t, "errors.downloads.no_space_kept")["notes"].(string); !strings.Contains(notes, "sent again with the LectureFlow extension") {
		t.Errorf("errors.downloads.no_space_kept context does not match its text: %s", notes)
	}
}

// TX-06 (I77): the clauses of dashboard.retried_partial's {reason}
// (view_dashboard.js:853-860, U.clauseJoin) tell their translators where
// they stand: joined by common.clause_sep, the first one right after a
// full stop in en / ru / hi; those with {n} start with the number, and
// "cancelled" may come first or after a separator.
func TestR9RetrySkipContextSaysWhereItStands(t *testing.T) {
	for _, k := range []string{"dashboard.retry_skip.model", "dashboard.retry_skip.futile", "dashboard.retry_skip.no_action", "dashboard.retry_skip.cancelled"} {
		notes, _ := contextEntry(t, k)["notes"].(string)
		want := []string{"dashboard.retried_partial", "common.clause_sep", "full stop"}
		if k == "dashboard.retry_skip.cancelled" {
			want = append(want, "come first", "after the separator")
		} else {
			want = append(want, "Start with {n}")
		}
		for _, w := range want {
			if !strings.Contains(notes, w) {
				t.Errorf("%s context lacks %q: %s", k, w, notes)
			}
		}
	}
}

// TX-07 (R87 section 4): the workbench warning for an earlier Colab
// session left without a usable authorization (view_dashboard.js:218)
// has dashboard.colab.leftover_session_title above
// resources.remote.leftover_session_any or errors.colab.leftover_session;
// the title no longer says the body's first sentence again.
func TestR9LeftoverSessionTitleNotTheBody(t *testing.T) {
	limit, _ := contextEntry(t, "dashboard.colab.leftover_session_title")["max"].(float64)
	for loc, c := range r9Catalogs(t) {
		// R89 low 3: within the heading's max in both languages
		if s := text(t, c, "dashboard.colab.leftover_session_title"); limit <= 0 || len([]rune(s)) > int(limit) {
			t.Errorf("%s dashboard.colab.leftover_session_title = %q: %d characters, max %v", loc, s, len([]rune(s)), limit)
		}
		title := strings.TrimRight(text(t, c, "dashboard.colab.leftover_session_title"), "。.!！ ")
		for _, k := range []string{"resources.remote.leftover_session_any", "errors.colab.leftover_session"} {
			body := text(t, c, k)
			body = strings.ReplaceAll(strings.ReplaceAll(body, "（{accelerator}）", ""), " ({accelerator})", "")
			if title == "" || strings.HasPrefix(strings.ToLower(body), strings.ToLower(title)) {
				t.Errorf("%s dashboard.colab.leftover_session_title %q starts %s: %s", loc, title, k, body)
			}
		}
	}
}

// sizeUnitAfterParam: a {…_mb} or {…_bytes} placeholder followed by a unit.
var sizeUnitAfterParam = regexp.MustCompile(`\{\w+_(?:mb|bytes)\}\s*(?:(?:[KMGT]i?B|B|bytes?|megabytes?|gigabytes?)\b|字节|兆)`)

// TX-08: codedText (util.js paramValue) and the components' codeText
// (components_ui.js) give a {…_mb} / {…_bytes} param of a back-end text
// as a size with its unit (fmtMB / fmtBytes: “512 MB”, “3.4 GB”), so a
// unit written after the placeholder shows twice (“3.4 GB MB”). Every
// errors / notes / warnings / common.retrying text of zh-CN and en.
// (system.devices.vram_queue* keep their “{per_job_mb} MB”: view_system.js
// vramText gives those params as plain numbers.)
func TestR9NoUnitAfterSizeParams(t *testing.T) {
	for _, s := range []string{"需要约 {required_mb} MB，", "剩余 {free_mb} MB", "{free_mb} MB free", "{x_bytes} bytes", "{x_bytes}字节"} {
		if !sizeUnitAfterParam.MatchString(s) {
			t.Fatalf("the check misses %q", s)
		}
	}
	for _, s := range []string{"需要约 {required_mb}，", "{free_mb} free", "{per_job_mb} of memory", "{task_bytes}; removing"} {
		if sizeUnitAfterParam.MatchString(s) {
			t.Fatalf("the check flags %q", s)
		}
	}
	coded := []string{"errors.", "notes.", "warnings.", "common.retrying."}
	for loc, c := range r9Catalogs(t) {
		for k := range c.textKeys() {
			shown := false
			for _, p := range coded {
				shown = shown || strings.HasPrefix(k, p)
			}
			if !shown {
				continue
			}
			if s := text(t, c, k); sizeUnitAfterParam.MatchString(s) {
				t.Errorf("%s %s writes a unit after a size param that already has one: %s", loc, k, s)
			}
		}
	}
}

// TX-09 (R86 I6): launchCostMode (update_confirm.js:166-169) says
// "transcribe_only" when the three lecture roles have no service, even if
// the outline or glossary roles have one; the sentence names those three
// roles as the ones without a service, not the whole app.
func TestR9TranscribeOnlyNamesTheRoles(t *testing.T) {
	want := map[string][2]string{
		"zh-CN": {"摘要、修复与翻译", "还没有配置模型服务"},
		"en":    {"Summary, correction and translation", "no model service set up"},
	}
	for loc, c := range r9Catalogs(t) {
		s := text(t, c, "course.confirm.launch_cost_transcribe_only")
		roles, none := strings.Index(s, want[loc][0]), strings.Index(s, want[loc][1])
		if roles != 0 || none < 0 {
			t.Errorf("%s course.confirm.launch_cost_transcribe_only does not start with the roles that have no service: %s", loc, s)
		}
	}
}
