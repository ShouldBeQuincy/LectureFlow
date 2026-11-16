// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Command-line switches of the packaged app (SPEC-v3 V2, acceptance V12.2
// finding 11, security audit FB1 L-5). The Electron fuses turn off Node's
// --inspect, but Chromium's own switches stay available: whoever starts
// LectureFlow with --remote-debugging-port could drive the window over the
// DevTools protocol, --log-net-log writes the requests that carry the
// access token to a file, --proxy-server sends them elsewhere,
// --load-extension, --no-sandbox, --disable-web-security and many more
// weaken the app. A list of the dangerous ones is never complete, so the
// packaged app starts only with the switches it uses itself
// (PACKAGED_SWITCHES) and refuses every other one.
//
// LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets them through (acceptance runs drive the
// packaged app over CDP, scripts/smoke.js checks it). Like the command line
// itself it can only be set by a local user who starts the program, so it is
// a test entry, not a security boundary.
//
// The packaged app also refuses to start when its environment turns the
// sandbox off (ELECTRON_DISABLE_SANDBOX, sandboxDisabledByEnv and
// electronAddedNoSandbox), even with LECTUREFLOW_E2E_ALLOW_DEBUG=1, with the
// same message box and exit code as for a switch. startRefusal decides
// both and which box; main.js only shows it.
//
// The packaged app also drops the development switches of its environment
// (devEnvToDrop): variables that only development builds and tests read, and
// the service's LECTUREFLOW_DEV_* switches (security review sec2-ext L-C).
//
// No Electron imports.
'use strict';

// The switches the packaged app starts with, and how:
//   value  'any'      as before (the app's own switches)
//          'none'     only without a value (--disable-gpu, not --disable-gpu=1)
//          'required' only as --name=value with a value, which is not interpreted
//   win32  true       Windows only
// The app's own: --background (the browser extension's host,
// lib/nmhost.js), --updated (an update, selfupdate.js and the NSIS
// installer's --force-run), --lf-restore (the restore program,
// selfupdate.js). Review R44: -Embedding (Windows appends it when COM starts
// the app, e.g. a click on a notification after the app quit);
// --disable-gpu and --disable-gpu-compositing (the app has no setting to
// turn hardware acceleration off; these only lower what the renderer
// does); --lang=xx (the interface language of Chromium's own parts).
// None of them opens a debugging channel.
const PACKAGED_SWITCHES = Object.freeze({
  background: { value: 'any' },
  updated: { value: 'any' },
  'lf-restore': { value: 'any' },
  embedding: { value: 'none', win32: true },
  'disable-gpu': { value: 'none' },
  'disable-gpu-compositing': { value: 'none' },
  lang: { value: 'required' },
});

// macOS (before 10.9) passes the process serial number as -psn_0_NNN.
const MAC_PSN = /^psn_\d+_\d+$/;

// The debugging switches Chromium's own parse (app.commandLine.hasSwitch)
// is asked about as well, in case it reads the command line differently
// from process.argv. Only switches Electron never puts on the main
// process's command line by itself (review R44): before main.js runs,
// Electron appends ELECTRON_ADDED_SWITCHES (ElectronMainDelegate::
// PreSandboxStartup), and --no-sandbox when ELECTRON_DISABLE_SANDBOX is
// set; asking hasSwitch about those refused every start. These eleven were
// the whole check of the packaged releases up to 3.2.0, which started.
const HASSWITCH_SWITCHES = Object.freeze([
  'remote-debugging-port',
  'remote-debugging-pipe',
  'remote-debugging-address',
  'remote-debugging-io-pipes',
  'remote-allow-origins',
  'inspect',
  'inspect-brk',
  'inspect-port',
  'inspect-publish-uid',
  'debug',
  'debug-brk',
]);

// Switches Electron itself appends to the main process's command line
// (never in process.argv): --allow-file-access-from-files everywhere
// (measured on Windows with Electron 44.9.3, test/unit/switchprobe.test.js),
// --enable-avfoundation on macOS. Never to be asked about with hasSwitch.
const ELECTRON_ADDED_SWITCHES = Object.freeze(['allow-file-access-from-files', 'enable-avfoundation']);

