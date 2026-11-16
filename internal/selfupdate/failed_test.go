// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A version whose installation did not work (result.json rolled_back or
// failed) is not offered again, and its package is removed, until a
// strictly newer version appears; "try again" (downloading it) stays.

// electronFailed plays the desktop shell after a failed installation of
// version: pending.json is gone (renamed to applying.json, then finished)
// and result.json says outcome at time at.
func electronFailed(t *testing.T, data, version, outcome, at string) {
	t.Helper()
	dir := filepath.Join(data, "updates")
	if err := os.Remove(filepath.Join(dir, pendingFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Result{Version: version, From: "3.1.0", Outcome: outcome, At: at, Error: "the service did not answer within 30 seconds"})
	if err := os.WriteFile(filepath.Join(dir, resultFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func failedKV(t *testing.T, kv *memKV) *failedRecord {
	t.Helper()
	v, _, _ := kv.GetKV(bg, KVFailedInstall)
	if v == "" {
		return nil
	}
	var r failedRecord
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		t.Fatalf("kv %s: %q: %v", KVFailedInstall, v, err)
	}
	return &r
}

func TestFailedInstallIsRememberedUntilANewerVersion(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	data, kv := privateDataDir(t), newMemKV()
	opts := func(v string) func(*Options) {
		return func(o *Options) { o.DataDir, o.KV, o.Version = data, kv, v }
	}
	pkgDir := filepath.Join(data, "updates", "3.2.0")
	start := func() *Updater {
		t.Helper()
		u := newTestUpdater(t, f, k, opts("3.1.0"))
		t.Cleanup(u.Close)
		return u
	}

	u := start()
	u.Check(bg)
	waitDownloaded(t, u)
	u.Close()
	if !exists(pkgDir) {
		t.Fatal("no package")
	}

	// 3.2.0 did not start and the shell went back to 3.1.0
	electronFailed(t, data, "3.2.0", OutcomeRolledBack, "2026-11-03T08:00:00Z")
	u = start()
	st := u.Status(bg)
	if st.State != StateInstallFailed || st.InstallFailed == nil || st.InstallFailed.Version != "3.2.0" ||
		st.InstallFailed.Outcome != OutcomeRolledBack || st.InstallFailed.At != "2026-11-03T08:00:00Z" || st.InstallFailed.Error == "" {
		t.Fatalf("after the failure: %+v %+v", st, st.InstallFailed)
	}
	// "try again" needs the version: still known (latest.json)
	if st.Available == nil || st.Available.Version != "3.2.0" {
		t.Fatalf("available: %+v", st.Available)
	}
	if exists(pkgDir) {
		t.Error("the failed package is still there")
	}
	if !exists(filepath.Join(data, "updates", latestFile)) {
		t.Error("latest.json removed")
	}
	if r := failedKV(t, kv); r == nil || r.Version != "3.2.0" || r.Retry {
		t.Fatalf("kv: %+v", r)
	}
	u.Close()

	// remembered across starts
	u = start()
	if st := u.Status(bg); st.State != StateInstallFailed || st.InstallFailed.Version != "3.2.0" {
		t.Fatalf("after a restart: %+v", st)
	}
	// a check that finds the same version again: still not "available"
	if st, _ := u.Check(bg); st.State != StateInstallFailed || st.InstallFailed == nil || st.Available.Version != "3.2.0" {
		t.Fatalf("checked again: %+v", st)
	}
	if exists(pkgDir) {
		t.Error("the check brought the package back")
	}

	// "try again": the download is allowed, the failure no longer applies
	st = waitDownloaded(t, u)
	if st.InstallFailed != nil || st.Pending.Version != "3.2.0" {
		t.Fatalf("retry: %+v", st)
	}
	if r := failedKV(t, kv); r == nil || !r.Retry {
		t.Fatalf("kv after the retry: %+v", r)
	}
	u.Close()
	// a restart before the installation: pending, the old result.json is
	// not recorded again
	u = start()
	if st := u.Status(bg); st.State != StatePending || st.InstallFailed != nil {
		t.Fatalf("pending after the retry: %+v", st)
	}
	u.Close()

	// it fails again: recorded again, the package goes again
	electronFailed(t, data, "3.2.0", OutcomeFailed, "2026-11-04T09:00:00Z")
	u = start()
	if st := u.Status(bg); st.State != StateInstallFailed || st.InstallFailed.At != "2026-11-04T09:00:00Z" || st.InstallFailed.Outcome != OutcomeFailed {
		t.Fatalf("failed again: %+v %+v", st, st.InstallFailed)
	}
	if exists(pkgDir) {
		t.Error("the package of the second failure is still there")
	}

	// a strictly newer version: offered as usual, the record is gone
	f.publishLatest(makeRelease(t, f, k, "3.3.0", ChannelStable, nil))
	if st, _ := u.Check(bg); st.State != StateAvailable || st.Available.Version != "3.3.0" || st.InstallFailed != nil {
		t.Fatalf("newer version: %+v", st)
	}
	if r := failedKV(t, kv); r != nil {
		t.Fatalf("kv after a newer version: %+v", r)
	}
	u.Close()
	u = start()
	if st := u.Status(bg); st.State != StateAvailable || st.InstallFailed != nil {
		t.Fatalf("restart after a newer version: %+v", st)
	}
}

// A retry whose download does not finish: the user asked to try it again,
// so the version is offered as usual (not "failed" again from the same
// result.json).
func TestFailedInstallRetryThatDoesNotFinish(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	data, kv := privateDataDir(t), newMemKV()
	opts := func(o *Options) { o.DataDir, o.KV = data, kv }
	u := newTestUpdater(t, f, k, opts)
	u.Check(bg)
	waitDownloaded(t, u) // (only a version Go verified is remembered as failed)
	u.Close()
	electronFailed(t, data, "3.2.0", OutcomeRolledBack, "2026-11-03T08:00:00Z")
	u = newTestUpdater(t, f, k, opts)
	if st := u.Status(bg); st.State != StateInstallFailed {
		t.Fatalf("%+v", st)
	}
	f.setStatus(downloadPath("3.2.0", "LectureFlow-Setup-3.2.0-x64.exe"), 500)
	if _, err := u.Download(bg); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, u, notState(StateDownloading))
	if st.State != StateError || st.InstallFailed != nil {
		t.Fatalf("failed retry: %+v", st)
	}
	u.Close()
	u = newTestUpdater(t, f, k, opts)
	if st := u.Status(bg); st.State != StateAvailable || st.InstallFailed != nil {
		t.Fatalf("restart after the retry: %+v", st)
	}
}

// What is not a failed installation of a newer version.
func TestFailedInstallNotRecorded(t *testing.T) {
	k := newTestKey(t)
	for name, c := range map[string]struct {
		result  string
		pending bool
		version string
	}{
		"installed":  {`{"version":"3.2.0","from":"3.1.0","outcome":"installed","at":"2026-11-03T08:00:00Z"}`, false, "3.1.0"},
		"kept":       {`{"version":"3.2.0","from":"3.1.0","outcome":"failed","at":"2026-11-03T08:00:00Z","error":"no verdict; not installed at this start"}`, true, "3.1.0"},
		"this one":   {`{"version":"3.1.0","from":"3.0.0","outcome":"failed","at":"2026-11-03T08:00:00Z"}`, false, "3.1.0"},
		"older":      {`{"version":"3.0.0","from":"2.9.0","outcome":"rolled_back","at":"2026-11-03T08:00:00Z"}`, false, "3.1.0"},
		"by hand":    {`{"version":"3.2.0","from":"3.1.0","outcome":"rolled_back","at":"2026-11-03T08:00:00Z"}`, false, "3.2.0"},
		"traversal":  {`{"version":"../../victim","from":"3.1.0","outcome":"failed","at":"2026-11-03T08:00:00Z"}`, false, "3.1.0"},
		"separator":  {`{"version":"3.2.0/../../victim","from":"3.1.0","outcome":"failed","at":"2026-11-03T08:00:00Z"}`, false, "3.1.0"},
		"unreadable": {`{`, false, "3.1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			data, kv := privateDataDir(t), newMemKV()
			dir := filepath.Join(data, "updates")
			os.MkdirAll(filepath.Join(dir, "3.2.0"), 0o755)
			os.WriteFile(filepath.Join(dir, "3.2.0", "x.exe"), []byte("x"), 0o644)
			victim := filepath.Join(data, "victim")
			os.MkdirAll(victim, 0o755)
			if c.pending {
				os.WriteFile(filepath.Join(dir, pendingFile), []byte(`{"schema":1,"version":"3.2.0","platform":"windows-x64","file":"3.2.0/x.exe","size":1}`), 0o644)
			}
			os.WriteFile(filepath.Join(dir, resultFile), []byte(c.result), 0o644)
			u := newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.KV, o.Version = data, kv, c.version })
			st := u.Status(bg)
			if st.InstallFailed != nil || st.State == StateInstallFailed {
				t.Fatalf("%+v", st)
			}
			if r := failedKV(t, kv); r != nil {
				t.Fatalf("kv: %+v", r)
			}
			if !exists(victim) {
				t.Fatal("a directory outside the updates folder was removed")
			}
			if c.pending && !exists(filepath.Join(dir, "3.2.0", "x.exe")) {
				t.Fatal("the pending package was removed")
			}
		})
	}
}

