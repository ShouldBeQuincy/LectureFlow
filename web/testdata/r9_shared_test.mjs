// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
//
// Round 9, the shared web modules (the r9-web-shared line):
//   WH-01  a menu closed by a scroll of the page or a resized window gives
//          the focus back to its opener when the focus was in it (T28 G2);
//   WH-05  a size under 1000 bytes in the catalog's words, not Intl's
//          English "byte" in zh-TW (T28 G7);
//   WH-07  a lecture stage's refusal or time limit says only "retry with
//          another model…" (convention C2, T28 suggestion 2);
//   (lead)  a size param the back end sends as -1 (unknown) is said
//          "unknown", not "-1 MB";
//   WH-10  t() reads {name:lc} (convention C5, T28 suggestion 10);
//   WH-11  launchCostMode knows the zero-cost plan (R86 L2);
//   R90 M1 a toast told again becomes the newest (shown under WH-04's limit);
// The modules run on the DOM stand-in of r7_dom.mjs; web/r9_shared_test.go
// starts it with the web directory as a file: URL. Stand-in nodes are
// compared by identity only (assert.ok(a === b, …)).

import { assert, base, doc, body, El, fireWindow } from './r7_dom.mjs';

const I = await import(new URL('i18n.js', base).href);
const M = await import(new URL('menu.js', base).href);
await I.setLocale('zh-CN');

// ---- WH-01: a menu closed by a scroll or a resize gives the focus back ----

