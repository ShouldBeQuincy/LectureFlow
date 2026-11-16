// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 8, the r8-web-lecture line (the lecture and course pages, the
// lecture list, the player's transcript); each section names its item:
//   WL-01  the lecture page's two "更新" and the course page's "启动全部草稿"
//          ask first, once (T25 X1, R74 low 2a);
// (the later sections name their own items).
// The modules run on the DOM stand-in of r7_dom.mjs; web/r8_lecture_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared by identity (assert.ok(a === b)), never with equal / deepEqual.

import { registerHooks } from 'node:module';
import { assert, El, body, doc, base, sent, net, settle, until, fk, told, dialogs, event } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const U = await import(new URL('util.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
const Lec = await import(new URL('view_lecture.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

// ---- what the checks share ----

// What the modules ask of the stand-in beyond r7_dom: a list of selectors
// and :not() (player_transcript.js's arrow keys, menu.js's text fields),
// scrollIntoView.
{
  const baseMatches = El.prototype.matches;
  El.prototype.matches = function (sel) {
    if (sel.includes(',')) return sel.split(',').some((s) => this.matches(s.trim()));
    const nots = [];
    const core = sel.replace(/:not\(([^)]*)\)/g, (_, x) => (nots.push(x), ''));
    if (nots.length) return (core === '' || baseMatches.call(this, core)) && !nots.some((x) => this.matches(x));
    return baseMatches.call(this, sel);
  };
  El.prototype.scrollIntoView = function () {};
}

const openOnes = () => dialogs.filter((d) => d.open && d.isConnected);
const okOf = (d) => d.querySelectorAll('button').find((b) => /btn-primary/.test(b.className));
const cancelOf = (d) => d.querySelectorAll('button').find((b) => b.className === 'btn');
// the requests that change something (not the reads)
const writes = () => sent.filter((s) => !s.startsWith('GET '));
const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, stats: {}, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'C1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [],
    usage: { prompt_tokens: 0, cache_hit_tokens: 0, completion_tokens: 0, estimated_cost: null },
    stages: names.map((n) => stage(n, 'pending')), ...o,
  };
}
const usage = (cost) => ({ prompt_tokens: 1000, cache_hit_tokens: 0, completion_tokens: 100, estimated_cost: cost });
const glossaryReason = { code: 'glossary_updated', params: { from: 0, to: 1 } };
// a lecture whose correction and translation are out of date (the glossary
// changed): "更新" reruns them and the export
function staleLecture(id, o = {}) {
  return lecture(id, {
    lecture_actions: ['update'],
    stale: { reasons: [glossaryReason], stages: ['repair', 'translate'] },
    stages: names.map((n) => (n === 'repair' || n === 'translate' ? stage(n, 'stale', { duration_ms: 60000, stats: usage(0.1) }) : stage(n, 'done', { duration_ms: n === 'export' ? 1000 : 5000 }))),
    ...o,
  });
}

// The lecture page on lec: GET /api/lectures/{id} answers it, extra(m, u)
// the rest it knows (null: an empty 200). → {page, view, done()}
async function mountLecture(lec, { extra = () => null, res = null } = {}) {
  net.answer = (m, u) => {
    const x = extra(m, u);
    if (x) return x;
    if (u.startsWith('/api/settings')) return { status: 200, body: { providers: [], roles: [] } };
    if (u.startsWith(`/api/lectures/${lec.id}/sentences`)) return { status: 200, body: { total: 0, sentences: [] } };
    if (m === 'GET' && u.startsWith(`/api/lectures/${lec.id}`) && !u.includes('/sentences') && !u.includes('/patches')) return { status: 200, body: lec };
    return { status: 200, body: {} };
  };
  const page = new El('main');
  body.append(page);
  const ctx = { system: () => null, resources: () => res, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} };
  const view = Lec.mountLecture(page, { id: lec.id, tab: 'review' }, ctx);
  await until(() => page.querySelector('.lec-stats'), 'the lecture page is drawn');
  return {
    page,
    view,
    done() {
      view.destroy();
      page.remove();
      body.focus();
    },
  };
}

// ---- WL-01: the lecture page's "更新" asks first (T25 X1) ----

{
  // the stale notice's "更新" (the lecture says why it is out of date)
  const updated = staleLecture('U', { lecture_actions: [], stale: null, stages: names.map((n) => stage(n, n === 'repair' ? 'queued' : n === 'translate' || n === 'export' ? 'pending' : 'done')) });
  const m = await mountLecture(staleLecture('U'), { extra: (meth, u) => (meth === 'POST' && u === '/api/lectures/U/update' ? { status: 200, body: updated } : null) });
  const notice = m.page.querySelector('div.lec-notice.is-stale');
  assert.ok(notice, 'the stale notice is shown');
  let b = fk('lecd:U:update');
  assert.ok(b && notice.contains(b), 'its "更新" is in the notice');
  const tip = b.getAttribute('title');
  assert.ok(tip && tip.includes(U.stageName('repair')), 'its hover text names the stages, as before');
  sent.length = 0;
  told();
  const before = openOnes().length;
  b.focus();
  b.click();
  b.click(); // a double click: one dialog
  await settle();
  assert.equal(openOnes().length, before + 1, 'the notice\'s "更新" asks first, once');
  let dlg = openOnes().at(-1);
  assert.deepEqual(writes(), [], 'nothing is sent while it asks');
  const text = dlg.textContent;
  assert.ok(text.includes(t('course.confirm.update_title')), 'one lecture');
  for (const s of ['repair', 'translate', 'export']) assert.ok(text.includes(U.stageName(s)), `it names ${s}`);
  assert.ok(text.includes(U.staleReasonText(glossaryReason)), 'and why');
  assert.ok(text.includes(t('course.confirm.update_replace_keep')), 'that the results are replaced and billed again');
  assert.ok(text.includes(U.usageText({ prompt_tokens: 2000, cache_hit_tokens: 0, completion_tokens: 200, estimated_cost: 0.2 })), 'and what those stages took the last time');
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null, 'the focus starts on "cancel"');
  cancelOf(dlg).click();
  await settle();
  assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  assert.ok(doc.activeElement === b, 'cancelled: the focus is back on "更新"');
  // confirmed: the update as before
  b.click();
  await settle();
  dlg = openOnes().at(-1);
  okOf(dlg).click();
  await until(() => writes().length > 0, 'the update is sent');
  await settle();
  assert.deepEqual(writes(), ['POST /api/lectures/U/update'], 'confirmed: the update is sent as before');
  assert.equal(openOnes().length, before, 'no other dialog');
  assert.ok(!m.page.querySelector('div.lec-notice.is-stale'), 'the page shows the lecture the update answered with');
  m.done();

  // the 409 of a changed model: the model choice after the confirmation, as before
  const m2 = await mountLecture(staleLecture('U'), { extra: (meth, u) => (meth === 'POST' && u === '/api/lectures/U/update' ? { status: 409, body: { error: { code: 'needs_model_choice' }, diff: [{ lecture_id: 'U', lecture_title: 'Lecture U', stage: 'translate', snapshot: { label: 'old' }, current: { label: 'new' }, snapshot_usable: true }] } } : null) });
  sent.length = 0;
  fk('lecd:U:update').click();
  await settle();
  const first = openOnes().at(-1);
  assert.ok(first && first.textContent.includes(t('course.confirm.update_title')), 'first the confirmation');
  okOf(first).click();
  await until(() => openOnes().length === before + 1 && openOnes().at(-1) !== first, 'then the model choice');
  assert.deepEqual(writes(), ['POST /api/lectures/U/update'], 'the update was sent once');
  assert.ok(openOnes().at(-1).textContent.includes(t('common.model_choice.title')), 'the model choice, as before');
  cancelOf(openOnes().at(-1)).click();
  await settle();
  assert.deepEqual(writes(), ['POST /api/lectures/U/update'], 'the model choice cancelled: nothing more');
  m2.done();
  told();

  // the header's "更新" (no stale reasons to show: the button is in the header)
  const plain = staleLecture('H', { stale: null });
  const m3 = await mountLecture(plain, { extra: (meth, u) => (meth === 'POST' && u === '/api/lectures/H/update' ? { status: 200, body: { ...plain, lecture_actions: [], stages: names.map((n) => stage(n, 'done')) } } : null) });
  b = fk('lecd:H:update');
  assert.ok(b && m3.page.querySelector('.page-actions').contains(b), 'the header has "更新"');
  assert.ok(!m3.page.querySelector('div.lec-notice.is-stale'), 'and no stale notice');
  assert.equal(b.getAttribute('title'), t('lecture.page.update_tip_plain'), 'its hover text as before');
  sent.length = 0;
  b.focus();
  b.click();
  await settle();
  dlg = openOnes().at(-1);
  assert.ok(dlg && dlg.textContent.includes(t('course.confirm.update_title')), 'the header\'s "更新" asks first');
  assert.deepEqual(writes(), [], 'nothing sent while it asks');
  cancelOf(dlg).click();
  await settle();
  assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  assert.ok(doc.activeElement === b, 'the focus is back on it');
  b.click();
  await settle();
  okOf(openOnes().at(-1)).click();
  await until(() => writes().length > 0, 'the update is sent');
  await settle();
  assert.deepEqual(writes(), ['POST /api/lectures/H/update'], 'confirmed: sent as before');
  m3.done();
  told();
}

