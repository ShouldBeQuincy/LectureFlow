// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Settings: profiles (packaged and locally imported, the latter marked
// "未审核", with the sites each one needs; a local file with a packaged
// profile's id replaces it only after the user confirms), the sites the
// user has authorized, the trusted embedding sites (src/lib/embed.js; each
// can be removed), the platforms whose restricted reminder the user turned
// off (each can be undone), and exporting the redacted detection report of
// a page.

import '../vendor/browser-polyfill.js';
import { el, icon, t, localize } from '../ui/dom.js';
import { applyChosenLanguage } from '../ui/language.js';
import { checkProfile, profilePermissionOrigins } from '../lib/profile.js';
import { requestableOrigin, parseHttpUrl } from '../lib/url.js';
import { hostCategory } from '../lib/blocklist.js';
import { blockedText } from '../ui/sendcard.js';
import { UI_LANGUAGES, LANGUAGE_KEY, reportRowKey } from '../lib/i18n.js';
import { RESTRICTED_KEY, rememberedList, withoutRemembered } from '../lib/remember.js';
import { TRUST_KEY, trustedList, withoutTrusted } from '../lib/embed.js';
import { localEntry, withLocalEntry, confirmedReplacement } from '../lib/localprofiles.js';
import { HEALTH_KEY, healthKey, isStopped } from '../lib/health.js';

const browser = globalThis.browser;
const $ = (id) => document.getElementById(id);
const LOCAL_KEY = 'localProfiles';

// The port to the background, for the reports.
const port = browser.runtime.connect({ name: 'options' });
const waiting = new Map();
let seq = 0;
port.onMessage.addListener((msg) => {
  const w = msg && waiting.get(msg.id);
  if (!w) return;
  waiting.delete(msg.id);
  w(msg);
});
function ask(cmd, extra = {}) {
  const id = ++seq;
  return new Promise((resolve) => {
    waiting.set(id, resolve);
    port.postMessage({ id, cmd, ...extra });
  });
}

function note(id, text, tone = '') {
  const n = $(id);
  n.className = `note${tone ? ' note-' + tone : ''}`;
  n.textContent = text;
  n.hidden = !text;
}

async function schema() {
  return (await fetch(browser.runtime.getURL('schema/profile.schema.json'))).json();
}

async function bundled() {
  try {
    const index = await (await fetch(browser.runtime.getURL('profiles/index.json'))).json();
    const out = [];
    for (const f of index.profiles || []) out.push(await (await fetch(browser.runtime.getURL(`profiles/${f}`))).json());
    return out;
  } catch {
    return [];
  }
}

async function local() {
  const v = (await browser.storage.local.get(LOCAL_KEY))[LOCAL_KEY];
  return Array.isArray(v) ? v : [];
}

function hostsLine(profile) {
  const hosts = [...profile.match.hosts];
  const shown = hosts.slice(0, 4).join(', ');
  return t('profileHosts', [hosts.length > 4 ? `${shown} …` : shown]);
}

/** "Allow these sites" for a profile, or a note when they are granted. */
async function grantControl(profile) {
  // the pages' hosts; media hosts are asked for one by one in the popup, when cookies are needed
  const origins = profilePermissionOrigins(profile, { media: false });
  let granted = false;
  try {
    granted = await browser.permissions.contains({ origins });
  } catch {
    granted = false;
  }
  if (granted) return el('span', { class: 'chip chip-ok', text: t('profileGranted') });
  return el('button', {
    class: 'btn btn-sm',
    dataset: { grant: profile.id },
    onclick: () => {
      // Inside the click, before any await.
      browser.permissions.request({ origins }).then(
        () => renderAll(),
        () => note('import-msg', t('grantFailed'), 'bad'),
      );
    },
  }, el('span', { text: t('profileGrant') }));
}

// Profiles stopped as too slow (src/lib/health.js): their records.
async function health() {
  const h = (await browser.storage.local.get(HEALTH_KEY))[HEALTH_KEY];
  return h && typeof h === 'object' ? h : {};
}

/** Clears the record of a profile entry; the background tries it again. */
async function clearHealth(entry) {
  const h = await health();
  const key = healthKey(entry);
  if (!(key in h)) return;
  delete h[key];
  await browser.storage.local.set({ [HEALTH_KEY]: h });
  try {
    await browser.runtime.sendMessage({ cmd: 'regexReset' });
  } catch {
    // the background starts afresh anyway
  }
}

