// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// DASH manifest (MPD) parsing and splitting into tracks (SPEC-ext 通用轨道
// 探测: AdaptationSets counted by contentType as audio or video). Pure.

import { parseXml, localName, getAttr, childElements, textOf } from './xml.js';
import { kindOfCodecs, KIND_AUDIO, KIND_VIDEO, KIND_UNKNOWN, FORMAT_DASH } from './classify.js';
import { urlKey, resolveRef, resolveCost, basePath, baseRead } from './url.js';

/** ISO 8601 duration ("PT1H2M3.5S", "P1DT2H") to seconds; null if bad. */
export function parseIsoDuration(s) {
  const str = String(s || '').trim();
  const m = /^P(?:(\d+(?:\.\d+)?)Y)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)W)?(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$/.exec(
    str,
  );
  if (!m || str === 'P' || str.endsWith('T')) return null;
  const [, y, mo, w, d, h, mi, se] = m.map((x) => (x === undefined ? 0 : Number(x)));
  return y * 365 * 86400 + mo * 30 * 86400 + w * 7 * 86400 + d * 86400 + h * 3600 + mi * 60 + se;
}

function kindOfMime(mime) {
  const t = String(mime || '').toLowerCase();
  if (t.startsWith('audio/')) return KIND_AUDIO;
  if (t.startsWith('video/')) return KIND_VIDEO;
  if (t.startsWith('text/') || t.startsWith('image/') || t.startsWith('application/ttml')) return 'text';
  return '';
}

function kindOfAdaptationSet(as) {
  const ct = (getAttr(as, 'contentType') || '').toLowerCase();
  if (ct === 'audio' || ct === 'video') return ct;
  if (ct === 'text' || ct === 'image') return 'text';
  for (const cc of childElements(as, 'ContentComponent')) {
    const c = (getAttr(cc, 'contentType') || '').toLowerCase();
    if (c === 'video') return KIND_VIDEO;
    if (c === 'audio') return KIND_AUDIO;
  }
  const byMime = kindOfMime(getAttr(as, 'mimeType'));
  if (byMime) return byMime;
  const reps = childElements(as, 'Representation');
  for (const r of reps) {
    const k = kindOfMime(getAttr(r, 'mimeType'));
    if (k) return k;
  }
  const codecs = getAttr(as, 'codecs') || reps.map((r) => getAttr(r, 'codecs') || '').join(',');
  if (/^(stpp|wvtt)/i.test(codecs)) return 'text';
  const k = kindOfCodecs(codecs);
  return k;
}

// A base or address longer than TEMPLATE_LIMITS.urlLength is not used:
// every address resolved against a base carries it, so a long one would
// multiply the memory a page's MPD costs (SEC23c 中-4). What counts for a
// base is the part relative addresses read, its path without the query
// (url.js resolveRef, RFC 3986 §5.2.2), so a long signed query on the MPD
// address or a <BaseURL> costs nothing (SEC25 中-4); the whole base is
// kept below 2 × urlLength.
function usableBase(href) {
  return href.length <= 2 * TEMPLATE_LIMITS.urlLength && basePath(href).length <= TEMPLATE_LIMITS.urlLength;
}

function baseUrlOf(el, parentBase) {
  const b = childElements(el, 'BaseURL')[0];
  if (!b) return parentBase;
  const text = textOf(b).trim();
  if (text.length > TEMPLATE_LIMITS.urlLength) return parentBase;
  const u = resolveRef(text, parentBase, TEMPLATE_LIMITS.urlLength);
  if (!u) return parentBase;
  u.hash = '';
  return usableBase(u.href) ? u.href : parentBase;
}

/**
 * { key, cost }: urlKey of raw resolved against base (null when not http(s)
 * or too long) and what the attempt costs the byte budget (url.js
 * resolveCost), refused or not.
 */
function attemptUrl(raw, base) {
  if (!raw || raw.length > TEMPLATE_LIMITS.urlLength) return { key: null, cost: 0 };
  const abs = resolveRef(raw, base, TEMPLATE_LIMITS.urlLength);
  const key = abs ? urlKey(abs.href) : null;
  const cost = resolveCost(raw, base, TEMPLATE_LIMITS.urlLength, key);
  return { key: key && key.length <= TEMPLATE_LIMITS.urlLength ? key : null, cost };
}

function hasProtection(el) {
  return childElements(el, 'ContentProtection').length > 0;
}

