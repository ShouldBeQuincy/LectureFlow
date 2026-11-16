// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { iconPng, renderRgba } = require('../../src/main/lib/pngicon');
const { buildMenuTemplate, shortcutFor, nextZoomLevel } = require('../../src/main/lib/menu');
const { createLogger } = require('../../src/main/logger');

const DESKTOP = path.join(__dirname, '..', '..');

test('icon: valid PNG of the requested size, transparent corners, blue body, white bars', () => {
  const png = iconPng(32);
  assert.deepEqual([...png.subarray(0, 8)], [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  assert.equal(png.toString('ascii', 12, 16), 'IHDR');
  assert.equal(png.readUInt32BE(16), 32);
  assert.equal(png.readUInt32BE(20), 32);
  const px = renderRgba(64);
  const at = (x, y) => [...px.subarray((y * 64 + x) * 4, (y * 64 + x) * 4 + 4)];
  assert.equal(at(0, 0)[3], 0, 'corner transparent');
  assert.deepEqual(at(6, 32), [0x1f, 0x6f, 0xeb, 255], 'accent blue');
  assert.deepEqual(at(Math.round(0.43 * 64), 32), [255, 255, 255, 255], 'bar white');
  assert.throws(() => iconPng(4), RangeError);
});

test('menu: none on Windows; macOS keeps an application menu', () => {
  assert.equal(buildMenuTemplate({ platform: 'win32', isDev: true }), null);
  assert.equal(buildMenuTemplate({ platform: 'linux', isDev: false }), null);
  const mac = buildMenuTemplate({ platform: 'darwin', isDev: false, onOpenSettings: () => {} });
  assert.deepEqual(mac.map((m) => m.id), ['app', 'edit', 'view', 'window']);
  const appMenu = mac[0].submenu;
  assert.equal(appMenu[0].role, 'about');
  assert.ok(appMenu.some((i) => i.id === 'settings' && i.accelerator === 'Cmd+,'));
  assert.equal(appMenu[appMenu.length - 1].role, 'quit');
  assert.doesNotMatch(JSON.stringify(mac), /toggleDevTools/);
  assert.match(JSON.stringify(buildMenuTemplate({ platform: 'darwin', isDev: true })), /toggleDevTools/);
});

test('shortcuts: zoom everywhere on Windows, reload and DevTools only in development', () => {
  const key = (k, mods = {}, code = '') => ({ type: 'keyDown', key: k, code, control: false, shift: false, alt: false, meta: false, ...mods });
  const win = { platform: 'win32', isDev: false };
  const dev = { platform: 'win32', isDev: true };
  assert.equal(shortcutFor(key('=', { control: true }), win), 'zoomIn');
  assert.equal(shortcutFor(key('+', { control: true, shift: true }), win), 'zoomIn');
  assert.equal(shortcutFor(key('+', { control: true }, 'NumpadAdd'), win), 'zoomIn');
  assert.equal(shortcutFor(key('-', { control: true }), win), 'zoomOut');
  assert.equal(shortcutFor(key('0', { control: true }), win), 'zoomReset');
  assert.equal(shortcutFor(key('=', {}), win), null, 'no Ctrl');
  assert.equal(shortcutFor(key('=', { control: true, alt: true }), win), null, 'AltGr combinations are typing');
  assert.equal(shortcutFor({ ...key('=', { control: true }), type: 'keyUp' }, win), null);
  assert.equal(shortcutFor(key('r', { control: true }), win), null);
  assert.equal(shortcutFor(key('I', { control: true, shift: true }), win), null);
  assert.equal(shortcutFor(key('r', { control: true }), dev), 'reload');
  assert.equal(shortcutFor(key('I', { control: true, shift: true }), dev), 'devTools');
  assert.equal(shortcutFor(key('=', { control: true }), { platform: 'darwin', isDev: true }), null, 'macOS: the menu handles it');
  assert.equal(nextZoomLevel(0, 'zoomIn'), 0.5);
  assert.equal(nextZoomLevel(4, 'zoomIn'), 4);
  assert.equal(nextZoomLevel(-3, 'zoomOut'), -3);
  assert.equal(nextZoomLevel(2.5, 'zoomReset'), 0);
  assert.equal(nextZoomLevel(undefined, 'zoomOut'), -0.5);
});

test('logger never writes the token', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-log-'));
  try {
    const file = path.join(dir, 'logs', 'desktop.log');
    const log = createLogger({ file });
    const token = 'f'.repeat(64);
    log.addSecret(token);
    log.warn(`service: something ${token} happened`);
    log.info('plain line');
    const text = fs.readFileSync(file, 'utf8');
    assert.doesNotMatch(text, new RegExp(token));
    assert.match(text, /something \[token\] happened/);
    assert.match(text, /INFO plain line/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('every source file carries the SPDX header (v3-brief §0)', () => {
  const files = [];
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      if (e.name === 'node_modules' || e.name.startsWith('.') || e.name === 'test-results' || e.name === 'dist' || e.name === 'dist-next') continue;
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (/\.(js|cjs|mjs|html|css)$/.test(e.name)) files.push(p);
    }
  };
  walk(DESKTOP);
  assert.ok(files.length > 15);
  for (const f of files) {
    const head = fs.readFileSync(f, 'utf8').split(/\r?\n/).slice(0, 3).join('\n');
    assert.match(head, /SPDX-License-Identifier: AGPL-3\.0-or-later/, path.relative(DESKTOP, f));
    assert.match(head, /Copyright \(C\) 2026 LectureFlow contributors/, path.relative(DESKTOP, f));
  }
});

test('dependencies are pinned to exact versions', () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8'));
  const deps = { ...pkg.dependencies, ...pkg.devDependencies };
  assert.deepEqual(deps, {
    electron: '44.9.3',
    'electron-builder': '26.17.0',
    '@playwright/test': '1.64.1',
  });
  assert.ok(fs.existsSync(path.join(DESKTOP, 'package-lock.json')));
});

// A runtime dependency goes into app.asar and THIRD_PARTY_NOTICES.md: whoever
// adds one updates the notices (go run ./scripts/licaudit), the allowlist
// if its license needs it, and these two assertions.
const RUNTIME_DEP_HINT = 'a new runtime npm dependency ships in app.asar: regenerate THIRD_PARTY_NOTICES.md (go run ./scripts/licaudit), check packaging/licenses/allowlist.json, then update this test';

test('no runtime npm dependency at all: app.asar ships no node_modules (any new one must update the notices and this test)', () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8'));
  assert.equal(pkg.dependencies, undefined, `package.json has "dependencies"; ${RUNTIME_DEP_HINT}`);
  const lock = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package-lock.json'), 'utf8'));
  assert.equal(lock.packages[''].dependencies, undefined, `package-lock.json: the root has "dependencies"; ${RUNTIME_DEP_HINT}`);
});

test('electron-updater is no dependency: the packaged app never loads it (in-app updates go through internal/selfupdate)', () => {
  const lock = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package-lock.json'), 'utf8'));
  const left = Object.keys(lock.packages).filter((k) => /(^|\/)node_modules\/electron-updater$/.test(k));
  assert.deepEqual(left, [], 'package-lock.json has no electron-updater');
});

test('splash: the text colours are the web app\'s (web/style.css), light and dark', () => {
  const css = fs.readFileSync(path.join(DESKTOP, '..', 'web', 'style.css'), 'utf8');
  const splash = fs.readFileSync(path.join(DESKTOP, 'src', 'splash.html'), 'utf8');
  const darkAt = splash.indexOf('prefers-color-scheme: dark');
  assert.ok(darkAt > 0, 'the splash has a dark scheme');
  for (const n of [1, 2, 3]) {
    const web = new RegExp(`--text-${n}:\\s*light-dark\\((#[0-9a-f]{6}),\\s*(#[0-9a-f]{6})\\)`, 'i').exec(css);
    assert.ok(web, `web/style.css --text-${n}`);
    const found = [...splash.matchAll(new RegExp(`--text-${n}:\\s*(#[0-9a-f]{6})`, 'gi'))].map((m) => ({ at: m.index, value: m[1].toLowerCase() }));
    assert.equal(found.length, 2, `splash --text-${n}: light and dark`);
    assert.ok(found[0].at < darkAt && found[1].at > darkAt, `splash --text-${n}: light first, then dark`);
    assert.deepEqual(found.map((f) => f.value), [web[1].toLowerCase(), web[2].toLowerCase()], `--text-${n}`);
  }
});
