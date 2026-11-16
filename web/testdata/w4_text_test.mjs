// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 2 (W4), run in Node (web/w4_fixes_test.go starts it with the web
// directory as a file: URL), with the real catalogs of every UI language:
// - a compound warning (two coded parts, T8: the media was cut short and the
//   recording is very quiet) reads as two sentences in English and Russian
//   ("… again. The recording …", not "…; The recording …"), keeps the
//   clause separator in the languages without letter case, and no letter's
//   case is changed anywhere;
// - the provider's tariff summary starts with a capital (T8: ru opened it
//   with "пекинское время"), and is unchanged in scripts without case.
// Only strings are compared here (no DOM nodes).

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const base = process.argv[2];
const store = { getItem: () => null, setItem() {}, removeItem() {} };
const el = () => ({ dataset: {}, style: {}, classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, append() {}, addEventListener() {} });
Object.assign(globalThis, {
  location: { search: '', hash: '', pathname: '/', origin: 'http://127.0.0.1' },
  localStorage: store,
  sessionStorage: store,
  document: { documentElement: el(), body: el(), addEventListener() {}, querySelector: () => null, getElementById: () => null, createElement: el },
  addEventListener() {},
  dispatchEvent: () => true,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
});
globalThis.window = globalThis;
globalThis.fetch = async (url) => {
  const u = String(url);
  if (!u.startsWith('i18n/')) throw new Error(`unexpected request ${u}`);
  const cat = JSON.parse(await readFile(new URL(u, base), 'utf8'));
  return { ok: true, status: 200, json: async () => cat };
};

const I = await import(new URL('i18n.js', base).href);
const U = await import(new URL('util.js', base).href);
const T = await import(new URL('tariff_ui.js', base).href);

const truncated = { code: 'prep.media_truncated', params: { declared: '1:00:00', processed: '18:00', extra: [{ code: 'prep.low_volume' }] } };
const tariff = {
  timezone: 'Asia/Shanghai',
  peak_windows: [{ days: [1, 2, 3, 4, 5], from: '09:00', to: '12:00' }],
  offpeak_multiplier: 0.5,
  exclude_holidays: 'CN',
};

for (const loc of ['en', 'ru', 'hi', 'zh-CN', 'zh-TW', 'yue-Hant-HK']) {
  await I.setLocale(loc);
  assert.equal(I.getLocale(), loc);
  const sep = I.t('common.clause_sep');
  const main = I.t('warnings.prep.media_truncated', { declared: '1:00:00', processed: '18:00' });
  const extra = I.t('warnings.prep.low_volume');
  assert.ok(main && extra && !main.includes('{'), `${loc}: the two warnings have texts`);
  const text = U.codedText('warnings', truncated, '');
  const cased = loc === 'en' || loc === 'ru';
  if (cased && /^\p{Lu}/u.test(extra)) {
    // a new sentence: a full stop (unless the first part has one) and the
    // second part as it is, capital and all
    const stop = /[.!?…]$/u.test(main) ? ' ' : '. ';
    assert.equal(text, main + stop + extra, `${loc}: two sentences`);
    assert.ok(!text.includes(sep + extra), `${loc}: not "${sep}${extra.slice(0, 12)}…"`);
  } else {
    assert.equal(text, main + sep + extra, `${loc}: the clause separator`);
  }
  // no case is changed, and the last part ends as the catalog has it
  assert.ok(text.endsWith(extra), `${loc}: the second part unchanged`);
  // a part in lower case stays after the clause separator in every language
  assert.equal(U.sentenceJoin(['one', 'two']), `one${sep}two`, `${loc}: lower case`);
  assert.equal(U.sentenceJoin(['One']), 'One');
  assert.equal(U.sentenceJoin([]), '');
  if (cased) {
    assert.equal(U.sentenceJoin(['One', 'Two']), 'One. Two', `${loc}: capital`);
    assert.equal(U.sentenceJoin(['One.', 'Two']), 'One. Two', `${loc}: a full stop already`);
    assert.equal(U.sentenceJoin(['One?', 'Two', 'three']), `One? Two${sep}three`, `${loc}: mixed`);
  } else {
    assert.equal(U.sentenceJoin(['One', 'Two']), `One${sep}Two`, `${loc}: no sentences of its own`);
  }

  // the tariff summary: the zone first, its first letter raised where the
  // script has case
  const zone = I.t('tariff.zone.beijing');
  const sum = T.tariffSummary(tariff);
  const first = String.fromCodePoint(zone.codePointAt(0));
  const up = first.toLocaleUpperCase(loc);
  assert.ok(sum.startsWith(up + zone.slice(first.length) + ' · '), `${loc}: ${sum}`);
  assert.ok(!/^\p{Ll}/u.test(sum), `${loc}: no lower-case start: ${sum}`);
}