// Switches that open a debugging channel or weaken the app (documentation
// and debugSwitchesIn; every switch outside PACKAGED_SWITCHES is refused on
// the argv anyway).
const DEBUG_SWITCHES = [
  ...HASSWITCH_SWITCHES,
  'log-net-log',
  'net-log-capture-mode',
  'enable-logging',
  'v',
  'vmodule',
  'proxy-server',
  'proxy-pac-url',
  'proxy-bypass-list',
  'host-resolver-rules',
  'host-rules',
  'ignore-certificate-errors',
  'ignore-certificate-errors-spki-list',
  'load-extension',
  'disable-extensions-except',
  'no-sandbox',
  'disable-gpu-sandbox',
  'disable-web-security',
  'disable-site-isolation-trials',
  'disable-features',
  'enable-features',
  'js-flags',
  'user-data-dir',
  'renderer-cmd-prefix',
  'gpu-launcher',
  'utility-cmd-prefix',
  'browser-subprocess-path',
];

const MAX_SHOWN = 10;
const MAX_NAME = 64;
// The message box shows the refused arguments as they were written
// (--disable-gpu=1, not --disable-gpu), which come from outside: each is cut
// to MAX_ARG characters and the list to MAX_LIST, both with an ellipsis;
// control characters, line breaks and every character that is not drawn or
// changes how the text around it is drawn are taken out first (by Unicode
// property: controls, format characters such as the bidi marks and the
// tag characters, line and paragraph separators, and the default-ignorable
// ones such as variation selectors and Hangul fillers; review R54 S-6).
const MAX_ARG = 80;
const MAX_LIST = 400;
const ELLIPSIS = '…';
const UNSHOWN = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Default_Ignorable_Code_Point}]/gu;

function prefixFor(platform) {
  return platform === 'win32' ? /^(?:--|-|\/)/ : /^(?:--|-)/;
}

/**
 * switchesIn(argv, platform) → the names of the switches on a command line,
 * each once, lower case. Chromium also accepts "-name" and, on Windows,
 * "/name"; nothing after a bare "--" is a switch.
 */
function parseSwitches(argv, platform) {
  const out = [];
  const prefix = prefixFor(platform);
  for (const raw of argv || []) {
    const a = String(raw);
    if (a === '--') break;
    if (!prefix.test(a)) continue;
    const rest = a.replace(prefix, '');
    const eq = rest.indexOf('=');
    const name = (eq < 0 ? rest : rest.slice(0, eq)).toLowerCase();
    if (name) out.push({ name, value: eq < 0 ? null : rest.slice(eq + 1), raw: a });
  }
  return out;
}

function switchesIn(argv, platform = process.platform) {
  const found = [];
  for (const { name } of parseSwitches(argv, platform)) if (!found.includes(name)) found.push(name);
  return found;
}

/** debugSwitchesIn(argv) → the switches of DEBUG_SWITCHES named on a command line. */
function debugSwitchesIn(argv, platform = process.platform) {
  return switchesIn(argv, platform).filter((n) => DEBUG_SWITCHES.includes(n));
}

function allowed({ name, value }, platform) {
  if (platform === 'darwin' && MAC_PSN.test(name)) return true;
  if (!Object.prototype.hasOwnProperty.call(PACKAGED_SWITCHES, name)) return false;
  const rule = PACKAGED_SWITCHES[name];
  if (rule.win32 && platform !== 'win32') return false;
  if (rule.value === 'none') return value === null;
  if (rule.value === 'required') return typeof value === 'string' && value !== '';
  return true;
}

/** unexpectedSwitchesIn(argv) → the switches (names, each once) the packaged app does not start with. */
function unexpectedSwitchesIn(argv, platform = process.platform) {
  const found = [];
  for (const s of parseSwitches(argv, platform)) {
    if (!allowed(s, platform) && !found.includes(s.name)) found.push(s.name);
  }
  return found;
}

/**
 * refusals(opts) → [{ name, arg }]: every switch that makes the packaged
 * app refuse to start (empty = start), each name once, in order; arg is the
 * argument as written (the first refused one of that name), or --name for
 * a switch only Chromium's parse found. hasSwitch is
 * app.commandLine.hasSwitch.
 */
