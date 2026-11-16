// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The extension's own requests during detection (security review FB1 M-2,
// L-4; T-SEC-02): the background fallback carries cookies only to the
// detected frame's origin or the profile's declared hosts, and both the
// background and the page read a playlist with a size cap and one timeout
// that covers the body.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { fallbackCredentials, readTextCapped, backgroundFetcher } from '../../src/lib/bgfetch.js';
import { pageFetch } from '../../src/content/scan.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));

const PAGE = 'https://lectures.example.edu/watch/7';
const profile = { match: { hosts: ['*.video.example.com'], mediaHosts: ['media.example.net'] } };

/** A response whose body is the given chunks, then (end false) never ends. */
function streamed(chunks, { status = 200, headers = {}, end = true } = {}) {
  const state = { cancelled: false };
  const enc = new TextEncoder();
  const body = new ReadableStream({
    start(c) {
      for (const ch of chunks) c.enqueue(typeof ch === 'string' ? enc.encode(ch) : ch);
      if (end) c.close();
    },
    cancel() {
      state.cancelled = true;
    },
  });
  return { res: new Response(body, { status, headers }), state };
}

/** A fake fetch that records each call and answers with respond(url, init). */
function recorder(respond = () => new Response('#EXTM3U\n', { status: 200, headers: { 'content-type': 'application/vnd.apple.mpegurl' } })) {
  const calls = [];
  const fn = async (url, init = {}) => {
    calls.push({ url, credentials: init.credentials, redirect: init.redirect, method: init.method || 'GET' });
    return respond(url, init);
  };
  return { fn, calls };
}

test('cookies only on the detected frame’s origin or the profile’s declared hosts', () => {
  const at = (url, o = {}) => fallbackCredentials(url, { frameUrl: PAGE, profile, ...o });
  assert.equal(at('https://lectures.example.edu/media/master.m3u8'), 'include', 'same origin');
  assert.equal(at('https://lectures.example.edu:8443/x.m3u8'), 'omit', 'another port is another origin');
  assert.equal(at('http://lectures.example.edu/x.m3u8'), 'omit', 'another scheme is another origin');
  assert.equal(at('https://lms.example.edu/api/delete?id=1'), 'omit', 'a granted host elsewhere');
  assert.equal(at('https://tenant.video.example.com/v/master.m3u8'), 'include', 'match.hosts');
  assert.equal(at('https://video.example.com/v/master.m3u8'), 'include', 'the wildcard covers the bare host');
  assert.equal(at('https://media.example.net/a.mp4'), 'include', 'match.mediaHosts');
  assert.equal(at('http://media.example.net/a.mp4'), 'omit', 'declared hosts over HTTPS only');
  assert.equal(at('https://evil-media.example.net/a.mp4'), 'omit');
  assert.equal(at('https://cdn.example.org/a.mp4', { profile: null }), 'omit', 'generic detection: the frame’s origin only');
  assert.equal(at('https://cdn.example.org/a.mp4', { profile: { match: { hosts: ['*'] } } }), 'omit', 'never a wildcard over everything');
  assert.equal(at('https://cdn.example.org/a.mp4', { frameUrl: '' }), 'omit', 'no frame named by the browser');
  for (const bad of ['', 'javascript:alert(1)', 'data:text/plain,x', 'file:///c:/x', 'chrome-extension://abc/x']) assert.equal(at(bad), '', bad);
  // Panopto as the page itself: its own hosts carry cookies.
  assert.equal(fallbackCredentials('https://tenant.hosted.panopto.com/Panopto/x.m3u8', { frameUrl: 'https://other.hosted.panopto.com/Panopto/Pages/Viewer.aspx', profile: panopto }), 'include');
});

test('a profile matched through an embedded frame: only that frame’s own references on its own origin carry cookies (SEC15 L1)', () => {
  // A page embeds a Panopto tenant's player (frame 3); the profile matched
  // that frame, not the page, so it is not passed as `profile`.
  const TENANT = 'https://tenant.hosted.panopto.com';
  const embedded = [{ frameId: 3, origin: TENANT }];
  const at = (url, frameIds) => fallbackCredentials(url, { frameUrl: 'https://attacker.example.org/p', profile: null, embedded, frameIds });
  assert.equal(at(`${TENANT}/Panopto/Pages/Auth/Logout.aspx`, [0]), 'omit', 'the page itself points at the tenant');
  assert.equal(at(`${TENANT}/Panopto/Podcast/x.mp4`, [3]), 'include', 'the player’s own media');
  assert.equal(at(`${TENANT}/Panopto/Podcast/x.mp4`, [0, 3]), 'include', 'the player loaded it too');
  assert.equal(at('https://other.hosted.panopto.com/Panopto/x.mp4', [3]), 'omit', 'another tenant, although a declared host');
  assert.equal(at(`${TENANT}/Panopto/x.mp4`, []), 'omit');
});

