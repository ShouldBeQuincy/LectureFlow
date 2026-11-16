// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// What the send card's buttons do (the popup's and the in-page card's), in
// the background so that closing either does not cut a send short: check a
// session, send it ("发送至 LectureFlow" / "确认发送"), give a needs_refresh
// task new credentials ("续传"), open LectureFlow on a lecture or task.
// The cards name tracks by id only; URLs, cookies and page addresses never
// pass through them. Each action works on a detection result: a tab's, a
// frame's, or a listed session's (src/lib/inpage.js sessionResult).

import { browser, message } from './env.js';
import { nm } from './nm.js';
import { cookieApi } from './cookies.js';
import { allProfiles } from './profiles.js';
import { listedTracks, shownOf, selectionError, buildEnqueue, buildRefresh, cookiePlan, cookieAllowed, quietHostsOf, pickTracks, platformOf, checkOutcome, sendBlockedCategory, topSiteOf } from '../lib/send.js';
import { cookieScope, readCookiesWith } from '../lib/partition.js';
import { RESTRICTED_KEY, isRemembered, withRemembered } from '../lib/remember.js';
import { PRESET_KEY, presetKey, presetOf, presetFor, withPreset } from '../lib/sentpreset.js';

export const LAST_COURSE_KEY = 'lastCourseId';

const msg = (key) => message(key);

export function fail(code, params = {}, extra = {}) {
  return { ok: false, error: { code, params }, ...extra };
}

export function fromBridge(e) {
  return fail(e && e.code ? String(e.code) : 'ext.failed', e && e.params && typeof e.params === 'object' ? e.params : {});
}

const usable = (r) => (r && r.status === 'ok' ? r : null);

async function checkItem(r) {
  const reply = await nm().check(platformOf(r), [r.sessionId]);
  return (reply.items || []).find((i) => i.sessionId === r.sessionId) || null;
}

/** This session's place in LectureFlow: { ok, item, status }. */
export async function checkResult(result) {
  const r = usable(result);
  if (!r) return fail('ext.stale');
  try {
    const item = await checkItem(r);
    const status = item && item.taskId ? nm().latestStatus(item.taskId) : null;
    return { ok: true, item, status };
  } catch (e) {
    return fromBridge(e);
  }
}

/**
 * Several sessions of a platform, with their listed titles for LectureFlow's
 * dedup by title. noLaunch: the row states a list page asks for on its own
 * (never starts LectureFlow).
 */
export async function checkMany(platform, sessionIds, { titles, courseId, noLaunch = false } = {}) {
  try {
    const reply = await nm().check(platform, sessionIds, { titles, courseId, noLaunch });
    return { ok: true, items: reply.items || [] };
  } catch (e) {
    return fromBridge(e);
  }
}

/**
 * Whether the user turned off the reminder before sending a possibly
 * restricted recording of this platform ("对这个平台不再提醒").
 */
export async function restrictedRemembered(platform) {
  try {
    return isRemembered((await browser.storage.local.get(RESTRICTED_KEY))[RESTRICTED_KEY], platform);
  } catch {
    return false;
  }
}

async function rememberRestricted(platform) {
  try {
    const current = (await browser.storage.local.get(RESTRICTED_KEY))[RESTRICTED_KEY];
    await browser.storage.local.set({ [RESTRICTED_KEY]: withRemembered(current, platform) });
  } catch {
    // only a convenience: the next send reminds again
  }
}

/**
 * Which of the three choices a lecture was last sent with, so that "续传"
 * starts with the same tracks (src/lib/sentpreset.js); '' when unknown.
 */
export async function sentPreset(result) {
  const r = usable(result);
  if (!r || !r.sessionId) return '';
  try {
    const key = await presetKey(platformOf(r), r.sessionId);
    return presetFor((await browser.storage.local.get(PRESET_KEY))[PRESET_KEY], key);
  } catch {
    return '';
  }
}

