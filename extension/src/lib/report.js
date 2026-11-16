// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The redacted detection report (SPEC-ext 贡献与合并 step 1; ext-brief §5).
//
// What it is for: a contributor exports it from a lecture page, reads it,
// and submits it as a fixture (fixtures/<profile id>/) next to a new
// profile. CI replays generic detection on it (src/lib/replay.js) and checks
// the profile names the tracks as the report says.
//
// Replay design: the report records the inputs of every route, redacted the
// same way everywhere —
//   inputs.frames[]       media elements and resource-timing entries per frame
//   inputs.webRequests[]  what the webRequest route saw (type, status, size)
//   inputs.fetches[]      every manifest, media playlist and file probe the
//                         detection made, with the redacted manifest text
// — and the result as `expected`. A replay feeds inputs.frames and
// inputs.webRequests to the same detectTracks(), answering its fetches from
// inputs.fetches by URL. Because one ID always becomes the same fake, a
// master playlist still points at its media playlists after redaction, so
// the replay must reproduce `expected` exactly.
//
// No cookie is ever read for a report. Anything that looks like personal
// data stops the export (findings), even where the redaction removed it.
// Pure.

import { createRedactor, shape, isIdentityField, tenantBasesOf, safeLanguage } from './redact.js';
import { classifyUrl, C_OTHER, KIND_AUDIO, KIND_VIDEO } from './classify.js';
import { summarize } from './detect.js';
import { cleanObservedRecord } from './observe.js';
import { parseHttpUrl, hostMatches } from './url.js';
import { RETAIN_LIMITS } from './retain.js';

export const REPORT_KIND = 'lectureflow-detection-report';
export const REPORT_VERSION = 1;
// a longer manifest text is left out (`truncated`); the background keeps no
// longer one either (src/lib/retain.js reportTextLength)
const MAX_TEXT = RETAIN_LIMITS.textChars;
const MAX_NODES = 3;
const REPORT_LIMIT_KEYS = ['frames', 'elements', 'sources', 'resources', 'webRequests', 'longUrls', 'scanChars', 'longValues', 'fileTracks', 'tracks'];

/**
 * input: {
 *   extension: { version, browser },
 *   pageUrl,
 *   frames: [{ frameId, url, elements, resources, reads: [{ key, selector,
 *     attr, count, value, nodes: [{ path: [{ tag, id, classes }], attrNames }] }] }],
 *   webRequests: [...],
 *   detection: detectTracks() result,
 *   match: { profile, reviewed, url } | null,
 *   named: applyProfile() result | null,
 *   session: { sessionId, source },
 *   lecture: { title, recordedAt, durationSec },
 *   downloadPolicy: 'enabled' | 'disabled' | 'unknown',
 *   limits: { frames, elements, sources, resources, webRequests, longUrls,
 *     scanChars, longValues, fileTracks, tracks } | null: what was left out past a
 *     limit (the report's `limits`, only the counts above 0),
 *   now: Date,
 * }
 * Returns { report, findings }. Export only when findings is empty.
 */
