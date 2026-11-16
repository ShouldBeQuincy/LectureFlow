// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// actions.js as r8_lecture_test.mjs gives it to one copy of lecture_list.js
// (a resolve hook there): everything of the real module, with
// launchLectures(lectures, opts) taking the shared line's (r8-web-shared)
// opts.ask, (list) => Promise<boolean>: a false answer ends with [] and
// nothing sent. There it is asked after the glossary check; here before
// the real launchLectures, which does the glossary check and the request
// (the checks that use it have no glossary pending). The calls are
// recorded with the dialogs open when they came.

export * from '../actions.js';
import * as Real from '../actions.js';

export const launchCalls = [];

export function launchLectures(lectures, opts = {}) {
  const ask = opts && typeof opts.ask === 'function' ? opts.ask : null;
  const list = lectures.filter((l) => (l.lecture_actions || []).includes('launch'));
  const open = document.querySelectorAll('dialog').filter((d) => d.open && d.isConnected).length;
  launchCalls.push({ ids: list.map((l) => l.id), ask: !!ask, dialogsOpen: open });
  if (!list.length) return Promise.resolve([]);
  const key = list
    .map((l) => l.id)
    .sort()
    .join(',');
  return Real.once(`stand-in launch:${key}`, async () => {
    if (ask && !(await ask(list))) return [];
    return Real.launchLectures(list);
  });
}
