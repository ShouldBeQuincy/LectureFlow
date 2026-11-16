// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
)

// Tool components (KindTool, 3.1.0): a vendor's program downloaded from its
// vendor on the user's computer, today the Google Cloud CLI (gcloud) that
// signs in to Google for Colab. Layout:
//
//	{dir}/{version}/tool/            the archive's contents (File.Strip drops google-cloud-sdk/)
//	{dir}/{version}/installed.json   the receipt (Exe = "tool/" + Build.Exe, Env = Build.Env)
//
// The archive is unpacked into the staging directory's ".u" first and
// renamed to "tool" once complete, so a pause or a crash during the unpack
// never leaves a tool directory that looks whole. The post-install check
// runs Exe with Component.CheckArgs (gcloud --version) and Build.Env plus
// Component.CheckEnv; it replaces the worker self-test of an update (V8):
// an update whose check fails keeps the installed version.

// unpackTmp is the staging subdirectory a tool archive is unpacked into
// (renamed to "tool" when complete); short: Windows path length.
const unpackTmp = ".u"

// installTool installs a tool component.
func (m *Manager) installTool(ctx context.Context, t *task, it *item, c *Component, b *Build) error {
	staging, err := m.stagingDir(t, it)
	if err != nil {
		return err
	}
	f := b.Files[0]
	toolDir := filepath.Join(staging, "tool")
	exe := filepath.Join(toolDir, filepath.FromSlash(b.Exe))
	if !dirExists(toolDir) { // not unpacked completely before a pause
		p, err := m.obtain(ctx, t, it, f)
		if err != nil {
			return err
		}
		m.setPhase(t, it, PhaseUnpacking)
		tmp := filepath.Join(staging, unpackTmp)
		if err := removeAllRetry(tmp); err != nil {
			return asCoded(err, nil)
		}
		if err := unpack(ctx, p, f.Unpack, fileLayout(f), tmp); err != nil {
			return err
		}
		if !fileExists(filepath.Join(tmp, filepath.FromSlash(b.Exe))) {
			return cerr(CodeUnpackFailed, map[string]any{"file": f.Name}, fmt.Errorf("the archive has no %s", b.Exe))
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := renameRetry(tmp, toolDir); err != nil {
			return cerr(CodeIO, nil, err)
		}
	}
	if !fileExists(exe) {
		return cerr(CodeUnpackFailed, map[string]any{"file": f.Name}, fmt.Errorf("the archive has no %s", b.Exe))
	}
	m.fileDone(t, it, f.Size)
	if err := m.checkTool(ctx, t, it, c, b, staging, toolDir); err != nil {
		return err
	}
	// the archive stays until the check passed: a retry after a failed
	// check unpacks it again without downloading it again
	os.Remove(m.dl.finalPath(f.SHA256))
	removeIfEmpty(m.dl.dir)
	rec := Installed{Exe: "tool/" + b.Exe, Env: copyEnv(b.Env)}
	req := SelfTestRequest{ID: c.ID, Version: c.Version, Kind: c.Kind, Dir: staging}
	return m.finish(ctx, t, it, c, staging, rec, req)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func copyEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// checkTool runs the tool's post-install check (components.verify_failed
// with the output's last lines when it fails).
func (m *Manager) checkTool(ctx context.Context, t *task, it *item, c *Component, b *Build, staging, toolDir string) error {
	if len(c.CheckArgs) == 0 {
		return nil
	}
	m.setPhase(t, it, PhaseChecking)
	tmp := filepath.Join(staging, ".chk")
	if err := removeAllRetry(tmp); err != nil {
		return asCoded(err, nil)
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return asCoded(err, nil)
	}
	defer func() {
		if err := removeAllRetry(tmp); err != nil {
			m.log.Warn("components: cannot remove the check's temporary directory", "dir", tmp, "error", err)
		}
	}()
	m.mu.Lock()
	vars := m.resolveEnv(b.Env, toolDir, "", m.st)
	vars = append(vars, m.resolveEnv(c.CheckEnv, toolDir, tmp, m.st)...)
	m.mu.Unlock()
	exe := filepath.Join(toolDir, filepath.FromSlash(b.Exe))
	cctx, cancel := context.WithTimeout(ctx, m.checkTimeout)
	defer cancel()
	out, err := m.runTool(cctx, exe, c.CheckArgs, vars)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (no answer within %s)", err, m.checkTimeout)
		}
		m.log.Warn("components: the tool's check failed", "id", c.ID, "error", err)
		return &model.CodedError{Info: model.ErrorInfo{Code: CodeVerifyFailed, Params: map[string]any{"id": it.id},
			Detail: tail(out, 20)}, Err: err}
	}
	return nil
}

