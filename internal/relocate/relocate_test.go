// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/colab"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// ---- fixtures ----

// fixture is a data directory: a database made by the real migrations
// (kept open, as the running application keeps it), media files and
// absolute paths in plain columns and inside JSON.
type fixture struct {
	dir string
	db  *sql.DB
}

var fixtureFiles = map[string]string{
	"courses/C1/lectures/L1/media.mp4":    "video bytes",
	"courses/C1/lectures/L1/audio.wav":    "wave bytes",
	"courses/C1/materials/M1/source.pdf":  "%PDF-1.7",
	"courses/C1/lectures/L1/job.json":     `{"audio": "somewhere"}`,
	"components/python/1.0/pyvenv.cfg":    "home = somewhere",
	"backups/lectureflow-old.db":          "old backup",
	"logs/lectureflow.log":                "log line\n",
	"models/openvino/whisper/model.bin":   "weights",
	"selftest/cuda_0/clip.wav":            "clip",
	"courses/C1/lectures/L1/export/a.srt": "1\n00:00:00,000 --> 00:00:01,000\nhi\n",
	"courses/C1/lectures/L1/empty.txt":    "",
	"courses/C1/lectures/L1/中文 名字.txt":    "unicode name",
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// jsonEsc is s as it appears inside a JSON string.
func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// The fixture's kv rows holding paths (a JSON value, a slash path, prose).
const (
	kvFixtureJSON  = model.KVResumePrefix + "L1"
	kvFixtureSlash = "title_legacy:L1"
	kvFixtureProse = model.KVJanitorLastRunPrefix + "C1"
)

// seed inserts rows with absolute paths under dir into d (columns present
// in every schema version).
func seed(t *testing.T, d *sql.DB, dir string) {
	t.Helper()
	media := filepath.Join(dir, "courses", "C1", "lectures", "L1", "media.mp4")
	pdf := filepath.Join(dir, "courses", "C1", "materials", "M1", "source.pdf")
	sibling := dir + "2" + string(filepath.Separator) + "x.mp4" // a sibling directory: never rewritten
	stmts := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO course (id, name, created_at) VALUES ('C1', 'Cryptography', '2026-01-01T00:00:00Z')", nil},
		{"INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('L1', 'C1', 'Week 1', ?, '2026-01-02T00:00:00Z')", []any{media}},
		{"INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('L2', 'C1', 'Week 2', ?, '2026-01-03T00:00:00Z')", []any{sibling}},
		{"INSERT INTO material (id, course_id, filename, path, created_at) VALUES ('M1', 'C1', 'source.pdf', ?, '2026-01-02T00:00:00Z')", []any{pdf}},
		// kv rows of keys an import keeps (importedKV), so the import tests see them rewritten too
		{"INSERT INTO kv (key, value) VALUES ('" + kvFixtureJSON + "', ?)", []any{`{"audio":"` + jsonEsc(media) + `","n":1}`}},
		{"INSERT INTO kv (key, value) VALUES ('" + kvFixtureSlash + "', ?)", []any{filepath.ToSlash(media)}},
		{"INSERT INTO kv (key, value) VALUES ('" + kvFixtureProse + "', ?)", []any{"open " + pdf + ": denied"}},
	}
	for _, s := range stmts {
		if _, err := d.Exec(s.q, s.args...); err != nil {
			t.Fatalf("%s: %v", s.q, err)
		}
	}
}

// newFixture makes a data directory with a current-version database.
func newFixture(t *testing.T, dir string) *fixture {
	t.Helper()
	writeFiles(t, dir, fixtureFiles)
	d, err := db.OpenDB(filepath.Join(dir, DBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	seed(t, d, dir)
	return &fixture{dir: dir, db: d}
}

// migrateFiles applies internal/db/migrations up to upTo to d (the upgrade
// fixtures need an older schema; db.migrateUpTo is not exported).
func migrateFiles(t *testing.T, d *sql.DB, upTo int) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, p := range names {
		n, err := strconv.Atoi(strings.SplitN(filepath.Base(p), "_", 2)[0])
		if err != nil || n > upTo {
			continue
		}
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if _, err := d.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			t.Fatal(err)
		}
	}
}

