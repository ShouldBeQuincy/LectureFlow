// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The course page's "update", "update all out-of-date lectures" and "start
// all drafts" ask first (T25 X1, T22 G3): one click reran a whole lecture's
// correction and translation, or started every draft, with nothing said of
// what it does or costs. The confirmation says how many lectures, which
// stages run (again) and why, and, for an update, what those stages took
// the last time they ran (their stats on the page: tokens, the estimated
// cost, the time). What runs is not changed here: an update reruns the
// earliest stale stage and the later ones follow (v1.1 A2.3, the back end
// decides); a launch runs every stage.

import { h, confirmDialog } from './dom.js';
import { kindTag } from './kinds.js';
import * as U from './util.js';
import { t } from './i18n.js';

// at most this many lectures are named in a dialog (as the delete dialog)
const SHOWN = 8;

/**
 * The stages an update of lec runs: its earliest stale stage and every
 * stage after it (the back end's EarliestStale and its rerun). [] when
 * nothing is stale.
 */
export function updateStages(lec) {
  const stages = (lec && lec.stages) || [];
  const i = stages.findIndex((s) => s.status === 'stale');
  return i < 0 ? [] : stages.slice(i);
}

/**
 * What the stages took the last time they ran: {usage (UsageStats, the
 * estimated cost null when no stage had one), ms (their time added up)}.
 */
export function lastRun(stages) {
  const usage = { prompt_tokens: 0, cache_hit_tokens: 0, completion_tokens: 0, estimated_cost: null };
  let ms = 0;
  for (const s of stages) {
    const st = s.stats || {};
    usage.prompt_tokens += Number(st.prompt_tokens) || 0;
    usage.cache_hit_tokens += Number(st.cache_hit_tokens) || 0;
    usage.completion_tokens += Number(st.completion_tokens) || 0;
    if (st.estimated_cost != null && isFinite(st.estimated_cost)) usage.estimated_cost = (usage.estimated_cost || 0) + Number(st.estimated_cost);
    if (Number(s.duration_ms) > 0) ms += Number(s.duration_ms);
  }
  return { usage, ms };
}

// R74 medium 3: an edited or retranslated sentence starts an incremental
// translation, whose job then is the stage's current one with the stats of
// those few sentences (the note translate.incremental, StageView.note_info,
// also among its params.extra): "last time" would be one or two orders of
// magnitude below the whole lecture.
const NOTE_INCREMENTAL = 'translate.incremental';
function incremental(st) {
  const n = st && st.note_info;
  if (!n) return false;
  if (n.code === NOTE_INCREMENTAL) return true;
  const extra = n.params && Array.isArray(n.params.extra) ? n.params.extra : [];
  return extra.some((x) => x && x.code === NOTE_INCREMENTAL);
}

/**
 * The plan of updating these lectures (those with something stale):
 * {lectures, stages (names in pipeline order), reasons (texts),
 * reasonsInline (the same reasons as the dialog says them after its label,
 * in mid-sentence: U.staleReasonInline, round 10 C190), usage, ms,
 * keepsEdits (no rerun reaches the transcription: the sentences edited by
 * hand stay), partial (a stage's last run was an incremental one: usage
 * and ms do not stand for a whole run)}.
 */
export function updatePlan(lectures) {
  const list = (lectures || []).filter((l) => updateStages(l).length);
  const order = [];
  const reasons = [];
  const reasonsInline = [];
  const all = [];
  let keepsEdits = true;
  for (const l of list) {
    const st = updateStages(l);
    all.push(...st);
    for (const s of st) if (!order.includes(s.stage)) order.push(s.stage);
    if (st.some((s) => s.stage === 'prep' || s.stage === 'asr')) keepsEdits = false;
    // Lecture.stale's reasons, else each stale stage's (stale_info, else
    // the transitional stale_reason text, said as written)
    const why = l.stale && (l.stale.reasons || []).length ? l.stale.reasons : st.filter((s) => s.status === 'stale').map((s) => s.stale_info || s.stale_reason || '');
    for (const r of why) {
      const text = typeof r === 'string' ? r : U.staleReasonText(r);
      if (!text || reasons.includes(text)) continue;
      reasons.push(text);
      reasonsInline.push(typeof r === 'string' ? r : U.staleReasonInline(r));
    }
  }
  const pipeline = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
  order.sort((a, b) => pipeline.indexOf(a) - pipeline.indexOf(b));
  return { lectures: list, stages: order, reasons, reasonsInline, ...lastRun(all), keepsEdits, partial: all.some(incremental) };
}

