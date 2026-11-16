// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The last check of an update package before the shell runs it
// (internal/selfupdate/README.md "What Electron does at start"): the
// installed service program checks the package again with the public key
// built into it, signature included:
//
//   lectureflow -selfupdate-verify <updates dir> -app-version <running version>
//               [-selfupdate-previous] [-selfupdate-copy <temporary dir>]
//               [-selfupdate-private]
//
// -selfupdate-previous: previous.json's package (the version a rollback
// returns to) instead of pending.json's. -selfupdate-copy: the package is
// copied, while it is hashed, into a new directory only this account can
// use (<temporary dir>/lectureflow-update-<n>/<name>), and the shell runs
// that copy, not the file in the updates folder (security review
// sec-update M3: nothing can swap the file between the check and the
// start). It prints one line on stdout:
//   exit 0  ok version=X.Y.Z kind=K file=<relative path> sha256=<hex>[ copy=<path>]
//   exit 1  invalid <code>: <reason>
//   exit 2  not_configured: …   (a build without a public key)
//   exit 3  untrusted_dir: …    (other accounts of this computer can change
//                                the data folder: nothing in it is
//                                installed or rolled back)
//
// Only exit 0 with a line naming exactly the expected package (and, when
// a copy was asked for, a copy in the right place) lets the shell run it.
//
// -selfupdate-private (with -selfupdate-copy; security audit M-3) verifies
// no package: after the same check of the data folder's permissions the
// program makes an empty directory only this account can use and names it,
//   exit 0  ok private=<temporary dir>/lectureflow-update-<n>
// The shell copies a backup of the installation there and checks the
// copy's hash before a rollback uses it (interpretPrivateDir).
//
// -selfupdate-trust-dir <dir> (Windows; security review SEC17 中-B): the
// folder outside the data folder where the shell keeps its update record.
// Every run checks it after the data folder (exit 3 when it lies in the
// data folder, is a link or others can change it). -selfupdate-trust-check
// verifies no package, only these checks: exit 0 "ok trusted"
// (interpretTrustCheck).
//
// -selfupdate-install-dir <dir> (Windows installation; security audit
// M-4): the installation directory (updatefiles.installDirOf). Every run
// checks it last, in every mode: when other accounts can change it (or
// remove or rename it or a folder above it) the program prints
//   exit 3  untrusted_install_dir: …
// and nothing is installed or rolled back either; the user installs the
// new version by hand (the diagnostics page says so).
// No Electron imports.
'use strict';

const childProcess = require('node:child_process');
const path = require('node:path');

const VERIFY_FLAG = '-selfupdate-verify';
const VERSION_FLAG = '-app-version';
const PREVIOUS_FLAG = '-selfupdate-previous';
const COPY_FLAG = '-selfupdate-copy';
const PRIVATE_FLAG = '-selfupdate-private';
const TRUST_DIR_FLAG = '-selfupdate-trust-dir';
const TRUST_CHECK_FLAG = '-selfupdate-trust-check';
const INSTALL_DIR_FLAG = '-selfupdate-install-dir';
// The check hashes the package (about 200 MB) once more and copies it:
// seconds on most disks, longer on a slow one with a virus scanner looking
// at the new file.
const VERIFY_TIMEOUT_MS = 120 * 1000;
const MAX_OUTPUT = 64 * 1024;
// The private directory a copy goes to (selfupdate.CopyPrefix + digits).
const COPY_DIR_RE = /^lectureflow-update-[0-9A-Za-z]+$/;