// ---- WL-01 (R74 low 2a): the course page's "启动全部草稿" asks once, after the glossary question ----

const Course = await import(new URL('view_course.js', base).href);

// The course page on course with lectures: GET /api/courses/{id} and its
// lectures answer them, extra(m, u) the rest it knows. → {page, view, done()}
async function mountCourse(course, lectures, { extra = () => null } = {}) {
  net.answer = (m, u) => {
    const x = extra(m, u);
    if (x) return x;
    const path = u.replace(/\?.*$/, '');
    if (m === 'GET' && path === `/api/courses/${course.id}`) return { status: 200, body: course };
    if (m === 'GET' && path === `/api/courses/${course.id}/lectures`) return { status: 200, body: lectures };
    if (m === 'GET') return { status: 404, body: { error: 'not in this stand-in' } };
    return { status: 200, body: {} };
  };
  const page = new El('main');
  body.append(page);
  const ctx = { system: () => null, resources: () => null, courses: () => [course], setCurrentCourse() {}, subscribe: () => () => {} };
  const view = Course.mountCourse(page, { id: course.id }, ctx);
  await until(() => page.querySelector('.lrow'), 'the course page shows its lectures');
  return {
    page,
    view,
    done() {
      view.destroy();
      page.remove();
      body.focus();
    },
  };
}
const courseOf = (o = {}) => ({ id: 'C1', code: 'CSC1', name: 'Course', term_start: '', skip_weeks: [], numbering: 'auto', materials: [], course_actions: ['launch_drafts'], draft_count: 2, stale_lecture_count: 0, lecture_count: 2, glossary_pending: false, ...o });
const draftLecture = (id) => lecture(id, { draft: true, lecture_actions: ['launch'] });

{
  const launched = { results: [{ id: 'D1', ok: true }, { id: 'D2', ok: true }] };
  // the glossary is still being extracted: its question first, then the confirmation, each once
  let course = courseOf({ glossary_pending: true });
  Act.setCourseLookup((id) => (id === 'C1' ? course : null));
  const m = await mountCourse(course, [draftLecture('D1'), draftLecture('D2')], { extra: (meth, u) => (meth === 'POST' && u === '/api/courses/C1/launch-drafts' ? { status: 200, body: launched } : null) });
  const before = openOnes().length;
  let b = fk('course:drafts');
  assert.ok(b, 'the header has "启动全部草稿"');
  sent.length = 0;
  told();
  b.focus();
  b.click();
  await settle();
  assert.equal(openOnes().length, before + 1, 'one dialog at a time');
  let dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('common.glossary_check.title')), 'the glossary question comes first');
  okOf(dlg).click();
  await settle();
  assert.equal(openOnes().length, before + 1, 'then one more dialog');
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('course.confirm.launch_title', { n: 2 })), 'then the confirmation, with how many drafts');
  assert.ok(dlg.textContent.includes('Lecture D1') && dlg.textContent.includes('Lecture D2'), 'naming them');
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null, 'the focus starts on "cancel"');
  assert.deepEqual(writes(), [], 'nothing sent while it asks');
  cancelOf(dlg).click();
  await settle();
  assert.equal(openOnes().length, before, 'no third dialog');
  assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  b = fk('course:drafts');
  assert.ok(doc.activeElement === b, 'the focus is back on "启动全部草稿"');
  // the glossary question declined: no confirmation, nothing sent
  b.click();
  await settle();
  cancelOf(openOnes().at(-1)).click();
  await settle();
  assert.equal(openOnes().length, before, 'the glossary question declined: nothing more asked');
  assert.deepEqual(writes(), [], 'nor sent');
  // both answered yes: the drafts are launched as before
  fk('course:drafts').click();
  await settle();
  okOf(openOnes().at(-1)).click();
  await settle();
  okOf(openOnes().at(-1)).click();
  await until(() => writes().length > 0, 'the launch is sent');
  await settle();
  assert.deepEqual(writes(), ['POST /api/courses/C1/launch-drafts'], 'confirmed: the drafts are launched as before');
  assert.equal(openOnes().length, before, 'no dialog left');
  m.done();
  told();

  // no glossary question: the confirmation alone
  course = courseOf();
  const m2 = await mountCourse(course, [draftLecture('D1'), draftLecture('D2')], { extra: (meth, u) => (meth === 'POST' && u === '/api/courses/C1/launch-drafts' ? { status: 200, body: launched } : null) });
  sent.length = 0;
  fk('course:drafts').click();
  await settle();
  assert.equal(openOnes().length, before + 1);
  dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('course.confirm.launch_title', { n: 2 })), 'the confirmation');
  okOf(dlg).click();
  await until(() => writes().length > 0, 'the launch is sent');
  await settle();
  assert.equal(openOnes().length, before, 'no other dialog');
  assert.deepEqual(writes(), ['POST /api/courses/C1/launch-drafts']);
  m2.done();
  told();
  Act.setCourseLookup(() => null);
}

// ---- WL-04 (R82 C-I2): no inline style that style.css already gives ----

{
  const inline = (el) => Object.keys(el.style).filter((k) => el.style[k] !== '' && el.style[k] != null);
  // a waiting stage's notice (the daily budget): wraps by .lec-notice.is-wait
  const waiting = lecture('W', { stages: names.map((n) => (n === 'summary' ? stage(n, 'queued', { waiting_for: 'budget' }) : n === 'prep' || n === 'asr' ? stage(n, 'done') : stage(n, 'pending'))) });
  const m = await mountLecture(waiting);
  const notice = m.page.querySelector('div.lec-notice.is-wait');
  assert.ok(notice, 'the budget wait has its notice');
  assert.deepEqual(inline(notice), [], 'the waiting notice: no inline style (.lec-notice.is-wait)');
  assert.deepEqual(inline(notice.querySelector('.lec-notice-text')), [], 'its text: none (.lec-notice.is-wait > .lec-notice-text)');
  assert.deepEqual(inline(notice.querySelector('a.btn')), [], 'its link: none (.lec-notice.is-wait > .btn)');
  // the large bar's share: .pipe-lg .pipe-pct
  const pct = m.page.querySelector('.pipe-lg').querySelector('.pipe-pct');
  assert.ok(pct, 'the large bar has its share');
  assert.deepEqual(inline(pct), [], 'the large bar\'s share: no inline style (.pipe-lg .pipe-pct)');
  m.done();
  // a stage card's folded server text: .card-errdetail
  const P = await import(new URL('pipeline.js', base).href);
  const failedSt = stage('translate', 'failed', { error: 'HTTP 500 from the server, in its own words', error_info: { code: 'llm.server_error', params: { role: 'translate', status: 500 } }, allowed_actions: ['retry'] });
  const bar = P.pipelineBar([stage('prep', 'done'), failedSt], { key: 'wl04', onAction: async () => null });
  const det = bar.querySelector('details.card-errdetail');
  assert.ok(det, 'the server\'s text is folded under "技术详情"');
  assert.deepEqual(inline(det), [], 'the folded detail: no inline margin (.card-errdetail)');
}

