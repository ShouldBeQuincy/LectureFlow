// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Stage pipeline bar with hover cards (SPEC §9.5, §9.8; v1.1 A2.1, A12.1).
// Rounded segments, one per stage; the state is shown by colour *and*
// pattern (queued dots, running flow, stale / cancelled / outdated stripes).
// Stage names appear in the hover card, never inside the bar; the running
// stage's percentage is shown next to the bar. A queued asr stage the power
// policy put back in line keeps its checkpoint (StageView.checkpoint): its
// segment is filled that far and the share is shown, muted, next to the bar
// while nothing runs. Every action button comes from the stage's
// allowed_actions.
// SPEC-v3 V14.5: an asr stage that runs (or ran) on Google Colab carries a
// small remote mark on its segment; while it runs, its card lists the
// remote steps 等卡 → 上传音频 → 远端转写 → 下载结果 → 校验 with the current
// one marked and the time left. 3.2.0 fix: stats.remote is only what the
// lecture was loaded with (job events carry the progress alone, so the
// views do not reload while the stage runs, and the card stayed on 等卡);
// the steps now follow the device's card in the resources (remotes[].jobs[]
// of this lecture, opts.lecture, every 2 s), redrawn when the card opens and
// with every progress event, and fall back to stats.remote. The other lines
// that change while a stage runs or waits are redrawn the same way: the time
// it has run (it stayed at what it was when the lecture was loaded), the
// waiting reason (live numbers) and the tokens; and when a card opens on a
// running stage the resources do not list (or that counts tokens), or on a
// queued stage that waits, the lecture is read again — at most once per
// lecture in LECTURE_REFRESH_MS — and those lines follow it, without
// reloading the view (freshStage).
// stream-parallel: a source that must be transcoded gets its playback file
// from a background task after prep, in parallel with asr (opts.stream =
// Lecture.stream): while it runs the prep segment carries a small play mark,
// and prep's card has a "播放文件" line (generating n%, ready, failed…),
// updated in place by setProgress('stream', p).

import { h, toast, confirmDialog, copyText, everySecond, stopEverySecond } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t } from './i18n.js';
import * as C from './settings_colab.js';
import { api } from './api.js';

let cardSeq = 0;

// stages: []StageView. opts:
//   key       unique prefix for focus keys (e.g. "lec:{id}")
//   sys, res  () => latest System / Resources (live numbers in waiting_for texts)
//   onAction  (stage, action) => Promise, called after confirmation
//   size      "sm" (default) | "lg" (stage names under the bar)
//   pct       show the running percentage next to the bar (default true)
//   stream    Lecture.stream (the playback file), shown with prep
//   lecture   the lecture's id: a remote stage's live steps (remotes[].jobs[]),
//             and the job events' stats merged in (mergeJobEvent, v3.2.x)
//   fnext     focus keys for a card's action when its segment is gone too
//             (the view dropped the row), after the segment (dom.js preserveFocus)
//   providers () => the settings' providers the view read (with their probed
//             models), or null: a failure's model the provider no longer
//             lists (U.settingsFix, T24 S3); without it, not known
//   lazyCards build a card's content only when it first opens (hover, focus,
//             Enter or Space), for views with many bars (P2 hotspot 3: the
//             six cards were some 80% of a lecture row's elements); a stage
//             whose segment is described by its card (an error, a waiting
//             reason: R19) has its card built at once, as without it
//
// The bar is one Tab stop (T23 severe 2): the segments are a roving
// tabindex — the arrow keys, Home and End move between them, the one
// focused last (at first the one that matters: failed, running, …, a
// warning) keeps the Tab stop, also in the bar drawn anew with the same
// key (R79 L8). The focus on a segment opens its card as before, and Tab
// goes on into it; Shift+Tab from after the bar comes back through that
// card's controls to the segment, not past them.
export function pipelineBar(stages, opts) {
  const size = opts.size || 'sm';
  const segs = h('div', { class: 'pipe-segs' });
  const bar = h('div', { class: `pipe pipe-${size}`, role: 'toolbar', 'aria-label': opts.label || t('common.pipe.label') }, segs);
  const n = stages.length;
  stages.forEach((st, i) => segs.append(segment(st, i, n, opts)));
  rovingSegments(bar, segs, stages, opts.key);
  // The share next to the bar is the running stage's, not the lecture's
  // (T25 G9): the large bar (the lecture page) names the stage beside it
  // ("转写 42%"; it may take more room than the number alone), the small one
  // on hover; a run that reported no progress yet shows none (not "0%").
  // (the large bar's share takes the room it needs: .pipe-lg .pipe-pct)
  const pctEl = opts.pct === false ? null : h('span', { class: 'pipe-pct', 'aria-hidden': 'true' });
  if (pctEl) bar.append(pctEl);
  const syncPct = () => {
    if (!pctEl) return;
    const run = U.runningStage(stages);
    // nothing runs: the share a queued stage keeps in its checkpoint
    const ck = run ? null : stages.find((s) => U.checkpointProgress(s) != null);
    const of = run && Number(run.progress) > 0 ? run : ck;
    const share = of ? U.pct(run ? run.progress : U.checkpointProgress(ck)) : '';
    const stage = of ? U.stageName(of.stage) : '';
    pctEl.textContent = !share ? '' : size === 'lg' ? t('common.pipe.pct_stage', { stage, pct: share }) : share;
    pctEl.classList.toggle('is-ckpt', !!ck);
    if (ck) pctEl.setAttribute('title', U.stageStatusText(ck));
    else if (share) pctEl.setAttribute('title', t('common.pipe.pct_tip', { stage, pct: share }));
    else pctEl.removeAttribute('title');
  };
  syncPct();
  // In-place progress update from an SSE job event (no re-render, keeps hover/focus).
  // 'stream' (the background playback file) updates prep's card line.
  bar.setProgress = (stage, progress) => {
    if (stage === 'stream') {
      if (!streamGenerating(opts.stream)) return;
      opts.stream.progress = progress;
      const line = bar.querySelector('.card-stream');
      if (line) line.textContent = streamText(opts.stream);
      return;
    }
    const wrap = bar.querySelector(`.seg-wrap[data-stage="${CSS.escape(stage)}"]`);
    if (!wrap) return;
    const st = stages.find((s) => s.stage === stage);
    if (st) st.progress = progress;
    if (st && st.status === 'running') noteShare(runKey(st, opts), progress); // the time left (T25 S2)
    const fill = wrap.querySelector('.seg-fill');
    const p = st ? U.barProgress(st) : progress;
    if (fill) fill.style.width = p != null ? U.pct(p) : '';
    const status = wrap.querySelector('.card-status-text');
    if (status && st) status.textContent = U.stageStatusText(st);
    if (wrap.refresh) wrap.refresh(); // the lines that change while it runs follow (3.2.0)
    const btn = wrap.querySelector('.seg');
    if (btn && st) btn.setAttribute('aria-label', segLabel(st, opts));
    syncPct();
  };
  // v3.2.x P1-c: the statistics a job event carries (stats, llm,
  // last_activity_at) merged into the stage of the status the bar shows, and
  // its card redrawn; a status change is left to the view (it reloads).
  bar.mergeStats = (ev) => {
    const st = stages.find((s) => s.stage === ev.stage);
    if (!st || st.status !== ev.status) return false;
    mergeEventStats(st, ev);
    const wrap = bar.querySelector(`.seg-wrap[data-stage="${CSS.escape(ev.stage)}"]`);
    if (wrap && wrap.refresh) wrap.refresh();
    return true;
  };
  if (opts.lecture) watchBar(opts.lecture, bar);
  return bar;
}

// ---- one Tab stop per bar (T23 severe 2) ----

// The segment that holds the Tab stop when the bar is drawn: the stage that
// matters most now (failed, running, cancelled, out of date, queued), then
// a done stage with a warning (R79 L8: before the plain done ones; it is
// something to read, not something going on), else the first.
const TAB_STOP_ORDER = ['failed', 'running', 'cancelled', 'stale', 'queued'];
export function tabStopIndex(stages) {
  const i = mattersIndex(stages);
  return i >= 0 ? i : 0;
}

