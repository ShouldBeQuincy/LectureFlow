// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Small DOM helpers: element builder, buttons, toasts, dialogs, drop targets.

import { icon } from './icons.js';
import { t } from './i18n.js';
import { DESKTOP, attachDroppedPaths, pickLocalFiles } from './api.js';

export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  let value;
  if (props) {
    for (const [k, v] of Object.entries(props)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'value') value = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, String(v));
    }
  }
  append(el, children);
  // After the children, so a <select> already has its options.
  if (value !== undefined) el.value = value;
  return el;
}

export function append(el, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(el, c);
    else if (c instanceof Node) el.append(c);
    else el.append(document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

// Re-render while keeping keyboard focus on the element with the same
// data-fkey, so background refreshes (SSE) do not throw the user's focus away.
// A control that may be gone after the re-render (a row's "启动" becomes
// "重试" or nothing once launched, 3.2.0 L14) names where the focus goes
// then in data-fnext: fkeys separated by spaces, tried in order (the first
// one present, enabled and shown wins). The fallbacks are for keyboard focus
// only (R19): a control clicked with the mouse has the focus too, and moving
// it to a segment would open that segment's card (:focus-within) under no
// pointer.
//
// A control still there but disabled (the dashboard disables a button while
// its request runs) cannot hold the focus, and none of the fallbacks takes
// it either while the request runs (R21): each of them does something else
// on Enter (opens a lecture, the review page, "新建课程"), and a key pressed
// twice or held down would set it off. The focus waits on the page (body),
// where Enter does nothing; the next re-render settles it: on the control
// again once it is enabled (the request failed, a dialog was cancelled), or
// on its fallbacks once it is gone (done). Focus moved elsewhere meanwhile
// ends the wait, as soon as it moves (a focusin watch, R22): anywhere but
// into another dialog (a confirmation the action asks for). A move within
// the dialog the control itself is in (a wizard's next step) ends it too.
//
// A control in a popover that shows only while the focus is in it (a stage
// card, :focus-within; data-fopener on the popover) is drawn anew hidden:
// with keyboard focus it takes the focus through the popover's opener (the
// segment), as a dialog gives it back (giveBack; R79 M2). A popover built
// only when it opens (pipeline.js lazyCards) has the control once its
// opener has the focus.
let waitingFor = null; // {key, next, dlg, kb, pop} of the control disabled in place

const findKey = (k) => document.querySelector(`[data-fkey="${CSS.escape(k)}"]`);

export function preserveFocus(fn) {
  const a = document.activeElement;
  const wait = waiting(a);
  const key = wait ? wait.key : a && a.dataset ? a.dataset.fkey : null;
  const next = wait ? wait.next : key && a.dataset.fnext && keyboardFocused(a) ? a.dataset.fnext.split(' ').filter(Boolean) : [];
  // keyboard focus (R19): only then does a popover's opener take it (pop: its fkey)
  const kb = wait ? wait.kb : !!key && keyboardFocused(a);
  const pop = wait ? wait.pop : key ? popoverOpener(a) : null;
  fn();
  // the view moved the focus itself while it re-rendered (focusin ended
  // the wait): the focus is where the view put it (R29)
  if (wait && waitingFor !== wait) return;
  if (!key) return;
  const same = findAgain(key, pop, kb);
  if (same && same.disabled) {
    startWaiting(key, next, same, kb, pop);
    // The control drawn anew leaves the focus on the page. A browser does
    // this itself once the focused element is removed (activeElement is then
    // body already); this is for a DOM stand-in, which keeps the removed node
    // as activeElement.
    const cur = document.activeElement;
    if (cur && cur !== same && !cur.isConnected && typeof cur.blur === 'function') cur.blur();
    return;
  }
  if (wait) waitingFor = null;
  let el = same && !same.disabled ? same : null;
  for (let i = 0; !el && i < next.length; i++) {
    const c = findKey(next[i]);
    if (c && !c.disabled && shown(c)) el = c;
  }
  if (el && el !== document.activeElement) {
    el.focus({ preventScroll: true });
    throughOpener(el, kb);
  }
}

// kb, pop: how the control had the focus and its popover's opener, for
// when it is enabled again (preserveFocus)
function startWaiting(key, next, same, kb = false, pop = null) {
  waitingFor = { key, next, dlg: dialogOf(same) };
  Object.assign(waitingFor, { kb, pop });
  watchFocus();
}

// ---- a control in a popover (preserveFocus, giveBack) ----

// The fkey of the always-shown opener of the popover el is in (its
// data-fopener), or null. el may be off the page (drawn anew): its old
// popover still holds it.
function popoverOpener(el) {
  for (let n = el ? el.parentNode : null; n && n.dataset; n = n.parentNode) if (n.dataset.fopener) return n.dataset.fopener;
  return null;
}

// The control with fkey key after a redraw. One in a popover built only
// when it opens is not there yet: with keyboard focus, the popover's opener
// (pop: its fkey) takes the focus, which opens and builds it, and the
// control is looked for again. Found disabled (its request runs), the
// opener does not keep the focus: it waits on the page (R21).
function findAgain(key, pop, kb) {
  const el = findKey(key);
  if (el || !kb || !pop) return el;
  const o = findKey(pop);
  if (!o || o.disabled || !shown(o)) return null;
  o.focus({ preventScroll: true });
  const again = findKey(key);
  if (again && again.disabled && document.activeElement === o && typeof o.blur === 'function') o.blur();
  return again;
}

// After el.focus(): a control in a popover hidden while the focus is
// outside it did not take the focus. With keyboard focus its opener takes
// it first, which shows the popover again, then the control; the focus
// stays on the opener if the control still cannot take it. A mouse click's
// focus is not moved there (R19: the card would stay open under no pointer).
function throughOpener(el, kb) {
  if (document.activeElement === el || !kb) return;
  const opener = openerOf(el);
  if (!opener) return;
  opener.focus({ preventScroll: true });
  el.focus({ preventScroll: true });
}

// The control the focus waits for, when the focus is still where the wait
// left it (the page, or that control); focus taken elsewhere ends the wait,
// except into another dialog (a confirmation the action asks for, R22: not
// the dialog the control is in).
function waiting(a) {
  const w = waitingFor;
  if (!w) return null;
  // the dialog the control was in is closed: nothing to wait for (R29)
  if (w.dlg && !w.dlg.isConnected) {
    waitingFor = null;
    return null;
  }
  if (!a || a === document.body || a === document.documentElement || (a.dataset && a.dataset.fkey === w.key)) return w;
  const d = dialogOf(a);
  if (!d || d === w.dlg) waitingFor = null;
  return null;
}

// One focusin watch for every wait, added with the first: focus moved by the
// user (or a view) ends the wait right away, so a later return to the page
// (a click on its background) does not bring it back to life (R22).
let focusWatched = false;
function watchFocus() {
  if (focusWatched || typeof document.addEventListener !== 'function') return;
  focusWatched = true;
  document.addEventListener(
    'focusin',
    (e) => {
      if (waitingFor) waiting(e.target);
    },
    true,
  );
  // A press with the pointer anywhere but in another dialog (a
  // confirmation, its backdrop) ends the wait too, also on what takes no
  // focus (text, the page's background), where no focusin comes: the
  // control enabled much later (a request that hangs, a review page waiting
  // to be reviewable) must not take the focus unseen (R29). Not waiting():
  // that counts a target of body as the focus still in place.
  document.addEventListener(
    'pointerdown',
    (e) => {
      const w = waitingFor;
      if (!w) return;
      const d = dialogOf(e.target);
      if (!d || d === w.dlg) waitingFor = null;
    },
    true,
  );
}

function dialogOf(el) {
  for (let n = el; n; n = n.parentNode) if (n.tagName === 'DIALOG') return n;
  return null;
}

// Whether a fallback can take the focus: not hidden, and laid out (no
// display:none on it or above it). A stand-in without layout counts as shown.
function shown(el) {
  if (el.hidden) return false;
  return typeof el.getClientRects !== 'function' || el.getClientRects().length > 0;
}

// Whether el has the focus from the keyboard (:focus-visible). A browser
// (or a test's stand-in) that cannot tell counts as keyboard, the safe side
// for keyboard users.
function keyboardFocused(el) {
  try {
    return typeof el.matches !== 'function' || el.matches(':focus-visible');
  } catch {
    return true;
  }
}

// dataset of a control: its focus key and where the focus goes when it is
// gone after a re-render (preserveFocus)
function focusData(o) {
  if (!o.fkey) return undefined;
  const next = Array.isArray(o.fnext) ? o.fnext.join(' ') : o.fnext;
  return next ? { fkey: o.fkey, fnext: next } : { fkey: o.fkey };
}

// ---- texts that change by the second (3.2.0 L12) ----
// everySecond(node, fn): fn() runs once a second while node is in the
// document, until it returns false (nothing left to count) or
// stopEverySecond(node). One timer serves them all, and it runs only while
// there is one: a page without a countdown or an open card runs none. A
// node out of the document for two ticks running (redrawn, the page left)
// is dropped.
const tickers = new Map(); // node → {fn, out}
let tickTimer = 0;

export function everySecond(node, fn) {
  tickers.set(node, { fn, out: 0 });
  if (!tickTimer) tickTimer = setInterval(tickAll, 1000);
}

export function stopEverySecond(node) {
  tickers.delete(node);
  if (!tickers.size && tickTimer) {
    clearInterval(tickTimer);
    tickTimer = 0;
  }
}

// (for tests: how many nodes are ticking, and whether the timer runs)
export function ticking() {
  return { nodes: tickers.size, timer: !!tickTimer };
}

function tickAll() {
  for (const [node, tk] of [...tickers]) {
    let more = true;
    if (!node.isConnected) more = ++tk.out < 2;
    else {
      tk.out = 0;
      try {
        more = tk.fn() !== false;
      } catch {
        more = false;
      }
    }
    if (!more && tickers.get(node) === tk) tickers.delete(node);
  }
  if (!tickers.size && tickTimer) {
    clearInterval(tickTimer);
    tickTimer = 0;
  }
}

// ---- buttons ----

// btn(label, {icon: 'play', kind: 'primary'|'danger'|'ghost'|'quiet', size: 'sm', onclick, title, fkey})
export function btn(label, o = {}) {
  const cls = ['btn'];
  if (o.kind) cls.push('btn-' + o.kind);
  if (o.size) cls.push('btn-' + o.size);
  if (o.class) cls.push(o.class);
  return h(
    'button',
    {
      type: o.type || 'button',
      class: cls.join(' '),
      title: o.title,
      disabled: o.disabled,
      'aria-label': o.ariaLabel,
      dataset: focusData(o),
      onclick: o.onclick,
    },
    o.icon ? icon(o.icon, { size: o.size === 'sm' ? 14 : 16 }) : null,
    label ? h('span', { class: 'btn-label' }, label) : null,
    o.count != null ? h('span', { class: 'btn-count' }, String(o.count)) : null,
  );
}

// Square icon-only button with an accessible name (also the tooltip).
export function iconBtn(name, label, o = {}) {
  return h(
    'button',
    {
      type: 'button',
      class: 'icon-btn' + (o.class ? ' ' + o.class : ''),
      'aria-label': label,
      title: o.title || label,
      'aria-haspopup': o.menu ? 'menu' : undefined,
      'aria-pressed': o.pressed == null ? undefined : String(!!o.pressed),
      dataset: focusData(o),
      onclick: o.onclick,
    },
    icon(name, { size: o.size || 16 }),
  );
}

// Placeholder blocks while data loads.
export function skeleton(lines = 3, cls = '') {
  return h(
    'div',
    { class: 'skel-group ' + cls, 'aria-hidden': 'true' },
    Array.from({ length: lines }, (_, i) => h('div', { class: 'skel', style: { width: `${[92, 74, 84, 60, 88][i % 5]}%` } })),
  );
}

// An error inside a card, with a retry button (loading failed).
export function errorBlock(message, onRetry) {
  return h(
    'div',
    { class: 'error-block', role: 'alert' },
    icon('circle-alert', { size: 18 }),
    h('div', { class: 'error-block-text' }, h('strong', null, t('common.load_failed')), h('span', null, message || t('common.unknown_error'))),
    onRetry ? btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', onclick: onRetry }) : null,
  );
}

// ---- toasts (SPEC §9.8) ----
// info / success / warn disappear after 4 seconds; error stays until closed.
// opts.sticky === true (round 8 interface F1): a toast of any kind stays
// until closed, for a text too long to read in 4 seconds; it keeps its close
// button, merging, role and kind. A toast once shown sticky stays sticky when
// the same text is merged into it (and a sticky text merged into a fading
// toast stops its fading). Without opts a toast behaves as before.
// A text told again is news: its toast, counted, becomes the newest (the
// last), as the page shows only the newest few (style.css, round 9 WH-04);
// merged in place into one hidden behind newer ones, a new failure was
// neither seen nor read out (R90 M1). Moved, it is added to the live region
// (#toasts, aria-live) again and read out; a focus inside it (its close
// button) stays there.

const TOAST_ICONS = { info: 'circle-alert', success: 'circle-check', warn: 'triangle-alert', error: 'circle-alert' };

export function toast(message, kind = 'info', opts = {}) {
  const root = document.getElementById('toasts');
  if (!root || !message) return;
  const sticky = !!opts && opts.sticky === true;
  for (const el of root.children) {
    if (el.dataset.msg === message && el.dataset.kind === kind) {
      const n = Number(el.dataset.count || 1) + 1;
      el.dataset.count = String(n);
      el.querySelector('.toast-count').textContent = `×${n}`;
      if (sticky) el._sticky = true;
      if (el._sticky) clearTimeout(el._timer);
      else if (kind !== 'error') armToast(el);
      if (el !== root.children[root.children.length - 1]) {
        const a = document.activeElement;
        const inside = !!a && a !== el && el.contains(a);
        root.append(el);
        if (inside && a.isConnected && document.activeElement !== a) a.focus({ preventScroll: true });
      }
      return;
    }
  }
  const el = h(
    'div',
    { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status', dataset: { msg: message, kind } },
    icon(TOAST_ICONS[kind] || 'circle-alert', { size: 16, class: 'toast-icon' }),
    h('span', { class: 'toast-msg', text: message }),
    h('span', { class: 'toast-count' }),
    h('button', { type: 'button', class: 'toast-close', 'aria-label': t('common.toast_close'), title: t('common.close'), onclick: () => el.remove() }, icon('x', { size: 14 })),
  );
  root.append(el);
  if (sticky) el._sticky = true;
  else if (kind !== 'error') armToast(el);
}

function armToast(el) {
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.remove(), 4000);
}

// ---- dialogs (native <dialog>: focus trap and Esc for free) ----

// The dialogs openDialog has open, each with the route (location.hash) it
// was opened on (G3).
const openDialogs = new Set();

// G3 (T16, T18): a route change (the navigation, the browser's back and
// forward) closes the dialogs opened on another route, as cancelled; app.js
// calls it on every route. A dialog opened on the new route stays: one that
// an action opens together with its navigation. A dialog whose request runs
// (busy) leaves the page at once; the request goes on, and the dialog
// resolves with its answer (close(v)), or as cancelled once it ended without
// one. No focus is given back here: the view of the new route is not drawn
// yet. → {closed: how many, opener: the control that opened the first of
// them, or null, restore(fallback)}. restore, for a view that stays (its
// update() ran): when the focus is still on the page's body (or on a
// removed node), it goes back to the opener as a closed dialog gives it
// back (giveBack: found again by its data-fkey when redrawn), else to
// fallback (R62 N3); a focus the view placed itself is left alone.
export function closeDialogsOnRoute() {
  const gone = [...openDialogs].filter((d) => d.hash !== location.hash);
  for (const d of [...gone].reverse()) d.leave();
  const first = gone[0] || null;
  const restore = (fallback) => {
    if (!first || !focusLost()) return;
    giveBack(first.prev, first.kb);
    if (focusLost() && fallback && typeof fallback.focus === 'function') fallback.focus({ preventScroll: true });
  };
  return { closed: gone.length, opener: first ? first.prev : null, restore };
}

// The focus is on the page's body, or on a node no longer in the page.
function focusLost() {
  const a = document.activeElement;
  return !a || a === document.body || !a.isConnected;
}

// A modal that is no openDialog (wizard.js: the wizard and the syllabus
// editor ask before they drop unsaved work, so a route change leaves them
// open) hands the focus back by the same rules (T21 M4). focusOpener() when
// it opens → back(fallback), to call once it is closed: the opener in place
// takes the focus without scrolling; one redrawn meanwhile (the navigation
// on every route, a view on a refresh) is found again as after a dialog
// (giveBack: data-fkey, data-fnext); and when the route changed and nothing
// took the focus (the opener went with its page), fallback does (the view),
// as in closeDialogsOnRoute().restore.
export function focusOpener() {
  const prev = document.activeElement;
  const kb = !!prev && prev !== document.body && keyboardFocused(prev);
  const hash = location.hash;
  return (fallback) => {
    if (prev && prev.isConnected && !prev.disabled && typeof prev.focus === 'function') prev.focus({ preventScroll: true });
    if (document.activeElement !== prev) giveBack(prev, kb);
    if (location.hash !== hash && focusLost() && fallback && typeof fallback.focus === 'function') fallback.focus({ preventScroll: true });
  };
}

// openDialog(title, build(close, busy) → Node, {wide, className, route})
// → Promise of the value passed to close (null on Esc / cancel, or when a
// route change closed it). busy(promise) → the promise: a request the dialog
// sends, which a route change lets finish (closeDialogsOnRoute); when it
// fails with the dialog off the page (a route change, or closed by the
// user meanwhile), its error is toasted (tellFailure). route:
// false for a dialog that belongs to no page and stays across a route
// change (R62 N1: the uploads' "which file to keep", whose cancel means
// "none" while the flow goes on). confirmCancel (round 9 F1): () → boolean
// or a promise of one, asked only when Esc or a click on the backdrop would
// cancel the dialog; false keeps it open with the focus on the control it
// was on, true closes it as without it (null). While it answers, Esc and
// the backdrop do nothing more. Not asked: the dialog's own close(v) (its
// "cancel" button decides itself whether to ask), a route change (leave),
// a request in flight (busy, as before). Without it: as before.
export function openDialog(title, build, opts = {}) {
  return new Promise((resolve) => {
    const prev = document.activeElement;
    const kb = !!prev && prev !== document.body && keyboardFocused(prev);
    const dlg = h('dialog', { class: 'dlg' + (opts.wide ? ' dlg-wide' : '') + (opts.className ? ' ' + opts.className : ''), 'aria-label': title });
    // Toasts outside a modal sit behind its backdrop (inert, blurred): while
    // the dialog is open it hosts them, then they go back to where they were
    // (the page, or a wizard that opened this dialog), as wizard.js does.
    const toasts = document.getElementById('toasts');
    const home = toasts && toasts.parentNode;
    let done = false;
    let left = false; // closed by a route change while busy: off the page, not resolved yet
    let pending = 0;
    const entry = { hash: location.hash, prev, kb, leave: () => leave() };
    const takeDown = () => {
      openDialogs.delete(entry);
      if (toasts && toasts.parentNode === dlg) (home && home.isConnected ? home : document.body).append(toasts);
      if (dlg.open) dlg.close();
      dlg.remove();
    };
    const close = (v) => {
      if (done) return;
      done = true;
      const wasLeft = left;
      takeDown();
      if (!wasLeft) giveBack(prev, kb);
      resolve(v);
    };
    const leave = () => {
      if (done || left) return;
      if (!pending) {
        done = true;
        takeDown();
        resolve(null);
        return;
      }
      left = true;
      takeDown();
    };
    const busy = (p) => {
      pending++;
      const end = () => {
        pending--;
        // after the dialog's own handling of the answer (close(v)) ran
        if (left && !pending) setTimeout(() => close(null), 0);
      };
      // R62 N5: the dialog shows the error of its request itself; off the
      // page it cannot, and the error is told as a toast. Off the page: a
      // route change took it there, or the user closed it while the
      // request ran (Esc, the backdrop, "cancel": R68). This runs before
      // the dialog's own handling of the error, so `done` is a close from
      // before the request ended.
      const failed = (err) => {
        if (left || done) tellFailure(err);
        end();
      };
      Promise.resolve(p).then(end, failed);
      return p;
    };
    // opts.confirmCancel asked before Esc or the backdrop cancels (F1); one
    // question at a time
    let asking = false;
    const askCancel = () => {
      if (done || left || asking) return;
      const was = document.activeElement;
      const answer = (ok) => {
        asking = false;
        if (done || left) return;
        if (ok) close(null);
        // kept open: the focus stays on (or goes back to) the control it was on
        else if (was && was !== document.activeElement && was.isConnected && dlg.contains(was) && typeof was.focus === 'function') was.focus({ preventScroll: true });
      };
      asking = true;
      let r;
      try {
        r = opts.confirmCancel();
      } catch (err) {
        asking = false;
        throw err;
      }
      // a promise that fails keeps the dialog open
      if (r && typeof r.then === 'function') Promise.resolve(r).then(answer, () => answer(false));
      else answer(r);
    };
    dlg.addEventListener('cancel', (e) => {
      e.preventDefault();
      // e.cancelable false: the browser closes it anyway (no user
      // activation since the last Esc), so it closes as without the question
      if (typeof opts.confirmCancel !== 'function' || e.cancelable === false) close(null);
      else askCancel();
    });
    // a click on the backdrop (outside the dialog box) cancels
    dlg.addEventListener('mousedown', (e) => {
      if (e.target !== dlg) return;
      const r = dlg.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
        if (typeof opts.confirmCancel !== 'function') close(null);
        else {
          // the focus stays on the control it is on while the question runs
          e.preventDefault();
          askCancel();
        }
      }
    });
    dlg.append(h('h2', { class: 'dlg-title', text: title }), build(close, busy));
    document.body.append(dlg);
    dlg.showModal();
    if (opts.route !== false) openDialogs.add(entry);
    if (toasts) dlg.append(toasts);
  });
}

// The error of a request whose dialog left the page while it ran (busy),
// as an error toast, which stays until closed: the user is elsewhere by
// now. Not an error the API layer told already (a request not sent
// quiet: err.reported, api.js), nor a request that was aborted.
// A network failure (status 0) while the "connection lost" banner of the
// page shows (index.html #conn-banner) is told as the app tells a request
// then (app.js onRequestError, T24 G6; R76 L-4; r8-web-dash WD-09): an
// action's (a write, or a request of no known method) gets a short warning
// that it did not happen, the same text, so however many fail it is one
// toast, counted; a read's (err.method GET or HEAD, api.js) none: the
// banner says that the connection is gone and the page what it lacks.
function tellFailure(err) {
  if (!err || err.reported || err.name === 'AbortError' || err.status === -1) return;
  if (err.status === 0 && connectionBannerShown()) {
    if (err.method !== 'GET' && err.method !== 'HEAD') toast(t('nav.conn_lost_action'), 'warn');
    return;
  }
  toast(err.message || t('common.unknown_error'), 'error');
}

function connectionBannerShown() {
  const b = document.getElementById('conn-banner');
  return !!b && !b.hidden;
}

// The focus back to the control that opened a dialog (T6 #1).
// - Redrawn while the dialog was open (a refresh): the control with its
//   data-fkey takes it; gone, its data-fnext fallbacks (keyboard focus
//   only, as in preserveFocus); still there but disabled (its request runs):
//   the focus waits for it on the page, as in preserveFocus.
// - A control in a popover that shows only while the focus is in it or the
//   pointer over it (a stage card, :hover / :focus-within) is hidden once the
//   dialog has the focus, and focus() on it does nothing. With keyboard
//   focus, the popover's opener (data-fopener on the popover: the fkey of a
//   control next to it that is always shown, the stage's segment) takes the
//   focus first, which shows the popover again, then the control; the focus
//   stays on the opener if the control still cannot take it. A mouse
//   click's focus is not moved there (R19: the card would stay open under
//   no pointer).
function giveBack(prev, kb) {
  if (!prev || typeof prev.focus !== 'function') return;
  let el = prev;
  const redrawn = !prev.isConnected;
  const pop = popoverOpener(prev);
  if (prev.isConnected && prev.disabled && prev.dataset && prev.dataset.fkey) {
    // disabled in place while the dialog was open (its request runs): the
    // focus waits for it, as for a control drawn anew disabled (R29)
    startWaiting(prev.dataset.fkey, kb && prev.dataset.fnext ? prev.dataset.fnext.split(' ').filter(Boolean) : [], prev, kb, pop);
    return;
  }
  if (redrawn) {
    const key = prev.dataset ? prev.dataset.fkey : null;
    if (!key) return;
    el = findAgain(key, pop, kb);
    if (el && el.disabled) {
      startWaiting(key, kb && prev.dataset.fnext ? prev.dataset.fnext.split(' ').filter(Boolean) : [], el, kb, pop);
      return;
    }
    if (!el && kb && prev.dataset.fnext) {
      for (const k of prev.dataset.fnext.split(' ').filter(Boolean)) {
        const c = findKey(k);
        if (c && !c.disabled && shown(c)) {
          el = c;
          break;
        }
      }
    }
    if (!el) return;
  }
  // a control found anew is where the old one was, or elsewhere (a
  // fallback): no scrolling, as in preserveFocus; one in a hidden popover
  // through its opener
  el.focus(redrawn ? { preventScroll: true } : undefined);
  throughOpener(el, kb);
}

// The always-shown control a popover around el names (data-fopener), or null.
function openerOf(el) {
  for (let n = el.parentNode; n && n.dataset; n = n.parentNode) {
    if (!n.dataset.fopener) continue;
    const scope = n.parentNode && typeof n.parentNode.querySelector === 'function' ? n.parentNode : document;
    const o = scope.querySelector(`[data-fkey="${CSS.escape(n.dataset.fopener)}"]`);
    return o && o !== el && !o.disabled ? o : null;
  }
  return null;
}

// focusCancel: the cancel button has the initial focus (always for danger;
// also for a non-destructive choice the user should think twice about).
export function confirmDialog({ title, message, okText = t('common.ok'), cancelText = t('common.cancel'), danger = false, focusCancel = danger, body = null }) {
  return openDialog(title, (close) =>
    h(
      'div',
      null,
      message ? h('p', { class: 'dlg-msg', text: message }) : null,
      body,
      h(
        'div',
        { class: 'dlg-actions' },
        h('button', { type: 'button', class: 'btn', autofocus: focusCancel, onclick: () => close(false) }, cancelText),
        h('button', { type: 'button', class: danger ? 'btn btn-danger-solid' : 'btn btn-primary', autofocus: !focusCancel, onclick: () => close(true) }, okText),
      ),
    ),
  ).then((v) => v === true);
}

export function inputDialog({ title, label, value = '', okText = t('common.ok'), placeholder = '', type = 'text', hint = '', validate = null }) {
  return openDialog(title, (close) => {
    const input = h('input', { type, class: 'input', value, placeholder, autofocus: true, required: true, 'aria-label': label });
    const ok = h('button', { type: 'submit', class: 'btn btn-primary' }, okText);
    const err = h('p', { class: 'dlg-field-error', role: 'alert' });
    const sync = () => {
      const v = input.value.trim();
      const msg = v && validate ? validate(v) : '';
      err.textContent = msg || '';
      ok.disabled = !v || !!msg;
    };
    input.addEventListener('input', sync);
    sync();
    return h(
      'form',
      {
        onsubmit: (e) => {
          e.preventDefault();
          if (!ok.disabled) close(input.value.trim());
        },
      },
      h('label', { class: 'dlg-field' }, h('span', { class: 'dlg-label', text: label }), input, hint ? h('span', { class: 'dlg-hint', text: hint }) : null, err),
      h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), ok),
    );
  });
}

