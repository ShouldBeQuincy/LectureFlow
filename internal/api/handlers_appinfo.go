// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

// The settings page's "App" and "About" sections (SPEC-v3 V11, V7):
//
//	GET  /api/app/storage      the data directory, its size and the free space
//	POST /api/app/open-logs    opens {data_dir}/logs in the system file manager
//	POST /api/app/diagnostics  a zip of recent logs and environment facts
//	GET  /api/app/license      LICENSE (text/plain)
//	GET  /api/app/notices      THIRD_PARTY_NOTICES.md (text/markdown)
//
// The diagnostics never contain lecture content or keys: logs/llm/ (prompts
// and transcript text) is left out, every file is passed through redact()
// (key shapes, Bearer / Authorization / X-LF-Token values, long hex runs,
// the configured provider keys and desktop token, key hints, user names in
// paths, this machine's name and other accounts: redact_account.go) and the
// settings carry no key hint.

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	lectureflow "lectureflow"
	"lectureflow/internal/hwdetect"
)

func init() {
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("GET /api/app/storage", s.getAppStorage)
		h("POST /api/app/open-logs", s.postOpenLogs)
		h("POST /api/app/diagnostics", s.postDiagnostics)
		h("GET /api/app/license", s.getLicense)
		h("GET /api/app/notices", s.getNotices)
	})
}

// diagLogBudget caps the log bytes of a diagnostics zip (before redaction).
const diagLogBudget = 5 << 20

// Injectable in tests (no file manager windows, no hardware probes).
var (
	// openFolder shows dir in the system file manager.
	openFolder = openFolderSystem
	// diagHardware is the hardware section of the diagnostics.
	diagHardware = func(ctx context.Context, dataDir string) (any, error) {
		hw, err := hwdetect.Detect(ctx, dataDir)
		if err != nil {
			return nil, err
		}
		return map[string]any{"hw": hw, "recommendation": hwdetect.Recommend(hw)}, nil
	}
)

// DiagSection adds one JSON file to the diagnostics zip; v3 files that own
// a subsystem (components, Colab) register theirs from init. ok = false
// leaves the file out (the subsystem is not available).
type DiagSection struct {
	Name    string // file name inside the zip, e.g. "components.json"
	Collect func(ctx context.Context, s *Server) (v any, ok bool)
}

var diagSections []DiagSection

// diagExportKey is the context key of one export's memo (diagOnce).
type diagExportKey struct{}

type diagExport struct {
	mu   sync.Mutex
	vals map[string]any
}

// withDiagExport starts one export: diagOnce computes each value once
// under the returned context.
func withDiagExport(ctx context.Context) context.Context {
	return context.WithValue(ctx, diagExportKey{}, &diagExport{vals: map[string]any{}})
}

