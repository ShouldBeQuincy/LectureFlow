// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The source archive AMO asks for (docs/extension/store/firefox-amo.md §9):
// what the Firefox build reads, from one commit, made with git archive, with
// build instructions (README-AMO.md) at its root. Then it proves the
// instructions: the archive is unpacked into a temporary directory and
// built exactly as README-AMO.md says, and the add-on that comes out is
// compared file by file with the submitted one (--check), or, without
// --check, with a build of the same commit's extension in this checkout's
// dist (when that is the same version).
//
//   node scripts/amo-source.mjs --ref <tag or commit> [--out DIR] [--check lectureflow-firefox-X.Y.Z.zip]
//
// Writes DIR/lectureflow-extension-<version>-source.zip and DIR/README-AMO.md
// (the same text as in the archive). DIR defaults to dist/amo-source. The
// archive never holds the maintainers' agent and process files (CLAUDE.md,
// .claude/, PROGRESS.md, NOTES.md, SPEC*.md, AGENTS.md) or the
// extension's documentation pictures; the script refuses to write one that
// does. Node standard library and git only.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const REPO_URL = 'https://github.com/ShouldBeQuincy/LectureFlow';

// What the build reads (scripts/build.mjs): the extension's own tree, the
// app's design tokens and the licence. The rest of the extension's tree
// (tests, fixtures, schema) is source too and goes with it; extension/go.mod
// is not (it only keeps the app's Go tools out of extension/, P6 C6).
const PATHS = ['extension', 'web/style.css', 'LICENSE'];
const EXCLUDE = ['extension/CLAUDE.md', 'extension/AGENTS.md', 'extension/docs', 'extension/go.mod'];
const FORBIDDEN = /(^|\/)(CLAUDE\.md|AGENTS\.md|PROGRESS\.md|NOTES\.md|SPEC[^/]*\.md)$|(^|\/)\.claude\//;

function arg(name, def = '') {
  const i = process.argv.indexOf(name);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
}

// git runs at the top of the repository: the paths above are relative to it
const top = execFileSync('git', ['rev-parse', '--show-toplevel'], { cwd: root, encoding: 'utf8' }).trim();
const git = (...args) => execFileSync('git', args, { cwd: top, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }).trim();
const sha256 = (buf) => crypto.createHash('sha256').update(buf).digest('hex');

/** The entries of a zip file: [{ name, data }] (stored or deflated; no zip64). */
export function readZip(buf) {
  let eocd = -1;
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 22 - 65535); i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) throw new Error('not a zip file');
  const count = buf.readUInt16LE(eocd + 10);
  let p = buf.readUInt32LE(eocd + 16);
  const out = [];
  for (let n = 0; n < count; n++) {
    if (buf.readUInt32LE(p) !== 0x02014b50) throw new Error('bad central directory');
    const method = buf.readUInt16LE(p + 10);
    const csize = buf.readUInt32LE(p + 20);
    const nameLen = buf.readUInt16LE(p + 28);
    const extraLen = buf.readUInt16LE(p + 30);
    const commentLen = buf.readUInt16LE(p + 32);
    const local = buf.readUInt32LE(p + 42);
    const name = buf.toString('utf8', p + 46, p + 46 + nameLen);
    p += 46 + nameLen + extraLen + commentLen;
    if (buf.readUInt32LE(local) !== 0x04034b50) throw new Error(`bad local header for ${name}`);
    const start = local + 30 + buf.readUInt16LE(local + 26) + buf.readUInt16LE(local + 28);
    const raw = buf.subarray(start, start + csize);
    if (name.endsWith('/')) continue;
    if (method === 0) out.push({ name, data: Buffer.from(raw) });
    else if (method === 8) out.push({ name, data: zlib.inflateRawSync(raw) });
    else throw new Error(`${name}: compression method ${method}`);
  }
  return out;
}

function unzipTo(buf, dir) {
  for (const { name, data } of readZip(buf)) {
    const file = path.join(dir, ...name.split('/'));
    if (!file.startsWith(dir + path.sep)) throw new Error(`unsafe path in archive: ${name}`);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, data);
  }
}

/** Differences between two add-on zips, file by file. */
function compareZips(a, b) {
  const ma = new Map(readZip(a).map((e) => [e.name, e.data]));
  const mb = new Map(readZip(b).map((e) => [e.name, e.data]));
  const diffs = [];
  for (const [name, data] of ma) {
    if (!mb.has(name)) diffs.push(`only in the build from source: ${name}`);
    else if (!data.equals(mb.get(name))) diffs.push(`differs: ${name}`);
  }
  for (const name of mb.keys()) if (!ma.has(name)) diffs.push(`only in the submitted add-on: ${name}`);
  return { files: ma.size, diffs };
}