{
  const menuOpen = () => doc.querySelectorAll('.ctx-menu').length > 0;
  const inMenu = () => !!(doc.activeElement && doc.activeElement.closest && doc.activeElement.closest('.ctx-menu'));
  // a scroll event of the page (document), or of an element (target)
  const scroll = (target = doc) => {
    for (const fn of [...(doc.listeners.scroll || [])]) fn({ type: 'scroll', target });
  };
  // the opener, focused: records the options of its focus() calls
  function opener(fkey) {
    const b = new El('button');
    b.dataset.fkey = fkey;
    b.focusOpts = [];
    const own = b.focus;
    b.focus = function (o) {
      this.focusOpts.push(o);
      own.call(this, o);
    };
    body.append(b);
    b.focus();
    return b;
  }
  const items = () => [
    { label: 'Alpha', onSelect() {} },
    { label: 'Beta', onSelect() {} },
  ];
  let scrolledTo = 0;
  const realScrollTo = globalThis.scrollTo;
  globalThis.scrollTo = () => scrolledTo++;
  try {
    // opened from the keyboard (Shift+F10 on a row, the "⋯" with ↓): the
    // focus is in the menu; a scroll of the page closes it and gives the
    // focus back to the opener, without scrolling to it
    for (const [what, close] of [
      ['a scroll of the page', () => scroll()],
      ['a scroll of an element outside the menu', () => scroll(body)],
      ['a resized window', () => fireWindow('resize')],
    ]) {
      const b = opener('r9:menu');
      assert.ok(M.openMenu(items(), { anchor: b }, { returnFocus: b }), `${what}: the menu opens`);
      assert.ok(inMenu(), `${what}: the focus is in the menu`);
      b.focusOpts.length = 0;
      close();
      assert.ok(!menuOpen(), `${what}: the menu is closed`);
      assert.ok(doc.activeElement === b, `${what}: the focus is back on the opener`);
      assert.equal(b.focusOpts.length, 1, `${what}: focused once`);
      assert.ok(b.focusOpts[0] && b.focusOpts[0].preventScroll === true, `${what}: without scrolling to it`);
      assert.equal(scrolledTo, 0, `${what}: the page is not scrolled`);
      b.remove();
    }

    // the opener redrawn meanwhile (an SSE refresh): found again by its data-fkey
    {
      const b = opener('r9:menu-redrawn');
      M.openMenu(items(), { anchor: b }, { returnFocus: b });
      b.remove();
      const again = opener('r9:menu-redrawn');
      doc.querySelectorAll('.ctx-item')[1].focus(); // moved down with ↓
      scroll();
      assert.ok(!menuOpen(), 'redrawn: closed');
      assert.ok(doc.activeElement === again, 'redrawn: the focus is on the redrawn opener');
      again.remove();
    }

    // the menu's own scroll (a menu limited in height) does not close it
    {
      const b = opener('r9:menu-own');
      M.openMenu(items(), { anchor: b }, { returnFocus: b });
      scroll(doc.querySelector('.ctx-menu'));
      scroll(doc.querySelectorAll('.ctx-item')[0]);
      assert.ok(menuOpen(), 'its own scroll: still open');
      assert.ok(inMenu(), 'its own scroll: the focus still in it');
      M.closeMenu({ focus: false });
      b.remove();
    }

    // the focus elsewhere on the page (the menu was opened with the
    // pointer and the focus went on): closed, the focus not moved
    for (const [what, close] of [
      ['scroll', () => scroll()],
      ['resize', () => fireWindow('resize')],
    ]) {
      const b = opener('r9:menu-pointer');
      M.openMenu(items(), { x: 40, y: 40 }, { returnFocus: b });
      const other = new El('input');
      body.append(other);
      other.focus();
      b.focusOpts.length = 0;
      close();
      assert.ok(!menuOpen(), `focus elsewhere, ${what}: closed`);
      assert.ok(doc.activeElement === other, `focus elsewhere, ${what}: the focus stays where it is`);
      assert.equal(b.focusOpts.length, 0, `focus elsewhere, ${what}: the opener is not focused`);
      other.remove();
      b.remove();
    }

    // the window loses the focus (blur): closed, the focus not given back
    {
      const b = opener('r9:menu-blur');
      M.openMenu(items(), { anchor: b }, { returnFocus: b });
      const item = doc.activeElement;
      b.focusOpts.length = 0;
      fireWindow('blur');
      assert.ok(!menuOpen(), 'blur: closed');
      assert.equal(b.focusOpts.length, 0, 'blur: the opener is not focused');
      assert.ok(doc.activeElement === item, 'blur: the focus is not moved');
      body.focus();
      b.remove();
    }

    // a press outside the menu: closed, the focus not taken
    {
      const b = opener('r9:menu-outside');
      M.openMenu(items(), { anchor: b }, { returnFocus: b });
      b.focusOpts.length = 0;
      for (const fn of [...(doc.listeners.pointerdown || [])]) fn({ type: 'pointerdown', target: body });
      assert.ok(!menuOpen(), 'outside: closed');
      assert.equal(b.focusOpts.length, 0, 'outside: the opener is not focused');
      body.focus();
      b.remove();
    }
    assert.equal(scrolledTo, 0, 'nothing scrolled the page');
  } finally {
    globalThis.scrollTo = realScrollTo;
  }
}

// ---- WH-05: sizes under 1000 bytes in the catalog's words ----

