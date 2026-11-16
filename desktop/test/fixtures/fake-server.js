// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// A fake LectureFlow service for the shell's tests. It follows the command
// line and HTTP contract of v3-brief §1–§2 (desktop mode only): token on the
// first stdin line, stdin EOF stops it, "LECTUREFLOW_READY <url>" on stdout,
// "ERROR: <reason>" on stderr before exit code 1, token and Host checks on
// /api/. Its state (attempt counter, activity, settings, calls received) is
// kept in {data dir}/fake-state.json so tests can steer and inspect it
// across restarts. The /test/ endpoints exist only here (/test/open prints
// the browser extension's "LECTUREFLOW_OPEN <route>" line).
//
// Behaviour switches (environment):
//   FAKE_PORT_FAIL_TIMES=n   the first n attempts fail with "port in use"
//   FAKE_START_FAIL_TIMES=n  the first n attempts fail with a database error
//   FAKE_HANG=1              never become ready
//   FAKE_FAILURE=json        fail with "LECTUREFLOW_FAILURE <json>" before the ERROR line
//   FAKE_STEPS=stdin         the start goes as the test says, by lines on stdin
//                            after the token: "starting" prints one
//                            "LECTUREFLOW_STARTING upgrade-backup", "ready"
//                            listens and prints the ready line (the shell's
//                            start timeouts on a fake clock: server.test.js)
//   FAKE_IGNORE_STOP=1       ignore /api/app/shutdown and stdin EOF
//   FAKE_BAD_MODE=1          /api/health reports mode "web"
//   FAKE_SELFUPDATE=no_key   GET /api/selfupdate: not_configured / no_public_key
//   FAKE_SELFUPDATE=configured  GET /api/selfupdate: idle (a public key is built in)
//                            (unset: 404, like a service without the in-app update)
//
// -selfupdate-verify DIR [-app-version V] [-selfupdate-previous]
// [-selfupdate-copy TMP] [-selfupdate-private] (internal/selfupdate/README.md): prints one line
// and exits, like the real program; nothing else runs. -selfupdate-previous
// checks previous.json's package instead of pending.json's (none: exit 1
// not_available); -selfupdate-copy copies the package into
// TMP/lectureflow-update-<n>/ and names it (" copy=…").
//   FAKE_SELFUPDATE_VERIFY unset / ok   0 "ok …" naming the JSON file's package
//                          invalid      1 "invalid selfupdate.signature_invalid: …"
//                          not_configured  2 "not_configured: …"
//                          untrusted    3 "untrusted_dir: …"
//                          other        0 "ok …" naming another SHA-256
//   FAKE_VERIFY_LOG=file   each call appends {dir, app_version, previous, copy} as a JSON line
'use strict';

const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');

// install-dir, selfupdate-install-dir: accepted and not checked (the installation
// directory's permissions are the real program's business, security audit M-4)
const VALUE_FLAGS = new Set(['listen', 'data-dir', 'resources', 'app-version', 'config', 'location-file', 'launcher', 'nm-endpoint', 'selfupdate-verify', 'selfupdate-copy', 'install-dir', 'selfupdate-install-dir']);
const BOOL_FLAGS = new Set(['portable', 'no-self-update', 'selfupdate-previous', 'selfupdate-private']);

function parseArgs(argv) {
  const out = { desktop: false };
  for (let i = 0; i < argv.length; i++) {
    const m = /^--?([a-z-]+)$/.exec(argv[i]);
    if (!m) fatal(`unexpected argument ${argv[i]}`);
    if (m[1] === 'desktop') out.desktop = true;
    else if (BOOL_FLAGS.has(m[1])) out[m[1]] = true;
    else if (VALUE_FLAGS.has(m[1])) out[m[1]] = argv[++i];
    else fatal(`flag provided but not defined: -${m[1]}`);
  }
  return out;
}

function fatal(reason, code = 1) {
  process.stderr.write(`ERROR: ${reason}\n`);
  process.exit(code);
}

