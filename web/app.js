// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// LectureFlow front end entry (v1.1 A9, A10, A12.1): the shell — narrow nav
// on the left, content in the middle, the collapsible resource inspector on
// the right — hash router, SSE dispatch (§8.2), browser notifications
// (§9.5) and the first-run redirect (§9.2). All data comes from the back
// end; nothing here changes state locally.

import { api, setHandlers, connectEvents, MOCK, MOCK_SCENARIO, DEV } from './api.js';
import { h, clear, toast, actionsDialog, installMenuCloser, preventStrayDrops, preserveFocus, closeDialogsOnRoute, everySecond } from './dom.js';
import { icon } from './icons.js';
import { mountInspector } from './resources.js';
import { mountDashboard } from './view_dashboard.js';
import { mountCourses } from './view_courses.js';
import { mountCourse } from './view_course.js';
import { mountLecture } from './view_lecture.js';
import { mountSystem } from './view_system.js';
import { mountSettings } from './view_settings.js';
import { setUploadGuard } from './uploads.js';
import { setCourseLookup } from './actions.js';
import { mergeJobEvent } from './pipeline.js';
import { openWizard } from './wizard.js';
import { closeMenu, menuButton } from './menu.js';
import * as U from './util.js';
import { t, setLocale, setDevMode, getLocale } from './i18n.js';
import { initialLocale, refreshLocale, chooseLocale, localeChoices, localeName, langAttr, savedZhDisplay } from './language.js';
import { prepareZh, syncZh } from './zh_display.js';
import { mountPlayer } from './view_player.js';
import { coursePart } from './view_transcript.js';
import { loadKinds } from './kinds.js';
import { pauseDevice } from './asr_devices.js';
import { mountOnboarding } from './onboarding.js';
import { mountPrivacy } from './onboarding_privacy.js';
import { obApi } from './onboarding_api.js';
import { loadServers } from './settings_remote.js';
import { downloadsApi } from './downloads.js';

const listeners = new Set();

const state = {
  system: null,
  systemJSON: '',
  resources: null,
  general: null, // settings.general (A5.3): the notification switch lives here
  courses: [],
  coursesLoaded: false,
  currentCourse: null,
  view: null,
  route: null,
  connected: true,
  guide: null, // the setup guide's state (GET /api/onboarding) where read, else null (WD-04)
};

const ctx = {
  system: () => state.system,
  resources: () => state.resources,
  courses: () => state.courses,
  coursesLoaded: () => state.coursesLoaded,
  setSystem,
  refreshSystem,
  refreshCourses,
  newCourse: () => openWizard(ctx), // A8 wizard
  // SSE for components that are not the routed view (wizard, editors):
  // fn(type, ev) with type 'job' or 'changed', or 'reconnected' (ev null:
  // the stream came back, refetch); returns an unsubscribe function.
  subscribe(fn) {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },
  setCurrentCourse(id) {
    if (state.currentCourse === id) return;
    state.currentCourse = id;
    renderNav();
  },
  // round 8 WD-04: the setup guide's state as last read, or null when not
  // known (the workbench's "continue the guide"; U.guideLeft judges it)
  guide: () => state.guide,
};

// The guide's state is read where the page asks anyway (the first launch on
// the workbench, when the guide is not done and the library has courses)
// and once more when the guide's page is left ("leave setup" lands on the
// workbench, or it was done); never by a view (round 8 WD-04).
function setGuide(g) {
  state.guide = g || null;
  if (state.view && state.view.onGuideChanged) state.view.onGuideChanged();
}

function rereadGuide() {
  setGuide(null); // what was read before the guide was open is out of date
  obApi
    .onboarding({ quiet: true })
    .then(setGuide)
    .catch(() => {});
}

setCourseLookup((id) => state.courses.find((c) => c.id === id) || null);

const app = document.getElementById('app');
const nav = document.getElementById('nav');
const main = document.getElementById('main');
const inspEl = document.getElementById('inspector');

// ---- data ----

function setSystem(sys) {
  if (!sys) return;
  const json = JSON.stringify(sys);
  if (json === state.systemJSON) return;
  state.system = sys;
  state.systemJSON = json;
  renderNav();
  inspector.systemChanged();
  if (state.view && state.view.onSystemChanged) state.view.onSystemChanged();
  firstRunCheck(sys);
}

// First run (SPEC §9.2): any error check → diagnostics page. Right after the
// server starts some checks are still running (detail code check.checking);
// the redirect then happens when they finish, if the user is still where
// the page opened.
const CHECK_PENDING = 'check.checking';
let firstRun = { active: true, hash: null, until: Date.now() + 60000 };

function firstRunCheck(sys) {
  if (!firstRun.active) return;
  // the onboarding (SPEC-v3 V4) installs what the checks miss: no redirect
  if (FULL_WINDOW.has(parseRoute(location.hash).name)) {
    firstRun.active = false;
    return;
  }
  if (firstRun.hash === null) firstRun.hash = location.hash;
  const pending = sys.checks.some((c) => c.detail_info && c.detail_info.code === CHECK_PENDING);
  const error = sys.checks.some((c) => c.status === 'error');
  if (error && location.hash === firstRun.hash && parseRoute(location.hash).name !== 'system') {
    firstRun.active = false;
    if (state.route) location.hash = '#/system';
    else history.replaceState(null, '', '#/system'); // before the first route: no hashchange needed
    return;
  }
  if (!pending || Date.now() > firstRun.until || location.hash !== firstRun.hash) firstRun.active = false;
}

async function refreshSystem() {
  const sys = await api.system();
  setSystem(sys);
  return sys;
}

async function refreshCourses() {
  const list = await api.courses();
  state.courses = list || [];
  state.coursesLoaded = true;
  renderNav();
  if (state.view && state.view.onCoursesChanged) state.view.onCoursesChanged();
  return state.courses;
}

async function refreshGeneral() {
  const s = await api.settings();
  if (s && s.general) state.general = s.general;
  renderNav();
}

