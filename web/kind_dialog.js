// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "Change type…" of a lecture (lecture kinds): every kind of the catalog as a
// radio list (taught sessions, then teaching materials), where the current
// kind came from, and "use the automatic type" for one set by hand. The
// kind changes citation numbers (W02T1), so a course whose citations were
// exported asks first (409 calendar.renumber, as for recording times).

import { api } from './api.js';
import { h, openDialog, toast } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { withRenumber } from './calendar_ui.js';
import { kindList, kindName, kindOf, kindSourceText, lectureLabel, loadKinds } from './kinds.js';

/** opts: {onSaved(lecture)} */
export async function openKindDialog(lec, opts = {}) {
  await loadKinds(); // custom kinds added in another tab
  const cur = kindOf(lec);
  const l = await openDialog(
    t('kind.dialog.title'),
    (close, busy) => {
      const name = 'kind' + Math.random().toString(36).slice(2);
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const save = h('button', { type: 'submit', class: 'btn btn-primary', disabled: true }, t('common.save'));
      const groups = ['session', 'material'].map((g) => {
        const kinds = kindList().filter((k) => k.group === g);
        if (!kinds.length) return null;
        return h(
          'fieldset',
          { class: 'kdlg-group' },
          h('legend', { class: 'kdlg-legend' }, t(`kind.group.${g}`)),
          kinds.map((k) =>
            h(
              'label',
              { class: 'dlg-choice kdlg-choice' },
              h('input', { type: 'radio', name, value: k.id, checked: k.id === cur, autofocus: k.id === cur, onchange: () => (save.disabled = false) }),
              h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, kindName(k.id, 'full')), h('span', { class: 'dlg-choice-hint' }, k.builtin ? t('kind.dialog.code', { code: k.code }) : t('kind.dialog.custom_code', { code: k.code }))),
            ),
          ),
        );
      });
      const send = async (kind) => {
        save.disabled = true;
        err.textContent = '';
        try {
          // busy: a route change while it runs lets it finish (R62 N4)
          const updated = await busy(withRenumber((confirm) => api.setLectureKind(lec.id, kind, confirm, { quiet: true })));
          if (updated) close(updated);
          else save.disabled = false; // kept the old numbers
        } catch (ex) {
          save.disabled = false;
          err.textContent = ex.message;
        }
      };
      return h(
        'form',
        {
          class: 'kdlg',
          novalidate: true,
          onsubmit: (e) => {
            e.preventDefault();
            const sel = e.currentTarget.querySelector(`input[name="${name}"]:checked`);
            if (sel && !save.disabled) send(sel.value);
          },
        },
        h('p', { class: 'dlg-msg' }, h('span', { class: 'rec-name' }, lectureLabel(lec))),
        h('p', { class: 'kdlg-current' }, icon('tag', { size: 13 }), t('kind.dialog.current', { kind: kindName(cur, 'full'), source: kindSourceText(lec.kind_source) })),
        h('div', { class: 'kdlg-groups', role: 'radiogroup', 'aria-label': t('kind.dialog.title') }, groups),
        h('p', { class: 'dlg-hint' }, t('kind.dialog.hint')),
        err,
        h(
          'div',
          { class: 'dlg-actions' },
          lec.kind_source === 'user' ? h('button', { type: 'button', class: 'btn btn-ghost rec-auto', title: t('kind.dialog.auto_tip'), onclick: () => send('') }, t('kind.dialog.auto')) : null,
          h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')),
          save,
        ),
      );
    },
    { className: 'dlg-kind' },
  );
  if (l) {
    toast(l.kind_source === 'user' ? t('kind.dialog.saved', { kind: kindName(kindOf(l), 'full') }) : t('kind.dialog.auto_saved', { kind: kindName(kindOf(l), 'full') }), 'success');
    if (opts.onSaved) opts.onSaved(l);
  }
  return l;
}
