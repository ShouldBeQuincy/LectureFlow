// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sshremote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"lectureflow/internal/model"
	"lectureflow/internal/remote"
	"lectureflow/internal/secrets"
)

type memKV map[string]string

func (m memKV) GetKV(_ context.Context, k string) (string, bool, error) {
	v, ok := m[k]
	return v, ok && v != "", nil
}
func (m memKV) SetKV(_ context.Context, k, v string) error { m[k] = v; return nil }

func TestKeysAreSealed(t *testing.T) {
	ctx := context.Background()
	sealed, line, err := newKey("lectureflow-test", KeyRef("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsBound(sealed) || strings.Contains(sealed, "PRIVATE KEY") {
		t.Fatal("the private key is not sealed bound")
	}
	if !strings.HasPrefix(line, "restrict ssh-ed25519 ") || !strings.HasSuffix(line, " lectureflow-test") {
		t.Fatalf("authorized_keys line %q", line)
	}
	kv := memKV{KeyRef("s1"): sealed}
	signer, err := loadSigner(ctx, kv, "s1", KeyRef("s1"))
	if err != nil {
		t.Fatal(err)
	}
	if PublicFingerprint(line) != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatal("the public key does not belong to the private key")
	}
	// A plain value is refused (never used, never written).
	kv["plain"] = "-----BEGIN OPENSSH PRIVATE KEY-----"
	if _, err := loadSigner(ctx, kv, "s1", "plain"); err == nil {
		t.Fatal("a plain key was used")
	}
	// sec3 L3: the key opens for its own server only. Copied into another
	// server's row, or reached through another server's key_ref, it does
	// not.
	kv[KeyRef("s2")] = sealed
	if _, err := loadSigner(ctx, kv, "s2", KeyRef("s2")); !errors.Is(err, secrets.ErrUndecryptable) {
		t.Fatalf("s1's key copied into s2's row: %v", err)
	}
	if _, err := loadSigner(ctx, kv, "s2", KeyRef("s1")); !errors.Is(err, secrets.ErrUndecryptable) {
		t.Fatalf("s2 with key_ref pointing at s1's row: %v", err)
	}
	// 3.2.0's unbound format (sealed again bound at the next start) still opens
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "old")
	old, err := secrets.Seal(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	kv[KeyRef("s3")] = old
	if _, err := loadSigner(ctx, kv, "s3", KeyRef("s3")); err != nil {
		t.Fatalf("enc:v1: key: %v", err)
	}
}

func TestAgentCommandRefusesArguments(t *testing.T) {
	id := strings.Repeat("a", 16)
	cmd, err := agentCommand(remote.PlatformDarwinArm64, id, "put", "--job", "01A", "--kind", "audio", "--offset", "0")
	if err != nil {
		t.Fatal(err)
	}
	want := `sh -c 'exec "$HOME/Library/Application Support/LectureFlow Agent/agent/` + id + `/lf-agent" "$@"' lf-agent put --job 01A --kind audio --offset 0`
	if cmd != want {
		t.Fatalf("%s\n%s", cmd, want)
	}
	for _, a := range []string{"a b", "$(id)", "`id`", "a;b", "a'b", "", "x\ny", "../x/y"} {
		if _, err := agentCommand(remote.PlatformLinuxX64, id, "get", a); err == nil {
			t.Errorf("accepted %q", a)
		}
	}
	if shellQuote(`it's`) != `'it'\''s'` {
		t.Fatal("quote")
	}
	if !strings.Contains(installScript(remote.PlatformLinuxX64, id), "${XDG_DATA_HOME:-$HOME/.local/share}/lectureflow-agent") {
		t.Fatal("linux root")
	}
}

