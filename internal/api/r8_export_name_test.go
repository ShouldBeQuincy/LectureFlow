// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

// AUD1 I-5: Windows drops a file name's trailing dots and spaces, so
// "x.tmp." opened the half-written "x.tmp" the download refuses by name.
// An export file name ending in a dot or a space is refused (400
// export.bad_name) like one ending in .tmp; the export's own names never
// end so (stages.SafeFileName) and download as before.
func TestExportFileNameEndingInDotOrSpace(t *testing.T) {
	e := newEnv(t)
	c := e.course("C")
	l := e.lecture(c.ID, "L")
	exp := filepath.Join(model.LectureDir(e.cfg.DataDir, c.ID, l.ID), model.ExportDirName)
	if err := os.MkdirAll(exp, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"Week 1.bilingual.md": "# Week 1\n", "Week 1.en.txt.tmp": "partial"} {
		if err := os.WriteFile(filepath.Join(exp, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/lectures/" + l.ID + "/export/"
	for _, bad := range []string{"Week%201.en.txt.tmp.", "Week%201.en.txt.tmp%20", "Week%201.en.txt.tmp.%20.", "Week%201.bilingual.md."} {
		if info := e.callErrInfo("GET", base+bad, nil, http.StatusBadRequest); info.Code != "export.bad_name" {
			t.Errorf("%s: %s, want export.bad_name", bad, info.Code)
		}
	}
	res, b := e.do("GET", base+"Week%201.bilingual.md", nil)
	if res.StatusCode != http.StatusOK || string(b) != "# Week 1\n" {
		t.Fatalf("download: %d %q", res.StatusCode, b)
	}
}
