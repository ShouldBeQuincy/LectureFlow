// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// HTTP calls of the onboarding (SPEC-v3 V4) and of the component manager
// (V3, .claude/v3-brief.md §7.2-§7.4) that api.js does not have yet. Same
// conventions as api.js: JSON in and out, failures become ApiError (shown
// through the page's error handler unless opts.quiet), mock mode appends
// ?mock=… to every request.

import { api, ApiError, errorText, MOCK, MOCK_SCENARIO } from './api.js';
import { t } from './i18n.js';
import { toast } from './dom.js';

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

let onError = null;

/** The handler for failed requests that are not quiet (the onboarding page shows a toast). */
export function setErrorHandler(fn) {
  onError = fn;
}

/** The handler set now (the settings' preset dialog puts back only its own). */
export function errorHandler() {
  return onError;
}

async function request(method, path, body, opts = {}) {
  const init = { method, cache: 'no-store', headers: {} };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const fail = (err) => {
    if (!opts.quiet && onError) onError(err);
    return err;
  };
  let res;
  try {
    res = await fetch(withMock(path), init);
  } catch {
    throw fail(new ApiError(0, t('common.net.unreachable')));
  }
  const text = await res.text().catch(() => '');
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) throw fail(new ApiError(res.status, errorText(res.status, parsed), parsed));
  if (text && parsed === null && text.trim() !== 'null') throw fail(new ApiError(res.status, t('common.net.bad_response')));
  return parsed;
}

const get = (p, o) => request('GET', p, undefined, o);
const post = (p, b, o) => request('POST', p, b, o);
const put = (p, b, o) => request('PUT', p, b, o);
const enc = encodeURIComponent;

export const obApi = {
  app: (o) => get('/api/app', o), // AppInfo
  onboarding: (o) => get('/api/onboarding', o), // OnboardingState {step, done, data}
  saveOnboarding: (state, o) => put('/api/onboarding', state, o), // OnboardingState
  envKeys: (o) => get('/api/onboarding/env-keys', o), // [{provider_type, env, present, masked}]
  useEnvKey: (providerType, o) => post('/api/onboarding/use-env-key', { provider_type: providerType }, o), // ProviderView
  hardware: (o) => get('/api/hardware', o), // {hw, recommendation}
  detectHardware: (o) => post('/api/hardware/detect', undefined, o), // {hw, recommendation}
  components: (o) => get('/api/components', o), // {platform, manifest, components, tasks}
  plan: (ids, o) => post('/api/components/plan', { ids }, o), // {ids, download_bytes, install_bytes, free_bytes, enough_space, need_bytes}
  install: (ids, o) => post('/api/components/install', { ids }, o), // 202 {task}
  tasks: (o) => get('/api/components/tasks', o), // [TaskStatus]
  taskAction: (id, action, o) => post(`/api/components/tasks/${enc(id)}/${enc(action)}`, undefined, o), // {task}
  sources: (o) => get('/api/components/sources', o), // Sources
  saveSources: (s, o) => put('/api/components/sources', { pypi: s.pypi, models: s.models }, o), // Sources
  presets: (o) => get('/api/components/presets', o), // Presets
  startImport: (path, o) => post('/api/app/import', { path }, o), // 202 import status
  // The import status: GET /api/app/import, or GET /api/app/data-dir where
  // the server has no such route (404). Resolves to {state, copied_bytes, total_bytes, error?}.
  async importStatus(o = {}) {
    try {
      return await get('/api/app/import', { quiet: true });
    } catch (e) {
      if (e.status !== 404) {
        if (!o.quiet && onError) onError(e);
        throw e;
      }
    }
    const d = await get('/api/app/data-dir', o);
    return (d && (d.import || d.move || d)) || null;
  },
  // 3.1.0 (.claude/v31-brief.md §2): the data folder, the Colab sign-in
  dataDir: (o) => get('/api/app/data-dir', o), // DataDirStatus {data_dir, can_move, free_bytes, state, …}
  checkDataDir: (path, o) => post('/api/app/data-dir/check', { path }, o), // {path, free_bytes, ok, error}
  moveDataDir: (path, o) => post('/api/app/data-dir', { path }, o), // 202 DataDirStatus
  health: (o) => get('/api/health', o),
  colab: (o) => get('/api/colab', o), // ColabView {status, settings, adc, held}
  colabSettings: (patch, o) => put('/api/colab/settings', patch, o), // ColabView
  colabLogin: (o) => get('/api/colab/auth/login', o), // ColabLogin {state, url, started_at, error}
  startColabLogin: (o) => post('/api/colab/auth/login', undefined, o), // 202 ColabLogin
  cancelColabLogin: (o) => request('DELETE', '/api/colab/auth/login', undefined, o), // ColabLogin
  // existing endpoints, re-exported for the onboarding modules
  settings: api.settings,
  providers: api.providers,
  createProvider: (input, o) => post('/api/providers', input, o),
  patchProvider: (id, input, o) => request('PATCH', `/api/providers/${enc(id)}`, input, o),
  testProvider: (id, o) => post(`/api/providers/${enc(id)}/test`, undefined, o),
  probeModels: (id, o) => post(`/api/providers/${enc(id)}/models`, undefined, o),
  setRole: (role, providerId, model, o) => put(`/api/settings/roles/${enc(role)}`, { provider_id: providerId, model, params: {} }, o),
  devices: (o) => get('/api/asr/devices', o),
  startSelfTest: (id, o) => post(`/api/asr/devices/${enc(id)}/selftest`, undefined, o),
  selfTest: (id, o) => get(`/api/asr/devices/${enc(id)}/selftest`, o),
  cancelSelfTest: (id, o) => request('DELETE', `/api/asr/devices/${enc(id)}/selftest`, undefined, o),
  applySelfTest: (id, model, precision, o) => post(`/api/asr/devices/${enc(id)}/selftest/apply`, { model, precision }, o),
};

