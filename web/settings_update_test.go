// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"lectureflow/internal/selfupdate"
)

// The in-app update in the settings page's App section and on the
// workbench (SPEC-ext 4; web-builder, ext-step10 M5): every status and
// reason of internal/selfupdate has its text, the requests are routes of
// handlers_selfupdate.go, the mirror limit is the updater's, the section is
// mounted, the workbench hint follows the in-app update when it is
// configured, and every text the files use exists in zh-CN, en and context
// — in the catalogs or (until the lead merges them) in the parts
// web/i18n/_parts/ext-m5-web and selfupdate-320 (3.2.0 fixes: no_manifest,
// the note of a failed check, the next automatic check).

var updateFiles = []string{"settings_update.js", "update_hint.js"}

var updateParts = []string{"ext-m5-web", "selfupdate-320"}

// catalogWithUpdate is the catalog of locale with the not yet merged
// update parts laid over it (absent parts: the catalog alone), and the
// parts' own keys.
func catalogWithUpdate(t *testing.T, locale string) (catalog, catalog) {
	t.Helper()
	c := loadCatalog(t, filepath.Join(catalogDir, locale+".json"))
	part := catalog{}
	for _, name := range updateParts {
		partFile := filepath.Join(catalogDir, "_parts", name+"."+locale+".json")
		if _, err := os.Stat(partFile); err == nil {
			for k, v := range loadCatalog(t, partFile) {
				c[k] = v
				part[k] = v
			}
		}
	}
	return c, part
}

func TestSelfUpdateTexts(t *testing.T) {
	zh, zhPart := catalogWithUpdate(t, "zh-CN")
	en, enPart := catalogWithUpdate(t, "en")
	ctx, ctxPart := catalogWithUpdate(t, "context")
	var keys []string
	for _, f := range updateFiles {
		keys = append(keys, literalKeys(t, f)...)
	}
	if len(keys) < 60 {
		t.Fatalf("only %d keys found in %v", len(keys), updateFiles)
	}
	for _, k := range keys {
		zv, zok := zh[k]
		ev, eok := en[k]
		if !zok || !eok {
			t.Errorf("%s: missing text (zh-CN %v, en %v)", k, zok, eok)
			continue
		}
		zp, err1 := paramSets(zv)
		ep, err2 := paramSets(ev)
		if err1 != nil || err2 != nil {
			t.Errorf("%s: %v %v", k, err1, err2)
			continue
		}
		want := strings.Join(zp[""], ",")
		for form, ps := range ep {
			if got := strings.Join(ps, ","); got != want {
				t.Errorf("%s: en %q has {%s}, zh-CN {%s}", k, form, got, want)
			}
		}
		if _, ok := ctx[k]; !ok {
			t.Errorf("%s: no context entry", k)
		}
	}
	// the part's keys: in all three files, each explained to the translators
	if len(zhPart) == 0 && len(ctxPart) == 0 {
		return // merged into the catalogs
	}
	for k := range zhPart {
		if _, ok := enPart[k]; !ok {
			t.Errorf("%s: in the zh-CN part, not in the en part", k)
		}
		c, ok := ctxPart[k].(map[string]any)
		if !ok || c["where"] == nil || c["notes"] == nil {
			t.Errorf("%s: no context entry with where and notes in the part", k)
		}
	}
	for k := range enPart {
		if _, ok := zhPart[k]; !ok {
			t.Errorf("%s: in the en part, not in the zh-CN part", k)
		}
	}
	for k := range ctxPart {
		if _, ok := zhPart[k]; !ok {
			t.Errorf("%s: in the context part, not in the zh-CN part", k)
		}
	}
	// the names the error texts point to (errors.selfupdate.*): the row
	// titles must say the same
	if !strings.Contains(text(t, zh, "errors.selfupdate.unreachable"), "“"+text(t, zh, "settings.update.file_label")+"”") {
		t.Errorf("errors.selfupdate.unreachable does not name the row %q", text(t, zh, "settings.update.file_label"))
	}
	if !strings.Contains(text(t, en, "errors.selfupdate.unreachable"), "“"+text(t, en, "settings.update.file_label")+"”") {
		t.Errorf("en: errors.selfupdate.unreachable does not name the row %q", text(t, en, "settings.update.file_label"))
	}
	if !strings.Contains(text(t, zh, "errors.selfupdate.unreachable"), text(t, zh, "settings.update.sources_label")) {
		t.Errorf("errors.selfupdate.unreachable does not name %q", text(t, zh, "settings.update.sources_label"))
	}
	// the spec's points: same signature for every source, privacy, next start
	for k, want := range map[string]string{
		"settings.update.sources_hint": "所有源都验同一个签名",
		"settings.update.privacy":      "不附带任何用户信息",
		"settings.update.hint":         "下次启动",
		"settings.update.auto_hint":    "每天最多检查一次",
	} {
		if !strings.Contains(text(t, zh, k), want) {
			t.Errorf("%s: %q does not say %q", k, text(t, zh, k), want)
		}
	}
}

