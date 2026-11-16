// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// SEC9 new-low-1: provider_rules.js's sameOrigin on the pairs of base_urls
// web/provider_origin_test.go sends on stdin (JSON [[a, b], ...]); prints
// the answers as JSON [true, false, ...] for the test to hold against
// settings.SameOrigin. Started with the web directory as a file: URL.

import { readFileSync } from 'node:fs';

const base = process.argv[2];
const R = await import(new URL('provider_rules.js', base).href);
const pairs = JSON.parse(readFileSync(0, 'utf8'));
process.stdout.write(JSON.stringify(pairs.map(([a, b]) => R.sameOrigin(a, b))));
