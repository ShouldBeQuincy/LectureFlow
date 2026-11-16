// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Google Colab remote transcription in the UI (SPEC-v3 V14.2, V14.4, V14.5;
// .claude/v3-brief.md §8).
//
// - The settings page's "Colab 远端转写" section: what is uploaded where
//   (V14.1: the recordings go to Google's servers); 3.1.0: the in-app
//   sign-in of colab_login.js (install the Google Cloud CLI component, then
//   "sign in to Google" — the same as the onboarding's remote step), with
//   the earlier manual way folded under "advanced: use your own gcloud":
//   the gcloud guide with
//   the login command the back end gives (adc.login_command), 检测授权,
//   importing with a lifecycle (N days / this run only / until I
//   disconnect); the authorization's state, time left and source
//   (auth.source: "app" = the in-app sign-in, "gcloud" = imported from the
//   user's own gcloud, also every 3.0.0 import), 断开连接 (which also signs
//   gcloud's application-default login out — only for the source gcloud,
//   whose refresh token gcloud shares), and the settings:
//   enabled, accelerator (colab-batch: measured speed and cost per lecture
//   of each, the paid tier's default L4 until one is picked), model and
//   precision, memory mode, "极速模式" (off by default; the larger GPUs
//   only), the longest wait for a GPU, "电池模式下优先使用
//   Colab". Every change is
//   sent at once; what is shown is the last GET /api/colab or the answer
//   of the change (nothing optimistic).
// - The lecture menu's "改用 Colab 处理" (POST /api/lectures/{id}/use-colab).
// - Shared helpers of the resource panel, the pipeline card and the
//   workbench line: the last resource snapshot's "remote" (Colab status;
//   null = switched off), durations, the state names.
// - colab-obs (user decisions 2026-10-27 07:00 and 14:50): the account
//   (status.account: level paid | free, the paid balance, the free quota,
//   eligibility_known false = not read, only the T4 then), the
//   accelerators it unlocks (selectable_accelerators, effective_accelerator,
//   locked_reason: a locked choice is kept, the T4 is used meanwhile), the
//   resource card of the snapshot's remotes[] (internal/remote.Card:
//   machine, usage, jobs) and the memory bar's segments. The settings page
//   shows the account ("重新查询"), the accelerators as a radio list with
//   what each measured, and no paid / free switch any more (the tier
//   follows the account).

import { h, btn, skeleton, errorBlock, toast, confirmDialog, copyText } from './dom.js';
import { icon } from './icons.js';
import { t, has, getLocale } from './i18n.js';
import * as U from './util.js';
import { appRequest } from './settings_app.js';
import { createColabLogin } from './colab_login.js';
import { keepChildren } from './components_ui.js';

// ---- shared state: the resource snapshot's "remote" ----

let lastRemote = null; // colab.Status | null
let heldShown = false; // GET /api/colab held (after a shutdown button)
const listeners = new Set();

/** Called with every resource snapshot (resources.js): r.remote or null. */
export function noteRemote(remote) {
  const prev = JSON.stringify(lastRemote);
  lastRemote = remote || null;
  if (JSON.stringify(lastRemote) !== prev) for (const fn of listeners) fn(lastRemote);
}

export function remoteStatus() {
  return lastRemote;
}

export function onRemoteChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function colabHeld() {
  return heldShown;
}

function noteView(v) {
  if (v && typeof v.held === 'boolean') heldShown = v.held;
}

/** Colab is switched on and holds a usable authorization. */
export function colabConfigured() {
  return !!(lastRemote && lastRemote.configured);
}

// A session that exists (the workbench line, the resource card).
export const ACTIVE_STATES = new Set(['queued', 'booting', 'running', 'stopping']);

export function stateName(s) {
  return has(`colab.state.${s}`) ? t(`colab.state.${s}`) : s || '';
}

export function stepName(s) {
  return has(`colab.step.${s}`) ? t(`colab.step.${s}`) : s || '';
}

export function stageName(s) {
  return has(`colab.stage.${s}`) ? t(`colab.stage.${s}`) : s || '';
}

export function releaseName(r) {
  return r && has(`colab.release.${r}`) ? t(`colab.release.${r}`) : (r || '');
}

/** "1 小时 47 分", "4 小时", "12 分 3 秒", "6 天" (whole days from one day on). */
export function durText(sec) {
  if (sec == null || !isFinite(sec) || sec < 0) return '';
  const s = Math.round(sec);
  if (s >= 86400) {
    const days = Math.floor(s / 86400);
    return t('resources.remote.dur_days', { n: days, days });
  }
  if (s >= 3600 && s % 3600 < 60) {
    const hours = Math.floor(s / 3600);
    return t('resources.remote.dur_hours', { n: hours, h: hours });
  }
  return U.fmtDuration(s * 1000);
}

/** Compute units with up to two decimals. */
export function cuText(v) {
  return v == null || !isFinite(v) ? '--' : U.fmtDecimal(v, 2);
}

/** The authorization's time left: "剩余 6 天" / "仅本次运行" / "直到手动断开". */
export function authLeftText(auth) {
  if (!auth || !auth.present) return '';
  if (auth.lifecycle === 'session') return t('resources.remote.auth_session');
  if (auth.lifecycle === 'manual') return t('resources.remote.auth_manual');
  if (auth.remaining_sec >= 0) return t('resources.remote.auth_left', { time: durText(auth.remaining_sec) || durText(0) });
  return '';
}

/** A Colab error / waiting reason with its params ({code, params}). */
export function infoText(info) {
  return info && info.code ? U.errorInfoText(info, info.code) : '';
}

/** Whether a stage view's asr runs (or ran) on Colab. */
export function stageOnColab(st) {
  if (!st || st.stage !== 'asr') return false;
  if (st.stats && st.stats.remote) return true;
  const a = st.model && st.model.asr;
  return !!(a && a.device_id === 'colab:remote');
}

/**
 * "极速模式" of a lecture's last transcription (colab-obs): the number of
 * parts Colab cut it into (asr_timing.postprocess.split), 0 when it was
 * transcribed whole.
 */
export function splitParts(lec) {
  const asr = ((lec && lec.stages) || []).find((s) => s.stage === 'asr');
  const sp = asr && asr.asr_timing && asr.asr_timing.postprocess && asr.asr_timing.postprocess.split;
  const n = sp ? Number(sp.parts) : 0;
  return Number.isInteger(n) && n > 1 ? n : 0;
}

// ---- colab-obs: the account and the accelerators it unlocks ----