{
  const U = await import(new URL('util.js', base).href);
  // Intl's long name of the byte was English in Traditional Chinese ("714
  // byte", T28 G7): every language now says it with common.size_bytes. A
  // catalog without the key falls back along its chain (i18n.js CHAINS), as
  // any text does: until the translators add it, zh-TW, ru and hi say en's
  // "714 bytes" (and yue-Hant-HK zh-TW's, else en's). So an English unit
  // shows only where the language's own catalog lacks the key and the chain
  // reached en; never Intl's word, never a key name.
  const { readFile } = await import('node:fs/promises');
  const chains = { 'zh-CN': ['zh-CN', 'en'], en: ['en'], 'zh-TW': ['zh-TW', 'en'], 'yue-Hant-HK': ['yue-Hant-HK', 'zh-TW', 'en'], ru: ['ru', 'en'], hi: ['hi', 'en'] };
  const cats = {};
  for (const c of Object.keys(chains)) cats[c] = JSON.parse(await readFile(new URL(`i18n/${c}.json`, base), 'utf8'));
  // the text of the first catalog of the chain with the key (its form for n)
  const expected = (loc, n) => {
    for (const c of chains[loc]) {
      const v = cats[c]['common.size_bytes'];
      if (!v) continue;
      const form = v[new Intl.PluralRules(c).select(n)] || v.other;
      return { from: c, text: form.replace('{n}', String(n)) };
    }
    return null;
  };
  const english = /\bbytes?\b/i;
  for (const loc of ['zh-TW', 'zh-CN', 'en', 'ru', 'hi', 'yue-Hant-HK']) {
    await I.setLocale(loc);
    const s = U.fmtBytes(714);
    const want = expected(loc, 714);
    assert.ok(want, `${loc}: some catalog of its chain has common.size_bytes`);
    assert.equal(s, want.text, `${loc}: fmtBytes(714) is common.size_bytes of ${want.from}`);
    assert.ok(s !== 'common.size_bytes' && s.includes('714'), `${loc}: a text with the number: ${s}`);
    // an English unit only by the fallback to en (the language's own key missing)
    if (loc !== 'en' && english.test(s)) assert.ok(want.from === 'en' && !cats[loc]['common.size_bytes'], `${loc}: English "${s}" though its chain has a key before en`);
    if (loc !== 'en' && cats[loc]['common.size_bytes']) assert.ok(!english.test(s), `${loc}: its own words, not English: ${s}`);
    // never Intl's long name (English "byte" in zh-TW), unless a catalog says the same
    const intl = new Intl.NumberFormat(loc, { style: 'unit', unit: 'byte', unitDisplay: 'long' }).format(714);
    assert.ok(s !== intl || want.from === loc, `${loc}: not Intl's "${intl}"`);
    // 999.6 rounds to 1000: the next unit, as before
    assert.equal(U.fmtBytes(999.6), U.fmtBytes(1000), `${loc}: 999.6 is written as 1000 bytes are`);
    assert.ok(!U.fmtBytes(999.6).includes('1000') && !U.fmtBytes(999.6).includes('1,000'), `${loc}: in the next unit: ${U.fmtBytes(999.6)}`);
  }
  // the source catalogs' words (as round 8's table: "714 bytes", "714字节")
  await I.setLocale('en');
  for (const [n, want] of [[0, '0 bytes'], [1, '1 byte'], [714, '714 bytes'], [999, '999 bytes'], [999.6, '1 kB']]) assert.equal(U.fmtBytes(n), want, `en fmtBytes(${n})`);
  await I.setLocale('zh-CN');
  for (const [n, want] of [[0, '0字节'], [1, '1字节'], [714, '714字节'], [999, '999字节'], [999.6, '1 kB']]) assert.equal(U.fmtBytes(n), want, `zh-CN fmtBytes(${n})`);
}

// ---- WH-07: a lecture stage refused or out of time: only its own way out ----

