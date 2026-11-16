// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func TestNotConfigured(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	cases := map[string]struct {
		mod    func(*Options)
		reason string
	}{
		"no key":   {func(o *Options) { o.PublicKey = "" }, ReasonNoPublicKey}, // builtinPublicKey is "" below
		"bad key":  {func(o *Options) { o.PublicKey = "RWQnope" }, ReasonNoPublicKey},
		"web mode": {func(o *Options) { o.Desktop = false }, ReasonWebMode},
		"dev":      {func(o *Options) { o.Version = "dev" }, ReasonDevBuild},
		"linux":    {func(o *Options) { o.Platform = "linux-x64" }, ReasonPlatform},
	}
	saved := builtinPublicKey
	builtinPublicKey = ""
	defer func() { builtinPublicKey = saved }()
	for name, c := range cases {
		u := newTestUpdater(t, f, k, c.mod)
		st := u.Status(bg)
		if st.State != StateNotConfigured || st.Reason != c.reason {
			t.Fatalf("%s: status %+v", name, st)
		}
		_, err := u.Check(bg)
		wantCode(t, err, CodeNotConfigured)
		_, err = u.Download(bg)
		wantCode(t, err, CodeNotConfigured)
		_, err = u.ImportPaths(bg, []string{"x"})
		wantCode(t, err, CodeNotConfigured)
		u.Run(bg) // returns at once
	}
	if len(f.requests()) != 0 {
		t.Fatalf("a not configured updater made requests: %+v", f.requests())
	}
}

