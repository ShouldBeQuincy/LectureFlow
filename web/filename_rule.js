// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Recording times from file names (SPEC-v2 P13.3.3): the calm notice on the
// course page ("the format was recognized, view the result" / the undoable
// "recording times were set from the file names"), the preview dialog (file
// name → date and time → title → status, the model's one-sentence
// explanation, live preview while the regex is edited, apply / ignore),
// "recognize the format again" and the recording-time part of the course
// settings. The back end
// validates and parses; this module only shows what it returns.
// lecture kinds: the rule's optional (?P<kind>…) group and its kind_map (code
// → lecture kind, edited here); the preview shows each file's kind.

import { api } from './api.js';
import { h, append, clear, btn, iconBtn, openDialog, toast, skeleton, errorBlock } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { withRenumber, parseYMD, fmtDayShort } from './calendar_ui.js';
import * as U from './util.js';
import { kindGroup, kindList, kindName, loadKinds } from './kinds.js';

const STATUS_TONE = { ok: 'ok', unmatched: 'quiet', invalid: 'err', suspicious: 'warn' };

// The undo notice can be put away; it stays away for this rule.
const SEEN_KEY = 'lectureflow.filenameUndoSeen';

function seenUndo(course) {
  try {
    const m = JSON.parse(localStorage.getItem(SEEN_KEY) || '{}');
    return !!course.filename_rule && m[course.id] === course.filename_rule.created_at;
  } catch {
    return false;
  }
}

function markUndoSeen(course) {
  try {
    const m = JSON.parse(localStorage.getItem(SEEN_KEY) || '{}');
    m[course.id] = course.filename_rule ? course.filename_rule.created_at : '';
    localStorage.setItem(SEEN_KEY, JSON.stringify(m));
  } catch {
    // private mode: shown again next time
  }
}

/** The "organizer" role is not configured (409 from generate). */
function isNotConfigured(err) {
  const e = err && err.body && err.body.error;
  const code = e && typeof e === 'object' ? e.code || '' : '';
  return !!err && err.status === 409 && /not_configured|organizer/.test(code);
}

function fmtParsed(row) {
  const d = parseYMD(row.recorded_at);
  if (!d) return '';
  const m = /T(\d{2}:\d{2})/.exec(row.recorded_at || '');
  const day = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  return row.has_time && m ? `${day} ${m[1]}` : day;
}

// Why a row failed: its coded reason (error_info, P14.3), else the text.
function rowReason(row) {
  return U.errorInfoText(row.error_info, row.error || '');
}

// The validation problems: coded (problems_info), else the texts.
function problemTexts(p) {
  if (p && Array.isArray(p.problems_info) && p.problems_info.length) return p.problems_info.map((x) => U.errorInfoText(x));
  return (p && p.problems) || [];
}

// kind_map as sorted [code, kind] pairs (for comparing)
function mapKey(m) {
  return JSON.stringify(Object.entries(m || {}).sort(([a], [b]) => (a < b ? -1 : 1)));
}

// The preview's kind cell: "Tutorial (T)", "X (not mapped)", or nothing; a
// file recorded more than 30 days before the term start (past) is a past
// lecture when its kind is a class session (or none, a lecture), as the back
// end's effective kind; a kind of the material group stays itself.
function kindCell(r) {
  if (r.past && (!r.kind || kindGroup(r.kind) === 'session')) {
    const kind = kindName('past_lecture');
    return h('span', { class: 'fn-kind', title: t('kind.source.date') }, r.kind_code ? t('course.filename.kind_cell', { kind, code: r.kind_code }) : kind);
  }
  if (r.kind) return h('span', { class: 'fn-kind' }, t('course.filename.kind_cell', { kind: kindName(r.kind), code: r.kind_code }));
  if (r.kind_code) return h('span', { class: 'fn-kind is-unmapped', title: t('course.filename.kind_unmapped_tip', { code: r.kind_code }) }, t('course.filename.kind_unmapped', { code: r.kind_code }));
  return h('span', { class: 'dim' }, '—');
}

