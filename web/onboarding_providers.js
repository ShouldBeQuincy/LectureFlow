// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Onboarding step 4, "translation and repair" (SPEC-v3 V4.2): a cost tier
// (from the manifest's presets, GET /api/components/presets — nothing about
// the tiers is written here), then the API key of each service the tier
// uses: numbered steps (where to sign up, where to create the key, roughly
// what it costs), "test connection", and when the key is already in an
// environment variable, "use the key from the environment" (the server
// reads it; the key never passes through the browser). Free channels carry
// the note that some free services may use what is sent to improve their
// models. Applying the tier sets every role to its service and model
// through the existing role settings; the zero-cost tier also turns on
// "only dispatch off-peak" for its free services with a night window; the
// transcription-only tier configures no service at all.

import { h, btn, confirmDialog } from './dom.js';
import { icon } from './icons.js';
import { t, has } from './i18n.js';
import * as U from './util.js';
import { obApi, desktop, openExternal } from './onboarding_api.js';

const FREE = '$free';

// The zero-cost tier's processing window: requests are only sent between
// 23:00 and 08:00 local time (a peak window covering the day, off-peak only).
const NIGHT_FROM = '08:00';
const NIGHT_TO = '23:00';

function nightTariff() {
  let tz = 'UTC';
  try {
    tz = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  } catch {
    tz = 'UTC';
  }
  return { timezone: tz, peak_windows: [{ days: [1, 2, 3, 4, 5, 6, 7], from: NIGHT_FROM, to: NIGHT_TO }], offpeak_multiplier: 1 };
}

const sameURL = (a, b) => String(a || '').replace(/\/+$/, '').toLowerCase() === String(b || '').replace(/\/+$/, '').toLowerCase();

function text(key, params) {
  return key && has(key) ? t(key, params) : '';
}

// A preset's link (signup_url, keys_url, pricing_url, terms_url) is a link
// only when it is https (AUD5 info-1: the list may come from the network
// one day; the other external links of the app check the same): anything
// else is not drawn at all.
const httpsURL = (u) => (typeof u === 'string' && /^https:\/\/\S+$/i.test(u) ? u : '');

function extLink(url, label, fkey) {
  const href = httpsURL(url);
  if (!href) return null;
  return h(
    'a',
    {
      class: 'ob-ext',
      href,
      target: '_blank',
      rel: 'noopener noreferrer',
      dataset: { fkey },
      onclick: (e) => {
        if (desktop()) {
          e.preventDefault();
          openExternal(href);
        }
      },
    },
    label,
    icon('external-link', { size: 12 }),
  );
}

