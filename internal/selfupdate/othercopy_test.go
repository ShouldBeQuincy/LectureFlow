// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Copies of LectureFlow sharing one data directory (r6; security review
// SEC29 section 6, case 6): the installed and the portable copy of one
// account both use %APPDATA%\LectureFlow by default, and pending.json and
// previous.json have one slot each. The update one copy downloaded is not
// the other's: the other neither takes it for its own, nor removes it or
// its package, nor writes its own update over it.

// updatesState reads what the other copy's update consists of: pending.json,
// previous.json and the package, as bytes ("" = missing).
func updatesState(t *testing.T, dir string, pkg string) [3]string {
	t.Helper()
	var out [3]string
	for i, p := range []string{pendingFile, previousFile, pkg} {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err == nil {
			out[i] = string(b)
		}
	}
	return out
}

func TestOtherCopyPending(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ownerPortable bool   // the copy that downloaded the update
		version       string // of the copy that starts next
	}{
		{"installed copy's update, the portable copy starts", false, "3.1.0"},
		{"portable copy's update, the installed copy starts", true, "3.1.0"},
		{"installed copy's update, a portable copy of that very version starts", false, "3.2.0"},
		{"installed copy's update, a newer portable copy starts", false, "3.2.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			k := newTestKey(t)
			f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
			kv := newMemKV()
			data := privateDataDir(t)
			dir := filepath.Join(data, "updates")
			copyOf := func(portable bool, version string) *Updater {
				return newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV, o.Version, o.Portable = data, kv, version, portable })
			}
			ownerKind, ownerPkg := KindInstaller, "3.2.0/LectureFlow-Setup-3.2.0-x64.exe"
			otherPkg := "LectureFlow-3.3.0-x64-portable.exe"
			if tc.ownerPortable {
				ownerKind, ownerPkg = KindPortable, "3.2.0/LectureFlow-3.2.0-x64-portable.exe"
				otherPkg = "LectureFlow-Setup-3.3.0-x64.exe"
			}

			owner := copyOf(tc.ownerPortable, "3.1.0")
			owner.Check(bg)
			waitDownloaded(t, owner)
			owner.Close()
			before := updatesState(t, dir, ownerPkg)
			if before[0] == "" || before[1] == "" || before[2] == "" {
				t.Fatalf("the owner's update is not there: %q", before[:2])
			}

			// the other kind of copy starts on the same data directory
			u := copyOf(!tc.ownerPortable, tc.version)
			same := func(when string) {
				t.Helper()
				if got := updatesState(t, dir, ownerPkg); got != before {
					t.Fatalf("%s: the other copy's pending.json, previous.json or package changed", when)
				}
			}
			same("after the start (and its clean-up)")
			st := u.Status(bg)
			if st.Pending != nil || st.State == StatePending {
				t.Fatalf("another copy's update shown as this copy's: %+v", st)
			}

			// a newer version for this copy: offered, but not started while
			// the other copy's update waits
			f.publishLatest(makeRelease(t, f, k, "3.3.0", ChannelStable, nil))
			st, err := u.Check(bg)
			if err != nil || st.State != StateAvailable || st.Available == nil || st.Available.Version != "3.3.0" || st.Pending != nil {
				t.Fatalf("check: %+v %v", st, err)
			}
			st, err = u.Download(bg)
			wantCode(t, err, CodeOtherCopyPending)
			// kind names the copy the update belongs to as a code of its
			// own: the page shows that code's text in the sentence
			wantCopy := map[bool]string{false: CodeCopyInstaller, true: CodeCopyPortable}[tc.ownerPortable]
			info := InfoOf(err)
			kind, _ := info.Params["kind"].(map[string]any)
			if info.Params["version"] != "3.2.0" || len(info.Params) != 2 || len(kind) != 1 || kind["code"] != wantCopy {
				t.Fatalf("params %+v (the update is a %s package)", info.Params, ownerKind)
			}
			if b, err := json.Marshal(info); err != nil || !strings.Contains(string(b), `"kind":{"code":"`+wantCopy+`"}`) {
				t.Fatalf("as JSON: %s %v", b, err)
			}
			if st.State != StateAvailable || st.Progress != nil || st.Pending != nil || st.Error != nil {
				t.Fatalf("after the refused download: %+v", st)
			}
			if n := f.count(downloadPath("3.3.0", otherPkg)); n != 0 {
				t.Fatalf("%d requests for this copy's package", n)
			}
			if _, err := os.Stat(filepath.Join(dir, "3.3.0")); !os.IsNotExist(err) {
				t.Fatalf("a directory for this copy's update was made: %v", err)
			}
			same("after the refused download")

			// an offline update is refused the same way, nothing staged stays
			rel := makeRelease(t, nil, k, "3.3.0", ChannelStable, nil)
			tmp := t.TempDir()
			_, err = u.ImportPaths(bg, []string{userFile(t, tmp, "update.json", rel.raw), userFile(t, tmp, "update.json.minisig", rel.sig),
				userFile(t, tmp, otherPkg, rel.pkgs[otherPkg])})
			wantCode(t, err, CodeOtherCopyPending)
			noStaging(t, u)
			// refused before anything is staged or verified, not only at the
			// last look (makePending): nothing of the update is put into
			// updates/<version>/ (SEC40 G3)
			if _, err := os.Stat(filepath.Join(dir, "3.3.0")); !os.IsNotExist(err) {
				t.Fatalf("the refused offline update left updates/3.3.0: %v", err)
			}
			same("after the refused offline update")

			// "discard the pending update" of this copy has nothing to discard
			if st, err := u.DiscardPending(bg); err != nil || st.Pending != nil {
				t.Fatalf("discard: %+v %v", st, err)
			}
			same("after discarding")
			u.Close()

			// the owner still finds its update (and the newer version the
			// other copy's check left in the shared directory)
			owner = copyOf(tc.ownerPortable, "3.1.0")
			if st := owner.Status(bg); st.Pending == nil || st.Pending.Version != "3.2.0" {
				t.Fatalf("the owner lost its update: %+v", st)
			}
			same("after the owner started again")
			owner.Close()

			// ... and installs it (its shell renames pending.json): the other
			// copy's record is gone, this copy updates as ever
			if err := os.Rename(filepath.Join(dir, pendingFile), filepath.Join(dir, applyingFile)); err != nil {
				t.Fatal(err)
			}
			u = copyOf(!tc.ownerPortable, tc.version)
			u.Check(bg)
			st = waitDownloaded(t, u)
			if st.Pending == nil || st.Pending.Version != "3.3.0" || st.Pending.File != otherPkg {
				t.Fatalf("after the other copy's update was installed: %+v", st.Pending)
			}
		})
	}
}

