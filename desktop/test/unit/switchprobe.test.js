// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The start check of the packaged app against the real Electron binary
// (review R44: Electron appends --allow-file-access-from-files to the main
// process's command line before main.js runs, so asking
// app.commandLine.hasSwitch about it refused every start; stand-ins of
// hasSwitch could not show that). Needs the electron package, like
// perfvisibility.test.js.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const electron = require('electron'); // the binary's path when required from Node
const PROBE = path.join(__dirname, '..', 'fixtures', 'switchprobe.js');

function probe(args, extraEnv = {}) {
  const userData = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-switchprobe-'));
  const env = { ...process.env, LF_PROBE_USERDATA: userData };
  const drop = ['ELECTRON_RUN_AS_NODE', 'ELECTRON_DISABLE_SANDBOX', 'LECTUREFLOW_E2E_ALLOW_DEBUG', 'ELECTRON_ENABLE_LOGGING'];
  for (const k of Object.keys(env)) if (drop.includes(k.toUpperCase())) delete env[k];
  Object.assign(env, extraEnv);
  try {
    const r = spawnSync(electron, [PROBE, ...args], { env, encoding: 'utf8', timeout: 60000, killSignal: 'SIGKILL', windowsHide: true });
    const line = String(r.stdout || '').split(/\r?\n/).find((l) => l.startsWith('SWITCHPROBE '));
    assert.ok(line, `no probe output (status ${r.status}, signal ${r.signal}): ${String(r.stderr || '').slice(0, 500)}`);
    const out = JSON.parse(line.slice('SWITCHPROBE '.length));
    assert.ok(!out.error, `probe failed: ${out.error}`);
    return out;
  } finally {
    fs.rmSync(userData, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
  }
}

test('real Electron: a plain start and the app\'s own switches pass', () => {
  const plain = probe([]);
  assert.equal(plain.electronAdded, true, 'Electron still appends --allow-file-access-from-files (else this test proves less)');
  assert.deepEqual(plain.refused, []);
  assert.deepEqual(probe(['--updated', '--background']).refused, []);
});

test('real Electron: a switch of the person\'s own is still refused', () => {
  assert.deepEqual(probe(['--lf-probe-unknown']).refused, ['lf-probe-unknown']);
});

// T14 finding 2: the app reads ELECTRON_DISABLE_SANDBOX as Electron does
// (Electron appends --no-sandbox exactly when it turns the sandbox off).
// The whole start check (startRefusal) refuses exactly when Electron turned
// the sandbox off, and the app's own reading of the variable agrees with
// Electron on every platform the tests run on (review R54 S-1, S-2).
test('real Electron: ELECTRON_DISABLE_SANDBOX is read as Electron reads it', () => {
  const plain = probe([]);
  assert.equal(plain.noSandbox, false, 'hasSwitch(\'no-sandbox\') is false without the variable (else every start would be refused)');
  assert.equal(plain.sandboxEnv, false);
  assert.equal(plain.refusal, null, 'a plain start passes the whole check');
  assert.equal(probe(['--updated', '--background']).refusal, null);
  for (const v of ['1', '0', 'false']) {
    const r = probe([], { ELECTRON_DISABLE_SANDBOX: v });
    assert.equal(r.noSandbox, true, `${JSON.stringify(v)}: Electron turns the sandbox off whatever the value`);
    assert.equal(r.sandboxEnv, true, JSON.stringify(v));
    assert.equal(r.refusal, 'sandbox', JSON.stringify(v));
    assert.deepEqual(r.refused, [], `${JSON.stringify(v)}: the switch Electron appends is not the person's own`);
  }
  // an empty value and a lower-case name: what Electron does differs by
  // platform (measured on Windows: an empty value leaves the sandbox on, a
  // lower-case name turns it off); only agreement is asserted, so the CI
  // runs on macOS and Linux check the rule there
  for (const env of [{ ELECTRON_DISABLE_SANDBOX: '' }, { electron_disable_sandbox: '1' }]) {
    const r = probe([], env);
    assert.equal(r.sandboxEnv, r.noSandbox, `${process.platform} ${JSON.stringify(env)}: the app and Electron agree`);
    assert.equal(r.refusal, r.noSandbox ? 'sandbox' : null, `${process.platform} ${JSON.stringify(env)}`);
  }
});

test('real Electron: --no-sandbox on the command line is refused as an option', () => {
  const r = probe(['--no-sandbox']);
  assert.deepEqual(r.refused, ['no-sandbox']);
  assert.equal(r.sandboxEnv, false);
  assert.equal(r.refusal, 'args');
});
