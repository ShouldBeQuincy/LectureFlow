// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/db"
	"lectureflow/internal/model"
)

// longDir is an absolute path of exactly n characters (not created).
func longDir(t *testing.T, n int) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "d")
	if len(base) >= n {
		t.Fatalf("the temp dir is already %d characters", len(base))
	}
	return base + strings.Repeat("a", n-len(base))
}

// test-upgrade F10, R6-E: on Windows the data_dir_location row warns when
// the data directory is longer than dataDirPathHard, so that no process
// starts in the folders below it (too long to start, with {hard}; R66 F1),
// longer than dataDirPathMax, so that the files the workers open below it
// may be past what Windows opens without long path support (too long), or
// within dataDirPathMargin of that limit (near the limit), with the hint
// to move it (Settings → Data folder). Not on macOS.
func TestDataDirLocationRowPathLength(t *testing.T) {
	e := newEnv(t)
	e.srv.diag.waitStartup()
	oldLimited, oldFS := dataDirPathLimited, dataDirFSType
	dataDirPathLimited = true
	dataDirFSType = func(string) (string, bool) { return "NTFS", false }
	t.Cleanup(func() { dataDirPathLimited, dataDirFSType = oldLimited, oldFS; clearCredentialRows() })

	row := func(dir string) (SystemCheck, int) {
		t.Helper()
		e.srv.dataDir = dir
		return rowNamed(systemRows(t, e), checkDataDirLocation)
	}
	if _, i := row(longDir(t, dataDirPathMax-dataDirPathMargin)); i >= 0 {
		t.Fatal("a row for a data directory short enough")
	}
	for _, tc := range []struct {
		n    int
		code string
	}{
		{dataDirPathMax - dataDirPathMargin + 1, codeDataDirPathNearLimit},
		{dataDirPathMax, codeDataDirPathNearLimit},
		{dataDirPathMax + 1, codeDataDirPathTooLong},
		{dataDirPathHard, codeDataDirPathTooLong},
		{dataDirPathHard + 1, codeDataDirPathTooLongToStart},
		{229, codeDataDirPathTooLongToStart}, // test-upgrade's failing case
	} {
		dir := longDir(t, tc.n)
		c, i := row(dir)
		if i < 0 || c.Status != statusWarn || c.DetailInfo == nil || c.DetailInfo.Code != tc.code || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirShorter {
			t.Fatalf("%d: %+v", tc.n, c)
		}
		if c.DetailInfo.Params["path"] != dir || c.DetailInfo.Params["len"] != float64(tc.n) || c.DetailInfo.Params["max"] != float64(dataDirPathMax) {
			t.Fatalf("%d: params %v", tc.n, c.DetailInfo.Params)
		}
		// R66 F1: "too long to start" also says where nothing starts
		if hard, has := c.DetailInfo.Params["hard"]; has != (tc.code == codeDataDirPathTooLongToStart) || has && hard != float64(dataDirPathHard) {
			t.Fatalf("%d: params %v", tc.n, c.DetailInfo.Params)
		}
	}
	// a 160-character data directory works (test-upgrade): not too long
	// (R6-E: close to the limit since it is 165)
	if c, i := row(longDir(t, 160)); i < 0 || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathNearLimit {
		t.Fatalf("160 characters: %+v", c)
	}
	// characters outside the BMP count twice, as Windows counts them
	if _, n, ok := dataDirPathCheck(`C:\` + strings.Repeat("𝄞", (dataDirPathMax-2)/2)); !ok || n != 3+2*((dataDirPathMax-2)/2) {
		t.Fatalf("UTF-16 length: %d %v", n, ok)
	}
	// macOS and Linux: no limit, no row
	dataDirPathLimited = false
	if _, i := row(longDir(t, 229)); i >= 0 {
		t.Fatal("a row without the limit")
	}
}

// The depth of a lecture folder and of a material folder (the PDF
// extraction worker's working directory) below the data directory is what
// the threshold assumes, and it uses the deeper of the two.
func TestLectureDirDepth(t *testing.T) {
	rel := model.LectureDir("", db.NewID(), db.NewID())
	if got := components.PathLen(rel) + 1; got != lectureDirDepth {
		t.Fatalf("lecture folder depth %d (%s), the check assumes %d", got, rel, lectureDirDepth)
	}
	mrel := model.MaterialDir("", db.NewID(), db.NewID())
	if got := components.PathLen(mrel) + 1; got != materialDirDepth {
		t.Fatalf("material folder depth %d (%s), the check assumes %d", got, mrel, materialDirDepth)
	}
	if workDirDepth < lectureDirDepth || workDirDepth < materialDirDepth {
		t.Fatalf("workDirDepth %d is not the deepest (%d, %d)", workDirDepth, lectureDirDepth, materialDirDepth)
	}
}

// R6-E (T20 §1.2, R59 (d), R66 F4): the files the Python workers open in a
// lecture folder (asr.py, asr_openvino.py, asr_mlx.py, textrepair.py) and
// in a material folder (extract_pdf.py): the names LectureFlow gives them
// (internal/stages: job.json, the --out names tmpFor tries, the
// checkpoint; the upload's source.pdf), each also with every suffix the
// worker's script, or a worker module it imports, appends to a path
// (workerSuffixes: asr_common.py's checkpoint + ".tmp", textrepair.py's
// --out + ".part"). lectureDeepestFile and materialDeepestFile are among
// them and nothing is deeper, so deepestFileDepth is the deepest file a
// worker opens in these folders (not elsewhere: asr_openvino.py preparing
// a model itself goes deeper below models\, R66 F3). A worker appending a
// new or longer suffix fails here when it goes deeper (R66 mutation 2).
func TestDeepestWorkerFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "stages", "replace.go"))
	if err != nil {
		t.Fatal(err)
	}
	// tmpFor's names for --out: out.tmp, out.1.tmp … out.{tmpNames−1}.tmp
	m := regexp.MustCompile(`(?m)^const tmpNames = (\d+)\r?$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("internal/stages/replace.go: no tmpNames")
	}
	n, _ := strconv.Atoi(string(m[1]))
	outs := []string{"result.json.tmp"}
	for i := 1; i < n; i++ {
		outs = append(outs, fmt.Sprintf("result.json.%d.tmp", i))
	}
	withSuffixes := func(names, sfx []string) []string {
		out := append([]string{}, names...)
		for _, name := range names {
			for _, s := range sfx {
				out = append(out, name+s)
			}
		}
		return out
	}
	lecture := withSuffixes(append([]string{"job.json", model.AudioFile, model.ASRCheckpointFile}, outs...),
		workerSuffixes(t, "asr", "asr_openvino", "asr_mlx", "textrepair"))
	material := withSuffixes(append([]string{"job.json", "source.pdf"}, outs...), workerSuffixes(t, "extract_pdf"))
	for _, f := range []struct {
		folder  string
		depth   int
		names   []string
		deepest string
	}{
		{"lecture folder", lectureDirDepth, lecture, lectureDeepestFile},
		{"material folder", materialDirDepth, material, materialDeepestFile},
	} {
		deep := f.depth + components.PathLen(f.deepest)
		if deep > deepestFileDepth {
			t.Errorf("%s: %s is %d deep, deeper than deepestFileDepth %d", f.folder, f.deepest, deep, deepestFileDepth)
		}
		among := false
		for _, name := range f.names {
			if d := f.depth + components.PathLen(`\`+name); d > deep {
				t.Errorf("%s: %s is %d deep, deeper than %s (%d)", f.folder, name, d, f.deepest, deep)
			}
			among = among || `\`+name == f.deepest
		}
		if !among {
			t.Errorf("%s: %s is no file a worker opens (%v)", f.folder, f.deepest, f.names)
		}
	}
	if deepestFileDepth != max(lectureDirDepth+components.PathLen(lectureDeepestFile), materialDirDepth+components.PathLen(materialDeepestFile)) {
		t.Errorf("deepestFileDepth %d is not the deeper of the two folders' deepest files", deepestFileDepth)
	}
}

// Path suffixes in a worker script: a string literal starting with a dot
// appended to a path ("+ '.tmp'") or after a "%s" ("%s.old-%d"); and the
// script's imports, to follow the worker modules (worker/{name}.py).
var (
	workerSuffixRe = regexp.MustCompile(`\+\s*['"](\.[A-Za-z0-9_.*-]+)['"]|['"]%s(\.[A-Za-z0-9_.-]+(?:%d)?)['"]`)
	workerImportRe = regexp.MustCompile(`(?m)^[ \t]*(?:from[ \t]+(\w+)[ \t]+import|import[ \t]+(\w+))`)
)

// workerModelDirSuffixes: names asr_openvino.py makes of the OpenVINO
// model directory ({model_dir}.tmp-{pid} / .old-{pid} and their globs),
// below models\, not in a lecture or material folder.
var workerModelDirSuffixes = map[string]bool{".tmp-*": true, ".old-*": true, ".tmp-%d": true, ".old-%d": true}

// workerSuffixes: the path suffixes the worker scripts (worker/{entry}.py)
// and the worker modules they import use, sorted; the model directory's
// only in asr_openvino.py, and left out.
func workerSuffixes(t *testing.T, entries ...string) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "worker")
	seen, sfx := map[string]bool{}, map[string]bool{}
	queue := append([]string{}, entries...)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 100 {
			t.Fatal("worker imports: no end")
		}
		mod := queue[0]
		queue = queue[1:]
		if seen[mod] {
			continue
		}
		seen[mod] = true
		b, err := os.ReadFile(filepath.Join(dir, mod+".py"))
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range workerImportRe.FindAllSubmatch(b, -1) {
			name := string(im[1]) + string(im[2])
			if _, err := os.Stat(filepath.Join(dir, name+".py")); err == nil && !seen[name] {
				queue = append(queue, name)
			}
		}
		for _, s := range workerSuffixRe.FindAllSubmatch(b, -1) {
			x := string(s[1]) + string(s[2])
			if workerModelDirSuffixes[x] {
				if mod != "asr_openvino" {
					t.Errorf("worker/%s.py: %q is a model directory's suffix, counted only in asr_openvino.py", mod, x)
				}
				continue
			}
			sfx[x] = true
		}
	}
	out := make([]string, 0, len(sfx))
	for s := range sfx {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// The scan finds the suffixes the derivation counts: the checkpoint's .tmp
// and textrepair.py's .part in the lecture folder, none in the material
// folder; and it follows imports (asr.py has its .tmp from asr_common.py).
func TestWorkerSuffixes(t *testing.T) {
	if got := workerSuffixes(t, "asr", "asr_openvino", "asr_mlx", "textrepair"); strings.Join(got, " ") != ".part .tmp" {
		t.Errorf("lecture folder workers: %q", got)
	}
	if got := workerSuffixes(t, "asr"); strings.Join(got, " ") != ".part .tmp" {
		t.Errorf("asr.py with its modules: %q", got)
	}
	if got := workerSuffixes(t, "extract_pdf"); len(got) != 0 {
		t.Errorf("extract_pdf.py: %q", got)
	}
	for _, c := range []struct {
		src  string
		want string
	}{
		{`self.tmp = job.path + ".tmp"`, ".tmp"},
		{`tmp = path + '.tmp.partial'`, ".tmp.partial"},
		{`aside = "%s.old-%d" % (target, os.getpid())`, ".old-%d"},
		{`glob.escape(model_dir) + ".tmp-*"`, ".tmp-*"},
		{`is_sentence_end(cores[k] + ".")`, ""},
		{`return head + ". " + context`, ""},
		{`raise InputError("%s.asr_text must be a string" % where)`, ""},
	} {
		got := ""
		if s := workerSuffixRe.FindStringSubmatch(c.src); s != nil {
			got = s[1] + s[2]
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.src, got, c.want)
		}
	}
}

// The boundary (security review L3, R6-E): with the longest data directory
// that passes, every working directory below it fits in workDirMax and
// every file a worker opens in filePathMax; one more character and the
// deepest file no longer fits, and the row says so. The file budget is the
// one that binds (the texts speak of writing files first); the working
// directory budget, dataDirPathHard ({hard}), is where "too long to start"
// begins: past it the material folder (the first) is too long to start a
// process in, long path support or not (R66 F1). The user-visible numbers
// are pinned here once: the limit, where "close to the limit" starts, and
// {hard}.
func TestDataDirPathMaxBoundary(t *testing.T) {
	if dataDirPathMax != 165 || dataDirPathMax-dataDirPathMargin+1 != 156 || dataDirPathHard != 186 {
		t.Fatalf("dataDirPathMax %d, close from %d, hard %d; want 165, from 156, 186", dataDirPathMax, dataDirPathMax-dataDirPathMargin+1, dataDirPathHard)
	}
	if dataDirPathMax != filePathMax-deepestFileDepth || dataDirPathHard != workDirMax-workDirDepth || dataDirPathMax >= dataDirPathHard {
		t.Fatalf("dataDirPathMax %d: the file budget %d − %d does not bind (working directories: %d − %d = %d)",
			dataDirPathMax, filePathMax, deepestFileDepth, workDirMax, workDirDepth, dataDirPathHard)
	}
	if n := dataDirPathHard + workDirDepth; n > workDirMax {
		t.Errorf("at {hard} the deepest working directory has %d characters, over %d", n, workDirMax)
	}
	if n := dataDirPathHard + 1 + workDirDepth; n <= workDirMax {
		t.Errorf("one past {hard} the deepest working directory has %d characters, within %d", n, workDirMax)
	}
	for _, rel := range []string{model.LectureDir("", db.NewID(), db.NewID()), model.MaterialDir("", db.NewID(), db.NewID())} {
		if n := dataDirPathMax + components.PathLen(rel) + 1; n > workDirMax {
			t.Errorf("%s: %d characters at the threshold, over %d", rel, n, workDirMax)
		}
	}
	if n := dataDirPathMax + deepestFileDepth; n > filePathMax {
		t.Errorf("the deepest file has %d characters at the threshold, over %d", n, filePathMax)
	}
	if n := dataDirPathMax + 1 + deepestFileDepth; n <= filePathMax {
		t.Errorf("one past the threshold the deepest file has %d characters, within %d", n, filePathMax)
	}
	old := dataDirPathLimited
	dataDirPathLimited = true
	t.Cleanup(func() { dataDirPathLimited = old })
	dir := `C:\` + strings.Repeat("a", dataDirPathMax-3)
	if code, n, _ := dataDirPathCheck(dir); n != dataDirPathMax || code == codeDataDirPathTooLong {
		t.Errorf("%d characters: %q", n, code)
	}
	if code, n, _ := dataDirPathCheck(dir + "a"); n != dataDirPathMax+1 || code != codeDataDirPathTooLong {
		t.Errorf("%d characters: %q", n, code)
	}
	hard := `C:\` + strings.Repeat("a", dataDirPathHard-3)
	if code, n, _ := dataDirPathCheck(hard); n != dataDirPathHard || code != codeDataDirPathTooLong {
		t.Errorf("%d characters: %q", n, code)
	}
	if code, n, _ := dataDirPathCheck(hard + "a"); n != dataDirPathHard+1 || code != codeDataDirPathTooLongToStart {
		t.Errorf("%d characters: %q", n, code)
	}
}

// R6-E (replacing C5's steps, R9 suggestion 5): the two texts say what the
// limit is about. The file budget binds (TestDataDirPathMaxBoundary), so
// they speak of transcription, fixing sentence breaks (textrepair.py, the
// deepest file: R66 F5) and slide text extraction (T18: "close to the
// limit" named only ffmpeg and transcription) not writing their result
// files without Windows' long path support, the files about
// deepestFileDepth (rounded to ten) deeper; "may", as what fails beyond
// the limit depends on the file. Past {hard} "too long to start" says both
// (R66 F1): no process starts in the folders about workDirDepth (rounded)
// deeper, long path support or not (measured), and within {hard} but past
// {max} the files may still fail. The limit is derived from the code, not
// measured: no "measured", and no more "from 2 characters more" steps.
func TestDataDirPathTexts(t *testing.T) {
	files := fmt.Sprintf("约 %d 个字符", (deepestFileDepth+5)/10*10)
	dirs := fmt.Sprintf("约 %d 个字符深的文件夹", (workDirDepth+5)/10*10)
	for _, tc := range []struct {
		code      string
		want, bad []string
	}{
		{codeDataDirPathTooLongToStart,
			[]string{"超过了 {hard}", dirs, "不论是否开启 Windows 长路径支持", "转写、修复断句与课件文本抽取都无法启动", "超过 {max} 时", "没有开启长路径支持", "这几步也可能写不了结果文件"},
			[]string{"实测", "多出 2 个字符", "这两步"}},
		{codeDataDirPathTooLong,
			[]string{"超过了 {max}", "转写、修复断句与课件文本抽取", files, "没有开启 Windows 长路径支持", "这几步可能写不了结果文件"},
			[]string{"实测", "多出 2 个字符", "约 70 个字符", "这两步", "无法启动", "{hard}"}},
		{codeDataDirPathNearLimit,
			[]string{"接近上限 {max}", "转写、修复断句与课件文本抽取", files, "没有开启 Windows 长路径支持", "这几步就可能写不了结果文件"},
			[]string{"实测", "多出 2 个字符", "约 70 个字符", "这两步", "无法启动", "{hard}"}},
	} {
		zh := errorCatalog[tc.code].zh
		for _, want := range tc.want {
			if !strings.Contains(zh, want) {
				t.Errorf("%s: %q lacks %q", tc.code, zh, want)
			}
		}
		for _, bad := range tc.bad {
			if strings.Contains(zh, bad) {
				t.Errorf("%s: %q still says %q", tc.code, zh, bad)
			}
		}
	}
	// "too long" keeps the params the other locales' texts carry
	for code, want := range map[string]string{codeDataDirPathTooLongToStart: "path len max hard", codeDataDirPathTooLong: "path len max", codeDataDirPathNearLimit: "path len max"} {
		if p := errorCatalog[code].params; strings.Join(p, " ") != want {
			t.Errorf("%s params %v, want %s", code, p, want)
		}
	}
}
