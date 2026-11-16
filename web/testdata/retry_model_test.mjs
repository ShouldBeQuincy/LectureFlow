// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x P1-b: the decisions of the retry model dialog (web/retry_model.js):
// "沿用原模型" is offered only when every snapshot of the 409's diff can be
// used; "临时指定" starts from the retried stage's own model, never from
// another stage's; a provider change does not keep a model name the new
// provider is not known to offer. web/retry_model_test.go starts this with
// node; retry_model.js imports only provider_rules.js (no DOM either), so
// no DOM stand-in is needed.

import assert from 'node:assert/strict';
import { ownDiff, snapshotChoice, entrySnapshotChoice, serverKind, stageModels, manualStart, knownModels, modelAfterProviderChange, currentClearsTranslations, parseMaxTokens, manualThinkingFloors, currentChoice, initialChoice, currentChoiceFor, diffCurrentChoice } from '../retry_model.js';

const ds = { provider_id: 'ds', provider_name: 'DeepSeek', model: 'deepseek-chat' };
const dsNew = { provider_id: 'ds', provider_name: 'DeepSeek', model: 'deepseek-v4' };
const an = { provider_id: 'an', provider_name: 'Anthropic', model: 'claude-x' };
const providers = [
  { id: 'ds', name: 'DeepSeek', models: ['deepseek-chat', 'deepseek-v4'] },
  { id: 'an', name: 'Anthropic' },
];

// R1: one unusable snapshot anywhere in the diff disables "沿用原模型"
{
  const diff = [
    { stage: 'repair', snapshot: ds, current: dsNew, snapshot_usable: true },
    { stage: 'translate', snapshot: an, current: dsNew, snapshot_usable: false, reason: 'no key' },
  ];
  const c = snapshotChoice(diff);
  assert.equal(c.usable, false, 'a later stage with an unusable snapshot must disable the snapshot choice');
  assert.equal(c.bad, diff[1], 'the reason comes from the first unusable entry');
  assert.deepEqual(snapshotChoice([diff[0]]), { usable: true, bad: null });
  assert.equal(snapshotChoice([{ stage: 'repair', snapshot: ds, current: dsNew }]).usable, true, 'a missing flag is not a refusal');
  assert.equal(snapshotChoice([diff[1], diff[0]]).bad, diff[1]);
  assert.deepEqual(snapshotChoice(null), { usable: true, bad: null });
}

// R2: the retried stage is not in the diff (only translate changed): its
// own card model and role are used, never the translate entry
{
  const diff = [{ stage: 'translate', snapshot: an, current: dsNew, snapshot_usable: true }];
  assert.equal(ownDiff(diff, 'repair'), null);
  assert.equal(ownDiff(diff, 'translate'), diff[0]);
  const st = { stage: 'repair', model: ds };
  const roleRepair = { provider_id: 'ds', model: 'deepseek-v4' };
  let m = stageModels('repair', diff, st, roleRepair);
  assert.equal(m.snap, ds, "the stage card's model is the snapshot");
  assert.equal(m.current, roleRepair, "the stage's role is the current side");
  assert.equal(manualStart(m.snap, m.current, providers), ds);
  // nothing of the stage's own: empty, not the translate entry
  m = stageModels('repair', diff, { stage: 'repair' }, null);
  assert.equal(m.snap, null);
  assert.equal(m.current, null);
  assert.equal(manualStart(m.snap, m.current, providers), null, 'nothing to prefill: the fields start empty');
  // the stage's own entry wins over the card
  const own = [{ stage: 'repair', snapshot: an, current: dsNew, snapshot_usable: true }, diff[0]];
  m = stageModels('repair', own, st, roleRepair);
  assert.equal(m.snap, an);
  assert.equal(m.current, dsNew);
  // a snapshot whose provider is gone falls back to the stage's current model
  assert.equal(manualStart({ provider_id: 'gone', model: 'x' }, roleRepair, providers), roleRepair);
  assert.equal(manualStart({ provider_id: 'gone', model: 'x' }, null, providers), null);
  // the offered models name only this provider's
  assert.deepEqual(knownModels(providers[1], ds, an), ['claude-x']);
  assert.deepEqual(knownModels(providers[0], ds, an), ['deepseek-chat', 'deepseek-v4']);
  assert.deepEqual(knownModels(null, ds), []);
}