// What each accelerator measured on Colab (colab-setup-time.md §5.1 and
// §9.1; a 55-minute lecture with large-v3, about 30 s of setup included):
// the runtime's GPU memory and memory in GB, minutes per lecture, the
// session's compute units per hour. The H100 was never lent to the test
// account: its GPU memory only.
export const SPECS = {
  T4: { vram: 15, ram: 13, min: 3.8, cuh: 1.07 },
  L4: { vram: 24, ram: 54, min: 3, cuh: 1.54 },
  A100: { vram: 40, ram: 85, min: 2.3, cuh: 5.3 },
  G4: { vram: 96, ram: 181, min: 1.9, cuh: 8.9 },
  H100: { vram: 80, ram: null, min: null, cuh: null },
};

/** Compute units one lecture costs on accelerator x (null: not measured). */
export function cuPerLecture(x) {
  const s = SPECS[x];
  return s && s.min && s.cuh ? (s.min * s.cuh) / 60 : null;
}

/** "约 N 节": hundreds from 1,000 on, tens from 100 on, else whole lectures. */
export function approxCount(n) {
  if (n == null || !isFinite(n) || n < 0) return null;
  if (n >= 1000) return Math.round(n / 100) * 100;
  if (n >= 100) return Math.round(n / 10) * 10;
  return Math.floor(n);
}

/** The account of a Colab status (null: a back end without it). */
export function accountOf(st) {
  return (st && st.account && typeof st.account === 'object' && st.account) || null;
}

/** The account said which GPUs it may borrow (false: only the T4). */
export function accountKnown(a) {
  return !!(a && a.eligibility_known);
}

/** Why the last query of the account failed ({code, params}; null: it did not). */
export function accountError(a) {
  return a && a.read_ok === false && a.read_error && a.read_error.code ? a.read_error : null;
}

/**
 * The failed query in words: the authorization's own texts (they say what
 * to do); any other code is the dispatch's wording ("这节讲座已转回本机…"),
 * so a plain "the query failed" with the code's text as the hover detail.
 * {text, detail}; null when the last query did not fail.
 */
export function accountErrorText(a) {
  const err = accountError(a);
  if (!err) return null;
  const detail = infoText(err);
  if (err.code.startsWith('colab.auth_')) return { text: detail, detail: '' };
  return { text: t('resources.remote.check_failed'), detail };
}

/** "重新查询" is offered: the last query failed, or only the T4 is open. */
export function accountNeedsCheck(a) {
  return !!a && (a.read_ok === false || !accountKnown(a));
}

/** The paid balance of the reading shown (null: never read). */
export function paidBalance(a) {
  return a && typeof a.paid_balance_cu === 'number' && isFinite(a.paid_balance_cu) ? a.paid_balance_cu : null;
}

const freeQuota = (a) => (a && typeof a.free_quota_cu === 'number' && a.free_quota_cu > 0 ? a.free_quota_cu : 0);

/**
 * About how many lectures the account pays for on accelerator x: the paid
 * balance, plus the free quota on a T4 (user decision 14:50). Null when the
 * balance is not known or x was never measured.
 */
export function fitFor(a, x) {
  const per = cuPerLecture(x);
  if (!a || !per) return null;
  let cu = typeof a.paid_balance_cu === 'number' && isFinite(a.paid_balance_cu) ? Math.max(0, a.paid_balance_cu) : null;
  if (x === 'T4' && freeQuota(a)) cu = (cu || 0) + freeQuota(a);
  return cu == null ? null : approxCount(cu / per);
}

/** "付费" / "免费层" (account.level; the settings' tier before colab-obs). */
export function levelText(st) {
  const a = accountOf(st);
  const level = a ? a.level : st && st.tier;
  return level === 'paid' ? t('resources.remote.tier.paid') : t('resources.remote.tier.free');
}

/** The accelerator lectures go to now: the choice, or the T4 while it is locked. */
export function effectiveAccel(st) {
  return (st && (st.effective_accelerator || st.accelerator)) || '';
}

/**
 * Why accelerator x cannot be borrowed now ('' = it can): need_auth,
 * account_unknown, no_balance or ineligible (texts resources.remote.lock.*).
 * Every accelerator is open with a back end that does not list them. An
 * account not read because Colab is not authorized yet is need_auth, not
 * account_unknown, which reads like a failure (round 2, T6 #11).
 */
export function lockOf(st, x) {
  const list = st && st.selectable_accelerators;
  if (!Array.isArray(list) || list.includes(x)) return '';
  const a = accountOf(st);
  if (!accountKnown(a)) return st.auth && !st.auth.present ? 'need_auth' : 'account_unknown';
  if (!(a.paid_balance_cu > 0)) return 'no_balance';
  return 'ineligible';
}

export function lockText(reason) {
  return reason && has(`resources.remote.lock.${reason}`) ? t(`resources.remote.lock.${reason}`) : reason || '';
}

/**
 * lockText on a line of its own (the accelerator row): the reasons are
 * lower-case fragments made to sit inside a sentence, so the first letter
 * is raised there (no change in scripts without case).
 */
export function lockLine(reason) {
  const s = lockText(reason);
  if (!s) return '';
  let first;
  try {
    first = s.charAt(0).toLocaleUpperCase(getLocale() || undefined);
  } catch {
    first = s.charAt(0).toUpperCase();
  }
  return first + s.slice(1);
}

/** "免费额度约 3.2 CU，下次补充 10月28日 08:00" ('' without a free quota). */
export function freeQuotaText(a) {
  const q = freeQuota(a);
  if (!q) return '';
  const cu = cuText(q);
  return a.free_refill_at ? t('resources.remote.free_quota', { cu, time: U.fmtDateTime(a.free_refill_at) }) : t('resources.remote.free_quota_now', { cu });
}

/** "查询于 14:02（2 分钟前）" ('' before the first query). */
export function checkedText(a) {
  return a && a.read_at ? t('resources.remote.checked_at', { time: U.fmtHM(a.read_at), ago: U.relTime(a.read_at) }) : '';
}

// ---- colab-obs: the resource card (remotes[]) and its memory bar ----

/**
 * The Colab area of a resource snapshot: the Colab card of remotes[]
 * (internal/remote.Card) with the Colab status (snapshot.remote, else the
 * card's colab part). {st, machine, usage, jobs, queued, ahead}; null when
 * Colab is off. jobs: [{title, step, stage, progress, eta, split}].
 */
