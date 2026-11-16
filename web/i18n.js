// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// UI language (SPEC-v2 P14). FROZEN by the lead: t() is the only way the
// front end gets UI text (P14.2); catalogs live in web/i18n/{locale}.json as
// flat dotted keys. A value is a string with {param} placeholders, or a
// plural object {"one": "...", "few": "...", "many": "...", "other": "..."}
// selected with Intl.PluralRules on params.n (P14.2); a form missing in one
// language falls back along the chain like a missing key.
//
// Lecture content, glossary entries and model-written titles are not UI
// text and are never passed through t() (P14.1).

export const LOCALES = [
  { code: 'zh-CN', name: '简体中文' },
  { code: 'en', name: 'English' },
  { code: 'zh-TW', name: '繁體中文（台灣）' },
  { code: 'yue-Hant-HK', name: '粵語（香港）' },
  { code: 'ru', name: 'Русский' },
  { code: 'hi', name: 'हिन्दी' },
];

// en-XA is the development pseudo-locale of P14.9 (stretched, accented English).
export const PSEUDO = 'en-XA';

// Fallback chains of P14.2 (the locale itself first).
const CHAINS = {
  'zh-CN': ['zh-CN', 'en'],
  en: ['en'],
  'zh-TW': ['zh-TW', 'en'],
  'yue-Hant-HK': ['yue-Hant-HK', 'zh-TW', 'en'],
  ru: ['ru', 'en'],
  hi: ['hi', 'en'],
  [PSEUDO]: [PSEUDO, 'en'],
};

const catalogs = {}; // locale → { key: value }
const plurals = {}; // locale → Intl.PluralRules
const warned = new Set();
let current = 'zh-CN';
let dev = false;

/** Development mode: missing keys warn once and render as ⟦key⟧ (P14.2). */
export function setDevMode(on) {
  dev = !!on;
}

export function getLocale() {
  return current;
}

/** Picks the best supported locale for navigator.languages (P14.7). */
export function detectLocale(languages = navigator.languages || [navigator.language]) {
  for (const raw of languages || []) {
    const l = String(raw || '').toLowerCase();
    if (!l) continue;
    if (l.startsWith('yue') || l === 'zh-hk' || l.startsWith('zh-hant-hk') || l === 'zh-mo') return 'yue-Hant-HK';
    if (l.startsWith('zh-hant') || l === 'zh-tw') return 'zh-TW';
    if (l.startsWith('zh')) return 'zh-CN';
    if (l.startsWith('ru')) return 'ru';
    if (l.startsWith('hi')) return 'hi';
    if (l.startsWith('en')) return 'en';
  }
  return 'en';
}

async function load(code) {
  if (catalogs[code]) return;
  if (code === PSEUDO) {
    await load('en');
    catalogs[PSEUDO] = pseudoCatalog(catalogs.en);
    return;
  }
  try {
    const res = await fetch(`i18n/${code}.json`, { cache: 'no-cache' });
    catalogs[code] = res.ok ? await res.json() : {};
  } catch {
    catalogs[code] = {};
  }
}

/**
 * Loads the catalogs of the locale's chain and makes it current. Sets
 * <html lang> (fonts per :lang(), P14.5) and dispatches "localechange" on
 * window so mounted views can re-render without a reload (P14.7).
 */
export async function setLocale(code) {
  if (!CHAINS[code]) code = 'en';
  await Promise.all(CHAINS[code].map(load));
  current = code;
  document.documentElement.lang = code === PSEUDO ? 'en' : code;
  window.dispatchEvent(new CustomEvent('localechange', { detail: { locale: code } }));
}

function pluralRules(locale) {
  if (!plurals[locale]) plurals[locale] = new Intl.PluralRules(locale === PSEUDO ? 'en' : locale);
  return plurals[locale];
}

// {name:lc} (round 9, T28 suggestion 10): the value of a string param with
// the first letter of each word that starts with a capital letter of a
// script other than Latin followed by a small letter (Cyrillic "Ждёт",
// "Перевод") lower-cased in the UI language, for a fragment (a stage's name,
// what a stage waits for, why a lecture is out of date) placed after a colon
// or a dash of a Russian template: "Расшифровка: ждёт …", a list joined
// before ("этапы: исправление, перевод, экспорт"). Everything else is left
// as it is: words that start with a Latin letter ("LLM", "Colab",
// "DeepSeek"), all-capital abbreviations ("ГПУ"), numbers, a param that is
// not a string. A fragment shown alone keeps its capital, so the modifier is
// the translator's, in templates whose fragments name nothing proper in
// such a script. The test of web/i18n_check_test.go counts {name:lc} as
// {name}, and knows no other modifier: {name:other} is left as written.
// The ru catalog's first use (round 9) asked for two more rules:
//   - a word of one capital letter is lower-cased too when a space and a
//     word that starts with a small letter follow ("В очереди" -> "в
//     очереди"); before a capital or a digit it is left alone (part of an
//     abbreviation, "В ГПУ");
//   - what is quoted is a name quoted as written (a settings section
//     «Компоненты», a device «Моя Карта» the user named): from an opening
//     quote (« „ “ " ‘ ' where a word may start, not after a letter or a
//     digit: an apostrophe is not one) to its closing one (» “ ” " ’ ')
//     nothing is changed, quotes inside it included; without a closing
//     quote, to the end.
const LC_QUOTES = { '«': '»', '„': '“”', '“': '”', '"': '"', '‘': '’', "'": "'’" };
const LC_WORDCHAR = /[\p{L}\p{M}\p{N}_]/u;
const LC_CAPITAL = /^(?!\p{Script=Latin})\p{Lu}$/u;
const LC_SMALL = /^\p{Ll}$/u;
const LC_SPACE = /^\s$/u;

