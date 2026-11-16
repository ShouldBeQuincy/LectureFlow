// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The six interface languages (SPEC-ext 界面设计, M4): every _locales file
// has the same keys and placeholders as en, the store fields keep within
// the stores' limits, the traditional Chinese files hold no simplified
// characters, buttons quoted in a sentence are named as the buttons are;
// and the settings' language choice formats messages as getMessage does.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { UI_LANGUAGES, isLanguage, chainOf, bcp47Of, formatMessage, mergeCatalogs, loadCatalog, tracksFoundKey, reportRowKey } from '../../src/lib/i18n.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const read = (dir) => JSON.parse(fs.readFileSync(path.join(root, '_locales', dir, 'messages.json'), 'utf8'));
const DIRS = fs.readdirSync(path.join(root, '_locales')).sort();
const en = read('en');
const used = (m) => [...new Set([...m.matchAll(/\$([A-Za-z0-9_@]+)\$/g)].map((x) => x[1].toLowerCase()))].sort();

test('six languages, the same as LectureFlow: one _locales directory each, default en', () => {
  assert.deepEqual(DIRS, ['en', 'hi', 'ru', 'zh_CN', 'zh_HK', 'zh_TW']);
  assert.deepEqual(UI_LANGUAGES.map((l) => l.code).sort(), DIRS);
  assert.deepEqual(UI_LANGUAGES.map((l) => l.bcp47), ['zh-CN', 'en', 'zh-TW', 'yue-Hant-HK', 'ru', 'hi'], 'web/i18n.js LOCALES, same order');
  assert.deepEqual(UI_LANGUAGES.map((l) => l.name), ['简体中文', 'English', '繁體中文（台灣）', '粵語（香港）', 'Русский', 'हिन्दी'], 'each in its own name, as in LectureFlow');
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'manifest.base.json'), 'utf8'));
  assert.equal(manifest.default_locale, 'en');
});

for (const dir of DIRS) {
  test(`${dir}: every key of en, placeholders as in en, nothing empty or untranslated by accident`, () => {
    const m = read(dir);
    assert.deepEqual(Object.keys(m), Object.keys(en), 'same keys, same order');
    for (const [k, v] of Object.entries(m)) {
      assert.equal(typeof v.message, 'string', k);
      assert.ok(v.message.trim().length > 0, `${k} is empty`);
      assert.deepEqual(used(v.message), used(en[k].message), `${k}: placeholders used`);
      assert.deepEqual(v.placeholders || null, en[k].placeholders || null, `${k}: placeholder declarations`);
      for (const name of used(v.message)) assert.ok(v.placeholders && Object.keys(v.placeholders).some((p) => p.toLowerCase() === name), `${k}: $${name}$ declared`);
      assert.ok(!/\s{2,}/.test(v.message.trim()), `${k}: double space`);
    }
    if (dir !== 'en' && !dir.startsWith('zh')) {
      // copied English would show up as identical prose (names and formats excepted)
      const same = Object.keys(m).filter((k) => m[k].message === en[k].message && /[a-z]{4,} [a-z]{3,}/.test(en[k].message));
      assert.deepEqual(same, [], `${dir}: still English`);
    }
  });

  test(`${dir}: store fields within the limits (name 45 for every store, Chrome 75; description 132)`, () => {
    const m = read(dir);
    const len = (s) => [...s].length;
    assert.ok(len(m.extName.message) <= 45, `extName ${len(m.extName.message)}`);
    assert.ok(m.extDescription.message.length <= 132, `extDescription ${m.extDescription.message.length} UTF-16 units`);
    assert.ok(len(m.extDescription.message) <= 132, `extDescription ${len(m.extDescription.message)}`);
    assert.ok(!/\b(Chrome|Firefox|Edge)\b/.test(m.extDescription.message), 'no browser named (the stores share the text)');
    assert.ok(len(m.actionTitle.message) <= 75, 'toolbar tooltip');
  });
}

