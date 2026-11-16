// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/pyenv"
)

// runItem installs one component of a task. Every path keeps the rule that
// state.json lists a version only after its directory is complete: work
// happens in a staging directory (or, for the python runtime, in a version
// directory state.json does not reference yet) and the receipt and the
// state entry come last.
func (m *Manager) runItem(ctx context.Context, t *task, it *item) error {
	if it.offline != nil {
		if err := m.prepareImport(ctx, t, it); err != nil {
			return err
		}
		if it.phase == PhaseSkipped {
			return nil
		}
	}
	man := t.man
	c := man.Component(it.id)
	if c == nil {
		return cerr(CodeUnknown, map[string]any{"id": it.id}, fmt.Errorf("unknown component %q", it.id))
	}
	b := c.Build(m.platform)
	if b == nil {
		return cerr(CodeUnsupportedPlatform, map[string]any{"id": it.id, "platform": m.platform}, errors.New("no build"))
	}

	m.mu.Lock()
	cur, installed := m.st.Active[it.id]
	m.mu.Unlock()
	switch {
	case installed && !it.update:
		m.setPhase(t, it, PhaseSkipped)
		return nil
	case installed && it.update && cur.Version == it.version && !m.needsRebuild(cur):
		m.setPhase(t, it, PhaseSkipped)
		return nil
	case !installed && it.update:
		it.update = false // uninstalled meanwhile: a plain install
	}
	m.mu.Lock()
	it.doneBytes, it.curBytes = 0, 0 // counted again from the files (resume, retry)
	m.mu.Unlock()

	var err error
	switch c.Kind {
	case KindModel, KindBinary:
		err = m.installFiles(ctx, t, it, c, b)
	case KindVenv:
		err = m.installVenv(ctx, t, it, c, b)
	case KindPythonRuntime:
		err = m.installPython(ctx, t, it, c, b)
	case KindTool:
		err = m.installTool(ctx, t, it, c, b)
	default:
		err = fmt.Errorf("unknown kind %q", c.Kind)
	}
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return asCoded(err, map[string]any{"id": it.id})
	}
	return nil
}

func (m *Manager) needsRebuild(e Installed) bool {
	if e.Kind != KindVenv || e.Python == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	py, ok := m.st.Active[IDPython]
	return ok && py.Version != e.Python
}

// stagingDir creates (or reuses, after a pause) the item's staging
// directory {dir}/.s{n}: the first n that does not exist yet, created
// exclusively, so two tasks never share one. The name is short on purpose
// (V12.2 acceptance issue 1): the venv is built and checked in it, so its
// path must not be longer than the version directory it becomes
// (RelPathNeed counts {version}-r99). cleanOrphans removes leftovers.
func (m *Manager) stagingDir(t *task, it *item) (string, error) {
	if it.staging != "" {
		if fi, err := os.Stat(it.staging); err == nil && fi.IsDir() {
			return it.staging, nil
		}
	}
	base := filepath.Join(m.root, DirName(it.id))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", asCoded(err, nil)
	}
	for n := 1; n < 100; n++ {
		dir := filepath.Join(base, fmt.Sprintf(".s%d", n))
		err := os.Mkdir(dir, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", asCoded(err, nil)
		}
		m.mu.Lock()
		it.staging = dir
		m.mu.Unlock()
		return dir, nil
	}
	return "", cerr(CodeIO, nil, fmt.Errorf("%s: too many staging directories", base))
}

// finalRel picks the version directory: {dir}/{version}, or
// {dir}/{version}-r{n} when that one is in use (a venv rebuilt with the
// same version). A leftover directory state.json does not reference is
// removed first.
func (m *Manager) finalRel(id, version string) (string, error) {
	m.mu.Lock()
	refs := m.st.referencedDirs()
	m.mu.Unlock()
	base := DirName(id) + "/" + version
	for n := 1; n < 100; n++ {
		rel := base
		if n > 1 {
			rel = fmt.Sprintf("%s-r%d", base, n)
		}
		if refs[rel] {
			continue
		}
		if err := removeAllRetry(m.abs(rel)); err != nil {
			return "", asCoded(err, nil)
		}
		return rel, nil
	}
	return "", cerr(CodeIO, nil, errors.New("too many versions"))
}

