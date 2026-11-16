// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Stages everything that goes next to the app (electron-builder
// extraResources) into desktop/build/resources:
//
//   bin/lectureflow(.exe)   go build -trimpath -ldflags "-s -w -X main.version=V"
//   bin/lectureflow-nmhost(.exe)  the browser extension's Native Messaging host
//                           (./cmd/lectureflow-nmhost, built like the server)
//   agent/<target>/lf-agent only with LECTUREFLOW_BUNDLE_LF_AGENT=1 (off by default
//                           while the SSH servers are experimental, behind
//                           LECTUREFLOW_EXPERIMENTAL_SSH; see bundleAgent):
//                           the SSH inference servers' agent (ssh-1b; ./cmd/lf-agent,
//                           CGO_ENABLED=0) for every remote target: linux-x64,
//                           darwin-arm64, windows-x64 (cmd/lectureflow/ssh.go reads
//                           resources/agent); agent/kit.json has the version and the
//                           SHA-256 of each as built (electron-builder signs the macOS
//                           app's Mach-O files again, the darwin-arm64 one included,
//                           so there the shipped bytes differ: the service always
//                           hashes the file it uploads)
//   bin/ffmpeg, ffprobe     LGPL build from pinned sources (packaging/third-party.json;
//                           Windows: scripts/build-ffmpeg-windows.sh, macOS:
//                           scripts/build-ffmpeg-macos.sh), configure line and ffmpeg -L checked
//   bin/uv                  pinned release
//   bin/aria2c(.exe)        aria2 1.37.0 (SPEC-ext, ext-brief §6.1): Windows the pinned
//                           official build (its SHA-256 is also in components.json, core
//                           component), macOS built from the pinned source by
//                           packaging/aria2c/build-aria2c-macos.sh
//   worker/*.py             the runtime scripts, no tests
//   components.json, locks/ packaging/components/components.json and its lock files
//   sample/                 assets/sample (sample lecture, reference text, licence)
//   LICENSE, LICENSE-EXCEPTION.md, THIRD_PARTY_NOTICES.md, licenses/{ffmpeg,uv,aria2}/
//                           (licenses/{ffmpeg,aria2}/SOURCE.txt: the corresponding source is
//                           the release attachment of scripts/sources.js)
//
// macOS: every Mach-O file in resources/ must run on the app's minimum macOS
// (third-party.json macos.minimum_system_version, = electron-builder's
// mac.minimumSystemVersion): its LC_BUILD_VERSION (or LC_VERSION_MIN_MACOSX)
// minos is not newer, and it has an arm64 slice. Otherwise staging fails.
//
// Usage: node scripts/stage.js [--platform win32|darwin]
'use strict';

const { execFileSync, spawnSync } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { fetchPinned, extract, globFiles, sha256File } = require('./lib/fetch');
const { assetName, assetURL, releaseRepo, releaseURL } = require('./sources');

const DESKTOP = path.resolve(__dirname, '..');
const REPO = path.resolve(DESKTOP, '..');
const CACHE = path.join(DESKTOP, '.cache');
const OUT = path.join(DESKTOP, 'build', 'resources');
const MANIFEST = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'packaging', 'third-party.json'), 'utf8'));
const APP_VERSION = JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8')).version;

// Python scripts the service runs (no tests, no conftest). colab_remote.py
// is uploaded to the Colab runtime with asr.py, asr_common.py and
// textrepair.py (internal/colab/backend.go workerFiles); it was optional
// until the Colab backend shipped (3.2.0), now a missing one fails staging.
const WORKER_FILES = ['asr.py', 'asr_common.py', 'asr_openvino.py', 'asr_mlx.py', 'textrepair.py', 'extract_pdf.py', 'colab_remote.py'];
const SAMPLE_FILES = ['sample-lecture.flac', 'reference.txt', 'LICENSE', 'SOURCE.md'];

function targetKey(platform) {
  if (platform === 'win32') return 'windows-x64';
  if (platform === 'darwin') return 'darwin-arm64';
  throw new Error(`unsupported target platform ${platform} (SPEC-v3 V0: Windows x64 and macOS arm64)`);
}

function copy(src, dest) {
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.copyFileSync(src, dest);
}

/**
 * gplFree(expr) → true when an SPDX expression can be met without GPL terms
 * (SPEC-v3 V7: the distributed ffmpeg contains no GPL component). LGPL is
 * fine; "X OR GPL-…" is fine through X; the GCC Runtime Library Exception
 * exempts the compiled program from the GPL.
 */
function gplFree(expr) {
  return String(expr).split(/\s+OR\s+/).some((alt) => alt.replace(/[()]/g, '').split(/\s+AND\s+/)
    .every((term) => !/^A?GPL-/.test(term.trim()) || /\sWITH\s+GCC-exception-3\.1$/.test(term.trim())));
}

/** gplComponents(binaries) → "binary / library [license]" of every ffmpeg part that needs the GPL. */
function gplComponents(binaries) {
  const out = [];
  for (const b of binaries.filter((x) => /^ffmpeg/i.test(x.name))) {
    if (!gplFree(b.license)) out.push(`${b.name} [${b.license}]`);
    for (const l of b.bundled || []) if (!gplFree(l.license)) out.push(`${b.name} / ${l.name} [${l.license}]`);
  }
  return out;
}

/** checkLicenseText(text) → true when ffmpeg -L names the GNU LGPL and no GPL-only terms. */
function checkLicenseText(text) {
  const t = String(text).replace(/\s+/g, ' ');
  return /GNU Lesser General Public License/.test(t) && !/version 3 of the License/.test(t) && !/GNU General Public License as published/.test(t);
}

