// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package relocate

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// editSource runs statements on the import source's database (foreign
// keys off, as openDB opens it: a prepared database need not keep them).
func editSource(t *testing.T, e *importEnv, stmts ...string) {
	t.Helper()
	d, err := openDB(filepath.Join(e.src, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// sqlQuote is s as an SQL string literal.
func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// wantImportRefused: CheckImportSource and StageImport both refuse the
// source with code, and no staging area is left.
func wantImportRefused(t *testing.T, e *importEnv, code string) {
	t.Helper()
	ctx := context.Background()
	if _, err := CheckImportSource(ctx, e.src); codeOf(err) != code {
		t.Fatalf("check: %v, want %s", err, code)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != code {
		t.Fatalf("stage: %v, want %s", err, code)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging area left after a refusal")
	}
}

// AUD1 M-1: the forms of id the app never writes, the device names and
// the ones it does (ULIDs; the test fixtures' short ids).
func TestRecordIDForm(t *testing.T) {
	for _, s := range []string{
		`..\..\..\..\..\..\Documents`, "../x", "..", ".", `C:\x`, "C:", `\\server\share`, "//server/share",
		"a b", "x.", "x ", "a.b", "a:b", "a/b", `a\b`, "", strings.Repeat("A", 65), "NUL", "con", "COM1", "lpt9",
		"Ünï", "a\x00b",
	} {
		if recordIDOK(s) {
			t.Errorf("%q accepted as a record id", s)
		}
	}
	for _, s := range []string{db.NewID(), "C1", "L1", "M1", "p_1-x", strings.Repeat("A", 64), "COM10", "CONX"} {
		if !recordIDOK(s) {
			t.Errorf("%q refused as a record id", s)
		}
	}
	for _, s := range []string{"", ".", "..", "a/b.pdf", `a\b.pdf`, `C:x.pdf`, `\\server\share\x.pdf`, "x.pdf.", "x.pdf ",
		"NUL", "nul.pdf", "COM¹.txt", "a\tb.pdf", "a:stream"} {
		if plainFileName(s) {
			t.Errorf("%q accepted as a plain file name", s)
		}
	}
	for _, s := range []string{"syllabus.pdf", "Course Outline 2026.pdf", " leading.txt", "中文 名字.txt", "nul-ish.pdf"} {
		if !plainFileName(s) {
			t.Errorf("%q refused as a plain file name", s)
		}
	}
}

// AUD1 M-1 (the auditor's TestAUD1ImportKeepsPathLikeIDs, inverted): an id
// that names a path in any column the app joins into paths refuses the
// whole import; nothing is staged.
func TestImportRefusesPathLikeIDs(t *testing.T) {
	const at = "'2026-01-04T00:00:00Z'"
	bad := []string{`..\..\..\..\..\..\Documents`, "../../x", `C:\Windows`, `\\server\share`, "NUL", "x."}
	for i, tc := range []struct {
		name string
		stmt func(id string) string
	}{
		{"course.id", func(id string) string {
			return "INSERT INTO course (id, name, created_at) VALUES (" + sqlQuote(id) + ", 'X', " + at + ")"
		}},
		{"lecture.id", func(id string) string {
			return "INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES (" + sqlQuote(id) + ", 'C1', 'Week 9', '', " + at + ")"
		}},
		{"lecture.course_id", func(id string) string {
			return "INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('L9', " + sqlQuote(id) + ", 'Week 9', '', " + at + ")"
		}},
		{"material.id", func(id string) string {
			return "INSERT INTO material (id, course_id, filename, path, created_at) VALUES (" + sqlQuote(id) + ", 'C1', 'slides.pdf', '', " + at + ")"
		}},
		{"material.course_id", func(id string) string {
			return "INSERT INTO material (id, course_id, filename, path, created_at) VALUES ('M9', " + sqlQuote(id) + ", 'slides.pdf', '', " + at + ")"
		}},
		{"material.lecture_id", func(id string) string {
			return "INSERT INTO material (id, course_id, lecture_id, filename, path, created_at) VALUES ('M9', 'C1', " + sqlQuote(id) + ", 'slides.pdf', '', " + at + ")"
		}},
		{"syllabus.course_id", func(id string) string {
			return "INSERT INTO syllabus (course_id, filename, path) VALUES (" + sqlQuote(id) + ", 'syllabus.pdf', '')"
		}},
		{"job.id", func(id string) string {
			return "INSERT INTO job (id, scope, scope_id, stage, status, created_at) VALUES (" + sqlQuote(id) + ", 'lecture', 'L1', 'asr', 'pending', " + at + ")"
		}},
		{"job.scope_id", func(id string) string {
			return "INSERT INTO job (id, scope, scope_id, stage, status, created_at) VALUES ('J9', 'material', " + sqlQuote(id) + ", 'material_extract', 'pending', " + at + ")"
		}},
		{"download_task.course_id", func(id string) string {
			return "INSERT INTO download_task (platform, session_id, profile_id, course_id, title, tracks_json, state, created_at, updated_at) VALUES ('generic', 's9', 'generic', " + sqlQuote(id) + ", 'Week 9', '[]', 'queued', " + at + ", " + at + ")"
		}},
		{"download_task.lecture_id", func(id string) string {
			return "INSERT INTO download_task (platform, session_id, profile_id, course_id, title, tracks_json, state, lecture_id, created_at, updated_at) VALUES ('generic', 's9', 'generic', 'C1', 'Week 9', '[]', 'done', " + sqlQuote(id) + ", " + at + ", " + at + ")"
		}},
	} {
		id := bad[i%len(bad)]
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmt(id))
			wantImportRefused(t, e, CodeModified)
		})
	}
}

// What the app writes stays importable: ULIDs everywhere, an optional
// reference left empty, a syllabus with a plain name, OpenVINO and MLX
// devices with the models of the list, a faster-whisper model that is a
// name, a Hugging Face id or a folder on this computer, and asr snapshots
// of each kind.
func TestImportAcceptsOwnRecords(t *testing.T) {
	e := newImportEnv(t)
	c, l, m, j, j2 := db.NewID(), db.NewID(), db.NewID(), db.NewID(), db.NewID()
	const at = "'2026-01-04T00:00:00Z'"
	local := sqlQuote(filepath.Join(e.src, "models", "my-whisper"))
	ov := sqlQuote(model.MarshalSnapshot(model.ASRSnapshot{DeviceID: model.DeviceOVGPU, Backend: model.BackendOpenVINO, Model: "large-v3-turbo", Precision: model.PrecisionINT8, Language: "en"}))
	v11 := sqlQuote(`{"model":"large-v2","compute_type":"float16","device":"cuda","language":"en"}`)
	editSource(t, e,
		"INSERT INTO course (id, name, created_at) VALUES ("+sqlQuote(c)+", 'Networks', "+at+")",
		"INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ("+sqlQuote(l)+", "+sqlQuote(c)+", 'Week 1', '', "+at+")",
		"INSERT INTO material (id, course_id, lecture_id, filename, path, created_at) VALUES ("+sqlQuote(m)+", "+sqlQuote(c)+", NULL, 'Week 1 slides.pdf', '', "+at+")",
		"INSERT INTO syllabus (course_id, filename, path) VALUES ("+sqlQuote(c)+", 'Course Outline.pdf', "+sqlQuote(filepath.Join(e.src, "courses", c, "syllabus", "syllabus.pdf"))+")",
		"INSERT INTO job (id, scope, scope_id, stage, status, config_snapshot, created_at) VALUES ("+sqlQuote(j)+", 'lecture', "+sqlQuote(l)+", 'asr', 'done', "+ov+", "+at+")",
		"INSERT INTO job (id, scope, scope_id, stage, status, config_snapshot, created_at) VALUES ("+sqlQuote(j2)+", 'lecture', 'L1', 'asr', 'done', "+v11+", "+at+")",
		"INSERT INTO download_task (platform, session_id, profile_id, course_id, title, tracks_json, state, lecture_id, created_at, updated_at) VALUES ('generic', 's1', 'generic', "+sqlQuote(c)+", 'Week 1', '[]', 'queued', NULL, "+at+", "+at+")",
		"INSERT INTO download_task (platform, session_id, profile_id, course_id, title, tracks_json, state, lecture_id, created_at, updated_at) VALUES ('generic', 's2', 'generic', "+sqlQuote(c)+", 'Week 1', '[]', 'done', "+sqlQuote(l)+", "+at+", "+at+")",
		"INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('ov:GPU', 'openvino', 'Intel GPU', 'large-v3-turbo', 'int8')",
		"INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('mlx:gpu', 'mlx', 'Apple GPU', 'large-v3', 'fp16')",
		"INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('cuda:0', 'faster-whisper', 'GPU', 'Systran/faster-whisper-large-v3', 'auto')",
		"INSERT INTO asr_device (id, backend, label, model, precision) VALUES ('fw:cpu', 'faster-whisper', 'CPU', "+local+", 'auto')",
		"INSERT INTO kv (key, value) VALUES ('"+model.KVSettingsASR+"', '{\"model\":\"large-v2\",\"device\":\"cuda\"}')",
	)
	ctx := context.Background()
	if _, err := CheckImportSource(ctx, e.src); err != nil {
		t.Fatal(err)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
}

// The other path parts: a syllabus' file name, an OpenVINO or MLX model or
// precision (in the device table or an asr snapshot) refuse the import as
// not LectureFlow's; a faster-whisper model on another computer as a
// network path.
func TestImportRefusesPathLikeValues(t *testing.T) {
	snap := func(s model.ASRSnapshot) string { return sqlQuote(model.MarshalSnapshot(s)) }
	job := func(snapshot string) string {
		return "INSERT INTO job (id, scope, scope_id, stage, status, config_snapshot, created_at) VALUES ('J9', 'lecture', 'L1', 'asr', 'queued', " + snapshot + ", '2026-01-04T00:00:00Z')"
	}
	device := func(id, backend, mdl, precision string) string {
		return "INSERT INTO asr_device (id, backend, label, model, precision) VALUES (" + sqlQuote(id) + ", " + sqlQuote(backend) + ", 'x', " + sqlQuote(mdl) + ", " + sqlQuote(precision) + ")"
	}
	for _, tc := range []struct {
		name, stmt, code string
	}{
		{"syllabus.filename", "INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', '..\\..\\..\\secret.txt', '')", CodeModified},
		{"openvino model", device("ov:GPU", "openvino", `..\..\..\..\x`, "int8"), CodeModified},
		{"openvino precision", device("ov:NPU", "openvino", "large-v3", "int8/../../x"), CodeModified},
		{"mlx model", device("mlx:gpu", "mlx", "../../../x", "fp16"), CodeModified},
		{"openvino snapshot", job(snap(model.ASRSnapshot{DeviceID: model.DeviceOVGPU, Model: `..\..\..\x`, Precision: "int8", Language: "en"})), CodeModified},
		{"mlx snapshot", job(snap(model.ASRSnapshot{DeviceID: model.DeviceMLXGPU, Backend: model.BackendMLX, Model: "large-v3", Precision: `..\x`, Language: "en"})), CodeModified},
		{"faster-whisper device on a share", device("cuda:0", "faster-whisper", `\\server\share\whisper`, "auto"), codeImportNetworkPath},
		{"faster-whisper snapshot on a share", job(snap(model.ASRSnapshot{DeviceID: model.DeviceCUDA0, Model: "//server/share/whisper", Language: "en"})), codeImportNetworkPath},
		{"legacy settings on a share", "INSERT INTO kv (key, value) VALUES ('" + model.KVSettingsASR + "', '{\"model\":\"\\\\\\\\server\\\\share\\\\m\"}')", codeImportNetworkPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmt)
			wantImportRefused(t, e, tc.code)
		})
	}
}

