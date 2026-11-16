// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "Transcribe only" (SPEC-v3 V4.2 step 4, 3.1.0): a lecture launched
// without any model provider. Its API stages wait with waiting_for
// "no_provider" (internal/sched transcribe_only.go) and continue by
// themselves once a provider is set up. Meanwhile the lecture page and the
// player show the English only (no empty Chinese column), and the English
// subtitles and transcript can be downloaded as soon as asr is done
// (GET /api/lectures/{id}/english/{vtt|txt}, made from the sentences, not
// by the export stage).

import { api } from './api.js';
import { h, download } from './dom.js';
import { icon } from './icons.js';
import { menuButton } from './menu.js';
import { exportItems } from './actions.js';
import * as U from './util.js';
import { t } from './i18n.js';

export const WAIT_NO_PROVIDER = 'no_provider';

const LLM_STAGES = ['summary', 'repair', 'translate'];

/** True while one of the lecture's API stages waits for a model provider. */
export function waitsForProvider(lec) {
  return !!(lec && Array.isArray(lec.stages)) && lec.stages.some((s) => LLM_STAGES.includes(s.stage) && (s.status === 'pending' || s.status === 'queued') && s.waiting_for === WAIT_NO_PROVIDER);
}

/**
 * True when the lecture has (and for now gets) no Chinese at all: no
 * sentence has a translation and its API stages wait for a provider. The
 * pages then show the English only.
 */
export function englishOnly(lec) {
  if (!waitsForProvider(lec)) return false;
  const c = lec.counts || {};
  return (c.zh_ok || 0) + (c.zh_qa_failed || 0) + (c.zh_stale || 0) === 0;
}

/** URL of the lecture's on-demand English file (format "vtt" or "txt"). */
export function englishURL(lectureId, format) {
  return api.exportURL(`/api/lectures/${encodeURIComponent(lectureId)}/english/${format}`);
}

// The English files' sizes asked so far: `${url}\n${content}` → a promise
// of the bytes (null: not known). Each answer is the server making the
// file whole (sentences read, file written): asked once per lecture
// content, a failure asked again next time.
const sizes = new Map();
const SIZES_MAX = 64;

// What the files are made from, as the page knows it: the stages (an edit
// starts an incremental translation, a rerun changes them) and the counts.
function contentKey(lec) {
  return JSON.stringify([(lec.stages || []).map((s) => [s.stage, s.status, s.finished_at || '']), lec.counts || null]);
}

function askSize(url, content) {
  const k = `${url}\n${content}`;
  let p = sizes.get(k);
  if (p) return p;
  p = fetch(url, { method: 'HEAD', cache: 'no-store' }).then(
    (r) => {
      const n = r && r.ok && r.headers ? Number(r.headers.get('Content-Length')) : NaN;
      return n > 0 ? n : null;
    },
    () => null,
  );
  sizes.set(k, p);
  if (sizes.size > SIZES_MAX) sizes.delete(sizes.keys().next().value);
  p.then((n) => n == null && sizes.get(k) === p && sizes.delete(k));
  return p;
}

/**
 * item with the size of an English file as its hint (T25 B3: the export
 * files showed theirs, these none). They are made on demand: the size is
 * asked (HEAD, the server's Content-Length) when the item is drawn — menu.js
 * reads the hint then: the "download" menu when it opens, the lecture
 * menu's download submenu when that opens, not every time a lecture's menu
 * is built (R74 low 4) — and shown once it comes; `known` (bytes) is shown
 * meanwhile. A size that cannot be asked leaves the hint as it is: the
 * download works the same.
 */
function withSize(item, url, content, known = 0) {
  let el = null;
  Object.defineProperty(item, 'hint', {
    enumerable: true,
    get() {
      if (!el) {
        el = h('span', { class: 'en-size' }, known > 0 ? U.fmtBytes(known) : '');
        askSize(url, content).then((n) => {
          if (n) el.textContent = U.fmtBytes(n);
        });
      }
      return el;
    },
  });
  return item;
}

/**
 * Menu items of the English subtitles (.vtt) and transcript (.txt); the
 * server names the files "{number} {English title}.en.vtt" (P13.6). The
 * export's own English text, when export ran, has the transcript's size.
 */
export function englishItems(lec) {
  const txt = (lec.exports || []).find((f) => f.name.endsWith('.en.txt'));
  const content = contentKey(lec);
  return [
    withSize({ id: 'english:vtt', label: t('menu.export.vtt_en'), desc: t('menu.export.vtt_en_desc'), icon: 'captions', onSelect: () => download(englishURL(lec.id, 'vtt')) }, englishURL(lec.id, 'vtt'), content),
    withSize({ id: 'english:txt', label: t('menu.export.txt_en'), desc: t('menu.export.txt_en_desc'), icon: 'file-text', onSelect: () => download(englishURL(lec.id, 'txt')) }, englishURL(lec.id, 'txt'), content, txt ? txt.size : 0),
  ];
}

/**
 * The lecture page's download menu: the export files (when export ran)
 * and, once the lecture is playable (asr done), the English files made on
 * demand. The export's own English text is left out: the on-demand one has
 * the same content and is always current. null when there is nothing.
 */
export function lectureDownloadItems(lec) {
  const exports = (lec.exports || []).filter((f) => !f.name.endsWith('.en.txt'));
  const items = exportItems(exports);
  if (!lec.playable) return (lec.exports || []).length ? exportItems(lec.exports) : null;
  const en = englishItems(lec);
  if (!items.length) return en;
  // before the "Traditional Chinese" group exportItems ends with
  const sep = items.indexOf('sep');
  if (sep < 0) return [...items, 'sep', ...en];
  return [...items.slice(0, sep), 'sep', ...en, ...items.slice(sep)];
}

export function lectureDownloadMenu(lec, fkey) {
  return menuButton(
    h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey } }, icon('download', { size: 14 }), h('span', { class: 'btn-label' }, t('common.download')), icon('chevron-down', { size: 13, class: 'btn-caret' })),
    () => lectureDownloadItems(lec) || [],
  );
}
