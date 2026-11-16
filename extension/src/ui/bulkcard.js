// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// "更新这门课程到 LectureFlow" (user request 2026-10-27): many sessions of a
// platform's folder list at once — the whole page, or the rows the user
// ticked. On opening, one check with the sessions' ids and listed names
// (LectureFlow dedups by session and by title within the chosen course);
// imported and downloading ones are shown but cannot be ticked. Only when
// the user clicks "发送 N 节": for each session to send, one after another,
// the platform's sessionInfo request (made by the content script, see
// src/lib/inpage.js), then, after one warning if the lecturer turned
// downloads off for any of them (none when the user turned that reminder
// off for this platform), one enqueue each. A session that fails is marked
// with the reason; the others go on.
//
// ask(cmd, extra): connection, checkMany {sessionIds, titles, courseId},
// session.load {sessionId, values, url}, send {source, trackIds, courseId,
// overrideDisabled, rememberPlatform}. fetchInfo(sessionId) → { ok, values,
// url, status }.

import { el, icon, t } from './dom.js';
import { bulkTrackIds, rowState, bulkCounts } from '../lib/inpage.js';
import { errorText, rememberBox, restrictedNotice, disconnectedText, registerMayHelp, noCourse } from './sendcard.js';

const GAP_MS = 250;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * createBulkCard({ mount, ask, rows: [{ sessionId, title }], origin: 'page' | 'selection',
 *   fetchInfo, presets, platformName, onSent, onClose })
 */
