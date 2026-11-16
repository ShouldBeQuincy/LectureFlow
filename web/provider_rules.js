// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Provider rules shared by the settings page and onboarding: the limit
// ranges and the DeepSeek preset's limits (perf-llm), and whether a
// base_url edit moves a provider to another host (sec2-app L2: the back end
// then clears the stored key).

// Ranges of internal/settings (MaxConcurrencyLimit, MaxRPM); rpm 0 = no
// per-minute limit.
export const MAX_CONCURRENCY = 512;
export const MAX_RPM = 10000;

// DEEPSEEK_LIMITS are the DeepSeek preset's limits (packaging/components
// manifest, config.example.toml; web/perf_llm_test.go keeps them in step):
// DeepSeek limits concurrent requests per account and has no per-minute
// limit.
export const DEEPSEEK_LIMITS = Object.freeze({ max_concurrency: 64, rpm: 0 });

// OLD_LIMITS are the conservative presets of earlier versions (4 / 60, then
// 8 / 300). A DeepSeek provider at or below them is offered the new ones.
export const OLD_LIMITS = Object.freeze({ max_concurrency: 8, rpm: 300 });

// v3.2.x: the limits a new provider's dialog starts with, by type. An
// Anthropic account's limits depend on its usage tier, and a per-minute
// limit set here only slows the requests down (the API answers 429 when it
// is over, and the client lowers its concurrency): a few concurrent
// requests and no per-minute limit. Other types keep the earlier 4 / 60.
export const ANTHROPIC_LIMITS = Object.freeze({ max_concurrency: 4, rpm: 0 });
export const DEFAULT_LIMITS = Object.freeze({ max_concurrency: 4, rpm: 60 });

export function limitsPrefill(type) {
  return type === 'anthropic' ? ANTHROPIC_LIMITS : DEFAULT_LIMITS;
}

// The official page on Anthropic's rate limits (checked 2026-11-11; the
// older docs.anthropic.com address redirects here).
export const ANTHROPIC_RATE_LIMITS_URL = 'https://platform.claude.com/docs/en/api/rate-limits';

// Ranges of the v3.2.x role and general settings (internal/settings
// MaxTokensLimit, MaxBatch; model.DefaultLLMIdleTimeoutS; the idle timeout's
// bounds of the settings check, MinLLMIdleTimeoutS and MaxLLMIdleTimeoutS:
// 0 = off, else MIN..MAX). web/settings_v32x_test.go keeps them in step.
export const MAX_TOKENS_LIMIT = 1000000;
export const MAX_BATCH = 20;
export const DEFAULT_IDLE_TIMEOUT_S = 90;
export const MIN_IDLE_TIMEOUT_S = 30;
export const MAX_IDLE_TIMEOUT_S = 3600;

// v3.2.x P2 (user 2026-11-11; Claude models only after review R24): the
// translate batch an empty role setting means. The provider view's
// default_batch_claude (settings.ClaudeBatch: 10 on an Anthropic provider
// that is not DeepSeek, 1 otherwise) applies to a Claude model, a name
// containing "claude" in any case (settings.IsClaudeModel); every other
// model is 1. web/batch_default_test.go runs this and settings.DefaultBatch
// on the same cases.
export function isClaudeModel(model) {
  return String(model || '').toLowerCase().includes('claude');
}
export function defaultBatch(prov, model) {
  const n = prov && Number.isInteger(prov.default_batch_claude) ? prov.default_batch_claude : 1;
  return n > 1 && isClaudeModel(model) ? n : 1;
}

