// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Packaging (SPEC-v3 V7, V9, V10): fuses, signing from the environment,
// pinned downloads, the LGPL check of ffmpeg, the size report.
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const { signingFrom, dropEmptySigning } = require('../../scripts/lib/signing');
const { fetchPinned, sha256File, globFiles, cacheName } = require('../../scripts/lib/fetch');
const { buildSizeReport, renderMarkdown, groupTopLevel, walk, BUDGET_BYTES } = require('../../scripts/lib/sizereport');
const {
  checkConfigure, checkLicenseText, gplFree, gplComponents, targetKey, WORKER_FILES,
  checkAria2cVersion, aria2cSourceText, ffmpegSourceText, bundledFileProblems, GO_PROGRAMS,
  stageAgent, bundleAgent, BUNDLE_AGENT_ENV, BUNDLE_AGENT_DEFAULT, agentBuildArgs, AGENT_TARGETS, cachedBuildProblems, reuseCachedBuild,
  machoSlices, machoVersion, compareVersions, macMinimumCheck,
} = require('../../scripts/stage');
const sources = require('../../scripts/sources');

const DESKTOP = path.join(__dirname, '..', '..');
const REPO = path.join(DESKTOP, '..');

function loadConfig(env) {
  const saved = { ...process.env };
  for (const k of ['CSC_LINK', 'CSC_NAME', 'WIN_CSC_LINK', 'APPLE_API_KEY', 'APPLE_API_KEY_ID', 'APPLE_API_ISSUER', 'APPLE_ID', 'APPLE_APP_SPECIFIC_PASSWORD', 'APPLE_TEAM_ID', 'LECTUREFLOW_UPDATE_REPO', 'LECTUREFLOW_PUBLISHER_NAME', 'GH_TOKEN', 'GITHUB_TOKEN']) delete process.env[k];
  Object.assign(process.env, env);
  const p = require.resolve('../../electron-builder.config.js');
  delete require.cache[p];
  try {
    return require(p);
  } finally {
    for (const k of Object.keys(process.env)) if (!(k in saved)) delete process.env[k];
    Object.assign(process.env, saved);
    delete require.cache[p];
  }
}