// The clean-up keeps the package directory of another copy's pending
// update for that update's sake alone (SEC40 G1): here nothing else
// refers to it — it is not the running version's, not previous.json's,
// and no check result names it — at the start and at later clean-ups,
// also once the update is given up for its age (it is not removed for
// that). What nothing refers to still goes.
func TestOtherCopyPackageKeptByCleanup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ownerPortable bool
		later         time.Duration // how long after the download the other copy starts
	}{
		{"installed copy's update, the portable copy starts", false, time.Hour},
		{"portable copy's update, the installed copy starts", true, time.Hour},
		{"installed copy's update, given up for its age", false, 40 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGH(t)
			k := newTestKey(t)
			f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
			kv := newMemKV()
			data := privateDataDir(t)
			dir := filepath.Join(data, "updates")
			t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
			copyAt := func(portable bool, at time.Time) *Updater {
				return newTestUpdater(t, f, k, func(o *Options) {
					o.DataDir, o.KV, o.Portable = data, kv, portable
					o.Now = func() time.Time { return at }
				})
			}
			owner := copyAt(tc.ownerPortable, t0)
			owner.Check(bg)
			waitDownloaded(t, owner)
			owner.Close()
			pkg := "3.2.0/LectureFlow-Setup-3.2.0-x64.exe"
			if tc.ownerPortable {
				pkg = "3.2.0/LectureFlow-3.2.0-x64-portable.exe"
			}
			// nothing but pending.json refers to updates/3.2.0/
			for _, n := range []string{latestFile, latestSigFile} {
				if err := os.Remove(filepath.Join(dir, n)); err != nil {
					t.Fatal(err)
				}
			}
			var prev Previous
			if ok, err := readJSON(filepath.Join(dir, previousFile), &prev); !ok || err != nil || prev.Version != "3.1.0" {
				t.Fatalf("previous.json %+v %v", prev, err)
			}
			stale := filepath.Join(dir, "3.0.5") // a version directory nothing refers to
			if err := os.MkdirAll(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			before := updatesState(t, dir, pkg)

			u := copyAt(!tc.ownerPortable, t0.Add(tc.later))
			check := func(when string) {
				t.Helper()
				if got := updatesState(t, dir, pkg); got != before {
					t.Fatalf("%s: the other copy's package, pending.json or previous.json went or changed", when)
				}
				for _, n := range []string{manifestFile, signatureFile} {
					if _, err := os.Stat(filepath.Join(dir, "3.2.0", n)); err != nil {
						t.Fatalf("%s: %s of the other copy's update: %v", when, n, err)
					}
				}
			}
			check("after the start's clean-up")
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Fatalf("the clean-up did not run: %v", err)
			}
			if st := u.Status(bg); st.Pending != nil || st.Available != nil {
				t.Fatalf("status %+v", st)
			}
			u.cleanup(false)
			check("after a later clean-up")
			u.cleanup(true)
			check("after another clean-up of a start")
		})
	}
}