{
  const U = await import(new URL('util.js', base).href);
  const failed = (stage, code, extra = {}) => ({
    stage,
    status: 'failed',
    allowed_actions: ['retry'],
    error: 'raw',
    error_info: { code, params: { role: stage === 'material_terms' ? 'glossary' : stage, retries: 0, provider: 'Anthropic', model: 'claude-x', seconds: 600, category: '' } },
    ...extra,
  });
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const button = `“${I.t('common.action.retry_model')}”`;
    const roles = `“${I.t('settings.block.roles')}”`;
    for (const code of ['llm.refusal', 'llm.time_limit']) {
      // a lecture's summary, correction, translation: the lecture's own text
      for (const stage of ['summary', 'repair', 'translate']) {
        const st = failed(stage, code);
        const text = U.stageErrorText(st);
        const what = `${loc} ${stage} ${code}`;
        assert.equal(text, U.codedText('common.lecture_error', st.error_info, 'raw'), `${what}: common.lecture_error.${code}`);
        assert.ok(I.has(`common.lecture_error.${code}`) && text !== U.errorInfoText(st.error_info, 'raw'), `${what}: not the errors.* sentence`);
        assert.ok(text.includes(button), `${what}: names the button: ${text}`);
        assert.ok(!text.includes(roles), `${what}: not the roles: ${text}`);
        assert.ok(text.includes(U.roleName(stage)) && text.includes('Anthropic') && text.includes('claude-x'), `${what}: the role, the provider and the model: ${text}`);
        if (code === 'llm.time_limit') assert.ok(text.includes('600'), `${what}: the time limit: ${text}`);
        // the automatic retry has no text of its own for these codes: the same
        assert.equal(U.stageErrorText({ ...st, status: 'queued' }), text, `${what}: queued again, the same text`);
      }
      // the course's stages and a lecture's title task: the whole sentence,
      // the roles' way out included, as before
      for (const stage of ['material_terms', 'glossary_seed', 'glossary_review', 'title']) {
        const st = failed(stage, code);
        const text = U.stageErrorText(st);
        assert.equal(text, U.errorInfoText(st.error_info, 'raw'), `${loc} ${stage} ${code}: errors.${code} as before`);
        assert.ok(text.includes(roles), `${loc} ${stage} ${code}: the roles' way out: ${text}`);
      }
      // errorInfoText (the dashboard, other callers) is unchanged
      assert.ok(U.errorInfoText(failed('translate', code).error_info).includes(roles), `${loc} ${code}: errorInfoText as before`);
    }
    // other codes of a lecture stage: as before; the retry in progress
    // keeps its own text
    const busy = failed('summary', 'llm.server_error', { status: 'queued' });
    busy.error_info.params.status = 503;
    assert.equal(U.stageErrorText(busy), U.codedText('common.retrying', busy.error_info, 'raw'), `${loc}: the retry in progress: common.retrying`);
    const plain = failed('translate', 'llm.server_error');
    plain.error_info.params.status = 503;
    assert.equal(U.stageErrorText(plain), U.errorInfoText(plain.error_info, 'raw'), `${loc}: another code: errors.* as before`);
    // a warning (done with a warning): as before
    const warned = { stage: 'translate', status: 'done', error: 'w', warning_info: { code: 'translate.low_quality', params: {} }, error_info: failed('translate', 'llm.refusal').error_info };
    assert.equal(U.stageErrorText(warned), U.codedText('warnings', warned.warning_info, 'w'), `${loc}: a warning first, as before`);
  }
  await I.setLocale('zh-CN');
}

// ---- a size the back end could not tell (-1): "unknown", not "-1 MB" ----

{
  const U = await import(new URL('util.js', base).href);
  const negative = (s) => /[-−]\s?\d/.test(s);
  for (const loc of ['zh-CN', 'en']) {
    await I.setLocale(loc);
    const unknown = I.t('compui.unknown_size');
    assert.ok(unknown && unknown !== 'compui.unknown_size', `${loc}: the components page's "unknown"`);
    // a copy that hit a full disk (internal/components/importfile.go)
    const disk = U.errorInfoText({ code: 'components.disk_full', params: { required_mb: -1, free_mb: -1, id: 'asr-cuda' } });
    assert.ok(!negative(disk), `${loc} components.disk_full: no negative size: ${disk}`);
    assert.equal(disk.split(unknown).length - 1, 2, `${loc} components.disk_full: both sizes unknown: ${disk}`);
    // the remote agent without its figures (internal/remote/protocol.go)
    const remote = U.errorInfoText({ code: 'remote.no_space', params: { device: 'gpu-box', free_mb: -1, need_mb: 2048 } });
    assert.ok(!negative(remote) && remote.includes(unknown) && remote.includes(U.fmtMB(2048)), `${loc} remote.no_space: the free space unknown, the need known: ${remote}`);
    // a size in bytes, the same rule
    const bytes = U.errorInfoText({ code: 'downloads.no_space', params: { free_bytes: -1, keep_bytes: 5e9 } });
    assert.ok(!negative(bytes) && bytes.includes(unknown) && bytes.includes(U.fmtBytes(5e9)), `${loc} downloads.no_space: ${bytes}`);
    // known sizes, 0 among them (a full disk), as before
    const known = U.errorInfoText({ code: 'components.disk_full', params: { required_mb: 3072, free_mb: 0, id: 'asr-cuda' } });
    assert.ok(known.includes(U.fmtMB(3072)) && known.includes(U.fmtMB(0)) && !known.includes(unknown), `${loc}: known sizes as before: ${known}`);
  }
  await I.setLocale('zh-CN');
}

