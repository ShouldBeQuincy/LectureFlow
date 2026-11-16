// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Pure helpers shared by the views: no DOM, no network, no timers.
// Everything here is deterministic so it stays easy to reason about
// (SPEC §9.5 waiting_for texts, §9.6 word diff, formatting). UI text comes
// from t() (SPEC-v2 P14.2); the coded fields of the back end (P14.3: stale
// reasons, errors, notes, warnings) are rendered here, so every view shows
// them the same way.

import { t, has, getLocale, PSEUDO } from './i18n.js';

export const LECTURE_STAGES = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];

// t(prefix + code) when the catalog has it, else the code itself (an id the
// front end does not know yet stays readable instead of showing ⟦key⟧).
function named(prefix, code) {
  return code && has(prefix + code) ? t(prefix + code) : code;
}

// A read-only name table whose values are looked up with t() on every read,
// so a language switch needs no reload. Unknown keys read as undefined
// (callers fall back to the raw code); Object.keys/entries work.
function nameTable(prefix, keys) {
  const known = new Set(keys);
  const value = (k) => (typeof k === 'string' && known.has(k) ? t(prefix + k) : undefined);
  return new Proxy(
    {},
    {
      get: (_, k) => value(k),
      has: (_, k) => known.has(k),
      ownKeys: () => [...keys],
      getOwnPropertyDescriptor: (_, k) => (known.has(k) ? { value: value(k), enumerable: true, configurable: true, writable: false } : undefined),
    },
  );
}

/**
 * A Mac (3.1.0 M8): shortcut hints say ⌘ there, Ctrl elsewhere. The key
 * handlers accept both (ctrlKey || metaKey), so Ctrl keeps working on a Mac.
 */
export function isMac() {
  if (typeof navigator === 'undefined') return false;
  const p = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || '';
  return /^mac|iphone|ipad/i.test(p);
}

/** The command key of a shortcut hint: ⌘ on a Mac, else Ctrl. */
export function modKey() {
  return isMac() ? '⌘' : 'Ctrl';
}

/** A translated text naming a Ctrl shortcut ("Ctrl+E …"): ⌘ on a Mac. */
export function macKeys(text) {
  return isMac() ? String(text).replace(/\bCtrl\b/g, '⌘') : text;
}

/** List of items with the current language's list separator. */
export function listJoin(items) {
  return (items || []).join(t('common.list_sep'));
}

/** A name (a slide file's, say) in the current language's quotation marks (settings.quote), as written. */
export function quoteName(name) {
  return t('settings.quote', { name: String(name) });
}

/** Clauses joined with the current language's clause separator (compound notes, several stale reasons). */
export function clauseJoin(items) {
  return (items || []).join(t('common.clause_sep'));
}

// UI languages written with letter case, where a sentence starts with a
// capital (en-XA, the pseudo-locale, is English)
const CASED_LOCALE = /^(en|ru)(-|$)/;

/**
 * The parts of a compound note (codedText's params.extra), each a sentence
 * of its own. In English and Russian a part that starts with a capital
 * follows as a new sentence ("… upload the complete file again. The
 * recording is very quiet …"), not after the clause separator ("…; The
 * recording …", T8); the catalog's notes end without a full stop, and so
 * does the last part. Parts that start in lower case, and every part in
 * the languages without letter case (zh, yue, hi), keep the clause
 * separator; no letter's case is changed.
 * Not to be confused with joinSentences: that one takes texts that are
 * already complete sentences (each with its own end mark) and only puts
 * the space between them; sentenceJoin is for parts without a full stop.
 */
export function sentenceJoin(items) {
  const parts = items || [];
  let cased = false;
  try {
    cased = CASED_LOCALE.test(getLocale() || '');
  } catch {
    cased = false;
  }
  if (!cased) return clauseJoin(parts);
  const sep = t('common.clause_sep');
  let out = '';
  parts.forEach((p, i) => {
    const s = String(p);
    if (i === 0) out = s;
    else if (/^\p{Lu}/u.test(s)) out += (/[.!?…]$/u.test(out) ? ' ' : '. ') + s;
    else out += sep + s;
  });
  return out;
}

// A full-width sentence end (。！？), possibly inside closing quotes or a
// closing bracket (。” 。」 。）): Chinese and Cantonese go on without a
// space. Written with escapes: a CJK literal in code is a hard-coded text
// to TestI18nNoHardcodedText.
const FULL_WIDTH_STOP = /[\u3002\uff01\uff1f][\u201d\u2019\u300d\u300f\uff09]*$/u;

/**
 * Complete sentences (each a catalog text with its own end mark) as one
 * text: nothing after a full-width sentence end (。！？), one space after
 * anything else (". ", "। ", or a text without a mark). Empty parts are
 * left out. Unlike sentenceJoin, no mark or separator is added: the parts
 * already are sentences. Use this one for catalog texts that end in their
 * own full stop (rate-limit notes, VRAM hints); use sentenceJoin for the
 * parts of a compound note, which end without one and are joined by the
 * clause separator (or as new sentences in en / ru).
 */
export function joinSentences(parts) {
  return (parts || [])
    .filter((p) => p != null && p !== false && String(p) !== '')
    .map(String)
    .reduce((a, b) => (a === '' ? b : a + (FULL_WIDTH_STOP.test(a) ? '' : ' ') + b), '');
}

/**
 * The text with its first letter in upper case, in the UI language's rules
 * ("пекинское время · …" → "Пекинское время · …"); scripts without letter
 * case are unchanged.
 */
export function upperFirst(s) {
  const text = String(s || '');
  if (!text) return '';
  const first = String.fromCodePoint(text.codePointAt(0));
  let up;
  try {
    up = first.toLocaleUpperCase(getLocale() || undefined);
  } catch {
    up = first.toUpperCase();
  }
  return up + text.slice(first.length);
}

// Stage names: stage.{id} (lecture, course and material stages; v2 G3.1
// glossary_review; v2 auxiliary tasks).
export function stageName(stage) {
  return named('stage.', stage);
}

// Role names: role.{id} (the LLM roles of the settings page).
export function roleName(role) {
  return named('role.', role);
}

// Thinking levels of a role (model.ThinkingLevels; "off" is the default).
export const THINKING_LEVELS = ['off', 'low', 'high', 'max'];

// The label of a model snapshot (StageModel, SnapshotView) in the interface
// language: "DeepSeek / deepseek-flash · temperature 0.3 · window 3 ·
// thinking: high" from the structured fields (an LLM snapshot with params;
// thinking off is not shown). Other snapshots (asr: "device · model ·
// precision · language" since phase 3b, no language words) and older
// responses without params keep the back end's preformatted label; an
// empty side is "(none)".
export function modelLabel(m) {
  if (!m) return '';
  // asr (SPEC-v2 I6, gate round 2): an automatic snapshot not bound to a
  // device yet has no device name: "Automatic (waiting for a device) · model
  // · precision · language" from the structured snapshot (SnapshotView.asr)
  const a = m.asr;
  if (a && a.auto && !a.device_id) return [t('common.model.asr_auto_waiting'), a.model, a.precision || a.compute_type, a.language].filter(Boolean).join(' · ');
  const p = m.params;
  if (p && m.model) {
    const parts = [[m.provider_name, m.model].filter(Boolean).join(' / ')];
    // with thinking on the temperature is not sent to the model
    const thinking = p.thinking === 'low' || p.thinking === 'high' || p.thinking === 'max';
    if (typeof p.temperature === 'number' && !thinking) parts.push(t('common.model.temperature', { value: fmtDecimal(p.temperature) }));
    if (typeof p.window === 'number') parts.push(t('common.model.window', { n: p.window }));
    if (p.thinking === 'low') parts.push(t('common.model.thinking_low'));
    else if (p.thinking === 'high') parts.push(t('common.model.thinking_high'));
    else if (p.thinking === 'max') parts.push(t('common.model.thinking_max'));
    return parts.join(' · ');
  }
  if (!m.model && !m.asr && !m.provider_id) return t('common.model.none');
  return m.label || '';
}

// pending … cancelled, and v1.1 A2.1's display-only "outdated".
export function statusName(status) {
  return named('common.status.', status);
}

// v1.1 A2.1: the back end computes display_status (status, or "outdated").
export function displayStatus(st) {
  return st.display_status || st.status;
}

// Error codes of a failure that a retry cannot fix (test-media I6): the
// stored media file itself is damaged, so the stage's retry (still among
// its allowed_actions, SPEC §4) runs the same file again. Retry is then no
// primary action, and the stage card explains what to do instead.
const FUTILE_RETRY_CODES = new Set(['media.corrupt']);

/** Whether a failed stage (or a dashboard failure) has an error that a retry cannot fix. */
export function retryFutile(st) {
  return !!(st && st.error_info && FUTILE_RETRY_CODES.has(st.error_info.code));
}

