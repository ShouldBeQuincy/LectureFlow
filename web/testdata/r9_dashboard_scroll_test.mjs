// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the workbench line (r9-web-dash), from the UI recheck of round 8
// (.agent-log/reports/T28-ui-recheck-r8.md):
//   WD9-01 (T28 S1)  a redraw of the workbench keeps the page where it was;
//   WD9-02 (T28 G1)  the recently finished rows' titles get the focus back;
//   WD9-03 (T28 G4)  a control the focus is handed to is brought into view.
// The modules run on the DOM stand-in of r7_dom.mjs; web/r9_dashboard_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared with assert.ok(a === b), never with equal / deepEqual.
//
// The stand-in has no layout: this file adds a simple one. The page is one
// column of boxes (the classes of BOX, each its height; what is inside a box
// shares its box), the window has a height and a scroll position, and
// scrollTo / scrollIntoView move it. Chromium lays the page out when the
// control with the focus is taken out of it, and its scroll anchoring keeps
// what it anchored to (here: the focused control, as laid out on the frame
// before, flush()) in place on the screen: that is what made the page jump
// (T28 S1: 174 px, the header and notices above the cards, removed first).

import { assert, El, Node, body, doc, base, net, settle, until, fk, event, dialogs, toastsEl, told, cancelDialogs, confirmDialogs } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
const M = await import(new URL('menu.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });
const Dash = await import(new URL('view_dashboard.js', base).href);

// ---- a laid-out page ----

const BOX = { 'page-head': 54, 'dash-prompt': 102, 'card-hd': 40, 'card-empty': 40, 'ip-row': 50, 'rd-row': 44, 'todo-head': 36, 'todo-sub': 28, 'todo-note': 20, 'fail-row': 60, 'ccard-wrap': 100, ccard: 100 };
const MAX_BOXES = 5000;
function boxHeight(n) {
  if (!(n instanceof El)) return 0;
  for (const c of n.className.split(/\s+/)) if (BOX[c]) return BOX[c];
  // a review row of the to-do card
  if (n.tagName === 'LI' && n.className.split(/\s+/).includes('lgrid-row')) return 44;
  return 0;
}
// box → [top, height] in the document, and the document's height
function layout() {
  const tops = new Map();
  let y = 24;
  const walk = (n) => {
    for (const c of n.children) {
      if (!(c instanceof El)) continue;
      const hgt = boxHeight(c);
      if (hgt) {
        if (tops.size > MAX_BOXES) throw new Error('layout stand-in: too many boxes');
        tops.set(c, [y, hgt]);
        y += hgt;
        continue;
      }
      walk(c);
    }
  };
  walk(doc.documentElement);
  return { tops, height: y + 24 };
}
// [top, bottom] of n in the document, or null (not laid out)
function docBox(n, L = layout()) {
  if (!n || !n.isConnected) return null;
  for (let x = n, i = 0; x && i < 10000; x = x.parentNode, i++) {
    const b = L.tops.get(x);
    if (b) return [b[0], b[0] + b[1]];
  }
  let top = Infinity;
  let bottom = -Infinity;
  for (const [b, [bt, bh]] of L.tops) {
    if (!n.contains(b)) continue;
    top = Math.min(top, bt);
    bottom = Math.max(bottom, bt + bh);
  }
  return top === Infinity ? null : [top, bottom];
}

const win = { y: 0 };
const scrolls = []; // the scrolling the page asked for: "to 250", "into dash:add nearest"
// win.cut: as a browser, a page drawn shorter cuts the position back to its
// new end once it is laid out (read); off unless a check turns it on
Object.defineProperty(globalThis, 'scrollY', {
  get: () => {
    if (win.cut) win.y = Math.min(win.y, Math.max(0, layout().height - globalThis.innerHeight));
    return win.y;
  },
  configurable: true,
});
globalThis.scrollX = 0;
globalThis.innerHeight = 300;
const setY = (y) => {
  win.y = Math.min(Math.max(0, layout().height - globalThis.innerHeight), Math.max(0, Math.round(y)));
};
globalThis.scrollTo = (a, b) => {
  const y = a && typeof a === 'object' ? a.top : b;
  scrolls.push(`to ${Math.round(y)}`);
  setY(y);
};
globalThis.scrollBy = () => scrolls.push('by');
El.prototype.getBoundingClientRect = function () {
  const b = docBox(this);
  if (!b) return { top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0 };
  return { top: b[0] - win.y, bottom: b[1] - win.y, left: 0, right: 100, width: 100, height: b[1] - b[0] };
};
El.prototype.scrollIntoView = function (o = {}) {
  scrolls.push(`into ${this.dataset.fkey} ${o.block}`);
  const r = this.getBoundingClientRect();
  if (r.top < 0) setY(win.y + r.top);
  else if (r.bottom > globalThis.innerHeight) setY(win.y + r.bottom - globalThis.innerHeight);
};

// Chromium's anchoring (see above): flush() is the frame drawn before; the
// control with the focus taken out lays the page out then and there.
let anchored = null; // {node, top}
function flush() {
  const a = doc.activeElement;
  const b = a && a !== body ? docBox(a) : null;
  anchored = b ? { node: a, top: b[0] } : null;
}
const plainRemove = Node.prototype.remove;
Node.prototype.remove = function () {
  const a = doc.activeElement;
  if (this.parentNode && a && a !== body && (this === a || (this instanceof El && this.contains(a))) && this.isConnected) {
    const b = anchored && docBox(anchored.node);
    if (b) setY(win.y + b[0] - anchored.top);
    anchored = null;
  }
  return plainRemove.call(this);
};

// where the control with focus key k is on the screen (its top), or null
const topOf = (k) => {
  const n = fk(k);
  return n ? Math.round(n.getBoundingClientRect().top) : null;
};

// ---- the workbench ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'c1', course_code: 'C1', course_name: 'Course c1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [], usage: null,
    stages: names.map((n) => stage(n, 'done')), ...o,
  };
}
const running = (id) => lecture(id, { stages: names.map((n) => stage(n, n === 'prep' ? 'done' : n === 'asr' ? 'running' : 'pending')) });
const finished = (id) => lecture(id, { lecture_actions: ['update'] });
const course = (id, o = {}) => ({ id, code: id.toUpperCase(), name: `Course ${id}`, lecture_count: 4, term_count: 0, draft_count: 0, stale_count: 0, running_count: 0, patches_pending: 0, last_activity_at: null, course_actions: [], ...o });
const counted = (id, n) => ({ course_id: id, course_code: id.toUpperCase(), course_name: `Course ${id}`, count: n });
const review = (id) => ({ lecture_id: id, course_id: 'c1', course_code: 'C1', course_name: 'Course c1', lecture_title: `Lecture ${id}`, kind: 'lecture', week: 0, count: 2 });

