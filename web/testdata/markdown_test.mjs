// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// markdown.js (the SYLLABUS.md preview and the third-party notices), run in
// Node by web/markdown_test.go with the web directory as a file: URL, on a
// stand-in DOM. AUD5 low-3: malformed input must not hang the page.
// - markdown_golden.json: the output of typical and hostile documents, as
//   the renderer wrote it before the change, stays the same; the output
//   only ever has the allowed elements and attributes, and links are only
//   http(s) or mailto;
// - a source longer than what is rendered at once is rendered in slices
//   (renderMarkdown returns first) to the same output, and clearing the
//   container stops it;
// - the inputs that took time quadratic in the line (unclosed "*a ", "[a](",
//   nested labels, runs of spaces after "#", …) or overflowed the stack
//   (50 000 ">") each finish within LIMIT_MS: every one runs in a worker
//   that is terminated at the limit, so a slow renderer fails at once;
// - nesting past the bounds is shown as plain text.
// Stand-in nodes are compared with assert.ok(a === b), never assert.equal.

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { isMainThread, Worker, workerData, parentPort } from 'node:worker_threads';

const LIMIT_MS = 4000;
// Probes whose slow version stays under LIMIT_MS: a bound on the rendering
// time measured in the worker (without its start), with a wide margin.
const RENDER_MS = { 'blank-lines-after-deep-list': 1000 };
// Probes whose page would be blocked too long at once while the whole
// rendering stays under LIMIT_MS: a bound on the longest slice (the
// renderMarkdown call itself, or one of the later tasks it sets).
const SLICE_MAX_MS = { 'long-line-after-deep-list-and-blank': 100, 'wide-blank-lines-after-deep-list': 100 };

class Node0 {
  constructor() {
    this.parentNode = null;
    this.childNodes = [];
  }
  get firstChild() {
    return this.childNodes[0] || null;
  }
  get lastChild() {
    return this.childNodes[this.childNodes.length - 1] || null;
  }
  // in the stand-in document: under page (below)
  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return n === page;
  }
  detach() {
    if (this.parentNode) {
      const a = this.parentNode.childNodes;
      a.splice(a.lastIndexOf(this), 1);
      this.parentNode = null;
    }
  }
  adopt(n) {
    if (n instanceof Frag) {
      const kids = n.childNodes.splice(0);
      for (const k of kids) k.parentNode = null;
      return kids;
    }
    n.detach();
    return [n];
  }
  append(...ns) {
    for (const n of ns) {
      for (const k of this.adopt(typeof n === 'string' ? new Text(n) : n)) {
        k.parentNode = this;
        this.childNodes.push(k);
      }
    }
  }
  insertBefore(n, ref) {
    let at = this.childNodes.lastIndexOf(ref);
    if (at < 0) throw new Error('insertBefore: the reference is not a child');
    for (const k of this.adopt(n)) {
      k.parentNode = this;
      this.childNodes.splice(at++, 0, k);
    }
    return n;
  }
  removeChild(n) {
    if (n.parentNode !== this) throw new Error('removeChild: not a child');
    n.detach();
    return n;
  }
}
class Text extends Node0 {
  constructor(s) {
    super();
    this.data = String(s);
  }
  get textContent() {
    return this.data;
  }
}
class Frag extends Node0 {}
class El extends Node0 {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.attrs = new Map();
    this.style = {};
  }
  setAttribute(k, v) {
    this.attrs.set(k, String(v));
  }
  set className(v) {
    this.attrs.set('class', String(v));
  }
  set target(v) {
    this.attrs.set('target', String(v));
  }
  set rel(v) {
    this.attrs.set('rel', String(v));
  }
  set textContent(v) {
    for (const k of this.childNodes) k.parentNode = null;
    this.childNodes = v === '' ? [] : [new Text(v)];
    for (const k of this.childNodes) k.parentNode = this;
  }
  get textContent() {
    return this.childNodes.map((c) => c.textContent).join('');
  }
}
// the stand-in document's root: a node under it is connected
const page = new El('html');
globalThis.document = {
  documentElement: {},
  createElement: (t) => new El(t),
  createTextNode: (s) => new Text(s),
  createDocumentFragment: () => new Frag(),
};
// what i18n.js needs to load the catalogs (setLocale below)
globalThis.window = { dispatchEvent: () => true };

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
// serialize: the children of n as HTML (attributes sorted, style as key:value)
function serialize(n) {
  const out = [];
  const walk = (n) => {
    if (n instanceof Text) {
      out.push(esc(n.data));
      return;
    }
    const tag = n.tagName.toLowerCase();
    let a = '';
    for (const [k, v] of [...n.attrs].sort((x, y) => (x[0] < y[0] ? -1 : 1))) a += ` ${k}="${esc(v).replace(/"/g, '&quot;')}"`;
    const st = Object.entries(n.style);
    if (st.length) a += ` style="${esc(st.map(([k, v]) => `${k}:${v}`).join(';'))}"`;
    out.push(`<${tag}${a}>`);
    if ((tag === 'br' || tag === 'hr') && !n.childNodes.length) return;
    n.childNodes.forEach(walk);
    out.push(`</${tag}>`);
  };
  n.childNodes.forEach(walk);
  return out.join('');
}

