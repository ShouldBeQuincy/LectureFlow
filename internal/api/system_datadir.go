// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/model"
	"lectureflow/internal/selfupdate"
)

// Who may use the data directory (security review sec-update M2). The data
// directory holds the database with the providers' API keys, the Python
// environments the service runs and the in-app update's files; a data
// directory moved to a place like D:\ used to inherit "Authenticated Users:
// modify" from there. LectureFlow now makes the data directories it creates
// or moves private (internal/dirperm); one that already exists is never
// changed behind the user's back:
//
//   - the data_dir row of the diagnostics turns into a warning when other
//     accounts of this computer can change (check.data_dir_shared) or read
//     (check.data_dir_readable) the data directory, with the hint
//     check.hint_data_dir_restrict (live, like disk; cached a few seconds);
//   - POST /api/system/data-dir/restrict makes it private when the user
//     asks: the directory itself gets the private permissions, what lies
//     below inherits them. 200 {"check": the data_dir row} (and
//     "rollback_reset": true, see below); 409
//     data_dir.restrict_failed when other accounts can still reach it
//     afterwards (another account owns the folder, or the drive keeps no
//     permissions: FAT, exFAT); 500 data_dir.restrict_failed when the
//     permissions cannot be set. An error body carries "rollback_reset":
//     true next to "error" too when the state below was removed before the
//     request failed for another reason.
//
// While other accounts can change the data directory, the in-app update
// neither downloads nor installs anything (selfupdate.untrusted_dir).
//
// What lies below (security audit M-3). A folder or file another account
// made in the data directory while it could write there stays that
// account's, and entries that name another account outright survive the
// change of the data directory's own permissions. So the parts of the data
// directory that are run or trusted are looked at entry by entry
// (dirperm.CheckTree, dataDirScope):
//
//	updates/      the in-app update's packages, JSON files and the backups
//	              of the installation a rollback runs
//	components/   the Python environments and tools the service runs (and
//	              what they are built from: downloads, uv's cache)
//	nmhost/       launch.json: the program the browser extension's host
//	              starts (in the data directory while it is the default one)
//	electron/     the desktop shell's profile, when it lies in the data
//	              directory: its record of the backups' hashes
//	              (selfupdate-trust.json) and the browser's own files
//	              (settings, caches of compiled scripts: what the window
//	              that holds the API token loads; security review SEC17)
//	every other entry of the data directory itself (the database, the
//	              folders of the courses, models, logs): the entry, not what
//	              a folder holds — lecture data is large and never run
//
// Only owners and permissions are read, never a file's content. The walk
// takes about 0.07 ms per entry on an SSD: 2 s for the 30 000 files of two
// Python environments, 0.1 to 0.4 s more for a profile of 2 000 to 5 000
// files (1.4 s with a browser cache of 20 000). So it does not run when
// GET /api/system is polled: the data_dir check runs it with the other
// diagnostics (at start, in the background; "重新检查"; after a component
// was installed) and keeps the result; "收紧权限" runs it again.
//
// The in-app update asks the same question about updates/ with bounds of
// its own (selfupdate.CheckTrust: a few thousand entries are expected
// there, 50 000 are the limit; this walk's limit is for the whole data
// directory with its Python environments). So that the row never says
// "private" about a data directory the update then refuses, the walk ends
// with CheckTrust itself and shows what it says.
//
// An entry other accounts may change turns the row into
// check.data_dir_shared with that entry's path and the hint to restrict.
// A tree the walk cannot vouch for (a link on Windows, more entries than
// the bounds, an entry that cannot be examined) is another matter:
// "收紧权限" cannot repair it, so the row is check.data_dir_unconfirmed
// {path, reason} with the hint check.hint_data_dir_unconfirmed (what to
// remove or move), and no button.
//
// "收紧权限" also resets those entries (dirperm.RestrictTree: the owner
// becomes this account, the entry's own permissions are dropped for its
// folder's) and looks again: 409 data_dir.restrict_failed, naming the
// entry, for what could not be reset — never 200 while the walk still
// finds something. A reset makes an entry this account's; it cannot tell
// whether another account changed what the entry contains (the log names
// the entries that were reset).
//
// The update's rollback state (security review SEC17, 中-3). What a failed
// update returns to is decided by files the desktop shell keeps in its
// profile and in updates/; while the profile lies in the data directory
// and others could write there, they can all be prepared together, and
// tightening the permissions afterwards makes them look like the user's
// own. So when "收紧权限" finds that the data directory was open to change
// — it could be changed before the click (or that could not be told), an
// entry had to be reset or could not be, or the last walk had not vouched
// for it — it removes that state (selfupdate.VoidRollback: the shell's
// record, applying.json, updates/backup, previous.json's "backup"; a link
// in place of updates/ or electron/ is removed, never followed), also when
// the request then ends in 409 for another reason, and says so with
// "rollback_reset": true, in the 200 answer or next to the error (the page
// shows it either way). A state that could not all be removed is a 500
// without it. A data directory that
// was private all along keeps it, and so does one whose shell profile lies
// elsewhere. Without the click nothing needs removing: while others can
// change the data directory, selfupdate.CheckTrust refuses every rollback
// (and every installation) from it.

