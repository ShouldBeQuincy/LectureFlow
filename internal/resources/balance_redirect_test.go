// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package resources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/model"
)

// The balance query sends the provider's key (Authorization: Bearer). Its
// client follows a redirect only within the origin, as the LLM clients do
// (AUD1 I-2): net/http's own policy keeps Authorization on a redirect to
// the same host name, http and other ports included.

// authLog records the Authorization header of every request a server got.
type authLog struct {
	mu   sync.Mutex
	seen []string
}

func (a *authLog) add(r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.URL.Path+" "+r.Header.Get("Authorization"))
	a.mu.Unlock()
}

func (a *authLog) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

const redirectBalanceBody = `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.50"}]}`

// elsewhere is a plain http server that answers a balance and records
// what reached it.
func elsewhere(t *testing.T) (*httptest.Server, *authLog) {
	t.Helper()
	got := &authLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.add(r)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(redirectBalanceBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// redirecting answers /user/balance with a 302 to target (a path or a URL)
// and /moved/user/balance with the balance.
func redirecting(target string, got *authLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.add(r)
		if r.URL.Path == "/moved/user/balance" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(redirectBalanceBody))
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
}

func wantRedirectNotFollowed(t *testing.T, res balanceResult, other *authLog) {
	t.Helper()
	if res.errInfo == nil || res.errInfo.Code != CodeBalanceHTTP || res.errInfo.Params["status"] != http.StatusFound {
		t.Errorf("result %+v (%v), want %s with status 302", res.errInfo, res.err, CodeBalanceHTTP)
	}
	if got := other.list(); len(got) != 0 {
		t.Errorf("the redirect was followed: %q", got)
	}
	if res.err != nil && strings.Contains(res.err.Error(), testKey) {
		t.Errorf("the key is in the error: %v", res.err)
	}
}

// The service's own client (Deps.HTTP nil): a redirect to another port of
// the same host name is not followed, so the key stays where it was sent.
func TestBalanceRedirectToAnotherOriginNotFollowed(t *testing.T) {
	other, otherAuth := elsewhere(t)
	firstAuth := &authLog{}
	first := httptest.NewServer(redirecting(other.URL+"/user/balance", firstAuth))
	t.Cleanup(first.Close)
	s := New(Deps{})
	p := model.Provider{ID: "ds", BaseURL: first.URL, APIKey: testKey}
	res := fetchDeepSeekBalance(context.Background(), s.client, p)
	wantRedirectNotFollowed(t, res, otherAuth)
	if got := firstAuth.list(); len(got) != 1 || got[0] != "/user/balance Bearer "+testKey {
		t.Errorf("requests of the provider's server: %q", got)
	}
}

// A client given in Deps without a redirect policy gets the same one: from
// https down to http on the same host name is not followed; within the
// origin a redirect is followed as before.
func TestBalanceGivenClientRedirects(t *testing.T) {
	other, otherAuth := elsewhere(t)
	firstAuth := &authLog{}
	// https://127.0.0.1:port → http://127.0.0.1:other port: the same host
	// name, so net/http would keep Authorization
	first := httptest.NewTLSServer(redirecting(other.URL+"/user/balance", firstAuth))
	t.Cleanup(first.Close)
	s := New(Deps{HTTP: first.Client()})
	if first.Client().CheckRedirect != nil {
		t.Fatal("the test server's client already has a redirect policy")
	}
	p := model.Provider{ID: "ds", BaseURL: first.URL, APIKey: testKey}
	wantRedirectNotFollowed(t, fetchDeepSeekBalance(context.Background(), s.client, p), otherAuth)

	sameAuth := &authLog{}
	same := httptest.NewTLSServer(redirecting("/moved/user/balance", sameAuth))
	t.Cleanup(same.Close)
	s = New(Deps{HTTP: same.Client()})
	res := fetchDeepSeekBalance(context.Background(), s.client, model.Provider{ID: "ds", BaseURL: same.URL, APIKey: testKey})
	if res.err != nil || res.balance != "12.50" || res.currency != "CNY" {
		t.Fatalf("same-origin redirect: %+v %v", res.errInfo, res.err)
	}
	if got := sameAuth.list(); len(got) != 2 || got[1] != "/moved/user/balance Bearer "+testKey {
		t.Errorf("requests %q", got)
	}
}

// A client with a redirect policy of its own keeps it.
func TestBalanceClientPolicyKept(t *testing.T) {
	called := false
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { called = true; return http.ErrUseLastResponse }}
	other, otherAuth := elsewhere(t)
	first := httptest.NewServer(redirecting(other.URL+"/user/balance", &authLog{}))
	t.Cleanup(first.Close)
	s := New(Deps{HTTP: c})
	wantRedirectNotFollowed(t, fetchDeepSeekBalance(context.Background(), s.client,
		model.Provider{ID: "ds", BaseURL: first.URL, APIKey: testKey}), otherAuth)
	if !called || s.client != c {
		t.Errorf("the caller's client was replaced (policy called %v)", called)
	}
}
