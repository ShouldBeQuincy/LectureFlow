// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Path grants (security audit FB1 L-7, T-SEC-08; internal/api/pathgrant.go).
// The page copies a local file into the app by sending its path; the
// service only accepts a path the main process registered as a one-time
// grant: the paths the system dialog returned, and the paths of a drop
// (the preload reads them with webUtils and passes them on). The grant is
// registered with the main process's own API client and the
// X-LF-Path-Grant header, an HMAC of the access token: the page's requests
// carry the token (added by the session), but the page never sees it and
// cannot make this header. No Electron imports.
'use strict';

const crypto = require('node:crypto');

const PATH_GRANT_HEADER = 'X-LF-Path-Grant';
const PATH_GRANT_LABEL = 'lectureflow path grant v1';
// One registration of the service takes at most 1000 paths; smaller
// requests keep each body well under its 1 MB limit even with long paths.
const PATH_GRANT_CHUNK = 200;

/** pathGrantSecret(token): the header value, hex HMAC-SHA256 of the label keyed with the token. */
function pathGrantSecret(token) {
  if (typeof token !== 'string' || token === '') throw new TypeError('pathGrantSecret: no token');
  return crypto.createHmac('sha256', token).update(PATH_GRANT_LABEL).digest('hex');
}

/**
 * createPathGranter({ api, log }) → async grant(paths, kind = 'file'):
 * registers the paths with api.grantPaths in chunks. kind 'file': paths the
 * page may copy by path (the file dialog, a drop); 'folder': the folder
 * dialog's pick, which the data directory endpoints need for every path,
 * local or on the network (security review SEC24 R1; the main process
 * grants every folder the dialog picks). Never throws: without a running
 * service (or on any error) the paths stay ungranted and the service
 * refuses them ("choose the file / folder again"); a log line says why.
 */
function createPathGranter({ api, log }) {
  if (!api || typeof api.grantPaths !== 'function') throw new TypeError('createPathGranter: api.grantPaths is required');
  return async function grantPaths(paths, kind = 'file') {
    if (kind !== 'file' && kind !== 'folder') throw new TypeError(`grantPaths: unknown kind ${kind}`);
    const list = Array.isArray(paths) ? paths.filter((p) => typeof p === 'string' && p !== '') : [];
    for (let i = 0; i < list.length; i += PATH_GRANT_CHUNK) {
      try {
        await api.grantPaths(list.slice(i, i + PATH_GRANT_CHUNK), { kind });
      } catch (e) {
        if (log) log.warn(`path grant not registered (${list.length - i} path(s)): ${e && e.message}`);
        return false;
      }
    }
    return true;
  };
}

module.exports = { PATH_GRANT_HEADER, PATH_GRANT_LABEL, PATH_GRANT_CHUNK, pathGrantSecret, createPathGranter };