// The other copy installs its update while this copy runs (its record
// goes): no restart is needed, the next download starts.
func TestOtherCopyPendingGoneWhileRunning(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	kv := newMemKV()
	data := privateDataDir(t)
	dir := filepath.Join(data, "updates")
	copyOf := func(portable bool) *Updater {
		return newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV, o.Portable = data, kv, portable })
	}
	owner := copyOf(false)
	owner.Check(bg)
	waitDownloaded(t, owner)
	owner.Close()

	u := copyOf(true)
	u.Check(bg)
	_, err := u.Download(bg)
	wantCode(t, err, CodeOtherCopyPending)
	if err := os.Remove(filepath.Join(dir, pendingFile)); err != nil {
		t.Fatal(err)
	}
	st := waitDownloaded(t, u)
	if st.Pending == nil || st.Pending.File != "LectureFlow-3.2.0-x64-portable.exe" {
		t.Fatalf("pending %+v", st.Pending)
	}
}

// Another copy's update appears after this copy's download began (two
// services never share a data directory, so only something outside
// LectureFlow could do it): the finished download still does not replace
// pending.json or previous.json.
func TestOtherCopyPendingNeverOverwritten(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	u := newTestUpdater(t, f, k, func(o *Options) { o.Portable = true })
	u.Check(bg)
	dir := u.Dir()
	os.MkdirAll(filepath.Join(dir, "3.2.0"), 0o755)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	os.WriteFile(filepath.Join(dir, "3.2.0", name), rel.pkgs[name], 0o644)
	theirs := `{"schema":1,"version":"3.2.0","from_version":"3.1.0","platform":"windows-x64","kind":"installer","file":"3.2.0/` + name + `","size":` +
		strconv.Itoa(len(rel.pkgs[name])) + `}`
	theirPrev := `{"schema":1,"version":"3.1.0","platform":"windows-x64","kind":"installer","backup":"backup/3.1.0"}`
	os.WriteFile(filepath.Join(dir, pendingFile), []byte(theirs), 0o644)
	os.WriteFile(filepath.Join(dir, previousFile), []byte(theirPrev), 0o644)

	a, ok := rel.m.Asset(PlatformWindowsX64, KindPortable)
	if !ok {
		t.Fatal("no portable package in the release")
	}
	err := u.makePending(rel.m, a, OriginDownload)
	wantCode(t, err, CodeOtherCopyPending)
	if b, _ := os.ReadFile(filepath.Join(dir, pendingFile)); string(b) != theirs {
		t.Fatalf("pending.json replaced: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, previousFile)); string(b) != theirPrev {
		t.Fatalf("previous.json replaced: %s", b)
	}
	if st := u.Status(bg); st.Pending != nil {
		t.Fatalf("pending %+v", st.Pending)
	}
}

// Whose a pending update is: one rule for the service (otherCopys) and
// the desktop shell (selfupdate.js otherCopysPending). The same table, row
// for row, is in desktop/test/unit/selfupdate.test.js (OTHER_COPY_TABLE):
// change both together. A row: this copy (what it installs),
// pending.json's platform and kind, and whether the pending update is
// another copy's.
var otherCopyTable = []struct {
	copy, platform, kind string
	other                bool
}{
	{"installer", "windows-x64", "installer", false},
	{"installer", "windows-x64", "portable", true},
	{"installer", "windows-x64", "zip", false},
	{"installer", "windows-x64", "dmg", false},
	{"installer", "windows-x64", "msi", false},
	{"installer", "windows-x64", "", false},
	{"installer", "darwin-arm64", "portable", false},
	{"installer", "darwin-arm64", "zip", false},
	{"portable", "windows-x64", "installer", true},
	{"portable", "windows-x64", "portable", false},
	{"portable", "windows-x64", "zip", false},
	{"portable", "windows-x64", "dmg", false},
	{"portable", "windows-x64", "msi", false},
	{"portable", "windows-x64", "", false},
	{"portable", "darwin-arm64", "installer", false},
	{"zip", "darwin-arm64", "zip", false},
	{"zip", "darwin-arm64", "dmg", false},
	{"zip", "darwin-arm64", "installer", false},
	{"zip", "darwin-arm64", "portable", false},
	{"zip", "windows-x64", "installer", false},
	{"zip", "windows-x64", "portable", false},
	{"none", "windows-x64", "installer", true},
	{"none", "windows-x64", "portable", true},
	{"none", "windows-x64", "zip", true},
	{"none", "windows-x64", "msi", true},
	{"none", "windows-x64", "", true},
	{"none", "darwin-arm64", "zip", true},
	{"none", "darwin-arm64", "dmg", true},
}

// copyOptions sets o to a row's copy.
func copyOptions(t *testing.T, copy string, o *Options) {
	t.Helper()
	switch copy {
	case "installer":
		o.Platform = PlatformWindowsX64
	case "portable":
		o.Platform, o.Portable = PlatformWindowsX64, true
	case "zip":
		o.Platform = PlatformDarwinArm64
	case "none": // a copy that cannot update itself (the shell says so)
		o.Platform, o.NoInstall = PlatformWindowsX64, true
	default:
		t.Fatalf("unknown copy %q", copy)
	}
}

// The shell's copy of the table is this one, row for row.
func TestOtherCopyKindsTableIsTheShells(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "desktop", "test", "unit", "selfupdate.test.js"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(src), "const OTHER_COPY_TABLE = [")
	body, _, ok2 := strings.Cut(rest, "];")
	if !ok || !ok2 {
		t.Fatal("OTHER_COPY_TABLE not found in the shell's tests")
	}
	rows := regexp.MustCompile(`\['([^']*)', '([^']*)', '([^']*)', (true|false)\]`).FindAllStringSubmatch(body, -1)
	if len(rows) != len(otherCopyTable) || strings.Count(body, "[") != len(rows) {
		t.Fatalf("the shell's table has %d rows (%d read), this one %d", strings.Count(body, "["), len(rows), len(otherCopyTable))
	}
	for i, r := range rows {
		w := otherCopyTable[i]
		if r[1] != w.copy || r[2] != w.platform || r[3] != w.kind || (r[4] == "true") != w.other {
			t.Errorf("row %d: the shell has %v, this table %+v", i, r[1:], w)
		}
	}
}