// SegmentTemplate media / initialization strings come from the page's MPD,
// so the patterns built from them must not backtrack badly (SEC18 低-9).
// A template is literal text and $…$ identifiers; $RepresentationID$, $$
// and a known $Bandwidth$ are literal text (a known $Bandwidth%0<w>d$
// zero-padded to w digits, ISO/IEC 23009-1 5.3.9.4.4, for a width of 1 to
// TEMPLATE_LIMITS.digits as LectureFlow takes it; any other width leaves
// the value as it is), the rest ($Number$, $Time$,
// $SubNumber$, unknown names, an unknown $Bandwidth$) 1 to 32 digits.
// The pattern is the escaped literal text with one digit quantifier per
// run of k adjacent digit identifiers (\d{k,32k}: the strings k separate
// \d{1,32} would match). A template where two digit quantifiers are
// separated by digits only (\d+0\d+, from $Number$0$Time$ or a numeric
// $RepresentationID$ between two numbers) is refused: there the split
// between the two is ambiguous and the regex engine tries every split.
// Otherwise every quantifier is followed by a non-digit or the end, so it
// can only end at the end of its run of digits and the anchored pattern
// matches in linear time; the upper bound keeps each backtracking step
// short. A refused template gives null: no pattern, so its segment
// requests stay ordinary file candidates, as for a template that could
// not be resolved. The number of patterns is bounded as well (SEC23):
// per AdaptationSet and per MPD in parseMpd, per detection in
// childMatchers.
export const TEMPLATE_LIMITS = Object.freeze({
  templateLength: 1024, // characters of a media / initialization attribute
  identifiers: 8, // $…$ identifiers in one template ($$ included)
  digits: 32, // digits of one $Number$ / $Time$ (a uint64 has 20; Go allows %032d)
  repIdLength: 256, // a Representation id put into the pattern
  resolvedLength: 4096, // the resolved absolute template URL
  urlLength: 4096, // a request URL tested against a pattern; a base or address the MPD gives
  patternRepsPerSet: 32, // Representations of one AdaptationSet that get patterns
  patternsPerMpd: 128, // distinct patterns of one MPD
  matchers: 256, // distinct patterns compiled in one detection
  representations: 2000, // Representations of one MPD read at all
  adaptationSets: 512, // AdaptationSets of one MPD read at all (text ones included)
  segmentUrls: 50000, // SegmentURL / Initialization addresses one MPD's lists may give
  urlBytes: 16 * 1024 * 1024, // characters one MPD may resolve (ASCII: bytes); each attempt costs its longest of address, base read, result
  attrLength: 256, // a value a track keeps: codecs, Representation id, lang (real ones are under 100)
});

// A value a track keeps (SEC28 中-1): codecs and a Representation id longer
// than TEMPLATE_LIMITS.attrLength count as absent, not cut, so that no
// made-up value looks real; a Representation's own codecs override the
// AdaptationSet's even when absent this way (an empty one does not, as before).
const keptValue = (v) => (v !== undefined && v.length <= TEMPLATE_LIMITS.attrLength ? v : undefined);

const DIGITS = Symbol('digits');
const MARK = 'lfvar';

/** Literal text and identifier names of a template; null past the limits. */
function splitTemplate(template) {
  const t = String(template ?? '');
  if (t.length > TEMPLATE_LIMITS.templateLength) return null;
  const parts = [];
  const re = /\$([A-Za-z]*)(%0\d+d)?\$/g;
  let last = 0;
  let m;
  while ((m = re.exec(t))) {
    if (parts.length / 2 >= TEMPLATE_LIMITS.identifiers) return null;
    // the width of a %0<w>d format; 0 without one (the template is at most
    // templateLength characters, so the number is short)
    parts.push(t.slice(last, m.index), { id: m[1], width: m[2] ? Number(m[2].slice(2, -1)) : 0 });
    last = re.lastIndex;
  }
  parts.push(t.slice(last));
  return parts;
}

function identifierValue(part, repId, bandwidth) {
  const { id, width } = part;
  if (id === '') return '$';
  if (id === 'RepresentationID') return repId;
  if (id === 'Bandwidth' && bandwidth) {
    const s = String(bandwidth);
    return width >= 1 && width <= TEMPLATE_LIMITS.digits ? s.padStart(width, '0') : s;
  }
  return DIGITS; // Number, Time, SubNumber
}