let dash = {};
let courses = [];
const dashboard = () => ({ courses, in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [], ...dash });
const settingsBody = { general: {}, providers: [{ id: 'p1', name: 'Anthropic', has_key: true }], roles: [{ role: 'summary', configured: true }, { role: 'repair', configured: true }, { role: 'translate', configured: true, provider_id: 'p1', model: 'claude-opus-5-5' }] };
const defaultAnswer = (m, u) => {
  if (u.startsWith('/api/dashboard')) return { status: 200, body: dashboard() };
  if (u.startsWith('/api/settings')) return { status: 200, body: settingsBody };
  const lec = /^\/api\/lectures\/([^/?]+)$/.exec(u);
  if (lec) return { status: 200, body: lecture(decodeURIComponent(lec[1])) };
  return { status: 200, body: {} };
};
net.answer = defaultAnswer;
Act.setCourseLookup((id) => courses.find((c) => c.id === id) || null);
const page = new El('main');
body.append(page);
const dctx = { system: () => null, resources: () => null, courses: () => courses, newCourse() {}, subscribe: () => () => {} };
const view = Dash.mountDashboard(page, dctx);
await settle();

// The workbench of the checks: the header and a device notice (156 px)
// above the cards; four lectures in progress, six recently finished, two
// stale, a dozen courses.
//   page-head 24–78, dash-prompt 78–180, in progress: head 180–220, rows
//   220–420 (dash:open:P1 at 220); recent: head 420–460, rows 460–724; to
//   do: head 724–764, the stale group 764–848 (dash:stale at 764); …
const base0 = () => ({
  in_progress: ['P1', 'P2', 'P3', 'P4'].map(running),
  recent_done: ['R1', 'R2', 'R3', 'R4', 'R5', 'R6'].map(finished),
  stale_total: 2,
  stale: [counted('c1', 2)],
  device_prompts: [{ device_id: 'gpu0', label: 'GPU 0' }],
});
courses = Array.from({ length: 12 }, (_, i) => course(i ? `c${i + 1}` : 'c1', i ? {} : { stale_count: 2, course_actions: ['update_stale'] }));
// draws the workbench with d, the page at y (laid out: the frame before)
async function show(d, y) {
  dash = d;
  view.refresh();
  await settle();
  setY(y);
  assert.equal(win.y, y, `the page can be scrolled to ${y}`);
}
// a refresh with d (an SSE event's load): the frame before it is laid out
// with the focus where it is now
async function redraw(d) {
  flush();
  scrolls.length = 0;
  dash = d;
  view.refresh();
  await settle();
}