// A build without a public key (the real Repo; builtinPublicKey emptied:
// how the application was built before the maintainer's key, and still a
// build made with -X lectureflow/internal/selfupdate.builtinPublicKey=):
// not configured, and not one request leaves it, to GitHub or anywhere.
// The application as built now is TestBuiltinKeyOfflineBacksOff.
func TestNotConfiguredMakesNoRequest(t *testing.T) {
	if Repo != "ShouldBeQuincy/LectureFlow" {
		t.Fatalf("Repo %q", Repo)
	}
	saved := builtinPublicKey
	builtinPublicKey = ""
	defer func() { builtinPublicKey = saved }()
	var mu sync.Mutex
	var asked []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, r.URL.String())
		mu.Unlock()
		return nil, errors.New("no network in this test")
	})}
	u := New(Options{DataDir: t.TempDir(), Version: "3.1.0", KV: newMemKV(), Desktop: true, Platform: PlatformWindowsX64,
		Client: client, Downloader: &HTTPDownloader{Client: client}, Log: quietLog()})
	t.Cleanup(u.Close)
	u.Startup(bg)
	if got := u.githubLatest(); got != "https://github.com/ShouldBeQuincy/LectureFlow/releases/latest/download/update.json" {
		t.Fatalf("latest %q", got)
	}
	if st := u.Status(bg); st.State != StateNotConfigured || st.Reason != ReasonNoPublicKey {
		t.Fatalf("status %+v", st)
	}
	_, err := u.Check(bg)
	wantCode(t, err, CodeNotConfigured)
	_, err = u.Download(bg)
	wantCode(t, err, CodeNotConfigured)
	u.Run(bg) // returns at once
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 0 {
		t.Fatalf("requests without a public key: %v", asked)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckDownloadPending(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	// latest/download redirects in real GitHub; a redirect must not add a Referer
	f.mu.Lock()
	f.redirect[latestPath+".minisig"] = downloadPath("3.2.0", signatureFile)
	f.mu.Unlock()
	u := newTestUpdater(t, f, k, nil)
	changes := 0
	var mu sync.Mutex
	u.OnChange(func() { mu.Lock(); changes++; mu.Unlock() })

	if st := u.Status(bg); st.State != StateIdle || st.AutoCheck != true || st.Channel != ChannelStable {
		t.Fatalf("initial status %+v", st)
	}
	st, err := u.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateAvailable || st.Available == nil || st.Available.Version != "3.2.0" || !st.Available.Direct ||
		st.Available.File != "LectureFlow-Setup-3.2.0-x64.exe" || st.Available.Source != "github" || st.LastCheck == "" {
		t.Fatalf("after check %+v %+v", st, st.Available)
	}
	if _, err := os.Stat(filepath.Join(u.Dir(), latestFile)); err != nil {
		t.Fatalf("latest.json not saved: %v", err)
	}
	st, err = u.Download(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDownloading && st.State != StatePending {
		t.Fatalf("download started: %+v", st)
	}
	st = waitFor(t, u, notState(StateDownloading))
	if st.State != StatePending || st.Pending == nil || st.Pending.Version != "3.2.0" || st.Pending.Origin != OriginDownload ||
		st.Error != nil || st.Available != nil {
		t.Fatalf("after download %+v", st)
	}
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	got, err := os.ReadFile(filepath.Join(u.Dir(), "3.2.0", name))
	if err != nil || sha(got) != sha(rel.pkgs[name]) {
		t.Fatalf("package on disk: %v", err)
	}
	for _, n := range []string{manifestFile, signatureFile} {
		if _, err := os.Stat(filepath.Join(u.Dir(), "3.2.0", n)); err != nil {
			t.Fatalf("%s not kept: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(u.Dir(), "3.2.0", name+".part")); !os.IsNotExist(err) {
		t.Fatalf("part file left: %v", err)
	}
	var p Pending
	if ok, err := readJSON(filepath.Join(u.Dir(), pendingFile), &p); !ok || err != nil {
		t.Fatalf("pending.json: %v %v", ok, err)
	}
	a, _ := rel.m.Asset(PlatformWindowsX64, KindInstaller)
	want := Pending{Schema: 1, Version: "3.2.0", FromVersion: "3.1.0", Channel: ChannelStable, Platform: PlatformWindowsX64,
		Kind: KindInstaller, File: "3.2.0/" + name, Size: a.Size, SHA256: a.SHA256, Manifest: "3.2.0/update.json",
		Signature: "3.2.0/update.json.minisig", Origin: OriginDownload, VerifiedAt: p.VerifiedAt, KeyID: p.KeyID}
	if p != want || p.VerifiedAt == "" || len(p.KeyID) != 16 {
		t.Fatalf("pending.json %+v", p)
	}
	var prev Previous
	if ok, err := readJSON(filepath.Join(u.Dir(), previousFile), &prev); !ok || err != nil {
		t.Fatalf("previous.json: %v %v", ok, err)
	}
	if prev.Version != "3.1.0" || prev.File != "" || prev.Kind != KindInstaller || prev.Platform != PlatformWindowsX64 {
		t.Fatalf("previous.json %+v", prev)
	}
	if st.Rollback == nil || st.Rollback.Version != "3.1.0" || st.Rollback.Package {
		t.Fatalf("rollback %+v", st.Rollback)
	}
	mu.Lock()
	if changes < 4 {
		t.Errorf("only %d change notifications", changes)
	}
	mu.Unlock()

	// the requests: only the User-Agent identifies anything
	for _, r := range f.requests() {
		if ua := r.header.Get("User-Agent"); ua != UserAgent {
			t.Errorf("%s: User-Agent %q", r.path, ua)
		}
		for h := range r.header {
			switch h {
			case "User-Agent", "Accept-Encoding", "Range", "Accept":
			default:
				t.Errorf("%s: header %s: %q", r.path, h, r.header.Get(h))
			}
		}
	}
	if f.count(downloadPath("3.2.0", signatureFile)) != 1 {
		t.Fatal("the redirect was not followed")
	}
	// a second download is refused: the version is pending
	_, err = u.Download(bg)
	wantCode(t, err, CodeNotAvailable)
}

func TestCheckUpToDateAndDowngrade(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	for _, v := range []string{"3.1.0", "3.0.9", "3.1.0+build.7", "3.1.0-beta.9"} {
		ch := ChannelStable
		if strings.Contains(v, "-") {
			ch = ChannelBeta
		}
		f.publishLatest(makeRelease(t, f, k, v, ch, nil))
		u := newTestUpdater(t, f, k, func(o *Options) {
			o.KV.SetKV(bg, "app.update_channel", ChannelBeta)
		})
		st, err := u.Check(bg)
		if err != nil || st.State != StateUpToDate || st.Available != nil {
			t.Fatalf("%s: %+v %v", v, st, err)
		}
		_, err = u.Download(bg)
		wantCode(t, err, CodeNotAvailable)
	}
}

func TestChannels(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	beta := makeRelease(t, f, k, "3.2.0-beta.1", ChannelBeta, nil)
	stable := makeRelease(t, f, k, "3.1.5", ChannelStable, nil)
	f.publishLatest(stable)
	// the releases API lists the beta (newest) and the stable one
	list := []map[string]any{}
	for _, r := range []*release{stable, beta, makeRelease(t, f, k, "3.3.0", ChannelStable, nil)} {
		draft := r.version == "3.3.0" // drafts are not offered
		list = append(list, map[string]any{"tag_name": "v" + r.version, "draft": draft, "assets": []map[string]any{
			{"name": manifestFile, "browser_download_url": f.srv.URL + downloadPath(r.version, manifestFile)},
			{"name": signatureFile, "browser_download_url": f.srv.URL + downloadPath(r.version, signatureFile)},
		}})
	}
	b, _ := json.Marshal(list)
	f.set(apiPath, b)

	kv := newMemKV()
	u := newTestUpdater(t, f, k, func(o *Options) { o.KV = kv })
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available.Version != "3.1.5" || f.count(apiPath) != 0 {
		t.Fatalf("stable: %+v (API asked %d times)", st, f.count(apiPath))
	}
	kv.SetKV(bg, "app.update_channel", ChannelBeta)
	st, _ = u.Check(bg)
	if st.State != StateAvailable || st.Available.Version != "3.2.0-beta.1" || st.Available.Channel != ChannelBeta ||
		st.Available.Source != "github-api" || st.Channel != ChannelBeta {
		t.Fatalf("beta: %+v %+v", st, st.Available)
	}
	for _, r := range f.requests() {
		if r.path == apiPath && (r.header.Get("Accept") != "application/vnd.github+json" || r.header.Get("User-Agent") != UserAgent) {
			t.Errorf("API request headers %v", r.header)
		}
	}
	// back to stable: the beta found is not offered
	kv.SetKV(bg, "app.update_channel", ChannelStable)
	if st := u.Status(bg); st.Available != nil || st.State != StateUpToDate {
		t.Fatalf("stable after beta: %+v", st)
	}
	_, err := u.Download(bg)
	wantCode(t, err, CodeNotAvailable)
	// a beta update.json served as "latest" (a mistake) is not offered to stable users
	f.publishLatest(beta)
	st, _ = u.Check(bg)
	if st.State != StateUpToDate {
		t.Fatalf("beta as latest on stable: %+v", st)
	}
	// the API unreachable: beta users fall back to latest
	kv.SetKV(bg, "app.update_channel", ChannelBeta)
	f.publishLatest(stable)
	f.setStatus(apiPath, http.StatusForbidden) // rate limit
	st, _ = u.Check(bg)
	if st.State != StateAvailable || st.Available.Version != "3.1.5" {
		t.Fatalf("API down: %+v", st)
	}
}

func TestSourcesFallBack(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.setStatus(latestPath, http.StatusInternalServerError)
	// a directory mirror with the release copied into it
	f.set("/mirror/lf/update.json", rel.raw)
	f.set("/mirror/lf/update.json.minisig", rel.sig)
	kv := newMemKV()
	u := newTestUpdater(t, f, k, func(o *Options) { o.KV = kv })
	if _, err := u.UpdateSettings(bg, SettingsPatch{Sources: &[]string{f.srv.URL + "/mirror/lf"}}); err != nil {
		t.Fatal(err)
	}
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available.Source != f.srv.URL+"/mirror/lf" {
		t.Fatalf("dir mirror: %+v", st)
	}
	// the package is downloaded from the mirror first (it has none: 404) then GitHub
	waitDownloaded(t, u)
	if f.count("/mirror/lf/LectureFlow-Setup-3.2.0-x64.exe") != 1 || f.count(downloadPath("3.2.0", "LectureFlow-Setup-3.2.0-x64.exe")) != 1 {
		t.Fatalf("download order: %+v", f.requests())
	}

	// a tampered default source and a proxy mirror: the proxy wins
	f2 := newFakeGH(t)
	rel2 := makeRelease(t, f2, k, "3.2.0", ChannelStable, nil)
	f2.publishLatest(rel2)
	tampered := append([]byte{}, rel2.raw...)
	tampered[10] ^= 1
	f2.mu.Lock()
	f2.files[latestPath] = tampered
	f2.files["/proxy:"+latestPath] = rel2.raw // the proxy serves the genuine file
	f2.mu.Unlock()
	u2 := newTestUpdater(t, f2, k, nil)
	u2.UpdateSettings(bg, SettingsPatch{Sources: &[]string{f2.srv.URL + "/proxy/{url}"}})
	st, _ = u2.Check(bg)
	if st.State != StateAvailable || st.Available.Source != f2.srv.URL+"/proxy/{url}" {
		t.Fatalf("proxy mirror: %+v", st)
	}
	waitDownloaded(t, u2)
	if f2.count("/proxy:"+downloadPath("3.2.0", "LectureFlow-Setup-3.2.0-x64.exe")) != 1 {
		t.Fatalf("the package did not come through the proxy: %+v", f2.requests())
	}

	// every source fails: the most telling error
	u3 := newTestUpdater(t, f2, k, nil)
	st, err := u3.Check(bg)
	if err != nil || st.State != StateError || st.Error.Code != CodeSignatureInvalid {
		t.Fatalf("tampered only: %+v %v", st, err)
	}
	f2.setStatus(latestPath, http.StatusNotFound)
	f2.setStatus(latestPath+".minisig", http.StatusNotFound)
	st, _ = u3.Check(bg)
	if st.State != StateError || st.Error.Code != CodeNoManifest {
		t.Fatalf("no update.json: %+v", st)
	}
	f2.setStatus(latestPath, http.StatusServiceUnavailable)
	st, _ = u3.Check(bg)
	if st.State != StateError || st.Error.Code != CodeUnreachable {
		t.Fatalf("unreachable: %+v", st)
	}
}

// A release without update.json or without its signature (HTTP 404: the
// maintainer has not uploaded the signature yet) is selfupdate.no_manifest,
// not "cannot reach the update source"; it ranks above a source that could
// not be reached and below a bad signature, and the detail lists what
// every source answered.
func TestNoManifest(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.set(latestPath, rel.raw) // update.json is there, its signature is not
	u := newTestUpdater(t, f, k, nil)
	st, err := u.Check(bg)
	if err != nil || st.State != StateError || st.Error.Code != CodeNoManifest || st.Available != nil {
		t.Fatalf("no signature: %+v %v", st, err)
	}
	if d := st.Error.Detail; !strings.Contains(d, "github: ") || !strings.Contains(d, "update.json.minisig: HTTP status 404") {
		t.Fatalf("detail %q", d)
	}
	// no update.json either (410 Gone counts as well)
	f.setStatus(latestPath, http.StatusGone)
	if st, _ = u.Check(bg); st.Error == nil || st.Error.Code != CodeNoManifest {
		t.Fatalf("no update.json: %+v", st)
	}

	// a mirror that cannot be reached and GitHub without update.json: both
	// sources in the detail, the more telling no_manifest as the code
	mirror := "http://127.0.0.1:1/lf/"
	u.UpdateSettings(bg, SettingsPatch{Sources: &[]string{mirror}})
	st, _ = u.Check(bg)
	if st.State != StateError || st.Error.Code != CodeNoManifest {
		t.Fatalf("mirror down: %+v", st)
	}
	lines := strings.Split(st.Error.Detail, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "github: "+CodeNoManifest+": ") ||
		!strings.HasPrefix(lines[1], mirror+": "+CodeUnreachable+": "+mirror+"update.json: ") {
		t.Fatalf("detail %q", st.Error.Detail)
	}
	// a mirror with a tampered update.json: the bad signature comes first
	tampered := append([]byte{}, rel.raw...)
	tampered[10] ^= 1
	f.set("/bad/update.json", tampered)
	f.set("/bad/update.json.minisig", rel.sig)
	u.UpdateSettings(bg, SettingsPatch{Sources: &[]string{f.srv.URL + "/bad/"}})
	if st, _ = u.Check(bg); st.Error == nil || st.Error.Code != CodeSignatureInvalid || strings.Count(st.Error.Detail, "\n") != 1 {
		t.Fatalf("tampered mirror: %+v", st.Error)
	}

	// the beta channel: the releases API lists no release with both files
	b, _ := json.Marshal([]map[string]any{{"tag_name": "v3.2.0", "assets": []map[string]any{
		{"name": manifestFile, "browser_download_url": f.srv.URL + downloadPath("3.2.0", manifestFile)}}}})
	f.set(apiPath, b)
	kv := newMemKV()
	kv.SetKV(bg, "app.update_channel", ChannelBeta)
	u2 := newTestUpdater(t, f, k, func(o *Options) { o.KV = kv })
	st, _ = u2.Check(bg)
	if st.State != StateError || st.Error.Code != CodeNoManifest || !strings.Contains(st.Error.Detail, "github-api: ") {
		t.Fatalf("beta: %+v", st.Error)
	}
	// once the signature is uploaded, the next check finds the release
	f.setStatus(latestPath, 0)
	f.publishLatest(rel)
	if st, _ = u2.Check(bg); st.State != StateAvailable || st.Error != nil {
		t.Fatalf("after the upload: %+v", st)
	}
}

// A failed check does not hide an update that can be installed (the
// version found before, the one downloaded): the state stays available or
// pending, the failure is a note (Status.Error). A failed download of the
// available version is the state error.
func TestFailedCheckKeepsUpdate(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	data, kv := privateDataDir(t), newMemKV()
	same := func(o *Options) { o.DataDir, o.KV = data, kv }
	u := newTestUpdater(t, f, k, same)
	if st, _ := u.Check(bg); st.State != StateAvailable {
		t.Fatalf("check: %+v", st)
	}
	f.setStatus(latestPath, http.StatusBadGateway)
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available == nil || st.Available.Version != "3.2.0" || st.Error == nil || st.Error.Code != CodeUnreachable {
		t.Fatalf("failed check after one that found 3.2.0: %+v", st)
	}
	f.setStatus(latestPath+".minisig", http.StatusNotFound)
	f.setStatus(latestPath, 0)
	if st, _ = u.Check(bg); st.State != StateAvailable || st.Error == nil || st.Error.Code != CodeNoManifest {
		t.Fatalf("no signature after one that found 3.2.0: %+v", st)
	}
	// the download works although the last check failed
	st = waitDownloaded(t, u)
	if st.Error != nil {
		t.Fatalf("after the download: %+v", st.Error)
	}
	// pending, and a check fails: still pending, with the note
	if st, _ = u.Check(bg); st.State != StatePending || st.Pending == nil || st.Error == nil || st.Error.Code != CodeNoManifest {
		t.Fatalf("failed check while pending: %+v", st)
	}
	// a restart remembers both
	u.Close()
	u = newTestUpdater(t, f, k, same)
	if st := u.Status(bg); st.State != StatePending || st.Error == nil || st.Error.Code != CodeNoManifest || st.Error.Detail == "" {
		t.Fatalf("after a restart: %+v", st)
	}

	// a failed download of the available version is the state error
	f2 := newFakeGH(t)
	rel := makeRelease(t, f2, k, "3.2.0", ChannelStable, nil)
	f2.publishLatest(rel)
	f2.setStatus(downloadPath("3.2.0", "LectureFlow-Setup-3.2.0-x64.exe"), http.StatusNotFound)
	u2 := newTestUpdater(t, f2, k, nil)
	u2.Check(bg)
	u2.Download(bg)
	if st := waitFor(t, u2, notState(StateDownloading)); st.State != StateError || st.Error.Code != CodeDownloadFailed || st.Available == nil {
		t.Fatalf("failed download: %+v", st)
	}
}

// After a failed download (or check), an offline update that works is the
// state pending: the earlier error is gone (it used to stay, so the
// settings said "检查更新失败" and the workbench hid the pending update).
func TestImportAfterFailedDownload(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	f.setStatus(downloadPath("3.2.0", name), http.StatusInternalServerError)
	u := newTestUpdater(t, f, k, nil)
	u.Check(bg)
	u.Download(bg)
	if st := waitFor(t, u, notState(StateDownloading)); st.State != StateError || st.Error == nil || st.Error.Code != CodeDownloadFailed {
		t.Fatalf("failed download: %+v", st)
	}
	// the same version from files
	src := t.TempDir()
	st, err := u.ImportPaths(bg, []string{userFile(t, src, "update.json", rel.raw), userFile(t, src, "update.json.minisig", rel.sig),
		userFile(t, src, name, rel.pkgs[name])})
	if err != nil || st.State != StatePending || st.Error != nil || st.Pending == nil || st.Pending.Origin != OriginFile {
		t.Fatalf("import after a failed download: %+v %v", st, err)
	}
	// a refused import leaves the status as it was
	f2 := newFakeGH(t)
	u2 := newTestUpdater(t, f2, k, nil)
	f2.setStatus(latestPath, http.StatusNotFound)
	u2.Check(bg)
	bad := append([]byte{}, rel.raw...)
	bad[5] ^= 1
	_, err = u2.ImportPaths(bg, []string{userFile(t, src, "b.json", bad), userFile(t, src, "b.json.minisig", rel.sig),
		userFile(t, src, "b-"+name, rel.pkgs[name])})
	wantCode(t, err, CodeSignatureInvalid)
	if st := u2.Status(bg); st.State != StateError || st.Error == nil || st.Error.Code != CodeNoManifest {
		t.Fatalf("after a refused import: %+v", st)
	}
}

// Discarding the pending update deletes its files (pending.json and
// updates/<version>/), not those of the running version nor Electron's
// backups; downloading it again starts from the beginning.
func TestDiscardDeletesFiles(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	u := newTestUpdater(t, f, k, nil)
	dir := u.Dir()
	for _, p := range []string{filepath.Join("3.1.0", "LectureFlow-Setup-3.1.0-x64.exe"), filepath.Join("backup", "3.1.0", "x")} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("keep"), 0o644)
	}
	u.Check(bg)
	waitDownloaded(t, u)
	if _, err := os.Stat(filepath.Join(dir, "3.2.0", name)); err != nil {
		t.Fatal(err)
	}
	st, err := u.DiscardPending(bg)
	if err != nil || st.Pending != nil || st.State != StateAvailable {
		t.Fatalf("discard: %+v %v", st, err)
	}
	for _, p := range []string{pendingFile, "3.2.0"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join("3.1.0", "LectureFlow-Setup-3.1.0-x64.exe"), filepath.Join("backup", "3.1.0", "x"), previousFile} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	n := f.count(downloadPath("3.2.0", name))
	waitDownloaded(t, u)
	if f.count(downloadPath("3.2.0", name)) != n+1 {
		t.Fatal("the package was not downloaded again")
	}
	// an imported update as well
	u.DiscardPending(bg)
	src := t.TempDir()
	u.ImportPaths(bg, []string{userFile(t, src, "update.json", rel.raw), userFile(t, src, "update.json.minisig", rel.sig),
		userFile(t, src, name, rel.pkgs[name])})
	if _, err := u.DiscardPending(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "3.2.0")); !os.IsNotExist(err) {
		t.Fatalf("the imported package is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, name)); err != nil {
		t.Fatalf("the user's own file was touched: %v", err)
	}
}

func TestCheckInterval(t *testing.T) {
	h := time.Hour
	for _, c := range []struct {
		failures int
		code     string
		want     time.Duration
	}{
		{0, "", 24 * h}, {1, CodeUnreachable, h}, {2, CodeUnreachable, 3 * h}, {3, CodeSignatureInvalid, 6 * h},
		{4, CodeUnreachable, 24 * h}, {40, CodeUnreachable, 24 * h},
		{1, CodeNoManifest, 2 * h}, {12, CodeNoManifest, 2 * h}, {13, CodeNoManifest, 6 * h},
		// after unreachable twice, the release lacks update.json: 2 h
		{3, CodeNoManifest, 2 * h},
	} {
		if got := CheckInterval(c.failures, c.code); got != c.want {
			t.Errorf("CheckInterval(%d, %s) = %v, want %v", c.failures, c.code, got, c.want)
		}
	}
}

// A failed check is followed by another an hour later; one that works
// brings back the daily rhythm.
func TestAutoCheckBackoffResets(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.1.0", ChannelStable, nil))
	f.setStatus(latestPath, http.StatusServiceUnavailable)
	var mu sync.Mutex
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	u := newTestUpdater(t, f, k, func(o *Options) { o.Now, o.FirstCheckDelay, o.CheckEvery = clock, time.Millisecond, 2*time.Millisecond })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitChecks := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for f.count(latestPath) < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d checks, want %d", f.count(latestPath), n)
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond)
		if c := f.count(latestPath); c != n {
			t.Fatalf("%d checks, want %d", c, n)
		}
	}
	waitChecks(1)
	f.setStatus(latestPath, 0)
	advance(time.Hour)
	waitChecks(2)
	if st := u.Status(bg); st.State != StateUpToDate || st.Error != nil {
		t.Fatalf("after the check that worked: %+v", st)
	}
	advance(23 * time.Hour)
	waitChecks(2)
	advance(time.Hour)
	waitChecks(3)
}

