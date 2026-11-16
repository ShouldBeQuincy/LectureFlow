// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 8, the workbench line (r8-web-dash) on the page shell, app.js,
// booted whole on the DOM stand-in of r7_dom.mjs with the page of
// index.html (as r7_app_test.mjs does); each section names its item (WD-nn).
// web/r8_dashboard_test.go starts it with the web directory as a file: URL.
// Stand-in nodes are compared with assert.ok(a === b), never with equal /
// deepEqual.

import { assert, El, body, doc, toastsEl, base, sent, net, settle, until, fk, told, sources, fireWindow } from './r7_dom.mjs';

// ---- the page of index.html ----

function el(tag, id, o = {}) {
  const e = new El(tag);
  if (id) e.attrs.id = id;
  if (o.cls) e.className = o.cls;
  Object.assign(e.dataset, o.data || {});
  return e;
}
const app = el('div', 'app', { cls: 'app' });
const nav = el('nav', 'nav', { cls: 'nav', data: { i18nAriaLabel: 'nav.label' } });
const workspace = el('div', null, { cls: 'workspace' });
const banner = el('div', 'conn-banner', { cls: 'conn-banner', data: { i18n: 'nav.conn_lost' } });
banner.hidden = true;
const main = el('main', 'main');
const inspector = el('aside', 'inspector', { cls: 'inspector', data: { i18nAriaLabel: 'nav.inspector' } });
workspace.append(banner, main);
app.append(nav, workspace, inspector);
body.prepend(el('button', 'skip-link', { cls: 'skip-link', data: { i18n: 'nav.skip' } }), app);

// ---- the server ----

const course = (id, o = {}) => ({ id, code: id.toUpperCase(), name: `Course ${id}`, lecture_count: 3, term_count: 0, draft_count: 0, stale_lecture_count: 0, running_count: 0, created_at: '2026-10-01T10:00:00Z', ...o });
const courses = [course('c1')];
const settings = { general: { notifications: true }, providers: [], roles: [] };
// the setup guide: left at its providers step (not done, the library has a course)
let guide = { step: 'providers', done: false, data: { mode: 'local' } };
let guideDown = false; // the guide's page shows its "could not load" (its steps are not modelled)
let policy = null; // the power policy of the snapshots (WD-06)
let downloads = []; // GET /api/downloads (WD-08)
const resources = () => ({ sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: null, policy });
const dashboard = () => ({ courses: courses.map((c) => ({ ...c, stale_count: 0, patches_pending: 0 })), in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [] });
function server(m, u, b) {
  const path = u.replace(/\?.*$/, '');
  if (path === '/api/settings/general' && m === 'PUT') {
    settings.general = b;
    return { status: 200, body: settings };
  }
  if (path === '/api/settings/ui') return { status: 200, body: { locale: 'zh-CN' } };
  if (path === '/api/app') return { status: 200, body: { onboarding_done: guide.done } };
  if (path === '/api/onboarding' && m === 'GET') return guideDown ? { status: 500, body: { error: 'down' } } : { status: 200, body: guide };
  if (path === '/api/courses') return { status: 200, body: courses };
  if (path === '/api/system') return { status: 200, body: { checks: [], disk: { free_mb: 100000 } } };
  if (path === '/api/settings') return { status: 200, body: settings };
  if (path === '/api/resources') return { status: 200, body: resources() };
  if (path === '/api/dashboard') return { status: 200, body: dashboard() };
  if (path === '/api/downloads') return { status: 200, body: { items: downloads } };
  if (path === '/api/remote/servers') return { status: 404, body: {} };
  // the rest (the onboarding's parts …) is not modelled
  if (m === 'GET') return { status: 404, body: { error: 'not in this stand-in' } };
  return { status: 204, body: null };
}
net.answer = server;

// ---- boot ----

const I = await import(new URL('i18n.js', base).href);
await import(new URL('app.js', base).href);
await until(() => sources.length === 1, 'the event stream');
sources[0].open();
await until(() => I.getLocale() === 'zh-CN' && nav.querySelector('a.nav-course'), 'the navigation with the courses');
const t = I.t;
await settle(60);
told();
const go = async (hash) => {
  location.hash = hash;
  fireWindow('hashchange');
  await settle(60);
};
const reads = (path) => sent.filter((s) => s.replace(/\?.*$/, '') === `GET ${path}`).length;

// ---- WD-04 (T22 G10): the workbench offers to continue the guide ----

