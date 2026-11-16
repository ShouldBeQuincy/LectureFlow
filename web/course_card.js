// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Course card of the dashboard's course overview and the course list (v1.1
// A9, A12.2 course-card context menu). Works with api.CourseCard and
// api.Course (failed_count: round 7, absent from an older server).

import { h, iconBtn } from './dom.js';
import { icon } from './icons.js';
import { bindContextMenu, menuButton } from './menu.js';
import { courseMenuItems } from './actions.js';
import * as U from './util.js';
import { t } from './i18n.js';

// t(key, {n}) with the number in bold: the catalog text is split at the
// formatted number.
function boldCount(key, n) {
  const s = t(key, { n });
  const num = String(n);
  const i = s.indexOf(num);
  if (i < 0) return [s];
  return [s.slice(0, i), h('b', null, num), s.slice(i + num.length)].filter((x) => x !== '');
}

// Course → the CourseCard shape (same field names as the dashboard's cards).
export function cardOf(c) {
  if (c.stale_count !== undefined) return c;
  return {
    id: c.id,
    name: c.name,
    code: c.code,
    lecture_count: c.lecture_count,
    term_count: c.term_count,
    draft_count: c.draft_count || 0,
    stale_count: c.stale_lecture_count || 0,
    running_count: c.running_count || 0,
    failed_count: c.failed_count || 0,
    patches_pending: c.patches_pending || 0,
    last_activity_at: c.last_activity_at || c.created_at,
    course_actions: c.course_actions || [],
    glossary_pending: c.glossary_pending,
    glossary_busy: U.glossaryBusyText(c),
  };
}

// settings (optional): () => the settings the page has read, or null; the
// course menu judges the launch's cost by them (round 8 convention F6,
// actions.js courseMenuItems). A page without them passes none.
export function courseCard(card, { onChanged, settings } = {}) {
  const chips = [];
  if (card.running_count) chips.push(h('span', { class: 'chip chip-run' }, h('span', { class: 'chip-dot' }), t('course.card.running', { n: card.running_count })));
  // T24 S2: the lectures that failed, also those of more than 24 hours ago
  // (the workbench lists only the last 24 hours)
  if (card.failed_count) chips.push(h('span', { class: 'chip chip-err' }, t('course.card.failed', { n: card.failed_count })));
  if (card.draft_count) chips.push(h('span', { class: 'chip chip-draft' }, t('course.card.drafts', { n: card.draft_count })));
  if (card.stale_count) chips.push(h('span', { class: 'chip chip-warn' }, t('course.card.stale', { n: card.stale_count })));
  if (card.patches_pending) chips.push(h('span', { class: 'chip chip-quiet' }, t('course.card.reviews', { n: card.patches_pending })));
  const link = h(
    'a',
    { class: 'ccard', href: `#/course/${encodeURIComponent(card.id)}`, dataset: { fkey: 'ccard:' + card.id } },
    h(
      'div',
      { class: 'ccard-top' },
      card.code ? h('span', { class: 'ccard-code' }, card.code) : h('span', { class: 'ccard-code is-none' }, t('course.card.no_code')),
      card.glossary_pending ? h('span', { class: 'ccard-flag', title: t('course.glossary_pending_tip') }, icon('sparkles', { size: 13 }), card.glossary_busy || t('course.card.glossary_pending')) : null,
    ),
    h('div', { class: 'ccard-name', title: card.name }, card.name),
    h('div', { class: 'ccard-stats' }, h('span', null, boldCount('course.card.lectures', card.lecture_count)), h('span', null, boldCount('course.card.terms', card.term_count))),
    h('div', { class: 'ccard-chips' }, chips.length ? chips : h('span', { class: 'ccard-quiet' }, card.lecture_count ? t('course.card.all_ready') : t('course.card.no_lectures'))),
    h('div', { class: 'ccard-foot' }, icon('clock', { size: 12 }), U.relTime(card.last_activity_at) || '—'),
  );
  const items = () => courseMenuItems(card, settings ? { onChanged, settings } : { onChanged });
  // T22 G15 (round 8 WD-03): the right-click menu was the card's only way
  // to its actions. A visible ⋯ at its top right opens the same items, as
  // the course page's ⋯ (view_course.js, same name). It is the link's
  // sibling, not inside it (no control in a link: its click never opens the
  // course, the link's name stays the card's text), next after it in the
  // Tab order; the wrapper takes the card's place in the grid, and the
  // right-click menu is bound to it (on the ⋯ too).
  // R86 L6 (r9 WD-06): a page has many cards, so each ⋯ names its course
  // (the code, else the name), as the lecture rows' ⋯ name their lecture.
  const more = menuButton(iconBtn('ellipsis', t('course.card.menu_named', { name: card.code || card.name }), { fkey: `ccard:${card.id}:menu`, menu: true, class: 'ccard-more' }), items);
  const el = h('div', { class: 'ccard-wrap' }, link, more);
  bindContextMenu(el, items);
  return el;
}

export function addCourseCard(onClick) {
  return h(
    'button',
    { type: 'button', class: 'ccard ccard-add', onclick: onClick, dataset: { fkey: 'ccard:add' } },
    h('span', { class: 'ccard-add-icon' }, icon('plus', { size: 18 })),
    h('span', null, t('course.add')),
  );
}
