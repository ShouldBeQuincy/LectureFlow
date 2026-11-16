// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/settings (v1.1 A5.3): providers (add, edit, test connection, probe
// models, delete), role assignment, transcription, storage and other. Every
// block saves on its own with an explicit button; what is shown as saved is
// always the last Settings the back end returned (GET /api/settings or the
// response of a save). Unsaved edits are kept per block — also across a
// background refresh (SSE changed settings) and across leaving the page —
// until they are saved or undone.
//
// v2 (SPEC-v2 P12, P13.6, appendix D1): the tariff of a provider (time-of-day
// pricing, per-model prices, "send requests only in off-peak hours";
// tariff_ui.js), the public holidays (view and edit), the optional roles
// chapters and organizer, and the lecture-title language of this browser.
// v2 P14: every UI text comes from t(); role names from role.{id}. The
// interface language (P14.7, language.js) sits with the title language in
// the "language and display" block; P14.6 (traditional-Chinese display) is
// hidden (appendix D4, see language.js).
// lecture kinds: the "lecture types" block — the built-in kinds and the user's
// own (add, rename, delete; GET/POST/PATCH/DELETE /api/lecture-kinds).
// SPEC-v2 I6-I9 (phase 3b): the Transcription block has the global settings
// (language, transcription device, several devices at once, battery
// awareness, reserved GPU memory and system memory; saved with the block's
// button) and the device list of asr_devices.js (every change saved at once).
// Power management: the transcription device is "automatic (power-aware)" or
// "choose devices yourself" (ticked devices, never a CPU device; ticking a
// second one on a laptop warns first; not used on battery); battery
// awareness is retired and its switch hidden (the value is sent back as it is).
// ext-step1: the Transcription block starts with "转写方式" (local / only Colab,
// settings_asrmode.js; saved at once through PUT /api/asr-mode).

import { api } from './api.js';
import { h, clear, btn, iconBtn, skeleton, errorBlock, preserveFocus, openDialog, confirmDialog, toast } from './dom.js';
import { icon } from './icons.js';
import { bindContextMenu, menuButton } from './menu.js';
import * as U from './util.js';
import { t, has, getLocale, PSEUDO } from './i18n.js';
import { tariffEditor, tariffSummary, tariffTag, hasPeaks, dateRanges, rangeText, openHolidaysEditor } from './tariff_ui.js';
import { titleLang, setTitleLang, TITLE_LANGS } from './calendar_ui.js';
import { localeChoices, chooseLocale, chooseZhDisplay, langAttr, hasCatalog, fallbackName } from './language.js';
import { ZH_MODES, zh, zhLang, zhSetting, zhWanted, zhFailed } from './zh_display.js';
import { BUILTIN_KINDS, kindList, kindName, loadKinds } from './kinds.js';
import { createDeviceList } from './asr_devices.js';
import { createAppSection } from './settings_app.js';
import { createAboutSection } from './settings_about.js';
import { createComponentsSection } from './settings_components.js';
import { createDownloadsSection } from './settings_downloads.js';
import { createColabSection } from './settings_colab.js';
import { createRemoteSection, sshEnabled, onServersChange } from './settings_remote.js';
import { createASRModeRow } from './settings_asrmode.js';
import {
  MAX_CONCURRENCY,
  MAX_RPM,
  DEEPSEEK_LIMITS,
  hasOldLimits,
  sameOrigin,
  endpointParts,
  limitsPrefill,
  ANTHROPIC_RATE_LIMITS_URL,
  MAX_TOKENS_LIMIT,
  MAX_BATCH,
  DEFAULT_IDLE_TIMEOUT_S,
  MIN_IDLE_TIMEOUT_S,
  MAX_IDLE_TIMEOUT_S,
  thinkingFloorsAtLow,
  defaultBatch,
} from './provider_rules.js';
import { openExternal, obApi, setErrorHandler as setObErrorHandler, errorHandler as obErrorHandler } from './onboarding_api.js';
import { providersStep } from './onboarding_providers.js';
import { createLimitsHint, limitsBanner } from './provider_limits.js';
import { serverKind, parseMaxTokens } from './retry_model.js';

const typeName = (type) => (has(`settings.provider.type.${type}`) ? t(`settings.provider.type.${type}`) : type);
const roleHint = (role) => (has(`settings.role_hint.${role}`) ? t(`settings.role_hint.${role}`) : '');
// Roles that may stay unconfigured (v2 G2: without a janitor the glossary
// is tidied by the common-word rule; v2 P7/P13: without chapters or
// organizer those optional features are not offered). Not configuring them
// is not an error.
const OPTIONAL_ROLES = new Set(['glossary_janitor', 'chapters', 'organizer']);
// Turning thinking on for a role whose requests go sentence by sentence
// (translate, repair) asks first, with cancel as the default
// (.claude/thinking-brief.md 3); null for the other roles.
function thinkingConfirm(role) {
  if (role === 'translate') return { title: t('settings.roles.thinking_confirm.translate_title'), message: t('settings.roles.thinking_confirm.translate_message') };
  if (role === 'repair') return { title: t('settings.roles.thinking_confirm.repair_title'), message: t('settings.roles.thinking_confirm.repair_message') };
  return null;
}
function thinkingName(level) {
  if (level === 'low') return t('settings.roles.thinking.low');
  if (level === 'high') return t('settings.roles.thinking.high');
  if (level === 'max') return t('settings.roles.thinking.max');
  return t('settings.roles.thinking.off');
}
const optionalNote = (role) => t(`settings.role_unset.${role}`);
// “只转写” (3.1.0): none of the launch roles has a provider. Lectures still
// start (transcription, playback, English subtitles); summary, correction
// and translation wait until a provider is set up, so this is not an error.
const LAUNCH_ROLES = ['summary', 'repair', 'translate'];
const transcribeOnly = (roles) => LAUNCH_ROLES.every((id) => roles.some((r) => r.role === id && !r.configured));
// a role that is not usable and needs the user (optional roles never do,
// nor any role while nothing is set up: transcription only)
const roleBroken = (r, _i, roles) => !r.configured && !OPTIONAL_ROLES.has(r.role) && !(roles && transcribeOnly(roles));
// The role's name: role.{id}, else the back end's label (a role the front
// end does not know yet).
const roleNameOf = (r) => (has(`role.${r.role}`) ? U.roleName(r.role) : r.label);
const quote = (name) => t('settings.quote', { name });
// Language codes offered for the lecture audio; their names come from
// Intl.DisplayNames in the current UI language.
const LANGS = ['en', 'zh', 'ja', 'ko', 'fr', 'de', 'es', 'ru'];
function langName(code) {
  if (!LANGS.includes(code)) return '';
  try {
    return new Intl.DisplayNames([U.intlLocale()], { type: 'language' }).of(code) || '';
  } catch {
    return '';
  }
}
const CURRENCIES = ['CNY', 'USD', 'EUR', 'GBP', 'JPY'];
// "Use for the roles…": confirmations in a row while the settings keep
// changing elsewhere (R81 N1)
const MAX_ROUNDS = 5;
const MANUAL = '__manual__';
const AUTO = 'auto'; // ASRSettings.device_mode "自动（电源感知）" (model.ASRDeviceModeAuto)
const MANUAL_MODE = 'manual'; // "手动指定" with manual_devices (model.ASRDeviceModeManual)
// [name, icon, i18n key of the title]
const BLOCKS = [
  ['providers', 'server', 'settings.block.providers'],
  ['roles', 'bot', 'settings.block.roles'],
  ['asr', 'mic', 'settings.block.asr'],
  // SPEC-v3 V14.2, V14.4 (settings_colab.js): own data and loading
  ['colab', 'zap', 'settings.colab.title'],
  // ssh-1b (settings_remote.js): the SSH inference servers; own data, and
  // only while the back end has them switched on (sshEnabled)
  ['remote', 'plug', 'settings.remote.title'],
  ['general', 'sliders-horizontal', 'settings.block.general'],
  ['kinds', 'tag', 'settings.kinds.title'],
  ['holidays', 'calendar', 'settings.holidays.title'],
  ['display', 'languages', 'i18n.block_title'],
  // SPEC-ext (settings_downloads.js): the browser extension's recording downloads; own data
  ['downloads', 'download', 'settings.downloads.title'],
  // SPEC-v3 V11 (settings_components.js, settings_app.js, settings_about.js): own data and loading
  ['components', 'layers', 'settings.comp.title'],
  ['app', 'settings', 'settings.app.title'],
  ['about', 'info', 'settings.about.title'],
];
// Blocks that load their own data (not GET /api/settings).
const SELF_LOADED = new Set(['components', 'app', 'about', 'colab', 'remote', 'downloads']);
// Blocks shown only when their back end has them (ssh-1b: the SSH servers).
const shownBlock = (b) => b[0] !== 'remote' || sshEnabled();
const blockTitle = (b) => t(b[2]);

// Kept for the whole page session: unsaved edits, the last connection test
// of each provider and the probes in flight.
const drafts = { roles: {}, asr: null, general: null };
const tests = new Map(); // provider id → {state: running|ok|fail, latency_ms, model, error, at}
const probing = new Set(); // provider ids

const num = (v) => {
  const s = String(v == null ? '' : v).trim();
  return s === '' ? NaN : Number(s);
};
const isInt = (n) => Number.isInteger(n);

// Local servers may run without a key (settings.IsLocal). The same reading
// as the retry dialog's (retry_model.js serverKind, C3): 127.0.0.0/8 mapped
// into IPv6 ([::ffff:127.0.0.1]) is local too; an address the browser and
// the back end may read differently is not taken for local.
function isLocal(baseURL) {
  return serverKind(baseURL) === 'local';
}

function hostOf(baseURL) {
  try {
    return new URL(baseURL).hostname.toLowerCase();
  } catch {
    return '';
  }
}

// AUD1 I-3: a provider's address on plain http to a computer off the local
// network: its API key and the lecture content cross the network
// unencrypted. A note only (the back end takes any http(s) address, and a
// gateway on the local network commonly runs on plain http). No note for
// this computer (isLocal, or a loopback address as the browser reads it),
// a private, link-local, unique-local or carrier-grade NAT address (an IPv6
// address carrying an IPv4 one as that address), or a name only a local
// network resolves (one label, .local, .home.arpa, .internal).
function plainHTTPFar(baseURL) {
  let u;
  try {
    u = new URL(String(baseURL == null ? '' : baseURL).trim());
  } catch {
    return false;
  }
  if (u.protocol !== 'http:' || !u.hostname || isLocal(baseURL)) return false;
  let host = u.hostname.toLowerCase().replace(/\.$/, '');
  if (host.startsWith('[')) {
    const ip6 = host.slice(1, -1);
    const v4in6 = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(ip6);
    // ::1, ::, fc00::/7, fe80::/10, fec0::/10
    if (!v4in6) return !(ip6 === '::1' || ip6 === '::' || /^f[cd][0-9a-f]{2}:/.test(ip6) || /^fe[89a-f][0-9a-f]:/.test(ip6));
    const hi = parseInt(v4in6[1], 16);
    const lo = parseInt(v4in6[2], 16);
    host = [hi >> 8, hi & 255, lo >> 8, lo & 255].join('.');
  }
  const v4 = /^(\d{1,3})\.(\d{1,3})\.\d{1,3}\.\d{1,3}$/.exec(host);
  if (v4) {
    const a = Number(v4[1]);
    const b = Number(v4[2]);
    return !(a === 0 || a === 10 || a === 127 || (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168));
  }
  return host.includes('.') && !/\.(local|home\.arpa|internal)$/.test(host) && host !== 'localhost' && !host.endsWith('.localhost');
}

function needsKey(p) {
  // cred-enc: a saved key this computer cannot decrypt needs entering again
  return p.key_unreadable || (!p.key_hint && !isLocal(p.base_url));
}