// Error codes of a failure that the same request cannot get past, and that
// is billed again when retried as it is (T24 B1): the model refused
// (llm.refusal), or the answer was still coming at the total time limit
// (llm.time_limit). The way out is another model: where such a failure is
// shown, "retry with another model…" is the action offered first, not a
// plain retry. Shared by the lecture page, the stage cards, the dashboard
// and the lecture rows: codes may be added here, the names stay.
export const MODEL_RETRY_CODES = new Set(['llm.refusal', 'llm.time_limit']);

/** Whether a failed stage (or a dashboard failure) needs another model rather than a plain retry. */
export function retryNeedsModel(st) {
  return !!(st && st.error_info && MODEL_RETRY_CODES.has(st.error_info.code));
}

// Whether the first-run guide (GET /api/onboarding) was left unfinished and
// can be continued: started and not done. A state never started (the welcome
// step with no answers, e.g. an install from before the guide) asks for
// nothing. The settings page's rule, shared (round 8 interface F3) so every
// "continue the guide" entry judges the same.
export function guideLeft(guide) {
  return !!guide && !guide.done && (guide.step !== 'welcome' || (!!guide.data && Object.keys(guide.data).length > 0));
}

// Error codes of a failure whose way out is in Settings (T24 S3, T22 G7),
// by the block of the settings page that fixes it: a provider's key, a
// provider the config names but does not set up, a request rejected or
// failed for the key, base_url or the model name (the providers); a role
// without a model, or whose provider was deleted (the roles).
const SETTINGS_FIX_BLOCKS = {
  'llm.no_api_key': 'providers',
  'llm.provider_not_configured': 'providers',
  'llm.rejected': 'providers',
  'llm.request_failed': 'providers',
  'llm.no_model': 'roles',
  'llm.provider_deleted': 'roles',
  'settings.role_not_configured': 'roles',
  'settings.role_provider_deleted': 'roles',
};

/**
 * Whether a failed LLM request got the answer an unknown model gets: HTTP
 * 404 (llm.request_failed), or 400 (llm.rejected; DeepSeek's "Model Not
 * Exist"); never a key refused (401, 403). Only then is the model looked up
 * in its provider's list (unlistedModel), and the page reads the settings.
 */
export function mayBeUnlisted(st) {
  const info = st && st.error_info;
  const status = Number(info && info.params && info.params.status);
  return !!info && ((info.code === 'llm.request_failed' && status === 404) || (info.code === 'llm.rejected' && status === 400));
}

/**
 * The model of a failed LLM request (mayBeUnlisted) that its provider's
 * last probe does not list (T24 S3, T22 G8: the "oldmodel" case), as
 * {model, provider (its name)}; null otherwise. providers: the settings'
 * providers (GET /api/settings, each with its probed `models`); the
 * snapshot's provider is found by id (st.model), else by the error's
 * provider name. Without the settings, or a provider never probed (no
 * list), nothing is known: null.
 */
export function unlistedModel(st, providers) {
  if (!mayBeUnlisted(st) || !Array.isArray(providers)) return null;
  const p = st.error_info.params || {};
  const m = st.model || {};
  const prov = (m.provider_id && providers.find((x) => x && x.id === m.provider_id)) || (p.provider && providers.find((x) => x && x.name === p.provider)) || null;
  const model = m.model || p.model;
  if (!prov || !model || !Array.isArray(prov.models) || !prov.models.length || modelListed(model, prov.models)) return null;
  return { model, provider: prov.name || prov.id };
}

// The date that dated model ids append to an alias ("…-20250929").
const DATED_SUFFIX = /^[0-9]{6,8}$/;

/**
 * Whether a model name is in a provider's model list, by the back end's
 * rule (internal/api/role_models.go modelListed, the diagnostics' and the
 * settings page's; R72 M2): the same id ignoring case and the spaces
 * around it, or an alias of a dated id in the list — "claude-sonnet-4-5"
 * (or "claude-sonnet-4-5-latest") for "claude-sonnet-4-5-20250929":
 * providers answer such aliases while their lists show the dated ids.
 */
export function modelListed(name, list) {
  if (!Array.isArray(list)) return false;
  const m = String(name == null ? '' : name).trim().toLowerCase();
  const base = m.endsWith('-latest') ? m.slice(0, -'-latest'.length) : m;
  return list.some((x) => {
    const id = String(x).toLowerCase();
    return id === m || (id.startsWith(base + '-') && DATED_SUFFIX.test(id.slice(base.length + 1)));
  });
}

// The stages whose retry may name another model ("retry with another
// model…"): pipeline.js MODEL_RETRY_STAGES, which util.js cannot import
// (pipeline.js imports util.js); r8_shared_test.go pins the two together.
const MODEL_RETRY_STAGE_SET = new Set(['summary', 'repair', 'translate']);

/**
 * Where in Settings a failed stage's way out is (T24 S3, T22 G7, G8): {href
 * (#/settings/{block}; a provider's row #/settings/providers/{id} when the
 * failure tells which, failedProviderId), unlisted (unlistedModel, or
 * null)}; null when Settings does not fix it. A model its provider no
 * longer lists is changed in the roles. providers as for unlistedModel
 * (null: not known).
 * A refusal or the time limit (MODEL_RETRY_CODES) of a stage that has no
 * "retry with another model…" (the course's term extraction and glossary
 * cleanup, the slides' terms, a lecture's title task: no snapshot to retry
 * another model with) is fixed in the roles: a plain retry would fail, and
 * be billed, again (T24 B1; the stage card then puts "open settings" first).
 */
export function settingsFix(st, providers) {
  const code = st && st.error_info && st.error_info.code;
  const block = code && Object.prototype.hasOwnProperty.call(SETTINGS_FIX_BLOCKS, code) ? SETTINGS_FIX_BLOCKS[code] : '';
  if (!block) return code && MODEL_RETRY_CODES.has(code) && !MODEL_RETRY_STAGE_SET.has(st.stage) ? { href: '#/settings/roles', unlisted: null } : null;
  const unlisted = unlistedModel(st, providers);
  if (unlisted) return { href: '#/settings/roles', unlisted };
  // a provider's key or address: its row (#/settings/providers/{id}, round
  // 8 convention F4) when the failure tells which one, else the section
  const id = block === 'providers' ? failedProviderId(st, providers) : '';
  return { href: id ? `#/settings/providers/${encodeURIComponent(id)}` : `#/settings/${block}`, unlisted: null };
}

/**
 * The id of the provider a failed request names, or ''. The back end names
 * it by its name (error_info.params.provider: llm.no_api_key, llm.rejected,
 * llm.request_failed); an id it may give (params.provider_id) is taken as
 * it is. The name is looked up in the providers the page read: one of that
 * name, its id; several, the one the stage's snapshot names (st.model); not
 * found, ''. The providers not read (a key failure reads none): the
 * snapshot's provider when it has that name (a lecture stage's request
 * goes to its snapshot's provider, whose current name the view gives).
 */
function failedProviderId(st, providers) {
  const p = (st.error_info && st.error_info.params) || {};
  if (typeof p.provider_id === 'string' && p.provider_id) return p.provider_id;
  if (!p.provider) return '';
  const m = st.model || {};
  if (!Array.isArray(providers)) return m.provider_id && m.provider_name === p.provider ? m.provider_id : '';
  const named = providers.filter((x) => x && x.id && x.name === p.provider);
  if (named.length === 1) return named[0].id;
  return named.length > 1 && m.provider_id && named.some((x) => x.id === m.provider_id) ? m.provider_id : '';
}

// A done stage that still carries an error text finished with a warning
// (e.g. repair chunks that could not be repaired, NOTES.md Gate B 4). It is
// not a failure: no failure styling, no failure notification.
export function hasWarning(st) {
  return st.status === 'done' && !!st.error;
}

// "Running 42%" for running stages, "Queued · 39% done (checkpoint)" for a
// queued stage that keeps its checkpoint, the plain status name otherwise.
export function stageStatusText(st) {
  if (st.status === 'running') return Number(st.progress) > 0 ? t('common.status.running_pct', { pct: pct(st.progress) }) : startingText(st);
  const ck = checkpointProgress(st);
  if (ck != null) return t('common.status.queued_checkpoint', { pct: pct(ck) });
  if (hasWarning(st)) return t('common.status.done_warning');
  return statusName(displayStatus(st));
}

// A run that has reported no progress yet (T25 G1, G2): not "运行中 0%". A
// transcription on this computer first loads its model and finds the
// speech; run again (a retry, the restart after the app was closed) it may
// go on from its checkpoint — said without a promise (R79 M3: another
// device, model or snapshot starts over, asr.go discardForeignCheckpoint).
// A remote transcription never continues the local checkpoint, and a
// translation retried with another model starts over: they, and anything
// else, are getting ready.
function startingText(st) {
  if (st.stage !== 'asr' || remoteASR(st)) return t('common.status.running_preparing');
  return st.attempt > 0 ? t('common.status.running_resuming') : t('common.status.running_preparing_asr');
}

