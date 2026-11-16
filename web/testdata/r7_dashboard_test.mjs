// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 7 (the user walk-throughs, .agent-log/reports T22-T26) on the
// workbench, the course card and the resource panel:
//   T24 B1, G1  a failure that a plain retry cannot get past offers "换模型
//               重试…", a damaged file says what to do, and the way out at
//               the end of an error text is not cut off;
// (the later sections name their own items).
// The modules run on the DOM stand-in of r7_dom.mjs; web/r7_dashboard_test.go
// starts it with the web directory as a file: URL.


import { assert, El, body, doc, dialogs, toastsEl, base, sent, net, settle, until, fk, told, cancelDialogs, confirmDialogs } from './r7_dom.mjs';

const D = await import(new URL('dom.js', base).href);
const U = await import(new URL('util.js', base).href);
const I = await import(new URL('i18n.js', base).href);
const A = await import(new URL('api.js', base).href);
await I.setLocale('zh-CN');
const t = I.t;
// the page's handler of failed requests (app.js): a toast
A.setHandlers({ onError: (e) => D.toast(e.message, 'error') });

const Dash = await import(new URL('view_dashboard.js', base).href);

// ---- the workbench ----

const stage = (name, status, o = {}) => ({ stage: name, status, display_status: status, progress: status === 'done' ? 1 : 0, attempt: 0, error: null, allowed_actions: [], started_at: null, finished_at: null, duration_ms: null, ...o });
const names = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
function lecture(id, o = {}) {
  return {
    id, course_id: 'C1', title: `Lecture ${id}`, label: 'L01', recorded_at: '', numbered: false, week: 0, kind: 'lecture', draft: false, created_at: '2026-10-27T10:00:00Z',
    duration_ms: 0, counts: { sentences: 0, patches_pending: 0 }, exports: [], materials: [], playable: false, lecture_actions: [],
    stages: names.map((n) => stage(n, n === 'translate' ? 'failed' : n === 'export' ? 'pending' : 'done')), ...o,
  };
}
const course = (id, o = {}) => ({ id, code: id.toUpperCase(), name: `Course ${id}`, lecture_count: 4, term_count: 0, draft_count: 0, stale_count: 0, running_count: 0, patches_pending: 0, last_activity_at: null, course_actions: [], ...o });
const fail = (id, o = {}) => ({ lecture_id: id, stage: 'translate', course_id: 'c1', course_code: 'C1', course_name: 'Course c1', lecture_title: `Lecture ${id}`, kind: 'lecture', week: 0, allowed_actions: ['retry'], error: 'x', finished_at: null, ...o });
const timeLimit = { code: 'llm.time_limit', params: { role: 'translate', seconds: 600, provider: 'Anthropic', model: 'claude-opus-5-5' } };
const refusal = { code: 'llm.refusal', params: { role: 'translate', provider: 'Anthropic', model: 'claude-opus-5-5' } };
const corrupt = { code: 'media.corrupt', params: {} };
const balance = { code: 'llm.insufficient_balance', params: { role: 'translate', provider: 'DeepSeek' } };

let dash = {};
const dashboard = () => ({ courses: [course('c1')], in_progress: [], draft_total: 0, drafts: [], review_total: 0, reviews: [], stale_total: 0, stale: [], failures: [], recent_done: [], usage_today: [], device_prompts: [], ...dash });
const settingsBody = { general: {}, providers: [{ id: 'p1', name: 'Anthropic', has_key: true }], roles: [{ role: 'translate', configured: true, provider_id: 'p1', model: 'claude-opus-5-5' }] };
const defaultAnswer = (m, u) => {
  if (u.startsWith('/api/dashboard')) return { status: 200, body: dashboard() };
  if (u.startsWith('/api/settings')) return { status: 200, body: settingsBody };
  const lec = /^\/api\/lectures\/([^/?]+)/.exec(u);
  if (lec) return { status: 200, body: lecture(decodeURIComponent(lec[1])) };
  return { status: 200, body: {} };
};
net.answer = defaultAnswer;
const page = new El('main');
body.append(page);
const dctx = { system: () => null, resources: () => null, courses: () => [course('c1')], newCourse() {}, subscribe: () => () => {} };
const view = Dash.mountDashboard(page, dctx);
await settle();
const show = async (d) => {
  dash = d;
  view.refresh();
  await settle();
};
const rowOf = (id) => {
  const a = fk(`dash:fail:${id}:translate`);
  return a ? a.closest('li') : null;
};