export function buildReport(input) {
  // Tenant subdomains of the matched platform become "tenant"; the school
  // page a platform is embedded in (an LMS) is masked (lead ruling 8).
  const profile = input.match ? input.match.profile : null;
  const maskHosts = [];
  if (profile) {
    for (const raw of [input.pageUrl, ...(input.frames || []).map((f) => f.url)]) {
      const u = parseHttpUrl(raw);
      if (u && !profile.match.hosts.some((h) => hostMatches(h, u.hostname))) maskHosts.push(u.hostname);
    }
  }
  const r = createRedactor({ tenantBases: tenantBasesOf(profile), maskHosts });
  const at = (w) => r.setWhere(w);

  at('page');
  const page = {
    url: r.url(input.pageUrl),
    frames: (input.frames || []).map((f) => ({ frameId: f.frameId, url: r.url(f.url) })),
  };

  // Inputs: only what generic detection looks at.
  at('inputs.frames');
  const frames = (input.frames || []).map((f) => ({
    frameId: f.frameId,
    url: r.url(f.url),
    encryptedEvents: f.encryptedEvents > 0 ? f.encryptedEvents : 0,
    elements: (f.elements || []).map((e) => ({
      tag: e.tag,
      src: e.src ? r.url(e.src) : '',
      currentSrc: e.currentSrc ? r.url(e.currentSrc) : '',
      sources: (e.sources || []).map((s) => ({ src: r.url(s.src), type: safeType(s.type) })),
      mediaKeys: !!e.mediaKeys,
      videoWidth: numOrNull(e.videoWidth),
      videoHeight: numOrNull(e.videoHeight),
      duration: Number.isFinite(e.duration) ? Math.round(e.duration * 1000) / 1000 : null,
      readyState: numOrNull(e.readyState),
    })),
    resources: (f.resources || [])
      .filter((x) => classifyUrl(x.url, { initiatorType: x.initiatorType }).cls !== C_OTHER)
      .map((x) => ({
        url: r.url(x.url),
        initiatorType: String(x.initiatorType || ''),
        responseStatus: numOrNull(x.responseStatus),
      })),
    // Values the profile's observe took from the page's own responses:
    // only valid records of the matched profile; URLs redacted like every
    // other URL, other strings reduced to their shape.
    ...(profile && (f.observed || []).length ? { observed: redactObserved(r, profile, f.observed) } : {}),
  }));

  at('inputs.webRequests');
  const webRequests = (input.webRequests || [])
    .filter((w) => classifyUrl(w.url, { contentType: w.contentType, resourceType: w.resourceType, status: w.statusCode }).cls !== C_OTHER)
    .map((w) => ({
      url: r.url(w.url),
      frameId: w.frameId ?? 0,
      resourceType: String(w.resourceType || ''),
      statusCode: numOrNull(w.statusCode),
      contentType: safeType(w.contentType),
      contentLength: numOrNull(w.contentLength),
      contentRangeTotal: numOrNull(w.contentRangeTotal),
    }));

  const fetches = (input.detection?.fetches || []).map((f, i) => {
    at(`inputs.fetches[${i}]`);
    const out = {
      purpose: f.purpose,
      url: r.url(f.url),
      ok: !!f.ok,
      status: f.status || 0,
      contentType: safeType(f.contentType),
    };
    if (f.purpose === 'file') out.size = f.size ?? -1;
    else {
      const text = String(f.text || '');
      const body = text.replace(/^\ufeff/, '').trimStart();
      // f.truncated: the background did not keep this text (src/lib/retain.js)
      if (f.truncated || body.length > MAX_TEXT) out.truncated = true;
      else if (body.startsWith('#EXTM3U')) out.text = r.hls(text);
      else if (body.startsWith('<')) out.text = r.mpd(text);
      else if (body) out.text = '';
    }
    if (f.error) out.error = shape(f.error, 80);
    return out;
  });

  // DOM: only the nodes the profile's selectors hit, as structure.
  at('dom');
  const dom = [];
  for (const f of input.frames || []) {
    for (const rd of f.reads || []) {
      dom.push({
        frameId: f.frameId,
        key: rd.key,
        selector: rd.selector,
        attr: rd.attr || '',
        count: rd.count || 0,
        valueShape: shape(rd.value || ''),
        nodes: (rd.nodes || []).slice(0, MAX_NODES).map((n) => ({
          path: (n.path || []).map((p) => ({
            tag: String(p.tag || '').toLowerCase().slice(0, 32),
            id: p.id ? r.segment(String(p.id).slice(0, 64)) : '',
            classes: (p.classes || []).slice(0, 8).map((c) => r.segment(String(c).slice(0, 64))),
          })),
          attrNames: (n.attrNames || []).slice(0, 32).map((a) => {
            if (isIdentityField(a)) r.note('identity_field', `attribute "${a}" on a selected node`);
            return String(a).slice(0, 64);
          }),
        })),
      });
    }
  }

  at('expected');
  const det = input.detection || { tracks: [], evidence: {}, segmentRuns: [] };
  const expected = {
    summary: summarize(det.tracks),
    tracks: det.tracks.map((t) => redactTrack(r, t)),
    segmentRuns: (det.segmentRuns || []).map((s) => ({ count: s.count })),
    evidence: {
      mse: !!det.evidence?.mse,
      mediaKeys: !!det.evidence?.mediaKeys,
      mediaKeysOnStream: !!det.evidence?.mediaKeysOnStream,
      encrypted: !!det.evidence?.encrypted,
    },
    profile: null,
  };
  if (input.match && input.named) {
    expected.profile = {
      id: input.match.profile.id,
      version: input.match.profile.version,
      sessionIdSource: input.session?.source || 'generic',
      lecture: {
        titleShape: shape(input.lecture?.title || ''),
        recordedAtShape: shape(input.lecture?.recordedAt || ''),
        durationSec: input.lecture?.durationSec || 0,
      },
      downloadPolicy: input.downloadPolicy || 'unknown',
      tracks: input.named.tracks.map((t) => ({
        id: redactTrackId(r, t),
        role: t.role,
        labelIndex: t.labelIndex || 0,
        recommended: !!t.recommended,
      })),
      others: input.named.others.length,
    };
  }

  const report = {
    kind: REPORT_KIND,
    reportVersion: REPORT_VERSION,
    generatedAt: dateOnly(input.now || new Date()),
    extension: { version: String(input.extension?.version || ''), browser: String(input.extension?.browser || '') },
    page,
    profile: input.match
      ? { id: input.match.profile.id, version: input.match.profile.version, reviewed: !!input.match.reviewed }
      : null,
    inputs: { frames, webRequests, fetches },
    dom,
    expected,
  };
  // what the extension left out past its limits (docs/extension/limits.md), only when something was
  const limits = {};
  for (const k of REPORT_LIMIT_KEYS) {
    const n = input.limits ? Number(input.limits[k]) : 0;
    if (Number.isInteger(n) && n > 0) limits[k] = n;
  }
  if (Object.keys(limits).length) report.limits = limits;

  const findings = [...r.findings, ...scanReport(report)].map(({ kind, where, detail }) => ({ kind, where, detail }));
  return { report, findings: dedupeFindings(findings) };
}

