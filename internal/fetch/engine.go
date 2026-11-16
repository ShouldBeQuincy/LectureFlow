// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lectureflow/internal/procutil"
)

// The aria2c process (ext-brief §6.2): started when a task needs it, in RPC
// mode only, listening on a random port of the loopback interface with a
// random secret (--rpc-listen-all=false binds both 127.0.0.1 and [::1];
// the client uses 127.0.0.1; --disable-ipv6 is not set, it would also
// keep IPv6 download sources away); stopped after IdleTimeout without
// users. The secret reaches aria2c in a temporary --conf-path file (0600)
// that is deleted as soon as the RPC answers; the file also keeps aria2c
// from reading the user's own aria2 configuration. Nothing secret is on
// its command line or in its environment, it writes no log and no session
// file, and --stop-with-process ends it with this process. The secret goes
// to 127.0.0.1:P only once the system shows aria2c listening there
// (listener.go, security review H1).
//
// aria2c reads none of the user's files and opens nothing it was not
// asked for (security review, NOTES): --no-netrc keeps the credentials of
// ~/.netrc from going to a host that matches it; a response that is a
// Metalink or a .torrent is not followed (aria2c would fetch the hosts a
// Metalink lists, with the task's Cookie and Referer, or join BitTorrent
// and listen on 6881-6999), and DHT, local peer discovery and peer
// exchange stay off. --load-cookies, --netrc-path, --input-file,
// --save-session, --log and --dht-file-path are not set; the CA
// certificates are the system's (--ca-certificate is not set).
//
// Those BitTorrent and Metalink options exist only in an aria2c built with
// them: the official Windows build has both, the macOS build of
// packaging/aria2c neither (Enabled Features: GZip, HTTPS, Message Digest),
// and it refuses an option it does not know ("unrecognized option", exit
// 28: every download failed, lead 2026-10-27). So "aria2c --version" is
// read once per engine (ariaFeatures) and each group is passed only when
// its feature is there; a build without the feature cannot follow a
// torrent or a Metalink anyway, and followedBy stays the last check.
// When --version cannot be read aria2c is not started at all
// (errFeaturesUnknown, SEC19 low-4): BitTorrent's peers, DHT and UDP
// trackers would not go through the guard proxy.
// --async-dns=false likewise only for a build with Async DNS
// (resolverArgs): names are resolved by the system, as Go resolves them.
//
// aria2c connects to no server itself: each aria2c process has a guard
// proxy of LectureFlow's (guard.go), started before it and stopped with
// it, which is its proxy for every protocol (guardArgs on the command
// line, each download's session options over the RPC) and makes every
// connection in its place, judged by the rules of local.go. The proxy
// variables are taken out of its environment (launchEnv); the user's
// proxy is the guard's to use. With the guard aria2c resolves no name of
// a download; --async-dns=false stays for a request that would get past
// it all the same.

// Process is a started aria2c.
type Process interface {
	// Wait blocks until the process has exited.
	Wait() error
	Kill() error
	// Pid is its process id: the RPC port must be this process's
	// (listener.go).
	Pid() int
}

// Launcher starts aria2c (exe with args) with its stdout and stderr going
// to out. The default runs the program (hidden window on Windows); tests
// start a fake.
type Launcher func(exe string, args []string, out io.Writer) (Process, error)

// maxStarts bounds the starts of one acquire: another one follows only when
// aria2c exited at once or its RPC port was taken (a new random port).
const maxStarts = 5

// maxActive is aria2's --max-concurrent-downloads: the queue keeps the
// real limits itself (tasks × tracks × connections ≤ 6 × 2 × 16).
const maxActive = 6 * 2 * 16

// errEngineMissing: the aria2c executable does not exist.
var errEngineMissing = errors.New("aria2c not found")

type engine struct {
	exe    string
	launch Launcher
	idle   time.Duration
	hc     *http.Client
	log    *slog.Logger
	// startTimeout bounds the wait for the first RPC answer.
	startTimeout time.Duration
	// pickPort chooses the RPC port (default freePort; tests).
	pickPort func() (int, error)
	// versionOf runs "exe --version" (default runVersion; tests).
	versionOf func(exe string) (string, error)
	featMu    sync.Mutex
	featKnown bool // feats and asyncDNS were read (once per engine)
	feats     ariaFeatures
	asyncDNS  bool // built with Async DNS (c-ares): resolverArgs
	// sysResolver: aria2c resolves names as Go does, with the system's
	// resolver (its features were read: --async-dns=false given, or no
	// Async DNS built in); false until then (systemResolver)
	sysResolver atomic.Bool

	mu     sync.Mutex
	inst   *instance
	users  int
	timer  *time.Timer
	closed bool
}

