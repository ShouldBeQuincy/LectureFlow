// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// 3.2.0 fix (the user's A100 lecture in speed mode: the hover card of the
// running asr stage stayed on 等卡 while the lecture transcribed): the
// card's remote steps follow the device's card in the resources
// (remotes[].jobs[] of this lecture) and fall back to stats.remote, which is
// only what the lecture was loaded with. The other lines that change while a
// stage runs or waits follow too: the time it has run (it stayed at what it
// was when the lecture was loaded), the waiting reason, the tokens; and a
// card that opens on a stage the resources cannot bring up to date reads the
// lecture again, at most once per lecture in LECTURE_REFRESH_MS (a fake clock
// and a fake fetch here). A lecture loaded before the run's first stats flush
// (no stats.remote yet) still gets the card's steps, and its accelerator with
// the read. pipeline.js runs on a small DOM stand-in (enough
// for h(), icon(), querySelector by class / data / aria attribute,
// replaceChildren); web/pipeline_remote_test.go starts it with the web
// directory as a file: URL.

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

// fetch: the UI catalogs from the web directory (the times are texts of the
// catalog; without one t() gives the key, the same at any time) and the
// lecture reads, answered with `lecture` and recorded in `reads`.
let lecture = null;
const reads = [];
globalThis.fetch = async (url) => {
  const u = String(url);
  if (u.startsWith('i18n/')) {
    const body = await readFile(new URL(u, base), 'utf8');
    return { ok: true, status: 200, json: async () => JSON.parse(body), text: async () => body };
  }
  reads.push(u);
  const body = JSON.stringify(lecture);
  return { ok: true, status: 200, text: async () => body };
};
const settle = () => new Promise((r) => setTimeout(r, 0)); // a read answered