test('the fallback: same origin and declared hosts with cookies (redirects not followed), any other granted host without', async () => {
  const { fn, calls } = recorder();
  const f = backgroundFetcher({ fetch: fn, permitted: async () => true, frameUrl: PAGE, profile });
  assert.equal((await f.text('https://lectures.example.edu/media/master.m3u8')).ok, true);
  assert.equal((await f.text('https://lms.example.edu/api/delete?id=1')).ok, true);
  await f.probe('https://media.example.net/a.mp4');
  await f.probe('https://lms.example.edu/a.mp4');
  assert.deepEqual(calls.map((c) => [c.method, c.credentials, c.redirect]), [
    ['GET', 'include', 'manual'],
    ['GET', 'omit', 'follow'],
    ['HEAD', 'include', 'manual'],
    ['HEAD', 'omit', 'follow'],
  ]);
  // the embedded-frame rule through the fetcher: frameIds come with each call
  const e = recorder();
  const g = backgroundFetcher({ fetch: e.fn, permitted: async () => true, frameUrl: 'https://attacker.example.org/p', embedded: [{ frameId: 3, origin: 'https://tenant.hosted.panopto.com' }] });
  await g.probe('https://tenant.hosted.panopto.com/Panopto/Pages/Auth/Logout.aspx', { frameIds: [0] });
  await g.probe('https://tenant.hosted.panopto.com/Panopto/Podcast/x.mp4', { frameIds: [3] });
  assert.deepEqual(e.calls.map((c) => c.credentials), ['omit', 'include']);
});

test('the fallback: a redirect with cookies is asked again without them; nothing for an origin not granted', async () => {
  const { fn, calls } = recorder((url, init) =>
    init.redirect === 'manual' ? Object.assign(new Response(null, { status: 302, headers: { location: 'https://lms.example.edu/x' } })) : new Response('#EXTM3U\n'),
  );
  const f = backgroundFetcher({ fetch: fn, permitted: async () => true, frameUrl: PAGE, profile: null });
  const r = await f.text('https://lectures.example.edu/go');
  assert.equal(r.ok, true);
  assert.deepEqual(calls.map((c) => [c.credentials, c.redirect]), [['include', 'manual'], ['omit', 'follow']]);

  const none = recorder();
  const g = backgroundFetcher({ fetch: none.fn, permitted: async () => false, frameUrl: PAGE, profile: null });
  assert.equal(await g.text('https://cdn.example.org/a.m3u8'), null);
  assert.equal(await g.probe('https://cdn.example.org/a.mp4'), null);
  assert.equal(await g.text('javascript:alert(1)'), null);
  assert.equal(none.calls.length, 0);
});

test('the fallback: a playlist past the cap or one that never ends is abandoned', async () => {
  // the server keeps sending: no Content-Length, no end in sight
  const big = streamed(['#EXTM3U\n', 'x'.repeat(600), 'y'.repeat(600)], { end: false });
  const f =backgroundFetcher({ fetch: async () => big.res, permitted: async () => true, frameUrl: PAGE, profile: null, maxBytes: 1000 });
  assert.deepEqual(await f.text('https://lectures.example.edu/big.m3u8'), { ok: false, status: 200, contentType: '', text: '', error: 'too large' });
  assert.equal(big.state.cancelled, true, 'the body is cancelled');

  const declared = recorder(() => new Response('x', { status: 200, headers: { 'content-length': '5000' } }));
  const g = backgroundFetcher({ fetch: declared.fn, permitted: async () => true, frameUrl: PAGE, profile: null, maxBytes: 1000 });
  assert.equal((await g.text('https://lectures.example.edu/big.m3u8')).error, 'too large');

  const slow = streamed(['#EXTM3U\n'], { end: false });
  const h = backgroundFetcher({ fetch: async () => slow.res, permitted: async () => true, frameUrl: PAGE, profile: null, timeoutMs: 50 });
  const t0 = Date.now();
  const r = await h.text('https://lectures.example.edu/endless.m3u8');
  assert.equal(r.ok, false);
  assert.equal(r.error, 'AbortError');
  assert.ok(Date.now() - t0 < 5000);
  assert.equal(slow.state.cancelled, true);

  const fine = streamed(['#EXT', 'M3U\n', '中文\n']);
  assert.equal(await readTextCapped(fine.res, 1000), '#EXTM3U\n中文\n');
});

