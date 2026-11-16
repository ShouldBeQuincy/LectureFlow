// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the workbench line (r9-web-dash) on the workbench and the course
// card; each section names its item (WD-nn) and the report it comes from.
// The modules run on the DOM stand-in of r7_dom.mjs; web/r9_dash_items_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared with assert.ok(a === b), never with equal / deepEqual.

import { assert, El, body, doc, dialogs, toastsEl, base, sent, net, settle, until, fk, told, cancelDialogs, confirmDialogs } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
const U = await import(new URL('util.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
// the page's handler of failed requests (app.js): a toast
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });
const Dash = await import(new URL('view_dashboard.js', base).href);

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
const course = (id, o = {}) => ({ id, code: id.toUpperCase(), name: `Course ${id}`, lecture_count: 4, term_count: 0, draft_count: 0, stale_count: 0, running_count: 0, patches_pending: 0, last_activity_at: null, course_actions: [], ...o });
const counted = (id, n, o = {}) => ({ course_id: id, course_code: id.toUpperCase(), course_name: `Course ${id}`, count: n, ...o });

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
const view = Dash.mountDashboard(page, dctx);
await settle();
const show = async (d) => {
  dash = d;
  view.refresh();
  await settle();
};
const writes = () => sent.filter((s) => !s.startsWith('GET ')).map((s) => s.replace(/\?.*$/, ''));
const opened = () => dialogs.filter((d) => d.open);

// ---- WD-04 (R86 L1): the launch for several courses names them ----

{
  // two courses, one without a code (its name instead): one confirmation
  // that says how many courses and which, in the order shown
  courses = [course('c1', { draft_count: 2, course_actions: ['launch_drafts'] }), course('c2', { code: '', name: 'Algorithms', draft_count: 3, course_actions: ['launch_drafts'] })];
  await show({ draft_total: 5, drafts: [counted('c1', 2), counted('c2', 3, { course_code: '', course_name: 'Algorithms' })] });
  net.answer = (m, u) => (m === 'POST' && u.startsWith('/api/courses/') ? { status: 200, body: okResults(u.includes('/c1/') ? 2 : 3) } : defaultAnswer(m, u));
  const line = t('course.confirm.launch_courses', { n: 2, courses: U.listJoin(['C1', 'Algorithms']) });
  fk('dash:drafts').focus();
  sent.length = 0;
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation of 启动全部草稿');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.launch_title', { n: 5 })), 'all the lectures counted');
  assert.ok(opened()[0].textContent.includes(line), `the courses named: ${line}`);
  await cancelDialogs();
  await settle(60);
  assert.deepEqual(writes(), [], 'cancelled: nothing is sent');
  await until(() => doc.activeElement === fk('dash:drafts'), 'cancelled: the focus back on the button');
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation again');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  assert.equal(opened().length, 0, 'asked once');
  assert.deepEqual(writes(), ['POST /api/courses/c1/launch-drafts', 'POST /api/courses/c2/launch-drafts'], 'each course as before');
  assert.deepEqual(told(), [`success: ${t('dashboard.launched', { n: 5 })}`]);

  // one course: the shared launch's confirmation, without that line
  courses = [course('c1', { draft_count: 3, course_actions: ['launch_drafts'] })];
  await show({ draft_total: 3, drafts: [counted('c1', 3)] });
  net.answer = (m, u) => (m === 'POST' && u.startsWith('/api/courses/') ? { status: 200, body: okResults(3) } : defaultAnswer(m, u));
  fk('dash:drafts').click();
  await until(() => opened().length === 1, 'the confirmation of one course');
  assert.ok(opened()[0].textContent.includes(t('course.confirm.launch_title', { n: 3 })));
  assert.ok(!opened()[0].textContent.includes(t('course.confirm.launch_courses', { n: 1, courses: 'C1' })), 'one course: not named');
  await cancelDialogs();
  await settle(60);
  told();
  net.answer = defaultAnswer;
  courses = [course('c1')];
  await show({});
}

// ---- WD-05 (R86 L5): the Colab warning read out by the part that changed ----

const C = await import(new URL('settings_colab.js', base).href);
const remote = (o = {}) => ({ configured: true, state: 'running', accelerator: 'L4', tier: 'paid', window_used_sec: 3600, window_total_sec: 14400, lectures_left: 3, session_spent_cu: 1.31, cu_per_hour: 1.54, runtime: { held: true, reachable: 'unreachable', billing: true }, ...o });
const snapshot = (r) => ({ sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: r });
const slot = () => page.querySelector('div.dash-colab-slot');
// the text nodes under n, in order (a part changed in place gets a new one)
const textNodes = (n, out = []) => {
  for (const c of n.children) {
    if (c instanceof El) textNodes(c, out);
    else out.push(c);
  }
  return out;
};

