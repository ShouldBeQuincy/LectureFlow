// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// A small Markdown renderer for the SYLLABUS.md preview (v1.1 A8.1 step 3,
// A8.2 course-page editor). It builds DOM nodes and only ever sets text
// through text nodes — never innerHTML — so HTML in the source (from the
// model or the user) is shown as text, not executed. The one exception is
// <br>, which LLMs often put in table cells: it becomes a <br> element.
//
// Supported: ATX headings, paragraphs, bullet and numbered lists (nested by
// indentation, tolerant of the 2/3/4-space mixes LLMs write), GFM pipe
// tables with alignment (outer pipes optional), blockquotes, fenced code,
// horizontal rules; inline **bold**, *italic* / _italic_, ~~strike~~,
// `code`, backslash escapes, common HTML entities, [links](url "title"),
// <autolinks> and bare http(s) URLs. Only http, https and mailto links are
// made clickable; anything else is shown as plain text. Images show their
// alt text (nothing is loaded from elsewhere).
//
// Malformed input must not hang the page (AUD5 low-3: a syllabus is up to
// 8 MiB, and the preview renders on the main thread). So:
//   - every scan is linear in its line: the searches for a closing
//     delimiter, bracket, code span or URL end are answered from indexes
//     built once per text (Scan), and headings, rules, delimiter rows and
//     trailing spaces are checked by hand, not by regular expressions that
//     backtrack (quadratically, or deeper than the stack) on a long line;
//     the list, fence and quote expressions are not run on a line they
//     cannot match because of a U+2028 / U+2029 (listItem);
//   - nesting is bounded (blockquotes and lists count together): inside
//     MAX_DEPTH of them, the rest of a blockquote is plain text, as
//     written; a list item nested deeper goes on as a line of its item's
//     text, which is still parsed inline (emphasis and links as usual, its
//     marker as text). Text past MAX_INLINE_DEPTH emphasis and link labels,
//     and a line longer than MAX_INLINE, is plain text, as written;
//   - a source longer than SYNC_CHARS is rendered in slices of about
//     SLICE_MS: the first before renderMarkdown returns, the rest in later
//     tasks, inserted in order where the fragment's content ends up;
//   - a table pads its short rows with empty cells only while the padding
//     stays within the table's own size in characters;
//   - past about MAX_NODES nodes, the rest of the document is shown as
//     written, in one paragraph, after a line that says so (cut): the DOM
//     of a malformed 8 MiB source would take gigabytes. Below the bound
//     nothing changes.

import { t } from './i18n.js';