func TestPlatformOfUname(t *testing.T) {
	for in, want := range map[string]string{"Darwin arm64\n": remote.PlatformDarwinArm64, "Linux x86_64": remote.PlatformLinuxX64} {
		if got, err := platformOfUname(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"Darwin x86_64", "Linux aarch64", "MINGW64_NT x86_64", "", "a b c"} {
		if _, err := platformOfUname(in); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestHostKeyCallback(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(priv2)
	pinned := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	var seen ssh.PublicKey
	cb := hostKeyCallback(pinned, func(k ssh.PublicKey) { seen = k })
	if err := cb("h", nil, s.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := cb("h", nil, other.PublicKey()); !errors.Is(err, errHostKeyMismatch) || seen == nil {
		t.Fatalf("other key: %v", err)
	}
	if err := hostKeyCallback("", nil)("h", nil, s.PublicKey()); !errors.Is(err, errHostKeyUnknown) {
		t.Fatalf("unpinned: %v", err)
	}
	info := describeKey(s.PublicKey(), "host", 22)
	if info.Algorithm != "ssh-ed25519" || !strings.HasPrefix(info.Fingerprint, "SHA256:") || !strings.Contains(info.Check, "ssh_host_ed25519_key.pub") {
		t.Fatalf("%+v", info)
	}
}

// The kit of a Mac from the real component manifest: python-build-standalone,
// the asr-mlx lock, the MLX model, this build's worker scripts.
func TestKitFromManifestDarwin(t *testing.T) {
	manifest := filepath.Join("..", "..", "packaging", "components", "components.json")
	k, err := kitFromManifest(manifest, filepath.Join("..", "..", "worker"), "3.1.0", remote.PlatformDarwinArm64,
		remote.EngineMLX, "large-v3-turbo", model.PrecisionFP16)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Python.URL, "https://github.com/astral-sh/python-build-standalone/") || k.Python.Exe != "python/bin/python3" ||
		k.Lock.Name != "asr-mlx-darwin-arm64.txt" || k.Model.ID != "model:mlx:large-v3-turbo:fp16" || len(k.Worker) != len(WorkerFiles) ||
		!strings.HasPrefix(k.Model.Files[0].URL, "https://huggingface.co/") || k.IndexURL != "https://pypi.org/simple" {
		t.Fatalf("%+v", k)
	}
	if _, err := kitFromManifest(manifest, filepath.Join("..", "..", "worker"), "3.1.0", remote.PlatformDarwinArm64,
		remote.EngineMLX, "no-such-model", model.PrecisionFP16); err == nil {
		t.Fatal("an unknown model was accepted")
	}
}

func TestModelPrecision(t *testing.T) {
	if p := modelPrecision(remote.EngineMLX, model.PrecisionFP16, -1); p != model.PrecisionFP16 {
		t.Fatal(p)
	}
	if p := modelPrecision(remote.EngineFasterWhisper, model.PrecisionAuto, 24000); p != model.PrecisionFloat16 && p != model.PrecisionInt8Float16 {
		t.Fatal(p)
	}
	if p := modelPrecision(remote.EngineFasterWhisper, model.PrecisionInt8Float16, 24000); p != model.PrecisionInt8Float16 {
		t.Fatal(p)
	}
}

func TestAddValidation(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	for name, req := range map[string]AddRequest{
		"no name":   {Host: "h", Username: "u"},
		"bad host":  {Name: "x", Host: "h;rm -rf", Username: "u"},
		"bad user":  {Name: "x", Host: "h", Username: "-oProxyCommand=x"},
		"bad port":  {Name: "x", Host: "h", Port: 70000, Username: "u"},
		"bad group": {Name: "x", Host: "h", Username: "u", Group: "first"},
		"bad slug":  {ID: "Bad Slug", Name: "x", Host: "h", Username: "u"},
	} {
		var fe *FieldError
		if _, err := e.m.Add(ctx, req); !errors.As(err, &fe) {
			t.Errorf("%s: %v", name, err)
		}
	}
	v, err := e.m.Add(ctx, AddRequest{Name: "Mac mini (lab)", Host: "mac.example.test", Username: "q"})
	if err != nil || v.ID != "mac-mini-lab" || v.Port != 22 || v.Settings.Group != GroupSupplement {
		t.Fatalf("%+v %v", v, err)
	}
	v2, err := e.m.Add(ctx, AddRequest{Name: "Mac mini (lab)", Host: "mac.example.test", Username: "q"})
	if err != nil || v2.ID != "mac-mini-lab-2" {
		t.Fatalf("second: %+v %v", v2, err)
	}
	// A changed host drops the confirmed host key.
	s := e.m.server(v.ID)
	s.mu.Lock()
	s.rec.HostKey = "ssh-ed25519 AAAA"
	s.mu.Unlock()
	host := "other.example.test"
	got, err := e.m.Update(ctx, v.ID, Patch{Host: &host})
	if err != nil || got.Trusted || got.State != StateUntrusted {
		t.Fatalf("%+v %v", got, err)
	}
	pk, err := e.m.PublicKeyOf(v.ID)
	if err != nil || !strings.Contains(pk.Command, "authorized_keys") || pk.Fingerprint == "" {
		t.Fatalf("%+v %v", pk, err)
	}
	if err := e.m.Remove(ctx, v.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.Get(v.ID); ok {
		t.Fatal("not removed")
	}
}

// The kit of a Linux + NVIDIA server from the real manifest (linux-x64:
// only a remote target).
func TestKitFromManifestLinux(t *testing.T) {
	manifest := filepath.Join("..", "..", "packaging", "components", "components.json")
	k, err := kitFromManifest(manifest, filepath.Join("..", "..", "worker"), "3.1.0", remote.PlatformLinuxX64,
		remote.EngineFasterWhisper, "large-v3", model.PrecisionFloat16)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.Python.URL, "x86_64-unknown-linux-gnu-install_only_stripped") || k.Lock.Name != "asr-cuda-linux-x64.txt" ||
		k.Model.ID != "model:faster-whisper:large-v3:float16" || len(k.Model.Files) < 4 {
		t.Fatalf("%+v", k)
	}
}

func TestHostKeyAlgorithms(t *testing.T) {
	if a := hostKeyAlgorithms(""); a[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("first contact %v", a)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	pinned := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	if a := hostKeyAlgorithms(pinned); len(a) != 1 || a[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("pinned %v", a)
	}
	if a := hostKeyAlgorithms("ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg="); len(a) != 1 || a[0] != ssh.KeyAlgoECDSA256 {
		t.Fatalf("pinned ecdsa %v", a)
	}
}
