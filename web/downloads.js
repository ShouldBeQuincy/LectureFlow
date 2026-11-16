// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Recordings sent by the LectureFlow browser extension and downloaded by
// the app (SPEC-ext 用户流程 4–6 and 凭据处理, .claude/ext-brief.md §6.2
// "接口（冻结…）"; ext-step6):
//
//   - downloadsSlot(): the rows above a course page's lecture table, in the
//     look of the upload rows (grey: the lecture does not exist yet) —
//     title, state, progress, the error; the buttons are the task's
//     allowed_actions. A finished task (done) is not shown: its lecture is
//     in the table. needs_refresh says how to resume (open the recording's
//     page again and click the extension).
//   - downloadsHint(): one line on the workbench while recordings download
//     or need the user, with links to their courses; nothing otherwise.
//
// Both re-read GET /api/downloads on the SSE "changed {kind: downloads}"
// (throttled) and when the stream comes back. Nothing is changed
// optimistically: a row shows what the server answered.
//
// The calls of /api/downloads are here, not in api.js (same conventions:
// JSON, ApiError with the translated text, ?mock=… in mock mode).

import { ApiError, errorText, MOCK, MOCK_SCENARIO } from './api.js';
import { h, clear, btn, toast, confirmDialog, preserveFocus } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';

const enc = encodeURIComponent;
// the SSE "changed downloads" events of one task come every 500 ms at most;
// the list is read again at most this often
const REFRESH_MS = 500;

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

async function request(method, path) {
  let res;
  try {
    res = await fetch(withMock(path), { method, cache: 'no-store' });
  } catch {
    throw new ApiError(0, t('common.net.unreachable'));
  }
  const text = await res.text().catch(() => '');
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) throw new ApiError(res.status, errorText(res.status, parsed), parsed);
  if (text && parsed === null && text.trim() !== 'null') throw new ApiError(res.status, t('common.net.bad_response'));
  return parsed;
}

/** /api/downloads (ext-brief §6.2). Failures reject with an ApiError; nothing is toasted here. */
export const downloadsApi = {
  // {"items": [Download]}, newest first; courseId '' = every course
  list: (courseId) => request('GET', courseId ? `/api/downloads?course_id=${enc(courseId)}` : '/api/downloads'),
  cancel: (id) => request('POST', `/api/downloads/${enc(id)}/cancel`), // Download
  retry: (id) => request('POST', `/api/downloads/${enc(id)}/retry`), // Download
  remove: (id) => request('DELETE', `/api/downloads/${enc(id)}`), // 204
};

const itemsOf = (res) => (res && Array.isArray(res.items) ? res.items.filter((d) => d && d.id != null) : []);

/** States of a task that has not ended and is not waiting for the user. */
export const ACTIVE_STATES = new Set(['queued', 'downloading', 'merging', 'importing']);
/** States that wait for the user (shown on the workbench too). */
export const ATTENTION_STATES = new Set(['failed', 'needs_refresh']);

// The main program's status tags (SPEC-ext 界面设计): ok / run / warn / bad / idle.
export function stateTone(state) {
  switch (state) {
    case 'downloading':
    case 'merging':
    case 'importing':
      return 'run';
    case 'failed':
      return 'bad';
    case 'needs_refresh':
      return 'warn';
    case 'done':
      return 'ok';
    default:
      return 'idle'; // queued, cancelled
  }
}
const CHIP = { ok: 'chip-ok', run: 'chip-run', warn: 'chip-warn', bad: 'chip-err', idle: 'chip-quiet' };

export function stateLabel(state) {
  switch (state) {
    case 'queued':
      return t('downloads.state.queued');
    case 'downloading':
      return t('downloads.state.downloading');
    case 'merging':
      return t('downloads.state.merging');
    case 'importing':
      return t('downloads.state.importing');
    case 'done':
      return t('downloads.state.done');
    case 'failed':
      return t('downloads.state.failed');
    case 'needs_refresh':
      return t('downloads.state.needs_refresh');
    case 'cancelled':
      return t('downloads.state.cancelled');
    default:
      return state || '';
  }
}

function trackName(tr) {
  if (tr.label) return tr.label;
  if (tr.kind === 'audio') return t('downloads.track.audio');
  if (tr.kind === 'video') return t('downloads.track.video');
  return tr.kind || '';
}

// what has been downloaded: "38% · 488 MB / 1.28 GB", or "488 MB downloaded"
// while the size is not known yet (bytes_total -1)
function bytesText(d) {
  const done = Math.max(0, Number(d.bytes_done) || 0);
  const total = Number(d.bytes_total) || 0;
  if (total > 0) return t('downloads.progress', { pct: U.pct(done / total), done: U.fmtBytes(done), total: U.fmtBytes(total) });
  if (done > 0) return t('downloads.progress_unknown', { done: U.fmtBytes(done) });
  return '';
}

