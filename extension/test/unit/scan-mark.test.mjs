// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The scan's mark of its document (src/content/scan.js scanFrame, SEC36
// 低-9 / SEC36b 信-17): scanFrame runs here against a stand-in with the few
// properties it reads, and its mark against the real pageFetch check.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { scanFrame, pageFetch } from '../../src/content/scan.js';

const PAGE = 'https://player.example.edu/embed/42?start=0';
const doc = { documentElement: {}, querySelectorAll: () => [], title: 't' };
globalThis.window = { document: doc, location: { href: PAGE }, performance: { getEntriesByType: () => [] } };
globalThis.document = doc;
Object.defineProperty(globalThis, 'location', { value: { href: PAGE, origin: 'https://player.example.edu', ancestorOrigins: [] }, configurable: true, writable: true });

let asked = 0;
globalThis.fetch = async () => {
  asked++;
  return new Response('#EXTM3U\n', { status: 200 });
};
const fetchWith = async (expected) => {
  asked = 0;
  const [r] = await pageFetch([{ url: 'https://player.example.edu/m.m3u8', mode: 'text', maxBytes: 1000 }], null, expected);
  return `${r.error || 'fetched'}/${asked}`;
};

test('scanFrame marks its document: a random string, the same on every scan of that document', async () => {
  delete globalThis.__lectureflowScan;
  const a = scanFrame([]).token;
  assert.equal(typeof a, 'string');
  assert.match(a, /^[0-9a-z]+(-[0-9a-z]+){3}$/);
  assert.equal(scanFrame([]).token, a, 'a second scan (a profile’s selector reads) gives the same mark');
  // the mark lets the page fetch through, also after replaceState
  assert.equal(await fetchWith({ token: a }), 'fetched/1');
  globalThis.location.href = 'https://player.example.edu/embed/42?start=120';
  assert.equal(await fetchWith({ token: a }), 'fetched/1');
  // another document has none (a new isolated world), or its own once scanned
  delete globalThis.__lectureflowScan;
  assert.equal(await fetchWith({ token: a }), 'moved/0');
  const b = scanFrame([]).token;
  assert.notEqual(b, a);
  assert.equal(await fetchWith({ token: a }), 'moved/0');
  assert.equal(await fetchWith({ token: b }), 'fetched/1');
});

test('scanFrame: a value the page put under the name (an element, a frame) is never taken for a mark', async () => {
  const mark = scanFrame([]).token;
  globalThis.__lectureflowScan = { nodeType: 1, toString: () => mark, valueOf: () => mark };
  assert.equal(await fetchWith({ token: mark }), 'moved/0');
  const fresh = scanFrame([]).token;
  assert.equal(typeof fresh, 'string');
  assert.equal(typeof globalThis.__lectureflowScan, 'string');
  assert.equal(await fetchWith({ token: fresh }), 'fetched/1');
  delete globalThis.__lectureflowScan;
});
