// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which application update runs: the in-app update of SPEC-ext
// (internal/selfupdate, minisign-signed update.json, installed at the next
// start by selfupdate.js) or the older electron-updater path of SPEC-v3 V8
// (updates.js). The choice belongs to the service: its built-in minisign
// public key decides. While that key is empty, GET /api/selfupdate reports
// state "not_configured" with reason "no_public_key" and the electron-updater
// path stays exactly as it was. Once a key is built in, electron-updater is
// never loaded.
// Since security audit FB1 L-8 the packaged app's loadUpdater (main.js)
// never loads electron-updater on that path either: there it only means "no
// automatic updates"; development runs keep the tests' stand-in.
//
// Decided once per run, after the service is ready (the shell's poll).
// Only an answer decides: a service that does not answer (yet) is asked
// again at every poll, and no update mechanism runs meanwhile. Falling back
// to electron-updater after a while (as before) would let a slow or sick
// service switch a build with a public key to the weaker mechanism (no
// signature of the maintainer's own; security review sec-update).
// No Electron imports.
'use strict';

const SELFUPDATE = 'selfupdate';
const ELECTRON_UPDATER = 'electron-updater';
const MAX_ATTEMPTS = 6; // polls (5 s apart) without an answer before it is logged (once)

/**
 * updatePathFor(status) → 'selfupdate' | 'electron-updater' for a
 * GET /api/selfupdate answer.
 */
function updatePathFor(status) {
  if (!status || typeof status !== 'object' || typeof status.state !== 'string') return ELECTRON_UPDATER;
  if (status.state === 'not_configured' && status.reason === 'no_public_key') return ELECTRON_UPDATER;
  return SELFUPDATE;
}

/**
 * askService(api) → 'selfupdate' | 'electron-updater' | null (no answer yet).
 * A service without the in-app update answers 404 (older service) or 503
 * (server.unavailable: no updater): the electron-updater path.
 */
async function askService(api) {
  try {
    return updatePathFor(await api.selfUpdate({ timeoutMs: 3000 }));
  } catch (err) {
    if (err && (err.status === 404 || err.status === 503)) return ELECTRON_UPDATER;
    return null;
  }
}

class UpdatePathChooser {
  /**
   * api: { selfUpdate(opts) }; loadUpdater() → { updater, note } (electron-updater
   * or null; only called on the electron-updater path); createController(updater)
   * → an UpdateController (started here).
   */
  constructor({ api, loadUpdater, createController, log, maxAttempts = MAX_ATTEMPTS }) {
    this.api = api;
    this.loadUpdater = loadUpdater;
    this.createController = createController;
    this.log = log || { info() {}, warn() {} };
    this.maxAttempts = maxAttempts;
    this.state = 'undecided';
    this.attempts = 0;
    this.controller = null;
    this.deciding = null;
  }

  get decided() {
    return this.state !== 'undecided';
  }

  /** decide(): ask the service (again) until it answers; returns the state. */
  decide() {
    if (this.decided) return Promise.resolve(this.state);
    if (!this.deciding) this.deciding = this._decide().finally(() => { this.deciding = null; });
    return this.deciding;
  }

  async _decide() {
    this.attempts++;
    const choice = await askService(this.api);
    if (!choice && this.attempts === this.maxAttempts) {
      this.log.warn(`updates: the service did not say whether in-app updates are configured (${this.attempts} tries); no update mechanism runs until it does`);
    }
    if (!choice || this.decided) return this.state;
    this.state = choice;
    if (choice === SELFUPDATE) {
      this.log.info('updates: in-app updates (signed update.json) are configured; electron-updater is not loaded');
      return this.state;
    }
    const { updater, note } = this.loadUpdater();
    if (note) this.log.info(note);
    if (updater) {
      this.controller = this.createController(updater);
      this.controller.start();
    }
    return this.state;
  }
}

module.exports = { UpdatePathChooser, updatePathFor, askService, SELFUPDATE, ELECTRON_UPDATER, MAX_ATTEMPTS };