// The toasts' 4-second timers are held while fn runs and fired by its
// fade(); other timers stay real.
async function withFading(fn) {
  const realSet = globalThis.setTimeout;
  const realClear = globalThis.clearTimeout;
  const fading = new Map();
  let nextId = 1;
  globalThis.setTimeout = (f, ms, ...args) => {
    if (ms !== 4000) return realSet(f, ms, ...args);
    const id = `fade-${nextId++}`;
    fading.set(id, f);
    return id;
  };
  globalThis.clearTimeout = (id) => (typeof id === 'string' && id.startsWith('fade-') ? fading.delete(id) : realClear(id));
  const fade = () => {
    const fns = [...fading.values()];
    fading.clear();
    for (const f of fns) f();
  };
  try {
    await fn(fade, fading);
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
  }
}
const toastsEl = doc.getElementById('toasts');
const toastOf = (msg) => toastsEl.children.find((el) => el.dataset.msg === msg) || null;

// ---- WL-05 (C158c): "the same translation came back" is dom.js's sticky toast ----

{
  const RW = await import(new URL('retranslate_watch.js', base).href);
  const same = t('transcript.retranslate.same');
  const sentence = (idx, o = {}) => ({ idx, text: `Sentence ${idx}.`, zh: `句子 ${idx}。`, zh_status: 'ok', ...o });
  // "translate again" of s, read waiting, then read back with the same translation
  const sameAgain = (w, s) => {
    w.start(s);
    w.seen({ ...s, zh_status: 'stale' });
    return w.seen({ ...s });
  };
  told();
  await withFading(async (fade, fading) => {
    const w = RW.retranslateWatch();
    assert.equal(sameAgain(w, sentence(1)), true, 'told');
    const el = toastOf(same);
    assert.ok(el, 'the note is shown');
    assert.ok(el.className === 'toast toast-info' && el.getAttribute('role') === 'status', 'an info toast, read out politely');
    assert.equal(el._sticky, true, 'drawn by dom.js as a sticky toast');
    assert.equal(fading.size, 0, 'no 4-second timer');
    fade();
    assert.ok(toastOf(same) === el, 'still there after 4 seconds');
    // the same note again (another sentence): one toast, counted as dom.js counts
    assert.equal(sameAgain(w, sentence(2)), true);
    assert.equal(toastsEl.children.filter((x) => x.dataset.msg === same).length, 1, 'shown once');
    assert.equal(el.querySelector('.toast-count').textContent, '×2', 'merged, with dom.js\'s count');
    fade();
    assert.ok(toastOf(same) === el, 'still sticky after the merge');
    el.querySelector('.toast-close').click();
    assert.equal(toastOf(same), null, 'closed by its button');
  });
  told();
}

// ---- WL-06 (R78c): "用当前设置重翻此句" says the truth while the translation is held ----

{
  const RC = await import(new URL('retranslate_current.js', base).href);
  const withTranslate = (status) => lecture('R', { stages: names.map((n) => (n === 'translate' ? stage(n, status) : n === 'export' ? stage(n, 'pending') : stage(n, 'done'))) });
  let mixed = false;
  net.answer = (m, u) => (m === 'POST' && u === '/api/lectures/R/sentences/3/retranslate' ? { status: 200, body: { idx: 3, zh_status: 'stale', mixed } } : { status: 200, body: {} });
  await settle();
  told();
  for (const status of ['stale', 'failed', 'cancelled']) {
    for (const mx of [false, true]) {
      mixed = mx;
      const res = await RC.retranslateCurrent('R', 3, withTranslate(status));
      assert.ok(res && res.idx === 3, `${status}: the sentence the server answered with`);
      // round 9 WV-01: an info toast that stays (r9_views_lecture_test.mjs)
      assert.deepEqual(told(), [`info: ${t('player.menu.retranslate_held')}`], `${status}${mx ? ' (mixed)' : ''}: marked, translated when the translation runs again`);
    }
  }
  // not held (done; running or queued: the request waits for that run): as before
  for (const status of ['done', 'running', 'queued']) {
    mixed = false;
    await RC.retranslateCurrent('R', 3, withTranslate(status));
    assert.deepEqual(told(), [`success: ${t('player.menu.retranslate_done')}`], `${status}: as before`);
    mixed = true;
    await RC.retranslateCurrent('R', 3, withTranslate(status));
    assert.deepEqual(told(), [`success: ${t('player.menu.retranslate_current_mixed')}`], `${status}: the mixed models said, as before`);
  }
  // a refusal: its reason, as before
  net.answer = () => ({ status: 409, body: { error: { code: 'snapshot.unusable.no_key', params: {} } } });
  assert.equal(await RC.retranslateCurrent('R', 3, withTranslate('stale')), null);
  const refused = told();
  assert.ok(refused.length === 1 && refused[0].startsWith('warn: '), 'refused: one warning, as before');

  // the transcript tab passes its lecture (the player passes S.lec: pinned in r8_lecture_test.go)
  const T = await import(new URL('view_transcript.js', base).href);
  const rows = [{ idx: 0, start_ms: 0, end_ms: 900, text: 'Sentence 0.', asr_text: 'Sentence 0.', human_text: null, zh: '', zh_status: 'qa_failed', patch: null, low_conf_words: [], qa_error: null }];
  net.answer = (m, u) => {
    if (m === 'GET' && u.includes('/api/lectures/R/sentences')) return { status: 200, body: { total: rows.length, sentences: rows } };
    if (m === 'POST' && u === '/api/lectures/R/sentences/0/retranslate') return { status: 200, body: { ...rows[0], zh_status: 'stale', mixed: false } };
    return { status: 200, body: {} };
  };
  for (const [status, want] of [['failed', 'player.menu.retranslate_held'], ['done', 'player.menu.retranslate_done']]) {
    const host = D.h('div');
    body.append(host);
    const view = T.mountTranscript(host, { ...withTranslate(status), reviewable: true, counts: { sentences: 1, patches_pending: 0 } }, { courses: () => [], system: () => null, resources: () => null });
    await until(() => fk('more:0'), 'the transcript shows its sentence');
    told();
    fk('more:0').click();
    await settle(10);
    const item = doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(t('player.menu.retranslate_current')));
    assert.ok(item, 'the sentence menu has "用当前设置重翻此句"');
    item.click();
    await until(() => toastsEl.children.length > 0, 'the result is told');
    assert.deepEqual(told(), [`${status === 'failed' ? 'info' : 'success'}: ${t(want)}`], `the transcript tab, translation ${status} (round 9 WV-01: held, an info toast)`);
    view.destroy();
    host.remove();
  }
}

// ---- WL-13 (I64): "约还需 N 小时 M 分钟" can change its words with the hours ----

{
  const P = await import(new URL('pipeline.js', base).href);
  // a catalog that writes common.pipe.eta_hm as a plural object chosen by
  // the hours (Hindi: one for 0 and 1, other from 2)
  const realFetch = globalThis.fetch;
  const forms = { one: '{h} घंटा {m} मिनट', other: '{h} घंटे {m} मिनट' };
  globalThis.fetch = async (url, init) => {
    if (String(url) === 'i18n/hi.json') {
      const cat = { 'common.pipe.eta_hm': forms };
      return { ok: true, status: 200, json: async () => cat, text: async () => JSON.stringify(cat) };
    }
    return realFetch(url, init);
  };
  try {
    await I.setLocale('hi');
    assert.equal(P.etaText(61 * 60000), '1 घंटा 5 मिनट', 'one hour: the form for one');
    assert.equal(P.etaText(125 * 60000), '2 घंटे 5 मिनट', 'two hours: the other form');
  } finally {
    globalThis.fetch = realFetch;
    await I.setLocale('zh-CN');
  }
  // the catalogs as they are: the same texts as before
  assert.equal(P.etaText(61 * 60000), t('common.pipe.eta_hm', { h: 1, m: 5 }));
  assert.equal(P.etaText(3 * 3600000 + 5 * 60000), t('common.pipe.eta_hm', { h: 3, m: 5 }));
}

