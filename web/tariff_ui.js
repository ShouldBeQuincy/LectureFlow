// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Time-of-day pricing in the UI (SPEC-v2 P12, appendix D3): the peak /
// off-peak tag of a provider (resource inspector, settings), its hover text
// with the next change in local time first and the tariff's own zone second
// (P12.4), the countdown and today's peak schedule of the dashboard usage
// card, a one-line summary of a tariff, the tariff editor of the provider
// dialog and the public-holiday editor (P12.2). State and times come from
// the back end (TariffState, pushed every minute with SSE "resources");
// nothing here decides whether it is peak time.

import { h, clear, btn, iconBtn, openDialog } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { fmtHM, fmtDecimal, fmtMonthDay, fmtDayShort, weekdayNames, clauseJoin, upperFirst } from './util.js';
import { ymd, addDays, parseYMD } from './calendar_ui.js';

const BEIJING = 'Asia/Shanghai';

function localZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  } catch {
    return '';
  }
}

/** "Beijing time" for Asia/Shanghai, else "Europe/London time" (tariff.zone.*). */
export function zoneName(tz) {
  return tz === BEIJING ? t('tariff.zone.beijing') : t('tariff.zone.named', { tz });
}

/** The tariff's zone shows the same wall clock as this computer right now. */
function sameClock(date, tz) {
  if (!tz) return true;
  try {
    const f = (zone) => new Intl.DateTimeFormat('en-GB', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(date);
    return f(tz) === f(undefined);
  } catch {
    return false;
  }
}

function hm(date, tz) {
  return fmtHM(date, tz);
}

// "01:00", or "tomorrow 01:00" / "26 Oct Mon 01:00" when not today (local).
function localWhen(date, now = new Date()) {
  const day = ymd(date);
  const time = hm(date);
  if (day === ymd(now)) return time;
  if (day === ymd(addDays(now, 1))) return t('tariff.when.tomorrow', { time });
  return t('tariff.when.day', { day: fmtDayShort(date), time });
}

/** "01:00 local time (09:00 Beijing time)"; just "01:00" when both zones agree. */
export function bothTimes(date, tz, now = new Date()) {
  if (sameClock(date, tz)) return localWhen(date, now);
  return t('tariff.both_times', { local: localWhen(date, now), zone: zoneName(tz), remote: hm(date, tz) });
}

/** Whether the provider's tariff has peak windows at all (a tag is shown). */
export function hasPeaks(ts) {
  return !!ts && (ts.peak || !!ts.next_change);
}

export function tariffTagText(ts) {
  if (ts.peak) return ts.offpeak_only ? t('tariff.tag.peak_paused') : t('tariff.tag.peak');
  const m = Number(ts.offpeak_multiplier);
  return m === 0.5 ? t('tariff.tag.offpeak_half') : t('tariff.tag.offpeak', { m: fmtDecimal(m, 2) });
}

/** Hover text of the tag (P12.4), several lines. */
export function tariffTip(ts, now = new Date()) {
  const lines = [];
  const next = ts.next_change ? new Date(ts.next_change) : null;
  if (next && !isNaN(next)) lines.push(t(ts.peak ? 'tariff.tip.peak' : 'tariff.tip.offpeak', { when: bothTimes(next, ts.timezone, now) }));
  else lines.push(t(ts.peak ? 'tariff.tip.peak_no_end' : 'tariff.tip.offpeak_no_end'));
  if (ts.offpeak_only) lines.push(t(ts.peak ? 'tariff.tip.offpeak_only_paused' : 'tariff.tip.offpeak_only'));
  if (ts.missing_holidays) lines.push(t('tariff.tip.missing_holidays'));
  return lines.join('\n');
}

/** The tag itself: soft green when off-peak, neutral grey at peak (P12.4). */
export function tariffTag(ts, opts = {}) {
  const el = h('span', { class: 'tariff-tag', tabindex: opts.focusable === false ? undefined : '0' });
  updateTariffTag(el, ts);
  return el;
}

/** Updates a tag in place (the inspector keeps the element under the pointer). */
export function updateTariffTag(el, ts, now = new Date()) {
  const text = tariffTagText(ts);
  const tip = tariffTip(ts, now);
  // the CSS tooltip (class tip) shows on hover and on keyboard focus
  el.className = 'tariff-tag tip ' + (ts.peak ? 'is-peak' : 'is-offpeak') + (ts.missing_holidays ? ' has-gap' : '');
  el.dataset.tip = tip;
  el.setAttribute('aria-label', `${text}. ${tip.replace(/\n/g, ' ')}`);
  clear(el);
  el.append(h('span', { class: 'tariff-dot', 'aria-hidden': 'true' }), h('span', null, text));
  if (ts.missing_holidays) el.append(icon('info', { size: 12, class: 'tariff-gap' }));
}

// ---------- dashboard: countdown and today's schedule ----------

/** "2 h 15 min" until iso; '' when unknown. */
export function durationText(iso, now = Date.now()) {
  const d = new Date(iso);
  if (!iso || isNaN(d)) return '';
  const min = Math.max(0, Math.round((d.getTime() - now) / 60000));
  if (min < 1) return t('tariff.dur.lt1');
  const days = Math.floor(min / 1440);
  const hours = Math.floor((min % 1440) / 60);
  const mins = min % 60;
  if (days) return t('tariff.dur.dh', { d: days, h: hours });
  if (hours) return t('tariff.dur.hm', { h: hours, m: mins });
  return t('tariff.dur.m', { m: mins });
}

/** "Peak in 2 h 15 min" style countdown for the usage card. */
export function countdownText(ts, now = Date.now()) {
  if (!ts.next_change) return '';
  return t(ts.peak ? 'tariff.countdown.until_offpeak' : 'tariff.countdown.until_peak', { d: durationText(ts.next_change, now) });
}

// One UTC interval of TariffState.today_peak in local "HH:MM–HH:MM"; an
// interval cut at the end of the local day ends at "24:00".
function localInterval([from, to], tz) {
  const a = new Date(from);
  const b = new Date(to);
  const end = (d) => {
    const s = hm(d, tz);
    const midnight = tz ? s === '00:00' : d.getHours() === 0 && d.getMinutes() === 0;
    return midnight && d > a ? '24:00' : s;
  };
  return `${hm(a, tz)}–${end(b)}`;
}

/** Today's peak periods in local time, or null when the whole day is off-peak. */
export function todayPeakText(ts) {
  const list = ts.today_peak || [];
  if (!list.length) return null;
  return list.map((iv) => localInterval(iv)).join(t('tariff.list_sep'));
}

/** The same periods on the tariff's clock (hover text: Beijing time). */
export function todayPeakRemote(ts) {
  const list = ts.today_peak || [];
  if (!list.length || !ts.timezone) return '';
  return list.map((iv) => localInterval(iv, ts.timezone)).join(t('tariff.list_sep'));
}

/** The usage card's block for one provider with a tariff (P12.4). */
export function tariffUsageBlock(ts, now = Date.now()) {
  const sched = todayPeakText(ts);
  const remote = todayPeakRemote(ts);
  return h(
    'div',
    { class: 'urow-tariff' },
    h('div', { class: 'urow-tariff-now' }, tariffTag(ts), h('span', { class: 'urow-tariff-cd' }, countdownText(ts, now))),
    h(
      'div',
      { class: 'urow-tariff-sched', title: remote && !sameClock(new Date(now), ts.timezone) ? t('tariff.today.remote', { zone: zoneName(ts.timezone), list: remote }) : '' },
      icon('clock', { size: 12 }),
      sched ? t('tariff.today.peaks', { list: sched }) : t('tariff.today.all_offpeak'),
    ),
    ts.missing_holidays ? h('div', { class: 'urow-tariff-gap' }, icon('info', { size: 12 }), t('tariff.missing_holidays_short')) : null,
  );
}

// ---------- one-line summary (settings provider row) ----------

function daysText(days) {
  const s = [...new Set(days || [])].sort((a, b) => a - b).join(',');
  if (s === '1,2,3,4,5') return t('tariff.days.weekdays');
  if (s === '6,7') return t('tariff.days.weekend');
  if (s === '1,2,3,4,5,6,7') return t('tariff.days.every');
  const names = weekdayNames('short');
  return (days || []).map((d) => names[d - 1]).join(t('tariff.list_sep'));
}

/**
 * "Beijing time · Weekdays 09:00–12:00, 14:00–18:00 · off-peak ×0.5 ·
 * public holidays off-peak". The zone is a part of its own (round 2, T6
 * #12): run into the day names ("Beijing time Weekdays …") a capital stood
 * mid-sentence in en and ru. The line starts with a capital (T8: ru's
 * "пекинское время" opened it in lower case; upperFirst leaves scripts
 * without letter case alone).
 */
export function tariffSummary(tariff) {
  if (!tariff) return '';
  const groups = new Map();
  for (const w of tariff.peak_windows || []) {
    const k = daysText(w.days);
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k).push(`${w.from}–${w.to}`);
  }
  const parts = [];
  if (groups.size) {
    parts.push(zoneName(tariff.timezone));
    parts.push(clauseJoin([...groups].map(([d, ws]) => `${d} ${ws.join(t('tariff.list_sep'))}`)));
    parts.push(t('tariff.summary.multiplier', { m: fmtDecimal(Number(tariff.offpeak_multiplier), 2) }));
    if (tariff.exclude_holidays === 'CN') parts.push(t('tariff.summary.holidays'));
  } else parts.push(t('tariff.summary.no_windows'));
  const n = Object.keys(tariff.models || {}).length;
  if (n) parts.push(t('tariff.summary.models', { n }));
  return upperFirst(parts.join(' · '));
}

