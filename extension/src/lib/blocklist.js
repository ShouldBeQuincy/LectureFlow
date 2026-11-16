// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Sites the extension does not handle at all, in every build (Chrome,
// Edge and Firefox alike):
//
//   youtube  YouTube and its media hosts (lead ruling 10, NOTES "ext 第三波
//            前的裁决"; user 2026-10-27: "Chrome 版本不能刮 YouTube"). Chrome
//            Web Store rejects extensions that help download YouTube videos
//            (Blue Lithium), and lecture recordings do not live there.
//   courses  MOOCs and online / paid course platforms (user 2026-10-27:
//            "全部版本不能刮 MOOC / 付费课程平台（Udemy之类的）"). LectureFlow
//            is for the recordings a student's own school gives them, not
//            for a course platform's catalogue.
//
// An entry is a host ("udemy.com": that host and every subdomain) or a
// host with a path prefix ("linkedin.com/learning": only pages whose path
// is /learning or below it, on that host and its subdomains; the rest of
// the site is not blocked, and the site itself may still be authorized).
// Hosts are compared after the URL parser's normalisation (lower case,
// IDNA / punycode, no port or user info) without a trailing dot; paths
// after decoding percent escapes, lower-cased, with "\" read as "/",
// repeated slashes collapsed and dot segments resolved; a prefix covers
// the path itself and everything below it ("/learning", "/learning/…",
// "/learning;…"), never "/learning-login".
//
// What "blocked" means where (the callers):
//   - a page (the tab's top-level address) that is blocked: the popup says
//     LectureFlow does not handle the site, nothing is detected or counted,
//     the badge stays empty, no in-page button, nothing can be sent;
//   - a frame whose address is blocked, or that sits inside a blocked page
//     or frame (its location.ancestorOrigins): nothing is read in it, and
//     no media request it made counts, whichever host serves the media
//     (course platforms keep their media on general CDNs) — see
//     src/lib/blockedframes.js;
//   - a track whose URL is blocked (YouTube's media hosts, Udemy's CDN):
//     dropped wherever it was seen;
//   - a host that is blocked as a whole cannot be authorized (options, the
//     popup's "在此网站自动探测") and gets no content script; a profile
//     whose match covers a blocked host or a blocked path is refused
//     (src/lib/profile.js checkProfile: the extension's import, the tests
//     and the repository's CI check, scripts/profilecheck).
// LectureFlow refuses the same list on its side (internal/blocklist, with a
// test that reads the BLOCKLIST array below), as a second line of defence.
//
// The list is what the names and public knowledge say a site is; nothing
// here was "checked" by visiting it. Deliberately not on it (user
// 2026-10-27): 雨课堂 yuketang.cn ("THU 和很多高校就是拿来录": a school's
// own lecture recordings, like Panopto); the Open University's open.edu
// ("不管，发函投诉到我再做甄别"); 超星 / 学习通 chaoxing.com and 智慧树
// zhihuishu.com, for the same reason as 雨课堂 (schools keep their own
// lecturers' recordings there too; "全部按照推荐执行"). Known gap: course
// stores on their own domains (Teachable, Thinkific, Kajabi, 小鹅通 with a
// custom domain) are not caught; only the platforms' own domains are.
// Pure.

import { parseHttpUrl } from './url.js';
import { regexComplexity } from './regexcost.js';
import { guardedRegex, regexTrip } from './regexguard.js';

export const CATEGORY_YOUTUBE = 'youtube';
export const CATEGORY_COURSES = 'courses';

