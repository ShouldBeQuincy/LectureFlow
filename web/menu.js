// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Context menus (v1.1 A12.2): right click, Shift+F10 and the Menu key open
// them; Esc closes (one level), arrow keys move, → / ← open and close
// submenus (▸), Enter / Space activate. Items that are not available are not
// passed in at all (SPEC §9.8: unavailable actions are not rendered).
//
// Item: {label, icon?, hint?, desc?, danger?, onSelect?, items?: Item[], checked?, lang?} | 'sep'
// checked (true/false) makes the item a menuitemradio with a check mark;
// lang marks a label written in another language (fonts, SPEC-v2 P14.5);
// desc is a line of small text under the label (what the item is, T22 S4).
// Falsy entries are dropped, separators are collapsed, empty submenus removed.

import { h } from './dom.js';
import { icon } from './icons.js';

let root = null; // the open top-level menu element
let openedAt = 0;
let restoreFocus = null;
let descSeq = 0; // ids of the items' descriptions

function normalize(items) {
  const out = [];
  for (const it of items || []) {
    if (!it) continue;
    if (it === 'sep') {
      if (out.length && out[out.length - 1] !== 'sep') out.push('sep');
      continue;
    }
    if (it.items) {
      const sub = normalize(it.items);
      if (!sub.length) continue;
      out.push({ ...it, items: sub });
      continue;
    }
    out.push(it);
  }
  while (out.length && out[out.length - 1] === 'sep') out.pop();
  return out;
}

export function closeMenu({ focus = true } = {}) {
  if (!root) return;
  root = null;
  for (const m of document.querySelectorAll('.ctx-menu')) m.remove(); // with open submenus
  document.removeEventListener('pointerdown', onOutside, true);
  window.removeEventListener('resize', onResize);
  window.removeEventListener('blur', onDismiss);
  document.removeEventListener('scroll', onScroll, true);
  window.removeEventListener('hashchange', onDismiss);
  let back = restoreFocus;
  restoreFocus = null;
  // the invoking element may have been re-rendered meanwhile (SSE refresh)
  if (back && !back.isConnected && back.dataset && back.dataset.fkey) back = document.querySelector(`[data-fkey="${CSS.escape(back.dataset.fkey)}"]`);
  if (focus && back && back.isConnected && back.focus) back.focus({ preventScroll: true });
}

function onOutside(e) {
  if (root && !e.target.closest('.ctx-menu')) closeMenu({ focus: false });
}
function onDismiss() {
  closeMenu({ focus: false });
}
// A scroll of the page or a resized window closes the menu. When the focus
// is in it then (a menu opened from the keyboard, or one whose items the
// arrows or the pointer moved through), it goes back to the menu's opener
// (found again by its data-fkey when redrawn), without scrolling to it: left
// on the removed item it fell to the page's body, and the next Enter did
// nothing (T28 G2, as T21 M1). A focus elsewhere on the page stays where it
// is; so does it when the window loses the focus (blur), and on a press
// outside the menu (onOutside).
function focusInMenu() {
  const a = document.activeElement;
  return !!(a && a.closest && a.closest('.ctx-menu'));
}
function onResize() {
  closeMenu({ focus: focusInMenu() });
}
function onScroll(e) {
  if (root && !(e.target instanceof Node && e.target.closest && e.target.closest('.ctx-menu'))) closeMenu({ focus: focusInMenu() });
}

function itemsOf(menu) {
  return Array.from(menu.querySelectorAll(':scope > .ctx-item'));
}

function focusItem(menu, i) {
  const list = itemsOf(menu);
  if (!list.length) return;
  const n = (i + list.length) % list.length;
  list[n].focus();
}

// A menu is measured as it is laid out (offsetWidth / offsetHeight). Its
// rect is the menu as drawn, and while it opens the animation draws it 1.5%
// smaller (lf-pop): placed by that, it ended a few pixels past the 8px it
// keeps from the window's edges, and a menu too tall for the room beside
// its button passed for one that fits (T21 M3).
function place(menu, x, y, alignRightTo = null) {
  menu.style.left = '0px';
  menu.style.top = '0px';
  const w = menu.offsetWidth;
  const hgt = menu.offsetHeight;
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  let left = x;
  let top = y;
  if (left + w > vw - 8) left = alignRightTo != null ? alignRightTo - w : vw - 8 - w;
  if (top + hgt > vh - 8) top = Math.max(8, vh - 8 - hgt);
  menu.style.left = `${Math.max(8, left)}px`;
  menu.style.top = `${Math.max(8, top)}px`;
}

