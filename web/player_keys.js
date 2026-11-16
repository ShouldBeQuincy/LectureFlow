// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Keyboard shortcuts of the player (SPEC-v2 P6.3). They work anywhere on the
// player page, but not while a text field has focus, a dialog or a menu is
// open, or a modifier key (Ctrl / Alt / ⌘) is held. A focused slider, radio
// or checkbox is not a text field: Space and the letter keys still work
// there (after dragging the volume, Space pauses); only the arrow keys stay
// with the controls that move by them.

import { h, openDialog, isTypingTarget } from './dom.js';
import { t } from './i18n.js';

// [keys shown, i18n key of the action]; also the help table (P6.3 "?").
// opts.angles: the lecture has a second camera angle (ext web3: V swaps).
export function shortcutRows(opts = {}) {
  const space = t('player.key.space');
  return [
    [[space, 'K'], 'player.help.play'],
    [['←', '→'], 'player.help.seek5'],
    [['J', 'L'], 'player.help.seek10'],
    [['A', 'D'], 'player.help.sentence'],
    [['R'], 'player.help.loop'],
    [['[', ']'], 'player.help.rate'],
    [['C'], 'player.help.display'],
    [['T'], 'player.help.theater'],
    [['F'], 'player.help.fullscreen'],
    opts.angles ? [['V'], 'player.angle.help'] : null,
    [['Y'], 'player.help.cite'],
    // T22 G15: the sentence menu (edit the English, translate again) has no
    // button on the transcript's rows; this list names it
    [['Shift+F10'], 'player.help.sentence_menu'],
    [['/'], 'player.help.search'],
    [['?'], 'player.help.help'],
  ].filter(Boolean);
}

export function openHelp(opts = {}) {
  return openDialog(
    t('player.help.title'),
    (close) =>
      h(
        'div',
        { class: 'pl-help' },
        h(
          'table',
          { class: 'pl-help-table' },
          h(
            'tbody',
            null,
            shortcutRows(opts).map(([keys, label]) =>
              h(
                'tr',
                null,
                h(
                  'td',
                  { class: 'pl-help-keys' },
                  keys.map((k, i) => [i ? h('span', { class: 'pl-help-or' }, '/') : null, h('kbd', null, k)]),
                ),
                h('td', null, t(label)),
              ),
            ),
          ),
        ),
        h('p', { class: 'pl-help-note' }, t('player.help.note')),
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn btn-primary', autofocus: true, onclick: () => close(null) }, t('common.close'))),
      ),
    { className: 'pl-help-dlg' },
  );
}

// Native controls that the arrow keys move (a range slider changes its
// value, a radio group its choice).
export function ownsArrowKeys(el) {
  return !!(el && el.matches && el.matches('input[type="range"], input[type="radio"]'));
}

// bindKeys(handlers) → unbind. handlers: {toggle, seekBy(ms), prev, next,
// loop, rate(dir), display, theater, fullscreen, cite, search, help,
// angle → false when there is no second angle}.
export function bindKeys(root, hd) {
  let swallowSpaceUp = false;
  function onKey(e) {
    if (e.defaultPrevented || e.ctrlKey || e.altKey || e.metaKey || e.isComposing) return;
    if (isTypingTarget(e.target)) return;
    if (document.querySelector('dialog[open], .ctx-menu')) return;
    if (!root.isConnected) return;
    const k = e.key;
    const lower = k.length === 1 ? k.toLowerCase() : k;
    let done = true;
    switch (lower) {
      case ' ':
      case 'k':
        hd.toggle();
        swallowSpaceUp = k === ' ';
        break;
      case 'ArrowLeft':
        if (e.shiftKey || ownsArrowKeys(e.target)) return;
        hd.seekBy(-5000);
        break;
      case 'ArrowRight':
        if (e.shiftKey || ownsArrowKeys(e.target)) return;
        hd.seekBy(5000);
        break;
      case 'j':
        hd.seekBy(-10000);
        break;
      case 'l':
        hd.seekBy(10000);
        break;
      case 'a':
        hd.prev();
        break;
      case 'd':
        hd.next();
        break;
      case 'r':
        hd.loop();
        break;
      case '[':
        hd.rate(-1);
        break;
      case ']':
        hd.rate(1);
        break;
      case 'c':
        hd.display();
        break;
      case 't':
        hd.theater();
        break;
      case 'f':
        hd.fullscreen();
        break;
      case 'y':
        hd.cite();
        break;
      case 'v':
        done = !!(hd.angle && hd.angle());
        break;
      case '/':
        hd.search();
        break;
      case '?':
        hd.help();
        break;
      default:
        done = false;
    }
    if (done) e.preventDefault();
  }
  // A focused button would also be "clicked" by the Space key's keyup.
  function onKeyUp(e) {
    if (e.key === ' ' && swallowSpaceUp) {
      swallowSpaceUp = false;
      e.preventDefault();
    }
  }
  document.addEventListener('keydown', onKey);
  document.addEventListener('keyup', onKeyUp, true);
  return () => {
    document.removeEventListener('keydown', onKey);
    document.removeEventListener('keyup', onKeyUp, true);
  };
}