/**
 * buildWindowsFfmpeg(spec): runs scripts/build-ffmpeg-windows.sh, directly
 * on Linux, through Docker (spec.docker_image, pinned by digest) elsewhere.
 */
function buildWindowsFfmpeg(spec) {
  const script = path.join(__dirname, 'build-ffmpeg-windows.sh');
  if (process.platform === 'linux') {
    execFileSync('bash', [script], { cwd: DESKTOP, stdio: 'inherit' });
    return;
  }
  const docker = spawnSync('docker', ['version', '--format', '{{.Server.Os}}'], { encoding: 'utf8' });
  if (docker.status !== 0 || docker.stdout.trim() !== 'linux') {
    throw new Error('the Windows ffmpeg is built from source on Linux (scripts/build-ffmpeg-windows.sh); start Docker with Linux containers, or put a finished build in desktop/.cache/ffmpeg-windows-<version>');
  }
  execFileSync('docker', ['run', '--rm', '-v', `${DESKTOP}:/src`, '-w', '/src', '-e', 'FFWIN_WORK=/tmp/ffwin', spec.docker_image,
    'bash', 'scripts/build-ffmpeg-windows.sh', '--install-deps'], { stdio: 'inherit', env: { ...process.env, MSYS_NO_PATHCONV: '1' } });
}

/** checkConfigure(line, forbidden) → the forbidden flags present in an ffmpeg configure line. */
function checkConfigure(line, forbidden) {
  const flags = String(line).split(/\s+/);
  return forbidden.filter((f) => flags.includes(f));
}

// The Go programs in resources/bin: the server and the Native Messaging host
// that browsers start for the extension (ext-brief §4). Both are pure Go
// (CGO_ENABLED=0), cross-compiled for the target.
const GO_PROGRAMS = [
  { name: 'lectureflow', pkg: './cmd/lectureflow' },
  { name: 'lectureflow-nmhost', pkg: './cmd/lectureflow-nmhost' },
];

// ssh-1b: the agent of the SSH inference servers, one static build per
// remote target (resources/agent/<target>/lf-agent; internal/remote
// Platforms), whatever platform the installer is for: a Windows or macOS
// app may drive a Linux, Mac or Windows server. About 7 MB each.
const AGENT_PKG = './cmd/lf-agent';
const AGENT_TARGETS = [
  { target: 'linux-x64', goos: 'linux', goarch: 'amd64' },
  { target: 'darwin-arm64', goos: 'darwin', goarch: 'arm64' },
  { target: 'windows-x64', goos: 'windows', goarch: 'amd64' },
];

// The installers leave lf-agent out while the SSH servers are experimental
// (LECTUREFLOW_EXPERIMENTAL_SSH; user decision 2026-11-11): three cross
// builds of about 7 MB each and their build time for a feature that is off.
// Without them a switched-on service says so (remote.agent_missing);
// development builds use make.ps1 agent (tools/lf-agent). When the feature
// becomes public, set BUNDLE_AGENT_DEFAULT to true (the one place); the
// environment variable LECTUREFLOW_BUNDLE_LF_AGENT=1 / 0 overrides it for
// one build.
const BUNDLE_AGENT_ENV = 'LECTUREFLOW_BUNDLE_LF_AGENT';
const BUNDLE_AGENT_DEFAULT = false;

/** bundleAgent(env) → whether stage builds resources/agent. */
function bundleAgent(env = process.env) {
  const v = env[BUNDLE_AGENT_ENV];
  if (v === '1') return true;
  if (v === '0') return false;
  if (v !== undefined && v !== '') throw new Error(`${BUNDLE_AGENT_ENV} must be 1 or 0, not ${JSON.stringify(v)}`);
  return BUNDLE_AGENT_DEFAULT;
}

/** agentBuildArgs(version, out) → the go build arguments of one agent (as make.ps1 agent). */
function agentBuildArgs(version, out) {
  return ['build', '-trimpath', '-ldflags', `-s -w -X lectureflow/internal/agent.Version=${version}`, '-o', out, AGENT_PKG];
}

/**
 * stageAgent(version, {out, build}) builds every agent target into
 * out/agent/<target>/lf-agent and writes out/agent/kit.json {version,
 * builds: {target: sha256}}; returns that object. build(target, file) is
 * replaced in tests (default: go build, CGO_ENABLED=0, cross-compiled).
 */
function stageAgent(version, { out = OUT, build } = {}) {
  const dir = path.join(out, 'agent');
  const kit = { version, builds: {} };
  for (const t of AGENT_TARGETS) {
    const file = path.join(dir, t.target, 'lf-agent');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    if (build) build(t, file);
    else {
      const env = { ...process.env, CGO_ENABLED: '0', GOOS: t.goos, GOARCH: t.goarch };
      console.log(`go build ${AGENT_PKG} ${t.goos}/${t.goarch} version ${version}`);
      execFileSync('go', agentBuildArgs(version, file), { cwd: REPO, env, stdio: 'inherit' });
    }
    if (!fs.existsSync(file)) throw new Error(`lf-agent for ${t.target} was not built`);
    if (t.goos !== 'windows') fs.chmodSync(file, 0o755);
    kit.builds[t.target] = sha256File(file);
  }
  fs.writeFileSync(path.join(dir, 'kit.json'), `${JSON.stringify(kit, null, 2)}\n`);
  return kit;
}

