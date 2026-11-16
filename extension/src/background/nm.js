// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The one Native Messaging client of the background (src/lib/nmclient.js
// does the work). connectNative runs only when a request needs it.

import { browser, BUILD, extensionVersion, browserName, uiLocale, portError } from './env.js';
import { NmClient, HOST_NAME, connectionView } from '../lib/nmclient.js';

let client = null;
// The e2e build lets the test put a fake LectureFlow behind the port (no
// registry, no real host), and never opens a real port: until a test puts
// its fake there, there is no LectureFlow (as Chrome says when no host is
// registered). Otherwise, on a computer with LectureFlow installed, the
// test browser would find the user's own registration (Chrome for Testing
// falls back to SOFTWARE\Google\Chrome\NativeMessagingHosts; Firefox reads
// Mozilla's) and talk to the user's running LectureFlow. The real host is
// test/native's, with the store build. The store builds never read this.
let testConnect = null;
const noHost = () => {
  throw new Error('Specified native messaging host not found.');
};

export function nm() {
  if (!client) {
    client = new NmClient({
      connect: () => (BUILD.e2e ? (testConnect || noHost)(HOST_NAME) : browser.runtime.connectNative(HOST_NAME)),
      disconnectError: portError,
      // read at every hello: the interface language may change in between
      extension: {
        version: extensionVersion(),
        browser: browserName(),
        get locale() {
          return uiLocale();
        },
      },
    });
  }
  return client;
}

/** e2e build only: replaces connectNative and drops the current connection. */
export function setTestConnect(fn) {
  if (!BUILD.e2e) return;
  testConnect = fn;
  if (client) client.close();
}

/**
 * hello (or a fresh one: recheck), as the popup shows it. noLaunch for a
 * passive query (src/lib/nmclient.js hello()); a fresh one with noLaunch
 * looks again without starting LectureFlow.
 */
export async function connection(recheck = false, { noLaunch = false } = {}) {
  const c = nm();
  try {
    let reply;
    if (recheck && noLaunch) {
      c.close();
      reply = await c.hello({ noLaunch: true });
    } else reply = recheck ? await c.recheck() : await c.hello({ noLaunch });
    return connectionView('connected', reply, null);
  } catch (e) {
    return { status: 'unavailable', reason: e && e.code ? e.code : 'ext.host_unavailable', params: e && e.params ? { reason: e.params.reason || '' } : {} };
  }
}