async function profileRow(profile, { reviewed, removable, entry = null, packaged = false, problems = [], slow = null }) {
  const status = reviewed ? el('span', { class: 'chip chip-ok', text: t('reviewed') }) : el('span', { class: 'chip chip-idle', text: t('unreviewed') });
  // a local file with a packaged profile's id: in force only when confirmed on import
  let replaces = null;
  if (entry && problems.length) {
    // not in use (src/background/profiles.js skips it), whatever it replaces;
    // the packaged profile of that id stays in force (SEC18 低-3)
    const first = problems[0];
    replaces = el('span', { class: 'chip chip-warn', dataset: { inactive: 'true', invalid: 'true' }, text: t('profileInvalid', [`${first.path} ${first.message}`]) });
  } else if (slow) {
    // stopped by the runtime regex guard (src/lib/regexguard.js): a local
    // file until it is imported again, a packaged one until enabled here
    // or the extension is updated
    replaces = el('span', { class: 'chip chip-warn', dataset: { inactive: 'true', slow: 'true' }, text: t(reviewed ? 'profileTooSlowBuiltin' : 'profileTooSlow', [slow.rule, String(slow.ms)]) });
  } else if (entry && packaged) {
    replaces = confirmedReplacement(entry)
      ? el('span', { class: 'small faint', dataset: { replaces: entry.replaces }, text: t('profileReplaces', [entry.replaces]) })
      : el('span', { class: 'chip chip-warn', dataset: { inactive: 'true' }, text: t('profileInactive') });
  }
  const actions = el('div', { class: 'row-actions' }, await grantControl(profile));
  if (slow && reviewed) {
    actions.append(
      el('button', {
        class: 'btn btn-sm',
        dataset: { reenable: profile.id },
        onclick: async () => {
          await clearHealth({ profile, source: 'bundled' });
          renderProfiles();
        },
      }, el('span', { text: t('profileReenable') })),
    );
  }
  if (removable) {
    actions.append(
      el('button', {
        class: 'btn btn-sm btn-ghost',
        dataset: { remove: profile.id },
        onclick: async () => {
          const rest = (await local()).filter((x) => x.profile.id !== profile.id);
          await browser.storage.local.set({ [LOCAL_KEY]: rest });
          await clearHealth({ profile, source: 'local' });
          renderProfiles();
        },
      }, icon('trash'), el('span', { text: t('remove') })),
    );
  }
  return el(
    'li',
    { class: 'row', dataset: { profile: profile.id, reviewed: String(reviewed) } },
    el('div', { class: 'row-stack' }, el('div', { class: 'row-main' }, el('span', { class: 'row-title', text: `${profile.name} ${profile.version}` }), status), el('span', { class: 'small faint', text: hostsLine(profile) }), replaces),
    actions,
  );
}

async function renderProfiles() {
  const rows = [];
  const packaged = await bundled();
  const ids = new Set(packaged.map((p) => p.id));
  const sch = await schema();
  const h = await health();
  const version = browser.runtime.getManifest().version;
  const slowOf = (entry) => (isStopped(h[healthKey(entry)], entry, version) ? h[healthKey(entry)] : null);
  for (const e of await local()) {
    if (!e || !e.profile) continue;
    // the same check as loading them (src/background/profiles.js)
    let problems;
    try {
      problems = checkProfile(sch, e.profile, { unreviewed: true });
    } catch {
      problems = [{ path: '', message: 'unreadable' }];
    }
    const slow = slowOf({ profile: e.profile, source: 'local' });
    rows.push(await profileRow(e.profile, { reviewed: false, removable: true, entry: e, packaged: ids.has(e.profile.id), problems, slow }));
  }
  for (const p of packaged) rows.push(await profileRow(p, { reviewed: true, removable: false, slow: slowOf({ profile: p, source: 'bundled' }) }));
  $('profiles').replaceChildren(...rows);
}

async function storeLocal(entry) {
  await browser.storage.local.set({ [LOCAL_KEY]: withLocalEntry(await local(), entry) });
  // a file imported again is tried again (src/lib/health.js)
  await clearHealth({ profile: entry.profile, source: 'local' });
  note('import-msg', t('importDone', [entry.profile.name]), 'ok');
  renderProfiles();
}

/**
 * A file with a packaged profile's id (security review L7): it replaces the
 * packaged, reviewed one only when the user says so here; it is marked
 * "未审核" like every local file.
 */
