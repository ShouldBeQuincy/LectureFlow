// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the background keeps of a detection, and how much of it a card shows
// (SEC25 低-2). Detection itself is bounded per run (detect.js LIMITS,
// hls.js HLS_LIMITS, dash.js TEMPLATE_LIMITS); these limits bound what
// stays in memory afterwards and what reaches a card.
//
// What a stored detection holds (src/background/detector.js, memory only,
// per tab and per in-page frame; cleared when the tab closes, navigates to
// another address, or the background is unloaded):
//   - the tracks, which the cards show and a send reads (their URLs): the
//     shown ones always, at most tracksKept in all;
//   - the scan (raw.frames, raw.webRequests), held to detect.js LIMITS
//     (boundScan) before detection, for the same report, so only the
//     tab's top-level detection keeps it: up to totalScanChars of
//     addresses (scanChars) over all tabs, past it the largest (evictTexts)
//     giving its scan up first (and its texts with it: its report detects
//     again anyway);
//   - at most frameResults in-page detections a tab (each frame's in-page
//     card keeps its own), the oldest going first;
//   - raw.detection.fetches: every manifest and media playlist text it
//     read, for one use only, the redacted report of the tab's top-level
//     detection (src/lib/report.js), which leaves out any text longer than
//     textChars anyway (`truncated`). So a text longer than that is not
//     kept at all; the rest is kept up to tabTextChars per tab, and up to
//     totalTextChars over all tabs, the largest (evictTexts) giving its
//     texts up first. A text left out is marked `truncated`, as the report marks
//     it; a report of a tab whose texts were given up detects again.
// Every limit is far above a real lecture page: a master playlist is a few
// KB, a media playlist of a four-hour lecture in 2 s segments some 0.7 MB
// (already more than textChars), a page has a few to a few dozen tracks.
// Pure; no imports, so the in-page bundle can take the constant.

export const RETAIN_LIMITS = Object.freeze({
  // tracks a card lists (popup, in-page card, confirmation window); the
  // rest are counted ("N more not shown") and cannot be ticked. Real pages:
  // a few to a few dozen.
  tracksShown: 200,
  // tracks a stored detection keeps at all (SEC28 低-1): the ones a card
  // shows first (src/lib/send.js shownTracks, with this limit in place of
  // tracksShown, so the shown ones are the same), then the others in list
  // order; the rest are given up and counted (the card's "N more not
  // shown", the report's `limits.tracks`). The counts in the header and on
  // the badge are of all tracks.
  tracksKept: 1000,
  // characters of one manifest text the report takes (report.js); a longer
  // one is never used, so never kept
  textChars: 512 * 1024,
  // characters of manifest text one tab keeps for its report (its frames'
  // in-page detections keep none: no report is made of them). V8 stores
  // ASCII text one byte a character, other text two.
  tabTextChars: 8 * 1024 * 1024,
  // characters of manifest text all tabs keep together
  totalTextChars: 32 * 1024 * 1024,
  // address characters of the scans all detections keep together for the
  // report (scanChars; one scan holds at most about 20 Mi: detect.js
  // LIMITS.scanChars and requestChars, and the page's own address)
  totalScanChars: 64 * 1024 * 1024,
  // in-page detections one tab keeps (one per frame whose in-page card
  // detected; a real page has one or two players)
  frameResults: 16,
});

/**
 * The address characters a stored scan holds (what boundScan counts, and
 * the frames' own addresses): frames, media elements and their <source>s,
 * resource entries, request records.
 */
export function scanChars(frames, webRequests) {
  const len = (s) => (typeof s === 'string' ? s.length : 0);
  let n = 0;
  for (const f of Array.isArray(frames) ? frames : []) {
    if (!f) continue;
    n += len(f.url);
    for (const el of Array.isArray(f.elements) ? f.elements : []) {
      if (!el) continue;
      n += len(el.src) + len(el.currentSrc);
      for (const s of Array.isArray(el.sources) ? el.sources : []) n += s ? len(s.src) : 0;
    }
    for (const r of Array.isArray(f.resources) ? f.resources : []) n += r ? len(r.url) : 0;
  }
  for (const w of Array.isArray(webRequests) ? webRequests : []) n += w ? len(w.url) : 0;
  return n;
}

/** The length of a fetched text as the report measures it: no BOM, no leading white space. */
export function reportTextLength(text) {
  return String(text ?? '')
    .replace(/^\ufeff/, '')
    .trimStart().length;
}

/** Characters of manifest text a detection's fetches hold. */
export function keptTextChars(detection) {
  let n = 0;
  for (const f of (detection && detection.fetches) || []) if (typeof f.text === 'string') n += f.text.length;
  return n;
}

const dropped = (f) => ({ ...f, text: '', truncated: true });
const holdsText = (f) => f && f.purpose !== 'file' && typeof f.text === 'string' && f.text !== '';

// The longest string detachedCopy deduplicates: V8 computes the hash of a
// string of up to String::kMaxHashCalcLength (16383) characters from its
// contents and of a longer one from its length only, so a Map keyed by
// many long strings of one length degrades to comparing them pairwise
// (SEC28b 中-3: 1000 addresses of 16400 characters, detection 1.6 → 2.8 s).
// Real addresses and values are far shorter; a longer one is copied at
// every place that holds it instead.
export const DEDUP_MAX_LENGTH = 16383;