// The segment tabStopIndex picks for what it is (failed … queued, then a
// warning), or -1 when nothing in the bar matters more than another.
function mattersIndex(stages) {
  const list = stages || [];
  for (const s of TAB_STOP_ORDER) {
    const i = list.findIndex((st) => st.status === s);
    if (i >= 0) return i;
  }
  return list.findIndex((st) => U.hasWarning(st));
}

// That segment's stage and status ("translate:failed"), or null: the same
// stage in another state (running, then failed) is another thing that
// matters (R91 low-3).
function mattersStage(stages) {
  const i = mattersIndex(stages);
  return i >= 0 ? `${stages[i].stage}:${stages[i].status}` : null;
}

// R79 L8: the segment focused last in a bar, by the bar's key (opts.key:
// a lecture's, so never another lecture's bar): a view draws its bar anew
// after every read, and the new bar keeps the Tab stop there instead of
// going back to tabStopIndex. Bounded (the oldest key goes first).
// Round 9 WV-03 (R87 L1): with it what mattered most then (mattersStage:
// the segment tabStopIndex picked for what it is, with its status); once
// something else matters most (a new failure, a stage running, out of
// date, or the same stage in another state: running, then failed, R91
// low-3) the Tab stop goes back to it and the memory is dropped, until a
// segment is focused again. A bar where nothing matters any more keeps the
// memory (there is nothing to go back to), and forgets what mattered
// then, so the same stage failing again counts as new.
const lastFocused = new Map(); // bar key → {stage, top: mattersStage when focused}
const LAST_FOCUSED_MAX = 200;

function rememberFocused(key, stage, top) {
  if (!key || !stage) return;
  lastFocused.delete(key);
  if (lastFocused.size >= LAST_FOCUSED_MAX) lastFocused.delete(lastFocused.keys().next().value);
  lastFocused.set(key, { stage, top });
}

// The Tab stop of a bar drawn: the segment focused last in the bar of
// that key, while its stage is in the bar and what matters most is what
// mattered then; else tabStopIndex.
function startIndex(stages, key) {
  const mem = key ? lastFocused.get(key) : null;
  if (!mem) return tabStopIndex(stages);
  const top = mattersStage(stages);
  if (top !== null && top !== mem.top) {
    lastFocused.delete(key);
    return tabStopIndex(stages);
  }
  if (top === null) mem.top = null;
  const i = (stages || []).findIndex((st) => st.stage === mem.stage);
  return i >= 0 ? i : tabStopIndex(stages);
}

// The controls of a card in document order (its buttons, links and
// disclosure summaries).
function controlsOf(el, out = []) {
  for (const c of el.children || []) {
    if (!c.tagName) continue;
    if (c.tagName === 'BUTTON' || c.tagName === 'SUMMARY' || (c.tagName === 'A' && c.getAttribute('href'))) out.push(c);
    controlsOf(c, out);
  }
  return out;
}

// Whether node comes after el in the document (and is not inside it).
function follows(node, el) {
  if (!node || !el || typeof el.compareDocumentPosition !== 'function') return false;
  const pos = el.compareDocumentPosition(node);
  return !!(pos & 4) && !(pos & 16); // DOCUMENT_POSITION_FOLLOWING, not CONTAINED_BY
}

function rovingSegments(bar, segs, stages, key) {
  const buttons = () => [...segs.children].map((w) => w.querySelector('.seg')).filter(Boolean);
  const hold = (i) => buttons().forEach((b, j) => b.setAttribute('tabindex', j === i ? '0' : '-1'));
  hold(startIndex(stages, key));
  segs.addEventListener('keydown', (e) => {
    const btns = buttons();
    const i = btns.indexOf(e.target);
    if (i < 0 || e.altKey || e.ctrlKey || e.metaKey) return;
    const last = btns.length - 1;
    const j = e.key === 'ArrowRight' ? Math.min(last, i + 1) : e.key === 'ArrowLeft' ? Math.max(0, i - 1) : e.key === 'Home' ? 0 : e.key === 'End' ? last : -1;
    if (j < 0) return;
    e.preventDefault();
    if (j === i) return;
    hold(j);
    btns[j].focus();
  });
  // a segment the pointer pressed is focused by it: the focus stays there
  let pressedAt = -Infinity;
  segs.addEventListener('pointerdown', () => (pressedAt = Date.now()));
  segs.addEventListener('focusin', (e) => {
    const btns = buttons();
    const i = btns.indexOf(e.target);
    if (i < 0) return;
    hold(i);
    if (stages[i]) rememberFocused(key, stages[i].stage, mattersStage(stages)); // the next bar of this key starts here (WV-03: while what matters most stays)
    // Shift+Tab from after the bar: the segment's card opened with the
    // focus (its wrap's focusin ran first); its last control takes the
    // focus, so Shift+Tab goes back through the card to the segment
    const from = e.relatedTarget;
    if (!from || bar.contains(from) || Date.now() - pressedAt < 1000 || !follows(from, bar)) return;
    const card = e.target.parentNode && e.target.parentNode.querySelector('.seg-card');
    const ctrls = card ? controlsOf(card) : [];
    if (ctrls.length) ctrls[ctrls.length - 1].focus();
  });
}

// ---- job events with statistics (v3.2.x P1-c) ----

// The bars drawn for a lecture (opts.lecture), held weakly: a bar a view
// dropped is forgotten once it is gone from the page or collected.
const liveBars = new Map(); // lecture id → Set<WeakRef<bar>>

function watchBar(id, bar) {
  if (typeof WeakRef !== 'function') return;
  let set = liveBars.get(id);
  if (!set) liveBars.set(id, (set = new Set()));
  set.add(new WeakRef(bar));
}

// The fields of a job event that update a stage in place: stats when the
// event has them, and with them llm and last_activity_at (absent = none).
export function mergeEventStats(st, ev) {
  if (ev.stats) {
    st.stats = ev.stats;
    st.llm = ev.llm || undefined;
    st.last_activity_at = ev.last_activity_at || undefined;
    return;
  }
  if (ev.llm) st.llm = ev.llm;
  if (ev.last_activity_at) st.last_activity_at = ev.last_activity_at;
}

// mergeJobEvent(JobEvent) (app.js, before the view's own handler): a lecture
// job event that carries statistics updates the stage of every bar drawn for
// that lecture and redraws its card, without reading the lecture again.
// Events without them change nothing here.
export function mergeJobEvent(ev) {
  if (!ev || ev.scope !== 'lecture' || !(ev.stats || ev.llm || ev.last_activity_at)) return;
  const set = liveBars.get(ev.scope_id);
  if (!set) return;
  for (const ref of set) {
    const bar = ref.deref();
    if (bar && bar.isConnected) bar.seen = true;
    if (!bar || (bar.isConnected === false && bar.seen)) {
      set.delete(ref);
      continue;
    }
    bar.mergeStats(ev);
  }
  if (!set.size) liveBars.delete(ev.scope_id);
}

function segLabel(st, opts) {
  const label = t('common.pipe.seg_label', { stage: U.stageName(st.stage), status: U.stageStatusText(st) });
  if (streamMark(st, opts)) return U.clauseJoin([label, t('common.pipe.stream_bg_label')]);
  return C.stageOnColab(st) ? U.clauseJoin([label, t('common.pipe.remote_label')]) : label;
}

// ---- the playback file (stream-parallel) ----

function streamGenerating(stream) {
  return !!stream && stream.status === 'generating';
}

// prep's segment carries the mark while the playback file is made in the background
function streamMark(st, opts) {
  return st.stage === 'prep' && streamGenerating(opts && opts.stream);
}

// A job event of lec's playback-file task (stage "stream", one of lec.aux):
// a progress event of the status lec already shows updates lec and bar
// (when given) in place and returns true; anything else (a status change,
// another stage) returns false and the caller reloads as before. Keeps the
// minutes of a background transcode from reloading a view every 500 ms.
export function streamProgress(lec, bar, ev) {
  if (!lec || !ev || ev.stage !== 'stream') return false;
  const job = (lec.aux || []).find((a) => a.stage === 'stream');
  if (!job || job.status !== ev.status) return false;
  job.progress = ev.progress;
  if (bar) bar.setProgress('stream', ev.progress);
  else if (streamGenerating(lec.stream)) lec.stream.progress = ev.progress;
  return true;
}