// runTool runs a tool with the process environment minus the variables
// that would redirect its Python (ToolBaseEnv), plus vars. Its working
// directory is a new empty directory only this account can open, made
// below the system's temporary directory for each run and removed
// afterwards. Not the temporary directory itself: cmd.exe (gcloud.cmd)
// looks for the programs a script calls in the working directory first,
// so a file someone put there would run (security audit L-13). Not inside
// the data directory: on Windows, ending gcloud.cmd leaves the programs it
// started running for a moment, which would keep a working directory there
// from being moved or removed meanwhile. Every path the tool gets is
// absolute (exe, the {dir} / {tmp} / {exe:ID} variables).
func (m *Manager) runTool(ctx context.Context, exe string, args, vars []string) (string, error) {
	wd, err := dirperm.MkdirTempPrivate(os.TempDir(), "lectureflow-tool-")
	if err != nil {
		return "", asCoded(err, nil)
	}
	defer func() {
		if err := removeAllRetry(wd); err != nil {
			m.log.Warn("components: cannot remove the tool's working directory", "dir", wd, "error", err)
		}
	}()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = append(ToolBaseEnv(os.Environ()), vars...)
	cmd.Dir = wd
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	err = cmd.Run()
	return out.String(), err
}

// ToolBaseEnv returns environ without the variables that would make a
// tool's Python load other code (PYTHONHOME, PYTHONPATH, PYTHONSTARTUP,
// PYTHONUSERBASE, an active virtualenv or conda environment). Run a tool
// with ToolBaseEnv(os.Environ()) plus ToolEnv.
func ToolBaseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && k != "" {
			switch strings.ToUpper(k) {
			case "PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONUSERBASE", "VIRTUAL_ENV", "CONDA_PREFIX":
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}

// resolveEnv expands Build.Env / CheckEnv templates: {dir} the tool
// directory, {exe:ID} the executable of the installed component ID, {tmp}
// the check's temporary directory. A variable whose component is not
// installed is left out. Sorted "KEY=value" entries. Caller holds m.mu.
func (m *Manager) resolveEnv(env map[string]string, toolDir, tmp string, st *state) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		v, ok := m.expandEnv(env[k], toolDir, tmp, st)
		if !ok {
			m.log.Warn("components: a tool variable names a component that is not installed", "var", k, "value", env[k])
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// envUses reports whether a tool's variables name component id ({exe:id}):
// an installed tool the manifest no longer lists still needs it.
func envUses(env map[string]string, id string) bool {
	for _, v := range env {
		if strings.Contains(v, "{exe:"+id+"}") {
			return true
		}
	}
	return false
}

func (m *Manager) expandEnv(v, toolDir, tmp string, st *state) (string, bool) {
	isPath := false
	ok := true
	res := envTmplRe.ReplaceAllStringFunc(v, func(tm string) string {
		inner := strings.Trim(tm, "{}")
		switch {
		case inner == "dir":
			isPath = true
			return toolDir
		case inner == "tmp" && tmp != "":
			return tmp
		case strings.HasPrefix(inner, "exe:"):
			isPath = true
			e, found := st.Active[strings.TrimPrefix(inner, "exe:")]
			p, good := "", false
			if found {
				p, good = m.exePath(e)
			}
			if !good {
				ok = false
				return tm
			}
			return p
		}
		ok = false
		return tm
	})
	if !ok {
		return "", false
	}
	if isPath {
		res = filepath.FromSlash(res)
	}
	return res, true
}

// ToolPath returns the executable of an installed tool component
// (gcloud: …\components\gcloud\{version}\tool\bin\gcloud.cmd on Windows,
// …/components/gcloud/{version}/tool/bin/gcloud on macOS); false when it is
// not installed or the file is missing.
func (m *Manager) ToolPath(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.st.Active[id]
	if !ok || e.Kind != KindTool {
		return "", false
	}
	p, ok := m.exePath(e)
	if !ok || !fileExists(p) {
		return "", false
	}
	return p, true
}

// ToolEnv returns the variables ("KEY=value") an installed tool runs with,
// to be appended to ToolBaseEnv(os.Environ()) (os/exec keeps the last
// value of a repeated key, so they override the user's own). gcloud: the
// update check is off (the component manager updates it), on Windows it
// uses its bundled Python, on macOS the python component's interpreter
// (CLOUDSDK_PYTHON). nil when the tool is not installed.
func (m *Manager) ToolEnv(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.st.Active[id]
	if !ok || e.Kind != KindTool {
		return nil
	}
	toolDir := filepath.Join(m.abs(e.Dir), "tool")
	return m.resolveEnv(e.Env, toolDir, "", m.st)
}
