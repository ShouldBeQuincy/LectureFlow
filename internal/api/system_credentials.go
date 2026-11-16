// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/components"
	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/hwdetect"
	"lectureflow/internal/model"
	"lectureflow/internal/secrets"
)

// The stored credentials and where the data directory lives (cred-enc,
// user decision 2026-10-27; security review 2 "数据库明文凭据", L5). Two live
// rows of the diagnostics, after data_dir (withDataDirAccess adds them):
//
//   - credentials: how the data key of the stored API keys and the Colab
//     authorisation is protected (internal/secrets): Windows DPAPI or the
//     macOS Keychain (ok), a plain 0600 key file (warning: macOS web /
//     development mode), no key at all (error: stored unsealed). A warning
//     too while stored credentials cannot be decrypted with this
//     installation's key (the data directory came from another computer or
//     account): they have to be entered again.
//   - data_dir_location, only when there is something to say: the data
//     directory is in a folder a cloud service syncs (OneDrive, iCloud
//     Drive, Dropbox, Google Drive: the database and the key file go to the
//     cloud and to the other devices), or on a FAT / exFAT drive (no access
//     permissions: every account of the computer can read it).
const (
	checkCredentials     = "credentials"
	checkDataDirLocation = "data_dir_location"

	codeCredentialsDPAPI       = "check.credentials_dpapi"
	codeCredentialsKeychain    = "check.credentials_keychain"
	codeCredentialsFile        = "check.credentials_file"
	codeCredentialsNone        = "check.credentials_none"
	codeCredentialsUnavailable = "check.credentials_unavailable"
	codeCredentialsUnreadable  = "check.credentials_unreadable"
	codeCredentialsReplaced    = "check.credentials_replaced"
	codeHintCredentialsReenter = "check.hint_credentials_reenter"
	codeHintCredentialsNone    = "check.hint_credentials_none"
	codeDataDirCloudSync       = "check.data_dir_cloud_sync"
	codeDataDirNoACL           = "check.data_dir_no_acl"
	codeHintDataDirMove        = "check.hint_data_dir_move"

	// test-upgrade F6 (see credentialsCheck)
	codeCredentialsKeyMissing     = "check.credentials_key_missing"
	codeHintCredentialsRestoreKey = "check.hint_credentials_restore_key"

	// test-upgrade F9 (see credentialsCheck)
	codeCredentialsScrubPending = "check.credentials_scrub_pending"
	codeHintCredentialsScrub    = "check.hint_credentials_scrub_pending"

	// test-upgrade F10, R6-E (see dataDirPathCheck)
	codeDataDirPathTooLongToStart = "check.data_dir_path_too_long_to_start"
	codeDataDirPathTooLong        = "check.data_dir_path_too_long"
	codeDataDirPathNearLimit      = "check.data_dir_path_near_limit"
	codeHintDataDirShorter        = "check.hint_data_dir_shorter"
	// T27 P2, SEC52 L-1/L-2 (see blockedComponents): the detail and its
	// clauses, one per component that cannot be installed (params.extra)
	codeDataDirPathComponents       = "check.data_dir_path_components"
	codeDataDirComponentPython      = "check.data_dir_component_python"
	codeDataDirComponentASRCUDA     = "check.data_dir_component_asr_cuda"
	codeDataDirComponentASROpenVINO = "check.data_dir_component_asr_openvino"
	codeDataDirComponentGcloud      = "check.data_dir_component_gcloud"

	credentialRowsCacheFor = 5 * time.Second
)

// KVCredentialKeyNotice (kv) remembers, across starts, why the stored
// credentials cannot be decrypted when a start had to make a new data key
// (test-upgrade F6): cmd/lectureflow writes it after sealing while some
// cannot be decrypted, and clears it once none is left. JSON
// CredentialKeyNotice; it holds a file path, never a key.
const KVCredentialKeyNotice = "secrets.key_notice"

// Kinds of CredentialKeyNotice.
const (
	KeyNoticeMissing  = "missing"  // the key file was not there: a new key was made
	KeyNoticeReplaced = "replaced" // the key file could not be opened: kept as Kept, a new key made
)