// ---- WH-10: t() reads {name:lc} ----

{
  // a fresh i18n.js (its own catalogs) reading test catalogs: ru with the
  // templates of a translator, en as its fallback
  const ru = {
    'r9.lc': 'Расшифровка: {w:lc}',
    'r9.pending': '{stage}: {wait:lc}',
    'r9.plain': 'Расшифровка: {w}',
    'r9.two': '{w:lc} — {w}',
    'r9.unknown_mod': 'Этапы: {w:foo}',
    'r9.count': { one: '{n:lc} этап: {w:lc}', few: '{n:lc} этапа: {w:lc}', many: '{n:lc} этапов: {w:lc}', other: '{n:lc} этапа: {w:lc}' },
  };
  const en = { 'r9.lc': 'Transcription: {w}', 'r9.en_only': 'Waiting: {w:lc}' };
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const u = String(url);
    if (u === 'i18n/ru.json' || u === 'i18n/en.json') {
      const cat = u === 'i18n/ru.json' ? ru : en;
      return { ok: true, status: 200, json: async () => cat, text: async () => JSON.stringify(cat) };
    }
    return realFetch(url, init);
  };
  let T;
  try {
    T = await import(new URL('i18n.js?r9-wh10', base).href);
    await T.setLocale('ru');
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.equal(T.getLocale(), 'ru', 'the test catalogs are read');
  const lc = (w) => T.t('r9.lc', { w });
  // a word that starts with a capital of another script than Latin and a
  // small letter: lower-cased at its first letter; a list, every word
  assert.equal(lc('Ждёт терминов…'), 'Расшифровка: ждёт терминов…');
  assert.equal(lc('Ждёт свободного устройства…'), 'Расшифровка: ждёт свободного устройства…');
  assert.equal(lc('Исправление, Перевод, Экспорт'), 'Расшифровка: исправление, перевод, экспорт');
  assert.equal(lc('Глоссарий обновлён (v3 → v4)'), 'Расшифровка: глоссарий обновлён (v3 → v4)');
  // round 9 C185d: what is quoted is left as written (a name)
  assert.equal(lc('«Перевод»'), 'Расшифровка: «Перевод»', 'a quoted word: as written');
  // Latin words, all-capital abbreviations, numbers, the empty string: as they are
  assert.equal(lc('LLM недоступен'), 'Расшифровка: LLM недоступен');
  assert.equal(lc('Colab занят'), 'Расшифровка: Colab занят');
  assert.equal(lc('DeepSeek отвечает'), 'Расшифровка: DeepSeek отвечает');
  assert.equal(lc('ГПУ занят'), 'Расшифровка: ГПУ занят');
  assert.equal(lc('Я'), 'Расшифровка: Я', 'one capital letter alone is not a word that starts with a small letter after it');
  assert.equal(lc('42'), 'Расшифровка: 42');
  assert.equal(lc(42), 'Расшифровка: 42', 'a number param');
  assert.equal(lc(''), 'Расшифровка: ');
  assert.equal(lc('уже строчные'), 'Расшифровка: уже строчные');
  // without the modifier: as before, byte for byte
  assert.equal(T.t('r9.plain', { w: 'Ждёт терминов…' }), 'Расшифровка: Ждёт терминов…');
  assert.equal(T.t('r9.two', { w: 'Перевод' }), 'перевод — Перевод', 'the same param with and without');
  // a param not given, or null: the placeholder left as written (as {w})
  assert.equal(T.t('r9.lc', {}), 'Расшифровка: {w:lc}');
  assert.equal(T.t('r9.lc', { w: null }), 'Расшифровка: {w:lc}');
  assert.equal(T.t('r9.plain', {}), 'Расшифровка: {w}');
  // a modifier t() does not know: left as written (the catalog test reports it)
  assert.equal(T.t('r9.unknown_mod', { w: 'Перевод' }), 'Этапы: {w:foo}');
  // plural forms: chosen by n as before; a count object is not a string
  const U = await import(new URL('util.js', base).href);
  assert.equal(T.t('r9.count', { n: 3, w: 'Перевод, Экспорт' }), '3 этапа: перевод, экспорт');
  assert.equal(T.t('r9.count', { n: 5, w: 'Перевод' }), '5 этапов: перевод');
  assert.equal(T.t('r9.count', { n: U.count(1234), w: 'Перевод' }), `${U.count(1234)} этапа: перевод`, 'a count object: formatted, its form chosen by its number (1234: few)');
  // the fallback language's template (en) with the modifier: the same rule
  // (Latin words are left alone)
  assert.equal(T.t('r9.en_only', { w: 'Glossary, Перевод' }), 'Waiting: Glossary, перевод');

  // round 9 C185d, the ru catalog's first use: the real fragments in
  // common.summary.pending ("{stage}: {wait:lc}"); quoted names as written,
  // a one-letter word before a small letter lower-cased
  const pend = (wait) => T.t('r9.pending', { stage: 'Расшифровка', wait });
  for (const [wait, want] of [
    // waiting.component_missing, its device named by the user
    [
      'Для устройства расшифровки «Моя Карта» не установлен нужный компонент; расшифровка начнётся сама после его установки в настройках, в разделе «Компоненты»',
      'Расшифровка: для устройства расшифровки «Моя Карта» не установлен нужный компонент; расшифровка начнётся сама после его установки в настройках, в разделе «Компоненты»',
    ],
    // waiting.model_missing
    [
      'Модель для устройства расшифровки «Моя Карта» не установлена; установите её в настройках, в разделе «Компоненты», или выберите для устройства установленную модель',
      'Расшифровка: модель для устройства расшифровки «Моя Карта» не установлена; установите её в настройках, в разделе «Компоненты», или выберите для устройства установленную модель',
    ],
    // waiting.no_asr_device
    ['Нет доступного устройства расшифровки (см. раздел «Расшифровка» в настройках)', 'Расшифровка: нет доступного устройства расшифровки (см. раздел «Расшифровка» в настройках)'],
    // waiting.power_queue: a one-letter word
    ['В очереди (учёт питания): ждёт, пока освободится NVIDIA RTX 4060', 'Расшифровка: в очереди (учёт питания): ждёт, пока освободится NVIDIA RTX 4060'],
    ['В очереди (учёт питания)…', 'Расшифровка: в очереди (учёт питания)…'],
    // a list of stages; a reason with two clauses
    ['Исправление, Перевод, Экспорт', 'Расшифровка: исправление, перевод, экспорт'],
    ['Глоссарий обновлён (v3 → v4); Слайды изменились', 'Расшифровка: глоссарий обновлён (v3 → v4); слайды изменились'],
    ['Ждёт свободного устройства «Моя Карта»', 'Расшифровка: ждёт свободного устройства «Моя Карта»'],
    // other quotes: „…“, “…”, "…", ‘…’, '…', nested, and one never closed
    ['Ждёт „Мой Сервер“ и Перевод', 'Расшифровка: ждёт „Мой Сервер“ и перевод'],
    ['Ждёт “Мой Сервер” и Перевод', 'Расшифровка: ждёт “Мой Сервер” и перевод'],
    ['Ждёт "Мой Сервер" и Перевод', 'Расшифровка: ждёт "Мой Сервер" и перевод'],
    ['Ждёт ‘Мой Сервер’ и Перевод', 'Расшифровка: ждёт ‘Мой Сервер’ и перевод'],
    ["Ждёт 'Мой Сервер' и Перевод", "Расшифровка: ждёт 'Мой Сервер' и перевод"],
    ['Ждёт «Сервер „Мой Дом“ Второй» и Перевод', 'Расшифровка: ждёт «Сервер „Мой Дом“ Второй» и перевод'],
    ['Ждёт «Мой Сервер и Перевод', 'Расшифровка: ждёт «Мой Сервер и Перевод'],
    // one capital letter: before a capital, a digit or nothing it stays
    ['В ГПУ нет места', 'Расшифровка: В ГПУ нет места'],
    ['В 3 этапах', 'Расшифровка: В 3 этапах'],
    ['Ждёт: В', 'Расшифровка: ждёт: В'],
    ['ГПУ В очереди', 'Расшифровка: ГПУ в очереди'],
    // an apostrophe in a word is not a quote
    ["О'Брайен Ждёт", "Расшифровка: О'брайен ждёт"],
    // Latin words and all-capital abbreviations as they are
    ['LLM недоступен; A test; GPU и ГПУ', 'Расшифровка: LLM недоступен; A test; GPU и ГПУ'],
  ]) {
    assert.equal(pend(wait), want, `{wait:lc} of "${wait}"`);
  }
  // without the modifier, byte for byte as before
  assert.equal(T.t('r9.plain', { w: 'В очереди «Моя Карта»' }), 'Расшифровка: В очереди «Моя Карта»');
}

