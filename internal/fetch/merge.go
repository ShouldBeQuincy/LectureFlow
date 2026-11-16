// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lectureflow/internal/bridge"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/procutil"
)

// merge turns the downloaded tracks into the lecture's files, unless one is
// an encrypted MP4 (checkProtected, CodeDRMProtected): HLS and
// DASH tracks are joined (or the single-file path remuxed) with the
// bundled ffmpeg; the second camera angle, when there is one, is remuxed
// alone into AngleFile (angle.go); then the audio track with the main
// picture are combined (-c copy, -shortest: the length both have) into
// media{ext}, or the one of them there is becomes it. media{ext} comes
// last: process skips the merge once it exists. Nothing here uses the
// network or the credentials.
//
// Every downloaded file is read with the whitelists of media.InputArgs
// (security review 2, M3): its bytes come from the web and its extension
// from the URL, so a "video" may well be a playlist that would make ffmpeg
// read other files of this computer or a network share. Only the local
// HLS playlist LectureFlow writes itself is read as HLS (hlsDemuxer).
func (q *Queue) merge(ctx context.Context, t *task, s runSnapshot, dir string) (string, error) {
	n := len(s.records)
	lay := layoutOf(s.records)
	angled := lay.angle >= 0 && angleDone(dir)
	finals := make([]string, n)
	work := n > 1
	for i := range s.records {
		if i == lay.angle && angled {
			continue
		}
		finals[i] = findFinal(dir, trackName(i))
		work = work || finals[i] == ""
	}
	// an encrypted MP4 is not handed to ffmpeg nor imported (drm.go)
	if err := checkProtected(dir, finals, n); err != nil {
		return "", err
	}
	if work {
		if err := q.setState(t, bridge.StateMerging); err != nil {
			return "", err
		}
	}
	for i, r := range s.records {
		if finals[i] != "" || (i == lay.angle && angled) {
			continue
		}
		f, err := q.joinTrack(ctx, dir, i, r.Kind)
		if err != nil {
			return "", err
		}
		finals[i] = f
	}
	if lay.angle >= 0 && !angled {
		if err := q.remuxAngle(ctx, dir, finals[lay.angle]); err != nil {
			return "", err
		}
		os.Remove(finals[lay.angle])
	}
	if lay.audio < 0 || lay.main < 0 {
		one := lay.main
		if one < 0 {
			one = lay.audio
		}
		media := filepath.Join(dir, "media"+filepath.Ext(finals[one]))
		if err := renameRetry(finals[one], media); err != nil {
			return "", writeFailure(err)
		}
		return media, nil
	}
	video, audio := finals[lay.main], finals[lay.audio]
	media := filepath.Join(dir, "media.mp4")
	tmp := media + ".tmp"
	args := []string{"-hide_banner", "-nostdin", "-y"}
	args = appendInput(args, video)
	args = appendInput(args, audio)
	args = append(args, "-map", "0:v:0?", "-map", "1:a:0", "-c", "copy", "-shortest", "-f", "mp4", tmp)
	if err := q.ffmpeg(ctx, args); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := renameRetry(tmp, media); err != nil {
		return "", writeFailure(err)
	}
	os.Remove(video)
	os.Remove(audio)
	return media, nil
}

// appendInput adds a downloaded file as the next ffmpeg input: the
// whitelists of media.InputArgs right before its "-i" (after it they would
// be output options and do nothing).
func appendInput(args []string, path string) []string {
	args = append(args, lfmedia.InputArgs()...)
	return append(args, "-i", path)
}

// hlsDemuxer are the ffmpeg options of a local HLS input: any file name
// (keys, segments without the usual extensions), local files and the
// AES-128 decryption only, and the hls demuxer with, for the segments, the
// demuxers of lecture media (media.HLSFormatWhitelist; security review 2,
// M3). ffmpeg before 7.1 knows only -allowed_extensions and the whitelists
// (hlsDemuxerOld).
var (
	hlsDemuxer = []string{"-allowed_extensions", "ALL", "-allowed_segment_extensions", "ALL", "-extension_picky", "0",
		"-protocol_whitelist", "file,crypto", "-format_whitelist", lfmedia.HLSFormatWhitelist}
	hlsDemuxerOld = []string{"-allowed_extensions", "ALL", "-protocol_whitelist", "file,crypto", "-format_whitelist", lfmedia.HLSFormatWhitelist}
)

