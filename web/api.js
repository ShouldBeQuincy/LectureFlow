// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// HTTP JSON API client (SPEC §8, §17.1, v1.1 brief §5), XHR uploads (§9.4)
// and SSE (§8.2, v1.1 A10 "resources").
//
// Mock mode (?mock=… in the page URL, served by `go run ./internal/api/mockserve`):
// every request goes to the mock server like it would to the real one, with
// the scenario appended as ?mock=…; the front end renders only what the API
// returns. Scenarios: 1 (default), empty, nogpu, error, slow, diag, many.

import { t } from './i18n.js';
import { codedText } from './util.js';

const params = new URLSearchParams(location.search);
export const MOCK = params.has('mock');
export const MOCK_SCENARIO = params.get('mock') || '1';
// Development mode (SPEC-v2 P14.2, P14.9): the mock, or ?dev=1 on the real
// server. Missing UI keys show as ⟦key⟧ and the en-XA pseudo-locale is offered.
export const DEV = MOCK || params.get('dev') === '1';

export class ApiError extends Error {
  constructor(status, message, body = null) {
    super(message);
    this.status = status; // HTTP status; 0 = network failure; -1 = aborted by the user
    this.body = body; // parsed JSON error body, if any
    // v2 P14.3: {"error": {"code", "params", "detail"}}; "" for the v1.1 string form
    const e = body && body.error;
    this.code = e && typeof e === 'object' ? e.code || '' : '';
    this.params = e && typeof e === 'object' ? e.params || {} : {};
    this.detail = e && typeof e === 'object' ? e.detail || '' : '';
  }
  // v1.1 A4.3: 409 {"error":"needs_model_choice","diff":[...]}; v2:
  // {"error":{"code":"needs_model_choice"},"diff":[...]}
  get needsModelChoice() {
    return this.status === 409 && !!this.body && (this.body.error === 'needs_model_choice' || this.code === 'needs_model_choice');
  }
}

let onError = null;

// onError(ApiError) is called for every failed request (SPEC §9.8: show the
// back end's error text). needs_model_choice is not an error for the user:
// the caller answers it with the model-choice dialog.
export function setHandlers(h) {
  onError = h.onError || null;
}

// err.reported: the error was handed to onError (the app toasts it), so
// who would tell it elsewhere need not (dom.js: a dialog's busy request).
function report(err) {
  if (onError && err.status !== -1 && !err.needsModelChoice) {
    err.reported = true;
    onError(err);
  }
  return err;
}

