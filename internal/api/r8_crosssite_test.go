// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// AUD1 I-6: the handlers' own crossSite() check (defence in depth behind
// secure's crossSiteFetch) refused only "cross-site"; it now uses the same
// rule: anything but same-origin, none or no Fetch Metadata is refused,
// same-site (another port of localhost) and unknown values too. Called
// directly here, without secure in front.
func TestCrossSiteMatchesSecure(t *testing.T) {
	for site, want := range map[string]bool{
		"": false, "same-origin": false, "none": false,
		"cross-site": true, "same-site": true, "Cross-Site": true, "other": true,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/app/open-logs", nil)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		if got := crossSite(rec, req); got != want || got != crossSiteFetch(req) {
			t.Errorf("Sec-Fetch-Site %q: crossSite %v, want %v (secure: %v)", site, got, want, crossSiteFetch(req))
		}
		if want && rec.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %q: status %d, want 403", site, rec.Code)
		}
	}

	opened := stubAppInfo(t)
	e := newEnv(t)
	for name, h := range map[string]http.HandlerFunc{
		"open-logs":   e.srv.postOpenLogs,
		"diagnostics": e.srv.postDiagnostics,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/app/"+name, nil)
		req.Header.Set("Sec-Fetch-Site", "same-site")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusForbidden || errCode(t, rec.Body.Bytes()) != "app.cross_site" {
			t.Errorf("%s from a same-site page: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("the logs folder was opened for a same-site page: %v", *opened)
	}
	// the app's own page
	req := httptest.NewRequest(http.MethodPost, "/api/app/open-logs", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	e.srv.postOpenLogs(rec, req)
	if rec.Code == http.StatusForbidden || len(*opened) != 1 {
		t.Errorf("open-logs from the app's page: %d, opened %v", rec.Code, *opened)
	}
}