function readme({ version, commit, ref, prefix, addonSha, files, node, platform }) {
  return `# Building "Send to LectureFlow" ${version} for Firefox from source

This archive holds the source of the LectureFlow browser extension
(Firefox add-on ID \`lectureflow@qu1ncy.dev\`), version ${version}, from
commit \`${commit}\`${/^[0-9a-f]{7,40}$/.test(ref) ? '' : ` (${ref})`}. Project repository:
${REPO_URL}. It was made with:

    git archive --format=zip --prefix=${prefix}/ ${commit} -- ${PATHS.join(' ')} ${EXCLUDE.map((x) => `':(exclude)${x}'`).join(' ')}

plus this file. Left out: the AI-agent instructions for contributors
(extension/CLAUDE.md, extension/AGENTS.md), the documentation pictures
(extension/docs/) and extension/go.mod (it only keeps the repository's Go
tools out of extension/); the build reads none of them.

## Environment

- Any operating system. The build uses the Node.js standard library only:
  no \`npm install\`, no network access, nothing from npm. A small in-repo
  bundler (scripts/bundle.mjs, Node standard library only, no minifier)
  joins content/inpage.js and its relative imports into one classic
  script; nothing is minified or obfuscated.
- Node.js 24 or later (https://nodejs.org/). Checked with Node.js ${node}
  on ${platform}. The output is meant not to depend on the Node.js version
  or the operating system: the PNG icons are drawn by the build script and
  written with stored (uncompressed) blocks, and the add-on zip is stored,
  in sorted order, with fixed 1980-01-01 timestamps.
- The devDependencies in extension/package.json (playwright, web-ext) are
  used by the end-to-end tests and the linter only, not by the build.

## Contents

- \`extension/\`: the extension's source (src/), its locales (_locales/),
  manifests (manifest.base.json + manifest.firefox.json), platform profiles
  (profiles/, CC0) with their JSON schema (schema/), the build scripts
  (scripts/), tests (test/) and redacted detection reports (fixtures/).
- \`web/style.css\`: the LectureFlow app's stylesheet; the build takes the
  design tokens (colours, sizes, fonts) from it.
- \`LICENSE\`: GNU AGPL v3 (the extension is AGPL-3.0-or-later).

## Build

    cd extension
    node scripts/build.mjs --target firefox
    node scripts/package.mjs --target firefox

The first command writes \`extension/dist/firefox/\` (the add-on's files),
the second packs it into \`extension/dist/lectureflow-firefox-${version}.zip\`.
Do not add \`--e2e\`: that builds the test variant (extra host permissions
for a local test site and test hooks), never submitted.

## Check

The same source gives the same bytes, so the package equals the add-on
as it was uploaded, byte for byte:

    sha256sum extension/dist/lectureflow-firefox-${version}.zip
    # ${addonSha}

or, file by file (${files} files):

    mkdir /tmp/submitted && cd /tmp/submitted && unzip <uploaded add-on>.zip
    diff -r /tmp/submitted <archive>/extension/dist/firefox    # no output

(A copy signed by AMO also holds the META-INF/ directory the signing adds.)

## What the build generates (everything else is copied unchanged)

- \`manifest.json\`: manifest.base.json merged with manifest.firefox.json;
  the version from package.json.
- \`ui/tokens.css\`, \`ui/tokens.js\`: the design tokens from web/style.css.
- \`ui/icons.js\`: the path data of src/vendor/tabler/*.svg (Tabler Icons
  3.49.0, MIT; the SVG files themselves are not copied).
- \`ui/inpage-css.js\`: tokens + src/ui/base.css + src/ui/card.css +
  src/content/inpage.css as one string (for the in-page shadow roots).
- \`content/inpage.js\`: src/content/inpage.js and the modules it imports,
  joined into one classic script by scripts/bundle.mjs (content scripts
  cannot be modules). Readable source, not minified or renamed.
- \`icons/icon-{16,32,48,128}.png\`: drawn by scripts/lib.mjs from the app's
  mark.
- \`build-info.js\`: build target and version.
- \`profiles/index.json\`: the list of bundled platform profiles.
- Not copied: src/lib/replay.js (fixture replay for CI) and the Tabler SVGs.

## Third-party code

- \`src/vendor/browser-polyfill.js\` (copied as \`vendor/browser-polyfill.js\`):
  webextension-polyfill 0.12.0, dist/browser-polyfill.js, unmodified (MPL-2.0).
  Release: https://github.com/mozilla/webextension-polyfill/releases/tag/0.12.0
  Package: https://registry.npmjs.org/webextension-polyfill/-/webextension-polyfill-0.12.0.tgz
- \`src/vendor/tabler/*.svg\`: Tabler Icons 3.49.0 outline icons (MIT),
  turned into ui/icons.js by the build.
  Release: https://github.com/tabler/tabler-icons/releases/tag/v3.49.0
  Package: https://registry.npmjs.org/@tabler/icons/-/icons-3.49.0.tgz

No other third-party code; no remote code.
`;
}