async function refreshResources() {
  try {
    setResources(await api.resources());
  } catch (e) {
    inspector.setError(e.message);
  }
}

function setResources(r) {
  if (!r) return;
  state.resources = r;
  inspector.setResources(r);
  if (state.view && state.view.onResources) state.view.onResources(r);
  announcePower(r);
}

// The power-aware policy's notices (power management: policy.notices of the
// resources snapshot, "已切换 / 已挂起 / 已恢复：电源感知 - …"): each id is
// announced once, like completions — a toast while the page is looked at,
// else a system notification. Round 8 WD-06 (as T23 一般 1 for the
// failures): only what happens while this page is open; a notice of before
// (its at, sched.PowerNotice, before the page opened: a new browser with its
// own list of the announced, a list cleared) is remembered without a word.
function announcePower(r) {
  for (const n of (r.policy && r.policy.notices) || []) {
    const body = n && n.id && n.info ? U.codedText('notes', n.info, '') : '';
    if (!body) continue;
    const hash = n.lecture_id ? `#/lecture/${encodeURIComponent(n.lecture_id)}` : '#/';
    announce(`power:${n.id}`, n.at, t('nav.notify.power_title'), body, hash, n.info.code === 'power.suspended' ? 'warn' : 'info', body);
  }
}

// ---- layout: nav rail below 1280 px, inspector rail below 1200 px ----

const INSP_KEY = 'lectureflow.inspector';
const NAV_RAIL_BELOW = 1280;
const INSP_OVERLAY_BELOW = 1200;
let inspPref = loadPref();
let inspOverlay = false;

function loadPref() {
  try {
    return localStorage.getItem(INSP_KEY) === 'collapsed' ? 'collapsed' : 'open';
  } catch {
    return 'open';
  }
}

function savePref(v) {
  inspPref = v;
  try {
    localStorage.setItem(INSP_KEY, v);
  } catch {
    // storage unavailable: the choice lasts for this page only
  }
}

// The layout lives on <html data-nav data-insp> (index.html sets it before
// the first paint; ?theme=… is handled there too).
const root = document.documentElement;

function applyLayout() {
  const w = window.innerWidth;
  const navMode = w < NAV_RAIL_BELOW ? 'rail' : 'full';
  if (root.dataset.nav !== navMode) root.dataset.nav = navMode;
  const narrow = w < INSP_OVERLAY_BELOW;
  if (!narrow) inspOverlay = false;
  const mode = narrow ? (inspOverlay ? 'overlay' : 'collapsed') : inspPref;
  if (root.dataset.insp !== mode) {
    root.dataset.insp = mode;
    inspector.render();
  }
}

function collapseInspector() {
  if (window.innerWidth < INSP_OVERLAY_BELOW) inspOverlay = false;
  else savePref('collapsed');
  applyLayout();
  focusInspectorToggle();
}

function expandInspector() {
  if (window.innerWidth < INSP_OVERLAY_BELOW) inspOverlay = true;
  else savePref('open');
  applyLayout();
  focusInspectorToggle();
}

function focusInspectorToggle() {
  const toggle = inspEl.querySelector('.insp-toggle');
  if (toggle) toggle.focus({ preventScroll: true });
}

const inspector = mountInspector(inspEl, {
  onCollapse: collapseInspector,
  onExpand: expandInspector,
  onToggleGPU: toggleGPU,
  onToggleDevice: toggleDevice,
  onRetry: refreshResources,
  system: () => state.system,
  subscribe: ctx.subscribe, // stream-parallel: background playback files in the system area
  // R76 L-5: a Colab shutdown in flight or answered: the views (the
  // workbench's warning) see its busy state and the status it brought
  onRemoteBusy: () => {
    const r = inspector.get();
    if (r) setResources(r);
  },
});
// the workbench's "立即关机" is the panel's (one busy state, R76 L-5)
ctx.colabShutdown = (mode) => inspector.shutdownColab(mode);
ctx.colabBusy = () => !!inspector.colabBusy();

// The overlay inspector (narrow windows) closes on Esc or a click outside it.
document.addEventListener('pointerdown', (e) => {
  if (root.dataset.insp === 'overlay' && !inspEl.contains(e.target) && !e.target.closest('dialog, .ctx-menu')) collapseInspector();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && root.dataset.insp === 'overlay' && inspEl.contains(document.activeElement)) collapseInspector();
});

// GPU pause switch (A10.1; SPEC §17.2 semantics unchanged).
// SPEC-v2 I8: with transcription devices it is the main switch over all of
// them; the transcriptions counted are the running pipeline tasks of every
// device (a device test is not one).
async function toggleGPU() {
  const g = state.resources && state.resources.gpu;
  if (!g) return;
  const devices = state.resources.devices || [];
  const withDevices = devices.length > 0 || !!state.resources.power;
  const running = withDevices ? devices.reduce((n, d) => n + (d.tasks || []).filter((tk) => !tk.selftest).length, 0) : g.asr_slots_used;
  let cancelRunning = false;
  if (!g.paused && running > 0) {
    const choice = await actionsDialog({
      title: withDevices ? t('resources.devices.pause_all_title') : t('nav.gpu.pause_title'),
      message: t('nav.gpu.pause_message', { n: running }),
      actions: [
        { value: 'finish', label: t('nav.gpu.finish') },
        { value: 'cancel', label: t('nav.gpu.cancel_running'), danger: true },
      ],
    });
    if (!choice) return;
    cancelRunning = choice === 'cancel';
  }
  try {
    const sys = await api.setGPU(!g.paused, cancelRunning);
    if (sys) setSystem(sys);
  } catch {
    return; // shown by the API layer
  }
  refreshResources();
}

// One device's switch on its card (SPEC-v2 I8: pausing by device).
async function toggleDevice(card) {
  if (!card) return;
  const running = (card.tasks || []).filter((tk) => !tk.selftest).length;
  const v = await pauseDevice({ id: card.id, label: card.label }, !card.paused, running);
  if (v) refreshResources();
}

