// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// ssh-1b: the logic of the SSH servers in the UI run in Node
// (web/ssh_1b_test.go starts it with the web directory as a file: URL).
// settings_remote.js is imported as the page imports it, with the browser
// globals its imports read at load stubbed and fetch answering the server
// list; t() answers the keys (no catalog loaded). Checks: switched off on a
// 404, the states and their tones, the setup that can be continued, the
// texts of a server, the group the wizard suggests, the lecture menu item,
// the resource cards, the diagnostics' rows, the waiting texts.

import assert from 'node:assert/strict';

const store = { getItem: () => null, setItem() {}, removeItem() {} };
const el = () => ({ dataset: {}, style: {}, classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, append() {}, addEventListener() {} });
let fetches = 0;
let answer = { status: 404, body: { error: { code: 'api.not_found' } } };
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: { documentElement: el(), body: el(), addEventListener() {}, querySelector: () => null, getElementById: () => null, createElement: el },
  addEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  fetch: async () => (fetches++, { ok: answer.status < 400, status: answer.status, text: async () => JSON.stringify(answer.body), headers: { get: () => null } }),
});
globalThis.window = globalThis;

const R = await import(new URL('settings_remote.js', process.argv[2]).href);
const U = await import(new URL('util.js', process.argv[2]).href);

const ready = {
  id: 'gpu-box', device_id: 'ssh:gpu-box', name: 'gpu-box', host: 'gpu-box.example.net', port: 22, username: 'student', state: 'ready', trusted: true,
  platform: 'linux-x64', engine: 'faster-whisper', kit_ready: true, agent_version: '3.2.0', agent_current: true, model: 'large-v3', precision: 'auto',
  settings: { group: 'prefer' }, latency_ms: 18,
  probe: { os_version: 'Ubuntu 24.04', cpus: 24, mem_total_mb: 64215, disk_free_mb: 386210, gpu_name: 'NVIDIA GeForce RTX 4090', vram_total_mb: 24564, driver: '580.95.05', metal: false },
};
const mac = { ...ready, id: 'mac', device_id: 'ssh:mac', name: 'mac', platform: 'darwin-arm64', engine: 'mlx', state: 'offline', agent_version: '3.1.0', agent_current: false,
  model: 'large-v3-turbo', precision: 'fp16', settings: { group: 'supplement' }, latency_ms: null, last_error: { code: 'remote.offline', params: { device: 'mac' } },
  probe: { os_version: 'macOS 26.1', mem_total_mb: 16384, disk_free_mb: 1000, gpu_name: 'Apple M4', vram_total_mb: -1, metal: true } };
const fresh = { id: 'lab', device_id: 'ssh:lab', name: 'lab', host: 'lab.example.org', port: 2222, username: 's1', state: 'untrusted', trusted: false, kit_ready: false,
  settings: { group: 'supplement' } };
const paused = { ...ready, id: 'p', device_id: 'ssh:p', name: 'paused one', settings: { group: 'prefer', paused: true } };

// ---- switched off: a 404 ----
// (a module instance of its own: once off, it stays off)
{
  const Off = await import(new URL('settings_remote.js?off', process.argv[2]).href);
  const before = fetches;
  await Off.loadServers();
  assert.equal(Off.sshEnabled(), false, 'a 404 means the SSH servers are off');
  assert.deepEqual(Off.sshServers(), []);
  const lec = { id: 'l1', stages: [{ stage: 'asr', status: 'failed' }] };
  assert.equal(Off.useRemoteMenuItem(lec, () => {}), null, 'no menu item while off');
  assert.equal(fetches - before, 1, 'the first load asks once');
  await Off.loadServers();
  await Off.loadServers();
  assert.equal(fetches - before, 1, 'switched off: no more requests');
  assert.equal(Off.sshEnabled(), false);
}