// The record itself: dropped once this version is not older; an unreadable
// or strange one is ignored and removes nothing.
func TestFailedInstallRecord(t *testing.T) {
	k := newTestKey(t)
	rec := `{"version":"3.2.0","at":"2026-11-03T08:00:00Z","outcome":"rolled_back"}`

	// 3.2.0 was installed by hand since: forgotten
	data, kv := privateDataDir(t), newMemKV()
	kv.SetKV(bg, KVFailedInstall, rec)
	u := newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.KV, o.Version = data, kv, "3.2.0" })
	if st := u.Status(bg); st.InstallFailed != nil {
		t.Fatalf("installed by hand: %+v", st)
	}
	if r := failedKV(t, kv); r != nil {
		t.Fatalf("kv not cleared: %+v", r)
	}

	// still in effect without result.json (it is the record that counts)
	data, kv = privateDataDir(t), newMemKV()
	kv.SetKV(bg, KVFailedInstall, rec)
	u = newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.KV = data, kv })
	if st := u.Status(bg); st.State != StateInstallFailed || st.InstallFailed.Version != "3.2.0" || st.Available != nil {
		t.Fatalf("record alone: %+v", st)
	}

	for _, bad := range []string{`{`, `{"version":"..","at":"x"}`, `{"version":"../3.2.0"}`, `{"version":"3.2.0\\..\\..\\x"}`, `""`, `[]`} {
		data, kv = privateDataDir(t), newMemKV()
		victim := filepath.Join(data, "x")
		os.MkdirAll(victim, 0o755)
		kv.SetKV(bg, KVFailedInstall, bad)
		u = newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.KV = data, kv })
		if st := u.Status(bg); st.InstallFailed != nil || st.State == StateInstallFailed {
			t.Errorf("%s: %+v", bad, st)
		}
		if !exists(victim) {
			t.Errorf("%s: removed a directory outside the updates folder", bad)
		}
	}
}

