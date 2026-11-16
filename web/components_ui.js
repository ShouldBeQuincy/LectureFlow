// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Component manager widgets (SPEC-v3 V3, V4.2 steps 3 and 5, V11): the
// names of components, the pick list with sizes, the plan summary, the
// download-source picker and the progress of an install task (overall and
// per item, speed, time left, failure with "retry" / "change source").
// Shared by the onboarding and the settings page's Components block; the
// widgets only render what the server returned (ComponentStatus, Plan,
// TaskStatus, Sources of .claude/v3-brief.md §7.2) and report the user's
// choices through callbacks — the caller makes the requests.

import { h, btn, openDialog } from './dom.js';
import { icon } from './icons.js';
import { t, has } from './i18n.js';
import * as U from './util.js';

const BACKEND_KEY = { 'faster-whisper': 'cuda', openvino: 'openvino', mlx: 'mlx' };

/** Splits a model id "model:{backend}:{name}:{precision}". */
export function parseModelID(id) {
  const p = String(id || '').split(':');
  if (p[0] !== 'model' || p.length < 4) return null;
  return { backend: p[1], model: p[2], precision: p.slice(3).join(':') };
}

function asComponent(c) {
  if (typeof c !== 'string') return c || {};
  const m = parseModelID(c);
  return m ? { id: c, kind: 'model', ...m } : { id: c };
}

/** The name of a component (ComponentStatus or id) in the UI language. */
export function componentName(c) {
  c = asComponent(c);
  if (c.name_key && has(c.name_key)) return t(c.name_key);
  if (c.kind === 'model' || parseModelID(c.id)) {
    const m = c.model ? c : parseModelID(c.id) || {};
    return t('compui.name.model', { model: m.model || '', precision: m.precision || '' });
  }
  const key = `compui.name.${String(c.id || '').replace(/-/g, '_')}`;
  return has(key) ? t(key) : String(c.id || '');
}

/** One line about what a component is for. */
export function componentHint(c) {
  c = asComponent(c);
  if (c.desc_key && has(c.desc_key)) return t(c.desc_key);
  const m = c.kind === 'model' || parseModelID(c.id) ? (c.backend ? c : parseModelID(c.id) || {}) : null;
  if (m) {
    const key = `compui.hint.model_${BACKEND_KEY[m.backend] || ''}`;
    return has(key) ? t(key) : '';
  }
  const key = `compui.hint.${String(c.id || '').replace(/-/g, '_')}`;
  return has(key) ? t(key) : '';
}

const isModel = (c) => c.kind === 'model' || !!parseModelID(c.id);

// ---- pick list ----

/**
 * The component pick list. opts:
 *   components   ComponentStatus[] (GET /api/components)
 *   selected     Set of ids the user picked
 *   implied      Set of ids the plan adds as requirements (shown checked, locked)
 *   recommended  Set of ids of the hardware recommendation (a quiet note)
 *   onToggle(id, on)
 * The bundled core is not listed.
 */
export function componentPicker({ components, selected, implied = new Set(), recommended = new Set(), onToggle }) {
  const list = (components || []).filter((c) => c.id !== 'core' && c.kind !== 'binary');
  const groups = [
    { key: 'runtime', items: list.filter((c) => !isModel(c)) },
    { key: 'models', items: list.filter(isModel) },
  ].filter((g) => g.items.length);
  return h(
    'div',
    { class: 'cu-picker' },
    groups.map((g) =>
      h(
        'fieldset',
        { class: 'cu-group' },
        h('legend', { class: 'cu-group-title' }, t(`compui.group.${g.key}`)),
        g.items.map((c) => pickRow(c, { selected, implied, recommended, onToggle })),
      ),
    ),
  );
}