// ---- WL-14 (C159): the "transcribe only" notice's "添加服务" opens the presets ----

{
  // the API stages wait for a model provider (3.1.0 "transcribe only")
  const lec = lecture('P', { playable: true, stages: names.map((n) => (n === 'prep' || n === 'asr' ? stage(n, 'done') : n === 'export' ? stage(n, 'pending') : stage(n, 'queued', { waiting_for: 'no_provider' }))) });
  const m = await mountLecture(lec);
  const link = fk('lecd:P:providers');
  assert.ok(link && link.tagName === 'A', 'the notice links to the providers');
  assert.ok(link.textContent.includes(t('lecture.notice.no_provider_settings')), 'with its label');
  assert.equal(link.getAttribute('href'), '#/settings/providers/preset', '"用预设添加服务" (route F4; without it the address still lands on the providers)');
  // its focus key is the one it had (a redraw finds it again)
  link.focus();
  m.view.onChanged({ kind: 'lecture', id: 'P' });
  await settle(400);
  const again = fk('lecd:P:providers');
  assert.ok(again && doc.activeElement === again, 'a redraw keeps the focus on it');
  m.done();
}

// ---- WL-02 (P2 hotspot 3): the course list builds a stage card when it opens ----

{
  const L = await import(new URL('lecture_list.js', base).href);
  const P = await import(new URL('pipeline.js', base).href);
  const failedSt = (o = {}) => stage('translate', 'failed', { error: 'boom', error_info: { code: 'llm.server_error', params: { role: 'translate', status: 500 } }, allowed_actions: ['retry'], ...o });
  // 40 lectures: every tenth failed (an error), running, queued with a reason; the rest done
  const lec40 = (i) => {
    const kind = i % 10;
    const stages = names.map((n, j) => {
      if (kind === 0) return j < 4 ? stage(n, 'done') : j === 4 ? failedSt() : stage(n, 'pending');
      if (kind === 1) return j < 2 ? stage(n, 'done') : j === 2 ? stage(n, 'running', { progress: 0.4 }) : stage(n, 'pending');
      if (kind === 2) return j < 1 ? stage(n, 'done') : j === 1 ? stage(n, 'queued', { waiting_for: 'gpu_busy' }) : stage(n, 'pending');
      return stage(n, 'done', { duration_ms: 1000 });
    });
    return lecture(`K${i}`, { title: `Lecture K${i}`, seq: i, playable: true, counts: { sentences: 100, patches_pending: 0 }, stages });
  };
  const all = Array.from({ length: 40 }, (_, i) => lec40(i));
  const ctx = { system: () => null, resources: () => null };
  const list = L.createLectureList({ ctx, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => false });
  body.append(list.table);
  list.render(all);
  // the cards built at once: the stages their card describes (an error, a waiting reason)
  const described = all.flatMap((l) => l.stages).filter((s) => s.error || s.waiting_for).length;
  assert.equal(described, 8, 'four failed and four waiting stages');
  assert.equal(list.table.querySelectorAll('.seg-card-body').length, described, 'only the cards that describe their segment are built with the list');
  // every segment's card is there (aria-controls points at it), empty until it opens
  const segs = list.table.querySelectorAll('button.seg');
  assert.equal(segs.length, 240);
  for (const b of segs) {
    const card = doc.getElementById(b.getAttribute('aria-controls'));
    assert.ok(card && card.classList.contains('seg-card'), 'each segment controls its card, which exists');
  }
  const failedSeg = fk('lec:K0:seg:translate');
  assert.ok(failedSeg.getAttribute('aria-describedby'), 'a failed segment is described by its card at once (R19)');
  assert.ok(fk('lec:K2:seg:asr').getAttribute('aria-describedby'), 'so is a waiting one');
  // opened (hover, focus): the same content as a card built at once
  const sameAsEager = (lec, name) => {
    const wrap = list.table.querySelector(`div.lrow[data-id="${lec.id}"]`).querySelector(`div.seg-wrap[data-stage="${name}"]`);
    const card = wrap.querySelector('.seg-card');
    assert.equal(card.querySelectorAll('.seg-card-body').length, 0, `${lec.id} ${name}: not built before it opens`);
    return { wrap, card, eager: P.pipelineBar(lec.stages, { key: 'eager', lecture: lec.id, sys: ctx.system, res: ctx.resources, onAction: async () => null }).querySelector(`div.seg-wrap[data-stage="${name}"]`).querySelector('.seg-card') };
  };
  let x = sameAsEager(all[5], 'translate');
  x.wrap.dispatchEvent(event('pointerenter', { bubbles: false }));
  assert.equal(x.card.querySelectorAll('.seg-card-body').length, 1, 'hovered: built');
  assert.equal(x.card.textContent, x.eager.textContent, 'hovered: the same content as a card built at once');
  x.wrap.dispatchEvent(event('pointerleave', { bubbles: false }));
  x = sameAsEager(all[1], 'summary');
  x.wrap.querySelector('button.seg').focus();
  x.wrap.dispatchEvent(event('focusin', { relatedTarget: null }));
  assert.equal(x.card.querySelectorAll('.seg-card-body').length, 1, 'focused: built');
  assert.equal(x.card.textContent.replace(/\d+ 秒/g, ''), x.eager.textContent.replace(/\d+ 秒/g, ''), 'focused: the same content (a running stage)');
  body.focus();
  // one lecture changed (its row alone redrawn, updateRow): its cards still wait to be opened
  const changed = { ...all[7], stages: all[7].stages.map((s) => (s.stage === 'export' ? stage('export', 'running', { progress: 0.5 }) : s)) };
  all[7] = changed;
  assert.equal(list.updateRow(changed), true, 'the row is redrawn in place');
  const row = list.table.querySelector('div.lrow[data-id="K7"]');
  assert.equal(row.querySelectorAll('.seg-card-body').length, 0, 'the redrawn row builds its cards when they open');
  assert.equal(row.querySelectorAll('.seg-card').length, 6, 'its six cards are there');
  list.table.remove();
}

// ---- WL-03 (T22 G15): the player's sentences have a visible "⋯" ----

