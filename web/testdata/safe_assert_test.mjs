// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Checks safe_assert.mjs, the preload of every node run of web's tests
// (web/nodetest_test.go starts this one under it): every patched function
// passes and fails exactly where node's own does, keeps node's message for
// small plain data, and fails on two nodes of a stand-in page at once with
// a message of a few lines.

import assert from 'node:assert/strict';
import looseAssert from 'node:assert';
import { equal as namedEqual, deepEqual as namedDeepEqual } from 'node:assert/strict';
import { inspect } from 'node:util';

const MARK = Symbol.for('lectureflow.safeAssert');
const ORIGINAL = Symbol.for('lectureflow.safeAssert.original');
// Nothing below runs unguarded: a failed comparison of stand-in nodes
// without the preload is the failure this file is about.
if (!globalThis[MARK] || !assert.equal[MARK]) {
  console.error('safe_assert.mjs is not preloaded (node --import ./testdata/safe_assert.mjs ...)');
  process.exit(2);
}
const { isSmall } = await import('./safe_assert.mjs');

const fail = (msg) => {
  console.error(`safe_assert_test: ${msg}`);
  process.exit(1);
};
const check = (cond, msg) => cond || fail(msg);

check(namedEqual[MARK] && namedDeepEqual[MARK], 'the named exports of node:assert/strict are not patched');
check(looseAssert.equal[MARK] && looseAssert.strict === assert, 'node:assert is not patched');

// ---- a stand-in page (as in ui320_test.mjs, cut down) ----
class Node {
  constructor() {
    this.parentNode = null;
    this.listeners = {};
  }
}
class Text extends Node {
  constructor(s) {
    super();
    this.data = String(s);
    this.children = [];
  }
}
class El extends Node {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.className = '';
  }
  appendChild(c) {
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
}
function page(rows) {
  const html = new El('html');
  const body = html.appendChild(new El('body'));
  const doc = { documentElement: html, body, activeElement: body };
  html.ownerDocument = doc;
  const out = [];
  for (let i = 0; i < rows; i++) {
    const row = body.appendChild(new El('div'));
    row.className = 'seg';
    row.dataset.fkey = `s${i}`;
    row.listeners.click = [() => {}];
    row.appendChild(new Text(`sentence ${i}`));
    out.push(row);
  }
  return { doc, rows: out };
}

// ---- the same answer as node's own function, case by case ----
const small = page(3);
const twin = page(3);
const lines = (n, seed) => Array.from({ length: n }, (_, i) => `line ${(i * seed) % 101}`).join('\n');
const longA = lines(1200, 7);
const sym = Symbol('s');
const date = new Date(0);
// a shared object is printed again at each place: 2^depth copies
const dag = (depth, leaf = 1) => {
  let x = { v: leaf };
  for (let i = 0; i < depth; i++) x = { l: x, r: x };
  return x;
};
const keyed = (n) => Object.fromEntries(Array.from({ length: n }, (_, i) => [`k${i}`, i]));
const sk = Symbol('k');
const revoked = Proxy.revocable({}, {});
revoked.revoke();
const cyc = { a: 1 };
cyc.self = cyc;
const CASES = [
  [1, 1], [1, 2], [1, '1'], [0, -0], [NaN, NaN], [null, undefined], [null, null], ['a', 'b'], [true, 1],
  [10n, 10n], [sym, sym], [undefined, undefined],
  [{ a: 1 }, { a: 1 }], [{ a: 1 }, { a: '1' }], [{ a: 1 }, { a: 2 }], [[1, [2, 3]], [1, [2, 3]]], [[1, 2], [1, 3]],
  [new Map([[1, { x: 1 }]]), new Map([[1, { x: 1 }]])], [new Set([1, 2]), new Set([2, 3])],
  [date, new Date(0)], [date, new Date(1)], [/a/g, /a/g], [/a/g, /a/i],
  [Object.create(null), {}],
  [small.rows[0], small.rows[0]], [small.rows[0], small.rows[1]], [small.rows[0], twin.rows[0]],
  [small.rows, small.rows.slice()], [[small.rows[0]], [small.rows[1]]], [{ el: small.rows[0] }, { el: small.rows[0] }],
  [small.doc, small.doc], [small.doc, twin.doc],
  [new Error('x'), new Error('x')], [new Uint8Array([1]), new Uint8Array([1])],
  [longA, longA], [longA, lines(1200, 13)], [longA, 1],
  [Array.from({ length: 1500 }, (_, i) => i), Array.from({ length: 1500 }, (_, i) => i)],
  [Array.from({ length: 1500 }, (_, i) => i), Array.from({ length: 1500 }, (_, i) => (i === 700 ? -1 : i))],
  // R30: what the first walk let through
  [dag(6), dag(6)], [dag(6), dag(6, 2)], [dag(12), dag(12)], [dag(12), dag(12, 2)], [dag(16), dag(16, 2)],
  [Array(40).fill(keyed(40)), Array(40).fill(keyed(40))], [Array(40).fill(keyed(40)), Array(40).fill(keyed(39))],
  [{ [sk]: small.rows[0] }, { [sk]: small.rows[0] }], [{ [sk]: small.rows[0] }, { [sk]: small.rows[1] }], [{ [sk]: 1 }, { [sk]: 2 }],
  [Object.assign([1], { el: small.rows[0] }), Object.assign([1], { el: small.rows[1] })],
  [Object.assign(new Map(), { el: small.rows[0] }), Object.assign(new Map(), { el: small.rows[1] })],
  [Object.assign(new Date(0), { el: small.rows[0] }), Object.assign(new Date(0), { el: small.rows[1] })],
  [Object.assign(() => 1, { el: small.rows[0] }), Object.assign(() => 1, { el: small.rows[1] })],
  [new String(lines(3000, 7)), new String(lines(3000, 13))], [new String('a'), new String('a')], [new String('a'), new String('b')],
  [new Proxy({ a: 1 }, {}), { a: 1 }], [new Proxy(small.rows[0], {}), small.rows[1]],
  [revoked.proxy, revoked.proxy], [revoked.proxy, {}],
  [cyc, cyc], [cyc, { a: 1, self: cyc }], [cyc, { a: 2, self: cyc }],
];
const NAMES = ['equal', 'notEqual', 'strictEqual', 'notStrictEqual', 'deepEqual', 'notDeepEqual', 'deepStrictEqual', 'notDeepStrictEqual', 'partialDeepStrictEqual'];

