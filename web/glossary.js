// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The course page's glossary card (SPEC §9.3; SPEC-v2 G1 tiers and locks,
// G3.5 the janitor's header and report, G6 rule 3 where a translation came
// from). Terms are grouped by tier — enforced, reference, disabled — and
// move between the groups by drag and drop or with the A12.2 term menu;
// every move is a PATCH whose response is what the page shows (nothing
// optimistic). The header shows the latest janitor run and the "tidy up"
// button; the report dialog lists every change with its own undo button.

import { api } from './api.js';
import { h, append, clear, toast, confirmDialog, preserveFocus, btn, skeleton, errorBlock, openDialog, iconBtn } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar } from './pipeline.js';
import { menuButton, bindContextMenu } from './menu.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { zh as zhText, zhLang, zhOn, zhQuery } from './zh_display.js';
import { zhEditNote } from './widgets.js';

// Texts are looked up when shown (the UI language can change at run time).
const GROUPS = [
  { key: 'enforce', icon: 'shield-check' },
  { key: 'reference', icon: 'book-open' },
  { key: 'off', icon: 'ban' },
];

// "Enforced" / "Reference" / "Disabled": the name of a group.
function groupName(g) {
  return g === 'off' ? t('glossary.group.off') : U.TIER_NAMES[g];
}

const tierName = (tier) => U.TIER_NAMES[tier] || U.TIER_NAMES.enforce;

const DRAG_TYPE = 'application/x-lectureflow-term';
const COLLAPSE_KEY = 'lectureflow.glossary.collapsed';
// Typing in the filter applies it once the keys pause this long (R7-PF1).
const FILTER_DELAY_MS = 150;

// t() text whose {name} placeholders (not params) become the given nodes,
// e.g. keyboard keys inside a sentence.
function tNodes(key, nodes) {
  return t(key)
    .split(/(\{\w+\})/)
    .filter(Boolean)
    .map((p) => {
      const m = /^\{(\w+)\}$/.exec(p);
      return m && nodes[m[1]] ? nodes[m[1]] : p;
    });
}

// The group a term is shown in: disabled terms form their own group
// whatever their tier.
export function groupOf(term) {
  if (!term.enabled) return 'off';
  return term.tier === 'reference' ? 'reference' : 'enforce';
}

// The TermPatch that moves term into group g; null when it is there already.
export function movePatch(term, g) {
  if (groupOf(term) === g) return null;
  if (g === 'off') return { enabled: false };
  const p = { tier: g };
  if (!term.enabled) p.enabled = true;
  return p;
}

function readCollapsed() {
  try {
    const v = JSON.parse(localStorage.getItem(COLLAPSE_KEY) || '[]');
    return new Set(Array.isArray(v) ? v : []);
  } catch {
    return new Set();
  }
}

function saveCollapsed(set) {
  try {
    localStorage.setItem(COLLAPSE_KEY, JSON.stringify([...set]));
  } catch {
    // a per-viewer convenience only
  }
}

// Group and translation of a TermState of the report ("disabled" and
// "(keep English)" included).
function stateParts(s) {
  // the rendering is lecture content: shown in the display script (P14.6)
  return { group: groupName(s.enabled ? (s.tier === 'reference' ? 'reference' : 'enforce') : 'off'), zh: s.zh ? zhText(s.zh) : t('glossary.keep_english'), lang: s.zh ? zhLang() : undefined };
}

let mkSeq = 0;
let mkHovered = null; // the marker under the pointer (Esc closes its card)
let mkEscapeWatched = false;

// A small marker with a hover / focus card (position: fixed, so the
// scrolling table never clips it). body: nodes; action: {label, icon,
// onclick, fkey}; title: the card's name when body has no .mk-title line.
// The card may hold a button, so it is no tooltip (C2, R19 §5; as the
// stage cards, L16): the marker controls it and says whether it is open,
// the card is a named group. The marker's label already says all the
// card's text, so the marker is not described by the card (it would be
// heard twice, the button's label included). Esc closes the card (under
// the pointer too, WCAG 1.4.13) and leaves the focus on the marker; Enter
// or Space on the marker opens it again.
export function markCard({ ic, cls, label, title, body, action, fkey }) {
  const id = `mk-${++mkSeq}`;
  const inner = h('div', { class: 'mk-card-body' }, body, action ? h('div', { class: 'mk-card-actions' }, btn(action.label, { size: 'sm', icon: action.icon, onclick: action.onclick, fkey: action.fkey })) : null);
  // named by its title line (R58: an aria-label of the same words was
  // heard twice in browse mode); by title when the body has no such line
  const head = inner.querySelector('.mk-title');
  if (head) head.setAttribute('id', `${id}-t`);
  const card = h('div', { class: 'mk-card', id, role: 'group', 'aria-labelledby': head ? `${id}-t` : null, 'aria-label': head ? null : title }, inner);
  const b = h('button', { type: 'button', class: `mk ${cls}`, 'aria-label': label, 'aria-controls': id, 'aria-expanded': 'false', dataset: { fkey } }, icon(ic, { size: 12, stroke: 2.2 }));
  const wrap = h('span', { class: 'mk-wrap' }, b, card);
  const place = () => {
    const r = b.getBoundingClientRect();
    const w = 280;
    card.style.left = `${Math.max(8, Math.min(r.left - 10, window.innerWidth - w - 12))}px`;
    const below = r.bottom + 160 < window.innerHeight;
    card.classList.toggle('mk-up', !below);
    card.style.top = below ? `${r.bottom}px` : '';
    card.style.bottom = below ? '' : `${window.innerHeight - r.top}px`;
  };
  let hovered = false;
  let focused = false;
  const isOpen = () => (hovered || focused) && !wrap.classList.contains('is-dismissed');
  const sync = () => b.setAttribute('aria-expanded', String(isOpen()));
  const closed = () => {
    if (!hovered && !focused) wrap.classList.remove('is-dismissed'); // the next hover or focus opens it again
    sync();
  };
  wrap.dismiss = () => {
    if (!isOpen()) return false;
    const a = document.activeElement;
    if (a && a !== b && wrap.contains(a)) b.focus({ preventScroll: true });
    wrap.classList.add('is-dismissed');
    sync();
    return true;
  };
  wrap.addEventListener('pointerenter', () => {
    hovered = true;
    mkHovered = wrap;
    watchMkEscape();
    wrap.classList.remove('is-dismissed');
    place();
    sync();
  });
  wrap.addEventListener('pointerleave', () => {
    hovered = false;
    if (mkHovered === wrap) mkHovered = null;
    closed();
  });
  wrap.addEventListener('focusin', () => {
    if (!focused) wrap.classList.remove('is-dismissed'); // the focus came from outside
    focused = true;
    place();
    sync();
  });
  wrap.addEventListener('focusout', (e) => {
    focused = !!(e.relatedTarget && wrap.contains(e.relatedTarget));
    closed();
  });
  wrap.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && wrap.dismiss()) e.stopPropagation();
  });
  b.addEventListener('click', () => {
    if (!wrap.classList.contains('is-dismissed')) return;
    wrap.classList.remove('is-dismissed');
    place();
    sync();
  });
  wrap._place = place;
  // Its row taken off the page and put back (renderTable reuses rows): no
  // pointerleave came, nothing is under the pointer or focused any more
  // (R71 L1).
  wrap._reset = () => {
    hovered = false;
    focused = false;
    if (mkHovered === wrap) mkHovered = null;
    wrap.classList.remove('is-dismissed');
    sync();
  };
  return wrap;
}

