// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Small shared pieces of UI built from back-end data.

import { h } from './dom.js';
import { icon } from './icons.js';
import { menuButton } from './menu.js';
import { exportItems } from './actions.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { zhOn } from './zh_display.js';

/**
 * The line under an editor of Chinese lecture content (glossary rendering,
 * Chinese title) while it is shown in Traditional characters (P14.6): the
 * editor holds the stored Simplified text, saved as typed and converted
 * only for display. null in Simplified mode.
 */
export function zhEditNote(id) {
  if (!zhOn()) return null;
  return h('p', { class: 'zh-edit-note', id: id || undefined }, icon('info', { size: 12 }), h('span', null, t('common.zh_edit_note')));
}

// "Download ▾" button opening the export files as a menu (same items as the
// lecture context menu's Export ▸).
export function downloadMenu(exports, fkey) {
  return menuButton(
    h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey } }, icon('download', { size: 14 }), h('span', { class: 'btn-label' }, t('common.download')), icon('chevron-down', { size: 13, class: 'btn-caret' })),
    () => exportItems(exports),
  );
}

// Word-level diff a -> b: deletions struck through, insertions underlined.
export function diffView(a, b) {
  const ops = U.wordDiff(a, b);
  return h(
    'span',
    { class: 'diff' },
    ops.map((op, i) => [
      i ? ' ' : null,
      op.t === 'eq'
        ? op.w
        : op.t === 'del'
          ? h('del', null, h('span', { class: 'visually-hidden' }, t('common.diff.deleted_open')), op.w, h('span', { class: 'visually-hidden' }, t('common.diff.close')))
          : h('ins', null, h('span', { class: 'visually-hidden' }, t('common.diff.inserted_open')), op.w, h('span', { class: 'visually-hidden' }, t('common.diff.close'))),
    ]),
  );
}

function uploadStatusText(item) {
  if (item.status === 'waiting') return item.local ? t('common.native.waiting') : t('common.upload.waiting');
  if (item.status === 'failed') return t('common.upload.failed', { error: item.error });
  // desktop app: the server copies the local file, no byte progress (SPEC-v3 V1.4)
  if (item.local) return t('common.native.copying');
  if (item.fraction == null) return t('common.upload.uploading', { loaded: U.fmtBytes(item.loaded) });
  if (item.fraction >= 1) return t('common.upload.confirming');
  return t('common.upload.uploading_pct', { pct: U.pct(item.fraction), loaded: U.fmtBytes(item.loaded), total: U.fmtBytes(item.total) });
}

// One row of the upload queue (SPEC §9.4.2: progress, "retry upload" on failure).
// The file name is shown once; a lecture whose title is not simply the file
// name without its extension (§9.4) shows the title first.
export function uploadRow(item, { onRetry, onRemove }) {
  const name = item.file.name;
  const title = item.kind === 'lecture' && item.title && item.title !== U.stripExt(name) ? item.title : '';
  const buttons = [];
  if (item.status === 'failed') {
    buttons.push(h('button', { type: 'button', class: 'btn btn-sm btn-primary', dataset: { fkey: `${item.key}:retry` }, onclick: () => onRetry(item) }, t('common.upload.retry')));
    buttons.push(h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey: `${item.key}:remove` }, onclick: () => onRemove(item) }, t('common.remove')));
  } else if (item.status === 'waiting') {
    buttons.push(h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey: `${item.key}:remove` }, onclick: () => onRemove(item) }, t('common.remove')));
  } else {
    buttons.push(h('button', { type: 'button', class: 'btn btn-sm', dataset: { fkey: `${item.key}:remove` }, onclick: () => onRemove(item) }, item.local ? t('common.native.cancel') : t('common.upload.cancel')));
  }
  return h(
    'li',
    { class: `upload-row upload-${item.status}`, dataset: { up: item.key } },
    h('span', { class: 'up-name', title: name }, title || name, h('span', { class: 'muted' }, `${title ? ` · ${name}` : ''}${item.file.size == null ? '' : ` · ${U.fmtBytes(item.file.size)}`}`)),
    // named by its file (L16); the value (or none: indeterminate) is the element's own
    item.status === 'failed' ? null : h('progress', { class: 'up-progress', max: '1', 'aria-label': title || name, value: item.status === 'uploading' ? item.fraction : 0 }),
    h('span', { class: 'up-status', text: uploadStatusText(item) }),
    h('span', { class: 'up-actions' }, buttons),
  );
}

// In-place progress update (avoids re-rendering on every progress event).
export function updateUploadRow(container, item) {
  const row = container.querySelector(`[data-up="${CSS.escape(item.key)}"]`);
  if (!row) return false;
  const prog = row.querySelector('.up-progress');
  if (prog) {
    if (item.fraction == null) prog.removeAttribute('value');
    else prog.value = item.fraction;
  }
  const st = row.querySelector('.up-status');
  if (st) st.textContent = uploadStatusText(item);
  return true;
}
