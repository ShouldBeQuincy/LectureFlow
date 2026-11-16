// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

// Closing the window (SPEC-v3 V1.1; T26 S1, J10): the shell controller
// with a stand-in electron module, so the dialog it shows and what it does
// with the answer can be checked without a real window.

const test = require('node:test');
const assert = require('node:assert/strict');
const Module = require('node:module');

const rec = { quits: 0, dialogs: [], notes: [] };
let answer = { response: 2, checkboxChecked: false };

class FakeTray {
  constructor() {
    this.menu = null;
    this.destroyed = false;
    FakeTray.last = this;
  }
  on() {}
  setContextMenu(menu) { this.menu = menu; }
  setToolTip() {}
  destroy() { this.destroyed = true; }
}

class FakeNotification {
  constructor(opts) { this.opts = opts; }
  static isSupported() { return true; }
  on() {}
  show() { rec.notes.push(this.opts); }
}

const electron = {
  app: { quit: () => { rec.quits++; } },
  BrowserWindow: { getAllWindows: () => [] },
  dialog: {
    showMessageBox: async (...args) => {
      rec.dialogs.push(args);
      return answer;
    },
  },
  nativeImage: { createFromBuffer: () => ({ addRepresentation() {} }) },
  nativeTheme: { shouldUseDarkColors: false },
  Notification: FakeNotification,
  powerSaveBlocker: { start: () => 1, stop() {}, isStarted: () => false },
  screen: {},
  session: {},
  shell: {},
  Menu: { buildFromTemplate: (template) => ({ template }) },
  Tray: FakeTray,
  ipcMain: { handle() {}, on() {} },
  powerMonitor: {},
};

const load = Module._load;
Module._load = function (request, ...rest) {
  if (request === 'electron') return electron;
  return load.call(this, request, ...rest);
};
let Shell;
try {
  ({ Shell } = require('../../src/main/shell'));
} finally {
  Module._load = load;
}

function fakeWindow({ visible = true } = {}) {
  const w = {
    visible,
    shows: 0,
    isDestroyed: () => false,
    isVisible: () => w.visible,
    isMinimized: () => false,
    hide: () => { w.visible = false; },
    show: () => { w.shows++; w.visible = true; },
    restore() {},
    focus() {},
  };
  return w;
}

/**
 * makeShell(activity, settings) → a Shell whose service is ready and
 * answers /api/app/activity and /api/app/settings with these values.
 */
function makeShell({ activity, settings = { close_behavior: 'ask' }, visible = true, phase = 'ready' }) {
  rec.quits = 0;
  rec.dialogs = [];
  rec.notes = [];
  answer = { response: 2, checkboxChecked: false };
  const logs = [];
  const log = { info: (m) => logs.push(m), warn: (m) => logs.push(m), error: (m) => logs.push(m) };
  const sh = new Shell({ translator: { t: (k) => k, use: () => false, locale: 'en' }, log, isDev: false });
  sh.platform = 'win32';
  sh.phase = phase;
  sh.win = fakeWindow({ visible });
  sh.patches = [];
  sh.api = {
    activity: async () => activity,
    settings: async () => settings,
    patchSettings: async (p) => { sh.patches.push(p); },
  };
  sh.logs = logs;
  return sh;
}

// Lets setImmediate callbacks and the awaited fakes run (bounded).
async function settle() {
  for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
}

test('closing the window with lecture jobs: the usual question, on the window', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 2, downloads: 0 } });
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1);
  const [parent, opts] = rec.dialogs[0];
  assert.ok(parent === sh.win, 'the visible window is the parent');
  assert.equal(opts.message, 'close.message');
  assert.equal(opts.detail, 'close.detail');
  assert.deepEqual(opts.buttons, ['close.background', 'close.quit', 'close.cancel']);
  assert.equal(opts.checkboxLabel, 'close.remember');
  assert.equal(rec.quits, 0, 'cancel');
  assert.ok(sh.logs.some((m) => m.startsWith('close requested: ask (running 1)')), sh.logs.join('\n'));
});

