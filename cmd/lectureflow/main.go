// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command lectureflow: load config, migrate, recover, start scheduler and HTTP (SPEC §1.1).
//
// Command-line contract (SPEC-v3 V1, v3-brief §1): -config, -listen,
// -data-dir, -desktop, -resources, -app-version; for the browser extension
// (SPEC-ext, nmhost.go) -launcher, -launcher-blocked, -portable,
// -nm-endpoint. Once HTTP is
// ready it prints "LECTUREFLOW_READY http://127.0.0.1:PORT" on stdout; a failed
// start ends with "ERROR: <reason>" as the last line of stderr and exit
// code 1; exit code 3 asks the desktop shell to start the service again.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"lectureflow/internal/api"
	"lectureflow/internal/config"
	"lectureflow/internal/dirperm"
	"lectureflow/internal/relocate"
)

// version is set at build time with -ldflags "-X main.version=X.Y.Z";
// -app-version takes precedence for what the app reports. Left at "dev"
// (nmhost.DevBuildVersion) it makes a development build, whose browser
// registration admits the unpacked development extension too (nmhost.go).
var version = "dev"

// applyStaged is the startup hook run before the database is opened: it
// switches to the staged data of an "import existing data" (SPEC-v3 V1.3,
// internal/relocate). Tests replace it; nil = nothing to apply.
var applyStaged func(ctx context.Context, dataDir string) error = relocate.ApplyStaged

// runApplyStaged runs applyStaged. A *relocate.ApplyError with Restored
// (the import failed but the data directory is back as it was) is logged
// and the start goes on with the previous database; any other error (the
// directory could not be restored, or something else failed) stops the
// start: the database must not be opened half switched.
func runApplyStaged(ctx context.Context, dataDir string) error {
	if applyStaged == nil {
		return nil
	}
	err := applyStaged(ctx, dataDir)
	if err == nil {
		return nil
	}
	var ae *relocate.ApplyError
	if errors.As(err, &ae) && ae.Restored {
		slog.Error("staged import not applied; starting with the previous data", "err", err)
		return nil
	}
	return fmt.Errorf("staged data: %w", err)
}

// Exit codes (v3-brief §1).
const (
	exitOK      = 0
	exitFailure = 1
	exitRestart = 3 // the desktop shell starts the service again
)

// ReadyPrefix starts the line printed on stdout once HTTP accepts requests.
const ReadyPrefix = "LECTUREFLOW_READY "

// MinTokenLen is the minimum length of the desktop access token (the shell
// sends 32 random bytes as hex, 64 characters).
const MinTokenLen = 32

// Time limits of the start and the graceful stop.
var (
	tokenReadTimeout = 30 * time.Second
	shutdownLimit    = 10 * time.Second // SPEC-v3 V1.1: the whole graceful stop
)

// webModeNotice is printed in web mode (SPEC-v3 V1.2, v3-brief §1).
const webModeNotice = "网页模式：只监听 127.0.0.1，不需要访问令牌，仅供开发与高级用户使用。 / Web mode: listening on 127.0.0.1 only, no access token required; for development and advanced users only."

func main() {
	// A desktop shell that is killed closes the service's stdout / stderr;
	// without this the next log line raises SIGPIPE and the service exits
	// without its orderly stop (Mac acceptance, 2026-10-28). Writes then
	// just fail with EPIPE.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(runMain(runEnv{
		args:   os.Args[1:],
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		exit:   os.Exit,
	}))
}

