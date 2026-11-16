// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Installing in-app updates at the next start (SPEC-ext "应用内更新" and
// M5; the contract with the Go side is internal/selfupdate/README.md "The
// updates directory"). Go verifies a package and leaves pending.json in
// {data_dir}/updates; this module, run by the shell before it starts the
// service, installs it, keeps the previous version and goes back to it when
// the new version does not start.
//
//   start (old version)  pending.json valid and vouched for by the installed
//                        service program (-selfupdate-verify, its built-in
//                        public key: lib/verifyupdate.js), which also copies
//                        the package into a private temporary folder: only
//                        that copy is run (security review sec-update M3) →
//                        back up what previous.json lacks, snapshot the
//                        database, record the installation in the trust
//                        file, rename pending.json to applying.json,
//                        install, quit:
//                          installer  NSIS --updated /S --force-run /D=<dir>
//                                     (it closes this app, installs per user
//                                     and starts the new version)
//                          portable   replace PORTABLE_EXECUTABLE_FILE, start it
//                          zip / dmg  unpack the .app next to the current one,
//                                     check it, swap, keep the old one in
//                                     backup/, start the new one
//   start (new version)  applying.json names this version → probation: the
//                        shell starts the service; once it is healthy and the
//                        window has loaded, result.json "installed". A failed
//                        start is retried once ("two starts in a row"); then
//                        the database snapshot and the previous version are
//                        put back (applying.json "rolling_back") and the
//                        previous version is started.
//   start (old again)    applying.json "rolling_back" → result.json
//                        "rolled_back" with the reason; Go shows it.
//
// What a rollback runs is checked like an update (sec-update M2): the
// previous version's package by the service program (-selfupdate-verify
// -selfupdate-previous: signature, size, SHA-256, older version, run from
// a private copy); a backup of the installation by the hash of its whole
// tree recorded when this shell made it. The service program also says
// whether other accounts can change the data folder or anything below its
// updates folder (exit 3): then nothing in it is installed or rolled back.
// For the Windows installation set up by the installer every check also
// names the installation directory (-selfupdate-install-dir,
// F.installDirOf; security audit M-4): while other accounts can change it
// (or move it or a folder above it away) the answer is exit 3
// "untrusted_install_dir" and nothing is installed or rolled back either
// (pending.json is kept: once the folder is fixed the update installs); the
// user installs the new version by hand.
//
// A backup is never run or restored from the data folder (security audit
// M-3):
//   - its recorded hash is the hash of the installation it was copied
//     from, and the copy must have it once it is on the disk (a file
//     swapped while the backup was being made is noticed then or at the
//     rollback);
//   - a backup that is already there serves an update again only while its
//     recorded hash is the hash of the installation as it is now; any
//     other backup or record of the running version is dropped before the
//     update is installed (one prepared together with its record is not a
//     copy of what is installed);
//   - a rollback copies the backup into a new folder only this account can
//     use (made by the service program, -selfupdate-private:
//     <tmp>/lectureflow-update-<n>/app), checks that copy against the
//     recorded hash and uses only the copy: nothing can change it between
//     the check and its use;
//   - the Windows installation is put back by a copy of the failed
//     version (the program that runs now) in the same private folder
//     (…/helper, started with --lf-restore), not by the backup's own
//     program: no code of the backup runs before the backup is in place,
//     and the restore does not depend on what an older version's restore
//     program accepts.
//
// The trust file is this module's own record. On Windows it is
// F.TRUST_DIR_FILE in a folder outside the data folder
// (%LOCALAPPDATA%\LectureFlow\selfupdate, F.trustDirFor; security review
// SEC17 中-B): a profile in the data folder could have been prepared by
// another account that once could write there, tracks and all. Every run
// of the service program's check names that folder
// (-selfupdate-trust-dir) and refuses it when it lies in the data folder,
// is a link or others can change it; the profile's record of an older
// version is looked at once (_migrateTrust). Elsewhere (macOS, a known
// limitation: no Mac to test on) it stays F.TRUST_FILE in the shell's
// profile (userData), which the check covers when it lies in the data
// folder. Its content:
//   install   { version, from_version, target, kind, started_at }: the
//             installation this shell started. applying.json counts only
//             when it matches (a planted applying.json is ignored), and its
//             target must be this installation's place (one that names
//             another place is another copy's, sharing the data folder:
//             left exactly as it is for that copy). A rollback from a
//             backup adds restore { dir, tree }: the private folder with
//             the checked copy (app) and the restore program (helper), and
//             the hash the copy has.
//   backups   { <version>: { dir, kind, item, tree, files, bytes, created_at } }:
//             the backups this shell made (tree: F.treeHash of the
//             installation, which backup/<v>/app/<item> had when it was
//             recorded).
// Why a hash rather than a code signature: builds may be unsigned, and a
// hash pins this very copy (a signature would let any older signed version
// pass); the profile is the user's own.
//
// applying.json is this module's own file (Go never reads it): pending.json's
// fields plus
//   phase       installing | rolling_back
//   tries       starts of the new version so far (at most MAX_TRIES)
//   env_refunds starts given back by refundTry so far (at most MAX_REFUNDS;
//               anything but a whole number in 0..MAX_REFUNDS: used up)
//   target      what is replaced: installation directory, portable .exe or .app
//   exe         the executable's file name
//   started_at  when the installation began
//   db_snapshot backup/<from>/db (relative), the database before the update
//   db_kept     a rollback decided about keeping the database it replaces
//               (true: kept as lectureflow.db.lf-before-rollback; false:
//               not); a rollback that starts over does not decide again
//   pid, pid_at the installer or restore program started for it, and when
//   error       why the new version is being rolled back (or the last failed start)
//   restore     file | backup: what the rollback restores
//   restore_error  why the rollback failed (written by the restore program)
//
// Nothing here is installed while the service runs: the shell calls atStart
// before it starts the service, and stops the service before failed(), so no
// task can be running (SPEC-v3 V8). No Electron imports: effects that need
// Electron (single-instance lock, the profile folder) or start programs come
// in as ops.
'use strict';

const childProcess = require('node:child_process');
const { constants: FS } = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const F = require('./lib/updatefiles');
const { APP_DIR_NAME } = require('./lib/datadir');
const { interpretVerify, interpretPrivateDir, interpretTrustCheck, COPY_DIR_RE } = require('./lib/verifyupdate');

const { fsp } = F;

const MAX_TRIES = 2;
// Starts of the new version that failed for a reason outside it
// (refundTry) and were not counted as tries, at most this many per
// installation: a defect of the new version can produce the same codes
// (security review SEC12 M-1), so after these the failures count again and
// the update is rolled back as usual.
const MAX_REFUNDS = 3;
// An installer or restore program started this recently may still be working.
const HELPER_WAIT_MS = 10 * 60 * 1000;
// The look at the installation folder after a rollback put it back
// (_installDirAfterRestore): the same service program has just answered
// for the private copy, so an answer this late means it will not come;
// the old version is not kept waiting for the update check's full limit
// (security review SEC20 low-D).
const RESTORE_CHECK_TIMEOUT_MS = 15 * 1000;
const RESTORE_ARG = '--lf-restore';
const UPDATED_ARG = '--updated';
const CONTINUE = Object.freeze({ action: 'continue' });
// What a rollback keeps of the database it replaces (_restoreDb): this
// name in the data folder, and -wal / -shm after it.
const DB_BEFORE_ROLLBACK_NAME = `${F.DB_FILES[0]}.lf-before-rollback`;

/**
 * rmRetry(file): remove a file (no error when there is none), retrying
 * for a while on Windows sharing errors like F.renameRetry (a scanner or
 * an indexer looking at it, a process that is still exiting).
 */
async function rmRetry(file, { tries = 20, delayMs = 100 } = {}) {
  let last;
  for (let i = 0; i < tries; i++) {
    try {
      await fsp.rm(file, { force: true });
      return;
    } catch (err) {
      last = err;
      if (!err || !['EPERM', 'EACCES', 'EBUSY'].includes(err.code)) throw err;
      await new Promise((r) => setTimeout(r, delayMs));
    }
  }
  throw last;
}

/**
 * dbBroken(message) → the error of _restoreDb when the database is left
 * without its last log (two faults in a row): the rollback must not go
 * on over it (_rollback).
 */
function dbBroken(message) {
  return Object.assign(new Error(message), { dbBroken: true });
}

/** keptDbName('lectureflow.db-wal') → 'lectureflow.db.lf-before-rollback-wal'. */
function keptDbName(name) {
  return `${DB_BEFORE_ROLLBACK_NAME}${name.slice(F.DB_FILES[0].length)}`;
}

function stamp(d) {
  return d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
}

/**
 * plainDir(p) → p is a directory itself, not a link to one (lstat: Node
 * reports a Windows junction as a symbolic link, so neither counts).
 */
async function plainDir(p) {
  try {
    return (await fsp.lstat(p)).isDirectory();
  } catch {
    return false;
  }
}

function samePath(a, b) {
  if (!a || !b || typeof a !== 'string' || typeof b !== 'string') return false;
  const na = path.resolve(a);
  const nb = path.resolve(b);
  return process.platform === 'win32' ? na.toLowerCase() === nb.toLowerCase() : na === nb;
}

/**
 * refundsUsed(v): applying.json's env_refunds as a count; missing (an
 * installation started by an older shell, or none given back yet) is 0;
 * anything but a whole number in 0..MAX_REFUNDS (negative, too large, not
 * a number) counts as used up.
 */
function refundsUsed(v) {
  if (v === undefined) return 0;
  return Number.isInteger(v) && v >= 0 && v <= MAX_REFUNDS ? v : MAX_REFUNDS;
}

function validApplying(v) {
  return !!v && v.schema === F.SCHEMA && !!F.parseVersion(v.version) && !!F.parseVersion(v.from_version)
    && (v.phase === undefined || v.phase === 'installing' || v.phase === 'rolling_back');
}

/**
 * otherCopysPending(install, p) → pending.json p (a platform and a kind)
 * is the update of another copy of LectureFlow than install: for a copy
 * that cannot update itself every one; else on Windows, where the
 * installed copy and the portable copy share the default data folder, the
 * package of the other one (installer / portable). Nothing else is: on
 * macOS the zip and the dmg are both this installation's, and a platform
 * or kind that no copy here takes is simply not installable (dropped and
 * reported). The same rule as the service's (internal/selfupdate
 * othercopy.go otherCopys); a table in both tests keeps them together.
 */
function otherCopysPending(install, p) {
  // a copy that cannot update itself installs none: whatever waits there
  // is another copy's (the service is told: -no-self-update)
  if (install.unsupported) return true;
  return install.platform === F.PLATFORMS.windows && p.platform === install.platform
    && (p.kind === F.KINDS.installer || p.kind === F.KINDS.portable) && !install.kinds.includes(p.kind);
}