export function colabCard(res) {
  if (!res) return null;
  const card = Array.isArray(res.remotes) ? res.remotes.find((c) => c && c.kind === 'colab') || null : null;
  const own = card && card.colab && typeof card.colab === 'object' ? card.colab : null;
  const st = res.remote || own;
  if (!st && !card) return null;
  const s = st || {};
  const c = card || {};
  let jobs;
  if (Array.isArray(c.jobs) && c.jobs.length) {
    jobs = c.jobs.map((j) => ({ title: j.lecture_title || '', step: '', stage: j.stage || '', progress: j.progress, eta: j.eta_sec, split: j.split || 0, reconnect: reconnectOf(j) }));
  } else {
    const list = Array.isArray(s.jobs) && s.jobs.length ? s.jobs : s.current ? [s.current] : [];
    jobs = list.map((j) => ({ title: j.lecture_title || '', step: j.step || '', stage: j.stage || '', progress: j.overall, eta: j.eta_sec, split: j.split || 0, reconnect: reconnectOf(j) }));
  }
  // stopping (v3.2.x): the runtime is being released; the lectures it ran
  // are back in the local queue, so their old steps and progress are not
  // shown (the back end drops them already; this guards a late snapshot)
  if (s.state === 'stopping') jobs = [];
  return {
    st: s,
    machine: c.machine || s.machine || null,
    usage: c.usage || s.usage || null,
    jobs,
    queued: card ? c.queued || 0 : s.planned || 0,
    ahead: card ? c.uploaded_ahead || 0 : s.uploaded_ahead || 0,
  };
}

// ---- v3.2.x: reconnecting, still billed, releasing ----

// A card job's reconnect {attempt, of, next_at} (internal/remote.Reconnect),
// null when the lecture is not waiting for the runtime to answer.
function reconnectOf(j) {
  const r = j && j.reconnect;
  if (!r || typeof r !== 'object' || !(Number(r.attempt) > 0)) return null;
  return { attempt: Number(r.attempt), of: Number(r.of) || 0, nextAt: r.next_at || null };
}

/**
 * "连接不稳定，正在重试（2/3）" of a job's reconnect, with the seconds to
 * the next try when next_at is still ahead of now (ms): {text, next}; null
 * without a reconnect.
 */
export function reconnectText(rc, now = Date.now()) {
  if (!rc) return null;
  const text = t('resources.remote.reconnecting', { attempt: U.fmtInt(rc.attempt), of: U.fmtInt(rc.of || rc.attempt) });
  const at = rc.nextAt ? new Date(rc.nextAt).getTime() : NaN;
  const sec = isFinite(at) ? Math.ceil((at - now) / 1000) : 0;
  return { text, next: sec > 0 ? t('resources.remote.reconnect_next', { time: durText(sec) }) : '' };
}

/**
 * The warning of a runtime that is held but does not answer (status.runtime;
 * user decision: a hint only, nothing is released because of it): paid
 * "仍在计费（本次已用 X CU，约 Y CU/小时）", free "仍在占用本次使用窗口".
 * Null when the runtime answers or nothing asked it lately (unknown), while
 * a lecture is retrying in place (its own line says so). While the runtime
 * is being released (stopping, the release retried by itself) a paid one
 * gets its own text without the "立即关机" hint (the button is hidden), a
 * free one "仍在占用" (R8 B-s1: both tiers are told).
 */
export function runtimeWarning(st, jobs) {
  const rt = st && st.runtime;
  if (!rt || !rt.held || rt.reachable !== 'unreachable') return null;
  if (st.state === 'stopping') return rt.billing ? t('resources.remote.still_billing_releasing') : t('resources.remote.still_holding');
  if (Array.isArray(jobs) && jobs.some((j) => j && j.reconnect)) return null;
  if (!rt.billing) return t('resources.remote.still_holding');
  const spent = Number(st.session_spent_cu);
  const rate = Number(st.cu_per_hour);
  if (spent > 0 && rate > 0) return t('resources.remote.still_billing', { cu: cuText(spent), rate: U.fmtDecimal(rate, 2) });
  return t('resources.remote.still_billing_plain');
}

/**
 * The Colab error worth showing: status.last_error unless it is older than
 * the current session (session_started_at; the back end clears it when a
 * session starts, this is the second guard).
 */
export function visibleError(st) {
  const e = st && st.last_error;
  if (!e || !e.code) return null;
  const at = st.last_error_at ? new Date(st.last_error_at).getTime() : NaN;
  const since = st.session_started_at ? new Date(st.session_started_at).getTime() : NaN;
  if (isFinite(at) && isFinite(since) && at < since) return null;
  return e;
}

/**
 * A runtime that was not released (status.leftover: an earlier run's
 * session, or a runtime whose release failed and is retried by itself, also
 * one known only by its notebook, without an accelerator).
 */
export function leftoverText(lo) {
  if (!lo || !lo.code) return '';
  const acc = lo.accelerator || '';
  if (lo.code === 'colab.release_failed') return acc ? t('resources.remote.leftover_retrying', { accelerator: acc }) : t('resources.remote.leftover_retrying_any');
  if (lo.code === 'colab.leftover_session' && !acc) return t('resources.remote.leftover_session_any');
  return infoText({ code: lo.code, params: { accelerator: acc } });
}

// RAM segment kinds left to right (internal/remote SegRAMDisk …); "used"
// alone when the remote reports total and free only.
export const SEG_KINDS = ['ramdisk', 'whisper', 'system', 'data', 'used'];

/**
 * The memory bar of a remote reading: {total, free, freeFrac, segs: [{kind,
 * mb, frac}]} in SEG_KINDS order, the free part apart (it fills the rest);
 * segments of 0 MB and unknown kinds are left out. The total is the
 * segments' sum (they add up to the machine's memory). Null without one.
 */
export function ramSegments(usage) {
  const list = usage && Array.isArray(usage.ram) ? usage.ram : [];
  const mb = (k) => list.reduce((n, s) => n + (s && s.kind === k ? Math.max(0, Number(s.mb) || 0) : 0), 0);
  const segs = SEG_KINDS.map((kind) => ({ kind, mb: mb(kind) })).filter((s) => s.mb > 0);
  const free = mb('free');
  const total = segs.reduce((n, s) => n + s.mb, 0) + free;
  if (!(total > 0)) return null;
  for (const s of segs) s.frac = s.mb / total;
  return { total, free, freeFrac: free / total, segs };
}

// The accelerator a GPU name stands for (nvidia-smi names: "Tesla T4",
// "NVIDIA L4", "NVIDIA A100-SXM4-40GB", G4 = "NVIDIA RTX PRO 6000
// Blackwell Server Edition"); a requested accelerator the reported name
// does not match is said (the card's identity comes from the remote).
const GPU_NAMES = { T4: /\bT4\b/i, L4: /\bL4\b/i, A100: /A100/i, H100: /H100/i, G4: /RTX PRO 6000|Blackwell|\bG4\b/i };

export function gpuMismatch(machine) {
  if (!machine || !machine.gpu_name || !machine.requested) return false;
  const re = GPU_NAMES[machine.requested];
  return !!re && !re.test(machine.gpu_name);
}

/**
 * The Colab cell of the collapsed resource rail (colab-obs) for a
 * colabCard(): with a session the remote GPU's usage, the GPU below it and
 * the memory segments (mini); without one the compute units left (the paid
 * balance, else the free quota) above "CU"; "--" in the warning colour
 * above "仅 T4" when the account was not read. Null while Colab is off.
 */
