// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The player's transcript stream (SPEC-v2 P5): one entry per sentence
// (time, English = text, Chinese), the current sentence highlighted and
// kept in the middle; manual scrolling pauses that for 5 seconds and shows
// player.side.back_to_current; a click jumps to the sentence (the play state does not
// change); search in both languages with Enter / Shift+Enter moving between
// results (scrolling only); marks for modified sentences (thin line) and
// pending review changes (amber, hover card with before / after / reason);
// the sentence context menu.
// ext web3 (user request 2026-10-27 no. 5): with chapters, a heading
// before each chapter's first sentence (time and title; a click jumps to
// the chapter start) that folds the chapter's sentences away (its chevron,
// or the left / right arrow keys on the heading); "fold to chapters" in the
// search bar folds or unfolds them all, so the stream reads as a table of
// contents. The heading of the current chapter is marked, and stands in for
// the current sentence while that is folded away; a search result in a
// folded chapter unfolds it. Up / down move through the headings and the
// visible sentences. Without chapters the stream is as before.

import { h, clear } from './dom.js';
import { t } from './i18n.js';
import { pIcon } from './player_icons.js';
import { bindContextMenu, menuButton } from './menu.js';
import { diffView } from './widgets.js';
import { fmtPlayerTime } from './player_util.js';
import { lastStartingBefore } from './player_subs.js';
import { zh as zhText, zhLang, zhQuery, markNeedles } from './zh_display.js';

const MANUAL_PAUSE_MS = 5000; // P5

export function isModified(s) {
  return s.human_text != null || (s.asr_text != null && s.text !== s.asr_text);
}

export function isPending(s) {
  return !!(s.patch && s.patch.review === 'pending');
}