// T18e: where a menu opened from a button goes (rect: the button's
// getBoundingClientRect; height: the menu's, as laid out), 4px from the
// button and 8px from the window's edges: below it when it fits, else above
// it when it fits there, else on the larger side, as tall as that side
// allows (it then scrolls; maxHeight). It never covers its own button.
// openMenu tries menuBeside before it limits a menu. → {top, maxHeight}
export function anchoredMenuPlace(rect, height, viewHeight) {
  const below = viewHeight - 8 - (rect.bottom + 4);
  const above = rect.top - 4 - 8;
  if (height <= below) return { top: rect.bottom + 4, maxHeight: null };
  if (height <= above) return { top: rect.top - 4 - height, maxHeight: null };
  if (above > below) return { top: 8, maxHeight: Math.max(0, above) };
  return { top: rect.bottom + 4, maxHeight: Math.max(0, below) };
}

// T21 M2, R68: a menu with room neither below nor above its button, limited
// to the larger side, was often cut by a few pixels (9 of a 369px menu),
// with a scroll bar and its last item, "delete", out of sight. Shown whole
// over the button instead, it took the second click on the button, the one
// that closes the menu (and a double click), on one of its items. So a menu
// that fits the window (8px from its top and bottom edges) goes beside the
// button, whole and off it: 4px from the button, level with it, or as far up
// as it must go to end 8px from the bottom edge. A right-aligned menu (the
// "⋯" at the end of a row) goes to the button's left, else to its right; a
// left-aligned one to the right first. size: the menu's {width, height} as
// laid out; view: the window's. → {left, top}, or null when the menu is
// taller than the window or neither side is wide enough (a narrow window,
// an anchor as wide as the page): it is then limited (anchoredMenuPlace).
export function menuBeside(rect, size, view, alignRight) {
  if (size.height > view.height - 16) return null;
  const onLeft = rect.left - 4 - size.width;
  const onRight = rect.right + 4;
  const fitsLeft = onLeft >= 8;
  const fitsRight = onRight + size.width <= view.width - 8;
  if (!fitsLeft && !fitsRight) return null;
  const left = (alignRight ? fitsLeft : !fitsRight) ? onLeft : onRight;
  return { left, top: Math.max(8, Math.min(rect.top, view.height - 8 - size.height)) };
}

