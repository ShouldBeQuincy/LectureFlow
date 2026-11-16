// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 8, the shared web modules (the r8-web-shared line):
//   WH-01  the menus' "update" (a lecture row, the lecture page's "⋯"), a
//          course card's "start all drafts" and "update all out-of-date
//          lectures" ask first (T25 X1); the glossary question comes before
//          the "start all drafts" confirmation (R74 low 2a);
//   WH-07  that confirmation's cost sentence follows the settings
//          (convention F6: billed, transcription only, prices at 0); its
//          initial focus is "cancel" (R74 low 2d, low 9);
//   WH-06  a dialog's failed write while the connection is lost says "not
//          done" as the app does (R76 L-4);
//   WH-02  a control in a stage card keeps the keyboard focus over a
//          redraw, through the card's segment (R79 M2);
//   WH-05  a course-level stage the model refused offers the roles first;
//   WH-09  "open settings" goes to the provider the failure names;
//   WH-04  file sizes counted by 1000 as kB / MB / GB, "714 bytes";
//   WH-03  the download menu says what each file is (menu.js desc);
//   handed over: launchLectures' opts.ask (r8-web-lecture); api.js
//          err.method, and tellFailure tells only an action's failure while
//          the connection is lost (r8-web-dash WD-09's rule);
// The modules run on the DOM stand-in of r7_dom.mjs; web/r8_shared_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared by identity only (assert.ok(a === b, …)).

import { assert, base, net, settle, told, cancelDialogs, confirmDialogs, dialogs, doc, body, El } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
const M = await import(new URL('menu.js', base).href);
const UC = await import(new URL('update_confirm.js', base).href);
const { t } = I;
await I.setLocale('zh-CN');
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

// The write requests sent, as "METHOD url body" (body: the JSON sent, ''
// for none); answers: "METHOD url" → (body) => {status, body}.
const writes = [];
let answers = {};
net.answer = (m, u, b) => {
  if (m !== 'GET') writes.push(`${m} ${u} ${b === undefined ? '' : JSON.stringify(b)}`);
  const f = answers[`${m} ${u}`];
  return f ? f(b) : { status: 200, body: {} };
};
const takeWrites = () => writes.splice(0);
const openTitles = () => dialogs.filter((d) => d.open).map((d) => d.getAttribute('aria-label'));

// Opens items as the menu of a "⋯" button and selects the item labelled
// label (its .ctx-label); → the button (the menu's opener, focused).
let opener = null;
function select(items, label) {
  if (opener) opener.remove();
  opener = new El('button');
  opener.dataset.fkey = 'r8:opener';
  body.append(opener);
  opener.focus();
  assert.ok(M.openMenu(items, { anchor: opener }, { returnFocus: opener }), 'the menu opens');
  const item = doc.querySelectorAll('.ctx-item').find((b) => {
    const l = b.querySelector('.ctx-label');
    return !!l && l.textContent === label;
  });
  assert.ok(item, `the menu has "${label}"`);
  item.click();
  return opener;
}

// ---- WH-01: the menus' batch operations ask first ----