export function createBulkCard({ mount, ask, rows, origin = 'page', fetchInfo, presets = null, platformName = '', onSent = () => {}, onClose = () => {} }) {
  const S = {
    conn: null,
    courseId: '',
    items: new Map(),
    checked: false,
    mode: 'main', // main | dual | audio (the same three choices as the send card)
    picked: new Set(),
    phase: 'loading', // loading | idle | reading | confirm | sending | done
    progress: { i: 0, n: 0 },
    results: new Map(), // sessionId → { state: queued | failed | skipped, error }
    disabled: [], // sessionIds whose download switch is off (known after reading)
    policyStopped: false, // the switch could not be read: the profile was stopped as too slow (SEC18f 低-18)
    remembered: false, // the user turned the restricted reminder off for this platform
    rememberTick: false, // "对这个平台不再提醒", ticked in the open reminder
    message: null,
  };
  const root = el('div', { class: 'lf-card bulk' });
  mount.replaceChildren(root);
  let decide = null;

  const stateOf = (sid) => rowState(S.items.get(sid));
  const pickable = (sid) => {
    const s = stateOf(sid);
    return (s === 'new' || s === 'again') && !S.results.has(sid);
  };
  const busy = () => S.phase === 'reading' || S.phase === 'confirm' || S.phase === 'sending';

  function chipFor(sid) {
    const res = S.results.get(sid);
    if (res) {
      if (res.state === 'queued') return el('span', { class: 'chip chip-run', text: t('bulkQueued') });
      if (res.state === 'skipped') return el('span', { class: 'chip chip-idle', text: t('bulkSkipped') });
      return el('span', { class: 'chip chip-bad', text: t('bulkFailed') });
    }
    const s = stateOf(sid);
    if (s === 'imported') return el('span', { class: 'chip chip-ok', text: t('state_imported') });
    if (s === 'active') return el('span', { class: 'chip chip-run', text: t('state_downloading') });
    if (s === 'refresh') return el('span', { class: 'chip chip-warn', text: t('state_needs_refresh') });
    return el('span', { class: 'chip chip-idle', text: t('bulkNew') });
  }

  function rowItem(r) {
    const can = pickable(r.sessionId) && !busy() && S.phase !== 'done';
    const box = el('input', { type: 'checkbox', 'aria-label': t('selectTrack', [r.title || r.sessionId]) });
    box.checked = S.picked.has(r.sessionId);
    box.disabled = !can;
    box.addEventListener('change', () => {
      if (box.checked) S.picked.add(r.sessionId);
      else S.picked.delete(r.sessionId);
      render();
    });
    const res = S.results.get(r.sessionId);
    return el(
      'li',
      { class: 'bulk-row', dataset: { sessionId: r.sessionId, state: res ? res.state : stateOf(r.sessionId), disabled: String(!pickable(r.sessionId)) } },
      box,
      el('span', { class: 'bulk-title', text: r.title || r.sessionId }),
      chipFor(r.sessionId),
      res && res.error ? el('span', { class: 'bulk-error', text: res.error }) : null,
    );
  }

  function courseField() {
    const c = S.conn || {};
    const courses = c.courses || [];
    if (courses.length === 0) return noCourse(() => start(true));
    const select = el(
      'select',
      { class: 'input', id: 'course', 'aria-label': t('courseLabel') },
      courses.map((x) => el('option', { value: String(x.id), text: String(x.id) === c.lastCourseId ? `${x.name}${t('courseLast')}` : x.name })),
    );
    select.value = S.courseId;
    select.disabled = busy() || S.phase === 'done';
    select.addEventListener('change', () => {
      S.courseId = select.value;
      // Dedup by title is per course: ask again.
      check();
    });
    return el('label', { class: 'field-inline' }, el('span', { class: 'field-label', text: t('courseLabel') }), select);
  }

  function modeField() {
    const seg = el('div', { class: 'seg', id: 'presets', role: 'group', 'aria-label': t('contentLabel') });
    for (const [mode, key] of [['main', 'presetMain'], ['dual', 'presetDual'], ['audio', 'presetAudio']]) {
      const b = el('button', { type: 'button', dataset: { preset: mode }, 'aria-pressed': String(S.mode === mode), text: t(key) });
      b.disabled = busy() || S.phase === 'done';
      b.addEventListener('click', () => {
        S.mode = mode;
        render();
      });
      seg.append(b);
    }
    return el('div', { class: 'field-wrap' }, el('span', { class: 'field-label', text: t('contentLabel') }), seg);
  }

  function overlay() {
    if (S.phase !== 'confirm') return null;
    const n = String(S.disabled.length);
    return el(
      'div',
      { class: 'overlay', id: 'override' },
      el(
        'section',
        { class: 'card dialog', role: 'alertdialog', 'aria-modal': 'true' },
        el('h2', { class: 'dialog-title' }, el('span', { class: 'dialog-icon' }, icon('alert-triangle')), el('span', { text: t('restrictedTitle') })),
        el('p', { id: 'override-text', text: t('bulkRestrictedConfirm', [n]) }),
        platformName
          ? rememberBox(platformName, S.rememberTick, (on) => {
              S.rememberTick = on;
            })
          : null,
        el(
          'div',
          { class: 'dialog-actions' },
          el('button', { class: 'btn', id: 'override-skip', onclick: () => decide && decide(false) }, el('span', { text: t('bulkSkipDisabled', [n]) })),
          el('button', { class: 'btn btn-warn', id: 'override-confirm', onclick: () => decide && decide(true) }, el('span', { text: t('sendAnyway') })),
        ),
      ),
    );
  }

  function render() {
    const counts = bulkCounts(rows, [...S.items.values()], S.picked);
    const parts = [];
    // nothing is detected here: the card waits for LectureFlow (T26 G7)
    if (S.phase === 'loading') parts.push(el('p', { class: 'muted', id: 'bulk-loading', text: t('connectingApp') }));
    else if (S.conn && S.conn.status !== 'connected') {
      // Two messages, each a sentence of its own in every language; the
      // reason as the popup says it, and the button it names (T26 G8).
      // a request the background refused (ext.not_allowed, ext.site_blocked):
      // its own reason, not "not found" (SEC42 低-3)
      parts.push(el('p', { class: 'note', id: 'bulk-disconnected' }, el('strong', { text: t('disconnectedTitle') }), ' ', S.conn.error ? errorText(S.conn.error) : disconnectedText(S.conn)));
      if (registerMayHelp(S.conn)) parts.push(el('p', { class: 'small muted', id: 'disc-register', text: t('discRegister') }));
      parts.push(el('div', { class: 'btn-row' }, el('button', { class: 'btn', id: 'bulk-recheck', type: 'button', onclick: () => start(true) }, icon('refresh'), el('span', { text: t('connRecheck') }))));
    } else {
      parts.push(
        el(
          'p',
          { class: 'bulk-summary', id: 'bulk-summary' },
          t('bulkSummary', [String(counts.total), String(counts.imported), String(counts.active)]),
          el('strong', { text: t('bulkSummaryTo', [String(counts.toSend)]) }),
        ),
      );
      parts.push(el('ul', { class: 'bulk-rows', id: 'bulk-rows' }, rows.map(rowItem)));
      // why those cannot be ticked, and where they are resumed (T26 G5)
      if (counts.refresh > 0) parts.push(el('p', { class: 'small muted', id: 'bulk-refresh-note', dataset: { count: String(counts.refresh) }, text: t('bulkRefreshNote', [String(counts.refresh)]) }));
      if (origin === 'page') parts.push(el('p', { class: 'small faint', text: t('bulkPageOnly') }));
      // Known only once the sessions were read, so the reminder is behind:
      // the notice without "发送前会再提醒一次" (T26 G6).
      if (S.disabled.length && S.phase !== 'confirm') {
        const banner = (text) => el('p', { class: 'note note-warn', id: 'bulk-warning' }, el('span', { class: 'note-icon' }, icon('alert-triangle')), text);
        parts.push(...(S.remembered ? restrictedNotice({ remembered: true, name: platformName, banner }) : [banner(t('restrictedNoticeQuiet'))]));
      }
      // as the send card's chip: why some switches are unknown (SEC18f 低-18)
      if (S.policyStopped) parts.push(el('p', { class: 'note note-warn', id: 'bulk-policy-stopped', dataset: { flag: 'policy-stopped' } }, el('span', { class: 'note-icon' }, icon('alert-triangle')), t('policyStopped')));
      parts.push(modeField());
      parts.push(courseField());
      if (S.phase === 'done') {
        const sent = [...S.results.values()].filter((r) => r.state === 'queued').length;
        parts.push(el('p', { class: 'note note-ok', id: 'bulk-done', text: t('bulkDone', [String(sent), String(S.results.size - sent)]) }));
        parts.push(el('button', { class: 'btn btn-block', id: 'bulk-close', onclick: onClose }, el('span', { text: t('done') })));
      } else {
        let label = t('bulkSend', [String(counts.toSend)]);
        if (S.phase === 'reading') label = t('bulkReading', [String(S.progress.i), String(S.progress.n)]);
        // the reminder is open: nothing is being sent yet (T26 G6)
        else if (S.phase === 'confirm') label = t('bulkWaiting');
        else if (S.phase === 'sending') label = t('bulkSending', [String(S.progress.i), String(S.progress.n)]);
        const b = el('button', { class: 'btn btn-primary btn-block', id: 'primary', onclick: send }, icon('send'), el('span', { text: label }));
        b.disabled = busy() || counts.toSend === 0 || !S.courseId || !S.checked;
        parts.push(b);
      }
      parts.push(el('p', { class: 'data-note', text: t('bulkQueueNote') }));
    }
    if (S.message) parts.push(el('p', { class: `note${S.message.tone ? ' note-' + S.message.tone : ''}`, id: 'msg', role: 'status', text: S.message.text }));
    parts.push(overlay());
    root.dataset.phase = S.phase;
    root.replaceChildren(...parts.filter(Boolean));
  }

  async function check() {
    S.checked = false;
    render();
    const r = await ask('checkMany', { sessionIds: rows.map((x) => x.sessionId), titles: rows.map((x) => x.title || ''), courseId: S.courseId });
    S.items = new Map((r.ok ? r.items : []).map((i) => [i.sessionId, i]));
    S.checked = r.ok;
    if (!r.ok) S.message = { text: errorText(r.error), tone: 'bad' };
    // Tick what can be sent; keep the user's unticks.
    for (const x of rows) if (!pickable(x.sessionId)) S.picked.delete(x.sessionId);
    if (!S.touched) for (const x of rows) if (pickable(x.sessionId)) S.picked.add(x.sessionId);
    S.touched = true;
    if (S.phase === 'loading') S.phase = 'idle';
    render();
  }

  async function send() {
    if (busy()) return;
    const todo = rows.filter((x) => S.picked.has(x.sessionId));
    if (todo.length === 0) return;
    S.message = null;
    S.policyStopped = false;
    // 1. The platform's sessionInfo, one session after another, only now.
    S.phase = 'reading';
    S.progress = { i: 0, n: todo.length };
    const prepared = [];
    for (const x of todo) {
      S.progress.i++;
      render();
      try {
        const info = await fetchInfo(x.sessionId);
        if (!info.ok) {
          S.results.set(x.sessionId, { state: 'failed', error: errorText({ code: 'ext.info_failed', params: { status: info.status || 0 } }) });
        } else {
          const r = await ask('session.load', { sessionId: x.sessionId, values: info.values, url: info.url });
          if (r.ok) S.remembered = !!r.view.remembered;
          const ids = r.ok ? bulkTrackIds(r.view.tracks || [], S.mode, presets) : [];
          // a profile stopped as too slow: the same reason as the send card's chip (SEC22 L-D)
          if (!r.ok && r.policyStopped) S.policyStopped = true;
          if (!r.ok) S.results.set(x.sessionId, { state: 'failed', error: r.policyStopped ? t('policyStopped') : errorText(r.error) });
          else if (ids.length === 0) S.results.set(x.sessionId, { state: 'failed', error: t('bulkNoTrack') });
          else prepared.push({ sessionId: x.sessionId, ids, disabled: r.view.downloadPolicy === 'disabled', stopped: !!r.view.policyStopped });
        }
      } catch (e) {
        S.results.set(x.sessionId, { state: 'failed', error: errorText({ code: 'ext.info_failed', params: { status: 0 } }) });
      }
      if (S.progress.i < todo.length) await sleep(GAP_MS);
    }
    // 2. One warning for the sessions whose lecturer turned downloads off,
    // unless the user turned it off for this platform.
    S.disabled = prepared.filter((p) => p.disabled).map((p) => p.sessionId);
    S.policyStopped = S.policyStopped || prepared.some((p) => p.stopped);
    let override = false;
    let remember = false;
    if (S.disabled.length && S.remembered) override = true;
    else if (S.disabled.length) {
      S.phase = 'confirm';
      S.rememberTick = false;
      render();
      override = await new Promise((resolve) => {
        decide = resolve;
      });
      decide = null;
      remember = override && S.rememberTick && !!platformName;
      if (!override) for (const sid of S.disabled) S.results.set(sid, { state: 'skipped', error: t('restrictedChip') });
    }
    // 3. One enqueue per session, in list order.
    const go = prepared.filter((p) => !p.disabled || override);
    S.phase = 'sending';
    S.progress = { i: 0, n: go.length };
    for (const p of go) {
      S.progress.i++;
      render();
      const r = await ask('send', { source: { kind: 'session', sessionId: p.sessionId }, trackIds: p.ids, courseId: S.courseId, overrideDisabled: p.disabled && override, rememberPlatform: p.disabled && remember });
      if (r.ok) S.results.set(p.sessionId, { state: 'queued', taskId: r.taskId });
      else if (r.error && r.error.code === 'queue.duplicate') S.results.set(p.sessionId, { state: 'skipped', error: t('blockedImported') });
      else S.results.set(p.sessionId, { state: 'failed', error: errorText(r.error) });
    }
    if (remember) S.remembered = true;
    for (const x of todo) S.picked.delete(x.sessionId);
    S.phase = 'done';
    if (S.conn) S.conn.lastCourseId = S.courseId;
    render();
    onSent(new Map(S.results));
  }

  /** hello (recheck: "重新检测", a fresh one), the course, then the check. */
  async function start(recheck = false) {
    if (busy()) return;
    S.phase = 'loading';
    render();
    const r = await ask('connection', { recheck });
    S.conn = r.ok ? r.connection : { status: 'unavailable', reason: (r.error && r.error.code) || '', params: (r.error && r.error.params) || {}, error: r.error || { code: 'ext.failed', params: {} } };
    if (S.conn.status !== 'connected') {
      S.phase = 'idle';
      render();
      return;
    }
    const ids = (S.conn.courses || []).map((c) => String(c.id));
    const last = S.conn.lastCourseId || '';
    S.courseId = ids.includes(last) ? last : ids[0] || '';
    await check();
  }

  return { start, state: S, render };
}