// the text after the state tag
function detailText(d) {
  if (d.state === 'queued') return d.position > 0 ? t('downloads.position', { n: U.count(d.position) }) : '';
  if (d.state === 'downloading' || d.state === 'needs_refresh') return bytesText(d);
  return '';
}

// 0…1 of the bar; null = indeterminate (size not known yet, or merging and
// creating the lecture, which report no progress)
function fraction(d) {
  if (d.state === 'merging' || d.state === 'importing') return null;
  const total = Number(d.bytes_total) || 0;
  if (total > 0) return Math.max(0, Math.min(1, (Number(d.bytes_done) || 0) / total));
  return d.state === 'downloading' ? null : 0;
}

// Why a task failed or needs a refresh (Download.error). When the lecture
// import refused the fetched file itself (an upload.media_* code,
// fetch.RefusedFile: empty, a web page, a playlist, a refused format) the
// task has its own texts: the upload's say to upload another file.
function reasonText(info) {
  return refusedText(info) || U.errorInfoText(info, '');
}

// The task's own text of a refused file ({code, params}: an ErrorInfo or an
// ApiError), '' for any other code.
function refusedText(info) {
  const p = (info && info.params) || {};
  switch (info && info.code) {
    case 'upload.media_empty':
      return t('downloads.refused.empty');
    case 'upload.media_html':
      return t('downloads.refused.html');
    case 'upload.media_playlist':
      return t('downloads.refused.playlist', { kind: p.kind ?? '?' });
    case 'upload.media_format':
      return t('downloads.refused.format', { format: p.format ?? '?' });
  }
  return '';
}

const hasBar = (d) => d.state !== 'failed' && d.state !== 'cancelled';

function setBar(bar, d) {
  if (!bar) return;
  const f = fraction(d);
  if (f == null) bar.removeAttribute('value');
  else bar.value = f;
}

// Problems first (they wait for the user), then what runs, then the queue in
// its order, then the cancelled ones; within each group the server's order
// (newest first). done is not shown.
const RANK = { needs_refresh: 0, failed: 0, downloading: 1, merging: 1, importing: 1, queued: 2, cancelled: 3 };
function shownRows(items) {
  return items
    .filter((d) => d.state !== 'done')
    .map((d, i) => ({ d, i }))
    .sort((a, b) => (RANK[a.d.state] ?? 2) - (RANK[b.d.state] ?? 2) || (a.d.state === 'queued' && b.d.state === 'queued' ? (a.d.position || 0) - (b.d.position || 0) : 0) || a.i - b.i)
    .map((x) => x.d);
}

/**
 * The download rows of one course, above its lecture table. opts:
 *   ctx         the app context (ctx.subscribe: SSE "changed", "reconnected")
 *   courseId
 *   onFinished  called when a task shown before has created its lecture
 *               (done), so the page re-reads its lectures at once
 * Returns {el, refresh(), destroy()}; el is empty and hidden without rows.
 */