function buildMenu(items, level) {
  const menu = h('div', { class: 'ctx-menu', role: 'menu', tabindex: '-1', dataset: { level: String(level) } });
  let openSub = null;
  let hoverTimer = null;
  const closeSub = () => {
    clearTimeout(hoverTimer);
    if (openSub) {
      openSub.menu.remove();
      openSub.item.setAttribute('aria-expanded', 'false');
      openSub = null;
    }
  };
  const showSub = (item, sub, focusFirst) => {
    if (openSub && openSub.item === item) {
      if (focusFirst) focusItem(openSub.menu, 0);
      return;
    }
    closeSub();
    const m = buildMenu(sub, level + 1);
    document.body.append(m);
    const r = item.getBoundingClientRect();
    // A menu limited in height scrolls, and its scroll bar is between its
    // items and its edge: the submenu starts at the menu's outer edge there
    // (as laid out: where place() put it, and its width), not over the
    // scroll bar (T21 M3). Limited is the max-height openMenu gave it: the
    // scroll and client heights, rounded each, can differ by a pixel on a
    // menu that does not scroll (R68).
    const scrolls = !!menu.style.maxHeight;
    place(m, scrolls ? parseFloat(menu.style.left) + menu.offsetWidth : r.right - 2, r.top - 5, r.left + 2);
    item.setAttribute('aria-expanded', 'true');
    openSub = { item, menu: m, scrollTop: menu.scrollTop };
    m._parentItem = item;
    if (focusFirst) focusItem(m, 0);
  };
  menu._closeSub = closeSub;
  // The menu scrolled under its open submenu (the wheel), which would stay
  // where it was, beside nothing: the submenu closes, and a focus that was
  // in it goes to its item, without scrolling the menu back (T21 M3). A
  // scroll event without a move since the submenu opened is not one: the
  // scroll that brought the focused item into view is reported after the
  // key that opened its submenu.
  menu.addEventListener('scroll', () => {
    if (!openSub || menu.scrollTop === openSub.scrollTop) return;
    const item = openSub.item;
    const inSub = openSub.menu.contains(document.activeElement);
    closeSub();
    if (inSub) item.focus({ preventScroll: true });
  });
  for (const it of items) {
    if (it === 'sep') {
      menu.append(h('div', { class: 'ctx-sep', role: 'separator' }));
      continue;
    }
    const hasSub = !!it.items;
    const radio = typeof it.checked === 'boolean';
    // desc: the item's description, heard after its name (aria-describedby)
    // rather than part of it: a screen reader names each item briefly and
    // says what it is when it is focused; hidden from the name computed from
    // the content (a description may still point at hidden text)
    const descId = it.desc ? `ctx-desc-${++descSeq}` : null;
    const el = h(
      'button',
      {
        type: 'button',
        class: 'ctx-item' + (it.danger ? ' is-danger' : '') + (descId ? ' has-desc' : ''),
        role: radio ? 'menuitemradio' : 'menuitem',
        'aria-checked': radio ? String(it.checked) : undefined,
        tabindex: '-1',
        'aria-haspopup': hasSub ? 'menu' : undefined,
        'aria-expanded': hasSub ? 'false' : undefined,
        'aria-describedby': descId || undefined,
      },
      h('span', { class: 'ctx-icon' }, it.icon ? icon(it.icon, { size: 15 }) : radio && it.checked ? icon('check', { size: 15 }) : null),
      h('span', { class: 'ctx-label', lang: it.lang || undefined }, it.label),
      descId ? h('span', { class: 'ctx-desc', id: descId, 'aria-hidden': 'true' }, it.desc) : null,
      it.hint ? h('span', { class: 'ctx-hint' }, it.hint) : null,
      hasSub ? h('span', { class: 'ctx-arrow' }, icon('chevron-right', { size: 14 })) : null,
    );
    el.addEventListener('mouseenter', () => {
      el.focus({ preventScroll: true });
      clearTimeout(hoverTimer);
      if (hasSub) hoverTimer = setTimeout(() => showSub(el, it.items, false), 110);
      else hoverTimer = setTimeout(closeSub, 110);
    });
    el.addEventListener('click', (e) => {
      e.preventDefault();
      if (hasSub) {
        showSub(el, it.items, true);
        return;
      }
      closeMenu();
      if (it.onSelect) it.onSelect();
    });
    el.addEventListener('keydown', (e) => {
      const list = itemsOf(menu);
      const i = list.indexOf(el);
      if (e.key === 'ArrowDown') focusItem(menu, i + 1);
      else if (e.key === 'ArrowUp') focusItem(menu, i - 1);
      else if (e.key === 'Home') focusItem(menu, 0);
      else if (e.key === 'End') focusItem(menu, list.length - 1);
      else if (e.key === 'ArrowRight' && hasSub) showSub(el, it.items, true);
      else if (e.key === 'ArrowLeft' || (e.key === 'Escape' && level > 0)) {
        if (level === 0) return;
        const parent = menu._parentItem;
        const parentMenu = parent && parent.closest('.ctx-menu');
        if (parentMenu && parentMenu._closeSub) parentMenu._closeSub();
        if (parent) parent.focus();
      } else if (e.key === 'Escape') closeMenu();
      else if (e.key === 'Tab') closeMenu();
      else if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        el.click();
        return;
      } else return;
      e.preventDefault();
      e.stopPropagation();
    });
    menu.append(el);
  }
  menu.addEventListener('contextmenu', (e) => e.preventDefault());
  return menu;
}

