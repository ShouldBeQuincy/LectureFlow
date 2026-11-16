// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The main process's own calls to the service (/api/app/activity,
// /api/app/settings, /api/settings/ui, the update relay, /api/selfupdate,
// /api/app/path-grants),
// with the access token. No Electron imports.
'use strict';

const { TOKEN_HEADER } = require('./lib/security');
const { PATH_GRANT_HEADER, pathGrantSecret } = require('./lib/pathgrant');

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/**
 * createApiClient({ getServer }) where getServer() returns
 * { origin, token } of the running service, or null.
 */
function createApiClient({ getServer, fetchImpl = fetch }) {
  async function request(method, path, { body, timeoutMs = 3000, grant = false } = {}) {
    const srv = getServer();
    if (!srv || !srv.origin || !srv.token) throw new ApiError(0, 'service not running');
    const headers = { [TOKEN_HEADER]: srv.token };
    if (grant) headers[PATH_GRANT_HEADER] = pathGrantSecret(srv.token);
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetchImpl(`${srv.origin}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) throw new ApiError(res.status, `${method} ${path}: HTTP ${res.status}`);
    const text = await res.text();
    return text ? JSON.parse(text) : null;
  }
  return {
    activity: (opts) => request('GET', '/api/app/activity', opts),
    settings: (opts) => request('GET', '/api/app/settings', opts),
    uiSettings: (opts) => request('GET', '/api/settings/ui', opts),
    patchSettings: (patch, opts) => request('PATCH', '/api/app/settings', { ...opts, body: patch }),
    getUpdate: (opts) => request('GET', '/api/app/update', opts),
    // the main process's report: the grant header too (AUD1 I-1; the page's requests carry the token as well)
    postUpdateState: (body, opts) => request('POST', '/api/app/update/state', { ...opts, body, grant: true }),
    selfUpdate: (opts) => request('GET', '/api/selfupdate', opts),
    // one-time grants for the paths the dialog or a drop gave the page (lib/pathgrant.js)
    // opts.kind: 'file' (default) or 'folder' (the folder dialog's pick)
    grantPaths: (paths, { kind = 'file', ...opts } = {}) => request('POST', '/api/app/path-grants', { ...opts, body: { paths, kind }, grant: true }),
  };
}

module.exports = { createApiClient, ApiError };