func TestOtherCopyKindsTable(t *testing.T) {
	k := newTestKey(t)
	for _, r := range otherCopyTable {
		u := newTestUpdater(t, nil, k, func(o *Options) { copyOptions(t, r.copy, o) })
		if got := u.otherCopys(&Pending{Platform: r.platform, Kind: r.kind}); got != r.other {
			t.Errorf("%s copy, pending %s %q: another copy's = %v, want %v", r.copy, r.platform, r.kind, got, r.other)
		}
	}
}

// A copy that cannot update itself (an unpacked or copied folder; the
// desktop shell says so, Options.NoInstall): every pending update in the
// data directory is another copy's — not shown as this copy's, not
// removed whatever its version, its package kept — and this copy never
// makes one: a newer version is still found and shown, without a package
// (the page then says to install it by hand and offers no download), and
// a download or an offline update is refused with selfupdate.no_package.
func TestCopyThatCannotUpdateItself(t *testing.T) {
	for _, version := range []string{"3.1.0", "3.2.0", "3.2.5"} { // older than the pending update, the same, newer
		f := newFakeGH(t)
		k := newTestKey(t)
		f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
		kv := newMemKV()
		data := privateDataDir(t)
		dir := filepath.Join(data, "updates")
		owner := newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV = data, kv })
		owner.Check(bg)
		waitDownloaded(t, owner)
		owner.Close()
		pkg := "3.2.0/LectureFlow-Setup-3.2.0-x64.exe"
		before := updatesState(t, dir, pkg)
		os.Remove(filepath.Join(dir, latestFile)) // (nothing but the pending update refers to 3.2.0/)
		os.Remove(filepath.Join(dir, latestSigFile))

		u := newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV, o.Version, o.NoInstall = data, kv, version, true })
		same := func(when string) {
			t.Helper()
			if got := updatesState(t, dir, pkg); got != before {
				t.Fatalf("%s, %s: the installed copy's pending.json, previous.json or package changed", version, when)
			}
		}
		same("after the start")
		if st := u.Status(bg); st.Pending != nil || st.State == StatePending || st.State == StateNotConfigured {
			t.Fatalf("%s: %+v", version, st)
		}
		f.publishLatest(makeRelease(t, f, k, "3.3.0", ChannelStable, nil))
		st, err := u.Check(bg)
		if err != nil || st.State != StateAvailable || st.Available == nil || st.Available.Version != "3.3.0" || st.Available.File != "" || st.Available.NotesURL == "" {
			t.Fatalf("%s: check: %+v %+v %v", version, st, st.Available, err)
		}
		_, err = u.Download(bg)
		wantCode(t, err, CodeNoPackage)
		if n := f.count(downloadPath("3.3.0", "LectureFlow-Setup-3.3.0-x64.exe")); n != 0 {
			t.Fatalf("%s: %d requests for a package", version, n)
		}
		rel := makeRelease(t, nil, k, "3.3.0", ChannelStable, nil)
		tmp := t.TempDir()
		name := "LectureFlow-Setup-3.3.0-x64.exe"
		_, err = u.ImportPaths(bg, []string{userFile(t, tmp, "update.json", rel.raw), userFile(t, tmp, "update.json.minisig", rel.sig),
			userFile(t, tmp, name, rel.pkgs[name])})
		wantCode(t, err, CodeNoPackage)
		noStaging(t, u)
		if _, err := os.Stat(filepath.Join(dir, "3.3.0")); !os.IsNotExist(err) {
			t.Fatalf("%s: a directory for an update was made: %v", version, err)
		}
		// refused before anything is staged: the user's files are not even
		// copied (SEC40b G10: without the early answer the same code came
		// only after they were staged and verified)
		staged := false
		_, err = u.ImportWith(bg, func(string) error { staged = true; return nil })
		wantCode(t, err, CodeNoPackage)
		if staged {
			t.Fatalf("%s: the offline update was staged before it was refused", version)
		}
		if st, err := u.DiscardPending(bg); err != nil || st.Pending != nil {
			t.Fatalf("%s: discard: %+v %v", version, st, err)
		}
		same("after the refused download, offline update and discard")
		u.Close()

		// the installed copy still has its update
		owner = newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV = data, kv })
		if st := owner.Status(bg); st.Pending == nil || st.Pending.Version != "3.2.0" {
			t.Fatalf("%s: the installed copy lost its update: %+v", version, st)
		}
		owner.Close()
	}
}