function goBuild(platform, version, { name, pkg } = GO_PROGRAMS[0]) {
  const exe = platform === 'win32' ? `${name}.exe` : name;
  const out = path.join(OUT, 'bin', exe);
  if (!fs.existsSync(path.join(REPO, ...pkg.split('/')))) throw new Error(`${pkg} is missing: the installer needs ${exe}`);
  const env = { ...process.env, CGO_ENABLED: '0', GOOS: platform === 'win32' ? 'windows' : 'darwin', GOARCH: platform === 'win32' ? 'amd64' : 'arm64' };
  console.log(`go build ${pkg} ${env.GOOS}/${env.GOARCH} version ${version}`);
  execFileSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.version=${version}`, '-o', out, pkg], { cwd: REPO, env, stdio: 'inherit' });
  return out;
}

/**
 * sourceSection(program, key, version, repo) → the "Corresponding source
 * code" lines of SOURCE.txt: the release attachment (scripts/sources.js),
 * then the upstream locations of the same tarballs and what is not
 * attached, with the reason.
 */
function sourceSection(program, key, version, repo) {
  const spec = MANIFEST[program][key];
  const id = `${program}-${key}`;
  const script = (spec.build_scripts || []).find((s) => s.endsWith('.sh'));
  const out = [
    'Corresponding source code:',
    `attached to the LectureFlow ${version} release, next to the installers, as ${assetName(version, id)}`,
    `  ${assetURL(version, id, repo)}`,
    `  (release page: ${releaseURL(version, repo)})`,
    'It holds the upstream source tarballs listed below, unchanged and checked by SHA-256,',
    `${script ? `the build script (${script}), ` : ''}LectureFlow's pins (desktop/packaging/third-party.json) and a README.txt on how this binary was built.`,
    '',
    'Upstream locations of the same tarballs:',
    ...spec.source_code.filter((s) => !s.release_asset && !s.not_attached).map((s) => `- ${s.what}: ${s.url}`),
  ];
  const left = spec.source_code.filter((s) => s.not_attached);
  if (left.length) out.push('', 'Not attached (no source obligation):', ...left.flatMap((s) => [`- ${s.what}: ${s.url}`, `  ${s.not_attached}`]));
  return out;
}

/** ffmpegSourceText(key, version, repo) → resources/licenses/ffmpeg/SOURCE.txt (LGPL: where the corresponding source is). */
function ffmpegSourceText(key, version = APP_VERSION, repo = releaseRepo()) {
  const f = MANIFEST.ffmpeg;
  const spec = f[key];
  const win = key === 'windows-x64';
  const out = [
    win
      ? `FFmpeg ${f.version}: resources/bin/ffmpeg.exe, ffprobe.exe and the FFmpeg DLLs next to them (av*.dll, sw*.dll) are an LGPL build of FFmpeg ${f.version}. The DLLs can be replaced with your own build of the same FFmpeg version.`
      : `FFmpeg ${f.version}: resources/bin/ffmpeg and ffprobe are an LGPL build of FFmpeg ${f.version} for macOS ${MANIFEST.macos.minimum_system_version} or later (static executables: to change FFmpeg, rebuild them from the source below and replace both files).`,
    `License: ${f.license} (COPYING.LGPLv2.1, LICENSE.md). The libraries linked in are listed in THIRD_PARTY_NOTICES.md; their license texts are in this directory. CONFIGURE.txt has the configure flags, BUILDINFO.txt the build environment.`,
    '',
    ...sourceSection('ffmpeg', key, version, repo),
    '',
    `Build: ${spec.build}.`,
  ];
  if (spec.toolchain) out.push('', 'Toolchain:', ...Object.entries(spec.toolchain).map(([k, v]) => `- ${k} ${v}`));
  out.push('');
  return out.join('\n');
}

/**
 * cachedBuildProblems(root, program, key, manifest) → why a build in
 * desktop/.cache (ffmpeg-windows-*, ffmpeg-macos-*, aria2c-macos-*; in CI
 * restored from the Actions cache or the ffmpeg-windows artifact) does not
 * belong to the pins in third-party.json. The build scripts write
 * BUILDINFO.txt: its first line names program, version and platform, and
 * the macOS scripts record the SHA-256 of the source tarball (and of
 * ffmpeg's dependencies) they built from; each must be the pinned one.
 *
 * This is a consistency check, not a tamper check: the repository pins the
 * sources, not the built binaries (no expected SHA-256 of them exists), and
 * BUILDINFO.txt lives in the same cache as the binaries. stage logs the
 * SHA-256 of every cached binary it ships (stage-info.json) so a release
 * can be compared with another build.
 */
function cachedBuildProblems(root, program, key, manifest = MANIFEST) {
  const file = path.join(root, 'BUILDINFO.txt');
  if (!fs.existsSync(file)) return [`${file} is missing`];
  const lines = fs.readFileSync(file, 'utf8').split(/\r?\n/);
  const m = manifest[program];
  const spec = m[key];
  const platform = key === 'windows-x64' ? 'Windows x64' : 'macOS arm64';
  const header = `${program === 'ffmpeg' ? 'FFmpeg' : 'aria2'} ${m.version} for ${platform},`;
  const problems = [];
  if (!lines[0].startsWith(header)) problems.push(`BUILDINFO.txt starts "${lines[0]}", expected "${header} …"`);
  if (key === 'darwin-arm64') {
    if (!lines.includes(`source sha256: ${spec.sha256}`)) problems.push(`BUILDINFO.txt does not name the pinned source (sha256 ${spec.sha256})`);
    for (const d of spec.deps || []) {
      if (program === 'ffmpeg' && !lines.some((l) => l.startsWith(`dependency: ${d.name} ${d.version} `) && l.includes(` sha256 ${d.sha256} `))) {
        problems.push(`BUILDINFO.txt does not name the pinned ${d.name} ${d.version} (sha256 ${d.sha256})`);
      }
    }
  }
  return problems;
}

