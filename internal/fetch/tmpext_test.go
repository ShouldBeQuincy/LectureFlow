// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lectureflow/internal/bridge"
)

// SEC43b info-8: the one file of an HLS byte-range playlist or of a DASH
// SegmentBase Representation was saved as "src" plus its URL's
// extension. Ending in .tmp, it was taken for a temporary file after a
// failure for want of space (removeTempFiles), while ready.json still
// named it: every retry's merge failed. Its local name never ends in .tmp
// now (hlsPartExt: .seg), and the retry merges what was downloaded.
func TestSingleFileNamedTmpSurvivesNoSpace(t *testing.T) {
	for _, format := range []string{bridge.FormatHLS, bridge.FormatDASH} {
		t.Run(format, func(t *testing.T) {
			var low atomic.Bool
			e := newQEnv(t, func(o *Options) {
				o.MinFree = 1 << 30
				o.SpaceInterval = 10 * time.Millisecond
				o.FreeSpace = func(string) (int64, error) {
					if low.Load() {
						return 512 << 20, nil
					}
					return 100 << 30, nil
				}
			})
			video := pattern(3, 400_000)
			e.media.put("/files/v.mp4", video)
			e.media.stall("/files/v.mp4") // track 0 keeps the run going
			var single []byte
			var audio bridge.Track
			if format == bridge.FormatHLS {
				pl, end := panoptoMedia(5, 1000, true)
				single = pattern(9, int(end)+100)
				e.media.put("/h/fragmented.tmp", single)
				e.media.put("/h/index.m3u8", []byte(strings.ReplaceAll(pl, "fragmented.mp4", "fragmented.tmp")))
				audio = e.hlsTrack(bridge.KindAudio, "/h/index.m3u8")
			} else {
				single = pattern(50, 9000)
				e.media.put("/d/a/de.tmp", single)
				e.media.put("/d/manifest.mpd", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT8S" minBufferTime="PT2S">
  <Period>
    <AdaptationSet mimeType="audio/mp4" lang="de">
      <Representation id="a-de" bandwidth="64000" codecs="mp4a.40.2"><BaseURL>a/de.tmp</BaseURL><SegmentBase indexRange="0-100"/></Representation>
    </AdaptationSet>
  </Period>
</MPD>`))
				audio = e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:de:0")
			}
			id := e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), audio))
			sub := filepath.Join(e.q.taskDir(id), "t1")
			e.waitFor("the single file downloaded", func() bool { return fileExists(filepath.Join(sub, "ready.json")) })
			low.Store(true)
			info := e.wait(id, bridge.StateFailed)
			if info.Error == nil || !strings.HasPrefix(info.Error.Code, CodeNoSpace) {
				t.Fatalf("%+v", info.Error)
			}
			kept, _ := filepath.Glob(filepath.Join(sub, "src.*"))
			if len(kept) != 1 || strings.HasSuffix(kept[0], ".tmp") {
				t.Fatalf("the downloaded file after the failure: %q (files %v)", kept, filesIn(t, sub))
			}
			low.Store(false)
			e.media.release()
			if _, err := e.q.Retry(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			info = e.wait(id, bridge.StateDone)
			if got := readAll(t, e.lecture(info.LectureID).MediaPath); !bytes.Contains(got, single) || !bytes.Contains(got, video) {
				t.Fatalf("media of %d bytes lacks a track", len(got))
			}
		})
	}
}

// The single file's local name: the URL's media extension as before, the
// neutral .seg for any other (.tmp, .part, .aria2, a playlist's).
func TestSingleFileLocalName(t *testing.T) {
	for raw, want := range map[string]string{
		"https://cdn.example.com/s/fragmented.mp4?sig=1": "src.mp4",
		"https://cdn.example.com/s/audio.m4a":            "src.m4a",
		"https://cdn.example.com/s/seg.m4s":              "src.m4s",
		"https://cdn.example.com/s/file":                 "src.mp4",
		"https://cdn.example.com/s/file.tmp":             "src.seg",
		"https://cdn.example.com/s/file.TMP":             "src.seg",
		"https://cdn.example.com/s/file.part":            "src.seg",
		"https://cdn.example.com/s/file.aria2":           "src.seg",
		"https://cdn.example.com/s/file.m3u8":            "src.seg",
		"https://cdn.example.com/s/file.mpd":             "src.seg",
	} {
		if got := singleFileName(raw, ".mp4"); got != want {
			t.Errorf("singleFileName(%s) = %s, want %s", raw, got, want)
		}
	}
}
