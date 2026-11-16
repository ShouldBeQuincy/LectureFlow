// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// The BitTorrent and Metalink options exist only in an aria2c built with
// them: the macOS build (GZip, HTTPS, Message Digest) refused them with
// "unrecognized option", exit 28, and every download failed (lead
// 2026-10-27).

var bitTorrentArgs = []string{"--follow-torrent=false", "--enable-dht=false", "--enable-dht6=false", "--bt-enable-lpd=false",
	"--enable-peer-exchange=false"}

func TestParseFeatures(t *testing.T) {
	cases := map[string]ariaFeatures{
		fakeVersion(featuresWindows):           {bitTorrent: true, metalink: true},
		fakeVersion(featuresMac):               {},
		fakeVersion("BitTorrent, GZip"):        {bitTorrent: true},
		fakeVersion("GZip, Metalink"):          {metalink: true},
		"Enabled Features: Metalink\r\n":       {metalink: true},
		"":                                     {},
		"aria2 version 1.37.0\nno features\n":  {},
		"Enabled Features: BitTorrentish, Foo": {},
	}
	for out, want := range cases {
		if got := parseFeatures(out); got != want {
			t.Errorf("%q: %+v, want %+v", out, got, want)
		}
	}
}

func TestAriaArgsFollowFeatures(t *testing.T) {
	for _, f := range []ariaFeatures{{}, {bitTorrent: true}, {metalink: true}, {bitTorrent: true, metalink: true}} {
		args := ariaArgs("c.conf", 40000, f)
		if !slices.Contains(args, "--no-netrc=true") || !slices.Contains(args, "--rpc-listen-port=40000") {
			t.Errorf("%+v: %v", f, args)
		}
		for _, a := range bitTorrentArgs {
			if slices.Contains(args, a) != f.bitTorrent {
				t.Errorf("%+v: %s present %v", f, a, !f.bitTorrent)
			}
		}
		if slices.Contains(args, "--follow-metalink=false") != f.metalink {
			t.Errorf("%+v: --follow-metalink", f)
		}
	}
}

// The aria2c of each build starts (a separate process, this test binary,
// playing the build: its --version and its refusal of unknown options);
// --version is read once per engine. Given the options its build lacks,
// the macOS build does not start: the regression.
func TestEngineFollowsBuildFeatures(t *testing.T) {
	for _, c := range []struct {
		name, features string
		bt             bool
	}{{"windows", featuresWindows, true}, {"macos", featuresMac, false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LF_FAKE_ARIA2C_FEATURES", c.features)
			eng := childEngine(t, io.Discard, nil)
			var probes atomic.Int32
			eng.versionOf = func(exe string) (string, error) {
				probes.Add(1)
				return runVersion(exe) // the child's own --version
			}
			eng.idle = 50 * time.Millisecond
			for round := 0; round < 2; round++ { // started, stopped when idle, started again
				in, err := eng.acquire(context.Background())
				if err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
				args := in.proc.(*execProcess).cmd.Args[1:]
				if slices.Contains(args, "--follow-torrent=false") != c.bt || slices.Contains(args, "--follow-metalink=false") != c.bt ||
					!slices.Contains(args, "--no-netrc=true") {
					t.Fatalf("args %v", args)
				}
				eng.release()
				select {
				case <-in.exited:
				case <-time.After(10 * time.Second):
					t.Fatal("aria2c did not stop when idle")
				}
			}
			if n := probes.Load(); n != 1 {
				t.Fatalf("--version read %d times", n)
			}
		})
	}
	t.Run("macos given every option", func(t *testing.T) {
		t.Setenv("LF_FAKE_ARIA2C_FEATURES", featuresMac)
		eng := childEngine(t, io.Discard, nil)
		eng.versionOf = func(string) (string, error) { return fakeVersion(featuresWindows), nil }
		if _, err := eng.acquire(context.Background()); !errors.Is(err, errExitedEarly) {
			t.Fatalf("the macOS build started with options it does not know: %v", err)
		}
	})
	// SEC19 low-4: without its features known aria2c is not started. Its
	// BitTorrent and Metalink options cannot be given to a build that lacks
	// them, and without them a build that has them follows a .torrent: the
	// peers, DHT and UDP trackers are not reached through the guard proxy.
	// The next start reads --version again (a virus scanner may have held
	// the first one up).
	t.Run("version unreadable", func(t *testing.T) {
		t.Setenv("LF_FAKE_ARIA2C_FEATURES", featuresWindows)
		eng := childEngine(t, io.Discard, nil)
		var launches, probes atomic.Int32
		launch := eng.launch
		eng.launch = func(exe string, args []string, out io.Writer) (Process, error) {
			launches.Add(1)
			return launch(exe, args, out)
		}
		readable := false
		for _, out := range []string{"", "aria2 version 1.37.0\n"} { // (an error; no features line)
			eng.versionOf = func(exe string) (string, error) {
				probes.Add(1)
				if readable {
					return runVersion(exe)
				}
				if out == "" {
					return "", errors.New("exit status 1")
				}
				return out, nil
			}
			_, err := eng.acquire(context.Background())
			if !errors.Is(err, errFeaturesUnknown) || !strings.Contains(err.Error(), "--version") {
				t.Fatalf("%q: %v", out, err)
			}
			if launches.Load() != 0 {
				t.Fatalf("%q: aria2c was started", out)
			}
			if eng.systemResolver() {
				t.Errorf("%q: the system's resolver without the features", out)
			}
		}
		readable = true
		in, err := eng.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer eng.release()
		if args := in.proc.(*execProcess).cmd.Args; !slices.Contains(args, "--follow-torrent=false") || !slices.Contains(args, "--follow-metalink=false") {
			t.Fatalf("args %v", args)
		}
		if n := probes.Load(); n != 3 || launches.Load() != 1 {
			t.Errorf("--version read %d times, aria2c started %d times", n, launches.Load())
		}
	})
}