{
  let res = snapshot(remote());
  dctx.resources = () => res;
  await show({});
  view.onResources(res);
  assert.ok(slot().getAttribute('role') === 'status', 'a status');
  assert.ok(slot().getAttribute('aria-atomic') === 'false', 'read out by the nodes that change, not whole');
  const warn = slot().querySelector('section.dash-colab-warn');
  assert.ok(warn, 'the warning appears in the live region (read out when it appears)');
  const title = warn.querySelector('p.asr-notice-title');
  const text = warn.querySelector('p.asr-notice-text');
  // the title and the text as shown before: the parts joined
  const head = t('dashboard.colab.head', { accelerator: 'L4', state: C.stateName('running') });
  assert.equal(title.textContent, [head, t('dashboard.colab.window_left', { time: C.durText(14400 - 3600) }), t('dashboard.colab.lectures_left', { n: 3 })].join(' · '), 'the title as before');
  assert.ok(text.textContent === C.runtimeWarning(res.remote, []), 'the warning sentence as before');
  const before = { title: textNodes(title), text: textNodes(text) };
  // only the time used and the compute units spent change
  res = snapshot(remote({ window_used_sec: 3700, session_spent_cu: 1.42 }));
  view.onResources(res);
  assert.ok(slot().querySelector('section.dash-colab-warn') === warn, 'the same warning');
  const after = { title: textNodes(title), text: textNodes(text) };
  assert.equal(after.title.length, before.title.length, 'the title has the same parts');
  const changed = after.title.filter((n, i) => n !== before.title[i]).map((n) => n.textContent);
  const left = t('dashboard.colab.window_left', { time: C.durText(14400 - 3700) });
  assert.deepEqual(changed, [left], 'in the title only the time left is new');
  assert.ok(title.textContent.includes(left));
  const changedText = after.text.filter((n, i) => n !== before.text[i]).map((n) => n.textContent);
  assert.deepEqual(changedText, [C.runtimeWarning(res.remote, [])], 'in the text only the sentence with the compute units');
  // nothing changed: no node replaced
  view.onResources(res);
  assert.ok(textNodes(title).every((n, i) => n === after.title[i]) && textNodes(text).every((n, i) => n === after.text[i]), 'the same numbers: nothing replaced');
  // a part added (the lecture's step): the title drawn anew, the warning kept
  res = snapshot(remote({ window_used_sec: 3700, session_spent_cu: 1.42, current: { step: 'transcribe' } }));
  view.onResources(res);
  assert.ok(slot().querySelector('section.dash-colab-warn') === warn, 'still the same warning');
  assert.ok(title.textContent.includes(t('dashboard.colab.step', { step: C.stepName('transcribe') })), 'with the step');
  // the calm line: its parts in place too
  res = snapshot(remote({ runtime: { held: true, reachable: 'ok', billing: true } }));
  view.onResources(res);
  const line = slot().querySelector('p.dash-colab');
  assert.ok(line, 'the calm line');
  const calm = textNodes(line);
  res = snapshot(remote({ runtime: { held: true, reachable: 'ok', billing: true }, lectures_left: 2 }));
  view.onResources(res);
  const calmChanged = textNodes(line).filter((n, i) => n !== calm[i]).map((n) => n.textContent);
  assert.deepEqual(calmChanged, [t('dashboard.colab.lectures_left', { n: 2 })], 'the calm line: only the count of lectures is new');
  dctx.resources = () => null;
  view.onResources(null);
  await show({});
}

// ---- WD-06 (R86 L6): each course card's ⋯ names its course ----

{
  const { event } = await import('./r7_dom.mjs');
  const M = await import(new URL('menu.js', base).href);
  courses = [course('c1', { draft_count: 2, course_actions: ['launch_drafts'] }), course('c2', { code: '', name: 'Algorithms' }), course('c3')];
  await show({});
  const name = (id) => fk(`ccard:${id}:menu`).getAttribute('aria-label');
  assert.equal(name('c1'), t('course.card.menu_named', { name: 'C1' }), 'named with its code');
  assert.equal(name('c2'), t('course.card.menu_named', { name: 'Algorithms' }), 'no code: its name');
  assert.ok(new Set(['c1', 'c2', 'c3'].map(name)).size === 3, 'no two alike');
  assert.equal(fk('ccard:c1:menu').getAttribute('title'), name('c1'), 'the tooltip says the same');
  // the ⋯ and the right-click menu: the same items as before
  const labels = () => doc.querySelectorAll('div.ctx-menu').flatMap((m) => m.querySelectorAll('button.ctx-item')).map((b) => b.querySelector('span.ctx-label').textContent);
  fk('ccard:c1:menu').click();
  const fromMore = labels();
  M.closeMenu({ focus: false });
  const realMatches = El.prototype.matches;
  El.prototype.matches = function (sel) {
    return sel.includes(',') ? ['INPUT', 'TEXTAREA', 'SELECT'].includes(this.tagName) : realMatches.call(this, sel);
  };
  try {
    fk('ccard:c1').dispatchEvent(event('contextmenu', { clientX: 20, clientY: 20, button: 2 }));
    assert.ok(fromMore.includes(t('menu.course.launch_drafts', { n: 2 })), `the ⋯ opens the course menu (${fromMore.join(' | ')})`);
    assert.deepEqual(labels(), fromMore, 'the same items as the right-click menu');
    M.closeMenu({ focus: false });
  } finally {
    El.prototype.matches = realMatches;
  }
  courses = [course('c1')];
  await show({});
}

