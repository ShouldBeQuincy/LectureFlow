// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// The translate batch default of the settings page (provider_rules.js
// defaultBatch) against settings.DefaultBatch: web/batch_default_test.go
// passes the web directory as a file: URL and the cases as JSON, each with
// the provider view's default_batch_claude (settings.ClaudeBatch), a model
// name and the Go result.

import assert from 'node:assert/strict';

const base = process.argv[2];
const cases = JSON.parse(process.argv[3]);
const R = await import(new URL('provider_rules.js', base).href);

assert.ok(cases.length > 0);
for (const c of cases) {
  assert.equal(R.defaultBatch({ default_batch_claude: c.claude }, c.model), c.want, JSON.stringify(c));
}
// a provider view without the field (older data) or no provider: 1
assert.equal(R.defaultBatch({}, 'claude-sonnet-5-5'), 1);
assert.equal(R.defaultBatch(null, 'claude-sonnet-5-5'), 1);
assert.equal(R.defaultBatch(undefined, undefined), 1);

console.log('batch default checks: ok');