// The other copy may be gone for good (a portable copy that was tried and
// deleted): its update would hold this copy back for ever. One verified
// more than OtherPendingMaxDays ago no longer does (lead's ruling, SEC40):
// it is still not this copy's and still not removed at the start, but a
// download or an offline update then goes ahead and takes the slot, as it
// always did. No verified_at or an unreadable one: not given up; one more
// than a day ahead of this computer's clock: given up as well.
func TestOtherCopyPendingGivenUp(t *testing.T) {
	if OtherPendingMaxDays != 14 {
		t.Fatalf("OtherPendingMaxDays = %d; the texts of %s say 14", OtherPendingMaxDays, CodeOtherCopyPending)
	}
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	kv := newMemKV()
	data := privateDataDir(t)
	dir := filepath.Join(data, "updates")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	copyAt := func(portable bool, at time.Time) *Updater {
		return newTestUpdater(t, f, k, func(o *Options) {
			o.DataDir, o.KV, o.Portable = data, kv, portable
			o.Now = func() time.Time { return at }
		})
	}
	owner := copyAt(false, t0)
	owner.Check(bg)
	waitDownloaded(t, owner)
	owner.Close()
	pkg := "3.2.0/LectureFlow-Setup-3.2.0-x64.exe"
	before := updatesState(t, dir, pkg)

	// 14 days less a second: still waited for
	u := copyAt(true, t0.Add(14*24*time.Hour-time.Second))
	u.Check(bg)
	_, err := u.Download(bg)
	wantCode(t, err, CodeOtherCopyPending)
	u.Close()

	// 14 days and a second: not this copy's, not removed by the start ...
	u = copyAt(true, t0.Add(14*24*time.Hour+time.Second))
	if st := u.Status(bg); st.Pending != nil {
		t.Fatalf("taken for this copy's: %+v", st.Pending)
	}
	if got := updatesState(t, dir, pkg); got != before {
		t.Fatal("the start removed or changed the other copy's update")
	}
	// ... but it no longer holds this copy back
	u.Check(bg)
	st := waitDownloaded(t, u)
	if st.Pending == nil || st.Pending.File != "LectureFlow-3.2.0-x64-portable.exe" {
		t.Fatalf("pending %+v", st.Pending)
	}
	var p Pending
	if ok, err := readJSON(filepath.Join(dir, pendingFile), &p); !ok || err != nil || p.Kind != KindPortable {
		t.Fatalf("pending.json %+v %v", p, err)
	}
	if err := u.otherCopyPending(); err != nil {
		t.Fatalf("this copy's own pending update holds it back: %v", err)
	}
}

