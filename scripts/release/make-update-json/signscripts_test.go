// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

// sign-update.sh and sign-update.ps1 must refuse a release whose update.json
// does not match the local packages and the tag, before they touch the key
// or minisign. These tests run both scripts with a key path that does not
// exist: a refusal must come from the check, and a consistent release must
// get past the check to the key ("the secret key file does not exist").
// The signing step itself runs with a stand-in for minisign (a script that
// writes a dummy signature file) and a dummy key file, never a real key.
// No network is used.
//
// The platform's own interpreter must be there (Windows: powershell.exe,
// elsewhere: bash); the other one (Git Bash on Windows, pwsh elsewhere) is
// used when installed. Every script run has a deadline.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lectureflow/internal/selfupdate"
)

const scriptTimeout = 2 * time.Minute

type scriptShell struct {
	name string
	cmd  func(ctx context.Context, script string, args signArgs) *exec.Cmd
}

type signArgs struct {
	tag, dir, manifest, channel, secret, minisign string
	allowMirrors                                  bool
}

func findBash() string {
	if runtime.GOOS != "windows" {
		p, _ := exec.LookPath("bash")
		return p
	}
	// Git for Windows' bash, never System32\bash.exe (WSL).
	if git, err := exec.LookPath("git"); err == nil {
		for _, rel := range []string{`..\bin\bash.exe`, `..\..\bin\bash.exe`, `..\usr\bin\bash.exe`} {
			p := filepath.Join(filepath.Dir(git), rel)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// goDir is the directory of the go command the scripts run: PATH first,
// then this test's own toolchain.
func goDir(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return filepath.Dir(p)
	}
	exe := "go"
	if runtime.GOOS == "windows" {
		exe = "go.exe"
	}
	p := filepath.Join(runtime.GOROOT(), "bin", exe)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return filepath.Dir(p)
	}
	t.Fatal("no go command (PATH or GOROOT/bin): the scripts run make-update-json -verify through it")
	return ""
}

func scriptShells(t *testing.T) []scriptShell {
	t.Helper()
	bashCmd := func(bash string) scriptShell {
		return scriptShell{"sh", func(ctx context.Context, script string, a signArgs) *exec.Cmd {
			args := []string{filepath.ToSlash(script), "-t", a.tag, "-d", filepath.ToSlash(a.dir), "-m", filepath.ToSlash(a.manifest), "-s", filepath.ToSlash(a.secret)}
			if a.channel != "" {
				args = append(args, "-c", a.channel)
			}
			if a.allowMirrors {
				args = append(args, "-A")
			}
			if a.minisign != "" {
				args = append(args, "-M", filepath.ToSlash(a.minisign))
			}
			return exec.CommandContext(ctx, bash, args...)
		}}
	}
	psCmd := func(ps string) scriptShell {
		return scriptShell{"ps1", func(ctx context.Context, script string, a signArgs) *exec.Cmd {
			args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
				"-Tag", a.tag, "-PackageDir", a.dir, "-Manifest", a.manifest, "-SecretKey", a.secret}
			if a.channel != "" {
				args = append(args, "-Channel", a.channel)
			}
			if a.allowMirrors {
				args = append(args, "-AllowMirrors")
			}
			if a.minisign != "" {
				args = append(args, "-Minisign", a.minisign)
			}
			return exec.CommandContext(ctx, ps, args...)
		}}
	}
	// In CI (CI set, as on GitHub's runners, which have both) a missing
	// interpreter fails; on a developer's machine only the platform's own.
	ci := os.Getenv("CI") != ""
	missing := func(what string) {
		if ci {
			t.Fatalf("%s (CI is set: both scripts must be tested)", what)
		}
		t.Log(what)
	}
	var out []scriptShell
	if runtime.GOOS == "windows" {
		ps, err := exec.LookPath("powershell.exe")
		if err != nil {
			t.Fatal("powershell.exe not found: sign-update.ps1 must be tested on Windows")
		}
		out = append(out, psCmd(ps))
		if bash := findBash(); bash != "" {
			out = append(out, bashCmd(bash))
		} else {
			missing("Git Bash not found: sign-update.sh not run on this machine")
		}
	} else {
		bash := findBash()
		if bash == "" {
			t.Fatal("bash not found: sign-update.sh must be tested")
		}
		out = append(out, bashCmd(bash))
		if ps, err := exec.LookPath("pwsh"); err == nil {
			out = append(out, psCmd(ps))
		} else {
			missing("pwsh not found: sign-update.ps1 not run on this machine")
		}
	}
	return out
}

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runSignScript(t *testing.T, sh scriptShell, a signArgs, env ...string) (code int, stdout, stderr string) {
	t.Helper()
	script := scriptPath(t, "sign-update.sh")
	if sh.name == "ps1" {
		script = scriptPath(t, "sign-update.ps1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()
	cmd := sh.cmd(ctx, script, a)
	cmd.Dir = t.TempDir() // the scripts must not depend on the caller's directory
	cmd.Env = append(os.Environ(), "PATH="+goDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader("") // a prompt gets end of input, never waits
	cmd.WaitDelay = 10 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s: no result within %v\nstdout: %s\nstderr: %s", sh.name, scriptTimeout, o.String(), e.String())
	}
	if err != nil {
		var x *exec.ExitError
		if errors.As(err, &x) {
			return x.ExitCode(), o.String(), e.String()
		}
		t.Fatalf("%s: %v", sh.name, err)
	}
	return 0, o.String(), e.String()
}

// fakeMinisign writes a stand-in for minisign into dir (a .ps1 for the
// PowerShell script, a bash script for the sh one). Its "signature" is the
// SHA-256 of the bytes it signed (line 4 of the .minisig); -V checks that
// against the file and that -P is FAKE_MINISIGN_KEY. Environment switches:
//   - FAKE_MINISIGN_TAMPER: append to update.json after signing (changed
//     while minisign waited for the password);
//   - FAKE_MINISIGN_SWAP: sign other bytes, then put update.json back as it
//     was (changed and restored while minisign read it).
func fakeMinisign(t *testing.T, dir string, forPS bool) string {
	t.Helper()
	if forPS {
		p := filepath.Join(dir, "fake-minisign.ps1")
		body := `$mode = ""; $m = ""; $x = ""; $tc = ""; $P = ""
for ($i = 0; $i -lt $args.Count; $i++) {
  switch -CaseSensitive ("$($args[$i])") {
    "-S" { $mode = "S" }
    "-V" { $mode = "V" }
    "-m" { $i++; $m = $args[$i] }
    "-x" { $i++; $x = $args[$i] }
    "-t" { $i++; $tc = $args[$i] }
    "-P" { $i++; $P = $args[$i] }
    "-s" { $i++ }
    "-c" { $i++ }
    "-p" { $i++ }
  }
}
function Fake-Sha256([string]$f) { (Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash.ToLowerInvariant() }
if ($mode -eq "V") {
  if ($P -ne $env:FAKE_MINISIGN_KEY) { [Console]::Error.WriteLine("stand-in: wrong public key $P"); exit 1 }
  $l = [System.IO.File]::ReadAllLines($x)
  if ($l[3] -ne (Fake-Sha256 $m)) { [Console]::Error.WriteLine("stand-in: signature verification failed"); exit 1 }
  Write-Host "Signature and comment signature verified"; exit 0
}
if ($env:FAKE_MINISIGN_SWAP) {
  $orig = [System.IO.File]::ReadAllBytes($m)
  [System.IO.File]::WriteAllText($m, "other content" + [char]10)
}
$sum = Fake-Sha256 $m
if ($env:FAKE_MINISIGN_SWAP) { [System.IO.File]::WriteAllBytes($m, $orig) }
[System.IO.File]::WriteAllLines($x, [string[]]@("untrusted comment: stand-in", "RWstandin", "trusted comment: $tc", $sum))
if ($env:FAKE_MINISIGN_TAMPER) { [System.IO.File]::AppendAllText($m, "tampered" + [char]10) }
exit 0
`
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, "fake-minisign")
	body := `#!/usr/bin/env bash
mode='' m='' x='' tc='' P=''
while [ $# -gt 0 ]; do
  case "$1" in
    -S) mode=S; shift ;;
    -V) mode=V; shift ;;
    -m) m="$2"; shift 2 ;;
    -x) x="$2"; shift 2 ;;
    -t) tc="$2"; shift 2 ;;
    -P) P="$2"; shift 2 ;;
    -s|-c|-p) shift 2 ;;
    *) shift ;;
  esac
done
h() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | sed 's/^\\//' | cut -d' ' -f1; }
if [ "$mode" = V ]; then
  [ "$P" = "${FAKE_MINISIGN_KEY:-}" ] || { echo "stand-in: wrong public key $P" >&2; exit 1; }
  [ "$(sed -n 4p "$x")" = "$(h "$m")" ] || { echo "stand-in: signature verification failed" >&2; exit 1; }
  echo "Signature and comment signature verified"; exit 0
fi
if [ -n "${FAKE_MINISIGN_SWAP:-}" ]; then cp "$m" "$m.orig"; printf 'other content\n' > "$m"; fi
sum="$(h "$m")"
[ -z "${FAKE_MINISIGN_SWAP:-}" ] || mv -f "$m.orig" "$m"
printf 'untrusted comment: stand-in\nRWstandin\ntrusted comment: %s\n%s\n' "$tc" "$sum" > "$x"
[ -z "${FAKE_MINISIGN_TAMPER:-}" ] || printf 'tampered\n' >> "$m"
exit 0
`
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const noKeyMsg = "the secret key file does not exist"

func TestSignScriptsCheckBeforeSigning(t *testing.T) {
	shells := scriptShells(t)
	for _, sh := range shells {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Run("consistent release", func(t *testing.T) {
				t.Parallel()
				dir, mf := signFixture(t)
				secret := filepath.Join(t.TempDir(), "no-such.key")
				for _, ch := range []string{"", "stable"} {
					// a signature from an earlier run must not survive a failed one
					if err := os.WriteFile(mf+".minisig", []byte("old signature\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					code, out, errOut := runSignScript(t, sh, signArgs{tag: "v3.2.0", dir: dir, manifest: mf, channel: ch, secret: secret})
					if code == 0 || !strings.Contains(out, "verified: 4 packages") || !strings.Contains(out, "checked update.json sha256 ") ||
						!strings.Contains(errOut, noKeyMsg) {
						t.Fatalf("channel %q: want the check passed and the key asked for; exit %d\nstdout: %s\nstderr: %s", ch, code, out, errOut)
					}
					if _, err := os.Stat(mf + ".minisig"); err == nil {
						t.Fatal("the old signature is still there, or a new one was written")
					}
				}
				// also when the check refuses
				os.WriteFile(mf+".minisig", []byte("old signature\n"), 0o644)
				if code, _, _ := runSignScript(t, sh, signArgs{tag: "v9.9.9", dir: dir, manifest: mf, secret: secret}); code != 1 {
					t.Fatalf("wrong tag accepted: exit %d", code)
				}
				if _, err := os.Stat(mf + ".minisig"); err == nil {
					t.Fatal("the old signature survived a refused run")
				}
			})
			t.Run("mirrors only when allowed", func(t *testing.T) {
				t.Parallel()
				dir, mf := signFixture(t)
				editManifest(t, mf, func(m *selfupdate.Manifest) { m.Mirrors = append(m.Mirrors, mirrorRule) })
				secret := filepath.Join(t.TempDir(), "no-such.key")
				code, out, errOut := runSignScript(t, sh, signArgs{tag: "v3.2.0", dir: dir, manifest: mf, secret: secret})
				if code != 1 || !strings.Contains(errOut, "mirrors") || strings.Contains(errOut, noKeyMsg) {
					t.Fatalf("mirrors refused by default; exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
				}
				code, out, errOut = runSignScript(t, sh, signArgs{tag: "v3.2.0", dir: dir, manifest: mf, secret: secret, allowMirrors: true})
				if code == 0 || !strings.Contains(out, "MIRROR RULE") || !strings.Contains(errOut, noKeyMsg) {
					t.Fatalf("allowed mirrors pass the check and are shown; exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
				}
			})
			t.Run("signing with a stand-in minisign", func(t *testing.T) {
				t.Parallel()
				tools := t.TempDir()
				fake := fakeMinisign(t, tools, sh.name == "ps1")
				secret := filepath.Join(tools, "dummy.key")
				if err := os.WriteFile(secret, []byte("not a key\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				key := "FAKE_MINISIGN_KEY=" + selfupdate.PublicKey
				dir, mf := signFixture(t)
				code, out, errOut := runSignScript(t, sh, signArgs{tag: "v3.2.0", dir: dir, manifest: mf, secret: secret, minisign: fake}, key)
				if code != 0 || !strings.Contains(out, "Wrote ") || !strings.Contains(out, "signature verified") {
					t.Fatalf("unchanged update.json: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
				}
				if b, err := os.ReadFile(mf + ".minisig"); err != nil || !strings.Contains(string(b), "trusted comment: lectureflow update version:3.2.0") {
					t.Fatalf("signature file: %v %q", err, b)
				}

				for name, c := range map[string]struct{ env, want string }{
					// update.json replaced while minisign waited for the password
					"changed": {"FAKE_MINISIGN_TAMPER=1", "changed while minisign signed it"},
					// other bytes signed, update.json restored before the hash check
					"swapped and restored": {"FAKE_MINISIGN_SWAP=1", "does not verify against"},
					// verification with the key the application is built with
					"another key": {"FAKE_MINISIGN_KEY=RWother", "does not verify against"},
				} {
					dir, mf = signFixture(t)
					env := []string{key, c.env}
					code, out, errOut = runSignScript(t, sh, signArgs{tag: "v3.2.0", dir: dir, manifest: mf, secret: secret, minisign: fake}, env...)
					if code != 1 || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, "the signature was deleted") {
						t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", name, code, out, errOut)
					}
					if _, err := os.Stat(mf + ".minisig"); err == nil {
						t.Fatalf("%s: the signature was kept", name)
					}
				}
			})
			for _, c := range signCases() {
				c := c
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					dir, mf := signFixture(t)
					if c.mutate != nil {
						c.mutate(t, dir, mf)
					}
					tag := c.tag
					if tag == "" {
						tag = "v3.2.0"
					}
					secret := filepath.Join(t.TempDir(), "no-such.key")
					code, out, errOut := runSignScript(t, sh, signArgs{tag: tag, dir: dir, manifest: mf, channel: c.channel, secret: secret})
					if code != 1 || !strings.Contains(errOut, "REFUSED") || !strings.Contains(errOut, c.want) ||
						!strings.Contains(errOut, "nothing was signed") || strings.Contains(errOut, noKeyMsg) {
						t.Fatalf("want a refusal naming %q before the key; exit %d\nstdout: %s\nstderr: %s", c.want, code, out, errOut)
					}
				})
			}
		})
	}
}