// The lines of node's inspect of v as an assertion message shows it
// (lib/internal/assert/assertion_error.js inspectValue).
const NODE_INSPECT = { compact: false, customInspect: false, depth: 1000, maxArrayLength: Infinity, showHidden: false, showProxy: false, sorted: true, getters: true };
const inspectLines = (v) => inspect(v, NODE_INSPECT).split('\n').length;
// What isSmall promises: node's inspect of an operand it lets through has
// at most this many lines, so node's diff of two of them keeps under
// (N + 1) * (2N + 1) * 4 bytes with N = 2 * LINE_BOUND (about 128 MB).
const LINE_BOUND = 2 * 1000 + 2;

const NOPE = new Error('the check failed');
function outcome(fn, self, a, b, msg) {
  try {
    if (msg) fn.call(self, a, b, msg);
    else fn.call(self, a, b);
    return { ok: true };
  } catch (e) {
    return { ok: false, assertion: e instanceof looseAssert.AssertionError, code: e?.code, message: e?.message, error: e, nope: e === NOPE };
  }
}

let compared = 0;
let guarded = 0;
for (const target of [looseAssert, assert]) {
  for (const name of NAMES) {
    const fn = target[name];
    if (typeof fn !== 'function') continue;
    check(fn[MARK], `${name} is not patched`);
    const orig = fn[ORIGINAL];
    for (const [i, [a, b]] of CASES.entries()) {
      const where = `${target === assert ? 'strict' : 'loose'}.${name} case ${i}`;
      const got = outcome(fn, target, a, b);
      // node's own answer, given an Error as the message so that it builds
      // no message (node throws that Error as is): no case here costs
      // node's whole diff unless isSmall lets both operands through
      const want = outcome(orig, target, a, b, NOPE);
      check(got.ok === want.ok, `${where}: passes ${got.ok}, node's own ${want.ok}`);
      if (!got.ok) {
        check(want.nope ? got.assertion : got.error?.constructor === want.error?.constructor, `${where}: throws ${got.error}, node's own ${want.error}`);
        if (!got.assertion) {
          // node's own error from the comparison itself (a revoked Proxy), passed on
        } else if (isSmall(a) && isSmall(b)) {
          const full = outcome(orig, target, a, b);
          check(got.message === full.message, `${where}: small operands, not node's message:\n${got.message}`);
          const n = full.message.split('\n').length;
          check(n <= 2 * LINE_BOUND + 10, `${where}: node's message has ${n} lines`);
        } else {
          guarded++;
          check(got.code === 'ERR_ASSERTION' && got.message.length < 1000, `${where}: a message of ${got.message.length} characters`);
          check(got.error.operator && got.error.actual === a && got.error.expected === b, `${where}: operator, actual and expected kept`);
          check(!Object.keys(got.error).includes('actual'), `${where}: actual is printed with the error`);
        }
      }
      compared++;
    }
  }
}
check(guarded > 100, `only ${guarded} guarded failures among the cases`);