// CredentialKeyNotice is the value of KVCredentialKeyNotice.
type CredentialKeyNotice struct {
	Kind string `json:"kind"`
	Kept string `json:"kept,omitempty"` // KeyNoticeReplaced: the kept file
}

var credentialCheckCodes = map[string]errDef{
	codeCredentialsDPAPI:       {"保存的 API key 与 Colab 授权已加密，密钥由 Windows 数据保护（DPAPI）绑定在当前账户上", nil},
	codeCredentialsKeychain:    {"保存的 API key 与 Colab 授权已加密，密钥保存在 macOS 钥匙串中", nil},
	codeCredentialsFile:        {"保存的 API key 与 Colab 授权已加密，但密钥只是数据目录里的文件 {path}（仅当前账户可读），没有系统凭据库保护", []string{"path"}},
	codeCredentialsNone:        {"没能建立加密密钥，API key 与 Colab 授权以明文保存：{error}", []string{"error"}},
	codeCredentialsUnavailable: {"系统凭据库现在不可用（{error}）：本次没有使用已保存的 API key 与 Colab 授权，密钥文件保持不变，下次启动会再试；现在填写的 key 会以明文保存", []string{"error"}},
	codeCredentialsUnreadable:  {"有 {n} 项保存的凭据无法在这台电脑或这个账户上解密（数据目录可能来自别的电脑或账户），需要重新输入。重新输入提供商的 API key 之前，请先确认这个提供商的地址是你设置的；如果不是，先把地址改回正确的再输入", []string{"n"}},
	codeCredentialsReplaced:    {"这台电脑或这个账户打不开原来的密钥文件，已生成新密钥（原文件保留为 {path}）；有 {n} 项保存的凭据需要重新输入", []string{"path", "n"}},
	codeHintCredentialsReenter: {"在设置页的“提供商”中为标有“需要重新输入”的提供商填写 API key；Colab 需要重新授权", nil},
	codeHintCredentialsNone:    {"查看日志 logs/lectureflow.log 中以 credentials 开头的记录，排除问题后重启 LectureFlow", nil},
	codeDataDirCloudSync:       {"数据目录 {path} 在 {service} 的同步文件夹中：数据库（含加密后的 API key）与密钥文件会上传到云端并同步到其他设备", []string{"path", "service"}},
	codeDataDirNoACL:           {"数据目录 {path} 所在的磁盘是 {fs} 格式，这种格式不记录访问权限，本机的任何账户都能读取其中的文件", []string{"path", "fs"}},
	codeHintDataDirMove:        {"在设置页的“数据目录”中把它移到不会同步的本地 NTFS / APFS 磁盘（例如默认位置）", nil},

	codeCredentialsKeyMissing:     {"数据目录里的密钥文件 secrets.key 不见了（被删除，或复制数据目录时没有一起复制），已生成新密钥；有 {n} 项保存的凭据（API key、Colab 授权）是用原来的密钥加密的，现在无法解密，但密文仍原样保留", []string{"n"}},
	codeHintCredentialsRestoreKey: {"如果还有原来的 secrets.key（例如在数据目录的备份里），先退出 LectureFlow，把它改名为 secrets.key.unreadable-restored 放进数据目录 {dir}（不要覆盖现有的 secrets.key），再启动即可恢复，不必重新输入；这只在当初创建它的电脑与 Windows 账户上有效。没有原文件时，在设置页的“提供商”中为标有“需要重新输入”的提供商重新填写 API key，Colab 需要重新授权", []string{"dir"}},

	codeCredentialsScrubPending: {"保存的 API key 与 Colab 授权已加密，但更换、删除或加密凭据之后的清理还没有完成：数据库文件里可能还留有旧值（包括加密之前的明文）", nil},
	codeHintCredentialsScrub:    {"清理会在下次启动 LectureFlow 时自动完成。重启后仍有这条提醒时，先关闭正在读取 lectureflow.db 的其他程序（数据库查看工具、备份或同步软件），再重启 LectureFlow", nil},

	codeDataDirPathTooLongToStart: {"数据目录的路径 {path} 有 {len} 个字符，超过了 {hard}：处理进程要在它下面约 70 个字符深的文件夹里启动，不论是否开启 Windows 长路径支持，转写、修复断句与课件文本抽取都无法启动；即使缩短到 {hard} 以内，路径超过 {max} 时，在没有开启长路径支持的电脑上这几步也可能写不了结果文件", []string{"path", "len", "max", "hard"}},
	codeDataDirPathTooLong:        {"数据目录的路径 {path} 有 {len} 个字符，超过了 {max}：转写、修复断句与课件文本抽取要读写的文件在它下面还要深约 90 个字符，在没有开启 Windows 长路径支持的电脑上，这几步可能写不了结果文件", []string{"path", "len", "max"}},
	codeDataDirPathNearLimit:      {"数据目录的路径 {path} 有 {len} 个字符，接近上限 {max}：转写、修复断句与课件文本抽取要读写的文件在它下面还要深约 90 个字符，路径再长一些，在没有开启 Windows 长路径支持的电脑上，这几步就可能写不了结果文件", []string{"path", "len", "max"}},
	codeHintDataDirShorter:        {"在设置页的“数据目录”中把它移到路径更短的位置（例如 D:\\LectureFlow）", nil},

	codeDataDirPathComponents:       {"数据目录的路径 {path} 有 {len} 个字符，超过了 {max}，这台电脑要用的一些组件在这里装不了（它们最深的文件会超过 Windows 的路径上限 250 个字符）", []string{"path", "len", "max", "extra"}},
	codeDataDirComponentPython:      {"“Python 运行环境”：本机转写与课件文本抽取都要用到它", nil},
	codeDataDirComponentASRCUDA:     {"“NVIDIA 显卡转写”：这台电脑推荐用它转写", nil},
	codeDataDirComponentASROpenVINO: {"“Intel 转写（OpenVINO）”：这台电脑推荐用它转写", nil},
	codeDataDirComponentGcloud:      {"“Google Cloud CLI（gcloud）”：在 LectureFlow 里登录 Colab 要用到它", nil},
}

