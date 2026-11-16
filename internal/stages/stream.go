// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"lectureflow/internal/db"
	lfmedia "lectureflow/internal/media"
	"lectureflow/internal/model"
	"lectureflow/internal/procutil"
	"lectureflow/internal/sched"
	"lectureflow/internal/state"
)

// The playback file of SPEC-v2 P8.1: stream.mp4 in the lecture directory,
// browser-playable MP4 (H.264 + AAC). prep remuxes it right after audio.wav;
// a source that must be transcoded gets it from the auxiliary task "stream",
// which prep queues and which runs in parallel with asr (stream-parallel,
// user decision 2026-10-27); the same task makes it for lectures prepared
// before v2 (queued by GET /media).
// Its state is kv stream:{lecture} (model.StreamState, lead ruling 7 of
// phase 2), which records outcomes only: while a stream job is queued or
// running the API shows "generating", and no kv entry means "pending".

// Stream states written to kv stream:{lecture} (api.StreamInfo.Status).
const (
	StreamReady     = "ready"      // stream.mp4 exists
	StreamAudioOnly = "audio_only" // the source has no video: the player runs in audio mode
	StreamNoSpace   = "no_space"   // appendix D6: under StreamMinFreeBytes free, not generated
	StreamFailed    = "failed"     // StreamState.Error is the JSON of a model.ErrorInfo
)

// StreamMinFreeBytes is the free space appendix D6 requires on the lecture
// directory's volume before a playback file is generated.
const StreamMinFreeBytes = 10 << 30

// Error codes of the playback file (appendix D6; P14.3 "errors.{code}").
// The ffmpeg output is the error's detail.
const (
	MediaCorrupt         = "media.corrupt"          // the file is incomplete or damaged
	MediaUnsupported     = "media.unsupported"      // a codec or format ffmpeg cannot handle
	MediaNoAudio         = "media.no_audio"         // the file has no audio track
	MediaFailed          = "media.failed"           // anything else ("处理这个文件时出错")
	MediaOriginalMissing = "media.original_missing" // the stream task found no original file (deleted after prep)
	// MediaFormatRejected (security review 2, M3): the media is a text
	// playlist (internal/media.SniffFile), or ffmpeg / ffprobe refused it
	// because its demuxer (or a protocol it needs) is outside the
	// whitelists of media.InputArgs: it would make ffmpeg read other
	// files. Params: format, the playlist kind ("m3u8", "ffconcat") or the
	// demuxer / protocol ("hls", "concat", "http"), "?" when ffmpeg did not
	// name it. Permanent: converting the file to a common format is the way
	// out.
	MediaFormatRejected = "media.format_rejected"
)

// mediaTools runs the external programs of prep and of the playback file
// and answers the free-space question of appendix D6. The stages use
// execMedia; tests inject fakes to check arguments, states and error
// classes without ffmpeg.
type mediaTools interface {
	// FFmpeg runs ffmpeg with args in dir. progress gets the fraction of
	// the input duration processed (from -progress pipe:1 and the
	// "Duration:" line), 1 at the end. It returns the tail of stderr; err
	// is the process error (*ffmpegStartError when it could not start).
	FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (stderr string, err error)
	// Probe returns ffprobe's JSON for media (-show_format -show_streams)
	// and the tail of its stderr.
	Probe(ctx context.Context, media string) (out []byte, stderr string, err error)
	// FreeSpace returns the bytes available to this user on dir's volume.
	FreeSpace(dir string) (int64, error)
	// H264Encoders checks the H.264 encoders of SPEC-v3 V7 (h264Candidates,
	// in that order) on this ffmpeg; cached per ffmpeg for the process.
	H264Encoders(ctx context.Context) ([]encoderCheck, error)
}

// ffmpegStartError: ffmpeg (or ffprobe) could not be started at all.
type ffmpegStartError struct {
	exe string
	err error
}