// ---- nav (A12.1: narrow, Dashboard / courses / settings; diagnostics and notifications at the bottom) ----

// The course link's tooltip: name, then what is going on in the course.
// T24 S2: and its failed lectures (failed_count, absent from an older server).
function courseTitle(c, busy, attention) {
  const lines = [`${c.code ? c.code + ' ' : ''}${c.name}`];
  if (busy) lines.push(t('nav.course.running', { n: c.running_count }));
  if (c.failed_count > 0) lines.push(t('nav.course.failed', { n: c.failed_count }));
  if (c.draft_count) lines.push(t('nav.course.drafts', { n: c.draft_count }));
  if (attention) lines.push(t('nav.course.stale', { n: c.stale_lecture_count }));
  return lines.join('\n');
}

// The course link's dot: busy, else something to look at (failures, stale
// lectures); its name says every one of them (T24 S2: a screen reader reads
// the link as code, name and this).
function courseDot(c, busy, attention) {
  const failed = c.failed_count > 0;
  if (!busy && !failed && !attention) return null;
  const said = [busy ? t('nav.course.busy') : '', failed ? t('nav.course.failed', { n: c.failed_count }) : '', attention ? t('nav.course.attention') : ''].filter(Boolean);
  return h('span', { class: busy ? 'nav-dot is-busy' : 'nav-dot is-attn', 'aria-label': U.clauseJoin(said) });
}

function navItem(href, ic, label, active, extra = null) {
  return h(
    'a',
    { class: 'nav-item' + (active ? ' is-active' : ''), href, 'aria-current': active ? 'page' : undefined, title: label, dataset: { fkey: 'nav:' + href } },
    icon(ic, { size: 18 }),
    h('span', { class: 'nav-label' }, label),
    extra,
  );
}

let navKey = '';

// Rebuilt only when something it shows changed (no element is replaced
// under the pointer because of an unrelated refresh).
function renderNav() {
  syncTitle(); // the route, the course list or the language changed
  const r = state.route || { name: '' };
  const sys = state.system;
  const hasError = !!sys && sys.checks.some((c) => c.status === 'error');
  const inCourse = r.name === 'course' || r.name === 'lecture';
  const key = JSON.stringify([
    r.name,
    inCourse ? state.currentCourse : null,
    hasError,
    notifyEnabled(),
    'Notification' in window ? Notification.permission : '',
    getLocale(),
    state.courses.map((c) => [c.id, c.code, c.name, c.running_count, c.draft_count, c.stale_lecture_count, c.failed_count || 0]),
  ]);
  if (key === navKey) return;
  navKey = key;
  preserveFocus(() => {
    clear(nav);
    nav.append(
      h(
        'div',
        { class: 'nav-brand' },
        h('a', { class: 'brand', href: '#/', title: 'LectureFlow' }, h('span', { class: 'brand-mark' }, icon('audio-lines', { size: 16, stroke: 2.4 })), h('span', { class: 'brand-name' }, 'LectureFlow')),
        MOCK ? h('span', { class: 'chip chip-warn brand-mock', title: t('nav.mock_title', { scenario: MOCK_SCENARIO }) }, 'MOCK') : null,
      ),
      h(
        'div',
        { class: 'nav-main' },
        navItem('#/', 'layout-dashboard', t('nav.dashboard'), r.name === 'dashboard'),
        navItem('#/courses', 'library', t('nav.courses'), r.name === 'courses', state.courses.length ? h('span', { class: 'nav-count' }, String(state.courses.length)) : null),
        h(
          'ul',
          { class: 'nav-courses', 'aria-label': t('nav.courses') },
          state.courses.map((c) => {
            const active = inCourse && state.currentCourse === c.id;
            const busy = c.running_count > 0;
            const attention = c.stale_lecture_count > 0;
            return h(
              'li',
              null,
              h(
                'a',
                {
                  class: 'nav-course' + (active ? ' is-active' : ''),
                  href: `#/course/${encodeURIComponent(c.id)}`,
                  'aria-current': active ? 'page' : undefined,
                  title: courseTitle(c, busy, attention),
                  dataset: { fkey: 'nav:course:' + c.id },
                },
                h('span', { class: 'nav-course-code' }, c.code || abbr(c.name)),
                h('span', { class: 'nav-course-name' }, c.name),
                courseDot(c, busy, attention),
              ),
            );
          }),
          h('li', null, h('button', { type: 'button', class: 'nav-course nav-add', dataset: { fkey: 'nav:add' }, onclick: () => ctx.newCourse() }, icon('plus', { size: 14 }), h('span', null, t('nav.add_course')))),
        ),
        navItem('#/settings', 'settings', t('nav.settings'), r.name === 'settings'),
      ),
      h(
        'div',
        { class: 'nav-foot' },
        navItem('#/system', 'stethoscope', t('nav.system'), r.name === 'system', hasError ? h('span', { class: 'nav-alert', 'aria-label': t('nav.checks_failed') }) : null),
        notifyToggle(),
        languageSwitch(),
      ),
    );
  });
}

// ---- page title (T23 general 15) ----

// The browser tab, its history and the task bar name the page: "讲座名 ·
// 课程 — LectureFlow", "设置 — LectureFlow". From what the page already has
// (the route, the course list, the view's own heading), with no request of
// its own; a view's heading comes with its data, so the main area is
// watched for it.
const FIXED_TITLES = { dashboard: 'nav.dashboard', courses: 'nav.courses', settings: 'nav.settings', system: 'nav.system', privacy: 'privacy.title', notfound: 'nav.notfound.title' };