class SelfUpdateApplier {
  /**
   * install: lib/updatefiles.describeInstallation() — null in development
   *   (nothing is touched), { unsupported } for a packaged copy that cannot be
   *   updated in place, else { platform, kind, kinds, target, exe }.
   * currentVersion: the running version (app.getVersion()).
   * dataDir: the service's data directory; updates are in {dataDir}/updates.
   * privateDir: the shell's profile (default ops.privateDir()); the trust
   *   file lives there (F.TRUST_FILE) unless trustDir is given.
   * trustDir: where the trust file lives instead (default ops.trustDir():
   *   Windows %LOCALAPPDATA%\LectureFlow\selfupdate, F.trustDirFor; null
   *   elsewhere): F.TRUST_DIR_FILE, believed only while the service
   *   program vouches for the folder (-selfupdate-trust-dir on every
   *   check); the profile's record of an older version is looked at once
   *   (_migrateTrust). tmpDir: where the service program puts the private
   *   copies (default ops.tmpDir(), else the system's temporary folder).
   *   appDataDir: this account's %APPDATA% (default ops.appDataDir(),
   *   F.roamingAppDataFor; null when unknown), only for _migrateTrust.
   * ops: {
   *   spawnDetached(file, args, { verbatim }) → Promise<pid>  a program that outlives this app
   *   run(file, args) → Promise<{ stdout }>                   helper programs (macOS: ditto, codesign, …)
   *   verifyPending(updatesDir, version, { previous, copyTo, privateDir }) → Promise<{ code, stdout, stderr, timedOut, error }>
   *                                                           the installed service program's
   *                                                           -selfupdate-verify (lib/verifyupdate.js);
   *                                                           privateDir: -selfupdate-private
   *   releaseLock()                                           let the next instance take the single-instance lock
   *   pidAlive(pid) → boolean
   *   privateDir() → string, trustDir() → string|null, tmpDir() → string,
   *   appDataDir() → string|null                              (optional, see above)
   * }
   */
  constructor({ install, currentVersion, dataDir, ops, log, now = () => new Date(), privateDir = null, trustDir = null, tmpDir = null, appDataDir = null }) {
    this.install = install;
    this.version = currentVersion;
    this.dataDir = dataDir;
    this.dir = path.join(dataDir, 'updates');
    this.ops = ops;
    this.log = log || { info() {}, warn() {}, error() {} };
    this.now = now;
    this.privateDir = privateDir || (ops && typeof ops.privateDir === 'function' ? ops.privateDir() : null);
    this.trustDir = trustDir || (ops && typeof ops.trustDir === 'function' ? ops.trustDir() : null) || null;
    this.tmpDir = path.resolve(tmpDir || (ops && typeof ops.tmpDir === 'function' ? ops.tmpDir() : null) || os.tmpdir());
    this.appDataDir = appDataDir || (ops && typeof ops.appDataDir === 'function' ? ops.appDataDir() : null) || null;
    this.applying = null; // the applying.json of a probation in progress
    this.restoreCheckTimeoutMs = RESTORE_CHECK_TIMEOUT_MS;
  }

  file(name) {
    return path.join(this.dir, F.FILES[name]);
  }

  /**
   * _looksAfterShared → this copy looks after what lies outside its data
   * folder and is shared by every copy of LectureFlow of this account: the
   * trust folder (Windows %LOCALAPPDATA%\LectureFlow\selfupdate, not
   * moved by LECTUREFLOW_DATA_DIR), the profile's record of an older
   * version and the private copies in the temporary folder. A copy that
   * cannot update itself (install.unsupported: an unpacked or copied
   * folder, a smoke test's run) never makes any of them, so at its start
   * it neither reads nor changes nor removes them (packaged smoke test
   * T14, C132); in its data folder it leaves applying.json and a pending
   * update as they are too (_resolveApplying, _checkPending: another copy
   * may share the folder). Started by hand with --lf-restore,
   * restoreFromBackup still reads the trust file once and then refuses
   * (not the recorded restore program); it changes nothing (SEC29).
   */
  _looksAfterShared() {
    return !!this.install && !this.install.unsupported;
  }

  backupDir(version) {
    return path.join(this.dir, F.BACKUP_DIR, version);
  }

  // ---- at start ----

  /**
   * atStart({ background }) → what the shell does next:
   *   { action: 'continue' }   start the service as usual
   *   { action: 'probation' }  start it; report healthy() or failed(reason)
   *   { action: 'exit' }       an installer / the other version was started: quit
   * background: started by the browser extension's host without a window;
   * a pending update then waits for a normal start.
   * A copy that cannot update itself changes nothing: it leaves
   * applying.json (always another copy's: _resolveApplying), a pending
   * update (_checkPending) and what other copies share
   * (_looksAfterShared) alone. A copy that can leaves alone what belongs
   * to another copy sharing its data folder: applying.json of another
   * place, a pending update of a kind it does not take.
   */
  async atStart({ background = false } = {}) {
    if (!this.install) return CONTINUE;
    const shared = this._looksAfterShared();
    try {
      await this._cleanupLeftovers();
      if (shared) {
        await this._cleanupCopies();
        await this._migrateTrust();
      }
      const read = await F.readJson(this.file('applying'));
      if (read.exists) {
        const r = await this._resolveApplying(read);
        if (r) return r;
      } else if (shared) {
        await this._dropStaleInstall();
      }
      return await this._handlePending({ background });
    } catch (err) {
      // Never keep the app from starting.
      this.log.error(`selfupdate: ${F.shortError(err)}`);
      return CONTINUE;
    }
  }

  async _resolveApplying(read) {
    if (this.install.unsupported) {
      // Only a copy that installs writes applying.json (_install), never
      // this one: it is another copy's (the installed or the portable
      // one, sharing this data folder), on probation or rolling back.
      // It, its backups and its database snapshot are left exactly as they
      // are for that copy (which also drops it when it is not its own);
      // finishing it here would end that copy's probation and leave its
      // trust record behind for a matching applying.json put back later
      // (security review SEC29 低-1). Nothing is reported: nothing of this
      // copy's happened. (A pending update is left alone likewise:
      // _checkPending.)
      this.log.info('selfupdate: applying.json is another copy\'s (this copy cannot update itself); left alone');
      return null;
    }
    if (read.error || !validApplying(read.value)) {
      this.log.warn(`selfupdate: ignoring applying.json (${read.error || 'not a valid applying.json'})`);
      await F.removeFile(this.file('applying'));
      return null;
    }
    const a = read.value;
    const v = this.version;
    if (this._elsewhere(a.target)) {
      // The installation of another copy of this account that shares the
      // data folder (the installed and the portable one both use
      // %APPDATA%\LectureFlow by default), on probation or rolling back:
      // only that copy finishes it. It, the backups, the database snapshot
      // and previous.json stay exactly as they are and nothing is
      // reported; removing it here took away that copy's way back (r6;
      // security review SEC29 第六节 #6, S1). Nothing in it is run here
      // either: _foreignApplying would refuse it for its target. One whose
      // copy is gone stays until a copy installs an update (_install
      // replaces it); Go never reads it.
      this.log.info(`selfupdate: applying.json is another copy's (${a.from_version} → ${a.version} at ${a.target}); left alone`);
      return null;
    }
    const foreign = await this._foreignApplying(a);
    if (foreign) {
      // Not an installation this shell started (or not of this place):
      // nothing in it is run.
      this.log.warn(`selfupdate: ignoring applying.json (${a.from_version} → ${a.version}): ${foreign}`);
      await F.removeFile(this.file('applying'));
      return null;
    }
    if (a.phase === 'rolling_back') {
      if (F.sameVersion(v, a.from_version)) {
        await this._finish(a, 'rolled_back', a.error || `LectureFlow ${a.version} did not start`);
        return null;
      }
      if (F.sameVersion(v, a.version)) {
        if (this._helperRunning(a)) return this._waitForHelper(a);
        const why = a.restore_error ? `: ${a.restore_error}` : '';
        await this._finish(a, 'failed', `${a.error || `LectureFlow ${a.version} did not start`}; going back to ${a.from_version} failed${why}`);
        return null;
      }
    } else {
      if (F.sameVersion(v, a.version)) return this._beginProbation(a);
      if (F.sameVersion(v, a.from_version)) {
        if (this._helperRunning(a)) return this._waitForHelper(a);
        await this._finish(a, 'failed', a.error || `the installer did not install ${a.version}; ${v} is still installed`);
        return null;
      }
    }
    // Neither version runs: something else was installed by hand.
    this.log.warn(`selfupdate: ${v} runs, applying.json is about ${a.from_version} → ${a.version}; dropping it`);
    await F.removeFile(this.file('applying'));
    return null;
  }

  /**
   * _elsewhere(target) → target names a place (an absolute path) that is
   * not where this copy of LectureFlow is installed: what goes with it
   * belongs to another copy. Anything else (this place, no path, a
   * relative one) does not.
   */
  _elsewhere(target) {
    return typeof target === 'string' && path.isAbsolute(target) && !samePath(target, this.install.target);
  }

  /**
   * _foreignApplying(a) → why applying.json is not this shell's own
   * installation of this place (null when it is): the trust file must
   * record the same versions and target, and the target must be where this
   * copy of LectureFlow is installed.
   */
  async _foreignApplying(a) {
    if (!samePath(a.target, this.install.target)) return 'its target is not where LectureFlow is installed';
    const rec = (await this._readTrust()).install;
    if (!rec) return 'this LectureFlow did not start an installation (no record in its profile)';
    if (!F.sameVersion(rec.version, a.version) || !F.sameVersion(rec.from_version, a.from_version) || !samePath(rec.target, a.target)) {
      return `the installation this LectureFlow started was ${rec.from_version} → ${rec.version}`;
    }
    return null;
  }

  _helperRunning(a) {
    if (!Number.isInteger(a.pid) || a.pid <= 0) return false;
    const at = Date.parse(a.pid_at || '');
    if (!Number.isFinite(at) || this.now().getTime() - at > HELPER_WAIT_MS) return false;
    return !!this.ops.pidAlive(a.pid);
  }

  _waitForHelper(a) {
    // The installer (or the restore program) will start LectureFlow itself.
    this.log.info(`selfupdate: the update to ${a.version} is still being ${a.phase === 'rolling_back' ? 'rolled back' : 'installed'} (pid ${a.pid}); this start ends`);
    return { action: 'exit', why: 'busy' };
  }

  async _beginProbation(a) {
    const tries = (Number.isInteger(a.tries) && a.tries > 0 ? a.tries : 0) + 1;
    if (tries > MAX_TRIES) {
      const r = await this._rollback(a, a.error || `LectureFlow ${a.version} did not start ${MAX_TRIES} times in a row`);
      return r.action === 'exit' ? r : CONTINUE;
    }
    a.tries = tries;
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
    // Written now so that the service (which reads result.json at its start)
    // reports it; a rollback replaces it.
    await this._writeResult(a, 'installed');
    this.applying = a;
    this.log.info(`selfupdate: LectureFlow ${a.version} starts for the first time after the update (try ${tries} of ${MAX_TRIES})`);
    return { action: 'probation', version: a.version, tries };
  }

  // ---- probation (new version) ----

  /** inProbation: the shell is starting a freshly installed version. */
  get inProbation() {
    return !!this.applying;
  }

  /** healthy: the service is up and the window has loaded the app. */
  async healthy() {
    const a = this.applying;
    if (!a) return;
    this.applying = null;
    try {
      await this._finish(a, 'installed');
      this.log.info(`selfupdate: LectureFlow ${a.version} is installed`);
    } catch (err) {
      this.log.warn(`selfupdate: finishing the update: ${F.shortError(err)}`);
    }
  }

  /**
   * failed(reason): the new version failed to start (the shell has stopped
   * the service). → { action: 'retry' } start once more,
   * { action: 'exit' } the previous version is being put back: quit,
   * { action: 'giveup', error } nothing to go back to: show the error.
   */
  async failed(reason) {
    const a = this.applying;
    if (!a) return { action: 'none' };
    this.applying = null;
    a.error = F.shortError(reason) || 'the new version did not start';
    try {
      if (a.tries < MAX_TRIES) {
        await F.writeJsonAtomic(this.file('applying'), a);
        this.log.warn(`selfupdate: LectureFlow ${a.version} did not start (${a.error}); trying once more`);
        return { action: 'retry' };
      }
      return await this._rollback(a, a.error);
    } catch (err) {
      this.log.error(`selfupdate: ${F.shortError(err)}`);
      return { action: 'giveup', error: F.shortError(err) };
    }
  }

  /**
   * refundTry(): this start of the new version failed for a reason outside
   * it (lib/startfail.js isEnvironmentFailure: the data folder is in use;
   * the database is locked, read-only, cannot be opened, the disk is full
   * or failing). It is no verdict on the version: the try _beginProbation
   * counted is given back (tries never goes below 0) and this start's
   * probation ends; the next start ("Try again" on the error page, or the
   * next launch) begins it again. The environment failure is not recorded
   * as error (which keeps the last real failure, if any, for a rollback).
   * At most MAX_REFUNDS times per installation (env_refunds in
   * applying.json): after that it returns false and the probation goes on,
   * so the shell reports the failure (failed()) and the usual rule rolls
   * the update back. Returns false (and touches nothing) outside a
   * probation.
   */
  async refundTry() {
    const a = this.applying;
    if (!a) return false;
    const used = refundsUsed(a.env_refunds);
    if (used >= MAX_REFUNDS) {
      this.log.warn(`selfupdate: LectureFlow ${a.version} did not start for a reason that looks outside it, ${MAX_REFUNDS} such starts were not counted already; this one counts`);
      return false;
    }
    this.applying = null;
    a.tries = Number.isInteger(a.tries) && a.tries > 0 ? a.tries - 1 : 0;
    a.env_refunds = used + 1;
    try {
      await F.writeJsonAtomic(this.file('applying'), a);
      this.log.info(`selfupdate: LectureFlow ${a.version} did not start for a reason outside it; this start does not count (${a.tries} of ${MAX_TRIES} tries used, ${a.env_refunds} of ${MAX_REFUNDS} such starts)`);
    } catch (err) {
      // not written: the try stays counted on disk (the safe side)
      this.log.warn(`selfupdate: ${F.shortError(err)}`);
    }
    return true;
  }

