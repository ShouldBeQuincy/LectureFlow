// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 7 (the user walk-throughs, .agent-log/reports T22-T26) on the page
// shell, app.js, booted whole on the DOM stand-in of r7_dom.mjs with the
// page of index.html:
//   T24 S2      a course's failures in the navigation (tooltip and the
//               name a screen reader reads);
// (the later sections name their own items).
// web/r7_dashboard_test.go starts it with the web directory as a file: URL.

import { assert, El, body, doc, toastsEl, base, sent, net, settle, until, fk, told, tick, clock, sources, notes, fireWindow } from './r7_dom.mjs';

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
let courses = [course('c1', { failed_count: 2 }), course('c2', { failed_count: 0 }), course('c3')];
let settings = { general: { notifications: true }, providers: [], roles: [] };
let system = { checks: [], disk: { free_mb: 100000 } };
let remote = null; // the Colab status of the snapshots (R76 L-5)
const resources = () => ({ sampled_at: new Date(clock.now).toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote });
let dashboard = () => ({ courses: [], in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [] });
let downloads = [];
const lectures = new Map();
function server(m, u) {
  const path = u.replace(/\?.*$/, '');
  if (path === '/api/settings/ui') return { status: 200, body: { locale: 'zh-CN' } };
  if (path === '/api/app') return { status: 200, body: { onboarding_done: true } };
  if (path === '/api/courses') return { status: 200, body: courses };
  if (path === '/api/system') return { status: 200, body: system };
  if (path === '/api/settings') return { status: 200, body: settings };
  if (path === '/api/resources') return { status: 200, body: resources() };
  if (path === '/api/dashboard') return { status: 200, body: { ...dashboard(), courses: dashboard().courses.length ? dashboard().courses : courses.map((c) => ({ ...c, stale_count: 0, patches_pending: 0 })) } };
  if (path === '/api/downloads') return { status: 200, body: { items: downloads } };
  if (path === '/api/remote/servers') return { status: 404, body: {} };
  const lec = /^\/api\/lectures\/([^/]+)$/.exec(path);
  if (lec && lectures.has(decodeURIComponent(lec[1]))) return { status: 200, body: lectures.get(decodeURIComponent(lec[1])) };
  // the rest (a course's page, the settings' parts …) is not modelled: the
  // views show their "could not load" (the page title does not need them)
  if (m === 'GET') return { status: 404, body: { error: 'not in this stand-in' } };
  return { status: 204, body: null };
}
net.answer = server;

// ---- boot ----

const I = await import(new URL('i18n.js', base).href);
const U = await import(new URL('util.js', base).href);
await import(new URL('app.js', base).href);
await until(() => sources.length === 1, 'the event stream');
sources[0].open();
await until(() => I.getLocale() === 'zh-CN' && nav.querySelector('a.nav-course'), 'the navigation with the courses');
const t = I.t;
await settle(60);
told();

// ---- T24 S2: a course's failures in the navigation ----

{
  const link = (id) => fk(`nav:course:${id}`);
  const dotLabel = (a) => {
    const d = a.querySelector('span.nav-dot');
    return d ? d.getAttribute('aria-label') || '' : '';
  };
  const failed = t('nav.course.failed', { n: 2 });
  assert.ok(link('c1').getAttribute('title').includes(failed), 'the tooltip names the failures');
  assert.ok(dotLabel(link('c1')).includes(failed), 'the name a screen reader reads too');
  assert.ok(!/失败/.test(link('c2').getAttribute('title')) && !dotLabel(link('c2')), 'no failure, nothing said');
  assert.ok(!/失败/.test(link('c3').getAttribute('title')), 'a server without the count: nothing said');
}

// ---- T24 G6, T25 G4: the service gone ----