// Radio choice; resolves to the chosen value or null. A choice may be
// {disabled: true, hint} (shown, not selectable); `initial` preselects.
export function choiceDialog({ title, message, choices, okText = t('common.ok'), cancelText = t('common.cancel'), initial = null, body = null, route = true }) {
  return openDialog(title, (close) => {
    const name = 'c' + Math.random().toString(36).slice(2);
    // the choices are named by the question (the message), else the title (T6 #10)
    const msgId = `${name}-q`;
    const first = initial != null ? choices.findIndex((c) => c.value === initial && !c.disabled) : -1;
    // the focus starts on the preselected choice, else on the first one
    // that can be chosen, else (none can: R6) on cancel
    const focusAt = first >= 0 ? first : choices.findIndex((c) => !c.disabled);
    const ok = h('button', { type: 'submit', class: 'btn btn-primary', disabled: first < 0 }, okText);
    const radios = choices.map((c, i) =>
      h(
        'label',
        { class: 'dlg-choice' + (c.disabled ? ' is-disabled' : '') },
        h('input', { type: 'radio', name, value: String(i), checked: i === first, disabled: c.disabled, autofocus: i === focusAt, onchange: () => (ok.disabled = false) }),
        h('span', { class: 'dlg-choice-text' }, h('span', { class: 'dlg-choice-label' }, c.label), c.hint ? h('span', { class: 'dlg-choice-hint' }, c.hint) : null),
      ),
    );
    return h(
      'form',
      {
        onsubmit: (e) => {
          e.preventDefault();
          const sel = e.currentTarget.querySelector(`input[name="${name}"]:checked`);
          if (sel) close(choices[Number(sel.value)].value);
        },
      },
      message ? h('p', { class: 'dlg-msg', id: msgId, text: message }) : null,
      body,
      h('div', { class: 'dlg-choices', role: 'radiogroup', 'aria-labelledby': message ? msgId : null, 'aria-label': message ? null : title }, radios),
      h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', autofocus: focusAt < 0, onclick: () => close(null) }, cancelText), ok),
    );
  }, { route });
}

