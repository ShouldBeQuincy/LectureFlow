// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// The control channel ("lf-agent serve", remote/protocol.go): requests
// with a sequence number get their reply by number; a job's events go to
// the job's subscriber; machine / sample events to the sampler. A line
// that breaks the contract closes the channel and the connection (the
// agent cannot be trusted any more); the lecture goes back to the local
// devices.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/remote"
)

type control struct {
	sess *ssh.Session
	in   io.WriteCloser

	wmu sync.Mutex
	seq atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan remote.AgentEvent
	jobs    map[string]chan remote.AgentEvent
	err     error
	dead    chan struct{}

	onMachine func(map[string]any)
	onSample  func(map[string]any)
	hello     remote.AgentEvent
}

var errControlClosed = errors.New("the control channel closed")

// openControl starts "lf-agent serve" and says hello.
func openControl(ctx context.Context, c *conn, cmd string, retain time.Duration,
	onMachine, onSample func(map[string]any)) (*control, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, wrapNet(errLost, err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.Stderr = io.Discard
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, wrapNet(errLost, err)
	}
	ctl := &control{sess: sess, in: in, pending: map[int64]chan remote.AgentEvent{}, jobs: map[string]chan remote.AgentEvent{},
		dead: make(chan struct{}), onMachine: onMachine, onSample: onSample}
	go ctl.read(out, c)
	go ctl.pinger()
	go func() {
		select {
		case <-c.done:
			ctl.fail(errLost)
		case <-ctl.dead:
		}
	}()
	ev, err := ctl.call(ctx, remote.Request{Op: remote.OpHello, Version: agentVersionForHello(), RetainS: int(retain / time.Second)})
	if err != nil {
		ctl.close()
		return nil, err
	}
	if ev.Type != remote.EvHello {
		ctl.close()
		return nil, fmt.Errorf("%w: unexpected hello reply", remote.ErrContract)
	}
	ctl.hello = ev
	return ctl, nil
}

func (ctl *control) read(out io.Reader, c *conn) {
	r := bufio.NewReaderSize(out, remote.MaxEventLine+2)
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				ctl.fail(fmt.Errorf("%w: line too long", remote.ErrContract))
				c.closeWith(remote.ErrContract)
				return
			}
			ctl.fail(errControlClosed)
			return
		}
		line = line[:len(line)-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		ev, err := remote.ParseEvent(line)
		if err != nil {
			ctl.fail(err)
			c.closeWith(err)
			return
		}
		ctl.dispatch(ev)
	}
}

func (ctl *control) dispatch(ev remote.AgentEvent) {
	switch ev.Type {
	case remote.EventMachine:
		if ctl.onMachine != nil {
			ctl.onMachine(ev.Machine)
		}
		return
	case remote.EventSample:
		if ctl.onSample != nil {
			ctl.onSample(ev.Sample)
		}
		return
	case remote.EvProgress, remote.EvStatus, remote.EvDone, remote.EvError:
		ctl.mu.Lock()
		ch := ctl.jobs[ev.Job]
		ctl.mu.Unlock()
		if ch != nil {
			select {
			case ch <- ev:
			default:
				if ev.Type == remote.EvDone || ev.Type == remote.EvError {
					// Never drop a run's end (progress may be dropped); the
					// subscriber is gone after a minute at the latest.
					go func() {
						select {
						case ch <- ev:
						case <-time.After(time.Minute):
						}
					}()
				}
			}
		}
		return
	}
	// hello, ok, fail, pong: a reply (fail without seq: a line the agent
	// could not parse — a bug on our side; it ends the channel)
	if ev.Seq == 0 {
		ctl.fail(fmt.Errorf("%w: reply without seq", remote.ErrContract))
		return
	}
	ctl.mu.Lock()
	ch := ctl.pending[ev.Seq]
	delete(ctl.pending, ev.Seq)
	ctl.mu.Unlock()
	if ch != nil {
		ch <- ev
	}
}

func (ctl *control) fail(err error) {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	if ctl.err != nil {
		return
	}
	ctl.err = err
	close(ctl.dead)
	_ = ctl.sess.Close()
}

func (ctl *control) close() { ctl.fail(errControlClosed) }

func (ctl *control) alive() bool {
	select {
	case <-ctl.dead:
		return false
	default:
		return true
	}
}

// Err is why the channel closed.
func (ctl *control) Err() error {
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return ctl.err
}

// call sends a request and waits for its reply (fail is an *agentFail).
func (ctl *control) call(ctx context.Context, req remote.Request) (remote.AgentEvent, error) {
	req.Seq = ctl.seq.Add(1)
	ch := make(chan remote.AgentEvent, 1)
	ctl.mu.Lock()
	if ctl.err != nil {
		err := ctl.err
		ctl.mu.Unlock()
		return remote.AgentEvent{}, err
	}
	ctl.pending[req.Seq] = ch
	ctl.mu.Unlock()
	b, err := remote.Encode(req)
	if err == nil && len(b) > remote.MaxRequestLine {
		err = errors.New("request too long")
	}
	if err == nil {
		ctl.wmu.Lock()
		_, err = ctl.in.Write(b)
		ctl.wmu.Unlock()
	}
	if err != nil {
		ctl.mu.Lock()
		delete(ctl.pending, req.Seq)
		ctl.mu.Unlock()
		ctl.fail(wrapNet(errLost, err))
		return remote.AgentEvent{}, errLost
	}
	select {
	case ev := <-ch:
		if ev.Type == remote.EvFail {
			return ev, &agentFail{code: ev.Code, freeMB: ev.FreeMB, needMB: ev.NeedMB}
		}
		return ev, nil
	case <-ctl.dead:
		return remote.AgentEvent{}, ctl.Err()
	case <-ctx.Done():
		ctl.mu.Lock()
		delete(ctl.pending, req.Seq)
		ctl.mu.Unlock()
		return remote.AgentEvent{}, ctx.Err()
	}
}

// subscribe returns the channel of a job's events (until unsubscribe).
func (ctl *control) subscribe(job string) chan remote.AgentEvent {
	ch := make(chan remote.AgentEvent, 64)
	ctl.mu.Lock()
	ctl.jobs[job] = ch
	ctl.mu.Unlock()
	return ch
}

func (ctl *control) unsubscribe(job string) {
	ctl.mu.Lock()
	delete(ctl.jobs, job)
	ctl.mu.Unlock()
}

// pinger keeps the agent's heartbeat (remote.HeartbeatEvery).
func (ctl *control) pinger() {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctl.dead:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*heartbeatEvery)
		_, err := ctl.call(ctx, remote.Request{Op: remote.OpPing})
		cancel()
		if err != nil && ctl.alive() {
			ctl.fail(wrapNet(errLost, err))
			return
		}
	}
}

// heartbeatEvery is remote.HeartbeatEvery (tests shorten it).
var heartbeatEvery = remote.HeartbeatEvery

// agentFail is a "fail" reply.
type agentFail struct {
	code           string
	freeMB, needMB int64
}

func (e *agentFail) Error() string { return "agent: " + e.code }

func failCode(err error) string {
	var f *agentFail
	if errors.As(err, &f) {
		return f.code
	}
	return ""
}

// agentVersionForHello is the version the controller announces ("" = not
// checked by the agent: the controller already compared the build's hash).
func agentVersionForHello() string { return "" }
