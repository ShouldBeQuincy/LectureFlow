// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The files of the in-app update (SPEC-ext "应用内更新", contract in
// internal/selfupdate/README.md "The updates directory"): versions, paths
// inside {data_dir}/updates, small JSON files written atomically, hashes,
// and what this installation is. No Electron imports.
//
// In Electron the patched fs treats *.asar files as directories, which
// would break copying an installation (resources/app.asar); "original-fs"
// is the unpatched module, so it is used whenever it exists.
'use strict';

const crypto = require('node:crypto');
const path = require('node:path');

// eslint-disable-next-line global-require
const fs = process.versions.electron ? require('original-fs') : require('node:fs');
const fsp = fs.promises;

const SCHEMA = 1;
const FILES = {
  pending: 'pending.json',
  previous: 'previous.json',
  applying: 'applying.json',
  result: 'result.json',
};
const BACKUP_DIR = 'backup';
const BACKUP_MARK = 'backup.json'; // written once a backup is complete
// The shell's own record of the installation it started and the backups it
// made (selfupdate.js), kept in the shell's profile (userData): under the
// user's profile folder (%APPDATA%\LectureFlow\electron, ~/Library/…),
// which only this account can change, also when the data folder was moved.
const TRUST_FILE = 'selfupdate-trust.json';
// On Windows the record lives outside the data folder instead (security
// review SEC17 中-B: a profile in the data folder could have been written
// by another account that once could write there): trust.json in
// trustDirFor(), which the service program checks (-selfupdate-trust-dir).
// migrated.json there says the record of an older version's profile was
// looked at once (selfupdate.js _migrateTrust) and is never looked at again.
const TRUST_DIR_FILE = 'trust.json';
const TRUST_MIGRATED_FILE = 'migrated.json';
const DB_FILES = ['lectureflow.db', 'lectureflow.db-wal', 'lectureflow.db-shm'];
const PLATFORMS = { windows: 'windows-x64', darwin: 'darwin-arm64' };
const KINDS = { installer: 'installer', portable: 'portable', zip: 'zip', dmg: 'dmg' };
const MAC_BUNDLE_ID = 'org.lectureflow.desktop'; // = appId of electron-builder.config.js

// ---- versions (semver 2.0 precedence, as internal/components/semver.go) ----

const VERSION_RE = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$/;

/** parseVersion("3.2.0-beta.1+x") → { major, minor, patch, pre: [...] } or null (no "v"). */
function parseVersion(s) {
  if (typeof s !== 'string') return null;
  const m = VERSION_RE.exec(s);
  if (!m) return null;
  const pre = m[4] ? m[4].split('.') : [];
  if (pre.some((p) => p === '')) return null;
  return { major: Number(m[1]), minor: Number(m[2]), patch: Number(m[3]), pre };
}

/** compareVersions(a, b) → -1 / 0 / 1; an unparsable version sorts first. */
function compareVersions(a, b) {
  const va = parseVersion(a);
  const vb = parseVersion(b);
  if (!va || !vb) {
    if (!va && !vb) return a < b ? -1 : a > b ? 1 : 0;
    return va ? 1 : -1;
  }
  for (const k of ['major', 'minor', 'patch']) {
    if (va[k] !== vb[k]) return va[k] < vb[k] ? -1 : 1;
  }
  if (!va.pre.length || !vb.pre.length) {
    if (va.pre.length === vb.pre.length) return 0;
    return va.pre.length ? -1 : 1; // a pre-release is older than the release
  }
  for (let i = 0; i < va.pre.length && i < vb.pre.length; i++) {
    const x = va.pre[i];
    const y = vb.pre[i];
    const xn = /^\d+$/.test(x);
    const yn = /^\d+$/.test(y);
    if (xn && yn) {
      if (Number(x) !== Number(y)) return Number(x) < Number(y) ? -1 : 1;
    } else if (xn !== yn) {
      return xn ? -1 : 1; // numeric identifiers have lower precedence
    } else if (x !== y) {
      return x < y ? -1 : 1;
    }
  }
  if (va.pre.length === vb.pre.length) return 0;
  return va.pre.length < vb.pre.length ? -1 : 1;
}