// ---- T24 B1, G1: the failures of the workbench ----

{
  const tl = U.errorInfoText(timeLimit, '');
  await show({ failures: [fail('T', { error_info: timeLimit }), fail('R', { error_info: refusal }), fail('X', { stage: 'prep', error_info: corrupt }), fail('B', { error_info: balance })] });
  // B1: a refusal or the total time limit: no plain retry (billed again, fails
  // again); "换模型重试…" in its place
  for (const id of ['T', 'R']) {
    assert.equal(fk(`dash:retry:${id}:translate`), null, `${id}: no plain retry for a failure another model is needed for`);
    const b = fk(`dash:retry_model:${id}:translate`);
    assert.ok(b && b.tagName === 'BUTTON', `${id}: "换模型重试…" instead`);
    assert.equal(b.textContent.trim(), t('common.action.retry_model'));
    assert.ok(!/btn-primary/.test(b.className), 'not in the primary colour');
  }
  assert.ok(fk('dash:retry:B:translate'), 'another failure keeps its 重试');
  assert.equal(fk('dash:retry_model:B:translate'), null, 'and has no 换模型重试');
  // G1: the way out at the end of a long text is there in full (not cut at 120 characters)
  assert.ok(tl.length > 120, 'the time-limit text is long');
  // (r9 WD-10, convention C2: the row says it as the lecture page's stage
  // card does, U.stageErrorText; the shared line's WH-07 gives this stage
  // its own sentence there)
  const tlSaid = U.stageErrorText({ stage: 'translate', status: 'failed', error_info: timeLimit, error: 'x' });
  assert.ok(rowOf('T').textContent.includes(tlSaid), 'the whole text of the time limit, with its way out');
  // G1: a damaged file has no retry, and says what to do instead (as the lecture page does)
  const x = fk('dash:fail:X:prep').closest('li');
  assert.equal(x.querySelectorAll('button').length, 0, 'no button for a damaged file');
  assert.ok(x.textContent.includes(t('common.pipe.corrupt_hint')), 'the way out of a damaged file');
  // "换模型重试…" reads the lecture (its stage's models) and opens the model dialog
  sent.length = 0;
  fk('dash:retry_model:T:translate').click();
  await until(() => dialogs.some((d) => d.open), 'the model dialog opens');
  assert.ok(sent.includes('GET /api/lectures/T'), 'the lecture is read for its stage models');
  const dlg = dialogs.find((d) => d.open);
  assert.ok(dlg.textContent.includes(t('common.model_choice.manual')), 'with the choice of another model');
  await cancelDialogs();
  assert.ok(!sent.some((s) => s.startsWith('POST')), 'cancelled: nothing retried');
  told();
}

// ---- T24 S1: many failures at once: one reason said once, "全部重试" ----