{
  const st = (stage, status) => ({ stage, status, allowed_actions: [] });
  const lec = {
    id: 'L1',
    title: 'Lecture 1',
    course_id: 'c1',
    lecture_actions: ['update'],
    stages: [st('prep', 'done'), st('asr', 'done'), st('summary', 'done'), st('repair', 'stale'), st('translate', 'done'), st('export', 'done')],
  };
  const changed = [];
  const hd = { onLectures: (r) => changed.push(r), onDeleted() {} };
  const lectureOK = { status: 200, body: { ...lec, lecture_actions: [] } };
  const diff = [{ lecture_id: 'L1', lecture_title: 'Lecture 1', stage: 'repair', snapshot: { provider_id: 'p1', provider_name: 'P', model: 'm1' }, current: { provider_id: 'p1', provider_name: 'P', model: 'm2' }, snapshot_usable: true }];
  const needsChoice = (b) => (b && b.model_choice ? lectureOK : { status: 409, body: { error: { code: 'needs_model_choice' }, diff } });
  Act.setCourseLookup(() => null);
  takeWrites();
  told();

  // the request of an update as it was sent before (updateLectures itself
  // does not ask: the course page asks before it calls it)
  answers = { 'POST /api/lectures/L1/update': () => lectureOK };
  await Act.updateLectures([lec]);
  const updateReq = takeWrites();
  assert.deepEqual(updateReq, ['POST /api/lectures/L1/update '], 'the update request');
  told();

  // the lecture menu's "update": asked first; cancelled, nothing is sent and
  // the focus is back on the menu's button
  let btn = select(Act.lectureMenuItems(lec, hd), t('menu.lecture.update'));
  await settle();
  assert.deepEqual(openTitles(), [t('course.confirm.update_title')], 'the lecture menu\'s "update" asks first');
  assert.equal(writes.length, 0, 'nothing is sent while it asks');
  await cancelDialogs();
  assert.equal(writes.length, 0, 'cancelled: nothing is sent');
  assert.ok(doc.activeElement === btn, 'cancelled: the focus is back on the button that opened the menu');
  // selected twice while it asks: one dialog; confirmed, the same request
  // as before, once
  select(Act.lectureMenuItems(lec, hd), t('menu.lecture.update'));
  await settle();
  select(Act.lectureMenuItems(lec, hd), t('menu.lecture.update'));
  await settle();
  assert.equal(openTitles().length, 1, 'selected twice: one dialog');
  changed.length = 0;
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), updateReq, 'confirmed: the same request as before, once');
  // (each selection hears of the one answer they share, as for a double click)
  assert.ok(changed.length > 0 && changed.every((r) => Array.isArray(r) && r.length === 1 && r[0].id === 'L1'), 'and the view hears of the lecture');
  told();
  // the model question (409 needs_model_choice) comes after the confirmation
  answers = { 'POST /api/lectures/L1/update': needsChoice };
  select(Act.lectureMenuItems(lec, hd), t('menu.lecture.update'));
  await settle();
  assert.deepEqual(openTitles(), [t('course.confirm.update_title')], 'the confirmation first');
  assert.equal(writes.length, 0);
  await confirmDialogs();
  await settle();
  assert.deepEqual(openTitles(), [t('common.model_choice.title')], 'then the model question');
  assert.deepEqual(takeWrites(), updateReq, 'asked after the first request, as before');
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), ['POST /api/lectures/L1/update {"model_choice":"snapshot"}'], 'the chosen model sent as before');
  told();

  // a course card's menu: "start all drafts" and "update all out-of-date
  // lectures"
  const card = { id: 'c1', code: 'C1', name: 'Course 1', course_actions: ['launch_drafts', 'update_stale'], draft_count: 3, stale_lecture_count: 2 };
  const results = { results: [{ ok: true, lecture: { id: 'L9', title: 'Lecture 9' } }] };
  const courseHd = { onChanged: (r) => changed.push(r) };
  answers = {
    'POST /api/courses/c1/launch-drafts': () => ({ status: 200, body: results }),
    'POST /api/courses/c1/update-stale': () => ({ status: 200, body: results }),
  };
  await Act.launchCourseDrafts(card);
  const launchReq = takeWrites();
  assert.deepEqual(launchReq, ['POST /api/courses/c1/launch-drafts '], 'the launch request');
  await Act.updateCourseStale(card);
  const staleReq = takeWrites();
  assert.deepEqual(staleReq, ['POST /api/courses/c1/update-stale '], 'the update-stale request');
  told();

  const launchTitle = t('course.confirm.launch_title', { n: 3 });
  btn = select(Act.courseMenuItems(card, courseHd), t('menu.course.launch_drafts', { n: 3 }));
  await settle();
  assert.deepEqual(openTitles(), [launchTitle], '"start all drafts" asks first, with the course\'s count');
  await cancelDialogs();
  assert.equal(writes.length, 0, 'cancelled: no drafts started');
  assert.ok(doc.activeElement === btn, 'cancelled: the focus is back on the menu\'s button');
  changed.length = 0;
  select(Act.courseMenuItems(card, courseHd), t('menu.course.launch_drafts', { n: 3 }));
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), launchReq, 'confirmed: the same request as before');
  assert.equal(changed.length, 1, 'and the view hears of it');
  told();
  // the glossary still being extracted: its question first, then this one
  // (R74 low 2a), one of each
  Act.setCourseLookup((id) => (id === 'c1' ? { ...card, glossary_pending: true } : null));
  select(Act.courseMenuItems(card, courseHd), t('menu.course.launch_drafts', { n: 3 }));
  await settle();
  assert.deepEqual(openTitles(), [t('common.glossary_check.title')], 'the glossary question first, alone');
  await confirmDialogs();
  await settle();
  assert.deepEqual(openTitles(), [launchTitle], 'then the confirmation');
  assert.equal(writes.length, 0, 'nothing sent before it is answered');
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), launchReq, 'both answered: started once');
  assert.deepEqual(openTitles(), [], 'no third dialog');
  told();
  // the course page's header path (view_course.js, launchCourseDrafts with
  // ask, interface F2): the glossary question, then one confirmation
  const drafts = [{ id: 'D1', title: 'Draft 1', course_id: 'c1', draft: true, lecture_actions: ['launch'] }];
  const before = dialogs.length;
  const p = Act.launchCourseDrafts(card, { ask: () => UC.confirmLaunchDrafts(drafts, card.draft_count) });
  await settle();
  assert.deepEqual(openTitles(), [t('common.glossary_check.title')], 'the header path: the glossary question first');
  await confirmDialogs();
  await settle();
  assert.deepEqual(openTitles(), [t('course.confirm.launch_title', { n: 1 })], 'then its confirmation');
  await confirmDialogs();
  await p;
  assert.equal(dialogs.length - before, 2, 'two dialogs in all');
  assert.deepEqual(takeWrites(), launchReq, 'started once');
  Act.setCourseLookup(() => null);
  told();

  const staleTitle = t('course.confirm.update_title_many', { n: 2 });
  btn = select(Act.courseMenuItems(card, courseHd), t('menu.course.update_stale', { n: 2 }));
  await settle();
  assert.deepEqual(openTitles(), [staleTitle], '"update all out-of-date lectures" asks first, with the course\'s count');
  const dlg = dialogs.filter((d) => d.open)[0];
  assert.ok(dlg.textContent.includes(t('course.confirm.update_unknown')), 'what an update does, without the list');
  await cancelDialogs();
  assert.equal(writes.length, 0, 'cancelled: nothing updated');
  assert.ok(doc.activeElement === btn, 'cancelled: the focus is back on the menu\'s button');
  select(Act.courseMenuItems(card, courseHd), t('menu.course.update_stale', { n: 2 }));
  await settle();
  select(Act.courseMenuItems(card, courseHd), t('menu.course.update_stale', { n: 2 }));
  await settle();
  assert.equal(openTitles().length, 1, 'selected twice: one dialog');
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), staleReq, 'confirmed: the same request as before, once');
  told();
  // the model question after the confirmation
  answers['POST /api/courses/c1/update-stale'] = (b) => (b && b.model_choice ? { status: 200, body: results } : { status: 409, body: { error: { code: 'needs_model_choice' }, diff } });
  select(Act.courseMenuItems(card, courseHd), t('menu.course.update_stale', { n: 2 }));
  await settle();
  assert.deepEqual(openTitles(), [staleTitle], 'the confirmation first');
  await confirmDialogs();
  await settle();
  assert.deepEqual(openTitles(), [t('common.model_choice.title')], 'then the model question');
  await confirmDialogs();
  await settle();
  assert.deepEqual(takeWrites(), [...staleReq, 'POST /api/courses/c1/update-stale {"model_choice":"snapshot"}'], 'the requests as before');
  told();
  opener.remove();
  opener = null;
}

