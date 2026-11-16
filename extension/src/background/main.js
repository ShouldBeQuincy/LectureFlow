// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Background entry: a service worker on Chrome and Edge, an event page on
// Firefox. Every listener is registered synchronously at the top level, as
// Manifest V3 requires for a worker that the browser may stop at any time.

import { browser, BUILD, refreshLanguage, languageReady, chosenCatalog } from './env.js';
import { LANGUAGE_KEY } from '../lib/i18n.js';
import * as requests from './requests.js';
import { detectTab, forgetTab, lastResult, viewOf, suggestedOrigins, cookieHostOrigins, reportFor, setBadge, reportableTabs } from './detector.js';
import { isAuthorized, syncObserver, grantedOrigins } from './sites.js';
import { allProfiles, recordTrip, watchRegexes, forgetTrips } from './profiles.js';
import { fromExtensionPage, recordRemoved, HEALTH_KEY } from '../lib/health.js';
import { nm, connection, setTestConnect } from './nm.js';
import { checkResult, checkMany, sendResult, refreshResult, openInApp, latestStatus, fail, restrictedRemembered, sentPreset, LAST_COURSE_KEY } from './actions.js';
import { cookieApi } from './cookies.js';
import { findProfile, profileMatchesUrl, profilePermissionOrigins } from '../lib/profile.js';
import { platformOf } from '../lib/send.js';
import { observeConfig } from '../lib/observe.js';
import { findInpage, inpageConfig, sessionLoadReply, sessionInfoRequest, sessionInfoSpec } from '../lib/inpage.js';
import { contextCategory } from '../lib/blockedframes.js';
import { isBlockedUrl } from '../lib/blocklist.js';
import { urlKey, parseHttpUrl, hostMatches, requestableOrigin } from '../lib/url.js';
import { frameSite, embedRoute, windowReason, TRUST_KEY, isTrusted, withTrusted, withoutTrusted } from '../lib/embed.js';
import { portKindOf, statusForPage } from '../lib/ports.js';
import { uiLocale } from './env.js';

const AUTO_DELAY_MS = 1500;
const SESSION_TTL_MS = 30 * 60 * 1000;
const autoTimers = new Map();
const tabUrls = new Map(); // tabId → page URL without fragment, to spot navigations
// Listed sessions loaded by an in-page card ("tabId:frameId:sessionId" →
// { result, at }): memory only, gone with the tab or after 30 minutes.
const sessions = new Map();

function forgetSessions(tabId) {
  for (const k of [...sessions.keys()]) if (k.startsWith(`${tabId}:`)) sessions.delete(k);
}
function sessionGet(key) {
  const s = sessions.get(key);
  if (!s) return null;
  if (Date.now() - s.at > SESSION_TTL_MS) {
    sessions.delete(key);
    return null;
  }
  return s.result;
}

// Automatic counting, only on sites the user authorized.
async function scheduleAuto(tabId) {
  if (autoTimers.has(tabId)) clearTimeout(autoTimers.get(tabId));
  autoTimers.set(
    tabId,
    setTimeout(async () => {
      autoTimers.delete(tabId);
      try {
        const tab = await browser.tabs.get(tabId);
        if (tab.url && (await isAuthorized(tab.url))) await detectTab(tabId);
      } catch {
        // tab closed
      }
    }, AUTO_DELAY_MS),
  );
}

requests.install((tabId) => scheduleAuto(tabId));

browser.tabs.onUpdated.addListener((tabId, info, tab) => {
  // A new address makes the last detection stale. The request buffer is
  // cleared by the main-frame request instead (see requests.js): this event
  // can arrive after the new page's first requests.
  if (info.url !== undefined) {
    const k = urlKey(info.url) || info.url;
    if (tabUrls.has(tabId) && tabUrls.get(tabId) !== k) {
      forgetTab(tabId);
      forgetSessions(tabId);
    }
    tabUrls.set(tabId, k);
    // a single-page site moving onto a blocked path (linkedin.com/learning):
    // no number left over from the page before
    if (isBlockedUrl(info.url)) setBadge(tabId, 0);
  }
  if (info.status === 'complete' && tab && tab.url) scheduleAuto(tabId);
});

browser.tabs.onRemoved.addListener((tabId) => {
  requests.forget(tabId);
  forgetTab(tabId);
  forgetSessions(tabId);
  tabUrls.delete(tabId);
  if (autoTimers.has(tabId)) clearTimeout(autoTimers.get(tabId));
  autoTimers.delete(tabId);
});

