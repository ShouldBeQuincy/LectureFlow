// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "Translate again" of one sentence (the lecture page's transcript, the
// player) marks it and the translation comes some seconds later with the
// sentences' SSE refresh. When the model gave back the very same text the
// page changed nothing and said nothing: the user could not tell whether it
// ran at all (T25 S3). A watch remembers the translation each such sentence
// had and, when the sentence is read again with its retranslation done,
// says so if nothing changed, with the ways to a different translation
// (the translation itself cannot be edited: the English sentence and the
// glossary are what it is made from).
//
// R74 low 5: it compares only
// - a sentence that had a translation (one without any, "translated again"
//   without one, is not "the same");
// - after a read has shown the sentence waiting (stale): a read sent before
//   the retranslation and answered after it still has the old translation
//   as done;
// - within RESULT_MS: a retranslation that never came (failed, cancelled)
//   is not matched by a whole update much later.
// The notice stays until the user closes it: it comes some seconds after
// the action, and it is two sentences long (dom.js's sticky toast, round 8
// F1: merged and counted as other toasts are).

import { toast } from './dom.js';
import { t } from './i18n.js';

const RESULT_MS = 30 * 60 * 1000;

/**
 * Whether the lecture's translation is held: its stage is stale, failed or
 * cancelled. An edited, restored or retranslated sentence is then only
 * marked; it is translated when the translation runs again ("Update" of
 * the lecture, a retry): sched processIncremental drops the request, and
 * state.StartIncrementalTranslate starts only from a done translation
 * (R78b medium 2). Pending, queued or running: the request waits for that
 * run, so it is not held.
 */
export function translationHeld(lec) {
  const st = lec && (lec.stages || []).find((s) => s.stage === 'translate');
  return !!st && (st.status === 'stale' || st.status === 'failed' || st.status === 'cancelled');
}

export function retranslateWatch() {
  const asked = new Map(); // idx → {zh: the translation when asked, at, waiting: seen stale since}
  return {
    /** s: the sentence as it was when "translate again" was sent. */
    start(s) {
      if (s && s.zh) asked.set(s.idx, { zh: s.zh, at: Date.now(), waiting: false });
      else if (s) asked.delete(s.idx);
    },
    /** The sentence changed otherwise (edited, reset): nothing to compare. */
    drop(idx) {
      asked.delete(idx);
    },
    /** A sentence read again; true when the same translation was told. */
    seen(s) {
      const a = s && asked.get(s.idx);
      if (!a) return false;
      if (Date.now() - a.at > RESULT_MS) {
        asked.delete(s.idx);
        return false;
      }
      if (s.zh_status === 'stale') {
        a.waiting = true;
        return false;
      }
      if (!a.waiting) return false; // read before the retranslation began
      asked.delete(s.idx);
      if (!s.zh || s.zh !== a.zh) return false;
      toast(t('transcript.retranslate.same'), 'info', { sticky: true });
      return true;
    },
  };
}
