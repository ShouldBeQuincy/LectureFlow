// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The user's own SSH inference servers in the UI (ssh-1b;
// .claude/research/remote-ssh-plan.md §6). Experimental: the back end
// answers GET /api/remote/servers only with LECTUREFLOW_EXPERIMENTAL_SSH=1
// (404 api.not_found otherwise), and nothing here shows while it is off.
//
// - Shared state: the server list (GET /api/remote/servers, internal/
//   sshremote.ServerView), loaded at start and on SSE "changed devices";
//   sshEnabled() is false until the back end answered with a list.
// - Settings → 远程服务器: the privacy note, the server cards (state, user
//   @ host, GPU, memory, disk, model, how it is used, the agent's version,
//   the install's progress, the last error) with 测试连接 / 暂停 / 更新代理
//   / 修改 / 移除 (optionally uninstalling on the server) / 继续设置, and
//   the wizard "添加服务器": name and address → authorize (the public key
//   line as one command per system: Linux, macOS, Windows) → check the host
//   key fingerprint (the command that shows it on the server, the user's
//   own known_hosts verdict; when known_hosts lists another key, a box to
//   tick first) → detect (what the server reports; the model
//   its GPU memory, on a Mac its memory, suits — editable) → install (the
//   size first; it goes on in the background) → test → how to use it
//   (before this computer's devices, or when they are busy: the device
//   order). Every change is sent at once; what is shown is the server's
//   answer (nothing optimistic).
// - A confirmed host key that changed (SEC7 M1): the card's 继续设置 opens
//   the host key step, which shows the confirmed fingerprint next to the
//   presented one, a warning (reinstalled, or someone in the middle), the
//   command to check it on the server and a box to tick before 替换为新密钥
//   (trust with replace and previous).
// - The lecture menu's "改用“服务器”处理" (POST /api/lectures/{id}/use-remote).
// - Helpers of the resource panel (the cards of the snapshot's remotes[]
//   whose kind is ssh), the device list and the diagnostics page.

import { h, clear, btn, toast, confirmDialog, openDialog, copyText, skeleton, errorBlock } from './dom.js';
import { icon } from './icons.js';
import { t, has } from './i18n.js';
import * as U from './util.js';
import { api } from './api.js';
import { appRequest } from './settings_app.js';

// ---- shared state ----

let enabled = null; // null: not asked yet; true: the list came; false: 404 (switched off)
let servers = [];
let inflight = null;
const listeners = new Set();

/** The SSH servers are switched on (the back end answered with a list). */
export function sshEnabled() {
  return enabled === true;
}

/** The last server list (ServerView[], by name). */
export function sshServers() {
  return servers;
}

export function onServersChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function setServers(on, list) {
  const prev = JSON.stringify([enabled, servers]);
  enabled = on;
  servers = Array.isArray(list) ? list : [];
  if (JSON.stringify([enabled, servers]) !== prev) for (const fn of listeners) fn();
}

/**
 * GET /api/remote/servers. 404 means the SSH servers are off (nothing is
 * shown); other failures keep the last list. The switch is read when the
 * app starts, so once the back end said it is off nothing is asked again.
 */
