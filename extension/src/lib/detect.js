// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Generic track detection (SPEC-ext 通用轨道探测). Pure: the network is
// reached only through the two functions in `io`, so the same code runs in
// the background and in the fixture replay (src/lib/replay.js).

import { buildCandidates, mergeCandidates, collapseSegmentRuns, longestDuration, SOURCE_ORDER, SRC_ELEMENT, MAX_URL_LENGTH } from './merge.js';
import { C_HLS, C_DASH, C_FILE, KIND_AUDIO, KIND_VIDEO, KIND_UNKNOWN, FORMAT_FILE, FORMAT_HLS, FORMAT_DASH, mediaType, formatInPath, extensionClass } from './classify.js';
import { parseHls, hlsTracks, estimateSize, mediaPlaylistKind } from './hls.js';
import { parseMpd, dashTracks, childMatchers } from './dash.js';
import { urlKey, extensionOfPath } from './url.js';

export const LIMITS = Object.freeze({
  manifests: 12, // manifests fetched per detection
  probes: 8, // media playlists fetched for duration and DRM signals
  files: 8, // single files probed for size and type
  // address characters all manifests of one detection may resolve, on top
  // of each one's own 16 Mi: a four-hour lecture in 2 s segments signed
  // with 1.3 KB is ~9.4 MB per playlist, a page plays two or three
  urlBytes: 48 * 1024 * 1024,
  // What one scan hands detection, and the background keeps for the report
  // (boundScan below; SEC28 低-1, SEC28b 中-4). Past each, the first ones
  // in the scan's order stay and the rest are left out; the report says
  // how many (`limits`). A real lecture page has a few frames, a few media
  // elements and a few hundred to two thousand resource entries per frame
  // (the scan takes at most 2000 a frame), of addresses up to a few KB.
  //   an address longer than this (as the page gives it; as detection keys
  //   it, without its fragment) is left out: past 8 KB most servers and
  //   CDNs refuse the request anyway (nginx, Apache, CloudFront), and V8
  //   hashes a string of up to 16383 characters by its content, so every
  //   table detection keys by address stays linear (MAX_URL_LENGTH)
  urlLength: MAX_URL_LENGTH,
  frames: 128, // frames of one scan (the top frame always stays)
  elements: 1000, // media elements of all frames together
  sources: 16, // <source> children of one media element
  resources: 10000, // resource-timing entries of all frames together
  scanChars: 16 * 1024 * 1024, // address characters the embedded frames and all of the above hold together
  // address characters the request records hold, an allowance of their
  // own (SEC33 低-2: the browser's record of media and manifest responses,
  // the most reliable evidence, is never crowded out by page content);
  // requests.js keeps at most 300 a tab, 2.4 Mi characters at most
  requestChars: 4 * 1024 * 1024,
  // other values a scan hands on (SEC33 低-3): a <source>'s type longer
  // than attrLength counts as absent (as hls.js / dash.js treat an
  // over-long CODECS); in a selector read, a node's tag, id and classes
  // are cut to attrLength and its attribute names to the first attrNames
  // (the report keeps 64 characters and 32 names of them). Real values are
  // well under 100 characters, a node has a few attributes.
  attrLength: 256,
  attrNames: 32,
  fileTracks: 1000, // single-file tracks (first the ones any media element refers to: src, currentSrc or a <source>)
});

// A manifest address longer than this is skipped before anything is read:
// every DASH track's id carries it, up to 512 times per MPD (SEC25 中-3).
// Signed manifest addresses are 1-2 KB, rarely over 4 KB. The same as
// LIMITS.urlLength, which leaves such an address out before this.
export const MAX_MANIFEST_URL_LENGTH = 8192;

/**
 * input: { frames, webRequests } (see buildCandidates)
 * io: {
 *   fetchText(url, { frameIds, purpose }) → Promise<{ ok, status, contentType, text, error }>
 *   probeFile(url, { frameIds }) → Promise<{ ok, status, contentType, size, error }>
 * }
 * Returns { tracks, evidence, segmentRuns, fetches, ignored }, and
 * `limited` ({ longUrls?, fileTracks? }: what was left out past
 * LIMITS.urlLength and LIMITS.fileTracks) only when something was.
 * fetches is the log of every io call in call order; the report keeps it
 * so a replay can answer the same calls.
 */