  // ---- rollback ----

  async _rollback(a, error) {
    error = F.shortError(error);
    this.log.warn(`selfupdate: going back to ${a.from_version}: ${error}`);
    const prev = await this._readPrevious();
    const src = await this._restoreSource(prev, a);
    if (!src.path) {
      await this._finish(a, 'failed', `${error}; ${src.error}`);
      return { action: 'giveup', error: `${error}; ${src.error}` };
    }
    try {
      await this._restoreDb(a);
    } catch (err) {
      this.log.warn(`selfupdate: restoring the database snapshot: ${F.shortError(err)}`);
      if (err && err.dbBroken) {
        // The database is left without its last log (see _restoreDb, (B)):
        // the previous version is not started over it. applying.json
        // stays as it is, so the next start goes through the rollback
        // again and finishes it; the snapshot and the kept copy stay.
        await this._dropSource(src);
        const msg = F.shortError(`${error}; going back to ${a.from_version} stopped: ${F.shortError(err)}`);
        await this._writeResult(a, 'failed', msg).catch((e) => this.log.warn(`selfupdate: ${F.shortError(e)}`));
        this.log.error(`selfupdate: ${msg}`);
        return { action: 'giveup', error: msg };
      }
      // The snapshot is not in place and the database is as the new
      // version left it, whole (see _restoreDb, (A)): the program goes
      // back as it always did then; the result says what did not.
      error = F.shortError(`${error}; the database of before the update was not put back (${F.shortError(err)}): it is as ${a.version} left it`);
    }
    a.phase = 'rolling_back';
    a.error = error;
    a.restore = src.kind;
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
    try {
      const pid = await this._restore(src, a, prev);
      if (pid) {
        a.pid = pid;
        a.pid_at = this.now().toISOString();
        await F.writeJsonAtomic(this.file('applying'), a);
      }
      return { action: 'exit' };
    } catch (err) {
      await this._dropSource(src);
      const msg = `${error}; going back to ${a.from_version} failed: ${F.shortError(err)}`;
      await this._finish(a, 'failed', msg);
      return { action: 'giveup', error: msg };
    }
  }

  /**
   * _restoreSource → { kind: 'file', path, packageKind, copy } (the
   * previous version's package, verified by the service program, as its
   * private copy) or { kind: 'backup', path, private, tree } (a backup this
   * shell recorded, as a private copy that has the recorded tree hash), or
   * { error }.
   */
  async _restoreSource(prev, a) {
    if (!prev) return { error: 'previous.json is missing' };
    if (!F.sameVersion(prev.version, a.from_version)) return { error: `previous.json describes ${prev.version}, not ${a.from_version}` };
    const checked = await this._verifyPrevious(prev);
    if (checked.error) return { error: checked.error };
    const file = checked.file;
    // (the backup is hashed as its private copy, not where it lies)
    const backup = await this._backupItem(prev, { hash: false });
    const order = this.install.kind === F.KINDS.zip ? [backup, file] : [file, backup];
    let pick = null;
    let backupWhy = '';
    for (const c of order) {
      if (!c) continue;
      if (c.kind !== 'backup') {
        pick = c;
        break;
      }
      const copy = await this._privateBackup(c);
      if (copy.path) {
        pick = copy;
        break;
      }
      backupWhy = copy.error;
      this.log.warn(`selfupdate: the backup of ${prev.version} is not used: ${copy.error}`);
    }
    if (file && pick !== file) await this._removeCopy(file.copy);
    if (pick) return pick;
    const why = [checked.why ? `its package: ${checked.why}` : '', backupWhy ? `its backup: ${backupWhy}` : ''].filter(Boolean).join('; ');
    return { error: `no package or backup of ${prev.version} to return to${why ? ` (${why})` : ''}` };
  }

