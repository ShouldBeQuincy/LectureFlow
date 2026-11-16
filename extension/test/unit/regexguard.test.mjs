// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The runtime guard of profile regexes (SEC18d): a match too slow, or too
// much time in all, stops every regex of that profile in this context and
// is recorded; the records stop the profile later (local at once, bundled
// after several trips), name the rule and the time, never the page; and no
// code runs a profile regex around the guard. Slow matches are simulated
// with an injected clock; one test runs a really slow pattern once, on a
// short input (about 2 s), to show the second call returns at once.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { guardedRegex, setRegexGuard, resetRegexGuard, resetRegexBudget, regexTrip, SLOW_MS, BUDGET_MS, PAGE_WINDOW_MS } from '../../src/lib/regexguard.js';
import { regexComplexity } from '../../src/lib/regexcost.js';
import { HEALTH_KEY, BUNDLED_TRIPS, BUNDLED_SEVERE_MS, MAX_RECORDS, healthKey, isStopped, tripEntry, nextRecord, withRecord, cleanTrip, recordable, fromExtensionPage, recordRemoved } from '../../src/lib/health.js';
import { applyProfile, profileRegexes, MAX_TESTS_PER_RULE, downloadPolicyFor, profileStopped } from '../../src/lib/profile.js';
import { profileKey } from '../../src/lib/observe.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));
const realNow = () => performance.now();

/** A clock whose every match takes `cost` ms: (now, set(cost), calls()). */
function fakeClock() {
  let t = 0;
  let cost = 0;
  let odd = false;
  let calls = 0;
  return {
    now: () => {
      calls++;
      odd = !odd;
      if (!odd) t += cost;
      return t;
    },
    set: (c) => {
      cost = c;
    },
    calls: () => calls,
  };
}

function fresh(clock) {
  resetRegexGuard();
  const trips = [];
  setRegexGuard({ now: clock ? clock.now : realNow, onTrip: (t) => trips.push(t) });
  return trips;
}

test('one match slower than SLOW_MS stops every regex of that profile here, once reported', () => {
  const clock = fakeClock();
  const trips = fresh(clock);
  const a = guardedRegex('^/a/', { owner: 'p:1' });
  const b = guardedRegex('^/b/', { owner: 'p:1' });
  const other = guardedRegex('^/a/', { owner: 'q:1' });
  clock.set(1);
  assert.equal(a.test('/a/x'), true);
  assert.deepEqual(trips, []);
  clock.set(SLOW_MS + 50);
  assert.equal(a.test('/a/x'), true, 'the slow match still answers');
  assert.deepEqual(trips, [{ owner: 'p:1', source: '^/a/', ms: SLOW_MS + 50, reason: 'slow' }]);
  // afterwards: no match, and nothing is run or timed
  const before = clock.calls();
  assert.equal(a.test('/a/x'), false);
  assert.equal(b.test('/b/x'), false, 'every regex of the profile');
  assert.equal(a.exec('/a/x'), null);
  assert.equal(clock.calls(), before);
  assert.equal(trips.length, 1);
  // another profile with the same regex is not (SEC18e 低-12: a trip is the profile's, not the source's)
  assert.equal(other.test('/a/x'), true);
  assert.ok(regexTrip('p:1', '^/b/'));
  // a new budget does not lift a trip
  resetRegexBudget();
  assert.equal(b.test('/b/x'), false);
  resetRegexGuard();
  assert.equal(b.test('/b/x'), true);
});

test('matches of one profile adding up to more than BUDGET_MS trip it; the budget starts again each epoch', () => {
  const clock = fakeClock();
  const trips = fresh(clock);
  const re = guardedRegex('x', { owner: 'p:1' });
  const step = 100;
  clock.set(step);
  const allowed = Math.floor(BUDGET_MS / step);
  for (let i = 0; i < allowed; i++) assert.equal(re.test('x'), true, `call ${i}`);
  resetRegexBudget();
  for (let i = 0; i < allowed; i++) assert.equal(re.test('x'), true, `epoch 2, call ${i}`);
  assert.deepEqual(trips, []);
  assert.equal(re.test('x'), true);
  assert.equal(trips.length, 1);
  assert.equal(trips[0].reason, 'budget');
  assert.equal(trips[0].ms, BUDGET_MS + step);
  assert.equal(re.test('x'), false);
});

