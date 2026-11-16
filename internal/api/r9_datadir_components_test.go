// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/sched"
)

// shippedManifest is the manifest the application ships.
func shippedManifest(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "packaging", "components", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// shippedLimit is the longest data directory under which the shipped
// manifest's Windows build of component id installs (components.checkPathLen).
func shippedLimit(t *testing.T, id string) int {
	t.Helper()
	man, err := components.LoadManifest(shippedManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	c := man.Component(id)
	if c == nil {
		t.Fatalf("no %s in the shipped manifest", id)
	}
	need := components.RelPathNeed(c, c.Build(components.PlatformWindowsX64))
	if need == 0 {
		t.Fatalf("the shipped %s has no measured depth (max_path)", id)
	}
	return components.MaxPathLen - 1 - need
}

// componentsWith is a component manager on the shipped manifest (Windows),
// with the Python runtime recorded as installed when python is set.
func componentsWith(t *testing.T, python bool) *components.Manager {
	t.Helper()
	data := t.TempDir()
	if python {
		state := `{"schema":1,"active":{"python":{"id":"python","version":"1.0.0","kind":"python-runtime","platform":"windows-x64",` +
			`"dir":"python/1.0.0","installed_at":"2026-11-01T00:00:00Z","size_bytes":1,"exe":"runtime/python/python.exe"}}}`
		if err := os.MkdirAll(filepath.Join(data, "components", "python", "1.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "components", "state.json"), []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := components.New(components.Options{
		DataDir: data, AppVersion: "3.0.0", Platform: components.PlatformWindowsX64, BuiltinManifest: shippedManifest(t),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	installed := false
	for _, st := range m.List() {
		installed = installed || st.ID == components.IDPython && st.Installed
	}
	if installed != python {
		t.Fatalf("the Python runtime installed: %v, want %v", installed, python)
	}
	return m
}

// shortBaseDir is an absolute path of exactly n characters below the
// system's temporary folder (not created): shorter ones than longDir's,
// whose base is the test's own temporary directory.
func shortBaseDir(t *testing.T, n int) string {
	t.Helper()
	base := filepath.Join(os.TempDir(), "d")
	if len(base) >= n {
		t.Fatalf("the temporary folder is already %d characters", len(base))
	}
	return base + strings.Repeat("a", n-len(base))
}

// extraCodes are the clause codes of a detail (params.extra), in order.
func extraCodes(t *testing.T, c SystemCheck) []string {
	t.Helper()
	if c.DetailInfo == nil {
		return nil
	}
	raw, _ := c.DetailInfo.Params["extra"].([]any)
	var out []string
	for _, x := range raw {
		m, _ := x.(map[string]any)
		code, _ := m["code"].(string)
		out = append(out, code)
	}
	return out
}

// T27 P2, SEC52 L-1 / L-2: the data_dir_location row names the components
// this computer needs that cannot be installed under the data directory
// (check.data_dir_path_components {path, len, max}, one clause per
// component in params.extra, hint: a shorter path): python; the
// transcription components of the hardware recommendation, only once the
// hardware is known and not when only Colab transcribes; gcloud only while
// Colab is in use. Each has its own limit (shipped manifest: gcloud 81,
// asr-cuda 96, python 108, asr-openvino 110); {max} is the smallest of the
// named ones. Installed components and those a user's own program stands
// in for are not named. Windows only.
func TestDataDirLocationRowComponents(t *testing.T) {
	lim := map[string]int{}
	for _, id := range []string{components.IDPython, hwdetect.ComponentASRCUDA, hwdetect.ComponentASROpenVINO, components.IDGcloud} {
		lim[id] = shippedLimit(t, id)
	}
	if lim[components.IDGcloud] >= lim[hwdetect.ComponentASRCUDA] || lim[hwdetect.ComponentASRCUDA] >= lim[components.IDPython] ||
		lim[components.IDPython] >= lim[hwdetect.ComponentASROpenVINO] || lim[hwdetect.ComponentASROpenVINO] >= dataDirPathMax-dataDirPathMargin {
		t.Fatalf("the shipped limits changed order (%v): revisit the cases below", lim)
	}
	m := componentsWith(t, false)
	mInstalled := componentsWith(t, true)
	ce := newColabEnv(t)
	e := ce.env
	e.srv.diag.waitStartup()
	e.srv.comps = m
	e.srv.cfg.Python = "" // the desktop application: no interpreter of its own
	ctx := context.Background()

	oldLimited, oldFS, oldGcloud := dataDirPathLimited, dataDirFSType, gcloudLookup
	dataDirPathLimited = true
	noACL := false
	dataDirFSType = func(string) (string, bool) {
		if noACL {
			return "exFAT", true
		}
		return "NTFS", false
	}
	ownGcloud := false
	gcloudLookup = func(*Server) (string, bool) { return `C:\gcloud\gcloud.cmd`, ownGcloud }
	t.Cleanup(func() {
		dataDirPathLimited, dataDirFSType, gcloudLookup = oldLimited, oldFS, oldGcloud
		clearCredentialRows()
	})
	// the hardware the diagnostics remember (kv hardware.last); nil: not known
	hardware := func(h *HardwareLast) {
		d := e.srv.diag
		d.hwMu.Lock()
		d.hwLast, d.hwRead = h, true
		d.hwMu.Unlock()
	}
	hardware(nil)

	row := func(dir string) (SystemCheck, int) {
		t.Helper()
		e.srv.dataDir = dir
		return rowNamed(systemRows(t, e), checkDataDirLocation)
	}
	none := func(name string, n int) {
		t.Helper()
		if c, i := row(shortBaseDir(t, n)); i >= 0 {
			t.Fatalf("%s, %d characters: %+v", name, n, c)
		}
	}
	want := func(name string, n int, ids []string, hint string) SystemCheck {
		t.Helper()
		dir := shortBaseDir(t, n)
		c, i := row(dir)
		if i < 0 || c.Status != statusWarn || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathComponents || c.HintInfo == nil || c.HintInfo.Code != hint {
			t.Fatalf("%s, %d characters: %+v", name, n, c)
		}
		var codes []string
		max := 0
		for _, id := range ids {
			codes = append(codes, dataDirComponentClause[id])
			if max == 0 || lim[id] < max {
				max = lim[id]
			}
		}
		p := c.DetailInfo.Params
		if p["path"] != dir || p["len"] != float64(n) || p["max"] != float64(max) || len(p) != 4 {
			t.Fatalf("%s, %d characters: params %v, want max %d", name, n, p, max)
		}
		if got := extraCodes(t, c); !slices.Equal(got, codes) {
			t.Fatalf("%s, %d characters: clauses %v, want %v", name, n, got, codes)
		}
		// the server's own text names them too, and promises no way out
		for _, code := range codes {
			if !strings.Contains(c.Detail, errorCatalog[code].zh) {
				t.Fatalf("%s: detail %q lacks %q", name, c.Detail, errorCatalog[code].zh)
			}
		}
		if strings.Contains(c.Detail, "只能用") {
			t.Fatalf("%s: detail %q", name, c.Detail)
		}
		return c
	}
	py, cuda, ov, gc := components.IDPython, hwdetect.ComponentASRCUDA, hwdetect.ComponentASROpenVINO, components.IDGcloud

	// Hardware not known yet: python only (no guess at a transcription
	// component, no false alarm). T27's case, 110 characters.
	want("unknown hardware", 110, []string{py}, codeHintDataDirShorter)
	none("unknown hardware, at python's limit", lim[py])
	none("unknown hardware, past asr-cuda's limit", lim[cuda]+1)
	want("unknown hardware", lim[py]+1, []string{py}, codeHintDataDirShorter)

	// An NVIDIA GPU: asr-cuda counts (96).
	hardware(&HardwareLast{NVIDIA: []string{"NVIDIA GeForce RTX 4060 Laptop GPU"}})
	none("NVIDIA, at asr-cuda's limit", lim[cuda])
	want("NVIDIA", lim[cuda]+1, []string{cuda}, codeHintDataDirShorter)
	want("NVIDIA", 110, []string{py, cuda}, codeHintDataDirShorter)
	// with an Intel NPU as well: asr-openvino too (battery mode)
	hardware(&HardwareLast{NVIDIA: []string{"NVIDIA GeForce RTX 4060 Laptop GPU"}, IntelNPU: true})
	want("NVIDIA and NPU", lim[ov]+1, []string{py, cuda, ov}, codeHintDataDirShorter)
	// Intel GPU: asr-openvino (110), not asr-cuda
	hardware(&HardwareLast{IntelGPU: true})
	none("Intel, past asr-cuda's limit", lim[cuda]+1)
	want("Intel", lim[py]+1, []string{py}, codeHintDataDirShorter)
	want("Intel", lim[ov]+1, []string{py, ov}, codeHintDataDirShorter)
	// no GPU at all (known): asr-openvino on the CPU
	hardware(&HardwareLast{NVIDIA: []string{}})
	want("CPU only", lim[ov]+1, []string{py, ov}, codeHintDataDirShorter)

	// Colab in use, no gcloud of the user's own: gcloud counts (81).
	hardware(nil)
	none("Colab off, past gcloud's limit", lim[gc]+1)
	if err := sched.WriteRemotePrefs(ctx, e.st, sched.RemotePrefs{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	none("Colab on, at gcloud's limit", lim[gc])
	want("Colab on", lim[gc]+1, []string{gc}, codeHintDataDirShorter)
	want("Colab on", 110, []string{py, gc}, codeHintDataDirShorter)
	ownGcloud = true
	none("Colab on, gcloud on PATH", lim[gc]+1)
	ownGcloud = false
	// Only Colab transcribes (asr mode remote): no local transcription
	// component even with an NVIDIA GPU; gcloud counts (Colab in use).
	hardware(&HardwareLast{NVIDIA: []string{"NVIDIA GeForce RTX 4060 Laptop GPU"}})
	if err := sched.WriteRemotePrefs(ctx, e.st, sched.RemotePrefs{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetKV(ctx, kvASRMode, ASRModeRemote); err != nil {
		t.Fatal(err)
	}
	want("remote", lim[cuda]+1, []string{gc}, codeHintDataDirShorter)
	if err := e.st.SetKV(ctx, kvASRMode, ASRModeLocal); err != nil {
		t.Fatal(err)
	}

	// The order: "too long to start" first, then this note, then the
	// security notes and the file budget's notes. At 160 and 170
	// characters (near the limit, too long for the files) this note takes
	// the row; past {hard} "too long to start" does, as before.
	hardware(nil)
	for _, n := range []int{160, dataDirPathMax + 5} {
		want("python missing", n, []string{py}, codeHintDataDirShorter)
	}
	if c, i := row(shortBaseDir(t, dataDirPathHard+1)); i < 0 || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathTooLongToStart {
		t.Fatalf("past {hard}: %+v", c)
	}
	// With a security note too: one row, this note, and the security
	// notes' hint (an unsynced local NTFS disk such as the default
	// location: short enough as well).
	dir := filepath.Join(t.TempDir(), "Dropbox", "d")
	dir += strings.Repeat("a", 120-len(dir))
	if c, i := row(dir); i < 0 || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathComponents || c.HintInfo == nil || c.HintInfo.Code != codeHintDataDirMove {
		t.Fatalf("synced: %+v", c)
	}
	noACL = true
	want("no permissions", 120, []string{py}, codeHintDataDirMove)
	noACL = false

	// Python installed: not named (it was checked when installed); with an
	// NVIDIA GPU asr-cuda still is. Without the hardware: the rows of
	// before (none at 110, "near the limit" at 160).
	e.srv.comps = mInstalled
	none("python installed", 110)
	if c, i := row(shortBaseDir(t, 160)); i < 0 || c.DetailInfo == nil || c.DetailInfo.Code != codeDataDirPathNearLimit {
		t.Fatalf("python installed, 160: %+v", c)
	}
	hardware(&HardwareLast{NVIDIA: []string{"NVIDIA GeForce RTX 4060 Laptop GPU"}})
	want("python installed, NVIDIA", 110, []string{cuda}, codeHintDataDirShorter)
	// an interpreter of the user's own (config.toml python, which the
	// faster-whisper worker runs as well) needs neither component
	e.srv.comps = m
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Python = exe
	none("own interpreter, NVIDIA", 110)
	e.srv.cfg.Python = ""
	// no component manager: nothing to install, no note
	e.srv.comps = nil
	none("no component manager", 110)
	e.srv.comps = m
	// macOS: no limit, no row
	dataDirPathLimited = false
	none("macOS", 110)
}