/**
 * reuseCachedBuild(root, bin, program, key) → true when root holds a build
 * (root/bin exists) that cachedBuildProblems accepts. A build it refuses is
 * deleted, so the caller builds it again from the pinned sources.
 */
function reuseCachedBuild(root, bin, program, key, manifest = MANIFEST) {
  if (!fs.existsSync(path.join(root, bin))) return false;
  const problems = cachedBuildProblems(root, program, key, manifest);
  if (!problems.length) return true;
  console.log(`discarding the cached ${program} build ${root}: ${problems.join('; ')}`);
  fs.rmSync(root, { recursive: true, force: true });
  return false;
}

/** checkFreshBuild(root, program, key): a build just made must pass cachedBuildProblems too. */
function checkFreshBuild(root, program, key) {
  const problems = cachedBuildProblems(root, program, key);
  if (problems.length) throw new Error(`${program} build in ${root}: ${problems.join('; ')}`);
}

async function stageFfmpeg(platform, key, version = APP_VERSION) {
  const spec = MANIFEST.ffmpeg[key];
  let root;
  if (platform === 'win32') {
    // Built from the pinned sources by scripts/build-ffmpeg-windows.sh
    // (Linux + mingw-w64; on Windows through Docker, CI: an ubuntu job).
    root = path.join(CACHE, `ffmpeg-windows-${MANIFEST.ffmpeg.version}`);
    if (!reuseCachedBuild(root, path.join('bin', 'ffmpeg.exe'), 'ffmpeg', key)) {
      buildWindowsFfmpeg(spec);
      checkFreshBuild(root, 'ffmpeg', key);
    }
  } else {
    // Built from the pinned source by scripts/build-ffmpeg-macos.sh (CI).
    root = path.join(CACHE, `ffmpeg-macos-${MANIFEST.ffmpeg.version}`);
    if (!reuseCachedBuild(root, path.join('bin', 'ffmpeg'), 'ffmpeg', key)) {
      if (process.platform !== 'darwin') throw new Error('the macOS ffmpeg is built on macOS (scripts/build-ffmpeg-macos.sh)');
      execFileSync('bash', [path.join(__dirname, 'build-ffmpeg-macos.sh')], { cwd: DESKTOP, stdio: 'inherit' });
      checkFreshBuild(root, 'ffmpeg', key);
    }
  }
  let n = 0;
  const binaries = {};
  for (const pattern of spec.bin) {
    for (const f of globFiles(root, pattern)) {
      copy(f, path.join(OUT, 'bin', path.basename(f)));
      binaries[path.basename(f)] = sha256File(f);
      n++;
    }
  }
  if (n < 2) throw new Error(`ffmpeg: expected ffmpeg and ffprobe in ${root}`);
  console.log(`ffmpeg ${MANIFEST.ffmpeg.version} from ${root}: ${Object.entries(binaries).map(([b, s]) => `${b} ${s}`).join(', ')}`);
  for (const lic of spec.licenses) {
    for (const f of globFiles(root, lic)) copy(f, path.join(OUT, 'licenses', 'ffmpeg', path.basename(f)));
  }
  if (fs.existsSync(path.join(root, 'BUILDINFO.txt'))) copy(path.join(root, 'BUILDINFO.txt'), path.join(OUT, 'licenses', 'ffmpeg', 'BUILDINFO.txt'));
  // The configure line must not enable GPL or non-free parts (SPEC-v3 V7).
  const ffmpeg = path.join(OUT, 'bin', platform === 'win32' ? 'ffmpeg.exe' : 'ffmpeg');
  let configure = spec.configure ? spec.configure.join(' ') : '';
  if (process.platform === platform) {
    const r = spawnSync(ffmpeg, ['-hide_banner', '-buildconf'], { encoding: 'utf8' });
    if (r.status !== 0) throw new Error(`ffmpeg -buildconf failed: ${r.stderr}`);
    configure = r.stdout.split(/\r?\n/).map((l) => l.trim()).filter((l) => l.startsWith('--')).join(' ');
  }
  const bad = checkConfigure(configure, MANIFEST.ffmpeg.forbidden_configure);
  if (bad.length) throw new Error(`ffmpeg build is not LGPL-only: ${bad.join(' ')}`);
  const gpl = gplComponents(JSON.parse(fs.readFileSync(path.join(REPO, 'packaging', 'licenses', 'binaries.json'), 'utf8')).binaries);
  if (gpl.length) throw new Error(`ffmpeg declares GPL components (packaging/licenses/binaries.json): ${gpl.join('; ')}`);
  const checks = ['configure line without --enable-gpl / --enable-nonfree / --enable-version3', 'no GPL library in binaries.json bundled'];
  if (process.platform === platform) {
    // The license the binary reports (ffmpeg -L) must be the LGPL (V7).
    const r = spawnSync(ffmpeg, ['-hide_banner', '-L'], { encoding: 'utf8' });
    if (r.status !== 0 || !checkLicenseText(r.stdout)) throw new Error(`ffmpeg -L does not report the LGPL: ${r.stdout}`);
    checks.push('ffmpeg -L: GNU LGPL version 2.1 or later');
  }
  console.log(`ffmpeg license checks passed: ${checks.join('; ')}`);
  fs.mkdirSync(path.join(OUT, 'licenses', 'ffmpeg'), { recursive: true });
  fs.writeFileSync(path.join(OUT, 'licenses', 'ffmpeg', 'CONFIGURE.txt'),
    `FFmpeg ${MANIFEST.ffmpeg.version} (${spec.build || spec.url}), LGPL build. Configure flags:\n${configure.split(' ').join('\n')}\n`);
  fs.writeFileSync(path.join(OUT, 'licenses', 'ffmpeg', 'SOURCE.txt'), ffmpegSourceText(key, version));
  return { configure, licenseChecks: checks, binaries };
}