function fmtParsedHint(row) {
  const d = parseYMD(row.recorded_at);
  return d ? fmtDayShort(d) : ''; // weekday and date in the language's own order
}

/**
 * "Recognize the format again" (P13.3.2): queues the course's filename_rule task.
 * Resolves to '' on success or to the reason it could not start (shown by
 * the caller next to the button).
 */
export async function regenerateFilenameRule(course) {
  try {
    await api.generateFilenameRule(course.id, { quiet: true });
    toast(t('course.filename.regenerate_started'), 'success');
    return '';
  } catch (err) {
    if (isNotConfigured(err)) return t('course.filename.organizer_missing');
    return err.message || t('course.filename.regenerate_failed');
  }
}

// regenerateFilenameRule for a dialog's busy() (dom.js openDialog): the
// reason it could not start is a rejection, which busy tells as a toast
// when the dialog has left the page meanwhile (R62 N4, N5).
function regenerateOrThrow(course) {
  return regenerateFilenameRule(course).then((why) => {
    if (why) throw new Error(why);
  });
}

/**
 * The notice above the lecture list, or null. handlers: {onChanged(course),
 * onOpen()} — onOpen opens the preview dialog.
 */
export function filenameNotice(course, handlers) {
  const task = course.filename_task;
  if (course.filename_undo && !seenUndo(course)) {
    const undoBtn = btn(t('course.filename.undo'), { size: 'sm', icon: 'undo-2', fkey: 'fn:undo', onclick: () => undo() });
    const box = h(
      'div',
      { class: 'fn-notice is-done', role: 'status' },
      icon('circle-check', { size: 15, class: 'fn-notice-icon' }),
      h('span', { class: 'fn-notice-text' }, t('course.filename.applied_auto')),
      h('span', { class: 'fn-notice-actions' }, undoBtn, btn(t('course.filename.view_rule'), { size: 'sm', kind: 'ghost', fkey: 'fn:view', onclick: handlers.onOpen })),
      iconBtn('x', t('course.filename.dismiss'), {
        class: 'fn-notice-close',
        fkey: 'fn:dismiss',
        onclick: () => {
          markUndoSeen(course);
          box.remove();
        },
      }),
    );
    async function undo() {
      undoBtn.disabled = true;
      try {
        const c = await withRenumber((confirm) => api.undoFilenameRule(course.id, confirm, { quiet: true }));
        if (c) {
          toast(t('course.filename.undone'), 'success');
          handlers.onChanged(c);
        } else undoBtn.disabled = false;
      } catch (err) {
        undoBtn.disabled = false;
        toast(err.message, 'error');
      }
    }
    return box;
  }
  if (course.filename_proposal) {
    return h(
      'div',
      { class: 'fn-notice', role: 'status' },
      icon('sparkles', { size: 15, class: 'fn-notice-icon' }),
      h('span', { class: 'fn-notice-text' }, t('course.filename.proposal')),
      h('span', { class: 'fn-notice-actions' }, btn(t('course.filename.view_result'), { size: 'sm', kind: 'primary', fkey: 'fn:open', onclick: handlers.onOpen })),
    );
  }
  if (task && (task.status === 'queued' || task.status === 'running')) {
    return h('div', { class: 'fn-notice is-quiet', role: 'status' }, icon('loader-circle', { size: 15, class: 'fn-notice-icon spin' }), h('span', { class: 'fn-notice-text' }, t('course.filename.running')));
  }
  if (task && task.status === 'failed') {
    const why = U.stageErrorText(task);
    const err = h('span', { class: 'fn-notice-err', role: 'alert' });
    const again = btn(t('course.filename.retry'), {
      size: 'sm',
      kind: 'ghost',
      icon: 'rotate-cw',
      fkey: 'fn:retry',
      onclick: async () => {
        again.disabled = true;
        err.textContent = await regenerateFilenameRule(course);
        again.disabled = false;
        if (!err.textContent) handlers.onChanged(null);
      },
    });
    return h(
      'div',
      { class: 'fn-notice is-quiet' },
      icon('info', { size: 15, class: 'fn-notice-icon' }),
      h('span', { class: 'fn-notice-text', title: why || '' }, t('course.filename.failed'), why ? h('span', { class: 'fn-notice-detail' }, U.excerpt(why.split('\n')[0], 120)) : null),
      h('span', { class: 'fn-notice-actions' }, again),
      err,
    );
  }
  return null;
}