function selfUpdateVerify(dir, appVersion, { previous = false, copy = '', priv = false } = {}) {
  const say = (line, code) => {
    process.stdout.write(`${line}\n`);
    process.exit(code);
  };
  if (process.env.FAKE_VERIFY_LOG) {
    const entry = { dir, app_version: appVersion || '', previous, copy };
    if (priv) entry.private = true;
    fs.appendFileSync(process.env.FAKE_VERIFY_LOG, `${JSON.stringify(entry)}\n`);
  }
  const mode = process.env.FAKE_SELFUPDATE_VERIFY || 'ok';
  if (mode === 'untrusted') say(`untrusted_dir: selfupdate.untrusted_dir: other accounts of this computer can change ${path.dirname(dir)} (Authenticated Users)`, 3);
  if (priv) {
    // -selfupdate-private: no package is verified; an empty folder for the
    // shell's copy of a backup
    if (!copy) say('invalid selfupdate.io_failed: no directory to make the private directory in', 1);
    const d = path.join(copy, `lectureflow-update-${process.pid}`);
    fs.mkdirSync(d, { recursive: true });
    say(`ok private=${d}`, 0);
  }
  if (mode === 'invalid') say('invalid selfupdate.signature_invalid: update.json.minisig does not verify with the built-in key', 1);
  if (mode === 'not_configured') say('not_configured: this build has no update public key', 2);
  const which = previous ? 'previous.json' : 'pending.json';
  let p;
  try {
    p = JSON.parse(fs.readFileSync(path.join(dir, which), 'utf8'));
  } catch {
    say(`invalid selfupdate.not_available: nothing to check (${which})`, 1);
  }
  if (!p.file) say(`invalid selfupdate.not_available: ${which} names no package`, 1);
  const sha256 = mode === 'ok' ? p.sha256 : 'f'.repeat(64);
  let line = `ok version=${p.version} kind=${p.kind} file=${p.file} sha256=${sha256}`;
  if (copy) {
    const d = path.join(copy, `lectureflow-update-${process.pid}`);
    fs.mkdirSync(d, { recursive: true });
    const to = path.join(d, path.posix.basename(p.file));
    fs.copyFileSync(path.join(dir, ...p.file.split('/')), to);
    line += ` copy=${to}`;
  }
  say(line, 0);
}

const opts = parseArgs(process.argv.slice(2));
if (opts['selfupdate-verify'] !== undefined) {
  selfUpdateVerify(opts['selfupdate-verify'], opts['app-version'], { previous: !!opts['selfupdate-previous'], copy: opts['selfupdate-copy'] || '', priv: !!opts['selfupdate-private'] });
}
if (!opts.desktop) fatal('the fake service only knows desktop mode');
if (!opts.listen || !opts['data-dir']) fatal('-listen and -data-dir are required');
const port = Number(opts.listen.split(':')[1]);
const dataDir = opts['data-dir'];
const stateFile = path.join(dataDir, 'fake-state.json');
const env = process.env;

function loadState() {
  try {
    return JSON.parse(fs.readFileSync(stateFile, 'utf8'));
  } catch {
    return {
      attempts: 0,
      pids: [],
      args: [],
      shutdownCalls: 0,
      stdinEof: 0,
      patches: [],
      unauthorized: 0,
      activity: { running: 0, queued: 0, transcribing: false, api_running: false },
      settings: { close_behavior: 'ask', prevent_sleep: true, update_channel: 'stable' },
      ui: { locale: '', zh_display: '' },
    };
  }
}
const state = loadState();
state.ui = state.ui || { locale: '', zh_display: '' };
function save() {
  fs.mkdirSync(dataDir, { recursive: true });
  fs.writeFileSync(stateFile, JSON.stringify(state, null, 2));
}
state.attempts++;
state.pids.push(process.pid);
state.args.push(process.argv.slice(2));
save();

let token = null;
let stopping = false;

function stop(why) {
  if (stopping) return;
  if (env.FAKE_IGNORE_STOP === '1') return;
  stopping = true;
  process.stdout.write(`fake: stopping (${why})\n`);
  setTimeout(() => process.exit(0), 50);
}

// stdin: first line = token (then, with FAKE_STEPS=stdin, the test's
// steps); EOF = the shell has gone.
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  if (token !== null && env.FAKE_STEPS !== 'stdin') return;
  buf += chunk;
  for (let nl = buf.indexOf('\n'); nl >= 0; nl = buf.indexOf('\n')) {
    const line = buf.slice(0, nl).trim();
    buf = buf.slice(nl + 1);
    if (token === null) {
      token = line;
      if (token.length < 32) fatal(`desktop mode: access token too short (${token.length} characters, at least 32)`);
      begin();
    } else if (env.FAKE_STEPS === 'stdin') {
      step(line);
    }
  }
});
process.stdin.on('end', () => {
  state.stdinEof++;
  save();
  if (token === null) fatal('desktop mode: no access token on stdin');
  stop('stdin EOF');
});

const PAGE = `<!doctype html>
<html><head><meta charset="utf-8"><title>Fake LectureFlow</title><script src="/app.js"></script></head>
<body><h1>Fake LectureFlow</h1><a id="ext" href="https://example.com/">external</a></body></html>`;
const SCRIPT = `window.__fakeLoaded = true;
fetch('/api/health').then((r) => { document.documentElement.dataset.health = String(r.status); });`;

function send(res, status, body, type = 'application/json') {
  const data = typeof body === 'string' ? body : JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': type, 'Cache-Control': 'no-store' });
  res.end(data);
}

function readBody(req) {
  return new Promise((resolve) => {
    let s = '';
    req.on('data', (c) => { s += c; });
    req.on('end', () => {
      try {
        resolve(s ? JSON.parse(s) : {});
      } catch {
        resolve(null);
      }
    });
  });
}

