// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the interfaces frozen before the lines start:
//   F1  dom.js openDialog's opts.confirmCancel: asked only when Esc or the
//       backdrop would cancel; false keeps the dialog open with the focus
//       where it was, true closes it and gives the focus back; one question
//       at a time; the dialog's own close(v) and a route change do not ask;
//       without it, as before (WV-06 uses it);
//   F2  update_confirm.js confirmLaunchDrafts's opts.courses: two or more
//       courses add a line naming them after the stages, before the cost
//       sentence; fewer, or none given, the dialog is as before (WD-04).
// The modules run on the DOM stand-in of r7_dom.mjs; web/r9_freeze_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared by identity only (assert.ok(a === b, …)).

import { assert, base, settle, cancelDialogs, dialogs, doc, body, El, event } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const UC = await import(new URL('update_confirm.js', base).href);
const { t } = I;
await I.setLocale('zh-CN');

const lastOpen = () => dialogs.filter((d) => d.open).at(-1) || null;
const esc = (d, o = {}) => d.dispatchEvent(event('cancel', { bubbles: false, ...o }));
// a press on the backdrop: on the dialog itself, outside its box (the
// stand-in's box is 0 × 0 at 0, 0)
const backdrop = (d) => d.dispatchEvent(event('mousedown', { target: d, clientX: 5, clientY: 5 }));
// a press inside the box, on a control of the dialog
const inside = (d, el) => d.dispatchEvent(event('mousedown', { target: el, clientX: 0, clientY: 0 }));

// The control that opens a dialog (focused), and a dialog with a text box
// (focused) and a button of its own that closes it with 'mine'.
function opener() {
  const b = new El('button');
  b.dataset.fkey = 'r9:opener';
  body.append(b);
  b.focus();
  return b;
}
function open(opts) {
  const parts = {};
  const p = D.openDialog(
    'R9 dialog',
    (close) => {
      parts.input = D.h('input', { type: 'text' });
      parts.own = D.h('button', { type: 'button', class: 'btn', onclick: () => close('mine') }, 'own');
      return D.h('div', null, parts.input, parts.own);
    },
    opts,
  );
  const d = lastOpen();
  assert.ok(d, 'a dialog is open');
  parts.input.focus();
  let result;
  let resolved = false;
  p.then((v) => {
    resolved = true;
    result = v;
  });
  return { p, d, ...parts, state: () => ({ resolved, result }) };
}

// ---- F1: openDialog opts.confirmCancel ----

