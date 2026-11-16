// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 8, the workbench line (r8-web-dash) on the workbench and the course
// card; each section names its item (WD-nn) and the report it comes from.
// The modules run on the DOM stand-in of r7_dom.mjs; web/r8_dashboard_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared with assert.ok(a === b), never with equal / deepEqual.

import { assert, El, body, doc, dialogs, toastsEl, base, sent, net, settle, until, fk, told, cancelDialogs, confirmDialogs } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
// the page's handler of failed requests (app.js): a toast
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

const Dash = await import(new URL('view_dashboard.js', base).href);
const U2 = await import(new URL('util.js', base).href);

// ---- the workbench ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'c1', course_code: 'C1', course_name: 'Course c1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [], usage: null,
    stages: names.map((n) => stage(n, n === 'translate' ? 'failed' : n === 'export' ? 'pending' : 'done')), ...o,
  };
}
const course = (id, o = {}) => ({ id, code: id.toUpperCase(), name: `Course ${id}`, lecture_count: 4, term_count: 0, draft_count: 0, stale_count: 0, running_count: 0, patches_pending: 0, last_activity_at: null, course_actions: [], ...o });
const fail = (id, o = {}) => ({ lecture_id: id, stage: 'translate', course_id: 'c1', course_code: 'C1', course_name: 'Course c1', lecture_title: `Lecture ${id}`, kind: 'lecture', week: 0, allowed_actions: ['retry'], error: 'x', finished_at: null, ...o });
const balance = { code: 'llm.insufficient_balance', params: { role: 'translate', provider: 'DeepSeek' } };
const counted = (id, n) => ({ course_id: id, course_code: id.toUpperCase(), course_name: `Course ${id}`, count: n });

let dash = {};
let courses = [course('c1')];
const dashboard = () => ({ courses, in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [], ...dash });
const settingsBody = { general: {}, providers: [{ id: 'p1', name: 'Anthropic', has_key: true }], roles: [{ role: 'summary', configured: true }, { role: 'repair', configured: true }, { role: 'translate', configured: true, provider_id: 'p1', model: 'claude-opus-5-5' }] };
const okResults = (n) => ({ results: Array.from({ length: n }, (_, i) => ({ lecture_id: `L${i}`, ok: true, lecture: lecture(`L${i}`) })) });
const defaultAnswer = (m, u) => {
  if (u.startsWith('/api/dashboard')) return { status: 200, body: dashboard() };
  if (u.startsWith('/api/settings')) return { status: 200, body: settingsBody };
  const lec = /^\/api\/lectures\/([^/?]+)$/.exec(u);
  if (lec) return { status: 200, body: lecture(decodeURIComponent(lec[1])) };
  return { status: 200, body: {} };
};
net.answer = defaultAnswer;
// the glossary question of a launch (actions.js) looks the courses up as app.js has them
Act.setCourseLookup((id) => courses.find((c) => c.id === id) || null);
const page = new El('main');
body.append(page);
const dctx = { system: () => null, resources: () => null, courses: () => courses, newCourse() {}, subscribe: () => () => {} };
let view = Dash.mountDashboard(page, dctx);
await settle();
const show = async (d) => {
  dash = d;
  view.refresh();
  await settle();
};
const writes = () => sent.filter((s) => !s.startsWith('GET ')).map((s) => s.replace(/\?.*$/, ''));
const opened = () => dialogs.filter((d) => d.open);
const OPEN_MAX = 5; // a check opens at most this many dialogs in a row

// ---- WD-01 (T25 X1): "启动全部草稿" and "全部更新" ask first ----

