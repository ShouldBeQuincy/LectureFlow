// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The confirmation window (security review M4; src/lib/embed.js): a
// platform's in-page button inside another site's page that the user has
// not trusted opens this small extension window instead of a card in the
// page — a page can neither cover it nor make it transparent. It shows
// where the button sits (the recording page's host, the site it is
// embedded in), the same send card or bulk card as the page would (the
// detection of that frame, the lecture or lectures to send), and offers to
// trust that site ("以后在 <站点> 里直接在页面上确认"). A trusted site's frame
// that has no room for the card (a small embedded player) opens it too,
// without that note and offer. Its commands go to the
// background with the window's ticket; the background acts only on what
// the ticket names. The confirming controls act only once they have been
// shown for a moment (src/ui/clickguard.js), so a double click on the page
// cannot land on them as the window opens.

import '../vendor/browser-polyfill.js';
import { el, t, localize } from '../ui/dom.js';
import { applyChosenLanguage } from '../ui/language.js';
import { createSendCard, fitSend } from '../ui/sendcard.js';
import { createBulkCard } from '../ui/bulkcard.js';
import { guardClicks } from '../ui/clickguard.js';
import { windowNotice } from '../lib/embed.js';

const browser = globalThis.browser;
const $ = (id) => document.getElementById(id);
const ticket = new URLSearchParams(location.search).get('ticket') || '';

// ------------------------------------------------------------ port to bg

const port = browser.runtime.connect({ name: 'confirm' });
const waiting = new Map();
let seq = 0;
let card = null;
port.onMessage.addListener((msg) => {
  if (msg && msg.push === 'status') {
    if (card && card.onStatus) card.onStatus(msg.status);
    return;
  }
  const w = msg && waiting.get(msg.id);
  if (!w) return;
  waiting.delete(msg.id);
  w(msg);
});
port.onDisconnect.addListener(() => {
  for (const w of waiting.values()) w({ ok: false, error: { code: 'ext.closed', params: {} } });
  waiting.clear();
});
function ask(cmd, extra = {}) {
  const id = ++seq;
  return new Promise((resolve) => {
    waiting.set(id, resolve);
    try {
      port.postMessage({ ...extra, id, cmd, ticket });
    } catch {
      waiting.delete(id);
      resolve({ ok: false, error: { code: 'ext.closed', params: {} } });
    }
  });
}

async function closeWindow() {
  try {
    const w = await browser.windows.getCurrent();
    await browser.windows.remove(w.id);
  } catch {
    window.close();
  }
}

// ------------------------------------------------------------- the page

function showEmbed(view) {
  const host = view.frameHost || '';
  // A trusted site's player with no room for the card: no click-jacking
  // note and no trust offer, only where the recording page is.
  const notice = windowNotice(view);
  document.body.dataset.notice = notice.warn ? 'warn' : 'plain';
  if (notice.warn) $('embed-why').textContent = view.site ? t('embedWhy', [view.site]) : t('embedWhyUnknown');
  else $('embed-why').hidden = true;
  $('embed-from').textContent = t('embedFrom', [host]);
  if (notice.offerTrust) {
    $('trust-text').textContent = t('embedTrust', [view.site]);
    const box = $('trust');
    box.checked = !!view.trusted;
    box.addEventListener('change', async () => {
      const r = await ask('trust', { on: box.checked });
      if (!r.ok) box.checked = !box.checked;
    });
    $('trust-line').hidden = false;
  }
  $('embed').hidden = false;
}

function gone() {
  document.body.dataset.view = 'gone';
  $('main').replaceChildren(el('section', { class: 'card state' }, el('p', { class: 'muted', id: 'state-title', text: t('confirmGone') })));
}

async function main() {
  await applyChosenLanguage();
  localize();
  // Only after a moment can anything here be confirmed (a double click on
  // the page must not land on this window as it opens).
  guardClicks(document.body);
  const r = await ask('ticket');
  if (!r.ok || !r.ticket) {
    gone();
    return;
  }
  const view = r.ticket;
  document.body.dataset.flow = view.flow;
  $('cf-title').textContent = t(view.flow === 'bulk' ? 'bulkTitle' : 'sendButton');
  showEmbed(view);
  const mount = $('main');
  if (view.flow === 'bulk') {
    card = createBulkCard({
      mount,
      ask,
      rows: view.rows,
      origin: view.origin,
      fetchInfo: (sessionId) => ask('fetchInfo', { sessionId }),
      presets: view.presets,
      platformName: view.platform.name,
      onClose: closeWindow,
    });
    document.body.dataset.view = 'bulk';
    await card.start();
    return;
  }
  // the send block keeps in sight in a window tall enough for it (the
  // viewport scrolls; SEC42 中-1)
  const fit = () => fitSend(document.documentElement, window.innerHeight, document);
  window.addEventListener('resize', fit);
  card = createSendCard({
    mount,
    ask,
    host: 'window',
    onView: (v) => {
      document.body.dataset.view = v.view;
      document.body.dataset.reason = v.reason || '';
      fit();
    },
    openUrl: (url) => browser.tabs.create({ url }),
  });
  await card.start();
}

main();
