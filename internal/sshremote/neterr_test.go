// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC7 M2: the package's errors and logs never carry the network's own
// text (addresses, host names, user names), only the classified code and
// the failure's class.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"lectureflow/internal/remote"
)

var (
	testRemoteIP = net.ParseIP("192.0.2.10")
	testLocalIP  = net.ParseIP("192.0.2.77")
)

func opErr(op string, inner error) error {
	return &net.OpError{Op: op, Net: "tcp", Source: &net.TCPAddr{IP: testLocalIP, Port: 51234},
		Addr: &net.TCPAddr{IP: testRemoteIP, Port: 22}, Err: inner}
}

func dnsErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "gpu7.cs.example.com",
		Server: "192.0.2.53:53", IsNotFound: true}}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrorClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{opErr("dial", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}), ClassRefused},
		{opErr("dial", errors.New("connectex: No connection could be made because the target machine actively refused it.")), ClassRefused},
		{opErr("read", &os.SyscallError{Syscall: "wsarecv", Err: errors.New("An existing connection was forcibly closed by the remote host.")}), ClassReset},
		{opErr("read", errors.New("connection reset by peer")), ClassReset},
		{opErr("dial", timeoutErr{}), ClassTimeout},
		{context.DeadlineExceeded, ClassTimeout},
		{context.Canceled, ClassCanceled},
		{dnsErr(), ClassDNS},
		{opErr("dial", errors.New("connect: no route to host")), ClassUnreachable},
		{io.EOF, ClassEOF},
		{fmt.Errorf("ssh: handshake failed: %w", io.EOF), ClassEOF},
		{net.ErrClosed, ClassClosed},
		{errors.New("ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"), ClassAuth},
		{fmt.Errorf("%w (presented SHA256:x)", errHostKeyMismatch), ClassHostKey},
		{errors.New("something else"), ClassOther},
	}
	for _, c := range cases {
		if got := ErrorClass(c.err); got != c.want {
			t.Errorf("ErrorClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// wrapNet drops the cause's text but keeps it for errors.Is / As and the
// classification (the card's code).
func TestWrapNetHidesTheAddresses(t *testing.T) {
	for _, c := range []struct {
		kind, cause error
		code        string
	}{
		{errOffline, opErr("dial", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}), remote.CodeOffline},
		{errOffline, dnsErr(), remote.CodeOffline},
		{errLost, opErr("read", errors.New("connection reset by peer")), remote.CodeOffline},
		{errAuth, errors.New("ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"), remote.CodeAuthFailed},
	} {
		err := wrapNet(c.kind, c.cause)
		for _, leak := range []string{"192.0.2", "gpu7", "example.com", "51234"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%q names %q", err, leak)
			}
		}
		if !errors.Is(err, c.kind) || !errors.Is(err, c.cause) {
			t.Errorf("%v: the chain lost the kind or the cause", err)
		}
		if got := ConnErrorCode(err); got != c.code {
			t.Errorf("%v: code %q, want %q", err, got, c.code)
		}
		if cl := ErrorClass(err); cl == ClassOther || !strings.Contains(err.Error(), "("+cl+")") {
			t.Errorf("%v: class %q", err, cl)
		}
		// wrapped again (a coded lecture error) the text stays clean
		if s := fallback(remote.CodeDisconnected, nil, err).Error(); strings.Contains(s, "192.0.2") {
			t.Errorf("fallback names the address: %q", s)
		}
	}
	if wrapNet(errLost, nil) != errLost {
		t.Fatal("wrapNet(kind, nil) is not kind")
	}
	if !errors.Is(wrapNet(errLost, context.Canceled), context.Canceled) {
		t.Fatal("a cancelled call is no longer context.Canceled")
	}
}

// logCapture collects every slog record (text form) while a test runs.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureSlog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// A failed connection logs the server's device, its state, the code and
// the failure's class: never the host, the address or the user name.
func TestConnectFailureLogsNoAddress(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		class string
	}{
		{"refused", opErr("dial", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}), ClassRefused},
		{"dns", dnsErr(), ClassDNS},
		{"timeout", opErr("dial", timeoutErr{}), ClassTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureSlog(t)
			e := newTestEnv(t, func(o *Options) {
				o.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, c.err }
			})
			v, err := e.m.Add(context.Background(), AddRequest{Name: "Lab", Host: "gpu7.cs.example.com", Port: 22, Username: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.m.Test(context.Background(), v.ID)
			if err == nil {
				t.Fatal("connected through a failing dialer")
			}
			got, _ := e.m.Get(v.ID)
			if got.LastError == nil || got.LastError.Code != remote.CodeOffline {
				t.Fatalf("last error: %+v", got.LastError)
			}
			text := logs.String()
			if !strings.Contains(text, "class="+c.class) || !strings.Contains(text, "code="+remote.CodeOffline) {
				t.Fatalf("the log has no code or class:\n%s", text)
			}
			for _, leak := range []string{"192.0.2", "gpu7", "example.com", "alice", "51234"} {
				if strings.Contains(text, leak) || strings.Contains(err.Error(), leak) {
					t.Errorf("%q in the log or the error %q:\n%s", leak, err, text)
				}
			}
		})
	}
}