// cliOptions are the parsed command-line flags.
type cliOptions struct {
	Config     string
	Listen     string
	DataDir    string
	Desktop    bool
	Resources  string
	AppVersion string
	// LocationFile is {default data dir}/location.json, passed by the
	// desktop shell unless LECTUREFLOW_DATA_DIR chose the data directory
	// (DesktopOptions.LocationFile; step 5 "change the data directory").
	LocationFile string
	// Native Messaging (SPEC-ext, ext-brief §4, nmhost.go): the program the
	// native host starts the desktop app with (launch.json), whether this
	// is the portable build (the host is copied to a stable place), and
	// the IPC endpoint ("" = LECTUREFLOW_NM_ENDPOINT or the per-user
	// default, "off" = none).
	Launcher   string
	Portable   bool
	NMEndpoint string
	// LauncherBlocked is why the desktop shell passes no -launcher
	// although it is the desktop app (macOS: "translocated" or "dmg",
	// nmhost.Launcher*): the diagnostics ask to move the app.
	LauncherBlocked string
	// SelfUpdateVerify, when set, is an updates directory: check its
	// pending update again and exit (selfupdate_verify.go); nothing else
	// runs.
	SelfUpdateVerify string
	// SelfUpdatePrevious: check previous.json's package (the version a
	// rollback returns to) instead; SelfUpdateCopy: copy the vouched
	// package into a new private directory there (sec-update M2, M3).
	SelfUpdatePrevious bool
	SelfUpdateCopy     string
	// SelfUpdatePrivate: check nothing but the data directory's
	// permissions and make an empty private directory in SelfUpdateCopy:
	// the shell copies a backup of the installation there and checks its
	// hash before a rollback uses it (security audit M-3).
	SelfUpdatePrivate bool
	// SelfUpdateTrustDir: the directory the shell keeps its record of the
	// installation and the backups in (Windows: outside the data
	// directory); checked too (selfupdate.CheckTrustDir), every mode.
	// SelfUpdateTrustCheck: check nothing else (ok trusted).
	SelfUpdateTrustDir   string
	SelfUpdateTrustCheck bool
	// SelfUpdateInstallDir: the installation directory (the Windows
	// installation set up by the installer; security audit M-4); checked
	// too (selfupdate.CheckInstallDir), every mode.
	SelfUpdateInstallDir string
	// InstallDir, desktop mode: the installation directory the shell names
	// (as SelfUpdateInstallDir; "" = none: development, the portable build,
	// macOS). The diagnostics' install_dir row and the in-app update look at
	// it.
	InstallDir string
	// NoSelfUpdate, desktop mode (-no-self-update): the desktop shell found
	// that this copy of the app cannot replace itself (lib/updatefiles.js
	// describeInstallation: unsupported); selfupdate.Options.NoInstall.
	NoSelfUpdate bool
	// DataKeyStdin: the line after the token is the data key of the stored
	// credentials (the macOS desktop shell keeps it in the Keychain;
	// secrets_setup.go).
	DataKeyStdin bool
}

