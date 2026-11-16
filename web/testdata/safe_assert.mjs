// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Preloaded into every node run of web's JavaScript tests
// (node --import ./testdata/safe_assert.mjs ..., see README.md and
// nodetest_test.go): keeps a failed comparison of node:assert small.
//
// Node's message for a failed equal()/deepEqual() of two objects inspects
// both whole (depth 1000, every array item) and diffs the two texts line
// by line with a Myers diff that keeps one Int32Array of the size of both
// texts per step, so the memory grows with the square of the lines. Two
// nodes of a stand-in DOM are the whole page each (parents, children,
// listeners): on 2026-11-11 one failed assert.equal(nodeA, nodeB) took
// some 236 GB and the machine with it.
//
// So every comparison of node:assert and node:assert/strict (equal,
// strictEqual, deepEqual, ... and their not- forms) is wrapped here:
//   - operands that are small plain data (primitives, plain objects and
//     arrays, Map, Set, Date, RegExp, at most BUDGET values and string lines
//     each, no accessor, no DOM-like key) go to node's function as before,
//     with its full message and diff;
//   - anything else is still compared by node's own function, given an
//     Error as its message: node throws that Error as is when the check
//     fails, before it builds any message. The wrapper then throws a short
//     AssertionError of its own (tag, class, data-*, a few keys; at most a
//     couple of hundred characters per operand).
// Whether an assertion passes or fails is node's answer in both cases;
// only the message of a failure on large operands is shorter.
//
// This rests on node throwing an Error given as the message as is, before
// it builds a message (lib/assert.js innerFail, Node 24). Should a later
// node build the message first, the wrapper would still answer right but
// silently stop protecting: safe_assert_test.mjs then fails (its guarded
// cases get node's long messages), so run
// go test -run TestNodeSafeAssert ./web/ after a change of node version.
// The memory cap of the Go side (nodetest_test.go) is the second line.
//
// The functions are patched on the objects the tests import
// (import assert from 'node:assert/strict' or 'node:assert'), and the
// named exports of the built-in modules are synced, so
// import { equal } from 'node:assert/strict' gets the wrapper too.

import assert, { AssertionError } from 'node:assert';
import { syncBuiltinESMExports } from 'node:module';
import { types } from 'node:util';

// The values and string lines one operand may have to count as small: the
// diff of two texts of about 2,000 lines is some 100 MB, of 1,000 a quarter
// of it.
const BUDGET = 1000;
// One operand in a short message.
const BRIEF = 200;
// Keys that make an object a DOM node or document (real or stand-in).
const DOM_KEYS = ['parentNode', 'childNodes', 'nodeType', 'ownerDocument', 'documentElement', 'tagName', 'listeners'];
const LEAF_PROTOS = new Set([Date.prototype, RegExp.prototype, Number.prototype, Boolean.prototype]);

const MARK = Symbol.for('lectureflow.safeAssert');
const ORIGINAL = Symbol.for('lectureflow.safeAssert.original');

// The lines of s beyond the first, counted up to cap + 1.
function newlines(s, cap) {
  let n = 0;
  for (let i = s.indexOf('\n'); i !== -1 && n <= cap; i = s.indexOf('\n', i + 1)) n++;
  return n;
}

// Whether v is small plain data, so that node's inspect of it (depth 1000,
// every item, compact: false) has at most 2 * BUDGET + 2 lines: every value
// is counted where it occurs (node prints a shared object again at each
// place; only a cycle back to an ancestor is [Circular], and a cycle here
// runs out of the budget), each counted value takes at most two lines (an
// object's opening and closing brace; a key or a Map key is on its value's
// line), every hole of an array counts as a value (a run of holes is one
// line) and every newline in a string one more. The walk stops at the
// budget, never calls a getter or a Proxy trap and goes into no object
// other than plain objects, arrays, Map and Set: anything else (a class
// instance such as a stand-in node, a function, an Error, a typed array, a
// Proxy, an array, Map, Set or Date with own properties of its own) is not
// small. An exception during the walk (a revoked Proxy, a throwing trap)
// counts as not small too: node's own function still decides.
export function isSmall(v) {
  try {
    return walk(v);
  } catch {
    return false;
  }
}

// The enumerable own data properties of x (string and symbol keys) except
// the keys skip accepts; null when one of them is an accessor.
function ownValues(x, skip) {
  const out = [];
  const keys = Reflect.ownKeys(x);
  if (keys.length > BUDGET + 2) return null;
  for (const k of keys) {
    if (skip && skip(k)) continue;
    if (typeof k === 'symbol' && (k.description || '').includes('\n')) return null; // a key over lines
    const d = Reflect.getOwnPropertyDescriptor(x, k);
    if (!d || !d.enumerable) continue; // inspect shows enumerable properties only
    if (!('value' in d)) return null; // an accessor
    out.push(d.value);
  }
  return out;
}