// The time of the last check "in the future" (the clock was set back, or
// it was wrong when the check ran) counts as now: automatic checks go on a
// day later instead of waiting for that date.
func TestFutureLastCheck(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.1.0", ChannelStable, nil))
	var mu sync.Mutex
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	kv := newMemKV()
	kv.SetKV(bg, KVLastCheck, `{"at":"2027-03-01T00:00:00Z","result":"up_to_date"}`)
	u := newTestUpdater(t, f, k, func(o *Options) {
		o.Now, o.KV, o.FirstCheckDelay, o.CheckEvery = clock, kv, time.Millisecond, 2*time.Millisecond
	})
	st := u.Status(bg)
	if st.LastCheck != "2026-11-01T09:00:00Z" || st.NextCheck != "2026-11-02T09:00:00Z" || st.State != StateUpToDate {
		t.Fatalf("status %+v", st)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(30 * time.Millisecond)
	if n := f.count(latestPath); n != 0 {
		t.Fatalf("%d checks right away", n)
	}
	advance(24 * time.Hour)
	deadline := time.Now().Add(5 * time.Second)
	for f.count(latestPath) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no check a day later")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitDownloaded(t *testing.T, u *Updater) Status {
	t.Helper()
	if _, err := u.Download(bg); err != nil {
		t.Fatal(err)
	}
	st := waitFor(t, u, notState(StateDownloading))
	if st.State != StatePending {
		t.Fatalf("download: %+v %+v", st, st.Error)
	}
	return st
}

func TestDownloadTamperedPackage(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	good := rel.pkgs[name]
	for _, i := range []int{0, len(good) / 2, len(good) - 1} {
		bad := append([]byte{}, good...)
		bad[i] ^= 0x80
		f.set(downloadPath("3.2.0", name), bad)
		u := newTestUpdater(t, f, k, nil)
		u.Check(bg)
		if _, err := u.Download(bg); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, u, notState(StateDownloading))
		if st.State != StateError || st.Error == nil || st.Error.Code != CodeChecksum || st.Error.Params["file"] != name ||
			st.Pending != nil || st.Available == nil {
			t.Fatalf("byte %d: %+v", i, st)
		}
		for _, p := range []string{name, name + ".part"} {
			if _, err := os.Stat(filepath.Join(u.Dir(), "3.2.0", p)); !os.IsNotExist(err) {
				t.Fatalf("byte %d: %s left behind", i, p)
			}
		}
		if _, err := os.Stat(filepath.Join(u.Dir(), pendingFile)); !os.IsNotExist(err) {
			t.Fatal("pending.json written")
		}
	}
	// a package shorter or longer than update.json says
	for _, b := range [][]byte{good[:len(good)-1], append(append([]byte{}, good...), 0)} {
		f.set(downloadPath("3.2.0", name), b)
		u := newTestUpdater(t, f, k, nil)
		u.Check(bg)
		u.Download(bg)
		if st := waitFor(t, u, notState(StateDownloading)); st.State != StateError || st.Pending != nil {
			t.Fatalf("size %d: %+v", len(b), st)
		}
	}
	// the mirror rule of update.json takes over when the URL fails
	rel2 := makeRelease(t, f, k, "3.3.0", ChannelStable, func(m *Manifest) {
		m.Mirrors = []MirrorRule{{Prefix: f.srv.URL + "/o/r/", Replace: f.srv.URL + "/m/"}}
	})
	f.publishLatest(rel2)
	n2 := "LectureFlow-Setup-3.3.0-x64.exe"
	f.setStatus(downloadPath("3.3.0", n2), http.StatusNotFound)
	f.set("/m/releases/download/v3.3.0/"+n2, rel2.pkgs[n2])
	u := newTestUpdater(t, f, k, nil)
	u.Check(bg)
	waitDownloaded(t, u)
}