// ---------- the tariff editor (provider dialog) ----------

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/;
const toMin = (s) => (s === '24:00' ? 1440 : Number(s.slice(0, 2)) * 60 + Number(s.slice(3, 5)));

let zoneList = null;
function zones() {
  if (!zoneList) {
    try {
      zoneList = Intl.supportedValuesOf('timeZone');
    } catch {
      zoneList = [BEIJING, 'UTC', 'Europe/London', 'America/New_York', 'Asia/Tokyo'];
    }
  }
  return zoneList;
}

/**
 * Editor of provider.tariff and offpeak_only (P12.2, P12.6, D3).
 * opts: {tariff (object|null), offpeakOnly, isNew, deepseek() → bool (a new
 * DeepSeek provider gets the preset from the back end when no tariff is sent)}.
 * Returns {el, validate() → '' | message, value() → {tariff (undefined =
 * unchanged / back-end preset, null = none, object), offpeak_only}, sync()}.
 */
export function tariffEditor(opts) {
  const init = opts.tariff || null;
  let on = !!init;
  const st = {
    timezone: (init && init.timezone) || BEIJING,
    windows: init ? (init.peak_windows || []).map((w) => ({ days: [...(w.days || [])], from: w.from, to: w.to })) : [{ days: [1, 2, 3, 4, 5], from: '09:00', to: '12:00' }],
    holidays: init ? init.exclude_holidays === 'CN' : false,
    mult: init ? String(init.offpeak_multiplier ?? '') : '0.5',
    models: init ? Object.entries(init.models || {}).map(([name, p]) => ({ name, input: String(p.input ?? ''), hit: String(p.input_cache_hit ?? ''), output: String(p.output ?? ''), currency: p.currency || '' })) : [],
    offpeakOnly: !!opts.offpeakOnly,
  };
  const names = weekdayNames('short');

  const toggle = h('input', { type: 'checkbox', checked: on, id: 'trf-on', onchange: () => ((on = toggle.checked), render()) });
  const body = h('div', { class: 'trf-body' });
  const presetNote = h('p', { class: 'dlg-hint trf-preset' });
  const el = h(
    'fieldset',
    { class: 'set-dlg-tariff' },
    h('legend', { class: 'dlg-label' }, t('tariff.editor.legend')),
    h('label', { class: 'trf-on' }, toggle, h('span', null, t('tariff.editor.enable')), h('span', { class: 'dlg-hint' }, t('tariff.editor.enable_hint'))),
    presetNote,
    body,
  );

  function field(label, control, hint, cls = '') {
    return h('label', { class: 'dlg-field ' + cls }, h('span', { class: 'dlg-label' }, label), control, hint ? h('span', { class: 'dlg-hint' }, hint) : null);
  }

  function windowRow(w, i) {
    const days = h(
      'div',
      { class: 'trf-days', role: 'group', 'aria-label': t('tariff.editor.days') },
      names.map((n, k) =>
        h(
          'button',
          {
            type: 'button',
            class: 'trf-day' + (w.days.includes(k + 1) ? ' is-on' : ''),
            'aria-pressed': w.days.includes(k + 1) ? 'true' : 'false',
            dataset: { fkey: `trf:w${i}:d${k + 1}` },
            onclick: () => {
              const d = k + 1;
              w.days = w.days.includes(d) ? w.days.filter((x) => x !== d) : [...w.days, d].sort((a, b) => a - b);
              render();
            },
          },
          n,
        ),
      ),
    );
    const time = (key, label) => h('input', { class: 'input mono trf-time', value: w[key], placeholder: 'HH:MM', maxlength: '5', 'aria-label': label, dataset: { fkey: `trf:w${i}:${key}` }, oninput: (e) => (w[key] = e.target.value.trim()) });
    return h(
      'div',
      { class: 'trf-win' },
      days,
      h('span', { class: 'trf-range' }, time('from', t('tariff.editor.from')), h('span', { class: 'dim' }, '–'), time('to', t('tariff.editor.to'))),
      iconBtn('x', t('tariff.editor.remove_window'), { fkey: `trf:w${i}:rm`, onclick: () => (st.windows.splice(i, 1), render()) }),
    );
  }

  function modelRow(m, i) {
    const num = (key, label) => h('input', { class: 'input input-num', type: 'number', min: '0', step: 'any', value: m[key], 'aria-label': `${m.name || t('tariff.editor.model')} ${label}`, oninput: (e) => (m[key] = e.target.value) });
    return h(
      'div',
      { class: 'trf-model' },
      h('input', { class: 'input mono', value: m.name, placeholder: 'deepseek-flash', 'aria-label': t('tariff.editor.model'), spellcheck: 'false', dataset: { fkey: `trf:m${i}:name` }, oninput: (e) => (m.name = e.target.value) }),
      num('input', t('tariff.editor.price_input')),
      num('hit', t('tariff.editor.price_hit')),
      num('output', t('tariff.editor.price_output')),
      h('input', { class: 'input set-cur', value: m.currency, placeholder: 'CNY', maxlength: '8', list: 'set-currencies', 'aria-label': t('tariff.editor.currency'), oninput: (e) => (m.currency = e.target.value) }),
      iconBtn('x', t('tariff.editor.remove_model'), { fkey: `trf:m${i}:rm`, onclick: () => (st.models.splice(i, 1), render()) }),
    );
  }

  function render() {
    clear(body);
    const preset = !on && opts.isNew && opts.deepseek && opts.deepseek();
    presetNote.textContent = preset ? t('tariff.editor.preset') : '';
    presetNote.hidden = !preset;
    if (!on && !preset) {
      body.hidden = true;
      return;
    }
    body.hidden = false;
    const offSwitch = h(
      'label',
      { class: 'trf-offpeak' },
      h('input', { type: 'checkbox', checked: st.offpeakOnly, onchange: (e) => (st.offpeakOnly = e.target.checked) }),
      h('span', null, h('span', { class: 'trf-offpeak-t' }, t('tariff.editor.offpeak_only')), h('span', { class: 'dlg-hint' }, t('tariff.editor.offpeak_only_hint'))),
    );
    if (!on) {
      // a new DeepSeek provider: the preset is filled in by the back end
      body.append(offSwitch);
      return;
    }
    const tz = h('input', { class: 'input mono', value: st.timezone, list: 'trf-zones', spellcheck: 'false', autocomplete: 'off', 'aria-label': t('tariff.editor.timezone'), oninput: (e) => (st.timezone = e.target.value.trim()) });
    const mult = h('input', { class: 'input input-num', type: 'number', min: '0', max: '1', step: '0.05', value: st.mult, oninput: (e) => (st.mult = e.target.value) });
    body.append(
      h('div', { class: 'set-dlg-grid' }, field(t('tariff.editor.timezone'), tz, t('tariff.editor.timezone_hint'), 'span-2'), h('datalist', { id: 'trf-zones' }, zones().map((z) => h('option', { value: z })))),
      h(
        'div',
        { class: 'dlg-field' },
        h('span', { class: 'dlg-label' }, t('tariff.editor.windows')),
        st.windows.length ? h('div', { class: 'trf-wins' }, st.windows.map(windowRow)) : h('p', { class: 'dlg-hint' }, t('tariff.editor.no_windows')),
        h('div', null, btn(t('tariff.editor.add_window'), { size: 'sm', kind: 'ghost', icon: 'plus', fkey: 'trf:add-window', onclick: () => (st.windows.push({ days: [1, 2, 3, 4, 5], from: '09:00', to: '12:00' }), render()) })),
        h('span', { class: 'dlg-hint' }, t('tariff.editor.windows_hint')),
      ),
      h('div', { class: 'set-dlg-grid' }, field(t('tariff.editor.multiplier'), mult, t('tariff.editor.multiplier_hint')), h('label', { class: 'dlg-field trf-hol' }, h('span', { class: 'dlg-label' }, t('tariff.editor.holidays')), h('span', { class: 'trf-check' }, h('input', { type: 'checkbox', checked: st.holidays, onchange: (e) => (st.holidays = e.target.checked) }), h('span', null, t('tariff.editor.holidays_cn'))))),
      h(
        'div',
        { class: 'dlg-field' },
        h('span', { class: 'dlg-label' }, t('tariff.editor.models')),
        st.models.length
          ? h(
              'div',
              { class: 'trf-models' },
              h('div', { class: 'trf-model trf-model-head', 'aria-hidden': 'true' }, [t('tariff.editor.model'), t('tariff.editor.price_input'), t('tariff.editor.price_hit'), t('tariff.editor.price_output'), t('tariff.editor.currency'), ''].map((x) => h('span', null, x))),
              st.models.map(modelRow),
            )
          : null,
        h('div', null, btn(t('tariff.editor.add_model'), { size: 'sm', kind: 'ghost', icon: 'plus', fkey: 'trf:add-model', onclick: () => (st.models.push({ name: '', input: '', hit: '', output: '', currency: '' }), render()) })),
        h('span', { class: 'dlg-hint' }, t('tariff.editor.models_hint')),
      ),
      // only a tariff with peak hours can hold requests back (settings.offpeak_needs_peak)
      st.windows.length ? offSwitch : null,
    );
  }

  function validate() {
    if (!on) return '';
    if (!st.timezone) return t('tariff.editor.err.timezone');
    if (zoneList && !zones().includes(st.timezone) && st.timezone !== 'UTC') {
      try {
        new Intl.DateTimeFormat('en', { timeZone: st.timezone });
      } catch {
        return t('tariff.editor.err.timezone');
      }
    }
    for (const [i, w] of st.windows.entries()) {
      if (!w.days.length) return t('tariff.editor.err.days', { n: i + 1 });
      if (!HHMM.test(w.from) || !(HHMM.test(w.to) || w.to === '24:00')) return t('tariff.editor.err.time', { n: i + 1 });
      if (toMin(w.from) >= toMin(w.to)) return t('tariff.editor.err.order', { n: i + 1 });
    }
    const m = Number(st.mult);
    if (st.windows.length && (st.mult === '' || isNaN(m) || m <= 0 || m > 1)) return t('tariff.editor.err.multiplier');
    const seen = new Set();
    for (const x of st.models) {
      const name = x.name.trim();
      if (!name) return t('tariff.editor.err.model_name');
      if (seen.has(name)) return t('tariff.editor.err.model_dup', { model: name });
      seen.add(name);
      for (const k of ['input', 'hit', 'output']) {
        const v = Number(x[k]);
        if (x[k] === '' || isNaN(v) || v < 0) return t('tariff.editor.err.price', { model: name });
      }
    }
    return '';
  }

  function value() {
    const preset = !on && opts.isNew && opts.deepseek && opts.deepseek();
    // switched off: clear a saved tariff; never had one: leave it out
    // (a new DeepSeek provider then gets the back end's preset)
    if (!on) return { tariff: init ? null : undefined, offpeak_only: preset ? st.offpeakOnly : false };
    const models = {};
    for (const x of st.models) models[x.name.trim()] = { input: Number(x.input), input_cache_hit: Number(x.hit), output: Number(x.output), currency: x.currency.trim() };
    const tariff = { timezone: st.timezone, peak_windows: st.windows.map((w) => ({ days: [...w.days], from: w.from, to: w.to })), offpeak_multiplier: st.windows.length ? Number(st.mult) : Number(st.mult) || 0 };
    if (st.holidays) tariff.exclude_holidays = 'CN';
    if (Object.keys(models).length) tariff.models = models;
    return { tariff, offpeak_only: st.windows.length ? st.offpeakOnly : false };
  }

  render();
  return { el, validate, value, sync: render };
}

