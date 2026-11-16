// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// "#/" Dashboard (v1.1 A9): in progress, to-do (drafts, reviews, stale
// lectures, failures of the last 24 h), recently finished, today's usage and
// the course overview. Everything comes from GET /api/dashboard; actions
// re-fetch it afterwards (no optimistic changes).
//
// lecture kinds: a lecture is named by its course code and kind ("CSC1003 ·
// Week 3 · Tutorial") instead of W03L2. The lecture lists are grids whose
// rows share columns (CSS subgrid): the codes, the kinds and the titles
// start on the same vertical lines, and the date, length and cost of the
// recently finished list are right-aligned columns with a reserved place
// for the "to review" badge.
//
// SPEC-v3 V14.5: while a Colab session exists a line under the title says
// how it goes ("Colab · T4 运行中 · 使用时限剩余 2 小时 13 分 · 本次还能处理约
// 2 节"), updated in place from every resource snapshot.
//
// 3.1.0 M2: with nothing to transcribe with (no usable local device, Colab
// not signed in) a prominent "cannot transcribe yet" notice with its two
// entries comes first, also on the empty workbench (asr_ready.js).

import { api } from './api.js';
import { h, clear, btn, skeleton, errorBlock, preserveFocus, confirmDialog, toast, everySecond } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar, streamProgress, MODEL_RETRY_STAGES } from './pipeline.js';
import { bindContextMenu, menuButton } from './menu.js';
import { lectureMenuItems, exportItems, stageAction, modelChoiceDialog, batchError, launchCourseDrafts } from './actions.js';
import { confirmUpdate, confirmLaunchDrafts } from './update_confirm.js';
// launchCostMode (round 8 convention F6) through the namespace: the shared
// modules' line adds it, and a named import of it would not load before
import * as UC from './update_confirm.js';
import { courseCard, addCourseCard } from './course_card.js';
import * as U from './util.js';
import { t, has } from './i18n.js';
import { tariffUsageBlock, hasPeaks } from './tariff_ui.js';
import { zh } from './zh_display.js';
import { lectureTagText, lectureLabel } from './kinds.js';
import { createUpdateHint } from './update_hint.js';
import * as C from './settings_colab.js';
import { asrNotice } from './asr_ready.js';
import { downloadsHint } from './downloads.js';

const enc = encodeURIComponent;

// R76 L-8 (round 8 WD-11): one "全部重试" round at a time, known to every
// workbench view, also one opened again (the workbench left and come back
// to) while a round of the view before still sends its requests: its button
// and the rows' 重试 show busy until the round ends. views: each view's
// callback, told when a round starts and ends.
const retryRound = { running: false, views: new Set() };
function retryRoundChanged() {
  for (const fn of [...retryRound.views]) fn();
}

