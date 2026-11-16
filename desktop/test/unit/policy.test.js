// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const {
  decideClose, decideQuit, closeDialogKeys, pendingWork, interpretCloseDialog, wantSleepBlock, runningCount, queuedCount,
} = require('../../src/main/lib/policy');
const { SleepGuard, BLOCK_TYPE } = require('../../src/main/sleepguard');

const act = (running, queued = 0) => ({ running, queued, transcribing: running > 0, api_running: false });

test('close without running or queued tasks: quit on Windows, stay in the Dock on macOS', () => {
  for (const settings of [null, { close_behavior: 'ask' }, { close_behavior: 'background' }, { close_behavior: 'quit' }]) {
    assert.equal(decideClose({ platform: 'win32', activity: act(0), settings }), 'quit');
    assert.equal(decideClose({ platform: 'darwin', activity: act(0), settings }), 'hide');
    assert.equal(decideClose({ platform: 'win32', activity: null, settings }), 'quit', 'service unreachable');
  }
});

// T26 J10: queued tasks are unfinished work too. Queued lecture jobs wait
// for the next start (SPEC §4.7 leaves them alone), but queued fetches of
// recordings lose their credentials, so closing asks as for running ones.
test('close with only queued tasks follows close_behavior like running ones (T26 J10)', () => {
  for (const platform of ['win32', 'darwin']) {
    assert.equal(decideClose({ platform, activity: act(0, 3), settings: { close_behavior: 'ask' } }), 'ask');
    assert.equal(decideClose({ platform, activity: act(0, 3), settings: null }), 'ask', 'unknown settings ask');
    assert.equal(decideClose({ platform, activity: act(0, 1), settings: { close_behavior: 'background' } }), 'background');
    assert.equal(decideClose({ platform, activity: act(0, 1), settings: { close_behavior: 'quit' } }), 'quit');
    const waitingFetch = { running: 0, queued: 1, downloads: 1, transcribing: false, api_running: false };
    assert.equal(decideClose({ platform, activity: waitingFetch, settings: { close_behavior: 'ask' } }), 'ask', 'a waiting fetch');
  }
  assert.equal(queuedCount({ queued: 2.7 }), 2);
  assert.equal(queuedCount({ queued: -1 }), 0);
  assert.equal(queuedCount({ queued: '3' }), 0);
  assert.equal(queuedCount(null), 0);
});

// T26 S1: quitting from the tray asks the same question as closing the
// window, but an explicit Quit never turns into "stay in the background".
test('quitting from the tray: no tasks quits, a remembered "quit" quits, anything else asks', () => {
  for (const settings of [null, { close_behavior: 'ask' }, { close_behavior: 'background' }, { close_behavior: 'quit' }]) {
    assert.equal(decideQuit({ activity: act(0), settings }), 'quit');
    assert.equal(decideQuit({ activity: null, settings }), 'quit', 'service unreachable');
  }
  for (const activity of [act(1), act(0, 2), { running: 1, queued: 0, downloads: 1 }]) {
    assert.equal(decideQuit({ activity, settings: { close_behavior: 'ask' } }), 'ask');
    assert.equal(decideQuit({ activity, settings: { close_behavior: 'background' } }), 'ask', 'a remembered "background" still asks');
    assert.equal(decideQuit({ activity, settings: null }), 'ask');
    assert.equal(decideQuit({ activity, settings: { close_behavior: 'bogus' } }), 'ask');
    assert.equal(decideQuit({ activity, settings: { close_behavior: 'quit' } }), 'quit');
  }
  // a device self-test is not a task
  assert.equal(decideQuit({ activity: { running: 0, queued: 0, transcribing: true }, settings: { close_behavior: 'ask' } }), 'quit');
});

// T26 S1: fetches of recordings do not survive a restart (their credentials
// are in memory only), lecture and course jobs do; the dialog says which.
test('close dialog text: lecture jobs, recordings being fetched, or both', () => {
  const keys = (running, queued, downloads) => closeDialogKeys({ running, queued, downloads });
  assert.deepEqual(keys(2, 1, 0), { message: 'close.message', detail: 'close.detail' }, 'jobs only');
  assert.deepEqual(keys(0, 2, 0), { message: 'close.message_queued', detail: 'close.detail' }, 'queued jobs only');
  assert.deepEqual(keys(1, 0, 1), { message: 'close.message', detail: 'close.detail_downloads' }, 'one fetch only');
  assert.deepEqual(keys(1, 2, 3), { message: 'close.message', detail: 'close.detail_downloads' }, 'fetches only');
  assert.deepEqual(keys(0, 1, 1), { message: 'close.message_queued', detail: 'close.detail_downloads' }, 'a waiting fetch only');
  assert.deepEqual(keys(2, 0, 1), { message: 'close.message', detail: 'close.detail_both' }, 'a job and a fetch');
  assert.deepEqual(keys(1, 1, 1), { message: 'close.message', detail: 'close.detail_both' }, 'a fetch and a queued job');
  // an older service without the downloads field: lecture jobs
  assert.deepEqual(closeDialogKeys({ running: 1, queued: 0 }), { message: 'close.message', detail: 'close.detail' });
  // a downloads count above the total is capped (never "both" without a job)
  assert.deepEqual(keys(1, 0, 5), { message: 'close.message', detail: 'close.detail_downloads' });
  assert.deepEqual(pendingWork({ running: 2, queued: 3, downloads: 4 }), { running: 2, queued: 3, downloads: 4, jobs: 1 });
  // the cap itself (review R69 L-6, mutation M3): never more fetches than tasks, never negative jobs
  assert.deepEqual(pendingWork({ running: 1, queued: 0, downloads: 5 }), { running: 1, queued: 0, downloads: 1, jobs: 0 });
  assert.deepEqual(pendingWork({ running: 0, queued: 0, downloads: 2 }), { running: 0, queued: 0, downloads: 0, jobs: 0 });
  assert.deepEqual(pendingWork(null), { running: 0, queued: 0, downloads: 0, jobs: 0 });
});