// ---- WH-07: the cost sentence of "start all drafts" says what happens ----

{
  const lastOpen = () => dialogs.filter((d) => d.open).at(-1) || null;
  const cost = {
    billed: t('course.confirm.launch_cost'),
    transcribe_only: t('course.confirm.launch_cost_transcribe_only'),
    free: t('course.confirm.launch_cost_free'),
  };
  assert.ok(new Set(Object.values(cost)).size === 3, 'three different sentences');
  // which of the three a dialog says (exactly one of them)
  const said = (d) => Object.keys(cost).filter((k) => d.textContent.includes(cost[k]));
  const cases = [
    [[], 'billed', 'no opts: as before'],
    [[{}], 'billed', 'no costMode: as before'],
    [[null], 'billed', 'null opts: as before'],
    [[{ costMode: 'billed' }], 'billed', 'billed'],
    [[{ costMode: 'transcribe_only' }], 'transcribe_only', 'transcription only'],
    [[{ costMode: 'free' }], 'free', 'prices at 0'],
    [[{ costMode: 'constructor' }], 'billed', 'an unknown mode: as before'],
  ];
  for (const [extra, want, what] of cases) {
    const p = UC.confirmLaunchDrafts([], 2, ...extra);
    await settle();
    const d = lastOpen();
    assert.ok(d, `${what}: a dialog`);
    assert.deepEqual(said(d), [want], `${what}: the cost sentence`);
    await cancelDialogs();
    assert.equal(await p, false, `${what}: cancelled`);
  }
  // the default focus is on "cancel" (R74 low 9): the browser focuses the
  // autofocus control when the dialog opens, and Enter there cancels
  const p = UC.confirmLaunchDrafts([], 2, { costMode: 'billed' });
  await settle();
  const d = lastOpen();
  const auto = d.querySelectorAll('button').filter((b) => b.hasAttribute('autofocus'));
  assert.equal(auto.length, 1, 'one control has the initial focus');
  assert.equal(auto[0].textContent, t('common.cancel'), 'and it is "cancel"');
  assert.ok(!/btn-primary/.test(auto[0].className), 'not the primary button');
  auto[0].focus();
  auto[0].click(); // Enter on the focused button
  assert.equal(await p, false, 'Enter on the initial focus does not start the drafts');
  d.open = false;

  // the mode from the settings a page has (launchCostMode)
  const role = (r, configured, provider_id = 'p1') => ({ role: r, configured, provider_id });
  const three = (configured, ids = ['p1', 'p1', 'p1']) => ['summary', 'repair', 'translate'].map((r, i) => role(r, configured, ids[i]));
  const zero = { id: 'p0', price_input: 0, price_cache_hit: 0, price_output: 0, tariff: null };
  const priced = { id: 'p1', price_input: 2, price_cache_hit: 0.5, price_output: 8, tariff: null };
  const unpriced = { id: 'p2', price_input: null, price_cache_hit: null, price_output: null, tariff: null };
  const modes = [
    [null, undefined, 'no settings'],
    [{}, undefined, 'no roles'],
    [{ roles: [...three(false), role('organizer', true)], providers: [priced] }, 'transcribe_only', 'no lecture role set up'],
    [{ roles: three(true), providers: [priced] }, 'billed', 'priced'],
    [{ roles: three(true, ['p2', 'p2', 'p2']), providers: [unpriced] }, 'billed', 'prices not known'],
    [{ roles: three(true, ['p0', 'p0', 'p0']), providers: [zero] }, 'free', 'every price 0'],
    [{ roles: three(true, ['p0', 'p0', 'p0']), providers: [{ ...zero, price_cache_hit: null }] }, 'free', 'no cache price, the others 0'],
    [{ roles: three(true, ['p0', 'p0', 'p0']), providers: [{ ...zero, tariff: { peaks: [] } }] }, 'billed', 'peak pricing'],
    [{ roles: three(true, ['p0', 'p0', 'p1']), providers: [zero, priced] }, 'billed', 'one of them priced'],
    [{ roles: three(true, ['p0', 'p0', 'gone']), providers: [zero] }, 'billed', 'a provider not in the list'],
    [{ roles: [role('summary', false)], providers: [] }, 'billed', 'roles missing: not transcription only'],
  ];
  for (const [s, want, what] of modes) assert.equal(UC.launchCostMode(s), want, `launchCostMode: ${what}`);

  // a course menu passes the mode when its page gave the settings
  // (hd.settings, never read for it), else says what it always said
  const card = { id: 'c2', code: 'C2', name: 'Course 2', course_actions: ['launch_drafts'], draft_count: 2 };
  const label = t('menu.course.launch_drafts', { n: 2 });
  const only = { roles: three(false), providers: [] };
  for (const [hd, want, what] of [
    [{}, 'billed', 'no settings given'],
    [{ settings: only }, 'transcribe_only', 'the settings given'],
    [{ settings: () => only }, 'transcribe_only', 'the settings given as a function'],
    [{ settings: () => null }, 'billed', 'the settings not read yet'],
  ]) {
    select(Act.courseMenuItems(card, hd), label);
    await settle();
    const dlg = lastOpen();
    assert.ok(dlg, `menu, ${what}: asked`);
    assert.deepEqual(said(dlg), [want], `menu, ${what}: the cost sentence`);
    await cancelDialogs();
  }
  assert.equal(writes.length, 0, 'nothing sent');
  opener.remove();
  opener = null;
  told();
}