{
  const bal = U.errorInfoText(balance, '');
  const failures = [fail('B1', { error_info: balance }), fail('T', { error_info: timeLimit }), fail('B2', { error_info: balance }), fail('X', { stage: 'prep', error_info: corrupt }), fail('B3', { error_info: balance }), fail('B4', { error_info: balance })];
  await show({ failures });
  const group = fk('dash:fail:B1:translate').closest('ul');
  const times = group.textContent.split(bal).length - 1;
  assert.equal(times, 1, 'the reason four lectures share is said once');
  const reason = group.querySelector('li.fail-reason');
  assert.ok(reason && reason.textContent.includes(bal), 'as the head of its lectures');
  // the rows of one reason follow each other, in the order they came
  const order = group.querySelectorAll('a.fail-title').map((a) => a.dataset.fkey);
  assert.deepEqual(order, ['dash:fail:B1:translate', 'dash:fail:B2:translate', 'dash:fail:B3:translate', 'dash:fail:B4:translate', 'dash:fail:T:translate', 'dash:fail:X:prep']);
  // a reason of one lecture stays in its row
  assert.ok(rowOf('T').textContent.includes(U.stageErrorText({ stage: 'translate', status: 'failed', error_info: timeLimit, error: 'x' })));
  // "全部重试": the four plain failures, not the two a plain retry cannot help
  const all = fk('dash:retryall');
  assert.ok(all && all.tagName === 'BUTTON', 'the failures have 全部重试');
  assert.equal(all.textContent.trim(), t('dashboard.todo.retry_all'));
  sent.length = 0;
  const bodies = [];
  net.answer = (m, u, b) => {
    if (m === 'POST') {
      bodies.push(`${u} ${JSON.stringify(b || {})}`);
      if (u.includes('/B2/') && !(b && b.model_choice)) return { status: 409, body: { error: { code: 'needs_model_choice' }, diff: [{ stage: 'translate', lecture_title: 'Lecture B2', snapshot: { provider_id: 'p0', provider_name: 'Old', model: 'old' }, current: { provider_id: 'p1', provider_name: 'Anthropic', model: 'claude-opus-5-5' }, snapshot_usable: true }] } };
      if (u.includes('/B3/')) return { status: 409, body: { error: { code: 'state.conflict' } } };
      return { status: 200, body: lecture('B1') };
    }
    return defaultAnswer(m, u);
  };
  all.click();
  await until(() => dialogs.some((d) => d.open), 'a snapshot that differs asks for the model once');
  await confirmDialogs();
  await until(() => toastsEl.children.length > 0, 'the result of 全部重试');
  const posted = sent.filter((s) => s.startsWith('POST')).map((s) => s.replace(/\?.*$/, ''));
  assert.deepEqual(posted, ['POST /api/lectures/B1/stages/translate/retry', 'POST /api/lectures/B2/stages/translate/retry', 'POST /api/lectures/B3/stages/translate/retry', 'POST /api/lectures/B4/stages/translate/retry', 'POST /api/lectures/B2/stages/translate/retry'], 'the plain failures, one by one; the one asked about again with the choice');
  assert.ok(bodies[bodies.length - 1].includes('"model_choice"'), 'with the model chosen');
  const msgs = told();
  assert.equal(msgs.length, 1, `one result (${msgs.join(' | ')})`);
  const conflict = U.errorInfoText({ code: 'state.conflict' }, '');
  const why = U.clauseJoin([t('dashboard.retry_skip.model', { n: 1 }), t('dashboard.retry_skip.futile', { n: 1 }), conflict]);
  assert.equal(msgs[0], `warn: ${t('dashboard.retried_partial', { ok: 3, n: 3, reason: why })}`, 'how many were retried, and why the others were not');
  net.answer = defaultAnswer;
  // all of them retried: the plain result
  await show({ failures: [fail('B1', { error_info: balance }), fail('B2', { error_info: balance })] });
  net.answer = (m, u) => (m === 'POST' ? { status: 200, body: lecture('B1') } : defaultAnswer(m, u));
  fk('dash:retryall').click();
  await until(() => toastsEl.children.length > 0, 'the result of 全部重试');
  assert.deepEqual(told(), [`success: ${t('dashboard.retried', { n: 2 })}`]);
  net.answer = defaultAnswer;
  // a single plain failure has its own 重试 only
  await show({ failures: [fail('B1', { error_info: balance }), fail('T', { error_info: timeLimit })] });
  assert.equal(fk('dash:retryall'), null, 'one plain failure: no 全部重试');
  await show({ failures: [] });
}

// ---- T24 S2: failures of more than 24 hours ago, the course cards ----