// The "播放文件" line of prep's card (Lecture.stream.status).
function streamText(stream) {
  switch (stream.status) {
    case 'generating':
      return t('common.pipe.stream_generating', { pct: U.pct(stream.progress) });
    case 'ready':
      return stream.transcoded ? t('common.pipe.stream_ready_transcoded') : t('common.stream_status.ready');
    case 'pending':
      return t('common.pipe.stream_pending');
    case 'audio_only':
    case 'no_space':
    case 'failed':
      return t(`common.stream_status.${stream.status}`);
  }
  return '';
}

function segment(st, i, n, opts) {
  const fkey = `${opts.key}:seg:${st.stage}`;
  if (st.status === 'running') noteShare(runKey(st, opts), st.progress); // the time left (T25 S2)
  const ds = U.displayStatus(st);
  const cardId = `segcard-${++cardSeq}`;
  const btn = h(
    'button',
    {
      type: 'button',
      class: 'seg' + (U.hasWarning(st) ? ' seg-warn' : ''),
      dataset: { st: ds, fkey },
      'aria-label': segLabel(st, opts),
      // the card holds buttons, so it is no tooltip: the segment controls
      // it and says whether it is open (L16)
      'aria-controls': cardId,
      'aria-expanded': 'false',
    },
    h('span', { class: 'seg-bar' }, h('span', { class: 'seg-fill', style: { width: U.barProgress(st) != null ? U.pct(U.barProgress(st)) : '' } })),
  );
  const card = h('div', { class: 'seg-card' + (i >= n / 2 ? ' seg-card-end' : ''), id: cardId, role: 'group', 'aria-label': U.stageName(st.stage) });
  // the card shows only while the focus is in it or the pointer over it; a
  // dialog one of its buttons opened gives the focus back through the
  // segment, which opens it again (dom.js giveBack, T6 #1)
  card.dataset.fopener = fkey;
  // The card's content: built with the bar, or (opts.lazyCards, P2 hotspot
  // 3) when it first opens; a stage its card describes (below) has it built
  // at once, and one whose waiting reason appears later when it appears.
  let built = false;
  let errLine = null;
  let errLabel = null;
  let waitLine = null;
  const build = () => {
    if (built) return;
    built = true;
    card.append(cardBody(st, opts));
    // Tab reads why the stage failed or waits (R19): the segment is described
    // by the card's error line and waiting line, not by the whole card
    errLine = card.querySelector('.card-errmsg') || card.querySelector('.card-errtext');
    if (errLine) errLine.setAttribute('id', `${cardId}-err`);
    // with its label ("错误", "警告", "上次错误", R21): a queued stage's last
    // error is not heard as a failure now
    const errBox = errLine ? card.querySelector('.card-error') : null;
    errLabel = errBox ? errBox.querySelector('.card-k') : null;
    if (errLabel) errLabel.setAttribute('id', `${cardId}-errk`);
    // the waiting line by its value alone: its label ("等待") would be heard
    // twice, as the reason says it waits (T6 #8)
    waitLine = card.querySelector('.card-wait');
    const waitVal = waitLine ? waitLine.querySelector('.card-v') : null;
    if (waitVal) waitVal.setAttribute('id', `${cardId}-wait`);
  };
  const failed = st.status === 'failed';
  card.describe = () => describeSegment(btn, [errLabel, errLine], waitLine, failed);
  if (!opts.lazyCards || describedByCard(st)) build();
  card.describe();
  const remote = C.stageOnColab(st);
  if (remote) btn.append(h('span', { class: 'seg-remote', 'aria-hidden': 'true' }, icon('server', { size: 9 })));
  if (streamMark(st, opts)) {
    // the remote mark's look (settings_v3.css), placed on the segment like it
    btn.style.position = 'relative';
    btn.append(h('span', { class: 'seg-remote seg-bg', 'aria-hidden': 'true' }, icon('play', { size: 9 })));
  }
  const wrap = h('div', { class: 'seg-wrap' + (remote ? ' is-remote' : ''), dataset: { stage: st.stage } }, btn, card);
  if (opts.size === 'lg') wrap.append(h('span', { class: 'seg-name', 'aria-hidden': 'true' }, U.stageName(st.stage)));
  // a card not built yet is built once its segment is to be described by it
  // (a waiting reason appeared); true when it is built (refreshCard)
  card.ensure = () => {
    if (!built && describedByCard(st)) {
      build();
      card.describe();
    }
    return built;
  };
  // the lines that change while the stage runs (3.2.0): redrawn from st and
  // the latest system / resources
  wrap.refresh = () => refreshCard(card, st, opts);
  // build the card and bring it up to date, then open it upwards when there
  // is no room below it and room above it (cardOpensUp); read the lecture
  // again when only that can bring it up to date
  const flip = () => {
    build();
    wrap.refresh();
    wrap.classList.toggle('seg-up', cardOpensUp(wrap.getBoundingClientRect(), card.offsetHeight, window.innerHeight));
    const fresh = freshStage(st, opts);
    if (fresh)
      fresh.then((f) => {
        if (!f || f.status !== st.status) return; // a status change reloads the view anyway
        // the progress stays the job events' (newer than a stats flush)
        Object.assign(st, { stats: f.stats, waiting_for: f.waiting_for, started_at: f.started_at, llm: f.llm, last_activity_at: f.last_activity_at });
        wrap.refresh();
      });
  };
  // The card is open while the pointer is over the segment or the focus is
  // in it, unless Esc closed it. While it is open, a stage that runs or
  // waits has its lines redrawn every second (the time it has run, a
  // retry's countdown: 3.2.0 L12); a closed card runs no timer.
  let hovered = false;
  let focused = false;
  const isOpen = () => (hovered || focused) && !wrap.classList.contains('is-dismissed');
  const syncExpanded = () => btn.setAttribute('aria-expanded', String(isOpen()));
  const tick = () => {
    if (!isOpen()) return false;
    wrap.refresh();
    return true;
  };
  const opened = () => {
    // D2 (T16): one card at a time; the one open before closes as Esc
    // closes it (the focus stays on its segment, Enter or Space opens it
    // again, the next focus or hover too)
    if (openWrap && openWrap !== wrap && openWrap.isConnected) openWrap.dismiss();
    openWrap = wrap;
    flip();
    if (st.status === 'running' || st.waiting_for) everySecond(wrap, tick);
  };
  const closed = () => {
    syncExpanded();
    if (hovered || focused) return;
    wrap.classList.remove('is-dismissed'); // the next hover or focus opens it again
    stopEverySecond(wrap);
  };
  // Esc closes the card — under the pointer too (WCAG 1.4.13) — and leaves
  // the focus on the segment (3.2.0 L13; blur() left it on the page). true
  // when there was an open card to close.
  wrap.dismiss = () => {
    if (!isOpen()) return false;
    const a = document.activeElement;
    if (a && a !== btn && wrap.contains(a)) btn.focus({ preventScroll: true });
    wrap.classList.add('is-dismissed');
    stopEverySecond(wrap);
    syncExpanded();
    return true;
  };
  wrap.addEventListener('pointerenter', () => {
    hovered = true;
    hoveredWrap = wrap;
    watchEscape();
    wrap.classList.remove('is-dismissed');
    opened();
    syncExpanded();
  });
  wrap.addEventListener('pointerleave', () => {
    hovered = false;
    if (hoveredWrap === wrap) hoveredWrap = null;
    closed();
  });
  wrap.addEventListener('focusin', () => {
    if (!focused) wrap.classList.remove('is-dismissed'); // the focus came from outside
    focused = true;
    opened();
    syncExpanded();
  });
  wrap.addEventListener('focusout', (e) => {
    focused = !!(e.relatedTarget && wrap.contains(e.relatedTarget));
    closed();
  });
  wrap.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && wrap.dismiss()) e.stopPropagation();
  });
  // Enter or Space on the segment opens a card Esc closed (R19: it says
  // "collapsed", so it must open); an open card stays open on a click
  btn.addEventListener('click', () => {
    if (!wrap.classList.contains('is-dismissed')) return;
    wrap.classList.remove('is-dismissed');
    opened();
    syncExpanded();
  });
  return wrap;
}

