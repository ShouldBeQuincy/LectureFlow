// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 10, the stale reasons (the r10-web-stale line, C190):
//   1  every slide file name of a reason (materials_changed: added,
//      removed; material_changed: filename) in the UI language's quotation
//      marks (settings.quote), each one as written; nothing else of the
//      text changes (Russian: R95's wording, the marks from the code).
//   2  several reasons in one sentence: a reason in mid-sentence (after
//      the clause separator; in the update dialog, after its label) takes
//      the form of stale.reason_inline, {reason:lc} in Russian ("Почему
//      нужно обновить: изменились слайды…; глоссарий обновлён…"), {reason}
//      in zh-CN and en (unchanged); the file names and recorded texts a
//      reason carries stay as written. Russian stage names after "Нужно
//      обновить:" in lower case too ({stages:lc}, R95).
// The modules run on the DOM stand-in of r7_dom.mjs; web/r10_stale_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared by identity only (assert.ok(a === b, …)).

import { assert, base, dialogs, cancelDialogs } from './r7_dom.mjs';
import { readFile } from 'node:fs/promises';

const I = await import(new URL('i18n.js', base).href);
const U = await import(new URL('util.js', base).href);

// the catalogs as files, and the value of a key along a language's chain
// (i18n.js CHAINS)
const CHAINS = { 'zh-CN': ['zh-CN', 'en'], en: ['en'], 'zh-TW': ['zh-TW', 'en'], 'yue-Hant-HK': ['yue-Hant-HK', 'zh-TW', 'en'], ru: ['ru', 'en'], hi: ['hi', 'en'] };
const cats = {};
for (const c of Object.keys(CHAINS)) cats[c] = JSON.parse(await readFile(new URL(`i18n/${c}.json`, base), 'utf8'));
function cat(loc, key) {
  for (const c of CHAINS[loc]) if (typeof cats[c][key] === 'string') return cats[c][key];
  throw new Error(`${loc}: no ${key} along its chain`);
}
// a template filled as t() fills it, for templates without a modifier
function fill(tpl, params) {
  return tpl.replace(/\{(\w+)(?::(\w+))?\}/g, (m, name, mod) => {
    if (mod) throw new Error(`fill: ${m} has a modifier (${tpl})`);
    return params[name] == null ? m : String(params[name]);
  });
}
const swap = (s, from, to) => s.split(from).join(to);

// file names with a capital, quote characters (one closing quote alone among
// them), spaces, Cyrillic and Chinese letters
const ADDED = ['Week 3 Лекция.PDF', 'B.pdf', 'c "draft".pdf'];
const REMOVED = ['Глава» Два.pdf', '第三讲 课件.pdf', "O'Neil’s „Notes“.pptx"];

// ---- 1: each file name of a list in the language's quotation marks ----