function askReplace(profile, builtin) {
  const box = $('import-confirm');
  const close = () => {
    box.hidden = true;
    box.replaceChildren();
  };
  box.replaceChildren(
    el('p', { id: 'import-confirm-text', text: t('importReplaceConfirm', [builtin.name, builtin.version]) }),
    el(
      'div',
      { class: 'row-actions' },
      el('button', { class: 'btn btn-sm', id: 'import-cancel', type: 'button', onclick: close }, el('span', { text: t('cancel') })),
      el('button', {
        class: 'btn btn-sm btn-warn',
        id: 'import-replace',
        type: 'button',
        onclick: async () => {
          close();
          await storeLocal(localEntry(profile, { replaces: builtin.version }));
        },
      }, el('span', { text: t('importReplace') })),
    ),
  );
  box.hidden = false;
}

async function importFile(file) {
  note('import-msg', '');
  $('import-confirm').hidden = true;
  let profile;
  try {
    profile = JSON.parse(await file.text());
  } catch {
    note('import-msg', t('importNotJson'), 'bad');
    return;
  }
  const errors = checkProfile(await schema(), profile, { unreviewed: true });
  // A profile for a do-not-handle site (src/lib/blocklist.js): said plainly.
  const blocked = errors.find((e) => e.blocked);
  if (blocked) {
    const first = t('importBlocked', [blocked.blocked.host]);
    // no space after a CJK full stop
    note('import-msg', `${first}${/[。！？：]$/.test(first) ? '' : ' '}${blockedText(blocked.blocked.category)}`, 'bad');
    return;
  }
  if (errors.length) {
    note('import-msg', `${t('importInvalid')} ${errors.slice(0, 5).map((e) => `${e.path} ${e.message}`).join('; ')}`, 'bad');
    return;
  }
  const builtin = (await bundled()).find((p) => p.id === profile.id);
  if (builtin) {
    askReplace(profile, builtin);
    return;
  }
  await storeLocal(localEntry(profile));
}