// Codes of the data_dir row and of the endpoint (the API's catalog; init
// adds them).
const (
	codeDataDirShared         = "check.data_dir_shared"
	codeDataDirReadable       = "check.data_dir_readable"
	codeHintDataDirRestrict   = "check.hint_data_dir_restrict"
	codeDataDirUnconfirmed    = "check.data_dir_unconfirmed"
	codeHintDataDirUnconfirm  = "check.hint_data_dir_unconfirmed"
	codeDataDirRestrictFailed = "data_dir.restrict_failed"
	checkDataDir              = "data_dir"
	dataDirAccessCacheFor     = 5 * time.Second
)

var dataDirCheckCodes = map[string]errDef{
	codeDataDirShared:         {"{path} 可写，但本机的其他账户也能修改其中的文件（{who}）", []string{"path", "who"}},
	codeDataDirReadable:       {"{path} 可写，但本机的其他账户也能读取其中的文件（{who}）", []string{"path", "who"}},
	codeHintDataDirRestrict:   {"点击“收紧权限”，让数据目录只有你的账户（以及系统与管理员）可以访问", nil},
	codeDataDirUnconfirmed:    {"{path} 可写，但无法确认其中的文件只有你的账户能修改：{reason}", []string{"path", "reason"}},
	codeHintDataDirUnconfirm:  {"原因里提到链接（junction、符号链接）时，删除该链接，或在设置页把数据目录改到实际所在的文件夹；提到无法检查的项目时，删除它或请管理员处理；提到条目数或层数超出上限时，把不属于 LectureFlow 的文件移出数据目录里的 updates、components、electron 文件夹；然后点击“重新检查”", nil},
	codeDataDirRestrictFailed: {"没能收紧数据目录 {path} 的权限：{error}", []string{"path", "error"}},
}

func init() {
	for code, def := range dataDirCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
	v3Routes = append(v3Routes, func(s *Server, h func(string, http.HandlerFunc)) {
		h("POST /api/system/data-dir/restrict", s.postRestrictDataDir)
	})
}

// A data directory that is a junction (security review SEC17, 低-2). The
// permissions of the link are not those of the folder it leads to, and a
// walk never enters a link: such a data directory used to be "cannot be
// confirmed" for good, and the folder that really holds the data was never
// looked at or made private. Now (dataDirPlace) the folder a junction leads
// to is what the row checks, the walk starts from and "收紧权限" makes
// private — but only when nobody else can have chosen that folder:
//
//   - the junction leads to a folder on a drive letter, not a drive's root,
//     and the path it holds is the path the system really opens (no second
//     link on the way, no loop, the folder exists and is not a link
//     itself: dirperm.Junction);
//   - no other account owns the link or may change it (whoever may, may
//     point it elsewhere), and none may remove or rename it or a folder
//     above it, nor a folder above the folder it leads to
//     (dirperm.CheckAbove, as for the installation directory: the
//     strictest rule there is; reading the link only shows where it
//     leads).
//
// Otherwise the row is check.data_dir_unconfirmed with the reason (the
// hint says to remove the link or name the real folder in the settings),
// and "收紧权限" changes nothing: 409 data_dir.restrict_failed. The link
// itself is never changed. A symbolic link stays what it was: the walk
// starts from where it leads (dataDirTreeRoot). macOS and Linux have no
// junctions.

// dataDirPlace is where the data directory's own permissions are looked at
// and set (see above).
type dataDirPlace struct {
	// root is the data directory, or the folder the junction leads to
	root string
	// junction: the data directory is a junction that leads to root
	junction bool
	// doubt: the data directory is a junction that cannot be vouched for
	// (why, short English naming it); root is then the data directory and
	// nothing is to be checked or set through it
	doubt string
}

// dataDirAboveTop is where the folders above a junction and above the
// folder it leads to stop being looked at ("" = the root of the drive;
// tests stop at a folder of their own).
var dataDirAboveTop = ""