// obtain makes the verified file available in downloads/ (from an installed
// component with the same content, the offline archive, or the network)
// and returns its path.
func (m *Manager) obtain(ctx context.Context, t *task, it *item, f File) (string, error) {
	spec := fileSpec{Name: f.Name, SHA256: f.SHA256, Size: f.Size}
	if ok, err := m.dl.haveVerified(ctx, spec); err != nil {
		return "", err
	} else if ok {
		return m.dl.finalPath(f.SHA256), nil
	}
	if it.offline != nil {
		return it.offline.extractFile(ctx, m, it.id, f, m.progress(t, it))
	}
	m.mu.Lock()
	reuse := m.installedFiles(m.st)[f.SHA256]
	m.mu.Unlock()
	if reuse != "" && f.Unpack == "" {
		// the installed copy is hashed again before it is reused, like a
		// download (security audit L-13); a copy of another size is not
		// read at all
		same := false
		if fi, err := os.Stat(reuse); err == nil && fi.Mode().IsRegular() && fi.Size() == f.Size {
			if same, err = fileMatches(ctx, reuse, f.Size, f.SHA256); err != nil {
				return "", err
			}
		}
		if same {
			os.MkdirAll(m.dl.dir, 0o755)
			dst := m.dl.finalPath(f.SHA256)
			if err := linkOrCopy(reuse, dst); err == nil {
				m.log.Info("components: reusing an installed file", "file", f.Name, "from", reuse)
				return dst, nil
			}
			os.Remove(dst)
		}
	}
	urls, err := t.man.resolveURL(f.URL, t.src)
	if err != nil {
		return "", cerr(CodeManifestInvalid, map[string]any{"id": it.id}, err)
	}
	spec.URLs = urls
	m.setSource(t, it, urls[0])
	m.setPhase(t, it, PhaseDownloading)
	return m.dl.fetch(ctx, spec, m.progress(t, it), func(u sourceURL, reason model.ErrorInfo) {
		m.setSource(t, it, sourceURL{})
		m.notice(t, model.ErrorInfo{Code: CodeMirrorFallback,
			Params: map[string]any{"group": u.Group, "mirror": redactURL(u.Mirror), "reason": reason.Code}})
	})
}

// linkOrCopy hard-links src to dst, copying when the volume cannot link.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".copy"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return renameRetry(tmp, dst)
}

