// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/dirperm"
	"lectureflow/internal/selfupdate"
)

// The installation directory (security audit M-4). A live row of the
// diagnostics, install_dir, after the data folder's rows (data_dir,
// credentials, data_dir_location), only when the desktop shell names an
// installation directory (DesktopOptions.InstallDir: the Windows
// installation set up by the installer; development, the portable build,
// macOS and web mode have none and show no row):
//
//   - ok, check.install_dir_private {path}: only the user (with SYSTEM, the
//     administrators, TrustedInstaller) can change the programs there;
//   - warn, check.install_dir_shared {path, who}: another account owns or
//     may change the folder or an entry in it, or may remove or rename it
//     or a folder above it (path names which); the next start could run
//     their programs, and the in-app update installs nothing
//     (selfupdate.untrusted_install_dir);
//   - warn, check.install_dir_unconfirmed {path, reason}: the check could
//     not vouch for it (a link, more entries than expected, an entry or a
//     folder above whose permissions cannot be read); the update refuses
//     it the same way.
//
// Both warnings carry a hint of what to do (security review SEC20 低-1:
// the installer proposes the folder of the last installation, so "install
// again" alone would put it back where it was):
//
//   - check.hint_install_dir_profile {path}: the installation lies in the
//     user's own profile or %LOCALAPPDATA% (the default) and still is not
//     vouched for — installing again into the same place does not help;
//     an administrator has to look at path;
//   - check.hint_install_dir_unconfirmed {path, default}: the check could
//     not vouch for path (a link on the way, an unreadable entry): install
//     again into the default folder, or into one reached without a link;
//   - check.hint_install_dir_above {path, default}: path is a folder above
//     the installation (as written, or above where it really lies: subst,
//     mapped drives, links) that others may rename or move: install again
//     into the default folder, or tighten path's permissions;
//   - check.hint_install_dir_root {path, default}: the same when path is
//     the root of a drive or share (C:\, \\server\share\), which is not
//     renamed or moved, but whose entries others may delete or replace
//     (SEC20 recheck 低-E); its permissions are an administrator's;
//   - check.hint_install_dir {dir, default}: the folder itself or something
//     in it: uninstall and install again into the default folder, or
//     tighten dir's permissions.
//
// default is the folder the installer proposes once LectureFlow was
// uninstalled (the uninstaller removes the record of the last place):
// %LOCALAPPDATA%\Programs\LectureFlow (the product name: the installer
// is per user and not one-click), written out (SEC20b 低-C).
//
// There is no button: the folder may
// need an administrator, and changing a program folder behind the user's
// back is worse than saying so. The row asks selfupdate.CheckInstallDir
// itself, so it never calls private what the update refuses. The walk
// reads the owners and permissions of about 150 entries (a few
// milliseconds); the row is kept for installDirCacheFor (GET /api/system
// is polled).
const (
	checkInstallDir               = "install_dir"
	codeInstallDirPrivate         = "check.install_dir_private"
	codeInstallDirShared          = "check.install_dir_shared"
	codeInstallDirUnconfirmed     = "check.install_dir_unconfirmed"
	codeHintInstallDir            = "check.hint_install_dir"
	codeHintInstallDirAbove       = "check.hint_install_dir_above"
	codeHintInstallDirRoot        = "check.hint_install_dir_root"
	codeHintInstallDirProfile     = "check.hint_install_dir_profile"
	codeHintInstallDirUnconfirmed = "check.hint_install_dir_unconfirmed"
	installDirCacheFor            = 10 * time.Second
)