async function stageUv(platform, key) {
  const spec = MANIFEST.uv[key];
  const archive = await fetchPinned(spec, path.join(CACHE, 'downloads'));
  const dir = path.join(CACHE, 'extract', `uv-${spec.sha256.slice(0, 12)}`);
  if (!fs.existsSync(dir)) extract(archive, dir);
  for (const b of spec.bin) {
    const f = path.join(dir, spec.root || '', b);
    if (!fs.existsSync(f)) throw new Error(`uv: ${b} missing in the archive`);
    copy(f, path.join(OUT, 'bin', path.basename(f)));
    if (platform !== 'win32') fs.chmodSync(path.join(OUT, 'bin', path.basename(f)), 0o755);
  }
  for (const lic of MANIFEST.uv.license_texts) {
    const f = await fetchPinned(lic, path.join(CACHE, 'downloads'));
    copy(f, path.join(OUT, 'licenses', 'uv', lic.name));
  }
}

/**
 * checkAria2cVersion(text, version, key) → problems with the output of
 * aria2c --version: the pinned version, HTTPS, never OpenSSL or GnuTLS
 * (license_note); on macOS AppleTLS and none of the libraries and features
 * build-aria2c-macos.sh turns off.
 */
function checkAria2cVersion(text, version, key) {
  const lines = String(text).split(/\r?\n/);
  const field = (name) => (lines.find((l) => l.startsWith(`${name}:`)) || '').slice(name.length + 1).trim();
  const libs = field('Libraries');
  const features = field('Enabled Features').split(/,\s*/).filter(Boolean);
  const problems = [];
  if (lines[0].trim() !== `aria2 version ${version}`) problems.push(`version line "${lines[0].trim()}", expected "aria2 version ${version}"`);
  if (!features.includes('HTTPS')) problems.push(`HTTPS is not enabled (features: ${features.join(', ') || 'none'})`);
  if (/OpenSSL|GnuTLS/i.test(libs)) problems.push(`links OpenSSL or GnuTLS: ${libs}`);
  if (key === 'darwin-arm64') {
    if (!/\bAppleTLS\b/.test(libs)) problems.push(`AppleTLS missing: ${libs || 'no libraries'}`);
    const bad = libs.split(/\s+/).filter((l) => /^(nettle|GMP|libgcrypt|libxml2|expat|sqlite3|libssh2|c-ares)\b/i.test(l));
    if (bad.length) problems.push(`libraries the macOS build turns off: ${bad.join(' ')}`);
    const off = features.filter((f) => ['BitTorrent', 'Metalink', 'SFTP', 'Firefox3 Cookie', 'XML-RPC', 'Async DNS'].includes(f));
    if (off.length) problems.push(`features the macOS build turns off: ${off.join(', ')}`);
  }
  return problems;
}

/** aria2cSourceText(key, version, repo) → resources/licenses/aria2/SOURCE.txt (GPL: where the corresponding source is). */
function aria2cSourceText(key, version = APP_VERSION, repo = releaseRepo()) {
  const a = MANIFEST.aria2c;
  const spec = a[key];
  const out = [
    spec.bin_sha256
      ? `aria2 ${a.version}: resources/bin/aria2c.exe is aria2's official Windows build, unchanged (SHA-256 ${spec.bin_sha256}).`
      : `aria2 ${a.version}: resources/bin/aria2c is built from the aria2 source (BUILDINFO.txt) and signed with the application.`,
    `License: ${a.license}. ${a.license_note}`,
    '',
    ...sourceSection('aria2c', key, version, repo),
    '',
    `Build: ${spec.build}.`,
  ];
  if (spec.configure) {
    out.push('', `Built by packaging/aria2c/build-aria2c-macos.sh of the LectureFlow source (https://github.com/${repo})`,
      `for macOS ${MANIFEST.macos.minimum_system_version} or later with these configure flags (BUILDINFO.txt has the compiler and SDK):`,
      ...spec.configure.map((f) => `  ${f}`));
  }
  if (spec.toolchain) out.push('', 'Toolchain:', ...Object.entries(spec.toolchain).map(([k, v]) => `- ${k} ${v}`));
  out.push('', 'License texts: COPYING (GNU GPL version 2), LICENSE.OpenSSL (the OpenSSL linking permission), thirdparty/ (the libraries linked in).', '');
  return out.join('\n');
}

/**
 * extractOnce(archive, dir, members): unpacks the archive (only members,
 * when given) into dir unless dir exists. It unpacks into dir.partial and
 * renames that when complete, so an interrupted run never leaves a dir
 * that looks finished.
 */
function extractOnce(archive, dir, members = []) {
  if (fs.existsSync(dir)) return;
  const part = `${dir}.partial`;
  fs.rmSync(part, { recursive: true, force: true });
  fs.mkdirSync(part, { recursive: true });
  const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  execFileSync(tar, ['-xf', archive, '-C', part, ...members], { stdio: 'inherit' });
  fs.renameSync(part, dir);
}

