// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The settings page's "Components" section (SPEC-v3 V11, V3, V8): the
// installed components with their version and disk use; update (when the
// manifest has a newer version), repair and uninstall (explained when other
// components need it or a task works on it); installing more components
// with the size of the plan; the download sources (PyPI and models each:
// default, a preset mirror or a custom address; a mirror that does not
// answer falls back to the default source by itself); importing a component
// archive from a file; and the install / update / import tasks with their
// overall and per-item progress and pause / resume / cancel / retry.
//
// The widgets come from components_ui.js (shared with the onboarding). The
// section shows only what the server returned (.claude/v3-brief.md §7.2):
// every action re-reads the list afterwards. Task progress follows the
// SSE "components" event and is polled once a second while the stream does
// not deliver it; only the task cards update in place, the section is
// redrawn when a task changes state.

import { h, btn, iconBtn, skeleton, errorBlock, toast, confirmDialog, filePicker } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { api, onComponentsEvent, componentsEventFresh, DESKTOP } from './api.js';
import { componentName, componentHint, componentPicker, planSummary, planSpaceText, sourcePicker, sourcesText, chooseSourcesDialog, taskView, taskActive, keepChildren } from './components_ui.js';

const POLL_MS = 1000;
const PLAN_DELAY = 250;
const MAX_FAILED_SHOWN = 3;

const isInstalled = (c) => c.state === 'installed' || c.state === 'broken' || !!c.installed_version;
const isBundled = (c) => c.bundled || c.kind === 'binary' || c.id === 'core';
const sizeOf = (c) => c.disk_bytes || c.install_bytes || 0;

// The desktop shell's component-archive picker (preload whitelist), or null.
function nativeImport() {
  const lf = typeof window !== 'undefined' ? window.lectureflow : null;
  return lf && lf.components && typeof lf.components.importFile === 'function' ? lf.components : null;
}

