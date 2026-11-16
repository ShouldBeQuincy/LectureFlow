// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/system: diagnostics (SPEC §9.2, §17.1) — checks in green / amber / red
// with the back end's fix hints and "check again" — plus GPU, API, today's
// usage, disk (per course, §17.5) and the read-only configuration (§9.10).
// v1.1: providers and keys live on the settings page; the configuration
// shown here never contains a key (the back end sends key_hint only).
// v2 P14.3: the check details and hints come as codes (detail_info,
// hint_info: t("errors."+code)); a detail without a code is a program's own
// output (ffmpeg -version, python --version) and is shown as it is.
// SPEC-v2 I10 (phase 3b): the OpenVINO checks form their own group (at most
// warn), and a card lists the transcription devices, the power state and
// the OpenVINO environment ("check again" also runs the device detection,
// on the server).
// SPEC-v3 (V12.2 acceptance, item 5): in desktop mode (GET /api/app mode)
// Python, faster-whisper, pypdf, the CUDA self-test and the OpenVINO
// environment come from the component manager, so their development hints
// (.venv, README, requirements.txt) give way to "install it in Settings →
// Components" with a link there; web mode keeps the server's hints.
// mac-opti M3: the server picks the checks by platform and installed
// components (macOS: the MLX group instead of GPU / CUDA; GPU, CUDA and
// OpenVINO only with that hardware) and, in desktop mode, sends hints that
// name the component to install (check.hint_install_*): they keep their
// text and get the link to the Components page. The NVIDIA card is left
// out when the server shows no GPU check (no NVIDIA GPU).
// 3.1.0: the read-only configuration lists what is in effect here: the
// interpreter of each back end of this platform with its source ("not
// installed" without one), the transcription devices with their own
// model, precision and parallel tasks; nvidia-smi and the VRAM settings
// only with an NVIDIA GPU (the server decides).
// ext-step1 (SPEC-ext "主程序配套改动 1"): the server picks the rows by
// hardware and transcription mode — NVIDIA: GPU, cuDNN, CUDA; Apple
// silicon: the MLX group with its self-test; neither: the OpenVINO group and
// the CPU self-test; only Colab: none of these but the "云端" group (gcloud,
// authorisation, reachability). The page hides nothing; it only names the
// new rows, links their hints and keeps NVIDIA out of its own texts where
// the server shows no GPU row (the "check again" note, the configuration's
// faster-whisper interpreter, OpenVINO's ignored devices).
// ext web3 (SPEC-ext "2. 内建 aria2c", "3. Native Messaging 宿主"): the row
// aria2c (version and SHA-256, as the server words them) joins the
// environment; browser_ext gets its own group, "浏览器扩展", where the
// server's row is followed by the registration of each browser, the latest
// extension connection (when the row does not say it) and "重新登记"
// (system_ext.js).
// datadir-ui (sec-update M2): a data_dir row that other accounts can reach
// (hint check.hint_data_dir_restrict) gets "收紧权限" in its hint
// (system_datadir.js).
// ssh-1b: with the SSH servers on, a card "远程服务器" with one group of rows
// per server — connection, agent version, Python environment, GPU / driver,
// model, disk (settings_remote.js diagRows) — and "测试连接" per server.
// The diagnostics zip leaves their host and user names out (the server).

import { api } from './api.js';
import { h, clear, btn, skeleton, errorBlock, preserveFocus, copyText, toast } from './dom.js';
import { icon } from './icons.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { obApi } from './onboarding_api.js';
import { LOCAL_ENTRY } from './asr_ready.js';
import { createBrowserExt } from './system_ext.js';
import { createDataDirRestrict, restrictable } from './system_datadir.js';
import { sshEnabled, sshServers, loadServers, onServersChange, noteServer, diagRows, requestError } from './settings_remote.js';
import { appRequest, exportDiagnosticsFile } from './settings_app.js';

// The detail code of a check that is still running (internal/api/system.go).
const CHECK_PENDING = 'check.checking';
// Fix hints that point at a development setup (.venv, requirements.txt):
// in desktop mode the components replace them.
const COMPONENT_HINTS = new Set(['check.hint_python', 'check.hint_python_version', 'check.hint_pip', 'check.hint_selftest', 'check.hint_openvino_venv']);
// The desktop mode's own hints that name a component (mac-opti M3): their
// text, plus the link to the Components page.
const INSTALL_HINTS = new Set([
  'check.hint_install_python',
  'check.hint_install_fw',
  'check.hint_install_mlx',
  'check.hint_install_openvino',
  'check.hint_install_mlx_model',
  'check.hint_selftest_desktop',
  // ext-step1
  'check.hint_install_gcloud',
  'check.hint_repair_local',
  'check.hint_repair_openvino',
]);
// ext-step1: hints with a link of their own — [href, label key]
const HINT_LINKS = {
  'check.hint_install_local': [LOCAL_ENTRY, 'system.install_local'],
  'check.hint_colab_auth': ['#/settings/colab', 'system.open_colab'],
  'check.hint_colab_reach': ['#/settings/colab', 'system.open_colab'],
};
// ext-step1: names of the new rows (products as they are, the others by
// common.check.{name})
const CHECK_NAMES = {
  cudnn: () => 'cuDNN',
  gcloud: () => 'Google Cloud CLI',
  mlx_selftest: () => t('common.check.mlx_selftest'),
  cpu_selftest: () => t('common.check.cpu_selftest'),
  colab_auth: () => t('common.check.colab_auth'),
  colab_reach: () => t('common.check.colab_reach'),
  // ext web3 (the names are core3's, with the rows)
  aria2c: () => t('common.check.aria2c'),
  browser_ext: () => t('common.check.browser_ext'),
};
const checkName = (name) => (CHECK_NAMES[name] ? CHECK_NAMES[name]() : U.checkName(name));
// The application's mode (GET /api/app), fetched once per page load.
let appMode = null;
const GROUPS = [
  ['env', 'layers', (n) => ['data_dir', 'credentials', 'data_dir_location', 'ffmpeg', 'aria2c', 'python', 'faster-whisper', 'pypdf', 'install_dir'].includes(n)],
  ['gpu', 'cpu', (n) => n === 'gpu' || n === 'cudnn' || n === 'cuda'],
  ['mlx', 'cpu', (n) => n === 'mlx_python' || n === 'mlx' || n === 'mlx_model' || n === 'mlx_selftest'],
  ['openvino', 'cpu', (n) => ['openvino_venv', 'openvino', 'openvino_devices', 'npu_driver'].includes(n)],
  ['cpu', 'cpu', (n) => n === 'cpu_selftest'],
  ['cloud', 'cloud', (n) => n === 'gcloud' || n === 'colab_auth' || n === 'colab_reach'],
  ['ext', 'plug', (n) => n === 'browser_ext'],
  ['api', 'server', (n) => n.startsWith('api_key:') || n.startsWith('api:') || n.startsWith('role_model:')],
  ['disk', 'hard-drive', (n) => n === 'disk'],
];
const STATUS_ICON = { ok: 'circle-check', warn: 'triangle-alert', error: 'circle-alert', pending: 'loader-circle' };
const statusText = (st) => t(`system.status.${st}`);
// r2 N15: the hints a device of GET /api/system may carry (vram_hint: the
// NVIDIA card's VRAM against cuda:0's model; advice only). Another code
// (a newer server) shows nothing. params.extra may add the sentence
// system.devices.vram_queue / vram_queue_switch / vram_queue_estimate (the
// per-task estimate plus the reserve exceed the card; the estimate alone
// does, and the card's recommended model fits / nothing fits: r2
// T-PERF-02); vram_queue_only is the hint that only carries that sentence.
// r6 (T043): vram_low_missing / vram_low_missing_install stand in for
// vram_low when the model the card is too small for is not installed
// either (lectures wait with model_missing): the card's recommended model
// is installed / still has to be installed.
const VRAM_HINTS = new Set([
  'system.devices.vram_low',
  'system.devices.vram_model_missing',
  'system.devices.vram_queue_only',
  'system.devices.vram_low_missing',
  'system.devices.vram_low_missing_install',
]);
// The hints whose first step is installing a model: they link to the
// Components page as well.
const VRAM_INSTALL_HINTS = new Set(['system.devices.vram_low_missing_install']);
const VRAM_EXTRAS = new Set(['system.devices.vram_queue', 'system.devices.vram_queue_estimate', 'system.devices.vram_queue_switch']);
// The devices card's title (the GPU group's name, without the count): it
// names the table's sideways-scrolling region.
const DEVICES_TITLE_ID = 'sys-devices-title';
// The table's wrapper keeps the focus across a re-render (preserveFocus
// finds it by this key): the page is drawn anew when the system info, the
// resources (a GPU slot taken or freed) or the server list change.
export const DEVICE_TABLE_FKEY = 'sys:dev:table';