{
  // without it: Esc and the backdrop close as before, the focus back on the opener
  for (const [how, opts] of [
    ['Esc, no opts', undefined],
    ['Esc, {}', {}],
  ]) {
    const b = opener();
    const x = open(opts);
    esc(x.d);
    await settle();
    assert.ok(!x.d.open, `${how}: closed`);
    assert.ok(!x.d.isConnected, `${how}: removed`);
    assert.equal(x.state().resolved, true, `${how}: answered`);
    assert.equal(x.state().result, null, `${how}: as cancelled`);
    assert.ok(doc.activeElement === b, `${how}: the focus back on the opener`);
    b.remove();
  }
  {
    const b = opener();
    const x = open();
    inside(x.d, x.input);
    assert.ok(x.d.open, 'no opts: a press inside the box does not close');
    backdrop(x.d);
    await settle();
    assert.ok(!x.d.open, 'no opts: the backdrop closes');
    assert.equal(x.state().result, null, 'no opts: the backdrop cancels');
    assert.ok(doc.activeElement === b, 'no opts: the focus back on the opener after the backdrop');
    b.remove();
  }

  // false (at once): Esc and the backdrop leave it open, asked once each,
  // the focus stays on the text box
  {
    const b = opener();
    let asked = 0;
    const x = open({
      confirmCancel: () => {
        asked++;
        return false;
      },
    });
    const notCancelled = esc(x.d);
    await settle();
    assert.equal(notCancelled, false, 'false: Esc\'s cancel is prevented');
    assert.ok(x.d.open && x.d.isConnected, 'false: Esc leaves it open');
    assert.equal(asked, 1, 'false: Esc asks once');
    assert.ok(doc.activeElement === x.input, 'false: the focus stays on the text box after Esc');
    const pressed = event('mousedown', { target: x.d, clientX: 5, clientY: 5 });
    x.d.dispatchEvent(pressed);
    await settle();
    assert.ok(x.d.open, 'false: the backdrop leaves it open');
    assert.equal(asked, 2, 'false: the backdrop asks once');
    assert.equal(pressed.defaultPrevented, true, 'false: the backdrop\'s press does not move the focus');
    assert.ok(doc.activeElement === x.input, 'false: the focus stays on the text box after the backdrop');
    inside(x.d, x.input);
    assert.equal(asked, 2, 'a press inside the box does not ask');
    assert.equal(x.state().resolved, false, 'false: not answered');
    // the dialog's own close(v) does not ask
    x.own.click();
    await settle();
    assert.equal(asked, 2, 'its own close(v) does not ask');
    assert.ok(!x.d.open, 'its own close(v) closes');
    assert.equal(x.state().result, 'mine', 'its own close(v) answers with its value');
    assert.ok(doc.activeElement === b, 'its own close(v): the focus back on the opener');
    b.remove();
  }

  // a promise: one question at a time; false keeps it open (the focus back
  // on the text box if it moved meanwhile), true closes it and gives the
  // focus back
  {
    const b = opener();
    let asked = 0;
    let reply = null;
    const x = open({
      confirmCancel: () => {
        asked++;
        return new Promise((r) => {
          reply = r;
        });
      },
    });
    esc(x.d);
    esc(x.d);
    backdrop(x.d);
    await settle();
    assert.equal(asked, 1, 'while it answers, Esc and the backdrop do not ask again');
    assert.ok(x.d.open, 'while it answers: open');
    doc.activeElement = body; // the question took the focus away meanwhile
    reply(false);
    await settle();
    assert.ok(x.d.open && x.d.isConnected, 'answered false: still open');
    assert.ok(doc.activeElement === x.input, 'answered false: the focus back on the text box');
    assert.equal(x.state().resolved, false, 'answered false: not answered');
    backdrop(x.d);
    await settle();
    assert.equal(asked, 2, 'after the answer the backdrop asks again');
    reply(true);
    await settle();
    assert.ok(!x.d.open && !x.d.isConnected, 'answered true: closed');
    assert.equal(x.state().resolved, true, 'answered true: answered');
    assert.equal(x.state().result, null, 'answered true: as cancelled');
    assert.ok(doc.activeElement === b, 'answered true: the focus back on the opener');
    b.remove();
  }

  // true at once: closes as without it
  {
    const b = opener();
    let asked = 0;
    const x = open({
      confirmCancel: () => {
        asked++;
        return true;
      },
    });
    esc(x.d);
    await settle();
    assert.equal(asked, 1, 'true: asked once');
    assert.ok(!x.d.open, 'true: closed');
    assert.equal(x.state().result, null, 'true: as cancelled');
    assert.ok(doc.activeElement === b, 'true: the focus back on the opener');
    b.remove();
  }

  // a promise that fails keeps it open
  {
    const b = opener();
    let asked = 0;
    const x = open({
      confirmCancel: () => {
        asked++;
        return Promise.reject(new Error('r9: failed question'));
      },
    });
    esc(x.d);
    await settle();
    assert.equal(asked, 1, 'failed: asked once');
    assert.ok(x.d.open, 'failed: still open');
    x.own.click();
    await settle();
    b.remove();
  }

  // an Esc the browser will not let be cancelled (cancelable false) closes
  // as before, without asking: the dialog cannot be kept open
  {
    const b = opener();
    let asked = 0;
    const x = open({
      confirmCancel: () => {
        asked++;
        return false;
      },
    });
    esc(x.d, { cancelable: false });
    await settle();
    assert.equal(asked, 0, 'not cancelable: not asked');
    assert.ok(!x.d.open, 'not cancelable: closed');
    assert.equal(x.state().result, null, 'not cancelable: as cancelled');
    b.remove();
  }

  // a route change closes it without asking
  {
    const b = opener();
    let asked = 0;
    const x = open({
      confirmCancel: () => {
        asked++;
        return false;
      },
    });
    const was = location.hash;
    location.hash = '#/r9-elsewhere';
    const r = D.closeDialogsOnRoute();
    location.hash = was;
    await settle();
    assert.equal(r.closed, 1, 'route change: one dialog closed');
    assert.equal(asked, 0, 'route change: not asked');
    assert.ok(!x.d.open, 'route change: closed');
    assert.equal(x.state().result, null, 'route change: as cancelled');
    b.remove();
  }
}

