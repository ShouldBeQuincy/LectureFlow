// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The packaged Panopto profile's zh-CN hint for its audio track (user
// decision needs-human 1.20 a; T26 G13): "只要转写时最小" did not read, the
// other languages all say "smallest file; enough just to transcribe". A
// change to the packaged file raises its version by a patch, as 4d9caf67
// did for the labels (1.1.0 → 1.1.1).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const panopto = JSON.parse(fs.readFileSync(path.join(root, 'profiles', 'panopto.json'), 'utf8'));

test('Panopto’s audio track: the zh-CN hint says what the other languages say', () => {
  const audio = panopto.tracks.filter((t) => t.hint && t.hint['zh-TW'] === '檔案最小，轉錄用它就夠');
  assert.equal(audio.length, 1, 'the one track with that hint');
  assert.equal(audio[0].hint['zh-CN'], '文件最小，只转写用它就够');
  assert.equal(audio[0].hint.en, 'Smallest; enough to transcribe', 'en as before');
});

test('the packaged profile’s version moved on past 1.1.1', () => {
  const v = panopto.version.split('.').map(Number);
  assert.equal(v.length, 3);
  assert.ok(v[0] > 1 || (v[0] === 1 && (v[1] > 1 || (v[1] === 1 && v[2] > 1))), panopto.version);
});