var installDirCheckCodes = map[string]errDef{
	codeInstallDirPrivate:         {"LectureFlow 安装在 {path}，只有你的账户（以及系统与管理员）能修改其中的程序", []string{"path"}},
	codeInstallDirShared:          {"本机的其他账户可以修改或替换 LectureFlow 的程序文件（{path}：{who}）：下次启动时运行的可能是他们放进去的程序。为安全起见，应用内更新已停用", []string{"path", "who"}},
	codeInstallDirUnconfirmed:     {"无法确认 LectureFlow 的安装目录只有你的账户能修改（{path}）：{reason}。为安全起见，应用内更新已停用", []string{"path", "reason"}},
	codeHintInstallDir:            {"卸载 LectureFlow，再到发布页下载新版安装包重新安装，安装时保持安装程序给出的默认位置（{default}）；或者自己（或请管理员）把 {dir} 的权限收紧为只有你的账户、SYSTEM 与 Administrators 可以修改。处理后点击“重新检查”", []string{"dir", "default"}},
	codeHintInstallDirAbove:       {"问题出在上层文件夹 {path}：其他账户可以把它改名或移走，换成自己的文件夹。卸载 LectureFlow，再到发布页下载新版安装包重新安装，安装时保持安装程序给出的默认位置（{default}）；或者把 {path} 的权限收紧为只有你的账户、SYSTEM 与 Administrators 可以修改（这个文件夹如果是你自己建的，不需要管理员）。处理后点击“重新检查”", []string{"path", "default"}},
	codeHintInstallDirRoot:        {"问题出在 {path}，也就是这个磁盘的根目录或网络共享的根目录：其他账户可以删掉或替换其中的条目（包括通往安装目录的那个文件夹），换成自己的。卸载 LectureFlow，再到发布页下载新版安装包重新安装，安装时保持安装程序给出的默认位置（{default}）；或者请管理员把 {path} 的权限收紧为只有你的账户、SYSTEM 与 Administrators 可以修改（这会影响其中的所有文件夹）。处理后点击“重新检查”", []string{"path", "default"}},
	codeHintInstallDirUnconfirmed: {"无法核实 {path}（原因见上一行）。卸载 LectureFlow，再到发布页下载新版安装包重新安装，安装时保持安装程序给出的默认位置（{default}）；原因里提到链接（junction、符号链接）时，也可以把 LectureFlow 装到不经过链接的文件夹。处理后点击“重新检查”", []string{"path", "default"}},
	codeHintInstallDirProfile:     {"LectureFlow 装在你自己的用户文件夹里，这里通常只有你能修改；现在判为不可信，说明 {path} 的权限被改过（例如公司电脑的管理软件加了别的账户），或者这个位置是用链接搬到别处的。重新安装到同一位置解决不了：请把这一行的内容交给电脑的管理员，检查 {path} 的权限。处理后点击“重新检查”", []string{"path"}},
}