// Esc while the pointer rests on a marker (the focus elsewhere) closes its
// card; one listener, added with the first hover.
function watchMkEscape() {
  if (mkEscapeWatched) return;
  mkEscapeWatched = true;
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || e.defaultPrevented || !mkHovered) return;
    if (!mkHovered.isConnected) {
      mkHovered = null;
      return;
    }
    if (mkHovered.contains(document.activeElement)) return; // the marker's own keydown has it
    if (mkHovered.dismiss()) e.stopPropagation();
  });
}

// createGlossary({courseId, ctx, onCourse(course)}) → {el, setCourse, reload, onJob, destroy}.
// onCourse receives a Course an action returned (the page renders it).
export function createGlossary({ courseId, ctx, onCourse, reloadCourse }) {
  let alive = true;
  let course = null;
  let terms = null; // null until loaded
  let termsError = '';
  let editingTerm = null; // term id
  let termDraft = '';
  let termFocusPending = false;
  let termFocusAfter = null; // fkey to focus once a deleted term's row is gone
  let termAdded = null; // WV-05: the id of a term just added, scrolled to and highlighted once drawn
  let termFilter = ''; // the filter applied (the input's text once typing pauses)
  let filterTimer = 0;
  let termSource = '';
  let dragId = null;
  let reviewing = false; // POST glossary/review in flight
  const moving = new Set(); // term ids with a PATCH in flight
  const collapsed = readCollapsed();
  let reviewBar = null;
  let reviewChip = null; // the "tidying 42%" chip while the janitor runs
  // the table as drawn (renderTable, R7-PF1)
  const rowCache = new Map(); // term id → {key, row}
  let shell = null; // the table on the page: {table, groups: {key → {body, head, toggle, count, empty}}}; null while flat
  let flatKey = null; // what the card shows in place of the table (flatState); null while the table is shown
  let footKey = null; // what the foot says ('' = nothing)
  // renderTable is changing the page: a blur it causes is not the user
  // leaving an editor (Chromium sends a focused element its blur while
  // taking it off the page, still connected; R71 M1)
  let drawing = 0;

  // ---- skeleton (inputs are built once and never re-rendered) ----
  const termsHead = h('h2', { class: 'card-title' });
  const janEl = h('div', { class: 'jan-bar', role: 'region', 'aria-label': t('glossary.janitor.title') });
  const tableWrap = h('div', { class: 'terms-table-wrap' });
  const termsFoot = h('p', { class: 'terms-foot' });
  const enInput = h('input', { class: 'input', placeholder: t('glossary.add.en'), 'aria-label': t('glossary.add.en'), required: true, autocomplete: 'off', spellcheck: 'false' });
  // P14.6: in a Traditional display the rendering is still saved as typed (Simplified expected)
  const zhInput = h('input', { class: 'input', placeholder: t('glossary.add.zh_placeholder'), 'aria-label': t('glossary.add.zh_label'), 'aria-describedby': zhOn() ? 'term-add-zh-note' : undefined, autocomplete: 'off' });
  const addForm = h('form', { class: 'term-add', onsubmit: onAddTerm }, enInput, zhInput, h('button', { type: 'submit', class: 'btn btn-sm' }, icon('plus', { size: 14 }), t('glossary.add.submit')));
  const filterInput = h('input', {
    class: 'input term-search',
    type: 'search',
    placeholder: t('glossary.filter.placeholder'),
    'aria-label': t('glossary.filter.label'),
    oninput: () => {
      clearTimeout(filterTimer);
      filterTimer = setTimeout(() => {
        filterTimer = 0;
        if (!alive) return;
        termFilter = filterInput.value;
        renderTable();
      }, FILTER_DELAY_MS);
    },
  });
  const sourceSel = h(
    'select',
    {
      class: 'select term-source',
      'aria-label': t('glossary.filter.source_label'),
      onchange: () => {
        termSource = sourceSel.value;
        renderTable();
      },
    },
    h('option', { value: '' }, t('glossary.filter.all_sources')),
    Object.entries(U.SOURCE_NAMES).map(([k, v]) => h('option', { value: k }, v)),
  );
  const el = h(
    'section',
    { class: 'card card-terms', 'aria-label': t('glossary.title'), id: 'course-terms' },
    h('header', { class: 'card-hd' }, termsHead, h('div', { class: 'card-tools term-filters' }, filterInput, sourceSel)),
    h(
      'div',
      { class: 'card-bd' },
      janEl,
      h(
        'div',
        { class: 'term-tools' },
        zhOn() ? h('div', { class: 'zh-edit' }, addForm, zhEditNote('term-add-zh-note')) : addForm,
        h('p', { class: 'res-hint term-note', text: t('glossary.note') }),
      ),
      tableWrap,
      termsFoot,
    ),
  );

  bindContextMenu(tableWrap, (e) => {
    const term = termOf(e.target);
    return term ? termMenuItems(term) : null;
  });
  // Keys on a term row (anywhere but the translation input): F2 edits the
  // translation, Delete deletes; ↑ / ↓ move between rows (across the
  // groups) on the translation button (↓ on ⋯ opens its menu).
  tableWrap.addEventListener('keydown', (e) => {
    if (e.defaultPrevented || e.target.closest('input, select, textarea')) return;
    const row = e.target.closest('tr[data-term]');
    const term = termOf(e.target);
    if (!term || e.altKey || e.ctrlKey || e.metaKey) return;
    if (e.key === 'F2') {
      e.preventDefault();
      startEditTerm(term);
    } else if (e.key === 'Delete') {
      e.preventDefault();
      deleteTerm(term);
    } else if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && e.target.classList.contains('zh-btn')) {
      const rows = [...tableWrap.querySelectorAll('tr[data-term]')];
      const next = rows[rows.indexOf(row) + (e.key === 'ArrowDown' ? 1 : -1)];
      const target = next && next.querySelector('.zh-btn');
      if (target) {
        e.preventDefault();
        target.focus();
      }
    }
  });
  // the fixed-position marker cards follow the table when it scrolls
  tableWrap.addEventListener(
    'scroll',
    () => {
      for (const w of tableWrap.querySelectorAll('.mk-wrap:hover, .mk-wrap:focus-within')) if (w._place) w._place();
    },
    { passive: true },
  );

  function termOf(target) {
    const row = target.closest && target.closest('tr[data-term]');
    return (row && terms && terms.find((x) => x.id === row.dataset.term)) || null;
  }

  // ---- loading ----

  // Until the glossary has loaded once, a failure is shown in its card (with
  // a retry button) instead of a toast; later failures keep the list shown
  // and are toasted by the API layer. The same list as shown (the server's
  // "changed terms" after the user's own change, already in place) draws
  // nothing (R7-PF1).
  async function reload() {
    // the term just added when this read began: a read begun before the
    // adding may not list it yet, and leaves it to the adding's own read
    const added = termAdded;
    let next;
    try {
      const ts = await api.terms(courseId, { quiet: !terms });
      if (!alive) return;
      next = ts || [];
    } catch (e) {
      // not drawn now: not highlighted by a later draw either; a read begun
      // before the adding leaves it to the adding's own read (R93)
      if (termAdded === added) termAdded = null;
      if (!alive || terms) return;
      termsError = e.message || t('common.load_failed');
      renderTable();
      return;
    }
    // R91 low-4: not in a read begun after its adding (not there, or gone
    // again): not highlighted by a much later draw that has it either
    if (added && termAdded === added && !next.some((x) => x.id === added)) termAdded = null;
    if (terms && !termFocusAfter && !termAdded && JSON.stringify(next) === JSON.stringify(terms)) return;
    terms = next;
    termsError = '';
    renderTable();
  }

  function retryTerms() {
    termsError = '';
    renderTable(); // skeleton while it loads again
    reload();
  }

  function setCourse(c) {
    course = c;
    renderHead();
    renderJanitor();
    // the table depends on the course only while it is empty ("extracting…")
    if (terms && !terms.length) renderTable();
  }

  function replaceTerm(updated) {
    if (!terms || !updated) return;
    const i = terms.findIndex((x) => x.id === updated.id);
    if (i >= 0) terms[i] = updated;
  }

  // ---- header ----

  function renderHead() {
    if (!course) return;
    clear(termsHead);
    append(termsHead, [
      icon('hash', { size: 16 }),
      h('span', null, t('glossary.title')),
      h('span', { class: 'card-count' }, String(course.term_count)),
      U.extractionBusy(course) ? h('span', { class: 'chip chip-run' }, icon('loader-circle', { size: 11, class: 'spin' }), t('glossary.extracting')) : null,
      h('span', { class: 'card-sub', title: t('glossary.version_tip') }, t('glossary.version', { version: course.glossary_version })),
    ]);
  }

  // ---- janitor header (G3.5) ----

  function renderJanitor() {
    if (!course) return;
    const j = course.janitor || {};
    const st = U.reviewStage(course);
    const busy = U.reviewBusy(course);
    let status = null;
    if (st && st.status === 'running') status = h('span', { class: 'chip chip-run' }, icon('loader-circle', { size: 11, class: 'spin' }), t('glossary.janitor.running', { pct: U.pct(st.progress) }));
    else if (st && (st.status === 'queued' || st.status === 'pending')) status = h('span', { class: 'chip chip-quiet' }, st.waiting_for ? U.stageWaitingText(st, ctx.system(), ctx.resources()) : t('glossary.janitor.waiting'));
    else if (st && st.status === 'failed') status = h('span', { class: 'chip chip-warn' }, t('glossary.janitor.last_failed'));
    else if (st && st.status === 'cancelled') status = h('span', { class: 'chip chip-quiet' }, t('glossary.janitor.last_cancelled'));
    reviewChip = st && st.status === 'running' ? status : null;

    const meta = [];
    if (j.last_at) {
      meta.push(
        h('span', { title: U.fmtDateTime(j.last_at) }, t('glossary.janitor.last_at', { when: U.relTime(j.last_at) })),
        // a fallback report has no model: the common-word rule did it
        h('span', { class: 'jan-model' + (j.last_fallback ? ' is-rule' : '') }, j.last_fallback ? t('glossary.janitor.rule') : j.last_model || '—'),
        j.last_version ? h('span', { class: 'mono dim', title: t('glossary.janitor.version_tip') }, t('glossary.version', { version: j.last_version })) : null,
      );
    } else {
      meta.push(h('span', null, t('glossary.janitor.never')));
    }
    const notes = [];
    if (!j.configured) {
      notes.push(
        h(
          'p',
          { class: 'jan-note' },
          icon('info', { size: 13 }),
          h('span', null, t('glossary.janitor.not_configured')),
          h('a', { class: 'set-link', href: '#/settings' }, t('course.filename.to_settings')),
        ),
      );
    }
    const failure = st && st.status === 'failed' && st.error ? U.stageErrorText(st) : '';
    if (failure) {
      notes.push(h('p', { class: 'jan-note is-warn' }, icon('triangle-alert', { size: 13 }), h('span', null, t('glossary.janitor.failed_note', { error: U.excerpt(failure, 200) }))));
    }
    reviewBar = st ? pipelineBar([st], { key: `jan:${courseId}`, sys: ctx.system, res: ctx.resources, onAction: reviewAction, label: t('glossary.janitor.progress_label'), pct: false }) : null;
    const reviewBtn = btn(busy ? t('glossary.janitor.busy') : t('glossary.janitor.review'), {
      size: 'sm',
      kind: busy ? undefined : 'primary',
      icon: busy || reviewing ? 'loader-circle' : 'wand-sparkles',
      disabled: busy || reviewing,
      title: busy ? t('glossary.janitor.busy_tip') : j.configured ? t('glossary.janitor.review_tip') : t('glossary.janitor.no_janitor_tip'),
      fkey: 'jan:review',
      onclick: review,
    });
    if (busy || reviewing) reviewBtn.querySelector('.icon').classList.add('spin');
    preserveFocus(() => {
      clear(janEl);
      janEl.append(
        h('div', { class: 'jan-icon' }, icon('wand-sparkles', { size: 16 })),
        h(
          'div',
          { class: 'jan-main' },
          h('div', { class: 'jan-title' }, h('strong', null, t('glossary.janitor.title')), status),
          h('div', { class: 'jan-meta' }, meta),
          notes,
        ),
        h(
          'div',
          { class: 'jan-actions' },
          reviewBar ? h('div', { class: 'jan-pipe' }, reviewBar) : null,
          j.last_at
            ? btn(t('glossary.janitor.report'), {
                size: 'sm',
                icon: 'list-checks',
                fkey: 'jan:report',
                // last_changes: the not yet undone changes of the latest report that changed something
                count: j.last_changes || null,
                title: j.last_changes ? t('glossary.janitor.report_tip', { n: j.last_changes }) : t('glossary.janitor.report_tip_none'),
                onclick: () => openReport(),
              })
            : null,
          reviewBtn,
        ),
      );
    });
  }

  async function reviewAction(stage, action) {
    const updated = await api.courseStageAction(courseId, stage, action);
    if (alive && updated) onCourse(updated);
  }

  async function review() {
    if (reviewing || U.reviewBusy(course)) return;
    reviewing = true;
    renderJanitor();
    try {
      const res = await api.reviewGlossary(courseId);
      if (!alive || !res) return;
      if (res.queued) {
        toast(t('glossary.janitor.started'), 'info');
      } else if (res.report) {
        const n = (res.report.items || []).length;
        reload();
        if (n) {
          toast(t('glossary.janitor.fallback_done', { n }), 'success');
          openReport(res.report);
        } else {
          toast(t('glossary.janitor.fallback_none'), 'success');
        }
      }
    } catch {
      // shown by the API layer
    } finally {
      reviewing = false;
      if (alive) {
        renderJanitor();
        reloadCourse();
      }
    }
  }

  // ---- report dialog (G3.5) ----

  // The dialog shows the latest run (time, model, number of changes, the
  // rejected suggestions; GET .../glossary/report?run=last) and the change
  // list of the latest report that changed something (GET .../glossary/report),
  // whose items can be undone. initial: a report the caller already has (the
  // answer of a fallback run), shown as the latest run while both load.
  function openReport(initial = null) {
    let run = initial;
    let report = initial && (initial.items || []).length ? initial : null;
    let loaded = false;
    let error = '';
    const undoing = new Set();
    let focusAfter = null;
    openDialog(
      t('glossary.report.title'),
      (close, busy) => {
        const body = h('div', { class: 'jr' });
        const load = async () => {
          error = '';
          render();
          const opt = { quiet: true };
          const [r1, r2] = await Promise.allSettled([api.glossaryLastRun(courseId, opt), api.glossaryReport(courseId, null, opt)]);
          // 404: no run / no report with changes yet; anything else is an error
          const missing = (r) => r.status === 'rejected' && r.reason && r.reason.status === 404;
          if (r1.status === 'fulfilled') run = r1.value;
          else if (!missing(r1)) error = r1.reason.message || t('common.load_failed');
          if (r2.status === 'fulfilled') report = r2.value;
          else if (missing(r2)) report = null;
          else error = r2.reason.message || t('common.load_failed');
          loaded = true;
          render();
        };
        const undo = async (it) => {
          if (undoing.has(it.term_id) || !report) return;
          undoing.add(it.term_id);
          render();
          try {
            // busy: a route change while it runs lets it finish (R62 N4)
            const r = await busy(api.undoJanitorItem(courseId, report.version, it.term_id));
            if (r) report = r;
            toast(t('glossary.report.undone_toast', { en: it.en }), 'success');
            const rest = report.items.filter((x) => !x.undone && x.term_id !== it.term_id);
            focusAfter = rest.length ? `jr:undo:${rest[0].term_id}` : 'jr:close';
            if (alive) {
              reload();
              reloadCourse();
            }
          } catch (e) {
            if (e && e.status === 409) {
              try {
                report = await api.glossaryReport(courseId, report.version, { quiet: true });
              } catch {
                // keep what is shown
              }
            }
          } finally {
            undoing.delete(it.term_id);
            render();
          }
        };
        const render = () => {
          preserveFocus(() => {
            clear(body);
            if (error) {
              body.append(errorBlock(error, load));
              return;
            }
            if (!loaded && !run) {
              body.append(skeleton(5));
              return;
            }
            append(body, reportView(run, report, { undo, undoing }));
          });
          if (focusAfter) {
            const want = focusAfter;
            focusAfter = null;
            const target = body.parentNode && body.parentNode.querySelector(`[data-fkey="${CSS.escape(want)}"]`);
            if (target) target.focus();
          }
        };
        render();
        load();
        return h('div', null, body, h('div', { class: 'dlg-actions' }, btn(t('common.close'), { onclick: () => close(null), fkey: 'jr:close' })));
      },
      { wide: true, className: 'dlg-report' },
    );
  }

  const runModel = (r) => (r.fallback ? t('glossary.janitor.rule') : [r.provider_name, r.model].filter(Boolean).join(' / ') || '—');
  const sameRun = (a, b) => !!a && !!b && a.version === b.version && a.created_at === b.created_at;

  function termEn(id) {
    const term = terms && terms.find((x) => x.id === id);
    return term ? term.en : '';
  }

  function reportView(run, rep, { undo, undoing }) {
    const parts = [];
    if (run) {
      const n = (run.items || []).length;
      parts.push(
        h(
          'div',
          { class: 'jr-head' },
          h('span', { class: 'jr-lead' }, t('glossary.report.latest')),
          h('span', null, U.fmtDateTime(run.created_at)),
          h('span', { class: 'jan-model' + (run.fallback ? ' is-rule' : '') }, runModel(run)),
          h('span', null, n ? t('glossary.report.changes', { n }) : t('glossary.report.no_changes')),
          h('span', { class: 'jr-ver mono' }, n ? t('glossary.version_change', { from: run.from_version, to: run.version }) : t('glossary.version', { version: run.version })),
        ),
      );
      const note = U.codedText('notes', run.note_info, run.note);
      if (note) parts.push(h('p', { class: 'jr-note' }, icon('info', { size: 13 }), h('span', null, note)));
    }
    const items = (rep && rep.items) || [];
    if (!rep || !items.length) {
      parts.push(h('div', { class: 'terms-empty' }, icon('check', { size: 20 }), h('strong', null, t('glossary.report.empty'))));
    } else {
      const undone = items.filter((x) => x.undone).length;
      if (!sameRun(run, rep)) {
        // the list comes from an earlier run than the latest one
        parts.push(
          h(
            'div',
            { class: 'jr-head jr-sub' },
            h('span', { class: 'jr-lead' }, t('glossary.report.latest_with_changes')),
            h('span', null, U.fmtDateTime(rep.created_at)),
            h('span', { class: 'jan-model' + (rep.fallback ? ' is-rule' : '') }, runModel(rep)),
            h('span', { class: 'jr-ver mono' }, t('glossary.version_change', { from: rep.from_version, to: rep.version })),
          ),
        );
      }
      parts.push(
        h('p', { class: 'jr-hint' }, undone ? t('glossary.report.hint_undone', { n: items.length, undone }) : t('glossary.report.hint', { n: items.length })),
        h(
          'div',
          { class: 'jr-table-wrap' },
          h(
            'table',
            { class: 'jr-table' },
            h(
              'thead',
              null,
              h(
                'tr',
                null,
                h('th', null, t('glossary.report.col.term')),
                h('th', null, t('glossary.report.col.before')),
                h('th', null, t('glossary.report.col.after')),
                h('th', null, t('glossary.report.col.reason')),
                h('th', null, h('span', { class: 'visually-hidden' }, t('glossary.col.actions'))),
              ),
            ),
            h('tbody', null, items.map((it) => itemRow(it, { undo, undoing }))),
          ),
        ),
      );
    }
    const rejected = (run && run.rejected) || [];
    if (rejected.length) {
      parts.push(
        h(
          'details',
          { class: 'jr-rejected' },
          h('summary', null, t('glossary.report.rejected', { n: rejected.length })),
          h('p', { class: 'jr-hint' }, t('glossary.report.rejected_hint')),
          h(
            'ul',
            { class: 'jr-rej-list' },
            rejected.map((x) =>
              h(
                'li',
                { class: 'jr-rej' },
                h('div', { class: 'jr-rej-head' }, h('span', { class: 'term-en' }, termEn(x.term_id) || t('glossary.report.unknown_term', { id: x.term_id })), h('span', { class: 'chip chip-quiet' }, U.JANITOR_ACTION_NAMES[x.action] || x.action)),
                x.reason ? h('div', { class: 'jr-rej-reason' }, h('span', { class: 'jr-k' }, t('glossary.report.col.reason')), h('span', { lang: 'en' }, x.reason)) : null,
                h('div', { class: 'jr-rej-why' }, h('span', { class: 'jr-k' }, t('glossary.report.why')), h('span', null, U.codedText('notes', { code: x.why_code }, x.why) || '—')),
              ),
            ),
          ),
        ),
      );
    }
    return parts;
  }

  // "Merge into algorithm"; a fix_zh made by a merge (into = the merged
  // term) reads "Filled in from the merged algorithms" and is undone with
  // that merge.
  function actionText(it, action) {
    if (it.action === 'merge' && it.into_en) return t('glossary.report.merge_into', { into: it.into_en });
    if (it.action === 'fix_zh' && it.into) return t('glossary.report.fill_from_merge', { from: it.into_en || termEn(it.into) || it.into });
    return action;
  }

  function itemRow(it, { undo, undoing }) {
    const b = stateParts(it.before);
    const a = stateParts(it.after);
    const cell = (s, other, cls) =>
      h('td', { class: cls }, h('span', { class: 'jr-group' + (s.group !== other.group ? ' is-changed' : '') }, s.group), h('span', { class: 'jr-zh' + (s.zh !== other.zh ? ' is-changed' : ''), lang: s.lang }, s.zh));
    const action = U.JANITOR_ACTION_NAMES[it.action] || it.action;
    return h(
      'tr',
      { class: it.undone ? 'is-undone' : '' },
      h(
        'td',
        { class: 'jr-term' },
        h('span', { class: 'term-en' }, it.en),
        h('span', { class: 'jr-action' }, actionText(it, action)),
      ),
      cell(b, a, 'jr-before'),
      cell(a, b, 'jr-after'),
      h('td', { class: 'jr-reason', lang: 'en' }, it.reason || '—'),
      h(
        'td',
        { class: 'jr-op' },
        it.undone
          ? h('span', { class: 'jr-undone', title: it.undone_at ? t('glossary.report.undone_at', { when: U.fmtDateTime(it.undone_at) }) : '' }, icon('undo-2', { size: 13 }), t('glossary.report.undone'))
          : btn(undoing.has(it.term_id) ? t('glossary.report.undoing') : t('glossary.report.undo'), { size: 'sm', kind: 'ghost', icon: 'undo-2', disabled: undoing.has(it.term_id), fkey: `jr:undo:${it.term_id}`, onclick: () => undo(it) }),
      ),
    );
  }

  // ---- table ----

  function shownTerms() {
    return terms.filter(filterOf());
  }

  // Whether a term passes the search and the source filter applied.
  function filterOf() {
    const q = termFilter.trim().toLowerCase();
    const zq = zhQuery(q, renderTable); // P14.6: a Traditional term finds the stored Simplified rendering
    return (term) => (!termSource || term.source === termSource) && (!q || term.en.toLowerCase().includes(q) || zq.test(term.zh));
  }

  function filtersActive() {
    return !!termFilter.trim() || !!termSource;
  }

  function clearFilters() {
    clearTimeout(filterTimer);
    filterTimer = 0;
    termFilter = '';
    termSource = '';
    filterInput.value = '';
    sourceSel.value = '';
    renderTable();
    filterInput.focus();
  }

  // The table is kept up to date rather than built anew (R7-PF1: with 500
  // terms each build took 60–90 ms, 0.4–0.8 s on a slow machine, and editing
  // one term built it four times). A term's row is built once and reused
  // while its term, its editor and its busy state stay the same (rowCache);
  // renderTable() puts in each group the rows it shows, in order, taking out
  // the rows the filter hides and putting the same rows back, and changes a
  // count, a placeholder or the foot only when its text changes. So a change
  // of one term draws its row, and a list or a course that changes nothing
  // draws nothing. The focus stays where it is; a row drawn anew under the
  // focus gets it back through preserveFocus (data-fkey), its editor with the
  // text typed and the caret where they were (rowCache, shell, flatKey,
  // footKey above).
  function renderTable() {
    const a = document.activeElement;
    const caret = a && a.classList && a.classList.contains('input-term') && tableWrap.contains(a) ? { el: a, start: a.selectionStart, end: a.selectionEnd } : null;
    drawing++;
    try {
      preserveFocus(() => {
        const shown = terms ? shownTerms() : [];
        // the term being edited is no longer shown (filtered out, its group
        // folded, deleted): the edit ends with its row, and nothing typed in
        // it is saved
        if (editingTerm && !shown.some((x) => x.id === editingTerm && !collapsed.has(groupOf(x)))) editingTerm = null;
        const flat = flatState(shown);
        if (flat) drawFlat(flat);
        else syncTable(shown);
      });
    } finally {
      drawing--;
    }
    // the editor drawn anew under the focus: the caret back where it was
    if (caret && caret.start != null) {
      const b = document.activeElement;
      if (b && b !== caret.el && b.classList && b.classList.contains('input-term') && typeof b.setSelectionRange === 'function') b.setSelectionRange(caret.start, caret.end);
    }
    if (termFocusAfter) {
      const want = termFocusAfter;
      termFocusAfter = null;
      const target = want === 'terms:add' ? enInput : tableWrap.querySelector(`[data-fkey="${CSS.escape(want)}"]`);
      if (target) target.focus();
    }
    if (termFocusPending) {
      termFocusPending = false;
      const input = tableWrap.querySelector('.input-term');
      if (input) {
        input.focus();
        input.select();
      }
    }
    // WV-05: a term just added, once the list drawn has it
    if (termAdded && terms && terms.some((x) => x.id === termAdded)) {
      const id = termAdded;
      termAdded = null;
      showAdded(id);
    }
  }

  // Round 9 WV-05 (needs-human 1.15 b, T23 general 8): the row of a term
  // just added is scrolled to within the table (.terms-table-wrap scrolls
  // by itself), into the part of it the window shows when that part can
  // hold it (R91b low-7: no page scroll then); else the page by as little
  // as it takes (R91 medium-1). "The window" ends above the toasts where
  // they are over the table (narrow windows: the success toast would cover
  // the row). The field that has the focus (the add form, for the next
  // term) stays in the window: where both cannot be had (a narrow window,
  // a tall table) it wins, the row shows as far as it can, and the toast
  // ("已添加术语：…") says the term was added. The row is briefly
  // highlighted (.term-row.flash; nothing where it is not styled).
  const FLASH_MS = 2000;
  const REVEAL_GAP = 8; // px kept between the row or the field and an edge
  function showAdded(id) {
    const row = tableWrap.querySelector(`tr[data-term="${CSS.escape(id)}"]`);
    if (!row || !row.isConnected) return; // hidden: onAddTerm said so
    revealRow(row);
    revealInPage(row);
    row.classList.remove('flash');
    row.classList.add('flash');
    clearTimeout(row._flashTimer);
    row._flashTimer = setTimeout(() => row.classList.remove('flash'), FLASH_MS);
  }

  // The bottom of the window for the row: above the toasts (#toasts) where
  // they cover the table's columns, else the window's bottom.
  function usableBottom(box) {
    const vh = window.innerHeight || 0;
    const ts = document.getElementById('toasts');
    if (!ts || typeof ts.getBoundingClientRect !== 'function') return vh;
    const t = ts.getBoundingClientRect();
    if (!(t.bottom > t.top) || t.top >= vh || t.right <= box.left || t.left >= box.right) return vh;
    return Math.max(0, Math.min(vh, t.top - REVEAL_GAP));
  }

  // Scrolls the table so that row is in view below its sticky column
  // header, by as little as it takes (as scrollIntoView block "nearest"
  // does for the table alone; that one would scroll the page too, and put
  // a row above the view under the header): within the part of the table
  // the window shows when that part can hold the row; else as near that
  // part as the table can (its top when the table lies below what the
  // window shows, its bottom when above), for the page to follow.
  function revealRow(row) {
    if (typeof tableWrap.getBoundingClientRect !== 'function' || typeof row.getBoundingClientRect !== 'function') return;
    const box = tableWrap.getBoundingClientRect();
    const r = row.getBoundingClientRect();
    const h = r.bottom - r.top;
    const head = shell && shell.table.querySelector('thead');
    const hr = head ? head.getBoundingClientRect() : null;
    const inner = box.top + (tableWrap.clientTop || 0);
    let top = inner + (hr ? hr.bottom - hr.top : 0);
    let bottom = inner + (tableWrap.clientHeight || box.bottom - box.top);
    if (window.innerHeight) {
      const seenTop = Math.max(top, 0);
      const seenBottom = Math.min(bottom, usableBottom(box));
      if (seenBottom - seenTop >= h) {
        top = seenTop;
        bottom = seenBottom;
      } else if (seenBottom < top + h) {
        bottom = top + h; // the table below what the window shows: the row to its top
      } else {
        top = bottom - h; // above: to its bottom
      }
    }
    const at = tableWrap.scrollTop || 0;
    if (r.top < top) tableWrap.scrollTop = at - (top - r.top);
    else if (r.bottom > bottom) tableWrap.scrollTop = at + Math.min(r.bottom - bottom, r.top - top);
  }

  // Then the page (the window scrolls it), vertically and by as little as
  // it takes, when the row placed in the table is not wholly in the window
  // (above the toasts); a row in the window leaves the page where it is.
  // The field with the focus in the card stays in the window (it wins).
  // Done by hand rather than with row.scrollIntoView: that one would also
  // move the table's own box, sideways too where the table is wider than
  // it (narrow windows). Whole pixels, so the row's edge is not cut off.
  function revealInPage(row) {
    const vh = window.innerHeight || 0;
    if (!vh || typeof row.getBoundingClientRect !== 'function' || typeof window.scrollBy !== 'function') return;
    const r = row.getBoundingClientRect();
    const bottom = usableBottom(tableWrap.getBoundingClientRect());
    let dy = 0;
    if (r.top < 0) dy = Math.floor(r.top);
    else if (r.bottom > bottom) dy = Math.ceil(Math.min(r.bottom - bottom, r.top));
    if (!dy) return;
    const a = document.activeElement;
    if (a && a !== document.body && el.contains(a) && typeof a.getBoundingClientRect === 'function') {
      const f = a.getBoundingClientRect();
      // only a field the window shows now is kept there
      if (f.bottom > f.top && f.top >= 0 && f.bottom <= vh) {
        const down = Math.max(0, Math.floor(f.top - REVEAL_GAP)); // how far the page may go down
        const up = Math.min(0, Math.ceil(f.bottom + REVEAL_GAP - vh)); // and up (negative)
        dy = Math.max(up, Math.min(down, dy));
      }
    }
    if (dy) window.scrollBy({ top: dy, left: 0, behavior: 'auto' });
  }

  // What the card shows in place of the table, as a key that changes with
  // its text; null: the table.
  function flatState(shown) {
    if (!terms) return termsError ? `error:${termsError}` : 'loading';
    if (!terms.length) return U.extractionBusy(course) ? 'empty:busy' : 'empty';
    if (!shown.length) return 'none';
    return null;
  }

  function drawFlat(key) {
    shell = null;
    if (footKey !== '') {
      clear(termsFoot);
      footKey = '';
    }
    if (key === flatKey) return;
    flatKey = key;
    clear(tableWrap);
    tableWrap.classList.add('is-flat');
    if (key === 'loading') {
      tableWrap.append(skeleton(5));
    } else if (key.startsWith('error:')) {
      tableWrap.append(errorBlock(termsError, retryTerms));
    } else if (key === 'none') {
      tableWrap.append(h('div', { class: 'terms-empty' }, icon('search', { size: 20 }), h('strong', null, t('glossary.filter.none')), btn(t('glossary.filter.clear'), { size: 'sm', onclick: clearFilters })));
    } else {
      const busy = key === 'empty:busy';
      tableWrap.append(
        h(
          'div',
          { class: 'terms-empty' },
          icon('hash', { size: 20 }),
          h('strong', null, busy ? t('glossary.empty.extracting') : t('glossary.empty.title')),
          h('span', null, busy ? t('glossary.empty.extracting_hint') : t('glossary.empty.hint')),
        ),
      );
    }
  }

  function syncTable(shown) {
    flatKey = null;
    if (!shell) buildShell();
    shell.table.classList.toggle('is-dragging', !!dragId);
    const all = { enforce: 0, reference: 0, off: 0 };
    const lists = { enforce: [], reference: [], off: [] };
    const live = new Set();
    for (const term of terms) {
      all[groupOf(term)]++;
      live.add(term.id);
    }
    for (const term of shown) lists[groupOf(term)].push(term);
    for (const tid of rowCache.keys()) if (!live.has(tid)) rowCache.delete(tid);
    for (const g of GROUPS) syncGroup(g, lists[g.key], all[g.key]);
    syncFoot(shown.length);
  }

  function buildShell() {
    clear(tableWrap);
    tableWrap.classList.remove('is-flat');
    const groups = {};
    for (const g of GROUPS) groups[g.key] = groupShell(g);
    const table = h(
      'table',
      { class: 'terms terms-grouped' },
      h(
        'colgroup',
        null,
        h('col', { class: 'col-grip' }),
        h('col', { class: 'col-en' }),
        h('col', { class: 'col-zh' }),
        h('col', { class: 'col-src' }),
        h('col', { class: 'col-freq' }),
        h('col', { class: 'col-menu' }),
      ),
      h(
        'thead',
        null,
        h(
          'tr',
          null,
          h('th', null, h('span', { class: 'visually-hidden' }, t('glossary.col.drag'))),
          h('th', null, t('glossary.col.en')),
          h('th', null, t('glossary.col.zh')),
          h('th', null, t('glossary.col.source')),
          h('th', { class: 'num' }, t('glossary.col.freq')),
          h('th', null, h('span', { class: 'visually-hidden' }, t('glossary.col.actions'))),
        ),
      ),
      GROUPS.map((g) => groups[g.key].body),
    );
    shell = { table, groups };
    tableWrap.append(table);
  }

  // A group's rows: its header (the fold toggle and the count), the rows of
  // the terms shown, or a placeholder when it shows none.
  function syncGroup(g, list, total) {
    const v = shell.groups[g.key];
    const isCollapsed = collapsed.has(g.key);
    const count = filtersActive() ? `${list.length} / ${total}` : String(total);
    if (v.count.textContent !== count) v.count.textContent = count;
    if (v.toggle.getAttribute('aria-expanded') !== String(!isCollapsed)) v.toggle.setAttribute('aria-expanded', String(!isCollapsed));
    v.body.classList.toggle('is-collapsed', isCollapsed);
    const want = [v.head];
    if (!isCollapsed) {
      for (const term of list) want.push(rowFor(term));
      if (!list.length) {
        const text = filtersActive() && total ? t('glossary.group.no_match') : t(`glossary.group.${g.key}_drop`);
        if (!v.empty || v.empty.textContent !== text) v.empty = h('tr', { class: 'tg-empty' }, h('td', { colspan: '6' }, text));
        want.push(v.empty);
      }
    }
    placeRows(v.body, want);
  }

  // The row of a term: the one on the page while its term, editor and busy
  // state are the same, else a new one.
  function rowFor(term) {
    const key = JSON.stringify(term) + (editingTerm === term.id ? '\n(editing)' : '') + (moving.has(term.id) ? '\n(busy)' : '');
    const c = rowCache.get(term.id);
    if (c && c.key === key) {
      // put back after a time off the page: its markers closed (R71 L1)
      if (!c.row.isConnected) for (const w of c.row.querySelectorAll('.mk-wrap')) if (w._reset) w._reset();
      return c.row;
    }
    const row = termRow(term);
    rowCache.set(term.id, { key, row });
    return row;
  }

  // Makes the children of body the rows of want, in order, moving only what
  // is out of place: a row that stays is not touched (nor its focus).
  function placeRows(body, want) {
    const keep = new Set(want);
    let cur = body.firstChild;
    for (const row of want) {
      while (cur && !keep.has(cur)) {
        const next = cur.nextSibling;
        cur.remove();
        cur = next;
      }
      if (cur === row) cur = cur.nextSibling;
      else body.insertBefore(row, cur);
    }
    while (cur) {
      const next = cur.nextSibling;
      cur.remove();
      cur = next;
    }
  }

  function syncFoot(n) {
    const active = filtersActive();
    const key = `${active}:${n}:${terms.length}`;
    if (key === footKey) return;
    footKey = key;
    clear(termsFoot);
    append(termsFoot, [
      h('span', null, active ? t('glossary.foot.shown', { shown: n, n: terms.length }) : t('glossary.foot.total', { n: terms.length })),
      active ? btn(t('glossary.filter.clear'), { size: 'sm', kind: 'ghost', onclick: clearFilters }) : null,
      h('span', { class: 'terms-keys' }, tNodes('glossary.foot.keys', { f2: h('kbd', null, 'F2'), del: h('kbd', null, 'Del'), more: h('kbd', null, 'Shift+F10') })),
    ]);
  }

  // A group's header and its body (the drop target), built with the table;
  // syncGroup fills in the count, the fold state and the rows.
  function groupShell(g) {
    const count = h('span', { class: 'tg-count' });
    const toggle = h(
      'button',
      {
        type: 'button',
        class: 'tg-toggle',
        'aria-expanded': 'true',
        dataset: { fkey: `tg:${g.key}` },
        onclick: () => {
          if (collapsed.has(g.key)) collapsed.delete(g.key);
          else collapsed.add(g.key);
          saveCollapsed(collapsed);
          renderTable();
        },
      },
      icon('chevron-right', { size: 14, class: 'tg-chev' }),
      icon(g.icon, { size: 14, class: 'tg-icon' }),
      h('span', { class: 'tg-name' }, groupName(g.key)),
      count,
    );
    const head = h('tr', { class: 'tg-hd' }, h('th', { colspan: '6', scope: 'rowgroup' }, h('div', { class: 'tg-hd-in' }, toggle, h('span', { class: 'tg-desc' }, t(`glossary.group.${g.key}_desc`)))));
    const body = h('tbody', { class: `tg tg-${g.key}`, dataset: { group: g.key } }, head);
    // drop target: the whole group, its header included (also when collapsed)
    body.addEventListener('dragover', (e) => {
      const term = dragId && terms && terms.find((x) => x.id === dragId);
      if (!term || groupOf(term) === g.key) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      body.classList.add('is-drop');
    });
    body.addEventListener('dragleave', (e) => {
      if (!body.contains(e.relatedTarget)) body.classList.remove('is-drop');
    });
    body.addEventListener('drop', (e) => {
      body.classList.remove('is-drop');
      const term = dragId && terms && terms.find((x) => x.id === dragId);
      if (!term) return;
      e.preventDefault();
      dragId = null;
      moveTerm(term, g.key);
    });
    return { body, head, toggle, count, empty: null };
  }

  function termRow(term) {
    const g = groupOf(term);
    const busy = moving.has(term.id);
    const more = menuButton(iconBtn('ellipsis', t('glossary.term.actions_label', { en: term.en }), { fkey: `term:${term.id}:menu`, menu: true }), () => termMenuItems(term));
    const marks = [];
    if (term.locked) {
      marks.push(
        markCard({
          ic: 'lock',
          cls: 'mk-lock',
          label: t('glossary.term.locked_label'),
          title: t('glossary.term.locked'),
          fkey: `term:${term.id}:lock`,
          body: [h('div', { class: 'mk-title' }, icon('lock', { size: 12 }), t('glossary.term.locked')), h('p', { class: 'mk-text' }, t('glossary.term.locked_text'))],
          action: { label: t('glossary.term.unlock'), icon: 'lock-open', fkey: `term:${term.id}:unlock`, onclick: () => unlockTerm(term) },
        }),
      );
    }
    if (term.janitor_note) {
      marks.push(
        markCard({
          ic: 'wand-sparkles',
          cls: 'mk-janitor',
          label: t('glossary.term.janitor_label', { note: term.janitor_note }),
          title: t('glossary.term.janitor_title'),
          fkey: `term:${term.id}:jnote`,
          body: [h('div', { class: 'mk-title' }, icon('wand-sparkles', { size: 12 }), t('glossary.term.janitor_title')), h('p', { class: 'mk-text', lang: 'en' }, term.janitor_note)],
        }),
      );
    }
    const row = h(
      'tr',
      {
        class: 'term-row' + (term.enabled ? '' : ' term-disabled') + (busy ? ' is-busy' : ''),
        draggable: editingTerm === term.id || busy ? 'false' : 'true',
        dataset: { term: term.id },
        'aria-busy': busy ? 'true' : undefined,
      },
      h('td', { class: 'td-grip', title: t('glossary.term.drag_tip') }, icon('grip-vertical', { size: 14 })),
      h(
        'td',
        { class: 'term-en-cell' },
        h('span', { class: 'term-en', text: term.en }),
        marks,
        g === 'off' ? h('span', { class: 'tier-tag', title: t('glossary.term.tier_tag_tip', { tier: tierName(term.tier) }) }, t('glossary.term.tier_tag', { tier: tierName(term.tier) })) : null,
      ),
      h('td', { class: 'term-zh' }, termZhCell(term)),
      h('td', null, h('span', { class: `src src-${term.source}` }, U.SOURCE_NAMES[term.source] || term.source)),
      h('td', { class: 'num muted', text: String(term.freq) }),
      h('td', { class: 'td-end' }, more),
    );
    row.addEventListener('dragstart', (e) => {
      if (e.target.closest && e.target.closest('input, button.mk')) {
        e.preventDefault();
        return;
      }
      dragId = term.id;
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData(DRAG_TYPE, term.id);
      e.dataTransfer.setData('text/plain', term.en);
      row.classList.add('is-dragging');
      const table = row.closest('table');
      if (table) table.classList.add('is-dragging');
    });
    row.addEventListener('dragend', () => {
      dragId = null;
      row.classList.remove('is-dragging');
      for (const x of tableWrap.querySelectorAll('.is-dragging, .is-drop')) x.classList.remove('is-dragging', 'is-drop');
    });
    return row;
  }

  function termZhCell(term) {
    const src = h('span', { class: `zh-src zh-src-${term.zh_source}`, title: t('glossary.term.zh_source_tip') }, U.ZH_SOURCE_NAMES[term.zh_source] || term.zh_source);
    if (editingTerm !== term.id) {
      return h(
        'div',
        { class: 'zh-cell' },
        h(
          'button',
          {
            type: 'button',
            class: 'zh-btn' + (term.zh ? '' : ' muted'),
            lang: term.zh ? zhLang() : undefined, // lecture content: its own fonts (P14.5), display script (P14.6)
            title: t('glossary.term.edit_tip'),
            dataset: { fkey: `term:${term.id}:zh` },
            onclick: () => startEditTerm(term),
          },
          term.zh ? zhText(term.zh) : t('glossary.keep_english'),
        ),
        src,
      );
    }
    // P14.6: the editor holds the stored (Simplified) rendering, saved as typed
    const note = zhEditNote(`term-${term.id}-zh-note`);
    const input = h('input', {
      class: 'input input-term',
      value: termDraft,
      'aria-label': t('glossary.term.zh_input_label', { en: term.en }),
      'aria-describedby': note ? `term-${term.id}-zh-note` : undefined,
      dataset: { fkey: `term:${term.id}:zh-input` },
    });
    let finished = false;
    const finish = async (save, refocus) => {
      if (finished) return;
      finished = true;
      const zh = termDraft.trim();
      if (save && zh !== (term.zh || '')) await patchTerm(term, { zh }, false);
      if (!alive) return;
      // another term's editor opened while the change was sent stays open
      if (editingTerm === term.id) editingTerm = null;
      renderTable();
      // the editor gone, the focus is on the page: back to the rendering
      // (not when the user went on elsewhere while the change was sent)
      const a = document.activeElement;
      if (refocus && (!a || a === document.body)) {
        const b = tableWrap.querySelector(`[data-fkey="${CSS.escape(`term:${term.id}:zh`)}"]`);
        if (b) b.focus();
      }
    };
    input.addEventListener('input', () => (termDraft = input.value));
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.isComposing) {
        e.preventDefault();
        finish(true, true);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        finish(false, true);
      }
    });
    // leaving the editor saves it; not the blur of the editor being taken
    // off the page by a redraw (moved, drawn anew, or gone with its row)
    input.addEventListener('blur', () => {
      if (input.isConnected && !drawing) finish(true, false);
    });
    return note ? h('div', { class: 'zh-edit' }, input, note) : input;
  }

  // A12.2 + G1: only what applies to this term is listed.
  function termMenuItems(term) {
    const g = groupOf(term);
    return [
      { label: t('glossary.menu.edit'), icon: 'pencil', hint: 'F2', onSelect: () => startEditTerm(term) },
      'sep',
      g === 'reference' ? { label: t('glossary.menu.enforce'), icon: 'shield-check', onSelect: () => moveTerm(term, 'enforce') } : null,
      g === 'enforce' ? { label: t('glossary.menu.reference'), icon: 'book-open', onSelect: () => moveTerm(term, 'reference') } : null,
      g === 'off'
        ? { label: t('glossary.menu.enable'), icon: 'check', hint: tierName(term.tier), onSelect: () => moveTerm(term, term.tier === 'reference' ? 'reference' : 'enforce') }
        : { label: t('glossary.menu.disable'), icon: 'ban', onSelect: () => moveTerm(term, 'off') },
      term.locked ? { label: t('glossary.term.unlock'), icon: 'lock-open', onSelect: () => unlockTerm(term) } : null,
      'sep',
      { label: t('common.delete'), icon: 'trash-2', danger: true, hint: 'Del', onSelect: () => deleteTerm(term) },
    ];
  }

  function startEditTerm(term) {
    editingTerm = term.id;
    termDraft = term.zh || '';
    termFocusPending = true;
    renderTable();
  }

  async function moveTerm(term, g) {
    const p = movePatch(term, g);
    if (!p || moving.has(term.id)) return;
    moving.add(term.id);
    renderTable();
    try {
      const updated = await api.patchTerm(term.id, p);
      if (alive && updated) {
        replaceTerm(updated);
        // "set to enforced" / "disabled" / "enabled as reference", and the
        // note that the move locked it
        const done = g === 'off' ? t('glossary.move.off', { en: term.en }) : term.enabled ? t(`glossary.move.${g}`, { en: term.en }) : t('glossary.move.enable', { en: term.en, group: groupName(g) });
        toast(term.locked ? done : done + t('glossary.move.locked_note'), 'success');
        reloadCourse(); // glossary_version
      }
    } catch {
      // shown by the API layer
    } finally {
      moving.delete(term.id);
      if (alive) renderTable();
    }
  }

  async function unlockTerm(term) {
    try {
      const updated = await api.patchTerm(term.id, { locked: false });
      if (alive && updated) {
        replaceTerm(updated);
        toast(t('glossary.term.unlocked', { en: term.en }), 'success');
      }
    } catch {
      // shown by the API layer
    }
    if (alive) renderTable();
  }

  async function patchTerm(term, patch, rerender = true) {
    try {
      const updated = await api.patchTerm(term.id, patch);
      if (alive && updated) {
        replaceTerm(updated);
        reloadCourse(); // glossary_version
      }
    } catch {
      // shown by the API layer
    }
    if (alive && rerender) renderTable();
  }

  async function deleteTerm(term) {
    // keyboard focus afterwards: the same control in the next (or previous) row
    const a = document.activeElement;
    const kind = a && tableWrap.contains(a) && a.dataset.fkey ? a.dataset.fkey.split(':').pop() : '';
    const rows = [...tableWrap.querySelectorAll('tr[data-term]')].map((r) => r.dataset.term);
    const k = rows.indexOf(term.id);
    const near = rows[k + 1] || rows[k - 1];
    const ok = await confirmDialog({
      title: t('glossary.delete.title'),
      message: t('glossary.delete.message', { en: term.en }),
      okText: t('common.delete'),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteTerm(term.id);
    } catch {
      return;
    }
    if (!alive) return;
    if (kind) termFocusAfter = near ? `term:${near}:${kind}` : 'terms:add';
    toast(t('glossary.delete.done', { en: term.en }), 'success');
    reload();
    reloadCourse();
  }

  async function onAddTerm(e) {
    e.preventDefault();
    const en = enInput.value.trim();
    const zh = zhInput.value.trim();
    if (!en) {
      toast(t('glossary.add.en_required'), 'warn');
      enInput.focus();
      return;
    }
    let term;
    try {
      term = await api.addTerm(courseId, en, zh);
      if (!term) return;
    } catch {
      return;
    }
    // WV-05: scrolled to and highlighted once drawn; one the search, the
    // source filter or a collapsed group hides is not, and the toast says so
    const hidden = !filterOf()(term) || collapsed.has(groupOf(term));
    toast(t(hidden ? 'glossary.add.done_hidden' : 'glossary.add.done', { en }), 'success');
    if (!hidden && term.id) termAdded = term.id;
    enInput.value = '';
    zhInput.value = '';
    enInput.focus();
    reload();
    reloadCourse();
  }

  return {
    el,
    setCourse,
    reload,
    openReport,
    // A glossary_review job event: progress in place; false = reload the course.
    onJob(ev) {
      const st = U.reviewStage(course);
      if (st && st.status === ev.status && reviewBar) {
        reviewBar.setProgress(ev.stage, ev.progress);
        if (reviewChip && reviewChip.lastChild) reviewChip.lastChild.textContent = t('glossary.janitor.running', { pct: U.pct(ev.progress) });
        return true;
      }
      return false;
    },
    destroy() {
      alive = false;
      clearTimeout(filterTimer);
    },
  };
}