test('closing the window while recordings are fetched says how to resume them (T26 S1)', async () => {
  for (const [activity, message, detail] of [
    [{ running: 1, queued: 0, downloads: 1 }, 'close.message', 'close.detail_downloads'],
    [{ running: 0, queued: 3, downloads: 3 }, 'close.message_queued', 'close.detail_downloads'],
    [{ running: 2, queued: 1, downloads: 2 }, 'close.message', 'close.detail_both'],
  ]) {
    const sh = makeShell({ activity });
    await sh.onCloseRequested();
    assert.equal(rec.dialogs.length, 1, JSON.stringify(activity));
    const opts = rec.dialogs[0][1];
    assert.equal(opts.message, message, JSON.stringify(activity));
    assert.equal(opts.detail, detail, JSON.stringify(activity));
  }
});

test('closing the window with only queued jobs asks, and says they are queued (T26 J10)', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 2, downloads: 0 } });
  answer = { response: 1, checkboxChecked: false };
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1);
  assert.equal(rec.dialogs[0][1].message, 'close.message_queued');
  assert.equal(rec.dialogs[0][1].detail, 'close.detail');
  assert.equal(rec.quits, 1, 'answered "quit"');
  assert.ok(sh.logs.some((m) => m.startsWith('close requested: ask (running 0) (queued 2)')), sh.logs.join('\n'));
});

test('closing the window: "continue in background" + remember hides it to the tray', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 } });
  answer = { response: 0, checkboxChecked: true };
  await sh.onCloseRequested();
  assert.deepEqual(sh.patches, [{ close_behavior: 'background' }]);
  assert.equal(sh.win.visible, false);
  assert.ok(sh.tray.visible);
  assert.equal(rec.notes.length, 1);
  assert.equal(rec.quits, 0);
});

test('closing the window without tasks quits without asking', async () => {
  for (const activity of [{ running: 0, queued: 0, downloads: 0 }, null]) {
    const sh = makeShell({ activity });
    await sh.onCloseRequested();
    assert.equal(rec.dialogs.length, 0);
    assert.equal(rec.quits, 1);
  }
});

// ---- Quit in the tray menu (T26 S1) ----

/** clickTrayQuit(sh): the window is in the background; click "Quit" in the tray's menu. */
async function clickTrayQuit(sh) {
  sh.tray.show();
  const items = FakeTray.last.menu.template;
  const quit = items.find((i) => i.label === 'tray.quit');
  assert.ok(quit, 'the tray menu has Quit');
  quit.click();
  assert.equal(rec.quits, 0, 'nothing happens inside the menu click itself');
  await settle();
}

test('tray Quit with tasks asks the close question, without a parent window while hidden', async () => {
  for (const [activity, message, detail] of [
    [{ running: 1, queued: 0, downloads: 0 }, 'close.message', 'close.detail'],
    [{ running: 0, queued: 2, downloads: 0 }, 'close.message_queued', 'close.detail'],
    [{ running: 1, queued: 1, downloads: 2 }, 'close.message', 'close.detail_downloads'],
    [{ running: 2, queued: 0, downloads: 1 }, 'close.message', 'close.detail_both'],
  ]) {
    const sh = makeShell({ activity, visible: false });
    await clickTrayQuit(sh);
    assert.equal(rec.dialogs.length, 1, JSON.stringify(activity));
    const args = rec.dialogs[0];
    assert.equal(args.length, 1, 'no parent: the window is hidden');
    assert.equal(args[0].message, message);
    assert.equal(args[0].detail, detail);
    assert.deepEqual(args[0].buttons, ['close.background', 'close.quit', 'close.cancel']);
    // review R69 L-2: "remember my choice" is about closing the window
    assert.equal(args[0].checkboxLabel, undefined, 'no "remember my choice" from the tray');
    assert.equal(args[0].checkboxChecked, undefined);
    assert.equal(rec.quits, 0, 'cancelled');
    assert.equal(sh.win.shows, 0, 'the window stays hidden');
    assert.ok(sh.tray.visible, 'the tray icon stays');
    assert.ok(sh.logs.some((m) => m.startsWith('quit requested: ask (running ')), sh.logs.join('\n'));
  }
});

test('tray Quit: answering "quit" quits; "continue in background" keeps it in the tray; nothing is remembered', async () => {
  // checkboxChecked: true stands for a dialog that would still carry the
  // box: the tray's answer must not change how closing the window behaves
  let sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 }, settings: { close_behavior: 'background' }, visible: false });
  answer = { response: 1, checkboxChecked: true };
  sh.tray.show();
  FakeTray.last.menu.template.find((i) => i.label === 'tray.quit').click();
  await settle();
  assert.equal(rec.quits, 1);
  assert.deepEqual(sh.patches, [], '"quit" from the tray does not become the close behaviour');

  sh = makeShell({ activity: { running: 1, queued: 0, downloads: 0 }, visible: false });
  sh.tray.show();
  answer = { response: 0, checkboxChecked: true };
  FakeTray.last.menu.template.find((i) => i.label === 'tray.quit').click();
  await settle();
  assert.equal(rec.quits, 0);
  assert.deepEqual(sh.patches, [], 'nothing is remembered from the tray');
  assert.equal(sh.win.shows, 0);
  assert.ok(sh.tray.visible);
});