// ---- WD-07 (T28 suggestion 11): an earlier runtime not released is folded away ----

{
  const manual = t('resources.remote.manual_end');
  const earlierAny = t('resources.remote.leftover_earlier_any');
  // billed, with 立即关机, and an earlier runtime not released either
  let res = snapshot(remote({ leftover: { code: 'colab.release_failed', accelerator: '' } }));
  dctx.resources = () => res;
  await show({});
  view.onResources(res);
  const warn = slot().querySelector('section.dash-colab-warn');
  const details = warn.querySelector('details');
  assert.ok(details, 'the earlier runtime in a folded part');
  assert.ok(!details.open, 'folded by default');
  assert.equal(details.querySelector('summary').textContent, t('dashboard.colab.earlier_summary'), 'under a line of its own');
  const folded = details.querySelector('p').textContent;
  assert.ok(folded.includes(earlierAny), 'opened: the whole sentence about it');
  assert.ok(folded.includes(manual), 'with the way to end it by hand (no session being released)');
  const outside = warn.querySelector('p.asr-notice-text').textContent;
  assert.equal(outside, C.runtimeWarning(res.remote, []), "outside: this session's warning alone");
  assert.ok(fk('dash:colab:now'), 'and its button');
  // opened, then only the numbers change: the same folded part, still open
  details.open = true;
  res = snapshot(remote({ leftover: { code: 'colab.release_failed', accelerator: '' }, session_spent_cu: 1.6, window_used_sec: 4000 }));
  view.onResources(res);
  assert.ok(slot().querySelector('details') === details && details.open, 'numbers changed: the same folded part, still open');
  // the warning drawn anew (its button busy): still open
  const realBusy = dctx.colabBusy;
  dctx.colabBusy = () => true;
  view.onResources(res);
  assert.ok(fk('dash:colab:now').disabled, 'drawn anew, the button busy');
  assert.ok(slot().querySelector('details').open, 'the folded part still open');
  dctx.colabBusy = realBusy;
  view.onResources(res);
  // being released and still billed: the way out by hand stays in sight
  res = snapshot(remote({ state: 'stopping', leftover: { code: 'colab.release_failed', accelerator: '' } }));
  view.onResources(res);
  const w2 = slot().querySelector('section.dash-colab-warn');
  assert.ok(w2.querySelector('p.asr-notice-text').textContent.includes(manual), 'releasing: the way out by hand in sight');
  assert.ok(w2.querySelector('details').querySelector('p').textContent === earlierAny, 'the earlier runtime folded alone');
  assert.ok(slot().textContent.includes(earlierAny), 'still in the warning (the T24 S6 checks)');
  // no earlier runtime: nothing folded
  res = snapshot(remote());
  view.onResources(res);
  assert.ok(slot().querySelector('details') === null, 'no earlier runtime: nothing folded');
  // a runtime not released without a session: its usual warning, not folded
  res = snapshot({ ...remote({ state: 'none', runtime: null }), leftover: { code: 'colab.release_failed', accelerator: 'T4' } });
  view.onResources(res);
  assert.ok(slot().querySelector('details') === null, 'alone: not folded');
  assert.ok(slot().textContent.includes(C.leftoverText(res.remote.leftover)) && slot().textContent.includes(manual));
  dctx.resources = () => null;
  view.onResources(null);
  await show({});
}

// ---- R88 M1, L2 (WD-07): the folded part keeps the focus; an earlier runtime appearing later ----