export function railColab(c) {
  if (!c) return null;
  const r = c.st;
  const a = accountOf(r);
  const accel = effectiveAccel(r);
  if (ACTIVE_STATES.has(r.state) || r.current) {
    const u = c.usage;
    const value = u && typeof u.gpu_util_pct === 'number' ? U.fmtPercent(u.gpu_util_pct / 100, 0) : '--';
    const lay = u ? ramSegments(u) : null;
    const title = [t('resources.remote.service', { accelerator: accel }), t('resources.rail.gpu', { value }), lay ? t('resources.remote.ram_free', { free: U.fmtMB(lay.free), total: U.fmtMB(lay.total) }) : ''];
    return { value, sub: (c.machine && c.machine.requested) || accel, title: title.filter(Boolean).join(' · '), warn: false, mini: lay };
  }
  const paid = a ? paidBalance(a) : typeof r.balance_cu === 'number' ? r.balance_cu : null;
  const left = paid != null && paid > 0 ? paid : freeQuota(a) || paid;
  const value = left == null ? '--' : U.fmtInt(Math.floor(left));
  if (a && !accountKnown(a)) return { value: paid == null ? '--' : value, sub: t('resources.remote.cards_t4'), title: t('resources.remote.unknown_t4'), warn: true, mini: null };
  const title = [t('resources.remote.unused'), levelText(r), paid != null ? t('resources.remote.balance', { balance: cuText(paid) }) : '', freeQuotaText(a)];
  return { value, sub: 'CU', title: title.filter(Boolean).join(' · '), warn: false, mini: null };
}

// ---- requests ----

const colabGet = () => appRequest('GET', '/api/colab');
const colabPut = (patch) => appRequest('PUT', '/api/colab/settings', patch);

/** POST /api/colab/shutdown {mode}; resolves to the ColabView or throws. */
export async function shutdownColab(mode) {
  const v = await appRequest('POST', '/api/colab/shutdown', { mode });
  noteView(v);
  return v;
}

/**
 * "重新查询" (colab-obs): POST /api/colab/account/refresh reads the account
 * from Colab now and answers the ColabView; a back end without the
 * endpoint (404 / 405) answers GET /api/colab instead.
 */
export async function refreshAccount() {
  let v;
  try {
    v = await appRequest('POST', '/api/colab/account/refresh');
  } catch (e) {
    if (!e || (e.status !== 404 && e.status !== 405)) throw e;
    v = await colabGet();
  }
  noteView(v);
  return v;
}

/** "立即关机" asks first (the running lecture goes back to the queue). */
export async function confirmShutdownNow() {
  return confirmDialog({
    title: t('resources.remote.now_title'),
    message: t('resources.remote.now_message'),
    okText: t('resources.remote.now_ok'),
    danger: true,
  });
}

// ---- "改用 Colab 处理" (V14.4) ----

const USE_STATES = new Set(['queued', 'running', 'failed', 'cancelled']);

// The 409 reasons: remote.disabled, remote.unavailable {waiting_for}
// (the waiting text follows as its own sentence), state.conflict.
function useColabError(e) {
  const b = e && e.body && e.body.error;
  if (b && b.code === 'remote.unavailable' && b.params && b.params.waiting_for) {
    const w = U.waitingForText(b.params.waiting_for);
    return U.clauseJoin([U.errorInfoText({ code: b.code }), w].filter(Boolean));
  }
  return (b && b.code && U.errorInfoText(b, e.message)) || (e && e.message) || t('common.unknown_error');
}

// Also the asr stage's allowed action use_colab (colabfix: Colab handed the
// lecture back and no local device can take it; actions.js stageAction).
export async function useColab(lec) {
  const ok = await confirmDialog({
    title: t('lecture.use_colab.title', { title: lec.title || '' }),
    message: t('lecture.use_colab.message'),
    okText: t('lecture.use_colab.ok'),
  });
  if (!ok) return null;
  try {
    const l = await appRequest('POST', `/api/lectures/${encodeURIComponent(lec.id)}/use-colab`);
    heldShown = false;
    toast(t('lecture.use_colab.done'), 'success');
    return l;
  } catch (e) {
    toast(useColabError(e), 'error');
    throw e;
  }
}

/**
 * The lecture menu item (null when not offered): Colab is on and
 * authorized, and the lecture's transcription is queued, running (on this
 * computer), failed or cancelled — the back end refuses done, stale and
 * pending ones. done(promise of the lecture) renders the answer.
 */
export function useColabMenuItem(lec, done) {
  if (!colabConfigured()) return null;
  const asr = (lec.stages || []).find((s) => s.stage === 'asr');
  if (!asr || !USE_STATES.has(asr.status) || stageOnColab(asr)) return null;
  return { id: 'use_colab', label: t('menu.lecture.use_colab'), icon: 'upload', onSelect: () => done(useColab(lec)) };
}

// ---- the settings section ----

// colab-obs: the radio list's order (the measured ones by speed, then the
// H100); "极速模式" works on the larger GPUs only.
const ACCELERATORS = ['T4', 'L4', 'A100', 'G4', 'H100'];
const FAST_ACCELERATORS = ['L4', 'A100', 'G4', 'H100'];
const PRECISIONS = ['float16', 'int8_float16', 'int8', 'bfloat16', 'float32', 'int8_bfloat16', 'int8_float32'];
const MODELS = ['large-v3', 'large-v3-turbo', 'large-v2', 'medium', 'small'];
const LIFECYCLES = ['days', 'session', 'manual'];