func TestOtherCopyPendingGivenUpOnlyByItsDate(t *testing.T) {
	k := newTestKey(t)
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		verifiedAt string
		waits      bool
	}{
		"no verified_at": {"", true},
		"not a date":     {"yesterday", true},
		// a clock that is a little off is no reason; one verified "more
		// than a day from now" was made with a clock set far ahead and
		// would otherwise never be given up (SEC40b (c))
		"an hour ahead":            {"2026-11-20T10:00:00Z", true},
		"exactly a day ahead":      {"2026-11-21T09:00:00Z", true},
		"a day and a second ahead": {"2026-11-21T09:00:01Z", false},
		"far in the future":        {"2027-01-01T00:00:00Z", false},
		"a minute ago":             {"2026-11-20T08:59:00Z", true},
		"exactly 14 days":          {"2026-11-06T09:00:00Z", true},
		"14 days and a second":     {"2026-11-06T08:59:59Z", false},
		"a year ago":               {"2025-11-20T09:00:00Z", false},
		"another zone, 15 days":    {"2026-11-05T12:00:00+03:00", false},
	} {
		data := privateDataDir(t)
		dir := filepath.Join(data, "updates")
		os.MkdirAll(filepath.Join(dir, "3.2.0"), 0o755)
		os.WriteFile(filepath.Join(dir, "3.2.0", "x.exe"), []byte("x"), 0o644)
		pending := `{"schema":1,"version":"3.2.0","platform":"windows-x64","kind":"portable","file":"3.2.0/x.exe","size":1,"verified_at":"` + tc.verifiedAt + `"}`
		os.WriteFile(filepath.Join(dir, pendingFile), []byte(pending), 0o644)
		u := newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.Now = data, func() time.Time { return now } })
		err := u.otherCopyPending()
		if tc.waits != (CodeOf(err) == CodeOtherCopyPending) {
			t.Errorf("%s: waits = %v, want %v (%v)", name, err != nil, tc.waits, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, pendingFile)); string(b) != pending {
			t.Errorf("%s: pending.json changed or removed", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "3.2.0", "x.exe")); err != nil {
			t.Errorf("%s: the package went: %v", name, err)
		}
		if st := u.Status(bg); st.Pending != nil {
			t.Errorf("%s: taken for this copy's", name)
		}
	}
}