func TestSelfUpdateStatesAndReasons(t *testing.T) {
	src := readWeb(t, "settings_update.js")
	for _, s := range []string{selfupdate.StateIdle, selfupdate.StateChecking, selfupdate.StateUpToDate, selfupdate.StateAvailable,
		selfupdate.StateDownloading, selfupdate.StateImporting, selfupdate.StatePending, selfupdate.StateError,
		selfupdate.StateInstallFailed} {
		if !strings.Contains(src, "case '"+s+"':") {
			t.Errorf("settings_update.js shows nothing for the state %q", s)
		}
	}
	if !strings.Contains(src, "st.state !== '"+selfupdate.StateNotConfigured+"'") || !strings.Contains(src, "st.state === '"+selfupdate.StateNotConfigured+"'") {
		t.Errorf("settings_update.js does not handle %q", selfupdate.StateNotConfigured)
	}
	// a release without update.json or signature has its own line, not
	// "检查更新失败"; a failed check is a note under an update that can
	// still be installed (the server keeps the state available / pending);
	// a trusted comment without a version is not "the version  differs"
	for _, want := range []string{
		"err.code === '" + selfupdate.CodeNoManifest + "'",
		"t('settings.update.state.no_manifest')",
		"...(a ? availableLines(a, false) : []), checkNote(),",
		"[...pendingLines(st.pending, true), checkNote()]",
		"info.code === '" + selfupdate.CodeVersionMismatch + "' && !p.signed",
		"t('settings.update.next_check'",
		// a version whose installation failed: its own line, not "available",
		// and "try again" downloads it (after a check when it is not known)
		"return failedLines(st.install_failed, a);",
		"t('settings.update.state.install_failed', { version: f.version })",
		"if (!same(s)) s = await suRequest('POST', '/api/selfupdate/check');",
		"return suRequest('POST', '/api/selfupdate/download');",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("settings_update.js: %q missing", want)
		}
	}
	for _, r := range []string{selfupdate.ReasonNoPublicKey, selfupdate.ReasonDevBuild, selfupdate.ReasonPlatform, selfupdate.ReasonNoSource} {
		if !strings.Contains(src, "case '"+r+"':\n      return t('settings.update.not_configured."+r+"');") {
			t.Errorf("no text for the reason %q", r)
		}
	}
	if !strings.Contains(src, "st.reason === '"+selfupdate.ReasonWebMode+"'") {
		t.Errorf("web mode is not shown as desktop-only")
	}
	// the errors of a download (the others are those of a check)
	for _, c := range []string{selfupdate.CodeDownloadFailed, selfupdate.CodeChecksum, selfupdate.CodeIO} {
		if !regexp.MustCompile(`const DOWNLOAD_ERRORS = new Set\(\[[^\]]*'` + regexp.QuoteMeta(c) + `'`).MatchString(src) {
			t.Errorf("%s is not a download error", c)
		}
	}
	// result.json's outcomes (README "result.json")
	for _, o := range []string{"installed", "rolled_back", "failed"} {
		if !strings.Contains(src, "r.outcome === '"+o+"'") {
			t.Errorf("no text for the outcome %q", o)
		}
	}
	// the offline update takes the files of either platform
	if !strings.Contains(src, "export const IMPORT_ACCEPT = '.json,.minisig,.exe,.zip,.dmg';") {
		t.Error("IMPORT_ACCEPT does not list update.json, the signature and the packages")
	}
}

// Every request of the two files is a route of handlers_selfupdate.go.
func TestSelfUpdateRoutes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "internal", "api", "handlers_selfupdate.go"))
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, m := range regexp.MustCompile(`h\("([A-Z]+ /api/selfupdate[^"]*)"`).FindAllStringSubmatch(string(b), -1) {
		routes[m[1]] = true
	}
	if len(routes) != 8 {
		t.Fatalf("handlers_selfupdate.go: %d routes %v", len(routes), routes)
	}
	used := map[string]bool{}
	for _, f := range updateFiles {
		for _, m := range regexp.MustCompile(`suRequest\('([A-Z]+)', '(/api/selfupdate[^']*)'`).FindAllStringSubmatch(readWeb(t, f), -1) {
			r := m[1] + " " + m[2]
			used[r] = true
			if !routes[r] {
				t.Errorf("%s calls %s, which the server does not have", f, r)
			}
		}
	}
	for r := range routes {
		if !used[r] {
			t.Errorf("the page never calls %s", r)
		}
	}
}