// resolveDataDir finds the dataDirPlace of the data directory dir (tests
// replace it).
// (dirperm.ResolveJunction: the in-app update's selfupdate.CheckTrust asks
// it too, so that the row and the update agree.)
var resolveDataDir = func(dir string) dataDirPlace {
	root, isJunction, doubt := dirperm.ResolveJunction(dir, dataDirAboveTop)
	if doubt != "" {
		return dataDirPlace{root: dir, doubt: doubt}
	}
	return dataDirPlace{root: root, junction: isJunction}
}

// dataDirAccess caches dirperm.Check of the data directory (GET /api/system
// is polled).
var dataDirAccess struct {
	mu    sync.Mutex
	dir   string
	at    time.Time
	e     dirperm.Exposure
	doubt string
	err   error
}

// checkDataDirAccess is dirperm.Check (tests replace it).
var checkDataDirAccess = dirperm.Check

// dataDirExposure is what other accounts may do with the data directory —
// with the folder a junction leads to, when it is one that can be vouched
// for (doubt: why it cannot; nothing else is said then).
func (s *Server) dataDirExposure(fresh bool) (dirperm.Exposure, string, error) {
	dataDirAccess.mu.Lock()
	defer dataDirAccess.mu.Unlock()
	if !fresh && dataDirAccess.dir == s.dataDir && time.Since(dataDirAccess.at) < dataDirAccessCacheFor {
		return dataDirAccess.e, dataDirAccess.doubt, dataDirAccess.err
	}
	var e dirperm.Exposure
	var err error
	place := resolveDataDir(s.dataDir)
	if place.doubt == "" {
		e, err = checkDataDirAccess(place.root)
	}
	dataDirAccess.dir, dataDirAccess.at, dataDirAccess.e, dataDirAccess.doubt, dataDirAccess.err = s.dataDir, time.Now(), e, place.doubt, err
	return e, place.doubt, err
}

// withDataDirAccess turns an ok data_dir row into a warning while other
// accounts can reach the data directory (see the top of this file).
func (s *Server) withDataDirAccess(rows []SystemCheck) []SystemCheck {
	for i, c := range rows {
		if c.Name == checkDataDir && c.Status == statusOK {
			rows[i] = s.dataDirAccessRow(c, false)
		}
	}
	// cred-enc: credentials, data_dir_location (system_credentials.go); then
	// install_dir (system_installdir.go)
	return s.withInstallDirRow(s.withCredentialRows(rows))
}

func (s *Server) dataDirAccessRow(c SystemCheck, fresh bool) SystemCheck {
	e, doubt, err := s.dataDirExposure(fresh)
	if err != nil {
		e = dirperm.Exposure{}
	}
	abs, aerr := filepath.Abs(s.dataDir)
	if aerr != nil {
		abs = s.dataDir
	}
	if doubt != "" {
		// a junction that cannot be vouched for: nothing "收紧权限" could
		// repair (see dataDirPlace)
		c.Status = statusWarn
		c.setDetail(codeDataDirUnconfirmed, map[string]any{"path": abs, "reason": doubt})
		c.setHint(codeHintDataDirUnconfirm, nil)
		return c
	}
	params := map[string]any{"path": abs, "who": strings.Join(e.Who, ", ")}
	unconfirmed := ""
	if !e.Write {
		// what the data directory holds (the last walk)
		if rep, ok := s.dataDirTreeReport(); ok && !rep.OK() {
			if p, who := rep.Problem(); p != "" {
				e = dirperm.Exposure{Write: true}
				params = map[string]any{"path": p, "who": who}
			} else {
				unconfirmed = rep.Unconfirmed
			}
		}
	}
	if !e.Exposed() {
		if unconfirmed != "" {
			// nothing "收紧权限" could repair: say what to do instead
			c.Status = statusWarn
			c.setDetail(codeDataDirUnconfirmed, map[string]any{"path": abs, "reason": unconfirmed})
			c.setHint(codeHintDataDirUnconfirm, nil)
		}
		return c
	}
	c.Status = statusWarn
	if e.Write {
		c.setDetail(codeDataDirShared, params)
	} else {
		c.setDetail(codeDataDirReadable, params)
	}
	c.setHint(codeHintDataDirRestrict, nil)
	return c
}

// restrictDataDir is dirperm.Restrict (tests replace it).
var restrictDataDir = dirperm.Restrict