func init() {
	for code, def := range installDirCheckCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// checkInstallDirTrust is selfupdate.CheckInstallDir (tests replace it).
var checkInstallDirTrust = selfupdate.CheckInstallDir

// installDirRow caches the row.
var installDirRow struct {
	mu  sync.Mutex
	dir string
	at  time.Time
	row SystemCheck
}

// installDir is the installation directory the shell named ("" = none).
func (s *Server) installDir() string {
	if s.desktop == nil {
		return ""
	}
	return s.desktop.InstallDir
}

// installDirCheck is the install_dir row (false: no installation
// directory, no row).
func (s *Server) installDirCheck(fresh bool) (SystemCheck, bool) {
	dir := s.installDir()
	if dir == "" {
		return SystemCheck{}, false
	}
	installDirRow.mu.Lock()
	defer installDirRow.mu.Unlock()
	if !fresh && installDirRow.dir == dir && time.Since(installDirRow.at) < installDirCacheFor {
		return installDirRow.row, true
	}
	c := SystemCheck{Name: checkInstallDir, Status: statusOK}
	err := checkInstallDirTrust(dir)
	switch {
	case err == nil:
		c.setDetail(codeInstallDirPrivate, map[string]any{"path": dir})
	case selfupdate.CodeOf(err) == selfupdate.CodeUntrustedInstallDir:
		info := selfupdate.InfoOf(err)
		p, _ := info.Params["path"].(string)
		who, _ := info.Params["who"].(string)
		if p == "" {
			p = dir
		}
		c.Status = statusWarn
		why, unconfirmed := strings.CutPrefix(who, dirperm.NotConfirmed)
		if unconfirmed {
			c.setDetail(codeInstallDirUnconfirmed, map[string]any{"path": p, "reason": why})
		} else {
			c.setDetail(codeInstallDirShared, map[string]any{"path": p, "who": who})
		}
		setInstallDirHint(&c, dir, p, unconfirmed)
	default:
		// the folder is gone or cannot be read: not vouched for either
		// (the update's check refuses it too)
		c.Status = statusWarn
		c.setDetail(codeInstallDirUnconfirmed, map[string]any{"path": dir, "reason": selfupdate.InfoOf(err).Detail})
		setInstallDirHint(&c, dir, dir, true)
	}
	installDirRow.dir, installDirRow.at, installDirRow.row = dir, time.Now(), c
	return c, true
}

// Replaced in tests: the user's %LOCALAPPDATA% and profile folder, and
// where a path really lies (dirperm.FinalPath).
var (
	installLocalAppData = func() string { return lookupEnv("LOCALAPPDATA") }
	installProfile      = func() string { h, _ := userHome(); return h }
	installFinalPath    = dirperm.FinalPath
)

// defaultInstallDir is where the installer proposes to install for this
// user (electron-builder: %LOCALAPPDATA%\Programs\<name>), written out so
// that it can be compared with what the installer shows.
func defaultInstallDir() string {
	if l := installLocalAppData(); l != "" && filepath.IsAbs(l) {
		return filepath.Join(l, "Programs", "LectureFlow")
	}
	return `%LOCALAPPDATA%\Programs\LectureFlow`
}

// setInstallDirHint gives the warning about the installation dir the hint
// that fits where the problem p lies (see the top of this file):
// unconfirmed — the check could not vouch for p (a link on the way, an
// unreadable entry): no claim about who may change what.
func setInstallDirHint(c *SystemCheck, dir, p string, unconfirmed bool) {
	def := defaultInstallDir()
	switch {
	case insideFolder(dir, installLocalAppData()) || insideFolder(dir, installProfile()):
		c.setHint(codeHintInstallDirProfile, map[string]any{"path": p})
	case unconfirmed:
		c.setHint(codeHintInstallDirUnconfirmed, map[string]any{"path": p, "default": def})
	case above(p, dir) && volumeRoot(p):
		c.setHint(codeHintInstallDirRoot, map[string]any{"path": p, "default": def})
	case above(p, dir):
		c.setHint(codeHintInstallDirAbove, map[string]any{"path": p, "default": def})
	default:
		c.setHint(codeHintInstallDir, map[string]any{"dir": dir, "default": def})
	}
}

// above: p is a folder above dir, as written or above where dir really
// lies (a subst or mapped drive letter, a link: the check walks up that
// path too).
func above(p, dir string) bool {
	if p == "" {
		return false
	}
	ds := []string{dir}
	if real, err := installFinalPath(dir); err == nil {
		ds = append(ds, real)
	}
	for _, d := range ds {
		if !strings.EqualFold(filepath.Clean(p), filepath.Clean(d)) && insideFolder(d, p) {
			return true
		}
	}
	return false
}

// volumeRoot: p is the root of a drive or share (C:\, \\server\share
// with or without the separator; / elsewhere), judged as the check's walk
// up judges its top (filepath.Dir of a root is itself).
func volumeRoot(p string) bool {
	c := filepath.Clean(p)
	return filepath.Dir(c) == c
}

// insideFolder: p is folder or lies in it (absolute paths; without regard
// to case, as on Windows, where installations are checked). An empty
// folder holds nothing.
func insideFolder(p, folder string) bool {
	if folder == "" || !filepath.IsAbs(folder) {
		return false
	}
	rel, err := filepath.Rel(strings.ToLower(filepath.Clean(folder)), strings.ToLower(filepath.Clean(p)))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// withInstallDirRow inserts the install_dir row after the data folder's
// rows (data_dir, credentials, data_dir_location; at the end without them).
func (s *Server) withInstallDirRow(rows []SystemCheck) []SystemCheck {
	c, ok := s.installDirCheck(false)
	if !ok {
		return rows
	}
	at := len(rows)
	for i, r := range rows {
		if r.Name == checkDataDir || r.Name == checkCredentials || r.Name == checkDataDirLocation {
			at = i + 1
		}
	}
	out := make([]SystemCheck, 0, len(rows)+1)
	out = append(out, rows[:at]...)
	out = append(out, c)
	return append(out, rows[at:]...)
}