// What is not another copy's stays as it was: a pending update of the other
// kind whose package is gone is removed (nobody can install it); on macOS
// the zip and the dmg are both this installation's; a kind that is none of
// LectureFlow's is taken as before (the shell drops and reports it).
func TestOtherCopyPendingLimits(t *testing.T) {
	k := newTestKey(t)
	start := func(platform, pending string, withPackage bool) (*Updater, string) {
		data := privateDataDir(t)
		dir := filepath.Join(data, "updates")
		os.MkdirAll(filepath.Join(dir, "3.2.0"), 0o755)
		if withPackage {
			os.WriteFile(filepath.Join(dir, "3.2.0", "x.pkg"), []byte("x"), 0o644)
		}
		os.WriteFile(filepath.Join(dir, pendingFile), []byte(pending), 0o644)
		return newTestUpdater(t, nil, k, func(o *Options) { o.DataDir, o.Platform = data, platform }), dir
	}
	u, dir := start(PlatformWindowsX64, `{"schema":1,"version":"3.2.0","platform":"windows-x64","kind":"portable","file":"3.2.0/x.pkg","size":1}`, false)
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
		t.Errorf("a pending update of the other kind without its package was kept: %v", err)
	}
	if err := u.otherCopyPending(); err != nil {
		t.Errorf("it still holds this copy back: %v", err)
	}

	u, _ = start(PlatformDarwinArm64, `{"schema":1,"version":"3.2.0","platform":"darwin-arm64","kind":"dmg","file":"3.2.0/x.pkg","size":1}`, true)
	if st := u.Status(bg); st.Pending == nil || st.State != StatePending {
		t.Errorf("macOS, a dmg: %+v", st)
	}
	if err := u.otherCopyPending(); err != nil {
		t.Errorf("macOS, a dmg holds this copy back: %v", err)
	}

	u, _ = start(PlatformWindowsX64, `{"schema":1,"version":"3.2.0","platform":"windows-x64","kind":"msi","file":"3.2.0/x.pkg","size":1}`, true)
	if st := u.Status(bg); st.Pending == nil {
		t.Errorf("a kind that is none of LectureFlow's: %+v", st)
	}
	if err := u.otherCopyPending(); err != nil {
		t.Errorf("a kind that is none of LectureFlow's holds this copy back: %v", err)
	}

	// a pending update of this copy's own kind that is not newer goes as before
	_, dir = start(PlatformWindowsX64, `{"schema":1,"version":"3.1.0","platform":"windows-x64","kind":"installer","file":"3.2.0/x.pkg","size":1}`, true)
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
		t.Errorf("this copy's own pending update that is not newer was kept: %v", err)
	}
}
