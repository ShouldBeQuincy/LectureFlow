// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"fmt"

	"lectureflow/internal/db"
)

// A database that cannot be opened or migrated (test-upgrade F5): held by
// another program, read-only, not a database (damaged), not openable, the
// disk full or failing. The start fails as before, with a code the desktop
// shell explains in the interface language (desktop/locales
// "startfail.db_*") and an ERROR line in Chinese and English that still
// ends with the technical cause (also in the log). Other errors keep their
// "open db: …" text.
const (
	failDBBusy     = "startup.db_busy"           // params: path
	failDBReadOnly = "startup.db_readonly"       // params: path
	failDBNotDB    = "startup.db_not_a_database" // params: path, backups
	failDBCantOpen = "startup.db_cant_open"      // params: path
	failDBFull     = "startup.db_disk_full"      // params: path
	failDBIO       = "startup.db_io_error"       // params: path
)

// dbFailure is err (from opening, migrating or first using
// {dataDir}/lectureflow.db) as a coded start failure when its cause is
// known, else "<what>: err".
func dbFailure(dataDir, what string, err error) error {
	path := dbFile(dataDir)
	tech := what + ": " + err.Error()
	var code, zh, en string
	params := map[string]any{"path": path}
	switch db.ClassifyOpenError(err) {
	case db.OpenFailBusy:
		code = failDBBusy
		zh = fmt.Sprintf("数据库文件 %s 正被其他程序占用（另一个 LectureFlow、数据库查看工具、备份、同步或杀毒软件），数据没有改动。请关闭它后重试。", path)
		en = fmt.Sprintf("The database file %s is in use by another program (another LectureFlow, a database viewer, backup, sync or antivirus software); nothing was changed. Close it and try again.", path)
	case db.OpenFailReadOnly:
		code = failDBReadOnly
		zh = fmt.Sprintf("数据库文件 %s 不能写入：文件或数据目录是只读的，或当前账户没有修改权限，也可能被其他程序（备份、同步、杀毒软件）占用。请取消只读、检查数据目录的权限后重试；被其他程序占用时稍后重试。", path)
		en = fmt.Sprintf("The database file %s cannot be written: the file or the data folder is read-only, or this account may not change it, or another program (backup, sync or antivirus software) is holding it. Clear the read-only setting, check the data folder's permissions and try again; if another program holds it, try again later.", path)
	case db.OpenFailNotDB:
		code = failDBNotDB
		b := backupsDir(dataDir)
		params["backups"] = b
		zh = fmt.Sprintf("%s 不是可用的数据库文件（已损坏，或被别的文件替换），LectureFlow 没有改动它。可以把它连同同名的 lectureflow.db-wal、lectureflow.db-shm（如有）一起移走，再把 %s 里的备份复制到数据目录并改名为 lectureflow.db（只放这一个文件）；或把这几个文件一起移走后以空的资料库启动。", path, b)
		en = fmt.Sprintf("%s is not a usable database file (damaged, or replaced by another file); LectureFlow left it as it is. Move it away together with lectureflow.db-wal and lectureflow.db-shm next to it (if any), then copy a backup from %s into the data folder and rename it to lectureflow.db (only that one file); or move those files away together to start with an empty library.", path, b)
	case db.OpenFailCantOpen:
		code = failDBCantOpen
		zh = fmt.Sprintf("无法打开数据库文件 %s：可能被其他程序（备份、同步、杀毒软件）占用，或当前账户无权访问数据目录，或数据目录所在的磁盘不可用。被占用时稍后重试；否则请检查数据目录的权限与磁盘后重试。", path)
		en = fmt.Sprintf("The database file %s cannot be opened: another program (backup, sync or antivirus software) may be holding it, this account may not have access to the data folder, or its drive is not available. If another program holds it, try again later; otherwise check the data folder's permissions and drive, then try again.", path)
	case db.OpenFailFull:
		code = failDBFull
		zh = fmt.Sprintf("数据库文件 %s 所在的磁盘已满，数据库无法写入。请清理出空间后重试。", path)
		en = fmt.Sprintf("The disk of the database file %s is full and the database cannot be written. Free some space and try again.", path)
	case db.OpenFailIO:
		code = failDBIO
		zh = fmt.Sprintf("读写数据库文件 %s 时磁盘出错。请确认磁盘已连接且工作正常（移动硬盘、网络盘）后重试。", path)
		en = fmt.Sprintf("The disk failed while reading or writing the database file %s. Make sure the drive is connected and working (external or network drives), then try again.", path)
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
	return &startFailure{
		Code:   code,
		Params: params,
		Err:    err,
		Msg:    zh + " / " + en + " (" + tech + ")",
	}
}
