// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The extension's six interface languages (SPEC-ext 界面设计: the same six
// as LectureFlow) and the language the user picks in the settings.
//
// By default the browser chooses among _locales/ (browser.i18n). That is
// not enough for Cantonese: neither browser offers a Cantonese interface
// (Chrome shows Hong Kong users its zh-TW interface and has no yue code;
// Firefox ships zh-CN and zh-TW only), so _locales/zh_HK, which holds the
// Cantonese text, is never chosen by itself. The settings therefore offer
// every language explicitly; a chosen language is read from the
// extension's own _locales files with the main app's fallback chains
// (yue-Hant-HK → zh-TW → en; others → en; web/i18n.js).
//
// Pure: message formatting follows browser.i18n.getMessage, so the same
// messages.json works both ways.

/** storage.local key of the chosen language; absent or "" = the browser's. */
export const LANGUAGE_KEY = 'uiLanguage';

/** _locales directory, BCP 47 tag (the main app's code), own name. */
export const UI_LANGUAGES = Object.freeze([
  Object.freeze({ code: 'zh_CN', bcp47: 'zh-CN', name: '简体中文' }),
  Object.freeze({ code: 'en', bcp47: 'en', name: 'English' }),
  Object.freeze({ code: 'zh_TW', bcp47: 'zh-TW', name: '繁體中文（台灣）' }),
  Object.freeze({ code: 'zh_HK', bcp47: 'yue-Hant-HK', name: '粵語（香港）' }),
  Object.freeze({ code: 'ru', bcp47: 'ru', name: 'Русский' }),
  Object.freeze({ code: 'hi', bcp47: 'hi', name: 'हिन्दी' }),
]);

export function isLanguage(code) {
  return UI_LANGUAGES.some((l) => l.code === code);
}

/** The _locales directories to read for a language, most specific first. */
export function chainOf(code) {
  if (!isLanguage(code)) return [];
  if (code === 'en') return ['en'];
  if (code === 'zh_HK') return ['zh_HK', 'zh_TW', 'en'];
  return [code, 'en'];
}

export function bcp47Of(code) {
  const l = UI_LANGUAGES.find((x) => x.code === code);
  return l ? l.bcp47 : '';
}

/**
 * One messages.json entry with its substitutions, as getMessage does it:
 * "$name$" becomes the placeholder's content (names are case-insensitive),
 * then "$1".."$9" the substitutions (a missing one is empty), "$$" a "$".
 */
export function formatMessage(entry, subs) {
  if (!entry || typeof entry.message !== 'string') return '';
  const ph = {};
  for (const [k, v] of Object.entries(entry.placeholders || {})) ph[k.toLowerCase()] = String((v && v.content) ?? '');
  const list = subs === undefined || subs === null ? [] : Array.isArray(subs) ? subs : [subs];
  const named = entry.message.replace(/\$([A-Za-z0-9_@]+)\$/g, (m, name) => (Object.hasOwn(ph, name.toLowerCase()) ? ph[name.toLowerCase()] : m));
  return named.replace(/\$(\$|[1-9])/g, (m, d) => (d === '$' ? '$' : list[Number(d) - 1] === undefined ? '' : String(list[Number(d) - 1])));
}

/**
 * The message key for "N tracks found": _locales has no plural rules, so
 * exactly one track takes its own key (English "1 track found").
 */
export function tracksFoundKey(n) {
  return Number(n) === 1 ? 'tracksDetectedOne' : 'tracksDetected';
}

/**
 * The message key for the options page's "<host> · N tracks" row, chosen
 * by count the same way (English "example.org · 1 track").
 */
export function reportRowKey(n) {
  return Number(n) === 1 ? 'reportRowOne' : 'reportRow';
}

/** The catalogs of a chain (most specific first) as one: the first that has a key wins. */
export function mergeCatalogs(catalogs) {
  const out = {};
  for (const c of [...catalogs].reverse()) {
    if (!c || typeof c !== 'object') continue;
    for (const [k, v] of Object.entries(c)) if (v && typeof v.message === 'string') out[k] = { message: v.message, ...(v.placeholders ? { placeholders: v.placeholders } : {}) };
  }
  return out;
}

/**
 * Reads a chosen language's catalog: load(dir) returns the parsed
 * messages.json of a _locales directory (or throws). Null when nothing is
 * chosen, or nothing could be read (the browser's choice applies then).
 */
export async function loadCatalog(code, load) {
  const chain = chainOf(code);
  if (chain.length === 0) return null;
  const parts = [];
  for (const dir of chain) {
    try {
      parts.push(await load(dir));
    } catch {
      parts.push(null);
    }
  }
  if (!parts[0]) return null;
  return { lang: bcp47Of(code), messages: mergeCatalogs(parts) };
}