test('tray Quit: a remembered "background" still asks; a remembered "quit" quits at once', async () => {
  let sh = makeShell({ activity: { running: 1, queued: 0, downloads: 0 }, settings: { close_behavior: 'background' }, visible: false });
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 1, 'an explicit Quit is never swallowed by "background"');
  assert.equal(rec.quits, 0);

  sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 }, settings: { close_behavior: 'quit' }, visible: false });
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 1);
});

test('tray Quit without tasks, or before the service is ready, quits at once', async () => {
  for (const [activity, phase] of [[{ running: 0, queued: 0, downloads: 0 }, 'ready'], [null, 'ready'], [{ running: 3, queued: 0 }, 'starting'], [{ running: 3, queued: 0 }, 'failed']]) {
    const sh = makeShell({ activity, phase, visible: false });
    await clickTrayQuit(sh);
    assert.equal(rec.dialogs.length, 0, `${phase} ${JSON.stringify(activity)}`);
    assert.equal(rec.quits, 1, `${phase} ${JSON.stringify(activity)}`);
  }
});

// Review R69 L-1: the tray's question has no parent window. When the window
// is called back meanwhile (tray icon, a second start, the extension's
// "open"), the question is withdrawn as cancelled, so closing the window
// asks again instead of doing nothing.
test('the window called back while the tray question is open: it is withdrawn, closing asks again', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 }, visible: false });
  const shown = electron.dialog.showMessageBox;
  electron.dialog.showMessageBox = (...args) => {
    rec.dialogs.push(args);
    const opts = args[args.length - 1];
    // stays open until its signal (if any) aborts it, as Electron does on Windows
    return new Promise((resolve) => {
      if (opts.signal) opts.signal.addEventListener('abort', () => resolve({ response: opts.cancelId, checkboxChecked: true }), { once: true });
    });
  };
  try {
    await clickTrayQuit(sh);
    assert.equal(rec.dialogs.length, 1);
    const trayOpts = rec.dialogs[0][0];
    assert.ok(trayOpts.signal, 'the tray question can be withdrawn');
    assert.equal(trayOpts.signal.aborted, false);
    sh.showMainWindow();
    await settle();
    assert.equal(sh.win.visible, true);
    assert.equal(trayOpts.signal.aborted, true, 'withdrawn when the window is shown');
    assert.equal(rec.quits, 0, 'withdrawn = cancelled');
    assert.deepEqual(sh.patches, [], 'nothing remembered');
    assert.ok(sh.logs.some((m) => m.includes('withdrawn')), sh.logs.join('\n'));

    electron.dialog.showMessageBox = async (...args) => {
      rec.dialogs.push(args);
      return { response: 1, checkboxChecked: false };
    };
    await sh.onCloseRequested();
    assert.equal(rec.dialogs.length, 2, 'closing the window asks');
    assert.ok(rec.dialogs[1][0] === sh.win, 'on the window');
    assert.equal(rec.dialogs[1][1].signal, undefined, 'the window question is modal; nothing withdraws it');
    assert.equal(rec.quits, 1);
  } finally {
    electron.dialog.showMessageBox = shown;
  }
});

test('the window called back while the service is still being asked: no tray question, no quit', async () => {
  const sh = makeShell({ activity: null, visible: false });
  let answerActivity;
  sh.api.activity = () => new Promise((r) => { answerActivity = r; });
  sh.tray.show();
  FakeTray.last.menu.template.find((i) => i.label === 'tray.quit').click();
  await settle();
  assert.equal(typeof answerActivity, 'function', 'the service is being asked');
  sh.showMainWindow();
  answerActivity({ running: 1, queued: 0, downloads: 0 });
  await settle();
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 0);
  assert.ok(sh.logs.some((m) => m.includes('withdrawn')), sh.logs.join('\n'));
});