const OK_RE = /^ok version=(\S+) kind=(\S+) file=(.+?) sha256=([0-9a-f]{64})(?: copy=(.+))?$/;
const INVALID_RE = /^invalid ([^\s:]*):\s*(.*)$/;
const NOT_CONFIGURED_RE = /^not_configured\b:?\s*(.*)$/;
const UNTRUSTED_RE = /^untrusted_dir:\s*(.*)$/;
const UNTRUSTED_INSTALL_RE = /^untrusted_install_dir:\s*(.*)$/;
// What an untrusted installation directory means, whatever was asked
// (short: the program's reason with the folder follows, in 300 characters).
const INSTALL_DIR_TEXT = 'other accounts can change the installation folder: nothing is installed or rolled back in the app (install the new version by hand)';
// The program's reason when the folder the shell keeps its update record
// in (-selfupdate-trust-dir: %LOCALAPPDATA%\LectureFlow\selfupdate) and the
// data folder overlap (internal/selfupdate/trustdir.go CheckTrustDir).
// Tightening permissions does not help then; moving the data folder does
// (security review SEC17 round 4, low-I). Anchored at the end: the program
// always ends its reason with it ("… cannot vouch for the record (%s)"),
// while a path earlier in another reason may contain the same words
// (review R47 S1).
const OVERLAP_RE = /\(it lies in the data directory, or the data directory lies in it\)$/;
// (the folder it names: the rest of the reason says what OVERLAP_TEXT says)
const OVERLAP_DIR_RE = /trust directory (.+) cannot vouch for the record \(it lies in the data directory, or the data directory lies in it\)$/;
const OVERLAP_TEXT = 'the data folder overlaps %LOCALAPPDATA%\\LectureFlow, where the update record is kept';
const OVERLAP_FIX = '(move the data folder somewhere else)';
const PRIVATE_RE = /^ok private=(.+)$/;
const TRUSTED_LINE = 'ok trusted';

/**
 * verifyArgs(updatesDir, appVersion, { previous, copyTo, privateDir }) →
 * the arguments after the program (and its prefix arguments). privateDir:
 * make an empty private directory in copyTo instead of verifying a package.
 */
function verifyArgs(updatesDir, appVersion, { previous = false, copyTo = '', privateDir = false, trustDir = '', trustCheck = false, installDir = '' } = {}) {
  const args = [VERIFY_FLAG, updatesDir];
  if (appVersion) args.push(VERSION_FLAG, appVersion);
  if (trustCheck) args.push(TRUST_CHECK_FLAG);
  else if (privateDir) args.push(PRIVATE_FLAG);
  else if (previous) args.push(PREVIOUS_FLAG);
  if (copyTo && !trustCheck) args.push(COPY_FLAG, copyTo);
  if (trustDir) args.push(TRUST_DIR_FLAG, trustDir);
  if (installDir) args.push(INSTALL_DIR_FLAG, installDir);
  return args;
}

/** untrustedInstall(line) → the reason when line says the installation directory is not vouched for, else null. */
function untrustedInstall(line) {
  const m = line ? UNTRUSTED_INSTALL_RE.exec(line) : null;
  // (the program's reason repeats the code; the folder matters more)
  return m ? oneLine(`${INSTALL_DIR_TEXT}: ${m[1].replace(/^selfupdate\.untrusted_install_dir:\s*/, '')}`) : null;
}

/**
 * untrustedDir(reason, { others, overlap }) → the text for an
 * "untrusted_dir: <reason>" line: overlap (then OVERLAP_FIX) when the
 * reason is that the trust folder and the data folder overlap (followed by
 * the trust folder the program names, or failing that its whole reason),
 * else others followed by the program's reason, as before.
 */
function untrustedDir(reason, { others, overlap }) {
  if (OVERLAP_RE.test(reason)) {
    const dir = OVERLAP_DIR_RE.exec(reason);
    return oneLine(`${OVERLAP_TEXT}${overlap} ${OVERLAP_FIX}: ${dir ? dir[1] : reason}`);
  }
  return oneLine(`${others}: ${reason}`);
}

/**
 * stripQueries(text): URLs keep scheme, host and path; query strings and
 * fragments (which may carry tokens) are cut before anything is logged or
 * written to result.json.
 */
