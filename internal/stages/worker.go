// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lectureflow/internal/config"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/pyenv"
	"lectureflow/internal/sched"
)

// stderrTailBytes is how much of a failed process's stderr goes into
// job.error (SPEC §6.0).
const stderrTailBytes = 2048

// tailBuffer keeps the end of what is written to it; String returns the
// last n bytes (cut at a rune boundary).
type tailBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	// keep a few bytes more than n so String can move the cut to a rune boundary
	if extra := len(t.buf) - (t.n + utf8.UTFMax); extra > 0 {
		t.buf = append(t.buf[:0], t.buf[extra:]...)
	}
	return len(p), nil
}

// String returns the tail as valid UTF-8 (see utf8Tail).
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return utf8Tail(t.buf, t.n)
}

// utf8Tail returns at most the last n bytes of b as valid, trimmed UTF-8
// text: the cut is moved forward to a rune boundary (a character split by
// the cut is dropped, never half-kept) and any other invalid byte sequence
// (e.g. a console code page) is replaced by U+FFFD. Every place that
// truncates process output for job.error uses it (SPEC §6.0).
func utf8Tail(b []byte, n int) string {
	if n >= 0 && len(b) > n {
		b = b[len(b)-n:]
		for k := 0; k < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); k++ {
			b = b[1:]
		}
	}
	return strings.TrimSpace(strings.ToValidUTF8(string(b), "�"))
}