export function loadServers() {
  if (enabled === false) return Promise.resolve();
  if (inflight) return inflight;
  inflight = appRequest('GET', '/api/remote/servers')
    .then(
      (r) => setServers(true, (r && r.servers) || []),
      (e) => {
        if (e && e.status === 404) setServers(false, []);
      },
    )
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

/** One server's new view (an answer of the API) replaces it in the list. */
export function noteServer(v) {
  if (!v || !v.id) return;
  const list = servers.slice();
  const i = list.findIndex((s) => s.id === v.id);
  if (i >= 0) list[i] = v;
  else list.push(v);
  setServers(true, list);
}

/** Whether a device id is an SSH server's ("ssh:<slug>"). */
export function isSSHDevice(id) {
  return typeof id === 'string' && id.startsWith('ssh:');
}

/** The server of a device row (null when unknown). */
export function serverOfDevice(id) {
  return servers.find((s) => s.device_id === id) || null;
}

// ---- names and texts ----

export function stateName(s) {
  return s && has(`remote.state.${s}`) ? t(`remote.state.${s}`) : s || '';
}

// chip tone of a state
const OK_STATES = new Set(['ready', 'idle']);
const RUN_STATES = new Set(['busy', 'installing', 'connecting']);
const ERR_STATES = new Set(['auth_failed', 'host_key_changed', 'unsupported', 'error']);
export function stateTone(s) {
  if (OK_STATES.has(s)) return 'ok';
  if (RUN_STATES.has(s)) return 'run';
  if (ERR_STATES.has(s)) return 'err';
  return 'warn';
}
const CHIP = { ok: 'chip-ok', run: 'chip-run', err: 'chip-err', warn: 'chip-warn' };
export function stateChip(s) {
  return h('span', { class: `chip ${CHIP[stateTone(s)]}` }, stateName(s));
}

/** The setup is not finished: the wizard can continue it. */
export function needsSetup(v) {
  return !!v && (!v.trusted || ['untrusted', 'auth_failed', 'setup', 'host_key_changed'].includes(v.state) || (!v.kit_ready && v.state !== 'installing'));
}

/** The server's last error ({code, params}) in words ('' without one). */
export function lastErrorText(v) {
  const e = v && v.last_error;
  return e && e.code ? U.errorInfoText({ code: e.code, params: { device: v.name, ...(e.params || {}) } }) : '';
}

/** "user@host:port" (the port only when it is not 22). */
export function endpointText(v) {
  return `${v.username}@${v.host}${v.port && v.port !== 22 ? ':' + v.port : ''}`;
}

/**
 * The machine of a hop (connect's host_key_hop): the hop-th jump host, or
 * past the last one the server itself. Its user, host and port, for
 * endpointText.
 */
export function hopEndpoint(v, hop) {
  const jumps = (v && v.jumps) || [];
  return hop >= 0 && hop < jumps.length ? jumps[hop] : v;
}

/** "RTX 4090 · 24 GB" (Apple: the GPU name with "Metal"), '' before a probe. */
export function gpuText(p) {
  if (!p) return '';
  const vram = p.vram_total_mb > 0 ? U.fmtMB(p.vram_total_mb) : '';
  return [p.gpu_name, p.metal ? 'Metal' : vram].filter(Boolean).join(' · ');
}

/** Platform and engine as brand names ("Linux · faster-whisper"). */
export function platformText(v) {
  const os = { 'linux-x64': 'Linux', 'darwin-arm64': 'macOS', 'windows-x64': 'Windows' }[v.platform] || '';
  const engine = v.engine === 'mlx' ? 'MLX' : v.engine || '';
  return [os, engine].filter(Boolean).join(' · ');
}

/** The install's step in words ({text, frac}; null while not installing). */
export function installText(v) {
  const s = v && v.install;
  if (!s) return null;
  const frac = Math.max(0, Math.min(1, Number(s.fraction) || 0));
  if (s.step === 'done') return { text: t('settings.remote.install_done'), frac: 1 };
  return { text: s.step === 'model' ? t('settings.remote.install_model') : t('settings.remote.install_env'), frac };
}

/** "Use it first" or "when this computer is busy" (the server's settings). */
export function groupText(v) {
  return v && v.settings && v.settings.group === 'prefer' ? t('settings.remote.group.prefer') : t('settings.remote.group.supplement');
}

/**
 * The group the wizard preselects: before this computer's devices when it
 * has no GPU of its own (only CPU devices are on), else after them.
 */
export function suggestGroup(devices) {
  const local = (devices || []).filter((d) => d && d.enabled && !isSSHDevice(d.id) && d.kind !== 'remote');
  return local.some((d) => d.kind && d.kind !== 'cpu') ? 'supplement' : 'prefer';
}

// ---- requests ----

const enc = encodeURIComponent;
const req = (method, path, body) => appRequest(method, path, body);
const serverPath = (id, rest = '') => `/api/remote/servers/${enc(id)}${rest}`;

/** An API error in words (the remote.* and remote_server.* codes have texts). */
export function requestError(e, name) {
  const b = e && e.body && e.body.error;
  if (b && b.code) return U.errorInfoText({ code: b.code, params: { device: name || '', ...(b.params || {}) } }, e.message);
  return (e && e.message) || t('common.unknown_error');
}

// ---- host keys ----

/**
 * A presented host key that needs a ticked box before it is trusted: it
 * replaces a confirmed key (changed, SEC7 M1), or the user's own
 * known_hosts lists another key for the host or revokes this one (L1).
 */
export function keyNeedsCheck(k) {
  return !!k && (!!k.changed || k.known_hosts === 'mismatch');
}

/**
 * The presented key of a connect / trust answer (null without one); a
 * changed key carries changed and the confirmed key's previous_fingerprint.
 */
export function presentedKey(r) {
  if (!r || !r.host_key) return null;
  return r.changed ? { ...r.host_key, changed: true, previous_fingerprint: r.previous_fingerprint || '' } : r.host_key;
}

/** The body of POST …/trust: replacing a confirmed key names it (previous). */
export function trustBody(k, hop) {
  const b = { hop: hop || 0, fingerprint: k.fingerprint };
  if (k.changed) {
    b.replace = true;
    b.previous = k.previous_fingerprint;
  }
  return b;
}

// ---- "改用“服务器”处理" ----

const USE_STATES = new Set(['queued', 'running', 'failed', 'cancelled']);

/** Servers a lecture can be sent to: set up, not paused, on a supported system. */
export function usableServers() {
  return servers.filter((s) => s.trusted && s.kit_ready && s.model && !(s.settings && s.settings.paused) && s.state !== 'unsupported');
}

export async function useRemote(lec, server) {
  const ok = await confirmDialog({
    title: t('menu.lecture.use_remote', { name: server.name }),
    message: t('lecture.use_remote.message', { name: server.name }),
    okText: t('common.ok'),
  });
  if (!ok) return null;
  try {
    const l = await req('POST', `/api/lectures/${enc(lec.id)}/use-remote`, { device: server.device_id });
    toast(t('lecture.use_remote.done', { name: server.name }), 'success');
    return l;
  } catch (e) {
    toast(requestError(e, server.name), 'error');
    throw e;
  }
}

/**
 * The lecture menu item (null when not offered): the SSH servers are on,
 * one is set up, and the lecture's transcription is queued, running,
 * failed or cancelled and not already on that server. Several servers: a
 * submenu. done(promise of the lecture) renders the answer.
 */
export function useRemoteMenuItem(lec, done) {
  if (!sshEnabled()) return null;
  const asr = ((lec && lec.stages) || []).find((s) => s.stage === 'asr');
  if (!asr || !USE_STATES.has(asr.status)) return null;
  const on = asr.model && asr.model.asr && asr.model.asr.device_id;
  const list = usableServers().filter((s) => !(on === s.device_id && (asr.status === 'running' || asr.status === 'queued')));
  if (!list.length) return null;
  const item = (s) => ({ id: `use_remote:${s.id}`, label: t('menu.lecture.use_remote', { name: s.name }), icon: 'plug', onSelect: () => done(useRemote(lec, s)) });
  if (list.length === 1) return { ...item(list[0]), id: 'use_remote' };
  return { id: 'use_remote', label: t('menu.lecture.use_remote', { name: t('settings.remote.title') }), icon: 'plug', items: list.map(item) };
}

// ---- the resource panel: the snapshot's SSH cards ----

/** The SSH servers' cards of a resource snapshot (internal/remote.Card, kind ssh). */
export function sshCards(res) {
  return res && Array.isArray(res.remotes) ? res.remotes.filter((c) => c && c.kind === 'ssh') : [];
}

/** A card has a live session (machine readings, or ready / busy): the full card; otherwise one line. */
export function cardConnected(c) {
  return !!c && (!!c.machine || c.state === 'ready' || c.state === 'busy');
}

/** A lecture's step on a server in words (the connection step is "正在连接"). */
export function jobStepText(stage) {
  if (stage === 'wait_accelerator') return stateName('connecting');
  return stage && has(`colab.step.${stage}`) ? t(`colab.step.${stage}`) : stage || '';
}

// Round 9 WV-11 (R86 L3; convention C3): disk space is shown in decimal
// units (U.fmtBytes), as the install's download and installed sizes beside
// it are: 8200 MiB free and 8.6e9 bytes to install both read "8.6 GB".
// Memory and video memory stay in U.fmtMB (1024).
export function diskText(mb) {
  return U.fmtBytes(mb * 1048576);
}

/**
 * The sentence above the install step: what it downloads, what it takes
 * when installed, and the server's free disk space ('' without kit_size).
 */
export function installSizeText(v) {
  if (!v || !v.kit_size) return '';
  return t('settings.remote.install_size', { download: U.fmtBytes(v.kit_size.download_bytes), installed: U.fmtBytes(v.kit_size.installed_bytes), free: v.probe && v.probe.disk_free_mb >= 0 ? diskText(v.probe.disk_free_mb) : '--' });
}

// ---- the diagnostics: one group of rows per server ----

/**
 * The diagnostics rows of a server: [{key, label, status: ok|warn|error,
 * value, hint}] — connection, agent, Python environment, GPU / driver,
 * model, disk.
 */
export function diagRows(v) {
  const rows = [];
  const tone = stateTone(v.state);
  const conn = [stateName(v.state), v.latency_ms > 0 ? t('resources.ssh.latency', { ms: U.fmtInt(v.latency_ms) }) : ''].filter(Boolean).join(' · ');
  rows.push({ key: 'connection', label: t('system.remote.connection'), status: tone === 'err' ? 'error' : tone === 'warn' ? 'warn' : 'ok', value: conn, hint: lastErrorText(v) });
  let agent = { status: 'warn', value: '--' };
  if (v.agent_version) agent = v.agent_current ? { status: 'ok', value: v.agent_version } : { status: 'warn', value: t('system.remote.agent_old', { version: v.agent_version }) };
  rows.push({ key: 'agent', label: t('system.remote.agent'), hint: '', ...agent });
  const inst = installText(v);
  const env = v.kit_ready
    ? { status: 'ok', value: t('compui.state.installed') }
    : inst
      ? { status: 'warn', value: `${inst.text} ${U.pct(inst.frac)}` }
      : { status: 'warn', value: t('system.config.python.none') };
  rows.push({ key: 'python', label: t('system.remote.python'), hint: '', ...env });
  const p = v.probe;
  // never reached yet (setup not finished): nothing more is known
  if (!p) return [rows[0], rows[2]];
  const gpu = p ? [gpuText(p), p.driver].filter(Boolean).join(' · ') : '';
  rows.push({ key: 'gpu', label: t('system.remote.gpu'), status: p && (p.gpu_name || p.metal) ? 'ok' : 'warn', value: gpu || '--', hint: '' });
  rows.push({ key: 'model', label: t('settings.asr.model'), status: v.model ? 'ok' : 'warn', value: v.model ? U.comboText(v.model, v.precision) : '--', hint: '' });
  let disk = { status: 'warn', value: '--' };
  if (p && p.disk_free_mb >= 0 && p.platform !== '') {
    const needMB = !v.kit_ready && v.kit_size ? v.kit_size.installed_bytes / (1024 * 1024) : 2048;
    disk = { status: p.disk_free_mb < needMB ? 'warn' : 'ok', value: diskText(p.disk_free_mb) };
  }
  rows.push({ key: 'disk', label: t('settings.remote.fact.disk'), hint: '', ...disk });
  return rows;
}

// ---- the settings section ----

const OSES = [
  ['linux', 'Linux'],
  ['mac', 'macOS'],
  ['windows', 'Windows'],
];
const STEPS = ['address', 'authorize', 'hostkey', 'detect', 'install', 'test', 'use'];
const POLL_MS = 1000;

// createRemoteSection({head, formRow, switchBtn, segmented, rerender}):
// the helpers of view_settings.js (like settings_colab.js).
export function createRemoteSection(ui) {
  let alive = true;
  let loadError = '';
  let loaded = false;
  const busy = new Set(); // server ids with a request in flight
  const errors = {}; // server id → the last failed action's text
  let wiz = null; // the wizard: {step, id, form, fieldErr, error, busy, pub, os, hostKey, hop, keyChecked, focus, devices, group, tested}
  let pollTimer = null;
  const redraw = () => alive && ui.rerender();
  const unsub = onServersChange(() => {
    syncPoll();
    redraw();
  });

  async function load() {
    try {
      await loadServers();
      loadError = '';
    } catch (e) {
      loadError = (e && e.message) || t('common.load_failed');
    }
    loaded = true;
    syncPoll();
    redraw();
  }

  // While a server installs (the wizard's step or a card) its view is read
  // every second; SSE "changed devices" also reloads the list.
  function syncPoll() {
    const installing = servers.some((s) => s.state === 'installing' || s.install);
    clearTimeout(pollTimer);
    pollTimer = null;
    if (alive && installing) pollTimer = setTimeout(() => loadServers().then(syncPoll), POLL_MS);
  }

  const current = () => (wiz && wiz.id ? servers.find((s) => s.id === wiz.id) || null : null);

  async function act(v, fn) {
    if (busy.has(v.id)) return;
    busy.add(v.id);
    delete errors[v.id];
    redraw();
    try {
      const r = await fn();
      if (r && r.id) noteServer(r);
      else if (r && r.server) noteServer(r.server);
    } catch (e) {
      errors[v.id] = requestError(e, v.name);
    }
    busy.delete(v.id);
    await loadServers();
    redraw();
  }

  const testServer = (v) => act(v, () => req('POST', serverPath(v.id, '/test')));
  const updateAgent = (v) => act(v, () => req('POST', serverPath(v.id, '/install')));
  const setPaused = (v, paused) => act(v, () => req('PATCH', serverPath(v.id), { paused }));

  async function editServer(v) {
    const r = await openDialog(t('settings.remote.edit_title', { name: v.name }), (close) => {
      const f = endpointFields({ name: v.name, host: v.host, port: v.port, username: v.username });
      return h(
        'form',
        {
          onsubmit: (e) => {
            e.preventDefault();
            close(f.values());
          },
        },
        f.el,
        h('p', { class: 'dlg-hint rs-edit-hint' }, t('settings.remote.edit_hint')),
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), h('button', { type: 'submit', class: 'btn btn-primary' }, t('common.save'))),
      );
    });
    if (!r) return;
    const patch = {};
    for (const k of ['name', 'host', 'port', 'username']) if (r[k] !== v[k]) patch[k] = r[k];
    if (!Object.keys(patch).length) return;
    await act(v, () => req('PATCH', serverPath(v.id), patch));
  }

  async function removeServer(v) {
    const uninstall = h('input', { type: 'checkbox', checked: true, dataset: { fkey: `set:remote:${v.id}:uninstall` } });
    const ok = await confirmDialog({
      title: t('settings.remote.remove_title', { name: v.name }),
      message: t('settings.remote.remove_message'),
      body: h(
        'div',
        { class: 'rs-remove' },
        h('code', { class: 'mono rs-cmd-text' }, v.public_key || ''),
        h('label', { class: 'rs-check' }, uninstall, h('span', null, t('settings.remote.remove_uninstall'))),
      ),
      okText: t('common.remove'),
      danger: true,
    });
    if (!ok) return;
    busy.add(v.id);
    redraw();
    try {
      await req('DELETE', serverPath(v.id, uninstall.checked ? '?uninstall=1' : ''));
      if (wiz && wiz.id === v.id) wiz = null;
    } catch (e) {
      errors[v.id] = requestError(e, v.name);
    }
    busy.delete(v.id);
    await loadServers();
    redraw();
  }

  // ---- the wizard ----

  function openWizard(v) {
    wiz = { step: 0, id: null, form: { name: '', host: '', port: 22, username: '' }, fieldErr: '', error: '', busy: false, pub: null, os: 'linux', hostKey: null, hop: 0, keyChecked: false, focus: null, devices: null, group: null, tested: null };
    if (v) {
      wiz.id = v.id;
      wiz.form = { name: v.name, host: v.host, port: v.port, username: v.username };
      // continue where the setup stopped; a changed host key: its own check,
      // also while the server itself is not confirmed yet (a jump host's
      // key changed: nothing has to be authorized again, SEC39 L-A)
      if (v.state === 'host_key_changed') wiz.step = STEPS.indexOf('hostkey');
      else if (!v.trusted || v.state === 'untrusted') wiz.step = 1;
      else if (v.state === 'auth_failed') wiz.step = 1;
      else if (!v.kit_ready) wiz.step = v.probe ? 4 : 2;
      else wiz.step = 5;
      enterStep(wiz.step);
    }
    redraw();
  }

  function closeWizard() {
    wiz = null;
    redraw();
  }

  async function step(fn) {
    if (!wiz || wiz.busy) return;
    wiz.busy = true;
    wiz.error = '';
    redraw();
    const w = wiz;
    try {
      await fn(w);
    } catch (e) {
      if (wiz === w) w.error = requestError(e, (current() || w.form).name);
    }
    if (wiz === w) w.busy = false;
    redraw();
    // a step that shows something new puts the focus on it (the changed key's warning)
    if (wiz === w && w.focus) {
      const el = document.querySelector(`[data-fkey="${w.focus}"]`);
      w.focus = null;
      if (el) el.focus();
    }
  }

  function goto(n) {
    if (!wiz) return;
    wiz.step = n;
    wiz.error = '';
    enterStep(n);
    redraw();
  }

  // What a step loads when it is entered.
  function enterStep(n) {
    const name = STEPS[n];
    if (name === 'authorize' && !wiz.pub) step(async (w) => (w.pub = await req('GET', serverPath(w.id, '/pubkey'))));
    if (name === 'hostkey') step(connect);
    if (name === 'detect') step(loadDevices);
    if (name === 'test') step(runTest);
    if (name === 'use' && !wiz.group) step(async (w) => {
      await loadDevices(w);
      w.group = suggestGroup(w.devices);
    });
  }

  async function saveAddress(w) {
    const f = w.form;
    if (!w.id) {
      const v = await req('POST', '/api/remote/servers', { name: f.name, host: f.host, port: f.port, username: f.username });
      noteServer(v);
      w.id = v.id;
    } else {
      const v = current();
      const patch = {};
      for (const k of ['name', 'host', 'port', 'username']) if (!v || f[k] !== v[k]) patch[k] = f[k];
      if (Object.keys(patch).length) noteServer(await req('PATCH', serverPath(w.id), patch));
    }
    w.pub = await req('GET', serverPath(w.id, '/pubkey'));
    w.step = 1;
  }

  // A host key to confirm from a connect / trust answer (false: none).
  function showKey(w, r) {
    w.hostKey = presentedKey(r);
    w.keyChecked = false;
    w.announced = false;
    if (!w.hostKey) return false;
    w.hop = r.host_key_hop || 0;
    if (w.hostKey.changed) w.focus = 'set:remote:wiz:keychanged';
    return true;
  }

  // POST …/connect: a host key to confirm (new, or changed since it was
  // confirmed), or (already confirmed) the connected server with what it
  // reported.
  async function connect(w) {
    w.hostKey = null;
    w.keyChecked = false;
    const r = await req('POST', serverPath(w.id, '/connect'));
    if (r && r.server) noteServer(r.server);
    if (showKey(w, r)) return;
    w.step = STEPS.indexOf('detect');
    await loadDevices(w);
  }

  async function trust(w) {
    // the box must be ticked (the button is disabled until then)
    if (!w.hostKey || (keyNeedsCheck(w.hostKey) && !w.keyChecked)) return;
    const r = await req('POST', serverPath(w.id, '/trust'), trustBody(w.hostKey, w.hop));
    if (r && r.server) noteServer(r.server);
    if (showKey(w, r)) return;
    w.step = STEPS.indexOf('detect');
    await loadDevices(w);
  }

  async function loadDevices(w) {
    w.devices = await api.asrDevices();
  }

  async function setModel(w, patch) {
    const v = current();
    if (!v) return;
    await api.patchDevice(v.device_id, patch);
    await loadDevices(w);
    await loadServers(); // the kit's size follows the model
  }

  async function install(w) {
    noteServer(await req('POST', serverPath(w.id, '/install')));
    syncPoll();
  }

  async function runTest(w) {
    w.tested = null;
    const v = await req('POST', serverPath(w.id, '/test'));
    noteServer(v);
    w.tested = v;
  }

  async function finish(w) {
    noteServer(await req('PATCH', serverPath(w.id), { group: w.group || 'supplement' }));
    wiz = null;
  }

  // ---- render ----

  const note = (ic, text, cls = '') => h('p', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('span', null, text));

  function copyBtn(text, fkey) {
    return btn(t('settings.remote.copy'), {
      size: 'sm',
      kind: 'ghost',
      icon: 'copy',
      fkey,
      onclick: async () => {
        const ok = await copyText(text);
        toast(ok ? t('settings.remote.copied') : t('common.copy_failed'), ok ? 'success' : 'warn');
      },
    });
  }

  function cmdBlock(text, fkey) {
    return h('div', { class: 'colab-cmd rs-cmd' }, h('code', { class: 'mono colab-cmd-text' }, text), copyBtn(text, fkey));
  }

  // name, address, port, user name: {el, values()}
  function endpointFields(init, onInput) {
    const input = (key, type, extra = {}) => {
      const el = h('input', { class: 'input' + (type === 'number' ? ' input-num' : ' mono'), type, value: init[key] == null ? '' : String(init[key]), spellcheck: 'false', autocomplete: 'off', dataset: { fkey: `set:remote:f:${key}` }, ...extra });
      if (onInput) el.addEventListener('input', () => onInput(key, type === 'number' ? Number(el.value) : el.value.trim()));
      return el;
    };
    const els = {
      name: input('name', 'text', { class: 'input', maxlength: '64', placeholder: t('settings.example', { value: 'gpu-box' }) }),
      host: input('host', 'text', { placeholder: 'gpu-box.local' }),
      port: input('port', 'number', { min: '1', max: '65535', step: '1' }),
      username: input('username', 'text'),
    };
    const field = (label, el, hint) => h('label', { class: 'dlg-field rs-field' }, h('span', { class: 'dlg-label' }, label), el, hint ? h('span', { class: 'dlg-hint' }, hint) : null);
    return {
      el: h(
        'div',
        { class: 'rs-fields' },
        field(t('settings.provider.name'), els.name),
        h('div', { class: 'rs-fields-row' }, field(t('settings.remote.host'), els.host, t('settings.remote.host_hint')), field(t('settings.remote.port'), els.port)),
        field(t('settings.remote.username'), els.username),
      ),
      values: () => ({ name: els.name.value.trim(), host: els.host.value.trim(), port: Number(els.port.value) || 22, username: els.username.value.trim() }),
    };
  }

  function stepper() {
    return h(
      'ol',
      { class: 'rs-steps', 'aria-label': t('settings.remote.add') },
      STEPS.map((s, i) =>
        h(
          'li',
          { class: 'rs-step' + (i === wiz.step ? ' is-current' : i < wiz.step ? ' is-done' : ''), 'aria-current': i === wiz.step ? 'step' : undefined },
          h('span', { class: 'rs-step-n' }, i < wiz.step ? icon('check', { size: 12 }) : String(i + 1)),
          h('span', { class: 'rs-step-t' }, t(`settings.remote.step.${s}`)),
        ),
      ),
    );
  }

  function wizFoot(...actions) {
    return h('div', { class: 'rs-wiz-foot' }, wiz.error ? note('circle-alert', wiz.error, 'is-err') : null, h('div', { class: 'set-inline rs-wiz-actions' }, actions.filter(Boolean)));
  }

  const backBtn = (n) => btn(t('onboarding.back'), { size: 'sm', kind: 'ghost', icon: 'arrow-left', disabled: wiz.busy, fkey: 'set:remote:wiz:back', onclick: () => goto(n) });
  const nextBtn = (label, onclick, o = {}) =>
    btn(label, { size: 'sm', kind: 'primary', icon: wiz.busy ? 'loader-circle' : o.icon || 'arrow-right', disabled: wiz.busy || !!o.disabled, fkey: o.fkey || 'set:remote:wiz:next', onclick });

  function stepAddress() {
    const f = endpointFields(wiz.form, (k, val) => {
      wiz.form[k] = val;
      syncNext();
    });
    const next = nextBtn(t('wizard.foot.next'), () => step(saveAddress));
    const syncNext = () => {
      const x = wiz.form;
      next.disabled = wiz.busy || !(x.name && x.host && x.username && x.port >= 1 && x.port <= 65535);
    };
    syncNext();
    return [f.el, wizFoot(next)];
  }

  function stepAuthorize() {
    const p = wiz.pub;
    if (!p) return [skeleton(3), wizFoot(backBtn(0))];
    const os = ui.segmented('set:remote:os', OSES, wiz.os, (v) => ((wiz.os = v), redraw()), t('settings.remote.step.authorize'));
    const cmds =
      wiz.os === 'windows'
        ? [cmdBlock(p.command_windows || p.command_windows_admin, 'set:remote:copy:win'), cmdBlock(p.command_windows_admin, 'set:remote:copy:winadmin')]
        : [cmdBlock(p.command, 'set:remote:copy:posix')];
    return [
      h('p', { class: 'set-intro' }, t('settings.remote.auth_intro')),
      os,
      h('p', { class: 'rs-os-hint' }, t(`settings.remote.os_hint.${wiz.os}`)),
      ...cmds,
      wizFoot(backBtn(0), nextBtn(t('settings.remote.auth_done'), () => goto(2), { fkey: 'set:remote:wiz:authorized' })),
    ];
  }

  function stepHostKey() {
    const k = wiz.hostKey;
    if (!k) {
      // connecting (or failed: retry, back to the authorization)
      return [
        wiz.busy ? note('loader-circle', stateName('connecting')) : null,
        wizFoot(backBtn(1), wiz.error ? btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', fkey: 'set:remote:wiz:retry', onclick: () => step(connect) }) : null),
      ];
    }
    const verdict = k.known_hosts === 'match' ? note('circle-check', t('settings.remote.hostkey.match'), 'is-ok') : k.known_hosts === 'mismatch' ? note('triangle-alert', t('settings.remote.hostkey.mismatch'), 'is-warn') : null;
    if (k.changed) return stepKeyChanged(k, verdict);
    const needs = keyNeedsCheck(k);
    const go = nextBtn(t('settings.remote.hostkey_trust'), () => step(trust), { icon: 'shield-check', fkey: 'set:remote:wiz:trust', disabled: needs && !wiz.keyChecked });
    return [
      h('p', { class: 'set-intro' }, t('settings.remote.hostkey_intro')),
      h('div', { class: 'rs-fpbox' }, h('span', { class: 'chip chip-quiet mono' }, k.algorithm), h('code', { class: 'mono rs-fp-big' }, k.fingerprint)),
      verdict,
      cmdBlock(k.check, 'set:remote:copy:check'),
      needs ? keyCheckBox(t('settings.remote.hostkey.mismatch_confirm'), go) : null,
      wizFoot(backBtn(1), go),
    ];
  }

  // The box to tick before a key that needs it is trusted (keyNeedsCheck);
  // go is the button it enables.
  function keyCheckBox(label, go) {
    const box = h('input', {
      type: 'checkbox',
      checked: wiz.keyChecked,
      dataset: { fkey: 'set:remote:wiz:keycheck' },
      onchange: (e) => {
        wiz.keyChecked = !!e.target.checked;
        go.disabled = wiz.busy || !wiz.keyChecked;
      },
    });
    return h('label', { class: 'rs-check rs-keycheck' }, box, h('span', null, label));
  }

  // SEC7 M1: the confirmed key changed. The warning (announced once, and
  // the focus goes to it), the machine it is about (a jump host or the
  // server: where the new fingerprint has to be checked, SEC39 L-A), both
  // fingerprints side by side, the command to check the new one on that
  // machine, and the box to tick before replacing.
  function stepKeyChanged(k, verdict) {
    const announce = !wiz.announced;
    wiz.announced = true;
    const at = hopEndpoint(current() || wiz.form, wiz.hop);
    const go = nextBtn(t('settings.remote.keychange.replace'), () => step(trust), { icon: 'shield-check', fkey: 'set:remote:wiz:replace', disabled: !wiz.keyChecked });
    const fp = (label, cls, ...value) => h('div', { class: 'rs-fpcmp-row' + cls }, h('dt', { class: 'rs-fpcmp-k' }, label), h('dd', { class: 'rs-fpcmp-v' }, ...value));
    return [
      h(
        'div',
        { class: 'rs-keychange', role: announce ? 'alert' : 'group', 'aria-labelledby': 'rs-keychange-title', tabindex: '-1', dataset: { fkey: 'set:remote:wiz:keychanged' } },
        icon('triangle-alert', { size: 18 }),
        h('div', { class: 'rs-keychange-text' }, h('strong', { id: 'rs-keychange-title' }, t('settings.remote.keychange.title')), h('p', null, t('settings.remote.keychange.warning'))),
      ),
      at && at.host ? h('div', { class: 'rs-facts rs-keyhop' }, fact(t('settings.remote.host'), endpointText(at))) : null,
      h(
        'dl',
        { class: 'rs-fpcmp' },
        fp(t('settings.remote.keychange.previous'), ' is-old', h('code', { class: 'mono rs-fp-big' }, k.previous_fingerprint || '--')),
        fp(t('settings.remote.keychange.presented'), ' is-new', h('span', { class: 'chip chip-quiet mono' }, k.algorithm), h('code', { class: 'mono rs-fp-big' }, k.fingerprint)),
      ),
      verdict,
      h('p', { class: 'set-intro' }, t('settings.remote.keychange.check')),
      cmdBlock(k.check, 'set:remote:copy:check'),
      keyCheckBox(t('settings.remote.keychange.confirm'), go),
      wizFoot(backBtn(1), go),
    ];
  }

  function fact(k, v) {
    return h('div', { class: 'rs-fact' }, h('span', { class: 'rs-fact-k' }, k), h('span', { class: 'rs-fact-v' }, v || '--'));
  }

  function facts(v) {
    const p = v.probe || {};
    return h(
      'div',
      { class: 'rs-facts' },
      fact(t('resources.remote.seg.system'), [platformText(v), p.os_version].filter(Boolean).join(' · ')),
      fact(t('settings.remote.fact.gpu'), [gpuText(p), p.driver].filter(Boolean).join(' · ')),
      fact(t('resources.system.mem'), p.mem_total_mb > 0 ? U.fmtMB(p.mem_total_mb) : ''),
      fact(t('settings.remote.fact.disk'), p.disk_free_mb >= 0 && v.probe ? diskText(p.disk_free_mb) : ''),
    );
  }

  // model and precision of the server's device row (its options)
  function modelRow(v) {
    const d = (wiz.devices || []).find((x) => x.id === v.device_id);
    if (!d) return null;
    const o = d.options || {};
    const models = (o.models || []).includes(d.model) ? o.models : [d.model, ...(o.models || [])];
    const precs = (o.precisions || []).includes(d.precision) ? o.precisions : [d.precision, ...(o.precisions || [])];
    const sel = (fkey, list, value, label, key) =>
      h(
        'select',
        { class: 'select mono', disabled: wiz.busy, 'aria-label': label, dataset: { fkey }, onchange: (e) => step((w) => setModel(w, { [key]: e.target.value })) },
        list.map((m) => h('option', { value: m, selected: m === value }, m)),
      );
    const rec = v.recommended;
    const isRec = rec && rec.model === d.model && rec.precision === d.precision;
    return h(
      'div',
      { class: 'rs-model' },
      h('span', { class: 'rs-fact-k' }, t('settings.asr.model')),
      h(
        'span',
        { class: 'set-inline' },
        sel('set:remote:model', models, d.model, t('settings.asr.model'), 'model'),
        sel('set:remote:precision', precs, d.precision, t('devices.precision'), 'precision'),
        isRec ? h('span', { class: 'chip chip-ok' }, t('devices.recommended')) : null,
        rec && !isRec ? h('span', { class: 'rs-rec dim small' }, h('span', { class: 'chip chip-quiet' }, t('devices.recommended')), h('span', { class: 'mono' }, U.comboText(rec.model, rec.precision))) : null,
      ),
    );
  }

  function stepDetect() {
    const v = current();
    if (!v || !v.probe) return [note('loader-circle', stateName('connecting')), wizFoot(backBtn(2))];
    if (v.state === 'unsupported') return [facts(v), note('circle-alert', lastErrorText(v) || stateName(v.state), 'is-err'), wizFoot(backBtn(2))];
    return [h('p', { class: 'set-intro' }, t('settings.remote.detect_intro')), facts(v), modelRow(v), wizFoot(backBtn(2), nextBtn(t('wizard.foot.next'), () => goto(4)))];
  }

  function progress(v) {
    const p = installText(v);
    if (!p) return null;
    return h(
      'div',
      { class: 'rs-progress' },
      h('div', { class: 'rs-progress-top' }, h('span', null, p.text), h('span', { class: 'rs-progress-p' }, U.pct(p.frac))),
      h('div', { class: 'meter meter-accent' }, h('span', { class: 'meter-fill', style: { width: `${(p.frac * 100).toFixed(1)}%` } })),
    );
  }

  function stepInstall() {
    const v = current();
    if (!v) return [wizFoot(backBtn(3))];
    if (v.kit_ready && v.state !== 'installing') return [note('circle-check', t('settings.remote.install_done'), 'is-ok'), wizFoot(backBtn(3), nextBtn(t('wizard.foot.next'), () => goto(5)))];
    const installing = v.state === 'installing' || !!v.install;
    const size = installSizeText(v);
    const err = !installing && v.last_error ? lastErrorText(v) : '';
    return [
      size ? h('p', { class: 'set-intro' }, size) : null,
      installing ? progress(v) || note('loader-circle', stateName('installing')) : null,
      installing ? note('info', t('settings.remote.install_background')) : null,
      err ? note('circle-alert', err, 'is-err') : null,
      wizFoot(backBtn(3), installing ? null : nextBtn(t('settings.remote.install_start'), () => step(install), { icon: 'download', fkey: 'set:remote:wiz:install' })),
    ];
  }

  function stepTest() {
    const v = wiz.tested;
    const ok = v && (v.state === 'ready' || v.state === 'busy');
    return [
      wiz.busy ? note('loader-circle', stateName('connecting')) : null,
      ok ? note('circle-check', [t('settings.remote.test_ok', { version: v.agent_version || '--' }), v.latency_ms > 0 ? t('resources.ssh.latency', { ms: U.fmtInt(v.latency_ms) }) : ''].filter(Boolean).join(' · '), 'is-ok') : null,
      v && !ok && !wiz.busy ? note('circle-alert', lastErrorText(v) || stateName(v.state), 'is-err') : null,
      wizFoot(
        backBtn(4),
        btn(t('settings.provider.test'), { size: 'sm', icon: 'refresh-cw', disabled: wiz.busy, fkey: 'set:remote:wiz:test', onclick: () => step(runTest) }),
        nextBtn(t('wizard.foot.next'), () => goto(6), { disabled: !ok }),
      ),
    ];
  }

  function stepUse() {
    const name = 'rs-group';
    const choice = (value) =>
      h(
        'label',
        { class: 'rs-choice' + (wiz.group === value ? ' is-sel' : '') },
        h('input', { type: 'radio', name, value, checked: wiz.group === value, dataset: { fkey: `set:remote:group:${value}` }, onchange: () => ((wiz.group = value), redraw()) }),
        h('span', { class: 'rs-choice-text' }, h('strong', null, t(`settings.remote.group.${value}`)), h('span', { class: 'dim small' }, t(`settings.remote.group.${value}_hint`))),
      );
    return [
      h('div', { class: 'rs-choices', role: 'radiogroup', 'aria-label': t('settings.remote.step.use') }, choice('prefer'), choice('supplement')),
      wizFoot(backBtn(5), nextBtn(t('wizard.step.done'), () => step(finish), { icon: 'check', disabled: !wiz.group, fkey: 'set:remote:wiz:finish' })),
    ];
  }

  function wizardEl() {
    const name = STEPS[wiz.step];
    const body = { address: stepAddress, authorize: stepAuthorize, hostkey: stepHostKey, detect: stepDetect, install: stepInstall, test: stepTest, use: stepUse }[name]();
    const v = current();
    return h(
      'section',
      { class: 'rs-wiz', 'aria-label': t('settings.remote.add') },
      h(
        'header',
        { class: 'rs-wiz-hd' },
        h('h3', { class: 'colab-sub' }, v ? v.name : t('settings.remote.add')),
        btn(t('common.close'), { size: 'sm', kind: 'ghost', icon: 'x', fkey: 'set:remote:wiz:close', onclick: closeWizard }),
      ),
      stepper(),
      h('div', { class: 'rs-wiz-bd', dataset: { step: name } }, h('h4', { class: 'rs-wiz-title' }, t(`settings.remote.step.${name === 'hostkey' && wiz.hostKey && wiz.hostKey.changed ? 'hostkey_changed' : name}`)), body),
    );
  }

  // ---- the server cards ----

  // r7 (T24 J2): on the server's own card a host key to check names the
  // card's "继续设置" (offered while the setup needs it), not "in Settings"
  // as the same error says elsewhere (stage card, diagnostics).
  function cardErrorText(v) {
    const e = v && v.last_error;
    if (e && needsSetup(v) && (e.code === 'remote.host_key_mismatch' || e.code === 'remote.host_key_unknown')) {
      return t(`settings.remote.card.${e.code.slice('remote.'.length)}`, { device: v.name, fingerprint: '', ...(e.params || {}) });
    }
    return lastErrorText(v);
  }

  function card(v) {
    const isBusy = busy.has(v.id);
    const paused = !!(v.settings && v.settings.paused);
    const p = v.probe;
    const err = errors[v.id] || cardErrorText(v);
    const facts = [
      p ? [t('settings.remote.fact.gpu'), [gpuText(p), p.driver].filter(Boolean).join(' · ')] : null,
      p && p.mem_total_mb > 0 ? [t('resources.system.mem'), U.fmtMB(p.mem_total_mb)] : null,
      p && p.disk_free_mb >= 0 ? [t('settings.remote.fact.disk'), diskText(p.disk_free_mb)] : null,
      v.model ? [t('settings.asr.model'), U.comboText(v.model, v.precision)] : null,
      [t('settings.remote.step.use'), groupText(v)],
    ].filter(Boolean);
    const actions = [
      needsSetup(v) ? btn(t('settings.remote.continue'), { size: 'sm', kind: 'primary', icon: 'arrow-right', disabled: isBusy, fkey: `set:remote:${v.id}:continue`, onclick: () => openWizard(v) }) : null,
      v.trusted ? btn(t('settings.provider.test'), { size: 'sm', icon: isBusy ? 'loader-circle' : 'plug', disabled: isBusy, fkey: `set:remote:${v.id}:test`, onclick: () => testServer(v) }) : null,
      btn(paused ? t('devices.resume') : t('devices.pause'), { size: 'sm', kind: paused ? 'primary' : 'ghost', icon: paused ? 'play' : 'pause', disabled: isBusy, fkey: `set:remote:${v.id}:pause`, onclick: () => setPaused(v, !paused) }),
      v.kit_ready ? btn(t('settings.remote.update_agent'), { size: 'sm', kind: 'ghost', icon: 'refresh-cw', disabled: isBusy || v.state === 'installing', fkey: `set:remote:${v.id}:update`, onclick: () => updateAgent(v) }) : null,
      btn(t('common.edit'), { size: 'sm', kind: 'ghost', icon: 'pencil', disabled: isBusy, fkey: `set:remote:${v.id}:edit`, onclick: () => editServer(v) }),
      btn(t('common.remove'), { size: 'sm', kind: 'ghost', icon: 'trash-2', disabled: isBusy, fkey: `set:remote:${v.id}:remove`, onclick: () => removeServer(v) }),
    ];
    return h(
      'li',
      { class: 'rs-card' + (paused ? ' is-paused' : ''), dataset: { server: v.id } },
      h(
        'div',
        { class: 'rs-card-hd' },
        icon('server', { size: 16 }),
        h('span', { class: 'rs-name', title: v.name }, v.name),
        stateChip(paused ? 'paused' : v.state),
        v.latency_ms > 0 ? h('span', { class: 'chip chip-quiet' }, t('resources.ssh.latency', { ms: U.fmtInt(v.latency_ms) })) : null,
      ),
      h('div', { class: 'rs-sub' }, h('span', { class: 'mono' }, endpointText(v)), platformText(v) ? h('span', null, platformText(v)) : null, v.agent_version ? h('span', null, t('settings.remote.agent', { version: v.agent_version })) : null),
      facts.length ? h('div', { class: 'rs-facts' }, facts.map(([k, x]) => fact(k, x))) : null,
      progress(v),
      err ? note('circle-alert', err, 'is-warn') : null,
      h('div', { class: 'set-inline rs-actions' }, actions.filter(Boolean)),
    );
  }

  function privacyBlock() {
    return h(
      'div',
      { class: 'colab-privacy', role: 'note' },
      icon('shield-check', { size: 16 }),
      h('div', { class: 'colab-privacy-text' }, h('strong', null, t('settings.remote.privacy_title')), h('p', null, t('settings.remote.privacy'))),
    );
  }

  function render(shell) {
    shell.hidden = !sshEnabled();
    if (!sshEnabled()) return;
    const add = btn(t('settings.remote.add'), { size: 'sm', icon: 'plus', disabled: !!wiz, fkey: 'set:remote:add', onclick: () => openWizard(null) });
    shell.append(ui.head('plug', t('settings.remote.title'), servers.length ? h('span', { class: 'card-count' }, String(servers.length)) : null, add));
    if (!loaded && !servers.length) {
      shell.append(h('div', { class: 'card-bd' }, loadError ? errorBlock(loadError, load) : skeleton(3)));
      return;
    }
    const empty = () =>
      h(
        'div',
        { class: 'set-empty' },
        h('div', { class: 'set-empty-icon' }, icon('server', { size: 22 })),
        h('div', null, h('p', null, t('settings.remote.empty'))),
        btn(t('settings.remote.add'), { size: 'sm', kind: 'primary', icon: 'plus', fkey: 'set:remote:add-empty', onclick: () => openWizard(null) }),
      );
    const list = servers.length ? h('ul', { class: 'rs-list' }, servers.map(card)) : wiz ? null : empty();
    shell.append(h('div', { class: 'card-bd set-form rs-form' }, privacyBlock(), wiz ? wizardEl() : null, list));
  }

  return {
    render,
    load,
    // SSE "changed devices": a server's state or the device rows changed
    onChanged(ev) {
      if (ev && ev.kind === 'devices') {
        loadServers();
        if (wiz && STEPS[wiz.step] === 'detect' && !wiz.busy) step(loadDevices);
      }
    },
    destroy() {
      alive = false;
      clearTimeout(pollTimer);
      unsub();
    },
  };
}