/**
 * The device table's wrapper. It scrolls sideways on a narrow screen: a
 * region in the Tab order, named by the card's title, so the keyboard can
 * scroll it (T16 D5), with a stable focus key (R48 M1).
 */
export function deviceTableWrap(table) {
  return h('div', { class: 'sys-dev-wrap', tabindex: '0', role: 'region', 'aria-labelledby': DEVICES_TITLE_ID, dataset: { fkey: DEVICE_TABLE_FKEY } }, table);
}
function vramText(info, known) {
  if (!info || !known.has(info.code)) return '';
  const p = {};
  for (const [k, v] of Object.entries(info.params || {})) if (k !== 'extra') p[k] = typeof v === 'number' ? U.fmtInt(v) : v;
  return t(info.code, p);
}
// The hint's parts: its own text, then each extra sentence (each a whole
// piece of advice of its own); [] for a hint this page does not know.
function vramHintParts(info) {
  const main = vramText(info, VRAM_HINTS);
  if (!main) return [];
  const extra = Array.isArray(info.params && info.params.extra) ? info.params.extra.map((x) => vramText(x, VRAM_EXTRAS)).filter(Boolean) : [];
  return [main, ...extra];
}
export function vramHintText(info) {
  // sentences: no space after a full-width stop (zh), one otherwise
  return U.joinSentences(vramHintParts(info));
}
/** The devices whose VRAM hint this page shows (each one counts as a warning in the summary). */
export function vramHintCount(sys) {
  return ((sys && sys.devices) || []).filter((d) => vramHintParts(d.vram_hint).length > 0).length;
}
/**
 * The row of a device's VRAM hint (d.vram_hint) below the device table, in
 * the checks' warning style, with the link to the transcription settings
 * (and to Components when the hint says to install a model); null without
 * a hint this page knows.
 */
export function vramHintRow(d) {
  const parts = vramHintParts(d.vram_hint);
  if (!parts.length) return null;
  // r7 (T24 G9): the extra sentence is advice of its own (e.g. lower the
  // reserve), on a line of its own rather than run on after the first
  const lines = [];
  parts.forEach((p, i) => lines.push(i ? h('br') : null, p));
  return h(
    'div',
    { class: 'sys-hint is-warn' },
    icon('triangle-alert', { size: 13 }),
    h('span', null, lines),
    h('a', { class: 'sys-hint-link', href: '#/settings/asr', dataset: { fkey: `sys:vram:${d.id}` } }, t('system.open_settings'), icon('chevron-right', { size: 13 })),
    VRAM_INSTALL_HINTS.has(d.vram_hint.code)
      ? h('a', { class: 'sys-hint-link', href: '#/settings/components', dataset: { fkey: `sys:vramcomp:${d.id}` } }, t('system.open_components'), icon('chevron-right', { size: 13 }))
      : null,
  );
}
// Options of config.toml and the settings sections described by
// system.config.label.{key}.
const CONFIG_KEYS = new Set([
  'listen',
  'data_dir',
  'python',
  'python_openvino',
  'ffmpeg',
  'nvidia_smi',
  'worker_dir',
  'log_level',
  'asr',
  'asr_devices',
  'general',
  'asr.model',
  'asr.compute_type',
  'asr.device',
  'asr.language',
  'asr.max_parallel',
  'asr.vram_reserve_mb',
  'asr.vram_per_job_mb',
  'asr.fake_result',
  'asr.fake_result_openvino',
  'general.keep_original_media',
  'general.notifications',
  'general.daily_budget',
]);
// SPEC-v2 I8 settings in the configuration: named like on the settings page
const CONFIG_SETTING_LABELS = {
  'asr.device_mode': 'settings.asr.device_label',
  'asr.manual_devices': 'settings.asr.manual_label', // power management (manual mode)
  'asr.multi_device': 'settings.asr.multi_device',
  'asr.mem_reserve_mb': 'settings.asr.mem_reserve_label',
};
// Settings the configuration table no longer lists: battery_aware was
// retired by power management (still sent by the back end, ignored by the
// policy; "复制" keeps the whole configuration).
const CONFIG_HIDDEN = new Set(['asr.battery_aware']);
const configLabel = (key) => (CONFIG_KEYS.has(key) ? t(`system.config.label.${key}`) : CONFIG_SETTING_LABELS[key] ? t(CONFIG_SETTING_LABELS[key]) : '');
// 3.1.0: the sections of the configuration in the order they are shown
// (the server's JSON is sorted by key); others follow.
const CONFIG_ORDER = ['python', 'asr', 'asr_devices', 'general'];
const configOrder = (key) => (CONFIG_ORDER.includes(key) ? CONFIG_ORDER.indexOf(key) : CONFIG_ORDER.length);
// The component whose interpreter a backend runs (config python list).
const PYTHON_COMPONENT = {
  'faster-whisper': 'compui.name.asr_cuda',
  openvino: 'compui.name.asr_openvino',
  mlx: 'compui.name.asr_mlx',
  base: 'compui.name.python',
};
// Where an interpreter comes from (config.PythonSource).
const PYTHON_SOURCE = {
  component: 'system.config.python.source.component',
  config: 'system.config.python.source.config',
  default: 'system.config.python.source.default',
};

