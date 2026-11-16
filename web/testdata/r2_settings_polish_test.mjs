// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 2 (W3), the logic run in Node (web/r2_settings_polish_test.go
// starts it with the web directory as a file: URL): the Colab accelerators
// locked before Colab is authorized say so (T6 #11), the reason on a line
// of its own starts with a capital, and the provider's tariff summary keeps
// the zone apart from the day names (T6 #12). settings_colab.js and
// tariff_ui.js are imported as the page imports them, with the few browser
// globals their imports read at load stubbed; t() answers the keys.

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
const T = await import(new URL('tariff_ui.js', process.argv[2]).href);

// ---- T6 #11: not authorized is not "the account could not be read" ----
{
  const unknown = { read_at: null, read_ok: false, level: 'free', paid_balance_cu: null, eligible_gpus: [], ineligible_gpus: [], eligibility_known: false };
  const st = (auth) => ({ account: unknown, selectable_accelerators: ['T4'], auth });
  for (const x of ['L4', 'A100', 'G4', 'H100']) {
    assert.equal(C.lockOf(st({ present: false }), x), 'need_auth', `${x}: not authorized`);
    assert.equal(C.lockOf(st({ present: true }), x), 'account_unknown', `${x}: authorized, the account not read`);
    assert.equal(C.lockOf(st(undefined), x), 'account_unknown', `${x}: a status without auth`);
  }
  assert.equal(C.lockOf(st({ present: false }), 'T4'), '', 'the T4 is never locked');
  // a known account: the auth does not matter
  const paid = { read_ok: true, level: 'paid', paid_balance_cu: 10, eligibility_known: true };
  assert.equal(C.lockOf({ account: paid, selectable_accelerators: ['T4'], auth: { present: false } }, 'L4'), 'ineligible');
  // the reason on a line of its own: first letter raised, the rest kept
  // (no catalog is loaded here, so lockText answers the reason itself)
  assert.equal(C.lockText('need_auth'), 'need_auth');
  assert.equal(C.lockLine('need_auth'), 'Need_auth');
  assert.equal(C.lockLine('ineligible'), 'Ineligible');
  assert.equal(C.lockLine(''), '');
}

// ---- T6 #12: the zone is a part of its own ----
{
  const tariff = {
    timezone: 'Asia/Shanghai',
    peak_windows: [
      { days: [1, 2, 3, 4, 5], from: '09:00', to: '12:00' },
      { days: [1, 2, 3, 4, 5], from: '14:00', to: '18:00' },
    ],
    offpeak_multiplier: 0.5,
    exclude_holidays: 'CN',
  };
  // (W4: the line starts with a capital, here the key's first letter;
  // the real catalogs are checked in w4_text_test.mjs)
  const s = T.tariffSummary(tariff);
  const parts = s.split(' · ');
  assert.equal(parts[0], 'Tariff.zone.beijing', s);
  assert.ok(parts[1].startsWith('tariff.days.weekdays 09:00–12:00'), s);
  assert.ok(!s.includes('Tariff.zone.beijing tariff.days'), s);
  assert.equal(parts.length, 4, s);
  assert.equal(T.tariffSummary({ timezone: 'Asia/Shanghai', peak_windows: [] }), 'Tariff.summary.no_windows');
}

console.log('r2 settings polish checks: ok');
