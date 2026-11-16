// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// T-PERF-03 (user 2026-11-11: suggest, never apply): the account limits an
// Anthropic provider's answers reported (the provider view's rate_limits,
// llm.RateLimits: only Anthropic's own API host, the model with the smallest
// request limit seen in the last 24 hours) and the settings suggested from
// them (rate_limits.advice, llm.AdviseLimits). The provider dialog shows
// them under the concurrency and per-minute fields with "填入建议值", which
// only fills the two fields: the user still saves. A provider row whose
// settings are clearly below a suggestion that rests on measured answers
// gets a banner whose button opens the dialog with the suggestion filled
// in.

import { h, btn } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import { fmtInt, relTime, joinSentences } from './util.js';

// The id of the dialog's limits text (the text only, not its button or
// status): the concurrency and per-minute fields are described by it
// (aria-describedby) while it shows.
export const LIMITS_HINT_ID = 'pv-limits-observed';

// How long after the dialog opens the "filled in" status is written, so
// screen readers announce it (a live region's text present when it is
// inserted is usually not read).
export const ANNOUNCE_DELAY_MS = 120;

// The only host whose limits the back end reads and reports: https and
// api.anthropic.com (any case, any port). Keep in step with
// llm.OfficialAnthropicHost (internal/llm/ratelimits.go): a change there
// is a change here (review R38b N1, N2).
export function officialAnthropicHost(raw) {
  try {
    const u = new URL(String(raw == null ? '' : raw).trim());
    return u.protocol === 'https:' && u.hostname.toLowerCase() === 'api.anthropic.com';
  } catch {
    return false;
  }
}

// 'none': nothing observed (or not an Anthropic provider at the official
// host); 'below': the current settings are clearly below the suggestion;
// 'ok': observed, the settings are not below it.
export function rateLimitState(p) {
  const rl = p && p.type === 'anthropic' && officialAnthropicHost(p.base_url) ? p.rate_limits : null;
  if (!rl || !(rl.requests_per_minute > 0) || !rl.advice) return 'none';
  return rl.advice.below ? 'below' : 'ok';
}

// The suggestion as text: rpm 0 keeps "no per-minute limit".
export function adviceText(advice) {
  return advice.rpm > 0 ? t('settings.provider.rate_limits.advice', { max: fmtInt(advice.max_concurrency), rpm: fmtInt(advice.rpm) }) : t('settings.provider.rate_limits.advice_unlimited', { max: fmtInt(advice.max_concurrency) });
}

// What was observed: the model's request limit (when) and its token
// limits when both were reported.
function observedText(rl) {
  const parts = [t('settings.provider.rate_limits.observed', { rpm: fmtInt(rl.requests_per_minute), model: rl.model, when: relTime(rl.observed_at) })];
  if (rl.input_tokens_per_minute > 0 && rl.output_tokens_per_minute > 0) {
    parts.push(t('settings.provider.rate_limits.tokens', { input: fmtInt(rl.input_tokens_per_minute), output: fmtInt(rl.output_tokens_per_minute) }));
  }
  return joinSentences(parts);
}

// The row banner's text: which current setting is clearly below (R38 M1:
// never "the per-minute limit" when there is none).
export function belowText(p) {
  const rl = p.rate_limits;
  const a = rl.advice;
  const conc = !!a.concurrency_below;
  const rpm = !!a.rpm_below && p.rpm > 0;
  const which = conc && rpm ? 'both' : rpm ? 'rpm' : 'concurrency';
  return t(`settings.provider.rate_limits.card_below_${which}`, { model: rl.model, rpm: fmtInt(rl.requests_per_minute), conc: fmtInt(p.max_concurrency), cur_rpm: fmtInt(p.rpm) });
}

// fillAdvice puts the suggestion into the dialog's two fields, as typed
// by the user (an input event each), and nothing else: no request.
export function fillAdvice(advice, conc, rpm) {
  conc.value = String(advice.max_concurrency);
  rpm.value = String(advice.rpm);
  for (const el of [conc, rpm]) el.dispatchEvent(new Event('input', { bubbles: true }));
}