// installFiles installs a model or binary component.
func (m *Manager) installFiles(ctx context.Context, t *task, it *item, c *Component, b *Build) error {
	staging, err := m.stagingDir(t, it)
	if err != nil {
		return err
	}
	sub := "model"
	if c.Kind == KindBinary {
		sub = "bin"
	}
	var files []InstalledFile
	for _, f := range b.Files {
		dst := filepath.Join(staging, sub, filepath.FromSlash(f.Name))
		if f.Unpack == "" {
			// moved before a pause: hashed again before it is kept (security
			// audit L-13), deleted and obtained again when it differs
			if fi, err := os.Stat(dst); err == nil && fi.Size() == f.Size {
				same, err := fileMatches(ctx, dst, f.Size, f.SHA256)
				if err != nil {
					return err
				}
				if same {
					m.fileDone(t, it, f.Size)
					files = append(files, InstalledFile{Path: sub + "/" + f.Name, SHA256: f.SHA256, Size: f.Size})
					continue
				}
				m.log.Warn("components: a file moved before a pause no longer matches its SHA-256; obtaining it again", "file", f.Name)
				os.Remove(dst)
			}
		}
		p, err := m.obtain(ctx, t, it, f)
		if err != nil {
			return err
		}
		if f.Unpack != "" {
			m.setPhase(t, it, PhaseUnpacking)
			if err := unpack(ctx, p, f.Unpack, fileLayout(f), filepath.Join(staging, sub)); err != nil {
				return err
			}
			os.Remove(p)
		} else {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := renameRetry(p, dst); err != nil {
				return err
			}
			files = append(files, InstalledFile{Path: sub + "/" + f.Name, SHA256: f.SHA256, Size: f.Size})
		}
		m.fileDone(t, it, f.Size)
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	req := SelfTestRequest{ID: c.ID, Version: c.Version, Kind: c.Kind, Backend: c.Backend, Dir: staging}
	if c.Kind == KindModel {
		req.ModelDir = filepath.Join(staging, sub)
		if venvID := m.venvComponentLocked(c.Backend); venvID != "" {
			req.Python, _ = m.Python(c.Backend)
		}
	}
	return m.finish(ctx, t, it, c, staging, Installed{Files: files}, req)
}

func (m *Manager) venvComponentLocked(backend string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.venvComponent(backend)
}

// finish runs the update self-test, writes the receipt, renames the staging
// directory to its version directory and switches state.json.
func (m *Manager) finish(ctx context.Context, t *task, it *item, c *Component, staging string, rec Installed, req SelfTestRequest) error {
	if it.update {
		// a tool's own check (installTool) is its self-test
		if c.Kind != KindTool {
			if err := m.runSelfTest(ctx, t, it, req); err != nil {
				return err
			}
		}
		if err := m.checkInUse(it.id); err != nil {
			return err // the old version stays; the new one is removed with the staging directory
		}
	}
	rel, err := m.finalRel(c.ID, c.Version)
	if err != nil {
		return err
	}
	rec.ID, rec.Version, rec.Kind, rec.Backend, rec.Platform, rec.Dir = c.ID, c.Version, c.Kind, c.Backend, m.platform, rel
	rec.InstalledAt = m.now().UTC().Format(time.RFC3339)
	rec.SizeBytes = dirSize(staging)
	if err := writeReceipt(staging, rec); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err := renameRetry(staging, m.abs(rel)); err != nil {
		return cerr(CodeIO, nil, err)
	}
	m.mu.Lock()
	it.staging = ""
	m.mu.Unlock()
	return m.activate(t, it, rec)
}

// activate switches state.json to rec and removes what the switch freed.
func (m *Manager) activate(t *task, it *item, rec Installed) error {
	m.mu.Lock()
	ns := m.st.clone()
	old, hadOld := ns.Active[rec.ID]
	ns.Active[rec.ID] = rec
	var drop []string
	if hadOld && old.Dir != rec.Dir {
		keepOld := false
		if old.Kind == KindPythonRuntime {
			for _, e := range ns.Active {
				if e.Kind == KindVenv && e.Python == old.Version {
					keepOld = true
				}
			}
		}
		if keepOld {
			ns.Retained = appendUnique(ns.Retained, old)
		} else {
			drop = append(drop, m.abs(old.Dir))
		}
	}
	if err := m.writeState(ns); err != nil {
		m.mu.Unlock()
		removeAllRetry(m.abs(rec.Dir)) // not referenced: never looks installed
		return err
	}
	m.st = ns
	it.phase = PhaseDone
	it.curBytes = 0
	it.doneBytes = it.total
	m.emitTask(t, true)
	m.emitComponents()
	drop = append(drop, m.gcRetained()...)
	m.mu.Unlock()
	m.removeDirs(drop)
	m.log.Info("components: installed", "id", rec.ID, "version", rec.Version, "dir", rec.Dir)
	return nil
}

// runSelfTest runs Options.SelfTest for an update.
func (m *Manager) runSelfTest(ctx context.Context, t *task, it *item, req SelfTestRequest) error {
	if m.selfTest == nil {
		m.notice(t, model.ErrorInfo{Code: CodeSelfTestSkipped, Params: map[string]any{"id": it.id}})
		return nil
	}
	m.setPhase(t, it, PhaseSelfTest)
	if err := m.selfTest(ctx, req); err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		m.log.Warn("components: update self-test failed; keeping the installed version", "id", it.id, "error", err)
		return cerr(CodeSelfTestFailed, map[string]any{"id": it.id}, err)
	}
	return nil
}