export function mountDashboard(root, ctx) {
  let data = null;
  let error = '';
  let settings = null;
  let alive = true;
  let busy = new Set(); // action keys in flight (buttons disabled meanwhile)
  const pipes = new Map(); // lecture id → {bar, lec, statusEl}
  const timers = new Map();
  // a round started or ended (retryRound): drawn busy; ended, read again
  // (the view that ran it reads again itself, run())
  const onRetryRound = () => {
    if (!alive) return;
    if (retryRound.running) {
      if (data) render();
    } else if (!busy.has('retryall')) load();
  };
  retryRound.views.add(onRetryRound);

  const el = h('div', { class: 'page page-dashboard' });
  root.append(el);
  // SPEC-v3 V8: a quiet line at the bottom when the desktop app has an update
  const updateHint = createUpdateHint({ variant: 'dashboard' });
  // SPEC-v3 V14.5: the Colab line (kept across renders, filled in place).
  // R86 L5 (r9 WD-05): a status is read out whole on any change unless told
  // otherwise (aria-atomic): read out by the nodes that change, the warning
  // is heard when it appears, and a number that changes alone (fillParts)
  const colabSlot = h('div', { class: 'dash-colab-slot', role: 'status', 'aria-atomic': 'false' });
  // 3.1.0 M2: "cannot transcribe yet" (kept across renders, filled in place)
  const asrSlot = asrNotice({ variant: 'page' });
  // SPEC-ext: "正在下载 2 节录播" while the browser extension's recordings download (downloads.js)
  const dlHint = downloadsHint({ ctx });

  async function load() {
    try {
      const d = await api.dashboard();
      if (!alive) return;
      data = d;
      error = '';
    } catch (e) {
      if (!alive) return;
      if (!data) error = e.message || t('common.load_failed');
    }
    render();
  }

  async function loadSettings() {
    try {
      const s = await api.settings();
      if (!alive) return;
      settings = s;
      if (data) render();
    } catch {
      // optional: the usage card works without the budget line
    }
  }

  function schedule(key, fn, ms = 400) {
    clearTimeout(timers.get(key));
    timers.set(
      key,
      setTimeout(() => {
        timers.delete(key);
        fn();
      }, ms),
    );
  }

  // ---- render ----

  function render() {
    const settle = holdScroll();
    preserveFocus(() => {
      clearPage();
      pipes.clear();
      if (!data && !error) {
        const sk = (k, n) => h('section', { class: `card card-${k} is-loading` }, h('div', { class: 'card-hd' }, h('div', { class: 'skel skel-title' })), h('div', { class: 'card-bd' }, skeleton(n)));
        el.append(head(null), h('div', { class: 'dash' }, h('div', { class: 'dash-col dash-main' }, sk('ip', 6), sk('recent', 5)), h('div', { class: 'dash-col dash-side' }, sk('todo', 5), sk('usage', 4))));
        return;
      }
      if (error) {
        el.append(head(null), h('section', { class: 'card' }, h('div', { class: 'card-bd' }, errorBlock(error, retry))));
        return;
      }
      if (data.courses.length === 0) {
        el.append(asrSlot.el, guideNotice() || '', emptyState(), updateHint.el);
        return;
      }
      fillColab();
      el.append(
        head(data),
        asrSlot.el,
        // one notice at a time, the guide's first: it sets up the model
        // services the roles need too (round 8 WD-04)
        guideNotice() || rolesNotice() || '',
        colabSlot,
        dlHint.el,
        devicePrompt() || '',
        h(
          'div',
          { class: 'dash' },
          h('div', { class: 'dash-col dash-main' }, inProgressCard(), recentCard()),
          h('div', { class: 'dash-col dash-side' }, todoCard(), (usageEl = usageCard())),
          coursesCard(),
        ),
        updateHint.el,
      );
    });
    settle();
  }

  // T28 S1 (r9 WD9-01): the page is drawn anew on every refresh. Chromium
  // lays the page out as the control with the focus is taken out of it;
  // taken out in order, the header and the notices above the cards were
  // gone by then and the cards still there, and its scroll anchoring moved
  // the page up by their height (T28: 174 px), which nothing moved back
  // once they were drawn again (with the focus in the navigation, or on
  // none, nothing is laid out until the page is whole): the part holding
  // the focus goes first, while everything around it is still in place.
  function clearPage() {
    for (let n = document.activeElement; n && n !== el; n = n.parentNode) {
      if (n.parentNode !== el) continue;
      n.remove();
      break;
    }
    clear(el);
  }

  // The control with focus key k on the workbench (the first one).
  const byKey = (k) => el.querySelector(`[data-fkey="${CSS.escape(k)}"]`);

  // The box of n when it is on the screen (drawn, at least in part in the
  // window), else null.
  function onScreen(n) {
    const r = n.getBoundingClientRect();
    return r.bottom > r.top && r.bottom > 0 && r.top < (window.innerHeight || 0) ? r : null;
  }

  // What a redraw keeps in place (WD9-01): the control with the focus, when
  // it is on the screen; with the focus on none (the page's body, the page),
  // the first control on the screen that has a focus key. → {key, top}, or
  // null.
  function scrollAnchor(focused) {
    if (focused) {
      const key = focused.dataset ? focused.dataset.fkey : null;
      const r = key && byKey(key) === focused ? onScreen(focused) : null;
      if (r) return { key, top: r.top };
      if (key) return null;
    }
    for (const n of el.querySelectorAll('[data-fkey]')) {
      const r = onScreen(n);
      if (r && r.top >= 0) return { key: n.dataset.fkey, top: r.top };
    }
    return null;
  }

  // WD9-01: the scroll position across a redraw (render). The anchor
  // (scrollAnchor), found again by its focus key, stays where it was on the
  // screen: rows added or gone above it move the page with them, as the
  // browser's anchoring does for a page changed in place (it cannot follow
  // one drawn anew). Without one the page keeps its position; at the top it
  // stays at the top. With the focus outside the workbench (an open menu,
  // whose scrolling closes it; a dialog; the navigation) the position is
  // only kept. Measured once the page is drawn, before the next frame:
  // nothing is seen to move, and a page that did not change does not scroll.
  // T28 G4 (WD9-03): a control the redraw handed the focus to (its own is
  // gone: data-fnext; or it waited, disabled, on the page) is brought into
  // view when it is out of it; R88 L3: not when the user scrolled the page
  // while the focus waited (waitY), as no redraw pulls the user back.
  function holdScroll() {
    if (typeof window.scrollTo !== 'function') return () => {};
    const y = window.scrollY || 0;
    const before = document.activeElement;
    const inPage = !!before && before !== el && el.contains(before);
    const free = onNoControl(before);
    const anchor = inPage || free ? scrollAnchor(inPage ? before : null) : null;
    const key = inPage && before.dataset ? before.dataset.fkey : null;
    // the page moved since the focus began to wait, not by a redraw: the user
    const userScrolled = free && waitY !== null && Math.abs(y - waitY) >= 1;
    return () => {
      if (!el.isConnected) return;
      let want = y;
      if (anchor && y > 0) {
        const same = byKey(anchor.key);
        const r = same ? same.getBoundingClientRect() : null;
        if (r && r.bottom > r.top) want = (window.scrollY || 0) + r.top - anchor.top;
      }
      want = Math.max(0, want);
      if (Math.abs(want - (window.scrollY || 0)) >= 1) window.scrollTo({ left: window.scrollX || 0, top: want, behavior: 'instant' });
      // what the redraw moved the page by is not the user's scrolling: its
      // own scrolling, and the browser's (a page drawn shorter is cut back
      // to its new end; R88b L4)
      if (waitY !== null) waitY += (window.scrollY || 0) - y;
      const now = document.activeElement;
      const handed = (inPage || free) && !!now && now !== before && el.contains(now) && !(key && now.dataset && now.dataset.fkey === key);
      if (handed && !userScrolled && !fullyShown(now) && typeof now.scrollIntoView === 'function') now.scrollIntoView({ block: 'nearest' });
      // a control of the page lost the focus to the page (disabled while its
      // request runs): the wait begins; the focus on a control again ends it
      if (inPage && onNoControl(now)) waitY = window.scrollY || 0;
      else if (now && now !== el && el.contains(now)) waitY = null;
    };
  }

  // R88 L3: the scroll position when the focus began to wait on the page,
  // moved along with the redraws' own scrolling; null while not waiting.
  let waitY = null;

  // The focus on no control: the page's body, or the page itself (app.js
  // focuses main once a view is mounted).
  function onNoControl(n) {
    return !n || (typeof n.contains === 'function' && n.contains(el));
  }

  function fullyShown(n) {
    const r = n.getBoundingClientRect();
    return r.top >= 0 && r.bottom <= (window.innerHeight || 0);
  }

  function retry() {
    error = '';
    data = null;
    render();
    load();
  }

  function head(d) {
    const now = new Date();
    const parts = [U.fmtDayWithWeekday(now)];
    if (d) {
      if (d.in_progress.length) parts.push(t('dashboard.head.in_progress', { n: d.in_progress.length }));
      if (d.draft_total) parts.push(t('dashboard.head.drafts', { n: d.draft_total }));
      if (d.review_total) parts.push(t('dashboard.head.reviews', { n: d.review_total }));
      if (!d.in_progress.length && !d.draft_total && !d.review_total && !d.stale_total && !d.failures.length && !(d.earlier_failed_total > 0)) parts.push(t('dashboard.head.all_clear'));
    }
    return h(
      'header',
      { class: 'page-head' },
      h('div', { class: 'page-titles' }, h('h1', { class: 'page-title' }, t('dashboard.title')), h('p', { class: 'page-sub' }, parts.join(' · '))),
      h('div', { class: 'page-actions' }, btn(t('course.add'), { icon: 'plus', onclick: ctx.newCourse, fkey: 'dash:add' })),
    );
  }

  // "Colab · T4 运行中 · 使用时限剩余 2 小时 13 分 · 本次还能处理约 2 节"
  // T24 S6: a runtime that does not answer but is still billed, one being
  // released that does not go, or one an earlier run left: a warning with
  // the resource panel's sentence (the panel may be closed), "立即关机"
  // while the session can still be shut down, and the way to end it by hand
  // while the program keeps trying.
  // Redrawn only when what it shows changed: every resource snapshot (2 s)
  // calls it, and its button must not be replaced under the pointer. R76
  // L-6 (round 8 WD-10): drawn anew only when its kind or its button
  // changes (shown, a warning or the calm line, 立即关机 and its busy
  // state); new numbers alone (the compute units spent, the time left) only
  // change its texts in place, so the focus stays on 立即关机 and a screen
  // reader hears only the text that changed.
  let colabBusy = false;
  let colabSig = null;
  let colabText = null; // {line, text}: the texts of what is drawn, changed in place
  const setText = (el, s) => {
    if (el && el.textContent !== s) el.textContent = s;
  };
  // R86 L5 (r9 WD-05): each part of a text (a "·" segment of the line, a
  // sentence of the warning) is a span of its own, its text changed in
  // place: the live region (colabSlot) reads out only the part whose number
  // changed. el gets the parts of s (the parts joined, as shown), with what
  // joins them between; drawn anew only when the parts or their joins
  // change. Parts not found in s in order: s whole, in one span.
  const partsDrawn = new WeakMap(); // el → [{sep, span}]
  function fillParts(el, parts, s) {
    let segs = [];
    let at = 0;
    for (const p of parts) {
      const i = p ? s.indexOf(p, at) : -1;
      if (i < 0) {
        segs = null;
        break;
      }
      segs.push({ sep: s.slice(at, i), text: p });
      at = i + p.length;
    }
    if (!segs || !segs.length || at !== s.length) segs = [{ sep: '', text: s }];
    const old = partsDrawn.get(el);
    if (old && old.length === segs.length && old.every((o, k) => o.sep === segs[k].sep)) {
      old.forEach((o, k) => setText(o.span, segs[k].text));
      return;
    }
    clear(el);
    const drawn = [];
    for (const g of segs) {
      if (g.sep) el.append(g.sep);
      const span = h('span', null, g.text);
      el.append(span);
      drawn.push({ sep: g.sep, span });
    }
    partsDrawn.set(el, drawn);
  }
  function fillColab() {
    const res = ctx.resources();
    const r = res && res.remote;
    const active = !!r && C.ACTIVE_STATES.has(r.state);
    const card = active ? C.colabCard(res) : null;
    const billing = active ? C.runtimeWarning(r, card ? card.jobs : []) : null;
    // with the session's own warning, a runtime of before is said to be
    // another one (as in the resource panel)
    const lo = r && r.leftover;
    const earlier = !!billing && !!lo && lo.code === 'colab.release_failed';
    const leftover = !lo ? '' : earlier ? (lo.accelerator ? t('resources.remote.leftover_earlier', { accelerator: lo.accelerator }) : t('resources.remote.leftover_earlier_any')) : C.leftoverText(lo);
    const shown = active || !!leftover;
    // R76 I-5: a session left without its sign-in may still run (not "could
    // not be released"); its own title (the copy line's key, round 8)
    const lineParts = !shown ? [] : active ? colabParts(r) : [t(lo && lo.code === 'colab.leftover_session' ? 'dashboard.colab.leftover_session_title' : 'dashboard.colab.leftover_title')];
    const line = lineParts.join(' · ');
    const releasing = shown && r.state === 'stopping';
    const byHand = (billing && releasing) || (r && r.leftover && r.leftover.code === 'colab.release_failed');
    const manual = byHand ? t('resources.remote.manual_end') : '';
    // T28 suggestion 11 (r9 WD-07): an earlier runtime not released either
    // is folded away under a line of its own (dashboard.colab.earlier_summary),
    // with the way to end it by hand when that is about it (no session being
    // released); this session's warning and its button stay in sight
    const textParts = (earlier ? [billing, releasing ? manual : ''] : billing || leftover ? [billing, leftover, manual] : []).filter(Boolean);
    const text = textParts.length ? U.sentenceJoin(textParts) : '';
    const earlierParts = earlier ? [leftover, releasing ? '' : manual].filter(Boolean) : [];
    const earlierText = earlierParts.length ? U.sentenceJoin(earlierParts) : '';
    const withNow = !!billing && !releasing;
    // R76 L-5: busy also while the resource panel's own button runs
    const busyNow = colabBusy || (ctx.colabBusy ? ctx.colabBusy() : false);
    const sig = JSON.stringify([shown, !!text, withNow, busyNow, earlier]);
    if (sig === colabSig && colabText) {
      fillParts(colabText.line, lineParts, line);
      if (colabText.text) fillParts(colabText.text, textParts, text);
      if (colabText.earlier) fillParts(colabText.earlier, earlierParts, earlierText);
      return;
    }
    // the folded part opened stays open when the warning is drawn anew
    const wasOpen = !!(colabText && colabText.details && colabText.details.open);
    colabSig = sig;
    colabText = null;
    clear(colabSlot);
    colabSlot.hidden = !shown;
    if (!shown) return;
    if (!text) {
      const lineEl = h('span');
      fillParts(lineEl, lineParts, line);
      colabSlot.append(h('p', { class: 'dash-colab', title: t('resources.remote.title') }, icon('server', { size: 15 }), lineEl));
      colabText = { line: lineEl, text: null };
      return;
    }
    const now = withNow ? btn(t('resources.remote.now'), { size: 'sm', kind: 'danger', icon: 'ban', disabled: busyNow, fkey: 'dash:colab:now', fnext: ['dash:add'], onclick: shutdownNow }) : null;
    const titleEl = h('p', { class: 'asr-notice-title' });
    fillParts(titleEl, lineParts, line);
    const textEl = h('p', { class: 'asr-notice-text' });
    fillParts(textEl, textParts, text);
    let earlierEl = null;
    let details = null;
    if (earlier) {
      earlierEl = h('p', { class: 'asr-notice-text' });
      fillParts(earlierEl, earlierParts, earlierText);
      // card-errdetail: the folded detail's look (a stage card's error); its
      // summary found again by its focus key when the page is drawn anew
      // (R88 M1; as pipeline.js's folded details)
      details = h('details', { class: 'dash-colab-earlier card-errdetail' }, h('summary', { dataset: { fkey: 'dash:colab:earlier' } }, t('dashboard.colab.earlier_summary')), earlierEl);
      details.open = wasOpen;
    }
    colabSlot.append(
      h(
        'section',
        { class: 'asr-notice dash-colab-warn', 'aria-label': t('resources.remote.title') },
        h('span', { class: 'asr-notice-icon', 'aria-hidden': 'true' }, icon('triangle-alert', { size: 18 })),
        h('div', { class: 'asr-notice-main' }, titleEl, textEl, details),
        now ? h('div', { class: 'asr-notice-actions' }, now) : null,
      ),
    );
    colabText = { line: titleEl, text: textEl, earlier: earlierEl, details };
  }

  // "立即关机" from the warning: the resource panel's own action when the
  // page has one (app.js ctx.colabShutdown: one confirmation, one busy
  // state for both buttons, R76 L-5), else its confirmation and request.
  async function shutdownNow() {
    if (ctx.colabShutdown) {
      await ctx.colabShutdown('now');
      return;
    }
    if (colabBusy) return;
    if (!(await C.confirmShutdownNow())) return;
    colabBusy = true;
    preserveFocus(fillColab);
    try {
      await C.shutdownColab('now');
      toast(t('resources.remote.now_done'), 'success');
    } catch (e) {
      toast(e.message, 'error');
    }
    colabBusy = false;
    if (alive && colabSlot.isConnected) preserveFocus(fillColab);
  }

  // The parts of the Colab line, shown joined by " · " (fillParts).
  function colabParts(r) {
    const texts = [t('dashboard.colab.head', { accelerator: r.accelerator, state: C.stateName(r.state) })];
    if (r.window_total_sec > 0) texts.push(t('dashboard.colab.window_left', { time: C.durText(Math.max(0, r.window_total_sec - (r.window_used_sec || 0))) || C.durText(0) }));
    if (r.state !== 'stopping') texts.push(r.lectures_left > 0 ? t('dashboard.colab.lectures_left', { n: r.lectures_left }) : t('dashboard.colab.lectures_none'));
    // v3.2.x: while releasing, no step of a lecture handed back; a lecture
    // retrying in place says so instead of its (frozen) step
    const rc = r.state !== 'stopping' && r.current && r.current.reconnect && r.current.reconnect.attempt > 0 ? C.reconnectText({ attempt: r.current.reconnect.attempt, of: r.current.reconnect.of }) : null;
    if (rc) texts.push(rc.text);
    else if (r.state !== 'stopping' && r.current && r.current.step) texts.push(t('dashboard.colab.step', { step: C.stepName(r.current.step) }));
    return texts;
  }

  // T22 S5: a role the lectures need has no usable model (its provider was
  // deleted, it was never chosen): the settings page's alert, here too,
  // with the way to the role settings. From the settings the page reads
  // anyway (the budget line); the rule is the settings page's
  // (view_settings.js roleBroken): optional roles never count, nor any role
  // while no launch role is set up (transcription only is a choice). A copy
  // of it: the two must change together (r7_dashboard_test.go pins them).
  const LAUNCH_ROLES = ['summary', 'repair', 'translate'];
  const OPTIONAL_ROLES = new Set(['glossary_janitor', 'chapters', 'organizer']);
  function rolesNotice() {
    const roles = settings && Array.isArray(settings.roles) ? settings.roles : [];
    const transcribeOnly = LAUNCH_ROLES.every((id) => roles.some((r) => r.role === id && !r.configured));
    const broken = transcribeOnly ? [] : roles.filter((r) => r && !r.configured && !OPTIONAL_ROLES.has(r.role));
    if (!broken.length) return null;
    const names = U.listJoin(broken.map((r) => t('settings.quote', { name: has(`role.${r.role}`) ? U.roleName(r.role) : r.label || r.role })));
    const text = t(broken.some((r) => LAUNCH_ROLES.includes(r.role)) ? 'settings.alert.roles_broken_launch' : 'settings.alert.roles_broken', { roles: names });
    return h(
      'section',
      // no live role: the workbench is drawn again every minute and on events
      { class: 'asr-notice dash-roles', 'aria-label': t('dashboard.roles.title') },
      h('span', { class: 'asr-notice-icon', 'aria-hidden': 'true' }, icon('circle-alert', { size: 18 })),
      h('div', { class: 'asr-notice-main' }, h('p', { class: 'asr-notice-title' }, t('dashboard.roles.title')), h('p', { class: 'asr-notice-text' }, text)),
      h('div', { class: 'asr-notice-actions' }, h('a', { class: 'btn btn-sm btn-primary', href: '#/settings/roles', dataset: { fkey: 'dash:roles:fix' } }, h('span', { class: 'btn-label' }, t('settings.alert.fix')))),
    );
  }

  // T22 G10, G13 (round 8 WD-04): the setup guide left before its end
  // ("leave setup" lands here) is continued from the workbench too, not only
  // from the top of the settings page: its words and its button there, by
  // the same rule (U.guideLeft, round 8 interface F3). The guide's state is
  // what the page shell has (ctx.guide: app.js reads it where it already
  // asks, and again once the guide is left); not known, nothing is shown.
  // Where a role is not set up too, this one is shown alone: continuing the
  // guide sets up the model services the roles need, and two calls to
  // action at the top would compete.
  function guideNotice() {
    if (!ctx.guide || !U.guideLeft(ctx.guide())) return null;
    return h(
      'section',
      // no live role: the workbench is drawn again every minute and on events
      { class: 'asr-notice dash-guide' },
      h('span', { class: 'asr-notice-icon', 'aria-hidden': 'true' }, icon('graduation-cap', { size: 18 })),
      h('div', { class: 'asr-notice-main' }, h('p', { class: 'asr-notice-text' }, t('settings.alert.guide_left'))),
      h('div', { class: 'asr-notice-actions' }, h('a', { class: 'btn btn-sm btn-primary', href: '#/onboarding', dataset: { fkey: 'dash:guide:go' } }, h('span', { class: 'btn-label' }, t('settings.alert.guide_continue')))),
    );
  }

  // SPEC-v2 I9: a calm, closable notice about new transcription devices that
  // were never tested ("test now" opens the device's test button).
  function devicePrompt() {
    const ps = data.device_prompts || [];
    if (!ps.length) return null;
    const text =
      ps.length === 1
        ? t('devices.prompt.text', { device: ps[0].label })
        : t('devices.prompt.text_many', { devices: U.listJoin(ps.map((p) => t('settings.quote', { name: p.label }))) });
    const key = 'dash:devprompt';
    return h(
      'section',
      { class: 'dash-prompt', 'aria-label': t('devices.prompt.title') },
      h('span', { class: 'dash-prompt-icon', 'aria-hidden': 'true' }, icon('gauge', { size: 18 })),
      h('div', { class: 'dash-prompt-main' }, h('p', { class: 'dash-prompt-text' }, text), h('p', { class: 'dash-prompt-hint' }, t('devices.prompt.hint'))),
      h(
        'div',
        { class: 'dash-prompt-actions' },
        h('a', { class: 'btn btn-sm btn-primary', href: `#/settings/asr/${enc(ps[0].device_id)}`, dataset: { fkey: `${key}:go` } }, h('span', { class: 'btn-label' }, t('devices.prompt.go'))),
        // disabled while it runs, then the notice is gone: the focus goes to
        // the page's own action (L14)
        btn(t('devices.prompt.dismiss'), { size: 'sm', kind: 'ghost', disabled: busy.has(key), fkey: `${key}:dismiss`, fnext: ['dash:add'], onclick: () => run(key, () => Promise.all(ps.map((p) => api.dismissDevicePrompt(p.device_id)))) }),
      ),
    );
  }

  function card(cls, ic, title, count, tools, body) {
    return h(
      'section',
      { class: `card ${cls}`, 'aria-label': title },
      h(
        'header',
        { class: 'card-hd' },
        h('h2', { class: 'card-title' }, icon(ic, { size: 16 }), h('span', null, title), count != null ? h('span', { class: 'card-count' }, String(count)) : null),
        tools ? h('div', { class: 'card-tools' }, tools) : null,
      ),
      h('div', { class: 'card-bd' }, body),
    );
  }

  function emptyLine(ic, text) {
    return h('div', { class: 'card-empty' }, icon(ic, { size: 18 }), h('span', null, text));
  }

  // The course code and kind cells of a lecture row (lecture, review group or
  // failure: all carry kind, week, kind_n, show_n).
  function lref(code, item) {
    const tag = lectureTagText(item);
    return [h('span', { class: 'lref-code' }, code || ''), h('span', { class: 'lref-tag', title: tag }, tag)];
  }

  const menuHandlers = { onLectures: () => load(), onDeleted: () => load() };

  // ---- in progress ----

  function inProgressCard() {
    const list = data.in_progress;
    return card(
      'card-ip',
      'activity',
      t('dashboard.in_progress.title'),
      list.length || null,
      null,
      list.length ? h('div', { class: 'ip-list lgrid' }, list.map(ipRow)) : emptyLine('circle-check', t('dashboard.in_progress.empty')),
    );
  }

  function statusFor(lec) {
    const sum = U.lectureSummary(lec.stages, { sys: ctx.system(), res: ctx.resources(), staleText: U.lectureStaleText(lec), draft: lec.draft });
    let text = sum.text;
    if (sum.kind === 'running' && sum.stage.started_at) {
      const ms = Date.now() - new Date(sum.stage.started_at).getTime();
      if (ms > 0) text = t('dashboard.in_progress.running', { status: text, duration: U.fmtDuration(ms) });
    }
    return { text, tone: sum.tone };
  }

  function ipRow(lec, i, list) {
    // a card's action (取消) often takes the lecture off this list: the focus
    // goes to the next row, else the one before (L14)
    const away = [list[i + 1], list[i - 1]].filter(Boolean).map((x) => `dash:open:${x.id}`);
    const bar = pipelineBar(lec.stages, {
      key: `dash:${lec.id}`,
      fnext: [...away, 'dash:add'],
      lecture: lec.id, // a remote asr's live steps (pipeline.js)
      sys: ctx.system,
      res: ctx.resources,
      stream: lec.stream, // stream-parallel: the playback file with prep
      onAction: (stage, action) => stageAction(lec, stage, action, true).then((r) => r && load()),
      // P2 hotspot 3 (round 8 WD-02): a card is built when it first opens;
      // one describing its segment (an error, a waiting reason) at once
      lazyCards: true,
    });
    const st = statusFor(lec);
    const statusEl = h('div', { class: `ip-status tone-${st.tone}`, title: st.text }, st.text); // truncated: full text on hover (P14.5)
    pipes.set(lec.id, { bar, lec, statusEl });
    // a retry's countdown goes on by the second (3.2.0 L12)
    if (U.backoffCounting(lec.stages))
      everySecond(statusEl, () => {
        const s = statusFor(lec);
        statusEl.textContent = s.text;
        statusEl.title = s.text;
        return U.backoffCounting(lec.stages);
      });
    const row = h(
      'div',
      { class: 'ip-row lgrid-row' },
      lref(lec.course_code, lec),
      h('div', { class: 'ip-main' }, h('a', { class: 'ip-link', href: `#/lecture/${enc(lec.id)}`, title: `${lec.course_name} · ${zh(lec.title)}`, dataset: { fkey: `dash:open:${lec.id}` } }, zh(lec.title)), statusEl),
      h('div', { class: 'ip-pipe' }, bar),
    );
    bindContextMenu(row, () => lectureMenuItems(lec, menuHandlers));
    return row;
  }

  // ---- to do ----

  function todoCard() {
    const d = data;
    const groups = [];
    if (d.draft_total) {
      groups.push(
        todoGroup(
          'file-text',
          t('dashboard.todo.drafts'),
          d.draft_total,
          btn(t('course.launch_drafts'), { size: 'sm', kind: 'primary', icon: 'play', disabled: busy.has('drafts'), onclick: launchAllDrafts, fkey: 'dash:drafts', fnext: todoNext('drafts') }),
          h('div', { class: 'todo-sub' }, d.drafts.map((c) => h('a', { class: 'todo-chip', href: `#/course/${enc(c.course_id)}` }, c.course_code || c.course_name, h('b', null, String(c.count))))),
        ),
      );
    }
    if (d.review_total) {
      const shown = d.reviews.slice(0, 5);
      groups.push(
        todoGroup(
          'list-checks',
          t('dashboard.todo.reviews'),
          d.review_total,
          null,
          h(
            'ul',
            { class: 'todo-list lgrid lgrid-review' },
            shown.map((r) =>
              h(
                'li',
                { class: 'lgrid-row' },
                h(
                  'a',
                  { class: 'todo-link lgrid-row', href: `#/lecture/${enc(r.lecture_id)}/review`, title: `${r.course_name} · ${r.lecture_title}`, dataset: { fkey: `dash:review:${r.lecture_id}` } },
                  lref(r.course_code, r),
                  h('span', { class: 'todo-link-title' }, r.lecture_title),
                  h('span', { class: 'todo-num' }, String(r.count)),
                  icon('chevron-right', { size: 14, class: 'todo-go' }),
                ),
              ),
            ),
            d.reviews.length > shown.length ? h('li', { class: 'todo-more' }, t('dashboard.todo.more_reviews', { n: d.reviews.length - shown.length })) : null,
          ),
          t('dashboard.todo.reviews_note'),
        ),
      );
    }
    if (d.stale_total) {
      groups.push(
        todoGroup(
          'refresh-cw',
          t('dashboard.todo.stale'),
          d.stale_total,
          btn(t('dashboard.todo.update_all'), { size: 'sm', icon: 'refresh-cw', disabled: busy.has('stale'), onclick: updateAllStale, fkey: 'dash:stale', fnext: todoNext('stale') }),
          h('div', { class: 'todo-sub' }, d.stale.map((c) => h('a', { class: 'todo-chip todo-chip-warn', href: `#/course/${enc(c.course_id)}` }, c.course_code || c.course_name, h('b', null, String(c.count))))),
          t('dashboard.todo.stale_note'),
        ),
      );
    }
    // T24 S2: the failures of more than 24 hours ago are counted below the
    // list (earlier_failed_total, absent from an older server); alone they
    // make a group of their own
    const earlier = d.earlier_failed_total > 0 ? d.earlier_failed_total : 0;
    if (d.failures.length || earlier) {
      // T24 S1: "全部重试" for the failures a plain retry may help (two or more).
      // R76 M-1: a translation retried with the current settings loses the
      // lecture's translations (it is translated again, and billed): the
      // model question says so itself (actions.js modelChoiceDialog), in a
      // batch's words (R82 D-L1: not on the button too)
      const plain = d.failures.filter(plainRetry);
      groups.push(
        todoGroup(
          'circle-alert',
          d.failures.length ? t('dashboard.todo.failures') : t('dashboard.todo.failures_earlier'),
          d.failures.length || earlier,
          plain.length > 1 ? btn(t('dashboard.todo.retry_all'), { size: 'sm', icon: 'rotate-cw', disabled: busy.has('retryall') || retryRound.running, onclick: retryAllFailures, fkey: 'dash:retryall', fnext: todoNext('fail') }) : null,
          [d.failures.length ? h('ul', { class: 'todo-list lgrid lgrid-fail' }, failureList()) : null, earlier ? earlierFailures(earlier, d.failures.length > 0) : null],
          null,
          'is-error',
        ),
      );
    }
    return card('card-todo', 'inbox', t('dashboard.todo.title'), null, null, groups.length ? h('div', { class: 'todo' }, groups) : emptyLine('circle-check', t('dashboard.todo.empty')));
  }

  // Where the focus goes from a to-do group's button, disabled while it runs
  // and gone with its group once done (L14): the next to-do item, else the
  // one before, else the page's own action.
  function todoNext(which) {
    const d = data;
    const review = d.review_total && d.reviews.length ? `dash:review:${d.reviews[0].lecture_id}` : null;
    const fail = d.failures.length ? failKey(d.failures[0]) : null;
    // "全部重试": the failures it leaves (not retried), then the other groups
    if (which === 'fail') return [...d.failures.filter((f) => !plainRetry(f)).map(failKey), review, d.stale_total ? 'dash:stale' : null, d.draft_total ? 'dash:drafts' : null, 'dash:add'].filter(Boolean);
    const order = which === 'drafts' ? [review, d.stale_total ? 'dash:stale' : null, fail] : [fail, review, d.draft_total ? 'dash:drafts' : null];
    return [...order.filter(Boolean), 'dash:add'];
  }

  function failKey(f) {
    return `dash:fail:${f.lecture_id}:${f.stage}`;
  }

  function todoGroup(ic, title, count, action, body, note = null, cls = '') {
    return h(
      'div',
      { class: 'todo-group ' + cls },
      h('div', { class: 'todo-head' }, h('span', { class: 'todo-icon' }, icon(ic, { size: 15 })), h('span', { class: 'todo-title' }, title), h('span', { class: 'todo-count' }, String(count)), action ? h('span', { class: 'todo-action' }, action) : null),
      body,
      note ? h('p', { class: 'todo-note' }, note) : null,
    );
  }

  // "还有 N 节更早失败" (T24 S2) and the courses they are in, each a link to
  // its page: a course's failed lectures (failed_count) less those listed
  // above.
  function earlierFailures(n, listed) {
    const shown = new Map(); // course id → its lectures listed above
    for (const f of data.failures) {
      if (!shown.has(f.course_id)) shown.set(f.course_id, new Set());
      shown.get(f.course_id).add(f.lecture_id);
    }
    const chips = [];
    for (const c of data.courses) {
      const k = (c.failed_count || 0) - (shown.has(c.id) ? shown.get(c.id).size : 0);
      if (k > 0) chips.push(h('a', { class: 'todo-chip', href: `#/course/${enc(c.id)}`, title: t('dashboard.todo.earlier_course', { course: c.name, n: k }) }, c.code || c.name, h('b', null, String(k))));
    }
    return [h('p', { class: 'todo-note' }, listed ? t('dashboard.todo.earlier_failed', { n }) : t('dashboard.todo.earlier_note')), chips.length ? h('div', { class: 'todo-sub' }, chips) : null];
  }

  // The failures by reason (T24 S1): the failures whose error reads the same
  // follow each other, in the order of the first of them (the newest first);
  // a reason of two or more lectures heads them, said once, and their rows
  // leave it out. → the rows' order (the focus fallbacks follow it).
  function failureGroups() {
    const byText = new Map();
    const out = [];
    for (const f of data.failures) {
      const text = failText(f);
      let g = text ? byText.get(text) : null;
      if (!g) {
        g = { text, items: [] };
        if (text) byText.set(text, g);
        out.push(g);
      }
      g.items.push(f);
    }
    return out;
  }

  function failureList() {
    const groups = failureGroups();
    const order = groups.flatMap((g) => g.items);
    const out = [];
    for (const g of groups) {
      const shared = g.items.length > 1;
      if (shared) out.push(reasonHead(g));
      for (const f of g.items) out.push(failureRow(f, order.indexOf(f), order, shared));
    }
    return out;
  }

  // The head of the lectures one reason failed: the reason, how many, and
  // what to do where a plain retry does not help.
  function reasonHead(g) {
    const hint = failHint(g.items[0]);
    return h(
      'li',
      { class: 'todo-more fail-reason' },
      h('span', { class: 'tone-err' }, g.text),
      ' · ',
      t('dashboard.failure.group_count', { n: g.items.length }),
      hint ? h('div', { class: 'fail-hint' }, hint) : null,
    );
  }

  function failureRow(f, i, list, shared = false) {
    const key = `retry:${f.lecture_id}:${f.stage}`;
    // "重试" is disabled while it runs (the focus goes to the row's title),
    // then the row is gone once the stage runs again: the next failure, else
    // the one before, else the other to-do actions (L14)
    const away = [...[list[i + 1], list[i - 1]].filter(Boolean).map(failKey), 'dash:stale', 'dash:drafts', 'dash:add'];
    return h(
      'li',
      { class: 'fail-row lgrid-row' },
      lref(f.course_code, f),
      h('a', { class: 'fail-title', href: `#/lecture/${enc(f.lecture_id)}`, title: `${f.course_name} · ${lectureLabel({ ...f, title: f.lecture_title })}`, dataset: { fkey: failKey(f), fnext: away.join(' ') } }, f.lecture_title),
      h('span', { class: 'fail-end' }, failAction(f, key, away)),
      // the stage, course and time, and the error, under the title
      h(
        'div',
        { class: 'fail-sub' },
        h('div', { class: 'fail-meta' }, h('span', { class: 'tone-err' }, t('dashboard.failure.stage', { stage: U.stageName(f.stage) })), ' · ', f.course_name, f.finished_at ? ` · ${U.relTime(f.finished_at)}` : ''),
        shared ? null : failWhy(f),
      ),
    );
  }

  // The failure's action (T24 B1): "重试"; for a failure that the same
  // request cannot get past and that is billed again (U.retryNeedsModel: a
  // refusal, the total time limit) "换模型重试…" in its place, which opens
  // the lecture's model dialog; none for a damaged file (U.retryFutile: the
  // row says what to do instead).
  const canRetry = (f) => (f.allowed_actions || []).includes('retry') && !U.retryFutile(f);
  const modelRetry = (f) => canRetry(f) && U.retryNeedsModel(f) && MODEL_RETRY_STAGES.has(f.stage);
  const plainRetry = (f) => canRetry(f) && !U.retryNeedsModel(f);
  function failAction(f, key, away) {
    if (!canRetry(f)) return null;
    if (U.retryNeedsModel(f)) {
      if (!modelRetry(f)) return null;
      const mk = `retry_model:${f.lecture_id}:${f.stage}`;
      return btn(t('common.action.retry_model'), { size: 'sm', icon: 'rotate-cw', disabled: busy.has(mk), fkey: 'dash:' + mk, fnext: [failKey(f), ...away], onclick: () => retryWithModel(f, mk) });
    }
    // busy too while a "全部重试" round runs: it retries this failure (WD-11)
    return btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', disabled: busy.has(key) || retryRound.running, fkey: 'dash:' + key, fnext: [failKey(f), ...away], onclick: () => retryFailure(f, key) });
  }

  // Why it failed, in full (T24 G1: the way out ends the text, a cut would
  // take it away), and what to do where the row has no plain retry: a
  // damaged file (the lecture page's sentence), another model.
  function failWhy(f) {
    // the coded error (v2 P14.3), else the job's text
    const err = failError(f);
    const hint = failHint(f);
    return [
      err ? h('div', { class: 'fail-meta fail-why', title: err }, h('span', { class: 'tone-err' }, failText(f))) : null,
      hint ? h('div', { class: 'fail-meta fail-hint' }, hint) : null,
    ];
  }

  // T28 suggestion 2 (r9 WD-10, round 9 convention C2): a failure is said as
  // the lecture page says its stage's (U.stageErrorText, which knows the
  // stage: a refusal or the total time limit of a stage that can retry
  // with another model says only that)
  const failError = (f) => U.stageErrorText({ stage: f.stage, status: 'failed', error_info: f.error_info, error: f.error || '' });
  // the first line of the error (what the rows of one reason share)
  const failText = (f) => failError(f).split('\n')[0];
  const failHint = (f) => (U.retryFutile(f) ? t('common.pipe.corrupt_hint') : modelRetry(f) ? t('dashboard.failure.model_hint') : '');

  async function run(key, fn) {
    if (busy.has(key)) return;
    busy.add(key);
    render();
    try {
      await fn();
    } catch {
      // shown by the API layer
    } finally {
      busy.delete(key);
      if (alive) load();
    }
  }

  function retryFailure(f, key) {
    run(key, () => stageAction({ id: f.lecture_id }, f.stage, 'retry', true));
  }

  // "换模型重试…" (T24 B1): the model dialog of the lecture page's stage
  // card (actions.js), given the lecture for its stage's models.
  function retryWithModel(f, key) {
    run(key, async () => {
      const lec = await api.lecture(f.lecture_id);
      if (lec && alive) await stageAction(lec, f.stage, 'retry_model', true);
    });
  }

  // F6: the cost sentence of the launch confirmation (transcription only,
  // free, billed), judged by the shared rule (update_confirm.js
  // launchCostMode) from the settings the page reads anyway (the budget
  // line, the roles notice). Without the settings, or without that rule,
  // nothing is passed: the dialog's usual sentence.
  function launchCost() {
    const mode = typeof UC.launchCostMode === 'function' ? UC.launchCostMode(settings) : undefined;
    return mode ? { costMode: mode } : {};
  }

  // A3: launch the drafts of every course. T25 X1 (round 8 WD-01): asked
  // first, as on the course page: how many lectures start, which stages run,
  // that it is billed (update_confirm.js), once.
  // - One course: the shared launch (actions.js launchCourseDrafts, round 8
  //   interface F2), as the course menu and the course page do: its glossary
  //   question when the glossary is still being extracted, then this
  //   confirmation, then its request and its result.
  // - Several courses: one glossary question for all of them whose glossary
  //   is still being extracted (as before), then one confirmation for all
  //   the lectures, then each course's request as before and one result for
  //   all (launchCourseDrafts would ask and tell once per course).
  // R86 L1 (r9 WD-04): the confirmation for several courses names them
  // (round 9 F2: the code, the name for a course without one, in the order
  // shown); the one course's (the shared launch) as before.
  function launchAllDrafts() {
    run('drafts', async () => {
      const list = data.drafts;
      const total = list.reduce((n, c) => n + (Number(c.count) || 0), 0) || data.draft_total;
      const courses = list.map((c) => c.course_code || c.course_name);
      const confirm = () => confirmLaunchDrafts([], total, list.length > 1 ? { ...launchCost(), courses } : launchCost());
      if (list.length === 1) {
        const c = list[0];
        await launchCourseDrafts(ctx.courses().find((x) => x.id === c.course_id) || { id: c.course_id, code: c.course_code, name: c.course_name }, { ask: confirm });
        return;
      }
      const pending = list.map((c) => ctx.courses().find((x) => x.id === c.course_id)).filter((c) => c && c.glossary_pending);
      if (pending.length) {
        const ok = await confirmDialog({
          title: t('dashboard.glossary_pending.title'),
          message: t('dashboard.glossary_pending.message', { courses: U.listJoin(pending.map((c) => c.code || c.name)) }),
          okText: t('dashboard.glossary_pending.ok'),
        });
        if (!ok) return;
      }
      if (!(await confirm())) return;
      let ok = 0;
      const errors = [];
      for (const c of list) {
        const res = await api.launchDrafts(c.course_id);
        for (const r of (res && res.results) || []) r.ok ? ok++ : errors.push(batchError(r));
      }
      toast(errors.length ? t('dashboard.launched_partial', { ok, n: errors.length, error: errors[0] }) : t('dashboard.launched', { n: ok }), errors.length ? 'warn' : 'success');
    });
  }

  // A2.3: update every course; the model choice is asked once for all of
  // them (courses without differences are updated right away). T25 X1
  // (round 8 WD-01): asked first, once for all of them, as the course page's
  // "update all" asks (update_confirm.js); the lectures are not on the
  // workbench, so the confirmation says how many.
  function updateAllStale() {
    run('stale', async () => {
      const count = data.stale.reduce((n, c) => n + (Number(c.count) || 0), 0) || data.stale_total;
      if (!(await confirmUpdate([], { named: true, count }))) return;
      let ok = 0;
      const errors = [];
      const ask = [];
      const diff = [];
      const tally = (res) => {
        for (const r of (res && res.results) || []) r.ok ? ok++ : errors.push(batchError(r));
      };
      for (const c of data.stale) {
        try {
          tally(await api.updateStale(c.course_id));
        } catch (e) {
          if (!e.needsModelChoice) throw e;
          ask.push(c);
          diff.push(...(e.body.diff || []));
        }
      }
      if (ask.length) {
        const choice = await modelChoiceDialog(diff, 'update');
        if (choice) for (const c of ask) tally(await api.updateStale(c.course_id, choice));
      }
      if (ok || errors.length) toast(errors.length ? t('dashboard.updated_partial', { ok, n: errors.length, error: errors[0] }) : t('dashboard.updated', { n: ok }), errors.length ? 'warn' : 'success');
    });
  }

  // T24 S1: retry every failure a plain retry may help, one request each (the
  // lecture page's retry); a snapshot that differs from the settings is
  // asked about once for all of them, as "全部更新" does. The failures left
  // out (another model is needed, a damaged file) and the refused ones are
  // said in the result: "已重试 N 节；M 节没有重试：原因". One round at a
  // time across the workbench views (retryRound, R76 L-8).
  function retryAllFailures() {
    if (retryRound.running) return;
    run('retryall', async () => {
      retryRound.running = true;
      retryRoundChanged();
      try {
        await retryRoundOf(data.failures);
      } finally {
        retryRound.running = false;
        retryRoundChanged();
      }
    });
  }

  async function retryRoundOf(list) {
    let ok = 0;
    const errors = [];
    const ask = [];
    const diff = [];
    const send = async (f, choice) => {
      try {
        await api.lectureStageAction(f.lecture_id, f.stage, 'retry', choice, { quiet: true });
        ok++;
      } catch (e) {
        if (!choice && e.needsModelChoice) {
          ask.push(f);
          // R76 L-1: a single retry's 409 names no lecture; named here, the
          // question is a batch's ("N 节讲座", each lecture once, the
          // translations sentence's _many) and judges no lecture alone
          diff.push(...(e.body.diff || []).map((x) => ({ ...x, lecture_id: f.lecture_id, lecture_title: f.lecture_title })));
        } else errors.push(e.message || t('common.unknown_error'));
      }
    };
    for (const f of list.filter(plainRetry)) await send(f);
    if (ask.length) {
      // the model question says itself that the current settings may clear
      // a translation (modelChoiceDialog, R76 M-1)
      const choice = await modelChoiceDialog(diff, 'retry');
      if (choice) for (const f of ask) await send(f, choice);
      else errors.push(...ask.map(() => t('dashboard.retry_skip.cancelled')));
    }
    const model = list.filter((f) => canRetry(f) && U.retryNeedsModel(f)).length;
    const futile = list.filter((f) => (f.allowed_actions || []).includes('retry') && U.retryFutile(f)).length;
    // R76 L-9: a failure whose actions offer no retry (rare) is left out too
    const noAction = list.filter((f) => !(f.allowed_actions || []).includes('retry')).length;
    const skipped = model + futile + noAction + errors.length;
    if (!skipped) {
      toast(t('dashboard.retried', { n: ok }), 'success');
      return;
    }
    const why = [];
    if (model) why.push(t('dashboard.retry_skip.model', { n: model }));
    if (futile) why.push(t('dashboard.retry_skip.futile', { n: futile }));
    if (noAction) why.push(t('dashboard.retry_skip.no_action', { n: noAction }));
    why.push(...new Set(errors));
    // R76 L-2 (round 8 WD-08): with its reasons it is long; it stays until
    // closed (the short "已重试 N 节" fades as before)
    toast(t('dashboard.retried_partial', { ok, n: skipped, reason: U.clauseJoin(why) }), 'warn', { sticky: true });
  }

  // ---- recently finished ----

  function recentCard() {
    const list = data.recent_done;
    return card('card-recent', 'circle-check', t('dashboard.recent.title'), null, null, list.length ? h('div', { class: 'rd-list lgrid' }, list.map(recentRow)) : emptyLine('clock', t('dashboard.recent.empty')));
  }

  function recentRow(lec, i, list) {
    const exp = lec.stages.find((s) => s.stage === 'export');
    // T28 G1 (r9 WD9-02): the title is found again by its focus key after
    // a redraw (a menu's action, a dialog cancelled, a refresh), as the
    // in-progress rows' are; a row gone (updated: the lecture runs again,
    // deleted) hands the focus to the lecture in progress, else to the
    // next row, else the one before (L14)
    const away = [`dash:open:${lec.id}`, ...[list[i + 1], list[i - 1]].filter(Boolean).map((x) => `dash:recent:${x.id}`)];
    // three right-aligned columns (empty cells keep their place)
    const meta = [exp && exp.finished_at ? U.relTime(exp.finished_at) : '', lec.duration_ms ? U.fmtClock(lec.duration_ms) : '', costText(lec) || ''];
    const dl = lec.exports.length
      ? menuButton(
          h('button', { type: 'button', class: 'icon-btn', 'aria-label': t('course.lectures.download_label', { title: zh(lec.title) }), title: t('dashboard.recent.download'), dataset: { fkey: `dash:dl:${lec.id}` } }, icon('download', { size: 16 })),
          () => exportItems(lec.exports),
        )
      : null;
    const review = lec.counts.patches_pending > 0;
    // has-review: the narrow layout leaves the badge room on the title's
    // line (a class, not :has(), which Firefox < 121 lacks)
    const row = h(
      'div',
      { class: review ? 'rd-row lgrid-row has-review' : 'rd-row lgrid-row' },
      lref(lec.course_code, lec),
      h('a', { class: 'rd-title', href: `#/lecture/${enc(lec.id)}`, title: `${lec.course_name} · ${zh(lec.title)}`, dataset: { fkey: `dash:recent:${lec.id}`, fnext: [...away, 'dash:add'].join(' ') } }, zh(lec.title)),
      h('span', { class: 'rd-badge' }, review ? h('a', { class: 'chip chip-review', href: `#/lecture/${enc(lec.id)}/review`, title: t('course.lectures.review_tip') }, t('course.lectures.review', { n: lec.counts.patches_pending })) : null),
      h(
        'span',
        { class: 'rd-meta' },
        meta.map((m, i) => h('span', { class: `rd-m rd-m${i}` }, m)),
      ),
      h(
        'span',
        { class: 'rd-end' },
        // SPEC-v2 P1: "play" in the recently finished list
        lec.playable ? h('a', { class: 'icon-btn', href: `#/play/${enc(lec.id)}`, 'aria-label': t('player.entry.play_named', { title: zh(lec.title) }), title: t('player.entry.play'), dataset: { fkey: `dash:play:${lec.id}` } }, icon('play', { size: 16 })) : null,
        dl,
      ),
    );
    bindContextMenu(row, () => lectureMenuItems(lec, menuHandlers));
    return row;
  }

  // The lecture's estimated cost, only when all priced stages used providers
  // of one currency (a sum across currencies would be meaningless).
  function costText(lec) {
    if (!lec.usage || lec.usage.estimated_cost == null) return null;
    const prov = (ctx.resources() && ctx.resources().providers) || [];
    const curs = new Set();
    for (const st of lec.stages) {
      if (!st.stats || st.stats.estimated_cost == null) continue;
      const p = st.model && prov.find((x) => x.id === st.model.provider_id);
      curs.add(p ? p.currency || '' : '?');
    }
    if (curs.size !== 1 || curs.has('?')) return null;
    return U.fmtMoney(lec.usage.estimated_cost, [...curs][0] || null);
  }

  // ---- today's usage ----

  let usageEl = null;
  let tariffKey = '';
  let waitKey = U.waitContextKey(ctx.resources()); // onResources

  // v2 P12.4: the provider's peak state from the resource snapshot
  function tariffOf(id) {
    const res = ctx.resources();
    const p = res && (res.providers || []).find((x) => x.id === id);
    return p && p.tariff_state && hasPeaks(p.tariff_state) ? p.tariff_state : null;
  }

  // The usage card follows the peak state and its countdown (SSE resources,
  // compared once a minute) without re-rendering the rest of the page.
  function tariffSig() {
    const res = ctx.resources();
    const states = ((res && res.providers) || []).map((p) => (p.tariff_state ? [p.id, p.tariff_state.peak, p.tariff_state.next_change, p.tariff_state.missing_holidays, p.tariff_state.today_peak] : null));
    return JSON.stringify([Math.floor(Date.now() / 60000), states]);
  }

  function refreshUsage() {
    const sig = tariffSig();
    if (sig === tariffKey) return;
    tariffKey = sig;
    if (!data || !usageEl || !usageEl.isConnected) return;
    preserveFocus(() => {
      const n = usageCard();
      usageEl.replaceWith(n);
      usageEl = n;
    });
  }

  function usageCard() {
    const list = data.usage_today;
    const totals = new Map();
    for (const u of list) if (u.cost != null) totals.set(u.currency || '', (totals.get(u.currency || '') || 0) + u.cost);
    const budget = settings && settings.general && settings.general.daily_budget > 0 ? settings.general.daily_budget : 0;
    const spent = [...totals.values()].reduce((a, b) => a + b, 0);
    return card(
      'card-usage',
      'wallet',
      t('dashboard.usage.title'),
      null,
      null,
      h(
        'div',
        { class: 'ucard' },
        h(
          'div',
          { class: 'usage-total' },
          totals.size
            ? [...totals.entries()].map(([cur, v]) => h('span', { class: 'usage-money' }, U.fmtMoney(v, cur || null)))
            : h('span', { class: 'usage-money is-quiet' }, list.some((u) => u.prompt_tokens) ? t('dashboard.usage.no_prices') : '0'),
          h('span', { class: 'usage-total-k' }, t('dashboard.usage.estimated')),
        ),
        budget
          ? h(
              'div',
              { class: 'usage-budget', title: t('dashboard.usage.budget_tip') },
              h(
                'div',
                { class: 'usage-budget-top' },
                h('span', null, t('dashboard.usage.budget', { amount: budget })),
                h('span', { class: spent >= budget ? 'tone-err' : 'dim' }, t('dashboard.usage.used', { pct: Math.min(999, Math.round((spent / budget) * 100)) })),
              ),
              h('div', { class: 'meter ' + (spent >= budget ? 'meter-warn' : 'meter-accent') }, h('span', { class: 'meter-fill', style: { width: `${Math.min(100, (spent / budget) * 100).toFixed(1)}%` } })),
            )
          : null,
        list.length ? h('div', { class: 'usage-list' }, list.map(usageRow)) : emptyLine('wallet', t('dashboard.usage.no_providers')),
      ),
    );
  }

  function usageRow(u) {
    const ts = tariffOf(u.provider_id);
    const unpriced = u.cost_unpriced_requests > 0 ? u.cost_unpriced_requests : 0;
    const unpricedTip = unpriced ? t('resources.unpriced_tip', { n: unpriced }) : '';
    if (!u.requests && !u.prompt_tokens && !u.completion_tokens) {
      const idle = h('div', { class: 'urow is-idle' }, h('span', { class: 'urow-name' }, u.name), h('span', { class: 'urow-idle' }, t('dashboard.usage.idle')));
      return ts ? h('div', { class: 'urow-wrap' }, idle, tariffUsageBlock(ts)) : idle;
    }
    return h(
      'div',
      { class: 'urow' },
      h(
        'div',
        { class: 'urow-top' },
        h('span', { class: 'urow-name' }, u.name),
        h(
          'span',
          { class: 'urow-cost' },
          // D3: without an estimate (cost null) only tokens are shown
          u.cost != null ? U.fmtMoney(u.cost, u.currency) : h('span', { class: 'dim', title: unpriced ? unpricedTip : undefined }, t('dashboard.usage.no_price')),
        ),
      ),
      // part of today's requests had no price: the amount is too low
      u.cost != null && unpriced ? h('div', { class: 'urow-tariff-gap', title: unpricedTip }, icon('info', { size: 12 }), t('resources.unpriced')) : null,
      h(
        'div',
        { class: 'urow-nums' },
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.input')} `), U.fmtTokens(u.prompt_tokens)),
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.output')} `), U.fmtTokens(u.completion_tokens)),
        h('span', null, h('span', { class: 'dim' }, `${t('dashboard.usage.requests')} `), String(u.requests)),
      ),
      h(
        'div',
        { class: 'urow-cache', title: t('dashboard.usage.cache_tip') },
        h('span', { class: 'dim' }, t('dashboard.usage.cache')),
        h('div', { class: 'meter meter-ok' }, h('span', { class: 'meter-fill', style: { width: `${((u.cache_hit_rate || 0) * 100).toFixed(1)}%` } })),
        h('span', { class: 'urow-rate' }, u.cache_hit_rate != null ? U.fmtPercent(u.cache_hit_rate) : '--'),
      ),
      ts ? tariffUsageBlock(ts) : null,
    );
  }

  // ---- course overview ----

  function coursesCard() {
    return card(
      'card-courses',
      'library',
      t('course.list.title'),
      data.courses.length,
      h('a', { class: 'card-link', href: '#/courses' }, t('dashboard.courses.all'), icon('chevron-right', { size: 14 })),
      // settings: the cards' menus judge the launch's cost by them (F6)
      h('div', { class: 'ccards' }, data.courses.map((c) => courseCard(c, { onChanged: load, settings: () => settings })), addCourseCard(ctx.newCourse)),
    );
  }

  // ---- empty state (no courses) ----

  function emptyState() {
    return h(
      'div',
      { class: 'onboard' },
      h('div', { class: 'onboard-icon' }, icon('graduation-cap', { size: 30 })),
      h('h1', { class: 'onboard-title' }, t('dashboard.onboard.title')),
      h('p', { class: 'onboard-text' }, t('dashboard.onboard.text')),
      btn(t('course.add'), { kind: 'primary', icon: 'plus', onclick: ctx.newCourse, fkey: 'dash:first' }),
      h(
        'ol',
        { class: 'onboard-steps' },
        [
          ['book-open', 'add'],
          ['upload', 'drop'],
          ['sparkles', 'auto'],
        ].map(([ic, k], i) =>
          h(
            'li',
            null,
            h('span', { class: 'onboard-step-icon' }, icon(ic, { size: 16 })),
            h('span', { class: 'onboard-step-t' }, `${i + 1}. ${t(`dashboard.onboard.${k}`)}`),
            h('span', { class: 'onboard-step-s' }, t(`dashboard.onboard.${k}_hint`)),
          ),
        ),
      ),
    );
  }

  // ---- live updates ----

  function onJob(ev) {
    if (ev.scope !== 'lecture') return;
    const ref = pipes.get(ev.scope_id);
    if (!ref) {
      // the progress of a playback file made in the background (stream-parallel)
      // shows nowhere here: no reload for each of its events
      if (ev.stage === 'stream' && ev.status === 'running') return;
      schedule('load', load);
      return;
    }
    if (streamProgress(ref.lec, ref.bar, ev)) return;
    const st = ref.lec.stages.find((s) => s.stage === ev.stage);
    if (st && st.status === ev.status) {
      ref.bar.setProgress(ev.stage, ev.progress);
      const s = statusFor(ref.lec);
      ref.statusEl.textContent = s.text;
      ref.statusEl.title = s.text;
    } else {
      schedule('load', load);
    }
  }

  render(); // skeleton until the first response
  load();
  loadSettings();
  const tick = setInterval(() => alive && data && render(), 60000); // relative times
  const onKinds = () => alive && data && render(); // kind names (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  return {
    refresh: load,
    onJob,
    onResources(r) {
      refreshUsage();
      if (data && !error && colabSlot.isConnected) preserveFocus(fillColab);
      // the waiting texts name devices and follow the power policy: redraw
      // when that changes (the first snapshot may come after the dashboard)
      const k = U.waitContextKey(r);
      if (k !== waitKey) {
        waitKey = k;
        if (data && !error) render();
      }
    },
    // the setup guide's state came or changed (app.js ctx.guide, WD-04)
    onGuideChanged() {
      if (alive && data && !error) render();
    },
    onChanged(ev) {
      if (['lecture', 'course', 'patches', 'devices'].includes(ev.kind)) schedule('load', load); // devices: the I9 notice
      if (ev.kind === 'settings') loadSettings();
      updateHint.onChanged(ev);
      asrSlot.onChanged(ev);
    },
    destroy() {
      alive = false;
      retryRound.views.delete(onRetryRound);
      updateHint.destroy();
      asrSlot.destroy();
      dlHint.destroy();
      clearInterval(tick);
      window.removeEventListener('kindschange', onKinds);
      for (const tm of timers.values()) clearTimeout(tm);
    },
  };
}
