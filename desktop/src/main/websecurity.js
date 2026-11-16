// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Wires the decisions of lib/security.js into Electron (SPEC-v3 V1.2, V2):
// token injection, CSP, request and permission filtering on the window's
// session; navigation, redirect, window-open and webview guards on every
// webContents.
'use strict';

const {
  TOKEN_HEADER, classifyNavigation, shouldInjectToken, isRequestAllowed, isPermissionAllowed,
  isDownloadAllowed, withSecurityHeaders, isServerUrl,
} = require('./lib/security');
const { PATH_GRANT_HEADER } = require('./lib/pathgrant');

// Headers only the main process may send: a page that sets one of them has
// it dropped (the token is added back below; a path grant is sent by the
// main process's own calls only, apiclient.js, which use Node's fetch and
// never pass through this session; security review SEC24 L5).
const SHELL_ONLY_HEADERS = new Set([TOKEN_HEADER.toLowerCase(), PATH_GRANT_HEADER.toLowerCase()]);

function originOf(url) {
  try {
    const u = new URL(url);
    return u.origin === 'null' ? `${u.protocol}` : u.origin;
  } catch {
    return '(invalid url)';
  }
}

/**
 * hardenSession(ses, ctx) with ctx:
 *   getPort(), getToken()   the running service (null while it is not running)
 *   shellRoot              directory of the shell's own pages (splash)
 *   devTools               allow devtools: requests (development only)
 *   openExternal(url)      hand an allowed external link to the system
 *   log
 * The token lives only in this closure and in request headers sent to the
 * service; the page never sees it.
 */
function hardenSession(ses, ctx) {
  const { webRequest } = ses;

  webRequest.onBeforeRequest((details, callback) => {
    const ok = isRequestAllowed(details.url, ctx.getPort(), { shellRoot: ctx.shellRoot, devTools: ctx.devTools });
    if (!ok) ctx.log.warn(`blocked request to ${originOf(details.url)}`);
    callback({ cancel: !ok });
  });

  webRequest.onBeforeSendHeaders((details, callback) => {
    const headers = {};
    for (const [k, v] of Object.entries(details.requestHeaders)) {
      if (!SHELL_ONLY_HEADERS.has(k.toLowerCase())) headers[k] = v; // never forward a page-made token or path-grant header
    }
    const token = ctx.getToken();
    if (token && shouldInjectToken(details.url, ctx.getPort())) headers[TOKEN_HEADER] = token;
    callback({ requestHeaders: headers });
  });

  webRequest.onHeadersReceived((details, callback) => {
    if (!isServerUrl(details.url, ctx.getPort())) {
      callback({});
      return;
    }
    callback({ responseHeaders: withSecurityHeaders(details.responseHeaders) });
  });

  ses.setPermissionRequestHandler((wc, permission, callback, details) => {
    const url = (details && details.requestingUrl) || wc.getURL();
    if (permission === 'openExternal') {
      // A link to an external scheme (mailto:) followed in place: open it
      // ourselves when it is allowed, never let Chromium hand it over.
      const target = details && details.externalURL;
      if (isServerUrl(url, ctx.getPort()) && classifyNavigation(target, ctx.getPort()) === 'external' && ctx.openExternal) ctx.openExternal(target);
      callback(false);
      return;
    }
    const ok = isPermissionAllowed(permission, url, ctx.getPort());
    if (!ok) ctx.log.info(`denied permission ${permission}`);
    callback(ok);
  });
  ses.setPermissionCheckHandler((wc, permission, requestingOrigin) => isPermissionAllowed(permission, requestingOrigin, ctx.getPort()));
  ses.setDevicePermissionHandler(() => false);

  ses.on('will-download', (event, item) => {
    if (!isDownloadAllowed(item.getURL(), ctx.getPort())) {
      ctx.log.warn(`blocked download from ${originOf(item.getURL())}`);
      event.preventDefault();
    }
  });

  // Chromium would fetch dictionaries from the network; English course
  // terms would also be underlined everywhere.
  disableSpellcheck(ses);
}

/**
 * disableSpellcheck(ses): no spellchecker and no languages, so no Hunspell
 * dictionary is ever downloaded for the session. Both calls are needed: with
 * the sandbox on (app.enableSandbox), a session whose spellchecker is only
 * switched off still downloads the dictionary of the UI language from
 * Google's CDN (redirector.gvt1.com). setSpellCheckerLanguages is a no-op on
 * macOS (the system spellchecker downloads nothing).
 */
function disableSpellcheck(ses) {
  try {
    ses.setSpellCheckerEnabled(false);
  } catch {
    // an Electron without the API: nothing to switch off
  }
  try {
    ses.setSpellCheckerLanguages([]);
  } catch {
    // macOS, or an Electron without the API
  }
}

/**
 * noSpellcheckDownloads(app): every session gets disableSpellcheck as soon
 * as Electron creates it, the default session included, which no window of
 * the shell uses: with the sandbox on it downloaded en-US-10-1.bdic from
 * redirector.gvt1.com at every start (privacy audit before 3.2.0, NOTES.md
 * "无遥测"). Call before the app is ready.
 */
function noSpellcheckDownloads(app) {
  app.on('session-created', disableSpellcheck);
}

/**
 * hardenWebContents(contents, ctx) with ctx:
 *   getPort(), openExternal(url), childWindowOptions(), log
 */
function hardenWebContents(contents, ctx) {
  contents.on('will-attach-webview', (event) => event.preventDefault());

  contents.on('will-navigate', (event) => {
    const url = event.url;
    const verdict = classifyNavigation(url, ctx.getPort());
    if (verdict === 'allow') return;
    event.preventDefault();
    if (verdict === 'external') ctx.openExternal(url);
    else ctx.log.warn(`blocked navigation to ${originOf(url)}`);
  });

  contents.on('will-frame-navigate', (event) => {
    if (event.isMainFrame) return; // will-navigate above
    if (classifyNavigation(event.url, ctx.getPort()) !== 'allow') {
      event.preventDefault();
      ctx.log.warn(`blocked frame navigation to ${originOf(event.url)}`);
    }
  });

  contents.on('will-redirect', (event) => {
    if (classifyNavigation(event.url, ctx.getPort()) !== 'allow') {
      event.preventDefault();
      ctx.log.warn(`blocked redirect to ${originOf(event.url)}`);
    }
  });

  contents.setWindowOpenHandler(({ url }) => {
    const verdict = classifyNavigation(url, ctx.getPort());
    if (verdict === 'allow') {
      return { action: 'allow', overrideBrowserWindowOptions: ctx.childWindowOptions() };
    }
    if (verdict === 'external') ctx.openExternal(url);
    else ctx.log.warn(`blocked window.open to ${originOf(url)}`);
    return { action: 'deny' };
  });
}

module.exports = { hardenSession, hardenWebContents, disableSpellcheck, noSpellcheckDownloads };
