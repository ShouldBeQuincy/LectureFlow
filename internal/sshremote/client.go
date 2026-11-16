// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// One SSH connection to a server (plan §3): golang.org/x/crypto/ssh, never
// the system's ssh (no dependence on the user's configuration, host keys
// and keys under LectureFlow's control, an in-process server in the
// tests). Public-key authentication only: no password or
// keyboard-interactive method is registered. Optional jump hosts, each
// with its own pinned host key. Keepalive every 15 s; three missed in a
// row close the connection.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// Timing of a connection (variables for the tests).
var (
	dialTimeout       = 15 * time.Second
	keepaliveEvery    = 15 * time.Second
	keepaliveMisses   = 3
	handshakeDeadline = 30 * time.Second
)

// Jump is one jump host (remote_server.jump_json).
type Jump struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	HostKey  string `json:"host_key"`
}

// dialer opens TCP connections (tests replace it).
type dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// target is what a connection needs.
type target struct {
	host     string
	port     int
	user     string
	hostKey  string
	jumps    []Jump
	signer   ssh.Signer
	dial     dialer
	seenKey  func(hop int, key ssh.PublicKey) // the presented key of each hop (0 = first jump or the server)
	clientID string
}

// conn is a live connection.
type conn struct {
	client  *ssh.Client
	chain   []*ssh.Client // jump clients, closed with the connection
	done    chan struct{}
	once    sync.Once
	err     atomic.Value // error that closed it
	latency atomic.Int64 // last keepalive round trip, ms
}

func (c *conn) closeWith(err error) {
	c.once.Do(func() {
		if err != nil {
			c.err.Store(err)
		}
		c.client.Close()
		for i := len(c.chain) - 1; i >= 0; i-- {
			c.chain[i].Close()
		}
		close(c.done)
	})
}

func (c *conn) close() { c.closeWith(nil) }

func (c *conn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// connect dials the target (through its jump hosts).
func connect(ctx context.Context, t target) (*conn, error) {
	hops := append(append([]Jump(nil), t.jumps...), Jump{Host: t.host, Port: t.port, Username: t.user, HostKey: t.hostKey})
	var chain []*ssh.Client
	var prev *ssh.Client
	cleanup := func() {
		for i := len(chain) - 1; i >= 0; i-- {
			chain[i].Close()
		}
	}
	for i, h := range hops {
		addr := net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
		var raw net.Conn
		var err error
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		if prev == nil {
			raw, err = t.dial(dctx, "tcp", addr)
		} else {
			raw, err = prev.DialContext(dctx, "tcp", addr)
		}
		cancel()
		if err != nil {
			cleanup()
			return nil, wrapNet(errOffline, err)
		}
		hop := i
		cfg := &ssh.ClientConfig{
			User: h.Username,
			Auth: []ssh.AuthMethod{ssh.PublicKeys(t.signer)},
			HostKeyCallback: hostKeyCallback(h.HostKey, func(k ssh.PublicKey) {
				if t.seenKey != nil {
					t.seenKey(hop, k)
				}
			}),
			Timeout:           dialTimeout,
			ClientVersion:     t.clientID,
			HostKeyAlgorithms: hostKeyAlgorithms(h.HostKey),
		}
		_ = raw.SetDeadline(time.Now().Add(handshakeDeadline))
		cc, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
		if err != nil {
			raw.Close()
			cleanup()
			var mm *keyMismatchError
			if errors.As(err, &mm) {
				mm.hop = i // which hop's pinned key it was (Connect shows that hop)
			}
			return nil, classifyHandshake(err)
		}
		_ = raw.SetDeadline(time.Time{})
		client := ssh.NewClient(cc, chans, reqs)
		if i < len(hops)-1 {
			chain = append(chain, client)
			prev = client
			continue
		}
		c := &conn{client: client, chain: chain, done: make(chan struct{})}
		go c.keepalive()
		go func() {
			err := client.Wait()
			c.closeWith(wrapNet(errLost, err))
		}()
		return c, nil
	}
	cleanup()
	return nil, errOffline
}

// Connection errors (classified for the card and the scheduler).
var (
	errOffline   = errors.New("the server cannot be reached")
	errAuth      = errors.New("the server refused LectureFlow's key")
	errLost      = errors.New("the connection to the server was lost")
	errHandshake = errors.New("the SSH handshake failed")
)

func classifyHandshake(err error) error {
	switch {
	case errors.Is(err, errHostKeyUnknown), errors.Is(err, errHostKeyMismatch):
		return err
	case strings.Contains(err.Error(), "unable to authenticate"), strings.Contains(err.Error(), "no supported methods remain"):
		return wrapNet(errAuth, err)
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) {
		return wrapNet(errOffline, err)
	}
	return wrapNet(errHandshake, err)
}

// keepalive sends keepalive@openssh.com; keepaliveMisses failures in a row
// close the connection.
func (c *conn) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		start := time.Now()
		res := make(chan error, 1)
		go func() {
			_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		var err error
		select {
		case err = <-res:
		case <-time.After(keepaliveEvery):
			err = errors.New("keepalive timed out")
		case <-c.done:
			return
		}
		if err != nil {
			misses++
			if misses >= keepaliveMisses {
				c.closeWith(fmt.Errorf("%w: %d keepalives missed", errLost, misses))
				return
			}
			continue
		}
		misses = 0
		c.latency.Store(time.Since(start).Milliseconds())
	}
}

// exitError is a remote command's non-zero exit.
type exitError struct {
	code   int
	stderr string
}

func (e *exitError) Error() string {
	return fmt.Sprintf("remote command exited with %d", e.code)
}

// run executes cmd on the server with stdin, writing its stdout to stdout;
// stderr (at most 4 KB) is returned for the log. A cancelled ctx closes the
// session.
func (c *conn) run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) (string, error) {
	if !c.alive() {
		return "", errLost
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return "", wrapNet(errLost, err)
	}
	defer sess.Close()
	var stderr limitedBuffer
	stderr.max = 4096
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, &stderr
	if err := sess.Start(cmd); err != nil {
		return "", wrapNet(errLost, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		<-done
		return stderr.String(), ctx.Err()
	case <-c.done:
		sess.Close()
		<-done
		return stderr.String(), errLost
	}
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return stderr.String(), &exitError{code: ee.ExitStatus(), stderr: stderr.String()}
		}
		return stderr.String(), wrapNet(errLost, err)
	}
	return stderr.String(), nil
}

// output runs cmd and returns its stdout (at most max bytes).
func (c *conn) output(ctx context.Context, cmd string, stdin io.Reader, max int) ([]byte, string, error) {
	var out limitedBuffer
	out.max = max
	stderr, err := c.run(ctx, cmd, stdin, &out)
	if out.over {
		return nil, stderr, errTooLong
	}
	return out.Bytes(), stderr, err
}

var errTooLong = errors.New("remote output too long")

// limitedBuffer keeps at most max bytes (over: more came).
type limitedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// shellQuote quotes s for a POSIX shell (single quotes).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hostKeyAlgorithms asks for the pinned key's type only (a server with
// several host keys then always presents the confirmed one); before a key
// is confirmed ed25519 comes first (the type users usually check).
func hostKeyAlgorithms(pinned string) []string {
	if pinned != "" {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned)); err == nil {
			if k.Type() == ssh.KeyAlgoRSA {
				return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
			}
			return []string{k.Type()}
		}
	}
	return []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
}
