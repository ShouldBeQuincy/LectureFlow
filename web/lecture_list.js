// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Lecture list of the course page (v1.1 A2, A3.2, A7, A12): one row per
// lecture — checkbox (Shift selects a range, the header selects all),
// number L03, title with the draft tag, pipeline bar, the status line
// (stale reasons, waiting_for, errors) and the row actions; a floating
// batch bar for the selection; the lecture context menu. Every button comes
// from lecture_actions / allowed_actions; results come from the back end.
//
// v2 P13.4, P13.6: rows are grouped by teaching week ("Week 2 · 6 Oct–12
// Oct", "Time unknown" last; a calm hint opens the setting a lecture without
// a week needs); main and sub title and the one-line blurb follow the
// lecture title language preference.
//
// lecture kinds: the first column is the kind (Lecture, Tutorial, …) in the
// accent colour with a quieter "No. 2" under it when the back end says so
// (show_n); W02L1 stays the citation number (in the hover text). Teaching
// materials without a teaching week (a past lecture recorded before the
// term) form their own group after the weeks; a week's header shows how
// many slide files apply to it.
//
// course-ux (2026-10-27): the selection bar reruns one stage of the
// selected lectures (course_ux.js, one confirmation) and selects the whole
// course; a teaching material can be dragged (or, from its handle's menu,
// sent) under a teaching week, where it shows indented as that week's
// supplement; a make-up lecture is listed in the week whose content it is,
// after that week's own lectures, with a yellow "补" tag.

import { h, append, clear, btn, iconBtn, preserveFocus, everySecond } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar, streamProgress, cardActions } from './pipeline.js';
import { bindContextMenu, menuButton, openMenu } from './menu.js';
import * as A from './actions.js';
import { api } from './api.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { lectureTitles, titleLang, weekTitle, weekRangeText, mondayOf, parseYMD, ymd, fmtRecordedAt, openRecordedAtDialog } from './calendar_ui.js';
import { zh, zhOn } from './zh_display.js';
import { zhEditNote } from './widgets.js';
import { kindTag, isMaterial, weekOf, lectureTagText, kindSourceText } from './kinds.js';
import { batchRerun, rerunStages, weekMenuItems, hangItems, canHang, appliedOverride, makeupTag, weekMonday, hangUnder, clearOverride } from './course_ux.js';
import { confirmUpdate, confirmLaunchDrafts } from './update_confirm.js';

const enc = encodeURIComponent;

// t(key, {n}) with the number shown as a node (e.g. bold): the catalog text
// is split at the formatted number.
function countNodes(key, n, wrap) {
  const s = t(key, { n });
  const num = String(n);
  const i = s.indexOf(num);
  if (i < 0) return [s];
  return [s.slice(0, i), wrap(num), s.slice(i + num.length)].filter((x) => x !== '');
}

