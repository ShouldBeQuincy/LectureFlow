// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 7 (the user simulations T22–T25): the course page, the transcript
// and the player. The behaviour is checked by testdata/r7_wc_test.mjs on a
// DOM stand-in; the checks here pin what it cannot reach (the course page
// itself is not mounted there).
func TestR7WebCourse(t *testing.T) {
	want := map[string][]string{
		// T22 B1: the course page's ⋯ menu renames the course in its settings
		"view_course.js": {
			"{ id: 'rename', label: t('course.menu.rename'), icon: 'pencil', onSelect: () => openSettings({ focus: 'name' }) },",
			"focus: o.focus,",
			// R74 low 6: the header's term start link opens the settings on it
			"onclick: () => openSettings({ focus: 'term' }) },",
			// T25 X1, T22 G3: the course's batch operations ask first, from
			// the header and from the ⋯ menu (update_confirm.js)
			// (R74 low 7: once done the buttons are gone; the focus goes to the ⋯)
			"fkey: 'course:drafts', fnext: 'course:menu', onclick: () => courseOp('drafts', launchDrafts) })",
			"fkey: 'course:stale', fnext: 'course:menu', onclick: () => courseOp('stale', updateStale) })",
			"it && it.id === 'launch_drafts' ? { ...it, onSelect: () => courseOp('drafts', launchDrafts) }",
			"it && it.id === 'update_stale' ? { ...it, onSelect: () => courseOp('stale', updateStale) }",
			// (round 8 WL-01: after the glossary question, through
			// launchCourseDrafts's ask; the behaviour in r8_lecture_test.mjs)
			"return launchCourseDrafts(course, { ask: () => confirmLaunchDrafts(withAction('launch').filter((l) => l.draft), course.draft_count) });",
			"if (!(await confirmUpdate(withAction('update'), { named: true, count: course.stale_lecture_count }))) return null;",
			// P2 hotspot 3: one lecture changed redraws its row only (lecture_list.js updateRow)
			"if (!inPlace([l])) renderLectures();",
			// R78 low 3: the minute tick works out the rows' times again
			"(renderHead(), list.refreshTimes())",
			"for (const l of updated) upsertLecture(l);\n      if (!inPlace(updated)) renderLectures();",
		},
		// T25 S3: the player tells a retranslation that came back the same
		// (the transcript tab is checked in r7_wc_test.mjs)
		"view_player.js": {
			"const retranslated = retranslateWatch();",
			"for (const s of sentences) retranslated.seen(s);",
			"if (op === 'retranslate' && !wait) retranslated.start(s);\n      else retranslated.drop(s.idx);",
			"retranslated.drop(s.idx); // the English changed",
			// R78b medium 2: the lecture's translation held, the player's texts
			// say the edit waits (the transcript tab is checked in r7_wc_test.mjs)
			"const wait = translationHeld(S.lec);",
			// (round 9 WV-02: with the id the text box's aria-describedby names)
			"h('p', { class: 'dlg-hint', id: 'pl-edit-hint' }, wait ? t('player.edit.hint_held') : t('player.edit.hint')),",
			// round 9 WV-01: the held texts in a toast that stays (heldToast)
			"if (wait) heldToast(t('player.edit.saved_held'));\n          else toast(t('player.edit.saved'), 'success');",
			"const wait = translationHeld(S.lec); // R78b medium 2",
			"if (wait) heldToast(t(op === 'reset' ? 'player.menu.reset_done_held' : 'player.menu.retranslate_held'));\n      else toast(t(op === 'reset' ? 'player.menu.reset_done' : 'player.menu.retranslate_done'), 'success');",
			// T23 serious 3: a copy gives the focus back (view_transcript.js copyKeepingFocus)
			"const ok = await copyKeepingFocus(ref);",
			"const ok = await copyKeepingFocus(text);",
			// T23 general 13: an on-screen "back 5 seconds" (← on the keyboard)
			"const backBtn = ctlBtn('rotate-ccw', t('player.ctl.back5'), '←', () => seekBy(-5000)",
			"playBtn, backBtn, prevBtn, nextBtn, loopBtn, clockEl",
			// T23 general 17: the speed is in the button's name; the speed menu is a radio menu
			"rateBtn.setAttribute('aria-label', t('player.ctl.rate_now', { rate: `${fmtDecimal(r)}×` }));",
			"RATES.map((r) => ({ label: `${fmtDecimal(r)}×`, checked: r === S.rate, onSelect: () => setRate(r) })),",
		},
		"player_transcript.js": {"dataset: { idx: String(i), fkey: `pl:s:${s.idx}` },"},
		// T25 X1: the lecture menu's "update" asks as the row's does
		"lecture_list.js": {"if (up >= 0) items[up] = { ...items[up], onSelect: () => done(updateConfirmed([lec])) };"},
		// T22 B1: the wizard and the settings share the server's limits
		"wizard.js": {"courseNameError as nameError, courseCodeError as codeError"},
	}
	for f, list := range want {
		src := readWeb(t, f)
		for _, w := range list {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q", f, w)
			}
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the JavaScript checks of testdata/r7_wc_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "r7_wc_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "r7 wc checks: ok") {
		t.Fatalf("r7_wc_test.mjs: %v\n%s", err, out)
	}
}