/** Whether an asr stage runs (or is bound to run) away from this computer: Colab, an SSH server. */
export function remoteASR(st) {
  if (!st || st.stage !== 'asr') return false;
  if (st.stats && st.stats.remote) return true;
  const d = st.model && st.model.asr ? st.model.asr.device_id : '';
  return typeof d === 'string' && (d === 'colab:remote' || d.startsWith('ssh:'));
}

// Short hint for the stages that finished with a warning ("Repair has warnings").
export function warningHint(stages) {
  const names = (stages || []).filter(hasWarning).map((s) => stageName(s.stage));
  return names.length ? t('common.warning_hint', { stages: listJoin(names) }) : '';
}

export function actionName(action) {
  return named('common.action.', action);
}

// Review classes and states (SPEC §6.4, §9.6).
export const CLASS_NAMES = nameTable('common.class.', ['auto', 'absorb', 'decide']);
export const CLASS_HINTS = nameTable('common.class_hint.', ['auto', 'absorb', 'decide']);
export const REVIEW_NAMES = nameTable('common.review_state.', ['none', 'pending', 'accepted', 'rejected', 'edited']);
// Glossary vocabulary (these keys live in the glossary section of the catalogs).
export const SOURCE_NAMES = nameTable('glossary.source.', ['syllabus', 'material', 'review', 'manual']);
// v2 G1: term tiers and the three groups of the glossary (disabled terms
// form their own group whatever their tier).
export const TIER_NAMES = nameTable('glossary.tier.', ['enforce', 'reference']);
// v2 G6 rule 3: where a term's translation came from.
export const ZH_SOURCE_NAMES = nameTable('glossary.zh_source.', ['extractor', 'janitor', 'user']);
// v2 G3.3 janitor actions, as the report shows them.
export const JANITOR_ACTION_NAMES = nameTable('glossary.janitor_action.', ['enforce', 'reference', 'disable', 'merge', 'fix_zh']);

// Diagnostics checks: product names stay as they are; the others are named
// by common.check.{name}.
const CHECK_PRODUCTS = { ffmpeg: 'ffmpeg', python: 'Python', 'faster-whisper': 'faster-whisper', pypdf: 'pypdf', gpu: 'GPU', openvino: 'OpenVINO', mlx: 'MLX' };
// SPEC-v2 I10 added openvino_venv, openvino_devices and npu_driver; 3.1.0
// (mac-opti M3) mlx_python and mlx_model
const CHECK_KEYS = new Set(['data_dir', 'credentials', 'data_dir_location', 'install_dir', 'cuda', 'disk', 'openvino_venv', 'openvino_devices', 'npu_driver', 'mlx_python', 'mlx_model']);

export function checkName(name) {
  if (CHECK_KEYS.has(name)) return t(`common.check.${name}`);
  if (CHECK_PRODUCTS[name]) return CHECK_PRODUCTS[name];
  if (name.startsWith('api_key:')) return t('common.check.api_key', { name: name.slice('api_key:'.length) });
  if (name.startsWith('api:')) return t('common.check.api', { name: name.slice('api:'.length) });
  // "role_model:{provider}/{model}": a role's model missing from the provider's list
  if (name.startsWith('role_model:')) {
    const rest = name.slice('role_model:'.length);
    const i = rest.indexOf('/');
    return t('common.check.role_model', { provider: i < 0 ? rest : rest.slice(0, i), model: i < 0 ? '' : rest.slice(i + 1) });
  }
  return name;
}

export const CHECK_STATUS_NAMES = nameTable('common.check_status.', ['ok', 'warn', 'error']);

// Floor, so 0.999 shows as 99% rather than a misleading 100%.
export function pct(p) {
  const v = Math.max(0, Math.min(1, Number(p) || 0));
  return numberFormat(Math.floor(v * 100) / 100, { style: 'percent', maximumFractionDigits: 0 });
}

const WAIT_UPSTREAM = 'upstream:';
const WAIT_MATERIAL_TERMS = 'material_terms:';
const WAIT_RETRY_BACKOFF = 'retry_backoff:';
const WAIT_OFFPEAK = 'offpeak:';
const WAIT_DEVICE_PAUSED = 'device_paused:';
// fresh-cuda: the device's environment or model is missing or being installed
const WAIT_COMPONENT_MISSING = 'component_missing:';
const WAIT_MODEL_MISSING = 'model_missing:';
const WAIT_COMPONENT_INSTALLING = 'component_installing:';
const WAIT_DEVICE_SELFTEST = 'device_selftest:';
const WAIT_POWER_QUEUE = 'power_queue:';
const WAIT_POWER_SWITCH = 'power_switch_pending:';

// ---- transcription devices (SPEC-v2 I6-I10) ----

function deviceLists(sys, res) {
  return [(res && res.devices) || [], (sys && sys.devices) || []];
}

function hasDevices(sys, res) {
  return deviceLists(sys, res).some((l) => l.length > 0);
}

/**
 * Whether the power-aware rules of a machine with a battery apply (power
 * management): the resources snapshot's policy.battery_machine; before the
 * first snapshot with a policy, a known power state with a battery.
 */
export function batteryMachine(res) {
  if (!res) return false;
  if (res.policy) return !!res.policy.battery_machine;
  return !!(res.power && res.power.known && res.power.has_battery);
}

// One device at a time (devices.Place rules 5a–5e): a machine with a
// battery outside the manual mode (policy.mode is the effective mode, so
// "manual" only while plugged in).
function singleActive(res) {
  return !!(res && res.policy && res.policy.battery_machine && res.policy.mode !== 'manual');
}

/**
 * Whether a waiting_for is the power-aware policy's (power_* codes, and a
 * self-test wait under the single-active rule): their texts say "挂起 /
 * 排队：电源感知 - …" themselves, so summaries do not add "queued" to them.
 */
export function isPowerWait(w, res) {
  if (!w) return false;
  return w.startsWith('power_') || (w.startsWith(WAIT_DEVICE_SELFTEST) && singleActive(res));
}

/**
 * What waitingForText takes from a resources snapshot (the devices' names,
 * the power policy's single-active state): views that render waiting texts
 * before the first snapshot redraw them when this changes.
 */
export function waitContextKey(res) {
  if (!res) return '';
  return JSON.stringify([((res && res.devices) || []).map((d) => [d.id, d.label]), singleActive(res), batteryMachine(res)]);
}

/** A device's own name (its label, never translated) from the latest device lists; its id when unknown. */
export function deviceLabel(id, sys, res) {
  for (const list of deviceLists(sys, res)) {
    const d = list.find((x) => x.id === id);
    if (d && d.label) return d.label;
  }
  return id;
}

/** The kind of a device in the interface language (devices.kind.{cuda|igpu|npu|cpu|apple|remote}); '' when unknown. */
export function deviceKindName(kind) {
  return kind ? named('devices.kind.', kind) : '';
}

/** Model and precision of a device or a test result: "large-v3-turbo · fp16" (not translated). */
export function comboText(m, precision) {
  return [m, precision].filter(Boolean).join(' · ');
}

/**
 * A device's inference back end as the resource monitor names it (brand
 * names, not translated): OpenVINO; CUDA for faster-whisper on an NVIDIA
 * card; faster-whisper on the CPU.
 */
export function backendName(d) {
  if (!d) return '';
  if (d.backend === 'openvino') return 'OpenVINO';
  if (d.backend === 'mlx') return 'MLX';
  if (d.kind === 'cuda') return 'CUDA';
  return d.backend || '';
}

/**
 * Power management (real-machine check 6.8 item 2): the share of the lecture
 * a queued asr stage already holds in its checkpoint (StageView.checkpoint,
 * set by the back end only after a power interrupt), or null — a normal
 * queued stage, and every other one, has none. progress stays the job's own.
 */
export function checkpointProgress(st) {
  const c = st && st.status === 'queued' ? st.checkpoint : null;
  const p = c ? Number(c.progress) : NaN;
  return p > 0 ? Math.min(1, p) : null;
}

/**
 * How far a stage's bar is filled: the progress of a running stage, the
 * checkpoint of a queued one that keeps it, null otherwise (no fill).
 */
export function barProgress(st) {
  if (!st) return null;
  if (st.status === 'running') return st.progress;
  return checkpointProgress(st);
}

/**
 * The parts of an asr snapshot that was resumed on another device (power
 * management: snapshot parts) when there are two or more and they differ in
 * device, model or precision; null otherwise.
 */
export function asrParts(m) {
  const parts = m && m.asr && Array.isArray(m.asr.parts) ? m.asr.parts : [];
  if (parts.length < 2) return null;
  const key = (p) => [p.device_id, p.model, p.precision].join('|');
  return parts.some((p) => key(p) !== key(parts[0])) ? parts : null;
}