// dataDirScope says which entries of the data directory the walk looks at
// (rel: "/"-separated, below the data directory; see the top of this
// file). Names are compared without regard to case: a folder renamed to
// "Updates" on Windows or macOS is still the one that is used.
func dataDirScope(rel string) dirperm.Scope {
	top, _, below := strings.Cut(rel, "/")
	switch {
	case rel == "":
		return dirperm.ScopeAll
	case strings.EqualFold(top, "updates"), strings.EqualFold(top, "components"), strings.EqualFold(top, "nmhost"),
		strings.EqualFold(top, selfupdate.ShellProfileDir):
		return dirperm.ScopeAll
	case !below:
		return dirperm.ScopeSelf
	}
	return dirperm.ScopeSkip
}

// dataDirTreeOptions bound the walk: ten times the entries of a data
// directory with every component installed; beyond them the row says that
// the folder could not be confirmed.
var dataDirTreeOptions = dirperm.TreeOptions{MaxEntries: 300000, MaxDepth: 64, Scope: dataDirScope}

// dataDirTreeRoot is where the walk starts: the data directory, or what it
// leads to when it is a symbolic link (the user's own choice of place). A
// junction is not resolved here (Go does not): see dataDirPlace.
func dataDirTreeRoot(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// checkDataDirTree and restrictDataDirTree are dirperm.CheckTree and
// dirperm.RestrictTree of the data directory (tests replace them).
// checkDataDirTree is given the data directory; restrictDataDirTree is
// given its dataDirPlace's root (the folder a junction leads to).
var (
	checkDataDirTree = func(dir string) (dirperm.TreeReport, error) {
		place := resolveDataDir(dir)
		if place.doubt != "" {
			return dirperm.TreeReport{Unconfirmed: place.doubt}, nil
		}
		root := dataDirTreeRoot(place.root)
		rep, err := dirperm.CheckTree(root, dataDirTreeOptions)
		if err != nil || !rep.OK() {
			return rep, err
		}
		// what the in-app update will say about it (its own bounds)
		terr := checkUpdateTrust(filepath.Join(root, "updates"))
		if selfupdate.CodeOf(terr) != selfupdate.CodeUntrustedDir {
			return rep, nil
		}
		p, _ := selfupdate.InfoOf(terr).Params["path"].(string)
		who, _ := selfupdate.InfoOf(terr).Params["who"].(string)
		if why, ok := strings.CutPrefix(who, dirperm.NotConfirmed); ok {
			rep.Unconfirmed = why
		} else {
			rep.Findings = append(rep.Findings, dirperm.TreeFinding{Path: p, Who: []string{who}})
		}
		return rep, nil
	}
	restrictDataDirTree = func(dir string) ([]string, error) {
		return dirperm.RestrictTree(dataDirTreeRoot(dir), dataDirTreeOptions)
	}
)

// checkUpdateTrust is selfupdate.CheckTrust (tests replace it).
var checkUpdateTrust = selfupdate.CheckTrust

// voidRollbackState is selfupdate.VoidRollback (tests replace it).
var voidRollbackState = selfupdate.VoidRollback

// dataDirTree keeps the last walk of the data directory.
var dataDirTree struct {
	mu  sync.Mutex
	dir string
	ok  bool // rep is a finished walk of dir
	rep dirperm.TreeReport
}

// scanDataDirTree walks the data directory now and keeps the result.
func (s *Server) scanDataDirTree() (dirperm.TreeReport, error) {
	rep, err := checkDataDirTree(s.dataDir)
	dataDirTree.mu.Lock()
	dataDirTree.dir, dataDirTree.ok, dataDirTree.rep = s.dataDir, err == nil, rep
	dataDirTree.mu.Unlock()
	if err != nil {
		slog.Warn("api: looking at what the data directory holds", "dir", s.dataDir, "err", err)
	} else if !rep.OK() {
		p, who := rep.Problem()
		slog.Warn("api: other accounts may change entries of the data directory", "entry", p, "who", who, "entries", len(rep.Findings)+rep.More)
	}
	return rep, err
}

// dataDirTreeReport is the last walk of the data directory (false: none
// yet).
func (s *Server) dataDirTreeReport() (dirperm.TreeReport, bool) {
	dataDirTree.mu.Lock()
	defer dataDirTree.mu.Unlock()
	if !dataDirTree.ok || dataDirTree.dir != s.dataDir {
		return dirperm.TreeReport{}, false
	}
	return dataDirTree.rep, true
}

// checkDataDirEntries is the part of the data_dir check that looks below
// the data directory (checkDataDir calls it with the other diagnostics).
func (s *Server) checkDataDirEntries() {
	s.scanDataDirTree()
	s.installDirCheck(true) // the install_dir row looks again too (system_installdir.go)
}

// postRestrictDataDir: POST /api/system/data-dir/restrict.
func (s *Server) postRestrictDataDir(w http.ResponseWriter, r *http.Request) {
	abs, err := filepath.Abs(s.dataDir)
	if err != nil {
		abs = s.dataDir
	}
	// rollbackReset: the update's rollback state was removed (see the top
	// of this file); the answer says so also when it is an error
	rollbackReset := false
	fail := func(status int, err error) {
		info := model.ErrorInfo{Code: codeDataDirRestrictFailed,
			Params: map[string]any{"path": abs, "error": err.Error()}, Detail: err.Error()}
		if rollbackReset {
			writeJSON(w, status, map[string]any{"error": info, "rollback_reset": true})
			return
		}
		writeErrorInfo(w, status, info)
	}
	// a junction: the folder it leads to, or nothing at all when nobody can
	// tell who chose that folder (see dataDirPlace)
	place := resolveDataDir(s.dataDir)
	if place.doubt != "" {
		slog.Warn("api: the data directory is a junction that cannot be vouched for; nothing is changed", "dir", abs, "why", place.doubt)
		fail(http.StatusConflict, errors.New(dirperm.NotConfirmed+place.doubt))
		return
	}
	// what it was like before anything is changed: decides whether the
	// update's rollback state still counts (see the top of this file)
	before, berr := checkDataDirAccess(place.root)
	lastWalk, walked := s.dataDirTreeReport()
	if err := restrictDataDir(place.root); err != nil {
		slog.Warn("api: restricting the data directory", "dir", abs, "err", err)
		fail(http.StatusInternalServerError, err)
		return
	}
	if place.junction {
		slog.Info("api: the data directory is a junction; the folder it leads to was made private", "dir", abs, "folder", place.root)
	}
	// what other accounts left below it (audit M-3)
	reset, terr := restrictDataDirTree(place.root)
	if len(reset) > 0 {
		slog.Warn("api: entries of the data directory that other accounts could change are this account's again; what they contain was not checked", "entries", reset)
	}
	if berr != nil || before.Write || len(reset) > 0 || terr != nil || (walked && !lastWalk.OK()) {
		v, verr := voidRollbackState(dataDirTreeRoot(place.root))
		if v.Any() {
			slog.Warn("api: the update's rollback state was kept where other accounts could change it and is removed; the next update backs the installation up again",
				"dir", abs, "record", v.Record, "applying", v.Applying, "backups", v.Backups, "previous", v.Previous, "links", v.Links)
		}
		if verr != nil {
			// part of it may be left: not "reset"
			slog.Warn("api: removing the update's rollback state", "dir", abs, "err", verr)
			fail(http.StatusInternalServerError, verr)
			return
		}
		rollbackReset = v.Any()
	}
	e, doubt, err := s.dataDirExposure(true)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	if doubt != "" {
		// the junction changed meanwhile
		fail(http.StatusConflict, errors.New(dirperm.NotConfirmed+doubt))
		return
	}
	if e.Exposed() {
		err := &restrictIncomplete{who: e.Who}
		// "who" joined by ", " as everywhere else: the diagnostics' redaction
		// splits a who value there (redact_account.go)
		slog.Warn("api: the data directory is still reachable by other accounts", "dir", abs, "who", strings.Join(e.Who, ", "))
		fail(http.StatusConflict, err)
		return
	}
	rep, serr := s.scanDataDirTree()
	switch {
	case terr != nil:
		slog.Warn("api: resetting what the data directory holds", "dir", abs, "err", terr)
		fail(http.StatusConflict, terr)
		return
	case serr != nil:
		fail(http.StatusInternalServerError, serr)
		return
	case !rep.OK():
		p, who := rep.Problem()
		fail(http.StatusConflict, &restrictIncomplete{who: []string{who}, entry: p})
		return
	}
	slog.Info("api: the data directory admits this account only now", "dir", abs)
	c := SystemCheck{Name: checkDataDir, Status: statusOK}
	c.setDetail("check.dir_writable", map[string]any{"path": abs})
	out := map[string]any{"check": c}
	if rollbackReset {
		out["rollback_reset"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// restrictIncomplete: other accounts (who) can still reach the data
// directory, or still change the entry of it named by entry.
type restrictIncomplete struct {
	who   []string
	entry string
}

func (e *restrictIncomplete) Error() string {
	if e.entry != "" {
		return "other accounts can still change " + e.entry + " (" + strings.Join(e.who, ", ") + ")"
	}
	return "other accounts can still reach it (" + strings.Join(e.who, ", ") +
		"): another account owns the folder, or the drive keeps no permissions (FAT, exFAT)"
}