type instance struct {
	rpc    *rpcClient
	proc   Process
	exited chan struct{}
	tr     *http.Transport // the RPC client's connections (closed once it stopped)
	// guard is this aria2c's proxy to every server (guard.go): started
	// before the process, closed when it has exited.
	guard *guard
}

func (in *instance) alive() bool {
	select {
	case <-in.exited:
		return false
	default:
		return true
	}
}

// acquire returns the running aria2c, starting it when needed. Every
// successful acquire must be paired with release.
func (e *engine) acquire(ctx context.Context) (*instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if e.inst != nil && !e.inst.alive() {
		e.inst = nil
	}
	if e.inst == nil {
		var err error
		for attempt := 0; attempt < maxStarts; attempt++ {
			// another attempt only for a lost port race (aria2c exited) or
			// a port another program holds
			if e.inst, err = e.start(ctx); err == nil || !(errors.Is(err, errExitedEarly) || errors.Is(err, errPortTaken)) {
				break
			}
		}
		if err != nil {
			e.inst = nil
			if e.users == 0 {
				e.armIdleLocked()
			}
			return nil, err
		}
	}
	e.users++
	return e.inst, nil
}

// release ends one use; the last one arms the idle timer.
func (e *engine) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.users > 0 {
		e.users--
	}
	if e.users == 0 {
		e.armIdleLocked()
	}
}

func (e *engine) armIdleLocked() {
	if e.closed || e.inst == nil {
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(e.idle, e.idleStop)
}

func (e *engine) idleStop() {
	e.mu.Lock()
	if e.users > 0 || e.inst == nil || e.closed {
		e.mu.Unlock()
		return
	}
	in := e.inst
	e.inst, e.timer = nil, nil
	e.mu.Unlock()
	e.stop(in, false)
	e.log.Info("downloads: aria2c stopped (idle)")
}

// running reports whether an aria2c process is up (tests, diagnostics).
func (e *engine) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inst != nil && e.inst.alive()
}

// close stops aria2c for good.
func (e *engine) close() {
	e.mu.Lock()
	e.closed = true
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	in := e.inst
	e.inst = nil
	e.mu.Unlock()
	if in != nil {
		e.stop(in, true)
	}
}

// Waits for aria2c to exit after its shutdown call before it is killed:
// idle (nobody waits) and at the application's close (forced). aria2c
// 1.37.0 takes 4.3–4.5 s to exit after answering forceShutdown (M1
// acceptance), and the whole graceful stop of the application has 10 s
// (SPEC-v3 V1.1). By then every download was removed (stopGIDs), so its
// control files are written and killing it loses nothing; were the kill to
// fail, --stop-with-process ends it with this process.
var (
	idleExitWait  = 5 * time.Second
	forceExitWait = time.Second
)

// stop asks aria2c to shut down and kills it when it does not exit in time
// (force: the application closes; forceShutdown and a short wait).
func (e *engine) stop(in *instance, force bool) {
	rpcWait, exitWait := 2*time.Second, idleExitWait
	if force {
		rpcWait, exitWait = 500*time.Millisecond, forceExitWait
	}
	if in.tr != nil {
		defer in.tr.CloseIdleConnections()
	}
	defer in.guard.close() // (also when aria2c did not exit: it then reaches no server)
	ctx, cancel := context.WithTimeout(context.Background(), rpcWait)
	_ = in.rpc.shutdown(ctx, force)
	cancel()
	select {
	case <-in.exited:
		return
	case <-time.After(exitWait):
	}
	_ = in.proc.Kill()
	select {
	case <-in.exited:
	case <-time.After(5 * time.Second):
		e.log.Warn("downloads: aria2c did not exit after kill")
	}
}

var errExitedEarly = errors.New("aria2c exited before answering")