function pickRow(c, { selected, implied, recommended, onToggle }) {
  const installed = c.state === 'installed';
  const unavailable = c.state === 'unavailable';
  const busy = c.state === 'installing';
  const isImplied = !selected.has(c.id) && implied.has(c.id);
  const checked = installed || busy || selected.has(c.id) || isImplied;
  const locked = installed || busy || unavailable || isImplied;
  const id = 'cu-' + c.id.replace(/[^a-z0-9]+/gi, '-');
  let note = '';
  if (installed) note = t('compui.state.installed');
  else if (busy) note = t('compui.state.installing');
  else if (unavailable) note = unavailableText(c);
  else if (c.state === 'broken') note = t('compui.state.broken');
  else if (isImplied) note = t('compui.implied');
  else if (recommended.has(c.id)) note = t('compui.recommended');
  return h(
    'label',
    { class: 'cu-row' + (locked ? ' is-locked' : '') + (unavailable ? ' is-unavailable' : ''), for: id },
    h('input', {
      type: 'checkbox',
      id,
      checked,
      disabled: locked,
      dataset: { fkey: 'cu:pick:' + c.id },
      onchange: (e) => onToggle && onToggle(c.id, e.target.checked),
    }),
    h('span', { class: 'cu-row-main' }, h('span', { class: 'cu-row-name' }, componentName(c)), h('span', { class: 'cu-row-hint' }, [componentHint(c), note].filter(Boolean).join(' · '))),
    h('span', { class: 'cu-row-size' }, installed || unavailable ? '' : U.fmtBytes(c.download_bytes || 0)),
  );
}

function unavailableText(c) {
  const key = `compui.unavailable.${c.unavailable_reason || ''}`;
  return c.unavailable_reason && has(key) ? t(key) : t('compui.state.unavailable');
}

/** The list of components a plan installs, with their sizes (read-only). */
export function componentList(ids, components) {
  const byID = new Map((components || []).map((c) => [c.id, c]));
  return h(
    'ul',
    { class: 'cu-list' },
    (ids || [])
      .filter((id) => id !== 'core')
      .map((id) => {
        const c = byID.get(id) || asComponent(id);
        const installed = c.state === 'installed';
        return h(
          'li',
          { class: 'cu-list-row' },
          icon(isModel(c) ? 'layers' : 'cpu', { size: 15 }),
          h('span', { class: 'cu-row-main' }, h('span', { class: 'cu-row-name' }, componentName(c)), componentHint(c) ? h('span', { class: 'cu-row-hint' }, componentHint(c)) : null),
          h('span', { class: 'cu-row-size' }, installed ? t('compui.state.installed') : c.download_bytes ? U.fmtBytes(c.download_bytes) : ''),
        );
      }),
  );
}

/** The size line of a plan (POST /api/components/plan). */
export function planSummary(plan) {
  if (!plan) return '';
  if (!plan.download_bytes) return t('compui.plan.nothing');
  return t('compui.plan.summary', { download: U.fmtBytes(plan.download_bytes), install: U.fmtBytes(plan.install_bytes || 0) });
}

/** Not enough disk space for a plan: what is needed and what is free; '' when it fits. */
export function planSpaceText(plan) {
  if (!plan || plan.enough_space !== false) return '';
  return t('compui.plan.no_space', { need: U.fmtBytes(plan.need_bytes || 0), free: U.fmtBytes(plan.free_bytes || 0) });
}

// ---- download sources ----

const GROUPS = ['pypi', 'models'];
const CUSTOM = '__custom__';

function sourceValue(s) {
  if (!s || s.mode !== 'mirror') return 'default';
  if (s.custom_url && !s.mirror_id) return CUSTOM;
  return 'mirror:' + (s.mirror_id || '');
}

/** True when one of the groups downloads from a mirror. */
export function usesMirror(sources) {
  return !!sources && GROUPS.some((g) => sources[g] && sources[g].mode === 'mirror');
}

/** The first preset mirror of each group (the onboarding's suggestion). */
export function mirrorSources(sources) {
  const out = {};
  for (const g of GROUPS) {
    const first = ((sources.options || {})[g] || [])[0];
    out[g] = first ? { mode: 'mirror', mirror_id: first.id, custom_url: '' } : { mode: 'default', mirror_id: '', custom_url: '' };
  }
  return out;
}

/** Both groups on the default source. */
export function defaultSources() {
  return { pypi: { mode: 'default', mirror_id: '', custom_url: '' }, models: { mode: 'default', mirror_id: '', custom_url: '' } };
}

