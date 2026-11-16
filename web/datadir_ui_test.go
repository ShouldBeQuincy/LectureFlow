// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// datadir-ui (web-builder, 2026-10-27; security review sec-update M2): the
// diagnostics row data_dir offers "收紧权限" exactly when the server's hint
// is check.hint_data_dir_restrict; the request is the real route (the mock
// serves it too); every text exists in zh-CN, en and context.

func TestDataDirRestrictUI(t *testing.T) {
	src := readWeb(t, "system_datadir.js")
	server, err := os.ReadFile(filepath.Join("..", "internal", "api", "system_datadir.go"))
	if err != nil {
		t.Fatal(err)
	}
	mock, err := os.ReadFile(filepath.Join("..", "internal", "api", "mockserve", "datadir.go"))
	if err != nil {
		t.Fatal(err)
	}
	// the hint that brings the button is the one the server sends
	m := regexp.MustCompile(`export const RESTRICT_HINT = '([a-z_.]+)';`).FindStringSubmatch(src)
	if m == nil || !strings.Contains(string(server), `"`+m[1]+`"`) {
		t.Errorf("system_datadir.js's RESTRICT_HINT %v is not a hint of internal/api/system_datadir.go", m)
	}
	for _, want := range []string{
		"c.name === 'data_dir'",
		"c.hint_info.code === RESTRICT_HINT",
		"confirmDialog(",
		"await api.restrictDataDir()",
		"toast(t('system.data_dir_restricted'), 'success')",
		"if (res && res.rollback_reset) toast(t('system.data_dir_rollback_reset'), 'info')",       // the rollback state that went with it
		"if (e.body && e.body.rollback_reset) toast(t('system.data_dir_rollback_reset'), 'info')", // ... also when the request then failed (409)
		"toast(e.message",              // the coded error's text (data_dir.restrict_failed)
		"await opts.refresh()",         // the row is the server's
		"c.detail_info.params.path",    // the confirmation names the folder
		"disabled: busy",               // one request at a time
		"fkey: 'sys:datadir:restrict'", // focus survives the page's renders
	} {
		if !strings.Contains(src, want) {
			t.Errorf("system_datadir.js lacks %q", want)
		}
	}
	// the request: quiet (the module toasts the failure, kept until closed), the real route
	apiJS := readWeb(t, "api.js")
	if !strings.Contains(apiJS, "restrictDataDir: () => post('/api/system/data-dir/restrict', undefined, { quiet: true }),") {
		t.Error("api.js has no quiet restrictDataDir posting /api/system/data-dir/restrict")
	}
	if !strings.Contains(string(server), `h("POST /api/system/data-dir/restrict"`) || !strings.Contains(string(mock), `"POST /api/system/data-dir/restrict"`) {
		t.Error("POST /api/system/data-dir/restrict is not a route of the server and the mock")
	}
	// wired into the page: the button sits in the row's hint
	sys := readWeb(t, "view_system.js")
	for _, want := range []string{
		"import { createDataDirRestrict, restrictable } from './system_datadir.js';",
		"st !== 'pending' && restrictable(c) ? dataDir.button(c) : null,",
		"dataDir.destroy();",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("view_system.js lacks %q", want)
		}
	}
	// the texts (also those picked by a ternary, which the general key check does not see)
	zh := loadCatalog(t, filepath.Join(catalogDir, "zh-CN.json"))
	en := loadCatalog(t, filepath.Join(catalogDir, "en.json"))
	ctx := loadCatalog(t, filepath.Join(catalogDir, "context.json"))
	for _, k := range []string{"system.data_dir_restrict", "system.data_dir_restricting", "system.data_dir_restricted", "system.data_dir_rollback_reset", "system.data_dir_restrict_title", "system.data_dir_restrict_confirm"} {
		if !strings.Contains(src, "'"+k+"'") {
			t.Errorf("system_datadir.js does not use %s", k)
		}
		if text(t, zh, k) == "" || text(t, en, k) == "" || ctx[k] == nil {
			t.Errorf("%s is missing in zh-CN, en or context", k)
		}
	}
	for _, c := range []catalog{zh, en} {
		if !strings.Contains(text(t, c, "system.data_dir_restrict_confirm"), "{path}") {
			t.Errorf("system.data_dir_restrict_confirm does not name the folder: %q", text(t, c, "system.data_dir_restrict_confirm"))
		}
	}
	// the hint tells the user to click the button by its name
	if !strings.Contains(text(t, zh, "errors.check.hint_data_dir_restrict"), "“"+text(t, zh, "system.data_dir_restrict")+"”") {
		t.Error("the zh-CN hint does not name the button")
	}
}