function parseBody(text) {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

// The text of an error body: the v1.1 string, or t("errors."+code, params)
// of the v2 form (P14.3, util.codedText), falling back to its detail when
// the code has no translation (lead: transition period).
export function errorText(status, body) {
  const e = body && body.error;
  if (typeof e === 'string' && e) return e;
  if (e && typeof e === 'object') {
    const s = codedText('errors', e, '');
    if (s) return s;
  }
  return t('errors.http', { status });
}

/** The text of a coded error (ErrorInfo) outside an HTTP response: job errors, batch items. */
export { errorInfoText } from './util.js';

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

async function send(path, init) {
  const res = await fetch(withMock(path), init);
  const text = await res.text().catch(() => '');
  return { res, text };
}

// perf-fix: GETs of the same read issued together share one request (on
// load the dashboard and app.js both read /api/settings, the diagnostics
// page and app.js /api/system; each of those used to run nvidia-smi). Only
// the reads below, only while in flight and only within SHARE_MS of the
// first; any write through request() and any server event end the sharing,
// so a read issued after a change never gets an answer from before it.
// Every caller parses the text itself: no two get the same object.
const SHARED_GETS = new Set(['/api/settings', '/api/system', '/api/app']);
const SHARE_MS = 100;
const inflight = new Map(); // path → {p: Promise<{res, text}>, at}

function sharedGet(path, init) {
  const e = inflight.get(path);
  if (e && performance.now() - e.at < SHARE_MS) return e.p;
  const entry = { p: null, at: performance.now() };
  entry.p = send(path, init).finally(() => {
    if (inflight.get(path) === entry) inflight.delete(path);
  });
  inflight.set(path, entry);
  return entry.p;
}

function forgetShared() {
  inflight.clear();
}

// opts.quiet: do not toast failures (optional data the page can do without).
// opts.signal: an AbortSignal; an aborted request rejects with the browser's
// AbortError (name === 'AbortError') and is never toasted (a request with a
// signal is never shared).
async function request(method, path, body, opts = {}) {
  // err.method: the request's HTTP method (upper case), so a read's failure
  // can be told apart from an action's (round 8: app.js onRequestError,
  // dom.js tellFailure: while the connection is lost only actions are told)
  const fail = (err) => {
    err.method = method;
    return opts.quiet ? err : report(err);
  };
  const init = { method, cache: 'no-store', headers: {}, signal: opts.signal };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const write = method !== 'GET';
  if (write) forgetShared();
  let res, text;
  try {
    ({ res, text } = !write && !opts.signal && SHARED_GETS.has(path) ? await sharedGet(path, init) : await send(path, init));
  } catch (e) {
    if (opts.signal && opts.signal.aborted) throw e;
    throw fail(new ApiError(0, t('common.net.unreachable')));
  } finally {
    if (write) forgetShared();
  }
  if (opts.signal && opts.signal.aborted) throw opts.signal.reason ?? new DOMException('Aborted', 'AbortError');
  if (!res.ok) {
    const b = parseBody(text);
    throw fail(new ApiError(res.status, errorText(res.status, b), b));
  }
  if (res.status === 204 || !text) return null;
  const v = parseBody(text);
  if (v === null && text.trim() !== 'null') throw fail(new ApiError(res.status, t('common.net.bad_response')));
  return v;
}

const get = (p, opts) => request('GET', p, undefined, opts);
const post = (p, body, opts) => request('POST', p, body, opts);
const patch = (p, body, opts) => request('PATCH', p, body, opts);
const del = (p, opts) => request('DELETE', p, undefined, opts);
const put = (p, body, opts) => request('PUT', p, body, opts);
const enc = encodeURIComponent;
// model_choice is sent only when the user picked one (A4.3).
const choiceBody = (choice) => (choice ? { model_choice: choice } : undefined);

// Every endpoint the front end uses. Response types refer to
// internal/api/types.go and types_v11.go.
export const api = {
  // §17.1
  system: () => get('/api/system'), // System
  recheck: () => post('/api/system/recheck'), // System
  // sec-update M2: the data folder made private; quiet, system_datadir.js
  // toasts the failure itself (data_dir.restrict_failed, kept until closed)
  restrictDataDir: () => post('/api/system/data-dir/restrict', undefined, { quiet: true }), // {check: SystemCheck, rollback_reset?: true}; errors may carry rollback_reset too
  setGPU: (paused, cancelRunning = false) => post('/api/system/gpu', { paused, cancel_running: cancelRunning }), // System

  // v1.1 A9, A10, A5. The dashboard, resources and settings reads are
  // quiet: their cards show the error themselves (with a retry button).
  dashboard: () => request('GET', '/api/dashboard', undefined, { quiet: true }), // Dashboard
  resources: () => request('GET', '/api/resources', undefined, { quiet: true }), // Resources
  settings: () => request('GET', '/api/settings', undefined, { quiet: true }), // Settings
  setRole: (role, providerId, model, params) => put(`/api/settings/roles/${enc(role)}`, { provider_id: providerId, model, params: params || {} }), // Settings
  setASR: (asr) => put('/api/settings/asr', asr), // Settings
  setGeneral: (general) => put('/api/settings/general', general), // Settings
  providers: () => get('/api/providers'), // []ProviderView
  createProvider: (input) => post('/api/providers', input), // ProviderView
  patchProvider: (id, input) => patch(`/api/providers/${enc(id)}`, input), // ProviderView (api_key "" = unchanged)
  deleteProvider: (id) => del(`/api/providers/${enc(id)}`), // ProviderDeleteResult | 409
  testProvider: (id) => post(`/api/providers/${enc(id)}/test`), // ProviderTest
  probeModels: (id) => post(`/api/providers/${enc(id)}/models`), // ProviderModels

  // courses
  courses: () => get('/api/courses'), // []Course
  course: (id, opts) => get(`/api/courses/${enc(id)}`, opts), // Course
  createCourse: (name, code) => post('/api/courses', code ? { name, code } : { name }), // Course
  patchCourse: (id, p, opts) => patch(`/api/courses/${enc(id)}`, p, opts), // Course | 409 RenumberConflict (v2 P13.6); p: CoursePatch
  courseStageAction: (courseId, stage, action) => post(`/api/courses/${enc(courseId)}/stages/${enc(stage)}/${enc(action)}`), // Course
  launchDrafts: (courseId) => post(`/api/courses/${enc(courseId)}/launch-drafts`), // CourseOpResult
  updateStale: (courseId, choice) => post(`/api/courses/${enc(courseId)}/update-stale`, choiceBody(choice)), // CourseOpResult | 409
  courseCiteURL: (courseId) => withMock(`/api/courses/${enc(courseId)}/export/cite.json`),
  // opts: {quiet, signal} (see request)
  syllabus: (courseId, opts) => get(`/api/courses/${enc(courseId)}/syllabus`, opts), // SyllabusDoc
  syllabusRawText: (courseId, text, opts) => post(`/api/courses/${enc(courseId)}/syllabus/raw`, { text }, opts), // SyllabusDoc
  formatSyllabus: (courseId, opts) => post(`/api/courses/${enc(courseId)}/syllabus/format`, undefined, opts), // SyllabusFormatResult (not saved)
  saveSyllabus: (courseId, markdown, opts) => put(`/api/courses/${enc(courseId)}/syllabus`, { markdown }, opts), // Course
  terms: (courseId, opts) => get(`/api/courses/${enc(courseId)}/terms`, opts), // []Term; opts {quiet, signal}
  addTerm: (courseId, en, zh) => post(`/api/courses/${enc(courseId)}/terms`, { en, zh }), // Term
  patchTerm: (id, p) => patch(`/api/terms/${enc(id)}`, p), // Term; p: TermPatch {zh?, enabled?, tier?} or {locked: false}
  deleteTerm: (id) => del(`/api/terms/${enc(id)}`), // 204
  // v2 G3 glossary janitor (types_v2.go)
  reviewGlossary: (courseId) => post(`/api/courses/${enc(courseId)}/glossary/review`), // GlossaryReviewResult (202 queued | 200 report)
  glossaryReport: (courseId, version, opts) => get(`/api/courses/${enc(courseId)}/glossary/report${version ? '?version=' + enc(version) : ''}`, opts), // JanitorReport | 404
  glossaryLastRun: (courseId, opts) => get(`/api/courses/${enc(courseId)}/glossary/report?run=last`, opts), // JanitorReport of the latest run (also one without changes) | 404
  undoJanitorItem: (courseId, version, termId) => post(`/api/courses/${enc(courseId)}/glossary/report/undo`, { version, term_id: termId }), // JanitorReport | 404 | 409

  // materials
  setMaterialLecture: (id, lectureId) => patch(`/api/materials/${enc(id)}`, { lecture_id: lectureId }), // Material
  setMaterialBind: (id, bind) => patch(`/api/materials/${enc(id)}`, { bind }), // Material; bind: MaterialBind {scope: week|lecture|course, week, lecture_id}
  materialStageAction: (id, stage, action) => post(`/api/materials/${enc(id)}/stages/${enc(stage)}/${enc(action)}`), // Material
  deleteMaterial: (id) => del(`/api/materials/${enc(id)}`), // 204

  // lectures
  lectures: (courseId) => get(`/api/courses/${enc(courseId)}/lectures`), // []Lecture
  lecture: (id, opts) => get(`/api/lectures/${enc(id)}${opts && opts.usageTotal ? '?usage_total=1' : ''}`, opts), // Lecture; opts {quiet, signal, usageTotal (also its usage_total, T25 S1: it scans the jobs, so asked for only now and then)}
  patchLecture: (id, p, opts) => patch(`/api/lectures/${enc(id)}`, p, opts), // Lecture | 409 RenumberConflict; p: LecturePatch {title?, title_zh?, recorded_at?, confirm?}
  renameLecture: (id, title) => patch(`/api/lectures/${enc(id)}`, { title }), // Lecture
  deleteLecture: (id, force = false, opts) => del(`/api/lectures/${enc(id)}${force ? '?force=1' : ''}`, opts), // 204 | 409; opts {quiet}
  lectureStageAction: (id, stage, action, choice, opts) => post(`/api/lectures/${enc(id)}/stages/${enc(stage)}/${enc(action)}`, choiceBody(choice), opts), // Lecture | 409
  // v3.2.x P1-b: a retry of summary / repair / translate with a model named
  // for this retry only: manual {provider_id, model, thinking?, max_tokens?}
  // (C10 contract; 400 model_choice.* / settings.*). opts: {quiet}
  lectureRetryManual: (id, stage, manual, opts) => post(`/api/lectures/${enc(id)}/stages/${enc(stage)}/retry`, { model_choice: 'manual', manual }, opts), // Lecture | 400
  // "修复断句" (textrepair-int): the asr rerun with text_only
  lectureTextRepair: (id, choice) => post(`/api/lectures/${enc(id)}/stages/asr/rerun`, { text_only: true, ...choiceBody(choice) }), // Lecture | 409
  launchLecture: (id) => post(`/api/lectures/${enc(id)}/launch`), // Lecture
  updateLecture: (id, choice) => post(`/api/lectures/${enc(id)}/update`, choiceBody(choice)), // Lecture | 409
  launchLectures: (ids) => post('/api/lectures/launch', { ids }), // BatchResponse
  cancelLectures: (ids) => post('/api/lectures/cancel', { ids }), // BatchResponse
  updateLectures: (ids, choice) => post('/api/lectures/update', choice ? { ids, model_choice: choice } : { ids }), // BatchResponse | 409
  deleteLectures: (ids, force) => post('/api/lectures/delete', { ids, force: !!force }), // BatchResponse
  sentences: (id, offset, limit) => get(`/api/lectures/${enc(id)}/sentences?offset=${offset}&limit=${limit}`), // SentencePage
  sentenceEdit: (id, idx, text) => post(`/api/lectures/${enc(id)}/sentences/${idx}/edit`, { text }), // Sentence
  sentenceReset: (id, idx) => post(`/api/lectures/${enc(id)}/sentences/${idx}/reset`), // Sentence
  sentenceRetranslate: (id, idx) => post(`/api/lectures/${enc(id)}/sentences/${idx}/retranslate`), // Sentence
  // v3.2.x P1-b: a pending sentence to the current translate model; 409 when that setting is unusable
  sentenceRetranslateCurrent: (id, idx, opts) => post(`/api/lectures/${enc(id)}/sentences/${idx}/retranslate`, { use_current: true }, opts), // RetranslateResult
  patches: (id, review) => get(`/api/lectures/${enc(id)}/patches${review ? '?review=' + enc(review) : ''}`), // []Patch
  review: (patchId, action, text) => post(`/api/patches/${enc(patchId)}/review`, text == null ? { action } : { action, text }), // Patch
  audioURL: (id, startMs, endMs) => withMock(`/api/lectures/${enc(id)}/audio?start_ms=${startMs}&end_ms=${endMs}`),
  exportURL: (url) => withMock(url),

  // uploads (multipart, XHR for progress; a local copy by path in the desktop app, SPEC-v3 V1.4)
  uploadLecture: (courseId, title, file, onProgress) => upload(`/api/courses/${enc(courseId)}/lectures`, { title }, file, onProgress), // Lecture (draft)
  uploadSyllabus: (courseId, file, onProgress) => upload(`/api/courses/${enc(courseId)}/syllabus`, {}, file, onProgress), // Course (legacy: raw text used as SYLLABUS.md)
  uploadSyllabusRaw: (courseId, file, onProgress, opts) => upload(`/api/courses/${enc(courseId)}/syllabus/raw`, {}, file, onProgress, 'POST', opts), // SyllabusDoc (A8)
  replaceMedia: (lectureId, file, onProgress) => upload(`/api/lectures/${enc(lectureId)}/media`, {}, file, onProgress, 'PUT'), // Lecture (drafts only)
  uploadMaterial: (courseId, file, lectureId, onProgress) =>
    upload(`/api/courses/${enc(courseId)}/materials`, { lecture_id: lectureId }, file, onProgress), // Course

  // v2 player (SPEC-v2 P6.4, P7, P8.2; types_v2.go). opts: {quiet, signal}
  cues: (id, opts) => get(`/api/lectures/${enc(id)}/cues`, opts), // []Cue
  chapters: (id, opts) => get(`/api/lectures/${enc(id)}/chapters`, opts), // ChaptersView | 404 (never generated)
  generateChapters: (id) => post(`/api/lectures/${enc(id)}/chapters`), // ChaptersView (202, running)
  resume: (id, opts) => get(`/api/lectures/${enc(id)}/resume`, opts), // ResumeView
  // Saved every few seconds and when the page goes away: always quiet;
  // opts.keepalive lets the request outlive the page (pagehide).
  saveResume: (id, positionMs, opts = {}) => {
    const path = `/api/lectures/${enc(id)}/resume`;
    const body = { position_ms: Math.max(0, Math.round(positionMs)) };
    if (!opts.keepalive) return put(path, body, { quiet: true });
    return fetch(withMock(path), { method: 'PUT', keepalive: true, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }).then(
      () => null,
      () => null,
    );
  }, // ResumeView
  // stream.mp4, or audio.wav with audio=true (the player's audio mode); both support Range requests
  mediaURL: (id, audio = false) => withMock(`/api/lectures/${enc(id)}/media${audio ? '?audio=1' : ''}`),

  // v2 P12, P13 (types_v2.go). opts: {quiet, signal}
  filenameRule: (courseId, opts) => get(`/api/courses/${enc(courseId)}/filename-rule`, opts), // FilenameRuleView
  generateFilenameRule: (courseId, opts) => post(`/api/courses/${enc(courseId)}/filename-rule/generate`, undefined, opts), // 202 FilenameRuleView | 409 filename_rule.not_configured / no_samples
  // kindMap: the rule's kind_map (code → lecture kind id; types_kinds.go)
  previewFilenameRule: (courseId, regex, kindMap, opts) => post(`/api/courses/${enc(courseId)}/filename-rule/preview`, { regex, kind_map: kindMap || {} }, opts), // FilenamePreviewResult
  applyFilenameRule: (courseId, regex, kindMap, confirm, opts) =>
    put(`/api/courses/${enc(courseId)}/filename-rule`, confirm ? { regex, kind_map: kindMap || {}, confirm: true } : { regex, kind_map: kindMap || {} }, opts), // Course | 409 RenumberConflict
  dismissFilenameProposal: (courseId, opts) => del(`/api/courses/${enc(courseId)}/filename-rule/proposal`, opts), // Course
  dismissRenumberNotices: (courseId, opts) => del(`/api/courses/${enc(courseId)}/renumber-notices`, opts), // Course (lecture kinds: Course.renumber_notices)
  undoFilenameRule: (courseId, confirm, opts) => post(`/api/courses/${enc(courseId)}/filename-rule/undo`, confirm ? { confirm: true } : undefined, opts), // Course
  // lecture kinds (lecture kinds; types_kinds.go): the global catalog and a lecture's kind
  lectureKinds: (opts) => get('/api/lecture-kinds', opts), // []LectureKind
  createKind: (input, opts) => post('/api/lecture-kinds', input, opts), // LectureKind; input {name, code, group}
  patchKind: (id, input, opts) => patch(`/api/lecture-kinds/${enc(id)}`, input, opts), // LectureKind | 409 kind.in_use
  deleteKind: (id, opts) => del(`/api/lecture-kinds/${enc(id)}`, opts), // 204 | 409 kind.in_use
  setLectureKind: (id, kind, confirm, opts) => put(`/api/lectures/${enc(id)}/kind`, confirm ? { kind, confirm: true } : { kind }, opts), // Lecture | 409 RenumberConflict; kind "" = automatic
  // course-ux (internal/api/types_course_ux.go): batch rerun of one stage; week overrides
  // opts: {choice (model_choice), dryRun, quiet}
  rerunCourse: (courseId, stage, ids, opts = {}) =>
    post(`/api/courses/${enc(courseId)}/rerun`, { stage, lecture_ids: ids, ...(opts.choice ? { model_choice: opts.choice } : {}), ...(opts.dryRun ? { dry_run: true } : {}) }, opts), // CourseRerunResult | 409 needs_model_choice
  setLectureWeek: (id, week, kind, confirm, opts) => put(`/api/lectures/${enc(id)}/week`, confirm ? { week, kind, confirm: true } : { week, kind }, opts), // Lecture | 409 RenumberConflict; kind supplement | makeup
  clearLectureWeek: (id, confirm, opts) => del(`/api/lectures/${enc(id)}/week${confirm ? '?confirm=1' : ''}`, opts), // Lecture | 409 RenumberConflict
  regenerateTitle: (id, force, opts) => post(`/api/lectures/${enc(id)}/title`, force ? { force: true } : undefined, opts), // 202 | 409 title.user_set
  holidays: (opts) => get('/api/settings/holidays', opts), // HolidaysView
  saveHolidays: (years, opts) => put('/api/settings/holidays', { years }, opts), // HolidaysView; years null = the built-in data
  // v2 P14.7 UI language and P14.6 Chinese content display (language.js, zh_display.js)
  // SPEC-v2 I6-I10: transcription devices (internal/api/types_intel.go)
  asrDevices: (opts) => get('/api/asr/devices', opts), // []ASRDeviceView, priority order
  detectDevices: (opts) => post('/api/asr/devices/detect', undefined, opts), // []ASRDeviceView
  patchDevice: (id, p, opts) => patch(`/api/asr/devices/${enc(id)}`, p, opts), // ASRDeviceView; p: {enabled?, model?, precision?, max_parallel?}
  orderDevices: (ids, opts) => put('/api/asr/devices/order', { ids }, opts), // []ASRDeviceView; every id once, highest priority first
  pauseDevice: (id, paused, cancelRunning = false) => post(`/api/asr/devices/${enc(id)}/pause`, { paused, cancel_running: cancelRunning }), // ASRDeviceView
  startSelfTest: (id) => post(`/api/asr/devices/${enc(id)}/selftest`), // 202 SelfTestView | 409 selftest.running / selftest.no_audio
  selfTest: (id, opts) => get(`/api/asr/devices/${enc(id)}/selftest`, opts), // SelfTestView
  cancelSelfTest: (id) => del(`/api/asr/devices/${enc(id)}/selftest`), // SelfTestView (idempotent)
  applySelfTest: (id, model, precision) => post(`/api/asr/devices/${enc(id)}/selftest/apply`, { model, precision }), // ASRDeviceView | 400 devices.apply_not_tested
  dismissDevicePrompt: (id, opts) => post(`/api/asr/devices/${enc(id)}/prompt/dismiss`, undefined, opts), // 204
  uiSettings: (opts) => get('/api/settings/ui', opts), // UISettings {locale, zh_display};  = not chosen
  saveUISettings: (ui, opts) => put('/api/settings/ui', ui, opts), // UISettings; both values are written

  // SPEC-v3 V3, V11: the component manager (.claude/v3-brief.md §7.2;
  // internal/api/handlers_components.go). opts: {quiet}
  components: (opts) => get('/api/components', opts), // {platform, manifest, components: [ComponentView], tasks: [TaskStatus]}
  planComponents: (ids, opts) => post('/api/components/plan', { ids }, opts), // {ids, download_bytes, install_bytes, free_bytes, enough_space, need_bytes}
  installComponents: (ids, opts) => post('/api/components/install', { ids }, opts), // 202 {task} | 409 components.busy / up_to_date
  updateComponent: (id, opts) => post(`/api/components/${enc(id)}/update`, undefined, opts), // 202 {task} | 409 components.up_to_date / busy
  uninstallComponent: (id, opts) => del(`/api/components/${enc(id)}`, opts), // 204 | 409 components.in_use / busy / bundled
  componentTasks: (opts) => get('/api/components/tasks', opts), // [TaskStatus]
  componentTaskAction: (id, action, opts) => post(`/api/components/tasks/${enc(id)}/${enc(action)}`, undefined, opts), // 202 {task}; action pause|resume|cancel|retry
  componentSources: (opts) => get('/api/components/sources', opts), // Sources {pypi, models, options, suggest_mirror}
  saveComponentSources: (s, opts) => put('/api/components/sources', { pypi: s.pypi, models: s.models }, opts), // Sources
  // "从文件导入组件": a local path (desktop: window.lectureflow.components.importFile()) …
  importComponentPath: (path, opts) => post('/api/components/import', { path }, opts), // 202 {task}
  // … or a .zip uploaded with progress (web mode; a NativeFile in the desktop app is sent as {path}). {promise, abort}
  importComponentFile: (file, onProgress, opts) => upload('/api/components/import', {}, file, onProgress, 'POST', opts), // 202 {task}

  // SPEC-v3 V8 application update, relayed by the desktop main process
  // (internal/api/handlers_update.go; desktop mode only, 404 in web mode)
  appInfo: (opts) => get('/api/app', opts), // AppInfo {version, mode, platform, ...}
  appActivity: (opts) => get('/api/app/activity', opts), // {running, queued, transcribing, api_running}
  appUpdate: (opts) => get('/api/app/update', opts), // UpdateStatus {state, version, percent, error, download_requested, install_on_quit}
  downloadUpdate: (opts) => post('/api/app/update/download', undefined, opts), // 202 UpdateStatus | 409 update.not_available
  installUpdateOnQuit: (enabled, opts) => post('/api/app/update/install-on-quit', { enabled: !!enabled }, opts), // UpdateStatus

  // r6 path-grants (internal/api/handlers_datadir.go; desktop app). The
  // folder the user picked (window.lectureflow.pickFolder) will not be used:
  // they declined the confirmation, the check refused it, or the service
  // refused the move or the import (every page then asks again). The service
  // forgets the pick now instead of when it expires (10 minutes). Callers do
  // not wait for it: it never rejects and never toasts, a pick that could
  // not be given up just expires.
  forgetFolderPick: (path) => request('DELETE', '/api/app/folder-pick', { path }, { quiet: true }).then(noAnswer, noAnswer), // 204
};

function noAnswer() {
  return null;
}

// ---- SSE "components" (v3-brief §7.2): {tasks: [TaskStatus]}, at most once a second ----
//
// Any module can follow the component tasks without app.js knowing about
// it; componentsEventFresh() tells a poller whether the event stream is
// delivering them (polling stays the fallback: an old server, a dropped
// stream, a proxy that buffers).

const componentsListeners = new Set();
let componentsEventAt = 0;

/** Calls fn({tasks}) for every SSE "components" event; returns the unsubscribe function. */
export function onComponentsEvent(fn) {
  componentsListeners.add(fn);
  return () => componentsListeners.delete(fn);
}

/** True when a "components" event arrived within the last withinMs milliseconds. */
export function componentsEventFresh(withinMs = 3000) {
  return componentsEventAt > 0 && Date.now() - componentsEventAt <= withinMs;
}

function dispatchComponents(ev) {
  componentsEventAt = Date.now();
  for (const fn of [...componentsListeners]) {
    try {
      fn(ev);
    } catch (e) {
      console.error(e);
    }
  }
}

// ---- desktop app: native files (SPEC-v3 V1.4) ----
//
// In the desktop app (window.lectureflow, the preload whitelist of
// .claude/v3-brief.md section 3) files the user drops or picks in the system
// dialog are not uploaded: the upload URL gets JSON {"path", ...fields} and
// the server copies the local file into the data directory itself. A file
// is "local" when its path is known: a NativeFile from pickLocalFiles, or a
// dropped File whose path attachDroppedPaths found. Everything else (web
// mode, an item without a path on disk) is uploaded over HTTP as before.

const native = typeof window !== 'undefined' && window.lectureflow && typeof window.lectureflow.pickFiles === 'function' ? window.lectureflow : null;
/** True in the desktop app. */
export const DESKTOP = !!native;

const droppedPaths = new WeakMap(); // File (dropped) -> local path

// A file picked in the system dialog: only its path is known (its size is
// null until the server has copied it). name and type are what the
// file-type checks (util.isMediaFile, isPDF, isSyllabusFile) read.
class NativeFile {
  constructor(path) {
    this.path = path;
    this.name = path.split(/[\\/]/).pop() || path;
    this.type = '';
    this.size = null;
  }
}

/** The local path of a file, '' when it has to be uploaded over HTTP. */
export function localPath(file) {
  if (!native || !file) return '';
  if (file instanceof NativeFile) return file.path;
  return droppedPaths.get(file) || '';
}

/** True when file is copied by the server from its local path (the UI says "copying"). */
export const isLocalFile = (file) => localPath(file) !== '';

// attachDroppedPaths(files) records the local path of each dropped File
// (desktop app only); a File without one (e.g. dragged from a web page)
// keeps the HTTP upload. Returns files.
export function attachDroppedPaths(files) {
  if (!native || typeof native.pathsForDroppedFiles !== 'function') return files;
  for (const f of files) {
    try {
      // one at a time: the preload leaves out files without a path
      const [p] = native.pathsForDroppedFiles([f]);
      if (typeof p === 'string' && p) droppedPaths.set(f, p);
    } catch {
      // no path: uploaded over HTTP
    }
  }
  return files;
}

// Same list as util.js MEDIA_EXTS (the system dialog filters by extension).
const MEDIA_EXT_LIST = ['mp4', 'm4v', 'mkv', 'mov', 'webm', 'avi', 'wmv', 'flv', 'ts', 'mts', 'm2ts', 'mpg', 'mpeg', '3gp', 'ogv', 'mp3', 'm4a', 'aac', 'wav', 'flac', 'ogg', 'oga', 'opus', 'mka', 'wma', 'amr', 'aiff', 'aif'];
const MIME_EXTS = { 'audio/*': MEDIA_EXT_LIST, 'video/*': MEDIA_EXT_LIST, 'application/pdf': ['pdf'], 'text/plain': ['txt'], 'text/markdown': ['md'] };

// The system dialog's filters for an <input accept> value: the accepted
// extensions first, then "all files" (as a browser's file dialog offers).
function acceptFilters(accept) {
  const exts = new Set();
  for (const raw of String(accept || '').split(',')) {
    const a = raw.trim().toLowerCase();
    if (/^\.[a-z0-9]+$/.test(a)) exts.add(a.slice(1));
    else for (const e of MIME_EXTS[a] || []) exts.add(e);
  }
  const all = { name: t('common.native.filter_all'), extensions: ['*'] };
  if (!exts.size) return [all];
  return [{ name: t('common.native.filter_supported'), extensions: [...exts].slice(0, 30) }, all];
}

// pickLocalFiles({accept, multiple}) opens the system dialog. Resolves to
// the picked files ([] when the user cancels), or null when the dialog is
// not available (the caller falls back to <input type=file>).
export async function pickLocalFiles({ accept, multiple } = {}) {
  if (!native) return null;
  let paths;
  try {
    paths = await native.pickFiles({ multiple: !!multiple, filters: acceptFilters(accept) });
  } catch (e) {
    console.warn('pickFiles failed, using the browser file dialog', e);
    return null;
  }
  return (Array.isArray(paths) ? paths : []).filter((p) => typeof p === 'string' && p).map((p) => new NativeFile(p));
}

// copyUpload sends a local file's path instead of its bytes (same
// {promise, abort} and errors as upload). Local copying reports no byte
// progress: onProgress(null, 0, size) once (an indeterminate bar).
function copyUpload(path, fields, file, onProgress, method, opts) {
  const fail = (err) => (opts.quiet ? err : report(err));
  const ctrl = new AbortController();
  const body = { path: localPath(file) };
  for (const [k, v] of Object.entries(fields)) if (v != null) body[k] = String(v);
  const cancelled = () => new ApiError(-1, t('common.native.copy_cancelled_plain'));
  const promise = (async () => {
    if (onProgress) onProgress(null, 0, file.size || 0);
    let res;
    try {
      res = await fetch(withMock(path), { method, cache: 'no-store', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal: ctrl.signal });
    } catch {
      if (ctrl.signal.aborted) throw cancelled();
      throw fail(new ApiError(0, t('common.net.upload_interrupted')));
    }
    const text = await res.text().catch(() => '');
    if (ctrl.signal.aborted) throw cancelled();
    if (!res.ok) {
      const b = parseBody(text);
      throw fail(new ApiError(res.status, errorText(res.status, b), b));
    }
    const v = text ? parseBody(text) : null;
    if (text && v === null) throw fail(new ApiError(res.status, t('common.net.bad_response')));
    return v;
  })();
  return { promise, abort: () => ctrl.abort() };
}

// Upload with progress: a local copy in the desktop app (copyUpload), else
// multipart over XHR. Text fields are sent before the file part so the
// server can read them first while streaming the file to disk (§17.4).
// Returns {promise, abort}; onProgress(fraction|null, loaded, total).
// opts.quiet: do not toast failures (the caller shows them inline).
function upload(path, fields, file, onProgress, method = 'POST', opts = {}) {
  if (isLocalFile(file)) return copyUpload(path, fields, file, onProgress, method, opts);
  const fail = (err) => (opts.quiet ? err : report(err));
  const xhr = new XMLHttpRequest();
  const promise = new Promise((resolve, reject) => {
    const fd = new FormData();
    for (const [k, v] of Object.entries(fields)) if (v != null) fd.append(k, v);
    fd.append('file', file, file.name);
    xhr.open(method, withMock(path));
    xhr.upload.onprogress = (e) => {
      if (onProgress) onProgress(e.lengthComputable ? e.loaded / e.total : null, e.loaded, e.lengthComputable ? e.total : file.size);
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        const v = xhr.responseText ? parseBody(xhr.responseText) : null;
        if (xhr.responseText && v === null) reject(fail(new ApiError(xhr.status, t('common.net.bad_response'))));
        else resolve(v);
      } else {
        const b = parseBody(xhr.responseText);
        reject(fail(new ApiError(xhr.status, errorText(xhr.status, b), b)));
      }
    };
    xhr.onerror = () => reject(fail(new ApiError(0, t('common.net.upload_interrupted'))));
    xhr.onabort = () => reject(new ApiError(-1, t('common.net.upload_cancelled')));
    xhr.send(fd);
  });
  return { promise, abort: () => xhr.abort() };
}

