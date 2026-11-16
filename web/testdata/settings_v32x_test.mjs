// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x settings page (web-builder): the rules of provider_rules.js the
// settings page uses — the limits a new provider's dialog starts with by
// type (Anthropic 4 / no per-minute limit, others 4 / 60), and which models
// cannot switch thinking off (claude-opus-5-5 and its dated / -latest ids,
// not claude-sonnet-5-5). web/settings_v32x_test.go starts it with the web
// directory as a file: URL.

import assert from 'node:assert/strict';

const base = process.argv[2];
const R = await import(new URL('provider_rules.js', base).href);

assert.deepEqual({ ...R.limitsPrefill('anthropic') }, { max_concurrency: 4, rpm: 0 });
assert.deepEqual({ ...R.limitsPrefill('openai') }, { max_concurrency: 4, rpm: 60 });
assert.deepEqual({ ...R.limitsPrefill('') }, { max_concurrency: 4, rpm: 60 });
assert.ok(Object.isFrozen(R.limitsPrefill('anthropic')));

for (const m of ['claude-opus-5-5', 'claude-opus-5-5-20261101', 'claude-opus-5-5-latest', ' Claude-Opus-5-5 ', 'claude-opus-5-5@20261101']) {
  assert.equal(R.thinkingFloorsAtLow(m), true, m);
}
for (const m of ['claude-sonnet-5-5', 'claude-sonnet-5-5-20261101', 'claude-opus-5', 'claude-opus-5-55', 'claude-opus-4-5', 'x-claude-opus-5-5', '', null, undefined]) {
  assert.equal(R.thinkingFloorsAtLow(m), false, String(m));
}
// W4c (R35 S3): the Fable and Mythos families cannot switch it off either;
// Opus 5, Sonnet 5 and Haiku 5.5 can
// The same ids as internal/llm/thinking.go's anthropicAlwaysThinks (and its
// tests, thinking_on_test.go / thinking_off_c91_test.go): claude-opus-5-5
// followed by "-", "@" or "."; any id starting claude-fable- / claude-mythos-
for (const m of ['claude-fable-5', 'claude-fable-5-1', 'claude-fable-5-2', 'claude-mythos-5-1', 'claude-mythos-5-2', 'claude-fable-5-20261101', 'Claude-Fable-5-latest', ' claude-mythos-5-1@20261101 ', 'claude-opus-5-5.1', 'CLAUDE-OPUS-5-5']) {
  assert.equal(R.thinkingFloorsAtLow(m), true, m);
}
// W4c (R34c S1): what the back end does not run at effort low gets no note
// (a bare family name, ":" after the id, "@" or "." right after the family)
for (const m of ['claude-fable', 'claude-mythos', 'claude-mythos:beta', 'claude-opus-5-5:beta', 'claude-fable@20261101', 'claude-fable.5', 'claude-mythos_5']) {
  assert.equal(R.thinkingFloorsAtLow(m), false, m);
}
for (const m of ['claude-opus-5', 'claude-opus-5-20261101', 'claude-sonnet-5', 'claude-sonnet-5-5', 'claude-haiku-5-5', 'claude-opus-4-8', 'claude-fables-1', 'claude-mythology', 'x-claude-fable-1', 'fable-1', 'claude-mythos1']) {
  assert.equal(R.thinkingFloorsAtLow(m), false, m);
}

assert.match(R.ANTHROPIC_RATE_LIMITS_URL, /^https:\/\/platform\.claude\.com\/docs\/en\/api\/rate-limits$/);

console.log('settings v3.2.x checks: ok');