// ---- WH-06: a dialog's failed request while the connection is lost ----

{
  // the "connection lost" banner of the page (index.html #conn-banner)
  const banner = new El('div');
  banner.attrs.id = 'conn-banner';
  banner.hidden = true;
  body.append(banner);
  // a dialog whose request (busy) fails with err after the dialog was
  // closed by the user (Esc, "cancel") while it ran
  const failAfterClose = async (err) => {
    let rej;
    const req = new Promise((_, r) => (rej = r));
    let closeIt;
    const done = D.openDialog('t', (close, busy) => {
      closeIt = close;
      busy(req).catch(() => {}); // the dialog's own handling (it is closed)
      return new El('div');
    });
    closeIt(null);
    await done;
    rej(err);
    await settle();
  };
  const lost = () => new A.ApiError(0, t('common.net.unreachable'));
  const warnLine = `warn: ${t('nav.conn_lost_action')}`;
  told();

  // the banner shows: the app's own sentence, as a warning (app.js onError)
  banner.hidden = false;
  await failAfterClose(lost());
  const shown = told();
  assert.deepEqual(shown, [warnLine], 'the banner shows: "not done", the warning the app gives');
  // several fail: one toast, counted
  await failAfterClose(lost());
  await failAfterClose(lost());
  const toasts = [...doc.getElementById('toasts').children];
  assert.equal(toasts.length, 1, 'several failures: one toast');
  assert.equal(toasts[0].querySelector('.toast-count').textContent, '×2', 'counted');
  told();
  // told already by the API layer, aborted, or cancelled by the user: nothing
  const reported = lost();
  reported.reported = true;
  await failAfterClose(reported);
  const aborted = new Error('aborted');
  aborted.name = 'AbortError';
  await failAfterClose(aborted);
  await failAfterClose(new A.ApiError(-1, 'cancelled'));
  assert.deepEqual(told(), [], 'reported, aborted, cancelled: nothing more');
  // the banner hidden (connected): the error as before
  banner.hidden = true;
  await failAfterClose(lost());
  assert.deepEqual(told(), [`error: ${t('common.net.unreachable')}`], 'connected: the error toast, as before');
  await failAfterClose(new A.ApiError(500, 'server says no'));
  assert.deepEqual(told(), ['error: server says no'], 'another error: as before');
  banner.remove();
}

// ---- WH-02: a control in a stage card keeps the focus over a redraw ----

{
  const P = await import(new URL('pipeline.js', base).href);
  // realFocus (as in a11y_test.mjs): a control in a stage card is shown,
  // and takes the focus, only while the focus is in its stage (the card's
  // :focus-within; the pointer is not modelled); a disabled control takes
  // none; a removed focused element leaves the focus on the page; the
  // focus moves with focusin / focusout; :focus-visible is keyboard focus.
  const { Node: SNode } = await import('./r7_dom.mjs');
  const { event } = await import('./r7_dom.mjs');
  const plain = { focus: El.prototype.focus, matches: El.prototype.matches, remove: SNode.prototype.remove };
  let focusBy = 'keyboard';
  const hiddenInCard = (el) => {
    for (let n = el; n; n = n.parentNode) {
      if (n.classList && n.classList.contains('seg-card')) {
        const wrap = n.parentNode;
        return !wrap || wrap.classList.contains('is-dismissed') || !wrap.contains(doc.activeElement);
      }
    }
    return false;
  };
  El.prototype.focus = function () {
    const prev = doc.activeElement;
    if (prev === this || this.disabled || hiddenInCard(this)) return;
    doc.activeElement = this;
    if (prev && prev.dispatchEvent) prev.dispatchEvent(event('focusout', { relatedTarget: this }));
    this.dispatchEvent(event('focusin', { relatedTarget: prev }));
  };
  El.prototype.matches = function (sel) {
    return sel === ':focus-visible' ? focusBy === 'keyboard' : plain.matches.call(this, sel);
  };
  SNode.prototype.remove = function () {
    if (this.contains && this.contains(doc.activeElement)) doc.activeElement = body;
    return plain.remove.call(this);
  };
  try {
    const host = new El('div');
    body.append(host);
    const done = (stage) => ({ stage, status: 'done', allowed_actions: [] });
    const failed = () => ({
      stage: 'translate',
      status: 'failed',
      allowed_actions: ['retry'],
      error: 'HTTP 400 from the provider: model not found',
      error_info: { code: 'llm.rejected', params: { role: 'translate', retries: 0, status: 400, provider: 'P', model: 'm' } },
    });
    const running = () => ({ stage: 'translate', status: 'running', progress: 0.3, allowed_actions: ['cancel'] });
    const draw = (last, lazy = false) => host.replaceChildren(P.pipelineBar([done('prep'), done('asr'), done('summary'), done('repair'), last()], { key: 'r8f', sys: () => null, res: () => null, onAction: () => Promise.resolve(null), lazyCards: lazy }));
    const fk = (k) => doc.querySelector(`[data-fkey="${k}"]`);
    const seg = () => fk('r8f:seg:translate');
    // into the card from its segment, as Tab goes (the card opens with the focus)
    const into = (key) => {
      seg().focus();
      const el = fk(key);
      assert.ok(el, `${key} is drawn`);
      el.focus();
      assert.ok(doc.activeElement === el, `${key} takes the focus in the open card`);
      return el;
    };

    // the stand-in hides a closed card
    draw(failed);
    body.focus();
    doc.activeElement = body;
    fk('r8f:details:translate').focus();
    assert.ok(doc.activeElement === body, 'a control of a closed card takes no focus');

    // keyboard focus on "技术详情", "复制完整错误", "重试": the bar redrawn (a
    // read of the lecture, a job event) gives it back to the new one
    for (const key of ['r8f:details:translate', 'r8f:copy:translate', 'r8f:act:translate:retry']) {
      const old = into(key);
      D.preserveFocus(() => draw(failed));
      const now = fk(key);
      assert.ok(now && now !== old && now.isConnected, `${key}: drawn anew`);
      assert.ok(doc.activeElement === now, `${key}: the focus is on the new one after the redraw (got ${doc.activeElement && doc.activeElement.tagName})`);
      assert.ok(seg().getAttribute('aria-expanded') === 'true', `${key}: its card is open again`);
    }
    // a card built only when it opens (lazyCards): its control is found
    // once its segment opened it
    draw(running, true);
    into('r8f:act:translate:cancel');
    D.preserveFocus(() => draw(running, true));
    assert.ok(doc.activeElement === fk('r8f:act:translate:cancel'), 'lazy card: the focus on the new "cancel"');
    // the same for a dialog the card's control opened (giveBack): the card
    // drawn anew, unbuilt, while the dialog is open
    into('r8f:act:translate:cancel');
    const asked = D.confirmDialog({ title: 'q', message: 'm' });
    const dlg = dialogs.filter((x) => x.open).at(-1);
    dlg.querySelectorAll('button')[0].focus(); // the dialog has the focus
    draw(running, true);
    await cancelDialogs();
    assert.equal(await asked, false);
    assert.ok(doc.activeElement === fk('r8f:act:translate:cancel'), 'a dialog from a lazy card: back on the new "cancel"');
    // a control gone with the redraw: its fallback (the segment), as before
    into('r8f:act:translate:cancel');
    D.preserveFocus(() => draw(() => ({ ...running(), allowed_actions: [] }), true));
    assert.ok(doc.activeElement === seg(), 'gone: the segment (data-fnext), as before');

    // a mouse click's focus is not moved to the segment (R19: the card
    // would stay open under no pointer): the page, as before
    draw(failed);
    into('r8f:details:translate');
    focusBy = 'mouse';
    D.preserveFocus(() => draw(failed));
    assert.ok(doc.activeElement === body, 'mouse focus: on the page, as before');
    focusBy = 'keyboard';

    // a control in no card: as before
    const plainBtn = () => {
      const b = new El('button');
      b.dataset.fkey = 'r8f:plain';
      return b;
    };
    host.replaceChildren(plainBtn());
    fk('r8f:plain').focus();
    D.preserveFocus(() => host.replaceChildren(plainBtn()));
    assert.ok(doc.activeElement === fk('r8f:plain'), 'not in a card: the new control, as before');
    // a card control disabled by the redraw (its request runs): the focus
    // waits on the page (R21), and comes back once it is enabled
    draw(failed);
    into('r8f:act:translate:retry');
    D.preserveFocus(() => {
      draw(failed);
      fk('r8f:act:translate:retry').disabled = true;
    });
    assert.ok(doc.activeElement === body, 'disabled meanwhile: waiting on the page');
    D.preserveFocus(() => draw(failed));
    assert.ok(doc.activeElement === fk('r8f:act:translate:retry'), 'enabled again: back on it, through its segment');
    host.remove();
  } finally {
    El.prototype.focus = plain.focus;
    El.prototype.matches = plain.matches;
    SNode.prototype.remove = plain.remove;
    doc.activeElement = body;
  }
}