test('inputs over MAX_REGEX_INPUT are never run; a profile regex without owner stops by its source', () => {
  const clock = fakeClock();
  const trips = fresh(clock);
  const re = guardedRegex('a');
  assert.equal(re.test('a'.repeat(1025)), false);
  assert.equal(clock.calls(), 0);
  clock.set(SLOW_MS + 1);
  re.test('a');
  assert.equal(trips[0].owner, '');
  assert.equal(guardedRegex('a').test('a'), false);
  assert.equal(guardedRegex('b').test('b'), true);
});

test('applyProfile: a slow rule stops the profile for the rest of the call; at most MAX_TESTS_PER_RULE addresses per rule', () => {
  const clock = fakeClock();
  const trips = fresh(clock);
  const GUID = 'a0000000-0000-4000-8000-0000000000a1';
  const track = (i) => ({ id: `t${i}`, url: `https://cdn.example.org/x/${i}/sessions/${GUID}/${GUID}.hls/master.m3u8`, kind: 'video', format: 'hls', sizeBytes: -1, sizeEstimated: false, sources: ['request'], frameIds: [0], protected: false });
  clock.set(0);
  // audio HLS tracks: only the primary-audio rule tests them
  const many = Array.from({ length: MAX_TESTS_PER_RULE + 10 }, (_, i) => ({ ...track(i), kind: 'audio' }));
  assert.deepEqual(panopto.tracks.filter((r) => r.detect.urlPattern !== undefined && r.detect.kind === 'audio' && r.detect.format === 'hls').length, 1);
  applyProfile(panopto, many, 'en');
  assert.equal(clock.calls(), MAX_TESTS_PER_RULE * 2, 'MAX_TESTS_PER_RULE matches, two clock reads each');
  clock.set(SLOW_MS + 1);
  const named = applyProfile(panopto, [track(1), track(2)], 'en');
  assert.equal(trips.length, 1);
  assert.equal(trips[0].owner, profileKey(panopto));
  assert.ok(named.tracks.length <= 1, 'only the match that tripped it names a track');
  const calls = clock.calls();
  applyProfile(panopto, [track(3)], 'en');
  assert.equal(clock.calls(), calls, 'stopped: no regex of the profile runs');
});

test('a really slow pattern (SEC18d) runs once, then returns at once', { timeout: 60000 }, () => {
  fresh(null);
  const br = (look) => `(?:${['x', '[xb]', '[xc]', '[xd]', '[xe]', '[xf]', '[xg]', '[xh]'].map((c) => c + look).join('|')})`;
  const G = br('(?=.{300})');
  const re = guardedRegex(`${G}.*${G}.?.?z`, { owner: 'evil:1' });
  const input = 'x'.repeat(330); // about 1-3 s once; 1024 characters take far longer
  const t0 = performance.now();
  assert.equal(re.test(input), false);
  const first = performance.now() - t0;
  assert.ok(first > SLOW_MS, `the first match took ${first} ms`);
  assert.ok(regexTrip('evil:1', ''));
  const t1 = performance.now();
  assert.equal(re.test(input), false);
  assert.ok(performance.now() - t1 < 5, 'the second call returns at once');
});

// ------------------------------------------------------------ records

const local = { profile: { ...panopto, id: 'demo', version: '0.1.0' }, reviewed: false, source: 'local' };
const bundled = { profile: panopto, reviewed: true, source: 'bundled' };
const RULE = '/tracks/1/detect/urlPattern';
const SRC = panopto.tracks[1].detect.urlPattern;