// opts: {ctx, onLectures(updated[]), onDeleted(ids[]), onCourseSettings(),
// onShowMaterial(id) (a week's slide file chosen in its header menu),
// course() (the course: term start, weeks; course-ux), filtered() (a search
// narrows the list)}
export function createLectureList(opts) {
  const { ctx } = opts;
  const course = () => (opts.course ? opts.course() : null);
  let drag = null; // course-ux: {lec} while a teaching material is dragged
  let pendingList = null; // a render that arrived during the drag
  let lectures = [];
  const selected = new Set();
  let anchor = null; // last clicked id (Shift range)
  let editing = null; // id being renamed
  let draft = '';
  let focusTitleInput = false;
  const refs = new Map(); // id → {bar, statusEl, lec}

  const headCheck = h('input', { type: 'checkbox', class: 'check', 'aria-label': t('course.lectures.select_all'), dataset: { fkey: 'lt:all' }, onchange: () => toggleAll(headCheck.checked) });
  const headCell = h('span', { class: 'lt-check', role: 'columnheader' }, headCheck);
  // R70 low 2 (round 8 WL-12): a click on the cell around the box is one on
  // the box (see checkCell)
  cellClicks(headCell, headCheck, () => {
    // as a click on the box: all when not all are selected (also when some are)
    headCheck.checked = !(lectures.length > 0 && selected.size === lectures.length);
    toggleAll(headCheck.checked);
  });
  const head = h(
    'div',
    { class: 'lt-head', role: 'row' },
    // a column header too (named by the select-all box): the rows' first
    // cell is the check box, so without it the headers were off by one (T11)
    headCell,
    h('span', { class: 'lt-h', role: 'columnheader' }, t('course.lectures.col.kind')),
    h('span', { class: 'lt-h', role: 'columnheader' }, t('course.lectures.col.lecture')),
    h('span', { class: 'lt-h', role: 'columnheader' }, t('course.lectures.col.progress')),
    h('span', { class: 'lt-h lt-h-end', role: 'columnheader' }, h('span', { class: 'visually-hidden' }, t('course.lectures.col.actions'))),
  );
  const body = h('div', { class: 'lt-body', role: 'rowgroup' });
  // course-ux: shown while an attached supplement is dragged
  const unhangZone = h('div', { class: 'lt-unhang', role: 'row', 'aria-hidden': 'true' }, h('span', { role: 'cell' }, icon('undo-2', { size: 14 }), t('course.week.drop_unhang')));
  const table = h('div', { class: 'lt', role: 'table', 'aria-label': t('course.lectures.table_label') }, head, body);
  const bar = h('div', { class: 'batchbar', role: 'region', 'aria-label': t('course.lectures.batch.label'), hidden: true });

  table.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && selected.size && !editing) {
      selected.clear();
      render(lectures);
    }
  });

  const menuHandlers = {
    onLectures: (ls) => opts.onLectures(ls),
    onDeleted: (ids) => opts.onDeleted(ids),
    onRename: (lec) => startRename(lec.id),
  };

  function setSelection(ids, on) {
    for (const id of ids) on ? selected.add(id) : selected.delete(id);
  }

  function toggleAll(on) {
    setSelection(
      lectures.map((l) => l.id),
      on,
    );
    render(lectures);
  }

  function onCheck(lec, e) {
    const on = e.target.checked;
    if (e.shiftKey && anchor && anchor !== lec.id) {
      const ids = lectures.map((l) => l.id);
      const a = ids.indexOf(anchor);
      const b = ids.indexOf(lec.id);
      if (a >= 0 && b >= 0) setSelection(ids.slice(Math.min(a, b), Math.max(a, b) + 1), on);
    } else {
      setSelection([lec.id], on);
    }
    anchor = lec.id;
    render(lectures);
  }

  function selectedLectures() {
    return lectures.filter((l) => selected.has(l.id));
  }

  // R70 low 2 (round 8 WL-12): the check box's cell takes the clicks
  // around the box as the box's own. Its wider hit area was a ::after of
  // the box (style.css), which only Chromium draws on a native check box:
  // Firefox and Safari kept 15 px. A click on the cell itself (not on the
  // box, nor on a material's drag handle in it) toggles the box with the
  // Shift of the click (a range, as onCheck does) and gives it the focus,
  // as a click on it does; the cell stays a table cell (a <label> would
  // also have done, but its click reaches the box with the Shift of the
  // browser's own making, and a Shift+click on it selects text). toggle
  // (shift) does the box's work.
  function cellClicks(cell, box, toggle) {
    cell.addEventListener('mousedown', (e) => {
      if (e.target === cell && e.shiftKey) e.preventDefault(); // no text selection by Shift+click
    });
    cell.addEventListener('click', (e) => {
      if (e.target !== cell || box.disabled) return;
      box.focus();
      toggle(!!e.shiftKey);
    });
  }

  // ---- rows ----

  function render(list) {
    if (drag) {
      pendingList = list; // re-rendering would remove the dragged row
      return;
    }
    // its own copy: the caller's array is changed in place before it calls
    // updateRow, which compares with the list as drawn
    lectures = list.slice();
    for (const id of [...selected]) if (!lectures.some((l) => l.id === id)) selected.delete(id);
    preserveFocus(() => {
      clear(body);
      refs.clear();
      const lang = titleLang();
      const groups = groupLectures(lectures);
      for (const g of groups) {
        body.append(groupRow(g));
        for (const lec of g.lectures) body.append(row(lec, lang, g.week));
        for (const lec of g.children || []) body.append(row(lec, lang, g.week, true));
      }
      body.append(unhangZone);
      headCheck.checked = lectures.length > 0 && selected.size === lectures.length;
      headCheck.indeterminate = selected.size > 0 && selected.size < lectures.length;
      headCheck.disabled = lectures.length === 0;
      table.classList.toggle('is-empty', lectures.length === 0);
      renderBar();
    });
    if (focusTitleInput) {
      focusTitleInput = false;
      const input = body.querySelector('.lrow-title-input');
      if (input) {
        input.focus();
        input.select();
      }
    }
  }

  function statusLine(lec) {
    const sum = U.lectureSummary(lec.stages, { sys: ctx.system(), res: ctx.resources(), staleText: U.lectureStaleText(lec), draft: lec.draft });
    const meta = [];
    if (lec.draft) meta.push(t('course.lectures.uploaded', { when: U.relTime(lec.created_at) }));
    else if (!['stale', 'failed', 'retrying'].includes(sum.kind)) {
      // (a stale reason or an error needs the whole line)
      if (lec.duration_ms) meta.push(U.fmtClock(lec.duration_ms));
      if (lec.counts.sentences) meta.push(t('course.lectures.sentences', { n: U.count(lec.counts.sentences) }));
      if (sum.kind === 'done') {
        const exp = lec.stages.find((s) => s.stage === 'export');
        if (exp && exp.finished_at) meta.push(t('course.lectures.finished', { when: U.relTime(exp.finished_at) }));
      }
    }
    // a teaching material outside the teaching weeks: when it was recorded
    const d = isMaterial(lec) && !weekOf(lec) ? parseYMD(lec.recorded_at) : null;
    if (d) meta.unshift(t('lecture.recorded_month', { month: U.fmtMonthYear(d) }));
    if (lec.same_day_by_name) meta.push(t('lecture.same_day_by_name')); // P13.3.4
    const warn = U.warningHint(lec.stages);
    let text = sum.text;
    if (sum.kind === 'failed' && sum.stage.error) text = t('course.lectures.status_error', { status: text, error: U.excerpt(U.stageErrorText(sum.stage).split('\n')[0], 80) });
    if (sum.kind === 'running' && sum.stage.started_at) {
      const ms = Date.now() - new Date(sum.stage.started_at).getTime();
      if (ms > 0) text = t('course.lectures.status_running', { status: text, duration: U.fmtDuration(ms) });
    }
    return { sum, text, meta, warn };
  }

  // The one inline action of a row: start / retry / update (A2.3, A3.2),
  // else play once the lecture is playable (SPEC-v2 P1).
  function primaryAction(lec) {
    const acts = lec.lecture_actions || [];
    const fnext = (x) => endNext(`lec:${lec.id}`, x);
    if (acts.includes('launch')) return btn(t('course.lectures.launch'), { size: 'sm', kind: 'primary', icon: 'play', fkey: `lec:${lec.id}:launch`, fnext: fnext('launch'), onclick: () => done(A.launchLectures([lec])) });
    // a retry that cannot succeed (a damaged file, U.retryFutile) is not the row's action
    const failed = lec.stages.find((s) => s.status === 'failed' && (s.allowed_actions || []).includes('retry') && !U.retryFutile(s));
    // T24 B1: a failure the same request cannot get past (a refusal, the
    // total time limit: U.retryNeedsModel) fails again, and is billed again,
    // when retried as it is: the row offers "retry with another model…"
    // (the stage card's, actions.js) instead; without it, no retry here
    if (failed && U.retryNeedsModel(failed)) {
      if (cardActions(failed).includes('retry_model'))
        return btn(t('common.action.retry_model'), { size: 'sm', icon: 'rotate-cw', fkey: `lec:${lec.id}:retry_model`, fnext: fnext('retry_model'), title: t('course.lectures.retry_model_tip', { stage: U.stageName(failed.stage) }), onclick: () => done(A.stageAction(lec, failed.stage, 'retry_model')) });
    } else if (failed) return btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', fkey: `lec:${lec.id}:retry`, fnext: fnext('retry'), title: t('course.lectures.retry_tip', { stage: U.stageName(failed.stage) }), onclick: () => done(A.stageAction(lec, failed.stage, 'retry', true)) });
    if (acts.includes('update')) return btn(t('course.lectures.update'), { size: 'sm', kind: 'warn', icon: 'refresh-cw', fkey: `lec:${lec.id}:update`, fnext: fnext('update'), title: t('course.lectures.update_tip'), onclick: () => done(updateConfirmed([lec])) });
    if (lec.playable) return btn(t('player.entry.play'), { size: 'sm', kind: U.isAllDone(lec.stages) ? 'primary' : '', icon: 'play', fkey: `lec:${lec.id}:play`, fnext: fnext('play'), title: t('player.entry.play_hint'), onclick: () => (location.hash = `#/play/${enc(lec.id)}`) });
    return null;
  }

  // Where the focus goes when an action at the row's end is gone after the
  // re-render it caused (3.2.0 L14): the row's new action in the same place
  // ("启动" becomes "重试", "换模型重试…", "更新" or "播放"), else the next
  // control of the row; "更多操作" is always there.
  function endNext(fk, self) {
    const prim = ['launch', 'retry', 'retry_model', 'update', 'play'];
    const rest = ['review', 'dl', 'more'];
    const keys = prim.includes(self) ? [...prim.filter((k) => k !== self), ...rest] : rest.slice(rest.indexOf(self) + 1);
    return keys.map((k) => `${fk}:${k}`);
  }

  // T25 X1: an update says first which stages run again, why, and what
  // they took the last time (update_confirm.js); one dialog for the same
  // lectures at a time (a second click while it asks)
  function updateConfirmed(list) {
    const ls = list.filter((l) => (l.lecture_actions || []).includes('update'));
    if (!ls.length) return Promise.resolve([]);
    const key = ls
      .map((l) => l.id)
      .sort()
      .join(',');
    // a selection names the lectures it updates, one included (R74 low 3)
    return A.once(`confirm_update:${key}`, async () => ((await confirmUpdate(ls, { named: list.length > 1 })) ? A.updateLectures(ls) : []));
  }

  // R74 medium 4: "start" of the selected drafts asks as "start all
  // drafts" does (the same dialog). Round 8 (C178b): asked by
  // launchLectures's opts.ask, after its glossary question (the glossary
  // still being extracted), each once, not the confirmation first and the
  // glossary's after it; launchLectures keeps it to one dialog at a time
  // for the same lectures (once).
  function launchConfirmed(list) {
    const ls = list.filter((l) => (l.lecture_actions || []).includes('launch'));
    if (!ls.length) return Promise.resolve([]);
    return A.launchLectures(ls, { ask: (sel) => confirmLaunchDrafts(sel, sel.length) });
  }

  function done(p) {
    return p.then(
      (r) => {
        if (Array.isArray(r)) r.length && opts.onLectures(r);
        else if (r) opts.onLectures([r]);
      },
      () => {},
    );
  }

  // ---- grouping by teaching week (P13.4) ----

  // Consecutive runs of lectures in list order (seq) that share a week;
  // then the teaching materials without a week; "time unknown" last.
  // course-ux: lectures with a week override join the group of their week
  // (made when no lecture of its own is in it): make-ups after the week's
  // own lectures, as the back end numbers them; supplements as its
  // indented children.
  function groupLectures(list) {
    const out = [];
    let materials = null;
    let unknown = null;
    const placed = [];
    for (const lec of list) {
      if (appliedOverride(lec)) {
        placed.push(lec);
        continue;
      }
      if (isMaterial(lec) && !weekOf(lec)) {
        if (!materials) materials = { key: 'mat', title: t('lecture.group.materials'), hint: t('lecture.group.materials_hint'), lectures: [] };
        materials.lectures.push(lec);
        continue;
      }
      const g = groupOf(lec);
      if (g.key === 'none') {
        if (!unknown) unknown = { ...g, lectures: [] };
        unknown.lectures.push(lec);
        continue;
      }
      const last = out[out.length - 1];
      if (last && last.key === g.key) last.lectures.push(lec);
      else out.push({ ...g, lectures: [lec] });
    }
    for (const lec of placed) {
      const o = appliedOverride(lec);
      let g = out.find((x) => x.week === o.week);
      if (!g) {
        const mon = weekMonday(course(), o.week);
        g = { key: `w${o.week}`, title: mon ? weekTitle(o.week, mon) : t('kind.week', { week: o.week }), week: o.week, mon, lectures: [] };
        const at = mon ? out.findIndex((x) => x.mon && x.mon > mon) : -1;
        out.splice(at < 0 ? out.length : at, 0, g);
      }
      if (o.kind === 'supplement') (g.children || (g.children = [])).push(lec);
      else g.lectures.push(lec);
    }
    if (materials) out.push(materials);
    if (unknown) out.push(unknown);
    return out;
  }

  function groupOf(lec) {
    const d = parseYMD(lec.recorded_at);
    if (!d) return { key: 'none', title: t('lecture.group.unknown'), hint: t('lecture.group.unknown_hint') };
    const mon = mondayOf(d);
    if (lec.numbered && lec.week > 0) return { key: `w${lec.week}`, title: weekTitle(lec.week, mon), week: lec.week, mon };
    const range = weekRangeText(mon);
    if (lec.calendar_hint === 'skipped_week') return { key: `s${ymd(mon)}`, title: t('lecture.group.skipped', { range }), mon };
    if (lec.calendar_hint === 'before_term') return { key: `b${ymd(mon)}`, title: t('lecture.group.before_term', { range }), mon };
    return { key: `c${ymd(mon)}`, title: range, mon }; // no term start: calendar weeks
  }

  function groupRow(g) {
    const n = g.lectures.length + (g.children ? g.children.length : 0);
    return h(
      'div',
      { class: 'lt-group' + (g.key === 'none' ? ' is-unknown' : '') + (g.key === 'mat' ? ' is-materials' : ''), role: 'row', dataset: g.week ? { week: g.week, dropWeek: g.week } : undefined },
      h('span', { class: 'lt-group-t', role: 'cell', title: g.hint || undefined }, g.title, h('span', { class: 'lt-group-n' }, t('lecture.group.count', { n }))),
      g.week ? weekSlides(g) : null,
    );
  }

  // The slide files of a week (bound to the week or to one of its
  // lectures; the back end lists them per lecture): a count that opens them.
  function weekSlides(g) {
    const seen = new Map();
    for (const l of [...g.lectures, ...(g.children || [])]) for (const m of l.materials || []) if (m.via !== 'course' && !seen.has(m.id)) seen.set(m.id, m);
    const mats = [...seen.values()];
    if (!mats.length) return null;
    const b = h(
      'button',
      { type: 'button', class: 'lt-group-mat', title: t('lecture.group.slides_tip', { week: g.week }), dataset: { fkey: `lt:slides:${g.week}` } },
      icon('layers', { size: 13 }),
      t('lecture.group.slides', { n: mats.length }),
    );
    return h(
      'span',
      { class: 'lt-group-end', role: 'cell' },
      menuButton(b, () =>
        mats.map((m) => ({
          label: m.filename,
          icon: 'file-text',
          hint: m.via === 'lecture' ? t('lecture.group.slides_one') : '',
          onSelect: () => opts.onShowMaterial && opts.onShowMaterial(m.id),
        })),
      ),
    );
  }

  // The calm W?? hint of P13.4; it opens the setting it needs. Teaching
  // materials need no teaching week (a past lecture is before the term).
  function calendarHint(lec) {
    const k = lec.calendar_hint;
    if (lec.numbered || !k || isMaterial(lec)) return null;
    const onclick =
      k === 'recorded_at'
        ? () => openRecordedAtDialog(lec, { onSaved: (l) => opts.onLectures([l]) })
        : () => opts.onCourseSettings && opts.onCourseSettings();
    return h('button', { type: 'button', class: 'chip chip-cal', title: t(`lecture.hint.${k}_tip`), dataset: { fkey: `lec:${lec.id}:cal` }, onclick }, icon('calendar', { size: 11 }), t(`lecture.hint.${k}`));
  }

  // The kind cell's hover text: kind and week in full, recording time,
  // citation number, where the kind came from.
  function kindTitle(lec) {
    const when = fmtRecordedAt(lec);
    const cite = lec.cite_label || lec.label;
    return [
      lectureTagText(lec, { form: 'full' }),
      when ? t('lecture.kind_tip.recorded', { when }) : t('lecture.kind_tip.no_time'),
      cite ? t('lecture.kind_tip.cite', { cite }) : null,
      kindSourceText(lec.kind_source),
    ]
      .filter(Boolean)
      .join('\n');
  }

  // week: the teaching week of the row's group (a drop target for
  // supplements, course-ux); child: an indented supplement of that week.
  function row(lec, lang = titleLang(), week = 0, child = false) {
    const fk = `lec:${lec.id}`;
    const tt = lectureTitles(lec, lang);
    const isSel = selected.has(lec.id);
    const pipe = pipelineBar(lec.stages, {
      key: fk,
      lecture: lec.id, // a remote asr's live steps (pipeline.js)
      sys: ctx.system,
      res: ctx.resources,
      stream: lec.stream, // stream-parallel: the playback file with prep
      onAction: (stage, action) => done(A.stageAction(lec, stage, action, true)),
      // P2 hotspot 3 (round 8 WL-02): a card's content is built when it
      // first opens; a stage its card describes (an error, a waiting
      // reason) has it at once (40 lectures on the test stand-in: 5093 →
      // 2513 elements in the list)
      lazyCards: true,
    });
    const st = statusLine(lec);
    const statusEl = h('span', { class: `lrow-status tone-${st.sum.tone}`, title: st.text }, st.text);
    const metaEl = st.meta.length ? h('span', { class: 'lrow-meta', title: st.meta.join(' · ') }, st.meta.join(' · ')) : null;
    refs.set(lec.id, { bar: pipe, statusEl, metaEl, lec });
    // a retry's countdown goes on by the second (3.2.0 L12)
    if (U.backoffCounting(lec.stages))
      everySecond(statusEl, () => {
        const s = statusLine(lec);
        statusEl.textContent = s.text;
        statusEl.title = s.text;
        return U.backoffCounting(lec.stages);
      });
    const check = h('input', { type: 'checkbox', class: 'check', checked: isSel, 'aria-label': t('course.lectures.select', { label: lectureTagText(lec), title: lec.title }), dataset: { fkey: `${fk}:check` }, onclick: (e) => onCheck(lec, e) });
    const allDone = U.isAllDone(lec.stages);
    // SPEC-v2 P1: once everything is done the row's action is "play" and
    // the downloads live in the context menu (the export submenu).
    const dl = lec.exports.length && !(allDone && lec.playable)
      ? menuButton(
          iconBtn('download', t('course.lectures.download_label', { title: lec.title }), { fkey: `${fk}:dl`, fnext: endNext(fk, 'dl'), title: allDone ? t('course.lectures.download_tip') : t('course.lectures.download_tip_outdated') }),
          () => A.exportItems(lec.exports),
        )
      : null;
    const more = menuButton(iconBtn('ellipsis', t('course.lectures.more_label', { title: lec.title }), { fkey: `${fk}:more`, menu: true }), () => menuFor(lec));
    // course-ux: a teaching material's drag handle; Enter opens the weeks
    const hangable = canHang(lec, course());
    const grip = hangable
      ? h(
          'button',
          {
            type: 'button',
            class: 'lrow-grip',
            'aria-label': t('course.week.grip', { title: lec.title }),
            title: t('course.week.grip', { title: lec.title }),
            'aria-haspopup': 'menu',
            dataset: { fkey: `${fk}:grip` },
            onclick: (e) => openMenu(hangItems(lec, course(), lectures, (l) => opts.onLectures([l])), { anchor: e.currentTarget }),
          },
          icon('grip-vertical', { size: 14 }),
        )
      : null;
    const checkCell = h('span', { class: 'lt-check', role: 'cell' }, grip, check);
    cellClicks(checkCell, check, (shiftKey) => {
      check.checked = !selected.has(lec.id); // the box toggled, as a click on it leaves it
      onCheck(lec, { target: check, shiftKey });
    });
    const el = h(
      'div',
      {
        class: 'lrow' + (isSel ? ' is-selected' : '') + (lec.draft ? ' is-draft' : '') + (hangable ? ' can-hang' : '') + (child ? ' is-child' : ''),
        role: 'row',
        dataset: week ? { id: lec.id, week, dropWeek: week } : { id: lec.id },
      },
      checkCell,
      h('span', { class: 'lrow-kind', role: 'cell', title: kindTitle(lec) }, kindTag(lec)),
      h(
        'div',
        { class: 'lrow-main', role: 'cell' },
        h(
          'div',
          { class: 'lrow-title-line' },
          titleCell(lec, tt, lang),
          tt.sub && editing !== lec.id ? h('span', { class: 'lrow-subtitle', title: tt.sub, lang: tt.subLang }, tt.sub) : null, // the Chinese title (fonts, P14.5; script, P14.6)
          makeupTag(lec),
          lec.draft ? h('span', { class: 'chip chip-draft' }, t('course.lectures.draft')) : null,
          calendarHint(lec),
        ),
        child ? h('span', { class: 'visually-hidden' }, t('course.week.child', { week })) : null,
        editing === lec.id && renamesZh(lec) ? zhEditNote(`lec-${lec.id}-zh-note`) : null, // P14.6: the editor holds the stored Simplified title
        tt.blurb ? h('div', { class: 'lrow-blurb', title: tt.blurbFull, lang: tt.blurbLang }, tt.blurb) : null,
        h('div', { class: 'lrow-sub' }, statusEl, st.warn ? h('span', { class: 'chip chip-warn', title: t('course.lectures.warn_tip') }, st.warn) : null, metaEl),
      ),
      h('div', { class: 'lrow-pipe', role: 'cell' }, pipe),
      h(
        'div',
        { class: 'lrow-end', role: 'cell' },
        primaryAction(lec),
        // T16 D3: the review chip, the downloads and the menu stay together
        // on one line; at phone width, when they do not fit beside the
        // primary action, they wrap to the second line as a whole (plain
        // wrap, style.css 480px). Where even that line is too narrow (360
        // wide, T18 N1) the review chip and the downloads + menu unit
        // (.lrow-dlmenu, never split: the menu is never alone) take a line
        // each inside the cell. Always in this DOM order (the Tab and
        // reading order, R34b)
        h(
          'span',
          { class: 'lrow-tail' },
          lec.counts.patches_pending > 0
            ? h(
                'a',
                { class: 'chip chip-review', href: `#/lecture/${enc(lec.id)}/review`, title: lec.reviewable ? t('course.lectures.review_tip') : t('course.lectures.review_busy_tip'), dataset: { fkey: `${fk}:review`, fnext: endNext(fk, 'review').join(' ') } },
                t('course.lectures.review', { n: lec.counts.patches_pending }),
              )
            : null,
          h('span', { class: 'lrow-dlmenu' }, dl, more),
        ),
      ),
    );
    bindContextMenu(el, (e) => (editing === lec.id ? null : menuFor(lec)));
    el.addEventListener('keydown', (e) => onRowKey(e, lec, el));
    if (grip) dragSource(el, grip, lec);
    refs.get(lec.id).el = el;
    return el;
  }

  // What the group rows show of a list (groupLectures): their titles, the
  // lectures in each and the slide files of a week; equal keys draw the
  // same group rows and the same rows in the same places.
  function groupsKey(list) {
    return JSON.stringify(
      groupLectures(list).map((g) => [
        g.key,
        g.title,
        g.hint || '',
        g.week || 0,
        g.lectures.map((l) => l.id),
        (g.children || []).map((l) => l.id),
        [...g.lectures, ...(g.children || [])].flatMap((l) => (l.materials || []).filter((m) => m.via !== 'course').map((m) => [m.id, m.filename, m.via])),
      ]),
    );
  }

  // P2 hotspot 3: one lecture changed (its SSE refresh, an action's answer).
  // Rebuilding the whole list took 180–640 ms at 40 lectures with the CPU
  // slowed 4x; its row alone is redrawn when it stays where it was (the
  // same place in the list, the same group rows): → true. The selection,
  // the other rows and the keyboard focus stay (preserveFocus). false: the
  // caller renders the whole list (the lecture is not shown, it moved to
  // another place or group, a rename or a drag is on).
  function updateRow(lec) {
    // a search narrows the list (the caller's filter): the lecture may show or not now
    if (drag || editing || (opts.filtered && opts.filtered())) return false;
    const i = lectures.findIndex((l) => l.id === lec.id);
    const ref = refs.get(lec.id);
    if (i < 0 || !ref || !ref.el || !ref.el.isConnected || lectures[i].seq !== lec.seq) return false;
    const next = lectures.slice();
    next[i] = lec;
    if (groupsKey(next) !== groupsKey(lectures)) return false;
    const g = groupLectures(next).find((x) => x.lectures.includes(lec) || (x.children || []).includes(lec));
    if (!g) return false;
    lectures = next;
    preserveFocus(() => {
      ref.el.replaceWith(row(lec, titleLang(), g.week, (g.children || []).includes(lec)));
      if (selected.has(lec.id)) renderBar(); // the selection's counts (start, update, …)
    });
    return true;
  }

  function menuFor(lec) {
    const sel = selectedLectures();
    if (selected.has(lec.id) && sel.length > 1) return batchItems(sel);
    const items = A.lectureMenuItems(lec, menuHandlers);
    // T25 X1: the menu's "update" asks first, as the row's does
    const up = items.findIndex((x) => x && x.id === 'update');
    if (up >= 0) items[up] = { ...items[up], onSelect: () => done(updateConfirmed([lec])) };
    // course-ux: hang under a week / make-up, after "change kind"
    const extra = weekMenuItems(lec, course(), lectures, (l) => opts.onLectures([l]));
    if (extra.length) {
      const i = items.findIndex((x) => x && x.id === 'kind');
      items.splice(i < 0 ? items.length : i + 1, 0, ...extra);
    }
    return items;
  }

  // ---- course-ux: dragging a teaching material under a week ----

  // The row is draggable only from its handle (text stays selectable).
  function dragSource(el, grip, lec) {
    grip.addEventListener('pointerdown', () => (el.draggable = true));
    el.addEventListener('dragstart', (e) => {
      if (!el.draggable) return;
      drag = { lec };
      el.classList.add('is-dragging');
      table.classList.add('is-dragging');
      table.classList.toggle('can-unhang', !!lec.week_override);
      e.dataTransfer.effectAllowed = 'move';
      try {
        e.dataTransfer.setData('text/plain', lec.title);
      } catch {
        // the lecture is kept in drag
      }
    });
    el.addEventListener('dragend', () => {
      el.draggable = false;
      el.classList.remove('is-dragging');
      endDrag();
    });
  }

  function endDrag() {
    if (!drag) return;
    drag = null;
    table.classList.remove('is-dragging', 'can-unhang');
    highlight(null);
    if (pendingList) {
      const l = pendingList;
      pendingList = null;
      render(l);
    }
  }

  const hasFiles = (e) => Array.from((e.dataTransfer && e.dataTransfer.types) || []).includes('Files');

  // The week a drop at e lands in: {week} (0 = detach), or null.
  function dropAt(e) {
    const tgt = e.target && e.target.closest ? e.target.closest('[data-drop-week], .lt-unhang') : null;
    if (!tgt || !body.contains(tgt)) return null;
    if (tgt.classList.contains('lt-unhang')) return drag.lec.week_override ? { week: 0 } : null;
    const week = Number(tgt.dataset.dropWeek);
    return week > 0 ? { week } : null;
  }

  function highlight(week) {
    for (const x of body.querySelectorAll('.is-drop')) x.classList.remove('is-drop');
    if (week == null) return;
    for (const x of body.querySelectorAll(week ? `[data-drop-week="${week}"]` : '.lt-unhang')) x.classList.add('is-drop');
  }

  body.addEventListener('dragover', (e) => {
    if (!drag || hasFiles(e)) return;
    const d = dropAt(e);
    highlight(d ? d.week : null);
    if (!d) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
  });
  body.addEventListener('dragleave', (e) => {
    if (drag && !body.contains(e.relatedTarget)) highlight(null);
  });
  body.addEventListener('drop', (e) => {
    if (!drag || hasFiles(e)) return;
    const d = dropAt(e);
    const lec = drag.lec;
    if (d) e.preventDefault();
    endDrag();
    if (!d) return;
    (d.week ? hangUnder(lec, d.week) : clearOverride(lec)).then((l) => l && opts.onLectures([l]));
  });

  function onRowKey(e, lec, el) {
    if (e.target.closest('input[type="text"], textarea')) return;
    if (e.key === 'F2') {
      e.preventDefault();
      startRename(lec.id);
    } else if (e.key === 'Delete') {
      e.preventDefault();
      const sel = selectedLectures();
      const list = selected.has(lec.id) && sel.length > 1 ? sel : [lec];
      A.deleteLectures(list).then(
        (ids) => ids.length && opts.onDeleted(ids),
        () => {},
      );
    } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      const tgt = e.target;
      const kind = tgt.classList.contains('check') ? '.check' : tgt.classList.contains('lrow-title') ? '.lrow-title' : null;
      if (!kind) return;
      let next = e.key === 'ArrowDown' ? el.nextElementSibling : el.previousElementSibling;
      while (next && !next.classList.contains('lrow')) next = e.key === 'ArrowDown' ? next.nextElementSibling : next.previousElementSibling; // group headers
      const target = next && next.querySelector(kind);
      if (target) {
        e.preventDefault();
        target.focus();
      }
    }
  }

  // ---- inline rename (A12.2 rename; also F2) ----

  // The longest title (title or title_zh) the rename input takes.
  const TITLE_MAX = 200;

  // The field renamed is the one shown as the main title (title_zh when the
  // title language is Chinese and there is one).
  const renamesZh = (lec) => titleLang() === 'zh' && !!(lec.title_zh || '').trim();

  function startRename(id) {
    const lec = lectures.find((l) => l.id === id);
    if (!lec) return;
    editing = id;
    draft = renamesZh(lec) ? lec.title_zh : lec.title;
    focusTitleInput = true;
    render(lectures);
  }

  function titleCell(lec, tt = lectureTitles(lec), lang = titleLang()) {
    if (editing !== lec.id) {
      const full = [lec.title, lec.title_zh].filter(Boolean).map(zh).join('\n');
      return h('a', { class: 'lrow-title', href: `#/lecture/${enc(lec.id)}`, title: full, lang: tt.mainLang, dataset: { fkey: `lec:${lec.id}:title` } }, tt.main);
    }
    const zhNote = renamesZh(lec) && zhOn();
    const input = h('input', {
      class: 'input lrow-title-input',
      value: draft,
      // the server keeps at most 200 characters (media.MaxTitleRunes);
      // maxlength counts UTF-16 units, so it never lets through more
      maxlength: String(TITLE_MAX),
      'aria-label': t('course.lectures.title_input'),
      'aria-describedby': zhNote ? `lec-${lec.id}-zh-note` : undefined,
      dataset: { fkey: `lec:${lec.id}:title-input` },
    });
    let finished = false;
    const finish = async (save, refocus) => {
      if (finished) return;
      finished = true;
      const val = draft.trim();
      const zh = renamesZh(lec);
      if (save && val && val !== (zh ? lec.title_zh : lec.title)) {
        try {
          const updated = await api.patchLecture(lec.id, zh ? { title_zh: val } : { title: val });
          editing = null;
          if (updated) opts.onLectures([updated]);
        } catch {
          // shown by the API layer
        }
      }
      editing = null;
      render(lectures);
      if (refocus) {
        const b = body.querySelector(`[data-fkey="${CSS.escape(`lec:${lec.id}:title`)}"]`);
        if (b) b.focus();
      }
    };
    input.addEventListener('input', () => (draft = input.value));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.isComposing) {
        e.preventDefault();
        finish(true, true);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        finish(false, true);
      }
    });
    // A blur caused by a re-render (element already detached) is not a save.
    input.addEventListener('blur', () => {
      if (input.isConnected) finish(true, false);
    });
    return input;
  }

  // ---- batch (A7) ----

  function counts(sel) {
    const has = (a) => sel.filter((l) => (l.lecture_actions || []).includes(a)).length;
    return { launch: has('launch'), cancel: has('cancel'), update: has('update') };
  }

  function batchItems(sel) {
    const c = counts(sel);
    return [
      c.launch ? { label: t('course.lectures.batch.launch_n', { n: c.launch }), icon: 'play', onSelect: () => batch(() => launchConfirmed(sel)) } : null,
      c.cancel ? { label: t('course.lectures.batch.cancel_n', { n: c.cancel }), icon: 'ban', onSelect: () => batch(() => A.cancelLectures(sel)) } : null,
      c.update ? { label: t('course.lectures.batch.update_n', { n: c.update }), icon: 'refresh-cw', onSelect: () => batch(() => updateConfirmed(sel)) } : null,
      rerunStages(sel).length ? { label: t('course.lectures.batch.rerun_menu'), icon: 'rotate-cw', onSelect: () => rerun(sel) } : null,
      'sep',
      { label: t('course.lectures.batch.delete_n', { n: sel.length }), icon: 'trash-2', danger: true, onSelect: () => batchDelete(sel) },
    ];
  }

  function batch(fn) {
    return fn().then(
      (ls) => ls && ls.length && opts.onLectures(ls),
      () => {},
    );
  }

  // course-ux: one stage of the selected lectures, confirmed once
  function rerun(sel) {
    const c = course();
    return batch(() => batchRerun(c ? c.id : sel[0].course_id, sel));
  }

  function batchDelete(sel) {
    return A.deleteLectures(sel).then(
      (ids) => {
        if (!ids.length) return;
        for (const id of ids) selected.delete(id);
        opts.onDeleted(ids);
      },
      () => {},
    );
  }

  function renderBar() {
    const sel = selectedLectures();
    const was = !bar.hidden;
    clear(bar);
    if (!sel.length) {
      bar.hidden = true;
      bar.classList.remove('is-in');
      return;
    }
    const c = counts(sel);
    const filtered = !!(opts.filtered && opts.filtered());
    append(bar, [
      h('span', { class: 'batchbar-count' }, countNodes('course.lectures.batch.selected', sel.length, (num) => h('b', null, num))),
      // course-ux: "select the whole course" (the shown lectures while a search narrows the list)
      sel.length < lectures.length
        ? h(
            'button',
            { type: 'button', class: 'batchbar-all', dataset: { fkey: 'batch:all' }, onclick: () => toggleAll(true) },
            filtered ? t('course.lectures.batch.select_shown', { n: U.count(lectures.length) }) : t('course.lectures.batch.select_course', { n: U.count(lectures.length) }),
          )
        : null,
      h('span', { class: 'batchbar-sep' }),
      c.launch ? btn(t('course.lectures.batch.launch'), { size: 'sm', kind: 'primary', icon: 'play', count: c.launch, fkey: 'batch:launch', onclick: () => batch(() => launchConfirmed(sel)) }) : null,
      c.cancel ? btn(t('course.lectures.batch.cancel'), { size: 'sm', kind: 'inverse', icon: 'ban', count: c.cancel, fkey: 'batch:cancel', onclick: () => batch(() => A.cancelLectures(sel)) }) : null,
      c.update ? btn(t('course.lectures.batch.update'), { size: 'sm', kind: 'inverse', icon: 'refresh-cw', count: c.update, fkey: 'batch:update', onclick: () => batch(() => updateConfirmed(sel)) }) : null,
      rerunStages(sel).length ? btn(t('course.lectures.batch.rerun'), { size: 'sm', kind: 'inverse', icon: 'rotate-cw', fkey: 'batch:rerun', onclick: () => rerun(sel) }) : null,
      btn(t('course.lectures.batch.delete'), { size: 'sm', kind: 'inverse-danger', icon: 'trash-2', count: sel.length, fkey: 'batch:delete', onclick: () => batchDelete(sel) }),
      h('span', { class: 'batchbar-sep' }),
      iconBtn('x', t('course.lectures.batch.clear'), {
        class: 'icon-btn-inverse',
        fkey: 'batch:clear',
        onclick: () => {
          selected.clear();
          render(lectures);
        },
      }),
    ]);
    bar.hidden = false;
    if (!was) requestAnimationFrame(() => bar.classList.add('is-in'));
    else bar.classList.add('is-in');
  }

  // R78 low 3: the texts of the rows that change with time ("running for
  // 3 min", "uploaded 5 min ago") are worked out again without redrawing
  // the rows: rows are no longer redrawn by every other lecture's refresh
  // (updateRow). The course page calls it with its minute tick.
  function refreshTimes() {
    for (const ref of refs.values()) {
      if (!ref.el || !ref.el.isConnected) continue;
      const s = statusLine(ref.lec);
      ref.statusEl.textContent = s.text;
      ref.statusEl.title = s.text;
      if (ref.metaEl && s.meta.length) {
        const m = s.meta.join(' · ');
        ref.metaEl.textContent = m;
        ref.metaEl.title = m;
      }
    }
  }

  // In-place progress from SSE (keeps hover cards and focus).
  function setProgress(ev) {
    const ref = refs.get(ev.scope_id);
    if (!ref) return false;
    if (streamProgress(ref.lec, ref.bar, ev)) return true; // the background playback file
    const st = ref.lec.stages.find((s) => s.stage === ev.stage);
    if (!st || st.status !== ev.status) return false;
    ref.bar.setProgress(ev.stage, ev.progress);
    const s = statusLine(ref.lec);
    ref.statusEl.textContent = s.text;
    ref.statusEl.title = s.text;
    return true;
  }

  function has(id) {
    return refs.has(id);
  }

  return { table, bar, render, updateRow, refreshTimes, setProgress, has, openMenuFor: (lec, anchorEl) => openMenu(menuFor(lec), { anchor: anchorEl }) };
}

// A legend of the stage states (colour + pattern), shown under the list.
export function stateLegend() {
  const items = ['pending', 'queued', 'running', 'done', 'outdated', 'stale', 'failed', 'cancelled'].map((st) => [st, t(`course.lectures.legend.${st}`)]);
  return h(
    'div',
    { class: 'legend', 'aria-label': t('course.lectures.legend.label') },
    items.map(([st, label]) => h('span', { class: 'legend-item' }, h('span', { class: 'legend-swatch seg', dataset: { st } }, h('span', { class: 'seg-bar' }, h('span', { class: 'seg-fill', style: { width: st === 'running' ? '60%' : '' } }))), label)),
  );
}