// ---- WH-05: a course-level stage the model refused: "open settings" first ----

{
  const P = await import(new URL('pipeline.js', base).href);
  const U = await import(new URL('util.js', base).href);
  const failedWith = (stage, code) => ({
    stage,
    status: 'failed',
    allowed_actions: ['retry'],
    error: 'the provider refused',
    error_info: { code, params: { role: stage === 'material_terms' ? 'glossary' : stage, retries: 0, provider: 'P', model: 'm', category: '' } },
  });
  // the card's actions, as "kind:primary?" (a: the settings link, with its href)
  const actionsOf = (st) => {
    const host = new El('div');
    body.append(host);
    host.append(P.pipelineBar([st], { key: 'r8s', sys: () => null, res: () => null, onAction: () => Promise.resolve(null), pct: false }));
    const box = host.querySelector('.card-actions');
    const out = box
      ? box.children.map((c) => {
          const primary = /btn-primary/.test(c.className) ? '!' : '';
          return c.tagName === 'A' ? `settings${primary} ${c.getAttribute('href')}` : `${c.dataset.fkey.split(':').pop()}${primary}`;
        })
      : [];
    host.remove();
    return out;
  };
  for (const code of ['llm.refusal', 'llm.time_limit']) {
    for (const stage of ['material_terms', 'glossary_seed', 'glossary_review', 'title']) {
      const st = failedWith(stage, code);
      assert.equal(JSON.stringify(U.settingsFix(st, null)), JSON.stringify({ href: '#/settings/roles', unlisted: null }), `${stage} ${code}: the way out is the roles`);
      assert.deepEqual(actionsOf(st), ['settings! #/settings/roles', 'retry'], `${stage} ${code}: "open settings" first, "retry" a plain button`);
    }
    // the lecture's three stages: "retry with another model…" first, as before
    for (const stage of ['summary', 'repair', 'translate']) {
      const st = failedWith(stage, code);
      assert.equal(U.settingsFix(st, null), null, `${stage} ${code}: no settings fix, as before`);
      assert.deepEqual(actionsOf(st), ['retry_model!', 'retry'], `${stage} ${code}: "retry with another model…" first, as before`);
    }
  }
  // other codes of a course-level stage: as before
  assert.equal(U.settingsFix(failedWith('material_terms', 'llm.network'), null), null, 'a network failure: no settings fix');
  assert.equal(JSON.stringify(U.settingsFix(failedWith('material_terms', 'llm.no_model'), null)), JSON.stringify({ href: '#/settings/roles', unlisted: null }), 'llm.no_model: the roles, as before');
  assert.equal(U.settingsFix({ stage: 'material_terms', status: 'failed' }, null), null, 'no coded error: nothing');
  assert.equal(U.settingsFix(null, null), null, 'no stage: nothing');
}