// ---- F2: confirmLaunchDrafts opts.courses ----

{
  const drafts = [
    { id: 'L1', title: 'Week 1', course_id: 'c1', duration_ms: 3600000 },
    { id: 'L2', title: 'Week 2', course_id: 'c2', duration_ms: 1800000 },
  ];
  // the dialog's text, cancelled
  async function shown(...extra) {
    const p = UC.confirmLaunchDrafts(drafts, 2, ...extra);
    await settle();
    const d = lastOpen();
    assert.ok(d, 'a dialog');
    const text = d.textContent;
    const paras = d.querySelectorAll('p');
    await cancelDialogs();
    assert.equal(await p, false, 'cancelled');
    return { text, paras };
  }
  const before = await shown();
  const line = (codes) => t('course.confirm.launch_courses', { n: codes.length, courses: codes.join(t('common.list_sep')) });

  // two courses: the line, with both codes, a hint after the stages and
  // before the cost sentence
  const two = await shown({ courses: ['CSC3631', 'MTH2001'] });
  const want = line(['CSC3631', 'MTH2001']);
  assert.ok(want !== 'course.confirm.launch_courses' && want.includes('CSC3631') && want.includes('MTH2001'), 'the key names both courses');
  const at = two.paras.findIndex((p) => p.textContent === want);
  assert.ok(at >= 0, 'two courses: the line is shown');
  assert.ok(/(^|\s)dlg-hint(\s|$)/.test(two.paras[at].className), 'two courses: the line is a hint');
  const stagesAt = two.paras.findIndex((p) => /(^|\s)dlg-msg(\s|$)/.test(p.className));
  const costAt = two.paras.findIndex((p) => p.textContent === t('course.confirm.launch_cost'));
  assert.ok(stagesAt >= 0 && costAt >= 0, 'the stages and the cost sentence are shown');
  assert.ok(stagesAt < at && at < costAt, 'two courses: after the stages, before the cost sentence');
  assert.equal(two.text.split(want).length, 2, 'two courses: the line once');
  assert.equal(two.text.replace(want, ''), before.text, 'two courses: the rest as before');

  // three, with the cost mode: the line and the mode's sentence
  const three = await shown({ courses: ['A1', 'B2', 'Course without code'], costMode: 'free' });
  assert.ok(three.text.includes(line(['A1', 'B2', 'Course without code'])), 'three courses: the line');
  assert.ok(three.text.includes(t('course.confirm.launch_cost_free')), 'three courses: the cost mode still applies');

  // one, none, not given: as before
  for (const [extra, what] of [
    [[], 'not given'],
    [[{}], 'no courses'],
    [[null], 'null opts'],
    [[{ courses: [] }], 'empty'],
    [[{ courses: ['CSC3631'] }], 'one course'],
    [[{ courses: 'CSC3631, MTH2001' }], 'not a list'],
  ]) {
    const s = await shown(...extra);
    assert.equal(s.text, before.text, `${what}: as before`);
    assert.equal(s.paras.length, before.paras.length, `${what}: no other line`);
  }

  // en: the plural form, the list joined by its separator
  await I.setLocale('en');
  const en = await shown({ courses: ['CSC3631', 'MTH2001'] });
  assert.ok(en.text.includes('From 2 courses: CSC3631, MTH2001.'), 'en: "From 2 courses: CSC3631, MTH2001."');
  await I.setLocale('zh-CN');
  const zh = await shown({ courses: ['CSC3631', 'MTH2001'] });
  assert.ok(zh.text.includes('来自 2 门课：CSC3631、MTH2001。'), 'zh-CN: "来自 2 门课：CSC3631、MTH2001。"');
}

console.log('r9 freeze checks: ok');
