// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// Bringing lf-agent to a server (plan §2, §4). Only two things run through
// the remote shell, both fixed strings: "uname -s -m" (the platform) and
// the install script, which reads the agent binary from stdin into the
// agent's own 0700 directory and renames it when complete. Every other
// command starts the agent itself: sh -c 'exec "<root>/agent/<id>/lf-agent"
// "$@"' lf-agent <args>, with args from a fixed set and checked values
// (POSIX sh, whatever the user's login shell is). The agent's directory
// carries the first 16 hex characters of its SHA-256, so a new build never
// replaces a running one and the controller always runs exactly its own
// build (the SHA-256 the agent reports is compared with the local file).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"lectureflow/internal/remote"
)

// unameCommand reads the platform.
const unameCommand = "uname -s -m"

// platformOfUname maps "Darwin arm64" / "Linux x86_64" to remote.Platform*.
func platformOfUname(out string) (string, error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return "", errors.New("unexpected uname output")
	}
	switch strings.ToLower(f[0]) + "/" + f[1] {
	case "darwin/arm64":
		return remote.PlatformDarwinArm64, nil
	case "linux/x86_64", "linux/amd64":
		return remote.PlatformLinuxX64, nil
	}
	return "", fmt.Errorf("%w: %s %s", errUnsupported, cleanToken(f[0]), cleanToken(f[1]))
}

var errUnsupported = errors.New("this server's platform is not supported")

var tokenRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func cleanToken(s string) string {
	s = tokenRe.ReplaceAllString(s, "")
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// rootExpr is the agent's data directory as a POSIX sh expression
// (agent.ResolveDirs).
func rootExpr(platform string) string {
	if platform == remote.PlatformDarwinArm64 {
		return `$HOME/Library/Application Support/LectureFlow Agent`
	}
	return `${XDG_DATA_HOME:-$HOME/.local/share}/lectureflow-agent`
}

// agentBuild is a local agent binary for one platform.
type agentBuild struct {
	path string
	sum  string // SHA-256 hex
}

func (b agentBuild) id() string { return b.sum[:16] }

// findAgent returns the agent binary for platform from dir
// (dir/<platform>/lf-agent).
func findAgent(dir, platform string) (agentBuild, error) {
	if dir == "" {
		return agentBuild{}, fmt.Errorf("%w (no agent directory)", errAgentMissing)
	}
	p := filepath.Join(dir, platform, "lf-agent")
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return agentBuild{}, fmt.Errorf("%w for %s", errAgentMissing, platform)
	}
	if err != nil {
		return agentBuild{}, fmt.Errorf("the lf-agent build for %s cannot be read: %w", platform, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return agentBuild{}, err
	}
	return agentBuild{path: p, sum: hex.EncodeToString(h.Sum(nil))}, nil
}

// agentScript starts the agent (the arguments follow as "$@").
func agentScript(platform, id string) string {
	return `exec "` + rootExpr(platform) + `/agent/` + id + `/lf-agent" "$@"`
}

// installScript reads the agent from stdin into its directory.
func installScript(platform, id string) string {
	root := rootExpr(platform)
	return `umask 077; r="` + root + `"; d="$r/agent/` + id + `"; ` +
		`mkdir -p "$d" && chmod 700 "$r" "$r/agent" "$d" && cat > "$d/lf-agent.part" && ` +
		`chmod 700 "$d/lf-agent.part" && mv -f "$d/lf-agent.part" "$d/lf-agent"`
}

var argRe = regexp.MustCompile(`^[A-Za-z0-9_.:=-]{1,128}$`)

// agentCommand is the command line that runs the agent with args. The
// args are fixed words and checked values; anything else is refused.
func agentCommand(platform, id string, args ...string) (string, error) {
	var b strings.Builder
	b.WriteString("sh -c ")
	b.WriteString(shellQuote(agentScript(platform, id)))
	b.WriteString(" lf-agent")
	for _, a := range args {
		if !argRe.MatchString(a) {
			return "", fmt.Errorf("sshremote: refusing agent argument %q", a)
		}
		b.WriteByte(' ')
		b.WriteString(a)
	}
	return b.String(), nil
}

// installCommand is the command line of the install script.
func installCommand(platform, id string) string {
	return "sh -c " + shellQuote(installScript(platform, id))
}

// detectPlatform runs uname.
func detectPlatform(ctx context.Context, c *conn) (string, error) {
	out, _, err := c.output(ctx, unameCommand, nil, 256)
	if err != nil {
		return "", err
	}
	return platformOfUname(string(out))
}

// agentVersion runs "lf-agent version"; ok false when the agent is not
// there (or not this build).
func agentVersion(ctx context.Context, c *conn, platform string, b agentBuild) (remote.AgentEvent, bool, error) {
	cmd, err := agentCommand(platform, b.id(), "version")
	if err != nil {
		return remote.AgentEvent{}, false, err
	}
	out, _, err := c.output(ctx, cmd, nil, remote.MaxEventLine+1)
	var ee *exitError
	if errors.As(err, &ee) {
		return remote.AgentEvent{}, false, nil // not installed (sh: exec failed: 126/127)
	}
	if err != nil {
		return remote.AgentEvent{}, false, err
	}
	ev, err := remote.ParseEvent(bytes.TrimSpace(out))
	if err != nil || ev.Type != remote.EvVersion {
		return remote.AgentEvent{}, false, nil
	}
	return ev, ev.SHA256 == b.sum, nil // the build is chosen by platform; its hash is what counts
}

// installAgent uploads the agent and checks it.
func installAgent(ctx context.Context, c *conn, platform string, b agentBuild) (remote.AgentEvent, error) {
	f, err := os.Open(b.path)
	if err != nil {
		return remote.AgentEvent{}, err
	}
	defer f.Close()
	if _, err := c.run(ctx, installCommand(platform, b.id()), f, io.Discard); err != nil {
		return remote.AgentEvent{}, fmt.Errorf("installing lf-agent: %w", err)
	}
	ev, ok, err := agentVersion(ctx, c, platform, b)
	if err != nil {
		return ev, err
	}
	if !ok {
		return ev, errors.New("the installed lf-agent does not match this build")
	}
	return ev, nil
}

// agentEvent runs an agent command that answers with one event line.
func agentEvent(ctx context.Context, c *conn, platform string, b agentBuild, stdin io.Reader, args ...string) (remote.AgentEvent, error) {
	cmd, err := agentCommand(platform, b.id(), args...)
	if err != nil {
		return remote.AgentEvent{}, err
	}
	out, _, err := c.output(ctx, cmd, stdin, remote.MaxEventLine+1)
	line := bytes.TrimSpace(out)
	if len(line) > 0 {
		ev, perr := remote.ParseEvent(line)
		if perr != nil {
			return remote.AgentEvent{}, perr
		}
		if ev.Type == remote.EvFail {
			return ev, &agentFail{code: ev.Code, freeMB: ev.FreeMB, needMB: ev.NeedMB}
		}
		return ev, err
	}
	if err == nil {
		err = fmt.Errorf("%w: no answer", remote.ErrContract)
	}
	return remote.AgentEvent{}, err
}
