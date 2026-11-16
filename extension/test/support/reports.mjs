// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Builds reports from the synthetic pages the way the background does.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import * as S from './synth.mjs';
import { detectTracks } from '../../src/lib/detect.js';
import { findProfile, applyProfile, readResults, sessionIdFor, lectureFor, downloadPolicyFor } from '../../src/lib/profile.js';
import { combineObserved, profileKey } from '../../src/lib/observe.js';
import { buildReport } from '../../src/lib/report.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const panoptoProfile = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));

export async function runSynthetic(kind, opts = {}) {
  const isPanopto = kind === 'panopto';
  const frames = isPanopto ? S.panoptoFrames(opts.observed === false ? null : profileKey(panoptoProfile), opts) : S.genericFrames();
  const webRequests = isPanopto ? [] : S.genericWebRequests;
  const io = isPanopto ? S.fakeIo(S.panoptoTexts) : S.fakeIo(S.genericTexts, S.genericFiles);
  const pageUrl = isPanopto ? S.panoptoPageUrl : `${S.GEN_HOST}/watch/lecture-07`;
  const detection = await detectTracks({ frames, webRequests }, io);
  const match = findProfile([{ profile: panoptoProfile, reviewed: true }], pageUrl);
  const observed = match ? combineObserved(match.profile, frames) : null;
  const named = match ? applyProfile(match.profile, detection.tracks, 'en', observed) : null;
  const sel = readResults(frames);
  const pageDuration = detection.evidence.mediaDurations.length ? Math.max(...detection.evidence.mediaDurations) : 0;
  const session = await sessionIdFor(match, pageUrl, sel);
  // As src/background/detector.js: the page's title (frameTitle) only without a profile.
  const lecture = lectureFor(match, sel, pageDuration, observed, match ? null : { title: isPanopto ? '' : S.GEN_PAGE_TITLE, url: pageUrl });
  return { frames, webRequests, pageUrl, detection, match, observed, named, session, lecture, downloadPolicy: downloadPolicyFor(match, sel, observed), io };
}

export async function reportFromSynthetic(kind) {
  const r = await runSynthetic(kind);
  return buildReport({
    extension: { version: '0.1.0', browser: 'chrome' },
    pageUrl: r.pageUrl,
    frames: r.frames,
    webRequests: r.webRequests,
    detection: r.detection,
    match: r.match,
    named: r.named,
    session: r.session,
    lecture: r.lecture,
    downloadPolicy: r.downloadPolicy,
    now: new Date('2026-10-26T12:00:00Z'),
  });
}