// createComponentsSection({head, formRow, rerender}): the helpers of
// view_settings.js; rerender() redraws the block.
export function createComponentsSection(ui) {
  let alive = true;
  let list = null; // GET /api/components
  let error = '';
  let sources = null; // GET /api/components/sources
  let picker = null; // sourcePicker of the stored sources
  let pickerKey = '';
  let savingSources = false;
  const selected = new Set();
  let plan = null; // POST /api/components/plan of the selection
  let planning = false;
  let planTimer = null;
  let installing = false;
  const rowBusy = new Set(); // component ids with an update / uninstall in flight
  const rowErrors = new Map(); // component id → message of the last failed action
  const views = new Map(); // task id → taskView
  // Round 9 WV-10 (R87 S-R1, R81 L8): what holds a task view is kept and
  // put in place with keepChildren, never taken out and put back on a
  // redraw (its status line and its alert stay where they are): the
  // section's head and body, the tasks block and each task's card
  // (task id → {el, hd, title, hide}). view_settings.js renderBlock leaves
  // the section's shell to render (aligns).
  const cards = new Map();
  let headEl = null;
  let introEl = null;
  let bodyEl = null;
  let tasksEl = null;
  let tasksTitle = null;
  const actingTasks = new Set(); // task ids with an action in flight
  const seenActive = new Set(); // tasks seen running while the page was open
  const hidden = new Set(); // finished tasks the user put away
  let upload = null; // {name, fraction, abort} while a web import uploads
  let pollTimer = null;
  let reloadTimer = null;

  const redraw = () => alive && ui.rerender();
  const comps = () => (list && list.components) || [];
  const tasks = () => (list && list.tasks) || [];
  const byID = (id) => comps().find((c) => c.id === id);

  // ---- data ----

  async function load() {
    try {
      const [l, s] = await Promise.all([api.components({ quiet: true }), sources ? Promise.resolve(sources) : api.componentSources({ quiet: true }).catch(() => null)]);
      if (!alive) return;
      list = l || { components: [], tasks: [] };
      if (s) setSources(s);
      error = '';
      for (const tk of tasks()) if (taskActive(tk)) seenActive.add(tk.id);
      for (const id of [...selected]) {
        const c = byID(id);
        if (!c || isInstalled(c) || c.state !== 'available') selected.delete(id);
      }
    } catch (e) {
      if (!alive) return;
      if (!list) error = e.message || t('common.load_failed');
    }
    redraw();
    schedulePoll();
  }

  function reloadSoon() {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(load, 150);
  }

  function setSources(s) {
    sources = s;
    const key = JSON.stringify([s.pypi, s.models, s.options]);
    if (key !== pickerKey) {
      pickerKey = key;
      picker = sourcePicker(s, { allowCustom: true, onChange: syncSourcesButton });
    }
  }

  // New task statuses (SSE or poll): in-place progress, or a reload of the
  // whole list when a task appeared, finished or changed state.
  function applyTasks(next) {
    if (!list || !Array.isArray(next)) return;
    const prev = new Map(tasks().map((x) => [x.id, x.state]));
    let changed = next.length !== prev.size;
    for (const tk of next) {
      if (prev.get(tk.id) !== tk.state) changed = true;
      if (taskActive(tk)) seenActive.add(tk.id);
    }
    list = { ...list, tasks: next };
    if (changed) {
      reloadSoon(); // states of the components change with their tasks
      redraw();
    } else {
      for (const tk of next) {
        const v = views.get(tk.id);
        if (v && !actingTasks.has(tk.id)) v.update(tk);
      }
    }
    schedulePoll();
  }

  const unsubscribe = onComponentsEvent((ev) => alive && applyTasks(ev && ev.tasks));

  function schedulePoll() {
    clearTimeout(pollTimer);
    if (!alive || !tasks().some(taskActive)) return;
    pollTimer = setTimeout(poll, POLL_MS);
  }

  async function poll() {
    if (!alive) return;
    if (componentsEventFresh()) {
      schedulePoll(); // the event stream delivers the progress
      return;
    }
    try {
      const next = await api.componentTasks({ quiet: true });
      if (!alive) return;
      applyTasks(next || []);
    } catch {
      schedulePoll();
    }
  }

  // ---- install more ----

  function toggle(id, on) {
    if (on) selected.add(id);
    else selected.delete(id);
    clearTimeout(planTimer);
    if (!selected.size) {
      plan = null;
      planning = false;
      redraw();
      return;
    }
    planning = true;
    redraw();
    const ids = [...selected];
    planTimer = setTimeout(async () => {
      let p = null;
      try {
        p = await api.planComponents(ids, { quiet: true });
      } catch (e) {
        if (alive && e.message) toast(e.message, 'error');
      }
      if (!alive || ids.join() !== [...selected].join()) return;
      plan = p;
      planning = false;
      redraw();
    }, PLAN_DELAY);
  }

  async function install() {
    if (installing || !selected.size) return;
    installing = true;
    redraw();
    try {
      const r = await api.installComponents([...selected], { quiet: true });
      if (r && r.task) seenActive.add(r.task.id);
      selected.clear();
      plan = null;
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    installing = false;
    if (alive) load();
  }

  // ---- installed components ----

  async function rowAction(c, fn) {
    if (rowBusy.has(c.id)) return;
    rowBusy.add(c.id);
    rowErrors.delete(c.id);
    redraw();
    try {
      await fn();
    } catch (e) {
      if (alive) rowErrors.set(c.id, e.message || t('common.unknown_error'));
    }
    rowBusy.delete(c.id);
    if (alive) load();
  }

  const update = (c) =>
    rowAction(c, async () => {
      const r = await api.updateComponent(c.id, { quiet: true });
      if (r && r.task) seenActive.add(r.task.id);
    });

  async function uninstall(c) {
    const name = componentName(c);
    const ok = await confirmDialog({
      title: t('settings.comp.uninstall_title', { name }),
      message: t('settings.comp.uninstall_msg', { size: U.fmtBytes(sizeOf(c)) }),
      okText: t('settings.comp.uninstall'),
      danger: true,
    });
    if (!ok || !alive) return;
    rowAction(c, async () => {
      await api.uninstallComponent(c.id, { quiet: true });
      if (alive) toast(t('settings.comp.uninstall_done', { name }), 'success');
    });
  }

  // ---- tasks ----

  async function taskAction(tk, name) {
    if (actingTasks.has(tk.id)) return;
    if (name === 'change_source') {
      let src = sources;
      if (!src) {
        try {
          src = await api.componentSources();
          setSources(src);
        } catch {
          return;
        }
      }
      const next = await chooseSourcesDialog(src);
      if (!next || !alive) return;
      try {
        setSources(await api.saveComponentSources(next));
      } catch {
        return; // shown by the error handler
      }
      name = 'retry';
    }
    actingTasks.add(tk.id);
    const v = views.get(tk.id);
    if (v) v.update(tk);
    try {
      await api.componentTaskAction(tk.id, name);
      if (name === 'retry' || name === 'resume') seenActive.add(tk.id);
    } catch {
      // shown by the error handler
    }
    actingTasks.delete(tk.id);
    if (alive) load();
  }

  // Which tasks get a card: the active ones, the failed ones (the latest
  // few; they offer retry), and those that finished or were cancelled while
  // the page was open. The user can put a finished card away.
  function shownTasks() {
    const all = tasks();
    const failed = all.filter((x) => x.state === 'failed' && !hidden.has(x.id)).slice(-MAX_FAILED_SHOWN);
    return all.filter((x) => !hidden.has(x.id) && (taskActive(x) || failed.includes(x) || ((x.state === 'done' || x.state === 'cancelled') && seenActive.has(x.id))));
  }

  function taskTitle(tk) {
    const names = U.listJoin((tk.ids || []).filter((id) => id !== 'core').map((id) => componentName(byID(id) || id)));
    if (tk.kind === 'import') return names ? t('settings.comp.task.import_named', { names }) : t('settings.comp.task.import');
    return t(tk.kind === 'update' ? 'settings.comp.task.update' : 'settings.comp.task.install', { names });
  }

  // A task's card: its title and "hide" (once it ended) changed in place,
  // its view kept (WV-10).
  function taskCard(tk) {
    let v = views.get(tk.id);
    if (!v) {
      v = taskView({ components: comps, busy: () => actingTasks.has(tk.id), onAction: (name) => taskAction(tasks().find((x) => x.id === tk.id) || tk, name) });
      views.set(tk.id, v);
    }
    v.update(tk);
    let c = cards.get(tk.id);
    if (!c) {
      const title = h('span', { class: 'comp-task-title' });
      const hd = h('div', { class: 'comp-task-hd' }, title);
      c = { el: h('div', { class: 'comp-task', dataset: { task: tk.id } }, hd, v.el), hd, title, hide: null };
      cards.set(tk.id, c);
    }
    const text = taskTitle(tk);
    if (c.title.textContent !== text) c.title.textContent = text;
    if (!taskActive(tk) && !c.hide) {
      c.hide = iconBtn('x', t('settings.comp.task.hide'), {
        size: 14,
        fkey: `comp:task:${tk.id}:hide`,
        onclick: () => {
          hidden.add(tk.id);
          redraw();
        },
      });
    }
    keepChildren(c.hd, [c.title, taskActive(tk) ? null : c.hide]);
    keepChildren(c.el, [c.hd, v.el]);
    return c.el;
  }

  // ---- download sources ----

  let sourcesBtn = null;
  let sourcesMsg = null;

  function sourcesValue() {
    return picker ? picker.value() : null;
  }

  function sourcesDirty() {
    const v = sourcesValue();
    if (!v || !sources) return !!picker && !v; // an unfinished custom address counts as a change
    const norm = (s) => JSON.stringify({ mode: s.mode || 'default', mirror_id: s.mirror_id || '', custom_url: s.custom_url || '' });
    return norm(v.pypi) !== norm(sources.pypi) || norm(v.models) !== norm(sources.models);
  }

  function syncSourcesButton() {
    if (!sourcesBtn) return;
    const v = sourcesValue();
    sourcesBtn.disabled = savingSources || !v || !sourcesDirty();
    sourcesBtn.hidden = !sourcesDirty();
    if (sourcesMsg) {
      sourcesMsg.hidden = !!v;
    }
  }

  async function saveSources() {
    const v = sourcesValue();
    if (!v || savingSources) return;
    savingSources = true;
    syncSourcesButton();
    try {
      const s = await api.saveComponentSources(v);
      if (!alive) return;
      pickerKey = ''; // rebuild from what was stored
      setSources(s);
      toast(t('settings.comp.sources_saved'), 'success');
    } catch {
      // shown by the error handler; the picker keeps the edit
    }
    savingSources = false;
    redraw();
  }

  // ---- import ----

  const webPicker = filePicker({ accept: '.zip,application/zip', multiple: false, onFiles: (files) => importFile(files[0]) });

  async function startImport() {
    const nat = nativeImport();
    if (!nat) {
      webPicker.open();
      return;
    }
    let path = '';
    try {
      path = await nat.importFile();
    } catch (e) {
      console.warn('components.importFile failed, using the file dialog', e);
      webPicker.open();
      return;
    }
    if (!path || typeof path !== 'string' || !alive) return;
    try {
      const r = await api.importComponentPath(path);
      if (r && r.task) seenActive.add(r.task.id);
      toast(t('settings.comp.import_started'), 'info');
    } catch {
      // shown by the error handler
    }
    if (alive) load();
  }

  // A .zip from the file dialog: uploaded with progress (web mode), or sent
  // as its path (a file the desktop app's dialog picked).
  async function importFile(file) {
    if (!file || upload) return;
    const job = api.importComponentFile(file, (fraction) => {
      if (!upload) return;
      upload.fraction = fraction;
      if (upload.textEl) upload.textEl.textContent = uploadText();
    });
    upload = { name: file.name, fraction: 0, abort: job.abort, textEl: null };
    redraw();
    try {
      const r = await job.promise;
      if (r && r.task) seenActive.add(r.task.id);
      if (alive) toast(t('settings.comp.import_started'), 'info');
    } catch {
      // failures are shown by the error handler; a cancel is quiet
    }
    upload = null;
    if (alive) load();
  }

  function uploadText() {
    if (!upload) return '';
    if (upload.fraction == null || DESKTOP) return t('settings.comp.import_copying', { name: upload.name });
    return t('settings.comp.import_uploading', { name: upload.name, percent: U.fmtPercent(upload.fraction, 0) });
  }

  // ---- render ----

  const note = (ic, text, cls = '') => h('p', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('span', null, text));

  function installedRow(c) {
    const busy = rowBusy.has(c.id) || c.state === 'installing' || !!c.task || tasks().some((x) => taskActive(x) && (x.ids || []).includes(c.id));
    const installedNow = comps().filter((x) => isInstalled(x) && x.id !== c.id);
    const dependents = installedNow.filter((x) => (x.requires || []).includes(c.id));
    const meta = [t('settings.comp.version', { version: c.installed_version || c.version || '' })];
    if (sizeOf(c)) meta.push(t('settings.comp.size', { size: U.fmtBytes(sizeOf(c)) }));
    if (isBundled(c)) meta.push(t('settings.comp.bundled'));
    const notes = [];
    const actions = [];
    if (c.update_version && !isBundled(c)) {
      notes.push(h('span', { class: 'chip chip-run comp-chip' }, icon('arrow-up', { size: 12 }), t('settings.comp.update_to', { version: c.update_version })));
      actions.push(btn(t('settings.comp.update'), { size: 'sm', icon: 'download', disabled: busy, onclick: () => update(c), fkey: `comp:${c.id}:update` }));
    } else if ((c.state === 'broken' || c.rebuild_needed) && !isBundled(c)) {
      actions.push(btn(t('settings.comp.repair'), { size: 'sm', icon: 'rotate-cw', disabled: busy, onclick: () => update(c), fkey: `comp:${c.id}:repair` }));
    }
    if (!isBundled(c)) {
      actions.push(btn(t('settings.comp.uninstall'), { size: 'sm', kind: 'ghost', icon: 'trash-2', disabled: busy || dependents.length > 0, onclick: () => uninstall(c), fkey: `comp:${c.id}:uninstall` }));
    }
    const lines = [];
    if (c.state === 'broken') lines.push(note('triangle-alert', t('settings.comp.broken'), 'is-warn'));
    else if (c.rebuild_needed) lines.push(note('triangle-alert', t('settings.comp.rebuild_needed'), 'is-warn'));
    if (busy && !rowBusy.has(c.id)) lines.push(note('loader-circle', t('settings.comp.busy')));
    else if (dependents.length && !isBundled(c)) lines.push(note('info', t('settings.comp.needed_by', { names: U.listJoin(dependents.map((x) => componentName(x))) })));
    if (rowErrors.has(c.id)) lines.push(h('p', { class: 'set-note is-warn', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, rowErrors.get(c.id))));
    const hint = componentHint(c);
    return h(
      'li',
      { class: 'comp-row', dataset: { comp: c.id } },
      h('span', { class: 'comp-row-icon' }, icon(c.kind === 'model' ? 'layers' : isBundled(c) ? 'shield-check' : 'cpu', { size: 16 })),
      h(
        'div',
        { class: 'comp-row-main' },
        h('div', { class: 'comp-row-name' }, h('span', null, componentName(c)), notes),
        hint ? h('div', { class: 'comp-row-hint' }, hint) : null,
        h('div', { class: 'comp-row-meta' }, meta.join(' · ')),
        lines,
      ),
      actions.length ? h('div', { class: 'comp-row-actions' }, actions) : null,
    );
  }

  function installedBlock() {
    const installed = comps().filter(isInstalled);
    installed.sort((a, b) => Number(isBundled(b)) - Number(isBundled(a)));
    const anyASR = installed.some((c) => !isBundled(c) && c.id !== 'python');
    return h(
      'div',
      { class: 'comp-sec' },
      h('h3', { class: 'comp-sec-title' }, t('settings.comp.installed_title')),
      installed.length ? h('ul', { class: 'comp-list' }, installed.map(installedRow)) : null,
      anyASR ? null : note('info', t('settings.comp.none_installed')),
    );
  }

  function addBlock() {
    const rest = comps().filter((c) => !isInstalled(c) && !isBundled(c));
    const implied = new Set(plan ? (plan.ids || []).filter((id) => !selected.has(id)) : []);
    const offer = rest.some((c) => c.state === 'available');
    const space = planSpaceText(plan);
    return h(
      'div',
      { class: 'comp-sec' },
      h('h3', { class: 'comp-sec-title' }, t('settings.comp.add_title')),
      !rest.length || !offer
        ? note('circle-check', t('settings.comp.all_installed'))
        : [
            h('p', { class: 'comp-sec-hint' }, t('settings.comp.select_hint')),
            componentPicker({ components: rest, selected, implied, onToggle: toggle }),
            h(
              'div',
              { class: 'comp-plan' },
              h('span', { class: 'comp-plan-text' }, planning ? t('settings.comp.planning') : plan ? planSummary(plan) : ''),
              btn(t('settings.comp.install_selected'), {
                kind: 'primary',
                size: 'sm',
                icon: 'download',
                disabled: !selected.size || planning || installing || (plan && plan.enough_space === false),
                onclick: install,
                fkey: 'comp:install',
              }),
            ),
            space ? note('triangle-alert', space, 'is-warn') : null,
          ],
    );
  }

  function tasksBlock() {
    const shown = shownTasks();
    for (const id of [...views.keys()]) {
      if (!shown.some((x) => x.id === id)) {
        views.delete(id);
        cards.delete(id);
      }
    }
    if (!shown.length) return null;
    if (!tasksEl) {
      tasksEl = h('div', { class: 'comp-sec comp-tasks' });
      tasksTitle = h('h3', { class: 'comp-sec-title' }, t('settings.comp.tasks_title'));
    }
    keepChildren(tasksEl, [tasksTitle, ...shown.map(taskCard), shown.some(taskActive) ? note('info', t('settings.comp.task.background')) : null]);
    return tasksEl;
  }

  function sourcesRow() {
    if (!sources || !picker) return null;
    sourcesBtn = btn(savingSources ? t('settings.saving') : t('settings.comp.sources_save'), { size: 'sm', kind: 'primary', onclick: saveSources, fkey: 'comp:sources:save' });
    sourcesMsg = note('triangle-alert', t('settings.comp.sources_invalid'), 'is-warn');
    syncSourcesButton();
    return ui.formRow(
      t('settings.comp.sources_label'),
      h('div', { class: 'comp-sources' }, picker.el, h('div', { class: 'comp-sources-foot' }, sourcesBtn)),
      t('settings.comp.sources_hint'),
      [sourcesMsg, note('info', t('settings.comp.sources_current', { sources: sourcesText(sources) })), note('shield-check', t('settings.comp.sources_fallback'))],
    );
  }

  function importRow() {
    let ctl;
    if (upload) {
      upload.textEl = h('span', null, uploadText());
      ctl = h(
        'div',
        { class: 'set-inline comp-import-busy' },
        icon('loader-circle', { size: 14, class: 'spin' }),
        upload.textEl,
        btn(t('settings.comp.import_cancel'), { size: 'sm', kind: 'ghost', onclick: () => upload && upload.abort(), fkey: 'comp:import:cancel' }),
      );
    } else {
      ctl = btn(t('settings.comp.import_btn'), { size: 'sm', icon: 'upload', onclick: startImport, fkey: 'comp:import' });
    }
    return ui.formRow(t('settings.comp.import_label'), [ctl, webPicker.input], t('settings.comp.import_hint'));
  }

  // The section in its shell, which keeps what render put there before
  // (aligns: view_settings.js does not empty it); the parts that hold the
  // task views are kept, the others drawn anew as before (WV-10).
  function render(shell) {
    if (!headEl) headEl = ui.head('layers', t('settings.comp.title'));
    if (!list) {
      keepChildren(shell, [headEl, h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.comp.load_error', { error }), retry) : skeleton(5))]);
      return;
    }
    if (!bodyEl) {
      bodyEl = h('div', { class: 'card-bd comp-body' });
      introEl = h('p', { class: 'comp-intro' }, t('settings.comp.intro'));
    }
    keepChildren(bodyEl, [introEl, tasksBlock(), installedBlock(), addBlock(), h('div', { class: 'set-form comp-form' }, sourcesRow(), importRow())]);
    keepChildren(shell, [headEl, bodyEl]);
  }

  function retry() {
    error = '';
    redraw();
    load();
  }

  return {
    render,
    aligns: true, // WV-10: render puts its children in place itself
    load,
    destroy() {
      alive = false;
      clearTimeout(pollTimer);
      clearTimeout(reloadTimer);
      clearTimeout(planTimer);
      unsubscribe();
      if (upload) upload.abort();
    },
  };
}
