// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
)

// netFailure is a connection error without the network's own text (SEC7
// M2): Go's net errors name the addresses ("dial tcp 192.0.2.10:22",
// "read tcp <local>-><remote>:22", "lookup <host> on <resolver>:53") and
// the errors of this package reach the log, the card and the lecture's
// error. Its text is the classified error and the failure's class only;
// errors.Is / errors.As still reach the cause (context.Canceled, …).
type netFailure struct {
	kind  error  // errOffline, errLost, errAuth or errHandshake
	class string // ErrorClass of the cause
	cause error
}

func (e *netFailure) Error() string { return e.kind.Error() + " (" + e.class + ")" }

func (e *netFailure) Unwrap() []error { return []error{e.kind, e.cause} }

// wrapNet wraps cause as kind without cause's text (kind alone when cause
// is nil).
func wrapNet(kind, cause error) error {
	if cause == nil {
		return kind
	}
	return &netFailure{kind: kind, class: ErrorClass(cause), cause: cause}
}

// Error classes (ErrorClass): what the log keeps of a connection's failure.
const (
	ClassRefused     = "refused"
	ClassTimeout     = "timeout"
	ClassDNS         = "dns"
	ClassReset       = "reset"
	ClassUnreachable = "unreachable"
	ClassEOF         = "eof"
	ClassClosed      = "closed"
	ClassAuth        = "auth"
	ClassHostKey     = "host_key"
	ClassCanceled    = "canceled"
	ClassOther       = "other"
)

// ErrorClass classifies a connection error without naming any address:
// refused, timeout, dns, reset, unreachable, eof, closed, auth, host_key,
// canceled or other ("" for nil). Logs write it (with the remote.Code* of
// ConnErrorCode) instead of the error's text.
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	var nf *netFailure
	if errors.As(err, &nf) {
		return nf.class
	}
	var dns *net.DNSError
	switch {
	case errors.Is(err, errHostKeyUnknown), errors.Is(err, errHostKeyMismatch):
		return ClassHostKey
	case errors.Is(err, context.Canceled):
		return ClassCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.As(err, &dns):
		return ClassDNS
	case errors.Is(err, net.ErrClosed):
		return ClassClosed
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ClassEOF
	}
	msg := strings.ToLower(err.Error())
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("unable to authenticate", "no supported methods remain"):
		return ClassAuth
	case has("connection refused", "actively refused"):
		return ClassRefused
	case has("connection reset", "forcibly closed", "broken pipe", "connection aborted"):
		return ClassReset
	case has("no such host", "server misbehaving", "getaddrinfo", "dnsquery"):
		return ClassDNS
	case has("unreachable", "no route to host"):
		return ClassUnreachable
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || has("timed out", "timeout") {
		return ClassTimeout
	}
	return ClassOther
}
