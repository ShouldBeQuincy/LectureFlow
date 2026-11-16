// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// v3.2.x r5: SEC32b's leftovers. All names here are made up.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// SEC32b: the user name "test" (a remote server's, or this machine's
// account) names no one: replaced in a user's places only, not in the
// slow-request log's ".../{id}/test" route or a "test" key. A user name of
// its own stays replaced everywhere.
func TestRedactGenericUserNameTest(t *testing.T) {
	plan := []remoteRedactEntry{{ID: "zed-box", Hosts: []string{"gpu7.example.test"}, Users: []string{"test", "qelvin"}, label: "server-1"}}
	savedSources, savedUser := redactWordSources, diagUserName
	t.Cleanup(func() { redactWordSources, diagUserName = savedSources, savedUser })
	redactWordSources = []func(context.Context, *Server) []redactWord{
		func(context.Context, *Server) []redactWord { return remoteRedactWords(plan) },
	}
	for _, me := range []string{"test", "Test"} {
		diagUserName = func() string { return me }
		r := (&Server{}).redactor(context.Background())
		r.secrets = nil
		cases := map[string]string{
			// kept: the routes, a key, the word
			`level=WARN msg="slow request" method=POST route="POST /api/providers/{id}/test" path=/api/providers/{id}/test`: `level=WARN msg="slow request" method=POST route="POST /api/providers/{id}/test" path=/api/providers/{id}/test`,
			`{"test": true, "name": "Test run"}`: `{"test": true, "name": "Test run"}`,
			// replaced: the user's places, the other user everywhere
			`ssh test@gpu7.example.test`:                      `ssh <user>@<host>`,
			`C:\Users\test\AppData and /home/test/x`:          `C:\Users\<user>\AppData and /home/<user>/x`,
			`detail="/scratch/test/lf: denied" qelvin ran it`: `detail="/scratch/<user>/lf: denied" <user> ran it`,
		}
		for in, want := range cases {
			if got := string(r.apply([]byte(in))); got != want {
				t.Errorf("user %q: redact(%q)\n got %q\nwant %q", me, in, got, want)
			}
		}
	}
	// this machine's user name of its own: everywhere, as before
	diagUserName = func() string { return "zorvexa" }
	r := (&Server{}).redactor(context.Background())
	if got := string(r.apply([]byte(`Zorvexa ran it`))); got != `<user> ran it` {
		t.Errorf("own user name: %q", got)
	}
}

// SEC35 L-5: this machine's user name names no one only when it is in a
// small table (test, admin, administrator, user, guest, root). The remote
// servers' table (opc, lab, git, student, distributions: an image's or a
// lab's account on a server) stays as it was, but on a desktop such a name
// may be the user's own choice: replaced everywhere there.
func TestLocalGenericUsersSmallerThanRemote(t *testing.T) {
	savedSources, savedUser := redactWordSources, diagUserName
	t.Cleanup(func() { redactWordSources, diagUserName = savedSources, savedUser })
	redactWordSources = nil
	for name, generic := range map[string]bool{
		"test": true, "Admin": true, "administrator": true, "user": true, "guest": true, "root": true,
		"opc": false, "lab": false, "git": false, "student": false, "fedora": false, "ubuntu": false,
	} {
		diagUserName = func() string { return name }
		r := (&Server{}).redactor(context.Background())
		in := "run " + name + " now"
		want := in
		if !generic {
			want = "run <user> now"
		}
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("this machine's user %q: redact(%q) = %q, want %q", name, in, got, want)
		}
		if !genericRemoteUsers[strings.ToLower(name)] {
			t.Errorf("remote user %q: no longer generic", name)
		}
	}
}

// SEC32 信息-5: a 500's log line names the route and a lecture's or
// course's id, never the request's path (an export's file name is the
// lecture's title).
func TestFailLogsRouteNotPath(t *testing.T) {
	logs := captureLogs(t)
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/lectures/{id}/export/{file}", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, errors.New("disk on fire"))
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/lectures/L11/export/Quantum%20Qelt%20Secrets.srt", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	out := logs.String()
	for _, want := range []string{`route="GET /api/lectures/{id}/export/{file}"`, `path=/api/lectures/L11/export/{file}`, "disk on fire"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Qelt") {
		t.Errorf("the log names the file:\n%s", out)
	}
}