const P = await import(new URL('pipeline.js', base).href);
const C = await import(new URL('settings_colab.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const U = await import(new URL('util.js', base).href);
await I.setLocale('zh-CN');

const STEPS = ['wait_accelerator', 'upload_audio', 'remote_transcribe', 'download_result', 'verify'];

// The running asr stage as the lecture was loaded: the remote run had just
// begun (等卡).
function asrStage() {
  return {
    stage: 'asr', status: 'running', display_status: 'running', progress: 0.02, attempt: 0, error: null,
    allowed_actions: [], started_at: new Date().toISOString(), finished_at: null, duration_ms: null,
    stats: { remote: { step: 'wait_accelerator', steps: STEPS, eta_sec: 300, accelerator: 'A100' } },
  };
}

// The card's current step (the item marked aria-current) and its detail.
function current(bar) {
  const li = bar.querySelector('li[aria-current="step"]');
  if (!li) return null;
  const detail = li.querySelector('.rstep-detail');
  return { name: li.querySelector('.rstep-name').textContent, detail: detail ? detail.textContent : '' };
}

let res = null;
const opts = { key: 'lec:L1', lecture: 'L1', sys: () => null, res: () => res };
const card = (jobs) => ({ remotes: [{ id: 'colab:remote', kind: 'colab', label: 'Google Colab · A100', state: 'running', jobs }] });

{
  // No resources (yet): stats.remote as loaded.
  const st = asrStage();
  const bar = P.pipelineBar([st], opts);
  assert.deepEqual(current(bar), { name: C.stepName('wait_accelerator'), detail: '' });

  // The resources say it transcribes (speed mode, 2 parts): the card follows
  // with the next progress event, without a reload of the lecture.
  res = card([{ lecture_id: 'L1', stage: 'transcribe', progress: 0.78, eta_sec: 120, split: 2 }]);
  bar.setProgress('asr', 0.78);
  assert.deepEqual(current(bar), { name: C.stepName('remote_transcribe'), detail: '' });
  const wrap = bar.querySelector('.seg-wrap[data-stage="asr"]');
  assert.ok(wrap.querySelector('.card-remote').textContent.includes(C.durText(120)), 'the time left follows too');

  // A worker stage other than transcribe is the step's detail.
  res = card([{ lecture_id: 'L1', stage: 'load_model', progress: 0.05 }]);
  bar.setProgress('asr', 0.05);
  assert.deepEqual(current(bar), { name: C.stepName('remote_transcribe'), detail: C.stageName('load_model') });

  // download, verify, then back to an earlier step: always the card's.
  for (const [stage, step] of [['download', 'download_result'], ['verify', 'verify'], ['upload', 'upload_audio'], ['wait', 'wait_accelerator']]) {
    res = card([{ lecture_id: 'L1', stage, progress: 0.5 }]);
    bar.setProgress('asr', 0.5);
    assert.equal(current(bar).name, C.stepName(step), stage);
  }

  // Opening the card redraws it too (no progress event needed).
  res = card([{ lecture_id: 'L1', stage: 'check', progress: 0.05 }]);
  wrap.dispatch('pointerenter');
  assert.deepEqual(current(bar), { name: C.stepName('remote_transcribe'), detail: C.stageName('check') });
}

{
  // Two lectures on the same runtime: each card follows its own lecture.
  res = card([
    { lecture_id: 'L2', stage: 'upload', progress: 0.01 },
    { lecture_id: 'L1', stage: 'transcribe', progress: 0.4 },
  ]);
  const one = P.pipelineBar([asrStage()], opts);
  const two = P.pipelineBar([asrStage()], { ...opts, key: 'lec:L2', lecture: 'L2' });
  assert.equal(current(one).name, C.stepName('remote_transcribe'));
  assert.equal(current(two).name, C.stepName('upload_audio'));
  // Not on the card (another device, the card gone): stats.remote.
  const three = P.pipelineBar([asrStage()], { ...opts, key: 'lec:L3', lecture: 'L3' });
  assert.equal(current(three).name, C.stepName('wait_accelerator'));
  // A view without the lecture's id: stats.remote as before.
  const four = P.pipelineBar([asrStage()], { key: 'x', sys: () => null, res: () => res });
  assert.equal(current(four).name, C.stepName('wait_accelerator'));
}

{
  // An SSH server's card names the step itself.
  res = { remotes: [{ id: 'ssh:lab', kind: 'ssh', jobs: [{ lecture_id: 'L1', stage: 'download_result' }] }] };
  const bar = P.pipelineBar([asrStage()], opts);
  assert.equal(current(bar).name, C.stepName('download_result'));
  for (const [stage, want] of [
    ['wait_accelerator', { step: 'wait_accelerator', stage: '' }], ['wait', { step: 'wait_accelerator', stage: '' }],
    ['upload', { step: 'upload_audio', stage: '' }], ['install', { step: 'remote_transcribe', stage: 'install' }],
    ['transcribe', { step: 'remote_transcribe', stage: 'transcribe' }], ['download', { step: 'download_result', stage: '' }],
    ['verify', { step: 'verify', stage: '' }], ['', { step: 'wait_accelerator', stage: '' }],
  ]) {
    assert.deepEqual(P.cardStep(stage), want, stage);
  }
  // A waiting reason is the stage's own and shown only while it waits.
  const st = asrStage();
  st.stats.remote.waiting_for = 'remote_queue';
  res = card([{ lecture_id: 'L1', stage: 'upload' }]);
  assert.equal(P.remoteNow(st, opts).waiting_for, undefined);
  res = card([{ lecture_id: 'L1', stage: 'wait' }]);
  assert.equal(P.remoteNow(st, opts).waiting_for, 'remote_queue');
  assert.equal(P.remoteNow(st, opts).eta_sec, -1, 'no time left on the card: unknown');
  // not running: no remote steps at all
  assert.equal(P.remoteNow({ ...st, status: 'done' }, opts), null);
}

// ---- the other lines that change while a stage runs or waits ----

// A fake clock from here on (the time a card shows is Date.now() - started_at).
let clock = Date.now();
Date.now = () => clock;
// The value of a card line (null: no such line; 'hidden': hidden).
function line(bar, cls) {
  const l = bar.querySelector(`.${cls}`);
  if (!l) return null;
  return l.style.display === 'none' ? 'hidden' : l.querySelector('.card-v').textContent;
}
const elapsed = (ms) => I.t('common.pipe.elapsed', { time: U.fmtDuration(ms) });
const running = (stage, extra) => ({
  stage, status: 'running', display_status: 'running', progress: 0.1, attempt: 0, error: null,
  allowed_actions: [], started_at: new Date(clock - 6000).toISOString(), finished_at: null, duration_ms: null, stats: null, ...extra,
});

{
  // The time it has run follows the clock: it used to stay at what it was
  // when the lecture was loaded ("已运行 6 秒" while the lecture page said
  // "已运行 2 分 0 秒"). Any stage, not only a remote one.
  assert.notEqual(elapsed(6000), 'common.pipe.elapsed', 'the catalog is loaded');
  assert.notEqual(elapsed(6000), elapsed(120000));
  const bar = P.pipelineBar([running('translate')], { key: 'lec:L5', sys: () => null, res: () => null });
  const wrap = bar.querySelector('.seg-wrap[data-stage="translate"]');
  assert.equal(line(bar, 'card-time'), elapsed(6000));
  clock += 114000;
  wrap.dispatch('pointerenter'); // the card opens
  assert.equal(line(bar, 'card-time'), elapsed(120000));
  clock += 5000;
  bar.setProgress('translate', 0.2); // a progress event
  assert.equal(line(bar, 'card-time'), elapsed(125000));
  wrap.dispatch('focusin');
  assert.equal(line(bar, 'card-time'), elapsed(125000), 'no clock change, no change');
  // A finished stage keeps its duration.
  const done = P.pipelineBar([{ ...running('summary'), status: 'done', display_status: 'done', duration_ms: 42000 }], { key: 'lec:L5', sys: () => null, res: () => null });
  clock += 60000;
  done.querySelector('.seg-wrap[data-stage="summary"]').dispatch('pointerenter');
  assert.equal(line(done, 'card-time'), U.fmtDuration(42000));
  await settle();
  assert.deepEqual(reads, [], 'a view without the lecture id reads nothing');
}

{
  // A queued stage's waiting reason: its live numbers are the latest
  // system's; the reason itself (the scheduler's, changed without an event)
  // comes with a read of the lecture when the card opens.
  let sys = { gpu: { free_mb: 1000, estimated_job_mb: 3000 } };
  const st = { ...running('asr'), status: 'queued', display_status: 'queued', started_at: null, progress: 0, waiting_for: 'vram' };
  const bar = P.pipelineBar([st], { key: 'lec:L6', sys: () => sys, res: () => null });
  const wrap = bar.querySelector('.seg-wrap[data-stage="asr"]');
  const before = line(bar, 'card-wait');
  assert.equal(before, U.waitingForText('vram', sys, null));
  sys = { gpu: { free_mb: 2500, estimated_job_mb: 3000 } };
  wrap.dispatch('pointerenter');
  assert.equal(line(bar, 'card-wait'), U.waitingForText('vram', sys, null));
  assert.notEqual(line(bar, 'card-wait'), before);
  await settle();
  assert.deepEqual(reads, []);

  const st2 = { ...st, waiting_for: 'vram' };
  const bar2 = P.pipelineBar([st2], { key: 'lec:L6', lecture: 'L6', sys: () => sys, res: () => null });
  lecture = { id: 'L6', stages: [{ ...st2, waiting_for: 'gpu_slot' }] };
  bar2.querySelector('.seg-wrap[data-stage="asr"]').dispatch('pointerenter');
  await settle();
  assert.deepEqual(reads, ['/api/lectures/L6']);
  assert.equal(line(bar2, 'card-wait'), U.waitingForText('gpu_slot', sys, null));
  reads.length = 0;
}

{
  // The resources do not list the lecture (not yet, or any more): opening the
  // card reads the lecture once (throttled) and the steps follow its
  // stats.remote, without a reload.
  res = card([{ lecture_id: 'L2', stage: 'upload' }]);
  const st = asrStage(); // 等卡, as loaded
  const bar = P.pipelineBar([st], opts); // lecture L1
  const wrap = bar.querySelector('.seg-wrap[data-stage="asr"]');
  const served = asrStage();
  served.progress = 0.5;
  served.stats.remote.step = 'remote_transcribe';
  served.stats.remote.stage = 'transcribe';
  lecture = { id: 'L1', stages: [{ stage: 'prep', status: 'done' }, served] };
  wrap.dispatch('pointerenter');
  assert.equal(current(bar).name, C.stepName('wait_accelerator'), 'until the lecture is read');
  await settle();
  assert.deepEqual(reads, ['/api/lectures/L1']);
  assert.deepEqual(current(bar), { name: C.stepName('remote_transcribe'), detail: '' });
  assert.equal(st.progress, 0.02, "the progress stays the job events'");

  // Within 5 s: no second read (opening again, focus, a progress event).
  assert.equal(P.LECTURE_REFRESH_MS, 5000);
  served.stats.remote.step = 'download_result';
  clock += 4000;
  wrap.dispatch('pointerenter');
  wrap.dispatch('focusin');
  bar.setProgress('asr', 0.6);
  await settle();
  assert.equal(reads.length, 1);
  assert.equal(current(bar).name, C.stepName('remote_transcribe'));
  // Then the next opening reads it again.
  clock += 1000;
  wrap.dispatch('pointerenter');
  await settle();
  assert.equal(reads.length, 2);
  assert.equal(current(bar).name, C.stepName('download_result'));

  // The resources list the lecture: they are live, nothing is read.
  res = card([{ lecture_id: 'L1', stage: 'verify' }]);
  clock += 5000;
  wrap.dispatch('pointerenter');
  await settle();
  assert.equal(reads.length, 2);
  assert.equal(current(bar).name, C.stepName('verify'));

  // A read that finds another status (it finished) leaves the card to the
  // view, which reloads on the status change.
  res = card([]);
  served.status = 'done';
  served.stats.remote.step = 'upload_audio';
  clock += 5000;
  wrap.dispatch('pointerenter');
  await settle();
  assert.equal(reads.length, 3);
  assert.equal(current(bar).name, C.stepName('download_result'));
  reads.length = 0;
}

{
  // A running stage loaded before its first stats flush: the tokens line has
  // its place (hidden) and a read of the lecture fills it.
  const st = running('translate');
  const bar = P.pipelineBar([st], { key: 'lec:L7', lecture: 'L7', sys: () => null, res: () => null });
  assert.equal(line(bar, 'card-tokens'), 'hidden');
  const usage = { prompt_tokens: 12000, completion_tokens: 3400 };
  lecture = { id: 'L7', stages: [{ ...st, stats: usage }] };
  bar.querySelector('.seg-wrap[data-stage="translate"]').dispatch('pointerenter');
  await settle();
  assert.deepEqual(reads, ['/api/lectures/L7']);
  assert.equal(line(bar, 'card-tokens'), U.usageText(usage));
  // A stage that neither runs nor waits has none of these lines.
  const idle = P.pipelineBar([{ ...st, stage: 'export', status: 'pending', display_status: 'pending', started_at: null, stats: null }], { key: 'lec:L7', sys: () => null, res: () => null });
  assert.equal(line(idle, 'card-tokens'), null);
  assert.equal(line(idle, 'card-time'), null);
  assert.equal(line(idle, 'card-wait'), null);
  reads.length = 0;
}

{
  // The lecture was loaded right after asr turned running, before the
  // scheduler's first stats flush (stats null, or stats without remote): the
  // steps are the card's at once, every progress event follows it, and
  // opening the card reads the lecture once (throttled), whose stats.remote
  // brings the accelerator. (Before: no steps at all, and no read because the
  // card listed the lecture.)
  const colabCard = (jobs, extra) => ({ remotes: [{ id: 'colab:remote', kind: 'colab', label: 'Google Colab', state: 'booting', jobs, ...extra }] });
  for (const [id, stats] of [['L9', null], ['L10', {}]]) {
    const st = { ...asrStage(), stats, model: { label: 'Colab', asr: { device_id: 'colab:remote' } } };
    res = colabCard([{ lecture_id: id, stage: 'transcribe', progress: 0.5, eta_sec: 100 }]);
    const o = { key: `lec:${id}`, lecture: id, sys: () => null, res: () => res };
    const bar = P.pipelineBar([st], o);
    const wrap = bar.querySelector('.seg-wrap[data-stage="asr"]');
    assert.deepEqual(current(bar), { name: C.stepName('remote_transcribe'), detail: '' }, id);
    assert.equal(wrap.querySelector('.card-remote').querySelectorAll('li').length, 5, 'the five steps');
    assert.ok(wrap.querySelector('.card-remote').textContent.includes(C.durText(100)), 'the time left');
    assert.equal(line(bar, 'card-where'), 'Google Colab', 'no accelerator known yet: the card says Colab');
    res = colabCard([{ lecture_id: id, stage: 'download' }]);
    bar.setProgress('asr', 0.9);
    assert.equal(current(bar).name, C.stepName('download_result'));
    await settle();
    assert.deepEqual(reads, [], 'a progress event reads nothing');

    lecture = { id, stages: [{ stage: 'prep', status: 'done' }, { ...asrStage(), progress: 0.9 }] }; // stats.remote: A100
    wrap.dispatch('pointerenter');
    await settle();
    assert.deepEqual(reads, [`/api/lectures/${id}`]);
    assert.equal(P.remoteNow(st, o).accelerator, 'A100');
    assert.equal(line(bar, 'card-where'), I.t('common.pipe.remote_where', { accelerator: 'A100' }));
    assert.equal(current(bar).name, C.stepName('download_result'), "the step stays the card's");
    // stats.remote is there now: a later opening reads nothing
    clock += 6000;
    wrap.dispatch('pointerenter');
    await settle();
    assert.equal(reads.length, 1);
    reads.length = 0;
  }

  // The card names the accelerator itself (Colab's session; an SSH server's
  // name): shown before any read.
  const st = { ...asrStage(), stats: null, model: { label: 'Colab', asr: { device_id: 'colab:remote' } } };
  const o = { key: 'lec:L11', lecture: 'L11', sys: () => null, res: () => res };
  res = colabCard([{ lecture_id: 'L11', stage: 'upload' }], { label: 'Google Colab · L4', colab: { accelerator: 'L4' } });
  assert.equal(P.remoteNow(st, o).accelerator, 'L4');
  assert.equal(P.remoteNow(st, o).step, 'upload_audio');
  const ssh = { ...st, model: { label: 'lab', asr: { device_id: 'ssh:lab' } } };
  res = { remotes: [{ id: 'ssh:lab', kind: 'ssh', label: 'Lab server', jobs: [{ lecture_id: 'L11', stage: 'remote_transcribe' }] }] };
  const bar = P.pipelineBar([ssh], o);
  assert.equal(current(bar).name, C.stepName('remote_transcribe'));
  assert.equal(line(bar, 'card-where'), 'Lab server', 'an SSH server by its name, not as Colab');
  // Without the card, an SSH run's stats name the server (its label) too.
  const sshStats = { ...ssh, stats: { remote: { step: 'upload_audio', steps: STEPS, eta_sec: -1, accelerator: 'Lab server' } } };
  assert.equal(line(P.pipelineBar([sshStats], { key: 'x', sys: () => null, res: () => null }), 'card-where'), 'Lab server');
  // A stage other than asr never has remote steps (the card may still list
  // the lecture for a moment after asr finished).
  assert.equal(P.remoteNow(running('summary'), o), null);
  await settle();
  reads.length = 0;
}

{
  // v3.2.x: the Colab runtime does not answer and the lecture tries again in
  // place: stats.remote.reconnect (or, while the resources list the
  // lecture, its card job's reconnect) is a line of the remote steps; it
  // goes away once the runtime answers.
  const text = (bar) => {
    const p = bar.querySelector('.card-reconnect');
    return p ? p.textContent : null;
  };
  const want = I.t('common.pipe.remote_reconnecting', { attempt: 2, of: 4 });
  assert.notEqual(want, 'common.pipe.remote_reconnecting', 'the catalog has the key');
  assert.ok(want.includes('2') && want.includes('4'));
  const st = asrStage();
  st.stats.remote.reconnect = { attempt: 2, of: 4, next_at: '2026-11-11T09:00:05Z' };
  const o = { key: 'lec:L12', lecture: 'L12', sys: () => null, res: () => res };
  res = null;
  assert.equal(text(P.pipelineBar([st], o)), want, 'from stats.remote');
  assert.equal(text(P.pipelineBar([asrStage()], o)), null, 'none without a reconnect');

  // The card lists the lecture: its value is the live one.
  res = card([{ lecture_id: 'L12', stage: 'transcribe', reconnect: { attempt: 3, of: 4, next_at: '2026-11-11T09:00:20Z' } }]);
  const bar = P.pipelineBar([asrStage()], o);
  assert.equal(text(bar), I.t('common.pipe.remote_reconnecting', { attempt: 3, of: 4 }));
  res = card([{ lecture_id: 'L12', stage: 'transcribe' }]); // it answers again
  bar.setProgress('asr', 0.5);
  assert.equal(text(bar), null, 'cleared with the next event');
  assert.equal(P.remoteNow(st, o).reconnect, undefined, "the card's (none) over stale stats");

  // An SSH server is not Colab: no such line.
  const ssh = { ...st, model: { label: 'lab', asr: { device_id: 'ssh:lab' } } };
  assert.equal(text(P.pipelineBar([ssh], { key: 'x', sys: () => null, res: () => null })), null);
  res = null;
}

console.log('pipeline remote checks: ok');
