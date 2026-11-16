// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The in-page entry points (user request 2026-10-27; src/lib/inpage.js has
// the rules). Registered only on authorized sites of a profile with
// `inpage` (src/background/sites.js), in every frame (a list or player
// embedded in an LMS). It asks the background for this frame's
// configuration and then:
//   player: a "发送至 LectureFlow" button where the profile says; it opens
//     the send card (src/ui/sendcard.js) for this page.
//   list:   per session row a check box before its title and the button
//     after it (or the row's state: 已导入 / 下载中 42% / 需要刷新); a
//     button for the whole page; a bar for the ticked rows; both open the
//     bulk card (src/ui/bulkcard.js).
// In a frame inside another site's page (an LMS, or a page trying to
// clickjack the buttons), where the card confirms is the background's call
// (src/lib/embed.js): the top-level site comes from the tab's address or
// from location.ancestorOrigins, read here; a trusted site keeps the card in
// the page, armed only after a short delay and (Chrome) while really
// visible (src/ui/clickguard.js); any other site gets the extension's own
// confirmation window (src/confirm/), and this frame only answers that
// window's sessionInfo requests for rows it lists. A frame of a trusted
// site with no room for the card (a small embedded player) gets the window
// too (src/lib/embed.js cardFits).
// Everything it draws lives in closed shadow roots (styles from the
// design tokens, nothing leaks either way) and swallows its own events so
// the page's handlers never see them. The platform's sessionInfo request
// is made here, from the page's own origin, only for a session the user
// clicked to send; only the declared values of its answer go on.
//
// An ES module in the source tree; scripts/build.mjs bundles it into a
// classic script (content scripts cannot be modules).

import { el, icon, t, useCatalog, uiLang } from '../ui/dom.js';
import { createSendCard, fitSend } from '../ui/sendcard.js';
import { createBulkCard } from '../ui/bulkcard.js';
import { rowSessionId, sessionInfoRequest, extractPointers, rowState } from '../lib/inpage.js';
import { progressOf } from '../lib/popupstate.js';
import { guardClicks, watchVisibility } from '../ui/clickguard.js';
import { cardFits, cardPlacement } from '../lib/embed.js';
import { INPAGE_CSS } from '../ui/inpage-css.js';
import { BUILD } from '../build-info.js';
import { setRegexGuard, PAGE_WINDOW_MS } from '../lib/regexguard.js';

const api = globalThis.browser || globalThis.chrome;
const MAX_INFO = 4 * 1024 * 1024;
// Events that stay with our elements: the page's own handlers (a row that
// opens the player when clicked, keyboard shortcuts) never see them.
const OWN_EVENTS = ['click', 'dblclick', 'auxclick', 'mousedown', 'mouseup', 'pointerdown', 'pointerup', 'touchstart', 'touchend', 'keydown', 'keyup', 'keypress', 'input', 'change', 'submit', 'contextmenu'];

main();

async function main() {
  if (globalThis.__lectureflowInpage) return;
  globalThis.__lectureflowInpage = true;
  let cfg = null;
  try {
    // the origins this frame is embedded in: the background checks that
    // none is a page LectureFlow does not handle (src/lib/blocklist.js)
    cfg = await api.runtime.sendMessage({ cmd: 'inpageConfig', ancestors: window.top !== window ? ancestorOrigins() : null });
  } catch {
    return;
  }
  if (!cfg || (cfg.mode !== 'player' && cfg.mode !== 'list')) return;
  // a profile regex too slow here (src/lib/regexguard.js): it stops in this
  // page at once; the background records it (src/background/health.js)
  setRegexGuard({
    windowMs: PAGE_WINDOW_MS,
    onTrip: (tr) => {
      try {
        Promise.resolve(api.runtime.sendMessage({ cmd: 'regexSlow', owner: tr.owner, source: tr.source, ms: tr.ms, reason: tr.reason })).catch(() => {});
      } catch {
        // the background is gone; the page stays unblocked anyway
      }
    },
  });
  // the language chosen in the extension's settings, if any
  if (cfg.i18n && cfg.i18n.messages && typeof cfg.i18n.lang === 'string') useCatalog(cfg.i18n);
  const ui = createInpage(cfg);
  if (cfg.mode === 'player') ui.player();
  else ui.list();
}