{
  const PT = await import(new URL('player_transcript.js', base).href);
  const M = await import(new URL('menu.js', base).href);
  const sentence = (idx, o = {}) => ({ idx, start_ms: idx * 2000, end_ms: idx * 2000 + 1800, text: `Sentence ${idx}.`, asr_text: `Sentence ${idx}.`, human_text: null, zh: `句子 ${idx}。`, zh_status: 'ok', patch: null, ...o });
  let sentences = [sentence(0), sentence(1), sentence(2, { patch: { review: 'pending', reason: 'r' } })];
  const seeks = [];
  const menuFor = (s) => [
    { id: 'edit', label: `修改原文 ${s.idx}`, onSelect() {} },
    { id: 'retranslate', label: `重翻此句 ${s.idx}`, onSelect() {} },
  ];
  const tr = PT.createTranscript({ onSeek: (s) => seeks.push(s.idx), menuItems: menuFor, duration: () => 600000 });
  body.append(tr.el);
  tr.el.querySelector('input').value = ''; // the search field (the stand-in's has no value of its own)
  tr.setSentences(sentences);
  tr.setCurrent(1);
  const rowOf = (i) => tr.el.querySelector(`div.pl-row[data-idx="${i}"]`);
  const moreOf = (i) => rowOf(i).querySelector('button.pl-row-more');
  const menuLabels = () => doc.querySelectorAll('.ctx-item').map((b) => b.textContent.trim());
  for (let i = 0; i < 3; i++) {
    const b = moreOf(i);
    assert.ok(b, `sentence ${i} has its "⋯"`);
    assert.ok(b.classList.contains('s-more-btn'), 'drawn as the transcript tab\'s (.s-more-btn)');
    assert.equal(b.getAttribute('tabindex'), '-1', 'not a Tab stop of its own (the list is one, roving)');
    assert.equal(b.getAttribute('aria-haspopup'), 'menu');
    assert.equal(b.getAttribute('aria-label'), t('transcript.more_aria', { n: i + 1 }), 'named for its sentence');
    assert.ok(b.dataset.fkey === `pl:more:${i}`, 'a focus key (a redraw while its menu is open finds it again)');
  }
  assert.equal(tr.el.querySelectorAll('[tabindex="0"]').length, 1, 'the list is still one Tab stop');
  // "⋯" opens the same menu as the right click / Shift+F10, and does not seek
  rowOf(1).focus();
  rowOf(1).dispatchEvent(event('keydown', { key: 'F10', shiftKey: true }));
  await settle(10);
  const byKey = menuLabels();
  assert.deepEqual(byKey, ['修改原文 1', '重翻此句 1'], 'Shift+F10: the sentence\'s menu');
  M.closeMenu({ focus: false });
  moreOf(1).focus();
  moreOf(1).click();
  await settle(10);
  assert.deepEqual(menuLabels(), byKey, '"⋯": the same menu');
  assert.deepEqual(seeks, [], '"⋯" does not jump to the sentence');
  M.closeMenu({ focus: false });
  // Enter on a focused "⋯" opens its menu (the button's own click), not the sentence's playback
  moreOf(0).focus();
  const enter = event('keydown', { key: 'Enter' });
  moreOf(0).dispatchEvent(enter);
  assert.ok(!enter.defaultPrevented, 'Enter on "⋯" is left to the button');
  assert.deepEqual(seeks, [], 'and does not play the sentence');
  // ↓ on "⋯" opens its menu; the focus is not moved to the next sentence
  moreOf(0).dispatchEvent(event('keydown', { key: 'ArrowDown' }));
  await settle(10);
  assert.deepEqual(menuLabels(), ['修改原文 0', '重翻此句 0'], '↓ on "⋯": its menu');
  assert.ok(doc.activeElement !== rowOf(1), 'the focus stays out of the next row');
  M.closeMenu({ focus: false });
  // the rows' own keys as before: ↓ moves, Enter plays
  rowOf(0).focus();
  rowOf(0).dispatchEvent(event('keydown', { key: 'ArrowDown' }));
  assert.ok(doc.activeElement === rowOf(1), '↓ on a sentence: the next one');
  rowOf(1).dispatchEvent(event('keydown', { key: 'Enter' }));
  assert.deepEqual(seeks, [1], 'Enter on a sentence plays it');
  rowOf(2).click();
  assert.deepEqual(seeks, [1, 2], 'a click on a sentence plays it');
  // the current sentence keeps its mark; a sentence redrawn in place keeps its "⋯"
  assert.equal(rowOf(1).getAttribute('aria-current'), 'true');
  tr.updateSentence(sentence(1, { text: 'Edited.', human_text: 'Edited.' }));
  assert.ok(moreOf(1), 'redrawn (an edit came back): its "⋯" is there');
  assert.equal(rowOf(1).getAttribute('aria-current'), 'true', 'still the current sentence');
  sentences = sentences.map((s) => (s.idx === 0 ? { ...s, zh: '新译文。' } : s));
  tr.setSentences(sentences);
  assert.ok(moreOf(0), 'redrawn by a refresh: its "⋯" is there');
  tr.el.remove();
  body.focus();
}

// ---- WL-07 (R72 L4, R79b (d)): the lecture page's read of the providers ----

{
  // a translation that failed with a 404 for a model its provider may no
  // longer list: the page asks the settings (U.mayBeUnlisted)
  const llmInfo = (code, params = {}) => ({ code, params: { role: 'translate', retries: 0, provider: 'DeepSeek', model: 'deepseek-chat', ...params } });
  const st = stage('translate', 'failed', { error: 'raw', error_info: llmInfo('llm.request_failed', { status: 404 }), model: { provider_id: 'deepseek', model: 'deepseek-chat' }, allowed_actions: ['retry'] });
  const lec = lecture('V', { stages: names.map((n, j) => (j < 4 ? stage(n, 'done') : n === 'translate' ? st : stage(n, 'pending'))) });
  const providers = [{ id: 'deepseek', name: 'DeepSeek', models: ['deepseek-flash'] }];
  const unlisted = (page) => {
    const n = page.querySelector('div.lec-notice.is-failed');
    return !!n && !!n.querySelector('.lec-notice-hint');
  };
  const settingsReads = () => sent.filter((s) => s.startsWith('GET /api/settings')).length;
  const { clock } = await import('./r7_dom.mjs');

  // a failed read: drawn without the hint; read again when needed later, not at once
  let settingsAnswer = () => ({ status: 500, body: { error: 'boom' } });
  sent.length = 0;
  const m = await mountLecture(lec, { extra: (meth, u) => (u.startsWith('/api/settings') ? settingsAnswer() : null) });
  await settle();
  assert.ok(m.page.querySelector('div.lec-notice.is-failed'), 'the failure is drawn');
  assert.ok(!unlisted(m.page), 'the settings could not be read: no "may no longer be listed"');
  assert.equal(settingsReads(), 1, 'read once');
  settingsAnswer = () => ({ status: 200, body: { providers, roles: [] } });
  m.view.onChanged({ kind: 'lecture', id: 'V' });
  await settle(400);
  assert.equal(settingsReads(), 1, 'the next draw soon after: not read again (no read on every draw)');
  clock.now += 31000;
  m.view.onChanged({ kind: 'lecture', id: 'V' });
  await until(() => unlisted(m.page), 'read again when needed later, and the hint drawn');
  assert.equal(settingsReads(), 2, 'read a second time');
  // read: kept for the next draws
  m.view.onChanged({ kind: 'lecture', id: 'V' });
  await settle(400);
  assert.equal(settingsReads(), 2, 'a read that worked is kept');
  // the settings changed elsewhere (a probe found the model again): read again, the hint goes
  providers[0].models = ['deepseek-flash', 'deepseek-chat'];
  m.view.onChanged({ kind: 'settings', id: '' });
  await until(() => !unlisted(m.page), 'the providers read again after "changed settings"');
  assert.equal(settingsReads(), 3);
  providers[0].models = ['deepseek-flash'];
  m.done();
  await settle();
  told();

  // a read that hangs: the page is drawn after about 3 seconds without
  // the hint, and once more when the read comes back
  let release;
  const gate = new Promise((r) => (release = r));
  sent.length = 0;
  const page = new El('main');
  body.append(page);
  net.answer = (meth, u) => {
    if (u.startsWith('/api/settings')) return gate.then(() => ({ status: 200, body: { providers, roles: [] } }));
    if (meth === 'GET' && u.startsWith('/api/lectures/V') && !u.includes('/sentences') && !u.includes('/patches')) return { status: 200, body: lec };
    return { status: 200, body: {} };
  };
  const view = Lec.mountLecture(page, { id: 'V', tab: 'review' }, { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} });
  await settle(1500);
  assert.equal(page.querySelector('.lec-stats'), null, 'waits for the settings a while (R72 L3: drawn once with what they tell)');
  await until(() => page.querySelector('.lec-stats'), 'drawn after about 3 seconds', 300);
  const drawn = page.querySelector('div.lec-notice.is-failed');
  assert.ok(drawn && !unlisted(page), 'drawn without knowing: no hint yet');
  release();
  await until(() => unlisted(page), 'the read came back: drawn again with the hint');
  const again = page.querySelector('div.lec-notice.is-failed');
  await settle(200);
  assert.ok(page.querySelector('div.lec-notice.is-failed') === again, 'drawn once more, not again and again');
  assert.equal(settingsReads(), 1, 'one read');
  view.destroy();
  page.remove();
  body.focus();
  told();
}