export async function detectTracks(input, io, limits = LIMITS) {
  const { candidates, evidence, longUrls } = buildCandidates(input, { urlLength: limits.urlLength ?? LIMITS.urlLength });
  const merged = mergeCandidates(candidates);
  const segmentRuns = collapseSegmentRuns(merged);
  const fetches = [];
  const textCache = new Map();
  // one budget per detection, shared by every manifest it parses
  const budget = { left: limits.urlBytes ?? LIMITS.urlBytes };

  const fetchText = async (url, ctx) => {
    if (textCache.has(url)) return textCache.get(url);
    let res;
    try {
      res = await io.fetchText(url, ctx);
    } catch (e) {
      res = { ok: false, status: 0, contentType: '', text: '', error: String(e && e.message ? e.message : e) };
    }
    res = normalizeFetch(res);
    fetches.push({ purpose: ctx.purpose, url, ...res });
    textCache.set(url, res);
    return res;
  };

  // 1. Manifests: fetch and parse.
  const manifestCands = merged.filter((m) => (m.cls === C_HLS || m.cls === C_DASH) && m.url.length <= MAX_MANIFEST_URL_LENGTH);
  const parsedManifests = [];
  for (const m of manifestCands.slice(0, limits.manifests)) {
    const res = await fetchText(m.url, { frameIds: m.frameIds, purpose: 'manifest' });
    let parsed = null;
    let cls = m.cls;
    if (res.ok) {
      // The body decides when the server's type and the name disagree.
      const body = res.text.replace(/^\ufeff/, '').trimStart();
      if (body.startsWith('#EXTM3U')) cls = C_HLS;
      else if (body.startsWith('<')) cls = C_DASH;
      parsed = cls === C_HLS ? parseHls(res.text, m.url, { budget }) : parseMpd(res.text, m.url, { budget });
      if (parsed.type === 'invalid') parsed = null;
    }
    parsedManifests.push({ cand: m, cls, parsed, fetched: res.ok });
  }
  const unread = manifestCands.slice(limits.manifests);

  // 2. Tracks from manifests. Masters and MPDs first, so that media
  //    playlists the player loaded on their behalf are absorbed.
  const absorbed = new Map(); // child url → owning track
  const patterns = childMatchers(); // DASH segment templates → owning track
  const tracks = [];
  const order = [...parsedManifests].sort((a, b) => rank(a) - rank(b));
  for (const pm of order) {
    if (pm.parsed && pm.cls === C_HLS && pm.parsed.type === 'media' && absorbed.has(pm.cand.url)) {
      const owner = absorbed.get(pm.cand.url);
      addSources(owner, pm.cand.sources);
      pm.parsed.segments.forEach((s) => absorbed.set(s, owner));
      if (pm.parsed.drm) owner.protected = true;
      if (!owner.durationSec && pm.parsed.durationSec) owner.durationSec = pm.parsed.durationSec;
      continue;
    }
    if (!pm.parsed) {
      tracks.push(baseTrack({
        url: pm.cand.url,
        kind: KIND_UNKNOWN,
        format: pm.cls === C_HLS ? FORMAT_HLS : FORMAT_DASH,
        sources: pm.cand.sources,
        frameIds: pm.cand.frameIds,
        unreadable: true,
      }));
      continue;
    }
    const derived = pm.cls === C_HLS ? hlsTracks(pm.parsed, pm.cand.url) : dashTracks(pm.parsed, pm.cand.url);
    for (const d of derived) {
      const t = baseTrack({ ...d, sources: pm.cand.sources, frameIds: pm.cand.frameIds });
      tracks.push(t);
      for (const c of d.childUrls || []) absorbed.set(c, t);
      // distinct patterns only, at most TEMPLATE_LIMITS.matchers; the rest absorb nothing
      for (const p of d.childPatterns || []) patterns.add(p, t);
    }
    if (pm.cls === C_HLS && pm.parsed.type === 'media') {
      pm.parsed.segments.forEach((s) => absorbed.set(s, tracks[tracks.length - 1]));
      pm.parsed.initUris.forEach((s) => absorbed.set(s, tracks[tracks.length - 1]));
    }
  }
  for (const m of unread) {
    tracks.push(baseTrack({
      url: m.url,
      kind: KIND_UNKNOWN,
      format: m.cls === C_HLS ? FORMAT_HLS : FORMAT_DASH,
      sources: m.sources,
      frameIds: m.frameIds,
      unreadable: true,
    }));
  }

  // 3. Probe one media playlist per HLS track for duration and DRM.
  let probes = 0;
  for (const t of tracks) {
    if (t.format !== FORMAT_HLS || !t.probeUrl || probes >= limits.probes) continue;
    if (t.durationSec && t.probeUrl === t.url) continue;
    probes++;
    const res = await fetchText(t.probeUrl, { frameIds: t.frameIds, purpose: 'probe' });
    if (!res.ok) continue;
    const p = parseHls(res.text, t.probeUrl, { budget });
    if (p.type !== 'media') continue;
    if (p.drm) t.protected = true;
    if (p.durationSec) t.durationSec = p.durationSec;
    if (t.kind === KIND_UNKNOWN) {
      const k = mediaPlaylistKind(p);
      if (k !== KIND_UNKNOWN) t.kind = k;
    }
    p.segments.forEach((s) => absorbed.set(s, t));
    p.initUris.forEach((s) => absorbed.set(s, t));
  }

  // 4. Single files that no manifest owns: at most limits.fileTracks of
  //    them: first the ones any media element refers to (its src,
  //    currentSrc or a <source>, playing or not), then the rest, each in
  //    address order.
  const ignored = [];
  let fileProbes = 0;
  const owners = new Map(); // merged entry → owning track, or null
  const unowned = [];
  for (const m of merged) {
    if (m.cls !== C_FILE) continue;
    const owner = absorbed.get(m.url) || patterns.find(m.url) || null;
    owners.set(m, owner);
    if (!owner) unowned.push(m);
  }
  const maxFiles = limits.fileTracks ?? LIMITS.fileTracks;
  let skipped = null; // the files left out, when there are too many
  if (unowned.length > maxFiles) {
    const kept = new Set([...unowned.filter((m) => m.sources.includes(SRC_ELEMENT)), ...unowned.filter((m) => !m.sources.includes(SRC_ELEMENT))].slice(0, maxFiles));
    skipped = new Set(unowned.filter((m) => !kept.has(m)));
  }
  for (const m of merged) {
    if (m.cls !== C_FILE) continue;
    const owner = owners.get(m);
    if (owner) {
      addSources(owner, m.sources);
      continue;
    }
    if (skipped && skipped.has(m)) continue;
    const t = baseTrack({
      url: m.url,
      kind: m.kind,
      format: FORMAT_FILE,
      sources: m.sources,
      frameIds: m.frameIds,
      mime: mediaType(m.hints.contentType || m.hints.sourceType || ''),
      sizeBytes: m.hints.size > 0 ? m.hints.size : -1,
      durationSec: m.hints.duration || null,
    });
    // What the media element knows: an <audio>, or a <video> with no
    // picture once its metadata is in, is audio.
    if (m.hints.tag === 'audio') t.kind = KIND_AUDIO;
    else if (m.hints.tag === 'video' && m.hints.readyState >= 1) {
      if (m.hints.videoWidth === 0 && m.hints.videoHeight === 0) t.kind = KIND_AUDIO;
      else if (m.hints.videoHeight > 0) {
        t.kind = KIND_VIDEO;
        t.quality = `${m.hints.videoHeight}p`;
      }
    }
    if ((t.sizeBytes < 0 || t.kind === KIND_UNKNOWN || !t.mime) && fileProbes < limits.files) {
      fileProbes++;
      let res;
      try {
        res = await io.probeFile(m.url, { frameIds: m.frameIds });
      } catch (e) {
        res = { ok: false, status: 0, contentType: '', size: -1, error: String(e && e.message ? e.message : e) };
      }
      res = normalizeProbe(res);
      fetches.push({ purpose: 'file', url: m.url, ...res });
      if (res.ok) {
        if (res.size > 0 && t.sizeBytes < 0) t.sizeBytes = res.size;
        const type = mediaType(res.contentType);
        if (type && !t.mime) t.mime = type;
        if (t.kind === KIND_UNKNOWN) {
          if (type.startsWith('audio/')) t.kind = KIND_AUDIO;
          else if (type.startsWith('video/')) t.kind = KIND_VIDEO;
        }
      }
    }
    tracks.push(t);
  }
  for (const m of merged) {
    if (m.cls !== C_FILE && m.cls !== C_HLS && m.cls !== C_DASH) ignored.push({ url: m.url, cls: m.cls, sources: m.sources });
  }

  // 5. EME: a media element with MediaKeys protects what it plays. With
  //    MSE (blob: source) we cannot tell which manifest feeds it, so every
  //    adaptive track is marked.
  const keyed = new Set(evidence.mediaKeysUrls);
  for (const t of tracks) {
    if (keyed.has(t.url)) t.protected = true;
    if (evidence.mediaKeysOnStream && t.format !== FORMAT_FILE) t.protected = true;
  }

  // 6. Durations and size estimates.
  const pageDuration = longestDuration(evidence.mediaDurations);
  for (const t of tracks) {
    if (!t.durationSec && pageDuration && t.format !== FORMAT_FILE) t.durationSec = pageDuration;
    if (t.sizeBytes < 0 && t.format !== FORMAT_FILE) t.sizeBytes = estimateSize(t.bandwidth, t.durationSec);
    if (t.format !== FORMAT_FILE && t.sizeBytes > 0) t.sizeEstimated = true;
  }

  const final = dedupeTracks(tracks).map(finalize).sort(compareTracks);
  const out = { tracks: final, evidence, segmentRuns, fetches, ignored };
  const limited = {};
  if (longUrls) limited.longUrls = longUrls;
  if (skipped) limited.fileTracks = skipped.size;
  if (Object.keys(limited).length) out.limited = limited;
  return out;
}

