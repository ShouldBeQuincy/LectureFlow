// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// Host keys (plan §3): the first connection shows the server's key (its
// algorithm and SHA256 fingerprint, and whether the user's own known_hosts
// agrees) and stops; only after the user confirmed it is the key pinned
// (remote_server.host_key). From then on any other key is refused, before
// authentication: nothing is sent to a server that is not the pinned one.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyInfo is a presented host key.
type HostKeyInfo struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"` // "SHA256:…"
	// Key is the key in authorized_keys format (what Trust pins).
	Key string `json:"key"`
	// KnownHosts: "match" (the user's ~/.ssh/known_hosts lists this key for
	// the host), "mismatch" (it lists another, or marks this one @revoked),
	// "" (no entry or no file; @cert-authority lines are no entry).
	KnownHosts string `json:"known_hosts,omitempty"`
	// Check is the command that shows the fingerprint on the server.
	Check string `json:"check"`
}

var (
	// errHostKeyUnknown: no key is pinned yet (the connection stopped
	// after reading it).
	errHostKeyUnknown = errors.New("the server's host key is not confirmed yet")
	// errHostKeyMismatch: the server presented another key than the pinned one.
	errHostKeyMismatch = errors.New("the server's host key differs from the confirmed one")
)

// hostKeyCallback checks the presented key against pinned (authorized_keys
// format; "" = none yet). seen receives the presented key either way.
func hostKeyCallback(pinned string, seen func(ssh.PublicKey)) ssh.HostKeyCallback {
	var want ssh.PublicKey
	if pinned != "" {
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned)); err == nil {
			want = k
		}
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if seen != nil {
			seen(key)
		}
		if pinned == "" {
			return errHostKeyUnknown
		}
		if want == nil || !bytes.Equal(want.Marshal(), key.Marshal()) {
			return &keyMismatchError{presented: key, pinned: pinned}
		}
		return nil
	}
}

// keyMismatchError is errHostKeyMismatch with the key the server presented
// instead of the pinned one (SEC7 M1: Connect shows it next to the pinned
// one's fingerprint, the error's params name it), and the hop it came from
// (connect sets it; 0 = the first jump host or the server).
type keyMismatchError struct {
	hop       int
	presented ssh.PublicKey
	pinned    string // authorized_keys format
}

func (e *keyMismatchError) Error() string {
	return fmt.Sprintf("%s (presented %s)", errHostKeyMismatch, ssh.FingerprintSHA256(e.presented))
}

func (e *keyMismatchError) Unwrap() error { return errHostKeyMismatch }

// PresentedFingerprint is the fingerprint of the host key a server
// presented instead of the pinned one ("" unless err is a host key
// mismatch): the fingerprint param of remote.host_key_mismatch.
func PresentedFingerprint(err error) string {
	var mm *keyMismatchError
	if errors.As(err, &mm) && mm.presented != nil {
		return ssh.FingerprintSHA256(mm.presented)
	}
	return ""
}

// connErrParams are the params of a connection error's code: the server's
// name, and for a changed host key the presented key's fingerprint.
func connErrParams(device string, err error) map[string]any {
	p := map[string]any{"device": device}
	if fp := PresentedFingerprint(err); fp != "" {
		p["fingerprint"] = fp
	}
	return p
}

// keyFingerprint is the SHA256 fingerprint of a key in authorized_keys
// format ("" when it does not parse).
func keyFingerprint(line string) string {
	if line == "" {
		return ""
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// NormalizeFingerprint is a SHA256 fingerprint in ssh.FingerprintSHA256's
// form ("SHA256:" and the unpadded base64), however it was typed or copied:
// surrounding spaces, the prefix in any case or missing, the base64's "="
// padding. The base64 itself is case-sensitive and stays as it is.
func NormalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if len(fp) >= 7 && strings.EqualFold(fp[:7], "SHA256:") {
		fp = fp[7:]
	}
	fp = strings.TrimRight(strings.TrimSpace(fp), "=")
	if fp == "" {
		return ""
	}
	return "SHA256:" + fp
}

// describeKey is the HostKeyInfo of a presented key for host:port.
func describeKey(key ssh.PublicKey, host string, port int) HostKeyInfo {
	info := HostKeyInfo{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key),
		Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))}
	alg := strings.TrimPrefix(key.Type(), "ssh-")
	if strings.HasPrefix(key.Type(), "ecdsa-") {
		alg = "ecdsa"
	}
	info.Check = fmt.Sprintf("ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub", alg)
	info.KnownHosts = knownHostsVerdict(key, host, port)
	return info
}

// knownHostsVerdict compares with the user's own ~/.ssh/known_hosts (read
// only; LectureFlow never writes it).
func knownHostsVerdict(key ssh.PublicKey, host string, port int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return knownHostsVerdictIn(filepath.Join(home, ".ssh", "known_hosts"), key, host, port)
}

// knownHostsVerdictIn is knownHostsVerdict with this known_hosts file.
func knownHostsVerdictIn(file string, key ssh.PublicKey, host string, port int) string {
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	cb, err := knownhosts.New(file)
	if err != nil {
		return ""
	}
	// The callback takes "host:port" (it splits it itself, and matches
	// port 22 against plain host entries); knownhosts.Normalize's form
	// drops ":22" and would never match.
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	err = cb(addr, &net.TCPAddr{}, key)
	// N30-L2: knownhosts counts a matching @cert-authority line among the
	// keys it wants for the host, but such a line names the key that signs
	// host certificates, not a host's key, and LectureFlow asks for plain
	// keys only (hostKeyAlgorithms). When every wanted key comes from such
	// lines the file has no entry for this host's plain key: "", not a
	// "mismatch" the user is asked to tick off on every first connection.
	// (A plain key that is itself listed as an authority still "matches".)
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) > 0 {
		ca := certAuthorityLines(file)
		plain := false
		for _, w := range ke.Want {
			if !ca[w.Line] {
				plain = true
				break
			}
		}
		if !plain {
			return ""
		}
	}
	return knownHostsResult(err)
}

// certAuthorityLines are the numbers (from 1) of file's @cert-authority
// lines, counted as knownhosts counts them (KnownKey.Line).
func certAuthorityLines(file string) map[int]bool {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[int]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := bytes.Trim(sc.Bytes(), " \t")
		if i := bytes.IndexAny(line, " \t"); i > 0 && string(line[:i]) == "@cert-authority" {
			out[n] = true
		}
	}
	return out
}

// knownHostsResult is the verdict of a known_hosts callback's answer. A key
// the file marks @revoked is a "mismatch" (SEC7 L1): the user's own file
// says not to trust it, so it must not pass for an unknown host that can be
// trusted in one click.
func knownHostsResult(err error) string {
	var ke *knownhosts.KeyError
	var re *knownhosts.RevokedError
	switch {
	case err == nil:
		return "match"
	case errors.As(err, &re):
		return "mismatch"
	case errors.As(err, &ke) && len(ke.Want) > 0:
		return "mismatch"
	}
	return ""
}