const LIST_RE = /^( *)([-*+]|\d{1,9}[.)])(?: +(.*))?$/;
const HEADING_OPEN_RE = /^ {0,3}(#{1,6})(?=[ \t]|$)/;
const FENCE_RE = /^ {0,3}(`{3,}|~{3,})(.*)$/;
const QUOTE_RE = /^ {0,3}> ?(.*)$/;
const DELIM_CELL_RE = /^ *:?-+:? *$/;
const LINE_BREAK_RE = /[\n\r\u2028\u2029]/;
const WS_RE = /\s/;
const URL_HEAD_RE = /https?:\/\//iy;
const URL_STOP_RE = /[\s<>`]/;
const URL_TRAIL_RE = /[?!.,:;*_~'"]/;
const AUTOLINK_RE = /<((?:https?:\/\/|mailto:)[^\s<>]+)>/iy;
const PUNCT = '\\`*_{}[]()#+-.!|~<>"\'&';
const ENTITIES = {
  amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: '\u00a0', copy: '©', reg: '®', trade: '™',
  ndash: '–', mdash: '—', hellip: '…', lsquo: '‘', rsquo: '’', ldquo: '“', rdquo: '”', laquo: '«', raquo: '»',
  middot: '·', bull: '•', times: '×', divide: '÷', deg: '°', plusmn: '±', le: '≤', ge: '≥', ne: '≠', rarr: '→', larr: '←',
};

// The bounds (see the top of the file).
const MAX_DEPTH = 32;
const MAX_INLINE_DEPTH = 16;
const MAX_INLINE = 100000;
const SYNC_CHARS = 256 * 1024;
const SLICE_MS = 12;
// Checked between blocks, list items, table rows and paragraph lines, so
// it is passed by at most one line's (or row's) nodes. A syllabus-like
// outline has about 0.2 nodes per character: 1.5 MiB of it stays below.
const MAX_NODES = 300000;

// The work between two yields: about this many characters (lines count one
// more). A yield passes through every generator of the nesting (up to
// MAX_DEPTH + 1), so the generators yield only this often, not per line.
const UNIT = 4096;

const now = typeof performance !== 'undefined' && performance.now ? () => performance.now() : () => Date.now();

// The rendering whose work is running (set by run: renderings in slices may
// take turns): the work done since its last yield, the nodes made, whether
// the line about the cut is shown.
let active = null;

// due(n): counts n characters of work; true when it is time to yield.
function due(n) {
  active.units += n + 1;
  if (active.units < UNIT) return false;
  active.units = 0;
  return true;
}

function el(tag, cls) {
  active.nodes++;
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  return e;
}

function txt(s) {
  active.nodes++;
  return document.createTextNode(s);
}

const over = () => active.nodes > MAX_NODES;

// cut(lines, out): past MAX_NODES, the rest as written in one paragraph,
// after the line that says so (once per rendering).
function cut(lines, out) {
  const text = lines.join('\n').trim();
  if (!text) return;
  if (!active.noted) {
    active.noted = true;
    const p = el('p');
    const em = el('em');
    em.append(txt(t('common.markdown_rest_plain')));
    p.append(em);
    out.append(p);
  }
  const p = el('p');
  p.append(txt(text));
  out.append(p);
}

// renderMarkdown(src) → DocumentFragment. A source of up to SYNC_CHARS is
// complete in the fragment; a longer one goes on rendering after the
// fragment is appended, in order, before an empty text node that marks
// where it is (removed at the end). Clearing the container stops it, and
// so does taking the container out of the document once it was in it (a
// dialog closed with remove()); a fragment not appended yet goes on.
export function renderMarkdown(src) {
  const text = String(src || '')
    .replace(/\r\n?/g, '\n')
    .replace(/\t/g, '    ');
  const frag = document.createDocumentFragment();
  const top = { append: (n) => frag.append(n) };
  const ctx = { units: 0, nodes: 0, noted: false };
  const work = blocks(text.split('\n'), top, 0);
  if (text.length <= SYNC_CHARS) {
    run(ctx, work, Infinity);
    return frag;
  }
  if (run(ctx, work, SLICE_MS)) return frag;
  const mark = document.createTextNode('');
  frag.append(mark);
  top.append = (n) => mark.parentNode.insertBefore(n, mark);
  let shown = false; // the marker was in the document
  const next = () => {
    if (!mark.parentNode) return; // the preview was cleared or drawn anew
    if (mark.isConnected) shown = true;
    else if (shown) return; // taken out of the document
    if (run(ctx, work, SLICE_MS)) mark.parentNode.removeChild(mark);
    else setTimeout(next, 0);
  };
  setTimeout(next, 0);
  return frag;
}

// Runs the work of the rendering ctx for about ms (the clock is read at
// each yield); true when it is done.
function run(ctx, work, ms) {
  const prev = active;
  active = ctx;
  try {
    const end = now() + ms;
    for (;;) {
      if (work.next().done) return true;
      if (now() >= end) return false;
    }
  } finally {
    active = prev;
  }
}

function isBlank(line) {
  return !line || !line.trim();
}

function indentOf(line) {
  return line.length - line.trimStart().length;
}

const isSpaceTab = (c) => c === ' ' || c === '\t';

// LIST_RE, FENCE_RE and QUOTE_RE end in "(.*)$": a line with U+2028 or
// U+2029 cannot match them ("." does not match these, and nothing else in
// them does), but they backtrack over it quadratically. Such a line is no
// match, without running them.
const listItem = (line) => (LINE_BREAK_RE.test(line) ? null : LIST_RE.exec(line));
const fenceOpen = (line) => (LINE_BREAK_RE.test(line) ? null : FENCE_RE.exec(line));
const quoteLine = (line) => (LINE_BREAK_RE.test(line) ? null : QUOTE_RE.exec(line));

// An ATX heading: {level, text}, or null. The same as matching
//   /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/
// (text: the lazy group), without its backtracking on runs of spaces.
function heading(line) {
  const m = HEADING_OPEN_RE.exec(line);
  if (!m) return null;
  const rest = line.slice(m[0].length);
  if (LINE_BREAK_RE.test(rest)) return null; // "." does not match these
  let a = 0;
  while (a < rest.length && isSpaceTab(rest[a])) a++;
  // the longest suffix that is ([ \t]+#+)?[ \t]* is not part of the text
  let b = rest.length;
  while (b > a && isSpaceTab(rest[b - 1])) b--;
  let c = b;
  while (c > a && rest[c - 1] === '#') c--;
  if (c < b && c > a && isSpaceTab(rest[c - 1])) {
    b = c;
    while (b > a && isSpaceTab(rest[b - 1])) b--;
  }
  return { level: m[1].length, text: rest.slice(a, b) };
}

// A horizontal rule. The same as matching
//   /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/
// whose backtracking overflows the stack on a long line of "- - - …".
function isRule(line) {
  let k = 0;
  while (k < 3 && line[k] === ' ') k++;
  const c = line[k];
  if (c !== '-' && c !== '*' && c !== '_') return false;
  let marks = 0;
  for (; k < line.length; k++) {
    if (line[k] === c) marks++;
    else if (!isSpaceTab(line[k])) return false;
  }
  return marks >= 3;
}

// A GFM delimiter row. The same as matching
//   /^ *\|? *:?-+:? *(?:\| *:?-+:? *)*\|? *$/
// without its backtracking on runs of spaces.
function isDelimRow(line) {
  let a = 0;
  let b = line.length;
  while (a < b && line[a] === ' ') a++;
  while (b > a && line[b - 1] === ' ') b--;
  if (a < b && line[a] === '|') a++;
  if (a < b && line[b - 1] === '|') b--;
  if (a >= b) return false;
  return line
    .slice(a, b)
    .split('|')
    .every((cell) => DELIM_CELL_RE.test(cell));
}

// A GFM table starts with a header row and a delimiter row that both have
// pipes and the same number of cells (rows longer than MAX_INLINE are
// text, below).
function isTableStart(lines, i) {
  if (i + 1 >= lines.length) return false;
  const head = lines[i];
  const delim = lines[i + 1];
  if (head.length > MAX_INLINE || delim.length > MAX_INLINE) return false;
  if (!head.includes('|') || !delim.includes('|') || !delim.includes('-') || !isDelimRow(delim)) return false;
  return splitRow(head).length === splitRow(delim).length;
}

// Starts a block that ends a table (GFM: "the beginning of another block").
function startsOtherBlock(line) {
  return !!heading(line) || isRule(line) || !!fenceOpen(line) || !!quoteLine(line) || !!listItem(line);
}

// Starts a block that may interrupt a paragraph. As in CommonMark, a
// numbered list only does when it starts at 1 ("2026. Year" goes on as
// text), and an empty list item never does.
function interrupts(lines, i) {
  const line = lines[i];
  if (heading(line) || isRule(line) || fenceOpen(line) || quoteLine(line) || isTableStart(lines, i)) return true;
  const m = listItem(line);
  return !!m && !!m[3] && (!/\d/.test(m[2]) || parseInt(m[2], 10) === 1);
}

// Lines shown as they are written, in one paragraph: what a blockquote has
// past MAX_DEPTH (a list item nested that deep goes on as text of its item
// instead: list()).
function plain(lines, out) {
  const text = lines.join('\n').trim();
  if (!text) return;
  const p = el('p');
  p.append(txt(text));
  out.append(p);
}

// blocks(lines, out, depth) renders the blocks of lines into out (anything
// with append); depth is the number of blockquotes and lists around them.
// A generator: it yields after about UNIT of work (due), so that a long
// source can be rendered in slices.
function* blocks(lines, out, depth) {
  if (depth >= MAX_DEPTH) {
    plain(lines, out);
    return;
  }
  let i = 0;
  while (i < lines.length) {
    if (over()) {
      cut(lines.slice(i), out);
      return;
    }
    const line = lines[i];
    if (due(line.length)) yield;
    if (isBlank(line)) {
      i++;
      continue;
    }
    let m;
    if ((m = fenceOpen(line))) {
      const fence = m[1];
      const closeRe = new RegExp(`^ {0,3}${fence[0] === '`' ? '`' : '~'}{${fence.length},}[ \\t]*$`);
      const body = [];
      i++;
      while (i < lines.length && !closeRe.test(lines[i])) {
        body.push(lines[i]);
        i++;
        if (due(lines[i - 1].length)) yield;
      }
      i++; // closing fence (or end of input)
      const pre = el('pre', 'md-pre');
      const code = el('code');
      code.textContent = body.join('\n');
      active.nodes++;
      pre.append(code);
      out.append(pre);
      continue;
    }
    if (isRule(line)) {
      // before lists: "- - -" is a rule, not a list item (CommonMark)
      out.append(el('hr'));
      i++;
      continue;
    }
    const hd = heading(line);
    if (hd) {
      const node = el('h' + hd.level);
      inline(hd.text, node);
      out.append(node);
      i++;
      continue;
    }
    if (isTableStart(lines, i)) {
      i = yield* table(lines, i, out);
      continue;
    }
    if (quoteLine(line)) {
      const inner = [];
      while (i < lines.length && !isBlank(lines[i]) && (m = quoteLine(lines[i]))) {
        inner.push(m[1]);
        i++;
        if (due(m[1].length)) yield;
      }
      const bq = el('blockquote');
      yield* blocks(inner, bq, depth + 1);
      out.append(bq);
      continue;
    }
    if (listItem(line)) {
      i = yield* list(lines, i, out, -1, depth + 1);
      continue;
    }
    // paragraph
    const start = i;
    const para = [];
    while (i < lines.length && !isBlank(lines[i]) && (para.length === 0 || !interrupts(lines, i))) {
      para.push(lines[i].trimStart());
      i++;
      if (due(lines[i - 1].length)) yield;
    }
    const p = el('p');
    const done = yield* inlineLines(para, p);
    out.append(p);
    if (done < para.length) i = start + done; // past MAX_NODES: cut from there
  }
}

// Paragraph lines: a soft break is a line break inside the paragraph only
// when the line ends with two spaces or a backslash (CommonMark hard break).
// Returns how many lines it rendered: all, or fewer past MAX_NODES (the
// caller cuts the rest).
function* inlineLines(lines, parent) {
  let hard = false;
  for (let k = 0; k < lines.length; k++) {
    if (k) {
      if (over()) return k;
      parent.append(hard ? el('br') : txt(' '));
    }
    const last = k === lines.length - 1;
    let text = lines[k];
    hard = false;
    if (!last && text.endsWith('  ')) hard = true;
    else if (!last && text.endsWith('\\')) {
      text = text.slice(0, -1);
      hard = true;
    }
    if (due(text.length)) yield;
    inline(text.trimEnd(), parent);
  }
  return lines.length;
}

// list(lines, i, out, parentInd, depth) parses a list whose items are
// indented more than parentInd (the parent item's marker, -1 at the top
// level); depth counts this list. LLMs mix 2-, 3- and 4-space nesting, so
// the rule is simpler than CommonMark's: an item indented at least 2
// columns past the current item's marker opens (or continues) its
// sub-list; anything less, but still past the parent's marker, is a
// sibling. Past MAX_DEPTH a sub-list item goes on as text of its item.
// Returns the next line.
function* list(lines, i, out, parentInd, depth) {
  const first = listItem(lines[i]);
  const ordered = /\d/.test(first[2]);
  const node = el(ordered ? 'ol' : 'ul');
  if (ordered) {
    const start = parseInt(first[2], 10);
    if (start !== 1) node.setAttribute('start', String(start));
  }
  let li = null;
  let cur = -1; // marker indentation of the current item
  let paras = 0; // paragraphs already in the current item
  let buf = [];
  // The item's first paragraph goes straight into the <li> (tight list);
  // later ones (after a blank line) are <p> elements.
  function* flush() {
    if (li && buf.length) {
      const target = paras ? el('p') : document.createDocumentFragment();
      const done = yield* inlineLines(buf, target);
      if (done < buf.length) cut(buf.slice(done), target);
      li.append(target);
      paras++;
    }
    buf = [];
  }
  while (i < lines.length) {
    if (over()) break; // past MAX_NODES: blocks() cuts from this line
    const line = lines[i];
    if (due(line.length)) yield;
    if (isBlank(line)) {
      // The list goes on after blank lines when the next line still belongs to it.
      // Each list of the nesting looks past the same blank lines in turn:
      // every line read is counted by its length (a blank line may be long:
      // spaces, wide spaces), the line found too, before it is read whole
      // below (rule, indent, list item) (R80b)
      let j = i + 1;
      while (j < lines.length && isBlank(lines[j])) {
        j++;
        if (due(lines[j - 1].length)) yield;
      }
      if (j < lines.length && due(lines[j].length)) yield;
      if (j >= lines.length || !li || isRule(lines[j])) break;
      const ind = indentOf(lines[j]);
      const goesOn = listItem(lines[j]) ? ind > parentInd && (ind >= cur + 2 || sameKind(lines[j], ordered)) : ind >= cur + 2;
      if (!goesOn) break;
      yield* flush();
      i = j;
      continue;
    }
    const m = listItem(line);
    const ind = indentOf(line);
    if (m) {
      if (isRule(line) || ind <= parentInd) break; // a rule, or an item of an outer list
      if (!li || ind < cur + 2) {
        if (!sameKind(line, ordered)) break;
        yield* flush();
        li = el('li');
        node.append(li);
        cur = ind;
        paras = 0;
        buf = m[3] ? [m[3].trimStart()] : [];
        i++;
        continue;
      }
      if (depth < MAX_DEPTH) {
        yield* flush();
        i = yield* list(lines, i, li, cur, depth + 1);
        continue;
      }
      // nested too deep: a continuation line of the item (below)
    }
    // continuation: indented under the item, or a lazy line of its paragraph
    if (li && (ind >= cur + 2 || (buf.length && !interrupts(lines, i)))) {
      buf.push(line.trimStart());
      i++;
      continue;
    }
    break;
  }
  yield* flush();
  out.append(node);
  return i;
}

function sameKind(line, ordered) {
  const m = listItem(line);
  return !!m && /\d/.test(m[2]) === ordered;
}

// Scan holds the indexes of one text that keep every search of the inline
// parser linear in the text overall. Each is built on first use.
class Scan {
  constructor(text) {
    this.text = text;
    this.n = text.length;
  }

  // The maximal runs of backticks: starts and ends in order, and the
  // starts of the runs of each length.
  runs() {
    if (!this._runs) {
      const s = this.text;
      const starts = [];
      const ends = [];
      const byLen = new Map();
      for (let k = s.indexOf('`'); k >= 0; ) {
        let e = k + 1;
        while (s.charCodeAt(e) === 96) e++;
        starts.push(k);
        ends.push(e);
        const same = byLen.get(e - k);
        if (same) same.push(k);
        else byLen.set(e - k, [k]);
        k = s.indexOf('`', e);
      }
      this._runs = { starts, ends, byLen };
    }
    return this._runs;
  }

  // The end of the run of backticks that the backtick at i is in.
  runEnd(i) {
    const { starts, ends } = this.runs();
    let lo = 0;
    let hi = starts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (starts[mid] <= i) lo = mid;
      else hi = mid - 1;
    }
    return ends[lo];
  }

  // The end of the code span whose opening backtick run starts at i (the
  // index after the closing run: the next run of exactly as many
  // backticks), or -1 when the run is not closed.
  codeEnd(i) {
    const e0 = this.runEnd(i);
    const n = e0 - i;
    const same = this.runs().byLen.get(n);
    if (!same) return -1;
    let lo = 0;
    let hi = same.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (same[mid] < e0) lo = mid + 1;
      else hi = mid;
    }
    return lo < same.length ? same[lo] + n : -1;
  }

  // Finds the closing delimiter for an emphasis run starting at i. Code
  // spans and escaped characters in between are skipped, so `a*b` inside
  // *…* does not end the emphasis. A single * or _ is not half of a double
  // one, and a closing _ is not followed by a letter or digit.
  //
  // The scan from any position takes the same steps, so the answer for
  // each position on the way is kept (memo: -2 unknown, -1 none).
  closing(i, delim) {
    const s = this.text;
    const start = i + delim.length;
    if (start >= this.n || WS_RE.test(s[start])) return -1;
    // the first position is never a closer: step over it
    let p = start + 1;
    if (s[start] === '\\') p = start + 2;
    else if (s[start] === '`') {
      const e = this.codeEnd(start);
      if (e > 0) p = e;
    }
    const memo = ((this._close ||= {})[delim] ||= new Int32Array(this.n).fill(-2));
    const path = [];
    let found = -1;
    while (p < this.n) {
      if (memo[p] !== -2) {
        found = memo[p];
        break;
      }
      const c = s[p];
      let next = p + 1;
      if (c === '\\') next = p + 2;
      else if (c === '`') {
        const e = this.codeEnd(p);
        if (e > 0) next = e;
      } else if (s.startsWith(delim, p) && !WS_RE.test(s[p - 1])) {
        if (delim.length === 1 && s[p + 1] === delim) next = p + 2;
        else if (!(delim === '_' && isWordChar(s[p + 1]))) {
          found = p;
          break;
        }
      }
      path.push(p);
      p = next;
    }
    for (const q of path) memo[q] = found;
    return found;
  }

  // The ']' that closes the '[' at i, or -1. Brackets nest; escaped
  // characters and code spans are skipped. close[p]: from p on, the first
  // ']' that is not closed by a '[' at or after p.
  bracket(i) {
    if (!this._close1) {
      const s = this.text;
      const n = this.n;
      const close = new Int32Array(n + 2).fill(-1);
      for (let p = n - 1; p >= 0; p--) {
        const c = s[p];
        let e;
        if (c === '\\') close[p] = close[p + 2];
        else if (c === '`' && (e = this.codeEnd(p)) > 0) close[p] = close[e];
        else if (c === ']') close[p] = p;
        else if (c === '[') close[p] = close[p + 1] < 0 ? -1 : close[close[p + 1] + 1];
        else close[p] = close[p + 1];
      }
      this._close1 = close;
    }
    return this._close1[i + 1];
  }

  // The end of a link destination starting at k: the first whitespace, or
  // ')' that is not balanced by a '(' after k; a backslash escapes the
  // next character.
  destEnd(k) {
    if (!this._dest) {
      const s = this.text;
      const n = this.n;
      const end = new Int32Array(n + 2).fill(n);
      for (let p = n - 1; p >= 0; p--) {
        const c = s[p];
        if (c === '\\' && p + 1 < n) end[p] = end[p + 2];
        else if (WS_RE.test(c) || c === ')') end[p] = p;
        else if (c === '(') {
          const q = end[p + 1];
          end[p] = q < n && s[q] === ')' ? end[q + 1] : q;
        } else end[p] = end[p + 1];
      }
      this._dest = end;
    }
    return this._dest[k];
  }

  // next(test)[k]: the first index at or after k whose character passes
  // test (n when none). Cached under key.
  next(key, test) {
    const cache = (this._next ||= {});
    if (!cache[key]) {
      const s = this.text;
      const n = this.n;
      const at = new Int32Array(n + 1).fill(n);
      for (let p = n - 1; p >= 0; p--) at[p] = test(s[p]) ? p : at[p + 1];
      cache[key] = at;
    }
    return cache[key];
  }

  // indexOf(ch, k) for the characters a link looks for.
  find(ch, k) {
    if (k >= this.n) return -1;
    const at = this.next('=' + ch, (c) => c === ch)[k];
    return at < this.n ? at : -1;
  }

  // The first index at or after k that is not a space.
  spaces(k) {
    if (this.text[k] !== ' ') return k;
    return this.next('nonspace', (c) => c !== ' ')[k];
  }

  // count(ch)[k]: how many ch there are before k.
  count(ch) {
    const cache = (this._count ||= {});
    if (!cache[ch]) {
      const s = this.text;
      const pre = new Int32Array(this.n + 1);
      for (let p = 0; p < this.n; p++) pre[p + 1] = pre[p] + (s[p] === ch ? 1 : 0);
      cache[ch] = pre;
    }
    return cache[ch];
  }

  // A bare http(s) URL at i (GFM autolink extension): trailing punctuation
  // and an unbalanced closing parenthesis are not part of it.
  bareURL(i) {
    const s = this.text;
    URL_HEAD_RE.lastIndex = i;
    if (!URL_HEAD_RE.test(s)) return null;
    const b = URL_HEAD_RE.lastIndex; // after "://"
    const end = this.next('stop', (c) => URL_STOP_RE.test(c))[b];
    if (end === b) return null;
    // The tail: the characters before end that may be trimmed. Trailing
    // punctuation is; a ')' is while the URL has more ')' than '(' (the
    // tail has no '(', and "http://" ends with '/', which is not in it).
    if (!this._tail) {
      const ts = new Int32Array(this.n + 1);
      for (let p = 1; p <= this.n; p++) ts[p] = URL_TRAIL_RE.test(s[p - 1]) || s[p - 1] === ')' ? ts[p - 1] : p;
      this._tail = ts;
    }
    const ts = this._tail[end];
    let e = ts;
    const close = this.count(')');
    const parens = close[end] - close[ts]; // the ')' of the tail
    if (parens > 0) {
      const open = this.count('(');
      // trimming stops at the k-th ')' of the tail (k: the surplus of '(')
      const k = Math.min(open[ts] - open[b] - (close[ts] - close[b]), parens);
      if (k > 0) {
        let lo = ts;
        let hi = end - 1;
        while (lo < hi) {
          const mid = (lo + hi) >> 1;
          if (close[mid + 1] - close[ts] >= k) hi = mid;
          else lo = mid + 1;
        }
        e = lo + 1;
      }
    }
    return e > b && !'/?#'.includes(s[b]) ? s.slice(i, e) : null;
  }
}