function pageTitle() {
  const r = state.route;
  if (!r) return '';
  if (FIXED_TITLES[r.name]) return t(FIXED_TITLES[r.name]);
  const text = (sel) => {
    const e = main.querySelector(sel);
    return e ? e.textContent.trim() : '';
  };
  const courseName = (id) => {
    const c = id ? state.courses.find((x) => x.id === id) : null;
    return c ? c.name : '';
  };
  const both = (page, course) => (page && course ? t('nav.doc_title_in', { page, course }) : page || course);
  if (r.name === 'course') return courseName(r.id) || text('h1.page-title');
  if (r.name === 'lecture') return both(text('h1.page-title'), courseName(state.currentCourse));
  // T28 suggestion 8 (r9 WD-08): the player names the course as the lecture
  // page does (its name, the lecture's course kept from there); without it
  // (opened from the workbench, the course list not loaded) the player's
  // own course code. R88 L1: only when the course kept is this lecture's,
  // which the player's code tells (the code it shows is its lecture's
  // course's, coursePart): another course's page opened from the player and
  // left with "back" keeps that other course
  if (r.name === 'play') {
    const code = text('span.pl-course');
    const kept = state.currentCourse ? state.courses.find((x) => x.id === state.currentCourse) : null;
    return both(text('span.pl-name'), (kept && code && coursePart(kept) === code ? kept.name : '') || code);
  }
  if (r.name === 'onboarding') return text('h1.ob-title');
  return '';
}

function syncTitle() {
  const page = pageTitle();
  // R76 I-2 (round 8 WD-13): the guide's heading that names the app itself
  // ("欢迎使用 LectureFlow") is the title alone, not followed by
  // " — LectureFlow" again (a lecture's title is the user's: as it is)
  const own = !!state.route && state.route.name === 'onboarding' && page.includes('LectureFlow');
  const want = !page ? 'LectureFlow' : own ? page : t('nav.doc_title', { page });
  if (document.title !== want) document.title = want;
}

new MutationObserver(syncTitle).observe(main, { childList: true, subtree: true, characterData: true });

// Course code fallback: initials of the name (A11.3 rule, display only).
function abbr(name) {
  return (name || '')
    .split(/\s+/)
    .filter((w) => /^[A-Za-z]/.test(w))
    .map((w) => w[0].toUpperCase())
    .join('')
    .slice(0, 6) || (name || '').slice(0, 4);
}

// ---- UI language switch (SPEC-v2 P14.7: bottom of the navigation) ----

// Shows the current language in its own name; the menu lists every
// language in its own name (development mode adds en-XA).
function languageSwitch() {
  const cur = getLocale();
  const name = localeName(cur);
  const b = h(
    'button',
    { type: 'button', class: 'nav-item nav-lang', title: t('i18n.language.nav_title', { name }), 'aria-label': t('i18n.language.nav_title', { name }), dataset: { fkey: 'nav:lang' } },
    icon('languages', { size: 18 }),
    h('span', { class: 'nav-label', lang: langAttr(cur) }, name),
  );
  return menuButton(
    b,
    () => localeChoices().map((l) => ({ label: l.name, lang: langAttr(l.code), checked: l.code === getLocale(), onSelect: () => chooseLocale(l.code) })),
    'left',
  );
}

// ---- browser notifications (SPEC §9.5) ----

// The switch is settings.general.notifications (v1.1 A5.3 moves it into the
// settings, replacing SPEC §9.5's localStorage flag): the sidebar toggle and
// the settings page edit the same value. On until the settings are loaded.
function notifyEnabled() {
  return state.general ? !!state.general.notifications : true;
}

function notifyToggle() {
  const supported = 'Notification' in window;
  const denied = supported && Notification.permission === 'denied';
  const on = supported && notifyEnabled() && !denied;
  const title = !supported ? t('nav.notify.unsupported') : denied ? t('nav.notify.denied') : on ? t('nav.notify.on_title') : t('nav.notify.off_title');
  return h(
    'button',
    {
      type: 'button',
      class: 'nav-item nav-toggle' + (on ? '' : ' is-off'),
      role: 'switch',
      'aria-checked': on ? 'true' : 'false',
      // round 8 WD-05: named by its label in both layouts (the rail hides
      // the label and the "开 / 关" text, which left only the title); on or
      // off is aria-checked, what it does or why it cannot (unsupported,
      // denied) the title, its description
      'aria-label': t('nav.notify.label'),
      title,
      disabled: !supported,
      dataset: { fkey: 'nav:notify' },
      onclick: async () => {
        if (denied) {
          toast(t('nav.notify.denied_toast'), 'warn');
          return;
        }
        const next = !notifyEnabled();
        try {
          if (!state.general) await refreshGeneral();
          const s = await api.setGeneral({ ...state.general, notifications: next });
          if (s && s.general) state.general = s.general;
        } catch {
          return; // api.js already showed the server's error
        }
        if (next && Notification.permission === 'default') {
          try {
            await Notification.requestPermission();
          } catch {
            // ignored
          }
        }
        renderNav();
      },
    },
    icon(on ? 'bell' : 'bell-off', { size: 18 }),
    h('span', { class: 'nav-label' }, t('nav.notify.label')),
    h('span', { class: 'nav-state' }, on ? t('nav.notify.on') : t('nav.notify.off')),
  );
}

// Each completion/failure is announced once, also across page reloads: the
// keys (object id + stage + attempt + finished_at) are kept in localStorage.
const NOTIFIED_KEY = 'lectureflow.notified';
const NOTIFIED_MAX = 300;
const notified = loadNotified();

function loadNotified() {
  try {
    const v = JSON.parse(localStorage.getItem(NOTIFIED_KEY) || '[]');
    return new Set(Array.isArray(v) ? v : []);
  } catch {
    return new Set();
  }
}

function rememberNotified(key) {
  notified.add(key);
  try {
    localStorage.setItem(NOTIFIED_KEY, JSON.stringify([...notified].slice(-NOTIFIED_MAX)));
  } catch {
    // storage unavailable: dedupe only for this page
  }
}

