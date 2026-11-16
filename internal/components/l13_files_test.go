// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Security audit L-13: Files[].Path of receipts, and the entry names of
// offline archives.

// A receipt (and state.json) whose Files[].Path leaves the component's
// directory is damaged like a bad Exe: the model counts as not installed.
func TestReceiptFilePathOutsideItsDirectory(t *testing.T) {
	abs := "/tmp/elsewhere/model.xml"
	if runtime.GOOS == "windows" {
		abs = `C:\elsewhere\model.xml`
	}
	for i, bad := range []string{"../../elsewhere/model.xml", "model/../../../x", abs, "C:x.xml",
		`\\server\share\x.xml`, "//server/share/x.xml", `model\x.xml`, ""} {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			t.Logf("path %q", bad)
			e := newTestEnv(t)
			m := e.open()
			mustInstall(t, m, Sources{}, ovModel)
			m.Close()

			root := filepath.Join(e.data, "components")
			statePath := filepath.Join(root, stateFileName)
			var st map[string]any
			data, _ := os.ReadFile(statePath)
			if err := json.Unmarshal(data, &st); err != nil {
				t.Fatal(err)
			}
			ent := st["active"].(map[string]any)[ovModel].(map[string]any)
			ent["files"].([]any)[0].(map[string]any)["path"] = bad
			b, _ := json.Marshal(st)
			os.WriteFile(statePath, b, 0o644)
			receipt := filepath.Join(root, filepath.FromSlash(ent["dir"].(string)), receiptFileName)
			var r map[string]any
			data, _ = os.ReadFile(receipt)
			json.Unmarshal(data, &r)
			r["files"].([]any)[0].(map[string]any)["path"] = bad
			b, _ = json.Marshal(r)
			os.WriteFile(receipt, b, 0o644)

			m2 := e.open()
			if s, _ := statusOf(m2, ovModel); s.Installed {
				t.Fatalf("model with file path %q counts as installed", bad)
			}
			if aside, _ := filepath.Glob(statePath + ".corrupt-*"); len(aside) != 1 {
				t.Errorf("state.json moved aside: %v", aside)
			}
		})
	}
}

// Where Files[].Path is used (intact, installedFiles), an unsafe one is
// never joined into a path.
func TestFilePathUseChecks(t *testing.T) {
	e := newTestEnv(t)
	m := e.open()
	good := Installed{ID: ovModel, Version: "1.0.0", Kind: KindModel, Dir: "x/1.0.0",
		Files: []InstalledFile{{Path: "model/a.xml", SHA256: strings.Repeat("a", 64), Size: 1}}}
	bad := good
	bad.Files = []InstalledFile{{Path: "../../outside.xml", SHA256: strings.Repeat("b", 64), Size: 1}}
	if !filesOK(good) || filesOK(bad) {
		t.Fatal("filesOK")
	}
	if _, ok := m.filePath(bad, bad.Files[0]); ok {
		t.Error("filePath accepted ../")
	}
	st := newState()
	st.Active[ovModel] = bad
	if got := m.installedFiles(st); len(got) != 0 {
		t.Errorf("installedFiles offers %v", got)
	}
	if m.intact(bad) {
		t.Error("intact with an unsafe file path")
	}
}

// Offline import: entry names never leave the destination (zip slip).
func TestImportEntryNamesStayInside(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "wheels")
	for _, name := range []string{"../evil.whl", `..\evil.whl`, `a\..\..\evil.whl`, "/abs/evil.whl", `C:\evil.whl`,
		"C:evil.whl", `\\server\share\evil.whl`, "//server/share/evil.whl", "a/../../evil.whl"} {
		p, err := entryPath(dst, name)
		if err != nil || p == "" {
			continue // refused
		}
		rel, rerr := filepath.Rel(dst, p)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Errorf("%q maps to %q outside %q", name, p, dst)
		}
	}
	// extractWheels: a backslash name and a symbolic link entry
	for name, data := range map[string][]byte{
		"backslash": makeZip(t, map[string][]byte{`wheels/..\..\evil.whl`: []byte("x")}),
		"symlink":   zipWithSymlink(t, "wheels/link.whl", "../../evil.whl"),
	} {
		p := filepath.Join(t.TempDir(), "a.zip")
		os.WriteFile(p, data, 0o644)
		base := t.TempDir()
		out := filepath.Join(base, "c", "wheels")
		_, err := (&offlineSource{path: p}).extractWheels(context.Background(), out)
		for _, f := range listFiles(t, base) {
			if !strings.HasPrefix(f, "c/wheels/") {
				t.Errorf("%s: wrote %s outside the wheels directory", name, f)
			}
		}
		if name == "symlink" && err == nil {
			t.Error("a symbolic link entry was accepted")
		}
	}
}

func zipWithSymlink(t *testing.T, name, target string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: name, Method: zip.Store}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(target))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