// Everything the renderer may produce (AUD5 appendix A): these elements,
// these attributes, links that are http(s) or mailto only. Answers the
// problems found (an empty list when there are none); iterative, so a deep
// tree does not overflow the stack here.
const TAGS = new Set('P H1 H2 H3 H4 H5 H6 UL OL LI BLOCKQUOTE PRE CODE HR BR DIV TABLE THEAD TBODY TR TH TD STRONG EM DEL A'.split(' '));
function unsafe(root) {
  const bad = [];
  const stack = [...root.childNodes];
  while (stack.length && bad.length < 5) {
    const n = stack.pop();
    if (n instanceof Text) continue;
    if (!TAGS.has(n.tagName)) bad.push('element ' + n.tagName);
    for (const [k, v] of n.attrs) {
      const ok =
        (n.tagName === 'A' && k === 'href' && /^(https?:\/\/|mailto:)\S+$/i.test(v)) ||
        (n.tagName === 'A' && k === 'target' && v === '_blank') ||
        (n.tagName === 'A' && k === 'rel' && v === 'noopener noreferrer') ||
        (n.tagName === 'OL' && k === 'start' && /^\d+$/.test(v)) ||
        (n.tagName === 'PRE' && k === 'class' && v === 'md-pre') ||
        (n.tagName === 'DIV' && k === 'class' && v === 'md-table-wrap');
      if (!ok) bad.push(`${n.tagName} ${k}=${JSON.stringify(v).slice(0, 80)}`);
    }
    for (const [k, v] of Object.entries(n.style)) {
      if (!((n.tagName === 'TH' || n.tagName === 'TD') && k === 'textAlign' && ['left', 'right', 'center'].includes(v))) bad.push(`${n.tagName} style ${k}=${v}`);
    }
    if (n.tagName === 'A' && !n.attrs.has('href')) bad.push('A without href');
    for (const c of n.childNodes) stack.push(c);
  }
  return bad;
}

const isMark = (n) => n instanceof Text && n.data === '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Waits (at most maxMs) until a rendering in root is complete: a sliced one
// keeps an empty text node at the end until then.
async function finish(root, maxMs = 20000) {
  const end = Date.now() + maxMs;
  while (isMark(root.lastChild)) {
    if (Date.now() > end) throw new Error(`the rendering did not finish in ${maxMs} ms`);
    await sleep(0);
  }
}

function count(root) {
  let n = 0;
  const stack = [root];
  while (stack.length) {
    const k = stack.pop();
    n++;
    for (const c of k.childNodes) stack.push(c);
  }
  return n;
}

