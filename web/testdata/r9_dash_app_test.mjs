// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the workbench line (r9-web-dash) on the page shell, app.js,
// booted whole on the DOM stand-in of r7_dom.mjs with the page of
// index.html (as r7_app_test.mjs does); each section names its item (WD-nn).
// web/r9_dash_items_test.go starts it with the web directory as a file: URL.
// Stand-in nodes are compared with assert.ok(a === b), never with equal /
// deepEqual.

import { assert, El, body, doc, base, sent, net, settle, until, told, sources, fireWindow } from './r7_dom.mjs';

// What the player asks of the stand-in (as r8_lecture_test.mjs gives it):
// selector lists and :not(), media elements, insertion after a node, style
// properties, animation frames, layout boxes, scrolling.
{
  const proto = El.prototype;
  const baseMatches = proto.matches;
  proto.matches = function (sel) {
    if (sel.includes(',')) return sel.split(',').some((s) => this.matches(s.trim()));
    const nots = [];
    const core = sel.replace(/:not\(([^)]*)\)/g, (_, x) => (nots.push(x), ''));
    if (nots.length) return (core === '' || baseMatches.call(this, core)) && !nots.some((x) => this.matches(x));
    return baseMatches.call(this, sel);
  };
  proto.scrollIntoView = function () {};
  proto.play = function () {
    this.paused = false;
    return Promise.resolve();
  };
  proto.pause = function () {
    this.paused = true;
  };
  proto.load = function () {};
  proto.getClientRects = function () {
    return [{}];
  };
  Object.defineProperty(proto, 'played', { get: () => ({ length: 0 }), configurable: true });
  Object.defineProperty(proto, 'value', {
    get() {
      return this._value ?? '';
    },
    set(v) {
      this._value = v;
    },
    configurable: true,
  });
  proto.after = function (...ns) {
    const p = this.parentNode;
    const ref = this.nextSibling;
    for (const n of ns) (ref ? p.insertBefore(n, ref) : p.append(n));
  };
  proto.scrollTo = function () {};
  const withStyle = (e) => {
    Object.defineProperty(e.style, 'setProperty', { value(k, v) { this[k] = v; } });
    Object.defineProperty(e.style, 'removeProperty', { value(k) { delete this[k]; } });
    return e;
  };
  const mk = doc.createElement;
  doc.createElement = (tag) => withStyle(mk(tag));
  const mkNS = doc.createElementNS;
  doc.createElementNS = (ns, tag) => withStyle(mkNS(ns, tag));
  globalThis.cancelAnimationFrame = (id) => clearTimeout(id);
}

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
const courses = [course('c1'), course('c2')];
const system = { checks: [], disk: { free_mb: 100000 } };
const resources = () => ({ sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: null });
const dashboard = () => ({ courses: courses.map((c) => ({ ...c, stale_count: 0, patches_pending: 0 })), in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [] });
const stage = (name, status) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null });
const lectures = new Map();
lectures.set('L9', { id: 'L9', course_id: 'c1', title: 'Block ciphers', label: 'L09', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z', duration_ms: 60000, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: true, lecture_actions: [], allowed_actions: [], stream: { status: 'ready' }, stages: ['prep', 'asr', 'summary', 'repair', 'translate', 'export'].map((n) => stage(n, 'done')) });
function server(m, u) {
  const path = u.replace(/\?.*$/, '');
  if (path === '/api/settings/ui') return { status: 200, body: { locale: 'zh-CN' } };
  if (path === '/api/app') return { status: 200, body: { onboarding_done: true } };
  if (path === '/api/courses') return { status: 200, body: courses };
  if (path === '/api/system') return { status: 200, body: system };
  if (path === '/api/settings') return { status: 200, body: { general: {}, providers: [], roles: [] } };
  if (path === '/api/resources') return { status: 200, body: resources() };
  if (path === '/api/dashboard') return { status: 200, body: dashboard() };
  if (path === '/api/downloads') return { status: 200, body: { items: [] } };
  if (path === '/api/remote/servers') return { status: 404, body: {} };
  if (/^\/api\/lectures\/[^/]+\/sentences$/.test(path)) return { status: 200, body: { total: 0, sentences: [] } };
  if (/^\/api\/lectures\/[^/]+\/cues$/.test(path)) return { status: 200, body: { cues: [] } };
  const lec = /^\/api\/lectures\/([^/]+)$/.exec(path);
  if (lec && lectures.has(decodeURIComponent(lec[1]))) return { status: 200, body: lectures.get(decodeURIComponent(lec[1])) };
  // the rest (the player's text, a course's page …) is not modelled
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
const title = (page) => t('nav.doc_title', { page });

// ---- WD-08 (T28 suggestion 8): the player's title names the course as the lecture page's ----

{
  await go('#/lecture/L9');
  const lecturePage = title(t('nav.doc_title_in', { page: 'Block ciphers', course: 'Course c1' }));
  await until(() => doc.title === lecturePage, `the lecture page: its title and its course's name (${doc.title})`);
  // the player, opened from the lecture page: the same course part
  await go('#/play/L9');
  await until(() => main.querySelector('span.pl-name') && main.querySelector('span.pl-name').textContent === 'Block ciphers', "the player's heading");
  await until(() => doc.title === lecturePage, `the player: the same as the lecture page (${doc.title})`);
  // opened from the workbench (no course kept): the player's own course
  // code; or the course's name, once the player keeps its lecture's course
  // itself (the page line's view_player.js, round 9): this lecture's course
  // either way, as the course kept is then this lecture's
  await go('#/');
  await go('#/play/L9');
  await until(() => main.querySelector('span.pl-name') && main.querySelector('span.pl-name').textContent === 'Block ciphers', "the player's heading");
  const code = main.querySelector('span.pl-course').textContent;
  assert.ok(code, "the player's course code");
  const byCode = title(t('nav.doc_title_in', { page: 'Block ciphers', course: code }));
  const other = title(t('nav.doc_title_in', { page: 'Block ciphers', course: 'Course c2' }));
  await until(() => doc.title === byCode || doc.title === lecturePage, `opened from the workbench: this lecture's course, its code or its name (${doc.title})`);
  await settle(60);
  const fromWorkbench = doc.title;
  // R88 L1: from the player to another course's page, then back: the other
  // course kept is not this lecture's, its name is never shown: the
  // player's code, as opened from the workbench (or, once the player keeps
  // its lecture's course again, the name, as opened from the workbench)
  await go('#/lecture/L9');
  await until(() => doc.title === lecturePage, 'the lecture page again');
  await go('#/play/L9');
  await until(() => doc.title === lecturePage, 'the player with the course kept');
  await go('#/course/c2');
  await until(() => doc.title === title('Course c2'), `the other course's page (${doc.title})`);
  await go('#/play/L9');
  await until(() => main.querySelector('span.pl-name') && main.querySelector('span.pl-name').textContent === 'Block ciphers', "the player's heading");
  await settle(60);
  assert.ok(doc.title !== other, `back from another course's page: never that course's name (${doc.title})`);
  assert.equal(doc.title, fromWorkbench, "back from another course's page: this lecture's course, as opened from the workbench");
  await go('#/');
  told();
}

// ---- WD-09 (R86 L3, convention C3): too little disk space, in decimal units ----

{
  const U = await import(new URL('util.js', base).href);
  const Up = await import(new URL('uploads.js', base).href);
  // 2500 MB free (the page reads it with the system's checks, again on the
  // event that they changed), 1 GB to upload: under the 2 GB kept free,
  // refused before it is sent
  system.disk.free_mb = 2500;
  const n = sent.length;
  sources[0].emit('changed', { kind: 'system' });
  await until(() => sent.slice(n).some((s) => s.startsWith('GET /api/system')), 'the system read again');
  await settle(60);
  const free = U.fmtBytes(2500 * 1048576);
  assert.ok(free !== U.fmtMB(2500), `decimal (${free}), not binary (${U.fmtMB(2500)})`);
  const item = Up.enqueueUpload('c1', 'lecture', { name: 'week9.mp4', size: 1024 * 1024 * 1024 });
  await until(() => item.status === 'failed', 'refused up front');
  assert.equal(item.error, t('common.upload.no_space', { free }), "the space left as the upload list's sizes say it");
  told();
  system.disk.free_mb = 100000;
}

console.log('r9 dash app checks: ok');
process.exit(0);