test('in the page: the same cap, and the timeout covers the body', async () => {
  const saved = Object.getOwnPropertyDescriptor(globalThis, 'location');
  const savedFetch = globalThis.fetch;
  Object.defineProperty(globalThis, 'location', { value: { href: PAGE, origin: 'https://lectures.example.edu' }, configurable: true, writable: true });
  try {
    const big = streamed(['#EXTM3U\n', 'x'.repeat(600), 'y'.repeat(600)], { end: false });
    globalThis.fetch = async () => big.res;
    let [r] = await pageFetch([{ url: 'https://lectures.example.edu/big.m3u8', mode: 'text', maxBytes: 1000 }]);
    assert.deepEqual(r, { ok: false, status: 200, contentType: '', text: '', error: 'too large' });
    assert.equal(big.state.cancelled, true);

    const slow = streamed(['#EXTM3U\n'], { end: false });
    globalThis.fetch = async () => slow.res;
    const t0 = Date.now();
    [r] = await pageFetch([{ url: 'https://lectures.example.edu/endless.m3u8', mode: 'text', maxBytes: 1000 }], 50);
    assert.equal(r.ok, false);
    assert.equal(r.error, 'AbortError');
    assert.ok(Date.now() - t0 < 5000);
    assert.equal(slow.state.cancelled, true);

    globalThis.fetch = async () => new Response('#EXTM3U\n', { status: 200, headers: { 'content-type': 'application/vnd.apple.mpegurl' } });
    [r] = await pageFetch([{ url: 'https://lectures.example.edu/ok.m3u8', mode: 'text', maxBytes: 1000 }]);
    assert.deepEqual(r, { ok: true, status: 200, contentType: 'application/vnd.apple.mpegurl', text: '#EXTM3U\n' });

    // a probe: HEAD without a size, then the first byte; the range body is never read
    const range = streamed(['x'], { status: 206, headers: { 'content-range': 'bytes 0-0/12345' }, end: false });
    globalThis.fetch = async (url, init) => (init.method === 'HEAD' ? new Response(null, { status: 200 }) : range.res);
    [r] = await pageFetch([{ url: 'https://lectures.example.edu/a.mp4', mode: 'probe' }]);
    assert.deepEqual(r, { ok: true, status: 206, contentType: '', size: 12345 });

    // the address the scan saw (SEC33 信-1): another one, and nothing is fetched at all
    const asked = [];
    globalThis.fetch = async (url) => {
      asked.push(url);
      return new Response('#EXTM3U\n', { status: 200 });
    };
    const reqs = [{ url: 'https://lectures.example.edu/ok.m3u8', mode: 'text', maxBytes: 1000 }, { url: 'https://lectures.example.edu/a.mp4', mode: 'probe' }];
    const movedAll = reqs.map(() => ({ ok: false, status: 0, contentType: '', text: '', size: -1, error: 'moved' }));
    assert.deepEqual(await pageFetch(reqs, null, { url: 'https://lectures.example.edu/watch/other' }), movedAll);
    assert.deepEqual(asked, []);
    // the same address (a fragment aside), and no timeout given: fetched as usual
    [r] = await pageFetch([reqs[0]], null, { url: `${PAGE}#t=60` });
    assert.deepEqual([r.ok, r.text, asked.length], [true, '#EXTM3U\n', 1]);
    // the scan's mark of its document (SEC36 低-9): another document has none
    // or another; the same document whose address changed (replaceState) is
    // still the same
    assert.deepEqual(await pageFetch(reqs, null, { token: 'tok-1', url: PAGE }), movedAll, 'no mark here: another document');
    globalThis.__lectureflowScan = 'tok-2';
    try {
      assert.deepEqual(await pageFetch(reqs, null, { token: 'tok-1', url: PAGE }), movedAll);
      globalThis.location.href = `${PAGE}?t=120`;
      [r] = await pageFetch([reqs[0]], null, { token: 'tok-2', url: PAGE });
      assert.deepEqual([r.ok, asked.length], [true, 2]);
    } finally {
      delete globalThis.__lectureflowScan;
    }
    assert.equal(asked.length, 2);
  } finally {
    globalThis.fetch = savedFetch;
    if (saved) Object.defineProperty(globalThis, 'location', saved);
    else delete globalThis.location;
  }
});