// Text for a waiting_for value (SPEC §9.5, v1.1 A3.1 not_started; the codes
// are model.Wait*). `sys` is the latest GET /api/system response (vram
// numbers); `res` the latest resources snapshot (live ASR slot usage, v1.1 A10).
export function waitingForText(w, sys, res) {
  if (!w) return '';
  if (w === 'not_started') return t('waiting.not_started');
  if (w.startsWith(WAIT_UPSTREAM)) return t('waiting.upstream', { stage: stageName(w.slice(WAIT_UPSTREAM.length)) });
  if (w === 'glossary_seed') return t('waiting.glossary_seed');
  if (w === 'glossary_review') return t('waiting.glossary_review'); // v2 G3.1
  if (w === 'no_provider') return t('waiting.no_provider'); // 3.1.0 "transcribe only": no model provider set up
  if (w.startsWith(WAIT_MATERIAL_TERMS)) return t('waiting.material_terms', { filename: w.slice(WAIT_MATERIAL_TERMS.length) });
  // SPEC-v2 I8 (phase 3b): with transcription devices, gpu_paused is the
  // main switch over every device and gpu_slot means every device the job
  // may use is busy; the device of device_paused:{id} / device_selftest:{id}
  // is named by its own label (resources or system device lists).
  // (before the first system / resources response the device wording is
  // used too: every 3b back end has devices; a back end without them sends
  // neither device lists nor the power state)
  const withDevices = (!sys && !res) || hasDevices(sys, res) || !!(res && res.power) || !!(sys && sys.power);
  if (w === 'gpu_paused') return withDevices ? t('waiting.all_paused') : t('waiting.gpu_paused');
  if (w.startsWith(WAIT_DEVICE_PAUSED)) return t('waiting.device_paused', { device: deviceLabel(w.slice(WAIT_DEVICE_PAUSED.length), sys, res) });
  if (w.startsWith(WAIT_COMPONENT_MISSING)) return t('waiting.component_missing', { device: deviceLabel(w.slice(WAIT_COMPONENT_MISSING.length), sys, res) });
  if (w.startsWith(WAIT_MODEL_MISSING)) return t('waiting.model_missing', { device: deviceLabel(w.slice(WAIT_MODEL_MISSING.length), sys, res) });
  if (w.startsWith(WAIT_COMPONENT_INSTALLING)) return t('waiting.component_installing', { device: deviceLabel(w.slice(WAIT_COMPONENT_INSTALLING.length), sys, res) });
  if (w.startsWith(WAIT_DEVICE_SELFTEST)) {
    // on a machine with a battery the job waits for the test because only
    // one device is active at a time (power-aware, requirement 7)
    const device = deviceLabel(w.slice(WAIT_DEVICE_SELFTEST.length), sys, res);
    return singleActive(res) ? t('waiting.power_device_selftest', { device }) : t('waiting.device_selftest', { device });
  }
  if (w === 'on_battery') return t('waiting.on_battery'); // before power management (old data)
  // power management: the power-aware waits (each says so, requirement 7);
  // device ids are shown by the devices' own names
  if (w === 'power_wait_ac') return t('waiting.power_wait_ac');
  if (w === 'power_no_saver_device') return t('waiting.power_no_saver_device');
  if (w === 'power_no_device') return t('waiting.power_no_device');
  if (w.startsWith(WAIT_POWER_QUEUE)) return t('waiting.power_queue', { device: deviceLabel(w.slice(WAIT_POWER_QUEUE.length), sys, res) });
  if (w.startsWith(WAIT_POWER_SWITCH)) {
    // power_switch_pending:{from}:{to}, each id with one colon (model.ParseWaitPowerSwitchPending)
    const p = w.slice(WAIT_POWER_SWITCH.length).split(':');
    if (p.length === 4 && p.every(Boolean)) return t('waiting.power_switch_pending', { from: deviceLabel(`${p[0]}:${p[1]}`, sys, res), to: deviceLabel(`${p[2]}:${p[3]}`, sys, res) });
  }
  // SPEC-v3 V14.5: Google Colab; remote_busy / remote_fallback / remote_held
  // (colabfix): the lecture waits for a busy Colab, or Colab handed it back
  // or a shutdown holds the automatic dispatch while no local device can
  // take it (the asr stage then offers use_colab)
  if (w === 'remote_queue') return t('waiting.remote_queue');
  if (w === 'remote_window') return t('waiting.remote_window');
  if (w === 'remote_auth') return t('waiting.remote_auth');
  if (w === 'remote_balance') return t('waiting.remote_balance');
  if (w === 'remote_busy') return t('waiting.remote_busy');
  if (w === 'remote_fallback') return t('waiting.remote_fallback');
  if (w === 'remote_held') return t('waiting.remote_held');
  // ssh-mvp / ssh-1b: the user's own SSH servers — not reachable now, the
  // environment not installed (or installing), handed back with no local
  // device to take it (the lecture menu offers "改用“服务器”处理")
  if (w === 'remote_offline') return t('waiting.remote_offline');
  if (w === 'remote_setup') return t('waiting.remote_setup');
  if (w === 'remote_fallback_ssh') return t('waiting.remote_fallback_ssh');
  if (w === 'memory') return t('waiting.memory');
  if (w === 'no_asr_device') return t('waiting.no_asr_device');
  if (w === 'gpu_slot' && withDevices) return t('waiting.device_slot');
  if (w === 'gpu_slot') {
    const r = res && res.gpu;
    if (r && r.asr_slots_max) return t('waiting.gpu_slot_count', { used: r.asr_slots_used, max: r.asr_slots_max });
    const g = sys && sys.gpu;
    return g ? t('waiting.gpu_slot_count', { used: g.slots_used, max: g.slots_max }) : t('waiting.gpu_slot');
  }
  if (w === 'vram') {
    const g = sys && sys.gpu;
    if (!g) return t('waiting.vram');
    // The scheduler needs free - vram_reserve_mb >= estimate (SPEC §5), so
    // the reserve is part of what is "needed" when the config exposes it.
    const reserve = Number(sys.config && sys.config.asr && sys.config.asr.vram_reserve_mb) || 0;
    return t('waiting.vram_detail', { free: fmtInt(g.free_mb), need: fmtInt(g.estimated_job_mb + reserve) });
  }
  if (w === 'cpu_slot') return t('waiting.cpu_slot'); // added by lead (model.WaitCPUSlot)
  if (w === 'api_slot') return t('waiting.api_slot');
  if (w === 'budget') return t('waiting.budget');
  if (w.startsWith(WAIT_RETRY_BACKOFF)) {
    const raw = w.slice(WAIT_RETRY_BACKOFF.length);
    const n = Number(raw);
    return t('waiting.retry_backoff', { n: raw !== '' && Number.isFinite(n) ? n : raw });
  }
  // v2 P12.6: offpeak:{local HH:MM} — the provider only takes requests off-peak
  if (w.startsWith(WAIT_OFFPEAK)) return t('waiting.offpeak', { time: w.slice(WAIT_OFFPEAK.length) });
  return w;
}

// The power-aware waits that hold a transcription until the computer is
// plugged in ("挂起：电源感知 - …"; on_battery before power management).
const POWER_SUSPENDED = new Set(['power_wait_ac', 'power_no_saver_device', 'power_no_device', 'on_battery']);

/** Whether a waiting_for holds the transcription until the computer is plugged in (T24 S4). */
export function powerSuspended(w) {
  return typeof w === 'string' && POWER_SUSPENDED.has(w);
}

/**
 * Whether the resources list an Intel GPU or NPU that is present but turned
 * off (T24 S4): turned on in Settings, it could take a transcription the
 * power policy holds on battery. false without a snapshot.
 */
export function saverDeviceOff(res) {
  return !!res && Array.isArray(res.devices) && res.devices.some((d) => !!d && (d.kind === 'igpu' || d.kind === 'npu') && !!d.present && !d.enabled);
}

/**
 * Whether plugging in gives a transcription a device (the policy's AC
 * chain, internal/devices/policy.go: the first usable NVIDIA GPU, else the
 * first usable Intel GPU; the NPU and the CPU are not in it): a cuda or
 * igpu device that is turned on and present. true without a snapshot (not
 * known: the notice says what holds as a rule).
 */
export function acDeviceUsable(res) {
  if (!res || !Array.isArray(res.devices)) return true;
  return res.devices.some((d) => !!d && (d.kind === 'cuda' || d.kind === 'igpu') && !!d.present && !!d.enabled);
}

// ---- the retry countdown (3.2.0 L12) ----
// waiting_for "retry_backoff:{s}" holds the seconds left when the lecture
// was read (sched.WaitingFor); the texts count down from there, by a
// deadline kept per stage object: a view that redraws the same lecture
// counts on, a lecture read again starts from the server's new number.
const backoffs = new WeakMap(); // stage → {w, until}

/** Seconds left of a stage's retry backoff (0: due), or null when it has none. */
export function backoffLeft(st, now = Date.now()) {
  const w = st && st.waiting_for;
  if (typeof w !== 'string' || !w.startsWith(WAIT_RETRY_BACKOFF)) return null;
  const raw = w.slice(WAIT_RETRY_BACKOFF.length);
  const n = Number(raw);
  if (raw === '' || !Number.isFinite(n)) return null;
  let b = backoffs.get(st);
  if (!b || b.w !== w) {
    b = { w, until: now + n * 1000 };
    backoffs.set(st, b);
  }
  return Math.max(0, Math.ceil((b.until - now) / 1000));
}

