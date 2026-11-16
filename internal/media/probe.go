// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package media

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Runner runs a program and returns its standard output and error (the
// signature of api.Server.runCmd).
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

// Problem is why a file is refused as a lecture's media.
type Problem struct {
	// Kind is "empty" (the file has no bytes), "html" (SniffHTML: a web
	// page, test-media I6), "playlist" (SniffPlaylist found a playlist) or
	// "format" (ffmpeg would read it with a demuxer outside Demuxers).
	Kind string
	// Format is the playlist kind of SniffPlaylist, or the demuxer name
	// ffprobe reported ("hls", "concat", …; "" when it did not say); ""
	// for empty and html.
	Format string
}

// Kinds of Problem.
const (
	ProblemEmpty    = "empty"
	ProblemHTML     = "html"
	ProblemPlaylist = "playlist"
	ProblemFormat   = "format"
)

// ProbeTimeout bounds the ffprobe run of Check.
const ProbeTimeout = 60 * time.Second

// FFprobeFor returns the ffprobe next to ffmpeg ("ffmpeg" in the file name
// replaced by "ffprobe"), else the one in PATH (the rule of internal/stages).
func FFprobeFor(ffmpeg string) (string, error) {
	dir, base := filepath.Split(ffmpeg)
	ext := filepath.Ext(base)
	if name := strings.TrimSuffix(base, ext); strings.EqualFold(name, "ffmpeg") && dir != "" {
		p := filepath.Join(dir, "ffprobe"+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("ffprobe")
	if err != nil {
		return "", fmt.Errorf("ffprobe not found next to %s or in PATH: %w", ffmpeg, err)
	}
	return p, nil
}

// notOnWhitelistRe finds the demuxer of ffprobe's "[hls @ 0000…] Format not
// on whitelist '…'" (the message of libavformat's avformat_open_input).
var notOnWhitelistRe = regexp.MustCompile(`\[([A-Za-z0-9_,]+) @ [0-9A-Fa-fx]+\] Format not on whitelist`)

// ProbeFormat runs ffprobe on path with InputArgs and reports the demuxer:
// format is ffprobe's format_name; refused is true when ffprobe refused the
// file because its demuxer is not in Demuxers (format is then the
// demuxer's name when ffprobe printed it) or reported a format_name
// outside Demuxers. err is set when ffprobe failed for any other reason
// (no such program, not a media file, a damaged file): the file is then
// neither known to be safe nor refused here; ffmpeg reads it later with
// the same InputArgs.
func ProbeFormat(ctx context.Context, run Runner, ffprobe, path string) (format string, refused bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	args := append([]string{"-v", "error", "-hide_banner"}, InputArgs()...)
	args = append(args, "-show_entries", "format=format_name", "-of", "default=noprint_wrappers=1:nokey=1", path)
	stdout, stderr, err := run(ctx, ffprobe, args...)
	if err != nil {
		if strings.Contains(stderr, "not on whitelist") {
			if m := notOnWhitelistRe.FindStringSubmatch(stderr); m != nil {
				return m[1], true, nil
			}
			return "", true, nil
		}
		msg := strings.TrimSpace(stderr)
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return "", false, fmt.Errorf("ffprobe: %w: %s", err, msg)
	}
	format = strings.TrimSpace(strings.SplitN(strings.TrimSpace(stdout), "\n", 2)[0])
	if format == "" {
		return "", false, fmt.Errorf("ffprobe printed no format_name")
	}
	return format, !FormatAllowed(format), nil
}

// Check inspects a stored upload at path: an empty file is a ProblemEmpty,
// an HTML page (SniffHTML) a ProblemHTML (test-media I6: refused when it
// arrives, not when prep fails on it), a text playlist (SniffPlaylist) a
// ProblemPlaylist; otherwise ffprobe (next to ffmpeg, FFprobeFor) runs with
// InputArgs, and a demuxer outside Demuxers is a ProblemFormat. When
// ffprobe cannot be found or cannot read the file, the file passes (nil,
// nil): prep reports an unreadable file with its own message, and reads it
// with InputArgs as well. err is only an I/O error reading path.
func Check(ctx context.Context, run Runner, ffmpeg, path string) (*Problem, error) {
	head, err := readHead(path)
	if err != nil {
		return nil, err
	}
	if len(head) == 0 {
		return &Problem{Kind: ProblemEmpty}, nil
	}
	if SniffHTML(head) {
		return &Problem{Kind: ProblemHTML}, nil
	}
	if kind := SniffPlaylist(head); kind != "" {
		return &Problem{Kind: ProblemPlaylist, Format: kind}, nil
	}
	if run == nil {
		return nil, nil
	}
	ffprobe, err := FFprobeFor(ffmpeg)
	if err != nil {
		return nil, nil
	}
	format, refused, err := ProbeFormat(ctx, run, ffprobe, path)
	if err != nil || !refused {
		return nil, nil
	}
	return &Problem{Kind: ProblemFormat, Format: format}, nil
}