async function stageAria2c(platform, key, version = APP_VERSION) {
  const a = MANIFEST.aria2c;
  const spec = a[key];
  const downloads = path.join(CACHE, 'downloads');
  const exe = platform === 'win32' ? 'aria2c.exe' : 'aria2c';
  const licDir = path.join(OUT, 'licenses', 'aria2');
  let root;
  if (platform === 'win32') {
    // aria2's own Windows build, pinned by the SHA-256 of the zip and of aria2c.exe.
    const archive = await fetchPinned(spec, downloads);
    const dir = path.join(CACHE, 'extract', `aria2-${spec.sha256.slice(0, 12)}`);
    extractOnce(archive, dir);
    root = path.join(dir, spec.root);
  } else {
    // No upstream macOS binary: built from the pinned source (CI: macos-14, cached).
    root = path.join(CACHE, `aria2c-macos-${a.version}`);
    if (!reuseCachedBuild(root, spec.bin, 'aria2c', key)) {
      if (process.platform !== 'darwin') throw new Error('the macOS aria2c is built on macOS (packaging/aria2c/build-aria2c-macos.sh)');
      execFileSync('bash', [path.join(REPO, 'packaging', 'aria2c', 'build-aria2c-macos.sh')], { cwd: REPO, stdio: 'inherit' });
      checkFreshBuild(root, 'aria2c', key);
    }
  }
  const src = path.join(root, spec.bin);
  if (!fs.existsSync(src)) throw new Error(`aria2c: ${spec.bin} missing in ${root}`);
  const dest = path.join(OUT, 'bin', exe);
  copy(src, dest);
  if (platform !== 'win32') fs.chmodSync(dest, 0o755);
  const sha256 = sha256File(dest);
  const size = fs.statSync(dest).size;
  if (spec.bin_sha256 && (sha256 !== spec.bin_sha256 || size !== spec.bin_size)) {
    throw new Error(`aria2c: ${exe} has SHA-256 ${sha256} (${size} bytes), third-party.json pins ${spec.bin_sha256} (${spec.bin_size} bytes)`);
  }
  for (const pattern of spec.licenses) {
    const files = globFiles(root, pattern);
    if (!files.length && !pattern.includes('*')) throw new Error(`aria2c: ${pattern} missing in ${root}`);
    for (const f of files) copy(f, path.join(licDir, path.relative(root, f)));
  }
  // Windows: the license texts of the libraries aria2's build linked in,
  // from their pinned source tarballs, and of the toolchain runtimes.
  for (const d of spec.deps || []) {
    if (!d.licenses.length) continue;
    const archive = await fetchPinned(d, downloads);
    const members = d.licenses.map((l) => `${d.root}/${l}`);
    // Keyed by the archive and the members: the aria2 source gives wslay's license.
    const dir = path.join(CACHE, 'extract', `aria2-lic-${crypto.createHash('sha256').update(`${d.sha256} ${members.join(' ')}`).digest('hex').slice(0, 12)}`);
    extractOnce(archive, dir, members);
    for (const l of d.licenses) copy(path.join(dir, d.root, l), path.join(licDir, 'thirdparty', `${d.name}-${path.basename(l)}.txt`));
  }
  for (const t of spec.license_texts || []) {
    const f = await fetchPinned(t, downloads);
    copy(f, path.join(licDir, 'thirdparty', t.name));
  }
  fs.writeFileSync(path.join(licDir, 'SOURCE.txt'), aria2cSourceText(key, version));
  const checks = [spec.bin_sha256 ? 'SHA-256 of the official build' : 'built from the pinned source'];
  if (process.platform === platform) {
    const r = spawnSync(dest, ['--version'], { encoding: 'utf8' });
    if (r.status !== 0) throw new Error(`aria2c --version failed: ${r.stderr || r.error}`);
    const problems = checkAria2cVersion(r.stdout, a.version, key);
    if (problems.length) throw new Error(`aria2c --version: ${problems.join('; ')}`);
    checks.push(`aria2c --version: ${a.version}, HTTPS${key === 'darwin-arm64' ? ' (AppleTLS)' : ''}, no OpenSSL`);
  }
  console.log(`aria2c ${a.version}: ${sha256} (${checks.join('; ')})`);
  return { version: a.version, sha256, size, checks };
}

/**
 * bundledFileProblems(manifest, key, file) → why the shipped
 * components.json disagrees with a staged binary ({name, sha256, size,
 * required}). The core component lists the bundled third-party binaries
 * whose SHA-256 is known before packaging (Windows: aria2's official
 * aria2c.exe, {name: "bin/aria2c.exe", url: "", sha256, size}); the
 * diagnostics compare the file in resources/ with that entry. macOS has no
 * entry: aria2c is built in CI, and electron-builder signs every Mach-O in
 * the app again, which changes its bytes (ad hoc as well as with a
 * Developer ID), so the diagnostics show the file's own SHA-256 there.
 */
function bundledFileProblems(manifest, key, file) {
  const core = ((manifest && manifest.components) || []).find((c) => c.id === 'core' && c.bundled);
  if (!core) return ['components.json has no bundled core component'];
  const b = (core.platforms || {})[key];
  const entry = b && (b.files || []).find((f) => f.name === file.name);
  if (!entry) return file.required ? [`the core component records no ${file.name} for ${key}`] : [];
  const problems = [];
  if (entry.sha256 !== file.sha256 || entry.size !== file.size) {
    problems.push(`the core component records ${file.name} for ${key} as ${entry.sha256} (${entry.size} bytes), the staged file is ${file.sha256} (${file.size} bytes)`);
  }
  if (entry.url !== '') problems.push(`${file.name}: a bundled file has no download URL`);
  return problems;
}