func (e *ffmpegStartError) Error() string {
	// The name without its directory, whichever separator the path uses
	// (a Windows path stays one element for filepath.Base on macOS).
	name := e.exe[strings.LastIndexAny(e.exe, `/\`)+1:]
	return fmt.Sprintf("无法启动 %s（%s）：%v", strings.TrimSuffix(name, filepath.Ext(name)), e.exe, e.err)
}
func (e *ffmpegStartError) Unwrap() error { return e.err }

// execMedia runs the real programs. ffprobe is looked for next to the
// configured ffmpeg (the official builds ship both in one directory), then
// in PATH.
type execMedia struct {
	ffmpeg string // resolved path
}

// tools returns the injected tools, or execMedia for the configured ffmpeg
// (error: ffmpeg not found).
func (d Deps) tools(injected mediaTools) (mediaTools, error) {
	if injected != nil {
		return injected, nil
	}
	ffmpeg, err := executable(d.Cfg.FFmpeg)
	if err != nil {
		return nil, err
	}
	return &execMedia{ffmpeg: ffmpeg}, nil
}

var durationRE = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)

func (m *execMedia) FFmpeg(ctx context.Context, dir string, args []string, progress func(float64)) (string, error) {
	cmd := exec.CommandContext(ctx, m.ffmpeg, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	// Tied: ffmpeg ends with LectureFlow, also when LectureFlow crashes or
	// is killed (test-chaos 1: an orphaned ffmpeg kept writing its .tmp).
	proc, err := procutil.Start(cmd, procutil.Tied)
	if err != nil {
		return "", &ffmpegStartError{exe: m.ffmpeg, err: err}
	}
	defer proc.Close()
	var (
		mu       sync.Mutex
		duration float64 // seconds, from stderr
	)
	tail := &tailBuffer{n: stderrTailBytes}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanLines(stderr, func(line string) {
			_, _ = io.WriteString(tail, line+"\n")
			if m := durationRE.FindStringSubmatch(line); m != nil {
				h, _ := strconv.Atoi(m[1])
				mi, _ := strconv.Atoi(m[2])
				sec, _ := strconv.ParseFloat(m[3], 64)
				mu.Lock()
				if duration == 0 {
					duration = float64(h*3600+mi*60) + sec
				}
				mu.Unlock()
			}
		})
	}()
	scanLines(stdout, func(line string) {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return
		}
		switch k {
		case "out_time_us":
			us, err := strconv.ParseFloat(v, 64)
			mu.Lock()
			total := duration
			mu.Unlock()
			if err == nil && total > 0 {
				progress(min(0.99, us/1e6/total))
			}
		case "progress":
			if v == "end" {
				progress(1)
			}
		}
	})
	wg.Wait()
	werr := cmd.Wait()
	return tail.String(), werr
}

// ffprobeFor returns the ffprobe next to ffmpeg ("ffmpeg" in the file name
// replaced by "ffprobe"), else the one in PATH.
func ffprobeFor(ffmpeg string) (string, error) {
	dir, base := filepath.Split(ffmpeg)
	ext := filepath.Ext(base)
	if name := strings.TrimSuffix(base, ext); strings.EqualFold(name, "ffmpeg") {
		p := filepath.Join(dir, "ffprobe"+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("ffprobe")
	if err != nil {
		return "", fmt.Errorf("找不到 ffprobe（应与 ffmpeg 位于同一目录：%s）：%w", ffmpeg, err)
	}
	return p, nil
}

// probeArgs are the ffprobe arguments of Probe: the input read with the
// whitelists of media.InputArgs (security review 2, M3), before the media.
func probeArgs(media string) []string {
	args := append([]string{"-v", "error"}, lfmedia.InputArgs()...)
	return append(args, "-print_format", "json", "-show_format", "-show_streams", media)
}

func (m *execMedia) Probe(ctx context.Context, media string) ([]byte, string, error) {
	ffprobe, err := ffprobeFor(m.ffmpeg)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, ffprobe, probeArgs(media)...)
	cmd.WaitDelay = 3 * time.Second
	tail := &tailBuffer{n: stderrTailBytes}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = tail
	err = procutil.Run(cmd, procutil.Tied) // ends with LectureFlow (test-chaos 1)
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, tail.String(), &ffmpegStartError{exe: ffprobe, err: err}
	}
	return out.Bytes(), tail.String(), err
}

func (m *execMedia) FreeSpace(dir string) (int64, error) { return diskFree(dir) }

// ---------- probing ----------

// mediaInfo is what the playback-file step needs to know about a media file.
type mediaInfo struct {
	Format       string       // ffprobe format_name, e.g. "mov,mp4,m4a,3gp,3g2,mj2"
	Video        *mediaStream // the first video stream that is not a cover picture; nil = none
	Audio        *mediaStream // the lecture's audio stream (see parseProbe); nil = none
	CreationTime *time.Time   // the container's (or a stream's) creation_time, UTC; nil = unknown
	// Duration is the declared duration in seconds of the lecture's audio
	// (declaredDuration); 0 = unknown or not to be trusted. prep compares
	// audio.wav with it (noteTruncation, test-media I9).
	Duration float64
}

type mediaStream struct {
	Index  int    // ffprobe stream index (the -map 0:{index} of ffmpeg)
	Codec  string // codec_name, e.g. h264, hevc, aac
	PixFmt string // pix_fmt of a video stream, e.g. yuv420p
}

// probeJSON is the part of ffprobe's JSON that is read.
type probeJSON struct {
	Streams []struct {
		Index       int               `json:"index"`
		CodecType   string            `json:"codec_type"`
		CodecName   string            `json:"codec_name"`
		PixFmt      string            `json:"pix_fmt"`
		Duration    string            `json:"duration"`
		Disposition map[string]int    `json:"disposition"`
		Tags        map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// parseProbe reads ffprobe's JSON.
//
// The lecture's audio track (test-media I3): with several audio tracks it
// is the first one marked default, else the first one. audio.wav (prep's
// audioArgs) and stream.mp4 (streamArgs) both map exactly this stream with
// -map 0:{index}; left to itself ffmpeg picked the track of audio.wav by
// its own rule (default flag, then the channel count) while stream.mp4 took
// the first, so transcription and playback could use different tracks.
func parseProbe(raw []byte) (mediaInfo, error) {
	var p probeJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return mediaInfo{}, fmt.Errorf("ffprobe 的输出无法解析：%w", err)
	}
	info := mediaInfo{Format: p.Format.FormatName}
	info.CreationTime = creationTime(p.Format.Tags)
	audioDefault := false // whether info.Audio is marked default
	audioDuration := ""   // the duration of info.Audio's stream
	for _, s := range p.Streams {
		switch {
		case s.CodecType == "video" && info.Video == nil && s.Disposition["attached_pic"] == 0:
			info.Video = &mediaStream{Index: s.Index, Codec: s.CodecName, PixFmt: s.PixFmt}
		case s.CodecType == "audio" && (info.Audio == nil || !audioDefault && s.Disposition["default"] != 0):
			info.Audio = &mediaStream{Index: s.Index, Codec: s.CodecName}
			audioDefault = s.Disposition["default"] != 0
			audioDuration = s.Duration
		}
		if info.CreationTime == nil {
			info.CreationTime = creationTime(s.Tags)
		}
	}
	info.Duration = declaredDuration(info.Format, audioDuration, p.Format.Duration)
	return info, nil
}

// indexedFormats are the ffprobe format names whose declared duration comes
// from the file's header or index (the MP4 moov, Matroska's segment info,
// the AVI and ASF headers, FLV's onMetaData, WAV's data size, FLAC's
// STREAMINFO): a file cut off after it was written still declares its full
// length there, which is what the truncation check of prep looks for.
// Raw bitstreams (aac/ADTS, mp3, ac3, …) are not in it: ffprobe estimates
// their duration from the bitrate of the first frames ("Estimating
// duration from bitrate, this may be inaccurate"), which for VBR audio is
// off by any amount (review R5: a complete 2-minute VBR ADTS file declared
// 54 min, a VBR MP3 without a Xing header 3:15). Neither is MPEG-TS or Ogg,
// whose duration ffprobe reads from the timestamps at the end of the file:
// a truncated one declares its truncated length anyway.
var indexedFormats = map[string]bool{
	"mov": true, "mp4": true, "matroska": true, "webm": true, "avi": true,
	"asf": true, "flv": true, "wav": true, "w64": true, "flac": true,
}

// declaredDuration is mediaInfo.Duration: the duration the container
// declares for the lecture's audio stream (streamDur), else for the whole
// file (formatDur), in seconds — or 0 when the format is not one of the
// indexedFormats, whose declared duration is not to be trusted. The audio
// stream's own duration comes first because the file's is the longest of
// its streams: a screen recording that went on after the microphone was
// stopped is no truncated file.
func declaredDuration(format, streamDur, formatDur string) float64 {
	indexed := false
	for _, f := range strings.Split(format, ",") {
		indexed = indexed || indexedFormats[strings.TrimSpace(f)]
	}
	if !indexed {
		return 0
	}
	for _, v := range []string{streamDur, formatDur} {
		if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

// creationTime reads the creation_time tag (any letter case). Values before
// 2000 are muxer defaults (the MP4 epoch 1904, the Unix epoch), not a
// recording time, and count as unknown.
func creationTime(tags map[string]string) *time.Time {
	for k, v := range tags {
		if !strings.EqualFold(k, "creation_time") {
			continue
		}
		v = strings.TrimSpace(v)
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, v); err == nil {
				if t.Year() < 2000 {
					return nil
				}
				t = t.UTC()
				return &t
			}
		}
	}
	return nil
}

// probeMedia runs ffprobe and parses it; errors are CodedErrors (D6).
func probeMedia(ctx context.Context, tools mediaTools, media string) (mediaInfo, error) {
	out, stderr, err := tools.Probe(ctx, media)
	if err != nil {
		if ctx.Err() != nil {
			return mediaInfo{}, ctx.Err()
		}
		return mediaInfo{}, mediaError(stderr, err)
	}
	info, err := parseProbe(out)
	if err != nil {
		return mediaInfo{}, model.Errorf(MediaFailed, nil, err)
	}
	return info, nil
}

// ---------- generating stream.mp4 ----------

// browserReady reports whether the media can be remuxed into stream.mp4
// without re-encoding: H.264 video in 8-bit 4:2:0 (what browsers decode;
// High 10 or 4:4:4 H.264 is not playable) and AAC audio, in any container.
// test-media I5: an MKV from OBS, a TS or an FLV with these codecs used to
// be transcoded in full only because of its container; -c copy -f mp4 gives
// the MP4 with H.264 + AAC of P8.1 in seconds. A remux out of another
// container that ffmpeg cannot finish falls back to the transcode, which
// the stream task does (makeStream, errRemuxNeedsTranscode).
func browserReady(info mediaInfo) bool {
	v, a := info.Video, info.Audio
	return v != nil && a != nil && v.Codec == "h264" &&
		(v.PixFmt == "" || v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p") && a.Codec == "aac"
}

// mp4Family reports whether ffprobe's format_name is the MP4 family
// ("mov,mp4,m4a,3gp,3g2,mj2").
func mp4Family(format string) bool {
	for _, f := range strings.Split(format, ",") {
		if strings.TrimSpace(f) == "mp4" {
			return true
		}
	}
	return false
}

// streamArgs are the ffmpeg arguments that write out from media: a remux
// (-c copy, P8.1) when enc is "", else a transcode to H.264 with encoder
// enc (SPEC-v3 V7, videoEncodeArgs: 8-bit 4:2:0, even dimensions) + AAC.
// Both map exactly the probed video and audio stream, use -threads 2
// (SPEC §17.3) and put the index first (+faststart) so seeking plays at
// once. The input is read with the whitelists of media.InputArgs (security
// review 2, M3), right before its "-i" (after it they would be output
// options and silently do nothing).
func streamArgs(info mediaInfo, media, out, enc string) []string {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-progress", "pipe:1", "-y", "-threads", "2"}
	args = append(args, lfmedia.InputArgs()...)
	args = append(args, "-i", media,
		"-map", "0:"+strconv.Itoa(info.Video.Index), "-map", "0:"+strconv.Itoa(info.Audio.Index))
	if enc == "" {
		args = append(args, "-c", "copy")
	} else {
		args = append(args, videoEncodeArgs(enc)...)
		args = append(args, "-c:a", "aac", "-b:a", "128k", "-threads", "2")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

// streamPlan tells how the playback file will be made, for weighting the
// progress of prep: no file, a quick remux, or a long transcode.
type streamPlan int

const (
	planNone streamPlan = iota
	planRemux
	planTranscode
)

func planOf(info mediaInfo) streamPlan {
	switch {
	case info.Video == nil || info.Audio == nil:
		return planNone
	case browserReady(info):
		return planRemux
	}
	return planTranscode
}

// makeStream makes dir/stream.mp4 from media (P8.1, appendix D6) and
// returns the state to record: audio_only (no video; a stale stream.mp4 is
// removed), no_space, failed with MediaNoH264Encoder (a transcode is needed
// and no encoder of SPEC-v3 V7 is usable; the player runs in audio mode,
// as with no_space), or ready. Failures are CodedErrors (media.*;
// MediaEncoderFailed when the encoders themselves failed); a cancelled ctx
// returns ctx.Err(). A file made only after an encoder failed is noted on
// the job (NotePrepEncoderRetried). The file is written as stream.mp4.tmp
// (another free .tmp name when that one is still held: tmpFor) and put in
// place with replaceFile, so a partial file is never served; a stream.mp4
// that another program has open (the player) is a prep.file_in_use error
// and stays as it was.
//
// This is the stream task's call: a failed remux out of a container other
// than MP4 falls back to the transcode right here. prep calls
// makeStreamInPrep instead.
func makeStream(ctx context.Context, tools mediaTools, dir, media string, info mediaInfo, progress func(float64)) (model.StreamState, error) {
	return makeStreamWith(ctx, tools, dir, media, info, progress, true)
}

// errRemuxNeedsTranscode is the error of makeStreamInPrep when the remux out
// of a container other than MP4 failed: the transcode it falls back to
// takes minutes and is left to the stream task, like any other transcode
// (stream-parallel), so that it never holds up prep and asr. It wraps the
// remux's own error.
var errRemuxNeedsTranscode = errors.New("remux failed, the playback file is to be transcoded by the stream task")

// makeStreamInPrep is makeStream for prep: it never transcodes. A failed
// remux out of a container other than MP4 returns errRemuxNeedsTranscode
// (wrapping the remux error), and prep queues the stream task; out of an MP4
// it stays the error it was.
func makeStreamInPrep(ctx context.Context, tools mediaTools, dir, media string, info mediaInfo, progress func(float64)) (model.StreamState, error) {
	return makeStreamWith(ctx, tools, dir, media, info, progress, false)
}

// makeStreamWith is makeStream; transcodeFallback tells whether a failed
// remux out of a container other than MP4 falls back to the transcode here
// (the stream task) or returns errRemuxNeedsTranscode (prep).
func makeStreamWith(ctx context.Context, tools mediaTools, dir, media string, info mediaInfo, progress func(float64), transcodeFallback bool) (model.StreamState, error) {
	out := filepath.Join(dir, model.StreamFile)
	if info.Video == nil {
		if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("stream: remove stale stream.mp4", "dir", dir, "err", err)
		}
		return model.StreamState{Status: StreamAudioOnly}, nil
	}
	if info.Audio == nil {
		return model.StreamState{}, sched.Permanent(model.Errorf(MediaNoAudio, nil, errors.New("媒体文件中没有音轨")))
	}
	free, err := tools.FreeSpace(dir)
	switch {
	case err != nil:
		slog.Warn("stream: free space unknown, generating anyway", "dir", dir, "err", err)
	case free < StreamMinFreeBytes:
		return model.StreamState{Status: StreamNoSpace}, nil
	}
	tmp, err := tmpFor(out)
	if err != nil {
		return model.StreamState{}, streamFileError(err)
	}
	remux := browserReady(info)
	encoders := []string{""} // "" = remux
	if !remux {
		checks, err := tools.H264Encoders(ctx)
		if ctx.Err() != nil {
			return model.StreamState{}, ctx.Err()
		}
		if err != nil {
			return model.StreamState{}, model.Errorf(MediaFailed, nil, fmt.Errorf("checking the H.264 encoders: %w", err))
		}
		if encoders = usableEncoders(checks); len(encoders) == 0 {
			slog.Warn("stream: no usable H.264 encoder, playback file not generated (audio mode)", "dir", dir, "checks", checks)
			return noEncoderState(checks), nil
		}
	}
	// A remux out of a container other than MP4 (test-media I5) depends on
	// what ffmpeg makes of that container's timestamps and stream headers:
	// when it fails, the transcode of before is tried rather than leaving
	// the lecture without a playback file. The encoders are only checked
	// then.
	remuxFallback := remux && !mp4Family(info.Format)
	// A transcode falls back to the next usable encoder when one fails for
	// a reason that is not the file's (a hardware encoder can pass the test
	// and still refuse a size, run out of sessions or glitch under load).
	// When the encoder itself failed (transcodeError), a hardware encoder
	// is run once more first. Only after every usable encoder failed is
	// there a conclusion (transcodeFailure). The progress never goes back.
	var done float64
	mono := func(p float64) {
		if p > done {
			done = p
			progress(p)
		}
	}
	var (
		stderr string
		tries  []encoderTry // failed transcode attempts
		used   = -1         // index of the encoder that made the file
	)
encoders:
	for i := 0; i < len(encoders); i++ { // remuxFallback may append the encoders
		enc := encoders[i]
		for attempt := 1; attempt <= encoderAttempts(enc); attempt++ {
			os.Remove(tmp)
			stderr, err = tools.FFmpeg(ctx, dir, streamArgs(info, media, tmp, enc), mono)
			if ctx.Err() != nil {
				os.Remove(tmp)
				return model.StreamState{}, ctx.Err()
			}
			if err == nil {
				used = i
				break encoders
			}
			os.Remove(tmp)
			if enc == "" {
				merr := mediaError(stderr, err)
				if !remuxFallback {
					return model.StreamState{}, merr
				}
				if !transcodeFallback {
					return model.StreamState{}, fmt.Errorf("%w: %w", errRemuxNeedsTranscode, merr)
				}
				checks, cerr := tools.H264Encoders(ctx)
				if ctx.Err() != nil {
					return model.StreamState{}, ctx.Err()
				}
				usable := usableEncoders(checks)
				if cerr != nil || len(usable) == 0 {
					slog.Warn("stream: remux failed and no H.264 encoder to transcode with", "dir", dir, "checks", checks, "err", cerr)
					return model.StreamState{}, merr
				}
				slog.Warn("stream: remux failed, transcoding instead", "dir", dir, "format", info.Format, "err", merr)
				encoders = append(encoders, usable...)
				break
			}
			merr, fault := transcodeError(stderr, err, enc)
			if sched.IsPermanent(merr) {
				return model.StreamState{}, merr // the file's own problem: no encoder helps
			}
			tries = append(tries, encoderTry{enc: enc, attempt: attempt, err: merr, fault: fault})
			if fault && attempt < encoderAttempts(enc) {
				slog.Warn("stream: the encoder failed, trying it once more", "dir", dir, "encoder", enc, "err", merr)
				continue
			}
			if i+1 < len(encoders) {
				slog.Warn("stream: encoder failed, trying the next one", "dir", dir, "encoder", enc, "next", encoders[i+1], "err", merr)
			}
			break
		}
	}
	if used < 0 {
		return model.StreamState{}, transcodeFailure(tries)
	}
	if enc := encoders[used]; enc != "" {
		slog.Info("stream: transcoded", "dir", dir, "encoder", enc, "failed_attempts", len(tries))
		if len(tries) > 0 {
			noteEncoderRetried(ctx, tryNames(tries), enc)
		}
	}
	fi, err := os.Stat(tmp)
	if err != nil || fi.Size() == 0 {
		os.Remove(tmp)
		return model.StreamState{}, model.Errorf(MediaFailed, nil, fmt.Errorf("ffmpeg 退出码为 0 但没有写出 %s：%v。ffmpeg 输出：%s", tmp, err, stderr))
	}
	if err := replaceFile(tmp, out); err != nil {
		os.Remove(tmp)
		return model.StreamState{}, streamFileError(err)
	}
	progress(1)
	return model.StreamState{Status: StreamReady, Transcoded: encoders[used] != "", SizeBytes: fi.Size()}, nil
}

// streamFileError is the error of a stream.mp4 that could not be written
// or put in place: prep.file_in_use when another program holds it,
// media.failed otherwise.
func streamFileError(err error) error {
	if _, ok := asFileInUse(err); ok {
		return inUseCoded(CodePrepFileInUse, err)
	}
	return model.Errorf(MediaFailed, nil, err)
}

// unsupportedMarkers are fatal ffmpeg/ffprobe messages about a codec or
// format that cannot be handled.
var unsupportedMarkers = []string{
	"Unknown decoder", "Decoder (codec", "Unsupported codec", "unsupported codec",
	"codec not currently supported in container", "Could not find codec parameters",
}

// outputFailureMarkers are ffmpeg messages that a write failed; with one of
// them the failure is not blamed on the input file, whatever warnings about
// the input's streams precede it (a full disk is not "格式不支持").
var outputFailureMarkers = []string{"No space left on device"}

// mediaError classifies a failed ffmpeg/ffprobe run (appendix D6) into a
// CodedError whose wrapped error carries the raw output ("查看详情").
// Problems of the file itself are permanent. prep's audio conversion and
// the playback file share it (lead ruling at the phase-2 gate).
func mediaError(stderr string, err error) error {
	var se *ffmpegStartError
	if errors.As(err, &se) {
		return model.Errorf(MediaFailed, nil, err)
	}
	cause := errors.New(ffmpegDetail(stderr, err))
	if what, refused := lfmedia.Refused(stderr); refused {
		if what == "" {
			what = "?"
		}
		return sched.Permanent(model.Errorf(MediaFormatRejected, map[string]any{"format": what}, cause))
	}
	if outputFailed(stderr) {
		return model.Errorf(MediaFailed, nil, cause)
	}
	switch classifyFFmpeg(stderr) {
	case ffmpegNoAudio:
		return sched.Permanent(model.Errorf(MediaNoAudio, nil, cause))
	case ffmpegCorrupt:
		return sched.Permanent(model.Errorf(MediaCorrupt, nil, cause))
	}
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "Error opening output") {
			continue
		}
		for _, m := range unsupportedMarkers {
			if strings.Contains(line, m) {
				return sched.Permanent(model.Errorf(MediaUnsupported, nil, cause))
			}
		}
	}
	return model.Errorf(MediaFailed, nil, cause)
}

// playlistRefusal is the media.format_rejected error of a media file that
// starts like a text playlist (media.SniffFile: HLS, ffconcat, PLS, DASH
// and other manifests), nil otherwise — also when the file cannot be read
// (ffmpeg then reports that).
func playlistRefusal(path string) error {
	kind, err := lfmedia.SniffFile(path)
	if err != nil || kind == "" {
		return nil
	}
	return sched.Permanent(model.Errorf(MediaFormatRejected, map[string]any{"format": kind},
		fmt.Errorf("媒体文件是播放列表或清单（%s），不是录音录像本身；ffmpeg 会读取其中列出的其他文件，为安全起见不处理：%s", kind, path)))
}

// isFormatRejected reports whether err is a media.format_rejected error.
func isFormatRejected(err error) bool {
	var ce *model.CodedError
	return errors.As(err, &ce) && ce.Info.Code == MediaFormatRejected
}

// ffmpegDetail is the "查看详情" of a failed ffmpeg/ffprobe run: the exit
// code (or the process error) and the tail of its output.
func ffmpegDetail(stderr string, err error) string {
	if stderr == "" {
		return err.Error()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("退出码 %d：%s", ee.ExitCode(), stderr)
	}
	return fmt.Sprintf("%v：%s", err, stderr)
}

// outputFailed reports whether ffmpeg's output says a write failed
// (outputFailureMarkers).
func outputFailed(stderr string) bool {
	for _, m := range outputFailureMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// errorInfoJSON is the language-neutral form of err for StreamState.Error:
// the JSON of a model.ErrorInfo, Detail = the wrapped error's text (the
// same convention as job.error of a CodedError).
func errorInfoJSON(err error) string {
	info := model.ErrorInfo{Code: MediaFailed, Detail: err.Error()}
	var ce *model.CodedError
	if errors.As(err, &ce) {
		info.Code, info.Params = ce.Info.Code, ce.Info.Params
		if ce.Err != nil {
			info.Detail = ce.Err.Error()
		} else {
			info.Detail = ""
		}
	}
	b, _ := json.Marshal(info)
	return string(b)
}

// setStreamState records the playback file's state (kv stream:{lecture})
// and tells the UI. A failure to record is logged: the file itself is what
// GET /media serves.
func (d Deps) setStreamState(ctx context.Context, lectureID string, st model.StreamState) {
	st.UpdatedAt = d.now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(st)
	if err := d.Store.SetKV(context.WithoutCancel(ctx), model.KVStreamPrefix+lectureID, string(b)); err != nil {
		slog.Warn("stream: record state", "lecture", lectureID, "err", err)
		return
	}
	d.events().Changed("lecture", lectureID)
}

// StreamStateOf reads kv stream:{lecture}; ok is false when nothing was
// recorded (pending, or a lecture prepared before v2).
func StreamStateOf(ctx context.Context, st db.Store, lectureID string) (model.StreamState, bool, error) {
	raw, ok, err := st.GetKV(ctx, model.KVStreamPrefix+lectureID)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return model.StreamState{}, false, err
	}
	var s model.StreamState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return model.StreamState{}, false, fmt.Errorf("kv %s%s: %w", model.KVStreamPrefix, lectureID, err)
	}
	return s, true, nil
}

// ---------- the auxiliary task ----------

// streamStage is the auxiliary task "stream" (P8.1, lead ruling 1 of phase
// 2): it makes stream.mp4 from the lecture's original media by the rules of
// prep. prep queues it for a source that must be transcoded
// (stream-parallel: the transcode runs in the background, in parallel with
// asr, instead of holding up prep); GET /media queues it for a lecture
// prepared before v2. It needs prep done (state auxDeps) and one slot of
// the CPU pool, like prep; it neither waits for nor holds a transcription
// device, whichever H.264 encoder it uses (NOTES stream-parallel). A
// failure is not retried automatically.
type streamStage struct {
	d     Deps
	media mediaTools // nil = execMedia
}

func (s *streamStage) Name() string                 { return model.StageStream }
func (s *streamStage) Resource() sched.ResourceKind { return sched.ResourceCPU }

// InputHash: an auxiliary task has none.
func (s *streamStage) InputHash(context.Context, string) (string, error) { return "", nil }

func (s *streamStage) Run(ctx context.Context, lectureID string, progress func(float64)) error {
	d := s.d
	l, err := d.Store.GetLecture(ctx, lectureID)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(d.lectureDir(l))
	if err != nil {
		return err
	}
	media := ""
	if !state.StoredPathUnsafe(state.PathLectureMedia, lectureID, l.MediaPath) {
		media = resolvePath(l.MediaPath, dir) // a path naming another computer is never opened (M3)
	}
	fail := func(err error) error {
		d.setStreamState(ctx, lectureID, model.StreamState{Status: StreamFailed, Error: errorInfoJSON(err)})
		if !d.keepOriginalMedia() {
			// the original stays for a retry; the note says why (as prep's did)
			removeOriginal(ctx, d, lectureID, media, dir, StreamFailed)
		}
		return err
	}
	if fi, err := os.Stat(media); media == "" || err != nil || fi.IsDir() {
		err := sched.Permanent(model.Errorf(MediaOriginalMissing, nil,
			fmt.Errorf("讲座的原始媒体文件不存在（%s），无法生成播放文件；可能已按 keep_original_media = false 在预处理后删除", l.MediaPath)))
		d.setStreamState(ctx, lectureID, model.StreamState{Status: StreamFailed, Error: errorInfoJSON(err)})
		return err
	}
	if err := playlistRefusal(media); err != nil {
		return fail(err)
	}
	tools, err := d.tools(s.media)
	if err != nil {
		return fail(sched.Permanent(model.Errorf(MediaFailed, nil, fmt.Errorf("找不到 ffmpeg（%s）：%w", d.Cfg.FFmpeg, err))))
	}
	info, err := probeMedia(ctx, tools, media)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fail(err)
	}
	st, err := makeStream(ctx, tools, dir, media, info, progress)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fail(err)
	}
	d.setStreamState(ctx, lectureID, st)
	// The original that prep left for this task (a transcode, or a playback
	// file prep could not make; lead ruling on keep_original_media) goes now
	// that the playback file exists — unless prep is being run again, which
	// reads it (a prep rerun queued or started while this task ran): then it
	// stays, as with keep_original_media = true, rather than leaving the
	// lecture without audio.wav and without its source.
	if !d.keepOriginalMedia() {
		if prepSettled(ctx, d, lectureID) {
			removeOriginal(ctx, d, lectureID, media, dir, st.Status)
		} else {
			slog.Info("keep_original_media=false: original kept, prep runs again", "lecture", lectureID)
		}
	}
	progress(1)
	return nil
}

// prepSettled reports whether the lecture's current prep job is done (no
// rerun waiting or running that still needs the original media).
func prepSettled(ctx context.Context, d Deps, lectureID string) bool {
	j, err := d.Store.GetCurrentJob(context.WithoutCancel(ctx), model.ScopeLecture, lectureID, model.StagePrep)
	return err == nil && j.Status == model.StatusDone
}