/** Pattern source of literal strings and DIGITS; null when ambiguous. */
function itemsToRegex(items) {
  const seq = []; // { lit } | { digits: n }
  for (const it of items) {
    const prev = seq[seq.length - 1];
    if (it === DIGITS) {
      if (prev && prev.digits) prev.digits++;
      else seq.push({ digits: 1 });
    } else if (it !== '') {
      if (prev && prev.lit !== undefined) prev.lit += it;
      else seq.push({ lit: it });
    }
  }
  let out = '';
  let sinceDigits = null; // literal text since the last quantifier
  for (const s of seq) {
    if (s.digits) {
      if (sinceDigits !== null && !/[^0-9]/.test(sinceDigits)) return null;
      out += `\\d{${s.digits},${s.digits * TEMPLATE_LIMITS.digits}}`;
      sinceDigits = '';
    } else {
      out += escapeRe(s.lit);
      if (sinceDigits !== null) sinceDigits += s.lit;
    }
  }
  return out;
}

/**
 * Turns a SegmentTemplate media/initialization string into a RegExp
 * source (unanchored, unresolved); null for a template that is too long,
 * has too many identifiers or would be ambiguous (see above).
 */
export function templateToRegex(template, repId, bandwidth) {
  const parts = splitTemplate(template);
  const rep = String(repId || '');
  if (!parts || (usesRepId(parts) && rep.length > TEMPLATE_LIMITS.repIdLength)) return null;
  return itemsToRegex(parts.map((p) => (typeof p === 'string' ? p : identifierValue(p, rep, bandwidth))));
}

/**
 * A RegExp source matching the absolute URLs of a template relative to
 * base: each $…$ identifier is swapped for a marker, the URL resolved, and
 * the markers turned into their patterns. null when the template is
 * refused (see above) or does not resolve.
 */
export function resolvedTemplateRegex(template, base, repId, bandwidth) {
  const parts = splitTemplate(template);
  const rep = String(repId || '');
  if (!parts || (usesRepId(parts) && rep.length > TEMPLATE_LIMITS.repIdLength)) return null;
  const ids = [];
  const marked = parts.map((p) => {
    if (typeof p === 'string') return p;
    ids.push(p);
    return `${MARK}${ids.length - 1}lf`;
  }).join('');
  // the part of base the resolution reads (url.js resolveRef): no query
  // unless the template is empty or a fragment, nothing for an absolute one
  const used = baseRead(marked, base);
  if (used.length > TEMPLATE_LIMITS.resolvedLength) return null;
  // The markers must not be confused with the page's own text: a template
  // or base that already says "lfvar" (in any case) is refused. The URL
  // parser can still make one up (percent-decoding in the host); then the
  // check below refuses it, or at worst the pattern is wrong but, like
  // every pattern here, linear.
  if ((String(template) + used).toLowerCase().includes(MARK)) return null;
  const abs = resolveRef(marked, base);
  if (!abs) return null;
  abs.hash = '';
  const href = abs.href;
  if (href.length > TEMPLATE_LIMITS.resolvedLength) return null;
  // Every "lfvar" left in the URL must be one of our markers, each once
  // (a marker can vanish, e.g. with the fragment or a "..").
  const items = [];
  const seen = new Set();
  let pos = 0;
  for (;;) {
    const at = href.indexOf(MARK, pos);
    if (at < 0) break;
    const m = /^lfvar(\d)lf/.exec(href.slice(at, at + MARK.length + 3));
    if (!m || Number(m[1]) >= ids.length || seen.has(m[1])) return null;
    seen.add(m[1]);
    items.push(href.slice(pos, at), identifierValue(ids[Number(m[1])], rep, bandwidth));
    pos = at + m[0].length;
  }
  items.push(href.slice(pos));
  const out = itemsToRegex(items);
  return out === null ? null : '^' + out + '$';
}

function usesRepId(parts) {
  return parts.some((p) => typeof p !== 'string' && p.id === 'RepresentationID');
}

/**
 * Whether a source has the shape resolvedTemplateRegex gives: ^ and $
 * around literal or escaped characters and \d{k,32k} quantifiers (k up to
 * TEMPLATE_LIMITS.identifiers), with a non-digit between any two
 * quantifiers. One pass over the source.
 */