// Table cells: outer pipes are optional, \| is a literal pipe, and a pipe
// inside a (closed) code span does not split the cell.
function splitRow(line) {
  let s = line.trim();
  if (s.startsWith('|')) s = s.slice(1);
  if (s.endsWith('|') && !s.endsWith('\\|')) s = s.slice(0, -1);
  const scan = new Scan(s);
  const cells = [];
  let cur = '';
  for (let k = 0; k < s.length; k++) {
    const c = s[k];
    if (c === '\\' && s[k + 1] === '|') {
      cur += '\\|'; // unescaped by inline()
      k++;
    } else if (c === '`') {
      const e = scan.codeEnd(k);
      if (e > 0) {
        cur += s.slice(k, e).replace(/\\\|/g, '|'); // GFM: \| is a pipe in code too
        k = e - 1;
      } else {
        cur += c;
      }
    } else if (c === '|') {
      cells.push(cur.trim());
      cur = '';
    } else {
      cur += c;
    }
  }
  cells.push(cur.trim());
  return cells;
}

function* table(lines, i, out) {
  const head = splitRow(lines[i]);
  const aligns = splitRow(lines[i + 1]).map((d) => {
    const l = d.startsWith(':');
    const r = d.endsWith(':');
    return l && r ? 'center' : r ? 'right' : l ? 'left' : '';
  });
  // the empty cells that may still pad short rows: the table's characters
  // so far, at least 10 000, which no normal table reaches (-1 once spent:
  // no row is padded after that)
  let room = Math.max(lines[i].length + lines[i + 1].length, 10000);
  i += 2;
  const wrap = el('div', 'md-table-wrap');
  const tbl = el('table');
  const thead = el('thead');
  const tr = el('tr');
  for (let k = 0; k < head.length; k++) {
    if (due(head[k].length)) yield;
    tr.append(cell('th', head[k], aligns[k]));
  }
  thead.append(tr);
  tbl.append(thead);
  const tbody = el('tbody');
  while (i < lines.length && !over() && !isBlank(lines[i]) && lines[i].includes('|') && lines[i].length <= MAX_INLINE && !startsOtherBlock(lines[i])) {
    if (due(lines[i].length)) yield;
    const cells = splitRow(lines[i]);
    const own = Math.min(cells.length, head.length);
    let width = head.length;
    if (room >= 0) room += lines[i].length;
    if (room >= head.length - own) room -= head.length - own;
    else {
      room = -1;
      width = own;
    }
    const row = el('tr');
    for (let k = 0; k < width; k++) {
      const text = cells[k] || '';
      if (due(text.length)) yield;
      row.append(cell('td', text, aligns[k]));
    }
    tbody.append(row);
    i++;
  }
  tbl.append(tbody);
  wrap.append(tbl);
  out.append(wrap);
  return i;
}

