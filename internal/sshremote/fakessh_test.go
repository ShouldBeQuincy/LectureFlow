// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// An in-process SSH server for the tests (golang.org/x/crypto/ssh): public
// key authentication only, "exec" requests only. It plays a remote
// machine whose shell knows exactly the commands the controller sends:
// "uname -s -m", the install script and the agent's start line, which runs
// the real lf-agent built for this machine (TestMain) with a private HOME
// / LOCALAPPDATA. Anything else is refused (exit 127), so a test fails when
// the controller sends a command it should not.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type fakeRemote struct {
	t        *testing.T
	ln       net.Listener
	host     ssh.Signer
	uname    string
	platform string
	home     string // the remote user's HOME (and LOCALAPPDATA base)

	mu         sync.Mutex
	authorized map[string]bool
	conns      []net.Conn
	commands   []string
	agentRuns  int
	refuse     bool // refuse new TCP connections (closes them at once)
	forward    bool // a jump host: "direct-tcpip" channels are forwarded
	authTries  int  // public keys offered for authentication
}

// newFakeRemote starts a server playing a machine of uname (e.g.
// "Darwin arm64").
func newFakeRemote(t *testing.T, uname string) *fakeRemote {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	host, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	p, err := platformOfUname(uname)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRemote{t: t, ln: ln, host: host, uname: uname, platform: p, home: t.TempDir(), authorized: map[string]bool{}}
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.dropAll()
	})
	return f
}

func (f *fakeRemote) addr() (string, int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// authorize adds an authorized_keys line (as the user would).
func (f *fakeRemote) authorize(line string) {
	line = strings.TrimPrefix(strings.TrimSpace(line), "restrict ")
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.authorized[string(pub.Marshal())] = true
	f.mu.Unlock()
}

// newHostKey replaces the server's host key (a reinstalled or impostor server).
func (f *fakeRemote) newHostKey() {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	f.mu.Lock()
	f.host = s
	f.mu.Unlock()
}

// dropAll closes every connection (a network failure).
func (f *fakeRemote) dropAll() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (f *fakeRemote) setRefuse(v bool) {
	f.mu.Lock()
	f.refuse = v
	f.mu.Unlock()
}

func (f *fakeRemote) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.refuse {
			f.mu.Unlock()
			c.Close()
			continue
		}
		f.conns = append(f.conns, c)
		host := f.host
		f.mu.Unlock()
		go f.handle(c, host)
	}
}

func (f *fakeRemote) handle(c net.Conn, host ssh.Signer) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.mu.Lock()
			f.authTries++
			ok := f.authorized[string(key.Marshal())]
			f.mu.Unlock()
			if ok {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(host)
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() == "direct-tcpip" && f.forwards() {
			go forwardChannel(nc)
			continue
		}
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go f.session(ch, creqs)
	}
}

// setForward makes the server a jump host (it forwards "direct-tcpip").
func (f *fakeRemote) setForward(v bool) {
	f.mu.Lock()
	f.forward = v
	f.mu.Unlock()
}

func (f *fakeRemote) forwards() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forward
}

// authAttempts is how many public keys clients offered so far (a client
// that stops at the host key offers none).
func (f *fakeRemote) authAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authTries
}

// forwardChannel serves one "direct-tcpip" channel (RFC 4254 §7.2): it
// connects to the asked address and copies both ways until one side ends.
func forwardChannel(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip request")
		return
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	out, err := d.Dial("tcp", net.JoinHostPort(p.Host, fmt.Sprint(p.Port)))
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "cannot connect")
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		out.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(out, ch); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ch, out); done <- struct{}{} }()
	<-done
	out.Close()
	ch.Close()
}

func (f *fakeRemote) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil) // no pty, no shell, no env, no subsystem
			continue
		}
		var p struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &p); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		f.mu.Lock()
		f.commands = append(f.commands, p.Command)
		f.mu.Unlock()
		code := f.exec(p.Command, ch, reqs)
		_, _ = ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(code)))
		return
	}
}

var (
	idRe         = regexp.MustCompile(`/agent/([0-9a-f]{16})`)
	agentCmdRe   = regexp.MustCompile(`^sh -c '([^']*)' lf-agent((?: [A-Za-z0-9_.:=-]+)*)$`)
	installCmdRe = regexp.MustCompile(`^sh -c '([^']*)'$`)
)

// agentPath is where the fake keeps an installed agent build.
func (f *fakeRemote) agentPath(id string) string {
	name := "lf-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(f.home, "installed-agents", id, name)
}

func (f *fakeRemote) exec(cmd string, ch ssh.Channel, reqs <-chan *ssh.Request) int {
	if cmd == unameCommand {
		fmt.Fprintln(ch, f.uname)
		return 0
	}
	if m := agentCmdRe.FindStringSubmatch(cmd); m != nil {
		id := idRe.FindStringSubmatch(m[1])
		if id == nil || m[1] != agentScript(f.platform, id[1]) {
			return 127
		}
		bin := f.agentPath(id[1])
		if _, err := os.Stat(bin); err != nil {
			fmt.Fprintf(ch.Stderr(), "sh: %s: not found\n", bin)
			return 127
		}
		f.mu.Lock()
		f.agentRuns++
		f.mu.Unlock()
		return f.runAgent(bin, strings.Fields(m[2]), ch, reqs)
	}
	if m := installCmdRe.FindStringSubmatch(cmd); m != nil {
		id := idRe.FindStringSubmatch(m[1])
		if id == nil || m[1] != installScript(f.platform, id[1]) {
			return 127
		}
		dst := f.agentPath(id[1])
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return 1
		}
		out, err := os.Create(dst + ".part")
		if err != nil {
			return 1
		}
		_, err = io.Copy(out, ch)
		out.Close()
		if err != nil {
			return 1
		}
		if err := os.Rename(dst+".part", dst); err != nil {
			return 1
		}
		_ = os.Chmod(dst, 0o700)
		return 0
	}
	fmt.Fprintln(ch.Stderr(), "fake remote: command not allowed")
	return 127
}

// agentEnv is the environment the remote user's agent runs with.
func (f *fakeRemote) agentEnv() []string {
	// No PATH: the agent finds no nvidia-smi, so the tests never query
	// this machine's GPU.
	env := []string{"HOME=" + f.home, "LOCALAPPDATA=" + filepath.Join(f.home, "AppData", "Local"), "PATH="}
	for _, k := range []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (f *fakeRemote) runAgent(bin string, args []string, ch ssh.Channel, reqs <-chan *ssh.Request) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = f.agentEnv()
	cmd.Dir = f.home
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 1
	}
	cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
	if err := cmd.Start(); err != nil {
		return 126
	}
	go func() {
		_, _ = io.Copy(stdin, ch) // until the client closes its side (or the connection drops)
		stdin.Close()
	}()
	go func() {
		for r := range reqs {
			if r.Type == "signal" {
				cancel() // SIGKILL from the client
			}
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

// agentRoot and jobsDir are the agent's directories on this "remote".
func (f *fakeRemote) agentRoot() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(f.home, "AppData", "Local", "LectureFlowAgent")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(f.home, "Library", "Application Support", "LectureFlow Agent")
	}
	return filepath.Join(f.home, ".local", "share", "lectureflow-agent")
}

func (f *fakeRemote) jobsDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(f.agentRoot(), "jobs")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(f.home, "Library", "Caches", "LectureFlow Agent", "jobs")
	}
	return filepath.Join(f.home, ".cache", "lectureflow-agent", "jobs")
}

func (f *fakeRemote) sentCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}