export function isTemplatePattern(source) {
  const s = String(source);
  if (s.length > 2 * TEMPLATE_LIMITS.resolvedLength + TEMPLATE_LIMITS.identifiers * (2 * TEMPLATE_LIMITS.repIdLength + 16)) return false;
  if (!s.startsWith('^') || !s.endsWith('$')) return false;
  const end = s.length - 1;
  let since = null; // characters since the last quantifier
  let i = 1;
  while (i < end) {
    if (s[i] === '\\' && s[i + 1] === 'd') {
      const q = /^\\d\{(\d{1,4}),(\d{1,5})\}/.exec(s.slice(i, i + 16));
      if (!q || (since !== null && !/[^0-9]/.test(since))) return false;
      const lo = Number(q[1]);
      if (lo < 1 || lo > TEMPLATE_LIMITS.identifiers || Number(q[2]) !== lo * TEMPLATE_LIMITS.digits) return false;
      since = '';
      i += q[0].length;
      continue;
    }
    let c;
    if (s[i] === '\\') {
      c = s[i + 1];
      if (i + 1 >= end || !/[.*+?^${}()|[\]\\]/.test(c)) return false;
      i += 2;
    } else {
      c = s[i];
      if (/[.*+?^${}()|[\]]/.test(c)) return false;
      i += 1;
    }
    if (since !== null) since += c;
  }
  return i === end;
}

function childPatternMatcher(source) {
  if (!isTemplatePattern(source)) return null;
  let re;
  try {
    re = new RegExp(source);
  } catch {
    return null;
  }
  return (url) => typeof url === 'string' && url.length <= TEMPLATE_LIMITS.urlLength && re.test(url);
}

/**
 * The tracks' childPatterns for one detection: each distinct source
 * compiled once (the first track to bring it owns it), at most
 * TEMPLATE_LIMITS.matchers of them. Sources that are not template
 * patterns, or come past the limit, are dropped: their requests stay
 * file candidates. find(url) gives the owner of the first match.
 */
export function childMatchers() {
  const list = []; // [test, owner]
  const sources = new Set();
  return {
    add(source, owner) {
      if (sources.has(source) || list.length >= TEMPLATE_LIMITS.matchers) return false;
      const test = childPatternMatcher(source);
      if (!test) return false;
      sources.add(source);
      list.push([test, owner]);
      return true;
    },
    find(url) {
      for (const [test, owner] of list) if (test(url)) return owner;
      return undefined;
    },
    get size() {
      return list.length;
    },
  };
}

function listItems(list) {
  return { segs: childElements(list, 'SegmentURL'), init: childElements(list, 'Initialization')[0] };
}

/** Largest positive pick(item), 0 when none (no argument spreading). */
function maxOf(items, pick) {
  let max = 0;
  for (const it of items) {
    const v = pick(it) || 0;
    if (v > max) max = v;
  }
  return max;
}

function escapeRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Parses an MPD. Returns {
 *   type: 'static' | 'dynamic' | 'invalid', durationSec,
 *   adaptationSets: [{ period, ordinal, kind, lang, protected, maxHeight,
 *     maxBandwidth, representations: [{ id, bandwidth, width, height, codecs }],
 *     childUrls: [url], childPatterns: [regexSource] }]
 * }
 * Only the first TEMPLATE_LIMITS.adaptationSets AdaptationSets and
 * representations Representations are read (AdaptationSets after either
 * limit are left out); only the first patternRepsPerSet of an
 * AdaptationSet, and patternsPerMpd distinct patterns in all, get segment
 * patterns; a SegmentList is read only while its addresses fit in what is
 * left of segmentUrls, else that Representation has no segment list.
 * Real MPDs have a few dozen Representations and a few thousand segments
 * per list.
 * opts.budget: { left } shared by one detection (detect.js); the MPD
 * resolves at most min(TEMPLATE_LIMITS.urlBytes, budget.left) characters
 * and takes what it spent off budget.left. Without it, only the MPD's own
 * limit applies.
 */
export function parseMpd(text, baseUrl, opts = {}) {
  let root;
  try {
    root = parseXml(text);
  } catch {
    return { type: 'invalid', durationSec: null, adaptationSets: [], drm: false };
  }
  return parseMpdTree(root, baseUrl, opts);
}