async function renderSites() {
  const all = await browser.permissions.getAll();
  const rows = (all.origins || [])
    .filter((x) => /^(\*|https?):\/\//.test(x))
    .map((o) =>
      el(
        'li',
        { class: 'row', dataset: { origin: o } },
        el('div', { class: 'row-main' }, icon('world'), el('span', { text: o })),
        el('button', {
          class: 'btn btn-sm btn-ghost',
          onclick: async () => {
            await browser.permissions.remove({ origins: [o] }).catch(() => false);
            renderAll();
          },
        }, el('span', { text: t('remove') })),
      ),
    );
  $('sites').replaceChildren(...rows);
}

function addSite(ev) {
  ev.preventDefault();
  const raw = $('site-input').value.trim();
  const u = parseHttpUrl(raw);
  const category = u ? hostCategory(u.hostname) : '';
  if (category) {
    note('site-msg', blockedText(category), 'bad');
    return;
  }
  const pattern = requestableOrigin(raw);
  if (!pattern) {
    note('site-msg', t('siteInvalid'), 'bad');
    return;
  }
  // Inside the submit event, before any await.
  browser.permissions.request({ origins: [pattern] }).then(
    (granted) => {
      note('site-msg', granted ? t('grantDone') : '', granted ? 'ok' : '');
      renderAll();
    },
    () => note('site-msg', t('grantFailed'), 'bad'),
  );
}

/**
 * "对这个平台不再提醒" (src/lib/remember.js): the platforms, by their
 * profiles' names, and "恢复提醒" for each, after which every send of a
 * possibly restricted recording reminds again.
 */
async function renderRestricted() {
  const names = {};
  for (const p of await bundled()) names[p.id] = p.name;
  for (const e of await local()) if (e && e.profile && !names[e.profile.id]) names[e.profile.id] = e.profile.name;
  const raw = (await browser.storage.local.get(RESTRICTED_KEY))[RESTRICTED_KEY];
  const list = rememberedList(raw, names);
  $('restricted-empty').hidden = list.length > 0;
  $('restricted').replaceChildren(
    ...list.map((x) =>
      el(
        'li',
        { class: 'row', dataset: { platform: x.id } },
        el('div', { class: 'row-stack' }, el('div', { class: 'row-main' }, el('span', { class: 'row-title', text: x.name })), el('span', { class: 'small faint', text: t('restrictedSince', [x.at.slice(0, 10)]) })),
        el('button', {
          class: 'btn btn-sm',
          dataset: { forget: x.id },
          onclick: async () => {
            const current = (await browser.storage.local.get(RESTRICTED_KEY))[RESTRICTED_KEY];
            const rest = withoutRemembered(current, x.id);
            if (Object.keys(rest).length) await browser.storage.local.set({ [RESTRICTED_KEY]: rest });
            else await browser.storage.local.remove(RESTRICTED_KEY);
            renderRestricted();
          },
        }, el('span', { text: t('restrictedForget') })),
      ),
    ),
  );
}

/**
 * The trusted embedding sites (src/lib/embed.js): the platform's in-page
 * buttons confirm on the page when it is embedded in one of these; each
 * can be removed (the extension's window confirms there again).
 */
async function renderTrusted() {
  const list = trustedList((await browser.storage.local.get(TRUST_KEY))[TRUST_KEY]);
  $('trusted-empty').hidden = list.length > 0;
  $('trusted').replaceChildren(
    ...list.map((x) =>
      el(
        'li',
        { class: 'row', dataset: { trusted: x.origin } },
        el('div', { class: 'row-stack' }, el('div', { class: 'row-main' }, icon('world'), el('span', { class: 'row-title', text: x.origin })), el('span', { class: 'small faint', text: t('trustedSince', [x.at.slice(0, 10)]) })),
        el('button', {
          class: 'btn btn-sm btn-ghost',
          dataset: { untrust: x.origin },
          onclick: async () => {
            const rest = withoutTrusted((await browser.storage.local.get(TRUST_KEY))[TRUST_KEY], x.origin);
            if (Object.keys(rest).length) await browser.storage.local.set({ [TRUST_KEY]: rest });
            else await browser.storage.local.remove(TRUST_KEY);
            renderTrusted();
          },
        }, el('span', { text: t('remove') })),
      ),
    ),
  );
}

async function renderReports() {
  const r = await ask('reportTabs');
  const tabs = r.ok ? r.tabs : [];
  $('reports-empty').hidden = tabs.length > 0;
  $('reports').replaceChildren(
    ...tabs.map((tab) =>
      el(
        'li',
        { class: 'row', dataset: { tabId: String(tab.tabId) } },
        el('div', { class: 'row-main' }, icon('world'), el('span', { text: t(reportRowKey(tab.total), [tab.host, String(tab.total)]) }), el('span', { class: 'chip chip-outline', text: tab.profile || t('sourceGeneric') })),
        el('button', { class: 'btn btn-sm', onclick: () => exportReport(tab.tabId) }, icon('download'), el('span', { text: t('exportOne') })),
      ),
    ),
  );
}

async function exportReport(tabId) {
  note('report-msg', t('reportBuilding'));
  const r = await ask('report', { tabId });
  if (!r.ok) {
    if (r.reason === 'personal_data') {
      const kinds = [...new Set((r.findings || []).map((f) => f.kind))];
      note('report-msg', `${t('reportRefused')} ${kinds.map((k) => t(`finding_${k}`)).join(' ')}`, 'bad');
    } else note('report-msg', t('reportFailed'), 'bad');
    return;
  }
  const blob = new Blob([JSON.stringify(r.report, null, 2) + '\n'], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = el('a', { href: url, download: r.fileName });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
  note('report-msg', t('reportSaved'), 'ok');
}

function renderAll() {
  return Promise.all([renderProfiles(), renderSites(), renderTrusted(), renderRestricted(), renderReports()]);
}

/**
 * The interface language: the browser's (default) or one of the six, each
 * named in its own language as in LectureFlow. Applies at once: this page
 * reloads; the popup and the in-page buttons use it the next time they open.
 */
function renderLanguage(current) {
  const sel = $('language');
  sel.replaceChildren(
    el('option', { value: '', text: t('languageAuto') }),
    ...UI_LANGUAGES.map((l) => el('option', { value: l.code, lang: l.bcp47, text: l.name })),
  );
  sel.value = current;
  sel.addEventListener('change', async () => {
    if (sel.value) await browser.storage.local.set({ [LANGUAGE_KEY]: sel.value });
    else await browser.storage.local.remove(LANGUAGE_KEY);
    location.reload();
  });
}

async function main() {
  const chosen = await applyChosenLanguage();
  localize();
  renderLanguage(chosen);
  $('version').textContent = t('versionLine', [browser.runtime.getManifest().version]);
  $('import-file').addEventListener('change', (ev) => {
    const f = ev.target.files && ev.target.files[0];
    if (f) importFile(f);
    ev.target.value = '';
  });
  $('add-site').addEventListener('submit', addSite);
  browser.permissions.onAdded.addListener(renderAll);
  browser.permissions.onRemoved.addListener(renderAll);
  // A card may turn a platform's reminder off while this page is open.
  browser.storage.onChanged.addListener((changes, area) => {
    if (area === 'local' && changes[RESTRICTED_KEY]) renderRestricted();
    if (area === 'local' && changes[TRUST_KEY]) renderTrusted();
  });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') renderReports();
  });
  await renderAll();
}

main();