/** True while a stage of stages counts down to its retry (backoffLeft > 0). */
export function backoffCounting(stages) {
  return (stages || []).some((s) => (backoffLeft(s) || 0) > 0);
}

/**
 * A stage's waiting_for text (waitingForText) with its retry countdown
 * counted from when the stage was read; "about to retry" once it is due
 * (the scheduler starts the retry within a second, or it waits for a slot).
 */
export function stageWaitingText(st, sys, res) {
  if (!st || !st.waiting_for) return '';
  const left = backoffLeft(st);
  if (left === 0) return t('waiting.retry_backoff_due');
  if (left != null) return t('waiting.retry_backoff', { n: left });
  return waitingForText(st.waiting_for, sys, res);
}

// ---- coded back-end texts (SPEC-v2 P14.3) ----

// Param values that are codes the front end names itself (codes-p2
// conventions in testdata/api/error_codes_backend.json): role and stage ids,
// job statuses, playback-file states; sizes by their unit suffix (*_bytes,
// *_mb). Other lists are joined as a list (e.g. the missing regex groups);
// a nested coded text ({code, params, detail}, e.g. the reason of a
// calendar.file_invalid problem) is rendered in the same namespace first.
// Everything else is interpolated as is.
// Integer params shown with the language's digit grouping, by code (other
// integers may be years, ports or ids: left as they are).
const GROUPED_INT_PARAMS = { 'check.gpu_vram': ['total', 'free'] };

function paramValue(ns, name, v, code) {
  if (name === 'role' && typeof v === 'string') return roleName(v);
  if (name === 'roles' && Array.isArray(v)) return listJoin(v.map(roleName));
  if (name === 'stage' && typeof v === 'string') return stageName(v);
  if (name === 'stages' && Array.isArray(v)) return listJoin(v.map(stageName));
  if (name === 'status' && typeof v === 'string') return statusName(v);
  if (name === 'stream_status' && typeof v === 'string') return named('common.stream_status.', v);
  if (typeof v === 'number' && isFinite(v)) {
    // a size the back end could not tell is sent as -1 (components.disk_full
    // when a copy hit a full disk, internal/components/importfile.go;
    // remote.no_space without the agent's figures, internal/remote
    // protocol.go): "unknown", as the components page says it
    // (components_ui.js), not "-1 MB"
    if ((name.endsWith('_bytes') || name.endsWith('_mb')) && v < 0) return t('compui.unknown_size');
    if (name.endsWith('_bytes')) return fmtBytes(v);
    if (name.endsWith('_mb')) return fmtMB(v);
    // decimals with the language's decimal sign (qa.length_ratio {ratio} 0.38, {min} 0.5)
    if (!Number.isInteger(v)) return fmtDecimal(v, 2);
    if ((GROUPED_INT_PARAMS[code] || []).includes(name)) return fmtInt(v);
  }
  if (Array.isArray(v) && v.every((x) => x == null || typeof x !== 'object')) return listJoin(v.map(String));
  if (v && typeof v === 'object' && !Array.isArray(v) && typeof v.code === 'string') return codedText(ns, v, '');
  return v;
}

/**
 * The text of a coded back-end text (ErrorInfo {code, params, detail}):
 * t(ns + "." + code, params) with ns "errors" | "notes" | "warnings".
 * params.extra ([]ErrorInfo, compound notes) are rendered in order and
 * joined as clauses. Without a code, or when the catalog has no such key,
 * the detail, then `fallback`, then the code itself.
 */
export function codedText(ns, info, fallback = '') {
  if (!info || !info.code) return (info && info.detail) || fallback || '';
  const key = `${ns}.${info.code}`;
  if (!has(key)) return info.detail || fallback || info.code;
  const raw = info.params || {};
  const params = {};
  for (const [k, v] of Object.entries(raw)) if (k !== 'extra') params[k] = paramValue(ns, k, v, info.code);
  const main = t(key, params);
  const extra = Array.isArray(raw.extra) ? raw.extra.map((x) => codedText(ns, x, '')).filter(Boolean) : [];
  return extra.length ? sentenceJoin([main, ...extra]) : main;
}

/** A coded error (HTTP error body, job error, batch item, balance, provider test). */
export function errorInfoText(info, fallback = '') {
  return codedText('errors', info, fallback);
}

/**
 * A stage's error: its coded warning (done with a warning) or error, else
 * the text. While the stage is retried (retryPending: the countdown, then
 * queued for a slot and running the next attempt — the back end keeps the
 * last error until the retry succeeds), an error whose text tells the user
 * to click "重试" (there is no button then, the retry comes by itself) is
 * said without that (T24 G5, R72 M3): common.retrying.{code}, the same
 * sentence up to the advice.
 * A refusal or the time limit (MODEL_RETRY_CODES) of a lecture's summary,
 * correction or translation (MODEL_RETRY_STAGE_SET) is said with only the
 * way out these stages have, "retry with another model…"
 * (common.lecture_error.{code}, T28 suggestion 2): errors.{code} also says
 * where the other tasks change the model (Settings → Roles), which the
 * lecture page's banner and stage cards do not need. A catalog without the
 * key: errors.{code}, as before. Course-level stages and the other callers
 * of errorInfoText are unchanged.
 */
export function stageErrorText(st) {
  if (!st) return '';
  if (hasWarning(st) && st.warning_info) return codedText('warnings', st.warning_info, st.error || '');
  if (st.error_info) {
    const code = st.error_info.code;
    if (retryPending(st) && code && has(`common.retrying.${code}`)) return codedText('common.retrying', st.error_info, st.error || '');
    if (MODEL_RETRY_CODES.has(code) && MODEL_RETRY_STAGE_SET.has(st.stage) && has(`common.lecture_error.${code}`)) return codedText('common.lecture_error', st.error_info, st.error || '');
    return codedText('errors', st.error_info, st.error || '');
  }
  return st.error || '';
}

/**
 * Whether a stage's error is a past attempt's, the stage being retried: it
 * is queued or running again (an automatic retry, a retry the user started,
 * the restart after the app was closed), or it failed and still waits for
 * its automatic retry (waiting_for retry_backoff:{s}). Not a final failure.
 */
export function retryPending(st) {
  if (!st) return false;
  if (st.status === 'queued' || st.status === 'running') return true;
  return typeof st.waiting_for === 'string' && st.waiting_for.startsWith(WAIT_RETRY_BACKOFF);
}

/** A stage's note (job.stats.note_info, else the note text). */
export function noteText(st) {
  if (!st) return '';
  return st.note_info ? codedText('notes', st.note_info, st.note || '') : st.note || '';
}

// One stale reason's text; keep(s) is what stands in it for a text the
// reason carries as written (a file name, a list of them, a text recorded
// before v2, an unknown code's string param): s itself, or a mark
// (staleReasonInline).
function staleReason(r, keep) {
  if (!r || !r.code) return t('stale.reason.input_changed');
  const p = r.params || {};
  switch (r.code) {
    case 'glossary_updated':
    case 'glossary_janitor':
      if (p.to == null) return t(`stale.reason.${r.code}_plain`);
      if (p.from == null) return t(`stale.reason.${r.code}_now`, { to: p.to });
      return t(`stale.reason.${r.code}`, { from: p.from, to: p.to });
    case 'material_changed':
      // one slide file, quoted as the lists' names are (R94, R95)
      return p.filename ? t('stale.reason.material_changed', { filename: keep(quoteName(p.filename)) }) : t('stale.reason.material_changed_plain');
    case 'materials_changed': {
      // lecture kinds: the slides a lecture uses (its own, its week's,
      // course-wide) changed; added / removed are file-name lists, each
      // name in the language's quotation marks (round 10 C190: a name may
      // hold the list separator, a capital, quotes of its own)
      const names = (v) => (Array.isArray(v) && v.length ? keep(listJoin(v.map((x) => quoteName(x)))) : '');
      const added = names(p.added);
      const removed = names(p.removed);
      if (added && removed) return t('stale.reason.materials_changed', { added, removed });
      if (added) return t('stale.reason.materials_changed_added', { added });
      if (removed) return t('stale.reason.materials_changed_removed', { removed });
      return t('stale.reason.materials_changed_plain');
    }
    case 'legacy':
      return p.text != null && String(p.text).trim() ? t('stale.reason.legacy', { text: keep(String(p.text)) }) : t('stale.reason.input_changed');
  }
  if (!has(`stale.reason.${r.code}`)) return t('stale.reason.input_changed');
  const params = {};
  for (const [k, v] of Object.entries(p)) params[k] = typeof v === 'string' ? keep(v) : v;
  return t(`stale.reason.${r.code}`, params);
}

/**
 * One stale reason {code, params} (model.StaleCode*), like the back end's
 * state.StaleReasonText: a glossary reason without a recorded "from" shows
 * the current version only; a legacy reason shows its original text. As
 * it reads at the start of a sentence (or alone).
 */
export function staleReasonText(r) {
  return staleReason(r, (s) => s);
}

