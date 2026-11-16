// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/settings"
)

// SPEC-v3 V11, V7: the settings page's "App" and "About" endpoints.

func stubAppInfo(t *testing.T) *[]string {
	t.Helper()
	opened := []string{}
	oldOpen, oldHW := openFolder, diagHardware
	openFolder = func(dir string) error {
		opened = append(opened, dir)
		return nil
	}
	diagHardware = func(context.Context, string) (any, error) {
		return map[string]any{"hw": map[string]any{"os": "windows", "cpu": "Test CPU"}}, nil
	}
	t.Cleanup(func() { openFolder, diagHardware = oldOpen, oldHW })
	return &opened
}

func TestAppLicenseAndNotices(t *testing.T) {
	e := newEnv(t)
	res, body := e.do("GET", "/api/app/license", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("license: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !bytes.Contains(body, []byte("GNU AFFERO GENERAL PUBLIC LICENSE")) {
		t.Errorf("license text is not the AGPL: %.80s", body)
	}
	res, body = e.do("GET", "/api/app/notices", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("notices: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !bytes.HasPrefix(body, []byte("# Third-Party Notices")) {
		t.Errorf("notices: %.80s", body)
	}
}

func TestAppStorage(t *testing.T) {
	e := newEnv(t)
	storageCache.Lock()
	storageCache.dir = ""
	storageCache.Unlock()
	if err := os.MkdirAll(filepath.Join(e.cfg.DataDir, "lectures", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.cfg.DataDir, "lectures", "x", "a.bin"), make([]byte, 3000), 0o644); err != nil {
		t.Fatal(err)
	}
	var out AppStorage
	e.call("GET", "/api/app/storage", nil, 200, &out)
	abs, _ := filepath.Abs(e.cfg.DataDir)
	if out.DataDir != abs || out.UsedBytes < 3000 || out.FreeBytes != 100<<30 {
		t.Errorf("storage = %+v", out)
	}
}

func TestOpenLogs(t *testing.T) {
	opened := stubAppInfo(t)
	e := newEnv(t)
	var out map[string]any
	e.call("POST", "/api/app/open-logs", nil, 200, &out)
	want, _ := filepath.Abs(filepath.Join(e.cfg.DataDir, "logs"))
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("opened %v, want %s", *opened, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("logs dir not created: %v", err)
	}
	openFolder = func(string) error { return errors.New("no file manager") }
	if info := e.callErrInfo("POST", "/api/app/open-logs", nil, 500); info.Code != "app.open_logs_failed" || info.Params["path"] != want {
		t.Errorf("failure = %+v", info)
	}
	// another site's page cannot trigger it (a form post needs no preflight)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/app/open-logs", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Errorf("cross-site: %d", res.StatusCode)
	}
}

func unzipAll(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(data)
	}
	return out
}

