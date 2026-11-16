// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"encoding/json"
	"go/scanner"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// v2 P14.3 (phase-2 lead ruling 6): every HTTP error is {"error": {"code",
// "params", "detail"}} with a code of the catalog.

// errCheckWriter keeps the body of an error response for checkErrorBodies.
type errCheckWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *errCheckWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *errCheckWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && w.body.Len() < 1<<16 {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *errCheckWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// extraErrorKeys: keys an error body of one endpoint may carry besides the
// usual ones — "rollback_reset" only for "Restrict access"
// (data_dir.restrict_failed after the update's rollback state was removed).
var extraErrorKeys = map[string][]string{
	"POST /api/system/data-dir/restrict": {"rollback_reset"},
}

// checkErrorBodies wraps the server of every test env: each 4xx/5xx answer
// under /api/ must be a coded body (checkErrorBody). The only exception is
// http.ServeContent's own plain-text 416 for an unsatisfiable Range of the
// media files. Violations fail the test when it ends.
func checkErrorBodies(t *testing.T, h http.Handler) http.Handler {
	var mu sync.Mutex
	var bad []string
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range bad {
			t.Errorf("error response is not a coded body: %s", b)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &errCheckWriter{ResponseWriter: w}
		h.ServeHTTP(cw, r)
		if cw.status < 400 || !strings.HasPrefix(r.URL.Path, "/api/") || cw.status == http.StatusRequestedRangeNotSatisfiable {
			return
		}
		if msg := checkErrorBody(cw.body.Bytes(), extraErrorKeys[r.Method+" "+r.URL.Path]...); msg != "" {
			mu.Lock()
			bad = append(bad, r.Method+" "+r.URL.Path+" "+strconv.Itoa(cw.status)+": "+msg)
			mu.Unlock()
		}
	})
}

// knownCodes is every code an error body may carry with its param names:
// this package's errorCatalog plus the codes of the other back-end packages
// listed in testdata/api/error_codes_backend.json (codes-p2), when present.
var knownCodes = sync.OnceValue(func() map[string][]string {
	out := make(map[string][]string, len(errorCatalog))
	for code, d := range errorCatalog {
		out[code] = d.params
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "api", "error_codes_backend.json"))
	if err != nil {
		return out
	}
	var more map[string]struct {
		Params []string `json:"params"`
	}
	if json.Unmarshal(b, &more) == nil {
		for code, d := range more {
			out[code] = append(out[code], d.Params...)
		}
	}
	return out
})

// checkErrorBody returns "" for a valid coded error body: an object whose
// "error" is {code (a known code), params (only its names), detail};
// besides it only "diff" (needs_model_choice) and "changes"
// (calendar.renumber); allowed names further keys the caller admits for
// one endpoint (restrictBodyKeys).
func checkErrorBody(b []byte, allowed ...string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return "not a JSON object: " + string(b)
	}
	for k := range top {
		if k != "error" && k != "diff" && k != "changes" && !slices.Contains(allowed, k) {
			return "unexpected key " + k + ": " + string(b)
		}
	}
	var e struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
		Detail string         `json:"detail"`
	}
	dec := json.NewDecoder(bytes.NewReader(top["error"]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil || e.Code == "" {
		return "error is not {code, params, detail}: " + string(b)
	}
	params, ok := knownCodes()[e.Code]
	if !ok {
		return "code not in the catalog: " + e.Code
	}
	for k := range e.Params {
		found := false
		for _, p := range params {
			found = found || p == k
		}
		if !found {
			return "param " + k + " not listed for " + e.Code
		}
	}
	return ""
}

// errorCodesFile renders the catalog as testdata/api/error_codes.json:
// {code: {"zh": ..., "params": [...]}}, sorted by code.
func errorCodesFile() []byte {
	type entry struct {
		Zh     string   `json:"zh"`
		Params []string `json:"params"`
	}
	out := make(map[string]entry, len(errorCatalog))
	for code, d := range errorCatalog {
		ps := append([]string{}, d.params...)
		out[code] = entry{Zh: d.zh, Params: ps}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out) // maps are encoded with sorted keys
	return buf.Bytes()
}

// testdata/api/error_codes.json is the catalog for the front end and i18n
// (t("errors."+code, params)); regenerate it with
// LECTUREFLOW_UPDATE_ERROR_CODES=1 go test ./internal/api -run TestErrorCodesCatalog.
func TestErrorCodesCatalog(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "api", "error_codes.json")
	want := errorCodesFile()
	if os.Getenv("LECTUREFLOW_UPDATE_ERROR_CODES") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatalf("%s is out of date: regenerate it (LECTUREFLOW_UPDATE_ERROR_CODES=1)", path)
	}
	// Every {placeholder} of a text is one of the code's params, and codes
	// are dotted lower-case names (needs_model_choice is the v1.1 name).
	ph := regexp.MustCompile(`\{([a-z_]+)\}`)
	name := regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	for code, d := range errorCatalog {
		if !name.MatchString(code) && code != "needs_model_choice" {
			t.Errorf("code %q is not dotted lower case", code)
		}
		if strings.TrimSpace(d.zh) == "" {
			t.Errorf("%s has no text", code)
		}
		for _, m := range ph.FindAllStringSubmatch(d.zh, -1) {
			found := false
			for _, p := range d.params {
				found = found || p == m[1]
			}
			if !found {
				t.Errorf("%s: placeholder {%s} is not a param", code, m[1])
			}
		}
	}
}