// T23 general 7, T22 S2: "Edit sentence" changed the English sentence, which
// readers of the translation could not tell; the English names say so.
func TestR7EditSaysEnglish(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(readWeb(t, "i18n/en.json")), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"player.menu.edit", "player.edit.title", "transcript.edit.aria"} {
		if s, _ := m[k].(string); !strings.Contains(s, "English") {
			t.Errorf("en %s = %q: it does not say the English sentence is edited", k, s)
		}
	}
	// the player's edit dialog, as the lecture page's inline edit
	// (r7_wc_test.mjs), says how the translation is made (round 9 WV-02:
	// with the id the text box's aria-describedby names)
	if !strings.Contains(readWeb(t, "view_player.js"), "h('p', { class: 'dlg-hint', id: 'pl-edit-zh-note' }, wait ? t('player.edit.zh_note_held') : t('player.edit.zh_note')),") {
		t.Error("view_player.js: the edit dialog lacks player.edit.zh_note")
	}
}

// R78b medium 2: the texts shown while the lecture's translation is held
// (stale, failed, cancelled) say that it runs again first and promise no
// translation now (what the texts for the other case say).
func TestR7HeldTextsPromiseNothing(t *testing.T) {
	keys := []string{"player.edit.zh_note_held", "player.edit.hint_held", "transcript.edit.hint_held", "player.edit.saved_held", "player.menu.retranslate_held", "player.menu.reset_done_held"}
	want := map[string][2][]string{
		"zh-CN": {{"要先重新运行", "“更新”整节"}, {"保存后只重翻", "会自动重翻", "稍后自动更新", "（会重翻这一句"}},
		"en":    {{"must run again first", "“Update” of the whole lecture"}, {"After saving, only", "will be translated again", "updates shortly", "are translated again)"}},
	}
	for loc, w := range want {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			s, _ := m[k].(string)
			for _, x := range w[0] {
				if !strings.Contains(s, x) {
					t.Errorf("%s %s lacks %q: %s", loc, k, x, s)
				}
			}
			for _, x := range w[1] {
				if strings.Contains(s, x) {
					t.Errorf("%s %s promises %q: %s", loc, k, x, s)
				}
			}
		}
	}
}

// T25 G10, T22 G12, T23 suggestion 10: the new-course wizard's last step
// said term extraction runs in the background after the syllabus was
// skipped (and no slides came), and the slides step did not say what
// slides are for. Round 8 (R74 info): slides that failed to upload do not
// count; the behaviour of both steps' texts is checked in
// testdata/r8_settings_test.mjs (WS-07).
func TestR7WizardSaysWhatHappens(t *testing.T) {
	src := readWeb(t, "wizard.js")
	for _, w := range []string{
		"const termsComing = () => !!(W.course && W.course.syllabus) || W.materials.some((x) => !matFailed(x));",
		"const stepDesc = () => t(W.step === 5 && !termsComing() ? 'wizard.done.desc_no_terms' : STEP_TEXT[W.step - 1][1]);",
		"desc.textContent = stepDesc();",
	} {
		if !strings.Contains(src, w) {
			t.Errorf("wizard.js lacks %q", w)
		}
	}
	for loc, word := range map[string]string{"zh-CN": "术语表", "en": "glossary"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		if s, _ := m["wizard.materials.desc_week"].(string); !strings.Contains(s, word) {
			t.Errorf("%s wizard.materials.desc_week = %q: it does not say what the slides are for", loc, s)
		}
		if s, _ := m["wizard.done.desc_no_terms"].(string); s == "" {
			t.Errorf("%s has no wizard.done.desc_no_terms", loc)
		}
	}
}