// ---- WL-08 (R72 L6): the stats' explanations reach the keyboard and screen readers ----

{
  const latest = { prompt_tokens: 70000, cache_hit_tokens: 35000, completion_tokens: 9000, estimated_cost: 0.08 };
  const done = (o = {}) => lecture('S', { counts: { sentences: 380, zh_ok: 380, patches_pending: 0 }, usage: latest, stages: names.map((n) => stage(n, 'done')), ...o });
  const statOf = (page, label) => page.querySelectorAll('.lec-stat').find((s) => s.querySelector('.lec-stat-k').textContent.includes(label));
  // a stat with an explanation: focusable, described by it (a visually
  // hidden copy), named by its label and value; the hover text as before
  const explained = (el, text, what) => {
    assert.ok(el, `${what}: the stat is there`);
    assert.equal(el.getAttribute('tabindex'), '0', `${what}: it can be focused`);
    const ids = (el.getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
    assert.ok(ids.length === 1, `${what}: described`);
    const d = doc.getElementById(ids[0]);
    assert.ok(d && el.contains(d) && d.textContent === text && d.className === 'visually-hidden', `${what}: by its explanation`);
    assert.equal(el.getAttribute('title'), text, `${what}: the mouse keeps it on hover`);
    const names = (el.getAttribute('aria-labelledby') || '').split(' ').filter(Boolean).map((id) => doc.getElementById(id));
    assert.ok(el.getAttribute('role') === 'group' && names.length >= 2 && names.every((n) => n && el.contains(n)), `${what}: named by its label and value`);
    assert.ok(el.dataset.fkey, `${what}: a focus key (a redraw keeps the focus)`);
  };
  // the total over every run at least the latest: shown, with the explanation
  const total = { prompt_tokens: 150000, cache_hit_tokens: 40000, completion_tokens: 20000, estimated_cost: 0.235 };
  let m = await mountLecture(done({ usage_total: total }));
  explained(statOf(m.page, t('lecture.stat.tokens')), t('lecture.stat.usage_tip'), 'tokens');
  explained(statOf(m.page, t('lecture.stat.cost')), t('lecture.stat.usage_tip'), 'cost');
  explained(statOf(m.page, t('lecture.stat.cache')), t('lecture.stat.cache_tip'), 'cache hits');
  assert.ok(statOf(m.page, t('lecture.stat.tokens')).textContent.includes(t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(total.prompt_tokens), output: U.fmtTokens(total.completion_tokens) })), 'the total is shown');
  // the stats without one stay as they were (not focusable)
  assert.equal(statOf(m.page, t('lecture.stat.sentences')).getAttribute('tabindex'), null, 'a stat without an explanation is no Tab stop');
  m.done();
  // a total below the latest runs added up (the back end says it never is): not shown
  m = await mountLecture(done({ usage_total: { prompt_tokens: 50000, cache_hit_tokens: 0, completion_tokens: 5000, estimated_cost: 0.05 } }));
  const tokens = statOf(m.page, t('lecture.stat.tokens'));
  assert.ok(tokens.textContent.includes(t('lecture.stat.tokens_sub')), 'a total below the latest: the latest alone');
  assert.ok(!tokens.textContent.includes(U.fmtTokens(50000)), 'the total is not shown');
  assert.ok(statOf(m.page, t('lecture.stat.cost')).textContent.includes(t('lecture.stat.cost_sub')), 'nor its cost');
  assert.ok(tokens.getAttribute('title') == null && tokens.getAttribute('tabindex') == null, 'nor the explanation of a total');
  explained(statOf(m.page, t('lecture.stat.cache')), t('lecture.stat.cache_tip'), 'cache hits (always explained)');
  m.done();
  // only the cost below (the tokens not): not shown either
  m = await mountLecture(done({ usage_total: { ...total, estimated_cost: 0.05 } }));
  assert.ok(statOf(m.page, t('lecture.stat.cost')).textContent.includes(t('lecture.stat.cost_sub')), 'a cost total below the latest: not shown');
  m.done();
}

// ---- WL-10 (R79 L8): the bar's Tab stop counts warnings, and stays where the focus was ----

{
  const P = await import(new URL('pipeline.js', base).href);
  const warned = (n) => stage(n, 'done', { error: 'some sentences kept their English', warning_info: { code: 'translate.partial' } });
  // a done stage with a warning comes before the plain done ones …
  assert.equal(P.tabStopIndex(names.map((n) => (n === 'repair' ? warned(n) : stage(n, 'done')))), 3, 'a warning: its stage');
  // … after what matters now (failed, running, cancelled, out of date, queued)
  for (const st of ['failed', 'running', 'cancelled', 'stale', 'queued']) {
    const stages = names.map((n) => (n === 'repair' ? warned(n) : n === 'translate' ? stage(n, st) : stage(n, n === 'export' ? 'pending' : 'done')));
    assert.equal(P.tabStopIndex(stages), 4, `${st} before a warning`);
  }
  assert.equal(P.tabStopIndex(names.map((n) => stage(n, 'done'))), 0, 'all done, no warning: the first');
  const stops = (bar) => bar.querySelectorAll('button.seg').map((b) => b.getAttribute('tabindex'));
  let bar = P.pipelineBar(names.map((n) => (n === 'translate' ? warned(n) : stage(n, 'done'))), { key: 'wl10:A' });
  assert.deepEqual(stops(bar), ['-1', '-1', '-1', '-1', '0', '-1'], 'drawn with the Tab stop on the warning');
  // the third segment focused, then the bar drawn again (a read of the lecture): still the third
  body.append(bar);
  const third = bar.querySelectorAll('button.seg')[2];
  third.focus();
  third.dispatchEvent(event('focusin', { relatedTarget: null }));
  bar.remove();
  body.focus();
  bar = P.pipelineBar(names.map((n) => (n === 'translate' ? warned(n) : stage(n, 'done'))), { key: 'wl10:A' });
  assert.deepEqual(stops(bar), ['-1', '-1', '0', '-1', '-1', '-1'], 'redrawn: the segment focused last keeps the Tab stop');
  // moved by the arrow keys: the same
  body.append(bar);
  const segs = bar.querySelectorAll('button.seg');
  segs[2].focus();
  segs[2].dispatchEvent(event('focusin', { relatedTarget: null }));
  segs[2].dispatchEvent(event('keydown', { key: 'ArrowRight' }));
  segs[3].dispatchEvent(event('focusin', { relatedTarget: segs[2] }));
  bar.remove();
  body.focus();
  assert.deepEqual(stops(P.pipelineBar(names.map((n) => stage(n, 'done')), { key: 'wl10:A' })), ['-1', '-1', '-1', '0', '-1', '-1'], 'after → : the fourth');
  // another lecture's bar: its own first Tab stop
  assert.deepEqual(stops(P.pipelineBar(names.map((n) => (n === 'translate' ? warned(n) : stage(n, 'done'))), { key: 'wl10:B' })), ['-1', '-1', '-1', '-1', '0', '-1'], 'another bar (lecture) is not affected');
}

// ---- WL-11 (R74 low 9): the gaps of the tests (the lecture list, the
// retranslation note, the course page's header) ----

