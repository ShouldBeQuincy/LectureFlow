// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"errors"
	"strings"
	"sync"

	"lectureflow/internal/model"
)

// failKind tells the caller (the scheduler, next task) what to do with the
// lecture after a failed Transcribe.
type failKind int

const (
	// kindFallback: hand the lecture back to the local devices (V14.3 step
	// 1, V14.6). Every remote failure is of this kind unless said otherwise.
	kindFallback failKind = iota + 1
	// kindRequeue: "立即关机" interrupted the lecture; it goes back to the
	// queue (V14.5) and may run on Colab again later.
	kindRequeue
	// kindLocal: a problem of the local input (missing audio): neither
	// Colab nor a local device can help by retrying; the scheduler treats it
	// as an ordinary job error.
	kindLocal
	// kindBusy: the runtime could not take the lecture yet (it is being
	// released or started, or another lecture still runs on it): nothing
	// was tried, nothing failed. The lecture goes back to the queue and may
	// go to Colab again (colabfix: a lecture dispatched while the runtime
	// was being released must not be handed back for good).
	kindBusy
)

// classified carries the failure kind and the waiting_for value inside the
// model.CodedError chain, so errors.As finds both the ErrorInfo (the
// scheduler stores it as job.error) and the classification.
type classified struct {
	kind       failKind
	waitingFor string
	err        error
}

func (c *classified) Error() string {
	if c.err != nil {
		return c.err.Error()
	}
	return "colab"
}

func (c *classified) Unwrap() error { return c.err }

// newErr builds the package's errors: a *model.CodedError (code, params)
// wrapping the classification and the cause. The cause text becomes
// job.error's detail: it never contains a credential, a token or text
// derived from the audio (callers build it from fixed strings, status
// codes and field names only).
func newErr(code string, params map[string]any, kind failKind, waitingFor string, cause error) error {
	if cause == nil {
		cause = errors.New(code)
	}
	return model.Errorf(code, params, &classified{kind: kind, waitingFor: waitingFor, err: cause})
}

func fallbackErr(code string, params map[string]any, cause error) error {
	return newErr(code, params, kindFallback, "", cause)
}

func classOf(err error) *classified {
	var c *classified
	if errors.As(err, &c) {
		return c
	}
	return nil
}

// IsFallback reports whether the lecture should go back to the local
// devices (V14.3: "任务回到本机调度"; V14.6: "回退到本机调度").
func IsFallback(err error) bool {
	c := classOf(err)
	return c != nil && c.kind == kindFallback
}

// IsRequeue reports whether "立即关机" interrupted the lecture: it goes
// back to the queue (V14.5) instead of failing.
func IsRequeue(err error) bool {
	c := classOf(err)
	return c != nil && c.kind == kindRequeue
}

// IsBusy reports whether the runtime could not take the lecture yet
// (CodeBusy: being released or started, or another lecture runs on it).
// Nothing ran: the lecture goes back to the queue without counting as a
// hand-back, and Colab may take it once CanDispatch says so.
func IsBusy(err error) bool {
	c := classOf(err)
	return c != nil && c.kind == kindBusy
}

// busyErr is CodeBusy (IsBusy).
func busyErr(cause error) error {
	return newErr(CodeBusy, nil, kindBusy, "", cause)
}

// WaitingFor is the waiting_for value an error implies (model.WaitRemote*),
// "" when none: remote_auth (credential missing, expired or rejected),
// remote_window, remote_balance, remote_queue (gave up waiting for an
// accelerator).
func WaitingFor(err error) string {
	if c := classOf(err); c != nil {
		return c.waitingFor
	}
	return ""
}

// Code is the error's code ("" for an error of another package).
func Code(err error) string {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		return ce.Info.Code
	}
	return ""
}

// codedParam returns a string param of the error's ErrorInfo.
func codedParam(err error, key string) (string, bool) {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		v, ok := ce.Info.Params[key].(string)
		return v, ok
	}
	return "", false
}

// ---- secrets ----

// secretSet remembers every secret this process handled (refresh token,
// client secret, access tokens, runtime proxy tokens, XSRF tokens) so that
// errors leaving the package and log lines can be checked against them: a
// last line of defence behind building messages from fixed strings only.
type secretSet struct {
	mu   sync.Mutex
	vals map[string]struct{}
}

func (s *secretSet) add(vals ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string]struct{}{}
	}
	for _, v := range vals {
		if len(v) >= 6 { // shorter strings are not secrets and would mangle ordinary text
			s.vals[v] = struct{}{}
		}
	}
}

// scrub replaces every known secret in text by "[redacted]".
func (s *secretSet) scrub(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for v := range s.vals {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, "[redacted]")
		}
	}
	return text
}

func (s *secretSet) contains(text string) bool {
	return s.scrub(text) != text
}

// scrubErr returns err unchanged when neither its text nor its params hold
// a known secret, and otherwise an equivalent error (same code, params and
// classification) whose cause text and params have the secrets removed.
// The params are looked at on their own: a CodedError's text does not
// include them (review R52 L2).
func (s *secretSet) scrubErr(err error) error {
	if err == nil || !s.contains(err.Error()) && !s.paramsHold(err) {
		return err
	}
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		return errors.New(s.scrub(err.Error()))
	}
	params := map[string]any{}
	for k, v := range ce.Info.Params {
		if str, ok := v.(string); ok {
			v = s.scrub(str)
		}
		params[k] = v
	}
	if len(params) == 0 {
		params = nil
	}
	cause := errors.New(s.scrub(ce.Error()))
	if c := classOf(err); c != nil {
		return newErr(ce.Info.Code, params, c.kind, c.waitingFor, cause)
	}
	return model.Errorf(ce.Info.Code, params, cause)
}

// paramsHold tells whether a string param of err's ErrorInfo holds a known
// secret.
func (s *secretSet) paramsHold(err error) bool {
	var ce *model.CodedError
	if !errors.As(err, &ce) {
		return false
	}
	for _, v := range ce.Info.Params {
		if str, ok := v.(string); ok && s.contains(str) {
			return true
		}
	}
	return false
}
