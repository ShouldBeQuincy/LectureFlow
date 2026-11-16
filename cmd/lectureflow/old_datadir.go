// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package main

import (
	"context"
	"log/slog"
	"time"

	"lectureflow/internal/db"
	"lectureflow/internal/relocate"
)

// scrubOldDataDirTimeout bounds the scrub of the old database at startup
// (a VACUUM of the user's database: milliseconds for a few MB).
const scrubOldDataDirTimeout = time.Minute

// scrubOld is relocate.ScrubOldDataDir; tests record its calls.
var scrubOld = relocate.ScrubOldDataDir

// scrubOldDataDir runs at startup on the database st of dataDir. After a
// data directory move has switched over (kv app.old_data_dir names the old
// directory), the old directory's database loses the Colab credential
// (relocate.ScrubOldDataDir; v3 Gate round 2, lead ruling) once: success
// is recorded in kv app.old_data_dir_scrubbed. The rest of the old
// directory stays until the user deletes it. A failure is only a warning
// and is tried again at the next start.
//
// A recorded directory that is not an absolute path on this computer's
// drives (relocate.LocalAbsPath: a UNC or device path, a URL, a relative
// path) is never opened (final security audit AUD1 L-1: an imported
// database could name one, and opening a path on another computer sends
// it the user's Windows credentials); the app's own moves record clean
// absolute paths.
func scrubOldDataDir(ctx context.Context, st db.Store, dataDir string) {
	old, ok, err := st.GetKV(ctx, relocate.KVOldDataDir)
	if err != nil || !ok || old == "" {
		return
	}
	if !relocate.LocalAbsPath(old) {
		slog.Warn("the recorded old data directory is not an absolute path on this computer's drives; it is left alone", "dir", old)
		return
	}
	if done, _, _ := st.GetKV(ctx, relocate.KVOldDataDirScrubbed); done == old {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, scrubOldDataDirTimeout)
	defer cancel()
	if err := scrubOld(sctx, old, dataDir); err != nil {
		slog.Warn("removing the Colab credential from the old data directory's database failed; trying again at the next start",
			"dir", old, "err", err)
		return
	}
	if err := st.SetKV(ctx, relocate.KVOldDataDirScrubbed, old); err != nil {
		slog.Warn("recording the scrub of the old data directory", "err", err)
		return
	}
	slog.Info("removed the Colab credential from the old data directory's database", "dir", old)
}
