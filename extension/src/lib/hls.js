// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// HLS playlist parsing (RFC 8216) and splitting a master playlist into
// tracks (SPEC-ext 通用轨道探测: every #EXT-X-STREAM-INF together is one
// video track; every independent #EXT-X-MEDIA:TYPE=AUDIO rendition is an
// audio track). Pure.

import { kindOfCodecs, KIND_AUDIO, KIND_VIDEO, KIND_UNKNOWN, FORMAT_HLS } from './classify.js';
import { urlKey, extensionOfPath, resolveRef, resolveCost } from './url.js';

/**
 * Parses an attribute list: KEY=VALUE pairs separated by commas, where a
 * value is a quoted string (may contain commas), a hex number or a plain
 * token. Returns a Map of upper-case keys to { value, quoted }.
 */
export function parseAttributeList(text) {
  const attrs = new Map();
  let i = 0;
  const s = String(text);
  while (i < s.length) {
    while (i < s.length && (s[i] === ' ' || s[i] === ',')) i++;
    const eq = s.indexOf('=', i);
    if (eq < 0) break;
    const key = s.slice(i, eq).trim().toUpperCase();
    i = eq + 1;
    let value = '';
    let quoted = false;
    if (s[i] === '"') {
      quoted = true;
      const end = s.indexOf('"', i + 1);
      if (end < 0) {
        value = s.slice(i + 1);
        i = s.length;
      } else {
        value = s.slice(i + 1, end);
        i = end + 1;
      }
    } else {
      const end = s.indexOf(',', i);
      value = (end < 0 ? s.slice(i) : s.slice(i, end)).trim();
      i = end < 0 ? s.length : end;
    }
    if (key) attrs.set(key, { value, quoted });
  }
  return attrs;
}

const attr = (m, k) => (m.has(k) ? m.get(k).value : undefined);

// An attribute value that ends up in a track, or undefined when it is
// longer than HLS_LIMITS.attrLength: such a value is treated as absent, not
// cut, so that no made-up codec list, name or language looks real (and the
// background does not keep it; SEC28 中-1).
const keptAttr = (m, k) => {
  const v = attr(m, k);
  return v !== undefined && v.length <= HLS_LIMITS.attrLength ? v : undefined;
};

