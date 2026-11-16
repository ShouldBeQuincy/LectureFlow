// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Transcription devices (SPEC-v2 I6, I8, I9; phase 3b): the device list of
// the settings' Transcription block — order (drag, or the up / down
// buttons), on / off, model, precision, parallel tasks, pause, the
// "first use prepares / compiles" notes — and the device test ("test this
// device": progress, the result table and "use"). Every change is saved at
// once through /api/asr/devices and the page shows what the server returned
// (no optimistic changes; while the order is being saved the dropped order
// stays visible until the answer replaces it). Shared with the resource
// panel and the workbench: pauseDevice (asks first while transcriptions
// run) and the names of kinds and combinations.
// ssh-1b: an SSH server's row ("ssh:<slug>", settings_remote.js) has model
// and precision only (the server's own recommendation), its state, no
// device test and a link to Settings → 远程服务器.

import { api } from './api.js';
import { h, clear, btn, iconBtn, toast, actionsDialog, preserveFocus } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { isSSHDevice, serverOfDevice, stateChip } from './settings_remote.js';

const NA = '--';
const MANUAL = '__manual__';
const ACTIVE = new Set(['waiting', 'running']);
const OV_PRECISION_HINT = new Set(['fp16', 'int8', 'int4']);
const POLL_MS = 1000;

/** True while a device test waits or runs. */
export function testActive(dev) {
  return !!dev && !!dev.selftest && ACTIVE.has(dev.selftest.state);
}

/**
 * Pause or resume one device (POST /api/asr/devices/{id}/pause). Pausing a
 * device with running transcriptions asks whether to let them finish or to
 * cancel them, as the main switch does. Resolves to the new ASRDeviceView,
 * or null when the user cancelled or the request failed (already shown).
 */
export async function pauseDevice(dev, paused, running) {
  let cancelRunning = false;
  const n = running != null ? running : (dev.running || []).length;
  if (paused && n > 0) {
    const choice = await actionsDialog({
      title: t('devices.pause_title', { device: dev.label }),
      message: t('devices.pause_message', { n: U.count(n) }),
      actions: [
        { value: 'finish', label: t('nav.gpu.finish') },
        { value: 'cancel', label: t('nav.gpu.cancel_running'), danger: true },
      ],
    });
    if (!choice) return null;
    cancelRunning = choice === 'cancel';
  }
  try {
    return await api.pauseDevice(dev.id, paused, cancelRunning);
  } catch {
    return null; // shown by the API layer
  }
}

// The hint of a precision option (faster-whisper: the compute types of v1.1).
function precisionHint(dev, p) {
  if (dev.backend === 'openvino') return OV_PRECISION_HINT.has(p) ? t(`devices.precision_hint.${p}`) : '';
  return ['auto', 'float16', 'int8_float16'].includes(p) ? t(`settings.asr.ct_hint.${p}`) : '';
}

const sameCombo = (a, m, p) => !!a && a.model === m && a.precision === p;

/**
 * The device list. opts:
 *   settings()      the last Settings (asr: device_mode, language; gpu_total_mb)
 *   resources()     the last resources snapshot (main switch, power)
 *   onDevices(list) a request returned new device views: the page stores them
 *   reload()        GET /api/settings again (a test ended)
 * Returns {el, render(devices), syncTests(devices), destroy()}.
 */
