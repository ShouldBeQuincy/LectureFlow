// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"lectureflow/internal/relocate"
)

// SEC44 low-4: a recorded old data directory that is not an absolute path
// on this computer's drives (relocate.LocalAbsPath: a share an imported
// database of an older version brought along, a device path, a URL, a
// relative path) is no old data directory for the settings page: GET
// shows none and never stats it, DELETE answers old_data_dir.none. (No
// share in the test: the old code would connect to it.)
func TestOldDataDirOnlyLocal(t *testing.T) {
	e := newDDEnv(t)
	ctx := context.Background()
	for _, v := range []string{"old data", `\\.\pipe\lf-sec44-none`, "file://server/share/LectureFlow", `C:relative`} {
		if err := e.st.SetKV(ctx, relocate.KVOldDataDir, v); err != nil {
			t.Fatal(err)
		}
		res, b := e.req("GET", "/api/app/old-data-dir", nil)
		var o OldDataDir
		if res.StatusCode != http.StatusOK || json.Unmarshal(b, &o) != nil || o.Path != "" || o.Exists || o.Bytes != 0 {
			t.Errorf("GET with %q: %d %s", v, res.StatusCode, b)
		}
		res, b = e.req("DELETE", "/api/app/old-data-dir", map[string]string{"path": v})
		if res.StatusCode != http.StatusNotFound || errCode(t, b) != "old_data_dir.none" {
			t.Errorf("DELETE with %q: %d %s", v, res.StatusCode, b)
		}
	}
}
