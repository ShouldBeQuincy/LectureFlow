// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// T-PERF-03: the observed account limits of an Anthropic provider in the
// provider dialog and row (provider_limits.js) — with data and the
// settings below the suggestion, with data and the settings enough, and
// without data; "填入建议值" only fills the two fields (an input event
// each, no request); the fields are described by the text only while it
// shows (the text only, not the button), and never while a new key is
// typed; the row banner says which setting is low and waits for a
// measured answer (review R38). Rendered on a small DOM stand-in with the zh-CN catalog; fetch
// answers the catalog files only and fails the test on anything else.
// web/provider_limits_test.go starts it with the web directory as a file:
// URL.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const base = process.argv[2];

// ---- DOM stand-in ----

class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  removeEventListener() {}
  dispatchEvent(ev) {
    for (const fn of [...(this.listeners[ev.type] || [])]) fn(ev);
    return true;
  }
}
class Text extends Node {
  constructor(s) {
    super();
    this.data = String(s);
    this.children = [];
  }
  get textContent() {
    return this.data;
  }
}
class El extends Node {
  constructor(tag) {
    super();
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.attrs = {};
    this.dataset = {};
    this.style = {};
    this.className = '';
    this.hidden = false;
    this.value = '';
  }
  append(...cs) {
    for (const c of cs) {
      const n = typeof c === 'string' ? new Text(c) : c;
      n.parentNode = this;
      this.children.push(n);
    }
  }
  replaceChildren(...cs) {
    this.children = [];
    this.append(...cs);
  }
  set textContent(v) {
    this.replaceChildren(new Text(v));
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  setAttribute(k, v) {
    if (k === 'class') this.className = String(v);
    else this.attrs[k] = String(v);
  }
  getAttribute(k) {
    if (k === 'id' && this.id) return this.id;
    return k in this.attrs ? this.attrs[k] : null;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  get id() {
    return this.attrs.id;
  }
  // depth-first search, bounded by the tree (no cycles in the stand-in)
  all(pred, out = []) {
    for (const c of this.children) {
      if (c instanceof El) {
        if (pred(c)) out.push(c);
        c.all(pred, out);
      }
    }
    return out;
  }
}
globalThis.Node = Node;
const store = { getItem: () => null, setItem() {}, removeItem() {} };
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: {
    documentElement: new El('html'),
    body: new El('body'),
    addEventListener() {},
    querySelector: () => null,
    getElementById: () => null,
    createElement: (tag) => new El(tag),
    createElementNS: (_, tag) => new El(tag),
    createTextNode: (s) => new Text(s),
  },
  addEventListener() {},
  dispatchEvent() {
    return true;
  },
  matchMedia: () => ({ matches: false, addEventListener() {} }),
});
globalThis.window = globalThis;
let otherFetches = 0;
globalThis.fetch = async (url) => {
  const m = /^i18n\/([\w-]+)\.json$/.exec(String(url));
  if (!m) {
    otherFetches++;
    throw new Error('unexpected fetch ' + url);
  }
  const text = await readFile(new URL(`i18n/${m[1]}.json`, base), 'utf8');
  return { ok: true, json: async () => JSON.parse(text) };
};

const I = await import(new URL('i18n.js', base).href);
await I.setLocale('zh-CN');
const L = await import(new URL('provider_limits.js', base).href);