// --- minimum macOS of the Mach-O files (third-party.json macos) -------------------

const MH_MAGIC = 0xfeedface;
const MH_MAGIC_64 = 0xfeedfacf;
const FAT_MAGIC = 0xcafebabe;
const FAT_MAGIC_64 = 0xcafebabf;
const LC_VERSION_MIN_MACOSX = 0x24;
const LC_BUILD_VERSION = 0x32;
const PLATFORM_MACOS = 1;
const CPU_TYPE_ARM64 = 0x0100000c;

/** machoVersion(v) → "13.0" or "13.0.1" for a Mach-O version (xxxx.yy.zz nibbles). */
function machoVersion(v) {
  const s = `${v >>> 16}.${(v >>> 8) & 0xff}`;
  return v & 0xff ? `${s}.${v & 0xff}` : s;
}

/** compareVersions("13.0", "12.6.1") → 1; numeric, missing parts are 0. */
function compareVersions(a, b) {
  const x = String(a).split('.').map(Number);
  const y = String(b).split('.').map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] || 0) - (y[i] || 0);
    if (d) return Math.sign(d);
  }
  return 0;
}

/** machoSlice(read, off) → {cpu, platform, minos} of the thin Mach-O at off, or null. */
function machoSlice(read, off) {
  const h = read(off, 32);
  if (h.length < 28) return null;
  const magic = h.readUInt32LE(0);
  if (magic !== MH_MAGIC && magic !== MH_MAGIC_64) return null;
  const headerSize = magic === MH_MAGIC_64 ? 32 : 28;
  const ncmds = h.readUInt32LE(16);
  const cmds = read(off + headerSize, Math.min(h.readUInt32LE(20), 1 << 24));
  const slice = { cpu: h.readInt32LE(4), platform: null, minos: null };
  for (let i = 0, p = 0; i < ncmds && p + 8 <= cmds.length; i++) {
    const cmd = cmds.readUInt32LE(p);
    const size = cmds.readUInt32LE(p + 4);
    if (cmd === LC_BUILD_VERSION && p + 16 <= cmds.length) {
      slice.platform = cmds.readUInt32LE(p + 8);
      slice.minos = machoVersion(cmds.readUInt32LE(p + 12));
    } else if (cmd === LC_VERSION_MIN_MACOSX && p + 12 <= cmds.length && slice.minos === null) {
      slice.platform = PLATFORM_MACOS;
      slice.minos = machoVersion(cmds.readUInt32LE(p + 8));
    }
    if (size < 8) break;
    p += size;
  }
  return slice;
}

/**
 * machoSlices(read) → the slices of a Mach-O file ({cpu, platform, minos}
 * each; a universal file has several), or null when it is not one.
 * read(offset, length) → Buffer (shorter at the end of the file).
 */
function machoSlices(read) {
  const h = read(0, 8);
  if (h.length < 8) return null;
  const magic = h.readUInt32BE(0);
  if (magic === FAT_MAGIC || magic === FAT_MAGIC_64) {
    const n = h.readUInt32BE(4);
    if (n === 0 || n > 32) return null; // Java class files start with CAFEBABE too
    const size = magic === FAT_MAGIC ? 20 : 32;
    const table = read(8, n * size);
    const slices = [];
    for (let i = 0; i < n; i++) {
      const offset = magic === FAT_MAGIC ? table.readUInt32BE(i * size + 8) : Number(table.readBigUInt64BE(i * size + 8));
      slices.push(machoSlice(read, offset) || { cpu: table.readInt32BE(i * size), platform: null, minos: null });
    }
    return slices;
  }
  const s = machoSlice(read, 0);
  return s && [s];
}

function machoSlicesOfFile(file) {
  const fd = fs.openSync(file, 'r');
  try {
    return machoSlices((off, len) => {
      const b = Buffer.alloc(len);
      return b.subarray(0, fs.readSync(fd, b, 0, len, off));
    });
  } finally {
    fs.closeSync(fd);
  }
}

/**
 * macMinimumCheck(dir, minimum, key) → {files: [{path, minos}], problems}:
 * every Mach-O file under dir must be built for macOS, at most for the
 * minimum macOS the app declares, and (darwin-arm64) have an arm64 slice.
 * The darwin-arm64 resources must contain Mach-O files at all (the Go
 * programs), so a check that sees none has looked in the wrong place.
 * Reads the load commands itself: otool cannot open files whose names end
 * in "(…)", which it takes for archive members.
 */
function macMinimumCheck(dir, minimum, key) {
  const files = [];
  const problems = [];
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile()) {
        const slices = machoSlicesOfFile(p);
        if (!slices) continue;
        const rel = path.relative(dir, p).split(path.sep).join('/');
        files.push({ path: rel, minos: slices.map((s) => s.minos).join(', ') });
        for (const s of slices) {
          if (s.minos === null) problems.push(`${rel}: records no minimum macOS (LC_BUILD_VERSION)`);
          else if (s.platform !== PLATFORM_MACOS) problems.push(`${rel}: built for platform ${s.platform}, not macOS`);
          else if (compareVersions(s.minos, minimum) > 0) problems.push(`${rel} needs macOS ${s.minos}, the app declares ${minimum} (build it with MACOSX_DEPLOYMENT_TARGET=${minimum}, or raise macos.minimum_system_version)`);
        }
        if (key === 'darwin-arm64' && !slices.some((s) => s.cpu === CPU_TYPE_ARM64)) problems.push(`${rel}: no arm64 code`);
      }
    }
  };
  walk(dir);
  if (key === 'darwin-arm64' && !files.length) problems.push(`no Mach-O file found in ${dir}`);
  return { files: files.sort((a, b) => a.path.localeCompare(b.path)), problems };
}

