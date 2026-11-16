// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Contribution checks for the browser extension's platform profiles and
// fixtures (SPEC-ext 贡献与合并, CONTRIBUTING-profiles.md). CI runs this
// before the extension's own tests (.github/workflows/extension.yml), so a
// contributor first sees a readable list of what to fix:
//
//   node scripts/profilecheck/check.mjs [--root DIR]
//
// 1. Layout: extension/profiles holds <id>.json files and LICENSE;
//    extension/fixtures holds <id>/<scenario>.json, <id> being a profile id
//    or "generic"; nothing else (no HAR files, screenshots, notes).
// 2. Profiles: the extension's own checkProfile (the JSON Schema plus what
//    a schema cannot say: regexes compile and cannot backtrack out of
//    control (regexComplexity), one capture group, unique roles,
//    nothing on the do-not-handle list — YouTube, MOOCs and online / paid
//    course platforms, extension/src/lib/blocklist.js, hosts and blocked
//    paths alike), so a profile valid here imports in the extension; the
//    file is named after its id; a profile that is not a draft comes with
//    fixtures; hosts are not a shared CDN, cloud storage or video site, nor
//    a whole public suffix.
// 3. Redaction scan, independent of the extension's redactor (a second line
//    of defence for hand-edited or badly exported reports): cookie and
//    authorisation headers, session cookies, "@" (e-mail addresses), JWTs,
//    bearer tokens, query values that are not redaction placeholders, URL
//    fragments and user info, real UUIDs, hex IDs, long numbers (student
//    numbers) and tokens that are not the redactor's fakes, field names
//    that name a person. A finding says where and what kind, never the
//    value: CI logs are public.
//
// Exit status: 0 clean, 1 errors, 2 the check itself could not run.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, '..', '..');

export const PROFILES_DIR = 'extension/profiles';
export const FIXTURES_DIR = 'extension/fixtures';
export const SCHEMA_FILE = 'extension/schema/profile.schema.json';
export const REPORT_KIND = 'lectureflow-detection-report';
const GENERIC = 'generic';
const MAX_PROFILE_BYTES = 64 * 1024;
const MAX_FIXTURE_BYTES = 1024 * 1024;
const ID_RE = /^[a-z][a-z0-9-]{1,39}$/;
const FIXTURE_NAME_RE = /^[a-z0-9][a-z0-9._-]{0,80}\.json$/;

// ------------------------------------------------------------ hosts

// Hosts no profile may claim: shared CDNs and cloud storage (a permission
// for the whole service, not for one platform), and the video sites the
// extension leaves alone (NOTES ext 第三波前的裁决 10; the whole
// do-not-handle list, course platforms included, is checkProfile's).
export const SHARED_HOSTS = [
  'cloudfront.net', 'amazonaws.com', 'akamaihd.net', 'akamaized.net', 'akamai.net', 'akamaitechnologies.com',
  'edgesuite.net', 'edgekey.net', 'azureedge.net', 'azurefd.net', 'windows.net', 'fastly.net', 'fastlylb.net',
  'cloudflare.net', 'cloudflarestream.com', 'b-cdn.net', 'googleapis.com', 'googleusercontent.com', 'gstatic.com',
  'github.io', 'githubusercontent.com', 'youtube.com', 'youtu.be', 'youtube-nocookie.com', 'googlevideo.com', 'ytimg.com',
];
const SUFFIX_LABELS = new Set(['ac', 'co', 'com', 'edu', 'gov', 'net', 'org', 'sch']);

/** Why a host pattern of a profile is not acceptable, or "". */
export function hostProblem(pattern) {
  const wild = pattern.startsWith('*.');
  const base = wild ? pattern.slice(2) : pattern;
  for (const s of SHARED_HOSTS) {
    if (base === s || base.endsWith(`.${s}`)) return `"${pattern}" is a shared service (${s}), not one platform`;
  }
  const labels = base.split('.');
  if (wild && labels.length === 2 && SUFFIX_LABELS.has(labels[0]) && /^[a-z]{2}$/.test(labels[1])) {
    return `"${pattern}" covers a whole public suffix`;
  }
  return '';
}

/** Whether host pattern a covers some host that b covers. */
export function hostsOverlap(a, b) {
  const base = (p) => (p.startsWith('*.') ? p.slice(2) : p);
  const under = (h, x) => h === x || h.endsWith(`.${x}`);
  const [ba, bb] = [base(a), base(b)];
  if (a.startsWith('*.') && under(bb, ba)) return true;
  if (b.startsWith('*.') && under(ba, bb)) return true;
  return ba === bb;
}