// opts: {onSeek(sentence), menuItems(sentence) → items, duration() → ms}
export function createTranscript(opts) {
  let sentences = [];
  let rows = [];
  let current = -1;
  let display = 'both';
  let order = 'en';
  let manualUntil = 0;
  let searchHold = false; // the user moved through search results: no auto-scroll until cleared
  let ours = false; // the list is being scrolled by us (auto-scroll), not by the user
  let oursTimer = 0;
  let query = '';
  let zhq = zhQuery(''); // the query over the Chinese (P14.6: Traditional terms find the stored Simplified)
  let hits = [];
  let hitPos = -1;
  let focusIdx = 0;
  let resumeTimer = 0;
  let searchTimer = 0;
  let tabStop = null; // the item of the list in the tab order (roving)
  // chapters (ext web3)
  let chapters = [];
  let chapFns = null; // {title(c), lang(c), onSeek(ms)}
  let chapOf = []; // sentence index -> chapter index (-1 before the first chapter)
  let heads = []; // chapter index -> heading element
  const folded = new Set(); // start_ms of the folded chapters
  let curChap = -1;
  // whether the times were drawn as h:mm:ss (fmtPlayerTime: an hour of media or more)
  const hourly = () => (Number(opts.duration()) || 0) >= 3600000;
  let drawnHourly = false;

  const input = h('input', {
    type: 'search',
    class: 'input pl-search-input',
    placeholder: t('player.side.search_placeholder'),
    'aria-label': t('player.side.search_label'),
    spellcheck: 'false',
    autocomplete: 'off',
  });
  const count = h('span', { class: 'pl-search-count', 'aria-live': 'polite' });
  const prevBtn = h('button', { type: 'button', class: 'icon-btn pl-search-nav', 'aria-label': t('player.side.search_prev'), title: t('player.side.search_prev'), onclick: () => moveHit(-1) }, pIcon('chevron-down', { size: 15, class: 'is-up' }));
  const nextBtn = h('button', { type: 'button', class: 'icon-btn pl-search-nav', 'aria-label': t('player.side.search_next'), title: t('player.side.search_next'), onclick: () => moveHit(1) }, pIcon('chevron-down', { size: 15 }));
  const foldAllBtn = h('button', { type: 'button', class: 'icon-btn pl-search-nav pl-fold-all', hidden: true, 'aria-pressed': 'false', onclick: () => foldAll() }, pIcon('list-collapse', { size: 15 }));
  const search = h('div', { class: 'pl-search' }, pIcon('search', { size: 15, class: 'pl-search-icon' }), input, count, prevBtn, nextBtn, foldAllBtn);
  const list = h('div', { class: 'pl-list', role: 'list', 'aria-label': t('player.side.title'), tabindex: '-1' });
  const backBtn = h('button', { type: 'button', class: 'btn btn-sm pl-back-current', hidden: true, onclick: () => backToCurrent() }, pIcon('locate', { size: 14 }), h('span', { class: 'btn-label' }, t('player.side.back_to_current')));
  const card = h('div', { class: 'pl-hovercard', role: 'tooltip', hidden: true });
  const el = h('div', { class: 'pl-transcript', dataset: { display, order } }, search, list, backBtn, card);

  // ---- rows ----

  function textNodes(text, q) {
    if (!q || !text) return [text || ''];
    const lower = text.toLowerCase();
    const out = [];
    let i = 0;
    for (;;) {
      const j = lower.indexOf(q, i);
      if (j < 0) break;
      if (j > i) out.push(text.slice(i, j));
      out.push(h('mark', null, text.slice(j, j + q.length)));
      i = j + q.length;
    }
    out.push(text.slice(i));
    return out;
  }

  function fillText(row, s) {
    const en = row.querySelector('.pl-row-en');
    const zh = row.querySelector('.pl-row-zh');
    clear(en).append(...textNodes(s.text, query));
    clear(zh);
    if (s.zh) zh.append(...markNeedles(zhText(s.zh), query ? zhq.needles : [], (x) => h('mark', null, x)));
    else zh.append(h('span', { class: 'pl-row-none' }, t('player.side.untranslated')));
    row.classList.toggle('no-zh', !s.zh);
    row.classList.toggle('is-stale-zh', s.zh_status === 'stale');
  }

  function renderRow(s, i) {
    const pending = isPending(s);
    const row = h(
      'div',
      {
        class: 'pl-row' + (isModified(s) ? ' is-mod' : '') + (pending ? ' is-pending' : ''),
        role: 'listitem',
        tabindex: i === focusIdx ? '0' : '-1',
        // fkey: a row redrawn while its menu or a copy had it (a refresh)
        // is found again, as other controls are (menu.js, dom.js)
        dataset: { idx: String(i), fkey: `pl:s:${s.idx}` },
      },
      h('span', { class: 'pl-row-time' }, fmtPlayerTime(s.start_ms, opts.duration())),
      h('div', { class: 'pl-row-text' }, h('p', { class: 'pl-row-en', lang: 'en' }), h('p', { class: 'pl-row-zh', lang: zhLang() })),
      pending
        ? h(
            'button',
            { type: 'button', class: 'pl-row-pend', tabindex: '-1', 'aria-label': t('player.side.pending_mark'), dataset: { idx: String(i) } },
            h('span', { class: 'pl-pend-glyph', 'aria-hidden': 'true' }),
          )
        : null,
      moreButton(i),
    );
    fillText(row, s);
    return row;
  }

  // T22 G15 (round 8 WL-03): the sentence's menu ("修改原文", "重翻此句", …)
  // from a "⋯" on the row, as the lecture page's transcript has it, not only
  // by the right click or Shift+F10. The list is one Tab stop (roving): the
  // button is no Tab stop of its own, and keyboard users keep Shift+F10 / the
  // menu key on the row. A click opens the menu without playing the
  // sentence (menuButton stops the click; the list's keys leave it alone).
  // Shown faintly, fully on the current sentence and on hover (style.css).
  function moreButton(i) {
    const s = sentences[i];
    const b = h(
      'button',
      { type: 'button', class: 'icon-btn s-more-btn pl-row-more', tabindex: '-1', title: t('transcript.more_tip'), 'aria-label': t('transcript.more_aria', { n: s.idx + 1 }), dataset: { fkey: `pl:more:${s.idx}` } },
      pIcon('ellipsis', { size: 16 }),
    );
    return menuButton(b, () => opts.menuItems(sentences[i]));
  }

  function render() {
    hideCard();
    drawnHourly = hourly();
    clear(list);
    rows = sentences.map(renderRow);
    heads = [];
    if (!rows.length) list.append(h('p', { class: 'pl-list-empty' }, t('player.side.empty')));
    else list.append(...rows);
    tabStop = rows[focusIdx] || null;
    if (current >= 0 && rows[current]) markCur(rows[current], true);
    placeChapters();
    runSearch(false);
  }

  // One sentence redrawn in place (an edit came back, a refresh): the
  // current-sentence mark, the search marks, a folded chapter, the tab stop
  // and the keyboard focus stay.
  function redrawRow(i, s) {
    sentences[i] = s;
    const old = rows[i];
    const row = renderRow(s, i);
    if (i === current) markCur(row, true);
    if (hits.includes(i)) row.classList.add('is-hit');
    if (hitPos >= 0 && hits[hitPos] === i) row.classList.add('is-hit-cur');
    row.hidden = old.hidden; // inside a folded chapter
    if (tabStop === old) tabStop = row;
    row.setAttribute('tabindex', tabStop === row ? '0' : '-1');
    if (cardFor === i) hideCard();
    const focused = old.contains(document.activeElement);
    old.replaceWith(row);
    rows[i] = row;
    if (focused) row.focus({ preventScroll: true });
  }

  // The sentence playing (T23 general 17): highlighted, and said to a
  // screen reader (aria-current).
  function markCur(row, on) {
    row.classList.toggle('is-cur', on);
    if (on) row.setAttribute('aria-current', 'true');
    else row.removeAttribute('aria-current');
  }

  // ---- chapters (ext web3) ----

  // placeChapters(): the headings between the rows, from `chapters` (the
  // rows stay; old headings are replaced).
  function placeChapters() {
    for (const head of heads) if (head) head.remove();
    heads = [];
    // a heading that held the tab stop is gone: back to the focused row
    if (tabStop && !tabStop.classList.contains('pl-row')) {
      tabStop = null;
      if (rows[focusIdx]) setTabStop(rows[focusIdx]);
    }
    chapOf = sentences.map((s) => lastStartingBefore(chapters, s.start_ms));
    // not while the list shows its skeleton or an error (the rows are not in it)
    const has = !!(chapFns && chapters.length && rows.length && rows[0].parentNode === list);
    foldAllBtn.hidden = !has;
    el.classList.toggle('has-chapters', has);
    if (!has) {
      for (const r of rows) r.hidden = false;
      curChap = -1;
      syncFoldAll();
      return;
    }
    const firstRow = new Map(); // chapter index -> index of its first sentence
    chapOf.forEach((k, i) => {
      if (k >= 0 && !firstRow.has(k)) firstRow.set(k, i);
    });
    // folds of chapters that are gone (regenerated) are forgotten
    const starts = new Set(chapters.map((c) => c.start_ms));
    for (const x of [...folded]) if (!starts.has(x)) folded.delete(x);
    for (let k = 0; k < chapters.length; k++) {
      const head = renderHead(k);
      heads[k] = head;
      // before the chapter's first sentence; a chapter without one before
      // the next chapter's (or at the end)
      let at = firstRow.get(k);
      for (let j = k + 1; at == null && j < chapters.length; j++) at = firstRow.get(j);
      const before = at != null ? rows[at] : null;
      if (before) list.insertBefore(head, before);
      else list.append(head);
    }
    // R78 low 1: sentences before the first chapter have no heading to fold
    // them (chapters made again may start later than the folded ones did)
    chapOf.forEach((k, i) => {
      if (k < 0 && rows[i]) rows[i].hidden = false;
    });
    for (let k = 0; k < chapters.length; k++) applyFold(k);
    if (tabStop && tabStop.hidden) {
      const k = chapOf[Number(tabStop.dataset.idx)];
      if (heads[k]) setTabStop(heads[k].querySelector('.pl-chap-go'));
    }
    curChap = current >= 0 ? chapOf[current] : -1;
    markCurChapter();
    syncFoldAll();
  }

  function chapterCount(k) {
    let n = 0;
    for (const c of chapOf) if (c === k) n++;
    return n;
  }

  function renderHead(k) {
    const c = chapters[k];
    const title = chapFns.title(c);
    const time = fmtPlayerTime(c.start_ms, opts.duration());
    const fold = h('button', { type: 'button', class: 'pl-chap-fold', tabindex: '-1', dataset: { chap: String(k) } }, pIcon('chevron-down', { size: 14 }));
    const go = h(
      'button',
      {
        type: 'button',
        class: 'pl-chap-go',
        tabindex: '-1',
        dataset: { chap: String(k) },
        'aria-label': t('player.chap.heading', { n: k + 1, title, time }),
        title: t('player.chap.jump'),
      },
      h('span', { class: 'pl-chap-time' }, time),
      h('span', { class: 'pl-chap-title', lang: chapFns.lang(c) }, title),
      h('span', { class: 'pl-chap-count' }, t('player.chap.count', { n: chapterCount(k) })),
    );
    return h('div', { class: 'pl-chap', role: 'listitem', dataset: { chap: String(k) } }, fold, go);
  }

  function isFolded(k) {
    return !!chapters[k] && folded.has(chapters[k].start_ms);
  }

  function applyFold(k) {
    const head = heads[k];
    if (!head) return;
    const on = isFolded(k);
    head.classList.toggle('is-folded', on);
    const go = head.querySelector('.pl-chap-go');
    const fold = head.querySelector('.pl-chap-fold');
    go.setAttribute('aria-expanded', String(!on));
    const label = t(on ? 'player.chap.unfold' : 'player.chap.fold');
    fold.setAttribute('aria-label', label);
    fold.title = label;
    chapOf.forEach((c, i) => {
      if (c === k && rows[i]) rows[i].hidden = on;
    });
  }

  function setFold(k, on) {
    if (!chapters[k]) return;
    if (on) folded.add(chapters[k].start_ms);
    else folded.delete(chapters[k].start_ms);
    applyFold(k);
    // the tab stop never hides: it moves to the heading
    if (tabStop && tabStop.hidden && heads[k]) setTabStop(heads[k].querySelector('.pl-chap-go'));
    markCurChapter();
    syncFoldAll();
    hideCard();
    requestAnimationFrame(syncBackButton);
  }

  function allFolded() {
    return heads.length > 0 && chapters.every((c, k) => !heads[k] || folded.has(c.start_ms));
  }

  function syncFoldAll() {
    const on = allFolded();
    foldAllBtn.setAttribute('aria-pressed', String(on));
    foldAllBtn.classList.toggle('is-on', on);
    const label = t(on ? 'player.chap.unfold_all' : 'player.chap.fold_all');
    foldAllBtn.setAttribute('aria-label', label);
    foldAllBtn.title = label;
  }

  function foldAll() {
    const on = !allFolded();
    for (let k = 0; k < chapters.length; k++) {
      if (on) folded.add(chapters[k].start_ms);
      else folded.delete(chapters[k].start_ms);
      applyFold(k);
    }
    if (tabStop && tabStop.hidden) {
      const k = chapOf[Number(tabStop.dataset.idx)];
      if (heads[k]) setTabStop(heads[k].querySelector('.pl-chap-go'));
    }
    markCurChapter();
    syncFoldAll();
    hideCard();
    // back to where the playback is (its heading while folded)
    manualUntil = 0;
    searchHold = false;
    const target = curTarget();
    if (target) scrollToRow(target, false);
    requestAnimationFrame(syncBackButton);
  }

  // The current chapter's heading is marked; it stands in for the current
  // sentence while that is folded away.
  function markCurChapter() {
    heads.forEach((head, k) => {
      if (!head) return;
      head.classList.toggle('is-cur', k === curChap);
      head.classList.toggle('is-cur-folded', k === curChap && isFolded(k));
    });
  }

  // What "the current sentence" is on screen: its row, or its chapter's
  // heading while the chapter is folded.
  function curTarget() {
    const row = rows[current];
    if (!row) return null;
    if (!row.hidden) return row;
    return heads[chapOf[current]] || null;
  }

  // ---- current sentence and auto-scroll (P5) ----

  function inView(row) {
    const a = list.getBoundingClientRect();
    const b = row.getBoundingClientRect();
    return b.bottom > a.top + 4 && b.top < a.bottom - 4 && a.height > 0;
  }

  function scrollToRow(row, smooth) {
    if (!row || !list.clientHeight) return;
    const top = row.offsetTop - list.clientHeight / 2 + row.offsetHeight / 2;
    ours = true;
    clearTimeout(oursTimer);
    oursTimer = setTimeout(() => (ours = false), smooth ? 1500 : 250);
    const reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    list.scrollTo({ top: Math.max(0, top), behavior: smooth && !reduced ? 'smooth' : 'auto' });
  }

  function autoScrollAllowed() {
    return performance.now() >= manualUntil && !searchHold;
  }

  function syncBackButton() {
    const row = curTarget();
    backBtn.hidden = !row || !list.clientHeight || inView(row);
  }

  function markManual() {
    manualUntil = performance.now() + MANUAL_PAUSE_MS;
    clearTimeout(resumeTimer);
    resumeTimer = setTimeout(syncBackButton, MANUAL_PAUSE_MS + 50);
    requestAnimationFrame(syncBackButton);
  }

  function backToCurrent() {
    manualUntil = 0;
    searchHold = false;
    const row = curTarget();
    if (row) scrollToRow(row, true);
    backBtn.hidden = true;
  }

  list.addEventListener('wheel', markManual, { passive: true });
  list.addEventListener('pointerdown', (e) => {
    if (e.target === list) markManual(); // the scrollbar
  });
  list.addEventListener('scroll', () => {
    if (!ours) markManual();
    syncBackButton();
    hideCard();
  });
  list.addEventListener('scrollend', () => {
    if (!ours) return;
    clearTimeout(oursTimer);
    oursTimer = setTimeout(() => (ours = false), 30);
  });

  // ---- clicks, keyboard, menu ----

  list.addEventListener('click', (e) => {
    const fold = e.target.closest('.pl-chap-fold');
    if (fold) {
      const k = Number(fold.dataset.chap);
      setFold(k, !isFolded(k));
      return;
    }
    const go = e.target.closest('.pl-chap-go');
    if (go) {
      const k = Number(go.dataset.chap);
      setTabStop(go);
      if (chapters[k] && chapFns) chapFns.onSeek(chapters[k].start_ms);
      return;
    }
    const pend = e.target.closest('.pl-row-pend');
    if (pend) {
      e.stopPropagation();
      showCard(Number(pend.dataset.idx), pend);
      return;
    }
    const row = e.target.closest('.pl-row');
    if (!row) return;
    const sel = window.getSelection && window.getSelection();
    if (sel && sel.toString().trim() && row.contains(sel.anchorNode)) return; // selecting text to copy
    setFocusRow(Number(row.dataset.idx), false);
    opts.onSeek(sentences[Number(row.dataset.idx)]);
  });

  list.addEventListener('keydown', (e) => {
    // a row's "⋯" has the focus (after its menu, or a click): Enter, Space
    // and ↓ are the button's (its menu), not the row's playback or move
    if (e.target.closest && e.target.closest('.pl-row-more')) return;
    const item = e.target.closest && e.target.closest('.pl-row, .pl-chap-go');
    if (!item) return;
    const head = item.classList.contains('pl-chap-go');
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      // the headings and the visible sentences, in order
      const items = [...list.querySelectorAll('.pl-row:not([hidden]):not(.is-skel), .pl-chap-go')];
      const j = items.indexOf(item) + (e.key === 'ArrowDown' ? 1 : -1);
      const next = items[Math.max(0, Math.min(items.length - 1, j))];
      if (next) focusItem(next);
      markManual();
    } else if (head && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
      e.preventDefault();
      setFold(Number(item.dataset.chap), e.key === 'ArrowLeft');
    } else if (!head && e.key === 'Enter') {
      e.preventDefault();
      opts.onSeek(sentences[Number(item.dataset.idx)]);
    }
  });

  function setTabStop(item) {
    if (tabStop && tabStop !== item) tabStop.tabIndex = -1;
    tabStop = item || null;
    if (tabStop) tabStop.tabIndex = 0;
    if (item && item.classList.contains('pl-row')) focusIdx = Number(item.dataset.idx);
  }

  function focusItem(item) {
    setTabStop(item);
    item.focus({ preventScroll: true });
    if (!inView(item)) item.scrollIntoView({ block: 'nearest' });
  }

  function setFocusRow(i, focus) {
    focusIdx = i;
    if (!rows[i]) return;
    setTabStop(rows[i]);
    if (focus) focusItem(rows[i]);
  }

  bindContextMenu(list, (e) => {
    const row = e.target.closest && e.target.closest('.pl-row');
    if (!row) return null;
    const sel = window.getSelection && window.getSelection();
    if (e.type === 'contextmenu' && sel && sel.toString().trim() && row.contains(sel.anchorNode)) return null; // browser menu to copy the selection
    return opts.menuItems(sentences[Number(row.dataset.idx)]);
  });

  // ---- pending change card (P5: before / after / reason on hover) ----

  let cardFor = -1;
  function showCard(i, anchor) {
    const s = sentences[i];
    if (!s || !s.patch) return;
    cardFor = i;
    clear(card).append(
      h('div', { class: 'pl-hc-title' }, h('span', { class: 'pl-pend-glyph', 'aria-hidden': 'true' }), t('player.side.pending_title')),
      h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.before')), h('span', { class: 'pl-hc-v', lang: 'en' }, s.asr_text)),
      h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.after')), h('span', { class: 'pl-hc-v', lang: 'en' }, diffView(s.asr_text, s.text))),
      s.patch.reason ? h('div', { class: 'pl-hc-row' }, h('span', { class: 'pl-hc-k' }, t('player.side.reason')), h('span', { class: 'pl-hc-v', lang: 'en' }, s.patch.reason)) : null,
      h('p', { class: 'pl-hc-foot' }, t('player.side.pending_foot')),
    );
    card.hidden = false;
    const r = anchor.getBoundingClientRect();
    const host = el.getBoundingClientRect();
    const cw = card.offsetWidth;
    const ch = card.offsetHeight;
    let top = r.bottom + 6;
    if (top + ch > window.innerHeight - 8) top = Math.max(8, r.top - ch - 6);
    card.style.top = `${top - host.top}px`;
    card.style.left = `${Math.max(8, Math.min(host.width - cw - 8, r.right - host.left - cw))}px`;
  }

  function hideCard() {
    cardFor = -1;
    card.hidden = true;
  }

  list.addEventListener('mouseover', (e) => {
    const pend = e.target.closest && e.target.closest('.pl-row-pend');
    if (pend && Number(pend.dataset.idx) !== cardFor) showCard(Number(pend.dataset.idx), pend);
  });
  list.addEventListener('mouseout', (e) => {
    const pend = e.target.closest && e.target.closest('.pl-row-pend');
    if (pend && !(e.relatedTarget && (pend.contains(e.relatedTarget) || card.contains(e.relatedTarget)))) hideCard();
  });
  card.addEventListener('mouseleave', hideCard);
  list.addEventListener('focusin', (e) => {
    // (not for its "⋯": the card would cover the menu it opens)
    const row = e.target.closest && !e.target.closest('.pl-row-more') && e.target.closest('.pl-row.is-pending');
    if (row) showCard(Number(row.dataset.idx), row.querySelector('.pl-row-pend'));
    else hideCard();
  });
  list.addEventListener('focusout', (e) => {
    if (!list.contains(e.relatedTarget)) hideCard();
  });

  // ---- search (P5) ----

  function matches(s, q) {
    return (s.text || '').toLowerCase().includes(q) || zhq.test(s.zh);
  }

  // Searched again when the Simplified form of a Traditional query is ready.
  const searchAgain = () => runSearch(false);

  function runSearch(scroll) {
    const q = input.value.trim().toLowerCase();
    const changed = q !== query;
    const before = new Set(hits);
    query = q;
    zhq = zhQuery(q, searchAgain);
    hits = q ? sentences.map((s, i) => (matches(s, q) ? i : -1)).filter((i) => i >= 0) : [];
    for (const i of new Set([...before, ...hits])) {
      if (rows[i]) {
        rows[i].classList.toggle('is-hit', hits.includes(i));
        rows[i].classList.remove('is-hit-cur');
        fillText(rows[i], sentences[i]);
      }
    }
    if (changed) {
      hitPos = -1;
      if (!q) searchHold = false;
    } else if (hitPos >= hits.length) hitPos = hits.length - 1;
    if (hitPos >= 0 && rows[hits[hitPos]]) rows[hits[hitPos]].classList.add('is-hit-cur');
    syncCount();
    if (scroll && hits.length && changed) moveHit(1);
  }

  function syncCount() {
    const has = !!query;
    count.textContent = !has ? '' : hits.length ? t('player.side.search_count', { i: hitPos >= 0 ? hitPos + 1 : 0, n: hits.length }) : t('player.side.search_none');
    count.classList.toggle('is-none', has && !hits.length);
    prevBtn.disabled = nextBtn.disabled = !hits.length;
  }

  // Enter / Shift+Enter: scroll the transcript only, the playback position
  // does not change (P5); a click on the result jumps there.
  function moveHit(dir) {
    if (!hits.length) return;
    if (hitPos >= 0 && rows[hits[hitPos]]) rows[hits[hitPos]].classList.remove('is-hit-cur');
    hitPos = hitPos < 0 ? (dir > 0 ? firstHitAfterCurrent() : lastHitBeforeCurrent()) : (hitPos + dir + hits.length) % hits.length;
    const row = rows[hits[hitPos]];
    if (row.hidden) setFold(chapOf[hits[hitPos]], false); // a result in a folded chapter unfolds it
    row.classList.add('is-hit-cur');
    searchHold = true;
    scrollToRow(row, false);
    setFocusRow(hits[hitPos], false);
    syncCount();
    requestAnimationFrame(syncBackButton);
  }

  function firstHitAfterCurrent() {
    const i = hits.findIndex((x) => x >= Math.max(0, current));
    return i >= 0 ? i : 0;
  }
  function lastHitBeforeCurrent() {
    for (let i = hits.length - 1; i >= 0; i--) if (hits[i] <= current) return i;
    return hits.length - 1;
  }

  input.addEventListener('input', () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => runSearch(true), 150);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault();
      clearTimeout(searchTimer);
      if (input.value.trim().toLowerCase() !== query) runSearch(false);
      moveHit(e.shiftKey ? -1 : 1);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      if (input.value) {
        input.value = '';
        runSearch(false);
        backToCurrent();
      } else input.blur();
    }
  });

  return {
    el,
    setSentences(next) {
      const fresh = next || [];
      // P2 hotspot 2: a "changed sentences" refresh, or the one after an
      // edit, reads every sentence again; rebuilding the whole list took
      // 0.5–1 s at 1500 sentences (x4). The same sentences at the same
      // times, with the rows on the page: only those that changed are
      // redrawn (their marks, the search and the focus stay, redrawRow).
      // Another number of sentences, or a time moved (the chapters follow
      // the times): the whole list, as before.
      // (R78 low 2: the times are "mm:ss", or "h:mm:ss" from an hour of
      // media on: when that changed, the whole column is drawn again)
      const same =
        rows.length > 0 &&
        rows.length === fresh.length &&
        rows[0].parentNode === list &&
        hourly() === drawnHourly &&
        fresh.every((s, i) => sentences[i] && s.idx === sentences[i].idx && s.start_ms === sentences[i].start_ms);
      if (same) {
        let changed = false;
        for (let i = 0; i < fresh.length; i++) {
          if (JSON.stringify(fresh[i]) === JSON.stringify(sentences[i])) continue;
          redrawRow(i, fresh[i]);
          changed = true;
        }
        sentences = fresh;
        if (changed && query) runSearch(false); // a changed text may match or not now
        return;
      }
      sentences = fresh;
      if (current >= sentences.length) current = -1;
      if (focusIdx >= sentences.length) focusIdx = 0;
      render();
    },
    setLoading() {
      clear(list).append(...Array.from({ length: 8 }, (_, i) => h('div', { class: 'pl-row is-skel', 'aria-hidden': 'true' }, h('span', { class: 'skel', style: { width: '36px' } }), h('div', { class: 'pl-row-text' }, h('div', { class: 'skel', style: { width: `${[92, 70, 84, 64][i % 4]}%` } }), h('div', { class: 'skel', style: { width: `${[60, 76, 52, 70][i % 4]}%` } })))));
    },
    setError(message, onRetry) {
      clear(list).append(
        h(
          'div',
          { class: 'pl-list-error', role: 'alert' },
          h('p', null, h('strong', null, t('player.side.load_failed')), ' ', message || ''),
          h('button', { type: 'button', class: 'btn btn-sm', onclick: onRetry }, t('common.retry')),
        ),
      );
    },
    setCurrent(i, { force = false } = {}) {
      if (i === current && !force) return;
      if (rows[current]) markCur(rows[current], false);
      const before = curTarget();
      current = i;
      const row = rows[i];
      if (row) markCur(row, true);
      // T23 general 17: Tab into the list lands on the sentence playing,
      // unless the keyboard is in the list already (its place stays)
      if (row && !row.hidden && !list.contains(document.activeElement)) setTabStop(row);
      const k = i >= 0 && i < chapOf.length ? chapOf[i] : -1;
      if (k !== curChap) {
        curChap = k;
        markCurChapter();
      }
      // a folded chapter's heading stays put while its sentences play
      const target = curTarget();
      if (target && (force || target !== before) && (force || autoScrollAllowed())) scrollToRow(target, !force);
      syncBackButton();
    },
    // Called when the transcript becomes visible again (it could not scroll while hidden).
    reveal() {
      requestAnimationFrame(() => {
        const row = curTarget();
        if (row && autoScrollAllowed()) scrollToRow(row, false);
        syncBackButton();
      });
    },
    // setChapters(list, {title(c), lang(c), onSeek(ms)}): the lecture's
    // chapters ([] for none); called again when they or the display change.
    setChapters(next, fns) {
      chapters = (next || []).slice().sort((a, b) => a.start_ms - b.start_ms);
      chapFns = fns || null;
      if (!rows.length || rows[0].parentNode !== list) return; // placed by the next render
      const keep = list.scrollTop;
      placeChapters();
      const target = curTarget();
      if (target && autoScrollAllowed()) scrollToRow(target, false);
      else list.scrollTop = keep;
      requestAnimationFrame(syncBackButton);
    },
    setDisplay(d, o) {
      display = d === 'off' ? 'both' : d; // P5: shares the display language; "off" only hides the subtitles
      order = o;
      el.dataset.display = display;
      el.dataset.order = order;
    },
    focusSearch() {
      input.focus();
      input.select();
    },
    updateSentence(s) {
      const i = sentences.findIndex((x) => x.idx === s.idx);
      if (i < 0 || !rows[i]) return;
      if (hourly() !== drawnHourly) {
        // the time format changed meanwhile: one format for the column
        sentences[i] = s;
        render();
        return;
      }
      redrawRow(i, s);
      // R78 medium 1: the edited text may match the search or not now; the
      // refresh that follows reads the same data and redraws nothing
      if (query) runSearch(false);
    },
    destroy() {
      clearTimeout(resumeTimer);
      clearTimeout(searchTimer);
      clearTimeout(oursTimer);
    },
  };
}