test('records: a local profile stops at its first trip; a bundled one after several, or one very slow match', () => {
  assert.equal(healthKey(local), 'local:demo');
  assert.equal(healthKey(bundled), `bundled:panopto@${panopto.version}`);
  const base = { rule: RULE, reason: 'slow', extVersion: '0.2.1', today: '2026-11-11' };
  const l = nextRecord(undefined, { ...base, entry: local, ms: 300 });
  assert.deepEqual(Object.keys(l).sort(), ['at', 'disabled', 'extVersion', 'id', 'ms', 'reason', 'rule', 'source', 'trips', 'version'], 'no address, no page, no input');
  assert.equal(l.disabled, true);
  assert.equal(isStopped(l, local, '0.2.1'), true);
  assert.equal(isStopped(l, local, '0.3.0'), true, 'until imported again, whatever the extension version');
  let b;
  for (let i = 1; i <= BUNDLED_TRIPS; i++) {
    b = nextRecord(b, { ...base, entry: bundled, ms: 300 });
    assert.equal(b.trips, i);
    assert.equal(b.disabled, i >= BUNDLED_TRIPS, `trip ${i}`);
  }
  assert.equal(isStopped(b, bundled, '0.2.1'), true);
  assert.equal(isStopped(b, bundled, '0.3.0'), false, 'an extension update tries it again');
  assert.equal(isStopped(b, { ...bundled, profile: { ...panopto, version: '9.9.9' } }, '0.2.1'), false, 'so does a new profile version');
  assert.equal(nextRecord(undefined, { ...base, entry: bundled, ms: BUNDLED_SEVERE_MS }).disabled, true, 'one very slow match');
  // a budget's total (the background's minute: 3.1 s) is one trip, not one very slow match (SEC22b S-1)
  const w = nextRecord(undefined, { ...base, reason: 'budget', entry: bundled, ms: 3100 });
  assert.equal(w.disabled, false);
  assert.equal(w.trips, 1);
  assert.equal(nextRecord(nextRecord(w, { ...base, reason: 'budget', entry: bundled, ms: 3100 }), { ...base, reason: 'budget', entry: bundled, ms: 3100 }).disabled, true, 'the third trip stops it');
  // trips of another extension version do not add up
  assert.equal(nextRecord({ ...b, extVersion: '0.2.0' }, { ...base, entry: bundled, ms: 300 }).trips, 1);
});

test('records: the trip is tied to the profile and rule that hold the regex; reports are checked; at most MAX_RECORDS kept', () => {
  const entries = [local, bundled];
  assert.deepEqual(tripEntry(entries, profileKey(panopto), SRC), { entry: bundled, rule: RULE });
  assert.equal(tripEntry(entries, profileKey(panopto), '^nothing-of-ours$'), null, 'a regex no profile has');
  assert.equal(tripEntry(entries, 'other:1', SRC), null, 'an owner no profile has');
  assert.equal(tripEntry(entries, '', SRC).entry, local, 'without owner: the first profile that has it');
  assert.ok(profileRegexes(panopto).some(([p]) => p === RULE));
  assert.equal(cleanTrip({ owner: 'p:1', source: 'x', ms: 300, reason: 'slow' }).ms, 300);
  for (const bad of [null, {}, { owner: 1, source: 'x', ms: 1, reason: 'slow' }, { owner: 'p', source: '', ms: 1, reason: 'slow' }, { owner: 'p', source: 'x', ms: -1, reason: 'slow' }, { owner: 'p', source: 'x', ms: Infinity, reason: 'slow' }, { owner: 'p', source: 'x', ms: 1, reason: 'other' }, { owner: 'p', source: 'x'.repeat(301), ms: 1, reason: 'slow' }]) {
    assert.equal(cleanTrip(bad), null, JSON.stringify(bad));
  }
  let h = {};
  for (let i = 0; i < MAX_RECORDS + 5; i++) h = withRecord(h, `local:p${i}`, { i });
  assert.equal(Object.keys(h).length, MAX_RECORDS);
  assert.ok(!('local:p0' in h) && 'local:p68' in h);
  assert.equal(HEALTH_KEY, 'profileHealth');
});

// ------------------------------------------------------------ no way around

const SRC_DIR = path.join(root, 'src');
const files = (dir) => fs.readdirSync(dir, { withFileTypes: true }).flatMap((d) => (d.isDirectory() ? (d.name === 'vendor' ? [] : files(path.join(dir, d.name))) : d.name.endsWith('.js') ? [path.join(dir, d.name)] : []));

