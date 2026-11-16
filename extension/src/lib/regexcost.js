// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What a platform profile's regular expressions may cost (security review
// L7, FB1 L-1, SEC18): a static check, run before any of them is executed
// — by the extension's import, its loading of local profiles, checkProfile
// itself and the repository's contribution check (scripts/profilecheck).
// No imports, so src/lib/blocklist.js can use it too. Pure.

// The longest input a profile regex is run on: page paths, addresses
// without their query, and short page texts; anything longer simply does
// not match. Measured with each regex warmed up first (src/lib/regexguard.js
// warmRegex; V8 runs a new regex's first match on less than 1000
// characters in its bytecode interpreter, 5 to 8 times slower), the
// dearest shapes allowed take about 0.4 to 0.7 s on 999 or 1024
// characters (".*x.*y": 0.16 s); some stacked choices, about 1.2 to 1.5 s
// (SEC22 L-B, SEC27 L-3; left to the runtime guard).
export const MAX_REGEX_INPUT = 1024;

// A bounded quantifier that may take more than this many different counts
// ("{0,2000}") backtracks like an unbounded one and is counted as one.
const WIDE_SPAN = 16;
// The most "wide" quantifiers in one expression.
const MAX_WIDE = 2;
// Ceiling on the estimated cost, as log2 of the paths tried (SEC18 中-1,
// measured: about 2^29 paths a second): log2(n) start positions, times
// n^k / (k + 1)! ways for k wide quantifiers to share n characters, times
// 2^B for the choices B of "?", "{n,m}" and "|". With n = 1024: no wide
// quantifier leaves B ≤ 18, one B ≤ 9, two B ≤ 0.
const MAX_COST_LOG2 = 28;
// A leading "^" saves the log2(n) start positions; measured, the saving is
// a little less (one wide quantifier and 19 "?" anchored: 1.35 s), so 8.
const ANCHORED_BONUS = 8;

// The character a written-out text element takes (case folded, as the
// profile regexes run with "i"), or null when it can take more than one:
// ".", a class, \d \D \w \W \s \S. tok is the element as written.
const ESCAPED = { t: '\t', n: '\n', r: '\r', f: '\f', v: '\v', 0: '\0' };
function textKey(tok) {
  if (tok.length === 1) return tok === '.' ? null : tok.toLowerCase();
  if (tok[0] !== '\\') return null;
  const e = tok[1];
  if ('dDwWsS'.includes(e)) return null;
  if ((e === 'u' || e === 'x') && tok.length > 2) return String.fromCharCode(parseInt(tok.slice(2), 16)).toLowerCase();
  if (e === 'c' && tok.length > 2) return String.fromCharCode(tok.charCodeAt(2) % 32);
  return Object.hasOwn(ESCAPED, e) ? ESCAPED[e] : e.toLowerCase();
}

/**
 * The capture groups of a regex, read from its source without running it
 * (SEC22b: running even "x|" on '' tries x first): "(…)" and "(?<name>…)",
 * not inside a class or after "\".
 */
export function captureGroupCount(src) {
  const s = String(src);
  let n = 0;
  let i = 0;
  while (i < s.length) {
    const c = s[i];
    if (c === '\\') i += 2;
    else if (c === '[') {
      // as JavaScript reads it: the first "]" closes the class ("[]", "[^]")
      i++;
      if (s[i] === '^') i++;
      while (i < s.length && s[i] !== ']') i += s[i] === '\\' ? 2 : 1;
      i++;
    } else {
      if (c === '(' && (s[i + 1] !== '?' || (s[i + 2] === '<' && s[i + 3] !== '=' && s[i + 3] !== '!'))) n++;
      i++;
    }
  }
  return n;
}

function log2Factorial(k) {
  let s = 0;
  for (let i = 2; i <= k; i++) s += Math.log2(i);
  return s;
}

/**
 * The choices ("?", "{n,m}", "|") a regex with k wide quantifiers may still
 * have, as log2, when it is tried from every start position. One anchored
 * with a leading "^" (and no top-level "|") is tried from the first
 * character only and gets ANCHORED_BONUS more: 26 / 17 / 8.
 */
export function choiceBudget(k) {
  const n = Math.log2(MAX_REGEX_INPUT);
  return MAX_COST_LOG2 - (k + 1) * n + log2Factorial(k + 1);
}

