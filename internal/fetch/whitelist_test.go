// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"lectureflow/internal/bridge"
	lfmedia "lectureflow/internal/media"
)

// Security review 2, M3 (media-integ): a downloaded file's bytes come from
// the web and its extension from the URL, so every ffmpeg input of the
// merge is read with the whitelists of media.InputArgs; only the local HLS
// playlist that LectureFlow writes itself is read as HLS
// (media.HLSFormatWhitelist, file and crypto only).

// checkGuardedInputs fails t unless every "-i x" of the ffmpeg call has
// the whitelists among its own options (after the previous input, before
// its -i: after it they would be output options).
func checkGuardedInputs(t *testing.T, call []string) {
	t.Helper()
	start := 0
	for i := 0; i+1 < len(call); i++ {
		if call[i] != "-i" {
			continue
		}
		in, opts := call[i+1], call[start:i]
		start = i + 2
		want := lfmedia.InputArgs()
		if filepath.Base(in) == "index.m3u8" {
			want = []string{"-format_whitelist", lfmedia.HLSFormatWhitelist, "-protocol_whitelist", "file,crypto"}
		}
		for k := 0; k+1 < len(want); k += 2 {
			if optionValue(opts, want[k]) != want[k+1] {
				t.Errorf("input %s: %s is %q, want %q (call %v)", filepath.Base(in), want[k], optionValue(opts, want[k]), want[k+1], call)
			}
		}
	}
}