export function providersStep(env) {
  const S = {
    loading: true,
    error: '',
    presets: null,
    envKeys: [],
    providers: [],
    region: '',
    tier: env.data.tier || '',
    keys: {}, // preset provider id → typed key (never saved in the onboarding state)
    editing: new Set(), // preset ids whose key is being replaced
    busy: new Set(), // preset ids with a request in flight
    tests: {}, // preset id → {state, text}
    applying: false,
  };

  load();

  async function load() {
    S.loading = true;
    S.error = '';
    env.rerender();
    try {
      const [presets, envKeys, providers, sources] = await Promise.all([
        obApi.presets({ quiet: true }),
        obApi.envKeys({ quiet: true }).catch(() => []),
        obApi.providers({ quiet: true }),
        obApi.sources({ quiet: true }).catch(() => null),
      ]);
      S.presets = presets || { tiers: [], providers: [] };
      S.envKeys = Array.isArray(envKeys) ? envKeys : [];
      S.providers = providers || [];
      S.region = sources && sources.suggest_mirror ? 'cn' : '';
      if (!S.tier || !tierByID(S.tier)) S.tier = (tierByID(S.presets.default) && S.presets.default) || (tiers()[0] && tiers()[0].id) || '';
    } catch (e) {
      S.error = e.message;
    }
    S.loading = false;
    if (env.alive()) env.rerender();
  }

  async function reloadProviders() {
    try {
      S.providers = (await obApi.providers({ quiet: true })) || [];
    } catch {
      // keep the last list
    }
  }

  // r7 (T22 S6): the settings page offers this step in a dialog ("add from
  // a preset", env.inSettings): there "transcription only" is no choice (it
  // would change nothing; a provider is removed in the list itself)
  const tiers = () => ((S.presets && S.presets.tiers) || []).filter((x) => !(env.inSettings && x.transcribe_only));
  const tierByID = (id) => tiers().find((x) => x.id === id) || null;
  const presetByID = (id) => ((S.presets && S.presets.providers) || []).find((p) => p.id === id) || null;

  function freeID() {
    const f = (S.presets && S.presets.free) || {};
    return (f.by_region && S.region && f.by_region[S.region]) || f.default || '';
  }

  const resolve = (id) => (id === FREE ? freeID() : id);

  // The services a tier uses, in the presets' order.
  function tierProviders(tier) {
    if (!tier || tier.transcribe_only) return [];
    const ids = new Set(Object.values(tier.roles || {}).map((r) => resolve(r.provider)));
    return ((S.presets && S.presets.providers) || []).filter((p) => ids.has(p.id));
  }

  // r7 (R75 M1): what the chosen plan would set, for the settings' dialog,
  // which never runs apply(): it changes roles only after its own
  // confirmation and keeps their parameters. Each role of the plan with its
  // preset service, the configured provider when it has a key (else null)
  // and the model; the free services the zero-cost plan sends at night
  // only (configured, with a key). null for no plan or transcription only.
  function planOf() {
    const tier = tierByID(S.tier);
    if (!tier || tier.transcribe_only) return null;
    const roles = Object.entries(tier.roles || {}).map(([role, r]) => {
      const pp = presetByID(resolve(r.provider));
      return { role, preset: pp, provider: pp && ready(pp) ? existing(pp) : null, model: r.model || (pp && pp.default_model) || '' };
    });
    const night = tier.night_processing ? tierProviders(tier).filter((pp) => pp.free && ready(pp)).map(existing) : [];
    return { tier, name: text(tier.name_key) || tier.id, roles, night, nightTariff: nightTariff(), nightFrom: NIGHT_TO, nightTo: NIGHT_FROM };
  }

  // The configured provider row of a preset service (same base URL).
  const existing = (pp) => S.providers.find((p) => sameURL(p.base_url, pp.base_url)) || null;
  const ready = (pp) => {
    const p = existing(pp);
    return !!p && !!p.key_hint;
  };

  async function chooseTier(id) {
    S.tier = id;
    env.data.tier = id;
    env.save();
    env.rerender();
  }

  async function test(pp) {
    const p = existing(pp);
    if (!p) return;
    S.tests[pp.id] = { state: 'running' };
    env.rerender();
    let r;
    try {
      // a new service has no role and no model list yet: the test needs a
      // model name, so list its models first (that also checks the key)
      if (!(p.used_by_roles || []).length && !(p.models || []).length) {
        await obApi.probeModels(p.id, { quiet: true }).catch(() => null);
        await reloadProviders();
      }
      r = await obApi.testProvider(p.id, { quiet: true });
      S.tests[pp.id] = r && r.ok ? { state: 'ok', text: t('onboarding.providers.test_ok', { ms: U.fmtInt(r.latency_ms || 0) }) } : { state: 'fail', text: (r && (U.errorInfoText(r.error_info, '') || r.error)) || t('common.unknown_error') };
    } catch (e) {
      S.tests[pp.id] = { state: 'fail', text: e.message };
    }
    if (env.alive()) env.rerender();
  }

  async function useEnv(pp) {
    S.busy.add(pp.id);
    env.rerender();
    try {
      await obApi.useEnvKey(pp.id);
      await reloadProviders();
      S.editing.delete(pp.id);
    } catch {
      // shown by the error handler
    }
    S.busy.delete(pp.id);
    if (!env.alive()) return;
    env.rerender();
    if (ready(pp)) test(pp);
  }

  async function saveKey(pp) {
    const key = (S.keys[pp.id] || '').trim();
    if (!key) return;
    S.busy.add(pp.id);
    env.rerender();
    try {
      const p = existing(pp);
      if (p) await obApi.patchProvider(p.id, { api_key: key });
      else {
        const input = { name: pp.name, type: pp.type, base_url: pp.base_url, api_key: key };
        if (pp.balance_api) input.balance_api = pp.balance_api;
        if (pp.max_concurrency) input.max_concurrency = pp.max_concurrency;
        input.rpm = pp.rpm || 0; // a preset without rpm: no per-minute limit (perf-llm; DeepSeek)
        if (S.providers.some((x) => x.name === pp.name)) input.name = `${pp.name} (${pp.id})`;
        await obApi.createProvider(input);
      }
      S.keys[pp.id] = '';
      S.editing.delete(pp.id);
      await reloadProviders();
    } catch {
      // shown by the error handler
    }
    S.busy.delete(pp.id);
    if (!env.alive()) return;
    env.rerender();
    if (ready(pp)) test(pp);
  }

  // Applies the tier: roles → services and models; the zero-cost tier's
  // night window. Services without a key are left out (said first).
  async function apply() {
    const tier = tierByID(S.tier);
    if (!tier) return;
    if (tier.transcribe_only) {
      await env.setData({ tier: tier.id, applied_tier: tier.id, providers_missing: [] });
      env.next();
      return;
    }
    const missing = tierProviders(tier).filter((pp) => !ready(pp));
    if (missing.length) {
      const ok = await confirmDialog({
        title: t('onboarding.providers.missing_title'),
        message: t('onboarding.providers.missing', { names: U.listJoin(missing.map((p) => p.name)) }),
        okText: t('onboarding.providers.missing_continue'),
        cancelText: t('onboarding.providers.missing_back'),
      });
      if (!ok || !env.alive()) return;
    }
    S.applying = true;
    env.rerender();
    let failed = false;
    for (const [role, r] of Object.entries(tier.roles || {})) {
      const pp = presetByID(resolve(r.provider));
      const p = pp && ready(pp) ? existing(pp) : null;
      if (!p) continue;
      try {
        await obApi.setRole(role, p.id, r.model || pp.default_model);
      } catch {
        failed = true; // shown by the error handler
        break;
      }
    }
    if (!failed && tier.night_processing) {
      for (const pp of tierProviders(tier)) {
        const p = existing(pp);
        if (!p || !pp.free || !ready(pp)) continue;
        try {
          await obApi.patchProvider(p.id, { tariff: nightTariff(), offpeak_only: true });
        } catch {
          failed = true;
          break;
        }
      }
    }
    S.applying = false;
    if (!env.alive()) return;
    if (failed) {
      env.rerender();
      return;
    }
    await env.setData({ tier: tier.id, applied_tier: tier.id, providers_missing: missing.map((p) => p.id) });
    env.next();
  }

  // ---- rendering ----

  function tierCards() {
    return h(
      'fieldset',
      { class: 'ob-field' },
      h('legend', { class: 'ob-label' }, t('onboarding.providers.tier_label')),
      h(
        'div',
        { class: 'ob-tiers', role: 'radiogroup' },
        tiers().map((tier) =>
          h(
            'label',
            { class: 'ob-tier' + (tier.id === S.tier ? ' is-on' : '') },
            h('input', { type: 'radio', name: 'ob-tier', value: tier.id, checked: tier.id === S.tier, dataset: { fkey: 'ob:tier:' + tier.id }, onchange: () => chooseTier(tier.id) }),
            h('span', { class: 'ob-tier-text' }, h('span', { class: 'ob-tier-name' }, text(tier.name_key) || tier.id), h('span', { class: 'ob-tier-desc' }, text(tier.desc_key)), text(tier.cost_key) ? h('span', { class: 'ob-tier-cost' }, text(tier.cost_key)) : null),
          ),
        ),
      ),
    );
  }

  function providerCard(pp) {
    const p = existing(pp);
    const isReady = ready(pp);
    const editing = S.editing.has(pp.id) || !isReady;
    const busy = S.busy.has(pp.id);
    const envKey = S.envKeys.find((k) => k.provider_type === pp.id && k.present);
    const tr = S.tests[pp.id];
    const steps = (pp.steps_keys || []).map((k) => text(k)).filter(Boolean);
    const links = [
      pp.signup_url ? extLink(pp.signup_url, t('onboarding.providers.open_signup'), `ob:prov:${pp.id}:signup`) : null,
      pp.keys_url ? extLink(pp.keys_url, t('onboarding.providers.open_keys'), `ob:prov:${pp.id}:keys`) : null,
      pp.pricing_url ? extLink(pp.pricing_url, t('onboarding.providers.open_pricing'), `ob:prov:${pp.id}:pricing`) : null,
    ].filter(Boolean);
    const inputID = `ob-key-${pp.id}`;
    return h(
      'div',
      { class: 'ob-prov' + (isReady ? ' is-ready' : ''), dataset: { provider: pp.id } },
      h(
        'div',
        { class: 'ob-prov-head' },
        h('span', { class: 'ob-prov-name' }, pp.name),
        pp.free ? h('span', { class: 'chip chip-quiet' }, t('onboarding.providers.free_chip')) : null,
        h('span', { class: 'ob-prov-state' + (isReady ? ' is-ok' : '') }, isReady ? [icon('circle-check', { size: 14 }), t('onboarding.providers.ready', { hint: p.key_hint })] : t('onboarding.providers.not_ready')),
      ),
      pp.free && pp.terms_notice
        ? h('p', { class: 'ob-callout is-info ob-terms' }, icon('info', { size: 14 }), h('span', null, t('onboarding.providers.terms_notice'), ' ', pp.terms_url ? extLink(pp.terms_url, t('onboarding.providers.open_terms'), `ob:prov:${pp.id}:terms`) : null))
        : null,
      h(
        'details',
        { class: 'ob-prov-how', open: !isReady || undefined },
        h('summary', null, icon('chevron-right', { size: 14, class: 'ob-chev' }), t('onboarding.providers.how')),
        steps.length ? h('ol', { class: 'ob-prov-steps' }, steps.map((s) => h('li', null, s))) : null,
        links.length ? h('p', { class: 'ob-prov-links' }, links) : null,
        text(pp.cost_key) ? h('p', { class: 'ob-hint' }, text(pp.cost_key)) : null,
      ),
      editing
        ? h(
            'div',
            { class: 'ob-key' },
            envKey && !isReady ? btn(t('onboarding.providers.use_env', { masked: envKey.masked || '' }), { icon: 'key-round', disabled: busy, fkey: `ob:prov:${pp.id}:env`, title: t('onboarding.providers.use_env_tip', { env: envKey.env }), onclick: () => useEnv(pp) }) : null,
            envKey && !isReady ? h('span', { class: 'ob-or' }, t('onboarding.providers.or')) : null,
            h(
              'form',
              {
                class: 'ob-key-form',
                onsubmit: (e) => {
                  e.preventDefault();
                  saveKey(pp);
                },
              },
              h('label', { class: 'visually-hidden', for: inputID }, t('onboarding.providers.key_label', { name: pp.name })),
              h('input', {
                type: 'password',
                class: 'input ob-key-input',
                id: inputID,
                autocomplete: 'off',
                spellcheck: 'false',
                placeholder: t('onboarding.providers.key_placeholder'),
                value: S.keys[pp.id] || '',
                dataset: { fkey: `ob:prov:${pp.id}:key` },
                oninput: (e) => {
                  S.keys[pp.id] = e.target.value;
                  const b = e.target.form && e.target.form.querySelector('button[type=submit]');
                  if (b) b.disabled = !e.target.value.trim() || busy;
                },
              }),
              h('button', { type: 'submit', class: 'btn', disabled: busy || !(S.keys[pp.id] || '').trim(), dataset: { fkey: `ob:prov:${pp.id}:save` } }, t('onboarding.providers.save_key')),
              isReady ? btn(t('common.cancel'), { kind: 'ghost', onclick: () => (S.editing.delete(pp.id), env.rerender()) }) : null,
            ),
          )
        : h(
            'div',
            { class: 'ob-key' },
            btn(tr && tr.state === 'running' ? t('onboarding.providers.testing') : t('onboarding.providers.test'), { icon: 'plug', disabled: busy || (tr && tr.state === 'running'), fkey: `ob:prov:${pp.id}:test`, onclick: () => test(pp) }),
            btn(t('onboarding.providers.replace_key'), { kind: 'ghost', icon: 'pencil', disabled: busy, fkey: `ob:prov:${pp.id}:replace`, onclick: () => (S.editing.add(pp.id), env.rerender()) }),
          ),
      tr && tr.state !== 'running'
        ? h('p', { class: 'ob-test is-' + tr.state, role: tr.state === 'fail' ? 'alert' : 'status' }, icon(tr.state === 'ok' ? 'circle-check' : 'circle-alert', { size: 14 }), h('span', null, tr.state === 'ok' ? tr.text : t('onboarding.providers.test_failed', { error: tr.text })))
        : null,
    );
  }

  return {
    plan: planOf,
    title: () => t('onboarding.providers.title'),
    desc: () => t('onboarding.providers.desc'),
    body() {
      if (S.loading) return h('div', { class: 'ob-loading' }, icon('loader-circle', { size: 18, class: 'spin' }), h('span', null, t('onboarding.loading')));
      if (S.error) return h('div', { class: 'ob-callout is-warn', role: 'alert' }, icon('circle-alert', { size: 16 }), h('div', null, h('p', null, t('onboarding.providers.load_failed', { error: S.error })), btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', onclick: load })));
      const tier = tierByID(S.tier);
      const provs = tierProviders(tier);
      return [
        tierCards(),
        tier && tier.transcribe_only
          ? h('p', { class: 'ob-callout is-info' }, icon('info', { size: 16 }), h('span', null, t('onboarding.providers.transcribe_only_note')))
          : provs.length
            ? h('div', { class: 'ob-section' }, h('h2', { class: 'ob-h2' }, t('onboarding.providers.keys_title')), h('div', { class: 'ob-provs' }, provs.map(providerCard)))
            : null,
        tier && tier.night_processing ? h('p', { class: 'ob-hint' }, icon('clock', { size: 14 }), ' ', t('onboarding.providers.night_note', { from: NIGHT_TO, to: NIGHT_FROM })) : null,
      ];
    },
    primary() {
      const tier = tierByID(S.tier);
      return {
        label: S.applying ? t('onboarding.providers.applying') : tier && tier.transcribe_only ? t('onboarding.continue') : t('onboarding.providers.apply'),
        icon: 'arrow-right',
        disabled: S.loading || !!S.error || !tier || S.applying,
        onclick: apply,
      };
    },
  };
}
