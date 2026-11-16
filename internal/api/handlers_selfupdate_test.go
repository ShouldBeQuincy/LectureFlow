// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
	"lectureflow/internal/selfupdate/selfupdatetest"
)

// suRelease is a signed fake release served by a fake GitHub.
type suRelease struct {
	key  *selfupdatetest.Key
	gh   *httptest.Server
	raw  []byte
	sig  []byte
	pkg  []byte
	name string
}

func newSURelease(t *testing.T) *suRelease {
	t.Helper()
	key, err := selfupdatetest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &suRelease{key: key, name: "LectureFlow-Setup-3.2.0-x64.exe", pkg: bytes.Repeat([]byte("LectureFlow update "), 20000)}
	mux := http.NewServeMux()
	r.gh = httptest.NewServer(mux)
	t.Cleanup(r.gh.Close)
	sum := sha256.Sum256(r.pkg)
	m := selfupdate.Manifest{Schema: 1, App: selfupdate.AppName, Version: "3.2.0", Channel: selfupdate.ChannelStable,
		ReleasedAt: "2026-11-02T10:00:00Z", Platforms: map[string][]selfupdate.Asset{
			selfupdate.PlatformWindowsX64: {{Kind: selfupdate.KindInstaller, Name: r.name,
				URL: r.gh.URL + "/o/r/releases/download/v3.2.0/" + r.name, Size: int64(len(r.pkg)), SHA256: hex.EncodeToString(sum[:])}},
		}}
	r.raw, _ = json.MarshalIndent(m, "", "  ")
	r.sig = key.Sign(r.raw, selfupdate.TrustedComment("3.2.0"), true)
	serve := func(b []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			http.ServeContent(w, req, "f", time.Time{}, bytes.NewReader(b))
		}
	}
	mux.Handle("/o/r/releases/latest/download/update.json", serve(r.raw))
	mux.Handle("/o/r/releases/latest/download/update.json.minisig", serve(r.sig))
	mux.Handle("/o/r/releases/download/v3.2.0/"+r.name, serve(r.pkg))
	return r
}

// newSUEnv is an env whose server has an updater for windows-x64 3.1.0
// (desktop: the server runs in desktop mode as well).
func newSUEnv(t *testing.T, r *suRelease, desktop *DesktopOptions) (*env, *selfupdate.Updater) {
	t.Helper()
	var u *selfupdate.Updater
	e := newEnvWith(t, envConfig{server: func(s *Server) {
		// the server's own data directory: private, as the application
		// makes it (an updater refuses one other accounts can change)
		u = selfupdate.New(selfupdate.Options{DataDir: s.dataDir, Version: "3.1.0", KV: s.st, Desktop: true,
			Platform: selfupdate.PlatformWindowsX64, PublicKey: r.key.PublicKeyFile(), Repo: "o/r", GitHubWeb: r.gh.URL,
			Downloader: &selfupdate.HTTPDownloader{Backoff: time.Millisecond}, RequestTimeout: 5 * time.Second})
		s.su = u
		s.desktop = desktop
	}})
	t.Cleanup(u.Close)
	if desktop != nil {
		pu, _ := url.Parse(e.ts.URL)
		desktop.Port, _ = strconv.Atoi(pu.Port())
	}
	return e, u
}

func suStatus(t *testing.T, b []byte) selfupdate.Status {
	t.Helper()
	var st selfupdate.Status
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("status %s: %v", b, err)
	}
	return st
}

// wantSUError checks an error answer: status, code, a valid coded body.
func wantSUError(t *testing.T, res *http.Response, b []byte, status int, code string) map[string]any {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("status %d, want %d: %s", res.StatusCode, status, b)
	}
	if msg := checkErrorBody(b); msg != "" {
		t.Fatalf("error body: %s", msg)
	}
	var eb ErrorBody
	json.Unmarshal(b, &eb)
	if eb.Error.Code != code {
		t.Fatalf("code %s, want %s: %s", eb.Error.Code, code, b)
	}
	return eb.Error.Params
}

