// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// v3.2.x P1-b: the decisions of the retry model dialog (retryModelDialog in
// actions.js), kept free of the DOM so that testdata/retry_model_test.mjs
// can call them on their own (the one import, provider_rules.js, has none).

import { thinkingFloorsAtLow, MAX_TOKENS_LIMIT } from './provider_rules.js';

// The entry of a needs_model_choice diff for the retried stage itself; null
// when the stage is not in it (Q9a: only a later stage changed). Never
// another stage's entry.
export function ownDiff(diff, stage) {
  return (Array.isArray(diff) && diff.find((x) => x && x.stage === stage)) || null;
}

// "沿用原模型" keeps every snapshot of the diff, so it is offered only when
// all of them can be used (as the two-choice dialog does); bad: the first
// entry that cannot, whose reason the dialog shows.
export function snapshotChoice(diff) {
  const bad = (Array.isArray(diff) && diff.find((x) => x && x.snapshot_usable === false)) || null;
  return { usable: !bad, bad };
}

// "换模型重试…" opens the dialog without a 409, so without the back end's
// snapshot_usable: whether "沿用原模型" can work is judged here from what the
// page has (needs-human 08:37, ruling a; SPEC-v1.1 A4.3: not selectable,
// with the reason, when the snapshot's provider was deleted or its key no
// longer works), as settings.SnapshotUsableInfo judges an LLM stage: the
// provider still exists, its saved key can be read, and it has a key unless
// it is a local server. No snapshot, a snapshot without a provider, or no
// provider list (the settings could not be read) decide nothing: the back
// end still checks, and its refusal shows in the dialog. Same shape as
// snapshotChoice; bad carries the back end's reason codes
// (errors.snapshot.unusable.*) as reason_info.
export function entrySnapshotChoice(stage, snap, providers) {
  const ok = { usable: true, bad: null };
  if (!snap || !snap.provider_id || !Array.isArray(providers)) return ok;
  const p = providers.find((x) => x && x.id === snap.provider_id) || null;
  const provider = (p && p.name) || snap.provider_name || snap.provider_id;
  let code = '';
  if (!p) code = 'snapshot.unusable.provider_deleted';
  else if (p.key_unreadable) code = 'snapshot.unusable.api_key_unreadable';
  else if (p.key_hint === '' && serverKind(p.base_url) === 'remote') code = 'snapshot.unusable.no_api_key';
  if (!code) return ok;
  return { usable: false, bad: { stage, snapshot: snap, snapshot_usable: false, reason_info: { code, params: { provider } } } };
}

// G4 (T16, T18): whether "使用当前设置" can work, judged from the settings
// the page read: the stage's role is configured (else the back end's coded
// reason, settings.role_provider_deleted / role_not_configured), and its
// provider can be used as entrySnapshotChoice judges a snapshot's. No
// settings (providers not a list) or no such role decide nothing: the back
// end still checks. → {usable, info: the reason as {code, params}, or null}.
export function currentChoice(stage, role, providers) {
  const ok = { usable: true, info: null };
  if (!role || !Array.isArray(providers)) return ok;
  if (!role.configured) return { usable: false, info: role.error_info && role.error_info.code ? role.error_info : { code: 'settings.role_not_configured', params: { role: stage } } };
  const c = entrySnapshotChoice(stage, role.provider_id ? { provider_id: role.provider_id, model: role.model } : null, providers);
  return c.usable ? ok : { usable: false, info: c.bad.reason_info };
}

const LLM_STAGES = new Set(['summary', 'repair', 'translate']);

// R62 N2: "使用当前设置" for the retry of stage, as the back end checks it
// (sched retry, ModelChoiceCurrent): the stage and every later LLM stage of
// the lecture take the current settings, so each of their roles must be
// configured (currentSnapshots of withDownstream); the stage itself and the
// later LLM stages still pending need a usable provider (currentUnusable
// of check). A lecture with no recorded LLM model at all ("只转写",
// deferredLLM) is checked for nothing there, nor here. roles: the
// settings' roles; stages: the lecture's stages in pipeline order (without
// them only the stage itself is judged). Same shape as currentChoice.
export function currentChoiceFor(stage, roles, providers, stages) {
  const ok = { usable: true, info: null };
  if (!Array.isArray(roles) || !Array.isArray(providers)) return ok;
  const list = Array.isArray(stages) ? stages.filter(Boolean) : [];
  const llm = list.filter((s) => LLM_STAGES.has(s.stage));
  if (llm.length && llm.every((s) => !s.model)) return ok;
  const i = list.findIndex((s) => s.stage === stage);
  const later = i < 0 ? [] : list.slice(i + 1).filter((s) => LLM_STAGES.has(s.stage));
  const roleOf = (st) => roles.find((r) => r && r.role === st) || null;
  for (const st of [stage, ...later.map((s) => s.stage)]) {
    const r = roleOf(st);
    if (r && !r.configured) return currentChoice(st, r, providers);
  }
  for (const st of [stage, ...later.filter((s) => s.status === 'pending').map((s) => s.stage)]) {
    const c = currentChoice(st, roleOf(st), providers);
    if (!c.usable) return c;
  }
  return ok;
}