func TestDownloadResumesAndCancels(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	f.publishLatest(rel)
	name := "LectureFlow-Setup-3.2.0-x64.exe"
	u := newTestUpdater(t, f, k, nil)
	u.Check(bg)
	// a partial download from an earlier run
	os.MkdirAll(filepath.Join(u.Dir(), "3.2.0"), 0o755)
	os.WriteFile(filepath.Join(u.Dir(), "3.2.0", name+".part"), rel.pkgs[name][:1000], 0o644)
	waitDownloaded(t, u)
	var ranged bool
	for _, r := range f.requests() {
		ranged = ranged || (r.path == downloadPath("3.2.0", name) && r.header.Get("Range") == "bytes=1000-")
	}
	if !ranged {
		t.Fatal("the download did not resume")
	}

	// cancel: the transfer hangs after a third of the file
	rel2 := makeRelease(t, f, k, "3.3.0", ChannelStable, nil)
	f.publishLatest(rel2)
	n2 := "LectureFlow-Setup-3.3.0-x64.exe"
	hold := make(chan struct{})
	defer close(hold)
	f.mu.Lock()
	f.hold[downloadPath("3.3.0", n2)] = hold
	f.mu.Unlock()
	u2 := newTestUpdater(t, f, k, func(o *Options) { o.Downloader = &HTTPDownloader{Backoff: time.Millisecond, Stall: time.Minute} })
	u2.Check(bg)
	u2.Download(bg)
	waitFor(t, u2, func(st Status) bool { return st.Progress != nil && st.Progress.BytesDone > 0 })
	_, err := u2.Download(bg)
	wantCode(t, err, CodeBusy)
	_, err = u2.Check(bg)
	wantCode(t, err, CodeBusy)
	if !u2.CancelDownload() {
		t.Fatal("no download to cancel")
	}
	st := waitFor(t, u2, notState(StateDownloading))
	if st.State != StateAvailable || st.Error != nil || st.Progress != nil {
		t.Fatalf("after cancel: %+v", st)
	}
	if fi, err := os.Stat(filepath.Join(u2.Dir(), "3.3.0", n2+".part")); err != nil || fi.Size() == 0 {
		t.Fatalf("the partial file should stay: %v", err)
	}
	if u2.CancelDownload() {
		t.Fatal("cancelled twice")
	}
}