{
  const CC = await import(new URL('course_card.js', base).href);
  // the course card: "N 失败" beside the other counts, read with the card's name
  const card = CC.courseCard(course('c1', { failed_count: 2, stale_count: 1 }));
  const chip = card.querySelectorAll('span.chip').find((c) => c.textContent === t('course.card.failed', { n: 2 }));
  assert.ok(chip, 'the card counts its failed lectures');
  assert.ok(chip.className.includes('chip-err'), 'in the error colour');
  assert.ok(!CC.courseCard(course('c2', { failed_count: 0 })).textContent.includes(t('course.card.failed', { n: 0 })), 'none failed: no chip');
  assert.ok(!/失败/.test(CC.courseCard(course('c3')).textContent), 'a server without the count: no chip');
  // the course list's cards (api.Course) carry it too
  assert.equal(CC.cardOf({ id: 'c4', name: 'n', lecture_count: 1, term_count: 0, stale_lecture_count: 0, failed_count: 3 }).failed_count, 3);
  assert.equal(CC.cardOf({ id: 'c5', name: 'n', lecture_count: 1, term_count: 0, stale_lecture_count: 0 }).failed_count, 0);
  // the workbench: "还有 N 节更早失败", with the courses they are in
  const courses = [course('c1', { failed_count: 2 }), course('c2', { failed_count: 2 }), course('c3', { failed_count: 0 })];
  await show({ courses, failures: [fail('A', { error_info: balance })], earlier_failed_total: 3 });
  const group = fk('dash:fail:A:translate').closest('div');
  const more = t('dashboard.todo.earlier_failed', { n: 3 });
  assert.ok(page.textContent.includes(more), 'the failures of before the last 24 hours are counted');
  const links = page.querySelectorAll('a.todo-chip').filter((a) => /^#\/course\//.test(a.getAttribute('href')) && group.contains(a));
  assert.deepEqual(links.map((a) => `${a.getAttribute('href')} ${a.textContent}`), ['#/course/c1 C11', '#/course/c2 C22'], 'with links to the courses that have them (the one recent failure of c1 left out)');
  // only earlier failures: the group is there all the same
  await show({ courses, failures: [], earlier_failed_total: 3 });
  assert.ok(page.textContent.includes(t('dashboard.todo.failures_earlier')), 'a group for the earlier failures alone');
  assert.ok(page.querySelectorAll('a.todo-chip').some((a) => a.getAttribute('href') === '#/course/c2'), 'with its links');
  assert.ok(!page.textContent.includes(t('dashboard.head.all_clear')), 'not "all clear"');
  // a server without the field: nothing more
  await show({ courses: [course('c1')], failures: [fail('A', { error_info: balance })] });
  assert.ok(!/更早/.test(page.textContent), 'no count without the field');
  await show({ failures: [] });
}

// ---- T24 S6: a Colab runtime still billed or not released ----

{
  const C = await import(new URL('settings_colab.js', base).href);
  const R = await import(new URL('resources.js', base).href);
  const remote = (o = {}) => ({ configured: true, state: 'running', accelerator: 'L4', tier: 'paid', window_used_sec: 3600, window_total_sec: 14400, lectures_left: 3, session_spent_cu: 1.31, cu_per_hour: 1.54, runtime: { held: true, reachable: 'unreachable', billing: true }, ...o });
  let res = { sampled_at: new Date().toISOString(), gpu: { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] }, providers: [], remote: remote() };
  dctx.resources = () => res;
  const slot = () => page.querySelector('div.dash-colab-slot');
  const manual = t('resources.remote.manual_end');
  // unreachable and billed: the workbench says so as a warning, with "立即关机"
  await show({ failures: [] });
  view.onResources(res);
  const billing = C.runtimeWarning(res.remote, []);
  assert.ok(billing && slot().textContent.includes(billing), 'the billing warning on the workbench, not only in the resource panel');
  assert.ok(slot().querySelector('section.asr-notice'), 'in the warning look');
  const now = fk('dash:colab:now');
  assert.ok(now && now.textContent.trim() === t('resources.remote.now'), 'with 立即关机');
  sent.length = 0;
  net.answer = (m, u) => (u.startsWith('/api/colab/shutdown') ? { status: 200, body: { status: remote({ state: 'stopping' }) } } : defaultAnswer(m, u));
  now.click();
  await until(() => dialogs.some((d) => d.open), 'the confirmation of 立即关机');
  await confirmDialogs();
  await until(() => sent.some((s) => s.startsWith('POST /api/colab/shutdown')), 'the shutdown request');
  await until(() => toastsEl.children.length > 0, 'its result');
  assert.deepEqual(told(), [`success: ${t('resources.remote.now_done')}`]);
  net.answer = defaultAnswer;
  // being released and still billed: no button (it is shutting down), the
  // way to end it by hand
  res = { ...res, remote: remote({ state: 'stopping' }) };
  view.onResources(res);
  assert.ok(slot().textContent.includes(C.runtimeWarning(res.remote, [])), 'the releasing warning');
  assert.ok(slot().textContent.includes(manual), 'and how to end the session by hand');
  assert.equal(fk('dash:colab:now'), null, 'no 立即关机 while it shuts down');
  // and an earlier runtime not released either: said to be another one
  res = { ...res, remote: remote({ state: 'stopping', leftover: { code: 'colab.release_failed', accelerator: '' } }) };
  view.onResources(res);
  assert.ok(slot().textContent.includes(t('resources.remote.leftover_earlier_any')), 'the earlier runtime is another one');
  assert.ok(!slot().textContent.includes(t('resources.remote.leftover_retrying_any')), 'not in the words of the session being released');
  // a runtime not released, without a session: the workbench says it too
  res = { ...res, remote: remote({ state: 'none', runtime: null, leftover: { code: 'colab.release_failed', accelerator: 'T4' } }) };
  view.onResources(res);
  assert.ok(!slot().hidden && slot().textContent.includes(C.leftoverText(res.remote.leftover)), 'the runtime not released');
  assert.ok(slot().textContent.includes(manual), 'with the way to end it by hand');
  // all well: the calm line of before
  res = { ...res, remote: remote({ runtime: { held: true, reachable: 'ok', billing: true } }) };
  view.onResources(res);
  assert.equal(slot().querySelector('section.asr-notice'), null, 'no warning');
  assert.ok(slot().querySelector('p.dash-colab'), 'the Colab line');
  // the resource panel: the session being released and an earlier runtime
  // not released are told apart, both with the way out by hand
  const insp = new El('aside');
  body.append(insp);
  const panel = R.mountInspector(insp, { onCollapse() {}, onExpand() {}, onToggleGPU() {}, onToggleDevice() {}, onRetry() {}, system: () => null });
  res = { ...res, remote: remote({ state: 'stopping', leftover: { code: 'colab.release_failed', accelerator: '' } }) };
  panel.setResources(res);
  const notes = insp.querySelectorAll('p.insp-note').map((p) => p.textContent);
  const releasing = notes.find((s) => s.includes(C.runtimeWarning(res.remote, [])));
  assert.ok(releasing && releasing.includes(manual), 'the session being released, with the way out by hand');
  const earlier = notes.find((s) => s.includes(t('resources.remote.leftover_earlier_any')));
  assert.ok(earlier, 'the earlier runtime is said to be another one');
  assert.ok(!notes.some((s) => s.includes(t('resources.remote.leftover_retrying_any'))), 'not in the words of the session being released');
  // a runtime not released alone: its usual words, with the way out by hand
  res = { ...res, remote: remote({ state: 'none', runtime: null, leftover: { code: 'colab.release_failed', accelerator: '' } }) };
  panel.setResources(res);
  const alone = insp.querySelectorAll('p.insp-note').map((p) => p.textContent).find((s) => s.includes(t('resources.remote.leftover_retrying_any')));
  assert.ok(alone && alone.includes(manual), 'alone: the usual words and the way out by hand');
  insp.remove();
  dctx.resources = () => null;
  view.onResources(null);
}

