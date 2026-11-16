// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What a URL is, from its Content-Type, extension and how the page loaded
// it. Pure.

import { parseHttpUrl, extensionOfPath } from './url.js';

export const KIND_AUDIO = 'audio';
export const KIND_VIDEO = 'video';
export const KIND_UNKNOWN = 'unknown';
export const FORMAT_FILE = 'file';
export const FORMAT_HLS = 'hls';
export const FORMAT_DASH = 'dash';

// Classes a candidate URL can fall into.
export const C_HLS = 'hls';
export const C_DASH = 'dash';
export const C_FILE = 'file';
export const C_SEGMENT = 'segment'; // a piece of a stream, never a track
export const C_OTHER = 'other'; // not media

const HLS_TYPES = new Set([
  'application/vnd.apple.mpegurl',
  'application/x-mpegurl',
  'application/mpegurl',
  'audio/mpegurl',
  'audio/x-mpegurl',
  'video/x-mpegurl',
]);
const DASH_TYPES = new Set(['application/dash+xml', 'video/vnd.mpeg.dash.mpd']);
const SEGMENT_TYPES = new Set([
  'video/mp2t',
  'video/iso.segment',
  'audio/iso.segment',
  'application/vnd.ms-sstr+xml', // Smooth Streaming manifest: not supported, not a track
]);
const AMBIGUOUS_TYPES = new Set([
  '',
  'application/octet-stream',
  'binary/octet-stream',
  'application/binary',
  'application/mp4', // fragments as often as whole files
]);

const HLS_EXT = new Set(['m3u8', 'm3u']);
const DASH_EXT = new Set(['mpd']);
const AUDIO_EXT = new Set(['m4a', 'mp3', 'aac', 'oga', 'ogg', 'opus', 'wav', 'flac', 'weba', 'wma']);
const VIDEO_EXT = new Set(['mp4', 'm4v', 'webm', 'mov', 'mkv', 'ogv', 'avi', 'wmv', 'flv']);
const SEGMENT_EXT = new Set(['ts', 'm4s', 'm4f', 'cmfv', 'cmfa', 'cmft', 'fmp4', 'm2ts', 'mts', 'vtt', 'webvtt']);

const AUDIO_CODEC = /^(mp4a|ac-3|ec-3|ac-4|opus|flac|mp3|vorbis|alac|dtsc|dtse|dtsh|dtsl)/i;
const VIDEO_CODEC = /^(avc1|avc3|hvc1|hev1|av01|vp09|vp8|vp9|mp4v|dvh1|dvhe|dva1|dvav|theora)/i;

/** "video/MP4; codecs=..." → "video/mp4". */
export function mediaType(contentType) {
  return String(contentType || '').split(';')[0].trim().toLowerCase();
}

/** Kind of a codecs list ("avc1.4d401f,mp4a.40.2" → video). */
export function kindOfCodecs(codecs) {
  const list = String(codecs || '').split(',').map((c) => c.trim()).filter(Boolean);
  if (list.length === 0) return KIND_UNKNOWN;
  if (list.some((c) => VIDEO_CODEC.test(c))) return KIND_VIDEO;
  if (list.every((c) => AUDIO_CODEC.test(c))) return KIND_AUDIO;
  return KIND_UNKNOWN;
}

/**
 * What a path's extension names, as classifyUrl reads it when nothing else
 * (Content-Type, a segment extension, a format in the path) says more:
 * C_HLS, C_DASH, C_FILE or ''. ext: lower case, as extensionOfPath gives it.
 */
export function extensionClass(ext) {
  if (HLS_EXT.has(ext)) return C_HLS;
  if (DASH_EXT.has(ext)) return C_DASH;
  if (AUDIO_EXT.has(ext) || VIDEO_EXT.has(ext)) return C_FILE;
  return '';
}

/**
 * Azure Media Services and similar servers name the format in the path:
 * ".../x.ism/manifest(format=m3u8-aapl)". Returns "hls", "dash" or "",
 * or C_SEGMENT for Smooth Streaming (unsupported).
 */
export function formatInPath(pathname) {
  const p = decodeURIComponentSafe(pathname).toLowerCase();
  if (/\.isml?\/manifest\(format=m3u8/.test(p)) return C_HLS;
  if (/\.isml?\/manifest\(format=mpd/.test(p)) return C_DASH;
  if (/\.isml?\/manifest$/.test(p)) return C_SEGMENT; // Smooth Streaming: unsupported
  return '';
}

function decodeURIComponentSafe(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

/**
 * Classifies one URL. Hints (all optional): contentType (from a response),
 * tag ("video" | "audio" | "source"), initiatorType (resource timing),
 * status (HTTP status from webRequest).
 *
 * Returns { cls, kind }: cls is one of C_HLS, C_DASH, C_FILE, C_SEGMENT,
 * C_OTHER; kind is audio | video | unknown for files.
 */
export function classifyUrl(raw, hints = {}) {
  const u = parseHttpUrl(raw);
  if (!u) return { cls: C_OTHER, kind: KIND_UNKNOWN };
  if (typeof hints.status === 'number' && hints.status >= 400) return { cls: C_OTHER, kind: KIND_UNKNOWN };

  const ext = extensionOfPath(u.pathname);
  const type = mediaType(hints.contentType);
  const inPath = formatInPath(u.pathname);

  if (HLS_TYPES.has(type)) return { cls: C_HLS, kind: KIND_UNKNOWN };
  if (DASH_TYPES.has(type)) return { cls: C_DASH, kind: KIND_UNKNOWN };
  if (SEGMENT_TYPES.has(type)) return { cls: C_SEGMENT, kind: KIND_UNKNOWN };
  if (inPath) return { cls: inPath, kind: KIND_UNKNOWN };
  if (SEGMENT_EXT.has(ext)) return { cls: C_SEGMENT, kind: KIND_UNKNOWN };

  if (type.startsWith('audio/')) return { cls: C_FILE, kind: KIND_AUDIO };
  if (type.startsWith('video/')) {
    const kind = AUDIO_EXT.has(ext) ? KIND_AUDIO : KIND_VIDEO;
    return { cls: C_FILE, kind };
  }

  if (type && !AMBIGUOUS_TYPES.has(type)) {
    // A specific non-media type (HTML error page, JSON, image, script):
    // the extension is not trusted over the server.
    return { cls: C_OTHER, kind: KIND_UNKNOWN };
  }

  if (HLS_EXT.has(ext)) return { cls: C_HLS, kind: KIND_UNKNOWN };
  if (DASH_EXT.has(ext)) return { cls: C_DASH, kind: KIND_UNKNOWN };
  if (AUDIO_EXT.has(ext)) return { cls: C_FILE, kind: KIND_AUDIO };
  if (VIDEO_EXT.has(ext)) return { cls: C_FILE, kind: KIND_VIDEO };

  if (hints.tag === 'audio') return { cls: C_FILE, kind: KIND_AUDIO };
  if (hints.tag === 'video' || hints.tag === 'source') return { cls: C_FILE, kind: KIND_UNKNOWN };
  if (hints.initiatorType === 'audio') return { cls: C_FILE, kind: KIND_AUDIO };
  if (hints.initiatorType === 'video') return { cls: C_FILE, kind: KIND_UNKNOWN };
  if (hints.resourceType === 'media') {
    // webRequest "media" with an ambiguous type: a media element asked for it.
    return { cls: C_FILE, kind: KIND_UNKNOWN };
  }
  return { cls: C_OTHER, kind: KIND_UNKNOWN };
}