// Every dotted code literal in the package's code (an area that the catalog
// knows, e.g. "lecture.", "media.") is in the catalog: a typo would send a
// code the front end cannot translate.
func TestErrorCodesInSourceAreCataloged(t *testing.T) {
	areas := map[string]bool{}
	for code := range errorCatalog {
		if a, _, ok := strings.Cut(code, "."); ok {
			areas[a] = true
		}
	}
	codeLit := regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var unknown []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		var sc scanner.Scanner
		sc.Init(fset.AddFile(f, -1, len(src)), src, nil, 0)
		for {
			pos, tok, lit := sc.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit)
			if err != nil || !codeLit.MatchString(v) {
				continue
			}
			if a, _, _ := strings.Cut(v, "."); !areas[a] {
				continue
			}
			if _, ok := errorCatalog[v]; !ok {
				unknown = append(unknown, fset.Position(pos).String()+" "+v)
			}
		}
	}
	sort.Strings(unknown)
	for _, u := range unknown {
		t.Errorf("code not in the catalog: %s", u)
	}
}

// The checker itself: what passes and what is reported.
func TestCheckErrorBody(t *testing.T) {
	for _, ok := range []string{
		`{"error":{"code":"lecture.not_found"}}`,
		`{"error":{"code":"lecture.running","params":{"title":"L"},"detail":"x"}}`,
		`{"error":{"code":"needs_model_choice"},"diff":[]}`,
		`{"error":{"code":"calendar.renumber","params":{"count":2}},"changes":[]}`,
	} {
		if msg := checkErrorBody([]byte(ok)); msg != "" {
			t.Errorf("%s: %s", ok, msg)
		}
	}
	for _, bad := range []string{
		`{"error":"找不到这节讲座"}`,
		`{"error":{"code":"lecture.nope"}}`,
		`{"error":{"code":"lecture.running","params":{"name":"L"}}}`,
		`{"error":{"code":"lecture.not_found","message":"x"}}`,
		`{"error":{"code":"lecture.not_found"},"extra":1}`,
		`404 page not found`,
	} {
		if msg := checkErrorBody([]byte(bad)); msg == "" {
			t.Errorf("%s passed", bad)
		}
	}
}

// "rollback_reset" is admitted next to "error" for "Restrict access" only.
func TestErrorBodyRollbackResetOnlyForRestrict(t *testing.T) {
	b := []byte(`{"error":{"code":"data_dir.restrict_failed","params":{"path":"x","error":"y"}},"rollback_reset":true}`)
	if msg := checkErrorBody(b); msg == "" {
		t.Error("rollback_reset admitted for any endpoint")
	}
	if msg := checkErrorBody(b, extraErrorKeys["POST /api/system/data-dir/restrict"]...); msg != "" {
		t.Errorf("restrict: %s", msg)
	}
	if len(extraErrorKeys) != 1 {
		t.Errorf("extra keys for more endpoints than restrict: %v", extraErrorKeys)
	}
}