// ---- what isSmall lets through stays within LINE_BOUND lines ----
{
  const chain = (n, wrap) => {
    let x = 1;
    for (let i = 0; i < n; i++) x = wrap(x);
    return x;
  };
  // each at the edge of the budget
  const STRESS = [
    Array.from({ length: 998 }, (_, i) => i),
    Object.fromEntries(Array.from({ length: 998 }, (_, i) => [`k${i}`, {}])),
    chain(998, (x) => ({ a: x })),
    chain(998, (x) => [x]),
    lines(998, 7),
    new Map(Array.from({ length: 333 }, (_, i) => [i, { x: i }])),
    new Set(Array.from({ length: 499 }, (_, i) => [i])),
    Array.from({ length: 333 }, () => ({ a: 'x\ny' })),
    dag(8),
    // (a wrapped string of over about 1,000 characters is held back: its
    // index keys are counted)
    new String(Array(450).fill('a').join('\n')),
    // sparse arrays (R30): each hole counted as a value
    Array.from({ length: 499 }, () => new Array(1)),
    chain(499, (x) => [, x]),
  ];
  for (const [i, x] of STRESS.entries()) {
    check(isSmall(x), `stress case ${i} is not small: it does not test the bound`);
    const n = inspectLines(x);
    check(n <= LINE_BOUND, `stress case ${i}: ${n} lines, over ${LINE_BOUND}`);
  }
  for (const [i, pair] of CASES.entries()) {
    for (const x of pair) if (isSmall(x)) check(inspectLines(x) <= LINE_BOUND, `case ${i}: ${inspectLines(x)} lines`);
  }
  // and what it holds back
  const HELD = [dag(12), Array(40).fill(keyed(40)), { [sk]: small.rows[0] }, Object.assign([1], { el: 1 }), Object.assign(new Map(), { el: 1 }),
    Object.assign(new Date(0), { el: 1 }), () => 1, new String(lines(3000, 7)), new Proxy({}, {}), revoked.proxy, cyc, small.rows[0], small.doc,
    // R30: small by its items alone, but some 3,000 lines of holes
    Array.from({ length: 996 }, () => new Array(3)), chain(996, (x) => [, x]), new Array(1500)];
  for (const [i, x] of HELD.entries()) check(!isSmall(x), `held-back case ${i} counts as small`);
}
// a message that is an Error is node's: thrown as is
{
  const mine = new Error('mine');
  let got = null;
  try {
    assert.equal(small.rows[0], small.rows[1], mine);
  } catch (e) {
    got = e;
  }
  check(got === mine, 'an Error as the message is not thrown as is');
}
// missing arguments: node's error
{
  let code = null;
  try {
    assert.equal(small.rows[0]);
  } catch (e) {
    code = e.code;
  }
  check(code === 'ERR_MISSING_ARGS', `equal(x) throws ${code}, not node's ERR_MISSING_ARGS`);
}

// ---- a failure on two nodes of a larger page: at once and small ----
{
  const big = page(300);
  const before = process.memoryUsage().arrayBuffers;
  const t0 = Date.now();
  let err = null;
  try {
    assert.equal(big.rows[0], big.rows[1], 'the focus stays on the row');
  } catch (e) {
    err = e;
  }
  const ms = Date.now() - t0;
  const grown = process.memoryUsage().arrayBuffers - before;
  check(err instanceof looseAssert.AssertionError && err.code === 'ERR_ASSERTION', `no AssertionError: ${err}`);
  check(err.message.startsWith('the focus stays on the row\n'), `the message does not start with the test's: ${err.message}`);
  check(err.message.includes('<div class="seg" data-fkey="s0">') && err.message.includes('data-fkey="s1"'), `the nodes are not named: ${err.message}`);
  check(err.message.length < 600, `a message of ${err.message.length} characters`);
  check(inspect(err).length < 3000, `the error prints as ${inspect(err).length} characters`);
  check(ms < 2000, `${ms} ms`);
  check(grown < 16 * 2 ** 20, `${grown} bytes of array buffers`);
  let deep = null;
  try {
    assert.deepEqual(big.rows, big.rows.slice(1));
  } catch (e) {
    deep = e;
  }
  check(deep && deep.message.includes('Array(300)') && deep.message.length < 600, `deepEqual of the rows: ${deep && deep.message}`);
  let str = null;
  try {
    assert.equal(lines(3000, 7), lines(3000, 7).replace('line 14', 'line 15'));
  } catch (e) {
    str = e;
  }
  check(str && str.message.includes('first difference at index') && str.message.length < 800, `long strings: ${str && str.message}`);
}

console.log(`safe_assert checks: ok (${compared} comparisons, ${guarded} guarded failures)`);