// createColabSection({head, formRow, switchBtn, segmented, rerender}): the
// helpers of view_settings.js (like settings_app.js).
export function createColabSection(ui) {
  let alive = true;
  let view = null; // GET /api/colab
  let error = '';
  let unavailable = false; // 503: this server has no Colab back end
  let saving = ''; // the field being saved
  let saveError = '';
  let checking = false;
  let checked = null; // the last 检测授权 answer {adc, auth}
  let importing = false;
  let disconnecting = false;
  let lifecycle = 'days';
  let days = 7;
  let daysError = '';
  let waitDraft = null; // the typed 等卡上限 while it is not saved
  let waitError = '';
  let modelDraft = null;
  let advancedOpen = null; // the "use your own gcloud" fold (null: not toggled yet)
  let loginStarted = false;
  let refreshing = false; // "重新查询" of the account
  // WV-10: kept across redraws (render, appLoginBlock)
  let headEl = null;
  let formEl = null;
  let loginEl = null;
  const redraw = () => alive && ui.rerender();
  // 3.1.0: the in-app sign-in (the gcloud component, then the Google sign-in)
  const appLogin = createColabLogin({
    rerender: redraw,
    onAuthorized: async () => {
      await load();
      if (alive) toast(t('settings.colab.login_done'), 'success');
    },
    fkey: 'set:colab:login',
  });

  function setView(v) {
    view = v;
    noteView(v);
  }

  async function load() {
    try {
      const v = await colabGet();
      if (!alive) return;
      setView(v);
      error = '';
      unavailable = false;
    } catch (e) {
      if (!alive) return;
      if (e && e.status === 503) unavailable = true;
      else if (!view) error = e.message || t('common.load_failed');
    }
    redraw();
  }

  async function save(field, patch) {
    if (saving) return;
    saving = field;
    saveError = '';
    redraw();
    try {
      const v = await colabPut(patch);
      if (!alive) return;
      setView(v);
      if (field === 'wait') waitDraft = null;
      if (field === 'model') modelDraft = null;
    } catch (e) {
      if (!alive) return;
      saveError = e.message;
    }
    saving = '';
    redraw();
  }

  // "重新查询": the account is read from Colab now (the answer is shown).
  async function recheck() {
    if (refreshing) return;
    refreshing = true;
    redraw();
    try {
      const v = await refreshAccount();
      if (alive && v && v.status) setView(v);
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    refreshing = false;
    redraw();
  }

  async function check() {
    if (checking) return;
    checking = true;
    redraw();
    try {
      const c = await appRequest('POST', '/api/colab/auth/check');
      if (!alive) return;
      checked = c;
      if (view) view = { ...view, adc: c.adc, status: { ...view.status, auth: c.auth, configured: !!(c.auth && c.auth.present) } };
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    checking = false;
    redraw();
    load();
  }

  async function importAuth() {
    if (importing) return;
    if (lifecycle === 'days' && !(Number.isInteger(days) && days >= 1 && days <= 90)) {
      daysError = t('settings.colab.days_invalid', { min: 1, max: 90 });
      redraw();
      return;
    }
    importing = true;
    daysError = '';
    redraw();
    try {
      await appRequest('POST', '/api/colab/auth/import', lifecycle === 'days' ? { lifecycle, days } : { lifecycle });
      if (alive) toast(t('settings.colab.imported'), 'success');
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    importing = false;
    checked = null;
    await load();
  }

  // Where the stored credential came from (auth.source): "app" = the in-app
  // sign-in, whose token the user's own gcloud never held; anything else
  // ("gcloud", or a back end that does not say) shares it with gcloud.
  const fromOwnGcloud = () => ((view && view.status && view.status.auth) || {}).source !== 'app';

  async function disconnect() {
    if (disconnecting) return;
    const ok = await confirmDialog({
      title: t('settings.colab.disconnect_title'),
      message: t('settings.colab.disconnect_message'),
      body: h(
        'ul',
        { class: 'colab-points' },
        h('li', null, icon('server', { size: 14 }), h('span', null, t('settings.colab.disconnect_point_release'))),
        h('li', null, icon('key-round', { size: 14 }), h('span', null, t('settings.colab.disconnect_point_revoke'))),
        // From the user's own gcloud: its application-default login uses this
        // very token. In-app sign-in: an isolated copy, but Google revokes the
        // account's whole grant to gcloud's client, so an own gcloud login
        // with the same account may need signing in again too.
        h('li', null, icon('triangle-alert', { size: 14 }), h('span', null, t(fromOwnGcloud() ? 'settings.colab.disconnect_point_gcloud' : 'settings.colab.disconnect_point_gcloud_app'))),
      ),
      okText: t('settings.colab.disconnect_ok'),
      danger: true,
    });
    if (!ok || !alive) return;
    disconnecting = true;
    redraw();
    try {
      const r = await appRequest('DELETE', '/api/colab/auth');
      if (!alive) return;
      const texts = [t('settings.colab.disconnected')];
      for (const w of (r && r.warnings) || []) texts.push(U.errorInfoText(w));
      for (const n of (r && r.notes) || []) texts.push(U.codedText('notes', n, ''));
      toast(U.clauseJoin(texts.filter(Boolean)), r && r.warnings && r.warnings.length ? 'warn' : 'success');
    } catch (e) {
      if (alive) toast(e.message, 'error');
    }
    disconnecting = false;
    checked = null;
    await load();
  }

  // ---- render ----

  const note = (ic, text, cls = '') => h('p', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('span', null, text));

  // 3.1.0: the in-app sign-in — install the Google Cloud CLI component when
  // it is missing, then "sign in to Google" (waiting, the fallback link,
  // cancel, failures with gcloud's message: as in the onboarding).
  // WV-10: the block is kept (loginEl) and its children put in place with
  // keepChildren, so the install's view (gcloudBlock's view.el: its status
  // line and alert) is never taken out and put back on a redraw.
  function appLoginBlock() {
    if (!loginStarted) {
      loginStarted = true;
      appLogin.start();
    }
    if (!loginEl) loginEl = h('div', { class: 'colab-login' });
    const parts = [h('p', { class: 'colab-login-hint' }, t('settings.colab.app_login_hint'))];
    if (appLogin.loading) {
      parts.push(note('loader-circle', t('onboarding.loading')));
      keepChildren(loginEl, parts);
      return loginEl;
    }
    const g = appLogin.gcloud();
    const ready = appLogin.gcloudReady();
    if (!ready && !appLogin.installBusy) {
      parts.push(
        h(
          'div',
          { class: 'set-inline colab-login-actions' },
          btn(t('settings.colab.install_gcloud'), { size: 'sm', kind: 'primary', icon: 'download', fkey: 'set:colab:gcloud', onclick: () => appLogin.install() }),
          g && g.download_bytes ? h('span', { class: 'dim small' }, t('settings.colab.gcloud_size', { size: U.fmtBytes(g.download_bytes) })) : null,
        ),
      );
    } else {
      const block = appLogin.gcloudBlock();
      if (block) parts.push(block);
    }
    if (appLogin.startError) parts.push(note('circle-alert', appLogin.startError, 'is-warn'));
    if (ready) {
      parts.push(...appLogin.loginBlock({ idleHint: false }));
      const a = appLogin.loginAction();
      parts.push(h('div', { class: 'set-inline colab-login-actions' }, btn(a.label, { size: 'sm', kind: 'primary', icon: a.icon, disabled: a.disabled, fkey: 'set:colab:login', onclick: a.onclick }), appLogin.cancelButton({ size: 'sm' })));
    }
    keepChildren(loginEl, parts);
    return loginEl;
  }

  // The earlier manual way, folded: run gcloud yourself, 检测授权, import.
  function advancedBlock() {
    const adc = view.adc || {};
    const open = advancedOpen != null ? advancedOpen : !!(checked || adc.detected);
    const d = h(
      'details',
      { class: 'colab-advanced', open },
      h('summary', { dataset: { fkey: 'set:colab:advanced' } }, t('settings.colab.advanced')),
      h('p', { class: 'dim small colab-advanced-hint' }, t('settings.colab.advanced_hint')),
      guide(),
      importRow(),
    );
    d.addEventListener('toggle', () => {
      advancedOpen = d.open;
    });
    return d;
  }
  const savingNote = (field) => (saving === field ? note('loader-circle', t('settings.saving')) : null);

  function privacyBlock() {
    return h(
      'div',
      { class: 'colab-privacy', role: 'note' },
      icon('shield-check', { size: 16 }),
      h('div', { class: 'colab-privacy-text' }, h('strong', null, t('settings.colab.privacy_title')), h('p', null, t('settings.colab.privacy')), h('p', { class: 'dim' }, t('settings.colab.fair_note'))),
    );
  }

  function guide() {
    const adc = view.adc || {};
    const cmd = adc.login_command || '';
    const found = adc.detected;
    const problem = !found && adc.problem ? U.errorInfoText({ code: adc.problem, params: { path: adc.path || '', field: '', type: '', scopes: '' } }) : '';
    return h(
      'ol',
      { class: 'colab-steps' },
      h('li', null, h('span', null, t('settings.colab.step1'))),
      h(
        'li',
        null,
        h('span', null, t('settings.colab.step2')),
        h(
          'div',
          { class: 'colab-cmd' },
          h('code', { class: 'mono colab-cmd-text' }, cmd),
          btn(t('settings.colab.copy_cmd'), {
            size: 'sm',
            kind: 'ghost',
            icon: 'copy',
            fkey: 'set:colab:copy',
            onclick: async () => {
              const ok = await copyText(cmd);
              toast(ok ? t('settings.colab.copied') : t('common.copy_failed'), ok ? 'success' : 'warn');
            },
          }),
        ),
      ),
      h(
        'li',
        null,
        h('span', null, t('settings.colab.step3')),
        h(
          'div',
          { class: 'set-inline colab-check' },
          btn(checking ? t('settings.colab.checking') : t('settings.colab.check'), { size: 'sm', icon: checking ? 'loader-circle' : 'scan-search', disabled: checking, fkey: 'set:colab:check', onclick: check }),
          checked || found || problem
            ? found
              ? note('circle-check', t('settings.colab.adc_found'), 'is-ok')
              : note(checked ? 'circle-alert' : 'info', problem || t('settings.colab.adc_missing'), checked ? 'is-warn' : '')
            : null,
        ),
      ),
    );
  }

  function importRow() {
    const adc = view.adc || {};
    const opts = LIFECYCLES.map((v) => [v, v === 'days' ? t('colab.lifecycle.days', { n: days, days }) : t(`colab.lifecycle.${v}`)]);
    const daysInput = h('input', {
      class: 'input input-num colab-days',
      type: 'number',
      min: '1',
      max: '90',
      step: '1',
      value: String(days),
      disabled: lifecycle !== 'days' || importing,
      'aria-label': t('settings.colab.days_label'),
      dataset: { fkey: 'set:colab:days' },
    });
    const commitDays = () => {
      const n = Number(daysInput.value.trim());
      if (n === days) return;
      days = n;
      daysError = Number.isInteger(n) && n >= 1 && n <= 90 ? '' : t('settings.colab.days_invalid', { min: 1, max: 90 });
      redraw();
    };
    daysInput.addEventListener('change', commitDays);
    daysInput.addEventListener('keydown', (e) => e.key === 'Enter' && commitDays());
    return ui.formRow(
      t('settings.colab.lifecycle_label'),
      h(
        'div',
        { class: 'colab-import' },
        ui.segmented('set:colab:lifecycle', opts, lifecycle, (v) => ((lifecycle = v), redraw()), t('settings.colab.lifecycle_label')),
        h(
          'div',
          { class: 'set-inline colab-import-line' },
          lifecycle === 'days' ? h('label', { class: 'set-inline colab-days-wrap' }, h('span', { class: 'dim small' }, t('settings.colab.days_label')), daysInput) : null,
          btn(importing ? t('settings.colab.importing') : view.status.auth && view.status.auth.present ? t('settings.colab.reimport') : t('settings.colab.import'), {
            size: 'sm',
            kind: view.status.auth && view.status.auth.present ? 'ghost' : 'primary',
            icon: importing ? 'loader-circle' : 'key-round',
            disabled: importing || !adc.detected || !!daysError,
            title: adc.detected ? undefined : t('settings.colab.import_need_adc'),
            fkey: 'set:colab:import',
            onclick: importAuth,
          }),
        ),
      ),
      t('settings.colab.lifecycle_hint'),
      daysError ? note('circle-alert', daysError, 'is-err') : null,
    );
  }

  // "导入于 {date}" with the credential's source when the back end gives it.
  function importedText(a) {
    const date = U.fmtDateTime(a.imported_at);
    if (a.source === 'app') return t('settings.colab.imported_at_app', { date });
    if (a.source === 'gcloud') return t('settings.colab.imported_at_gcloud', { date });
    return t('settings.colab.imported_at', { date });
  }

  function authRow() {
    const a = view.status.auth || {};
    if (!a.present) {
      return ui.formRow(t('settings.colab.status_label'), h('span', { class: 'chip chip-quiet' }, t('settings.colab.status_none')), t('settings.colab.status_none_hint'));
    }
    const lc = a.lifecycle || '';
    const lcText = lc === 'session' || lc === 'manual' ? t(`colab.lifecycle.${lc}`) : /^\d+d$/.test(lc) ? t('colab.lifecycle.days', { n: parseInt(lc, 10), days: parseInt(lc, 10) }) : lc;
    const left = a.expires_at ? t('settings.colab.expires', { date: U.fmtDateTime(a.expires_at), left: durText(a.remaining_sec) || durText(0) }) : authLeftText(a);
    return ui.formRow(
      t('settings.colab.status_label'),
      h(
        'div',
        { class: 'colab-auth' },
        h('div', { class: 'set-inline colab-auth-line' }, h('span', { class: 'chip chip-ok' }, icon('circle-check', { size: 12 }), t('settings.colab.status_ok')), h('span', { class: 'dim small' }, lcText)),
        left ? h('span', { class: 'colab-auth-left' }, left) : null,
        a.imported_at ? h('span', { class: 'dim small' }, importedText(a)) : null,
      ),
      null,
      btn(disconnecting ? t('settings.colab.disconnecting') : t('settings.colab.disconnect'), { size: 'sm', kind: 'danger', icon: 'lock-open', disabled: disconnecting, fkey: 'set:colab:disconnect', onclick: disconnect }),
    );
  }

  function enabledRow() {
    const s = view.settings;
    const authed = !!(view.status.auth && view.status.auth.present);
    return ui.formRow(
      t('settings.colab.enabled_label'),
      ui.switchBtn(!!s.enabled, t('settings.colab.enabled_label'), () => save('enabled', { enabled: !s.enabled }), 'set:colab:enabled'),
      t('settings.colab.enabled_hint'),
      [savingNote('enabled'), !authed && s.enabled ? note('info', t('settings.colab.enabled_need_auth'), 'is-warn') : null, view.held ? note('circle-pause', t('settings.colab.held'), 'is-warn') : null],
    );
  }

  // colab-obs: "账户" under the authorization — the tier (paid / free,
  // from the account), the paid balance and how many lectures it covers on
  // the GPU in use, the free quota, when it was read, "重新查询". Not read:
  // "--", why and "only the T4" (the query button is then the primary one).
  // No subscription type: Colab does not give it to other programs (user
  // decision 14:50).
  function accountRow() {
    const st = view.status;
    const a = accountOf(st);
    if (!a) return null; // a back end without the account
    const known = accountKnown(a);
    const accel = effectiveAccel(st);
    const fit = fitFor(a, accel);
    const paid = a.level === 'paid';
    const quota = freeQuotaText(a);
    const balance = paidBalance(a) == null ? '--' : cuText(paidBalance(a));
    const err = accountErrorText(a);
    const box = h(
      'div',
      { class: 'acct-box' },
      h('div', { class: 'acct-line' }, h('span', { class: 'chip ' + (paid ? 'chip-run' : 'chip-quiet'), title: known ? undefined : t('resources.remote.unknown_t4') }, levelText(st))),
      h(
        'div',
        { class: 'acct-cu' },
        h('span', { class: 'acct-num' }, balance),
        h('span', { class: 'acct-unit' }, t('resources.remote.balance_k')),
        fit > 0 ? h('span', { class: 'dim small' }, t('settings.colab.account_fit', { accelerator: accel, n: U.count(fit) })) : null,
      ),
      quota ? h('div', { class: 'acct-quota small' }, icon('coins', { size: 13 }), h('span', null, quota)) : null,
      h(
        'div',
        { class: 'acct-foot' },
        h('span', { class: 'dim small' }, checkedText(a)),
        btn(t('resources.remote.refresh'), {
          size: 'sm',
          kind: accountNeedsCheck(a) ? 'primary' : 'ghost',
          icon: refreshing ? 'loader-circle' : 'refresh-cw',
          disabled: refreshing,
          fkey: 'set:colab:recheck',
          onclick: recheck,
        }),
      ),
    );
    const unlocked = known && ACCELERATORS.some((x) => x !== 'T4' && !lockOf(st, x));
    return ui.formRow(t('settings.colab.account_label'), box, t('settings.colab.account_hint'), [
      err ? h('p', { class: 'set-note is-warn', title: err.detail || undefined }, icon('circle-alert', { size: 13 }), h('span', null, err.text)) : null,
      known ? null : note('lock', t('resources.remote.unknown_t4'), 'is-warn'),
      unlocked ? note('lock-open', t('settings.colab.account_unlocked'), 'is-ok') : null,
      known && !paid ? note('triangle-alert', t('settings.colab.tier_free_hint'), 'is-warn') : null,
    ]);
  }

  // colab-obs: the accelerators as a radio list — GPU memory and memory of
  // the measured runtime, the measured time and cost per lecture, how many
  // lectures the balance covers (the free quota counts on the T4). A GPU
  // the account cannot borrow now is greyed with its numbers kept and the
  // reason in its row; a locked choice stays the user's, the T4 is used
  // meanwhile (it comes back by itself). The checked row is where lectures
  // go now (effective_accelerator).
  function accelRow() {
    const s = view.settings;
    const st = view.status;
    const a = accountOf(st);
    const eff = effectiveAccel(st) || s.accelerator;
    const list = ACCELERATORS.includes(s.accelerator) ? ACCELERATORS : [...ACCELERATORS, s.accelerator];
    // a choice that is locked now (kept; a T4 meanwhile); a default never chosen is not one
    const chosenLock = s.accelerator_chosen && s.accelerator !== eff ? lockOf(st, s.accelerator) || st.locked_reason || '' : '';
    return ui.formRow(
      t('settings.colab.accel_label'),
      h('ul', { class: 'acc-list', role: 'radiogroup', 'aria-label': t('settings.colab.accel_label') }, list.map((x) => accelItem(x, eff, a))),
      t('settings.colab.accel_measured_hint'),
      [
        chosenLock ? note('info', t('settings.colab.locked_note', { accelerator: s.accelerator, reason: lockText(chosenLock) })) : null,
        s.accelerator_chosen ? null : note('info', t('settings.colab.accel_default_account')),
        savingNote('accel'),
      ],
    );
  }

  function accelItem(x, eff, a) {
    const st = view.status;
    const spec = SPECS[x] || null;
    const lock = lockOf(st, x);
    const on = x === eff;
    const per = cuPerLecture(x);
    const fit = lock ? null : fitFor(a, x);
    const radio = h('input', {
      class: 'acc-radio',
      type: 'radio',
      name: 'colab-accel',
      value: x,
      checked: on,
      disabled: !!lock || !!saving,
      dataset: { fkey: `set:colab:accel:${x}` },
      onchange: () => save('accel', { accelerator: x }),
    });
    const size = (gb) => U.fmtMB(gb * 1024);
    let side;
    if (lock) side = [icon('lock', { size: 14 }), h('small', null, t('settings.colab.acc_locked'))];
    else if (fit > 0) {
      const byQuota = x === 'T4' && !(a.paid_balance_cu > 0);
      side = [h('b', null, t('resources.remote.fit_n', { n: U.count(fit) })), h('small', null, t(byQuota ? 'settings.colab.acc_fit_free' : 'settings.colab.acc_fit_k'))];
    } else side = null;
    return h(
      'li',
      { class: 'acc' + (on ? ' is-sel' : '') + (lock ? ' is-locked' : ''), dataset: { accel: x } },
      h(
        'label',
        { class: 'acc-in' },
        radio,
        h(
          'span',
          { class: 'acc-main' },
          h(
            'span',
            { class: 'acc-line' },
            h('span', { class: 'acc-name mono' }, x),
            spec && spec.vram ? h('span', { class: 'chip chip-quiet' }, [t('resources.gpu.vram'), size(spec.vram)].join(' ')) : null,
            spec && spec.ram ? h('span', { class: 'chip chip-quiet' }, [t('resources.system.mem'), size(spec.ram)].join(' ')) : null,
            per ? null : h('span', { class: 'chip chip-draft' }, t('settings.colab.acc_unmeasured')),
          ),
          per && has(`settings.colab.accel_info.${x}`) ? h('span', { class: 'acc-meas' }, t(`settings.colab.accel_info.${x}`)) : null,
          per ? h('span', { class: 'acc-sub' }, t('settings.colab.acc_rate', { rate: U.fmtDecimal(spec.cuh, 2) })) : null,
          lock ? h('span', { class: 'acc-why' }, icon('lock', { size: 12 }), h('span', null, lockLine(lock))) : null,
        ),
        side ? h('span', { class: 'acc-fit' }, side) : null,
      ),
    );
  }

  // colab-batch: "极速模式" (user decision 2026-10-27, off by default): on
  // the larger GPUs a lecture is cut at silences into parts transcribed at
  // once; the result differs slightly from the whole lecture's.
  function fastRow() {
    const s = view.settings;
    const accel = effectiveAccel(view.status) || s.accelerator;
    const effective = FAST_ACCELERATORS.includes(accel);
    return ui.formRow(
      t('settings.colab.fast_label'),
      ui.switchBtn(!!s.fast_mode, t('settings.colab.fast_label'), () => save('fast', { fast_mode: !s.fast_mode }), 'set:colab:fast'),
      t('settings.colab.fast_hint'),
      [s.fast_mode && !effective ? note('info', t('settings.colab.fast_no_effect', { accelerator: accel })) : null, savingNote('fast')],
    );
  }

  function modelRow() {
    const s = view.settings;
    const typed = modelDraft != null ? modelDraft : s.model;
    const inp = h('input', {
      class: 'input mono colab-model',
      value: typed,
      list: 'colab-models',
      spellcheck: 'false',
      autocomplete: 'off',
      disabled: !!saving,
      'aria-label': t('settings.colab.model_label'),
      dataset: { fkey: 'set:colab:model' },
    });
    const commit = () => {
      const v = inp.value.trim();
      modelDraft = v;
      if (v && v !== s.model) save('model', { model: v });
    };
    inp.addEventListener('change', commit);
    inp.addEventListener('keydown', (e) => e.key === 'Enter' && commit());
    const precs = PRECISIONS.includes(s.compute_type) ? PRECISIONS : [s.compute_type, ...PRECISIONS];
    const prec = h(
      'select',
      { class: 'select mono colab-sel', 'aria-label': t('settings.colab.precision_label'), disabled: !!saving, dataset: { fkey: 'set:colab:precision' }, onchange: (e) => save('model', { compute_type: e.target.value }) },
      precs.map((p) => h('option', { value: p, selected: p === s.compute_type }, p)),
    );
    return ui.formRow(
      t('settings.colab.model_label'),
      h('div', { class: 'set-inline colab-model-line' }, inp, h('datalist', { id: 'colab-models' }, MODELS.map((m) => h('option', { value: m }))), prec),
      t('settings.colab.model_hint'),
      savingNote('model'),
    );
  }

  // Memory mode (on by default): the recording, the model and the Python
  // environment stay in the Colab machine's memory; a lecture that does not
  // fit uses the disk.
  function memoryRow() {
    const s = view.settings;
    return ui.formRow(
      t('settings.colab.memory_label'),
      ui.switchBtn(!!s.memory_mode, t('settings.colab.memory_label'), () => save('memory', { memory_mode: !s.memory_mode }), 'set:colab:memory'),
      t('settings.colab.memory_hint'),
      savingNote('memory'),
    );
  }

  // colab-obs: no paid / free switch any more — the tier follows the
  // account (user decision 2026-10-27 07:00); the account row says it.

  function waitRow() {
    const s = view.settings;
    const inp = h('input', {
      class: 'input input-num colab-wait',
      type: 'number',
      min: '1',
      max: '720',
      step: '1',
      value: String(waitDraft != null ? waitDraft : s.max_queue_wait_min),
      disabled: !!saving,
      'aria-label': t('settings.colab.wait_label'),
      dataset: { fkey: 'set:colab:wait' },
    });
    const commit = () => {
      const raw = inp.value.trim();
      const n = raw === '' ? NaN : Number(raw);
      if (n === s.max_queue_wait_min) {
        waitDraft = null;
        waitError = '';
        return;
      }
      waitDraft = raw;
      if (!Number.isInteger(n) || n < 1 || n > 720) {
        waitError = t('settings.colab.wait_invalid', { min: 1, max: 720 });
        redraw();
        return;
      }
      waitError = '';
      save('wait', { max_queue_wait_min: n });
    };
    inp.addEventListener('change', commit);
    inp.addEventListener('keydown', (e) => e.key === 'Enter' && commit());
    return ui.formRow(
      t('settings.colab.wait_label'),
      h('span', { class: 'set-inline' }, inp, h('span', { class: 'dim small' }, t('settings.colab.minutes'))),
      t('settings.colab.wait_hint'),
      [waitError ? note('circle-alert', waitError, 'is-err') : null, savingNote('wait')],
    );
  }

  function batteryRow() {
    const s = view.settings;
    return ui.formRow(
      t('settings.colab.battery_label'),
      ui.switchBtn(!!s.battery_prefer, t('settings.colab.battery_label'), () => save('battery', { battery_prefer: !s.battery_prefer }), 'set:colab:battery'),
      t('settings.colab.battery_hint'),
      savingNote('battery'),
    );
  }

  // The section in its shell, which keeps what render put there before
  // (aligns: view_settings.js does not empty it); the head, the form and
  // the sign-in block are kept, the rows drawn anew as before (WV-10).
  function render(shell) {
    if (!headEl) headEl = ui.head('zap', t('settings.colab.title'));
    if (unavailable) {
      keepChildren(shell, [headEl, h('div', { class: 'card-bd' }, note('info', t('settings.colab.unavailable')))]);
      return;
    }
    if (!view) {
      keepChildren(shell, [headEl, h('div', { class: 'card-bd' }, error ? errorBlock(t('settings.colab.load_error', { error }), retry) : skeleton(4))]);
      return;
    }
    const authed = !!(view.status.auth && view.status.auth.present);
    if (!formEl) formEl = h('div', { class: 'card-bd set-form colab-form' });
    keepChildren(formEl, [
      privacyBlock(),
      saveError ? h('p', { class: 'set-note is-warn', role: 'alert' }, icon('circle-alert', { size: 13 }), h('span', null, saveError)) : null,
      h('h3', { class: 'colab-sub' }, t('settings.colab.auth_title')),
      authed ? null : appLoginBlock(),
      authRow(),
      authed ? accountRow() : null,
      authed ? null : advancedBlock(),
      h('h3', { class: 'colab-sub' }, t('settings.colab.use_title')),
      enabledRow(),
      accelRow(),
      modelRow(),
      memoryRow(),
      fastRow(),
      waitRow(),
      batteryRow(),
    ]);
    keepChildren(shell, [headEl, formEl]);
  }

  function retry() {
    error = '';
    redraw();
    load();
  }

  return {
    render,
    aligns: true, // WV-10: render puts its children in place itself
    load,
    // SSE "changed settings": another window changed Colab (or the auth)
    onSettingsChanged() {
      if (!saving && !importing && !disconnecting) load();
    },
    // SSE "changed colab": the in-app sign-in moved on
    onChanged(ev) {
      appLogin.onChanged(ev);
    },
    destroy() {
      alive = false;
      appLogin.destroy();
    },
  };
}