// The probe sources: each pattern line is just under the 100 000 characters
// past which a line is plain text, five times, so that the scans run.
function lines(line, times = 5, sep = '\n\n') {
  return Array(times).fill(line).join(sep);
}
const PROBES = {
  'unclosed-em': () => lines('*a '.repeat(30000)),
  'unclosed-strong': () => lines('**a '.repeat(22000)),
  'unclosed-underscore': () => lines('_a '.repeat(30000)),
  'unclosed-strike': () => lines('~~a '.repeat(22000)),
  'unclosed-in-one-paragraph': () => lines('*a '.repeat(30000), 5, '\n'),
  'open-links': () => lines('[a]('.repeat(22000)),
  'open-brackets': () => lines('['.repeat(90000)),
  'nested-labels': () => lines('['.repeat(18000) + 'a' + '](x)'.repeat(18000)),
  'open-angle-destinations': () => lines('[a](<'.repeat(18000)),
  'open-titles': () => lines('[a](b ('.repeat(12000)),
  'code-spans-in-labels': () => lines('[`a'.repeat(30000)),
  'backtick-run': () => lines('*a' + '`'.repeat(90000)),
  'url-tails': () => lines('http:///'.repeat(5600) + '.'.repeat(45000)),
  'url-parentheses': () => lines('x http://a' + ')'.repeat(90000)),
  'heading-spaces': () => lines('# a' + ' '.repeat(90000) + 'b'),
  'delimiter-spaces': () => lines('a|b\n' + ' '.repeat(90000) + '|-x'),
  'trailing-spaces': () => lines('a' + ' '.repeat(90000) + 'x\nb'),
  // R80 medium-2: the list and fence expressions end in "(.*)$" and
  // backtracked quadratically over a U+2028 / U+2029 they cannot match
  'list-line-separator': () => lines('-' + ' '.repeat(90000) + '\u2028'),
  'fence-line-separator': () => lines('`'.repeat(90000) + '\u2029'),
  'deep-quote': () => '>'.repeat(50000) + ' x',
  'deep-list': () => Array.from({ length: 300 }, (_, k) => ' '.repeat(2 * k) + '- a').join('\n'),
  // R80 medium-1: every one of 32 nested lists looks past the same blank
  // lines; one yield per blank line went through all their generators
  // (3 s at once, under the size rendered in slices)
  'blank-lines-after-deep-list': () => {
    const items = Array.from({ length: 32 }, (_, k) => ' '.repeat(2 * k) + '- a').join('\n');
    const unit = items + '\n'.repeat(20001) + 'x\n';
    return unit.repeat(20).slice(0, 262144);
  },
  // R80b low: the line found after the blank lines was read whole (rule,
  // indent, list item) by each of 32 nested lists between two yields (one
  // slice of 0.9 s for an 8 MiB line ending in a U+2028), and a blank line
  // counted as one character however long (wide spaces trimmed slowly)
  'long-line-after-deep-list-and-blank': () => Array.from({ length: 32 }, (_, k) => ' '.repeat(2 * k) + '- a').join('\n') + '\n\n' + '- '.repeat(4 << 20) + '\u2028',
  'wide-blank-lines-after-deep-list': () => Array.from({ length: 32 }, (_, k) => ' '.repeat(2 * k) + '- a').join('\n') + '\n\n' + Array(84).fill('\u3000'.repeat(100000)).join('\n') + '\n\n  x',
  'rule-line': () => '- '.repeat(4000000) + 'x',
  'padded-table': () => '|a'.repeat(1000) + '|\n' + '|-'.repeat(1000) + '|\n' + '|\n'.repeat(200000),
  'outline-2mib': () => outline(2 << 20),
};

// a syllabus-like outline of about n characters (harmless marker values)
function outline(n) {
  const unit = [
    '# CSC0000 Example — Week {i}',
    '',
    'This week: **graphs**, *paths* and `bfs`. See [notes](https://example.org/{i}) or https://example.org/w{i}.',
    '',
    '1. Search',
    '   - pre-order',
    '2. Sort',
    '',
    '| Week | Topic |',
    '|:-----|------:|',
    '| {i} | Graphs |',
    '',
    '> Quiz on _Friday_ &mdash; bring a pen.',
    '',
    '```',
    'def bfs(g):',
    '```',
    '',
  ].join('\n');
  let out = '';
  for (let i = 1; out.length < n; i++) out += unit.replaceAll('{i}', String(i));
  return out;
}