// openMenu(items, {x, y} | {anchor: Element}, {returnFocus}) opens a menu at
// the pointer or below an anchor element. Returns false when nothing is shown.
export function openMenu(items, pos, opts = {}) {
  closeMenu({ focus: false });
  const list = normalize(items);
  if (!list.length) return false;
  restoreFocus = opts.returnFocus || document.activeElement;
  root = buildMenu(list, 0);
  document.body.append(root);
  if (pos.anchor) {
    const r = pos.anchor.getBoundingClientRect();
    const alignRight = opts.align === 'right';
    root.style.left = '0px';
    const size = { width: root.offsetWidth, height: root.offsetHeight };
    const at = anchoredMenuPlace(r, size.height, window.innerHeight);
    // room neither below nor above: beside the button when the window has
    // room for the whole menu there, else limited in height (R68)
    const side = at.maxHeight != null ? menuBeside(r, size, { width: window.innerWidth, height: window.innerHeight }, alignRight) : null;
    if (side) {
      place(root, side.left, side.top);
    } else {
      if (at.maxHeight != null) {
        root.style.maxHeight = `${at.maxHeight}px`;
        root.style.overflowY = 'auto';
      }
      // its width once limited: a scroll bar makes it wider, and a menu
      // right-aligned by its width without one stood past its button
      const w = root.offsetWidth;
      place(root, alignRight ? r.right - w : r.left, at.top, r.right);
    }
  } else {
    place(root, pos.x, pos.y);
  }
  openedAt = performance.now();
  // opts.focus: the item to open on (a list of choices opens on the one in
  // use, T21 M7); the first item otherwise, or when there is no such item
  const first = Number.isInteger(opts.focus) && opts.focus >= 0 && opts.focus < itemsOf(root).length ? opts.focus : 0;
  focusItem(root, first);
  document.addEventListener('pointerdown', onOutside, true);
  window.addEventListener('resize', onResize);
  window.addEventListener('blur', onDismiss);
  document.addEventListener('scroll', onScroll, true);
  window.addEventListener('hashchange', onDismiss);
  return true;
}

const TEXT_FIELDS = 'input:not([type="checkbox"]):not([type="radio"]):not([type="button"]), textarea, select, [contenteditable="true"]';

// The element a keyboard-opened menu is anchored to.
function keyboardAnchor(el, scope) {
  const a = document.activeElement;
  return a && scope.contains(a) ? a : el;
}

// bindContextMenu(el, getItems(event) → items) wires right click, Shift+F10
// and the Menu key on el (and anything inside it). getItems may return null
// to fall back to the browser menu (e.g. inside a text field).
export function bindContextMenu(el, getItems) {
  el.addEventListener('contextmenu', (e) => {
    if (e.target.closest(TEXT_FIELDS)) return; // keep the browser menu (copy / paste)
    // A keyboard-opened menu may be followed by the browser's own event.
    if (root && performance.now() - openedAt < 400) {
      e.preventDefault();
      return;
    }
    const items = getItems(e);
    if (!items) return;
    e.preventDefault();
    e.stopPropagation();
    const fromKeyboard = e.button !== 0 && e.clientX === 0 && e.clientY === 0;
    if (fromKeyboard) {
      const a = keyboardAnchor(el, el);
      openMenu(items, { anchor: a }, { returnFocus: a });
    } else {
      openMenu(items, { x: e.clientX, y: e.clientY }, { returnFocus: document.activeElement && el.contains(document.activeElement) ? document.activeElement : el });
    }
  });
  el.addEventListener('keydown', (e) => {
    const isMenuKey = e.key === 'ContextMenu' || (e.key === 'F10' && e.shiftKey);
    if (!isMenuKey || e.target.closest(TEXT_FIELDS)) return;
    const items = getItems(e);
    if (!items) return;
    e.preventDefault();
    e.stopPropagation();
    const a = keyboardAnchor(el, el);
    openMenu(items, { anchor: a }, { returnFocus: a });
  });
}

// A "⋯" style button that opens the same menu below itself.
export function menuButton(btnEl, getItems, align = 'right') {
  btnEl.setAttribute('aria-haspopup', 'menu');
  btnEl.addEventListener('click', (e) => {
    e.stopPropagation();
    if (root && root._owner === btnEl) {
      closeMenu();
      return;
    }
    if (openMenu(getItems(), { anchor: btnEl }, { returnFocus: btnEl, align }) && root) root._owner = btnEl;
  });
  btnEl.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (openMenu(getItems(), { anchor: btnEl }, { returnFocus: btnEl, align }) && root) root._owner = btnEl;
    }
  });
  return btnEl;
}