// While the user is looking at the page an in-page toast is enough; the
// system notification is for when they are elsewhere. If it cannot be shown
// (switched off, denied, unsupported) the toast is still there on return.
// toastText replaces "{title}：{body}" in the toast (a body that names its
// cause itself, e.g. the power-aware notices); toastOpts are the toast's
// (dom.js toast: sticky).
async function notify(key, title, body, hash, kind, toastText, toastOpts) {
  if (notified.has(key)) return;
  rememberNotified(key);
  const looking = document.visibilityState === 'visible' && document.hasFocus();
  if (!looking && (await systemNotification(key, title, body, hash))) return;
  toast(toastText || t('nav.notify.toast', { title, body }), kind, toastOpts);
}

async function systemNotification(key, title, body, hash) {
  if (!('Notification' in window) || !notifyEnabled()) return false;
  if (Notification.permission === 'default') {
    try {
      await Notification.requestPermission();
    } catch {
      return false;
    }
  }
  if (Notification.permission !== 'granted') return false;
  try {
    const n = new Notification(title, { body, tag: key });
    n.onclick = () => {
      window.focus();
      location.hash = hash;
      n.close();
    };
    return true;
  } catch {
    return false; // some browsers only allow notifications from a service worker
  }
}

// T23 一般 1: only what ends while this page is open is announced. An end
// of before (an event sent again when the stream connects, a new browser
// with its own list of the announced) is remembered without a toast — an
// error toast stays until closed, and the workbench lists those failures.
// NEW_SLACK_MS allows for a clock of the server a little ahead or behind.
const PAGE_OPENED = Date.now();
const NEW_SLACK_MS = 60000;

function endedNow(iso) {
  const ms = Date.parse(iso || '');
  return !Number.isFinite(ms) || ms >= PAGE_OPENED - NEW_SLACK_MS;
}

// notify() for an end at endedAt: remembered only when it is of before.
function announce(key, endedAt, ...args) {
  if (!endedNow(endedAt)) {
    if (!notified.has(key)) rememberNotified(key);
    return;
  }
  notify(key, ...args);
}

async function checkNotify(ev) {
  try {
    if (ev.scope === 'lecture') {
      if (!((ev.status === 'done' && ev.stage === 'export') || ev.status === 'failed')) return;
      const lec = await api.lecture(ev.scope_id);
      if (!lec) return;
      const hash = `#/lecture/${encodeURIComponent(lec.id)}`;
      if (ev.status === 'done' && U.isAllDone(lec.stages)) {
        const st = lec.stages.find((s) => s.stage === 'export');
        const pending = lec.counts.patches_pending;
        const warn = U.warningHint(lec.stages); // done with warnings is still done, not a failure
        const body = pending ? t('nav.notify.done_pending', { title: lec.title, n: pending }) : lec.title;
        announce(`done:${lec.id}:${st.finished_at}`, st.finished_at, t('nav.notify.done_title'), warn ? U.clauseJoin([body, warn]) : body, hash, 'success');
      } else if (ev.status === 'failed') {
        const st = lec.stages.find((s) => s.stage === ev.stage);
        if (st && U.isFinalFailure(st)) {
          announce(`fail:${lec.id}:${st.stage}:${st.attempt}:${st.finished_at}`, st.finished_at, t('nav.notify.failed_title', { stage: U.stageName(st.stage) }), t('nav.notify.failed_body', { name: lec.title, error: U.excerpt(U.stageErrorText(st), 120) }), hash, 'error');
        }
      }
    } else if (ev.status === 'failed') {
      const courses = await api.courses();
      const st = U.findCourseStage(courses || [], ev);
      if (!st || !U.isFinalFailure(st)) return;
      const course = (courses || []).find((c) => c.id === ev.scope_id || (c.materials || []).some((m) => m.id === ev.scope_id));
      if (!course) return;
      announce(
        `fail:${ev.scope_id}:${st.stage}:${st.attempt}:${st.finished_at}`,
        st.finished_at,
        t('nav.notify.failed_title', { stage: U.stageName(st.stage) }),
        t('nav.notify.failed_body', { name: course.name, error: U.excerpt(U.stageErrorText(st), 120) }),
        `#/course/${encodeURIComponent(course.id)}`,
        'error',
      );
    }
  } catch {
    // shown by the API layer
  }
}

// T26 G2: a recording the browser extension sent that failed or needs a
// refresh is announced like a lecture's failure (the same switch and the
// same once-only list): the user closed the extension's window and the app
// is in the tray. A failure that a retry can fix keeps its credentials for
// FAILED_CREDENTIAL_MINUTES only (internal/fetch DefaultFailedCredentialTTL),
// so its text says how long a retry goes on from where it stopped. Read on
// the SSE "changed downloads" (throttled) and when the stream comes back.
// The back end gives that time with no failed task: the two constants are
// held together by r8_dashboard_test.go (TestR8FailedCredentialMinutes).
const FAILED_CREDENTIAL_MINUTES = 30;

// At most one checkDownloads() per DOWNLOADS_MS while the events keep
// coming (R76 M-2: a debounce never ran while other recordings were
// fetching, each sending an event every 500 ms, so a failure waited for all
// of them to stop); a request while it runs runs it once more afterwards.
// The same as downloads.js's throttle, which that module does not export.
const DOWNLOADS_MS = 500;
const downloadsCheck = (() => {
  let timer = null;
  let running = false;
  let again = false;
  async function run() {
    timer = null;
    running = true;
    try {
      await checkDownloads();
    } finally {
      running = false;
      if (again) {
        again = false;
        request();
      }
    }
  }
  function request() {
    if (timer) return;
    if (running) {
      again = true;
      return;
    }
    timer = setTimeout(run, DOWNLOADS_MS);
  }
  return request;
})();