{
  // "启动全部草稿" of one course: the course page's confirmation, through
  // the shared launch (the glossary first, then the confirmation)
  courses = [course('c1', { draft_count: 3, course_actions: ['launch_drafts'] })];
  await show({ draft_total: 3, drafts: [counted('c1', 3)] });
  net.answer = (m, u) => (m === 'POST' && u.startsWith('/api/courses/') ? { status: 200, body: okResults(3) } : defaultAnswer(m, u));
  fk('dash:drafts').focus();
  sent.length = 0;
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation of 启动全部草稿');
  const ask = opened()[0];
  assert.ok(ask.textContent.includes(t('course.confirm.launch_title', { n: 3 })), 'it says how many lectures start');
  assert.ok(ask.textContent.includes(t('course.confirm.launch_cost')), 'and that the model requests are billed');
  await cancelDialogs();
  await settle(60);
  assert.deepEqual(writes(), [], 'cancelled: nothing is sent');
  await until(() => doc.activeElement === fk('dash:drafts'), 'cancelled: the focus back on the button');
  told();
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation again');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result of the launch');
  assert.deepEqual(writes(), ['POST /api/courses/c1/launch-drafts'], 'confirmed: the request of before');
  assert.deepEqual(told(), [`success: ${t('dashboard.launched', { n: 3 })}`], 'one result, counted as before');
  assert.equal(opened().length, 0);

  // its glossary still being extracted: that question first, then the confirmation, one each
  courses = [course('c1', { draft_count: 3, course_actions: ['launch_drafts'], glossary_pending: true })];
  await show({ draft_total: 3, drafts: [counted('c1', 3)] });
  sent.length = 0;
  const seen = [];
  fk('dash:drafts').click();
  for (let i = 0; i < OPEN_MAX; i++) {
    try {
      await until(() => opened().length === 1, 'a question', 50);
    } catch {
      break;
    }
    seen.push(opened()[0].textContent.includes(t('course.confirm.launch_title', { n: 3 })) ? 'launch' : opened()[0].textContent.includes(t('common.glossary_check.message_course', { course: 'C1' })) ? 'glossary' : 'other');
    await confirmDialogs();
  }
  assert.deepEqual(seen, ['glossary', 'launch'], 'the glossary question, then the confirmation');
  await until(() => toastsEl.children.length > 0, 'the result');
  assert.deepEqual(writes(), ['POST /api/courses/c1/launch-drafts']);
  told();

  // drafts in two courses: one confirmation for all of them (how many
  // lectures), then each course's request as before, one result
  courses = [course('c1', { draft_count: 2, course_actions: ['launch_drafts'] }), course('c2', { draft_count: 3, course_actions: ['launch_drafts'] })];
  await show({ draft_total: 5, drafts: [counted('c1', 2), counted('c2', 3)] });
  net.answer = (m, u) => (m === 'POST' && u.startsWith('/api/courses/') ? { status: 200, body: okResults(u.includes('/c1/') ? 2 : 3) } : defaultAnswer(m, u));
  fk('dash:drafts').focus();
  sent.length = 0;
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'one confirmation for both courses');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.launch_title', { n: 5 })), 'all the lectures counted');
  await cancelDialogs();
  await settle(60);
  assert.deepEqual(writes(), [], 'cancelled: nothing is sent');
  await until(() => doc.activeElement === fk('dash:drafts'), 'the focus back on the button');
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation again');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  assert.equal(opened().length, 0, 'asked once, not once per course');
  assert.deepEqual(writes(), ['POST /api/courses/c1/launch-drafts', 'POST /api/courses/c2/launch-drafts'], 'each course as before');
  assert.deepEqual(told(), [`success: ${t('dashboard.launched', { n: 5 })}`], 'one result for both');

  // one of them with its glossary pending: the workbench's one glossary question, then the confirmation
  courses = [course('c1', { draft_count: 2, course_actions: ['launch_drafts'] }), course('c2', { draft_count: 3, course_actions: ['launch_drafts'], glossary_pending: true })];
  await show({ draft_total: 5, drafts: [counted('c1', 2), counted('c2', 3)] });
  sent.length = 0;
  seen.length = 0;
  fk('dash:drafts').click();
  for (let i = 0; i < OPEN_MAX; i++) {
    try {
      await until(() => opened().length === 1, 'a question', 50);
    } catch {
      break;
    }
    seen.push(opened()[0].textContent.includes(t('course.confirm.launch_title', { n: 5 })) ? 'launch' : opened()[0].textContent.includes(t('dashboard.glossary_pending.message', { courses: 'C2' })) ? 'glossary' : 'other');
    await confirmDialogs();
  }
  assert.deepEqual(seen, ['glossary', 'launch'], 'one glossary question for all, then the confirmation');
  await until(() => toastsEl.children.length > 0, 'the result');
  assert.equal(writes().length, 2);
  told();

  // "全部更新": one confirmation for every course (the lectures are not on
  // the workbench: how many), then each course's request as before
  courses = [course('c1', { stale_count: 2, course_actions: ['update_stale'] }), course('c2', { stale_count: 1, course_actions: ['update_stale'] })];
  await show({ stale_total: 3, stale: [counted('c1', 2), counted('c2', 1)] });
  net.answer = (m, u) => (m === 'POST' && u.includes('/update-stale') ? { status: 200, body: okResults(u.includes('/c1/') ? 2 : 1) } : defaultAnswer(m, u));
  fk('dash:stale').focus();
  sent.length = 0;
  fk('dash:stale').click();
  await until(() => opened().length === 1, 'the confirmation of 全部更新');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.update_title_many', { n: 3 })), 'it says how many lectures');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.update_unknown')), 'and that the results are replaced and billed again');
  await cancelDialogs();
  await settle(60);
  assert.deepEqual(writes(), [], 'cancelled: nothing is sent');
  await until(() => doc.activeElement === fk('dash:stale'), 'cancelled: the focus back on the button');
  fk('dash:stale').click();
  await until(() => opened().length === 1, 'the confirmation again');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result of the update');
  assert.equal(opened().length, 0);
  assert.deepEqual(writes(), ['POST /api/courses/c1/update-stale', 'POST /api/courses/c2/update-stale'], 'confirmed: the requests of before');
  assert.deepEqual(told(), [`success: ${t('dashboard.updated', { n: 3 })}`], 'one result, counted as before');
  // a course whose models differ from the settings: the model question comes after the confirmation, once
  const diff = [{ stage: 'translate', snapshot: { provider_id: 'p0', provider_name: 'Old', model: 'old' }, current: { provider_id: 'p1', provider_name: 'Anthropic', model: 'claude-opus-5-5' }, snapshot_usable: true }];
  net.answer = (m, u, b) => {
    if (m === 'POST' && u.includes('/c2/update-stale') && !(b && b.model_choice)) return { status: 409, body: { error: { code: 'needs_model_choice' }, diff } };
    if (m === 'POST' && u.includes('/update-stale')) return { status: 200, body: okResults(u.includes('/c1/') ? 2 : 1) };
    return defaultAnswer(m, u);
  };
  await show({ stale_total: 3, stale: [counted('c1', 2), counted('c2', 1)] });
  sent.length = 0;
  fk('dash:stale').click();
  await until(() => opened().length === 1, 'the confirmation');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.update_title_many', { n: 3 })));
  await confirmDialogs();
  await until(() => opened().length === 1, 'then the model question');
  assert.ok(opened()[0].textContent.includes(t('common.model_choice.title')));
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  assert.deepEqual(writes(), ['POST /api/courses/c1/update-stale', 'POST /api/courses/c2/update-stale', 'POST /api/courses/c2/update-stale'], 'as before: the course asked about sent again with the choice');
  told();
  net.answer = defaultAnswer;
  courses = [course('c1')];
  await show({});
}