test('traditional Chinese (zh_TW, Cantonese zh_HK): no simplified characters', () => {
  // Simplified forms that differ from their traditional ones, among the
  // characters these texts use.
  const SIMPLIFIED = '这个们为会时后发录设网页条节课讲视频载导择选确认连线检测应请来过对开关闭务击户区单项资讯问题说读码库删辑显标贴图层签环态处议际级数据状获储报帮从样无么还进动启选适应与专业历发达门间边围场备复实将尽东动线终练组经结给绝统总纪纽纸约细织红纳纯纲纷纵纹驳顺须预领频题颜风飞饭马驱验鱼鸟麦黄齐龙铁银错键长闪队阶随隐难电';
  for (const dir of ['zh_TW', 'zh_HK']) {
    const m = read(dir);
    for (const [k, v] of Object.entries(m)) {
      const bad = [...v.message].filter((ch) => SIMPLIFIED.includes(ch));
      assert.deepEqual(bad, [], `${dir}.${k}: ${v.message}`);
    }
  }
  // and the simplified file stays simplified where the two differ
  assert.ok(read('zh_CN').sendButton.message.includes('发送'));
});

test('a button named inside a sentence is named exactly as the button', () => {
  const pairs = [
    ['discNotInstalled', 'connRecheck'],
    ['discNotRunning', 'connRecheck'],
    ['inpageNote', 'confirmSend'],
    ['refreshText', 'refreshButton'],
    ['profilesHint', 'unreviewed'],
    ['oneEach', 'presetDual'],
    ['discNotOpen', 'openApp'],
    ['discNotOpen', 'connRecheck'],
    ['failAngle', 'presetMain'],
    ['bulkRefreshNote', 'sendButton'],
    ['discRegister', 'connRecheck'],
    ['courseCreate', 'connRecheck'],
  ];
  for (const dir of DIRS) {
    const m = read(dir);
    for (const [sentence, button] of pairs) assert.ok(m[sentence].message.includes(m[button].message), `${dir}: ${sentence} quotes ${button} “${m[button].message}”`);
  }
});

test('generic detection’s label on the card is not the footer’s “Detect automatically on this site” (SEC42 低-7)', () => {
  assert.ok(!/automatic/i.test(en.sourceGeneric.message), en.sourceGeneric.message);
  const zh = read('zh_CN');
  assert.ok(!zh.grantSite.message.includes(zh.sourceGeneric.message), zh.sourceGeneric.message);
});