  /**
   * _privateBackup(b) → { kind: 'backup', path, private, tree }: the
   * backup b (_backupItem) copied into a new folder only this account can
   * use (private: <tmp>/lectureflow-update-<n>, path: its app[/<item>]),
   * where the copy has the tree hash the trust file records — or { error }
   * (then the folder is gone again). What lies in the data folder is only
   * read, and only as much as was recorded: a backup with more files or
   * bytes is refused before anything is copied.
   */
  async _privateBackup(b) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') return { error: 'this copy of LectureFlow has no update check program' };
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, this._verifyOpts({ privateDir: true, copyTo: this.tmpDir }));
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    const made = interpretPrivateDir(run, { copyTo: this.tmpDir });
    if (!made.dir) return { error: made.error };
    const priv = made.dir;
    const dest = path.join(priv, 'app', b.item === '.' ? '' : b.item);
    try {
      const bound = (n, fallback) => (Number.isSafeInteger(n) && n >= 0 ? n : fallback);
      const maxFiles = bound(b.files, 200000);
      await F.treeStat(b.path, { maxFiles, maxBytes: bound(b.bytes, 16 * 1024 * 1024 * 1024), maxEntries: maxFiles + 100000 });
      await fsp.mkdir(path.join(priv, 'app'));
      const st = await fsp.lstat(b.path);
      if (st.isDirectory()) await fsp.cp(b.path, dest, { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
      else if (st.isFile()) await fsp.copyFile(b.path, dest);
      else throw new Error('it is not a file or folder');
      const h = await F.treeHash(dest);
      if (h.sha256 !== b.tree) throw new Error('it changed since it was made');
    } catch (err) {
      await this._removePrivate(priv);
      return { error: F.shortError(err) };
    }
    this.log.info(`selfupdate: the backup of LectureFlow ${b.version} was copied to ${priv} and has its recorded hash`);
    return { kind: 'backup', path: dest, private: priv, tree: b.tree };
  }

  /**
   * _verifyPrevious(prev) → { file } (null: no usable package; the data
   * folder is trusted, so a backup may be used) or { error } (no rollback
   * at all: other accounts can change the data folder, or the service
   * program gave no verdict, so that cannot be ruled out).
   */
  async _verifyPrevious(prev) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') {
      return { error: 'this copy of LectureFlow has no update check program to vouch for the previous version' };
    }
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, this._verifyOpts({ previous: true, copyTo: this.tmpDir }));
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    const expected = { version: prev.version, kind: prev.kind, file: prev.file, sha256: prev.sha256 };
    const v = interpretVerify(run, expected, { copyTo: this.tmpDir, what: 'previous' });
    const accepted = this.install.kind === F.KINDS.zip ? [F.KINDS.zip, F.KINDS.dmg] : [this.install.kind];
    switch (v.verdict) {
      case 'ok':
        if (!prev.file || !accepted.includes(prev.kind)) {
          await this._removeCopy(v.copy);
          return { file: null };
        }
        this.log.info(`selfupdate: the service program verified the package of ${prev.version} with its built-in key`);
        return { file: { kind: 'file', path: v.copy, packageKind: prev.kind, copy: v.copy } };
      case 'invalid':
      case 'not_configured':
        // (a build without a public key never runs a package it cannot verify)
        await this._removeCopy(v.copy);
        if (!prev.file) return { file: null };
        this.log.warn(`selfupdate: the package of ${prev.version} cannot be used: ${v.drop || v.keep}`);
        return { file: null, why: v.drop || v.keep };
      case 'untrusted':
        return { error: v.keep };
      default:
        return { error: `the previous version could not be checked: ${v.keep}` };
    }
  }

  /**
   * _previousFileUsable(prev): previous.json names a package of this kind
   * that is still there with its size and SHA-256 (then no backup of the
   * installation is made; the package itself is verified by the service
   * program before a rollback runs it).
   */
  async _previousFileUsable(prev) {
    if (!prev.file) return false;
    const file = F.resolveInside(this.dir, prev.file);
    if (!file) return false;
    const accepted = this.install.kind === F.KINDS.zip ? [F.KINDS.zip, F.KINDS.dmg] : [this.install.kind];
    if (!accepted.includes(prev.kind)) return false;
    const bad = await F.checkFile(file, prev.size, prev.sha256);
    if (bad) {
      this.log.warn(`selfupdate: the package of ${prev.version} cannot be used: ${bad}`);
      return false;
    }
    return true;
  }

  /**
   * _backupItem(prev, { hash }) → { kind: 'backup', path, item, tree,
   * files, bytes, version } for a complete backup of prev.version that
   * this shell made: backup.json, the trust file's record (same folder,
   * kind and item; tree, files and bytes are the record's) and, with hash
   * (the default), the tree hash of what it holds now. A rollback does not
   * use it where it lies (_privateBackup).
   */
  async _backupItem(prev, { hash = true } = {}) {
    if (!prev.backup) return null;
    const dir = F.resolveInside(this.dir, prev.backup);
    if (!dir) return null;
    const mark = await F.readJson(path.join(dir, F.BACKUP_MARK));
    const m = mark.value;
    if (!m || m.schema !== F.SCHEMA || !F.sameVersion(m.version, prev.version) || m.kind !== this.install.kind || typeof m.item !== 'string') return null;
    if (m.item.includes('/') || m.item.includes('\\') || m.item === '..') return null;
    const item = path.join(dir, 'app', m.item === '.' ? '' : m.item);
    const probe = this.install.kind === F.KINDS.installer ? path.join(item, this.install.exe) : item;
    if (!(await F.exists(probe))) return null;
    const rec = ((await this._readTrust()).backups || {})[prev.version];
    if (!rec || rec.dir !== F.relativeTo(this.dir, dir) || rec.kind !== m.kind || rec.item !== m.item || typeof rec.tree !== 'string') {
      this.log.warn(`selfupdate: the backup of ${prev.version} was not made by this LectureFlow (no record in its profile); it is not used`);
      return null;
    }
    if (hash) {
      let h;
      try {
        h = await F.treeHash(item);
      } catch (err) {
        this.log.warn(`selfupdate: reading the backup of ${prev.version}: ${F.shortError(err)}`);
        return null;
      }
      if (h.sha256 !== rec.tree) {
        this.log.warn(`selfupdate: the backup of ${prev.version} changed since it was made; it is not used`);
        return null;
      }
    }
    return { kind: 'backup', path: item, item: m.item, tree: rec.tree, files: rec.files, bytes: rec.bytes, version: prev.version };
  }

  /**
   * _backupOfInstallation(prev) → the backup of the running version that
   * may serve again (_backupItem), or null. It may only when the hash
   * recorded for it is the hash of the installation as it is now: the
   * record alone proves nothing while the trust file lies in a data folder
   * other accounts could once write to — a backup, its mark and its record
   * prepared there agree with each other, not with what is installed
   * (security review SEC17, 中-3). Any other backup or record of the
   * running version is dropped here, before an update is installed: the
   * folder backup/<version>/app with its mark, the trust file's record and
   * previous.json's "backup"; the caller makes a new one where one is
   * needed.
   */
  async _backupOfInstallation(prev) {
    const dir = this.backupDir(this.version);
    const rec = ((await this._readTrust()).backups || {})[this.version];
    const named = prev.backup !== undefined;
    if (!named && !rec && !(await F.exists(path.join(dir, 'app'))) && !(await F.exists(path.join(dir, F.BACKUP_MARK)))) return null;
    let why = 'it is incomplete, changed or not recorded';
    const b = await this._backupItem(prev);
    if (b) {
      try {
        if ((await F.treeHash(this.install.target)).sha256 === b.tree) return b;
        why = 'it is not a copy of the installation as it is now';
      } catch (err) {
        why = `the installation could not be hashed: ${F.shortError(err)}`;
      }
    }
    this.log.warn(`selfupdate: the backup of ${this.version} is dropped: ${why}`);
    await fsp.rm(path.join(dir, 'app'), { recursive: true, force: true });
    await fsp.rm(path.join(dir, F.BACKUP_MARK), { force: true });
    if (rec) {
      await this._updateTrust((t) => {
        delete t.backups[this.version];
      });
    }
    if (named) {
      delete prev.backup;
      await F.writeJsonAtomic(this.file('previous'), prev);
    }
    return null;
  }

  async _restore(src, a, prev) {
    const { kind, target } = this.install;
    if (kind === F.KINDS.installer) {
      if (src.kind === 'file') return this.ops.spawnDetached(src.path, F.nsisArguments(target), { verbatim: true });
      // A copy of this installation (the failed version: the program that
      // runs now) next to the checked copy of the previous one puts that
      // copy into place once this app has quit (restoreFromBackup). Both
      // lie in the private folder; the trust file says where.
      const helperDir = path.join(src.private, 'helper');
      await fsp.cp(target, helperDir, { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
      await this._updateTrust((t) => {
        if (!t.install) throw new Error('the installation is not recorded in the profile');
        t.install.restore = { dir: src.private, tree: src.tree };
      });
      this.ops.releaseLock();
      return this.ops.spawnDetached(path.join(helperDir, this.install.exe), [RESTORE_ARG]);
    }
    if (kind === F.KINDS.portable) {
      await this._replaceFile(target, src.path);
      await this._dropSource(src);
      this.ops.releaseLock();
      return this._spawnQuiet(target, [UPDATED_ARG]);
    }
    // macOS
    if (src.kind === 'backup') {
      try {
        await this._swapApp(src.path, null);
      } finally {
        await this._dropSource(src);
      }
    } else {
      try {
        await this._installMacPackage(src.path, src.packageKind, prev.version, null);
      } finally {
        await this._removeCopy(src.copy);
      }
    }
    this.ops.releaseLock();
    return this._spawnQuiet('/usr/bin/open', ['-n', target, '--args', UPDATED_ARG]);
  }

  /**
   * _restoreDb(a): put the database snapshot of the update back, as it
   * always did (file by file: a copy of the snapshot's file takes the
   * place of the data folder's, a file the snapshot lacks is removed).
   *
   * Before that the database as it is now is kept (_keepDb): a copy of
   * lectureflow.db as lectureflow.db.lf-before-rollback, with -wal and
   * -shm after that name, so SQLite opens the kept database with its log.
   * What was done since the snapshot — in the failed version, or by
   * another copy of LectureFlow sharing this data folder while the update
   * was on probation (security review SEC29b b-1) — is not lost for good.
   * One generation: a later rollback replaces it. It holds whatever
   * credentials the database held; LectureFlow never opens it
   * (internal/relocate knows the name, BeforeRollbackName: after a move of
   * the data folder the one left in the old folder loses them like the
   * old database).
   *
   * Keeping never stands in the rollback's way and never takes the
   * database from its place (security review SEC40, PROBE-1 to 3):
   *   - it is a copy: lectureflow.db stays under its name until the
   *     snapshot's file replaces it in one rename, so a failure or a kill
   *     at any point leaves the database or the snapshot there, never
   *     neither. (Renaming it away first left no database when the
   *     snapshot then could not be put in place.) A copy rather than a
   *     hard link: a link is the same file as long as the database is not
   *     replaced, and anything that then writes to one writes to both.
   *   - when the snapshot does not get into the database's place at all,
   *     this run's kept copy is removed again: the database itself is
   *     still there, as it was before this was added;
   *   - applying.json records that the database was kept (db_kept) before
   *     anything is replaced. A rollback that starts over after a kill
   *     ("rolling_back" is written only after the database) then leaves
   *     the kept copy alone: what it would find in the data folder may
   *     already be the snapshot.
   *
   * Putting the snapshot back never leaves a database beside the log of
   * another (review AUD2 L1 = SEC40b PB-2; it used to replace
   * lectureflow.db first and deal with the -wal afterwards: when the -wal
   * could then not be removed, or the process died in between, the
   * snapshot's database lay beside the failed version's log, which SQLite
   * applies to it — the new version's content again, or "malformed"). The
   * steps, each retried like a rename while a file is in use:
   *   1. the snapshot's files are copied beside the database under
   *      temporary names (<name>.lf-restore);
   *   2. the database as it is now is kept (above; after 1, so that on a
   *      disk with little room the snapshot gets it, not the kept copy);
   *   3. the -wal and -shm that lie there are renamed aside
   *      (<name>.lf-aside);
   *   4. the snapshot's database takes the place of lectureflow.db;
   *   5. the snapshot's -wal and -shm, if it has them, follow;
   *   6. what was moved aside is removed.
   * What the data folder holds when a step fails, or the process is
   * killed after it, and what happens next:
   *
   *   step     fails                              killed after it
   *   1 copy   database + log as they were (A)    as they were, temporary
   *                                               files (R)
   *   2 keep   as they were, nothing kept; goes   as they were (R; kept
   *            on with 3                          again unless recorded)
   *   3 aside  moved back: as they were (A)       database without its
   *                                               last log; log aside (R)
   *   4 swap   log moved back: as they were (A)   the snapshot's database,
   *                                               no log beside it (R)
   *   5 follow the snapshot's database without    the whole snapshot,
   *            its log (B)                        stray aside files (R)
   *   6 remove the whole snapshot; a stray        the whole snapshot (R:
   *            .lf-aside file, logged             the same again)
   *   undo of 3 or 4 fails too: the database without its last log, which
   *   lies beside it as .lf-aside (B)
   *
   *   (A) nothing of the snapshot is in place and this run's kept copy is
   *       removed; the error is thrown, _rollback goes on with the
   *       program as it always did when the snapshot was not put back and
   *       adds the reason to the rollback's error (result.json).
   *   (B) two faults in a row. The database is a valid one without its
   *       last log, never one with another's; the kept copy has it
   *       whole. The error says so (dbBroken): _rollback stops, reports
   *       "failed" and leaves applying.json, so the next start is (R).
   *   (R) the next start of this installation goes through the rollback
   *       again: applying.json still says "installing" with the tries
   *       used up ("rolling_back" is written after this function), and
   *       atStart runs before the service is started. Every step can be
   *       done again from any of these states and ends with the whole
   *       snapshot; db_kept keeps step 2 from running twice.
   * In every one of these states lectureflow.db is there and is a
   * database of its own: whatever opens it in between — another copy of
   * LectureFlow sharing the data folder, a database tool — finds at worst
   * one that lacks its last log.
   */
  async _restoreDb(a) {
    if (!a.db_snapshot) return;
    const dir = F.resolveInside(this.dir, a.db_snapshot);
    if (!dir || !(await F.isDir(dir))) return;
    const [main, ...sidecars] = F.DB_FILES;
    const snap = (n) => path.join(dir, n);
    const live = (n) => path.join(this.dataDir, n);
    const fresh = (n) => `${live(n)}.lf-restore`;
    const aside = (n) => `${live(n)}.lf-aside`;
    if (!(await F.exists(snap(main)))) {
      // (never made by _snapshotDb: a -wal without its database is no snapshot)
      this.log.warn('selfupdate: the database snapshot has no lectureflow.db; the database stays as it is');
      return;
    }
    const dropFresh = async () => {
      for (const n of F.DB_FILES) await rmRetry(fresh(n)).catch(() => {});
    };

    // 1. the snapshot, beside the database
    const has = [];
    try {
      for (const n of F.DB_FILES) {
        await rmRetry(fresh(n));
        if (!(await F.exists(snap(n)))) continue;
        await fsp.copyFile(snap(n), fresh(n));
        has.push(n);
      }
    } catch (err) {
      await dropFresh();
      throw err;
    }

    // 2. the database as it is now
    const keptNow = await this._keepDb(a);

    // 3. its log aside, 4. the snapshot's database in its place
    const moved = [];
    try {
      for (const n of sidecars) {
        try {
          await F.renameRetry(live(n), aside(n));
          moved.push(n);
        } catch (err) {
          if (!err || err.code !== 'ENOENT') throw err;
        }
      }
      await F.renameRetry(fresh(main), live(main));
    } catch (err) {
      // nothing of the snapshot is in place: back to how it was
      for (const n of moved.reverse()) {
        try {
          await F.renameRetry(aside(n), live(n));
        } catch (e2) {
          await dropFresh();
          throw dbBroken(`the snapshot was not put in place (${F.shortError(err)}) and ${path.basename(aside(n))} could not be moved back to ${n} (${F.shortError(e2)}): the database lacks its last log until then`);
        }
      }
      await dropFresh();
      if (keptNow) await this._dropKeptDb(a);
      throw err;
    }

    // 5. the snapshot's log
    for (const n of sidecars) {
      if (!has.includes(n)) continue;
      try {
        await F.renameRetry(fresh(n), live(n));
      } catch (err) {
        await dropFresh();
        throw dbBroken(`the snapshot's database is in place, its ${n} could not be put beside it (${F.shortError(err)})`);
      }
    }

    // 6. what was moved aside (also by a run that was killed)
    for (const n of sidecars) {
      await rmRetry(aside(n)).catch((err) => this.log.warn(`selfupdate: removing ${path.basename(aside(n))}: ${F.shortError(err)}`));
    }
    this.log.info('selfupdate: database snapshot restored');
  }

  /**
   * _keepDb(a) → true when this call kept the database as it is now
   * (_restoreDb); it never throws. Once per installation: applying.json
   * gets db_kept (true: kept, false: could not be or there was none) as
   * soon as it is decided, and a call that finds it does nothing.
   */
  async _keepDb(a) {
    if (a.db_kept !== undefined) {
      this.log.info(`selfupdate: this rollback began before; what it ${a.db_kept === true ? `kept as ${DB_BEFORE_ROLLBACK_NAME}` : 'did not keep of the database'} stays as it is`);
      return false;
    }
    let kept = false;
    try {
      kept = await this._copyDbAside();
    } catch (err) {
      this.log.warn(`selfupdate: the database as it was could not be kept (${F.shortError(err)}); going back continues`);
    }
    a.db_kept = kept;
    try {
      await F.writeJsonAtomic(this.file('applying'), a);
    } catch (err) {
      // Not recorded: a rollback that starts over could not tell this
      // copy from one it should make, so there is none.
      this.log.warn(`selfupdate: recording that the database was kept: ${F.shortError(err)}; it is not kept`);
      delete a.db_kept;
      if (kept) await this._removeKeptDb();
      return false;
    }
    if (kept) this.log.info(`selfupdate: the database as it was is kept as ${DB_BEFORE_ROLLBACK_NAME}`);
    return kept;
  }

  /**
   * _copyDbAside → true when the database files of the data folder were
   * copied to their kept names (keptDbName), false when there is no
   * database; throws when nothing new is kept. The files are copied under
   * temporary names first (<kept name>.lf-new), then take their places in
   * an order that never leaves a log beside another database, whatever
   * fails or wherever the process dies: the earlier generation's -wal and
   * -shm go, the database replaces the earlier one, then its -wal and
   * -shm follow (one that cannot follow is left out and logged: the kept
   * database then lacks what was only in it).
   */
  async _copyDbAside() {
    const live = (n) => path.join(this.dataDir, n);
    const kept = (n) => path.join(this.dataDir, keptDbName(n));
    const fresh = (n) => `${kept(n)}.lf-new`;
    const [main, ...sidecars] = F.DB_FILES;
    const copied = [];
    try {
      for (const n of F.DB_FILES) await fsp.rm(fresh(n), { force: true });
      for (const n of F.DB_FILES) {
        try {
          // (a clone where the file system has them, else an ordinary copy)
          await fsp.copyFile(live(n), fresh(n), FS.COPYFILE_FICLONE);
          copied.push(n);
        } catch (err) {
          if (!err || err.code !== 'ENOENT') throw err;
          if (n === main) return false;
        }
      }
      for (const n of sidecars) await fsp.rm(kept(n), { force: true });
      await F.renameRetry(fresh(main), kept(main));
    } catch (err) {
      for (const n of F.DB_FILES) await fsp.rm(fresh(n), { force: true }).catch(() => {});
      throw err;
    }
    for (const n of sidecars) {
      if (!copied.includes(n)) continue;
      try {
        await F.renameRetry(fresh(n), kept(n));
      } catch (err) {
        this.log.warn(`selfupdate: the database's file could not be kept (${n}: ${F.shortError(err)}); going back continues`);
        await fsp.rm(fresh(n), { force: true }).catch(() => {});
      }
    }
    return true;
  }

  /**
   * _dropKeptDb(a): the copy this run kept is not needed after all (the
   * snapshot did not replace the database): applying.json no longer says
   * it was kept, then the copy goes.
   */
  async _dropKeptDb(a) {
    delete a.db_kept;
    await F.writeJsonAtomic(this.file('applying'), a).catch(() => {});
    await this._removeKeptDb();
    this.log.info('selfupdate: the snapshot did not take the database\'s place; the database is as it was, no copy of it is kept');
  }

  async _removeKeptDb() {
    // (the database last: never a database beside the log of another)
    for (const n of [...F.DB_FILES].reverse()) {
      await fsp.rm(path.join(this.dataDir, keptDbName(n)), { force: true })
        .catch((err) => this.log.warn(`selfupdate: removing ${keptDbName(n)}: ${F.shortError(err)}`));
    }
  }

  /**
   * abandonRestore(reason): the restore program could not start its work
   * (the failed version did not quit); the next start reports it.
   */
  async abandonRestore(reason) {
    const read = await F.readJson(this.file('applying'));
    const a = read.value;
    if (!a || !validApplying(a) || a.phase !== 'rolling_back') return;
    a.restore_error = F.shortError(reason);
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
  }

  /**
   * restoreFromBackup: run by the copy of the failed version that
   * _restore put into the private folder (<private>/helper, started with
   * --lf-restore while the failed version quits): it moves the failed
   * installation aside, copies the checked copy of the previous one
   * (<private>/app) into its place and starts it. Only for the
   * installation the trust file records, from the private folder recorded
   * with it (install.restore), and only while that copy still has the
   * recorded hash; nothing of the data folder's backup is run or copied
   * here. Returns true when it replaced the installation or tried to.
   */
  async restoreFromBackup({ execPath }) {
    const read = await F.readJson(this.file('applying'));
    const a = read.value;
    if (!a || !validApplying(a) || a.phase !== 'rolling_back' || a.restore !== 'backup') {
      this.log.warn('selfupdate: --lf-restore without a rollback in applying.json');
      return false;
    }
    const myDir = path.dirname(execPath);
    const target = a.target;
    const rec = (await this._readTrust()).install;
    if (typeof target !== 'string' || !path.isAbsolute(target) || samePath(target, myDir)
      || !rec || !F.sameVersion(rec.version, a.version) || !F.sameVersion(rec.from_version, a.from_version) || !samePath(rec.target, target)) {
      this.log.warn('selfupdate: --lf-restore for an installation this LectureFlow did not record; nothing is replaced');
      return false;
    }
    const rs = rec.restore;
    if (!rs || typeof rs.dir !== 'string' || typeof rs.tree !== 'string' || !this._isPrivateDir(rs.dir) || !samePath(myDir, path.join(rs.dir, 'helper'))) {
      this.log.warn('selfupdate: --lf-restore from a copy that is not the recorded restore program; nothing is replaced');
      return false;
    }
    const source = path.join(rs.dir, 'app');
    const exe = path.basename(execPath);
    try {
      const h = await F.treeHash(source);
      if (h.sha256 !== rs.tree) throw new Error(`the private copy of LectureFlow ${a.from_version} changed since it was checked`);
      await this._replaceDir(target, source);
      this.log.info(`selfupdate: LectureFlow ${a.from_version} put back into ${target}`);
      // The folder was made anew and took its parent's permissions
      // (security review SEC20 中-1, a known limitation): the installation
      // itself (target, not this program's own folder) is looked at once
      // more; what is wrong goes with the rollback's reason (the
      // diagnostics warn and in-app updates stay off). It never fails the
      // rollback.
      const why = await this._installDirAfterRestore(target);
      if (why) {
        this.log.error(`selfupdate: after putting back ${a.from_version}: ${why}`);
        a.error = F.shortError(`${a.error || `LectureFlow ${a.version} did not start`}; after going back: ${why}`);
      }
    } catch (err) {
      a.restore_error = F.shortError(err);
      this.log.error(`selfupdate: putting back ${a.from_version}: ${a.restore_error}`);
    }
    // The version started next must not wait for this program.
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a).catch(() => {});
    this.ops.releaseLock();
    await this._spawnQuiet(path.join(target, exe), [UPDATED_ARG]);
    return true;
  }

  // ---- pending.json (old version) ----

  async _handlePending({ background }) {
    const read = await F.readJson(this.file('pending'));
    if (!read.exists) return CONTINUE;
    const check = await this._checkPending(read);
    if (check.leave) {
      // Not this copy's to install, and not its to remove or to report on
      // (r6; security review SEC29 第六节 #6): the copy it belongs to
      // finds pending.json, its package and previous.json as they were.
      this.log.info(`selfupdate: ${check.leave}`);
      return CONTINUE;
    }
    if (check.drop) {
      this.log.info(`selfupdate: removing pending.json: ${check.drop}`);
      await F.removeFile(this.file('pending'));
      if (check.report) await this._writeResult({ version: check.version, from_version: this.version }, 'failed', check.drop);
      return CONTINUE;
    }
    if (background) {
      this.log.info(`selfupdate: LectureFlow ${check.pending.version} waits for a start with a window`);
      return CONTINUE;
    }
    const verdict = await this._verifyWithService(check.pending);
    if (!verdict.install) {
      await this._removeCopy(verdict.copy);
      const p = check.pending;
      if (verdict.drop) {
        this.log.warn(`selfupdate: not installing ${p.version}: ${verdict.drop}; removing pending.json`);
        await F.removeFile(this.file('pending'));
        await this._writeResult({ version: p.version, from_version: this.version }, 'failed', verdict.drop);
      } else {
        // No verdict: nothing is installed, pending.json stays (see
        // _verifyWithService); the result says why this start did not install it.
        this.log.warn(`selfupdate: not installing ${p.version} now: ${verdict.keep}; pending.json is kept`);
        await this._writeResult({ version: p.version, from_version: this.version }, 'failed', `${verdict.keep}; not installed at this start`);
      }
      return CONTINUE;
    }
    this.log.info(`selfupdate: the service program verified ${check.pending.version} with its built-in key`);
    return this._install(check.pending, verdict.copy);
  }

  /**
   * _verifyWithService(p): the installed service program checks the pending
   * update again with the public key built into it (lib/verifyupdate.js,
   * `-selfupdate-verify`) and copies the package into a private folder,
   * right before anything is backed up or started.
   * → { install: true, copy } | { drop: reason } | { keep: reason }.
   *
   * Only exit 0 naming exactly p, with its copy where it was asked for,
   * installs. Exit 1 ("invalid"): pending.json is removed like any other bad
   * update (the package directory is left to the service, which removes
   * unused version directories at its start). Exit 2 ("not_configured", a
   * build without a public key), exit 3 (other accounts can change the
   * data folder), a timeout, a program that cannot be run or an answer
   * outside the contract: not installed (nothing can vouch for the
   * package), but pending.json is kept. Such a build never writes
   * pending.json itself (Go writes it only after verifying with its key;
   * development builds have no installation and never get here), so a
   * pending update it finds belongs to another build sharing the data
   * directory, which can still verify and install it; once the data
   * folder's permissions are tightened, the update is installed; a timeout
   * or a program that does not start is tried again next time.
   */
  async _verifyWithService(p) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') {
      return { keep: 'this copy of LectureFlow has no update check program' };
    }
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, this._verifyOpts({ copyTo: this.tmpDir }));
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    return interpretVerify(run, p, { copyTo: this.tmpDir });
  }

  /**
   * _checkPending → { pending, file }, { drop: reason, report, version }
   * or { leave: what the log says }.
   *
   * leave: a readable pending.json that another copy of LectureFlow
   * sharing this data folder is to install (the installed and the portable
   * copy both use %APPDATA%\LectureFlow by default; pending.json has one
   * slot and does not say whose it is). It is neither removed nor reported
   * as failed, whatever its version (the other copy may be older than
   * this one); the service of this copy does not take it for its own and
   * starts no update over it (internal/selfupdate, loadPending):
   *   - this copy cannot update itself: it never installs any, so the copy
   *     that can finds it as it was (it used to be removed with the result
   *     "install the new version by hand");
   *   - it is the other Windows copy's (otherCopysPending).
   */
  async _checkPending(read) {
    const p = read.value;
    const version = p && F.parseVersion(p.version) ? p.version : null;
    const drop = (why, report = true) => ({ drop: why, report: report && !!version, version });
    if (read.error || !p) return drop(read.error || 'pending.json is unreadable');
    if (p.schema !== F.SCHEMA) return drop(`pending.json has schema ${JSON.stringify(p.schema)}, not ${F.SCHEMA}`);
    if (!version) return drop('pending.json has no valid version');
    if (otherCopysPending(this.install, p)) {
      if (this.install.unsupported) {
        return { leave: `pending.json is left for the copy that can install it (LectureFlow ${version}; this copy cannot update itself)` };
      }
      return { leave: `pending.json is another copy's (LectureFlow ${version}, a "${p.kind}" package; this installation takes "${this.install.kind}"); left alone` };
    }
    if (F.compareVersions(version, this.version) <= 0) return drop(`${version} is not newer than ${this.version}`, false);
    if (p.platform !== this.install.platform) return drop(`the package is for ${p.platform}, not ${this.install.platform}`);
    if (!this.install.kinds.includes(p.kind)) return drop(`this installation needs a "${this.install.kind}" package, not "${p.kind}"`);
    if (!Number.isSafeInteger(p.size) || p.size <= 0 || typeof p.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(p.sha256)) {
      return drop('pending.json has no valid size or SHA-256');
    }
    const file = F.resolveInside(this.dir, p.file);
    if (!file) return drop('pending.json names a file outside the updates folder');
    const bad = await F.checkFile(file, p.size, p.sha256);
    if (bad) return drop(bad);
    return { pending: p, file };
  }

  /** _install(p, copy): copy is the private copy of the package the service program made. */
  async _install(p, copy) {
    const v = this.version;
    const { kind, target } = this.install;
    const failEarly = async (why) => {
      await this._removeCopy(copy);
      await F.removeFile(this.file('pending'));
      await this._writeResult({ version: p.version, from_version: v }, 'failed', why);
      this.log.warn(`selfupdate: not installing ${p.version}: ${why}`);
      return CONTINUE;
    };
    if (!copy) return failEarly('the update check made no private copy of the package');
    if (!this._trustFile()) return failEarly('this copy of LectureFlow has no profile folder to record the installation in');
    const prev = await this._readPrevious();
    if (!prev || !F.sameVersion(prev.version, v) || prev.platform !== this.install.platform) {
      return failEarly('previous.json does not describe the running version');
    }
    this.log.info(`selfupdate: installing LectureFlow ${p.version} (${kind}) over ${v}`);

    // 1. What a failed update returns to, and the database before it.
    let dbRel = null;
    try {
      // (a backup of this version that is not a copy of this installation
      // goes first, whatever else there is to return to)
      const backup = await this._backupOfInstallation(prev);
      if ((kind === F.KINDS.installer || kind === F.KINDS.portable) && !(await this._previousFileUsable(prev)) && !backup) {
        prev.backup = await this._backupInstallation();
        await F.writeJsonAtomic(this.file('previous'), prev);
      }
      dbRel = await this._snapshotDb();
    } catch (err) {
      return failEarly(`could not back up LectureFlow ${v}: ${F.shortError(err)}`);
    }

    // 2. The trust file, then pending.json → applying.json (a crash from
    // here on is noticed).
    const startedAt = this.now().toISOString();
    try {
      await this._updateTrust((t) => {
        t.install = { version: p.version, from_version: v, target, kind, started_at: startedAt };
      });
    } catch (err) {
      return failEarly(`could not record the installation in ${path.dirname(this._trustFile())}: ${F.shortError(err)}`);
    }
    await F.renameRetry(this.file('pending'), this.file('applying'));
    const a = {
      ...p,
      phase: 'installing',
      tries: 0,
      target,
      exe: this.install.exe,
      started_at: startedAt,
    };
    if (dbRel) a.db_snapshot = dbRel;
    await F.writeJsonAtomic(this.file('applying'), a);

    // 3. Install (from the private copy).
    try {
      const pid = await this._installPackage(a, copy, prev);
      if (pid) {
        a.pid = pid;
        a.pid_at = this.now().toISOString();
        await F.writeJsonAtomic(this.file('applying'), a);
      }
      return { action: 'exit', version: a.version };
    } catch (err) {
      await this._removeCopy(copy);
      const why = `could not install ${a.version}: ${F.shortError(err)}`;
      this.log.warn(`selfupdate: ${why}`);
      await this._finish(a, 'failed', why);
      return CONTINUE;
    }
  }

  async _installPackage(a, file, prev) {
    const { kind, target } = this.install;
    if (kind === F.KINDS.installer) {
      // The installer closes this app (it is in the directory being
      // replaced), installs for this user only and starts the new version.
      // It runs from the private copy, which a later start removes.
      return this.ops.spawnDetached(file, F.nsisArguments(target), { verbatim: true });
    }
    if (kind === F.KINDS.portable) {
      await this._replaceFile(target, file);
      await this._removeCopy(file);
      this.ops.releaseLock();
      return this._spawnQuiet(target, [UPDATED_ARG]);
    }
    // macOS: the old .app becomes the backup of the previous version.
    const backupDir = this.backupDir(this.version);
    const keepOld = path.join(backupDir, 'app', path.basename(target));
    // The hash of the app where it is installed: what is kept as the
    // backup must still have it once it lies in the data folder.
    let source = null;
    try {
      source = await F.treeHash(target);
    } catch (err) {
      this.log.warn(`selfupdate: hashing ${path.basename(target)} before it is replaced: ${F.shortError(err)}; it will not serve as a backup`);
    }
    let kept;
    try {
      kept = await this._installMacPackage(file, a.kind, a.version, keepOld);
    } finally {
      await this._removeCopy(file);
    }
    if (kept) {
      const item = path.basename(target);
      await F.writeJsonAtomic(path.join(backupDir, F.BACKUP_MARK), {
        schema: F.SCHEMA, version: this.version, kind: this.install.kind, item, created_at: this.now().toISOString(),
      });
      // (without a record in the trust file the backup is never used)
      if (source) await this._recordBackup(backupDir, item, source).catch((err) => this.log.warn(`selfupdate: recording the backup: ${F.shortError(err)}`));
      prev.backup = F.relativeTo(this.dir, backupDir);
      await F.writeJsonAtomic(this.file('previous'), prev);
    }
    this.ops.releaseLock();
    return this._spawnQuiet('/usr/bin/open', ['-n', target, '--args', UPDATED_ARG]);
  }

  /**
   * _backupInstallation: copy what is replaced into backup/<version>/app.
   * The installation is hashed where it is installed; the copy must have
   * that hash once it is written, or there is no backup (and no update).
   */
  async _backupInstallation() {
    const dir = this.backupDir(this.version);
    const { kind, target } = this.install;
    const source = await F.treeHash(target);
    await fsp.rm(dir, { recursive: true, force: true });
    await fsp.mkdir(path.join(dir, 'app'), { recursive: true });
    try {
      let item;
      if (kind === F.KINDS.installer) {
        await fsp.cp(target, path.join(dir, 'app'), { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
        item = '.';
      } else {
        item = path.basename(target);
        await fsp.copyFile(target, path.join(dir, 'app', item));
      }
      await F.writeJsonAtomic(path.join(dir, F.BACKUP_MARK), {
        schema: F.SCHEMA, version: this.version, kind, item, created_at: this.now().toISOString(),
      });
      await this._recordBackup(dir, item, source);
    } catch (err) {
      await fsp.rm(dir, { recursive: true, force: true }).catch(() => {});
      throw err;
    }
    this.log.info(`selfupdate: LectureFlow ${this.version} backed up to ${F.relativeTo(this.dir, dir)}`);
    return F.relativeTo(this.dir, dir);
  }

  /**
   * _recordBackup(dir, item, source): the tree hash of the installation
   * (source: F.treeHash of what was copied or moved) into the trust file,
   * after backup/<version>/app/<item> was hashed again where it lies now:
   * a copy that differs from its source is an error and is not recorded.
   */
  async _recordBackup(dir, item, source) {
    const h = await F.treeHash(path.join(dir, 'app', item === '.' ? '' : item));
    if (!source || h.sha256 !== source.sha256) {
      throw new Error(`the backup of LectureFlow ${this.version} differs from the installation it was made from`);
    }
    await this._updateTrust((t) => {
      t.backups = t.backups || {};
      t.backups[this.version] = {
        dir: F.relativeTo(this.dir, dir), kind: this.install.kind, item, tree: h.sha256, files: h.files, bytes: h.bytes, created_at: this.now().toISOString(),
      };
    });
  }

  /** _snapshotDb: copy the database files into backup/<version>/db; null when there is none. */
  async _snapshotDb() {
    const dir = path.join(this.backupDir(this.version), 'db');
    await fsp.rm(dir, { recursive: true, force: true });
    await fsp.mkdir(dir, { recursive: true });
    let any = false;
    for (const name of F.DB_FILES) {
      try {
        await fsp.copyFile(path.join(this.dataDir, name), path.join(dir, name));
        any = true;
      } catch (err) {
        if (!err || err.code !== 'ENOENT') throw err;
      }
    }
    if (!any) {
      await fsp.rm(dir, { recursive: true, force: true });
      return null;
    }
    return F.relativeTo(this.dir, dir);
  }

  // ---- replacing files ----

  /**
   * _replaceFile(target, source): put a copy of source at target. The
   * running portable executable cannot be overwritten or deleted, but it can
   * be renamed: target → target.lf-old (removed at a later start).
   */
  async _replaceFile(target, source) {
    const incoming = `${target}.lf-new`;
    await fsp.rm(incoming, { force: true });
    await fsp.copyFile(source, incoming).catch((err) => {
      throw notWritable(err, target);
    });
    const aside = await this._freeName(`${target}.lf-old`);
    await F.renameRetry(target, aside);
    try {
      await F.renameRetry(incoming, target);
    } catch (err) {
      await F.renameRetry(aside, target).catch(() => {});
      await fsp.rm(incoming, { force: true }).catch(() => {});
      throw err;
    }
  }

  /**
   * _replaceDir(target, source): Windows installation directory from a
   * backup. The failed one is moved aside first (fails cleanly while one of
   * its programs still runs), the copy goes in, the failed one is removed.
   */
  async _replaceDir(target, source) {
    const aside = `${target}.lf-failed-${stamp(this.now())}`;
    await F.renameRetry(target, aside, { tries: 60, delayMs: 500 });
    try {
      await fsp.cp(source, target, { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
    } catch (err) {
      await fsp.rm(target, { recursive: true, force: true }).catch(() => {});
      await F.renameRetry(aside, target).catch(() => {});
      throw err;
    }
    await fsp.rm(aside, { recursive: true, force: true }).catch(() => {});
  }

  /**
   * _installDirAfterRestore(target) → null when the service program vouches
   * for the installation folder target after a rollback put it back (and
   * for the data and trust folders: -selfupdate-trust-check with
   * -selfupdate-install-dir target), else why not (short English). Never
   * throws. It waits restoreCheckTimeoutMs at most (the program is told the
   * same limit); no answer by then counts as "could not be confirmed".
   */
  async _installDirAfterRestore(target) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') return 'there is no update check program to look at the installation folder';
    const timeoutMs = this.restoreCheckTimeoutMs;
    let run;
    let timer = null;
    try {
      run = await Promise.race([
        this.ops.verifyPending(this.dir, this.version, { ...this._verifyOpts({ trustCheck: true }), installDir: target, timeoutMs }),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve({ timedOut: true }), timeoutMs);
        }),
      ]);
    } catch (err) {
      run = { error: F.shortError(err) };
    } finally {
      clearTimeout(timer);
    }
    const v = interpretTrustCheck(run, { timeoutMs });
    return v.ok ? null : v.error;
  }

  async _freeName(base) {
    try {
      await fsp.rm(base, { force: true });
      return base;
    } catch {
      return `${base}-${stamp(this.now())}`;
    }
  }

  /**
   * _installMacPackage(file, kind, version, keepOld): unpack the .app of a
   * zip (or copy it out of a dmg) next to the current one, check it, swap.
   * Returns true when the old .app was moved to keepOld.
   */
  async _installMacPackage(file, kind, version, keepOld) {
    const { target } = this.install;
    if (/\/AppTranslocation\//.test(target)) {
      throw new Error('LectureFlow runs from a read-only (translocated) location; move LectureFlow.app to the Applications folder');
    }
    const staging = path.join(path.dirname(target), `.${path.basename(target)}.lf-new-${version}`);
    await fsp.rm(staging, { recursive: true, force: true });
    await fsp.mkdir(staging).catch((err) => {
      throw notWritable(err, target); // e.g. started from the dmg
    });
    try {
      const out = path.join(staging, 'out');
      await fsp.mkdir(out);
      if (kind === F.KINDS.dmg) {
        const mnt = path.join(staging, 'mnt');
        await fsp.mkdir(mnt);
        await this.ops.run('/usr/bin/hdiutil', ['attach', '-nobrowse', '-readonly', '-noautoopen', '-mountpoint', mnt, file]);
        try {
          const mounted = await findApp(mnt);
          await this.ops.run('/usr/bin/ditto', [mounted, path.join(out, path.basename(mounted))]);
        } finally {
          await this.ops.run('/usr/bin/hdiutil', ['detach', mnt, '-force']).catch(() => {});
        }
      } else {
        await this.ops.run('/usr/bin/ditto', ['-x', '-k', file, out]);
      }
      const app = await findApp(out);
      await this._checkMacApp(app, version);
      await this.ops.run('/usr/bin/xattr', ['-dr', 'com.apple.quarantine', app]).catch(() => {});
      await this.ops.run('/usr/bin/codesign', ['--verify', '--deep', '--strict', app]);
      return await this._swapApp(app, keepOld);
    } finally {
      await fsp.rm(staging, { recursive: true, force: true }).catch(() => {});
    }
  }

  async _checkMacApp(app, version) {
    const plist = path.join(app, 'Contents', 'Info.plist');
    const read = async (key) => {
      const { stdout } = await this.ops.run('/usr/bin/plutil', ['-extract', key, 'raw', '-o', '-', plist]);
      return String(stdout).trim();
    };
    const id = await read('CFBundleIdentifier');
    if (id !== F.MAC_BUNDLE_ID) throw new Error(`the package holds ${id || 'an unknown app'}, not LectureFlow`);
    const shown = await read('CFBundleShortVersionString').catch(() => '');
    if (shown && shown !== version) this.log.warn(`selfupdate: the new app says version ${shown}, update.json ${version}`);
  }

  /**
   * _swapApp(incoming, keepOld): incoming .app takes the place of the
   * current one; the current one goes to keepOld (or is removed).
   */
  async _swapApp(incoming, keepOld) {
    const { target } = this.install;
    const aside = path.join(path.dirname(target), `.${path.basename(target)}.lf-old-${stamp(this.now())}`);
    await F.renameRetry(target, aside);
    try {
      await this._moveDir(incoming, target);
    } catch (err) {
      await F.renameRetry(aside, target).catch(() => {});
      throw err;
    }
    if (!keepOld) {
      await fsp.rm(aside, { recursive: true, force: true }).catch(() => {});
      return false;
    }
    try {
      await fsp.rm(keepOld, { recursive: true, force: true });
      await fsp.mkdir(path.dirname(keepOld), { recursive: true });
      await this._moveDir(aside, keepOld);
      return true;
    } catch (err) {
      this.log.warn(`selfupdate: keeping the previous app as a backup: ${F.shortError(err)}`);
      return false;
    }
  }

  async _moveDir(from, to) {
    try {
      await fsp.rename(from, to);
    } catch (err) {
      if (!err || err.code !== 'EXDEV') throw err;
      await this.ops.run('/usr/bin/ditto', [from, to]);
      await fsp.rm(from, { recursive: true, force: true }).catch(() => {});
    }
  }

  async _spawnQuiet(file, args) {
    try {
      return await this.ops.spawnDetached(file, args, {});
    } catch (err) {
      this.log.warn(`selfupdate: starting ${path.basename(file)}: ${F.shortError(err)}`);
      return null;
    }
  }

  // ---- the private copies ----

  /** _isPrivateDir(dir): dir is a private folder of the service program's making, <tmpDir>/lectureflow-update-<n>. */
  _isPrivateDir(dir) {
    return typeof dir === 'string' && !!dir && path.isAbsolute(dir) && samePath(path.dirname(dir), this.tmpDir) && COPY_DIR_RE.test(path.basename(dir));
  }

  /** _removePrivate(dir): remove a private folder (only one the service program made in tmpDir). */
  async _removePrivate(dir) {
    if (!this._isPrivateDir(dir)) return;
    await fsp.rm(dir, { recursive: true, force: true }).catch((err) => this.log.warn(`selfupdate: removing ${dir}: ${F.shortError(err)}`));
  }

  /** _removeCopy(copy): remove a private copy of a package with its folder. */
  async _removeCopy(copy) {
    if (copy) await this._removePrivate(path.dirname(copy));
  }

  /** _dropSource(src): remove the private folder of what a rollback was going to use (_restoreSource). */
  async _dropSource(src) {
    if (!src) return;
    if (src.kind === 'backup') await this._removePrivate(src.private);
    else await this._removeCopy(src.copy);
  }

  /**
   * _cleanupCopies: private copies an earlier start left (an installer runs
   * from its copy while this app quits). Only folders of the copies' name
   * that are older than an installer may take; one still in use cannot be
   * removed and stays for later.
   */
  async _cleanupCopies() {
    let names;
    try {
      names = await fsp.readdir(this.tmpDir);
    } catch {
      return;
    }
    const now = this.now().getTime();
    for (const name of names) {
      if (!COPY_DIR_RE.test(name)) continue;
      const p = path.join(this.tmpDir, name);
      try {
        const st = await fsp.lstat(p);
        if (!st.isDirectory() || now - st.mtimeMs < HELPER_WAIT_MS) continue;
        await fsp.rm(p, { recursive: true, force: true });
      } catch {
        // in use or gone: a later start tries again
      }
    }
  }

  // ---- bookkeeping ----

  async _readPrevious() {
    const read = await F.readJson(this.file('previous'));
    const p = read.value;
    if (!p || p.schema !== F.SCHEMA || !F.parseVersion(p.version)) return null;
    return p;
  }

  _trustFile() {
    if (this.trustDir) return path.join(this.trustDir, F.TRUST_DIR_FILE);
    return this.privateDir ? path.join(this.privateDir, F.TRUST_FILE) : null;
  }

  /**
   * _verifyOpts(o): o for ops.verifyPending, with the trust folder and the
   * installation directory (F.installDirOf: the Windows installation set
   * up by the installer; security audit M-4) the service program must
   * vouch for.
   */
  _verifyOpts(o) {
    const out = this.trustDir ? { ...o, trustDir: this.trustDir } : o;
    const installDir = F.installDirOf(this.install);
    return installDir ? { ...out, installDir } : out;
  }

  /** _legacyTrustFile → the record an older version kept in the profile (only with a trust folder), or null. */
  _legacyTrustFile() {
    return this.trustDir && this.privateDir ? path.join(this.privateDir, F.TRUST_FILE) : null;
  }

  /**
   * _removeLegacyTrust: the profile's old record goes once nothing needs it
   * (only when the profile is a plain folder: never through a link).
   */
  async _removeLegacyTrust() {
    const f = this._legacyTrustFile();
    if (!f || !(await plainDir(this.privateDir))) return;
    await F.removeFile(f).catch((err) => this.log.warn(`selfupdate: removing ${f}: ${F.shortError(err)}`));
  }

  /**
   * _migrateTrust (security review SEC17 中-B): the first start with a
   * trust folder looks once at the record an older version kept in the
   * profile — which may lie in the data folder, where another account that
   * once could write there may have prepared it — and takes over only what
   * cannot run anything of that account's:
   *   - only once: migrated.json in the trust folder (never a mark in the
   *     profile, which such an account could remove) is written before
   *     anything is taken over; with it the profile is never read again;
   *   - only the installation (install) this version is on probation for:
   *     applying.json "installing" of the running version, not started
   *     yet (tries 0 or missing), with the same
   *     versions and target as the old record, the target being this
   *     installation; the trust folder holds no installation yet; and the
   *     service program vouches for the data folder and the trust folder
   *     (-selfupdate-trust-check);
   *   - the backup of the version it came from (backups[from_version]),
   *     with it, when the profile does not lie in the data folder (then
   *     only this account could ever write the old record) or when the
   *     data folder is the default one, %APPDATA%\LectureFlow of this
   *     account, with the profile in it and no link on the way
   *     (_defaultDataInAppData; security review SEC17 中-H, option b,
   *     approved by the user 2026-11-11: most users keep the default, and
   *     without it the first update after the move could not go back).
   * Taken over, the backup's record is used like one this shell made: a
   * rollback asks the service program about the previous version first,
   * copies the backup into a folder the service program made, and uses the
   * copy only when it has the recorded hash (_restoreSource).
   * Outside the default data folder the worst a prepared old record can do
   * is therefore a rollback to a previous version's package the service
   * program verified with the built-in key (an older signed version). In
   * the default data folder an account that once could write there, and
   * removed every trace of itself before this start (so that the service
   * program vouches for the folder), can have its own program put in place
   * of the installation and started if the new version then fails to
   * start twice (an applying.json that says it was started already is not
   * taken over): the hashes checked are its own record's. An accepted
   * residual risk of this one move (internal/selfupdate/README.md). The old file stays while the installation it records is on
   * probation (a rollback to that older version needs it: its shell reads
   * only the profile) and goes otherwise (_finish removes it later).
   */
  async _migrateTrust() {
    if (!this.trustDir || !this.privateDir) return;
    const mark = path.join(this.trustDir, F.TRUST_MIGRATED_FILE);
    if (await F.exists(mark)) return;
    const legacy = (await F.readJson(this._legacyTrustFile())).value;
    const take = legacy && legacy.schema === F.SCHEMA && legacy.install && typeof legacy.install === 'object'
      ? await this._legacyToTake(legacy)
      : null;
    try {
      await F.writeJsonAtomic(mark, { schema: F.SCHEMA, at: this.now().toISOString(), install: !!take, backup: !!(take && take.backup) });
    } catch (err) {
      this.log.warn(`selfupdate: the update's trust folder cannot be written: ${F.shortError(err)}`);
      return;
    }
    if (take) {
      try {
        await this._updateTrust((t) => {
          t.install = take.install;
          if (take.backup) t.backups[take.install.from_version] = take.backup;
        });
      } catch (err) {
        this.log.warn(`selfupdate: updating the trust file: ${F.shortError(err)}`);
        return;
      }
      this.log.info(`selfupdate: the record of the update to ${take.install.version} was taken over from the profile${take.backup ? ' with its backup' : ' (without its backup)'}`);
    } else {
      await this._removeLegacyTrust();
    }
  }

  /** _legacyToTake(legacy) → { install, backup? } to take over, or null (see _migrateTrust). */
  async _legacyToTake(legacy) {
    const no = (why) => {
      this.log.warn(`selfupdate: the update record in the profile is not taken over: ${why}`);
      return null;
    };
    if (!this.install || this.install.unsupported) return no('this copy cannot be updated in place');
    const read = await F.readJson(this.file('applying'));
    const a = read.value;
    if (read.error || !validApplying(a) || (a.phase !== undefined && a.phase !== 'installing')) return no('no installation is on probation');
    // Before the new version's first start tries is 0 (_install writes it;
    // this runs before _beginProbation counts a start, and only once) or
    // missing; anything else was prepared to roll back at once (security
    // review SEC21 低-A).
    if (a.tries !== undefined && a.tries !== 0) return no('the installation on probation has been started already');
    const rec = legacy.install;
    if (!F.sameVersion(a.version, this.version)) return no(`the installation on probation is of ${a.version}, not ${this.version}`);
    if (typeof rec.target !== 'string' || !F.sameVersion(rec.version, a.version) || !F.sameVersion(rec.from_version, a.from_version)
      || !samePath(rec.target, a.target) || !samePath(a.target, this.install.target)) {
      return no('it does not match applying.json and this installation');
    }
    if ((await this._readTrust()).install) return no('the trust folder records an installation already');
    if (!this.ops || typeof this.ops.verifyPending !== 'function') return no('this copy of LectureFlow has no update check program');
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, this._verifyOpts({ trustCheck: true }));
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    const checked = interpretTrustCheck(run);
    if (!checked.ok) return no(checked.error);
    const install = { version: rec.version, from_version: rec.from_version, target: rec.target, kind: rec.kind };
    if (typeof rec.started_at === 'string') install.started_at = rec.started_at;
    let backup = null;
    const b = legacy.backups && typeof legacy.backups === 'object' ? legacy.backups[rec.from_version] : null;
    if (b && typeof b === 'object' && !Array.isArray(b)) {
      // (the service program has vouched for the data folder and the trust
      // folder just now: checked.ok above)
      if (await this._profileOutsideData() || await this._defaultDataInAppData()) {
        backup = {};
        for (const k of ['dir', 'kind', 'item', 'tree', 'files', 'bytes', 'created_at']) if (b[k] !== undefined) backup[k] = b[k];
      } else {
        this.log.warn(`selfupdate: the record of the backup of ${rec.from_version} is not taken over: the profile lies in a data folder that is not the default one in %APPDATA% (or there is a link on its way)`);
      }
    }
    return { install, backup };
  }

  /** _profileOutsideData → the profile and the data folder do not overlap (also with links resolved; unknown: false). */
  async _profileOutsideData() {
    const overlap = (a, b) => {
      const rel = path.relative(b, a);
      return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel));
    };
    const pairs = [[path.resolve(this.privateDir), path.resolve(this.dataDir)]];
    try {
      pairs.push([await fsp.realpath(this.privateDir), await fsp.realpath(this.dataDir)]);
    } catch {
      return false;
    }
    for (let [p, d] of pairs) {
      if (process.platform === 'win32' || process.platform === 'darwin') {
        p = p.toLowerCase();
        d = d.toLowerCase();
      }
      if (overlap(p, d) || overlap(d, p)) return false;
    }
    return true;
  }

  /**
   * _defaultDataInAppData → the data folder is the default one,
   * %APPDATA%\LectureFlow of this account (appDataDir: the APPDATA the
   * system sets for it; unknown or not an absolute path with a drive
   * letter: false), the profile lies in it, and there is no link on the
   * way: every folder from the root of the drive down to the profile is a
   * folder itself (lstat: Node reports a junction as a link), and the data
   * folder and the profile are their own real paths (no link, short 8.3
   * name or substituted drive). Case and a separator at the end do not
   * matter (samePath). Anything unknown: false.
   */
  async _defaultDataInAppData() {
    const root = typeof this.appDataDir === 'string' ? this.appDataDir.trim() : '';
    if (!root || root.includes('\0') || !(process.platform === 'win32' ? /^[A-Za-z]:[\\/]/.test(root) : path.isAbsolute(root))) return false;
    const data = path.resolve(this.dataDir);
    const profile = path.resolve(this.privateDir);
    if (!samePath(data, path.join(path.resolve(root), APP_DIR_NAME))) return false;
    const rel = path.relative(data, profile);
    if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) return false;
    for (let p = profile; ; p = path.dirname(p)) { // (ends at the root of the drive)
      if (!(await plainDir(p))) return false;
      if (path.dirname(p) === p) break;
    }
    try {
      return samePath(await fsp.realpath(data), data) && samePath(await fsp.realpath(profile), profile);
    } catch {
      return false;
    }
  }

  /**
   * _dropStaleInstall: without applying.json no installation is on
   * probation; a record of one (a crash between recording it and renaming
   * pending.json, or applying.json removed by "Restrict access") goes, so
   * that no applying.json put there later can match it. Only a record of
   * this installation (its target): the trust folder is shared by every
   * copy of this account, and another copy's installation (the installed
   * one, while a portable copy or one with another data folder starts)
   * may be on probation with applying.json in its own data folder; an
   * applying.json here never matches that record anyway
   * (_foreignApplying: the target must be this installation). Such a
   * record stays until that copy starts again, or until any copy's next
   * installation replaces it (_install); when that copy is gone (a
   * portable .exe deleted, moved or renamed) it may stay for good. It is
   * inert: only a copy of that target uses it, with a matching
   * applying.json (security review SEC29 低-3).
   */
  async _dropStaleInstall() {
    const f = this._trustFile();
    if (!f || !(await F.exists(f))) return;
    const rec = (await this._readTrust()).install;
    if (!rec || !samePath(rec.target, this.install.target)) return;
    await this._updateTrust((t) => {
      if (t.install && samePath(t.install.target, this.install.target)) delete t.install;
    }).catch((err) => this.log.warn(`selfupdate: updating the trust file: ${F.shortError(err)}`));
    this.log.info('selfupdate: dropped the record of an installation that is not on probation');
  }

  /** _readTrust → the trust file ({ schema, install?, backups }); empty when missing or unreadable. */
  async _readTrust() {
    const empty = { schema: F.SCHEMA, backups: {} };
    const f = this._trustFile();
    if (!f) return empty;
    const read = await F.readJson(f);
    const t = read.value;
    if (!t || t.schema !== F.SCHEMA) return empty;
    if (!t.backups || typeof t.backups !== 'object' || Array.isArray(t.backups)) t.backups = {};
    if (t.install && (typeof t.install !== 'object' || typeof t.install.target !== 'string')) delete t.install;
    return t;
  }

  async _updateTrust(change) {
    const f = this._trustFile();
    if (!f) throw new Error('no profile folder');
    const t = await this._readTrust();
    change(t);
    await F.writeJsonAtomic(f, t);
  }

  async _writeResult(a, outcome, error) {
    const r = { version: a.version, from: a.from_version, outcome, at: this.now().toISOString() };
    if (error) r.error = F.shortError(error);
    await F.writeJsonAtomic(this.file('result'), r);
  }

  /** _finish: result.json, applying.json and its record removed, database snapshot and stale backups removed. */
  async _finish(a, outcome, error) {
    await this._writeResult(a, outcome, outcome === 'installed' ? null : error);
    if (outcome !== 'installed') this.log.warn(`selfupdate: ${outcome}: ${a.from_version} → ${a.version}: ${F.shortError(error)}`);
    await F.removeFile(this.file('applying'));
    const shared = this._looksAfterShared(); // (a copy that cannot update itself: the data folder only)
    if (shared) {
      await this._updateTrust((t) => {
        if (t.install && F.sameVersion(t.install.version, a.version) && F.sameVersion(t.install.from_version, a.from_version)) delete t.install;
      }).catch((err) => this.log.warn(`selfupdate: updating the trust file: ${F.shortError(err)}`));
    }
    await this._pruneBackups().catch((err) => this.log.warn(`selfupdate: removing old backups: ${F.shortError(err)}`));
    // the profile's old record (kept for a rollback to the version before
    // the trust folder) is not needed any more
    if (shared) await this._removeLegacyTrust();
  }

  /**
   * _pruneBackups: keep only the backup previous.json names, without its
   * database snapshot (which only serves a rollback during the first start);
   * the trust file forgets the others.
   */
  async _pruneBackups() {
    const root = path.join(this.dir, F.BACKUP_DIR);
    let names = [];
    // security review SEC17: updates/ or backup/ that is a link (a
    // junction or a symbolic link, left by whoever could once write to the
    // data folder) is not followed: what lies where it leads is not ours
    // to remove. An entry below backup/ that is a link is removed itself
    // (fs.rm does not follow links), never entered.
    if (await plainDir(this.dir) && await plainDir(root)) {
      try {
        names = await fsp.readdir(root);
      } catch {
        names = [];
      }
    } else if (await F.exists(root)) {
      this.log.warn(`selfupdate: ${root} is reached through a link; no backup is removed through it`);
    }
    const prev = await this._readPrevious();
    const keep = prev && prev.backup ? F.resolveInside(this.dir, prev.backup) : null;
    for (const name of names) {
      const p = path.join(root, name);
      if (keep && samePath(p, keep)) {
        if (await plainDir(p)) await fsp.rm(path.join(p, 'db'), { recursive: true, force: true });
      } else {
        await fsp.rm(p, { recursive: true, force: true });
      }
    }
    if (!this._looksAfterShared() || !this._trustFile()) return;
    const keepRel = keep ? F.relativeTo(this.dir, keep) : null;
    await this._updateTrust((t) => {
      for (const v of Object.keys(t.backups)) {
        if (!t.backups[v] || t.backups[v].dir !== keepRel) delete t.backups[v];
      }
    });
  }

  /** _cleanupLeftovers: files an earlier installation could not remove yet. */
  async _cleanupLeftovers() {
    const { kind, target } = this.install;
    if (!target) return;
    const dir = path.dirname(target);
    const base = path.basename(target);
    let names;
    try {
      names = await fsp.readdir(dir);
    } catch {
      return;
    }
    const prefixes = kind === F.KINDS.portable ? [`${base}.lf-old`, `${base}.lf-new`]
      : kind === F.KINDS.installer ? [`${base}.lf-failed-`]
        : [`.${base}.lf-old-`, `.${base}.lf-new-`];
    for (const name of names) {
      if (!prefixes.some((p) => name.startsWith(p))) continue;
      await fsp.rm(path.join(dir, name), { recursive: true, force: true }).catch(() => {});
    }
  }
}

