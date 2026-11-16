// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"fmt"
	"path/filepath"
	"testing"

	"lectureflow/internal/bridge"
)

// AUD3 low-3 and info-1: a file track keeps the extension of its URL only
// when it is a lecture media extension (media.Exts, as an upload's name
// must have), else it gets the kind's default (.mp4 for a picture, .m4a for
// audio). A URL ending in .part, .tmp or .aria2 no longer leaves the task
// unfinished for good (the downloaded file looked like a partial one), and
// .exe or .html no longer reaches the lecture's file and its name.
func TestFileTrackExtension(t *testing.T) {
	e := newQEnv(t, nil)
	for i, c := range []struct{ path, kind, want string }{
		{"/files/v.part", bridge.KindVideo, ".mp4"},
		{"/files/a.aria2", bridge.KindAudio, ".m4a"},
		{"/files/v.tmp", bridge.KindVideo, ".mp4"},
		{"/files/v.exe", bridge.KindVideo, ".mp4"},
		{"/files/a.HTML", bridge.KindAudio, ".m4a"},
		{"/files/v.m3u8", bridge.KindVideo, ".mp4"},
		{"/files/v.MKV", bridge.KindVideo, ".mkv"},
		{"/files/a.mp3", bridge.KindAudio, ".mp3"},
		{"/files/v", bridge.KindVideo, ".mp4"},
	} {
		e.media.put(c.path, pattern(byte(i), 3000))
		id := e.enqueue(e.request(fmt.Sprint("s", i), e.fileTrack(c.kind, c.path)))
		info := e.wait(id, bridge.StateDone)
		imps := e.importsSnapshot()
		imp := imps[len(imps)-1]
		if filepath.Ext(imp.Path) != c.want || filepath.Ext(imp.Filename) != c.want ||
			filepath.Ext(e.lecture(info.LectureID).MediaPath) != c.want {
			t.Errorf("%s: import %q as %q, want %s", c.path, filepath.Base(imp.Path), imp.Filename, c.want)
		}
	}
}
