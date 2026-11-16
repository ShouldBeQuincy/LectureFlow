// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Resource inspector, the collapsible right column (v1.1 A10): GPU (name,
// backend, utilisation and temperature with 60 s sparklines, VRAM, power,
// ASR slots, the GPU pause switch moved here from the v1 top bar) and one
// row per provider (balance, tokens today / total, cache hit rate,
// concurrency). Data: GET /api/resources and SSE "resources"; a value the
// back end does not have is shown as "--". v2 P12.4: a provider with a
// tariff shows its peak / off-peak tag next to the name (updated in place,
// so its hover text survives the 2-second refresh).
// SPEC-v2 I10 (phase 3b): "transcription devices" lists one card per turned-on
// device (name, backend · model · precision, current tasks, slots; the CUDA
// card keeps the nvidia-smi metrics, an Intel device shows its usage or
// "--"), "battery mode" on the cards while on battery, a switch per device
// and the main switch over all of them.
// Power management (power-req part 2): with the power policy the panel has
// three areas — system and power (CPU, memory, CPU temperature, the power
// source with charge and charging, the tasks the power-aware policy
// suspended), the active transcription device cards with why they are
// active, and the other devices one row each (a row opens its details and
// closes again; the active device does not change).
// SPEC-v3 V14.5: with Colab switched on (snapshot "remote" not null) a remote
// GPU area sits between the active devices and the other devices: the
// session (service and accelerator, state, usage window, how many more
// lectures fit, the current lecture's step, the plan, compute units, the
// authorization's time left) and the two shutdown buttons; without a
// session it is one line "远端 GPU：未使用".
// colab-obs (2026-10-27): the area also reads the snapshot's remotes[]
// (internal/remote.Card: the machine and its readings) and the account of
// the Colab status — see the remote GPU section below; the collapsed rail
// has a Colab cell (GPU usage and a mini memory bar with a session, the
// compute units left without one, "--" in the warning colour when the
// account was not read).
// mac-opti M6: a reading the system cannot give at all (system.unsupported,
// e.g. the CPU temperature on macOS) is left out instead of showing "--";
// on Apple silicon (system.unified_memory) the memory is "统一内存", and the
// Apple GPU's card shows how much of it the GPU holds, without a
// temperature tile.
// stream-parallel: the system area says when playback files are being made
// in the background (the stream task: a CPU slot, in parallel with
// transcription), from the job events (opts.subscribe).
// ssh-1b: the snapshot's remotes[] of kind ssh (the user's own SSH servers,
// settings_remote.js) follow the Colab area, one card each: connected, the
// machine as nvidia-smi on it reports itself, latency, agent version and
// where the job files live, the CPU and GPU sparklines, the same segmented
// memory bar as Colab, the GPU memory used and the lectures running there;
// not connected, one line (name and state).

import { h, clear, iconBtn, errorBlock, skeleton, preserveFocus, toast } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t, has } from './i18n.js';
import { tariffTag, updateTariffTag, hasPeaks } from './tariff_ui.js';
import * as C from './settings_colab.js';
import * as R from './settings_remote.js';

const NA = '--';
const SVG_NS = 'http://www.w3.org/2000/svg';

function svg(tag, attrs, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
  for (const c of children) if (c) el.append(c);
  return el;
}

// 60 s sparkline (oldest first, one sample every 2 s, right-aligned). A
// missing reading (null) keeps its place on the time axis and breaks the
// line there (the time is never compressed). lo/hi fix the scale; null =
// from the data.
function sparkline(values, { lo = null, hi = null, label }) {
  const W = 120;
  const H = 30;
  const box = svg('svg', { viewBox: `0 0 ${W} ${H}`, class: 'spark', preserveAspectRatio: 'none', role: 'img', 'aria-label': label });
  const isNum = (v) => typeof v === 'number' && isFinite(v);
  const list = (values || []).slice(-30);
  const vals = list.filter(isNum);
  if (vals.length < 2) {
    box.append(svg('line', { x1: 0, y1: H - 1, x2: W, y2: H - 1, class: 'spark-empty' }));
    return box;
  }
  let min = lo != null ? lo : Math.min(...vals);
  let max = hi != null ? hi : Math.max(...vals);
  if (max - min < 6) {
    const mid = (max + min) / 2;
    min = mid - 3;
    max = mid + 3;
  }
  const step = W / 29; // 30 samples
  const x0 = W - step * (list.length - 1);
  const yOf = (v) => H - 2 - ((Math.min(max, Math.max(min, v)) - min) / (max - min)) * (H - 4);
  // the runs of consecutive readings, each drawn on its own
  const runs = [];
  let run = [];
  list.forEach((v, i) => {
    if (isNum(v)) run.push([x0 + i * step, yOf(v)]);
    else if (run.length) {
      runs.push(run);
      run = [];
    }
  });
  if (run.length) runs.push(run);
  const fmt = (pts) => pts.map(([x, y]) => `${x.toFixed(1)},${y.toFixed(1)}`).join(' ');
  for (const pts of runs) {
    if (pts.length === 1) {
      box.append(svg('circle', { cx: pts[0][0].toFixed(1), cy: pts[0][1].toFixed(1), r: 1.2, class: 'spark-dot' }));
      continue;
    }
    const line = fmt(pts);
    box.append(
      svg('polygon', { points: `${pts[0][0].toFixed(1)},${H} ${line} ${pts[pts.length - 1][0].toFixed(1)},${H}`, class: 'spark-area' }),
      svg('polyline', { points: line, class: 'spark-line' }),
    );
  }
  // the newest sample, when it was read
  if (isNum(list[list.length - 1])) box.append(svg('circle', { cx: W.toFixed(1), cy: yOf(list[list.length - 1]).toFixed(1), r: 2, class: 'spark-dot' }));
  return box;
}

function meter(frac, cls = '') {
  const f = frac == null || !isFinite(frac) ? 0 : Math.max(0, Math.min(1, frac));
  return h('div', { class: 'meter ' + cls, role: 'presentation' }, h('span', { class: 'meter-fill', style: { width: `${(f * 100).toFixed(1)}%` } }));
}

// Concurrency as cells ("3 / 8"); a plain meter above 16.
function slots(used, max, label) {
  if (!max) return h('span', { class: 'slots-text' }, NA);
  if (max > 16) return meter(used / max, 'meter-accent');
  return h(
    'span',
    { class: 'slots', role: 'img', 'aria-label': `${label} ${used} / ${max}` },
    Array.from({ length: max }, (_, i) => h('span', { class: 'slot' + (i < used ? ' is-on' : '') })),
  );
}

const num = (v) => (v == null ? NA : U.fmtInt(v)); // the tiles show the unit apart, smaller

// Hover text of a failed balance query: the coded reason when the catalog
// has it (its text is complete), else the back end's text after a prefix.
function balanceErrorText(p) {
  const info = p.balance_error_info;
  if (info && info.code && has(`errors.${info.code}`)) return U.errorInfoText(info, p.balance_error);
  return t('resources.balance_failed', { error: p.balance_error || (info && info.detail) || '' });
}