// ---- WD-02 (P2 hotspot 3): the in-progress bars build a card when it opens ----

{
  const { event } = await import('./r7_dom.mjs');
  const P = await import(new URL('pipeline.js', base).href);
  const count = (n) => (n instanceof El ? 1 + n.children.reduce((s, c) => s + count(c), 0) : 0);
  const started = new Date(Date.now() - 60000).toISOString();
  const runningStages = () => names.map((n) => (n === 'asr' ? stage(n, 'running', { progress: 0.3, allowed_actions: ['cancel'], started_at: started }) : stage(n, n === 'prep' ? 'done' : 'queued', n === 'prep' ? { duration_ms: 42000, allowed_actions: ['rerun'] } : {})));
  const waitingStages = () => names.map((n) => (n === 'asr' ? stage(n, 'queued', { waiting_for: 'gpu_slot' }) : stage(n, n === 'prep' ? 'done' : 'pending')));
  const failedStages = () => names.map((n) => (n === 'summary' ? stage(n, 'failed', { error: 'deepseek: 402 insufficient balance', allowed_actions: ['retry'] }) : stage(n, n === 'prep' || n === 'asr' ? 'done' : 'pending')));
  await show({ in_progress: [lecture('P', { stages: runningStages() }), lecture('Q', { stages: waitingStages() }), lecture('F', { stages: failedStages() })] });
  const card = page.querySelector('section.card-ip');
  console.log(`the workbench's in-progress card, three rows: ${count(card)} elements`);
  const row = (id) => fk(`dash:open:${id}`).closest('div.ip-row');
  const wrap = (id, s) => row(id).querySelector(`div.seg-wrap[data-stage="${s}"]`);
  const cardOf = (id, s) => wrap(id, s).querySelector('div.seg-card');
  const segOf = (id, s) => wrap(id, s).querySelector('button.seg');
  // not built before it opens; the card itself is there for aria-controls
  for (const s of ['prep', 'asr', 'summary']) {
    assert.equal(cardOf('P', s).children.length, 0, `P ${s}: its card is not built before it opens`);
    assert.ok(segOf('P', s).getAttribute('aria-controls') === cardOf('P', s).getAttribute('id'), `P ${s}: aria-controls names its card`);
  }
  // a stage its card describes (a waiting reason, an error: R19) is built at once and describes its segment
  assert.ok(cardOf('Q', 'asr').children.length > 0, 'a waiting reason: built at once');
  const describedBy = (id, s) => (segOf(id, s).getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
  assert.equal(describedBy('Q', 'asr').length, 1, 'the waiting stage is described by its reason');
  assert.ok(cardOf('F', 'summary').children.length > 0, 'an error: built at once');
  assert.equal(describedBy('F', 'summary').length, 2, 'the failed stage is described by its error line');
  // the pointer builds it, with the content of a card built at once
  const eager = P.pipelineBar(runningStages(), { key: 'eager', sys: () => null, res: () => null, onAction: () => Promise.resolve(null) });
  const eagerCard = (s) => eager.querySelector(`div.seg-wrap[data-stage="${s}"]`).querySelector('div.seg-card');
  wrap('P', 'prep').dispatchEvent(event('pointerenter'));
  assert.ok(cardOf('P', 'prep').children.length > 0, 'the pointer builds it');
  assert.equal(segOf('P', 'prep').getAttribute('aria-expanded'), 'true');
  assert.equal(cardOf('P', 'prep').textContent, eagerCard('prep').textContent, 'the same content as built at once');
  wrap('P', 'prep').dispatchEvent(event('pointerleave'));
  // the focus builds another, with its actions (取消 of the running stage)
  segOf('P', 'asr').focus();
  segOf('P', 'asr').dispatchEvent(event('focusin'));
  assert.ok(cardOf('P', 'asr').children.length > 0, 'the focus builds it');
  assert.ok(fk('dash:P:act:asr:cancel'), "the running stage's card has its 取消");
  segOf('P', 'asr').dispatchEvent(event('focusout'));
  body.focus();
  await show({});
}

// ---- WD-03 (T22 G15): the course card has a visible ⋯ ----

{
  const { event } = await import('./r7_dom.mjs');
  const CC = await import(new URL('course_card.js', base).href);
  const M = await import(new URL('menu.js', base).href);
  const card = course('c7', { draft_count: 2, stale_count: 1, course_actions: ['launch_drafts', 'update_stale', 'export_cite'] });
  const holder = new El('div');
  holder.className = 'ccards';
  body.append(holder);
  holder.append(CC.courseCard(card, { onChanged() {} }));
  const link = fk('ccard:c7');
  const more = fk('ccard:c7:menu');
  assert.ok(link && link.tagName === 'A', 'the card is a link to its course');
  assert.ok(more && more.tagName === 'BUTTON', 'with a visible ⋯');
  // r9 WD-06 (R86 L6): named with its course (was the course page's ⋯ name)
  assert.equal(more.getAttribute('aria-label'), t('course.card.menu_named', { name: 'C7' }), 'named with its course');
  assert.equal(more.getAttribute('aria-haspopup'), 'menu');
  assert.ok(more.className.split(' ').includes('icon-btn'), 'the icon button look');
  assert.ok(!link.contains(more), 'not inside the link: no control in a link');
  // the Tab order: the card's link, then its ⋯
  const stops = holder.querySelectorAll('a').concat(holder.querySelectorAll('button')).filter((e) => !e.disabled && e.getAttribute('tabindex') !== '-1');
  const order = [];
  const walk = (n) => {
    for (const c of n.children) {
      if (!(c instanceof El)) continue;
      if (stops.includes(c)) order.push(c);
      walk(c);
    }
  };
  walk(holder);
  assert.ok(order.length === 2 && order[0] === link && order[1] === more, 'Tab reaches the link, then the ⋯');
  // the ⋯ opens the right-click menu's items; a click on it does not open the course
  const labels = () => doc.querySelectorAll('div.ctx-menu').flatMap((m) => m.querySelectorAll('button.ctx-item')).map((b) => b.querySelector('span.ctx-label').textContent);
  let opened = 0;
  link.addEventListener('click', () => opened++);
  const hash = location.hash;
  more.click();
  const fromMore = labels();
  assert.ok(fromMore.length > 3, `the ⋯ opens the course menu (${fromMore.join(' | ')})`);
  assert.equal(opened, 0, 'a click on the ⋯ does not reach the link');
  assert.equal(location.hash, hash, 'and does not open the course');
  M.closeMenu({ focus: false });
  // the right-click menu (menu.js asks whether the target is a text field,
  // a selector list the stand-in does not take: none of these is one)
  const realMatches = El.prototype.matches;
  El.prototype.matches = function (sel) {
    return sel.includes(',') ? ['INPUT', 'TEXTAREA', 'SELECT'].includes(this.tagName) : realMatches.call(this, sel);
  };
  try {
    link.dispatchEvent(event('contextmenu', { clientX: 20, clientY: 20, button: 2 }));
    assert.deepEqual(labels(), fromMore, 'the same items as the right-click menu');
    M.closeMenu({ focus: false });
    more.dispatchEvent(event('contextmenu', { clientX: 20, clientY: 20, button: 2 }));
    assert.deepEqual(labels(), fromMore, 'a right-click on the ⋯ too');
    M.closeMenu({ focus: false });
  } finally {
    El.prototype.matches = realMatches;
  }
  holder.remove();
}

// ---- WD-04 (T22 G10, G13): the workbench offers to continue the guide ----

{
  const U = await import(new URL('util.js', base).href);
  const notice = () => page.querySelectorAll('section.asr-notice').find((s) => s.querySelector('a[href="#/onboarding"]')) || null;
  const rolesShown = () => page.querySelectorAll('section.asr-notice').some((s) => s.querySelector('a[href="#/settings/roles"]'));
  const left = { step: 'providers', done: false, data: { mode: 'local' } };
  assert.ok(U.guideLeft(left), 'a guide left before its end');
  // left: the settings page's words and its button, to the guide
  dctx.guide = () => left;
  await show({});
  const n = notice();
  assert.ok(n, 'the guide left: the workbench says so');
  assert.ok(n.textContent.includes(t('settings.alert.guide_left')), "the settings page's words");
  const go = n.querySelector('a[href="#/onboarding"]');
  assert.equal(go.textContent.trim(), t('settings.alert.guide_continue'), 'and its button');
  assert.equal(go.dataset.fkey, 'dash:guide:go');
  // done, never started, or not known (no data): nothing
  dctx.guide = () => ({ step: 'done', done: true, data: { mode: 'local' } });
  await show({});
  assert.ok(notice() === null, 'the guide done: nothing');
  dctx.guide = () => ({ step: 'welcome', done: false, data: {} });
  await show({});
  assert.ok(notice() === null, 'never started: nothing');
  dctx.guide = () => null;
  await show({});
  assert.ok(notice() === null, 'not known: nothing');
  delete dctx.guide;
  await show({});
  assert.ok(notice() === null, 'a page without the guide state: nothing');
  // with a role not set up too: one notice, the guide's (it sets the roles up)
  settingsBody.roles = [{ role: 'summary', configured: true }, { role: 'repair', configured: true }, { role: 'translate', configured: false }];
  view.onChanged({ kind: 'settings' });
  await until(() => rolesShown(), 'the roles notice alone');
  dctx.guide = () => left;
  view.onGuideChanged();
  await until(() => notice(), 'the guide notice once the state comes');
  assert.ok(!rolesShown(), 'one notice at a time: the guide first');
  dctx.guide = () => null;
  view.onGuideChanged();
  await until(() => rolesShown() && !notice(), 'the roles notice again');
  settingsBody.roles = [{ role: 'summary', configured: true }, { role: 'repair', configured: true }, { role: 'translate', configured: true, provider_id: 'p1', model: 'claude-opus-5-5' }];
  view.onChanged({ kind: 'settings' });
  await until(() => !rolesShown(), 'roles set up');
  // no course yet (the guide left before its first course): there too
  dctx.guide = () => left;
  courses = [];
  await show({});
  assert.ok(notice(), 'the empty workbench says it too');
  courses = [course('c1')];
  delete dctx.guide;
  await show({});
}

// ---- WD-07 (R82 D-L1, R76 L-1): "全部重试" asks in the words of a batch ----

{
  await show({ failures: [fail('B1', { error_info: balance }), fail('B2', { error_info: balance })] });
  assert.equal(fk('dash:retryall').getAttribute('title'), null, 'no description on the button: the dialog says it');
  // a single retry's 409 names no lecture (its diff as the server sends it)
  const diff = [{ stage: 'translate', snapshot: { provider_id: 'p0', provider_name: 'Old', model: 'old' }, current: { provider_id: 'p1', provider_name: 'Anthropic', model: 'claude-opus-5-5' }, snapshot_usable: true }];
  net.answer = (m, u, b) => (m === 'POST' && !(b && b.model_choice) ? { status: 409, body: { error: { code: 'needs_model_choice' }, diff } } : m === 'POST' ? { status: 200, body: lecture('B1') } : defaultAnswer(m, u));
  sent.length = 0;
  fk('dash:retryall').click();
  await until(() => opened().length === 1, 'the model question');
  const dlg = opened()[0];
  assert.ok(dlg.textContent.includes(t('common.model_choice.message_many', { what: t('common.model_choice.what_retry'), n: 2 })), 'it says it asks once for two lectures');
  assert.deepEqual(dlg.querySelectorAll('span.diff-item-lec').map((s) => s.textContent), ['Lecture B1', 'Lecture B2'], 'each lecture named once');
  const warns = dlg.querySelectorAll('p.dlg-warn');
  assert.equal(warns.length, 1, 'one sentence about the translations');
  assert.equal(warns[0].textContent, t('common.model_choice.translate_clears_many'), "in a batch's words");
  assert.ok(!sent.includes('GET /api/settings'), "a batch: the current settings are not judged as one lecture's");
  await cancelDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  told();
  net.answer = defaultAnswer;
  await show({});
}

// ---- WD-08 (R76 L-2): the long result of "全部重试" stays until closed ----

{
  // the toasts' 4-second timers are held here and fired by fade(); the
  // other timers stay real
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
    told();
    // some not retried: "已重试 N 节；M 节没有重试：原因…" stays
    await show({ failures: [fail('B1', { error_info: balance }), fail('B2', { error_info: balance }), fail('B3', { error_info: balance })] });
    net.answer = (m, u) => (m === 'POST' ? (u.includes('/B3/') ? { status: 409, body: { error: { code: 'state.conflict' } } } : { status: 200, body: lecture('B1') }) : defaultAnswer(m, u));
    fk('dash:retryall').click();
    await until(() => toastsEl.children.length > 0, 'the result of 全部重试');
    fade();
    const el = toastsEl.children[0];
    assert.ok(el && el.dataset.kind === 'warn' && el.dataset.msg.startsWith(t('dashboard.retried_partial', { ok: 2, n: 1, reason: '' })), `the partial result stays after 4 seconds (${el ? el.dataset.msg : 'gone'})`);
    el.querySelector('.toast-close').click();
    assert.equal(toastsEl.children.length, 0, 'closed by its button');
    // all of them retried: the short result fades as before
    await show({ failures: [fail('B1', { error_info: balance }), fail('B2', { error_info: balance })] });
    net.answer = (m, u) => (m === 'POST' ? { status: 200, body: lecture('B1') } : defaultAnswer(m, u));
    fk('dash:retryall').click();
    await until(() => toastsEl.children.length > 0, 'the result');
    assert.equal(toastsEl.children[0].dataset.msg, t('dashboard.retried', { n: 2 }));
    fade();
    assert.equal(toastsEl.children.length, 0, 'the short result fades');
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
  }
  told();
  net.answer = defaultAnswer;
  await show({});
}