{
  const MARKS = { 'zh-CN': ['「', '」'], en: ['“', '”'], 'zh-TW': ['「', '」'], 'yue-Hant-HK': ['「', '」'], ru: ['«', '»'], hi: ['“', '”'] };
  for (const loc of Object.keys(MARKS)) {
    await I.setLocale(loc);
    const [o, c] = MARKS[loc];
    assert.equal(I.t('settings.quote', { name: 'x' }), `${o}x${c}`, `${loc}: the language's quotation marks`);
    const sep = cat(loc, 'common.list_sep');
    const quoted = (names) => names && names.map((n) => o + n + c).join(sep);
    const bare = (names) => names && names.join(sep);
    for (const [params, key] of [
      [{ added: ADDED, removed: REMOVED }, 'stale.reason.materials_changed'],
      [{ added: ADDED }, 'stale.reason.materials_changed_added'],
      [{ removed: REMOVED }, 'stale.reason.materials_changed_removed'],
      [{ added: ['Only One.pdf'], removed: [] }, 'stale.reason.materials_changed_added'],
    ]) {
      const what = `${loc} ${key} ${JSON.stringify(params)}`;
      const got = U.staleReasonText({ code: 'materials_changed', params });
      assert.equal(got, fill(cat(loc, key), { added: quoted(params.added), removed: quoted(params.removed) }), `${what}: each name quoted`);
      const names = [...(params.added || []), ...(params.removed || [])];
      for (const n of names) assert.ok(got.includes(o + n + c), `${what}: "${n}" as written, in quotes: ${got}`);
      // the quotes are all that is new
      let unquoted = got;
      for (const n of names) unquoted = swap(unquoted, o + n + c, n);
      assert.equal(unquoted, fill(cat(loc, key), { added: bare(params.added), removed: bare(params.removed) }), `${what}: only the quotes are new`);
    }
    // without names: as before
    assert.equal(U.staleReasonText({ code: 'materials_changed', params: { added: [], removed: [] } }), cat(loc, 'stale.reason.materials_changed_plain'), `${loc}: empty lists`);
    assert.equal(U.staleReasonText({ code: 'materials_changed' }), cat(loc, 'stale.reason.materials_changed_plain'), `${loc}: no params`);
    // one slide file (material_changed, R94/R95): quoted the same way, as
    // written; without a name as before
    for (const n of [...ADDED, ...REMOVED]) {
      const what = `${loc} material_changed "${n}"`;
      const got = U.staleReasonText({ code: 'material_changed', params: { filename: n } });
      assert.equal(got, fill(cat(loc, 'stale.reason.material_changed'), { filename: o + n + c }), `${what}: quoted`);
      assert.equal(swap(got, o + n + c, n), fill(cat(loc, 'stale.reason.material_changed'), { filename: n }), `${what}: only the quotes are new`);
    }
    assert.equal(U.staleReasonText({ code: 'material_changed', params: {} }), cat(loc, 'stale.reason.material_changed_plain'), `${loc}: material_changed without a name`);
    // a lecture row's text names the quoted files too
    const row = U.staleSummaryText({ reasons: [{ code: 'materials_changed', params: { added: ['B.pdf'] } }], stages: ['repair'] });
    assert.ok(row.includes(`${o}B.pdf${c}`), `${loc}: the row's text: ${row}`);
  }

  // the source catalogs, written out
  await I.setLocale('zh-CN');
  assert.equal(
    U.staleReasonText({ code: 'materials_changed', params: { added: ['Week 3 Лекция.PDF', 'B.pdf'], removed: ['c "draft".pdf'] } }),
    '适用的课件已变化（新增 「Week 3 Лекция.PDF」、「B.pdf」；移除 「c "draft".pdf」）',
  );
  assert.equal(U.staleReasonText({ code: 'material_changed', params: { filename: 'B.pdf' } }), '课件已变化：「B.pdf」', 'zh-CN: one file quoted');
  await I.setLocale('en');
  assert.equal(
    U.staleReasonText({ code: 'materials_changed', params: { added: ['Week 3 Лекция.PDF', 'B.pdf'], removed: ['c "draft".pdf'] } }),
    'The slides this lecture uses changed (added “Week 3 Лекция.PDF”, “B.pdf”; removed “c "draft".pdf”)',
  );
  assert.equal(U.staleReasonText({ code: 'material_changed', params: { filename: 'B.pdf' } }), 'Slides changed: “B.pdf”', 'en: one file quoted');
  // Russian (R95): the marks come from the code, not the template; no colon
  await I.setLocale('ru');
  assert.equal(U.staleReasonText({ code: 'material_changed', params: { filename: 'Глава» Два.pdf' } }), 'Изменились слайды «Глава» Два.pdf»', 'ru: one file quoted once');
  assert.equal(U.staleReasonText({ code: 'material_changed' }), 'Изменились слайды', 'ru: one file without a name');
  await I.setLocale('zh-CN');
}

// ---- 2: several reasons in one sentence; Russian lowers mid-sentence ones ----