// upperFirst: letters with case raised, the rest as it is
await I.setLocale('en');
assert.equal(U.upperFirst('beijing time · weekdays'), 'Beijing time · weekdays');
assert.equal(U.upperFirst('Beijing'), 'Beijing');
assert.equal(U.upperFirst('北京时间'), '北京时间');
assert.equal(U.upperFirst('बीजिंग समय'), 'बीजिंग समय');
assert.equal(U.upperFirst('09:00'), '09:00');
assert.equal(U.upperFirst(''), '');
assert.equal(U.upperFirst(null), '');
await I.setLocale('ru');
assert.equal(U.upperFirst('пекинское время'), 'Пекинское время');
// W4b (R34): Cyrillic capitals start a sentence too (ru's low_volume is
// still an English placeholder, so the catalogs alone do not reach this)
assert.equal(U.sentenceJoin(['Запись неполная', 'Запись очень тихая']), 'Запись неполная. Запись очень тихая');
assert.equal(U.sentenceJoin(['Один', 'два']), `Один${I.t('common.clause_sep')}два`);
assert.equal(U.sentenceJoin(['Один!', 'Ёлка']), 'Один! Ёлка');
// a capital in a language without sentence case here (hi) changes nothing
await I.setLocale('hi');
assert.equal(U.sentenceJoin(['एक', 'Запись']), `एक${I.t('common.clause_sep')}Запись`);

// r4 A3: joinSentences (complete sentences, each with its own end mark):
// nothing after a full-width stop, one space after anything else; the
// rule is the text's, not the UI language's
assert.equal(U.joinSentences(['翻译会因此变慢。', '建议：并发 23。']), '翻译会因此变慢。建议：并发 23。', 'zh-CN: no space after 。');
assert.equal(U.joinSentences(['真的吗？', '是的！', '好。']), '真的吗？是的！好。', 'zh: ？ and ！ too');
assert.equal(U.joinSentences(['他说“好。”', '然后走了。']), '他说“好。”然后走了。', 'zh: a stop inside closing quotes');
assert.equal(U.joinSentences(['Slows down.', 'Suggested: 23.']), 'Slows down. Suggested: 23.', 'en: one space');
assert.equal(U.joinSentences(['धीमा होता है।', 'सुझाव: 23।']), 'धीमा होता है। सुझाव: 23।', 'hi: one space after ।');
assert.equal(U.joinSentences(['Ein Satz', '二。', 'x']), 'Ein Satz 二。x', 'mixed');
assert.equal(U.joinSentences(['a.', '', null, false, 'b.']), 'a. b.', 'empty parts are left out');
assert.equal(U.joinSentences([]), '');
assert.equal(U.joinSentences(null), '');
assert.equal(U.joinSentences(['只有一句。']), '只有一句。');
// with the real catalogs of zh-CN, en and hi
for (const [loc, sep] of [
  ['zh-CN', ''],
  ['en', ' '],
  ['hi', ' '],
]) {
  await I.setLocale(loc);
  const a = I.t('settings.provider.rate_limits.caveat');
  const b = I.t('settings.provider.rate_limits.gradual');
  assert.equal(U.joinSentences([a, b]), a + sep + b, `${loc}: two catalog sentences`);
}

// r6 (SEC40): the refusal "another copy has an update waiting". Its
// {kind} is a coded text of its own (which copy), rendered inside the
// sentence; zh-CN and en have the texts, the other languages fall back to
// en as a whole until they are translated.
{
  const refusal = (copy) => ({ code: 'selfupdate.other_copy_pending', params: { version: '3.2.1', kind: { code: `selfupdate.copy_${copy}` } } });
  await I.setLocale('zh-CN');
  assert.equal(
    U.errorInfoText(refusal('portable')),
    '另一份 LectureFlow（便携版，和这一份共用数据目录）已经准备好一个更新（3.2.1），等它下次启动时安装。请先启动那一份，等它装完，再回到这里更新；超过 14 天没有安装，这一份就不再等它',
  );
  assert.ok(U.errorInfoText(refusal('installer')).startsWith('另一份 LectureFlow（安装版，'));
  await I.setLocale('en');
  assert.equal(
    U.errorInfoText(refusal('installer')),
    'Another copy of LectureFlow that shares this data folder (the installed one) has an update ready (3.2.1) and installs it the next time it starts. Start that copy first; once it has installed the update, update this one. If it is not installed within 14 days, this copy stops waiting for it',
  );
  assert.ok(U.errorInfoText(refusal('portable')).includes('(the portable one)'));
  for (const loc of ['zh-TW', 'yue-Hant-HK', 'ru', 'hi']) {
    await I.setLocale(loc);
    for (const copy of ['installer', 'portable']) {
      const text = U.errorInfoText(refusal(copy));
      assert.ok(text && !text.includes('{') && !text.includes('selfupdate.') && text.includes('3.2.1'), `${loc} ${copy}: ${text}`);
    }
  }
}

console.log('w4 text checks: ok');