/**
 * Whether a stage card opens upwards: only when it does not fit below its
 * segment (rect: the segment's getBoundingClientRect) and does fit above it.
 * A card that fits neither way opens downwards, where the page can scroll
 * to it; opened upwards near the top of the page its first lines (and a
 * tall card's buttons) were out of reach (T7, T8). cardHeight is the
 * card's measured height, about 300 when it cannot be measured yet; 20px
 * stand for the gap the narrow bars keep above the segment.
 */
export function cardOpensUp(rect, cardHeight, viewHeight) {
  const need = cardHeight > 0 ? cardHeight : 300;
  return rect.bottom + need > viewHeight && rect.top >= need + 20;
}

// The segment's description: the ids of its card's error label and line and
// of the waiting line while that shows a reason; no attribute when there is
// none. A failed stage says its error first; a queued or running one its
// waiting reason, then the last error (R21).
function describeSegment(btn, errParts, waitLine, failed) {
  const err = errParts.filter(Boolean).map((e) => e.getAttribute('id'));
  const waitVal = waitLine ? waitLine.querySelector('.card-v') : null;
  const wait = waitVal && waitLine.style.display !== 'none' ? [waitVal.getAttribute('id')] : [];
  const ids = failed ? [...err, ...wait] : [...wait, ...err];
  if (ids.length) btn.setAttribute('aria-describedby', ids.join(' '));
  else btn.removeAttribute('aria-describedby');
}

// Whether a stage's segment is described by lines of its card (R19, R21):
// an error (or warning) or a waiting reason. Such a card is built with the
// bar even where cards are built when they open (opts.lazyCards).
function describedByCard(st) {
  return !!st.error || !!st.waiting_for;
}

// Esc while the pointer rests on a segment (the focus elsewhere) closes its
// card (3.2.0 L13); one listener for every bar, added with the first hover.
let hoveredWrap = null;
let escapeWatched = false;
// The segment whose card opened last (T16 D2: one card open at a time).
let openWrap = null;
function watchEscape() {
  if (escapeWatched) return;
  escapeWatched = true;
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !hoveredWrap) return;
    if (!hoveredWrap.isConnected) {
      hoveredWrap = null;
      return;
    }
    hoveredWrap.dismiss();
  });
}

// The text of key with the params named in whole each in an unbreakable
// span (the translation decides the order and the words around them).
const MARK = '\uE000';
function keptWhole(key, params, whole) {
  const marked = { ...params };
  whole.forEach((k, i) => (marked[k] = `${MARK}${i}${MARK}`));
  return t(key, marked)
    .split(MARK)
    .map((s, i) => (i % 2 ? h('span', { class: 'nowrap' }, String(params[whole[Number(s)]])) : s))
    .filter((x) => x !== '');
}

function cardBody(st, opts) {
  const sys = opts.sys ? opts.sys() : null;
  const res = opts.res ? opts.res() : null;
  const ds = U.displayStatus(st);
  const tone = U.hasWarning(st) ? 'warn' : ds;
  const rows = [];
  rows.push(
    h(
      'div',
      { class: 'card-head' },
      h('strong', { class: 'card-stage' }, U.stageName(st.stage)),
      h('span', { class: `card-status st-${tone}` }, h('span', { class: 'card-dot', dataset: { st: ds } }), h('span', { class: 'card-status-text', text: U.stageStatusText(st) })),
    ),
  );
  if (ds === 'outdated') rows.push(h('p', { class: 'card-note card-note-outdated' }, t('stale.outdated_hint')));
  if (st.status === 'stale') {
    const reason = U.stageStaleText(st) || t('stale.reason.input_changed');
    rows.push(h('p', { class: 'card-note card-note-stale' }, t('stale.card', { reason })));
  }
  // the lines that change while the stage runs or waits are redrawn in place
  // (refreshCard, 3.2.0); a queued stage's waiting reason may appear later
  if (st.waiting_for || st.status === 'queued') rows.push(liveLine(t('common.pipe.k_wait'), U.stageWaitingText(st, sys, res), 'card-wait'));
  // SPEC-v3 V14.5: the remote steps of a Colab (or SSH) run, live (3.2.0)
  const rs = remoteNow(st, opts);
  if (st.status === 'running') rows.push(h('div', { class: 'card-remote', style: { display: 'contents' } }, ...(rs ? remoteSteps(rs, sys, res) : [])));
  // power management: how far the checkpoint of a queued stage reaches (the
  // status above says how much is done)
  if (U.checkpointProgress(st) != null) {
    const ck = st.checkpoint;
    rows.push(kv(t('common.pipe.k_checkpoint'), t('common.pipe.checkpoint', { at: U.fmtClock(ck.resume_ms), total: U.fmtClock(ck.duration_ms) })));
  }
  const note = U.noteText(st);
  if (note) rows.push(kv(t('common.pipe.k_note'), note));
  // stream-parallel: the playback file, made after prep (in the background for a transcode)
  if (st.stage === 'prep' && opts.stream && streamText(opts.stream)) {
    rows.push(kv(U.stageName('stream'), h('span', { class: 'card-stream' }, streamText(opts.stream))));
  }
  if (st.attempt > 0) rows.push(kv(t('common.pipe.k_attempts'), t('common.pipe.attempts', { n: st.attempt })));
  // v3.2.x P1-c: the requests of the run (attempt, countdown, last outcome,
  // last activity), redrawn in place; empty for jobs without them
  rows.push(h('div', { class: 'card-llm', style: { display: 'contents' } }, ...liveRows(st)));
  const time = timeText(st);
  if (time || st.status === 'running') rows.push(liveLine(t('common.pipe.k_time'), time, 'card-time'));
  // T25 S2: about how long a running stage still takes, under the time it
  // has run (hidden until it can be told)
  if (st.status === 'running') rows.push(liveLine('', stageEta(st, opts), 'card-eta'));
  // tokens: a running stage's first usage arrives with a stats flush
  const hasUsage = U.hasUsage(st.stats);
  if (hasUsage || st.status === 'running') rows.push(liveLine(t('common.pipe.k_tokens'), hasUsage ? U.usageText(st.stats) : '', 'card-tokens'));
  // v3.2.x P1-c: the tokens of each attempt worth showing, one line each
  rows.push(h('div', { class: 'card-attempt-tokens', style: { display: 'contents' } }, ...attemptTokenRows(st)));
  // asr (SPEC-v2 I6): "device · model · precision · language"; an LLM
  // snapshot with its output limit and "临时指定" (v3.2.x P1-b)
  if (st.model && st.model.label) rows.push(kv(st.stage === 'asr' ? t('common.pipe.k_device') : t('common.pipe.k_model'), modelValue(st.model)));
  // v3.2.x P1-b: the translations come from more than one model
  const mixed = mixedModelsNote(st);
  if (mixed) rows.push(mixed);
  // power management: a run resumed from its checkpoint on another device
  // (snapshot parts), one line per part: time range · device · model · precision
  const parts = st.stage === 'asr' ? U.asrParts(st.model) : null;
  if (parts) {
    // a line breaks only between the device name's words, never inside a
    // time or "model · precision" (large-v3-turbo has hyphens)
    const lines = parts.map((p) =>
      h('span', { class: 'card-part' }, keptWhole('common.pipe.part', { from: U.fmtClock(p.from_ms), to: U.fmtClock(p.to_ms), device: U.deviceLabel(p.device_id, sys, res), combo: U.comboText(p.model, p.precision) }, ['from', 'to', 'combo'])),
    );
    rows.push(kv(t('common.pipe.k_parts'), lines));
  }
  // asr (phase 3b): the worker's timing of the last run, i.e. transcription
  // time and audio length, RTF, model load (first compile noted)
  if (st.stage === 'asr' && st.asr_timing) {
    const tm = U.asrTimingText(st.asr_timing);
    if (tm.time) rows.push(kv(t('common.pipe.k_asr_time'), tm.time));
    if (tm.speed) rows.push(kv(t('common.pipe.k_speed'), tm.speed));
    if (tm.load) rows.push(kv(t('common.pipe.k_load'), tm.load));
  }
  // T24 S3: where in Settings a failure's way out is (the providers the
  // view read tell a model the provider no longer lists; opts.providers)
  const fix = U.isFinalFailure(st) ? U.settingsFix(st, opts.providers ? opts.providers() : null) : null;
  if (st.error) {
    const warn = U.hasWarning(st);
    const coded = st.error_info || (warn && st.warning_info) ? U.stageErrorText(st) : '';
    const label = st.status === 'failed' ? t('common.pipe.k_error') : warn ? t('common.pipe.k_warning') : t('common.pipe.k_last_error');
    rows.push(
      h(
        'div',
        { class: warn ? 'card-error card-warning' : 'card-error' },
        h('div', { class: 'card-k' }, label),
        // v2 P14.3: the coded error in the UI language above the original text
        coded && coded !== st.error ? h('p', { class: 'card-errmsg' }, coded) : null,
        // test-media I6: a damaged file is not fixed by retrying it
        st.status === 'failed' && U.retryFutile(st) ? h('p', { class: 'card-errhint' }, t('common.pipe.corrupt_hint')) : null,
        // T24 S3, T22 G8: a model the provider no longer lists, said in full
        fix && fix.unlisted ? h('p', { class: 'card-errhint' }, t('common.pipe.model_unlisted', fix.unlisted)) : null,
        // v3.2.x: the output limits the truncation ladder went through
        triedLine(st),
        // A warning is shown in full; a failure shows its first 300
        // characters (SPEC §9.8). Under a coded text the server's own words
        // (Chinese whatever the UI language, and saying it again) are folded
        // into "技术详情" (T24 G2; v2 P14.3 "查看详情"); alone, they show.
        errText(st, warn, coded && coded !== st.error, opts),
        h(
          'button',
          {
            type: 'button',
            class: 'btn btn-sm btn-quiet',
            dataset: { fkey: `${opts.key}:copy:${st.stage}` },
            onclick: async () => {
              const ok = await copyText(st.error);
              toast(ok ? t('common.copied_all') : t('common.copy_failed'), ok ? 'success' : 'warn');
            },
          },
          icon('copy', { size: 13 }),
          warn ? t('common.pipe.copy_warning') : t('common.pipe.copy_error'),
        ),
      ),
    );
  }
  // a retry that cannot succeed (U.retryFutile) is offered, but not as the
  // primary action; nor where another model comes first (T24 B1) or the
  // way out is in Settings (T24 S3)
  const futile = U.retryFutile(st);
  const modelFirst = modelRetryFirst(st, fix);
  const settingsFirst = !!fix && !modelFirst;
  const actions = cardActions(st, fix).map((a) =>
    h(
      'button',
      {
        type: 'button',
        class: actionClass(a, futile, modelFirst, settingsFirst),
        // gone once done (the stage changed): the focus goes back to the
        // segment (L14), else where the view says (the row is gone)
        dataset: { fkey: `${opts.key}:act:${st.stage}:${a}`, fnext: [`${opts.key}:seg:${st.stage}`, ...(opts.fnext || [])].join(' ') },
        onclick: () => runAction(st, a, opts),
      },
      // use_colab: "改用 Colab 处理" (colabfix; its own confirmation, actions.js);
      // rerun_text_only: "修复断句" (textrepair-int; confirmTextRepair)
      icon(a === 'retry' || a === 'retry_model' ? 'rotate-cw' : a === 'cancel' ? 'ban' : a === 'use_colab' ? 'upload' : a === 'rerun_text_only' ? 'wand-sparkles' : 'refresh-cw', { size: 13 }),
      U.actionName(a),
    ),
  );
  // "打开设置" (T24 S3): first where it is the way out, else after the buttons
  const go = fix ? settingsLink(fix, `${opts.key}:act:${st.stage}:settings`, settingsFirst) : null;
  const items = opts.onAction ? actions : [];
  if (go) settingsFirst ? items.unshift(go) : items.push(go);
  if (items.length) rows.push(h('div', { class: 'card-actions' }, items));
  return h('div', { class: 'seg-card-body' }, rows);
}