/** parseMpd on a tree from parseXml (the root element). */
export function parseMpdTree(root, baseUrl, opts = {}) {
  if (localName(root.name) !== 'MPD') return { type: 'invalid', durationSec: null, adaptationSets: [], drm: false };
  const type = (getAttr(root, 'type') || 'static').toLowerCase() === 'dynamic' ? 'dynamic' : 'static';
  let durationSec = parseIsoDuration(getAttr(root, 'mediaPresentationDuration') || '');
  // an MPD address past the limit is no base: only absolute addresses resolve
  const ownBase = typeof baseUrl === 'string' && usableBase(baseUrl) ? baseUrl : undefined;
  const mpdBase = baseUrlOf(root, ownBase);
  const periods = childElements(root, 'Period');
  if (durationSec === null) {
    const sum = periods.reduce((acc, p) => acc + (parseIsoDuration(getAttr(p, 'duration') || '') || 0), 0);
    durationSec = sum > 0 ? sum : null;
  }
  const sets = [];
  let repsLeft = TEMPLATE_LIMITS.representations;
  let setsLeft = TEMPLATE_LIMITS.adaptationSets;
  let segUrlsLeft = TEMPLATE_LIMITS.segmentUrls;
  const shared = opts && opts.budget;
  const urlBytesStart = shared ? Math.max(0, Math.min(TEMPLATE_LIMITS.urlBytes, shared.left)) : TEMPLATE_LIMITS.urlBytes;
  let urlBytesLeft = urlBytesStart;
  const mpdPatterns = new Set();
  periods.forEach((period, pIndex) => {
    // a Period's base is read only for an AdaptationSet that is (SEC25b)
    let periodBase;
    let periodBaseRead = false;
    const periodBaseOf = () => {
      if (!periodBaseRead) {
        periodBase = baseUrlOf(period, mpdBase);
        periodBaseRead = true;
      }
      return periodBase;
    };
    const ordinals = new Map();
    for (const as of childElements(period, 'AdaptationSet')) {
      if (repsLeft <= 0 || setsLeft <= 0) break;
      setsLeft--;
      const kind = kindOfAdaptationSet(as);
      if (kind === 'text') continue;
      const lang = (getAttr(as, 'lang') || '').toLowerCase();
      // The lang is part of the track's selector, which LectureFlow matches
      // against the MPD as it is (internal/fetch/dash.go): an AdaptationSet
      // with a longer one is not offered at all rather than renamed.
      if (lang.length > TEMPLATE_LIMITS.attrLength) continue;
      const ordKey = `${kind}\u0000${lang}`;
      const ordinal = ordinals.get(ordKey) || 0;
      ordinals.set(ordKey, ordinal + 1);
      const asBase = baseUrlOf(as, periodBaseOf());
      const asTemplate = childElements(as, 'SegmentTemplate')[0];
      const asList = childElements(as, 'SegmentList')[0];
      const asListItems = asList && listItems(asList);
      const asListBases = new Set(); // repBases the shared SegmentList was read against
      const reps = [];
      const childUrls = [];
      const childPatterns = [];
      let prot = hasProtection(as);
      for (const r of childElements(as, 'Representation')) {
        if (repsLeft <= 0) break;
        repsLeft--;
        const id = getAttr(r, 'id') || '';
        const bandwidth = Number(getAttr(r, 'bandwidth')) || null;
        const height = Number(getAttr(r, 'height') || getAttr(as, 'height')) || null;
        const width = Number(getAttr(r, 'width') || getAttr(as, 'width')) || null;
        const ownCodecs = getAttr(r, 'codecs');
        const codecs = ownCodecs ? keptValue(ownCodecs) : keptValue(getAttr(as, 'codecs'));
        // the id as given still goes into templates (repIdLength refuses a long one there)
        reps.push({ id: keptValue(id) || '', bandwidth, width, height, codecs: codecs || '' });
        if (hasProtection(r)) prot = true;
        const repBase = baseUrlOf(r, asBase);
        const template = childElements(r, 'SegmentTemplate')[0] || asTemplate;
        const ownList = childElements(r, 'SegmentList')[0];
        const list = ownList || asList;
        if (template) {
          // Past the limits the segments stay file candidates, as for a
          // template that gives no pattern.
          if (reps.length > TEMPLATE_LIMITS.patternRepsPerSet) continue;
          for (const a of ['media', 'initialization']) {
            const t = getAttr(template, a);
            if (!t || mpdPatterns.size >= TEMPLATE_LIMITS.patternsPerMpd) continue;
            const re = resolvedTemplateRegex(t, repBase, id, bandwidth);
            if (re && !mpdPatterns.has(re)) {
              mpdPatterns.add(re);
              childPatterns.push(re);
            }
          }
        } else if (list) {
          if (!ownList) {
            // the AdaptationSet's list: read once per base, not per Representation
            if (asListBases.has(repBase)) continue;
            asListBases.add(repBase);
          }
          const { segs, init } = ownList ? listItems(ownList) : asListItems;
          // a list past the MPD's budget is not read: no segment list (SEC23b 中-3)
          const cost = segs.length + (init ? 1 : 0);
          if (cost > segUrlsLeft) continue;
          // nor is one whose addresses pass what is left of urlBytes (SEC23c 中-4)
          if (urlBytesLeft <= 0) continue;
          const got = [];
          let bytes = 0;
          for (let i = 0; i <= segs.length; i++) {
            const raw = i < segs.length ? getAttr(segs[i], 'media') : init && getAttr(init, 'sourceURL');
            const { key, cost } = attemptUrl(raw, repBase);
            bytes += cost;
            if (bytes > urlBytesLeft) break;
            if (key) got.push(key);
          }
          if (bytes > urlBytesLeft) {
            // the work is done once: no later list is read (SEC25 中-1)
            urlBytesLeft = 0;
            continue;
          }
          segUrlsLeft -= cost;
          urlBytesLeft -= bytes;
          for (const key of got) childUrls.push(key);
        } else if (childElements(r, 'BaseURL').length > 0) {
          // SegmentBase or a bare BaseURL: the representation is one file
          // the player reads with byte ranges.
          const key = repBase ? urlKey(repBase) : null;
          if (key && key.length <= TEMPLATE_LIMITS.urlLength && key.length <= urlBytesLeft) {
            urlBytesLeft -= key.length;
            childUrls.push(key);
          }
        }
      }
      sets.push({
        period: pIndex,
        ordinal,
        kind: kind || KIND_UNKNOWN,
        lang,
        protected: prot,
        maxHeight: maxOf(reps, (r) => r.height) || null,
        maxBandwidth: maxOf(reps, (r) => r.bandwidth) || null,
        representations: reps,
        childUrls,
        childPatterns,
      });
    }
  });
  if (shared) shared.left -= urlBytesStart - urlBytesLeft;
  return { type, durationSec, adaptationSets: sets, drm: sets.some((s) => s.protected) };
}

