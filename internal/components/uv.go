// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package components

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/pyenv"
)

// run executes a program with the sanitized environment and returns its
// combined output.
func (m *Manager) run(ctx context.Context, prog string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Env = m.env()
	cmd.Dir = m.root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 5 * time.Second
	hideWindow(cmd)
	// uv ends with LectureFlow, also on a crash: an orphaned install would
	// go on writing into the component's directory (procutil.Tied;
	// test-chaos 1).
	err := procutil.Run(cmd, procutil.Tied)
	return out.String(), err
}

// env is the process environment without variables that would redirect uv
// or python (a user's UV_INDEX_URL, PYTHONPATH, an active virtualenv), plus
// uv's cache and switches and pyenv's (no usage reporting: the check runs
// the component's python).
func (m *Manager) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			env = append(env, kv) // Windows "=C:=C:\..." entries
			continue
		}
		k := strings.ToUpper(kv[:i])
		switch {
		case strings.HasPrefix(k, "UV_"), strings.HasPrefix(k, "PIP_"),
			k == "VIRTUAL_ENV", k == "CONDA_PREFIX", k == "PYTHONHOME", k == "PYTHONPATH",
			k == "PYTHONSTARTUP", k == "PYTHONUSERBASE", k == "PYTHONNOUSERSITE":
			continue
		}
		env = append(env, kv)
	}
	return pyenv.Env(env,
		"UV_CACHE_DIR="+m.uvCache(),
		"UV_NO_PROGRESS=1",
		"UV_PYTHON_DOWNLOADS=never",
		"PYTHONUTF8=1",
	)
}

func (m *Manager) uvAvailable() error {
	if m.uvPath == "" || !fileExists(m.uvPath) {
		return cerr(CodeUVMissing, nil, fmt.Errorf("uv not found at %q", m.uvPath))
	}
	return nil
}

// buildVenv creates venv with the interpreter pyExe (unless a paused task
// created it already) and installs the lock file with --require-hashes:
// from the offline archive's wheels, or from the chosen package index with
// a fallback to the default one. uv cannot resume a single wheel download,
// but its cache keeps every finished wheel, so a retry only fetches what is
// missing.
func (m *Manager) buildVenv(ctx context.Context, t *task, it *item, pyExe, venv, lock string) error {
	if err := m.uvAvailable(); err != nil {
		return err
	}
	m.setPhase(t, it, PhaseInstalling)
	if !fileExists(filepath.Join(venv, "pyvenv.cfg")) || !fileExists(venvPython(venv)) {
		removeAllRetry(venv)
		out, err := m.run(ctx, m.uvPath, []string{"venv", "--no-config", "--no-project", "--python", pyExe, venv})
		if err != nil {
			if c := context.Cause(ctx); c != nil {
				return c
			}
			return uvError("venv", out, err)
		}
	}
	// --no-build: wheels only. The lock file also lists the hashes of the
	// source distributions; an index that offers only those would make uv
	// build them with build dependencies (setuptools, ...) that are not in
	// the lock file and not checked against any hash. Every lock is compiled
	// with --only-binary :all: (refresh-locks.ps1), so each package has a
	// wheel for its platform.
	sync := []string{"pip", "sync", "--no-config", "--python", venvPython(venv), "--require-hashes",
		"--no-build", "--link-mode", "copy", "--system-certs"}
	if it.offline != nil {
		wheels, err := it.offline.extractWheels(ctx, filepath.Join(filepath.Dir(venv), "wheels"))
		if err != nil {
			return err
		}
		out, err := m.run(ctx, m.uvPath, append(sync, "--offline", "--no-index", "--find-links", wheels, lock))
		removeAllRetry(wheels)
		if err != nil {
			if c := context.Cause(ctx); c != nil {
				return c
			}
			return uvError("sync", out, err)
		}
		return nil
	}
	urls, err := t.man.indexURLs(t.src)
	if err != nil {
		if CodeOf(err) != "" {
			return err
		}
		return cerr(CodeManifestInvalid, nil, err)
	}
	for i, u := range urls {
		m.setSource(t, it, u)
		var e error
		// a wheel whose hash does not match is deleted from uv's cache and
		// the same index is tried once more (V3.3 "删除并重试一次"), also
		// when it is the last or only index
		for attempt := 0; attempt < 2; attempt++ {
			out, err := m.run(ctx, m.uvPath, append(append([]string(nil), sync...), "--index-url", u.URL, lock))
			if err == nil {
				return nil
			}
			if c := context.Cause(ctx); c != nil {
				return c
			}
			e = uvError("sync", out, err)
			if CodeOf(e) != CodeChecksum || attempt == 1 {
				break
			}
			m.log.Warn("components: a package does not match its hash; clearing it from uv's cache and retrying",
				"index", redactURL(u.URL), "error", tail(out, 8))
			m.forgetCached(ctx, out)
		}
		if CodeOf(e) == CodeDiskFull || i == len(urls)-1 {
			return e
		}
		m.log.Warn("components: package index failed; trying the next one", "index", redactURL(u.URL), "error", ErrorInfoOf(e).Detail)
		m.notice(t, model.ErrorInfo{Code: CodeMirrorFallback,
			Params: map[string]any{"group": u.Group, "mirror": redactURL(u.Mirror), "reason": CodeOf(e)}})
	}
	return nil
}

