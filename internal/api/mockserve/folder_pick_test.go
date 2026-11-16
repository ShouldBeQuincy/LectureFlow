// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// r6 path-grants: the page gives a folder pick up when the user cancels the
// confirmation (web/api.js forgetFolderPick). The mock answers as the
// desktop server does, 204 with no body for any path, so the pages do not
// meet a 404 in mock mode.
func TestMockFolderPickGivenUp(t *testing.T) {
	u := testServer(t).URL
	for _, sc := range []string{"1", "ob_storage", "empty"} {
		for _, body := range []string{`{"path":"E:\\LectureFlow"}`, `{"path":""}`, `{}`} {
			code, b, _ := mockDo(t, "DELETE", u+"/api/app/folder-pick?mock="+sc, body)
			if code != 204 || len(b) != 0 {
				t.Errorf("?mock=%s DELETE /api/app/folder-pick %s: %d %s", sc, body, code, b)
			}
		}
	}
	if code, b, _ := mockDo(t, "DELETE", u+"/api/app/folder-pick?mock=1", `{"path":`); code != 400 {
		t.Errorf("a body that is not JSON: %d %s", code, b)
	}
	// giving a pick up changes nothing the mock shows
	if code, b, _ := mockDo(t, "GET", u+"/api/app/data-dir?mock=1", ""); code != 200 || !strings.Contains(string(b), `"state":"idle"`) {
		t.Errorf("data-dir after: %d %s", code, b)
	}
	// the route and its answer are the real server's
	src, err := os.ReadFile(filepath.Join("..", "handlers_datadir.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`h("DELETE /api/app/folder-pick", s.deleteFolderPick)`, "w.WriteHeader(http.StatusNoContent)"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("internal/api/handlers_datadir.go has no %s", want)
		}
	}
	// and the page calls this path
	js, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "api.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "request('DELETE', '/api/app/folder-pick', { path }") {
		t.Error("web/api.js does not send DELETE /api/app/folder-pick {path}")
	}
}