// postFiles posts multipart "file" parts.
func postFiles(t *testing.T, e *env, files map[string][]byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("note", "ignored")
	for name, b := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(b)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/selfupdate/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func TestSelfUpdateUnavailable(t *testing.T) {
	e := newEnv(t)
	for _, r := range [][2]string{{"GET", "/api/selfupdate"}, {"POST", "/api/selfupdate/check"},
		{"GET", "/api/selfupdate/settings"}, {"DELETE", "/api/selfupdate/pending"}} {
		res, b := e.do(r[0], r[1], nil)
		if p := wantSUError(t, res, b, 503, "server.unavailable"); p["service"] != "selfupdate" {
			t.Fatalf("%s %s: %s", r[0], r[1], b)
		}
	}
}

func TestSelfUpdateAPI(t *testing.T) {
	r := newSURelease(t)
	e, _ := newSUEnv(t, r, nil)

	res, b := e.do("GET", "/api/selfupdate", nil)
	if st := suStatus(t, b); res.StatusCode != 200 || st.State != selfupdate.StateIdle || st.CurrentVersion != "3.1.0" || !st.AutoCheck {
		t.Fatalf("initial: %d %s", res.StatusCode, b)
	}

	// settings
	res, b = e.do("PATCH", "/api/selfupdate/settings", map[string]any{})
	wantSUError(t, res, b, 400, "request.nothing_to_change")
	res, b = e.do("PATCH", "/api/selfupdate/settings", map[string]any{"sources": []string{"https://ok.invalid/", "ftp://x.invalid/"}})
	if p := wantSUError(t, res, b, 400, selfupdate.CodeSourcesInvalid); p["index"] != float64(1) {
		t.Fatalf("index: %s", b)
	}
	var set selfupdate.Settings
	e.call("PATCH", "/api/selfupdate/settings", map[string]any{"auto_check": false, "sources": []string{"https://m.invalid/{url}"}}, 200, &set)
	if set.AutoCheck || len(set.Sources) != 1 {
		t.Fatalf("settings %+v", set)
	}
	e.call("GET", "/api/selfupdate/settings", nil, 200, &set)
	if set.AutoCheck || set.Sources[0] != "https://m.invalid/{url}" {
		t.Fatalf("settings read back %+v", set)
	}
	e.call("PATCH", "/api/selfupdate/settings", map[string]any{"sources": []string{}}, 200, &set)

	// nothing to download yet
	res, b = e.do("POST", "/api/selfupdate/download", nil)
	wantSUError(t, res, b, 409, selfupdate.CodeNotAvailable)

	// check, download, pending
	res, b = e.do("POST", "/api/selfupdate/check", nil)
	if st := suStatus(t, b); res.StatusCode != 200 || st.State != selfupdate.StateAvailable || st.Available.Version != "3.2.0" {
		t.Fatalf("check: %d %s", res.StatusCode, b)
	}
	res, b = e.do("POST", "/api/selfupdate/download", nil)
	if res.StatusCode != 202 {
		t.Fatalf("download: %d %s", res.StatusCode, b)
	}
	var st selfupdate.Status
	// Download and verification take milliseconds; the margin is for a
	// machine under heavy load (flake-hunt #6).
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, b = e.do("GET", "/api/selfupdate", nil)
		if st = suStatus(t, b); st.State != selfupdate.StateDownloading || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st.State != selfupdate.StatePending || st.Pending.Version != "3.2.0" {
		t.Fatalf("after download: %s", b)
	}
	res, b = e.do("POST", "/api/selfupdate/download", nil)
	wantSUError(t, res, b, 409, selfupdate.CodeNotAvailable)
	e.do("DELETE", "/api/selfupdate/download", nil) // nothing running: fine
	res, b = e.do("DELETE", "/api/selfupdate/pending", nil)
	if st := suStatus(t, b); res.StatusCode != 200 || st.Pending != nil || st.State != selfupdate.StateAvailable {
		t.Fatalf("discard: %d %s", res.StatusCode, b)
	}

	// offline update by upload
	tampered := bytes.Clone(r.pkg)
	tampered[len(tampered)/2] ^= 1
	res, b = postFiles(t, e, map[string][]byte{"update.json": r.raw, "update.json.minisig": r.sig, r.name: tampered}, nil)
	if p := wantSUError(t, res, b, 400, selfupdate.CodeChecksum); p["file"] != r.name {
		t.Fatalf("tampered: %s", b)
	}
	badSig := bytes.Clone(r.raw)
	badSig[3] ^= 1
	res, b = postFiles(t, e, map[string][]byte{"update.json": badSig, "update.json.minisig": r.sig, r.name: r.pkg}, nil)
	wantSUError(t, res, b, 400, selfupdate.CodeSignatureInvalid)
	res, b = postFiles(t, e, map[string][]byte{"update.json": r.raw, r.name: r.pkg}, nil)
	if p := wantSUError(t, res, b, 400, selfupdate.CodeImportIncomplete); p["missing"] != "signature" {
		t.Fatalf("incomplete: %s", b)
	}
	res, b = postFiles(t, e, map[string][]byte{"a": {1}, "b": {2}, "c": {3}, "d": {4}}, nil)
	wantSUError(t, res, b, 400, selfupdate.CodeImportInvalid)
	res, b = postFiles(t, e, map[string][]byte{}, nil)
	wantSUError(t, res, b, 400, "upload.no_file")
	res, b = postFiles(t, e, map[string][]byte{"update.json": r.raw}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	wantSUError(t, res, b, 403, "app.cross_site")
	res, b = postFiles(t, e, map[string][]byte{"update.json": r.raw, "update.json.minisig": r.sig, r.name: r.pkg}, nil)
	if st := suStatus(t, b); res.StatusCode != 200 || st.State != selfupdate.StatePending || st.Pending.Origin != selfupdate.OriginFile {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	// paths are for desktop mode only
	res, b = e.do("POST", "/api/selfupdate/import", map[string]any{"paths": []string{"C:\\x"}})
	wantSUError(t, res, b, 400, "upload.not_multipart")
}

func TestSelfUpdateImportPaths(t *testing.T) {
	r := newSURelease(t)
	e, _ := newSUEnv(t, r, &DesktopOptions{Token: testToken})
	src := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(src, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	post := func(body map[string]any) (*http.Response, []byte) {
		// the paths come from the dialog: the main process granted them (T-SEC-08)
		if p, ok := body["path"].(string); ok {
			e.grant(p)
		}
		if ps, ok := body["paths"].([]string); ok {
			e.grant(ps...)
		}
		b, _ := json.Marshal(body)
		return e.doWith("POST", "/api/selfupdate/import", b, testToken, "")
	}
	res, b := post(map[string]any{"paths": []string{}})
	wantSUError(t, res, b, 400, "request.invalid")
	res, b = post(map[string]any{"path": "relative.json"})
	wantSUError(t, res, b, 400, "upload.path_invalid")
	res, b = post(map[string]any{"paths": []string{filepath.Join(src, "missing.json")}})
	wantSUError(t, res, b, 404, "upload.path_not_found")
	res, b = post(map[string]any{"paths": []string{"a", "b", "c", "d"}})
	wantSUError(t, res, b, 400, selfupdate.CodeImportInvalid)
	paths := []string{write("update.json", r.raw), write("update.json.minisig", r.sig), write(r.name, r.pkg)}
	res, b = post(map[string]any{"paths": paths})
	if st := suStatus(t, b); res.StatusCode != 200 || st.State != selfupdate.StatePending || st.Pending.File != r.name {
		t.Fatalf("import: %d %s", res.StatusCode, b)
	}
	// a single path (a zip bundle) may come as "path"
	res, b = post(map[string]any{"path": write("u.zip", []byte("not a zip"))})
	if p := wantSUError(t, res, b, 400, selfupdate.CodeImportIncomplete); p["missing"] != "manifest" {
		t.Fatalf("lone file: %s", b)
	}
}

// A release whose signature is not uploaded yet: the check answers 200 with
// error selfupdate.no_manifest (not unreachable) and the next automatic
// check; once the signature is there the update is found, and discarding
// the downloaded update removes its files.
func TestSelfUpdateNoManifestAndDiscard(t *testing.T) {
	r := newSURelease(t)
	// the release's own server, with the signature held back until uploaded
	var uploaded atomic.Bool
	release := r.gh.Config.Handler
	r.gh.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/o/r/releases/latest/download/update.json.minisig" && !uploaded.Load() {
			http.NotFound(w, req)
			return
		}
		release.ServeHTTP(w, req)
	})
	e, u := newSUEnv(t, r, nil)
	res, b := e.do("POST", "/api/selfupdate/check", nil)
	st := suStatus(t, b)
	if res.StatusCode != 200 || st.State != selfupdate.StateError || st.Error == nil || st.Error.Code != selfupdate.CodeNoManifest ||
		st.NextCheck == "" || st.Error.Detail == "" {
		t.Fatalf("no signature: %d %s", res.StatusCode, b)
	}
	uploaded.Store(true)
	_, b = e.do("POST", "/api/selfupdate/check", nil)
	if st = suStatus(t, b); st.State != selfupdate.StateAvailable || st.Error != nil {
		t.Fatalf("with the signature: %s", b)
	}
	e.do("POST", "/api/selfupdate/download", nil)
	deadline := time.Now().Add(30 * time.Second) // a margin for a loaded machine, as in TestSelfUpdateAPI
	for st.State != selfupdate.StatePending && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		_, b = e.do("GET", "/api/selfupdate", nil)
		st = suStatus(t, b)
	}
	if _, err := os.Stat(filepath.Join(u.Dir(), "3.2.0", r.name)); err != nil {
		t.Fatalf("downloaded package: %v (%s)", err, b)
	}
	res, b = e.do("DELETE", "/api/selfupdate/pending", nil)
	if st = suStatus(t, b); res.StatusCode != 200 || st.Pending != nil || st.State != selfupdate.StateAvailable {
		t.Fatalf("discard: %d %s", res.StatusCode, b)
	}
	if _, err := os.Stat(filepath.Join(u.Dir(), "3.2.0")); !os.IsNotExist(err) {
		t.Fatalf("the discarded update's files are still there: %v", err)
	}
}