// The staged copy is checked again: an id that appears in the copy only
// (the source changed after CheckImportSource) still refuses the import.
func TestStageImportChecksStagedRecords(t *testing.T) {
	e := newImportEnv(t)
	afterRewrite = func(d *sql.DB) {
		if _, err := d.Exec(`UPDATE lecture SET id = '..\..\..\x' WHERE id = 'L2'`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterRewrite = nil })
	if err := StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != CodeModified {
		t.Fatalf("stage: %v", err)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging area left after a refusal")
	}
}

// SEC44 medium-2: a stored file path (lecture.media_path, material.path,
// syllabus.path) must lie inside its own record's directory of the data
// directory; anything else refuses the whole import — the app would read
// the file (and send its text to the library's providers) after the
// restart, through a queued job of the library.
func TestImportRefusesStoredPathsOutside(t *testing.T) {
	outsideRoot := t.TempDir() // a directory of this computer outside the import
	outside := filepath.Join(outsideRoot, "Documents", "sec44-marker.pdf")
	// The long-path prefix in front of the data directory: on Windows that
	// is \\?\C:\…, a path on a local drive (media.UnsafeStoredPath is
	// false), so the record's own place decides: outside. On macOS and
	// Linux the data directory has no drive letter and the path is \\?\/…,
	// another device path (media.UnsafeStoredPath true, media.UnderRoot
	// false: rootedPath trusts \\?\ only before UNC\ or a drive), refused
	// as a path on another computer, which is reported before the paths
	// outside. Refused on both; each pins its own code.
	longPathCode := codeImportNetworkPath
	if runtime.GOOS == "windows" {
		longPathCode = CodeOutsidePath
	}
	for _, tc := range []struct {
		name string
		stmt func(src string) string
		code string
	}{
		{"media .. relative", func(string) string {
			return `UPDATE lecture SET media_path = '..\..\..\..\..\..\Documents\sec44-marker.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media ../ relative", func(string) string {
			return `UPDATE lecture SET media_path = '../../../../sec44-marker.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media plain relative", func(string) string {
			return `UPDATE lecture SET media_path = 'lectureflow.db' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media absolute outside", func(string) string {
			return "UPDATE lecture SET media_path = " + sqlQuote(outside) + " WHERE id = 'L1'"
		}, CodeOutsidePath},
		{"media in the data directory, outside its lecture", func(src string) string {
			return "UPDATE lecture SET media_path = " + sqlQuote(filepath.Join(src, DBName)) + " WHERE id = 'L1'"
		}, CodeOutsidePath},
		{"media in another lecture's directory", func(src string) string {
			return "UPDATE lecture SET media_path = " + sqlQuote(filepath.Join(src, "courses", "C1", "lectures", "L1", "media.mp4")) + " WHERE id = 'L2'"
		}, CodeOutsidePath},
		{"media with .. inside", func(src string) string {
			return "UPDATE lecture SET media_path = " + sqlQuote(filepath.Join(src, "courses", "C1", "lectures", "L1")+string(filepath.Separator)+".."+string(filepath.Separator)+"L1"+string(filepath.Separator)+"media.mp4") + " WHERE id = 'L1'"
		}, CodeOutsidePath},
		{"media long-path prefix, not its own place", func(src string) string {
			return "UPDATE lecture SET media_path = " + sqlQuote(`\\?\`+filepath.Join(src, "courses", "C1", "lectures", "L1", "media.mp4")) + " WHERE id = 'L2'"
		}, longPathCode},
		{"media device path", func(string) string { // a device path is what import_paths.go calls a network path
			return `UPDATE lecture SET media_path = '\\.\C:\sec44-marker.mp4' WHERE id = 'L1'`
		}, codeImportNetworkPath},
		{"media drive-relative", func(string) string {
			return `UPDATE lecture SET media_path = 'C:sec44-marker.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media on another computer", func(string) string {
			return `UPDATE lecture SET media_path = '\\server\share\sec44-marker.mp4' WHERE id = 'L1'`
		}, codeImportNetworkPath},
		{"material .. relative", func(string) string {
			return `UPDATE material SET path = '..\..\..\..\..\..\Documents\sec44-marker.pdf' WHERE id = 'M1'`
		}, CodeOutsidePath},
		{"material absolute outside", func(string) string {
			return "UPDATE material SET path = " + sqlQuote(outside) + " WHERE id = 'M1'"
		}, CodeOutsidePath},
		{"material the data key", func(src string) string {
			return "UPDATE material SET path = " + sqlQuote(filepath.Join(src, "secrets.key")) + " WHERE id = 'M1'"
		}, CodeOutsidePath},
		{"syllabus absolute outside", func(string) string {
			return "INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'syllabus.pdf', " + sqlQuote(outside) + ")"
		}, CodeOutsidePath},
		{"syllabus .. relative", func(string) string {
			return `INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'syllabus.pdf', '..\..\..\sec44-marker.pdf')`
		}, CodeOutsidePath},
		{"syllabus in another course", func(src string) string {
			return "INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'syllabus.pdf', " + sqlQuote(filepath.Join(src, "courses", "C2", "syllabus", "syllabus.pdf")) + ")"
		}, CodeOutsidePath},
		{"syllabus by file name only", func(string) string {
			// the stage would look the name up in the working directory first
			return "INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'lectureflow.db', '')"
		}, CodeModified},
		// the other place of a moved library's record is not the record's own
		{"media .. into its own place", func(src string) string {
			return `UPDATE lecture SET media_path = 'X:\old\courses\C1\lectures\L1\..\L1\media.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media of a moved library in another lecture's place", func(string) string {
			return `UPDATE lecture SET media_path = 'X:\old\courses\C1\lectures\L1\media.mp4' WHERE id = 'L2'`
		}, CodeOutsidePath},
		{"media of a moved library in another course's place", func(string) string {
			return `UPDATE lecture SET media_path = 'X:\old\courses\C2\lectures\L1\media.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media of a moved library, a folder deeper", func(string) string {
			return `UPDATE lecture SET media_path = 'X:\old\courses\C1\lectures\L1\sub\media.mp4' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"media of a moved library, a stream name", func(string) string {
			return `UPDATE lecture SET media_path = 'X:\old\courses\C1\lectures\L1\media.mp4:x' WHERE id = 'L1'`
		}, CodeOutsidePath},
		{"material in its lecture's place", func(string) string {
			return `UPDATE material SET path = 'X:\old\courses\C1\lectures\M1\source.pdf' WHERE id = 'M1'`
		}, CodeOutsidePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmt(e.src))
			wantImportRefused(t, e, tc.code)
		})
	}
}

// What the app writes stays importable: a stored path inside its record's
// directory (rewritten to the current data directory), a syllabus in the
// course's syllabus folder or the course folder, no media yet.
func TestImportAcceptsStoredPathsInside(t *testing.T) {
	e := newImportEnv(t)
	course := filepath.Join(e.src, "courses", "C1")
	editSource(t, e,
		"INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'Course Outline.pdf', "+sqlQuote(filepath.Join(course, "syllabus", "syllabus.pdf"))+")",
		"INSERT INTO course (id, name, created_at) VALUES ('C2', 'Other', '2026-01-01T00:00:00Z')",
		"INSERT INTO syllabus (course_id, filename, path) VALUES ('C2', 'notes.txt', "+sqlQuote(filepath.Join(e.src, "courses", "C2", "syllabus.raw.txt"))+")",
		"UPDATE lecture SET media_path = '' WHERE id = 'L2'",
	)
	ctx := context.Background()
	if _, err := CheckImportSource(ctx, e.src); err != nil {
		t.Fatal(err)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if got := readRows(t, filepath.Join(e.cur, StagingDir, DBName))["pdf"]; got != filepath.Join(e.cur, "courses", "C1", "materials", "M1", "source.pdf") {
		t.Fatalf("material path %q", got)
	}
}

// storedPaths reads the three stored paths of the fixture's records (L1,
// M1, C1's syllabus) from the database at path.
func storedPaths(t *testing.T, path string) [3]string {
	t.Helper()
	d, err := openDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var out [3]string
	for i, q := range []string{
		"SELECT media_path FROM lecture WHERE id = 'L1'",
		"SELECT path FROM material WHERE id = 'M1'",
		"SELECT path FROM syllabus WHERE course_id = 'C1'",
	} {
		if err := d.QueryRow(q).Scan(&out[i]); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return out
}

// wantImported: the import of e succeeds, and afterwards the fixture's
// three stored paths are the files in their records' directories under
// the current data directory (the syllabus in the course's syllabus
// folder) — the same result as importing the library where it was written.
func wantImported(t *testing.T, e *importEnv) {
	t.Helper()
	ctx := context.Background()
	if _, err := CheckImportSource(ctx, e.src); err != nil {
		t.Fatalf("check: %v", err)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	want := [3]string{
		filepath.Join(e.cur, "courses", "C1", "lectures", "L1", "media.mp4"),
		filepath.Join(e.cur, "courses", "C1", "materials", "M1", "source.pdf"),
		filepath.Join(e.cur, "courses", "C1", "syllabus", "syllabus.pdf"),
	}
	if got := storedPaths(t, filepath.Join(e.cur, DBName)); got != want {
		t.Fatalf("stored paths after the import:\n got %q\nwant %q", got, want)
	}
}

// SEC44b medium-1: a library copied to another folder, or written on
// another computer, still imports. Its stored paths name the folder it
// was written in; a path that is that folder plus the record's own place
// (courses\{course}\lectures\{lecture}\{file}, …\materials\{material}\
// {file}, courses\{course}[\syllabus]\{file}, worked out from the
// record's ids) is the record's file, now in the current data directory.
func TestImportMovedLibrary(t *testing.T) {
	// the same computer, another folder: the whole data directory moved
	root := t.TempDir()
	old := filepath.Join(root, "old", "data")
	f := newFixture(t, old)
	importableFixture(t, f.db)
	if _, err := f.db.Exec("INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'syllabus.pdf', ?)",
		filepath.Join(old, "courses", "C1", "syllabus", "syllabus.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "backup drive", "LectureFlow")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, moved); err != nil {
		t.Fatal(err)
	}
	e := &importEnv{src: moved, cur: filepath.Join(root, "AppData", "LectureFlow")}
	if err := os.MkdirAll(e.cur, 0o755); err != nil {
		t.Fatal(err)
	}
	wantImported(t, e)
}

// The same with the folder of another computer, in every spelling a
// stored path may have: another drive and user folder, macOS and Linux,
// a share, a long-path prefix, another case, a trailing separator, the
// current data directory itself, its parent, a sibling sharing its name.
func TestImportLibraryOfAnotherComputer(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(cur string) string // the folder the library was written in
		sep  string
	}{
		{"another drive and user", func(string) string { return `D:\Users\bob\AppData\Roaming\LectureFlow` }, `\`},
		{"macOS", func(string) string { return "/Users/bob/Library/Application Support/LectureFlow" }, "/"},
		{"Linux", func(string) string { return "/home/bob/.config/LectureFlow" }, "/"},
		{"share", func(string) string { return `\\nas\share\LectureFlow` }, `\`},
		{"long-path prefix", func(string) string { return `\\?\E:\LectureFlow` }, `\`},
		{"forward slashes", func(string) string { return "E:/LectureFlow" }, "/"},
		{"another case", func(string) string { return `D:\USERS\BOB\LECTUREFLOW` }, `\`},
		{"trailing separator", func(string) string { return `D:\LectureFlow\` }, `\`},
		{"the current data directory", func(cur string) string { return cur }, string(filepath.Separator)},
		{"the current directory's parent", func(cur string) string { return filepath.Dir(cur) }, string(filepath.Separator)},
		{"a sibling with a longer name", func(cur string) string { return cur + "2" }, string(filepath.Separator)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			dir := strings.TrimSuffix(tc.dir(e.cur), tc.sep)
			at := func(parts ...string) string { return sqlQuote(dir + tc.sep + strings.Join(parts, tc.sep)) }
			if tc.name == "trailing separator" {
				at = func(parts ...string) string { return sqlQuote(dir + tc.sep + strings.Join(parts, tc.sep) + tc.sep) }
			}
			editSource(t, e,
				"UPDATE lecture SET media_path = "+at("courses", "C1", "lectures", "L1", "media.mp4")+" WHERE id = 'L1'",
				"UPDATE material SET path = "+at("courses", "C1", "materials", "M1", "source.pdf")+" WHERE id = 'M1'",
				"INSERT INTO syllabus (course_id, filename, path) VALUES ('C1', 'syllabus.pdf', "+at("courses", "C1", "syllabus", "syllabus.pdf")+")",
			)
			wantImported(t, e)
		})
	}
}

// A moved library with one path that really points elsewhere is refused
// as a whole, with the code that says so (and how many); two records
// naming each other's places are both refused.
func TestImportMovedLibraryWithAnOutsidePath(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "Documents", "sec44-marker.pdf")
	for _, tc := range []struct {
		name  string
		stmts []string
		count int
	}{
		{"one outside", []string{
			`UPDATE lecture SET media_path = 'D:\old\courses\C1\lectures\L1\media.mp4' WHERE id = 'L1'`,
			"UPDATE material SET path = " + sqlQuote(outside) + " WHERE id = 'M1'",
		}, 1},
		{"each other's places", []string{
			`UPDATE lecture SET media_path = 'D:\old\courses\C1\lectures\L2\media.mp4' WHERE id = 'L1'`,
			`UPDATE lecture SET media_path = 'D:\old\courses\C1\lectures\L1\media.mp4' WHERE id = 'L2'`,
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmts...)
			wantImportRefused(t, e, CodeOutsidePath)
			_, err := CheckImportSource(context.Background(), e.src)
			var ce *model.CodedError
			if !errors.As(err, &ce) || ce.Info.Params["count"] != tc.count {
				t.Fatalf("params of %v", err)
			}
		})
	}
}

// An imported term whose English or Chinese is longer than the glossary's
// limit (100 characters each, as ctxbuild's) is dropped, row by row: the
// rest of the library, and of the glossary, is imported.
func TestImportDropsOverlongTerms(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	term := func(id, en, zh string) string {
		return "INSERT INTO term (id, course_id, en, en_key, zh, source) VALUES (" + sqlQuote(id) + ", 'C1', " + sqlQuote(en) + ", " + sqlQuote(strings.ToLower(en)) + ", " + sqlQuote(zh) + ", 'manual')"
	}
	editSource(t, e,
		term("T1", "Diffie-Hellman key exchange", "迪菲-赫尔曼密钥交换"),
		term("T2", strings.Repeat("a", 100), strings.Repeat("密", 100)), // at the limit: kept
		term("T3", strings.Repeat("b", 101), "长"),                      // English too long
		term("T4", "ok", strings.Repeat("长", 101)),                     // Chinese too long
		// at the limit once trimmed (ctxbuild.TermTooLong trims): kept
		term("T5", " "+strings.Repeat("c", 100)+"  ", strings.Repeat("密", 100)+"\n"),
	)
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	d, err := openDB(filepath.Join(e.cur, DBName), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var ids []string
	rows, err := d.Query("SELECT id FROM term ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if strings.Join(ids, ",") != "T1,T2,T5" {
		t.Fatalf("terms after the import: %v", ids)
	}
}

// sec44Bad is the reviewer's marker id (SEC44): a path-like id, harmless.
const sec44Bad = `..\..\..\sec44-marker`

// SEC44 medium-1: the five ways around the first id check. Each database
// is refused as a whole, by CheckImportSource and by StageImport.
func TestImportRefusesSchemaTricks(t *testing.T) {
	const at = "'2026-01-04T00:00:00Z'"
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		// a trigger the import's own writes (the dropped kv row) would fire
		// after the check, and that would stay in the database
		{"trigger", []string{
			"INSERT INTO kv (key, value) VALUES ('sec44.unlisted', 'x')",
			"CREATE TRIGGER sec44_t AFTER DELETE ON kv BEGIN INSERT OR IGNORE INTO material (id, course_id, filename, path, created_at) VALUES ('" + sec44Bad + "', 'C1', 'x.pdf', '', " + at + "); END",
		}},
		// a view in place of a table
		{"view as table", []string{
			"DROP TABLE material",
			"CREATE VIEW material AS SELECT '" + sec44Bad + "' AS id, 'C1' AS course_id, NULL AS lecture_id, 'x.pdf' AS filename, '' AS path, NULL AS pages_md, " + at + " AS created_at, NULL AS bind_week",
		}},
		// a table name in another case (SQLite resolves material to it)
		{"table name case", []string{
			"ALTER TABLE material RENAME TO sec44_tmp",
			"ALTER TABLE sec44_tmp RENAME TO Material",
			"INSERT INTO Material (id, course_id, filename, path, created_at) VALUES ('" + sec44Bad + "', 'C1', 'x.pdf', '', " + at + ")",
		}},
		// a column name in another case
		{"column name case", []string{
			"ALTER TABLE lecture RENAME COLUMN id TO sec44_tmp",
			"ALTER TABLE lecture RENAME COLUMN sec44_tmp TO ID",
			"INSERT INTO lecture (ID, course_id, title, media_path, created_at) VALUES ('" + sec44Bad + "', 'C1', 'Week 9', '', " + at + ")",
		}},
		// a generated column (pragma_table_info does not list it)
		{"generated column", []string{
			"DROP TABLE syllabus",
			"CREATE TABLE syllabus (sec44_k TEXT PRIMARY KEY, filename TEXT NOT NULL, path TEXT NOT NULL, text TEXT, course_id TEXT GENERATED ALWAYS AS ('" + sec44Bad + "') VIRTUAL)",
			"INSERT INTO syllabus (sec44_k, filename, path) VALUES ('k', 'syllabus.pdf', '')",
		}},
		// a generated column with one of the app's column names, nothing else
		{"generated app column", []string{
			"DROP TABLE syllabus",
			"CREATE TABLE syllabus (course_id TEXT PRIMARY KEY, filename TEXT NOT NULL, path TEXT NOT NULL, text TEXT GENERATED ALWAYS AS ('sec44 marker') VIRTUAL)",
		}},
		// more of the same family: a table or column the app does not
		// have, a virtual table
		// SEC44b information: a table without rowid, a column default the
		// app's schema does not have
		{"without rowid", []string{
			"DROP TABLE kv",
			"CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID",
		}},
		{"column default", []string{
			"DROP TABLE kv",
			"CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '" + sec44Bad + "')",
		}},
		{"unknown table", []string{"CREATE TABLE sec44_extra (x TEXT)"}},
		{"unknown column", []string{"ALTER TABLE course ADD COLUMN sec44_extra TEXT"}},
		{"virtual table", []string{"CREATE VIRTUAL TABLE sec44_v USING fts5(x)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmts...)
			wantImportRefused(t, e, CodeModified)
		})
	}
}

// wantShownPath: the {path} parameter of err is a cleaned excerpt of a
// stored value that started with prefix: no control, format (Cf: the bidi
// controls, zero-width characters, the BOM, tags) or line and paragraph
// separator character, at most 200 characters and "…".
func wantShownPath(t *testing.T, err error, prefix string) {
	t.Helper()
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("not coded: %v", err)
	}
	p, _ := ce.Info.Params["path"].(string)
	for _, r := range p {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Errorf("path %+q keeps U+%04X", p, r)
		}
	}
	if n := utf8.RuneCountInString(p); n > 201 || !strings.HasSuffix(p, "…") || n < 100 {
		t.Errorf("path of %d characters: %q", n, p)
	}
	if !strings.HasPrefix(p, prefix) {
		t.Errorf("path %q does not start with %q", p, prefix)
	}
}

// SEC44c c-info-1: the {path} of data_import.outside_path and network_path
// echoes a value of the imported database; it is cleaned before it goes
// into the message (line breaks, U+202E and the like removed, cut after
// 200 characters), so it cannot stretch or disguise the notice.
func TestImportRefusalShowsACleanPath(t *testing.T) {
	// SEC49 I1: the invisible and format characters too \u2014 U+200B\u2013U+200F
	// (ZWJ and ZWNJ as well: in an echoed path they only hide), U+2028 /
	// U+2029, the BOM, U+061C, U+2061, a tag character, U+FFF9
	invisible := string([]rune{0x200b, 0x200c, 0x200d, 0x2028, 0x2029, 0xfeff, 0x061c, 0x2061, 0xe0041, 0xfff9})
	tail := "a\nb\r\tc\x1b[31m\u202eexe.\u2066mp4" + invisible + strings.Repeat("x", 5000)
	outside := filepath.Join(t.TempDir(), "sec44c")
	for _, tc := range []struct {
		name, path, code, prefix string
	}{
		{"outside", outside + string(filepath.Separator) + tail, CodeOutsidePath, outside + string(filepath.Separator) + "abc[31mexe.mp4"},
		{"network", `\\server\share\` + tail, codeImportNetworkPath, `\\server\share\abc[31mexe.mp4`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, "UPDATE lecture SET media_path = "+sqlQuote(tc.path)+" WHERE id = 'L1'")
			wantImportRefused(t, e, tc.code)
			_, err := CheckImportSource(context.Background(), e.src)
			wantShownPath(t, err, tc.prefix)
			err = StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur})
			wantShownPath(t, err, tc.prefix)
		})
	}
	if got := ShownPath(`C:\data\courses\C1\media.mp4`); got != `C:\data\courses\C1\media.mp4` {
		t.Errorf("a plain path changed: %q", got)
	}
	if got := ShownPath(strings.Repeat("é", 200)); got != strings.Repeat("é", 200) {
		t.Errorf("200 characters are cut: %q", got)
	}
}

// SEC44c c-low-2: after relocateStoredPaths the staged copy is checked
// strictly — a path must lie in its record's directory under the current
// data directory; another folder followed by the record's own place (what
// the relocation turns into the current one) is no longer enough. A path
// of that form that appears after the import's last write (as if the
// relocation had missed a column) refuses the import.
func TestStageImportChecksStrictlyAfterRelocation(t *testing.T) {
	e := newImportEnv(t)
	elsewhere := filepath.Join(t.TempDir(), "another folder")
	moved := filepath.Join(elsewhere, "courses", "C1", "lectures", "L1", "media.mp4")
	afterImportWrites = func(d *sql.DB) {
		if _, err := d.Exec("UPDATE lecture SET media_path = ? WHERE id = 'L1'", moved); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterImportWrites = nil })
	if err := StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != CodeOutsidePath {
		t.Fatalf("stage: %v, want %s", err, CodeOutsidePath)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging area left after a refusal")
	}
}

// SEC44c c-low-1: two records of one kind whose ids differ only in case
// (course C1 and c1) are two rows to the database but one directory on a
// file system that ignores case (NTFS, APFS): deleting one would delete
// the other's files, a syllabus uploaded for c1 would replace C1's. Such a
// library is refused as a whole, by CheckImportSource and StageImport.
func TestImportRefusesIDsDifferingInCase(t *testing.T) {
	const at = "'2026-01-04T00:00:00Z'"
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"course.id", "INSERT INTO course (id, name, created_at) VALUES ('c1', 'Other', " + at + ")"},
		{"lecture.id", "INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('l1', 'C1', 'Week 9', '', " + at + ")"},
		{"material.id", "INSERT INTO material (id, course_id, filename, path, created_at) VALUES ('m1', 'C1', 'slides.pdf', '', " + at + ")"},
		{"job.id", "INSERT INTO job (id, scope, scope_id, stage, status, created_at) VALUES ('J1', 'lecture', 'L1', 'asr', 'done', " + at + "), ('j1', 'lecture', 'L2', 'asr', 'done', " + at + ")"},
		{"syllabus.course_id", "INSERT INTO syllabus (course_id, filename, path, text) VALUES ('C1', '', '', 'pasted'), ('c1', '', '', 'pasted')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			editSource(t, e, tc.stmt)
			wantImportRefused(t, e, CodeModified)
		})
	}
}

// The last check of the staged copy looks for them too: a case twin that
// appears only after the import's last write still refuses it.
func TestStageImportChecksCaseTwinsAfterLastWrite(t *testing.T) {
	e := newImportEnv(t)
	afterImportWrites = func(d *sql.DB) {
		if _, err := d.Exec("INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('l2', 'C1', 'Week 9', '', '2026-01-04T00:00:00Z')"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterImportWrites = nil })
	if err := StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != CodeModified {
		t.Fatalf("stage: %v", err)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging area left after a refusal")
	}
}

// Ids that differ only in case but name records of different kinds live
// in different directories (courses\C1, …\lectures\c1, …\materials\C1):
// still importable, as are the app's own ULIDs (upper case only).
func TestImportAcceptsCaseTwinsOfOtherKinds(t *testing.T) {
	e := newImportEnv(t)
	const at = "'2026-01-04T00:00:00Z'"
	editSource(t, e,
		"INSERT INTO lecture (id, course_id, title, media_path, created_at) VALUES ('c1', 'C1', 'Week 9', '', "+at+")",
		"INSERT INTO material (id, course_id, filename, path, created_at) VALUES ('l1', 'C1', 'slides.pdf', '', "+at+")",
		"INSERT INTO job (id, scope, scope_id, stage, status, created_at) VALUES ('m1', 'lecture', 'L1', 'asr', 'done', "+at+")",
		"INSERT INTO course (id, name, created_at) VALUES ("+sqlQuote(db.NewID())+", 'Networks', "+at+")",
	)
	ctx := context.Background()
	if _, err := CheckImportSource(ctx, e.src); err != nil {
		t.Fatal(err)
	}
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
}

// The app's own schema passes at every version: the migrations create no
// view, trigger, virtual table or generated column, and an older version
// has no table or column the current one lacks.
func TestImportSchemaOfEveryVersion(t *testing.T) {
	ctx := context.Background()
	sup, err := SupportedVersion()
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= sup; v++ {
		d, err := openDB(filepath.Join(t.TempDir(), DBName), false)
		if err != nil {
			t.Fatal(err)
		}
		migrateFiles(t, d, v)
		if err := checkImportSchema(ctx, d, "dir"); err != nil {
			t.Errorf("version %d: %v", v, err)
		}
		d.Close()
	}
}

// The staged copy is checked again after the import's last write: a bad
// row that appears only then still refuses the import.
func TestStageImportChecksAfterLastWrite(t *testing.T) {
	e := newImportEnv(t)
	afterImportWrites = func(d *sql.DB) {
		if _, err := d.Exec("INSERT INTO material (id, course_id, filename, path, created_at) VALUES (?, 'C1', 'x.pdf', '', '2026-01-04T00:00:00Z')", sec44Bad); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterImportWrites = nil })
	if err := StageImport(context.Background(), ImportOptions{Src: e.src, DataDir: e.cur}); codeOf(err) != CodeModified {
		t.Fatalf("stage: %v", err)
	}
	if exists(filepath.Join(e.cur, StagingDir)) {
		t.Fatal("staging area left after a refusal")
	}
}

// AUD1 L-1: an import keeps the imported kv rows importedKV lists and
// drops the rest — app.old_data_dir (the auditor's
// TestAUD1ImportCarriesOldDataDir: the next start then scrubbed the
// database of the directory it named) among them; this installation's
// app.* and onboarding.* rows are taken over as before.
func TestImportKeepsOnlyListedKV(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	victim := filepath.Join(filepath.Dir(filepath.Dir(e.cur)), "elsewhere", "data")
	kept := map[string]string{
		model.KVSettingsGeneral:                           `{"daily_budget":3}`,
		model.KVUILocale:                                  "en",
		model.KVChaptersPrefix + "L1":                     `{"chapters":[]}`,
		model.KVProviderModelsPref + "p1":                 `{"models":["m"]}`,
		model.KVStaleReasonJSON:                           "1",
		model.KVASRDevicePromptDismissedPrefix + "cuda:0": "1",
		"remote.ssh.redact_history":                       "[]",
		"v1_1.usage_migrated":                             "1",
	}
	dropped := map[string]string{
		KVOldDataDir:             victim,
		KVOldDataDirScrubbed:     victim,
		model.KVAppCloseBehavior: "background",
		model.KVOnboardingState:  `{"step":"done","done":true,"data":{}}`,
		"upgrade.backup":         "15|lectureflow-before-upgrade.db",
		"selfupdate.sources":     `["https://updates.example.invalid/lf"]`,
		"components.sources":     `{"pypi":"https://pypi.example.invalid/simple"}`,
		"colab.session":          `{"id":"s1"}`,
		"secrets.scrub_pending":  "1",
		"hardware.last":          `{}`,
		model.KVVRAMObservedPrefix + "large-v3:float16": "5000",
		"some.unknown_key": "x",
	}
	var stmts []string
	for k, v := range kept {
		stmts = append(stmts, "INSERT INTO kv (key, value) VALUES ("+sqlQuote(k)+", "+sqlQuote(v)+")")
	}
	for k, v := range dropped {
		stmts = append(stmts, "INSERT INTO kv (key, value) VALUES ("+sqlQuote(k)+", "+sqlQuote(v)+")")
	}
	editSource(t, e, stmts...)
	cd, err := openDB(filepath.Join(e.cur, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cd.Exec("INSERT INTO kv (key, value) VALUES (?, 'quit')", model.KVAppCloseBehavior); err != nil {
		t.Fatal(err)
	}
	cd.Close()

	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	d, err := openDB(filepath.Join(e.cur, DBName), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query("SELECT key, value FROM kv")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	rows.Close()
	for k, v := range kept {
		if got[k] != v {
			t.Errorf("kept row %s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{kvFixtureJSON, kvFixtureSlash, kvFixtureProse} {
		if _, ok := got[k]; !ok {
			t.Errorf("fixture row %s not imported", k)
		}
	}
	for k := range dropped {
		switch k {
		case model.KVAppCloseBehavior:
			if got[k] != "quit" {
				t.Errorf("%s = %q, want this installation's quit", k, got[k])
			}
		case model.KVOnboardingState:
			if !strings.Contains(got[k], "welcome") {
				t.Errorf("%s = %q, want this installation's state", k, got[k])
			}
		default:
			if v, ok := got[k]; ok {
				t.Errorf("row %s = %q imported", k, v)
			}
		}
	}
}

// The kv keys of the code are found three ways (SEC44 low-3):
//   - stringConstRe: every constant or variable given a string literal (or
//     literals joined with +), typed or not; those whose name starts with
//     KV or kv are kv keys;
//   - kvCallRe: the key argument of every GetKV / SetKV / DeleteKV call: a
//     literal is a kv key, an identifier (package-qualified or not) names
//     a constant found by stringConstRe whose value is one.
var (
	stringConstRe = regexp.MustCompile(`(?m)^\s*(?:const\s+|var\s+)?([A-Za-z_]\w*)(?:\s+[A-Za-z_][\w.]*)?\s*=\s*("[^"\n]*"(?:\s*\+\s*"[^"\n]*")*)`)
	kvCallRe      = regexp.MustCompile(`\b(?:GetKV|SetKV|DeleteKV)\(\s*[^,\n]*,\s*(?:"([^"\n]*)"|(?:[A-Za-z_]\w*\.)?([A-Za-z_]\w*))`)
)

// Every kv key the code defines is, on purpose, either taken from the
// imported database, taken from this installation's, or kept by neither,
// and the three lists do not overlap: a new key gets a decision.
func TestImportedKVCoversEveryKey(t *testing.T) {
	dropped := func(k string) bool { return slices.Contains(droppedKVKeys, k) }
	classes := func(k string) []string {
		var c []string
		if importedKV(k) {
			c = append(c, "imported")
		}
		if installKV(k) {
			c = append(c, "installation")
		}
		if dropped(k) {
			c = append(c, "dropped")
		}
		return c
	}
	lit := regexp.MustCompile(`"([^"\n]*)"`)
	keys := map[string]string{}       // key → file
	consts := map[string][]string{}   // constant name → values (any package)
	callIdents := map[string]string{} // identifier used as a kv key → file
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join("..", "..", top), func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if de.IsDir() {
				if de.Name() == "testdata" || de.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			file := filepath.ToSlash(p)
			for _, m := range stringConstRe.FindAllStringSubmatch(string(b), -1) {
				var k strings.Builder
				for _, part := range lit.FindAllStringSubmatch(m[2], -1) {
					k.WriteString(part[1])
				}
				consts[m[1]] = append(consts[m[1]], k.String())
				if strings.HasPrefix(m[1], "KV") || strings.HasPrefix(m[1], "kv") {
					keys[k.String()] = file
				}
			}
			for _, m := range kvCallRe.FindAllStringSubmatch(string(b), -1) {
				if m[2] == "" {
					keys[m[1]] = file // a literal key
				} else {
					callIdents[m[2]] = file
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for id, file := range callIdents {
		for _, v := range consts[id] {
			if _, ok := keys[v]; !ok {
				keys[v] = file + " (" + id + ")"
			}
		}
	}
	if len(keys) < 60 { // 74 distinct keys on 2026-11-15
		t.Fatalf("found only %d kv keys: the scan is broken (%v)", len(keys), keys)
	}
	for k, file := range keys {
		switch c := classes(k); len(c) {
		case 0:
			t.Errorf("kv key %q (%s): decide whether an import takes it from the imported database (importedKVKeys / importedKVPrefixes), from this installation's (installKVKeys / installKVPrefixes) or from neither (droppedKVKeys)", k, file)
		case 1:
		default:
			t.Errorf("kv key %q (%s) is in more than one list: %v", k, file, c)
		}
	}
	var listed []string
	for _, l := range [][]string{importedKVKeys, importedKVPrefixes, installKVKeys, installKVPrefixes, droppedKVKeys} {
		listed = append(listed, l...)
	}
	for _, k := range listed {
		if c := classes(k); len(c) != 1 {
			t.Errorf("listed %q is in more than one list: %v", k, c)
		}
	}
	t.Logf("%d kv keys classified", len(keys))
}

// SEC44 low-1: the installation's own kv rows (update and download
// settings, hardware, key bookkeeping) stay as they were in this
// installation's database: neither lost nor replaced by the imported
// database's; the backup record of an upgrade is kept by neither.
func TestImportKeepsThisInstallationsRows(t *testing.T) {
	ctx := context.Background()
	e := newImportEnv(t)
	cur := map[string]string{
		"selfupdate.auto_check": "0",
		"components.sources":    `{"pypi":"https://mirror.example.invalid/simple"}`,
		"hardware.last":         `{"marker":"this computer"}`,
		"secrets.key_notice":    "",
		model.KVAppPreventSleep: "1",
		"upgrade.backup":        "15|lectureflow-before-upgrade-this.db",
		model.KVMemObservedPrefix + "ov:GPU:large-v3:int8": "2000",
	}
	cd, err := openDB(filepath.Join(e.cur, DBName), false)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range cur {
		if _, err := cd.Exec("INSERT INTO kv (key, value) VALUES (?, ?)", k, v); err != nil {
			t.Fatal(err)
		}
	}
	cd.Close()
	editSource(t, e,
		"INSERT INTO kv (key, value) VALUES ('selfupdate.auto_check', '1')",
		"INSERT INTO kv (key, value) VALUES ('selfupdate.sources', '[\"https://updates.example.invalid/lf\"]')",
		"INSERT INTO kv (key, value) VALUES ('components.sources', '{\"pypi\":\"https://other.example.invalid/simple\"}')",
		"INSERT INTO kv (key, value) VALUES ('upgrade.backup', '9|lectureflow-before-upgrade-other.db')")
	if err := StageImport(ctx, ImportOptions{Src: e.src, DataDir: e.cur}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyStaged(ctx, e.cur); err != nil {
		t.Fatal(err)
	}
	d, err := openDB(filepath.Join(e.cur, DBName), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	get := func(k string) (string, bool) {
		var v string
		err := d.QueryRow("SELECT value FROM kv WHERE key = ?", k).Scan(&v)
		return v, err == nil
	}
	for k, want := range cur {
		got, ok := get(k)
		if k == "upgrade.backup" {
			if ok {
				t.Errorf("%s = %q kept", k, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s = %q (%v), want this installation's %q", k, got, ok, want)
		}
	}
	if v, ok := get("selfupdate.sources"); ok {
		t.Errorf("the imported selfupdate.sources %q was taken", v)
	}
}

// LocalAbsPath: only an absolute path on a drive of this computer.
func TestLocalAbsPath(t *testing.T) {
	refused := []string{"", " /x", "/x ", "relative/dir", `relative\dir`, `\\server\share\LectureFlow`, "//server/share/LectureFlow",
		`\\?\UNC\server\share\x`, `\\?\C:\x`, `\\.\C:\x`, `\??\C:\x`, "file:///C:/x", "smb://server/share", "file://server/share"}
	var accepted []string
	if runtime.GOOS == "windows" {
		accepted = []string{`C:\Users\someone\AppData\Roaming\LectureFlow`, "D:/LectureFlow", `c:\x`}
		refused = append(refused, "C:x", `C:`, `\x`, "/x", `C:\a\..\b`, `1:\x`)
	} else {
		accepted = []string{"/home/someone/.config/LectureFlow", "/Volumes/Data/LectureFlow"}
		refused = append(refused, `C:\x`, "/a/../b", "x/y")
	}
	for _, p := range accepted {
		if !LocalAbsPath(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range refused {
		if LocalAbsPath(p) {
			t.Errorf("%q accepted", p)
		}
	}
}