// ---- WH-09: "open settings" goes to the provider the failure names ----

{
  const U = await import(new URL('util.js', base).href);
  const fix = (code, params, providers, model) => JSON.stringify(U.settingsFix({ stage: 'translate', status: 'failed', model, error_info: { code, params } }, providers));
  const want = (href, unlisted = null) => JSON.stringify({ href, unlisted });
  const ds = { id: 'p-ds', name: 'DeepSeek', models: ['deepseek-chat'] };
  const other = { id: 'p-x', name: 'Other', models: [] };
  // the back end names the provider by its name (llm.no_api_key {provider},
  // llm.rejected / llm.request_failed {provider, model}); found once in the
  // providers the page read: its row
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, [other, ds]), want('#/settings/providers/p-ds'), 'no key: the provider\'s row');
  assert.equal(fix('llm.rejected', { role: 'translate', status: 401, provider: 'DeepSeek', model: 'deepseek-chat' }, [ds]), want('#/settings/providers/p-ds'), 'a key refused: the provider\'s row');
  assert.equal(fix('llm.request_failed', { role: 'translate', status: 500, provider: 'DeepSeek', model: 'deepseek-chat' }, [ds]), want('#/settings/providers/p-ds'), 'a request failed: the provider\'s row');
  // an id named by the failure itself (not given by the back end today)
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek', provider_id: 'p-given' }, null), want('#/settings/providers/p-given'), 'params.provider_id: its row');
  // ids that need escaping in a route
  assert.equal(fix('llm.no_api_key', { provider: 'Odd' }, [{ id: 'a/b c', name: 'Odd' }]), want('#/settings/providers/a%2Fb%20c'), 'the id escaped');
  // the providers not read (the lecture page reads none for a key): the
  // stage's snapshot names the provider of that name
  const snapDs = { provider_id: 'p-ds', provider_name: 'DeepSeek', model: 'deepseek-chat' };
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, null, snapDs), want('#/settings/providers/p-ds'), 'not read: the snapshot\'s provider of that name');
  assert.equal(fix('llm.no_api_key', { provider: 'Other' }, null, snapDs), want('#/settings/providers'), 'not read, the snapshot another provider: the section');
  // not known: the providers section, as before
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, null), want('#/settings/providers'), 'the providers not read: the section');
  assert.equal(fix('llm.no_api_key', { provider: 'Gone' }, [ds], { provider_id: 'p-gone', provider_name: 'Gone' }), want('#/settings/providers'), 'read, the provider not among them: the section');
  assert.equal(fix('llm.no_api_key', { provider: 'Gone' }, [ds]), want('#/settings/providers'), 'no such provider: the section');
  assert.equal(fix('llm.no_api_key', {}, [ds]), want('#/settings/providers'), 'no provider named: the section');
  assert.equal(fix('llm.provider_not_configured', { role: 'translate', provider: 'legacy' }, [ds]), want('#/settings/providers'), 'a config.toml provider: the section');
  // two providers of that name: the one the stage's snapshot names, else the section
  const twin = { id: 'p-ds2', name: 'DeepSeek', models: [] };
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, [ds, twin], { provider_id: 'p-ds2' }), want('#/settings/providers/p-ds2'), 'two of that name: the snapshot\'s');
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, [ds, twin], { provider_id: 'p-x' }), want('#/settings/providers'), 'two of that name, the snapshot another: the section');
  assert.equal(fix('llm.no_api_key', { provider: 'DeepSeek' }, [ds, twin]), want('#/settings/providers'), 'two of that name, no snapshot: the section');
  // a model the provider no longer lists: the roles, as before
  assert.equal(
    fix('llm.rejected', { role: 'translate', status: 400, provider: 'DeepSeek', model: 'old-model' }, [ds]),
    want('#/settings/roles', { model: 'old-model', provider: 'DeepSeek' }),
    'unlisted: the roles, as before',
  );
  // the roles block: as before
  assert.equal(fix('llm.no_model', { role: 'translate' }, [ds]), want('#/settings/roles'), 'no model: the roles');
  assert.equal(fix('llm.provider_deleted', { provider: 'DeepSeek' }, [ds]), want('#/settings/roles'), 'provider deleted: the roles');
}

// ---- WH-04: file sizes in the units their names say ----

{
  const U = await import(new URL('util.js', base).href);
  const GiB = 1024 * 1024 * 1024;
  // 1 kB = 1000 bytes (CLDR's kilobyte, the unit Intl writes); below 1000
  // the unit's long name, with its plural in English
  const table = [
    [0, '0 bytes', '0字节'],
    [1, '1 byte', '1字节'],
    [714, '714 bytes', '714字节'],
    [999, '999 bytes', '999字节'],
    [999.6, '1 kB', '1 kB'],
    [1000, '1 kB', '1 kB'],
    [1023, '1 kB', '1 kB'],
    [1024, '1 kB', '1 kB'],
    [82440, '82.4 kB', '82.4 kB'],
    [999949, '999.9 kB', '999.9 kB'],
    [999960, '1 MB', '1 MB'],
    [1500000, '1.5 MB', '1.5 MB'],
    [1.5 * GiB, '1.61 GB', '1.61 GB'],
    [1234567890123, '1,234.57 GB', '1,234.57 GB'],
  ];
  for (const [loc, col] of [['en', 1], ['zh-CN', 2]]) {
    await I.setLocale(loc);
    for (const row of table) assert.equal(U.fmtBytes(row[0]), row[col], `${loc} fmtBytes(${row[0]})`);
    assert.equal(U.fmtBytes(null), row0(loc), `${loc} fmtBytes(null) is 0`);
  }
  await I.setLocale('zh-CN');
  // fmtMB is not changed (it keeps 1024 MB to the GB)
  assert.equal(U.fmtMB(2048), '2 GB', 'fmtMB as before');
  function row0(loc) {
    return loc === 'en' ? '0 bytes' : '0字节';
  }
}

