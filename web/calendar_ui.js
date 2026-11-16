// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Course calendar and lecture numbering in the UI (SPEC-v2 P13.4, P13.6):
// date helpers, the teaching-week calendar (term start + weeks that do not
// count), the course settings dialog, the "numbers will change"
// confirmation of a 409 RenumberConflict, the recording-time dialog and the
// lecture title language preference. Numbers themselves (W02L1) are always computed by the back
// end; the calendar here only shows which calendar week becomes which
// teaching week while the user edits.

import { api } from './api.js';
import { h, clear, btn, iconBtn, openDialog, preserveFocus, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { fmtMonthDay, fmtWeekday, fmtDayShort, fmtMonth, weekdayNames } from './util.js';
import { zh, zhLang } from './zh_display.js';
import { lectureLabel, lectureTagText } from './kinds.js';

// The date formatters live in util.js (one set of formats for the whole UI,
// P14.4); re-exported for the modules that import them from here.
export { fmtMonthDay, fmtWeekday, fmtDayShort, weekdayNames };

// ---------- dates (local calendar days, no time zone arithmetic) ----------

/** "YYYY-MM-DD" (or the date part of "YYYY-MM-DDTHH:MM") → local Date, or null. */
export function parseYMD(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(String(s || ''));
  if (!m) return null;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  return d.getMonth() === Number(m[2]) - 1 ? d : null;
}

const two = (n) => String(n).padStart(2, '0');

export function ymd(d) {
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())}`;
}

export function addDays(d, n) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n);
}

/** The Monday of d's calendar week (weeks run Monday to Sunday, P13.4). */
export function mondayOf(d) {
  return addDays(d, -((d.getDay() + 6) % 7));
}

/** "6 Oct–12 Oct" for the week starting at monday. */
export function weekRangeText(monday) {
  return t('calendar.range', { from: fmtMonthDay(monday), to: fmtMonthDay(addDays(monday, 6)) });
}

/** "Week 2 · 6 Oct–12 Oct" (P13.4 group title). */
export function weekTitle(week, monday) {
  return t('calendar.week_title', { week, range: weekRangeText(monday) });
}

/** Local "YYYY-MM-DDTHH:MM" → {date, time ('' when only the date is known)}. */
export function splitRecordedAt(s, hasTime) {
  const d = parseYMD(s);
  if (!d) return { date: null, time: '' };
  const m = /T(\d{2}):(\d{2})/.exec(s);
  return { date: d, time: m && hasTime !== false ? `${m[1]}:${m[2]}` : '' };
}

/** "Mon 6 Oct, 14:00" or "Mon 6 Oct" (en) for a lecture's recording time. */
export function fmtRecordedAt(lec) {
  const { date, time } = splitRecordedAt(lec.recorded_at, lec.recorded_has_time);
  if (!date) return '';
  if (!time) return fmtDayShort(date);
  const at = new Date(date.getFullYear(), date.getMonth(), date.getDate(), Number(time.slice(0, 2)), Number(time.slice(3, 5)));
  return fmtDayShort(at, true);
}

// ---------- lecture titles (P13.6 lecture title language) ----------

// The UISettings of SPEC-v2 has no field for it, so the preference lives in
// this browser (NOTES.md, web-course-p2).
const TITLE_LANG_KEY = 'lectureflow.titleLang';
export const TITLE_LANGS = ['both', 'en', 'zh'];

export function titleLang() {
  try {
    const v = localStorage.getItem(TITLE_LANG_KEY);
    return TITLE_LANGS.includes(v) ? v : 'both';
  } catch {
    return 'both';
  }
}

export function setTitleLang(v) {
  if (!TITLE_LANGS.includes(v)) return;
  try {
    localStorage.setItem(TITLE_LANG_KEY, v);
  } catch {
    // private mode: this page only
  }
  window.dispatchEvent(new CustomEvent('titlelangchange', { detail: { lang: v } }));
}

const clean = (s) => (s == null ? '' : String(s).trim());

/**
 * Main title, subtitle and blurb of a lecture row by the title language
 * (P13.6): both = English main, Chinese subtitle; en; zh (English when no
 * Chinese title exists yet). The blurb follows the same setting; in both
 * it is the Chinese one (one line) and the hover text holds both.
 * Chinese is shown in the display script (P14.6 zh(), also any Chinese
 * typed into the English title); mainLang, subLang and blurbLang are the
 * lang attributes of the three parts (fonts, P14.5).
 */
export function lectureTitles(lec, lang = titleLang()) {
  const en = zh(clean(lec.title));
  const zt = zh(clean(lec.title_zh));
  const ben = clean(lec.blurb_en);
  const bzh = zh(clean(lec.blurb_zh));
  const Z = zhLang();
  if (lang === 'en') return { main: en, sub: '', blurb: ben || bzh, blurbFull: ben || bzh, mainLang: 'en', subLang: Z, blurbLang: ben ? 'en' : Z };
  if (lang === 'zh') return { main: zt || en, sub: '', blurb: bzh || ben, blurbFull: bzh || ben, mainLang: zt ? Z : 'en', subLang: Z, blurbLang: bzh ? Z : 'en' };
  return { main: en, sub: zt && zt !== en ? zt : '', blurb: bzh || ben, blurbFull: [ben, bzh].filter(Boolean).join('\n'), mainLang: 'en', subLang: Z, blurbLang: bzh ? Z : 'en' };
}

/** "Week 2 · Tutorial {English title}": a lecture in dialogs (lecture kinds: the kind replaces W02L1). */
export function lectureName(lec) {
  return lectureLabel(lec);
}

// ---------- the teaching-week calendar ----------

/**
 * Term start and the weeks that do not count (P13.4 course settings),
 * shared by the course settings dialog and the add-course wizard (step 1).
 * opts: {termStart, skipWeeks, lectures (for the per-week counts), idPrefix,
 * onChange()}. Returns {el, value() → {term_start, skip_weeks}, changed(course)}.
 */
export function calendarFields(opts) {
  const pre = opts.idPrefix || 'cal';
  let ts = opts.termStart || '';
  const skips = new Set(opts.skipWeeks || []);
  let more = 0; // extra weeks shown below the default range
  const lectures = opts.lectures || [];

  const tsInput = h('input', { type: 'date', class: 'input cal-date', id: `${pre}-term`, value: ts, 'aria-describedby': `${pre}-term-hint`, dataset: { fkey: `${pre}:term` } });
  const tsNote = h('span', { class: 'field-hint', role: 'status' });
  const clearBtn = btn(t('calendar.term_start.clear'), { size: 'sm', kind: 'ghost', icon: 'x', fkey: `${pre}:term-clear`, onclick: () => setTermStart('') });
  const grid = h('div', { class: 'cal-grid-wrap' });

  tsInput.addEventListener('change', () => setTermStart(tsInput.value));

  function setTermStart(v) {
    const d = parseYMD(v);
    const mon = d ? ymd(mondayOf(d)) : '';
    const aligned = !!d && mon !== v;
    ts = mon;
    tsInput.value = mon;
    syncTermNote(aligned);
    renderGrid();
    if (opts.onChange) opts.onChange();
  }

  function syncTermNote(aligned) {
    clearBtn.hidden = !ts;
    const d = parseYMD(ts);
    tsNote.textContent = d ? t(aligned ? 'calendar.term_start.aligned' : 'calendar.term_start.week1', { range: weekRangeText(d) }) : t('calendar.term_start.unset');
    tsNote.classList.toggle('is-quiet', !d);
  }

  // per calendar week (Monday "YYYY-MM-DD") → number of lectures recorded in it
  function lectureCounts() {
    const out = new Map();
    for (const l of lectures) {
      const d = parseYMD(l.recorded_at);
      if (!d) continue;
      const k = ymd(mondayOf(d));
      out.set(k, (out.get(k) || 0) + 1);
    }
    return out;
  }

  function weekdayHeads() {
    const mon = new Date(2024, 0, 1); // a Monday
    return Array.from({ length: 7 }, (_, i) => h('span', { class: 'cal-wd', 'aria-hidden': 'true' }, fmtWeekday(addDays(mon, i), 'narrow')));
  }

  function renderGrid() {
    preserveFocus(() => {
      clear(grid);
      const start = parseYMD(ts);
      if (!start) {
        grid.append(h('p', { class: 'cal-empty' }, icon('calendar', { size: 15 }), t('calendar.skip.need_term_start')));
        return;
      }
      const counts = lectureCounts();
      // default: 16 weeks, or up to the last lecture / skipped week
      let last = addDays(start, 15 * 7);
      for (const k of [...counts.keys(), ...skips]) {
        const d = parseYMD(k);
        if (d && d > last) last = d;
      }
      last = addDays(last, more * 7);
      const today = ymd(new Date());
      const rows = [];
      let n = 0;
      for (let mon = start; mon <= last; mon = addDays(mon, 7)) {
        const key = ymd(mon);
        const skipped = skips.has(key);
        if (!skipped) n++;
        const c = counts.get(key) || 0;
        const days = Array.from({ length: 7 }, (_, i) => {
          const d = addDays(mon, i);
          const first = d.getDate() === 1 || (rows.length === 0 && i === 0);
          return h('span', { class: 'cal-day' + (ymd(d) === today ? ' is-today' : '') + (i >= 5 ? ' is-weekend' : '') }, first ? h('span', { class: 'cal-month' }, fmtMonth(d)) : null, String(d.getDate()));
        });
        const label = skipped ? t('calendar.skip.row_skipped', { range: weekRangeText(mon) }) : t('calendar.skip.row_counted', { week: n, range: weekRangeText(mon) });
        rows.push(
          h(
            'button',
            {
              type: 'button',
              class: 'cal-week' + (skipped ? ' is-skipped' : ''),
              'aria-pressed': skipped ? 'true' : 'false',
              'aria-label': c ? `${label} ${t('calendar.skip.lectures', { n: c })}` : label,
              title: skipped ? t('calendar.skip.click_count') : t('calendar.skip.click_skip'),
              dataset: { fkey: `${pre}:week:${key}` },
              onclick: () => {
                if (skips.has(key)) skips.delete(key);
                else skips.add(key);
                renderGrid();
                if (opts.onChange) opts.onChange();
              },
            },
            h('span', { class: 'cal-wk' }, skipped ? t('calendar.skip.short') : `W${two(n)}`),
            days,
            h('span', { class: 'cal-count' }, c ? t('calendar.skip.lectures', { n: c }) : ''),
          ),
        );
      }
      grid.append(
        h('div', { class: 'cal-grid', role: 'group', 'aria-label': t('calendar.skip.label') }, h('div', { class: 'cal-head', 'aria-hidden': 'true' }, h('span', { class: 'cal-wk' }), weekdayHeads(), h('span', { class: 'cal-count' })), rows),
        h('div', { class: 'cal-foot' }, btn(t('calendar.skip.more'), { size: 'sm', kind: 'ghost', icon: 'chevron-down', fkey: `${pre}:more`, onclick: () => ((more += 4), renderGrid()) }), skips.size ? h('span', { class: 'muted small' }, t('calendar.skip.count', { n: [...skips].filter((k) => k >= ts).length })) : null),
      );
    });
  }

  syncTermNote(false);
  renderGrid();

  const el = h(
    'div',
    { class: 'cal-fields' },
    h(
      'div',
      { class: 'field' },
      h('label', { class: 'field-label', for: `${pre}-term` }, t('calendar.term_start.label'), h('span', { class: 'field-opt' }, t('calendar.optional'))),
      h('div', { class: 'cal-term-row' }, tsInput, clearBtn),
      h('span', { class: 'field-hint', id: `${pre}-term-hint` }, t('calendar.term_start.hint')),
      tsNote,
    ),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, t('calendar.skip.label'), h('span', { class: 'field-opt' }, t('calendar.optional'))), h('span', { class: 'field-hint' }, t('calendar.skip.hint')), grid),
  );

  const value = () => ({ term_start: ts, skip_weeks: [...skips].filter((k) => !ts || k >= ts).sort() });
  return {
    el,
    value,
    /** The CoursePatch fields that differ from course ({} when nothing changed). */
    patchFor(course) {
      const v = value();
      const p = {};
      if (v.term_start !== (course && course.term_start ? course.term_start : '')) p.term_start = v.term_start;
      const old = [...((course && course.skip_weeks) || [])].filter((k) => !v.term_start || k >= v.term_start).sort();
      if (JSON.stringify(old) !== JSON.stringify(v.skip_weeks)) p.skip_weeks = v.skip_weeks;
      return p;
    },
  };
}

// ---------- 409 RenumberConflict (P13.6) ----------

export function isRenumberConflict(err) {
  const e = err && err.body && err.body.error;
  return !!err && err.status === 409 && !!e && typeof e === 'object' && e.code === 'calendar.renumber';
}

// The message of a group of renumbering notices, by code: the one-time check
// after the past-lecture rule shipped, or (any other code) a kind the title
// task set by itself.
function renumberGroupText(code, n) {
  return code === 'calendar.renumbered_upgrade' ? t('notes.calendar.renumbered_upgrade', { n }) : t('notes.calendar.renumbered_auto', { n });
}

/**
 * The course page's notice of automatic renumbering after a citation export
 * (Course.renumber_notices), or null. Codes: calendar.renumbered_auto (a
 * kind the title task set by itself changed other lectures' citation
 * numbers) and calendar.renumbered_upgrade (the one-time check when the
 * past-lecture rule shipped). The notices are grouped by code, groups in
 * the order they first occurred, each with its own message; within a group
 * each lecture appears once, from its first old number to its latest one.
 * One × dismisses them all. lectures: the course's lectures (titles);
 * opts.onDismissed(course).
 */
export function renumberNotice(course, lectures, opts = {}) {
  const notices = (course && course.renumber_notices) || [];
  const groups = new Map(); // code -> Map(lecture id -> {id, from, to})
  for (const n of notices) {
    const code = n && n.code === 'calendar.renumbered_upgrade' ? n.code : 'calendar.renumbered_auto';
    if (!groups.has(code)) groups.set(code, new Map());
    const byId = groups.get(code);
    for (const c of (n && n.params && Array.isArray(n.params.changes) && n.params.changes) || []) {
      if (!c || !c.lecture_id) continue;
      const prev = byId.get(c.lecture_id);
      byId.set(c.lecture_id, { id: c.lecture_id, from: prev ? prev.from : c.from, to: c.to });
    }
  }
  const shown = [...groups.entries()].map(([code, byId]) => ({ code, changes: [...byId.values()].filter((c) => c.from !== c.to) })).filter((g) => g.changes.length);
  if (!shown.length) return null;
  const titles = new Map((lectures || []).map((l) => [l.id, l.title]));
  const err = h('span', { class: 'fn-notice-err', role: 'alert' });
  const close = iconBtn('x', t('course.renumbered.dismiss'), {
    class: 'fn-notice-close',
    fkey: 'renum:dismiss',
    onclick: async () => {
      close.disabled = true;
      err.textContent = '';
      try {
        const c = await api.dismissRenumberNotices(course.id, { quiet: true });
        if (opts.onDismissed) opts.onDismissed(c);
      } catch (ex) {
        close.disabled = false;
        err.textContent = ex.message;
      }
    },
  });
  return h(
    'div',
    { class: 'fn-notice renum-notice', role: 'status' },
    icon('info', { size: 15, class: 'fn-notice-icon' }),
    h(
      'div',
      { class: 'fn-notice-text' },
      shown.map((g) =>
        h(
          'div',
          { class: 'renum-notice-group', dataset: { code: g.code } },
          h('p', { class: 'renum-notice-msg' }, renumberGroupText(g.code, g.changes.length)),
          h(
            'ul',
            { class: 'renum-list renum-notice-list', 'aria-label': t('calendar.renumber.list') },
            g.changes.map((c) => {
              const title = titles.has(c.id) ? zh(titles.get(c.id)) : '';
              return h('li', null, h('span', { class: 'renum-from mono' }, c.from), icon('arrow-right', { size: 13, class: 'dim' }), h('span', { class: 'renum-to mono' }, c.to), title ? h('span', { class: 'renum-title', title }, title) : null);
            }),
          ),
        ),
      ),
      h('p', { class: 'renum-notice-hint' }, t('calendar.renumber.hint')),
      err,
    ),
    close,
  );
}

/** Lists the number changes; resolves to true when the user confirms. */
export function renumberDialog(changes) {
  const list = changes || [];
  return openDialog(t('calendar.renumber.title'), (close) =>
    h(
      'div',
      null,
      h('p', { class: 'dlg-msg' }, t('calendar.renumber.message', { n: list.length })),
      h(
        'div',
        { class: 'dlg-list-wrap' },
        h(
          'ul',
          { class: 'dlg-list renum-list', 'aria-label': t('calendar.renumber.list') },
          list.map((c) => h('li', null, h('span', { class: 'renum-from mono' }, c.from), icon('arrow-right', { size: 13, class: 'dim' }), h('span', { class: 'renum-to mono' }, c.to), h('span', { class: 'renum-title', title: c.title }, c.title))),
        ),
      ),
      h('p', { class: 'dlg-hint' }, t('calendar.renumber.hint')),
      h(
        'div',
        { class: 'dlg-actions' },
        h('button', { type: 'button', class: 'btn', autofocus: true, onclick: () => close(false) }, t('common.cancel')),
        h('button', { type: 'button', class: 'btn btn-primary', onclick: () => close(true) }, t('calendar.renumber.ok')),
      ),
    ),
  ).then((v) => v === true);
}

/**
 * Sends a change that may renumber lectures: send(confirm) must make a quiet
 * request. On 409 calendar.renumber the user sees the changes and the
 * request is sent again with confirm; resolves to the response, or null when
 * the user kept the old numbers. Other errors are thrown to the caller.
 */
export async function withRenumber(send) {
  try {
    return await send(false);
  } catch (err) {
    if (!isRenumberConflict(err)) throw err;
    const ok = await renumberDialog(err.body.changes);
    if (!ok) return null;
    return send(true);
  }
}

// ---------- course name and code (the new-course wizard, the course settings) ----------

// The limits of the server (POST / PATCH /api/courses: course.name_too_long,
// course.code_too_long, course.code_has_space).
export const COURSE_NAME_MAX = 200;
export const COURSE_CODE_MAX = 32;

/** What is wrong with a course code (trimmed; "" is no code), or "". */
export function courseCodeError(code) {
  if (!code) return '';
  if ([...code].length > COURSE_CODE_MAX) return t('wizard.basics.code_too_long', { max: COURSE_CODE_MAX });
  if (/\s/.test(code)) return t('wizard.basics.code_has_space');
  return '';
}

/** What is wrong with a course name (trimmed), or "". The wizard says "required" beside the field. */
export function courseNameError(name) {
  if ([...name].length > COURSE_NAME_MAX) return t('wizard.basics.name_too_long', { max: COURSE_NAME_MAX });
  return '';
}

// ---------- course settings (P13.4: the course page's menu → course settings) ----------

/**
 * opts: {lectures, onSaved(course), recordingSection(close, busy) → Node,
 * focus: 'name' (T22 B1: "rename…" opens the dialog on the name, selected)
 * or 'term' (the header's term start link)}.
 * The filename-rule part only links to its own dialog (filename_rule.js).
 */
export function openCourseSettings(course, opts = {}) {
  return openDialog(
    t('course.settings.title'),
    (close, busy) => {
      // T22 B1: the name and the code of a course made earlier (a typo, a
      // code added later); the same PATCH as the calendar
      const name0 = course.name || '';
      const code0 = course.code || '';
      const nameErr = h('span', { class: 'field-error', id: 'cset-name-err', role: 'alert' });
      const codeErr = h('span', { class: 'field-error', id: 'cset-code-err', role: 'alert' });
      const nameIn = h('input', {
        class: 'input',
        id: 'cset-name',
        value: name0,
        maxlength: String(COURSE_NAME_MAX + 20),
        autocomplete: 'off',
        'aria-required': 'true',
        'aria-describedby': 'cset-name-err',
        autofocus: opts.focus === 'name',
        dataset: { fkey: 'cset:name' },
      });
      const codeIn = h('input', {
        class: 'input input-code',
        id: 'cset-code',
        value: code0,
        maxlength: String(COURSE_CODE_MAX + 8),
        autocomplete: 'off',
        spellcheck: 'false',
        placeholder: t('wizard.basics.code_placeholder'),
        'aria-describedby': 'cset-code-note cset-code-err',
        dataset: { fkey: 'cset:code' },
      });
      // the dialog is shown (showModal) right after this build: the name, which
      // has the focus then, is selected to be typed over
      if (opts.focus === 'name') queueMicrotask(() => nameIn.isConnected && nameIn.select());
      for (const x of [nameIn, codeIn]) x.addEventListener('input', () => sync());
      // R74 low 8: a code from before codes could not hold spaces is not
      // judged while it is left as it is (the rest can still be saved)
      const basicsError = () => {
        const n = nameIn.value.trim();
        const c = codeIn.value.trim();
        return { name: n ? courseNameError(n) : t('errors.course.name_empty'), code: c === code0 ? '' : courseCodeError(c) };
      };
      const basicsEl = h(
        'div',
        { class: 'cset-basics' },
        h('div', { class: 'dlg-field' }, h('label', { class: 'dlg-label', for: 'cset-name' }, t('wizard.basics.name')), nameIn, nameErr),
        h(
          'div',
          { class: 'dlg-field' },
          h('label', { class: 'dlg-label', for: 'cset-code' }, t('wizard.basics.code'), ' ', h('span', { class: 'dlg-hint' }, t('calendar.optional'))),
          codeIn,
          h('span', { class: 'dlg-hint', id: 'cset-code-note' }, t('course.settings.code_note')),
          codeErr,
        ),
      );
      const cal = calendarFields({ termStart: course.term_start || '', skipWeeks: course.skip_weeks || [], lectures: opts.lectures || [], idPrefix: 'cset', onChange: sync });
      // R74 low 6: the header's "term start" link opens the settings on it
      const termIn = opts.focus === 'term' ? cal.el.querySelector('[data-fkey="cset:term"]') : null;
      if (termIn) termIn.setAttribute('autofocus', '');
      // lecture kinds: when the list shows "No. n" under the kind
      const numbering0 = course.numbering || 'auto';
      let numbering = numbering0;
      const numberingEl = h(
        'div',
        { class: 'cset-num', role: 'radiogroup', 'aria-label': t('course.settings.numbering') },
        ['auto', 'always', 'never'].map((v) =>
          h(
            'label',
            { class: 'dlg-choice cset-num-opt' },
            h('input', {
              type: 'radio',
              name: 'cset-numbering',
              value: v,
              checked: v === numbering,
              dataset: { fkey: `cset:num:${v}` },
              onchange: () => {
                numbering = v;
                sync();
              },
            }),
            h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, t(`course.settings.numbering.${v}`)), h('span', { class: 'dlg-choice-hint' }, t(`course.settings.numbering.${v}_hint`))),
          ),
        ),
      );
      const patchFor = () => {
        const p = cal.patchFor(course);
        if (numbering !== numbering0) p.numbering = numbering;
        const n = nameIn.value.trim();
        const c = codeIn.value.trim();
        if (n !== name0) p.name = n;
        if (c !== code0) p.code = c; // "" clears the code
        return p;
      };
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const save = h('button', { type: 'submit', class: 'btn btn-primary', dataset: { fkey: 'cset:save' } }, t('course.settings.save'));
      function sync() {
        const bad = basicsError();
        nameErr.textContent = bad.name;
        codeErr.textContent = bad.code;
        nameIn.setAttribute('aria-invalid', String(!!bad.name));
        codeIn.setAttribute('aria-invalid', String(!!bad.code));
        save.disabled = !!(bad.name || bad.code) || !Object.keys(patchFor()).length;
        err.textContent = '';
      }
      sync();
      const onSubmit = async (e) => {
        e.preventDefault();
        const p = patchFor();
        if (!Object.keys(p).length || save.disabled) return;
        save.disabled = true;
        save.textContent = t('course.settings.saving');
        try {
          // busy: a route change while it runs lets it finish (R62 N4)
          const c = await busy(withRenumber((confirm) => api.patchCourse(course.id, confirm ? { ...p, confirm: true } : p, { quiet: true })));
          if (!c) {
            save.disabled = false;
            save.textContent = t('course.settings.save');
            return; // kept the old numbers: stay in the dialog
          }
          close(c);
        } catch (ex) {
          save.disabled = false;
          save.textContent = t('course.settings.save');
          err.textContent = ex.message || t('course.settings.failed');
        }
      };
      return h(
        'form',
        { class: 'cset', onsubmit: onSubmit, novalidate: true },
        h('section', { class: 'cset-sec' }, h('h3', { class: 'cset-h' }, icon('pencil', { size: 15 }), t('course.settings.basics')), basicsEl),
        h('section', { class: 'cset-sec' }, h('h3', { class: 'cset-h' }, icon('calendar', { size: 15 }), t('course.settings.calendar')), h('p', { class: 'cset-intro' }, t('course.settings.calendar_intro')), cal.el),
        h('section', { class: 'cset-sec' }, h('h3', { class: 'cset-h' }, icon('hash', { size: 15 }), t('course.settings.numbering')), h('p', { class: 'cset-intro' }, t('course.settings.numbering_intro')), numberingEl),
        opts.recordingSection ? h('section', { class: 'cset-sec' }, h('h3', { class: 'cset-h' }, icon('clock', { size: 15 }), t('course.settings.recording')), opts.recordingSection(close, busy)) : null,
        err,
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), save),
      );
    },
    { wide: true, className: 'dlg-cset' },
  ).then((c) => {
    if (c && opts.onSaved) {
      toast(t('course.settings.saved'), 'success');
      opts.onSaved(c);
    }
    return c;
  });
}

// ---------- recording time of one lecture (P13.3.4 user, P13.4 "set recording time") ----------

/** opts: {onSaved(lecture)} */
export function openRecordedAtDialog(lec, opts = {}) {
  const cur = splitRecordedAt(lec.recorded_at, lec.recorded_has_time);
  return openDialog(
    t('calendar.recorded.title'),
    (close, busy) => {
      const date = h('input', { type: 'date', class: 'input', value: cur.date ? ymd(cur.date) : '', 'aria-label': t('calendar.recorded.date'), autofocus: true, required: true });
      const time = h('input', { type: 'time', class: 'input', value: cur.time, 'aria-label': t('calendar.recorded.time'), step: '60' });
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const save = h('button', { type: 'submit', class: 'btn btn-primary' }, t('calendar.recorded.save'));
      const src = lec.recorded_at_source || 'none';
      const sync = () => {
        save.disabled = !parseYMD(date.value);
        err.textContent = '';
      };
      date.addEventListener('input', sync);
      sync();
      const send = async (value) => {
        save.disabled = true;
        try {
          // busy: a route change while it runs lets it finish (R62 N4)
          const l = await busy(withRenumber((confirm) => api.patchLecture(lec.id, confirm ? { recorded_at: value, confirm: true } : { recorded_at: value }, { quiet: true })));
          if (l) close(l);
          else sync();
        } catch (ex) {
          sync();
          err.textContent = ex.message || t('calendar.recorded.failed');
        }
      };
      return h(
        'form',
        {
          class: 'rec-dlg',
          novalidate: true,
          onsubmit: (e) => {
            e.preventDefault();
            if (save.disabled) return;
            const d = parseYMD(date.value);
            send(time.value ? `${ymd(d)}T${time.value.slice(0, 5)}` : ymd(d));
          },
        },
        h('p', { class: 'dlg-msg' }, h('span', { class: 'rec-name' }, lectureName(lec))),
        h(
          'dl',
          { class: 'rec-facts' },
          h('dt', null, t('calendar.recorded.current')),
          h('dd', null, lec.recorded_at ? fmtRecordedAt(lec) : t('calendar.recorded.none'), h('span', { class: 'chip chip-quiet' }, t(`calendar.recorded.source.${src}`))),
          lec.original_filename ? [h('dt', null, t('calendar.recorded.filename')), h('dd', { class: 'mono rec-file', title: lec.original_filename }, lec.original_filename)] : null,
        ),
        h('div', { class: 'rec-inputs' }, h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label' }, t('calendar.recorded.date')), date), h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label' }, t('calendar.recorded.time'), ' ', h('span', { class: 'dlg-hint' }, t('calendar.optional'))), time)),
        h('p', { class: 'dlg-hint' }, t('calendar.recorded.hint')),
        err,
        h(
          'div',
          { class: 'dlg-actions' },
          src === 'user' ? h('button', { type: 'button', class: 'btn btn-ghost rec-auto', title: t('calendar.recorded.auto_hint'), onclick: () => send('') }, t('calendar.recorded.auto')) : null,
          h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')),
          save,
        ),
      );
    },
    { className: 'dlg-rec' },
  ).then((l) => {
    if (l) {
      toast(t('calendar.recorded.saved_tag', { tag: lectureTagText(l) }), 'success');
      if (opts.onSaved) opts.onSaved(l);
    }
    return l;
  });
}