// One entry per line, [category, 'host' or 'host/path'], so that
// internal/blocklist's test can read this array; keep the Go copy
// (internal/blocklist/blocklist.go) in the same order.
// blocklist:begin
export const BLOCKLIST = Object.freeze([
  // --- YouTube
  ['youtube', 'youtube.com'], // YouTube, YouTube Music, m.youtube.com
  ['youtube', 'youtu.be'], // YouTube short links
  ['youtube', 'youtube-nocookie.com'], // YouTube's privacy-enhanced embeds
  ['youtube', 'youtubekids.com'], // YouTube Kids
  ['youtube', 'youtube.googleapis.com'], // YouTube's API and legacy embed host
  ['youtube', 'youtubeeducation.com'], // YouTube's embed host for schools (YouTube Player for Education)
  ['youtube', 'googlevideo.com'], // YouTube's media servers
  // --- Course platforms: international MOOCs and paid / online course sites
  ['courses', 'udemy.com'], // Udemy: paid course marketplace (and Udemy Business tenants)
  ['courses', 'udemycdn.com'], // Udemy's own CDN (course video and images)
  ['courses', 'coursera.org'], // Coursera: MOOCs, including Coursera for Campus
  ['courses', 'edx.org'], // edX: MOOCs
  ['courses', 'edx-video.net'], // edX's course video host
  ['courses', 'udacity.com'], // Udacity: paid "Nanodegree" courses
  ['courses', 'skillshare.com'], // Skillshare: paid creative courses
  ['courses', 'linkedin.com/learning'], // LinkedIn Learning (only /learning; the rest of LinkedIn is not blocked)
  ['courses', 'lynda.com'], // Lynda.com, now LinkedIn Learning
  ['courses', 'pluralsight.com'], // Pluralsight: paid tech courses
  ['courses', 'masterclass.com'], // MasterClass: paid video classes
  ['courses', 'domestika.org'], // Domestika: paid creative courses
  ['courses', 'futurelearn.com'], // FutureLearn: MOOCs
  ['courses', 'khanacademy.org'], // Khan Academy: free online courses
  ['courses', 'datacamp.com'], // DataCamp: paid data science courses
  ['courses', 'codecademy.com'], // Codecademy: paid coding courses
  ['courses', 'brilliant.org'], // Brilliant: paid interactive courses
  ['courses', 'frontendmasters.com'], // Frontend Masters: paid video courses
  ['courses', 'egghead.io'], // egghead: paid developer video courses
  ['courses', 'educative.io'], // Educative: paid developer courses
  ['courses', 'teamtreehouse.com'], // Treehouse: paid tech courses
  ['courses', 'learning.oreilly.com'], // O'Reilly online learning (the rest of oreilly.com is not blocked)
  ['courses', 'wondrium.com'], // Wondrium (The Great Courses): paid video courses
  ['courses', 'thegreatcourses.com'], // The Great Courses
  ['courses', 'thegreatcoursesplus.com'], // The Great Courses Plus
  ['courses', 'creativelive.com'], // CreativeLive: paid creative classes
  ['courses', 'teachable.com'], // Teachable: hosted course stores
  ['courses', 'thinkific.com'], // Thinkific: hosted course stores
  ['courses', 'kajabi.com'], // Kajabi: hosted course stores
  ['courses', 'mykajabi.com'], // Kajabi stores' default domain
  ['courses', 'podia.com'], // Podia: hosted course stores
  ['courses', 'learnworlds.com'], // LearnWorlds: hosted course stores
  ['courses', 'zerotomastery.io'], // Zero To Mastery: paid tech courses
  ['courses', 'scrimba.com'], // Scrimba: paid coding courses
  ['courses', 'cbtnuggets.com'], // CBT Nuggets: paid IT training
  ['courses', 'acloudguru.com'], // A Cloud Guru: paid cloud training
  ['courses', 'cloudacademy.com'], // Cloud Academy: paid cloud training
  ['courses', 'laracasts.com'], // Laracasts: paid developer screencasts
  ['courses', 'percipio.com'], // Skillsoft Percipio: company e-learning
  ['courses', 'alison.com'], // Alison: free online courses
  ['courses', 'kadenze.com'], // Kadenze: arts and technology MOOCs
  ['courses', 'maven.com'], // Maven: paid cohort-based courses
  ['courses', 'fun-mooc.fr'], // FUN (France Université Numérique): French MOOCs
  ['courses', 'miriadax.net'], // MiríadaX: Spanish-language MOOCs
  ['courses', 'edraak.org'], // Edraak: Arabic MOOCs
  ['courses', 'kmooc.kr'], // K-MOOC: Korean national MOOCs
  ['courses', 'gacco.org'], // gacco: Japanese MOOCs
  ['courses', 'schoo.jp'], // Schoo: Japanese paid online classes
  ['courses', 'mooc.fi'], // MOOC.fi: the University of Helsinki's public MOOCs
  ['courses', 'mitxonline.mit.edu'], // MITx Online: MIT's MOOCs (the rest of mit.edu is not blocked)
  ['courses', 'openhpi.de'], // openHPI: the Hasso Plattner Institute's MOOCs
  ['courses', 'openlearning.com'], // OpenLearning: online course platform
  ['courses', 'gumroad.com'], // Gumroad: marketplace that sells online courses among other things
  ['courses', 'skool.com'], // Skool: paid community and course platform
  // --- Course platforms: Chinese
  ['courses', 'icourse163.org'], // 中国大学 MOOC
  ['courses', 'icourses.cn'], // 爱课程
  ['courses', 'cnmooc.org'], // 好大学在线
  ['courses', 'xuetangx.com'], // 学堂在线
  ['courses', 'study.163.com'], // 网易云课堂 (the rest of 163.com is not blocked)
  ['courses', 'ke.qq.com'], // 腾讯课堂 (the rest of qq.com is not blocked)
  ['courses', 'time.geekbang.org'], // 极客时间
  ['courses', 'bilibili.com/cheese'], // B 站课堂 (only /cheese; the rest of bilibili is not blocked)
  ['courses', 'dedao.cn'], // 得到
  ['courses', 'igetget.com'], // 得到 (older domain)
  ['courses', 'xiaoe-tech.com'], // 小鹅通: hosted course stores
  ['courses', 'xeknow.com'], // 小鹅通 stores' domain
  ['courses', 'lizhiweike.com'], // 荔枝微课
  ['courses', 'qianliao.tv'], // 千聊
  ['courses', 'cctalk.com'], // CCtalk
  ['courses', 'imooc.com'], // 慕课网: paid IT courses
  ['courses', 'edu.51cto.com'], // 51CTO 学堂 (the rest of 51cto.com is not blocked)
  ['courses', 'koolearn.com'], // 新东方在线
  ['courses', 'smartedu.cn'], // 国家智慧教育平台 (its higher-education part gathers MOOCs)
  ['courses', 'ximalaya.com'], // 喜马拉雅: audio platform that sells paid courses
  ['courses', 'hahow.in'], // Hahow 好學校
  ['courses', 'pressplay.cc'], // PressPlay Academy
  ['courses', 'ewant.org'], // ewant 育網
  ['courses', 'sharecourse.net'], // ShareCourse 學聯網
  // --- Course platforms: Russian
  ['courses', 'stepik.org'], // Stepik
  ['courses', 'openedu.ru'], // Открытое образование
  ['courses', 'lektorium.tv'], // Лекториум
  ['courses', 'universarium.org'], // Универсариум
  ['courses', 'skillbox.ru'], // Skillbox
  ['courses', 'netology.ru'], // Нетология
  ['courses', 'geekbrains.ru'], // GeekBrains
  ['courses', 'gb.ru'], // GeekBrains (current domain)
  ['courses', 'practicum.yandex.ru'], // Яндекс Практикум (the rest of yandex.ru is not blocked)
  ['courses', 'skillfactory.ru'], // SkillFactory
  ['courses', 'otus.ru'], // OTUS
  ['courses', 'foxford.ru'], // Фоксфорд: paid online school
  ['courses', 'htmlacademy.ru'], // HTML Academy
  // --- Course platforms: India
  ['courses', 'swayam.gov.in'], // SWAYAM: national MOOCs
  ['courses', 'nptel.ac.in'], // NPTEL: IIT video courses
  ['courses', 'unacademy.com'], // Unacademy
  ['courses', 'byjus.com'], // BYJU'S
  ['courses', 'vedantu.com'], // Vedantu
  ['courses', 'pw.live'], // Physics Wallah
  ['courses', 'upgrad.com'], // upGrad
  ['courses', 'simplilearn.com'], // Simplilearn
  ['courses', 'mygreatlearning.com'], // Great Learning
  ['courses', 'edureka.co'], // Edureka
  ['courses', 'scaler.com'], // Scaler
  ['courses', 'codingninjas.com'], // Coding Ninjas
]);
// blocklist:end