const base = isMainThread ? process.argv[2] : workerData.base;
globalThis.fetch = async (url) => {
  const u = String(url);
  if (!/^i18n\/[\w-]+\.json$/.test(u)) throw new Error(`unexpected request ${u}`);
  const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
  return { ok: true, status: 200, json: async () => cat };
};
const I = await import(new URL('i18n.js', base).href);
await I.setLocale('zh-CN');
const M = await import(new URL('markdown.js', base).href);

if (!isMainThread) {
  // one probe: render it to the end, answer how long it took and its
  // longest slice (renderMarkdown itself, or a later task it set)
  const src = PROBES[workerData.probe]();
  const root = new El('div');
  let slice = 0;
  const realSetTimeout = globalThis.setTimeout;
  globalThis.setTimeout = (fn, ms, ...args) =>
    realSetTimeout(() => {
      const s = performance.now();
      try {
        fn(...args);
      } finally {
        slice = Math.max(slice, performance.now() - s);
      }
    }, ms);
  const t0 = performance.now();
  const frag = M.renderMarkdown(src);
  slice = Math.max(slice, performance.now() - t0);
  root.append(frag);
  await finish(root, LIMIT_MS * 2);
  const ms = performance.now() - t0;
  parentPort.postMessage({ ms, slice, chars: src.length, nodes: count(root), unsafe: unsafe(root) });
} else {
  await main();
}

function runProbe(name) {
  return new Promise((resolve, reject) => {
    const w = new Worker(new URL(import.meta.url), { workerData: { base, probe: name }, resourceLimits: { maxOldGenerationSizeMb: 1024 } });
    let done = false;
    const end = (fn, v) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      w.terminate();
      fn(v);
    };
    const timer = setTimeout(() => end(reject, new Error(`${name}: not rendered within ${LIMIT_MS} ms`)), LIMIT_MS);
    w.once('message', (m) => end(resolve, m));
    w.once('error', (e) => end(reject, new Error(`${name}: ${e.name}: ${e.message}`)));
    w.once('exit', (code) => end(reject, new Error(`${name}: the worker exited (${code}) without an answer`)));
  });
}

function render(src) {
  const root = new El('div');
  root.append(M.renderMarkdown(src));
  return root;
}

