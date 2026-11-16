// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Transcript tab (SPEC §9.6, v1.1 A12.2, A13): English | Chinese aligned by
// sentence, client-side search, a click on the timestamp plays the sentence
// (the playing sentence shows the shared audio bar), change marks for auto /
// absorb / decide / human edits (colour and shape) that expand to the word
// diff and the reason, qa_failed / stale states, 200 sentences per page, and
// the sentence menu (right click, the ⋯ button, Shift+F10): play · copy
// citation · copy English · copy Chinese · edit · reset · retranslate.
// "Transcribe only" (3.1.0, lecture_english.js): while the lecture has no
// Chinese and waits for a model provider, the Chinese column is hidden.

import { api } from './api.js';
import { h, clear, toast, preserveFocus, copyText, errorBlock, focusOpener } from './dom.js';
import { icon } from './icons.js';
import { bindContextMenu, menuButton } from './menu.js';
import * as Clip from './audiobar.js';
import { diffView } from './widgets.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { zh as zhText, zhLang, zhQuery, markNeedles } from './zh_display.js';
import { englishOnly } from './lecture_english.js';
import { canRetranslateCurrent, retranslateCurrent, heldToast } from './retranslate_current.js';
import { retranslateWatch, translationHeld } from './retranslate_watch.js';

const PAGE = 200;
const enc = encodeURIComponent;
const busyText = () => t('review.busy');

// ---- citation ([CSC1003 L03@12:34], v1.1 A11.3; same rules as internal/cite) ----

// The course part of a reference: the course code, or the first character of
// each word of the course name, upper-cased (words are runs of letters and
// digits; an apostrophe inside a word does not end it).
export function coursePart(course) {
  const code = course && course.code ? String(course.code).trim() : '';
  if (code) return code;
  let out = '';
  let inWord = false;
  for (const ch of (course && course.name) || '') {
    if (/[\p{L}\p{N}]/u.test(ch)) {
      if (!inWord) out += ch.toUpperCase();
      inWord = true;
    } else if (!(inWord && (ch === "'" || ch === '’'))) {
      inWord = false;
    }
  }
  return out || 'COURSE';
}

// mm:ss below one hour, h:mm:ss from one hour on; sub-seconds dropped.
export function refTime(ms) {
  const sec = Math.floor(Math.max(0, Number(ms) || 0) / 1000);
  const two = (n) => String(n).padStart(2, '0');
  if (sec >= 3600) return `${Math.floor(sec / 3600)}:${two(Math.floor(sec / 60) % 60)}:${two(sec % 60)}`;
  return `${two(Math.floor(sec / 60))}:${two(sec % 60)}`;
}

export function citeRef(course, label, ms) {
  return `${coursePart(course)} ${label}@${refTime(ms)}`;
}

/**
 * copyText that leaves the focus where it was (T23 serious 3). With the
 * clipboard API refused (a window without the focus) dom.js copies through
 * a hidden text field, which takes the focus and, removed, leaves it on the
 * page's body: a keyboard user lost their place in the sentences. When the
 * focus is lost so, it goes back as from a closed dialog (dom.js
 * focusOpener: the same element, else the one with its data-fkey).
 */
export async function copyKeepingFocus(text) {
  const back = focusOpener();
  const ok = await copyText(text);
  const a = document.activeElement;
  if (!a || a === document.body || !a.isConnected) back();
  return ok;
}

// ---- marks: colour + shape (A12.1: never colour alone) ----

// Text keys of the mark names.
const MARKS = {
  auto: 'transcript.mark.auto',
  absorb: 'transcript.mark.absorb',
  decide: 'transcript.mark.decide',
  human: 'transcript.mark.human',
};

// The mark's name on this page: transcript.mark.* everywhere — legend,
// row label, tooltips, detail line — so one screen uses one set of names
// (the short common.class.* names are for narrow columns elsewhere).
function markName(cls) {
  return MARKS[cls] ? t(MARKS[cls]) : U.CLASS_NAMES[cls] || cls;
}

function glyph(cls, hollow = false) {
  return h('span', { class: `mk-glyph mk-g-${cls}${hollow ? ' is-hollow' : ''}`, 'aria-hidden': 'true' });
}