// The server's text of a stage's error or warning (st.error): shown as it
// is, or folded under "技术详情" (folded: a translated text shows above it).
// R72 L3: a section opened stays open when its card is drawn again (a view
// redraws after every read), and its summary has a focus key, so a focus
// on it comes back (dom.js preserveFocus). Bar key and stage → open.
const openDetails = new Set();

function errText(st, warn, folded, opts) {
  const pre = h('pre', { class: 'card-errtext', text: warn ? st.error : U.excerpt(st.error, 300) });
  if (!folded) return pre;
  const key = `${opts.key}:details:${st.stage}`;
  // (the margin is the text's own, which a closed section does not show:
  // .card-errdetail in style.css)
  const det = h('details', { class: 'card-errdetail' }, h('summary', { dataset: { fkey: key } }, t('common.pipe.tech_details')), pre);
  if (openDetails.has(key)) det.open = true;
  det.addEventListener('toggle', () => {
    if (det.open) {
      if (openDetails.size >= 100) openDetails.delete(openDetails.values().next().value);
      openDetails.add(key);
    } else openDetails.delete(key);
  });
  return det;
}

const REMOTE_STEPS = ['wait_accelerator', 'upload_audio', 'remote_transcribe', 'download_result', 'verify'];

// The card lines that change while a stage runs or waits (3.2.0), redrawn in
// place: the time it has run, the waiting reason (live numbers of system /
// resources, e.g. free VRAM), the tokens and the remote steps. These lines
// are built for every stage that may show them (hidden while empty), so a
// value that appears later has its place; the other lines change only with
// the status, which reloads the view.
function refreshCard(card, st, opts) {
  if (card.ensure && !card.ensure()) return; // not built yet (opts.lazyCards)
  const sys = opts.sys ? opts.sys() : null;
  const res = opts.res ? opts.res() : null;
  setLine(card.querySelector('.card-time'), timeText(st));
  setLine(card.querySelector('.card-eta'), stageEta(st, opts)); // T25 S2
  setLine(card.querySelector('.card-wait'), U.stageWaitingText(st, sys, res)); // a retry counts down (L12)
  setLine(card.querySelector('.card-tokens'), U.hasUsage(st.stats) ? U.usageText(st.stats) : '');
  const box = card.querySelector('.card-remote');
  const rs = remoteNow(st, opts);
  if (box) box.replaceChildren(...(rs ? remoteSteps(rs, sys, res) : []));
  const llm = card.querySelector('.card-llm');
  if (llm) llm.replaceChildren(...liveRows(st));
  const per = card.querySelector('.card-attempt-tokens');
  if (per) per.replaceChildren(...attemptTokenRows(st));
  if (card.describe) card.describe(); // a waiting reason may have appeared or gone
}

// ---- the requests of a run (v3.2.x P1-c) ----

// The thinking level an attempt sent (off: '').
function thinkingText(level) {
  return level === 'low' || level === 'high' || level === 'max' ? t(`common.model.thinking_${level}`) : '';
}

// "第 2 次尝试 · claude-opus-5-5 · max_tokens 32,768 · 深度思考 高"
export function attemptHead(a) {
  return [t('common.pipe.attempt_n', { n: a.n || 1 }), a.model || '', a.max_tokens ? maxTokensText(a.max_tokens) : '', thinkingText(a.thinking)].filter(Boolean).join(' · ');
}

// "12 秒后进行第 3 次尝试" while the request waits out a backoff
// (current.retry_at), "即将进行第 3 次尝试" once the time is up; '' otherwise.
export function retryCountdown(cur, now = Date.now()) {
  if (!cur || !cur.retry_at) return '';
  const at = new Date(cur.retry_at).getTime();
  if (!isFinite(at)) return '';
  const s = Math.ceil((at - now) / 1000);
  const n = (cur.n || 1) + 1;
  return s > 0 ? t('common.pipe.retry_in', { s, n }) : t('common.pipe.retry_due', { n });
}