test('close with running tasks follows close_behavior', () => {
  for (const platform of ['win32', 'darwin']) {
    assert.equal(decideClose({ platform, activity: act(2), settings: { close_behavior: 'ask' } }), 'ask');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'background' } }), 'background');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'quit' } }), 'quit');
    assert.equal(decideClose({ platform, activity: act(1), settings: null }), 'ask', 'unknown settings ask');
    assert.equal(decideClose({ platform, activity: act(1), settings: { close_behavior: 'bogus' } }), 'ask');
  }
});

test('close dialog: buttons and "remember my choice"', () => {
  assert.deepEqual(interpretCloseDialog({ response: 0, checkboxChecked: false }), { action: 'background', remember: null });
  assert.deepEqual(interpretCloseDialog({ response: 0, checkboxChecked: true }), { action: 'background', remember: 'background' });
  assert.deepEqual(interpretCloseDialog({ response: 1, checkboxChecked: true }), { action: 'quit', remember: 'quit' });
  assert.deepEqual(interpretCloseDialog({ response: 2, checkboxChecked: true }), { action: 'cancel', remember: null }, 'cancel never saves');
  assert.deepEqual(interpretCloseDialog({ response: 7 }), { action: 'cancel', remember: null });
  assert.deepEqual(interpretCloseDialog(null), { action: 'cancel', remember: null });
});

test('sleep is blocked only while tasks run and the setting allows it', () => {
  assert.equal(wantSleepBlock(act(1), { prevent_sleep: true }), true);
  assert.equal(wantSleepBlock(act(1), null), true, 'default on');
  assert.equal(wantSleepBlock(act(1), { prevent_sleep: false }), false);
  assert.equal(wantSleepBlock(act(0, 5), { prevent_sleep: true }), false, 'queued only');
  assert.equal(wantSleepBlock(null, { prevent_sleep: true }), false);
  assert.equal(runningCount({ running: -1 }), 0);
  assert.equal(runningCount({ running: '3' }), 0);
});

test('a device self-test (transcribing, no running job) blocks sleep too, under prevent_sleep', () => {
  const selfTest = { running: 0, queued: 0, transcribing: true, api_running: false };
  assert.equal(wantSleepBlock(selfTest, { prevent_sleep: true }), true);
  assert.equal(wantSleepBlock(selfTest, null), true, 'default on');
  assert.equal(wantSleepBlock(selfTest, {}), true, 'setting missing: default on');
  assert.equal(wantSleepBlock(selfTest, { prevent_sleep: false }), false);
  assert.equal(wantSleepBlock({ ...selfTest, queued: 4 }, { prevent_sleep: false }), false);
  // not transcribing and nothing running: no block
  assert.equal(wantSleepBlock({ running: 0, queued: 2, transcribing: false, api_running: true }, { prevent_sleep: true }), false);
  assert.equal(wantSleepBlock({ running: 0 }, { prevent_sleep: true }), false, 'transcribing absent');
  // the self-test is not a running task: closing is unchanged
  assert.equal(decideClose({ platform: 'win32', activity: selfTest, settings: { close_behavior: 'ask' } }), 'quit');
  assert.equal(runningCount(selfTest), 0);
});

test('SleepGuard starts once, stops when no longer wanted', () => {
  const calls = [];
  let next = 7;
  const started = new Set();
  const blocker = {
    start: (type) => { calls.push(['start', type]); started.add(next); return next++; },
    stop: (id) => { calls.push(['stop', id]); started.delete(id); },
    isStarted: (id) => started.has(id),
  };
  const g = new SleepGuard({ blocker });
  g.update(false);
  g.update(true);
  g.update(true);
  assert.equal(g.active, true);
  g.update(false);
  g.update(false);
  assert.equal(g.active, false);
  g.update(true);
  g.release();
  g.release();
  assert.deepEqual(calls, [['start', BLOCK_TYPE], ['stop', 7], ['start', BLOCK_TYPE], ['stop', 8]]);
  assert.equal(BLOCK_TYPE, 'prevent-app-suspension');
});