test('generic detection’s label and the footer’s “Detect automatically on this site” share no word in any language; zh_CN and en say the same (I61, I66)', () => {
  // Chinese is written without spaces: no two characters in a row in common;
  // the others: no word of four letters or more in common.
  const words = (s) => s.toLowerCase().split(/[\s.,;:!?()«»“”"'·]+/u).filter((w) => [...w].length >= 4);
  const pairs = (s) => [...s].slice(1).map((ch, i) => [...s][i] + ch).filter((p) => /^\p{Script=Han}{2}$/u.test(p));
  for (const dir of DIRS) {
    const m = read(dir);
    const a = m.sourceGeneric.message;
    const b = m.grantSite.message;
    const shared = dir.startsWith('zh') ? pairs(a).filter((p) => b.includes(p)) : words(a).filter((w) => words(b).includes(w));
    assert.deepEqual(shared, [], `${dir}: “${a}” / “${b}”`);
  }
  // one meaning in the two finished languages: “found on the page”
  assert.equal(en.sourceGeneric.message, 'Found on the page');
  assert.equal(read('zh_CN').sourceGeneric.message, '在页面上找到');
});

test('the report’s two descriptions say what the redaction does to addresses: the path is kept, long IDs in it become fakes, parameter values placeholders; read it before sharing (R83 信息 1)', () => {
  // the old wording promised more than src/lib/redact.js does: path segments
  // such as a course code stay as they are
  const old = { en: /path structure/i, zh_CN: /路径结构/, zh_TW: /路徑結構/, zh_HK: /路徑結構/, ru: /структура пути/i, hi: /पाथ की संरचना/ };
  const fakes = { en: /fakes of the same shape/, zh_CN: /同形的假值/, zh_TW: /格式相同的假值/, zh_HK: /格式一樣嘅假值/, ru: /фиктивными значениями того же вида/, hi: /नकली मानों/ };
  const before = { en: /[Rr]ead it before you share it\./, zh_CN: /分享前请先看一遍。/, zh_TW: /分享前請先看過一遍。/, zh_HK: /分享之前請先睇一次。/, ru: /просмотрите его\./, hi: /शेयर करने से पहले इसे एक बार पढ़ लें।/ };
  for (const dir of DIRS) {
    const m = read(dir);
    for (const key of ['exportReportHint', 'reportsHint']) {
      const s = m[key].message;
      assert.ok(!old[dir].test(s), `${dir}.${key} still says “path structure”: ${s}`);
      assert.ok(fakes[dir].test(s), `${dir}.${key}: long IDs in the path become fakes`);
      assert.ok(before[dir].test(s), `${dir}.${key}: read it before sharing`);
    }
  }
  // ru: «поддельный» reads as "forged" in a privacy note; the reviewer's «фиктивными» (I73, I77)
  for (const key of ['exportReportHint', 'reportsHint']) assert.ok(!/поддельн/i.test(read('ru')[key].message), `ru.${key} still says «поддельн…»`);
});

test('the terms LectureFlow’s own translations use (glossary §9, web/i18n): 刮削, the DRM sentence, the presets', () => {
  const m = Object.fromEntries(DIRS.map((d) => [d, read(d)]));
  // fetch (刮削) is never "download" in any language
  const download = { en: /download/i, zh_CN: /下载/, zh_TW: /下載/, zh_HK: /下載/, ru: /скач|загруз/i, hi: /डाउनलोड/ };
  for (const d of DIRS) for (const [k, v] of Object.entries(m[d])) if (k !== 'getLectureFlow') assert.ok(!download[d].test(v.message), `${d}.${k}: ${v.message}`);
  assert.equal(m.zh_TW.state_downloading.message, '擷取中');
  assert.equal(m.zh_HK.state_downloading.message, '擷取緊');
  assert.equal(m.ru.state_downloading.message, 'Получение');
  assert.equal(m.hi.state_downloading.message, 'फ़ेच जारी');
  // the DRM sentence is LectureFlow's errors.downloads.drm_protected, word for word (flat dotted keys)
  const web = (l) => JSON.parse(fs.readFileSync(path.join(root, '..', 'web', 'i18n', `${l}.json`), 'utf8'));
  const drm = (l) => web(l)['errors.downloads.drm_protected'];
  for (const [dir, l] of [['zh_CN', 'zh-CN'], ['en', 'en'], ['zh_TW', 'zh-TW'], ['zh_HK', 'yue-Hant-HK'], ['ru', 'ru'], ['hi', 'hi']]) {
    assert.equal(m[dir].blockedProtected.message, drm(l), dir);
  }
  // the presets LectureFlow's zh-TW and yue texts already quote (errors.downloads.angle_no_video)
  assert.ok(web('zh-TW')['errors.downloads.angle_no_video'].includes(`「${m.zh_TW.presetMain.message}」`));
  assert.ok(web('yue-Hant-HK')['errors.downloads.angle_no_video'].includes(`「${m.zh_HK.presetMain.message}」`));
});

test('formatMessage substitutes as getMessage does', () => {
  const e = { message: '$P1$% · $P2$ / $P3$', placeholders: { p1: { content: '$1' }, p2: { content: '$2' }, p3: { content: '$3' } } };
  assert.equal(formatMessage(e, ['42', '1 MB', '2 MB']), '42% · 1 MB / 2 MB');
  assert.equal(formatMessage(e, ['42']), '42% ·  / ', 'a missing substitution is empty');
  assert.equal(formatMessage({ message: 'Price: $$5 for $Who$', placeholders: { who: { content: '$1' } } }, 'you'), 'Price: $5 for you');
  assert.equal(formatMessage({ message: '$UNKNOWN$ stays' }), '$UNKNOWN$ stays');
  assert.equal(formatMessage({ message: '$P1$ and $P1$', placeholders: { P1: { content: '$1' } } }, [7]), '7 and 7', 'names are case-insensitive, numbers become text');
  assert.equal(formatMessage(undefined, ['x']), '');
  assert.equal(formatMessage({ message: 'x' }, null), 'x');
  // every real message formats to text without a leftover $
  for (const dir of DIRS) for (const [k, v] of Object.entries(read(dir))) assert.ok(!formatMessage(v, ['1', '2', '3']).includes('$'), `${dir}.${k}`);
});

test('chains, codes and loading the chosen language', async () => {
  assert.deepEqual(chainOf('zh_HK'), ['zh_HK', 'zh_TW', 'en'], 'Cantonese → Traditional Chinese → English, as web/i18n.js');
  assert.deepEqual(chainOf('ru'), ['ru', 'en']);
  assert.deepEqual(chainOf('en'), ['en']);
  assert.deepEqual(chainOf('xx'), []);
  assert.equal(isLanguage('zh_HK'), true);
  assert.equal(isLanguage(''), false);
  assert.equal(isLanguage('__proto__'), false);
  assert.equal(bcp47Of('zh_HK'), 'yue-Hant-HK');
  const merged = mergeCatalogs([{ a: { message: 'A-hk' } }, { a: { message: 'A-tw' }, b: { message: 'B-tw' } }, { a: { message: 'A' }, b: { message: 'B' }, c: { message: 'C' } }]);
  assert.deepEqual(Object.fromEntries(Object.entries(merged).map(([k, v]) => [k, v.message])), { a: 'A-hk', b: 'B-tw', c: 'C' });
  const reads = [];
  const cat = await loadCatalog('zh_HK', async (dir) => {
    reads.push(dir);
    return read(dir);
  });
  assert.deepEqual(reads, ['zh_HK', 'zh_TW', 'en']);
  assert.equal(cat.lang, 'yue-Hant-HK');
  assert.equal(formatMessage(cat.messages.tracksDetected, ['3']), '偵測到 3 條軌道');
  // nothing chosen, or the language's own file unreadable: the browser decides
  assert.equal(await loadCatalog('', read), null);
  assert.equal(await loadCatalog('ru', async (dir) => (dir === 'en' ? read('en') : Promise.reject(new Error('404')))), null);
  // a missing fallback only loses the fallback
  const partial = await loadCatalog('zh_HK', async (dir) => (dir === 'zh_TW' ? Promise.reject(new Error('404')) : read(dir)));
  assert.equal(partial.messages.sendButton.message, read('zh_HK').sendButton.message);
});

test('the store name is the in-page button (user decision 2026-10-27); the quiet restricted notice is the user’s text without its last sentence', () => {
  for (const dir of DIRS) {
    const m = read(dir);
    assert.equal(m.extName.message, m.sendButton.message, `${dir}: the name says what the button says`);
    // When the reminder is off for a platform, "发送前会再提醒一次" would no
    // longer be true: the same text, cut before that last sentence only.
    const notice = m.restrictedNotice.message;
    const quiet = m.restrictedNoticeQuiet.message;
    assert.ok(notice.startsWith(quiet) && quiet.length > notice.length / 2, dir);
    const dropped = notice.slice(quiet.length).trim();
    assert.ok(dropped.length > 0 && !/[。.।]\s*\S/.test(dropped.replace(/[。.।]\s*$/, '')), `${dir}: one sentence dropped: ${dropped}`);
    assert.match(m.restrictedRemember.message, /\$P1\$/, `${dir}: the platform's name in the check box`);
  }
  assert.equal(read('en').extName.message, 'Send to LectureFlow');
  assert.equal(read('zh_CN').extName.message, '发送至 LectureFlow');
  assert.equal(read('zh_CN').restrictedNoticeQuiet.message, '检测到该媒体流或受讲师或大学政策限制。LectureFlow 仅将您有权访问的媒体流拉取至本地用于个人无障碍学习（转录/翻译）。请确保您遵守学校规定。');
  assert.equal(read('zh_CN').restrictedRemember.message, '对这个平台（$P1$）不再提醒');
});

test('the track count header is singular for one track (T081)', () => {
  assert.equal(tracksFoundKey(1), 'tracksDetectedOne');
  for (const n of [0, 2, 3, 11, '2']) assert.equal(tracksFoundKey(n), 'tracksDetected');
  assert.equal(formatMessage(en[tracksFoundKey(1)], ['1']), '1 track found');
  assert.equal(formatMessage(en[tracksFoundKey(3)], ['3']), '3 tracks found');
  assert.equal(formatMessage(en[tracksFoundKey(0)], ['0']), '0 tracks found');
});

test('the options page report row is singular for one track', () => {
  assert.equal(reportRowKey(1), 'reportRowOne');
  for (const n of [0, 2, 3, 11, '2']) assert.equal(reportRowKey(n), 'reportRow');
  assert.equal(formatMessage(en[reportRowKey(1)], ['example.org', '1']), 'example.org · 1 track');
  assert.equal(formatMessage(en[reportRowKey(4)], ['example.org', '4']), 'example.org · 4 tracks');
  for (const dir of ['en', 'hi', 'ru', 'zh_CN', 'zh_HK', 'zh_TW']) {
    const c = read(dir);
    assert.ok(c.reportRowOne, dir);
    assert.deepEqual(c.reportRowOne.placeholders, c.reportRow.placeholders, dir);
    const one = formatMessage(c.reportRowOne, ['example.org', '1']);
    assert.ok(one.includes('example.org') && one.includes('1'), `${dir}: ${one}`);
  }
});