// thinkingFloorsAtLow: the model cannot switch thinking off; "off" runs at
// the lowest level (effort low). Kept the same as anthropicAlwaysThinks in
// internal/llm/thinking.go, which decides what is sent: change one, change
// the other. There, after trimming and lower-casing: claude-opus-5-5 itself
// or followed by "-", "@" or "." (anthropicModelIs: dated, -latest, @…),
// and every id starting "claude-fable-" or "claude-mythos-" (R35 S3, per
// Anthropic's documentation). Not claude-opus-5, Sonnet 5 or Haiku 5.5
// (they can switch it off; Sonnet 5.5 thinks between tools), and nothing
// the back end would not run at effort low (a bare "claude-fable",
// "claude-mythos:beta", "claude-opus-5-5:…"): better no note than a wrong one.
export function thinkingFloorsAtLow(model) {
  return /^claude-(?:opus-5-5(?:$|[-@.])|fable-|mythos-)/.test(String(model || '').trim().toLowerCase());
}

function parse(u) {
  try {
    return new URL(String(u || '').trim());
  } catch {
    return null;
  }
}

const defaultPort = (scheme) => (scheme === 'https' ? '443' : scheme === 'http' ? '80' : '');

// ASCII letters A–Z only (db.BaseURLOrigin's asciiLower; no Unicode mapping).
const asciiLower = (s) => s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));

// Go's strings.TrimSpace (unicode.IsSpace), which BaseURLOrigin uses: also
// U+0085, not U+FEFF (String.prototype.trim is the other way round).
const goTrim = (s) =>
  s.replace(/^[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]+/, '')
    .replace(/[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]+$/, '');

// A byte outside ASCII, written as is or percent-encoded.
const NON_ASCII = /[^\x00-\x7f]|%[89a-fA-F][0-9a-fA-F]/;

const utf8 = new TextEncoder();