// ---- WH-11: the zero-cost plan is told "may cost money", not "billed" ----

{
  const UC = await import(new URL('update_confirm.js', base).href);
  const role = (r, configured, provider_id) => ({ role: r, configured, provider_id });
  const three = (ids) => ['summary', 'repair', 'translate'].map((r, i) => role(r, true, ids[i]));
  // the plan as the setup guide and the settings presets make it
  // (onboarding_providers.js: no prices, nightTariff(), offpeak_only)
  const night = { timezone: 'Asia/Shanghai', peak_windows: [{ days: [1, 2, 3, 4, 5, 6, 7], from: '08:00', to: '23:00' }], offpeak_multiplier: 1 };
  const plan = { id: 'pz', name: 'Zhipu', price_input: null, price_cache_hit: null, price_output: null, currency: null, tariff: night, offpeak_only: true };
  const zero = { id: 'p0', price_input: 0, price_cache_hit: 0, price_output: 0, tariff: null, offpeak_only: false };
  const priced = { id: 'p1', price_input: 2, price_cache_hit: 0.5, price_output: 8, tariff: null, offpeak_only: false };
  const mode = (providers, ids = ['pz', 'pz', 'pz']) => UC.launchCostMode({ roles: three(ids), providers });
  assert.equal(mode([plan]), 'free', 'the zero-cost plan: free');
  assert.equal(mode([plan, zero], ['pz', 'p0', 'pz']), 'free', 'the plan and prices at 0: free');
  assert.equal(mode([plan, priced], ['pz', 'pz', 'p1']), 'billed', 'one role priced: billed');
  for (const [p, what] of [
    [{ ...plan, tariff: { ...night, offpeak_multiplier: 0.5 } }, 'off-peak at half price'],
    [{ ...plan, tariff: { ...night, offpeak_multiplier: undefined } }, 'no multiplier'],
    [{ ...plan, price_input: 1 }, 'a price'],
    [{ ...plan, price_input: 0 }, 'a price set (0), the others not'],
    [{ ...plan, price_output: 0.5 }, 'an output price'],
    [{ ...plan, offpeak_only: false }, 'requests at any hour'],
    [{ ...plan, offpeak_only: undefined }, 'off-peak not known'],
    [{ ...plan, tariff: null }, 'no tariff, no prices (prices not known)'],
    [{ ...plan, tariff: 'night' }, 'a tariff that is not an object'],
  ]) assert.equal(mode([p]), 'billed', `${what}: billed`);
  // round 8's cases are checked by r8_shared_test.mjs; transcription only
  // as before
  assert.equal(UC.launchCostMode({ roles: ['summary', 'repair', 'translate'].map((r) => role(r, false, '')), providers: [plan] }), 'transcribe_only', 'no lecture role: transcription only');
  assert.equal(UC.launchCostMode(null), undefined, 'no settings');
}