{
  const L = await import(new URL('lecture_list.js', base).href);
  const M = await import(new URL('menu.js', base).href);
  // a lecture row's own menu (right click, not a selection): "更新过期阶段" asks first
  const list = L.createLectureList({ ctx: { system: () => null, resources: () => null }, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => false });
  body.append(list.table);
  list.render([staleLecture('U'), lecture('P')]);
  net.answer = (meth, u) => (meth === 'POST' && u === '/api/lectures/U/update' ? { status: 200, body: { ...staleLecture('U'), lecture_actions: [], stale: null } } : { status: 200, body: {} });
  const pickUpdate = async () => {
    const row = list.table.querySelector('div.lrow[data-id="U"]');
    row.dispatchEvent(event('contextmenu', { clientX: 10, clientY: 10, button: 2 }));
    await settle(10);
    const item = doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(t('menu.lecture.update')));
    assert.ok(item, 'the row\'s menu has "更新过期阶段"');
    item.click();
    await settle();
  };
  const before = openOnes().length;
  sent.length = 0;
  await pickUpdate();
  assert.equal(openOnes().length, before + 1, 'the row menu\'s "update" asks first');
  let dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('course.confirm.update_title')) && dlg.textContent.includes(U.stageName('repair')), 'with the update\'s stages');
  assert.deepEqual(writes(), [], 'nothing sent while it asks');
  cancelOf(dlg).click();
  await settle();
  assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  await pickUpdate();
  okOf(openOnes().at(-1)).click();
  await until(() => writes().length > 0, 'the update is sent');
  assert.deepEqual(writes(), ['POST /api/lectures/U/update'], 'confirmed: sent once');
  M.closeMenu({ focus: false });
  list.table.remove();
  body.focus();
  told();

  // the retranslation note compares only after a read showed the sentence waiting (stale)
  const RW = await import(new URL('retranslate_watch.js', base).href);
  const w = RW.retranslateWatch();
  const s = { idx: 4, zh: '旧译文。', zh_status: 'ok' };
  w.start(s);
  assert.equal(w.seen({ ...s }), false, 'a read from before the retranslation (still "ok", the old text): not the result');
  assert.equal(toastsEl.children.length, 0, 'nothing said');
  assert.equal(w.seen({ ...s, zh_status: 'stale' }), false, 'waiting');
  assert.equal(w.seen({ ...s }), true, 'then the result: the same text, said');
  told();
}

{
  // the course page's header: "更新全部过期讲座" asks first (the drafts' one is checked above)
  const stale2 = [staleLecture('S1'), staleLecture('S2')];
  const course = courseOf({ course_actions: ['update_stale'], draft_count: 0, stale_lecture_count: 2 });
  const m = await mountCourse(course, stale2, { extra: (meth, u) => (meth === 'POST' && u === '/api/courses/C1/update-stale' ? { status: 200, body: { results: [] } } : null) });
  const before = openOnes().length;
  let b = fk('course:stale');
  assert.ok(b, 'the header has "更新全部过期讲座"');
  sent.length = 0;
  b.focus();
  b.click();
  await settle();
  assert.equal(openOnes().length, before + 1, 'it asks first');
  let dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('course.confirm.update_title_many', { n: 2 })) && dlg.textContent.includes('Lecture S1') && dlg.textContent.includes('Lecture S2'), 'how many, naming them');
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null, 'the focus starts on "cancel"');
  cancelOf(dlg).click();
  await settle();
  assert.deepEqual(writes(), [], 'cancelled: nothing sent');
  assert.ok(doc.activeElement === fk('course:stale'), 'the focus is back on the button');
  fk('course:stale').click();
  await settle();
  okOf(openOnes().at(-1)).click();
  await until(() => writes().length > 0, 'the update is sent');
  await settle();
  assert.deepEqual(writes(), ['POST /api/courses/C1/update-stale'], 'confirmed: sent once, as before');
  assert.equal(openOnes().length, before, 'no other dialog');
  m.done();
  told();
}

// ---- WL-12 (R70 low 2): a click on the check box's cell selects the row
// (the box's wider hit area, input.check::after, is drawn by Chromium only) ----

{
  const L = await import(new URL('lecture_list.js', base).href);
  const M = await import(new URL('menu.js', base).href);
  const course = { id: 'C1', code: 'CSC1', name: 'Course', term_start: '2026-09-07', skip_weeks: [] };
  const list = L.createLectureList({ ctx: { system: () => null, resources: () => null }, onLectures() {}, onDeleted() {}, course: () => course, filtered: () => false });
  body.append(list.table, list.bar);
  const ls = [lecture('A'), lecture('B'), lecture('C'), lecture('D', { kind: 'material', title: 'Slides D' })];
  list.render(ls);
  const rowOf = (id) => list.table.querySelector(`div.lrow[data-id="${id}"]`);
  const cellOf = (id) => rowOf(id).querySelector('span.lt-check');
  const selectedIds = () => list.table.querySelectorAll('div.lrow').filter((r) => r.classList.contains('is-selected')).map((r) => r.dataset.id);
  const clickCell = (id, o = {}) => cellOf(id).dispatchEvent(event('click', o));
  const nameBefore = fk('lec:A:check').getAttribute('aria-label');
  assert.ok(nameBefore && nameBefore.includes('Lecture A'), 'the box is named for its lecture');
  // the cell's blank area: as a click on the box
  clickCell('A');
  assert.deepEqual(selectedIds(), ['A'], 'a click on the cell selects the row');
  assert.ok(fk('lec:A:check').hasAttribute('checked'), 'and checks its box (drawn checked)');
  assert.ok(doc.activeElement === fk('lec:A:check'), 'the box has the focus, as after a click on it (↑ / ↓ go on from there)');
  // Shift: the range from the last one clicked, as with the box
  clickCell('C', { shiftKey: true });
  assert.deepEqual(selectedIds(), ['A', 'B', 'C'], 'Shift+click on a cell selects the range');
  clickCell('B', { shiftKey: true });
  assert.deepEqual(selectedIds(), ['A'], 'Shift+click again: the range unchecked, as the box does');
  clickCell('A');
  assert.deepEqual(selectedIds(), [], 'a second click unchecks it');
  // the box itself as before (one toggle, not two)
  const box = fk('lec:B:check');
  box.checked = true; // as the click leaves it
  box.click();
  assert.deepEqual(selectedIds(), ['B'], 'a click on the box: one toggle');
  // a teaching material's drag handle in the cell is no click on the cell
  const grip = rowOf('D').querySelector('button.lrow-grip');
  assert.ok(grip && cellOf('D').contains(grip), 'the material has its drag handle in the cell');
  grip.click();
  await settle(10);
  assert.deepEqual(selectedIds(), ['B'], 'the handle does not select');
  M.closeMenu({ focus: false });
  // the box's name for a screen reader is unchanged
  assert.equal(fk('lec:A:check').getAttribute('aria-label'), nameBefore, 'the box\'s name is unchanged');
  assert.equal(cellOf('A').getAttribute('aria-label'), null, 'the cell adds no name of its own');
  assert.equal(cellOf('A').getAttribute('role'), 'cell', 'still a table cell');
  // the header's cell: select all
  const head = list.table.querySelector('div.lt-head').querySelector('span.lt-check');
  head.dispatchEvent(event('click'));
  assert.deepEqual(selectedIds().sort(), ['A', 'B', 'C', 'D'], 'a click on the header\'s cell selects all');
  head.dispatchEvent(event('click'));
  assert.deepEqual(selectedIds(), [], 'and again none');
  list.table.remove();
  list.bar.remove();
  body.focus();
}

// ---- C178b (R74 low 2a, the selection's part): "start" of the selected
// drafts asks through launchLectures's opts.ask (after the glossary
// question there), once; no confirmation of its own before it ----