async function checkDownloads() {
  let res;
  try {
    res = await downloadsApi.list('');
  } catch {
    return; // the workbench's line and the course page say it
  }
  for (const d of (res && Array.isArray(res.items) ? res.items : [])) {
    if (!d || (d.state !== 'failed' && d.state !== 'needs_refresh')) continue;
    const name = d.title || t('downloads.untitled');
    const why = U.errorInfoText(d.error, '') || (d.state === 'failed' ? t('downloads.failed_unknown') : t('downloads.needs_refresh.hint'));
    const retry = d.state === 'failed' && (d.allowed_actions || []).includes('retry');
    const body = t('nav.notify.failed_body', { name, error: retry ? U.sentenceJoin([why, t('nav.notify.download_retry_window', { minutes: FAILED_CREDENTIAL_MINUTES })]) : why });
    const hash = d.course_id ? `#/course/${encodeURIComponent(d.course_id)}` : '#/';
    // R76 L-2 (round 8 WD-08): long, and a thing to do (in the time a retry
    // goes on, or in the browser): the toast stays until closed
    announce(`dl:${d.id}:${d.state}:${d.updated_at}`, d.updated_at, t(d.state === 'failed' ? 'nav.notify.download_failed_title' : 'nav.notify.download_refresh_title'), body, hash, 'warn', undefined, { sticky: true });
  }
}

// ---- SSE ----

const banner = document.getElementById('conn-banner');
const timers = {};

function debounce(name, fn, ms) {
  clearTimeout(timers[name]);
  timers[name] = setTimeout(() => fn().catch(() => {}), ms);
}

// T24 G6, T25 G4: "reconnecting in 3 seconds" for ever read as if the page
// still worked. After LONG_DOWN_MS without the service the banner says that
// LectureFlow may have quit and should be opened again (the desktop app's
// window: that its service does not answer); data-i18n follows, so a
// language switch keeps those words. Checked once a second while down.
const LONG_DOWN_MS = 30000;
let downAt = 0;

function setBanner(key) {
  if (banner.dataset.i18n === key && banner.textContent === t(key)) return;
  banner.dataset.i18n = key;
  banner.textContent = t(key);
}

function bannerKey() {
  if (Date.now() - downAt < LONG_DOWN_MS) return 'nav.conn_lost';
  const desktop = !!window.lectureflow && typeof window.lectureflow === 'object';
  return desktop ? 'nav.conn_lost_long_desktop' : 'nav.conn_lost_long';
}

function startEvents() {
  connectEvents({
    onUp(reconnected) {
      state.connected = true;
      banner.hidden = true;
      setBanner('nav.conn_lost');
      inspector.setOffline(false);
      if (reconnected) {
        fullRefresh();
        for (const fn of listeners) fn('reconnected', null);
      }
    },
    onDown() {
      state.connected = false;
      downAt = Date.now();
      setBanner('nav.conn_lost');
      banner.hidden = false;
      // the resource panel no longer shows the readings of before
      inspector.setOffline(true);
      everySecond(banner, () => {
        if (state.connected) return false;
        setBanner(bannerKey());
        return true;
      });
    },
    onJob(ev) {
      // v3.2.x P1-c: an event's stats, llm and last_activity_at go into the
      // stage cards drawn for its lecture (no reload); then the view as before
      mergeJobEvent(ev);
      if (state.view && state.view.onJob) state.view.onJob(ev);
      for (const fn of listeners) fn('job', ev);
      // running counts, glossary_pending: refetch the course list on status changes
      if (ev.status !== 'running') debounce('courses', refreshCourses, 600);
      checkNotify(ev);
    },
    onChanged(ev) {
      if (ev.kind === 'system') debounce('system', refreshSystem, 200);
      if (ev.kind === 'settings') {
        debounce('general', refreshGeneral, 200);
        debounce('locale', refreshLocale, 200); // another page may have switched the language (P14.7)
        debounce('system', refreshSystem, 200); // the diagnostics' role-model check follows the roles and probes
      }
      if (ev.kind === 'course' || ev.kind === 'lecture') debounce('courses', refreshCourses, 300);
      if (ev.kind === 'kinds') debounce('kinds', loadKinds, 200); // the lecture kind catalog (lecture kinds)
      if (ev.kind === 'downloads') downloadsCheck(); // T26 G2 (throttled, R76 M-2)
      if (ev.kind === 'devices') {
        // SPEC-v2 I6-I10: a device row, a pause, a test or the power state
        debounce('system', refreshSystem, 300);
        debounce('resources', refreshResources, 300);
        debounce('servers', loadServers, 300); // ssh-1b: a remote server's state
      }
      if (state.view && state.view.onChanged) state.view.onChanged(ev);
      for (const fn of listeners) fn('changed', ev);
    },
    onResources: setResources,
  });
}

function fullRefresh() {
  loadKinds();
  refreshSystem().catch(() => {});
  refreshCourses().catch(() => {});
  refreshGeneral().catch(() => {});
  refreshResources();
  loadServers(); // ssh-1b
  checkDownloads(); // T26 G2: a download that failed while the stream was down
  if (state.view && state.view.refresh) state.view.refresh();
}

// ---- router (SPEC §9.1, v1.1 A9: "#/" is the dashboard) ----

// Routes shown without the navigation and the inspector (SPEC-v3 V4).
const FULL_WINDOW = new Set(['onboarding', 'privacy']);

// First launch (SPEC-v3 V4.1, v3-brief §7.4): the onboarding opens by itself
// when GET /api/app says it is not done and there is no course yet — only
// when the page opens on the workbench (a link to another page is kept).
// Waits at most 2.5 s; a slow server just shows the workbench.
// After "import existing data" (onboarding step 1, desktop mode) the library
// has courses but this install has no components yet: the onboarding opens
// too while it is not done (its state has data.imported; it continues with
// "your computer"), instead of the diagnostics' red checks.
async function firstLaunch() {
  const hash = location.hash;
  if (hash && hash !== '#/' && hash !== '#') return false;
  const check = Promise.all([obApi.app({ quiet: true }), refreshCourses()]).then(
    async ([info, courses]) => {
      if (!info || info.onboarding_done !== false) return false;
      if ((courses || []).length === 0) return true;
      const st = await obApi.onboarding({ quiet: true }).catch(() => null);
      if (st) setGuide(st); // the workbench's "continue the guide" (WD-04), also after the 2.5 s
      return !!st && !!st.data && st.data.imported === true;
    },
    () => false,
  );
  const timeout = new Promise((resolve) => setTimeout(() => resolve(false), 2500));
  return Promise.race([check, timeout]);
}