// Private-use marks (U+E000, U+E001) around the index of a kept text while
// the mid-sentence form is made: {reason:lc} sees the marks and a number,
// no letter.
const KEEP_OPEN = String.fromCharCode(0xe000);
const KEEP_CLOSE = String.fromCharCode(0xe001);
const KEEP_MARK = new RegExp(`${KEEP_OPEN}([0-9]+)${KEEP_CLOSE}`, 'g');

/**
 * One stale reason in mid-sentence (round 10 C190): after the clause
 * separator when several are joined (staleSummaryText), after the label of
 * the update dialog (update_confirm.js). stale.reason_inline: {reason:lc}
 * in Russian lowers the first letter of the reason's own words ("Почему
 * нужно обновить: изменились слайды…"), {reason} elsewhere leaves it as
 * staleReasonText.
 * The texts the reason carries (file names, a recorded text) are kept out
 * of the modifier and come back as written (in one pass: a kept text that
 * holds such marks itself is not read again).
 */
export function staleReasonInline(r) {
  const kept = [];
  const text = staleReason(r, (s) => {
    kept.push(s);
    return `${KEEP_OPEN}${kept.length - 1}${KEEP_CLOSE}`;
  });
  return t('stale.reason_inline', { reason: text }).replace(KEEP_MARK, (m, i) => (Number(i) < kept.length ? kept[Number(i)] : m));
}

/** A stale stage's reason: stale_info, else the transitional stale_reason text. */
export function stageStaleText(st) {
  if (!st) return '';
  if (st.stale_info) return staleReasonText(st.stale_info);
  return st.stale_reason || '';
}

/**
 * Lecture.stale {reasons, stages} as the row text of v1.1 A2.2
 * ("{reasons}, needs updating: {stages}"); "" when nothing is stale. The
 * first reason starts the sentence; the next ones, after the clause
 * separator, are in mid-sentence (staleReasonInline). A reason said twice
 * is said once.
 */
export function staleSummaryText(stale) {
  if (!stale || !(stale.stages || []).length) return '';
  const seen = new Set();
  const reasons = [];
  for (const r of stale.reasons || []) {
    const text = staleReasonText(r);
    if (seen.has(text)) continue;
    seen.add(text);
    reasons.push(reasons.length ? staleReasonInline(r) : text);
  }
  if (!reasons.length) reasons.push(t('stale.reason.input_changed'));
  return t('stale.need_update', { reasons: clauseJoin(reasons), stages: listJoin(stale.stages.map(stageName)) });
}

/** A lecture's stale text: Lecture.stale, else the transitional stale_text. */
export function lectureStaleText(lec) {
  if (!lec) return '';
  if (lec.stale) return staleSummaryText(lec.stale);
  return lec.stale_text || '';
}

export function isAllDone(stages) {
  return Array.isArray(stages) && stages.length > 0 && stages.every((s) => s.status === 'done');
}

// A failed stage is final (no automatic retry left) when the back end does
// not report a pending retry through waiting_for (retry_backoff:{s}).
export function isFinalFailure(st) {
  return st.status === 'failed' && !st.waiting_for;
}

// One-line summary of a pipeline, worst state first.
// tone: ok | run | warn | err | muted. `extra` = {sys, res, staleText, stale,
// draft} (v1.1: the lecture's stale_text and draft flag; v2 P14.3: stale is
// Lecture.stale, used when staleText is not given); older callers pass sys only.
export function lectureSummary(stages, extra) {
  const sum = pipelineSummary(stages, extra);
  // a queued stage the power policy put back in line keeps its checkpoint:
  // "{summary} · 39% done (checkpoint)" (StageView.checkpoint)
  const ck = sum.kind === 'queued' ? checkpointProgress(sum.stage) : null;
  if (ck != null) sum.text = t('common.summary.checkpoint', { summary: sum.text, pct: pct(ck) });
  return sum;
}

function pipelineSummary(stages, extra) {
  const isOpts = extra && ('sys' in extra || 'res' in extra || 'staleText' in extra || 'stale' in extra || 'draft' in extra);
  const opt = isOpts ? extra : { sys: extra };
  if (!Array.isArray(stages) || stages.length === 0) return { text: t('common.summary.none'), tone: 'muted', kind: 'none' };
  if (opt.draft) return { text: t('common.summary.draft'), tone: 'muted', kind: 'draft' };
  const find = (status) => stages.find((s) => s.status === status);
  const wait = (st) => stageWaitingText(st, opt.sys, opt.res); // a retry counts down (3.2.0 L12)
  let st;
  if ((st = find('failed'))) {
    const w = wait(st);
    const stage = stageName(st.stage);
    return { text: w ? t('common.summary.failed_wait', { stage, wait: w }) : t('common.summary.failed', { stage }), tone: 'err', kind: 'failed', stage: st };
  }
  if ((st = find('running'))) return { text: t('common.summary.running', { stage: stageName(st.stage) }), tone: 'run', kind: 'running', stage: st };
  if ((st = stages.find((s) => s.status === 'queued' && s.error && s.waiting_for && s.waiting_for.startsWith(WAIT_RETRY_BACKOFF)))) {
    return { text: t('common.summary.failed_wait', { stage: stageName(st.stage), wait: wait(st) }), tone: 'warn', kind: 'retrying', stage: st };
  }
  if ((st = find('stale'))) {
    const text = opt.staleText || (opt.stale ? staleSummaryText(opt.stale) : '') || t('common.summary.stale', { stage: stageName(st.stage) });
    return { text, tone: 'warn', kind: 'stale', stage: st };
  }
  if ((st = find('cancelled'))) return { text: t('common.summary.cancelled', { stage: stageName(st.stage) }), tone: 'muted', kind: 'cancelled', stage: st };
  if ((st = find('queued'))) {
    const w = wait(st);
    const stage = stageName(st.stage);
    // a power-aware wait names its state itself ("挂起：电源感知 - …"):
    // "{stage}：{wait}", never "{stage}排队中：挂起：…"
    if (w && isPowerWait(st.waiting_for, opt.res)) return { text: t('common.summary.pending', { stage, wait: w }), tone: 'muted', kind: 'queued', stage: st };
    return { text: w ? t('common.summary.queued_wait', { stage, wait: w }) : t('common.summary.queued', { stage }), tone: 'muted', kind: 'queued', stage: st };
  }
  if ((st = find('pending'))) return { text: t('common.summary.pending', { stage: stageName(st.stage), wait: wait(st) || statusName('pending') }), tone: 'muted', kind: 'pending', stage: st };
  return { text: statusName('done'), tone: 'ok', kind: 'done' };
}

// The running stage, if any (its progress is shown next to the bar).
export function runningStage(stages) {
  return (stages || []).find((s) => s.status === 'running') || null;
}

const isBusy = (s) => s.status === 'pending' || s.status === 'queued' || s.status === 'running';

// True while any term extraction job of the course is unfinished (SPEC §9.3
// "extracting…"); the v2 glossary_review is not an extraction (reviewBusy).
export function extractionBusy(course) {
  if (!course) return false;
  if ((course.stages || []).some((s) => s.stage !== 'glossary_review' && isBusy(s))) return true;
  return (course.materials || []).some((m) => (m.stages || []).some(isBusy));
}

// The course's glossary_review stage (v2 G3.1), or null.
export function reviewStage(course) {
  return (course && (course.stages || []).find((s) => s.stage === 'glossary_review')) || null;
}

// True while the glossary janitor waits, is queued or runs.
export function reviewBusy(course) {
  const st = reviewStage(course);
  return !!st && isBusy(st);
}

// What keeps a course's glossary from being ready (glossary_pending, A3.2):
// extracting terms / reviewing the glossary / "" when nothing.
export function glossaryBusyText(course) {
  if (!course) return '';
  if (extractionBusy(course)) return t('common.glossary_busy.extracting');
  if (reviewBusy(course)) return t('common.glossary_busy.reviewing');
  return course.glossary_pending ? t('common.glossary_busy.pending') : '';
}

// The course or material stage a job event refers to, or null.
export function findCourseStage(courses, ev) {
  for (const c of courses || []) {
    if (ev.scope === 'course' && c.id === ev.scope_id) return (c.stages || []).find((s) => s.stage === ev.stage) || null;
    if (ev.scope === 'material') {
      const m = (c.materials || []).find((x) => x.id === ev.scope_id);
      if (m) return (m.stages || []).find((s) => s.stage === ev.stage) || null;
    }
  }
  return null;
}

// ---- formatting (SPEC-v2 P14.4) ----
//
// The one set of formatters of the whole UI (lead ruling at the gate): views
// format numbers, sizes, money, dates and relative times only through these,
// never with i18n.js's fmt* or toLocaleString. Everything follows the UI
// language through Intl (compact notation "1.8M" / "90.1万", byte units,
// currencies, DateTimeFormat, RelativeTimeFormat). Durations take their
// words from the catalog; fmtClock is language-neutral.