func readRows(t *testing.T, path string) map[string]string {
	t.Helper()
	d, err := openDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out := map[string]string{}
	q := func(key, query string) {
		var v sql.NullString
		if err := d.QueryRow(query).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s: %v", query, err)
		}
		out[key] = v.String
	}
	q("media", "SELECT media_path FROM lecture WHERE id = 'L1'")
	q("sibling", "SELECT media_path FROM lecture WHERE id = 'L2'")
	q("pdf", "SELECT path FROM material WHERE id = 'M1'")
	q("json", "SELECT value FROM kv WHERE key = '"+kvFixtureJSON+"'")
	q("slash", "SELECT value FROM kv WHERE key = '"+kvFixtureSlash+"'")
	q("prose", "SELECT value FROM kv WHERE key = '"+kvFixtureProse+"'")
	q("old", "SELECT value FROM kv WHERE key = '"+KVOldDataDir+"'")
	q("onboarding", "SELECT value FROM kv WHERE key = '"+model.KVOnboardingState+"'")
	return out
}

func codeOf(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

// listTree is every file and directory under root (slash paths).
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- the rewriter ----

func TestRewriterForms(t *testing.T) {
	r := newRewriterFold(`C:\Users\x\data`, `D:\New Place\LF`, true)
	cases := []struct{ in, want string }{
		{`C:\Users\x\data\courses\a.mp4`, `D:\New Place\LF\courses\a.mp4`},
		{`c:\users\X\DATA\courses\a.mp4`, `D:\New Place\LF\courses\a.mp4`},
		{`C:\Users\x\data`, `D:\New Place\LF`},
		{`{"audio":"C:\\Users\\x\\data\\courses\\a.wav"}`, `{"audio":"D:\\New Place\\LF\\courses\\a.wav"}`},
		{`C:/Users/x/data/courses/a.mp4`, `D:/New Place/LF/courses/a.mp4`},
		{`open C:\Users\x\data\a: denied; C:\Users\x\data\b`, `open D:\New Place\LF\a: denied; D:\New Place\LF\b`},
		// not on a boundary: unchanged
		{`C:\Users\x\data2\a.mp4`, `C:\Users\x\data2\a.mp4`},
		{`C:\Users\x\data.bak`, `C:\Users\x\data.bak`},
		{`E:\C:\Users\x\data\a`, `E:\C:\Users\x\data\a`},
		{`plain text`, `plain text`},
	}
	for _, c := range cases {
		got, _ := r.rewrite(c.in)
		if got != c.want {
			t.Errorf("rewrite(%q) = %q, want %q", c.in, got, c.want)
		}
		if r.residual(got) {
			t.Errorf("residual after rewrite(%q): %q", c.in, got)
		}
	}
	// Case-sensitive (POSIX): another case is another directory, and a new
	// prefix containing the old one is not a residual.
	p := newRewriterFold(`/home/u/data`, `/mnt/big/home/u/data`, false)
	if got, _ := p.rewrite(`/HOME/u/data/a`); got != `/HOME/u/data/a` {
		t.Errorf("case-sensitive rewrite changed %q", got)
	}
	got, _ := p.rewrite(`/home/u/data/a`)
	if got != `/mnt/big/home/u/data/a` || p.residual(got) {
		t.Errorf("rewrite = %q (residual %v)", got, p.residual(got))
	}
}

// ---- MoveDataDir ----

type moveEnv struct {
	src, dst, loc string
	fx            *fixture
}

// newMoveEnv: the source is the default data directory (it holds the shell
// profile and is where location.json goes); the target does not exist yet.
func newMoveEnv(t *testing.T) *moveEnv {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "LectureFlow")
	fx := newFixture(t, src)
	writeFiles(t, src, map[string]string{"electron/Local State": "{}",
		// the browser extension's native messaging files (internal/nmhost)
		"nmhost/launch.json": "{}", "nmhost/org.lectureflow.nmhost.chromium.json": "{}", "run/nm.sock": ""})
	return &moveEnv{src: src, dst: filepath.Join(root, "Big Disk", "LectureFlow Data"), loc: filepath.Join(src, LocationName), fx: fx}
}

