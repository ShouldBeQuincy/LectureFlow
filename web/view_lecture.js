// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/lecture/{id}[/review]: the lecture header — title, L number, course,
// pipeline (pipeline.js), the stale notice with "Update", lecture actions from
// lecture_actions (and the same context menu as a lecture row, A12.2),
// counts, usage and prefix-cache hit rate (§17.7), export downloads incl.
// cite.json (A11) — and the two tabs, transcript (§9.6) and review (§9.7).
// lecture kinds: the eyebrow says "CSC1003 · Week 1 · Lecture · No. 2" (the
// citation number on hover), and the slides this lecture uses are listed
// with where they come from (its week, itself, the whole course).
// "Transcribe only" (3.1.0, lecture_english.js): while the API stages wait
// for a model provider, a notice says so (with a link to the providers),
// Play is the primary action, and the English subtitles and transcript are
// in the download menu as soon as asr is done.
// ext web3: the ⋯ menu offers "添加第二视角…" in the desktop app (lecture_angles.js).
// colab-obs: a lecture transcribed in speed mode says so ("本节用极速模式（切成 N 段）").

import { api, DESKTOP } from './api.js';
import { h, clear, btn, iconBtn, preserveFocus, toast, everySecond, stopEverySecond } from './dom.js';
import { icon } from './icons.js';
import { pipelineBar, streamProgress, modelRetryFirst, settingsLink, stageEta, mixedModelsText } from './pipeline.js';
import { menuButton, bindContextMenu } from './menu.js';
import { lectureDownloadMenu, waitsForProvider, englishOnly } from './lecture_english.js';
import * as A from './actions.js';
import * as Clip from './audiobar.js';
import { mountTranscript } from './view_transcript.js';
import { mountReview } from './view_review.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { lectureTitles, fmtRecordedAt, openRecordedAtDialog } from './calendar_ui.js';
import { lectureTagText } from './kinds.js';
import { openKindDialog } from './kind_dialog.js';
import { angleMenuItems } from './lecture_angles.js';
import { splitParts } from './settings_colab.js';
import { confirmUpdate } from './update_confirm.js';

const enc = encodeURIComponent;

let statSeq = 0; // the ids of the stats' labels and explanations (WL-08)

// A stage's error in a notice: the translated text (error_info), the same
// text on hover (the line may be cut short). The server's own text is not
// the hover of a stage's notice: it is in the UI's language only by chance
// (T6 #4), and the stage card shows it, with "复制完整错误" (v2 P14.3). A
// task without a card (the playback file, the title) has it in the hover
// after the translated text (detail), or it would be nowhere (R29).
function errorSpan(text, detail = '') {
  const title = detail && detail !== text ? `${U.excerpt(text, 300)}\n\n${U.excerpt(detail, 300)}` : U.excerpt(text, 300);
  return h('span', { class: 'lec-notice-err', title }, U.excerpt(text, 300));
}

/** The key of the lecture page's "processing goes on in the background" note: the desktop app's (closing asks) or a browser's (a view only). */
export function backgroundNoteKey(desktop) {
  return desktop ? 'lecture.page.background_desktop' : 'lecture.page.background_browser';
}