browser.runtime.onInstalled.addListener(() => syncObserver());
browser.runtime.onStartup.addListener(() => syncObserver());
browser.permissions.onAdded.addListener(() => syncObserver());
browser.permissions.onRemoved.addListener(() => syncObserver());
// A locally imported profile may add or drop an observation or a button;
// the interface language may change in the settings.
browser.storage.onChanged.addListener((changes, area) => {
  // a profile stopped as too slow, or enabled again (src/lib/health.js)
  if (area === 'local' && (changes.localProfiles || changes[HEALTH_KEY])) syncObserver();
  // a record removed (enabled again, imported again): this background tries again too
  if (area === 'local' && changes[HEALTH_KEY] && recordRemoved(changes[HEALTH_KEY].oldValue, changes[HEALTH_KEY].newValue)) forgetTrips();
  if (area === 'local' && changes[LANGUAGE_KEY]) refreshLanguage();
});
refreshLanguage();
watchRegexes();

const senderUrl = (sender) => (sender && (sender.url || (sender.tab && sender.frameId === 0 ? sender.tab.url : ''))) || '';
const senderTopUrl = (sender) => (sender && sender.tab && typeof sender.tab.url === 'string' ? sender.tab.url : '');
const ancestorList = (msg) => (msg && Array.isArray(msg.ancestors) ? msg.ancestors.map(String) : null);

/**
 * Whether a content script's frame is, or sits inside, a page of a
 * do-not-handle site (src/lib/blocklist.js): its own address, the tab's
 * (when the browser gives it), the origins it is embedded in.
 */
function senderBlocked(sender, msg) {
  return contextCategory({ frameUrl: senderUrl(sender), topUrl: senderTopUrl(sender), ancestors: ancestorList(msg) }) !== '';
}

/**
 * The observer content script of a frame asks whether its page's profile
 * observes a response (src/content/observe.js). Only for a frame of an
 * authorized site that matches a profile with `observe`, not in or under
 * a blocked page; null otherwise.
 */
async function observeConfigFor(sender, msg) {
  const url = senderUrl(sender);
  if (!url || senderBlocked(sender, msg) || !(await isAuthorized(url))) return null;
  const match = findProfile(await allProfiles(), url);
  return match ? observeConfig(match.profile) : null;
}

/** The in-page entry of a frame: a profile's `inpage` on an authorized site, not in or under a blocked page; or null. */
async function inpageFor(sender, msg = null) {
  const url = senderUrl(sender);
  if (!url || !sender.tab || senderBlocked(sender, msg) || !(await isAuthorized(url))) return null;
  return findInpage(await allProfiles(), url);
}

browser.runtime.onMessage.addListener((msg, sender) => {
  if (!msg || !sender || sender.id !== browser.runtime.id) return undefined;
  if (msg.cmd === 'observeConfig') return observeConfigFor(sender, msg).catch(() => null);
  // a profile regex too slow in a content script (src/lib/regexguard.js):
  // recorded for a local profile only (src/lib/health.js recordable)
  const ownPage = fromExtensionPage(sender, browser.runtime.getURL(''));
  if (msg.cmd === 'regexSlow') return sender.tab && !ownPage ? recordTrip(msg, 'content').then(() => ({ ok: true }), () => ({ ok: false })) : undefined;
  // the settings page (in a tab or not) enabled a stopped profile again
  if (msg.cmd === 'regexReset') {
    if (!ownPage) return undefined;
    forgetTrips();
    return Promise.resolve({ ok: true });
  }
  if (msg.cmd === 'inpageConfig') {
    return Promise.all([inpageFor(sender, msg), languageReady()]).then(([f]) => (f ? { ...inpageConfig(f), i18n: chosenCatalog() } : null), () => null);
  }
  return undefined;
});

/**
 * hello, plus what the cards preselect: the course used last, the system.
 * passive: a query nobody clicked for — the row states a page asks for on
 * its own, the popup just opened — or the popup's "重新检测" beside "打开
 * LectureFlow" (a fresh look), which must not start LectureFlow; every
 * other click may.
 */
async function connectionFor(recheck, passive = false) {
  const view = await connection(recheck, { noLaunch: passive });
  try {
    view.lastCourseId = String((await browser.storage.local.get(LAST_COURSE_KEY))[LAST_COURSE_KEY] || '');
  } catch {
    view.lastCourseId = '';
  }
  try {
    view.os = (await browser.runtime.getPlatformInfo()).os;
  } catch {
    view.os = '';
  }
  return view;
}