async function main() {
  const golden = JSON.parse(await readFile(new URL('testdata/markdown_golden.json', base), 'utf8'));

  // ---- the output stays as it was, and only has what is allowed ----
  for (const c of golden.cases) {
    const root = render(c.src);
    assert.ok(!isMark(root.lastChild), `${c.name}: a small source is rendered at once`);
    assert.equal(serialize(root), c.html, `${c.name}: the same output as before`);
    assert.deepEqual(unsafe(root), [], `${c.name}: only the allowed elements and attributes`);
  }

  // ---- a long source: rendered in slices, to the same output ----
  const SEP = '\n\n---\n\n';
  const parts = golden.cases.filter((c) => golden.joinable.includes(c.name));
  assert.ok(parts.length >= 20, 'most cases can be joined');
  let doc = '';
  let want = '';
  while (doc.length < 600000) {
    for (const c of parts) {
      doc += (doc ? SEP : '') + c.src;
      want += (want ? '<hr>' : '') + c.html;
    }
  }
  {
    const root = render(doc);
    const first = root.childNodes.length;
    assert.ok(isMark(root.lastChild), 'a long source is not complete when renderMarkdown returns');
    await finish(root);
    assert.ok(root.childNodes.length > first, 'the rest came in later slices');
    assert.ok(serialize(root) === want, 'the sliced rendering is the same as the parts rendered at once');
  }
  {
    // the slices go where the fragment's content went, even when it is
    // appended later
    const frag = M.renderMarkdown(doc);
    await sleep(5);
    const root = new El('div');
    root.append(frag);
    await finish(root);
    assert.ok(serialize(root) === want, 'appended after a while: the same output');
  }
  {
    // clearing the container (dom.js clear) stops it; nothing more is added
    const root = render(doc);
    const mark = root.lastChild;
    assert.ok(isMark(mark), 'still rendering');
    while (root.firstChild) root.removeChild(root.firstChild);
    const other = new El('p');
    root.append(other);
    await sleep(100);
    assert.ok(root.childNodes.length === 1 && root.firstChild === other, 'nothing is added after the container was cleared');
    assert.ok(mark.parentNode === null, 'the marker stays out');
  }
  {
    // R80 low-2: a container taken out of the document (a dialog closed
    // with remove()) stops it too, once it was in the document
    const dialog = new El('dialog');
    page.append(dialog);
    dialog.append(M.renderMarkdown(doc + SEP + doc + SEP + doc));
    await sleep(20); // a slice or more in the document
    assert.ok(isMark(dialog.lastChild), 'still rendering');
    dialog.detach();
    await sleep(0); // a slice that may already be due
    const kept = dialog.childNodes.length;
    await sleep(100);
    assert.equal(dialog.childNodes.length, kept, 'nothing is added after the container left the document');
    assert.ok(isMark(dialog.lastChild), 'the rendering stopped before its end');
  }

  // ---- nesting past the bounds is plain text ----
  {
    // 40 ">": 32 blockquotes, then the rest as written
    const root = render('>'.repeat(40) + ' x');
    let n = root.firstChild;
    let depth = 0;
    while (n && n.tagName === 'BLOCKQUOTE') {
      depth++;
      n = n.firstChild;
    }
    assert.equal(depth, 32, 'blockquotes nest 32 deep');
    assert.ok(n && n.tagName === 'P', 'then a paragraph');
    assert.equal(n.textContent, '>'.repeat(8) + ' x', 'with the rest of the line as written');
  }
  {
    // 40 nested list items: 32 lists, the deeper items are text of the last
    const src = Array.from({ length: 40 }, (_, k) => ' '.repeat(2 * k) + '- i' + k).join('\n');
    const root = render(src);
    let n = root.firstChild;
    let depth = 0;
    let li = null;
    while (n && n.tagName === 'UL') {
      depth++;
      li = n.firstChild;
      n = li.childNodes.find((c) => c instanceof El && c.tagName === 'UL');
    }
    assert.equal(depth, 32, 'lists nest 32 deep');
    assert.equal(li.textContent, Array.from({ length: 9 }, (_, k) => (k ? '- i' : 'i') + (31 + k)).join(' '), 'the deeper items go on as text');
  }
  {
    // 20 nested link labels: 16 levels, then the label as written
    const src = '['.repeat(20) + 'a' + '](https://e.x)'.repeat(20);
    const root = render(src);
    const p = root.firstChild;
    assert.ok(p.firstChild instanceof El && p.firstChild.tagName === 'A', 'the outer label is a link');
    assert.equal(p.firstChild.attrs.get('href'), 'https://e.x');
    assert.equal(p.textContent, '[[[[a](https://e.x)](https://e.x)](https://e.x)](https://e.x)', 'the labels past 16 levels are shown as written');
  }
  {
    // a line longer than 100 000 characters is text as written
    const long = render('*b* ' + 'x'.repeat(100000));
    assert.ok(long.firstChild.childNodes.length === 1 && long.firstChild.firstChild instanceof Text, 'a long line is one text node');
    const fits = render('*b* ' + 'x'.repeat(99990));
    assert.ok(fits.firstChild.firstChild instanceof El && fits.firstChild.firstChild.tagName === 'EM', 'a line just under the bound is parsed');
  }
  {
    // short rows are padded with empty cells while the padding stays within
    // the table's size (at least 10 000 cells); past it a row has its own
    // cells only
    const src = '|' + 'h|'.repeat(50) + '\n|' + '-|'.repeat(50) + '\n' + '| x |\n'.repeat(300);
    const root = render(src);
    const rows = root.firstChild.firstChild.childNodes[1].childNodes;
    assert.equal(rows.length, 300);
    assert.equal(rows[0].childNodes.length, 50, 'the first short rows are padded');
    assert.equal(rows[226].childNodes.length, 50, 'while the padding stays within the bound');
    assert.equal(rows[227].childNodes.length, 1, 'past the bound, a row has its own cells');
    assert.ok(rows.slice(227).every((r) => r.childNodes.length === 1), 'and so do all the rows after it');
    // R80 note 3: a compact table with bare "|" rows is padded as before
    const compact = render('|W|D|T|R|L|Q|U|N|\n|-|-|-|-|-|-|-|-|\n' + '|\n'.repeat(100));
    const crows = compact.firstChild.firstChild.childNodes[1].childNodes;
    assert.ok(crows.length === 100 && crows.every((r) => r.childNodes.length === 8), 'every short row of a compact table is padded');
  }

  // ---- R80 low-1: past about 300 000 nodes, the rest as written after a
  // line that says so (below the bound: the golden checks above) ----
  const zh = JSON.parse(await readFile(new URL('i18n/zh-CN.json', base), 'utf8'));
  const NOTE = zh['common.markdown_rest_plain'];
  assert.ok(NOTE && !/[{}]/.test(NOTE), 'the line has a zh-CN text');
  const isNote = (n) => n instanceof El && n.tagName === 'P' && n.childNodes.length === 1 && n.firstChild.tagName === 'EM' && n.textContent === NOTE;
  {
    // a list: its first items, then the line, then the rest of the source
    const N = 200000;
    const root = render('- a\n'.repeat(N));
    await finish(root);
    assert.equal(root.childNodes.length, 3, 'the list, the line, the rest');
    const [ul, note, rest] = root.childNodes;
    const items = ul.childNodes.length;
    assert.ok(ul.tagName === 'UL' && items > 140000 && items < 160000, `${items} items before the cut`);
    assert.ok(isNote(note), 'the line about the cut');
    assert.ok(rest.tagName === 'P' && rest.childNodes.length === 1, 'the rest is one text');
    assert.ok(rest.textContent === Array(N - items).fill('- a').join('\n'), 'the rest is the source as written');
    assert.ok(count(root) < 300100, `${count(root)} nodes`);
  }
  {
    // a paragraph: cut between its lines, from the line where it stopped
    const N = 400000;
    const root = render('w\n'.repeat(N));
    await finish(root);
    assert.equal(root.childNodes.length, 3, 'the paragraph, the line, the rest');
    const [p, note, rest] = root.childNodes;
    const done = (p.childNodes.length + 1) / 2;
    assert.ok(p.lastChild instanceof Text && p.lastChild.data === 'w', 'the paragraph ends with its last line, not a break');
    assert.ok(isNote(note), 'the line about the cut');
    assert.ok(rest.textContent === Array(N - done).fill('w').join('\n'), 'the rest of the paragraph as written');
  }
  {
    // the lines of one list item: the line and the rest inside the item
    const root = render('- w\n' + '  w\n'.repeat(400000));
    await finish(root);
    const li = root.firstChild.firstChild;
    const kids = li.childNodes;
    assert.ok(kids.some(isNote), 'the line about the cut is in the item');
    assert.ok(root.childNodes.length === 1, 'nothing after the list');
    assert.ok(count(root) < 300100, `${count(root)} nodes`);
  }

  // ---- the slow and the overflowing inputs finish, each within LIMIT_MS ----
  for (const name of Object.keys(PROBES)) {
    const r = await runProbe(name);
    assert.deepEqual(r.unsafe, [], `${name}: only the allowed elements and attributes`);
    assert.ok(r.nodes <= 2 * r.chars + 10, `${name}: ${r.nodes} nodes for ${r.chars} characters`);
    if (RENDER_MS[name]) assert.ok(r.ms < RENDER_MS[name], `${name}: rendered in ${r.ms.toFixed(0)} ms, more than ${RENDER_MS[name]} ms`);
    if (SLICE_MAX_MS[name]) assert.ok(r.slice < SLICE_MAX_MS[name], `${name}: a slice of ${r.slice.toFixed(0)} ms, more than ${SLICE_MAX_MS[name]} ms`);
    console.log(`${name}: ${r.chars} characters, ${r.nodes} nodes, ${r.ms.toFixed(0)} ms, longest slice ${r.slice.toFixed(0)} ms`);
  }

  console.log('markdown checks: ok');
}