// executable resolves a configured program: a value containing a path
// separator is made absolute (it is relative to the working directory, while
// the process runs in the lecture directory); a bare name is looked up in PATH.
func executable(p string) (string, error) {
	if strings.ContainsAny(p, `/\`) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", err
		}
		return abs, nil
	}
	return exec.LookPath(p)
}

// notInstalledCode is the job error of a worker without an interpreter
// (3.1.0): the component its backend needs is not installed.
func notInstalledCode(workerCode string) string {
	switch workerCode {
	case workerASR:
		return CodeWorkerFWNotInstalled
	case workerASROV:
		return devices.CodeOVNotInstalled
	case workerASRMLX:
		return devices.CodeMLXNotInstalled
	}
	return CodeWorkerPythonNotInstalled
}

// workerCall is one Python worker invocation (SPEC §6.0).
type workerCall struct {
	name     string // asr | asr_openvino | asr_mlx | extract_pdf
	label    string // human-readable name for errors, e.g. "转写进程"
	code     string // prefix of the error codes: workerASR | workerASROV | workerASRMLX | workerPDF (codes.go)
	dir      string // working directory (lecture / material / syllabus dir)
	job      any    // written to {dir}/job.json
	out      string // result file name in dir, e.g. "result.json"
	progress func(float64)
	status   func(string) // "STATUS x" lines

	// The interpreter (SPEC-v2 I7; SPEC-v3 step 5): with python and
	// pythonKey both "" the config python (.venv). Otherwise python is used
	// as it is (even when empty, never falling back): the backend's
	// component environment or its configured path
	// (config.Config.WorkerPython); pythonCode is the code of a missing
	// one ("" = worker.python_missing; devices.ov_python_missing,
	// devices.mlx_python_missing), pythonKey the config key named in the
	// text ("" = "python"; its development venv, config.DevVenv, too). An
	// empty interpreter (no component, nothing configured: the desktop
	// application) fails with the "not installed" code of its worker
	// (notInstalledCode) instead.
	python     string
	pythonCode string
	pythonKey  string
	// errorCode maps the worker's "ERROR_CODE <name> [<json>]" line (brief
	// section 4: at most one, before a failed exit) to the job error's code
	// and params; code "" = unknown name (the generic {code}.worker_* error
	// stays). nil = such lines are only logged.
	errorCode func(name, raw string) (code string, params map[string]any)

	// limits are the process's OS limits (procutil.Start; security review
	// 2, M2: extract_pdf); zero = none. memoryErr, when set, is the error of
	// a run that went over limits.MemoryBytes (failed: non-zero exit), or
	// nil; it wins over the exit code and ERROR_CODE lines.
	limits    procutil.Limits
	memoryErr func(u procutil.Usage, failed bool) error
}

// runWorker runs {python} {worker_dir}/{name}.py --in {dir}/job.json
// --out {dir}/{out}.tmp with the working directory dir (SPEC §6.0; another
// free .tmp name when that one is still held: tmpFor). The process ends with
// LectureFlow, however LectureFlow ends (procutil EndWithParent). Every
// path is absolute, so nothing depends on the working directory. It parses
// PROGRESS / STATUS lines, renames {out}.tmp to {out} on exit code 0 and
// returns its content. Exit code 2 → sched.Permanent; other codes are
// retryable; the last 2 KB of stderr are part of the error. A failed run
// that printed an ERROR_CODE line known to w.errorCode gets that code
// (same exit-code rule). ctx cancel kills the process
// (exec.CommandContext), removes {out}.tmp and returns ctx.Err().
func (d Deps) runWorker(ctx context.Context, w workerCall) ([]byte, error) {
	dir, err := filepath.Abs(w.dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	pyPath, pyCode, pyKey := d.Cfg.Python, CodeWorkerPythonMissing, "python"
	if w.python != "" || w.pythonKey != "" {
		pyPath = w.python
		if w.pythonCode != "" {
			pyCode = w.pythonCode
		}
		if w.pythonKey != "" {
			pyKey = w.pythonKey
		}
	}
	if pyPath == "" {
		// The desktop application without the component (config
		// WorkerPython ""): not a path to look for, the component to install.
		code := notInstalledCode(w.code)
		return nil, sched.Permanent(model.Errorf(code, nil,
			fmt.Errorf("没有可用的 Python 解释器（%s）：请在“组件”中安装对应的组件", w.label)))
	}
	py, err := executable(pyPath)
	if err != nil {
		return nil, sched.Permanent(model.Errorf(pyCode, map[string]any{"path": pyPath},
			fmt.Errorf("找不到 Python 解释器 %q（config.toml 的 %s）：%v。请在“组件”中安装对应的组件，或按说明建立 %s 并在配置中填写解释器路径",
				pyPath, pyKey, err, config.DevVenv(pyKey))))
	}
	script, err := filepath.Abs(filepath.Join(d.Cfg.WorkerDir, w.name+".py"))
	if err == nil {
		_, err = os.Stat(script)
	}
	if err != nil {
		path := filepath.Join(d.Cfg.WorkerDir, w.name+".py")
		return nil, sched.Permanent(model.Errorf(CodeWorkerScriptMissing, map[string]any{"path": path},
			fmt.Errorf("找不到 worker 脚本 %s（config.toml 的 worker_dir）：%v", path, err)))
	}
	jb, err := json.MarshalIndent(w.job, "", "  ")
	if err != nil {
		return nil, err
	}
	jobPath := filepath.Join(dir, "job.json")
	if err := writeFileAtomic(jobPath, jb); err != nil {
		return nil, err
	}
	outPath := filepath.Join(dir, w.out)
	// Never a .tmp another program still writes (a worker left running by a
	// LectureFlow that was killed before workers were tied to it): tmpFor
	// (test-chaos 1).
	tmpPath, err := tmpFor(outPath)
	if err != nil {
		return nil, model.Errorf(w.code+workerError, nil, fmt.Errorf("%s的输出文件无法写入：%v", w.label, err))
	}

	// -s and the environment without pyenv.Dropped (pyenv "Isolation"), with
	// pyenv's switches (no usage reporting)
	cmd := exec.CommandContext(ctx, py, pyenv.ScriptArgs(script, "--in", jobPath, "--out", tmpPath)...)
	cmd.Dir = dir
	cmd.Env = pyenv.Environ("PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1")
	cmd.WaitDelay = 3 * time.Second
	tail := &tailBuffer{n: stderrTailBytes}
	cmd.Stderr = tail
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// The worker ends with LectureFlow, also when LectureFlow crashes or is
	// killed (test-chaos 1): an orphaned whisper kept the GPU busy and
	// wrote its .tmp on.
	limits := w.limits
	limits.EndWithParent = true
	proc, err := procutil.Start(cmd, limits)
	if err != nil {
		if ctx.Err() != nil { // killed while it was being put into its job
			return nil, ctx.Err()
		}
		return nil, sched.Permanent(model.Errorf(w.code+workerStartFailed, nil, fmt.Errorf("无法启动%s（%s）：%v", w.label, py, err)))
	}
	defer proc.Close()
	var errName, errRaw string // the last ERROR_CODE line
	scanLines(stdout, func(line string) {
		switch {
		case w.errorCode != nil && strings.HasPrefix(line, "ERROR_CODE "):
			rest := strings.TrimSpace(line[len("ERROR_CODE "):])
			errName, errRaw, _ = strings.Cut(rest, " ")
			errRaw = strings.TrimSpace(errRaw)
			slog.Info("worker", "worker", w.name, "dir", dir, "line", line)
		case strings.HasPrefix(line, "PROGRESS "):
			if v, err := strconv.ParseFloat(strings.TrimSpace(line[len("PROGRESS "):]), 64); err == nil && w.progress != nil {
				w.progress(v)
			}
		case strings.HasPrefix(line, "STATUS "):
			if w.status != nil {
				w.status(strings.TrimSpace(line[len("STATUS "):]))
			}
		default:
			slog.Info("worker", "worker", w.name, "dir", dir, "line", line)
		}
	})
	werr := cmd.Wait()
	if ctx.Err() != nil {
		os.Remove(tmpPath)
		return nil, ctx.Err()
	}
	if w.memoryErr != nil {
		if err := w.memoryErr(proc.Usage(), werr != nil); err != nil {
			os.Remove(tmpPath)
			return nil, err
		}
	}
	if werr != nil {
		os.Remove(tmpPath)
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code := ee.ExitCode()
			if errName != "" {
				if c, params := w.errorCode(errName, errRaw); c != "" {
					err := model.Errorf(c, params, fmt.Errorf("%s失败（退出码 %d，%s）：%s", w.label, code, errName, tail.String()))
					if code == 2 {
						return nil, sched.Permanent(err)
					}
					return nil, err
				}
			}
			if code == 2 {
				return nil, sched.Permanent(model.Errorf(w.code+workerInputError, nil,
					fmt.Errorf("%s报告输入错误（退出码 2，不会自动重试）：%s", w.label, tail.String())))
			}
			return nil, model.Errorf(w.code+workerFailed, map[string]any{"exit_code": code},
				fmt.Errorf("%s失败（退出码 %d）：%s", w.label, code, tail.String()))
		}
		return nil, model.Errorf(w.code+workerError, nil, fmt.Errorf("%s失败：%v：%s", w.label, werr, tail.String()))
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return nil, model.Errorf(w.code+workerNoOutput, nil, fmt.Errorf("%s退出码为 0 但没有写出 %s：%v", w.label, w.out+".tmp", err))
	}
	return os.ReadFile(outPath)
}

// scanLines calls fn for every line of r until EOF (lines up to 1 MB).
func scanLines(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		fn(strings.TrimRight(sc.Text(), "\r"))
	}
	// drain whatever is left (a line longer than the buffer) so the process never blocks
	_, _ = io.Copy(io.Discard, r)
}