function lectureList(list) {
  const shown = list.slice(0, SHOWN);
  return h(
    'div',
    { class: 'dlg-list-wrap' },
    h(
      'ul',
      { class: 'dlg-list' },
      shown.map((l) => h('li', null, kindTag(l, { class: 'ktag-inline' }), ' ', l.title)),
      list.length > shown.length ? h('li', { class: 'dim' }, t('common.lectures.delete_more', { n: list.length })) : null,
    ),
  );
}

/**
 * Asks before an update of these lectures (one: its row's "update"; more:
 * the selection, the course's "update all"). Resolves to true to go on;
 * true at once when none of them has anything stale (the back end answers
 * as before). opts.named: the lectures are listed even when there is one
 * (the course's "update all": which one is not seen where it was clicked).
 * opts.count: how many lectures the operation updates as the caller knows
 * it (the course's count): without a list to tell more (not read yet,
 * failed), the dialog says that much (R74 medium 4).
 */
export function confirmUpdate(lectures, opts = {}) {
  const p = updatePlan(lectures);
  const n = p.lectures.length;
  const count = Number(opts.count) || 0;
  if (!n && count > 0) {
    return confirmDialog({
      title: count === 1 ? t('course.confirm.update_title') : t('course.confirm.update_title_many', { n: count }),
      message: t('course.confirm.update_unknown'),
      okText: t('course.lectures.update'),
      focusCancel: true,
    });
  }
  if (!n) return Promise.resolve(true);
  const usage = U.hasUsage(p.usage) ? U.usageText(p.usage) : '';
  const time = p.ms > 0 ? U.fmtDuration(p.ms) : '';
  let last = '';
  if (p.partial) last = t('course.confirm.last_partial');
  else if (usage && time) last = t('course.confirm.last_run', { usage, time });
  else if (usage) last = t('course.confirm.last_usage', { usage });
  else if (time) last = t('course.confirm.last_time', { time });
  return confirmDialog({
    title: n === 1 ? t('course.confirm.update_title') : t('course.confirm.update_title_many', { n }),
    message: t('course.confirm.update_stages', { stages: U.listJoin(p.stages.map(U.stageName)) }),
    body: h(
      'div',
      { class: 'cf-update' },
      p.reasons.length ? h('p', { class: 'dlg-hint' }, t('course.confirm.update_why', { reasons: U.clauseJoin(p.reasonsInline) })) : null,
      n > 1 || opts.named ? lectureList(p.lectures) : null,
      h('p', { class: 'dlg-warn' }, p.keepsEdits ? t('course.confirm.update_replace_keep') : t('course.confirm.update_replace')),
      last ? h('p', { class: 'dlg-hint' }, last) : null,
    ),
    okText: t('course.lectures.update'),
    // it costs money: Enter pressed twice (or held) does not send it
    focusCancel: true,
  });
}

// The lecture roles. None configured: a launch is "transcription only", its
// LLM stages wait for a model service (the back end's TranscribeOnly,
// internal/sched/transcribe_only.go; the settings page's transcribeOnly,
// whose rule launchCostMode follows).
const LAUNCH_ROLES = ['summary', 'repair', 'translate'];

/**
 * How "start all drafts" bills, judged from the settings a page has (GET
 * /api/settings as it read them; never read for this): 'transcribe_only'
 * when no lecture role is configured; 'free' when each one is configured
 * on a free provider (freeProvider); 'billed' otherwise; undefined without
 * the settings (the dialog's general sentence). Round 8 convention F6.
 */
export function launchCostMode(settings) {
  const roles = settings && Array.isArray(settings.roles) ? settings.roles : null;
  if (!roles) return undefined;
  if (LAUNCH_ROLES.every((id) => roles.some((r) => r && r.role === id && !r.configured))) return 'transcribe_only';
  const providers = Array.isArray(settings.providers) ? settings.providers : [];
  const free = LAUNCH_ROLES.every((id) => {
    const r = roles.find((x) => x && x.role === id);
    const p = r && r.configured ? providers.find((x) => x && x.id === r.provider_id) : null;
    return freeProvider(p);
  });
  return free ? 'free' : 'billed';
}