function main() {
  const ref = arg('--ref');
  if (!ref) throw new Error('usage: node scripts/amo-source.mjs --ref <tag or commit> [--out DIR] [--check submitted.zip]');
  const out = path.resolve(arg('--out', path.join(root, 'dist', 'amo-source')));
  const check = arg('--check') ? path.resolve(arg('--check')) : '';
  const commit = git('rev-parse', '--verify', `${ref}^{commit}`);
  const version = JSON.parse(git('show', `${commit}:extension/package.json`)).version;
  const prefix = `lectureflow-extension-${version}`;
  const pathspec = ['--', ...PATHS, ...EXCLUDE.map((x) => `:(exclude)${x}`)];
  const work = fs.mkdtempSync(path.join(os.tmpdir(), 'lf-amo-src-'));
  try {
    // 1. The archive without the instructions, built as they will say: the
    //    add-on's digest goes into README-AMO.md.
    const bare = path.join(work, 'bare.zip');
    git('archive', '--format=zip', `--prefix=${prefix}/`, '-o', bare, commit, ...pathspec);
    const built = buildFrom(fs.readFileSync(bare), path.join(work, 'bare'), prefix, version);
    const addonSha = sha256(built.zip);

    // 2. The archive with README-AMO.md.
    fs.mkdirSync(out, { recursive: true });
    const text = readme({ version, commit, ref, prefix, addonSha, files: built.files, node: process.version, platform: `${process.platform} ${process.arch}` });
    const readmeFile = path.join(work, 'README-AMO.md');
    fs.writeFileSync(readmeFile, text);
    const archive = path.join(out, `${prefix}-source.zip`);
    git('archive', '--format=zip', `--prefix=${prefix}/`, `--add-file=${readmeFile}`, '-o', archive, commit, ...pathspec);
    fs.writeFileSync(path.join(out, 'README-AMO.md'), text);
    const names = readZip(fs.readFileSync(archive)).map((e) => e.name);
    const bad = names.filter((n) => FORBIDDEN.test(n.slice(prefix.length + 1)));
    if (bad.length) {
      fs.rmSync(archive, { force: true });
      throw new Error(`the archive would hold files that must not be published: ${bad.join(', ')}`);
    }

    // 3. Follow README-AMO.md on the final archive and compare.
    const again = buildFrom(fs.readFileSync(archive), path.join(work, 'final'), prefix, version);
    if (!again.zip.equals(built.zip)) throw new Error('the build from the final archive differs from the first one');
    console.log(`source archive: ${archive} (${names.length} files, sha256 ${sha256(fs.readFileSync(archive))})`);
    console.log(`commit ${commit}, extension ${version}; built with Node.js ${process.version}`);
    console.log(`add-on built from the archive: lectureflow-firefox-${version}.zip, ${built.files} files, sha256 ${addonSha}`);
    const reference = check || path.join(root, 'dist', `lectureflow-firefox-${version}.zip`);
    if (!fs.existsSync(reference)) {
      console.log(check ? `missing: ${check}` : '(no submitted add-on to compare with; pass --check)');
      if (check) process.exitCode = 1;
      return;
    }
    const ref2 = fs.readFileSync(reference);
    const { files, diffs } = compareZips(built.zip, ref2);
    console.log(`compared with ${reference}: ${files} files, ${diffs.length} differences; byte-identical zip: ${built.zip.equals(ref2) ? 'yes' : 'no'} (sha256 ${sha256(ref2)})`);
    for (const d of diffs) console.log(`  ${d}`);
    if (diffs.length) process.exitCode = 1;
  } finally {
    fs.rmSync(work, { recursive: true, force: true });
  }
}

/** Unpacks an archive and runs the build of README-AMO.md; returns the add-on zip and its file count. */
function buildFrom(archiveBuf, dir, prefix, version) {
  unzipTo(archiveBuf, dir);
  const ext = path.join(dir, prefix, 'extension');
  for (const script of [['scripts/build.mjs', '--target', 'firefox'], ['scripts/package.mjs', '--target', 'firefox']]) {
    execFileSync(process.execPath, script, { cwd: ext, stdio: ['ignore', 'ignore', 'inherit'] });
  }
  const zip = fs.readFileSync(path.join(ext, 'dist', `lectureflow-firefox-${version}.zip`));
  return { zip, files: readZip(zip).length };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (e) {
    console.error(String(e && e.message ? e.message : e));
    process.exitCode = 1;
  }
}
