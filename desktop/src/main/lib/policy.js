// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Window-close and sleep decisions (SPEC-v3 V1.1). Pure functions over the
// service's /api/app/activity and /api/app/settings answers.
'use strict';

const CLOSE_BEHAVIORS = ['ask', 'background', 'quit'];

// The counts of /api/app/activity (internal/api/desktop.go AppActivity):
//   running    jobs running + fetches of recordings in progress
//   queued     jobs queued + fetches waiting
//   downloads  fetches in progress or waiting (already counted above)
function count(activity, key) {
  const n = activity && activity[key];
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
}

function runningCount(activity) {
  return count(activity, 'running');
}

function queuedCount(activity) {
  return count(activity, 'queued');
}

/**
 * hasTasks: something quitting would stop. Queued tasks count too (T26
 * J10): queued jobs only wait for the next start (SPEC §4.7 leaves them
 * alone), but a waiting fetch loses its credentials like a running one.
 */
function hasTasks(activity) {
  return runningCount(activity) + queuedCount(activity) > 0;
}

/**
 * decideClose: what closing the main window does.
 *   'quit'       stop the service and exit
 *   'background' keep running without the window (tray on Windows, dock on macOS)
 *   'ask'        show the close dialog
 *   'hide'       macOS without tasks: close the window, stay in the dock
 * activity is null when the service could not be asked (it then cannot be
 * doing anything worth keeping alive for).
 */
function decideClose({ platform, activity, settings }) {
  if (!hasTasks(activity)) return platform === 'darwin' ? 'hide' : 'quit';
  const b = settings && settings.close_behavior;
  if (b === 'background' || b === 'quit') return b;
  return 'ask';
}

/**
 * decideQuit: what "Quit" in the tray menu does (T26 S1) — 'quit' or 'ask'
 * (the close dialog). The same question as closing the window, but an
 * explicit Quit never becomes "stay in the background": a remembered
 * 'background' asks, a remembered 'quit' quits.
 */
function decideQuit({ activity, settings }) {
  if (!hasTasks(activity)) return 'quit';
  return settings && settings.close_behavior === 'quit' ? 'quit' : 'ask';
}

/**
 * pendingWork: { running, queued, downloads, jobs } — downloads are the
 * fetches of recordings among running + queued, jobs the lecture and course
 * jobs (the rest).
 */
function pendingWork(activity) {
  const running = runningCount(activity);
  const queued = queuedCount(activity);
  const downloads = Math.min(count(activity, 'downloads'), running + queued);
  return { running, queued, downloads, jobs: running + queued - downloads };
}

/**
 * closeDialogKeys: the message and detail of the close dialog (T26 S1).
 * A restart continues lecture and course jobs (SPEC §4.7), but an
 * unfinished fetch of a recording becomes "needs refresh" (its sign-in data
 * is in memory only; internal/fetch Queue.Start), so with fetches the
 * detail says how to resume them, and with both kinds it says both.
 */
function closeDialogKeys(activity) {
  const w = pendingWork(activity);
  const message = w.running === 0 && w.queued > 0 ? 'close.message_queued' : 'close.message';
  let detail = 'close.detail';
  if (w.downloads > 0) detail = w.jobs > 0 ? 'close.detail_both' : 'close.detail_downloads';
  return { message, detail };
}

// Buttons of the close dialog, in order. Esc / closing the dialog = cancel.
const CLOSE_DIALOG_BUTTONS = ['background', 'quit', 'cancel'];

/**
 * interpretCloseDialog turns showMessageBox's { response, checkboxChecked }
 * into { action: 'background'|'quit'|'cancel', remember: 'background'|'quit'|null }.
 */
function interpretCloseDialog(result) {
  const action = CLOSE_DIALOG_BUTTONS[result && result.response] || 'cancel';
  const remember = action !== 'cancel' && !!(result && result.checkboxChecked) ? action : null;
  return { action, remember };
}

/**
 * wantSleepBlock: keep the system awake while tasks run and the setting
 * allows it. Unknown settings fall back to the default (prevent_sleep: true).
 */
function wantSleepBlock(activity, settings) {
  // A device self-test only sets transcribing (it is not a running job).
  if (runningCount(activity) === 0 && !(activity && activity.transcribing)) return false;
  return !(settings && settings.prevent_sleep === false);
}

module.exports = {
  CLOSE_BEHAVIORS, CLOSE_DIALOG_BUTTONS, runningCount, queuedCount, hasTasks, pendingWork, decideClose, decideQuit, closeDialogKeys,
  interpretCloseDialog, wantSleepBlock,
};