function checkState(c) {
  return c.detail_info && c.detail_info.code === CHECK_PENDING ? 'pending' : c.status;
}

// The text of a coded detail or hint; the untranslated string when there is
// no code (or no translation). Free space comes in MB (free_mb).
// The browser extension's rows name browsers as one string the server
// joined with ", " ("Chrome, Edge"; internal/api system_ext.go
// browserNames): listed in the language's own way here (T26 J9: "Chrome、Edge").
function checkText(info, text) {
  if (info && info.params && typeof info.params.free_mb === 'number') info = { ...info, params: { ...info.params, free_mb: U.fmtMB(info.params.free_mb) } };
  if (info && info.params && typeof info.params.browsers === 'string' && info.params.browsers.includes(', ')) info = { ...info, params: { ...info.params, browsers: U.listJoin(info.params.browsers.split(', ')) } };
  return U.errorInfoText(info, text);
}

// A sentence with a link in place of {link}.
function withLink(text, link) {
  const i = text.indexOf('{link}');
  return i < 0 ? [text, link] : [text.slice(0, i), link, text.slice(i + '{link}'.length)];
}

export function mountSystem(root, ctx) {
  let alive = true;
  let checking = false;
  let error = '';
  let gpuKey = '';
  let ext = null; // the browser extension's details (system_ext.js), once the row is there
  const el = h('div', { class: 'page page-system' });
  // "收紧权限" of the data_dir row (system_datadir.js)
  const dataDir = createDataDirRestrict({ refresh: () => refresh(), changed: () => alive && preserveFocus(render) });
  root.append(el);
  if (!appMode) {
    appMode = obApi
      .app({ quiet: true })
      .then((info) => (info && info.mode) || 'web')
      .catch(() => {
        appMode = null; // try again on the next visit
        return 'web';
      });
  }
  let desktopMode = false;
  appMode.then((m) => {
    if (!alive || m !== 'desktop') return;
    desktopMode = true;
    if (ctx.system()) preserveFocus(render);
  });

  async function refresh() {
    try {
      await ctx.refreshSystem();
      error = '';
    } catch (e) {
      if (!ctx.system()) error = e.message || t('common.load_failed');
    }
    if (alive) preserveFocus(render);
  }

  let exporting = false;
  let noteOpen = false; // the folded privacy note under the title
  async function exportDiag() {
    if (exporting) return;
    exporting = true;
    preserveFocus(render);
    await exportDiagnosticsFile(() => alive);
    exporting = false;
    if (alive) preserveFocus(render);
  }

  async function recheck() {
    if (checking) return;
    checking = true;
    preserveFocus(render);
    try {
      const sys = await api.recheck();
      if (alive && sys) ctx.setSystem(sys);
    } catch {
      // shown by the API layer
    } finally {
      checking = false;
      if (alive) preserveFocus(render);
    }
  }

  function render() {
    const sys = ctx.system();
    clear(el);
    el.append(
      h(
        'header',
        { class: 'page-head' },
        h('div', { class: 'page-titles' }, h('h1', { class: 'page-title' }, t('system.title')), h('p', { class: 'page-sub' }, t('system.subtitle'))),
        h(
          'div',
          { class: 'page-actions' },
          // r7 (T24 G10, T25 G8): where a user in trouble looks first, as in Settings → App
          btn(exporting ? t('settings.app.diag_exporting') : t('settings.app.diag_export'), {
            icon: exporting ? 'loader-circle' : 'download',
            disabled: exporting,
            onclick: exportDiag,
            fkey: 'sys:diag',
          }),
          h(
            'button',
            { type: 'button', class: 'btn btn-primary', disabled: checking, dataset: { fkey: 'sys:recheck' }, onclick: recheck },
            icon('rotate-cw', { size: 16, class: checking ? 'spin' : '' }),
            h('span', { class: 'btn-label' }, checking ? recheckingText(sys) : t('system.recheck')),
          ),
        ),
      ),
      // what the export holds and leaves out (the same words as in Settings
      // → App), folded into one line: the summary below stays the first
      // thing on the page (R75 L8)
      h(
        'details',
        // opened stays open across the page's redraws
        { class: 'sys-ext-details sys-diag-note', open: noteOpen, ontoggle: (e) => (noteOpen = !!e.target.open) },
        // the focus key on the line that takes the focus (R81 L4)
        h('summary', { dataset: { fkey: 'sys:diag:note' } }, t('system.diag_note')),
        h('p', { class: 'dim small' }, icon('shield-check', { size: 13 }), ' ', t('settings.app.diag_privacy')),
      ),
    );
    if (!sys) {
      // spread: the native append prints an array as "[object HTMLElement],…" (3.2.0 P1)
      el.append(
        ...(error
          ? [h('section', { class: 'card' }, h('div', { class: 'card-bd' }, errorBlock(error, () => ((error = ''), render(), refresh()))))]
          : [h('section', { class: 'card sys-summary is-loading' }, h('div', { class: 'card-bd' }, skeleton(2))), h('section', { class: 'card' }, h('div', { class: 'card-bd' }, skeleton(8)))]),
      );
      return;
    }
    // ext-step1: only Colab has no device card (el.append would print a null)
    el.append(
      ...[summary(sys), checksCard(sys), sys.devices || sys.openvino ? devicesCard(sys) : null, remoteCard(), h('div', { class: 'sys-grid' }, nvidiaShown(sys) ? gpuCard(sys) : null, apiCard(sys), usageCard(sys), diskCard(sys)), configCard(sys.config || {}, nvidiaShown(sys))].filter(Boolean),
    );
    gpuKey = gpuSig();
  }

  // What "check again" runs (ext-step1): the CUDA self-test only with an
  // NVIDIA GPU; another self-test (MLX, CPU); or no inference at all.
  function recheckingText(sys) {
    const names = new Set(((sys && sys.checks) || []).map((c) => c.name));
    if (names.has('cuda')) return t('system.rechecking');
    if (names.has('mlx_selftest') || names.has('cpu_selftest')) return t('system.rechecking_selftest');
    return t('system.rechecking_plain');
  }

  // ---- summary ----

  function summary(sys) {
    const states = sys.checks.map(checkState);
    const n = (s) => states.filter((x) => x === s).length;
    const errors = n('error');
    // r7 (T24 G9): a device's VRAM hint (the devices card) is a warning too
    const warns = n('warn') + vramHintCount(sys);
    const pending = n('pending');
    let tone = 'ok';
    let title = t('system.summary.ok_title');
    let text = t('system.summary.ok_text');
    if (errors) {
      tone = 'err';
      title = t('system.summary.err_title', { n: errors });
      text = t('system.summary.err_text');
    } else if (pending || checking) {
      tone = 'run';
      title = t('system.summary.run_title');
      text = t('system.summary.run_text');
    } else if (warns) {
      tone = 'warn';
      title = t('system.summary.warn_title', { n: warns });
      text = t('system.summary.warn_text');
    }
    const noCourses = ctx.coursesLoaded() && ctx.courses().length === 0;
    return h(
      'section',
      { class: `card sys-summary tone-${tone}`, 'aria-live': 'polite' },
      h('div', { class: 'sys-summary-icon' }, icon(tone === 'ok' ? 'circle-check' : tone === 'err' ? 'circle-alert' : tone === 'run' ? 'loader-circle' : 'triangle-alert', { size: 22, class: tone === 'run' ? 'spin' : '' })),
      h('div', { class: 'sys-summary-main' }, h('div', { class: 'sys-summary-title' }, title), h('div', { class: 'sys-summary-text' }, text)),
      h(
        'div',
        { class: 'sys-counts' },
        countPill('ok', n('ok')),
        countPill('warn', warns),
        countPill('error', errors),
        pending ? countPill('pending', pending) : null,
      ),
      !errors && noCourses ? h('div', { class: 'sys-summary-cta' }, h('span', null, t('system.summary.cta')), btn(t('system.summary.add_course'), { kind: 'primary', size: 'sm', icon: 'plus', onclick: ctx.newCourse, fkey: 'sys:add' })) : null,
    );
  }

  function countPill(state, count) {
    return h('span', { class: `sys-count is-${state}` + (count ? '' : ' is-zero') }, icon(STATUS_ICON[state], { size: 13 }), h('b', null, String(count)), statusText(state));
  }

  // ---- checks ----

  function checksCard(sys) {
    const groups = GROUPS.map(([key, ic, match]) => ({ key, ic, title: t(`system.group.${key}`), items: sys.checks.filter((c) => match(c.name)) }));
    const rest = sys.checks.filter((c) => !GROUPS.some((g) => g[2](c.name)));
    if (rest.length) groups.push({ key: 'other', ic: 'list-checks', title: t('system.group.other'), items: rest });
    return h(
      'section',
      { class: 'card card-flush sys-checks', 'aria-label': t('system.checks') },
      h('header', { class: 'card-hd' }, h('h2', { class: 'card-title' }, icon('stethoscope', { size: 16 }), h('span', null, t('system.checks')), h('span', { class: 'card-count' }, String(sys.checks.length)))),
      groups
        .filter((g) => g.items.length)
        .map((g) => {
          const bad = g.items.filter((c) => checkState(c) === 'error').length;
          const warn = g.items.filter((c) => checkState(c) === 'warn').length;
          return h(
            'div',
            { class: 'sys-group' },
            h(
              'h3',
              { class: 'sys-group-title' },
              icon(g.ic, { size: 14 }),
              h('span', null, g.title),
              bad ? h('span', { class: 'sys-group-flag is-error' }, t('system.group.flag_error', { n: bad })) : warn ? h('span', { class: 'sys-group-flag is-warn' }, t('system.group.flag_warn', { n: warn })) : null,
            ),
            h('ul', { class: 'sys-check-list' }, g.items.map(checkRow)),
          );
        }),
    );
  }

  // desktop mode: a missing Python, package or back end is installed from Settings → Components
  function componentHint(c, st) {
    return h(
      'div',
      { class: `sys-hint is-${st}` },
      icon('info', { size: 13 }),
      h('span', null, t('system.hint.components')),
      h('a', { class: 'sys-hint-link', href: '#/settings/components', dataset: { fkey: `sys:gocomp:${c.name}` } }, t('system.open_components'), icon('chevron-right', { size: 13 })),
    );
  }

  function checkRow(c) {
    const st = checkState(c);
    const provider = c.name.startsWith('api_key:') || c.name.startsWith('api:') || c.name.startsWith('role_model:');
    const toComponents = desktopMode && st !== 'ok' && st !== 'pending' && !!c.hint_info && COMPONENT_HINTS.has(c.hint_info.code);
    const installLink = st !== 'ok' && st !== 'pending' && !!c.hint_info && INSTALL_HINTS.has(c.hint_info.code);
    const ownLink = st !== 'ok' && st !== 'pending' && !!c.hint_info && HINT_LINKS[c.hint_info.code];
    return h(
      'li',
      { class: `sys-check is-${st}` },
      h('span', { class: 'sys-check-icon', title: statusText(st) }, icon(STATUS_ICON[st], { size: 16, class: st === 'pending' ? 'spin' : '', label: statusText(st) })),
      h(
        'div',
        { class: 'sys-check-main' },
        h('div', { class: 'sys-check-name' }, checkName(c.name), st !== 'ok' && st !== 'pending' ? h('span', { class: `sys-check-state is-${st}` }, statusText(st)) : null),
        c.detail || c.detail_info ? h('div', { class: 'sys-check-detail' }, checkText(c.detail_info, c.detail)) : null,
        toComponents
          ? componentHint(c, st)
          : c.hint || c.hint_info
          ? h(
              'div',
              { class: `sys-hint is-${st}` },
              icon('info', { size: 13 }),
              h('span', null, checkText(c.hint_info, c.hint)),
              provider && st !== 'ok' ? h('a', { class: 'sys-hint-link', href: '#/settings', dataset: { fkey: `sys:goset:${c.name}` } }, t('system.open_settings'), icon('chevron-right', { size: 13 })) : null,
              installLink ? h('a', { class: 'sys-hint-link', href: '#/settings/components', dataset: { fkey: `sys:gocomp:${c.name}` } }, t('system.open_components'), icon('chevron-right', { size: 13 })) : null,
              ownLink ? h('a', { class: 'sys-hint-link', href: ownLink[0], dataset: { fkey: `sys:golink:${c.name}` } }, t(ownLink[1]), icon('chevron-right', { size: 13 })) : null,
              st !== 'pending' && restrictable(c) ? dataDir.button(c) : null,
            )
          : null,
        c.name === 'browser_ext' ? extBlock(c) : null,
      ),
    );
  }

  // ---- ssh-1b: the SSH servers ----

  const testing = new Set(); // server ids whose "测试连接" runs
  const testErrors = {};
  async function testServer(v) {
    if (testing.has(v.id)) return;
    testing.add(v.id);
    delete testErrors[v.id];
    preserveFocus(render);
    try {
      noteServer(await appRequest('POST', `/api/remote/servers/${encodeURIComponent(v.id)}/test`));
    } catch (e) {
      testErrors[v.id] = requestError(e, v.name);
      loadServers();
    }
    testing.delete(v.id);
    if (alive) preserveFocus(render);
  }

  function remoteRow(v, r) {
    const st = r.status;
    return h(
      'li',
      { class: `sys-check is-${st}`, dataset: { row: r.key } },
      h('span', { class: 'sys-check-icon', title: statusText(st) }, icon(STATUS_ICON[st], { size: 16, label: statusText(st) })),
      h(
        'div',
        { class: 'sys-check-main' },
        h('div', { class: 'sys-check-name' }, r.label, st !== 'ok' ? h('span', { class: `sys-check-state is-${st}` }, statusText(st)) : null),
        h('div', { class: 'sys-check-detail' }, r.value),
        r.hint ? h('div', { class: `sys-hint is-${st}` }, icon('info', { size: 13 }), h('span', null, r.hint)) : null,
      ),
    );
  }

  function remoteCard() {
    const list = sshServers();
    if (!sshEnabled() || !list.length) return null;
    return h(
      'section',
      { class: 'card card-flush sys-checks sys-remote', 'aria-label': t('settings.remote.title') },
      h(
        'header',
        { class: 'card-hd' },
        h('h2', { class: 'card-title' }, icon('plug', { size: 16 }), h('span', null, t('settings.remote.title')), h('span', { class: 'card-count' }, String(list.length))),
        h('a', { class: 'sys-hint-link', href: '#/settings/remote', dataset: { fkey: 'sys:goremote' } }, t('system.open_settings'), icon('chevron-right', { size: 13 })),
      ),
      list.map((v) => {
        const rows = diagRows(v);
        if (testErrors[v.id]) rows[0] = { ...rows[0], status: 'error', hint: testErrors[v.id] };
        const bad = rows.filter((r) => r.status === 'error').length;
        const warn = rows.filter((r) => r.status === 'warn').length;
        return h(
          'div',
          { class: 'sys-group', dataset: { server: v.id } },
          h(
            'h3',
            { class: 'sys-group-title' },
            icon('server', { size: 14 }),
            h('span', null, v.name),
            bad ? h('span', { class: 'sys-group-flag is-error' }, t('system.group.flag_error', { n: bad })) : warn ? h('span', { class: 'sys-group-flag is-warn' }, t('system.group.flag_warn', { n: warn })) : null,
            v.trusted
              ? h(
                  'button',
                  { type: 'button', class: 'btn btn-sm btn-ghost sys-remote-test', disabled: testing.has(v.id), dataset: { fkey: `sys:remote:${v.id}:test` }, onclick: () => testServer(v) },
                  icon(testing.has(v.id) ? 'loader-circle' : 'plug', { size: 14, class: testing.has(v.id) ? 'spin' : '' }),
                  h('span', { class: 'btn-label' }, t('settings.provider.test')),
                )
              : null,
          ),
          h('ul', { class: 'sys-check-list' }, rows.map((r) => remoteRow(v, r))),
        );
      }),
    );
  }

  // ext web3: the same element in every render (its state survives them)
  function extBlock(c) {
    if (!ext) ext = createBrowserExt({ recheck });
    ext.setRow(c);
    return ext.el;
  }

  // ---- resources ----

  function panel(ic, title, body, extra) {
    return h('section', { class: 'card sys-panel' }, h('header', { class: 'card-hd' }, h('h2', { class: 'card-title' }, icon(ic, { size: 16 }), h('span', null, title)), extra), h('div', { class: 'card-bd' }, body));
  }

  function meter(fraction, tone) {
    const f = Math.max(0, Math.min(1, fraction || 0));
    return h('div', { class: `meter meter-${tone}`, role: 'img', 'aria-label': U.fmtPercent(f, 0) }, h('div', { class: 'meter-fill', style: { width: `${(f * 100).toFixed(1)}%` } }));
  }

  function kvList(pairs) {
    return h(
      'dl',
      { class: 'sys-kv' },
      pairs.filter(Boolean).map(([k, v, cls]) => [h('dt', null, k), h('dd', { class: cls || '' }, v)]),
    );
  }

  function gpuSig() {
    const r = ctx.resources();
    return r && r.gpu ? `${r.gpu.name}|${r.gpu.backend}|${r.gpu.available}` : '';
  }

  // The GPU card is the NVIDIA card's: only where the server shows the GPU
  // check (an NVIDIA GPU; never on a Mac, mac-opti M3; ext-step1: not with
  // only Colab, whose page has no device list either).
  function nvidiaShown(sys) {
    return (sys.checks || []).some((c) => c.name === 'gpu');
  }

  function gpuCard(sys) {
    const g = sys.gpu;
    const res = ctx.resources();
    const live = res && res.gpu && res.gpu.available ? res.gpu : null;
    const used = g.total_mb ? g.total_mb - g.free_mb : 0;
    const frac = g.total_mb ? used / g.total_mb : 0;
    return panel(
      'cpu',
      // with the device list (I10) this card is the NVIDIA card's
      sys.devices ? t('devices.kind.cuda') : t('system.group.gpu'),
      [
        live ? h('div', { class: 'sys-gpu-name' }, h('span', { class: 'sys-gpu-model' }, live.name), live.backend ? h('span', { class: 'chip chip-quiet mono' }, live.backend) : null) : null,
        g.total_mb
          ? h(
              'div',
              { class: 'sys-metric' },
              h('div', { class: 'sys-metric-top' }, h('span', { class: 'sys-metric-k' }, t('system.gpu.vram')), h('span', { class: 'sys-metric-v' }, `${U.fmtMB(used)} `, h('small', null, `/ ${U.fmtMB(g.total_mb)}`))),
              meter(frac, frac > 0.9 ? 'warn' : 'accent'),
            )
          : h('p', { class: 'sys-empty' }, t('system.gpu.no_vram')),
        kvList([
          [t('system.gpu.asr_jobs'), h('span', { class: 'sys-slots' }, Array.from({ length: Math.max(g.slots_max, 1) }, (_, i) => h('span', { class: 'slot' + (i < g.slots_used ? ' is-on' : '') })), ` ${g.slots_used} / ${g.slots_max}`), 'is-num'],
          [t('system.gpu.per_job'), U.fmtMB(g.estimated_job_mb), 'is-num'],
          [t('system.gpu.gpu_jobs'), g.paused ? h('span', { class: 'tone-warn' }, t('system.gpu.paused')) : t('system.gpu.running')],
        ]),
        // R37 recheck E1: why the estimate is above the table's value
        g.estimate_raised ? h('p', { class: 'sys-gpu-note' }, icon('info', { size: 13 }), t('system.gpu.per_job_raised')) : null,
      ],
    );
  }

  // ---- transcription devices, power, OpenVINO (SPEC-v2 I10) ----

  function powerText(p) {
    if (!p || !p.known) return t('system.power.unknown');
    if (!p.on_battery) return p.has_battery && p.charging ? [t('system.power.ac'), t('resources.power.charging')].join(' · ') : t('system.power.ac');
    return p.battery_pct != null ? t('system.power.battery_pct', { pct: U.fmtPercent(p.battery_pct / 100, 0) }) : t('system.power.battery');
  }

  function deviceState(d) {
    const chips = [];
    if (!d.present) chips.push(h('span', { class: 'chip chip-warn', title: t('devices.not_present_tip') }, t('devices.not_present')));
    if (!d.enabled) chips.push(h('span', { class: 'chip chip-quiet' }, t('devices.disabled')));
    if (d.paused) chips.push(h('span', { class: 'chip chip-warn' }, t('devices.paused')));
    if (!chips.length) chips.push(h('span', { class: 'chip chip-ok' }, t('system.devices.ready')));
    return chips;
  }

  function devicesCard(sys) {
    const devs = (sys.devices || []).slice().sort((a, b) => b.priority - a.priority);
    const ov = sys.openvino;
    const ovByID = new Map(((ov && ov.devices) || []).filter((x) => x.device_id).map((x) => [x.device_id, x]));
    const rows = devs.map((d) => {
      const o = ovByID.get(d.id);
      const sub = [U.deviceKindName(d.kind), d.backend === 'openvino' ? 'OpenVINO' : d.backend === 'mlx' ? 'MLX' : d.backend];
      if (o) sub.push(o.name, o.driver_version ? t('system.openvino.driver', { version: o.driver_version }) : '');
      return h(
        'tr',
        { class: d.enabled && d.present ? '' : 'is-muted' },
        h('th', { scope: 'row' }, h('span', { class: 'sys-dev-name', title: d.label }, d.label), h('span', { class: 'sys-dev-sub' }, sub.filter(Boolean).join(' · '))),
        h('td', { class: 'mono' }, U.comboText(d.model, d.precision)),
        h('td', { title: d.detected_at ? t('system.devices.detected_at', { when: U.relTime(d.detected_at) }) : undefined }, h('span', { class: 'sys-dev-state' }, deviceState(d))),
      );
    });
    // r2 N15: below the table, in the checks' warning style
    const vramHints = devs.map(vramHintRow).filter(Boolean);
    const ovBlock = ov
      ? h(
          'div',
          { class: 'sys-ov' },
          h('h3', { class: 'sys-group-title' }, icon('cpu', { size: 14 }), h('span', null, 'OpenVINO'), ov.detected_at ? h('span', { class: 'sys-ov-at' }, t('system.devices.detected_at', { when: U.relTime(ov.detected_at) })) : null),
          ov.available
            ? kvList([
                [t('system.openvino.python'), h('span', { class: 'mono sys-ov-path', title: ov.python }, ov.python)],
                ov.version ? ['OpenVINO', h('span', { class: 'mono' }, ov.version)] : null,
                ov.genai_version ? ['GenAI', h('span', { class: 'mono' }, ov.genai_version)] : null,
                [t('system.openvino.convert'), ov.can_convert ? t('system.openvino.convert_ok') : h('span', { class: 'tone-warn' }, t('system.openvino.convert_no'))],
              ])
            : h('p', { class: 'sys-empty tone-warn' }, icon('triangle-alert', { size: 13 }), ov.error_info ? U.errorInfoText(ov.error_info) : t('system.openvino.unavailable')),
          (ov.ignored || []).length
            ? h(
                'div',
                { class: 'sys-ov-ignored' },
                h('div', { class: 'sys-disk-cap' }, t('system.openvino.ignored')),
                h(
                  'ul',
                  { class: 'sys-ov-list' },
                  ov.ignored.map((x) => {
                    // ext-step1: without an NVIDIA GPU another vendor's card is not "for CUDA"
                    const reason = x.reason === 'not_intel' && !nvidiaShown(sys) ? 'not_intel_plain' : x.reason;
                    const why = ['not_intel', 'not_intel_plain', 'second_intel_gpu', 'error'].includes(reason) ? t(`system.openvino.reason.${reason}`) : x.reason || '';
                    return h('li', null, h('span', { class: 'sys-dev-name', title: x.full_name || x.name }, x.full_name || x.name), h('span', { class: 'sys-dev-sub' }, [x.name, why].filter(Boolean).join(' · ')));
                  }),
                ),
              )
            : null,
        )
      : null;
    return h(
      'section',
      { class: 'card sys-devices' },
      h(
        'header',
        { class: 'card-hd' },
        h('h2', { class: 'card-title' }, icon('mic', { size: 16 }), h('span', { id: DEVICES_TITLE_ID }, t('system.group.gpu')), devs.length ? h('span', { class: 'card-count' }, String(devs.length)) : null),
        h('a', { class: 'card-link', href: '#/settings/asr', dataset: { fkey: 'sys:dev:settings' } }, t('system.open_settings'), icon('chevron-right', { size: 13 })),
      ),
      h(
        'div',
        { class: 'card-bd sys-devices-bd' },
        h(
          'div',
          { class: 'sys-devices-main' },
          devs.length
            ? deviceTableWrap(
                h(
                  'table',
                  { class: 'sys-dev-table' },
                  h('thead', null, h('tr', null, h('th', { scope: 'col' }, t('settings.asr.device')), h('th', { scope: 'col' }, t('selftest.col.combo')), h('th', { scope: 'col' }, t('system.devices.col.state')))),
                  h('tbody', null, rows),
                ),
              )
            : h('p', { class: 'sys-empty' }, t('devices.empty_title')),
          ...vramHints,
          kvList([[t('system.power.label'), h('span', { class: sys.power && sys.power.on_battery ? 'tone-warn' : '' }, powerText(sys.power))]]),
        ),
        ovBlock,
      ),
    );
  }

  function apiCard(sys) {
    const rows = Object.entries(sys.api || {}).sort((a, b) => a[0].localeCompare(b[0]));
    return panel(
      'activity',
      t('system.api.title'),
      rows.length
        ? h(
            'div',
            { class: 'sys-api' },
            rows.map(([name, a]) =>
              h(
                'div',
                { class: 'sys-metric' },
                h('div', { class: 'sys-metric-top' }, h('span', { class: 'sys-metric-k strong' }, name), h('span', { class: 'sys-metric-v' }, `${a.inflight} `, h('small', null, `/ ${a.rpm_limit > 0 ? t('system.api.limits', { max: a.max, rpm: a.rpm_limit }) : t('system.api.limits_unlimited', { max: a.max })}`))),
                meter(a.max ? a.inflight / a.max : 0, a.inflight >= a.max && a.max ? 'warn' : 'accent'),
              ),
            ),
          )
        : h('p', { class: 'sys-empty' }, t('system.api.none'), h('a', { href: '#/settings' }, t('system.api.add'))),
      h('a', { class: 'card-link', href: '#/settings', dataset: { fkey: 'sys:api:settings' } }, t('system.api.settings'), icon('chevron-right', { size: 13 })),
    );
  }

  function usageCard(sys) {
    const u = sys.usage_today;
    const rate = U.cacheHitRate(u);
    const budget = Number(sys.config && sys.config.general && sys.config.general.daily_budget) || 0;
    return panel('coins', t('system.usage.title'), [
      h('div', { class: 'sys-usage-big' }, u.estimated_cost != null ? h('span', { class: 'sys-usage-money' }, U.fmtCost(u.estimated_cost)) : h('span', { class: 'sys-usage-money is-quiet' }, t('system.usage.no_price')), h('span', { class: 'sys-usage-k' }, t('system.usage.cost'))),
      budget > 0 && u.estimated_cost != null
        ? h(
            'div',
            { class: 'sys-metric' },
            h('div', { class: 'sys-metric-top' }, h('span', { class: 'sys-metric-k' }, t('system.usage.budget', { budget: U.fmtDecimal(budget, 2) })), h('span', { class: 'sys-metric-v' }, t('system.usage.used', { pct: U.fmtPercent(u.estimated_cost / budget, 0) }))),
            meter(u.estimated_cost / budget, u.estimated_cost >= budget ? 'warn' : 'ok'),
          )
        : null,
      kvList([
        [t('system.usage.input'), U.fmtTokens(u.prompt_tokens), 'is-num'],
        [t('system.usage.cache_hit'), rate != null ? t('system.usage.cache_hit_value', { tokens: U.fmtTokens(u.cache_hit_tokens), pct: U.fmtPercent(rate) }) : U.fmtTokens(u.cache_hit_tokens), 'is-num'],
        [t('system.usage.output'), U.fmtTokens(u.completion_tokens), 'is-num'],
        [t('system.usage.budget_today'), sys.budget_hit ? h('span', { class: 'tone-warn' }, t('system.usage.budget_hit')) : budget > 0 ? t('system.usage.budget_ok') : t('system.usage.unlimited')],
      ]),
    ]);
  }

  function diskCard(sys) {
    const d = sys.disk;
    const entries = Object.entries(d.courses_mb || {}).sort((a, b) => b[1] - a[1]);
    const maxMB = entries.length ? Math.max(...entries.map((e) => e[1]), 1) : 1;
    const courses = new Map(ctx.courses().map((c) => [c.id, c]));
    const low = d.free_mb > 0 && d.free_mb < 5 * 1024;
    return panel('hard-drive', t('system.disk.title'), [
      kvList([
        [t('system.disk.free'), h('span', { class: low ? 'tone-warn' : '' }, U.fmtMB(d.free_mb)), 'is-num'],
        [t('system.disk.data'), U.fmtMB(d.data_mb), 'is-num'],
        // v2 P8.1: the playback files, part of the data folder size
        d.stream_mb != null ? [h('span', { title: t('system.disk.stream_tip') }, t('system.disk.stream')), U.fmtMB(d.stream_mb), 'is-num'] : null,
      ]),
      entries.length
        ? h(
            'div',
            { class: 'sys-disk' },
            h('div', { class: 'sys-disk-cap' }, t('system.disk.per_course')),
            h(
              'ul',
              { class: 'sys-disk-list' },
              entries.map(([id, size]) => {
                const c = courses.get(id);
                return h(
                  'li',
                  { class: 'sys-disk-row' },
                  h(
                    'span',
                    { class: 'sys-disk-name', title: c ? c.name : id },
                    c ? [c.code ? h('span', { class: 'sys-disk-code' }, c.code) : null, h('span', null, c.name)] : h('span', { class: 'mono dim' }, id),
                  ),
                  h('span', { class: 'sys-disk-bar' }, h('span', { style: { width: `${((size / maxMB) * 100).toFixed(1)}%` } })),
                  h('span', { class: 'sys-disk-v' }, U.fmtMB(size)),
                );
              }),
            ),
          )
        : null,
    ]);
  }

  // ---- configuration ----

  // 3.1.0: the interpreters (python) and the devices (asr_devices) are
  // lists with a table each; the other sections are flattened key / value
  // rows. Tables in this order, then any other section.
  // nvidia: the server shows the GPU row (ext-step1: the faster-whisper
  // interpreter is named after the CPU otherwise).
  function configCard(cfg, nvidia) {
    const scalars = Object.entries(cfg).filter(([, v]) => v === null || typeof v !== 'object');
    const objects = Object.entries(cfg)
      .filter(([, v]) => v && typeof v === 'object')
      .sort((a, b) => configOrder(a[0]) - configOrder(b[0]));
    const row = (key, label, v) =>
      h('tr', null, h('th', { scope: 'row' }, h('span', { class: 'mono' }, key.split('.').pop()), label ? h('span', { class: 'sys-cfg-label' }, label) : null), h('td', { class: 'mono' }, fmtValue(v)));
    const flatten = (prefix, obj) =>
      Object.entries(obj)
        .flatMap(([k, v]) => (v && typeof v === 'object' && !Array.isArray(v) ? flatten(`${prefix}.${k}`, v) : [[`${prefix}.${k}`, v]]))
        .filter(([k]) => !CONFIG_HIDDEN.has(k));
    const caption = (k) => h('caption', null, configLabel(k) ? t('system.config.caption', { label: configLabel(k), key: k }) : k);
    const section = (k, v) => {
      if (k === 'python' && Array.isArray(v)) return h('table', { class: 'sys-cfg' }, caption(k), h('tbody', null, v.map((p) => pythonRow(p, nvidia))));
      if (k === 'asr_devices' && Array.isArray(v)) {
        return h('table', { class: 'sys-cfg' }, caption(k), h('tbody', null, v.length ? v.map(deviceRow) : h('tr', null, h('td', { colspan: 2, class: 'dim' }, t('devices.empty_title')))));
      }
      return h('table', { class: 'sys-cfg' }, caption(k), h('tbody', null, flatten(k, v).map(([kk, vv]) => row(kk, configLabel(kk), vv))));
    };
    return h(
      'section',
      { class: 'card sys-config' },
      h(
        'header',
        { class: 'card-hd' },
        h('h2', { class: 'card-title' }, icon('braces', { size: 16 }), h('span', null, t('system.config.title'))),
        h(
          'div',
          { class: 'card-tools' },
          btn(t('system.config.copy'), {
            size: 'sm',
            icon: 'copy',
            fkey: 'sys:copycfg',
            onclick: async () => toast((await copyText(JSON.stringify(cfg, null, 2))) ? t('system.config.copied') : t('system.config.copy_failed'), 'info'),
          }),
        ),
      ),
      h(
        'div',
        { class: 'card-bd' },
        h('p', { class: 'sys-cfg-note' }, icon('info', { size: 13 }), withLink(t('system.config.note'), h('a', { href: '#/settings' }, t('system.config.note_link')))),
        Object.keys(cfg).length === 0
          ? h('p', { class: 'sys-empty' }, t('system.config.empty'))
          : h(
              'div',
              { class: 'sys-cfg-grid' },
              scalars.length ? h('table', { class: 'sys-cfg' }, h('caption', null, t('system.config.startup')), h('tbody', null, scalars.map(([k, v]) => row(k, configLabel(k), v)))) : null,
              objects.map(([k, v]) => section(k, v)),
            ),
      ),
    );
  }

  // One interpreter: the backend with the component it belongs to, the
  // path and where it comes from; "not installed" without a path.
  function pythonRow(p, nvidia) {
    const name = p.backend === 'faster-whisper' && !nvidia ? 'system.config.python.fw_cpu' : PYTHON_COMPONENT[p.backend];
    let value;
    if (p.source === 'none' || !p.path) {
      value = h('span', { class: 'dim' }, t('system.config.python.none'));
    } else {
      const src = PYTHON_SOURCE[p.source];
      value = [
        h('span', { class: 'mono' }, p.path),
        h(
          'span',
          { class: 'sys-cfg-label' },
          src ? t(src) : p.source,
          p.exists ? null : [' · ', h('span', { class: 'tone-warn' }, t('system.config.python.missing'))],
        ),
      ];
    }
    return h('tr', null, h('th', { scope: 'row' }, h('span', { class: 'mono' }, p.backend), name ? h('span', { class: 'sys-cfg-label' }, t(name)) : null), h('td', null, value));
  }

  // One transcription device: what it runs with (its own model, precision
  // and parallel tasks: the global ones of the legacy settings are not in
  // effect with the device table).
  function deviceRow(d) {
    const sub = [`${t('devices.max_parallel')} ${d.max_parallel}`];
    if (!d.enabled) sub.push(t('devices.disabled'));
    return h(
      'tr',
      { class: d.enabled ? '' : 'is-muted' },
      h('th', { scope: 'row' }, h('span', { class: 'mono' }, d.id), d.label ? h('span', { class: 'sys-cfg-label' }, d.label) : null),
      h('td', null, h('span', { class: 'mono' }, [d.backend, U.comboText(d.model, d.precision)].filter(Boolean).join(' · ')), h('span', { class: 'sys-cfg-label' }, sub.join(' · '))),
    );
  }

  function fmtValue(v) {
    if (v === null || v === undefined) return h('span', { class: 'dim' }, 'null');
    if (typeof v === 'boolean') return h('span', { class: v ? 'tone-ok' : 'tone-muted' }, String(v));
    if (Array.isArray(v)) return v.length ? v.map(String).join(', ') : h('span', { class: 'dim' }, '[]');
    if (v === '') return h('span', { class: 'dim' }, '""');
    return String(v);
  }

  render();
  refresh();
  loadServers();
  const serversUnsub = onServersChange(() => alive && ctx.system() && preserveFocus(render));

  return {
    refresh,
    onSystemChanged: () => {
      preserveFocus(render);
      if (ext) ext.refresh();
    },
    onCoursesChanged: () => preserveFocus(render),
    onResources() {
      if (ctx.system() && gpuSig() !== gpuKey) preserveFocus(render);
    },
    destroy() {
      alive = false;
      if (ext) ext.destroy();
      dataDir.destroy();
      serversUnsub();
    },
  };
}
