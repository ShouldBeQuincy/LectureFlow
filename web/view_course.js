// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/course/{id}: the course page — header with the course-level actions
// (v1.1 A2.3 update all stale lectures, A3.2 start all drafts), the lecture
// list (A2, A3.2, A7, A12; lecture_list.js) with uploads (SPEC §9.4, drafts
// since A3), the course materials (§9.3; v1.1 A8.2 SYLLABUS.md editor,
// slides with lecture binding and the A12.2 slide menu) and the glossary
// (glossary.js: v2 tier groups, locks, the janitor header and report).
// #/course/{id}/syllabus opens the syllabus editor (the course card menu's
// "edit syllabus"). v2 P13: the course settings (term start, weeks that do
// not count, the file-name rule; calendar_ui.js, filename_rule.js), the
// file-name notice above the list and the list's search over titles, blurbs
// and numbers (P13.6). lecture kinds: the search also finds kind names; the
// slides are grouped by teaching week, each with a week / lecture / course
// binding (material_bind.js).

import { api } from './api.js';
import { h, append, clear, toast, confirmDialog, preserveFocus, dropzone, makeDropTarget, filePicker, btn, iconBtn, skeleton, errorBlock } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar } from './pipeline.js';
import { menuButton, bindContextMenu } from './menu.js';
import { createLectureList, stateLegend } from './lecture_list.js';
import { launchCourseDrafts, updateCourseStale, courseMenuItems } from './actions.js';
import { cardOf } from './course_card.js';
import { addLectureFiles, addMaterialFiles, retryUpload, removeUpload, uploadItems, onUploadChange } from './uploads.js';
import { uploadRow, updateUploadRow } from './widgets.js';
import { openSyllabusEditor } from './wizard.js';
import { createGlossary } from './glossary.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { openCourseSettings, parseYMD, fmtMonthDay, renumberNotice } from './calendar_ui.js';
import { filenameNotice, openFilenameRuleDialog, recordingSection } from './filename_rule.js';
import { zhQuery } from './zh_display.js';
import { lectureLabel, lectureTagText, kindSearchText } from './kinds.js';
import { bindOf, bindSelect, fromFileName, materialGroups, suggestedWeek, weekChoices } from './material_bind.js';
import { asrNotice } from './asr_ready.js';
import { downloadsSlot } from './downloads.js';
import { confirmUpdate, confirmLaunchDrafts } from './update_confirm.js';

const MEDIA_ACCEPT = U.MEDIA_ACCEPT; // extensions only (util.js, security review 2 M3)
// SYLLABUS.md top-level sections (A8.3) as shown on the syllabus card;
// other headings are shown as written.
const SECTION_KEYS = ['course information', 'learning outcomes', 'weekly topics', 'assessment', 'reading list', 'key terms'];

function sectionName(key, heading) {
  return SECTION_KEYS.includes(key) ? t(`course.syllabus.section.${key.replace(/ /g, '_')}`) : heading;
}

function syllabusOutline(md) {
  const out = [];
  let cur = null;
  for (const line of (md || '').split('\n')) {
    const m = /^##\s+(.+?)\s*$/.exec(line);
    if (m) {
      const key = m[1].toLowerCase();
      cur = { key, name: sectionName(key, m[1]), n: 0 };
      out.push(cur);
    } else if (cur && cur.key === 'weekly topics' && /^\|\s*[^\s|-]/.test(line) && !/^\|\s*week\s*\|/i.test(line)) {
      cur.n++;
    } else if (cur && cur.key === 'key terms' && line.trim()) {
      cur.n += line.split(',').filter((x) => x.trim()).length;
    }
  }
  return out;
}