// A data directory with a fake provider key, a fake desktop token, an
// environment key, a Bearer header and model request logs: the zip holds
// none of them, and none of the llm log.
func TestDiagnosticsExcludeSecretsAndLectureContent(t *testing.T) {
	stubAppInfo(t)
	const (
		providerKey = "fake-provider-key-0000AAAA1111"
		envKey      = "fake-env-key-ZZZZ9999yyyy"
		bearer      = "fakebearertoken.abc123"
		skKey       = "sk-fakeKEY0123456789abcdef"
		transcript  = "THIS IS A TRANSCRIPT SENTENCE FROM A LECTURE"
	)
	t.Setenv("LFTEST_FAKE_API_KEY", envKey)
	e := newDesktopEnv(t, &DesktopOptions{Token: testToken})
	ctx := context.Background()
	name, typ, base, key := "Fake", "openai", "http://127.0.0.1:9/v1", providerKey
	if _, err := e.set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key}); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(filepath.Join(logs, "llm"), 0o755); err != nil {
		t.Fatal(err)
	}
	logText := strings.Join([]string{
		`time=2026-10-24T10:00:00Z level=INFO msg="lectureflow started" data_dir=C:\Users\alice\AppData\Roaming\LectureFlow`,
		`time=2026-10-24T10:00:01Z level=INFO msg=request header="Authorization: Bearer ` + bearer + `"`,
		`time=2026-10-24T10:00:02Z level=INFO msg=req X-LF-Token=` + testToken,
		`time=2026-10-24T10:00:03Z level=WARN msg="provider" key=` + providerKey + ` other=` + skKey,
		`time=2026-10-24T10:00:04Z level=INFO msg="env" value=` + envKey + ` home=/Users/bob/Library`,
		`time=2026-10-24T10:00:05Z level=INFO msg="job done" stage=export ms=15`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(logText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "llm", "01REQ.jsonl"), []byte(`{"prompt":"`+transcript+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a database file in the data directory for the row counts
	st, err := db.Open(filepath.Join(e.cfg.DataDir, "lectureflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	res, body := e.doWith("POST", "/api/app/diagnostics", nil, testToken, "")
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content type %q", ct)
	}
	cd := res.Header.Get("Content-Disposition")
	if !regexp.MustCompile(`^attachment; filename="lectureflow-diagnostics-\d{4}-\d{2}-\d{2}\.zip"$`).MatchString(cd) {
		t.Errorf("content disposition %q", cd)
	}
	files := unzipAll(t, body)
	for _, want := range []string{"README.txt", "info.json", "app-settings.json", "checks.json", "hardware.json", "database.json", "logs/lectureflow.log"} {
		if _, ok := files[want]; !ok {
			t.Errorf("zip has no %s (has %v)", want, keys(files))
		}
	}
	for name, content := range files {
		if strings.HasPrefix(name, "logs/llm") {
			t.Errorf("zip contains the model request log %s", name)
		}
		for _, secret := range []string{providerKey, envKey, bearer, skKey, testToken, transcript, "alice", "/Users/bob"} {
			if strings.Contains(content, secret) {
				t.Errorf("%s contains %q", name, secret)
			}
		}
	}
	log := files["logs/lectureflow.log"]
	for _, want := range []string{`msg="job done" stage=export ms=15`, `C:\Users\<user>\AppData`, "Authorization: <redacted>", "/Users/<user>/Library"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lost %q:\n%s", want, log)
		}
	}
	var dbInfo struct {
		SchemaVersion int              `json:"schema_version"`
		Tables        map[string]int64 `json:"tables"`
	}
	if err := json.Unmarshal([]byte(files["database.json"]), &dbInfo); err != nil || dbInfo.SchemaVersion == 0 || len(dbInfo.Tables) == 0 {
		t.Errorf("database.json = %s (%v)", files["database.json"], err)
	}
	if _, ok := dbInfo.Tables["lecture"]; !ok {
		t.Errorf("no lecture count: %v", dbInfo.Tables)
	}
	if !strings.Contains(files["app-settings.json"], `"key_hint": ""`) || !strings.Contains(files["app-settings.json"], `"close_behavior"`) {
		t.Errorf("settings.json: %s", files["app-settings.json"])
	}
	if !strings.Contains(files["info.json"], `"mode": "desktop"`) {
		t.Errorf("info.json: %s", files["info.json"])
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The logs are capped: the newest file first, a long one by its last lines.
func TestDiagnosticsLogBudget(t *testing.T) {
	stubAppInfo(t)
	e := newEnv(t)
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	big := strings.Repeat(line, (diagLogBudget+(1<<20))/len(line)) + "LAST LINE\n"
	if err := os.WriteFile(filepath.Join(logs, "old.log"), []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(logs, "old.log"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	got := files["logs/lectureflow.log"]
	if len(got) > diagLogBudget+64 || !strings.HasSuffix(got, "LAST LINE\n") || !strings.HasPrefix(got, "[earlier lines omitted]\n") {
		t.Errorf("tail: %d bytes, starts %.40q", len(got), got)
	}
	if _, ok := files["logs/old.log"]; ok {
		t.Errorf("the older log should not fit in the budget")
	}
}

func TestRedactPatterns(t *testing.T) {
	r := &redactor{secrets: []string{"exact-secret-value"}}
	cases := map[string]string{
		`Authorization: Bearer abc.def`:               `Authorization: <redacted>`,
		`"authorization":"Bearer xyz"`:                `"authorization":"<redacted>"`,
		`api_key=abcd1234`:                            `api_key=<redacted>`,
		`"apiKey": "k-123456"`:                        `"apiKey": "<redacted>"`,
		`key sk-ant-api03-ABCDEFGH done`:              `key sk-<redacted> done`,
		`id 0123456789abcdef0123456789abcdef end`:     `id <redacted> end`,
		`C:\\Users\\carol\\data`:                      `C:\\Users\\<user>\\data`,
		`D:/Users/dave/x`:                             `D:/Users/<user>/x`,
		`/home/erin/lf`:                               `/home/<user>/lf`,
		`v=exact-secret-value;`:                       `v=<redacted>;`,
		`initial_prompt: 25 terms, 110 tokens`:        `initial_prompt: 25 terms, 110 tokens`,
		`job=01M5P4AQC760BXFEC84XD877QQ stage=export`: `job=01M5P4AQC760BXFEC84XD877QQ stage=export`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// SEC13 §13.4-3, SEC14 N31-I3: the user information of a URL (a proxy
// with an account in a remote server's stderr, a mirror's URL) is
// redacted, up to the last "@" before the host; what only has an "@"
// elsewhere stays readable.
//
// Known residue (SEC32 L-4): a password with an unescaped "/", "?" or "#"
// ("https://u:pa/ss@h.example/") is not a URL's user information to the
// rule at all, so the whole user name and password stay as they are (not
// only a part of them). Not covered either: a percent-encoded URL inside a
// parameter. ("user:pass@host" without a scheme, "\/\/" escaped slashes in
// JSON, "//u:p@h" and "_https://": TestRedactBareUserinfo, v3.2.x r5.)
func TestRedactURLUserinfo(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		// redacted: a user, a user and a password, "@" and ":" in the password
		`fetch https://mirror-user@mirror.example/simple/ failed`:  `fetch https://<redacted>@mirror.example/simple/ failed`,
		`proxy http://bob:hunter2@proxy.example:3128 refused`:      `proxy http://<redacted>@proxy.example:3128 refused`,
		`url=https://a@b:p@ss:w0rd@pkgs.example/x`:                 `url=https://<redacted>@pkgs.example/x`,
		`"index":"HTTPS://Carol:pa:ss@Index.Example/simple?x=1"`:   `"index":"HTTPS://<redacted>@Index.Example/simple?x=1"`,
		`git+ssh://dave:s3cret@git.example/repo.git`:               `git+ssh://<redacted>@git.example/repo.git`,
		`socks5h://erin:token=abc@proxy.example:1080`:              `socks5h://<redacted>@proxy.example:1080`,
		`two: ftp://u1:p1@a.example/ and ftp://u2@b.example/`:      `two: ftp://<redacted>@a.example/ and ftp://<redacted>@b.example/`,
		`stderr: Could not fetch URL https://frank:pw@192.0.2.9/x`: `stderr: Could not fetch URL https://<redacted>@<ip>/x`,
		// kept: no user information
		`https://pkgs.example/simple/`:                              `https://pkgs.example/simple/`,
		`https://forum.example/u/@grace/posts?by=@heidi#@top`:       `https://forum.example/u/@grace/posts?by=@heidi#@top`,
		`https://pkgs.example/x?mail=ivan@mail.example`:             `https://pkgs.example/x?mail=ivan@mail.example`,
		`contact judy@mail.example or mailto:judy@mail.example`:     `contact judy@mail.example or mailto:judy@mail.example`,
		`ssh kim@gpu.example (no scheme) and git@git.example:x.git`: `ssh kim@gpu.example (no scheme) and git@git.example:x.git`,
		`see https://docs.example/a and @mention`:                   `see https://docs.example/a and @mention`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
