// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Page-response observation, the part that runs in the page's own JavaScript
// world (profile `observe`; src/lib/observe.js explains the design). It is
// registered only on sites the user authorized whose hosts belong to a
// profile that observes responses (src/background/sites.js), at
// document_start, before the page's own scripts.
//
// What it does: it wraps window.fetch and XMLHttpRequest so that, when a
// response comes back whose URL the profile declares, it reads that JSON
// and takes the values of the declared pointers — strings of at most 2048
// characters, numbers, booleans, null; never an object or array as a whole
// — and hands only those to the extension's content script
// (src/content/observe.js) in a DOM event. Requests and responses are
// passed to the page unchanged; nothing is sent anywhere; nothing is kept
// here. Which requests and pointers come from the content script (event
// "lectureflow-observe-config"); until then JSON responses wait at most 15
// seconds, and with no configuration the wrappers only pass through.
//
// A classic script with no extension APIs: it cannot import anything, so it
// carries its own copy of the pointer rules (tested against the same cases
// as src/lib/observe.js in test/unit/observe.test.mjs).

(() => {
  const CONFIG_EVENT = 'lectureflow-observe-config';
  const READY_EVENT = 'lectureflow-observe-ready';
  const RESULT_EVENT = 'lectureflow-observed';
  const WAIT_MS = 15000;
  const MAX_BODY = 4 * 1024 * 1024;
  const MAX_STRING = 2048;
  const MAX_ARRAY = 16;

  const win = window;
  const doc = document;
  // The runtime guard of the profile's regexes, as src/lib/regexguard.js (a
  // classic script cannot import it): a match over SLOW_MS, or matches adding
  // up to more than BUDGET_MS within WINDOW_MS, stop them for the rest of the
  // page. The page could fake this world's clock, so nothing is reported from
  // here: the content script is told which regex and address it was
  // (SLOW_EVENT), runs that once more under its own clock, and reports it
  // only if it is really slow (SEC18e 中-6).
  const SLOW_MS = 250;
  const BUDGET_MS = 1000;
  const WINDOW_MS = 60000;
  const SLOW_EVENT = 'lectureflow-observe-slow';
  const perf = win.performance;
  const perfNow = perf && perf.now ? perf.now.bind(perf) : Date.now;
  let reSpent = 0;
  let reWindow = null;
  let reStopped = false;
  // A regex's first match on fewer than 1000 characters runs in V8's
  // bytecode interpreter, 5 to 8 times slower: before a spec's first match
  // here it runs once on '', timed with that match, so a regex slow only on
  // '' trips the guard like any other (SEC22 L-A, SEC22b M-1).
  const guardedTest = (index, s) => {
    if (reStopped) return false;
    const t0 = perfNow();
    if (reWindow === null) reWindow = t0;
    else if (t0 - reWindow >= WINDOW_MS) {
      reSpent = 0;
      reWindow = t0;
    }
    const spec = config.specs[index];
    if (!spec.warm) {
      spec.warm = true;
      spec.re.test('');
    }
    const ok = config.specs[index].re.test(s);
    const ms = perfNow() - t0;
    reSpent += ms;
    if (ms > SLOW_MS || reSpent > BUDGET_MS) {
      reStopped = true;
      try {
        doc.dispatchEvent(new CE(SLOW_EVENT, { detail: JSON.stringify({ key: config.key, index, url: s }) }));
      } catch {
        // stopped here anyway
      }
    }
    return ok;
  };
  const origFetch = win.fetch;
  const XHR = win.XMLHttpRequest;
  const origOpen = XHR && XHR.prototype.open;
  const origSend = XHR && XHR.prototype.send;
  const CE = win.CustomEvent;
  const parse = JSON.parse;
  const stringify = JSON.stringify;
  const dispatch = (type, detail) => {
    try {
      doc.dispatchEvent(new CE(type, { detail }));
    } catch {
      // never disturb the page
    }
  };

  let state = 'waiting'; // waiting | ready | none
  let config = null; // { key, specs: [{ re, hosts, pointers: [[name, segments]] }] }
  const waiters = [];
  const settle = (s) => {
    if (state !== 'waiting') return;
    state = s;
    for (const w of waiters.splice(0)) w();
  };
  const configured = () => (state !== 'waiting' ? Promise.resolve() : new Promise((r) => waiters.push(r)));
  setTimeout(() => settle('none'), WAIT_MS);

  const parsePointer = (p) => {
    if (typeof p !== 'string' || p.charAt(0) !== '/') return null;
    const parts = p.slice(1).split('/');
    if (parts.length === 0 || parts.length > 8) return null;
    const out = [];
    for (const s of parts) {
      if (s === '') return null;
      if (s === '*') out.push({ any: true });
      else if (/~[^01]|~$/.test(s) || s.indexOf('*') >= 0) return null;
      else out.push({ key: s.replace(/~1/g, '/').replace(/~0/g, '~') });
    }
    return out;
  };

  const hostMatches = (pattern, host) => {
    const p = String(pattern).toLowerCase();
    const h = String(host).toLowerCase();
    if (p.indexOf('*.') === 0) return h === p.slice(2) || h.endsWith(p.slice(1));
    return h === p;
  };

  doc.addEventListener(CONFIG_EVENT, (ev) => {
    if (state !== 'waiting') return;
    try {
      const c = parse(String(ev.detail));
      if (!c || !Array.isArray(c.specs) || typeof c.key !== 'string') return settle('none');
      const specs = [];
      for (const s of c.specs.slice(0, 4)) {
        const pointers = [];
        for (const name of Object.keys(s.pointers || {}).slice(0, 8)) {
          const seg = parsePointer(s.pointers[name]);
          if (seg) pointers.push([name, seg]);
        }
        specs.push({ re: new RegExp(String(s.urlPattern), 'i'), hosts: (s.hosts || []).map(String), pointers }); // run only through guardedTest
      }
      config = { key: c.key, specs };
      settle(specs.length ? 'ready' : 'none');
    } catch {
      settle('none');
    }
  });
  dispatch(READY_EVENT, '');

  const absolute = (raw) => {
    try {
      const u = new URL(String(raw), location.href);
      return u.protocol === 'http:' || u.protocol === 'https:' ? u : null;
    } catch {
      return null;
    }
  };

  const specIndex = (url) => {
    if (state !== 'ready' || !url) return -1;
    const bare = `${url.protocol}//${url.host}${url.pathname}`;
    for (let i = 0; i < config.specs.length; i++) {
      const s = config.specs[i];
      if (s.hosts.some((h) => hostMatches(h, url.hostname)) && bare.length <= 1024 && guardedTest(i, bare)) return i; // MAX_REGEX_INPUT (src/lib/regexcost.js)
    }
    return -1;
  };

  const scalar = (v) =>
    v === null || typeof v === 'boolean' || (typeof v === 'number' && isFinite(v)) || (typeof v === 'string' && v.length <= MAX_STRING);

  const read = (obj, segments) => {
    let many = false;
    let cur = [obj];
    for (const s of segments) {
      const next = [];
      for (const c of cur) {
        if (s.any) {
          many = true;
          if (Array.isArray(c)) for (const x of c.slice(0, MAX_ARRAY)) next.push(x);
        } else if (c !== null && typeof c === 'object') {
          if (Array.isArray(c)) {
            if (/^(0|[1-9]\d*)$/.test(s.key) && Number(s.key) < c.length) next.push(c[Number(s.key)]);
          } else if (Object.prototype.hasOwnProperty.call(c, s.key)) next.push(c[s.key]);
        }
      }
      cur = next;
    }
    if (many) return cur.filter(scalar).slice(0, MAX_ARRAY);
    if (cur.length === 0) return undefined;
    return scalar(cur[0]) ? cur[0] : undefined;
  };

  // Takes the declared values out of a parsed response and hands only them on.
  const emit = (index, url, obj) => {
    const spec = config.specs[index];
    const values = {};
    for (const [name, seg] of spec.pointers) {
      const v = read(obj, seg);
      if (v !== undefined) values[name] = v;
    }
    dispatch(RESULT_EVENT, stringify({ key: config.key, index, url: `${url.protocol}//${url.host}${url.pathname}`, values }));
  };

  const fromText = (index, url, text) => {
    if (typeof text !== 'string' || text.length > MAX_BODY) return;
    let obj;
    try {
      obj = parse(text);
    } catch {
      return;
    }
    emit(index, url, obj);
  };

  const jsonish = (type) => /json|javascript|text\/plain/i.test(type || '');

  // fetch: our reaction is registered before the page's, so the clone is
  // made before the page reads the body.
  if (typeof origFetch === 'function') {
    const wrapped = function fetch(input, init) {
      const p = origFetch.apply(this, arguments);
      try {
        p.then(
          (res) => {
            try {
              if (state === 'none' || !res || !res.ok) return;
              const url = absolute(res.url || (input && input.url) || input);
              if (!url || (state === 'ready' && specIndex(url) < 0)) return;
              if (state === 'waiting' && !jsonish(res.headers.get('content-type'))) return;
              const len = Number(res.headers.get('content-length'));
              if (len > MAX_BODY) return;
              const copy = res.clone();
              configured().then(() => {
                const i = specIndex(url);
                if (i < 0) {
                  if (copy.body) copy.body.cancel().catch(() => {});
                  return;
                }
                copy.text().then((t) => fromText(i, url, t), () => {});
              });
            } catch {
              // never disturb the page
            }
          },
          () => {},
        );
      } catch {
        // never disturb the page
      }
      return p;
    };
    try {
      win.fetch = wrapped;
    } catch {
      // not writable here
    }
  }

  // XHR: the URL from open(), the body once loaded.
  if (XHR && typeof origOpen === 'function' && typeof origSend === 'function') {
    const urls = new WeakMap();
    XHR.prototype.open = function open(method, url) {
      try {
        urls.set(this, url);
      } catch {
        // never disturb the page
      }
      return origOpen.apply(this, arguments);
    };
    XHR.prototype.send = function send() {
      try {
        const xhr = this;
        if (state !== 'none') {
          xhr.addEventListener('load', () => {
            try {
              if (state === 'none' || xhr.status < 200 || xhr.status > 299) return;
              const url = absolute(xhr.responseURL || urls.get(xhr));
              if (!url || (state === 'ready' && specIndex(url) < 0)) return;
              configured().then(() => {
                try {
                  const i = specIndex(url);
                  if (i < 0) return;
                  const rt = xhr.responseType;
                  if (rt === '' || rt === 'text') fromText(i, url, xhr.responseText);
                  else if (rt === 'json' && xhr.response && typeof xhr.response === 'object') emit(i, url, xhr.response);
                } catch {
                  // never disturb the page
                }
              });
            } catch {
              // never disturb the page
            }
          });
        }
      } catch {
        // never disturb the page
      }
      return origSend.apply(this, arguments);
    };
  }
})();
