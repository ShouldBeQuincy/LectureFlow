// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// aria2c runs only while there is work: it stops after the idle time and
// starts again (new port, new secret) for the next task.
func TestEngineIdleStopAndRestart(t *testing.T) {
	e := newQEnv(t, func(o *Options) { o.IdleTimeout = 100 * time.Millisecond })
	e.media.put("/files/a.m4a", pattern(1, 1000))
	if e.q.eng.running() || e.aria.running() {
		t.Fatal("aria2c runs before any task")
	}
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id, bridge.StateDone)
	e.waitFor("aria2c stopped when idle", func() bool { return !e.q.eng.running() && !e.aria.running() })
	_, args1, conf1, _, shutdowns := e.aria.snapshot()
	if shutdowns != 1 {
		t.Fatalf("%d shutdowns", shutdowns)
	}
	id2 := e.enqueue(e.request("s2", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	e.wait(id2, bridge.StateDone)
	_, args2, conf2, _, _ := e.aria.snapshot()
	if argValue(args1, "rpc-listen-port") == "" || conf1 == conf2 || argValue(args1, "conf-path") == argValue(args2, "conf-path") {
		t.Fatalf("the second start reused the secret or the conf file:\n%v\n%v", args1, args2)
	}
}

// The process manager with a real child process (this test binary acting
// as aria2c): the conf file is gone once the RPC answers, the idle stop
// ends the process.
func TestEngineChildProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LF_FAKE_ARIA2C", "1")
	eng := &engine{exe: exe, launch: execLauncher, idle: 200 * time.Millisecond,
		hc: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}, log: slog.Default()}
	defer eng.close()
	ctx := context.Background()
	inst, err := eng.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ep := inst.proc.(*execProcess)
	args := ep.cmd.Args[1:]
	conf := argValue(args, "conf-path")
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Fatalf("conf file %s still there: %v", conf, err)
	}
	for _, a := range args {
		if strings.Contains(a, inst.rpc.token[len("token:"):]) {
			t.Fatalf("the secret is on the command line: %s", a)
		}
	}
	if v, err := inst.rpc.version(ctx); err != nil || !strings.Contains(v, "fake") {
		t.Fatalf("version %q %v", v, err)
	}
	// a wrong secret is refused
	bad := newRPC(0, "wrong", eng.hc)
	bad.url = inst.rpc.url
	if _, err := bad.version(ctx); err == nil {
		t.Fatal("a wrong secret was accepted")
	}
	// a second user shares the process
	inst2, err := eng.acquire(ctx)
	if err != nil || inst2 != inst {
		t.Fatalf("second acquire %v", err)
	}
	eng.release()
	eng.release()
	select {
	case <-inst.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("aria2c did not stop when idle")
	}
	if eng.running() {
		t.Fatal("engine still reports running")
	}
}

func TestEngineMissing(t *testing.T) {
	e := newQEnv(t, func(o *Options) {
		o.Aria2c = filepath.Join(t.TempDir(), "no-aria2c.exe")
		o.Launcher = nil
	})
	e.media.put("/files/a.m4a", pattern(1, 1000))
	id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a")))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeEngineMissing || !e.q.hasCredsOf(id) {
		t.Fatalf("%+v", info)
	}
}

func (q *Queue) hasCredsOf(id int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t := q.tasks[id]
	return t != nil && t.hasCreds()
}

// Not enough room for the data directory's reserve: failed, retryable.
// Nothing downloaded yet, so no_space itself, without task_bytes (not
// no_space_kept: removing the task would free nothing).
func TestNoSpace(t *testing.T) {
	e := newQEnv(t, func(o *Options) {
		o.MinFree = 2 << 30
		o.FreeSpace = func(string) (int64, error) { return 1 << 30, nil }
	})
	tr := e.fileTrack(bridge.KindAudio, "/files/a.m4a")
	tr.SizeBytes = 100
	id := e.enqueue(e.request("s1", tr))
	info := e.wait(id, bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeNoSpace || info.Error.Params["free_bytes"] != float64(1<<30) ||
		info.Error.Params["keep_bytes"] != float64(2<<30) || info.Error.Params["task_bytes"] != nil {
		t.Fatalf("%+v", info.Error)
	}
}

// All cookies expired before the task started: needs a refresh at once.
func TestExpiredCookies(t *testing.T) {
	e := newQEnv(t, nil)
	req := e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))
	req.Cookies = []bridge.Cookie{{Name: "sid", Value: testCookie, Domain: "127.0.0.1", Path: "/", Expires: time.Now().Unix() - 10}}
	id := e.enqueue(req)
	info := e.wait(id, bridge.StateNeedsRefresh)
	if info.Error == nil || info.Error.Code != CodeCredentialsExpired {
		t.Fatalf("%+v", info.Error)
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Fatalf("downloaded with expired cookies: %+v", adds)
	}
}
