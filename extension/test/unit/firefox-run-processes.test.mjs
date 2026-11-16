// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which processes the Firefox tests may stop (test/firefox/run-processes.mjs),
// on made-up process tables: never one the run cannot prove its own (a reused
// stale parent id, a process that was there before, one older than the test,
// missing data), and the Firefox a failed start left behind with what it started.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { runProcesses, isInside, isFirefox } from '../firefox/run-processes.mjs';

const SELF = 4100;
const T0 = Date.UTC(2026, 10, 13, 14, 0, 0); // this test process was created
const row = (ProcessId, ParentProcessId, Name, Created, CommandLine = '') => ({ ProcessId, ParentProcessId, Name, Created, CommandLine });
const self = row(SELF, 4000, 'node.exe', T0);
const DIRS = ['C:\\cache\\tmp\\lf-ext-firefox-abc', 'C:\\cache\\firefox-temp', 'C:\\cache\\153.6.0esr-win64'];
const base = { mainPid: 0, dirs: DIRS, selfPid: SELF, selfStart: T0 };
const stopped = (list) => list.filter((p) => p.stop).map((p) => p.ProcessId).sort((a, b) => a - b);

test('a Firefox this process started after a failed start, and what it started, are stopped', () => {
  const rows = [
    self,
    row(5000, SELF, 'firefox.exe', T0 + 2000), // command line hidden by security software
    row(5001, 5000, 'firefox.exe', T0 + 2500), // content process
    row(5002, 5000, 'crashhelper.exe', T0 + 2100),
  ];
  const list = runProcesses({ ...base, rows, preexisting: new Set([SELF, 4000]) });
  assert.deepEqual(stopped(list), [5000, 5001, 5002]);
  assert.equal(list.find((p) => p.ProcessId === 5000).ours, 'started');
});

test('the user\'s Firefox whose stale parent id equals this process is never stopped', () => {
  // The user's Firefox (launched by a launcher that has exited) carries the
  // launcher's old id, which Windows later gave to this test process.
  const rows = [self, row(700, SELF, 'firefox.exe', T0 - 3600e3), row(701, 700, 'firefox.exe', T0 - 3500e3)];
  // in the snapshot taken before the run started Firefox
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF, 700, 701]) })), []);
  // even if the snapshot somehow missed it: older than this process
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF]) })), []);
  assert.deepEqual(runProcesses({ ...base, rows, preexisting: new Set([SELF]) }), [], 'not even listed as this run\'s');
});

test('a process in the snapshot is not stopped even when it looks new', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000)];
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF, 5000]) })), []);
});

test('a process created before this test process is not stopped', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 - 1)];
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF]) })), []);
});

test('missing data stops nothing: no snapshot, no own start time, no creation time', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000), row(5002, 5000, 'crashhelper.exe', T0 + 2100)];
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: null })), []);
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF]), selfStart: null })), []);
  const noTimes = rows.map((r) => ({ ...r, Created: undefined }));
  assert.deepEqual(stopped(runProcesses({ ...base, rows: noTimes, preexisting: new Set([SELF]) })), []);
  assert.deepEqual(runProcesses({ ...base, rows: undefined, preexisting: new Set() }), []);
});

test('with a main pid the parent-id rule is off: the main Firefox and its tree are listed, not stopped', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000), row(5001, 5000, 'firefox.exe', T0 + 2500)];
  const list = runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF]) });
  assert.deepEqual(list.map((p) => [p.ProcessId, p.ours, p.stop]), [[5000, 'main', false], [5001, 'tree', false]]);
});

test('below a started Firefox, a descendant older than it (stale parent id) is neither listed nor stopped', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000), row(800, 5000, 'firefox.exe', T0 - 60e3)];
  const list = runProcesses({ ...base, rows, preexisting: new Set([SELF]) });
  assert.deepEqual(list.map((p) => p.ProcessId), [5000]);
});

test('a descendant that was there before the run is listed but not stopped', () => {
  // created after this process and after the started Firefox, yet in the
  // snapshot: impossible in practice, and still not stopped
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000), row(5003, 5000, 'crashhelper.exe', T0 + 2100)];
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF, 5003]) })), [5000]);
});

test('a Firefox the user opens during a failed run (another parent) is neither listed nor stopped', () => {
  // new, not in the snapshot, created after this process: only its parent
  // (explorer, a launcher) tells it is not this run's
  const rows = [self, row(900, 1, 'explorer.exe', T0 - 86400e3), row(5100, 900, 'firefox.exe', T0 + 5000), row(5101, 5100, 'firefox.exe', T0 + 5100)];
  const list = runProcesses({ ...base, rows, preexisting: new Set([SELF, 900]) });
  assert.deepEqual(list, []);
});

test('with a main pid, a new firefox whose parent is this process is not stopped by the parent-id rule', () => {
  const rows = [self, row(5000, SELF, 'firefox.exe', T0 + 2000), row(5009, SELF, 'firefox.exe', T0 + 3000)];
  const list = runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF]) });
  assert.deepEqual(stopped(list), []);
  assert.ok(!list.some((p) => p.ProcessId === 5009 && p.ours === 'started'));
});