// parseArgs parses the command line. Usage and parse errors go to stderr.
func parseArgs(args []string, stderr io.Writer) (cliOptions, error) {
	var o cliOptions
	fs := flag.NewFlagSet("lectureflow", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.Config, "config", "config.toml", "path to config.toml (a missing file means all defaults)")
	fs.StringVar(&o.Listen, "listen", "", "listen address 127.0.0.1:PORT (overrides the config; port 0 picks a free one)")
	fs.StringVar(&o.DataDir, "data-dir", "", "data directory (overrides the config)")
	fs.BoolVar(&o.Desktop, "desktop", false, "desktop mode: read the access token from the first line of stdin; stdin EOF stops the service")
	fs.BoolVar(&o.DataKeyStdin, "datakey-stdin", false, "desktop mode: the second line of stdin is the data key of the stored credentials (64 hex characters; the macOS shell keeps it in the Keychain)")
	fs.StringVar(&o.Resources, "resources", "", "packaged resources directory (worker/, bin/ffmpeg, bin/uv, components.json)")
	fs.StringVar(&o.AppVersion, "app-version", "", "application version (default: the build version)")
	fs.StringVar(&o.LocationFile, "location-file", "", "desktop mode: path of location.json in the default data directory")
	fs.StringVar(&o.Launcher, "launcher", "", "desktop mode: the program that starts the desktop app (the browser extension's host runs it with --background)")
	fs.StringVar(&o.LauncherBlocked, "launcher-blocked", "", "desktop mode on macOS: why there is no -launcher (translocated | dmg: the app runs from a place that disappears; the diagnostics ask to move it to Applications)")
	fs.BoolVar(&o.Portable, "portable", false, "portable build: copy the browser extension's host to a stable place before registering it")
	fs.StringVar(&o.NMEndpoint, "nm-endpoint", "", "browser extension endpoint: a named pipe or a socket path, \"off\" = none (default: LECTUREFLOW_NM_ENDPOINT, else per user)")
	fs.StringVar(&o.SelfUpdateVerify, "selfupdate-verify", "", "verify the pending update of this updates directory with the built-in key, print one line and exit (0 valid, 1 invalid, 2 not configured, 3 other accounts can change the data directory or the installation)")
	fs.BoolVar(&o.SelfUpdatePrevious, "selfupdate-previous", false, "with -selfupdate-verify: verify the package of previous.json (the version a rollback returns to) instead")
	fs.StringVar(&o.SelfUpdateCopy, "selfupdate-copy", "", "with -selfupdate-verify: copy the package, while hashing it, into a new private directory in this directory and name it on the ok line (copy=<path>)")
	fs.BoolVar(&o.SelfUpdatePrivate, "selfupdate-private", false, "with -selfupdate-verify and -selfupdate-copy: verify no package; check the data directory's permissions, make an empty private directory in the -selfupdate-copy directory and name it (ok private=<path>)")
	fs.StringVar(&o.SelfUpdateTrustDir, "selfupdate-trust-dir", "", "with -selfupdate-verify: the directory the desktop shell keeps its update record in; refused (exit 3) unless it lies outside the data directory and only this account can change it (made privately when missing)")
	fs.StringVar(&o.SelfUpdateInstallDir, "selfupdate-install-dir", "", "with -selfupdate-verify: the installation directory; refused (exit 3, untrusted_install_dir) unless only this account (and the system) can change it and every folder above it stays in its place")
	fs.StringVar(&o.InstallDir, "install-dir", "", "desktop mode: the installation directory set up by the Windows installer (the diagnostics and the in-app update check who else can change it)")
	fs.BoolVar(&o.NoSelfUpdate, "no-self-update", false, "desktop mode: this copy of the app cannot replace itself (an unpacked or copied folder, not the installer's installation or the portable build): the in-app update only tells about new versions; a pending update in the data directory is another copy's")
	fs.BoolVar(&o.SelfUpdateTrustCheck, "selfupdate-trust-check", false, "with -selfupdate-verify: verify no package; check the data directory's permissions (and -selfupdate-trust-dir) only (ok trusted)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if o.AppVersion == "" {
		o.AppVersion = version
	}
	if o.AppVersion == "" {
		o.AppVersion = "dev"
	}
	return o, nil
}

// readToken reads the desktop access token from the first line of r
// (trimmed; at least MinTokenLen characters). It returns the reader to keep
// watching for EOF. A shell that never writes the line fails the start
// after tokenReadTimeout.
func readToken(r io.Reader) (string, *bufio.Reader, error) {
	br := bufio.NewReader(r)
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := br.ReadString('\n')
		ch <- res{line, err}
	}()
	var got res
	select {
	case got = <-ch:
	case <-time.After(tokenReadTimeout):
		return "", nil, errors.New("desktop mode: no access token on stdin")
	}
	if got.err != nil && !errors.Is(got.err, io.EOF) {
		return "", nil, fmt.Errorf("desktop mode: read access token: %w", got.err)
	}
	tok := strings.TrimSpace(got.line)
	if len(tok) < MinTokenLen {
		return "", nil, fmt.Errorf("desktop mode: access token too short (%d characters, at least %d)", len(tok), MinTokenLen)
	}
	return tok, br, nil
}