// Review R69 L-6, mutation M8: once the shell is stopping, Quit does nothing
// more (no request to a service being stopped, no question, no second quit).
test('tray Quit while the shell is already quitting does nothing', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 }, visible: false });
  let asked = 0;
  sh.api.activity = async () => {
    asked++;
    return { running: 1, queued: 0, downloads: 1 };
  };
  sh.tray.show();
  const quit = FakeTray.last.menu.template.find((i) => i.label === 'tray.quit');
  sh.quitting = true;
  quit.click();
  await settle();
  assert.equal(asked, 0, 'the service is not asked');
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 0);
});

// ---- the service does not answer (review R69 L-7, r8 BC-04) ----

/**
 * withClock(sh): the shell's clock (milliseconds) moved by the test;
 * returns advance(ms). poll() runs against the fake service (uiSettings
 * answers nothing).
 */
function withClock(sh) {
  let now = 5_000_000;
  sh.now = () => now;
  sh.api.uiSettings = async () => null;
  return (ms) => { now += ms; };
}

function failActivity(sh) {
  sh.api.activity = async () => { throw new Error('GET /api/app/activity: timed out'); };
}

test('closing the window while the service does not answer: a reading of 10 s ago with tasks asks', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 } });
  const advance = withClock(sh);
  await sh.poll();
  advance(10_000);
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  const [parent, opts] = rec.dialogs[0];
  assert.ok(parent === sh.win, 'on the window');
  assert.equal(opts.message, 'close.message');
  assert.equal(opts.detail, 'close.detail_downloads', 'the dialog describes the reading');
  assert.equal(rec.quits, 0, 'cancelled');
  assert.ok(sh.logs.some((m) => m === 'close requested: ask (running 1) (queued 0) (activity from 10 s ago)'), sh.logs.join('\n'));
});

test('tray Quit while the service does not answer: a reading of 10 s ago with tasks asks', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 2, downloads: 0 }, visible: false });
  const advance = withClock(sh);
  await sh.poll();
  advance(10_000);
  failActivity(sh);
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.equal(rec.dialogs[0].length, 1, 'no parent: the window is hidden');
  assert.equal(rec.dialogs[0][0].message, 'close.message_queued');
  assert.equal(rec.quits, 0);
  assert.ok(sh.logs.some((m) => m === 'quit requested: ask (running 0) (queued 2) (activity from 10 s ago)'), sh.logs.join('\n'));
});

// The detail of an unknown activity names both kinds of task (review R84
// L-3): a fetch that may be running loses its sign-in data on quit.
test('the service does not answer and nothing was read: closing and tray Quit ask (activity unknown)', async () => {
  let sh = makeShell({ activity: null });
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.equal(rec.dialogs[0][1].message, 'close.message');
  assert.equal(rec.dialogs[0][1].detail, 'close.detail_both');
  assert.equal(rec.quits, 0);
  assert.ok(sh.logs.some((m) => m === 'close requested: ask (activity unknown)'), sh.logs.join('\n'));
  assert.ok(!sh.logs.some((m) => m.includes('(running ')), 'not logged as counts');

  sh = makeShell({ activity: null, visible: false });
  failActivity(sh);
  answer = { response: 1, checkboxChecked: false };
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.equal(rec.dialogs[0][0].message, 'close.message');
  assert.equal(rec.dialogs[0][0].detail, 'close.detail_both');
  assert.equal(rec.quits, 1, 'answered "quit"');
  assert.ok(sh.logs.some((m) => m === 'quit requested: ask (activity unknown)'), sh.logs.join('\n'));
});

test('a reading older than about 30 s does not count: the activity is unknown and the shell asks', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 0, downloads: 0 } });
  const advance = withClock(sh);
  await sh.poll();
  advance(31_000);
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.equal(rec.quits, 0);
  assert.ok(sh.logs.some((m) => m === 'close requested: ask (activity unknown)'), sh.logs.join('\n'));
});

test('a recent reading without tasks quits at once; the log says how old the reading is', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 0, downloads: 0 }, visible: false });
  const advance = withClock(sh);
  await sh.poll();
  advance(4_000);
  failActivity(sh);
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 1);
  assert.ok(sh.logs.some((m) => m === 'quit requested: quit (running 0) (queued 0) (activity from 4 s ago)'), sh.logs.join('\n'));
});

test('the service answers that nothing runs: closing quits at once, logged as no tasks', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 0, downloads: 0 } });
  withClock(sh);
  await sh.poll();
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 1);
  assert.ok(sh.logs.some((m) => m === 'close requested: quit (running 0) (queued 0)'), sh.logs.join('\n'));
});

