// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"lectureflow/internal/model"
)

func TestMigration014Upgrade(t *testing.T) {
	ctx := context.Background()
	d, err := openRaw(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrateUpTo(ctx, d, 13); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO asr_device (id, backend, label, enabled, model, precision, max_parallel, priority) VALUES ('cuda:0','faster-whisper','GPU',1,'large-v3','auto',1,300)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, d); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM remote_server`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("remote_server: %d %v", n, err)
	}
	// The asr_device CHECK is unchanged: an ssh row carries its engine.
	if _, err := d.Exec(`INSERT INTO asr_device (id, backend, label, enabled, model, precision, max_parallel, priority) VALUES ('ssh:mac','mlx','Mac',1,'large-v3-turbo','fp16',1,6)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO asr_device (id, backend, label, enabled, model, precision, max_parallel, priority) VALUES ('ssh:x','ssh','X',1,'m','p',1,6)`); err == nil {
		t.Fatal("the CHECK accepted backend ssh")
	}
}

func TestRemoteServerCRUD(t *testing.T) {
	ctx := context.Background()
	st := openTest(t).(*sqlStore)
	r := RemoteServer{ID: "mac", Name: "Mac", Host: "203.0.113.5", Port: 22, Username: "alice", KeyRef: "remote.ssh.key.mac",
		PublicKey: "restrict ssh-ed25519 AAAA lectureflow-mac", CreatedAt: Now(), UpdatedAt: Now()}
	if err := st.InsertRemoteServer(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRemoteServer(ctx, r); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := st.GetRemoteServer(ctx, "mac")
	if err != nil || got.JumpJSON != "[]" || got.ProbeJSON != "{}" || got.SettingsJSON != "{}" || got.Port != 22 {
		t.Fatalf("%+v %v", got, err)
	}
	got.HostKey, got.Platform, got.Engine = "ssh-ed25519 AAAAhost", "darwin-arm64", model.BackendMLX
	if err := st.UpdateRemoteServer(ctx, got); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListRemoteServers(ctx)
	if err != nil || len(list) != 1 || list[0].HostKey == "" {
		t.Fatalf("%+v %v", list, err)
	}
	if err := st.UpdateRemoteServer(ctx, RemoteServer{ID: "gone"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing row: %v", err)
	}
	if err := st.SetKV(ctx, r.KeyRef, "enc:v1:xyz"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRemoteServer(ctx, "mac", r.KeyRef); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRemoteServer(ctx, "mac"); !errors.Is(err, ErrNotFound) {
		t.Fatal("row left")
	}
	if _, ok, _ := st.GetKV(ctx, r.KeyRef); ok {
		t.Fatal("key left")
	}
	r.Port = 70000
	if err := st.InsertRemoteServer(ctx, r); err == nil {
		t.Fatal("port CHECK")
	}
}

func TestDeleteASRDevice(t *testing.T) {
	ctx := context.Background()
	st := openTest(t).(*sqlStore)
	d := model.ASRDevice{ID: "ssh:mac", Backend: model.BackendMLX, Label: "Mac", Enabled: true, Model: "small", Precision: "fp16",
		MaxParallel: 1, Priority: 6}
	if err := st.InsertASRDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteASRDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetASRDevice(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("row left")
	}
	if err := st.DeleteASRDevice(ctx, d.ID); err != nil {
		t.Fatal("a missing row is not an error")
	}
}