// A row of buttons; resolves to the clicked action's value or null.
export function actionsDialog({ title, message, actions, cancelText = t('common.cancel') }) {
  return openDialog(title, (close) =>
    h(
      'div',
      null,
      message ? h('p', { class: 'dlg-msg', text: message }) : null,
      h(
        'div',
        { class: 'dlg-actions' },
        h('button', { type: 'button', class: 'btn', autofocus: true, onclick: () => close(null) }, cancelText),
        actions.map((a) => h('button', { type: 'button', class: a.danger ? 'btn btn-danger-solid' : 'btn btn-primary', onclick: () => close(a.value) }, a.label)),
      ),
    ),
  );
}

// ---- <details class="menu"> dropdowns (transcript sentence menu) ----

export function installMenuCloser() {
  document.addEventListener('click', (e) => {
    for (const d of document.querySelectorAll('details.menu[open]')) if (!d.contains(e.target)) d.open = false;
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    for (const d of document.querySelectorAll('details.menu[open]')) {
      d.open = false;
      const s = d.querySelector('summary');
      if (s && d.contains(document.activeElement)) s.focus();
    }
  });
}

export function closeMenuOf(el) {
  const d = el.closest('details.menu');
  if (d) d.open = false;
}

// ---- clipboard ----

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = h('textarea', { style: { position: 'fixed', top: '0', left: '0', opacity: '0' } });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}