// Every mention of RegExp in the extension's sources (a constructor call
// with or without "new", an alias, globalThis.RegExp), every
// Reflect.construct, and every .match / .matchAll / .search whose argument
// is not a regex literal (a string there becomes a RegExp): each must be
// one of these exact lines, with why it is not a profile regex run around
// the guard (SEC18e 低-13). [file, the whole line, trimmed]
const ALLOWED = [
  ['lib/regexguard.js', 'const re = new RegExp(src, flags);'], // the guard itself
  ['lib/jsonschema.js', "if (!regexCache.has(p)) regexCache.set(p, new RegExp(p, 'u'));"], // the schema's own patterns
  ['lib/redact.js', 'const IDENTITY_FIELD = new RegExp('], // fixed patterns of the redactor
  ['lib/redact.js', 'const SEGMENT_RE = new RegExp('],
  ['lib/redact.js', "if (new RegExp(UUID_RE).test(m)) return '';"],
  ['lib/redact.js', "if (new RegExp(`^${UUID_RE}$`).test(v)) cls = 'uuid';"],
  ['lib/dash.js', 're = new RegExp(source);'], // DASH segment templates, not a profile; built linear (SEC18 低-9)
  ['lib/profile.js', "new RegExp(src, 'i'); // compiles only"], // checkProfile: compiles, never runs
  // classic scripts: their own copy of the guard; the compiled regex runs only in guardedTest (below)
  ['content/observe.js', "compiled = c.specs.map((s) => ({ re: new RegExp(s.urlPattern, 'i'), source: String(s.urlPattern), hosts: s.hosts, names: Object.keys(s.pointers) })); // run only through guardedTest"],
  ['content/observe-main.js', "specs.push({ re: new RegExp(String(s.urlPattern), 'i'), hosts: (s.hosts || []).map(String), pointers }); // run only through guardedTest"],
];
// In the classic scripts, the only lines that run a compiled spec.
const CLASSIC_RUNS = [
  ['content/observe.js', 'const ok = spec.re.test(s);'],
  ['content/observe-main.js', 'const ok = config.specs[index].re.test(s);'],
  // the warm-up on '', inside the timing of a spec's first match (SEC22 L-A, SEC22b M-1)
  ['content/observe.js', "spec.re.test('');"],
  ['content/observe-main.js', "spec.re.test('');"],
];
const COMMENT = /^\s*(\/\/|\/\*|\*)/;