// The task of a queue whose aria2c --version cannot be read fails with
// downloads.engine_failed, saying why, and nothing is downloaded.
func TestQueueAria2cFeaturesUnknown(t *testing.T) {
	e := newQEnv(t, func(o *Options) {
		o.ariaVersion = func(string) (string, error) { return "", errors.New("exit status 1") }
	})
	e.media.put("/files/a.m4a", pattern(1, 1000))
	info := e.wait(e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateFailed)
	if info.Error == nil || info.Error.Code != CodeEngineFailed {
		t.Fatalf("%+v", info.Error)
	}
	if e.aria.running() {
		t.Error("aria2c was started")
	}
	if adds, _, _, _, _ := e.aria.snapshot(); len(adds) != 0 {
		t.Errorf("aria2c was given %+v", adds)
	}
	if !strings.Contains(e.logs.String(), "--version") {
		t.Errorf("the log does not say why:\n%s", e.logs.String())
	}
}

// The whole queue on a macOS build: the download completes, with the
// options it has.
func TestAria2cOptionsMacBuild(t *testing.T) {
	e := newQEnv(t, func(o *Options) {
		o.ariaVersion = func(string) (string, error) { return fakeVersion(featuresMac), nil }
	})
	e.aria.noBitTorrent, e.aria.noMetalink = true, true
	e.media.put("/files/a.m4a", pattern(1, 1000))
	e.wait(e.enqueue(e.request("s1", e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
	_, args, _, _, _ := e.aria.snapshot()
	if !slices.Contains(args, "--no-netrc=true") || slices.Contains(args, "--follow-metalink=false") || slices.Contains(args, "--enable-dht=false") {
		t.Fatalf("args %v", args)
	}
}

// realAria2c finds the bundled aria2c: LF_ARIA2C, the staged
// desktop/build/resources/bin, tools/; "" when there is none.
func realAria2c() string {
	if p := os.Getenv("LF_ARIA2C"); p != "" {
		return p
	}
	name := "aria2c"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, p := range []string{filepath.Join("..", "..", "desktop", "build", "resources", "bin", name),
		filepath.Join("..", "..", "tools", name), filepath.Join("..", "..", "tools", "aria2c", name)} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			if a, err := filepath.Abs(p); err == nil {
				return a
			}
		}
	}
	return ""
}

// The bundled aria2c accepts every option the engine gives it (its own
// features read as in the product): an unknown option ends aria2c at once
// with exit 28. The download is a dry run to a port of this computer that
// refuses connections; nothing leaves the computer. Skipped where no
// bundled aria2c is staged (like the stages' tests with the real ffmpeg).
func TestRealAria2cAcceptsOurOptions(t *testing.T) {
	exe := realAria2c()
	if exe == "" {
		t.Skip("no bundled aria2c (stage the desktop resources or set LF_ARIA2C)")
	}
	eng := &engine{exe: exe, log: slog.New(slog.NewTextHandler(io.Discard, nil)), hc: &http.Client{}}
	feats, err := eng.features(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %+v", exe, feats)
	secret, _ := randomHex(24)
	conf, err := writeConf(secret)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(conf)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (code int, out string) {
		t.Helper()
		cmd := exec.Command(exe, args...)
		cmd.Dir = t.TempDir()
		hideWindow(cmd)
		var buf strings.Builder
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), buf.String()
			}
			return 0, buf.String()
		case <-time.After(3 * time.Second): // still running: the options were taken (RPC mode stays up)
			cmd.Process.Kill()
			<-done
			return -1, ""
		}
	}
	// the harness tells an unknown option
	if code, out := run("--lectureflow-no-such-option=1"); code != 28 {
		t.Fatalf("an unknown option: exit %d\n%s", code, out)
	}
	args := append(ariaArgs(conf, port, feats), "--dir="+t.TempDir(), "--dry-run=true", "http://127.0.0.1:1/smoke.mp4")
	if code, out := run(args...); code == 28 {
		t.Fatalf("aria2c refused an option of the engine (exit 28):\n%s\nargs %v", out, args)
	}
}