export function parseRoute(hash) {
  const path = (hash || '').replace(/^#/, '');
  const parts = path
    .split('/')
    .filter(Boolean)
    .map((p) => {
      try {
        return decodeURIComponent(p);
      } catch {
        return p;
      }
    });
  if (parts.length === 0) return { name: 'dashboard' };
  if (parts[0] === 'courses') return { name: 'courses' };
  if (parts[0] === 'course' && parts[1]) return { name: 'course', id: parts[1], section: parts[2] || null };
  if (parts[0] === 'lecture' && parts[1]) return { name: 'lecture', id: parts[1], tab: parts[2] === 'review' ? 'review' : 'transcript' };
  // #/play/{lecture_id}?t={ms} (SPEC-v2 P1)
  if (parts[0] === 'play' && parts[1]) {
    const [id, query] = parts[1].split('?');
    const raw = new URLSearchParams(query || '').get('t');
    const t = raw === null || raw === '' ? NaN : Number(raw);
    return { name: 'play', id, t: Number.isFinite(t) && t >= 0 ? t : null };
  }
  // #/settings/{block}/{device}: a block of the page, e.g. a device's test (SPEC-v2 I9)
  if (parts[0] === 'settings') return { name: 'settings', section: parts[1] || null, device: parts[2] || null };
  if (parts[0] === 'system') return { name: 'system' };
  // SPEC-v3 V4: the onboarding and its privacy details page
  // #/onboarding/{step}/{local|remote}: a step with the transcription mode chosen (3.1.0)
  if (parts[0] === 'onboarding') return { name: 'onboarding', step: parts[1] || null, branch: parts[2] || null };
  if (parts[0] === 'privacy') return { name: 'privacy' };
  return { name: 'notfound' };
}

function onRoute() {
  closeMenu({ focus: false });
  // G3: the dialogs of the page left close (the back and forward buttons
  // reach here with a modal open)
  const left = closeDialogsOnRoute();
  const route = parseRoute(location.hash);
  if (state.view && state.view.update && state.view.update(route)) {
    state.route = route;
    renderNav();
    // the same view: a focus left on the page's body goes back to the
    // dialog's opener (found again when redrawn), else to the view; one the
    // view placed itself stays (R62 N3)
    left.restore(main);
    return;
  }
  if (state.view && state.view.destroy) state.view.destroy();
  state.view = null;
  clear(main);
  const prevName = state.route ? state.route.name : null;
  state.route = route;
  if (route.name !== 'course' && route.name !== 'lecture' && route.name !== 'play') state.currentCourse = null;
  // The player owns the whole window (SPEC-v2 P2): app.is-player hides the
  // navigation and the resource inspector.
  app.classList.toggle('is-player', route.name === 'play');
  // The onboarding and the privacy page own the whole window too (SPEC-v3 V4).
  app.classList.toggle('is-onboarding', FULL_WINDOW.has(route.name));
  renderNav();
  window.scrollTo(0, 0);
  if (route.name === 'dashboard') state.view = mountDashboard(main, ctx);
  else if (route.name === 'courses') state.view = mountCourses(main, ctx);
  else if (route.name === 'course') state.view = mountCourse(main, route, ctx);
  else if (route.name === 'lecture') state.view = mountLecture(main, route, ctx);
  else if (route.name === 'play') state.view = mountPlayer(main, route, ctx);
  else if (route.name === 'settings') state.view = mountSettings(main, ctx, route);
  else if (route.name === 'system') state.view = mountSystem(main, ctx);
  else if (route.name === 'onboarding') state.view = mountOnboarding(main, ctx, route);
  else if (route.name === 'privacy') state.view = mountPrivacy(main, ctx, prevName);
  else {
    main.append(
      h(
        'div',
        { class: 'page' },
        h('div', { class: 'empty-state' }, h('div', { class: 'empty-icon' }, icon('circle-alert', { size: 28 })), h('h1', { text: t('nav.notfound.title') }), h('a', { class: 'btn btn-primary', href: '#/' }, t('nav.notfound.back'))),
      ),
    );
  }
  // the guide's page left (or done): its state as it was left (WD-04)
  if (prevName === 'onboarding' && route.name !== 'onboarding') rereadGuide();
  main.focus({ preventScroll: true });
}

// ---- UI language (SPEC-v2 P14) ----

// The static text of index.html: data-i18n="key" sets the text,
// data-i18n-aria-label / data-i18n-title set those attributes.
function translateStatic(root = document) {
  for (const el of root.querySelectorAll('[data-i18n]')) el.textContent = t(el.dataset.i18n);
  for (const el of root.querySelectorAll('[data-i18n-aria-label]')) el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel));
  for (const el of root.querySelectorAll('[data-i18n-title]')) el.setAttribute('title', t(el.dataset.i18nTitle));
}

// A language switch (P14.7) takes effect without a reload: the static text,
// the navigation and the inspector are rebuilt and the current view is
// mounted again (views read their text when they render). The scroll
// position and the focused control (data-fkey) are restored once the view
// has rendered them again (views fetch their data first). A change of the
// Chinese content display (P14.6, "zhdisplaychange") re-renders the same
// way; a language switch also moves a display that follows the language.
function onLocaleChange() {
  syncZh({ locale: getLocale() });
  const a = document.activeElement;
  const fkey = a && a.dataset ? a.dataset.fkey || '' : '';
  // a control in the page: keep it where it was on the screen
  const top = fkey && main.contains(a) ? a.getBoundingClientRect().top : null;
  const keep = { y: window.scrollY, fkey, top };
  translateStatic();
  navKey = '';
  renderNav();
  inspector.render();
  if (state.view && state.view.destroy) state.view.destroy();
  state.view = null;
  onRoute();
  restoreAfterRemount(keep);
}

