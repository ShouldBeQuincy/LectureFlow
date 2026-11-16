// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/secrets"
)

func shortLockWait(t *testing.T, d time.Duration) {
	old := lockWait
	lockWait = d
	t.Cleanup(func() { lockWait = old })
}

// The lock is exclusive between two holders (two open files, in this
// process as in two), names the holder's process id, and is free again
// once released.
func TestInstanceLockExclusive(t *testing.T) {
	shortLockWait(t, 300*time.Millisecond)
	dir := t.TempDir()
	a, err := acquireInstanceLock(dir)
	if err != nil || a == nil {
		t.Fatalf("first: %v", err)
	}
	if pid := readLockPID(filepath.Join(dir, LockFileName)); pid != os.Getpid() {
		t.Fatalf("pid %d", pid)
	}
	t0 := time.Now()
	_, err = acquireInstanceLock(dir)
	var sf *startFailure
	if !errors.As(err, &sf) || sf.Code != failDataDirInUse || sf.Params["pid"] != os.Getpid() || sf.Params["data_dir"] != dir {
		t.Fatalf("second: %v", err)
	}
	if time.Since(t0) < 250*time.Millisecond {
		t.Fatal("did not wait for the lock")
	}
	if !strings.Contains(sf.Msg, fmt.Sprintf("进程 %d", os.Getpid())) || !strings.Contains(sf.Msg, "is using this data directory") {
		t.Fatalf("message %q", sf.Msg)
	}
	// the file can still be read (and copied) while it is held
	if _, err := os.ReadFile(filepath.Join(dir, LockFileName)); err != nil {
		t.Fatalf("reading the held lock file: %v", err)
	}
	a.Release()
	a.Release() // twice is fine
	b, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	b.Release()
	var none *instanceLock
	none.Release()
}

// A lock released while another start waits: that start gets it (the
// restart after exit code 3).
func TestInstanceLockWaitsForRelease(t *testing.T) {
	shortLockWait(t, 5*time.Second)
	dir := t.TempDir()
	a, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.Release()
	}()
	b, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("waiting start: %v", err)
	}
	b.Release()
}

// TestInstanceLockHelperProcess is the other process of
// TestInstanceLockReleasedWhenProcessEnds (not a test by itself).
func TestInstanceLockHelperProcess(t *testing.T) {
	dir := os.Getenv("LF_LOCK_HELPER_DIR")
	if dir == "" {
		return
	}
	l, err := acquireInstanceLock(dir)
	if err != nil || l == nil {
		fmt.Println("failed", err)
		os.Exit(2)
	}
	fmt.Println("locked")
	bufio.NewReader(os.Stdin).ReadString('\n') // until killed
	// l is not used after the check above: without this the collector may
	// finalize its *os.File, closing the handle and so ending the lock
	// while the other process still expects it held (the flaky refusal
	// that came back nil under load).
	runtime.KeepAlive(l)
	os.Exit(0)
}

// The system releases the lock of a process that ends without releasing
// it (killed): the next start gets it; while it runs, the refusal names its
// process id.
func TestInstanceLockReleasedWhenProcessEnds(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceLockHelperProcess$")
	cmd.Env = append(os.Environ(), "LF_LOCK_HELPER_DIR="+dir)
	stdin, _ := cmd.StdinPipe()
	defer stdin.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper: %q", line)
	}
	shortLockWait(t, 200*time.Millisecond)
	stray, err := acquireInstanceLock(dir)
	var sf *startFailure
	if !errors.As(err, &sf) || sf.Params["pid"] != cmd.Process.Pid {
		stray.Release() // so the temporary directory can still be removed
		t.Fatalf("while the other process holds it: %v (helper pid %d)", err, cmd.Process.Pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	shortLockWait(t, 5*time.Second)
	l, err := acquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("after the holder was killed: %v", err)
	}
	if pid := readLockPID(filepath.Join(dir, LockFileName)); pid != os.Getpid() {
		t.Fatalf("pid %d after taking over", pid)
	}
	l.Release()
}

// F4: a second service on the same data directory is refused before it
// touches anything (desktop mode: the code line with the holder's pid); the
// first keeps running; once it has stopped, a new start works. The update
// check (-selfupdate-verify) is not affected by the lock.
func TestSecondInstanceRefused(t *testing.T) {
	t.Cleanup(secrets.ResetForTest)
	shortLockWait(t, 300*time.Millisecond)
	cfgPath, dataDir := writeTestConfig(t)
	a := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	url := a.ready()
	<-a.app

	b := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0", "-desktop"}, testToken+"\n", AppOptions{})
	if c := b.wait(15 * time.Second); c != exitFailure {
		t.Fatalf("second instance exit %d", c)
	}
	code, params, last := failureOf(t, b.stderr.String())
	if code != failDataDirInUse || params["pid"].(float64) != float64(os.Getpid()) || params["data_dir"] != filepath.Clean(dataDir) {
		t.Fatalf("failure %q %v", code, params)
	}
	if !strings.Contains(last, "正在使用这个数据目录") || !strings.Contains(last, strconv.Itoa(os.Getpid())) {
		t.Fatalf("ERROR line %q", last)
	}
	if strings.Contains(b.stdout(), ReadyPrefix) {
		t.Fatal("the second instance became ready")
	}
	if c := get(t, url+"/api/courses", nil); c != 200 {
		t.Fatalf("the first instance: %d", c)
	}

	upd := filepath.Join(dataDir, "updates")
	os.MkdirAll(upd, 0o755)
	vcode, vout := runVerify(t, "-selfupdate-verify", upd, "-data-dir", dataDir, "-app-version", "3.1.0")
	if vout == "" || strings.Contains(vout, "data directory") || vcode == exitFailure && strings.Contains(vout, "LectureFlow") {
		t.Fatalf("-selfupdate-verify while the lock is held: %d %q", vcode, vout)
	}

	a.sigc <- os.Interrupt
	if c := a.wait(15 * time.Second); c != exitOK {
		t.Fatalf("first instance exit %d", c)
	}
	secrets.ResetForTest()
	c := startProc(t, []string{"-config", cfgPath, "-listen", "127.0.0.1:0"}, "", AppOptions{})
	c.ready()
	<-c.app
}