// ---- R90 M1 (WH-04): a toast told again is the newest one, shown ----

{
  const D = await import(new URL('dom.js', base).href);
  const root = doc.getElementById('toasts');
  root.replaceChildren();
  const order = () => root.children.map((el) => el.dataset.msg);
  // notices that stay (failed fetches, one each), the oldest first: the
  // page shows the newest few (style.css), so the oldest is hidden
  for (const m of ['A', 'B', 'C', 'D']) D.toast(`r9 notice ${m}`, 'warn', { sticky: true });
  const a = root.children[0];
  // A fails again: counted on its toast, which is now the newest (shown,
  // and added to the live region again)
  D.toast('r9 notice A', 'warn', { sticky: true });
  assert.equal(root.children.length, 4, 'merged: no second toast');
  assert.ok(root.children[3] === a, 'the toast told again is the last, the newest');
  assert.deepEqual(order(), ['r9 notice B', 'r9 notice C', 'r9 notice D', 'r9 notice A'], 'the others keep their order');
  assert.equal(a.querySelector('.toast-count').textContent, '×2', 'counted');
  assert.equal(a.getAttribute('role'), 'status', 'its role kept');
  assert.equal(a._sticky, true, 'still sticky');
  // the newest told again: in its place
  D.toast('r9 notice A', 'warn', { sticky: true });
  assert.ok(root.children[3] === a && a.querySelector('.toast-count').textContent === '×3', 'the newest: counted in place');
  // a focus on its close button stays there. A browser drops the focus of
  // a node taken out of the tree, moved too (Chrome: to the body; R90b L5),
  // the stand-in does not: here, while the toast moves, it does as Chrome
  const b = root.children[0];
  const close = b.querySelector('.toast-close');
  close.focus();
  const realRemove = El.prototype.remove;
  El.prototype.remove = function () {
    if (this.parentNode && this.contains(doc.activeElement)) doc.activeElement = body;
    return realRemove.call(this);
  };
  try {
    D.toast('r9 notice B', 'warn', { sticky: true });
  } finally {
    delete El.prototype.remove;
  }
  assert.ok(El.prototype.remove === realRemove, 'the stand-in restored');
  assert.ok(root.children[3] === b, 'moved to the end');
  assert.ok(doc.activeElement === close, 'the focus stays on its close button (focused again after the move)');
  body.focus();
  // an error told again: the newest, read out as an alert
  D.toast('r9 error E', 'error');
  D.toast('r9 notice C', 'warn', { sticky: true });
  const e = root.children.find((el) => el.dataset.msg === 'r9 error E');
  D.toast('r9 error E', 'error');
  assert.ok(root.children[root.children.length - 1] === e && e.getAttribute('role') === 'alert', 'an error told again is the newest, an alert');
  assert.equal(e.querySelector('.toast-count').textContent, '×2', 'the error counted');
  // the same text of another kind is another toast, as before
  D.toast('r9 notice D', 'info', { sticky: true });
  assert.equal(root.children.filter((el) => el.dataset.msg === 'r9 notice D').length, 2, 'another kind: its own toast');
  root.replaceChildren();
}

console.log('r9 shared checks: ok');