// Starts a download of url (export files) without leaving the page.
export function download(url, name) {
  const a = h('a', { href: url, download: name || '' });
  document.body.append(a);
  a.click();
  a.remove();
}

// ---- file drop ----

function hasFiles(e) {
  return !!e.dataTransfer && Array.from(e.dataTransfer.types || []).includes('Files');
}

// Makes `el` accept dropped files; adds class "drag-over" while hovering.
export function makeDropTarget(el, onFiles) {
  let depth = 0;
  el.addEventListener('dragenter', (e) => {
    if (!hasFiles(e)) return;
    depth++;
    el.classList.add('drag-over');
  });
  el.addEventListener('dragover', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  el.addEventListener('dragleave', (e) => {
    if (!hasFiles(e)) return;
    depth = Math.max(0, depth - 1);
    if (depth === 0) el.classList.remove('drag-over');
  });
  el.addEventListener('drop', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    depth = 0;
    el.classList.remove('drag-over');
    // desktop app: dropped files carry their local path (SPEC-v3 V1.4, api.js)
    const files = attachDroppedPaths(Array.from(e.dataTransfer.files || []));
    if (files.length) onFiles(files);
  });
}

// A hidden <input type=file> and a function that opens it. In the desktop
// app open() shows the system dialog instead (local files, copied by the
// server: SPEC-v3 V1.4); cancelling it does nothing.
export function filePicker({ accept, multiple, onFiles }) {
  const input = h('input', {
    type: 'file',
    accept,
    multiple,
    class: 'visually-hidden',
    tabindex: '-1',
    'aria-hidden': 'true',
    onchange: () => {
      const files = Array.from(input.files || []);
      input.value = '';
      if (files.length) onFiles(files);
    },
  });
  const open = !DESKTOP
    ? () => input.click()
    : async () => {
        const picked = await pickLocalFiles({ accept, multiple });
        if (picked === null) input.click(); // no system dialog: the browser's
        else if (picked.length) onFiles(multiple ? picked : picked.slice(0, 1));
      };
  return { input, open };
}

