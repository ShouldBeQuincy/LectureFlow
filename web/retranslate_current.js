// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x P1-b (Q10a): "用当前设置重翻此句" in the sentence menus (player and
// the lecture's transcript tab). A sentence the model left without a usable
// translation (zh_status qa_failed, "待处理") can be sent to the translate
// role's current model (POST .../retranslate {"use_current": true}); the
// lecture's other translations stay, so it may then mix two models. The
// current setting may be unusable (409 snapshot.unusable.*: provider gone,
// no key…): the reason is shown with what it refused.

import { api, ApiError } from './api.js';
import { toast } from './dom.js';
import { t } from './i18n.js';
import { translationHeld } from './retranslate_watch.js';

// Whether the sentence may be sent to the current model: only a sentence
// without a usable translation (the back end refuses others with 409
// sentence.not_pending).
export function canRetranslateCurrent(s) {
  return !!s && s.zh_status === 'qa_failed';
}

// The toast text of a refused request: the current setting's own reason
// (snapshot.unusable.*) after what could not be done; any other error as
// the API layer words it.
export function retranslateCurrentError(e) {
  const msg = (e && e.message) || t('common.unknown_error');
  if (e instanceof ApiError && e.status === 409 && String(e.code || '').startsWith('snapshot.unusable.')) {
    return t('player.menu.retranslate_current_unusable', { reason: msg });
  }
  return msg;
}

// Round 9 WV-01 (T28 suggestion 3): what a sentence action says while the
// lecture's translation is held (the *_held texts: two sentences, with the
// way out) stays until closed, as "the same translation came back" does
// (retranslate_watch.js, round 8 WL-05): an info toast, not a success one —
// the result the user asked for has not come yet, the text says what to do
// for it. The short texts said when the translation is not held fade as
// before.
export function heldToast(message) {
  toast(message, 'info', { sticky: true });
}

// Sends it; resolves to the updated Sentence, or null when it was refused
// (the reason toasted here). lec: the lecture as it was when it was sent
// (R78c, round 8 WL-06): while its translation is held (stale, failed,
// cancelled: retranslate_watch.js translationHeld) the sentence is only
// marked and translated when the translation runs again, and the toast
// says so (player.menu.retranslate_held, as the other sentence actions
// say), not "稍后自动更新" — the mixed models' sentence promises that too.
export async function retranslateCurrent(lectureId, idx, lec = null) {
  const held = translationHeld(lec);
  try {
    const res = await api.sentenceRetranslateCurrent(lectureId, idx, { quiet: true });
    if (held) heldToast(t('player.menu.retranslate_held'));
    else toast(res && res.mixed ? t('player.menu.retranslate_current_mixed') : t('player.menu.retranslate_done'), 'success');
    return res;
  } catch (e) {
    toast(retranslateCurrentError(e), 'warn');
    return null;
  }
}
