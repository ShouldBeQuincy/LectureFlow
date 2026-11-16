// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "收紧权限" in the diagnostics row data_dir (security review sec-update M2;
// datadir-ui, web-builder). While other accounts of this computer can change
// or read the data folder, the server turns the row into a warning
// (check.data_dir_shared / check.data_dir_readable) with the hint
// check.hint_data_dir_restrict (internal/api/system_datadir.go); only then
// the hint gets this button. After a short confirmation it asks
// POST /api/system/data-dir/restrict to give the folder itself private
// permissions (what lies below inherits them; nothing else is touched),
// toasts system.data_dir_restricted on success and reads the diagnostics
// again: the row is the server's, nothing changes optimistically. A failure
// (data_dir.restrict_failed: 500 the permissions could not be set, 409
// other accounts can still reach the folder — another account owns it, or
// the drive keeps no permissions) is the coded error's text in an error
// toast, which stays until it is closed (the reason is long); the
// diagnostics are read again as well.

import { h, toast, confirmDialog } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { api } from './api.js';

// The hint of a data folder that other accounts can reach (system_datadir.go).
export const RESTRICT_HINT = 'check.hint_data_dir_restrict';

// restrictable(c): the row offers "收紧权限".
export const restrictable = (c) => !!c && c.name === 'data_dir' && !!c.hint_info && c.hint_info.code === RESTRICT_HINT;

// opts: {refresh() → Promise (GET /api/system and render), changed() (render)}
export function createDataDirRestrict(opts) {
  let busy = false;
  let alive = true;

  async function restrict(path) {
    if (busy) return;
    const ok = await confirmDialog({
      title: t('system.data_dir_restrict_title'),
      message: t('system.data_dir_restrict_confirm', { path }),
      okText: t('system.data_dir_restrict'),
    });
    if (!ok || !alive || busy) return;
    busy = true;
    opts.changed();
    try {
      const res = await api.restrictDataDir();
      if (alive) {
        toast(t('system.data_dir_restricted'), 'success');
        // the update's rollback state was kept where others could change
        // it and went with the old permissions (system_datadir.go)
        if (res && res.rollback_reset) toast(t('system.data_dir_rollback_reset'), 'info');
      }
    } catch (e) {
      if (alive) {
        toast(e.message || t('common.load_failed'), 'error');
        // the state can go although the request then fails (409): the
        // error body says so next to "error"
        if (e.body && e.body.rollback_reset) toast(t('system.data_dir_rollback_reset'), 'info');
      }
    }
    // the row is the server's: read the diagnostics before the button is usable again
    try {
      if (alive) await opts.refresh();
    } finally {
      busy = false;
      if (alive) opts.changed();
    }
  }

  return {
    // button(c): "收紧权限" for a restrictable row
    button(c) {
      const path = (c.detail_info && c.detail_info.params && c.detail_info.params.path) || '';
      return h(
        'button',
        { type: 'button', class: 'btn btn-sm', disabled: busy, dataset: { fkey: 'sys:datadir:restrict' }, onclick: () => restrict(path) },
        icon(busy ? 'loader-circle' : 'lock', { size: 14, class: busy ? 'spin' : '' }),
        h('span', { class: 'btn-label' }, t(busy ? 'system.data_dir_restricting' : 'system.data_dir_restrict')),
      );
    },
    destroy() {
      alive = false;
    },
  };
}