/**
 * A copy of plain data (objects, arrays, primitives) that shares no string
 * with the original: every distinct string is copied once, by value, and
 * every place that held it gets that one copy; objects and arrays are
 * copied once each, so shared ones stay shared. The point: a string a
 * parser took out of a manifest (a CODECS value, an address) can be a V8
 * slice that keeps the whole manifest text alive; a copy keeps only
 * itself. structuredClone would detach too, but writes a string once per
 * reference, so one value shared by 2000 representations became 2000
 * copies (SEC28 高-1). Cost: one pass over the graph plus one copy of each
 * distinct string; the copy is never larger than the original's distinct
 * strings and objects. Anything other than plain data (none is expected)
 * is copied with structuredClone.
 * A string longer than DEDUP_MAX_LENGTH is copied at every place that
 * holds it, without the table (SEC28 中-3).
 * copyString: tests count the copies. Returns { value, strings, chars,
 * long }: how many distinct strings went through the table, the length of
 * everything copied, and how many copies of long strings were made.
 */
export function detachedCopy(root, copyString = (s) => structuredClone(s)) {
  const strings = new Map();
  const objects = new Map();
  let chars = 0;
  let long = 0;
  const walk = (v) => {
    if (typeof v === 'string') {
      // V8 hashes a longer string by its length alone, so many long strings
      // of one length would make every lookup compare them one by one
      if (v.length > DEDUP_MAX_LENGTH) {
        long++;
        chars += v.length;
        return copyString(v);
      }
      let c = strings.get(v);
      if (c === undefined) {
        c = copyString(v);
        strings.set(v, c);
        chars += v.length;
      }
      return c;
    }
    if (v === null || typeof v !== 'object') return v;
    const seen = objects.get(v);
    if (seen !== undefined) return seen;
    if (Array.isArray(v)) {
      const out = new Array(v.length);
      objects.set(v, out);
      for (let i = 0; i < v.length; i++) out[i] = walk(v[i]);
      return out;
    }
    const proto = Object.getPrototypeOf(v);
    if (proto !== Object.prototype && proto !== null) {
      const out = structuredClone(v);
      objects.set(v, out);
      return out;
    }
    const out = {};
    objects.set(v, out);
    for (const k of Object.keys(v)) out[k] = walk(v[k]);
    return out;
  };
  const value = walk(root);
  return { value, strings: strings.size, chars, long };
}

/**
 * A detection as the background stores it: its fetches keep their texts in
 * call order while they fit `allowance` characters (each at most
 * limits.textChars); the others are emptied and marked `truncated`.
 * Tracks are untouched. The result is a detached copy (detachedCopy) of
 * detection and named together, so that a text left out is not kept alive
 * by a string taken out of it, and tracks the two share stay one object.
 * Returns { detection, named, keptChars, droppedTexts, copiedStrings,
 * copiedChars }.
 */
export function retainDetection({ detection, named = null }, allowance, limits = RETAIN_LIMITS) {
  let left = Math.max(0, Number(allowance) || 0);
  let keptChars = 0;
  let droppedTexts = 0;
  const fetches = ((detection && detection.fetches) || []).map((f) => {
    if (!holdsText(f)) return f;
    const len = f.text.length;
    if (reportTextLength(f.text) <= limits.textChars && len <= left) {
      left -= len;
      keptChars += len;
      return f;
    }
    droppedTexts++;
    return dropped(f);
  });
  const copy = detachedCopy({ detection: { ...detection, fetches }, named });
  return { detection: copy.value.detection, named: copy.value.named, keptChars, droppedTexts, copiedStrings: copy.strings, copiedChars: copy.chars };
}

/** The detection with every manifest text given up (marked `truncated`). */
export function dropTexts(detection) {
  return { ...detection, fetches: ((detection && detection.fetches) || []).map((f) => (holdsText(f) ? dropped(f) : f)) };
}

/**
 * Which stored detections give up their texts (or scans) so that all of
 * them keep at most `cap` characters, never `keep` (the one just stored):
 * first the other detections of keep's own tab, then the others, each
 * group the largest first, of equal ones the oldest (SEC36 低-10: a few
 * large pages do not push every smaller tab's out first). entries:
 * [{ key, at, chars }]; a key is a tab id or "tabId:frameId". Returns the
 * keys.
 */
export function evictTexts(entries, keep, cap = RETAIN_LIMITS.totalTextChars) {
  let total = 0;
  for (const e of entries) total += e.chars;
  const out = [];
  const tabOf = (k) => String(k).split(':')[0];
  const own = (e) => (tabOf(e.key) === tabOf(keep) ? 0 : 1);
  const older = entries.filter((e) => e.key !== keep && e.chars > 0).sort((a, b) => own(a) - own(b) || b.chars - a.chars || a.at - b.at);
  for (const e of older) {
    if (total <= cap) break;
    out.push(e.key);
    total -= e.chars;
  }
  return out;
}
