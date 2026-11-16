// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x P1-c / P1-b: the stage hover card shows what a run's LLM requests
// are doing (StageView.llm, last_activity_at): the attempt with its model and
// output limit, the countdown to the next attempt, the last outcome, the
// tokens of each attempt, the last activity and the time a request has
// waited; job events that carry stats update it without reading the
// lecture; the model line names a model chosen for one retry; "换模型重试…"
// sits next to "重试" of a failed or cancelled summary, repair or translate
// stage. pipeline.js runs on the DOM stand-in of pipeline_remote_test.mjs
// (copied); web/stage_card_llm_test.go starts it with the web directory as a
// file: URL.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

class Node {}
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
    this.listeners = {};
    const self = this;
    this.classList = {
      toggle(c, on) {
        const set = new Set(self.className.split(/\s+/).filter(Boolean));
        if (on === undefined ? !set.has(c) : on) set.add(c);
        else set.delete(c);
        self.className = [...set].join(' ');
      },
      add(c) {
        this.toggle(c, true);
      },
      remove(c) {
        this.toggle(c, false);
      },
      contains(c) {
        return self.className.split(/\s+/).includes(c);
      },
    };
  }
  set textContent(v) {
    this.children = [new Text(v)];
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join('');
  }
  setAttribute(k, v) {
    if (k === 'class') this.className = String(v);
    else this.attrs[k] = String(v);
  }
  getAttribute(k) {
    return k in this.attrs ? this.attrs[k] : null;
  }
  removeAttribute(k) {
    delete this.attrs[k];
  }
  append(...cs) {
    for (const c of cs) this.children.push(c instanceof Node ? c : new Text(c));
  }
  replaceChildren(...cs) {
    this.children = [];
    this.append(...cs);
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  dispatch(type) {
    for (const fn of this.listeners[type] || []) fn({});
  }
  getBoundingClientRect() {
    return { top: 0, bottom: 0 };
  }
  matches(sel) {
    const m = /^([a-z]*)((?:\.[\w-]+)*)(?:\[([\w-]+)(?:="([^"]*)")?\])?$/.exec(sel);
    if (!m) throw new Error('selector not supported here: ' + sel);
    const [, tag, classes, attr, val] = m;
    if (tag && this.tagName !== tag.toUpperCase()) return false;
    const own = this.className.split(/\s+/);
    for (const c of classes.split('.').filter(Boolean)) if (!own.includes(c)) return false;
    if (attr) {
      const v = attr.startsWith('data-') ? this.dataset[attr.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] : this.getAttribute(attr);
      if (v == null || (val !== undefined && v !== val)) return false;
    }
    return true;
  }
  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => {
      for (const c of n.children) {
        if (c instanceof El) {
          if (c.matches(sel)) out.push(c);
          walk(c);
        }
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
}

const store = { getItem: () => null, setItem() {}, removeItem() {} };
const root = new El('html');
Object.assign(globalThis, {
  Node,
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: {
    documentElement: root,
    body: new El('body'),
    addEventListener() {},
    querySelector: () => null,
    getElementById: () => null,
    createElement: (t) => new El(t),
    createElementNS: (_, t) => new El(t),
    createTextNode: (s) => new Text(s),
    visibilityState: 'visible',
  },
  addEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  innerHeight: 900,
  CSS: { escape: (s) => String(s) },
});
globalThis.window = globalThis;

const base = process.argv[2];

// fetch: the UI catalogs from the web directory; nothing else is read here
// (a card that reads the lecture again would show up in `reads`).
const reads = [];
globalThis.fetch = async (url) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const body = await readFile(new URL(u, base), 'utf8');
    return { ok: true, status: 200, json: async () => JSON.parse(body), text: async () => body };
  }
  reads.push(u);
  return { ok: true, status: 200, text: async () => 'null' };
};

const P = await import(new URL('pipeline.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const U = await import(new URL('util.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;

// A fake clock (the countdown, the last activity, the time waited).
let clock = Date.parse('2026-11-11T00:34:00Z');
Date.now = () => clock;
const iso = (ms) => new Date(ms).toISOString();

// The value of a card line (null: no such line; 'hidden': hidden).
function line(el, cls) {
  const l = el.querySelector(`.${cls}`);
  if (!l) return null;
  return l.style.display === 'none' ? 'hidden' : l.querySelector('.card-v').textContent;
}
const stage = (stage, status, extra) => ({
  stage, status, display_status: status, progress: status === 'running' ? 0.3 : 0, attempt: 0, error: null,
  allowed_actions: [], started_at: status === 'running' ? iso(clock - 60000) : null, finished_at: null, duration_ms: null, stats: null, ...extra,
});

// ---- the texts ----
{
  assert.notEqual(t('common.pipe.attempt_n', { n: 2 }), 'common.pipe.attempt_n', 'the catalog is loaded');
  assert.equal(P.attemptHead({ n: 2, model: 'claude-opus-5-5', max_tokens: 32768, thinking: 'high' }),
    [t('common.pipe.attempt_n', { n: 2 }), 'claude-opus-5-5', t('common.pipe.max_tokens', { value: U.fmtInt(32768) }), t('common.model.thinking_high')].join(' · '));
  // no limit sent, thinking off: neither is named
  assert.equal(P.attemptHead({ n: 1, model: 'deepseek-chat', thinking: '' }), [t('common.pipe.attempt_n', { n: 1 }), 'deepseek-chat'].join(' · '));
  assert.equal(t('common.pipe.attempt_n', { n: 2 }), '第 2 次尝试');

  // the countdown: seconds rounded up, the next attempt's number; due at 0
  const cur = { n: 2, retry_at: iso(clock + 11200) };
  assert.equal(P.retryCountdown(cur, clock), '12 秒后进行第 3 次尝试');
  assert.equal(P.retryCountdown(cur, clock + 11200), '即将进行第 3 次尝试');
  assert.equal(P.retryCountdown({ n: 2 }, clock), '');

  // outcomes
  assert.equal(P.outcomeText({ outcome: 'truncated' }), '输出被截断');
  assert.equal(P.outcomeText({ outcome: 'truncated', empty_text: true }), '额度被思考用完');
  assert.equal(P.outcomeText({ outcome: 'http_error', status: 529 }), 'HTTP 529');
  for (const [o, want] of [['timeout', '超时'], ['network', '网络错误'], ['idle', '空闲超时'], ['refused', '被拒答'], ['bad_response', '返回内容无效'], ['canceled', '已取消'], ['ok', '成功']]) assert.equal(P.outcomeText({ outcome: o }), want, o);
  assert.equal(P.outcomeText({ outcome: 'something_new' }), 'something_new', 'an unknown outcome as it came');
  assert.equal(P.lastOutcomeText({ attempts: [{ outcome: 'http_error', status: 503 }, { outcome: 'truncated' }] }), '上次：输出被截断');
  assert.equal(P.lastOutcomeText({ attempts: [{ outcome: 'truncated' }, { outcome: 'ok' }] }), '', 'the last one went through');
  assert.equal(P.lastOutcomeText(null), '');

  // the last activity: seconds, then minutes and seconds
  assert.equal(P.agoText(iso(clock - 42000), clock), '42 秒前');
  assert.equal(P.agoText(iso(clock - 185000), clock), '3 分 5 秒前');
  assert.equal(P.agoText(iso(clock + 3000), clock), '0 秒前', 'a clock a little ahead: never negative');
  assert.equal(P.agoText(undefined, clock), '');

  // the time a request has waited, and its timeout
  assert.equal(P.waitedText({ started_at: iso(clock - 130000), timeout_ms: 1104000 }, clock), `已等待 ${U.fmtClock(130000)} / 超时 ${U.fmtClock(1104000)}`);
  assert.equal(P.waitedText({ started_at: iso(clock - 1000) }, clock), '', 'no timeout: no line');

  // the tokens of one attempt
  assert.equal(P.attemptTokensText({ n: 1, prompt_tokens: 1840, completion_tokens: 2000, outcome: 'truncated' }),
    t('common.pipe.attempt_tokens', { n: 1, input: U.fmtTokens(1840), output: U.fmtTokens(2000), cached: U.fmtTokens(0), result: '输出被截断' }));
}

// ---- the card ----
{
  // A job without the v3.2.x fields: no new lines (the card is as before).
  const bar = P.pipelineBar([stage('summary', 'running', { stats: { prompt_tokens: 10, completion_tokens: 5 } })], { key: 'lec:A', sys: () => null, res: () => null });
  assert.equal(bar.querySelector('.card-llm').children.length, 0);
  assert.equal(bar.querySelector('.card-attempt-tokens').children.length, 0);
  assert.equal(line(bar, 'card-activity'), null);
}

{
  // One request (summary): attempt, time waited, last activity; it follows the clock.
  const st = stage('summary', 'running', {
    llm: { in_flight: 1, current: { n: 2, model: 'claude-opus-5-5', max_tokens: 32768, thinking: 'high', started_at: iso(clock - 20000), timeout_ms: 1104000 }, attempts: [{ n: 1, model: 'claude-opus-5-5', outcome: 'truncated', empty_text: true, prompt_tokens: 18220, completion_tokens: 16000 }], retries: 1 },
    last_activity_at: iso(clock - 20000),
  });
  const bar = P.pipelineBar([st], { key: 'lec:B', sys: () => null, res: () => null });
  assert.equal(line(bar, 'card-attempt'), P.attemptHead(st.llm.current));
  assert.equal(line(bar, 'card-last'), '上次：额度被思考用完');
  assert.equal(line(bar, 'card-activity'), '20 秒前');
  assert.equal(line(bar, 'card-waited'), P.waitedText(st.llm.current, clock));
  assert.ok(bar.querySelector('.card-per-attempt').textContent.includes('额度被思考用完'), 'the tokens of each attempt');
  clock += 65000;
  bar.querySelector('.seg-wrap[data-stage="summary"]').dispatch('pointerenter');
  assert.equal(line(bar, 'card-activity'), '1 分 25 秒前', 'recomputed when the card is drawn again');
  // streaming: what it is doing
  st.llm.current.phase = 'thinking';
  bar.setProgress('summary', 0.4);
  assert.ok(bar.querySelector('.card-phase').textContent.includes('正在思考'));
  // a done stage keeps its attempts but no last activity
  const done = P.pipelineBar([{ ...st, status: 'done', display_status: 'done', llm: { attempts: st.llm.attempts, retries: 1 } }], { key: 'lec:B', sys: () => null, res: () => null });
  assert.equal(line(done, 'card-activity'), null);
  assert.equal(line(done, 'card-attempt'), null);
  assert.equal(line(done, 'card-last'), '上次：额度被思考用完');
}

{
  // Many requests (translate) waiting out a backoff: their number, the
  // retries, the countdown and the last outcome; no time waited.
  const st = stage('translate', 'running', {
    llm: { in_flight: 2, current: { n: 2, model: 'claude-sonnet-4-5', max_tokens: 2000, started_at: iso(clock - 1000), timeout_ms: 120000, retry_at: iso(clock + 8000) }, attempts: [{ n: 1, outcome: 'http_error', status: 429 }], retries: 2 },
    last_activity_at: iso(clock - 1000),
  });
  const bar = P.pipelineBar([st], { key: 'lec:C', sys: () => null, res: () => null });
  assert.equal(line(bar, 'card-inflight'), '进行中 2 个请求 · 已重试 2 次');
  assert.equal(line(bar, 'card-countdown'), '8 秒后进行第 3 次尝试');
  assert.equal(line(bar, 'card-last'), '上次：HTTP 429');
  assert.equal(line(bar, 'card-waited'), null);
  assert.equal(line(bar, 'card-attempt'), null);
  clock += 3000;
  bar.setProgress('translate', 0.31);
  assert.equal(line(bar, 'card-countdown'), '5 秒后进行第 3 次尝试', 'counts down');
  // pending sentences (when the view carries them)
  const pend = P.pipelineBar([{ ...stage('translate', 'done'), stats: { prompt_tokens: 1, completion_tokens: 1, pending_sentences: 3 } }], { key: 'lec:C', sys: () => null, res: () => null });
  assert.equal(line(pend, 'card-pending'), '3 句待处理');
}

{
  // The model line: the output limit and "临时指定".
  const st = stage('summary', 'done', { model: { label: 'x', provider_name: 'Anthropic', model: 'claude-opus-5-5', params: { temperature: 0.2, max_tokens: 32768 }, source: 'manual' } });
  const bar = P.pipelineBar([st], { key: 'lec:D', sys: () => null, res: () => null });
  const ln = bar.querySelector('.card-manual');
  assert.ok(ln && ln.textContent === '临时指定', 'the manual tag');
  const all = bar.querySelector('.seg-card').textContent;
  assert.ok(all.includes(t('common.pipe.max_tokens', { value: U.fmtInt(32768) })), all);
  const plain = P.pipelineBar([{ ...st, model: { ...st.model, source: undefined, params: { temperature: 0.2 } } }], { key: 'lec:D', sys: () => null, res: () => null });
  assert.equal(plain.querySelector('.card-manual'), null);
  assert.ok(!plain.querySelector('.seg-card').textContent.includes('max_tokens'));
}

{
  // "换模型重试…" next to "重试" of a failed or cancelled LLM stage only.
  assert.deepEqual(P.cardActions(stage('summary', 'failed', { allowed_actions: ['retry'] })), ['retry', 'retry_model']);
  assert.deepEqual(P.cardActions(stage('translate', 'cancelled', { allowed_actions: ['retry'] })), ['retry', 'retry_model']);
  assert.deepEqual(P.cardActions(stage('repair', 'failed', { allowed_actions: ['retry', 'rerun'] })), ['retry', 'retry_model', 'rerun']);
  assert.deepEqual(P.cardActions(stage('asr', 'failed', { allowed_actions: ['retry'] })), ['retry']);
  assert.deepEqual(P.cardActions(stage('summary', 'done', { allowed_actions: ['rerun'] })), ['rerun']);
  assert.deepEqual(P.cardActions(stage('summary', 'running', { allowed_actions: ['cancel'] })), ['cancel']);
  const actions = [];
  const bar = P.pipelineBar([stage('summary', 'failed', { allowed_actions: ['retry'] })], { key: 'lec:E', sys: () => null, res: () => null, onAction: (s, a) => actions.push(a) });
  assert.ok(bar.querySelector('button[data-fkey="lec:E:act:summary:retry_model"]'), 'the button is drawn');
  assert.equal(I.t('common.action.retry_model'), '换模型重试…');
}

{
  // A job event with stats goes into the bars of its lecture, without a read.
  const st = stage('translate', 'running', { stats: { prompt_tokens: 0, completion_tokens: 0 } });
  const bar = P.pipelineBar([st], { key: 'lec:F', lecture: 'F', sys: () => null, res: () => null });
  const other = stage('translate', 'running');
  P.pipelineBar([other], { key: 'lec:G', lecture: 'G', sys: () => null, res: () => null });
  const ev = {
    scope: 'lecture', scope_id: 'F', stage: 'translate', status: 'running', progress: 0.3,
    stats: { prompt_tokens: 1200, cache_hit_tokens: 0, completion_tokens: 300, estimated_cost: null },
    llm: { in_flight: 1, current: { n: 1, model: 'claude-sonnet-4-5', started_at: iso(clock - 2000), timeout_ms: 120000 } },
    last_activity_at: iso(clock - 2000),
  };
  P.mergeJobEvent(ev);
  assert.equal(st.stats.prompt_tokens, 1200);
  assert.equal(line(bar, 'card-tokens'), U.usageText(ev.stats));
  assert.equal(line(bar, 'card-attempt'), P.attemptHead(ev.llm.current));
  assert.equal(line(bar, 'card-activity'), '2 秒前');
  assert.equal(other.llm, undefined, 'another lecture is not touched');
  // a later stats event without llm: the request lines go
  P.mergeJobEvent({ ...ev, stats: { ...ev.stats, prompt_tokens: 1500 }, llm: undefined, last_activity_at: undefined });
  assert.equal(st.llm, undefined);
  assert.equal(line(bar, 'card-attempt'), null);
  assert.equal(line(bar, 'card-activity'), null);
  // events without these fields, or of another status, change nothing
  P.mergeJobEvent({ scope: 'lecture', scope_id: 'F', stage: 'translate', status: 'running', progress: 0.5 });
  assert.equal(st.stats.prompt_tokens, 1500);
  P.mergeJobEvent({ ...ev, status: 'done' });
  assert.equal(st.llm, undefined, 'a status change is left to the view');
  P.mergeJobEvent({ ...ev, scope: 'course' });
  assert.equal(st.llm, undefined);
  assert.deepEqual(reads, []);
}

{
  // v3.2.x P1-b: a translate stage whose translations come from more than
  // one model (stats.mixed_models) says so in a neutral line; views without
  // the field (older jobs), one model or another stage: no line.
  const models = ['DeepSeek / deepseek-chat', 'Anthropic / claude-sonnet-4-5'];
  const mixed = stage('translate', 'done', { stats: { prompt_tokens: 1, completion_tokens: 1, mixed_models: models } });
  assert.equal(P.mixedModelsText(mixed), '这节讲座的译文来自不同的模型：DeepSeek / deepseek-chat、Anthropic / claude-sonnet-4-5');
  const bar = P.pipelineBar([mixed], { key: 'lec:M', sys: () => null, res: () => null });
  const note = bar.querySelector('.card-mixed-models');
  assert.ok(note, 'the mixed models line');
  assert.equal(note.textContent, P.mixedModelsText(mixed));
  assert.ok(!note.className.includes('warn') && !note.className.includes('stale'), note.className);
  for (const st of [
    stage('translate', 'done', { stats: { prompt_tokens: 1, completion_tokens: 1 } }),
    stage('translate', 'done', { stats: { prompt_tokens: 1, completion_tokens: 1, mixed_models: [models[0]] } }),
    stage('translate', 'done'),
    stage('repair', 'done', { stats: { prompt_tokens: 1, completion_tokens: 1, mixed_models: models } }),
  ]) {
    assert.equal(P.mixedModelsText(st), '');
    const b = P.pipelineBar([st], { key: 'lec:M2', sys: () => null, res: () => null });
    assert.equal(b.querySelector('.card-mixed-models'), null);
  }
  assert.equal(P.mixedModelsText(undefined), '');
  // English
  await I.setLocale('en');
  assert.equal(P.mixedModelsText(mixed), "This lecture's translations come from different models: DeepSeek / deepseek-chat, Anthropic / claude-sonnet-4-5");
  await I.setLocale('zh-CN');
}

{
  // A stage still truncated after the ladder: the limits it tried, under the error.
  const info = { code: 'llm.truncated', params: { role: 'summary', retries: 0, provider: 'P', model: 'm', max_tokens: 64000, tried: 'off/16000, high/32000, low/0' } };
  assert.equal(P.triedText(info), '已尝试的输出上限：16,000 → 32,000（深度思考 高） → 服务端默认（深度思考 低）');
  assert.equal(P.triedText({ code: 'llm.truncated', params: { tried: 'off/8000' } }), '已尝试的输出上限：8,000');
  assert.equal(P.triedText({ code: 'llm.truncated', params: { tried: '' } }), '', 'no ladder recorded');
  assert.equal(P.triedText({ code: 'llm.truncated', params: {} }), '');
  assert.equal(P.triedText({ code: 'llm.timeout', params: { tried: 'off/8000' } }), '', 'only the truncation error');
  assert.equal(P.triedText(null), '');
  const failed = P.pipelineBar([stage('summary', 'failed', { error: 'LLM 输出被截断', error_info: info, allowed_actions: ['retry'] })], { key: 'lec:T', sys: () => null, res: () => null });
  assert.equal(failed.querySelector('.card-tried').textContent, P.triedText(info));
  const other = P.pipelineBar([stage('summary', 'failed', { error: 'x', error_info: { code: 'llm.network', params: { role: 'summary' } } })], { key: 'lec:U', sys: () => null, res: () => null });
  assert.equal(other.querySelector('.card-tried'), null);
}

{
  // "用当前设置重翻此句" (v3.2.x P1-b): only for a sentence without a usable
  // translation; {"use_current": true} is sent; a 409 of an unusable current
  // setting says so with its reason, other errors as the API words them.
  const R = await import(new URL('retranslate_current.js', base).href);
  const A = await import(new URL('api.js', base).href);
  assert.equal(R.canRetranslateCurrent({ zh_status: 'qa_failed' }), true);
  for (const z of ['ok', 'stale', 'none', undefined]) assert.equal(R.canRetranslateCurrent({ zh_status: z }), false, String(z));
  assert.equal(R.canRetranslateCurrent(null), false);
  const unusable = new A.ApiError(409, '提供商「DS」没有设置 API key', { error: { code: 'snapshot.unusable.no_api_key', params: { provider: 'DS' } } });
  assert.equal(R.retranslateCurrentError(unusable), '当前设置不可用：提供商「DS」没有设置 API key');
  const notPending = new A.ApiError(409, '这句已有可用的译文', { error: { code: 'sentence.not_pending' } });
  assert.equal(R.retranslateCurrentError(notPending), '这句已有可用的译文');
  const saved = globalThis.fetch;
  const sent = [];
  let answer = { status: 200, body: { idx: 3, zh_status: 'stale', mixed: true } };
  globalThis.fetch = async (url, init) => {
    sent.push({ url: String(url), method: init.method, body: init.body });
    const text = JSON.stringify(answer.body);
    return { ok: answer.status < 400, status: answer.status, text: async () => text };
  };
  try {
    const res = await R.retranslateCurrent('L1', 3);
    assert.equal(res.idx, 3);
    assert.equal(res.mixed, true);
    assert.deepEqual(sent[0], { url: '/api/lectures/L1/sentences/3/retranslate', method: 'POST', body: '{"use_current":true}' });
    answer = { status: 409, body: { error: { code: 'snapshot.unusable.no_api_key', params: { provider: 'DS' } } } };
    assert.equal(await R.retranslateCurrent('L1', 3), null, 'refused: nothing to apply');
  } finally {
    globalThis.fetch = saved;
  }
}

console.log('stage card llm checks: ok');
process.exit(0);