function stageFiles() {
  for (const f of WORKER_FILES) copy(path.join(REPO, 'worker', f), path.join(OUT, 'worker', f));
  copy(path.join(REPO, 'packaging', 'components', 'components.json'), path.join(OUT, 'components.json'));
  // The components manager looks for the lock files next to components.json.
  const locks = path.join(REPO, 'packaging', 'components', 'locks');
  for (const f of fs.readdirSync(locks)) {
    if (fs.statSync(path.join(locks, f)).isFile()) copy(path.join(locks, f), path.join(OUT, 'locks', f));
  }
  for (const f of SAMPLE_FILES) copy(path.join(REPO, 'assets', 'sample', f), path.join(OUT, 'sample', f));
  copy(path.join(REPO, 'LICENSE'), path.join(OUT, 'LICENSE'));
  copy(path.join(REPO, 'THIRD_PARTY_NOTICES.md'), path.join(OUT, 'THIRD_PARTY_NOTICES.md'));
  copy(path.join(REPO, 'LICENSE-EXCEPTION.md'), path.join(OUT, 'LICENSE-EXCEPTION.md'));
}

async function stage({ platform = process.platform, version, env = process.env } = {}) {
  const key = targetKey(platform);
  version = version || JSON.parse(fs.readFileSync(path.join(DESKTOP, 'package.json'), 'utf8')).version;
  const withAgent = bundleAgent(env); // before anything is built: a bad value fails at once
  fs.rmSync(OUT, { recursive: true, force: true });
  fs.mkdirSync(OUT, { recursive: true });
  const [server, nmhost] = GO_PROGRAMS.map((p) => goBuild(platform, version, p));
  // null = not bundled (stage-info.json says so); OUT was emptied above, so
  // no agent of an earlier build is left in resources/.
  const agent = withAgent ? stageAgent(version) : null;
  if (!agent) console.log(`lf-agent: not bundled (${BUNDLE_AGENT_ENV}=1 to include it)`);
  const ff = await stageFfmpeg(platform, key, version);
  await stageUv(platform, key);
  const aria2c = await stageAria2c(platform, key, version);
  stageFiles();
  // The diagnostics compare aria2c with the SHA-256 the core component
  // records (Windows); the shipped manifest must agree with the staged file.
  const manifest = JSON.parse(fs.readFileSync(path.join(OUT, 'components.json'), 'utf8'));
  const bad = bundledFileProblems(manifest, key, {
    name: `bin/${platform === 'win32' ? 'aria2c.exe' : 'aria2c'}`, sha256: aria2c.sha256, size: aria2c.size, required: Boolean(MANIFEST.aria2c[key].bin_sha256),
  });
  if (bad.length) throw new Error(`packaging/components/components.json: ${bad.join('; ')}`);
  // Nothing in resources/ may need a newer macOS than the app declares
  // (electron-builder writes the same value into LSMinimumSystemVersion).
  const minimum = MANIFEST.macos.minimum_system_version;
  const mac = macMinimumCheck(OUT, minimum, key);
  if (mac.problems.length) throw new Error(`minimum macOS ${minimum} (third-party.json macos.minimum_system_version): ${mac.problems.join('; ')}`);
  if (mac.files.length) console.log(`Mach-O files run on macOS ${minimum}: ${mac.files.map((f) => `${f.path} (${f.minos})`).join(', ')}`);
  const info = {
    version,
    platform: key,
    macos: { minimum_system_version: minimum, macho: mac.files },
    ffmpeg: { version: MANIFEST.ffmpeg.version, url: MANIFEST.ffmpeg[key].url, sha256: MANIFEST.ffmpeg[key].sha256, configure: ff.configure, license_checks: ff.licenseChecks, binaries_sha256: ff.binaries },
    uv: { version: MANIFEST.uv.version, url: MANIFEST.uv[key].url, sha256: MANIFEST.uv[key].sha256 },
    aria2c: { version: aria2c.version, url: MANIFEST.aria2c[key].url, url_sha256: MANIFEST.aria2c[key].sha256, sha256: aria2c.sha256, size: aria2c.size, checks: aria2c.checks },
    server_sha256: sha256File(server),
    nmhost_sha256: sha256File(nmhost),
    agent,
  };
  fs.writeFileSync(path.join(DESKTOP, 'build', 'stage-info.json'), `${JSON.stringify(info, null, 2)}\n`);
  console.log(`staged ${OUT}`);
  return info;
}

module.exports = {
  stage, stageAria2c, stageAgent, bundleAgent, BUNDLE_AGENT_ENV, BUNDLE_AGENT_DEFAULT, cachedBuildProblems, reuseCachedBuild, agentBuildArgs, AGENT_TARGETS, checkConfigure, checkLicenseText, gplFree, gplComponents, targetKey, checkAria2cVersion, aria2cSourceText, ffmpegSourceText, bundledFileProblems,
  machoSlices, machoVersion, compareVersions, macMinimumCheck,
  WORKER_FILES, GO_PROGRAMS, OUT,
};

if (require.main === module) {
  const i = process.argv.indexOf('--platform');
  stage({ platform: i > 0 ? process.argv[i + 1] : process.platform }).catch((err) => {
    console.error(`stage failed: ${err.message}`);
    process.exit(1);
  });
}