// "已尝试的输出上限：16,000 → 32,000（深度思考 高）": the truncation ladder of
// a stage that failed with llm.truncated (error_info.params.tried, "level/
// max_tokens" per attempt, comma separated; 0 = the server's default limit);
// '' for any other error or without the param.
export function triedText(info) {
  const raw = info && info.code === 'llm.truncated' && info.params ? info.params.tried : '';
  if (typeof raw !== 'string' || !raw.trim()) return '';
  const steps = raw.split(',').map((x) => x.trim()).filter(Boolean).map((x) => {
    const m = /^([a-z]*)\/(\d+)$/.exec(x);
    if (!m) return x;
    const n = Number(m[2]);
    const limit = n > 0 ? U.fmtInt(n) : t('common.pipe.tried_server_default');
    const thinking = thinkingText(m[1]);
    return thinking ? t('common.pipe.tried_step', { limit, thinking }) : limit;
  });
  return steps.length ? t('common.pipe.tried_limits', { list: steps.join(' → ') }) : '';
}

function triedLine(st) {
  const text = st.status === 'failed' ? triedText(st.error_info) : '';
  return text ? h('p', { class: 'card-tried' }, text) : null;
}

// What a finished attempt came to.
export function outcomeText(a) {
  if (!a || !a.outcome) return '';
  switch (a.outcome) {
    case 'truncated':
      return a.empty_text ? t('common.pipe.outcome.truncated_thinking') : t('common.pipe.outcome.truncated');
    case 'http_error':
      return t('common.pipe.outcome.http_error', { status: a.status ? String(a.status) : '?' });
    case 'ok':
    case 'timeout':
    case 'network':
    case 'idle':
    case 'refused':
    case 'bad_response':
    case 'canceled':
      return t(`common.pipe.outcome.${a.outcome}`);
  }
  return a.outcome;
}

// "上次：输出被截断" when the last finished attempt did not end ok; ''.
export function lastOutcomeText(llm) {
  const list = (llm && llm.attempts) || [];
  const last = list[list.length - 1];
  if (!last || !last.outcome || last.outcome === 'ok') return '';
  return t('common.pipe.last_outcome', { outcome: outcomeText(last) });
}

// "42 秒前", "3 分 5 秒前": the time since iso ('' without one).
export function agoText(iso, now = Date.now()) {
  const at = iso ? new Date(iso).getTime() : NaN;
  if (!isFinite(at)) return '';
  const sec = Math.max(0, Math.floor((now - at) / 1000));
  if (sec < 60) return t('common.pipe.ago_s', { n: sec });
  return t('common.pipe.ago_ms', { m: Math.floor(sec / 60), s: sec % 60 });
}

// "已等待 02:10 / 超时 18:24": how long the request in flight has waited
// for its answer and when it times out; '' without both times.
export function waitedText(cur, now = Date.now()) {
  if (!cur || !cur.started_at || !(cur.timeout_ms > 0)) return '';
  const at = new Date(cur.started_at).getTime();
  if (!isFinite(at)) return '';
  return t('common.pipe.waited', { waited: U.fmtClock(Math.max(0, now - at)), timeout: U.fmtClock(cur.timeout_ms) });
}

// "第 1 次：输入 1,840 · 输出 2,000 · 缓存命中 0 · 输出被截断"
export function attemptTokensText(a) {
  return t('common.pipe.attempt_tokens', {
    n: a.n || 1,
    input: U.fmtTokens(a.prompt_tokens || 0),
    output: U.fmtTokens(a.completion_tokens || 0),
    cached: U.fmtTokens(a.cache_hit_tokens || 0),
    result: outcomeText(a),
  });
}

// The card lines of the run's requests (st.llm) and its last activity. One
// request in flight (summary) shows its attempt, model and limit; several
// (translate, repair) their number and the retries so far. Then the
// countdown to the next attempt or what a streamed request is doing, how
// long the request has waited (to tell slow from stuck), the last outcome
// that was not ok, and the last sign of life of a run that runs or waits.
// Translate: the sentences left pending (stats.pending_sentences, when the
// view carries it). Jobs without these fields: no lines.
export function liveRows(st, now = Date.now()) {
  const rows = [];
  const llm = st.llm;
  if (llm) {
    const cur = llm.current;
    const many = llm.in_flight > 1;
    const k = t('common.pipe.k_request');
    const label = () => (rows.length ? '' : k);
    if (many) rows.push(kv(k, t('common.pipe.in_flight', { n: llm.in_flight, retries: llm.retries || 0 }), 'card-inflight'));
    else if (cur) rows.push(kv(k, attemptHead(cur), 'card-attempt'));
    if (cur && cur.retry_at) rows.push(kv(label(), retryCountdown(cur, now), 'card-countdown'));
    else if (cur && (cur.phase === 'thinking' || cur.phase === 'writing')) rows.push(kv(label(), t(`common.pipe.phase_${cur.phase}`), 'card-phase'));
    const last = lastOutcomeText(llm);
    if (last) rows.push(kv(label(), last, 'card-last'));
    const waited = cur && !cur.retry_at && !many ? waitedText(cur, now) : '';
    if (waited) rows.push(kv(t('common.pipe.k_waited'), waited, 'card-waited'));
  }
  const live = st.status === 'running' || !!st.waiting_for;
  const ago = live ? agoText(st.last_activity_at, now) : '';
  if (ago) rows.push(kv(t('common.pipe.k_activity'), ago, 'card-activity'));
  const pending = st.stats && st.stats.pending_sentences;
  if (st.stage === 'translate' && pending > 0) rows.push(kv(t('common.pipe.k_pending'), t('common.pipe.pending_sentences', { n: pending }), 'card-pending'));
  return rows;
}

// ---- a lecture's translations from several models (v3.2.x P1-b) ----

// The note naming the models a translate stage's translations come from
// (stats.mixed_models: more than one, after pending sentences were
// retranslated with the current setting's model); '' otherwise, also for
// views without the field. The lecture page shows the same text.
export function mixedModelsText(st) {
  const list = st && st.stage === 'translate' && st.stats ? st.stats.mixed_models : null;
  return Array.isArray(list) && list.length > 1 ? t('common.pipe.mixed_models', { models: U.listJoin(list) }) : '';
}

// The card's neutral line for mixedModelsText (no warning tone), or null.
function mixedModelsNote(st) {
  const text = mixedModelsText(st);
  return text ? h('p', { class: 'card-note card-mixed-models' }, text) : null;
}

// The tokens of each attempt worth showing (llm.attempts), under the totals.
export function attemptTokenRows(st) {
  const list = (st.llm && st.llm.attempts) || [];
  if (!list.length) return [];
  return [kv(t('common.pipe.k_attempt_tokens'), list.map((a) => h('span', { class: 'card-attempt-line' }, attemptTokensText(a))), 'card-per-attempt')];
}

// ---- the model of a retry (v3.2.x P1-b) ----

// The model line's value: the snapshot's label, its output limit when one
// was set, and "临时指定" for a model named by hand for one retry.
function modelValue(m) {
  const mt = m.params && m.params.max_tokens;
  const label = mt ? U.modelLabel(m) + ' · ' + maxTokensText(mt) : U.modelLabel(m);
  if (m.source !== 'manual') return label;
  return [label, ' ', h('span', { class: 'chip chip-quiet card-manual' }, t('common.pipe.manual_tag'))];
}

export function maxTokensText(n) {
  return t('common.pipe.max_tokens', { value: U.fmtInt(n) });
}

// The stages whose retry may name another model.
export const MODEL_RETRY_STAGES = new Set(['summary', 'repair', 'translate']);

// Whether another model is the way out of a stage's failure: the same
// request cannot get past it (U.retryNeedsModel, T24 B1), or its model is
// one the provider no longer lists (fix.unlisted of U.settingsFix, T24 S3).
function needsModel(st, fix) {
  return !!st && st.status === 'failed' && (U.retryNeedsModel(st) || !!(fix && fix.unlisted));
}

// A card's buttons: the back end's allowed_actions, and right after "重试"
// of a failed or cancelled summary, repair or translate stage
// "换模型重试…" (the same retry, its model dialog opened whatever the
// snapshot) — right before it when another model is the way out
// (needsModel; fix: U.settingsFix of the stage, when known).
export function cardActions(st, fix = null) {
  const out = [];
  for (const a of st.allowed_actions || []) {
    out.push(a);
    if (a === 'retry' && MODEL_RETRY_STAGES.has(st.stage) && (st.status === 'failed' || st.status === 'cancelled')) {
      if (needsModel(st, fix)) out.splice(out.length - 1, 0, 'retry_model');
      else out.push('retry_model');
    }
  }
  return out;
}