// perf-fix: the back end samples the GPU (nvidia-smi every 2 s) only while
// a page is shown (SPEC-v1.1 A10 "没有前端连接时停止采样", internal/api/
// visibility.go). The stream says whether this page is shown when it
// connects (?visible=) and every change is posted; a hidden page keeps its
// stream, since the completion notices arrive through it. Shown = the
// document is visible (Page Visibility) and, in the desktop app, the window
// is not hidden to the tray or minimized: the shell dispatches
// "lectureflow:window-visibility" {detail: {visible}} on hide and show.
const clientId = newClientId();
let visSeq = 0; // orders this page's reports (query and posts)
let shellVisible = true;
const visibilityHooks = new Set(); // the open streams' report functions
// Registered when the module loads: the shell also reports on page load.
window.addEventListener('lectureflow:window-visibility', (e) => {
  shellVisible = !(e && e.detail && e.detail.visible === false);
  for (const fn of visibilityHooks) fn();
});

function newClientId() {
  try {
    if (crypto.randomUUID) return crypto.randomUUID();
  } catch {
    // not a secure context: fall through
  }
  return Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 12);
}

function pageVisible() {
  return document.visibilityState !== 'hidden' && shellVisible;
}

function postVisibility(visible, seq) {
  // keepalive: the post still goes out while the page is being hidden
  fetch(withMock('/api/events/visibility'), {
    method: 'POST',
    cache: 'no-store',
    keepalive: true,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ client: clientId, visible, seq }),
  }).catch(() => {}); // unreachable: the stream drops too and the reconnect sends the state
}

