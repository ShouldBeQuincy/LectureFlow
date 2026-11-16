// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/fetch"
)

// Review R10 M1 and T18 N2: a download task whose import refused the
// downloaded file (fetch.ImportRefused: an upload.media_* code, e.g. a
// sign-in page saved as the recording) offers no retry (its retry imports
// the same file: fetch.AllowedActions; the buttons follow allowed_actions),
// and its reason is a fetch's own text (downloads.refused.*: what was
// fetched, what to do — send it again from the extension, which fetches it
// anew), not the upload's text (which says to upload another file) with a
// second, contradicting sentence after it.
func TestDownloadRefusedFile(t *testing.T) {
	codes := []string{"upload.media_empty", "upload.media_html", "upload.media_playlist", "upload.media_format"}
	for _, code := range codes {
		if !fetch.RefusedFile(code) {
			t.Errorf("%s: not a refused file for the back end", code)
		}
		if !slices.Equal(fetch.AllowedActions("failed", code), []string{fetch.ActionRemove}) {
			t.Errorf("%s: the back end still offers %v", code, fetch.AllowedActions("failed", code))
		}
	}
	if fetch.RefusedFile(fetch.CodeImportFailed) {
		t.Error("downloads.import_failed counted as a refused file")
	}
	// no judgement of its own in the front end
	if util := readWeb(t, "util.js"); strings.Contains(util, "downloadRetryFutile") {
		t.Error("util.js still has downloadRetryFutile")
	}
	src := readWeb(t, "downloads.js")
	for _, want := range []string{
		"const reason = reasonText(d.error);",
		"case 'upload.media_empty':\n      return t('downloads.refused.empty');",
		"case 'upload.media_html':\n      return t('downloads.refused.html');",
		"case 'upload.media_playlist':\n      return t('downloads.refused.playlist', { kind: p.kind ?? '?' });",
		"case 'upload.media_format':\n      return t('downloads.refused.format', { format: p.format ?? '?' });",
		"return refusedText(info) || U.errorInfoText(info, '');",
		// R61 F4: a refused retry of a refused file, in the row's words
		"if (alive) toast(refusedText(e) || e.message, 'error');",
		// the back end offers no retry for it (fetch.AllowedActions); the
		// buttons follow allowed_actions alone
		"if (acts.includes('retry')) out.push(btn(t('downloads.action.retry'), { size: 'sm', kind: 'primary',",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("downloads.js lacks %q", want)
		}
	}
	for _, gone := range []string{"downloads.refused_hint", "Futile", "dl-note-hint"} {
		if strings.Contains(src, gone) {
			t.Errorf("downloads.js still uses %s", gone)
		}
	}
	if i := strings.Index(src, "  function buttons(d) {"); i < 0 {
		t.Error("downloads.js has no buttons(d)")
	} else if body := src[i : i+strings.Index(src[i:], "\n  }\n")]; strings.Contains(body, "d.error") || strings.Contains(body, "Futile") || strings.Contains(body, "d.state") {
		t.Errorf("buttons(d) judges the task itself instead of following allowed_actions:\n%s", body)
	}
	if css := readWeb(t, "style.css"); strings.Contains(css, ".dl-note-hint") {
		t.Error("style.css keeps the unused .dl-note-hint")
	}

	load := func(loc string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(readWeb(t, "i18n/"+loc+".json")), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// the upload texts stay for uploads, in every locale
	for _, loc := range []string{"zh-CN", "en", "zh-TW", "yue-Hant-HK", "ru", "hi", "context"} {
		m := load(loc)
		for _, code := range codes {
			if s, ok := m["errors."+code].(string); m["errors."+code] == nil || (ok && strings.TrimSpace(s) == "") {
				t.Errorf("%s: no text for errors.%s", loc, code)
			}
		}
	}
	// the fetch's texts: zh-CN and en (the other languages fall back until
	// their translators add them); no action the row does not offer, no
	// upload, the glossary's word for fetching recordings, and the way out
	zh, en, ctx := load("zh-CN"), load("en"), load("context")
	for _, k := range []string{"downloads.refused.empty", "downloads.refused.html", "downloads.refused.playlist", "downloads.refused.format"} {
		z, _ := zh[k].(string)
		e, _ := en[k].(string)
		if z == "" || e == "" {
			t.Errorf("%s: zh-CN %q en %q", k, z, e)
			continue
		}
		for _, bad := range []string{"重试", "上传", "下载"} {
			if strings.Contains(z, bad) {
				t.Errorf("%s (zh-CN) says %s: %s", k, bad, z)
			}
		}
		for _, bad := range []string{"retry", "Retry", "upload", "Upload", "download", "Download", "—"} {
			if strings.Contains(e, bad) {
				t.Errorf("%s (en) says %s: %s", k, bad, e)
			}
		}
		if !strings.Contains(z, "LectureFlow 扩展") || !strings.Contains(e, "LectureFlow extension") {
			t.Errorf("%s: does not say to send it again from the extension", k)
		}
		// only a web page is likely a sign-in or error page
		if login := strings.Contains(z, "登录页") || strings.Contains(e, "sign-in page"); login != (k == "downloads.refused.html") {
			t.Errorf("%s: sign-in page %v", k, login)
		}
		c, _ := ctx[k].(map[string]any)
		if c == nil || c["notes"] == nil || c["where"] != "error" {
			t.Errorf("%s: no context entry", k)
		}
	}
	// the old hint is kept for the translators' files but marked unused
	if c, _ := ctx["downloads.refused_hint"].(map[string]any); c == nil || !strings.HasPrefix(c["notes"].(string), "已废弃") {
		t.Error("context.json: downloads.refused_hint is not marked deprecated")
	}
}
