// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The main process's report of the update mechanism (AUD1 I-1, SEC44
// information-1): POST /api/app/update/state carries the grant only the
// main process can compute (lib/pathgrant.js), as POST /api/app/path-grants
// does; the service refuses the report without it (internal/api
// handlers_update.go), so a script in the page cannot fake one.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');

const TOKEN = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';

function client(seen) {
  const { createApiClient } = require('../../src/main/apiclient');
  return createApiClient({
    getServer: () => ({ origin: 'http://127.0.0.1:4000', token: TOKEN }),
    fetchImpl: async (url, init) => {
      seen.push({ url, init });
      return { ok: true, status: 200, text: async () => '{"state":"none"}' };
    },
  });
}

test('apiclient.postUpdateState: POST /api/app/update/state with the token and the grant header', async () => {
  const { pathGrantSecret } = require('../../src/main/lib/pathgrant');
  const seen = [];
  const api = client(seen);
  assert.deepEqual(await api.postUpdateState({ state: 'ready', version: '9.9.9' }), { state: 'none' });
  assert.equal(seen.length, 1);
  assert.equal(seen[0].url, 'http://127.0.0.1:4000/api/app/update/state');
  assert.equal(seen[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(seen[0].init.body), { state: 'ready', version: '9.9.9' });
  assert.equal(seen[0].init.headers['X-LF-Token'], TOKEN);
  assert.equal(seen[0].init.headers['X-LF-Path-Grant'], pathGrantSecret(TOKEN));
});

test('apiclient: the update relay\'s other calls carry no grant header', async () => {
  const seen = [];
  const api = client(seen);
  await api.getUpdate();
  await api.settings();
  assert.equal(seen.length, 2);
  for (const s of seen) assert.equal(s.init.headers['X-LF-Path-Grant'], undefined, s.url);
});