function sameVersion(a, b) {
  return !!parseVersion(a) && !!parseVersion(b) && compareVersions(a, b) === 0;
}

// ---- paths ----

/**
 * resolveInside(dir, rel): a "/"-separated path relative to the updates
 * directory → absolute path, or null when it is absolute, has a drive or a
 * backslash, or leaves the directory (same rules as absPath in store.go).
 */
function resolveInside(dir, rel) {
  if (typeof rel !== 'string' || rel === '' || rel.includes('\\') || rel.includes('\0')) return null;
  if (path.posix.isAbsolute(rel) || path.win32.isAbsolute(rel) || /^[A-Za-z]:/.test(rel)) return null;
  const clean = path.posix.normalize(rel);
  if (clean === '.' || clean === '..' || clean.startsWith('../')) return null;
  return path.join(dir, ...clean.split('/'));
}

/** relativeTo(dir, abs) → "/"-separated path of abs inside dir (abs must be inside). */
function relativeTo(dir, abs) {
  return path.relative(dir, abs).split(path.sep).join('/');
}

// ---- JSON files ----

/** readJson(file) → { exists: false } | { exists: true, value } | { exists: true, error } */
async function readJson(file) {
  let text;
  try {
    text = await fsp.readFile(file, 'utf8');
  } catch (err) {
    if (err && (err.code === 'ENOENT' || err.code === 'ENOTDIR')) return { exists: false };
    return { exists: true, error: `${path.basename(file)}: ${err.code || err.message}` };
  }
  if (text.length > 1 << 20) return { exists: true, error: `${path.basename(file)}: too large` };
  try {
    const value = JSON.parse(text.replace(/^\ufeff/, ''));
    if (!value || typeof value !== 'object' || Array.isArray(value)) return { exists: true, error: `${path.basename(file)}: not an object` };
    return { exists: true, value };
  } catch {
    return { exists: true, error: `${path.basename(file)}: not JSON` };
  }
}