// SSE with manual reconnect: 3 seconds after a drop (SPEC §8.2).
// handlers: onJob(JobEvent), onChanged(ChangedEvent), onResources(Resources),
// onComponents({tasks}) (SPEC-v3; also delivered to onComponentsEvent
// subscribers), onUp(reconnected: bool), onDown()
export function connectEvents(handlers) {
  let es = null;
  let timer = null;
  let down = false; // true between an error and the next successful open
  let stopped = false;
  let opened = false; // the current stream is registered on the server
  let reported = true; // the visibility the server has for the current stream
  // A change before the stream is open is sent by the open handler.
  const report = () => {
    const v = pageVisible();
    if (stopped || !opened || v === reported) return;
    reported = v;
    postVisibility(v, ++visSeq);
  };
  document.addEventListener('visibilitychange', report);
  visibilityHooks.add(report);
  const listen = (name, fn) => {
    es.addEventListener(name, (e) => {
      forgetShared(); // something changed: the next read asks the server again
      let ev;
      try {
        ev = JSON.parse(e.data);
      } catch {
        return;
      }
      if (fn) fn(ev);
    });
  };
  const open = () => {
    if (stopped) return;
    opened = false;
    reported = pageVisible();
    es = new EventSource(withMock(`/api/events?client=${encodeURIComponent(clientId)}&visible=${reported ? 1 : 0}&seq=${++visSeq}`));
    es.addEventListener('open', () => {
      opened = true;
      report(); // shown or hidden while connecting
      const reconnected = down;
      down = false;
      handlers.onUp && handlers.onUp(reconnected);
    });
    listen('job', handlers.onJob);
    listen('changed', handlers.onChanged);
    listen('resources', handlers.onResources);
    listen('components', (ev) => {
      if (handlers.onComponents) handlers.onComponents(ev);
      dispatchComponents(ev);
    });
    es.onerror = () => {
      componentsEventAt = 0; // pollers take over until the stream is back
      opened = false;
      es.close();
      if (!down) {
        down = true;
        handlers.onDown && handlers.onDown();
      }
      clearTimeout(timer);
      timer = setTimeout(open, 3000);
    };
  };
  open();
  return () => {
    stopped = true;
    clearTimeout(timer);
    document.removeEventListener('visibilitychange', report);
    visibilityHooks.delete(report);
    if (es) es.close();
  };
}
