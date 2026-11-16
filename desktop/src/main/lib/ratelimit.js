// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A sliding-window rate limit for IPC requests of the app page (security
// audit FB1 L-6: openExternal must not become an outward channel that
// script injected into the page can use at will).
// No Electron imports.
'use strict';

// window.lectureflow.openExternal: at most 3 links in 10 seconds. A person
// opens one link per click; the preload also requires a user activation.
const OPEN_EXTERNAL_LIMIT = Object.freeze({ max: 3, windowMs: 10000 });

// window.lectureflow.notify (lf:notify): at most 1 system notification in
// 3 seconds (final security audit AUD2 L3: any text, no user gesture).
// What the app announces (a lecture finished, a stage failed for good)
// comes once per event and seconds apart; a faster stream of system
// notifications would only serve script injected into the page.
const NOTIFY_LIMIT = Object.freeze({ max: 1, windowMs: 3000 });

/**
 * createRateLimit({ max, windowMs, now }) → take(): true when one more
 * request may go through now (and counts it), false when max requests went
 * through in the last windowMs. Refused requests do not count. A clock that
 * goes back moves the counted requests to "now", so it never opens the gate.
 */
function createRateLimit({ max, windowMs, now = Date.now }) {
  if (!Number.isInteger(max) || max < 1) throw new TypeError('createRateLimit: max must be a positive integer');
  if (!Number.isFinite(windowMs) || windowMs <= 0) throw new TypeError('createRateLimit: windowMs must be positive');
  let times = [];
  return function take() {
    const t = now();
    times = times.map((x) => Math.min(x, t)).filter((x) => x > t - windowMs);
    if (times.length >= max) return false;
    times.push(t);
    return true;
  };
}

module.exports = { OPEN_EXTERNAL_LIMIT, NOTIFY_LIMIT, createRateLimit };