// ---- WD-10 (R76 L-6): the Colab warning is not redrawn for its numbers ----

{
  const C = await import(new URL('settings_colab.js', base).href);
  const remote = (o = {}) => ({ configured: true, state: 'running', accelerator: 'L4', tier: 'paid', window_used_sec: 3600, window_total_sec: 14400, lectures_left: 3, session_spent_cu: 1.31, cu_per_hour: 1.54, runtime: { held: true, reachable: 'unreachable', billing: true }, ...o });
  let res = { sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: remote() };
  dctx.resources = () => res;
  const slot = () => page.querySelector('div.dash-colab-slot');
  await show({});
  view.onResources(res);
  const warn = slot().querySelector('section.dash-colab-warn');
  const now = fk('dash:colab:now');
  assert.ok(warn && now, 'the billing warning with 立即关机');
  now.focus();
  // the next snapshots: only the compute units spent and the time used change
  for (const [cu, used] of [
    [1.42, 3700],
    [1.58, 3800],
  ]) {
    res = { ...res, sampled_at: new Date().toISOString(), remote: remote({ session_spent_cu: cu, window_used_sec: used }) };
    view.onResources(res);
    assert.ok(slot().querySelector('section.dash-colab-warn') === warn, `CU ${cu}: the same warning, not drawn anew`);
    assert.ok(fk('dash:colab:now') === now, `CU ${cu}: the same 立即关机`);
    assert.ok(doc.activeElement === now, `CU ${cu}: the focus stays on it`);
    assert.ok(slot().textContent.includes(C.runtimeWarning(res.remote, [])), `CU ${cu}: the new numbers are shown`);
  }
  // what it shows changes (being released: no 立即关机): drawn anew
  res = { ...res, remote: remote({ state: 'stopping' }) };
  view.onResources(res);
  assert.ok(fk('dash:colab:now') === null, 'released: no 立即关机');
  // the calm line follows its numbers in place too
  res = { ...res, remote: remote({ runtime: { held: true, reachable: 'ok', billing: true } }) };
  view.onResources(res);
  const line = slot().querySelector('p.dash-colab');
  assert.ok(line, 'the calm line');
  res = { ...res, remote: remote({ runtime: { held: true, reachable: 'ok', billing: true }, window_used_sec: 7200, lectures_left: 2 }) };
  view.onResources(res);
  assert.ok(slot().querySelector('p.dash-colab') === line, 'the same line');
  assert.ok(line.textContent.includes(t('dashboard.colab.lectures_left', { n: 2 })), 'with its new numbers');
  body.focus();
  dctx.resources = () => null;
  view.onResources(null);
  await show({});
}

