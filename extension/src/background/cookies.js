// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The only place the extension reads cookies (SPEC-ext 隐私: Cookie 只在用户点
// “送进 LectureFlow”时读取，只读轨道所在域名): the browser's cookies API, which
// src/background/actions.js reads through when the user clicks "送进
// LectureFlow" or "续传" — only for the ticked tracks' URLs that
// src/lib/send.js cookiePlan names (none on a host whose media a profile
// says need no cookie, none of another site whose address a page merely
// wrote), each unpartitioned answer held to that plan (send.js
// cookieAllowed: the page's site; a SameSite=None cookie of an embedded
// player that itself requested the track, in the Chrome build; the track
// host's own host-only cookies where a cookie proves it on that site), and
// of the partitioned cookies only the tab's own site's (src/lib/
// partition.js). The cookies go straight into that one message to
// LectureFlow; nothing here keeps, logs or stores them.

import { browser } from './env.js';
import { withAnswerDomains } from '../lib/send.js';

// One indirection so the end-to-end test can record the getAll calls
// (their arguments, never the answers); the e2e build only. Each answer's
// cookies are marked, in memory, with the Domain values of the answer's
// domain cookies (src/lib/send.js withAnswerDomains: the same-site proof
// of cookieAllowed; never part of a message).
// getPartitionKey: Chrome 132+ only, looked up at each call: the frames'
// partitions (src/lib/partition.js) and the tab's top-level site, which
// narrows send.js cookieAllowed (actions.js topSiteFor). It answers only
// with host access to the top-level site's own origin, which per-page
// grants seldom include (SEC51 低-1); else it throws and both go without
// it. Firefox has none (web-ext lint warns UNSUPPORTED_API about this one
// line; known).
export const cookieApi = {
  getAll: async (details) => withAnswerDomains(await browser.cookies.getAll(details)),
  get getPartitionKey() {
    const fn = browser.cookies.getPartitionKey; // bound to chrome.cookies by the polyfill
    return typeof fn === 'function' ? fn : undefined;
  },
};
