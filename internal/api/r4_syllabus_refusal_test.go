// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lectureflow/internal/llm"
	"lectureflow/internal/model"
	"lectureflow/internal/sched"
)

// errLLM is an llm.Client whose every request fails with err.
type errLLM struct{ err error }

func (c errLLM) Complete(context.Context, string, string, string, bool) (llm.Result, error) {
	return llm.Result{}, c.err
}

// serveDirect sends one request to the server's handler without a socket.
func serveDirect(t *testing.T, e *env, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// R25 B-3 (r4 D11): the provider declines to format the syllabus. The
// answer is the client's llm.refusal with the role, provider and model (the
// existing catalog text says asking again will not help), not the generic
// syllabus.format_failed; another LLM error still is format_failed.
func TestFormatSyllabusRefusal(t *testing.T) {
	refusal := sched.Permanent(model.Errorf(llm.CodeRefusal,
		map[string]any{"role": model.RoleSyllabusFormat, "retries": 0, "provider": "Fake", "model": "fake-model", "category": ""},
		fmt.Errorf("declined: %w", llm.ErrRefusal)))
	for _, c := range []struct {
		name     string
		err      error
		wantCode string
	}{
		{"refusal", refusal, llm.CodeRefusal},
		{"other", errors.New("HTTP 401: bad key"), "syllabus.format_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(s *Server) { s.llmc = errLLM{c.err} })
			rec := serveDirect(t, e, "POST", "/api/courses", map[string]string{"name": "C"})
			var course Course
			if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &course) != nil {
				t.Fatalf("create course: %d %s", rec.Code, rec.Body)
			}
			if rec := serveDirect(t, e, "POST", "/api/courses/"+course.ID+"/syllabus/raw", SyllabusRawInput{Text: "raw"}); rec.Code != http.StatusOK {
				t.Fatalf("raw syllabus: %d %s", rec.Code, rec.Body)
			}
			rec = serveDirect(t, e, "POST", "/api/courses/"+course.ID+"/syllabus/format", nil)
			var body ErrorBody
			if rec.Code != http.StatusBadGateway || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
				t.Fatalf("format: %d %s", rec.Code, rec.Body)
			}
			info := body.Error
			if info.Code != c.wantCode || info.Detail == "" {
				t.Fatalf("error %+v", info)
			}
			if c.wantCode != llm.CodeRefusal {
				return
			}
			// the parameters the catalog text errors.llm.refusal uses
			if info.Params["role"] != model.RoleSyllabusFormat || info.Params["provider"] != "Fake" || info.Params["model"] != "fake-model" {
				t.Errorf("params %+v", info.Params)
			}
			if !strings.Contains(info.Detail, "declined") {
				t.Errorf("detail %q", info.Detail)
			}
		})
	}
}