test('electron-builder config: fuses, targets, per-user NSIS, no update feed by default', () => {
  const c = loadConfig({});
  assert.equal(c.appId, 'org.lectureflow.desktop');
  assert.deepEqual(c.electronFuses, {
    runAsNode: false,
    enableNodeOptionsEnvironmentVariable: false,
    enableNodeCliInspectArguments: false,
    enableCookieEncryption: true,
    enableEmbeddedAsarIntegrityValidation: true,
    onlyLoadAppFromAsar: true,
    loadBrowserProcessSpecificV8Snapshot: false,
    grantFileProtocolExtraPrivileges: true,
  });
  assert.equal(c.asar, true);
  assert.deepEqual(c.win.target.map((t) => t.target), ['nsis', 'portable']);
  assert.equal(c.nsis.oneClick, false);
  assert.equal(c.nsis.perMachine, false);
  assert.equal(c.nsis.allowToChangeInstallationDirectory, true);
  assert.equal(c.nsis.deleteAppDataOnUninstall, false);
  assert.deepEqual(c.mac.target.map((t) => [t.target, t.arch]), [['dmg', ['arm64']], ['zip', ['arm64']]]);
  assert.equal(c.publish, null, 'no LECTUREFLOW_UPDATE_REPO: explicitly no publish configuration');
  assert.equal(c.win.verifyUpdateCodeSignature, true);
  assert.equal(c.win.signtoolOptions, undefined);
  // Signed builds leave the pinned upstream aria2c.exe unsigned; no positive
  // entry, which would take precedence over the exclusion.
  assert.deepEqual(c.win.signExts, ['!aria2c.exe']);
  assert.deepEqual(loadConfig({ WIN_CSC_LINK: 'cert.pfx' }).win.signExts, ['!aria2c.exe'], 'the same with a certificate');
  assert.deepEqual(c.extraResources, [{ from: 'build/resources', to: '.', filter: ['**/*'] }]);
  assert.ok(!c.files.some((f) => /test|scripts|\.cache|build\//.test(f)), 'tests and build scripts stay out of app.asar');
  assert.ok(fs.existsSync(path.join(DESKTOP, c.mac.entitlements)));
});

test('signing comes from the environment only; none = unsigned, no error', () => {
  assert.deepEqual(signingFrom({}), { macCert: false, macNotarize: false, winCert: false });
  assert.deepEqual(signingFrom({ CSC_LINK: '  ' }), { macCert: false, macNotarize: false, winCert: false }, 'empty secrets count as absent');
  assert.deepEqual(signingFrom({ WIN_CSC_LINK: 'x.pfx' }), { macCert: false, macNotarize: false, winCert: true });
  assert.equal(signingFrom({ CSC_LINK: 'x', APPLE_API_KEY: 'k', APPLE_API_KEY_ID: 'i' }).macNotarize, false, 'incomplete notarization credentials');
  assert.equal(signingFrom({ CSC_LINK: 'x', APPLE_API_KEY: 'k', APPLE_API_KEY_ID: 'i', APPLE_API_ISSUER: 's' }).macNotarize, true);
  assert.equal(signingFrom({ APPLE_ID: 'a', APPLE_APP_SPECIFIC_PASSWORD: 'p', APPLE_TEAM_ID: 't' }).macNotarize, false, 'no notarization without a certificate');
  // unset secrets arrive as empty strings: removed before electron-builder runs
  assert.deepEqual(dropEmptySigning({ CSC_LINK: '', CSC_KEY_PASSWORD: ' ', APPLE_API_KEY_ID: 'i', PATH: '' }), { APPLE_API_KEY_ID: 'i', PATH: '' });
  const unsigned = loadConfig({});
  assert.equal(unsigned.mac.identity, '-', 'ad-hoc signature: Apple silicon only runs signed code');
  assert.equal(unsigned.mac.notarize, false);
  const signed = loadConfig({ CSC_LINK: 'cert.p12', APPLE_ID: 'a', APPLE_APP_SPECIFIC_PASSWORD: 'p', APPLE_TEAM_ID: 't' });
  assert.equal(signed.mac.identity, undefined);
  assert.equal(signed.mac.notarize, true);
});

test('the main process never starts the crash reporter (no dumps that could hold the token)', () => {
  const src = path.join(DESKTOP, 'src');
  const files = [];
  const rec = (d) => fs.readdirSync(d, { withFileTypes: true }).forEach((e) => (e.isDirectory() ? rec(path.join(d, e.name)) : files.push(path.join(d, e.name))));
  rec(src);
  for (const f of files) assert.doesNotMatch(fs.readFileSync(f, 'utf8'), /crashReporter\s*\.\s*start|crashReporter\s*\.\s*setUploadToServer/, f);
});

test('third-party manifest: pinned HTTPS URLs and hashes, LGPL ffmpeg, binaries.json in step', () => {
  const m = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
  const pins = [m.ffmpeg['windows-x64'], m.ffmpeg['darwin-arm64'], m.uv['windows-x64'], m.uv['darwin-arm64'], ...m.uv.license_texts];
  for (const p of pins) {
    assert.match(p.url, /^https:\/\//, p.url);
    assert.match(p.sha256, /^[0-9a-f]{64}$/, p.url);
  }
  assert.deepEqual(checkConfigure(m.ffmpeg['darwin-arm64'].configure.join(' '), m.ffmpeg.forbidden_configure), []);
  assert.ok(m.ffmpeg['darwin-arm64'].configure.includes('--disable-gpl'));
  const bins = JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries;
  const text = JSON.stringify(bins);
  for (const p of pins.slice(0, 4)) assert.ok(text.includes(p.sha256), `binaries.json lacks ${p.sha256}`);
  assert.ok(bins.every((b) => !/to be pinned/.test(b.version)));
  assert.ok(text.includes(m.ffmpeg.version) && text.includes(m.uv.version));
});

test('ffmpeg: both builds from the pinned FFmpeg tarball, corresponding source and linked libraries declared, no GPL', () => {
  const m = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
  const bins = JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries;
  const win = bins.find((b) => b.name === 'ffmpeg / ffprobe (Windows build)');
  const mac = bins.find((b) => b.name === 'ffmpeg / ffprobe (macOS build)');
  const w = m.ffmpeg['windows-x64'];
  for (const [entry, pin] of [[win, w], [mac, m.ffmpeg['darwin-arm64']]]) {
    assert.deepEqual(entry.source_code, pin.source_code.map((s) => s.url), 'both manifests name the same source');
    for (const u of entry.source_code) assert.match(u, /^https:\/\//);
    assert.ok(Array.isArray(entry.bundled));
    assert.equal(entry.sha256, pin.sha256);
    assert.equal(entry.license, 'LGPL-2.1-or-later');
    assert.ok(entry.source_code.includes(pin.url));
  }
  // Windows: built from source, every input pinned by URL and SHA-256, nothing from BtbN.
  assert.equal(w.url, m.ffmpeg['darwin-arm64'].url, 'same FFmpeg source as macOS');
  assert.ok(!JSON.stringify(w).includes('BtbN') && !JSON.stringify(win).includes('BtbN'));
  assert.match(w.docker_image, /@sha256:[0-9a-f]{64}$/);
  for (const d of w.deps) {
    assert.match(d.url, /^https:\/\//, d.name);
    assert.match(d.sha256, /^[0-9a-f]{64}$/, d.name);
    assert.match(d.commit, /^[0-9a-f]{40}$/, d.name);
    assert.ok(win.source_code.includes(d.url), `${d.name} source listed`);
    assert.ok(win.bundled.some((l) => l.name.startsWith(d.name) && l.version.startsWith(d.version)), `${d.name} bundled`);
  }
  assert.deepEqual(checkConfigure(w.configure.join(' '), m.ffmpeg.forbidden_configure), []);
  for (const f of ['--disable-gpl', '--disable-nonfree', '--disable-version3', '--disable-autodetect', '--enable-shared']) assert.ok(w.configure.includes(f), f);
  for (const f of w.configure.filter((a) => /^--enable-lib/.test(a))) assert.ok(['--enable-libvpl', '--enable-libopenh264', '--enable-libdav1d'].includes(f), `unexpected external library ${f}`);
  assert.ok(Object.keys(w.toolchain).length >= 2);
  for (const v of Object.values(w.toolchain)) assert.ok(win.bundled.some((l) => l.version.includes(v)), `toolchain ${v} declared in bundled`);
  for (const l of win.bundled) {
    assert.ok(l.name && l.version && l.license && l.url, JSON.stringify(l));
    assert.match(l.url, /^https:\/\//, l.name);
  }
  assert.deepEqual(gplComponents(bins), [], 'no GPL component in the distributed ffmpeg (SPEC-v3 V7)');
  // macOS: system libraries plus dav1d (AV1 software decoder), pinned like the Windows deps.
  const md = m.ffmpeg['darwin-arm64'];
  assert.deepEqual(md.deps.map((d) => d.name), ['dav1d']);
  for (const d of md.deps) {
    assert.deepEqual(d, w.deps.find((x) => x.name === d.name), `${d.name}: same pin as Windows`);
    assert.ok(mac.source_code.includes(d.url), `${d.name} source listed`);
    assert.ok(mac.bundled.some((l) => l.name.startsWith(d.name) && l.version.startsWith(d.version)), `${d.name} bundled`);
  }
  assert.equal(mac.bundled.length, md.deps.length, 'macOS: nothing else bundled');
  for (const f of md.configure.filter((a) => /^--enable-lib/.test(a))) assert.ok(['--enable-libdav1d'].includes(f), `unexpected external library ${f}`);
  for (const t of md.build_tools) assert.match(t.sha256, /^[0-9a-f]{64}$/, t.name);
});

test('GPL check of ffmpeg components and of ffmpeg -L', () => {
  for (const ok of ['LGPL-2.1-or-later', 'MIT', 'BSD-2-Clause', 'Zlib', 'FTL OR GPL-2.0-or-later', 'Apache-2.0 OR GPL-2.0-or-later', 'GPL-3.0-or-later WITH GCC-exception-3.1', 'LGPL-3.0-or-later OR GPL-2.0-or-later', 'ZPL-2.1 AND MIT']) assert.ok(gplFree(ok), ok);
  for (const bad of ['GPL-2.0-or-later', 'GPL-2.0-only', 'LGPL-2.0-or-later AND GPL-2.0-only AND MIT', 'MIT AND GPL-3.0-or-later', 'AGPL-3.0-only', 'GPL-3.0-or-later WITH Bison-exception-2.2']) assert.ok(!gplFree(bad), bad);
  assert.deepEqual(gplComponents([{ name: 'ffmpeg / ffprobe (x)', license: 'LGPL-2.1-or-later', bundled: [{ name: 'FFTW', license: 'GPL-2.0-or-later' }, { name: 'zlib', license: 'Zlib' }] }, { name: 'uv', license: 'GPL-2.0-only' }]),
    ['ffmpeg / ffprobe (x) / FFTW [GPL-2.0-or-later]']);
  const lgpl = 'ffmpeg is free software; you can redistribute it and/or\nmodify it under the terms of the GNU Lesser General Public\nLicense as published by the Free Software Foundation; either\nversion 2.1 of the License, or (at your option) any later version.';
  assert.ok(checkLicenseText(lgpl));
  assert.ok(!checkLicenseText('ffmpeg is free software; you can redistribute it and/or modify it under the terms of the GNU General Public License as published by the Free Software Foundation; either version 2 of the License'));
  assert.ok(!checkLicenseText(lgpl.replace('2.1', '3')), 'version3 builds are not what is declared');
});

test('ffmpeg configure check: GPL and non-free flags fail the build', () => {
  const forbidden = ['--enable-gpl', '--enable-nonfree', '--enable-libx264', '--enable-libx265'];
  assert.deepEqual(checkConfigure('--enable-version3 --enable-shared --disable-libx264', forbidden), []);
  assert.deepEqual(checkConfigure('--prefix=/x --enable-gpl --enable-libx264', forbidden), ['--enable-gpl', '--enable-libx264']);
  assert.deepEqual(checkConfigure('--enable-nonfree', forbidden), ['--enable-nonfree']);
  assert.equal(targetKey('win32'), 'windows-x64');
  assert.equal(targetKey('darwin'), 'darwin-arm64');
  assert.throws(() => targetKey('linux'), /unsupported/);
  for (const f of WORKER_FILES) assert.ok(fs.existsSync(path.join(REPO, 'worker', f)), f);
  assert.ok(WORKER_FILES.every((f) => !/^test_|conftest/.test(f)));
  // Every worker module a packaged worker imports is packaged too (a
  // missing one fails every transcription at "import", e.g. textrepair.py).
  const local = new Set(fs.readdirSync(path.join(REPO, 'worker')).filter((f) => f.endsWith('.py')).map((f) => f.slice(0, -3)));
  for (const f of WORKER_FILES) {
    const src = fs.readFileSync(path.join(REPO, 'worker', f), 'utf8');
    for (const m of src.matchAll(/^\s*(?:import\s+([A-Za-z_]\w*)|from\s+([A-Za-z_]\w*)\s+import\b)/gm)) {
      const mod = m[1] || m[2];
      if (local.has(mod)) assert.ok(WORKER_FILES.includes(`${mod}.py`), `${f} imports ${mod}, which is not in WORKER_FILES`);
    }
  }
  // The Colab backend uploads these from resources/worker (pack-dry 3.2.0:
  // colab_remote.py was only an optional file of the stage).
  const colab = /var workerFiles = \[\]string\{([^}]*)\}/.exec(fs.readFileSync(path.join(REPO, 'internal', 'colab', 'backend.go'), 'utf8'));
  assert.ok(colab, 'internal/colab/backend.go: var workerFiles = []string{…}');
  const uploaded = [...colab[1].matchAll(/"([^"]+)"/g)].map((m) => m[1]);
  assert.ok(uploaded.includes('colab_remote.py'), uploaded.join(', '));
  for (const f of uploaded) assert.ok(WORKER_FILES.includes(f), `the Colab backend uploads ${f}, which is not in WORKER_FILES`);
});

test('pinned downloads: a failed fetch is tried again; an injected fetch never falls back to curl', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-fetch-retry-'));
  try {
    const body = Buffer.from('retried payload');
    const sha = crypto.createHash('sha256').update(body).digest('hex');
    let calls = 0;
    const flaky = async () => {
      calls++;
      if (calls === 1) throw new TypeError('fetch failed', { cause: { code: 'ECONNRESET' } });
      return new Response(body);
    };
    const lines = [];
    const f = await fetchPinned({ url: 'https://example.com/r/tool.zip', sha256: sha }, dir, { fetchImpl: flaky, log: (l) => lines.push(l) });
    assert.equal(sha256File(f), sha);
    assert.equal(calls, 2);
    assert.ok(lines.some((l) => /failed \(fetch failed: ECONNRESET\), try 1 of 3/.test(l)), lines.join('\n'));
    const down = async () => {
      throw new TypeError('fetch failed');
    };
    await assert.rejects(fetchPinned({ url: 'https://example.com/r/gone.zip', sha256: sha }, dir, { fetchImpl: down, log: () => {} }), /fetch failed/);
    assert.deepEqual(fs.readdirSync(dir).filter((n) => n.includes('gone')), [], 'nothing kept from a failed download');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('pinned downloads: verified before use, cached by hash, mismatches rejected', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-fetch-'));
  try {
    const body = Buffer.from('pinned payload');
    const sha = crypto.createHash('sha256').update(body).digest('hex');
    let calls = 0;
    const fakeFetch = async () => {
      calls++;
      return new Response(body);
    };
    const quiet = { fetchImpl: fakeFetch, log: () => {} };
    const f = await fetchPinned({ url: 'https://example.com/a/tool.zip', sha256: sha }, dir, quiet);
    assert.equal(sha256File(f), sha);
    assert.equal(path.basename(f), `${sha.slice(0, 12)}-tool.zip`);
    await fetchPinned({ url: 'https://example.com/a/tool.zip', sha256: sha }, dir, quiet);
    assert.equal(calls, 1, 'second call served from the cache');
    await assert.rejects(fetchPinned({ url: 'https://example.com/b.zip', sha256: 'a'.repeat(64) }, dir, quiet), /SHA-256 mismatch/);
    assert.deepEqual(fs.readdirSync(dir).filter((n) => n.includes('b.zip')), [], 'nothing kept from a bad download');
    await assert.rejects(fetchPinned({ url: 'http://example.com/c.zip', sha256: sha }, dir, quiet), /non-HTTPS/);
    assert.equal(cacheName('https://x/y/ffmpeg%20n9.zip?x=1'), 'ffmpeg_n9.zip');
    fs.mkdirSync(path.join(dir, 'bin'));
    for (const n of ['ffmpeg.exe', 'avcodec-63.dll', 'avutil-61.dll', 'readme.txt']) fs.writeFileSync(path.join(dir, 'bin', n), 'x');
    assert.deepEqual(globFiles(dir, 'bin/*.dll').map((p) => path.basename(p)).sort(), ['avcodec-63.dll', 'avutil-61.dll']);
    assert.deepEqual(globFiles(dir, 'bin/ffmpeg.exe').map((p) => path.basename(p)), ['ffmpeg.exe']);
    assert.deepEqual(globFiles(dir, 'bin/missing.exe'), []);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('cached builds: reused only when BUILDINFO.txt names the pinned version and sources, else deleted', () => {
  const m = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
  const mac = m.ffmpeg['darwin-arm64'];
  const ar = m.aria2c['darwin-arm64'];
  // BUILDINFO.txt as the build scripts write it (first lines that matter).
  const info = {
    'ffmpeg windows-x64': [`FFmpeg ${m.ffmpeg.version} for Windows x64, built by desktop/scripts/build-ffmpeg-windows.sh`, 'compiler: x'],
    'ffmpeg darwin-arm64': [`FFmpeg ${m.ffmpeg.version} for macOS arm64, built by desktop/scripts/build-ffmpeg-macos.sh`, `source: ${mac.url}`, `source sha256: ${mac.sha256}`,
      ...(mac.deps || []).map((d) => `dependency: ${d.name} ${d.version} ${d.url} sha256 ${d.sha256} (static library, meson)`), 'compiler: x'],
    'aria2c darwin-arm64': [`aria2 ${m.aria2c.version} for macOS arm64, built by packaging/aria2c/build-aria2c-macos.sh`, `source: ${ar.url}`, `source sha256: ${ar.sha256}`],
  };
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-cached-build-'));
  const make = (name, lines) => {
    const root = path.join(tmp, name);
    fs.mkdirSync(path.join(root, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(root, 'bin', 'tool'), 'binary');
    if (lines) fs.writeFileSync(path.join(root, 'BUILDINFO.txt'), `${lines.join('\n')}\n`);
    return root;
  };
  const log = console.log;
  console.log = () => {};
  try {
    for (const [id, lines] of Object.entries(info)) {
      const [program, key] = id.split(' ');
      const ok = make(`${program}-${key}-ok`, lines);
      assert.deepEqual(cachedBuildProblems(ok, program, key, m), [], id);
      assert.equal(reuseCachedBuild(ok, path.join('bin', 'tool'), program, key, m), true, `${id}: a matching build is reused`);
      assert.ok(fs.existsSync(ok));
      const crlf = make(`${program}-${key}-crlf`, lines.map((l) => `${l}\r`));
      assert.deepEqual(cachedBuildProblems(crlf, program, key, m), [], `${id}: CRLF`);

      const bad = {
        'no BUILDINFO.txt': null,
        'another version': [lines[0].replace(` ${m[program].version} `, ' 0.0.1 '), ...lines.slice(1)],
        'another platform': [lines[0].replace(/for (Windows x64|macOS arm64)/, 'for Linux x64'), ...lines.slice(1)],
      };
      if (key === 'darwin-arm64') {
        bad['another source'] = lines.map((l) => (l.startsWith('source sha256:') ? `source sha256: ${'0'.repeat(64)}` : l));
        bad['no source line'] = lines.filter((l) => !l.startsWith('source sha256:'));
      }
      if (id === 'ffmpeg darwin-arm64' && (mac.deps || []).length) {
        bad['another dependency'] = lines.map((l) => (l.startsWith('dependency:') ? l.replace(/sha256 [0-9a-f]{64}/, `sha256 ${'1'.repeat(64)}`) : l));
      }
      for (const [what, badLines] of Object.entries(bad)) {
        const root = make(`${program}-${key}-${what.replace(/ /g, '-')}`, badLines);
        assert.notDeepEqual(cachedBuildProblems(root, program, key, m), [], `${id}: ${what} refused`);
        assert.equal(reuseCachedBuild(root, path.join('bin', 'tool'), program, key, m), false, `${id}: ${what} not reused`);
        assert.ok(!fs.existsSync(root), `${id}: ${what}: the cached build is deleted, so it is built again`);
      }
    }
    // nothing cached: built, nothing deleted
    assert.equal(reuseCachedBuild(path.join(tmp, 'none'), path.join('bin', 'tool'), 'ffmpeg', 'windows-x64', m), false);
  } finally {
    console.log = log;
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  const src = fs.readFileSync(path.join(DESKTOP, 'scripts', 'stage.js'), 'utf8');
  assert.equal((src.match(/if \(!reuseCachedBuild\(root, /g) || []).length, 3, 'stage checks the Windows ffmpeg, the macOS ffmpeg and the macOS aria2c caches');
  assert.equal((src.match(/checkFreshBuild\(root, '/g) || []).length, 3, 'and every build it makes instead');
});

test('size report: budget, resources, ten largest files, Markdown', () => {
  const MB = 1024 * 1024;
  const report = buildSizeReport({
    version: '3.0.0',
    platform: 'windows-x64',
    artifacts: [{ name: 'Setup.exe', bytes: 180 * MB }, { name: 'big.exe', bytes: 260 * MB }],
    resourceFiles: [
      { path: 'bin/avcodec-63.dll', bytes: 87 * MB }, { path: 'bin/uv.exe', bytes: 31 * MB },
      { path: 'worker/asr.py', bytes: 100 }, { path: 'LICENSE', bytes: 34000 },
    ],
    asarBytes: 1.2 * MB,
    unpackedFiles: Array.from({ length: 15 }, (_, i) => ({ path: `f${i}`, bytes: i * MB })),
    updates: 'electron-updater feed: off (LECTUREFLOW_UPDATE_REPO is not set: no app-update.yml); in-app updates: on (minisign key 0102030405060708 built in)',
  });
  assert.equal(report.budgetBytes, BUDGET_BYTES);
  assert.equal(report.overBudget, true);
  assert.deepEqual(report.installers.map((a) => a.overBudget), [false, true]);
  assert.deepEqual(report.resources.map((g) => g.name), ['bin/', 'LICENSE', 'worker/']);
  assert.equal(report.bin[0].path, 'bin/avcodec-63.dll');
  assert.equal(report.largest.length, 10);
  assert.equal(report.largest[0].path, 'f14');
  const md = renderMarkdown(report);
  assert.match(md, /\| Setup\.exe \| 180\.0 MB \| yes \|/);
  assert.match(md, /\| big\.exe \| 260\.0 MB \| no \|/);
  assert.match(md, /Over budget\./);
  assert.match(md, /Ten largest files/);
  assert.match(md, /electron-updater feed: off \(LECTUREFLOW_UPDATE_REPO is not set: no app-update\.yml\); in-app updates: on \(minisign key 0102030405060708 built in\)\./);
  assert.deepEqual(groupTopLevel([{ path: 'a/b', bytes: 1 }, { path: 'a/c', bytes: 2 }, { path: 'x', bytes: 5 }]), [
    { name: 'x', bytes: 5, files: 1 }, { name: 'a/', bytes: 3, files: 2 },
  ]);
  assert.ok(walk(path.join(DESKTOP, 'locales')).some((f) => f.path === 'en.json'));
});

test('update feed: none without LECTUREFLOW_UPDATE_REPO, the named repository with it, malformed fails', () => {
  for (const env of [{}, { LECTUREFLOW_UPDATE_REPO: '' }, { GH_TOKEN: 'x' }]) {
    assert.equal(loadConfig(env).publish, null, JSON.stringify(env));
  }
  const c = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'example-owner/example.repo_1' });
  assert.deepEqual(c.publish, [{ provider: 'github', owner: 'example-owner', repo: 'example.repo_1', releaseType: 'draft' }]);
  assert.equal(c.win.signtoolOptions, undefined, 'unsigned: no publisher name');
  const bad = ['lectureflow', 'a/b/c', ' owner/name', 'owner/name ', 'https://github.com/o/r', 'o/r.git', '-o/r', 'o-/r', 'o--x/r', 'o/..', 'o/', '/r', 'ow ner/r', 'o/r\n'];
  for (const v of bad) assert.throws(() => loadConfig({ LECTUREFLOW_UPDATE_REPO: v }), /LECTUREFLOW_UPDATE_REPO must be/, JSON.stringify(v));
});

test('update feed: Windows publisher name only for signed builds', () => {
  const signed = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx', LECTUREFLOW_PUBLISHER_NAME: ' Example Publisher Ltd ' });
  assert.deepEqual(signed.win.signtoolOptions, { publisherName: 'Example Publisher Ltd' });
  assert.equal(signed.win.verifyUpdateCodeSignature, true);
  const derived = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx' });
  assert.equal(derived.win.signtoolOptions, undefined, 'electron-builder takes the common name of the certificate');
  const unsigned = loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', LECTUREFLOW_PUBLISHER_NAME: 'Example Publisher Ltd' });
  assert.equal(unsigned.win.signtoolOptions, undefined, 'ignored without a certificate');
  assert.throws(() => loadConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r', WIN_CSC_LINK: 'cert.pfx', LECTUREFLOW_PUBLISHER_NAME: 'a\nb' }), /LECTUREFLOW_PUBLISHER_NAME/);
});

test('update feed check of a finished build', () => {
  const { checkUpdateFeed, updateBuildConfig } = require('../../scripts/lib/updaterepo');
  assert.equal(updateBuildConfig({}).summary, 'electron-updater feed: off (LECTUREFLOW_UPDATE_REPO is not set: no app-update.yml)');
  assert.equal(updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }).summary, 'electron-updater feed: on (GitHub releases of o/r, app-update.yml)');
  assert.deepEqual(checkUpdateFeed({ repo: null, appUpdateYml: null, distFiles: ['LectureFlow-Setup-3.0.0-x64.exe', 'size-report.md'], platform: 'win32' }), []);
  assert.equal(checkUpdateFeed({ repo: null, appUpdateYml: 'provider: github\n', distFiles: [], platform: 'win32' }).length, 1);
  assert.equal(checkUpdateFeed({ repo: null, appUpdateYml: null, distFiles: ['latest.yml'], platform: 'win32' }).length, 1);
  const yml = 'owner: o\nrepo: r\nprovider: github\nreleaseType: draft\nupdaterCacheDirName: lectureflow-desktop-updater\n';
  const repo = { owner: 'o', repo: 'r' };
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: yml, distFiles: ['latest.yml'], platform: 'win32' }), []);
  assert.equal(checkUpdateFeed({ repo, appUpdateYml: null, distFiles: [], platform: 'win32' }).length, 1);
  assert.match(checkUpdateFeed({ repo: { owner: 'o', repo: 'x' }, appUpdateYml: yml, platform: 'win32' })[0], /expected github:o\/x/);
  assert.match(checkUpdateFeed({ repo, appUpdateYml: yml, platform: 'win32', winCert: true })[0], /publisherName/);
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: `${yml}publisherName:\n  - Example Publisher Ltd\n`, platform: 'win32', winCert: true }), []);
  assert.deepEqual(checkUpdateFeed({ repo, appUpdateYml: yml, platform: 'darwin', winCert: true }), [], 'publisherName is Windows only');
});

test('package.json names no repository that electron-builder could pick up', () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8'));
  assert.equal(pkg.repository, undefined);
  assert.equal(pkg.homepage, undefined);
  assert.ok(!fs.existsSync(path.join(DESKTOP, 'dev-app-update.yml')));
});

// --- aria2c and the Native Messaging host (SPEC-ext, ext-brief §4, §6.1) ---

const ARIA2C_WINDOWS_VERSION = `aria2 version 1.37.0
Copyright (C) 2006, 2019 Tatsuhiro Tsujikawa

** Configuration **
Enabled Features: Async DNS, BitTorrent, Firefox3 Cookie, GZip, HTTPS, Message Digest, Metalink, XML-RPC, SFTP
Hash Algorithms: sha-1, sha-224, sha-256, sha-384, sha-512, md5, adler32
Libraries: zlib/1.3 expat/2.5.0 sqlite3/3.43.1 GMP/6.3.0 c-ares/1.19.1 libssh2/1.11.0
Compiler: mingw-w64 8.0.0 (alpha) / gcc 10-win32 20220113
`;
const ARIA2C_MACOS_VERSION = `aria2 version 1.37.0
Copyright (C) 2006, 2019 Tatsuhiro Tsujikawa

** Configuration **
Enabled Features: GZip, HTTPS, Message Digest
Hash Algorithms: sha-1, sha-224, sha-256, sha-384, sha-512, md5, adler32
Libraries: zlib/1.2.12 AppleTLS
Compiler: Apple LLVM 21.0.0 (clang-2100.3.34.2)
`;

function readJSON(...p) {
  return JSON.parse(fs.readFileSync(path.join(...p), 'utf8'));
}

test('aria2c: pinned Windows build, macOS source and build script, GPL source and licenses declared', () => {
  const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
  const a = m.aria2c;
  const w = a['windows-x64'];
  const d = a['darwin-arm64'];
  assert.equal(a.version, '1.37.0');
  assert.equal(a.license, 'GPL-2.0-or-later', 'plain SPDX id: the OpenSSL permission is in license_note');
  assert.match(a.license_note, /OpenSSL/);
  // Every download pinned by an HTTPS URL and a SHA-256.
  for (const p of [a.source, w, d, ...w.deps, ...w.license_texts]) {
    assert.match(p.url, /^https:\/\//, p.url);
    assert.match(p.sha256, /^[0-9a-f]{64}$/, p.url);
  }
  assert.equal(w.url, 'https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0-win-64bit-build1.zip');
  assert.match(w.bin_sha256, /^[0-9a-f]{64}$/);
  assert.ok(w.bin_size > 1e6 && w.size > 1e6);
  assert.deepEqual([d.url, d.sha256, d.root], [a.source.url, a.source.sha256, a.source.root], 'macOS builds the release tarball');
  // macOS: AppleTLS only, the libraries and features of the brief turned off, nothing forbidden.
  for (const f of ['--with-appletls', '--without-openssl', '--without-gnutls', '--without-libxml2', '--without-libexpat', '--without-sqlite3',
    '--without-libssh2', '--without-libcares', '--disable-bittorrent', '--disable-metalink', 'ARIA2_STATIC=no']) assert.ok(d.configure.includes(f), f);
  assert.deepEqual(checkConfigure(d.configure.join(' '), d.forbidden_configure), []);
  assert.ok(!d.configure.some((f) => /^--with-(openssl|gnutls|lib)/.test(f) && f !== '--with-libz'), 'only zlib (a macOS system library) is enabled');
  assert.equal(d.deployment_target, undefined, 'the deployment target is macos.minimum_system_version, shared with ffmpeg and the app');
  const script = fs.readFileSync(path.join(REPO, 'packaging', 'aria2c', 'build-aria2c-macos.sh'), 'utf8');
  assert.match(script.split('\n').slice(0, 3).join('\n'), /SPDX-License-Identifier: AGPL-3\.0-or-later/);
  for (const s of ["m.aria2c['darwin-arm64'].configure", 'otool -L', 'forbidden_configure', 'codesign', 'minos',
    'm.macos.minimum_system_version', 'MACOSX_DEPLOYMENT_TARGET=$target', '-mmacosx-version-min=$target']) assert.ok(script.includes(s), s);
  // binaries.json: both builds, the same source as third-party.json, everything linked in declared.
  const bins = readJSON(REPO, 'packaging', 'licenses', 'binaries.json').binaries;
  const win = bins.find((b) => b.name === 'aria2c (Windows build)');
  const mac = bins.find((b) => b.name === 'aria2c (macOS build)');
  for (const [entry, pin] of [[win, w], [mac, d]]) {
    assert.ok(entry, 'binaries.json entry');
    assert.equal(entry.license, 'GPL-2.0-or-later');
    assert.match(entry.note, /OpenSSL/);
    assert.equal(entry.component, 'core');
    assert.deepEqual(entry.source_code, pin.source_code.map((s) => s.url), 'both manifests name the same source');
    assert.ok(entry.source_code.includes(a.source.url), 'the aria2 source tarball');
    assert.ok(entry.sha256.includes(pin.sha256));
    assert.ok(entry.version.startsWith(a.version));
    for (const l of entry.bundled) assert.ok(l.name && l.version && l.license && /^https:\/\//.test(l.url), JSON.stringify(l));
    assert.ok(entry.bundled.some((l) => l.name === 'wslay'), 'wslay (deps/ of the aria2 source)');
  }
  assert.deepEqual(win.platforms, ['windows']);
  assert.deepEqual(mac.platforms, ['darwin']);
  assert.ok(win.sha256.includes(w.bin_sha256), 'the SHA-256 of aria2c.exe itself');
  for (const dep of w.deps) {
    assert.ok(win.source_code.includes(dep.url), `${dep.name} source listed`);
    assert.ok(win.bundled.some((l) => l.name === dep.name && l.version === dep.version && l.license === dep.license), `${dep.name} bundled`);
    for (const l of dep.licenses) assert.ok(!l.includes('..') && !path.isAbsolute(l), l);
  }
  for (const v of Object.values(w.toolchain)) assert.ok(win.bundled.some((l) => l.version === v), `toolchain ${v} declared in bundled`);
  assert.equal(mac.configure, d.configure.join(' '));
  assert.deepEqual(mac.bundled.map((l) => l.name), ['wslay'], 'macOS: nothing else linked in');
  // The ffmpeg check stays about ffmpeg: aria2c is GPL, a separate program.
  assert.deepEqual(gplComponents(bins), []);
});

test('aria2c: the core component records the official Windows binary, which stage checks', () => {
  const w = readJSON(DESKTOP, 'packaging', 'third-party.json').aria2c['windows-x64'];
  const expect = { name: 'bin/aria2c.exe', url: '', sha256: w.bin_sha256, size: w.bin_size };
  for (const f of ['components.json', 'manifest.src.json']) {
    const core = readJSON(REPO, 'packaging', 'components', f).components.find((c) => c.id === 'core');
    assert.equal(core.bundled, true, f);
    assert.match(core.upstream, /aria2 1\.37\.0/, f);
    assert.deepEqual(core.platforms['windows-x64'].files, [expect], `${f}: in step with third-party.json`);
    assert.equal(core.platforms['windows-x64'].download_size, 0, 'bundled: nothing to download');
    assert.equal(core.platforms['darwin-arm64'], undefined, `${f}: no SHA-256 for the macOS build (built in CI, re-signed by electron-builder)`);
  }
  const shipped = readJSON(REPO, 'packaging', 'components', 'components.json');
  const staged = { name: 'bin/aria2c.exe', sha256: w.bin_sha256, size: w.bin_size, required: true };
  assert.deepEqual(bundledFileProblems(shipped, 'windows-x64', staged), []);
  assert.match(bundledFileProblems(shipped, 'windows-x64', { ...staged, sha256: 'a'.repeat(64) })[0], /records bin\/aria2c\.exe for windows-x64 as be2099c2/);
  assert.match(bundledFileProblems(shipped, 'windows-x64', { ...staged, size: 1 })[0], /\(1 bytes\)/);
  assert.match(bundledFileProblems(shipped, 'windows-x64', { ...staged, name: 'bin/other.exe' })[0], /records no bin\/other\.exe/);
  assert.deepEqual(bundledFileProblems(shipped, 'darwin-arm64', { name: 'bin/aria2c', sha256: 'b'.repeat(64), size: 1546192, required: false }), [], 'macOS: nothing recorded, nothing required');
  const withURL = JSON.parse(JSON.stringify(shipped));
  withURL.components.find((c) => c.id === 'core').platforms['windows-x64'].files[0].url = 'https://example.com/aria2c.exe';
  assert.match(bundledFileProblems(withURL, 'windows-x64', staged)[0], /no download URL/);
  assert.match(bundledFileProblems({ components: [{ id: 'core', platforms: {} }] }, 'windows-x64', staged)[0], /no bundled core/);
});

test('aria2c --version check', () => {
  assert.deepEqual(checkAria2cVersion(ARIA2C_WINDOWS_VERSION, '1.37.0', 'windows-x64'), []);
  assert.deepEqual(checkAria2cVersion(ARIA2C_MACOS_VERSION.replace(/\n/g, '\r\n'), '1.37.0', 'darwin-arm64'), []);
  assert.match(checkAria2cVersion(ARIA2C_WINDOWS_VERSION, '1.37.1', 'windows-x64')[0], /expected "aria2 version 1\.37\.1"/);
  assert.match(checkAria2cVersion(ARIA2C_WINDOWS_VERSION.replace(', HTTPS', ''), '1.37.0', 'windows-x64')[0], /HTTPS is not enabled/);
  assert.match(checkAria2cVersion(ARIA2C_WINDOWS_VERSION.replace('zlib/1.3', 'zlib/1.3 OpenSSL/3.0.0'), '1.37.0', 'windows-x64')[0], /OpenSSL/);
  // The Windows build's libraries and features would fail the macOS checks.
  const asMac = checkAria2cVersion(ARIA2C_WINDOWS_VERSION, '1.37.0', 'darwin-arm64');
  assert.match(asMac[0], /AppleTLS missing/);
  assert.match(asMac[1], /turns off: expat\/2\.5\.0 sqlite3\/3\.43\.1 GMP\/6\.3\.0 c-ares\/1\.19\.1 libssh2\/1\.11\.0$/);
  assert.match(asMac[2], /turns off: Async DNS, BitTorrent, Firefox3 Cookie, Metalink, XML-RPC, SFTP$/);
  assert.match(checkAria2cVersion(ARIA2C_MACOS_VERSION.replace('AppleTLS', 'GnuTLS/3.8.0'), '1.37.0', 'darwin-arm64').join('\n'), /GnuTLS[\s\S]*AppleTLS missing/);
  assert.equal(checkAria2cVersion('', '1.37.0', 'windows-x64').length, 2);
});

test('aria2c SOURCE.txt names the corresponding source (GPL): the release attachment first', () => {
  const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
  const a = m.aria2c;
  const repo = 'ShouldBeQuincy/LectureFlow';
  const win = aria2cSourceText('windows-x64', '3.2.0', repo);
  const mac = aria2cSourceText('darwin-arm64', '3.2.0', repo);
  assert.ok(win.startsWith(`aria2 1.37.0: resources/bin/aria2c.exe is aria2's official Windows build, unchanged (SHA-256 ${a['windows-x64'].bin_sha256}).`));
  assert.match(mac, /^aria2 1\.37\.0: resources\/bin\/aria2c is built from the aria2 source \(BUILDINFO\.txt\)/);
  for (const [text, key] of [[win, 'windows-x64'], [mac, 'darwin-arm64']]) {
    assert.match(text, /GPL-2\.0-or-later/);
    for (const s of a[key].source_code) assert.ok(text.includes(s.url), `${key}: ${s.url}`);
    const asset = `LectureFlow-3.2.0-source-aria2c-${key}.tar`;
    assert.ok(text.includes(`as ${asset}\n  https://github.com/${repo}/releases/download/v3.2.0/${asset}\n`), `${key}: the release attachment and its address`);
    assert.ok(text.includes(`https://github.com/${repo}/releases/tag/v3.2.0`));
    assert.ok(text.indexOf(asset) < text.indexOf(a.source.url), 'the attachment comes before the upstream addresses');
  }
  for (const f of a['darwin-arm64'].configure) assert.ok(mac.includes(`  ${f}\n`), f);
  assert.match(mac, /packaging\/aria2c\/build-aria2c-macos\.sh/);
  assert.ok(mac.includes(`for macOS ${m.macos.minimum_system_version} or later`));
  assert.match(win, /Dockerfile\.mingw/);
  assert.match(win, /gcc-mingw-w64 10\.3\.0/);
  assert.match(win, /Not attached \(no source obligation\):\n- MinGW-w64 runtime 8\.0\.0[^\n]*\n {2}The MinGW-w64 runtime is under the ZPL-2\.1/);
  assert.match(win, /GCC Runtime Library Exception 3\.1/);
  assert.doesNotMatch(mac, /Not attached/);
  // A fork's release run names its own repository.
  assert.ok(aria2cSourceText('darwin-arm64', '3.2.0', 'example-owner/lf').includes('https://github.com/example-owner/lf/releases/download/v3.2.0/LectureFlow-3.2.0-source-aria2c-darwin-arm64.tar'));
});

test('the Native Messaging host is built into resources/bin like the server', () => {
  assert.deepEqual(GO_PROGRAMS, [
    { name: 'lectureflow', pkg: './cmd/lectureflow' },
    { name: 'lectureflow-nmhost', pkg: './cmd/lectureflow-nmhost' },
  ]);
  const src = fs.readFileSync(path.join(DESKTOP, 'scripts', 'stage.js'), 'utf8');
  assert.match(src, /CGO_ENABLED: '0'/, 'pure Go, cross-compiled');
  assert.match(src, /GO_PROGRAMS\.map\(\(p\) => goBuild\(platform, version, p\)\)/, 'stage builds every program');
  for (const p of GO_PROGRAMS) assert.ok(fs.existsSync(path.join(REPO, ...p.pkg.split('/'), 'main.go')), `${p.pkg}/main.go`);
});

// --- minimum macOS (third-party.json macos) -------------------------------------------

test('minimum macOS: one value for the app, ffmpeg and aria2c; re-checked when Electron or Go changes', () => {
  const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
  const min = m.macos.minimum_system_version;
  assert.match(min, /^\d+\.\d+$/);
  assert.equal(loadConfig({}).mac.minimumSystemVersion, min, 'electron-builder writes it into LSMinimumSystemVersion');
  // Electron 44 and Go 1.27 need macOS 13.0 (third-party.json macos._doc). A new Electron
  // major or Go minor may need a newer macOS: look it up, then update macos.
  const pkg = readJSON(DESKTOP, 'package.json');
  assert.equal(pkg.devDependencies.electron.split('.')[0], m.macos.checked_with.electron,
    `Electron ${pkg.devDependencies.electron}: check its minimum macOS (LSMinimumSystemVersion of Electron.app, LC_BUILD_VERSION minos of Electron Framework) and update third-party.json macos`);
  const goVersion = /^go (\d+\.\d+)/m.exec(fs.readFileSync(path.join(REPO, 'go.mod'), 'utf8'))[1];
  assert.equal(goVersion, m.macos.checked_with.go, `Go ${goVersion}: check the linker's default macOS (cmd/link/internal/ld/macho.go) and update third-party.json macos`);
  assert.ok(compareVersions(min, '13.0') >= 0, 'Electron 44 and Go 1.27 need macOS 13.0');
  // Both macOS builds compile for it, check the result and only then publish the build.
  const ff = fs.readFileSync(path.join(DESKTOP, 'scripts', 'build-ffmpeg-macos.sh'), 'utf8');
  for (const s of ['m.macos.minimum_system_version', 'export MACOSX_DEPLOYMENT_TARGET="$target"', 'libdav1d.a', '[ "$m" = "$target" ]',
    'lipo -archs', 'DESTDIR="$destdir"', 'mv "$part" "$prefix"', 'BUILDINFO.txt']) assert.ok(ff.includes(s), s);
  assert.ok(ff.indexOf('mv "$part" "$prefix"') > ff.lastIndexOf('exit 1'), 'renamed after the last check');
  const ar = fs.readFileSync(path.join(REPO, 'packaging', 'aria2c', 'build-aria2c-macos.sh'), 'utf8');
  assert.ok(ar.indexOf('mv "$part" "$out"') > ar.lastIndexOf('exit 1'), 'renamed after the last check');
});

const LC_BUILD_VERSION = 0x32;
const ARM64 = 0x0100000c;
const X86_64 = 0x01000007;

function machVer(s) {
  const [a, b = 0, c = 0] = s.split('.').map(Number);
  return (a << 16) | (b << 8) | c;
}
function lcBuildVersion(minos, platform = 1) {
  const b = Buffer.alloc(24);
  b.writeUInt32LE(LC_BUILD_VERSION, 0);
  b.writeUInt32LE(24, 4);
  b.writeUInt32LE(platform, 8);
  b.writeUInt32LE(machVer(minos), 12);
  b.writeUInt32LE(machVer('26.5'), 16);
  return b;
}
function lcVersionMinMacosx(v) {
  const b = Buffer.alloc(16);
  b.writeUInt32LE(0x24, 0);
  b.writeUInt32LE(16, 4);
  b.writeUInt32LE(machVer(v), 8);
  b.writeUInt32LE(machVer('14.0'), 12);
  return b;
}
function lcSegment() {
  const b = Buffer.alloc(72); // LC_SEGMENT_64 without sections: a load command to skip
  b.writeUInt32LE(0x19, 0);
  b.writeUInt32LE(72, 4);
  return b;
}
function machO({ cpu = ARM64, cmds = [] } = {}) {
  const body = Buffer.concat(cmds);
  const h = Buffer.alloc(32);
  h.writeUInt32LE(0xfeedfacf, 0);
  h.writeInt32LE(cpu, 4);
  h.writeUInt32LE(2, 12); // MH_EXECUTE
  h.writeUInt32LE(cmds.length, 16);
  h.writeUInt32LE(body.length, 20);
  return Buffer.concat([h, body, Buffer.alloc(64)]);
}
function fatMachO(slices) {
  const header = Buffer.alloc(8 + 20 * slices.length);
  header.writeUInt32BE(0xcafebabe, 0);
  header.writeUInt32BE(slices.length, 4);
  const parts = [];
  let offset = 4096;
  slices.forEach((s, i) => {
    header.writeInt32BE(s.readInt32LE(4), 8 + i * 20);
    header.writeUInt32BE(offset, 8 + i * 20 + 8);
    header.writeUInt32BE(s.length, 8 + i * 20 + 12);
    header.writeUInt32BE(12, 8 + i * 20 + 16);
    parts.push({ offset, s });
    offset += Math.ceil(s.length / 4096) * 4096;
  });
  const buf = Buffer.alloc(offset);
  header.copy(buf, 0);
  for (const p of parts) p.s.copy(buf, p.offset);
  return buf;
}
const reader = (buf) => (off, len) => buf.subarray(off, off + len);

test('Mach-O minimum macOS: LC_BUILD_VERSION, LC_VERSION_MIN_MACOSX, universal files, not Mach-O', () => {
  assert.equal(machoVersion(0x000d0000), '13.0');
  assert.equal(machoVersion(0x000c0601), '12.6.1');
  assert.equal(machoVersion(machVer('27.0')), '27.0');
  assert.equal(compareVersions('13.0', '12.6.1'), 1);
  assert.equal(compareVersions('13.0', '13'), 0);
  assert.equal(compareVersions('13.0.1', '13.0'), 1);
  assert.equal(compareVersions('11.0', '13.0'), -1);
  assert.equal(compareVersions('10.15', '10.9'), 1, 'numeric, not lexical');
  assert.deepEqual(machoSlices(reader(machO({ cmds: [lcSegment(), lcBuildVersion('13.0'), lcSegment()] }))), [{ cpu: ARM64, platform: 1, minos: '13.0' }]);
  assert.deepEqual(machoSlices(reader(machO({ cmds: [lcVersionMinMacosx('10.13')] }))), [{ cpu: ARM64, platform: 1, minos: '10.13' }]);
  assert.deepEqual(machoSlices(reader(machO({ cmds: [lcSegment()] }))), [{ cpu: ARM64, platform: null, minos: null }]);
  assert.deepEqual(machoSlices(reader(machO({ cmds: [lcBuildVersion('17.0', 2)] }))), [{ cpu: ARM64, platform: 2, minos: '17.0' }], 'iOS');
  assert.deepEqual(machoSlices(reader(fatMachO([machO({ cmds: [lcBuildVersion('13.0')] }), machO({ cpu: X86_64, cmds: [lcBuildVersion('10.15')] })]))), [
    { cpu: ARM64, platform: 1, minos: '13.0' }, { cpu: X86_64, platform: 1, minos: '10.15' },
  ]);
  const notMachO = [Buffer.from('#!/bin/sh\necho hi\n'), Buffer.from([0x4d, 0x5a, 0x90, 0, 3, 0, 0, 0]), Buffer.from([0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 0x34, 0, 0]), Buffer.alloc(3)];
  for (const other of notMachO) assert.equal(machoSlices(reader(other)), null, other.toString('hex'));
});

test('stage check: every Mach-O file in resources/ runs on the declared minimum macOS', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-macho-'));
  const empty = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-macho-empty-'));
  try {
    const put = (rel, data) => {
      fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true });
      fs.writeFileSync(path.join(dir, rel), data);
    };
    put('bin/lectureflow', machO({ cmds: [lcSegment(), lcBuildVersion('13.0')] }));
    put('bin/uv', machO({ cmds: [lcBuildVersion('11.0')] }));
    put('worker/asr.py', 'print("hi")\n');
    put('licenses/aria2/COPYING', 'GNU GENERAL PUBLIC LICENSE');
    let r = macMinimumCheck(dir, '13.0', 'darwin-arm64');
    assert.deepEqual(r.problems, []);
    assert.deepEqual(r.files, [{ path: 'bin/lectureflow', minos: '13.0' }, { path: 'bin/uv', minos: '11.0' }]);
    // What the old ffmpeg build script produced: the build machine's macOS.
    put('bin/ffmpeg', machO({ cmds: [lcBuildVersion('27.0')] }));
    // otool cannot open names like this one; the check reads the file itself.
    put('Helper (GPU)/bin', machO({ cpu: X86_64, cmds: [lcBuildVersion('12.0')] }));
    put('bin/old', machO({ cmds: [lcSegment()] }));
    put('bin/ios', machO({ cmds: [lcBuildVersion('13.0', 2)] }));
    put('bin/universal', fatMachO([machO({ cmds: [lcBuildVersion('13.0')] }), machO({ cpu: X86_64, cmds: [lcBuildVersion('14.0')] })]));
    r = macMinimumCheck(dir, '13.0', 'darwin-arm64');
    const want = [
      'Helper (GPU)/bin: no arm64 code',
      'bin/ffmpeg needs macOS 27.0, the app declares 13.0 (build it with MACOSX_DEPLOYMENT_TARGET=13.0, or raise macos.minimum_system_version)',
      'bin/ios: built for platform 2, not macOS',
      'bin/old: records no minimum macOS (LC_BUILD_VERSION)',
      'bin/universal needs macOS 14.0, the app declares 13.0 (build it with MACOSX_DEPLOYMENT_TARGET=13.0, or raise macos.minimum_system_version)',
    ];
    assert.deepEqual(r.problems.slice().sort(), want.slice().sort());
    assert.deepEqual(macMinimumCheck(dir, '27.0', 'darwin-arm64').problems.filter((p) => /needs macOS/.test(p)), [], 'a higher declared minimum accepts them');
    fs.writeFileSync(path.join(empty, 'ffmpeg.exe'), 'MZ');
    assert.deepEqual(macMinimumCheck(empty, '13.0', 'windows-x64'), { files: [], problems: [] });
    assert.match(macMinimumCheck(empty, '13.0', 'darwin-arm64').problems[0], /no Mach-O file found/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(empty, { recursive: true, force: true });
  }
});

// --- corresponding source as release attachments (scripts/sources.js) -----------------------

test('source attachments: every source_code entry is attached, the attachment itself, or explained', () => {
  const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
  assert.deepEqual(sources.bundleProblems(), []);
  const b = sources.bundles();
  assert.deepEqual(b.map((x) => x.id), ['ffmpeg-windows-x64', 'ffmpeg-darwin-arm64', 'aria2c-windows-x64', 'aria2c-darwin-arm64']);
  const files = Object.fromEntries(b.map((x) => [x.id, x.files.map((f) => f.archive)]));
  assert.deepEqual(files, {
    'ffmpeg-windows-x64': ['ffmpeg-9.0.2.tar.xz', 'zlib-v1.3.1.tar.gz', 'nv-codec-headers-n12.1.14.0.tar.gz', 'OpenH264-v2.6.0.tar.gz', 'libvpl-v2.16.0.tar.gz', 'dav1d-1.5.4.tar.xz'],
    'ffmpeg-darwin-arm64': ['ffmpeg-9.0.2.tar.xz', 'dav1d-1.5.4.tar.xz'],
    'aria2c-windows-x64': ['aria2-1.37.0.tar.xz', 'gmp-6.3.0.tar.xz', 'expat-2.5.0.tar.bz2', 'sqlite-autoconf-3430100.tar.gz', 'zlib-1.3.tar.gz', 'c-ares-1.19.1.tar.gz', 'libssh2-1.11.0.tar.bz2'],
    'aria2c-darwin-arm64': ['aria2-1.37.0.tar.xz'],
  }, 'wslay is inside the aria2 tarball: attached once');
  assert.deepEqual(b.map((x) => x.scripts), [
    ['desktop/scripts/build-ffmpeg-windows.sh', 'desktop/packaging/third-party.json'],
    ['desktop/scripts/build-ffmpeg-macos.sh', 'desktop/packaging/third-party.json'],
    ['desktop/packaging/third-party.json'],
    ['packaging/aria2c/build-aria2c-macos.sh', 'desktop/packaging/third-party.json'],
  ]);
  // Only the toolchain runtimes stay out (GCC Runtime Library Exception, ZPL-2.1).
  assert.deepEqual(b.flatMap((x) => x.notAttached.map((s) => s.url)), [
    'https://launchpad.net/ubuntu/+source/mingw-w64/11.0.1-3build1', 'https://launchpad.net/ubuntu/+source/gcc-mingw-w64/26.1',
    'https://launchpad.net/ubuntu/+source/mingw-w64/8.0.0-1', 'https://launchpad.net/ubuntu/+source/gcc-mingw-w64/24.3',
  ]);
  // The libraries binaries.json declares as linked in are attached, except those runtimes.
  const bins = readJSON(REPO, 'packaging', 'licenses', 'binaries.json').binaries;
  const pairs = [['ffmpeg / ffprobe (Windows build)', 'ffmpeg-windows-x64'], ['ffmpeg / ffprobe (macOS build)', 'ffmpeg-darwin-arm64'],
    ['aria2c (Windows build)', 'aria2c-windows-x64'], ['aria2c (macOS build)', 'aria2c-darwin-arm64']];
  for (const [name, id] of pairs) {
    const entry = bins.find((x) => x.name === name);
    const bundle = b.find((x) => x.id === id);
    assert.equal(entry.source_code[0], `https://github.com/${m.release_sources.repository}/releases`, `${name}: the release attachment first`);
    assert.ok(entry.note.includes(`LectureFlow-<version>-source-${id}.tar`), `${name}: the note names the attachment`);
    for (const lib of entry.bundled) {
      if (/runtime/i.test(lib.name)) continue;
      assert.ok(bundle.files.some((f) => lib.name.startsWith(f.name) || (lib.name === 'wslay' && f.name === 'aria2')), `${name}: ${lib.name} attached`);
    }
  }
  // The checks catch an unexplained or unlisted source.
  const broken = (edit) => {
    const c = JSON.parse(JSON.stringify(m));
    edit(c);
    return sources.bundleProblems(c).join('\n');
  };
  assert.match(broken((c) => delete c.aria2c['windows-x64'].source_code.find((s) => s.not_attached).not_attached), /neither attached \(no pinned tarball\) nor explained/);
  assert.match(broken((c) => c.ffmpeg['windows-x64'].source_code.splice(2, 1)), /zlib.*attached but not listed in source_code/);
  assert.match(broken((c) => c.ffmpeg['darwin-arm64'].source_code.shift()), /exactly one release_asset entry, has 0/);
  assert.match(broken((c) => c.aria2c['darwin-arm64'].build_scripts.push('packaging/aria2c/missing.sh')), /build script packaging\/aria2c\/missing\.sh not found/);
  assert.match(broken((c) => { c.aria2c['darwin-arm64'].source_code[0].what = 'somewhere'; }), /does not name its attachment/);
  assert.equal(sources.assetName('3.1.0', 'ffmpeg-windows-x64'), 'LectureFlow-3.1.0-source-ffmpeg-windows-x64.tar');
  assert.equal(sources.assetName('3.2.0-beta.1', 'aria2c-darwin-arm64'), 'LectureFlow-3.2.0-beta.1-source-aria2c-darwin-arm64.tar');
  for (const bad of ['v3.1.0', '../3.1.0', '3.1', '']) assert.throws(() => sources.assetName(bad, 'x'), /bad version/);
  assert.equal(sources.releaseRepo({}), 'ShouldBeQuincy/LectureFlow');
  assert.equal(sources.releaseRepo({ GITHUB_REPOSITORY: 'example-owner/lf.fork' }), 'example-owner/lf.fork', 'a release run names its own repository');
  assert.equal(sources.releaseRepo({ GITHUB_REPOSITORY: 'a/b/c' }), 'ShouldBeQuincy/LectureFlow');
});

test('ffmpeg SOURCE.txt names the release attachment (LGPL)', () => {
  const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
  const repo = 'ShouldBeQuincy/LectureFlow';
  for (const key of ['windows-x64', 'darwin-arm64']) {
    const text = ffmpegSourceText(key, '3.2.0', repo);
    const asset = `LectureFlow-3.2.0-source-ffmpeg-${key}.tar`;
    assert.ok(text.includes(`as ${asset}\n  https://github.com/${repo}/releases/download/v3.2.0/${asset}\n`), key);
    assert.match(text, /LGPL-2\.1-or-later/);
    for (const s of m.ffmpeg[key].source_code) assert.ok(text.includes(s.url), `${key}: ${s.url}`);
  }
  assert.match(ffmpegSourceText('windows-x64', '3.2.0', repo), /DLLs can be replaced[\s\S]*Not attached \(no source obligation\)[\s\S]*GCC Runtime Library Exception/);
  assert.ok(ffmpegSourceText('darwin-arm64', '3.2.0', repo).includes(`for macOS ${m.macos.minimum_system_version} or later`));
});

/** readTar(buf) → [{name, mode, mtime, data}], checking every header checksum. */
function readTar(buf) {
  const out = [];
  for (let p = 0; p + 512 <= buf.length;) {
    const h = buf.subarray(p, p + 512);
    if (h.every((x) => x === 0)) break;
    const field = (o, n) => h.subarray(o, o + n).toString('ascii').replace(/\0[\s\S]*$/, '');
    let sum = 0;
    for (let i = 0; i < 512; i++) sum += i >= 148 && i < 156 ? 32 : h[i];
    assert.equal(parseInt(field(148, 8), 8), sum, 'header checksum');
    assert.equal(field(257, 6), 'ustar');
    const size = parseInt(field(124, 12), 8);
    const prefix = field(345, 155);
    out.push({ name: prefix ? `${prefix}/${field(0, 100)}` : field(0, 100), mode: parseInt(field(100, 8), 8), mtime: parseInt(field(136, 12), 8), data: buf.subarray(p + 512, p + 512 + size) });
    p += 512 + Math.ceil(size / 512) * 512;
  }
  return out;
}

test('source attachments: pinned downloads into reproducible tars, nothing left behind on failure', async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-sources-'));
  try {
    // A copy of the manifest whose every pinned file is a small fake served by fakeFetch.
    const m = readJSON(DESKTOP, 'packaging', 'third-party.json');
    const bodies = new Map();
    const pin = (o) => {
      if (o && typeof o === 'object') {
        if (typeof o.url === 'string' && typeof o.sha256 === 'string') {
          if (!bodies.has(o.url)) bodies.set(o.url, Buffer.from(`source tarball from ${o.url}\n`.repeat(50)));
          o.sha256 = crypto.createHash('sha256').update(bodies.get(o.url)).digest('hex');
        }
        Object.values(o).forEach(pin);
      }
    };
    pin(m);
    const fetched = [];
    const fakeFetch = async (url) => {
      fetched.push(url);
      return new Response(bodies.get(url));
    };
    const opts = { version: '3.2.0', repo: 'example-owner/lf', cacheDir: path.join(tmp, 'cache'), manifest: m, fetchImpl: fakeFetch, log: () => {}, mtime: 1790000000 };
    const results = await sources.buildSources({ ...opts, outDir: path.join(tmp, 'out') });
    assert.deepEqual(results.map((r) => r.asset), ['ffmpeg-windows-x64', 'ffmpeg-darwin-arm64', 'aria2c-windows-x64', 'aria2c-darwin-arm64'].map((id) => `LectureFlow-3.2.0-source-${id}.tar`));
    const unique = new Set(sources.bundles(m).flatMap((b) => b.files.map((f) => f.url)));
    assert.equal(fetched.length, unique.size, 'each tarball downloaded once (cache)');
    assert.deepEqual(fs.readdirSync(path.join(tmp, 'out')).sort(), results.map((r) => r.asset).sort(), 'no .partial left');
    const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
    for (const r of results) {
      const buf = fs.readFileSync(path.join(tmp, 'out', r.asset));
      assert.equal(buf.length, r.bytes);
      const entries = readTar(buf);
      const root = r.asset.replace(/\.tar$/, '');
      const bundle = sources.bundles(m).find((b) => b.id === r.id);
      assert.deepEqual(entries.map((e) => e.name), [
        `${root}/README.txt`, `${root}/SHA256SUMS`, ...bundle.files.map((f) => `${root}/upstream/${f.archive}`), ...bundle.scripts.map((s) => `${root}/lectureflow/${s}`),
      ]);
      assert.ok(entries.every((e) => e.mtime === 1790000000));
      const sums = entries[1].data.toString('utf8').trim().split('\n');
      assert.equal(sums.length, entries.length - 2);
      for (const line of sums) {
        const [sha, rel] = line.split('  ');
        const e = entries.find((x) => x.name === `${root}/${rel}`);
        assert.equal(crypto.createHash('sha256').update(e.data).digest('hex'), sha, rel);
      }
      for (const f of bundle.files) assert.deepEqual(entries.find((e) => e.name.endsWith(`/upstream/${f.archive}`)).data, bodies.get(f.url), 'unchanged upstream file');
      for (const s of bundle.scripts) {
        const e = entries.find((x) => x.name === `${root}/lectureflow/${s}`);
        assert.deepEqual(e.data, fs.readFileSync(path.join(REPO, ...s.split('/'))));
        assert.equal(e.mode, s.endsWith('.sh') ? 0o755 : 0o644);
      }
      const readme = entries[0].data.toString('utf8');
      assert.ok(readme.includes('https://github.com/example-owner/lf/releases/tag/v3.2.0'));
      for (const f of bundle.files) assert.ok(readme.includes(f.url) && readme.includes(f.sha256), f.url);
      // The system tar reads it too (bsdtar on Windows and macOS).
      const x = path.join(tmp, 'x', r.id);
      fs.mkdirSync(x, { recursive: true });
      const t = spawnSync(tar, ['-xf', path.join(tmp, 'out', r.asset), '-C', x], { encoding: 'utf8' });
      assert.equal(t.status, 0, t.stderr);
      for (const e of entries) assert.deepEqual(fs.readFileSync(path.join(x, ...e.name.split('/'))), e.data, e.name);
    }
    // Same inputs, same bytes.
    const again = await sources.buildSources({ ...opts, outDir: path.join(tmp, 'out2') });
    for (const r of again) assert.ok(fs.readFileSync(path.join(tmp, 'out2', r.asset)).equals(fs.readFileSync(path.join(tmp, 'out', r.asset))), r.asset);
    const notes = sources.renderNotes(results);
    assert.match(notes, /^## Source code of the bundled ffmpeg and aria2c/);
    assert.match(notes, /\| LectureFlow-3\.2\.0-source-aria2c-windows-x64\.tar \| aria2c\.exe in the Windows installers \(GPL-2\.0-or-later\) \| [\d.]+ MB \| aria2-1\.37\.0\.tar\.xz, gmp-6\.3\.0\.tar\.xz/);
    // A tarball that does not match its pin fails the run and leaves no attachment for it.
    const bad = JSON.parse(JSON.stringify(m));
    bad.aria2c['windows-x64'].deps.find((d) => d.name === 'GMP').sha256 = 'a'.repeat(64);
    const outBad = path.join(tmp, 'out-bad');
    await assert.rejects(sources.buildSources({ ...opts, manifest: bad, outDir: outBad, cacheDir: path.join(tmp, 'cache-bad') }), /SHA-256 mismatch for https:\/\/ftp\.gnu\.org/);
    assert.deepEqual(fs.readdirSync(outBad).filter((f) => /aria2c-windows|\.partial$/.test(f)), []);
    // Inconsistent manifests fail before anything is downloaded.
    const unexplained = JSON.parse(JSON.stringify(m));
    delete unexplained.ffmpeg['windows-x64'].source_code.find((s) => s.not_attached).not_attached;
    const n = fetched.length;
    await assert.rejects(sources.buildSources({ ...opts, manifest: unexplained, outDir: path.join(tmp, 'out3') }), /neither attached/);
    assert.equal(fetched.length, n);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test('tar headers: long names split into prefix and name, bad names refused', () => {
  const long = `${'d'.repeat(60)}/${'e'.repeat(60)}/file-with-a-long-name.tar.xz`;
  const h = sources.tarHeader({ name: long, size: 5, mode: 0o644, mtime: 0 });
  assert.equal(h.length, 512);
  const [e] = readTar(Buffer.concat([h, Buffer.from('hello'), Buffer.alloc(507 + 1024)]));
  assert.equal(e.name, long);
  assert.equal(e.data.toString(), 'hello');
  for (const bad of ['/abs/path', 'a/../b', 'caf\u00e9.txt', '', 'x'.repeat(101)]) assert.throws(() => sources.tarHeader({ name: bad, size: 0, mode: 0o644, mtime: 0 }), /tar: /, bad);
});

test('release workflow: source attachments next to the installers, macOS ffmpeg cached like aria2c', () => {
  const rel = fs.readFileSync(path.join(REPO, '.github', 'workflows', 'release.yml'), 'utf8');
  const ci = fs.readFileSync(path.join(REPO, '.github', 'workflows', 'ci.yml'), 'utf8');
  assert.match(rel, /\n {2}sources:\n/);
  assert.ok(rel.includes('node desktop/scripts/sources.js --out sources --version "${GITHUB_REF_NAME#v}" --notes sources/notes.md'));
  assert.match(rel, /needs: \[build, sources\]/);
  assert.match(rel, /gh release create[^\n]*\n?[^\n]*out\/\* sources\/\*\.tar/);
  const key = "ffmpeg-macos-${{ hashFiles('desktop/scripts/build-ffmpeg-macos.sh', 'desktop/packaging/third-party.json') }}";
  for (const [name, text] of [['release.yml', rel], ['ci.yml', ci]]) {
    assert.ok(text.includes(`key: ${key}`), `${name}: the ffmpeg-macos cache key`);
    assert.ok(text.includes('path: desktop/.cache/ffmpeg-macos-*'), `${name}: the ffmpeg-macos cache path`);
  }
  assert.match(ci, /\n {2}ffmpeg-macos:\n {4}runs-on: macos-14\n/);
});

test('update summary: the electron-updater feed and whether the in-app update key is built in (keys.go)', () => {
  const { updateBuildConfig, builtinUpdateKey, minisignKeyId, KEYS_GO } = require('../../scripts/lib/updaterepo');
  // The maintainer's key (internal/selfupdate/keys.go, update-key 2026-10-27): its id as minisign prints it.
  assert.equal(minisignKeyId('RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nTX'), '0FAC4F1B6C17636D');
  for (const bad of ['', 'RW', 'RWRtYxdsG0+sD5yxDvxA4MOrUrK3WGrkwpejK2VfGHH3GibjDpES6nT', 'x'.repeat(56), null]) assert.equal(minisignKeyId(bad), null, String(bad));
  const key = Buffer.concat([Buffer.from('Ed'), Buffer.from('0807060504030201', 'hex'), Buffer.alloc(32, 7)]).toString('base64');
  const read = (text) => (p) => {
    assert.equal(p, path.join('/repo', KEYS_GO));
    return text;
  };
  const withKey = builtinUpdateKey('/repo', read(`package selfupdate\n\n// PublicKey …\nconst PublicKey = "${key}"\n`));
  assert.deepEqual(withKey, { key, id: '0102030405060708' });
  const noKey = builtinUpdateKey('/repo', read('package selfupdate\n\nconst PublicKey = ""\n'));
  assert.deepEqual(noKey, { key: '', id: null });
  assert.throws(() => builtinUpdateKey('/repo', read('package selfupdate\n')), /no line const PublicKey/);
  assert.throws(() => builtinUpdateKey('/repo', read('const PublicKey = "not a key"\n')), /not a minisign public key/);
  assert.throws(() => builtinUpdateKey('/repo', () => { throw new Error('ENOENT'); }), /ENOENT/);
  // The checkout itself parses (dist.js reads it before staging).
  const own = builtinUpdateKey(REPO);
  assert.ok(own.key === '' || /^[0-9A-F]{16}$/.test(own.id), JSON.stringify(own));

  // The line goes into the build log, size-report.md and the release notes:
  // it must not say "automatic updates: off" for a build that updates itself.
  const offOn = updateBuildConfig({}, { inAppKey: withKey }).summary;
  assert.equal(offOn, 'electron-updater feed: off (LECTUREFLOW_UPDATE_REPO is not set: no app-update.yml); in-app updates: on (minisign key 0102030405060708 built in; the shell does not load electron-updater)');
  assert.doesNotMatch(offOn, /automatic updates: off/i);
  assert.equal(updateBuildConfig({}, { inAppKey: noKey }).summary, 'electron-updater feed: off (LECTUREFLOW_UPDATE_REPO is not set: no app-update.yml); in-app updates: off (no minisign key built in)');
  assert.equal(updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }, { inAppKey: withKey }).summary,
    'electron-updater feed: on (GitHub releases of o/r, app-update.yml, unused while in-app updates are on); in-app updates: on (minisign key 0102030405060708 built in; the shell does not load electron-updater)');
  assert.equal(updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }, { inAppKey: noKey }).summary, 'electron-updater feed: on (GitHub releases of o/r, app-update.yml); in-app updates: off (no minisign key built in)');
  // The rest of the build configuration does not depend on the key.
  assert.deepEqual(updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }, { inAppKey: withKey }).publish, updateBuildConfig({ LECTUREFLOW_UPDATE_REPO: 'o/r' }).publish);
  assert.equal(updateBuildConfig({}, { inAppKey: withKey }).publish, null);
});

// --- ssh-1b: the SSH servers' agent in resources/agent ---------------------------------

test('lf-agent: one static build per remote target, the platforms the service knows', () => {
  // the targets are internal/remote.Platforms, in its order
  const proto = fs.readFileSync(path.join(REPO, 'internal', 'remote', 'protocol.go'), 'utf8');
  const consts = Object.fromEntries([...proto.matchAll(/(Platform\w+)\s*=\s*"([a-z0-9-]+)"/g)].map((m) => [m[1], m[2]]));
  const list = proto.match(/var Platforms = \[\]string\{([^}]*)\}/)[1].split(',').map((x) => consts[x.trim()]);
  assert.deepEqual(AGENT_TARGETS.map((t) => t.target), list);
  assert.deepEqual(AGENT_TARGETS.map((t) => `${t.goos}/${t.goarch}`), ['linux/amd64', 'darwin/arm64', 'windows/amd64']);
  // built like make.ps1 agent: trimmed, stripped, the app version in internal/agent.Version
  const args = agentBuildArgs('3.2.0', '/out/lf-agent');
  assert.deepEqual(args, ['build', '-trimpath', '-ldflags', '-s -w -X lectureflow/internal/agent.Version=3.2.0', '-o', '/out/lf-agent', './cmd/lf-agent']);
  assert.ok(fs.existsSync(path.join(REPO, 'cmd', 'lf-agent', 'main.go')), 'cmd/lf-agent/main.go');
  assert.match(fs.readFileSync(path.join(REPO, 'internal', 'agent', 'agent.go'), 'utf8'), /\bVersion\b/, 'internal/agent has the Version the build sets');
  const src = fs.readFileSync(path.join(DESKTOP, 'scripts', 'stage.js'), 'utf8');
  assert.match(src, /CGO_ENABLED: '0', GOOS: t\.goos, GOARCH: t\.goarch/, 'pure Go, cross-compiled for each target');
  assert.match(src, /const agent = withAgent \? stageAgent\(version\) : null;/, 'stage builds the agents when they are bundled');
  // where the service looks: resources/agent/<platform>/lf-agent
  assert.match(fs.readFileSync(path.join(REPO, 'cmd', 'lectureflow', 'ssh.go'), 'utf8'), /filepath\.Join\(abs, "agent"\)/);
  assert.match(fs.readFileSync(path.join(REPO, 'internal', 'sshremote', 'bootstrap.go'), 'utf8'), /filepath\.Join\(dir, platform, "lf-agent"\)/);
});

test('lf-agent: stageAgent writes every target and kit.json with their SHA-256', () => {
  const out = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-agent-stage-'));
  try {
    const built = [];
    const kit = stageAgent('3.2.0', {
      out,
      build: (t, file) => {
        built.push(t.target);
        fs.writeFileSync(file, `agent for ${t.goos}/${t.goarch}`);
      },
    });
    assert.deepEqual(built, ['linux-x64', 'darwin-arm64', 'windows-x64']);
    const onDisk = JSON.parse(fs.readFileSync(path.join(out, 'agent', 'kit.json'), 'utf8'));
    assert.deepEqual(onDisk, kit);
    assert.equal(kit.version, '3.2.0');
    for (const t of AGENT_TARGETS) {
      const file = path.join(out, 'agent', t.target, 'lf-agent');
      assert.equal(kit.builds[t.target], crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex'), t.target);
      if (process.platform !== 'win32' && t.goos !== 'windows') assert.ok(fs.statSync(file).mode & 0o100, `${t.target} is executable`);
    }
    // a build that leaves no file fails staging
    assert.throws(() => stageAgent('3.2.0', { out, build: (t, file) => t.target === 'darwin-arm64' && fs.rmSync(file, { force: true }) }), /lf-agent for darwin-arm64 was not built/);
  } finally {
    fs.rmSync(out, { recursive: true, force: true });
  }
});

test('lf-agent: left out of the installers by default, LECTUREFLOW_BUNDLE_LF_AGENT=1 puts it in', () => {
  // user decision 2026-11-11: not bundled while LECTUREFLOW_EXPERIMENTAL_SSH is experimental
  assert.equal(BUNDLE_AGENT_ENV, 'LECTUREFLOW_BUNDLE_LF_AGENT');
  assert.equal(BUNDLE_AGENT_DEFAULT, false, 'flip this (and BUNDLE_AGENT_DEFAULT in stage.js) when the SSH servers become public');
  assert.equal(bundleAgent({}), false);
  assert.equal(bundleAgent({ LECTUREFLOW_BUNDLE_LF_AGENT: '' }), false);
  assert.equal(bundleAgent({ LECTUREFLOW_BUNDLE_LF_AGENT: '0' }), false);
  assert.equal(bundleAgent({ LECTUREFLOW_BUNDLE_LF_AGENT: '1' }), true);
  assert.equal(bundleAgent({ LECTUREFLOW_EXPERIMENTAL_SSH: '1' }), false, 'the runtime switch does not bundle it');
  for (const bad of ['true', 'yes', '2']) {
    assert.throws(() => bundleAgent({ LECTUREFLOW_BUNDLE_LF_AGENT: bad }), /LECTUREFLOW_BUNDLE_LF_AGENT must be 1 or 0/);
  }
  // stage reads the switch before building and builds resources/agent only with it
  const src = fs.readFileSync(path.join(DESKTOP, 'scripts', 'stage.js'), 'utf8');
  const body = src.slice(src.indexOf('async function stage('), src.indexOf('\nmodule.exports'));
  assert.ok(body.indexOf('bundleAgent(env)') >= 0 && body.indexOf('bundleAgent(env)') < body.indexOf('fs.rmSync(OUT'), 'the switch is read before OUT is emptied');
  assert.equal((body.match(/stageAgent\(/g) || []).length, 1, 'one call, behind the switch');
  assert.match(body, /const agent = withAgent \? stageAgent\(version\) : null;/);
  // OUT is emptied before staging, so no agent of an earlier build stays in resources/
  assert.ok(body.indexOf('fs.rmSync(OUT, { recursive: true, force: true })') < body.indexOf('stageAgent('));
  // nothing else in the build puts lf-agent into the installer: electron-builder copies build/resources only
  const c = loadConfig({});
  assert.deepEqual(c.extraResources, [{ from: 'build/resources', to: '.', filter: ['**/*'] }]);
  assert.ok(!JSON.stringify(c.files || []).includes('agent'), 'files does not name the agent');
  for (const f of fs.readdirSync(path.join(DESKTOP, 'scripts')).filter((x) => x.endsWith('.js') && x !== 'stage.js')) {
    assert.doesNotMatch(fs.readFileSync(path.join(DESKTOP, 'scripts', f), 'utf8'), /lf-agent|stageAgent/, `scripts/${f} does not build the agent`);
  }
});

test('lf-agent: the macOS app leaves the Linux and Windows agents unsigned', () => {
  const c = loadConfig({});
  const re = c.mac.signIgnore.map((x) => new RegExp(x));
  const app = '/build/dist/mac-arm64/LectureFlow.app/Contents/Resources';
  const ignored = (p) => re.some((r) => r.test(p));
  assert.ok(ignored(`${app}/agent/linux-x64/lf-agent`));
  assert.ok(ignored(`${app}/agent/windows-x64/lf-agent`));
  assert.ok(!ignored(`${app}/agent/darwin-arm64/lf-agent`), 'the Mach-O agent is signed like the rest');
  assert.ok(!ignored(`${app}/bin/lectureflow`));
  assert.deepEqual(loadConfig({ CSC_NAME: 'Developer ID Application: X' }).mac.signIgnore, c.mac.signIgnore, 'the same with a certificate');
});
