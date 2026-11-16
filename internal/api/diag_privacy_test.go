// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
)

// v3.2.x r4 (T19 F1, F2): the diagnostics zip holds no piece of a key (not
// even the hint's last characters) and names no account of this machine:
// the machine's name, account names that are not the system's own and the
// machine part of S-1-5-21 SIDs are replaced, in checks.json and in the
// logs by the same rules (the same name, the same placeholder). The
// diagnostics page itself (GET /api/system) still shows the key hint and
// the account names. All names, SIDs and keys here are made up.
func TestDiagnosticsNoKeyHintsOrAccountNames(t *testing.T) {
	stubAppInfo(t)
	const (
		host    = "WS-FICTION42"
		acct    = "zedkow"
		domAcct = "mira.vale"
		domain  = "CORPX"
		sid     = "S-1-5-21-1111111111-2222222222-3333333333-1004"
		// every 3 characters in a row hold a lower-case letter, an upper-case
		// letter and a digit: no ULID, hex run or word of the zip has them
		plainKey = "qV7rM2zX9kP4wB8nH3tJ5"
		skBody   = "mR4xT8pW2kN6vQ3yZ9hL7"
		skKey    = "sk-" + skBody
	)
	t.Setenv("COMPUTERNAME", host)
	t.Setenv("USERDOMAIN", host)
	e := newEnv(t)
	e.srv.diag.waitStartup()
	ctx := context.Background()
	for i, k := range []string{plainKey, skKey} {
		name, typ, base, key := []string{"Alpha", "Beta"}[i], "openai", "http://127.0.0.1:9/v1", k
		if _, err := e.set.CreateProvider(ctx, settings.ProviderInput{Name: &name, Type: &typ, BaseURL: &base, APIKey: &key}); err != nil {
			t.Fatal(err)
		}
	}
	who := []string{host + `\` + acct, domain + `\` + domAcct, sid, "Authenticated Users"}
	exp := dirperm.Exposure{Write: true, Read: true, Who: who, Writers: who}
	var rerr error
	fakeDataDirAccess(t, &exp, &rerr)

	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	logText := strings.Join([]string{
		// another case of the same names: the same placeholders
		`time=2026-11-13T10:00:00Z level=WARN msg="api: other accounts may change entries of the data directory" entry=D:\lf\data who="ws-fiction42\\ZEDKOW, Everyone, S-1-5-32-545, NT AUTHORITY\\SYSTEM, ` + sid + `" entries=1`,
		`time=2026-11-13T10:00:01Z level=WARN msg="restrict" err="other accounts can still reach it (` + domain + `\\` + domAcct + `, BUILTIN\\Users)"`,
		`time=2026-11-13T10:00:02Z level=INFO msg="lectureflow started" host=` + host + ` mode=web`,
		`time=2026-11-13T10:00:03Z level=WARN msg="llm" error="401: the key ` + model.KeyHint(skKey) + ` was rejected"`,
		`time=2026-11-13T10:00:04Z level=INFO msg="job done" stage=export ms=15`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(logText), 0o644); err != nil {
		t.Fatal(err)
	}

	// the diagnostics page: the key hints and the account names as before
	var sys System
	e.call("POST", "/api/system/recheck", nil, http.StatusOK, &sys)
	ui := map[string]SystemCheck{}
	for _, c := range sys.Checks {
		ui[c.Name] = c
	}
	for _, p := range e.set.Providers() {
		c := ui["api_key:"+p.Name]
		if c.DetailInfo == nil || c.DetailInfo.Code != "check.key_set" || c.DetailInfo.Params["key"] != model.KeyHint(p.APIKey) ||
			!strings.Contains(c.Detail, model.KeyHint(p.APIKey)) {
			t.Errorf("page: api_key:%s lost its key hint: %+v %+v", p.Name, c, c.DetailInfo)
		}
	}
	if c := ui[checkDataDir]; c.DetailInfo == nil || c.DetailInfo.Params["who"] != strings.Join(who, ", ") || !strings.Contains(c.Detail, host+`\`+acct) {
		t.Errorf("page: the data directory row lost its account names: %+v %+v", c, c.DetailInfo)
	}

	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	if _, ok := files["logs/lectureflow.log"]; !ok {
		t.Fatalf("zip has no log (has %v)", keys(files))
	}

	// F1: no piece of three characters or more of either key
	for name, content := range files {
		for _, k := range []string{plainKey, skBody} {
			for i := 0; i+3 <= len(k); i++ {
				if strings.Contains(content, k[i:i+3]) {
					t.Errorf("%s contains %q of a key", name, k[i:i+3])
					break
				}
			}
		}
	}
	var checks []SystemCheck
	if err := json.Unmarshal([]byte(files["checks.json"]), &checks); err != nil {
		t.Fatalf("checks.json: %v", err)
	}
	byName := map[string]SystemCheck{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	// (the rows' names "api_key:…" are themselves redacted by the api_key
	// rule; the rows are found by their detail code)
	keyRows := 0
	for _, c := range checks {
		if c.DetailInfo == nil || c.DetailInfo.Code != "check.key_set" {
			continue
		}
		keyRows++
		if c.DetailInfo.Params["key"] != "sk-…" || c.Detail != "已配置（sk-…）" {
			t.Errorf("zip: key row %+v %+v", c, c.DetailInfo)
		}
	}
	if keyRows < 2 { // Alpha, Beta and the test configuration's provider
		t.Errorf("zip: %d key rows, want 2 or more", keyRows)
	}

	// F2: no host name, account name outside the system's, domain or machine SID
	for name, content := range files {
		low := strings.ToLower(content)
		for _, bad := range []string{host, acct, domAcct, domain, "1111111111", "2222222222", "3333333333"} {
			if strings.Contains(low, strings.ToLower(bad)) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	dd := byName[checkDataDir]
	if dd.DetailInfo == nil {
		t.Fatalf("zip: no data_dir row: %+v", dd)
	}
	gotWho, _ := dd.DetailInfo.Params["who"].(string)
	m := regexp.MustCompile(`^<host>\\(<account-\d+>), <domain>\\(<account-\d+>), S-1-5-21-<machine>-1004, Authenticated Users$`).FindStringSubmatch(gotWho)
	if m == nil || m[1] == m[2] {
		t.Fatalf("zip: data_dir who = %q", gotWho)
	}
	if !strings.Contains(dd.Detail, `<host>\`+m[1]+", <domain>\\"+m[2]) {
		t.Errorf("zip: data_dir detail = %q", dd.Detail)
	}
	log := files["logs/lectureflow.log"]
	for _, want := range []string{
		`who="<host>\\` + m[1] + `, Everyone, S-1-5-32-545, NT AUTHORITY\\SYSTEM, S-1-5-21-<machine>-1004" entries=1`,
		`(<domain>\\` + m[2] + `, BUILTIN\\Users)`,
		`host=<host> mode=web`,
		`the key sk-… was rejected`,
		`msg="job done" stage=export ms=15`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// The account rules one by one (redact_account.go).
func TestRedactAccounts(t *testing.T) {
	r := newAccountRedactor([]string{"WS-FICTION42"}, nil)
	cases := []struct{ in, want string }{
		// a who value: the machine's and other domains' accounts, kept
		// system principals and SIDs
		{`who="WS-FICTION42\\amy, Users, NT SERVICE\\TrustedInstaller, S-1-1-0, S-1-5-21-9-8-7-500"`,
			`who="<host>\\<account-1>, Users, NT SERVICE\\TrustedInstaller, S-1-1-0, S-1-5-21-<machine>-500"`},
		{`"who": "OTHERDOM\\Bo Lin, Everyone"`, `"who": "<domain>\\<account-2>, Everyone"`},
		// the same name in another case and elsewhere in the text
		{`ws-fiction42\AMY and OTHERDOM\bo lin left`, `<host>\<account-1> and <domain>\<account-2> left`},
		// Unix groups: the system's stay
		{`who="group carla, group staff, others, owner 1001"`, `who="group <account-3>, group staff, others, owner 1001"`},
		// the bare machine name; a UNC path keeps its share name
		{`host ws-fiction42 at \\WS-FICTION42\share`, `host <host> at \\<host>\share`},
		// not a SID of a machine or a domain
		{`S-1-5-32-545 S-1-5-18 S-1-5-80-11-22-33-44-55`, `S-1-5-32-545 S-1-5-18 S-1-5-80-11-22-33-44-55`},
		// SEC34 中-1: a learned group in the checks' detail text (macOS, Linux)
		{`"detail": "/srv/lf 可写，但本机的其他账户也能修改其中的文件（group carla, others）"`,
			`"detail": "/srv/lf 可写，但本机的其他账户也能修改其中的文件（group <account-3>, others）"`},
		{`(group carla)`, `(group <account-3>)`},
		// SEC34 低-2: an Entra ID object and a Microsoft account
		{`owner S-1-12-1-1234567890-1234567891-1234567892-1234567893 done`, `owner S-1-12-1-<account> done`},
		{`S-1-11-96-3623454863-58364-18864-2661722203-1597581903-1111111111-2222222222, x`, `S-1-11-96-<account>, x`},
	}
	// SEC34 信息-3: in a JSON file a single backslash is an escape; "\\" is
	// the account's (the bare machine name is still replaced)
	jsonIn := `{"msg": "copied to WS-FICTION42\nnext line", "who": "WS-FICTION42\\amy", "x": "OTHERDOM\tlater"}`
	jsonWant := `{"msg": "copied to <host>\nnext line", "who": "<host>\\<account-1>", "x": "OTHERDOM\tlater"}`
	for _, c := range cases {
		r.learn(c.in, false)
	}
	r.learn(jsonIn, true)
	for _, c := range cases {
		if got := r.apply(c.in, false); got != c.want {
			t.Errorf("apply(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if got := r.apply(jsonIn, true); got != jsonWant {
		t.Errorf("apply(JSON)\n got %q\nwant %q", got, jsonWant)
	}
}

// SEC34 测试-1: the zip learns the who values of every file before it
// redacts any. The account's who value is only in an older, rotated log
// (later in the zip); the newer log names the same account in other forms.
// Both get the same placeholder.
func TestDiagnosticsLearnAllFilesFirst(t *testing.T) {
	stubAppInfo(t)
	e := newEnv(t)
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	newer := strings.Join([]string{
		`time=2026-11-13T10:00:00Z level=WARN msg="restrict" err="other accounts can still reach it (QUELLDOM\\vorn.tes, Users)"`,
		`time=2026-11-13T10:00:01Z level=INFO msg="note" account=QUELLDOM\vorn.tes`,
	}, "\n") + "\n"
	older := `time=2026-11-01T10:00:00Z level=WARN msg="api: other accounts may change entries of the data directory" entry=D:\lf\data who="QUELLDOM\\vorn.tes, Users" entries=1` + "\n"
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(newer), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(logs, "lectureflow-2026-11-01.log")
	if err := os.WriteFile(oldPath, []byte(older), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	gotNew, gotOld := files["logs/lectureflow.log"], files["logs/lectureflow-2026-11-01.log"]
	for name, content := range map[string]string{"newer": gotNew, "older": gotOld} {
		if low := strings.ToLower(content); strings.Contains(low, "quelldom") || strings.Contains(low, "vorn") {
			t.Errorf("%s log keeps the account:\n%s", name, content)
		}
	}
	m := regexp.MustCompile(`who="<domain>\\\\(<account-\d+>), Users"`).FindStringSubmatch(gotOld)
	if m == nil {
		t.Fatalf("older log:\n%s", gotOld)
	}
	for _, want := range []string{`(<domain>\\` + m[1] + `, Users)`, `account=<domain>\` + m[1]} {
		if !strings.Contains(gotNew, want) {
			t.Errorf("newer log lacks %q:\n%s", want, gotNew)
		}
	}
}

// SEC34 信息-1: a key hint is "sk-…" after a word boundary only.
func TestRedactKeyHintBoundary(t *testing.T) {
	r := &redactor{}
	cases := map[string]string{
		`已配置（sk-…AbC9）`:         `已配置（sk-…）`,
		`"key": "sk-…Zz=="`:     `"key": "sk-…"`,
		`disk-…tail task-…xy9z`: `disk-…tail task-…xy9z`,
	}
	for in, want := range cases {
		if got := string(r.apply([]byte(in))); got != want {
			t.Errorf("apply(%q) = %q, want %q", in, got, want)
		}
	}
}

// SEC34b 新-1: a log over the budget starts with "[earlier lines omitted]";
// it is still a log, not JSON: an unquoted who value with one backslash
// (who=DOMAIN\name) and the other mentions of that account are replaced.
func TestDiagnosticsTruncatedLogIsNotJSON(t *testing.T) {
	stubAppInfo(t)
	t.Setenv("COMPUTERNAME", "WS-FICTION42")
	t.Setenv("USERDOMAIN", "WS-FICTION42")
	e := newEnv(t)
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	tail := strings.Join([]string{
		`time=2026-11-13T10:00:00Z level=WARN msg="api: the data directory is still reachable by other accounts" dir=D:\lf who=FOLDDOM\pelkora`,
		`time=2026-11-13T10:00:01Z level=WARN msg="api: the data directory is still reachable by other accounts" dir=D:\lf who=WS-FICTION42\qobrin`,
		`time=2026-11-13T10:00:02Z level=INFO msg="note" account=FOLDDOM\pelkora`,
		`time=2026-11-13T10:00:03Z level=WARN msg="restrict" err="other accounts can still reach it (FOLDDOM\\pelkora, Users)"`,
	}, "\n") + "\n"
	big := strings.Repeat(line, (diagLogBudget+(1<<20))/len(line)) + tail
	if err := os.WriteFile(filepath.Join(logs, "lectureflow.log"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %.200s", res.StatusCode, body)
	}
	got := unzipAll(t, body)["logs/lectureflow.log"]
	if !strings.HasPrefix(got, "[earlier lines omitted]\n") {
		t.Fatalf("the log was not cut: %.60q", got)
	}
	end := got[max(0, len(got)-2000):]
	low := strings.ToLower(end)
	for _, bad := range []string{"folddom", "pelkora", "qobrin", "ws-fiction42"} {
		if strings.Contains(low, bad) {
			t.Errorf("the cut log keeps %q:\n%s", bad, end)
		}
	}
	m := regexp.MustCompile(`who=<domain>\\(<account-\d+>)\n`).FindStringSubmatch(end)
	if m == nil {
		t.Fatalf("the cut log:\n%s", end)
	}
	for _, want := range []string{`who=<host>\<account-`, `account=<domain>\` + m[1] + "\n", `(<domain>\\` + m[1] + `, Users)`} {
		if !strings.Contains(end, want) {
			t.Errorf("the cut log lacks %q:\n%s", want, end)
		}
	}
}

// SEC34b 新-1: text that starts with "[" is not JSON to the rules.
func TestRedactAccountsBracketTextIsNotJSON(t *testing.T) {
	r := newAccountRedactor([]string{"WS-FICTION42"}, nil)
	in := "[earlier lines omitted]\nlevel=WARN who=FOLDDOM\\pelkora\nlevel=INFO who=WS-FICTION42\\qobrin account=FOLDDOM\\pelkora\n"
	want := "[earlier lines omitted]\nlevel=WARN who=<domain>\\<account-1>\nlevel=INFO who=<host>\\<account-2> account=<domain>\\<account-1>\n"
	r.learn(in, false)
	if got := r.apply(in, false); got != want {
		t.Errorf("apply\n got %q\nwant %q", got, want)
	}
}

// SEC34c MJ: a file under logs/ is a log whatever its name ends with: a
// "*.json" log keeps a single backslash as a separator (who=DOMAIN\name),
// so its accounts are replaced. Only the zip's own "*.json" are JSON.
func TestDiagnosticsLogsAreNeverJSON(t *testing.T) {
	for name, want := range map[string]bool{
		"info.json": true, "colab.json": true, "Remote-Servers.JSON": true,
		"logs/tool.json": false, "logs/sub/state.json": false, "logs/lectureflow.log": false, "README.txt": false,
	} {
		if got := diagFileIsJSON(name); got != want {
			t.Errorf("diagFileIsJSON(%q) = %v, want %v", name, got, want)
		}
	}
	stubAppInfo(t)
	e := newEnv(t)
	logs := filepath.Join(e.cfg.DataDir, "logs")
	if err := os.MkdirAll(filepath.Join(logs, "old", "LLM"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := `time=2026-11-13T10:00:00Z level=WARN msg="api: the data directory is still reachable by other accounts" who=FOLDDOM\pelkora` + "\n" +
		`time=2026-11-13T10:00:01Z level=INFO msg="note" account=FOLDDOM\pelkora` + "\n"
	if err := os.WriteFile(filepath.Join(logs, "tool.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	// a model request log in a nested directory named LLM stays out too
	if err := os.WriteFile(filepath.Join(logs, "old", "LLM", "01REQ.jsonl"), []byte(`{"prompt":"QELT LECTURE SENTENCE"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, body := e.do("POST", "/api/app/diagnostics", nil)
	if res.StatusCode != 200 {
		t.Fatalf("diagnostics: %d %s", res.StatusCode, body)
	}
	files := unzipAll(t, body)
	got, ok := files["logs/tool.json"]
	if !ok {
		t.Fatalf("zip has no logs/tool.json (has %v)", keys(files))
	}
	if low := strings.ToLower(got); strings.Contains(low, "folddom") || strings.Contains(low, "pelkora") {
		t.Errorf("logs/tool.json keeps the account:\n%s", got)
	}
	m := regexp.MustCompile(`who=<domain>\\(<account-\d+>)\n`).FindStringSubmatch(got)
	if m == nil || !strings.Contains(got, `account=<domain>\`+m[1]+"\n") {
		t.Errorf("logs/tool.json:\n%s", got)
	}
	for name, content := range files {
		if strings.Contains(strings.ToLower(name), "llm") || strings.Contains(content, "QELT LECTURE") {
			t.Errorf("zip has the model request log in %s", name)
		}
	}
}