// ---- T24 S4 (resource panel), G13, G11 ----

{
  const R = await import(new URL('resources.js', base).href);
  const insp = new El('aside');
  body.append(insp);
  const panel = R.mountInspector(insp, { onCollapse() {}, onExpand() {}, onToggleGPU() {}, onToggleDevice() {}, onRetry() {}, system: () => null });
  const gpu = { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] };
  const device = { id: 'cuda:0', label: 'RTX 4060', kind: 'cuda', enabled: true, present: true, active: false, paused: false, tasks: [], slots_used: 0, slots_max: 1 };
  let at = 0;
  const snap = (o = {}) => ({ sampled_at: new Date(Date.UTC(2026, 10, 15, 12, 0, at++)).toISOString(), gpu, providers: [], system: { cpu_pct: 10, mem_used_mb: 1000, mem_total_mb: 16000 }, power: { known: true, source: 'battery', has_battery: true, battery_pct: 50 }, policy: { wait_ac: 1, wait_no_device: 1, battery_machine: true, none_reason: 'power_no_saver_device' }, devices: [device], remote: null, ...o });
  panel.setResources(snap());
  // S4: the two suspended notes have their way out, and a link to the
  // transcription devices of the settings
  const link = () => insp.querySelectorAll('a').find((a) => a.getAttribute('href') === '#/settings/asr');
  assert.ok(link(), 'a link to the settings of the transcription devices');
  assert.equal(link().textContent.trim(), t('resources.power.to_settings'));
  assert.ok(insp.textContent.includes(t('resources.power.wait_way_out')), 'what to do instead of waiting');
  const first = link();
  panel.setResources(snap());
  assert.ok(link() === first, 'the link is not replaced by the next snapshot (a click must not get lost)');
  // G13: every task suspended: the main switch does not say "运行中"
  const label = () => insp.querySelector('button.switch[data-fkey="insp:gpu"]').querySelector('span.switch-label').textContent;
  assert.equal(label(), t('resources.devices.suspended_label'), 'all suspended: the switch says so');
  panel.setResources(snap({ policy: { battery_machine: true } }));
  assert.equal(link(), undefined, 'nothing suspended: no link');
  assert.equal(label(), t('common.status.running'), 'nothing suspended: as before');
  // G11: the lectures that do not fit the Colab session: said plainly, and where they go
  const remote = { configured: true, state: 'running', accelerator: 'T4', tier: 'paid', window_used_sec: 14000, window_total_sec: 14400, lectures_left: 0, planned: 2, planned_fit: 0 };
  panel.setResources(snap({ policy: null, power: null, remote }));
  const text = insp.textContent;
  assert.ok(text.includes(t('resources.remote.plan_none', { n: 2 })), 'none fits: said so');
  assert.ok(!text.includes(t('resources.remote.plan', { n: 2, fit: 0 })), 'not "约 0 节能在本次处理"');
  assert.ok(text.includes(t('resources.remote.plan_rest')), 'and where the others go');
  panel.setResources(snap({ policy: null, power: null, remote: { ...remote, planned: 5, planned_fit: 3, window_used_sec: 3600, lectures_left: 3 } }));
  assert.ok(insp.textContent.includes(t('resources.remote.plan', { n: 5, fit: 3 })) && insp.textContent.includes(t('resources.remote.plan_rest')), 'some fit: as before, and where the others go');
  insp.remove();
}

