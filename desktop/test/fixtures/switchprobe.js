// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Run by test/unit/switchprobe.test.js as the main script of the real
// Electron binary: the start check of lib/debugflags.js with Electron's own
// command line (app.commandLine.hasSwitch), as the packaged app would run
// it. Prints one line "SWITCHPROBE <json>" and exits before "ready".
// An error is printed the same way ({ error }) and exits 1 at once: an
// uncaught one would open Electron's error box and wait for the test's
// timeout.
'use strict';

const path = require('node:path');
const { app } = require('electron');

try {
  app.setPath('userData', process.env.LF_PROBE_USERDATA || path.join(__dirname, 'switchprobe-userdata'));
  const { refuseDebugging, sandboxDisabledByEnv, startRefusal } = require(path.join(__dirname, '..', '..', 'src', 'main', 'lib', 'debugflags'));
  // unpackaged: argv is [electron, this script, ...]; the packaged app's own arguments follow
  const argv = process.argv.slice(2);
  const hasSwitch = (s) => app.commandLine.hasSwitch(s);
  const refused = refuseDebugging({ isPackaged: true, argv, env: {}, hasSwitch });
  // sandboxEnv: the app's reading of ELECTRON_DISABLE_SANDBOX; noSandbox: Electron's
  // (it appends --no-sandbox when it turns the sandbox off)
  const sandboxEnv = sandboxDisabledByEnv({ isPackaged: true, env: process.env, platform: process.platform });
  // refusal: the whole start check of the packaged app (main.js), with the real parse
  const r = startRefusal({ isPackaged: true, argv, env: process.env, hasSwitch, platform: process.platform });
  process.stdout.write(`SWITCHPROBE ${JSON.stringify({
    argv,
    refused,
    electronAdded: app.commandLine.hasSwitch('allow-file-access-from-files'),
    sandboxEnv,
    noSandbox: app.commandLine.hasSwitch('no-sandbox'),
    refusal: r ? r.kind : null,
  })}\n`);
  app.exit(0);
} catch (err) {
  process.stdout.write(`SWITCHPROBE ${JSON.stringify({ error: String((err && err.stack) || err) })}\n`);
  app.exit(1);
}