function num(v) {
  if (v === undefined || v === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

// A page's playlist is read up to MAX_MANIFEST_BYTES (2 MiB, lib/bgfetch.js),
// but what it resolves to can be far larger: every address carries the
// playlist's own address as its base (SEC23c). So each playlist gives at
// most this many variants, renditions, segments and #EXT-X-MAP addresses,
// no address (and no base) longer than urlLength, and urlBytes characters
// of addresses in all; past a limit the rest is left out, quietly, and not
// resolved at all (SEC25 中-2). For the base, what counts is the part
// relative addresses read: its path without the query (url.js resolveRef,
// RFC 3986 §5.2.2), so a long signed query costs nothing (SEC25 中-4).
export const HLS_LIMITS = Object.freeze({
  urlLength: 4096, // the playlist's address as a base; any address it gives
  variants: 256, // #EXT-X-STREAM-INF of one master (real ladders: a few to a few dozen)
  renditions: 256, // #EXT-X-MEDIA of one master (audio / subtitle languages × groups)
  segments: 30000, // of one media playlist (4 h in 2 s segments is 7,200)
  initUris: 256, // #EXT-X-MAP of one media playlist (one per discontinuity at most)
  urlBytes: 16 * 1024 * 1024, // characters one playlist may resolve (ASCII: bytes; 4 h of 2 s segments signed with 1.3 KB is ~9.4 MB); each attempt costs its longest of address, base read, result
  attrLength: 256, // an attribute value a track keeps (CODECS, NAME, LANGUAGE, GROUP-ID, CHANNELS; AUDIO and VIDEO with them); real ones are under 100
});

// Key systems that mean DRM, as opposed to plain AES-128 with a key URL.
const DRM_KEYFORMAT = /widevine|playready|streamingkeydelivery|fairplay|com\.microsoft|urn:uuid:(edef8ba9|9a04f079|94ce86fb)/i;

function isDrmKey(m) {
  const method = (attr(m, 'METHOD') || '').toUpperCase();
  if (method === 'NONE' || method === '') return false;
  if (method.startsWith('SAMPLE-AES')) return true;
  const fmt = attr(m, 'KEYFORMAT') || '';
  return DRM_KEYFORMAT.test(fmt);
}

/**
 * Splits text into lines (CRLF, LF or CR), dropping a UTF-8 BOM.
 */
export function playlistLines(text) {
  return String(text).replace(/^\ufeff/, '').split(/\r\n|\n|\r/);
}

/**
 * Parses a playlist. Relative URIs are resolved against baseUrl.
 *
 * Returns {
 *   type: 'master' | 'media' | 'invalid',
 *   variants: [{ uri, bandwidth, averageBandwidth, width, height, codecs, frameRate, audio, video }],
 *   renditions: [{ type, groupId, name, language, uri, isDefault, channels }],
 *   drm: boolean,             // SAMPLE-AES or a DRM KEYFORMAT anywhere
 *   aesKeys: number,          // plain AES-128 keys (not DRM)
 *   segments: [uri],          // media playlist only
 *   initUris: [uri],          // #EXT-X-MAP URIs
 *   durationSec: number|null, // media playlist only: sum of #EXTINF
 *   endList: boolean,
 *   playlistType: string,
 * }
 */
export function parseHls(text, baseUrl, opts = {}) {
  const out = {
    type: 'invalid',
    variants: [],
    renditions: [],
    drm: false,
    aesKeys: 0,
    segments: [],
    initUris: [],
    durationSec: null,
    endList: false,
    playlistType: '',
  };
  const L = HLS_LIMITS;
  const base = typeof baseUrl === 'string' ? baseUrl : undefined;
  let bytesLeft = L.urlBytes;
  // opts.budget: { left } shared by one detection (detect.js). Segment and
  // #EXT-X-MAP addresses, the bulk, also spend it (at most
  // min(urlBytes, budget.left) here); variants and renditions, which make
  // the tracks and are few, only spend the playlist's own budget.
  const shared = opts && opts.budget;
  const sharedStart = shared ? Math.max(0, Math.min(L.urlBytes, shared.left)) : Infinity;
  let sharedLeft = sharedStart;
  // an address, or null when unusable or past the byte budget. Every
  // attempt is charged (url.js resolveCost), refused ones too, and once the
  // budget is spent nothing more is resolved (SEC25 中-2).
  const take = (uri, bulk = false) => {
    if (bytesLeft <= 0 || (bulk && sharedLeft <= 0) || !uri || uri.length > L.urlLength) return null;
    const u = resolveRef(uri, base, L.urlLength);
    const r = u ? urlKey(u.href) : null;
    const cost = resolveCost(uri, base, L.urlLength, r);
    if (cost > bytesLeft || (bulk && cost > sharedLeft)) {
      if (cost > bytesLeft) bytesLeft = 0;
      if (bulk) sharedLeft = 0;
      return null;
    }
    bytesLeft -= cost;
    if (bulk) sharedLeft -= cost;
    return r && r.length <= L.urlLength ? r : null;
  };
  const lines = playlistLines(text);
  let first = 0;
  while (first < lines.length && lines[first].trim() === '') first++;
  if (first >= lines.length || !lines[first].trim().startsWith('#EXTM3U')) return out;

  let pendingVariant = null;
  let skipUri = false; // the URI line of a variant past the limit
  let pendingSegment = false;
  let duration = 0;
  let sawExtinf = false;
  let isMaster = false;

  for (let n = first + 1; n < lines.length; n++) {
    const line = lines[n].trim();
    if (line === '') continue;
    if (line.startsWith('#')) {
      if (!line.startsWith('#EXT')) continue; // comment
      const colon = line.indexOf(':');
      const tag = colon < 0 ? line : line.slice(0, colon);
      const value = colon < 0 ? '' : line.slice(colon + 1);
      switch (tag) {
        case '#EXT-X-STREAM-INF': {
          isMaster = true;
          if (out.variants.length >= L.variants) {
            pendingVariant = null;
            skipUri = true;
            break;
          }
          const m = parseAttributeList(value);
          const res = /^(\d+)x(\d+)$/i.exec(attr(m, 'RESOLUTION') || '');
          pendingVariant = {
            uri: null,
            bandwidth: num(attr(m, 'BANDWIDTH')),
            averageBandwidth: num(attr(m, 'AVERAGE-BANDWIDTH')),
            width: res ? Number(res[1]) : null,
            height: res ? Number(res[2]) : null,
            codecs: keptAttr(m, 'CODECS') || '',
            frameRate: num(attr(m, 'FRAME-RATE')),
            audio: keptAttr(m, 'AUDIO') || '',
            video: keptAttr(m, 'VIDEO') || '',
          };
          break;
        }
        case '#EXT-X-MEDIA': {
          isMaster = true;
          if (out.renditions.length >= L.renditions) break;
          const m = parseAttributeList(value);
          const uriRaw = attr(m, 'URI');
          out.renditions.push({
            type: (attr(m, 'TYPE') || '').toUpperCase(),
            groupId: keptAttr(m, 'GROUP-ID') || '',
            name: keptAttr(m, 'NAME') || '',
            language: keptAttr(m, 'LANGUAGE') || '',
            uri: take(uriRaw),
            isDefault: (attr(m, 'DEFAULT') || '').toUpperCase() === 'YES',
            channels: keptAttr(m, 'CHANNELS') || '',
          });
          break;
        }
        case '#EXT-X-I-FRAME-STREAM-INF':
          isMaster = true; // trick-play streams are never tracks
          break;
        case '#EXT-X-SESSION-KEY':
        case '#EXT-X-KEY': {
          const m = parseAttributeList(value);
          if (isDrmKey(m)) out.drm = true;
          else if ((attr(m, 'METHOD') || '').toUpperCase() === 'AES-128') out.aesKeys++;
          break;
        }
        case '#EXTINF': {
          const d = parseFloat(value);
          if (Number.isFinite(d) && d >= 0) duration += d;
          sawExtinf = true;
          pendingSegment = true;
          break;
        }
        case '#EXT-X-MAP': {
          if (out.initUris.length >= L.initUris) break;
          const m = parseAttributeList(value);
          const r = take(attr(m, 'URI'), true);
          if (r) out.initUris.push(r);
          break;
        }
        case '#EXT-X-ENDLIST':
          out.endList = true;
          break;
        case '#EXT-X-PLAYLIST-TYPE':
          out.playlistType = value.trim().toUpperCase();
          break;
        default:
          break;
      }
      continue;
    }
    // A URI line.
    if (skipUri) {
      skipUri = false;
    } else if (pendingVariant) {
      const r = take(line);
      pendingVariant.uri = r;
      if (r) out.variants.push(pendingVariant);
      pendingVariant = null;
    } else if (pendingSegment) {
      const r = out.segments.length < L.segments ? take(line, true) : null;
      if (r) out.segments.push(r);
      pendingSegment = false;
    }
  }

  if (shared) shared.left -= sharedStart - sharedLeft;
  if (isMaster) {
    out.type = 'master';
  } else if (sawExtinf || out.endList || out.segments.length > 0) {
    out.type = 'media';
    out.durationSec = sawExtinf ? Math.round(duration * 1000) / 1000 : null;
  } else {
    out.type = 'media';
  }
  return out;
}

function variantKind(v) {
  if (v.height || v.width) return KIND_VIDEO;
  return kindOfCodecs(v.codecs);
}

function qualityOf(height) {
  return height ? `${height}p` : '';
}

const AUDIO_SEGMENT_EXT = new Set(['aac', 'mp3', 'ac3', 'ec3', 'm4a', 'cmfa']);

/** Kind of a media playlist from its segment extensions. */
export function mediaPlaylistKind(parsed) {
  const exts = new Set(parsed.segments.slice(0, 20).map((u) => extensionOfPath(new URL(u).pathname)));
  if (exts.size > 0 && [...exts].every((e) => AUDIO_SEGMENT_EXT.has(e))) return KIND_AUDIO;
  return KIND_UNKNOWN;
}

/**
 * Tracks of one HLS playlist. manifestUrl is the playlist's own URL.
 *
 * Each track: { url, kind, format: 'hls', quality, bandwidth, variants,
 * protected, childUrls, durationSec, name, language }. childUrls are the
 * playlists this track owns (absorbed when the player also requested them
 * directly); probeUrl is the media playlist worth fetching for duration
 * and DRM signals.
 */
export function hlsTracks(parsed, manifestUrl) {
  const url = urlKey(manifestUrl);
  if (parsed.type === 'invalid') return [];
  if (parsed.type === 'media') {
    return [{
      url,
      kind: mediaPlaylistKind(parsed),
      format: FORMAT_HLS,
      quality: '',
      bandwidth: null,
      variants: [],
      protected: parsed.drm,
      childUrls: [],
      probeUrl: null,
      durationSec: parsed.durationSec,
      name: '',
      language: '',
    }];
  }

  const tracks = [];
  const videoVariants = parsed.variants.filter((v) => variantKind(v) !== KIND_AUDIO);
  const audioOnlyVariants = parsed.variants.filter((v) => variantKind(v) === KIND_AUDIO);
  const renditionsWithUri = parsed.renditions.filter((r) => r.type === 'AUDIO' && r.uri);

  // Video: every non-audio variant together, best first.
  if (videoVariants.length > 0) {
    const sorted = [...videoVariants].sort(
      (a, b) => (b.height || 0) - (a.height || 0) || (b.bandwidth || 0) - (a.bandwidth || 0),
    );
    const best = sorted[0];
    const kinds = new Set(videoVariants.map(variantKind));
    tracks.push({
      url,
      kind: kinds.has(KIND_VIDEO) ? KIND_VIDEO : KIND_UNKNOWN,
      format: FORMAT_HLS,
      quality: qualityOf(best.height),
      bandwidth: best.averageBandwidth || best.bandwidth || null,
      variants: sorted.map((v) => ({
        url: v.uri,
        bandwidth: v.bandwidth,
        width: v.width,
        height: v.height,
        codecs: v.codecs,
      })),
      protected: parsed.drm,
      childUrls: videoVariants.map((v) => v.uri),
      probeUrl: best.uri,
      durationSec: null,
      name: '',
      language: '',
    });
  }

  // Audio: independent renditions, one track per language and name; when
  // several groups (bitrates) carry the same rendition, the one used by the
  // best variant wins.
  if (renditionsWithUri.length > 0) {
    const groupRank = new Map();
    for (const v of [...parsed.variants].sort((a, b) => (b.bandwidth || 0) - (a.bandwidth || 0))) {
      if (v.audio && !groupRank.has(v.audio)) groupRank.set(v.audio, groupRank.size);
    }
    const byKey = new Map();
    for (const r of renditionsWithUri) {
      const key = `${r.language.toLowerCase()}\u0000${r.name}`;
      if (!byKey.has(key)) byKey.set(key, []);
      byKey.get(key).push(r);
    }
    for (const list of byKey.values()) {
      const ranked = [...list].sort(
        (a, b) => (groupRank.get(a.groupId) ?? 1e9) - (groupRank.get(b.groupId) ?? 1e9),
      );
      const pick = ranked[0];
      tracks.push({
        url: pick.uri,
        kind: KIND_AUDIO,
        format: FORMAT_HLS,
        quality: pick.channels ? `${pick.channels}ch` : '',
        bandwidth: null,
        variants: ranked.map((r) => ({ url: r.uri, groupId: r.groupId })),
        protected: parsed.drm,
        childUrls: ranked.map((r) => r.uri),
        probeUrl: pick.uri,
        durationSec: null,
        name: pick.name,
        language: pick.language,
      });
    }
  } else if (audioOnlyVariants.length > 0) {
    // No independent renditions, but audio-only variants: one audio track.
    const sorted = [...audioOnlyVariants].sort((a, b) => (b.bandwidth || 0) - (a.bandwidth || 0));
    const best = sorted[0];
    tracks.push({
      // A master holding nothing but audio is itself the audio track; next
      // to video variants, the best audio-only variant stands for it.
      url: videoVariants.length > 0 ? best.uri : url,
      kind: KIND_AUDIO,
      format: FORMAT_HLS,
      quality: '',
      bandwidth: best.averageBandwidth || best.bandwidth || null,
      variants: sorted.map((v) => ({ url: v.uri, bandwidth: v.bandwidth, codecs: v.codecs })),
      protected: parsed.drm,
      childUrls: sorted.map((v) => v.uri),
      probeUrl: best.uri,
      durationSec: null,
      name: '',
      language: '',
    });
  }
  return tracks;
}

/** Size estimate in bytes from a bit rate and a duration; -1 when unknown. */
export function estimateSize(bandwidth, durationSec) {
  if (!bandwidth || !durationSec) return -1;
  return Math.round((bandwidth * durationSec) / 8);
}

export { KIND_UNKNOWN };
