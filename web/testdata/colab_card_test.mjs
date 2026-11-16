// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x: the Colab card's retry in place, the runtime held but not
// answering, the release under way and the errors of an earlier session,
// run in Node (web/colab_card_test.go starts it with the web directory as a
// file: URL). Set up as colab_obs_test.mjs: settings_colab.js imported as
// the page imports it, the browser globals its imports read stubbed; t()
// answers the keys (no catalog loaded).

import assert from 'node:assert/strict';

const store = { getItem: () => null, setItem() {}, removeItem() {} };
const el = () => ({ dataset: {}, style: {}, classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, append() {}, addEventListener() {} });
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: { documentElement: el(), body: el(), addEventListener() {}, querySelector: () => null, getElementById: () => null, createElement: el },
  addEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
});
globalThis.window = globalThis;

const C = await import(new URL('settings_colab.js', process.argv[2]).href);

const now = Date.parse('2026-11-11T10:00:00Z');
const job = (extra) => ({ lecture_id: 'a', lecture_title: 'L7', stage: 'transcribe', progress: 0.62, eta_sec: 70, ...extra });

// ---- reconnect: carried from the card (and the status) to the job, in words ----
{
  const card = { kind: 'colab', jobs: [job({ reconnect: { attempt: 2, of: 3, next_at: '2026-11-11T10:00:12Z' } }), job({ lecture_id: 'b' })] };
  const c = C.colabCard({ remote: { state: 'running' }, remotes: [card] });
  assert.deepEqual(c.jobs[0].reconnect, { attempt: 2, of: 3, nextAt: '2026-11-11T10:00:12Z' });
  assert.equal(c.jobs[1].reconnect, null, 'no reconnect: null');
  const rc = C.reconnectText(c.jobs[0].reconnect, now);
  assert.equal(rc.text, 'resources.remote.reconnecting');
  assert.equal(rc.next, 'resources.remote.reconnect_next', 'next_at ahead: the countdown');
  assert.equal(C.reconnectText({ attempt: 1, of: 3, nextAt: '2026-11-11T09:59:59Z' }, now).next, '', 'next_at passed: no countdown');
  assert.equal(C.reconnectText({ attempt: 1, of: 3, nextAt: null }, now).next, '', 'no next_at: no countdown');
  assert.equal(C.reconnectText(null, now), null);
  // without remotes[]: the status's own jobs
  const s = C.colabCard({ remote: { state: 'running', jobs: [{ step: 'upload_audio', overall: 0.02, reconnect: { attempt: 1, of: 3 } }] } });
  assert.equal(s.jobs[0].reconnect.attempt, 1);
  // a malformed reconnect is ignored
  assert.equal(C.colabCard({ remote: { state: 'running' }, remotes: [{ kind: 'colab', jobs: [job({ reconnect: { attempt: 0 } })] }] }).jobs[0].reconnect, null);
}

// ---- stopping: the old steps and progress are not shown ----
{
  const c = C.colabCard({ remote: { state: 'stopping', jobs: [{ step: 'remote_transcribe', overall: 0.5 }] }, remotes: [{ kind: 'colab', jobs: [job()] }] });
  assert.deepEqual(c.jobs, [], 'releasing: no lectures');
}

// ---- the runtime held but not answering ----
{
  const rt = (reachable, billing, held = true) => ({ held, reachable, billing, unreachable_since: '2026-11-11T09:59:30Z' });
  const paid = { state: 'running', session_spent_cu: 1.34, cu_per_hour: 1.54, runtime: rt('unreachable', true) };
  assert.equal(C.runtimeWarning(paid, []), 'resources.remote.still_billing');
  assert.equal(C.runtimeWarning({ ...paid, session_spent_cu: 0 }, []), 'resources.remote.still_billing_plain', 'spent not known: no brackets');
  assert.equal(C.runtimeWarning({ ...paid, runtime: rt('unreachable', false) }, []), 'resources.remote.still_holding', 'free: the window');
  assert.equal(C.runtimeWarning({ ...paid, runtime: rt('unknown', true) }, []), null, 'unknown: no warning');
  assert.equal(C.runtimeWarning({ ...paid, runtime: rt('ok', true) }, []), null);
  assert.equal(C.runtimeWarning({ ...paid, runtime: rt('unreachable', true, false) }, []), null, 'not held');
  assert.equal(C.runtimeWarning({ ...paid, runtime: undefined }, []), null, 'a back end without runtime');
  assert.equal(C.runtimeWarning(paid, [{ reconnect: { attempt: 1, of: 3 } }]), null, 'a lecture retrying says so itself');
  assert.equal(C.runtimeWarning({ ...paid, state: 'stopping' }, []), 'resources.remote.still_billing_releasing', 'releasing, paid');
  assert.equal(C.runtimeWarning({ ...paid, state: 'stopping', runtime: rt('unreachable', false) }, []), 'resources.remote.still_holding', 'releasing, free: still holding the window (R8 B-s1)');
  assert.equal(C.runtimeWarning({ ...paid, state: 'stopping', runtime: rt('ok', false) }, []), null, 'releasing, free, answering: nothing to say');
}

// ---- the last error: not one of before the session ----
{
  const err = { code: 'colab.connection_lost' };
  assert.equal(C.visibleError({ last_error: err, last_error_at: '2026-11-11T09:00:00Z', session_started_at: '2026-11-11T09:30:00Z' }), null, 'older than the session');
  assert.equal(C.visibleError({ last_error: err, last_error_at: '2026-11-11T09:40:00Z', session_started_at: '2026-11-11T09:30:00Z' }), err);
  assert.equal(C.visibleError({ last_error: err, last_error_at: '2026-11-11T09:00:00Z' }), err, 'no session: shown');
  assert.equal(C.visibleError({ last_error: err, session_started_at: '2026-11-11T09:30:00Z' }), err, 'no time: shown');
  assert.equal(C.visibleError({}), null);
  assert.equal(C.visibleError(null), null);
}

// ---- a runtime not released ----
{
  assert.equal(C.leftoverText({ code: 'colab.release_failed', accelerator: 'L4' }), 'resources.remote.leftover_retrying');
  assert.equal(C.leftoverText({ code: 'colab.release_failed', accelerator: '' }), 'resources.remote.leftover_retrying_any', 'lost with its notebook only');
  assert.equal(C.leftoverText({ code: 'colab.leftover_session', accelerator: '' }), 'resources.remote.leftover_session_any');
  assert.equal(C.leftoverText(null), '');
}

console.log('colab-card checks: ok');