// R6: "使用当前设置" of the two-choice dialog (update, rerun, the retry of
// a stage that is not summary, repair or translate: modelChoiceDialog in
// actions.js), for ONE lecture's operation, judged from the 409's diff and
// the providers the page read. The back end takes the choice only when the
// current setting of every LLM stage it moves to the settings can be used
// (sched rerunSnapshots and retry → currentUnusable, which is
// settings.SnapshotUsableInfo of that setting). Every LLM stage listed in
// the diff is one of those stages, and its `current` is that setting; so
// the first such entry whose provider cannot be used, as entrySnapshotChoice
// judges a snapshot's, says the choice cannot work, with the back end's
// reason code. Nothing is decided without a provider list, or for an entry
// whose current side names no provider. Not seen here, and left to the back
// end's refusal: an LLM stage that takes the settings without being in the
// diff (its snapshot equals them), since the diff does not say which stages
// those are. Not for a batch: there the choice can still work for other
// lectures. Same shape as currentChoice.
export function diffCurrentChoice(diff, providers) {
  const ok = { usable: true, info: null };
  if (!Array.isArray(diff) || !Array.isArray(providers)) return ok;
  for (const d of diff) {
    if (!d || !LLM_STAGES.has(d.stage)) continue;
    const c = entrySnapshotChoice(d.stage, d.current, providers);
    if (!c.usable) return { usable: false, info: c.bad.reason_info };
  }
  return ok;
}

// The choice the dialog starts on: the snapshot, else the current settings,
// else `last`: the model named for this retry (G4: never a choice that
// cannot work); null for the two-choice dialog, which then starts on none.
export function initialChoice(snapshotUsable, currentUsable, last = 'manual') {
  if (snapshotUsable) return 'snapshot';
  return currentUsable ? 'current' : last;
}

/**
 * Whether a provider's address is a local server, which may run without a
 * key. The back end's settings.IsLocal (url.Parse, then the host is
 * "localhost", ends in ".localhost", or is a loopback IP: 127.0.0.0/8, ::1,
 * or 127.0.0.0/8 mapped into IPv6, ::ffff:127.x.y.z) decides; this answers
 * 'local' or 'remote' only where it reads the host as IsLocal does, and
 * 'unknown' wherever the browser's URL and Go's url.Parse may read the
 * host differently, so the entry decides nothing and leaves it to the back
 * end (R34, R34b). Unknown, where the front end is more careful than the
 * back end: an address that is not http(s) or has no host; a host the
 * browser rewrites — a trailing dot (127.0.0.1.), %-escapes
 * (%6cocalhost), non-ASCII letters (IDNA: ｌｏｃａｌｈｏｓｔ), an IPv4
 * address in another form (127.1, 0x7f.0.0.1, 0177.0.0.1) — and an IPv6
 * zone (%eth0). An IPv6 address is read in the browser's canonical form
 * ([::ffff:127.0.0.1] as [::ffff:7f00:1]), as net.ParseIP reads it.
 */