/**
 * The download-source picker: one select per group (default, the preset
 * mirrors, and with allowCustom a custom address). Returns {el, value()}
 * where value() is {pypi, models} for PUT /api/components/sources, or null
 * while a custom address is not a http(s) URL.
 */
export function sourcePicker(sources, { allowCustom = false, onChange } = {}) {
  const rows = {};
  const el = h('div', { class: 'cu-sources' });
  for (const g of GROUPS) {
    const cur = (sources && sources[g]) || { mode: 'default' };
    const opts = ((sources && sources.options) || {})[g] || [];
    const sel = h(
      'select',
      { class: 'select', id: `cu-src-${g}`, dataset: { fkey: `cu:src:${g}` }, value: sourceValue(cur) },
      h('option', { value: 'default' }, t('compui.source.default')),
      opts.map((o) => h('option', { value: 'mirror:' + o.id }, o.label)),
      allowCustom ? h('option', { value: CUSTOM }, t('compui.source.custom')) : null,
    );
    const url = h('input', { type: 'url', class: 'input cu-src-url', value: cur.custom_url || '', placeholder: t('compui.source.custom_placeholder'), 'aria-label': t('compui.source.custom'), hidden: sel.value !== CUSTOM });
    sel.addEventListener('change', () => {
      url.hidden = sel.value !== CUSTOM;
      if (onChange) onChange();
    });
    url.addEventListener('input', () => onChange && onChange());
    rows[g] = { sel, url };
    el.append(h('label', { class: 'cu-src-row', for: `cu-src-${g}` }, h('span', { class: 'cu-src-label' }, t(`compui.source.label_${g}`)), h('span', { class: 'cu-src-ctl' }, sel, url)));
  }
  function value() {
    const out = {};
    for (const g of GROUPS) {
      const v = rows[g].sel.value;
      if (v === 'default') out[g] = { mode: 'default', mirror_id: '', custom_url: '' };
      else if (v === CUSTOM) {
        const u = rows[g].url.value.trim();
        if (!/^https?:\/\/\S+$/i.test(u)) return null;
        out[g] = { mode: 'mirror', mirror_id: '', custom_url: u };
      } else out[g] = { mode: 'mirror', mirror_id: v.slice('mirror:'.length), custom_url: '' };
    }
    return out;
  }
  return { el, value };
}

/** A short description of the sources in use ("default" / the mirror names). */
export function sourcesText(sources) {
  if (!sources) return '';
  const name = (g) => {
    const s = sources[g] || {};
    if (s.mode !== 'mirror') return t('compui.source.default');
    if (s.custom_url && !s.mirror_id) return s.custom_url;
    const o = (((sources.options || {})[g]) || []).find((x) => x.id === s.mirror_id);
    return o ? o.label : s.mirror_id;
  };
  return GROUPS.map((g) => t('compui.source.line', { group: t(`compui.source.label_${g}`), source: name(g) })).join(' · ');
}

/**
 * Asks for other download sources ("换一个下载源"): resolves to the new
 * {pypi, models}, or null when cancelled. The dialog preselects the other
 * kind of source than the one in use (mirror ↔ default).
 */
export function chooseSourcesDialog(sources, { okText = t('compui.task.retry_with_source') } = {}) {
  const proposal = usesMirror(sources) ? defaultSources() : mirrorSources(sources);
  return openDialog(t('compui.source.dialog_title'), (close) => {
    const picker = sourcePicker({ ...sources, ...proposal }, { onChange: sync });
    const ok = h('button', { type: 'button', class: 'btn btn-primary', onclick: () => picker.value() && close(picker.value()) }, okText);
    function sync() {
      ok.disabled = !picker.value();
    }
    return h(
      'div',
      null,
      h('p', { class: 'dlg-msg' }, t('compui.source.dialog_msg')),
      picker.el,
      h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
    );
  });
}

// ---- task progress ----

const ACTIVE = new Set(['queued', 'running', 'verifying', 'installing', 'paused']);

/** True while a task still has work to do (paused counts). */
export function taskActive(task) {
  return !!task && ACTIVE.has(task.state);
}