// UI languages whose formatting data a browser may lack, with the closest
// locales to use instead, in order. Chrome ships no yue data: Intl formats
// yue-Hant-HK as en-GB ("29 Sept 2026", "1.84m"); zh-Hant-HK is Hong Kong
// Chinese with the same dates and numbers (NOTES p2-tail). A browser that
// has the language's data uses it.
const INTL_FALLBACK = { 'yue-Hant-HK': ['zh-Hant-HK', 'zh-HK', 'zh-TW'] };
const intlResolved = new Map(); // UI language → the Intl locale used

function intlSupports(loc) {
  try {
    return Intl.DateTimeFormat.supportedLocalesOf([loc]).length > 0 && Intl.NumberFormat.supportedLocalesOf([loc]).length > 0;
  } catch {
    return false;
  }
}

/**
 * The Intl locale of the UI language: en-XA formats as English; en dates
 * use en-GB's day-month order, which cannot be misread (P14.1); a language
 * the browser has no data for uses INTL_FALLBACK (yue-Hant-HK → zh-Hant-HK).
 */
export function intlLocale(forDates = false) {
  const loc = getLocale();
  if (loc === 'en' || loc === PSEUDO) return forDates ? 'en-GB' : 'en';
  let r = intlResolved.get(loc);
  if (r === undefined) {
    r = intlSupports(loc) ? loc : (INTL_FALLBACK[loc] || []).find(intlSupports) || loc;
    intlResolved.set(loc, r);
  }
  return r;
}

// Intl formatters are reused: the views format many numbers per render.
const formatters = new Map();

function formatter(kind, locale, opts) {
  const key = `${kind}|${locale}|${JSON.stringify(opts)}`;
  let f = formatters.get(key);
  if (!f) {
    if (kind === 'number') f = new Intl.NumberFormat(locale, opts);
    else if (kind === 'date') f = new Intl.DateTimeFormat(locale, opts);
    else f = new Intl.RelativeTimeFormat(locale, opts);
    formatters.set(key, f);
  }
  return f;
}

function numberFormat(n, opts = {}) {
  return formatter('number', intlLocale(), opts).format(n);
}

function dateFormat(d, opts) {
  return formatter('date', intlLocale(true), opts).format(d);
}

function toDate(d) {
  const date = d instanceof Date ? d : new Date(d);
  return isNaN(date.getTime()) ? null : date;
}

// A whole number with the language's digit grouping: 12,345.
export function fmtInt(n) {
  return numberFormat(Math.round(Number(n) || 0), { maximumFractionDigits: 0 });
}

/**
 * A count for t(): params.n of a plural text is shown with the language's
 * digit grouping (fmtInt) while the plural rules still see the number,
 * e.g. t('lecture.stat.total', { n: count(1234) }) → "共 1,234 句".
 */
export function count(n) {
  const v = Number(n) || 0;
  return { valueOf: () => v, toString: () => fmtInt(v) };
}

// A plain decimal (playback rate 1.25, price multiplier 0.5): at most
// maxDigits fraction digits, with the language's decimal sign.
export function fmtDecimal(n, maxDigits = 2) {
  if (n == null || !isFinite(n)) return '';
  return numberFormat(Number(n), { maximumFractionDigits: maxDigits });
}

// A date with its weekday: "Friday 23 Oct" (en).
export function fmtDayWithWeekday(date = new Date()) {
  const d = toDate(date);
  return d ? dateFormat(d, { weekday: 'long', day: 'numeric', month: 'short' }) : '';
}

// A calendar day without the year: "6 Oct" (en).
export function fmtMonthDay(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { month: 'short', day: 'numeric' }) : '';
}

// A calendar day with its short weekday, and the time of day when withTime,
// in the order of the UI language ("Fri 23 Oct", "Fri 23 Oct, 14:00" in en):
// never glue fmtMonthDay, fmtWeekday and a time together.
export function fmtDayShort(date, withTime = false) {
  const d = toDate(date);
  if (!d) return '';
  const opts = { weekday: 'short', month: 'short', day: 'numeric' };
  // another year (a past lecture recorded in 2024) says which one
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
  if (withTime) Object.assign(opts, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
  return dateFormat(d, opts);
}

// Month and year ("October 2024", zh 2024年10月): when a past lecture was recorded.
export function fmtMonthYear(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { year: 'numeric', month: 'long' }) : '';
}

// The short month name alone ("Oct"): month labels of the calendar.
export function fmtMonth(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { month: 'short' }) : '';
}

// The weekday name; style "long" | "short" | "narrow".
export function fmtWeekday(date, style = 'short') {
  const d = toDate(date);
  return d ? dateFormat(d, { weekday: style }) : '';
}

// The seven weekday names from Monday (weeks run Monday to Sunday, P13.4).
export function weekdayNames(style = 'short') {
  return Array.from({ length: 7 }, (_, i) => fmtWeekday(new Date(2024, 0, 1 + i), style)); // 2024-01-01 is a Monday
}

// "14:05" on a 24-hour clock in the IANA zone tz (the computer's own zone
// when tz is empty): tariff times are shown and compared in this form.
export function fmtHM(date, tz) {
  const d = toDate(date);
  if (!d) return '';
  const opts = { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };
  if (tz) opts.timeZone = tz;
  return dateFormat(d, opts);
}

// "14:05:09" on a 24-hour clock (sampling times of the resource panel).
export function fmtHMS(date) {
  const d = toDate(date);
  return d ? dateFormat(d, { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }) : '';
}