function refusals({ isPackaged, argv, env = {}, hasSwitch = () => false, platform = process.platform }) {
  if (!isPackaged) return [];
  if (env && env.LECTUREFLOW_E2E_ALLOW_DEBUG === '1') return [];
  const found = [];
  for (const s of parseSwitches(argv, platform)) {
    if (!allowed(s, platform) && !found.some((f) => f.name === s.name)) found.push({ name: s.name, arg: s.raw });
  }
  for (const s of HASSWITCH_SWITCHES) {
    let on = false;
    try {
      on = !!hasSwitch(s);
    } catch {
      on = false;
    }
    if (on && !found.some((f) => f.name === s)) found.push({ name: s, arg: `--${s}` });
  }
  return found;
}

/** cut(s, max) → s, or its first max-1 characters (code points) and an ellipsis. */
function cut(s, max) {
  const chars = Array.from(s);
  return chars.length > max ? chars.slice(0, max - 1).join('') + ELLIPSIS : s;
}

/**
 * refuseDebugging({ isPackaged, argv, env, hasSwitch, platform }) → the
 * names of the switches that make the packaged app refuse to start
 * (empty = start), at most MAX_SHOWN, each cut to MAX_NAME characters.
 */
function refuseDebugging(opts) {
  return refusals(opts).slice(0, MAX_SHOWN).map(({ name }) => (name.length > MAX_NAME ? name.slice(0, MAX_NAME) : name));
}

/**
 * refusedArgs(opts) → the same refusals as refuseDebugging, as the
 * arguments were written, for the message box: control characters and
 * line breaks taken out, each cut to MAX_ARG characters, at most MAX_SHOWN
 * and a last "…" when there are more. Empty = start.
 */
function refusedArgs(opts) {
  const found = refusals(opts);
  const shown = found.slice(0, MAX_SHOWN).map(({ arg }) => cut(String(arg).replace(UNSHOWN, ''), MAX_ARG));
  if (found.length > MAX_SHOWN) shown.push(ELLIPSIS);
  return shown;
}

/** joinRefusedArgs(list) → the list for {switches}: joined with ", ", cut to MAX_LIST characters. */
function joinRefusedArgs(list) {
  return cut((list || []).join(', '), MAX_LIST);
}

// ELECTRON_DISABLE_SANDBOX: Electron reads it at start (Chromium's
// base::Environment::HasVar, before main.js runs) and then runs without the
// sandbox. Together with app.enableSandbox() it crashes the packaged app
// (0xC0000005) as soon as a window opens, without a word (measured with
// Electron 44.9.3 on Windows, T14). The value is not looked at: "1", "0"
// and "false" all count. On Windows a variable with an empty value counts
// as unset (HasVar asks GetEnvironmentVariable, which reports no length;
// measured: no --no-sandbox, the window opens); elsewhere getenv() finds it.
// Variable names are case-insensitive on Windows only. (The non-Windows
// half is Chromium's code, not measured here; switchprobe.test.js compares
// it with the real binary on every CI platform, and startRefusal also asks
// Electron itself, so a wrong guess cannot let a start through.)
const SANDBOX_ENV = 'ELECTRON_DISABLE_SANDBOX';

/**
 * sandboxDisabledByEnv({ isPackaged, env, platform }) → true when the
 * packaged app's environment turns Electron's sandbox off
 * (ELECTRON_DISABLE_SANDBOX, as Electron reads it). Never in development.
 * LECTUREFLOW_E2E_ALLOW_DEBUG=1 does not change it (review R54 S-3): it lets
 * debugging switches through, and a start without the sandbox crashes
 * anyway.
 */
function sandboxDisabledByEnv({ isPackaged, env, platform = process.platform }) {
  if (!isPackaged || !env) return false;
  for (const k of Object.keys(env)) {
    const same = platform === 'win32' ? k.toUpperCase() === SANDBOX_ENV : k === SANDBOX_ENV;
    if (!same) continue;
    const v = env[k];
    if (v === undefined || v === null) continue;
    if (platform === 'win32' && String(v) === '') continue;
    return true;
  }
  return false;
}