export function mountCourse(root, route, ctx) {
  const id = route.id;
  let section = route.section;
  let alive = true;
  let course = null;
  let lectures = null; // null until loaded
  let lecturesError = '';
  let loadError = '';
  let busy = new Set();
  let sylDoc = null; // GET /api/courses/{id}/syllabus, for the card's outline
  let sylDocKey = '';
  let sylDocError = '';

  let matFocusPending = null; // fkey to focus once a deleted slide's row is gone ('mat:drop' = the drop zone's button)
  const materialRefs = new Map(); // id -> {bar}
  let glossaryBar = null;
  const probed = new Set();
  const timers = new Map();
  let waitKey = U.waitContextKey(ctx.resources()); // onResources

  // ---- skeleton (inputs and drop zones are built once and never re-rendered) ----
  const el = h('div', { class: 'page page-course' });
  const headEl = h('header', { class: 'page-head' });
  const bodyEl = h('div', { class: 'page-body' });
  el.append(headEl, bodyEl);
  root.append(el);

  const list = createLectureList({
    ctx,
    onLectures(updated) {
      for (const l of updated) upsertLecture(l);
      if (!inPlace(updated)) renderLectures();
      reloadCourse();
    },
    onDeleted(ids) {
      lectures = (lectures || []).filter((l) => !ids.includes(l.id));
      renderLectures();
      reloadCourse();
    },
    onCourseSettings: () => openSettings(), // the W?? hints (P13.4)
    onShowMaterial: (mid) => showMaterial(mid), // a week's slides (lecture kinds)
    course: () => course, // course-ux: weeks for supplements and make-ups, the batch rerun
    filtered: () => !!query.trim(), // course-ux: "select all" selects the shown lectures
  });

  const onLectureFiles = (files) => addLectureFiles(id, files);
  const lecPicker = filePicker({ accept: MEDIA_ACCEPT, multiple: true, onFiles: onLectureFiles });
  const lecCount = h('span', { class: 'card-count' });
  const lecUploads = h('ul', { class: 'upload-list' });
  const lecBody = h('div', { class: 'lec-body' });
  const fnNoticeEl = h('div', { class: 'fn-notice-slot' });
  // 3.1.0 M2: "cannot transcribe yet" above the lectures (asr_ready.js)
  const asrSlot = asrNotice({ variant: 'page' });
  // SPEC-ext: recordings the browser extension sent, above the lectures (downloads.js)
  const dlSlot = downloadsSlot({ ctx, courseId: id, onFinished: () => schedule('lectures', reloadLectures) });
  // P13.6: one search over titles (both languages), blurbs and numbers
  let query = '';
  const search = h('input', {
    type: 'search',
    class: 'input lec-search',
    placeholder: t('course.search.placeholder'),
    'aria-label': t('course.search.label'),
    dataset: { fkey: 'course:search' },
    oninput: () => {
      query = search.value;
      renderLectures();
    },
    onkeydown: (e) => {
      if (e.key === 'Escape' && search.value) {
        e.preventDefault();
        e.stopPropagation();
        search.value = '';
        query = '';
        renderLectures();
      }
    },
  });
  const lecSection = h(
    'section',
    { class: 'card card-lectures panel-drop', 'aria-label': t('course.lectures.title'), dataset: { drop: t('course.lectures.drop_release') } },
    h(
      'header',
      { class: 'card-hd' },
      h('h2', { class: 'card-title' }, icon('audio-lines', { size: 16 }), h('span', null, t('course.lectures.title')), lecCount),
      h('div', { class: 'card-tools' }, search, h('span', { class: 'card-hint' }, t('course.lectures.drop_hint')), btn(t('course.lectures.add'), { size: 'sm', icon: 'upload', onclick: lecPicker.open, fkey: 'course:upload' }), lecPicker.input),
    ),
    fnNoticeEl,
    dlSlot.el,
    lecUploads,
    lecBody,
  );
  makeDropTarget(lecSection, onLectureFiles);

  // syllabus card: a dropped file opens the editor and uploads it as the new raw text
  const syllabusEl = h('div', { class: 'res-card res-syl panel-drop-res', dataset: { drop: t('course.syllabus.drop') } });
  const syllabusTitle = h('h3', { class: 'res-title' });
  const syllabusBody = h('div', { class: 'res-syl-body' });
  syllabusEl.append(syllabusTitle, syllabusBody);
  makeDropTarget(syllabusEl, (files) => course && editSyllabus({ files }));

  const materialsEl = h('div', { class: 'res-card res-mat' });
  const materialsTitle = h('h3', { class: 'res-title' });
  const materialsList = h('ul', { class: 'mat-list', 'aria-label': t('course.materials.title') });
  const materialUploads = h('ul', { class: 'upload-list' });
  materialsEl.append(
    materialsTitle,
    h('p', { class: 'res-hint', text: t('course.materials.hint_week') }),
    materialsList,
    materialUploads,
    dropzone({ text: t('course.materials.drop'), accept: '.pdf,application/pdf', multiple: true, onFiles: (files) => addMaterialFiles(id, files), compact: true, buttonText: t('course.materials.pick') }),
  );

  const resSection = h(
    'section',
    { class: 'card', 'aria-label': t('course.resources.title'), id: 'course-materials' },
    h('header', { class: 'card-hd' }, h('h2', { class: 'card-title' }, icon('book-open', { size: 16 }), h('span', null, t('course.resources.title'))), h('span', { class: 'card-hint' }, t('course.resources.hint'))),
    h('div', { class: 'card-bd res-grid' }, syllabusEl, materialsEl),
  );

  bindContextMenu(materialsList, (e) => {
    const row = e.target.closest('li[data-mat]');
    const m = row && course && course.materials.find((x) => x.id === row.dataset.mat);
    return m ? materialMenuItems(m) : null;
  });

  // ---- glossary (glossary.js) ----
  const glossary = createGlossary({
    courseId: id,
    ctx,
    reloadCourse: () => reloadCourse(),
    onCourse(c) {
      course = c;
      renderHead();
      renderResources();
      glossary.setCourse(c);
    },
  });

  // ---- loading ----
  async function load() {
    glossary.reload(); // on its own: a slow or failing glossary never holds up the page
    const [c, ls] = await Promise.allSettled([api.course(id), api.lectures(id)]);
    if (!alive) return;
    if (c.status === 'fulfilled') {
      course = c.value;
      loadError = '';
    } else if (!course) loadError = c.reason.message || t('common.load_failed');
    if (ls.status === 'fulfilled') {
      lectures = ls.value || [];
      lecturesError = '';
    } else if (!lectures) lecturesError = ls.reason.message || t('common.load_failed');
    ctx.setCurrentCourse(id);
    renderAll();
    if (section === 'syllabus' && course) openSection();
  }

  // #/course/{id}/syllabus: open the editor once, then drop the suffix from
  // the URL (without a hashchange) so the next "edit syllabus" opens it again.
  function openSection() {
    section = null;
    history.replaceState(null, '', `#/course/${encodeURIComponent(id)}`);
    editSyllabus({});
  }

  function editSyllabus(opts) {
    openSyllabusEditor(course, {
      ...opts,
      ctx, // live glossary_seed state in the editor (ctx.subscribe)
      onSaved(c) {
        if (!alive || !c) return;
        course = c;
        renderHead();
        renderResources();
        glossary.setCourse(c);
        schedule('terms', glossary.reload, 800);
      },
    });
  }

  async function reloadCourse() {
    try {
      const c = await api.course(id);
      if (!alive || !c) return;
      course = c;
      renderHead();
      renderResources();
      glossary.setCourse(c);
    } catch {
      // shown by the API layer
    }
  }

  async function reloadLectures() {
    try {
      const ls = await api.lectures(id);
      if (!alive || !ls) return;
      lectures = ls;
      lecturesError = '';
      renderLectures();
      renderMaterials();
      renderNotice(); // the renumbering notice names the lectures
    } catch (e) {
      if (alive && !lectures) {
        lecturesError = e.message;
        renderLectures();
      }
    }
  }

  // Re-reads one lecture after an SSE event. A 404 means it was deleted
  // meanwhile (a running lecture deleted with force still sends the job
  // events of its cancellation): it leaves the list without a toast.
  async function refetchLecture(lid) {
    if (!lectures) return; // the list itself is not loaded (or failed): no partial list
    try {
      const l = await api.lecture(lid, { quiet: true });
      if (!alive || !l || l.course_id !== id || !lectures) return;
      upsertLecture(l);
      if (!inPlace([l])) renderLectures();
    } catch (e) {
      if (!alive || !lectures) return;
      if (e.status !== 404) {
        toast(e.message, 'error');
        return;
      }
      if (lectures.some((x) => x.id === lid)) {
        lectures = lectures.filter((x) => x.id !== lid);
        renderLectures();
        renderMaterials();
        reloadCourse();
      }
    }
  }

  function schedule(key, fn, ms = 250) {
    clearTimeout(timers.get(key));
    timers.set(
      key,
      setTimeout(() => {
        timers.delete(key);
        fn();
      }, ms),
    );
  }

  // P2 hotspot 3: lectures changed (an SSE refresh, an action's answer)
  // redraw their rows only (lecture_list.js updateRow) when the list stays
  // as it is around them; false: the whole list is rendered (a search is
  // on, which may now show or hide them: updateRow asks `filtered`; a
  // lecture new to the list, or moved). One event, one drawing.
  function inPlace(ls) {
    if (!ls.length || !lecBody.contains(list.table)) return false;
    return ls.every((l) => list.updateRow(l));
  }

  function upsertLecture(l) {
    if (!lectures) lectures = [];
    const i = lectures.findIndex((x) => x.id === l.id);
    if (i >= 0) lectures[i] = l;
    else lectures.push(l);
    lectures.sort((a, b) => a.seq - b.seq);
  }

  // ---- rendering ----
  function renderAll() {
    if (!course) {
      clear(headEl);
      clear(bodyEl);
      if (loadError) {
        bodyEl.append(
          h(
            'div',
            { class: 'empty-state' },
            h('div', { class: 'empty-icon' }, icon('circle-alert', { size: 26 })),
            h('h1', { text: t('course.load_failed') }),
            h('p', { text: loadError }),
            h('div', { class: 'empty-actions' }, btn(t('course.reload'), { kind: 'primary', icon: 'rotate-cw', onclick: load }), h('a', { class: 'btn', href: '#/' }, t('player.to_dashboard'))),
          ),
        );
      } else {
        headEl.append(h('div', { class: 'page-titles' }, h('div', { class: 'skel skel-eyebrow' }), h('div', { class: 'skel skel-h1' })));
        bodyEl.append(h('section', { class: 'card' }, h('div', { class: 'card-bd' }, skeleton(6))));
      }
      return;
    }
    if (!lecSection.isConnected) {
      clear(bodyEl);
      bodyEl.append(asrSlot.el, lecSection, list.bar, resSection, glossary.el);
    }
    renderHead();
    renderLectures();
    renderResources();
    glossary.setCourse(course);
  }

  function renderHead() {
    const acts = course.course_actions || [];
    const card = cardOf(course);
    const metaParts = [t('course.head.lectures', { n: course.lecture_count }), t('course.head.terms', { n: course.term_count, version: course.glossary_version })];
    if (course.last_activity_at) metaParts.push(t('course.head.last_activity', { when: U.relTime(course.last_activity_at) }));
    preserveFocus(() => {
      clear(headEl);
      headEl.append(
        h(
          'div',
          { class: 'page-titles' },
          h(
            'div',
            { class: 'page-eyebrow' },
            course.code ? h('span', { class: 'mono' }, course.code) : h('span', null, t('course.eyebrow')),
            course.glossary_pending ? h('span', { class: 'chip chip-run', title: t('course.glossary_pending_tip') }, icon('sparkles', { size: 12 }), U.glossaryBusyText(course) || t('course.glossary_pending')) : null,
          ),
          h('h1', { class: 'page-title' }, course.name),
          h('p', { class: 'page-sub' }, metaParts.join(' · '), ' · ', termStartLink()),
        ),
        h(
          'div',
          { class: 'page-actions' },
          acts.includes('launch_drafts')
            ? btn(t('course.launch_drafts'), { kind: 'primary', icon: 'play', count: course.draft_count, disabled: busy.has('drafts'), fkey: 'course:drafts', fnext: 'course:menu', onclick: () => courseOp('drafts', launchDrafts) })
            : null,
          acts.includes('update_stale')
            ? btn(t('course.update_stale'), { icon: 'refresh-cw', kind: 'warn', count: course.stale_lecture_count, disabled: busy.has('stale'), fkey: 'course:stale', fnext: 'course:menu', onclick: () => courseOp('stale', updateStale) })
            : null,
          menuButton(iconBtn('ellipsis', t('course.menu_label'), { fkey: 'course:menu', menu: true, class: 'icon-btn-bordered' }), () => [
            // T22 B1: the name and the code are in the course settings; "rename…" opens them on the name
            { id: 'rename', label: t('course.menu.rename'), icon: 'pencil', onSelect: () => openSettings({ focus: 'name' }) },
            { label: t('course.settings.menu'), icon: 'settings', onSelect: () => openSettings() },
            'sep',
            ...courseMenuItems(card, { onChanged: () => (reloadCourse(), reloadLectures()) })
              .filter((it) => !(it && it.id === 'open'))
              // T25 X1, T22 G3: the menu's batch operations ask first, as the buttons do
              .map((it) => (it && it.id === 'launch_drafts' ? { ...it, onSelect: () => courseOp('drafts', launchDrafts) } : it && it.id === 'update_stale' ? { ...it, onSelect: () => courseOp('stale', updateStale) } : it)),
          ]),
        ),
      );
    });
    renderNotice();
  }

  // P13.4: the term start in the header; a calm link when it is missing.
  function termStartLink() {
    const d = parseYMD(course.term_start);
    return h(
      'button',
      { type: 'button', class: 'page-sub-link' + (d ? '' : ' is-missing'), title: t('course.head.term_start_edit'), dataset: { fkey: 'course:term' }, onclick: () => openSettings({ focus: 'term' }) },
      icon('calendar', { size: 13 }),
      d ? t('course.head.term_start', { date: fmtMonthDay(d) }) : t('course.head.term_start_missing'),
    );
  }

  // ---- course settings and the file-name rule (P13.3.3, P13.4) ----

  // o.focus: 'name' from "rename…" (T22 B1)
  function openSettings(o = {}) {
    if (!course) return;
    openCourseSettings(course, {
      focus: o.focus,
      lectures: lectures || [],
      recordingSection: (close, busy) => recordingSection(course, { close, busy, onOpenRule: () => openRule(), onChanged: () => reloadCourse() }),
      onSaved: (c) => courseChanged(c),
    });
  }

  function openRule() {
    if (!course) return;
    openFilenameRuleDialog(course, { onChanged: (c) => courseChanged(c) });
  }

  // after a calendar or rule change: numbers and recording times moved
  function courseChanged(c) {
    if (!alive) return;
    if (c) {
      course = c;
      renderHead();
      glossary.setCourse(c);
    }
    reloadCourse();
    reloadLectures();
  }

  function renderNotice() {
    preserveFocus(() => {
      clear(fnNoticeEl);
      const n = course && filenameNotice(course, { onOpen: openRule, onChanged: (c) => courseChanged(c) });
      if (n) fnNoticeEl.append(n);
      // lecture kinds: the title task's kinds renumbered lectures after an export
      const r = course && renumberNotice(course, lectures, { onDismissed: (c) => courseChanged(c) });
      if (r) fnNoticeEl.append(r);
    });
  }

  // T25 X1, T22 G3: "start all drafts" and "update all out-of-date
  // lectures" say first how many lectures, which stages and (an update) what
  // they took the last time (update_confirm.js); what runs is unchanged.
  // Resolve to null when the user did not go on.
  // R74 low 2a (round 8 WL-01): "start all drafts" asks once, after the
  // glossary question when the glossary is still being extracted
  // (launchCourseDrafts asks it first, then opts.ask); not one dialog
  // before it and the glossary's after. The page has no settings of its
  // own, so the cost sentence is the default one (no opts.costMode).
  const withAction = (a) => (lectures || []).filter((l) => (l.lecture_actions || []).includes(a));
  function launchDrafts() {
    return launchCourseDrafts(course, { ask: () => confirmLaunchDrafts(withAction('launch').filter((l) => l.draft), course.draft_count) });
  }
  async function updateStale() {
    if (!(await confirmUpdate(withAction('update'), { named: true, count: course.stale_lecture_count }))) return null;
    return updateCourseStale(course);
  }

  async function courseOp(key, fn) {
    if (busy.has(key)) return;
    busy.add(key);
    renderHead();
    try {
      const res = await fn();
      if (alive && res && res.course) course = res.course;
      if (alive && res && res.results) for (const r of res.results) if (r.lecture) upsertLecture(r.lecture);
    } catch {
      // shown by the API layer
    } finally {
      busy.delete(key);
      if (alive) {
        renderHead();
        renderLectures();
        reloadLectures();
      }
    }
  }

  function renderLectures() {
    preserveFocus(() => {
      lecCount.textContent = lectures ? String(lectures.length) : '';
      renderUploads(lecUploads, 'lecture');
      clear(lecBody);
      if (!lectures) {
        lecBody.append(lecturesError ? h('div', { class: 'card-bd' }, errorBlock(lecturesError, reloadLectures)) : h('div', { class: 'card-bd' }, skeleton(5)));
        return;
      }
      if (lectures.length === 0) {
        lecBody.append(
          h(
            'div',
            { class: 'lec-empty' },
            dropzone({ text: t('course.lectures.drop'), accept: MEDIA_ACCEPT, multiple: true, target: false, onFiles: onLectureFiles }),
            h('p', { class: 'muted small' }, t('course.lectures.empty_hint')),
          ),
        );
        return;
      }
      const q = query.trim().toLowerCase();
      const zq = zhQuery(q, renderLectures); // P14.6: a Traditional term finds the stored Simplified titles
      const shown = q ? lectures.filter((l) => matches(l, q, zq)) : lectures;
      if (!shown.length) {
        lecBody.append(
          h(
            'div',
            { class: 'lec-nomatch', role: 'status' },
            icon('search', { size: 16 }),
            h('span', null, t('course.search.none', { q: query.trim() })),
            btn(t('course.search.clear'), {
              size: 'sm',
              kind: 'ghost',
              fkey: 'course:search-clear',
              onclick: () => {
                search.value = '';
                query = '';
                renderLectures();
                search.focus();
              },
            }),
          ),
        );
        return;
      }
      list.render(shown);
      lecBody.append(list.table, stateLegend());
    });
  }

  // P13.6: titles in both languages, blurbs and the number (W02L1); the
  // Chinese fields through zq (P14.6 Traditional display); lecture kinds:
  // the kind ("tutorial", "辅导") and "Week 3"
  function matches(l, q, zq) {
    return [l.label, l.cite_label, l.title, l.blurb_en, kindSearchText(l), lectureTagText(l, { form: 'full' })].some((x) => x && String(x).toLowerCase().includes(q)) || [l.title, l.title_zh, l.blurb_zh].some((x) => zq.test(x));
  }

  function renderUploads(container, kind) {
    clear(container);
    for (const item of uploadItems(id, kind)) container.append(uploadRow(item, { onRetry: retryUpload, onRemove: removeUpload }));
  }

  function renderResources() {
    renderSyllabus();
    renderMaterials();
  }

  // ---- syllabus card (A8.2) ----

  function renderSyllabus() {
    preserveFocus(() => {
      const s = course.syllabus;
      clear(syllabusTitle);
      append(syllabusTitle, [
        icon('file-text', { size: 15 }),
        t('course.syllabus.title'),
        s ? (s.formatted ? h('span', { class: 'chip chip-ok' }, icon('check', { size: 11, stroke: 2.6 }), 'SYLLABUS.md') : h('span', { class: 'chip chip-warn', title: t('course.syllabus.raw_tip') }, t('course.syllabus.raw'))) : null,
      ]);
      clear(syllabusBody);
      glossaryBar = null;
      if (!s) {
        syllabusBody.append(
          h(
            'div',
            { class: 'syl-card is-empty' },
            h('p', { class: 'res-hint' }, t('course.syllabus.empty')),
            h('div', { class: 'syl-card-actions' }, btn(t('course.syllabus.add'), { kind: 'primary', size: 'sm', icon: 'plus', fkey: 'syl:add', onclick: () => editSyllabus({ source: true }) }), h('span', { class: 'muted small' }, t('course.syllabus.drop_hint'))),
          ),
        );
        return;
      }
      // the outline needs SYLLABUS.md itself; read it again only when it changed
      const key = s.formatted ? `${s.filename}|${s.chars}` : '';
      if (key !== sylDocKey) {
        sylDocKey = key;
        sylDoc = null;
        sylDocError = '';
        if (key) loadSyllabusDoc(key);
      }
      const meta = [t('course.syllabus.chars', { n: Number(s.chars), count: U.fmtInt(Number(s.chars)) }), s.formatted ? t('course.syllabus.formatted') : t('course.syllabus.can_format')];
      const card = h(
        'div',
        { class: 'syl-card' },
        h(
          'button',
          { type: 'button', class: 'syl-card-file', title: t('course.syllabus.open_tip'), dataset: { fkey: 'syl:open' }, onclick: () => editSyllabus({}) },
          h('span', { class: 'syl-card-icon' }, icon('file-text', { size: 18 })),
          h('span', { class: 'syl-card-text' }, h('span', { class: 'fname', text: s.filename === 'pasted.txt' ? t('course.syllabus.pasted') : s.filename }), h('span', { class: 'muted small' }, meta.join(' · '))),
        ),
      );
      if (s.formatted && sylDocError) {
        card.append(
          h(
            'div',
            { class: 'res-doc-error', role: 'alert' },
            icon('circle-alert', { size: 14 }),
            h('span', { class: 'res-doc-error-text' }, t('course.syllabus.outline_failed', { error: sylDocError })),
            btn(t('common.retry'), { size: 'sm', kind: 'ghost', icon: 'rotate-cw', fkey: 'syl:doc-retry', onclick: retrySyllabusDoc }),
          ),
        );
      } else if (s.formatted && !sylDoc) {
        card.append(h('div', { class: 'res-outline-skel', 'aria-hidden': 'true' }, [64, 52, 70, 44].map((w) => h('span', { class: 'skel', style: { width: `${w}px` } }))));
      }
      const outline = s.formatted && sylDoc && !sylDocError ? syllabusOutline(sylDoc.markdown) : [];
      if (outline.length) {
        card.append(
          h(
            'ul',
            { class: 'syl-outline', 'aria-label': t('course.syllabus.outline_label') },
            outline.map((x) => h('li', null, x.name, x.n ? h('span', { class: 'num' }, x.key === 'weekly topics' ? t('course.syllabus.weeks', { n: x.n }) : t('course.syllabus.items', { n: x.n })) : null)),
          ),
        );
      }
      // glossary_seed only; the v2 glossary_review is shown in the glossary card
      const seed = course.stages.filter((x) => x.stage === 'glossary_seed');
      if (seed.length) {
        glossaryBar = pipelineBar(seed, { key: `crs:${id}`, sys: ctx.system, res: ctx.resources, onAction: courseAction, label: t('course.syllabus.progress_label'), pct: false });
        const sum = U.lectureSummary(seed, { sys: ctx.system(), res: ctx.resources() });
        const warn = U.warningHint(seed);
        card.append(h('div', { class: 'mini-pipe' }, glossaryBar, h('span', { class: `small tone-${sum.tone}`, text: sum.kind === 'done' ? t('course.terms_extracted') : sum.text }), warn ? h('span', { class: 'chip chip-warn', text: warn }) : null));
      }
      card.append(
        h(
          'div',
          { class: 'syl-card-actions' },
          btn(t('course.syllabus.edit'), { size: 'sm', icon: 'pencil', fkey: 'syl:edit', onclick: () => editSyllabus({}) }),
          btn(t('course.syllabus.replace'), { size: 'sm', kind: 'ghost', icon: 'upload', fkey: 'syl:replace', onclick: () => editSyllabus({ source: true }) }),
        ),
      );
      syllabusBody.append(card);
    });
  }

  // Optional data for the card's outline: a failure is shown in the card
  // with a retry button, never as a toast (the rest of the page works).
  async function loadSyllabusDoc(key) {
    try {
      const doc = await api.syllabus(id, { quiet: true });
      if (!alive || key !== sylDocKey) return;
      sylDoc = doc;
      sylDocError = '';
    } catch (e) {
      if (!alive || key !== sylDocKey) return;
      sylDocError = e.message || t('common.load_failed');
    }
    if (course) renderSyllabus();
  }

  function retrySyllabusDoc() {
    sylDocError = '';
    sylDoc = null;
    renderSyllabus(); // placeholder while it loads again
    if (sylDocKey) loadSyllabusDoc(sylDocKey);
  }

  // ---- materials (§9.3, A12.2 slide menu) ----

  function renderMaterials() {
    if (!course) return;
    preserveFocus(() => {
      clear(materialsTitle);
      append(materialsTitle, [icon('layers', { size: 15 }), t('course.materials.title'), course.materials.length ? h('span', { class: 'card-count' }, String(course.materials.length)) : null]);
      clear(materialsList);
      materialRefs.clear();
      if (course.materials.length === 0) materialsList.append(h('li', { class: 'mat-empty' }, t('course.materials.empty')));
      // lecture kinds: the whole course, then by teaching week
      for (const g of materialGroups(course.materials, lectures || [])) {
        materialsList.append(h('li', { class: 'mat-group', 'aria-hidden': 'true' }, h('span', null, g.title), h('span', { class: 'mat-group-n' }, String(g.items.length))));
        for (const m of g.items) materialsList.append(materialRow(m));
      }
      renderUploads(materialUploads, 'material');
    });
    if (matFocusPending) {
      const want = matFocusPending;
      const el = want === 'mat:drop' ? materialsEl.querySelector('.dropzone button') : materialsEl.querySelector(`[data-fkey="${CSS.escape(want)}"]`);
      if (el) {
        matFocusPending = null;
        el.focus();
      }
    }
  }

  function materialMenuItems(m) {
    const b = bindOf(m);
    const lecs = (lectures || []).filter((l) => !(b.scope === 'lecture' && l.id === b.lecture_id));
    const weeks = weekChoices(lectures, course.materials).filter((w) => !(b.scope === 'week' && b.week === w));
    return [
      { label: t('course.materials.bind_week'), icon: 'calendar', items: weeks.map((w) => ({ label: t('kind.week', { week: w }), hint: m.week_suggested === w ? t('course.materials.from_name') : '', onSelect: () => bindMaterial(m, { scope: 'week', week: w, lecture_id: null }) })) },
      lecs.length ? { label: t('course.materials.bind'), icon: 'audio-lines', items: lecs.map((l) => ({ label: lectureLabel(l), hint: l.draft ? t('course.lectures.draft') : '', onSelect: () => bindMaterial(m, { scope: 'lecture', week: null, lecture_id: l.id }) })) } : null,
      b.scope !== 'course' ? { label: t('course.materials.set_general'), icon: 'layers', onSelect: () => bindMaterial(m, { scope: 'course', week: null, lecture_id: null }) } : null,
      'sep',
      m.can_delete ? { label: t('common.delete'), icon: 'trash-2', danger: true, hint: 'Del', onSelect: () => deleteMaterial(m) } : null,
    ];
  }

  function materialRow(m) {
    const bar = pipelineBar(m.stages, { key: `mat:${m.id}`, sys: ctx.system, res: ctx.resources, onAction: (stage, action) => materialAction(m.id, stage, action), pct: false, label: t('course.materials.progress_label', { name: m.filename }) });
    materialRefs.set(m.id, { bar });
    const sel = bindSelect(m, {
      lectures,
      weeks: weekChoices(lectures, course.materials),
      label: t('course.materials.scope_label', { name: m.filename }),
      fkey: `mat:${m.id}:bind`,
      onChange: (b) => bindMaterial(m, b),
    });
    const named = fromFileName(m) ? h('span', { class: 'chip chip-quiet mat-named', title: t('course.materials.from_name_tip', { week: m.week_suggested }) }, t('course.materials.from_name')) : null;
    // course-wide slides whose name gives a week: "From the file name, probably week 3 · Use for week 3"
    const sw = suggestedWeek(m);
    const suggest = sw
      ? h(
          'div',
          { class: 'mat-suggest' },
          icon('calendar', { size: 13, class: 'dim' }),
          h('span', { class: 'mat-suggest-text' }, t('course.materials.suggest_week', { week: sw })),
          btn(t('course.materials.suggest_bind', { week: sw }), {
            size: 'sm',
            kind: 'ghost',
            class: 'mat-suggest-btn',
            ariaLabel: t('course.materials.suggest_bind_aria', { name: m.filename, week: sw }),
            fkey: `mat:${m.id}:suggest`,
            onclick: (e) => {
              e.currentTarget.disabled = true; // until the server's answer re-renders the row
              matFocusPending = `mat:${m.id}:bind`;
              bindMaterial(m, { scope: 'week', week: sw, lecture_id: null });
            },
          }),
        )
      : null;
    // v3 V6: many pages came out empty (scans, pictures of slides): a quiet note
    const textNote = m.text_warning === 'text_incomplete'
      ? h('div', { class: 'mat-suggest mat-text-note' }, icon('info', { size: 13, class: 'dim' }), h('span', { class: 'mat-suggest-text' }, t('course.materials.text_incomplete')))
      : null;
    const sum = U.lectureSummary(m.stages, { sys: ctx.system(), res: ctx.resources() });
    const warn = U.warningHint(m.stages);
    const more = menuButton(iconBtn('ellipsis', t('course.materials.actions_label', { name: m.filename }), { fkey: `mat:${m.id}:menu`, menu: true }), () => materialMenuItems(m));
    const row = h(
      'li',
      { class: 'mat-row', dataset: { mat: m.id } },
      h(
        'div',
        { class: 'mat-top' },
        h('div', { class: 'mat-main' }, icon('file-text', { size: 14, class: 'dim' }), h('span', { class: 'fname', text: m.filename, title: m.filename }), m.pages ? h('span', { class: 'muted small' }, t('course.materials.pages', { n: m.pages })) : null),
        h('span', { class: `small mat-state tone-${sum.tone}`, text: sum.kind === 'done' ? t('course.terms_extracted') : sum.text }),
        more,
      ),
      h('div', { class: 'mat-bottom' }, h('div', { class: 'mat-bind-row' }, sel, named), h('div', { class: 'mini-pipe' }, bar, warn ? h('span', { class: 'chip chip-warn', text: warn }) : null)),
      suggest,
      textNote,
    );
    row.addEventListener('keydown', (e) => {
      if (e.key === 'Delete' && !e.target.closest('select') && m.can_delete) {
        e.preventDefault();
        deleteMaterial(m);
      }
    });
    return row;
  }

  async function deleteMaterial(m) {
    // where the keyboard focus goes once the row is gone: the next (or previous) slide's ⋯ button
    const list = course.materials;
    const k = list.findIndex((x) => x.id === m.id);
    const near = list[k + 1] || list[k - 1];
    const ok = await confirmDialog({
      title: t('course.materials.delete_title'),
      message: t('course.materials.delete_message', { name: m.filename }),
      okText: t('common.delete'),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteMaterial(m.id);
    } catch {
      return; // shown by the API layer
    }
    toast(t('course.materials.deleted', { name: m.filename }), 'success');
    if (!alive) return;
    if (materialsEl.contains(document.activeElement)) matFocusPending = near ? `mat:${near.id}:menu` : 'mat:drop';
    reloadCourse();
  }

  // bind: {scope: week|lecture|course, week, lecture_id} (lecture kinds)
  async function bindMaterial(m, bind) {
    try {
      const updated = await api.setMaterialBind(m.id, bind);
      if (alive && updated) {
        const i = course.materials.findIndex((x) => x.id === updated.id);
        if (i >= 0) course.materials[i] = updated;
        const b = bindOf(updated);
        const l = b.scope === 'lecture' && (lectures || []).find((x) => x.id === b.lecture_id);
        let msg = t('course.materials.unbound', { name: m.filename });
        if (b.scope === 'week') msg = t('course.materials.bound_week', { name: m.filename, week: b.week });
        else if (l) msg = t('course.materials.bound', { name: m.filename, lecture: lectureLabel(l) });
        toast(msg, 'success');
        reloadLectures(); // the lectures' slides (and the week headers) changed
      }
    } catch {
      // shown by the API layer
    }
    if (alive) renderMaterials(); // shows the server's value (no optimistic change)
  }

  // A week header's slide file (lecture list): the row in the materials card.
  function showMaterial(mid) {
    const row = materialsList.querySelector(`li[data-mat="${CSS.escape(mid)}"]`);
    if (!row) return;
    row.scrollIntoView({ behavior: 'smooth', block: 'center' });
    row.classList.remove('flash');
    void row.offsetWidth; // restart the animation
    row.classList.add('flash');
    const sel = row.querySelector('select');
    if (sel) sel.focus({ preventScroll: true });
  }

  async function materialAction(mid, stage, action) {
    const updated = await api.materialStageAction(mid, stage, action);
    if (!alive || !updated) return;
    const i = course.materials.findIndex((x) => x.id === updated.id);
    if (i >= 0) course.materials[i] = updated;
    renderMaterials();
    glossary.setCourse(course);
  }

  async function courseAction(stage, action) {
    const updated = await api.courseStageAction(id, stage, action);
    if (!alive || !updated) return;
    course = updated;
    renderResources();
    glossary.setCourse(course);
  }

  // ---- uploads ----

  const unsubscribe = onUploadChange((cid, item, result, progressOnly) => {
    if (cid !== id || !alive || !course) return;
    const container = item.kind === 'lecture' ? lecUploads : item.kind === 'material' ? materialUploads : null;
    if (!container) return;
    if (progressOnly && updateUploadRow(container, item)) return;
    if (result) {
      if (item.kind === 'lecture') {
        if (lectures) upsertLecture(result);
        else reloadLectures();
        renderLectures();
        renderMaterials();
        reloadCourse();
      } else {
        course = result;
        renderResources();
        glossary.setCourse(course);
      }
      return;
    }
    preserveFocus(() => renderUploads(container, item.kind));
  });

  renderAll(); // skeleton until the first response
  load();
  // relative times: the header, and the rows' "running for", "… ago" (R78 low 3)
  const tick = setInterval(() => alive && lectures && course && (renderHead(), list.refreshTimes()), 60000);
  const onTitleLang = () => alive && lectures && renderLectures(); // the lecture title language changed in the settings
  window.addEventListener('titlelangchange', onTitleLang);
  const onKinds = () => alive && lectures && (renderLectures(), course && renderMaterials()); // a kind was added or renamed (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  return {
    update(r) {
      if (r.name !== 'course' || r.id !== id) return false;
      if (r.section === 'syllabus') {
        section = 'syllabus';
        if (course) openSection();
      }
      return true;
    },
    refresh: load,
    onJob(ev) {
      if (!course) return;
      if (ev.scope === 'lecture') {
        if (!lectures) return;
        if (!list.has(ev.scope_id)) {
          if (!probed.has(ev.scope_id)) {
            probed.add(ev.scope_id);
            refetchLecture(ev.scope_id);
          }
          return;
        }
        if (!list.setProgress(ev)) schedule('lec:' + ev.scope_id, () => refetchLecture(ev.scope_id));
      } else if (ev.scope === 'course' && ev.scope_id === id) {
        const st = course.stages.find((s) => s.stage === ev.stage);
        if (ev.stage === 'glossary_review') {
          if (!glossary.onJob(ev)) schedule('course', reloadCourse);
        } else if (st && st.status === ev.status && glossaryBar) glossaryBar.setProgress(ev.stage, ev.progress);
        else schedule('course', reloadCourse);
      } else if (ev.scope === 'material') {
        const m = course.materials.find((x) => x.id === ev.scope_id);
        if (!m) return;
        const st = m.stages.find((s) => s.stage === ev.stage);
        const ref = materialRefs.get(m.id);
        if (st && st.status === ev.status && ref) ref.bar.setProgress(ev.stage, ev.progress);
        else schedule('course', reloadCourse);
      }
    },
    // the lecture rows' waiting texts name devices and follow the power
    // policy: redraw them when that changes (the first snapshot may come
    // after the lectures)
    onResources(r) {
      const k = U.waitContextKey(r);
      if (k === waitKey) return;
      waitKey = k;
      if (lectures) renderLectures();
    },
    onChanged(ev) {
      if (ev.kind === 'course' && ev.id === id) {
        schedule('course', reloadCourse);
        schedule('lectures', reloadLectures);
      } else if (ev.kind === 'lecture' && !lectures) {
        schedule('lectures', reloadLectures);
      } else if (ev.kind === 'terms' && ev.id === id) {
        schedule('terms', glossary.reload);
      } else if (ev.kind === 'lecture') {
        schedule('lec:' + ev.id, () => refetchLecture(ev.id));
      }
      asrSlot.onChanged(ev);
    },
    destroy() {
      alive = false;
      glossary.destroy();
      asrSlot.destroy();
      dlSlot.destroy();
      clearInterval(tick);
      window.removeEventListener('titlelangchange', onTitleLang);
      window.removeEventListener('kindschange', onKinds);
      unsubscribe();
      for (const tm of timers.values()) clearTimeout(tm);
    },
  };
}