/** The text of a coded component message (an error or a note). */
function codeText(info) {
  if (!info) return '';
  const params = { ...(info.params || {}) };
  for (const [k, v] of Object.entries(params)) {
    if (typeof v === 'number' && k.endsWith('_mb')) params[k] = v < 0 ? t('compui.unknown_size') : U.fmtMB(v);
    if (typeof v === 'number' && k.endsWith('_bytes')) params[k] = U.fmtBytes(v);
  }
  if (info.code && has(info.code)) return t(info.code, params);
  if (info.code && has('errors.' + info.code)) return t('errors.' + info.code, params);
  return info.detail || '';
}

const ERR_KEYS = {
  'components.network': 'network',
  'components.disk_full': 'disk_full',
  'components.checksum': 'checksum',
  'components.http_status': 'http',
};

// Failures a retry from the same download source repeats: only another
// source helps, and the catalog text says so (components.redirect_refused:
// the source sent the request to an address LectureFlow does not follow).
const SOURCE_ONLY = new Set(['components.redirect_refused']);

/**
 * What happened and what can be done about a failed task: {what, todo,
 * sourceOnly}. The four failures of V3.3 have their own texts; another
 * code uses its catalog text (or detail) with a general suggestion.
 * sourceOnly (SOURCE_ONLY): no "you can retry" suggestion (todo is empty:
 * the text itself says to change the source), and the way out offered is
 * changing the download source, not a retry.
 */
export function taskErrorText(err, kind = '') {
  // An import has no download source to change: its own wording.
  const g = kind === 'import' ? 'import_' : '';
  if (!err) return { what: t(`compui.err.${g}generic`), todo: t(`compui.err.${g || 'generic_'}todo`) };
  if (SOURCE_ONLY.has(err.code) && !g) {
    const text = codeText(err);
    return { what: text ? t('compui.err.with_reason', { reason: text }) : t('compui.err.generic'), todo: '', sourceOnly: true };
  }
  const k = ERR_KEYS[err.code];
  if (k) {
    const p = err.params || {};
    const params = {
      // -1: not known (internal/components withDiskFree, a disk that
      // filled up while writing), as for free_mb: never "-1 MB"
      required: typeof p.required_mb === 'number' && p.required_mb >= 0 ? U.fmtMB(p.required_mb) : t('compui.unknown_size'),
      free: typeof p.free_mb === 'number' && p.free_mb >= 0 ? U.fmtMB(p.free_mb) : t('compui.unknown_size'),
      status: p.status != null ? p.status : '',
    };
    return { what: t(`compui.err.${k}`, params), todo: t(`compui.err.${k}_todo`) };
  }
  const text = codeText(err);
  return { what: text ? t(`compui.err.${g}with_reason`, { reason: text }) : t(`compui.err.${g}generic`), todo: t(`compui.err.${g || 'generic_'}todo`) };
}

/** The text of a task note (e.g. the mirror fell back to the default source). */
export function taskNoteText(note) {
  if (!note) return '';
  if (note.code === 'components.mirror_fallback') {
    const g = (note.params && note.params.group) || '';
    const group = has(`compui.source.group_${g}`) ? t(`compui.source.group_${g}`) : g;
    return t('compui.note.mirror_fallback', { group });
  }
  return codeText(note);
}

function bar(fraction, label, cls = '') {
  const pctNum = Math.max(0, Math.min(100, Math.round((fraction || 0) * 100)));
  return h(
    'div',
    { class: 'cu-bar ' + cls, role: 'progressbar', 'aria-label': label, 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(pctNum) },
    h('div', { class: 'cu-bar-fill', style: { width: `${pctNum}%` } }),
  );
}

function stateText(task) {
  // An import that failed was not a download.
  if (task.kind === 'import' && task.state === 'failed') return t('compui.task.state.import_failed');
  return t(`compui.task.state.${task.state}`);
}

/**
 * keepChildren(parent, kids): makes kids (nodes or strings, in this order)
 * the children of parent. A node among them already there stays where it
 * is, never taken out and put back (R81 L8: a task view's status line and
 * alert); the others are taken out, the new ones put in around it. (Two
 * kept nodes that swap places: one of them is moved; no caller does so.)
 */