// R74 medium 1: a term set in the glossary makes the lecture's correction
// and translation out of date, and the incremental translation of one
// sentence only starts while the translation is done
// (state.StartIncrementalTranslate): "set the term, then translate the
// sentence again" leaves it waiting until the whole lecture is updated.
// The texts that name the ways to another translation say so.
func TestR7GlossaryNeedsUpdate(t *testing.T) {
	want := map[string][2][]string{
		"zh-CN": {{"修改原文", "术语表", "“更新”整节讲座", "重新计费"}, {"指定译法后再重翻此句", "加术语后再试"}},
		"en":    {{"edit the English sentence", "glossary", "“Update” of the whole lecture", "billed again"}, {"set the term's translation in the glossary and translate it again", "set the term in the glossary, or translate"}},
	}
	for loc, w := range want {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"transcript.retranslate.same", "player.edit.zh_note"} {
			s, _ := m[k].(string)
			for _, x := range w[0] {
				if !strings.Contains(s, x) {
					t.Errorf("%s %s lacks %q: %s", loc, k, x, s)
				}
			}
			for _, x := range w[1] {
				if strings.Contains(s, x) {
					t.Errorf("%s %s still offers %q", loc, k, x)
				}
			}
		}
	}
}

// AUD5 low 4: the privacy page says everything that is sent. The model
// services also get, besides the sentences, syllabus, slides, glossary and
// titles: the course name and code (the glossary cleanup, the file-name
// rule), the uploaded files' original names (the title task, the file-name
// rule: at most 50, with their upload times and media creation times), the
// term start date and the names of the lecture kinds (internal/ctxbuild
// organizer.go, janitor.go). Colab also gets the transcription prompt, the
// glossary's English terms (stages/asr_colab.go InitialPrompt).
func TestR7PrivacySaysWhatIsSent(t *testing.T) {
	want := map[string]map[string][]string{
		"zh-CN": {
			// R78b medium 3: every slide file's whole text goes (material_terms,
			// stages/material.go); low 6: the kinds' "代码" as on the settings page
			"privacy.sent.2":  {"转写出的句子", "课程大纲与课件的全部文字", "术语表", "课程名称", "课程代码", "原始文件名", "上传时间", "创建时间", "开课日期", "讲座类型的名称与代码", "录音本身不会发送"},
			"privacy.colab.1": {"录音", "转写提示词", "术语"},
		},
		"en": {
			"privacy.sent.2":  {"transcribed sentences", "the full text of the syllabus and the slides", "glossary", "course name", "course code", "original names", "upload times", "creation times", "term start date", "names and codes of the lecture kinds", "recording itself is never sent"},
			"privacy.colab.1": {"Recordings", "transcription prompt", "glossary"},
		},
	}
	// what it no longer says: only "relevant text" of the slides; lecture
	// titles (they go only as the original file names of lectures from
	// before v2, stages/title.go); "代号"
	notWant := map[string][]string{"zh-CN": {"相关文字", "术语表、讲座标题", "代号"}, "en": {"relevant text", "lecture titles"}}
	for loc, keys := range want {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		for k, words := range keys {
			s, _ := m[k].(string)
			for _, w := range words {
				if !strings.Contains(s, w) {
					t.Errorf("%s %s lacks %q: %s", loc, k, w, s)
				}
			}
		}
		s, _ := m["privacy.sent.2"].(string)
		for _, w := range notWant[loc] {
			if strings.Contains(s, w) {
				t.Errorf("%s privacy.sent.2 still says %q", loc, w)
			}
		}
	}
}

// R78b low 7: the count-only update dialog is also shown for one lecture
// ("update this lecture?"): its text speaks of no number.
func TestR7UpdateUnknownAnyCount(t *testing.T) {
	for loc, w := range map[string]string{"zh-CN": "这些讲座", "en": "these lectures"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		if s, _ := m["course.confirm.update_unknown"].(string); s == "" || strings.Contains(strings.ToLower(s), w) {
			t.Errorf("%s course.confirm.update_unknown speaks of %q, also under the one-lecture title: %s", loc, w, s)
		}
	}
}

// R74 low 1: without a course code the citations start with the initials
// of the course name (coursePart, internal/cite), so a new name changes
// them too; the note under the code says so.
func TestR7CodeNoteSaysInitials(t *testing.T) {
	for loc, w := range map[string]string{"zh-CN": "首字母", "en": "initials"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		if s, _ := m["course.settings.code_note"].(string); !strings.Contains(s, w) {
			t.Errorf("%s course.settings.code_note does not say a course without a code is cited by its name's %s: %s", loc, w, s)
		}
	}
}

// T22 S7: a course cannot be deleted (SPEC §8 has DELETE /api/lectures/{id}
// only), so the privacy page does not say what deleting one does. The other
// languages follow with their translators.
func TestR7PrivacyDeletesLecturesOnly(t *testing.T) {
	for loc, course := range map[string]string{"zh-CN": "课程", "en": "course"} {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		s, _ := m["privacy.delete.1"].(string)
		if s == "" || strings.Contains(strings.ToLower(s), course) {
			t.Errorf("%s privacy.delete.1 = %q: it speaks of deleting a course, which the app cannot do", loc, s)
		}
	}
}