{
  const A = await import(new URL('api.js', base).href);
  const live = () => inspector.textContent.includes(t('resources.live'));
  await until(live, 'the resource panel live');
  const es = sources[sources.length - 1];
  net.down = true;
  es.drop();
  assert.ok(!banner.hidden && banner.textContent === t('nav.conn_lost'), 'the banner at once');
  // the resource panel says it is cut off and shows no readings of before
  assert.ok(inspector.textContent.includes(t('resources.offline')), 'the panel says "disconnected"');
  assert.ok(!live(), 'not "live"');
  assert.ok(!inspector.querySelector('div.gpu-body') && !inspector.querySelector('div.tiles'), 'no readings of before the service went');
  // an action while cut off: told, not silent (the request fails as status 0)
  told();
  await A.api.lectureStageAction('L1', 'translate', 'retry').catch(() => {});
  assert.deepEqual(told(), [`warn: ${t('nav.conn_lost_action')}`], 'a failed action is told');
  // more than 30 seconds: LectureFlow may have quit (a page in a browser)
  tick(29);
  assert.equal(banner.textContent, t('nav.conn_lost'), 'still the first words under 30 seconds');
  tick(2);
  assert.equal(banner.textContent, t('nav.conn_lost_long'), 'after 30 seconds: it may have quit, open it again');
  assert.equal(banner.dataset.i18n, 'nav.conn_lost_long', 'a language switch keeps these words');
  // the desktop app's window: its own words
  globalThis.lectureflow = { pickFiles() {} };
  tick(1);
  assert.equal(banner.textContent, t('nav.conn_lost_long_desktop'), 'the desktop app: its words');
  delete globalThis.lectureflow;
  // back: the banner goes and says the first words again; the panel is live
  net.down = false;
  await until(() => sources.length > 1 && sources[sources.length - 1] !== es, 'the reconnect (3 s)', 600);
  sources[sources.length - 1].open();
  assert.ok(banner.hidden, 'the banner goes');
  assert.equal(banner.dataset.i18n, 'nav.conn_lost');
  await until(live, 'the panel live again');
  assert.ok(!inspector.textContent.includes(t('resources.offline')), 'no longer "disconnected"');
  told();
}

// ---- T23 一般 1: only what happens while the page is open is announced ----

{
  const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: 0, attempt: 1, error: null, allowed_actions: [], started_at: null, finished_at: null, ...o });
  const lec = (id, stages) => ({ id, course_id: 'c1', title: `Lecture ${id}`, week: 0, kind: 'lecture', draft: false, counts: { sentences: 0, patches_pending: 0 }, exports: [], stages });
  const iso = (ms) => new Date(ms).toISOString();
  const err = { code: 'media.corrupt', params: {} };
  lectures.set('OLD', lec('OLD', [stage('prep', 'failed', { error_info: err, error: 'x', finished_at: iso(clock.now - 5 * 3600e3) })]));
  lectures.set('NEW', lec('NEW', [stage('prep', 'failed', { error_info: err, error: 'x', finished_at: iso(clock.now) })]));
  lectures.set('DONE', lec('DONE', ['prep', 'asr', 'summary', 'repair', 'translate', 'export'].map((n) => stage(n, 'done', { finished_at: iso(clock.now - 3 * 3600e3) }))));
  const es = sources[sources.length - 1];
  told();
  notes.length = 0;
  // a failure of hours ago (a new browser, an event sent again): no toast,
  // no notification — the workbench lists it
  es.emit('job', { scope: 'lecture', scope_id: 'OLD', stage: 'prep', status: 'failed', progress: 0 });
  es.emit('job', { scope: 'lecture', scope_id: 'DONE', stage: 'export', status: 'done', progress: 1 });
  await until(() => sent.some((s) => s.startsWith('GET /api/lectures/OLD')) && sent.some((s) => s.startsWith('GET /api/lectures/DONE')), 'the lectures read');
  await settle(60);
  assert.deepEqual(told(), [], 'nothing of before the page opened is toasted');
  assert.equal(notes.length, 0, 'nor notified');
  // a failure now: told as before
  es.emit('job', { scope: 'lecture', scope_id: 'NEW', stage: 'prep', status: 'failed', progress: 0 });
  await until(() => toastsEl.children.length > 0, 'the failure of now told');
  const msgs = told();
  assert.equal(msgs.length, 1);
  assert.ok(msgs[0].startsWith('error: ') && msgs[0].includes('Lecture NEW'), `the failure of now (${msgs[0]})`);
}

// ---- T23 一般 15: the page title follows the route ----