function cell(tag, text, align) {
  const c = el(tag);
  if (align) c.style.textAlign = align;
  inline(text, c);
  return c;
}

function safeHref(url) {
  const u = url.trim().replace(/\\([!-/:-@[-`{-~])/g, '$1');
  return /^(https?:\/\/|mailto:)\S+$/i.test(u) ? u : null;
}

function anchor(href) {
  const a = el('a');
  a.setAttribute('href', href);
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  return a;
}

const isWordChar = (c) => !!c && /[\p{L}\p{N}]/u.test(c);

// parseLink(scan, i) reads [label](destination "title") with text[i] === '['.
// Brackets in the label may nest; the destination may contain balanced
// parentheses or be written as <…>. Returns {label, url, end} or null.
function parseLink(scan, i) {
  const text = scan.text;
  const j = scan.bracket(i);
  if (j < 0 || text[j + 1] !== '(') return null;
  let k = scan.spaces(j + 2);
  let from;
  let to;
  if (text[k] === '<') {
    const e = scan.find('>', k);
    if (e < 0) return null;
    from = k + 1;
    to = e;
    k = e + 1;
  } else {
    from = k;
    k = to = scan.destEnd(k);
  }
  k = scan.spaces(k);
  const q = text[k];
  if (q === '"' || q === "'" || (q === '(' && k > j + 2)) {
    const e = scan.find(q === '(' ? ')' : q, k + 1);
    if (e < 0) return null;
    k = scan.spaces(e + 1);
  }
  if (text[k] !== ')') return null;
  return { label: text.slice(i + 1, j), url: text.slice(from, to), end: k + 1 };
}

// An HTML entity at i: {ch, len}, or null (then '&' is a plain character).
function entity(text, i) {
  const m = /^&(?:#(\d{1,7})|#[xX]([0-9a-fA-F]{1,6})|([A-Za-z][A-Za-z0-9]{1,31}));/.exec(text.slice(i, i + 40));
  if (!m) return null;
  let ch;
  if (m[3]) ch = ENTITIES[m[3]];
  else {
    const cp = m[1] ? parseInt(m[1], 10) : parseInt(m[2], 16);
    ch = cp > 0 && cp <= 0x10ffff && !(cp >= 0xd800 && cp <= 0xdfff) ? String.fromCodePoint(cp) : '\ufffd';
  }
  return ch == null ? null : { ch, len: m[0].length };
}

// inline(text, parent, inLink, depth): appends the inline nodes of text to
// parent. Inside a link label no further links are made. Text nested past
// MAX_INLINE_DEPTH, or longer than MAX_INLINE, is shown as written.
function inline(text, parent, inLink = false, depth = 0) {
  if (depth >= MAX_INLINE_DEPTH || text.length > MAX_INLINE) {
    if (text) parent.append(txt(text));
    return;
  }
  const scan = new Scan(text);
  let buf = '';
  const flush = () => {
    if (buf) parent.append(txt(buf));
    buf = '';
  };
  let i = 0;
  while (i < text.length) {
    const c = text[i];
    if (c === '\\' && i + 1 < text.length && PUNCT.includes(text[i + 1])) {
      buf += text[i + 1];
      i += 2;
      continue;
    }
    if (c === '`') {
      const e = scan.codeEnd(i);
      const n = scan.runEnd(i) - i;
      if (e > 0) {
        flush();
        const code = el('code');
        code.textContent = text.slice(i + n, e - n).replace(/^ (.*) $/, '$1');
        active.nodes++;
        parent.append(code);
        i = e;
        continue;
      }
      buf += text.slice(i, i + n);
      i += n;
      continue;
    }
    if (c === '<') {
      const br = /^<br\s*\/?>/i.exec(text.slice(i, i + 8));
      if (br) {
        flush();
        parent.append(el('br'));
        i += br[0].length;
        continue;
      }
      AUTOLINK_RE.lastIndex = i;
      const auto = AUTOLINK_RE.exec(text);
      const href = auto && !inLink && safeHref(auto[1]);
      if (href) {
        flush();
        const a = anchor(href);
        a.append(txt(auto[1]));
        parent.append(a);
        i += auto[0].length;
        continue;
      }
    }
    if (c === '&') {
      const ent = entity(text, i);
      if (ent) {
        buf += ent.ch;
        i += ent.len;
        continue;
      }
    }
    if (c === '!' && text[i + 1] === '[') {
      const lk = parseLink(scan, i + 1);
      if (lk) {
        flush();
        inline(lk.label, parent, true, depth + 1); // image: its alt text only
        i = lk.end;
        continue;
      }
    }
    if (c === '[') {
      const lk = parseLink(scan, i);
      if (lk) {
        flush();
        const href = !inLink && safeHref(lk.url);
        if (href) {
          const a = anchor(href);
          inline(lk.label, a, true, depth + 1);
          parent.append(a);
        } else {
          inline(lk.label, parent, inLink, depth + 1);
        }
        i = lk.end;
        continue;
      }
    }
    if ((c === 'h' || c === 'H') && !inLink && !isWordChar(text[i - 1])) {
      const url = scan.bareURL(i);
      if (url) {
        flush();
        const a = anchor(url);
        a.append(txt(url));
        parent.append(a);
        i += url.length;
        continue;
      }
    }
    const two = text.slice(i, i + 2);
    if (two === '**' || two === '__' || two === '~~') {
      const j = two === '__' && isWordChar(text[i - 1]) ? -1 : scan.closing(i, two);
      if (j > 0) {
        flush();
        const node = el(two === '~~' ? 'del' : 'strong');
        inline(text.slice(i + 2, j), node, inLink, depth + 1);
        parent.append(node);
        i = j + 2;
        continue;
      }
      buf += two;
      i += 2;
      continue;
    }
    if (c === '*' || c === '_') {
      const j = c === '_' && isWordChar(text[i - 1]) ? -1 : scan.closing(i, c);
      if (j > 0) {
        flush();
        const em = el('em');
        inline(text.slice(i + 1, j), em, inLink, depth + 1);
        parent.append(em);
        i = j + 1;
        continue;
      }
    }
    buf += c;
    i++;
  }
  flush();
}
