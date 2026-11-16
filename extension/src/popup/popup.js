// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The toolbar popup (SPEC-ext 界面设计): the connection to LectureFlow in
// the header, the send card (src/ui/sendcard.js, shared with the in-page
// card) in the middle, the report, site access and settings at the foot.
// One click on the icon, one on "发送至 LectureFlow".

import '../vendor/browser-polyfill.js';
import { el, icon, t, localize } from '../ui/dom.js';
import { applyChosenLanguage } from '../ui/language.js';
import { createSendCard, fitSend } from '../ui/sendcard.js';

const browser = globalThis.browser;
const $ = (id) => document.getElementById(id);

// ------------------------------------------------------------ port to bg

const port = browser.runtime.connect({ name: 'popup' });
const waiting = new Map();
let seq = 0;
let card = null;
let tabId = null;
port.onMessage.addListener((msg) => {
  if (msg && msg.push === 'status') {
    if (card) card.onStatus(msg.status);
    return;
  }
  const w = msg && waiting.get(msg.id);
  if (!w) return;
  waiting.delete(msg.id);
  w(msg);
});
function ask(cmd, extra = {}) {
  const id = ++seq;
  return new Promise((resolve) => {
    waiting.set(id, resolve);
    port.postMessage({ id, cmd, tabId, ...extra });
  });
}

async function currentTabId() {
  const q = new URLSearchParams(location.search).get('tabId');
  if (q && /^\d+$/.test(q)) return Number(q);
  const [tab] = await browser.tabs.query({ active: true, currentWindow: true });
  return tab ? tab.id : null;
}

function note(text, tone = '') {
  const n = $('foot-msg');
  n.className = `note${tone ? ' note-' + tone : ''}`;
  n.textContent = text;
  n.hidden = !text;
}

// ---------------------------------------------------- header and footer

function renderConnection(c) {
  const box = $('conn');
  const chip = $('conn-chip');
  const conn = c || { status: 'checking' };
  chip.replaceChildren();
  box.dataset.nmState = conn.status;
  if (conn.status === 'connected') {
    chip.className = 'chip chip-ok';
    chip.append(icon('plug-connected'), el('span', { text: conn.appVersion ? t('connConnectedVersion', [conn.appVersion]) : t('connConnected') }));
  } else if (conn.status === 'checking') {
    chip.className = 'chip chip-run';
    chip.append(el('span', { text: t('connChecking') }));
  } else {
    chip.className = 'chip chip-idle';
    chip.append(icon('plug-connected-x'), el('span', { text: t('connUnavailable') }));
  }
}

function onView(v, S) {
  document.body.dataset.view = v.view;
  document.body.dataset.reason = v.reason || '';
  renderConnection(S.conn);
  const d = S.detection;
  $('export').disabled = !(d && d.status === 'ok');
  const ok = d && d.status === 'ok';
  $('grant').hidden = !(ok && ((!d.authorized && S.origins.length > 0) || (S.origins.length > 0 && (S.cookieHosts || []).length > 0)));
  fitPopup();
}

/** The send block keeps in sight in a popup tall enough for it (the viewport scrolls; SEC42 中-1). */
function fitPopup() {
  fitSend(document.documentElement, window.innerHeight, document);
}

function grantSite(origins) {
  const wanted = Array.isArray(origins) ? origins : card.state.origins;
  // permissions.request must run inside the click, before any await.
  browser.permissions.request({ origins: wanted }).then(
    (granted) => {
      if (!granted) return;
      $('grant-panel').hidden = true;
      note(t('grantDone'), 'ok');
      card.redetect();
    },
    () => note(t('grantFailed'), 'bad'),
  );
}

/**
 * "在此网站自动探测" (security review L4): the page's own site. When a send
 * would read cookies of tracks on other hosts, those are listed apart, each
 * unticked, and asked for only when the user ticks them.
 */
function onGrantClick() {
  const hosts = card.state.cookieHosts || [];
  if (hosts.length === 0) {
    grantSite();
    return;
  }
  const panel = $('grant-panel');
  if (!panel.hidden) {
    panel.hidden = true;
    return;
  }
  $('grant-hosts').replaceChildren(
    ...hosts.map((o) => {
      const box = el('input', { type: 'checkbox', dataset: { origin: o } });
      return el('label', { class: 'check-line' }, box, el('span', { text: o.replace(/^https:\/\//, '').replace(/\/\*$/, '') }));
    }),
  );
  panel.hidden = false;
}

function onGrantAllow() {
  const extra = [...document.querySelectorAll('#grant-hosts input:checked')].map((b) => b.dataset.origin);
  grantSite([...card.state.origins, ...extra]);
}

async function exportReport() {
  $('export').disabled = true;
  note(t('reportBuilding'));
  try {
    const r = await ask('report');
    if (!r.ok) {
      if (r.reason === 'personal_data') {
        const kinds = [...new Set((r.findings || []).map((f) => f.kind))];
        note(`${t('reportRefused')} ${kinds.map((k) => t(`finding_${k}`)).join(' ')}`, 'bad');
      } else note(t('reportFailed'), 'bad');
      return;
    }
    const blob = new Blob([JSON.stringify(r.report, null, 2) + '\n'], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: r.fileName });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    note(t('reportSaved'), 'ok');
  } finally {
    $('export').disabled = false;
  }
}

async function main() {
  await applyChosenLanguage();
  localize();
  $('export').addEventListener('click', exportReport);
  $('grant').addEventListener('click', onGrantClick);
  $('grant-allow').addEventListener('click', onGrantAllow);
  $('options').addEventListener('click', () => browser.runtime.openOptionsPage());
  // Chrome grows the popup with its content, up to 600 px
  window.addEventListener('resize', fitPopup);
  tabId = await currentTabId();
  card = createSendCard({
    mount: $('main'),
    host: 'popup',
    ask: tabId === null ? async (cmd) => (cmd === 'detect' ? { ok: true, view: { status: 'no_access' } } : ask(cmd)) : ask,
    onView,
    onGrant: grantSite,
    openUrl: (url) => browser.tabs.create({ url }),
  });
  await card.start();
}

main();