// R3: a provider change keeps the model only when the new provider offers it
{
  assert.equal(modelAfterProviderChange('deepseek-chat', []), '', 'a provider with no probed models empties the field');
  assert.equal(modelAfterProviderChange('deepseek-chat', ['claude-x']), '', 'a name not on the list is not kept');
  assert.equal(modelAfterProviderChange(' claude-x ', ['claude-x']), 'claude-x');
  assert.equal(modelAfterProviderChange('', ['claude-x']), '');
  assert.equal(modelAfterProviderChange(undefined, null), '');
}

// A6: "使用当前设置" on a repair retry also switches a later translate stage
{
  const diff = [{ stage: 'translate', snapshot: an, current: dsNew, snapshot_usable: true }];
  assert.equal(currentClearsTranslations('repair', diff), true);
  assert.equal(currentClearsTranslations('repair', [{ stage: 'repair', snapshot: ds, current: dsNew }]), false);
  assert.equal(currentClearsTranslations('translate', diff), false, 'translate has its own warning');
  assert.equal(currentClearsTranslations('summary', null), false);
}

// max_tokens of "临时指定": empty = automatic; 0, negatives, fractions and
// text are refused by the dialog itself (the form is novalidate)
{
  assert.equal(parseMaxTokens(''), null);
  assert.equal(parseMaxTokens('  '), null);
  assert.equal(parseMaxTokens(undefined), null);
  assert.equal(parseMaxTokens(' 4096 '), 4096);
  assert.equal(parseMaxTokens('1'), 1);
  for (const bad of ['0', '00', '-1', '1.5', '1e3', 'abc']) assert.ok(Number.isNaN(parseMaxTokens(bad)), `max_tokens ${bad} must be refused`);
  // a number field the browser cannot parse ("12e") reads as '' with
  // validity.badInput set: refused, not taken for automatic
  assert.ok(Number.isNaN(parseMaxTokens('', true)), 'badInput must be refused');
  assert.ok(Number.isNaN(parseMaxTokens('4096', true)), 'badInput wins over the value');
  assert.equal(parseMaxTokens('', false), null, 'empty without badInput stays automatic');
  assert.equal(parseMaxTokens('4096', false), 4096);
  // C4 (R12 A-2): a number too long to be sent exactly is refused here, not
  // sent as 1e+25 for a generic 400; a safe one goes to the back end's range check
  for (const long of ['9007199254740992', '99999999999999999999', '1'.repeat(400)]) assert.ok(Number.isNaN(parseMaxTokens(long)), `max_tokens ${long.slice(0, 24)} must be refused`);
  // T18 M4: the settings page's and the back end's range, 1..1000000
  assert.equal(parseMaxTokens('1000000'), 1000000, 'the limit itself');
  for (const bad of ['1000001', '2000000', '1e3', '1.0', '+5', ' 1 2 ', '0x10']) assert.ok(Number.isNaN(parseMaxTokens(bad)), `max_tokens ${bad} must be refused`);
  assert.equal(parseMaxTokens('0100'), 100, 'leading zeros: the digits');
}