/**
 * The dialog's part: an element under the concurrency and per-minute
 * fields (span-2 of the dialog grid) and sync(type, keyTyped, baseURL),
 * which shows it for the Anthropic type at the official host only (no
 * other host ever reports limits, so nothing is promised there), hides it
 * while a new key is typed (its limits may be another account's), and
 * keeps the fields' descriptions in step. p: the provider being edited (null for a new one: nothing
 * observed); conc, rpm: the two inputs; filled: open with the suggestion
 * already filled in (the row banner's button).
 */
export function createLimitsHint(p, conc, rpm, { filled = false } = {}) {
  const state = rateLimitState(p);
  const rl = state === 'none' ? null : p.rate_limits;
  const status = h('span', { class: 'set-limits-filled', role: 'status' });
  const showFilled = () => {
    status.replaceChildren(icon('check', { size: 12 }), ' ', t('settings.provider.rate_limits.filled'));
  };
  // the described text: what was observed, the suggestion and its caveats
  const text = h(
    'span',
    { id: LIMITS_HINT_ID },
    rl
      ? [
          icon('gauge', { size: 12 }),
          ' ',
          // the sentences as one text: no space after a full-width stop
          joinSentences([
            observedText(rl),
            state === 'below' ? adviceText(rl.advice) : '',
            state === 'below' && !rl.advice.measured ? t('settings.provider.rate_limits.estimated') : '',
            t('settings.provider.rate_limits.caveat'),
            state === 'below' ? t('settings.provider.rate_limits.gradual') : '',
          ]),
        ]
      : p
        ? [icon('gauge', { size: 12 }), ' ', t('settings.provider.rate_limits.none')]
        : null,
  );
  const el = h(
    'div',
    { class: 'dlg-hint span-2 set-limits-observed', dataset: { fkey: 'set:prov:limits_observed', state }, style: { overflowWrap: 'anywhere' } },
    text,
    state === 'below'
      ? h(
          'div',
          { class: 'set-limits-actions', style: { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '8px', marginTop: '6px' } },
          btn(t('settings.provider.rate_limits.fill'), {
            size: 'sm',
            icon: 'gauge',
            fkey: 'set:prov:limits_fill',
            onclick: () => {
              fillAdvice(rl.advice, conc, rpm);
              showFilled();
            },
          }),
          status,
        )
      : null,
  );
  if (filled && state === 'below') {
    fillAdvice(rl.advice, conc, rpm);
    // written once the dialog shows, so it is announced (R38 S5)
    setTimeout(showFilled, ANNOUNCE_DELAY_MS);
  }
  // nothing to say for a new provider: the element stays hidden
  const empty = !p;
  const sync = (type, keyTyped = false, baseURL = '') => {
    const shown = !empty && type === 'anthropic' && !keyTyped && officialAnthropicHost(baseURL);
    el.hidden = !shown;
    for (const input of [conc, rpm]) {
      const ids = (input.getAttribute('aria-describedby') || '').split(/\s+/).filter((x) => x && x !== LIMITS_HINT_ID);
      if (shown) ids.push(LIMITS_HINT_ID);
      if (ids.length) input.setAttribute('aria-describedby', ids.join(' '));
      else input.removeAttribute('aria-describedby');
    }
  };
  return { el, sync };
}

/**
 * The provider row's banner when its settings are clearly below a
 * suggestion that rests on measured answers (null otherwise; R38 S1): the
 * observed limit, which setting is low, the suggestion, and a button that
 * opens the dialog with the suggestion filled in (onFill). Nothing is
 * saved from here.
 */
export function limitsBanner(p, onFill) {
  if (rateLimitState(p) !== 'below' || !p.rate_limits.advice.measured) return null;
  const rl = p.rate_limits;
  return h(
    'div',
    { class: 'banner banner-info set-banner pv-limits pv-rate-limits', style: { margin: '8px 0 0', flexWrap: 'wrap' } },
    icon('gauge', { size: 16 }),
    h('span', { class: 'set-banner-text', style: { flex: '1 1 16rem', overflowWrap: 'anywhere' } }, joinSentences([belowText(p), adviceText(rl.advice)])),
    btn(t('settings.provider.rate_limits.fill'), { size: 'sm', onclick: onFill, fkey: `set:prov:${p.id}:rate_limits` }),
  );
}