// ---------------------------------------------------- redaction scan

const PLACEHOLDER = /^\{(?:uuid|num\d+|hex\d+|tok\d+|str\d+)-\d+\}$/;
const UUID = /[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/g;
const FAKE_UUID = /^00000000-0000-4000-8000-[0-9a-f]{12}$/i;
// Published DRM system ids (DASH-IF registry), which manifests name in
// ContentProtection@schemeIdUri: public constants, not anybody's id.
const DRM_SYSTEM_IDS = new Set([
  '1077efec-c0b2-4d02-ace3-3c1e52e2fb4b', // W3C common PSSH
  'e2719d58-a985-b3c9-781a-b030af78d30e', // Clear Key
  'edef8ba9-79d6-4ace-a3c8-27dcd51d21ed', // Widevine
  '9a04f079-9840-4286-ab92-e65be0885f95', // PlayReady
  '94ce86fb-07ff-4f43-adb8-93d2fa968ca2', // FairPlay
  '5e629af5-38da-4063-8977-97ffbd9902d4', // Marlin
  'f239e769-efa3-4850-9c16-a903c6932efb', // Adobe Primetime
]);
// The extension tells the tracks of one DASH manifest apart by a fragment
// on the track id: <manifest>#lf=<kind>:<lang>:<index>.
const TRACK_SELECTOR = /^#lf=[a-z]+:[A-Za-z0-9-]+:\d+$/;
const HEX_RUN = /(?<![0-9A-Za-z])[0-9a-fA-F]{16,}(?![0-9A-Za-z])/g;
const FAKE_HEX = /^0+[0-9a-f]{0,4}$/i;
const DIGITS = /\d{5,}/g;
const FAKE_DIGITS = /^(?:0+\d{0,4}|\d{1,4}0+)$/;
// Tokens, as the redactor defines them: letters and digits only, 20 or
// more; or base64url-like (both cases and digits), 32 or more.
const TOKEN = /(?<![A-Za-z0-9])(?=[A-Za-z0-9]*[0-9])(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{20,}(?![A-Za-z0-9])/g;
const B64 = /(?<![A-Za-z0-9_-])(?=[A-Za-z0-9_-]*[a-z])(?=[A-Za-z0-9_-]*[A-Z])(?=[A-Za-z0-9_-]*[0-9])[A-Za-z0-9_-]{32,}(?![A-Za-z0-9_-])/g;
// The redactor's fake token: a counter, then the shape (A, a, 0).
const FAKE_TOKEN = /^\d{1,4}[Aa0]*$/;
const JWT = /eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}/;
const BEARER = /\bbearer\s+[A-Za-z0-9._~+/-]{8,}/i;
const HEADER_TEXT = /\b(?:set-cookie|cookie|authorization|proxy-authorization|x-api-key|x-auth-token)\s*:/i;
const HEADER_NAME = /^(?:set-cookie|cookie|cookies|authorization|proxy-authorization|x-api-key|x-auth-token|x-csrf-token|x-xsrf-token)$/i;
const SESSION_COOKIE = /\b(?:CloudFront-(?:Policy|Signature|Key-Pair-Id)|JSESSIONID|PHPSESSID|ASP\.NET_SessionId|\.ASPXAUTH|\.AspNet\.Cookies|connect\.sid|laravel_session|_session_id|sessionid|csrftoken|XSRF-TOKEN)=(?!\{)[^;&\s"']+/i;
const QUERY_PAIR = /[?&]([A-Za-z0-9_.~%+\-[\]]+)=([^&\s"'#<>]*)/g;
const URL_RE = /\b(?:https?|wss?):\/\/[^\s"'<>]+/gi;
const IDENTITY = /^(?:e-?mail(?:-?address)?|mail|user|user-?name|user-?id|login|login-?name|student|student-?(?:id|no|num|number)|stu-?(?:id|no)|net-?id|(?:first|last|full|given|family|sur|display|real)-?name|matric(?:ulation)?(?:-?(?:no|number))?|learner-?id|person-?id|author)$/i;

function isIdentityName(name) {
  let n = String(name);
  try {
    n = decodeURIComponent(n);
  } catch {
    // keep it
  }
  n = n.replace(/^data-/i, '').replace(/([a-z])([A-Z])/g, '$1-$2').replace(/[_\s.]+/g, '-');
  return IDENTITY.test(n);
}

function stripFakes(text, add, where) {
  let s = text.replace(UUID, (m) => {
    if (!FAKE_UUID.test(m) && !DRM_SYSTEM_IDS.has(m.toLowerCase())) add(where, 'unredacted UUID (a session or user id?)');
    return '.';
  });
  s = s.replace(HEX_RUN, (m) => {
    // The redactor's fake tokens (digits, A, a) are hex digits too.
    if (!FAKE_HEX.test(m) && !FAKE_TOKEN.test(m)) add(where, 'unredacted hexadecimal id');
    return '.';
  });
  return s;
}

function scanTokens(text, add, where) {
  for (const re of [B64, TOKEN]) {
    for (const m of text.matchAll(re)) {
      if (!FAKE_TOKEN.test(m[0])) add(where, 'unredacted token (a signature, ticket or session key?)');
    }
  }
}

function scanUrl(raw, add, where) {
  let u;
  try {
    u = new URL(raw);
  } catch {
    return;
  }
  if (u.username || u.password) add(where, 'user name or password in an address');
  if (u.hash && u.hash !== '#' && !TRACK_SELECTOR.test(u.hash)) add(where, 'address fragment kept (the extension drops it)');
  if (/\d{7,}/.test(u.hostname)) add(where, 'long number in a host name (a student number?)');
  let p = stripFakes(u.pathname, add, where);
  for (const m of p.matchAll(DIGITS)) {
    if (!FAKE_DIGITS.test(m[0])) add(where, 'unredacted number in an address path (a student number or id?)');
  }
  p = p.replace(DIGITS, '.');
  scanTokens(p, add, where);
}

/**
 * Scans one parsed fixture (or profile, with profile = true) for anything
 * a redacted report cannot contain. Returns [{ where, kind }] (one entry
 * per place and kind).
 */
export function scanForLeaks(value, { profile = false } = {}) {
  const out = [];
  const seen = new Set();
  const add = (where, kind) => {
    const k = `${where}|${kind}`;
    if (!seen.has(k)) {
      seen.add(k);
      out.push({ where: where || '(top level)', kind });
    }
  };
  const visitString = (s, where, key, parentKey) => {
    if (profile && parentKey === 'maintainers' && /^@[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/.test(s)) return;
    const noRanges = s.replace(/\d+@\d+/g, '');
    if (noRanges.includes('@') || /%40/i.test(noRanges)) add(where, '"@" (an e-mail address?)');
    if (HEADER_NAME.test(s.trim())) add(where, 'a cookie or authorisation header name');
    if (HEADER_TEXT.test(s)) add(where, 'a cookie or authorisation header');
    if (SESSION_COOKIE.test(s)) add(where, 'a session cookie');
    if (JWT.test(s)) add(where, 'a JSON Web Token');
    if (BEARER.test(s)) add(where, 'a bearer token');
    if (profile) return;
    for (const m of s.matchAll(QUERY_PAIR)) {
      if (isIdentityName(m[1])) add(where, 'a query parameter that names a person');
      if (m[2] !== '' && !PLACEHOLDER.test(m[2])) add(where, 'a query value that is not a redaction placeholder (a signature or token?)');
    }
    for (const m of s.matchAll(URL_RE)) scanUrl(m[0], add, where);
    // Outside addresses: UUIDs, hex ids and tokens must be fakes too.
    const rest = stripFakes(s.replace(URL_RE, ' '), add, where);
    scanTokens(rest.replace(QUERY_PAIR, ' '), add, where);
  };
  const walk = (v, where, key, parentKey) => {
    if (typeof v === 'string') visitString(v, where, key, parentKey);
    else if (Array.isArray(v)) v.forEach((x, i) => walk(x, `${where}[${i}]`, key, key));
    else if (v && typeof v === 'object') {
      for (const [k, x] of Object.entries(v)) {
        const at = where ? `${where}.${k}` : k;
        if (HEADER_NAME.test(k)) add(at, 'a cookie or authorisation field');
        if (isIdentityName(k)) add(at, 'a field that names a person');
        walk(x, at, k, key);
      }
    }
  };
  walk(value, '', '', '');
  return out;
}

// ------------------------------------------------------------ files

function readJson(file, rel, errors, max) {
  let text;
  try {
    const st = fs.statSync(file);
    if (st.size > max) {
      errors.push({ file: rel, where: '', message: `too large (${st.size} bytes, at most ${max})` });
      return undefined;
    }
    text = fs.readFileSync(file, 'utf8');
  } catch (e) {
    errors.push({ file: rel, where: '', message: `cannot be read: ${e.code || e.message}` });
    return undefined;
  }
  if (text.charCodeAt(0) === 0xfeff) {
    errors.push({ file: rel, where: '', message: 'starts with a byte order mark; save it as UTF-8 without BOM' });
    text = text.slice(1);
  }
  try {
    return JSON.parse(text);
  } catch (e) {
    errors.push({ file: rel, where: '', message: `not valid JSON: ${e.message}` });
    return undefined;
  }
}

function listDir(dir) {
  try {
    return fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  } catch {
    return null;
  }
}

/**
 * Checks the profiles and fixtures under root. checkProfile is the
 * extension's own (src/lib/profile.js), schema its profile schema.
 * Returns { errors, warnings, profiles, fixtures }; each finding is
 * { file, where, message } with paths relative to root.
 */
export function checkRepository({ root, schema, checkProfile }) {
  const errors = [];
  const warnings = [];
  const err = (file, where, message) => errors.push({ file, where, message });
  const profiles = new Map(); // id → { rel, profile }
  const fixtureCount = new Map(); // dir → reports

  const pdir = listDir(path.join(root, PROFILES_DIR)) || [];
  // Profile files by name, valid or not: fixtures next to a broken profile
  // are not reported a second time as orphans.
  const named = new Set(pdir.filter((e) => e.isFile() && e.name.endsWith('.json')).map((e) => e.name.slice(0, -5)));
  for (const e of pdir) {
    const rel = `${PROFILES_DIR}/${e.name}`;
    if (e.name === 'LICENSE' && e.isFile()) continue;
    if (!e.isFile() || !e.name.endsWith('.json')) {
      err(rel, '', `only <id>.json profiles and LICENSE belong in ${PROFILES_DIR}`);
      continue;
    }
    const p = readJson(path.join(root, rel), rel, errors, MAX_PROFILE_BYTES);
    if (p === undefined) continue;
    let problems;
    try {
      problems = checkProfile(schema, p);
    } catch (e) {
      err(rel, '', `the schema check failed: ${e.message}`);
      continue;
    }
    for (const x of problems) err(rel, x.path, x.message);
    if (problems.length) continue;
    if (`${p.id}.json` !== e.name) err(rel, '/id', `the file must be named ${p.id}.json`);
    if (profiles.has(p.id)) err(rel, '/id', `id "${p.id}" is used by ${profiles.get(p.id).rel} too`);
    if (p.fixtures !== undefined && p.fixtures !== `fixtures/${p.id}/`) err(rel, '/fixtures', `must be "fixtures/${p.id}/"`);
    for (const [field, list] of [['hosts', p.match.hosts], ['mediaHosts', p.match.mediaHosts || []]]) {
      list.forEach((h, i) => {
        const why = hostProblem(h);
        if (why) err(rel, `/match/${field}/${i}`, `${why}; name the platform's own domains`);
      });
    }
    for (const f of scanForLeaks(p, { profile: true })) err(rel, f.where, `contains ${f.kind}`);
    profiles.set(p.id, { rel, profile: p });
  }

  const fdir = listDir(path.join(root, FIXTURES_DIR)) || [];
  for (const d of fdir) {
    const relDir = `${FIXTURES_DIR}/${d.name}`;
    if (!d.isDirectory()) {
      err(relDir, '', `fixtures go in ${FIXTURES_DIR}/<id>/; nothing else belongs at this level`);
      continue;
    }
    if (d.name !== GENERIC && !named.has(d.name)) {
      err(relDir, '', ID_RE.test(d.name) ? `no profile ${PROFILES_DIR}/${d.name}.json for these fixtures` : 'not a profile id');
    }
    let n = 0;
    for (const f of listDir(path.join(root, relDir)) || []) {
      const rel = `${relDir}/${f.name}`;
      if (!f.isFile() || !FIXTURE_NAME_RE.test(f.name)) {
        err(rel, '', 'only detection reports exported by the extension (<scenario>.json, lower case) belong here; no HAR files, screenshots or notes');
        continue;
      }
      const report = readJson(path.join(root, rel), rel, errors, MAX_FIXTURE_BYTES);
      if (report === undefined) continue;
      n++;
      if (!report || report.kind !== REPORT_KIND) {
        err(rel, 'kind', `not a detection report exported by the extension (kind must be "${REPORT_KIND}")`);
      }
      const pid = report && report.profile ? report.profile.id : null;
      if (d.name === GENERIC && pid !== null) err(rel, 'profile', 'a generic fixture is exported where no profile matches (profile must be null)');
      if (d.name !== GENERIC && pid !== d.name) err(rel, 'profile.id', `was exported with ${pid === null ? 'no profile' : `profile "${pid}"`}; export it again with ${d.name}.json imported`);
      for (const x of scanForLeaks(report)) err(rel, x.where, `contains ${x.kind}; export the report again with the extension (do not edit it by hand)`);
    }
    fixtureCount.set(d.name, n);
  }

  for (const [id, { rel, profile }] of profiles) {
    if (!profile.draft && !fixtureCount.get(id)) {
      err(rel, '', `has no fixtures: add at least one report under ${FIXTURES_DIR}/${id}/, or mark it "draft": true`);
    }
  }
  const ids = [...profiles.keys()];
  for (let i = 0; i < ids.length; i++) {
    for (let j = i + 1; j < ids.length; j++) {
      const a = profiles.get(ids[i]);
      const b = profiles.get(ids[j]);
      for (const ha of a.profile.match.hosts) {
        const hb = b.profile.match.hosts.find((h) => hostsOverlap(ha, h));
        if (hb) {
          warnings.push({ file: b.rel, where: '/match/hosts', message: `"${hb}" overlaps "${ha}" of ${a.rel}; make sure match.pathPattern tells the two platforms apart` });
          break;
        }
      }
    }
  }
  return { errors, warnings, profiles: profiles.size, fixtures: [...fixtureCount.values()].reduce((s, x) => s + x, 0) };
}

// ------------------------------------------------------------- output

function escapeData(s) {
  return String(s).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A');
}
function escapeProperty(s) {
  return escapeData(s).replace(/:/g, '%3A').replace(/,/g, '%2C');
}

export function formatResult(result, { github = false } = {}) {
  const lines = [];
  const byFile = new Map();
  for (const [level, list] of [['error', result.errors], ['warning', result.warnings]]) {
    for (const f of list) {
      if (!byFile.has(f.file)) byFile.set(f.file, []);
      byFile.get(f.file).push({ level, ...f });
    }
  }
  for (const [file, list] of byFile) {
    lines.push(file);
    for (const f of list) lines.push(`  ${f.level}: ${f.where ? `${f.where}: ` : ''}${f.message}`);
  }
  if (github) {
    for (const [file, list] of byFile) {
      for (const f of list) {
        const title = f.level === 'error' ? 'Profile check' : 'Profile check (warning)';
        lines.push(`::${f.level} file=${escapeProperty(file)},title=${escapeProperty(title)}::${escapeData(`${f.where ? `${f.where}: ` : ''}${f.message}`)}`);
      }
    }
  }
  lines.push(`profile check: ${result.profiles} profiles, ${result.fixtures} fixtures, ${result.errors.length} errors, ${result.warnings.length} warnings`);
  return lines.join('\n');
}

export function summaryMarkdown(result) {
  const cell = (s) => String(s).replace(/\|/g, '\\|').replace(/\r?\n/g, ' ');
  const out = ['## Platform profiles and fixtures', ''];
  out.push(`${result.profiles} profiles, ${result.fixtures} fixtures: **${result.errors.length} errors**, ${result.warnings.length} warnings.`, '');
  const all = [...result.errors.map((f) => ['error', f]), ...result.warnings.map((f) => ['warning', f])];
  if (all.length) {
    out.push('| | File | Where | Problem |', '|---|---|---|---|');
    for (const [level, f] of all) out.push(`| ${level} | \`${cell(f.file)}\` | ${f.where ? `\`${cell(f.where)}\`` : ''} | ${cell(f.message)} |`);
    out.push('', 'See CONTRIBUTING-profiles.md. 见 CONTRIBUTING-profiles.md。');
  }
  return out.join('\n') + '\n';
}

async function main(argv) {
  const i = argv.indexOf('--root');
  const root = path.resolve(i >= 0 ? argv[i + 1] : repoRoot);
  const { checkProfile } = await import(pathToFileURL(path.join(repoRoot, 'extension', 'src', 'lib', 'profile.js')).href);
  const schema = JSON.parse(fs.readFileSync(path.join(root, SCHEMA_FILE), 'utf8'));
  const result = checkRepository({ root, schema, checkProfile });
  const github = process.env.GITHUB_ACTIONS === 'true';
  console.log(formatResult(result, { github }));
  if (github && process.env.GITHUB_STEP_SUMMARY) fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY, summaryMarkdown(result));
  return result.errors.length ? 1 : 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (e) => {
      console.error(`profile check could not run: ${e.stack || e}`);
      process.exit(2);
    },
  );
}
