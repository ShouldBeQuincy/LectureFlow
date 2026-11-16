// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the r9-web-views line, its lecture side (the lecture page, the
// transcript tab, the player, the stage bar, the glossary); each section
// names its item (WV-01 …). The modules run on the DOM stand-in of
// r7_dom.mjs; web/r9_views_test.go starts it with the web directory as a
// file: URL. Stand-in nodes are compared by identity (assert.ok(a === b)),
// never with equal / deepEqual. Every wait is bounded (until(): tries).

import { assert, El, body, doc, base, net, sent, settle, until, fk, told, event } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const M = await import(new URL('menu.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

// ---- what the checks share ----

// What the modules ask of the stand-in beyond r7_dom: a list of selectors
// and :not() (menu.js), scrollIntoView, the value of a form field (empty
// until set), a text field's caret.
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
  El.prototype.setSelectionRange = function () {};
  Object.defineProperty(El.prototype, 'value', {
    get() {
      return this._value ?? '';
    },
    set(v) {
      this._value = v;
    },
    configurable: true,
  });
}

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
// a lecture whose translation is in that state (held: stale, failed, cancelled)
const withTranslate = (id, status, o = {}) => lecture(id, { stages: names.map((n) => (n === 'translate' ? stage(n, status) : n === 'export' ? stage(n, 'pending') : stage(n, 'done'))), ...o });

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

// A sentence action's toast: held (the translation stale, failed or
// cancelled) its text stays until closed; else it fades after 4 seconds.
async function toldHeld(fade, what, key, act) {
  told();
  await act();
  await until(() => toastOf(t(key)), `${what}: its toast`);
  const el = toastOf(t(key));
  assert.ok(el.dataset.kind === 'info' && el.getAttribute('role') === 'status', `${what}: an info toast, read out politely (as "the same translation", WL-05)`);
  assert.equal(el._sticky, true, `${what}: drawn by dom.js as a sticky toast`);
  fade();
  assert.ok(toastOf(t(key)) === el, `${what}: still there after 4 seconds`);
  el.querySelector('.toast-close').click();
  assert.equal(toastOf(t(key)), null, `${what}: closed by its button`);
}
async function toldPlain(fade, what, key, act) {
  told();
  await act();
  await until(() => toastOf(t(key)), `${what}: its toast`);
  assert.equal(toastOf(t(key)).dataset.kind, 'success', `${what}: a success toast, as before`);
  fade();
  assert.equal(toastOf(t(key)), null, `${what}: gone after 4 seconds, as before`);
}

// ---- WV-01 (T28 suggestion 3): while the translation is held, what
// "重翻此句", "保存原文" and "恢复" say stays until closed ----

const RC = await import(new URL('retranslate_current.js', base).href);
const T = await import(new URL('view_transcript.js', base).href);

// The transcript tab of lec with two sentences: 0 edited by hand (it can be
// restored) with a translation, 1 whose translation failed its check.
const txRows = () => [
  { idx: 0, start_ms: 0, end_ms: 900, text: 'Sentence zero.', asr_text: 'Sentence 0.', human_text: 'Sentence zero.', zh: '句子零。', zh_status: 'ok', patch: null, low_conf_words: [], qa_error: null },
  { idx: 1, start_ms: 1000, end_ms: 1900, text: 'Sentence one.', asr_text: 'Sentence one.', human_text: null, zh: '句子一。', zh_status: 'qa_failed', patch: null, low_conf_words: [], qa_error: null },
];
function sentenceRoutes(id, rows) {
  return (m, u) => {
    if (m === 'GET' && u.includes(`/api/lectures/${id}/sentences`)) return { status: 200, body: { total: rows.length, sentences: rows } };
    const x = /\/sentences\/(\d+)\/(edit|reset|retranslate)$/.exec(u);
    if (m === 'POST' && x) {
      const s = rows[Number(x[1])];
      if (x[2] === 'edit') return { status: 200, body: { ...s, text: 'Edited.', human_text: 'Edited.', zh_status: 'stale' } };
      if (x[2] === 'reset') return { status: 200, body: { ...s, text: s.asr_text, human_text: null, zh_status: 'stale' } };
      return { status: 200, body: { ...s, zh_status: 'stale', mixed: false } };
    }
    return { status: 200, body: {} };
  };
}
async function mountTranscript(lec) {
  const rows = txRows();
  net.answer = sentenceRoutes(lec.id, rows);
  const host = D.h('div');
  body.append(host);
  const view = T.mountTranscript(host, { ...lec, reviewable: true, counts: { sentences: rows.length, patches_pending: 0 } }, { courses: () => [], system: () => null, resources: () => null });
  await until(() => fk('more:1'), 'the transcript shows its sentences');
  return {
    host,
    view,
    done() {
      M.closeMenu({ focus: false });
      view.destroy();
      host.remove();
      body.focus();
    },
  };
}
// picks the item of a sentence's menu (its "⋯") whose label has that text
async function pick(moreKey, label) {
  M.closeMenu({ focus: false });
  fk(moreKey).click();
  await settle(10);
  const item = doc.querySelectorAll('.ctx-item').find((b) => b.textContent.includes(label));
  assert.ok(item, `the menu has "${label}"`);
  item.click();
}
// the transcript's own edit of sentence idx, saved with Enter
async function txEdit(idx) {
  await pick(`more:${idx}`, t('player.menu.edit'));
  const ta = await (async () => {
    await until(() => fk(`edit:${idx}`), 'the edit box');
    return fk(`edit:${idx}`);
  })();
  ta.value = 'Edited.';
  ta.dispatchEvent(event('input'));
  ta.dispatchEvent(event('keydown', { key: 'Enter' }));
}

await withFading(async (fade) => {
  // the transcript tab, translation held (failed), then not (done)
  let m = await mountTranscript(withTranslate('R', 'failed'));
  await toldHeld(fade, 'transcript, 重翻此句', 'player.menu.retranslate_held', () => pick('more:0', t('player.menu.retranslate')));
  await toldHeld(fade, 'transcript, 用当前设置重翻此句', 'player.menu.retranslate_held', () => pick('more:1', t('player.menu.retranslate_current')));
  await toldHeld(fade, 'transcript, 恢复', 'player.menu.reset_done_held', () => pick('more:0', t('player.menu.reset')));
  await toldHeld(fade, 'transcript, 保存原文', 'player.edit.saved_held', () => txEdit(1));
  m.done();
  m = await mountTranscript(withTranslate('R', 'done'));
  await toldPlain(fade, 'transcript not held, 重翻此句', 'player.menu.retranslate_done', () => pick('more:0', t('player.menu.retranslate')));
  await toldPlain(fade, 'transcript not held, 恢复', 'player.menu.reset_done', () => pick('more:0', t('player.menu.reset')));
  await toldPlain(fade, 'transcript not held, 保存原文', 'player.edit.saved', () => txEdit(1));
  m.done();
  // "用当前设置重翻此句" itself (retranslate_current.js: the player calls it too)
  net.answer = (meth, u) => (meth === 'POST' && u === '/api/lectures/R/sentences/3/retranslate' ? { status: 200, body: { idx: 3, zh_status: 'stale', mixed: false } } : { status: 200, body: {} });
  for (const status of ['stale', 'failed', 'cancelled']) {
    await toldHeld(fade, `用当前设置重翻此句, translation ${status}`, 'player.menu.retranslate_held', () => RC.retranslateCurrent('R', 3, withTranslate('R', status)));
  }
  await toldPlain(fade, '用当前设置重翻此句, not held', 'player.menu.retranslate_done', () => RC.retranslateCurrent('R', 3, withTranslate('R', 'done')));
});
told();

// ---- WV-02 (T28 suggestion 4): the edit box of the English sentence is
// described by its two notes (aria-describedby) ----

// the texts of the elements a field's aria-describedby names (each must exist)
function describedBy(el, what) {
  const ids = (el.getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
  assert.equal(ids.length, 2, `${what}: described by two notes`);
  return ids.map((id) => {
    const d = doc.getElementById(id);
    assert.ok(d && d.isConnected, `${what}: the note ${id} is on the page`);
    return d.textContent;
  });
}
const writes = () => sent.filter((s) => !s.startsWith('GET '));

for (const [status, hint, zhNote] of [
  ['failed', 'transcript.edit.hint_held', 'player.edit.zh_note_held'],
  ['done', 'transcript.edit.hint', 'player.edit.zh_note'],
]) {
  const m = await mountTranscript(withTranslate('R', status));
  await pick('more:1', t('player.menu.edit'));
  await until(() => fk('edit:1'), 'the edit box');
  const ta = fk('edit:1');
  assert.deepEqual(describedBy(ta, `transcript, translation ${status}`), [t(hint), t(zhNote)], `transcript, translation ${status}: by the two notes under it, as they read`);
  assert.equal(ta.getAttribute('aria-label'), t('transcript.edit.aria', { n: 2 }), 'its name as before');
  assert.ok(doc.activeElement === ta, 'it has the focus, as before');
  // Shift+Enter is a new line (nothing sent), Esc cancels, as before
  sent.length = 0;
  ta.dispatchEvent(event('keydown', { key: 'Enter', shiftKey: true }));
  await settle(30);
  assert.deepEqual(writes(), [], 'Shift+Enter: nothing saved');
  const esc = event('keydown', { key: 'Escape' });
  ta.dispatchEvent(esc);
  assert.ok(esc.defaultPrevented, 'Esc is taken by the box');
  assert.equal(fk('edit:1'), null, 'Esc: the edit ends');
  m.done();
}
told();

// ---- WV-03 (R87 L1): the segment focused last keeps the bar's Tab stop
// only while what matters most stays the same ----

{
  const P = await import(new URL('pipeline.js', base).href);
  const stops = (bar) => bar.querySelectorAll('button.seg').map((b) => b.getAttribute('tabindex'));
  const stopAt = (bar) => stops(bar).indexOf('0');
  // segment i of the bar focused (as the keyboard or a click does), then the bar left
  const focusSeg = (bar, i) => {
    body.append(bar);
    const b = bar.querySelectorAll('button.seg')[i];
    b.focus();
    b.dispatchEvent(event('focusin', { relatedTarget: null }));
    bar.remove();
    body.focus();
  };
  const KEY = 'wv03:L';
  const draw = (stages) => P.pipelineBar(stages, { key: KEY });
  const allDone = () => names.map((n) => stage(n, 'done'));
  const translateIs = (status) => names.map((n) => (n === 'translate' ? stage(n, status) : n === 'export' ? stage(n, 'pending') : stage(n, 'done')));
  assert.equal(stopAt(draw(allDone())), 0, 'all done: the first segment');
  // "转写" focused: the bars drawn after keep it
  focusSeg(draw(allDone()), 1);
  assert.equal(stopAt(draw(allDone())), 1, 'redrawn: still "转写" (R79 L8)');
  assert.equal(stopAt(draw(allDone())), 1, 'and again');
  // the translation failed meanwhile: the Tab stop goes to it, and stays there
  assert.equal(stopAt(draw(translateIs('failed'))), 4, 'a new failure: the Tab stop on "翻译"');
  assert.equal(stopAt(draw(translateIs('failed'))), 4, 'redrawn: still on the failure (the memory dropped)');
  // another segment focused now: kept while the same stage matters most
  focusSeg(draw(translateIs('failed')), 2);
  assert.equal(stopAt(draw(translateIs('failed'))), 2, 'focused "摘要" after the failure: kept');
  // R91 low-3: the same stage in another state is what matters now
  assert.equal(stopAt(draw(translateIs('running'))), 4, 'the failed stage retried and running: the Tab stop on it');
  focusSeg(draw(translateIs('running')), 1);
  assert.equal(stopAt(draw(translateIs('running'))), 1, 'focused "转写" while it runs: kept on the redraws');
  assert.equal(stopAt(draw(translateIs('failed'))), 4, 'then it failed: the Tab stop on the failure');
  // a failure elsewhere: back to it
  const repairFailed = names.map((n) => (n === 'repair' ? stage(n, 'failed') : n === 'translate' || n === 'export' ? stage(n, 'pending') : stage(n, 'done')));
  assert.equal(stopAt(draw(repairFailed)), 3, 'another stage failed: the Tab stop on it');
  // all done again (nothing matters more): the memory of a focus stays
  focusSeg(draw(repairFailed), 0);
  assert.equal(stopAt(draw(allDone())), 0, 'focused, then all done: kept');
  focusSeg(draw(allDone()), 5);
  assert.equal(stopAt(draw(allDone())), 5, 'all done, "导出" focused: kept');
  // the same stage that once mattered failing again counts as new
  focusSeg(draw(translateIs('failed')), 1);
  assert.equal(stopAt(draw(allDone())), 1, 'all done after it: kept');
  assert.equal(stopAt(draw(translateIs('failed'))), 4, 'the translation failing again: back to it');
  // another bar's key is not affected
  assert.equal(stopAt(P.pipelineBar(allDone(), { key: 'wv03:other' })), 0, 'another bar: its own Tab stop');
}

// ---- WV-04 (R87 L2, I4): the lecture page's read of the providers: a
// read that hangs is waited for once; a late older read is not taken;
// "changed settings" in a row are read once ----

{
  const Lec = await import(new URL('view_lecture.js', base).href);
  // a translation that failed with a 404 for a model its provider may no
  // longer list: the page asks the settings (U.mayBeUnlisted)
  const llmInfo = { code: 'llm.request_failed', params: { role: 'translate', retries: 0, provider: 'DeepSeek', model: 'deepseek-chat', status: 404 } };
  const failedSt = stage('translate', 'failed', { error: 'raw', error_info: llmInfo, model: { provider_id: 'deepseek', model: 'deepseek-chat' }, allowed_actions: ['retry'] });
  let lec = lecture('V4', { title: 'Lecture V4', stages: names.map((n, j) => (j < 4 ? stage(n, 'done') : n === 'translate' ? failedSt : stage(n, 'pending'))) });
  const unlisted = (page) => {
    const n = page.querySelector('div.lec-notice.is-failed');
    return !!n && !!n.querySelector('.lec-notice-hint');
  };
  const providers = (models) => [{ id: 'deepseek', name: 'DeepSeek', models }];
  // every read of the settings hangs until the check answers it (any order)
  const reads = [];
  net.answer = (meth, u) => {
    if (u.startsWith('/api/settings')) {
      let answer;
      const p = new Promise((r) => (answer = r));
      reads.push((models) => answer({ status: 200, body: { providers: providers(models), roles: [] } }));
      return p;
    }
    if (u.startsWith('/api/lectures/V4/sentences')) return { status: 200, body: { total: 0, sentences: [] } };
    if (meth === 'GET' && u.startsWith('/api/lectures/V4') && !u.includes('/patches')) return { status: 200, body: lec };
    return { status: 200, body: {} };
  };
  // the page's 3-second waits are held, and fired by the check
  const realSet = globalThis.setTimeout;
  const realClear = globalThis.clearTimeout;
  const waits = new Map();
  let nextId = 1;
  globalThis.setTimeout = (f, ms, ...args) => {
    if (ms !== 3000) return realSet(f, ms, ...args);
    const id = `wait-${nextId++}`;
    waits.set(id, f);
    return id;
  };
  globalThis.clearTimeout = (id) => (typeof id === 'string' && id.startsWith('wait-') ? waits.delete(id) : realClear(id));
  const page = new El('main');
  body.append(page);
  let view = null;
  try {
    view = Lec.mountLecture(page, { id: 'V4', tab: 'review' }, { system: () => null, resources: () => null, courses: () => [{ id: 'C1', code: 'CSC1', name: 'Course' }], setCurrentCourse() {}, subscribe: () => () => {} });
    await until(() => reads.length === 1 && waits.size === 1, 'the first draw waits for the settings it reads');
    await settle(60);
    assert.equal(page.querySelector('.lec-stats'), null, 'not drawn while it waits (WL-07: drawn once with what they tell)');
    // the 3 seconds pass, the read still hangs: drawn not knowing
    for (const f of [...waits.values()]) f();
    waits.clear();
    await until(() => page.querySelector('.lec-stats'), 'drawn after the wait');
    assert.ok(!unlisted(page), 'not knowing: no hint');
    // the lecture read again (an SSE event): drawn at once, no second wait
    lec = { ...lec, title: 'Lecture V4 renamed' };
    view.onChanged({ kind: 'lecture', id: 'V4' });
    await until(() => page.textContent.includes('Lecture V4 renamed'), 'the next read drawn without waiting for the hanging read again', 100);
    assert.equal(waits.size, 0, 'no second 3-second wait for the same read');
    assert.equal(reads.length, 1, 'and no second read while it hangs');
    // the settings changed three times in a row: read once. The events are
    // 110 ms apart: past api.js's sharing of one read issued together
    // (SHARE_MS 100), within the page's merging of them
    for (let i = 0; i < 3; i++) {
      view.onChanged({ kind: 'settings', id: '' });
      await settle(110);
    }
    await settle(300);
    assert.equal(reads.length, 2, 'three "changed settings" in a row: one read');
    assert.equal(waits.size, 0, 'and the page does not wait for it');
    // the newer read comes back first (the model listed), the older one later (not listed)
    reads[1](['deepseek-flash', 'deepseek-chat']);
    await settle(80);
    reads[0](['deepseek-flash']);
    await settle(150);
    assert.ok(!unlisted(page), 'the older read came back last: not taken');
    view.onChanged({ kind: 'lecture', id: 'V4' });
    await settle(400);
    assert.ok(!unlisted(page), 'drawn again: the newer read\'s providers');
    assert.equal(reads.length, 2, 'kept, not read again');
    // a later change of the settings that drops the model: read again, the hint
    view.onChanged({ kind: 'settings', id: '' });
    await until(() => reads.length === 3, 'read again after "changed settings"');
    reads[2](['deepseek-flash']);
    await until(() => unlisted(page), 'the hint once it came');
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
    if (view) view.destroy();
    page.remove();
    body.focus();
    told();
  }
}

// ---- WV-05 (needs-human 1.15 b): a term just added is scrolled to within
// the glossary table and highlighted; the focus stays in the add form ----

{
  const G = await import(new URL('glossary.js', base).href);
  const mkTerm = (i, en, o = {}) => ({ id: `T${i}`, en, zh: `译${i}`, source: 'syllabus', enabled: true, freq: 10, tier: 'enforce', locked: false, common_word: false, janitor_note: '', zh_source: 'extractor', ...o });
  let terms = Array.from({ length: 30 }, (_, i) => mkTerm(i, `term ${String(i).padStart(2, '0')}`));
  let added = 0;
  net.answer = (m, u, b) => {
    const path = u.replace(/\?.*$/, '');
    if (m === 'GET' && path === '/api/courses/C1/terms') return { status: 200, body: terms };
    if (m === 'POST' && path === '/api/courses/C1/terms') {
      const term = mkTerm(100 + added++, b.en, { zh: b.zh, source: 'manual', zh_source: 'user', locked: true });
      terms = [...terms, term].sort((x, y) => (x.en < y.en ? -1 : 1));
      return { status: 201, body: term };
    }
    return { status: 200, body: {} };
  };
  const gl = G.createGlossary({ courseId: 'C1', ctx: { system: () => null, resources: () => null }, onCourse() {}, reloadCourse: async () => {} });
  body.append(gl.el);
  gl.setCourse({ id: 'C1', name: 'Course', term_count: 30, glossary_version: 1, stages: [], materials: [], janitor: { configured: true } });
  await gl.reload();
  const wrap = gl.el.querySelector('.terms-table-wrap');
  const form = gl.el.querySelector('.term-add');
  const [enInput, zhInput] = form.querySelectorAll('input');
  const sourceSel = gl.el.querySelector('.term-source');
  const rowOf = (en) => {
    const term = terms.find((x) => x.en === en);
    return term ? wrap.querySelector(`tr[data-term="${term.id}"]`) : null;
  };
  // a layout: the table's box 300 px high (at y = 100 at first), its column
  // header 30 px (sticky), the term rows 40 px each in their order, moved
  // by the table's scrollTop
  const HEAD = 30;
  let HEIGHT = 300;
  const ROW = 40;
  const realRect = El.prototype.getBoundingClientRect;
  const realInto = El.prototype.scrollIntoView;
  const realScrollTo = globalThis.scrollTo;
  const realScrollBy = globalThis.scrollBy;
  // the page: scrolled by pageY (window.scrollBy moves it, and is recorded);
  // the window is innerHeight (900) high. TOP is where the table's box is
  // when the page is not scrolled (R91 medium-1: partly out of the window
  // in the later cases).
  let TOP = 100;
  let pageY = 0;
  let VH = globalThis.innerHeight;
  const realVH = globalThis.innerHeight;
  const pageMoves = [];
  let otherScrolls = 0; // scrollIntoView / scrollTo: never used
  const boxTop = () => TOP - pageY;
  // the English field (30 px high) FIELD_ABOVE px above the table's box;
  // the toasts' area (fixed in the window; none: no height)
  let FIELD_ABOVE = 110;
  let toastBox = { top: 0, bottom: 0, left: 0, right: 0 };
  const toastsArea = doc.getElementById('toasts');
  El.prototype.getBoundingClientRect = function () {
    if (this === enInput) return { top: boxTop() - FIELD_ABOVE, bottom: boxTop() - FIELD_ABOVE + 30, left: 0, right: 300 };
    if (this === toastsArea) return toastBox;
    if (this === wrap) return { top: boxTop(), bottom: boxTop() + HEIGHT, left: 0, right: 600 };
    if (this.tagName === 'THEAD' && wrap.contains(this)) return { top: boxTop(), bottom: boxTop() + HEAD, left: 0, right: 600 };
    if (this.tagName === 'TR' && this.dataset.term && wrap.contains(this)) {
      const top = boxTop() + HEAD + wrap.querySelectorAll('tr[data-term]').indexOf(this) * ROW - (wrap.scrollTop || 0);
      return { top, bottom: top + ROW, left: 0, right: 600 };
    }
    return realRect.call(this);
  };
  El.prototype.scrollIntoView = function () {
    otherScrolls++;
  };
  globalThis.scrollTo = () => otherScrolls++;
  globalThis.scrollBy = (o) => {
    const dy = typeof o === 'object' ? o.top : o;
    assert.ok(typeof o !== 'object' || !o.left, 'the page is not scrolled sideways');
    pageMoves.push(dy);
    pageY += dy;
  };
  // the table's box scrolls as a browser's does: within its content
  let wrapTop = 0;
  Object.defineProperty(wrap, 'clientHeight', { get: () => HEIGHT, configurable: true });
  Object.defineProperty(wrap, 'scrollTop', {
    get: () => wrapTop,
    set: (v) => {
      const max = Math.max(0, HEAD + wrap.querySelectorAll('tr[data-term]').length * ROW - HEIGHT);
      wrapTop = Math.max(0, Math.min(max, Number(v) || 0));
    },
    configurable: true,
  });
  wrap.clientTop = 0;
  // shown in the table (below its column header) and in the window
  const inView = (row) => {
    const r = row.getBoundingClientRect();
    return r.top >= boxTop() + HEAD && r.bottom <= boxTop() + HEIGHT && r.top >= 0 && r.bottom <= VH;
  };
  // a new case: the table's box at top (page unscrolled), table scrolled to scroll
  const layout = (top, scroll, o = {}) => {
    TOP = top;
    pageY = 0;
    pageMoves.length = 0;
    HEIGHT = o.height || 300;
    VH = o.vh || realVH;
    globalThis.innerHeight = VH;
    FIELD_ABOVE = o.fieldAbove || 110;
    toastBox = o.toast || { top: 0, bottom: 0, left: 0, right: 0 };
    wrap.scrollTop = scroll;
  };
  const fieldInWindow = () => {
    const f = enInput.getBoundingClientRect();
    return f.top >= 0 && f.bottom <= VH;
  };
  // the add form submitted with en; → what it told
  const add = async (en) => {
    told();
    enInput.focus();
    enInput.value = en;
    zhInput.value = '';
    form.dispatchEvent(event('submit'));
    await until(() => toastsEl.children.length > 0, `"${en}" added`);
    await settle(60);
    return told();
  };
  try {
    // below what the table shows: scrolled down to it, by as little as it takes
    wrap.scrollTop = 0;
    assert.deepEqual(await add('zz last'), [`success: ${t('glossary.add.done', { en: 'zz last' })}`], 'added: said as before');
    let row = rowOf('zz last');
    assert.ok(row && row.isConnected, 'its row is drawn');
    assert.ok(row.classList.contains('flash'), 'highlighted (.term-row.flash)');
    assert.equal(wrap.scrollTop, 30 * ROW + ROW - (HEIGHT - HEAD), 'the table scrolled down just enough');
    assert.ok(inView(row), 'the row is in view');
    assert.ok(doc.activeElement === enInput, 'the focus stays in the English field (to add the next one)');
    assert.deepEqual(pageMoves, [], 'the table in the window: the page is not scrolled');
    // above what the table shows: scrolled up to it, below the sticky header
    wrap.scrollTop = 900;
    await add('aa first');
    row = rowOf('aa first');
    assert.ok(row.classList.contains('flash'), 'highlighted');
    assert.equal(wrap.scrollTop, 0, 'the table scrolled up to it');
    assert.ok(inView(row), 'in view, not under the column header');
    assert.ok(doc.activeElement === enInput, 'the focus stays in the English field');
    // already in view: not scrolled
    wrap.scrollTop = 0;
    await add('term 03a');
    row = rowOf('term 03a');
    assert.ok(row.classList.contains('flash') && inView(row), 'in view and highlighted');
    assert.equal(wrap.scrollTop, 0, 'not scrolled');
    assert.deepEqual(pageMoves, [], 'the page never scrolled');
    // R91 medium-1: the table's box half below the window (750-1050, the
    // window 900 high): the table scrolls the row to its box's bottom, then
    // the page the 150 px left, no more
    layout(750, 0);
    await add('zzz below the window');
    row = rowOf('zzz below the window');
    assert.ok(row.classList.contains('flash'), 'highlighted');
    assert.equal(row.getBoundingClientRect().bottom, boxTop() + HEIGHT, 'the table: the row at its box\'s bottom');
    assert.deepEqual(pageMoves, [150], 'the page scrolled the 150 px the row was below the window');
    assert.ok(inView(row), 'the row in the table\'s view and in the window');
    assert.ok(doc.activeElement === enInput, 'the focus stays in the English field');
    // the box above the window's top (-200-100), the table scrolled down: the
    // new first row goes below the column header, then the page up to it
    layout(-200, 900);
    await add('a top of all');
    row = rowOf('a top of all');
    assert.ok(row.classList.contains('flash'), 'highlighted');
    assert.equal(wrap.scrollTop, 0, 'the table scrolled up to it');
    assert.deepEqual(pageMoves, [-170], 'the page scrolled up the 170 px it was above the window');
    assert.ok(inView(row), 'in the window, not under the column header');
    assert.equal(row.getBoundingClientRect().top, 0, 'at the window\'s top: as little as it takes');
    // the box half out of the window, the new row in view: nothing scrolls
    layout(750, 0);
    await add('a0 already in view');
    row = rowOf('a0 already in view');
    assert.ok(row.classList.contains('flash') && inView(row), 'in view and highlighted');
    assert.equal(wrap.scrollTop, 0, 'the table not scrolled');
    assert.deepEqual(pageMoves, [], 'the page not scrolled');
    assert.equal(otherScrolls, 0, 'no scrollIntoView / scrollTo (they would move the table\'s box too)');
    // R91b low-7, a narrow window (740 high) with the success toast over
    // the table (684-728): the row goes as high in the table as it can,
    // then the page just enough to show it above the toast; the English
    // field stays in the window
    const narrowToast = { top: 684, bottom: 728, left: 80, right: 340 };
    layout(683, 0, { vh: 740, toast: narrowToast });
    const toldMiddle = await add('m middle term');
    row = rowOf('m middle term');
    assert.deepEqual(toldMiddle, [`success: ${t('glossary.add.done', { en: 'm middle term' })}`], 'said as before');
    assert.ok(row.classList.contains('flash'), 'highlighted');
    assert.deepEqual(pageMoves, [77], 'the page scrolled only the 77 px the row was below the toast\'s edge');
    assert.ok(row.getBoundingClientRect().bottom <= narrowToast.top - 8, `above the toast (${row.getBoundingClientRect().bottom})`);
    assert.ok(inView(row), 'in the table\'s view and in the window');
    assert.ok(fieldInWindow() && doc.activeElement === enInput, 'the English field still in the window, with the focus');
    // the same with a tall table (560) and the field well above it: both
    // cannot be had; the field stays (8 px from the top), the page goes as
    // far as that allows, the toast says the term was added
    layout(683, 0, { vh: 740, height: 560, fieldAbove: 266, toast: narrowToast });
    const toldLast = await add('zzzz last of all');
    row = rowOf('zzzz last of all');
    assert.deepEqual(toldLast, [`success: ${t('glossary.add.done', { en: 'zzzz last of all' })}`], 'the toast says it was added');
    assert.deepEqual(pageMoves, [683 - 266 - 8], 'the page went as far as keeping the field allows');
    assert.equal(enInput.getBoundingClientRect().top, 8, 'the field 8 px from the window\'s top');
    assert.ok(fieldInWindow() && doc.activeElement === enInput, 'the English field in the window, with the focus');
    assert.ok(row.classList.contains('flash'), 'highlighted (where the table shows it)');
    // a toast beside the table (a wide window): it does not count
    layout(750, 0, { toast: { top: 744, bottom: 788, left: 981, right: 1245 } });
    await add('zzzzz toast beside');
    row = rowOf('zzzzz toast beside');
    assert.deepEqual(pageMoves, [150], 'a toast beside the table: the window\'s bottom, as before');
    assert.ok(inView(row), 'in view');
    assert.equal(otherScrolls, 0, 'no scrollIntoView / scrollTo');
    layout(100, 0);
    // hidden by the source filter: not scrolled, said
    sourceSel.value = 'syllabus';
    sourceSel.dispatchEvent(event('change'));
    wrap.scrollTop = 40;
    assert.deepEqual(await add('kept out by the filter'), [`success: ${t('glossary.add.done_hidden', { en: 'kept out by the filter' })}`], 'hidden by the filter: said');
    assert.equal(rowOf('kept out by the filter'), null, 'not shown');
    assert.equal(wrap.scrollTop, 40, 'not scrolled');
    assert.ok(doc.activeElement === enInput, 'the focus stays in the English field');
    sourceSel.value = '';
    sourceSel.dispatchEvent(event('change'));
    assert.ok(!rowOf('kept out by the filter').classList.contains('flash'), 'shown later (the filter cleared): not highlighted then');
    // hidden by a collapsed group: the same
    fk('tg:enforce').click();
    assert.deepEqual(await add('kept out by the fold'), [`success: ${t('glossary.add.done_hidden', { en: 'kept out by the fold' })}`], 'in a collapsed group: said');
    assert.equal(rowOf('kept out by the fold'), null, 'not shown');
    fk('tg:enforce').click(); // unfolded again (the fold is kept in localStorage)
    assert.ok(rowOf('kept out by the fold') && !rowOf('kept out by the fold').classList.contains('flash'), 'unfolded later: not highlighted');
    assert.deepEqual(pageMoves, [], 'hidden: the page not scrolled');
    assert.equal(otherScrolls, 0, 'no scrollIntoView / scrollTo');
    // R91 low-4: a term the read after its adding does not list (not yet,
    // or gone again) is not scrolled to and highlighted by a much later
    // draw that has it (a later read, a change of filter)
    const answer = net.answer;
    const termsPath = (u) => u.replace(/\?.*$/, '') === '/api/courses/C1/terms';
    try {
      let omit = '';
      net.answer = (m, u, b) => {
        const a = answer(m, u, b);
        return m === 'GET' && omit && termsPath(u) ? { ...a, body: a.body.filter((x) => x.en !== omit) } : a;
      };
      layout(100, 0);
      omit = 'n not listed yet';
      await add('n not listed yet');
      assert.equal(rowOf('n not listed yet'), null, 'not in the read after it: not drawn');
      omit = '';
      await gl.reload();
      row = rowOf('n not listed yet');
      assert.ok(row && row.isConnected, 'in a later read: drawn');
      assert.ok(!row.classList.contains('flash'), 'drawn by a later read: not highlighted then');
      assert.equal(wrap.scrollTop, 0, 'not scrolled to');
      assert.deepEqual(pageMoves, [], 'the page not scrolled');
      // a read begun before the adding, answered after it without the term
      // (it was not there yet), does not drop the highlight: the adding's
      // own read draws it
      let holding = true;
      const held = [];
      net.answer = (m, u, b) => {
        if (m !== 'GET' || !holding || !termsPath(u)) return answer(m, u, b);
        const list = terms.slice(); // the list as the server has it when asked
        // held until called; called with true, it fails (a server error)
        return new Promise((r) => held.push((fail) => r(fail ? { status: 500, body: { error: { code: 'api.internal', params: {} } } } : { status: 200, body: list })));
      };
      const early = gl.reload();
      await until(() => held.length === 1, 'the earlier read asked');
      told();
      enInput.focus();
      enInput.value = 'o added during a read';
      zhInput.value = '';
      form.dispatchEvent(event('submit'));
      await until(() => held.length === 2, 'the read after the adding asked');
      assert.deepEqual(told(), [`success: ${t('glossary.add.done', { en: 'o added during a read' })}`], 'added: said');
      held[0](); // the earlier read: without the term
      await early;
      await settle(30);
      held[1](); // the adding's own read: with it
      holding = false;
      await until(() => rowOf('o added during a read'), 'drawn by the read after the adding');
      await settle(30);
      row = rowOf('o added during a read');
      assert.ok(row.classList.contains('flash'), 'the earlier read did not drop it: highlighted');
      assert.ok(inView(row), 'in view');
      // R93: the same with the earlier read failing after the adding
      layout(100, 0);
      holding = true;
      held.length = 0;
      const earlyFailing = gl.reload();
      await until(() => held.length === 1, 'the earlier read asked (it will fail)');
      told();
      enInput.focus();
      enInput.value = 'p added while a read fails';
      zhInput.value = '';
      form.dispatchEvent(event('submit'));
      await until(() => held.length === 2, 'the read after the adding asked');
      assert.deepEqual(told(), [`success: ${t('glossary.add.done', { en: 'p added while a read fails' })}`], 'added: said');
      held[0](true); // the earlier read: fails
      await earlyFailing;
      await settle(30);
      told(); // its failure, toasted by the API layer
      held[1](); // the adding's own read: with it
      holding = false;
      await until(() => rowOf('p added while a read fails'), 'drawn by the read after the adding');
      await settle(30);
      row = rowOf('p added while a read fails');
      assert.ok(row.classList.contains('flash'), 'the earlier read failing did not drop it: highlighted');
      assert.ok(inView(row), 'in view');
    } finally {
      net.answer = answer;
    }
  } finally {
    El.prototype.getBoundingClientRect = realRect;
    El.prototype.scrollIntoView = realInto;
    globalThis.scrollTo = realScrollTo;
    globalThis.scrollBy = realScrollBy;
    globalThis.innerHeight = realVH;
    if (gl.destroy) gl.destroy();
    gl.el.remove();
    body.focus();
    told();
  }
}

// ---- the player (last: it adds to the stand-in) ----

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
}
const Pl = await import(new URL('view_player.js', base).href);

// the courses the player told the shell it is in (ctx.setCurrentCourse)
const currentCourses = [];
// The player on lec (playable, reviewable) with the sentences of txRows.
async function mountPlayer(lec) {
  const rows = txRows();
  const full = { ...lec, playable: true, reviewable: true, duration_ms: 60000, stream: { status: 'ready' }, counts: { sentences: rows.length, patches_pending: 0 } };
  const routes = sentenceRoutes(lec.id, rows);
  net.answer = (meth, u) => {
    if (u.startsWith(`/api/lectures/${lec.id}/cues`)) return { status: 200, body: { cues: [] } };
    if (meth === 'GET' && new RegExp(`^/api/lectures/${lec.id}(\\?|$)`).test(u)) return { status: 200, body: full };
    return routes(meth, u);
  };
  const main = new El('main');
  body.append(main);
  const view = Pl.mountPlayer(main, { name: 'play', id: lec.id, t: null }, { courses: () => [], system: () => null, resources: () => null, setCurrentCourse: (cid) => currentCourses.push(cid), subscribe: () => () => {} });
  await until(() => main.querySelector('video') && fk('pl:more:1'), 'the player is drawn with its sentences');
  return {
    main,
    done() {
      M.closeMenu({ focus: false });
      view.destroy();
      main.remove();
      body.focus();
    },
  };
}
const openOnes = () => doc.querySelectorAll('dialog').filter((d) => d.open);
// the player's "修改原文" of sentence idx: its dialog
async function playerEditDialog(idx) {
  await pick(`pl:more:${idx}`, t('player.menu.edit'));
  await until(() => openOnes().some((d) => d.querySelector('textarea.pl-edit-ta')), 'the edit dialog');
  return openOnes().find((d) => d.querySelector('textarea.pl-edit-ta'));
}
async function playerSave(idx) {
  const dlg = await playerEditDialog(idx);
  const ta = dlg.querySelector('textarea.pl-edit-ta');
  ta.value = 'Edited.';
  dlg.querySelector('form').dispatchEvent(event('submit'));
}

await withFading(async (fade) => {
  let m = await mountPlayer(withTranslate('PL', 'cancelled'));
  await toldHeld(fade, 'player, 重翻此句', 'player.menu.retranslate_held', () => pick('pl:more:0', t('player.menu.retranslate')));
  await toldHeld(fade, 'player, 恢复', 'player.menu.reset_done_held', () => pick('pl:more:0', t('player.menu.reset')));
  await toldHeld(fade, 'player, 保存原文', 'player.edit.saved_held', () => playerSave(1));
  m.done();
  m = await mountPlayer(withTranslate('PL', 'done'));
  await toldPlain(fade, 'player not held, 重翻此句', 'player.menu.retranslate_done', () => pick('pl:more:0', t('player.menu.retranslate')));
  await toldPlain(fade, 'player not held, 恢复', 'player.menu.reset_done', () => pick('pl:more:0', t('player.menu.reset')));
  await toldPlain(fade, 'player not held, 保存原文', 'player.edit.saved', () => playerSave(1));
  m.done();
});
told();

// WV-02, the player's "修改原文" dialog: its box described by the two notes
for (const [status, hint, zhNote] of [
  ['stale', 'player.edit.hint_held', 'player.edit.zh_note_held'],
  ['done', 'player.edit.hint', 'player.edit.zh_note'],
]) {
  const m = await mountPlayer(withTranslate('PL', status));
  const dlg = await playerEditDialog(1);
  const ta = dlg.querySelector('textarea.pl-edit-ta');
  assert.deepEqual(describedBy(ta, `player, translation ${status}`), [t(hint), t(zhNote)], `player, translation ${status}: by the two notes under it, as they read`);
  assert.ok(ta.getAttribute('aria-describedby').split(' ').every((id) => dlg.contains(doc.getElementById(id))), 'notes of its own dialog');
  assert.ok(ta.getAttribute('autofocus') != null, 'autofocus as before');
  // Shift+Enter is a new line (nothing sent); Enter saves; as before
  sent.length = 0;
  ta.value = 'Edited.';
  ta.dispatchEvent(event('keydown', { key: 'Enter', shiftKey: true }));
  await settle(30);
  assert.deepEqual(writes(), [], 'Shift+Enter: nothing saved');
  ta.dispatchEvent(event('keydown', { key: 'Enter' }));
  await until(() => writes().length > 0, 'Enter saves');
  assert.deepEqual(writes(), ['POST /api/lectures/PL/sentences/1/edit'], 'Enter: saved once');
  await until(() => !dlg.open, 'the dialog closes once saved');
  // Esc closes the dialog unsaved, as before
  const again = await playerEditDialog(1);
  sent.length = 0;
  again.dispatchEvent(event('cancel', { bubbles: false }));
  await settle(30);
  assert.ok(!again.open, 'Esc: closed');
  assert.deepEqual(writes(), [], 'Esc: nothing saved');
  m.done();
  told();
}

// ---- T28 suggestion 8 (the player's half, handed over by the dashboard
// line): the player tells the shell which course it is in, as the lecture
// page does, so the window is named after the course also when the player
// was opened straight from the dashboard; no request of its own for it ----

{
  currentCourses.length = 0;
  sent.length = 0;
  const m = await mountPlayer(withTranslate('PL', 'done'));
  await settle(60);
  assert.ok(currentCourses.length > 0, 'the player sets the current course');
  assert.ok(currentCourses.every((x) => x === 'C1'), `the lecture's course (C1), not another: ${currentCourses.join(', ')}`);
  const courseReads = sent.filter((s) => s.startsWith('GET /api/courses'));
  assert.ok(courseReads.length <= 1, `no more reads of the course than before (its citation's one): ${courseReads.join(', ')}`);
  m.done();
  told();
}

console.log('r9 views lecture checks: ok');