function stripQueries(text) {
  return String(text == null ? '' : text).replace(/\b([a-z][a-z0-9+.-]*:\/\/[^\s?#"'<>]*)[?#][^\s"'<>]*/gi, '$1');
}

/** oneLine(text) → one line of at most 300 characters, without URL query strings. */
function oneLine(text) {
  const s = stripQueries(text).replace(/\s+/g, ' ').trim();
  return s.length > 300 ? `${s.slice(0, 299)}…` : s;
}

/**
 * runVerify(cmd, updatesDir, appVersion, { timeoutMs, spawn, previous,
 * copyTo, privateDir }) runs the service program with -selfupdate-verify. cmd is what
 * lib/serverutil.resolveServerCommand returns ({ command, prefixArgs,
 * extraEnv }) plus env (the environment to run it with).
 * → Promise<{ code, stdout, stderr, timedOut, error }>, never rejects:
 *   code      the exit code (null when it did not exit by itself)
 *   timedOut  it was stopped after timeoutMs
 *   error     why it could not be run (or how it ended without an exit code)
 */
function runVerify(cmd, updatesDir, appVersion, {
  timeoutMs = VERIFY_TIMEOUT_MS, spawn = childProcess.spawn, previous = false, copyTo = '', privateDir = false, trustDir = '', trustCheck = false, installDir = '',
} = {}) {
  return new Promise((resolve) => {
    let stdout = '';
    let stderr = '';
    let timer = null;
    let done = false;
    const finish = (r) => {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      resolve({ code: null, stdout, stderr, timedOut: false, error: null, ...r });
    };
    const args = [...(cmd.prefixArgs || []), ...verifyArgs(updatesDir, appVersion, { previous, copyTo, privateDir, trustDir, trustCheck, installDir })];
    const env = { ...(cmd.env || process.env), ...(cmd.extraEnv || {}) };
    let child;
    try {
      child = spawn(cmd.command, args, { env, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
    } catch (err) {
      finish({ error: oneLine(err && err.message ? err.message : err) });
      return;
    }
    const collect = (stream, add) => {
      if (!stream) return;
      stream.setEncoding('utf8');
      stream.on('data', add);
      stream.on('error', () => {});
    };
    collect(child.stdout, (s) => {
      if (stdout.length < MAX_OUTPUT) stdout += s;
    });
    collect(child.stderr, (s) => {
      if (stderr.length < MAX_OUTPUT) stderr += s;
    });
    child.once('error', (err) => finish({ error: oneLine(err && err.message ? err.message : err) }));
    child.once('close', (code, signal) => {
      if (typeof code === 'number') finish({ code });
      else finish({ error: `stopped${signal ? ` by ${signal}` : ''}` });
    });
    timer = setTimeout(() => {
      try {
        child.kill();
      } catch {
        // already gone
      }
      finish({ timedOut: true });
    }, timeoutMs);
  });
}

/** lastLine(text) → the last line of stderr that says something (for a failed run). */
function lastLine(text) {
  const lines = String(text || '').split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  return lines.length ? oneLine(lines[lines.length - 1]) : '';
}

function samePath(a, b) {
  const na = path.resolve(a);
  const nb = path.resolve(b);
  return process.platform === 'win32' ? na.toLowerCase() === nb.toLowerCase() : na === nb;
}

/**
 * copyPath(copy, copyTo, file) → copy when it is where the program was
 * asked to put it: <copyTo>/lectureflow-update-<n>/<the package's name>,
 * else null.
 */
function copyPath(copy, copyTo, file) {
  if (typeof copy !== 'string' || !copy || !copyTo || !path.isAbsolute(copy) || copy.includes('\0')) return null;
  const dir = path.dirname(copy);
  if (!samePath(path.dirname(dir), copyTo) || !COPY_DIR_RE.test(path.basename(dir))) return null;
  if (path.basename(copy) !== path.posix.basename(String(file || ''))) return null;
  return path.resolve(copy);
}

/**
 * interpretVerify(run, expected, { timeoutMs, copyTo, what }) → what the
 * shell does with the package expected ({ version, kind, file, sha256 }:
 * pending.json, or previous.json for a rollback), given runVerify's result:
 *   { install: true, line, copy }  exit 0 naming exactly the expected
 *                            package; copy: the private copy to run (null
 *                            when none was asked for)
 *   { drop: reason }         the program says the package is invalid
 *                            (exit 1 "invalid …"), or vouches for another
 *                            package (for pending.json: the contract's
 *                            "delete pending.json")
 *   { keep: reason }         no verdict: a build without a public key
 *                            (exit 2, notConfigured), other accounts can
 *                            change the data folder or it overlaps the
 *                            trust folder (exit 3, untrusted), a
 *                            timeout, a program that could not run or gave
 *                            an answer outside the contract (also exit 0
 *                            without the copy asked for)
 * Every result also has verdict: ok | invalid | not_configured |
 * untrusted | none, and copy when the line named one where it belongs (so
 * the caller can remove a copy it does not use). Reasons are short
 * English, without URL query strings.
 */
function interpretVerify(run, expected, { timeoutMs = VERIFY_TIMEOUT_MS, copyTo = '', what = 'pending' } = {}) {
  const r = run || {};
  const none = (keep) => ({ keep, verdict: 'none' });
  if (r.timedOut) return none(`the update check did not finish within ${Math.round(timeoutMs / 1000)} seconds`);
  if (r.error) return none(`the update check could not run: ${oneLine(r.error)}`);
  const lines = String(r.stdout || '').split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  const line = lines.length === 1 ? lines[0] : null;
  const unusable = () => {
    const detail = lastLine(r.stderr);
    return none(`the update check gave no usable answer (exit code ${r.code})${detail ? `: ${detail}` : ''}`);
  };
  if (r.code === 0) {
    const m = line ? OK_RE.exec(line) : null;
    if (!m) return unusable();
    const [, version, kind, file, sha256, named] = m;
    const p = expected || {};
    const copy = named ? copyPath(named, copyTo, file) : null;
    if (version !== p.version || kind !== p.kind || file !== p.file || sha256 !== p.sha256) {
      return { drop: oneLine(`the update check vouched for ${version} (${kind}, ${file}), not the ${what} ${p.version} (${p.kind}, ${p.file})`), verdict: 'invalid', copy };
    }
    if (copyTo && !copy) {
      return { ...unusable(), keep: `the update check did not copy the package where it was asked to${named ? ` (${oneLine(named)})` : ''}` };
    }
    return { install: true, line: oneLine(line), copy, verdict: 'ok' };
  }
  if (r.code === 1 && line) {
    const m = INVALID_RE.exec(line);
    if (m) return { drop: oneLine(`the update did not pass the check with the built-in key (${m[1] || 'invalid'}): ${m[2]}`), verdict: 'invalid', code: m[1] };
  }
  if (r.code === 2 && line && NOT_CONFIGURED_RE.test(line)) {
    return { keep: 'this build has no update public key, so the update cannot be verified here', verdict: 'not_configured' };
  }
  if (r.code === 3 && line) {
    const m = UNTRUSTED_RE.exec(line);
    if (m) {
      return {
        keep: untrustedDir(m[1], {
          others: 'other accounts of this computer can change the data folder, so no update is installed from it (tighten its permissions in Diagnostics)',
          overlap: ', so no update is installed from it',
        }),
        verdict: 'untrusted',
      };
    }
    const install = untrustedInstall(line);
    if (install) return { keep: install, verdict: 'untrusted' };
  }
  return unusable();
}

/**
 * interpretPrivateDir(run, { timeoutMs, copyTo }) → what runVerify's result
 * of a -selfupdate-private run means:
 *   { dir, verdict: 'ok' }   exit 0 naming a directory where it was asked
 *                            for: <copyTo>/lectureflow-update-<n>
 *   { error, verdict }       anything else, in short English: 'untrusted'
 *                            (exit 3: other accounts can change the data
 *                            folder), 'invalid' (exit 1: the directory
 *                            could not be made) or 'none' (a timeout, a
 *                            program that could not run, an answer outside
 *                            the contract — also a directory elsewhere)
 */
function interpretPrivateDir(run, { timeoutMs = VERIFY_TIMEOUT_MS, copyTo = '' } = {}) {
  const r = run || {};
  const none = (error) => ({ error, verdict: 'none' });
  if (r.timedOut) return none(`the update check did not finish within ${Math.round(timeoutMs / 1000)} seconds`);
  if (r.error) return none(`the update check could not run: ${oneLine(r.error)}`);
  const lines = String(r.stdout || '').split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  const line = lines.length === 1 ? lines[0] : null;
  const unusable = () => {
    const detail = lastLine(r.stderr);
    return none(`the update check gave no usable answer (exit code ${r.code})${detail ? `: ${detail}` : ''}`);
  };
  if (r.code === 0) {
    const m = line ? PRIVATE_RE.exec(line) : null;
    if (!m) return unusable();
    const dir = m[1];
    if (!copyTo || !path.isAbsolute(dir) || dir.includes('\0') || !samePath(path.dirname(dir), copyTo) || !COPY_DIR_RE.test(path.basename(dir))) {
      return none(`the update check did not make the private folder where it was asked to (${oneLine(dir)})`);
    }
    return { dir: path.resolve(dir), verdict: 'ok' };
  }
  if (r.code === 1 && line) {
    const m = INVALID_RE.exec(line);
    if (m) return { error: oneLine(`no private folder could be made (${m[1] || 'invalid'}): ${m[2]}`), verdict: 'invalid' };
  }
  if (r.code === 3 && line) {
    const m = UNTRUSTED_RE.exec(line);
    if (m) {
      return {
        error: untrustedDir(m[1], {
          others: 'other accounts of this computer can change the data folder, so nothing in it is used (tighten its permissions in Diagnostics)',
          overlap: ', so nothing in the data folder is used',
        }),
        verdict: 'untrusted',
      };
    }
    const install = untrustedInstall(line);
    if (install) return { error: install, verdict: 'untrusted' };
  }
  return unusable();
}

/**
 * interpretTrustCheck(run, { timeoutMs }) → what runVerify's result of a
 * -selfupdate-trust-check run means: { ok: true } only for exit 0 with
 * exactly "ok trusted"; anything else is { error } in short English (exit
 * 3: other accounts can change the data folder or the trust folder, or
 * the two overlap; a timeout, a program that could not run, an answer outside the contract).
 */
function interpretTrustCheck(run, { timeoutMs = VERIFY_TIMEOUT_MS } = {}) {
  const r = run || {};
  if (r.timedOut) return { error: `the update check did not finish within ${Math.round(timeoutMs / 1000)} seconds` };
  if (r.error) return { error: `the update check could not run: ${oneLine(r.error)}` };
  const lines = String(r.stdout || '').split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  const line = lines.length === 1 ? lines[0] : null;
  if (r.code === 0 && line === TRUSTED_LINE) return { ok: true };
  if (r.code === 3 && line) {
    const m = UNTRUSTED_RE.exec(line);
    if (m) {
      return {
        error: untrustedDir(m[1], {
          others: "other accounts of this computer can change the data folder or the update's trust folder",
          overlap: '',
        }),
      };
    }
    const install = untrustedInstall(line);
    if (install) return { error: install };
  }
  const detail = lastLine(r.stderr);
  return { error: `the update check gave no usable answer (exit code ${r.code})${detail ? `: ${detail}` : ''}` };
}

module.exports = {
  VERIFY_FLAG,
  TRUST_DIR_FLAG,
  TRUST_CHECK_FLAG,
  INSTALL_DIR_FLAG,
  interpretTrustCheck,
  VERIFY_TIMEOUT_MS,
  COPY_DIR_RE,
  verifyArgs,
  stripQueries,
  runVerify,
  copyPath,
  interpretVerify,
  interpretPrivateDir,
};
