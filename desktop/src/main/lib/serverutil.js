// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Helpers for starting the Go service (v3-brief §1). Pure functions.
'use strict';

const crypto = require('node:crypto');
const path = require('node:path');

const READY_PREFIX = 'LECTUREFLOW_READY ';
const EXIT_RESTART = 3;
const TOKEN_BYTES = 32;
const MAX_REASON_CHARS = 600;

/** generateToken: 32 random bytes as 64 hex characters (never logged, never on disk). */
function generateToken() {
  return crypto.randomBytes(TOKEN_BYTES).toString('hex');
}

/**
 * buildServerArgs: the command line of the service. The token is NOT part
 * of it (command lines are visible to other processes); it goes to stdin.
 * locationFile is omitted when the data dir came from LECTUREFLOW_DATA_DIR.
 * launcher (the program the browser extension's host starts this app with,
 * lib/nmhost.js), launcherBlocked (why there is none on macOS: the app runs
 * from its disk image or translocated) and portable (the portable build)
 * are for the native messaging registration. installDir (the Windows
 * installation set up by the installer, updatefiles.installDirOf; security
 * audit M-4) is the folder whose permissions the diagnostics and the
 * in-app update check. noSelfUpdate: this copy of the app cannot replace
 * itself (updatefiles.describeInstallation: unsupported — an unpacked or
 * copied folder); the service then installs no update here and leaves a
 * pending one to the copy it belongs to (internal/selfupdate NoInstall).
 */
function buildServerArgs({ port, dataDir, resourcesDir, appVersion, configFile, locationFile, launcher = null, launcherBlocked = null, portable = false, installDir = null, noSelfUpdate = false }) {
  const args = ['-desktop', '-listen', `127.0.0.1:${port}`, '-data-dir', dataDir];
  if (resourcesDir) args.push('-resources', resourcesDir);
  if (appVersion) args.push('-app-version', appVersion);
  if (configFile) args.push('-config', configFile);
  if (locationFile) args.push('-location-file', locationFile);
  if (launcher) args.push('-launcher', launcher);
  else if (launcherBlocked) args.push('-launcher-blocked', launcherBlocked);
  if (portable) args.push('-portable');
  if (installDir) args.push('-install-dir', installDir);
  if (noSelfUpdate) args.push('-no-self-update');
  return args;
}

/** parseReadyLine: "LECTUREFLOW_READY http://127.0.0.1:PORT" → { url, port } or null. */
function parseReadyLine(line) {
  if (typeof line !== 'string' || !line.startsWith(READY_PREFIX)) return null;
  const m = /^http:\/\/127\.0\.0\.1:(\d{1,5})\/?$/.exec(line.slice(READY_PREFIX.length).trim());
  if (!m) return null;
  const port = Number(m[1]);
  if (port < 1 || port > 65535) return null;
  return { url: `http://127.0.0.1:${port}`, port };
}

/** extractErrorReason: the text of the last "ERROR: ..." line on stderr, or null. */
function extractErrorReason(lines) {
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = /^\s*ERROR:\s*(.*\S)\s*$/.exec(lines[i]);
    if (m) return m[1].length > MAX_REASON_CHARS ? `${m[1].slice(0, MAX_REASON_CHARS)}…` : m[1];
  }
  return null;
}

/**
 * isPortInUseError: the service could not listen on the port we picked
 * (taken between our check and its bind, or inside a Windows excluded range).
 */
function isPortInUseError(reason) {
  if (typeof reason !== 'string') return false;
  return /address already in use|only one usage of each socket address|EADDRINUSE|WSAEADDRINUSE|forbidden by its access permissions|bind: permission denied/i.test(reason);
}

/** redact replaces every occurrence of secret in text. */
function redact(text, secret) {
  if (!secret || typeof text !== 'string') return text;
  return text.split(secret).join('[token]');
}