// ---- T22 S5 (workbench): a role the lectures need has no usable model ----

{
  const role = (id, configured, o = {}) => ({ role: id, label: id, configured, provider_id: configured ? 'p1' : '', model: configured ? 'm' : '', ...o });
  const ready = [role('summary', true), role('repair', true), role('translate', true), role('organizer', true)];
  const roles = (list) => {
    settingsBody.roles = list;
    view.onChanged({ kind: 'settings' });
  };
  const notice = () => page.querySelectorAll('section.asr-notice').find((s) => s.querySelector('a[href="#/settings/roles"]'));
  roles([role('summary', true), role('repair', true), role('translate', false), role('organizer', true)]);
  await until(() => notice(), 'the notice of a role without a model');
  const quoted = t('settings.quote', { name: U.roleName('translate') });
  assert.ok(notice().textContent.includes(t('settings.alert.roles_broken_launch', { roles: quoted })), 'which role, and that new lectures cannot start');
  const go = notice().querySelector('a[href="#/settings/roles"]');
  assert.equal(go.textContent.trim(), t('settings.alert.fix'), 'with the way to the role settings');
  // fixed: gone
  roles(ready);
  await until(() => !notice(), 'the notice gone once fixed');
  // transcription only (no launch role set up) is a choice, not an error; an
  // optional role is never one
  roles([role('summary', false), role('repair', false), role('translate', false), role('organizer', true)]);
  await settle(60);
  assert.equal(notice(), undefined, 'transcription only: no notice');
  roles([...ready.slice(0, 3), role('chapters', false)]);
  await settle(60);
  assert.equal(notice(), undefined, 'an optional role unset: no notice');
  roles(ready);
  await settle(60);
}