/**
 * Tracks of one MPD: one per AdaptationSet, merged across Periods by kind,
 * language and order. The track url is the MPD's; `selector` tells which
 * AdaptationSet (the bridge protocol has no field for it yet — see NOTES).
 */
export function dashTracks(parsed, manifestUrl) {
  const url = urlKey(manifestUrl);
  if (parsed.type === 'invalid') return [];
  const merged = new Map();
  for (const s of parsed.adaptationSets) {
    const key = `${s.kind}\u0000${s.lang}\u0000${s.ordinal}`;
    const cur = merged.get(key);
    if (!cur) {
      merged.set(key, { ...s, representations: [...s.representations], childUrls: [...s.childUrls], childPatterns: [...s.childPatterns] });
    } else {
      cur.protected = cur.protected || s.protected;
      cur.maxHeight = Math.max(cur.maxHeight || 0, s.maxHeight || 0) || null;
      cur.maxBandwidth = Math.max(cur.maxBandwidth || 0, s.maxBandwidth || 0) || null;
      for (const u of s.childUrls) cur.childUrls.push(u);
      for (const p of s.childPatterns) cur.childPatterns.push(p);
    }
  }
  const tracks = [];
  for (const s of merged.values()) {
    const sorted = [...s.representations].sort((a, b) => (b.height || 0) - (a.height || 0) || (b.bandwidth || 0) - (a.bandwidth || 0));
    tracks.push({
      url,
      selector: `${s.kind}:${s.lang || '-'}:${s.ordinal}`,
      kind: s.kind,
      format: FORMAT_DASH,
      quality: s.kind === KIND_VIDEO && s.maxHeight ? `${s.maxHeight}p` : '',
      bandwidth: s.maxBandwidth,
      variants: sorted.map((r) => ({ id: r.id, bandwidth: r.bandwidth, width: r.width, height: r.height, codecs: r.codecs })),
      protected: s.protected,
      childUrls: [...new Set(s.childUrls)],
      childPatterns: [...new Set(s.childPatterns)],
      probeUrl: null,
      durationSec: parsed.durationSec,
      name: '',
      language: s.lang,
    });
  }
  return tracks;
}