const GROUP_SYNTAX = 'unsupported group syntax: only "(…)", "(?:…)", "(?=…)", "(?!…)", "(?<=…)", "(?<!…)" and "(?<name>…)"';
const REPEATED = 'a repeated group must have a fixed width: no "?", "*", "+", "{n,m}", "|", anchors or lookarounds inside it (catastrophic backtracking)';
const ADJACENT = 'two "*", "+", "{n,}" or wide "{n,m}" in a row, with nothing required between them (slow backtracking)';
// Lookaheads and lookbehinds run again at every position a wide quantifier
// tries; their content is kept short (SEC18e 低-14).
export const MAX_ASSERTION_WIDTH = 64;
const ASSERTIONS = `lookaheads and lookbehinds may hold at most ${MAX_ASSERTION_WIDTH} characters in all (slow backtracking)`;
const BEHIND_WIDE = 'a lookbehind may not hold "*", "+", "{n,}" or wide "{n,m}" (slow backtracking)';
// An empty alternative takes no character and doubles the work of what
// follows it: such choices stack up past what the cost check sees (SEC22 L-B).
const EMPTY_BRANCH = 'an empty alternative ("a|", "(?:|x)"): write an optional group instead, "(?:x)?" (slow backtracking)';
// Alternatives that take no character ("^", "$", "\b", "\B", lookarounds,
// "{0}", empty groups) can all hold at the same place: a group with two of
// them tries everything after it twice, and such groups stack. V8 pays for
// that while it compiles the regex, on its first runs, whatever the input
// ("^" + "(?:^|$)" x 26: 4.6 s on "", 3.5 s on "/v/1"; SEC22c R-1); they
// also get round the empty-alternative rule ("(?:(?=)|(?=))", SEC22b S-2).
// log2 of their count, summed over the groups that have two or more, may be
// at most 1: one "(?:(?=/)|$)" is fine. A lookaround's own alternatives are
// exempt (it stops at the first that holds), and so are the top level's.
const MAX_ZERO_WIDTH_LOG2 = 1;
const ZERO_WIDTH = 'two alternatives that take no character ("^", "$", "\\b", "\\B", lookarounds, "{0}") in more than one group, or three in one group, as in "(?:^|$)(?:(?=/)|$)": each such group repeats everything after it (slow backtracking); one alternative of that kind in a group, as in "(?:/|$)", is fine. To fix it, put such a choice inside one lookahead, not "(?:(?=/)|$)" but "(?=/|$)", or drop an alternative that cannot hold there ("^" after other content)';