{
  // a copy of lecture_list.js whose actions.js is r8_actions_standin.mjs
  // (launchLectures with the shared line's opts.ask, which this branch's
  // actions.js does not have yet)
  if (typeof registerHooks !== 'function') throw new Error('node:module registerHooks is needed (node 22.15 or later)');
  const standIn = new URL('testdata/r8_actions_standin.mjs', base).href;
  const copy = new URL('lecture_list.js?launch-stand-in', base).href;
  const hooks = registerHooks({
    resolve(specifier, context, nextResolve) {
      if (specifier === './actions.js' && context.parentURL === copy) return { url: standIn, shortCircuit: true };
      return nextResolve(specifier, context);
    },
  });
  const L = await import(copy);
  const S = await import(standIn);
  hooks.deregister();
  const M = await import(new URL('menu.js', base).href);
  const draft = (id) => lecture(id, { draft: true, lecture_actions: ['launch'] });
  const list = L.createLectureList({ ctx: { system: () => null, resources: () => null }, onLectures() {}, onDeleted() {}, course: () => null, filtered: () => false });
  body.append(list.table, list.bar);
  const shown = [draft('D1'), draft('D2'), lecture('P3')];
  list.render(shown);
  for (const id of ['D1', 'D2']) {
    const box = fk(`lec:${id}:check`);
    box.checked = true; // as the click leaves it
    box.click();
  }
  net.answer = (meth, u) => (meth === 'POST' && u === '/api/lectures/launch' ? { status: 200, body: { results: [{ id: 'D1', ok: true }, { id: 'D2', ok: true }] } } : { status: 200, body: {} });
  const before = openOnes().length;
  sent.length = 0;
  S.launchCalls.length = 0;
  fk('batch:launch').click();
  await settle();
  assert.equal(S.launchCalls.length, 1, 'the bar\'s "start": launchLectures, once');
  assert.deepEqual(S.launchCalls[0].ids, ['D1', 'D2'], 'with the selected drafts');
  assert.ok(S.launchCalls[0].ask && S.launchCalls[0].dialogsOpen === before, 'with its confirmation as opts.ask, nothing asked before it');
  assert.equal(openOnes().length, before + 1, 'one dialog');
  let dlg = openOnes().at(-1);
  assert.ok(dlg.textContent.includes(t('course.confirm.launch_title', { n: 2 })) && dlg.textContent.includes('Lecture D1'), 'the confirmation of "start all drafts", for them');
  assert.ok(cancelOf(dlg).getAttribute('autofocus') != null, 'the focus starts on "cancel"');
  cancelOf(dlg).click();
  await settle();
  assert.equal(openOnes().length, before, 'cancelled: no other dialog');
  assert.deepEqual(writes(), [], 'and nothing sent');
  // the selection's menu: the same
  list.openMenuFor(shown[0], fk('lec:D1:more'));
  await settle(10);
  doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(t('course.lectures.batch.launch_n', { n: 2 }))).click();
  await settle();
  assert.equal(S.launchCalls.length, 2, 'the menu\'s "start": launchLectures');
  assert.ok(S.launchCalls[1].ask && S.launchCalls[1].dialogsOpen === before, 'asking through it');
  assert.equal(openOnes().length, before + 1, 'one dialog');
  okOf(openOnes().at(-1)).click();
  await until(() => writes().length > 0, 'the launch is sent');
  await settle();
  assert.deepEqual(writes(), ['POST /api/lectures/launch'], 'confirmed: one request, as before');
  assert.equal(openOnes().length, before, 'no dialog left');
  M.closeMenu({ focus: false });
  list.table.remove();
  list.bar.remove();
  body.focus();
  told();
}

// ---- C178b (T22 S4, with the shared line's WH-03): the English files on
// demand say what they are, as the export files do (menu.js item desc) ----

{
  const E = await import(new URL('lecture_english.js', base).href);
  const items = E.englishItems(lecture('EN', { playable: true }));
  const byId = (id) => items.find((x) => x && x.id === id);
  assert.equal(byId('english:vtt').desc, t('menu.export.vtt_en_desc'), 'the English subtitles: what they are');
  assert.equal(byId('english:txt').desc, t('menu.export.txt_en_desc'), 'the English transcript: what it is');
  assert.equal(byId('english:vtt').label, t('menu.export.vtt_en'), 'the labels as before');
  assert.equal(byId('english:txt').label, t('menu.export.txt_en'));
}

// ---- WL-11 (R74 low 9): the player's "后退 5 秒" (last: it adds to the stand-in) ----

{
  // what the player asks of the stand-in: media elements, insertion after
  // a node, style properties, animation frames, layout boxes
  const proto = El.prototype;
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
  // (form fields start empty: the transcript's search field)
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
  const withStyle = (el) => {
    Object.defineProperty(el.style, 'setProperty', { value(k, v) { this[k] = v; } });
    Object.defineProperty(el.style, 'removeProperty', { value(k) { delete this[k]; } });
    return el;
  };
  const mk = doc.createElement;
  doc.createElement = (tag) => withStyle(mk(tag));
  const mkNS = doc.createElementNS;
  doc.createElementNS = (ns, tag) => withStyle(mkNS(ns, tag));
  globalThis.cancelAnimationFrame = (id) => clearTimeout(id);

  const Pl = await import(new URL('view_player.js', base).href);
  // three sentences of 7.5 s, 8 s apart
  const sentences = [0, 1, 2].map((i) => ({ idx: i, start_ms: i * 8000, end_ms: i * 8000 + 7500, text: `S${i}.`, asr_text: `S${i}.`, human_text: null, zh: `句${i}`, zh_status: 'ok', patch: null }));
  const lec = lecture('PL', { playable: true, reviewable: true, duration_ms: 60000, stream: { status: 'ready' }, counts: { sentences: 3, patches_pending: 0 } });
  net.answer = (meth, u) => {
    if (u.startsWith('/api/lectures/PL/sentences')) return { status: 200, body: { total: 3, sentences } };
    if (u.startsWith('/api/lectures/PL/cues')) return { status: 200, body: { cues: [] } };
    if (meth === 'GET' && /^\/api\/lectures\/PL(\?|$)/.test(u)) return { status: 200, body: lec };
    return { status: 200, body: {} };
  };
  const main = new El('main');
  body.append(main);
  const view = Pl.mountPlayer(main, { name: 'play', id: 'PL', t: null }, { courses: () => [], system: () => null, resources: () => null, setCurrentCourse() {}, subscribe: () => () => {} });
  await until(() => main.querySelector('video') && main.querySelector('.pl-row'), 'the player is drawn with its sentences');
  const video = main.querySelector('video');
  video.duration = 60;
  video.currentTime = 0;
  video.dispatchEvent(event('loadedmetadata'));
  await settle();
  const back = main.querySelector('button.pl-ctl-back');
  const loop = main.querySelectorAll('button.pl-ctl').find((b) => b.getAttribute('aria-label') === t('player.ctl.loop'));
  assert.ok(back && back.getAttribute('aria-label') === t('player.ctl.back5'), '"后退 5 秒" is in the controls');
  assert.ok(loop && loop.getAttribute('aria-pressed') === 'false', 'and the single-sentence loop, off');
  const clock = main.querySelector('.pl-clock');
  const at = (s) => {
    video.currentTime = s;
    video.dispatchEvent(event('timeupdate'));
  };
  // near the start: back to 00:00, not before it
  at(2);
  back.click();
  assert.equal(video.currentTime, 0, '2 s in: back to the start');
  assert.ok(clock.textContent.startsWith('00:00 '), `the clock shows 00:00 (${clock.textContent})`);
  assert.equal(main.querySelector('div.pl-row[data-idx="0"]').getAttribute('aria-current'), 'true', 'at 00:00 the first sentence is the current one (not "before the start")');
  // within the sentence it loops: the loop stays
  at(7);
  loop.click();
  assert.equal(loop.getAttribute('aria-pressed'), 'true', 'the loop is on (the first sentence)');
  back.click();
  assert.equal(video.currentTime, 2, 'back 5 s');
  assert.equal(loop.getAttribute('aria-pressed'), 'true', 'still in its sentence: the loop stays');
  loop.click();
  // out of the sentence it loops: the loop ends
  at(9);
  loop.click();
  assert.equal(loop.getAttribute('aria-pressed'), 'true', 'the loop is on (the second sentence)');
  back.click();
  assert.equal(video.currentTime, 4, 'back 5 s, into the first sentence');
  assert.equal(loop.getAttribute('aria-pressed'), 'false', 'out of the sentence it looped: the loop ends');
  view.destroy();
  main.remove();
  body.focus();
  told();
}

console.log('r8 lecture checks: ok');