// joinTrack makes t{i}{ext} from the parts of an HLS or DASH track
// (ready.json): a DASH part's init section and segments are first
// concatenated into one file, then ffmpeg remuxes (-c copy) the part, or
// the part with its separate audio. When every part is an HLS playlist of
// fragmented MP4 without discontinuities (fmp4Continuous) the remux keeps
// the segments' own timestamps (-copyts -start_at_zero).
func (q *Queue) joinTrack(ctx context.Context, dir string, i int, kind string) (string, error) {
	sub := filepath.Join(dir, trackName(i))
	b, err := os.ReadFile(filepath.Join(sub, "ready.json"))
	if err != nil {
		return "", writeFailure(err)
	}
	var plan trackPlan
	if err := json.Unmarshal(b, &plan); err != nil {
		return "", &failure{code: CodeMergeFailed, detail: "ready.json: " + err.Error()}
	}
	parts := []trackPart{plan.Main}
	if plan.Audio != nil {
		parts = append(parts, *plan.Audio)
	}
	joined := make([]string, len(parts))
	webm := true
	copyTS := true // every input is an fMP4 playlist without discontinuities (fmp4Continuous)
	for k, p := range parts {
		webm = webm && p.Ext == ".webm"
		if p.Playlist == "" {
			copyTS = false
		} else {
			pl := filepath.Join(dir, filepath.FromSlash(p.Dir), p.Playlist)
			// security review 3, I1: no segment of the local playlist is
			// itself a playlist or manifest
			if err := refusePlaylistParts(pl); err != nil {
				if f, ok := err.(*failure); ok && f.code == CodeMergeFailed {
					os.Remove(filepath.Join(sub, "ready.json")) // a part is missing: the next run fetches it
				}
				return "", err
			}
			copyTS = copyTS && fmp4Continuous(pl)
		}
		if len(p.Parts) == 0 {
			continue
		}
		if joined[k], err = concatParts(filepath.Join(dir, filepath.FromSlash(p.Dir)), p); err != nil {
			if f, ok := err.(*failure); ok && f.code == CodeMergeFailed {
				// a part is missing: the next run fetches it. Not after a
				// write the disk refused (no space, write_failed): the
				// parts are all there, the retry joins them (SEC43
				// medium-2)
				os.Remove(filepath.Join(sub, "ready.json"))
			}
			return "", err
		}
	}
	ext, format := defaultExt(kind), "mp4"
	if webm {
		ext, format = ".webm", "webm"
	}
	out := filepath.Join(dir, trackName(i)+ext)
	tmp := out + ".tmp"
	build := func(demux []string) []string {
		args := []string{"-hide_banner", "-nostdin", "-y"}
		if copyTS {
			args = append(args, "-copyts", "-start_at_zero")
		}
		input := func(k int, p trackPart) {
			pdir := filepath.Join(dir, filepath.FromSlash(p.Dir))
			switch {
			case joined[k] != "":
				args = appendInput(args, joined[k])
			case p.Source != "":
				args = appendInput(args, filepath.Join(pdir, p.Source))
			default:
				args = append(args, demux...)
				args = append(args, "-i", filepath.Join(pdir, p.Playlist))
			}
		}
		for k, p := range parts {
			input(k, p)
		}
		if len(parts) > 1 {
			args = append(args, "-map", "0:v:0?", "-map", "1:a:0")
		}
		return append(args, "-c", "copy", "-f", format, tmp)
	}
	err = q.ffmpeg(ctx, build(hlsDemuxer))
	var f *failure
	if errors.As(err, &f) && (strings.Contains(f.detail, "allowed_segment_extensions") || strings.Contains(f.detail, "extension_picky")) {
		err = q.ffmpeg(ctx, build(hlsDemuxerOld))
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := renameRetry(tmp, out); err != nil {
		return "", writeFailure(err)
	}
	removeAllRetry(sub)
	return out, nil
}

// refusePlaylistParts checks the segments and init sections the local
// HLS playlist pl names (the keys are random bytes and not looked at): one
// that starts like a playlist or manifest (media.SniffFile) is no media,
// and the local playlist's whitelist admits the hls demuxer
// (media.HLSFormatWhitelist; security review 3, I1). Besides the file
// names of hlsPartExt this is the second defence of our own (FFmpeg's is
// that a nested playlist's entries are refused). CodePlaylistInvalid for
// such a part, CodeMergeFailed when one is missing.
func refusePlaylistParts(pl string) error {
	b, err := os.ReadFile(pl)
	if err != nil {
		return writeFailure(err)
	}
	dir := filepath.Dir(pl)
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		name := ""
		switch {
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			if i := strings.Index(l, `URI="`); i >= 0 {
				if j := strings.IndexByte(l[i+5:], '"'); j >= 0 {
					name = l[i+5 : i+5+j]
				}
			}
		case l == "", strings.HasPrefix(l, "#"):
		default:
			name = l
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		kind, err := lfmedia.SniffFile(filepath.Join(dir, name))
		switch {
		case os.IsNotExist(err):
			return &failure{code: CodeMergeFailed, detail: "a downloaded part is missing: " + name}
		case err != nil:
			return writeFailure(err)
		case kind != "":
			return &failure{code: CodePlaylistInvalid, detail: "a segment is a playlist or manifest (" + kind + "), not media: " + name}
		}
	}
	return nil
}

// fmp4Continuous reports whether the local HLS playlist pl (as local()
// wrote it) is fragmented MP4 (an EXT-X-MAP) without an
// EXT-X-DISCONTINUITY: its segments' timestamps then continue one another
// (RFC 8216 §4.3.2.3: a change of the timestamp sequence needs the
// DISCONTINUITY tag), and joinTrack reads it with -copyts -start_at_zero
// (test-media I1).
//
// Why: the hls demuxer is flagged AVFMT_TS_DISCONT, so ffmpeg corrects
// what looks like a timestamp jump. An fMP4 segment holds all its video
// samples before its audio ones, and ffmpeg checks a stream's first packet
// against the last packet of any stream: with segments longer than ffmpeg's
// dts_delta_threshold (10 s) the first audio packet looked like a jump
// back by a segment, and the whole audio came out one segment late (14 s
// and 60 s on test files; segments of 2–6 s, the usual, are not affected).
// The hls demuxer hands the init section and the segments to one mov
// demuxer as one stream of bytes, so their timestamps are those of the
// file the bytes would make; -copyts only switches the correction off and
// -start_at_zero starts the file at 0 as before. With a DISCONTINUITY
// (TS or fMP4) the correction is needed — a new period's timestamps may
// start over — and stays; an fMP4 playlist with one and segments over 10 s
// can still come out with its audio late (NOTES.md, test-media fixes).
func fmp4Continuous(pl string) bool {
	b, err := os.ReadFile(pl)
	if err != nil {
		return false
	}
	fmp4 := false
	for _, l := range strings.Split(string(b), "\n") {
		tag, _, _ := strings.Cut(strings.TrimSpace(l), ":")
		switch tag {
		case "#EXT-X-MAP":
			fmp4 = true
		case "#EXT-X-DISCONTINUITY":
			return false
		}
	}
	return fmp4
}

// joinCopy copies a part into the joined file (a variable for the tests: a
// disk that fills up during the join).
var joinCopy = io.Copy

// concatParts joins a DASH part's init section and segments, in order,
// into joined{ext} in its directory and removes them; a joined file from
// an earlier run is used as it is.
func concatParts(pdir string, p trackPart) (string, error) {
	ext := p.Ext
	if ext == "" {
		ext = ".mp4"
	}
	joined := filepath.Join(pdir, "joined"+ext)
	if fileExists(joined) {
		return joined, nil
	}
	tmp := joined + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", writeFailure(err)
	}
	for _, name := range p.Parts {
		in, err := os.Open(filepath.Join(pdir, name))
		if err != nil {
			out.Close()
			os.Remove(tmp)
			if os.IsNotExist(err) {
				return "", &failure{code: CodeMergeFailed, detail: "a downloaded part is missing: " + name}
			}
			return "", writeFailure(err)
		}
		_, err = joinCopy(out, in)
		in.Close()
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return "", writeFailure(err)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", writeFailure(err)
	}
	if err := renameRetry(tmp, joined); err != nil {
		return "", writeFailure(err)
	}
	for _, name := range p.Parts {
		os.Remove(filepath.Join(pdir, name))
	}
	return joined, nil
}