const isIndex = (k) => typeof k === 'string' && /^(0|[1-9][0-9]*)$/.test(k);

function walk(v) {
  let left = BUDGET;
  const stack = [];
  // every value is counted when it is reached, so the stack stays within
  // the budget too
  const add = (x) => {
    if (--left < 0) return false;
    if (typeof x === 'string' || typeof x === 'symbol') {
      left -= newlines(typeof x === 'string' ? x : x.description || '', left);
      if (left < 0) return false;
    } else if (x !== null && (typeof x === 'object' || typeof x === 'function')) stack.push(x);
    return true;
  };
  const addAll = (xs) => {
    if (xs === null || xs.length > left) return false;
    for (const x of xs) if (!add(x)) return false;
    return true;
  };
  if (!add(v)) return false;
  while (stack.length > 0) {
    const x = stack.pop();
    if (typeof x === 'function' || types.isProxy(x)) return false;
    const proto = Object.getPrototypeOf(x);
    if (proto === Array.prototype) {
      if (x.length > left) return false;
      // the items and no other property
      for (const k of Reflect.ownKeys(x)) if (k !== 'length' && !isIndex(k)) return false;
      const items = ownValues(x, (k) => k === 'length');
      if (items === null) return false;
      // the holes count too, one value each (R30): node prints a run of
      // them as "<n empty items>", a line of its own, so a list of short
      // sparse arrays ([ <3 empty items> ] is three lines) would else pass
      // with up to three lines per counted value
      left -= x.length - items.length;
      if (left < 0 || !addAll(items)) return false;
    } else if (proto === Object.prototype || proto === null) {
      for (const k of DOM_KEYS) if (Object.hasOwn(x, k)) return false;
      if (!addAll(ownValues(x))) return false;
    } else if (proto === Map.prototype || proto === Set.prototype) {
      if (ownValues(x)?.length !== 0 || x.size > left) return false;
      for (const e of x) if (proto === Map.prototype ? !add(e[0]) || !add(e[1]) : !add(e)) return false;
    } else if (proto === String.prototype) {
      // [String: '...']: its text, no other property
      if (ownValues(x, isIndex)?.length !== 0 || !add(String.prototype.valueOf.call(x))) return false;
    } else if (LEAF_PROTOS.has(proto)) {
      if (ownValues(x)?.length !== 0) return false;
    } else {
      return false;
    }
  }
  return true;
}