// ---- switched on: the list ----
{
  let changes = 0;
  const off = R.onServersChange(() => changes++);
  answer = { status: 200, body: { servers: [ready, fresh, mac, paused] } };
  await R.loadServers();
  assert.equal(R.sshEnabled(), true);
  assert.equal(R.sshServers().length, 4);
  assert.equal(changes, 1, 'listeners hear the change once');
  await R.loadServers();
  assert.equal(changes, 1, 'the same list again is no change');
  off();
  assert.equal(R.serverOfDevice('ssh:mac').name, 'mac');
  assert.equal(R.serverOfDevice('cuda:0'), null);
  assert.equal(R.isSSHDevice('ssh:mac'), true);
  assert.equal(R.isSSHDevice('colab:remote'), false);
  // a failed request (not 404) keeps the list
  answer = { status: 500, body: { error: { code: 'x' } } };
  await R.loadServers();
  assert.equal(R.sshServers().length, 4);
  R.noteServer({ ...ready, state: 'busy' });
  assert.equal(R.serverOfDevice('ssh:gpu-box').state, 'busy', 'an answer of the API replaces the server');
  R.noteServer(ready);
}

// ---- states and texts ----
{
  assert.equal(R.stateTone('ready'), 'ok');
  assert.equal(R.stateTone('idle'), 'ok');
  assert.equal(R.stateTone('busy'), 'run');
  assert.equal(R.stateTone('installing'), 'run');
  assert.equal(R.stateTone('auth_failed'), 'err');
  assert.equal(R.stateTone('host_key_changed'), 'err');
  assert.equal(R.stateTone('offline'), 'warn');
  assert.equal(R.stateTone('setup'), 'warn');
  assert.equal(R.stateName('ready'), 'ready', 'without a catalog: the state itself');
  assert.equal(R.needsSetup(fresh), true, 'host key not checked');
  assert.equal(R.needsSetup({ ...ready, state: 'auth_failed' }), true);
  assert.equal(R.needsSetup({ ...ready, kit_ready: false, state: 'setup' }), true);
  assert.equal(R.needsSetup({ ...ready, kit_ready: false, state: 'installing' }), false, 'installing goes on by itself');
  assert.equal(R.needsSetup(ready), false);
  assert.equal(R.needsSetup(mac), false, 'offline is not a setup step');
  assert.equal(R.endpointText(ready), 'student@gpu-box.example.net');
  assert.equal(R.endpointText(fresh), 's1@lab.example.org:2222');
  assert.match(R.gpuText(ready.probe), /^NVIDIA GeForce RTX 4090 · /);
  assert.equal(R.gpuText(mac.probe), 'Apple M4 · Metal');
  assert.equal(R.gpuText(null), '');
  assert.equal(R.platformText(ready), 'Linux · faster-whisper');
  assert.equal(R.platformText(mac), 'macOS · MLX');
  assert.equal(R.groupText(ready), 'settings.remote.group.prefer');
  assert.equal(R.groupText(mac), 'settings.remote.group.supplement');
  assert.equal(R.lastErrorText(ready), '');
  assert.equal(R.installText(ready), null);
  assert.deepEqual(R.installText({ install: { step: 'packages', fraction: 0.4 } }), { text: 'settings.remote.install_env', frac: 0.4 });
  assert.deepEqual(R.installText({ install: { step: 'model', fraction: 2 } }), { text: 'settings.remote.install_model', frac: 1 });
  assert.equal(R.installText({ install: { step: 'done', fraction: 1 } }).text, 'settings.remote.install_done');
}

// ---- the group the wizard suggests ----
{
  const dev = (id, kind, enabled = true) => ({ id, kind, enabled });
  assert.equal(R.suggestGroup([dev('cuda:0', 'cuda'), dev('fw:cpu', 'cpu')]), 'supplement', 'a GPU here: when it is busy');
  assert.equal(R.suggestGroup([dev('fw:cpu', 'cpu'), dev('ov:CPU', 'cpu')]), 'prefer', 'only the CPU here: use the server first');
  assert.equal(R.suggestGroup([dev('cuda:0', 'cuda', false), dev('fw:cpu', 'cpu')]), 'prefer', 'a GPU that is off does not count');
  assert.equal(R.suggestGroup([dev('ssh:x', ''), dev('colab:remote', 'remote')]), 'prefer', 'remote devices are not this computer');
  assert.equal(R.suggestGroup([]), 'prefer');
}