func init() {
	for code, def := range credentialCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// Replaced in tests.
var (
	credentialStatus = secrets.CurrentStatus
	dataDirFSType    = fsType // system_credentials_*.go
	userHome         = os.UserHomeDir
	lookupEnv        = os.Getenv
)

// credentialRows caches the rows (GET /api/system is polled; the kv check
// reads the database).
var credentialRows struct {
	mu   sync.Mutex
	dir  string
	at   time.Time
	rows []SystemCheck
}

// withCredentialRows inserts the credentials row (and data_dir_location
// when it warns) after the data_dir row (at the end without one).
func (s *Server) withCredentialRows(rows []SystemCheck) []SystemCheck {
	extra := s.credentialChecks(false)
	if len(extra) == 0 {
		return rows
	}
	at := len(rows)
	for i, c := range rows {
		if c.Name == checkDataDir {
			at = i + 1
			break
		}
	}
	out := make([]SystemCheck, 0, len(rows)+len(extra))
	out = append(out, rows[:at]...)
	out = append(out, extra...)
	return append(out, rows[at:]...)
}

func (s *Server) credentialChecks(fresh bool) []SystemCheck {
	credentialRows.mu.Lock()
	defer credentialRows.mu.Unlock()
	if !fresh && credentialRows.dir == s.dataDir && time.Since(credentialRows.at) < credentialRowsCacheFor {
		return append([]SystemCheck(nil), credentialRows.rows...)
	}
	var rows []SystemCheck
	if c, ok := s.credentialsCheck(); ok {
		rows = append(rows, c)
	}
	c, ok, partial := s.dataDirLocationCheck()
	if ok {
		rows = append(rows, c)
	}
	if partial {
		// not everything was read in time (blockedComponents; SEC53): not
		// kept, so the next request reads again
		credentialRows.dir, credentialRows.at, credentialRows.rows = "", time.Time{}, nil
	} else {
		credentialRows.dir, credentialRows.at, credentialRows.rows = s.dataDir, time.Now(), rows
	}
	return append([]SystemCheck(nil), rows...)
}

// unreadableCredentials counts the stored credentials no key opens: the
// providers from the settings cache, the Colab credential from the
// database (bounded: a long transaction must not stall the page).
func (s *Server) unreadableCredentials() int {
	n := 0
	if s.set != nil {
		for _, p := range s.set.Providers() {
			if s.set.KeyUnreadable(p.ID) {
				n++
			}
		}
	}
	if in, ok := s.st.(db.SecretInspector); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, kv, err := in.UnreadableSecrets(ctx); err == nil {
			n += len(kv)
		}
	}
	return n
}