{
  // a browser takes the focus off a control taken out of the page (the
  // stand-in keeps it): the page's redraw takes the Colab slot out and puts
  // it back
  const plainRemove = Object.getPrototypeOf(El.prototype).remove;
  Object.getPrototypeOf(El.prototype).remove = function () {
    const a = doc.activeElement;
    if (this.parentNode && a && a !== body && (this === a || (this instanceof El && this.contains(a)))) doc.activeElement = body;
    return plainRemove.call(this);
  };
  try {
    let res = snapshot(remote({ leftover: { code: 'colab.release_failed', accelerator: '' } }));
    dctx.resources = () => res;
    await show({});
    view.onResources(res);
    const sum = fk('dash:colab:earlier');
    assert.ok(sum && sum.tagName === 'SUMMARY', 'the folded part opens from a control with a focus key');
    sum.parentNode.open = true;
    sum.focus();
    // a whole redraw (a refresh): the focus stays on it, still open
    await show({});
    assert.ok(doc.activeElement === fk('dash:colab:earlier'), 'a redraw: the focus still on the folded part');
    assert.ok(fk('dash:colab:earlier').parentNode.open, 'still open');
    // the warning drawn anew (its button busy): the same
    const realBusy = dctx.colabBusy;
    dctx.colabBusy = () => true;
    view.onResources(res);
    assert.ok(fk('dash:colab:earlier') !== sum, 'the warning drawn anew');
    assert.ok(doc.activeElement === fk('dash:colab:earlier'), 'drawn anew: the focus on the new one');
    assert.ok(fk('dash:colab:earlier').parentNode.open, 'still open');
    dctx.colabBusy = realBusy;
    body.focus();

    // L2: billed with the warning shown, then an earlier runtime appears and
    // nothing else changes: it is drawn (the structure tells it apart)
    res = snapshot(remote());
    view.onResources(res);
    assert.ok(slot().querySelector('section.dash-colab-warn') && !slot().querySelector('details'), 'the warning, nothing folded');
    res = snapshot(remote({ leftover: { code: 'colab.release_failed', accelerator: '' } }));
    view.onResources(res);
    assert.ok(slot().querySelector('details'), 'an earlier runtime appearing later: folded in');
    assert.ok(slot().textContent.includes(t('resources.remote.leftover_earlier_any')), 'with its sentence');
    // and gone again
    res = snapshot(remote());
    view.onResources(res);
    assert.ok(!slot().querySelector('details') && !slot().textContent.includes(t('resources.remote.leftover_earlier_any')), 'gone again: nothing folded');
  } finally {
    Object.getPrototypeOf(El.prototype).remove = plainRemove;
  }
  dctx.resources = () => null;
  view.onResources(null);
  await show({});
}

// ---- WD-10 (T28 suggestion 2, convention C2): a failure said as the lecture page says it ----

{
  const refusal = { code: 'llm.refusal', params: { role: 'translate', provider: 'Anthropic', model: 'claude-opus-5-5' } };
  const timeLimit = { code: 'llm.time_limit', params: { role: 'translate', seconds: 600, provider: 'Anthropic', model: 'claude-opus-5-5' } };
  const balance = { code: 'llm.insufficient_balance', params: { role: 'translate', provider: 'DeepSeek' } };
  const fail = (id, o = {}) => ({ lecture_id: id, stage: 'translate', course_id: 'c1', course_code: 'C1', course_name: 'Course c1', lecture_title: `Lecture ${id}`, kind: 'lecture', week: 0, allowed_actions: ['retry'], error: 'x', finished_at: null, ...o });
  const list = [fail('R', { error_info: refusal }), fail('T', { error_info: timeLimit }), fail('B1', { error_info: balance }), fail('B2', { error_info: balance }), fail('S', { stage: 'summary', error_info: null, error: 'boom\nthe rest' })];
  // what the lecture page's stage card says (U.stageErrorText knows the stage)
  const said = (f) => U.stageErrorText({ stage: f.stage, status: 'failed', error_info: f.error_info, error: f.error });
  await show({ failures: list });
  const row = (f) => fk(`dash:fail:${f.lecture_id}:${f.stage}`).parentNode;
  for (const f of [list[0], list[1], list[4]]) {
    const why = row(f).querySelector('div.fail-why');
    assert.ok(why, `${f.lecture_id}: its reason under the title`);
    assert.equal(why.textContent, said(f).split('\n')[0], `${f.lecture_id}: as the stage card says it`);
    assert.equal(why.getAttribute('title'), said(f), `${f.lecture_id}: the whole text on hover`);
  }
  // one reason for two lectures: said once, above them, as before
  const heads = page.querySelectorAll('li.fail-reason');
  assert.equal(heads.length, 1, 'the rows grouped by reason as before');
  assert.equal(heads[0].querySelector('span').textContent, said(list[2]).split('\n')[0], 'the shared reason as the stage card says it');
  assert.equal(page.querySelectorAll('li.fail-row').length, 5);
  await show({});
}

view.destroy();
console.log('r9 dash items checks: ok');