{
  const notice = () => main.querySelectorAll('section.asr-notice').find((s) => s.querySelector('a[href="#/onboarding"]')) || null;
  // the boot read the guide's state (the library has a course, the guide
  // is not done): the workbench says it was left, with no request of its own
  await until(() => main.querySelector('section.card-todo'), 'the workbench');
  await until(() => notice(), 'the guide left: the workbench says so');
  assert.equal(reads('/api/onboarding'), 1, 'read once, at the boot');
  // the guide opened and left again, now done: read again once it is left
  guideDown = true;
  await go('#/onboarding');
  const before = reads('/api/onboarding');
  guideDown = false;
  guide = { step: 'done', done: true, data: { mode: 'local' } };
  await go('#/');
  await until(() => reads('/api/onboarding') > before, 'the guide read again once it is left');
  await until(() => main.querySelector('section.card-todo'), 'the workbench');
  await settle(60);
  assert.equal(notice(), null, 'the guide done: nothing');
  // leaving another page does not read it (the settings page reads it for
  // its own banner)
  const n = reads('/api/onboarding');
  await go('#/courses');
  await go('#/');
  assert.equal(reads('/api/onboarding'), n, 'no read when the guide was not open');
  told();
}

// ---- WD-05 (C159): the notification switch keeps its name in the rail ----

{
  // the name as a browser computes it here: aria-label, else the text that
  // shows (the rail hides .nav-label and .nav-state: style.css), else title
  const accName = (b) => {
    const label = b.getAttribute('aria-label');
    if (label) return label;
    const rail = doc.documentElement.dataset.nav === 'rail';
    const shown = rail ? [] : b.querySelectorAll('span').filter((s) => /\bnav-(label|state)\b/.test(s.className));
    return shown.map((s) => s.textContent).join('').trim() || b.getAttribute('title') || '';
  };
  const layout = async (width) => {
    globalThis.innerWidth = width;
    fireWindow('resize');
    await settle(20);
  };
  const sw = () => fk('nav:notify');
  for (const [on, what] of [
    [true, 'on'],
    [false, 'off'],
  ]) {
    await layout(1440);
    assert.equal(doc.documentElement.dataset.nav, 'full');
    assert.equal(sw().getAttribute('role'), 'switch');
    assert.equal(sw().getAttribute('aria-checked'), String(on), `${what}: aria-checked`);
    const full = accName(sw());
    await layout(1000);
    assert.equal(doc.documentElement.dataset.nav, 'rail');
    const rail = accName(sw());
    assert.equal(full, t('nav.notify.label'), `${what}, the full navigation: named by its label alone (${full})`);
    assert.equal(rail, full, `${what}, the rail: the same name (${rail})`);
    assert.equal(sw().getAttribute('aria-checked'), String(on), `${what}, the rail: aria-checked`);
    assert.ok(sw().getAttribute('title'), 'what it does stays in its title');
    if (on) {
      sw().click(); // switched off: the settings answer
      await until(() => sw() && sw().getAttribute('aria-checked') === 'false', 'switched off');
    }
  }
  await layout(1280);
  told();
}

// ---- WD-06 (C157, T23 一般 1): power notices of before the page are not replayed ----

{
  const U = await import(new URL('util.js', base).href);
  const es = sources[sources.length - 1];
  const iso = (ms) => new Date(ms).toISOString();
  // sched.PowerNotice: id "{job}:{code}:{at}", at (UTC RFC3339), info (notes.{code})
  const notice = (job, code, ms) => ({ id: `${job}:${code}:${iso(ms)}`, at: iso(ms), info: { code, params: { lecture: `Lecture ${job}`, device: 'NPU' } }, job_id: job, lecture_id: job, course_id: 'c1' });
  const snap = (notices) => {
    policy = { wait_ac: 0, wait_no_device: 0, battery_machine: false, notices };
    es.emit('resources', resources());
  };
  told();
  // a new browser (its own list of what was announced): the first snapshot
  // carries two notices of before the page opened
  const old1 = notice('J1', 'power.switched', Date.now() - 20 * 60e3);
  const old2 = notice('J2', 'power.suspended', Date.now() - 10 * 60e3);
  assert.ok(U.codedText('notes', old1.info, '') && U.codedText('notes', old2.info, ''), 'notices with their texts');
  snap([old1, old2]);
  await settle(60);
  assert.deepEqual(told(), [], 'the notices of before the page opened: nothing told');
  // the next snapshot has one more, of now: told once
  const now = notice('J3', 'power.resumed', Date.now());
  snap([old1, old2, now]);
  await settle(60);
  assert.deepEqual(told(), [`info: ${U.codedText('notes', now.info, '')}`], 'the notice of now, once');
  snap([old1, old2, now]);
  await settle(60);
  assert.deepEqual(told(), [], 'not twice');
  // a suspension of now: told as a warning, as before
  const sus = notice('J4', 'power.suspended', Date.now());
  snap([old1, old2, now, sus]);
  await settle(60);
  assert.deepEqual(told(), [`warn: ${U.codedText('notes', sus.info, '')}`]);
  policy = null;
  es.emit('resources', resources());
  await settle(20);
}

// ---- WD-08 (R76 L-2): a download's failure, told in the page, stays until closed ----