{
  const UC = await import(new URL('update_confirm.js', base).href);
  const glossary = { code: 'glossary_updated', params: { from: 3, to: 4 } };
  const lists = { code: 'materials_changed', params: { added: ['Week 3 Лекция.PDF', 'Глава» Два.pdf'], removed: ['c "draft".pdf'] } };
  const one = { code: 'material_changed', params: { filename: 'Лекция 5.pdf' } };
  const summary = { code: 'summary_updated' };
  // recorded before v2 (legacy): free text, shown as written
  const legacy = { code: 'legacy', params: { text: 'Слайды заменены вручную: Тема.pdf' } };
  const legacyZh = { code: 'legacy', params: { text: '课件已变化：Тема.pdf' } };
  // names holding the private-use marks util.js puts in place of the texts
  // it keeps out of {reason:lc} (U+E000, an index, U+E001), its own index
  // and the other list's
  const mark = (i) => `${String.fromCharCode(0xe000)}${i}${String.fromCharCode(0xe001)}`;
  const markedNames = [`Мой ${mark(0)} Файл.pdf`, `Б ${mark(1)}.pdf`];
  const marked = { code: 'materials_changed', params: { added: [markedNames[0]], removed: [markedNames[1]] } };
  const STALE = ['repair', 'translate'];
  const STAGES = ['prep', 'asr', 'summary', 'repair', 'translate', 'export'];
  const lecture = (id, o = {}) => ({ id, title: `Lecture ${id}`, kind: 'lecture', lecture_actions: ['update'], stages: STAGES.map((n) => ({ stage: n, status: STALE.includes(n) ? 'stale' : 'done' })), ...o });
  // the update dialog's text for these lectures (then cancelled)
  async function dialogText(lecs) {
    const before = dialogs.length;
    const answer = UC.confirmUpdate(lecs);
    assert.equal(dialogs.length, before + 1, 'the update dialog opens');
    const text = dialogs[dialogs.length - 1].textContent;
    await cancelDialogs();
    assert.equal(await answer, false, 'cancelled');
    return text;
  }
  const row = (reasons) => U.staleSummaryText({ reasons, stages: STALE });
  const rowWith = (texts) => I.t('stale.need_update', { reasons: texts.join(cat(I.getLocale(), 'common.clause_sep')), stages: U.listJoin(STALE.map(U.stageName)) });
  const why = (texts) => I.t('course.confirm.update_why', { reasons: texts.join(cat(I.getLocale(), 'common.clause_sep')) });
  const plain = (r) => U.staleReasonText(r);

  // Russian: a reason in mid-sentence starts in lower case; its own words
  // only (the ru templates of these reasons start with their one capital)
  await I.setLocale('ru');
  const mid = (r) => {
    const s = plain(r);
    assert.ok(/^\p{Lu}\p{Ll}/u.test(s), `ru: "${s}" starts with a capital word`);
    return s.charAt(0).toLocaleLowerCase('ru') + s.slice(1);
  };
  // the row: the first reason starts the sentence, the next ones follow
  // the clause separator; a repeated reason is said once
  const many = row([glossary, lists, one, legacy, legacyZh, summary, glossary]);
  assert.equal(many, rowWith([plain(glossary), mid(lists), mid(one), plain(legacy), plain(legacyZh), mid(summary)]), 'ru row: the first as it is, the next in lower case, the recorded texts as written');
  // written out; the stage names after "Нужно обновить:" in lower case too
  // (R95: {stages:lc}; stageName gives catalog words or a stage's id only)
  assert.equal(
    many,
    'Глоссарий обновлён (v3 → v4); изменился набор слайдов (добавлены: «Week 3 Лекция.PDF», «Глава» Два.pdf»; убраны: «c "draft".pdf»); изменились слайды «Лекция 5.pdf»; Слайды заменены вручную: Тема.pdf; 课件已变化：Тема.pdf; сводка обновлена. Нужно обновить: исправление, перевод',
    'ru row written out',
  );
  assert.equal(row([lists, glossary]), rowWith([plain(lists), mid(glossary)]), 'ru row: the file lists first, as they are');
  assert.equal(row([lists, glossary]), 'Изменился набор слайдов (добавлены: «Week 3 Лекция.PDF», «Глава» Два.pdf»; убраны: «c "draft".pdf»); глоссарий обновлён (v3 → v4). Нужно обновить: исправление, перевод', 'ru row: the names as written');
  assert.equal(row([one]), 'Изменились слайды «Лекция 5.pdf». Нужно обновить: исправление, перевод', 'ru row: one file');
  assert.equal(row([glossary]), rowWith([plain(glossary)]), 'ru row: one reason as before');
  for (const r of [glossary, lists, one, summary]) assert.equal(U.staleReasonInline(r), mid(r), `ru ${r.code}: the mid-sentence form`);
  for (const r of [legacy, legacyZh]) assert.equal(U.staleReasonInline(r), plain(r), 'ru: a recorded text as written');
  // names that hold the marks: as written all the same
  assert.equal(U.staleReasonInline(marked), mid(marked), 'ru: names with the marks: the mid-sentence form');
  assert.ok(row([glossary, marked]).includes(`; изменился набор слайдов (добавлены: «${markedNames[0]}»; убраны: «${markedNames[1]}»)`), 'ru row: the marked names as written');

  // the update dialog: every reason follows its label ("Почему нужно
  // обновить: …", R95: not the singular "Причина:" before several)
  let text = await dialogText([lecture('A', { stale: { reasons: [lists, glossary], stages: STALE } })]);
  assert.ok(text.includes(why([mid(lists), mid(glossary)])), `ru dialog: every reason in lower case after the label: ${text}`);
  assert.ok(text.includes('Почему нужно обновить: изменился набор слайдов (добавлены: «Week 3 Лекция.PDF», «Глава» Два.pdf»; убраны: «c "draft".pdf»); глоссарий обновлён (v3 → v4).'), `ru dialog written out: ${text}`);
  text = await dialogText([lecture('A1', { stale: { reasons: [one], stages: STALE } })]);
  assert.ok(text.includes('Почему нужно обновить: изменились слайды «Лекция 5.pdf».'), `ru dialog, one reason written out: ${text}`);
  assert.ok(!text.includes('Причина'), `ru dialog: no "Причина": ${text}`);
  text = await dialogText([lecture('B', { stale: { reasons: [legacy, one], stages: STALE } })]);
  assert.ok(text.includes(why([plain(legacy), mid(one)])), `ru dialog: a recorded text as written: ${text}`);
  // without Lecture.stale: the stale stages' reasons (stale_info; a text
  // of the transitional stale_reason as written)
  const own = STAGES.map((n) => (n === 'repair' ? { stage: n, status: 'stale', stale_info: one } : n === 'translate' ? { stage: n, status: 'stale', stale_reason: 'Тема изменилась' } : { stage: n, status: 'done' }));
  text = await dialogText([lecture('C', { stale: null, stages: own })]);
  assert.ok(text.includes(why([mid(one), 'Тема изменилась'])), `ru dialog from the stages: ${text}`);
  // two lectures, the same reason: said once
  text = await dialogText([lecture('D', { stale: { reasons: [summary], stages: STALE } }), lecture('E', { stale: { reasons: [summary, glossary], stages: STALE } })]);
  assert.ok(text.includes(why([mid(summary), mid(glossary)])), `ru dialog, two lectures: ${text}`);

  // zh-CN and en: every reason as it is (the clause separator, the quotes of 1)
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    for (const r of [glossary, lists, one, summary, legacy, marked]) assert.equal(U.staleReasonInline(r), plain(r), `${loc} ${r.code}: mid-sentence as it is`);
    assert.equal(row([glossary, lists, one, legacy, summary, glossary]), rowWith([glossary, lists, one, legacy, summary].map(plain)), `${loc} row: as before`);
    text = await dialogText([lecture('F', { stale: { reasons: [lists, glossary], stages: STALE } })]);
    assert.ok(text.includes(why([plain(lists), plain(glossary)])), `${loc} dialog: as before: ${text}`);
  }
  await I.setLocale('en');
  assert.ok(row([glossary, summary]).startsWith('Glossary updated (v3 → v4); Summary updated. '), 'en row written out');
  await I.setLocale('zh-CN');
  assert.ok(row([glossary, summary]).startsWith('术语表已更新（v3 → v4）；摘要已更新，'), 'zh-CN row written out');
}

console.log('r10 stale checks: ok');