const field = (value) => {
  const el = new El('input');
  el.value = value;
  return el;
};
const buttons = (root) => root.all((e) => e.tagName === 'BUTTON');
const described = (root) => {
  const found = root.all((e) => e.getAttribute('id') === L.LIMITS_HINT_ID);
  assert.equal(found.length, 1, 'one described text');
  return found[0];
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const nowISO = new Date(Date.now() - 3 * 60 * 1000).toISOString();
const limits = (advice) => ({
  model: 'claude-opus-5-5',
  requests_per_minute: 4000,
  input_tokens_per_minute: 2000000,
  output_tokens_per_minute: 400000,
  observed_at: nowISO,
  advice,
});
const OFFICIAL = 'https://api.anthropic.com';
const prov = (conc, rpm, advice) => ({ id: 'a', type: 'anthropic', base_url: OFFICIAL, max_concurrency: conc, rpm, rate_limits: limits(advice) });
const below = prov(6, 100, { max_concurrency: 23, rpm: 600, below: true, concurrency_below: true, rpm_below: true, measured: true });
const enough = prov(32, 0, { max_concurrency: 32, rpm: 0, below: false, concurrency_below: false, rpm_below: false, measured: true });
const none = { id: 'a', type: 'anthropic', base_url: OFFICIAL, max_concurrency: 4, rpm: 0 };

// ---- states ----
assert.equal(L.rateLimitState(below), 'below');
assert.equal(L.rateLimitState(enough), 'ok');
assert.equal(L.rateLimitState(none), 'none');
assert.equal(L.rateLimitState(null), 'none');
assert.equal(L.rateLimitState({ ...below, type: 'openai' }), 'none', 'only Anthropic providers');
assert.equal(L.rateLimitState({ ...below, rate_limits: { ...below.rate_limits, requests_per_minute: 0 } }), 'none');

// ---- with data, settings below the suggestion ----
{
  const conc = field('6');
  const rpm = field('100');
  conc.setAttribute('aria-describedby', 'other-hint');
  const inputs = [];
  for (const f of [conc, rpm]) f.addEventListener('input', () => inputs.push(f));
  const { el, sync } = L.createLimitsHint(below, conc, rpm);
  sync('anthropic', false, OFFICIAL);
  assert.equal(el.hidden, false);
  const text = el.textContent;
  for (const want of [
    '观察到 claude-opus-5-5 的限额：每分钟 4,000 次请求（',
    '每分钟 token 上限：输入 2,000,000，输出 400,000。',
    '建议：并发 23，每分钟 600 次。',
    '不是保证',
    '建议逐步调高',
    '加速限额（429）',
  ]) {
    assert.ok(text.includes(want), `below: ${want} in ${text}`);
  }
  assert.ok(!text.includes('尚无实测'), 'measured: no estimate note');
  assert.ok(!text.includes('settings.provider'), `no raw keys: ${text}`);
  // R38 S5: the fields are described by the text only, not the button or status
  const d = described(el);
  assert.ok(!d.textContent.includes('填入建议值'), `the description holds no button name: ${d.textContent}`);
  assert.ok(d.textContent.includes('建议：并发 23'), d.textContent);
  assert.equal(conc.getAttribute('aria-describedby'), `other-hint ${L.LIMITS_HINT_ID}`, 'concurrency described by the text, its own hint kept');
  assert.equal(rpm.getAttribute('aria-describedby'), L.LIMITS_HINT_ID);
  const bs = buttons(el);
  assert.equal(bs.length, 1);
  assert.equal(bs[0].getAttribute('type'), 'button', 'a real button (keyboard), not a submit');
  assert.ok(bs[0].textContent.includes('填入建议值'));
  const status = el.all((e) => e.getAttribute('role') === 'status');
  assert.equal(status.length, 1);
  assert.equal(status[0].textContent, '');
  // the values are untouched until the click
  assert.equal(conc.value, '6');
  bs[0].dispatchEvent({ type: 'click' });
  assert.equal(conc.value, '23');
  assert.equal(rpm.value, '600');
  assert.equal(inputs.length, 2, 'one input event per field (the dialog marks them as edited)');
  assert.ok(inputs[0] === conc && inputs[1] === rpm, 'the input events come from the two fields');
  assert.ok(status[0].textContent.includes('点“保存”后才会生效'), status[0].textContent);
  assert.equal(otherFetches, 0, 'filling sends no request');
  // switching the dialog to the OpenAI type hides it and drops the description
  sync('openai', false, OFFICIAL);
  assert.equal(el.hidden, true);
  assert.equal(conc.getAttribute('aria-describedby'), 'other-hint');
  assert.equal(rpm.getAttribute('aria-describedby'), null);
  sync('anthropic', false, OFFICIAL);
  assert.equal(conc.getAttribute('aria-describedby'), `other-hint ${L.LIMITS_HINT_ID}`, 'not added twice');
  // R38 S6: a new key typed in hides the old key's limits and suggestion
  sync('anthropic', true, OFFICIAL);
  assert.equal(el.hidden, true);
  assert.equal(conc.getAttribute('aria-describedby'), 'other-hint');
  sync('anthropic', false, OFFICIAL);
  assert.equal(el.hidden, false);
}

// opened from the row's banner: filled at once, the status written once
// the dialog shows (R38 S5)
{
  const conc = field('6');
  const rpm = field('100');
  const { el } = L.createLimitsHint(below, conc, rpm, { filled: true });
  assert.equal(conc.value, '23');
  assert.equal(rpm.value, '600');
  const status = el.all((e) => e.getAttribute('role') === 'status')[0];
  assert.equal(status.textContent, '', 'empty while the dialog is built');
  await sleep(L.ANNOUNCE_DELAY_MS + 80);
  assert.ok(status.textContent.includes('点“保存”后才会生效'), status.textContent);
}

// not measured yet (R38 S1): the dialog says the suggestion is an estimate
// from the request limit; no row banner
{
  const est = prov(6, 100, { max_concurrency: 32, rpm: 2400, below: true, concurrency_below: true, rpm_below: true, measured: false });
  const { el, sync } = L.createLimitsHint(est, field('6'), field('100'));
  sync('anthropic', false, OFFICIAL);
  assert.ok(el.textContent.includes('尚无实测的耗时与 token 数，这个建议只按每分钟请求数估算。'), el.textContent);
  assert.equal(buttons(el).length, 1, 'the dialog still offers the suggestion');
  assert.equal(L.limitsBanner(est, () => {}), null, 'no banner before a measured answer');
}

// ---- with data, settings enough ----
{
  const conc = field('32');
  const rpm = field('0');
  const { el, sync } = L.createLimitsHint(enough, conc, rpm, { filled: true });
  sync('anthropic', false, OFFICIAL);
  const text = el.textContent;
  assert.ok(text.includes('每分钟 4,000 次请求'), text);
  assert.ok(!text.includes('建议：') && !text.includes('逐步调高'), `no suggestion when the settings are enough: ${text}`);
  assert.equal(buttons(el).length, 0);
  assert.equal(conc.value, '32', 'never filled');
  assert.equal(rpm.value, '0');
  assert.equal(L.limitsBanner(enough, () => {}), null, 'no row banner');
}

// ---- without data ----
{
  const conc = field('4');
  const rpm = field('0');
  const { el, sync } = L.createLimitsHint(none, conc, rpm);
  sync('anthropic', false, OFFICIAL);
  assert.equal(el.hidden, false);
  assert.ok(el.textContent.includes('发出过请求之后，这里会显示观察到的模型限额。'), el.textContent);
  assert.equal(buttons(el).length, 0);
  assert.equal(L.limitsBanner(none, () => {}), null, 'no row banner without data');
  // a new provider: nothing to say, hidden and not describing anything
  const fresh = L.createLimitsHint(null, conc, rpm);
  fresh.sync('anthropic', false, OFFICIAL);
  assert.equal(fresh.el.hidden, true);
  assert.equal(conc.getAttribute('aria-describedby'), null);
}

// ---- the row's banner: says which setting is low (R38 M1) ----
{
  // Anthropic's default: 4 at a time, no per-minute limit
  const dflt = prov(4, 0, { max_concurrency: 23, rpm: 0, below: true, concurrency_below: true, rpm_below: false, measured: true });
  let opened = 0;
  const b = L.limitsBanner(dflt, () => opened++);
  assert.ok(b, 'a banner when below');
  const bt = b.textContent;
  assert.ok(bt.includes('观察到 claude-opus-5-5 的限额是每分钟 4,000 次请求，当前的并发（4）明显偏低'), bt);
  assert.ok(!bt.includes('每分钟次数（'), `no per-minute limit to call low: ${bt}`);
  assert.ok(bt.includes('建议：并发 23，每分钟次数保持不限。'), bt);
  assert.ok(b.style.flexWrap === 'wrap', 'wraps on narrow screens');
  const bs = buttons(b);
  assert.equal(bs.length, 1);
  bs[0].dispatchEvent({ type: 'click' });
  assert.equal(opened, 1, 'the button opens the dialog (it saves nothing)');

  // only the per-minute limit is low
  const rpmLow = prov(32, 100, { max_concurrency: 32, rpm: 600, below: true, concurrency_below: false, rpm_below: true, measured: true });
  const rt = L.limitsBanner(rpmLow, () => {}).textContent;
  assert.ok(rt.includes('当前的每分钟次数（100）明显偏低'), rt);
  assert.ok(!rt.includes('当前的并发（'), rt);
  assert.ok(rt.includes('建议：并发 32，每分钟 600 次。'), rt);

  // both
  const both = L.limitsBanner(below, () => {}).textContent;
  assert.ok(both.includes('当前的并发（6）与每分钟次数（100）都明显偏低'), both);
}
// ---- only Anthropic's own API host over https (R38b N1, N2; the same
// rule as llm.OfficialAnthropicHost) ----
{
  for (const [u, want] of [
    ['https://api.anthropic.com', true],
    ['https://API.Anthropic.com:443/', true],
    [' https://api.anthropic.com/v1 ', true],
    ['http://api.anthropic.com', false],
    ['https://api.anthropic.com.example.com', false],
    ['https://gateway.example.com/anthropic', false],
    ['https://api.deepseek.com/anthropic', false],
    ['api.anthropic.com', false],
    ['', false],
    [null, false],
  ]) {
    assert.equal(L.officialAnthropicHost(u), want, String(u));
  }
  // a provider at a gateway: no limits text at all (it would promise
  // limits that never come), not described, no banner
  const gw = { ...none, base_url: 'https://gateway.example.com/anthropic' };
  assert.equal(L.rateLimitState({ ...below, base_url: 'https://gateway.example.com' }), 'none');
  assert.equal(L.limitsBanner({ ...below, base_url: 'http://api.anthropic.com' }, () => {}), null, 'no banner over http');
  const conc = field('4');
  const rpm = field('0');
  const { el, sync } = L.createLimitsHint(gw, conc, rpm);
  sync('anthropic', false, gw.base_url);
  assert.equal(el.hidden, true, 'hidden at a gateway');
  assert.equal(conc.getAttribute('aria-describedby'), null);
  // the address typed over to the official host and back
  const b = L.createLimitsHint(none, conc, rpm);
  b.sync('anthropic', false, OFFICIAL);
  assert.equal(b.el.hidden, false);
  assert.equal(conc.getAttribute('aria-describedby'), L.LIMITS_HINT_ID);
  b.sync('anthropic', false, 'https://gateway.example.com');
  assert.equal(b.el.hidden, true, 'hidden while the address points elsewhere');
  assert.equal(conc.getAttribute('aria-describedby'), null);
  b.sync('anthropic', false, 'http://api.anthropic.com');
  assert.equal(b.el.hidden, true, 'hidden over http');
  b.sync('anthropic', false, 'https://api.anthropic.com/');
  assert.equal(b.el.hidden, false, 'shown again');
}
// ---- r4 A3: the sentences are joined by the UI language's rule: nothing
// after a full-width stop (zh-CN), one space after ". " (en) and "। " (hi);
// the strings are compared whole ----
{
  const U = await import(new URL('util.js', base).href);
  const est = prov(6, 100, { max_concurrency: 23, rpm: 600, below: true, concurrency_below: true, rpm_below: true, measured: false });
  for (const [loc, sep] of [
    ['zh-CN', ''],
    ['en', ' '],
    ['hi', ' '],
  ]) {
    await I.setLocale(loc);
    const k = (key, p) => I.t(`settings.provider.rate_limits.${key}`, p);
    const observed = k('observed', { rpm: U.fmtInt(4000), model: 'claude-opus-5-5', when: U.relTime(nowISO) });
    const tokens = k('tokens', { input: U.fmtInt(2000000), output: U.fmtInt(400000) });
    const advice = k('advice', { max: U.fmtInt(23), rpm: U.fmtInt(600) });
    for (const s of [observed, tokens, advice, k('estimated'), k('caveat'), k('gradual')]) {
      assert.ok(s && !s.includes('settings.provider') && !/\s$/.test(s), `${loc}: a catalog sentence: ${s}`);
    }
    const want = [observed, tokens, advice, k('estimated'), k('caveat'), k('gradual')].join(sep);
    const { el } = L.createLimitsHint(est, field('6'), field('100'));
    const got = described(el).textContent;
    assert.equal(got, ' ' + want, `${loc}: the dialog's sentences`);
    const banner = L.limitsBanner(below, () => {});
    const bannerText = banner.all((e) => (e.className || '').includes('set-banner-text'))[0].textContent;
    assert.equal(bannerText, L.belowText(below) + sep + L.adviceText(below.rate_limits.advice), `${loc}: the banner's sentences`);
    if (loc === 'zh-CN') assert.ok(!/[。！？] /u.test(got + bannerText), `zh-CN: no space after a full stop: ${got} | ${bannerText}`);
    if (loc === 'hi') assert.ok(got.includes('। '), `hi: a space after "।": ${got}`);
  }
  await I.setLocale('zh-CN');
}

assert.equal(otherFetches, 0);

console.log('provider limits checks: ok');