export function downloadsSlot({ ctx, courseId, onFinished }) {
  let alive = true;
  let items = []; // the last list of the server
  let shapeKey = null;
  const busy = new Set(); // task ids with an action in flight
  const el = h('ul', { class: 'upload-list dl-list', 'aria-label': t('downloads.list_label') });
  el.hidden = true;

  async function load() {
    try {
      const res = await downloadsApi.list(courseId);
      if (alive) apply(itemsOf(res));
    } catch {
      // keep what is shown; the next event (or the stream coming back) reads again
    }
  }
  const refresher = throttle(load, REFRESH_MS);

  function apply(list) {
    const before = new Map(items.map((d) => [d.id, d.state]));
    items = list;
    // a task that was on its way has created its lecture
    if (onFinished && items.some((d) => d.state === 'done' && before.has(d.id) && before.get(d.id) !== 'done')) onFinished();
    render();
  }

  function render() {
    if (!alive) return;
    const shown = shownRows(items);
    el.hidden = shown.length === 0;
    // what needs a new row: the order, a state, the buttons, the error, the
    // texts; progress and queue positions are updated in place
    const key = JSON.stringify(shown.map((d) => [d.id, d.state, d.allowed_actions, d.error, d.title, d.recorded_at, d.tracks, (Number(d.bytes_total) || 0) > 0, busy.has(d.id)]));
    if (key === shapeKey) {
      for (const d of shown) update(d);
      return;
    }
    shapeKey = key;
    preserveFocus(() => {
      clear(el);
      for (const d of shown) el.append(row(d));
    });
  }

  function update(d) {
    const li = el.querySelector(`[data-dl="${CSS.escape(String(d.id))}"]`);
    if (!li) return;
    setBar(li.querySelector('.up-progress'), d);
    const det = li.querySelector('.dl-detail');
    if (det) det.textContent = detailText(d);
  }

  function row(d) {
    const title = d.title || t('downloads.untitled');
    const meta = [d.recorded_at ? U.fmtDayShort(d.recorded_at, true) : '', U.listJoin((d.tracks || []).map(trackName).filter(Boolean))].filter(Boolean);
    const tone = stateTone(d.state);
    const bar = hasBar(d) ? h('progress', { class: 'up-progress', max: '1', 'aria-label': title }) : null; // named by the task (L16)
    setBar(bar, d);
    return h(
      'li',
      { class: `upload-row dl-row dl-${d.state}${bar ? '' : ' dl-nobar'}`, dataset: { dl: String(d.id) } },
      h('span', { class: 'up-name', title: [title, ...meta].join(' · ') }, title, meta.length ? h('span', { class: 'muted' }, ' · ', meta.join(' · ')) : null),
      bar,
      h('span', { class: 'up-status dl-status' }, h('span', { class: `chip ${CHIP[tone]}` }, stateLabel(d.state)), h('span', { class: 'dl-detail', text: detailText(d) })),
      h('span', { class: 'up-actions' }, buttons(d)),
      note(d),
    );
  }

  // the second line of a task that waits for the user
  function note(d) {
    const reason = reasonText(d.error);
    if (d.state === 'needs_refresh') {
      const hint = t('downloads.needs_refresh.hint');
      // the queue's reasons already end with the same instruction: say it once
      const text = reason && reason.includes(hint) ? [h('span', { class: 'dl-note-main' }, reason)] : [h('span', { class: 'dl-note-main' }, hint), reason ? h('span', { class: 'dl-note-why' }, t('downloads.reason', { reason })) : null];
      return h('div', { class: 'dl-note is-warn' }, icon('refresh-cw', { size: 13 }), h('span', null, text));
    }
    if (d.state === 'failed') {
      // the engine's own words (aria2c, ffmpeg; English) behind "details"
      const detail = (d.error && d.error.detail) || '';
      return h(
        'div',
        { class: 'dl-note is-err' },
        icon('circle-alert', { size: 13 }),
        h(
          'div',
          { class: 'dl-note-body' },
          h('span', { class: 'dl-note-main' }, reason || t('downloads.failed_unknown')),
          detail && detail !== reason ? h('details', { class: 'dl-detail-more' }, h('summary', { dataset: { fkey: `dl:${d.id}:detail` } }, t('downloads.details')), h('pre', { class: 'dl-detail-text mono', lang: 'en' }, detail)) : null,
        ),
      );
    }
    return null;
  }

  function buttons(d) {
    const acts = Array.isArray(d.allowed_actions) ? d.allowed_actions : [];
    const off = busy.has(d.id);
    const out = [];
    // the buttons are the task's allowed_actions and nothing else: a failure
    // a retry cannot fix offers remove only (internal/fetch failedRetry)
    if (acts.includes('retry')) out.push(btn(t('downloads.action.retry'), { size: 'sm', kind: 'primary', icon: 'rotate-cw', disabled: off, fkey: `dl:${d.id}:retry`, onclick: () => act(d, 'retry') }));
    if (acts.includes('cancel')) out.push(btn(t('downloads.action.cancel'), { size: 'sm', disabled: off, fkey: `dl:${d.id}:cancel`, onclick: () => act(d, 'cancel') }));
    if (acts.includes('remove')) out.push(btn(t('common.remove'), { size: 'sm', disabled: off, fkey: `dl:${d.id}:remove`, onclick: () => act(d, 'remove') }));
    return out;
  }

  async function act(d, kind) {
    if (busy.has(d.id)) return;
    // cancelling deletes what was downloaded: ask when there is something
    if (kind === 'cancel' && ((Number(d.bytes_done) || 0) > 0 || d.state === 'merging')) {
      const ok = await confirmDialog({
        title: t('downloads.cancel_confirm.title', { title: d.title || t('downloads.untitled') }),
        message: t('downloads.cancel_confirm.message'),
        okText: t('downloads.action.cancel'),
        cancelText: t('downloads.cancel_confirm.keep'),
        danger: true,
      });
      if (!ok || !alive) return;
    }
    busy.add(d.id);
    render();
    try {
      if (kind === 'remove') {
        await downloadsApi.remove(d.id);
        if (alive) items = items.filter((x) => x.id !== d.id);
      } else {
        const res = kind === 'cancel' ? await downloadsApi.cancel(d.id) : await downloadsApi.retry(d.id);
        if (alive && res && res.id === d.id) items = items.map((x) => (x.id === d.id ? res : x));
      }
    } catch (e) {
      // a retry no longer offered (the row was out of date) answers the
      // task's own reason (409): a refused file's in the row's words
      if (alive) toast(refusedText(e) || e.message, 'error');
    } finally {
      busy.delete(d.id);
      if (alive) {
        render();
        refresher.now();
      }
    }
  }

  const unsubscribe = ctx.subscribe((type, ev) => {
    if (type === 'reconnected') refresher.now();
    else if (type === 'changed' && ev && ev.kind === 'downloads') refresher.request();
  });
  load();

  return {
    el,
    refresh: () => refresher.now(),
    destroy() {
      alive = false;
      refresher.cancel();
      unsubscribe();
    },
  };
}