// credentialsCheck is the credentials row; none when the data key was
// never set up in this process (tests and tools that open the API without
// cmd's startup).
func (s *Server) credentialsCheck() (SystemCheck, bool) {
	c := SystemCheck{Name: checkCredentials, Status: statusOK}
	st := credentialStatus()
	if st.Protection == "" {
		return c, false
	}
	if st.Protection == secrets.ProtectionNone {
		c.Status = statusError
		why := st.Err
		if why == "" {
			why = "-"
		}
		if st.Unavailable { // sec3 L4: nothing replaced, tried again next start
			c.setDetail(codeCredentialsUnavailable, map[string]any{"error": why})
		} else {
			c.setDetail(codeCredentialsNone, map[string]any{"error": why})
		}
		c.setHint(codeHintCredentialsNone, nil)
		return c, true
	}
	if n := s.unreadableCredentials(); n > 0 {
		c.Status = statusWarn
		// test-upgrade F6: say why (the key file missing, or replaced and
		// kept: also at later starts, from KVCredentialKeyNotice), and on
		// Windows how the original key file brings them back.
		note := s.keyNotice(st)
		switch note.Kind {
		case KeyNoticeReplaced:
			c.setDetail(codeCredentialsReplaced, map[string]any{"path": note.Kept, "n": n})
		case KeyNoticeMissing:
			c.setDetail(codeCredentialsKeyMissing, map[string]any{"n": n})
		default:
			c.setDetail(codeCredentialsUnreadable, map[string]any{"n": n})
		}
		if st.Protection == secrets.ProtectionDPAPI && st.KeyFile != "" {
			c.setHint(codeHintCredentialsRestoreKey, map[string]any{"dir": filepath.Dir(st.KeyFile)})
		} else { // macOS: unchanged
			c.setHint(codeHintCredentialsReenter, nil)
		}
		return c, true
	}
	// test-upgrade F9: sealed, but a clean-up of old values (VACUUM, WAL
	// truncation) has not completed (db.KVScrubPending): a warning until
	// the next start finishes it.
	if s.scrubPending() {
		c.Status = statusWarn
		c.setDetail(codeCredentialsScrubPending, nil)
		c.setHint(codeHintCredentialsScrub, nil)
		return c, true
	}
	switch st.Protection {
	case secrets.ProtectionDPAPI:
		c.setDetail(codeCredentialsDPAPI, nil)
	case secrets.ProtectionKeychain:
		c.setDetail(codeCredentialsKeychain, nil)
	default: // ProtectionFile
		c.Status = statusWarn
		c.setDetail(codeCredentialsFile, map[string]any{"path": st.KeyFile})
	}
	return c, true
}

// scrubPending: db.KVScrubPending is set (bounded read, like the others).
func (s *Server) scrubPending() bool {
	if s.st == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, ok, err := s.st.GetKV(ctx, db.KVScrubPending)
	return err == nil && ok && v != ""
}