{
  const go = async (hash) => {
    location.hash = hash;
    fireWindow('hashchange');
    await settle(40);
  };
  const title = (page) => t('nav.doc_title', { page });
  await go('#/settings');
  assert.equal(doc.title, title(t('nav.settings')), 'Settings');
  await go('#/courses');
  assert.equal(doc.title, title(t('nav.courses')), 'the course list');
  await go('#/course/c2');
  assert.equal(doc.title, title('Course c2'), 'a course: its name');
  // a lecture: its title (from the page's own heading, once loaded) and its course
  const stage = (name, status) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null });
  lectures.set('L9', { id: 'L9', course_id: 'c1', title: 'Block ciphers', label: 'L09', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z', duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [], allowed_actions: [], stages: ['prep', 'asr', 'summary', 'repair', 'translate', 'export'].map((n) => stage(n, 'pending')) });
  await go('#/lecture/L9');
  const want = title(t('nav.doc_title_in', { page: 'Block ciphers', course: 'Course c1' }));
  await until(() => doc.title === want, `the lecture's title and course (${doc.title})`);
  await go('#/nowhere');
  assert.equal(doc.title, title(t('nav.notfound.title')), 'a page that does not exist');
  await go('#/');
  assert.equal(doc.title, title(t('nav.dashboard')), 'the workbench');
  told();
}

// ---- T26 G2: a download that failed or needs a refresh is announced ----

{
  const iso = (ms) => new Date(ms).toISOString();
  const dl = (id, state, error, o = {}) => ({ id, platform: 'x', course_id: 'c1', title: `Recording ${id}`, state, error, allowed_actions: [], created_at: iso(clock.now - 60e3), updated_at: iso(clock.now), tracks: [], ...o });
  downloads = [
    dl(7, 'failed', { code: 'downloads.network', params: {} }, { allowed_actions: ['retry', 'remove'] }),
    dl(8, 'needs_refresh', { code: 'downloads.credentials_expired', params: {} }),
    dl(9, 'failed', { code: 'downloads.network', params: {} }, { allowed_actions: ['retry', 'remove'], updated_at: iso(clock.now - 3 * 3600e3) }),
    dl(10, 'downloading', null),
  ];
  const es = sources[sources.length - 1];
  notes.length = 0;
  told();
  doc.focused = false; // the user is elsewhere: system notifications
  es.emit('changed', { kind: 'downloads' });
  await until(() => notes.length >= 2, 'the notifications of the downloads', 300);
  await settle(80);
  const by = (s) => notes.find((n) => n.body && n.body.includes(s));
  const failed = by('Recording 7');
  assert.ok(failed, 'the failed download is notified');
  assert.equal(failed.title, t('nav.notify.download_failed_title'));
  assert.ok(failed.body.includes(U.errorInfoText({ code: 'downloads.network', params: {} }, '')), 'with why');
  assert.ok(failed.body.includes(t('nav.notify.download_retry_window', { minutes: 30 })), 'and how long a retry can go on from where it stopped');
  const refresh = by('Recording 8');
  assert.ok(refresh && refresh.title === t('nav.notify.download_refresh_title'), 'the download that needs a refresh is notified');
  assert.ok(!refresh.body.includes(t('nav.notify.download_retry_window', { minutes: 30 })), 'a refresh has no retry window');
  assert.ok(!by('Recording 9'), 'a failure of before the page opened is not');
  assert.ok(!by('Recording 10'), 'a download that runs is not');
  assert.equal(notes.length, 2);
  // the next change of the list: nothing twice
  es.emit('changed', { kind: 'downloads' });
  await settle(700);
  assert.equal(notes.length, 2, 'each announced once');
  // R76 M-2: other recordings still fetching send an event every 300 ms; a
  // failure meanwhile is announced without waiting for them to stop
  downloads = [...downloads, dl(12, 'failed', { code: 'downloads.network', params: {} }, { allowed_actions: ['retry', 'remove'], updated_at: iso(clock.now) })];
  let seenAfter = 0;
  for (let i = 1; i <= 12 && !seenAfter; i++) {
    es.emit('changed', { kind: 'downloads' });
    await settle(300);
    if (by('Recording 12')) seenAfter = i;
  }
  assert.ok(seenAfter > 0 && seenAfter <= 5, `announced while the events go on (after ${seenAfter || 'more than 12'} events)`);
  // R76 M10a: a failure no retry can fix (its actions have no retry) does
  // not say that a retry goes on from where it stopped
  downloads = [...downloads, dl(11, 'failed', { code: 'downloads.drm_protected', params: {} }, { allowed_actions: ['remove'] })];
  es.emit('changed', { kind: 'downloads' });
  await until(() => by('Recording 11'), 'the failure without a retry announced', 300);
  assert.ok(!by('Recording 11').body.includes(t('nav.notify.download_retry_window', { minutes: 30 })), 'no retry window without a retry');
  doc.focused = true;
  told();
}

// ---- R76 M9: the page title follows the view's heading as it changes ----

{
  location.hash = '#/lecture/L9';
  fireWindow('hashchange');
  const title = (page) => t('nav.doc_title', { page: t('nav.doc_title_in', { page, course: 'Course c1' }) });
  await until(() => doc.title === title('Block ciphers'), `the lecture's title (${doc.title})`);
  // the heading changes with nothing else (a rename seen by the view): the
  // title follows it, by watching the page, not by the navigation's redraw
  main.querySelector('h1.page-title').textContent = 'Stream ciphers';
  await until(() => doc.title === title('Stream ciphers'), `the renamed lecture's title (${doc.title})`);
  location.hash = '#/';
  fireWindow('hashchange');
  await settle(40);
  told();
}

// ---- R76 M7a: a second loss of the connection counts its 30 seconds anew ----

{
  const es = sources[sources.length - 1];
  net.down = true;
  es.drop();
  assert.ok(!banner.hidden && banner.textContent === t('nav.conn_lost'), 'the banner at once');
  tick(29);
  assert.equal(banner.textContent, t('nav.conn_lost'), 'the first words for 30 seconds again, not from the first loss');
  tick(2);
  assert.equal(banner.textContent, t('nav.conn_lost_long'), 'then that LectureFlow may have quit');
  net.down = false;
  await until(() => sources[sources.length - 1] !== es, 'the reconnect (3 s)', 600);
  sources[sources.length - 1].open();
  assert.ok(banner.hidden, 'back');
  await settle(60);
  told();
}

// ---- R76 L-5: the workbench's and the panel's "立即关机" are one action ----

{
  const { dialogs, confirmDialogs } = await import('./r7_dom.mjs');
  const running = { configured: true, state: 'running', accelerator: 'L4', tier: 'paid', window_used_sec: 3600, window_total_sec: 14400, lectures_left: 3, session_spent_cu: 1.31, cu_per_hour: 1.54, runtime: { held: true, reachable: 'unreachable', billing: true } };
  const es = sources[sources.length - 1];
  const show = async (r) => {
    remote = r;
    es.emit('resources', resources());
    await settle(40);
  };
  // the shutdown request is held until the check lets it go
  let letGo = null;
  net.answer = (m, u, b) => {
    if (m === 'POST' && u.startsWith('/api/colab/shutdown')) return new Promise((resolve) => (letGo = () => resolve({ status: 200, body: { status: { ...running, state: 'stopping' } } })));
    return server(m, u, b);
  };
  const dash = () => fk('dash:colab:now');
  const panel = () => fk('insp:remote:now');
  for (const [first, other, what] of [
    [panel, dash, 'from the panel: the workbench shows it running'],
    [dash, panel, 'from the workbench: the panel shows it running'],
  ]) {
    await show(running);
    assert.ok(first() && other() && !first().disabled && !other().disabled, 'both offered');
    letGo = null;
    first().click();
    await until(() => dialogs.some((d) => d.open), 'the confirmation');
    await confirmDialogs();
    await until(() => letGo, 'the shutdown request');
    assert.equal(sent.filter((s) => s.startsWith('POST /api/colab/shutdown')).length > 0, true);
    assert.ok(other() && other().disabled, what);
    letGo();
    await until(() => !dash(), 'the shutdown answered: the session is being released');
    assert.equal(dialogs.filter((d) => d.open).length, 0);
  }
  net.answer = server;
  await show(null);
  told();
}

console.log('r7 app checks: ok');
process.exit(0);