/**
 * The preview dialog (P13.3.3). opts: {onChanged(course)} — called after
 * apply, ignore or a regenerate request.
 */
export function openFilenameRuleDialog(course, opts = {}) {
  return openDialog(
    t('course.filename.title'),
    (close, busy) => {
      let view = null; // FilenameRuleView
      let preview = null; // the table currently shown: {preview, matched, total, ok, problems, problems_info}
      let regex = '';
      let kindMap = {}; // lecture kinds: code → lecture kind id
      let edited = false;
      let ctl = null; // AbortController of the preview request in flight
      let timer = null;
      let previewErr = '';

      const body = h('div', { class: 'fn-body' }, skeleton(6));
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const foot = h('div', { class: 'dlg-actions fn-actions' });
      const box = h('div', { class: 'fn-dlg' }, body, err, foot);

      async function load() {
        clear(body);
        body.append(skeleton(6));
        try {
          view = await api.filenameRule(course.id, { quiet: true });
        } catch (e) {
          clear(body);
          body.append(errorBlock(e.message, load));
          renderFoot();
          return;
        }
        const p = view.proposal;
        regex = p ? p.rule.regex : view.rule ? view.rule.regex : '';
        kindMap = { ...((p ? p.kind_map : view.kind_map) || {}) };
        await loadKinds();
        preview = p ? { preview: p.preview, matched: p.matched, total: p.total, ok: p.ok, problems: p.problems, problems_info: p.problems_info } : view.rule ? { preview: view.preview, matched: view.rule.samples_matched, total: view.rule.samples_total, ok: true, problems: [] } : null;
        render();
      }

      const regexInput = h('textarea', {
        class: 'input mono fn-regex',
        rows: '2',
        spellcheck: 'false',
        autocomplete: 'off',
        'aria-label': t('course.filename.regex'),
        dataset: { fkey: 'fn:regex' },
        oninput: () => {
          const had = /\(\?P<kind>/.test(regex);
          regex = regexInput.value;
          edited = true;
          if (had !== /\(\?P<kind>/.test(regex)) renderKinds(); // the hint says whether the rule has a kind group
          schedulePreview();
          renderFoot();
        },
      });
      const kindsEl = h('div', { class: 'fn-kinds' });
      const status = h('div', { class: 'fn-status', role: 'status' });

      // The kind_map editor: one row per code (lecture kinds).
      function renderKinds() {
        clear(kindsEl);
        const hasGroup = /\(\?P<kind>/.test(regex);
        const rows = Object.entries(kindMap).sort(([a], [b]) => (a < b ? -1 : 1));
        const changed = () => {
          edited = true;
          schedulePreview();
          renderFoot();
        };
        const kinds = kindList();
        kindsEl.append(
          h('span', { class: 'dlg-label' }, t('course.filename.kinds')),
          h('span', { class: 'dlg-hint' }, hasGroup ? t('course.filename.kinds_hint') : t('course.filename.kinds_none')),
          rows.length
            ? h(
                'ul',
                { class: 'fn-kind-list' },
                rows.map(([code, kind], i) => {
                  const codeIn = h('input', {
                    class: 'input mono fn-kind-code',
                    value: code,
                    maxlength: '20',
                    spellcheck: 'false',
                    'aria-label': t('course.filename.kinds_code'),
                    dataset: { fkey: `fn:kcode:${i}` },
                    onchange: () => {
                      const v = codeIn.value.trim();
                      delete kindMap[code];
                      if (v) kindMap[v] = kind;
                      renderKinds();
                      changed();
                    },
                  });
                  const sel = h(
                    'select',
                    {
                      class: 'select fn-kind-sel',
                      'aria-label': t('course.filename.kinds_kind', { code }),
                      dataset: { fkey: `fn:kind:${i}` },
                      value: kind,
                      onchange: () => {
                        kindMap[code] = sel.value;
                        changed();
                      },
                    },
                    kinds.map((k) => h('option', { value: k.id }, kindName(k.id, 'full'))),
                  );
                  return h(
                    'li',
                    { class: 'fn-kind-row' },
                    codeIn,
                    icon('arrow-right', { size: 13, class: 'dim' }),
                    sel,
                    iconBtn('x', t('course.filename.kinds_remove', { code }), {
                      fkey: `fn:krm:${i}`,
                      onclick: () => {
                        delete kindMap[code];
                        renderKinds();
                        changed();
                      },
                    }),
                  );
                }),
              )
            : null,
          btn(t('course.filename.kinds_add'), {
            size: 'sm',
            kind: 'ghost',
            icon: 'plus',
            fkey: 'fn:kadd',
            onclick: () => {
              // the first code the preview found without a kind, else a blank one
              const free = ((preview && preview.preview) || []).map((r) => r.kind_code).find((c) => c && !(c in kindMap));
              kindMap[free || ''] = 'lecture';
              renderKinds();
              const inputs = kindsEl.querySelectorAll('.fn-kind-code');
              const target = [...inputs].find((x) => x.value === (free || ''));
              if (target) target.focus();
              if (free) changed();
            },
          }),
        );
      }
      const table = h('div', { class: 'fn-table-wrap' });

      function render() {
        clear(body);
        const p = view && view.proposal;
        if (p && p.explanation) {
          body.append(h('div', { class: 'fn-explain' }, icon('sparkles', { size: 14 }), h('div', null, h('span', { class: 'fn-explain-k' }, t('course.filename.explanation')), h('span', { class: 'fn-explain-v', lang: 'en' }, p.explanation))));
        } else if (!p && view && view.rule) {
          body.append(h('p', { class: 'fn-saved' }, icon('check', { size: 13 }), t(view.rule.source === 'user' ? 'course.filename.saved_user' : 'course.filename.saved_llm', { when: U.relTime(view.rule.created_at) })));
        } else if (!p) {
          body.append(h('p', { class: 'fn-saved is-none' }, icon('info', { size: 13 }), t('course.filename.none')));
        }
        regexInput.value = regex;
        body.append(
          h('label', { class: 'dlg-field fn-field' }, h('span', { class: 'dlg-label' }, t('course.filename.regex')), regexInput, h('span', { class: 'dlg-hint' }, t('course.filename.regex_hint'))),
          kindsEl,
          status,
          table,
        );
        renderKinds();
        renderPreview();
        renderFoot();
      }

      function renderPreview() {
        clear(status);
        clear(table);
        if (previewErr) {
          status.append(h('span', { class: 'tone-err' }, icon('circle-alert', { size: 13 }), previewErr));
          return;
        }
        if (!preview) {
          status.append(h('span', { class: 'dim' }, t('course.filename.no_preview')));
          return;
        }
        const rows = preview.preview || [];
        append(status, [
          h('span', { class: 'fn-count' }, t('course.filename.matched', { matched: preview.matched, total: preview.total })),
          preview.ok ? h('span', { class: 'chip chip-ok' }, icon('check', { size: 11, stroke: 2.6 }), t('course.filename.valid')) : h('span', { class: 'chip chip-warn' }, t('course.filename.invalid')),
          ctl ? h('span', { class: 'dim small' }, icon('loader-circle', { size: 12, class: 'spin' }), t('course.filename.refreshing')) : null,
        ]);
        const problems = problemTexts(preview);
        if (problems.length) {
          status.append(h('details', { class: 'fn-problems' }, h('summary', null, t('course.filename.problems', { n: problems.length })), h('ul', null, problems.map((x) => h('li', null, x)))));
        }
        table.append(
          h(
            'table',
            { class: 'fn-table' },
            h('thead', null, h('tr', null, h('th', null, t('course.filename.col.file')), h('th', null, t('course.filename.col.time')), h('th', null, t('course.filename.col.kind')), h('th', null, t('course.filename.col.title')), h('th', null, t('course.filename.col.status')))),
            h(
              'tbody',
              null,
              rows.map((r) =>
                h(
                  'tr',
                  { class: `is-${r.status}` },
                  h('td', { class: 'mono fn-file', title: r.filename }, r.filename),
                  h('td', { class: 'fn-time', title: fmtParsedHint(r) }, fmtParsed(r) || h('span', { class: 'dim' }, '—'), r.recorded_at && !r.has_time ? h('span', { class: 'fn-dateonly' }, t('course.filename.date_only')) : null),
                  h('td', { class: 'fn-kind-td' }, kindCell(r)),
                  h('td', { class: 'fn-title', title: r.title || '' }, r.title || h('span', { class: 'dim' }, '—')),
                  h(
                    'td',
                    { class: 'fn-st' },
                    h('span', { class: `chip chip-${STATUS_TONE[r.status] || 'quiet'}`, title: rowReason(r) }, t(`course.filename.status.${r.status}`)),
                    // why the date is unusable (an unmatched row needs no
                    // detail: the status says it)
                    r.status !== 'unmatched' && rowReason(r) ? h('span', { class: 'fn-err', title: rowReason(r) }, rowReason(r)) : null,
                  ),
                ),
              ),
            ),
          ),
        );
      }

      function schedulePreview() {
        clearTimeout(timer);
        timer = setTimeout(runPreview, 350);
      }

      async function runPreview() {
        if (ctl) ctl.abort();
        const mine = new AbortController();
        ctl = mine;
        renderPreview();
        try {
          const r = await api.previewFilenameRule(course.id, regex, kindMap, { quiet: true, signal: mine.signal });
          if (ctl !== mine) return;
          preview = r;
          previewErr = '';
        } catch (e) {
          if (mine.signal.aborted || ctl !== mine) return;
          previewErr = e.message;
        }
        ctl = null;
        renderPreview();
        renderFoot();
      }

      function renderFoot() {
        clear(foot);
        const p = view && view.proposal;
        const same = view && view.rule && !p && regex.trim() === view.rule.regex.trim() && mapKey(kindMap) === mapKey(view.kind_map);
        const regen = btn(t('course.filename.regenerate'), {
          kind: 'ghost',
          icon: 'refresh-cw',
          class: 'fn-regen',
          fkey: 'fn:regen',
          onclick: async () => {
            regen.disabled = true;
            try {
              await busy(regenerateOrThrow(course));
            } catch (e) {
              regen.disabled = false;
              err.textContent = e.message;
              return;
            }
            close('regenerate');
          },
        });
        foot.append(regen);
        if (p) foot.append(btn(t('course.filename.ignore'), { fkey: 'fn:ignore', onclick: ignore }));
        foot.append(h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')));
        const canApply = !!regex.trim() && !!preview && preview.matched > 0 && !previewErr && !same;
        foot.append(btn(edited ? t('course.filename.apply_edited') : t('course.filename.apply'), { kind: 'primary', icon: 'check', disabled: !canApply || !!ctl, fkey: 'fn:apply', title: same ? t('course.filename.unchanged') : '', onclick: apply }));
      }

      async function apply() {
        err.textContent = '';
        try {
          // busy (here and in ignore): a route change while it runs lets it
          // finish (R62 N4)
          const c = await busy(withRenumber((confirm) => api.applyFilenameRule(course.id, regex.trim(), kindMap, confirm, { quiet: true })));
          if (!c) return;
          toast(t('course.filename.applied'), 'success');
          close(c);
        } catch (e) {
          err.textContent = e.message;
        }
      }

      async function ignore() {
        err.textContent = '';
        try {
          await busy(api.dismissFilenameProposal(course.id, { quiet: true }));
          toast(t('course.filename.ignored'), 'info');
          close('ignored');
        } catch (e) {
          err.textContent = e.message;
        }
      }

      load();
      return box;
    },
    { wide: true, className: 'dlg-fn' },
  ).then((res) => {
    if (res && opts.onChanged) opts.onChanged(typeof res === 'object' ? res : null);
    return res;
  });
}

/**
 * The "recording times" section of the course settings dialog: the saved rule, its
 * buttons, and whether the organizer role can generate one.
 * opts: {onOpenRule(), onChanged(course), close, busy (of the settings dialog)}
 */
export function recordingSection(course, opts = {}) {
  const rule = course.filename_rule;
  const note = h('p', { class: 'cset-note', role: 'status' });
  const why = h('p', { class: 'dlg-field-error', role: 'alert' });
  const regen = btn(rule ? t('course.filename.regenerate') : t('course.filename.generate'), {
    size: 'sm',
    icon: 'refresh-cw',
    fkey: 'cset:fn-regen',
    onclick: async () => {
      regen.disabled = true;
      // busy (the settings dialog's): as in the rule's own dialog
      const sending = regenerateOrThrow(course);
      why.textContent = await (opts.busy ? opts.busy(sending) : sending).then(
        () => '',
        (e) => e.message,
      );
      regen.disabled = false;
      if (!why.textContent && opts.onChanged) opts.onChanged(null);
    },
  });
  const el = h(
    'div',
    { class: 'cset-rec' },
    h('p', { class: 'cset-intro' }, t('course.filename.section_intro')),
    rule
      ? h(
          'div',
          { class: 'cset-rule' },
          h('code', { class: 'cset-regex', title: rule.regex }, rule.regex),
          h('span', { class: 'muted small' }, t(rule.source === 'user' ? 'course.filename.rule_user' : 'course.filename.rule_llm'), ' · ', t('course.filename.matched', { matched: rule.samples_matched, total: rule.samples_total })),
        )
      : h('p', { class: 'muted small' }, t('course.filename.none')),
    course.filename_proposal ? h('p', { class: 'cset-pending' }, icon('sparkles', { size: 13 }), t('course.filename.proposal')) : null,
    h('div', { class: 'cset-rule-actions' }, rule || course.filename_proposal ? btn(course.filename_proposal ? t('course.filename.view_result') : t('course.filename.view_edit'), { size: 'sm', icon: 'pencil', fkey: 'cset:fn-open', onclick: opts.onOpenRule }) : btn(t('course.filename.write'), { size: 'sm', kind: 'ghost', icon: 'pencil', fkey: 'cset:fn-open', onclick: opts.onOpenRule }), regen),
    why,
    note,
  );
  // Whether the organizer role is usable (it writes the rule, P13.3.2).
  api.settings().then(
    (s) => {
      const r = s && (s.roles || []).find((x) => x.role === 'organizer');
      if (r && !r.configured) {
        note.classList.add('is-warn');
        // the link leaves the page: the dialog closes first (it is modal)
        note.append(icon('triangle-alert', { size: 13 }), t('course.filename.organizer_missing'), ' ', h('a', { href: '#/settings', class: 'set-link', onclick: () => opts.close && opts.close(null) }, t('course.filename.to_settings')));
      }
    },
    () => {},
  );
  return el;
}