// goUnescape is net/url's unescape: the bytes s stands for, or null where Go
// refuses it (a "%" without two hex digits; in a host, an escaped ASCII byte
// other than %25, or an ASCII character a host cannot have).
function goUnescape(s, host) {
  const out = [];
  for (let i = 0; i < s.length; ) {
    if (s[i] === '%') {
      const hex = s.slice(i + 1, i + 3);
      if (!/^[0-9a-fA-F]{2}$/.test(hex)) return null;
      const v = parseInt(hex, 16);
      if (host && v < 0x80 && hex !== '25') return null;
      out.push(v);
      i += 3;
      continue;
    }
    const ch = String.fromCodePoint(s.codePointAt(i));
    if (host && ch < '\x80' && !/[A-Za-z0-9\-._~!$&'()*+,;=:\]<>"]/.test(ch)) return null;
    out.push(...utf8.encode(ch));
    i += ch.length;
  }
  return out;
}

// goHost is what db.BaseURLOrigin reads from text (trimmed) with Go's
// url.Parse: {scheme, host (bytes), port}, or null where it is "raw:" (Go
// refuses the URL, or it has no host). Only for text with a byte outside
// ASCII; an IP literal with one is left "raw:" (never a weaker check: a
// "raw:" origin equals only the same text, and then so does Go's).
function goHost(text) {
  if (/[\x00-\x1f\x7f]/.test(text)) return null;
  const hash = text.indexOf('#');
  const u = hash < 0 ? text : text.slice(0, hash);
  if (hash >= 0 && !goUnescape(text.slice(hash + 1), false)) return null;
  const sm = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(u);
  if (!sm) return null; // no scheme: no http(s) base_url either
  const scheme = asciiLower(sm[1]);
  let rest = u.slice(sm[0].length);
  const q = rest.indexOf('?');
  if (q >= 0) rest = rest.slice(0, q);
  if (!rest.startsWith('//')) return null; // opaque, or no authority
  let auth = rest.slice(2);
  let path = '';
  const slash = auth.indexOf('/');
  if (slash >= 0) [auth, path] = [auth.slice(0, slash), auth.slice(slash)];
  if (!goUnescape(path, false)) return null;
  const at = auth.lastIndexOf('@');
  if (at >= 0) {
    const user = auth.slice(0, at);
    if (!/^[A-Za-z0-9\-._:~!$&'()*+,;=%@]*$/.test(user) || !goUnescape(user, false)) return null;
  }
  let host = auth.slice(at + 1);
  if (host === '' || host.includes('[')) return null;
  let port = '';
  const first = host.indexOf(':');
  if (first >= 0) {
    const last = host.lastIndexOf(':');
    // http(s): one colon at most (Go's strict colons); else the last one
    const i = last !== first && scheme !== 'http' && scheme !== 'https' ? last : first;
    if (!/^:[0-9]*$/.test(host.slice(i))) return null;
    port = host.slice(last + 1);
    host = host.slice(0, last);
  }
  const bytes = goUnescape(host, true);
  return bytes ? { scheme, host: bytes, port } : null;
}

function originOf(u) {
  const text = goTrim(String(u || ''));
  // db.BaseURLOrigin (SEC8 low-1): a host with a byte outside ASCII (as
  // written or percent-encoded) is "idn:" + scheme, its bytes and the port,
  // equal only to the same bytes. new URL() would map it by IDNA first
  // ("Bücher.example", "bücher.example" and "xn--bcher-kva.example" all
  // become the last; "api" with a full-width "a", U+FF41, becomes "api"), so the dialog saw
  // one origin where the back end sees two and clears the key (SEC9
  // new-low-1). Text with no such byte is judged as before.
  if (NON_ASCII.test(text)) {
    const g = goHost(text);
    if (!g) return 'raw:' + text;
    if (g.host.some((b) => b >= 0x80)) {
      // the bytes, ASCII as is but "%", the rest as %XX: one text per host
      const hex = (b) => '%' + b.toString(16).toUpperCase().padStart(2, '0');
      const h = g.host.map((b) => (b >= 0x80 || b === 0x25 ? hex(b) : String.fromCharCode(b))).join('');
      return `idn:${g.scheme}://${h}:${g.port || defaultPort(g.scheme)}`;
    }
  }
  const x = parse(text);
  if (!x || !x.host) return 'raw:' + text;
  const scheme = x.protocol.replace(/:$/, '').toLowerCase();
  const port = x.port || defaultPort(scheme);
  return `${scheme}://${x.hostname.toLowerCase()}:${port}`;
}

// sameOrigin mirrors settings.SameOrigin (db.BaseURLOrigin): scheme, host
// and port; a host with a byte outside ASCII equals only the same bytes
// (originOf), so such an edit after which the back end clears the key is
// one the dialog warns about (web/provider_origin_test.go).
export function sameOrigin(a, b) {
  return originOf(a) === originOf(b);
}

// isDeepSeekAPI: the provider talks to DeepSeek's own API (a relay may have
// limits of its own, so only the official host counts).
export function isDeepSeekAPI(p) {
  const x = p && parse(p.base_url);
  return !!x && x.hostname.toLowerCase() === 'api.deepseek.com';
}

// hasOldLimits: a DeepSeek provider still at (or below) the old
// conservative limits; the settings page suggests the preset's limits (it
// never changes them by itself).
export function hasOldLimits(p) {
  if (!isDeepSeekAPI(p)) return false;
  const rpm = Number(p.rpm) || 0;
  return Number(p.max_concurrency) <= OLD_LIMITS.max_concurrency || (rpm > 0 && rpm <= OLD_LIMITS.rpm);
}

// The provider's requests go to base_url — trailing slashes removed, as
// internal/llm does — followed by the type's path (/chat/completions,
// /v1/messages). The dialog's hint names that address as typed (3.2.0 L18:
// a literal "{base_url}" read as a placeholder left unfilled).
// endpointParts(text, mark, base): text is the hint with mark where
// {base_url} goes; returns [before, address, after], the address being base
// (or "https://…" while the field is empty) with the path the text writes
// right after it (a full stop that ends the sentence is not part of it);
// [text] when the text has no mark.
export function endpointParts(text, mark, base) {
  const at = text.indexOf(mark);
  if (at < 0) return [text];
  const after = text.slice(at + mark.length);
  const path = /^[\w/-]*(?:\.[\w-]+)*/.exec(after)[0];
  const b = String(base || '').trim().replace(/\/+$/, '') || 'https://…';
  return [text.slice(0, at), b + path, after.slice(path.length)];
}