type mapKV map[string]string

func (m mapKV) GetKV(_ context.Context, k string) (string, bool, error) {
	v, ok := m[k]
	return v, ok, nil
}

func (m mapKV) SetKV(_ context.Context, k, v string) error {
	m[k] = v
	return nil
}

// The section's mirror limit is the updater's (8 accepted, 9 refused).
func TestSelfUpdateMirrorLimit(t *testing.T) {
	m := regexp.MustCompile(`export const MAX_SOURCES = (\d+);`).FindStringSubmatch(readWeb(t, "settings_update.js"))
	if m == nil {
		t.Fatal("settings_update.js has no MAX_SOURCES")
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	u := selfupdate.New(selfupdate.Options{Version: "3.1.0", KV: mapKV{}, Desktop: true})
	list := func(k int) *[]string {
		out := make([]string, k)
		for i := range out {
			out[i] = "https://mirror" + string(rune('a'+i)) + ".example.org/"
		}
		return &out
	}
	if _, err := u.UpdateSettings(context.Background(), selfupdate.SettingsPatch{Sources: list(n)}); err != nil {
		t.Errorf("%d mirrors refused: %v", n, err)
	}
	if _, err := u.UpdateSettings(context.Background(), selfupdate.SettingsPatch{Sources: list(n + 1)}); selfupdate.CodeOf(err) != selfupdate.CodeSourcesInvalid {
		t.Errorf("%d mirrors: %v, want %s", n+1, err, selfupdate.CodeSourcesInvalid)
	}
}

func TestSelfUpdateMounted(t *testing.T) {
	app := readWeb(t, "settings_app.js")
	for _, want := range []string{"import { createUpdateSection } from './settings_update.js';", "const updUI = createUpdateSection(ui);", "updUI.load();", "channelRow(),\n        updUI.rows(),", "updUI.onSettingsChanged();", "updUI.onChanged(ev);", "updUI.destroy();"} {
		if !strings.Contains(app, want) {
			t.Errorf("settings_app.js does not mount the update rows: %q missing", want)
		}
	}
	hint := readWeb(t, "update_hint.js")
	for _, want := range []string{
		"ev.kind === 'update' || ev.kind === 'selfupdate'", // both SSE kinds
		"su = configured(s) ? s : null;",                   // the in-app update decides when configured
		"if (su) return desktop ? suContent() : null;",     // ... else the electron-updater relay
		"if (settings) return null;",                       // the settings page shows the section instead
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("update_hint.js: %q missing", want)
		}
	}
}

// The 3.2.0 part comes in all six languages, with the same keys and
// placeholders as zh-CN, and the texts say what the change is about.
func TestSelfUpdate320Part(t *testing.T) {
	part := func(locale string) catalog {
		f := filepath.Join(catalogDir, "_parts", "selfupdate-320."+locale+".json")
		if _, err := os.Stat(f); err != nil {
			return nil
		}
		return loadCatalog(t, f)
	}
	zh := part("zh-CN")
	if zh == nil {
		// merged: the catalogs carry the keys (TestI18nKeyCompleteness)
		return
	}
	ctx := part("context")
	for _, loc := range []string{"zh-TW", "yue-Hant-HK", "en", "ru", "hi"} {
		c := part(loc)
		if c == nil {
			t.Errorf("selfupdate-320.%s.json is missing", loc)
			continue
		}
		for k, zv := range zh {
			v, ok := c[k]
			if !ok {
				t.Errorf("%s: no %s", loc, k)
				continue
			}
			zp, _ := paramSets(zv)
			lp, _ := paramSets(v)
			if strings.Join(zp[""], ",") != strings.Join(lp[""], ",") {
				t.Errorf("%s %s: {%v}, zh-CN {%v}", loc, k, lp[""], zp[""])
			}
		}
		if len(c) != len(zh) {
			t.Errorf("%s: %d keys, zh-CN %d", loc, len(c), len(zh))
		}
	}
	for k := range zh {
		if _, ok := ctx[k]; !ok {
			t.Errorf("no context for %s", k)
		}
	}
	for k, want := range map[string]string{
		"errors.selfupdate.no_manifest": "不是你的网络问题",
		"settings.update.auto_hint":     "检查失败时",
	} {
		if !strings.Contains(text(t, zh, k), want) {
			t.Errorf("%s: %q does not say %q", k, text(t, zh, k), want)
		}
	}
}