/**
 * Why a profile regex is too costly to run on page data, or ''. Besides
 * the schema's 300 characters:
 *   - no backreferences; groups only "(…)", "(?:…)", "(?=…)", "(?!…)",
 *     "(?<=…)", "(?<!…)" and "(?<name>…)" (no modifier groups "(?i:…)");
 *   - a repeated group ("*", "+", "{n,}", "{n,m}" with m > 1) has a fixed
 *     width: single-character atoms (a character, ".", a class, an escape
 *     other than \b \B), "{n}" (n ≥ 1) on those or on fixed-width groups,
 *     and fixed-width groups — nothing else: no "?", "*", "+", "{n,m}" with
 *     n ≠ m, "{0}", "|", anchors, lookarounds or empty groups. Each
 *     repetition then takes its characters in exactly one way:
 *     "(?:[0-9a-f]{4}-){3}" is fine, "(a+)+", "(a|aa)*", "(?:a{1,2}){3}"
 *     are not;
 *   - "wide" quantifiers — "*", "+", "{n,}" and "{n,m}" with m − n > 16 —
 *     at most two, never two in a row with nothing required between them
 *     ("\d*\d*", "\w+\s?\w+", ".{0,999}.{0,999}");
 *   - the other choices — log2 of each "?" / "{n,m}" (m − n + 1 counts) and
 *     of each group's branches — plus log2 of the widest choice segment W
 *     (from a choice point — what a "?" or "{n,m}" applies to, a group with
 *     "|", a fixed-width group counted by its full width — over everything
 *     required after it, lookarounds included, up to the next choice
 *     point, wide quantifier or the end: "(?:.{30})?" and ".?.{29}" are
 *     both 30; 1 at least; a "*" / "+" / "{n,}" inside counted once; SEC18c
 *     中-4) add up to at most
 *     choiceBudget(k) for k wide quantifiers: 18 with none, 9 with one, 0
 *     with two; 8 more when the regex starts with "^" and has no
 *     top-level "|". Each path a choice opens compares up to W characters
 *     (SEC18 中-3: "(?:.{14})?" x 18 took 10.9 s).
 *   - what each wide quantifier requires after it — its wide segment, up to
 *     the next choice point, wide quantifier or the end, a choice group's
 *     content not (that is its choice segment) — is compared again for
 *     every place the wide quantifiers up to it can stop and every path
 *     through the choices before it. Its width W: every "{n}" repeat (n
 *     each) and the content of lookarounds, plus written-out text by how
 *     much it repeats itself (see wseg below; "\.mp4" next to 0, ".{4}" 4,
 *     "xXxX" 3), 1 at least. log2 W plus the choices before it add up to at
 *     most choiceBudget(j), j the wide quantifiers up to and including it:
 *     9 after the first (W up to 512 with no choice before it), 0.58 after
 *     the second (W below 1.5: no "{n}", text that hardly repeats itself), 8
 *     more with a leading "^" as above. "x.*.{511}y", "^.*x.*.{383}y", "/hls/.+/.+\.m3u8",
 *     "/api/.+/sessions/.+/delivery$" are fine; ".*x.*.{20}y" (7.4 s),
 *     ".?.?.?.?.?.?x.*.{300}y" (over 20 s) are not (SEC18f 低-17, SEC22 L-C);
 *   - no empty alternative ("a|", "(?:|x)"; SEC22 L-B): "(?:x)?" says the
 *     same.
 *   - at most one group with two alternatives that take no character ("^",
 *     "$", "\b", "\B", lookarounds, "{0}", empty groups), none with three;
 *     a lookaround's own alternatives and the top level's not counted
 *     (SEC22c R-1): "(?:(?=/)|$)" is fine once, "(?:^|$)(?:^|$)" is not,
 *     "(?=/|$)" and "(?:/|$)" are (within the choices allowed above).
 *   - lookaheads and lookbehinds hold at most MAX_ASSERTION_WIDTH (64)
 *     characters in all, and a lookbehind no wide quantifier: they run
 *     again at every position a wide quantifier tries (SEC18e 低-14).
 * Character classes are read the way JavaScript reads them ("[]" is empty,
 * "[^]" any character), so a class cannot hide a group from this check.
 */
