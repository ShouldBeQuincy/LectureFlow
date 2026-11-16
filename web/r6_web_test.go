// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"strings"
	"testing"
)

// R62 N4: a dialog that sends a write passes it through busy (dom.js
// openDialog), so a route change while it runs lets it finish, and its
// error has somewhere to go (N5). testdata/a11y_test.mjs drives the dialogs
// it can open on its DOM stand-in (course settings, recording time, lecture
// kind, the file-name rule); the two places it does not reach are pinned
// here: the tidy report's "undo" (inside the glossary view), and the course
// page handing the settings dialog's busy to its recording times section.
// R68: the error of a dialog's request is not toasted for a network failure
// while the "connection lost" banner shows (testdata/a11y_test.mjs). dom.js
// finds the banner by the id index.html gives it and app.js shows it by;
// the three must name the same element.
func TestBusyErrorMindsTheConnectionBanner(t *testing.T) {
	for f, w := range map[string]string{
		"index.html": `<div id="conn-banner" class="conn-banner" role="status" hidden`,
		"app.js":     "const banner = document.getElementById('conn-banner');",
		"dom.js":     "const b = document.getElementById('conn-banner');\n  return !!b && !b.hidden;",
	} {
		if !strings.Contains(readWeb(t, f), w) {
			t.Errorf("%s lacks %q", f, w)
		}
	}
}

func TestDialogWritesAreBusy(t *testing.T) {
	want := map[string][]string{
		"glossary.js": {
			"t('glossary.report.title'),\n      (close, busy) => {",
			"const r = await busy(api.undoJanitorItem(courseId, report.version, it.term_id));",
		},
		"view_course.js": {
			"recordingSection: (close, busy) => recordingSection(course, { close, busy,",
		},
		"calendar_ui.js": {
			"opts.recordingSection(close, busy)",
		},
	}
	for f, list := range want {
		src := readWeb(t, f)
		for _, w := range list {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q", f, w)
			}
		}
	}
}