func (e *moveEnv) opts() MoveOptions {
	return MoveOptions{Src: e.src, Dst: e.dst, LocationFile: e.loc}
}

func TestMoveDataDir(t *testing.T) {
	e := newMoveEnv(t)
	before := listTree(t, e.src)
	var fixed [2]string
	var last Progress
	var phases []string
	o := e.opts()
	o.FixComponents = func(oldRoot, newRoot string) error {
		fixed = [2]string{oldRoot, newRoot}
		if !exists(filepath.Join(newRoot, "components", "python", "1.0", "pyvenv.cfg")) {
			return errors.New("components not copied yet")
		}
		return nil
	}
	o.SetKV = map[string]string{"gpu_paused": "0"}
	o.Progress = func(p Progress) {
		last = p
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}
	committed := false
	o.BeforeCommit = func() error {
		committed = true
		if exists(e.loc) {
			return errors.New("location written before commit")
		}
		return nil
	}
	if err := MoveDataDir(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !committed || fixed != [2]string{e.src, e.dst} {
		t.Fatalf("committed %v, FixComponents %v", committed, fixed)
	}
	if strings.Join(phases, ",") != "copying,verifying" || last.CopiedBytes != last.TotalBytes || last.TotalBytes == 0 {
		t.Fatalf("progress %v %+v", phases, last)
	}
	// location.json points at the new directory.
	b, err := os.ReadFile(e.loc)
	if err != nil {
		t.Fatal(err)
	}
	var loc struct {
		DataDir string `json:"data_dir"`
	}
	if err := json.Unmarshal(b, &loc); err != nil || loc.DataDir != e.dst {
		t.Fatalf("location.json %s (%v)", b, err)
	}
	// Every file copied with its content; the shell's files and the live
	// database files stay behind.
	for rel, body := range fixtureFiles {
		got, err := os.ReadFile(filepath.Join(e.dst, filepath.FromSlash(rel)))
		if err != nil || string(got) != body {
			t.Errorf("%s: %q, %v", rel, got, err)
		}
	}
	for _, rel := range []string{"electron", "nmhost", "run", LocationName, DBName + "-wal", DBName + "-shm"} {
		if exists(filepath.Join(e.dst, rel)) {
			t.Errorf("%s copied", rel)
		}
	}
	// The paths of the copy are rewritten in every spelling.
	rows := readRows(t, filepath.Join(e.dst, DBName))
	media := filepath.Join(e.dst, "courses", "C1", "lectures", "L1", "media.mp4")
	want := map[string]string{
		"media":   media,
		"sibling": e.src + "2" + string(filepath.Separator) + "x.mp4",
		"pdf":     filepath.Join(e.dst, "courses", "C1", "materials", "M1", "source.pdf"),
		"json":    `{"audio":"` + jsonEsc(media) + `","n":1}`,
		"slash":   filepath.ToSlash(media),
		"prose":   "open " + filepath.Join(e.dst, "courses", "C1", "materials", "M1", "source.pdf") + ": denied",
		"old":     e.src,
	}
	for k, v := range want {
		if rows[k] != v {
			t.Errorf("%s = %q, want %q", k, rows[k], v)
		}
	}
	// The copy is a migrated, intact database the application can open.
	st, err := db.Open(filepath.Join(e.dst, DBName))
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := st.GetKV(context.Background(), "gpu_paused")
	st.Close()
	if v != "0" {
		t.Errorf("SetKV gpu_paused = %q", v)
	}
	// The source is untouched.
	if after := listTree(t, e.src); strings.Join(after, "|") != strings.Join(withSorted(before, LocationName), "|") {
		t.Errorf("source tree changed:\n%v\n%v", before, after)
	}
	src := readRows(t, filepath.Join(e.src, DBName))
	if src["media"] != filepath.Join(e.src, "courses", "C1", "lectures", "L1", "media.mp4") || src["old"] != "" {
		t.Errorf("source database changed: %v", src)
	}
}

func withSorted(list []string, add string) []string {
	out := append(append([]string{}, list...), add)
	sort.Strings(out)
	return out
}

func TestMoveRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("target not empty", func(t *testing.T) {
		e := newMoveEnv(t)
		writeFiles(t, e.dst, map[string]string{"keep.txt": "mine"})
		err := MoveDataDir(ctx, e.opts())
		if codeOf(err) != CodeTargetNotEmpty {
			t.Fatalf("err %v", err)
		}
		if exists(e.loc) || strings.Join(listTree(t, e.dst), "|") != "keep.txt" {
			t.Fatalf("location %v, target %v", exists(e.loc), listTree(t, e.dst))
		}
	})
	t.Run("target inside source", func(t *testing.T) {
		e := newMoveEnv(t)
		e.dst = filepath.Join(e.src, "sub")
		if err := MoveDataDir(ctx, e.opts()); codeOf(err) != CodeTargetInside {
			t.Fatalf("err %v", err)
		}
		if exists(e.dst) || exists(e.loc) {
			t.Fatal("left something behind")
		}
	})
	t.Run("target contains source", func(t *testing.T) {
		e := newMoveEnv(t)
		e.dst = filepath.Dir(e.src)
		if err := MoveDataDir(ctx, e.opts()); codeOf(err) != CodeTargetContains {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("same directory", func(t *testing.T) {
		e := newMoveEnv(t)
		e.dst = e.src + string(filepath.Separator)
		if err := MoveDataDir(ctx, e.opts()); codeOf(err) != CodeSameDir {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("relative path", func(t *testing.T) {
		e := newMoveEnv(t)
		e.dst = "relative/dir"
		if err := MoveDataDir(ctx, e.opts()); codeOf(err) != CodePathInvalid {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no space", func(t *testing.T) {
		e := newMoveEnv(t)
		old := freeSpace
		t.Cleanup(func() { freeSpace = old })
		var need int64
		freeSpace = func(string) (int64, error) { return 1000, nil }
		err := MoveDataDir(ctx, e.opts())
		if codeOf(err) != CodeNoSpace {
			t.Fatalf("err %v", err)
		}
		var ce *model.CodedError
		errors.As(err, &ce)
		need = ce.Info.Params["need_bytes"].(int64)
		if exists(e.dst) || exists(e.loc) {
			t.Fatal("left something behind")
		}
		// Exactly size × 1.1 is enough.
		freeSpace = func(string) (int64, error) { return need, nil }
		if _, err := CheckMove(ctx, e.src, e.dst, e.loc); err != nil {
			t.Fatalf("with %d free: %v", need, err)
		}
		freeSpace = func(string) (int64, error) { return need - 1, nil }
		if _, err := CheckMove(ctx, e.src, e.dst, e.loc); codeOf(err) != CodeNoSpace {
			t.Fatalf("with %d free: %v", need-1, err)
		}
	})
}

// A failure halfway through the copy removes what this run created (the
// target and the parents it made) and writes no location.json.
func TestMoveCopyFailureCleansUp(t *testing.T) {
	e := newMoveEnv(t)
	n := 0
	copyFileHook = func(rel string) error {
		n++
		if n == 4 {
			return errors.New("disk unplugged")
		}
		return nil
	}
	t.Cleanup(func() { copyFileHook = nil })
	err := MoveDataDir(context.Background(), e.opts())
	if codeOf(err) != CodeCopyFailed {
		t.Fatalf("err %v", err)
	}
	if exists(e.loc) {
		t.Fatal("location.json written")
	}
	if exists(filepath.Dir(e.dst)) {
		t.Fatalf("target left behind: %v", listTree(t, filepath.Dir(e.dst)))
	}
	// An existing empty target stays (it was not ours) and is empty again.
	if err := os.MkdirAll(e.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	n = 0
	if err := MoveDataDir(context.Background(), e.opts()); codeOf(err) != CodeCopyFailed {
		t.Fatalf("err %v", err)
	}
	if !exists(e.dst) || len(listTree(t, e.dst)) != 0 {
		t.Fatalf("target: exists %v, %v", exists(e.dst), listTree(t, e.dst))
	}
}

// An old prefix left in the copy fails the verification.
func TestMoveResidualPrefixCaught(t *testing.T) {
	e := newMoveEnv(t)
	afterRewrite = func(d *sql.DB) {
		d.Exec("UPDATE lecture SET title = ? WHERE id = 'L1'", filepath.Join(e.src, "courses", "x"))
	}
	t.Cleanup(func() { afterRewrite = nil })
	err := MoveDataDir(context.Background(), e.opts())
	if codeOf(err) != CodeVerifyFailed {
		t.Fatalf("err %v", err)
	}
	var ce *model.CodedError
	errors.As(err, &ce)
	if ce.Info.Params["what"] != "old_prefix:lecture.title" {
		t.Fatalf("what %v", ce.Info.Params)
	}
	if exists(e.loc) || exists(filepath.Dir(e.dst)) {
		t.Fatal("left something behind")
	}
}

// A commit to the source database after the snapshot fails the move.
func TestMoveSourceChanged(t *testing.T) {
	e := newMoveEnv(t)
	copyFileHook = func(string) error {
		_, err := e.fx.db.Exec("INSERT INTO kv (key, value) VALUES ('late', 'x') ON CONFLICT(key) DO UPDATE SET value = 'y'")
		return err
	}
	t.Cleanup(func() { copyFileHook = nil })
	if err := MoveDataDir(context.Background(), e.opts()); codeOf(err) != CodeSourceChanged {
		t.Fatalf("err %v", err)
	}
	if exists(e.loc) || exists(filepath.Dir(e.dst)) {
		t.Fatal("left something behind")
	}
}

func TestMoveComponentsAndCommitFailures(t *testing.T) {
	e := newMoveEnv(t)
	o := e.opts()
	o.FixComponents = func(string, string) error { return errors.New("venv broken") }
	if err := MoveDataDir(context.Background(), o); codeOf(err) != CodeComponentsFailed {
		t.Fatalf("err %v", err)
	}
	o = e.opts()
	o.BeforeCommit = func() error { return model.Errorf("data_dir.busy", nil, nil) }
	if err := MoveDataDir(context.Background(), o); codeOf(err) != "data_dir.busy" {
		t.Fatalf("err %v", err)
	}
	if exists(e.loc) || exists(filepath.Dir(e.dst)) {
		t.Fatal("left something behind")
	}
}

// Moving back into the default directory: the shell's own files there do
// not make it "not empty".
func TestMoveIntoDefaultDir(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "LectureFlow")
	writeFiles(t, def, map[string]string{"electron/Local State": "{}", LocationName: `{"data_dir":"x"}`,
		"nmhost/launch.json": "{}", "run/nm.sock": ""})
	src := filepath.Join(root, "moved")
	newFixture(t, src)
	loc := filepath.Join(def, LocationName)
	if err := MoveDataDir(context.Background(), MoveOptions{Src: src, Dst: def, LocationFile: loc}); err != nil {
		t.Fatal(err)
	}
	if rows := readRows(t, filepath.Join(def, DBName)); rows["media"] != filepath.Join(def, "courses", "C1", "lectures", "L1", "media.mp4") {
		t.Fatalf("rows %v", rows)
	}
	if !exists(filepath.Join(def, "electron", "Local State")) {
		t.Fatal("shell profile lost")
	}
	if !exists(filepath.Join(def, "nmhost", "launch.json")) || !exists(filepath.Join(def, "run", "nm.sock")) {
		t.Fatal("native messaging files lost")
	}
}

func TestIsInstallationEntry(t *testing.T) {
	for _, n := range []string{"location.json", "electron", "Electron", "nmhost", "NMHost", "run"} {
		if !IsInstallationEntry(n) {
			t.Errorf("%s: want installation entry", n)
		}
	}
	for _, n := range []string{"courses", "lectureflow.db", "components", "downloads", "nmhost2", "runs"} {
		if IsInstallationEntry(n) {
			t.Errorf("%s: want data", n)
		}
	}
}

// ---- import ----

// importableFixture drops the fixture's media path outside the data
// directory (lecture L2's sibling directory, which the move tests keep
// unrewritten): an import refuses a stored path outside its record's
// directory (SEC44 medium-2).
func importableFixture(t *testing.T, d *sql.DB) {
	t.Helper()
	if _, err := d.Exec("UPDATE lecture SET media_path = '' WHERE id = 'L2'"); err != nil {
		t.Fatal(err)
	}
}

type importEnv struct {
	src, cur string // the development data dir; the current (new) data dir
}

// newImportEnv: src is a development data directory, cur a fresh install
// whose database has only the onboarding state.
func newImportEnv(t *testing.T) *importEnv {
	t.Helper()
	root := t.TempDir()
	e := &importEnv{src: filepath.Join(root, "dev", "data"), cur: filepath.Join(root, "AppData", "LectureFlow")}
	importableFixture(t, newFixture(t, e.src).db)
	if err := os.MkdirAll(e.cur, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenDB(filepath.Join(e.cur, DBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("INSERT INTO kv (key, value) VALUES (?, ?)", model.KVOnboardingState, `{"step":"welcome","done":false,"data":{}}`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	return e
}

func TestCheckImportSource(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	info, err := CheckImportSource(ctx, e.src)
	if err != nil {
		t.Fatal(err)
	}
	sup, _ := SupportedVersion()
	if info.Version != sup || info.Courses != 1 || info.Lectures != 2 || info.Bytes == 0 {
		t.Fatalf("info %+v (supported %d)", info, sup)
	}
	// An empty folder and a database of something else are refused.
	empty := t.TempDir()
	if _, err := CheckImportSource(ctx, empty); codeOf(err) != CodeNotLectureFlow {
		t.Fatalf("empty: %v", err)
	}
	other := t.TempDir()
	d, err := openDB(filepath.Join(other, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	d.Exec("CREATE TABLE notes (x TEXT)")
	d.Exec("PRAGMA user_version = 3")
	d.Close()
	if _, err := CheckImportSource(ctx, other); codeOf(err) != CodeNotLectureFlow {
		t.Fatalf("other db: %v", err)
	}
	// A newer schema than this program's is refused.
	newer := t.TempDir()
	d, err = db.OpenDB(filepath.Join(newer, DBName))
	if err != nil {
		t.Fatal(err)
	}
	d.Exec(fmt.Sprintf("PRAGMA user_version = %d", sup+1))
	d.Close()
	if _, err := CheckImportSource(ctx, newer); codeOf(err) != CodeTooNew {
		t.Fatalf("newer: %v", err)
	}
}

func TestImportRefusesNonFreshLibrary(t *testing.T) {
	e := newImportEnv(t)
	d, err := openDB(filepath.Join(e.cur, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	d.Exec("INSERT INTO course (id, name, created_at) VALUES ('X', 'Mine', '2026-01-01T00:00:00Z')")
	d.Close()
	err = StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur})
	if codeOf(err) != CodeNotFresh {
		t.Fatalf("err %v", err)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging created")
	}
	if err := StageImport(context.Background(), ImportOptions{Src: e.cur, DataDir: e.cur}); codeOf(err) != CodeImportSameDir {
		t.Fatalf("same dir: %v", err)
	}
}

func TestImportStageAndApply(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	writeFiles(t, e.cur, map[string]string{"courses/.keep": "", "logs/lectureflow.log": "new install\n"})
	srcBefore := listTree(t, e.src)
	var last Progress
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur, Progress: func(p Progress) { last = p }}); err != nil {
		t.Fatal(err)
	}
	if last.CopiedBytes != last.TotalBytes || last.Phase != PhaseVerifying {
		t.Fatalf("progress %+v", last)
	}
	staging := filepath.Join(e.cur, StagingDir)
	got := listTree(t, staging)
	for _, p := range got {
		if strings.HasPrefix(p, "models") || strings.HasPrefix(p, "backups") || strings.HasPrefix(p, "logs") ||
			strings.HasPrefix(p, "selftest") || strings.HasPrefix(p, "components") {
			t.Errorf("staged %s", p)
		}
	}
	if !exists(filepath.Join(staging, ReadyMarker)) || !exists(filepath.Join(staging, "courses", "C1", "lectures", "L1", "media.mp4")) {
		t.Fatalf("staging %v", got)
	}
	if strings.Join(listTree(t, e.src), "|") != strings.Join(srcBefore, "|") {
		t.Fatal("source changed")
	}

	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	if exists(staging) {
		t.Fatal("staging left")
	}
	rows := readRows(t, filepath.Join(e.cur, DBName))
	media := filepath.Join(e.cur, "courses", "C1", "lectures", "L1", "media.mp4")
	if rows["media"] != media || rows["json"] != `{"audio":"`+jsonEsc(media)+`","n":1}` || rows["slash"] != filepath.ToSlash(media) {
		t.Fatalf("rows %v", rows)
	}
	if !strings.Contains(rows["onboarding"], "welcome") {
		t.Fatalf("onboarding state not carried over: %q", rows["onboarding"])
	}
	if b, err := os.ReadFile(media); err != nil || string(b) != "video bytes" {
		t.Fatalf("media %q %v", b, err)
	}
	// The replaced database and courses/ are in backups/; logs/ is untouched.
	baks, _ := filepath.Glob(filepath.Join(e.cur, "backups", "lectureflow-before-import-*.db"))
	courseBaks, _ := filepath.Glob(filepath.Join(e.cur, "backups", "courses-before-import-*"))
	if len(baks) != 1 || len(courseBaks) != 1 {
		t.Fatalf("backups %v %v", baks, courseBaks)
	}
	if b, _ := os.ReadFile(filepath.Join(e.cur, "logs", "lectureflow.log")); string(b) != "new install\n" {
		t.Fatal("logs changed")
	}
	st, err := db.Open(filepath.Join(e.cur, DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs, err := st.ListCourses(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name != "Cryptography" {
		t.Fatalf("courses %v %v", cs, err)
	}
	// Applying again is a no-op.
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
}

// A step of the switch fails: everything is moved back, READY is removed,
// the next start deletes the staging area.
func TestApplyStagedFailureRestores(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	writeFiles(t, e.cur, map[string]string{"courses/mine.txt": "keep"})
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	curBefore, _ := os.ReadFile(filepath.Join(e.cur, DBName))
	calls := 0
	rename = func(from, to string) error {
		calls++
		if strings.HasPrefix(filepath.Base(to), "courses") && strings.Contains(from, StagingDir) {
			return errors.New("access denied")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { rename = os.Rename })
	err := ApplyStaged(ctx, e.cur)
	var ae *ApplyError
	if !errors.As(err, &ae) || !ae.Restored {
		t.Fatalf("err %v", err)
	}
	if calls < 3 {
		t.Fatalf("only %d renames", calls)
	}
	if b, _ := os.ReadFile(filepath.Join(e.cur, DBName)); string(b) != string(curBefore) {
		t.Fatal("current database not restored")
	}
	if b, _ := os.ReadFile(filepath.Join(e.cur, "courses", "mine.txt")); string(b) != "keep" {
		t.Fatal("current courses not restored")
	}
	staging := filepath.Join(e.cur, StagingDir)
	if !exists(filepath.Join(staging, DBName)) || exists(filepath.Join(staging, ReadyMarker)) {
		t.Fatalf("staging %v", listTree(t, staging))
	}
	rename = os.Rename
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	if exists(staging) {
		t.Fatal("incomplete staging area not removed")
	}
	if rows := readRows(t, filepath.Join(e.cur, DBName)); rows["media"] != "" {
		t.Fatalf("import applied after the failure: %v", rows)
	}
}

func TestApplyStagedRemovesIncompleteStaging(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{StagingDir + "/courses/C1/x.mp4": "half", StagingDir + "/" + DBName: "half"})
	if err := ApplyStaged(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, StagingDir)) || exists(filepath.Join(dir, DBName)) {
		t.Fatalf("left %v", listTree(t, dir))
	}
	// Nothing staged: nothing happens.
	if err := ApplyStaged(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// A failed staging copy removes the staging area.
func TestStageImportFailureRemovesStaging(t *testing.T) {
	e := newImportEnv(t)
	copyFileHook = func(string) error { return errors.New("read error") }
	t.Cleanup(func() { copyFileHook = nil })
	if err := StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != CodeCopyFailed {
		t.Fatalf("err %v", err)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging left")
	}
}

// An older database is imported as is and upgraded by the normal migration.
func TestImportOlderVersionMigrates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "old", "data")
	writeFiles(t, src, fixtureFiles)
	d, err := openDB(filepath.Join(src, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	const oldVersion = 2 // before 003/004: the upgrade adds launched_at and backfills it
	migrateFiles(t, d, oldVersion)
	seed(t, d, src)
	importableFixture(t, d)
	d.Close()
	cur := filepath.Join(root, "new")
	if err := os.MkdirAll(cur, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := CheckImportSource(ctx, src)
	if err != nil || info.Version != oldVersion {
		t.Fatalf("info %+v %v", info, err)
	}
	if err := StageImport(ctx, ImportOptions{Src: src, DataDir: cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, cur); err != nil {
		t.Fatal(err)
	}
	st, err := db.Open(filepath.Join(cur, DBName))
	if err != nil {
		t.Fatal(err)
	}
	ls, err := st.ListLectures(ctx, "C1")
	st.Close()
	if err != nil || len(ls) != 2 {
		t.Fatalf("lectures %v %v", ls, err)
	}
	var l1 model.Lecture
	for _, l := range ls {
		if l.ID == "L1" {
			l1 = l
		}
	}
	if l1.MediaPath != filepath.Join(cur, "courses", "C1", "lectures", "L1", "media.mp4") || l1.LaunchedAt == nil {
		t.Fatalf("lecture %+v", l1)
	}
	d, err = openDB(filepath.Join(cur, DBName), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v int
	d.QueryRow("PRAGMA user_version").Scan(&v)
	sup, _ := SupportedVersion()
	if v != sup {
		t.Fatalf("user_version %d, want %d", v, sup)
	}
}

// TestApplyStagedScrubsColabCredential (v3 Gate round 1 item 9): the
// database an import replaces is kept in backups/, but without the Colab
// credential: no byte of the refresh token or client secret stays in it
// (nor in a WAL next to it); its other rows stay.
func TestApplyStagedScrubsColabCredential(t *testing.T) {
	if !slices.Contains(secretKVKeys, colab.KVCredential) {
		t.Fatalf("secretKVKeys %v lacks %s", secretKVKeys, colab.KVCredential)
	}
	ctx := context.Background()
	e := newImportEnv(t)
	const refresh, secret = "1//test-refresh-token-relocate-4c1d", "test-client-secret-relocate-9e0f"
	d, err := db.OpenDB(filepath.Join(e.cur, DBName))
	if err != nil {
		t.Fatal(err)
	}
	cred := fmt.Sprintf(`{"client_secret":%q,"refresh_token":%q}`, secret, refresh)
	for i := 0; i < 3; i++ { // rewritten: older copies in the file as well
		if _, err := d.Exec("INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			colab.KVCredential, cred+strings.Repeat(" ", i*40)); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	baks, _ := filepath.Glob(filepath.Join(e.cur, "backups", "lectureflow-before-import-*.db"))
	if len(baks) != 1 {
		t.Fatalf("backups %v", baks)
	}
	for _, sfx := range []string{"", "-wal", "-journal"} {
		b, err := os.ReadFile(baks[0] + sfx)
		if err != nil {
			continue
		}
		for _, needle := range []string{refresh, secret} {
			if strings.Contains(string(b), needle) {
				t.Errorf("%s still holds %q", filepath.Base(baks[0]+sfx), needle)
			}
		}
	}
	bd, err := openDB(baks[0], true)
	if err != nil {
		t.Fatal(err)
	}
	defer bd.Close()
	var n int
	if err := bd.QueryRow("SELECT count(*) FROM kv WHERE key = ?", colab.KVCredential).Scan(&n); err != nil || n != 0 {
		t.Errorf("credential rows %d %v", n, err)
	}
	var onboarding string
	if err := bd.QueryRow("SELECT value FROM kv WHERE key = ?", model.KVOnboardingState).Scan(&onboarding); err != nil || !strings.Contains(onboarding, "welcome") {
		t.Errorf("other rows of the backup: %q %v", onboarding, err)
	}
}