async function handle(req, res) {
  const host = String(req.headers.host || '').toLowerCase();
  if (host !== `127.0.0.1:${port}` && host !== `localhost:${port}`) return send(res, 403, { error: { code: 'auth.bad_host' } });
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const p = url.pathname;

  if (p === '/api' || p.startsWith('/api/')) {
    if (req.headers['x-lf-token'] !== token) {
      state.unauthorized++;
      save();
      return send(res, 401, { error: { code: 'auth.token_required' } });
    }
    if (p === '/api/health') return send(res, 200, { ok: true, version: opts['app-version'] || 'dev', mode: env.FAKE_BAD_MODE === '1' ? 'web' : 'desktop' });
    if (p === '/api/app/activity') return send(res, 200, state.activity);
    if (p === '/api/app/settings' && req.method === 'GET') return send(res, 200, state.settings);
    if (p === '/api/settings/ui' && req.method === 'GET') return send(res, 200, state.ui);
    if (p === '/api/selfupdate' && req.method === 'GET' && env.FAKE_SELFUPDATE) {
      state.selfupdateCalls = (state.selfupdateCalls || 0) + 1;
      save();
      const base = { current_version: opts['app-version'] || 'dev', channel: 'stable', auto_check: true };
      if (env.FAKE_SELFUPDATE === 'no_key') return send(res, 200, { ...base, state: 'not_configured', reason: 'no_public_key' });
      return send(res, 200, { ...base, state: 'idle', platform: 'windows-x64', kind: 'installer' });
    }
    if (p === '/api/app/settings' && req.method === 'PATCH') {
      const patch = await readBody(req);
      if (!patch || (patch.close_behavior !== undefined && !['ask', 'background', 'quit'].includes(patch.close_behavior))) {
        return send(res, 400, { error: { code: 'api.bad_request' } });
      }
      state.patches.push(patch);
      Object.assign(state.settings, patch);
      save();
      return send(res, 200, state.settings);
    }
    if (p === '/api/app/shutdown' && req.method === 'POST') {
      state.shutdownCalls++;
      save();
      send(res, 202, { ok: true });
      stop('POST /api/app/shutdown');
      return undefined;
    }
    return send(res, 404, { error: { code: 'api.not_found' } });
  }

  // Test-only controls (no token: the test runner calls them directly).
  if (p === '/test/state') return send(res, 200, { ...state, pid: process.pid });
  if (p === '/test/set' && req.method === 'POST') {
    const body = await readBody(req);
    if (body && body.activity) Object.assign(state.activity, body.activity);
    if (body && body.settings) Object.assign(state.settings, body.settings);
    if (body && body.ui) Object.assign(state.ui, body.ui);
    save();
    return send(res, 200, state);
  }
  if (p === '/test/open') {
    // The browser extension's "open" as the real service reports it.
    process.stdout.write(`LECTUREFLOW_OPEN ${url.searchParams.get('route') || ''}\n`);
    return send(res, 200, { ok: true });
  }
  if (p === '/test/exit') {
    const code = Number(url.searchParams.get('code') || '1');
    send(res, 200, { exiting: code });
    setTimeout(() => {
      if (code !== 0 && code !== 3) process.stderr.write('ERROR: simulated failure for the test\n');
      process.exit(code);
    }, 50);
    return undefined;
  }

  if (p === '/' || p === '/index.html') return send(res, 200, PAGE, 'text/html; charset=utf-8');
  if (p === '/app.js') return send(res, 200, SCRIPT, 'text/javascript; charset=utf-8');
  if (p === '/other.html') return send(res, 200, '<!doctype html><title>Other</title><p>other</p>', 'text/html; charset=utf-8');
  return send(res, 404, 'not found', 'text/plain');
}

function begin() {
  if (state.attempts <= Number(env.FAKE_PORT_FAIL_TIMES || 0)) {
    fatal(`listen tcp 127.0.0.1:${port}: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.`);
  }
  if (state.attempts <= Number(env.FAKE_START_FAIL_TIMES || 0)) fatal('open database: database is locked');
  if (env.FAKE_FAILURE) {
    process.stderr.write(`LECTUREFLOW_FAILURE ${env.FAKE_FAILURE}\n`);
    fatal('a coded failure for the test');
  }
  if (env.FAKE_STEPS === 'stdin') return; // step() goes on
  if (env.FAKE_HANG === '1') return;
  listen();
}

// FAKE_STEPS=stdin: one step of the start, as the test says.
function step(line) {
  if (line === 'starting') process.stdout.write('LECTUREFLOW_STARTING upgrade-backup\n');
  else if (line === 'ready') listen();
}

function listen() {
  const server = http.createServer((req, res) => {
    handle(req, res).catch((err) => send(res, 500, { error: { code: 'internal', message: String(err) } }));
  });
  server.on('error', (err) => fatal(`listen tcp 127.0.0.1:${port}: ${err.code}`));
  server.listen(port, '127.0.0.1', () => {
    process.stdout.write(`fake: listening\nLECTUREFLOW_READY http://127.0.0.1:${port}\n`);
  });
}