function redactObservedValue(r, v) {
  if (typeof v !== 'string') return v; // null, boolean, number
  return /^https?:\/\//i.test(v) ? r.url(v) : shape(v, 120);
}

function redactObserved(r, profile, records) {
  const out = [];
  for (const raw of records) {
    const rec = cleanObservedRecord(profile, raw);
    if (!rec) continue;
    const values = {};
    for (const [name, v] of Object.entries(rec.values)) values[name] = Array.isArray(v) ? v.map((x) => redactObservedValue(r, x)) : redactObservedValue(r, v);
    out.push({ key: rec.key, index: rec.index, url: r.url(rec.url), values });
  }
  return out;
}

function redactTrackId(r, t) {
  return t.selector ? `${r.url(t.url)}#lf=${t.selector}` : r.url(t.url);
}

export function redactTrack(r, t) {
  const out = {
    id: redactTrackId(r, t),
    url: r.url(t.url),
    kind: t.kind,
    format: t.format,
    sizeBytes: t.sizeBytes,
    sizeEstimated: !!t.sizeEstimated,
    sources: t.sources,
    frameIds: t.frameIds,
    protected: !!t.protected,
  };
  if (t.selector) out.selector = t.selector;
  if (t.quality) out.quality = t.quality;
  if (t.durationSec) out.durationSec = t.durationSec;
  if (t.mime) out.mime = safeType(t.mime);
  if (t.variants) out.variants = t.variants.map((v) => ({ ...v, ...(v.url ? { url: r.url(v.url) } : {}), ...(v.id ? { id: r.segment(String(v.id)) } : {}) }));
  if (t.name) out.name = shape(t.name);
  if (t.language) out.language = safeLanguage(t.language);
  if (t.unreadable) out.unreadable = true;
  return out;
}

function safeType(t) {
  const s = String(t || '').trim();
  return /^[A-Za-z0-9.+\-/]+(\s*;\s*[A-Za-z0-9.+\-]+=("[A-Za-z0-9.,+\- ]*"|[A-Za-z0-9.,+\-]+))*$/.test(s) && s.length <= 120 ? s : shape(s, 60);
}

function numOrNull(v) {
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

function dateOnly(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())}`;
}

/**
 * Last line of defence over the finished report: any "@" (other than an
 * HLS byte range "1000@2000") or an identity-like query parameter name.
 */
export function scanReport(report) {
  const findings = [];
  const walk = (v, path) => {
    if (typeof v === 'string') {
      const stripped = v.replace(/\d+@\d+/g, '');
      if (stripped.includes('@') || /%40/i.test(stripped)) findings.push({ kind: 'email', where: path, detail: '"@" in the report' });
      const qm = v.indexOf('?');
      if (/^https?:\/\//.test(v) && qm >= 0) {
        for (const part of v.slice(qm + 1).split('&')) {
          const name = part.split('=')[0];
          if (isIdentityField(decodeSafe(name))) findings.push({ kind: 'identity_field', where: path, detail: `query parameter "${name}"` });
        }
      }
    } else if (Array.isArray(v)) v.forEach((x, i) => walk(x, `${path}[${i}]`));
    else if (v && typeof v === 'object') for (const [k, x] of Object.entries(v)) walk(x, path ? `${path}.${k}` : k);
  };
  walk(report, '');
  return findings;
}

function decodeSafe(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

function dedupeFindings(list) {
  const seen = new Set();
  return list.filter((f) => {
    const k = `${f.kind}|${f.where}|${f.detail}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

/** File name for a downloaded report: no title, no host. */
export function reportFileName(report) {
  const id = report.profile ? report.profile.id : 'generic';
  return `lectureflow-report-${id}-${report.generatedAt}.json`;
}

export { KIND_AUDIO, KIND_VIDEO };