// keyNotice is why the stored credentials cannot be decrypted, as far as
// known: this start replaced or made the key file (secrets.Status; a new
// key file only counts for a key file, not the macOS Keychain), else what
// an earlier start recorded (KVCredentialKeyNotice); Kind "" otherwise.
func (s *Server) keyNotice(st secrets.Status) CredentialKeyNotice {
	switch {
	case st.Replaced != "":
		return CredentialKeyNotice{Kind: KeyNoticeReplaced, Kept: st.Replaced}
	case st.Created && st.KeyFile != "":
		return CredentialKeyNotice{Kind: KeyNoticeMissing}
	}
	if s.st == nil {
		return CredentialKeyNotice{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, ok, err := s.st.GetKV(ctx, KVCredentialKeyNotice)
	var note CredentialKeyNotice
	if err != nil || !ok || v == "" || json.Unmarshal([]byte(v), &note) != nil {
		return CredentialKeyNotice{}
	}
	switch {
	case note.Kind == KeyNoticeReplaced && note.Kept != "":
		return note
	case note.Kind == KeyNoticeMissing:
		return CredentialKeyNotice{Kind: KeyNoticeMissing}
	}
	return CredentialKeyNotice{}
}

// dataDirLocationCheck is the data_dir_location row (ok false: none);
// partial: the components it names come from an incomplete read
// (blockedComponents), so the row is not to be kept.
func (s *Server) dataDirLocationCheck() (c SystemCheck, ok, partial bool) {
	dir := s.absDataDir()
	if dir == "" {
		return SystemCheck{}, false, false
	}
	c = SystemCheck{Name: checkDataDirLocation, Status: statusWarn}
	// One row, one note: past dataDirPathHard first (no process starts
	// there, long path support or not), then too long for components this
	// computer needs (blockedComponents; T27 P2), then the security notes,
	// then "too long" for the files only (it works with long path support)
	// and "close to the limit" (security review SEC3 L4, R66 F2: they hid
	// them).
	code, n, long := dataDirPathCheck(dir)
	params := map[string]any{"path": dir, "len": n, "max": dataDirPathMax}
	if code == codeDataDirPathTooLongToStart {
		params["hard"] = dataDirPathHard
		c.setDetail(code, params)
		c.setHint(codeHintDataDirShorter, nil)
		return c, true, false
	}
	home, _ := userHome()
	svc := cloudSyncService(dir, home, lookupEnv, credPathExists)
	fs, noACL := dataDirFSType(dir)
	// T27 P2, SEC52 L-1/L-2: too long for components this computer needs
	// (blockedComponents). Like "too long to start", it comes before the
	// security notes: what they do cannot be had here until the data
	// directory moves, and moving it is what the security notes ask for
	// too. The row has one note; when a security note holds as well, the
	// hint is the security notes' (move it to an unsynced local NTFS / APFS
	// disk, for example the default location, which is short enough), so
	// the fix that is offered covers both and the other note is not lost
	// (SEC3 L4, R66 F2); after the move the row shows whatever still holds.
	// The detail names each component in a clause of its own (params.extra,
	// rendered after the sentence like a compound note).
	ids, limit, partial := s.blockedComponents(n)
	if len(ids) > 0 {
		extra := make([]model.ErrorInfo, 0, len(ids))
		zh := make([]string, 0, len(ids)+1)
		for _, id := range ids {
			extra = append(extra, model.ErrorInfo{Code: dataDirComponentClause[id]})
		}
		c.setDetail(codeDataDirPathComponents, map[string]any{"path": dir, "len": n, "max": limit, "extra": extra})
		zh = append(zh, c.Detail)
		for _, x := range extra {
			zh = append(zh, zhText(x))
		}
		c.Detail = strings.Join(zh, "；")
		if svc != "" || noACL {
			c.setHint(codeHintDataDirMove, nil)
		} else {
			c.setHint(codeHintDataDirShorter, nil)
		}
		return c, true, partial
	}
	if svc != "" {
		c.setDetail(codeDataDirCloudSync, map[string]any{"path": dir, "service": svc})
		c.setHint(codeHintDataDirMove, nil)
		return c, true, partial
	}
	if noACL {
		c.setDetail(codeDataDirNoACL, map[string]any{"path": dir, "fs": fs})
		c.setHint(codeHintDataDirMove, nil)
		return c, true, partial
	}
	if long { // too long for the files, or close to the limit
		c.setDetail(code, params)
		c.setHint(codeHintDataDirShorter, nil)
		return c, true, partial
	}
	return SystemCheck{}, false, partial
}

// dataDirComponentClause is the clause of the data_dir_location detail that
// names a component (params.extra of check.data_dir_path_components).
var dataDirComponentClause = map[string]string{
	components.IDPython:           codeDataDirComponentPython,
	hwdetect.ComponentASRCUDA:     codeDataDirComponentASRCUDA,
	hwdetect.ComponentASROpenVINO: codeDataDirComponentASROpenVINO,
	components.IDGcloud:           codeDataDirComponentGcloud,
}

// blockedComponents returns the components this computer needs that cannot
// be installed under a data directory of n characters, and the longest
// data directory under which all of them can: the smallest of their limits
// (components.Manager.DataDirLimit: the deepest file stays within
// components.MaxPathLen). With the shipped manifest the limits differ:
// gcloud 81, asr-cuda 96, python 108, asr-openvino 110 (models: 140 and
// more, not counted). Only on Windows (dataDirPathLimited) with a component
// manager. The components counted, in this order:
//   - python ("Python 运行环境"): every transcription component depends on
//     it, and the slide text extraction runs with it;
//   - the transcription components of this computer's hardware
//     recommendation (hwdetect.Recommend, the onboarding's rule: asr-cuda
//     with an NVIDIA GPU, asr-openvino with an Intel GPU or NPU or on the
//     CPU alone, also next to an NVIDIA GPU with an Intel NPU), from the
//     remembered hardware detection or the device detection's NVIDIA GPU;
//     none while the hardware is not known (no detection yet: no guess, no
//     false alarm) or when only Colab transcribes (asr mode remote);
//   - gcloud (Colab sign-in) only when Colab is set up and in use (Colab
//     switched on, or asr mode remote: the diagnostics' Colab rows).
//
// A component is not counted when it is installed (it was checked when it
// was installed, and the data directory cannot be moved to a longer path
// under it: relocate.CheckMove) or when something of the user's own
// stands in for it: an existing interpreter configured for its backend
// (config.toml python / python_openvino, or the development .venv), or a
// gcloud on PATH (gcloudLookup). A recommended component still counts when
// the user chose another one: on an NVIDIA computer that transcribes with
// asr-openvino alone, asr-cuda is named from 97 characters on (SEC52b
// info-10). Its clause says the computer's recommendation uses it, which
// holds; telling a deliberate choice from a component not installed yet
// would need a rule of its own (the device settings, the battery mode's
// pair), and a wrong one would hide the note from the computers it is for.
//
// Its database reads (the asr mode, Colab's switch, the first read of the
// remembered hardware) run under credentialRows.mu and share one bounded
// context, like the credentials row's (SEC52b info-9): a long transaction
// must not stall the page. When the deadline ran out (partial), what was
// not read is not known: asrMode would answer "local" for a computer that
// only transcribes on Colab, and its transcription components would be
// named (SEC53). Then only python counts, which holds whatever those
// values are, and the caller does not keep the result (credentialChecks:
// the next request reads again).
func (s *Server) blockedComponents(n int) (ids []string, limit int, partial bool) {
	if !dataDirPathLimited || s.comps == nil {
		return nil, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	remote := s.asrMode(ctx).Mode == ASRModeRemote
	want := []string{components.IDPython}
	if hw, ok := s.knownHardware(ctx); ok && !remote {
		for _, id := range hwdetect.Recommend(hw).Components {
			if id == hwdetect.ComponentASRCUDA || id == hwdetect.ComponentASROpenVINO {
				want = append(want, id)
			}
		}
	}
	if s.colab != nil && (remote || s.colabOn(ctx)) {
		want = append(want, components.IDGcloud)
	}
	if ctx.Err() != nil {
		want, partial = []string{components.IDPython}, true
	}
	installed := map[string]bool{}
	for _, st := range s.comps.List() {
		if st.Installed {
			installed[st.ID] = true
		}
	}
	for _, id := range want {
		if installed[id] || s.ownSubstitute(id) {
			continue
		}
		l, ok := s.comps.DataDirLimit(id)
		if !ok || n <= l {
			continue
		}
		ids = append(ids, id)
		if len(ids) == 1 || l < limit {
			limit = l
		}
	}
	return ids, limit, partial
}

// ownSubstitute: something of the user's own stands in for the component
// (blockedComponents).
func (s *Server) ownSubstitute(id string) bool {
	backend := ""
	switch id {
	case components.IDPython:
		backend = config.ComponentBase
	case hwdetect.ComponentASRCUDA:
		backend = model.BackendFasterWhisper
	case hwdetect.ComponentASROpenVINO:
		backend = model.BackendOpenVINO
	case components.IDGcloud:
		_, ok := gcloudLookup(s)
		return ok
	}
	if backend == "" || s.cfg == nil {
		return false
	}
	p, src := s.cfg.PythonSource(backend)
	return (src == config.PythonFromConfig || src == config.PythonFromDefault) && pythonExists(p)
}

// knownHardware is this computer's hardware as far as it is known, for
// hwdetect.Recommend: the remembered detection (kv hardware.last), else this
// run's, plus the device detection's NVIDIA GPU. ok false when nothing is
// known yet.
func (s *Server) knownHardware(ctx context.Context) (hwdetect.HW, bool) {
	hw := hwdetect.HW{OS: "windows"} // blockedComponents runs where paths are limited
	known := false
	if h, ok := s.hardwareLast(ctx); ok {
		known = true
		hw.IntelGPU, hw.IntelNPU = h.IntelGPU, h.IntelNPU
		for _, name := range h.NVIDIA {
			hw.NVIDIA = append(hw.NVIDIA, hwdetect.GPU{Name: name})
		}
	} else if c, ok := s.cachedHW(); ok {
		known = true
		hw.IntelGPU, hw.IntelNPU, hw.NVIDIA = c.IntelGPU, c.IntelNPU, c.NVIDIA
	}
	if s.dev != nil && len(hw.NVIDIA) == 0 {
		if name := s.dev.LastDetection().CUDAName; name != "" {
			known = true
			hw.NVIDIA = append(hw.NVIDIA, hwdetect.GPU{Name: name})
		}
	}
	return hw, known
}

// The data directory's path length on Windows (test-upgrade F10; R6-E,
// from T20 option A and R59 (d)). The pipeline works below it in the
// lecture folders, {data_dir}\courses\{26}\lectures\{26} (lectureDirDepth
// more characters: ffmpeg and the transcription and text repair workers),
// and the material folders, {data_dir}\courses\{26}\materials\{26}
// (materialDirDepth, one deeper: the slide text extraction worker). Two
// Windows limits apply there (lengths in UTF-16 units):
//   - a process does not start in a working directory longer than
//     workDirMax ("The directory name is invalid": MAX_PATH 260 less the
//     NUL and the backslash Windows appends; measured on Windows 11: 258
//     starts, 259 does not); the deepest working directory is workDirDepth
//     below the data directory;
//   - without the system's long path support (LongPathsEnabled, off by
//     default) a Python worker cannot open a file whose path is longer than
//     filePathMax (MAX_PATH less the NUL). Go (it prefixes \\?\ itself) and
//     ffmpeg (likewise; T20 wrote to a 304-character path) are not limited.
//     The deepest file a worker opens in these folders is deepestFileDepth
//     below the data directory: in a lecture folder textrepair.py's result,
//     which it writes to --out + ".part" with --out the last name
//     internal/stages tmpFor tries (lectureDeepestFile, 71 + 23 = 94); in a
//     material folder extract_pdf.py's --out (materialDeepestFile, 72 + 18
//     = 90, the depth T20 counted). Outside them is not this check's:
//     asr_openvino.py preparing a model itself (python_openvino set in
//     config.toml, no component) opens files about 144 deep below
//     models\openvino\hf_cache, which on such a computer would need a data
//     directory of about 115 (R66 F3, worked out from the code); components
//     check their own depths (components.MaxPathLen).
//
// dataDirPathMax, the longest data directory under which both hold, is the
// smaller budget: min(258 − 72, 259 − 94) = min(186, 165) = 165; the file
// budget binds (186, the working directory's, was the limit before R6-E).
// Derived from the code, not reproduced on a machine without long path
// support (T20 §5); beyond it what fails first depends on the file (a held
// .tmp, the checkpoint, the result), so the texts say "may".
// dataDirPathHard, the working directory budget, is where
// codeDataDirPathTooLongToStart begins ({hard}): past it no process starts
// in a material folder, long path support or not (one more character and
// in a lecture folder neither; a code of its own, not a {hard} added to
// "too long", which the other locales' texts would have to carry first);
// test-upgrade measured a 229-character data directory failing and a
// 160-character one working. Within dataDirPathMargin of the limit the row
// warns that it is close. TestLectureDirDepth pins the folder depths to
// model, TestDeepestWorkerFile the deepest files to internal/stages and
// the workers. Only the data directory itself is checked (the diagnostics
// do not change how the processes are started); not on macOS, which has no
// such limit.
const (
	workDirMax          = 258
	filePathMax         = 259
	lectureDirDepth     = 71 // len(`\courses\` + 26 + `\lectures\` + 26)
	materialDirDepth    = 72 // len(`\courses\` + 26 + `\materials\` + 26)
	workDirDepth        = max(lectureDirDepth, materialDirDepth)
	lectureDeepestFile  = `\result.json.9.tmp.part` // textrepair.py: --out + ".part"
	materialDeepestFile = `\result.json.9.tmp`      // extract_pdf.py: --out
	deepestFileDepth    = max(lectureDirDepth+len(lectureDeepestFile), materialDirDepth+len(materialDeepestFile))
	dataDirPathHard     = workDirMax - workDirDepth
	dataDirPathMax      = min(dataDirPathHard, filePathMax-deepestFileDepth)
	dataDirPathMargin   = 10
)

// dataDirPathLimited: the platform limits the length of a working directory
// and (without long path support) of the files a worker opens: Windows.
var dataDirPathLimited = runtime.GOOS == "windows"

// dataDirPathCheck is the detail code for a data directory path too long
// for any process to start below it, too long for the files, or close to
// that limit, and its length; ok false when it is fine.
func dataDirPathCheck(dir string) (code string, n int, ok bool) {
	if !dataDirPathLimited {
		return "", 0, false
	}
	n = components.PathLen(strings.TrimRight(dir, `\/`))
	switch {
	case n > dataDirPathHard:
		return codeDataDirPathTooLongToStart, n, true
	case n > dataDirPathMax:
		return codeDataDirPathTooLong, n, true
	case n > dataDirPathMax-dataDirPathMargin:
		return codeDataDirPathNearLimit, n, true
	}
	return "", n, false
}

func credPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// cloudSyncService names the cloud service whose sync folder holds dir
// ("" for none), from the folder names the clients use: OneDrive (also
// "OneDrive - Org" and the OneDrive* environment variables), iCloud Drive
// (Windows iCloudDrive, macOS Mobile Documents; macOS Desktop & Documents
// when they are synced), Dropbox, Google Drive ("My Drive", "我的云端硬盘",
// macOS GoogleDrive-…), and other macOS File Provider folders under
// ~/Library/CloudStorage.
func cloudSyncService(dir, home string, getenv func(string) string, exists func(string) bool) string {
	clean := filepath.Clean(dir)
	within := func(root string) bool {
		if root == "" {
			return false
		}
		root = filepath.Clean(root)
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			return false
		}
		return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	for _, v := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		if within(getenv(v)) {
			return "OneDrive"
		}
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	for i, part := range parts {
		if svc := syncFolderService(part); svc != "" {
			return svc
		}
		if strings.EqualFold(part, "CloudStorage") && i > 0 && strings.EqualFold(parts[i-1], "Library") && i+1 < len(parts) {
			if svc := syncFolderService(parts[i+1]); svc != "" {
				return svc
			}
			name, _, _ := strings.Cut(parts[i+1], "-") // ~/Library/CloudStorage/Box-Box
			return name
		}
	}
	// macOS "Desktop & Documents Folders" in iCloud Drive: the folders keep
	// their place; iCloud Drive then holds a Desktop and a Documents folder.
	if runtime.GOOS == "darwin" && home != "" {
		for _, f := range []string{"Desktop", "Documents"} {
			if within(filepath.Join(home, f)) && exists(filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", f)) {
				return "iCloud Drive"
			}
		}
	}
	return ""
}

// syncFolderService names the service of a sync client's folder name.
func syncFolderService(name string) string {
	p := strings.ToLower(name)
	switch {
	case p == "onedrive" || strings.HasPrefix(p, "onedrive - ") || strings.HasPrefix(p, "onedrive-"):
		return "OneDrive"
	case p == "icloud drive" || p == "iclouddrive" || p == "mobile documents" || p == "com~apple~clouddocs":
		return "iCloud Drive"
	case p == "dropbox" || strings.HasPrefix(p, "dropbox (") || strings.HasPrefix(p, "dropbox-"):
		return "Dropbox"
	case p == "google drive" || p == "my drive" || p == "我的云端硬盘" || p == "googledrive" || strings.HasPrefix(p, "googledrive-"):
		return "Google Drive"
	}
	return ""
}