/**
 * Site access the popup can offer on a page with no tracks: a profile whose
 * page is embedded but not readable (no access to its site yet), or else,
 * when the page embeds frames we cannot read, the profiles with in-page
 * entries the user has not granted (an LMS hides the platform's address
 * behind its own launch URL).
 */
async function offersFor(result) {
  if (!result || result.status !== 'ok' || !Array.isArray(result.opaque) || result.opaque.length === 0) return [];
  const entries = await allProfiles();
  const granted = await grantedOrigins();
  const has = (origins) => origins.every((o) => granted.includes(o));
  const out = [];
  for (const src of result.opaque) {
    const u = parseHttpUrl(src);
    if (!u) continue;
    for (const e of entries) {
      const p = e.profile;
      const inList = p.inpage && p.inpage.list && p.match.hosts.some((h) => hostMatches(h, u.hostname));
      if (!(profileMatchesUrl(p, src) || inList)) continue;
      // The embedded page's own site, nothing more (security review L4);
      // only when it cannot be asked for (plain HTTP), the profile's hosts.
      const own = requestableOrigin(src);
      const origins = own ? [own] : profilePermissionOrigins(p, { media: false });
      if (!has(origins) && !out.some((o) => o.id === p.id)) out.push({ id: p.id, name: p.name, origins, embedded: true });
    }
  }
  if (out.length === 0 && result.summary && result.summary.total === 0) {
    for (const e of entries) {
      const p = e.profile;
      if (!p.inpage || out.some((o) => o.id === p.id)) continue;
      // The LMS hides the platform's address: its page hosts (no media hosts).
      const origins = profilePermissionOrigins(p, { media: false });
      if (!has(origins)) out.push({ id: p.id, name: p.name, origins, embedded: false });
    }
  }
  return out;
}

// ---------------------------------------------------------------- ports
//
// The popup, the options page, the confirmation window and the in-page
// cards talk over ports: requests with an id, answers with the same id, and
// pushes (LectureFlow status) while open. A port is taken only from the
// page its name says (security review L3): popup, options and confirm from
// the extension's own pages, inpage from a content script in a web page.
// The popup names its tab; an in-page card's tab and frame are the port's
// sender, never taken from a message; a confirmation window's are its
// ticket's.

function portKind(port) {
  const sender = port.sender || {};
  return portKindOf(port.name, sender, { extensionId: browser.runtime.id, pageUrl: (rel) => browser.runtime.getURL(rel), frameUrl: senderUrl(sender) });
}

/** What an in-page port may be told about a task (src/lib/ports.js). */
function inpageStatus(ctx, status) {
  return statusForPage(status, { platform: ctx.platform, interest: ctx.interest });
}

/** A check item as a web page's frame sees it: without the course. */
function withoutCourse(item) {
  if (!item || typeof item !== 'object') return item ?? null;
  const { courseId: _c, ...rest } = item;
  return rest;
}

browser.runtime.onConnect.addListener((port) => {
  const kind = portKind(port);
  if (!kind) {
    try {
      port.disconnect();
    } catch {
      // gone
    }
    return;
  }
  const sender = port.sender || {};
  const inpage = kind === 'inpage';
  // topUrl: the tab's top-level address, for the partitioned cookies of an
  // embedded player and for where the page is embedded; the browser gives
  // it only with access to that page.
  const ctx = inpage
    ? {
        tabId: sender.tab.id,
        frameId: Number.isInteger(sender.frameId) ? sender.frameId : 0,
        frameUrl: senderUrl(sender),
        topUrl: typeof sender.tab.url === 'string' ? sender.tab.url : '',
        storeId: typeof sender.tab.cookieStoreId === 'string' ? sender.tab.cookieStoreId : '',
        windowId: sender.tab.windowId,
        found: inpageFor(sender),
        platform: '',
        interest: new Set(),
        port,
      }
    : null;
  if (ctx) ctx.found.then((f) => (ctx.platform = f ? f.profile.id : ''), () => {});
  let unsubscribe = null;
  port.onDisconnect.addListener(() => {
    if (unsubscribe) unsubscribe();
    // The page this frame showed is gone: its confirmation windows can no
    // longer act for it.
    if (inpage) for (const t of tickets.values()) if (t.port === port) t.alive = false;
  });
  const subscribe = () => {
    if (unsubscribe) return;
    unsubscribe = nm().onStatus((status) => {
      const out = inpage ? inpageStatus(ctx, status) : status;
      if (!out) return;
      try {
        port.postMessage({ push: 'status', status: out });
      } catch {
        // closed
      }
    });
  };
  port.onMessage.addListener(async (msg) => {
    if (!msg || typeof msg !== 'object') return;
    const reply = (body) => {
      try {
        port.postMessage({ id: msg.id, ...body });
      } catch {
        // closed
      }
    };
    try {
      if (inpage) reply(await inpageCommand(ctx, msg, subscribe));
      else if (kind === 'confirm') reply(await confirmCommand(msg, subscribe));
      else reply(await pageCommand(kind, msg, subscribe));
    } catch (e) {
      reply({ ok: false, error: { code: 'ext.failed', params: { message: String(e && e.message ? e.message : e).slice(0, 200) } } });
    }
  });
});

