// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package api

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"

	"lectureflow/internal/media"
)

// Lecture media checks (security review 2, M3; internal/media). ffmpeg and
// ffprobe follow playlists: an HLS playlist or ffconcat script saved as a
// lecture's media (or a second camera angle) would make them read files
// outside the lecture directory, and network shares by UNC path (NTLM). So
// a lecture's media and an angle are accepted only
//   - with a media file extension (media.Exts, the front end's
//     MEDIA_EXT_LIST), checked before anything is written;
//   - when the stored file does not start like a text playlist, and
//     ffprobe, run with the demuxer whitelist (media.InputArgs), does not
//     report a demuxer outside media.Demuxers.
// An empty file and an HTML page (a login or error page saved under the
// recording's name) are refused too, with their own codes, instead of
// failing later in prep (test-media I6). A file ffprobe cannot read at all
// still passes: prep reports it with its own message, as before.

// Codes of the media checks (part of the API's catalog; init adds them).
const (
	codeUploadNotMedia    = "upload.not_media"
	codeUploadPlaylist    = "upload.media_playlist"
	codeUploadMediaFormat = "upload.media_format"
	codeUploadEmpty       = "upload.media_empty"
	codeUploadHTML        = "upload.media_html"
	codeImportNetworkPath = "data_import.network_path"

	// mediaUploadBase is the name a replacement media file is stored under
	// until it is checked (replaceMedia).
	mediaUploadBase = ".media-upload"
)

var uploadMediaCodes = map[string]errDef{
	codeUploadNotMedia:    {"“{name}”不是支持的音频或视频文件。支持的扩展名：{exts}", []string{"name", "exts"}},
	codeUploadPlaylist:    {"“{name}”是播放列表或清单（{kind}），里面只是指向其他文件的路径，不是录音录像本身。为安全起见不接受这类文件，请上传实际的音频或视频文件", []string{"name", "kind"}},
	codeUploadMediaFormat: {"“{name}”的实际格式（{format}）不是支持的音视频格式，可能会读取其他文件。为安全起见不接受这个文件，请先转换为 MP4、MKV、MP3 等常见格式", []string{"name", "format"}},
	codeUploadEmpty:       {"“{name}”是空文件（0 字节），不是录音或录像。可能是下载没有完成，请重新下载后再导入", []string{"name"}},
	codeUploadHTML:        {"“{name}”其实是一个网页，不是录音或录像。下载到的往往是登录页或错误页：请在浏览器里登录后重新下载，再导入", []string{"name"}},
	codeImportNetworkPath: {"要导入的资料库里有 {count} 个文件路径指向网络共享或其他电脑（例如 {path}），不在这个数据目录之内。为了不让 LectureFlow 自动连接其他电脑，不能导入。如果这个数据目录本身就在网络共享上，请用当初使用的同一路径选择它", []string{"count", "path"}},
}

func init() {
	for code, def := range uploadMediaCodes {
		if _, dup := errorCatalog[code]; dup {
			panic("media check code defined twice: " + code)
		}
		errorCatalog[code] = def
	}
}

// acceptMediaName is receiveUpload's accept for a lecture's media and a
// second angle: the file name must have a media extension (any case).
func acceptMediaName(name string) error {
	if media.AllowedName(name) {
		return nil
	}
	return apiErr(http.StatusBadRequest, codeUploadNotMedia, map[string]any{"name": name, "exts": media.ExtList()})
}

// checkMediaContent refuses the stored file at path (name: the user's file
// name, for the message) when it is a text playlist or ffprobe reports a
// demuxer outside the whitelist (media.Check, with s.runCmd and the
// ffprobe next to the configured ffmpeg). The caller removes the file.
func (s *Server) checkMediaContent(ctx context.Context, path, name string) error {
	// absolute: ffmpeg must see a plain file path (a development data_dir
	// may be relative)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	pr, err := media.Check(ctx, media.Runner(s.runCmd), s.cfg.FFmpeg, path)
	if err != nil {
		return err
	}
	if pr == nil {
		return nil
	}
	slog.Warn("media refused", "name", name, "kind", pr.Kind, "format", pr.Format)
	switch pr.Kind {
	case media.ProblemEmpty:
		return apiErr(http.StatusBadRequest, codeUploadEmpty, map[string]any{"name": name})
	case media.ProblemHTML:
		return apiErr(http.StatusBadRequest, codeUploadHTML, map[string]any{"name": name})
	case media.ProblemPlaylist:
		return apiErr(http.StatusBadRequest, codeUploadPlaylist, map[string]any{"name": name, "kind": pr.Format})
	}
	format := pr.Format
	if format == "" {
		format = "?"
	}
	return apiErr(http.StatusBadRequest, codeUploadMediaFormat, map[string]any{"name": name, "format": format})
}