export function mountTranscript(root, lecture, ctx) {
  let lec = lecture;
  let alive = true;
  let sentences = []; // sentences[i].idx === i
  let total = null;
  let loading = false;
  let loadFailed = false;
  let firstError = '';
  let refreshQueued = false;
  let query = '';
  const expanded = new Set();
  let editing = null; // {idx, draft}
  const rows = new Map(); // idx -> row element
  let chain = Promise.resolve();
  let searchTimer = null;
  let changedTimer = null;
  let barKey = null; // clip whose bar is shown (Clip.currentKey())
  const retranslated = retranslateWatch(); // "translate again" asked: the translation it had (T25 S3)

  // Paging and refreshes run one after another, never interleaved.
  const serial = (fn) => (chain = chain.then(fn, fn));

  const search = h('input', {
    type: 'search',
    class: 'input tx-search-input',
    placeholder: t('transcript.search.placeholder'),
    'aria-label': t('transcript.search.aria'),
    dataset: { fkey: 'tx:search' },
    oninput: () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => {
        query = search.value;
        if (query.trim() && !allLoaded()) loadAll();
        applyFilter(true);
      }, 150);
    },
    onkeydown: (e) => {
      if (e.key === 'Escape' && search.value) {
        e.preventDefault();
        search.value = '';
        query = '';
        applyFilter(true);
      }
    },
  });
  const info = h('span', { class: 'tx-info', 'aria-live': 'polite' });
  const busyEl = h('span', { class: 'tx-busy', hidden: true }, icon('clock', { size: 13 }), t('transcript.busy_bar'));
  const legend = h(
    'div',
    { class: 'tx-legend', 'aria-label': t('transcript.legend.aria') },
    ['auto', 'absorb', 'decide', 'human'].map((c) =>
      h('span', { class: `tx-legend-item mk-${c}`, title: c === 'human' ? t('transcript.legend.human_tip') : t('transcript.legend.tip', { name: markName(c), hint: U.CLASS_HINTS[c] }) }, glyph(c), markName(c)),
    ),
  );
  const toolbar = h(
    'div',
    { class: 'tx-toolbar' },
    h('label', { class: 'tx-search' }, icon('search', { size: 15, class: 'tx-search-icon' }), search),
    info,
    busyEl,
    legend,
  );
  const headRow = h('div', { class: 'srow s-head', 'aria-hidden': 'true' }, h('div', null, t('transcript.col.time')), h('div'), h('div', null, t('transcript.col.en')), h('div', null, t('transcript.col.zh')), h('div'));
  const sticky = h('div', { class: 'tx-sticky' }, toolbar, headRow);
  const listEl = h('div', { class: 'tx-list' });
  const stateEl = h('div', { class: 'tx-state' });
  const moreBtn = h('button', { type: 'button', class: 'btn btn-sm', onclick: () => loadMore(true) }, t('transcript.more.load'));
  const sentinel = h('div', { class: 'tx-sentinel' });
  const footer = h('div', { class: 'tx-footer' }, moreBtn);
  const panel = h('div', { class: 'card tx-card' }, sticky, stateEl, listEl, sentinel, footer);
  const syncColumns = () => panel.classList.toggle('is-en-only', englishOnly(lec)); // no empty Chinese column
  syncColumns();
  root.append(panel);

  const io = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting) && !query.trim()) loadMore(false);
  });
  io.observe(sentinel);

  const spec = (s) => ({ lectureId: lec.id, startMs: s.start_ms, endMs: s.end_ms });
  const keyOf = (s) => Clip.clipKey(spec(s));

  // The bar follows the clip being played; the timestamps show which one.
  const unClip = Clip.onChange(syncClip);

  function syncClip() {
    const key = Clip.currentKey();
    const playing = Clip.playingKey();
    for (const b of listEl.querySelectorAll('.ts.is-playing')) if (b.dataset.clip !== playing) setTs(b, false);
    if (playing) {
      const b = listEl.querySelector(`.ts[data-clip="${CSS.escape(playing)}"]`);
      if (b) setTs(b, true);
    }
    if (key === barKey) return;
    const old = barKey;
    barKey = key;
    for (const r of rows.values()) {
      if (r.dataset.clip === old) {
        removeBar(r);
        r.classList.remove('is-current');
      }
    }
    if (key) {
      for (const r of rows.values()) if (r.dataset.clip === key) addBar(r);
    }
  }

  function setTs(b, on) {
    b.classList.toggle('is-playing', on);
    b.replaceChildren(icon(on ? 'audio-lines' : 'play', { size: 12, class: 'ts-icon' }), h('span', null, b.dataset.time));
  }

  function addBar(row) {
    if (row.querySelector(':scope > .s-audio')) return;
    const s = sentences[Number(row.dataset.idx)];
    if (!s) return;
    row.classList.add('is-current');
    row.append(h('div', { class: 's-audio' }, Clip.audioBar(spec(s), { fkey: `tx:${s.idx}:ab`, label: t('transcript.audio_label', { n: s.idx + 1 }) })));
  }

  function removeBar(row) {
    const wrap = row.querySelector(':scope > .s-audio');
    if (!wrap) return;
    const b = wrap.querySelector('.abar');
    if (b && b.destroy) b.destroy();
    wrap.remove();
  }

  function allLoaded() {
    return total != null && sentences.length >= total;
  }

  function loadMore(manual) {
    if (loading || allLoaded() || (loadFailed && !manual)) return;
    if (!moreFocused && document.activeElement === moreBtn) {
      let kb = true;
      try {
        kb = typeof moreBtn.matches !== 'function' || moreBtn.matches(':focus-visible');
      } catch {
        kb = true;
      }
      moreFocused = kb ? 'kb' : 'mouse';
      document.addEventListener('pointerdown', dropMoreFocus, true);
    }
    loading = true;
    loadFailed = false;
    updateFooter();
    serial(doLoadMore);
  }

  // "加载更多" had the focus when the page was asked for: it is disabled
  // while the page loads and hidden after the last one, and the focus would
  // be left on the page (T6 #5). Once loaded it goes back to the button, or,
  // the button gone, to the first sentence just loaded (its time, the row's
  // first control), else the last sentence shown — from the keyboard only:
  // a mouse user's Space would then play that sentence instead of scrolling
  // (R29); the button gone, their focus stays on the page as before. Focus
  // moved elsewhere meanwhile stays, and so does a pointer press anywhere
  // meanwhile (the user went on: the focus is theirs).
  let moreFocused = false; // false | 'kb' | 'mouse'
  function dropMoreFocus() {
    moreFocused = false;
    document.removeEventListener('pointerdown', dropMoreFocus, true);
  }
  function settleMoreFocus(firstNew) {
    const how = moreFocused;
    dropMoreFocus();
    if (!how) return;
    const a = document.activeElement;
    if (a && a !== moreBtn && a !== document.body && a !== document.documentElement) return;
    if (!moreBtn.hidden && !moreBtn.disabled) {
      if (a !== moreBtn) moreBtn.focus({ preventScroll: true });
      return;
    }
    if (how !== 'kb') return;
    const shownRow = (r) => r && !r.hidden;
    let row = firstNew != null && shownRow(rows.get(firstNew)) ? rows.get(firstNew) : null;
    // the first new sentence stands where the button was (on screen): no
    // scrolling. The last one shown instead (a filter hid every new one)
    // may be far off screen: the browser scrolls it into view, so the
    // focus is not on something the user cannot see (R29 detail b)
    const isNew = !!row;
    for (let i = sentences.length - 1; !row && i >= 0; i--) if (shownRow(rows.get(sentences[i].idx))) row = rows.get(sentences[i].idx);
    const ts = row && row.querySelector('.ts');
    if (ts) ts.focus(isNew ? { preventScroll: true } : undefined);
  }

  async function doLoadMore() {
    let firstNew = null;
    try {
      const page = await api.sentences(lec.id, sentences.length, PAGE);
      if (!alive || !page) return;
      total = page.total;
      firstError = '';
      if (!page.sentences.length) total = sentences.length; // defensive: stop paging
      for (const s of page.sentences) {
        if (s.idx < sentences.length) continue;
        sentences.push(s);
        const row = renderRow(s);
        rows.set(s.idx, row);
        listEl.append(row);
        if (firstNew == null) firstNew = s.idx;
      }
    } catch (e) {
      loadFailed = true;
      if (!sentences.length) firstError = (e && e.message) || t('common.load_failed');
    } finally {
      loading = false;
      if (alive) {
        updateFooter();
        applyFilter(false);
        renderState();
        settleMoreFocus(firstNew);
        // Keep filling while the bottom is still on screen.
        if (!loadFailed && !allLoaded() && !query.trim() && inView(sentinel)) setTimeout(() => loadMore(false), 0);
      }
    }
  }

  async function loadAll() {
    while (alive && !allLoaded() && !loadFailed) {
      loadMore(true);
      await chain;
    }
    applyFilter(true);
  }

  function inView(el) {
    const r = el.getBoundingClientRect();
    return r.top < window.innerHeight + 200 && r.bottom > -200;
  }

  // Re-fetch everything loaded so far and replace the rows that changed
  // (after `changed sentences`, SPEC §8.2; or a full refresh after reconnect).
  function refreshLoaded() {
    if (refreshQueued) return;
    if (!sentences.length && firstError) {
      retryFirst();
      return;
    }
    refreshQueued = true;
    serial(async () => {
      refreshQueued = false;
      const want = Math.max(sentences.length, PAGE);
      const fresh = [];
      let newTotal = total;
      try {
        for (let off = 0; off < want; off += PAGE) {
          const page = await api.sentences(lec.id, off, PAGE);
          if (!alive || !page) return;
          newTotal = page.total;
          fresh.push(...page.sentences);
          if (page.sentences.length < PAGE) break;
        }
      } catch {
        return;
      }
      total = newTotal;
      preserveFocus(() => {
        for (let i = 0; i < fresh.length; i++) {
          const s = fresh[i];
          retranslated.seen(s); // T25 S3: the same translation back is said
          if (i < sentences.length) {
            if (JSON.stringify(sentences[i]) !== JSON.stringify(s)) {
              sentences[i] = s;
              replaceRow(i);
            }
          } else {
            sentences.push(s);
            const row = renderRow(s);
            rows.set(s.idx, row);
            listEl.append(row);
          }
        }
        // Fewer sentences than before (e.g. asr was re-run): drop the rest.
        while (sentences.length > fresh.length) {
          const s = sentences.pop();
          const row = rows.get(s.idx);
          if (row) {
            removeBar(row);
            row.remove();
          }
          rows.delete(s.idx);
          expanded.delete(s.idx);
          if (editing && editing.idx === s.idx) editing = null;
        }
      });
      updateFooter();
      applyFilter(false);
      renderState();
    });
  }

  function retryFirst() {
    firstError = '';
    loadFailed = false;
    renderState();
    loadMore(true);
  }

  function updateFooter() {
    moreBtn.hidden = allLoaded() || total === 0 || (!sentences.length && !!firstError) || (total == null && !loadFailed);
    moreBtn.disabled = loading;
    moreBtn.textContent = loading ? t('transcript.more.loading') : loadFailed ? t('transcript.more.failed') : t('transcript.more.rest', { n: total == null ? '?' : total - sentences.length });
  }

  // The query over the Chinese (P14.6): in a Traditional display a
  // Traditional term also finds the stored Simplified text; the search runs
  // again once the converter for that has loaded.
  let zhq = zhQuery('');
  const filterAgain = () => applyFilter(true);
  function zhqFor(q) {
    if (zhq.q !== q) zhq = zhQuery(q, filterAgain);
    return zhq;
  }

  function matches(s, q) {
    return s.text.toLowerCase().includes(q) || zhqFor(q).test(s.zh) || s.asr_text.toLowerCase().includes(q);
  }

  // Shows / hides rows for the search; `marks` re-highlights the matches.
  function applyFilter(marks) {
    const q = query.trim().toLowerCase();
    let shown = 0;
    for (const s of sentences) {
      const row = rows.get(s.idx);
      const ok = !q || matches(s, q);
      if (row) {
        row.hidden = !ok;
        if (marks || q) highlightRow(row, s, q);
      }
      if (ok) shown++;
    }
    if (q) info.textContent = t('transcript.info.matched', { n: shown }) + (allLoaded() ? '' : t('transcript.info.loading_rest'));
    else info.textContent = total == null ? '' : allLoaded() ? t('lecture.stat.total', { n: total }) : t('transcript.info.loaded', { loaded: sentences.length, n: total });
    if (sentences.length && q && shown === 0) {
      clear(stateEl);
      stateEl.append(h('div', { class: 'tx-empty' }, icon('search', { size: 20 }), h('p', null, t('transcript.no_match', { q: query.trim() }))));
    } else if (sentences.length) {
      clear(stateEl);
    }
  }

  function highlight(text, q) {
    if (!q || !text) return text || '';
    const low = text.toLowerCase();
    const out = [];
    let i = 0;
    for (;;) {
      const j = low.indexOf(q, i);
      if (j < 0) break;
      if (j > i) out.push(text.slice(i, j));
      out.push(h('mark', { class: 'hl' }, text.slice(j, j + q.length)));
      i = j + q.length;
    }
    out.push(text.slice(i));
    return out;
  }

  // The translation as shown (P14.6 display script) with the query's matches marked.
  function highlightZh(text, q) {
    return markNeedles(zhText(text || ''), q ? zhqFor(q).needles : [], (x) => h('mark', { class: 'hl' }, x));
  }

  function highlightRow(row, s, q) {
    const en = row.querySelector('.s-text');
    if (en) en.replaceChildren(...[].concat(highlight(s.text, q)));
    const zh = row.querySelector('.s-zh-text');
    if (zh && s.zh) zh.replaceChildren(...highlightZh(s.zh, q));
  }

  // Loading / error / empty states before there are rows (the search's
  // "no match" message is applyFilter's).
  function renderState() {
    if (sentences.length) return;
    clear(stateEl);
    if (firstError) {
      stateEl.append(h('div', { class: 'tx-error' }, errorBlock(t('transcript.load_failed', { error: firstError }), retryFirst)));
      return;
    }
    if (total == null) {
      stateEl.append(skeletonRows());
      return;
    }
    if (total !== 0) return;
    const asr = lec.stages.find((s) => s.stage === 'asr');
    let text;
    if (asr && asr.status === 'done') text = t('transcript.empty.none_found');
    else {
      let state = t('transcript.empty.state_unknown');
      if (lec.draft) state = U.waitingForText('not_started');
      else if (asr) {
        state = U.stageStatusText(asr);
        if (asr.waiting_for) state = t('transcript.empty.state_wait', { state, wait: U.stageWaitingText(asr, ctx.system(), ctx.resources()) });
      }
      text = t('transcript.empty.pending', { state });
    }
    stateEl.append(h('div', { class: 'tx-empty' }, icon('audio-lines', { size: 22 }), h('h3', null, t('transcript.empty.title')), h('p', null, text)));
  }

  function skeletonRows() {
    return h(
      'div',
      { class: 'tx-skel', 'aria-hidden': 'true' },
      Array.from({ length: 7 }, (_, i) =>
        h(
          'div',
          { class: 'srow is-skeleton' },
          h('div', { class: 'skel', style: { width: '40px' } }),
          h('div'),
          h('div', { class: 'skel', style: { width: `${[88, 64, 92, 72, 80, 58, 76][i]}%` } }),
          h('div', { class: 'skel', style: { width: `${[70, 52, 82, 60, 66, 48, 74][i]}%` } }),
          h('div'),
        ),
      ),
    );
  }

  // ---- rows ----

  function markFor(s) {
    if (s.patch) {
      const c = s.patch.class;
      const review = s.patch.review;
      const pendingDecide = c === 'decide' && review === 'pending';
      let state = '';
      if (c === 'decide') state = U.REVIEW_NAMES[review] || review;
      return {
        cls: c,
        hollow: c === 'absorb' || (c === 'decide' && !pendingDecide),
        label: state ? t('transcript.mark.with_state', { name: markName(c), state }) : markName(c),
        title: t(state ? 'transcript.mark.tip_state' : 'transcript.mark.tip', { name: markName(c), state, hint: U.CLASS_HINTS[c] || '' }),
      };
    }
    if (s.human_text != null || s.asr_text !== s.text) return { cls: 'human', hollow: false, label: t('transcript.mark.human'), title: t('transcript.mark.human_tip') };
    return null;
  }

  function renderRow(s) {
    const key = keyOf(s);
    const mark = markFor(s);
    const open = expanded.has(s.idx);
    const playing = Clip.playingKey() === key;
    const time = U.fmtClock(s.start_ms);
    const q = query.trim().toLowerCase();
    const row = h(
      'div',
      {
        class: 'srow' + (s.zh_status === 'qa_failed' ? ' is-qa-failed' : '') + (open ? ' is-open' : ''),
        dataset: { idx: String(s.idx), clip: key },
      },
      h(
        'div',
        { class: 's-time' },
        h(
          'button',
          {
            type: 'button',
            class: 'ts' + (playing ? ' is-playing' : ''),
            title: t('transcript.play_tip'),
            'aria-label': t('transcript.play_aria', { n: s.idx + 1, time }),
            dataset: { fkey: `ts:${s.idx}`, clip: key, time },
            onclick: (e) => Clip.toggle(spec(s), e.shiftKey ? Clip.SLOW_RATE : 1),
          },
          icon(playing ? 'audio-lines' : 'play', { size: 12, class: 'ts-icon' }),
          h('span', null, time),
        ),
      ),
      h(
        'div',
        { class: 's-mark' },
        mark
          ? h(
              'button',
              {
                type: 'button',
                class: `mk mk-${mark.cls}`,
                title: mark.title,
                'aria-label': t(open ? 'transcript.mark.aria_collapse' : 'transcript.mark.aria_expand', { label: mark.label }),
                'aria-expanded': open ? 'true' : 'false',
                // C1: the details the button shows (only there while open)
                'aria-controls': open ? detailId(s.idx) : null,
                dataset: { fkey: `mk:${s.idx}` },
                onclick: () => toggleExpand(s.idx),
              },
              glyph(mark.cls, mark.hollow),
            )
          : null,
      ),
      h('div', { class: 's-en' }, h('span', { class: 's-text' }, highlight(s.text, q))),
      zhCell(s, q),
      h('div', { class: 's-more' }, menuButton(h('button', { type: 'button', class: 'icon-btn s-more-btn', title: t('transcript.more_tip'), 'aria-label': t('transcript.more_aria', { n: s.idx + 1 }), dataset: { fkey: `more:${s.idx}` } }, icon('ellipsis', { size: 16 })), () => menuItems(s))),
    );
    if (open) row.append(detail(s));
    if (editing && editing.idx === s.idx) row.append(editBox(s));
    if (key === barKey) addBar(row);
    return row;
  }

  function replaceRow(idx) {
    const old = rows.get(idx);
    const s = sentences[idx];
    if (!old || !s) return;
    removeBar(old);
    const row = renderRow(s);
    const q = query.trim().toLowerCase();
    row.hidden = !!q && !matches(s, q);
    old.replaceWith(row);
    rows.set(idx, row);
  }

  function rerender(idx) {
    preserveFocus(() => replaceRow(idx));
  }

  function zhCell(s, q) {
    if (s.zh_status === 'qa_failed') {
      return h(
        'div',
        {
          class: 's-zh zh-fail tip',
          tabindex: '0',
          // the check's reason, coded (qa_error_info), else the stored English text
          'data-tip': t('transcript.qa.tip', { reason: U.errorInfoText(s.qa_error_info, s.qa_error || '') || t('review.no_reason') }),
          'aria-label': t('transcript.qa.aria', { reason: U.errorInfoText(s.qa_error_info, s.qa_error || '') || t('transcript.qa.no_reason'), zh: zhText(s.zh || '') }),
          dataset: { fkey: `zh:${s.idx}` },
        },
        h('span', { class: 'zh-flag' }, icon('triangle-alert', { size: 11 }), t('transcript.qa.flag')),
        h('span', { class: 's-zh-text', lang: zhLang() }, highlightZh(s.zh, q)),
      );
    }
    if (s.zh_status === 'stale') {
      return h(
        'div',
        { class: 's-zh zh-stale', title: t('transcript.stale.tip') },
        h('span', { class: 'zh-flag' }, h('span', { class: 'zh-dot', 'aria-hidden': 'true' }), t('transcript.stale.flag')),
        s.zh ? h('span', { class: 's-zh-text', lang: zhLang() }, highlightZh(s.zh, q)) : null,
      );
    }
    if (s.zh_status === 'none' || s.zh == null) return h('div', { class: 's-zh zh-none' }, t('transcript.untranslated'));
    return h('div', { class: 's-zh' }, h('span', { class: 's-zh-text', lang: zhLang() }, highlightZh(s.zh, q)));
  }

  function detailId(idx) {
    return `s-detail-${idx}`;
  }

  function detail(s) {
    const p = s.patch;
    const mark = markFor(s);
    const line = (label, ...content) => h('div', { class: 'd-line' }, h('span', { class: 'd-k' }, label), h('div', { class: 'd-v' }, content));
    return h(
      'div',
      { class: 's-detail', id: detailId(s.idx) },
      line(t('transcript.detail.asr_vs_current'), s.asr_text === s.text ? h('span', { class: 'muted' }, t('transcript.detail.same')) : h('span', { class: 'd-diff' }, diffView(s.asr_text, s.text))),
      p ? line(t('transcript.detail.class'), h('span', { class: `d-mark mk-${p.class}` }, glyph(p.class, mark && mark.hollow), markName(p.class)), h('span', { class: 'muted' }, ` ${U.CLASS_HINTS[p.class] || ''}`)) : null,
      p ? line(t('player.side.reason'), p.reason ? h('span', { lang: 'en' }, p.reason) : t('review.no_reason')) : null,
      p && p.class === 'decide'
        ? line(t('transcript.detail.review_state'), U.REVIEW_NAMES[p.review] || p.review, p.review === 'pending' ? [' · ', h('a', { href: `#/lecture/${enc(lec.id)}/review` }, t('transcript.detail.go_review'))] : null)
        : null,
      s.human_text != null ? line(t('transcript.mark.human'), t('transcript.detail.human_text', { reset: t('player.menu.reset') })) : null,
      s.low_conf_words.length ? line(t('transcript.detail.low_conf'), s.low_conf_words.map((w) => h('span', { class: 'd-word' }, w.w))) : null,
    );
  }

  // Unavailable items are not shown (A12.2, §9.8): edit / reset / retranslate
  // need the lecture to be reviewable (not while repair or translate runs);
  // reset only when there is a human edit; retranslate only for a sentence
  // that has a translation and is not already being retranslated.
  function menuItems(s) {
    const course = ctx.courses().find((c) => c.id === lec.course_id) || null;
    const ref = citeRef(course, lec.cite_label || lec.label, s.start_ms);
    const playing = Clip.isPlaying(spec(s));
    const can = lec.reviewable;
    // done: the toast text after a successful copy
    const copy = async (text, done) => {
      const ok = await copyKeepingFocus(text);
      toast(ok ? done : t('common.copy_failed'), ok ? 'success' : 'warn');
    };
    return [
      { label: playing ? t('player.ctl.pause') : t('player.ctl.play'), icon: playing ? 'pause' : 'play', onSelect: () => Clip.toggle(spec(s), 1) },
      'sep',
      { label: t('player.menu.copy_cite'), icon: 'quote', hint: `[${ref}]`, onSelect: () => copy(`[${ref}]`, t('player.cite_copied', { ref: `[${ref}]` })) },
      { label: t('player.menu.copy_en'), icon: 'copy', onSelect: () => copy(s.text, t('player.copied_en')) },
      s.zh ? { label: t('player.menu.copy_zh'), icon: 'copy', onSelect: () => copy(zhText(s.zh), t('player.copied_zh')) } : null,
      'sep',
      can ? { label: t('player.menu.edit'), icon: 'pencil', onSelect: () => startEdit(s.idx) } : null,
      can && s.human_text != null ? { label: t('player.menu.reset'), icon: 'undo-2', hint: t('player.menu.reset_hint'), onSelect: () => sentenceOp(s.idx, 'reset') } : null,
      can && (s.zh_status === 'ok' || s.zh_status === 'qa_failed') ? { label: t('player.menu.retranslate'), icon: 'languages', onSelect: () => sentenceOp(s.idx, 'retranslate') } : null,
      can && canRetranslateCurrent(s) ? { label: t('player.menu.retranslate_current'), icon: 'languages', hint: t('player.menu.retranslate_current_hint'), onSelect: () => retranslateCurrentOp(s.idx) } : null,
      'sep',
      // SPEC-v2 P1: one second before the sentence
      lec.playable ? { label: t('player.entry.open_in_player'), icon: 'external-link', onSelect: () => (location.hash = `#/play/${enc(lec.id)}?t=${Math.max(0, s.start_ms - 1000)}`) } : null,
    ];
  }

  bindContextMenu(listEl, (e) => {
    const row = e.target.closest && e.target.closest('.srow');
    if (!row || !listEl.contains(row)) return null;
    // A text selection inside the row keeps the browser menu (copy it as is).
    const sel = window.getSelection && window.getSelection();
    if (e.type === 'contextmenu' && sel && sel.toString().trim() && row.contains(sel.anchorNode)) return null;
    const s = sentences[Number(row.dataset.idx)];
    return s ? menuItems(s) : null;
  });

  function toggleExpand(idx) {
    if (expanded.has(idx)) expanded.delete(idx);
    else expanded.add(idx);
    rerender(idx);
  }

  function startEdit(idx) {
    if (!lec.reviewable) {
      toast(busyText(), 'warn');
      return;
    }
    const prev = editing;
    editing = { idx, draft: sentences[idx].text };
    if (prev && prev.idx !== idx) rerender(prev.idx);
    replaceRow(idx);
    const ta = rows.get(idx).querySelector('textarea');
    if (ta) {
      ta.focus();
      ta.setSelectionRange(ta.value.length, ta.value.length);
    }
  }

  function endEdit(idx) {
    editing = null;
    replaceRow(idx);
    const b = rows.get(idx) && rows.get(idx).querySelector('.ts');
    if (b) b.focus();
  }

  // R78b medium 2: while the lecture's translation is held (stale, failed,
  // cancelled) an edit or "translate again" waits for its next run; the
  // texts say so instead of promising a translation now
  const held = () => translationHeld(lec);

  function editBox(s) {
    // round 9 WV-02 (T28 suggestion 4): the two notes of the box describe
    // it (one edit box at a time; the ids carry its sentence all the same)
    const hintId = `tx-edit-${s.idx}-hint`;
    const zhNoteId = `tx-edit-${s.idx}-zh-note`;
    const ta = h('textarea', {
      class: 'input s-edit-ta',
      rows: '2',
      'aria-label': t('transcript.edit.aria', { n: s.idx + 1 }),
      'aria-describedby': `${hintId} ${zhNoteId}`,
      dataset: { fkey: `edit:${s.idx}` },
    });
    ta.value = editing.draft;
    ta.addEventListener('input', () => {
      if (editing) editing.draft = ta.value;
    });
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        saveEdit(s.idx);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        endEdit(s.idx);
      }
    });
    return h(
      'div',
      { class: 's-edit' },
      ta,
      h(
        'div',
        { class: 's-edit-actions' },
        h('button', { type: 'button', class: 'btn btn-sm btn-primary', disabled: !lec.reviewable, onclick: () => saveEdit(s.idx) }, t('common.save')),
        h('button', { type: 'button', class: 'btn btn-sm', onclick: () => endEdit(s.idx) }, t('common.cancel')),
        h('span', { class: 'muted small', id: hintId }, !lec.reviewable ? busyText() : held() ? t('transcript.edit.hint_held') : t('transcript.edit.hint')),
      ),
      // T22 S2, T23 general 7: the translation is not edited itself
      h('p', { class: 'muted small', id: zhNoteId }, held() ? t('player.edit.zh_note_held') : t('player.edit.zh_note')),
    );
  }

  async function saveEdit(idx) {
    if (!editing || editing.idx !== idx) return;
    const text = editing.draft.trim();
    if (!text) {
      toast(t('player.edit.empty'), 'warn');
      return;
    }
    const wait = held(); // as the lecture was when it was sent
    try {
      const updated = await api.sentenceEdit(lec.id, idx, text);
      if (!alive || !updated) return;
      retranslated.drop(idx); // the English changed: its translation is a new one
      sentences[idx] = updated;
      endEdit(idx);
      // WV-01: the held text stays until closed
      if (wait) heldToast(t('player.edit.saved_held'));
      else toast(t('player.edit.saved'), 'success');
    } catch {
      // shown by the API layer; keep the edit box open
    }
  }

  async function sentenceOp(idx, op) {
    const was = sentences[idx];
    const wait = held(); // as the lecture was when it was sent
    try {
      const updated = op === 'reset' ? await api.sentenceReset(lec.id, idx) : await api.sentenceRetranslate(lec.id, idx);
      if (!alive || !updated) return;
      // held: no result comes soon, nothing to compare (the next run translates the whole lecture's waiting sentences)
      if (op === 'retranslate' && !wait) retranslated.start(was);
      else retranslated.drop(idx);
      sentences[idx] = updated;
      rerender(idx);
      if (wait) heldToast(t(op === 'reset' ? 'player.menu.reset_done_held' : 'player.menu.retranslate_held'));
      else toast(t(op === 'reset' ? 'player.menu.reset_done' : 'player.menu.retranslate_done'), 'success');
    } catch {
      // shown by the API layer
    }
  }

  async function retranslateCurrentOp(idx) {
    const updated = await retranslateCurrent(lec.id, idx, lec); // R78c: says when the translation is held
    if (!alive || !updated) return;
    sentences[idx] = updated;
    rerender(idx);
  }

  function syncBusy() {
    busyEl.hidden = !!lec.reviewable;
  }

  syncBusy();
  renderState(); // skeleton until the first page
  updateFooter();
  loadMore(true);

  return {
    setLecture(l) {
      const was = lec.reviewable;
      const hadNone = total === 0;
      lec = l;
      syncColumns();
      syncBusy();
      if (was !== l.reviewable && editing) rerender(editing.idx);
      if (total === 0) renderState();
      if (hadNone && l.counts.sentences > 0) refreshLoaded();
    },
    refresh: refreshLoaded,
    onChanged(ev) {
      if (ev.kind === 'sentences' && ev.id === lec.id) {
        clearTimeout(changedTimer);
        changedTimer = setTimeout(refreshLoaded, 400);
      }
    },
    destroy() {
      alive = false;
      dropMoreFocus(); // a page still loading leaves no pointerdown listener behind (R29 detail a)
      io.disconnect();
      unClip();
      clearTimeout(searchTimer);
      clearTimeout(changedTimer);
      for (const r of rows.values()) removeBar(r);
      clear(root);
    },
  };
}