// Whether a stage's card and notice offer "换模型重试…" as the primary
// action, before a plain "重试" (T24 B1): another model is the way out of
// its failure (needsModel), and its retry may name another model.
export function modelRetryFirst(st, fix = null) {
  return needsModel(st, fix) && cardActions(st, fix).includes('retry_model');
}

// The class of a card's action button. One action is primary: "换模型重试…"
// where it comes first (modelFirst); none of the others where "打开设置"
// is the way out (settingsFirst); else a retry that can succeed (not
// futile), or "改用 Colab 处理". "取消" is in the danger tone.
function actionClass(a, futile, modelFirst, settingsFirst) {
  if (modelFirst && (a === 'retry' || a === 'retry_model')) return a === 'retry_model' ? 'btn btn-sm btn-primary' : 'btn btn-sm';
  if (settingsFirst && a === 'retry') return 'btn btn-sm';
  return (a === 'retry' && !futile) || a === 'use_colab' ? 'btn btn-sm btn-primary' : a === 'cancel' ? 'btn btn-sm btn-danger' : 'btn btn-sm';
}

// "打开设置" (T24 S3, T22 G7): a link to the block of the settings page that
// fixes the failure (fix.href); primary when it is the way out.
export function settingsLink(fix, fkey, primary) {
  return h('a', { class: 'btn btn-sm' + (primary ? ' btn-primary' : ''), href: fix.href, dataset: { fkey } }, icon('settings', { size: 14 }), h('span', { class: 'btn-label' }, t('common.pipe.open_settings')));
}

// A card line (kv) with its value replaced; hidden while it has none.
function setLine(line, text) {
  if (!line) return;
  line.style.display = text ? '' : 'none';
  const v = line.querySelector('.card-v');
  if (v && text) v.textContent = text;
}

// A card that opens reads the lecture again (job events carry the progress
// alone; stats and waiting_for are what the lecture was loaded with): on a
// running stage whose device's card in the resources does not list the
// lecture (the remote steps fall back to stats.remote; tokens), or that
// counts tokens; on a queued stage with a waiting reason (the scheduler's
// reason changes without an event). At most once per lecture in
// LECTURE_REFRESH_MS; the promise of the stage as read (null when it could
// not be read), or null when there is nothing to read or it was read just
// now.
export const LECTURE_REFRESH_MS = 5000;
const lectureReads = new Map(); // lecture id → when it was last read

export function freshStage(st, opts) {
  const id = opts && opts.lecture;
  if (!id || !st) return null;
  // running: the resources do not list the lecture, it counts tokens, or the
  // lecture was loaded before the first stats flush of the run (no
  // stats.remote yet: the steps are the card's, its accelerator the stats')
  const want =
    st.status === 'running'
      ? !cardJob(opts) || U.hasUsage(st.stats) || !(st.stats && st.stats.remote)
      : st.status === 'queued' && !!st.waiting_for;
  if (!want) return null;
  const now = Date.now();
  const last = lectureReads.get(id);
  if (last != null && now - last < LECTURE_REFRESH_MS) return null;
  lectureReads.set(id, now);
  return api
    .lecture(id, { quiet: true })
    .then((lec) => (lec && Array.isArray(lec.stages) ? lec.stages.find((s) => s.stage === st.stage) || null : null))
    .catch(() => null);
}

// The lecture's job on its device's card in the resources, with the card
// (null: none).
function cardJob(opts) {
  const res = opts && opts.res ? opts.res() : null;
  const id = opts && opts.lecture;
  if (!id || !res || !Array.isArray(res.remotes)) return null;
  for (const card of res.remotes) {
    for (const j of (card && card.jobs) || []) if (j.lecture_id === id) return { job: j, card };
  }
  return null;
}

// The remote state of a running stage (null when it does not run remotely):
// for asr, its device's card in the resources (remotes[].jobs[] of
// opts.lecture) when it lists the lecture — live, even when the lecture was
// loaded before the run's first stats flush (no stats.remote: the five
// steps, the card's accelerator when it names one) — else stats.remote (as
// the lecture was loaded, or read again when the card opened). kind and
// label say where it runs (remoteWhere).
export function remoteNow(st, opts) {
  if (!st || st.status !== 'running') return null;
  const rs = st.stats && st.stats.remote;
  const on = st.stage === 'asr' ? cardJob(opts) : null;
  if (!on) return rs ? { ...rs, kind: deviceKind(st) } : null;
  const { job: j, card } = on;
  const m = cardStep(j.stage);
  return {
    steps: REMOTE_STEPS,
    ...rs,
    accelerator: (rs && rs.accelerator) || cardAccelerator(card),
    kind: card.kind || deviceKind(st),
    label: card.label || '',
    step: m.step,
    stage: m.stage,
    // waiting reasons are the stage's own (stats); only while it waits
    waiting_for: m.step === 'wait_accelerator' && rs ? rs.waiting_for : undefined,
    eta_sec: typeof j.eta_sec === 'number' ? j.eta_sec : -1,
    // v3.2.x: reconnecting to the device, live from its card (the same
    // value as stats.remote.reconnect, which may lag behind)
    reconnect: j.reconnect || undefined,
  };
}

// The accelerator a device's card names: Colab's session (Card.colab is its
// colab.Status), an SSH server's name (stats.remote.accelerator of an SSH
// run is the device's label too); '' when it names none.
function cardAccelerator(card) {
  if (card.kind === 'ssh') return card.label || '';
  return (card.colab && card.colab.accelerator) || '';
}

// "ssh" for a stage bound to an SSH server, else "colab".
function deviceKind(st) {
  const a = st.model && st.model.asr;
  return a && typeof a.device_id === 'string' && a.device_id.startsWith('ssh:') ? 'ssh' : 'colab';
}

// Where a remote run is: an SSH server by its name; Colab with its
// accelerator, or the card's label ("Google Colab") while the accelerator
// is not known; '' when nothing says (the line is left out).
function remoteWhere(rs) {
  if (rs.kind === 'ssh') return rs.accelerator || rs.label || '';
  if (rs.accelerator) return t('common.pipe.remote_where', { accelerator: rs.accelerator });
  return rs.label || '';
}

// A card job's stage as a remote step and the worker's stage: SSH cards name
// the step itself; Colab's name the worker's stage while it transcribes
// (check, install, download_model, load_model, transcribe, write) and wait,
// upload, download or verify otherwise (internal/resources/remotes.go).
export function cardStep(stage) {
  if (REMOTE_STEPS.includes(stage)) return { step: stage, stage: '' };
  switch (stage) {
    case 'wait':
    case '':
    case undefined:
    case null:
      return { step: 'wait_accelerator', stage: '' };
    case 'upload':
      return { step: 'upload_audio', stage: '' };
    case 'download':
      return { step: 'download_result', stage: '' };
    case 'verify':
      return { step: 'verify', stage: '' };
  }
  return { step: 'remote_transcribe', stage };
}

// The five remote steps (stats.remote.steps, in order) with the current one
// marked, its worker stage or waiting reason, and the time left.
export function remoteSteps(rs, sys, res) {
  const steps = rs.steps && rs.steps.length ? rs.steps : REMOTE_STEPS;
  const at = steps.indexOf(rs.step);
  const where = remoteWhere(rs);
  const out = where ? [kv(t('common.pipe.k_remote'), where, 'card-where')] : [];
  const items = steps.map((s, i) => {
    const state = at < 0 ? 'todo' : i < at ? 'done' : i === at ? 'now' : 'todo';
    let detail = '';
    if (i === at && rs.waiting_for) detail = U.waitingForText(rs.waiting_for, sys, res);
    else if (i === at && rs.stage && rs.stage !== 'transcribe' && s === 'remote_transcribe') detail = C.stageName(rs.stage);
    return h(
      'li',
      { class: `rstep is-${state}`, 'aria-current': state === 'now' ? 'step' : undefined },
      h('span', { class: 'rstep-dot', 'aria-hidden': 'true' }, state === 'done' ? icon('check', { size: 10 }) : null),
      h('span', { class: 'rstep-name' }, C.stepName(s)),
      detail ? h('span', { class: 'rstep-detail' }, detail) : null,
    );
  });
  out.push(h('ol', { class: 'rsteps', 'aria-label': t('common.pipe.k_remote_steps') }, items));
  // v3.2.x: the Colab runtime does not answer and the lecture tries again
  const rc = rs.reconnect;
  if (rc && rs.kind !== 'ssh')
    out.push(h('p', { class: 'card-note card-reconnect' },t('common.pipe.remote_reconnecting', { attempt: rc.attempt, of: rc.of })));
  if (rs.eta_sec > 0) out.push(kv(t('common.pipe.k_remote_eta'), C.durText(rs.eta_sec)));
  return out;
}