// installVenv creates a backend environment with the python component's
// interpreter and installs the lock file.
func (m *Manager) installVenv(ctx context.Context, t *task, it *item, c *Component, b *Build) error {
	m.mu.Lock()
	py, ok := m.st.Active[IDPython]
	m.mu.Unlock()
	if !ok {
		return cerr(CodeNotInstalled, map[string]any{"id": IDPython}, errors.New("the python component is not installed"))
	}
	pyExe, ok := m.exePath(py)
	if !ok {
		return cerr(CodeNotInstalled, map[string]any{"id": IDPython}, fmt.Errorf("the python component's receipt names no usable interpreter (%q)", py.Exe))
	}
	lock, err := m.lockFile(ctx, t, it, b)
	if err != nil {
		return err
	}
	staging, err := m.stagingDir(t, it)
	if err != nil {
		return err
	}
	venv := filepath.Join(staging, "venv")
	if err := m.buildVenv(ctx, t, it, pyExe, venv, lock); err != nil {
		return err
	}
	if err := m.checkImports(ctx, t, it, venvPython(venv), c.CheckImports); err != nil {
		return err
	}
	req := SelfTestRequest{ID: c.ID, Version: c.Version, Kind: c.Kind, Backend: c.Backend, Dir: staging,
		Python: venvPython(venv), ModelDir: m.anyModelDir(c.Backend)}
	return m.finish(ctx, t, it, c, staging, Installed{Python: py.Version}, req)
}

// anyModelDir returns an installed model of a backend ("" when none).
func (m *Manager) anyModelDir(backend string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.st.sortedIDs() {
		if b, _, _, ok := ParseModelID(id); ok && b == backend {
			return filepath.Join(m.abs(m.st.Active[id].Dir), "model")
		}
	}
	return ""
}

// installPython unpacks the runtime archive into a version directory that
// state.json does not reference yet (a venv records its interpreter's
// absolute home, so the runtime must be in its final place before the base
// venv is built), then builds the base venv there; the receipt and
// state.json come last.
func (m *Manager) installPython(ctx context.Context, t *task, it *item, c *Component, b *Build) error {
	lock, err := m.lockFile(ctx, t, it, b)
	if err != nil {
		return err
	}
	f := b.Files[0]
	rel := it.pythonRel
	if rel == "" || !fileExists(filepath.Join(m.abs(rel), "runtime", filepath.FromSlash(b.Exe))) {
		staging, err := m.stagingDir(t, it)
		if err != nil {
			return err
		}
		runtimeDir := filepath.Join(staging, "runtime")
		if !fileExists(filepath.Join(runtimeDir, filepath.FromSlash(b.Exe))) {
			p, err := m.obtain(ctx, t, it, f)
			if err != nil {
				return err
			}
			m.setPhase(t, it, PhaseUnpacking)
			removeAllRetry(runtimeDir)
			if err := unpack(ctx, p, f.Unpack, fileLayout(f), runtimeDir); err != nil {
				return err
			}
			if !fileExists(filepath.Join(runtimeDir, filepath.FromSlash(b.Exe))) {
				return cerr(CodeUnpackFailed, map[string]any{"file": f.Name},
					fmt.Errorf("the archive has no %s", b.Exe))
			}
			os.Remove(p)
		}
		m.fileDone(t, it, f.Size)
		if rel, err = m.finalRel(c.ID, c.Version); err != nil {
			return err
		}
		if err := renameRetry(staging, m.abs(rel)); err != nil {
			return cerr(CodeIO, nil, err)
		}
		m.mu.Lock()
		it.staging = m.abs(rel) // cancel / failure removes it: state.json does not reference it
		it.pythonRel = rel
		m.mu.Unlock()
	}
	dir := m.abs(rel)
	exe := filepath.Join(dir, "runtime", filepath.FromSlash(b.Exe))
	venv := filepath.Join(dir, "venv")
	if err := m.buildVenv(ctx, t, it, exe, venv, lock); err != nil {
		return err
	}
	if err := m.checkImports(ctx, t, it, venvPython(venv), c.CheckImports); err != nil {
		return err
	}
	if it.update {
		req := SelfTestRequest{ID: c.ID, Version: c.Version, Kind: c.Kind, Backend: BackendBase, Dir: dir, Python: venvPython(venv)}
		if err := m.runSelfTest(ctx, t, it, req); err != nil {
			return err
		}
		if err := m.checkInUse(it.id); err != nil {
			return err
		}
	}
	rec := Installed{ID: c.ID, Version: c.Version, Kind: c.Kind, Backend: BackendBase, Platform: m.platform, Dir: rel,
		InstalledAt: m.now().UTC().Format(time.RFC3339), SizeBytes: dirSize(dir),
		Exe: "runtime/" + b.Exe}
	if err := writeReceipt(dir, rec); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	m.mu.Lock()
	it.staging = ""
	m.mu.Unlock()
	return m.activate(t, it, rec)
}