export function createDeviceList(opts) {
  let devs = [];
  const el = h('div', { class: 'dev-sec' });
  const busy = new Set(); // device ids with a request in flight
  const errors = {}; // device id → error text of the last failed change
  const manual = new Set(); // faster-whisper devices whose model is typed by hand
  const openResults = new Set(); // devices whose result table is open
  const finished = new Set(); // devices whose test ended on this page (results shown open)
  let ordering = false; // PUT order in flight
  let detecting = false;
  let pollTimer = null;
  let alive = true;
  let dragId = null;

  const fk = (id, x) => ({ fkey: `set:dev:${id}:${x}` });

  // a test that was running here and has ended shows its results open
  let activeIds = new Set();
  function setDevs(list) {
    devs = (list || []).slice().sort((a, b) => b.priority - a.priority);
    const now = new Set(devs.filter(testActive).map((d) => d.id));
    for (const id of activeIds) if (!now.has(id)) finished.add(id);
    activeIds = now;
  }

  function replaceDevice(v) {
    if (!v) return;
    const i = devs.findIndex((d) => d.id === v.id);
    if (i >= 0) devs[i] = v;
    else devs.push(v);
    setDevs(devs);
    opts.onDevices(devs);
  }

  // ---- render ----

  function render(list) {
    if (list) setDevs(list);
    const was = replacing;
    replacing = true;
    try {
      preserveFocus(() => {
        clear(el);
        el.append(head(), ...[].concat(devs.length ? listEl() : empty()).filter(Boolean));
      });
    } finally {
      replacing = was;
    }
    schedulePoll();
    return el;
  }

  function head() {
    return h(
      'div',
      { class: 'dev-sec-head' },
      h('h3', { class: 'dev-sec-title' }, t('devices.title'), devs.length ? h('span', { class: 'card-count' }, String(devs.length)) : null),
      h(
        'button',
        { type: 'button', class: 'btn btn-sm', disabled: detecting, dataset: { fkey: 'set:dev:detect' }, onclick: detect },
        icon('rotate-cw', { size: 14, class: detecting ? 'spin' : '' }),
        h('span', { class: 'btn-label' }, detecting ? t('devices.detecting') : t('devices.detect')),
      ),
    );
  }

  function empty() {
    return h(
      'div',
      { class: 'set-empty dev-empty' },
      h('div', { class: 'set-empty-icon' }, icon('cpu', { size: 22 })),
      h('div', null, h('strong', null, t('devices.empty_title')), h('p', null, t('devices.empty_text'))),
      h('a', { class: 'btn btn-sm', href: '#/system', dataset: { fkey: 'set:dev:system' } }, icon('stethoscope', { size: 14 }), h('span', { class: 'btn-label' }, t('devices.open_system'))),
    );
  }

  function listEl() {
    const res = opts.resources();
    const allPaused = !!(res && res.gpu && res.gpu.paused);
    const ul = h('ol', { class: 'dev-list' + (ordering ? ' is-busy' : ''), 'aria-busy': ordering ? 'true' : undefined, 'aria-label': t('devices.title') }, devs.map((d, i) => row(d, i)));
    ul.addEventListener('dragover', onDragOver);
    ul.addEventListener('drop', onDrop);
    return [
      // a laptop: "automatic" picks the kind of device by the power state,
      // so the order only ranks devices of one kind and the manual list
      h('p', { class: 'set-intro dev-intro' }, U.batteryMachine(res) ? t('devices.order_hint_battery') : t('devices.order_hint')),
      allPaused ? h('p', { class: 'set-note is-warn' }, icon('circle-pause', { size: 13 }), t('devices.all_paused')) : null,
      ul,
    ];
  }

  function chip(text, cls, title) {
    return h('span', { class: `chip ${cls}`, title: title || undefined }, text);
  }

  function row(d, i) {
    const asr = opts.settings().asr;
    // chosen by hand (power management: manual with its list; a 3b value is one device id)
    const fixed = asr.device_mode === 'manual' ? (asr.manual_devices || []).includes(d.id) : !!asr.device_mode && asr.device_mode !== 'auto' && asr.device_mode === d.id;
    const isBusy = busy.has(d.id) || ordering;
    const li = h('li', {
      class: 'dev-row' + (d.enabled ? '' : ' is-off') + (d.present ? '' : ' is-absent'),
      dataset: { dev: d.id },
      'aria-label': d.label,
    });
    const grip = h('span', { class: 'dev-grip', title: t('devices.drag', { device: d.label }), 'aria-hidden': 'true' }, icon('grip-vertical', { size: 16 }));
    grip.addEventListener('pointerdown', () => {
      if (ordering) return;
      li.draggable = true;
    });
    li.addEventListener('dragstart', (e) => {
      dragId = d.id;
      li.classList.add('is-dragging');
      e.dataTransfer.effectAllowed = 'move';
      try {
        e.dataTransfer.setData('text/plain', d.id);
      } catch {
        // some browsers refuse custom data; the id is kept in dragId
      }
    });
    li.addEventListener('dragend', () => {
      li.draggable = false;
      li.classList.remove('is-dragging');
      if (dragId) finishDrag();
    });
    const sw = h(
      'button',
      {
        type: 'button',
        class: 'switch set-switch' + (d.enabled ? ' is-on' : ''),
        role: 'switch',
        'aria-checked': d.enabled ? 'true' : 'false',
        'aria-label': t('devices.enable', { device: d.label }),
        disabled: isBusy,
        dataset: fk(d.id, 'enabled'),
        onclick: () => patch(d, { enabled: !d.enabled }, false),
      },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'set-switch-text' }, d.enabled ? t('settings.switch.on') : t('settings.switch.off')),
    );
    const up = iconBtn('arrow-up', t('devices.move_up', { device: d.label }), { class: 'dev-move', fkey: `set:dev:${d.id}:up`, onclick: () => move(d.id, -1) });
    const down = iconBtn('arrow-down', t('devices.move_down', { device: d.label }), { class: 'dev-move', fkey: `set:dev:${d.id}:down`, onclick: () => move(d.id, 1) });
    up.disabled = i === 0 || ordering;
    down.disabled = i === devs.length - 1 || ordering;
    const pauseBtn = btn(d.paused ? t('devices.resume') : t('devices.pause'), {
      size: 'sm',
      kind: d.paused ? 'primary' : 'ghost',
      icon: d.paused ? 'play' : 'pause',
      title: d.paused ? t('devices.resume_tip') : t('devices.pause_tip'),
      disabled: isBusy,
      fkey: `set:dev:${d.id}:pause`,
      onclick: () => togglePause(d),
    });
    const ssh = isSSHDevice(d.id);
    const server = ssh ? serverOfDevice(d.id) : null;
    const meta = [ssh ? t('devices.kind.ssh') : U.deviceKindName(d.kind), d.backend === 'openvino' ? 'OpenVINO' : d.backend === 'mlx' ? 'MLX' : d.kind === 'remote' ? '' : d.backend].filter(Boolean).join(' · ');
    li.append(
      h(
        'div',
        { class: 'dev-head' },
        grip,
        h('span', { class: 'dev-rank', 'aria-hidden': 'true' }, String(i + 1)),
        h(
          'div',
          { class: 'dev-title' },
          h('span', { class: 'dev-name', title: d.label }, d.label),
          h(
            'span',
            { class: 'dev-meta' },
            h('span', null, meta),
            // an SSH server says its own state (offline, setup, …) instead of "not present"
            server && server.state !== 'ready' && server.state !== 'busy' ? stateChip(server.state) : null,
            !d.present && !server ? chip(t('devices.not_present'), 'chip-warn', t('devices.not_present_tip')) : null,
            !d.enabled ? chip(t('devices.disabled'), 'chip-quiet') : null,
            d.paused ? chip(t('devices.paused'), 'chip-warn') : null,
            fixed ? chip(t('devices.fixed'), 'chip-run') : null,
          ),
        ),
        h('div', { class: 'dev-tools' }, up, down, pauseBtn, sw),
      ),
      // SPEC-v3 V14 (colab:remote): model and precision live in the Colab
      // settings, and Colab has no device test (it measures every lecture)
      d.kind === 'remote'
        ? h('div', { class: 'dev-body' }, remoteFields(d), notes(d))
        : ssh
          ? h('div', { class: 'dev-body' }, sshFields(d, isBusy), notes(d))
          : h('div', { class: 'dev-body' }, fields(d, isBusy), notes(d), testPanel(d)),
    );
    return li;
  }

  // ---- fields: model, precision, parallel tasks ----

  function fields(d, isBusy, parallel = true) {
    const models = (d.options && d.options.models) || [];
    const fw = d.backend !== 'openvino';
    const inList = models.includes(d.model);
    const typed = fw && (manual.has(d.id) || !inList);
    let modelCtl;
    if (typed) {
      const inp = h('input', {
        class: 'input mono dev-w-model',
        value: d.model,
        spellcheck: 'false',
        autocomplete: 'off',
        disabled: isBusy,
        'aria-label': t('devices.model_label', { device: d.label }),
        placeholder: t('settings.example', { value: 'large-v3' }),
        dataset: fk(d.id, 'model'),
      });
      const commit = () => {
        const v = inp.value.trim();
        if (v !== d.model) patch(d, { model: v });
      };
      inp.addEventListener('change', commit);
      inp.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') commit();
        if (e.key === 'Escape') {
          inp.value = d.model;
          manual.delete(d.id);
          rerenderRow(d.id, 'model');
        }
      });
      const back = h(
        'button',
        {
          type: 'button',
          class: 'set-link',
          dataset: fk(d.id, 'from_list'),
          onclick: () => {
            manual.delete(d.id);
            if (!inList && models.length) patch(d, { model: models[0] });
            else rerenderRow(d.id, 'model');
          },
        },
        t('settings.asr.from_list'),
      );
      modelCtl = h('span', { class: 'set-inline' }, inp, inList || models.length ? back : null);
    } else {
      const options = models.map((m) => h('option', { value: m, selected: m === d.model }, m));
      if (!inList) options.unshift(h('option', { value: d.model, selected: true }, d.model));
      if (fw) options.push(h('option', { value: MANUAL }, t('settings.asr.manual')));
      modelCtl = h(
        'select',
        {
          class: 'select mono dev-w-model',
          disabled: isBusy,
          'aria-label': t('devices.model_label', { device: d.label }),
          dataset: fk(d.id, 'model'),
          onchange: (e) => {
            if (e.target.value === MANUAL) {
              manual.add(d.id);
              rerenderRow(d.id, 'model');
              return;
            }
            patch(d, { model: e.target.value });
          },
        },
        options,
      );
    }
    const precisions = (d.options && d.options.precisions) || [];
    const precOptions = precisions.map((p) => h('option', { value: p, selected: p === d.precision, title: precisionHint(d, p) || undefined }, p));
    if (!precisions.includes(d.precision)) precOptions.unshift(h('option', { value: d.precision, selected: true }, d.precision));
    const precCtl = h(
      'select',
      {
        class: 'select mono dev-w-prec',
        disabled: isBusy,
        title: precisionHint(d, d.precision) || undefined,
        'aria-label': t('devices.precision_label', { device: d.label }),
        dataset: fk(d.id, 'precision'),
        onchange: (e) => patch(d, { precision: e.target.value }),
      },
      precOptions,
    );
    const rec = d.options && d.options.recommended;
    const recChip = sameCombo(rec, d.model, d.precision) ? chip(t('devices.recommended'), 'chip-ok', t('devices.recommended_tip', { combo: U.comboText(rec.model, rec.precision) })) : null;
    const par = h('input', {
      class: 'input input-num dev-w-par',
      type: 'number',
      min: '1',
      max: '8',
      step: '1',
      value: String(d.max_parallel),
      disabled: isBusy,
      'aria-label': t('devices.max_parallel_label', { device: d.label }),
      dataset: fk(d.id, 'par'),
    });
    const commitPar = () => {
      const s = par.value.trim();
      const n = s === '' ? NaN : Number(s);
      if (n === d.max_parallel) return;
      if (!Number.isInteger(n) || n < 1 || n > 8) {
        errors[d.id] = t('errors.devices.max_parallel_invalid', { min: 1, max: 8 });
        rerenderRow(d.id, 'par');
        return;
      }
      patch(d, { max_parallel: n });
    };
    par.addEventListener('change', commitPar);
    par.addEventListener('keydown', (e) => e.key === 'Enter' && commitPar());
    const field = (label, ctl, extra) => h('div', { class: 'dev-field' }, h('span', { class: 'dev-field-k' }, label), h('span', { class: 'dev-field-v' }, ctl, extra));
    return h(
      'div',
      { class: 'dev-fields' },
      field(t('settings.asr.model'), modelCtl),
      field(t('devices.precision'), precCtl, recChip),
      parallel ? field(t('devices.max_parallel'), par) : null,
    );
  }

  // ssh-1b: an SSH server — model and precision (its own recommendation);
  // the connection, agent and environment live in Settings → 远程服务器
  function sshFields(d, isBusy) {
    return h(
      'div',
      { class: 'dev-ssh' },
      fields(d, isBusy, false),
      h(
        'p',
        { class: 'set-note' },
        icon('info', { size: 13 }),
        h('span', null, t('devices.ssh_note'), ' ', h('a', { class: 'set-link', href: '#/settings/remote', dataset: fk(d.id, 'remote') }, t('settings.remote.title'))),
      ),
    );
  }

  // colab:remote: model · precision read-only, edited in the Colab section
  function remoteFields(d) {
    return h(
      'div',
      { class: 'dev-fields dev-remote' },
      h('div', { class: 'dev-field' }, h('span', { class: 'dev-field-k' }, t('settings.asr.model')), h('span', { class: 'dev-field-v mono' }, U.comboText(d.model, d.precision))),
      h(
        'p',
        { class: 'set-note' },
        icon('info', { size: 13 }),
        h('span', null, t('devices.remote_managed_note'), ' ', h('a', { class: 'set-link', href: '#/settings/colab', dataset: fk(d.id, 'colab') }, t('devices.remote_settings_link'))),
      ),
    );
  }

  // ---- notes under the fields ----

  function computeTypeNote(d) {
    if (d.backend === 'openvino') return '';
    const cpu = d.kind === 'cpu';
    if (d.precision !== 'auto') return cpu && d.precision === 'float16' ? t('settings.asr.ct_cpu_float16') : '';
    if (cpu) return t('settings.asr.ct_cpu_auto');
    const total = opts.settings().gpu_total_mb;
    if (!total) return t('settings.asr.ct_unknown_vram');
    return t('settings.asr.ct_gpu', { vram: U.fmtMB(total), type: total >= 10240 ? 'float16' : 'int8_float16' });
  }

  function note(ic, text, cls = '') {
    return h('p', { class: `set-note ${cls}` }, icon(ic, { size: 13 }), h('span', null, text));
  }

  function notes(d) {
    const out = [];
    if (errors[d.id]) out.push(h('p', { class: 'set-note is-err', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, errors[d.id])));
    if (d.backend === 'openvino' && d.model_ready === false) out.push(note('info', t('devices.prepare_note')));
    else if (d.backend === 'openvino' && d.compile_cached === false) out.push(note('info', t('devices.compile_note')));
    if (d.backend !== 'openvino' && d.kind !== 'cpu' && !isSSHDevice(d.id)) {
      const ct = computeTypeNote(d);
      if (ct) out.push(note('info', ct));
    }
    if (d.kind === 'cpu') {
      // "very slow" only for a device that will be used (gate round 2)
      const ct = computeTypeNote(d);
      if (d.enabled) out.push(note('triangle-alert', ct ? U.clauseJoin([t('settings.asr.cpu_slow'), ct]) : t('settings.asr.cpu_slow'), 'is-warn'));
      else if (ct) out.push(note('info', ct));
    }
    if (d.battery_hold) out.push(note('battery-medium', t('devices.battery_hold'), 'is-warn'));
    if ((d.running || []).length) {
      const titles = U.listJoin(d.running.map((r) => r.lecture_title));
      out.push(h('p', { class: 'set-note dev-running', title: titles }, icon('audio-lines', { size: 13 }), h('span', null, t('devices.running', { titles }))));
    }
    return out.length ? h('div', { class: 'dev-notes' }, out) : null;
  }

  // ---- device test (I9) ----

  function testPanel(d) {
    return h('div', { class: 'dev-test', dataset: { devtest: d.id } }, testBody(d));
  }

  function testBody(d) {
    const st = d.selftest || { state: 'idle' };
    if (st.state === 'waiting' || st.state === 'running') return runningTest(d, st);
    // the stored benchmark; else this page's test that ended without one
    // (canceled before any combination finished)
    const bench = d.benchmark || (st.result || null);
    const tested = !!bench && (bench.results || []).length > 0;
    const blocked = d.selftest_blocked;
    // power management: a laptop never tests a CPU device, in any power
    // state (the back end refuses with selftest.power_cpu)
    const cpuOnLaptop = d.kind === 'cpu' && U.batteryMachine(opts.resources());
    const startBtn = btn(tested ? t('selftest.again') : t('selftest.start'), {
      size: 'sm',
      icon: 'gauge',
      kind: tested ? 'ghost' : undefined,
      title: t('selftest.start_tip'),
      disabled: busy.has(d.id) || !!blocked || !d.present || cpuOnLaptop,
      fkey: `set:dev:${d.id}:test`,
      onclick: () => startTest(d),
    });
    const why = !d.present
      ? t('selftest.not_present')
      : blocked
        ? U.errorInfoText(blocked)
        : cpuOnLaptop
          ? U.errorInfoText({ code: 'selftest.power_cpu', params: { device: d.label } })
          : '';
    const out = [h('div', { class: 'dev-test-line' }, startBtn, bench ? benchStatus(bench) : h('span', { class: 'dim small' }, t('selftest.never')))];
    if (why) out.push(note('info', why, 'dev-test-why'));
    if (bench) out.push(...benchView(d, bench));
    return out;
  }

  function runningTest(d, st) {
    const waiting = st.state === 'waiting';
    const frac = Math.max(0, Math.min(1, Number(st.progress) || 0));
    // a combination starts in "transcribing" with progress 0 while its
    // model loads (no loading phase in the reports)
    const loading = !waiting && st.phase === 'transcribing' && frac === 0;
    let phase = '';
    // power management: on a laptop a test waits while another device is
    // active (waiting_for is the power-aware code, e.g. power_queue:{id});
    // without it the test waits for this device's own transcriptions
    if (waiting) phase = st.waiting_for ? U.waitingForText(st.waiting_for, null, opts.resources()) : t('selftest.waiting');
    else if (loading) phase = t('selftest.phase.loading');
    else if (['clip', 'preparing_model', 'compiling_model', 'transcribing'].includes(st.phase)) phase = t(`selftest.phase.${st.phase}`);
    const step = !waiting && st.candidate ? t('selftest.running', { step: st.step, steps: st.steps, combo: U.comboText(st.candidate.model, st.candidate.precision) }) : '';
    const determinate = !waiting && st.phase === 'transcribing' && !loading;
    return [
      h(
        'div',
        { class: 'dev-test-line' },
        btn(t('selftest.cancel'), { size: 'sm', icon: 'ban', kind: 'ghost', disabled: busy.has(d.id), fkey: `set:dev:${d.id}:test`, onclick: () => cancelTest(d) }),
        h('span', { class: 'dev-test-state', role: 'status' }, icon('loader-circle', { size: 14, class: 'spin' }), h('span', null, step || phase)),
      ),
      step && phase ? h('p', { class: 'dev-test-phase' }, phase) : null,
      h(
        'div',
        {
          class: 'dev-test-bar' + (determinate ? '' : ' is-indeterminate'),
          role: 'progressbar',
          'aria-label': t('selftest.progress_label'),
          'aria-valuemin': '0',
          'aria-valuemax': '100',
          'aria-valuenow': determinate ? String(Math.round(frac * 100)) : undefined,
          'aria-valuetext': determinate ? U.pct(frac) : phase || undefined,
        },
        h('span', { class: 'dev-test-fill', style: { width: determinate ? `${(frac * 100).toFixed(1)}%` : '' } }),
      ),
    ];
  }

  function benchStatus(b) {
    const at = b.finished_at || b.started_at;
    const when = U.relTime(at);
    let key = b.status === 'failed' ? 'selftest.failed' : b.status === 'cancelled' ? 'selftest.cancelled' : 'selftest.done';
    // canceled before any combination finished (not stored, gate round 1):
    // no "the finished part is below", and no time when there is none
    if (b.status === 'cancelled' && (!(b.results || []).length || !when)) key = 'selftest.cancelled_empty';
    const ic = b.status === 'failed' ? 'circle-alert' : b.status === 'cancelled' ? 'ban' : 'circle-check';
    return h('span', { class: `dev-test-status is-${b.status}`, title: at ? U.fmtDateTime(at) : undefined }, icon(ic, { size: 14 }), h('span', null, t(key, { when })));
  }

  function benchView(d, b) {
    const out = [];
    if (b.error) out.push(note('circle-alert', U.errorInfoText(b.error), 'is-err'));
    const pick = b.pick;
    if (pick) {
      const combo = U.comboText(pick.model, pick.precision);
      const line = t(pick.rule === 'lowest_rtf' ? 'selftest.pick_fastest' : 'selftest.pick_best', { combo });
      if (sameCombo(pick, d.model, d.precision)) {
        out.push(h('div', { class: 'dev-pick is-current' }, icon('circle-check', { size: 14 }), h('span', null, line), h('span', { class: 'dim' }, t('selftest.pick_in_use'))));
      } else {
        out.push(
          h(
            'div',
            { class: 'dev-pick' },
            icon('sparkles', { size: 14 }),
            h('span', null, line),
            h('span', null, t('selftest.ask_apply')),
            btn(t('selftest.apply_pick'), { size: 'sm', kind: 'primary', disabled: busy.has(d.id), fkey: `set:dev:${d.id}:apply`, onclick: () => apply(d, pick.model, pick.precision) }),
          ),
        );
      }
    }
    if (b.audio && b.audio.reference_words === 0 && (b.results || []).length) out.push(note('info', t('selftest.no_reference')));
    if ((b.results || []).length) {
      const det = h('details', { class: 'dev-results', open: openResults.has(d.id) || finished.has(d.id) || undefined, dataset: fk(d.id, 'results') });
      det.addEventListener('toggle', () => (det.open ? openResults.add(d.id) : (openResults.delete(d.id), finished.delete(d.id))));
      det.append(
        h('summary', null, icon('chevron-right', { size: 14, class: 'dev-results-chev' }), t('selftest.results'), b.audio && b.audio.end_ms > b.audio.start_ms ? h('span', { class: 'dim small dev-clip' }, t('selftest.clip', { start: U.fmtClock(b.audio.start_ms), end: U.fmtClock(b.audio.end_ms) })) : null),
        resultsTable(d, b),
      );
      out.push(det);
    }
    return out;
  }

  function resultsTable(d, b) {
    const pick = b.pick;
    const head = h(
      'tr',
      null,
      h('th', { scope: 'col' }, t('selftest.col.combo')),
      h('th', { scope: 'col', class: 'is-num', title: t('selftest.rtf_tip') }, t('selftest.col.rtf')),
      h('th', { scope: 'col', class: 'is-num', title: t('selftest.wer_tip') }, t('selftest.col.wer')),
      h('th', { scope: 'col', class: 'is-num' }, t('selftest.col.load')),
      h('th', { scope: 'col' }, t('selftest.col.result')),
    );
    const rows = b.results.map((r, i) => {
      const combo = U.comboText(r.model, r.precision);
      const isPick = sameCombo(pick, r.model, r.precision);
      const isCur = sameCombo(r, d.model, d.precision);
      const tags = [];
      if (r.first_compile) tags.push(t('selftest.first_compile'));
      if (r.prepared) tags.push(t('selftest.prepared'));
      const errText = r.error ? U.errorInfoText(r.error) : '';
      return h(
        'tr',
        { class: (isPick ? 'is-pick' : '') + (r.ok ? '' : ' is-failed') },
        h('td', null, h('span', { class: 'mono' }, combo), isPick ? chip(t('selftest.pick'), 'chip-ok') : null, isCur ? chip(t('selftest.current'), 'chip-quiet') : null),
        h('td', { class: 'is-num mono' }, r.ok ? U.fmtDecimal(r.rtf, 2) : NA),
        h('td', { class: 'is-num mono' }, r.ok && r.wer != null ? U.fmtPercent(r.wer, 1) : NA),
        h('td', { class: 'is-num', title: tags.length ? U.listJoin(tags) : undefined }, r.ok || r.load_s ? U.fmtDuration(r.load_s * 1000) : NA, tags.length ? h('span', { class: 'dev-load-tag', 'aria-hidden': 'true' }, '*') : null, tags.length ? h('span', { class: 'visually-hidden' }, U.listJoin(tags)) : null),
        h(
          'td',
          null,
          r.ok
            ? isCur
              ? h('span', { class: 'tone-ok' }, icon('check', { size: 13 }), t('common.status.done'))
              : h(
                  'button',
                  {
                    type: 'button',
                    class: 'btn btn-sm',
                    disabled: busy.has(d.id),
                    'aria-label': t('selftest.apply_aria', { device: d.label, combo }),
                    dataset: fk(d.id, `apply:${i}`),
                    onclick: () => apply(d, r.model, r.precision),
                  },
                  t('selftest.apply'),
                )
            : h('span', { class: 'tone-err', title: errText || undefined }, icon('circle-alert', { size: 13 }), errText || t('common.status.failed')),
        ),
      );
    });
    return h('div', { class: 'dev-results-wrap' }, h('table', { class: 'dev-results-table' }, h('thead', null, head), h('tbody', null, rows)));
  }

  // ---- in-place updates ----

  // Replacing a row blurs a focused input in it, whose "change" may ask for
  // the row again: one replacement at a time.
  let replacing = false;
  function rerenderRow(id, focusField) {
    if (replacing) return;
    const li = el.querySelector(`.dev-row[data-dev="${CSS.escape(id)}"]`);
    const i = devs.findIndex((d) => d.id === id);
    if (!li || i < 0) {
      render();
      return;
    }
    replacing = true;
    try {
      preserveFocus(() => li.replaceWith(row(devs[i], i)));
    } finally {
      replacing = false;
    }
    if (focusField) {
      const f = el.querySelector(`[data-fkey="${CSS.escape(`set:dev:${id}:${focusField}`)}"]`);
      if (f && document.activeElement !== f) f.focus();
    }
  }

  function rerenderTest(id) {
    const box = el.querySelector(`.dev-test[data-devtest="${CSS.escape(id)}"]`);
    const d = devs.find((x) => x.id === id);
    if (!box || !d) return;
    preserveFocus(() => {
      clear(box);
      const body = testBody(d);
      for (const n of [].concat(body)) if (n) box.append(n);
    });
  }

  // ---- requests ----

  async function patch(d, p, announce = true) {
    if (busy.has(d.id)) return;
    busy.add(d.id);
    delete errors[d.id];
    rerenderRow(d.id);
    try {
      const v = await api.patchDevice(d.id, p, { quiet: true });
      busy.delete(d.id);
      manual.delete(d.id);
      if (!alive) return;
      replaceDevice(v);
      if (announce) toast(t('devices.saved', { device: v.label }), 'success');
    } catch (e) {
      busy.delete(d.id);
      if (!alive) return;
      errors[d.id] = e.message;
    }
    rerenderRow(d.id);
  }

  async function togglePause(d) {
    busy.add(d.id);
    rerenderRow(d.id);
    const v = await pauseDevice(d, !d.paused);
    busy.delete(d.id);
    if (!alive) return;
    if (v) replaceDevice(v);
    rerenderRow(d.id);
  }

  async function move(id, delta) {
    const ids = devs.map((d) => d.id);
    const i = ids.indexOf(id);
    const j = i + delta;
    if (i < 0 || j < 0 || j >= ids.length) return;
    [ids[i], ids[j]] = [ids[j], ids[i]];
    await saveOrder(ids, `set:dev:${id}:${delta < 0 ? 'up' : 'down'}`);
  }

  async function saveOrder(ids, focusKey) {
    if (ordering) return;
    ordering = true;
    el.querySelector('.dev-list')?.classList.add('is-busy');
    try {
      const list = await api.orderDevices(ids);
      ordering = false;
      if (!alive) return;
      setDevs(list);
      opts.onDevices(devs);
    } catch {
      ordering = false; // shown by the API layer; the saved order comes back
    }
    if (!alive) return;
    render();
    const f = focusKey && el.querySelector(`[data-fkey="${CSS.escape(focusKey)}"]`);
    if (f && !f.disabled) f.focus();
    else if (f) {
      const row = f.closest('.dev-row');
      const other = row && row.querySelector('.dev-move:not([disabled])');
      if (other) other.focus();
    }
  }

  // drag: the dragged row moves in the list while dragging; the order is
  // saved when it is dropped
  function onDragOver(e) {
    if (!dragId || ordering) return;
    e.preventDefault();
    const ul = e.currentTarget;
    const dragged = ul.querySelector(`.dev-row[data-dev="${CSS.escape(dragId)}"]`);
    const over = e.target.closest && e.target.closest('.dev-row');
    if (!dragged || !over || over === dragged) return;
    const r = over.getBoundingClientRect();
    const after = e.clientY > r.top + r.height / 2;
    if (after && over.nextSibling !== dragged) over.after(dragged);
    else if (!after && over.previousSibling !== dragged) over.before(dragged);
  }

  function onDrop(e) {
    if (!dragId) return;
    e.preventDefault();
    finishDrag();
  }

  function finishDrag() {
    const id = dragId;
    dragId = null;
    const ids = [...el.querySelectorAll('.dev-row')].map((li) => li.dataset.dev);
    if (ids.join('\n') === devs.map((d) => d.id).join('\n')) return;
    saveOrder(ids, `set:dev:${id}:up`);
  }

  async function detect() {
    if (detecting) return;
    detecting = true;
    render();
    try {
      const list = await api.detectDevices();
      if (!alive) return;
      setDevs(list);
      opts.onDevices(devs);
    } catch {
      // shown by the API layer
    }
    detecting = false;
    if (alive) render();
  }

  async function startTest(d) {
    busy.add(d.id);
    rerenderTest(d.id);
    try {
      const st = await api.startSelfTest(d.id);
      busy.delete(d.id);
      if (!alive) return;
      const cur = devs.find((x) => x.id === d.id);
      if (cur && st) cur.selftest = st;
      finished.delete(d.id);
      if (testActive(cur)) activeIds.add(d.id);
    } catch {
      busy.delete(d.id); // 409 selftest.running / selftest.no_audio: shown by the API layer
    }
    if (!alive) return;
    rerenderTest(d.id);
    schedulePoll();
    const f = el.querySelector(`[data-fkey="${CSS.escape(`set:dev:${d.id}:test`)}"]`);
    if (f) f.focus();
  }

  async function cancelTest(d) {
    busy.add(d.id);
    rerenderTest(d.id);
    try {
      const st = await api.cancelSelfTest(d.id);
      if (st) ended(d.id, st);
    } catch {
      // shown by the API layer
    }
    busy.delete(d.id);
    if (alive) rerenderTest(d.id);
  }

  async function apply(d, m, p) {
    if (busy.has(d.id)) return;
    busy.add(d.id);
    rerenderTest(d.id);
    try {
      const v = await api.applySelfTest(d.id, m, p);
      busy.delete(d.id);
      if (!alive) return;
      replaceDevice(v);
      toast(t('selftest.applied', { device: v.label, combo: U.comboText(v.model, v.precision) }), 'success');
    } catch {
      busy.delete(d.id); // shown by the API layer
    }
    if (alive) rerenderRow(d.id);
  }

  // A test ended (seen by polling or a cancel): its results open, the
  // device row comes back from the server with the stored benchmark.
  function ended(id, st) {
    const cur = devs.find((x) => x.id === id);
    if (cur) {
      cur.selftest = st;
      // a test canceled before any combination finished is not stored:
      // the device keeps its last benchmark (gate round 1)
      if (st.result && (st.result.results || []).length) cur.benchmark = st.result;
    }
    finished.add(id);
    opts.reload();
  }

  // While a test waits or runs: GET .../selftest every second, the test
  // panel is updated in place (nothing else is re-rendered).
  function schedulePoll() {
    clearTimeout(pollTimer);
    if (!alive || !devs.some(testActive)) return;
    pollTimer = setTimeout(poll, POLL_MS);
  }

  async function poll() {
    const active = devs.filter(testActive);
    await Promise.all(
      active.map(async (d) => {
        try {
          const st = await api.selfTest(d.id, { quiet: true });
          if (!alive || !st) return;
          if (ACTIVE.has(st.state)) {
            d.selftest = st;
            rerenderTest(d.id);
          } else ended(d.id, st);
        } catch {
          // try again with the next tick
        }
      }),
    );
    schedulePoll();
  }

  /** New device views from a settings reload: only the test panels change while nothing else did. */
  function syncTests(list) {
    setDevs(list);
    for (const d of devs) rerenderTest(d.id);
    schedulePoll();
  }

  return {
    el,
    render,
    syncTests,
    destroy() {
      alive = false;
      clearTimeout(pollTimer);
    },
  };
}