/**
 * resolveServerCommand: where the service executable is.
 *   packaged:   {resources}/bin/lectureflow(.exe), resources dir = process.resourcesPath
 *   development: LECTUREFLOW_SERVER (default: {repo}/lectureflow(.exe)),
 *               LECTUREFLOW_RESOURCES (default: the repository root, worker in worker/).
 *               A LECTUREFLOW_SERVER ending in .js/.cjs/.mjs is run with the
 *               Electron binary as Node (tests use a fake service this way).
 * Returns { command, prefixArgs, resourcesDir, extraEnv }.
 *
 * Packaged on macOS, extraEnv has PYTHONDONTWRITEBYTECODE=1: the worker
 * scripts are inside the signed app bundle (Contents/Resources/worker), and
 * the components' Python would otherwise write worker/__pycache__ there on
 * the first transcription, which breaks the bundle's seal ("file added" in
 * codesign --verify; mac-accept 3.2.0). The service passes its environment
 * on to every Python it starts; the components' own modules keep the
 * bytecode they were installed with.
 */
function resolveServerCommand({ isPackaged, resourcesPath, env = {}, repoRoot, platform, execPath, pathMod = path }) {
  const exe = platform === 'win32' ? 'lectureflow.exe' : 'lectureflow';
  if (isPackaged) {
    const extraEnv = platform === 'darwin' ? { PYTHONDONTWRITEBYTECODE: '1' } : {};
    return { command: pathMod.join(resourcesPath, 'bin', exe), prefixArgs: [], resourcesDir: resourcesPath, extraEnv };
  }
  const resourcesDir = env.LECTUREFLOW_RESOURCES ? pathMod.resolve(env.LECTUREFLOW_RESOURCES) : repoRoot;
  const command = env.LECTUREFLOW_SERVER ? pathMod.resolve(env.LECTUREFLOW_SERVER) : pathMod.join(repoRoot, exe);
  if (/\.(c|m)?js$/i.test(command)) {
    return { command: execPath, prefixArgs: [command], resourcesDir, extraEnv: { ELECTRON_RUN_AS_NODE: '1' } };
  }
  return { command, prefixArgs: [], resourcesDir, extraEnv: {} };
}

/**
 * NO_TELEMETRY_ENV: what the service gets on top of the shell's environment,
 * and passes on to everything it starts: the switches that turn off the
 * usage reporting of the Python libraries the workers load (NOTES.md
 * "无遥测"). The same list as internal/pyenv TelemetryOff, which the service
 * sets again for each Python it starts (also without this shell).
 */
const NO_TELEMETRY_ENV = Object.freeze({
  HF_HUB_DISABLE_TELEMETRY: '1',
  HF_HUB_DISABLE_IMPLICIT_TOKEN: '1',
  HF_XET_TELEMETRY_ENABLED: '0',
  DO_NOT_TRACK: '1',
});

/**
 * serviceEnv(base, extraEnv): the environment the service starts with: base
 * without NO_TELEMETRY_ENV's keys in any case (Windows keys are
 * case-insensitive, so a user's "hf_hub_disable_telemetry=0" must not stay
 * next to ours), then NO_TELEMETRY_ENV, then extraEnv
 * (resolveServerCommand's).
 */
function serviceEnv(base, extraEnv = {}) {
  const off = new Set(Object.keys(NO_TELEMETRY_ENV).map((k) => k.toUpperCase()));
  const env = {};
  for (const [k, v] of Object.entries(base || {})) {
    if (!off.has(k.toUpperCase())) env[k] = v;
  }
  return { ...env, ...NO_TELEMETRY_ENV, ...extraEnv };
}

module.exports = {
  READY_PREFIX,
  EXIT_RESTART,
  NO_TELEMETRY_ENV,
  generateToken,
  buildServerArgs,
  parseReadyLine,
  extractErrorReason,
  isPortInUseError,
  redact,
  resolveServerCommand,
  serviceEnv,
};