/** The desktop shell's bridge (SPEC-v3 V1.4), or null in a browser. */
export function desktop() {
  return typeof window !== 'undefined' && window.lectureflow && typeof window.lectureflow === 'object' ? window.lectureflow : null;
}

// The desktop shell opens at most 3 links in 10 seconds and refuses the
// next one (desktop/src/main/ipc.js, lib/ratelimit.js). Its error reaches
// the page through IPC as "Error invoking remote method
// 'lf:open-external': Error: openExternal: too many links in a short
// time", so the main process's own words are looked for anywhere in it.
const LINK_THROTTLED = 'too many links in a short time';

/** Whether openExternal's rejection is the shell's link limit. */
export function linkThrottled(err) {
  const msg = err && typeof err === 'object' ? err.message : err;
  return typeof msg === 'string' && msg.includes(LINK_THROTTLED);
}

/**
 * The catch of every window.lectureflow.openExternal call: a link refused
 * by the shell's limit says so in a notice (r4: a click that did nothing,
 * with no reason given); any other failure (no user activation, a URL the
 * shell refuses) stays silent as before.
 */
export function externalLinkFailed(err) {
  if (linkThrottled(err)) toast(t('common.link_throttled'), 'info');
}

/** Opens an external page: through the desktop shell, else a new tab. */
export function openExternal(url) {
  const d = desktop();
  if (d && typeof d.openExternal === 'function') {
    Promise.resolve(d.openExternal(url)).catch(externalLinkFailed);
    return;
  }
  window.open(url, '_blank', 'noopener');
}

/** The recommendation's components, without the bundled core. */
export function recommendedComponents(rec) {
  return ((rec && rec.components) || []).filter((id) => id !== 'core');
}

const LOCAL_BACKENDS = ['asr-cuda', 'asr-openvino', 'asr-mlx'];

/**
 * Which transcription mode the hardware detection recommends (3.1.0):
 * 'local' when it recommends a local back end other than the CPU fallback,
 * 'remote' when this computer has only the CPU (or no back end at all),
 * '' when nothing is known.
 */
export function recommendedMode(rec) {
  if (!rec) return '';
  const cpuOnly = (rec.reasons || []).some((r) => r && r.code === 'hw.reason.cpu_only');
  const backend = (rec.components || []).some((id) => LOCAL_BACKENDS.includes(id));
  return backend && !cpuOnly ? 'local' : 'remote';
}