// Review R84 L-2: an answer at close time always wins over poll()'s reading.
test('the activity answers at close time: an older reading with tasks is not used', async () => {
  for (const tray of [false, true]) {
    const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 1 }, visible: !tray });
    const advance = withClock(sh);
    await sh.poll();
    advance(3_000);
    sh.api.activity = async () => ({ running: 0, queued: 0, downloads: 0 });
    if (tray) await clickTrayQuit(sh);
    else await sh.onCloseRequested();
    const what = tray ? 'quit' : 'close';
    assert.equal(rec.dialogs.length, 0, `${what}: ${sh.logs.join('\n')}`);
    assert.equal(rec.quits, 1, what);
    assert.ok(sh.logs.some((m) => m === `${what} requested: quit (running 0) (queued 0)`), sh.logs.join('\n'));
    assert.ok(!sh.logs.some((m) => m.includes('activity from')), 'the old reading is not mentioned');
  }
});

// Review R84 L-2: the service starts again while poll() waits for its
// answer; that answer belongs to the stopped service and is not kept.
test('an answer poll() receives after the service restarted is not kept', async () => {
  const sh = makeShell({ activity: null });
  const advance = withClock(sh);
  let answerActivity;
  sh.api.activity = () => new Promise((r) => { answerActivity = r; });
  const polled = sh.poll();
  await settle();
  assert.equal(typeof answerActivity, 'function', 'poll() is waiting for the service');
  sh.generation++; // the service was started again
  answerActivity({ running: 0, queued: 0, downloads: 0 });
  await polled;
  advance(2_000);
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.equal(rec.quits, 0);
  assert.ok(sh.logs.some((m) => m === 'close requested: ask (activity unknown)'), sh.logs.join('\n'));
});

test('a reading from before the service restarted does not count', async () => {
  const sh = makeShell({ activity: { running: 0, queued: 0, downloads: 0 } });
  const advance = withClock(sh);
  await sh.poll();
  sh.generation++; // the service was started again
  advance(2_000);
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 1, sh.logs.join('\n'));
  assert.ok(sh.logs.some((m) => m === 'close requested: ask (activity unknown)'), sh.logs.join('\n'));
});

test('activity unknown: a remembered "quit" still quits at once, a remembered "background" hides the window', async () => {
  // the settings answer although the activity does not
  let sh = makeShell({ activity: null, settings: { close_behavior: 'quit' } });
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 1);
  assert.ok(sh.logs.some((m) => m === 'close requested: quit (activity unknown)'), sh.logs.join('\n'));

  sh = makeShell({ activity: null, settings: { close_behavior: 'quit' }, visible: false });
  failActivity(sh);
  await clickTrayQuit(sh);
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 1);

  sh = makeShell({ activity: null, settings: { close_behavior: 'background' } });
  failActivity(sh);
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 0);
  assert.equal(rec.quits, 0);
  assert.equal(sh.win.visible, false, 'hidden to the tray');

  // neither answers: the settings the last poll read decide
  sh = makeShell({ activity: { running: 1, queued: 0, downloads: 0 }, settings: { close_behavior: 'quit' } });
  const advance = withClock(sh);
  await sh.poll();
  advance(60_000);
  failActivity(sh);
  sh.api.settings = async () => { throw new Error('GET /api/app/settings: timed out'); };
  await sh.onCloseRequested();
  assert.equal(rec.dialogs.length, 0, sh.logs.join('\n'));
  assert.equal(rec.quits, 1);
  assert.ok(sh.logs.some((m) => m === 'close requested: quit (activity unknown)'), sh.logs.join('\n'));
});

test('tray Quit while the question is open does not ask twice', async () => {
  const sh = makeShell({ activity: { running: 1, queued: 0, downloads: 0 }, visible: false });
  let release;
  const shown = electron.dialog.showMessageBox;
  electron.dialog.showMessageBox = (...args) => {
    rec.dialogs.push(args);
    return new Promise((r) => { release = r; });
  };
  try {
    sh.tray.show();
    const quit = FakeTray.last.menu.template.find((i) => i.label === 'tray.quit');
    quit.click();
    await settle();
    quit.click();
    await settle();
    assert.equal(rec.dialogs.length, 1);
    release({ response: 1, checkboxChecked: false });
    await settle();
    assert.equal(rec.quits, 1);
  } finally {
    electron.dialog.showMessageBox = shown;
  }
});