/**
 * A scan held to LIMITS (see there): frames, media elements (and their
 * <source>s), resource entries and request records past their counts, or
 * holding an address longer than urlLength, or once scanChars (requests:
 * requestChars) of addresses are taken, are left out. A media element
 * whose address is left out stays without it (its duration and MediaKeys
 * still count). Within the limits the scan comes back as it was, but for
 * the fragments of embedded frames' addresses (below). Pure.
 *
 * What stays first when something must go (SEC33 低-2): the strongest
 * evidence of a lecture, not whatever the top document lists first.
 *   - Frames: the frame detection runs in (opts.frameId: an in-page
 *     button's frame) and the top frame always, whatever their address
 *     (their own addresses are not counted); then the frames that look
 *     like media (a media element with an address, a resource entry whose
 *     name looks like a manifest or media file: scanHint), then the others with
 *     something in them, then the empty ones, each in scan order. Another
 *     frame's address loses its `#` fragment (as buildCandidates keys
 *     addresses; a fragment never reaches a server) and is measured
 *     against urlLength and charged to scanChars so, as the frames are
 *     picked: at most (frames - 1) × urlLength characters.
 *   - Request records (the browser's own, media and manifest responses
 *     only): an allowance of their own, requestChars.
 *   - Then the frames' contents. Each frame orders its own by strength:
 *     resource entries that look like HLS or DASH manifests; media
 *     elements that play something (currentSrc); resource entries that
 *     look like media files; other media elements with an address;
 *     everything else (other resource entries, elements without an
 *     address). The first four (evidence) of every frame go before the
 *     fifth of any. Within each part the frames take turns (SEC33d 低-6):
 *     each turn every frame offers its strongest entry left, and the
 *     turn's entries go strongest first, those of one strength in frame
 *     order (the frames that always stay first, then the others in scan
 *     order). No frame takes a second entry before every other frame had a
 *     first; a frame's turns alternate between its own document and the
 *     same-origin child documents the scan read into its lists (`doc`:
 *     SEC36 低-7); an entry pays by its address characters: an element of
 *     more than urlLength waits a turn per further urlLength, and a turn it
 *     waits is its side's turn, the next one going to the other side
 *     (SEC36 低-8, SEC36b 低-11, SEC37 低-13). So a frame takes about
 *     urlLength a turn at most, and to crowd the first n entries of
 *     evidence of a frame's own document out of scanChars, the other
 *     frames must fill about n turns (2n with child documents): the first
 *     dozen or so are safe at the real limits (about eight with child
 *     documents), no more. The price: an entry of the frame's own document
 *     that itself waits t turns comes up to 2t turns after the one before
 *     when the frame has child documents (t when it has none).
 *   - The addresses of frames the scan could not read (`opaque`, not
 *     charged): one longer than urlLength loses its fragment, or is left
 *     out.
 * The output keeps every list in scan order.
 * input: { frames, webRequests } as detectTracks takes them. Returns
 * { frames, webRequests, left } where left counts what was left out:
 * { frames, elements, sources, resources, webRequests, longUrls,
 * scanChars, longValues }.
 */