function lowerWords(s) {
  const locale = current === PSEUDO ? 'en' : current;
  const ch = Array.from(s);
  const closers = []; // the closing quotes awaited, innermost last
  let out = '';
  for (let i = 0; i < ch.length; i++) {
    const c = ch[i];
    const wordStart = i === 0 || !LC_WORDCHAR.test(ch[i - 1]);
    if (closers.length && closers[closers.length - 1].includes(c)) {
      closers.pop();
      out += c;
      continue;
    }
    if (LC_QUOTES[c] && wordStart) {
      closers.push(LC_QUOTES[c]);
      out += c;
      continue;
    }
    if (!closers.length && wordStart && LC_CAPITAL.test(c)) {
      const next = ch[i + 1];
      const word = next !== undefined && LC_SMALL.test(next);
      const letter = next !== undefined && LC_SPACE.test(next) && ch[i + 2] !== undefined && LC_SMALL.test(ch[i + 2]);
      if (word || letter) {
        out += c.toLocaleLowerCase(locale);
        continue;
      }
    }
    out += c;
  }
  return out;
}

function interpolate(s, params) {
  if (!params) return s;
  return s.replace(/\{(\w+)(?::(\w+))?\}/g, (m, name, mod) => {
    const v = params[name];
    if (v === undefined || v === null) return m;
    if (mod === undefined) return String(v);
    if (mod === 'lc') return typeof v === 'string' ? lowerWords(v) : String(v);
    return m;
  });
}

function warnOnce(id, message) {
  if (!dev || warned.has(id)) return;
  warned.add(id);
  console.warn(message);
}

/**
 * The UI text of key in the current locale (P14.2). A plural object that
 * lacks the form Intl.PluralRules picks for params.n (a Russian "few", say)
 * counts as missing in that language: the next language of the fallback
 * chain is tried with its own rules. Only when no language has the form
 * is some language's "other" form used.
 */
export function t(key, params) {
  let other = null;
  for (const code of CHAINS[current] || ['en']) {
    const cat = catalogs[code];
    if (!cat || !(key in cat)) continue;
    let v = cat[key];
    if (v && typeof v === 'object') {
      // params.n may be a count from util.js count() (shown formatted, a number for the rules)
      const num = params ? Number(params.n) : NaN;
      const n = Number.isFinite(num) ? num : 0;
      const form = pluralRules(code).select(n);
      if (typeof v[form] !== 'string') {
        warnOnce(`${key}|${code}|${form}`, `i18n: ${key} has no plural form "${form}" in ${code}`);
        if (other === null && typeof v.other === 'string') other = v.other;
        continue;
      }
      v = v[form];
    }
    if (typeof v !== 'string') continue;
    return interpolate(v, params);
  }
  if (other !== null) return interpolate(other, params);
  warnOnce(key, `i18n: missing key ${key} (${current})`);
  return dev ? `⟦${key}⟧` : key;
}

/** Whether key exists in the current chain (for optional texts). */
export function has(key) {
  return (CHAINS[current] || ['en']).some((c) => catalogs[c] && key in catalogs[c]);
}

// ---- formatting (P14.4) ----
//
// Numbers, sizes, money, dates and relative times are formatted by util.js
// only (fmtInt, fmtTokens, fmtBytes, fmtMoney, fmtDateTime, relTime, …): one
// set of formats for the whole UI (lead ruling at the gate; checked by
// web/i18n_check_test.go).

// ---- en-XA pseudo-locale (P14.9) ----

const ACCENTS = { a: 'á', e: 'é', i: 'í', o: 'ö', u: 'ü', c: 'ç', n: 'ñ', y: 'ý', A: 'Å', E: 'É', I: 'Î', O: 'Ø', U: 'Û', C: 'Ç', N: 'Ñ' };

function pseudo(s) {
  // Keep {params} (and {param:lc}) intact; accent letters; stretch by ~40%.
  const parts = s.split(/(\{\w+(?::\w+)?\})/);
  const out = parts.map((p) => (/^\{\w+(?::\w+)?\}$/.test(p) ? p : p.replace(/[aeiouycnAEIOUCN]/g, (ch) => ACCENTS[ch] || ch))).join('');
  const pad = Math.ceil(s.length * 0.4);
  return `[${out}${pad > 0 ? ' ' + '~'.repeat(pad) : ''}]`;
}

function pseudoCatalog(en) {
  const out = {};
  for (const [k, v] of Object.entries(en || {})) {
    if (typeof v === 'string') out[k] = pseudo(v);
    else if (v && typeof v === 'object') out[k] = Object.fromEntries(Object.entries(v).map(([f, s]) => [f, typeof s === 'string' ? pseudo(s) : s]));
  }
  return out;
}