// ---- the lecture menu ----
{
  const lec = (status, device) => ({ id: 'l1', title: 'W1', stages: [{ stage: 'asr', status, model: device ? { asr: { device_id: device } } : null }] });
  assert.deepEqual(R.usableServers().map((s) => s.id), ['gpu-box', 'mac'], 'set up and not paused; not the one whose setup is not finished');
  const two = R.useRemoteMenuItem(lec('failed'), () => {});
  assert.equal(two.id, 'use_remote');
  assert.equal(two.items.length, 2, 'two servers: a submenu');
  assert.equal(two.items[0].id, 'use_remote:gpu-box');
  const one = R.useRemoteMenuItem(lec('running', 'ssh:mac'), () => {});
  assert.equal(one.id, 'use_remote', 'the server it runs on is left out');
  assert.equal(one.items, undefined);
  assert.equal(one.label, 'menu.lecture.use_remote');
  assert.equal(R.useRemoteMenuItem(lec('done'), () => {}), null, 'a done transcription is not sent again');
  assert.equal(R.useRemoteMenuItem(lec('pending'), () => {}), null);
  assert.equal(R.useRemoteMenuItem({ stages: [] }, () => {}), null);
}

// ---- the resource cards ----
{
  const res = { remotes: [{ id: 'colab:remote', kind: 'colab' }, { id: 'ssh:gpu-box', kind: 'ssh', state: 'busy', machine: { gpu_name: 'x' } }, { id: 'ssh:mac', kind: 'ssh', state: 'offline' }] };
  assert.deepEqual(R.sshCards(res).map((c) => c.id), ['ssh:gpu-box', 'ssh:mac']);
  assert.deepEqual(R.sshCards({}), []);
  assert.deepEqual(R.sshCards(null), []);
  assert.equal(R.cardConnected(res.remotes[1]), true);
  assert.equal(R.cardConnected(res.remotes[2]), false, 'not connected: one line');
  assert.equal(R.cardConnected({ state: 'ready' }), true);
  assert.equal(R.jobStepText('wait_accelerator'), 'connecting', 'the connection step is not "waiting for a GPU"');
  assert.equal(R.jobStepText('upload_audio'), 'upload_audio');
}

// ---- the diagnostics' rows ----
{
  const rows = R.diagRows(ready);
  assert.deepEqual(rows.map((r) => r.key), ['connection', 'agent', 'python', 'gpu', 'model', 'disk']);
  assert.ok(rows.every((r) => r.status === 'ok'), JSON.stringify(rows));
  assert.equal(rows[0].value, 'ready · resources.ssh.latency', 'the state and the latency');
  const m = R.diagRows(mac);
  assert.equal(m[0].status, 'warn');
  assert.equal(m[0].hint, 'remote.offline', 'the last error as the hint (without a catalog: its code)');
  assert.equal(m[1].status, 'warn');
  assert.equal(m[1].value, 'system.remote.agent_old', 'another version than this build');
  assert.equal(m[5].status, 'warn', 'too little free disk');
  const f = R.diagRows(fresh);
  assert.deepEqual(f.map((r) => r.key), ['connection', 'python'], 'never reached: nothing more is known');
  assert.equal(R.diagRows({ ...ready, state: 'auth_failed' })[0].status, 'error');
  const inst = R.diagRows({ ...ready, kit_ready: false, state: 'installing', install: { step: 'model', fraction: 0.5 } });
  assert.match(inst[2].value, /^settings\.remote\.install_model /);
}

// ---- the waiting texts ----
{
  assert.equal(U.waitingForText('remote_offline'), 'waiting.remote_offline');
  assert.equal(U.waitingForText('remote_setup'), 'waiting.remote_setup');
  assert.equal(U.waitingForText('remote_fallback_ssh'), 'waiting.remote_fallback_ssh');
}

console.log('ssh-1b checks: ok');