export function boundScan({ frames = [], webRequests = [] }, limits = LIMITS, { frameId = 0 } = {}) {
  const L = { ...LIMITS, ...limits };
  const left = { frames: 0, elements: 0, sources: 0, resources: 0, webRequests: 0, longUrls: 0, scanChars: 0, longValues: 0 };
  const scanBudget = { chars: L.scanChars };
  const requestBudget = { chars: L.requestChars };
  // an address that may stay: '' (none) always; otherwise within urlLength
  // and what is left of the budget
  const fits = (u, budget = scanBudget) => {
    if (typeof u !== 'string' || u === '') return true;
    if (u.length > L.urlLength) {
      left.longUrls++;
      return false;
    }
    if (u.length > budget.chars) {
      left.scanChars++;
      return false;
    }
    budget.chars -= u.length;
    return true;
  };
  // a frame's address without its fragment (nothing that reads a frame's
  // address looks at the fragment: profile matching takes host and path,
  // the session id query or path, the fetches the origin, the report
  // drops it)
  const withoutFragment = (s) => {
    if (typeof s !== 'string') return s;
    const i = s.indexOf('#');
    return i < 0 ? s : s.slice(0, i);
  };
  // what a resource entry looks like, for the order only (scanHint: no URL
  // parsing, so that ranking every frame's entries stays cheap); an
  // address too long to stay is not looked at; each entry once
  const hints = new Map();
  const hintOf = (r) => {
    if (!r || typeof r.url !== 'string' || r.url.length > L.urlLength) return '';
    let h = hints.get(r);
    if (h === undefined) hints.set(r, (h = scanHint(r.url, r.initiatorType, r.responseStatus)));
    return h;
  };
  const isManifest = (r) => hintOf(r) === 'manifest';
  const isMediaFile = (r) => hintOf(r) === 'file';
  const playing = (el) => !!el && typeof el.currentSrc === 'string' && el.currentSrc !== '';
  const addressed = (el) => !!el && ((typeof el.src === 'string' && el.src !== '') || (Array.isArray(el.sources) && el.sources.some((s) => s && typeof s.src === 'string' && s.src !== '')));
  const elementsOf = (f) => (Array.isArray(f.elements) ? f.elements : []);
  const resourcesOf = (f) => (Array.isArray(f.resources) ? f.resources : []);

  // 1. The frames.
  const first = [...new Set([frameId || 0, 0])];
  const always = (f) => first.includes(f.frameId);
  const present = new Set(frames.filter((f) => f && always(f)).map((f) => f.frameId));
  let frameRoom = Math.max(0, L.frames - present.size);
  const rankOf = (f) => {
    const els = elementsOf(f);
    const res = resourcesOf(f);
    if (els.some((el) => playing(el) || addressed(el)) || res.some((r) => isManifest(r) || isMediaFile(r))) return 0;
    return els.length || res.length ? 1 : 2;
  };
  const others = [];
  frames.forEach((f, i) => {
    if (f && !always(f)) others.push({ f, i, rank: rankOf(f) });
  });
  others.sort((a, b) => a.rank - b.rank || a.i - b.i);
  const picked = new Map(); // frame → the frame as it stays (its address without the fragment)
  for (const { f } of others) {
    const url = withoutFragment(f.url);
    if (frameRoom <= 0 || !fits(url)) {
      left.frames++;
      continue;
    }
    frameRoom--;
    picked.set(f, url === f.url ? f : { ...f, url });
  }
  const staying = [];
  for (const f of frames) {
    if (!f) continue;
    if (always(f)) staying.push(f);
    else if (picked.has(f)) staying.push(picked.get(f));
  }

  // 2. The request records, on their own allowance.
  const outRequests = webRequests.filter((w) => {
    if (w && fits(w.url, requestBudget)) return true;
    left.webRequests++;
    return false;
  });

  // 3. The frames' contents, strongest first.
  const order = [...first.flatMap((id) => staying.filter((x) => x.frameId === id)), ...staying.filter((x) => !always(x))];
  let elementRoom = L.elements;
  let resourceRoom = L.resources;
  const elementOut = new Map(); // element → what stays of it, or null
  const resourceOut = new Map(); // resource entry → true when it stays
  const takeElement = (el) => {
    if (elementRoom <= 0) {
      left.elements++;
      elementOut.set(el, null);
      return;
    }
    elementRoom--;
    const fix = {};
    for (const k of ['src', 'currentSrc']) if (!fits(el[k])) fix[k] = '';
    const srcs = Array.isArray(el.sources) ? el.sources : [];
    const sources = [];
    let typeCut = false;
    for (const s of srcs) {
      if (sources.length >= L.sources) {
        left.sources++;
        continue;
      }
      if (!s || !fits(s.src)) continue;
      // an over-long type counts as absent
      if (typeof s.type === 'string' && s.type.length > L.attrLength) {
        left.longValues++;
        typeCut = true;
        sources.push({ ...s, type: '' });
      } else sources.push(s);
    }
    if (sources.length !== srcs.length || typeCut) fix.sources = sources;
    elementOut.set(el, Object.keys(fix).length ? { ...el, ...fix } : el);
  };
  const takeResource = (r) => {
    if (resourceRoom <= 0) {
      left.resources++;
      resourceOut.set(r, false);
      return;
    }
    resourceRoom--;
    resourceOut.set(r, !!r && fits(r.url));
  };
  // Each frame sorts its entries into the five tiers (in each, the frame's
  // resource entries before its elements, in scan order; pairs: 0 and a
  // resource entry, or 1 and an element). The four tiers of evidence go
  // before the fifth (no evidence) of any frame; within each part the
  // frames take turns (SEC33d 低-6): each turn every frame with something
  // left offers its strongest remaining entry, and the turn's entries are
  // taken strongest tier first, a tier's in frame order (the frames that
  // always stay first). So no frame, the top one included, takes a second
  // entry before every other frame had a first. An entry pays by its
  // address characters (SEC36 低-8): one with more than urlLength of them
  // (an element with many long addresses) is offered only after its queue
  // (the frame's own document, a child document) sat out one of its turns
  // for each further urlLength; a turn sat out is that side's turn, the
  // next goes to the other side (SEC36b 低-11, SEC37 低-13), so a turn
  // costs any frame at most about urlLength characters.
  const len = (u) => (typeof u === 'string' && u.length <= L.urlLength ? u.length : 0);
  const turnsOf = (kind, entry) => {
    if (kind === 0 || !entry || !(L.urlLength > 0)) return 1;
    let n = len(entry.src) + len(entry.currentSrc);
    const srcs = Array.isArray(entry.sources) ? entry.sources : [];
    for (let i = 0; i < srcs.length && i < L.sources; i++) n += srcs[i] ? len(srcs[i].src) : 0;
    return Math.max(1, Math.ceil(n / L.urlLength));
  };
  // The scan reads a frame's same-origin child frames too (scanFrame,
  // `doc` > 0), into the frame's own lists: a frame's turns alternate
  // between its own document and those, the children in turn (SEC36 低-7),
  // so a child document full of junk cannot take all of its parent's turns.
  const byFrame = order.map((f) => {
    const docs = new Map(); // doc → entries by tier
    const of = (x) => {
      const d = x && Number.isInteger(x.doc) && x.doc > 0 ? x.doc : 0;
      if (!docs.has(d)) docs.set(d, [[], [], [], [], []]);
      return docs.get(d);
    };
    for (const r of resourcesOf(f)) of(r)[isManifest(r) ? 0 : isMediaFile(r) ? 2 : 4].push(0, r);
    for (const el of elementsOf(f)) of(el)[playing(el) ? 1 : addressed(el) ? 3 : 4].push(1, el);
    return { own: docs.get(0) || null, kids: [...docs.keys()].filter((d) => d > 0).sort((a, b) => a - b).map((d) => docs.get(d)) };
  });
  const takeTurns = (tiers) => {
    const queue = (byTier) => {
      const q = []; // triples: tier, 0 | 1, the entry
      if (byTier) for (const tier of tiers) for (let i = 0; i < byTier[tier].length; i += 2) q.push(tier, byTier[tier][i], byTier[tier][i + 1]);
      return { q, at: 0, wait: -1 };
    };
    const left = (x) => !!x && x.at < x.q.length;
    let active = [];
    for (const { own, kids } of byFrame) {
      const s = { own: queue(own), kids: kids.map(queue).filter(left), k: 0, ownTurn: true, cur: null };
      if (left(s.own) || s.kids.length) active.push(s);
    }
    // the queue a frame's next entry comes from: its own document's and its
    // children's in turn
    const pick = (s) => {
      if (s.cur) return s.cur;
      let kid = null;
      while (s.kids.length) {
        const i = s.k % s.kids.length;
        if (left(s.kids[i])) {
          kid = s.kids[i];
          s.k = i;
          break;
        }
        s.kids.splice(i, 1);
      }
      s.cur = (s.ownTurn || !kid) && left(s.own) ? s.own : kid;
      return s.cur;
    };
    const turn = [[], [], [], [], []];
    while (active.length) {
      for (const t of turn) t.length = 0;
      for (const s of active) {
        const c = pick(s);
        // the turns the queue's next entry still waits (-1: not counted
        // yet); a queue waits on its own (SEC36b 低-11), and a turn it
        // waits is the frame's turn spent, the next one going to the other
        // side (SEC37 低-13: the frame never takes more than about urlLength
        // a turn), so a child document's long element does not hold up the
        // frame's own document, nor the other way round
        if (!(c.wait >= 0)) c.wait = turnsOf(c.q[c.at + 1], c.q[c.at + 2]) - 1;
        if (c.wait > 0) {
          c.wait--;
          s.cur = null;
          s.ownTurn = c !== s.own;
          continue;
        }
        turn[c.q[c.at]].push(c.q[c.at + 1], c.q[c.at + 2]);
        c.at += 3;
        c.wait = -1;
        s.cur = null;
        if (c === s.own) s.ownTurn = false;
        else {
          s.ownTurn = true;
          s.k++;
        }
      }
      for (const t of turn) for (let i = 0; i < t.length; i += 2) (t[i] === 0 ? takeResource : takeElement)(t[i + 1]);
      active = active.filter((s) => left(s.own) || s.kids.some(left));
    }
  };
  takeTurns([0, 1, 2, 3]);
  takeTurns([4]);

  const outFrames = staying.map((f) => {
    let changed = false;
    const elements = [];
    for (const el of elementsOf(f)) {
      const e = elementOut.has(el) ? elementOut.get(el) : el;
      if (e !== el) changed = true;
      if (e) elements.push(e);
    }
    const resources = [];
    for (const r of resourcesOf(f)) {
      if (resourceOut.get(r) === false) changed = true;
      else resources.push(r);
    }
    // the addresses of frames the scan could not read (`opaque`, at most 20
    // a frame: the popup's site offers, which read host and path): one
    // longer than urlLength loses its fragment, or is left out
    let opaque = f.opaque;
    if (Array.isArray(opaque) && opaque.some((s) => typeof s === 'string' && s.length > L.urlLength)) {
      opaque = [];
      for (const s of f.opaque) {
        const u = typeof s === 'string' && s.length > L.urlLength ? withoutFragment(s) : s;
        if (typeof u === 'string' && u.length > L.urlLength) left.longUrls++;
        else opaque.push(u);
      }
      changed = true;
    }
    if (!changed) return f;
    const out = { ...f, elements, resources };
    if (opaque !== f.opaque) out.opaque = opaque;
    return out;
  });
  return { frames: outFrames, webRequests: outRequests, left };
}