const ENTRIES = BLOCKLIST.map(([category, entry]) => {
  const i = entry.indexOf('/');
  return Object.freeze(i < 0 ? { category, host: entry, path: '' } : { category, host: entry.slice(0, i), path: entry.slice(i) });
});
const HOST_ENTRIES = ENTRIES.filter((e) => !e.path);
const PATH_ENTRIES = ENTRIES.filter((e) => e.path);

/** Hosts blocked as a whole (subdomains included). */
export const BLOCKED_HOSTS = Object.freeze(HOST_ENTRIES.map((e) => e.host));
/** Host + path prefix entries: only those pages are blocked. */
export const BLOCKED_PATHS = Object.freeze(PATH_ENTRIES.map((e) => Object.freeze({ host: e.host, path: e.path, category: e.category })));

const under = (host, base) => host === base || host.endsWith(`.${base}`);

/**
 * Match patterns of every blocked host and path, for the content scripts'
 * excludeMatches (a path entry excludes a little more than it blocks:
 * "/learning*" also covers "/learning-login").
 */
export function excludeMatchPatterns() {
  return [...BLOCKED_HOSTS.map((h) => `*://*.${h}/*`), ...BLOCKED_PATHS.map((e) => `*://*.${e.host}${e.path}*`)];
}

/**
 * A host as the URL parser writes it: lower case, IDNA (punycode), no
 * port, user info or trailing dot. Takes a bare host, "host:port",
 * "user@host" too. '' when it is not a host.
 */