export function keepChildren(parent, kids) {
  const want = kids.filter((k) => k != null && k !== false && k !== true).map((k) => (k instanceof Node ? k : document.createTextNode(String(k))));
  const set = new Set(want);
  for (const c of [...parent.childNodes]) if (!set.has(c)) parent.removeChild(c);
  want.forEach((k, i) => {
    const at = parent.childNodes[i] || null;
    if (at !== k) parent.insertBefore(k, at);
  });
}

/**
 * The progress of an install task. opts:
 *   components()   ComponentStatus[] for the item names (may be empty)
 *   onAction(name) 'retry' | 'change_source' | 'pause' | 'resume' | 'cancel'
 *   busy()         true while an action is in flight (buttons disabled)
 *   actions        which buttons to offer (default all)
 * Returns {el, update(task)}.
 *
 * T23 general 18: the view is no live region (its figures change every
 * second, a screen reader would read them all the time). One status line,
 * kept across updates, says the task's state when it changes (started,
 * paused, done); a failure is told by its own alert, not twice. The
 * progress bars keep their values for whoever looks.
 * R81 L8: the alert is put in once, when the failure is first shown;
 * updates while that failure lasts leave it where it is (its buttons
 * follow busy() in place). The caller puts el in its place once and
 * keeps it there (keepChildren): taken out and put back, the status line
 * and the alert may be read again, or a change of the line not at all.
 */