async function rememberPreset(r, ids) {
  try {
    const key = await presetKey(platformOf(r), r.sessionId);
    const current = (await browser.storage.local.get(PRESET_KEY))[PRESET_KEY];
    await browser.storage.local.set({ [PRESET_KEY]: withPreset(current, key, presetOf(listedTracks(r), ids, r.presets || null)) });
  } catch {
    // only a default for "续传"
  }
}

/**
 * The browser's cookies API (src/background/cookies.js, looked up at each
 * call) with the unpartitioned answers held to a send's cookiePlan
 * (security audit AUD3 中-1); the partitioned reads pass as they are, and
 * src/lib/partition.js keeps only the tab's own site's partition of them.
 */
function plannedCookieApi(api, plan) {
  const byUrl = new Map(plan.map((p) => [p.url, p]));
  return {
    async getAll(details) {
      const list = await api.getAll(details);
      if (details && Object.prototype.hasOwnProperty.call(details, 'partitionKey')) return list;
      const entry = byUrl.get(details && details.url);
      return (Array.isArray(list) ? list : []).filter((c) => cookieAllowed(c, entry));
    },
    get getPartitionKey() {
      return api.getPartitionKey;
    },
  };
}

// How long a send waits for the browser's top-level site (topSiteFor)
// before it goes by the cookie proof alone.
let partitionKeyTimeoutMs = 3000;

/** Tests only: how long a send waits for getPartitionKey, in milliseconds. */
export function setPartitionKeyTimeout(ms) {
  partitionKeyTimeoutMs = ms;
}

/**
 * The tab's top-level site as the browser computed it: Chrome 132+'s
 * cookies.getPartitionKey for the top frame (src/lib/send.js topSiteOf).
 * Chrome answers only with host access to the top-level site's own origin
 * ("https://<registrable domain>/*"; SEC51 低-1), which the per-page grants
 * this extension asks for seldom include: on a page like lms.uni.example
 * it throws, and where it answers the top host is its own site
 * (evil.github.io, an address) or the user granted the bare site. No
 * permission is asked for to make it answer. '' — and the send goes by the
 * cookie proof alone, as before — without the API (Firefox, Chrome before
 * 132), without a tab, when it throws or rejects (no access, the frame is
 * gone), answers another shape, or does not answer within
 * partitionKeyTimeoutMs. It goes into the cookie plan only, never into a
 * message.
 */
async function topSiteFor(api, tabId) {
  if (!Number.isInteger(tabId) || tabId < 0) return '';
  let timer;
  try {
    if (typeof api.getPartitionKey !== 'function') return '';
    const late = new Promise((resolve) => {
      timer = setTimeout(() => resolve(null), partitionKeyTimeoutMs);
    });
    return topSiteOf(await Promise.race([Promise.resolve(api.getPartitionKey({ tabId, frameId: 0 })), late]));
  } catch {
    return '';
  } finally {
    clearTimeout(timer);
  }
}

/**
 * The cookies of the ticked tracks' URLs, read now, as far as cookiePlan
 * lets them be handed over. where: the tab the user sent from —
 * { tabId, topUrl } for the popup (the frames are the tracks' own),
 * { tabId, frameId, topUrl } for an in-page card — so that the partition of
 * an embedded player is read too (src/lib/partition.js). The browser's
 * top-level site (topSiteFor) only narrows the plan, and is asked only when
 * the plan holds an entry it applies to: a send that reads no cookie
 * touches no cookies API at all.
 */
async function cookiesFor(r, ids, where) {
  const tracks = pickTracks(r, ids);
  let quietHosts = [];
  try {
    quietHosts = quietHostsOf(await allProfiles());
  } catch {
    // no profiles to say so: the rest of the plan still holds
  }
  let plan = cookiePlan(r, tracks, where.topUrl, { quietHosts });
  if (plan.some((p) => !p.all)) {
    const topSite = await topSiteFor(cookieApi, where.tabId);
    if (topSite) plan = cookiePlan(r, tracks, where.topUrl, { quietHosts, topSite });
  }
  return readCookiesWith(plannedCookieApi(cookieApi, plan), plan.map((p) => p.url), cookieScope({ ...where, tracks }));
}