{
  const U = await import(new URL('util.js', base).href);
  const realSet = globalThis.setTimeout;
  const realClear = globalThis.clearTimeout;
  const fading = new Map();
  let nextId = 1;
  globalThis.setTimeout = (fn, ms, ...args) => {
    if (ms !== 4000) return realSet(fn, ms, ...args);
    const id = `fade-${nextId++}`;
    fading.set(id, fn);
    return id;
  };
  globalThis.clearTimeout = (id) => (typeof id === 'string' && id.startsWith('fade-') ? fading.delete(id) : realClear(id));
  const fade = () => {
    const fns = [...fading.values()];
    fading.clear();
    for (const fn of fns) fn();
  };
  try {
    const es = sources[sources.length - 1];
    const iso = (ms) => new Date(ms).toISOString();
    const dl = (id, state, error, o = {}) => ({ id, platform: 'x', course_id: 'c1', title: `Recording ${id}`, state, error, allowed_actions: [], created_at: iso(Date.now() - 60e3), updated_at: iso(Date.now()), tracks: [], ...o });
    told();
    // the user looks at the page: a toast, with the 30 minutes to retry in
    downloads = [dl(21, 'failed', { code: 'downloads.network', params: {} }, { allowed_actions: ['retry', 'remove'] }), dl(22, 'needs_refresh', { code: 'downloads.credentials_expired', params: {} })];
    es.emit('changed', { kind: 'downloads' });
    await until(() => toastsEl.children.length >= 2, 'the downloads told', 300);
    fade();
    const shown = toastsEl.children.map((e) => e.dataset.msg);
    assert.ok(shown.some((m) => m.includes('Recording 21') && m.includes(t('nav.notify.download_retry_window', { minutes: 30 }))), 'the failure with its 30 minutes stays after 4 seconds');
    assert.ok(shown.some((m) => m.includes('Recording 22') && m.includes(U.errorInfoText({ code: 'downloads.credentials_expired', params: {} }, ''))), 'so does the one that needs a refresh');
    for (const e of [...toastsEl.children]) e.querySelector('.toast-close').click();
    assert.equal(toastsEl.children.length, 0, 'closed by their buttons');
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
    downloads = [];
  }
  told();
}

// ---- WD-09 (R76 L-3): cut off, a read that fails is not told as an action ----

{
  const App = await import(new URL('app.js', base).href);
  const A = await import(new URL('api.js', base).href);
  const es = sources[sources.length - 1];
  net.down = true;
  es.drop();
  assert.ok(!banner.hidden, 'the banner shows');
  told();
  // the page's handler of failed requests: an error as api.js gives it, with
  // the request's method (ApiError.method)
  const failed = (method) => Object.assign(new A.ApiError(0, t('common.net.unreachable')), { method });
  App.onRequestError(failed('GET'));
  assert.deepEqual(told(), [], 'a read (another page opened): its page says "could not load", no toast');
  App.onRequestError(failed('POST'));
  assert.deepEqual(told(), [`warn: ${t('nav.conn_lost_action')}`], 'an action: told that it did not happen');
  App.onRequestError(failed('DELETE'));
  App.onRequestError(failed('PUT'));
  assert.deepEqual(told(), [`warn: ${t('nav.conn_lost_action')}`], 'the other writes too, one toast');
  // through the API layer: an action is told
  await A.api.lectureStageAction('L1', 'translate', 'retry').catch(() => {});
  assert.deepEqual(told(), [`warn: ${t('nav.conn_lost_action')}`], 'a POST that fails while cut off: told');
  // connected: a failure is told with its own words, read or not
  net.down = false;
  await until(() => sources[sources.length - 1] !== es, 'the reconnect (3 s)', 600);
  sources[sources.length - 1].open();
  await settle(60);
  told();
  App.onRequestError(Object.assign(new A.ApiError(500, 'boom'), { method: 'GET' }));
  assert.deepEqual(told(), ['error: boom'], 'connected: as before');
}

// ---- WD-13 (R76 I-2): a page title that names the app is not followed by it again ----

{
  guideDown = true; // the guide's page shows its "could not load" (its heading is set below)
  await go('#/onboarding');
  const h1 = await (async () => {
    await until(() => main.querySelector('h1.ob-title'), "the guide's heading");
    return main.querySelector('h1.ob-title');
  })();
  // the guide's welcome heading names the app: the title is the heading alone
  h1.textContent = t('onboarding.welcome.title');
  await until(() => doc.title === t('onboarding.welcome.title'), `the welcome heading alone (${doc.title})`);
  assert.equal(doc.title.split('LectureFlow').length - 1, 1, 'the app named once');
  // another heading: followed by the app's name as before
  h1.textContent = 'Your computer';
  await until(() => doc.title === t('nav.doc_title', { page: 'Your computer' }), `a heading with the app's name after it (${doc.title})`);
  guideDown = false;
  await go('#/');
  assert.equal(doc.title, t('nav.doc_title', { page: t('nav.dashboard') }), 'the workbench as before');
  told();
}

console.log('r8 app checks: ok');
process.exit(0);