// An offline update of the failed version is trying it again; of a newer
// one, supersedes it.
func TestFailedInstallOfflineUpdate(t *testing.T) {
	k := newTestKey(t)
	for _, c := range []struct{ version, wantKV string }{{"3.2.0", "retry"}, {"3.3.0", "none"}} {
		t.Run(c.version, func(t *testing.T) {
			data, kv := privateDataDir(t), newMemKV()
			kv.SetKV(bg, KVFailedInstall, `{"version":"3.2.0","at":"2026-11-03T08:00:00Z","outcome":"failed"}`)
			u := offlineUpdater(t, k, func(o *Options) { o.DataDir, o.KV = data, kv })
			rel := makeRelease(t, nil, k, c.version, ChannelStable, nil)
			name := "LectureFlow-Setup-" + c.version + "-x64.exe"
			src := t.TempDir()
			st, err := u.ImportPaths(bg, []string{userFile(t, src, name, rel.pkgs[name]), userFile(t, src, "update.json", rel.raw), userFile(t, src, "update.json.minisig", rel.sig)})
			if err != nil || st.State != StatePending || st.InstallFailed != nil {
				t.Fatalf("%+v %v", st, err)
			}
			r := failedKV(t, kv)
			switch c.wantKV {
			case "retry":
				if r == nil || !r.Retry {
					t.Fatalf("kv: %+v", r)
				}
			default:
				if r != nil {
					t.Fatalf("kv: %+v", r)
				}
			}
			if !exists(filepath.Join(u.Dir(), c.version, name)) {
				t.Fatal("the imported package was removed")
			}
		})
	}
}