export function normalizeHost(raw) {
  const s = String(raw ?? '').trim();
  if (!s) return '';
  let host;
  try {
    host = new URL(`http://${s}/`).hostname;
  } catch {
    return '';
  }
  return host.replace(/\.+$/, '');
}

/** The category of a host blocked as a whole, or ''. */
export function hostCategory(host) {
  const h = normalizeHost(host);
  if (!h) return '';
  const e = HOST_ENTRIES.find((x) => under(h, x.host));
  return e ? e.category : '';
}

/** Whether a host name is blocked as a whole (subdomains included; not the path entries). */
export function isBlockedHost(host) {
  return hostCategory(host) !== '';
}

/**
 * A URL path as the blocklist compares it: percent escapes decoded,
 * lower case, "\" as "/", repeated slashes collapsed, "." and ".."
 * resolved, starting with "/".
 */
export function normalizePath(pathname) {
  let p = String(pathname || '/').replace(/%([0-9a-fA-F]{2})/g, (_, x) => String.fromCharCode(parseInt(x, 16)));
  p = p.replace(/\\/g, '/').toLowerCase();
  const out = [];
  for (const seg of p.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') out.pop();
    else out.push(seg);
  }
  const joined = `/${out.join('/')}`;
  return out.length && p.endsWith('/') ? `${joined}/` : joined;
}

/** Whether a normalised path is the prefix itself or below it. */
export function pathCovered(path, prefix) {
  return path === prefix || path.startsWith(`${prefix}/`) || path.startsWith(`${prefix};`);
}

function asUrl(raw) {
  const u = parseHttpUrl(raw);
  if (u) return u;
  // blob:https://host/…: the page that made it
  try {
    const b = new URL(String(raw));
    if (b.protocol === 'blob:') return parseHttpUrl(b.pathname);
  } catch {
    // not a URL
  }
  return null;
}

/** The category of a blocked page, frame or media URL (host or path entries), or ''. */
export function blockedCategory(raw) {
  const u = asUrl(raw);
  if (!u) return '';
  const host = normalizeHost(u.hostname);
  const hc = hostCategory(host);
  if (hc) return hc;
  if (!PATH_ENTRIES.some((e) => under(host, e.host))) return '';
  const path = normalizePath(u.pathname);
  const e = PATH_ENTRIES.find((x) => under(host, x.host) && pathCovered(path, x.path));
  return e ? e.category : '';
}

/** Whether a page, frame or media URL is blocked (host or path entries). */
export function isBlockedUrl(raw) {
  return blockedCategory(raw) !== '';
}

/**
 * The category of an origin ("https://www.udemy.com", as in
 * location.ancestorOrigins) whose host is blocked as a whole, or ''. An
 * origin carries no path, so the path entries cannot apply.
 */
export function originCategory(origin) {
  const u = parseHttpUrl(origin);
  return u ? hostCategory(u.hostname) : '';
}

function wildcardBase(pattern) {
  const p = String(pattern || '').trim().toLowerCase();
  const wild = p.startsWith('*.');
  return { wild, base: normalizeHost(wild ? p.slice(2) : p) };
}

/**
 * The category of a host blocked as a whole that a host pattern
 * ("*.example.com", "video.example.com") could cover: it falls under one,
 * or is a wildcard wide enough to include one ("*.oreilly.com" covers
 * learning.oreilly.com). ''.
 */