function kv(k, v, cls) {
  return h('div', { class: cls ? `card-line ${cls}` : 'card-line' }, h('span', { class: 'card-k' }, k), h('span', { class: 'card-v' }, v));
}

// A card line refreshCard redraws (class cls), hidden while text is empty.
function liveLine(k, text, cls) {
  const line = kv(k, text, cls);
  if (!text) line.style.display = 'none';
  return line;
}

function timeText(st) {
  if (st.duration_ms != null) return U.fmtDuration(st.duration_ms);
  if (st.status === 'running' && st.started_at) {
    const ms = Date.now() - new Date(st.started_at).getTime();
    if (ms >= 0) return t('common.pipe.elapsed', { time: U.fmtDuration(ms) });
  }
  return '';
}

// ---- about how long a run still takes (T25 S2, T22 G2) ----
//
// A running stage's share over time, as this page saw it: from the first
// share above 0 (not the run's start: a run that goes on from a checkpoint
// jumps there at once, and the page may open in the middle of a run) to the
// latest. The time left is the share left at the rate seen in between,
// counted down from the latest share; none until ETA_MIN_MS and
// ETA_MIN_SHARE have passed (the first estimates jump about), nor for a run
// without shares. Kept per run (lecture or bar key, stage, start), so a
// view drawn again goes on with it.
export const ETA_MIN_MS = 20000;
export const ETA_MIN_SHARE = 0.02;
export const ETA_STALL_MS = 120000;
const runShares = new Map(); // run key → {t0, p0, t, p}

/** The key of a stage's run on a bar (opts.lecture, else opts.key). */
export function runKey(st, opts) {
  return `${(opts && (opts.lecture || opts.key)) || ''}|${st.stage}|${st.started_at || ''}`;
}

/** Records a running stage's share p seen at now. */
export function noteShare(key, p, now = Date.now()) {
  const v = Number(p);
  if (!(v > 0)) return;
  const r = runShares.get(key);
  if (!r || v < r.p) {
    if (runShares.size >= 200) runShares.delete(runShares.keys().next().value);
    runShares.set(key, { t0: now, p0: v, t: now, p: v });
    return;
  }
  if (v > r.p) {
    r.t = now;
    r.p = v;
  }
}

/** The time a run still takes in ms (0: about done), or null when not known yet. */
export function etaMs(key, now = Date.now()) {
  const r = runShares.get(key);
  if (!r) return null;
  const span = r.t - r.t0;
  const done = r.p - r.p0;
  if (span < ETA_MIN_MS || done < ETA_MIN_SHARE || r.p >= 1) return null;
  // stalled (R79 M4): the countdown ran out, or no new share for
  // ETA_STALL_MS — no estimate until the next share (not "less than a
  // minute" for as long as the run waits)
  const since = now - r.t;
  const left = ((1 - r.p) * span) / done - since;
  if (left <= 0 || since > ETA_STALL_MS) return null;
  return Math.round(left);
}

/** "约还需 3 分钟" (rounded up to minutes), "不到 1 分钟"; '' when not known. */
export function etaText(ms) {
  if (ms == null || !isFinite(ms)) return '';
  if (ms < 60000) return t('common.pipe.eta_soon');
  const m = Math.ceil(ms / 60000);
  if (m < 60) return t('common.pipe.eta_m', { n: m });
  // an hour or more: in 5 minutes (a rough figure), whole hours alone (R79 L3)
  const r = Math.ceil(m / 5) * 5;
  const h = Math.floor(r / 60);
  // n: the hours, which a language may choose the words of eta_hm by (a
  // plural object, I64: Hindi घंटा / घंटे); the text shows {h} and {m}
  return r % 60 ? t('common.pipe.eta_hm', { h, m: r % 60, n: h }) : t('common.pipe.eta_h', { n: h });
}

/**
 * A running stage's time left as a text ('' when not known): its share
 * noted first. A remote transcription has none here: its card's steps have
 * the remote run's own (R79 L2).
 */
export function stageEta(st, opts, now = Date.now()) {
  if (!st || st.status !== 'running' || U.remoteASR(st)) return '';
  const key = runKey(st, opts);
  noteShare(key, st.progress, now);
  return etaText(etaMs(key, now));
}

// What a rerun deletes and redoes, per stage (common.rerun.{stage}).
const RERUN_STAGES = new Set(['prep', 'asr', 'summary', 'repair', 'translate', 'export', 'glossary_seed', 'glossary_review', 'material_extract', 'material_terms']);

export function rerunText(stage) {
  return RERUN_STAGES.has(stage) ? t(`common.rerun.${stage}`) : t('common.rerun.default');
}

// What a stage's cancel keeps, and what its retry then does (R79 H1,
// checked against the back end): a transcription keeps its checkpoint and
// a retry on the same device and model goes on from there (asr.go
// checkpointResumable); a translation keeps the sentences translated and a
// retry translates the others (translate.go translateTodo), unless it
// switches models; correction writes at the end, the summary is one
// request, the course's term stages likewise: they start over and call the
// model again; preprocessing, export and slide extraction start over.
const CANCEL_LLM = new Set(['summary', 'repair', 'glossary_seed', 'glossary_review', 'material_terms']);
export function cancelText(stage) {
  if (stage === 'asr') return t('common.stage_cancel.message_asr');
  if (stage === 'translate') return t('common.stage_cancel.message_translate');
  return t(CANCEL_LLM.has(stage) ? 'common.stage_cancel.message_llm' : 'common.stage_cancel.message');
}

// Confirmation for the destructive stage actions (SPEC §4.6); true = go on.
export async function confirmStageAction(stage, action) {
  const name = U.stageName(stage);
  if (action === 'rerun_text_only') return confirmTextRepair();
  if (action === 'rerun') {
    return confirmDialog({
      title: t('common.rerun.title', { stage: name }),
      message: t('common.rerun.message', { text: rerunText(stage) }),
      okText: t('common.rerun.ok'),
      danger: stage === 'prep' || stage === 'asr',
    });
  }
  if (action === 'cancel') {
    return confirmDialog({
      title: t('common.stage_cancel.title', { stage: name }),
      message: cancelText(stage),
      okText: t('common.stage_cancel.ok'),
      cancelText: t('common.dont_cancel'),
      danger: true,
    });
  }
  return true;
}

// "修复断句" (textrepair-int; the asr stage's rerun_text_only): says what it
// does not do (transcribe again), what it keeps (the sentences a person
// edited), what runs again and what it costs, and what is cleared (the
// review's accept / reject records).
function confirmTextRepair() {
  return confirmDialog({
    title: t('lecture.text_repair.title'),
    message: t('lecture.text_repair.message'),
    body: h(
      'ul',
      { class: 'dlg-list' },
      ['no_transcribe', 'keep_reviewed', 'downstream', 'patches'].map((k) => h('li', null, t(`lecture.text_repair.${k}`))),
    ),
    okText: t('lecture.text_repair.ok'),
  });
}

// A card's action while the same one is on its way (a double click on
// "重试": two requests, two model-choice dialogs) does nothing (3.2.0 P2);
// this covers every bar (lectures, materials, the course's stages).
const acting = new Set(); // `${opts.key}|${stage}|${action}`

async function runAction(st, action, opts) {
  const k = `${opts.key}|${st.stage}|${action}`;
  if (acting.has(k)) return;
  acting.add(k);
  try {
    if (!(await confirmStageAction(st.stage, action))) return;
    await opts.onAction(st.stage, action);
  } catch {
    // the API layer already showed the error
  } finally {
    acting.delete(k);
  }
}