// ---- WD9-01 (T28 S1): a redraw keeps the page where it was ----

{
  // the focus on 全部更新, its page unchanged: no jump, no scrolling at all
  await show(base0(), 600);
  fk('dash:stale').focus();
  assert.equal(topOf('dash:stale'), 164);
  await redraw(base0());
  assert.ok(doc.activeElement === fk('dash:stale'), 'the focus back on 全部更新');
  assert.equal(win.y, 600, 'the page stays where it was (it jumped by the height above the cards)');
  assert.equal(topOf('dash:stale'), 164, 'the button stays where it was on the screen');
  assert.deepEqual(scrolls, [], 'nothing scrolls: a page that did not change does not move');

  // a course card's ⋯ and an in-progress title, each further down / up
  for (const [key, y] of [
    ['ccard:c3:menu', 1000],
    ['dash:open:P2', 200],
  ]) {
    await show(base0(), y);
    fk(key).focus();
    const top = topOf(key);
    await redraw(base0());
    assert.ok(doc.activeElement === fk(key), `${key}: the focus back`);
    assert.equal(win.y, y, `${key}: the page stays at ${y}`);
    assert.equal(topOf(key), top, `${key}: where it was on the screen`);
    assert.deepEqual(scrolls, [], `${key}: nothing scrolls`);
  }

  // a lecture added above the focused control: the control stays where it
  // was on the screen (the page moves with the rows, as anchoring would)
  await show(base0(), 200);
  fk('dash:open:P3').focus();
  assert.equal(topOf('dash:open:P3'), 120);
  const more = base0();
  more.in_progress = [running('P0'), ...more.in_progress];
  await redraw(more);
  assert.ok(doc.activeElement === fk('dash:open:P3'));
  assert.equal(topOf('dash:open:P3'), 120, 'a row added above: the focused title stays in place');
  assert.equal(win.y, 250);
  assert.deepEqual(scrolls, ['to 250']);
  // and one gone above it
  await redraw(base0());
  assert.equal(topOf('dash:open:P3'), 120, 'a row gone above: the focused title stays in place');
  assert.equal(win.y, 200);

  // at the top of the page: it stays at the top (the focused title moves
  // down with the row added above it)
  await show(base0(), 0);
  fk('dash:open:P1').focus();
  assert.equal(topOf('dash:open:P1'), 220);
  await redraw(more);
  assert.equal(win.y, 0, 'at the top: no scrolling');
  assert.equal(topOf('dash:open:P1'), 270);
  assert.deepEqual(scrolls, []);

  // the focus on none (the page's body): the first control on the screen
  // with a focus key stays in place
  await show(base0(), 200);
  body.focus();
  assert.equal(topOf('dash:open:P1'), 20);
  await redraw(more);
  assert.equal(topOf('dash:open:P1'), 20, 'no focus: what is on the screen stays in place');
  assert.equal(win.y, 250);
  // the page itself focused (app.js focuses main once a view is mounted): the same
  await show(base0(), 200);
  page.focus();
  await redraw(more);
  assert.equal(topOf('dash:open:P1'), 20, 'the page focused: what is on the screen stays in place');
  assert.equal(win.y, 250);
  body.focus();

  // the focused control off the screen: the page keeps its position
  await show(base0(), 1100);
  fk('dash:stale').focus();
  assert.ok(topOf('dash:stale') < 0);
  await redraw(more);
  assert.equal(win.y, 1100, 'the focus off the screen: the page keeps its position');
  assert.deepEqual(scrolls, []);

  // the focus outside the workbench (an open menu, which a scroll would
  // close; a dialog; the navigation): the page is not scrolled
  await show(base0(), 200);
  const outside = new El('button');
  body.append(outside);
  outside.focus();
  await redraw(more);
  assert.equal(win.y, 200, 'the focus outside: the page keeps its position');
  assert.deepEqual(scrolls, [], 'and is not scrolled');
  outside.remove();
  body.focus();

  // a redraw with the same data, the focus on 全部更新, three times running
  await show(base0(), 600);
  fk('dash:stale').focus();
  for (let i = 0; i < 3; i++) {
    await redraw(base0());
    assert.equal(win.y, 600, `redraw ${i + 1}: in place`);
    assert.equal(topOf('dash:stale'), 164);
  }
  // the user scrolls between two redraws: kept only within one redraw,
  // where the user scrolled to is not pulled back
  setY(700);
  await redraw(base0());
  assert.equal(win.y, 700, 'scrolled by the user meanwhile: the page stays where the user put it');
  assert.equal(topOf('dash:stale'), 64);
  assert.deepEqual(scrolls, []);

  // 全部更新 cancelled: drawn busy, the dialog, cancelled, drawn again; the
  // button is where it was, with the focus
  net.answer = defaultAnswer;
  await show(base0(), 600);
  fk('dash:stale').focus();
  flush();
  scrolls.length = 0;
  fk('dash:stale').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation of 全部更新');
  assert.equal(win.y, 600, 'drawn busy: in place');
  await cancelDialogs();
  await until(() => doc.activeElement === fk('dash:stale'), 'the focus back on 全部更新');
  assert.equal(win.y, 600, 'cancelled: in place');
  assert.equal(topOf('dash:stale'), 164);
  assert.deepEqual(scrolls, []);
}