// watchEOF calls stop once r reaches EOF (or fails): the desktop shell has
// exited or closed the pipe.
func watchEOF(r io.Reader, stop func()) {
	go func() {
		_, _ = io.Copy(io.Discard, r)
		stop()
	}()
}

// runEnv is the process environment of runMain (tests replace it).
type runEnv struct {
	args           []string
	stdin          io.Reader
	stdout, stderr io.Writer
	// exit ends the process at once (os.Exit): the hard stop when the
	// graceful stop exceeds shutdownLimit.
	exit func(int)
	// signals delivers SIGINT; nil = signal.Notify on os.Interrupt.
	signals <-chan os.Signal
	// appOpts adjusts the application (tests: fake GPU, short intervals).
	appOpts AppOptions
	// started, when set, is called once the ready line is printed (tests:
	// the application and, in desktop mode, its options).
	started func(*App, *api.DesktopOptions)
}

// stopper is the single entry of the graceful stop: SIGINT, stdin EOF,
// DesktopOptions.Shutdown (code 0) and DesktopOptions.Restart (code 3).
// The first request wins; later ones are ignored.
type stopper struct {
	once sync.Once
	ch   chan int
}

func newStopper() *stopper { return &stopper{ch: make(chan int, 1)} }

func (s *stopper) request(code int) {
	s.once.Do(func() { s.ch <- code })
}

