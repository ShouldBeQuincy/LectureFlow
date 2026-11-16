// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

// SEC7 L2: the authorize commands put the stored public key line between
// quotes for the user to paste into a shell on the server. The line is only
// ever written by Add (newKey), but the commands are built from the stored
// row: a line that is not exactly "restrict ssh-ed25519 <base64>
// lectureflow-<slug>" gets no command at all.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestPublicKeyOfRefusesALineThatIsNotLectureFlows(t *testing.T) {
	e := newTestEnv(t, nil)
	v, err := e.m.Add(context.Background(), AddRequest{Name: "Lab", Host: "198.51.100.5", Username: "q"})
	if err != nil {
		t.Fatal(err)
	}
	good, err := e.m.PublicKeyOf(v.ID)
	if err != nil || good.Line != v.PublicKey || good.Fingerprint == "" {
		t.Fatalf("the generated line: %v %+v", err, good)
	}
	if err := checkPublicKeyLine(good.Line); err != nil {
		t.Fatalf("the generated line %q: %v", good.Line, err)
	}
	fields := strings.Fields(good.Line) // restrict, ssh-ed25519, the key, the comment
	if len(fields) != 4 {
		t.Fatalf("the generated line %q", good.Line)
	}
	blob := fields[2]
	s := e.m.server(v.ID)
	for name, line := range map[string]string{
		"empty":                   "",
		"closes the quote":        "restrict ssh-ed25519 " + blob + " lectureflow-lab'; Remove-Item -Recurse $HOME #",
		"typographic quote":       "restrict ssh-ed25519 " + blob + " lectureflow-lab’; Remove-Item $HOME #",
		"command substitution":    "restrict ssh-ed25519 " + blob + " lectureflow-$(id)",
		"backquotes":              "restrict ssh-ed25519 " + blob + " lectureflow-`id`",
		"a second line":           good.Line + "\nssh-ed25519 " + blob + " someone",
		"a trailing line feed":    good.Line + "\n",
		"a carriage return":       good.Line + "\r",
		"no restrict":             strings.TrimPrefix(good.Line, "restrict "),
		"other options":           `command="/bin/sh",` + good.Line,
		"another comment":         "restrict ssh-ed25519 " + blob + " root@laptop",
		"no comment":              "restrict ssh-ed25519 " + blob,
		"a comment too long":      "restrict ssh-ed25519 " + blob + " lectureflow-" + strings.Repeat("a", 33),
		"an upper case comment":   "restrict ssh-ed25519 " + blob + " lectureflow-Lab",
		"another key type":        "restrict ssh-rsa " + blob + " lectureflow-lab",
		"not base64":              "restrict ssh-ed25519 AAAA*C3 lectureflow-lab",
		"base64 that is no key":   "restrict ssh-ed25519 AAAA lectureflow-lab",
		"two spaces":              "restrict  ssh-ed25519 " + blob + " lectureflow-lab",
		"a tab":                   "restrict\tssh-ed25519 " + blob + " lectureflow-lab",
		"leading space":           " " + good.Line,
		"trailing space":          good.Line + " ",
		"padding inside the key":  "restrict ssh-ed25519 " + blob[:8] + "=" + blob[8:] + " lectureflow-lab",
		"a quote inside the key":  "restrict ssh-ed25519 " + blob[:8] + "'" + blob[8:] + " lectureflow-lab",
		"a dollar inside the key": "restrict ssh-ed25519 " + blob[:8] + "$x" + blob[8:] + " lectureflow-lab",
	} {
		s.mu.Lock()
		s.rec.PublicKey = line
		s.mu.Unlock()
		k, err := e.m.PublicKeyOf(v.ID)
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "public_key" {
			t.Errorf("%s: %v, want invalid public_key", name, err)
		}
		if k.Command != "" || k.CommandWindows != "" || k.CommandWindowsAdmin != "" || k.Line != "" {
			t.Errorf("%s: a command was built: %+v", name, k)
		}
	}
	// the slugs Add makes and takes
	for _, comment := range []string{"lectureflow-a", "lectureflow-gpu-box-2", "lectureflow-" + strings.Repeat("a", 32), "lectureflow--x"} {
		if err := checkPublicKeyLine("restrict ssh-ed25519 " + blob + " " + comment); err != nil {
			t.Errorf("%s: %v", comment, err)
		}
	}
}

// SEC7 L8: an authorized_keys file whose last line has no line feed. The
// commands appended LectureFlow's line right behind it: one line made of
// the user's last key with "restrict ssh-ed25519 …" as the end of its
// comment, so LectureFlow's key was never authorized. Every command starts
// a new line first (an empty line in authorized_keys is ignored).
func TestAuthorizeCommandsStartANewLine(t *testing.T) {
	e := newTestEnv(t, nil)
	v, err := e.m.Add(context.Background(), AddRequest{Name: "Lab", Host: "198.51.100.5", Username: "q"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := e.m.PublicKeyOf(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.Command, `printf '\n%s\n' '`+k.Line+`' >> ~/.ssh/authorized_keys`) {
		t.Errorf("posix: %s", k.Command)
	}
	if !strings.Contains(k.CommandWindows, `\.ssh\authorized_keys" -Value '', '`+k.Line+`'`) {
		t.Errorf("windows: %s", k.CommandWindows)
	}
	if !strings.Contains(k.CommandWindowsAdmin, `\administrators_authorized_keys" -Value '', '`+k.Line+`'`) {
		t.Errorf("windows admin: %s", k.CommandWindowsAdmin)
	}

	// The POSIX command for real, where there is a sh to run it.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Logf("no sh on this machine: the command is not run (%v)", err)
		return
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	own := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " me@laptop"
	for name, before := range map[string]*string{
		"no file":                 nil,
		"no line feed at the end": ptr(own),
		"a line feed at the end":  ptr(own + "\n"),
	} {
		home := t.TempDir()
		file := filepath.Join(home, ".ssh", "authorized_keys")
		want := []string{k.Line}
		if before != nil {
			want = []string{own, k.Line}
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(*before), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, sh, "-c", k.Command)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" { // sshd ignores empty lines
				got = append(got, l)
			}
		}
		if !slices.Equal(got, want) || !strings.HasSuffix(string(b), "\n") {
			t.Errorf("%s: authorized_keys is %q, want the lines %q", name, b, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
