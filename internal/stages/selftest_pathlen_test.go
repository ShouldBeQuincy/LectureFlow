// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// The Windows data directory length check (internal/api dataDirPathCheck)
// lets a data directory as long as the smaller of two budgets (R6-E): every
// working directory within workDirMax, workDirDepth being the depth of the
// material folder, the deepest working directory of the pipeline
// (internal/api TestLectureDirDepth pins the two together); and every file
// a Python worker opens there within filePathMax, the deepest
// deepestFileDepth (94) below the data directory. A self-test keeping its
// directory and its files within the material folder's depth stays within
// both; deeper, it could fail under a data directory the diagnostics
// accept (R58: the component self-test reached 82 for its directory, 100
// with its files).

// selfTestPathBudget is the material folder's depth below the data
// directory (the separator before it included): internal/api workDirDepth,
// which this package cannot import.
func selfTestPathBudget() int {
	return components.PathLen(model.MaterialDir("", db.NewID(), db.NewID())) + 1
}

// selfTestWorkNames are the deepest names Go writes in a worker's work
// directory besides what the job names: job.json's temporary file
// (writeFileAtomic) and the last result name tmpFor tries.
func selfTestWorkNames() []string {
	return []string{"job.json.tmp", "job.json", "result.json", fmt.Sprintf("result.json.%d.tmp", tmpNames-1)}
}

// relDepth is the length of p below dataDir with the separator before it
// (as the diagnostics count: len(data_dir) + depth).
func relDepth(t *testing.T, dataDir, p string) int {
	t.Helper()
	rel, err := filepath.Rel(dataDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("%s is not inside the data directory %s", p, dataDir)
	}
	return components.PathLen(rel) + 1
}

// Every component of the shipped manifest that has a self-test (all
// backends) runs it in a directory whose every path stays within the
// budget; a device self-test likewise, for every device id.
func TestSelfTestPathsWithinBudget(t *testing.T) {
	budget := selfTestPathBudget()
	m, err := components.LoadManifest(filepath.Join("..", "..", "packaging", "components", "components.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	log := e.jobLog()
	exe := e.fakeExe()
	data := e.absDataDir()
	pages := filepath.Join(e.root, "pages.json")
	e.must(os.WriteFile(pages, []byte(`{"pages": ["LectureFlow self-test"]}`), 0o644))
	// a model directory each backend's file check accepts
	modelDirs := map[string]string{}
	for backend, files := range map[string][]string{
		model.BackendFasterWhisper: {"model.bin"},
		model.BackendOpenVINO:      devices.ModelFiles,
		model.BackendMLX:           {"config.json", "weights.safetensors"},
	} {
		dir := filepath.Join(e.root, "models", backend)
		e.must(os.MkdirAll(dir, 0o755))
		for _, f := range files {
			e.must(os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644))
		}
		modelDirs[backend] = dir
	}
	d := e.deps(nil)
	tested := map[string]bool{}
	longest := ""
	for _, c := range m.Components {
		check := ComponentCheck{ID: c.ID, Backend: c.Backend, Python: exe}
		if c.Kind == components.KindPythonRuntime {
			check.Backend = config.ComponentBase
		}
		switch check.Backend {
		case config.ComponentBase:
			e.useFakeWorker("ok", pages)
		case model.BackendFasterWhisper, model.BackendMLX:
			e.useFakeWorker("ok", fakeASR)
		case model.BackendOpenVINO:
			e.useFakeWorker("ov_ok", fakeASROV)
		default:
			continue // nothing to test
		}
		e.must(os.WriteFile(filepath.Join(e.cfg.WorkerDir, "asr_mlx.py"), []byte("# fake\n"), 0o644))
		check.ModelDir = modelDirs[check.Backend]
		if b, name, precision, ok := components.ParseModelID(c.ID); ok && b == check.Backend {
			check.Model, check.Precision = name, precision
		}
		n := len(fakeJobs(t, log))
		if err := d.ComponentSelfTest(e.ctx, check); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		jobs := fakeJobs(t, log)
		if len(jobs) != n+1 {
			t.Fatalf("%s: %d worker runs", c.ID, len(jobs)-n)
		}
		j := jobs[n]
		paths := []string{j.Cwd, j.In, j.Out}
		for _, k := range []string{"audio", "pdf"} {
			if p, ok := j.Job[k].(string); ok {
				paths = append(paths, p)
			}
		}
		for _, name := range selfTestWorkNames() {
			paths = append(paths, filepath.Join(j.Cwd, name))
		}
		for _, p := range paths {
			if n := relDepth(t, data, p); n > budget {
				t.Errorf("%s (%s): %s is %d characters below the data directory, over %d", c.ID, check.Backend, p, n, budget)
			}
		}
		if len(c.ID) > len(longest) {
			longest = c.ID
		}
		tested[check.Backend] = true
	}
	for _, b := range []string{config.ComponentBase, model.BackendFasterWhisper, model.BackendOpenVINO, model.BackendMLX} {
		if !tested[b] {
			t.Errorf("no component of backend %s in the manifest was tested", b)
		}
	}
	if len(longest) < 40 {
		t.Errorf("the longest component id tested is %q", longest)
	}

	// the device self-test: {data_dir}/selftest/{device}/ with the clip
	for _, id := range []string{model.DeviceCUDA0, "cuda:99", model.DeviceFWCPU, model.DeviceOVGPU, model.DeviceOVNPU,
		model.DeviceOVCPU, model.DeviceMLXGPU} {
		dir := selfTestDir(data, id)
		for _, name := range append(selfTestWorkNames(), model.AudioFile) {
			if n := relDepth(t, data, filepath.Join(dir, name)); n > budget {
				t.Errorf("device %s: %s is %d characters below the data directory, over %d", id, name, n, budget)
			}
		}
	}
}

// Self-tests of the same component at the same time never share a work
// directory.
func TestComponentTestDirUnique(t *testing.T) {
	e := newEnv(t)
	d := e.deps(nil)
	const n = 64
	const id = "model:faster-whisper:large-v3-turbo:int8_float16"
	dirs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = d.componentTestDir(id)
		}()
	}
	close(start)
	wg.Wait()
	seen := map[string]bool{}
	for i, dir := range dirs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if seen[dir] {
			t.Fatalf("two self-tests got %s", dir)
		}
		seen[dir] = true
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	// all below selftest/components, one per call
	parent := selfTestDir(e.absDataDir(), "components")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	sort.Strings(names)
	if len(names) != n {
		t.Fatalf("%d directories under %s: %v", len(names), parent, names)
	}
}