test('by directory: this run\'s crashhelper is stopped; the test process itself never', () => {
  const rows = [
    { ...self, CommandLine: `node --test ${DIRS[0]}` },
    row(6000, 1, 'crashhelper.exe', T0 + 4000, `crashhelper.exe ${DIRS[0]}\\dumps`),
    row(6001, 1, 'firefox.exe', T0 - 3600e3, 'C:\\Program Files\\Mozilla Firefox\\firefox.exe'), // the user's
  ];
  const list = runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF, 6001]) });
  assert.deepEqual(list.map((p) => [p.ProcessId, p.ours, p.stop]), [[6000, 'dirs', true]]);
});

test('by directory: a shell or script looking at these directories is neither listed nor stopped', () => {
  const rows = [
    self,
    row(6100, 1, 'powershell.exe', T0 + 4000, `powershell -Command Get-ChildItem ${DIRS[1]}`),
    row(6101, 1, 'du.exe', T0 + 4000, `du -sk ${DIRS[0]}`),
    row(6102, 1, 'node.exe', T0 + 4000, `node probe.mjs ${DIRS[2]}`),
  ];
  for (const preexisting of [new Set([SELF]), null]) {
    assert.deepEqual(runProcesses({ ...base, mainPid: 5000, rows, preexisting }), []);
    assert.deepEqual(runProcesses({ ...base, rows, preexisting }), []);
  }
});

test('by directory: the Firefox of another working copy\'s run, started earlier, is neither listed nor stopped', () => {
  const rows = [
    self,
    row(7000, 6900, 'firefox.exe', T0 - 60e3, `${DIRS[2]}\\app\\firefox.exe -profile C:\\cache\\tmp\\lf-ext-firefox-other\\p`),
    row(7001, 7000, 'crashhelper.exe', T0 - 59e3, `crashhelper.exe ${DIRS[1]}`),
  ];
  // in the snapshot and older than this process
  assert.deepEqual(runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF, 7000, 7001]) }), []);
  // not in the snapshot (it cannot be, but still): older than this process
  assert.deepEqual(runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF]) }), []);
  // in the snapshot, its time unknown: listed as maybe this run's, not stopped
  const noTime = rows.map((r) => (r.ProcessId === SELF ? r : { ...r, Created: undefined }));
  assert.deepEqual(stopped(runProcesses({ ...base, mainPid: 5000, rows: noTime, preexisting: new Set([SELF, 7000, 7001]) })), []);
});

test('by directory: without the data to tell, a suite program is listed but not stopped', () => {
  const rows = [self, row(6000, 1, 'crashhelper.exe', undefined, `crashhelper.exe ${DIRS[0]}\\dumps`), row(6002, 1, 'crashhelper.exe', T0 + 4000, `crashhelper.exe ${DIRS[0]}`)];
  for (const [preexisting, selfStart] of [[null, T0], [new Set([SELF]), null]]) {
    const list = runProcesses({ ...base, mainPid: 5000, rows, preexisting, selfStart });
    assert.deepEqual(list.map((p) => [p.ProcessId, p.ours, p.stop]), [[6000, 'dirs', false], [6002, 'dirs', false]]);
  }
  const list = runProcesses({ ...base, mainPid: 5000, rows, preexisting: new Set([SELF]) });
  assert.deepEqual(list.map((p) => [p.ProcessId, p.stop]), [[6000, false], [6002, true]]);
});

test('names: only firefox / firefox.exe', () => {
  assert.ok(isFirefox({ Name: 'firefox.exe' }) && isFirefox({ Name: 'FIREFOX.EXE' }) && isFirefox({ Name: '/usr/lib/firefox/firefox' }));
  assert.ok(!isFirefox({ Name: 'firefox-bin.exe' }) && !isFirefox({ Name: 'notfirefox.exe' }) && !isFirefox({}));
  const rows = [self, row(5000, SELF, 'pingsender.exe', T0 + 2000)];
  assert.deepEqual(stopped(runProcesses({ ...base, rows, preexisting: new Set([SELF]) })), []);
});

test('isInside: strictly below the directory, after normalising', () => {
  for (const api of [path.win32, path.posix]) {
    const d = api === path.win32 ? 'C:\\c\\tmp\\lf-ext-firefox-abc' : '/c/tmp/lf-ext-firefox-abc';
    const j = (...p) => api.join(d, ...p);
    assert.ok(isInside(j('firefox-profileX'), d, api));
    assert.ok(isInside(j('a', '..', 'firefox-profileX'), d, api));
    assert.ok(!isInside(d, d, api), 'the directory itself');
    assert.ok(!isInside(j('..', 'other'), d, api));
    assert.ok(!isInside(`${d}-sibling`, d, api));
    assert.ok(!isInside(j('..'), d, api));
    assert.ok(!isInside('', d, api) && !isInside(j('x'), '', api));
  }
});