/** location.ancestorOrigins as a list (nearest first, the top-level page last); null where there is none. */
function ancestorOrigins() {
  try {
    const list = globalThis.location.ancestorOrigins;
    return list ? [...list].slice(0, 64).map(String) : null;
  } catch {
    return null;
  }
}

function createInpage(cfg) {
  const lang = uiLang();
  // Whether this frame is inside another page, and that page's origins as
  // the browser reports them (the background decides what they mean).
  const sub = window.top !== window;
  const ancestors = sub ? ancestorOrigins() : null;

  // --------------------------------------------------- port to the bg

  let port = null;
  let seq = 0;
  const waiting = new Map();
  let card = null;
  const statusHandlers = new Set();

  function connectPort() {
    port = api.runtime.connect({ name: 'inpage' });
    port.onMessage.addListener((msg) => {
      if (msg && msg.push === 'status') {
        for (const h of statusHandlers) h(msg.status);
        if (card && card.ctl && card.ctl.onStatus) card.ctl.onStatus(msg.status);
        return;
      }
      const w = msg && waiting.get(msg.id);
      if (!w) return;
      waiting.delete(msg.id);
      w(msg);
    });
    port.onDisconnect.addListener(() => {
      port = null;
      for (const w of waiting.values()) w({ ok: false, error: { code: 'ext.closed', params: {} } });
      waiting.clear();
    });
  }

  function request(cmd, extra = {}) {
    if (!port) connectPort();
    const id = ++seq;
    return new Promise((resolve) => {
      waiting.set(id, resolve);
      try {
        port.postMessage({ id, cmd, ...extra, ancestors });
      } catch {
        waiting.delete(id);
        resolve({ ok: false, error: { code: 'ext.closed', params: {} } });
      }
    });
  }

  const askFor = (source) => (cmd, extra = {}) => request(cmd, { source, ...extra });

  /** The platform's sessionInfo for one session, from this page's origin. */
  async function fetchInfo(sessionId) {
    const req = sessionInfoRequest(cfg.sessionInfo, sessionId, location.href);
    if (!req) return { ok: false, status: 0 };
    let res;
    try {
      res = await fetch(req.url, { method: req.method, headers: req.headers, body: req.body, credentials: 'same-origin', cache: 'no-store', redirect: 'error' });
    } catch {
      return { ok: false, status: 0 };
    }
    if (!res.ok) return { ok: false, status: res.status };
    const text = await res.text();
    if (text.length > MAX_INFO) return { ok: false, status: res.status };
    let obj;
    try {
      obj = JSON.parse(text);
    } catch {
      return { ok: false, status: res.status };
    }
    // Only the declared values; the answer itself goes no further.
    return { ok: true, values: extractPointers(obj, cfg.pointers), url: req.url };
  }

  // The extension's confirmation window asks this frame for a listed
  // session's sessionInfo (only rows this page lists), and says when it
  // closes (the rows are checked again then).
  const listed = new Set();
  api.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    if (!msg || typeof msg !== 'object' || (sender && sender.id && sender.id !== api.runtime.id)) return undefined;
    if (msg.cmd === 'lf.sessionInfo') {
      const sid = String(msg.sessionId || '');
      if (!listed.has(sid)) {
        sendResponse({ ok: false, status: 0 });
        return false;
      }
      fetchInfo(sid).then(sendResponse, () => sendResponse({ ok: false, status: 0 }));
      return true;
    }
    if (msg.cmd === 'lf.confirmClosed') {
      runClosed(true);
      return false;
    }
    return undefined;
  });

  /** The send card's transport for one listed session: "detect" is its sessionInfo. */
  function sessionAsk(sessionId) {
    const base = askFor({ kind: 'session', sessionId });
    return async (cmd, extra = {}) => {
      if (cmd !== 'detect') return base(cmd, extra);
      const info = await fetchInfo(sessionId);
      if (!info.ok) return { ok: false, error: { code: 'ext.info_failed', params: { status: info.status } } };
      return request('session.load', { sessionId, values: info.values, url: info.url });
    };
  }

  // ------------------------------------------------------ elements

  let sheet = null;
  try {
    sheet = new CSSStyleSheet();
    sheet.replaceSync(INPAGE_CSS);
  } catch {
    sheet = null;
  }

  function makeHost(kind, rootTag = 'span') {
    const host = document.createElement('lectureflow-ui');
    host.setAttribute('data-kind', kind);
    // Closed in the store builds; the end-to-end test needs to look in.
    const shadow = host.attachShadow({ mode: BUILD.e2e ? 'open' : 'closed' });
    let adopted = false;
    if (sheet) {
      try {
        shadow.adoptedStyleSheets = [sheet];
        adopted = true;
      } catch {
        // Firefox content scripts may not adopt a sheet of their own realm
      }
    }
    if (!adopted) shadow.append(el('style', { text: INPAGE_CSS }));
    const root = el(rootTag, { class: 'lf-root', lang });
    shadow.append(root);
    for (const type of OWN_EVENTS) host.addEventListener(type, (ev) => ev.stopPropagation());
    return { host, shadow, root };
  }

  /** Puts a host where the first placement whose anchor exists says; returns that placement. */
  function place(host, placements) {
    for (const pl of placements || []) {
      let a = null;
      try {
        a = document.querySelector(pl.anchor);
      } catch {
        continue;
      }
      for (let i = 0; a && i < (pl.up || 0); i++) a = a.parentElement;
      if (!a || a === document.documentElement || a === document.body) continue;
      if (pl.position === 'before') a.before(host);
      else if (pl.position === 'after') a.after(host);
      else if (pl.position === 'prepend') a.prepend(host);
      else a.append(host);
      return pl;
    }
    return null;
  }

  // Keeps our elements on a page that re-renders (Panopto's React header).
  const watchers = new Set();
  let watchTimer = null;
  const observer = new MutationObserver(() => {
    if (watchTimer) return;
    watchTimer = setTimeout(() => {
      watchTimer = null;
      for (const w of watchers) {
        try {
          w();
        } catch {
          // never disturb the page
        }
      }
    }, 150);
  });
  function watch(fn) {
    watchers.add(fn);
    if (watchers.size === 1) observer.observe(document.documentElement, { childList: true, subtree: true });
  }

  function sendButton({ compact = false, small = false, label = t('sendButton'), iconName = 'send', onclick }) {
    return el(
      'button',
      { class: `ip-btn${compact ? ' compact' : ''}${small ? ' small' : ''}`, type: 'button', title: compact ? label : null, 'aria-label': label, onclick },
      icon(iconName),
      compact ? null : el('span', { text: label }),
    );
  }

  // ---------------------------------------------------------- the card

  // Where the card goes is mirrored in src/lib/embed.js cardFits (which
  // decides whether a sub-frame has room for it at all): keep them in step.
  function positionCard() {
    if (!card) return;
    const r = card.opener.getBoundingClientRect();
    const W = Math.min(360, window.innerWidth - 16);
    let left = r.left;
    if (left + W > window.innerWidth - 8) left = r.right - W;
    left = Math.max(8, Math.min(left, window.innerWidth - W - 8));
    let top = r.bottom + 6;
    let maxH = window.innerHeight - top - 8;
    if (maxH < 260 && r.top > window.innerHeight - r.bottom) {
      maxH = r.top - 14;
      top = Math.max(8, r.top - 6 - Math.min(maxH, card.frame.scrollHeight));
    }
    card.host.style.left = `${Math.round(left)}px`;
    card.host.style.top = `${Math.round(top)}px`;
    card.frame.style.width = `${W}px`;
    const height = Math.max(160, Math.round(maxH));
    card.frame.style.maxHeight = `${height}px`;
    // the send block keeps in sight only in a card tall enough for it; in a
    // low one it scrolls with the rest (SEC42 中-1)
    fitSend(card.frame, height);
  }

  function onOutside(ev) {
    if (!card) return;
    const path = ev.composedPath();
    if (path.includes(card.host) || path.includes(card.openerHost)) return;
    closeCard();
  }

  // Called after a card closes, or the extension's window for this frame
  // (fromWindow): the list re-checks its rows then.
  const cardClosed = new Set();

  function runClosed(fromWindow = false) {
    for (const f of cardClosed) {
      try {
        f(fromWindow);
      } catch {
        // never disturb the page
      }
    }
  }

  function closeCard() {
    if (!card) return;
    if (card.guard) card.guard.stop();
    card.host.remove();
    document.removeEventListener('mousedown', onOutside, true);
    window.removeEventListener('scroll', positionCard, true);
    window.removeEventListener('resize', positionCard);
    card = null;
    runClosed();
  }

  /**
   * Opens the card under a button; build(mount) returns its controller.
   * guarded: a frame inside a trusted site's page — the confirming controls
   * act only after a delay and while the card is really visible; a click on
   * a card that is not visible moves the confirmation to the extension's
   * window (onInvisible).
   */
  function openCard(opener, openerHost, titleKey, build, { guarded = false, onInvisible = () => {} } = {}) {
    closeCard();
    const h = makeHost('card', 'div');
    Object.assign(h.host.style, { position: 'fixed', zIndex: '2147483000', top: '0px', left: '0px' });
    const body = el('div', { class: 'ip-card-body' });
    const frame = el(
      'section',
      { class: 'ip-card', role: 'dialog', 'aria-label': t(titleKey) },
      el('div', { class: 'ip-card-head' }, el('h2', { text: t(titleKey) }), el('button', { class: 'btn btn-ghost ip-close', id: 'ip-close', type: 'button', 'aria-label': t('close'), title: t('close'), onclick: closeCard }, icon('x'))),
      body,
    );
    frame.addEventListener('keydown', (ev) => {
      if (ev.key === 'Escape') closeCard();
    });
    h.root.append(frame);
    (document.body || document.documentElement).append(h.host);
    card = { ...h, opener, openerHost, frame, ctl: null, guard: null };
    if (guarded) {
      card.guard = guardClicks(frame, {
        visibility: watchVisibility(frame),
        onInvisible: () => {
          closeCard();
          onInvisible();
        },
      });
    }
    positionCard();
    document.addEventListener('mousedown', onOutside, true);
    window.addEventListener('scroll', positionCard, true);
    window.addEventListener('resize', positionCard);
    card.ctl = build(body);
    // the content changes height as it loads
    new ResizeObserver(() => positionCard()).observe(frame);
  }

  function openSingle(opener, openerHost, ask, opts) {
    openCard(
      opener,
      openerHost,
      'sendButton',
      (mount) => {
        // every view may change the send block: place the card again
        const ctl = createSendCard({ mount, ask, host: 'inpage', onView: () => positionCard(), openUrl: (url) => window.open(url, '_blank', 'noopener') });
        ctl.start();
        return ctl;
      },
      opts,
    );
  }

  /** The extension's confirmation window for this frame (src/confirm/). */
  function openWindow(spec) {
    return request('confirmWindow', spec);
  }

  /**
   * A button was clicked: open its card here, or let the extension's window
   * confirm. spec: { flow: 'single', source } | { flow: 'bulk', rows, origin };
   * inPage(opts) opens the card in this page.
   */
  async function openFlow(opener, spec, inPage) {
    if (card && card.opener === opener) {
      closeCard(); // the button toggles
      return;
    }
    if (!sub) {
      inPage({});
      return;
    }
    const r = await request('route');
    const route = r && r.ok ? r.route : 'window';
    // A frame too small for the card (an embedded player) gets the window.
    const box = opener.getBoundingClientRect();
    const fits = cardFits({ viewWidth: window.innerWidth, viewHeight: window.innerHeight, openerTop: box.top, openerBottom: box.bottom, flow: spec.flow });
    const p = cardPlacement({ sub, route, fits });
    if (p.where === 'guarded') inPage({ guarded: true, onInvisible: () => openWindow({ ...spec, reason: 'invisible' }) });
    else if (p.where === 'inpage') inPage({});
    else {
      closeCard();
      openWindow({ ...spec, reason: p.reason });
    }
  }

  // -------------------------------------------------------- player

  function player() {
    const h = makeHost('player');
    const draw = (compact) => {
      const b = sendButton({ compact, onclick: () => openFlow(b, { flow: 'single', source: { kind: 'tab' } }, (opts) => openSingle(b, h.host, askFor({ kind: 'tab' }), opts)) });
      h.root.replaceChildren(b);
    };
    const ensure = () => {
      if (h.host.isConnected) return;
      const pl = place(h.host, cfg.player);
      if (pl) draw(!!pl.compact);
    };
    ensure();
    watch(ensure);
  }

  // ---------------------------------------------------------- list

  function list() {
    const L = cfg.list;
    const rows = new Map(); // sessionId → { sessionId, tr, title, box, ctl, item, status }
    const selected = new Set();
    const clean = (s) => String(s || '').replace(/\s+/g, ' ').trim().slice(0, 300);

    function renderRow(e) {
      const st = rowState(e.item);
      const can = st === 'new' || st === 'again';
      if (!can) selected.delete(e.sessionId);
      const cb = el('input', { type: 'checkbox', 'aria-label': t('selectRow', [e.title || e.sessionId]) });
      cb.checked = selected.has(e.sessionId);
      cb.disabled = !can;
      cb.addEventListener('change', () => {
        if (cb.checked) selected.add(e.sessionId);
        else selected.delete(e.sessionId);
        renderBar();
      });
      e.box.root.replaceChildren(el('span', { class: 'ip-check' }, cb));
      const parts = [];
      if (st === 'imported') {
        parts.push(el('span', { class: 'chip chip-ok', dataset: { rowState: 'imported' } }, icon('check'), el('span', { text: t('state_imported') })));
        const target = e.item.lectureId ? { lectureId: e.item.lectureId } : { taskId: e.item.taskId };
        parts.push(el('button', { class: 'link-btn', type: 'button', onclick: () => request('open', target) }, el('span', { text: t('openInApp') })));
      } else if (st === 'active') {
        const state = (e.status && e.status.state) || e.item.state;
        const p = progressOf(e.status || {});
        const text = state === 'downloading' && p.percent >= 0 ? t('rowDownloading', [String(p.percent)]) : t(`state_${state}`);
        parts.push(el('span', { class: 'chip chip-run', dataset: { rowState: state } }, el('span', { text })));
        if (p.percent >= 0) {
          const fill = el('div', { class: 'bar-fill' });
          fill.style.width = `${p.percent}%`;
          parts.push(el('div', { class: 'bar' }, fill));
        }
      } else {
        if (st === 'refresh') parts.push(el('span', { class: 'chip chip-warn', dataset: { rowState: 'needs_refresh' }, text: t('state_needs_refresh') }));
        const b = sendButton({
          small: true,
          onclick: () => openFlow(b, { flow: 'single', source: { kind: 'session', sessionId: e.sessionId }, rows: [{ sessionId: e.sessionId, title: e.title }] }, (opts) => openSingle(b, e.ctl.host, sessionAsk(e.sessionId), opts)),
        });
        parts.push(b);
      }
      e.ctl.root.replaceChildren(el('span', { class: 'ip-row', dataset: { sessionId: e.sessionId } }, ...parts));
    }

    // ---- the selection bar
    const bar = makeHost('bar', 'div');
    let barMode = 'fixed';
    function renderBar() {
      const n = [...selected].filter((sid) => rows.has(sid)).length;
      if (n === 0) {
        bar.host.remove();
        return;
      }
      const b = el('button', { class: 'btn btn-primary', type: 'button', id: 'ip-bar-send', onclick: () => openBulk(b, bar.host, [...rows.values()].filter((e) => selected.has(e.sessionId)), 'selection') }, icon('send'), el('span', { text: t('selSend', [String(n)]) }));
      bar.root.replaceChildren(
        el(
          'div',
          { class: 'ip-bar', role: 'region', 'aria-label': t('selBar', [String(n)]) },
          el('span', { id: 'ip-bar-count', text: t('selBar', [String(n)]) }),
          el('span', { class: 'sep', text: '·' }),
          el('button', { class: 'link-btn', type: 'button', id: 'ip-bar-clear', onclick: () => {
            selected.clear();
            for (const e of rows.values()) renderRow(e);
            renderBar();
          } }, el('span', { text: t('selClear') })),
          b,
        ),
      );
      placeBar();
    }
    function placeBar() {
      if (barMode === 'fixed') {
        Object.assign(bar.host.style, { position: 'fixed', bottom: '16px', left: '50%', transform: 'translateX(-50%)', top: '', zIndex: '2147482000', margin: '' });
        if (bar.host.parentNode !== document.body) (document.body || document.documentElement).append(bar.host);
      } else {
        // An LMS made this frame taller than the window: the bottom of the
        // frame is out of sight, so the bar sits under the toolbar instead.
        Object.assign(bar.host.style, { position: 'sticky', top: '8px', bottom: '', left: '', transform: '', zIndex: '2147482000', margin: '8px 0' });
        // right above the list (under the toolbar)
        const firstRow = [...rows.values()][0];
        if (firstRow) (firstRow.tr.closest('table') || firstRow.tr).before(bar.host);
        else if (header.host.isConnected) header.host.parentElement.after(bar.host);
      }
    }
    if (window.top !== window && typeof IntersectionObserver === 'function') {
      // Is the bottom of this frame's viewport visible in the browser window?
      const sentinel = document.createElement('lectureflow-ui');
      sentinel.setAttribute('data-kind', 'sentinel');
      sentinel.setAttribute('aria-hidden', 'true');
      Object.assign(sentinel.style, { position: 'fixed', bottom: '0px', left: '0px', width: '1px', height: '1px', pointerEvents: 'none' });
      (document.body || document.documentElement).append(sentinel);
      new IntersectionObserver((entries) => {
        const visible = entries.some((x) => x.isIntersecting);
        const mode = visible ? 'fixed' : 'flow';
        if (mode !== barMode) {
          barMode = mode;
          if (bar.host.isConnected) placeBar();
        }
      }).observe(sentinel);
    }

    // ---- the button for the whole page
    const header = makeHost('header');
    // Panopto's folder bar has a background layer above its content.
    Object.assign(header.host.style, { position: 'relative', zIndex: '5' });
    const headerBtn = sendButton({ label: t('bulkButton'), iconName: 'refresh', onclick: () => openBulk(headerBtn, header.host, [...rows.values()], 'page') });
    header.root.replaceChildren(headerBtn);

    function openBulk(opener, openerHost, list, origin) {
      if (list.length === 0) return;
      const spec = { flow: 'bulk', rows: list.map((e) => ({ sessionId: e.sessionId, title: e.title })), origin };
      openFlow(opener, spec, (opts) => openBulkHere(opener, openerHost, list, origin, opts));
    }

    function openBulkHere(opener, openerHost, list, origin, opts) {
      openCard(opener, openerHost, 'bulkTitle', (mount) => {
        const ctl = createBulkCard({
          mount,
          ask: request,
          rows: list.map((e) => ({ sessionId: e.sessionId, title: e.title })),
          origin,
          fetchInfo,
          presets: cfg.presets,
          platformName: cfg.name,
          onSent: (results) => {
            for (const [sid, r] of results) {
              const e = rows.get(sid);
              if (e && r.state === 'queued') {
                e.item = { sessionId: sid, state: 'queued', taskId: r.taskId };
                renderRow(e);
              }
            }
            selected.clear();
            for (const e of rows.values()) renderRow(e);
            renderBar();
          },
          onClose: closeCard,
        });
        ctl.start();
        return ctl;
      }, opts);
    }

    // ---- states from LectureFlow
    // Passive: the page asks on its own, so LectureFlow is not started for
    // it (hello noLaunch). Not running → no row states, the buttons work as
    // ever; once a card the user opened has brought LectureFlow up, the
    // rows are checked again when it closes.
    let connected = null;
    async function checkRows(ids) {
      if (connected === null) {
        const c = await request('connection', { passive: true });
        connected = !!(c.ok && c.connection && c.connection.status === 'connected');
      }
      if (!connected || ids.length === 0) return;
      const r = await request('checkMany', { sessionIds: ids, passive: true });
      if (!r.ok) return;
      for (const item of r.items || []) {
        const e = rows.get(item.sessionId);
        if (!e) continue;
        e.item = item;
        renderRow(e);
      }
      renderBar();
    }
    cardClosed.add((fromWindow) => {
      // the window may have sent some of the rows: always check again then
      if (connected !== false && !fromWindow) return;
      connected = null;
      checkRows([...rows.keys()]);
    });
    statusHandlers.add((status) => {
      if (!status || status.platform !== cfg.id) return;
      const e = rows.get(status.sessionId);
      if (!e) return;
      const state = status.state === 'done' ? (status.lectureId ? 'imported' : 'done') : status.state;
      e.item = { ...(e.item || {}), sessionId: status.sessionId, state, taskId: status.taskId, lectureId: status.lectureId || (e.item && e.item.lectureId) || '' };
      e.status = status;
      renderRow(e);
      renderBar();
    });

    // ---- rows on the page
    function scan() {
      const fresh = [];
      for (const tr of document.querySelectorAll(L.row)) {
        const sid = rowSessionId(tr.getAttribute(L.idAttr), L, cfg.key);
        if (!sid) continue;
        const anchor = tr.querySelector(L.anchor);
        if (!anchor) continue;
        let e = rows.get(sid);
        if (!e) {
          e = { sessionId: sid, item: null, status: null, box: makeHost('check'), ctl: makeHost('row') };
          rows.set(sid, e);
          listed.add(sid);
          fresh.push(sid);
        }
        e.tr = tr;
        const titleEl = tr.querySelector(L.title) || anchor;
        e.title = clean(titleEl.textContent);
        if (!anchor.contains(e.box.host)) anchor.prepend(e.box.host);
        if (e.ctl.host.previousElementSibling !== anchor) anchor.after(e.ctl.host);
        if (fresh.includes(sid) || !e.ctl.root.firstChild) renderRow(e);
      }
      for (const [sid, e] of rows) {
        if (!e.tr.isConnected) {
          rows.delete(sid);
          listed.delete(sid);
          selected.delete(sid);
          e.box.host.remove();
          e.ctl.host.remove();
        }
      }
      if (!header.host.isConnected && rows.size) place(header.host, L.header);
      renderBar();
      if (fresh.length) checkRows(fresh);
    }
    scan();
    watch(scan);
  }

  return { player, list };
}