let stopRestore = null;

// Puts the page back where the user was: the focused control (data-fkey)
// is focused again and kept at the same height on the screen, otherwise the
// scroll position is restored. Views fetch their data first and parts of a
// page may arrive later, so this follows the page for up to 3 seconds,
// until the user scrolls, clicks or types.
function restoreAfterRemount({ y, fkey, top }) {
  if (stopRestore) stopRestore();
  let focused = !fkey;
  const step = () => {
    const el = fkey ? document.querySelector(`[data-fkey="${CSS.escape(fkey)}"]`) : null;
    if (el && !focused) {
      el.focus({ preventScroll: true });
      focused = true;
    }
    if (el && top !== null) {
      const dy = el.getBoundingClientRect().top - top;
      if (Math.abs(dy) > 1) window.scrollBy(0, dy);
    } else if (top === null && y > 0) {
      const max = document.documentElement.scrollHeight - window.innerHeight;
      if (Math.abs(window.scrollY - Math.min(y, max)) > 1) window.scrollTo(0, Math.min(y, max));
    }
  };
  const mo = new MutationObserver(step);
  const timer = setTimeout(() => stop(), 3000);
  const events = ['wheel', 'pointerdown', 'keydown', 'touchstart'];
  function stop() {
    mo.disconnect();
    clearTimeout(timer);
    for (const ev of events) window.removeEventListener(ev, stop, true);
    if (stopRestore === stop) stopRestore = null;
  }
  stopRestore = stop;
  mo.observe(document.body, { childList: true, subtree: true });
  for (const ev of events) window.addEventListener(ev, stop, true);
  step();
}

// ---- boot ----

async function boot() {
  // UI language (SPEC-v2 P14.7): the choice saved on the server, else the
  // best match for navigator.languages. Development mode (P14.9): the mock
  // or ?dev=1.
  setDevMode(DEV);
  const locale = await initialLocale();
  // P14.6: a Traditional display is loaded before the first render (at
  // most 1.5 s; a slower load re-renders the page when it is done)
  const zhState = { setting: savedZhDisplay(), locale };
  await Promise.race([prepareZh(zhState), new Promise((resolve) => setTimeout(resolve, 1500))]);
  syncZh(zhState);
  await setLocale(locale);
  translateStatic();
  // the inspector was drawn when this module loaded, before the catalog
  // (its title read "resources.title" until the resources came, 3.2.0 L1)
  inspector.render();
  window.addEventListener('localechange', onLocaleChange);
  window.addEventListener('zhdisplaychange', onLocaleChange);
  installMenuCloser();
  preventStrayDrops();
  applyLayout();
  window.addEventListener('resize', applyLayout);
  // panel transitions only after the first layout (no animation on load)
  requestAnimationFrame(() => requestAnimationFrame(() => app.classList.add('has-motion')));
  // A link to "#main" would be taken as a route, so the skip link is a button.
  document.getElementById('skip-link').addEventListener('click', () => main.focus());
  // Same limits as the server (SPEC §17.4: 4 GB per file; §17.5: keep 2 GB free).
  setUploadGuard((file) => {
    const GB = 1024 * 1024 * 1024;
    if (file.size > 4 * GB) return t('common.upload.too_large');
    const freeMB = state.system && state.system.disk ? state.system.disk.free_mb : 0;
    if (freeMB > 0 && freeMB * 1024 * 1024 - file.size < 2 * GB) {
      // R86 L3 (r9 WD-09, round 9 convention C3): disk sizes in decimal
      // units, as the upload list shows the files' sizes (U.fmtBytes)
      return t('common.upload.no_space', { free: U.fmtBytes(freeMB * 1048576) });
    }
    return null;
  });
  setHandlers({ onError: onRequestError });
  renderNav();
  // ?quiet=1 suppresses the mock notice (screenshots).
  if (MOCK && !new URLSearchParams(location.search).has('quiet') && !sessionStorageFlag('lectureflow.mockToast')) toast(t('nav.mock_toast', { scenario: MOCK_SCENARIO }), 'info');

  // The page renders right away (views fetch their own data and show
  // skeletons); the first-run check (SPEC §9.2) redirects to #/system as soon
  // as GET /api/system reports a red check.
  window.addEventListener('hashchange', onRoute);
  loadKinds(); // lecture kinds (lecture kinds): the pages re-render on "kindschange"
  if (await firstLaunch()) history.replaceState(null, '', '#/onboarding');
  onRoute();
  refreshResources();
  refreshSystem().catch(() => {});
  if (!state.coursesLoaded) refreshCourses().catch(() => {});
  refreshGeneral().catch(() => {});
  loadServers(); // ssh-1b: the SSH servers (nothing shows while the back end has them off)
  startEvents();
}

// The page's handler of failed requests that are not quiet (api.js onError).
// T24 G6: an action that fails while the "connection lost" banner shows:
// the banner says the connection is gone, this says that the action did not
// happen (a short toast, once however many such requests fail). Round 8
// WD-09 (R76 L-3): a read is not an action ("没有完成…再试一次" does not fit
// it), and its page says "could not load" itself (another page opened
// while cut off): no toast. The request's method is err.method (api.js
// ApiError); an error without it is told, as before. dom.js tellFailure
// has the same rule for a dialog's requests (round 8 WH-06).
export function onRequestError(err) {
  if (err.status === 0 && !state.connected) {
    if (err.method !== 'GET' && err.method !== 'HEAD') toast(t('nav.conn_lost_action'), 'warn');
    return;
  }
  toast(err.message, err.status >= 500 || err.status === 0 ? 'error' : 'warn');
}

// true when the flag was already set in this tab (the mock notice once per tab)
function sessionStorageFlag(key) {
  try {
    if (sessionStorage.getItem(key)) return true;
    sessionStorage.setItem(key, '1');
  } catch {
    // ignored
  }
  return false;
}

boot();