func TestMinVersion(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "4.0.0", ChannelStable, func(m *Manifest) { m.MinVersion = "3.5.0" }))
	u := newTestUpdater(t, f, k, nil)
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available.Direct || st.Available.MinVersion != "3.5.0" {
		t.Fatalf("%+v %+v", st, st.Available)
	}
	_, err := u.Download(bg)
	wantCode(t, err, CodeTooOld)
	if p := InfoOf(err).Params; p["min_version"] != "3.5.0" || p["current"] != "3.1.0" {
		t.Fatalf("params %v", p)
	}
}

func TestNoPackageForThisInstallation(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, func(m *Manifest) {
		delete(m.Platforms, PlatformWindowsX64)
	}))
	u := newTestUpdater(t, f, k, func(o *Options) { o.Portable = true })
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available.File != "" || st.Kind != KindPortable {
		t.Fatalf("%+v", st)
	}
	_, err := u.Download(bg)
	wantCode(t, err, CodeNoPackage)
}

// After an update the new version cleans up, and the next update records
// the package of the running version for a rollback.
func TestStartupAndPrevious(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	kv := newMemKV()
	data := privateDataDir(t)
	opts := func(v string) func(*Options) {
		return func(o *Options) { o.DataDir, o.KV, o.Version = data, kv, v }
	}
	u := newTestUpdater(t, f, k, opts("3.1.0"))
	u.Check(bg)
	waitDownloaded(t, u)
	u.Close()

	// restart before installing: the pending update is still there
	u = newTestUpdater(t, f, k, opts("3.1.0"))
	if st := u.Status(bg); st.State != StatePending || st.Pending.Version != "3.2.0" || st.LastCheck == "" {
		t.Fatalf("restart: %+v", st)
	}
	u.Close()

	// Electron installed 3.2.0; leftovers of all kinds
	dir := filepath.Join(data, "updates")
	for _, d := range []string{"3.0.0", "9.9.9", ".import-123", "backup/3.1.0", "not-a-version"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, resultFile), []byte(`{"version":"3.2.0","from":"3.1.0","outcome":"installed","at":"2026-11-03T08:00:00Z"}`), 0o644)
	u = newTestUpdater(t, f, k, opts("3.2.0"))
	st := u.Status(bg)
	if st.Pending != nil || st.LastResult == nil || st.LastResult.Outcome != "installed" || st.Rollback == nil || st.Rollback.Version != "3.1.0" {
		t.Fatalf("after install: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
		t.Fatal("pending.json of the installed version not removed")
	}
	for d, keep := range map[string]bool{"3.0.0": false, "9.9.9": false, ".import-123": false, "backup": true,
		"not-a-version": true, "3.2.0": true} {
		_, err := os.Stat(filepath.Join(dir, d))
		if (err == nil) != keep {
			t.Errorf("%s: kept=%v", d, err == nil)
		}
	}

	// the next update: previous.json points at 3.2.0's verified package
	f.publishLatest(makeRelease(t, f, k, "3.3.0", ChannelStable, nil))
	u.Check(bg)
	waitDownloaded(t, u)
	var prev Previous
	readJSON(filepath.Join(dir, previousFile), &prev)
	if prev.Version != "3.2.0" || prev.File != "3.2.0/LectureFlow-Setup-3.2.0-x64.exe" || prev.SHA256 == "" || prev.Size == 0 {
		t.Fatalf("previous.json %+v", prev)
	}
	if st := u.Status(bg); !st.Rollback.Package {
		t.Fatalf("rollback %+v", st.Rollback)
	}
	// discarding the pending update
	st, err := u.DiscardPending(bg)
	if err != nil || st.Pending != nil || st.State != StateAvailable {
		t.Fatalf("discard: %+v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
		t.Fatal("pending.json not removed")
	}
}

func TestStartupDropsBadPending(t *testing.T) {
	k := newTestKey(t)
	for name, p := range map[string]string{
		"older":      `{"schema":1,"version":"3.0.0","platform":"windows-x64","file":"3.0.0/x.exe","size":1}`,
		"no package": `{"schema":1,"version":"3.2.0","platform":"windows-x64","file":"3.2.0/x.exe","size":1}`,
		"escape":     `{"schema":1,"version":"3.2.0","platform":"windows-x64","file":"../x.exe","size":1}`,
		"platform":   `{"schema":1,"version":"3.2.0","platform":"darwin-arm64","file":"3.2.0/x.exe","size":1}`,
		"schema":     `{"schema":2,"version":"3.2.0","platform":"windows-x64","file":"3.2.0/x.exe","size":1}`,
		"garbage":    `{`,
	} {
		data := privateDataDir(t)
		dir := filepath.Join(data, "updates")
		os.MkdirAll(filepath.Join(dir, "3.0.0"), 0o755)
		os.WriteFile(filepath.Join(dir, "3.0.0", "x.exe"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(data, "x.exe"), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(dir, pendingFile), []byte(p), 0o644)
		u := newTestUpdater(t, nil, k, func(o *Options) { o.DataDir = data })
		if st := u.Status(bg); st.Pending != nil {
			t.Errorf("%s: pending %+v", name, st.Pending)
		}
		if _, err := os.Stat(filepath.Join(dir, pendingFile)); !os.IsNotExist(err) {
			t.Errorf("%s: pending.json kept", name)
		}
	}
}

func TestLatestRestoredAtStartup(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.2.0", ChannelStable, nil))
	data, kv := privateDataDir(t), newMemKV()
	u := newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV = data, kv })
	u.Check(bg)
	u.Close()
	n := len(f.requests())
	u = newTestUpdater(t, f, k, func(o *Options) { o.DataDir, o.KV = data, kv })
	if st := u.Status(bg); st.State != StateAvailable || st.Available.Version != "3.2.0" {
		t.Fatalf("restored: %+v", st)
	}
	waitDownloaded(t, u) // works without the source that answered
	if len(f.requests()) == n {
		t.Fatal("no download request")
	}
	// another key now: the saved result does not verify and is dropped
	u.Close()
	os.Remove(filepath.Join(data, "updates", pendingFile))
	u = newTestUpdater(t, f, newTestKey(t), func(o *Options) { o.DataDir, o.KV = data, kv })
	if st := u.Status(bg); st.Available != nil {
		t.Fatalf("a result signed by another key was restored: %+v", st)
	}
	// up to date is remembered as well
	kv2 := newMemKV()
	f.publishLatest(makeRelease(t, f, k, "3.1.0", ChannelStable, nil))
	u = newTestUpdater(t, f, k, func(o *Options) { o.KV = kv2 })
	u.Check(bg)
	u.Close()
	u = newTestUpdater(t, f, k, func(o *Options) { o.KV = kv2 })
	if st := u.Status(bg); st.State != StateUpToDate {
		t.Fatalf("up to date not remembered: %+v", st)
	}
}

