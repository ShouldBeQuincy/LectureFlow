// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Keyboard and screen-reader fixes from the UI sweep (L14, L16). The
// behaviour is checked by testdata/a11y_test.mjs on a DOM stand-in; the
// checks here pin the declarations it relies on.
func TestA11yFocusAndNames(t *testing.T) {
	want := map[string][]string{
		// L14: a control that may be gone after the re-render names where the focus goes
		// (keyboard focus only: a mouse click's focus does not move, R19)
		"dom.js": {"const next = wait ? wait.next : key && a.dataset.fnext && keyboardFocused(a) ? a.dataset.fnext.split(' ').filter(Boolean) : [];", "dataset: focusData(o),",
			"return typeof el.matches !== 'function' || el.matches(':focus-visible');", "let el = same && !same.disabled ? same : null;", "if (c && !c.disabled && shown(c)) el = c;",
			// R21: a control disabled in place while its request runs keeps the focus waiting on the page, not on a fallback
			"if (same && same.disabled) {", "waitingFor = { key, next, dlg: dialogOf(same) };", "if (wait) waitingFor = null;",
			// R22: the dialog exemption only for another dialog than the control's; focus moved ends the wait at once
			"if (!d || d === w.dlg) waitingFor = null;", "if (waitingFor) waiting(e.target);",
			// R29: a pointer press ends the wait; a closed dialog's wait is void; a view that moved the focus keeps it
			"'pointerdown',", "if (w.dlg && !w.dlg.isConnected) {", "if (wait && waitingFor !== wait) return;",
			"if (prev.isConnected && prev.disabled && prev.dataset && prev.dataset.fkey) {", "el.focus(redrawn ? { preventScroll: true } : undefined);",
			// T6 #1: a dialog gives the focus back through the opener of a hidden popover (keyboard focus only)
			"giveBack(prev, kb);", "if (document.activeElement === el || !kb) return;", "opener.focus({ preventScroll: true });",
			// T6 #10: a choice dialog's radio group has a name
			"'aria-labelledby': message ? msgId : null, 'aria-label': message ? null : title",
			// G3: the dialogs remember their route; a busy one is let finish
			"const entry = { hash: location.hash, prev, kb, leave: () => leave() };", "giveBack(first.prev, first.kb);", "if (opts.route !== false) openDialogs.add(entry);", "build(close, busy)", "}, { route });"},
		// R62 N1: the uploads' "which file to keep" (cancel = none, the flow goes on) is not closed by a route change
		"uploads.js": {"cancelText: t('common.upload.same_none'),\n      // R62 N1:", "route: false,\n    });"},
		// G3: every route change closes the dialogs of the page left
		"app.js": {"const left = closeDialogsOnRoute();\n  const route = parseRoute(location.hash);",
			// R62 N3: after the view's update only a focus left on the body moves (dom.js restore)
			"renderNav();\n    // the same view: a focus left on the page's body goes back to the", "left.restore(main);\n    return;"},
		"actions.js": {"h('div', { class: 'dlg-choices', role: 'radiogroup', 'aria-labelledby': `${group}-q` }, radios),",
			// G3: the retry sent from the dialog survives a route change
			"const res = await busy(target ? api.lectureRetryManual("},
		// T6 #5: "加载更多" gone, the focus goes to the sentences just loaded
		// R29: from the keyboard only, and not after a pointer press meanwhile
		"view_transcript.js": {"moreFocused = kb ? 'kb' : 'mouse';", "document.addEventListener('pointerdown', dropMoreFocus, true);", "if (how !== 'kb') return;", "settleMoreFocus(firstNew);"},
		// the dashboard's and the lecture page's inline actions (R19 §5)
		"view_dashboard.js": {"fnext: [failKey(f), ...away],", "fnext: todoNext('drafts')", "fnext: todoNext('stale')", "fnext: ['dash:add'],", "fnext: [...away, 'dash:add'],"},
		// T6 #4; R29: the tasks without a stage card keep the server's text after the translated one
		"view_lecture.js": {"return h('span', { class: 'lec-notice-err', title }, U.excerpt(text, 300));",
			"err ? errorSpan(err, ended && ended.status === 'failed' ? ended.error : '') : null,", "taErr ? errorSpan(taErr, ta.error) : null", "err ? errorSpan(err) : failed ?",
			"fnext: focusNext(lec, 'launch'),", "fnext: focusNext(lec, 'cancel'),", "fnext: focusNext(lec, `retry:${st.stage}`),", "fnext: focusNext(lec, 'retry:title'),", "fnext: focusNext(lec, 'retry:stream'),"},
		"lecture_list.js": {"fkey: `lec:${lec.id}:launch`, fnext: fnext('launch'),", "fkey: `lec:${lec.id}:retry`, fnext: fnext('retry'),",
			"fkey: `lec:${lec.id}:update`, fnext: fnext('update'),", "fkey: `lec:${lec.id}:play`, fnext: fnext('play'),",
			"{ fkey: `${fk}:dl`, fnext: endNext(fk, 'dl'),", "dataset: { fkey: `${fk}:review`, fnext: endNext(fk, 'review').join(' ') } },"},
		"pipeline.js": {"fnext: [`${opts.key}:seg:${st.stage}`, ...(opts.fnext || [])].join(' ') },",
			// L16: the card is controlled by its segment, not a tooltip
			"'aria-controls': cardId,", "role: 'group', 'aria-label': U.stageName(st.stage) }", "const syncExpanded = () => btn.setAttribute('aria-expanded', String(isOpen()));",
			// R19: the segment opens a card Esc closed; it is described by the error and waiting lines
			"btn.addEventListener('click', () => {", "card.describe = () => describeSegment(btn, [errLabel, errLine], waitLine, failed);",
			// R21: the error line is heard with its label ("上次错误", "警告")
			"if (errLabel) errLabel.setAttribute('id', `${cardId}-errk`);",
			// T6 #8: the waiting line is heard by its value, not "等待" twice; T6 #1: the card names its opener
			"if (waitVal) waitVal.setAttribute('id', `${cardId}-wait`);", "card.dataset.fopener = fkey;",
			// D2 (T16): one stage card open at a time
			"if (openWrap && openWrap !== wrap && openWrap.isConnected) openWrap.dismiss();\n    openWrap = wrap;"},
	}
	if src := readWeb(t, "pipeline.js"); strings.Contains(src, "role: 'tooltip'") || strings.Contains(src, "'aria-describedby': cardId") {
		t.Error("pipeline.js: the stage card (with buttons) is still a tooltip or the segment's description")
	}
	// L16: every <progress> the front end draws has a name
	files, err := filepath.Glob("*.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		for i, line := range strings.Split(readWeb(t, f), "\n") {
			if strings.Contains(line, "h('progress'") && !strings.Contains(line, "'aria-label'") {
				t.Errorf("%s:%d: a <progress> without aria-label", f, i+1)
			}
		}
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
		t.Skip("node is not installed: the JavaScript checks of testdata/a11y_test.mjs need it")
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	url := "file:///" + strings.TrimPrefix(filepath.ToSlash(dir), "/") + "/"
	out, err := runNode(t, node, "a11y_test.mjs", url)
	if err != nil || !strings.Contains(string(out), "a11y checks: ok") {
		t.Fatalf("a11y_test.mjs: %v\n%s", err, out)
	}
}