/**
 * A detection's view for a card, with whether the user turned off the
 * restricted reminder for its platform (src/lib/remember.js) and the choice
 * the lecture was last sent with, for "续传" (src/lib/sentpreset.js).
 */
async function viewFor(r) {
  const view = viewOf(r);
  if (view.status === 'ok') {
    view.remembered = await restrictedRemembered(view.platform);
    view.lastPreset = await sentPreset(r);
  }
  return view;
}

// The popup sends a whole tab's detection: its top-level address is the
// one the detection read (tabs.get, with activeTab or the site's access);
// its cookie store is the tab's (Firefox containers).
async function tabScope(tabId, r) {
  let storeId = '';
  try {
    const tab = await browser.tabs.get(Number(tabId));
    if (tab && typeof tab.cookieStoreId === 'string') storeId = tab.cookieStoreId;
  } catch {
    storeId = '';
  }
  return { tabId: Number(tabId), topUrl: r && r.status === 'ok' ? r.pageUrl : '', storeId };
}

/** The patterns of a list the user has not granted yet. */
async function notGranted(patterns) {
  const out = [];
  for (const o of patterns) {
    let has = false;
    try {
      has = await browser.permissions.contains({ origins: [o] });
    } catch {
      has = false;
    }
    if (!has) out.push(o);
  }
  return out;
}

/** Commands of the popup and the options page. */
async function pageCommand(name, msg, subscribe) {
  const tabId = msg.tabId;
  await languageReady(); // track names and labels are in the interface language
  switch (msg.cmd) {
    case 'detect': {
      let r = msg.force || !lastResult(tabId) ? await detectTab(tabId, { force: !!msg.force }) : lastResult(tabId);
      // the tab moved on while it ran (nothing was stored): once more, on the page it shows now
      if (r && r.status === 'navigated') r = await detectTab(tabId, { force: true });
      return { ok: true, view: await viewFor(r), origins: suggestedOrigins(r), cookieHosts: await notGranted(cookieHostOrigins(r)), offers: await offersFor(r) };
    }
    case 'report':
      return reportFor(tabId);
    case 'reportTabs':
      return { ok: true, tabs: reportableTabs() };
    case 'connection': {
      // passive: the popup opening asks only whether LectureFlow runs, and
      // never starts it (T26 G1); its "重新检测" and "打开 LectureFlow" may
      const view = await connectionFor(!!msg.recheck, !!msg.passive);
      if (view.status === 'connected') subscribe();
      return { ok: true, connection: view };
    }
    case 'check': {
      // Whether this lecture is in LectureFlow is never a reason to start
      // it (SEC42 低-1): if it quit since the popup's hello, the check finds
      // it not running. Sending checks again and may start it.
      const r = lastResult(tabId);
      if (!r || r.status !== 'ok') return fail('ext.stale');
      const c = await checkMany(platformOf(r), [r.sessionId], { noLaunch: true });
      if (!c.ok) return c;
      const item = c.items.find((i) => i.sessionId === r.sessionId) || null;
      return { ok: true, item, status: item && item.taskId ? latestStatus(item.taskId) : null };
    }
    case 'send': {
      subscribe();
      const r = lastResult(tabId);
      return sendResult(r, { trackIds: msg.trackIds, courseId: msg.courseId, overrideDisabled: !!msg.overrideDisabled, rememberPlatform: !!msg.rememberPlatform }, await tabScope(tabId, r));
    }
    case 'refresh': {
      subscribe();
      const r = lastResult(tabId);
      return refreshResult(r, { trackIds: msg.trackIds }, await tabScope(tabId, r));
    }
    case 'open':
      return openInApp({ lectureId: msg.lectureId, taskId: msg.taskId });
    case 'status':
      return { ok: true, status: latestStatus(msg.taskId) };
    default:
      return fail('ext.unknown_command');
  }
}