// lockFile returns the path of the build's lock file with the manifest's
// SHA-256: next to the manifest, the built-in copy, the offline archive, or
// downloaded from its URL into components/manifest.d/.
func (m *Manager) lockFile(ctx context.Context, t *task, it *item, b *Build) (string, error) {
	l := b.Lock
	cands := []string{filepath.Join(t.man.dir, filepath.FromSlash(l.Path))}
	if m.builtin != t.man {
		cands = append(cands, filepath.Join(m.builtin.dir, filepath.FromSlash(l.Path)))
	}
	for _, p := range cands {
		if sum, _, err := sha256File(p, nil); err == nil && sum == l.SHA256 {
			return p, nil
		}
	}
	dst := filepath.Join(m.root, "manifest.d", filepath.FromSlash(l.Path))
	if it.offline != nil {
		if p, err := it.offline.extractLock(dst, l.SHA256); err == nil {
			return p, nil
		}
	}
	if l.URL != "" {
		urls, err := t.man.resolveURL(l.URL, t.src)
		if err == nil {
			spec := fileSpec{Name: l.Path, SHA256: l.SHA256, Size: -1, URLs: urls}
			if p, err := m.fetchSmall(ctx, spec); err == nil {
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err == nil && renameRetry(p, dst) == nil {
					return dst, nil
				}
			} else if c := context.Cause(ctx); c != nil {
				return "", c
			} else if CodeOf(err) == CodeChecksum {
				return "", err // twice the wrong content: say so, not "missing"
			}
		}
	}
	return "", cerr(CodeLockMissing, map[string]any{"id": it.id}, fmt.Errorf("lock file %s is missing or does not match", l.Path))
}

// fetchSmall downloads a small file of unknown size (a lock file) and
// checks its SHA-256; a mismatch is fetched once more from the same URL
// (V3.3 "删除并重试一次") before the next URL is tried.
func (m *Manager) fetchSmall(ctx context.Context, spec fileSpec) (string, error) {
	var lastErr error
	for _, u := range spec.URLs {
		for attempt := 0; attempt < 2; attempt++ {
			p, err := m.getSmall(ctx, u.URL, spec.SHA256)
			if err == nil {
				return p, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return "", context.Cause(ctx)
			}
			if CodeOf(err) != CodeChecksum {
				break
			}
			m.log.Warn("components: SHA-256 mismatch of a small file", "file", spec.Name, "url", redactURL(u.URL), "attempt", attempt+1)
		}
	}
	return "", lastErr
}

func (m *Manager) getSmall(ctx context.Context, url, sha string) (string, error) {
	data, err := httpGetLimited(ctx, m.client, url, 16<<20)
	if err != nil {
		return "", err
	}
	if got := sha256Hex(data); got != sha {
		return "", cerr(CodeChecksum, map[string]any{"file": path.Base(url)}, fmt.Errorf("SHA-256 %s, expected %s", got, sha))
	}
	tmp := filepath.Join(m.dl.dir, sha+".lock")
	if err := writeFileAtomic(tmp, data); err != nil {
		return "", asCoded(err, nil)
	}
	return tmp, nil
}

// checkImports runs the environment's interpreter on "import x, y" (the
// post-install check; components.verify_failed).
func (m *Manager) checkImports(ctx context.Context, t *task, it *item, python string, mods []string) error {
	if len(mods) == 0 {
		return nil
	}
	m.setPhase(t, it, PhaseChecking)
	cctx, cancel := context.WithTimeout(ctx, m.checkTimeout)
	defer cancel()
	// The telemetry packages are blocked first: `import openvino` alone
	// used to report to Google Analytics (pyenv).
	out, err := m.run(cctx, python, []string{"-I", "-c", pyenv.ImportCheck(mods)})
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		return &model.CodedError{Info: model.ErrorInfo{Code: CodeVerifyFailed, Params: map[string]any{"id": it.id},
			Detail: tail(out, 20)}, Err: err}
	}
	return nil
}