// ---- WD-11 (R76 L-8, L-9): a "全部重试" round busy everywhere; the failures without retry counted ----

{
  // the round's requests are held until the check lets them go
  const held = [];
  net.answer = (m, u) => (m === 'POST' ? new Promise((resolve) => held.push(() => resolve({ status: 200, body: lecture('B1') }))) : defaultAnswer(m, u));
  const failures = [fail('B1', { error_info: balance }), fail('B2', { error_info: balance })];
  await show({ failures });
  told();
  sent.length = 0;
  fk('dash:retryall').click();
  await until(() => held.length === 1, 'the first request of the round');
  await settle();
  assert.ok(fk('dash:retryall').disabled, 'the round runs: 全部重试 busy');
  assert.ok(fk('dash:retry:B1:translate').disabled && fk('dash:retry:B2:translate').disabled, 'and the rows\' 重试 of its failures');
  // the workbench left and opened again while the round runs: still busy
  view.destroy();
  page.replaceChildren();
  view = Dash.mountDashboard(page, dctx);
  await settle();
  await until(() => fk('dash:retryall'), 'the new workbench');
  assert.ok(fk('dash:retryall').disabled, 'the new workbench knows the round: 全部重试 busy');
  assert.ok(fk('dash:retry:B1:translate').disabled, 'its rows\' 重试 too');
  fk('dash:retryall').click();
  await settle();
  // the round goes on (the other view's), one request at a time; nothing more
  for (let i = 0; i < 4 && held.length; i++) {
    held.shift()();
    await settle();
  }
  await until(() => toastsEl.children.length > 0, 'the result of the round');
  assert.deepEqual(told(), [`success: ${t('dashboard.retried', { n: 2 })}`], 'one round: two retried');
  assert.equal(writes().filter((s) => s.startsWith('POST')).length, 2, 'no second round');
  await until(() => fk('dash:retryall') && !fk('dash:retryall').disabled, 'the round over: 全部重试 again');
  assert.ok(!fk('dash:retry:B1:translate').disabled, 'and the rows\' 重试');
  // L-9: a failure with no retry offered is not retried, and said
  net.answer = (m, u) => (m === 'POST' ? { status: 200, body: lecture('B1') } : defaultAnswer(m, u));
  await show({ failures: [...failures, fail('N1', { allowed_actions: [] })] });
  sent.length = 0;
  fk('dash:retryall').click();
  await until(() => toastsEl.children.length > 0, 'the result');
  const why = U2.clauseJoin([t('dashboard.retry_skip.no_action', { n: 1 })]);
  assert.deepEqual(told(), [`warn: ${t('dashboard.retried_partial', { ok: 2, n: 1, reason: why })}`], 'counted among those not retried, with why');
  assert.ok(!writes().some((s) => s.includes('/N1/')), 'and not sent');
  net.answer = defaultAnswer;
  await show({});
}