// Every way a track reaches ffmpeg: a file track combined with the audio,
// the second angle's remux, HLS segments with a key (the local playlist),
// the HLS single-file path, DASH segments joined.
func TestDownloadInputsWhitelisted(t *testing.T) {
	e := newQEnv(t, nil)
	e.media.put("/files/a.m4a", pattern(1, 4000))
	e.media.put("/files/v.mp4", pattern(2, 12000))
	e.media.put("/files/v2.mp4", pattern(3, 9000))
	v2 := e.fileTrack(bridge.KindVideo, "/files/v2.mp4")
	v2.Role = "secondary-video"
	ids := []int64{e.enqueue(e.request("files", e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/v.mp4"), v2))}
	hlsSegments(e, "v720", 3, true)
	ids = append(ids, e.enqueue(e.request("hls", e.hlsTrack(bridge.KindVideo, "/v720/index.m3u8"))))
	pl, end := panoptoMedia(5, 1000, false)
	e.media.put("/sessions/x.hls/fragmented.mp4", pattern(9, int(end)+100))
	e.media.put("/sessions/x.hls/index.m3u8", []byte(pl))
	ids = append(ids, e.enqueue(e.request("single", e.hlsTrack(bridge.KindVideo, "/sessions/x.hls/index.m3u8"))))
	dashFixture(e, "d", "")
	ids = append(ids, e.enqueue(e.request("dash", e.dashTrack(bridge.KindVideo, "/d/manifest.mpd", "video:-:0"), e.dashTrack(bridge.KindAudio, "/d/manifest.mpd", "audio:fr:0"))))
	for _, id := range ids {
		e.wait(id, bridge.StateDone)
	}
	calls := e.ffmpegCalls()
	seen := map[string]bool{}
	for _, c := range calls {
		checkGuardedInputs(t, c)
		j := strings.Join(c, " ")
		switch {
		case strings.Contains(j, "index.m3u8"):
			seen["hls"] = true
		case strings.Contains(j, "src.mp4"):
			seen["single"] = true
		case strings.Contains(j, "joined"):
			seen["dash"] = true
		case strings.Contains(j, "-an"):
			seen["angle"] = true
		case strings.Count(j, "-i ") == 2:
			seen["combine"] = true
		}
	}
	for _, k := range []string{"hls", "single", "dash", "angle", "combine"} {
		if !seen[k] {
			t.Errorf("no %s call among %v", k, calls)
		}
	}
}

// outsidePlaylist is an HLS playlist whose segment is a file outside the
// task directory: what a page could hand over as a "video file".
func outsidePlaylist(seg string) []byte {
	return []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\n" + seg + "\n#EXT-X-ENDLIST\n")
}

// A "video file" whose URL ends in .m3u8 (the track is saved as t1.mp4,
// fileTrackExt) and holds a playlist naming a file of this computer:
// ffmpeg refuses it
// (the fake as the real one does with the whitelists), the task fails and
// nothing is imported; the same as the second angle.
func TestPlaylistFromURLRefused(t *testing.T) {
	for _, angle := range []bool{false, true} {
		t.Run(fmt.Sprintf("angle=%v", angle), func(t *testing.T) {
			e := newQEnv(t, nil)
			secret := filepath.Join(e.root, "private.ts")
			e.must(os.WriteFile(secret, pattern(7, 3000), 0o644))
			e.media.put("/files/a.m4a", pattern(1, 4000))
			e.media.put("/files/v.mp4", pattern(2, 12000))
			e.media.put("/files/lecture.m3u8", outsidePlaylist(secret))
			tracks := []bridge.Track{e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/lecture.m3u8")}
			if angle {
				v2 := e.fileTrack(bridge.KindVideo, "/files/lecture.m3u8")
				v2.Role = "secondary-video"
				tracks = []bridge.Track{tracks[0], e.fileTrack(bridge.KindVideo, "/files/v.mp4"), v2}
			}
			info := e.wait(e.enqueue(e.request("s1", tracks...)), bridge.StateFailed)
			if info.Error == nil || info.Error.Code != CodeMergeFailed || !strings.Contains(info.Error.Detail, "not on whitelist") {
				t.Fatalf("task error %+v", info.Error)
			}
			if imps := e.importsSnapshot(); len(imps) != 0 {
				t.Fatalf("imported %+v", imps)
			}
			for _, c := range e.ffmpegCalls() {
				checkGuardedInputs(t, c)
			}
		})
	}
}

func (e *qenv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// ---------- with the real ffmpeg (skipped without one, SPEC §14) ----------

// realFFmpeg is the bundled ffmpeg when the desktop build has staged it,
// else the one in PATH.
func realFFmpeg(t *testing.T) string {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	ff, _ := filepath.Abs(filepath.Join("..", "..", "desktop", "build", "resources", "bin", "ffmpeg"+exe))
	if fi, err := os.Stat(ff); err == nil && !fi.IsDir() {
		return ff
	}
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found: the real merge tests are skipped (SPEC §14)")
	}
	return p
}

// ffconcat is an ffconcat document naming one file (relative to it).
func ffconcat(name string) []byte {
	return []byte("ffconcat version 1.0\nfile '" + name + "'\n")
}

func makeReal(t *testing.T, ff, out string, args ...string) []byte {
	t.Helper()
	all := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	if b, err := exec.Command(ff, append(all, out)...).CombinedOutput(); err != nil {
		t.Fatalf("generating %s: %v\n%s", filepath.Base(out), err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const realSine = "sine=frequency=440:duration=3"

// The real ffmpeg with the merge's options: a list of other files handed
// over as a video file or as the second angle is refused before anything
// is read (the plain ffmpeg would follow it: checked first); real files
// are combined; real HLS segments, AES-128 encrypted, are joined through
// the local playlist with the hls whitelist.
//
// The list is an ffconcat document served as /files/lecture.mp4: the file
// keeps that media extension (fileTrackExt), and ffmpeg recognises the
// concat demuxer by the content alone, so only the merge's whitelist
// refuses it (SEC43 info-3: a playlist saved as .mp4 is no longer taken
// for HLS by an ffmpeg from 7.1 on, which then tested nothing of ours).
// It names the audio track downloaded next to it.
func TestRealMergeWhitelists(t *testing.T) {
	ff := realFFmpeg(t)
	gen := t.TempDir()
	seg := filepath.Join(gen, "private.ts")
	makeReal(t, ff, seg, "-f", "lavfi", "-i", realSine, "-c:a", "mp2", "-f", "mpegts")
	pl := filepath.Join(gen, "plain.m3u8")
	if err := os.WriteFile(pl, outsidePlaylist(seg), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", pl, "-c", "copy", "-f", "mpegts", filepath.Join(gen, "leak.ts")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg without the whitelists did not follow the playlist: %v\n%s", err, b)
	}
	// an ffconcat document named .mp4: the plain ffmpeg reads the file it
	// names (relative, as the concat demuxer's safe mode allows)
	concat := filepath.Join(gen, "concat.mp4")
	if err := os.WriteFile(concat, ffconcat(filepath.Base(seg)), 0o644); err != nil {
		t.Fatal(err)
	}
	leak := filepath.Join(gen, "leak2.ts")
	if b, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", concat, "-c", "copy", "-f", "mpegts", leak).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg without the whitelists did not follow the ffconcat document: %v\n%s", err, b)
	}
	if fi, err := os.Stat(leak); err != nil || fi.Size() == 0 {
		t.Fatalf("ffmpeg without the whitelists read nothing through the ffconcat document: %v", err)
	}
	video := makeReal(t, ff, filepath.Join(gen, "v.mp4"), "-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=3", "-c:v", "mpeg4")
	audio := makeReal(t, ff, filepath.Join(gen, "a.m4a"), "-f", "lavfi", "-i", realSine, "-c:a", "aac")

	t.Run("playlist refused", func(t *testing.T) {
		for _, angle := range []bool{false, true} {
			e := newQEnv(t, func(o *Options) { o.FFmpeg = ff })
			e.media.put("/files/a.m4a", audio)
			e.media.put("/files/v.mp4", video)
			e.media.put("/files/lecture.mp4", ffconcat("t0.m4a"))
			tracks := []bridge.Track{e.fileTrack(bridge.KindAudio, "/files/a.m4a"), e.fileTrack(bridge.KindVideo, "/files/lecture.mp4")}
			if angle {
				v2 := e.fileTrack(bridge.KindVideo, "/files/lecture.mp4")
				v2.Role = "secondary-video"
				tracks = []bridge.Track{tracks[0], e.fileTrack(bridge.KindVideo, "/files/v.mp4"), v2}
			}
			info := e.wait(e.enqueue(e.request("s1", tracks...)), bridge.StateFailed)
			if info.Error == nil || info.Error.Code != CodeMergeFailed || !strings.Contains(info.Error.Detail, "not on whitelist") {
				t.Fatalf("angle=%v: task error %+v", angle, info.Error)
			}
			if imps := e.importsSnapshot(); len(imps) != 0 {
				t.Fatalf("angle=%v: imported %+v", angle, imps)
			}
		}
	})

	t.Run("files combined", func(t *testing.T) {
		e := newQEnv(t, func(o *Options) { o.FFmpeg = ff })
		e.media.put("/files/a.m4a", audio)
		e.media.put("/files/v.mp4", video)
		info := e.wait(e.enqueue(e.request("s1", e.fileTrack(bridge.KindVideo, "/files/v.mp4"), e.fileTrack(bridge.KindAudio, "/files/a.m4a"))), bridge.StateDone)
		checkMedia(t, ff, e.lecture(info.LectureID).MediaPath, 2)
	})

	t.Run("hls segments joined", func(t *testing.T) {
		e := newQEnv(t, func(o *Options) { o.FFmpeg = ff })
		segDir := filepath.Join(gen, "hls")
		if err := os.MkdirAll(segDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if b, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-nostdin", "-y",
			"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=3", "-f", "lavfi", "-i", realSine,
			"-c:v", "mpeg2video", "-g", "10", "-c:a", "mp2", "-f", "segment", "-segment_time", "1", "-segment_format", "mpegts",
			filepath.Join(segDir, "s%d.ts")).CombinedOutput(); err != nil {
			t.Fatalf("segments: %v\n%s", err, b)
		}
		key, iv := []byte("0123456789abcdef"), make([]byte, aes.BlockSize)
		iv[15] = 0x0A
		e.media.put("/hls/key.bin", key)
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n")
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/hls/key.bin?kt=" + testSig + "\",IV=0x0000000000000000000000000000000A\n")
		segs, _ := filepath.Glob(filepath.Join(segDir, "s*.ts"))
		if len(segs) < 2 {
			t.Fatalf("segments %v", segs)
		}
		for i := range segs {
			name := fmt.Sprintf("s%d.ts", i)
			plain, err := os.ReadFile(filepath.Join(segDir, name))
			if err != nil {
				t.Fatal(err)
			}
			e.media.put("/hls/"+name, encryptCBC(t, key, iv, plain))
			b.WriteString("#EXTINF:1.0,\n" + name + "\n")
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		e.media.put("/hls/index.m3u8", []byte(b.String()))
		info := e.wait(e.enqueue(e.request("s1", e.hlsTrack(bridge.KindVideo, "/hls/index.m3u8"))), bridge.StateDone)
		checkMedia(t, ff, e.lecture(info.LectureID).MediaPath, 2)
	})
}

// checkMedia probes the imported media with the whitelists of prep: an
// accepted container with streams streams.
func checkMedia(t *testing.T, ff, path string, streams int) {
	t.Helper()
	ffprobe, err := lfmedia.FFprobeFor(ff)
	if err != nil {
		t.Skipf("no ffprobe: %v", err)
	}
	args := append([]string{"-v", "error"}, lfmedia.InputArgs()...)
	out, err := exec.Command(ffprobe, append(args, "-show_entries", "format=format_name,nb_streams", "-of", "default=nw=1", path)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe %s: %v\n%s", path, err, out)
	}
	s := string(out)
	if !strings.Contains(s, fmt.Sprintf("nb_streams=%d", streams)) {
		t.Errorf("%s: %s", filepath.Base(path), s)
	}
	for _, l := range strings.Split(s, "\n") {
		if f, ok := strings.CutPrefix(strings.TrimSpace(l), "format_name="); ok && !lfmedia.FormatAllowed(f) {
			t.Errorf("%s: format %s", filepath.Base(path), f)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Errorf("%s: %v", path, err)
	}
}

// encryptCBC is AES-128-CBC with PKCS#7 padding (HLS METHOD=AES-128).
func encryptCBC(t *testing.T, key, iv, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	in := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(in))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, in)
	return out
}