export function serverKind(baseURL) {
  const raw = String(baseURL == null ? '' : baseURL).trim();
  let u;
  try {
    u = new URL(raw);
  } catch {
    return 'unknown';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'unknown';
  const host = u.hostname.toLowerCase();
  if (!host) return 'unknown';
  // the host as written: the authority (after "//", up to the path, query
  // or fragment; the browser also ends it at "\"), after any user info,
  // without the port
  const m = /^[a-z][a-z0-9+.-]*:\/\/([^/?#\\]*)/i.exec(raw);
  if (!m) return 'unknown';
  const hostPort = m[1].slice(m[1].lastIndexOf('@') + 1);
  const written = (hostPort.startsWith('[') ? hostPort.slice(0, hostPort.indexOf(']') + 1) : hostPort.replace(/:\d*$/, '')).toLowerCase();
  if (!written || written.includes('%') || /[^\x21-\x7e]/.test(written)) return 'unknown';
  if (host.startsWith('[')) {
    if (!written.startsWith('[')) return 'unknown';
    const ip6 = host.slice(1, -1);
    if (ip6 === '::1' || /^::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}$/.test(ip6)) return 'local';
    return 'remote';
  }
  if (written !== host || written.endsWith('.')) return 'unknown';
  if (host === 'localhost' || host.endsWith('.localhost')) return 'local';
  if (/^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host)) return host.startsWith('127.') ? 'local' : 'remote';
  return 'remote';
}

// The retried stage's own models: snap, its snapshot (the diff's entry for
// the stage, else the model on the stage's card); current, the diff's
// current side for the stage, else roleCurrent (the stage's role in the
// settings). Nothing is taken from another stage.
export function stageModels(stage, diff, st, roleCurrent) {
  const d = ownDiff(diff, stage);
  return {
    snap: (d && d.snapshot) || (st && st.model) || null,
    current: (d && d.current) || roleCurrent || null,
  };
}

// The model "临时指定" starts from: the stage's snapshot when its provider
// still exists, else the stage's current model, else null (the fields start
// empty and the user picks).
export function manualStart(snap, current, providers) {
  const list = Array.isArray(providers) ? providers : [];
  const has = (m) => !!(m && m.provider_id && list.some((p) => p && p.id === m.provider_id));
  if (has(snap)) return snap;
  if (has(current)) return current;
  return null;
}

// The models offered for a provider: its last probe, then the models the
// given StageModels name on it (the field also takes any name).
export function knownModels(p, ...named) {
  const out = [...((p && p.models) || [])];
  for (const m of named) if (m && m.model && p && m.provider_id === p.id && !out.includes(m.model)) out.push(m.model);
  return out;
}

// The model field after the provider changed: the name stays only when the
// new provider is known to offer it; otherwise the field is emptied (and
// must be filled before sending) rather than sending another provider's
// model name.
export function modelAfterProviderChange(name, known) {
  const v = String(name == null ? '' : name).trim();
  return v && Array.isArray(known) && known.includes(v) ? v : '';
}

// Whether "使用当前设置" on the retry of a stage other than translate also
// clears the lecture's translations: the later translate stage switches to
// the current settings when the diff names it (Q10a).
export function currentClearsTranslations(stage, diff) {
  return stage !== 'translate' && !!ownDiff(diff, 'translate');
}

// A max_tokens field, as the settings page's output limit and the retry
// dialog's "临时指定" both read it (T18 M4: one rule for both, the back
// end's): null when empty (automatic); the number when it is written in
// decimal digits only and lies in 1..MAX_TOKENS_LIMIT
// (internal/settings/manual.go: an int in [1, MaxTokensLimit]; the request
// body's JSON integer has no exponent, fraction or sign, so "1e3", "1.0",
// "+5" are refused here rather than turned into a number the user did not
// write); else NaN, refused with the page's own message. badInput is the
// field's validity.badInput: text the browser cannot parse as a number
// (such as "12e") reads as an empty value, which must not pass for
// automatic. A number too long to be sent exactly never gets past the
// limit (C4, R12 A-2: it went out as 1e+25 for a generic 400).
export function parseMaxTokens(raw, badInput = false) {
  if (badInput) return NaN;
  const v = String(raw == null ? '' : raw).trim();
  if (!v) return null;
  if (!/^\d+$/.test(v)) return NaN;
  const n = Number(v);
  return n >= 1 && n <= MAX_TOKENS_LIMIT ? n : NaN;
}

// Whether "临时指定" with thinking off actually runs at the lowest level
// (C4, R6 A8): the model cannot switch thinking off (claude-opus-5-5, the
// Fable and Mythos families) and the provider is Anthropic's kind, whose
// client sends "off" as effort low; as the settings page's note. p: the
// chosen provider (null: none).
export function manualThinkingFloors(p, model, thinking) {
  return !!p && p.type === 'anthropic' && !!p.supports_thinking && thinking === 'off' && thinkingFloorsAtLow(model);
}
