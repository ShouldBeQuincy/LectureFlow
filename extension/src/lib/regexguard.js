// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The runtime guard of a platform profile's regular expressions (SEC18d).
// The static check (./regexcost.js) screens a profile when it is imported
// or loaded, but four rounds of review each found a pattern it let through
// that took seconds; and a JavaScript regex match cannot be interrupted.
// So every match of a profile regex goes through guardedRegex, which times
// it afterwards:
//   - a match longer than SLOW_MS, or matches of one profile adding up to
//     more than BUDGET_MS within one epoch (a minute, PAGE_WINDOW_MS; in the
//     background also one detection at most), trips the guard;
//   - from then on in this context, every regex of that profile (its
//     `owner`, profileKey) and that source returns "no match" at once;
//   - onTrip, set by the context, hears about it once: the background
//     records the profile as too slow (src/background/health.js), a content
//     script tells the background.
// A stall is thus bounded by one match (which the static check bounds in
// practice) plus the budget, once per context; afterwards it is zero.
// test / exec take one argument and never run on more than maxInput
// characters. Pure, but for the module state below.

import { MAX_REGEX_INPUT } from './regexcost.js';

/** One match slower than this trips the guard (normal rules take well under a millisecond). */
export const SLOW_MS = 250;
/** Matches of one profile within one epoch adding up to more than this trip it. */
export const BUDGET_MS = 1000;


/** The budget window of a content script (SEC18e) and of the background (SEC18f 低-19): the budget starts again every minute. */
export const PAGE_WINDOW_MS = 60000;

/**
 * The background's cap for one profile within a window, whatever the
 * detections in it (SEC22 L-E): a page can start a detection about every
 * 1.5 s, and each starts its own BUDGET_MS. With it, one profile costs the
 * background at most this plus one match under SLOW_MS a minute (twice that
 * across a window's edge). The packaged Panopto profile takes about 0.01
 * ms a detection on its fixture.
 */
export const BACKGROUND_WINDOW_BUDGET_MS = 3000;

const state = {
  now: () => (typeof performance !== 'undefined' ? performance.now() : Date.now()),
  onTrip: null,
  owners: new Map(), // owner → trip
  sources: new Map(), // source → trip, for regexes without an owner only
  // Sources (with their flags) run once on '' in this context (SEC22 L-A):
  // V8 runs a regex's first match on fewer than 1000 characters in its
  // bytecode interpreter, 5 to 8 times slower ("x.*.{511}y" on 999
  // characters: 2.6 s, then 0.42 s), and compiles it for every later match,
  // of any RegExp with the same source and flags. So before a source's first
  // match here it runs once on '' — inside that match's timing, so a regex
  // slow only on '' trips the guard like any other (SEC22b M-1), and never
  // before the match, so nothing runs that was not checked (SEC22b M-2).
  // SpiderMonkey's tier-up is not measured.
  warmed: new Set(),
  spent: new Map(), // owner (or source) → ms in this epoch
  windowMs: 0, // 0: epochs end only by resetRegexBudget (tests; both contexts set PAGE_WINDOW_MS)
  windowStart: null,
  windowBudgetMs: 0, // 0: none; otherwise what one profile may spend in a window, resetRegexBudget or not
  spentWindow: new Map(), // owner (or source) → ms in this window
};

/**
 * Sets the clock (tests), the trip listener and the budget window: with
 * windowMs, the budget starts again every windowMs (content scripts, which
 * live as long as the page); with windowBudgetMs, a profile's matches in a
 * window may add up to at most that, however often resetRegexBudget starts
 * a new epoch (the background, SEC22 L-E).
 */
export function setRegexGuard({ now, onTrip, windowMs, windowBudgetMs } = {}) {
  if (typeof now === 'function') state.now = now;
  if (onTrip !== undefined) state.onTrip = typeof onTrip === 'function' ? onTrip : null;
  if (windowMs !== undefined) {
    state.windowMs = Number.isFinite(windowMs) && windowMs > 0 ? windowMs : 0;
    state.windowStart = null;
    state.spentWindow.clear();
  }
  if (windowBudgetMs !== undefined) state.windowBudgetMs = Number.isFinite(windowBudgetMs) && windowBudgetMs > 0 ? windowBudgetMs : 0;
}

/** A new epoch: the budget starts again (trips stay). */
export function resetRegexBudget() {
  state.spent.clear();
}

/** Forgets everything (tests; and when the stored records are cleared). */
export function resetRegexGuard() {
  state.owners.clear();
  state.sources.clear();
  state.spent.clear();
  state.spentWindow.clear();
  state.windowStart = null;
  state.warmed.clear();
}

/**
 * The trip that stopped this regex, or null: with an owner, a trip of that
 * profile (any of its regexes); without one, a trip of that source among
 * the regexes without owner. A trip never reaches another profile with
 * the same regex (SEC18e 低-12).
 */
export function regexTrip(owner, source) {
  if (owner) return state.owners.get(owner) || null;
  return (source !== undefined && state.sources.get(String(source))) || null;
}

function trip(owner, source, ms, reason) {
  const t = { owner: owner || '', source, ms: Math.round(ms), reason };
  if (owner) state.owners.set(owner, t);
  else state.sources.set(source, t);
  if (state.onTrip) {
    try {
      state.onTrip(t);
    } catch {
      // the listener's problem
    }
  }
}

/**
 * A profile regex behind the guard: { source, test(s), exec(s) }. owner is
 * the profile's key (profileKey), '' when the caller has no profile (the
 * checks at import, which the static check precedes).
 */
export function guardedRegex(source, { owner = '', flags = 'i', maxInput = MAX_REGEX_INPUT } = {}) {
  const src = String(source);
  const re = new RegExp(src, flags);
  const warmKey = `${flags}/${src}`;
  const run = (s, fn) => {
    if (regexTrip(owner, src)) return null;
    const str = String(s);
    if (str.length > maxInput) return null;
    const t0 = state.now();
    if (state.windowMs) {
      if (state.windowStart === null) state.windowStart = t0;
      else if (t0 - state.windowStart >= state.windowMs) {
        state.spent.clear();
        state.spentWindow.clear();
        state.windowStart = t0;
      }
    }
    if (!state.warmed.has(warmKey)) {
      state.warmed.add(warmKey);
      re.test('');
    }
    const out = fn(str);
    const ms = state.now() - t0;
    if (regexTrip(owner, src)) return out;
    const key = owner || src;
    const total = (state.spent.get(key) || 0) + ms;
    state.spent.set(key, total);
    const inWindow = (state.spentWindow.get(key) || 0) + ms;
    state.spentWindow.set(key, inWindow);
    if (ms > SLOW_MS) trip(owner, src, ms, 'slow');
    else if (total > BUDGET_MS) trip(owner, src, total, 'budget');
    else if (state.windowBudgetMs && inWindow > state.windowBudgetMs) trip(owner, src, inWindow, 'budget');
    return out;
  };
  return {
    source: re.source,
    test: (s) => run(s, (str) => re.test(str)) === true,
    exec: (s) => run(s, (str) => re.exec(str)),
  };
}