// ffmpeg runs the bundled ffmpeg on local files only; a failure keeps the
// end of its output as the detail, with any URL in it reduced to its host
// and path hash (redactText: a downloaded file's metadata may quote one;
// security review L7).
func (q *Queue) ffmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, q.o.FFmpeg, args...)
	hideWindow(cmd)
	cmd.WaitDelay = 5 * time.Second
	var stderr tailBuffer
	cmd.Stderr = &stderr
	err := procutil.Run(cmd, procutil.Tied) // ends with LectureFlow, also on a crash (test-chaos 1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		detail := redactText(strings.TrimSpace(stderr.String()))
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			detail = "ffmpeg not found: " + q.o.FFmpeg
		}
		if detail == "" {
			detail = err.Error()
		}
		return &failure{code: CodeMergeFailed, detail: detail}
	}
	return nil
}

// tailBuffer keeps the last 4 KB written.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = append([]byte(nil), t.b[len(t.b)-4096:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.ToValidUTF8(string(t.b), "") }

// importMedia hands the file to the lecture import and records the task
// as done.
func (q *Queue) importMedia(ctx context.Context, t *task, s runSnapshot, media string) error {
	q.mu.Lock()
	imp := q.imp
	q.mu.Unlock()
	if imp == nil {
		return &failure{code: CodeImportFailed, detail: "no lecture import is configured"}
	}
	title := cleanTitle(s.title)
	req := ImportRequest{TaskID: s.id, CourseID: s.courseID, Path: media, Title: title,
		Filename: fileStem(title) + strings.ToLower(filepath.Ext(media)), RecordedAt: s.recordedAt}
	if layoutOf(s.records).angle >= 0 {
		angle := filepath.Join(filepath.Dir(media), AngleFile)
		if !fileExists(angle) {
			// it was merged before media{ext}: gone means someone removed it
			return &failure{code: CodeMergeFailed, detail: "the second angle's file is missing: " + AngleFile}
		}
		req.Angles = []ImportAngle{{Name: AngleName, Path: angle}}
	}
	res, err := imp(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// the file itself was refused: its own reason, not "import failed"
		var rf *ImportRefused
		if errors.As(err, &rf) && RefusedFile(rf.Code) {
			return &failure{code: rf.Code, params: rf.Params}
		}
		return &failure{code: CodeImportFailed, detail: err.Error()}
	}
	if res.LaunchError != nil {
		q.log.Info("downloads: the lecture stays a draft", "task", s.id, "lecture", res.LectureID, "cause", res.LaunchError.Code)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	id := res.LectureID
	t.rec.LectureID = &id
	done, total := t.bytes()
	if total < done {
		total = done
	}
	t.prog = []trackProgress{{done: done, total: total, seen: true}}
	if err := q.saveLocked(t, bridge.StateDone, nil); err != nil {
		return err
	}
	t.forget()
	t.prog = nil
	delete(q.tasks, s.id)
	q.log.Info("downloads: task done", "task", s.id, "lecture", id)
	go removeAllRetry(q.taskDir(s.id))
	return nil
}

// cleanTitle is the platform's title for display: one line, single spaces,
// no bidirectional control characters (lfmedia.StripBidi; test-media I8),
// at most 200 characters.
func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, lfmedia.StripBidi(s))
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, 200)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

// windowsReserved are device names Windows refuses as file names, also
// before an extension or trailing spaces ("CON.txt", "nul .mp4").
var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true}

// fileStem makes a title usable as a file name on every system: characters
// Windows forbids become spaces, no trailing dots or spaces, no device
// name (the part before the first dot gets a "_": "CON.mp4" → "CON_.mp4"),
// no bidirectional control characters (they can make a name show another
// extension; test-media I8), at most 120 characters (121 with that "_");
// "lecture" when nothing is left.
func fileStem(title string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return ' '
		}
		return r
	}, lfmedia.StripBidi(title))
	s = strings.Join(strings.Fields(s), " ")
	s = truncateRunes(s, 120)
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "lecture"
	}
	stem, rest, dotted := strings.Cut(s, ".")
	if windowsReserved[strings.ToUpper(strings.TrimRight(stem, " "))] {
		s = strings.TrimRight(stem, " ") + "_"
		if dotted {
			s += "." + rest
		}
	}
	return s
}