// A visible drop zone with a keyboard-accessible "choose files" button.
// target=false when an enclosing element is the drop target instead.
export function dropzone({ text, accept, multiple, buttonText = t('common.choose_files'), onFiles, target = true, compact = false }) {
  const picker = filePicker({ accept, multiple, onFiles });
  const zone = h(
    'div',
    { class: 'dropzone' + (compact ? ' dropzone-compact' : '') },
    icon('upload', { size: 16, class: 'dropzone-icon' }),
    h('span', { class: 'dropzone-text', text }),
    h('button', { type: 'button', class: 'btn btn-sm', onclick: picker.open }, buttonText),
    picker.input,
  );
  if (target) makeDropTarget(zone, onFiles);
  return zone;
}

// Stops the browser from navigating away when a file is dropped outside a
// drop zone (which would leave the app).
export function preventStrayDrops() {
  window.addEventListener('dragover', (e) => {
    if (hasFiles(e)) e.preventDefault();
  });
  window.addEventListener('drop', (e) => {
    if (hasFiles(e)) e.preventDefault();
  });
}

// <input> types that take no typed text: keyboard shortcuts stay active on
// them (a slider just dragged, a checkbox just clicked, SPEC-v2 P6.3).
const NON_TEXT_INPUTS = new Set(['button', 'checkbox', 'color', 'file', 'hidden', 'image', 'radio', 'range', 'reset', 'submit']);

// True when keys typed at el go into a text field: a text-like <input>
// (text, search, number, date, …), a <textarea>, a <select> (it selects
// options by typed letters) or editable content. Sliders, checkboxes,
// radios and buttons are not text fields.
export function isTypingTarget(el) {
  if (!el || el.nodeType !== 1) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') return !NON_TEXT_INPUTS.has(String(el.type || 'text').toLowerCase());
  return false;
}