/**
 * The workbench's line while recordings download or wait for the user:
 * "正在下载 2 节录播 · 1 节需要刷新" with a link to each course concerned.
 * opts.ctx: the app context (subscribe, courses). Returns {el, refresh(),
 * destroy()}; el is hidden while there is nothing to say.
 */
export function downloadsHint({ ctx }) {
  let alive = true;
  let items = [];
  const el = h('div', { class: 'dash-dl-slot', role: 'status' });
  el.hidden = true;

  async function load() {
    try {
      const res = await downloadsApi.list('');
      if (!alive) return;
      items = itemsOf(res);
      render();
    } catch {
      // the workbench works without the line
    }
  }
  const refresher = throttle(load, REFRESH_MS * 2);

  let missing = false; // a course is not in ctx.courses() (yet)
  let nameTimer = null;
  let nameTries = 0;
  function courseName(id) {
    const c = (ctx.courses() || []).find((x) => x.id === id);
    if (!c) missing = true;
    return (c && (c.code || c.name)) || t('downloads.dash.course_unknown');
  }

  function render() {
    if (!alive) return;
    missing = false;
    clearTimeout(nameTimer);
    const active = items.filter((d) => ACTIVE_STATES.has(d.state));
    const refresh = items.filter((d) => d.state === 'needs_refresh');
    const failed = items.filter((d) => d.state === 'failed');
    clear(el);
    el.hidden = !active.length && !refresh.length && !failed.length;
    if (el.hidden) return;
    const parts = [];
    if (active.length) parts.push(h('span', null, t('downloads.dash.active', { n: U.count(active.length) })));
    if (refresh.length) parts.push(h('span', { class: 'tone-warn' }, t('downloads.dash.needs_refresh', { n: U.count(refresh.length) })));
    if (failed.length) parts.push(h('span', { class: 'tone-err' }, t('downloads.dash.failed', { n: U.count(failed.length) })));
    // the courses concerned, problems first
    const courses = [];
    for (const d of [...refresh, ...failed, ...active]) if (d.course_id && !courses.includes(d.course_id)) courses.push(d.course_id);
    const attention = refresh.length + failed.length > 0;
    el.append(
      h(
        'p',
        { class: 'dash-dl' + (attention ? ' is-attention' : '') },
        icon(attention ? 'triangle-alert' : 'download', { size: 15 }),
        h(
          'span',
          { class: 'dash-dl-text' },
          parts.map((p, i) => [i ? ' · ' : null, p]),
        ),
        h(
          'span',
          { class: 'dash-dl-links' },
          courses.map((id) => {
            const name = courseName(id);
            return h('a', { class: 'dash-dl-course', href: `#/course/${enc(id)}`, title: t('downloads.dash.open', { course: name }), dataset: { fkey: `dash:dl:${id}` } }, h('span', null, name), icon('chevron-right', { size: 13 }));
          }),
        ),
      ),
    );
    // the course list of the app may come after the downloads: name them then
    if (missing && nameTries < 10) {
      nameTries++;
      nameTimer = setTimeout(render, 1000);
    }
  }

  const unsubscribe = ctx.subscribe((type, ev) => {
    if (type === 'reconnected') refresher.now();
    else if (type === 'changed' && ev && (ev.kind === 'downloads' || ev.kind === 'course')) refresher.request(); // course: a name changed
  });
  load();

  return {
    el,
    refresh: () => refresher.now(),
    destroy() {
      alive = false;
      clearTimeout(nameTimer);
      refresher.cancel();
      unsubscribe();
    },
  };
}

// At most one fn() per ms while requests keep coming (a debounce would
// starve under a steady stream of progress events); a request during a
// running fn() runs it once more afterwards.
function throttle(fn, ms) {
  let timer = null;
  let running = false;
  let again = false;
  async function run() {
    timer = null;
    running = true;
    try {
      await fn();
    } finally {
      running = false;
      if (again) {
        again = false;
        request();
      }
    }
  }
  function request() {
    if (timer) return;
    if (running) {
      again = true;
      return;
    }
    timer = setTimeout(run, ms);
  }
  return {
    request,
    now() {
      clearTimeout(timer);
      timer = null;
      if (running) again = true;
      else run();
    },
    cancel() {
      clearTimeout(timer);
      timer = null;
      again = false;
    },
  };
}