function reducedMotion() {
  return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function mountSettings(root, ctx, route) {
  let alive = true;
  let data = null; // last Settings from the back end
  let dataKey = '';
  let error = '';
  let loadTimer = null;
  let current = 'providers'; // TOC highlight
  const saving = new Set(); // blocks being saved
  const blockErrors = {}; // block → back-end error of the last failed save
  const rowErrors = {}; // role → error of the last failed save / validation
  // v3.2.x: the field a role's error is about (max_tokens, batch), marked
  // aria-invalid next to the message; the general block's idle-timeout error
  // shown under its field
  const rowErrorFields = {};
  const fieldErrors = {};
  const batchOpen = new Set(); // roles whose folded batch field is open (kept across re-renders)
  const manualRoles = new Set(); // roles whose model is typed by hand
  // the device list (asr_devices.js); its requests return device views that
  // replace data.devices
  const devUI = createDeviceList({
    settings: () => data,
    resources: () => ctx.resources(),
    onDevices(list) {
      if (!data) return;
      data = { ...data, devices: list };
      dataKey = JSON.stringify(data);
      devKey = devicesKey(data);
      restKey = othersKey(data);
      renderASRGlobal();
    },
    reload: () => loadSoon(),
  });
  // ext-step1: "转写方式", the first row of the Transcription block
  const modeUI = createASRModeRow({ formRow, segmented, rerender: () => renderASRGlobal() });
  let devKey = '';
  let restKey = '';
  let allPausedShown = !!(ctx.resources() && ctx.resources().gpu && ctx.resources().gpu.paused);
  let batteryShown = U.batteryMachine(ctx.resources());
  let pendingJump = route && route.section ? route : null;
  let presetOpen = false; // "add from a preset" is open
  const foots = {};
  let balanceKey = '';

  const el = h('div', { class: 'page page-settings' });
  const alerts = h('div', { class: 'set-alerts' });
  const toc = h('nav', { class: 'set-toc', 'aria-label': t('settings.toc_label') });
  const shells = {};
  for (const b of BLOCKS) {
    shells[b[0]] = h('section', { class: 'card set-card', id: `set-${b[0]}`, 'aria-label': blockTitle(b) });
  }
  el.append(
    h(
      'header',
      { class: 'page-head' },
      h('div', { class: 'page-titles' }, h('h1', { class: 'page-title' }, t('settings.title')), h('p', { class: 'page-sub' }, t('settings.subtitle'))),
    ),
    alerts,
    h('div', { class: 'set-layout' }, toc, h('div', { class: 'set-main' }, BLOCKS.map(([name]) => shells[name]))),
  );
  root.append(el);

  // SPEC-v3 V11: the App and About sections render from their own requests
  const sectionUI = (name) => ({ head, formRow, switchBtn, segmented, rerender: () => renderBlock(name) });
  const appUI = createAppSection(sectionUI('app'));
  const aboutUI = createAboutSection(sectionUI('about'));
  const compUI = createComponentsSection(sectionUI('components'));
  const colabUI = createColabSection(sectionUI('colab'));
  const remoteUI = createRemoteSection(sectionUI('remote'));
  const dlUI = createDownloadsSection(sectionUI('downloads'));
  const selfUI = { components: compUI, app: appUI, about: aboutUI, colab: colabUI, remote: remoteUI, downloads: dlUI };
  // ssh-1b: the section (and its TOC entry) appears once the back end
  // answered with a server list; a jump to #/settings/remote waits for it
  const remoteUnsub = onServersChange(() => {
    renderBlock('remote');
    if (data) devUI.render(); // an SSH server's row shows its state
    jumpPending();
  });

  // ---- data ----

  // → whether the settings were read
  async function load() {
    try {
      const s = await api.settings();
      if (!alive) return false;
      error = '';
      setData(s);
      return true;
    } catch (e) {
      if (!alive) return false;
      if (!data) {
        error = e.message || t('common.load_failed');
        render();
      }
      return false;
    }
  }

  function loadSoon() {
    clearTimeout(loadTimer);
    loadTimer = setTimeout(load, 150);
  }

  // Re-renders only when the settings actually changed (a refresh must not
  // replace the control under the user's pointer or caret for nothing).
  // SPEC-v2 I9: while a device test runs only its panel changes (the
  // device list updates it in place); other device changes re-render the
  // Transcription block only.
  function setData(s) {
    if (!s) return;
    const key = JSON.stringify(s);
    data = s;
    pruneDrafts();
    if (key === dataKey) return;
    const prevDev = devKey;
    const prevRest = restKey;
    dataKey = key;
    devKey = devicesKey(s);
    restKey = othersKey(s);
    if (prevRest && prevRest === restKey) {
      if (prevDev === devKey) devUI.syncTests(s.devices || []);
      else renderBlock('asr');
    } else render();
    jumpPending();
  }

  // the device views without what changes while a test or a transcription runs
  function devicesKey(s) {
    return JSON.stringify((s.devices || []).map((d) => ({ ...d, selftest: d.selftest && d.selftest.state, running: (d.running || []).map((r) => r.job_id), slots_used: undefined })));
  }
  function othersKey(s) {
    return JSON.stringify({ ...s, devices: undefined });
  }

  // #/settings/asr/{device}: the block, then the device's test button (the
  // workbench prompt "test now", SPEC-v2 I9). Round 8 F4:
  // #/settings/providers/preset opens "add from a preset" (the focus on the
  // section's own entry once it is closed), #/settings/providers/{id} goes
  // to that provider's row. Each address is handled once: the page drawn
  // again does not repeat it.
  function jumpPending() {
    if (!pendingJump || !data) return;
    const r = pendingJump;
    pendingJump = null;
    if (!shells[r.section]) return;
    if (r.section === 'remote' && !sshEnabled()) {
      pendingJump = r; // the server list has not come yet
      return;
    }
    if (r.section === 'providers' && r.device) {
      jumpProvider(r.device);
      return;
    }
    jump(r.section);
    if (r.device) {
      const b = shells[r.section].querySelector(`[data-fkey="${CSS.escape(`set:dev:${r.device}:test`)}"]`);
      if (b) {
        b.focus({ preventScroll: true });
        const row = b.closest('.dev-row');
        if (row) row.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'center' });
      }
    }
  }

  // F4: "preset", or a provider's id (one that is not there: the section's top)
  function jumpProvider(dev) {
    const shell = shells.providers;
    if (dev === 'preset') {
      jump('providers');
      // the dialog gives the focus back to what had it: the section's entry
      const entry = shell.querySelector('[data-fkey="set:prov:preset"]');
      if (entry) entry.focus({ preventScroll: true });
      presetDialog();
      return;
    }
    const edit = shell.querySelector(`[data-fkey="${CSS.escape(`set:prov:${dev}:edit`)}"]`);
    const row = edit && edit.closest('.pv-row');
    if (!row) {
      jump('providers');
      return;
    }
    current = 'providers';
    renderToc();
    row.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'center' });
    edit.focus({ preventScroll: true });
    // a brief highlight (style.css .pv-row.flash; none where it is not styled)
    row.classList.remove('flash');
    row.classList.add('flash');
  }

  function retry() {
    error = '';
    render();
    load();
  }

  const providerOf = (id) => (data && id ? data.providers.find((p) => p.id === id) : null) || null;
  const roleLabel = (role) => {
    const r = data && data.roles.find((x) => x.role === role);
    return r ? roleNameOf(r) : U.roleName(role);
  };

  // ---- drafts ----

  // The device mode of settings as model.NormalizeASRDeviceMode reads it:
  // auto, or manual with its devices (no CPU device; a 3b single device id
  // is manual [id]); the list in the device list's priority order.
  function deviceModeOf(a) {
    const mode = (a.device_mode || '').trim();
    let ids = mode === MANUAL_MODE ? a.manual_devices || [] : mode && mode !== AUTO ? [mode] : [];
    const devs = (data && data.devices) || [];
    const kind = (id) => (devs.find((d) => d.id === id) || {}).kind || (id.endsWith(':cpu') || id.endsWith(':CPU') ? 'cpu' : '');
    ids = [...new Set(ids.map((x) => String(x).trim()).filter((x) => x && kind(x) !== 'cpu'))];
    return ids.length ? { device_mode: MANUAL_MODE, manual_devices: sortByPriority(ids) } : { device_mode: AUTO, manual_devices: [] };
  }
  function sortByPriority(ids) {
    const devs = (data && data.devices) || [];
    const rank = (id) => {
      const d = devs.find((x) => x.id === id);
      return d ? -d.priority : Infinity;
    };
    return [...ids].sort((x, y) => rank(x) - rank(y) || (x < y ? -1 : x > y ? 1 : 0));
  }

  function serverRole(r) {
    return {
      provider_id: r.provider_id || '',
      model: r.model || '',
      temperature: String(r.temperature),
      window: r.window != null ? String(r.window) : '',
      thinking: r.thinking || 'off',
      // v3.2.x: "" = automatic (max_tokens) / the provider's and model's
      // default (batch, translate only: defaultBatch, 10 for Claude on Anthropic, else 1)
      max_tokens: r.max_tokens != null ? String(r.max_tokens) : '',
      batch: r.batch != null ? String(r.batch) : '',
    };
  }
  function roleVal(r) {
    return drafts.roles[r.role] || serverRole(r);
  }
  // An optional number field: both empty, or the same number.
  function sameOpt(a, b) {
    const x = String(a == null ? '' : a).trim();
    const y = String(b == null ? '' : b).trim();
    return x === y || (x !== '' && y !== '' && Number(x) === Number(y));
  }
  function sameRole(a, b) {
    return (
      a.provider_id === b.provider_id &&
      a.model.trim() === b.model.trim() &&
      num(a.temperature) === num(b.temperature) &&
      num(a.window) === num(b.window) &&
      thinkingOf(a) === thinkingOf(b) &&
      sameOpt(a.max_tokens, b.max_tokens) &&
      sameOpt(a.batch, b.batch)
    );
  }
  // The thinking level a role value means: off on a provider without
  // thinking (settings.SupportsThinking; the select is disabled there).
  function thinkingOf(v) {
    const p = providerOf(v.provider_id);
    return p && p.supports_thinking ? v.thinking || 'off' : 'off';
  }
  function roleDirty(r) {
    const d = drafts.roles[r.role];
    return !!d && !sameRole(d, serverRole(r));
  }

  // The global transcription settings of the block (SPEC-v2 I8); model,
  // compute_type, device and max_parallel are per device now (the legacy
  // fields are sent back unchanged).
  function serverASR() {
    const a = data.asr;
    return {
      language: a.language,
      ...deviceModeOf(a),
      multi_device: !!a.multi_device,
      battery_aware: !!a.battery_aware,
      // "修复退化的转写窗口" (kv asr.redecode; a missing value means on)
      redecode: data.asr_redecode !== false,
      vram_reserve_mb: String(a.vram_reserve_mb),
      mem_reserve_mb: String(a.mem_reserve_mb == null ? '' : a.mem_reserve_mb),
    };
  }
  function asrVal() {
    return drafts.asr || serverASR();
  }
  function sameASR(a, b) {
    return (
      a.language.trim() === b.language.trim() &&
      a.device_mode === b.device_mode &&
      (a.device_mode !== MANUAL_MODE || a.manual_devices.join(' ') === b.manual_devices.join(' ')) &&
      a.multi_device === b.multi_device &&
      a.battery_aware === b.battery_aware &&
      a.redecode === b.redecode &&
      num(a.vram_reserve_mb) === num(b.vram_reserve_mb) &&
      num(a.mem_reserve_mb) === num(b.mem_reserve_mb)
    );
  }

  function serverGeneral() {
    const g = data.general;
    return {
      keep_original_media: g.keep_original_media,
      notifications: g.notifications,
      daily_budget: String(g.daily_budget),
      // v3.2.x: absent = on; absent idle timeout = the default ("" here)
      auto_chapters: g.auto_chapters !== false,
      llm_idle_timeout_s: g.llm_idle_timeout_s != null ? String(g.llm_idle_timeout_s) : '',
    };
  }
  function generalVal() {
    return drafts.general || serverGeneral();
  }
  function sameGeneral(a, b) {
    return (
      a.keep_original_media === b.keep_original_media &&
      a.notifications === b.notifications &&
      num(a.daily_budget) === num(b.daily_budget) &&
      a.auto_chapters === b.auto_chapters &&
      sameOpt(a.llm_idle_timeout_s, b.llm_idle_timeout_s)
    );
  }

  function dirty(block) {
    if (!data) return false;
    if (block === 'roles') return data.roles.some(roleDirty);
    if (block === 'asr') return !!drafts.asr && !sameASR(drafts.asr, serverASR());
    if (block === 'general') return !!drafts.general && !sameGeneral(drafts.general, serverGeneral());
    return false;
  }
  const anyDirty = () => ['roles', 'asr', 'general'].some(dirty);

  // Drafts equal to the saved settings are dropped (e.g. after someone else
  // saved the same values).
  function pruneDrafts() {
    for (const r of data.roles) if (drafts.roles[r.role] && !roleDirty(r)) delete drafts.roles[r.role];
    for (const role of Object.keys(drafts.roles)) if (!data.roles.some((r) => r.role === role)) delete drafts.roles[role];
    if (drafts.asr && sameASR(drafts.asr, serverASR())) drafts.asr = null;
    if (drafts.general && sameGeneral(drafts.general, serverGeneral())) drafts.general = null;
  }

  function setRoleDraft(r, patch, rerender = false) {
    const next = { ...roleVal(r), ...patch };
    if (sameRole(next, serverRole(r))) delete drafts.roles[r.role];
    else drafts.roles[r.role] = next;
    delete rowErrors[r.role];
    delete rowErrorFields[r.role];
    delete blockErrors.roles;
    if (rerender) {
      renderBlock('roles');
      return;
    }
    const row = shells.roles.querySelector(`.rl-row[data-role="${r.role}"]`);
    if (row) row.classList.toggle('is-dirty', roleDirty(r));
    if (row && r.role === 'translate') syncBatch(row, r);
    if (row) syncMaxTokens(row, r);
    if (row) syncThinkFloor(row, r);
    syncFoot('roles');
  }
  // v3.2.x: claude-opus-5-5 (and the Fable and Mythos families, W4c) cannot
  // switch thinking off; "off" runs at the lowest level (effort low). The
  // short note in the thinking cell, the full one under the row, which
  // describes the thinking select.
  // Only an Anthropic provider runs the model so (C3, R42: internal/llm's
  // anthropicAlwaysThinks is the Anthropic client's); an OpenAI-compatible
  // one sends "off" as it is.
  function floorNoteOf(v) {
    const thinkVal = thinkingOf(v);
    const p = providerOf(v.provider_id);
    return !!p && p.type === 'anthropic' && thinkVal === 'off' && thinkingFloorsAtLow(v.model) ? t('settings.roles.thinking_floor_note', { model: v.model.trim() }) : '';
  }
  const floorId = (r) => `rl-${r.role}-think_floor-hint`;
  const floorShort = (r, note) => h('span', { class: 'rl-sub rl-think-floor', title: note, dataset: { fkey: `set:role:${r.role}:think_floor` } }, icon('info', { size: 12 }), ' ', t('settings.roles.thinking_floor_short'));
  const floorMsg = (r, note) => h('p', { class: 'rl-msg rl-floor-msg', id: floorId(r) }, icon('info', { size: 13 }), note);
  // A model picked from the list or typed, or a thinking level picked, does
  // not re-render the row: the notes follow in place (T11 general 4).
  function syncThinkFloor(row, r) {
    const note = floorNoteOf(roleVal(r));
    const short = row.querySelector('.rl-think-floor');
    const msg = row.querySelector('.rl-floor-msg');
    if ((short ? short.getAttribute('title') : '') === note && !!msg === !!note) return;
    if (short) short.remove();
    if (msg) {
      const box = msg.parentNode;
      msg.remove();
      if (box && !box.children.length) box.remove();
    }
    const sel = row.querySelector(`[data-fkey="set:role:${r.role}:thinking"]`);
    if (sel) {
      if (note) sel.setAttribute('aria-describedby', floorId(r));
      else sel.removeAttribute('aria-describedby');
    }
    if (!note) return;
    const cell = row.querySelector('.rl-think');
    if (cell) cell.append(floorShort(r, note));
    let box = row.querySelector('.rl-notes');
    if (!box) {
      box = h('div', { class: 'rl-notes' });
      row.append(box);
    }
    box.append(floorMsg(r, note));
  }
  // The translate row's batch default (defaultBatch: the role's provider and
  // current model) and the chips of its folded summary. Typing a model name
  // or a batch does not re-render the row, so syncBatch updates them in place.
  function batchView(v) {
    const defBatch = defaultBatch(providerOf(v.provider_id), v.model);
    let setBatch = String(v.batch == null ? '' : v.batch).trim();
    // round 2 (T6 #9): a value outside 1–MAX_BATCH or not a whole number
    // gets no chip at all ("Set to 25" read as if it were saved); the
    // validation error says what is wrong
    const bad = setBatch !== '' && (!isInt(num(setBatch)) || num(setBatch) < 1 || num(setBatch) > MAX_BATCH);
    // a valid value as the number it is and is saved as ("1.0", "01": 1;
    // C3, R28 A6: the chip said "Set to 1.0")
    if (setBatch !== '' && !bad) setBatch = String(num(setBatch));
    return {
      defBatch,
      setBatch,
      bad,
      // empty: the default, as a chip when it is above 1
      showDefault: setBatch === '' && defBatch > 1,
      // a valid value: as set; 1 only where the default is not 1
      showValue: !bad && setBatch !== '' && (setBatch !== '1' || defBatch !== 1),
    };
  }
  function syncBatch(row, r) {
    const b = batchView(roleVal(r));
    const q = (x) => row.querySelector(`[data-fkey="set:role:${r.role}:${x}"]`);
    const inp = q('batch');
    const def = q('batch_default');
    const dchip = q('batch_default_chip');
    const vchip = q('batch_value_chip');
    if (inp) inp.placeholder = String(b.defBatch);
    if (def) def.textContent = t('settings.roles.batch_default', { n: b.defBatch });
    if (dchip) {
      dchip.textContent = t('settings.roles.batch_default', { n: b.defBatch });
      dchip.hidden = !b.showDefault;
    }
    if (vchip) {
      vchip.textContent = t('settings.roles.batch_value_chip', { value: b.setBatch });
      vchip.hidden = !b.showValue;
    }
    // round 2 (T6 #9): the error of the last save stays while the value is
    // still invalid and goes as soon as it is valid (empty included),
    // without a re-render (the field keeps the focus and the caret)
    if (inp && inp.getAttribute('aria-invalid') === 'true') {
      if (b.bad) {
        rowErrors[r.role] = t('errors.settings.batch_invalid', { min: 1, max: MAX_BATCH });
        rowErrorFields[r.role] = 'batch';
      } else {
        inp.removeAttribute('aria-invalid');
        dropErrRef(inp, r.role);
        const msg = row.querySelector('.rl-msg.is-err[role="alert"]');
        if (msg) msg.remove();
        row.classList.remove('is-error');
      }
    }
  }
  // C3 (R28 A5): the output limit's error, as the batch's (syncBatch): it
  // stays while the value is still invalid and goes as soon as it is valid
  // (empty included), without a re-render
  function syncMaxTokens(row, r) {
    const inp = row.querySelector(`[data-fkey="set:role:${r.role}:max_tokens"]`);
    if (!inp || inp.getAttribute('aria-invalid') !== 'true') return;
    if (maxTokensBad(roleVal(r), r)) {
      rowErrors[r.role] = t('errors.settings.max_tokens_invalid', { min: 1, max: MAX_TOKENS_LIMIT });
      rowErrorFields[r.role] = 'max_tokens';
      return;
    }
    inp.removeAttribute('aria-invalid');
    dropErrRef(inp, r.role);
    const msg = row.querySelector('.rl-msg.is-err[role="alert"]');
    if (msg) msg.remove();
    row.classList.remove('is-error');
  }
  // T18: a role row's error line (its id), which describes the field it is
  // about (aria-describedby) until the value is valid again
  function roleErrId(role) {
    return `rl-${role}-err`;
  }
  function dropErrRef(inp, role) {
    const ids = (inp.getAttribute('aria-describedby') || '').split(' ').filter((x) => x && x !== roleErrId(role));
    if (ids.length) inp.setAttribute('aria-describedby', ids.join(' '));
    else inp.removeAttribute('aria-describedby');
  }
  function setASRDraft(patch, rerender = false) {
    const next = { ...asrVal(), ...patch };
    drafts.asr = sameASR(next, serverASR()) ? null : next;
    delete blockErrors.asr;
    if (rerender) renderBlock('asr');
    else syncFoot('asr');
  }
  function setGeneralDraft(patch, rerender = false) {
    const next = { ...generalVal(), ...patch };
    drafts.general = sameGeneral(next, serverGeneral()) ? null : next;
    delete blockErrors.general;
    if ('llm_idle_timeout_s' in patch && fieldErrors.idle) {
      delete fieldErrors.idle;
      rerender = true;
    }
    if (rerender) renderBlock('general');
    else syncFoot('general');
  }

  function undo(block) {
    if (block === 'roles') {
      drafts.roles = {};
      for (const k of Object.keys(rowErrors)) delete rowErrors[k];
      for (const k of Object.keys(rowErrorFields)) delete rowErrorFields[k];
      manualRoles.clear();
    } else if (block === 'asr') {
      drafts.asr = null;
    } else if (block === 'general') {
      drafts.general = null;
      delete fieldErrors.idle;
    }
    delete blockErrors[block];
    renderBlock(block);
  }

  // ---- render ----

  function render() {
    renderAlerts();
    for (const [name] of BLOCKS) renderBlock(name);
    renderToc();
  }

  function renderBlock(name) {
    const shell = shells[name];
    if (SELF_LOADED.has(name)) {
      preserveFocus(() => {
        // round 9 WV-10 (R87 S-R1): a block that puts its own children in
        // place (aligns: the components and Colab sections, with
        // components_ui.js keepChildren) is not emptied first, so a task's
        // view stays where it is; the other blocks are drawn anew as before
        if (!selfUI[name].aligns) clear(shell);
        selfUI[name].render(shell);
      });
      renderToc();
      return;
    }
    preserveFocus(() => {
      clear(shell);
      shell.classList.toggle('is-loading', !data && !error);
      shell.classList.remove('has-error');
      // Loading: one skeleton per block. Failed: a single error card.
      shell.hidden = !data && !!error && name !== 'providers';
      if (!data) {
        const title = BLOCKS.find((b) => b[0] === name);
        if (error) shell.append(h('div', { class: 'card-bd' }, errorBlock(t('settings.load_error', { error }), retry)));
        else shell.append(head(title[1], blockTitle(title)), h('div', { class: 'card-bd' }, skeleton(name === 'providers' ? 5 : 4)));
        return;
      }
      if (name === 'providers') providersBlock(shell);
      else if (name === 'roles') rolesBlock(shell);
      else if (name === 'asr') asrBlock(shell);
      else if (name === 'holidays') holidaysBlock(shell);
      else if (name === 'display') displayBlock(shell);
      else if (name === 'kinds') kindsBlock(shell);
      else generalBlock(shell);
    });
    if (name !== 'providers') syncFoot(name);
    if (name === 'roles' || name === 'providers') renderAlerts();
    renderToc();
  }

  function head(ic, title, extra, tools) {
    return h(
      'header',
      { class: 'card-hd' },
      h('h2', { class: 'card-title', tabindex: '-1' }, icon(ic, { size: 16 }), h('span', null, title), extra),
      tools ? h('div', { class: 'card-tools' }, tools) : null,
    );
  }

  // r7 (T22 G10): a setup guide left before its end (started, not done:
  // GET /api/onboarding) is continued from here, where it left off; the
  // guide's "leave setup" says so. A state never started (the default,
  // e.g. an install from before the guide) asks for nothing. The rule is
  // util.js guideLeft, which the workbench's entry shares (round 8 F3).
  let guide = null;
  obApi
    .onboarding({ quiet: true })
    .then((st) => {
      if (!alive) return;
      guide = st;
      preserveFocus(renderAlerts);
    })
    .catch(() => {});

  function renderAlerts() {
    clear(alerts);
    if (!data) return;
    if (U.guideLeft(guide)) {
      alerts.append(
        h(
          'div',
          { class: 'banner banner-info set-banner' },
          icon('graduation-cap', { size: 16 }),
          h('span', { class: 'set-banner-text' }, t('settings.alert.guide_left')),
          btn(t('settings.alert.guide_continue'), { size: 'sm', onclick: () => (location.hash = '#/onboarding'), fkey: 'set:alert:guide' }),
        ),
      );
    }
    if (transcribeOnly(data.roles)) {
      alerts.append(
        h(
          'div',
          { class: 'banner banner-info set-banner' },
          icon('info', { size: 16 }),
          h('span', { class: 'set-banner-text' }, t('settings.alert.transcribe_only')),
          // r7 (T22 S6): the cost plans and how to get a key, not the bare form
          // gone once a service is added: the focus goes to the section's own entry (R75 L3)
          btn(t('settings.alert.add_provider'), { size: 'sm', onclick: presetDialog, fkey: 'set:alert:transcribe_only', fnext: 'set:prov:preset' }),
        ),
      );
    }
    const broken = data.roles.filter(roleBroken);
    if (broken.length) {
      alerts.append(
        h(
          'div',
          { class: 'banner banner-err set-banner', role: 'alert' },
          icon('circle-alert', { size: 16 }),
          h(
            'span',
            { class: 'set-banner-text' },
            // Launching needs summary, repair and translate (A4.1 snapshots).
            t(broken.some((r) => ['summary', 'repair', 'translate'].includes(r.role)) ? 'settings.alert.roles_broken_launch' : 'settings.alert.roles_broken', {
              roles: U.listJoin(broken.map((r) => quote(roleNameOf(r)))),
            }),
          ),
          btn(t('settings.alert.fix'), { size: 'sm', onclick: () => jump('roles'), fkey: 'set:alert:roles' }),
        ),
      );
    }
    const keyless = data.providers.filter((p) => needsKey(p) && p.used_by_roles.length);
    if (keyless.length) {
      alerts.append(
        h(
          'div',
          { class: 'banner banner-warn set-banner' },
          icon('key-round', { size: 16 }),
          h('span', { class: 'set-banner-text' }, t('settings.alert.keyless', { names: U.listJoin(keyless.map((p) => quote(p.name))) })),
          btn(t('settings.alert.fill_key'), { size: 'sm', onclick: () => providerDialog(keyless[0]), fkey: 'set:alert:key' }),
        ),
      );
    }
  }

  // Rebuilt only when the highlight or a marker changed (it is called on
  // every keystroke through syncFoot).
  let tocKey = '';
  function renderToc() {
    const blocks = BLOCKS.filter(shownBlock);
    const marks = blocks.map(([name]) => {
      if (dirty(name)) return 'dirty';
      if (data && name === 'roles' && data.roles.some(roleBroken)) return 'err';
      if (data && name === 'providers' && data.providers.some((p) => needsKey(p) && p.used_by_roles.length)) return 'warn';
      return '';
    });
    const key = JSON.stringify([current, marks]);
    if (key === tocKey && toc.firstChild) return;
    tocKey = key;
    const MARKS = { dirty: t('settings.unsaved'), err: t('settings.toc.err'), warn: t('settings.toc.warn') };
    const item = (b, i) => {
      const [name, ic] = b;
      const title = blockTitle(b);
      const mark = marks[i] ? h('span', { class: `set-toc-mark is-${marks[i]}`, title: MARKS[marks[i]] }, h('span', { class: 'visually-hidden' }, MARKS[marks[i]])) : null;
      return h(
        'li',
        null,
        h(
          'button',
          {
            type: 'button',
            class: 'set-toc-item' + (current === name ? ' is-current' : ''),
            'aria-current': current === name ? 'true' : undefined,
            dataset: { fkey: `set:toc:${name}` },
            onclick: () => jump(name),
          },
          icon(ic, { size: 15 }),
          h('span', { class: 'set-toc-label' }, title),
          mark,
        ),
      );
    };
    preserveFocus(() => {
      clear(toc);
      toc.append(h('ul', { class: 'set-toc-list' }, blocks.map(item)));
    });
  }

  function jump(name) {
    const shell = shells[name];
    current = name;
    renderToc();
    shell.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'start' });
    const title = shell.querySelector('.card-title');
    if (title) title.focus({ preventScroll: true });
  }

  // Scroll spy: the TOC follows the section at the top of the viewport.
  const spy =
    'IntersectionObserver' in window
      ? new IntersectionObserver(
          (entries) => {
            const vis = entries.filter((e) => e.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
            if (!vis.length) return;
            const name = vis[0].target.id.replace(/^set-/, '');
            if (name !== current) {
              current = name;
              renderToc();
            }
          },
          { rootMargin: '-8% 0px -62% 0px' },
        )
      : null;
  if (spy) for (const [name] of BLOCKS) spy.observe(shells[name]);

  // Save bar of a block (roles, asr, general): updated in place while typing.
  function foot(block, onSave) {
    const status = h('span', { class: 'set-foot-status', role: 'status' });
    const undoBtn = btn(t('settings.foot.undo'), { size: 'sm', kind: 'ghost', icon: 'undo-2', onclick: () => undo(block), fkey: `set:${block}:undo` });
    const saveBtn = btn(t('common.save'), { size: 'sm', kind: 'primary', onclick: onSave, fkey: `set:${block}:save` });
    const elx = h('footer', { class: 'set-foot' }, status, h('div', { class: 'set-foot-actions' }, undoBtn, saveBtn));
    foots[block] = { el: elx, status, undoBtn, saveBtn };
    return elx;
  }

  function syncFoot(block) {
    const f = foots[block];
    if (!f || !f.el.isConnected) return;
    const d = dirty(block);
    const busy = saving.has(block);
    const err = blockErrors[block];
    f.el.classList.toggle('is-dirty', d);
    f.el.classList.toggle('is-error', !!err);
    shells[block].classList.toggle('is-dirty', d);
    clear(f.status);
    if (busy) f.status.append(icon('loader-circle', { size: 14, class: 'spin' }), t('settings.saving'));
    else if (err) f.status.append(icon('circle-alert', { size: 14 }), h('span', { class: 'set-foot-err' }, err));
    else if (d) {
      const n = block === 'roles' ? data.roles.filter(roleDirty).length : 0;
      f.status.append(h('span', { class: 'set-dirty-dot', 'aria-hidden': 'true' }), n > 1 ? t('settings.foot.dirty_roles', { n }) : t('settings.unsaved'));
    } else f.status.append(icon('check', { size: 14 }), t('settings.foot.saved'));
    f.undoBtn.hidden = !d || busy;
    // T23 serious 3: a "save" that has the focus (it was just pressed) is
    // not disabled while it saves and once there is nothing left to save:
    // the focus would fall to the page. It is aria-disabled instead, as
    // "test connection" while it runs, and keeps the focus through the
    // block's redraw (preserveFocus by its data-fkey); a press then does
    // nothing (saveRoles / saveASR / saveGeneral return without changes or
    // while saving). Without the focus it is disabled as before.
    const off = !d || busy;
    const keep = off && document.activeElement === f.saveBtn;
    f.saveBtn.disabled = off && !keep;
    if (keep) f.saveBtn.setAttribute('aria-disabled', 'true');
    else f.saveBtn.removeAttribute('aria-disabled');
    const lbl = f.saveBtn.querySelector('.btn-label');
    if (lbl) lbl.textContent = busy ? t('settings.saving') : t('common.save');
    renderToc();
  }

  // ---- providers ----

  // r7 (T22 S6): "add from a preset" — the setup guide's step "translation
  // and repair" (onboarding_providers.js) in a dialog: the cost plans, then
  // for each service of the plan how to get an API key (steps, sign-up,
  // key and price pages), the key and "test connection". A key saved there
  // adds the service at once (POST /api/providers with the preset's
  // address and limits); that is all the dialog does by itself. Nothing of
  // the guide's own state is touched (save is a no-op here). Its requests
  // that are not quiet report their errors as toasts while it is open.
  // R75 M1: unlike the guide (nothing set up yet), the roles here are the
  // user's own. "Use this plan for the roles…" is a step of its own: a
  // confirmation lists each role that would change (now → then), the
  // parameters each role keeps (temperature, thinking, output limit, the
  // translation's window and batch: only the provider and the model
  // change; thinking goes off where the new service has none, said there),
  // the zero-cost plan's night window for free services that have no
  // tariff yet (one they have is kept, said there), and unsaved edits of
  // those roles that are dropped.
  async function presetDialog() {
    if (presetOpen) return; // one at a time (F4: the address may ask while it is open)
    presetOpen = true;
    let step = null;
    // the last roles step: {n changed, total confirmed, told}; its writes
    // while they run (R81 L1: Esc or the backdrop may close the dialog then)
    let result = null;
    let writing = null;
    // R81b L10: roles this dialog changed that the page's settings may not
    // show yet (stopped half way, and the settings could not be read again)
    let ownUnseen = false;
    const before = new Set(((data && data.providers) || []).map((p) => p.id)); // the services before
    const onErr = (err) => toast(err.message, err.status >= 500 || err.status === 0 ? 'error' : 'warn');
    setObErrorHandler(onErr);
    try {
      await openDialog(
        t('settings.provider.preset_title'),
        (close, busy) => {
          const box = h('div', { class: 'set-preset-body' });
          const actions = h('div', { class: 'dlg-actions' });
          let applying = false;
          const env = {
            inSettings: true,
            cache: {},
            data: { tier: '' },
            alive: () => alive && !!step,
            rerender: draw,
            save: async () => {},
            setData: async (patch) => void Object.assign(env.data, patch),
            next() {}, // the guide's "use this plan" (apply) is not offered here
          };
          function draw() {
            if (!step) return;
            preserveFocus(() => {
              clear(box);
              box.append(...[step.body()].flat(Infinity).filter(Boolean));
              clear(actions);
              const plan = step.plan();
              actions.append(
                // R81 N1: what changes is worked out when it is pressed, not here
                btn(t('settings.provider.preset_roles'), { icon: 'bot', disabled: applying || !plan, onclick: useForRoles, fkey: 'set:preset:roles' }),
                btn(t('common.close'), { kind: 'primary', disabled: applying, onclick: () => close(null), fkey: 'set:preset:close' }),
              );
            });
          }
          // R81 N1: the changes are worked out from the settings as the page
          // has them when the button is pressed (kept fresh by the event
          // stream); once confirmed, the settings are read again: the same
          // changes are written, else (changed elsewhere meanwhile) they are
          // worked out again and shown again, saying so, never written over
          // nor dropped unseen. Bounded: settings changing on every read
          // give up after MAX_ROUNDS confirmations, with a word.
          async function useForRoles() {
            if (!step) return;
            let ch = roleChanges(step.plan());
            let changed = '';
            // a confirmation shown again may be about this dialog's own
            // writes then: said without naming where the change came from
            const mine = ownUnseen;
            for (let round = 0; ; round++) {
              // R75 M2: nothing that could change (no key yet, or the roles
              // already use these): said, and no confirmation to nothing
              if (!ch.rows.length) {
                toast(t(ch.missing.length ? 'settings.provider.preset_roles_nokey' : 'settings.provider.preset_roles_same'), 'info');
                return;
              }
              if (round >= MAX_ROUNDS) {
                toast(t('settings.provider.preset_roles_moving'), 'warn');
                return;
              }
              if (!(await confirmRoleChanges(ch, changed)) || !step) return;
              let fresh = null;
              try {
                fresh = await api.settings();
              } catch {
                return; // shown by the API layer; nothing written
              }
              if (!step) return;
              ownUnseen = false;
              const again = roleChanges(step.plan(), fresh);
              if (changesKey(again) === changesKey(ch)) {
                ch = again;
                break;
              }
              ch = again;
              changed = mine ? 'any' : 'elsewhere';
            }
            applying = true;
            draw();
            writing = busy(applyRoleChanges(ch));
            const n = await writing;
            writing = null;
            result = { n, total: ch.rows.length, told: false };
            if (n === ch.rows.length) {
              applying = false;
              close(true);
              return;
            }
            // R81 L2: stopped half way (the error is told by the API layer):
            // what changed and what not; the dialog stays, the button lists
            // the rest
            if (n > 0) {
              toast(t('settings.provider.preset_roles_partial', { n: U.count(n), total: U.count(ch.rows.length) }), 'warn');
              result.told = true;
              // R81b L10: the settings as they are after these writes before
              // the button is offered again (the event stream that would
              // bring them may be late, or down): pressed again at once, it
              // lists the rest only
              ownUnseen = !(await load());
              if (!step) return;
            }
            applying = false;
            draw();
          }
          step = providersStep(env);
          draw();
          // R75 L2: a key saved here is stored as in the plain form, in the clear when the credential store is not available: said as there
          const store = data && data.credentials_error_info ? h('p', { class: 'dlg-hint tone-warn', role: 'status' }, icon('lock-open', { size: 12 }), ' ', U.errorInfoText(data.credentials_error_info)) : null;
          return h('div', { class: 'set-preset' }, h('p', { class: 'dlg-msg' }, t('settings.provider.preset_intro')), store, box, actions);
        },
        { wide: true },
      );
      // closed while the confirmed writes run (Esc, the backdrop): they go
      // on; their outcome is said once they end (R81 L1)
      if (writing) {
        await writing;
        await null; // the roles step's own ending runs first
      }
    } finally {
      presetOpen = false;
      step = null;
      // R75 L3: only its own (a route to the guide meanwhile set the guide's)
      if (obErrorHandler() === onErr) setObErrorHandler(null);
    }
    if (!alive) return;
    // the services (and roles) as they are now
    await load();
    if (!alive) return;
    // R75 M2: what happened, and nothing more: roles changed, or services
    // added with the roles as they were, or nothing at all
    if (result && result.n === result.total) toast(t('settings.provider.preset_applied', { n: U.count(result.n) }), 'success');
    else if (result && result.n > 0 && !result.told) toast(t('settings.provider.preset_roles_partial', { n: U.count(result.n), total: U.count(result.total) }), 'warn');
    else if (!result || !result.n) {
      const added = ((data && data.providers) || []).filter((p) => !before.has(p.id));
      if (added.length) toast(t('settings.provider.preset_added', { names: U.listJoin(added.map((p) => quote(p.name))) }), 'success');
    }
  }

  // What a plan (providersStep plan()) would change in the saved roles:
  // rows {r, provider, model, from, to, thinkingOff}; missing: roles whose
  // service has no key yet; night / nightKept: free services that get the
  // night window / keep their own tariff — by the tariff they have in s
  // (R81b L9: the plan's list is the guide step's, read when the dialog
  // opened or a key was saved; a tariff set elsewhere since then is kept).
  // s: the settings to compare with (the page's, or a fresh read; R81 N1).
  // Rows in the order of the Roles section (R81 L3); each with the params
  // it is written with: the role's own, thinking off where the new
  // provider has none.
  function roleChanges(plan, s = data) {
    const out = { plan, rows: [], missing: [], night: [], nightKept: [] };
    if (!plan || !s) return out;
    const label = (pid, model) => {
      const p = (s.providers || []).find((x) => x.id === pid);
      return p && model ? `${p.name} · ${model}` : t('settings.not_configured');
    };
    const byRole = new Map(plan.roles.map((x) => [x.role, x]));
    for (const r of s.roles || []) {
      const x = byRole.get(r.role);
      if (!x) continue;
      if (!x.provider) {
        out.missing.push(r);
        continue;
      }
      if (r.configured && r.provider_id === x.provider.id && r.model === x.model) continue;
      const thinkingOff = (r.thinking || 'off') !== 'off' && !x.provider.supports_thinking;
      const params = { thinking: thinkingOff ? 'off' : r.thinking || 'off' };
      if (r.temperature != null) params.temperature = r.temperature;
      if (r.role === 'translate' && r.window != null) params.window = r.window;
      if (r.max_tokens != null) params.max_tokens = r.max_tokens;
      if (r.role === 'translate' && r.batch != null) params.batch = r.batch;
      out.rows.push({ r, provider: x.provider, model: x.model, params, from: r.configured ? label(r.provider_id, r.model) : t('settings.not_configured'), to: `${x.provider.name} · ${x.model}`, thinkingOff });
    }
    for (const p0 of plan.night) {
      const p = (s.providers || []).find((x) => x.id === p0.id) || p0;
      (p.tariff ? out.nightKept : out.night).push(p);
    }
    return out;
  }

  // Everything a confirmation of roleChanges shows or writes (R81 N1),
  // with whether each free service has a tariff (R81b L9).
  function changesKey(ch) {
    return JSON.stringify([
      ch.rows.map((x) => [x.r.role, x.r.configured, x.r.provider_id, x.r.model, x.provider.id, x.model, x.params, x.from, x.to]),
      ch.missing.map((r) => r.role),
      ch.night.map((p) => [p.id, !!p.tariff]),
      ch.nightKept.map((p) => [p.id, !!p.tariff]),
    ]);
  }

  // changed: shown again because the settings changed meanwhile (R81 N1):
  // 'elsewhere', or 'any' when this dialog's own writes may be the change
  // (R81b L10: the page could not read the settings after them)
  function confirmRoleChanges(ch, changed = '') {
    const names = (rows) => U.listJoin(rows.map((x) => quote(roleNameOf(x.r || x))));
    const off = ch.rows.filter((x) => x.thinkingOff);
    const drafts_ = ch.rows.filter((x) => drafts.roles[x.r.role]);
    const note = (ic, text) => h('li', null, icon(ic, { size: 14 }), h('span', null, text));
    const notes = [
      off.length ? note('triangle-alert', t('settings.provider.preset_roles_thinking', { roles: names(off) })) : null,
      ch.missing.length ? note('info', t('settings.provider.preset_roles_missing', { roles: names(ch.missing) })) : null,
      ch.night.length ? note('clock', t('settings.provider.preset_roles_night', { names: U.listJoin(ch.night.map((p) => quote(p.name))), from: ch.plan.nightFrom, to: ch.plan.nightTo })) : null,
      ch.nightKept.length ? note('clock', t('settings.provider.preset_roles_night_kept', { names: U.listJoin(ch.nightKept.map((p) => quote(p.name))) })) : null,
      drafts_.length ? note('triangle-alert', t('settings.provider.preset_roles_drafts', { roles: names(drafts_) })) : null,
    ].filter(Boolean);
    return confirmDialog({
      title: t('settings.provider.preset_roles_title', { plan: ch.plan.name }),
      message: t('settings.provider.preset_roles_message'),
      okText: t('settings.provider.preset_roles_ok'),
      focusCancel: true,
      body: h(
        'div',
        { class: 'set-preset-roles' },
        changed ? h('p', { class: 'dlg-hint tone-warn', role: 'alert' }, icon('triangle-alert', { size: 13 }), ' ', t(changed === 'any' ? 'settings.provider.preset_roles_changed_any' : 'settings.provider.preset_roles_changed')) : null,
        h('ul', { class: 'app-move-points' }, ch.rows.map((x) => note('arrow-right', t('settings.provider.preset_roles_row', { role: roleNameOf(x.r), from: x.from, to: x.to })))),
        // R81 I8: no empty list when there is nothing more to note
        notes.length ? h('ul', { class: 'app-move-points' }, notes) : null,
      ),
    });
  }

  // Sets each role to its new provider and model with the parameters it
  // has (thinking off where the new provider has none); the night window
  // for free services without a tariff. → the number of roles changed.
  async function applyRoleChanges(ch) {
    let n = 0;
    for (const x of ch.rows) {
      const r = x.r;
      try {
        await api.setRole(r.role, x.provider.id, x.model, x.params);
        delete drafts.roles[r.role];
        n++;
      } catch {
        break; // shown by the API layer; the roles changed so far stay
      }
    }
    if (n === ch.rows.length) {
      for (const p of ch.night) {
        try {
          await obApi.patchProvider(p.id, { tariff: ch.plan.nightTariff, offpeak_only: true });
        } catch {
          break; // shown by the dialog's error handler
        }
      }
    }
    return n;
  }

  function providersBlock(shell) {
    const ps = data.providers;
    // fnext: the empty state's entry is gone once a service is added (R75 L3)
    const presetBtn = (fkey, kind, fnext) => btn(t('settings.provider.add_preset'), { size: kind ? undefined : 'sm', kind, icon: 'sparkles', title: t('settings.provider.add_preset_tip'), onclick: presetDialog, fkey, fnext });
    shell.append(
      head('server', t('settings.block.providers'), ps.length ? h('span', { class: 'card-count' }, String(ps.length)) : null, [
        presetBtn('set:prov:preset'),
        btn(t('settings.provider.add'), { size: 'sm', icon: 'plus', onclick: () => providerDialog(null), fkey: 'set:prov:add' }),
      ]),
      h(
        'div',
        { class: 'card-bd set-prov-bd' },
        ps.length
          ? h('ul', { class: 'pv-list' }, ps.map(providerRow))
          : h(
              'div',
              { class: 'set-empty' },
              h('div', { class: 'set-empty-icon' }, icon('server', { size: 22 })),
              h('div', null, h('strong', null, t('settings.provider.empty_title')), h('p', null, t('settings.provider.empty_text'))),
              presetBtn('set:prov:preset2', 'primary', 'set:prov:preset'),
              btn(t('settings.provider.add'), { icon: 'plus', onclick: () => providerDialog(null), fkey: 'set:prov:add2' }),
            ),
      ),
    );
    balanceKey = balanceSig();
  }

  function providerItems(p) {
    return [
      { label: t('common.edit'), icon: 'pencil', onSelect: () => providerDialog(p) },
      tests.get(p.id)?.state !== 'running' ? { label: t('settings.provider.test'), icon: 'plug', onSelect: () => testProvider(p) } : null,
      !probing.has(p.id) ? { label: t('settings.provider.probe'), icon: 'scan-search', onSelect: () => probe(p) } : null,
      'sep',
      // A provider a role uses cannot be deleted (409): not offered (§9.8).
      p.used_by_roles.length ? null : { label: t('common.delete'), icon: 'trash-2', danger: true, onSelect: () => deleteProvider(p) },
    ];
  }

  // the coded "以明文保存" of a provider (sec3 L2)
  function keyProblem(p) {
    return U.errorInfoText(p.key_error_info, '') || t('errors.settings.provider_key_unprotected', { provider: p.name });
  }

  function fact(k, v, title, cls = '') {
    return h('span', { class: 'pv-fact ' + cls, title }, h('span', { class: 'pv-k' }, k), h('span', { class: 'pv-v' }, v));
  }

  function priceText(p) {
    if (p.price_input == null || p.price_cache_hit == null || p.price_output == null) return null;
    const m = (v) => U.fmtMoney(v, p.currency) || String(v);
    return `${m(p.price_input)} / ${m(p.price_cache_hit)} / ${m(p.price_output)}`;
  }

  function balanceFact(p) {
    const label = t('settings.provider.balance');
    if (p.balance_api !== 'deepseek') return fact(label, h('span', { class: 'dim' }, t('settings.provider.balance_off')), t('settings.provider.balance_off_tip'));
    const res = ctx.resources();
    const rp = res && (res.providers || []).find((x) => x.id === p.id);
    if (rp && rp.balance != null) {
      return fact(label, U.fmtMoney(Number(rp.balance), rp.balance_currency) || rp.balance, rp.balance_at ? t('settings.provider.balance_at_tip', { when: U.relTime(rp.balance_at) }) : t('settings.provider.balance_refresh_tip'), 'is-num');
    }
    if (rp && (rp.balance_error || rp.balance_error_info)) return fact(label, h('span', { class: 'pv-bal-err' }, icon('triangle-alert', { size: 12 }), '--'), U.errorInfoText(rp.balance_error_info, rp.balance_error), 'is-warn');
    return fact(label, h('span', { class: 'dim' }, '--'), t('settings.provider.balance_pending_tip'));
  }

  function balanceSig() {
    const res = ctx.resources();
    if (!res || !data) return '';
    return JSON.stringify((res.providers || []).map((p) => [p.id, p.balance, p.balance_currency, p.balance_error, p.balance_error_info, p.tariff_state ? [p.tariff_state.peak, p.tariff_state.next_change, p.tariff_state.missing_holidays] : null]));
  }

  // v2 P12: the tariff in one line, its live state and the off-peak switch
  function tariffFact(p) {
    const tariff = p.tariff && typeof p.tariff === 'object' ? p.tariff : null;
    if (!tariff) return fact(t('settings.tariff.fact'), h('span', { class: 'dim' }, t('settings.tariff.none')), t('settings.tariff.none_hint'));
    const res = ctx.resources();
    const rp = res && (res.providers || []).find((x) => x.id === p.id);
    const ts = (rp && rp.tariff_state) || p.tariff_state;
    return h(
      'span',
      { class: 'pv-fact pv-tariff' },
      h('span', { class: 'pv-k' }, t('settings.tariff.fact')),
      h('span', { class: 'pv-v' }, ts && hasPeaks(ts) ? tariffTag(ts) : null, h('span', { class: 'pv-tariff-sum', title: tariffSummary(tariff) }, tariffSummary(tariff)), p.offpeak_only ? h('span', { class: 'chip chip-quiet' }, t('settings.tariff.offpeak_only_chip')) : null),
    );
  }

  function providerRow(p) {
    const test = tests.get(p.id);
    const price = priceText(p);
    // round 2 (T6 #7): while a test runs the button is aria-disabled and
    // aria-busy instead of disabled, so the re-renders before and after it
    // (preserveFocus by fkey) keep the keyboard focus on it; testProvider
    // ignores a click while one runs
    const testing = !!test && test.state === 'running';
    const testBtn = btn(testing ? t('settings.provider.testing') : t('settings.provider.test'), { size: 'sm', icon: 'plug', onclick: () => testProvider(p), fkey: `set:prov:${p.id}:test` });
    if (testing) {
      testBtn.setAttribute('aria-disabled', 'true');
      testBtn.setAttribute('aria-busy', 'true');
    }
    // the same for "probe models" (R28 A3); probe() ignores a click while
    // one runs
    const probingNow = probing.has(p.id);
    const probeBtn = btn(probingNow ? t('settings.provider.probing') : t('settings.provider.probe'), { size: 'sm', icon: 'scan-search', onclick: () => probe(p), fkey: `set:prov:${p.id}:probe` });
    if (probingNow) {
      probeBtn.setAttribute('aria-disabled', 'true');
      probeBtn.setAttribute('aria-busy', 'true');
    }
    const actions = h(
      'div',
      { class: 'pv-actions' },
      testBtn,
      probeBtn,
      iconBtn('pencil', t('settings.provider.edit_named', { name: p.name }), { onclick: () => providerDialog(p), fkey: `set:prov:${p.id}:edit` }),
      menuButton(iconBtn('ellipsis', t('settings.provider.more_named', { name: p.name }), { menu: true, fkey: `set:prov:${p.id}:menu` }), () => providerItems(p)),
    );
    const row = h(
      'li',
      { class: 'pv-row' + (needsKey(p) ? ' is-keyless' : '') },
      h('div', { class: 'pv-avatar', 'aria-hidden': 'true', dataset: { type: p.type } }, (Array.from(p.name.trim())[0] || '?').toUpperCase()),
      h(
        'div',
        { class: 'pv-main' },
        h(
          'div',
          { class: 'pv-title' },
          h('span', { class: 'pv-name' }, p.name),
          h('span', { class: 'chip chip-quiet' }, typeName(p.type)),
          needsKey(p) ? h('span', { class: 'chip chip-warn' }, icon('key-round', { size: 11 }), t(p.key_unreadable ? 'settings.provider.key_unreadable_chip' : 'settings.provider.no_key_chip')) : null,
          // sec3 L2: saved while there was no data key
          p.key_unprotected ? h('span', { class: 'chip chip-warn', title: keyProblem(p) }, icon('lock-open', { size: 11 }), t('settings.provider.key_unprotected_chip')) : null,
        ),
        h('div', { class: 'pv-url mono', title: p.base_url }, p.base_url),
        // AUD1 I-3: plain http to a computer off the local network
        plainHTTPFar(p.base_url) ? h('p', { class: 'set-note pv-plain-http' }, icon('lock-open', { size: 14 }), h('span', null, t('settings.provider.plain_http_note'))) : null,
        h(
          'div',
          { class: 'pv-facts' },
          p.key_unreadable
            ? fact(t('settings.provider.key'), h('span', { class: 'tone-warn' }, t('settings.provider.key_unreadable')), t('errors.settings.provider_key_unreadable', { provider: p.name }))
            : fact(
                t('settings.provider.key'),
                p.key_hint ? h('span', { class: 'mono' + (p.key_unprotected ? ' tone-warn' : '') }, p.key_hint) : h('span', { class: isLocal(p.base_url) ? 'dim' : 'tone-warn' }, isLocal(p.base_url) ? t('settings.provider.key_local') : t('settings.provider.key_unset')),
                p.key_unprotected ? keyProblem(p) : t('settings.provider.key_tip'),
              ),
          fact(t('settings.provider.concurrency'), String(p.max_concurrency), t('settings.provider.concurrency_tip'), 'is-num'),
          fact(t('settings.provider.rpm'), rpmText(p.rpm), t('settings.provider.rpm_tip'), 'is-num'),
          fact(t('settings.provider.price'), price || h('span', { class: 'dim' }, t('settings.not_configured')), price ? t('settings.provider.price_tip') : t('settings.provider.price_unset_tip'), 'is-num'),
          balanceFact(p),
          tariffFact(p),
        ),
        h(
          'div',
          { class: 'pv-meta' },
          p.used_by_roles.length
            ? h('span', { class: 'pv-uses' }, h('span', { class: 'pv-k' }, t('settings.provider.used_by')), p.used_by_roles.map((r) => h('span', { class: 'chip chip-run' }, roleLabel(r))))
            : h('span', { class: 'pv-uses dim' }, t('settings.provider.unused')),
          p.snapshot_refs ? h('span', { class: 'pv-refs', title: t('settings.provider.refs_tip') }, t('settings.provider.refs', { n: p.snapshot_refs })) : null,
        ),
        modelsLine(p),
        testLine(p, test),
        limitsHint(p),
        // T-PERF-03: the account's observed limits, when the settings are
        // clearly below them; the button only fills the dialog's fields
        limitsBanner(p, () => providerDialog(p, { fillLimits: true })),
      ),
      actions,
    );
    bindContextMenu(row, () => providerItems(p));
    return row;
  }

  // rpm 0 = no per-minute limit (perf-llm)
  function rpmText(rpm) {
    return rpm > 0 ? t('settings.provider.rpm_value', { n: rpm }) : t('settings.provider.rpm_unlimited');
  }

  // perf-llm: a DeepSeek provider still on the old conservative limits is
  // offered the preset's limits in one click (never changed by itself).
  const applyingLimits = new Set();
  function limitsHint(p) {
    if (!hasOldLimits(p)) return null;
    const busy = applyingLimits.has(p.id);
    const limits = p.rpm > 0 ? t('system.api.limits', { max: p.max_concurrency, rpm: p.rpm }) : t('system.api.limits_unlimited', { max: p.max_concurrency });
    return h(
      'div',
      { class: 'banner banner-info set-banner pv-limits', style: { margin: '8px 0 0', flexWrap: 'wrap' } },
      icon('gauge', { size: 16 }),
      h('span', { class: 'set-banner-text', style: { flex: '1 1 16rem' } }, t('settings.provider.limits_old', { limits, max: DEEPSEEK_LIMITS.max_concurrency })),
      btn(busy ? t('settings.provider.limits_applying') : t('settings.provider.limits_apply'), {
        size: 'sm',
        disabled: busy,
        onclick: () => applyLimits(p),
        fkey: `set:prov:${p.id}:limits`,
      }),
    );
  }

  async function applyLimits(p) {
    if (applyingLimits.has(p.id)) return;
    applyingLimits.add(p.id);
    renderBlock('providers');
    try {
      await api.patchProvider(p.id, { max_concurrency: DEEPSEEK_LIMITS.max_concurrency, rpm: DEEPSEEK_LIMITS.rpm });
      toast(t('settings.provider.limits_applied', { name: p.name }), 'success');
    } catch {
      // shown by the API layer
    } finally {
      applyingLimits.delete(p.id);
      if (alive) load();
    }
  }

  function modelsLine(p) {
    if (probing.has(p.id)) return h('div', { class: 'pv-models' }, icon('loader-circle', { size: 13, class: 'spin' }), t('settings.provider.models_probing'));
    const n = p.models.length;
    const when = p.models_probed_at ? U.relTime(p.models_probed_at) : '';
    if (p.models_error || p.models_error_info) {
      // the coded reason; the back end's full text (detail) on hover
      const msg = U.errorInfoText(p.models_error_info, p.models_error);
      const detail = (p.models_error_info && p.models_error_info.detail) || p.models_error || '';
      return h(
        'div',
        { class: 'pv-models is-error' },
        icon('triangle-alert', { size: 13 }),
        h(
          'span',
          null,
          h('span', { class: 'pv-models-err', title: detail && detail !== msg ? detail : undefined }, msg),
          h('span', { class: 'pv-models-hint' }, n ? t('settings.provider.models_keep', { n }) : t('settings.provider.models_manual')),
        ),
      );
    }
    if (!p.models_probed_at) return h('div', { class: 'pv-models dim' }, icon('scan-search', { size: 13 }), t('settings.provider.models_none'));
    return h(
      'div',
      { class: 'pv-models' },
      icon('list-checks', { size: 13 }),
      h('span', { title: p.models.slice(0, 40).join('\n') + (n > 40 ? '\n' + t('settings.provider.models_more', { n }) : '') }, n ? t('settings.provider.models_found', { n }) : t('settings.provider.models_empty')),
      when ? h('span', { class: 'dim' }, `· ${when}`) : null,
    );
  }

  function testLine(p, tr) {
    if (!tr) return null;
    if (tr.state === 'running') return h('div', { class: 'pv-test', role: 'status' }, icon('loader-circle', { size: 13, class: 'spin' }), t('settings.provider.test_running'));
    const when = U.relTime(new Date(tr.at).toISOString());
    const model = tr.model ? t('settings.provider.test_model', { model: tr.model }) : '';
    if (tr.state === 'ok') {
      return h(
        'div',
        { class: 'pv-test is-ok', role: 'status' },
        icon('circle-check', { size: 14 }),
        h('strong', null, t('settings.provider.test_ok')),
        h('span', { class: 'pv-test-meta' }, [t('settings.provider.latency', { ms: tr.latency_ms }), model, when].filter(Boolean).join(' · ')),
      );
    }
    // the coded reason, then the server's own text when it says more
    const msg = U.errorInfoText(tr.error_info, tr.error) || t('common.unknown_error');
    const detail = tr.error_info && tr.error_info.detail && tr.error_info.detail !== msg ? tr.error_info.detail : '';
    return h(
      'div',
      { class: 'pv-test is-err', role: 'alert' },
      h(
        'div',
        { class: 'pv-test-head' },
        icon('circle-alert', { size: 14 }),
        h('strong', null, t('settings.provider.test_fail')),
        h('span', { class: 'pv-test-meta' }, [tr.latency_ms ? t('settings.provider.latency_ms', { ms: tr.latency_ms }) : '', model, when].filter(Boolean).join(' · ')),
      ),
      h('pre', { class: 'pv-test-err' }, detail ? `${msg}\n${detail}` : msg),
    );
  }

  async function testProvider(p) {
    if (tests.get(p.id)?.state === 'running') return;
    tests.set(p.id, { state: 'running' });
    renderBlock('providers');
    let tr;
    try {
      const r = await api.testProvider(p.id);
      tr = { state: r.ok ? 'ok' : 'fail', latency_ms: r.latency_ms, model: r.model || '', error: r.error || '', error_info: r.error_info || null, at: Date.now() };
    } catch (e) {
      tr = { state: 'fail', error: e.message, at: Date.now() };
    }
    tests.set(p.id, tr);
    if (alive) renderBlock('providers');
  }

  async function probe(p) {
    if (probing.has(p.id)) return;
    probing.add(p.id);
    renderBlock('providers');
    renderBlock('roles');
    try {
      await api.probeModels(p.id); // the result is shown from the refreshed settings
    } catch {
      // shown by the API layer
    } finally {
      probing.delete(p.id);
      if (alive) {
        await load();
        renderBlock('providers');
        renderBlock('roles');
      }
    }
  }

  async function deleteProvider(p) {
    const refs = p.snapshot_refs;
    const ok = await confirmDialog({
      title: t('settings.provider.delete_title', { name: p.name }),
      message: refs > 0 ? t('settings.provider.delete_refs', { n: refs }) : t('settings.provider.delete_plain'),
      okText: t('common.delete'),
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await api.deleteProvider(p.id);
      tests.delete(p.id);
      // the back end warns (text only) when lecture snapshots referred to it
      toast(t(res && (res.warning || res.snapshot_refs > 0) ? 'settings.provider.deleted_refs' : 'settings.provider.deleted', { name: p.name }), 'success');
      load();
    } catch {
      // the back end's reason (e.g. 409 settings.provider_in_use) is shown by the API layer
    }
  }

  // New (p = null) or edit. The key is never shown: on edit an empty field
  // keeps the stored key (A5.2).
  // fillLimits (T-PERF-03): open with the suggestion from the observed
  // account limits filled into the concurrency and per-minute fields (not
  // saved until the user saves).
  // Round 9 WV-06 (T28 suggestion 5): Esc, a click on the backdrop and
  // "取消" ask "放弃修改？" first when a field differs from what the dialog
  // opened with (dom.js openDialog confirmCancel, round 9 F1); unchanged, or
  // while the save runs, they close at once. What it opened with is read
  // once the dialog is built, so the prefilled values (a new provider's
  // type limits and address, fillLimits' suggestion) are no change of the
  // user's; "填入建议值" pressed in the dialog is. The key counts only as
  // typed or not: its text is kept nowhere.
  function providerDialog(p, { fillLimits = false } = {}) {
    const isNew = !p;
    const defaults = data.default_base_urls || {};
    let edited = () => false; // set by the content once built
    let saving = () => false;
    const mayClose = () =>
      saving() || !edited()
        ? true
        : confirmDialog({
            title: t('settings.provider.discard_title'),
            message: t('settings.provider.discard_message'),
            okText: t('wizard.close.discard'),
            cancelText: t('wizard.close.keep'),
            danger: true,
          });
    return openDialog(
      isNew ? t('settings.provider.add') : t('settings.provider.edit_title', { name: p.name }),
      (close, busy) => {
        let type = p ? p.type : 'openai';
        let balanceTouched = !isNew;
        const field = (label, control, hint, cls = '') => h('label', { class: 'dlg-field ' + cls }, h('span', { class: 'dlg-label' }, label), control, hint ? h('span', { class: 'dlg-hint' }, hint) : null);
        const name = h('input', { class: 'input', value: p ? p.name : '', maxlength: '64', autocomplete: 'off', placeholder: t('settings.example', { value: 'DeepSeek' }), autofocus: true, 'aria-required': 'true' });
        const base = h('input', { class: 'input mono', type: 'url', value: p ? p.base_url : defaults[type] || '', placeholder: 'https://…', spellcheck: 'false', autocomplete: 'off', 'aria-required': 'true' });
        const key = h('input', {
          class: 'input mono',
          type: 'password',
          autocomplete: 'new-password',
          spellcheck: 'false',
          placeholder: p ? (p.key_unreadable ? t('settings.provider.key_unreadable') : p.key_hint ? t('settings.provider.key_keep', { hint: p.key_hint }) : t('settings.provider.key_keep_none')) : 'sk-…',
        });
        // v3.2.x: a new provider starts with its type's limits
        // (limitsPrefill: Anthropic 4 / no per-minute limit, others 4 / 60);
        // switching the type follows them until the user edits a field. An
        // existing provider keeps its own.
        const pre = limitsPrefill(type);
        const conc = h('input', { class: 'input input-num', type: 'number', min: '1', max: String(MAX_CONCURRENCY), step: '1', value: String(p ? p.max_concurrency : pre.max_concurrency) });
        const rpm = h('input', { class: 'input input-num', type: 'number', min: '0', max: String(MAX_RPM), step: '1', value: String(p ? p.rpm : pre.rpm) });
        const limitsTouched = { conc: !isNew, rpm: !isNew };
        conc.addEventListener('input', () => (limitsTouched.conc = true));
        rpm.addEventListener('input', () => (limitsTouched.rpm = true));
        // Anthropic: the account's tier decides the usable limits (no
        // numbers here; the official page, when it opens)
        const limitsNote = h('span', { class: 'dlg-hint span-2 set-limits-note', dataset: { fkey: 'set:prov:limits_note' } });
        // T-PERF-03: what the account's answers reported, the suggestion
        // and "填入建议值" (fills the two fields only); described fields
        const observed = createLimitsHint(p, conc, rpm, { filled: fillLimits });
        const syncLimitsNote = () => {
          // a new key typed in: its account's limits are not known yet (R38 S6)
          observed.sync(type, !!key.value.trim(), base.value);
          limitsNote.hidden = type !== 'anthropic';
          limitsNote.replaceChildren(
            ...(type === 'anthropic'
              ? [
                  icon('info', { size: 12 }),
                  ' ',
                  t('settings.provider.anthropic_limits_note'),
                  ' ',
                  h('button', { type: 'button', class: 'set-link', dataset: { fkey: 'set:prov:limits_link' }, onclick: () => openExternal(ANTHROPIC_RATE_LIMITS_URL) }, t('settings.provider.anthropic_limits_link'), ' ', icon('external-link', { size: 12 })),
                ]
              : []),
          );
        };
        const balance = h(
          'select',
          { class: 'select', value: p ? p.balance_api : 'none', onchange: () => (balanceTouched = true) },
          h('option', { value: 'none' }, t('settings.provider.balance_none')),
          h('option', { value: 'deepseek' }, t('settings.provider.balance_deepseek')),
        );
        const priceInput = (v, label) => h('input', { class: 'input input-num', type: 'number', min: '0', step: 'any', value: v == null ? '' : String(v), placeholder: '—', 'aria-label': label });
        const pIn = priceInput(p && p.price_input, t('settings.provider.price_input_label'));
        const pHit = priceInput(p && p.price_cache_hit, t('settings.provider.price_hit_label'));
        const pOut = priceInput(p && p.price_output, t('settings.provider.price_output_label'));
        const cur = h('input', { class: 'input set-cur', value: p && p.currency ? p.currency : '', placeholder: 'CNY', list: 'set-currencies', maxlength: '8', autocomplete: 'off', 'aria-label': t('tariff.editor.currency') });
        const err = h('p', { class: 'dlg-field-error set-dlg-error', role: 'alert' });
        const submitText = () => (isNew ? t('settings.provider.create') : t('common.save'));
        const submit = h('button', { type: 'submit', class: 'btn btn-primary' }, submitText());

        // v2 P12: time-of-day pricing; a new DeepSeek provider gets the
        // back end's preset when the user does not fill one in
        const isDeepSeek = () => /(^|\.)deepseek\.com$/.test(hostOf(base.value.trim())) || balance.value === 'deepseek';
        const trf = tariffEditor({ tariff: p && p.tariff && typeof p.tariff === 'object' ? p.tariff : null, offpeakOnly: p ? p.offpeak_only : false, isNew, deepseek: isDeepSeek });
        balance.addEventListener('change', () => trf.sync());

        const autoBalance = () => {
          if (!balanceTouched) balance.value = hostOf(base.value.trim()) === 'api.deepseek.com' ? 'deepseek' : 'none';
        };
        // AUD1 I-3: plain http to a computer off the local network (a note;
        // saving is not blocked), following the address typed
        const plainNote = h('span', { class: 'dlg-hint span-2 pv-plain-http', role: 'status', id: 'pv-url-plain' });
        // round 9 WV-07 (R87 S-L2): a live region, so it changes only when
        // the address goes near or far (not on every key typed: a screen
        // reader could read the same sentence again each time)
        let plainFar = null;
        const syncPlainNote = () => {
          const far = !!plainHTTPFar(base.value);
          if (far === plainFar) return;
          plainFar = far;
          plainNote.hidden = !far;
          plainNote.replaceChildren(...(far ? [icon('lock-open', { size: 12 }), ' ', t('settings.provider.plain_http_note')] : []));
          if (far) base.setAttribute('aria-describedby', 'pv-url-plain');
          else base.removeAttribute('aria-describedby');
        };
        syncPlainNote();
        base.addEventListener('input', () => {
          autoBalance();
          syncKeyHint();
          syncPlainNote();
          trf.sync();
          typeNote.replaceChildren(...endpointNote(type));
          syncLimitsNote(); // R38b N1: the limits only for Anthropic's own host
        });
        key.addEventListener('input', () => {
          syncKeyHint();
          syncLimitsNote();
        });
        const keyHint = h('span', { class: 'dlg-hint', id: 'pv-key-hint' });
        // sec2-app L2: moving an existing provider to another host clears
        // its stored key on save (the back end never sends it there)
        const movedHint = h('span', { class: 'dlg-hint tone-warn', role: 'status', id: 'pv-key-moved' });
        // sec3 L2: no data key now, so a key entered here is stored in the clear
        const storeHint = data && data.credentials_error_info ? h('span', { class: 'dlg-hint tone-warn', role: 'status', id: 'pv-key-store' }, icon('lock-open', { size: 12 }), ' ', U.errorInfoText(data.credentials_error_info)) : null;
        // round 2 (R28 S3, N35): the saved key cannot be decrypted; say so
        // where the key is re-entered, with "check the address first",
        // visible (not only a tooltip) and read with the key field
        const unreadHint = p && p.key_unreadable ? h('span', { class: 'dlg-hint tone-warn', id: 'pv-key-unreadable' }, icon('key-round', { size: 12 }), ' ', t('errors.settings.provider_key_unreadable', { provider: p.name })) : null;
        // round 2 W4 (R28): the key field is named by its label alone and
        // described by the hints under it, which sit outside the label: in
        // it they were part of the name, the unreadable key's a second time
        // as the description
        key.setAttribute('id', 'pv-key');
        key.setAttribute('aria-describedby', [unreadHint, keyHint, movedHint, storeHint].filter(Boolean).map((x) => x.getAttribute('id')).join(' '));
        // WV-07: the texts change only when they do (movedHint is a live region)
        let movedNow = null;
        const syncKeyHint = () => {
          const hint = isLocal(base.value.trim()) ? t('settings.provider.key_hint_local') : t('settings.provider.key_hint');
          if (keyHint.textContent !== hint) keyHint.textContent = hint;
          const moved = !!(p && p.key_hint && !key.value.trim() && !sameOrigin(p.base_url, base.value.trim()));
          if (moved === movedNow) return;
          movedNow = moved;
          movedHint.hidden = !moved;
          movedHint.textContent = moved ? t('settings.provider.key_moved_hint') : '';
        };
        syncKeyHint();
        // where the requests go: the address as typed, in code style
        // (3.2.0 L18; it was the literal "{base_url}")
        const endpointNote = (v) => {
          const mark = '\uE000';
          const [before, address, after] = endpointParts(t(v === 'anthropic' ? 'settings.provider.endpoint_anthropic' : 'settings.provider.endpoint_openai', { base_url: mark }), mark, base.value);
          return address === undefined ? [before] : [before, h('code', { translate: 'no' }, address), after];
        };

        const typeSeg = segmented(
          'dlg-ptype',
          ['openai', 'anthropic'].map((v) => [v, typeName(v), t(`settings.provider.type_hint.${v}`)]),
          type,
          (v) => {
            const prevDefault = defaults[type] || '';
            type = v;
            const lim = limitsPrefill(v);
            if (!limitsTouched.conc) conc.value = String(lim.max_concurrency);
            if (!limitsTouched.rpm) rpm.value = String(lim.rpm);
            // Prefill the usual address unless the user typed their own (A5.3).
            const now = base.value.trim().replace(/\/+$/, '');
            if (!now || now === prevDefault.replace(/\/+$/, '')) base.value = defaults[v] || '';
            autoBalance();
            syncKeyHint();
            // after the address: back on Anthropic's it shows the limits again (T11)
            syncLimitsNote();
            typeNote.replaceChildren(...endpointNote(v));
          },
          t('settings.provider.type_label'),
        );
        const typeNote = h('span', { class: 'dlg-hint' }, endpointNote(type));
        syncLimitsNote();

        const fail = (msg, input) => {
          err.textContent = msg;
          for (const i of [name, base, conc, rpm, pIn, pHit, pOut]) i.removeAttribute('aria-invalid');
          if (input) {
            input.setAttribute('aria-invalid', 'true');
            input.focus();
          }
        };

        const onSubmit = async (e) => {
          e.preventDefault();
          if (submit.disabled) return;
          const nm = name.value.trim();
          const url = base.value.trim();
          const c = num(conc.value);
          const r = num(rpm.value);
          const prices = [pIn, pHit, pOut].map((i) => num(i.value));
          const filled = prices.filter((v) => !isNaN(v)).length;
          if (!nm) return fail(t('errors.settings.provider_name_empty'), name);
          if (!/^https?:\/\/[^/\s]+/i.test(url)) return fail(t('errors.settings.base_url_invalid'), base);
          if (!isInt(c) || c < 1 || c > MAX_CONCURRENCY) return fail(t('errors.settings.max_concurrency_invalid', { min: 1, max: MAX_CONCURRENCY }), conc);
          if (!isInt(r) || r < 0 || r > MAX_RPM) return fail(t('errors.settings.rpm_invalid', { min: 0, max: MAX_RPM }), rpm);
          if (prices.some((v) => v < 0)) return fail(t('errors.settings.price_invalid'), [pIn, pHit, pOut][prices.findIndex((v) => v < 0)]);
          if (filled && filled < 3) return fail(t('settings.provider.err.prices_partial'), [pIn, pHit, pOut][prices.findIndex((v) => isNaN(v))]);
          const tmsg = trf.validate();
          if (tmsg) return fail(tmsg);
          const body = { name: nm, type, base_url: url, max_concurrency: c, rpm: r, balance_api: balance.value };
          const k = key.value.trim();
          if (k) body.api_key = k;
          if (filled === 3) {
            [body.price_input, body.price_cache_hit, body.price_output] = prices;
            body.currency = cur.value.trim();
          } else if (p && (p.price_input != null || p.price_cache_hit != null || p.price_output != null)) {
            body.clear_prices = true;
            body.currency = '';
          } else if (cur.value.trim() || (p && p.currency)) {
            body.currency = cur.value.trim();
          }
          const tv = trf.value();
          if (tv.tariff !== undefined) body.tariff = tv.tariff; // null clears it
          if (isNew ? tv.offpeak_only : tv.offpeak_only !== !!p.offpeak_only) body.offpeak_only = tv.offpeak_only;
          fail('');
          submit.disabled = true;
          submit.textContent = isNew ? t('settings.provider.creating') : t('settings.saving');
          try {
            const res = await busy(isNew ? api.createProvider(body) : api.patchProvider(p.id, body));
            key.value = '';
            close(res || true);
          } catch (ex) {
            submit.disabled = false;
            submit.textContent = submitText();
            fail(ex.message || t('settings.save_failed'));
          }
        };

        // WV-06: what the dialog opened with (prefills included)
        const fieldsNow = () => JSON.stringify([name.value, base.value, key.value !== '', type, conc.value, rpm.value, pIn.value, pHit.value, pOut.value, cur.value, balance.value, trf.value()]);
        const opened = fieldsNow();
        edited = () => fieldsNow() !== opened;
        saving = () => submit.disabled;
        let asking = false;
        const cancel = async () => {
          if (asking) return;
          asking = true;
          try {
            if (await mayClose()) close(null);
          } finally {
            asking = false;
          }
        };

        return h(
          'form',
          { class: 'set-dlg', onsubmit: onSubmit, novalidate: true },
          h(
            'div',
            { class: 'set-dlg-grid' },
            field(t('settings.provider.name'), name, null, 'span-2'),
            h('div', { class: 'dlg-field span-2' }, h('span', { class: 'dlg-label', id: 'dlg-ptype-l' }, t('settings.provider.type')), typeSeg, typeNote),
            field('base_url', base, t('settings.provider.base_url_hint'), 'span-2'),
            plainNote,
          ),
          h(
            'div',
            { class: 'set-dlg-grid' },
            h('div', { class: 'dlg-field span-2' }, h('label', { class: 'dlg-label', for: 'pv-key' }, icon('key-round', { size: 13 }), ' ', t('settings.provider.key')), key, unreadHint, keyHint, movedHint, storeHint),
            field(t('settings.provider.max_concurrency'), conc, `1–${MAX_CONCURRENCY}`),
            field(t('settings.provider.rpm_field'), rpm, t('settings.provider.rpm_range_hint', { max: MAX_RPM })),
            observed.el,
            limitsNote,
            field(t('settings.provider.balance_api'), balance, t('settings.provider.balance_api_hint'), 'span-2'),
          ),
          h(
            'fieldset',
            { class: 'set-dlg-prices' },
            h('legend', { class: 'dlg-label' }, t('settings.provider.prices_legend')),
            h(
              'div',
              { class: 'set-price-grid' },
              h('label', { class: 'set-price' }, h('span', { class: 'dlg-hint' }, t('tariff.editor.price_input')), pIn),
              h('label', { class: 'set-price' }, h('span', { class: 'dlg-hint' }, t('tariff.editor.price_hit')), pHit),
              h('label', { class: 'set-price' }, h('span', { class: 'dlg-hint' }, t('tariff.editor.price_output')), pOut),
              h('label', { class: 'set-price' }, h('span', { class: 'dlg-hint' }, t('tariff.editor.currency')), cur),
            ),
            h('span', { class: 'dlg-hint' }, t('settings.provider.prices_hint')),
            h('datalist', { id: 'set-currencies' }, CURRENCIES.map((c) => h('option', { value: c }))),
          ),
          trf.el,
          err,
          h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: cancel }, t('common.cancel')), submit),
        );
      },
      { wide: true, className: 'dlg-provider', confirmCancel: mayClose },
    ).then((res) => {
      if (!res || !alive) return;
      if (res.key_unprotected) toast(keyProblem(res), 'warn'); // sec3 L2
      if (isNew && res.id) {
        toast(t('settings.provider.created', { name: res.name }), 'success');
        load().then(() => {
          const np = providerOf(res.id);
          if (np) probe(np);
        });
      } else {
        toast(t('settings.provider.saved', { name: res.name || p.name }), 'success');
        load();
      }
    });
  }

  // ---- roles ----

  function rolesBlock(shell) {
    const broken = data.roles.filter(roleBroken).length;
    shell.classList.toggle('has-error', broken > 0);
    shell.append(
      head('bot', t('settings.block.roles'), broken ? h('span', { class: 'chip set-chip-err' }, t('settings.roles.broken_count', { n: broken })) : null),
      h(
        'div',
        { class: 'card-bd' },
        h('p', { class: 'set-intro' }, t('settings.roles.intro')),
        data.providers.length ? null : h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 14 }), t('settings.roles.no_providers')),
        h(
          'div',
          { class: 'rl' },
          h('div', { class: 'rl-head', 'aria-hidden': 'true' }, ['role', 'provider', 'model', 'temperature', 'thinking', 'window'].map((c) => h('span', null, t(`settings.roles.col.${c}`)))),
          data.roles.map(roleRow),
        ),
      ),
      foot('roles', saveRoles),
    );
  }

  function roleRow(r) {
    const v = roleVal(r);
    const prov = providerOf(v.provider_id);
    const isDirty = roleDirty(r);
    const rowErr = rowErrors[r.role];
    const optional = OPTIONAL_ROLES.has(r.role);
    const name = roleNameOf(r);
    // an optional role without a usable value falls back (G2): a note, not an error;
    // while nothing is set up (transcription only) no role is an error either
    const quiet = transcribeOnly(data.roles);
    const serverErr = !isDirty && !r.configured && !optional && !quiet ? (r.error_info ? U.errorInfoText(r.error_info, r.error) : r.error) : '';
    const fallback = !isDirty && !r.configured && optional;
    const unsetQuiet = !isDirty && !r.configured && !optional && quiet;
    const fk = (x) => ({ fkey: `set:role:${r.role}:${x}` });

    // provider
    const provSel = h(
      'select',
      {
        class: 'select',
        'aria-label': t('settings.roles.provider_label', { role: name }),
        dataset: fk('provider'),
        onchange: (e) => {
          const np = providerOf(e.target.value);
          const keep = np && np.models.includes(v.model.trim()) ? v.model : '';
          manualRoles.delete(r.role);
          // (thinking: a provider without it shows and saves "off", see thinkingOf)
          setRoleDraft(r, { provider_id: e.target.value, model: keep }, true);
        },
      },
      !v.provider_id ? h('option', { value: '', disabled: true, selected: true }, t('settings.roles.pick_provider')) : null,
      v.provider_id && !prov ? h('option', { value: v.provider_id, selected: true }, t('settings.roles.deleted_provider')) : null,
      data.providers.map((p) => h('option', { value: p.id, selected: p.id === v.provider_id }, p.name)),
    );

    // model: the probed list, or typed by hand
    const models = prov ? prov.models : [];
    const inList = models.includes(v.model.trim());
    // a model the provider no longer lists may have been retired (deepseek-chat):
    // a light warning on the row; nothing is said while the list is empty or not probed.
    // Listed: by name, or as an alias of a dated name ("claude-sonnet-4-5"
    // for "claude-sonnet-4-5-20250929", "-latest"): util.js modelListed, the
    // lecture page's rule and the back end's (R79 L12: one copy)
    const unlisted = !!prov && models.length > 0 && v.model.trim() !== '' && !U.modelListed(v.model, models);
    const unlistedTip = unlisted ? t('settings.roles.model_unlisted', { provider: prov.name, model: v.model.trim() }) : '';
    const manual = manualRoles.has(r.role) || !models.length || (v.model.trim() !== '' && !inList);
    let modelCtl;
    if (!manual) {
      modelCtl = h(
        'select',
        {
          class: 'select mono',
          'aria-label': t('settings.roles.model_label', { role: name }),
          dataset: fk('model'),
          onchange: (e) => {
            if (e.target.value === MANUAL) {
              manualRoles.add(r.role);
              renderBlock('roles');
              const inp = shells.roles.querySelector(`[data-fkey="set:role:${r.role}:model"]`);
              if (inp) inp.focus();
              return;
            }
            setRoleDraft(r, { model: e.target.value });
          },
        },
        !v.model ? h('option', { value: '', disabled: true, selected: true }, t('settings.roles.pick_model')) : null,
        models.map((m) => h('option', { value: m, selected: m === v.model.trim() }, m)),
        h('option', { value: MANUAL }, t('settings.roles.manual')),
      );
    } else {
      modelCtl = h('input', {
        class: 'input mono',
        value: v.model,
        placeholder: t('settings.example', { value: prov && prov.type === 'anthropic' ? (r.role === 'glossary_janitor' ? 'claude-opus-5-5' : 'claude-sonnet-4-5') : 'deepseek-flash' }),
        spellcheck: 'false',
        autocomplete: 'off',
        'aria-label': t('settings.roles.model_name_label', { role: name }),
        dataset: fk('model'),
        oninput: (e) => setRoleDraft(r, { model: e.target.value }),
      });
    }
    let modelNote = null;
    if (prov && models.length && manual) {
      // (the list itself offers "enter by hand" as its last option)
      modelNote = h(
        'span',
        { class: 'rl-sub' },
        v.model.trim() && !inList ? `${t('settings.roles.not_in_list')} · ` : null,
        h(
          'button',
          {
            type: 'button',
            class: 'set-link',
            dataset: fk('mode'),
            onclick: () => {
              manualRoles.delete(r.role);
              if (!inList) setRoleDraft(r, { model: '' });
              renderBlock('roles');
              const sel = shells.roles.querySelector(`[data-fkey="set:role:${r.role}:model"]`);
              if (sel) sel.focus();
            },
          },
          t('settings.roles.from_list', { n: models.length }),
        ),
      );
    } else if (prov && !models.length) {
      modelNote = h(
        'span',
        { class: 'rl-sub' },
        probing.has(prov.id)
          ? [icon('loader-circle', { size: 12, class: 'spin' }), ' ', t('settings.provider.models_probing')]
          : [
              `${prov.models_error || prov.models_error_info ? t('settings.roles.probe_failed') : t('settings.roles.not_probed')} · `,
              h('button', { type: 'button', class: 'set-link', dataset: fk('probe'), onclick: () => probe(prov) }, prov.models_error || prov.models_error_info ? t('settings.roles.reprobe') : t('settings.provider.probe')),
            ],
      );
    }

    // temperature
    const tVal = String(v.temperature);
    const anthropic = prov && prov.type === 'anthropic';
    const range = h('input', {
      type: 'range',
      class: 'set-range' + (anthropic ? ' is-muted' : ''),
      min: '0',
      max: '2',
      step: '0.05',
      value: isNaN(num(tVal)) ? '0' : tVal,
      'aria-label': t('settings.roles.temp_label', { role: name }),
      dataset: fk('trange'),
      oninput: (e) => {
        tNum.value = e.target.value;
        setRoleDraft(r, { temperature: e.target.value });
      },
    });
    const tNum = h('input', {
      type: 'number',
      class: 'input input-num rl-tnum',
      min: '0',
      max: '2',
      step: '0.05',
      value: tVal,
      'aria-label': t('settings.roles.temp_value_label', { role: name }),
      dataset: fk('temp'),
      oninput: (e) => {
        if (!isNaN(num(e.target.value))) range.value = e.target.value;
        setRoleDraft(r, { temperature: e.target.value });
      },
    });

    // thinking: off / low / high / max, on a provider that supports it
    // (supports_thinking); disabled with a hover note otherwise
    const canThink = !!prov && prov.supports_thinking;
    const thinkVal = thinkingOf(v);
    const thinkOffTip = canThink ? '' : prov ? t('settings.roles.thinking_unsupported', { provider: prov.name }) : t('settings.roles.thinking_no_provider');
    // "cannot switch thinking off" (syncThinkFloor keeps it current)
    const floorNote = floorNoteOf(v);
    const thinkSel = h(
      'select',
      {
        class: 'select',
        disabled: !canThink,
        'aria-label': t('settings.roles.thinking_label', { role: name }),
        'aria-description': thinkOffTip || undefined,
        // the full note under the row (T11 detail 6: it was only a tooltip here)
        'aria-describedby': floorNote ? floorId(r) : undefined,
        dataset: { ...fk('thinking'), level: thinkVal },
        onchange: (e) => chooseThinking(r, e.target),
      },
      U.THINKING_LEVELS.map((lv) => h('option', { value: lv, selected: lv === thinkVal }, thinkingName(lv))),
    );
    const thinkCell = h(
      'div',
      { class: 'rl-cell rl-think' },
      h('span', { class: 'rl-cap' }, t('settings.roles.col.thinking')),
      h('span', { class: 'rl-think-ctl' + (canThink ? '' : ' tip is-off'), 'data-tip': thinkOffTip || undefined }, thinkSel),
      floorNote ? floorShort(r, floorNote) : null,
    );

    // v3.2.x D1: the output limit; empty = automatic (by role and thinking level)
    const errField = rowErrorFields[r.role];
    // round 2 W4 (T6 #9): the long explanations under the output limit and
    // the batch size describe their fields (aria-describedby). The fields
    // are named by aria-label, so a hint inside the label is not part of
    // the name: each explanation is heard once, as the description.
    const hintId = (what) => `rl-${r.role}-${what}-hint`;
    // T18: the row's error describes the field it is about, first
    const errFirst = (field, ids) => (rowErr && errField === field ? `${roleErrId(r.role)} ${ids}` : ids);
    const maxIn = h('input', {
      type: 'number',
      class: 'input input-num rl-max-in',
      min: '1',
      max: String(MAX_TOKENS_LIMIT),
      step: '1',
      value: v.max_tokens,
      placeholder: t('settings.roles.max_tokens_auto'),
      'aria-label': t('settings.roles.max_tokens_label', { role: name }),
      'aria-describedby': errFirst('max_tokens', hintId('max_tokens')),
      'aria-invalid': errField === 'max_tokens' ? 'true' : undefined,
      dataset: fk('max_tokens'),
      oninput: (e) => setRoleDraft(r, { max_tokens: e.target.value }),
    });
    const extras = [
      h(
        'label',
        { class: 'rl-extra-field' },
        h('span', { class: 'rl-extra-title' }, t('settings.roles.max_tokens')),
        maxIn,
        h('span', { class: 'rl-sub', id: hintId('max_tokens') }, t('settings.roles.max_tokens_hint')),
      ),
    ];
    // v3.2.x P2: sentences per translate request. It is new and sits folded
    // away with an "experimental" chip (the value is kept and saved with the
    // role). Empty means the default of the provider and model
    // (defaultBatch: 10 for a Claude model on an Anthropic provider since
    // 2026-11-11, 1 otherwise, one request per sentence as before). The
    // empty field holds the number only (a 72px field cuts longer text,
    // review R24); "Default N" stands next to it and, when above 1, as a
    // chip on the folded summary (syncBatch keeps them current).
    if (r.role === 'translate') {
      const bv = batchView(v);
      const defBatch = bv.defBatch;
      const batchIn = h('input', {
        type: 'number',
        class: 'input input-num rl-batch-in',
        min: '1',
        max: String(MAX_BATCH),
        step: '1',
        value: v.batch,
        placeholder: String(defBatch),
        'aria-label': t('settings.roles.batch_label'),
        // the default ("Default 10", kept current by syncBatch) and the explanation
        'aria-describedby': errFirst('batch', `${hintId('batch_default')} ${hintId('batch')}`),
        'aria-invalid': errField === 'batch' ? 'true' : undefined,
        dataset: fk('batch'),
        oninput: (e) => setRoleDraft(r, { batch: e.target.value }),
      });
      extras.push(
        h(
          'details',
          {
            class: 'rl-batch',
            open: errField === 'batch' || batchOpen.has(r.role) || undefined,
            dataset: fk('batch_box'),
            ontoggle: (e) => (e.target.open ? batchOpen.add(r.role) : batchOpen.delete(r.role)),
          },
          h(
            'summary',
            { class: 'rl-extra-title' },
            t('settings.roles.batch_summary'),
            ' ',
            h('span', { class: 'chip chip-quiet' }, t('settings.roles.batch_experimental')),
            h('span', { class: 'chip chip-quiet', hidden: !bv.showDefault, dataset: fk('batch_default_chip') }, t('settings.roles.batch_default', { n: defBatch })),
            h('span', { class: 'chip chip-quiet', hidden: !bv.showValue, dataset: fk('batch_value_chip') }, t('settings.roles.batch_value_chip', { value: bv.setBatch })),
          ),
          h(
            'div',
            { class: 'rl-batch-body' },
            h(
              'span',
              { class: 'rl-batch-ctl' },
              batchIn,
              h('span', { class: 'dim' }, `1–${MAX_BATCH}`),
              h('span', { class: 'dim', id: hintId('batch_default'), dataset: fk('batch_default') }, t('settings.roles.batch_default', { n: defBatch })),
            ),
            h('span', { class: 'rl-sub', id: hintId('batch') }, t('settings.roles.batch_hint')),
          ),
        ),
      );
    }
    const extraCell = h('div', { class: 'rl-extra' }, extras);

    const winCell =
      r.role === 'translate'
        ? h(
            'div',
            { class: 'rl-cell rl-win' },
            h('span', { class: 'rl-cap' }, t('settings.roles.col.window')),
            h(
              'span',
              { class: 'rl-win-ctl' },
              h('input', {
                type: 'number',
                class: 'input input-num',
                min: '0',
                max: '10',
                step: '1',
                value: v.window,
                'aria-label': t('settings.roles.window_label'),
                title: t('settings.roles.window_tip'),
                dataset: fk('window'),
                oninput: (e) => setRoleDraft(r, { window: e.target.value }),
              }),
              h('span', { class: 'dim' }, t('settings.roles.window_unit')),
            ),
          )
        : h('div', { class: 'rl-cell rl-win is-empty', 'aria-hidden': 'true' }, h('span', { class: 'dim' }, '—'));

    // the back end's advice line (G3.1), from the catalog when it has one
    const advice = r.hint ? (has(`settings.role_advice.${r.role}`) ? t(`settings.role_advice.${r.role}`) : r.hint) : '';
    const notes = [];
    if (rowErr) notes.push(h('p', { class: 'rl-msg is-err', role: 'alert', id: roleErrId(r.role) }, icon('circle-alert', { size: 13 }), rowErr));
    else if (serverErr) notes.push(h('p', { class: 'rl-msg is-err' }, icon('circle-alert', { size: 13 }), serverErr));
    else if (fallback) notes.push(h('p', { class: 'rl-msg' + (v.provider_id ? ' is-warn' : '') }, icon(v.provider_id ? 'triangle-alert' : 'info', { size: 13 }), v.provider_id && !prov ? t('settings.roles.provider_deleted_note', { note: optionalNote(r.role) }) : optionalNote(r.role)));
    if (advice) notes.push(h('p', { class: 'rl-msg rl-hint' }, icon('sparkles', { size: 13 }), advice));
    if (prov && needsKey(prov)) notes.push(h('p', { class: 'rl-msg is-warn' }, icon('key-round', { size: 13 }), t('settings.roles.provider_no_key', { name: prov.name })));
    if (anthropic) notes.push(h('p', { class: 'rl-msg' }, icon('info', { size: 13 }), t('settings.roles.anthropic_temp_note')));
    if (floorNote) notes.push(floorMsg(r, floorNote));

    return h(
      'div',
      { class: 'rl-row' + (serverErr || rowErr ? ' is-error' : '') + (isDirty ? ' is-dirty' : ''), role: 'group', 'aria-label': name, dataset: { role: r.role } },
      h(
        'div',
        { class: 'rl-cell rl-role' },
        h(
          'span',
          { class: 'rl-name' },
          name,
          h('span', { class: 'set-dirty-dot', title: t('settings.roles.unsaved_tip') }),
          serverErr ? h('span', { class: 'chip set-chip-err' }, prov || !v.provider_id ? t('settings.not_configured') : t('settings.roles.provider_deleted_chip')) : null,
          fallback || unsetQuiet ? h('span', { class: 'chip chip-quiet' }, t('settings.not_configured')) : null,
          r.default && !isDirty ? h('span', { class: 'chip rl-default', title: t('settings.roles.default_tip') }, t('settings.roles.default_chip')) : null,
          unlisted ? h('span', { class: 'rl-unlisted tip', tabindex: '0', role: 'img', 'aria-label': unlistedTip, 'data-tip': unlistedTip, dataset: fk('unlisted') }, icon('triangle-alert', { size: 13 })) : null,
        ),
        h('span', { class: 'rl-sub' }, roleHint(r.role)),
      ),
      h('div', { class: 'rl-cell rl-prov' }, h('span', { class: 'rl-cap' }, t('settings.roles.col.provider')), provSel),
      h('div', { class: 'rl-cell rl-model' }, h('span', { class: 'rl-cap' }, t('settings.roles.col.model')), modelCtl, modelNote),
      h('div', { class: 'rl-cell rl-temp' }, h('span', { class: 'rl-cap' }, t('settings.roles.col.temperature')), h('span', { class: 'rl-temp-ctl' }, range, tNum)),
      thinkCell,
      winCell,
      extraCell,
      notes.length ? h('div', { class: 'rl-notes' }, notes) : null,
    );
  }

  // A thinking level was picked in a role's select. Turning it on (from off)
  // for translate or repair asks first; "cancel" puts the select back.
  async function chooseThinking(r, sel) {
    const before = sel.dataset.level || 'off';
    const level = sel.value;
    const ask = before === 'off' && level !== 'off' ? thinkingConfirm(r.role) : null;
    if (ask) {
      const ok = await confirmDialog({ ...ask, okText: t('settings.roles.thinking_confirm.ok'), cancelText: t('common.cancel'), focusCancel: true });
      if (!alive) return;
      if (!ok) {
        sel.value = before;
        return;
      }
    }
    sel.dataset.level = level;
    // (a background refresh may have re-rendered the table while the
    // dialog was open: then the new select must show the choice too)
    const cur = data.roles.find((x) => x.role === r.role) || r;
    setRoleDraft(cur, { thinking: level }, !sel.isConnected);
  }

  function validateRole(r, d) {
    const role = roleNameOf(r);
    if (!d.provider_id || !providerOf(d.provider_id)) return t('errors.settings.role_provider_missing', { role });
    if (!d.model.trim()) return t('errors.settings.role_model_missing', { role });
    const temp = num(d.temperature);
    if (isNaN(temp) || temp < 0 || temp > 2) return t('errors.settings.temperature_invalid', { min: 0, max: 2 });
    if (r.role === 'translate') {
      const w = num(d.window);
      if (!isInt(w) || w < 0 || w > 10) return t('settings.roles.err.window');
    }
    return validateRoleLimits(r, d).msg;
  }
  // v3.2.x: max_tokens empty or 1–MAX_TOKENS_LIMIT; batch (translate) empty
  // or 1–MAX_BATCH. {msg, field}
  // T18 M4: the retry dialog's rule (parseMaxTokens), with the field's
  // badInput when the row is on the page ("12e" reads as empty)
  function maxTokensBad(d, r) {
    const inp = r && shells.roles ? shells.roles.querySelector(`[data-fkey="set:role:${r.role}:max_tokens"]`) : null;
    return Number.isNaN(parseMaxTokens(d.max_tokens, !!(inp && inp.validity && inp.validity.badInput)));
  }
  function validateRoleLimits(r, d) {
    if (maxTokensBad(d, r)) return { msg: t('errors.settings.max_tokens_invalid', { min: 1, max: MAX_TOKENS_LIMIT }), field: 'max_tokens' };
    const b = String(d.batch || '').trim();
    if (r.role === 'translate' && b !== '' && (!isInt(num(b)) || num(b) < 1 || num(b) > MAX_BATCH)) return { msg: t('errors.settings.batch_invalid', { min: 1, max: MAX_BATCH }), field: 'batch' };
    return { msg: '', field: '' };
  }

  async function saveRoles() {
    if (saving.has('roles')) return;
    const list = data.roles.filter(roleDirty);
    if (!list.length) return;
    for (const r of list) {
      const msg = validateRole(r, drafts.roles[r.role]);
      if (msg) {
        rowErrors[r.role] = msg;
        const lim = validateRoleLimits(r, drafts.roles[r.role]);
        if (lim.msg === msg) rowErrorFields[r.role] = lim.field;
        renderBlock('roles');
        return;
      }
    }
    saving.add('roles');
    syncFoot('roles');
    let failed = false;
    for (const r of list) {
      const d = drafts.roles[r.role];
      if (!d) continue;
      const params = { temperature: num(d.temperature), thinking: thinkingOf(d) };
      if (r.role === 'translate') params.window = num(d.window);
      // v3.2.x: an empty field is left out, which clears the value
      if (String(d.max_tokens || '').trim() !== '') params.max_tokens = num(d.max_tokens);
      if (r.role === 'translate' && String(d.batch || '').trim() !== '') params.batch = num(d.batch);
      try {
        const s = await api.setRole(r.role, d.provider_id, d.model.trim(), params);
        delete drafts.roles[r.role];
        manualRoles.delete(r.role);
        if (s && alive) {
          data = s;
          dataKey = JSON.stringify(s);
        }
      } catch (e) {
        rowErrors[r.role] = e.message;
        if (e.code === 'settings.max_tokens_invalid') rowErrorFields[r.role] = 'max_tokens';
        else if (e.code === 'settings.batch_invalid') rowErrorFields[r.role] = 'batch';
        blockErrors.roles = t('settings.roles.save_failed', { role: roleNameOf(r), error: e.message });
        failed = true;
        break;
      }
    }
    saving.delete('roles');
    if (!alive) return;
    pruneDrafts();
    render();
    if (!failed) toast(t('settings.roles.saved'), 'success');
  }

  // ---- transcription ----

  function segmented(name, options, value, onChange, label) {
    return h(
      'div',
      { class: 'set-seg', role: 'radiogroup', 'aria-label': label },
      options.map(([v, text, hint]) =>
        h(
          'label',
          { class: 'set-seg-opt', title: hint || undefined },
          h('input', { type: 'radio', name, value: v, checked: v === value, dataset: { fkey: `${name}:${v}` }, onchange: () => onChange(v) }),
          h('span', null, text),
        ),
      ),
    );
  }

  function formRow(label, control, hint, extra) {
    return h('div', { class: 'set-row' }, h('div', { class: 'set-row-label' }, h('span', { class: 'set-row-title' }, label), hint ? h('span', { class: 'set-row-hint' }, hint) : null), h('div', { class: 'set-row-ctl' }, control, extra));
  }

  // ---- transcription (SPEC-v2 I6-I9) ----

  // On a machine with a battery the power-aware rules apply (resources
  // policy; the power state before the first snapshot with a policy).
  const batteryMachine = () => U.batteryMachine(ctx.resources());

  // "转写设备": automatic (power-aware) or the devices ticked by hand.
  function setDeviceMode(mode) {
    const a = asrVal();
    if (mode === a.device_mode) return;
    if (mode !== MANUAL_MODE || a.manual_devices.length) {
      setASRDraft({ device_mode: mode }, true);
      return;
    }
    // start the list with the device in use now (never a CPU device)
    const r = ctx.resources();
    const devs = data.devices || [];
    const first = ((r && r.policy && r.policy.active) || []).find((id) => devs.some((d) => d.id === id && d.kind !== 'cpu'));
    setASRDraft({ device_mode: mode, manual_devices: first ? [first] : [] }, true);
  }

  // Ticking a second device on a laptop warns first (power-req 6, the
  // user's wording): the box stays unticked while the question is open and
  // is ticked only when the user goes on; cancel leaves it unticked.
  async function toggleManual(d, input) {
    const a = asrVal();
    const rest = a.manual_devices.filter((id) => id !== d.id);
    if (!input.checked) {
      setASRDraft({ manual_devices: rest }, true);
      return;
    }
    if (rest.length === 1 && batteryMachine()) {
      input.checked = false;
      const ok = await confirmDialog({ title: t('settings.asr.manual_multi_title'), message: t('settings.asr.manual_multi_warn'), okText: t('common.continue') });
      if (!alive || !ok) return;
      // ticked again from the draft below (the list may have changed meanwhile)
      const now = asrVal().manual_devices.filter((id) => id !== d.id);
      setASRDraft({ manual_devices: sortByPriority([...now, d.id]) }, true);
      return;
    }
    setASRDraft({ manual_devices: sortByPriority([...rest, d.id]) }, true);
  }

  // The devices that may be chosen by hand (never a CPU device), with
  // their state; on a laptop the note that the choice is not used on
  // battery, and the warning while several are ticked.
  function manualPicker(a) {
    const devs = (data.devices || []).filter((d) => d.kind !== 'cpu');
    const battery = batteryMachine();
    const box = h('div', { class: 'set-manual' });
    if (!devs.length) {
      box.append(h('p', { class: 'set-note' }, icon('info', { size: 13 }), h('span', null, t('devices.empty_title'))));
      return box;
    }
    box.append(
      h(
        'div',
        { class: 'set-checks', role: 'group', 'aria-label': t('settings.asr.manual_label') },
        devs.map((d) =>
          h(
            'label',
            { class: 'set-check' + (d.enabled && d.present ? '' : ' is-muted') },
            h('input', { type: 'checkbox', checked: a.manual_devices.includes(d.id), dataset: { fkey: `set:asr:manual:${d.id}` }, onchange: (e) => toggleManual(d, e.target) }),
            h('span', { class: 'set-check-name', title: d.label }, d.label),
            h('span', { class: 'set-check-meta' }, [U.deviceKindName(d.kind), U.backendName(d)].filter(Boolean).join(' · ')),
            !d.present ? h('span', { class: 'chip chip-warn', title: t('devices.not_present_tip') }, t('devices.not_present')) : null,
            !d.enabled ? h('span', { class: 'chip chip-quiet' }, t('devices.disabled')) : null,
          ),
        ),
      ),
    );
    if (battery) box.append(h('p', { class: 'set-note' }, icon('battery-medium', { size: 13 }), h('span', null, t('settings.asr.manual_battery_note'))));
    if (battery && a.manual_devices.length > 1) box.append(h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 13 }), h('span', null, t('settings.asr.manual_multi_warn'))));
    return box;
  }

  function asrGlobalRows() {
    const a = asrVal();
    const fk = (x) => ({ fkey: `set:asr:${x}` });
    return [
      modeUI.row(),
      formRow(
        t('settings.asr.language'),
        h(
          'div',
          { class: 'set-inline' },
          h('input', { class: 'input mono set-w-sm', value: a.language, list: 'set-langs', spellcheck: 'false', autocomplete: 'off', placeholder: 'en', 'aria-label': t('settings.asr.language_label'), dataset: fk('lang'), oninput: (e) => setASRDraft({ language: e.target.value }) }),
          h('span', { class: 'dim small' }, langName(a.language.trim())),
          h('datalist', { id: 'set-langs' }, LANGS.map((c) => h('option', { value: c }, langName(c)))),
        ),
        t('settings.asr.language_hint'),
      ),
      formRow(
        t('settings.asr.device_label'),
        segmented(
          'set:asr:mode',
          [
            [AUTO, t('settings.asr.mode_auto')],
            [MANUAL_MODE, t('settings.asr.mode_manual')],
          ],
          a.device_mode,
          setDeviceMode,
          t('settings.asr.device_label'),
        ),
        batteryMachine() ? t('settings.asr.mode_hint_battery') : t('settings.asr.mode_hint_desktop'),
        a.device_mode === MANUAL_MODE ? manualPicker(a) : null,
      ),
      formRow(
        t('settings.asr.multi_device'),
        switchBtn(a.multi_device, t('settings.asr.multi_device'), () => setASRDraft({ multi_device: !a.multi_device }, true), 'set:asr:multi'),
        // a laptop: one device at a time in automatic mode (devices.Place)
        batteryMachine() ? t('settings.asr.multi_device_hint_battery') : t('settings.asr.multi_device_hint'),
      ),
      // the second decode of degenerate windows (worker/asr.py, faster-whisper and Colab)
      formRow(t('settings.asr.redecode'), switchBtn(a.redecode, t('settings.asr.redecode'), () => setASRDraft({ redecode: !a.redecode }, true), 'set:asr:redecode'), t('settings.asr.redecode_hint')),
      // battery awareness: retired by power management (hidden; kept as it is)
      formRow(
        t('settings.asr.vram_reserve'),
        h(
          'div',
          { class: 'set-inline' },
          h('input', { class: 'input input-num set-w-sm', type: 'number', min: '0', max: '65536', step: '256', value: a.vram_reserve_mb, 'aria-label': t('settings.asr.vram_reserve_label'), dataset: fk('reserve'), oninput: (e) => setASRDraft({ vram_reserve_mb: e.target.value }) }),
          h('span', { class: 'dim small' }, 'MB'),
        ),
        t('settings.asr.vram_reserve_hint'),
      ),
      formRow(
        t('settings.asr.mem_reserve'),
        h(
          'div',
          { class: 'set-inline' },
          h('input', { class: 'input input-num set-w-sm', type: 'number', min: '0', max: '65536', step: '256', value: a.mem_reserve_mb, 'aria-label': t('settings.asr.mem_reserve_label'), dataset: fk('memres'), oninput: (e) => setASRDraft({ mem_reserve_mb: e.target.value }) }),
          h('span', { class: 'dim small' }, 'MB'),
        ),
        t('settings.asr.mem_reserve_hint'),
      ),
    ];
  }

  // The global rows and the save bar are one form; the device list below
  // saves every change at once.
  const asrGlobal = h('div', { class: 'card-bd set-form' });
  function renderASRGlobal() {
    if (!data || !asrGlobal.isConnected) return;
    preserveFocus(() => {
      clear(asrGlobal);
      asrGlobal.append(...asrGlobalRows().filter(Boolean));
    });
  }

  function asrBlock(shell) {
    clear(asrGlobal);
    asrGlobal.append(...asrGlobalRows().filter(Boolean));
    shell.append(head('mic', t('settings.block.asr')), asrGlobal, foot('asr', saveASR), h('div', { class: 'card-bd dev-sec-bd' }, devUI.render(data.devices || [])));
  }

  async function saveASR() {
    if (saving.has('asr') || !dirty('asr')) return;
    const a = drafts.asr;
    const res = num(a.vram_reserve_mb);
    const mem = num(a.mem_reserve_mb);
    let msg = '';
    if (!a.language.trim()) msg = t('errors.settings.language_missing');
    else if (a.device_mode === MANUAL_MODE && !a.manual_devices.length) msg = t('errors.settings.manual_devices_empty');
    else if (!isInt(res) || res < 0 || res > 65536) msg = t('settings.asr.err.vram_reserve');
    else if (!isInt(mem) || mem < 0 || mem > 65536) msg = t('settings.asr.err.mem_reserve');
    if (msg) {
      blockErrors.asr = msg;
      syncFoot('asr');
      return;
    }
    saving.add('asr');
    syncFoot('asr');
    try {
      // the legacy fields (model, compute_type, device, max_parallel) go back
      // as they are; the server merges into its settings anyway (lead ruling 15)
      const s = await api.setASR({
        ...data.asr,
        language: a.language.trim(),
        device_mode: a.device_mode,
        manual_devices: a.device_mode === MANUAL_MODE ? a.manual_devices : [],
        multi_device: a.multi_device,
        battery_aware: a.battery_aware,
        vram_reserve_mb: res,
        mem_reserve_mb: mem,
        redecode: a.redecode,
      });
      drafts.asr = null;
      saving.delete('asr');
      if (!alive) return;
      if (s) setData(s);
      pruneDrafts();
      render();
      toast(t('settings.asr.saved'), 'success');
    } catch (e) {
      saving.delete('asr');
      blockErrors.asr = e.message;
      if (alive) syncFoot('asr');
    }
  }

  // ---- storage and other ----

  function switchBtn(on, label, onToggle, fkey) {
    return h(
      'button',
      { type: 'button', class: 'switch set-switch' + (on ? ' is-on' : ''), role: 'switch', 'aria-checked': on ? 'true' : 'false', 'aria-label': label, dataset: { fkey }, onclick: onToggle },
      h('span', { class: 'switch-track' }, h('span', { class: 'switch-thumb' })),
      h('span', { class: 'set-switch-text' }, on ? t('settings.switch.on') : t('settings.switch.off')),
    );
  }

  // T23 general 19: the switch shows what is in effect, as the navigation's
  // does (app.js notifyToggle): without browser notifications it is off and
  // cannot be turned on; while the browser blocks them it is off, says
  // "blocked by the browser", and a press says how to allow them instead of
  // changing the setting. The note below (permissionLine) has the details.
  function notifySwitch(g) {
    const label = t('settings.general.notifications');
    const supported = 'Notification' in window;
    const denied = supported && Notification.permission === 'denied';
    if (supported && !denied) return switchBtn(g.notifications, label, () => setGeneralDraft({ notifications: !g.notifications }, true), 'set:gen:notify');
    const sw = switchBtn(false, label, () => (denied ? toast(t('nav.notify.denied_toast'), 'warn') : null), 'set:gen:notify');
    if (!supported) sw.disabled = true;
    else {
      const text = sw.querySelector('.set-switch-text');
      if (text) text.textContent = t('settings.general.notify_blocked');
    }
    return sw;
  }

  function permissionLine() {
    if (!('Notification' in window)) return h('p', { class: 'set-note' }, icon('info', { size: 13 }), t('settings.general.notify_unsupported'));
    const perm = Notification.permission;
    if (perm === 'granted') return h('p', { class: 'set-note is-ok' }, icon('circle-check', { size: 13 }), t('settings.general.notify_granted'));
    if (perm === 'denied') return h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 13 }), t('settings.general.notify_denied'));
    return h(
      'p',
      { class: 'set-note' },
      icon('info', { size: 13 }),
      t('settings.general.notify_default'),
      h(
        'button',
        {
          type: 'button',
          class: 'set-link',
          dataset: { fkey: 'set:gen:perm' },
          onclick: async () => {
            try {
              await Notification.requestPermission();
            } catch {
              // ignored
            }
            if (alive) renderBlock('general');
          },
        },
        t('settings.general.notify_request'),
      ),
    );
  }

  function generalBlock(shell) {
    const g = generalVal();
    // A provider is priced by its own prices or by its tariff's model prices
    // (settings.HasAnyPrice, the backend's budget check; accept-320).
    const tariffModels = (p) => Object.values((p.tariff && typeof p.tariff === 'object' && p.tariff.models) || {});
    const currencies = [
      ...new Set(data.providers.flatMap((p) => (p.price_input != null && p.currency ? [p.currency] : tariffModels(p).map((m) => m && m.currency).filter(Boolean)))),
    ];
    const priced = data.providers.some((p) => (p.price_input != null && p.price_cache_hit != null && p.price_output != null) || tariffModels(p).length > 0);
    const res = ctx.resources();
    const spent = {};
    for (const p of (res && res.providers) || []) if (p.cost_today != null) spent[p.currency || ''] = (spent[p.currency || ''] || 0) + p.cost_today;
    const spentText = Object.entries(spent)
      .map(([c, v]) => U.fmtMoney(v, c))
      .join(' · ');
    const chaptersRole = data.roles.find((r) => r.role === 'chapters');
    const chaptersReady = !!(chaptersRole && chaptersRole.configured);

    shell.append(
      head('sliders-horizontal', t('settings.block.general')),
      h(
        'div',
        { class: 'card-bd set-form' },
        formRow(
          t('settings.general.keep_media'),
          switchBtn(g.keep_original_media, t('settings.general.keep_media'), () => setGeneralDraft({ keep_original_media: !g.keep_original_media }, true), 'set:gen:media'),
          t('settings.general.keep_media_hint'),
          !g.keep_original_media ? h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 13 }), t('settings.general.keep_media_off')) : null,
        ),
        formRow(
          t('settings.general.notifications'),
          notifySwitch(g),
          t('settings.general.notifications_hint'),
          permissionLine(),
        ),
        formRow(
          t('settings.general.budget'),
          h(
            'div',
            { class: 'set-inline' },
            h('input', { class: 'input input-num set-w-sm', type: 'number', min: '0', step: 'any', value: g.daily_budget, 'aria-label': t('settings.general.budget'), dataset: { fkey: 'set:gen:budget' }, oninput: (e) => setGeneralDraft({ daily_budget: e.target.value }) }),
            h('span', { class: 'dim small' }, currencies.length === 1 ? currencies[0] : currencies.length ? currencies.join(' + ') : ''),
            num(g.daily_budget) === 0 ? h('span', { class: 'chip chip-quiet' }, t('settings.general.unlimited')) : null,
          ),
          t('settings.general.budget_hint'),
          [
            !priced ? h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 13 }), t('settings.general.no_prices')) : null,
            currencies.length > 1 ? h('p', { class: 'set-note' }, icon('info', { size: 13 }), t('settings.general.mixed_currencies', { list: U.listJoin(currencies) })) : null,
            spentText ? h('p', { class: 'set-note' }, icon('coins', { size: 13 }), t('settings.general.spent_today', { amount: spentText })) : null,
          ],
        ),
        // v3.2.x P3: chapters after the first export (absent = on)
        formRow(
          t('settings.general.auto_chapters'),
          switchBtn(g.auto_chapters, t('settings.general.auto_chapters'), () => setGeneralDraft({ auto_chapters: !g.auto_chapters }, true), 'set:gen:chapters'),
          t('settings.general.auto_chapters_hint'),
          g.auto_chapters && !chaptersReady ? h('p', { class: 'set-note is-warn' }, icon('triangle-alert', { size: 13 }), t('settings.general.auto_chapters_no_role')) : null,
        ),
        // v3.2.x P1-c: idle timeout of an LLM request; empty = default, 0 = off,
        // else MIN_IDLE_TIMEOUT_S..MAX_IDLE_TIMEOUT_S (SEC5 N4)
        formRow(
          t('settings.general.idle_timeout'),
          h(
            'div',
            { class: 'set-inline' },
            h('input', {
              class: 'input input-num set-w-sm',
              type: 'number',
              min: '0',
              max: String(MAX_IDLE_TIMEOUT_S),
              step: '1',
              value: g.llm_idle_timeout_s,
              placeholder: t('settings.general.idle_timeout_default', { n: DEFAULT_IDLE_TIMEOUT_S }),
              'aria-label': t('settings.general.idle_timeout'),
              'aria-invalid': fieldErrors.idle ? 'true' : undefined,
              dataset: { fkey: 'set:gen:idle' },
              oninput: (e) => setGeneralDraft({ llm_idle_timeout_s: e.target.value }),
            }),
            h('span', { class: 'dim small' }, t('settings.general.idle_timeout_unit')),
            String(g.llm_idle_timeout_s).trim() !== '' && num(g.llm_idle_timeout_s) === 0 ? h('span', { class: 'chip chip-quiet' }, t('settings.general.idle_timeout_off')) : null,
          ),
          t('settings.general.idle_timeout_hint', { n: DEFAULT_IDLE_TIMEOUT_S, min: MIN_IDLE_TIMEOUT_S, max: MAX_IDLE_TIMEOUT_S }),
          fieldErrors.idle ? h('p', { class: 'set-note is-err', role: 'alert' }, icon('circle-alert', { size: 13 }), fieldErrors.idle) : null,
        ),
      ),
      foot('general', saveGeneral),
    );
  }

  async function saveGeneral() {
    if (saving.has('general') || !dirty('general')) return;
    const g = drafts.general;
    const b = num(g.daily_budget);
    if (isNaN(b) || b < 0) {
      blockErrors.general = t('errors.settings.budget_invalid');
      syncFoot('general');
      return;
    }
    const idle = String(g.llm_idle_timeout_s).trim();
    if (idle !== '' && (!isInt(num(idle)) || (num(idle) !== 0 && (num(idle) < MIN_IDLE_TIMEOUT_S || num(idle) > MAX_IDLE_TIMEOUT_S)))) {
      fieldErrors.idle = t('errors.settings.idle_timeout_invalid', { min: MIN_IDLE_TIMEOUT_S, max: MAX_IDLE_TIMEOUT_S });
      blockErrors.general = fieldErrors.idle;
      renderBlock('general');
      return;
    }
    // PUT /api/settings/general merges (v3.2.x): only the changed fields;
    // null puts an optional one back to its default
    const was = serverGeneral();
    const body = {};
    if (g.keep_original_media !== was.keep_original_media) body.keep_original_media = g.keep_original_media;
    if (g.notifications !== was.notifications) body.notifications = g.notifications;
    if (b !== num(was.daily_budget)) body.daily_budget = b;
    if (g.auto_chapters !== was.auto_chapters) body.auto_chapters = g.auto_chapters;
    if (!sameOpt(idle, was.llm_idle_timeout_s)) body.llm_idle_timeout_s = idle === '' ? null : num(idle);
    saving.add('general');
    syncFoot('general');
    try {
      const s = await api.setGeneral(body);
      drafts.general = null;
      delete fieldErrors.idle;
      saving.delete('general');
      if (!alive) return;
      if (s) {
        data = s;
        dataKey = JSON.stringify(s);
      }
      pruneDrafts();
      render();
      toast(t('settings.general.saved'), 'success');
    } catch (e) {
      saving.delete('general');
      blockErrors.general = e.message;
      if (e.code === 'settings.idle_timeout_invalid') {
        fieldErrors.idle = e.message;
        if (alive) renderBlock('general');
      } else if (alive) syncFoot('general');
    }
  }

  // ---- public holidays (v2 P12.2: view and edit) ----

  let hol = null; // HolidaysView
  let holError = '';

  async function loadHolidays() {
    try {
      const v = await api.holidays({ quiet: true });
      if (!alive) return;
      hol = v;
      holError = '';
    } catch (e) {
      if (!alive) return;
      if (!hol) holError = e.message || t('settings.holidays.load_failed');
    }
    if (data) renderBlock('holidays');
  }

  function holidaysBlock(shell) {
    const users = data.providers.filter((p) => p.tariff && typeof p.tariff === 'object' && p.tariff.exclude_holidays === 'CN');
    const tools = hol
      ? [
          hol.edited ? btn(t('settings.holidays.restore'), { size: 'sm', kind: 'ghost', icon: 'undo-2', fkey: 'set:hol:restore', onclick: restoreHolidays }) : null,
          btn(t('settings.holidays.edit'), { size: 'sm', icon: 'pencil', fkey: 'set:hol:edit', onclick: editHolidays }),
        ]
      : null;
    let content;
    if (holError) content = errorBlock(holError, () => ((holError = ''), renderBlock('holidays'), loadHolidays()));
    else if (!hol) content = skeleton(3);
    else {
      const years = Object.keys(hol.years || {}).sort();
      const now = new Date().getFullYear();
      const missing = [now, now + 1].filter((y) => !years.includes(String(y)));
      content = h(
        'div',
        { class: 'hol' },
        years.length
          ? years.map((y) => {
              const dates = hol.years[y] || [];
              return h('div', { class: 'hol-year' }, h('span', { class: 'hol-y' }, t('settings.holidays.year', { year: y })), h('span', { class: 'muted small hol-n' }, t('settings.holidays.days', { n: dates.length })), h('div', { class: 'hol-ranges' }, dateRanges(dates).map((r) => h('span', { class: 'chip chip-quiet' }, rangeText(r)))));
            })
          : h('p', { class: 'set-note' }, t('settings.holidays.empty')),
        missing.map((y) => h('p', { class: 'set-note hol-missing' }, icon('info', { size: 13 }), t('settings.holidays.missing', { year: y }))),
        h('p', { class: 'set-note' }, icon(hol.edited ? 'pencil' : 'file-text', { size: 13 }), hol.edited ? t('settings.holidays.source_user') : t('settings.holidays.source_builtin', { source: U.errorInfoText(hol.source_info, hol.source || '') })),
      );
    }
    shell.append(
      head('calendar', t('settings.holidays.title'), hol && hol.edited ? h('span', { class: 'chip chip-quiet' }, t('settings.holidays.edited')) : null, tools),
      h(
        'div',
        { class: 'card-bd' },
        h('p', { class: 'set-intro' }, t('settings.holidays.intro')),
        users.length ? h('p', { class: 'set-note' }, icon('server', { size: 13 }), t('settings.holidays.used_by', { names: users.map((p) => p.name).join(t('tariff.list_sep')) })) : h('p', { class: 'set-note' }, icon('info', { size: 13 }), t('settings.holidays.unused')),
        content,
      ),
    );
  }

  async function editHolidays() {
    if (!hol) return;
    const v = await openHolidaysEditor(hol, (years) => api.saveHolidays(years, { quiet: true }));
    if (!v || !alive) return;
    hol = v;
    renderBlock('holidays');
    toast(t('settings.holidays.saved'), 'success');
  }

  async function restoreHolidays() {
    const ok = await confirmDialog({ title: t('settings.holidays.restore_title'), message: t('settings.holidays.restore_message'), okText: t('settings.holidays.restore') });
    if (!ok) return;
    try {
      const v = await api.saveHolidays(null);
      if (!alive) return;
      hol = v;
      renderBlock('holidays');
      toast(t('settings.holidays.restored'), 'success');
    } catch {
      // shown by the API layer
    }
  }

  // ---- lecture types (lecture kinds) ----

  function kindsBlock(shell) {
    const list = kindList();
    const builtin = list.filter((k) => k.builtin);
    const custom = list.filter((k) => !k.builtin);
    const row = (k) =>
      h(
        'li',
        { class: 'kind-row' + (k.builtin ? ' is-builtin' : ''), dataset: { kind: k.id } },
        h('span', { class: 'kind-name' }, h('span', { class: 'kind-name-t', title: kindName(k.id, 'full') }, kindName(k.id, 'full')), k.builtin && kindName(k.id) !== kindName(k.id, 'full') ? h('span', { class: 'kind-short' }, kindName(k.id)) : null),
        h('span', { class: 'kind-code mono', title: t('settings.kinds.code_tip', { code: k.code }) }, k.code),
        h('span', { class: 'kind-group' }, t(`kind.group.${k.group}`)),
        k.builtin
          ? h('span', { class: 'chip chip-quiet' }, t('settings.kinds.builtin_tag'))
          : menuButton(iconBtn('ellipsis', t('settings.kinds.actions_label', { name: kindName(k.id) }), { fkey: `set:kind:${k.id}:menu`, menu: true }), () => [
              { label: t('settings.kinds.edit'), icon: 'pencil', onSelect: () => kindDialog(k) },
              'sep',
              { label: t('common.delete'), icon: 'trash-2', danger: true, onSelect: () => deleteKind(k) },
            ]),
      );
    const head2 = () => h('li', { class: 'kind-row kind-head', 'aria-hidden': 'true' }, h('span', null, t('settings.kinds.col.name')), h('span', null, t('settings.kinds.col.code')), h('span', null, t('settings.kinds.col.group')), h('span'));
    shell.append(
      head('tag', t('settings.kinds.title'), h('span', { class: 'card-count' }, String(list.length)), btn(t('settings.kinds.add'), { size: 'sm', icon: 'plus', onclick: () => kindDialog(null), fkey: 'set:kind:add' })),
      h(
        'div',
        { class: 'card-bd' },
        h('p', { class: 'set-intro' }, t('settings.kinds.intro')),
        h('h3', { class: 'kind-sub' }, t('settings.kinds.builtin')),
        h('ul', { class: 'kind-list', 'aria-label': t('settings.kinds.builtin') }, head2(), builtin.map(row)),
        h('h3', { class: 'kind-sub' }, t('settings.kinds.custom')),
        custom.length ? h('ul', { class: 'kind-list', 'aria-label': t('settings.kinds.custom') }, head2(), custom.map(row)) : h('p', { class: 'set-note' }, icon('info', { size: 13 }), t('settings.kinds.custom_empty')),
      ),
    );
  }

  // The built-in kind's name (short or full, in the interface language) that
  // a custom kind's name equals, ignoring case and white space; '' if none.
  function builtinNamed(v) {
    const norm = (x) => String(x || '').replace(/\s+/g, '').toLocaleLowerCase();
    const want = norm(v);
    if (!want) return '';
    for (const id of BUILTIN_KINDS) {
      for (const form of ['short', 'full']) {
        const n = kindName(id, form);
        if (norm(n) === want) return n;
      }
    }
    return '';
  }

  // Add (k null) or edit a custom kind: name (never translated), code and group.
  function kindDialog(k) {
    return openDialog(k ? t('settings.kinds.edit_title') : t('settings.kinds.add_title'), (close, busy) => {
      let group = k ? k.group : 'session';
      const name = h('input', { class: 'input', value: k ? k.name || '' : '', autofocus: true, maxlength: '40', 'aria-describedby': 'kind-name-hint kind-name-clash', dataset: { fkey: 'kind:name' } });
      // the same name as a built-in kind in this language: a note only (still saved)
      const clash = h('span', { class: 'kind-name-clash', id: 'kind-name-clash', 'aria-live': 'polite' });
      const code = h('input', { class: 'input mono kind-code-input', value: k ? k.code : '', maxlength: '3', autocapitalize: 'characters', spellcheck: 'false', 'aria-describedby': 'kind-code-hint', dataset: { fkey: 'kind:code' } });
      const err = h('p', { class: 'dlg-field-error', role: 'alert' });
      const ok = h('button', { type: 'submit', class: 'btn btn-primary' }, k ? t('common.save') : t('settings.kinds.add'));
      const sync = () => {
        code.value = code.value.toUpperCase().replace(/[^A-Z]/g, '');
        const bad = code.value && !/^[A-Z]{1,3}$/.test(code.value);
        const taken = kindList().some((x) => x.id !== (k && k.id) && x.code === code.value);
        err.textContent = bad ? t('settings.kinds.code_invalid') : taken ? t('settings.kinds.code_taken', { code: code.value }) : '';
        ok.disabled = !name.value.trim() || !code.value || bad || taken;
        const same = builtinNamed(name.value);
        const note = same ? t('settings.kinds.name_builtin', { name: same }) : '';
        if (clash.textContent !== note) clash.textContent = note;
      };
      name.addEventListener('input', sync);
      code.addEventListener('input', sync);
      sync();
      const groups = h(
        'div',
        { class: 'dlg-choices kind-groups', role: 'radiogroup', 'aria-label': t('settings.kinds.group_label') },
        ['session', 'material'].map((g) =>
          h(
            'label',
            { class: 'dlg-choice' },
            h('input', { type: 'radio', name: 'kind-group', value: g, checked: g === group, onchange: () => (group = g) }),
            h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, t(`kind.group.${g}`)), h('span', { class: 'dlg-choice-hint' }, t(`settings.kinds.group_${g}_hint`))),
          ),
        ),
      );
      const onSubmit = async (e) => {
        e.preventDefault();
        if (ok.disabled) return;
        ok.disabled = true;
        const input = { name: name.value.trim(), code: code.value, group };
        try {
          const saved = await busy(k ? api.patchKind(k.id, input, { quiet: true }) : api.createKind(input, { quiet: true }));
          close(saved);
        } catch (ex) {
          err.textContent = ex.message;
          ok.disabled = false;
        }
      };
      return h(
        'form',
        { class: 'kind-form', onsubmit: onSubmit, novalidate: true },
        h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label' }, t('settings.kinds.name_label')), name, h('span', { class: 'dlg-hint', id: 'kind-name-hint' }, t('settings.kinds.name_hint')), clash),
        h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label' }, t('settings.kinds.code_label')), code, h('span', { class: 'dlg-hint', id: 'kind-code-hint' }, t('settings.kinds.code_hint'))),
        h('div', { class: 'dlg-field' }, h('span', { class: 'dlg-label' }, t('settings.kinds.group_label')), groups),
        err,
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
      );
    }).then((saved) => {
      if (!saved) return;
      toast(k ? t('settings.kinds.saved', { name: saved.name }) : t('settings.kinds.added', { name: saved.name }), 'success');
      loadKinds();
    });
  }

  async function deleteKind(k) {
    const ok = await confirmDialog({ title: t('settings.kinds.delete_title'), message: t('settings.kinds.delete_message', { name: k.name }), okText: t('common.delete'), danger: true });
    if (!ok) return;
    try {
      await api.deleteKind(k.id);
    } catch {
      return; // shown by the API layer (409: lectures use it)
    }
    toast(t('settings.kinds.deleted', { name: k.name }), 'success');
    loadKinds();
  }

  // ---- language and display (P14.7 interface language, saved on the
  // server; P13.6 lecture-title language, kept in this browser) ----

  // The interface language: every language in its own name (P14.7), the
  // "translated and reviewed by AI" tag and the invitation to native
  // speakers (P14.8.3); a note when the chosen language has no catalog yet.
  let langNote = { code: '', text: '' };

  function languageRow() {
    const cur = getLocale();
    // Buttons, not radios: arrow keys on a radio group would switch (and
    // re-render) the whole interface at every step.
    const list = h(
      'div',
      { class: 'set-lang-list', role: 'group', 'aria-label': t('i18n.language.title') },
      localeChoices().map((l) =>
        h(
          'button',
          {
            type: 'button',
            class: 'set-lang-opt' + (l.code === cur ? ' is-current' : ''),
            'aria-pressed': l.code === cur ? 'true' : 'false',
            dataset: { fkey: `set:locale:${l.code}` },
            onclick: () => chooseLocale(l.code),
          },
          h('span', { class: 'set-lang-mark' }, l.code === cur ? icon('check', { size: 14 }) : null),
          h('span', { class: 'set-lang-name', lang: langAttr(l.code) }, l.name),
          l.code === 'zh-CN' ? null : h('span', { class: 'set-lang-tag' }, l.code === PSEUDO ? t('i18n.language.pseudo_tag') : t('i18n.language.ai_tag')),
        ),
      ),
    );
    if (langNote.code !== cur) {
      langNote = { code: cur, text: '' };
      hasCatalog(cur).then(async (ok) => {
        if (ok || !alive || getLocale() !== cur) return;
        langNote = { code: cur, text: t('i18n.language.incomplete', { fallback: await fallbackName(cur) }) };
        renderBlock('display');
      });
    }
    return formRow(t('i18n.language.title'), list, t('i18n.language.hint'), [
      langNote.text ? h('p', { class: 'set-note is-warn' }, icon('info', { size: 13 }), langNote.text) : null,
      h('p', { class: 'set-note' }, icon('languages', { size: 13 }), t('i18n.language.welcome')),
    ]);
  }

  // P14.6: how the Chinese of lecture content is shown. The choice is saved
  // on the server with the interface language; "" (nothing chosen) follows
  // the language (zh-TW → Taiwan, yue-Hant-HK → Hong Kong). Choosing loads
  // the converter, then the whole page re-renders (zhdisplaychange).
  let zhBusy = false;

  async function pickZhDisplay(v) {
    if (zhBusy) return;
    zhBusy = true;
    renderBlock('display');
    await chooseZhDisplay(v);
    zhBusy = false;
    if (alive) renderBlock('display'); // the page was not re-rendered (no change, or the load failed)
  }

  function zhDisplayRow() {
    const cur = zhWanted();
    const notes = [];
    if (zhBusy) notes.push(h('p', { class: 'set-note', role: 'status' }, icon('loader-circle', { size: 13, class: 'spin' }), t('settings.display.zh_loading')));
    else if (zhFailed()) notes.push(h('p', { class: 'set-note is-warn', role: 'status' }, icon('triangle-alert', { size: 13 }), t('settings.display.zh_load_failed')));
    notes.push(h('p', { class: 'set-note' }, icon('info', { size: 13 }), t(zhSetting() ? 'settings.display.zh_note' : 'settings.display.zh_note_following')));
    return formRow(
      t('settings.display.zh_display'),
      segmented(
        'set-zh-display',
        ZH_MODES.map((m) => [m, t(`settings.display.zh_${m}`)]),
        cur,
        (v) => pickZhDisplay(v),
        t('settings.display.zh_display'),
      ),
      t('settings.display.zh_display_hint'),
      notes,
    );
  }

  function displayBlock(shell) {
    const cur = titleLang();
    // example content (a lecture title is not UI text, but it lives in the catalog with the rest of this page)
    const ex = { main: t('settings.display.example_en'), zh: t('settings.display.example_zh'), blurb: t(cur === 'en' ? 'settings.display.example_blurb_en' : 'settings.display.example_blurb_zh') };
    const preview = h(
      'div',
      { class: 'set-title-preview', 'aria-hidden': 'true' },
      h('span', { class: 'ktag' }, h('span', { class: 'ktag-k' }, kindName('lecture')), h('span', { class: 'ktag-n' }, t('kind.nth', { n: 1 }))),
      // the example is lecture content: its Chinese is Simplified, shown
      // like lecture content (P14.6 display mode; lang, so a zh-TW or
      // Cantonese interface does not draw Simplified text with Traditional glyphs)
      h(
        'span',
        { class: 'set-title-preview-main' },
        h('span', { class: 'set-title-preview-t', lang: cur === 'zh' ? zhLang() : 'en' }, cur === 'zh' ? zh(ex.zh) : ex.main),
        cur === 'both' ? h('span', { class: 'set-title-preview-sub', lang: zhLang() }, zh(ex.zh)) : null,
        h('span', { class: 'set-title-preview-blurb', title: cur === 'en' ? ex.blurb : zh(ex.blurb), lang: cur === 'en' ? 'en' : zhLang() }, cur === 'en' ? ex.blurb : zh(ex.blurb)),
      ),
    );
    shell.append(
      head('languages', t('i18n.block_title')),
      h(
        'div',
        { class: 'card-bd set-form' },
        languageRow(),
        zhDisplayRow(),
        formRow(
          t('settings.display.title_lang'),
          segmented(
            'set-title-lang',
            TITLE_LANGS.map((v) => [v, t(`settings.display.title_lang_${v}`)]),
            cur,
            (v) => {
              setTitleLang(v);
              renderBlock('display');
            },
            t('settings.display.title_lang'),
          ),
          t('settings.display.title_lang_hint'),
          [preview, h('p', { class: 'set-note' }, icon('info', { size: 13 }), t('settings.display.local_note'))],
        ),
      ),
    );
  }

  // ---- lifecycle ----

  const beforeUnload = (e) => {
    if (anyDirty()) {
      e.preventDefault();
      e.returnValue = '';
    }
  };
  window.addEventListener('beforeunload', beforeUnload);
  const onKinds = () => data && renderBlock('kinds');
  window.addEventListener('kindschange', onKinds);

  render();
  load();
  loadHolidays();
  loadKinds();
  compUI.load();
  appUI.load();
  dlUI.load();
  aboutUI.load();
  colabUI.load();
  remoteUI.load();
  modeUI.load();
  return {
    refresh: () => (loadHolidays(), load()),
    onChanged(ev) {
      if (ev.kind === 'settings') {
        loadSoon();
        loadHolidays();
        appUI.onSettingsChanged();
        dlUI.onSettingsChanged();
        colabUI.onSettingsChanged();
        modeUI.load(); // ext-step1: the guide or another window may have changed it
      }
      if (ev.kind === 'devices') loadSoon(); // SPEC-v2 I6-I9: rows, pauses, tests, power
      appUI.onChanged(ev); // SPEC-v3 V8: "changed update" (the update status by the channel)
      colabUI.onChanged(ev); // 3.1.0: "changed colab" (the in-app Google sign-in)
      remoteUI.onChanged(ev); // ssh-1b: "changed devices" (a server's state)
    },
    // #/settings/asr/{device} while the page is open: jump there
    update(r) {
      if (!r || r.name !== 'settings') return false;
      if (r.section) {
        pendingJump = r;
        jumpPending();
      }
      return true;
    },
    // Balances come from the resource snapshots (A10.2): re-render the
    // provider list only when one of them changed.
    onResources(r) {
      if (!data) return;
      const sig = balanceSig();
      if (sig !== balanceKey) renderBlock('providers');
      // the main switch of the resource panel (the device list's note)
      const paused = !!(r && r.gpu && r.gpu.paused);
      if (paused !== allPausedShown) {
        allPausedShown = paused;
        devUI.render();
      }
      // whether the power-aware rules of a laptop apply: the snapshot may
      // come after the page (the hints, the manual list's notes, the order
      // hint of the device list)
      const battery = U.batteryMachine(r);
      if (battery !== batteryShown) {
        batteryShown = battery;
        renderASRGlobal();
        devUI.render();
      }
    },
    destroy() {
      alive = false;
      clearTimeout(loadTimer);
      devUI.destroy();
      compUI.destroy();
      appUI.destroy();
      dlUI.destroy();
      aboutUI.destroy();
      colabUI.destroy();
      remoteUI.destroy();
      remoteUnsub();
      modeUI.destroy();
      if (spy) spy.disconnect();
      window.removeEventListener('beforeunload', beforeUnload);
      window.removeEventListener('kindschange', onKinds);
      if (data && anyDirty()) toast(t('settings.leave_unsaved'), 'info');
    },
  };
}