// ---- WD9-02 (T28 G1): a recently finished row's title gets the focus back ----

{
  await show(base0(), 200);
  const title = fk('dash:recent:R2');
  assert.ok(title && title.className === 'rd-title', 'the title has a focus key');
  title.focus();
  await redraw(base0());
  assert.ok(doc.activeElement === fk('dash:recent:R2') && doc.activeElement !== title, 'a refresh: the focus on the title drawn anew');

  // Shift+F10 → 更新过期阶段 → the confirmation, a refresh while it is
  // open, cancelled: the focus back on the title (it was on the page's body)
  const realMatches = El.prototype.matches;
  El.prototype.matches = function (sel) {
    return sel.includes(',') ? ['INPUT', 'TEXTAREA', 'SELECT'].includes(this.tagName) : realMatches.call(this, sel);
  };
  try {
    for (const how of ['keyboard', 'mouse']) {
      const opener = fk('dash:recent:R2');
      opener.focus();
      const ev = how === 'keyboard' ? event('keydown', { key: 'F10', shiftKey: true }) : event('contextmenu', { clientX: 20, clientY: 20, button: 2 });
      fk('dash:recent:R2').dispatchEvent(ev);
      const item = doc.querySelectorAll('div.ctx-menu').flatMap((m) => m.querySelectorAll('button.ctx-item')).find((b) => b.textContent.includes(t('menu.lecture.update')));
      assert.ok(item, `${how}: the row's menu offers 更新过期阶段`);
      item.click();
      await until(() => dialogs.some((d) => d.open), `${how}: the confirmation`);
      // the dialog has the focus (the stand-in's showModal does not move it)
      dialogs.find((d) => d.open).querySelector('button').focus();
      await redraw(base0());
      assert.ok(!opener.isConnected, `${how}: the row was drawn anew while the dialog was open`);
      await cancelDialogs();
      await settle();
      assert.ok(doc.activeElement === fk('dash:recent:R2'), `${how}: cancelled, the focus back on the title`);
      M.closeMenu({ focus: false });
    }
  } finally {
    El.prototype.matches = realMatches;
  }

  // the row gone (updated: the lecture runs again): the focus to the
  // lecture in progress
  fk('dash:recent:R2').focus();
  const moved = base0();
  moved.recent_done = moved.recent_done.filter((l) => l.id !== 'R2');
  moved.in_progress = [...moved.in_progress, running('R2')];
  await redraw(moved);
  assert.ok(doc.activeElement === fk('dash:open:R2'), 'updated: the focus on the lecture in progress');
  // deleted: the next row
  fk('dash:recent:R3').focus();
  const gone = base0();
  gone.recent_done = gone.recent_done.filter((l) => l.id !== 'R3');
  await redraw(gone);
  assert.ok(doc.activeElement === fk('dash:recent:R4'), 'deleted: the focus on the next row');
  body.focus();
}