// ---- C177b (R76 I-5): a leftover session's warning has its own title ----

{
  // dashboard.colab.leftover_session_title is the copy line's (task/r8-copy,
  // landed with this one): the title is the text of that key, whatever this
  // tree's catalog has for it (t() gives the key where it has none)
  let res = { sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: { configured: true, state: 'none', runtime: null, leftover: { code: 'colab.leftover_session', accelerator: 'T4' } } };
  dctx.resources = () => res;
  await show({});
  view.onResources(res);
  const title = () => page.querySelector('div.dash-colab-slot').querySelector('p.asr-notice-title').textContent;
  assert.equal(title(), t('dashboard.colab.leftover_session_title'), 'a session left without its sign-in: may still be running');
  assert.ok(title() !== t('dashboard.colab.leftover_title'), 'not "could not be released"');
  // a runtime the program failed to release: as before
  res = { ...res, remote: { ...res.remote, leftover: { code: 'colab.release_failed', accelerator: 'T4' } } };
  view.onResources(res);
  assert.equal(title(), t('dashboard.colab.leftover_title'), 'release failed: the title of before');
  dctx.resources = () => null;
  view.onResources(null);
  await show({});
}

// ---- WD-01 with convention F6: the launch's cost sentence by the shared rule ----

{
  // update_confirm.js launchCostMode is the shared modules' line's (round 8
  // F6): with it, the workbench's and the course card's confirmations say
  // what a launch costs by the settings the page read; before it lands, the
  // usual sentence
  const UC = await import(new URL('update_confirm.js', base).href);
  const shared = typeof UC.launchCostMode === 'function';
  const transcribeOnly = { general: {}, providers: [], roles: [{ role: 'summary', configured: false }, { role: 'repair', configured: false }, { role: 'translate', configured: false }] };
  const saved = { ...settingsBody };
  Object.assign(settingsBody, transcribeOnly);
  view.onChanged({ kind: 'settings' });
  await settle(60);
  const sentence = shared ? t('course.confirm.launch_cost_transcribe_only') : t('course.confirm.launch_cost');
  courses = [course('c1', { draft_count: 2, course_actions: ['launch_drafts'] })];
  await show({ draft_total: 2, drafts: [counted('c1', 2)] });
  sent.length = 0;
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation');
  assert.ok(opened()[0].textContent.includes(sentence), `the workbench: ${shared ? 'transcription only, by the shared rule' : 'the usual sentence'}`);
  await cancelDialogs();
  await settle(60);
  assert.deepEqual(writes(), []);
  // the course card's menu gets the settings (courseMenuItems' settings)
  if (shared) {
    const card = page.querySelector('[data-fkey="ccard:c1:menu"]');
    assert.ok(card, "the course card's ⋯");
    card.click();
    const item = doc.querySelectorAll('div.ctx-menu').flatMap((m) => m.querySelectorAll('button.ctx-item')).find((b) => b.textContent.includes(t('menu.course.launch_drafts', { n: 2 })));
    assert.ok(item, 'its "start all drafts"');
    item.click();
    await until(() => opened().length === 1, "the card menu's confirmation");
    assert.ok(opened()[0].textContent.includes(sentence), 'the card menu: transcription only, by the settings the workbench passed');
    await cancelDialogs();
    await settle(60);
    assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  }
  for (const k of Object.keys(settingsBody)) delete settingsBody[k];
  Object.assign(settingsBody, saved);
  view.onChanged({ kind: 'settings' });
  courses = [course('c1')];
  await show({});
  console.log(`F6 hook-ups: ${shared ? 'checked with the shared rule' : 'the shared rule is not in this tree: the usual sentence checked'}`);
}

console.log('r8 dashboard checks: ok');
process.exit(0);