// hashMismatchRe finds the packages uv reports with a wrong hash
// ("Hash mismatch for `numpy==2.1.0`").
var hashMismatchRe = regexp.MustCompile("(?i)hash mismatch for `([A-Za-z0-9][A-Za-z0-9._-]*)")

// forgetCached removes the packages uv reported with a wrong hash from its
// cache ("uv cache clean <pkg>"); when uv named none, or that fails, the
// whole cache goes (it belongs to the components manager and only the
// running task uses it).
func (m *Manager) forgetCached(ctx context.Context, out string) {
	seen := map[string]bool{}
	var pkgs []string
	for _, mm := range hashMismatchRe.FindAllStringSubmatch(out, -1) {
		if n := strings.ToLower(mm[1]); !seen[n] {
			seen[n] = true
			pkgs = append(pkgs, n)
		}
	}
	if len(pkgs) > 0 {
		res, err := m.run(ctx, m.uvPath, append([]string{"cache", "clean", "--no-config"}, pkgs...))
		if err == nil {
			return
		}
		m.log.Warn("components: uv cache clean failed; removing the whole cache", "error", tail(res, 5))
	}
	removeAllRetry(m.uvCache())
}

// uvError classifies uv's failure by its output.
func uvError(step, out string, err error) error {
	low := strings.ToLower(out)
	code := CodeUVFailed
	switch {
	case strings.Contains(low, "no space left") || strings.Contains(low, "not enough space") ||
		strings.Contains(low, "os error 112") || strings.Contains(low, "os error 28") || strings.Contains(low, "disk full"):
		code = CodeDiskFull
	case strings.Contains(low, "hash mismatch"):
		code = CodeChecksum
	case strings.Contains(low, "failed to fetch") || strings.Contains(low, "error sending request") ||
		strings.Contains(low, "dns error") || strings.Contains(low, "connection") ||
		strings.Contains(low, "timed out") || strings.Contains(low, "tcp connect") ||
		strings.Contains(low, "network"):
		code = CodeNetwork
	}
	params := map[string]any{"step": step}
	if code == CodeDiskFull {
		params = map[string]any{"required_mb": -1, "free_mb": -1}
	}
	return &model.CodedError{Info: model.ErrorInfo{Code: code, Params: params, Detail: tail(out, 30)},
		Err: fmt.Errorf("uv %s: %w", step, err)}
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// httpGetLimited GETs a small document (a lock file; with the redirect
// chain's state, download.go).
func httpGetLimited(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(withRedirectChain(ctx), http.MethodGet, url, nil)
	if err != nil {
		return nil, cerr(CodeNetwork, nil, err)
	}
	req.Header.Set("User-Agent", "LectureFlow-components")
	resp, err := c.Do(req)
	if err != nil {
		return nil, asCoded(err, nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, cerr(CodeHTTPStatus, map[string]any{"status": resp.StatusCode}, &httpStatusError{resp.StatusCode})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, asCoded(err, nil)
	}
	if int64(len(data)) > limit {
		return nil, cerr(CodeIO, nil, errors.New("document too large"))
	}
	return data, nil
}