/** notWritable: a readable reason when the folder of what is replaced cannot be written. */
function notWritable(err, target) {
  if (!err || !['EACCES', 'EPERM', 'EROFS'].includes(err.code)) return err;
  return new Error(`the folder of ${path.basename(target)} cannot be written (${err.code}); move LectureFlow to a folder you can write to, or install the new version by hand`);
}

async function findApp(dir) {
  const apps = (await fsp.readdir(dir)).filter((n) => /\.app$/i.test(n) && !n.startsWith('.'));
  if (apps.length !== 1) throw new Error(`the package holds ${apps.length} apps, not one`);
  return path.join(dir, apps[0]);
}

// ---- ops ----

/**
 * spawnDetached(file, args, { verbatim }) → pid. verbatim (Windows): the
 * arguments go on the command line exactly as given (NSIS wants "/D=" last
 * and unquoted); the program path is quoted here.
 */
function spawnDetached(file, args, { verbatim = false } = {}) {
  return new Promise((resolve, reject) => {
    const opts = { detached: true, stdio: 'ignore', windowsHide: false };
    if (verbatim && process.platform === 'win32') {
      opts.windowsVerbatimArguments = true;
      opts.argv0 = `"${file}"`;
    }
    let child;
    try {
      child = childProcess.spawn(file, args, opts);
    } catch (err) {
      reject(err);
      return;
    }
    child.once('error', reject);
    child.once('spawn', () => {
      child.unref();
      resolve(child.pid);
    });
  });
}