func TestAutoCheckOnceADay(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	f.publishLatest(makeRelease(t, f, k, "3.1.0", ChannelStable, nil))
	var mu sync.Mutex
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	kv := newMemKV()
	u := newTestUpdater(t, f, k, func(o *Options) {
		o.Now, o.KV, o.FirstCheckDelay, o.CheckEvery = clock, kv, time.Millisecond, 2*time.Millisecond
	})
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitChecks := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for f.count(latestPath) < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d checks, want %d", f.count(latestPath), n)
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond) // many ticks: no further check
		if c := f.count(latestPath); c != n {
			t.Fatalf("%d checks, want %d", c, n)
		}
	}
	waitChecks(1)
	advance(23 * time.Hour)
	waitChecks(1)
	advance(time.Hour)
	waitChecks(2)
	// a check by hand counts as well
	advance(23 * time.Hour)
	u.Check(bg)
	advance(2 * time.Hour)
	waitChecks(3)
	// switched off
	off := false
	u.UpdateSettings(bg, SettingsPatch{AutoCheck: &off})
	advance(48 * time.Hour)
	waitChecks(3)
	if st := u.Status(bg); st.AutoCheck {
		t.Fatal("auto_check still on")
	}
}

func TestSettings(t *testing.T) {
	k := newTestKey(t)
	u := newTestUpdater(t, nil, k, nil)
	if s := u.Settings(bg); !s.AutoCheck || s.Sources == nil || len(s.Sources) != 0 {
		t.Fatalf("defaults %+v", s)
	}
	// plain http only to this computer (sec-update)
	srcs := []string{" https://mirror.invalid/lf/ ", "https://proxy.invalid/{url}", "https://mirror.invalid/lf/",
		"http://127.0.0.1:8080/x/update.json"}
	s, err := u.UpdateSettings(bg, SettingsPatch{Sources: &srcs})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://mirror.invalid/lf/", "https://proxy.invalid/{url}", "http://127.0.0.1:8080/x/update.json"}
	if strings.Join(s.Sources, " ") != strings.Join(want, " ") || !s.AutoCheck {
		t.Fatalf("saved %+v", s)
	}
	for i, bad := range [][]string{
		{"https://ok.invalid/", "ftp://x.invalid/"},
		{"https://ok.invalid/", "http://plain.invalid/x/update.json"},
		{"http://mirror.invalid/{url}"},
		{"https://ok.invalid/", "http://[::2]/lf/"},
		{"https://ok.invalid/", "https://ok2.invalid/", "not a url"},
		{"https://u:p@x.invalid/"},
		{"https://x.invalid/?q=1"},
		{"https://x.invalid/{url}/{url}"},
		{"{url}"},
		{"https://x.invalid/" + strings.Repeat("a", maxSourceLen)},
	} {
		_, err := u.UpdateSettings(bg, SettingsPatch{Sources: &bad})
		wantCode(t, err, CodeSourcesInvalid)
		if idx := InfoOf(err).Params["index"]; idx != len(bad)-1 {
			t.Errorf("case %d: index %v", i, idx)
		}
	}
	many := make([]string, maxSources+1)
	for i := range many {
		many[i] = "https://m" + string(rune('a'+i)) + ".invalid/"
	}
	_, err = u.UpdateSettings(bg, SettingsPatch{Sources: &many})
	wantCode(t, err, CodeSourcesInvalid)
	if s := u.Settings(bg); len(s.Sources) != 3 {
		t.Fatalf("a refused patch changed the settings: %+v", s)
	}
	off := false
	s, _ = u.UpdateSettings(bg, SettingsPatch{AutoCheck: &off})
	if s.AutoCheck || len(s.Sources) != 3 {
		t.Fatalf("%+v", s)
	}
	// a stored http source of an earlier version is left out; the others stay
	u.opt.KV.SetKV(bg, KVSources, `["http://plain.invalid/lf/","https://mirror.invalid/lf/","http://localhost:9/lf/"]`)
	if s := u.Settings(bg); strings.Join(s.Sources, " ") != "https://mirror.invalid/lf/ http://localhost:9/lf/" {
		t.Fatalf("stored http source: %+v", s.Sources)
	}
}

func TestSecureURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://github.com/o/r":     true,
		"http://github.com/o/r":      false,
		"http://127.0.0.1:8080/lf/":  true,
		"http://127.9.9.9/lf/":       true,
		"http://[::1]:9/lf/":         true,
		"http://localhost/lf/":       true,
		"http://LOCALHOST/lf/":       true,
		"http://localhost.evil.com/": false,
		"http://10.0.0.1/lf/":        false,
		"http://u:p@127.0.0.1/":      false,
		"ftp://127.0.0.1/":           false,
	} {
		if got := secureURL(s); got != want {
			t.Errorf("secureURL(%q) = %v", s, got)
		}
	}
}

// Every source is asked; the newest release that verifies wins, so a
// source that serves an old (validly signed) update.json cannot hide a
// newer one; a package URL over plain http elsewhere is not used.
func TestSourcesNewestWins(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	old := makeRelease(t, f, k, "3.2.0", ChannelStable, nil)
	newest := makeRelease(t, f, k, "3.3.0", ChannelStable, func(m *Manifest) {
		m.Mirrors = []MirrorRule{{Prefix: f.srv.URL + "/", Replace: "http://mirror.invalid/"}}
	})
	f.publishLatest(old) // GitHub (first in order) is behind
	f.set("/mirror/lf/update.json", newest.raw)
	f.set("/mirror/lf/update.json.minisig", newest.sig)
	u := newTestUpdater(t, f, k, nil)
	u.UpdateSettings(bg, SettingsPatch{Sources: &[]string{f.srv.URL + "/mirror/lf"}})
	st, _ := u.Check(bg)
	if st.State != StateAvailable || st.Available.Version != "3.3.0" || st.Available.Source != f.srv.URL+"/mirror/lf" {
		t.Fatalf("newest: %+v %+v", st, st.Available)
	}
	if f.count(latestPath) != 1 || f.count("/mirror/lf/update.json") != 1 {
		t.Fatalf("every source is asked once: %+v", f.requests())
	}
	a, _ := newest.m.Asset(PlatformWindowsX64, KindInstaller)
	for _, s := range u.assetURLs(bg, u.avail, a) {
		if strings.HasPrefix(s, "http://mirror.invalid/") {
			t.Fatalf("plain http package URL used: %v", s)
		}
	}
	// the same version everywhere: the first source in order
	f.publishLatest(newest)
	st, _ = u.Check(bg)
	if st.Available.Version != "3.3.0" || st.Available.Source != "github" {
		t.Fatalf("tie: %+v", st.Available)
	}
}

