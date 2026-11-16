// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// SEC44c c-info-1: data_import.network_path {path} echoes a value of the
// imported database. It reaches the page cleaned (relocate.ShownPath): no
// line break, no U+202E or other control character, at most 200
// characters and "…" — the notice can be neither stretched nor disguised.
func TestDataImportNetworkPathIsShownClean(t *testing.T) {
	e := newDDEnv(t)
	src := devDataDir(t)
	long := `\\192.0.2.70\share\a` + "\n" + "b\r\tc\x1b[31m‮exe.⁦mp4" + strings.Repeat("y", 5000)
	addStoredPaths(t, src, []string{long}, "")
	e.pick(src)
	res, b := e.req("POST", "/api/app/import", map[string]string{"path": src})
	if res.StatusCode != http.StatusBadRequest || errCode(t, b) != codeImportNetworkPath {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	p, _ := errParamsOf(t, b)["path"].(string)
	if strings.ContainsAny(p, "\n\r\t\x1b‮⁦") {
		t.Errorf("path %q keeps a control character", p)
	}
	if n := utf8.RuneCountInString(p); n > 201 || !strings.HasSuffix(p, "…") {
		t.Errorf("path of %d characters: %q", n, p)
	}
	if want := `\\192.0.2.70\share\abc[31mexe.mp4`; !strings.HasPrefix(p, want) {
		t.Errorf("path %q, want it to start with %q", p, want)
	}
}