/**
 * Where an in-page frame sits and how its buttons confirm (src/lib/embed.js):
 * { sub, site, source, route }. The confirmation window acts for its frame
 * after the user confirmed there: route 'window-page'.
 */
async function embedOf(ctx, msg) {
  if (ctx.window) return { sub: true, site: ctx.site, source: '', route: 'window-page' };
  const where = frameSite({ frameId: ctx.frameId, tabUrl: ctx.topUrl, ancestors: Array.isArray(msg.ancestors) ? msg.ancestors : null });
  let trusted = {};
  try {
    trusted = (await browser.storage.local.get(TRUST_KEY))[TRUST_KEY];
  } catch {
    trusted = {};
  }
  return { ...where, route: embedRoute(where, trusted) };
}

/**
 * Commands of an in-page card: this tab and frame only. In a frame inside
 * a site the user has not trusted, nothing is detected, loaded or sent from
 * the page itself: the button opens the extension's confirmation window
 * ("confirmWindow"), whose commands come back here with ctx.window set.
 */
async function inpageCommand(ctx, msg, subscribe) {
  const found = await ctx.found;
  await languageReady();
  if (!found) return fail('ext.not_allowed');
  // A frame in or under a do-not-handle page (the origins it reports with
  // every message; a confirmation window has its ticket's top-level site).
  const blocked = contextCategory({ frameUrl: ctx.frameUrl, topUrl: ctx.topUrl, ancestors: ctx.window ? (ctx.site ? [ctx.site] : null) : ancestorList(msg) });
  if (blocked) return fail('ext.site_blocked', { category: blocked });
  const embed = await embedOf(ctx, msg);
  const inWindowOnly = embed.route === 'window';
  const source = msg.source && typeof msg.source === 'object' ? msg.source : { kind: 'tab' };
  const sessionKey = (sid) => `${ctx.tabId}:${ctx.frameId}:${sid}`;
  // Cookies are read for this frame, in the tab its sender named; the
  // top-level site is the tab's address or, without access to it, the one
  // the browser reported to the frame (location.ancestorOrigins).
  const frameScope = { tabId: ctx.tabId, frameId: ctx.frameId, topUrl: ctx.topUrl || embed.site || '', storeId: ctx.storeId || '' };
  const resolve = () => (source.kind === 'session' ? sessionGet(sessionKey(String(source.sessionId || ''))) : lastResult(ctx.tabId, ctx.frameId));
  const note = (sid) => {
    if (sid && ctx.interest.size < 1000) ctx.interest.add(String(sid));
  };
  const resolved = () => {
    const r = resolve();
    if (r && r.status === 'ok') note(r.sessionId);
    return r;
  };
  switch (msg.cmd) {
    case 'route':
      return { ok: true, route: embed.route, site: embed.site };
    case 'confirmWindow':
      if (ctx.window) return fail('ext.not_allowed');
      return openConfirmWindow(ctx, found, embed, msg);
    case 'detect': {
      if (found.mode !== 'player') return fail('ext.not_allowed');
      if (inWindowOnly) return fail('ext.confirm_in_window');
      const r = await detectTab(ctx.tabId, { force: true, frameId: ctx.frameId, frameUrl: ctx.frameUrl });
      if (r && r.status === 'ok') note(r.sessionId);
      return { ok: true, view: await viewFor(r), origins: [], offers: [] };
    }
    case 'session.load': {
      // The values the content script took from the platform's sessionInfo
      // answer for one listed session (src/lib/inpage.js).
      if (found.mode !== 'list') return fail('ext.not_allowed');
      if (inWindowOnly) return fail('ext.confirm_in_window');
      const sid = String(msg.sessionId || '');
      if (!sid || sid.length > 200 || !sessionInfoRequest(sessionInfoSpec(found.profile, { reviewed: !!found.reviewed }), sid, ctx.frameUrl)) return fail('request.invalid', { field: 'sessionId' });
      const reply = sessionLoadReply({ found, sessionId: sid, values: msg.values && typeof msg.values === 'object' ? msg.values : {}, frameUrl: ctx.frameUrl, locale: uiLocale() });
      if (!reply.ok) return reply;
      const result = reply.result;
      sessions.set(sessionKey(sid), { result, at: Date.now() });
      note(sid);
      return { ok: true, view: await viewFor(result), origins: [], offers: [] };
    }
    case 'connection': {
      const view = await connectionFor(!!msg.recheck, !!msg.passive);
      if (view.status === 'connected') subscribe();
      // A frame that only shows row states has no use for the course list.
      if (inWindowOnly) return { ok: true, connection: { status: view.status, reason: view.reason, params: view.params } };
      return { ok: true, connection: view };
    }
    case 'check': {
      const r = await checkResult(resolved());
      if (r.ok && !ctx.window) return { ...r, item: withoutCourse(r.item), status: inpageStatus(ctx, r.status) };
      return r;
    }
    case 'checkMany': {
      const ids = Array.isArray(msg.sessionIds) ? msg.sessionIds.map(String).slice(0, 500) : [];
      const titles = Array.isArray(msg.titles) ? msg.titles.map((x) => String(x ?? '').slice(0, 300)).slice(0, 500) : undefined;
      for (const id of ids) note(id);
      subscribe();
      const r = await checkMany(found.profile.id, ids, { titles, courseId: msg.courseId ? String(msg.courseId) : '', noLaunch: !!msg.passive });
      if (r.ok && !ctx.window) r.items = r.items.map(withoutCourse);
      return r;
    }
    case 'send': {
      if (inWindowOnly) return fail('ext.confirm_in_window');
      subscribe();
      const r = await sendResult(resolved(), { trackIds: msg.trackIds, courseId: msg.courseId, overrideDisabled: !!msg.overrideDisabled, rememberPlatform: !!msg.rememberPlatform }, frameScope);
      if (!r.ok && r.item && !ctx.window) r.item = withoutCourse(r.item);
      return r;
    }
    case 'refresh':
      if (inWindowOnly) return fail('ext.confirm_in_window');
      subscribe();
      return refreshResult(resolved(), { trackIds: msg.trackIds }, frameScope);
    case 'open':
      return openInApp({ lectureId: msg.lectureId, taskId: msg.taskId });
    case 'status': {
      const st = latestStatus(msg.taskId);
      return { ok: true, status: ctx.window ? st : inpageStatus(ctx, st) };
    }
    default:
      return fail('ext.unknown_command');
  }
}