// opts: {onToggleGPU(), onToggleDevice(card), onCollapse(), onExpand(),
// subscribe(fn) (the app's job events)}
export function mountInspector(el, opts) {
  let res = null;
  let error = '';
  let loading = true;
  // stream-parallel: lecture id → progress of each playback file being made
  // in the background now (stream jobs that run); emptied when the event
  // stream comes back (the next progress event fills it again)
  const bgStreams = new Map();
  if (opts.subscribe) {
    opts.subscribe((type, ev) => {
      const had = bgStreams.size;
      if (type === 'reconnected') bgStreams.clear();
      else if (type === 'job' && ev && ev.stage === 'stream') {
        if (ev.status === 'running') bgStreams.set(ev.scope_id, ev.progress || 0);
        else bgStreams.delete(ev.scope_id);
      } else return;
      // a start or an end shows at once; the progress with the next snapshot
      if (bgStreams.size !== had && parts && parts.sysBody) {
        const b = systemBody();
        parts.sysBody.replaceWith(b);
        parts.sysBody = b;
      }
    });
  }
  // While the full panel is shown: {sw (GPU switch, kept), gpuBody, prov,
  // foot, provKey}; while collapsed: {rail: {k: value element}}. Updates
  // every 2 s change values in place — interactive elements are never
  // replaced under the pointer (a click must not get lost, SPEC §0.1).
  let parts = null;
  // the system readings of the last 60 s for the sparklines (one per new
  // sample; the back end sends the current value only)
  const cpuHist = [];
  const tempHist = [];
  let histAt = '';

  function pushHistory(r) {
    if (!r || !r.system || r.sampled_at === histAt) return;
    histAt = r.sampled_at;
    for (const [list, v] of [[cpuHist, r.system.cpu_pct], [tempHist, r.system.cpu_temp_c]]) {
      list.push(v);
      if (list.length > 30) list.shift();
    }
  }

  function setResources(r) {
    const fresh = loading || !!error || offline;
    C.noteRemote(r && r.remote);
    pushHistory(r);
    res = r;
    error = '';
    loading = false;
    offline = false; // a snapshot came: the service answers
    if (fresh || !parts) preserveFocus(render);
    else update();
  }

  // T24 G6, T25 G4: the event stream dropped (app.js): "已断开" in place of
  // "实时", and none of the readings of before the service went (they no
  // longer change); the next snapshot draws the panel again.
  let offline = false;
  function setOffline(on) {
    if (offline === !!on) return;
    offline = !!on;
    preserveFocus(render);
  }

  function update() {
    if (parts.rail) {
      updateRail();
      return;
    }
    if (devMode() !== parts.devMode) {
      preserveFocus(render);
      return;
    }
    syncSwitch(parts.sw, res.gpu);
    updateRemote();
    updateSSH();
    if (parts.devMode) updateDevices();
    else {
      const g = gpuBody(res.gpu);
      parts.gpuBody.replaceWith(g);
      parts.gpuBody = g;
    }
    const key = provKeyOf();
    if (key !== parts.provKey) {
      const list = res.providers || [];
      if (provShape(list) === parts.provShape) {
        // same providers: rows change in place (the tags keep their hover)
        for (const p of list) updateProviderRow(parts.provRows.get(p.id), p);
      } else {
        const p = providersSection(list);
        parts.prov.replaceWith(p);
        parts.prov = p;
      }
      parts.provKey = key;
    }
    parts.foot.textContent = t('resources.sampled_at', { time: timeOf(res.sampled_at) });
  }

  function budgetHit() {
    const sys = opts.system ? opts.system() : null;
    return !!(sys && sys.budget_hit);
  }

  function provKeyOf() {
    return JSON.stringify([budgetHit(), res.providers || []]);
  }

  // what decides the section's structure (rows are updated in place otherwise)
  function provShape(list) {
    return JSON.stringify([budgetHit(), list.map((p) => p.id)]);
  }

  function setError(msg) {
    if (res) return; // keep showing the last good snapshot
    error = msg || t('resources.load_error');
    loading = false;
    preserveFocus(render);
  }

  function render() {
    clear(el);
    parts = null;
    const collapsed = document.documentElement.dataset.insp === 'collapsed';
    if (collapsed) {
      el.append(buildRail());
      return;
    }
    el.append(
      h(
        'div',
        { class: 'insp-head' },
        h('h2', { class: 'insp-title' }, t('resources.title')),
        offline
          ? h('span', { class: 'insp-live tone-warn', title: t('resources.offline_tip') }, icon('circle-alert', { size: 12 }), t('resources.offline'))
          : res
            ? h('span', { class: 'insp-live', title: t('resources.live_tip') }, h('span', { class: 'live-dot' }), t('resources.live'))
            : null,
        iconBtn('panel-right-close', t('resources.collapse'), { class: 'insp-toggle', fkey: 'insp:toggle', onclick: opts.onCollapse }),
      ),
    );
    if (offline) {
      el.append(h('div', { class: 'insp-body' }, h('p', { class: 'insp-note tone-warn', role: 'status' }, icon('circle-alert', { size: 14 }), t('resources.offline_note'))));
      return;
    }
    if (loading) {
      el.append(h('div', { class: 'insp-body' }, skeleton(4), skeleton(3)));
      return;
    }
    if (error) {
      el.append(h('div', { class: 'insp-body' }, errorBlock(error, opts.onRetry)));
      return;
    }
    const sw = gpuSwitch();
    parts = { sw, devMode: devMode(), foot: h('div', { class: 'insp-foot' }, t('resources.sampled_at', { time: timeOf(res.sampled_at) })) };
    syncSwitch(sw, res.gpu);
    let gpuSec;
    if (parts.devMode) gpuSec = devicesSection(sw);
    else {
      parts.gpuBody = gpuBody(res.gpu);
      gpuSec = h('section', { class: 'insp-sec', 'aria-label': t('resources.gpu.title') }, h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.gpu.title')), sw), parts.gpuBody);
    }
    parts.prov = providersSection(res.providers || []);
    parts.provKey = provKeyOf();
    // without devices the remote area follows the GPU section
    const remote = parts.devMode ? null : remoteArea();
    const ssh = parts.devMode ? null : sshArea();
    el.append(h('div', { class: 'insp-body' }, gpuSec, remote, ssh, parts.prov), parts.foot);
  }

  // ---- transcription devices (SPEC-v2 I10) and power management ----

  // A back end with transcription devices reports the power state (and the
  // cards of the turned-on devices; none are omitted).
  function devMode() {
    return !!res && (!!res.power || (Array.isArray(res.devices) && res.devices.length > 0));
  }
  const rows = () => (res && res.devices) || [];
  // Power management (power-req part 2): a back end with the power policy
  // lists every device row and marks the active ones; the panel has three
  // areas — system and power, the active device cards, the other devices
  // one row each. Without it (phase 3b) every row is a card.
  const policy = () => (res && res.policy) || null;
  const cards = () => (policy() ? rows().filter((d) => d.active) : rows());
  const standby = () => (policy() ? rows().filter((d) => !d.active) : []);
  const suspendedCount = () => {
    const p = policy();
    return p ? (p.wait_ac || 0) + (p.wait_no_device || 0) : 0;
  };
  const devShapeOf = () => JSON.stringify([!!policy(), !!(res && res.system), cards().map((d) => d.id), standby().map((d) => d.id)]);

  function devicesSection(sw) {
    parts.devRefs = new Map();
    parts.sbRefs = new Map();
    parts.devShape = devShapeOf();
    parts.devTop = devTop();
    const list = cards().length ? h('div', { class: 'dcards' }, cards().map(deviceCard)) : null;
    const active = h(
      'section',
      { class: 'insp-sec', 'aria-label': policy() ? t('resources.active.title') : t('resources.gpu.title') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, policy() ? t('resources.active.title') : t('resources.gpu.title')), sw),
      parts.devTop,
      list,
    );
    const secs = [];
    if (policy() || res.system) secs.push(systemSection());
    secs.push(active);
    secs.push(remoteArea());
    secs.push(sshArea());
    if (standby().length) secs.push(standbySection());
    parts.devSec = h('div', { class: 'insp-group' }, secs);
    return parts.devSec;
  }

  // The notes above the cards: the main switch is off; the manual list does
  // not apply on battery; why there is no active device.
  function devTop() {
    const p = policy();
    const notes = [];
    if (res.gpu && res.gpu.paused) notes.push(h('p', { class: 'insp-note tone-warn' }, icon('circle-pause', { size: 14 }), t('resources.devices.paused_note')));
    if (p && p.manual_ignored) notes.push(h('p', { class: 'insp-note' }, icon('battery-medium', { size: 14 }), t('resources.power.manual_ignored')));
    if (!cards().length) {
      const none = p && p.none_reason ? U.waitingForText(p.none_reason, opts.system ? opts.system() : null, res) : t('resources.devices.none');
      notes.push(h('p', { class: 'insp-note' + (p && p.none_reason ? ' tone-warn' : '') }, icon(p && p.none_reason ? 'battery-low' : 'info', { size: 14 }), none));
    }
    return h('div', { class: 'dcards-top' }, notes);
  }

  function updateDevices() {
    if (devShapeOf() !== parts.devShape) {
      const old = parts.devSec;
      preserveFocus(() => old.replaceWith(devicesSection(parts.sw)));
      return;
    }
    const top = devTop();
    parts.devTop.replaceWith(top);
    parts.devTop = top;
    if (parts.sysBody) {
      const b = systemBody();
      parts.sysBody.replaceWith(b);
      parts.sysBody = b;
    }
    if (parts.pwWait) fillPowerWait();
    for (const d of cards()) {
      const ref = parts.devRefs.get(d.id);
      if (!ref) continue;
      syncDeviceSwitch(ref.sw, d);
      ref.card.classList.toggle('is-paused', !!d.paused);
      ref.card.classList.toggle('is-absent', !d.present);
      ref.name.textContent = d.label;
      ref.name.title = d.label;
      const body = deviceBody(d);
      ref.body.replaceWith(body);
      ref.body = body;
    }
    for (const d of standby()) updateStandbyRow(d);
  }

  // ---- remote GPU (SPEC-v3 V14.5; colab-obs 2026-10-27) ----

  // parts.remote: {wrap, shape, line | body + foot | top + ram + rest} —
  // the area is rebuilt when its shape changes; otherwise its blocks
  // without controls are replaced, the memory bar is updated in place (its
  // hover card survives the 2-second refresh) and the buttons stay (a click
  // must not get lost). The wrapper keeps the area's place while Colab is
  // off, so switching it on later fills the same slot.
  //
  // colab-obs (user decisions 2026-10-27 07:00 and 14:50): without a
  // session the area shows the account — tier (paid / free), the paid
  // balance and how many lectures it covers on the GPU in use, the free
  // quota, when it was read; not read: "--", "only the T4" and "重新查询".
  // With a session, the remote machine as it reports itself (name and GPU
  // memory, "来自 nvidia-smi · 申请 L4"), CPU and GPU sparklines, the
  // segmented memory bar, the GPU memory used, the lectures running there
  // (two at once on the larger GPUs), the queue with the lectures uploaded
  // ahead, compute units, the usage window as one line, the authorization.
  // A back end without the account keeps the one line of before.
  let remoteBusy = '';
  let refreshing = false;
  const cardOf = () => C.colabCard(res);
  const remoteOpen = (r) => C.ACTIVE_STATES.has(r.state) || !!r.current;
  const remoteShapeOf = () => {
    const c = cardOf();
    if (!c) return 'none';
    const r = c.st;
    const a = C.accountOf(r);
    return JSON.stringify([remoteOpen(r), r.state, !!r.shutdown_after_current, remoteBusy, !!a, C.accountNeedsCheck(a), refreshing, !!(c.usage && C.ramSegments(c.usage))]);
  };

  function remoteArea() {
    const wrap = h('div', { class: 'insp-remote-slot' });
    parts.remote = { wrap, shape: '' };
    fillRemote();
    return wrap;
  }

  const remoteHead = () => h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.remote.title')), settingsLink());

  function fillRemote() {
    const ref = parts.remote;
    ref.shape = remoteShapeOf();
    for (const k of ['line', 'body', 'foot', 'footText', 'top', 'ram', 'rest']) ref[k] = null;
    clear(ref.wrap);
    const c = cardOf();
    ref.wrap.hidden = !c;
    if (!c) return;
    const r = c.st;
    if (!remoteOpen(r)) {
      if (!C.accountOf(r)) {
        ref.line = remoteLine(r);
        ref.wrap.append(ref.line);
        return;
      }
      ref.body = idleBody(c);
      ref.foot = idleFoot(c);
      ref.wrap.append(h('section', { class: 'insp-sec insp-remote', 'aria-label': t('resources.remote.title') }, remoteHead(), ref.body, ref.foot));
      return;
    }
    ref.top = activeTop(c);
    const lay = c.usage ? C.ramSegments(c.usage) : null;
    if (lay) {
      ref.ram = ramMeter();
      ref.ram.update(lay, c);
    }
    ref.rest = activeRest(c);
    const stopping = r.state === 'stopping';
    const after = h(
      'button',
      {
        type: 'button',
        class: 'btn btn-sm',
        disabled: !!remoteBusy || stopping || !!r.shutdown_after_current,
        dataset: { fkey: 'insp:remote:after' },
        onclick: () => remoteShutdown('after_current'),
      },
      icon(remoteBusy === 'after_current' ? 'loader-circle' : 'circle-pause', { size: 13 }),
      h('span', { class: 'btn-label' }, t('resources.remote.after_current')),
    );
    const now = h(
      'button',
      { type: 'button', class: 'btn btn-sm btn-danger', disabled: !!remoteBusy || stopping, dataset: { fkey: 'insp:remote:now' }, onclick: () => remoteShutdown('now') },
      icon(remoteBusy === 'now' ? 'loader-circle' : 'ban', { size: 13 }),
      h('span', { class: 'btn-label' }, t('resources.remote.now')),
    );
    ref.wrap.append(
      h(
        'section',
        { class: 'insp-sec insp-remote', 'aria-label': t('resources.remote.title') },
        remoteHead(),
        h('div', { class: 'rc-body' }, ref.top, ref.ram ? ref.ram.el : null, ref.rest),
        stopping ? null : h('div', { class: 'rc-actions' }, after, now),
      ),
    );
  }

  const settingsLink = () => h('a', { class: 'rc-link', href: '#/settings/colab', dataset: { fkey: 'insp:remote:settings' } }, t('resources.remote.settings_link'));

  function updateRemote() {
    if (!parts || !parts.remote) return; // collapsed, or cut off (no parts)
    if (remoteShapeOf() !== parts.remote.shape) {
      preserveFocus(fillRemote);
      return;
    }
    const c = cardOf();
    if (!c) return;
    const ref = parts.remote;
    const swap = (k, el) => {
      ref[k].replaceWith(el);
      ref[k] = el;
    };
    if (ref.line) preserveFocus(fillRemote); // the line holds the settings link only
    else if (ref.body) {
      swap('body', idleBody(c));
      ref.footText.textContent = C.checkedText(C.accountOf(c.st));
    } else if (ref.top) {
      swap('top', activeTop(c));
      const lay = ref.ram && c.usage ? C.ramSegments(c.usage) : null;
      if (lay) ref.ram.update(lay, c);
      swap('rest', activeRest(c));
    }
  }

  // R76 L-5: the workbench's "立即关机" is this one too (app.js
  // ctx.colabShutdown): one confirmation, one busy state, one result; the
  // app hears of each change of the busy state (opts.onRemoteBusy) and of
  // the status the answer brought.
  async function remoteShutdown(mode) {
    if (remoteBusy) return;
    if (mode === 'now' && !(await C.confirmShutdownNow())) return;
    remoteBusy = mode;
    busyChanged();
    try {
      const v = await C.shutdownColab(mode);
      if (v && v.status && res) res = { ...res, remote: v.status };
      toast(mode === 'now' ? t('resources.remote.now_done') : t('resources.remote.after_current_done'), 'success');
    } catch (e) {
      toast(e.message, 'error');
    }
    remoteBusy = '';
    busyChanged();
  }

  function busyChanged() {
    updateRemote();
    if (opts.onRemoteBusy) opts.onRemoteBusy();
  }

  // "重新查询" (the account was not read): asks Colab now.
  async function remoteRecheck() {
    if (refreshing) return;
    refreshing = true;
    updateRemote();
    try {
      const v = await C.refreshAccount();
      if (v && v.status && res) res = { ...res, remote: v.status };
    } catch (e) {
      toast(e.message, 'error');
    }
    refreshing = false;
    updateRemote();
  }

  // The notes every remote state may carry: no authorization, a session
  // left over by an earlier run, the last error, the cool-down after a
  // used-up window, the automatic dispatch held after a shutdown button.
  // v3.2.x: the runtime held but not answering ("仍在计费" / "仍在占用本次
  // 使用窗口"; a hint only), a runtime not released and retried, and the
  // last error only when it is not older than the current session.
  // T24 S6: a runtime the program keeps trying to release (the session
  // being released, or one left from before) says it can be ended by hand
  // on the Colab website; with both at once the one from before is said to
  // be another runtime (the two sentences read nearly the same).
  function remoteNotes(r, jobs) {
    const out = [];
    if (!r.configured) out.push(h('p', { class: 'insp-note tone-warn' }, icon('key-round', { size: 14 }), t('resources.remote.auth_missing')));
    const warn = C.runtimeWarning(r, jobs);
    const byHand = t('resources.remote.manual_end');
    if (warn) out.push(h('p', { class: 'insp-note tone-warn rc-billing', role: 'status' }, icon('triangle-alert', { size: 14 }), h('span', null, r.state === 'stopping' ? U.sentenceJoin([warn, byHand]) : warn)));
    if (r.leftover) {
      const lo = r.leftover;
      const failed = lo.code === 'colab.release_failed';
      const text = failed && warn ? (lo.accelerator ? t('resources.remote.leftover_earlier', { accelerator: lo.accelerator }) : t('resources.remote.leftover_earlier_any')) : C.leftoverText(r.leftover);
      out.push(h('p', { class: 'insp-note tone-warn' }, icon('triangle-alert', { size: 14 }), failed ? U.sentenceJoin([text, byHand]) : text));
    }
    const err = C.visibleError(r);
    if (err) out.push(h('p', { class: 'insp-note tone-warn' }, icon('circle-alert', { size: 14 }), C.infoText(err)));
    if (r.cooldown_until) {
      const sec = (new Date(r.cooldown_until).getTime() - Date.now()) / 1000;
      if (sec > 0) out.push(h('p', { class: 'insp-note' }, icon('clock', { size: 14 }), t('resources.remote.cooldown', { time: C.durText(sec) })));
    }
    if (C.colabHeld() && !C.ACTIVE_STATES.has(r.state)) out.push(h('p', { class: 'insp-note' }, icon('circle-pause', { size: 14 }), t('resources.remote.held')));
    return out;
  }

  // No session and no account from the back end: one line ("远端 GPU：未使用" /
  // "远端 GPU：已释放（原因）").
  function remoteLine(r) {
    const released = r.state === 'released' && r.last_release;
    const text = released ? t('resources.remote.released_line', { reason: C.releaseName(r.last_release.reason) }) : t('resources.remote.unused');
    return h(
      'section',
      { class: 'insp-sec insp-remote is-line', 'aria-label': t('resources.remote.title') },
      h(
        'div',
        { class: 'rc-line' },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-line-t', title: released ? U.fmtDateTime(r.last_release.at) : undefined }, text),
        settingsLink(),
      ),
      remoteNotes(r),
    );
  }

  const kv = (ic, k, v, extra, cls = '') => h('div', { class: 'rc-kv' }, h('span', { class: 'rc-k' }, ic ? icon(ic, { size: 13 }) : null, k), h('span', { class: 'rc-v' + (cls ? ' ' + cls : '') }, v, extra || null));
  const rcTile = (k, v, unit, title, cls = '') => h('div', { class: 'tile rc-tile' + (cls ? ' ' + cls : ''), title: title || undefined }, h('div', { class: 'tile-k' }, k), h('div', { class: 'tile-v' }, v, unit ? h('small', null, unit) : null));

  // No session: the account (no controls; the query button is in the foot).
  function idleBody(c) {
    const r = c.st;
    const a = C.accountOf(r);
    const known = C.accountKnown(a);
    const released = r.state === 'released' && r.last_release;
    const accel = C.effectiveAccel(r);
    const fit = C.fitFor(a, accel);
    const err = C.accountErrorText(a);
    const why = err ? err.text : t('resources.remote.unknown_t4');
    const balance = C.paidBalance(a) == null ? NA : C.cuText(C.paidBalance(a));
    const sel = Array.isArray(r.selectable_accelerators) ? r.selectable_accelerators : null;
    const onlyT4 = !known || (sel && sel.length === 1 && sel[0] === 'T4');
    const tiles = [rcTile(t('resources.remote.balance_k'), balance, null, err || !known ? why : null)];
    if (fit > 0) tiles.push(rcTile(t('resources.remote.fit_k', { accelerator: accel }), U.fmtInt(fit), t('resources.remote.fit_unit', { n: U.count(fit) })));
    else tiles.push(rcTile(t('settings.colab.accel_label'), onlyT4 ? t('resources.remote.cards_t4') : sel ? sel.join(' · ') : NA, null, null, 'is-text'));
    const quota = C.freeQuotaText(a);
    const notes = [];
    if (!known) notes.push(h('p', { class: 'insp-note tone-warn' }, icon('lock', { size: 14 }), h('span', null, t('resources.remote.unknown_t4'))));
    else {
      if (a.level !== 'paid') notes.push(h('p', { class: 'insp-note' }, icon('info', { size: 14 }), h('span', null, t('resources.remote.free_t4'))));
      // the chosen GPU is locked now: kept, a T4 meanwhile (it comes back by itself)
      if (r.locked_reason) notes.push(h('p', { class: 'insp-note tone-warn' }, icon('lock', { size: 14 }), h('span', null, t('resources.remote.locked', { reason: C.lockText(r.locked_reason) }))));
    }
    if (err) notes.push(h('p', { class: 'insp-note', title: err.detail || undefined }, icon('circle-alert', { size: 14 }), h('span', null, err.text)));
    const auth = C.authLeftText(r.auth);
    return h(
      'div',
      { class: 'rc-body' },
      h(
        'div',
        { class: 'rc-head' },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-name' }, t('resources.remote.service', { accelerator: accel })),
        h('span', { class: 'rc-tags' }, h('span', { class: 'chip chip-quiet', title: released ? t('resources.remote.released_line', { reason: C.releaseName(r.last_release.reason) }) : undefined }, C.stateName(released ? 'released' : 'none'))),
      ),
      kv('layers', t('resources.remote.level_k'), C.levelText(r)),
      h('div', { class: 'tiles' }, tiles),
      quota ? h('p', { class: 'rc-quota' }, icon('coins', { size: 13 }), h('span', null, quota)) : null,
      notes,
      auth ? kv('key-round', t('resources.remote.auth_k'), auth) : null,
      remoteNotes(r),
    );
  }

  // "查询于 14:02（2 分钟前）" and, while the account is not read, "重新查询".
  function idleFoot(c) {
    const a = C.accountOf(c.st);
    const text = h('span', null, C.checkedText(a));
    parts.remote.footText = text;
    const again = !C.accountNeedsCheck(a)
      ? null
      : h(
          'button',
          { type: 'button', class: 'rc-link rc-link-btn', disabled: refreshing, dataset: { fkey: 'insp:remote:recheck' }, onclick: remoteRecheck },
          refreshing ? icon('loader-circle', { size: 12 }) : icon('refresh-cw', { size: 12 }),
          t('resources.remote.refresh'),
        );
    return h('p', { class: 'rc-checked', hidden: !again && !a.read_at }, text, again);
  }

  // With a session: the machine, its CPU and GPU (no controls).
  function activeTop(c) {
    const r = c.st;
    const m = c.machine;
    const u = c.usage;
    const st = r.state;
    const accel = C.effectiveAccel(r) || r.accelerator;
    const name = m && m.gpu_name ? [m.gpu_name, m.vram_total_mb > 0 ? U.fmtMB(m.vram_total_mb) : ''].filter(Boolean).join(' · ') : t('resources.remote.service', { accelerator: accel });
    const details = m ? [m.gpu_name, m.driver, m.cuda ? 'CUDA ' + m.cuda : '', m.compute_cap, m.vcpus > 0 ? m.vcpus + ' vCPU' : ''].filter(Boolean).join(' · ') : '';
    const out = [
      h(
        'div',
        { class: 'rc-head', title: details || undefined },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-name', title: name }, name),
        h(
          'span',
          { class: 'rc-tags' },
          h('span', { class: 'chip ' + (st === 'running' ? 'chip-ok' : st === 'stopping' ? 'chip-quiet' : 'chip-run') }, C.stateName(st)),
          h('span', { class: 'chip chip-quiet' }, C.levelText(r)),
        ),
      ),
    ];
    if (m && C.gpuMismatch(m)) out.push(h('p', { class: 'insp-note tone-warn' }, icon('triangle-alert', { size: 14 }), h('span', null, t('resources.remote.machine_mismatch', { accelerator: m.requested, name: m.gpu_name }))));
    else if (m) out.push(h('div', { class: 'rc-smi' }, t('resources.remote.machine_src', { source: m.source === 'colab_resources' ? 'Colab' : m.source || NA, accelerator: m.requested || accel })));
    if (u) {
      const tile = (k, v, hist, title) =>
        h('div', { class: 'tile', title: title || undefined }, h('div', { class: 'tile-k' }, k), h('div', { class: 'tile-v' }, num(v), v != null ? h('small', null, '%') : null), sparkline(hist, { lo: 0, hi: 100, label: k }));
      out.push(
        h(
          'div',
          { class: 'tiles' },
          tile(t('resources.system.cpu'), u.cpu_pct, u.cpu_history, m && m.vcpus > 0 ? m.vcpus + ' vCPU' : ''),
          tile(t('resources.remote.gpu_util'), u.gpu_util_pct, u.gpu_history),
        ),
      );
    }
    return h('div', { class: 'rc-part' }, out);
  }

  // The memory bar (colab-obs): segments left to right — 内存盘, Whisper,
  // 系统, 数据 (or 已用 alone when the remote reports total and free only) —
  // with the free part filling the rest, a two-column legend and a hover
  // card per segment (the others fade). Built once per area and updated in
  // place; the narrowest segment keeps 3 px (CSS), so it can be hovered.
  function ramMeter() {
    const val = h('span', { class: 'metric-v' });
    const bar = h('div', { class: 'rc-rbar', role: 'img' });
    const legend = h('ul', { class: 'rc-legend' });
    const tip = h('div', { class: 'rc-rtip', role: 'tooltip', hidden: true });
    const low = h('p', { class: 'insp-note tone-warn', hidden: true }, icon('triangle-alert', { size: 14 }), h('span', null, t('resources.remote.ram_low')));
    const el = h('div', { class: 'metric rc-ram' }, h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), t('resources.system.mem')), val), bar, legend, tip, low);
    const segs = new Map(); // kind → element
    let lay = null;
    let hover = '';
    const name = (k) => t(`resources.remote.seg.${k}`);
    function showTip() {
      const s = !hover || !lay ? null : hover === 'free' ? { kind: 'free', mb: lay.free, frac: lay.freeFrac } : lay.segs.find((x) => x.kind === hover);
      if (!s) hover = '';
      bar.classList.toggle('is-focus', !!s);
      for (const [k, e] of segs) e.classList.toggle('is-on', k === hover);
      if (!s) {
        tip.hidden = true;
        return;
      }
      clear(tip);
      tip.append(
        h('div', { class: 'rc-rtip-h' }, h('span', { class: 'rc-sw seg-' + s.kind }), h('span', null, name(s.kind)), h('span', { class: 'rc-rtip-n' }, [U.fmtMB(s.mb), U.fmtPercent(s.frac, 1)].join(' · '))),
        h('p', null, t(`resources.remote.seg_tip.${s.kind}`, { total: U.fmtMB(lay.total) })),
      );
      tip.hidden = false;
      const e = segs.get(hover);
      const w = el.clientWidth;
      const tw = tip.offsetWidth;
      const center = e ? e.offsetLeft + e.offsetWidth / 2 : w / 2;
      tip.style.left = `${Math.max(0, Math.min(w - tw, center - tw / 2))}px`;
    }
    const seg = (kind) => {
      const e = h('span', { class: 'rc-seg seg-' + kind, dataset: { seg: kind } });
      e.addEventListener('mouseenter', () => {
        hover = kind;
        showTip();
      });
      e.addEventListener('mouseleave', () => {
        if (hover !== kind) return;
        hover = '';
        showTip();
      });
      return e;
    };
    function update(next, c) {
      lay = next;
      const order = [...lay.segs.map((s) => s.kind), 'free'];
      for (const [k, e] of segs) {
        if (order.includes(k)) continue;
        e.remove();
        segs.delete(k);
      }
      let prev = null;
      for (const k of order) {
        let e = segs.get(k);
        if (!e) {
          e = seg(k);
          segs.set(k, e);
        }
        // in order, without moving an element already in place (the pointer may be on it)
        const at = prev ? prev.nextSibling : bar.firstChild;
        if (at !== e) bar.insertBefore(e, at);
        prev = e;
        const s = lay.segs.find((x) => x.kind === k);
        if (s) e.style.flexBasis = `${(s.frac * 100).toFixed(3)}%`;
      }
      // "剩余 42.2 GB / 53 GB", the free amount in bold
      const free = U.fmtMB(lay.free);
      const [before, after] = t('resources.remote.ram_free', { total: U.fmtMB(lay.total) }).split('{free}');
      clear(val);
      val.append(before || '', h('b', null, free), after || '');
      const isLow = lay.freeFrac < 0.15;
      val.classList.toggle('is-low', isLow);
      low.hidden = !(isLow && c.machine && c.machine.storage === 'memory');
      bar.setAttribute('aria-label', [t('resources.system.mem'), ...lay.segs.map((s) => name(s.kind) + ' ' + U.fmtMB(s.mb)), name('free') + ' ' + free].join(' · '));
      clear(legend);
      for (const s of lay.segs) legend.append(h('li', null, h('span', { class: 'rc-sw seg-' + s.kind }), h('span', { class: 'rc-lg-k' }, name(s.kind)), h('span', { class: 'rc-lg-v' }, U.fmtMB(s.mb))));
      if (hover) showTip();
    }
    return { el, update };
  }

  // A lecture running remotely: its title (or its step), the progress; the
  // step, the time left and the parts of speed mode below.
  // a card job's stage: the worker's own (check … write) or the step (wait, upload, download, verify)
  const CARD_STEPS = { wait: 'wait_accelerator', upload: 'upload_audio', download: 'download_result', verify: 'verify' };
  const jobStage = (s) => (!s ? '' : has(`colab.stage.${s}`) ? C.stageName(s) : C.stepName(CARD_STEPS[s] || s));
  function jobLine(j) {
    // the worker's stage only while it prepares (check, install, model…)
    const stage = j.step ? [C.stepName(j.step), j.step === 'remote_transcribe' && j.stage && j.stage !== 'transcribe' ? C.stageName(j.stage) : ''].filter(Boolean).join(' · ') : jobStage(j.stage);
    const title = j.title || stage;
    // v3.2.x: the runtime does not answer and the lecture retries in place —
    // "连接不稳定，正在重试（2/3）" and the time to the next try replace the
    // progress and the time left (both frozen meanwhile)
    const rc = C.reconnectText(j.reconnect);
    if (rc) {
      const sub = [j.title ? stage : '', rc.next].filter(Boolean).join(' · ');
      return [
        h('span', { class: 'rc-task' }, h('span', { class: 'rc-task-t', title }, title)),
        h('span', { class: 'rc-sub rc-reconnect tone-warn', role: 'status' }, icon('refresh-cw', { size: 12 }), h('span', null, rc.text)),
        sub ? h('span', { class: 'rc-sub' }, sub) : null,
      ];
    }
    const sub = [j.title ? stage : '', j.eta > 0 ? t('resources.remote.eta', { time: C.durText(j.eta) }) : '', j.split > 1 ? t('resources.remote.job_split', { n: j.split }) : ''].filter(Boolean).join(' · ');
    const p = j.step !== 'wait_accelerator' && j.progress > 0 ? U.pct(j.progress) : '';
    return [h('span', { class: 'rc-task' }, h('span', { class: 'rc-task-t', title }, title), p ? h('span', { class: 'rc-task-p' }, p) : null), sub ? h('span', { class: 'rc-sub' }, sub) : null];
  }

  // Compute units of the session: spent and the paid balance, the
  // account's rate (all its runtimes); the free tier its free quota.
  function cuRow(r, a) {
    if (a ? a.level !== 'paid' : r.tier === 'free') return kv('coins', t('resources.remote.balance_k'), C.freeQuotaText(a) || t('resources.remote.balance_free'));
    const bal = a && typeof a.paid_balance_cu === 'number' ? a.paid_balance_cu : r.balance_cu;
    const texts = [];
    if (r.session_spent_cu > 0) texts.push(t('resources.remote.spent', { cost: C.cuText(r.session_spent_cu) }));
    texts.push(t('resources.remote.balance', { balance: bal != null ? C.cuText(bal) : NA }));
    const sub = a && a.account_rate_cu_per_hour > 0 ? t('resources.remote.rate', { rate: U.fmtDecimal(a.account_rate_cu_per_hour, 2) }) : r.plan_cost_cu > 0 ? t('resources.remote.plan_cost', { cost: C.cuText(r.plan_cost_cu) }) : '';
    return kv('coins', t('resources.remote.balance_k'), texts.join(' · '), sub ? h('span', { class: 'rc-sub' }, sub) : null);
  }

  // With a session: GPU memory, the lectures, the queue, compute units, the
  // usage window, the authorization and the notes (no controls).
  function activeRest(c) {
    const r = c.st;
    const u = c.usage;
    const a = C.accountOf(r);
    const st = r.state;
    const rows = [];
    const line = (k, v, title) => h('div', { class: 'metric-top rc-line-v', title: title || undefined }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), k), h('span', { class: 'metric-v' }, v));
    if (u && u.vram_used_mb != null) rows.push(line(t('resources.remote.vram_used'), U.fmtMB(u.vram_used_mb), u.vram_whisper_mb != null ? t('resources.remote.seg.whisper') + ' ' + U.fmtMB(u.vram_whisper_mb) : ''));
    if (u && u.data_disk_mb != null) rows.push(line(t('resources.remote.data_disk'), U.fmtMB(u.data_disk_mb)));
    if (u) rows.push(h('div', { class: 'rc-sep' }));
    // stopping: "正在释放运行时…" in place of the lectures (handed back, so
    // their old steps and progress are not shown)
    if (st === 'stopping') rows.push(h('p', { class: 'insp-note rc-releasing', role: 'status' }, icon('loader-circle', { size: 14, class: 'spin' }), h('span', null, t('resources.remote.releasing_note'))));
    else if (c.jobs.length) rows.push(kv('audio-lines', t('resources.remote.jobs_k'), c.jobs.map(jobLine)));
    else if (st === 'running') rows.push(kv('audio-lines', t('resources.remote.jobs_k'), t('resources.remote.idle')));
    if (c.queued > 0 || c.ahead > 0) {
      const q = [[U.fmtInt(c.queued), t('resources.remote.fit_unit', { n: U.count(c.queued) })].join(' ')];
      if (c.ahead > 0) q.push(t('resources.remote.queue_ahead', { n: U.count(c.ahead) }));
      // T24 G11: none fits is said plainly ("约 0 节能在本次处理" read
      // badly), and where the others go
      const fit = r.planned_fit > 0 ? t('resources.remote.plan', { n: r.planned, fit: r.planned_fit }) : t('resources.remote.plan_none', { n: r.planned });
      const plan = r.planned > 0 && r.planned_fit < r.planned ? h('span', { class: 'rc-sub' }, U.sentenceJoin([fit, t('resources.remote.plan_rest')])) : null;
      rows.push(kv('list-checks', t('resources.remote.queue_k'), q.join(' · '), plan));
    }
    rows.push(cuRow(r, a));
    const used = r.window_used_sec || 0;
    const total = r.window_total_sec || 0;
    const late = total > 0 && used / total > 0.9;
    const left = st === 'stopping' ? null : h('span', { class: 'rc-sub' }, r.lectures_left > 0 ? t('resources.remote.left', { n: r.lectures_left }) : t('resources.remote.left_none'));
    rows.push(kv('clock', t('resources.remote.window_k'), t('resources.remote.window', { used: C.durText(used) || C.durText(0), total: C.durText(total) }), left, late ? 'tone-warn' : ''));
    const auth = C.authLeftText(r.auth);
    if (auth) rows.push(kv('key-round', t('resources.remote.auth_k'), auth));
    if (r.shutdown_after_current && st !== 'stopping') rows.push(h('p', { class: 'insp-note' }, icon('circle-pause', { size: 14 }), t('resources.remote.after_current_set')));
    rows.push(...remoteNotes(r, c.jobs));
    return h('div', { class: 'rc-part' }, rows);
  }

  // ---- SSH servers (ssh-1b) ----

  // parts.ssh: {wrap, shape, refs: Map(id → {top, ram, rest})} — rebuilt
  // when the servers or their connected / not connected shape change;
  // otherwise each card's blocks (no controls) are replaced and its memory
  // bar updated in place (its hover card survives the 2-second refresh).
  const sshShapeOf = () => JSON.stringify(R.sshCards(res).map((c) => [c.id, R.cardConnected(c), !!(c.usage && C.ramSegments(c.usage))]));

  function sshArea() {
    const wrap = h('div', { class: 'insp-ssh-slot' });
    parts.ssh = { wrap, shape: '', refs: new Map() };
    fillSSH();
    return wrap;
  }

  function fillSSH() {
    const ref = parts.ssh;
    ref.shape = sshShapeOf();
    ref.refs = new Map();
    clear(ref.wrap);
    const cards = R.sshCards(res);
    ref.wrap.hidden = !cards.length;
    if (!cards.length) return;
    const items = cards.map((c) => {
      if (!R.cardConnected(c)) return sshLine(c);
      const r = { top: sshTop(c), ram: null, rest: sshRest(c) };
      const lay = c.usage ? C.ramSegments(c.usage) : null;
      if (lay) {
        r.ram = ramMeter();
        r.ram.update(lay, c);
      }
      ref.refs.set(c.id, r);
      return h('div', { class: 'rc-body ssh-card', dataset: { server: c.id } }, r.top, r.ram ? r.ram.el : null, r.rest);
    });
    ref.wrap.append(
      h(
        'section',
        { class: 'insp-sec insp-remote insp-ssh', 'aria-label': t('settings.remote.title') },
        h('div', { class: 'insp-sec-head' }, h('h3', null, t('settings.remote.title')), h('a', { class: 'rc-link', href: '#/settings/remote', dataset: { fkey: 'insp:ssh:settings' } }, t('system.open_settings'))),
        items,
      ),
    );
  }

  function updateSSH() {
    if (!parts.ssh) return;
    if (sshShapeOf() !== parts.ssh.shape) {
      preserveFocus(fillSSH);
      return;
    }
    for (const c of R.sshCards(res)) {
      const r = parts.ssh.refs.get(c.id);
      if (!r) {
        // a one-line card: its text may change (no controls in it)
        const old = parts.ssh.wrap.querySelector(`[data-server="${CSS.escape(c.id)}"]`);
        if (old) old.replaceWith(sshLine(c));
        continue;
      }
      const top = sshTop(c);
      r.top.replaceWith(top);
      r.top = top;
      const lay = r.ram && c.usage ? C.ramSegments(c.usage) : null;
      if (lay) r.ram.update(lay, c);
      const rest = sshRest(c);
      r.rest.replaceWith(rest);
      r.rest = rest;
    }
  }

  // A server's last error in words (its name as the device).
  const sshError = (c) => (c.last_error && c.last_error.code ? U.errorInfoText({ code: c.last_error.code, params: { device: c.label, ...(c.last_error.params || {}) } }) : '');

  // Not connected: the name and the state on one line (the last error as its hover text).
  function sshLine(c) {
    const err = sshError(c);
    return h('div', { class: 'rc-line ssh-line', dataset: { server: c.id }, title: err || undefined }, icon('server', { size: 15 }), h('span', { class: 'rc-line-t' }, c.label), R.stateChip(c.state));
  }

  // Connected: name, state, latency; the machine as it reports itself
  // (GPU and its memory; driver, CUDA, vCPUs as the hover text), the agent's
  // version and where the job files live; the CPU and GPU sparklines.
  function sshTop(c) {
    const m = c.machine;
    const u = c.usage;
    const x = c.ssh || {};
    const gpu = m && m.gpu_name ? [m.gpu_name, m.vram_total_mb > 0 ? U.fmtMB(m.vram_total_mb) : ''].filter(Boolean).join(' · ') : '';
    const details = m ? [m.gpu_name, m.driver, m.cuda ? 'CUDA ' + m.cuda : '', m.vcpus > 0 ? m.vcpus + ' vCPU' : ''].filter(Boolean).join(' · ') : '';
    const storage = x.storage === 'memory' ? t('resources.remote.seg.ramdisk') : x.storage ? t('system.disk.title') : '';
    const out = [
      h(
        'div',
        { class: 'rc-head', title: details || undefined },
        icon('server', { size: 15 }),
        h('span', { class: 'rc-name', title: c.label }, c.label),
        h('span', { class: 'rc-tags' }, R.stateChip(c.state), x.latency_ms > 0 ? h('span', { class: 'chip chip-quiet' }, t('resources.ssh.latency', { ms: U.fmtInt(x.latency_ms) })) : null),
      ),
      h('div', { class: 'rc-smi' }, [gpu, x.agent_version ? t('settings.remote.agent', { version: x.agent_version }) : '', storage].filter(Boolean).join(' · ')),
    ];
    if (u) {
      const tile = (k, v, hist, title) =>
        h('div', { class: 'tile', title: title || undefined }, h('div', { class: 'tile-k' }, k), h('div', { class: 'tile-v' }, num(v), v != null ? h('small', null, '%') : null), sparkline(hist, { lo: 0, hi: 100, label: k }));
      out.push(h('div', { class: 'tiles' }, tile(t('resources.system.cpu'), u.cpu_pct, u.cpu_history, m && m.vcpus > 0 ? m.vcpus + ' vCPU' : ''), tile(t('resources.remote.gpu_util'), u.gpu_util_pct, u.gpu_history)));
    }
    return h('div', { class: 'rc-part' }, out);
  }

  // The GPU memory used, the job files on disk, the lectures running there, the last error.
  function sshRest(c) {
    const u = c.usage;
    const rows = [];
    const line = (k, v, title) => h('div', { class: 'metric-top rc-line-v', title: title || undefined }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), k), h('span', { class: 'metric-v' }, v));
    if (u && u.vram_used_mb != null) rows.push(line(t('resources.remote.vram_used'), U.fmtMB(u.vram_used_mb), u.vram_whisper_mb != null ? t('resources.remote.seg.whisper') + ' ' + U.fmtMB(u.vram_whisper_mb) : ''));
    if (u && u.data_disk_mb != null && u.data_disk_mb >= 0) rows.push(line(t('resources.remote.data_disk'), U.fmtMB(u.data_disk_mb)));
    const jobs = (c.jobs || []).map((j) => {
      const step = R.jobStepText(j.stage);
      const title = j.lecture_title || step;
      const p = j.progress > 0 ? U.pct(j.progress) : '';
      const sub = [j.lecture_title ? step : '', j.eta_sec > 0 ? t('resources.remote.eta', { time: C.durText(j.eta_sec) }) : ''].filter(Boolean).join(' · ');
      return [h('span', { class: 'rc-task' }, h('span', { class: 'rc-task-t', title }, title), p ? h('span', { class: 'rc-task-p' }, p) : null), sub ? h('span', { class: 'rc-sub' }, sub) : null];
    });
    if (jobs.length) rows.push(kv('audio-lines', t('resources.remote.jobs_k'), jobs));
    else if (c.state === 'ready') rows.push(kv('audio-lines', t('resources.remote.jobs_k'), t('resources.remote.idle')));
    const err = sshError(c);
    if (err) rows.push(h('p', { class: 'insp-note tone-warn' }, icon('circle-alert', { size: 14 }), h('span', null, err)));
    return h('div', { class: 'rc-part' }, rows);
  }

  // ---- upper area: system and power ----

  function systemSection() {
    parts.sysBody = systemBody();
    parts.pwWait = { el: h('div', { class: 'pw-waits' }), sig: null };
    fillPowerWait();
    return h('section', { class: 'insp-sec', 'aria-label': t('resources.system.title') }, h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.system.title'))), parts.pwWait.el, parts.sysBody);
  }

  // The tasks the power-aware policy suspended (T24 S4): how many and why,
  // that they go on by themselves once plugged in, and a link to the
  // transcription devices of the settings (another device takes them
  // without waiting). It holds a link, so it is redrawn only when its
  // numbers change, not with every snapshot (a click must not get lost).
  function fillPowerWait() {
    const ref = parts.pwWait;
    const p = policy();
    const ac = p && p.wait_ac > 0 ? p.wait_ac : 0;
    const none = p && p.wait_no_device > 0 ? p.wait_no_device : 0;
    const sig = JSON.stringify([ac, none]);
    if (sig === ref.sig) return;
    ref.sig = sig;
    preserveFocus(() => {
      clear(ref.el);
      if (ac) ref.el.append(h('p', { class: 'insp-note pw-wait tone-warn' }, icon('plug', { size: 14 }), t('resources.power.wait_ac', { n: ac })));
      if (none) ref.el.append(h('p', { class: 'insp-note pw-wait tone-warn' }, icon('battery-low', { size: 14 }), t('resources.power.wait_no_device', { n: none })));
      if (ac || none) {
        ref.el.append(
          h(
            'p',
            { class: 'insp-note pw-way' },
            icon('info', { size: 14 }),
            h('span', null, t('resources.power.wait_way_out'), ' ', h('a', { class: 'rc-link', href: '#/settings/asr', dataset: { fkey: 'insp:power:settings' } }, t('resources.power.to_settings'))),
          ),
        );
      }
    });
  }

  // Everything of the system section below its head (no interactive elements).
  function systemBody() {
    const y = res.system || {};
    const memFrac = y.mem_used_mb != null && y.mem_total_mb ? y.mem_used_mb / y.mem_total_mb : null;
    const tile = (k, v, unit, spark) =>
      h('div', { class: 'tile' }, h('div', { class: 'tile-k' }, k), h('div', { class: 'tile-v' }, num(v), v != null ? h('small', null, unit) : null), spark || null);
    // readings this system cannot give at all are left out (mac-opti M6)
    const unsupported = new Set(Array.isArray(y.unsupported) ? y.unsupported : []);
    const memKey = y.unified_memory ? 'resources.system.unified_mem' : 'resources.system.mem';
    const tiles = [
      unsupported.has('cpu_pct') ? null : tile(t('resources.system.cpu'), y.cpu_pct, '%', sparkline(cpuHist, { lo: 0, hi: 100, label: t('resources.system.cpu') })),
      unsupported.has('cpu_temp') ? null : tile(t('resources.system.cpu_temp'), y.cpu_temp_c, '°C', sparkline(tempHist, { label: t('resources.system.cpu_temp') })),
    ].filter(Boolean);
    return h(
      'div',
      { class: 'sys-body' },
      powerLine(res.power),
      bgStreamLine(),
      tiles.length ? h('div', { class: 'tiles' }, tiles) : null,
      unsupported.has('mem')
        ? null
        : h(
            'div',
            { class: 'metric' },
            h(
              'div',
              { class: 'metric-top' },
              h('span', { class: 'metric-k', title: y.unified_memory ? t('resources.system.unified_mem_tip') : undefined }, icon('memory-stick', { size: 14 }), t(memKey)),
              h('span', { class: 'metric-v' }, `${y.mem_used_mb == null ? NA : U.fmtMB(y.mem_used_mb)} / ${y.mem_total_mb == null ? NA : U.fmtMB(y.mem_total_mb)}`),
            ),
            meter(memFrac, memFrac != null && memFrac > 0.92 ? 'meter-warn' : 'meter-accent'),
          ),
    );
  }

  // The playback files made in the background (stream-parallel): one → its
  // progress, several → how many.
  function bgStreamLine() {
    if (!bgStreams.size) return null;
    const [p] = bgStreams.values();
    const text = bgStreams.size === 1 ? t('resources.system.stream_bg_one', { pct: U.pct(p) }) : t('resources.system.stream_bg_many', { n: bgStreams.size });
    return h('p', { class: 'insp-note', title: t('resources.system.stream_bg_tip') }, icon('play', { size: 14 }), text);
  }

  // The power state the power-aware choice follows: source (plugged in /
  // battery; USB-C PD and a power adapter cannot be told apart, power-brief
  // ruling 16), charge and whether it charges.
  function powerLine(pw) {
    if (!pw) return null;
    const known = !!(pw.known && pw.source);
    const onBattery = pw.source === 'battery';
    const level = pw.battery_pct;
    let ic = 'circle-alert';
    if (known && onBattery) ic = level == null ? 'battery-medium' : level < 25 ? 'battery-low' : level < 75 ? 'battery-medium' : 'battery-full';
    else if (known) ic = pw.charging ? 'battery-charging' : 'plug';
    const tags = [];
    if (known && pw.has_battery && level != null) tags.push(h('span', { class: 'pw-level' }, t('resources.power.level', { pct: U.fmtPercent(level / 100, 0) })));
    if (known && pw.has_battery && !onBattery) tags.push(h('span', { class: 'chip ' + (pw.charging ? 'chip-ok' : 'chip-quiet') }, pw.charging ? t('resources.power.charging') : t('resources.power.not_charging')));
    if (known && !pw.has_battery) tags.push(h('span', { class: 'chip chip-quiet' }, t('resources.power.no_battery')));
    return h(
      'div',
      { class: 'pw-line' + (onBattery ? ' is-battery' : ''), title: [t('resources.power.basis_tip'), onBatteryPolicy() ? t('resources.devices.battery_mode_tip') : ''].filter(Boolean).join('\n') },
      icon(ic, { size: 16 }),
      h('span', { class: 'pw-src' }, !known ? t('system.power.unknown') : onBattery ? t('resources.power.battery') : t('resources.power.ac')),
      tags.length ? h('span', { class: 'pw-tags' }, tags) : null,
    );
  }

  // ---- middle area: the active device cards ----

  function deviceSwitch(d) {
    return h(
      'button',
      {
        type: 'button',
        class: 'switch switch-sm',
        role: 'switch',
        'aria-label': t('resources.devices.device_switch', { device: d.label }),
        dataset: { fkey: `insp:dev:${d.id}` },
        onclick: () => opts.onToggleDevice && opts.onToggleDevice(rows().find((x) => x.id === d.id) || d),
      },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'switch-label' }),
    );
  }

  function syncDeviceSwitch(sw, d) {
    const on = !d.paused;
    sw.classList.toggle('is-on', on);
    sw.setAttribute('aria-checked', String(on));
    sw.title = on ? t('devices.pause_tip') : t('devices.resume_tip');
    sw.querySelector('.switch-label').textContent = on ? t('common.status.running') : t('devices.paused');
  }

  function deviceCard(d) {
    const sw = deviceSwitch(d);
    syncDeviceSwitch(sw, d);
    const name = h('span', { class: 'dcard-name', title: d.label }, d.label);
    const body = deviceBody(d);
    const card = h('div', { class: 'dcard' + (d.paused ? ' is-paused' : '') + (d.present ? '' : ' is-absent'), dataset: { dev: d.id } }, h('div', { class: 'dcard-head' }, icon('cpu', { size: 15 }), name, sw), body);
    parts.devRefs.set(d.id, { card, sw, name, body });
    return card;
  }

  // A machine with a battery runs on battery: the power-aware battery rules
  // apply (the NPU, else the Intel GPU; never the graphics card or the CPU).
  const onBatteryPolicy = () => {
    const p = policy();
    return !!(p && p.battery_machine && res.power && res.power.source === 'battery');
  };

  // The hover text of the battery rules, with the charge.
  function batteryTip() {
    const p = res.power;
    const rules = policy() ? onBatteryPolicy() : !!(p && p.battery_aware); // 3b: battery awareness
    return [rules ? t('resources.devices.battery_mode_tip') : '', p && p.battery_pct != null ? t('system.power.battery_pct', { pct: U.fmtPercent(p.battery_pct / 100, 0) }) : ''].filter(Boolean).join('\n');
  }

  // "为什么是它" (power-req part 2): why the active devices are active. A
  // running device that is no longer the preferred one (plugged in while
  // the NPU runs a lecture) says that it hands over after the lecture
  // instead of the plugged-in reason.
  const REASON_ICONS = { ac_auto: 'plug', battery: 'battery-medium' };
  function reasonChip(p, d) {
    if (!p) return null;
    if (p.next && d.id !== p.next) {
      return h('span', { class: 'chip chip-run dcard-why', title: t('resources.power.next', { device: U.deviceLabel(p.next, null, res) }) }, icon('arrow-right', { size: 12 }), t('resources.power.reason.switch_pending'));
    }
    if (!p.reason) return null;
    const tip = p.reason === 'battery' ? batteryTip() : '';
    return h(
      'span',
      { class: 'chip chip-run dcard-why', title: tip || undefined },
      REASON_ICONS[p.reason] ? icon(REASON_ICONS[p.reason], { size: 12 }) : null,
      has(`resources.power.reason.${p.reason}`) ? t(`resources.power.reason.${p.reason}`) : p.reason,
    );
  }

  // Everything of a device card below its head (no interactive elements).
  function deviceBody(d) {
    const pol = policy();
    const backend = U.backendName(d); // CUDA on an NVIDIA card, like the standby rows
    const tasks = d.tasks || [];
    const combo = [backend, U.comboText(d.model, d.precision)].filter(Boolean).join(' · ');
    // the switch of power management: the running device finishes its
    // lecture, the next one starts on pol.next
    const next = pol && pol.next && pol.next !== d.id && d.active && tasks.some((tk) => !tk.selftest);
    return h(
      'div',
      { class: 'dcard-body' },
      h('div', { class: 'dcard-combo mono', title: combo }, combo),
      h(
        'div',
        { class: 'gpu-meta' },
        h(
          'span',
          { class: 'dcard-tags' },
          h('span', { class: 'dcard-kind' }, U.deviceKindName(d.kind)),
          pol && d.active ? reasonChip(pol, d) : null,
          d.paused ? h('span', { class: 'chip chip-warn' }, t('devices.paused')) : null,
          !pol && d.battery_mode ? h('span', { class: 'chip chip-warn', title: batteryTip() || undefined }, icon('battery-medium', { size: 12 }), t('resources.devices.battery_mode')) : null,
        ),
        h('span', { class: 'gpu-slots', title: t('resources.gpu.slots_tip', { used: d.slots_used, max: d.slots_max }) }, h('span', { class: 'num-strong' }, `${d.slots_used}/${d.slots_max}`), slots(d.slots_used, d.slots_max, t('resources.gpu.slots_label'))),
      ),
      next ? h('p', { class: 'insp-note dcard-next' }, icon('arrow-right', { size: 14 }), t('resources.power.next', { device: U.deviceLabel(pol.next, null, res) })) : null,
      !d.present ? h('p', { class: 'insp-note tone-warn', title: t('devices.not_present_tip') }, icon('triangle-alert', { size: 14 }), t('devices.not_present')) : null,
      d.battery_hold ? h('p', { class: 'insp-note tone-warn' }, icon('battery-medium', { size: 14 }), t('devices.battery_hold')) : null,
      d.holds_suspended ? h('p', { class: 'insp-note tone-warn' }, icon('plug', { size: 14 }), t('waiting.power_wait_ac')) : null,
      d.kind === 'cuda' ? cudaMetrics(res.gpu) : usageTile(d),
      d.kind !== 'cuda' && (d.mem_used_mb != null || d.mem_total_mb != null) ? sharedMem(d) : null,
      h(
        'ul',
        { class: 'dcard-tasks' },
        tasks.length
          ? tasks.map((tk) => {
              const title = tk.selftest ? t('selftest.title') : tk.lecture_title || tk.job_id;
              return h('li', { class: 'dcard-task' }, icon(tk.selftest ? 'gauge' : 'audio-lines', { size: 13 }), h('span', { class: 'dcard-task-t', title }, title), h('span', { class: 'dcard-task-p' }, U.pct(tk.progress)));
            })
          : h('li', { class: 'dcard-task is-idle' }, t('resources.devices.idle')),
      ),
    );
  }

  // An Intel device's shared GPU memory (power management: "显存或内存");
  // the Apple GPU's share of the unified memory (mac-opti M6).
  function sharedMem(d) {
    const frac = d.mem_used_mb != null && d.mem_total_mb ? d.mem_used_mb / d.mem_total_mb : null;
    const label = d.kind === 'apple' ? t('resources.devices.apple_mem') : t('resources.system.mem');
    return h(
      'div',
      { class: 'metric' },
      h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), label), h('span', { class: 'metric-v' }, `${d.mem_used_mb == null ? NA : U.fmtMB(d.mem_used_mb)} / ${d.mem_total_mb == null ? NA : U.fmtMB(d.mem_total_mb)}`)),
      meter(frac, 'meter-accent'),
    );
  }

  // ---- bottom area: the other devices, one row each ----

  // Rows opened by the user (kept across updates; opening one never
  // changes the active device).
  const opened = new Set();

  function standbySection() {
    return h(
      'section',
      { class: 'insp-sec insp-standby', 'aria-label': t('resources.standby.title') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.standby.title'))),
      h('ul', { class: 'sb-list' }, standby().map(standbyRow)),
    );
  }

  // "名称 · 后端 · 待命 / 已停用 / 未测试 / 有任务挂起" (power-req part 2):
  // the state on the right of the name; the kind and back end below it,
  // followed by the small "not tested" and "suspended task" tags.
  function standbyState(d) {
    if (!d.enabled) return h('span', { class: 'chip chip-quiet sb-state' }, t('devices.disabled'));
    if (!d.present) return h('span', { class: 'chip chip-warn sb-state', title: t('devices.not_present_tip') }, t('devices.not_present'));
    if (d.paused) return h('span', { class: 'chip chip-warn sb-state' }, t('devices.paused'));
    return h('span', { class: 'chip chip-quiet sb-state' }, t('resources.standby.standby'));
  }

  function standbyMeta(d) {
    return h(
      'span',
      { class: 'sb-meta' },
      h('span', { class: 'sb-meta-t' }, [U.deviceKindName(d.kind), U.backendName(d)].filter(Boolean).join(' · ')),
      d.holds_suspended ? h('span', { class: 'chip chip-warn', title: t('waiting.power_wait_ac') }, t('resources.standby.holds_suspended')) : null,
      d.untested ? h('span', { class: 'chip chip-draft' }, t('resources.standby.untested')) : null,
    );
  }

  function standbyRow(d) {
    const open = opened.has(d.id);
    const name = h('span', { class: 'sb-name', title: d.label }, d.label);
    const meta = standbyMeta(d);
    const tags = standbyState(d);
    const btnEl = h(
      'button',
      {
        type: 'button',
        class: 'sb-row',
        'aria-expanded': String(open),
        dataset: { fkey: `insp:sb:${d.id}` },
        onclick: () => toggleStandby(d.id),
      },
      icon('chevron-right', { size: 14, class: 'sb-chev' }),
      h('span', { class: 'sb-main' }, name, meta),
      tags,
    );
    const ref = { name, meta, tags, btn: btnEl, li: null, sw: null, body: null };
    const li = h('li', { class: 'sb-item' + (open ? ' is-open' : '') + (d.enabled ? '' : ' is-off'), dataset: { dev: d.id } }, btnEl);
    if (open) {
      ref.body = deviceBody(d);
      const detail = h('div', { class: 'sb-detail' });
      if (d.enabled) {
        ref.sw = deviceSwitch(d);
        syncDeviceSwitch(ref.sw, d);
        detail.append(h('div', { class: 'sb-detail-head' }, ref.sw));
      }
      detail.append(ref.body);
      li.append(detail);
    }
    ref.li = li;
    parts.sbRefs.set(d.id, ref);
    return li;
  }

  function updateStandbyRow(d) {
    const ref = parts.sbRefs.get(d.id);
    if (!ref) return;
    if (!!ref.sw !== (opened.has(d.id) && d.enabled)) {
      preserveFocus(() => ref.li.replaceWith(standbyRow(d)));
      return;
    }
    ref.li.classList.toggle('is-off', !d.enabled);
    ref.name.textContent = d.label;
    ref.name.title = d.label;
    const meta = standbyMeta(d);
    ref.meta.replaceWith(meta);
    ref.meta = meta;
    const tags = standbyState(d);
    ref.tags.replaceWith(tags);
    ref.tags = tags;
    if (ref.sw) syncDeviceSwitch(ref.sw, d);
    if (ref.body) {
      const body = deviceBody(d);
      ref.body.replaceWith(body);
      ref.body = body;
    }
  }

  function toggleStandby(id) {
    if (opened.has(id)) opened.delete(id);
    else opened.add(id);
    const ref = parts && parts.sbRefs && parts.sbRefs.get(id);
    const d = standby().find((x) => x.id === id);
    if (!ref || !d) return;
    preserveFocus(() => ref.li.replaceWith(standbyRow(d)));
  }

  // An Intel device's usage — the system counter's reading, or "--" (I10) —
  // and its temperature (power-req part 2 "占用率与温度的小折线图"): no
  // counter reports it without administrator rights, so "--" unless the
  // back end sends temp_c / temp_history.
  function usageTile(d) {
    const na = d.util_pct == null;
    const temp = typeof d.temp_c === 'number' ? d.temp_c : null;
    return h(
      'div',
      { class: 'tiles' },
      h(
        'div',
        { class: 'tile', title: na ? t('resources.devices.util_na') : undefined },
        h('div', { class: 'tile-k' }, t('resources.gpu.util')),
        h('div', { class: 'tile-v' }, num(d.util_pct), !na ? h('small', null, '%') : null),
        sparkline(d.util_history, { lo: 0, hi: 100, label: t('resources.gpu.util_history') }),
      ),
      // the Apple GPU has no readable temperature (mac-opti M6): no tile
      d.kind === 'apple'
        ? null
        : h(
            'div',
            { class: 'tile', title: temp == null ? t('resources.devices.temp_na') : undefined },
            h('div', { class: 'tile-k' }, t('resources.gpu.temp')),
            h('div', { class: 'tile-v' }, num(temp), temp != null ? h('small', null, '°C') : null),
            sparkline(Array.isArray(d.temp_history) ? d.temp_history : [], { label: t('resources.gpu.temp_history') }),
          ),
    );
  }

  // The CUDA card keeps the nvidia-smi metrics of before 3b.
  function cudaMetrics(g) {
    if (!g || !g.available) return h('p', { class: 'insp-note', title: (g && g.error) || '' }, icon('circle-alert', { size: 14 }), g && g.error ? U.excerpt(g.error, 90) : t('resources.gpu.unavailable'));
    return [...gpuBody(g).querySelectorAll(':scope > .tiles, :scope > .metric')];
  }

  // the sampling time, 24-hour with seconds (util.js)
  function timeOf(iso) {
    return U.fmtHMS(iso) || NA;
  }

  function gpuSwitch() {
    return h(
      'button',
      { type: 'button', class: 'switch', role: 'switch', 'aria-label': t('resources.gpu.switch'), dataset: { fkey: 'insp:gpu' }, onclick: opts.onToggleGPU },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'switch-label' }),
    );
  }

  function syncSwitch(sw, g) {
    const on = !g.paused;
    sw.classList.toggle('is-on', on);
    sw.setAttribute('aria-checked', String(on));
    if (devMode()) {
      // SPEC-v2 I8: the main switch pauses every transcription device;
      // T24 G13: on while the power-aware policy holds every task (nothing
      // runs), it says "已挂起", not "运行中"
      const held = on && suspendedCount() > 0 && !rows().some((d) => (d.tasks || []).some((tk) => !tk.selftest));
      sw.setAttribute('aria-label', t('resources.devices.switch'));
      sw.title = on ? t('resources.devices.pause_tip') : t('resources.devices.resume_tip');
      sw.querySelector('.switch-label').textContent = held ? t('resources.devices.suspended_label') : on ? t('common.status.running') : t('devices.paused');
      return;
    }
    sw.setAttribute('aria-label', t('resources.gpu.switch'));
    sw.title = on ? t('resources.gpu.pause_tip') : t('resources.gpu.resume_tip');
    sw.querySelector('.switch-label').textContent = on ? t('resources.gpu.running') : t('resources.gpu.paused');
  }

  // Everything of the GPU section below its head (no interactive elements).
  function gpuBody(g) {
    const warn = !!g.temp_warn;
    const memFrac = g.mem_used_mb != null && g.mem_total_mb ? g.mem_used_mb / g.mem_total_mb : null;
    const powFrac = g.power_w != null && g.power_limit_w ? g.power_w / g.power_limit_w : null;
    return h(
      'div',
      { class: 'gpu-body' },
      h('div', { class: 'gpu-name', title: g.name || g.error || '' }, icon('cpu', { size: 15 }), h('span', null, g.available && g.name ? g.name : t('resources.gpu.unavailable'))),
      h(
        'div',
        { class: 'gpu-meta' },
        h('span', { class: 'chip chip-quiet mono' }, g.backend || NA),
        h('span', { class: 'gpu-slots', title: t('resources.gpu.slots_tip', { used: g.asr_slots_used, max: g.asr_slots_max }) }, h('span', { class: 'dim' }, 'ASR'), h('span', { class: 'num-strong' }, `${g.asr_slots_used}/${g.asr_slots_max}`), slots(g.asr_slots_used, g.asr_slots_max, t('resources.gpu.slots_label'))),
      ),
      g.paused ? h('p', { class: 'insp-note tone-warn' }, icon('circle-pause', { size: 14 }), t('resources.gpu.paused_note')) : null,
      !g.available && g.error ? h('p', { class: 'insp-note', title: g.error }, icon('circle-alert', { size: 14 }), U.excerpt(g.error, 90)) : null,
      h(
        'div',
        { class: 'tiles' },
        h(
          'div',
          { class: 'tile' },
          h('div', { class: 'tile-k' }, t('resources.gpu.util')),
          h('div', { class: 'tile-v' }, num(g.util_pct), g.util_pct != null ? h('small', null, '%') : null),
          sparkline(g.util_history, { lo: 0, hi: 100, label: t('resources.gpu.util_history') }),
        ),
        h(
          'div',
          { class: 'tile' + (warn ? ' is-warn' : '') },
          h('div', { class: 'tile-k' }, t('resources.gpu.temp'), warn ? h('span', { class: 'chip chip-warn' }, t('resources.gpu.hot')) : null),
          h('div', { class: 'tile-v' }, num(g.temp_c), g.temp_c != null ? h('small', null, '°C') : null),
          sparkline(g.temp_history, { label: t('resources.gpu.temp_history') }),
        ),
      ),
      h(
        'div',
        { class: 'metric' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('memory-stick', { size: 14 }), t('resources.gpu.vram')), h('span', { class: 'metric-v' }, `${g.mem_used_mb == null ? NA : U.fmtMB(g.mem_used_mb)} / ${g.mem_total_mb == null ? NA : U.fmtMB(g.mem_total_mb)}`)),
        meter(memFrac, memFrac != null && memFrac > 0.92 ? 'meter-warn' : 'meter-accent'),
      ),
      h(
        'div',
        { class: 'metric' },
        h('div', { class: 'metric-top' }, h('span', { class: 'metric-k' }, icon('zap', { size: 14 }), t('resources.gpu.power')), h('span', { class: 'metric-v' }, `${g.power_w == null ? NA : Math.round(g.power_w)} / ${g.power_limit_w == null ? NA : Math.round(g.power_limit_w)}`, h('small', null, ' W'))),
        meter(powFrac, 'meter-quiet'),
      ),
    );
  }

  function providersSection(list) {
    if (parts) {
      parts.provRows = new Map();
      parts.provShape = provShape(list);
    }
    return h(
      'section',
      { class: 'insp-sec', 'aria-label': t('resources.providers') },
      h('div', { class: 'insp-sec-head' }, h('h3', null, t('resources.providers'))),
      budgetHit() ? h('p', { class: 'insp-note insp-alert' }, icon('circle-pause', { size: 14 }), t('resources.budget_hit')) : null,
      list.length ? list.map(providerRow) : h('p', { class: 'insp-note' }, t('resources.no_providers')),
    );
  }

  function balanceEl(p) {
    const failed = !!(p.balance_error || p.balance_error_info);
    const bal = p.balance != null ? U.fmtMoney(Number(p.balance), p.balance_currency) || p.balance : NA;
    const balTitle = failed
      ? balanceErrorText(p)
      : p.balance != null
        ? p.balance_at
          ? t('resources.balance_tip_at', { when: U.relTime(p.balance_at) })
          : t('resources.balance_tip')
        : t('resources.no_balance_api');
    return h('span', { class: 'prov-bal' + (failed ? ' is-error' : ''), title: balTitle, tabindex: failed ? '0' : undefined }, failed ? icon('triangle-alert', { size: 13 }) : icon('wallet', { size: 13 }), bal);
  }

  // Everything of a provider row below its head (no interactive elements).
  function providerBody(p) {
    const row = (k, tk, rate) => [
      h('span', { class: 'ptab-k' }, k),
      h('span', { class: 'ptab-v' }, U.fmtTokens(tk.prompt_tokens)),
      h('span', { class: 'ptab-v' }, U.fmtTokens(tk.completion_tokens)),
      h('span', { class: 'ptab-v' }, rate == null ? NA : U.fmtPercent(rate, 0)),
    ];
    const ts = p.tariff_state;
    return h(
      'div',
      { class: 'prov-body' },
      ts && ts.missing_holidays ? h('p', { class: 'insp-note prov-gap' }, icon('info', { size: 13 }), t('resources.missing_holidays')) : null,
      h('div', { class: 'prov-conc', title: t('resources.conc_tip', { used: p.inflight, max: p.max }) }, h('span', { class: 'dim' }, t('resources.conc')), slots(p.inflight, p.max, t('resources.conc')), h('span', { class: 'num-strong' }, `${p.inflight} / ${p.max}`)),
      h(
        'div',
        { class: 'ptab', role: 'table', 'aria-label': t('resources.usage_label', { name: p.name }) },
        h('span', { class: 'ptab-h' }),
        h('span', { class: 'ptab-h' }, t('resources.col.input')),
        h('span', { class: 'ptab-h' }, t('resources.col.output')),
        h('span', { class: 'ptab-h' }, t('resources.col.cache_hit')),
        row(t('resources.row.today'), p.today, p.cache_hit_rate_today),
        row(t('resources.row.total'), p.total, p.cache_hit_rate_total),
      ),
      // D3: part of today's requests had no price (not in the estimate);
      // without any estimate (cost_today null) only tokens are shown
      p.cost_today != null && p.cost_unpriced_requests > 0
        ? h('p', { class: 'insp-note', title: t('resources.unpriced_tip', { n: p.cost_unpriced_requests }) }, icon('info', { size: 13 }), t('resources.unpriced'))
        : null,
    );
  }

  const balKeyOf = (p) => JSON.stringify([p.balance, p.balance_currency, p.balance_error, p.balance_at]);

  function providerRow(p) {
    const ref = { name: h('span', { class: 'prov-name' }, p.name), tag: null, tagSlot: h('span', { class: 'prov-tag' }), bal: balanceEl(p), balKey: balKeyOf(p), body: providerBody(p) };
    setTag(ref, p.tariff_state);
    if (parts && parts.provRows) parts.provRows.set(p.id, ref);
    return h('div', { class: 'prov' }, h('div', { class: 'prov-head' }, ref.name, ref.tagSlot, ref.bal), ref.body);
  }

  // The peak / off-peak tag (P12.4): the element stays, its text and hover change.
  function setTag(ref, ts) {
    if (!ts || !hasPeaks(ts)) {
      if (ref.tag) ref.tag.remove();
      ref.tag = null;
      return;
    }
    if (ref.tag) updateTariffTag(ref.tag, ts);
    else ref.tagSlot.append((ref.tag = tariffTag(ts)));
  }

  function updateProviderRow(ref, p) {
    if (!ref) return;
    // T041: a provider renamed in the settings shows its new name with the next snapshot
    if (ref.name.textContent !== p.name) ref.name.textContent = p.name;
    setTag(ref, p.tariff_state);
    const k = balKeyOf(p);
    if (k !== ref.balKey) {
      const b = balanceEl(p);
      ref.bal.replaceWith(b);
      ref.bal = b;
      ref.balKey = k;
    }
    const body = providerBody(p);
    ref.body.replaceWith(body);
    ref.body = body;
  }

  // Collapsed: the toggle plus four live numbers (updated in place), and
  // the Colab cell while Colab is on (colab-obs).
  function buildRail() {
    const refs = {};
    const item = (k, ic, label) => {
      const v = h('span', { class: 'rail-v' }, NA);
      const box = h('div', { class: 'rail-stat' }, icon(ic, { size: 15 }), h('span', { class: 'rail-k' }, label), v);
      refs[k] = { box, v };
      return box;
    };
    parts = { rail: refs };
    const colab = item('colab', 'server', 'Colab');
    colab.classList.add('rail-colab');
    refs.colab.sub = h('span', { class: 'rail-sub' });
    refs.colab.mini = h('span', { class: 'rail-mini', 'aria-hidden': 'true', hidden: true });
    colab.append(refs.colab.sub, refs.colab.mini);
    const wrap = h(
      'div',
      { class: 'insp-rail' },
      iconBtn('panel-right-open', t('resources.expand'), { class: 'insp-toggle', fkey: 'insp:toggle', onclick: opts.onExpand }),
      item('gpu', 'gauge', 'GPU'),
      item('temp', 'thermometer', t('resources.gpu.temp')),
      item('asr', 'audio-lines', 'ASR'),
      item('api', 'activity', 'API'),
      colab,
    );
    updateRail();
    return wrap;
  }

  // The rail's Colab cell (C.railColab): the value, below it the GPU or
  // "CU", the mini memory bar with a session; hidden while Colab is off.
  function updateRailColab(ref) {
    const cell = C.railColab(cardOf());
    if (!cell) {
      ref.box.hidden = true;
      ref.box.style.display = 'none';
      return;
    }
    ref.v.textContent = cell.value;
    ref.sub.textContent = cell.sub;
    ref.box.title = cell.title;
    ref.box.classList.toggle('is-warn', cell.warn);
    ref.box.hidden = false;
    ref.box.style.display = '';
    clear(ref.mini);
    ref.mini.hidden = !cell.mini;
    if (cell.mini) {
      for (const s of cell.mini.segs) ref.mini.append(h('span', { class: 'seg-' + s.kind, style: { flexBasis: `${(s.frac * 100).toFixed(2)}%` } }));
      ref.mini.append(h('span', { class: 'seg-free' }));
    }
  }

  function updateRail() {
    const r = parts.rail;
    const g = res && res.gpu;
    const prov = (res && res.providers) || [];
    // .rail-stat sets display: flex, which beats the hidden attribute
    const hide = (k) => {
      r[k].box.hidden = true;
      r[k].box.style.display = 'none';
    };
    const set = (k, value, title, warn = false) => {
      r[k].v.textContent = value;
      r[k].box.title = title;
      r[k].box.classList.toggle('is-warn', warn);
      r[k].box.hidden = false;
      r[k].box.style.display = '';
    };
    // cut off from the service (T24 G6): no readings of before, "--" and why
    if (offline) {
      for (const k of ['gpu', 'temp', 'asr', 'api']) set(k, NA, t('resources.offline_tip'), true);
      hide('colab');
      return;
    }
    if (!g) {
      for (const k of Object.keys(r)) hide(k);
      return;
    }
    const inflight = prov.reduce((n, p) => n + p.inflight, 0);
    const max = prov.reduce((n, p) => n + p.max, 0);
    // Without an NVIDIA card the Apple GPU's usage (its card, from ioreg) and
    // no temperature, which macOS does not give (mac-opti M6).
    const apple = !g.available ? rows().find((d) => d.kind === 'apple' && d.present) : null;
    const utilPct = apple ? apple.util_pct : g.util_pct;
    const util = utilPct == null ? NA : U.fmtPercent(utilPct / 100, 0);
    set('gpu', util, t('resources.rail.gpu', { value: util }));
    if (apple) hide('temp');
    else set('temp', g.temp_c == null ? NA : U.fmtCelsius(g.temp_c, true), t('resources.rail.temp', { value: g.temp_c == null ? NA : U.fmtCelsius(g.temp_c) }), !!g.temp_warn);
    if (devMode()) {
      // the slots of every turned-on device (SPEC-v2 I10); with the power
      // policy the tasks it suspended are named too
      const on = rows().filter((d) => d.enabled !== false);
      const used = on.reduce((n, d) => n + d.slots_used, 0);
      const max = on.reduce((n, d) => n + d.slots_max, 0);
      const n = suspendedCount();
      const title = g.paused ? t('resources.rail.asr_all_paused', { used, max }) : n ? t('resources.rail.asr_power', { used, max, n }) : t('resources.rail.asr', { used, max });
      set('asr', `${used}/${max}`, title, !!g.paused || n > 0 || cards().some((d) => d.paused || (!policy() && d.battery_hold)));
    } else set('asr', `${g.asr_slots_used}/${g.asr_slots_max}`, t(g.paused ? 'resources.rail.asr_paused' : 'resources.rail.asr', { used: g.asr_slots_used, max: g.asr_slots_max }), !!g.paused);
    set('api', `${inflight}/${max}`, t('resources.rail.api', { used: inflight, max }));
    updateRailColab(r.colab);
  }

  render();
  return {
    setResources,
    setError,
    setOffline,
    render,
    // R76 L-5: "立即关机" / "处理完当前这节后关机" from elsewhere (the
    // workbench), and whether one is in flight
    shutdownColab: remoteShutdown,
    colabBusy: () => remoteBusy,
    // the system state changed (budget_hit is shown with the providers)
    systemChanged() {
      if (parts && !parts.rail && res && provKeyOf() !== parts.provKey) update();
    },
    get: () => res,
  };
}