export function wildcardCategory(pattern) {
  const { wild, base } = wildcardBase(pattern);
  if (!base) return '';
  const hc = hostCategory(base);
  if (hc) return hc;
  if (wild) {
    const e = HOST_ENTRIES.find((x) => under(x.host, base));
    if (e) return e.category;
  }
  return '';
}

export function wildcardTouchesBlocked(pattern) {
  return wildcardCategory(pattern) !== '';
}

/** The path entries whose host a host pattern could cover (its pages there need a pathPattern that keeps out of them). */
export function pathEntriesFor(pattern) {
  const { wild, base } = wildcardBase(pattern);
  if (!base) return [];
  return BLOCKED_PATHS.filter((e) => under(base, e.host) || (wild && under(e.host, base)));
}

/**
 * The profile schema's refusal of these hosts (schema/profile.schema.json,
 * $defs.host.not.pattern; a test keeps the two equal): a host blocked as a
 * whole, a subdomain of one or a wildcard over one, and a wildcard over a
 * parent domain of one ("*.oreilly.com"). The path entries need a
 * pathPattern check no schema can say; checkProfile does it.
 */
export function schemaHostPattern() {
  const esc = (h) => h.replace(/\./g, '\\.');
  const parents = new Set();
  for (const h of BLOCKED_HOSTS) {
    const labels = h.split('.');
    for (let i = 1; i <= labels.length - 2; i++) parents.add(labels.slice(i).join('.'));
  }
  return `^(?:\\*\\.)?(?:[a-z0-9-]+\\.)*(?:${BLOCKED_HOSTS.map(esc).join('|')})$|^\\*\\.(?:${[...parents].map(esc).join('|')})$`;
}

/** Whether a regular expression source has a "|" outside any group or class. */
function topLevelAlternation(src) {
  let depth = 0;
  let cls = false;
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    if (c === '\\') {
      i++;
      continue;
    }
    if (cls) {
      if (c === ']') cls = false;
      continue;
    }
    if (c === '[') cls = true;
    else if (c === '(') depth++;
    else if (c === ')') depth = Math.max(0, depth - 1);
    else if (c === '|' && depth === 0) return true;
  }
  return false;
}

/**
 * Whether a profile's path regular expression (case-insensitive, tested
 * against the page path) could match a page under a blocked path prefix.
 * It cannot only when it is anchored ("^/") with a fixed first path
 * segment that is not the blocked one (nor the start of it), has no "|"
 * outside a group, and matches none of a set of sample paths under the
 * prefix. Everything else counts as "could" (a profile on such a host
 * must say plainly which pages it is for). An expression too costly to
 * run (src/lib/regexcost.js) is never run here: it counts as "could"
 * (SEC18 中-2).
 */
export function pathPatternMayReach(source, prefix) {
  const src = String(source ?? '');
  // checked before anything is compiled or run (SEC22b M-2)
  if (regexComplexity(src)) return true;
  let re;
  try {
    re = guardedRegex(src);
  } catch {
    return true;
  }
  const samples = ['', '/', '/x', '/1', '/a/b', '/embed/1', '/play/ep1', '/course/1/lesson/2', '/x/y/z/', '?x=1'].map((s) => prefix + s);
  if (samples.some((p) => re.test(p))) return true;
  // stopped by the runtime guard: "no match" means nothing, so it may reach
  if (regexTrip('', src)) return true;
  if (topLevelAlternation(src)) return true;
  const m = /^\^\/((?:[A-Za-z0-9_~-]|\\[./_~-])*)/.exec(src);
  if (!m || !m[1]) return true;
  let literal = m[1].replace(/\\(.)/g, '$1').toLowerCase();
  const rest = src.slice(m[0].length);
  // a quantifier makes the literal's last character optional or repeated
  const quantified = /^[?*+{]/.test(rest);
  if (quantified) literal = literal.slice(0, -1);
  if (!literal) return true;
  // the literal's first segment, and whether the pattern fixes where it ends
  const slash = literal.indexOf('/');
  const first = slash >= 0 ? literal.slice(0, slash) : literal;
  const complete = slash >= 0 || (!quantified && (rest === '' || rest.startsWith('$') || rest.startsWith('\\/') || rest.startsWith('/')));
  const blockedFirst = prefix.split('/')[1] || '';
  if (complete) return first === blockedFirst;
  return blockedFirst.startsWith(first);
}