function runProgram(file, args) {
  return new Promise((resolve, reject) => {
    childProcess.execFile(file, args, { timeout: 10 * 60 * 1000, maxBuffer: 4 << 20, windowsHide: true }, (err, stdout, stderr) => {
      if (err) {
        const detail = String(stderr || '').trim().split(/\r?\n/).pop();
        reject(new Error(`${path.basename(file)} failed${detail ? `: ${detail}` : ''}`));
        return;
      }
      resolve({ stdout, stderr });
    });
  });
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return !!err && err.code === 'EPERM';
  }
}

/**
 * electronOps({ app, verifyPending }): the real effects. verifyPending(dir,
 * version, opts) → lib/verifyupdate.runVerify's result: the installed
 * service program's -selfupdate-verify of the updates directory dir. The
 * trust file lives in the trust folder (F.trustDirFor: Windows), else in
 * the shell's profile (userData).
 */
function electronOps({ app, verifyPending }) {
  return {
    spawnDetached,
    run: runProgram,
    verifyPending,
    releaseLock: () => {
      try {
        app.releaseSingleInstanceLock();
      } catch {
        // not holding it
      }
    },
    pidAlive,
    privateDir: () => app.getPath('userData'),
    // Windows: outside the data folder (SEC17 中-B); macOS keeps the record
    // in the profile (no Mac to test a move on: a known limitation)
    trustDir: () => F.trustDirFor({ platform: process.platform, env: process.env, home: os.homedir() }),
    tmpDir: () => os.tmpdir(),
    // this account's %APPDATA%: where the default data folder lies (_migrateTrust)
    appDataDir: () => F.roamingAppDataFor({ platform: process.platform, env: process.env }),
  };
}