// start launches aria2c, waits until it listens on 127.0.0.1:port
// (awaitListener: nothing is sent there before), then for its first RPC
// answer.
func (e *engine) start(ctx context.Context) (*instance, error) {
	exe := e.exe
	if exe == "" {
		exe = "aria2c"
	}
	if !strings.ContainsAny(exe, `/\`) {
		p, err := exec.LookPath(exe)
		if err != nil {
			return nil, errEngineMissing
		}
		exe = p
	} else if _, err := os.Stat(exe); err != nil {
		return nil, errEngineMissing
	}
	feats, err := e.features(exe)
	if err != nil {
		return nil, err
	}
	secret, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	pick := e.pickPort
	if pick == nil {
		pick = freePort
	}
	port, err := pick()
	if err != nil {
		return nil, fmt.Errorf("aria2c port: %w", err)
	}
	conf, err := writeConf(secret)
	if err != nil {
		return nil, fmt.Errorf("aria2c conf file: %w", err)
	}
	removeConf := func() { _ = os.Remove(conf) }
	// the guard listens before aria2c exists: aria2c's command line names
	// it as the proxy of every protocol
	g, err := newGuard(e.log)
	if err != nil {
		removeConf()
		return nil, err
	}
	args := append(append(ariaArgs(conf, port, feats), resolverArgs(e.asyncDNS)...), guardArgs(g.addr)...)
	watch := newBindWatch()
	proc, err := e.launch(exe, args, watch)
	if err != nil {
		removeConf()
		g.close()
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return nil, errEngineMissing
		}
		return nil, fmt.Errorf("start aria2c: %w", err)
	}
	in := &instance{proc: proc, exited: make(chan struct{}), guard: g}
	go func() {
		_ = proc.Wait()
		// (whatever ended aria2c: a failed start, a stop, a crash) the
		// guard first: an instance marked exited no longer has a guard
		// listening (SEC43 info-7)
		g.close()
		close(in.exited)
	}()
	fail := func(err error) (*instance, error) {
		removeConf()
		_ = proc.Kill()
		if in.tr != nil {
			in.tr.CloseIdleConnections()
		}
		return nil, err
	}
	timeout := e.startTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if err := e.awaitListener(ctx, in, port, watch, deadline); err != nil {
		if errors.Is(err, errExitedEarly) {
			removeConf()
			return nil, err
		}
		return fail(err)
	}
	in.tr = e.rpcTransport(port, proc.Pid())
	in.rpc = newRPC(port, secret, &http.Client{Timeout: e.hc.Timeout, Transport: in.tr})
	for {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := in.rpc.version(cctx)
		cancel()
		if err == nil {
			removeConf() // the secret is in aria2c's memory now
			e.log.Info("downloads: aria2c started", "port", port)
			return in, nil
		}
		var re *rpcError
		if errors.As(err, &re) {
			// it answers but refuses the secret: not our aria2c
			return fail(fmt.Errorf("aria2c rpc refused the secret: %w", err))
		}
		if errors.Is(err, errPortTaken) {
			return fail(err) // (the connection check found another listener)
		}
		select {
		case <-in.exited:
			removeConf()
			return nil, errExitedEarly
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("aria2c did not answer within %s", timeout))
		}
	}
}

// ariaArgs is aria2c's command line: the conf file with the secret, the RPC
// port, and the options of the features it was built with.
func ariaArgs(conf string, port int, f ariaFeatures) []string {
	args := []string{
		"--conf-path=" + conf,
		"--enable-rpc=true",
		"--rpc-listen-all=false", // the loopback interface: 127.0.0.1 and [::1]
		"--rpc-listen-port=" + strconv.Itoa(port),
		"--stop-with-process=" + strconv.Itoa(os.Getpid()),
		"--continue=true",
		"--always-resume=false", // a server without ranges: start the file again instead of failing
		"--auto-file-renaming=false",
		"--allow-overwrite=false",
		"--file-allocation=none",
		"--check-certificate=true",
		"--console-log-level=warn", // (a failure to bind the RPC port is an error: bindWatch sees it)
		"--summary-interval=0",
		"--show-console-readout=false",
		"--enable-color=false",
		"--max-concurrent-downloads=" + strconv.Itoa(maxActive),
		"--max-tries=5",
		"--retry-wait=2",
		// nothing of the user's, nothing beyond the URL asked for
		"--no-netrc=true",
	}
	if f.metalink {
		args = append(args, "--follow-metalink=false")
	}
	if f.bitTorrent {
		args = append(args, "--follow-torrent=false", "--enable-dht=false", "--enable-dht6=false",
			"--bt-enable-lpd=false", "--enable-peer-exchange=false")
	}
	return args
}

// ariaFeatures are the optional parts aria2c was built with.
type ariaFeatures struct {
	bitTorrent, metalink bool
}

// parseFeatures reads the "Enabled Features:" line of aria2c --version
// ("Async DNS, BitTorrent, GZip, HTTPS, Message Digest, Metalink, ..."); no
// such line means none.
func parseFeatures(version string) ariaFeatures {
	return ariaFeatures{bitTorrent: hasFeature(version, "BitTorrent"), metalink: hasFeature(version, "Metalink")}
}

// hasFeature: the "Enabled Features:" line of aria2c --version names
// feature.
func hasFeature(version, feature string) bool {
	for _, line := range strings.Split(version, "\n") {
		list, ok := strings.CutPrefix(strings.TrimSpace(line), "Enabled Features:")
		if !ok {
			continue
		}
		for _, name := range strings.Split(list, ",") {
			if strings.TrimSpace(name) == feature {
				return true
			}
		}
	}
	return false
}

// resolverArgs make aria2c resolve names with the system's resolver,
// which Go's own look-ups use too (SEC11 low-1): the official Windows
// build has c-ares ("Async DNS", --async-dns on by default), which may
// look up a name the system's resolver refused, or answer it otherwise,
// and the address check would judge one answer while aria2c connects to
// the other. The option exists only in a build with Async DNS (the macOS
// build has none, and resolves with the system already): a build refuses
// an option it does not know (exit 28, like the BitTorrent ones). Given on
// the command line, it holds for every download of the process.
func resolverArgs(asyncDNS bool) []string {
	if !asyncDNS {
		return nil
	}
	return []string{"--async-dns=false"}
}

// errFeaturesUnknown: aria2c --version could not be read (an error, more
// than 10 s, no "Enabled Features:" line), so aria2c is not started (SEC19
// low-4). The options that switch BitTorrent and Metalink off exist only
// in a build with them, and a build refuses an option it does not know;
// without them the official Windows build follows a .torrent, and its
// peers, DHT and UDP trackers are not reached through the guard proxy. The
// task fails with downloads.engine_failed (it may be retried); the next
// start reads --version again.
var errFeaturesUnknown = errors.New("aria2c --version could not be read, so aria2c was not started " +
	"(without its features known, BitTorrent and Metalink cannot be switched off)")

// features is what the aria2c exe was built with, read once per engine
// when it can be read; errFeaturesUnknown when not (read again next time).
func (e *engine) features(exe string) (ariaFeatures, error) {
	e.featMu.Lock()
	defer e.featMu.Unlock()
	if e.featKnown {
		return e.feats, nil
	}
	run := e.versionOf
	if run == nil {
		run = runVersion
	}
	out, err := run(exe)
	if err != nil || !strings.Contains(out, "Enabled Features:") {
		e.log.Warn("downloads: aria2c --version could not be read; aria2c is not started (its BitTorrent and Metalink "+
			"options could not be given); the next download reads it again", "err", err)
		return ariaFeatures{}, errFeaturesUnknown
	}
	e.feats = parseFeatures(out)
	e.asyncDNS = hasFeature(out, "Async DNS")
	e.featKnown = true
	// with the features known, aria2c resolves with the system's resolver:
	// --async-dns=false is given, or it has no other one
	e.sysResolver.Store(true)
	e.log.Info("downloads: aria2c features", "bittorrent", e.feats.bitTorrent, "metalink", e.feats.metalink, "async_dns", e.asyncDNS)
	return e.feats, nil
}

// systemResolver: the aria2c started resolves names with the system's
// resolver, as Go's look-ups do, so a name Go cannot look up is one aria2c
// cannot either (resolver.checkHost refuses it; SEC16 R2). False before
// the first start and when its features are unknown.
func (e *engine) systemResolver() bool {
	return e != nil && e.sysResolver.Load()
}

// runVersion runs "exe --version" (hidden window, at most 10 s).
func runVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.Dir = filepath.Dir(exe)
	hideWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}

// writeConf writes the --conf-path file: only the RPC secret.
func writeConf(secret string) (string, error) {
	f, err := os.CreateTemp("", "lectureflow-aria2-*.conf")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Chmod(0o600)
	_, err = io.WriteString(f, "rpc-secret="+secret+"\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// execLauncher runs the real aria2c. Its output goes to out only (the
// engine's bindWatch, which keeps nothing of it: aria2c's warnings may
// quote a download's full URL). Its environment is this process's without
// the proxy variables (launchEnv, guard.go).
func execLauncher(exe string, args []string, out io.Writer) (Process, error) {
	if out == nil {
		out = io.Discard
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = launchEnv(os.Environ())
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = nil
	hideWindow(cmd)
	// Besides --stop-with-process (aria2c polls for it): on Windows the job
	// object ends aria2c at once when LectureFlow ends, also on a crash
	// (procutil.Tied; test-chaos 1).
	proc, err := procutil.Start(cmd, procutil.Tied)
	if err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, proc: proc}, nil
}

type execProcess struct {
	cmd  *exec.Cmd
	proc *procutil.Process
}

func (p *execProcess) Wait() error {
	err := p.cmd.Wait()
	p.proc.Close()
	return err
}
func (p *execProcess) Kill() error { return p.cmd.Process.Kill() }
func (p *execProcess) Pid() int    { return p.cmd.Process.Pid }