// 51200 -> "00:51", 3723000 -> "1:02:03"
export function fmtClock(ms) {
  const total = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const two = (n) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${two(m)}:${two(s)}` : `${two(m)}:${two(s)}`;
}

export function fmtDuration(ms) {
  if (ms == null || !isFinite(ms)) return '';
  const sec = Math.round(ms / 1000);
  if (sec < 1) return t('common.duration.lt1s');
  if (sec < 60) return t('common.duration.s', { s: sec });
  if (sec < 3600) return t('common.duration.ms', { m: Math.floor(sec / 60), s: sec % 60 });
  return t('common.duration.hm', { h: Math.floor(sec / 3600), m: Math.floor((sec % 3600) / 60) });
}

// A real-time factor (transcription time ÷ audio length): three fraction
// digits below 1 ("0.034"), two from 1 on ("1.6").
export function fmtRTF(rtf) {
  if (rtf == null || !isFinite(rtf)) return '';
  return numberFormat(Number(rtf), { maximumFractionDigits: rtf < 1 ? 3 : 2 });
}

/**
 * The hover-card texts of an asr stage's asr_timing (phase 3b): time is
 * the transcription time with the audio length, speed the RTF with the
 * speed-up ("RTF 0.034 (about 29× real time)" in en), load the model load time (noting a
 * first compile on the device). A part without its numbers is ''.
 */
export function asrTimingText(tm) {
  const out = { time: '', speed: '', load: '' };
  if (!tm) return out;
  const tr = Number(tm.transcribe_ms) || 0;
  const audio = Number(tm.audio_ms) || 0;
  if (tr > 0) out.time = audio > 0 ? t('common.pipe.asr_time', { time: fmtDuration(tr), audio: fmtDuration(audio) }) : fmtDuration(tr);
  if (tr > 0 && audio > 0) {
    const speed = audio / tr;
    out.speed = t('common.pipe.rtf', { rtf: fmtRTF(tr / audio), speed: fmtDecimal(speed, speed >= 10 ? 0 : 1) });
  }
  const load = Number(tm.load_ms) || 0;
  if (tm.first_compile) out.load = t('common.pipe.load_first_compile', { time: fmtDuration(load) });
  else if (load > 0) out.load = fmtDuration(load);
  return out;
}

// Token counts in the compact notation of the UI language with three
// significant digits: 901K, 1.84M (en). Numbers the language does not
// abbreviate (below ten thousand in Chinese) keep all their digits.
export function fmtTokens(n) {
  n = Number(n) || 0;
  const f = formatter('number', intlLocale(), { notation: 'compact', maximumSignificantDigits: 3 });
  const parts = f.formatToParts(n);
  return parts.some((p) => p.type === 'compact') ? parts.map((p) => p.value).join('') : fmtInt(n);
}

// An estimated cost without a currency: 4 decimals under 0.01, else 2.
export function fmtCost(c) {
  if (c == null || !isFinite(c)) return '';
  const digits = c < 0.01 ? 4 : 2;
  return numberFormat(c, { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

// Byte units of the UI language (byte, kB, MB, GB).
function unitFormat(n, unit, digits) {
  return numberFormat(n, { style: 'unit', unit, unitDisplay: 'short', maximumFractionDigits: digits });
}

// A file size in the units their names say (T25 B3): 1 kB = 1000 bytes,
// as CLDR defines the kilobyte that Intl writes (kB, кБ, …); 82,440 bytes
// were 80.5 "kB" counted by 1024. Every language gets its unit names from
// Intl, which has no KiB. Below 1000 the catalog's words (common.size_bytes,
// plural forms): "714 bytes", "1 byte", "714字节". Intl's long name of the
// byte was English in Traditional Chinese ("714 byte", T28 G7); a catalog
// without the key falls back along its chain as any text does. A value that
// rounds to 1000 is written in the next unit ("1 MB", not "1,000 kB").
const BYTE_UNITS = [
  ['kilobyte', 1e3, 1],
  ['megabyte', 1e6, 1],
  ['gigabyte', 1e9, 2],
];

export function fmtBytes(n) {
  n = Number(n) || 0;
  if (Math.round(n) < 1000) return t('common.size_bytes', { n: count(Math.round(n)) });
  for (let i = 0; ; i++) {
    const [unit, size, digits] = BYTE_UNITS[i];
    const v = n / size;
    if (i === BYTE_UNITS.length - 1 || Math.round(v * 10 ** digits) / 10 ** digits < 1000) return unitFormat(v, unit, digits);
  }
}

export function fmtMB(mb) {
  mb = Number(mb) || 0;
  return mb >= 1024 ? unitFormat(mb / 1024, 'gigabyte', 1) : unitFormat(mb, 'megabyte', 0);
}

// A calendar date in the current language, "12 Oct" style; the year only
// when asked (en uses the unambiguous day-month order, P14.1).
function dateText(d, withYear) {
  const opts = { month: 'short', day: 'numeric' };
  if (withYear) opts.year = 'numeric';
  return dateFormat(d, opts);
}

// RFC3339 -> now / 3 minutes ago / 2 hours ago (Intl.RelativeTimeFormat),
// today 09:12 / yesterday 14:20 (the catalog's words around the Intl time),
// else a date (with the year when it is not this year).
export function relTime(iso, now = Date.now()) {
  if (!iso) return '';
  const d = new Date(iso);
  const ms = d.getTime();
  if (isNaN(ms)) return iso;
  const s = Math.round((now - ms) / 1000);
  const rtf = formatter('relative', intlLocale(), { numeric: 'auto' });
  if (s < 45) return rtf.format(0, 'second');
  if (s < 3600) return rtf.format(-Math.max(1, Math.round(s / 60)), 'minute');
  const today = new Date(now);
  today.setHours(0, 0, 0, 0);
  if (ms >= today.getTime() && s < 6 * 3600) return rtf.format(-Math.round(s / 3600), 'hour');
  const time = () => dateFormat(d, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }); // 24-hour, like every time of the UI
  if (ms >= today.getTime()) return t('common.time.today', { time: time() });
  if (ms >= today.getTime() - 86400000) return t('common.time.yesterday', { time: time() });
  return dateText(d, d.getFullYear() !== today.getFullYear());
}

// 2.1415, "CNY" -> Intl's currency format of the provider's currency
// (P14.4 keeps the currency the provider reports; RMB is CNY); an unknown
// code -> "2.14 XYZ"; none -> "2.14".
export function fmtMoney(amount, currency) {
  if (amount == null || !isFinite(amount)) return '';
  const digits = amount > 0 && amount < 0.01 ? 4 : 2;
  const plain = { minimumFractionDigits: digits, maximumFractionDigits: digits };
  if (!currency) return numberFormat(amount, plain);
  const code = currency.toUpperCase() === 'RMB' ? 'CNY' : currency.toUpperCase();
  try {
    return numberFormat(amount, { ...plain, style: 'currency', currency: code });
  } catch {
    return `${numberFormat(amount, plain)} ${currency}`;
  }
}

// A temperature in degrees Celsius ("72 °C", "72°" when narrow: the resource rail).
export function fmtCelsius(c, narrow = false) {
  if (c == null || !isFinite(c)) return '';
  return numberFormat(Number(c), { style: 'unit', unit: 'celsius', unitDisplay: narrow ? 'narrow' : 'short', maximumFractionDigits: 0 });
}

export function fmtPercent(rate, digits = 1) {
  if (rate == null || !isFinite(rate)) return '';
  return numberFormat(rate, { style: 'percent', minimumFractionDigits: digits, maximumFractionDigits: digits });
}

// Course code + lecture label: "CSC1003 L03" (label only without a code).
export function lectureRef(code, label) {
  return code ? `${code} ${label}` : label;
}

// RFC3339 -> local date and time: "23 Oct 2026, 20:00" (en).
export function fmtDateTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return dateFormat(d, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
}

export function cacheHitRate(u) {
  if (!u || !u.prompt_tokens) return null;
  return u.cache_hit_tokens / u.prompt_tokens;
}

// "Input 901k (cache hits 92.1%) · Output 42.9k · Est. 0.12"
export function usageText(u) {
  if (!u) return '';
  const parts = [];
  const rate = cacheHitRate(u);
  const tokens = fmtTokens(u.prompt_tokens);
  parts.push(rate != null ? t('common.usage.input_cached', { tokens, rate: fmtPercent(rate, 1) }) : t('common.usage.input', { tokens }));
  parts.push(t('common.usage.output', { tokens: fmtTokens(u.completion_tokens) }));
  if (u.estimated_cost != null) parts.push(t('common.usage.cost', { cost: fmtCost(u.estimated_cost) }));
  return parts.join(' · ');
}

export function hasUsage(u) {
  return !!u && (u.prompt_tokens > 0 || u.completion_tokens > 0);
}

// First n characters (code points, not UTF-16 units) of an error text.
export function excerpt(text, n = 300) {
  const chars = Array.from(text || '');
  return chars.length <= n ? chars.join('') : chars.slice(0, n).join('') + '…';
}

export function stripExt(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(0, i) : name;
}

export function extOf(name) {
  const i = name.lastIndexOf('.');
  return i > 0 ? name.slice(i + 1).toLowerCase() : '';
}

// Files dropped together whose names differ only by extension (SPEC §9.4.3).
export function sameStemGroups(files) {
  const groups = new Map();
  for (const f of files) {
    const key = stripExt(f.name).toLowerCase();
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(f);
  }
  return [...groups.values()].filter((g) => g.length > 1);
}

// The file extensions a lecture's media (and a second camera angle) may
// have: the server accepts no other (internal/media.Exts; security review
// 2, M3: a playlist such as .m3u8 would make ffmpeg read other files).
// api.js keeps a copy for the system dialog; web/media_exts_test.go checks
// that the three lists are equal.
export const MEDIA_EXT_LIST = [
  'mp4', 'm4v', 'mkv', 'mov', 'webm', 'avi', 'wmv', 'flv', 'ts', 'mts', 'm2ts', 'mpg', 'mpeg', '3gp', 'ogv',
  'mp3', 'm4a', 'aac', 'wav', 'flac', 'ogg', 'oga', 'opus', 'mka', 'wma', 'amr', 'aiff', 'aif',
];
const MEDIA_EXTS = new Set(MEDIA_EXT_LIST);

// <input accept> of media pickers: the extensions only. "audio/*" would
// also offer playlists (Windows types .m3u and .m3u8 as audio/x-mpegurl).
export const MEDIA_ACCEPT = MEDIA_EXT_LIST.map((e) => '.' + e).join(',');

// By extension only, for the same reason (a playlist can have an audio/* type).
export function isMediaFile(file) {
  return MEDIA_EXTS.has(extOf((file && file.name) || ''));
}

export function isSyllabusFile(file) {
  return ['txt', 'md', 'pdf'].includes(extOf(file.name));
}

export function isPDF(file) {
  return extOf(file.name) === 'pdf';
}

// ---- word diff (SPEC §9.6, §9.7) ----

export function tokenize(s) {
  return (s || '').split(/\s+/).filter(Boolean);
}

const DIFF_CELL_LIMIT = 250000;

// Word-level diff of a -> b. Returns runs [{t: 'eq'|'del'|'ins', w: 'words'}],
// consecutive tokens of the same kind merged. Within a changed region the
// deletions come before the insertions.
export function wordDiff(a, b) {
  const A = tokenize(a);
  const B = tokenize(b);
  const n = A.length;
  const m = B.length;
  const ops = [];
  const push = (t, w) => {
    const last = ops[ops.length - 1];
    if (last && last.t === t) last.w += ' ' + w;
    else ops.push({ t, w });
  };
  if ((n + 1) * (m + 1) > DIFF_CELL_LIMIT) {
    if (n) push('del', A.join(' '));
    if (m) push('ins', B.join(' '));
    return ops;
  }
  // L[i*(m+1)+j] = length of the LCS of A[i:] and B[j:]
  const W = m + 1;
  const L = new Int32Array((n + 1) * W);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      L[i * W + j] = A[i] === B[j] ? L[(i + 1) * W + j + 1] + 1 : Math.max(L[(i + 1) * W + j], L[i * W + j + 1]);
    }
  }
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (A[i] === B[j]) {
      push('eq', A[i]);
      i++;
      j++;
    } else if (L[(i + 1) * W + j] >= L[i * W + j + 1]) {
      push('del', A[i]);
      i++;
    } else {
      push('ins', B[j]);
      j++;
    }
  }
  while (i < n) push('del', A[i++]);
  while (j < m) push('ins', B[j++]);
  return ops;
}