function clip(s, n = BRIEF) {
  s = String(s);
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

function quote(s, n = 60) {
  return JSON.stringify(s.length > n ? `${s.slice(0, n)}…` : s);
}

function ctorName(x) {
  try {
    const p = Object.getPrototypeOf(x);
    if (p === null) return '[Object: null prototype]';
    const c = Object.getOwnPropertyDescriptor(p, 'constructor');
    return (c && typeof c.value === 'function' && c.value.name) || 'Object';
  } catch {
    return 'Object';
  }
}

// A value's own data property, without calling a getter.
function own(x, k) {
  try {
    const d = Object.getOwnPropertyDescriptor(x, k);
    return d && 'value' in d ? d.value : undefined;
  } catch {
    return undefined;
  }
}

// One operand in a line: a DOM node as its start tag, other objects as
// their class and first entries (one level), strings quoted and cut.
export function brief(v, nested = false) {
  if (typeof v === 'string') return nested ? quote(v, 30) : clip(`${quote(v, BRIEF - 40)} (${v.length} chars)`);
  if (v === null || (typeof v !== 'object' && typeof v !== 'function')) return clip(typeof v === 'bigint' ? `${v}n` : String(v), nested ? 40 : BRIEF);
  if (typeof v === 'function') return clip(`[Function: ${v.name || '(anonymous)'}]`, nested ? 40 : BRIEF);
  if (types.isProxy(v)) return 'Proxy'; // no trap is called (a revoked one throws)
  try {
    if (Array.isArray(v)) {
      const head = `${ctorName(v)}(${v.length})`;
      if (nested || v.length === 0) return head;
      const items = [];
      for (let i = 0; i < v.length && i < 4; i++) items.push(brief(own(v, String(i)), true));
      return clip(`${head} [${items.join(', ')}${v.length > 4 ? ', …' : ''}]`);
    }
    if ('documentElement' in v) return 'document';
    const tag = own(v, 'tagName');
    if (typeof tag === 'string') {
      let s = `<${tag.toLowerCase()}`;
      const id = own(v, 'id');
      if (typeof id === 'string' && id) s += ` id="${clip(id, 40)}"`;
      const cls = own(v, 'className');
      if (typeof cls === 'string' && cls) s += ` class="${clip(cls, 60)}"`;
      const ds = own(v, 'dataset');
      if (ds && typeof ds === 'object') {
        let n = 0;
        for (const k in ds) {
          if (++n > 4) {
            s += ' …';
            break;
          }
          s += ` data-${k.replace(/[A-Z]/g, (c) => `-${c.toLowerCase()}`)}="${clip(String(own(ds, k)), 40)}"`;
        }
      }
      s += '>';
      if ('parentNode' in v && own(v, 'parentNode') === null) s += ' (detached)';
      return clip(s, nested ? 100 : BRIEF);
    }
    if ('nodeType' in v || 'parentNode' in v || ctorName(v) === 'Text') {
      const data = own(v, 'data');
      if (typeof data === 'string') return `#text ${quote(data, 40)}`;
    }
    // the first own keys (with their values, one level) of anything else
    const keys = [];
    for (const k in v) {
      if (keys.length === 6) {
        keys.push('…');
        break;
      }
      keys.push(nested ? k : `${k}: ${brief(own(v, k), true)}`);
    }
    const size = v instanceof Map || v instanceof Set ? `(${v.size})` : '';
    return clip(`${ctorName(v)}${size} {${keys.join(', ')}}`, nested ? 80 : BRIEF);
  } catch {
    return 'object';
  }
}

const HEADS = {
  equal: 'Expected values to be loosely equal',
  notEqual: 'Expected values to be loosely unequal',
  strictEqual: 'Expected values to be strictly equal',
  notStrictEqual: 'Expected values to be strictly unequal',
  deepEqual: 'Expected values to be loosely deep-equal',
  notDeepEqual: 'Expected values not to be loosely deep-equal',
  deepStrictEqual: 'Expected values to be strictly deep-equal',
  notDeepStrictEqual: 'Expected values not to be strictly deep-equal',
  partialDeepStrictEqual: 'Expected values to be partially and strictly deep-equal',
};

// Where two long strings part, for the message.
function stringDiff(a, b) {
  let i = 0;
  const n = Math.min(a.length, b.length);
  while (i < n && a.charCodeAt(i) === b.charCodeAt(i)) i++;
  const line = newlines(a.slice(0, i), Infinity) + 1;
  const at = Math.max(0, i - 20);
  return `\n  first difference at index ${i} (line ${line}): ${quote(a.slice(at, i + 40), 70)} vs ${quote(b.slice(at, i + 40), 70)}`;
}

// The operator node reports for a function: strict.equal is strictEqual.
function operatorOf(name, strict) {
  if (!strict) return name;
  return { equal: 'strictEqual', notEqual: 'notStrictEqual', deepEqual: 'deepStrictEqual', notDeepEqual: 'notDeepStrictEqual' }[name] || name;
}

const SENTINEL = new Error('safe_assert: the check failed');

// message instanceof Error, as node asks it; a message that cannot answer
// (a revoked Proxy) is node's to handle.
function isError(message) {
  try {
    return message instanceof Error;
  } catch {
    return true;
  }
}

function wrap(orig, name, strict) {
  const operator = operatorOf(name, strict);
  const wrapper = {
    [name](actual, expected, message) {
      // Node's own answers: missing arguments, a message that is an Error
      // (thrown as is) and small operands keep node's whole message.
      if (arguments.length < 2 || isError(message) || (isSmall(actual) && isSmall(expected))) {
        return Reflect.apply(orig, this, arguments);
      }
      try {
        Reflect.apply(orig, this, [actual, expected, SENTINEL]);
        return undefined;
      } catch (e) {
        if (e !== SENTINEL) throw e;
      }
      const head = message == null ? `${HEADS[operator] || operator}:` : String(message);
      let text = `${head}\n  actual:   ${brief(actual)}\n  expected: ${brief(expected)}`;
      if (typeof actual === 'string' && typeof expected === 'string') text += stringDiff(actual, expected);
      text += '\n  (safe_assert: large or non-plain operands, compared by node; its full diff is skipped)';
      const err = new AssertionError({ message: text, stackStartFn: wrapper });
      // Kept for a test that reads them, but not printed with the error.
      Object.defineProperty(err, 'actual', { value: actual, writable: true, configurable: true, enumerable: false });
      Object.defineProperty(err, 'expected', { value: expected, writable: true, configurable: true, enumerable: false });
      err.operator = operator;
      err.generatedMessage = message == null;
      throw err;
    },
  }[name];
  wrapper[ORIGINAL] = orig;
  wrapper[MARK] = true;
  return wrapper;
}

const NAMES = Object.keys(HEADS);
for (const [target, strict] of [[assert, false], [assert.strict, true]]) {
  for (const name of NAMES) {
    const orig = target[name];
    if (typeof orig !== 'function' || orig[MARK]) continue;
    target[name] = wrap(orig, name, strict);
  }
}
syncBuiltinESMExports();
globalThis[MARK] = true;