export function mountLecture(root, route, ctx) {
  const id = route.id;
  let tab = route.tab;
  let lecture = null;
  let loadError = '';
  let gone = false; // the lecture was deleted (404)
  let goneCourse = ''; // its course, for the "back to course" link
  let alive = true;
  let sub = null;
  let bar = null;
  let streamNoticeText = null; // the text of the "generating" notice (updated in place)
  let timer = null;
  let shownCost = null;
  let shownWait = ''; // the wait notices shown (they follow the resources)
  let headSum = null; // the status text beside the bar (renderHead)
  // T24 S3: the settings' providers with their probed models, read once,
  // and only when a failure may be a model its provider no longer lists
  let providers = null;
  // T25 S1: the last usage_total read (null: none given), and whether the
  // next read asks for it (load)
  let lastTotal = null;
  let wantTotal = true;

  const el = h('div', { class: 'page page-lecture' });
  const headEl = h('div', { class: 'lec-head' });
  const tabsEl = h('div', { class: 'lec-tabs', role: 'tablist', 'aria-label': t('lecture.tab.aria') });
  const panelEl = h('div', { class: 'lec-tabpanel', role: 'tabpanel' });
  el.append(headEl, tabsEl, panelEl);
  root.append(el);
  // Right click / Shift+F10 on the header: the lecture row's menu (A12.2).
  bindContextMenu(headEl, () => (lecture ? menuItems(lecture) : null));
  // the lecture row's menu without "open", and the second angle before
  // "delete" (ext web3)
  const menuItems = (lec) => {
    const items = A.lectureMenuItems(lec, handlers).filter((it) => !(it && it.id === 'open'));
    const at = items.findIndex((it) => it && it.id === 'delete');
    items.splice(at < 0 ? items.length : Math.max(0, at - 1), 0, ...angleMenuItems(lec, (l) => handlers.onLectures([l])));
    return items;
  };

  const handlers = {
    onLectures(ls) {
      const l = ls.find((x) => x.id === id);
      if (l && alive) {
        lecture = l;
        render();
      }
    },
    onDeleted() {
      location.hash = lecture ? `#/course/${enc(lecture.course_id)}` : '#/';
    },
  };

  // Quiet: the page shows load errors itself. A 404 means the lecture was
  // deleted (e.g. from another tab while this page was open): show the
  // not-found state instead of a stale header and an error toast. Other
  // errors on a background reload keep the last data and are toasted
  // (connection failures are covered by the SSE reconnect banner).
  // T25 S1: the lecture's total over every run (usage_total) comes only with
  // GET …?usage_total=1 (it scans the jobs): asked for when the page opens
  // and after a stage or task finished (wantTotal); the other reads, and
  // the lectures an action answers with, show the last total read.
  async function load() {
    const withTotal = wantTotal;
    wantTotal = false;
    try {
      const l = await api.lecture(id, withTotal ? { quiet: true, usageTotal: true } : { quiet: true });
      if (!alive) return;
      // a failure that may be an unlisted model: drawn once the settings
      // tell (R72 L3), or not knowing after a few seconds (R79b (d))
      await waitProviders(l);
      if (!alive) return;
      if (withTotal) lastTotal = l && l.usage_total && typeof l.usage_total === 'object' ? l.usage_total : null;
      lecture = l;
      loadError = '';
      gone = false;
    } catch (e) {
      if (withTotal) wantTotal = true; // asked for again by the next read
      if (!alive) return;
      if (e && e.status === 404) {
        gone = true;
        lecture = null;
        loadError = e.message || t('errors.lecture.not_found');
        if (sub) {
          sub.destroy();
          sub = null;
        }
        bar = null;
      } else if (!lecture) {
        loadError = (e && e.message) || t('common.load_failed');
      } else if (e && e.status > 0) {
        toast(e.message, 'error');
      }
    }
    render();
  }

  function retry() {
    loadError = '';
    render();
    load();
  }

  function render() {
    if (!lecture) {
      clear(headEl);
      clear(tabsEl);
      clear(panelEl);
      if (loadError) {
        const back = goneCourse ? h('a', { class: 'btn btn-primary', href: `#/course/${enc(goneCourse)}` }, t('lecture.page.back_to_course')) : null;
        headEl.append(
          h(
            'div',
            { class: 'empty-state' },
            h('div', { class: 'empty-icon' }, icon(gone ? 'search' : 'circle-alert', { size: 26 })),
            h('h1', { text: gone ? t('player.not_found') : t('lecture.page.load_failed') }),
            h('p', { text: loadError }),
            h(
              'div',
              { class: 'empty-actions' },
              gone ? back : btn(t('lecture.page.reload'), { kind: 'primary', icon: 'rotate-cw', onclick: retry }),
              h('a', { class: 'btn', href: '#/' }, t('player.to_dashboard')),
            ),
          ),
        );
      } else {
        headEl.append(skeletonHead());
      }
      return;
    }
    ctx.setCurrentCourse(lecture.course_id);
    goneCourse = lecture.course_id;
    renderHead();
    renderTabs();
    if (!sub) mountTab();
    else sub.setLecture(lecture);
  }

  function skeletonHead() {
    return h(
      'div',
      { class: 'lec-skel', 'aria-hidden': 'true' },
      h('div', { class: 'skel', style: { width: '160px', margin: '0 0 14px' } }),
      h('div', { class: 'skel skel-eyebrow' }),
      h('div', { class: 'skel skel-h1', style: { width: '420px' } }),
      h('div', { class: 'skel', style: { width: '260px', margin: '6px 0 18px' } }),
      h('div', { class: 'card lec-panel' }, h('div', { class: 'card-bd' }, h('div', { class: 'skel', style: { height: '10px', margin: '6px 0 22px' } }), h('div', { class: 'skel', style: { width: '70%' } }))),
    );
  }

  // ---- header ----

  const run = (p) =>
    p.then(
      (r) => r && handlers.onLectures(Array.isArray(r) ? r : [r]),
      () => {},
    );

  // What "更新" does (T25 X1): the hover text of its buttons names the out-
  // of-date stages it runs again (Lecture.stale.stages); the later ones
  // follow.
  function updateTip(lec) {
    const stages = (lec.stale && lec.stale.stages) || [];
    // only the export: no model is called (R79 L10)
    if (stages.length && stages.every((s) => s === 'export')) return t('lecture.page.update_tip_export');
    return stages.length ? t('lecture.page.update_tip', { stages: U.listJoin(stages.map(U.stageName)) }) : t('lecture.page.update_tip_plain');
  }

  // T25 X1 (round 8 WL-01): the header's and the stale notice's "更新" say
  // first which stages run again, why, that they are billed again and what
  // they took the last time (update_confirm.js, as the course page's rows
  // do); then the update and its model choice as before. One dialog for the
  // lecture at a time (a double click, or both buttons). The ⋯ menu's
  // "update" asks in actions.js (the shared line).
  function updateConfirmed(lec) {
    return A.once(`confirm_update:${lec.id}`, async () => ((await confirmUpdate([lec])) ? A.updateLectures([lec]) : []));
  }

  // A stage that waits for the user (R79 L5): paused devices, a device or
  // component to set up, no provider, a remote device to sign in to or
  // set up — "处理在后台进行" would not be so then.
  const USER_WAITS = new Set(['gpu_paused', 'no_asr_device', 'no_provider', 'remote_auth', 'remote_balance', 'remote_setup', 'remote_held', 'remote_fallback', 'remote_fallback_ssh']);
  const USER_WAIT_PREFIXES = ['device_paused:', 'component_missing:', 'model_missing:'];
  const waitsForUser = (w) => typeof w === 'string' && (USER_WAITS.has(w) || USER_WAIT_PREFIXES.some((p) => w.startsWith(p)));
  // whether the lecture is processed on its own now: a stage runs, or is
  // queued for a reason that passes by itself
  const goesOn = (lec) => (lec.stages || []).some((s) => s.status === 'running') || ((lec.stages || []).some((s) => s.status === 'queued') && !(lec.stages || []).some((s) => s.status === 'queued' && waitsForUser(s.waiting_for)));

  function headActions(lec) {
    const acts = lec.lecture_actions || [];
    const out = [];
    const k = (x) => `lecd:${lec.id}:${x}`;
    // SPEC-v2 P1: Play once asr is done (primary when everything is done,
    // or when the English is all there will be until a provider is set up)
    const playMain = (U.isAllDone(lec.stages) || englishOnly(lec)) && !acts.includes('update');
    if (lec.playable) out.push(btn(t('player.entry.play'), { kind: playMain ? 'primary' : '', icon: 'play', fkey: k('play'), title: t('player.entry.play_hint'), onclick: () => (location.hash = `#/play/${encodeURIComponent(lec.id)}`) }));
    if (acts.includes('launch')) out.push(btn(t('lecture.page.launch'), { kind: 'primary', icon: 'play', fkey: k('launch'), fnext: focusNext(lec, 'launch'), onclick: () => run(A.launchLectures([lec])) }));
    // "Update" sits in the stale notice when there is one.
    if (acts.includes('update') && !U.lectureStaleText(lec)) out.push(btn(t('lecture.page.update'), { kind: 'warn', icon: 'refresh-cw', fkey: k('update'), fnext: focusNext(lec, 'update'), title: updateTip(lec), onclick: () => run(updateConfirmed(lec)) }));
    if (acts.includes('cancel')) out.push(btn(t('lecture.page.cancel'), { kind: 'danger', icon: 'ban', fkey: k('cancel'), fnext: focusNext(lec, 'cancel'), onclick: () => run(A.cancelLectures([lec])) }));
    // export files, and the English files once asr is done (3.1.0)
    if (lec.exports.length || lec.playable) {
      const dl = lectureDownloadMenu(lec, k('dl'));
      if (lec.exports.length && !U.isAllDone(lec.stages)) dl.title = t('lecture.page.exports_outdated');
      out.push(dl);
    }
    out.push(
      menuButton(iconBtn('ellipsis', t('lecture.page.menu'), { fkey: k('menu'), menu: true, class: 'icon-btn-bordered' }), () => menuItems(lec)),
    );
    return out;
  }

  // Where the focus goes when an action of the header or of a notice is gone
  // after it ran (L14): the header's action that takes its place, else the
  // header's next control; "更多操作" is always there.
  function focusNext(lec, action) {
    const k = (x) => `lecd:${lec.id}:${x}`;
    switch (action) {
      case 'launch':
        return [k('cancel'), k('dl'), k('menu')];
      case 'update':
        return [k('cancel'), k('play'), k('menu')];
      case 'cancel':
        return [k('launch'), k('update'), k('play'), k('menu')];
      case 'launch2':
        return [k('cancel'), k('menu')];
      case 'retry:stream':
        return [k('play'), k('menu')];
      case 'retry:title':
        return [k('menu')];
    }
    // a stage's "换模型重试…" (T24 B1): its retry, else the stage's segment
    if (action.startsWith('retry_model:')) {
      const stage = action.slice('retry_model:'.length);
      return [k(`retry:${stage}`), k(`seg:${stage}`), k('menu')];
    }
    // a stage's retry: the stage's segment
    return [k(`seg:${action.slice('retry:'.length)}`), k('menu')];
  }

  // An amount of the lecture's estimated cost, with its currency only when
  // every priced stage used providers of one currency (a sum across
  // currencies would be meaningless); null without an amount.
  function moneyText(lec, amount) {
    if (amount == null) return null;
    const prov = (ctx.resources() && ctx.resources().providers) || [];
    const curs = new Set();
    for (const st of lec.stages) {
      if (!st.stats || st.stats.estimated_cost == null) continue;
      const p = st.model && prov.find((x) => x.id === st.model.provider_id);
      curs.add(p ? p.currency || '' : '?');
    }
    if (curs.size > 1 || curs.has('?')) return U.fmtCost(amount);
    return U.fmtMoney(amount, [...curs][0] || null);
  }

  // The lecture's estimated cost: the latest run of each stage (usage).
  function costText(lec) {
    return lec.usage ? moneyText(lec, lec.usage.estimated_cost) : null;
  }

  // T25 S1: what the lecture used over every task and attempt: its own
  // usage_total, else the last one read (load); null where the back end
  // gives none.
  function usageTotal(lec) {
    const u = lec.usage_total && typeof lec.usage_total === 'object' ? lec.usage_total : lastTotal;
    return u && typeof u === 'object' ? u : null;
  }

  // The cost texts a resources snapshot may change (the currency): the
  // header is redrawn when they do.
  function costKey(lec) {
    const total = usageTotal(lec);
    return `${costText(lec)}|${total ? moneyText(lec, total.estimated_cost) : ''}`;
  }

  // A stat: label, value, the line under it (sub), and opts {icon, href,
  // title (a link's), cls, extra (a node after the value: the cache's
  // meter), tip}. R72 L6 (round 8 WL-08): an explanation (tip) was only a
  // title on a div, which neither the keyboard nor a screen reader reaches.
  // Such a stat is a focusable group named by its label and value and
  // described by the explanation (aria-describedby, a visually hidden
  // copy); the mouse keeps it on hover (title). Not the page's CSS tips
  // (.tip, data-tip), which would show it on focus too: .lec-stats clips
  // what goes past it (overflow: hidden, for its rounded corners), so the
  // tip under a stat would not be seen, and the hover text would be gone.
  function stat(label, value, sub, opts = {}) {
    const tip = !opts.href && opts.tip ? opts.tip : null;
    const id = tip ? `lec-stat-${++statSeq}` : '';
    return h(
      opts.href ? 'a' : 'div',
      {
        class: 'lec-stat' + (opts.cls ? ' ' + opts.cls : ''),
        href: opts.href,
        title: opts.title || tip || undefined,
        tabindex: tip ? '0' : undefined,
        role: tip ? 'group' : undefined,
        'aria-labelledby': tip ? [`${id}-k`, `${id}-v`, sub ? `${id}-s` : ''].filter(Boolean).join(' ') : undefined,
        'aria-describedby': tip ? `${id}-d` : undefined,
        // (a redraw keeps the focus on it, as on the review link)
        dataset: opts.href || tip ? { fkey: `lecd:stat:${label}` } : undefined,
      },
      h('span', { class: 'lec-stat-k', id: tip ? `${id}-k` : undefined }, opts.icon ? icon(opts.icon, { size: 13 }) : null, label),
      h('span', { class: 'lec-stat-v', id: tip ? `${id}-v` : undefined }, value),
      sub ? h('span', { class: 'lec-stat-sub', id: tip ? `${id}-s` : undefined }, sub) : null,
      opts.extra || null,
      tip ? h('span', { class: 'visually-hidden', id: `${id}-d` }, tip) : null,
    );
  }

  // R72 L6: the total over every run is never below the latest runs added
  // up (the back end's word); where it is, it is not shown (a fallback).
  function totalBelow(total, u) {
    if (!total || !u) return false;
    const n = (x) => Number(x) || 0;
    if (n(total.prompt_tokens) < n(u.prompt_tokens) || n(total.completion_tokens) < n(u.completion_tokens)) return true;
    return total.estimated_cost != null && u.estimated_cost != null && n(total.estimated_cost) < n(u.estimated_cost) - 1e-9;
  }

  function stats(lec) {
    const c = lec.counts;
    const u = lec.usage;
    const rate = U.cacheHitRate(u);
    const used = U.hasUsage(u);
    const cost = costText(lec);
    shownCost = costKey(lec);
    // T25 S1: the stats are each stage's latest run (they go down after a
    // rerun or a few sentences translated again); beside them, what the
    // lecture used in all, with the difference explained. Without the
    // field, as before.
    const total = usageTotal(lec);
    const totalUsed = !!total && U.hasUsage(total) && !totalBelow(total, u);
    const totalCost = totalUsed ? moneyText(lec, total.estimated_cost) : null;
    const tip = totalUsed ? t('lecture.stat.usage_tip') : null;
    const tokensSub = totalUsed ? t('lecture.stat.tokens_total_sub', { input: U.fmtTokens(total.prompt_tokens), output: U.fmtTokens(total.completion_tokens) }) : used ? t('lecture.stat.tokens_sub') : t('lecture.stat.tokens_none');
    const costSub = totalCost ? t('lecture.stat.cost_total_sub', { total: totalCost }) : cost ? t('lecture.stat.cost_sub') : used || totalUsed ? t('lecture.stat.cost_none') : null;
    const zhSub = [];
    if (c.zh_qa_failed) zhSub.push(h('span', { class: 'tone-err' }, t('lecture.stat.qa_failed', { n: U.count(c.zh_qa_failed) })));
    if (c.zh_stale) zhSub.push(h('span', { class: 'tone-warn' }, t('lecture.stat.retranslating', { n: U.count(c.zh_stale) })));
    return h(
      'div',
      { class: 'lec-stats' },
      stat(t('lecture.stat.sentences'), c.sentences ? U.fmtInt(c.sentences) : '–', lec.duration_ms ? t('lecture.page.duration', { time: U.fmtClock(lec.duration_ms) }) : null, { icon: 'audio-lines' }),
      stat(t('lecture.stat.zh_ok'), c.sentences ? U.fmtInt(c.zh_ok) : '–', zhSub.length ? zhSub.reduce((acc, n, i) => (i ? [...acc, ' · ', n] : [n]), []) : c.sentences ? t('lecture.stat.total', { n: U.count(c.sentences) }) : null, { icon: 'languages' }),
      stat(t('lecture.stat.pending'), U.fmtInt(c.patches_pending), c.patches_pending ? t('lecture.stat.pending_sub') : t('lecture.stat.pending_none'), {
        icon: 'list-checks',
        href: c.patches_pending ? `#/lecture/${enc(lec.id)}/review` : null,
        cls: c.patches_pending ? 'is-warn' : '',
        title: c.patches_pending ? t('lecture.stat.open_review') : null,
      }),
      stat(t('lecture.stat.tokens'), used ? `${U.fmtTokens(u.prompt_tokens)} / ${U.fmtTokens(u.completion_tokens)}` : '–', tokensSub, { icon: 'activity', tip }),
      stat(t('lecture.stat.cache'), rate != null ? U.fmtPercent(rate) : '–', rate != null ? null : t('lecture.stat.cache_none'), {
        icon: 'gauge',
        tip: t('lecture.stat.cache_tip'),
        extra: rate != null ? h('span', { class: 'meter meter-ok lec-stat-meter' }, h('span', { class: 'meter-fill', style: { width: `${Math.round(rate * 100)}%` } })) : null,
      }),
      stat(t('lecture.stat.cost'), cost || '–', costSub, { icon: 'coins', tip }),
    );
  }

  // v3.2.x P1-b: the lecture's translations come from more than one model
  // (the translate stage's stats.mixed_models), a neutral line under the
  // stats; null otherwise.
  function mixedModels(lec) {
    const st = (lec.stages || []).find((s) => s.stage === 'translate');
    const text = mixedModelsText(st);
    return text ? h('p', { class: 'lec-mixed-models' }, icon('languages', { size: 13 }), h('span', null, text)) : null;
  }

  // The slide files this lecture uses and why (lecture kinds: slides by week).
  function slides(lec) {
    const list = lec.materials || [];
    if (!list.length) return null;
    const via = (m) => (m.via === 'week' ? t('lecture.slides.via_week', { week: m.week }) : m.via === 'lecture' ? t('lecture.slides.via_lecture') : t('lecture.slides.via_course'));
    return h(
      'div',
      { class: 'lec-slides' },
      h('span', { class: 'lec-slides-k', title: t('lecture.slides.tip') }, icon('layers', { size: 13 }), t('lecture.slides.title')),
      h(
        'ul',
        { class: 'lec-slides-list' },
        list.map((m) => h('li', { class: 'lec-slide', title: `${m.filename} · ${via(m)}` }, icon('file-text', { size: 13, class: 'dim' }), h('span', { class: 'lec-slide-name' }, m.filename), h('span', { class: 'lec-slide-via' }, via(m)))),
      ),
    );
  }

  // P8.1 / stream-parallel: the playback file. A source that must be
  // transcoded gets it in the background after prep, while the transcript
  // goes on: say so (with its progress), and when it could not be made (the
  // task failed, was cancelled or interrupted, or recorded no_space or a
  // failure) say why and offer the retry (POST …/stages/stream/retry: the
  // task's retry, or a new run). The player plays the audio meanwhile.
  function streamNotice(lec) {
    streamNoticeText = null;
    const s = lec.stream;
    if (lec.draft || !s) return null;
    if (s.status === 'generating') {
      streamNoticeText = h('span', { class: 'lec-notice-text' }, t('lecture.notice.stream_generating', { pct: U.pct(s.progress) }));
      return h('div', { class: 'lec-notice is-draft', role: 'status' }, icon('play', { size: 16 }), streamNoticeText);
    }
    if (s.status === 'ready' || s.status === 'audio_only') return null; // plays (an earlier file, or audio only)
    const job = (lec.aux || []).find((a) => a.stage === 'stream');
    const ended = job && (job.status === 'failed' || job.status === 'cancelled') ? job : null;
    if (!ended && s.status !== 'failed' && s.status !== 'no_space') return null;
    let err = '';
    if (ended && ended.status === 'failed') err = U.stageErrorText(ended);
    else if (!ended && s.status === 'no_space') err = t('errors.media.no_space');
    else if (!ended && s.error) err = U.errorInfoText({ code: s.error }, s.error);
    const canRetry = ended ? (ended.allowed_actions || []).includes('retry') : true;
    return h(
      'div',
      { class: 'lec-notice is-cancelled', role: 'status' },
      icon('info', { size: 16 }),
      h(
        'span',
        { class: 'lec-notice-text' },
        h('b', null, ended && ended.status === 'cancelled' ? t('lecture.notice.stream_cancelled') : t('lecture.notice.stream_failed')),
        err ? errorSpan(err, ended && ended.status === 'failed' ? ended.error : '') : null,
      ),
      canRetry ? btn(t('common.retry'), { icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry:stream`, fnext: focusNext(lec, 'retry:stream'), onclick: () => run(A.taskRetry(lec, 'stream')) }) : null,
    );
  }

  // The waits a notice tells how to end (T24 S4, G3), as {key, text, icon,
  // href, label, fkey}: the power policy holds the transcription (below), or
  // today's budget is used up (it comes back at midnight, local time; raised
  // in Settings). The policy itself is not changed here.
  //
  // Each power code says only what ends it (internal/devices/policy.go,
  // placeBattery; R72 H1):
  //   power_wait_ac — on battery, the run is bound to a device that is not
  //     the battery device (a pinned snapshot, or a checkpoint of another
  //     back end, e.g. CUDA's: never handed to the NPU or the Intel GPU):
  //     only the charger ends it; turning an Intel device on does not;
  //   power_no_saver_device — on battery, no usable NPU or Intel GPU: the
  //     charger ends it when an NVIDIA GPU or Intel GPU is usable on AC
  //     (U.acDeviceUsable), and turning on a present Intel device ends it
  //     now (U.saverDeviceOff);
  //   power_no_device — on AC (or pinned to a CPU device): no usable NVIDIA
  //     GPU or Intel GPU, the CPU is never used: the charger is no way out;
  //     a device turned on in Settings may be;
  //   on_battery — before power management: the charger.
  function powerNotice(w) {
    const res = ctx.resources();
    let key = 'power';
    if (w === 'power_wait_ac') key = 'power_wait_ac';
    else if (w === 'power_no_device') key = 'power_no_device';
    else if (w === 'power_no_saver_device') {
      const ac = U.acDeviceUsable(res);
      const saver = U.saverDeviceOff(res);
      key = ac ? (saver ? 'power_wait_other' : 'power') : saver ? 'power_enable_saver' : 'power_no_device';
    }
    const text = t(key === 'power' ? 'lecture.notice.power_wait' : `lecture.notice.${key}`);
    return { key, text, icon: 'plug', href: '#/settings/asr', label: t('lecture.notice.power_settings'), fkey: 'power' };
  }

  function waitNotices(lec) {
    if (lec.draft) return [];
    const stages = lec.stages || [];
    const out = [];
    const power = stages.find((s) => U.powerSuspended(s.waiting_for));
    if (power) out.push(powerNotice(power.waiting_for));
    if (stages.some((s) => s.waiting_for === 'budget')) {
      out.push({ key: 'budget', text: t('lecture.notice.budget_wait'), icon: 'coins', href: '#/settings/general', label: t('lecture.notice.budget_settings'), fkey: 'budget' });
    }
    return out;
  }

  function notices(lec) {
    const acts = lec.lecture_actions || [];
    const out = [];
    if (lec.draft) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-draft' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, t('lecture.notice.draft')),
          acts.includes('launch') ? btn(t('lecture.page.launch'), { kind: 'primary', icon: 'play', size: 'sm', fkey: `lecd:${lec.id}:launch2`, fnext: focusNext(lec, 'launch2'), onclick: () => run(A.launchLectures([lec])) }) : null,
        ),
      );
    }
    // "Transcribe only" (3.1.0): no model provider yet. Says what still
    // works (playing, English subtitles and transcript) and where to set
    // one up; the stages then continue by themselves. Its link opens "用预
    // 设添加服务" at once (route F4, round 8 WL-14; until the settings page
    // knows the route it lands on the providers section, as before).
    if (!lec.draft && waitsForProvider(lec)) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-draft', role: 'status' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, t('lecture.notice.no_provider')),
          h('a', { class: 'btn btn-sm', href: '#/settings/providers/preset', dataset: { fkey: `lecd:${lec.id}:providers` } }, icon('settings', { size: 14 }), h('span', { class: 'btn-label' }, t('lecture.notice.no_provider_settings'))),
        ),
      );
    }
    // T24 S4, G3: a wait the user can end says how (the reason itself is
    // in the status beside the bar)
    // (a narrow notice puts the button on a line of its own, as a failure's
    // does, instead of squeezing the text to nothing: the .lec-notice.is-wait
    // rules of style.css)
    shownWait = '';
    for (const w of waitNotices(lec)) {
      shownWait += w.key;
      out.push(
        h(
          'div',
          { class: 'lec-notice is-draft is-wait', role: 'status' },
          icon(w.icon, { size: 16 }),
          h('span', { class: 'lec-notice-text' }, w.text),
          h('a', { class: 'btn btn-sm', href: w.href, dataset: { fkey: `lecd:${lec.id}:${w.fkey}` } }, icon('settings', { size: 14 }), h('span', { class: 'btn-label' }, w.label)),
        ),
      );
    }
    // T25 S2, T22 G2: while the lecture is processed, what the user may do
    // meanwhile and what closing does (the desktop app asks, on closing its
    // window, whether to go on in the background; a browser tab is only a
    // view); a quiet note
    if (!lec.draft && !shownWait && goesOn(lec)) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-split is-background', role: 'note' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, t(backgroundNoteKey(DESKTOP))),
        ),
      );
    }
    // A failure (or cancellation) says what happened and offers the stage's
    // own next step (its allowed_actions), not only in the hover card (§9.8).
    for (const st of lec.stages) {
      const failed = U.isFinalFailure(st);
      if (!failed && st.status !== 'cancelled') continue;
      const canRetry = (st.allowed_actions || []).includes('retry');
      // test-media I6: retrying a damaged file fails again; say what to do instead
      const futile = failed && U.retryFutile(st);
      // T24 S3, T22 G7: a key, a provider or a model name is fixed in
      // Settings ("打开设置"); a model the provider no longer lists is said
      // in full (G8), and another model is the way out
      const fix = failed ? U.settingsFix(st, providers) : null;
      // T24 B1: the same request cannot get past it (the model refused, the
      // total time limit), or the model is gone: "换模型重试…" first, as the
      // primary action; else "打开设置" where that is the way out
      const modelFirst = failed && modelRetryFirst(st, fix);
      const settingsFirst = !!fix && !modelFirst;
      const err = failed ? U.stageErrorText(st) : '';
      out.push(
        h(
          'div',
          { class: 'lec-notice ' + (failed ? 'is-failed' : 'is-cancelled'), role: failed ? 'alert' : 'status' },
          icon(failed ? 'circle-alert' : 'ban', { size: 16 }),
          h(
            'span',
            { class: 'lec-notice-text' },
            h('b', null, t(failed ? 'lecture.notice.failed' : 'lecture.notice.cancelled', { stage: U.stageName(st.stage) })),
            err ? errorSpan(err) : failed ? null : t('lecture.notice.cancelled_hint'),
            futile ? h('span', { class: 'lec-notice-hint' }, t('common.pipe.corrupt_hint')) : null,
            fix && fix.unlisted ? h('span', { class: 'lec-notice-hint' }, t('common.pipe.model_unlisted', fix.unlisted)) : null,
          ),
          modelFirst ? btn(t('common.action.retry_model'), { kind: 'primary', icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry_model:${st.stage}`, fnext: focusNext(lec, `retry_model:${st.stage}`), onclick: () => run(A.stageAction(lec, st.stage, 'retry_model', true)) }) : null,
          fix ? settingsLink(fix, `lecd:${lec.id}:settings:${st.stage}`, settingsFirst) : null,
          canRetry ? btn(t('common.retry'), { kind: failed && !futile && !modelFirst && !fix ? 'primary' : '', icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry:${st.stage}`, fnext: focusNext(lec, `retry:${st.stage}`), onclick: () => run(A.stageAction(lec, st.stage, 'retry', true)) }) : null,
        ),
      );
    }
    // v2 P13.5: a failed title task is shown here only, with its retry
    const ta = (lec.aux || []).find((a) => a.stage === 'title' && a.status === 'failed');
    if (ta) {
      const taErr = U.stageErrorText(ta);
      out.push(
        h(
          'div',
          { class: 'lec-notice is-cancelled', role: 'status' },
          icon('info', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, h('b', null, t('lecture.detail.title_failed')), taErr ? errorSpan(taErr, ta.error) : null),
          (ta.allowed_actions || []).includes('retry') ? btn(t('common.retry'), { icon: 'rotate-cw', size: 'sm', fkey: `lecd:${lec.id}:retry:title`, fnext: focusNext(lec, 'retry:title'), onclick: () => run(A.taskRetry(lec, 'title')) }) : null,
        ),
      );
    }
    const sn = streamNotice(lec);
    if (sn) out.push(sn);
    // colab-obs: the last transcription used speed mode (Colab cut the
    // lecture into parts, asr_timing.postprocess.split); a quiet fact
    const parts = splitParts(lec);
    if (parts) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-split', role: 'note', title: t('settings.colab.fast_hint') },
          icon('zap', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, t('lecture.notice.split', { n: U.count(parts) })),
        ),
      );
    }
    const staleText = U.lectureStaleText(lec);
    if (staleText) {
      out.push(
        h(
          'div',
          { class: 'lec-notice is-stale', role: 'status' },
          icon('refresh-cw', { size: 16 }),
          h('span', { class: 'lec-notice-text' }, staleText),
          acts.includes('update') ? btn(t('lecture.page.update'), { kind: 'warn', icon: 'refresh-cw', size: 'sm', fkey: `lecd:${lec.id}:update`, fnext: focusNext(lec, 'update'), title: updateTip(lec), onclick: () => run(updateConfirmed(lec)) }) : null,
        ),
      );
    }
    return out;
  }

  // The status next to the bar; the full stale text is in the notice below,
  // with its "Update" button.
  function headSummary(lec) {
    const staleText = U.lectureStaleText(lec);
    const sum = U.lectureSummary(lec.stages, { sys: ctx.system(), res: ctx.resources(), staleText, draft: lec.draft });
    if (sum.kind === 'stale' && staleText) sum.text = t('lecture.page.needs_update');
    // T25 G5: a translation that goes on (reopened, retried) translates the
    // sentences left; its share counts those, so say what is already there
    const have = lec.counts && lec.counts.zh_ok;
    if (sum.kind === 'running' && sum.stage && sum.stage.stage === 'translate' && have > 0) sum.text = t('lecture.page.translate_have', { summary: sum.text, n: U.count(have) });
    // T25 S2, T22 G2: about how long the running stage still takes (the
    // bar's own estimate, pipeline.js)
    const eta = sum.kind === 'running' && sum.stage ? stageEta(sum.stage, { lecture: lec.id }) : '';
    if (eta) sum.text = `${sum.text} · ${eta}`;
    return sum;
  }

  // The settings' providers, for a failure U.mayBeUnlisted (T24 S3). A
  // read of the lecture waits for them before it draws (load), so the
  // failure notice is drawn once with what they tell (R72 L3: not first
  // with "打开设置" first, then again with "换模型重试…" first, its alert
  // announced twice) — at most PROVIDERS_WAIT_MS (R79b (d): a settings read
  // that hangs held the page on its skeleton): then it is drawn not knowing,
  // and again once they come if they change what it says. A lecture an
  // action answers with that needs them has the header drawn again when
  // they come, likewise.
  // R72 L4 (round 8 WL-07): a read that worked is kept until the settings
  // change (SSE changed {kind: "settings"}: a key, a provider, a probe of
  // its models, onChanged); one that failed is tried again when they are
  // needed next, but not within PROVIDERS_RETRY_MS of the failure (not on
  // every draw). readProviders resolves when read (or failed); null when
  // there is nothing to wait for.
  // Round 9 WV-04 (R87 L2, I4): a read that hangs is waited for once (the
  // first draw, at most PROVIDERS_WAIT_MS); the draws after that do not
  // wait for it again (providersWaited), they draw at once and again when
  // it comes. Only the latest read writes what it read: one the settings
  // changed after (dropped by onChanged) and that comes back late is not
  // taken. "changed settings" in a row are read once (SETTINGS_SOON_MS, as
  // the settings page's loadSoon).
  const PROVIDERS_WAIT_MS = 3000;
  const PROVIDERS_RETRY_MS = 30000;
  const SETTINGS_SOON_MS = 150;
  let providersRead = null; // the read in flight, or the one that worked
  let providersWaited = null; // the read in flight a draw waited PROVIDERS_WAIT_MS for
  let providersFailedAt = 0;
  let settingsTimer = 0;
  const needsProviders = (lec) => !!lec && (lec.stages || []).some((s) => U.isFinalFailure(s) && U.mayBeUnlisted(s));
  function readProviders(lec) {
    if (providersRead || !needsProviders(lec)) return providersRead;
    if (providersFailedAt && Date.now() - providersFailedAt < PROVIDERS_RETRY_MS) return null;
    const p = api.settings().then(
      (s) => {
        if (providersRead !== p) return; // a read from before the settings changed
        if (alive && s && Array.isArray(s.providers)) providers = s.providers;
        providersFailedAt = 0;
      },
      () => {
        if (providersRead !== p) return;
        providersFailedAt = Date.now();
        providersRead = null;
      },
    );
    providersRead = p;
    return p;
  }

  // What the providers change in the failure notices: redrawn after a read
  // only when this changes (not an alert announced again for nothing).
  const providersKey = (lec) => JSON.stringify((lec.stages || []).filter((s) => U.isFinalFailure(s)).map((s) => U.settingsFix(s, providers)));

  // Reads them for lec and draws the header again once they came, if they
  // changed what it says.
  function readThenRedraw(lec) {
    const before = providersKey(lec);
    const p = readProviders(lec);
    if (p) p.then(() => alive && lecture && providersKey(lecture) !== before && renderHead());
    return p;
  }

  // Waits for the providers lec needs, at most PROVIDERS_WAIT_MS; past that
  // the caller draws without them and they draw again when they come.
  function waitProviders(lec) {
    const before = providersKey(lec);
    const p = readProviders(lec);
    if (!p) return Promise.resolve();
    // waited for once already: the draw that waited redraws when it comes
    if (p === providersWaited) return Promise.resolve();
    let late = false;
    let settled = false;
    p.then(() => {
      settled = true;
      if (late && alive && lecture && providersKey(lecture) !== before) renderHead();
    });
    return Promise.race([p, new Promise((r) => setTimeout(r, PROVIDERS_WAIT_MS))]).then(() => {
      late = true;
      if (!settled) providersWaited = p;
    });
  }

  function renderHead() {
    const lec = lecture;
    const course = ctx.courses().find((c) => c.id === lec.course_id);
    // (a lecture an action answered with: drawn now, again with them)
    if (!providersRead) readThenRedraw(lec);
    preserveFocus(() => {
      clear(headEl);
      bar = pipelineBar(lec.stages, {
        key: `lecd:${lec.id}`,
        lecture: lec.id, // a remote asr's live steps (pipeline.js)
        size: 'lg',
        sys: ctx.system,
        res: ctx.resources,
        providers: () => providers, // a model the provider no longer lists (T24 S3)
        stream: lec.stream, // stream-parallel: the playback file with prep
        onAction: (stage, action) => A.stageAction(lec, stage, action, true).then((r) => r && handlers.onLectures([r])),
      });
      const sum = headSummary(lec);
      // a retry's countdown goes on by the second (3.2.0 L12)
      const sumText = document.createTextNode(sum.text);
      if (U.backoffCounting(lec.stages))
        everySecond(sumText, () => {
          sumText.data = headSummary(lec).text;
          return U.backoffCounting(lec.stages);
        });
      // R79b M4: so does the time left of a running stage — counted down,
      // and gone once it has run out or no progress came for two minutes
      // (pipeline.js etaMs) — only this text node, written when it changes
      // (the status is no live region: nothing is announced). The timer
      // ends with the run (the lecture read again draws a new header), the
      // page left or redrawn (the node out of the document), destroy().
      else if (U.runningStage(lec.stages))
        everySecond(sumText, () => {
          const text = headSummary(lec).text;
          if (sumText.data !== text) sumText.data = text;
          return lecture === lec && !!U.runningStage(lec.stages);
        });
      headSum = sumText; // the time left follows the progress events (onJob)
      const warn = U.warningHint(lec.stages);
      const tt = lectureTitles(lec);
      const sub = [t('lecture.page.created', { when: U.fmtDateTime(lec.created_at) })];
      if (lec.launched_at) sub.push(t('lecture.page.launched', { when: U.fmtDateTime(lec.launched_at) }));
      if (lec.duration_ms) sub.push(t('lecture.page.duration', { time: U.fmtClock(lec.duration_ms) }));
      headEl.append(
        h(
          'nav',
          { class: 'crumbs', 'aria-label': t('lecture.page.crumbs_aria') },
          h('a', { href: `#/course/${enc(lec.course_id)}`, dataset: { fkey: 'lecd:crumb' } }, icon('arrow-left', { size: 14 }), course ? h('span', null, course.code ? h('span', { class: 'mono crumbs-code' }, course.code) : null, course.name) : t('lecture.page.back_to_course')),
        ),
        h(
          'div',
          { class: 'page-head lec-titlebar' },
          h(
            'div',
            { class: 'page-titles' },
            h(
              'div',
              { class: 'page-eyebrow' },
              course && course.code ? h('span', { class: 'mono' }, course.code) : null,
              h(
                'button',
                {
                  type: 'button',
                  class: 'lec-kind',
                  title: t('lecture.page.kind_tip', { ref: U.lectureRef(course && course.code, lec.cite_label || lec.label) }),
                  dataset: { fkey: `lecd:${lec.id}:kind` },
                  onclick: () => openKindDialog(lec, { onSaved: (l) => handlers.onLectures([l]) }),
                },
                lectureTagText(lec, { form: 'full' }),
              ),
              lec.draft ? h('span', { class: 'chip chip-draft' }, t('lecture.page.draft')) : null,
              U.isAllDone(lec.stages) ? h('span', { class: 'chip chip-ok' }, icon('check', { size: 11 }), t('lecture.page.done')) : null,
            ),
            h('h1', { class: 'page-title', lang: tt.mainLang }, tt.main),
            // content languages (fonts, P14.5): the subtitle is the Chinese title
            tt.sub ? h('p', { class: 'lec-subtitle', lang: tt.subLang }, tt.sub) : null,
            tt.blurb ? h('p', { class: 'lec-blurb', title: tt.blurbFull, lang: tt.blurbLang }, tt.blurb) : null,
            h(
              'p',
              { class: 'page-sub' },
              sub.join(' · '),
              ' · ',
              // v2 P13.3.4: the recording time and where it came from (click to change)
              h(
                'button',
                { type: 'button', class: 'page-sub-link' + (lec.recorded_at ? '' : ' is-missing'), title: t('lecture.detail.recorded_edit'), dataset: { fkey: `lecd:${lec.id}:rec` }, onclick: () => openRecordedAtDialog(lec, { onSaved: (l) => handlers.onLectures([l]) }) },
                icon('calendar', { size: 13 }),
                lec.recorded_at ? t('lecture.detail.recorded', { when: fmtRecordedAt(lec), source: t(`calendar.recorded.source.${lec.recorded_at_source || 'none'}`) }) : t('lecture.detail.recorded_none'),
              ),
              lec.original_filename ? h('span', { class: 'lec-orig', title: lec.original_filename }, ' · ', t('lecture.detail.original', { name: lec.original_filename })) : null,
            ),
          ),
          h('div', { class: 'page-actions' }, headActions(lec)),
        ),
        h(
          'section',
          { class: 'card lec-panel', 'aria-label': t('lecture.page.panel_aria') },
          h(
            'div',
            { class: 'lec-pipe-row' },
            bar,
            h(
              'div',
              { class: 'lec-state' },
              h('span', { class: `lec-status tone-${sum.tone}` }, h('span', { class: `lec-status-dot is-${sum.tone}`, 'aria-hidden': 'true' }), sumText),
              warn ? h('span', { class: 'chip chip-warn', title: t('lecture.page.warning_tip') }, icon('triangle-alert', { size: 11 }), warn) : null,
            ),
          ),
          notices(lec),
          slides(lec),
          stats(lec),
          mixedModels(lec),
        ),
      );
    });
  }

  function renderTabs() {
    const n = lecture.counts.patches_pending;
    const sentences = lecture.counts.sentences;
    const mk = (key, label, ic, href, count, countCls) =>
      h(
        'a',
        { class: 'lec-tab' + (tab === key ? ' is-active' : ''), role: 'tab', 'aria-selected': tab === key ? 'true' : 'false', href, dataset: { fkey: `tab:${key}` } },
        icon(ic, { size: 15 }),
        label,
        count ? h('span', { class: `lec-tab-count ${countCls}` }, String(count)) : null,
      );
    preserveFocus(() => {
      clear(tabsEl);
      tabsEl.append(mk('transcript', t('lecture.tab.transcript'), 'file-text', `#/lecture/${enc(id)}`, sentences, ''), mk('review', t('lecture.tab.review'), 'list-checks', `#/lecture/${enc(id)}/review`, n, 'is-warn'));
    });
  }

  function mountTab() {
    if (sub) sub.destroy();
    Clip.stop();
    clear(panelEl);
    sub = tab === 'review' ? mountReview(panelEl, lecture, ctx) : mountTranscript(panelEl, lecture, ctx);
  }

  // total: the read also asks for usage_total (T25 S1); several reloads
  // within the delay are one read, which asks for it if any of them did.
  function scheduleReload(total = false) {
    if (total) wantTotal = true;
    clearTimeout(timer);
    timer = setTimeout(load, 250);
  }

  // A job that ended (a stage or task done, failed or cancelled): what the
  // lecture used in all may have grown (T25 S1).
  const FINAL = new Set(['done', 'failed', 'cancelled']);

  render(); // skeleton until the first response
  load();
  const onKinds = () => alive && lecture && renderHead(); // kind names (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  return {
    update(r) {
      if (r.name !== 'lecture' || r.id !== id) return false;
      if (r.tab !== tab) {
        tab = r.tab;
        if (lecture) {
          renderTabs();
          mountTab();
        }
      }
      return true;
    },
    refresh() {
      // (after the reload: a lecture deleted meanwhile shows "not found"
      // without the tab also failing on its sentences/patches); a job may
      // have ended meanwhile (the total, T25 S1)
      wantTotal = true;
      load().then(() => {
        if (alive && sub && !gone) sub.refresh();
      });
    },
    onJob(ev) {
      if (!lecture || ev.scope !== 'lecture' || ev.scope_id !== id) return;
      if (streamProgress(lecture, bar, ev)) {
        // the background playback file: its notice follows in place
        if (streamNoticeText && lecture.stream) streamNoticeText.textContent = t('lecture.notice.stream_generating', { pct: U.pct(lecture.stream.progress) });
        return;
      }
      const st = lecture.stages.find((s) => s.stage === ev.stage);
      // a job that ended, which a read before the event already showed:
      // the total over every run still has to be asked for (R72 L1)
      if (st && st.status === ev.status && FINAL.has(ev.status)) {
        scheduleReload(true);
        return;
      }
      if (st && st.status === ev.status && bar) {
        bar.setProgress(ev.stage, ev.progress);
        if (headSum) headSum.data = headSummary(lecture).text; // the time left (T25 S2)
        // T25 G3: the transcript's empty state says how far the
        // transcription is; it follows (cheap while there are no sentences)
        if (ev.stage === 'asr' && sub && !(lecture.counts && lecture.counts.sentences > 0)) sub.setLecture(lecture);
      } else {
        scheduleReload(FINAL.has(ev.status));
      }
    },
    onChanged(ev) {
      if (ev.kind === 'lecture' && ev.id === id && !gone) scheduleReload();
      // the providers or their probed models may have changed (WL-07):
      // the ones read are dropped, and read again if the page needs them;
      // several in a row once (WV-04)
      if (ev.kind === 'settings') {
        clearTimeout(settingsTimer);
        settingsTimer = setTimeout(() => {
          settingsTimer = 0;
          if (!alive) return;
          providersRead = null;
          providersFailedAt = 0;
          if (lecture && !gone && needsProviders(lecture)) readThenRedraw(lecture);
        }, SETTINGS_SOON_MS);
      }
      if (sub) sub.onChanged(ev);
    },
    onCoursesChanged() {
      if (lecture) renderHead();
    },
    onResources() {
      // The cost's currency comes from the providers (resources), and a
      // power wait's notice names a device that can take over; redraw the
      // header only when either text changes (they arrive every 2 s).
      if (lecture && (costKey(lecture) !== shownCost || waitNotices(lecture).map((w) => w.key).join('') !== shownWait)) renderHead();
    },
    destroy() {
      alive = false;
      window.removeEventListener('kindschange', onKinds);
      clearTimeout(timer);
      clearTimeout(settingsTimer);
      if (headSum) stopEverySecond(headSum); // the status's countdown (R79b M4)
      if (sub) sub.destroy();
      Clip.release(id);
    },
  };
}