// runMain runs the service and returns the process exit code.
func runMain(env runEnv) int {
	desktop := false
	fail := func(err error) int {
		slog.Error("fatal", "err", err)
		// a coded failure: its code for the desktop shell, just before the
		// ERROR line (startfail.go)
		var sf *startFailure
		if desktop && errors.As(err, &sf) {
			fmt.Fprintln(env.stderr, failureLine(sf))
		}
		fmt.Fprintf(env.stderr, "ERROR: %s\n", oneLine(err.Error()))
		return exitFailure
	}
	opts, err := parseArgs(env.args, env.stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return fail(err)
	}
	desktop = opts.Desktop
	if opts.SelfUpdateVerify != "" {
		return runSelfUpdateVerify(selfUpdateVerifyArgs{Dir: opts.SelfUpdateVerify, Current: opts.AppVersion,
			Previous: opts.SelfUpdatePrevious, CopyDir: opts.SelfUpdateCopy, Private: opts.SelfUpdatePrivate,
			TrustDir: opts.SelfUpdateTrustDir, TrustCheck: opts.SelfUpdateTrustCheck, InstallDir: opts.SelfUpdateInstallDir}, env.stdout)
	}

	stop := newStopper()
	var token string
	var shellKey []byte
	shellKeyUnavailable := false
	if opts.DataKeyStdin && !opts.Desktop {
		return fail(errors.New("-datakey-stdin needs -desktop"))
	}
	if opts.Desktop {
		tok, rest, err := readToken(env.stdin)
		if err != nil {
			return fail(err)
		}
		token = tok
		if opts.DataKeyStdin {
			shellKey, err = readDataKey(rest)
			if errors.Is(err, errShellKeyUnavailable) {
				shellKeyUnavailable = true // no key this run (sec3 L4)
			} else if err != nil {
				return fail(err)
			}
		}
		// The shell keeps stdin open; EOF means it is gone (v3-brief §1).
		watchEOF(rest, func() {
			slog.Info("stdin closed: stopping")
			stop.request(exitOK)
		})
	}

	cfg, err := config.LoadWith(opts.Config, config.Overrides{Listen: opts.Listen, DataDir: opts.DataDir, ResourcesDir: opts.Resources})
	if err != nil {
		return fail(err)
	}
	// A data directory created here is private from the start (sec-update
	// M2, internal/dirperm); an existing one is left as it is (the
	// diagnostics warn when other accounts can change it).
	if _, err := dirperm.MkdirPrivate(cfg.DataDir); err != nil {
		return fail(err)
	}
	// One service per data directory (instancelock.go, test-upgrade F4):
	// before anything in it is written.
	lock, err := acquireInstanceLock(cfg.DataDir)
	if err != nil {
		return fail(err)
	}
	defer lock.Release()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "logs"), 0o755); err != nil {
		return fail(err)
	}
	// Rotated by size (logrotate.go, T067): lectureflow.log.1 … .5.
	logFile, err := newRotatingLog(filepath.Join(cfg.DataDir, "logs", "lectureflow.log"), logRotateMaxBytes, logRotateKeep)
	if err != nil {
		return fail(err)
	}
	defer logFile.Close()
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	prevLogger := slog.Default()
	// The log file first: once the desktop shell is gone, writes to stdout
	// fail with EPIPE (SIGPIPE is ignored), and MultiWriter stops at the
	// first error (Mac acceptance, 2026-10-28).
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(logFile, env.stdout), &slog.HandlerOptions{Level: level})))
	defer slog.SetDefault(prevLogger)

	sigc := env.signals
	if sigc == nil {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		defer signal.Stop(c)
		sigc = c
	}
	go func() {
		if _, ok := <-sigc; ok {
			slog.Info("interrupt: stopping")
			stop.request(exitOK)
		}
	}()

	// Listen first: the actual port (-listen 127.0.0.1:0) goes into the
	// desktop options (Host check) and the ready line.
	ln, err := listenTCP(cfg.Listen) // listen_*.go: exclusive on Windows (sec2-app M1)
	if err != nil {
		return fail(fmt.Errorf("listen %s: %w", cfg.Listen, err))
	}
	port := ln.Addr().(*net.TCPAddr).Port

	var (
		waLn  net.Listener
		waKey string
	)
	if cfg.WebAccess.Enabled() {
		if !opts.Desktop {
			slog.Warn("web access is only available in desktop mode; ignoring [web_access] configuration", "listen", cfg.WebAccess.Listen)
		} else {
			waLn, err = listenTCP(cfg.WebAccess.Listen)
			if err != nil {
				ln.Close()
				return fail(fmt.Errorf("web access listen %s: %w", cfg.WebAccess.Listen, err))
			}
			var keyPath string
			var keyCreated bool
			waKey, keyPath, keyCreated, err = api.LoadOrCreateWebAccessKey(cfg.DataDir)
			if err != nil {
				waLn.Close()
				ln.Close()
				return fail(err)
			}
			slog.Info("web access key ready", "path", keyPath, "created", keyCreated)
		}
	}
	closeListeners := func() {
		ln.Close()
		if waLn != nil {
			waLn.Close()
		}
	}

	appOpts := env.appOpts
	appOpts.Version = opts.AppVersion
	appOpts.Portable = opts.Portable
	appOpts.NoSelfUpdate = opts.NoSelfUpdate
	if opts.Desktop {
		appOpts.Desktop = &api.DesktopOptions{
			Token:        token,
			Port:         port,
			ResourcesDir: cfg.ResourcesDir,
			LocationFile: absOrEmpty(opts.LocationFile),
			InstallDir:   opts.InstallDir, // as named: a relative path is not vouched for
			Shutdown:     func() { stop.request(exitOK) },
			Restart:      func() { stop.request(exitRestart) },
		}
	}

	// The application's context: cancelled only by the graceful stop, after
	// the scheduler (which has its own cancellation) has stopped.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Long steps before the ready line tell the desktop shell to keep
	// waiting (startfail.go, test-upgrade F3).
	prog := &progress{}
	if opts.Desktop {
		prog.w = env.stdout
	}
	// A database from a newer LectureFlow is left untouched (upgrade.go,
	// test-upgrade F2), the staged data of an import included.
	if err := checkNotNewer(ctx, cfg.DataDir, prog); err != nil {
		closeListeners()
		return fail(err)
	}
	// Before the database is opened: switch to the staged data of an
	// "import existing data" (step 5, internal/relocate).
	if err := runApplyStaged(ctx, cfg.DataDir); err != nil {
		closeListeners()
		return fail(err)
	}
	// The backup before an upgrade, then the migrations (upgrade.go,
	// test-upgrade F1).
	if err := prepareDatabase(ctx, cfg.DataDir, opts.AppVersion, prog); err != nil {
		closeListeners()
		return fail(err)
	}
	// The stored credentials' data key, then the one-time sealing of plain
	// ones (cred-enc, secrets_setup.go).
	setupSecretsWith(ctx, cfg.DataDir, shellKey, shellKeyUnavailable, prog)
	app, err := buildApp(ctx, cfg, appOpts)
	if err != nil {
		closeListeners()
		return fail(err)
	}
	if err := app.Start(ctx); err != nil { // scheduler + first device detection
		closeListeners()
		app.Close()
		return fail(err)
	}

	srv := &http.Server{Handler: app.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	var waSrv *http.Server
	if waLn != nil {
		waPort := waLn.Addr().(*net.TCPAddr).Port
		waSrv = &http.Server{
			Handler: app.Server.WebAccessHandler(api.WebAccessOptions{
				Key:   waKey,
				Port:  waPort,
				Hosts: cfg.WebAccess.Hosts,
			}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := waSrv.Serve(waLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("web access: %w", err)
			}
		}()
		slog.Info("web access listening", "addr", waLn.Addr().String(), "hosts", cfg.WebAccess.Hosts)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	// The browser extension's endpoint and registration (nmhost.go).
	nm := startNM(app, nmOptions{Endpoint: opts.NMEndpoint, Launcher: absOrEmpty(opts.Launcher), LauncherBlocked: opts.LauncherBlocked, Portable: opts.Portable,
		Desktop: opts.Desktop, Port: port, Version: opts.AppVersion, BuildVersion: version, Stdout: env.stdout, ResourcesDir: cfg.ResourcesDir})
	app.Server.SetNMHost(nm)
	mode := "web"
	if opts.Desktop {
		mode = "desktop"
	}
	slog.Info("lectureflow started", "url", url, "data_dir", cfg.DataDir, "mode", mode, "version", opts.AppVersion)
	if !opts.Desktop {
		fmt.Fprintln(env.stdout, webModeNotice)
	}
	fmt.Fprintln(env.stdout, ReadyPrefix+url)
	if env.started != nil {
		env.started(app, appOpts.Desktop)
	}

	code := exitOK
	select {
	case code = <-stop.ch:
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			if waSrv != nil {
				waSrv.Close()
			}
			srv.Close()
			cancel()
			app.Close()
			return fail(fmt.Errorf("http: %w", err))
		}
	}

	slog.Info("shutting down", "exit_code", code)
	// A stop past the limit dumps the goroutines into the log file only
	// (stopdump.go, T063).
	withLimit(shutdownLimit, code, env.exit, logFile, func(deadline time.Time) {
		nm.Close() // no new extension requests while the app stops
		app.Shutdown(deadline, srv, waSrv)
		cancel()
	})
	slog.Info("stopped", "exit_code", code)
	return code
}

// withLimit runs the graceful stop f under the hard limit of SPEC-v3 V1.1:
// when f has not returned after limit, exit(code) ends the process at once
// (the interrupted jobs are left to startup recovery, §4.7). f gets a
// deadline a little before the limit for its own bounded waits. Before that
// exit, the stacks of all goroutines go to dump (nil: none), so a stop that
// hangs shows where (test-upgrade F8); a stop within the limit writes
// nothing there.
func withLimit(limit time.Duration, code int, exit func(int), dump io.Writer, f func(deadline time.Time)) {
	watchdog := time.AfterFunc(limit, func() {
		slog.Warn("graceful stop exceeded its time limit: exiting now", "limit", limit)
		writeStackDump(dump, limit, stackDumpMaxBytes)
		exit(code)
	})
	defer watchdog.Stop()
	f(time.Now().Add(limit - limit/20))
}

// absOrEmpty makes a path absolute (empty stays empty).
func absOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// oneLine keeps the ERROR line a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