/**
 * Send: check again (another window may have sent it), then read the
 * cookies of the ticked tracks' URLs — the first and only moment cookies
 * are read — and enqueue. Returns { ok, taskId, position } or
 * { ok: false, error: { code, params }, item? }. rememberPlatform: the
 * user ticked "对这个平台不再提醒" in the reminder they just confirmed; the
 * platform is the result's own profile, never one a card names.
 */
export async function sendResult(result, { trackIds, courseId, overrideDisabled, rememberPlatform }, where = {}) {
  const r = usable(result);
  if (!r) return fail('ext.stale');
  const ids = Array.isArray(trackIds) ? trackIds.map(String) : [];
  const blocked = sendBlockedCategory(r, ids, where.topUrl);
  if (blocked) return fail('ext.site_blocked', { category: blocked });
  // One audio track and up to two pictures (双机位).
  // only tracks a card shows (shownOf): one past RETAIN_LIMITS.tracksShown is unknown here
  const why = selectionError(shownOf(r), ids, { maxVideo: 2 });
  if (why) return fail(`ext.selection.${why}`);
  if (r.downloadPolicy === 'disabled' && !overrideDisabled) return fail('ext.download_disabled');
  if (!courseId) return fail('ext.no_course');
  if (rememberPlatform && overrideDisabled && r.downloadPolicy === 'disabled' && r.profile) await rememberRestricted(r.profile.id);
  let item;
  try {
    item = await checkItem(r);
  } catch (e) {
    return fromBridge(e);
  }
  const outcome = checkOutcome(item);
  if (outcome === 'imported' || outcome === 'active' || outcome === 'needs_refresh') {
    return fail('queue.duplicate', { state: item.state, taskId: item.taskId || 0, lectureId: item.lectureId || '' }, { item });
  }
  const cookies = await cookiesFor(r, ids, where);
  try {
    const reply = await nm().enqueue(buildEnqueue({ result: r, ids, courseId, cookies, overrideDisabled: !!overrideDisabled, msg }));
    await rememberPreset(r, ids);
    try {
      await browser.storage.local.set({ [LAST_COURSE_KEY]: String(courseId) });
    } catch {
      // the course is only a default for next time
    }
    return { ok: true, taskId: reply.taskId, position: reply.position };
  } catch (e) {
    return fromBridge(e);
  }
}

/** "续传": the ticked tracks and their cookies, read now, for a needs_refresh task. */
export async function refreshResult(result, { trackIds }, where = {}) {
  const r = usable(result);
  if (!r) return fail('ext.stale');
  const ids = Array.isArray(trackIds) ? trackIds.map(String) : [];
  const blocked = sendBlockedCategory(r, ids, where.topUrl);
  if (blocked) return fail('ext.site_blocked', { category: blocked });
  // only tracks a card shows (shownOf): one past RETAIN_LIMITS.tracksShown is unknown here
  const why = selectionError(shownOf(r), ids, { maxVideo: 2 });
  if (why) return fail(`ext.selection.${why}`);
  const cookies = await cookiesFor(r, ids, where);
  try {
    const reply = await nm().refresh(buildRefresh({ result: r, ids, cookies, msg }));
    return { ok: true, taskId: reply.taskId, state: reply.state };
  } catch (e) {
    return fromBridge(e);
  }
}

/** "在 LectureFlow 中打开". */
export async function openInApp({ lectureId, taskId }) {
  try {
    await nm().open({ lectureId, taskId });
    return { ok: true };
  } catch (e) {
    return fromBridge(e);
  }
}

/** The last status push of a task, if this connection saw one. */
export function latestStatus(taskId) {
  try {
    return nm().latestStatus(taskId);
  } catch {
    return null;
  }
}
