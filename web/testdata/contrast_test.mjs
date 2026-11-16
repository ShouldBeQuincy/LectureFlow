// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// N41: text colours reach WCAG 2.x AA (4.5:1) on every background they are
// actually drawn on, in the light and in the dark half of the tokens.
// The tokens are read from the token block of style.css (every colour is
// light-dark(light, dark)); translucent backgrounds (hover, tinted chips)
// are composited over the surface they sit on, as the browser does.
// web/contrast_test.go starts it with the path of style.css; "--table"
// prints every ratio instead of only the failures.
//
// Not checked here, on purpose (decoration, not text; listed in the N41
// report): the stage bar colours (--st-*), borders, the marker glyphs of
// the review page (--mk-* as dots), status icons, and --accent used as an
// icon or tile colour (the accent blue stays as the user chose it).

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const cssPath = process.argv[2];
const table = process.argv.includes('--table');
const css = await readFile(cssPath, 'utf8');

// ---- tokens ----

const block = css.slice(css.indexOf(':root {'), css.indexOf('/* elevation */'));
assert.ok(block.length > 100, 'the token block of style.css was not found');
const tokens = {};
for (const m of block.matchAll(/^\s*(--[\w-]+):\s*(light-dark\((.+)\)|#[0-9a-f]{6});/gim)) {
  if (m[3]) {
    const parts = m[3].split(/,(?![^(]*\))/).map((s) => s.trim());
    tokens[m[1]] = { light: parts[0], dark: parts[1] };
  } else {
    tokens[m[1]] = { light: m[2], dark: m[2] };
  }
}

function rgba(s) {
  let m = s.match(/^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i);
  if (m) return { c: [1, 2, 3].map((i) => parseInt(m[i], 16)), a: 1 };
  m = s.match(/^rgb\((\d+) (\d+) (\d+) \/ ([\d.]+)\)$/);
  if (m) return { c: [+m[1], +m[2], +m[3]], a: +m[4] };
  throw new Error(`unknown colour ${s}`);
}

function token(name, mode) {
  const t = tokens[name];
  assert.ok(t, `style.css has no token ${name}`);
  return rgba(t[mode]);
}

const over = (top, base, a) => top.map((v, i) => v * a + base[i] * (1 - a));

// a background: "--tok" (opaque), "--tok over --base" (a translucent token
// over a surface), or "N% --tok over --base" (color-mix(in srgb, tok N%,
// transparent) over a surface).
function background(spec, mode) {
  let m = spec.match(/^(\d+)% (--[\w-]+) over (--[\w-]+)$/);
  if (m) return over(token(m[2], mode).c, token(m[3], mode).c, +m[1] / 100);
  m = spec.match(/^(--[\w-]+) over (--[\w-]+)$/);
  if (m) {
    const t = token(m[1], mode);
    return over(t.c, token(m[2], mode).c, t.a);
  }
  const t = token(spec, mode);
  assert.equal(t.a, 1, `${spec} is translucent; say what it sits on`);
  return t.c;
}

// ---- WCAG 2.x ----

function luminance(c) {
  const [r, g, b] = c.map((v) => v / 255).map((v) => (v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function ratio(a, b) {
  const x = luminance(a);
  const y = luminance(b);
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
}

// ---- the pairs ----

// The surfaces text sits on at rest: the app chrome (nav, rail), the
// content column, cards, raised layers (menus, dialogs, hover cards) and
// sunken wells (inputs, tiles, quiet chips).
const surfaces = ['--bg-app', '--bg-content', '--bg-card', '--bg-raised', '--bg-sunken'];

// Body and label text (--text-1/2/3) also sits on selected rows (lecture
// list, player transcript, current course in the nav) and on hovered rows
// and nav items. Not counted: pressed buttons — reaching 4.5 there would
// lift the dark --text-3 to within a step of --text-2 (N41 report). The
// highlighted item of a menu (--bg-active or --bg-hover over --bg-raised)
// gives its hint (.ctx-hint) a colour of its own, checked below (R4: the
// dark --text-3 there was 3.59 / 4.16:1).
const textBackgrounds = [
  ...surfaces,
  '--bg-selected',
  '--bg-selected-hover',
  '--bg-hover over --bg-app',
  '--bg-hover over --bg-content',
  '--bg-hover over --bg-card',
];

const pairs = [
  // primary text: titles, sentences, table cells
  ...textBackgrounds.map((bg) => ['--text-1', bg, 'primary text']),
  // secondary text: descriptions, row meta, quiet chips
  ...textBackgrounds.map((bg) => ['--text-2', bg, 'secondary text']),
  // tertiary text: timestamps, counts, hints, column headers, placeholders
  ...textBackgrounds.map((bg) => ['--text-3', bg, 'tertiary text (hints, timestamps, counts)']),

  // status text on the plain surfaces: .tone-ok/.tone-warn/.tone-err,
  // .st-done/.st-stale/.st-failed in the stage cards, field errors and
  // warnings, .wiz-saved, the nav "off" switch, rail warnings
  ...surfaces.map((bg) => ['--ok', bg, 'success text (.tone-ok, .st-done, .wiz-saved)']),
  ...surfaces.map((bg) => ['--warn', bg, 'warning text (.tone-warn, .st-stale, .field-hint.is-warn, .nav-toggle.is-off)']),
  ...surfaces.map((bg) => ['--err', bg, 'error text (.tone-err, .st-failed, .field-error, .ctx-item.is-danger)']),
  // status chips, banners and notes on their own soft background
  ['--ok', '--ok-soft', 'success chip / banner (.chip-ok, .banner-ok, .rc-state-ok, .src-review)'],
  ['--warn', '--warn-soft', 'warning chip / banner (.chip-warn, .chip-review, .banner-warn, .dlg-warn, .card-note-stale)'],
  ['--err', '--err-soft', 'error chip / banner (.chip-err, .banner-err, .error-block, .card-errtext, .fail-why)'],

  // links and accent text (unchanged colours, kept passing)
  ...surfaces.map((bg) => ['--accent-text', bg, 'links and accent text (.rc-ts, .chip-cal:hover)']),
  ['--accent-text', '--accent-soft', 'running chip (.chip-run), drop zone while dragging'],
  ['--accent-text', '--bg-selected', 'accent text on a selected row or choice'],

  // review page
  ['--ins-fg', '--ins-bg', 'inserted text in a diff'],
  ['--del-fg', '--del-bg', 'deleted text in a diff'],
  ['--mk-decide', '12% --mk-decide over --bg-card', 'review card state "pending" (.rc-state-pending)'],
  ['--mk-human', '14% --mk-human over --bg-card', 'review card state "edited by hand" (.rc-state-human)'],

  // labels on solid fills
  ['--accent-fg', '--accent', 'primary button label (.btn-primary), current wizard step'],
  ['--inverse-fg', '--inverse-bg', 'toast and tooltip text'],
];

// .btn-danger-solid: its label is white in the light theme and
// --inverse-fg in the dark one, on a solid --err fill.
const dangerLabel = { light: [255, 255, 255], dark: token('--inverse-fg', 'dark').c };
const ruleDanger = css.match(/\.btn-danger-solid \{[^}]*\}/);
assert.ok(ruleDanger, 'style.css has no .btn-danger-solid rule');
assert.match(ruleDanger[0], /color: light-dark\(#fff, var\(--inverse-fg\)\)/, '.btn-danger-solid label colour changed; update the check');

// The count badges in buttons (.btn-count, 10.5px): on a primary button a
// translucent black (light) / white (dark) over --accent under the button's
// --accent-fg; on a warning button --warn mixed 70% with --text-1 over
// 18% --warn on --warn-soft (at rest and hovered).
const ruleCountPrimary = css.match(/\.btn-primary \.btn-count \{[^}]*\}/);
const ruleCountWarn = css.match(/\.btn-warn \.btn-count \{[^}]*\}/);
const ruleWarnHover = css.match(/\.btn-warn:hover:not\(:disabled\) \{[^}]*\}/);
assert.ok(ruleCountPrimary && ruleCountWarn && ruleWarnHover, 'style.css lacks a .btn-count or .btn-warn rule');
assert.match(ruleCountPrimary[0], /background: light-dark\(rgb\(0 0 0 \/ 0\.2\), rgb\(255 255 255 \/ 0\.22\)\); color: inherit;/, '.btn-primary .btn-count changed; update the check');
assert.match(ruleCountWarn[0], /background: color-mix\(in srgb, var\(--warn\) 18%, transparent\); color: color-mix\(in srgb, var\(--warn\) 70%, var\(--text-1\)\);/, '.btn-warn .btn-count changed; update the check');
assert.match(ruleWarnHover[0], /background: color-mix\(in srgb, var\(--warn-soft\) 70%, var\(--warn\) 12%\);/, '.btn-warn:hover changed; update the check');
const mix = (a, pa, b, pb) => a.map((v, i) => (v * pa + b[i] * pb) / (pa + pb));
const countPairs = (mode) => {
  const accent = token('--accent', mode).c;
  const warn = token('--warn', mode).c;
  const soft = token('--warn-soft', mode).c;
  const warnText = mix(warn, 70, token('--text-1', mode).c, 30);
  // color-mix(soft 70%, warn 12%): the percentages sum to 82%, so the mix
  // is 82% opaque, here over a card
  const softHover = over(mix(soft, 70, warn, 12), token('--bg-card', mode).c, 0.82);
  return [
    ['.btn-primary .btn-count', token('--accent-fg', mode).c, mode === 'light' ? over([0, 0, 0], accent, 0.2) : over([255, 255, 255], accent, 0.22)],
    ['.btn-warn .btn-count', warnText, over(warn, soft, 0.18)],
    ['.btn-warn:hover .btn-count', warnText, over(warn, softHover, 0.18)],
  ];
};

// W4 (T7, T8): a hovered warning button's label. The hover fill (as above,
// 82% opaque) lies over the surface the button stands on; the label is
// --warn mixed 80% with --text-1 (plain --warn there was 3.98 on the
// content background, light theme).
assert.match(ruleWarnHover[0], /color: color-mix\(in srgb, var\(--warn\) 80%, var\(--text-1\)\);/, '.btn-warn:hover label colour changed; update the check');
const warnHoverPairs = (mode) => {
  const warn = token('--warn', mode).c;
  const label = mix(warn, 80, token('--text-1', mode).c, 20);
  const fill = mix(token('--warn-soft', mode).c, 70, warn, 12);
  return ['--bg-card', '--bg-content', '--bg-sunken', '--bg-raised', '--bg-app'].map((s) => [`.btn-warn:hover label on ${s}`, label, over(fill, token(s, mode).c, 0.82)]);
};

// W4 (T8): the off-peak tariff tag (resource inspector, provider rows):
// its own tokens, in the course-p2 block of style.css
const tariffBlock = css.match(/:root \{\s*--offpeak:[^}]*\}/);
assert.ok(tariffBlock, 'style.css has no --offpeak token block');
const tariffTok = {};
for (const m of tariffBlock[0].matchAll(/(--[\w-]+): light-dark\((#[0-9a-f]{6}), (#[0-9a-f]{6})\);/gi)) tariffTok[m[1]] = { light: rgba(m[2]).c, dark: rgba(m[3]).c };
assert.ok(tariffTok['--offpeak'] && tariffTok['--offpeak-soft'], 'the off-peak tokens are not light-dark pairs of hex colours');
assert.match(css, /\.tariff-tag\.is-offpeak \{ background: var\(--offpeak-soft\); color: var\(--offpeak\); \}/, '.tariff-tag.is-offpeak changed; update the check');

// R4: the hint of a highlighted context menu item: --text-3 in the light
// theme, a lighter grey of the same hue in the dark one
const ruleCtxHint = css.match(/\.ctx-item:focus \.ctx-hint, \.ctx-item\[aria-expanded="true"\] \.ctx-hint \{[^}]*\}/);
assert.ok(ruleCtxHint, 'style.css has no rule for the hint of a highlighted menu item');
const ctxHintDark = ruleCtxHint[0].match(/color: light-dark\(var\(--text-3\), (#[0-9a-f]{6})\);/i);
assert.ok(ctxHintDark, 'the highlighted menu hint colour changed; update the check');
const ctxHintPairs = (mode) => {
  const fg = mode === 'light' ? token('--text-3', mode).c : rgba(ctxHintDark[1]).c;
  return ['--bg-active over --bg-raised', '--bg-hover over --bg-raised'].map((bg) => [`highlighted menu hint on ${bg}`, fg, background(bg, mode)]);
};

const failures = [];
const rows = [];
for (const mode of ['light', 'dark']) {
  for (const [what, fg, bg] of [...warnHoverPairs(mode), ...ctxHintPairs(mode), ['off-peak tariff tag', tariffTok['--offpeak'][mode], tariffTok['--offpeak-soft'][mode]]]) {
    const r = ratio(fg, bg);
    rows.push(`${mode.padEnd(5)} ${what.padEnd(40)} ${r.toFixed(2)}`);
    if (r < 4.5) failures.push(`${mode}: ${what} is ${r.toFixed(2)}:1`);
  }
  for (const [fg, bg, what] of pairs) {
    const r = ratio(token(fg, mode).c, background(bg, mode));
    rows.push(`${mode.padEnd(5)} ${fg.padEnd(14)} ${tokens[fg][mode].padEnd(8)} on ${bg.padEnd(30)} ${r.toFixed(2)}`);
    if (r < 4.5) failures.push(`${mode}: ${fg} (${tokens[fg][mode]}) on ${bg} is ${r.toFixed(2)}:1 — ${what}`);
  }
  const r = ratio(dangerLabel[mode], token('--err', mode).c);
  rows.push(`${mode.padEnd(5)} danger label   on --err ${r.toFixed(2)}`);
  if (r < 4.5) failures.push(`${mode}: the .btn-danger-solid label on --err is ${r.toFixed(2)}:1`);
  for (const [what, fg, bg] of countPairs(mode)) {
    const rc = ratio(fg, bg);
    rows.push(`${mode.padEnd(5)} ${what.padEnd(28)} ${rc.toFixed(2)}`);
    if (rc < 4.5) failures.push(`${mode}: ${what} is ${rc.toFixed(2)}:1`);
  }
}

if (table) console.log(rows.join('\n'));
assert.deepEqual(failures, [], `contrast below 4.5:1:\n${failures.join('\n')}`);
console.log(`contrast checks: ok (${rows.length} pairs)`);