// ------------------------------------------------- confirmation window
//
// For a button in a frame inside a site the user has not trusted (or whose
// site the browser does not tell), the confirmation happens in the
// extension's own small window (src/confirm/): a page cannot cover it or
// make it transparent. A ticket ties the window to the frame: the tab, the
// frame and its address, the profile, what was clicked (one lecture of the
// player or the list, or the rows of a bulk send). Tickets live in memory;
// one window per frame; the ticket dies with the frame's page.

const tickets = new Map(); // ticket id → ticket
const ticketWindows = new Map(); // window id → ticket id
const WINDOW_W = 420;
const WINDOW_H = 680;

function newTicketId() {
  if (globalThis.crypto && crypto.randomUUID) return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

/**
 * What the window may act on, from the frame's request: null when it does
 * not fit the frame. reason: why the frame asked for the window
 * (windowReason); it only decides, together with the background's own
 * reading of the trusted list, whether the window shows the click-jacking
 * note (windowNotice).
 */
function ticketSpec(found, msg) {
  const spec = ticketScope(found, msg);
  return spec ? { ...spec, reason: windowReason(msg.reason) } : null;
}

function ticketScope(found, msg) {
  const clean = (rows) =>
    (Array.isArray(rows) ? rows : [])
      .slice(0, 500)
      .map((r) => ({ sessionId: String((r && r.sessionId) || ''), title: String((r && r.title) ?? '').replace(/\s+/g, ' ').trim().slice(0, 300) }))
      .filter((r) => r.sessionId && r.sessionId.length <= 200 && !/[\s{}]/.test(r.sessionId));
  if (msg.flow === 'bulk') {
    if (found.mode !== 'list') return null;
    const rows = clean(msg.rows);
    if (rows.length === 0) return null;
    return { flow: 'bulk', source: null, rows, origin: msg.origin === 'selection' ? 'selection' : 'page' };
  }
  const src = msg.source && typeof msg.source === 'object' ? msg.source : {};
  if (src.kind === 'session') {
    if (found.mode !== 'list') return null;
    const rows = clean([{ sessionId: src.sessionId, title: Array.isArray(msg.rows) && msg.rows[0] ? msg.rows[0].title : '' }]);
    if (rows.length === 0) return null;
    return { flow: 'single', source: { kind: 'session', sessionId: rows[0].sessionId }, rows, origin: '' };
  }
  if (found.mode !== 'player') return null;
  return { flow: 'single', source: { kind: 'tab' }, rows: [], origin: '' };
}

async function windowPlace(windowId) {
  try {
    const w = await browser.windows.get(windowId);
    if (!Number.isFinite(w.left) || !Number.isFinite(w.width)) return {};
    return { left: Math.max(0, Math.round(w.left + (w.width - WINDOW_W) / 2)), top: Math.max(0, Math.round(w.top + Math.min(120, Math.max(0, (w.height - WINDOW_H) / 2)))) };
  } catch {
    return {};
  }
}

function dropTicket(id, { notify = true } = {}) {
  const t = tickets.get(id);
  if (!t) return;
  tickets.delete(id);
  if (t.windowId !== null) ticketWindows.delete(t.windowId);
  // The frame checks its rows again (the window may have sent some).
  if (notify && t.alive) browser.tabs.sendMessage(t.tabId, { cmd: 'lf.confirmClosed' }, { frameId: t.frameId }).catch(() => {});
}

async function openConfirmWindow(ctx, found, embed, msg) {
  const spec = ticketSpec(found, msg);
  if (!spec) return fail('request.invalid', { field: 'flow' });
  // One window per frame: a new click replaces the old window.
  for (const [id, t] of [...tickets]) {
    if (t.tabId !== ctx.tabId || t.frameId !== ctx.frameId) continue;
    dropTicket(id, { notify: false });
    if (t.windowId !== null) browser.windows.remove(t.windowId).catch(() => {});
  }
  const id = newTicketId();
  const t = {
    id,
    ...spec,
    sessions: new Set(spec.rows.map((r) => r.sessionId)),
    tabId: ctx.tabId,
    frameId: ctx.frameId,
    frameUrl: ctx.frameUrl,
    topUrl: ctx.topUrl,
    site: embed.site,
    storeId: ctx.storeId,
    found,
    port: ctx.port,
    interest: ctx.interest,
    alive: true,
    windowId: null,
  };
  tickets.set(id, t);
  try {
    const w = await browser.windows.create({ url: browser.runtime.getURL(`confirm/confirm.html?ticket=${encodeURIComponent(id)}`), type: 'popup', width: WINDOW_W, height: WINDOW_H, focused: true, ...(await windowPlace(ctx.windowId)) });
    t.windowId = w.id;
    ticketWindows.set(w.id, id);
  } catch (e) {
    tickets.delete(id);
    return fail('ext.failed', { message: String(e && e.message ? e.message : e).slice(0, 200) });
  }
  return { ok: true };
}

browser.windows.onRemoved.addListener((windowId) => {
  const id = ticketWindows.get(windowId);
  if (id) dropTicket(id);
});

/** What the window shows about its frame (no URLs beyond the hosts). */
async function ticketView(t) {
  const p = t.found.profile;
  let trusted = false;
  try {
    trusted = !!t.site && isTrusted((await browser.storage.local.get(TRUST_KEY))[TRUST_KEY], t.site);
  } catch {
    trusted = false;
  }
  return {
    flow: t.flow,
    source: t.source,
    rows: t.rows,
    origin: t.origin,
    site: t.site,
    reason: t.reason,
    trusted,
    frameHost: parseHttpUrl(t.frameUrl)?.host || '',
    platform: { id: p.id, name: p.name, version: p.version, reviewed: !!t.found.reviewed },
    presets: p.presets || null,
    alive: t.alive,
  };
}

/** A listed session's sessionInfo, asked of the frame that lists it (its own origin and cookies). */
async function frameInfo(t, sessionId) {
  let r;
  try {
    r = await browser.tabs.sendMessage(t.tabId, { cmd: 'lf.sessionInfo', sessionId }, { frameId: t.frameId });
  } catch {
    return { ok: false, status: 0 };
  }
  if (!r || r.ok !== true || !r.values || typeof r.values !== 'object') return { ok: false, status: Number.isInteger(r && r.status) ? r.status : 0 };
  return { ok: true, values: r.values, url: typeof r.url === 'string' ? r.url : '' };
}

/**
 * Commands of a confirmation window: those of its frame's in-page card,
 * limited to what the ticket names, plus "ticket" (what to show), "trust"
 * (add or remove the frame's top-level site) and "fetchInfo".
 */
async function confirmCommand(msg, subscribe) {
  const t = tickets.get(String(msg.ticket || ''));
  if (!t) return fail('ext.confirm_gone');
  await languageReady();
  if (msg.cmd === 'ticket') return { ok: true, ticket: await ticketView(t) };
  if (msg.cmd === 'trust') {
    if (!t.site) return fail('ext.not_allowed');
    try {
      const current = (await browser.storage.local.get(TRUST_KEY))[TRUST_KEY];
      const next = msg.on ? withTrusted(current, t.site) : withoutTrusted(current, t.site);
      if (Object.keys(next).length) await browser.storage.local.set({ [TRUST_KEY]: next });
      else await browser.storage.local.remove(TRUST_KEY);
    } catch {
      return fail('ext.failed', { message: 'storage' });
    }
    return { ok: true, trusted: !!msg.on };
  }
  if (!t.alive) return fail('ext.stale');
  const ctx = { tabId: t.tabId, frameId: t.frameId, frameUrl: t.frameUrl, topUrl: t.topUrl, site: t.site, storeId: t.storeId, found: Promise.resolve(t.found), window: true, interest: t.interest };
  const listed = (sid) => t.sessions.has(String(sid || ''));
  // What the window acts on is the ticket's: one lecture, or the bulk rows.
  let source = t.source;
  if (t.flow === 'bulk') {
    const s = msg.source && typeof msg.source === 'object' ? msg.source : null;
    source = s && s.kind === 'session' && listed(s.sessionId) ? { kind: 'session', sessionId: String(s.sessionId) } : null;
  }
  switch (msg.cmd) {
    case 'detect': {
      if (t.flow !== 'single') return fail('ext.not_allowed');
      if (t.source.kind !== 'session') return inpageCommand(ctx, { cmd: 'detect', force: true }, subscribe);
      const info = await frameInfo(t, t.source.sessionId);
      if (!info.ok) return fail('ext.info_failed', { status: info.status || 0 });
      return inpageCommand(ctx, { cmd: 'session.load', sessionId: t.source.sessionId, values: info.values }, subscribe);
    }
    case 'fetchInfo': {
      if (!listed(msg.sessionId)) return fail('request.invalid', { field: 'sessionId' });
      return frameInfo(t, String(msg.sessionId));
    }
    case 'session.load':
      if (!listed(msg.sessionId)) return fail('request.invalid', { field: 'sessionId' });
      return inpageCommand(ctx, { cmd: 'session.load', sessionId: String(msg.sessionId), values: msg.values }, subscribe);
    case 'checkMany': {
      // the ticket's rows only, each id with its title
      const ids = Array.isArray(msg.sessionIds) ? msg.sessionIds.map(String) : [];
      const titles = Array.isArray(msg.titles) ? msg.titles : null;
      const keep = ids.map((id, i) => [id, titles ? titles[i] : '']).filter(([id]) => listed(id));
      return inpageCommand(ctx, { ...msg, sessionIds: keep.map((x) => x[0]), titles: titles ? keep.map((x) => x[1]) : undefined }, subscribe);
    }
    case 'check':
    case 'send':
    case 'refresh':
      if (!source) return fail('request.invalid', { field: 'source' });
      return inpageCommand(ctx, { ...msg, source }, subscribe);
    case 'connection':
    case 'open':
    case 'status':
      return inpageCommand(ctx, msg, subscribe);
    default:
      return fail('ext.unknown_command');
  }
}

// Test hooks, only in the e2e build (scripts/build.mjs --e2e).
if (BUILD.e2e && BUILD.target === 'firefox') {
  // WebDriver BiDi may not take a tab to moz-extension:// itself; the
  // Firefox test (test/firefox) gets into the extension through this tab.
  browser.runtime.onInstalled.addListener(() => browser.runtime.openOptionsPage());
}
if (BUILD.e2e) {
  globalThis.__lectureflowTest = {
    detectTab,
    viewOf,
    lastResult,
    setBadge,
    reportFor,
    requestsFor: requests.requestsFor,
    syncObserver,
    observeConfigFor,
    sessions,
    tickets,
    // An in-page command as a frame of this tab would send it (the
    // background's own checks, without a content script in between).
    inpageCommand: async (tabId, frameId, frameUrl, msg, topUrl = '') =>
      inpageCommand({ tabId, frameId, frameUrl, topUrl, storeId: '', windowId: null, found: Promise.resolve(findInpage(await allProfiles(), frameUrl)), platform: '', interest: new Set(), port: null }, msg, () => {}),
    setNativeConnect: setTestConnect,
    resetNative: () => nm().close(),
    // Records the arguments of every cookies.getAll call (never the answers).
    recordCookieCalls() {
      const calls = [];
      const real = cookieApi.getAll;
      cookieApi.getAll = (details) => {
        calls.push(JSON.parse(JSON.stringify(details)));
        return real(details);
      };
      return calls;
    },
  };
}