/**
 * Whether a provider is one of the free kind (launchCostMode), in one of
 * two shapes:
 *   - its prices all set to 0 (the cache-hit price may be unset), without
 *     peak pricing: the page's estimate is 0;
 *   - the zero-cost plan as the setup guide and the settings page's presets
 *     make it (onboarding_providers.js: createProvider without prices, then
 *     patchProvider with nightTariff() and offpeak_only; view_settings.js
 *     the same): no price set, requests only off-peak, and a tariff whose
 *     off-peak multiplier is 1 (its peak window only keeps the requests to
 *     the night; R86 L2: this plan was always told "billed").
 * Taken for the second shape by mistake, a provider is still told "may cost
 * money" (course.confirm.launch_cost_free says requests are sent), which
 * says more than less; anything else (a price, another multiplier, prices
 * set in part, requests at any hour) is 'billed'.
 */
function freeProvider(p) {
  if (!p) return false;
  const zero = (v) => v === 0;
  const unset = (v) => v == null;
  if (zero(p.price_input) && zero(p.price_output) && (unset(p.price_cache_hit) || zero(p.price_cache_hit)) && p.tariff == null) return true;
  const tariff = p.tariff && typeof p.tariff === 'object' ? p.tariff : null;
  return unset(p.price_input) && unset(p.price_cache_hit) && unset(p.price_output) && p.offpeak_only === true && !!tariff && tariff.offpeak_multiplier === 1;
}

// The cost sentence of "start all drafts" by costMode (F6), and whether it
// warns (requests are sent) or only tells (transcription only: none now).
const LAUNCH_COST = new Map([
  ['billed', ['course.confirm.launch_cost', 'dlg-warn']],
  ['transcribe_only', ['course.confirm.launch_cost_transcribe_only', 'dlg-hint']],
  ['free', ['course.confirm.launch_cost_free', 'dlg-warn']],
]);

/**
 * Asks before "start all drafts" (drafts: the course's lectures that can be
 * started; count: the course's draft count when the list is not loaded).
 * opts.costMode (round 8 convention F6): 'billed' | 'transcribe_only' |
 * 'free', as launchCostMode tells it from the settings the page has; the
 * dialog then says what the LLM stages do and cost (R74 low 2d). Without
 * it (or another value), the general sentence: they are billed by use.
 * opts.courses (round 9 F2): the courses the drafts come from (each one's
 * code, its name when it has none), in the order shown; two or more, and
 * a line after the stages names them. Fewer, or none given: as before.
 */
export function confirmLaunchDrafts(drafts, count, opts = {}) {
  const list = drafts || [];
  const n = list.length || Number(count) || 0;
  const known = list.length > 0 && list.every((l) => Number(l.duration_ms) > 0);
  const ms = known ? list.reduce((a, l) => a + Number(l.duration_ms), 0) : 0;
  const stages = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'].map(U.stageName);
  const [costKey, costClass] = LAUNCH_COST.get(opts && opts.costMode) || LAUNCH_COST.get('billed');
  const courses = opts && Array.isArray(opts.courses) ? opts.courses : [];
  return confirmDialog({
    title: t('course.confirm.launch_title', { n }),
    message: t('course.confirm.launch_stages', { stages: U.listJoin(stages) }),
    body: h(
      'div',
      { class: 'cf-launch' },
      courses.length >= 2 ? h('p', { class: 'dlg-hint' }, t('course.confirm.launch_courses', { n: courses.length, courses: U.listJoin(courses) })) : null,
      ms ? h('p', { class: 'dlg-hint' }, t('course.confirm.launch_length', { time: U.fmtDuration(ms) })) : null,
      list.length ? lectureList(list) : null, // which ones, one included (R74 low 2)
      h('p', { class: costClass }, t(costKey)),
    ),
    okText: t('course.lectures.launch'),
    // it starts every stage (and may cost money): Enter pressed twice (or
    // held) does not send it
    focusCancel: true,
  });
}