// ---- WH-03: the download menu says what each file is ----

{
  // menu.js: an item's desc is a second line, its description for a
  // screen reader (aria-describedby; not in its name)
  const anchor = new El('button');
  body.append(anchor);
  anchor.focus();
  assert.ok(M.openMenu([{ id: 'a', label: 'Alpha', desc: 'What alpha is', hint: '1 kB', onSelect() {} }, { id: 'b', label: 'Beta', hint: '2 kB', onSelect() {} }], { anchor }, { returnFocus: anchor }), 'the menu opens');
  const [a, b] = doc.querySelectorAll('.ctx-item');
  const desc = a.querySelector('.ctx-desc');
  assert.ok(desc && desc.textContent === 'What alpha is', 'the description is drawn');
  assert.ok(a.classList.contains('has-desc'), 'its item is marked (two lines)');
  const id = desc.getAttribute('id');
  assert.ok(id && a.getAttribute('aria-describedby') === id, 'the item is described by it');
  assert.equal(desc.getAttribute('aria-hidden'), 'true', 'and it is not part of the item\'s name');
  assert.equal(a.querySelector('.ctx-label').textContent, 'Alpha', 'the label as before');
  assert.equal(a.querySelector('.ctx-hint').textContent, '1 kB', 'the hint as before');
  assert.ok(!b.querySelector('.ctx-desc') && !b.hasAttribute('aria-describedby') && b.className === 'ctx-item', 'an item without desc: as before');
  M.closeMenu({ focus: false });
  // two menus, two ids
  M.openMenu([{ label: 'A', desc: 'x' }, { label: 'B', desc: 'y' }], { anchor }, { returnFocus: anchor });
  const ids = doc.querySelectorAll('.ctx-desc').map((d) => d.getAttribute('id'));
  assert.ok(ids.length === 2 && ids[0] !== ids[1] && !ids.includes(id), 'every description has its own id');
  M.closeMenu({ focus: false });
  anchor.remove();

  // the export items: one sentence per kind, what it is and what opens it
  const files = [
    { name: 'W01L1 Intro.bilingual.md', size: 82440, url: '/x/1' },
    { name: 'W01L1 Intro.bilingual.vtt', size: 1000, url: '/x/2' },
    { name: 'W01L1 Intro.zh.vtt', size: 900, url: '/x/3' },
    { name: 'W01L1 Intro.cite.json', size: 5000, url: '/x/4' },
    { name: 'W01L1 Intro.en.txt', size: 700, url: '/x/5' },
    { name: 'W01L1 Intro.other.bin', size: 10, url: '/x/6' },
  ];
  const Ux = await import(new URL('util.js', base).href);
  const items = Act.exportItems(files);
  const byId = (k) => items.find((it) => it && it.id === `export:${k}`);
  for (const [i, kind] of ['md_bilingual', 'vtt_bilingual', 'vtt_zh', 'cite_json', 'txt_en'].entries()) {
    const it = byId(kind);
    assert.ok(it, `${kind}: an item`);
    assert.equal(it.desc, t(`menu.export.${kind}_desc`), `${kind}: its description`);
    assert.ok(it.desc && it.desc !== `menu.export.${kind}_desc`, `${kind}: a text`);
    assert.equal(it.hint, Ux.fmtBytes(files[i].size), `${kind}: the size as before`);
  }
  const otherItem = items.find((it) => it && it.label === 'W01L1 Intro.other.bin');
  assert.ok(otherItem && !otherItem.desc, 'a file of no known kind: no description');
  assert.equal(new Set(['md_bilingual', 'vtt_bilingual', 'vtt_zh', 'cite_json', 'txt_en'].map((k) => byId(k).desc)).size, 5, 'five different sentences');
  // the descriptions for the English on-demand files and the Traditional
  // group exist in both languages
  for (const k of ['vtt_en_desc', 'zh_hant_desc']) assert.ok(t(`menu.export.${k}`) !== `menu.export.${k}`, `${k}: a text`);
}

// ---- launchLectures(lectures, opts): opts.ask after the glossary check
// (handed over by r8-web-lecture: the selection's "start" asked before the
// glossary question; the same order as launchCourseDrafts, F2) ----