// W4 (needs-human 08:37, a): "换模型重试…" (no 409) judges the stage's own
// snapshot from the providers the page read, as settings.SnapshotUsableInfo
{
  const ok = { usable: true, bad: null };
  const reason = (c) => c.bad && c.bad.reason_info;
  const list = (p) => [p, { id: 'an', name: 'Anthropic', base_url: 'https://api.anthropic.com', key_hint: 'sk-…wxyz' }];
  const withKey = { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' };
  assert.deepEqual(entrySnapshotChoice('translate', ds, list(withKey)), ok, 'provider with a key');
  // the provider was deleted: its name from the snapshot
  let c = entrySnapshotChoice('repair', ds, [list(withKey)[1]]);
  assert.equal(c.usable, false);
  assert.deepEqual(reason(c), { code: 'snapshot.unusable.provider_deleted', params: { provider: 'DeepSeek' } });
  assert.equal(c.bad.stage, 'repair');
  assert.equal(c.bad.snapshot_usable, false);
  assert.equal(c.bad.snapshot, ds);
  // no key
  c = entrySnapshotChoice('translate', ds, list({ ...withKey, key_hint: '' }));
  assert.deepEqual(reason(c), { code: 'snapshot.unusable.no_api_key', params: { provider: 'DeepSeek' } });
  // a key that cannot be decrypted (key_hint is "" then too)
  c = entrySnapshotChoice('translate', ds, list({ ...withKey, key_hint: '', key_unreadable: true }));
  assert.deepEqual(reason(c), { code: 'snapshot.unusable.api_key_unreadable', params: { provider: 'DeepSeek' } });
  // a local server runs without a key
  for (const base of ['http://localhost:11434/v1', 'http://127.0.0.1:8080', 'http://[::1]:8000', 'http://llm.localhost']) {
    assert.deepEqual(entrySnapshotChoice('translate', ds, list({ ...withKey, base_url: base, key_hint: '' })), ok, base);
  }
  assert.equal(entrySnapshotChoice('translate', ds, list({ ...withKey, base_url: 'http://192.0.2.10:8080', key_hint: '' })).usable, false, 'a LAN server needs a key');
  // W4b (R34): the back end's settings.IsLocal (net.ParseIP(...).IsLoopback)
  // also takes 127/8 mapped into IPv6 and the long form of ::1; addresses
  // the two parsers may read differently decide nothing
  const keyless = (base) => entrySnapshotChoice('translate', ds, list({ ...withKey, base_url: base, key_hint: '' }));
  for (const base of [
    'http://[::ffff:127.0.0.1]:8080',
    'http://[::FFFF:7f00:1]',
    'http://[::ffff:127.255.0.9]/v1',
    'http://[0:0:0:0:0:0:0:1]:11434',
    'HTTP://LOCALHOST:1234',
    'http://user@127.0.0.1:8080',
    'http://127.1:8080', // the browser reads 127.0.0.1, Go a host name: unknown
    'http://0x7f.0.0.1',
    'http://0177.0.0.1',
    'localhost:11434', // no scheme: unknown
    'ftp://127.0.0.1',
    'not an address',
    '',
    undefined,
  ]) {
    assert.deepEqual(keyless(base), ok, `local or unknown: ${base}`);
  }
  for (const base of ['http://[::ffff:192.0.2.1]', 'http://[2001:db8::1]:8000', 'http://[::127.0.0.1]', 'https://api.example.com/v1', 'http://localhost.example.com', 'http://localhost@192.0.2.1', 'http://x@localhost@example.com']) {
    assert.equal(keyless(base).usable, false, `remote: ${base}`);
  }
  assert.equal(serverKind('http://[::ffff:127.0.0.1]'), 'local');
  assert.equal(serverKind('http://127.1'), 'unknown');
  assert.equal(serverKind('http://[fe80::1%25eth0]'), 'unknown', 'a zone the browser cannot read');
  assert.equal(serverKind('https://api.example.com'), 'remote');
  // W4c (R34b B): the host as written, not anything later in the address,
  // is what the browser's host is compared with; a host the browser
  // rewrites is unknown, never local (the back end may read it as remote)
  for (const [base, want] of [
    ['http://127.0.0.1.', 'unknown'], // trailing dot: the browser drops it
    ['http://localhost.:8080', 'unknown'],
    ['http://foo.localhost.', 'unknown'],
    ['http://127.1/?q=//127.0.0.1', 'unknown'], // "//127.0.0.1" in the query
    ['http://127.1/x@127.0.0.1', 'unknown'],
    ['http://%6cocalhost', 'unknown'], // %-escape: decoded by the browser only
    ['http://ｌｏｃａｌｈｏｓｔ', 'unknown'], // full-width: IDNA maps it
    ['http://bücher.localhost', 'unknown'], // non-ASCII
    ['http://[::1%eth0]', 'unknown'],
    ['http://127.0.0.01', 'unknown'],
    ['http://2130706433', 'unknown'],
    ['http://evil.example@127.0.0.1', 'local'],
    ['http://localhost@evil.example', 'remote'],
    ['http://[::1]', 'local'],
    ['http://[0::1]:8080', 'local'],
    ['https://localhost:8443/v1?x=1#y', 'local'],
  ]) {
    assert.equal(serverKind(base), want, base);
  }
  // what the page cannot tell decides nothing: the back end still checks
  assert.deepEqual(entrySnapshotChoice('translate', null, list(withKey)), ok, 'no snapshot on the card');
  assert.deepEqual(entrySnapshotChoice('translate', { model: 'x' }, list(withKey)), ok, 'a snapshot without a provider');
  assert.deepEqual(entrySnapshotChoice('translate', ds, null), ok, 'the settings could not be read');
  assert.deepEqual(entrySnapshotChoice('translate', ds, list({ id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com' })), ok, 'a provider view without key_hint');
  // the provider's current name wins over the snapshot's
  c = entrySnapshotChoice('translate', ds, list({ ...withKey, name: 'DeepSeek (work)', key_hint: '' }));
  assert.equal(reason(c).params.provider, 'DeepSeek (work)');
}

// C4 (R6 A8): "临时指定" with thinking off runs at the lowest level only for
// a model that cannot switch thinking off on an Anthropic provider
{
  const an = { id: 'an', type: 'anthropic', supports_thinking: true };
  const oa = { id: 'oa', type: 'openai', supports_thinking: true };
  assert.equal(manualThinkingFloors(an, 'claude-opus-5-5', 'off'), true);
  assert.equal(manualThinkingFloors(an, 'claude-fable-1', 'off'), true);
  assert.equal(manualThinkingFloors(an, 'claude-opus-5-5', 'low'), false, 'not off');
  assert.equal(manualThinkingFloors(an, 'claude-opus-5', 'off'), false, 'a model that can switch it off');
  assert.equal(manualThinkingFloors(oa, 'claude-opus-5-5', 'off'), false, 'an OpenAI-compatible provider');
  assert.equal(manualThinkingFloors({ ...an, supports_thinking: false }, 'claude-opus-5-5', 'off'), false, 'no thinking field');
  assert.equal(manualThinkingFloors(null, 'claude-opus-5-5', 'off'), false, 'no provider');
}

// G4 (T16, T18): "使用当前设置" is judged from the settings the page read:
// a role that is not configured (its provider deleted, never set), or whose
// provider has no key or an unreadable one, cannot be used; the dialog then
// starts on a choice that can
{
  const ok = { usable: true, info: null };
  const ps = [
    { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' },
    { id: 'or', name: 'OpenRouter', base_url: 'https://openrouter.ai/api/v1', key_hint: '' },
    { id: 'lo', name: 'Local', base_url: 'http://127.0.0.1:8080/v1', key_hint: '' },
  ];
  const role = (o) => ({ role: 'translate', configured: true, provider_id: 'ds', model: 'deepseek-chat', ...o });
  assert.deepEqual(currentChoice('translate', role(), ps), ok, 'configured, with a key');
  assert.deepEqual(currentChoice('translate', role({ provider_id: 'lo' }), ps), ok, 'a local server needs no key');
  // the back end's coded reason is kept
  const deleted = { code: 'settings.role_provider_deleted', params: { role: 'translate' } };
  let c = currentChoice('translate', role({ configured: false, error_info: deleted, error: 'gone' }), ps);
  assert.equal(c.usable, false, 'the provider was deleted');
  assert.equal(c.info, deleted, "the back end's reason");
  c = currentChoice('translate', role({ configured: false, provider_id: null, model: null }), ps);
  assert.equal(c.usable, false, 'never configured');
  assert.deepEqual(c.info, { code: 'settings.role_not_configured', params: { role: 'translate' } }, 'not configured, without a coded reason');
  c = currentChoice('translate', role({ provider_id: 'or' }), ps);
  assert.equal(c.usable, false, 'a remote provider without a key');
  assert.deepEqual(c.info, { code: 'snapshot.unusable.no_api_key', params: { provider: 'OpenRouter' } });
  c = currentChoice('translate', role(), [{ ...ps[0], key_unreadable: true }]);
  assert.equal(c.usable, false, 'an unreadable key');
  assert.equal(c.info.code, 'snapshot.unusable.api_key_unreadable');
  c = currentChoice('translate', role({ provider_id: 'xx' }), ps);
  assert.equal(c.usable, false, 'configured on a provider the page does not have');
  assert.equal(c.info.code, 'snapshot.unusable.provider_deleted');
  // what the page cannot tell decides nothing
  assert.deepEqual(currentChoice('translate', role({ configured: false }), null), ok, 'the settings could not be read');
  assert.deepEqual(currentChoice('translate', null, ps), ok, 'the role is not in the settings');
  // the first choice that can be used
  assert.equal(initialChoice(true, true), 'snapshot');
  assert.equal(initialChoice(true, false), 'snapshot');
  assert.equal(initialChoice(false, true), 'current');
  assert.equal(initialChoice(false, false), 'manual', 'neither: the model for this retry');
}

// R62 N2: "使用当前设置" moves the stage and every later LLM stage to the
// settings (sched retry: currentSnapshots(withDownstream), currentUnusable
// of the pending ones): every later LLM role must be configured, and the
// pending later stages' providers need a key
{
  const ok = { usable: true, info: null };
  const ps = [
    { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' },
    { id: 'or', name: 'OpenRouter', base_url: 'https://openrouter.ai/api/v1', key_hint: '' },
  ];
  const role = (r, o) => ({ role: r, configured: true, provider_id: 'ds', model: 'm', ...o });
  const m = { provider_id: 'ds', model: 'm' };
  const st = (stage, status, model = m) => ({ stage, status, model });
  const stages = (sum, rep, tr) => [st('prep', 'done', null), st('asr', 'done', null), st('summary', ...sum), st('repair', ...rep), st('translate', ...tr), st('export', 'pending', null)];
  const fine = ['summary', 'repair', 'translate'].map((r) => role(r));
  const lec = stages(['failed'], ['pending'], ['pending']);
  assert.deepEqual(currentChoiceFor('summary', fine, ps, lec), ok, 'all configured, with keys');
  // a later role not configured (its provider deleted): refused, its reason
  const gone = { code: 'settings.role_provider_deleted', params: { role: 'translate' } };
  let roles = [role('summary'), role('repair'), role('translate', { configured: false, error_info: gone })];
  let c = currentChoiceFor('summary', roles, ps, lec);
  assert.equal(c.usable, false, 'a later role is not configured');
  assert.equal(c.info, gone);
  // ... also when that later stage is done (every later LLM stage is snapshotted anew)
  c = currentChoiceFor('summary', roles, ps, stages(['failed'], ['done'], ['done']));
  assert.equal(c.usable, false, 'configured is checked for done later stages too');
  // an earlier role does not matter
  roles = [role('summary', { configured: false }), role('repair'), role('translate')];
  assert.deepEqual(currentChoiceFor('repair', roles, ps, stages(['done'], ['failed'], ['pending'])), ok, 'an earlier role is not checked');
  // a later pending stage's provider without a key: refused; a done one is not checked for keys
  roles = [role('summary'), role('repair'), role('translate', { provider_id: 'or' })];
  c = currentChoiceFor('summary', roles, ps, stages(['failed'], ['pending'], ['pending']));
  assert.equal(c.usable, false, 'a pending later stage without a key');
  assert.deepEqual(c.info, { code: 'snapshot.unusable.no_api_key', params: { provider: 'OpenRouter' } });
  assert.deepEqual(currentChoiceFor('summary', roles, ps, stages(['failed'], ['pending'], ['done'])), ok, 'a done later stage: its key is not checked');
  // the stage itself is checked as before
  roles = [role('summary', { provider_id: 'or' }), role('repair'), role('translate')];
  assert.equal(currentChoiceFor('summary', roles, ps, lec).usable, false, 'the stage itself');
  // "只转写": no LLM stage has a recorded model, the back end checks none
  const deferred = stages(['failed', null], ['pending', null], ['pending', null]);
  roles = [role('summary'), role('repair'), role('translate', { configured: false })];
  assert.deepEqual(currentChoiceFor('summary', roles, ps, deferred), ok, 'transcribe only: nothing decided');
  // no settings: nothing decided; no stages: the stage alone
  assert.deepEqual(currentChoiceFor('summary', null, ps, lec), ok);
  assert.deepEqual(currentChoiceFor('summary', fine, null, lec), ok);
  assert.equal(currentChoiceFor('summary', [role('summary', { provider_id: 'or' })], ps, null).usable, false, 'without stages the stage itself is still checked');
}

// R6: the two-choice dialog (update, rerun) of one lecture judges "使用当前
// 设置" from the 409's diff: the current setting of an LLM stage listed
// there is one the back end checks before it takes the choice (sched
// rerunSnapshots / retry → currentUnusable), by the provider it names
{
  const ok = { usable: true, info: null };
  const withKey = { id: 'ds', name: 'DeepSeek', base_url: 'https://api.deepseek.com', key_hint: 'sk-…abcd' };
  const noKey = { id: 'or', name: 'OpenRouter', base_url: 'https://openrouter.ai/api/v1', key_hint: '' };
  const local = { id: 'ol', name: 'Ollama', base_url: 'http://localhost:11434/v1', key_hint: '' };
  const unreadable = { id: 'kx', name: 'Keyed', base_url: 'https://example.com/v1', key_hint: 'sk-…zz', key_unreadable: true };
  const ps = [withKey, noKey, local, unreadable];
  const cur = (id, name) => ({ provider_id: id, provider_name: name, model: 'm' });
  const entry = (stage, current, usable = false) => ({ stage, snapshot: an, current, snapshot_usable: usable });
  // the current setting has a key (or is a local server): usable
  assert.deepEqual(diffCurrentChoice([entry('summary', cur('ds', 'DeepSeek')), entry('translate', cur('ol', 'Ollama'))], ps), ok);
  // a remote provider without a key: not usable, with the back end's reason
  assert.deepEqual(diffCurrentChoice([entry('translate', cur('or', 'OpenRouter'))], ps), { usable: false, info: { code: 'snapshot.unusable.no_api_key', params: { provider: 'OpenRouter' } } });
  // a key that cannot be read; a provider deleted since the 409
  assert.deepEqual(diffCurrentChoice([entry('repair', cur('kx', 'Keyed'))], ps).info, { code: 'snapshot.unusable.api_key_unreadable', params: { provider: 'Keyed' } });
  assert.deepEqual(diffCurrentChoice([entry('repair', cur('gone', 'Gone'))], ps).info, { code: 'snapshot.unusable.provider_deleted', params: { provider: 'Gone' } });
  // the first one in the diff's (pipeline) order gives the reason
  assert.equal(diffCurrentChoice([entry('summary', cur('ds', 'DeepSeek')), entry('repair', cur('or', 'OpenRouter')), entry('translate', cur('kx', 'Keyed'))], ps).info.code, 'snapshot.unusable.no_api_key');
  // whatever the snapshot's own state: the choice is judged by the current side
  assert.equal(diffCurrentChoice([entry('translate', cur('or', 'OpenRouter'), true)], ps).usable, false);
  // asr and the other stages are not checked by the back end either: their
  // "current" names no LLM provider
  assert.deepEqual(diffCurrentChoice([entry('asr', { label: 'large-v2', provider_id: 'or' }), entry('export', cur('or', 'OpenRouter'))], ps), ok, 'only the LLM stages');
  // nothing to judge by: no current side, no provider on it, no provider
  // list (the settings could not be read), no diff
  assert.deepEqual(diffCurrentChoice([entry('summary', null), entry('repair', { label: '（无）' })], ps), ok);
  assert.deepEqual(diffCurrentChoice([entry('translate', cur('or', 'OpenRouter'))], null), ok, 'no settings: left to the back end');
  assert.deepEqual(diffCurrentChoice(null, ps), ok);
  assert.deepEqual(diffCurrentChoice([null], ps), ok);
  // the choice the two-choice dialog starts on: the snapshot, else the
  // current settings, else none (nothing to continue with)
  assert.equal(initialChoice(true, true), 'snapshot');
  assert.equal(initialChoice(false, true), 'current');
  assert.equal(initialChoice(true, false), 'snapshot');
  assert.equal(initialChoice(false, false, null), null, 'two choices, neither usable: none preselected');
  assert.equal(initialChoice(false, false), 'manual', 'the retry dialog still falls to its third choice');
}

console.log('retry model checks: ok');