// ---- R76 M-1: "全部重试" says that a translation failure may lose its translations ----

{
  // a translation retried with the current settings clears the lecture's
  // translations and translates it again: the model question says so, once
  // (round 8 WD-07, R82 D-L1: the shared dialog says it itself, so the
  // button no longer does)
  await show({ failures: [fail('B1', { error_info: balance }), fail('B2', { error_info: balance })] });
  assert.equal(fk('dash:retryall').getAttribute('title'), null, 'not on the button');
  net.answer = (m, u, b) => {
    if (m === 'POST') {
      if (u.includes('/B2/') && !(b && b.model_choice)) return { status: 409, body: { error: { code: 'needs_model_choice' }, diff: [{ stage: 'translate', lecture_title: 'Lecture B2', snapshot: { provider_id: 'p0', provider_name: 'Old', model: 'old' }, current: { provider_id: 'p1', provider_name: 'Anthropic', model: 'claude-opus-5-5' }, snapshot_usable: true }] } };
      return { status: 200, body: lecture('B1') };
    }
    return defaultAnswer(m, u);
  };
  fk('dash:retryall').click();
  await until(() => dialogs.some((d) => d.open), 'the model question of the translation asked about');
  const dlg = dialogs.find((d) => d.open);
  const warns = dlg.querySelectorAll('p.dlg-warn');
  assert.equal(warns.length, 1, 'one sentence about the translations');
  assert.equal(dlg.textContent.split(t('common.model_choice.translate_clears')).length - 1, 1, 'said once, in the dialog');
  await cancelDialogs();
  await until(() => toastsEl.children.length > 0, 'the result');
  told();
  net.answer = defaultAnswer;
  await show({ failures: [fail('S1', { stage: 'summary', error_info: balance }), fail('S2', { stage: 'summary', error_info: balance })] });
  assert.equal(fk('dash:retryall').getAttribute('title'), null, 'none of them a translation: nothing on the button either');
  await show({ failures: [] });
}

// ---- R76 M6a: some tasks suspended while another runs: the switch says "运行中" ----

{
  const R = await import(new URL('resources.js', base).href);
  const insp = new El('aside');
  body.append(insp);
  const panel = R.mountInspector(insp, { onCollapse() {}, onExpand() {}, onToggleGPU() {}, onToggleDevice() {}, onRetry() {}, system: () => null });
  const gpu = { available: false, paused: false, asr_slots_used: 0, asr_slots_max: 1, util_history: [], temp_history: [] };
  const npu = { id: 'npu:0', label: 'NPU', kind: 'npu', enabled: true, present: true, active: true, paused: false, slots_used: 1, slots_max: 1 };
  const snap = (tasks) => ({ sampled_at: new Date().toISOString(), gpu, providers: [], system: { cpu_pct: 10, mem_used_mb: 1000, mem_total_mb: 16000 }, power: { known: true, source: 'battery', has_battery: true, battery_pct: 50 }, policy: { wait_ac: 1, battery_machine: true, reason: 'battery' }, devices: [{ ...npu, tasks }], remote: null });
  const label = () => insp.querySelector('button.switch[data-fkey="insp:gpu"]').querySelector('span.switch-label').textContent;
  panel.setResources(snap([{ lecture_id: 'L1', stage: 'asr', selftest: false }]));
  assert.equal(label(), t('common.status.running'), 'one task waits, another runs: "running"');
  panel.setResources(snap([{ lecture_id: 'L1', stage: 'asr', selftest: true }]));
  assert.equal(label(), t('resources.devices.suspended_label'), 'a device test is no task: every task is suspended');
  insp.remove();
}

console.log('r7 dashboard checks: ok');
process.exit(0);