/** writeJsonAtomic(file, value): a temporary file in the same directory, renamed over file. */
async function writeJsonAtomic(file, value) {
  await fsp.mkdir(path.dirname(file), { recursive: true });
  const tmp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.${crypto.randomBytes(4).toString('hex')}.tmp`);
  const fh = await fsp.open(tmp, 'w');
  try {
    await fh.writeFile(`${JSON.stringify(value, null, 2)}\n`, 'utf8');
    await fh.sync();
  } finally {
    await fh.close();
  }
  try {
    await renameRetry(tmp, file);
  } catch (err) {
    await fsp.rm(tmp, { force: true }).catch(() => {});
    throw err;
  }
}

async function removeFile(file) {
  await fsp.rm(file, { force: true });
}

/**
 * renameRetry: rename, retrying for a while on Windows sharing errors (a
 * virus scanner looking at a fresh file, a process that is still exiting).
 */
async function renameRetry(from, to, { tries = 20, delayMs = 100 } = {}) {
  let last;
  for (let i = 0; i < tries; i++) {
    try {
      await fsp.rename(from, to);
      return;
    } catch (err) {
      last = err;
      if (!err || !['EPERM', 'EACCES', 'EBUSY'].includes(err.code)) throw err;
      await sleep(delayMs);
    }
  }
  throw last;
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// ---- files and hashes ----

/** hashFile(file) → { size, sha256 } (hex, lower case). */
function hashFile(file) {
  return new Promise((resolve, reject) => {
    const h = crypto.createHash('sha256');
    let size = 0;
    const s = fs.createReadStream(file, { highWaterMark: 1 << 20 });
    s.on('data', (b) => {
      size += b.length;
      h.update(b);
    });
    s.on('error', reject);
    s.on('end', () => resolve({ size, sha256: h.digest('hex') }));
  });
}

/**
 * checkFile(file, size, sha256) → null when the file is a regular file with
 * exactly that size and SHA-256, else a short English reason.
 */
async function checkFile(file, size, sha256) {
  let st;
  try {
    st = await fsp.stat(file);
  } catch (err) {
    return `${path.basename(file)} is missing (${err.code || 'error'})`;
  }
  if (!st.isFile()) return `${path.basename(file)} is not a file`;
  if (st.size !== size) return `${path.basename(file)} has ${st.size} bytes, not ${size}`;
  const h = await hashFile(file);
  if (h.size !== size || h.sha256 !== sha256) return `${path.basename(file)} is damaged (SHA-256 mismatch)`;
  return null;
}

/**
 * treeHash(root) → { sha256, files, bytes } of a file or a directory tree:
 * the SHA-256 of one line per entry, in name order — "d <path>",
 * "f <path>\0<size>\0<sha256>", "l <path>\0<link target>" — with paths
 * relative to root and "/"-separated. Symbolic links are not followed
 * (a macOS .app holds some); anything else (a device, a socket) is an
 * error. The same tree gives the same hash wherever it lies.
 */
async function treeHash(root) {
  const lines = [];
  let files = 0;
  let bytes = 0;
  async function walk(abs, rel) {
    const st = await fsp.lstat(abs);
    if (st.isSymbolicLink()) {
      lines.push(`l ${rel}\0${await fsp.readlink(abs)}`);
    } else if (st.isDirectory()) {
      lines.push(`d ${rel}`);
      const names = (await fsp.readdir(abs)).sort();
      for (const n of names) await walk(path.join(abs, n), rel ? `${rel}/${n}` : n);
    } else if (st.isFile()) {
      const h = await hashFile(abs);
      files += 1;
      bytes += h.size;
      lines.push(`f ${rel}\0${h.size}\0${h.sha256}`);
    } else {
      throw new Error(`${rel || path.basename(abs)} is not a file, folder or link`);
    }
  }
  await walk(root, '');
  return { sha256: crypto.createHash('sha256').update(lines.join('\n'), 'utf8').digest('hex'), files, bytes };
}

/**
 * treeStat(root, { maxFiles, maxBytes, maxEntries, maxDepth }) →
 * { files, bytes, entries } of a file or a directory tree without reading
 * any file (lstat only, links not followed). It throws as soon as the tree
 * has more regular files, more bytes, more entries (files, folders, links)
 * or more levels than the bounds: a tree someone filled up is refused
 * before it is copied or hashed, at a cost that does not grow with what
 * was added.
 */
async function treeStat(root, { maxFiles = 200000, maxBytes = 16 * 1024 * 1024 * 1024, maxEntries = 400000, maxDepth = 64 } = {}) {
  let files = 0;
  let bytes = 0;
  let entries = 0;
  const stack = [{ abs: root, depth: 0 }];
  while (stack.length) {
    const { abs, depth } = stack.pop();
    const st = await fsp.lstat(abs);
    entries += 1;
    if (entries > maxEntries) throw new Error(`more than ${maxEntries} entries`);
    if (st.isSymbolicLink()) continue;
    if (st.isFile()) {
      files += 1;
      bytes += st.size;
      if (files > maxFiles) throw new Error(`more than ${maxFiles} files`);
      if (bytes > maxBytes) throw new Error(`more than ${maxBytes} bytes`);
    } else if (st.isDirectory()) {
      if (depth >= maxDepth) throw new Error(`deeper than ${maxDepth} levels`);
      const names = await fsp.readdir(abs);
      if (entries + stack.length + names.length > maxEntries) throw new Error(`more than ${maxEntries} entries`);
      for (const n of names) stack.push({ abs: path.join(abs, n), depth: depth + 1 });
    } else {
      throw new Error(`${path.basename(abs)} is not a file, folder or link`);
    }
  }
  return { files, bytes, entries };
}

async function exists(p) {
  try {
    await fsp.lstat(p);
    return true;
  } catch {
    return false;
  }
}

async function isDir(p) {
  try {
    return (await fsp.stat(p)).isDirectory();
  } catch {
    return false;
  }
}

// ---- the trust directory ----

/**
 * trustDirFor({ platform, env, home }) → where the shell keeps its update
 * record (trust.json) outside the data folder, or null where it stays in
 * the profile (macOS: no Mac to test a move on, so it keeps the place it
 * had; Linux has no in-app update).
 *   Windows  %LOCALAPPDATA%\LectureFlow\selfupdate (installed and portable
 *            alike; like %LOCALAPPDATA%\LectureFlow\nmhost). Electron's
 *            getPath has no Local AppData, so it comes from the
 *            environment, else <home>\AppData\Local; null when neither is
 *            an absolute path. Not moved with the data folder, not
 *            affected by LECTUREFLOW_DATA_DIR; the service program refuses
 *            it when it lies in the data folder (-selfupdate-trust-dir).
 */
function trustDirFor({ platform, env = {}, home = '' }) {
  if (platform !== 'win32') return null;
  const local = typeof env.LOCALAPPDATA === 'string' ? env.LOCALAPPDATA.trim() : '';
  let base = null;
  if (local && path.win32.isAbsolute(local) && !local.includes('\0')) base = local;
  else if (typeof home === 'string' && home && path.win32.isAbsolute(home) && !home.includes('\0')) base = path.win32.join(home, 'AppData', 'Local');
  return base ? path.win32.join(base, 'LectureFlow', 'selfupdate') : null;
}

/**
 * roamingAppDataFor({ platform, env }) → this account's %APPDATA% (Roaming)
 * as the system sets it, or null: only on Windows, and only an absolute
 * path with a drive letter (not relative, not a network path). It serves
 * one decision: whether the data folder is the default one,
 * %APPDATA%\LectureFlow (selfupdate.js _migrateTrust); unknown counts as
 * "not the default".
 */
function roamingAppDataFor({ platform, env = {} }) {
  if (platform !== 'win32') return null;
  const v = typeof env.APPDATA === 'string' ? env.APPDATA.trim() : '';
  return v && /^[A-Za-z]:[\\/]/.test(v) && !v.includes('\0') ? v : null;
}

// ---- this installation ----

/** isUninstaller(name): the NSIS uninstaller electron-builder puts next to the installed exe. */
function isUninstaller(name) {
  return /^Uninstall .+\.exe$/i.test(String(name));
}

/**
 * portableExecutable({ isPackaged, platform, execPath, env, readdirSync }) →
 * the portable executable (PORTABLE_EXECUTABLE_FILE, set by
 * electron-builder's portable launcher, trimmed) or '' when this is not the
 * portable build. The variable counts only for the packaged Windows app and
 * only when no installer's uninstaller lies next to the running executable
 * (security audit FB1 L-13): the portable build runs from a folder it
 * unpacked itself, an installation always has the uninstaller, so the
 * variable cannot turn an installation into a "portable build" and skip
 * what only an installation gets (installDirOf's permission check). A
 * folder that cannot be read does not count as portable either.
 */
function portableExecutable({ isPackaged, platform, execPath, env = {}, readdirSync = fs.readdirSync }) {
  if (!isPackaged || platform !== 'win32' || typeof execPath !== 'string' || !execPath) return '';
  const v = env && typeof env.PORTABLE_EXECUTABLE_FILE === 'string' ? env.PORTABLE_EXECUTABLE_FILE.trim() : '';
  if (!v) return '';
  let names;
  try {
    names = readdirSync(path.dirname(execPath));
  } catch {
    return '';
  }
  if (!Array.isArray(names) || names.some(isUninstaller)) return '';
  return v;
}

/**
 * describeInstallation → what an update replaces here:
 *   null                  not packaged (development): nothing is touched
 *   { unsupported }       a packaged copy that cannot be updated in place
 *                         (English reason, shown as the update's error)
 *   { platform, kind, kinds, target, exe }
 *     Windows installed   kind installer, target = the installation directory
 *                         (only with the NSIS uninstaller next to the exe: an
 *                         unpacked or copied folder is not an installation)
 *     Windows portable    kind portable, target = the portable executable
 *                         (PORTABLE_EXECUTABLE_FILE, set by electron-builder;
 *                         ignored next to an uninstaller: portableExecutable)
 *     macOS               kind zip (an offline update may bring the dmg),
 *                         target = LectureFlow.app
 */
function describeInstallation({ isPackaged, platform, arch, execPath, env = {}, existsSync = fs.existsSync, readdirSync = fs.readdirSync }) {
  if (!isPackaged || typeof execPath !== 'string' || !execPath) return null;
  const exe = path.basename(execPath);
  const byHand = 'this copy of LectureFlow cannot update itself; install the new version by hand';
  if (platform === 'win32' && arch === 'x64') {
    const portable = portableExecutable({ isPackaged, platform, execPath, env, readdirSync });
    if (portable) {
      if (!path.win32.isAbsolute(portable) || !/\.exe$/i.test(portable)) return { unsupported: byHand };
      return { platform: PLATFORMS.windows, kind: KINDS.portable, kinds: [KINDS.portable], target: path.win32.normalize(portable), exe };
    }
    const dir = path.dirname(execPath);
    let names = [];
    try {
      names = readdirSync(dir);
    } catch {
      return { unsupported: byHand };
    }
    if (!names.some(isUninstaller) || !existsSync(path.join(dir, 'resources'))) {
      return { unsupported: 'this copy of LectureFlow was not set up by its installer; install the new version by hand' };
    }
    return { platform: PLATFORMS.windows, kind: KINDS.installer, kinds: [KINDS.installer], target: dir, exe };
  }
  if (platform === 'darwin' && arch === 'arm64') {
    const app = path.resolve(execPath, '..', '..', '..');
    if (!/\.app$/i.test(app) || path.basename(path.dirname(execPath)) !== 'MacOS') return { unsupported: byHand };
    return { platform: PLATFORMS.darwin, kind: KINDS.zip, kinds: [KINDS.zip, KINDS.dmg], target: app, exe };
  }
  return { unsupported: `in-app updates are not available on ${platform}-${arch}` };
}

/**
 * installDirOf(install) → the installation directory whose permissions the
 * service program checks (security audit M-4: -install-dir for the
 * diagnostics and the update's downloads, -selfupdate-install-dir before
 * anything is installed or rolled back), or null when nothing is checked:
 * only the Windows installation set up by the installer (kind installer,
 * describeInstallation's target) has one. Development (install null), the
 * portable build (its executable lies wherever the user keeps it) and
 * macOS (no Mac to test on: a known limitation) have none.
 */
function installDirOf(install) {
  if (!install || install.unsupported || install.platform !== PLATFORMS.windows || install.kind !== KINDS.installer) return null;
  return typeof install.target === 'string' && install.target ? install.target : null;
}

/**
 * nsisArguments(installDir) → the arguments of a silent electron-builder
 * NSIS installer run (per user, no elevation), to be passed verbatim:
 * --updated (keeps the shortcuts and the data, passes --updated on to the
 * app), /S (silent), --force-run (start the app when done), and "/D=" last
 * and unquoted (NSIS reads everything after it as the directory, spaces
 * included).
 */
function nsisArguments(installDir) {
  const args = ['--updated', '/S', '--force-run'];
  if (installDir) args.push(`/D=${installDir}`);
  return args;
}

/** shortError(err | text) → one line of at most 300 characters. */
function shortError(e) {
  let s = e && typeof e === 'object' && 'message' in e ? String(e.message) : String(e == null ? '' : e);
  s = s.replace(/\s+/g, ' ').trim();
  return s.length > 300 ? `${s.slice(0, 299)}…` : s;
}

module.exports = {
  fs,
  fsp,
  SCHEMA,
  FILES,
  BACKUP_DIR,
  BACKUP_MARK,
  TRUST_FILE,
  TRUST_DIR_FILE,
  TRUST_MIGRATED_FILE,
  DB_FILES,
  PLATFORMS,
  KINDS,
  MAC_BUNDLE_ID,
  parseVersion,
  compareVersions,
  sameVersion,
  resolveInside,
  relativeTo,
  readJson,
  writeJsonAtomic,
  removeFile,
  renameRetry,
  sleep,
  hashFile,
  checkFile,
  treeHash,
  treeStat,
  exists,
  isDir,
  trustDirFor,
  roamingAppDataFor,
  describeInstallation,
  portableExecutable,
  installDirOf,
  nsisArguments,
  shortError,
};