// recordingDL is a Downloader that keeps the requests it is given.
type recordingDL struct {
	inner Downloader
	mu    sync.Mutex
	reqs  []DownloadRequest
}

func (d *recordingDL) Download(ctx context.Context, req DownloadRequest) error {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	return d.inner.Download(ctx, req)
}

// SEC19 info-5: the downloader is told which package URLs come from the
// user's own sources (a directory, a {url} template), whose hosts may be
// on this computer or the local network; GitHub's and update.json's own
// URLs (its mirror rules too) are not among them. Everything here is the
// fake GitHub on 127.0.0.1.
func TestDownloadRequestUserURLs(t *testing.T) {
	f := newFakeGH(t)
	k := newTestKey(t)
	rel := makeRelease(t, f, k, "3.3.0", ChannelStable, func(m *Manifest) {
		m.Mirrors = []MirrorRule{{Prefix: f.srv.URL + "/", Replace: f.srv.URL + "/cdn/"}}
	})
	f.publishLatest(rel)
	rec := &recordingDL{inner: &HTTPDownloader{Backoff: time.Millisecond}}
	u := newTestUpdater(t, f, k, func(o *Options) { o.Downloader = rec })
	if _, err := u.UpdateSettings(bg, SettingsPatch{Sources: &[]string{f.srv.URL + "/mirror/lf", f.srv.URL + "/proxy/{url}"}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := u.Check(bg); st.State != StateAvailable || st.Available.Version != "3.3.0" {
		t.Fatalf("check %+v", st)
	}
	a, _ := rel.m.Asset(PlatformWindowsX64, KindInstaller)
	want := []string{f.srv.URL + "/mirror/lf/" + a.Name, f.srv.URL + "/proxy/" + a.URL}
	all := u.assetURLs(bg, u.avail, a)
	user := u.userAssetURLs(bg, u.avail, a)
	if strings.Join(user, " ") != strings.Join(want, " ") {
		t.Fatalf("user URLs %q, want %q (all: %q)", user, want, all)
	}
	for _, x := range user {
		found := false
		for _, y := range all {
			found = found || x == y
		}
		if !found || x == a.URL || strings.Contains(x, "/cdn/") {
			t.Errorf("user URL %q (all: %q)", x, all)
		}
	}
	if _, err := u.Download(bg); err != nil {
		t.Fatal(err)
	}
	if st := waitFor(t, u, notState(StateDownloading)); st.State != StatePending {
		t.Fatalf("after download %+v", st)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 1 || strings.Join(rec.reqs[0].UserURLs, " ") != strings.Join(want, " ") ||
		strings.Join(rec.reqs[0].URLs, " ") != strings.Join(all, " ") {
		t.Fatalf("requests %+v", rec.reqs)
	}
}

func TestParseSourceKinds(t *testing.T) {
	latest := "https://github.com/o/r/releases/latest/download/update.json"
	a := Asset{Name: "LectureFlow Setup.exe", URL: "https://github.com/o/r/releases/download/v1.0.0/LectureFlow-Setup.exe"}
	for raw, want := range map[string][3]string{ // manifest, sig, asset
		"https://p.invalid/{url}":       {"https://p.invalid/" + latest, "https://p.invalid/" + latest + ".minisig", "https://p.invalid/" + a.URL},
		"https://m.invalid/lf":          {"https://m.invalid/lf/update.json", "https://m.invalid/lf/update.json.minisig", "https://m.invalid/lf/LectureFlow%20Setup.exe"},
		"https://m.invalid/x/beta.json": {"https://m.invalid/x/beta.json", "https://m.invalid/x/beta.json.minisig", "https://m.invalid/x/LectureFlow%20Setup.exe"},
	} {
		s, err := parseSource(raw, latest)
		if err != nil {
			t.Fatal(err)
		}
		if got := [3]string{s.manifest, s.sig, s.assetURL(a)}; got != want {
			t.Errorf("%s:\n got %q\nwant %q", raw, got, want)
		}
	}
	// without a repository a proxy template has nothing to wrap
	if s, _ := parseSource("https://p.invalid/{url}", ""); s.manifest != "" {
		t.Fatal(s.manifest)
	}
}