/**
 * electronAddedNoSandbox({ argv, hasSwitch, platform }) → true when
 * Electron itself put --no-sandbox on the main process's command line
 * (it does so exactly when it turns the sandbox off, before main.js runs):
 * Chromium's parse has it and the person's argv does not. hasSwitch is
 * asked about no-sandbox only (review R44: Electron appends switches of its
 * own; this is the one asked about because it is one of them).
 */
function electronAddedNoSandbox({ argv, hasSwitch = () => false, platform = process.platform }) {
  let on = false;
  try {
    on = !!hasSwitch('no-sandbox');
  } catch {
    on = false;
  }
  return on && !switchesIn(argv, platform).includes('no-sandbox');
}

// The message boxes of a refused start (i18n keys of locales/*.json).
const REFUSAL_TEXT = Object.freeze({
  args: Object.freeze({ title: 'startup.unsupported_args.title', body: 'startup.unsupported_args.body' }),
  sandbox: Object.freeze({ title: 'startup.sandbox_disabled.title', body: 'startup.sandbox_disabled.body' }),
});

/**
 * startRefusal({ isPackaged, argv, env, hasSwitch, platform }) → whether
 * the packaged app refuses to start, and with which message box:
 *   null                                        start
 *   { kind: 'args', title, body, params }       options it does not start
 *                                               with (params.switches: the
 *                                               arguments as written)
 *   { kind: 'sandbox', title, body, params }    no sandbox: the environment
 *                                               has ELECTRON_DISABLE_SANDBOX,
 *                                               or Electron added --no-sandbox
 * title and body are i18n keys. When both apply, the options come first:
 * they are the more common case and the easier to change. Development
 * never refuses; LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets the options through but
 * not a start without the sandbox. main.js shows the box and exits with 2.
 */
function startRefusal(opts) {
  if (!opts || !opts.isPackaged) return null;
  const args = refusedArgs(opts);
  if (args.length) return { kind: 'args', ...REFUSAL_TEXT.args, params: { switches: joinRefusedArgs(args) } };
  if (sandboxDisabledByEnv(opts) || electronAddedNoSandbox(opts)) return { kind: 'sandbox', ...REFUSAL_TEXT.sandbox, params: {} };
  return null;
}

// Variables only development builds and tests read (each is ignored by the
// packaged code already; dropping them keeps them from reaching the
// service and everything it starts), and the service's development
// switches (LECTUREFLOW_DEV_EXTENSION, LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK,
// LECTUREFLOW_DEV_POWER and any later LECTUREFLOW_DEV_*).
const DEV_ENV = Object.freeze([
  'LECTUREFLOW_FAKE_UPDATER',
  'LECTUREFLOW_TEST_SELFUPDATE',
  'LECTUREFLOW_TEST_APPDATA',
  'LECTUREFLOW_SERVER',
  'LECTUREFLOW_RESOURCES',
]);
const DEV_ENV_PREFIX = 'LECTUREFLOW_DEV_';

/**
 * devEnvToDrop({ isPackaged, env }) → the names (as spelled in env) of the
 * development variables the packaged app removes from its environment
 * before anything reads it. Names are compared without case (Windows).
 * Development keeps them all.
 */
function devEnvToDrop({ isPackaged, env }) {
  if (!isPackaged || !env) return [];
  return Object.keys(env).filter((k) => {
    const u = k.toUpperCase();
    return DEV_ENV.includes(u) || u.startsWith(DEV_ENV_PREFIX);
  });
}

module.exports = {
  DEBUG_SWITCHES, HASSWITCH_SWITCHES, ELECTRON_ADDED_SWITCHES, PACKAGED_SWITCHES, DEV_ENV, DEV_ENV_PREFIX, SANDBOX_ENV,
  MAX_SHOWN, MAX_ARG, MAX_LIST,
  REFUSAL_TEXT,
  switchesIn, debugSwitchesIn, unexpectedSwitchesIn, refuseDebugging, refusedArgs, joinRefusedArgs, devEnvToDrop, sandboxDisabledByEnv,
  electronAddedNoSandbox, startRefusal,
};