// What a resource entry's address looks like, cheaply (boundScan's order
// only; detection classifies it properly, classifyUrl): 'manifest' for an
// HLS or DASH name (.m3u8, .m3u, .mpd, an Azure-style "manifest(format=…)",
// percent-encoded or not), 'file' for an audio or video file name or an
// entry a media element loaded, '' otherwise, and for a failed response
// (status 400 or more: never a track). The extension is the one of the
// path's last part (before any query or fragment), read as classifyUrl
// reads it; the rest of the path only when it holds a "(" or "%" (a format
// in the path, as is or percent-encoded); no URL parsing. So the hint is never weaker than what detection
// makes of the entry (SEC33d): an entry carries no Content-Type, so a
// manifest whose name has no extension is not one to detection either
// (only a request record, on its own allowance, can tell).
export function scanHint(url, initiatorType = '', status = undefined) {
  if (typeof status === 'number' && status >= 400) return '';
  const end = url.search(/[?#]/);
  const path = end < 0 ? url : url.slice(0, end);
  if (/[(%]/.test(path)) {
    const inPath = formatInPath(path);
    if (inPath === C_HLS || inPath === C_DASH) return 'manifest';
  }
  // the extension of the last part, as extensionOfPath reads it (a "\"
  // after the last "." makes it no extension either way)
  const dot = path.lastIndexOf('.');
  const ext = dot > path.lastIndexOf('/') + 1 && path.length - dot <= 7 ? extensionOfPath(path.slice(dot - 1)) : '';
  const cls = extensionClass(ext);
  if (cls === C_HLS || cls === C_DASH) return 'manifest';
  if (cls === C_FILE || initiatorType === 'video' || initiatorType === 'audio') return 'file';
  return '';
}

/**
 * Selector reads (the scan's `reads`, src/content/scan.js) held to
 * LIMITS.attrLength and attrNames (SEC33 低-3): each node's tag, id and
 * classes cut to attrLength characters, its attribute names to the first
 * attrNames, each cut likewise. The report keeps less of each (64
 * characters, 32 names), so it is the same; nothing else reads the nodes
 * (readResults takes count and value). Within the limits the reads come
 * back as they were (the same objects). Returns { reads, longValues }:
 * how many values were cut or names left out.
 */
export function boundReads(reads, limits = LIMITS) {
  const L = { ...LIMITS, ...limits };
  let longValues = 0;
  const cut = (v) => {
    if (typeof v !== 'string' || v.length <= L.attrLength) return v;
    longValues++;
    return v.slice(0, L.attrLength);
  };
  const node = (n) => {
    if (!n || typeof n !== 'object') return n;
    const before = longValues;
    const path = Array.isArray(n.path)
      ? n.path.map((p) => {
          if (!p || typeof p !== 'object') return p;
          const was = longValues;
          const out = { ...p, tag: cut(p.tag), id: cut(p.id), classes: Array.isArray(p.classes) ? p.classes.map(cut) : p.classes };
          return longValues === was ? p : out;
        })
      : n.path;
    let attrNames = n.attrNames;
    if (Array.isArray(attrNames)) {
      if (attrNames.length > L.attrNames) longValues += attrNames.length - L.attrNames;
      const was = longValues;
      const kept = attrNames.slice(0, L.attrNames).map(cut);
      if (longValues !== was || kept.length !== attrNames.length) attrNames = kept;
    }
    return longValues === before ? n : { ...n, path, attrNames };
  };
  const out = (Array.isArray(reads) ? reads : []).map((rd) => {
    if (!rd || !Array.isArray(rd.nodes)) return rd;
    const before = longValues;
    const nodes = rd.nodes.map(node);
    return longValues === before ? rd : { ...rd, nodes };
  });
  return { reads: Array.isArray(reads) ? (longValues ? out : reads) : reads, longValues };
}

function rank(pm) {
  if (!pm.parsed) return 2;
  if (pm.cls === C_DASH || pm.parsed.type === 'master') return 0;
  return 1;
}

function addSources(track, sources) {
  const set = new Set([...track.sources, ...sources]);
  track.sources = SOURCE_ORDER.filter((s) => set.has(s));
}

function baseTrack(d) {
  return {
    url: d.url,
    selector: d.selector || '',
    kind: d.kind || KIND_UNKNOWN,
    format: d.format,
    quality: d.quality || '',
    bandwidth: d.bandwidth || null,
    sizeBytes: d.sizeBytes ?? -1,
    sizeEstimated: false,
    durationSec: d.durationSec || null,
    sources: SOURCE_ORDER.filter((s) => (d.sources || []).includes(s)),
    frameIds: d.frameIds || [0],
    protected: !!d.protected,
    mime: d.mime || '',
    variants: d.variants || [],
    probeUrl: d.probeUrl || null,
    name: d.name || '',
    language: d.language || '',
    unreadable: !!d.unreadable,
  };
}

// The same rendition can come from two manifests (e.g. a master and a
// second master that shares an audio playlist).
function dedupeTracks(tracks) {
  const seen = new Map();
  for (const t of tracks) {
    const id = trackId(t);
    const cur = seen.get(id);
    if (!cur) seen.set(id, t);
    else {
      addSources(cur, t.sources);
      cur.protected = cur.protected || t.protected;
    }
  }
  return [...seen.values()];
}

export function trackId(t) {
  return t.selector ? `${t.url}#lf=${t.selector}` : t.url;
}

function finalize(t) {
  return {
    id: trackId(t),
    url: t.url,
    selector: t.selector || undefined,
    kind: t.kind,
    format: t.format,
    quality: t.quality || undefined,
    sizeBytes: t.sizeBytes > 0 ? t.sizeBytes : -1,
    sizeEstimated: t.sizeBytes > 0 ? t.sizeEstimated : false,
    durationSec: t.durationSec || undefined,
    sources: t.sources,
    frameIds: t.frameIds,
    protected: t.protected,
    mime: t.mime || undefined,
    variants: t.variants.length ? t.variants : undefined,
    name: t.name || undefined,
    language: t.language || undefined,
    unreadable: t.unreadable || undefined,
  };
}

const KIND_ORDER = { [KIND_VIDEO]: 0, [KIND_AUDIO]: 1, [KIND_UNKNOWN]: 2 };

function compareTracks(a, b) {
  return (
    (KIND_ORDER[a.kind] ?? 3) - (KIND_ORDER[b.kind] ?? 3) ||
    heightOf(b) - heightOf(a) ||
    b.sizeBytes - a.sizeBytes ||
    (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  );
}

function heightOf(t) {
  const m = /^(\d+)p$/.exec(t.quality || '');
  return m ? Number(m[1]) : 0;
}

function normalizeFetch(r) {
  return {
    ok: !!r.ok,
    status: Number.isFinite(r.status) ? r.status : 0,
    contentType: r.contentType || '',
    text: typeof r.text === 'string' ? r.text : '',
    error: r.error || undefined,
  };
}

function normalizeProbe(r) {
  return {
    ok: !!r.ok,
    status: Number.isFinite(r.status) ? r.status : 0,
    contentType: r.contentType || '',
    size: Number.isFinite(r.size) && r.size > 0 ? r.size : -1,
    error: r.error || undefined,
  };
}

/** Counts by kind, for the badge and the popup header. */
export function summarize(tracks) {
  const s = { total: tracks.length, audio: 0, video: 0, unknown: 0, protected: 0 };
  for (const t of tracks) {
    s[t.kind] = (s[t.kind] || 0) + 1;
    if (t.protected) s.protected++;
  }
  return s;
}

export { urlKey };
