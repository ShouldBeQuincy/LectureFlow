// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// The interfaces frozen for the round 8 web lines (r8-index section 2):
//   F1  dom.js toast(message, kind, opts): opts.sticky keeps a toast until it
//       is closed; without opts a toast behaves as before;
//   F2  actions.js launchCourseDrafts(course, opts): opts.ask is asked after
//       the glossary check, and a false answer sends nothing;
//   F3  util.js guideLeft(guide): the settings page's rule for a first-run
//       guide left unfinished.
// The modules run on the DOM stand-in of r7_dom.mjs; web/r8_interface_test.go
// starts it with the web directory as a file: URL.

import { assert, toastsEl, base, sent, net, settle, told, cancelDialogs, confirmDialogs, dialogs } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const U = await import(new URL('util.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
const Act = await import(new URL('actions.js', base).href);
await I.setLocale('zh-CN');
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

// ---- F1: sticky toasts ----

{
  // the toasts' 4-second timers are held here and fired by fade(); other
  // timers stay real
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
  const shown = () => toastsEl.children.map((el) => `${el.dataset.kind}: ${el.dataset.msg}`);
  const byMsg = (m) => toastsEl.children.find((el) => el.dataset.msg === m) || null;
  try {
    told();
    // without opts: as before (info / success / warn fade, error stays)
    D.toast('plain info');
    D.toast('plain warn', 'warn');
    D.toast('plain error', 'error');
    assert.equal(fading.size, 2, 'info and warn fade, error does not');
    assert.equal(byMsg('plain info').getAttribute('role'), 'status');
    assert.equal(byMsg('plain error').getAttribute('role'), 'alert');
    D.toast('plain info');
    assert.equal(byMsg('plain info').querySelector('.toast-count').textContent, '×2', 'the same text is merged');
    assert.equal(fading.size, 2, 'a merge re-arms the one timer');
    fade();
    assert.deepEqual(shown(), ['error: plain error'], 'after 4 seconds only the error is left');
    told();

    // sticky: no timer, the close button kept, merged as before
    D.toast('long result', 'success', { sticky: true });
    D.toast('long warning', 'warn', { sticky: true });
    assert.equal(fading.size, 0, 'a sticky toast does not fade');
    const el = byMsg('long result');
    assert.ok(el && el.className === 'toast toast-success', 'its kind is kept');
    assert.equal(el.getAttribute('role'), 'status', 'its role is kept');
    const close = el.querySelector('.toast-close');
    assert.ok(close, 'a sticky toast keeps its close button');
    D.toast('long result', 'success', { sticky: true });
    assert.equal(el.querySelector('.toast-count').textContent, '×2', 'a sticky toast is merged as before');
    D.toast('long result', 'success');
    assert.equal(el.querySelector('.toast-count').textContent, '×3');
    assert.equal(fading.size, 0, 'the same text merged without opts leaves it sticky');
    fade();
    // round 9 (R90 M1): the text told again is the newest toast, after the
    // warning (the page shows only the newest few)
    assert.deepEqual(shown(), ['warn: long warning', 'success: long result'], 'still there after 4 seconds');
    close.click();
    assert.deepEqual(shown(), ['warn: long warning'], 'closed by its button');
    // a sticky text merged into a fading toast stops its fading
    D.toast('then longer', 'info');
    assert.equal(fading.size, 1);
    D.toast('then longer', 'info', { sticky: true });
    assert.equal(fading.size, 0, 'the fading stopped');
    fade();
    assert.ok(byMsg('then longer'), 'still there');
    // opts that are not sticky: as without opts
    D.toast('not sticky', 'info', { sticky: 'yes' });
    D.toast('null opts', 'info', null);
    assert.equal(fading.size, 2, 'only sticky === true keeps a toast');
    fade();
    assert.equal(byMsg('not sticky'), null);
    assert.equal(byMsg('null opts'), null);
  } finally {
    globalThis.setTimeout = realSet;
    globalThis.clearTimeout = realClear;
    told();
  }
}

// ---- F2: launchCourseDrafts asks after the glossary check ----

{
  const course = { id: 'c1', code: 'C1', name: 'Course c1' };
  const launches = () => sent.filter((s) => s.startsWith('POST /api/courses/c1/launch-drafts')).length;
  const result = { results: [{ ok: true, lecture: { id: 'L1', title: 'Lecture 1' } }] };
  net.answer = (m, u) => (m === 'POST' && u.startsWith('/api/courses/c1/launch-drafts') ? { status: 200, body: result } : { status: 200, body: {} });
  Act.setCourseLookup(() => null);
  sent.length = 0;
  told();

  // without opts: launched as before
  let res = await Act.launchCourseDrafts(course);
  assert.equal(launches(), 1, 'without opts the drafts are launched');
  assert.ok(res && Array.isArray(res.results), 'and the answer returned');
  assert.equal(told().length, 1, 'with its toast');

  // ask answers false: null, nothing sent
  let asked = 0;
  res = await Act.launchCourseDrafts(course, { ask: async () => (asked++, false) });
  assert.equal(asked, 1, 'asked once');
  assert.equal(res, null, 'a "no" ends with null');
  assert.equal(launches(), 1, 'a "no" sends nothing');
  assert.equal(told().length, 0, 'and shows nothing');

  // ask answers true: launched once
  res = await Act.launchCourseDrafts(course, { ask: async () => (asked++, true) });
  assert.equal(asked, 2);
  assert.equal(launches(), 2, 'a "yes" launches');
  assert.ok(res && Array.isArray(res.results));
  told();

  // the glossary question comes first: declined, ask is not asked
  Act.setCourseLookup((id) => (id === 'c1' ? { ...course, glossary_pending: true } : null));
  let p = Act.launchCourseDrafts(course, { ask: async () => (asked++, true) });
  await cancelDialogs();
  res = await p;
  assert.equal(res, null);
  assert.equal(asked, 2, 'the glossary check declined: not asked');
  assert.equal(launches(), 2, 'and nothing sent');
  // accepted, then ask: its "no" still sends nothing
  const before = dialogs.length;
  let shownAtAsk = -1;
  p = Act.launchCourseDrafts(course, {
    ask: async () => {
      asked++;
      shownAtAsk = dialogs.length - before;
      return false;
    },
  });
  await settle();
  assert.equal(asked, 2, 'not asked while the glossary question is open');
  await confirmDialogs();
  res = await p;
  assert.equal(asked, 3, 'asked after the glossary check passed');
  assert.equal(shownAtAsk, 1, 'the glossary question was shown before the ask');
  assert.equal(res, null);
  assert.equal(launches(), 2, 'nothing sent');
  Act.setCourseLookup(() => null);
  await settle();
  told();
}

// ---- F3: guideLeft ----

{
  const cases = [
    [null, false, 'no state'],
    [undefined, false, 'no state'],
    [{ step: 'welcome', done: false }, false, 'never started'],
    [{ step: 'welcome', done: false, data: {} }, false, 'never started, empty answers'],
    [{ step: 'welcome', done: false, data: { locale: 'zh-CN' } }, true, 'answers given on the welcome step'],
    [{ step: 'providers', done: false }, true, 'left on a later step'],
    [{ step: 'providers', done: false, data: {} }, true, 'left on a later step, no answers'],
    [{ step: 'providers', done: true, data: { a: 1 } }, false, 'done'],
    [{ step: 'welcome', done: true, data: { a: 1 } }, false, 'done on the welcome step'],
  ];
  for (const [g, want, what] of cases) assert.equal(U.guideLeft(g), want, `guideLeft: ${what}`);
  assert.equal(typeof U.guideLeft({ step: 'x' }), 'boolean', 'answers a boolean');
}

console.log('r8 interface checks: ok');