export function taskView({ components = () => [], onAction, busy = () => false, actions = ['retry', 'change_source', 'pause', 'resume', 'cancel'] } = {}) {
  const live = h('p', { class: 'visually-hidden', role: 'status' });
  const el = h('div', { class: 'cu-task' }, live);
  let said = null;
  let alertEl = null; // the failure's alert, and what it says
  let alertKey = '';
  const can = (a) => actions.includes(a);
  function say(task) {
    const key = task ? `${task.kind}:${task.state}` : '';
    if (key === said) return;
    said = key;
    live.textContent = task && task.state !== 'failed' ? stateText(task) : '';
  }
  function update(task) {
    const failure = task && task.state === 'failed' ? taskErrorText(task.error, task.kind) : null;
    const key = failure ? JSON.stringify([task.id, task.kind, failure.what, failure.todo]) : '';
    const keep = key && key === alertKey && alertEl && alertEl.parentNode === el ? alertEl : null;
    for (const c of [...el.children]) if (c !== live && c !== keep) c.remove();
    if (!keep) {
      alertEl = null;
      alertKey = '';
    }
    say(task);
    if (!task) return;
    // what comes before the alert goes in before the one kept
    const put = (...ns) => {
      for (const n of ns) {
        if (keep) el.insertBefore(n, keep);
        else el.append(n);
      }
    };
    el.dataset.state = task.state;
    const total = task.total_bytes || 0;
    const done = Math.min(task.done_bytes || 0, total || Infinity);
    const frac = task.state === 'done' ? 1 : total ? done / total : 0;
    // An import copies the component file: its progress is a copy, not a download.
    const progress = (d, n) => (task.kind === 'import' ? t('onboarding.import.progress', { copied: U.fmtBytes(d), total: U.fmtBytes(n) }) : t('compui.task.progress', { done: U.fmtBytes(d), total: U.fmtBytes(n) }));
    const facts = [];
    if (total) facts.push(progress(done, total));
    if (task.state === 'running' && task.speed_bps > 0) facts.push(t('compui.task.speed', { speed: U.fmtBytes(task.speed_bps) }));
    if (task.state === 'running' && task.eta_s > 0) facts.push(t('compui.task.eta', { time: U.fmtDuration(task.eta_s * 1000) }));
    const ic = task.state === 'done' ? 'circle-check' : task.state === 'failed' ? 'circle-alert' : task.state === 'paused' || task.state === 'cancelled' ? 'circle-pause' : 'download';
    put(
      h(
        'div',
        { class: 'cu-task-head' },
        h('span', { class: 'cu-task-state' }, icon(ic, { size: 16 }), h('span', null, stateText(task))),
        h('span', { class: 'cu-task-facts' }, facts.join(' · ')),
      ),
      bar(frac, t('compui.task.overall_label'), 'cu-bar-main' + (task.state === 'failed' ? ' is-failed' : task.state === 'done' ? ' is-done' : '')),
    );
    for (const n of task.notes || []) {
      const s = taskNoteText(n);
      if (s) put(h('p', { class: 'cu-note' }, icon('info', { size: 14 }), h('span', null, s)));
    }
    if (failure && keep) {
      for (const b of keep.querySelectorAll('button')) b.disabled = busy();
    } else if (failure) {
      alertKey = key;
      // a failure only another source helps with: "change the download
      // source" is the way out, without a retry (unless no source can be
      // chosen here)
      const source = can('change_source') && task.kind !== 'import';
      const retry = can('retry') && !(failure.sourceOnly && source);
      alertEl = h(
        'div',
        { class: 'cu-error', role: 'alert' },
        h('p', { class: 'cu-error-what' }, failure.what),
        failure.todo ? h('p', { class: 'cu-error-todo' }, failure.todo) : null,
        h(
          'div',
          { class: 'cu-actions' },
          retry ? btn(t('compui.task.retry'), { kind: 'primary', size: 'sm', icon: 'rotate-cw', disabled: busy(), fkey: 'cu:task:retry', onclick: () => onAction && onAction('retry') }) : null,
          source ? btn(t('compui.task.change_source'), { kind: failure.sourceOnly ? 'primary' : undefined, size: 'sm', icon: 'server', disabled: busy(), fkey: 'cu:task:source', onclick: () => onAction && onAction('change_source') }) : null,
        ),
      );
      el.append(alertEl);
    } else if (taskActive(task)) {
      const tools = [];
      if (task.state === 'running' && can('pause')) tools.push(btn(t('compui.task.pause'), { size: 'sm', kind: 'ghost', icon: 'pause', disabled: busy(), fkey: 'cu:task:pause', onclick: () => onAction && onAction('pause') }));
      if (task.state === 'paused' && can('resume')) tools.push(btn(t('compui.task.resume'), { size: 'sm', icon: 'play', disabled: busy(), fkey: 'cu:task:resume', onclick: () => onAction && onAction('resume') }));
      if (can('cancel')) tools.push(btn(t('compui.task.cancel'), { size: 'sm', kind: 'ghost', icon: 'ban', disabled: busy(), fkey: 'cu:task:cancel', onclick: () => onAction && onAction('cancel') }));
      if (tools.length) el.append(h('div', { class: 'cu-actions cu-actions-quiet' }, tools));
    } else if (task.state === 'cancelled' && can('retry')) {
      el.append(h('div', { class: 'cu-actions' }, btn(t('compui.task.restart'), { size: 'sm', icon: 'rotate-cw', disabled: busy(), fkey: 'cu:task:retry', onclick: () => onAction && onAction('retry') })));
    }
    const byID = new Map((components() || []).map((c) => [c.id, c]));
    const items = task.items || [];
    if (items.length) {
      el.append(
        h(
          'ul',
          { class: 'cu-items', 'aria-label': t('compui.task.items_label') },
          items.map((it) => {
            const c = byID.get(it.id) || it.id;
            const f = it.total_bytes ? Math.min(1, (it.done_bytes || 0) / it.total_bytes) : ['installed', 'verifying', 'installing', 'downloaded'].includes(it.state) ? 1 : 0;
            const st = has(`compui.item.state.${it.state}`) ? t(`compui.item.state.${it.state}`) : it.state;
            const size = !it.total_bytes ? '' : it.done_bytes > 0 && it.done_bytes < it.total_bytes ? progress(it.done_bytes, it.total_bytes) : U.fmtBytes(it.total_bytes);
            return h(
              'li',
              { class: 'cu-item', dataset: { state: it.state || '' } },
              h('span', { class: 'cu-item-name' }, componentName(c)),
              h('span', { class: 'cu-item-meta' }, [st, size, it.source === 'mirror' ? t('compui.source.via_mirror') : ''].filter(Boolean).join(' · ')),
              bar(f, componentName(c), 'cu-bar-item' + (it.state === 'failed' ? ' is-failed' : '')),
            );
          }),
        ),
      );
    }
  }
  return { el, update };
}