// SEC12 L-3: a planted or stale result.json does not hold back the version
// on offer. Only an update from this version, of a version whose signed
// update.json Go keeps in updates/<version>/, is remembered as failed.
func TestFailedInstallNeedsThisVersionAndAVerifiedManifest(t *testing.T) {
	k := newTestKey(t)
	other := newTestKey(t)
	for name, c := range map[string]struct {
		from     string
		manifest func(dir string) // what updates/3.2.0/ holds
		want     bool
	}{
		"verified":        {"3.1.0", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)) }, true},
		"from another":    {"3.0.0", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)) }, false},
		"no from":         {"", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)) }, false},
		"no manifest":     {"3.1.0", func(string) {}, false},
		"another key":     {"3.1.0", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, other, "3.2.0", ChannelStable, nil)) }, false},
		"another version": {"3.1.0", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, k, "3.3.0", ChannelStable, nil)) }, false},
		"no signature file": {"3.1.0", func(dir string) {
			os.WriteFile(filepath.Join(dir, manifestFile), makeRelease(t, nil, k, "3.2.0", ChannelStable, nil).raw, 0o644)
		}, false},
		// SEC12 review residual: in a data directory other accounts may
		// change, result.json and update.json may be theirs
		"untrusted data directory": {"3.1.0", func(dir string) { writeRelease(t, dir, makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "untrusted data directory" {
				orig := checkTrust
				t.Cleanup(func() { checkTrust = orig })
				checkTrust = func(dir string) error {
					return cerr(CodeUntrustedDir, map[string]any{"path": dir, "who": "Authenticated Users"}, errors.New("test"))
				}
			}
			data, kv := privateDataDir(t), newMemKV()
			dir := filepath.Join(data, "updates")
			// the version on offer (a check found it): latest.json
			rel := makeRelease(t, nil, k, "3.2.0", ChannelStable, nil)
			os.MkdirAll(filepath.Join(dir, "3.2.0"), 0o755)
			os.WriteFile(filepath.Join(dir, latestFile), rel.raw, 0o644)
			os.WriteFile(filepath.Join(dir, latestSigFile), rel.sig, 0o644)
			c.manifest(filepath.Join(dir, "3.2.0"))
			b, _ := json.Marshal(Result{Version: "3.2.0", From: c.from, Outcome: OutcomeRolledBack, At: "2026-11-03T08:00:00Z"})
			os.WriteFile(filepath.Join(dir, resultFile), b, 0o644)
			u := newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.KV = data, kv })
			st := u.Status(bg)
			if got := st.State == StateInstallFailed && st.InstallFailed != nil; got != c.want {
				t.Fatalf("remembered %v, want %v: %+v", got, c.want, st)
			}
			if !c.want {
				if st.State != StateAvailable || st.Available == nil || st.Available.Version != "3.2.0" {
					t.Fatalf("the version on offer is held back: %+v", st)
				}
				if r := failedKV(t, kv); r != nil {
					t.Fatalf("kv: %+v", r)
				}
			}
		})
	}
}

// writeRelease puts rel's update.json and signature into dir.
func writeRelease(t *testing.T, dir string, rel *release) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, manifestFile), rel.raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, signatureFile), rel.sig, 0o644); err != nil {
		t.Fatal(err)
	}
}