/**
 * recordingOps(markFile, { verifyPending, privateDir, tmpDir }): end-to-end
 * tests of the unpackaged app (LECTUREFLOW_TEST_SELFUPDATE): programs are
 * not started; every effect is appended to markFile as one JSON line. The
 * update check is recorded and then really run (it only reads and makes
 * the private copy; in these tests the service program is the fake
 * service). The trust file goes to privateDir (default: "private" next to
 * markFile), the copies to tmpDir (default: "tmp" next to markFile).
 */
function recordingOps(markFile, { verifyPending, privateDir, tmpDir } = {}) {
  const fs = require('node:fs');
  const record = (entry) => fs.appendFileSync(markFile, `${JSON.stringify({ ...entry, at: Date.now() })}\n`);
  const tmp = tmpDir || path.join(path.dirname(markFile), 'tmp');
  return {
    spawnDetached: async (file, args, opts = {}) => {
      record({ op: 'spawn', file, args, verbatim: !!opts.verbatim });
      return null;
    },
    run: async (file, args) => {
      record({ op: 'run', file, args });
      return { stdout: '' };
    },
    verifyPending: async (dir, version, opts = {}) => {
      record({ op: 'verify', dir, version, previous: !!opts.previous, copyTo: opts.copyTo || '', private: !!opts.privateDir });
      if (typeof verifyPending !== 'function') return { code: null, stdout: '', stderr: '', timedOut: false, error: 'no update check program' };
      if (opts.copyTo) fs.mkdirSync(opts.copyTo, { recursive: true });
      return verifyPending(dir, version, opts);
    },
    releaseLock: () => record({ op: 'releaseLock' }),
    pidAlive: () => false,
    privateDir: () => privateDir || path.join(path.dirname(markFile), 'private'),
    tmpDir: () => tmp,
  };
}