{
  const lec = (id) => ({ id, title: `Lecture ${id}`, course_id: 'c3', lecture_actions: ['launch'] });
  const one = [lec('S1')];
  const two = [lec('S1'), lec('S2'), { id: 'S3', title: 'Running', course_id: 'c3', lecture_actions: ['cancel'] }];
  answers = {
    'POST /api/lectures/S1/launch': () => ({ status: 200, body: { ...lec('S1'), lecture_actions: [] } }),
    'POST /api/lectures/launch': (b) => ({ status: 200, body: { results: (b.ids || []).map((id) => ({ id, ok: true, lecture: { id, title: id } })) } }),
  };
  Act.setCourseLookup(() => null);
  takeWrites();
  told();
  // without opts: as before (one: its own request; several: the batch)
  let res = await Act.launchLectures(one);
  assert.deepEqual(takeWrites(), ['POST /api/lectures/S1/launch '], 'one lecture, no opts: launched as before');
  assert.ok(Array.isArray(res) && res.length === 1 && res[0].id === 'S1', 'and returned');
  res = await Act.launchLectures(two);
  assert.deepEqual(takeWrites(), ['POST /api/lectures/launch {"ids":["S1","S2"]}'], 'several, no opts: the batch as before (those that can start)');
  told();
  // ask answers false: [] and nothing sent; it is given the lectures launched
  let asked = [];
  res = await Act.launchLectures(two, { ask: async (list) => (asked.push(list.map((l) => l.id).join(',')), false) });
  assert.deepEqual(asked, ['S1,S2'], 'asked once, with the lectures it would start');
  assert.ok(Array.isArray(res) && res.length === 0, 'a "no" ends with []');
  assert.equal(writes.length, 0, 'a "no" sends nothing');
  assert.deepEqual(told(), [], 'and shows nothing');
  // ask answers true: the same request as without it
  res = await Act.launchLectures(two, { ask: async () => true });
  assert.deepEqual(takeWrites(), ['POST /api/lectures/launch {"ids":["S1","S2"]}'], 'a "yes": the batch as before');
  told();
  // nothing to launch: not asked
  asked = [];
  res = await Act.launchLectures([two[2]], { ask: async () => (asked.push('x'), true) });
  assert.ok(asked.length === 0 && res.length === 0 && writes.length === 0, 'nothing to start: not asked, nothing sent');
  // the glossary question first: declined, ask is not asked; accepted, then ask
  Act.setCourseLookup((id) => (id === 'c3' ? { id: 'c3', code: 'C3', glossary_pending: true } : null));
  asked = [];
  let p = Act.launchLectures(one, { ask: async () => (asked.push('ask'), true) });
  await settle();
  assert.deepEqual(openTitles(), [t('common.glossary_check.title')], 'the glossary question first');
  await cancelDialogs();
  res = await p;
  assert.ok(asked.length === 0 && res.length === 0 && writes.length === 0, 'glossary declined: not asked, nothing sent');
  const before = dialogs.length;
  p = Act.launchLectures(one, { ask: () => UC.confirmLaunchDrafts(one, 1) });
  await settle();
  assert.deepEqual(openTitles(), [t('common.glossary_check.title')], 'the glossary question first');
  await confirmDialogs();
  await settle();
  assert.deepEqual(openTitles(), [t('course.confirm.launch_title', { n: 1 })], 'then the confirmation');
  assert.equal(writes.length, 0);
  await confirmDialogs();
  await p;
  assert.equal(dialogs.length - before, 2, 'two dialogs in all');
  assert.deepEqual(takeWrites(), ['POST /api/lectures/S1/launch '], 'started once');
  Act.setCourseLookup(() => null);
  told();
}

// ---- api.js err.method; tellFailure tells a write's failure while the
// connection is lost, not a read's (handed over with r8-web-dash's WD-09,
// whose app.js rule it matches) ----

{
  told();
  takeWrites();
  // every ApiError of request() carries its method, quiet or not, network
  // failure or HTTP error; the handler hears it too
  const heard = [];
  A.setHandlers({ onError: (e) => heard.push(e.method) });
  net.down = true;
  const e1 = await A.api.updateLecture('Z1').catch((e) => e);
  const e2 = await A.api.course('Z1', { quiet: true }).catch((e) => e);
  net.down = false;
  answers = { 'PATCH /api/courses/Z1': () => ({ status: 500, body: { error: 'boom' } }) };
  const e3 = await A.api.patchCourse('Z1', { name: 'x' }, { quiet: true }).catch((e) => e);
  answers = {};
  net.answer = ((orig) => (m, u, b) => (m === 'GET' && u.startsWith('/api/courses/Z2') ? { status: 404, body: { error: 'none' } } : orig(m, u, b)))(net.answer);
  const e4 = await A.api.course('Z2').catch((e) => e);
  assert.ok(e1 instanceof A.ApiError && e1.status === 0 && e1.method === 'POST', 'a write that could not be sent: POST');
  assert.ok(e2 instanceof A.ApiError && e2.status === 0 && e2.method === 'GET', 'a quiet read that could not be sent: GET');
  assert.ok(e3 instanceof A.ApiError && e3.status === 500 && e3.method === 'PATCH', 'an HTTP error: its method');
  assert.ok(e4 instanceof A.ApiError && e4.status === 404 && e4.method === 'GET', 'a read refused: GET');
  assert.deepEqual(heard, ['POST', 'GET'], 'the handler hears the method of the errors it is told');
  A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });
  told();
  takeWrites();

  // a dialog's request (busy) failing after the dialog was closed, while
  // the "connection lost" banner shows
  const banner = new El('div');
  banner.attrs.id = 'conn-banner';
  banner.hidden = false;
  body.append(banner);
  const failAfterClose = async (err) => {
    let rej;
    const req = new Promise((_, r) => (rej = r));
    let closeIt;
    const done = D.openDialog('t', (close, busy) => {
      closeIt = close;
      busy(req).catch(() => {});
      return new El('div');
    });
    closeIt(null);
    await done;
    rej(err);
    await settle();
  };
  const lost = (method) => Object.assign(new A.ApiError(0, t('common.net.unreachable')), method ? { method } : {});
  for (const m of ['GET', 'HEAD']) {
    await failAfterClose(lost(m));
    assert.deepEqual(told(), [], `a ${m} while the banner shows: nothing (the banner and the page say it)`);
  }
  for (const m of ['POST', 'PATCH', 'PUT', 'DELETE', undefined]) {
    await failAfterClose(lost(m));
    assert.deepEqual(told(), [`warn: ${t('nav.conn_lost_action')}`], `a ${m || 'request of no known method'} while the banner shows: "not done"`);
  }
  // connected: a read's failure is an error toast, as before
  banner.hidden = true;
  await failAfterClose(lost('GET'));
  assert.deepEqual(told(), [`error: ${t('common.net.unreachable')}`], 'connected: as before');
  banner.remove();
}

console.log('r8 shared checks: ok');