export function regexComplexity(src) {
  const s = String(src);
  if (/\\[1-9]|\\k</.test(s)) return 'backreferences are not allowed';
  // Groups being read. `variable`: the group's width is not fixed (see
  // above); `atoms`: it holds something that takes a character. `trail`:
  // the input read so far may end in a wide quantifier followed only by
  // optional or zero-width items.
  // `width`: the most characters the current branch can take (a wide
  // quantifier counted at its minimum, 1 at least); `maxBranch`: the widest
  // finished branch.
  // `looks`: the width of the lookarounds inside, which a choice group's
  // segment carries too (SEC18d: they run once per path through it).
  // `zero`: the finished branches that take no character (width 0).
  const frame = (startTrail, look) => ({ variable: false, alternation: false, atoms: false, branches: 1, startTrail, altTrail: false, look, width: 0, maxBranch: 0, looks: 0, zero: 0 });
  const root = { ...frame(false, false), branchStart: 0 };
  const open = [];
  const top = () => (open.length ? open[open.length - 1] : root);
  const cap = (w) => Math.min(MAX_REGEX_INPUT, w);
  const times = (q) => (!q ? 1 : q.wide ? Math.max(q.min, 1) : q.max);
  let trail = false;
  let wide = 0;
  let choices = 0; // log2
  let unit = 1; // the widest choice unit
  // Choice segments (SEC18c 中-4): from a choice point ("?", "{n,m}", a
  // group with "|") over everything required after it — lookarounds
  // included — up to the next choice point, wide quantifier or the end.
  // Every path a choice opens compares up to a segment's width before the
  // next choice; `seg` is the open segment (null before the first choice
  // and after a wide quantifier), `maxSeg` the widest closed one.
  let seg = null;
  let maxSeg = 1;
  let assertions = 0; // the width of every lookahead and lookbehind, nested ones counted again
  let zeroWidth2 = 0; // log2 of the alternatives taking no character, over groups with two or more (SEC22c R-1)
  // Wide segments (SEC18f 低-17, SEC22 L-C): what a wide quantifier
  // requires after it, up to the next choice point, wide quantifier or the
  // end. It is tried again at every place the wide quantifiers up to it can
  // stop and for every path through the choices before it; log2 of its
  // width W plus those choices must stay within choiceBudget(j), j the wide
  // quantifiers up to it.
  // W counts what a run of such tries can compare again and again: a repeat
  // ("{n}" on a character or a fixed-width group, n each) and the content of
  // lookarounds in full (".*x.*.{20}y": 7.4 s, ".?.?.?.?.?.?x.*.{300}y":
  // over 20 s); of text written out, each element only as far as it can
  // match where an earlier one did: 1 / the distance to the nearest earlier
  // element that can take the same character ("." and classes can take any;
  // letters compared without case). Input that fits a text at every place
  // must repeat with that text, so "\.mp4" or "/delivery" (an "e" four
  // apart: 0.25) count next to nothing and ".*/a/.*\.mp4" passes, while
  // ".*x.*" + "xX" x 120 + "[^x]" (3.1 s on 999 characters) counts 240.
  // `wseg`: { loops, text (the weight of the text), t (elements so far),
  // last (the last element that can take any character), seen (character →
  // its last element), before (log2), j, id }.
  let wseg = null;
  let wsegs = 0;
  let wideOver = -Infinity; // the most a closed wide segment exceeds its budget by, in bits
  let wideWorst = null; // and that segment, for the message
  const wideWidth = (w) => Math.max(1, Math.round(cap(w.loops + w.text) * 100) / 100);
  const endSeg = () => {
    if (seg !== null) maxSeg = Math.max(maxSeg, cap(seg));
    if (wseg !== null) {
      const over = wseg.before + Math.log2(wideWidth(wseg)) - choiceBudget(wseg.j);
      if (over > wideOver) {
        wideOver = over;
        wideWorst = wseg;
      }
      wseg = null;
    }
  };
  const addSeg = (w) => {
    if (seg !== null) seg = cap(seg + w);
  };
  // In the wide segment: a repeat or anything inside a lookaround (key
  // null and loop), or one element of text (key: its character, lowercased;
  // null when it can take more than one character).
  const addWide = (w, loop, key) => {
    if (wseg === null || w <= 0) return;
    const t = wseg.t++;
    if (loop || open.some((o) => o.look)) {
      wseg.loops = cap(wseg.loops + w);
      wseg.last = t;
      return;
    }
    const prev = key === null ? t - 1 : Math.max(wseg.last, wseg.seen.has(key) ? wseg.seen.get(key) : -1);
    if (prev >= 0) wseg.text += 1 / (t - prev);
    if (key === null) wseg.last = t;
    else wseg.seen.set(key, t);
  };
  // after a wide quantifier: its segment opens (the choices so far include
  // those of a group it applies to)
  const openWide = () => {
    endSeg();
    seg = null;
    wseg = { loops: 0, text: 0, t: 0, last: -1, seen: new Map(), before: choices, j: wide + 1, id: ++wsegs };
  };
  let i = 0;
  const variable = () => {
    for (const o of open) o.variable = true;
  };
  const quantifier = (j) => {
    const c = s[j];
    if (c === '*' || c === '+') return { len: s[j + 1] === '?' ? 2 : 1, min: c === '+' ? 1 : 0, max: Infinity, wide: true };
    if (c === '?') return { len: s[j + 1] === '?' ? 2 : 1, min: 0, max: 1, wide: false };
    if (c === '{') {
      const m = /^\{(\d+)(,(\d*))?\}\??/.exec(s.slice(j));
      if (!m) return null; // "{" that is not a quantifier is a character (Annex B)
      const min = Number(m[1]);
      const max = m[2] === undefined ? min : m[3] === '' ? Infinity : Number(m[3]);
      return { len: m[0].length, min, max, wide: max === Infinity || max - min > WIDE_SPAN };
    }
    return null;
  };
  while (i < s.length) {
    const at = i;
    const c = s[i];
    let group = null;
    let zeroWidth = false;
    if (c === '\\') {
      zeroWidth = s[i + 1] === 'b' || s[i + 1] === 'B';
      i += 2;
      // one character written as "\uXXXX", "\xXX" or "\cX" (SEC22 I-1)
      if (s[i - 1] === 'u' && /^[0-9a-fA-F]{4}/.test(s.slice(i, i + 4))) i += 4;
      else if (s[i - 1] === 'x' && /^[0-9a-fA-F]{2}/.test(s.slice(i, i + 2))) i += 2;
      else if (s[i - 1] === 'c' && /^[A-Za-z]/.test(s[i] || '')) i += 1;
    } else if (c === '[') {
      // as JavaScript reads it: the first "]" closes the class, even right
      // after "[" or "[^" ("[]" matches nothing, "[^]" any character)
      i++;
      if (s[i] === '^') i++;
      while (i < s.length && s[i] !== ']') i += s[i] === '\\' ? 2 : 1;
      i++;
    } else if (c === '(') {
      let look = false;
      let behind = false;
      if (s[i + 1] !== '?') i += 1;
      else if (s[i + 2] === ':') i += 3;
      else if (s[i + 2] === '=' || s[i + 2] === '!') {
        look = true;
        i += 3;
      } else if (s[i + 2] === '<' && (s[i + 3] === '=' || s[i + 3] === '!')) {
        look = true;
        behind = true;
        i += 4;
      } else if (s[i + 2] === '<') {
        const m = /^[A-Za-z_$][\w$]*>/.exec(s.slice(i + 3));
        if (!m) return GROUP_SYNTAX;
        i += 3 + m[0].length;
      } else return GROUP_SYNTAX;
      if (look) variable(); // an assertion inside a group
      // the wide segment as the group opens: a choice group's content is no part of it
      open.push({ ...frame(trail, look), behind, wsegId: wseg ? wseg.id : 0, wsegAt: wseg ? { ...wseg, seen: new Map(wseg.seen) } : null, branchStart: i });
      continue;
    } else if (c === ')') {
      if (open.length && open[open.length - 1].branches > 1 && i === open[open.length - 1].branchStart) return EMPTY_BRANCH;
      group = open.pop() || null;
      i++;
      if (!group) continue; // a stray ")" does not compile; reported elsewhere
      if (group.width === 0) group.zero++;
      if (group.zero >= 2 && !group.look) zeroWidth2 += Math.log2(group.zero);
    } else if (c === '|') {
      const g = top();
      if (i === g.branchStart) return EMPTY_BRANCH;
      for (const o of [root, ...open]) o.alternation = true;
      variable();
      g.branches++;
      if (g.width === 0) g.zero++;
      g.maxBranch = Math.max(g.maxBranch, g.width);
      g.width = 0;
      g.altTrail = g.altTrail || trail;
      trail = g.startTrail;
      i++;
      g.branchStart = i;
      continue;
    } else {
      zeroWidth = c === '^' || c === '$';
      i++;
    }
    const atomEnd = i;
    const q = quantifier(i);
    const exact = q && q.min === q.max && q.min >= 1;
    const choice = q && !q.wide && q.max > q.min;
    if (group) {
      choices += Math.log2(group.branches);
      const gw = cap(Math.max(group.maxBranch, group.width));
      const costWidth = cap(gw + group.looks);
      if (group.look) assertions += costWidth;
      if (group.branches > 1 || choice) unit = Math.max(unit, costWidth);
      if (!group.look) top().width = cap(top().width + gw * times(q));
      top().looks = cap(top().looks + (group.look ? costWidth : group.looks));
      // its content is in the open segment already (an over-count)
      if (q && q.wide) openWide();
      else if (choice || group.branches > 1) {
        if (wseg !== null && wseg.id === group.wsegId) wseg = group.wsegAt;
        endSeg();
        seg = cap(costWidth * (choice ? q.max : 1));
      } else if (exact && !group.look) {
        addSeg(gw * (q.max - 1));
        if (q.max > 1) addWide(gw * q.max, true, null);
      }
      if (!group.atoms) group.variable = true; // empty, or only assertions
      const inner = group.altTrail || trail;
      if (q && q.max > 1 && (group.variable || group.alternation)) return REPEATED;
      if (group.variable || (q && !exact)) variable();
      if (group.atoms && !group.look) for (const o of open) o.atoms = true;
      if (group.look) trail = group.startTrail;
      else if (q && q.wide) {
        if (group.startTrail) return ADJACENT;
        trail = true;
      } else if (q && q.min === 0) trail = group.startTrail || inner;
      else trail = inner;
    } else {
      if (!zeroWidth) top().width = cap(top().width + times(q));
      const w = zeroWidth ? 0 : 1;
      if (q && q.wide) openWide();
      else if (choice) {
        endSeg();
        seg = w * q.max;
      } else {
        addSeg(w * (q ? q.max : 1));
        addWide(w * (q ? q.max : 1), !!q && q.max > 1, textKey(s.slice(at, atomEnd)));
      }
      if (zeroWidth) variable();
      else for (const o of open) o.atoms = true;
      if (q && !exact) variable();
      if (q && q.wide) {
        if (trail) return ADJACENT;
        trail = true;
      } else if (!zeroWidth && (!q || q.min > 0)) trail = false;
    }
    if (!q) continue;
    if (q.wide && open.some((o) => o.behind)) return BEHIND_WIDE;
    if (q.wide) wide++;
    else choices += Math.log2(q.max - q.min + 1);
    i += q.len;
  }
  if (root.branches > 1 && root.branchStart === s.length) return EMPTY_BRANCH;
  // the top level's alternatives are not counted: nothing follows them
  if (zeroWidth2 > MAX_ZERO_WIDTH_LOG2 + 1e-9) return ZERO_WIDTH;
  choices += Math.log2(root.branches);
  if (assertions > MAX_ASSERTION_WIDTH) return ASSERTIONS;
  endSeg();
  if (root.branches > 1) unit = Math.max(unit, cap(Math.max(root.maxBranch, root.width)));
  const W = Math.max(unit, maxSeg);
  const cost = choices + (choices > 0 ? Math.log2(W) : 0);
  // How to rewrite what the checks below refuse (SEC22b S-4): each hint
  // makes the common shapes pass (see the tests).
  const LEAD = 'leave out a leading or trailing ".*" (a regex without "^" is searched for anywhere and needs nothing after its last required part) or the part before the path ("https?://[^/]+")';
  if (wide > MAX_WIDE) return `too many "*", "+", "{n,}" or wide "{n,m}" (at most ${MAX_WIDE}). To fix it, ${LEAD}, or join two into one (not "/a/[^/]+/[^/]+/[^/]+\\.mp4" but "/a/.*/[^/]+\\.mp4")`;
  // "^" first, with no top-level "|": tried from the first character only
  const anchored = s.startsWith('^') && root.branches === 1;
  const budget = choiceBudget(wide) + (anchored ? ANCHORED_BONUS : 0);
  if (cost > budget + 1e-9) {
    const caret = anchored ? '' : 'start a pathPattern with "^" (it is matched against the path), ';
    const hint = wide === 2 ? `with "?", "{n,m}" or "|" keep one "*" or "+": ${LEAD}, or join the two (not "/a/.*/.*\\.(?:mp4|m4a)$" but "/a/.*\\.(?:mp4|m4a)$"); or ${caret}drop the choices` : `${caret}use fewer "?", "{n,m}" and "|", or require less after them`;
    return `too many ways to match: with ${wide} "*", "+" or wide "{n,m}", the "?", "{n,m}" and "|" plus log2 of the widest choice segment (${W}) may add at most ${Math.max(0, Math.floor((budget + 1e-9) * 100) / 100)} bits, this one ${Math.round(cost * 10) / 10} (slow backtracking). To fix it, ${hint}`;
  }
  if (wideWorst && wideOver > (anchored ? ANCHORED_BONUS : 0) + 1e-9) {
    const w = wideWorst;
    // the widest segment it allows there (SEC22 I-3: 1 after the second, unanchored)
    const most = Math.floor(2 ** (choiceBudget(w.j) + (anchored ? ANCHORED_BONUS : 0) - w.before) + 1e-9);
    const caret = anchored ? '' : 'start the regex with "^", ';
    const hint = w.j === 2 ? `${caret}leave out a leading ".*" (a regex without "^" is searched for anywhere), or bound one of the two, "{1,16}"` : `${caret}use fewer "?", "{n,m}" and "|" before it, or require less after it`;
    return `too many ways to match: after the ${w.j === 1 ? 'first' : 'second'} "*", "+" or wide "{n,m}", with the choices before it, what it requires next may weigh at most ${most} (each "{n}" repeat and lookaround in full, text written out by how much it repeats itself), here ${wideWidth(w)} (slow backtracking). To fix it, ${hint}`;
  }
  return '';
}