// ---- WD9-03 (T28 G4): the focus handed over is brought into view ----

{
  // 启动全部草稿 at the bottom of the screen; once launched its group is
  // gone and the focus goes to the first review, below the window: it is
  // scrolled into view (nearest: as little as needed)
  const drafts = () => ({ ...base0(), stale_total: 0, stale: [], draft_total: 3, drafts: [counted('c1', 3)], review_total: 2, reviews: [review('V1'), review('V2')] });
  courses[0] = course('c1', { draft_count: 3, course_actions: ['launch_drafts'] });
  await show(drafts(), 500);
  assert.equal(topOf('dash:drafts'), 264, 'the button at the bottom of the screen');
  net.answer = (m, u) => {
    if (m === 'POST' && u.startsWith('/api/courses/')) {
      dash = { ...drafts(), draft_total: 0, drafts: [] };
      return { status: 200, body: { results: [0, 1, 2].map((i) => ({ lecture_id: `L${i}`, ok: true, lecture: lecture(`L${i}`) })) } };
    }
    return defaultAnswer(m, u);
  };
  fk('dash:drafts').focus();
  flush();
  scrolls.length = 0;
  fk('dash:drafts').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation of 启动全部草稿');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result of the launch');
  await until(() => doc.activeElement === fk('dash:review:V1'), 'the focus on the first review');
  const r = fk('dash:review:V1').getBoundingClientRect();
  assert.ok(r.top >= 0 && r.bottom <= globalThis.innerHeight, `the review it went to is in view (${r.top}–${r.bottom}, window ${globalThis.innerHeight})`);
  assert.ok(scrolls.includes('into dash:review:V1 nearest'), `scrolled into view, nearest (${scrolls.join(', ')})`);
  told();
  net.answer = defaultAnswer;

  // R88 L3: the user scrolls elsewhere while the request runs: the focus
  // goes to the review all the same, but the page is not pulled back to it
  courses[0] = course('c1', { draft_count: 3, course_actions: ['launch_drafts'] });
  await show(drafts(), 500);
  net.answer = (m, u) => {
    if (m === 'POST' && u.startsWith('/api/courses/')) {
      dash = { ...drafts(), draft_total: 0, drafts: [] };
      return { status: 200, body: { results: [0, 1, 2].map((i) => ({ lecture_id: `L${i}`, ok: true, lecture: lecture(`L${i}`) })) } };
    }
    return defaultAnswer(m, u);
  };
  fk('dash:drafts').focus();
  flush();
  scrolls.length = 0;
  fk('dash:drafts').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation');
  setY(1200); // the user scrolls down meanwhile
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  await until(() => doc.activeElement === fk('dash:review:V1'), 'the focus on the first review');
  assert.ok(!scrolls.some((s) => s.startsWith('into')), `scrolled by the user while it ran: not pulled back to the review (${scrolls.join(', ')})`);
  // (the drafts group gone above, 64 px: what the user scrolled to stays in place)
  assert.equal(win.y, 1200 - 64, 'where the user scrolled to');
  told();
  net.answer = defaultAnswer;
  // a redraw while the focus waits moves the page itself (a row added
  // above): that is not the user's, the review is brought into view
  await show(drafts(), 500);
  net.answer = (m, u) => {
    if (m === 'POST' && u.startsWith('/api/courses/')) {
      dash = { ...drafts(), draft_total: 0, drafts: [] };
      return { status: 200, body: { results: [{ lecture_id: 'L0', ok: true, lecture: lecture('L0') }] } };
    }
    return defaultAnswer(m, u);
  };
  fk('dash:drafts').focus();
  flush();
  fk('dash:drafts').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation');
  body.focus(); // the stand-in's dialog takes no focus: the page's body, as it gives back
  scrolls.length = 0;
  dash = { ...drafts(), in_progress: [running('P0'), ...drafts().in_progress] };
  view.refresh();
  await settle();
  assert.deepEqual(scrolls, ['to 550'], 'the row above: the page moved with it');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  await until(() => doc.activeElement === fk('dash:review:V1'), 'the focus on the first review');
  assert.ok(scrolls.includes('into dash:review:V1 nearest'), `its own scrolling is not the user's: into view (${scrolls.join(', ')})`);
  told();
  net.answer = defaultAnswer;

  // R88b L4: while the focus waits the page is drawn much shorter, and the
  // browser cuts the position back to its new end: not the user's scrolling
  // either, the control the focus goes to (新建课程, at the top) is scrolled
  // to; the user scrolling after that: not pulled back
  const longCourses = courses;
  for (const userScrolls of [false, true]) {
    courses = longCourses;
    await show(drafts(), 600);
    net.answer = (m, u) => {
      if (m === 'POST' && u.startsWith('/api/courses/')) {
        dash = {};
        return { status: 200, body: { results: [{ lecture_id: 'L0', ok: true, lecture: lecture('L0') }] } };
      }
      return defaultAnswer(m, u);
    };
    fk('dash:drafts').focus();
    flush();
    fk('dash:drafts').click();
    await until(() => dialogs.some((d) => d.open), 'the confirmation');
    body.focus(); // the stand-in's dialog takes no focus: the page's body, as it gives back
    win.cut = true;
    try {
      // one course left, its drafts, nothing else: the page ends above 600
      courses = [course('c1', { draft_count: 3, course_actions: ['launch_drafts'] })];
      dash = { draft_total: 3, drafts: [counted('c1', 3)] };
      view.refresh();
      await settle();
      assert.ok(win.y < 600, `the page drawn shorter: cut back (${win.y})`);
      if (userScrolls) setY(40);
      scrolls.length = 0;
      await confirmDialogs();
      await until(() => toastsEl.children.length > 0, 'the result');
      await until(() => doc.activeElement === fk('dash:add'), 'the focus on 新建课程');
      if (userScrolls) assert.ok(!scrolls.some((s) => s.startsWith('into')), `the user scrolled: not pulled back (${scrolls.join(', ')})`);
      else {
        assert.ok(scrolls.includes('into dash:add nearest'), `cut back by the browser, not the user: into view (${scrolls.join(', ')})`);
        assert.ok(topOf('dash:add') >= 0, 'in view');
      }
    } finally {
      win.cut = false;
    }
    told();
    net.answer = defaultAnswer;
  }
  courses = longCourses;

  // the to-do card left empty: the focus to 新建课程 at the top, scrolled to
  await show({ ...base0(), stale_total: 0, stale: [], draft_total: 3, drafts: [counted('c1', 3)] }, 500);
  net.answer = (m, u) => {
    if (m === 'POST' && u.startsWith('/api/courses/')) {
      dash = { ...base0(), stale_total: 0, stale: [] };
      return { status: 200, body: { results: [{ lecture_id: 'L0', ok: true, lecture: lecture('L0') }] } };
    }
    return defaultAnswer(m, u);
  };
  fk('dash:drafts').focus();
  flush();
  scrolls.length = 0;
  fk('dash:drafts').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  await until(() => doc.activeElement === fk('dash:add'), 'the focus on 新建课程');
  assert.ok(topOf('dash:add') >= 0, 'in view');
  assert.ok(scrolls.includes('into dash:add nearest'), `scrolled into view (${scrolls.join(', ')})`);
  told();
  net.answer = defaultAnswer;

  // the control drawn again in place (a dialog cancelled): nothing scrolls
  await show(drafts(), 500);
  fk('dash:drafts').focus();
  flush();
  scrolls.length = 0;
  fk('dash:drafts').click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation');
  await cancelDialogs();
  await until(() => doc.activeElement === fk('dash:drafts'), 'the focus back on 启动全部草稿');
  assert.equal(win.y, 500);
  assert.deepEqual(scrolls, [], 'in view already: no scrolling');
}

// ---- WD9-01: the first draw (the skeleton, then the data) forces no scrolling ----

{
  view.destroy();
  page.replaceChildren();
  dash = base0();
  // app.js scrolls a page it mounts to the top and focuses main
  setY(0);
  page.focus();
  flush();
  scrolls.length = 0;
  const again = Dash.mountDashboard(page, dctx);
  assert.ok(page.querySelector('section.is-loading'), 'the skeleton first');
  await until(() => fk('dash:stale'), 'then the data');
  await settle();
  assert.equal(win.y, 0, 'at the top');
  assert.deepEqual(scrolls, [], 'no scrolling');
  again.destroy();
}

console.log('r9 dashboard scroll checks: ok');