test('no profile regex runs around the guard: every RegExp, Reflect.construct and string-pattern match in src is accounted for', () => {
  const found = [];
  const runs = [];
  for (const f of files(SRC_DIR)) {
    const rel = path.relative(SRC_DIR, f).split(path.sep).join('/');
    fs.readFileSync(f, 'utf8').split('\n').forEach((raw) => {
      const line = raw.trim();
      if (COMMENT.test(raw)) return;
      if (/\bRegExp\b|\bReflect\.construct\b|\.(?:match|matchAll|search)\(\s*[^/\s)]/.test(line)) found.push([rel, line]);
      if (/\.re\.(?:test|exec)\(|\bre\.(?:test|exec)\(/.test(line) && rel.startsWith('content/')) runs.push([rel, line]);
    });
  }
  const unaccounted = found.filter(([rel, line]) => !ALLOWED.some(([f, l]) => f === rel && l === line));
  assert.deepEqual(unaccounted, [], 'run a profile regex through profileRegex / guardedRegex (src/lib/regexguard.js)');
  const unguarded = runs.filter(([rel, line]) => !CLASSIC_RUNS.some(([f, l]) => f === rel && l === line));
  assert.deepEqual(unguarded, [], 'in a classic content script, run a compiled spec only in guardedTest');
  // the scan sees what it is meant to see
  const sees = (line) => /\bRegExp\b|\bReflect\.construct\b|\.(?:match|matchAll|search)\(\s*[^/\s)]/.test(line);
  for (const bad of ['const R = RegExp;', 'RegExp(src).test(x)', 'globalThis.RegExp(src)', 'Reflect.construct(C, [src])', 's.match(src)', 's.search(p.pattern)', 's.matchAll(String(src))']) assert.ok(sees(bad), bad);
  for (const fine of ['s.match(/a+/)', 's.search(/x/i)', 'const regexes = []']) assert.ok(!sees(fine), fine);
});

// ------------------------------------------------------------ SEC22

test('a source runs once on "" before its first match here, inside that match\'s timing (SEC22 L-A, SEC22b M-1)', () => {
  const order = [];
  let t = 0;
  let cost = 0;
  let odd = false;
  const trips = fresh({ now: () => (order.push('now'), (odd = !odd) ? t : (t += cost)) });
  // M-1: "^" + "(?:^|$)" x 26 + "\b\B" passed the static check and is slow
  // on its first runs (9.2 s); the static check refuses it now (SEC22c R-1),
  // the guard stays for a first match that is slow all the same; stand-in: a
  // clock that charges the first timed match 300 ms
  const SRC = `^${'(?:^|$)'.repeat(26)}\\b\\B`;
  assert.match(regexComplexity(SRC), /alternatives that take no character/, 'refused statically');
  const original = RegExp.prototype.test;
  RegExp.prototype.test = function (x) {
    if (this.source === 'w+y' || this.source === 'z+y') order.push(`test:${x}`);
    return original.call(this, x);
  };
  try {
    const a = guardedRegex('w+y', { owner: 'w:1' });
    const b = guardedRegex('w+y', { owner: 'w:1' });
    assert.deepEqual(order, [], 'nothing runs on creation');
    a.test('wy');
    assert.deepEqual(order, ['now', 'test:', 'test:wy', 'now'], 'the run on "" is timed with the first match');
    order.length = 0;
    b.test('wwy');
    a.test('wy');
    assert.deepEqual(order, ['now', 'test:wwy', 'now', 'now', 'test:wy', 'now'], 'once per source in this context, whatever the RegExp');
    // a regex whose first timed match (the run on "" with it) is slow trips the guard
    cost = SLOW_MS + 50;
    order.length = 0;
    const c = guardedRegex('z+y', { owner: 'm:1' });
    assert.equal(c.test('zy'), true);
    assert.deepEqual(order, ['now', 'test:', 'test:zy', 'now']);
    assert.deepEqual(trips.map((x) => [x.owner, x.reason]), [['m:1', 'slow']]);
    order.length = 0;
    assert.equal(guardedRegex('z+y', { owner: 'm:1' }).test('zy'), false, 'stopped: a new RegExp runs nothing, not even the warm-up');
    assert.deepEqual(order, []);
  } finally {
    RegExp.prototype.test = original;
  }
  // the classic content scripts: the same, inside guardedTest's timing, once per spec
  for (const rel of ['content/observe.js', 'content/observe-main.js']) {
    const src = fs.readFileSync(path.join(SRC_DIR, rel), 'utf8');
    const body = src.slice(src.indexOf('const guardedTest = '), src.indexOf('const ms = ', src.indexOf('const guardedTest = ')));
    assert.ok(/const t0 = [^;]+;[\s\S]*if \(!spec\.warm\) \{\s*spec\.warm = true;\s*spec\.re\.test\(''\);\s*\}[\s\S]*\.re\.test\(s\);/.test(body), rel);
  }
});

// ------------------------------------------------------------ SEC18e

test('a content script\'s budget starts again every PAGE_WINDOW_MS; the background\'s only per detection', () => {
  const clock = fakeClock();
  const trips = fresh(clock);
  setRegexGuard({ windowMs: PAGE_WINDOW_MS });
  const re = guardedRegex('x', { owner: 'p:1' });
  clock.set(200);
  // five matches of 200 ms fill the budget; after a window, the same again
  for (let i = 0; i < 5; i++) re.test('x');
  assert.deepEqual(trips, []);
  clock.set(PAGE_WINDOW_MS);
  re.test('x'); // a long pause (one "match" spanning the window): slow, trips
  assert.equal(trips.length, 1);
  // a page open for long: many cheap matches spread over windows never trip
  const trips2 = fresh(clock);
  setRegexGuard({ windowMs: PAGE_WINDOW_MS });
  const cheap = guardedRegex('y', { owner: 'q:1' });
  let t = 0;
  const steps = [];
  setRegexGuard({ now: () => steps.shift() ?? t });
  for (let i = 0; i < 100; i++) {
    // each match 100 ms, one every 10 s: 10 per window = 1 s, never more
    t += 10000;
    steps.push(t, t + 99);
    cheap.test('y');
  }
  assert.deepEqual(trips2, []);
  setRegexGuard({ windowMs: 0 });
});

test('a bundled profile is stopped for good only by the background\'s own measurements, never by content scripts (SEC18e 中-5)', () => {
  assert.equal(recordable(bundled, 'content'), false);
  assert.equal(recordable(bundled, 'background'), true);
  assert.equal(recordable(local, 'content'), true);
  // three frames each reporting a trip of the bundled profile: nothing is recorded
  let rec;
  for (let i = 0; i < 5; i++) {
    if (recordable(bundled, 'content')) rec = nextRecord(rec, { entry: bundled, rule: RULE, ms: 1100, reason: 'budget', extVersion: '0.2.1', today: '2026-11-11' });
  }
  assert.equal(rec, undefined);
  assert.equal(isStopped(rec, bundled, '0.2.1'), false);
});

test('messages from the settings page count as the extension\'s own, in a tab or not; a content script\'s do not (SEC18e 低-11)', () => {
  const base = 'chrome-extension://abcdefghijklmnopabcdefghijklmnop/';
  assert.equal(fromExtensionPage({ url: `${base}options/options.html`, tab: { id: 3 } }, base), true);
  assert.equal(fromExtensionPage({ url: `${base}options/options.html` }, base), true);
  assert.equal(fromExtensionPage({ url: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx', tab: { id: 3 } }, base), false);
  assert.equal(fromExtensionPage({ url: 'https://evil.example.com/chrome-extension://abcdefghijklmnopabcdefghijklmnop/' }, base), false);
  assert.equal(fromExtensionPage({ tab: { id: 3 } }, base), false);
  assert.equal(fromExtensionPage({ url: `${base}x` }, ''), false);
  // the background also forgets its trips when a record is removed
  assert.equal(recordRemoved({ 'local:a': {}, 'bundled:b@1': {} }, { 'bundled:b@1': {} }), true);
  assert.equal(recordRemoved({ 'local:a': {} }, { 'local:a': {}, 'local:b': {} }), false);
  assert.equal(recordRemoved({ 'local:a': {} }, undefined), true);
  assert.equal(recordRemoved(undefined, { 'local:a': {} }), false);
});

test('the download switch of a stopped profile is "unknown", never "enabled" (SEC18e 低-15)', () => {
  const clock = fakeClock();
  fresh(clock);
  const p = { ...panopto, id: 'switch', version: '1.0.0', downloadPolicy: { disabledWhen: [{ selector: '.x', pattern: 'restricted' }] } };
  const match = { profile: p, url: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1' };
  const results = { 'downloadPolicy.disabledWhen.0': { count: 1, value: 'restricted by the instructor' } };
  clock.set(0);
  assert.equal(downloadPolicyFor(match, results), 'disabled');
  assert.equal(downloadPolicyFor(match, { 'downloadPolicy.disabledWhen.0': { count: 1, value: 'open' } }), 'enabled');
  assert.equal(profileStopped(p), false);
  clock.set(SLOW_MS + 1);
  assert.equal(downloadPolicyFor(match, results), 'unknown', 'the match that tripped');
  clock.set(0);
  assert.equal(downloadPolicyFor(match, results), 'unknown', 'stopped: no "no match" read as "not restricted"');
  assert.equal(profileStopped(p), true);
});

test('a selector condition alone does not say "not restricted" when the page\'s observer stopped before seeing the response another condition needs (SEC22 I-4)', () => {
  fresh(fakeClock());
  const p = { ...panopto, id: 'mixed', version: '1.0.0', downloadPolicy: { disabledWhen: [{ selector: '.x' }, { observed: 'downloadUrl', empty: true }] } };
  const match = { profile: p, url: 'https://tenant.cloud.panopto.eu/Panopto/Pages/Viewer.aspx?id=1' };
  const none = { 'downloadPolicy.disabledWhen.0': { count: 0 } };
  const unseen = (stopped) => ({ seen: new Set(), values: {}, urls: {}, stopped });
  assert.equal(downloadPolicyFor(match, none, unseen(false)), 'enabled', 'as before: the response simply was not seen');
  assert.equal(downloadPolicyFor(match, none, unseen(true)), 'unknown');
  assert.equal(downloadPolicyFor(match, { 'downloadPolicy.disabledWhen.0': { count: 1 } }, unseen(true)), 'disabled', 'a condition that holds still says so');
});