// diagOnce is f's value, computed once per export (N31-I1: the redaction
// and remote-servers.json number the servers from one plan); outside an
// export every call computes it. f runs with the export's lock held: it
// must not call diagOnce itself (with any key), or it waits for itself
// (SEC32 信息-4).
func diagOnce[T any](ctx context.Context, key string, f func() T) T {
	e, _ := ctx.Value(diagExportKey{}).(*diagExport)
	if e == nil {
		return f()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.vals[key]; ok {
		return v.(T)
	}
	v := f()
	e.vals[key] = v
	return v
}

// ---- GET /api/app/storage ----

// AppStorage is GET /api/app/storage.
type AppStorage struct {
	DataDir   string `json:"data_dir"`   // absolute
	UsedBytes int64  `json:"used_bytes"` // regular files under the data directory
	FreeBytes int64  `json:"free_bytes"` // -1 = unknown
}

// The size walk is cached briefly: the page asks on every render.
var storageCache struct {
	sync.Mutex
	dir  string
	at   time.Time
	used int64
}

func (s *Server) absDataDir() string {
	if s.dataDir == "" {
		return ""
	}
	if a, err := filepath.Abs(s.dataDir); err == nil {
		return a
	}
	return s.dataDir
}

func dirBytes(ctx context.Context, root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries do not count
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (s *Server) getAppStorage(w http.ResponseWriter, r *http.Request) {
	dir := s.absDataDir()
	out := AppStorage{DataDir: dir, FreeBytes: -1}
	if dir != "" {
		storageCache.Lock()
		if storageCache.dir == dir && time.Since(storageCache.at) < 10*time.Second {
			out.UsedBytes = storageCache.used
		} else {
			out.UsedBytes = dirBytes(r.Context(), dir)
			if r.Context().Err() == nil {
				storageCache.dir, storageCache.at, storageCache.used = dir, time.Now(), out.UsedBytes
			}
		}
		storageCache.Unlock()
		if s.freeSpace != nil {
			if free, err := s.freeSpace(dir); err == nil {
				out.FreeBytes = free
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- POST /api/app/open-logs ----

// crossSite refuses a request another site's page sent (a form post needs
// no CORS preflight). Only defence in depth: secure refuses every such
// /api/ request before any handler (crossSiteFetch), and this uses the
// same rule (anything but same-origin, none or no Fetch Metadata: same-site
// and unknown values too), so it is never looser than that check (AUD1
// I-6). The desktop token already covers desktop mode.
func crossSite(w http.ResponseWriter, r *http.Request) bool {
	if crossSiteFetch(r) {
		writeError(w, http.StatusForbidden, "app.cross_site", nil)
		return true
	}
	return false
}

// POST /api/app/open-logs: creates {data_dir}/logs when missing and opens it
// (explorer, open, xdg-open). Works in web mode too: the server only
// listens on this computer. 500 app.open_logs_failed {path}.
func (s *Server) postOpenLogs(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	dir := filepath.Join(s.absDataDir(), "logs")
	if s.dataDir == "" {
		writeError(w, http.StatusServiceUnavailable, "server.unavailable", map[string]any{"service": "data_dir"})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.fail(w, r, apiErrDetail(http.StatusInternalServerError, "app.open_logs_failed", map[string]any{"path": dir}, err.Error()))
		return
	}
	if err := openFolder(dir); err != nil {
		s.fail(w, r, apiErrDetail(http.StatusInternalServerError, "app.open_logs_failed", map[string]any{"path": dir}, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

func openFolderSystem(dir string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// explorer.exe exits with status 1 even when it opened the window: only
	// a failure to start counts; reap the process in the background.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---- GET /api/app/license, /api/app/notices ----

func writeText(w http.ResponseWriter, contentType, text string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

func (s *Server) getLicense(w http.ResponseWriter, r *http.Request) {
	// The licence and its section 7 additional permission (LICENSE-EXCEPTION.md).
	writeText(w, "text/plain; charset=utf-8", lectureflow.License+"\n\n"+lectureflow.LicenseException)
}

func (s *Server) getNotices(w http.ResponseWriter, r *http.Request) {
	writeText(w, "text/markdown; charset=utf-8", lectureflow.ThirdPartyNotices)
}

// ---- POST /api/app/diagnostics ----

// diagReadme explains the zip to whoever opens it.
const diagReadme = `LectureFlow diagnostics
=======================

Created by "Export diagnostics" on the settings page. You decide whether to
share it. It contains:

  info.json          version, platform, data directory (user name replaced)
  app-settings.json  application settings, providers without keys, roles,
                     transcription and general settings
  checks.json        the last environment checks of the diagnostics page
  hardware.json      the detected hardware and the recommended components
  database.json      the number of rows of each database table
  colab.json         the Colab settings and state ("configured": false
                     when Colab is not set up)
  remote-servers.json  the transcription servers, names replaced (when on)
  logs/              the most recent server logs (at most 5 MB)

It never contains lecture content (recordings, transcripts, translations,
slides and syllabi), the model request logs (logs/llm/) or API keys, not
even their last characters. Values that look like keys or tokens are
replaced by <redacted>, key hints by "sk-…", user names in paths by <user>,
this computer's name by <host>, the names of other accounts (permission
checks) by <account-N> and the machine part of their SIDs by <machine>.
`

func (s *Server) postDiagnostics(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	ctx := withDiagExport(r.Context())
	now := time.Now()
	red := s.redactor(ctx)
	// the files are collected first: the redaction learns the account names
	// of every file before it redacts any (one placeholder per name in the
	// zip, redact_account.go)
	type diagFile struct {
		name string
		data []byte
	}
	var collected []diagFile
	add := func(name string, data []byte) error {
		collected = append(collected, diagFile{name, data})
		return nil
	}
	addJSON := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			b, _ = json.Marshal(map[string]string{"error": err.Error()})
		}
		return add(name, append(b, '\n'))
	}
	files := []struct {
		name string
		v    func() any
	}{
		{"info.json", func() any { return s.diagInfo(now) }},
		{"app-settings.json", func() any { return s.diagSettings(ctx) }},
		{"checks.json", func() any {
			if s.diag == nil {
				return []SystemCheck{}
			}
			return s.diag.snapshot()
		}},
		{"hardware.json", func() any {
			hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			v, err := diagHardware(hctx, s.absDataDir())
			if err != nil {
				return map[string]string{"error": err.Error()}
			}
			return v
		}},
		{"database.json", func() any { return s.diagTableCounts(ctx) }},
	}
	err := add("README.txt", []byte(diagReadme))
	for _, f := range files {
		if err == nil {
			err = addJSON(f.name, f.v())
		}
	}
	for _, sec := range diagSections {
		if err != nil {
			break
		}
		if v, ok := sec.Collect(ctx, s); ok {
			err = addJSON(sec.Name, v)
		}
	}
	if err == nil {
		err = s.diagLogs(add)
	}
	for _, f := range collected {
		red.accounts.learn(string(f.data), diagFileIsJSON(f.name))
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range collected {
		if err != nil {
			break
		}
		var zf io.Writer
		if zf, err = zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: now}); err == nil {
			_, err = zf.Write(red.applyAs(f.data, diagFileIsJSON(f.name)))
		}
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := "lectureflow-diagnostics-" + now.Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) diagInfo(now time.Time) map[string]any {
	return map[string]any{
		"version": s.appVersion(), "mode": s.mode(), "platform": runtime.GOOS, "arch": runtime.GOARCH,
		"go_version": runtime.Version(), "cpus": runtime.NumCPU(), "data_dir": s.absDataDir(),
		"created_at": now.Format(time.RFC3339),
	}
}

func (s *Server) diagSettings(ctx context.Context) map[string]any {
	out := map[string]any{"config": s.configView()}
	if a, err := s.appSettings(ctx); err == nil {
		out["app"] = a
	} else {
		out["app_error"] = err.Error()
	}
	if s.set != nil {
		v, err := s.settingsView(ctx)
		if err != nil {
			out["settings_error"] = err.Error()
		} else {
			for i := range v.Providers {
				v.Providers[i].KeyHint = "" // not even the last characters
			}
			out["settings"] = v
		}
	}
	return out
}

// diagTableCounts opens the database read-only and counts the rows of each
// table (no row content).
func (s *Server) diagTableCounts(ctx context.Context) map[string]any {
	path := filepath.Join(s.absDataDir(), "lectureflow.db")
	if _, err := os.Stat(path); err != nil {
		return map[string]any{"error": "database file not found"}
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer d.Close()
	rows, err := d.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	counts := map[string]int64{}
	for _, n := range names {
		var c int64
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(n, `"`, `""`)+`"`).Scan(&c); err == nil {
			counts[n] = c
		}
	}
	var version int64
	_ = d.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version)
	return map[string]any{"schema_version": version, "tables": counts}
}

// diagLogs adds the newest files under {data_dir}/logs (never a directory
// named llm) until diagLogBudget bytes; a file that does not fit whole
// contributes its last lines.
func (s *Server) diagLogs(add func(name string, data []byte) error) error {
	root := filepath.Join(s.absDataDir(), "logs")
	type logFile struct {
		path, rel string
		size      int64
		mod       time.Time
	}
	var list []logFile
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.EqualFold(d.Name(), "llm") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		list = append(list, logFile{path: p, rel: filepath.ToSlash(rel), size: fi.Size(), mod: fi.ModTime()})
		return nil
	})
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	left := int64(diagLogBudget)
	for _, f := range list {
		if left <= 0 {
			break
		}
		data, err := readTail(f.path, f.size, left)
		if err != nil {
			continue
		}
		left -= int64(len(data))
		if int64(len(data)) < f.size { // cut to fit: the budget is spent
			left = 0
		}
		if err := add("logs/"+f.rel, data); err != nil {
			return err
		}
	}
	return nil
}

// readTail reads the whole file, or its last max bytes starting at a line.
func readTail(path string, size, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if size <= max {
		return io.ReadAll(io.LimitReader(f, max))
	}
	b := make([]byte, max)
	n, _ := f.ReadAt(b, size-max)
	b = b[:n]
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}
	return append([]byte("[earlier lines omitted]\n"), b...), nil
}

// ---- redaction ----

type redactor struct {
	secrets  []string // exact values, longest first
	user     *regexp.Regexp
	words    []redactWord
	accounts *accountRedactor // nil: none (redact_account.go)
}

// redactWord is a whole word replaced: an SSH server's host or user name
// (ssh-1b), its name or slug (SEC7 M2). fold: in any case (ASCII letters;
// host names and server names), otherwise case-sensitive (user names).
// form: where the word is replaced (0: everywhere; N31-L2).
type redactWord struct {
	word, repl string
	fold       bool
	form       wordForm
}

// wordForm limits a word to the places where it is part of an address
// (N31-L2): a generic user or host name ("root", "gpu") is replaced there
// only, not as an ordinary word of the logs. 0: everywhere.
type wordForm uint8

const (
	// formUser: "word@…", the user of user@host, and a path segment
	// "/word/" or a path "/word" (its home, its directories).
	formUser wordForm = 1 << iota
	// formHost: "…@word", "//word", "[word]", "lookup word" and "word:22"
	// (a port of two digits or more; "cuda:0" is a device), a host of an
	// address.
	formHost
)

// allows: the occurrence s[j:end] of the word is in one of f's places.
func (f wordForm) allows(s string, j, end int) bool {
	if f == 0 {
		return true
	}
	if f&formUser != 0 {
		if end < len(s) && s[end] == '@' {
			return true
		}
		// a path of the user's (v3.2.x r5, SEC32 L-6: a remote installer's
		// stderr): a whole segment "…/root/…", or the root of a path
		// ("/root", "'/root'", "/root:"); "…/{id}/test" of a route stays
		if j > 0 && s[j-1] == '/' {
			if end < len(s) && s[end] == '/' {
				return true
			}
			if (j == 1 || strings.IndexByte(" \t\n\"'=([,", s[j-2]) >= 0) &&
				(end == len(s) || strings.IndexByte(" \t\r\n\"':),]", s[end]) >= 0) {
				return true
			}
		}
	}
	if f&formHost != 0 {
		switch {
		case j > 0 && s[j-1] == '@', j > 1 && s[j-2:j] == "//":
			return true
		case j > 0 && s[j-1] == '[' && end < len(s) && s[end] == ']':
			return true
		case j >= 7 && asciiLower(s[j-7:j]) == "lookup ":
			// a resolver's message of any text ("lookup gpu: operation
			// was canceled", v3.2.0's logs; SEC32 L-1)
			return true
		case end+2 < len(s) && s[end] == ':' && isDigitByte(s[end+1]) && isDigitByte(s[end+2]):
			return true
		}
	}
	return false
}

// redactWordSources add words the redaction replaces (remote_redact.go:
// the SSH servers' host and user names, names and slugs). Words shorter
// than 2 characters are left alone (they would hit ordinary text); for one
// word (ASCII case ignored) the last source's entry wins, it matches in
// any case when any entry does, and everywhere when any entry does (else
// in the places of every entry). ctx is the export's (diagOnce).
var redactWordSources []func(ctx context.Context, s *Server) []redactWord

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// the user information of URLs ("scheme://user@", "scheme://user:pass@",
	// a password with "@" or ":" in it): up to the last "@" before the path,
	// the query or the fragment, as internal/components does (userinfoRe).
	// First: a "token=" inside a password must not hide the "@" from it.
	// "mailto:a@b", an e-mail address and an "@" in a path or a query stay.
	// The scheme may be glued to a word ("_https://"; v3.2.x r5): the
	// scheme is kept, whatever is before it.
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/?#\s"'<>]*@`), "${1}<redacted>@"},
	// the same with the slashes JSON-escaped ("http:\/\/u:p@h", some Java
	// and PHP encoders)
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*:\\/\\/)[^/?#\s"'<>\\]*@`), "${1}<redacted>@"},
	// header-like values next: the whole value up to the end of the field
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)[^"'\r\n,}]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(x-lf-token["']?\s*[:=]\s*["']?)[^\s"',}]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=\-]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|access[_-]?token|secret|password|token)["']?\s*[:=]\s*["']?)[^\s"',}&]+`), "${1}<redacted>"},
	// key shapes: sk-…, sk-ant-…, and long hex runs (tokens, hashes)
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`), "sk-<redacted>"},
	// a key's hint (model.KeyHint, config.Mask: "sk-…" and the last 4
	// characters): the checks' key rows, an echoed key in a log (T19 F1)
	{regexp.MustCompile(`\bsk-…[^\s"'<>,;)）]{1,4}`), "sk-…"},
	{regexp.MustCompile(`\b[0-9A-Fa-f]{32,}\b`), "<redacted>"},
	// user names in home paths (C:\Users\x, C:\\Users\\x in JSON, /Users/x, /home/x)
	{regexp.MustCompile(`(?i)([A-Za-z]:(?:\\\\|\\|/)(?:Users|Documents and Settings)(?:\\\\|\\|/))[^\\/\s"'<>:]+`), "${1}<user>"},
	{regexp.MustCompile(`(/(?:Users|home)/)[^/\s"'<>:]+`), "${1}<user>"},
}

// redactor collects the secrets known to this server: provider keys, the
// desktop token and the values of *_API_KEY / *_TOKEN / *_SECRET variables.
func (s *Server) redactor(ctx context.Context) *redactor {
	var sec []string
	addSecret := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= 8 {
			sec = append(sec, v)
		}
	}
	if s.set != nil {
		for _, p := range s.set.Providers() {
			addSecret(p.APIKey)
		}
	}
	if s.desktop != nil {
		addSecret(s.desktop.Token)
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.ToUpper(k)
		if strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_TOKEN") || strings.HasSuffix(k, "_SECRET") {
			addSecret(v)
		}
	}
	sort.Slice(sec, func(i, j int) bool { return len(sec[i]) > len(sec[j]) })
	hosts, domains := localAccountDomains()
	r := &redactor{secrets: sec, accounts: newAccountRedactor(hosts, domains)}
	words := map[string]redactWord{}
	addWord := func(w redactWord) {
		if len(w.word) < 2 {
			return
		}
		key := asciiLower(w.word)
		if old, ok := words[key]; ok {
			w.fold = w.fold || old.fold
			if w.form == 0 || old.form == 0 {
				w.form = 0
			} else {
				w.form |= old.form
			}
		}
		words[key] = w
	}
	if name := diagUserName(); len(name) >= 3 {
		if genericLocalUsers[strings.ToLower(name)] {
			// a user name that names no one ("test", "admin", "user"):
			// in a user's places only, as a remote server's (SEC32b: the
			// slow-request log's ".../{id}/test" route stays); its home
			// paths are the path rules'
			addWord(redactWord{word: name, repl: redactedUser, fold: true, form: formUser})
		} else {
			r.user = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
		}
	}
	for _, src := range redactWordSources {
		for _, w := range src(ctx, s) {
			addWord(w)
		}
	}
	for _, w := range words {
		r.words = append(r.words, w)
	}
	sort.Slice(r.words, func(i, j int) bool {
		a, b := r.words[i].word, r.words[j].word
		return len(a) > len(b) || (len(a) == len(b) && a < b)
	})
	return r
}

// genericLocalUsers are this machine's user names that name no one (a
// default or placeholder account of every system). Smaller than the remote
// servers' genericRemoteUsers (SEC35 L-5): "opc", "lab", "git", "student"
// or a distribution's name is an image's account on a server, but on a
// desktop more likely a name the user chose (initials, a name): replaced
// everywhere. Lower case.
var genericLocalUsers = map[string]bool{
	"test": true, "admin": true, "administrator": true, "user": true, "guest": true, "root": true,
}

// diagUserName is this machine's user name for the redaction (tests: a
// made-up one).
var diagUserName = currentUserName

func currentUserName() string {
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if i := strings.LastIndexAny(name, `\/`); i >= 0 { // DOMAIN\name
		name = name[i+1:]
	}
	if name == "" {
		name = os.Getenv("USERNAME")
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	return name
}

// apply redacts text that is not a JSON file's.
func (r *redactor) apply(b []byte) []byte { return r.applyAs(b, false) }

// applyAs redacts b; js: b is a JSON file's text (diagFileIsJSON).
func (r *redactor) applyAs(b []byte, js bool) []byte {
	s := string(b)
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "<redacted>")
	}
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	for _, f := range redactFuncs {
		s = f(s)
	}
	s = r.accounts.apply(s, js)
	if r.user != nil {
		s = r.user.ReplaceAllString(s, "<user>")
	}
	for _, w := range r.words {
		s = replaceWord(s, w.word, w.repl, w.fold, w.form)
	}
	return []byte(redactUnknownSSHDevices(s))
}

// sshDeviceRe is an SSH server's device id, "ssh:<slug>".
var sshDeviceRe = regexp.MustCompile(`\bssh:([A-Za-z0-9_-]+)`)

// sshLabelRe is the device id the redaction gives a known server.
var sshLabelRe = regexp.MustCompile(`^server-[0-9]+$`)

// redactUnknownSSHDevices replaces the device ids left after the words
// (the known servers' are "ssh:server-N" by then) with "ssh:<server>": a
// server removed before the redaction history existed, or added while the
// zip was made, is in no history (N31-L3).
func redactUnknownSSHDevices(s string) string {
	if !strings.Contains(s, "ssh:") {
		return s
	}
	return sshDeviceRe.ReplaceAllStringFunc(s, func(m string) string {
		if sshLabelRe.MatchString(m[len("ssh:"):]) {
			return m
		}
		return "ssh:<server>"
	})
}

// replaceWord replaces the whole-word occurrences of w in s, in one pass
// (a replacement is never scanned again). A word is delimited by anything
// but letters, digits, "_" and "-" (and, before it, "."): the host
// "gpu.example.net" is replaced in "gpu.example.net:22" and
// "gpu.example.net.", not in "my-gpu.example.net" or "x.gpu.example.net".
// fold: ASCII letters match in any case ("GPU7.Example.NET"). form: only
// the occurrences in its places (wordForm).
func replaceWord(s, w, repl string, fold bool, form wordForm) string {
	inWord := func(c byte, before bool) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || (before && c == '.')
	}
	hay := s // searched; the same byte offsets as s
	if fold {
		hay, w = asciiLower(s), asciiLower(w)
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.Index(hay[i:], w)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		end := j + len(w)
		marker := j > 0 && s[j-1] == '<' && end < len(s) && s[end] == '>' // "<user>" of an earlier replacement
		if !marker && (j == 0 || !inWord(s[j-1], true)) && (end == len(s) || !inWord(s[end], false)) && form.allows(s, j, end) {
			b.WriteString(s[i:j])
			b.WriteString(repl)
			i = end
			continue
		}
		b.WriteString(s[i : j+1])
		i = j + 1
	}
	return b.String()
}