// ---------- public holidays (P12.2: view and edit) ----------

/** Contiguous dates as ranges: [["2026-01-01","2026-01-03"], …]. */
export function dateRanges(dates) {
  const out = [];
  for (const s of [...dates].sort()) {
    const d = parseYMD(s);
    const last = out[out.length - 1];
    if (last && d && ymd(addDays(parseYMD(last[1]), 1)) === s) last[1] = s;
    else out.push([s, s]);
  }
  return out;
}

export function rangeText([a, b]) {
  const da = parseYMD(a);
  const db = parseYMD(b);
  if (!da || !db) return a;
  return a === b ? fmtMonthDay(da) : t('calendar.range', { from: fmtMonthDay(da), to: fmtMonthDay(db) });
}

/** "2026-01-01..2026-01-03" lines for the editor. */
function linesOf(years) {
  const out = [];
  for (const y of Object.keys(years || {}).sort()) {
    out.push(`# ${y}`);
    for (const [a, b] of dateRanges(years[y] || [])) out.push(a === b ? a : `${a}..${b}`);
    out.push('');
  }
  return out.join('\n').trim();
}

/** Parses the editor text back into {year: [dates]}; throws a message. */
function parseLines(text) {
  const years = {};
  const add = (d) => {
    const y = d.slice(0, 4);
    (years[y] = years[y] || []).push(d);
  };
  for (const [i, raw] of text.split('\n').entries()) {
    const line = raw.replace(/#.*$/, '').trim();
    if (!line) continue;
    const m = /^(\d{4}-\d{2}-\d{2})(?:\s*(?:\.\.|–|—|~)\s*(\d{4}-\d{2}-\d{2}))?$/.exec(line);
    if (!m || !parseYMD(m[1]) || (m[2] && !parseYMD(m[2]))) throw t('settings.holidays.err_line', { n: i + 1, line });
    if (!m[2]) {
      add(m[1]);
      continue;
    }
    if (m[2] < m[1] || m[1].slice(0, 4) !== m[2].slice(0, 4)) throw t('settings.holidays.err_range', { n: i + 1, line });
    for (let d = parseYMD(m[1]); ymd(d) <= m[2]; d = addDays(d, 1)) add(ymd(d));
  }
  for (const y of Object.keys(years)) years[y] = [...new Set(years[y])].sort();
  return years;
}

/**
 * The holiday editor dialog. save(years) must PUT /api/settings/holidays
 * (quiet) and resolve to the new HolidaysView. Resolves to it or null.
 */
export function openHolidaysEditor(view, save) {
  return openDialog(
    t('settings.holidays.edit_title'),
    (close, busy) => {
      const ta = h('textarea', { class: 'input mono hol-text', rows: '14', spellcheck: 'false', autofocus: true, 'aria-label': t('settings.holidays.edit_label') });
      ta.value = linesOf(view.years);
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const ok = h('button', { type: 'submit', class: 'btn btn-primary' }, t('settings.holidays.save'));
      return h(
        'form',
        {
          novalidate: true,
          onsubmit: async (e) => {
            e.preventDefault();
            let years;
            try {
              years = parseLines(ta.value);
            } catch (msg) {
              err.textContent = String(msg);
              return;
            }
            ok.disabled = true;
            try {
              close(await busy(save(years)));
            } catch (ex) {
              ok.disabled = false;
              err.textContent = ex.message;
            }
          },
        },
        h('p', { class: 'dlg-msg' }, t('settings.holidays.edit_intro')),
        ta,
        h('p', { class: 'dlg-hint' }, t('settings.holidays.edit_hint')),
        err,
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
      );
    },
    { wide: true, className: 'dlg-hol' },
  );
}