/**
 * startFailureReason(err): a failed start of the service (server.js
 * StartError) in short English, for result.json.
 */
function startFailureReason(err) {
  if (!err || typeof err.kind !== 'string') return `the service did not start: ${F.shortError(err)}`;
  const reason = err.reason ? `: ${err.reason}` : '';
  switch (err.kind) {
    case 'spawn_failed':
      return `the service program could not be run${reason}`;
    case 'timeout':
      return 'the service did not answer within 30 seconds';
    case 'port_in_use':
      return 'the service found no port to listen on';
    case 'token_rejected':
      return 'the service rejected the access token';
    case 'bad_mode':
      return 'the service did not run in desktop mode';
    default:
      return `the service stopped while starting${err.code !== null && err.code !== undefined ? ` (exit code ${err.code})` : ''}${reason}`;
  }
}

/**
 * testInstallFrom(env, isPackaged) → { install, mark } for end-to-end tests
 * of the unpackaged app: LECTUREFLOW_TEST_SELFUPDATE is
 * {"install": {platform, kind, target, exe}, "mark": "<file>"}; programs are
 * then recorded in the mark file instead of started. Never in a packaged app.
 */
function testInstallFrom(env, isPackaged) {
  if (isPackaged || !env || !env.LECTUREFLOW_TEST_SELFUPDATE) return null;
  let v;
  try {
    v = JSON.parse(env.LECTUREFLOW_TEST_SELFUPDATE);
  } catch {
    return null;
  }
  const i = v && v.install;
  if (!i || typeof v.mark !== 'string' || !path.isAbsolute(v.mark)) return null;
  if (!Object.values(F.KINDS).includes(i.kind) || typeof i.target !== 'string' || !path.isAbsolute(i.target)) return null;
  const kinds = i.kind === F.KINDS.zip || i.kind === F.KINDS.dmg ? [F.KINDS.zip, F.KINDS.dmg] : [i.kind];
  return {
    install: { platform: String(i.platform || ''), kind: i.kind, kinds, target: i.target, exe: String(i.exe || 'LectureFlow.exe') },
    mark: v.mark,
  };
}

module.exports = {
  SelfUpdateApplier,
  electronOps,
  recordingOps,
  spawnDetached,
  startFailureReason,
  testInstallFrom,
  MAX_TRIES,
  MAX_REFUNDS,
  RESTORE_ARG,
  UPDATED_ARG,
  HELPER_WAIT_MS,
  DB_BEFORE_ROLLBACK_NAME,
  otherCopysPending,
};
