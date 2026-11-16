// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The packaged app refuses debugging switches (acceptance V12.2 finding 11).
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const { debugSwitchesIn, refuseDebugging, DEBUG_SWITCHES } = require('../../src/main/lib/debugflags');

test('debugging switches on a command line, in every spelling Chromium accepts', () => {
  assert.deepEqual(debugSwitchesIn(['--remote-debugging-port=9222'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['--remote-debugging-port', '9222'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['-remote-debugging-port=0'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['/remote-debugging-port=0'], 'win32'), ['remote-debugging-port']);
  assert.deepEqual(debugSwitchesIn(['/remote-debugging-port=0'], 'darwin'), [], '"/" is a path outside Windows');
  assert.deepEqual(debugSwitchesIn(['--Remote-Debugging-Pipe'], 'win32'), ['remote-debugging-pipe']);
  assert.deepEqual(debugSwitchesIn(['--inspect=9229', '--inspect-brk', '--lang=en'], 'darwin'), ['inspect', 'inspect-brk']);
  assert.deepEqual(debugSwitchesIn(['--lang=zh-CN', 'D:\\lecture.mp4', '--remote-debugging-portx'], 'win32'), []);
  assert.deepEqual(debugSwitchesIn(['--', '--remote-debugging-port=1'], 'win32'), [], 'after "--" there are only arguments');
  assert.deepEqual(debugSwitchesIn(undefined), []);
});

test('only the packaged app refuses, and LECTUREFLOW_E2E_ALLOW_DEBUG=1 lets the switches through', () => {
  const argv = ['--remote-debugging-port=9222'];
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv, platform: 'win32' }), ['remote-debugging-port']);
  assert.deepEqual(refuseDebugging({ isPackaged: false, argv, platform: 'win32' }), [], 'development and Playwright runs');
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv, env: { LECTUREFLOW_E2E_ALLOW_DEBUG: '1' }, platform: 'win32' }), []);
  for (const v of ['0', 'true', 'yes', '', ' 1']) {
    assert.deepEqual(refuseDebugging({ isPackaged: true, argv, env: { LECTUREFLOW_E2E_ALLOW_DEBUG: v }, platform: 'win32' }), ['remote-debugging-port'], JSON.stringify(v));
  }
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], platform: 'win32' }), []);
});

test("Chromium's own parse of the command line counts too", () => {
  const hasSwitch = (s) => s === 'remote-debugging-pipe';
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch, platform: 'win32' }), ['remote-debugging-pipe']);
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch: () => { throw new Error('x'); }, platform: 'win32' }), []);
  assert.ok(DEBUG_SWITCHES.includes('remote-debugging-port') && DEBUG_SWITCHES.includes('inspect-brk'));
});

test('main.js checks before anything else of the app starts', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const body = src.slice(src.indexOf('function main() {'));
  assert.match(body, /^function main\(\) \{\n {2}if \(refuseDebugSwitches\(\)\) return;\n/);
});

// ---- security audit FB1 L-5: an allow list, not a deny list (T-SEC-07) ----

test('the packaged app starts only with the switches it uses itself', () => {
  const { PACKAGED_SWITCHES } = require('../../src/main/lib/debugflags');
  assert.deepEqual(Object.keys(PACKAGED_SWITCHES).sort(), ['background', 'disable-gpu', 'disable-gpu-compositing', 'embedding', 'lang', 'lf-restore', 'updated']);
  const refused = (argv, platform = 'win32', hasSwitch) => refuseDebugging({ isPackaged: true, argv, platform, hasSwitch });
  for (const argv of [[], ['--background'], ['--updated'], ['--lf-restore'], ['--updated', '--background'], ['D:\\lecture.mp4'], ['--', '--no-sandbox']]) {
    assert.deepEqual(refused(argv), [], JSON.stringify(argv));
  }
  assert.deepEqual(refused(['-psn_0_1234567'], 'darwin'), [], 'the process serial number older macOS passes');
  assert.deepEqual(refused(['-psn_0_1234567'], 'win32'), ['psn_0_1234567']);
  // the switches the deny list missed (L-5), and others that weaken the app
  const cases = {
    '--log-net-log=C:\\x\\net.json': 'log-net-log',
    '--proxy-server=http://192.0.2.1:8080': 'proxy-server',
    '--load-extension=C:\\ext': 'load-extension',
    '--no-sandbox': 'no-sandbox',
    '--disable-site-isolation-trials': 'disable-site-isolation-trials',
    '--disable-web-security': 'disable-web-security',
    '--js-flags=--allow-natives-syntax': 'js-flags',
    '--user-data-dir=C:\\elsewhere': 'user-data-dir',
    '--enable-logging': 'enable-logging',
    '--lang': 'lang',
    '/host-resolver-rules=MAP * 192.0.2.1': 'host-resolver-rules',
    '-ignore-certificate-errors': 'ignore-certificate-errors',
  };
  for (const [arg, name] of Object.entries(cases)) assert.deepEqual(refused([arg]), [name], arg);
  assert.deepEqual(refused(['--background', '--no-sandbox', '--No-Sandbox', '--remote-debugging-port=1']), ['no-sandbox', 'remote-debugging-port'], 'each once');
  // development, Playwright and acceptance runs are unchanged
  assert.deepEqual(refuseDebugging({ isPackaged: false, argv: ['--no-sandbox', '--inspect'], platform: 'win32' }), []);
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: ['--remote-debugging-port=0', '--lang=en'], env: { LECTUREFLOW_E2E_ALLOW_DEBUG: '1' }, platform: 'win32' }), []);
});

// Review R44: Electron appends switches of its own to the main process's
// command line before main.js runs (ElectronMainDelegate::PreSandboxStartup:
// --allow-file-access-from-files everywhere, --enable-avfoundation on macOS;
// --no-sandbox when ELECTRON_DISABLE_SANDBOX is set). Measured with
// Electron 44.9.3 on Windows: a plain start has exactly
// allow-file-access-from-files on (test/unit/switchprobe.test.js checks the
// real binary). hasSwitch is therefore asked only about the debugging
// switches Electron never adds; the person's own switches are judged on the
// raw argv (process.argv has none of the appended ones).
const ELECTRON_MAIN_SWITCHES = {
  win32: ['allow-file-access-from-files'],
  darwin: ['allow-file-access-from-files', 'enable-avfoundation'],
};

test('review R44: rendering and language switches, and Windows COM activation, are allowed', () => {
  const refused = (argv, platform = 'win32') => refuseDebugging({ isPackaged: true, argv, platform });
  for (const platform of ['win32', 'darwin']) {
    for (const argv of [['--disable-gpu'], ['--disable-gpu-compositing'], ['--lang=zh-TW'], ['--lang=en', '--disable-gpu', '--updated'], ['-disable-gpu']]) {
      assert.deepEqual(refused(argv, platform), [], `${platform} ${JSON.stringify(argv)}`);
    }
    // a value-taking switch only as --name=value; switches without a value take none
    assert.deepEqual(refused(['--lang'], platform), ['lang'], platform);
    assert.deepEqual(refused(['--lang='], platform), ['lang'], platform);
    assert.deepEqual(refused(['--disable-gpu=1'], platform), ['disable-gpu'], platform);
    assert.deepEqual(refused(['--disable-gpu-compositing=no'], platform), ['disable-gpu-compositing'], platform);
  }
  // COM / notification activation: Windows appends -Embedding
  for (const a of ['-Embedding', '--embedding', '/Embedding', '--EMBEDDING']) assert.deepEqual(refused([a]), [], a);
  assert.deepEqual(refused(['--embedding=x']), ['embedding']);
  assert.deepEqual(refused(['--embedding'], 'darwin'), ['embedding'], 'Windows only');
});

test('the refusal says which option is not supported and to remove it (review R44)', () => {
  const { startRefusal } = require('../../src/main/lib/debugflags');
  const r = startRefusal({ isPackaged: true, argv: ['--remote-debugging-port=9222'], env: {}, platform: 'win32' });
  assert.deepEqual(r, {
    kind: 'args', title: 'startup.unsupported_args.title', body: 'startup.unsupported_args.body', params: { switches: '--remote-debugging-port=9222' },
  });
  const read = (l) => JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', 'locales', `${l}.json`), 'utf8'));
  const en = read('en');
  const zh = read('zh-CN');
  assert.match(en['startup.unsupported_args.body'], /\{switches\}.*not support.*Remove/s);
  assert.doesNotMatch(en['startup.unsupported_args.body'] + en['startup.unsupported_args.title'], /debug/i);
  assert.match(zh['startup.unsupported_args.body'], /\{switches\}.*去掉/s);
  assert.doesNotMatch(zh['startup.unsupported_args.body'] + zh['startup.unsupported_args.title'], /调试/);
  // {switches} may name several options, joined with commas, and the
  // translations have no plural forms: no words that count them as one
  assert.doesNotMatch(en['startup.unsupported_args.body'] + en['startup.unsupported_args.title'], /\b(this|that|the) (startup )?option\b|\bRemove (it|this)\b/i);
  // also with words between, as in "这个启动参数" / "该启动参数" / "此参数"
  assert.doesNotMatch(zh['startup.unsupported_args.body'] + zh['startup.unsupported_args.title'], /(这|该|此)个?[^，。（）]{0,4}参数/);
  assert.doesNotMatch('请去掉上述参数', /(这|该|此)个?[^，。（）]{0,4}参数/, 'the pattern itself passes the text in use');
  for (const s of ['这个启动参数', '该启动参数', '这个参数', '此参数']) assert.match(s, /(这|该|此)个?[^，。（）]{0,4}参数/, s);
  // {switches} once; the English text reads on with the arguments as written
  for (const m of [en, zh]) assert.equal(m['startup.unsupported_args.body'].split('{switches}').length, 2);
  assert.doesNotMatch(en['startup.unsupported_args.body'], /listed here/);
  assert.match(en['startup.unsupported_args.body'], /Remove the above/);
});

test("Chromium's own parse is asked only about the debugging switches Electron never adds", () => {
  const { HASSWITCH_SWITCHES, ELECTRON_ADDED_SWITCHES } = require('../../src/main/lib/debugflags');
  assert.deepEqual([...HASSWITCH_SWITCHES], [
    'remote-debugging-port', 'remote-debugging-pipe', 'remote-debugging-address', 'remote-debugging-io-pipes', 'remote-allow-origins',
    'inspect', 'inspect-brk', 'inspect-port', 'inspect-publish-uid', 'debug', 'debug-brk',
  ]);
  for (const s of [...ELECTRON_ADDED_SWITCHES, 'allow-file-access-from-files', 'enable-avfoundation', 'no-sandbox']) {
    assert.ok(!HASSWITCH_SWITCHES.includes(s), s);
  }
  const asked = [];
  refuseDebugging({ isPackaged: true, argv: [], hasSwitch: (x) => { asked.push(x); return false; }, platform: 'win32' });
  assert.deepEqual(asked, [...HASSWITCH_SWITCHES]);
});

test('with the switches Electron itself appends, every start of the app passes', () => {
  for (const [platform, added] of Object.entries(ELECTRON_MAIN_SWITCHES)) {
    const hasSwitch = (s) => added.includes(s);
    for (const argv of [[], ['--background'], ['--updated'], ['--lf-restore'], ['--updated', '--background']]) {
      assert.deepEqual(refuseDebugging({ isPackaged: true, argv, hasSwitch, platform }), [], `${platform} ${JSON.stringify(argv)}`);
    }
    // the person's own switches are still refused, on the argv
    assert.deepEqual(refuseDebugging({ isPackaged: true, argv: ['--allow-file-access-from-files'], hasSwitch, platform }), ['allow-file-access-from-files'], platform);
    assert.deepEqual(refuseDebugging({ isPackaged: true, argv: ['--no-sandbox'], hasSwitch, platform }), ['no-sandbox'], platform);
  }
  // a debugging switch only Chromium's parse sees is still refused
  const hasSwitch = (s) => s === 'remote-debugging-pipe' || ELECTRON_MAIN_SWITCHES.win32.includes(s);
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch, platform: 'win32' }), ['remote-debugging-pipe']);
});

test('a long or odd switch is shown short', () => {
  const r = refuseDebugging({ isPackaged: true, argv: [`--${'a'.repeat(500)}`, ...Array.from({ length: 30 }, (_, i) => `--x${i}`)], platform: 'win32' });
  assert.ok(r.length >= 1 && r.length <= 10, `1 to 10 names, got ${r.length}`);
  assert.ok(r.every((n) => n.length <= 64), 'names cut to 64 characters');
});

// ---- T14 finding 3: the box shows the refused arguments as written ----

test('the refused arguments are shown as written, a value-taking or value-less switch with its value', () => {
  const { refusedArgs, joinRefusedArgs } = require('../../src/main/lib/debugflags');
  const shown = (argv, platform = 'win32', extra = {}) => refusedArgs({ isPackaged: true, argv, platform, ...extra });
  for (const platform of ['win32', 'darwin']) {
    assert.deepEqual(shown(['--disable-gpu=1'], platform), ['--disable-gpu=1'], platform);
    assert.deepEqual(shown(['--disable-gpu-compositing=no'], platform), ['--disable-gpu-compositing=no'], platform);
    assert.deepEqual(shown(['--lang='], platform), ['--lang='], platform);
    assert.deepEqual(shown(['--lang'], platform), ['--lang'], platform);
    assert.deepEqual(shown(['--disable-gpu', '--lang=en', '--updated'], platform), [], `${platform}: allowed ones are not shown`);
  }
  assert.deepEqual(shown(['--embedding=x']), ['--embedding=x']);
  assert.deepEqual(shown(['-Remote-Debugging-Port=9222', '/proxy-server=http://192.0.2.1:8080']), ['-Remote-Debugging-Port=9222', '/proxy-server=http://192.0.2.1:8080'], 'spelling and case kept');
  // each name once, as first refused: --disable-gpu itself is allowed, --disable-gpu=1 is not
  assert.deepEqual(shown(['--disable-gpu', '--disable-gpu=1', '--disable-gpu=2', '--no-sandbox', '--No-Sandbox']), ['--disable-gpu=1', '--no-sandbox']);
  // a switch only Chromium's parse found
  assert.deepEqual(shown([], 'win32', { hasSwitch: (s) => s === 'remote-debugging-pipe' }), ['--remote-debugging-pipe']);
  // the same decision as refuseDebugging: development, e2e, and nothing refused
  assert.deepEqual(refusedArgs({ isPackaged: false, argv: ['--disable-gpu=1'], platform: 'win32' }), []);
  assert.deepEqual(shown(['--disable-gpu=1'], 'win32', { env: { LECTUREFLOW_E2E_ALLOW_DEBUG: '1' } }), []);
  assert.deepEqual(shown([]), []);
  // joined as before
  assert.equal(joinRefusedArgs(['--disable-gpu=1', '--no-sandbox']), '--disable-gpu=1, --no-sandbox');
  assert.equal(joinRefusedArgs(['--x']), '--x');
});

test('a refused argument is cleaned and cut before it is shown', () => {
  const { refusedArgs, joinRefusedArgs, MAX_ARG, MAX_LIST, MAX_SHOWN } = require('../../src/main/lib/debugflags');
  const shown = (argv) => refusedArgs({ isPackaged: true, argv, platform: 'win32' });
  // control characters, line breaks and text-reordering marks are taken out
  assert.deepEqual(shown(['--proxy-server=a\r\nb\tc\u0000d\u001be\u007ff\u0085g']), ['--proxy-server=abcdefg']);
  assert.deepEqual(shown(['--x=\u202eevil\u202c\u2066i\u2069\u200bz\u2028\u2029']), ['--x=eviliz']);
  assert.deepEqual(shown(['--\n']), ['--'], 'a name of control characters only');
  // one long argument: cut, with an ellipsis, by code points (no broken pair)
  const [long] = shown([`--proxy-server=${'x'.repeat(5000)}`]);
  assert.equal(Array.from(long).length, MAX_ARG);
  assert.ok(long.startsWith('--proxy-server=xxx') && long.endsWith('…'), long.slice(-5));
  const [emoji] = shown([`--x=${'\ud83d\ude00'.repeat(200)}`]);
  assert.equal(Array.from(emoji).length, MAX_ARG);
  assert.doesNotMatch(emoji, /[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/, 'no lone surrogate');
  assert.deepEqual(shown(['--x=' + 'y'.repeat(MAX_ARG - 4)]), ['--x=' + 'y'.repeat(MAX_ARG - 4)], 'exactly the limit is not cut');
  // many: at most MAX_SHOWN and an ellipsis; the whole list at most MAX_LIST
  const many = shown(Array.from({ length: 30 }, (_, i) => `--x${i}=${'v'.repeat(200)}`));
  assert.equal(many.length, MAX_SHOWN + 1);
  assert.equal(many[MAX_SHOWN], '…');
  const joined = joinRefusedArgs(many);
  assert.equal(Array.from(joined).length, MAX_LIST);
  assert.ok(joined.endsWith('…'));
  assert.doesNotMatch(joined, /[\r\n]/);
  assert.equal(shown(Array.from({ length: MAX_SHOWN }, (_, i) => `--y${i}`)).length, MAX_SHOWN, 'no ellipsis at exactly MAX_SHOWN');
  assert.equal(joinRefusedArgs(undefined), '');
});

test('review R54 S-6: every character that is not drawn is taken out, by Unicode property', () => {
  const { refusedArgs } = require('../../src/main/lib/debugflags');
  const shown = (argv) => refusedArgs({ isPackaged: true, argv, platform: 'win32' });
  const cp = (...c) => String.fromCodePoint(...c);
  const hidden = {
    'tag characters ("ASCII smuggling")': cp(0xe0001, 0xe0041, 0xe0020, 0xe007f),
    'variation selectors': cp(0xfe00, 0xfe0f, 0xe0100, 0xe01ef),
    'deprecated format controls U+206A-206F': cp(0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f),
    'Hangul fillers': cp(0x115f, 0x1160, 0x3164, 0xffa0),
    'combining grapheme joiner, Khmer and Mongolian ones': cp(0x034f, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180f),
    'shorthand format and musical controls': cp(0x1bca0, 0x1bca3, 0x1d173, 0x1d17a),
    'prepended concatenation marks': cp(0x0600, 0x0605, 0x06dd, 0x070f, 0x0890, 0x0891, 0x08e2, 0x110bd, 0x110cd),
    'Egyptian hieroglyph format controls': cp(0x13430, 0x1343f),
    'the ones of the old list': cp(0x00ad, 0x061c, 0x180e, 0x200b, 0x200f, 0x2028, 0x202e, 0x2060, 0x2069, 0xfeff, 0xfff9, 0xfffb),
  };
  for (const [what, s] of Object.entries(hidden)) {
    assert.deepEqual(shown([`--proxy-server=a${s}b`]), ['--proxy-server=ab'], what);
  }
  // visible text stays, also outside ASCII
  const kept = `--x=${cp(0x4e2d, 0x6587)}${cp(0x05d0)}${cp(0x0438)}${cp(0x0915)}${cp(0x1f600)}`;
  assert.deepEqual(shown([kept]), [kept]);
});

test('main.js shows the box startRefusal chose, and nothing else', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const fn = src.slice(src.indexOf('function refuseDebugSwitches'), src.indexOf('function dropDevEnvironment'));
  assert.match(fn, /const refusal = startRefusal\(\{\n {4}isPackaged: app\.isPackaged,\n {4}argv: process\.argv\.slice\(1\),\n {4}env: process\.env,\n {4}hasSwitch: \(s\) => app\.commandLine\.hasSwitch\(s\),\n {4}platform: process\.platform,\n {2}\}\);\n {2}if \(!refusal\) return false;\n/);
  assert.match(fn, /\n {2}return refuseStart\(t\(refusal\.title\), t\(refusal\.body, refusal\.params\)\);\n\}/);
  // no decision of its own: one return false, no other key, no other condition
  assert.equal(fn.match(/return false/g).length, 1);
  assert.doesNotMatch(fn, /startup\.|refusedArgs|sandboxDisabledByEnv|refuseDebugging|\.kind/);
  assert.equal((fn.match(/\bif \(/g) || []).length, 1);
  assert.doesNotMatch(fn, /`--\$\{/, 'no names put together here');
});

// ---- T14 finding 2: ELECTRON_DISABLE_SANDBOX ----
// Electron reads the variable before main.js runs and turns the sandbox off;
// with app.enableSandbox() the packaged app then crashed (0xC0000005) as its
// window opened, without a word. Measured with Electron 44.9.3 on Windows:
// "1", "0" and "false" add --no-sandbox and crash, an empty value does
// neither (test/unit/switchprobe.test.js checks the rule on the real binary).

test('the packaged app refuses to start with ELECTRON_DISABLE_SANDBOX, whatever its value', () => {
  const { sandboxDisabledByEnv, SANDBOX_ENV } = require('../../src/main/lib/debugflags');
  assert.equal(SANDBOX_ENV, 'ELECTRON_DISABLE_SANDBOX');
  const off = (env, platform = 'win32', isPackaged = true) => sandboxDisabledByEnv({ isPackaged, env, platform });
  for (const platform of ['win32', 'darwin', 'linux']) {
    for (const v of ['1', '0', 'false', 'no', ' ', 'true']) {
      assert.equal(off({ ELECTRON_DISABLE_SANDBOX: v }, platform), true, `${platform} ${JSON.stringify(v)}`);
    }
    assert.equal(off({}, platform), false, platform);
    assert.equal(off({ PATH: 'x', ELECTRON_ENABLE_LOGGING: '1', ELECTRON_DISABLE_SANDBOXX: '1' }, platform), false, platform);
    assert.equal(off({ ELECTRON_DISABLE_SANDBOX: undefined }, platform), false, `${platform} deleted`);
    // development and Playwright runs are left alone; the acceptance runs'
    // switch is not (review R54 S-3: such a start crashes anyway)
    assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '1' }, platform, false), false, `${platform} development`);
    assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '1', LECTUREFLOW_E2E_ALLOW_DEBUG: '1' }, platform), true, `${platform} e2e`);
  }
  // Windows: names without case, and an empty value counts as unset (Chromium's HasVar)
  assert.equal(off({ electron_disable_sandbox: '1' }, 'win32'), true);
  assert.equal(off({ Electron_Disable_Sandbox: '0' }, 'win32'), true);
  assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '' }, 'win32'), false);
  // elsewhere: an empty value is set (getenv finds it); the names are the next test's
  assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '' }, 'linux'), true);
  assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '' }, 'darwin'), true);
  assert.equal(sandboxDisabledByEnv({ isPackaged: true, env: undefined, platform: 'win32' }), false);
});

// v3.3.0 CI on macos-15: with electron_disable_sandbox=1 the real Electron
// added --no-sandbox, while the app read exact names only off Windows
// (switchprobe.test.js: "the app and Electron agree", false !== true).
// Chromium's Environment::GetVar looks the name up as written, then once in
// the reverse case (all lower case for this name); Windows ignores case.
test('ELECTRON_DISABLE_SANDBOX names: any case on Windows; elsewhere exact or all lower case', () => {
  const { sandboxDisabledByEnv } = require('../../src/main/lib/debugflags');
  const off = (env, platform) => sandboxDisabledByEnv({ isPackaged: true, env, platform });
  const cases = [
    // [name, value, win32, darwin and linux]
    ['ELECTRON_DISABLE_SANDBOX', '1', true, true],
    ['electron_disable_sandbox', '1', true, true],
    ['electron_disable_sandbox', '0', true, true],
    ['Electron_Disable_Sandbox', '1', true, false],
    ['ELECTRON_disable_SANDBOX', '1', true, false],
    ['eLECTRON_DISABLE_SANDBOX', '1', true, false],
    // an empty value: unset on Windows whatever the case, set elsewhere for the names getenv is asked about
    ['ELECTRON_DISABLE_SANDBOX', '', false, true],
    ['electron_disable_sandbox', '', false, true],
    ['Electron_Disable_Sandbox', '', false, false],
    // not the variable on any platform
    ['ELECTRON_DISABLE_SANDBOXX', '1', false, false],
    ['electron_disable_sandbox_', '1', false, false],
    [' ELECTRON_DISABLE_SANDBOX', '1', false, false],
    ['ELECTRON_DISABLE_SANDBOX ', '1', false, false],
  ];
  for (const [name, value, win, posix] of cases) {
    const env = { PATH: 'x', [name]: value };
    assert.equal(off(env, 'win32'), win, `win32 ${JSON.stringify(name)}=${JSON.stringify(value)}`);
    for (const platform of ['darwin', 'linux']) {
      assert.equal(off(env, platform), posix, `${platform} ${JSON.stringify(name)}=${JSON.stringify(value)}`);
    }
  }
  // a deleted variable is no variable, whatever the name
  for (const platform of ['win32', 'darwin', 'linux']) {
    assert.equal(off({ electron_disable_sandbox: undefined }, platform), false, `${platform} deleted`);
    assert.equal(off({ electron_disable_sandbox: null }, platform), false, `${platform} null`);
  }
  // both spellings off Windows (Linux keeps them apart): either one is enough
  assert.equal(off({ ELECTRON_DISABLE_SANDBOX: '1', electron_disable_sandbox: '' }, 'linux'), true);
  assert.equal(off({ ELECTRON_DISABLE_SANDBOX: undefined, electron_disable_sandbox: '1' }, 'linux'), true);
  // development is left alone, whatever the name
  for (const platform of ['win32', 'darwin', 'linux']) {
    assert.equal(sandboxDisabledByEnv({ isPackaged: false, env: { electron_disable_sandbox: '1' }, platform }), false, `${platform} development`);
  }
});

test('--no-sandbox on the command line is refused as an unsupported option', () => {
  for (const platform of ['win32', 'darwin', 'linux']) {
    for (const a of ['--no-sandbox', '-no-sandbox', '--NO-SANDBOX', '--no-sandbox=1', '--no-sandbox=']) {
      assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [a], platform }), ['no-sandbox'], `${platform} ${a}`);
    }
    assert.deepEqual(refuseDebugging({ isPackaged: true, argv: ['--lang=en', '--no-sandbox', '--updated'], platform }), ['no-sandbox'], platform);
  }
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: ['/no-sandbox'], platform: 'win32' }), ['no-sandbox']);
  // the --no-sandbox Electron appends for ELECTRON_DISABLE_SANDBOX is not on the argv:
  // that case is the environment check's (Chromium's parse is never asked about it)
  assert.deepEqual(refuseDebugging({ isPackaged: true, argv: [], hasSwitch: (s) => s === 'no-sandbox', platform: 'win32' }), []);
});

// ---- review R54 M-1: whether to refuse, and with which box, is one pure function ----

test('startRefusal: options, no sandbox, both, neither; development and the e2e switch', () => {
  const { startRefusal, REFUSAL_TEXT } = require('../../src/main/lib/debugflags');
  const ARGS = { kind: 'args', title: 'startup.unsupported_args.title', body: 'startup.unsupported_args.body' };
  const SANDBOX = { kind: 'sandbox', title: 'startup.sandbox_disabled.title', body: 'startup.sandbox_disabled.body', params: {} };
  const ENV = { ELECTRON_DISABLE_SANDBOX: '1' };
  const E2E = { LECTUREFLOW_E2E_ALLOW_DEBUG: '1' };
  // Electron's own parse: --no-sandbox when it turned the sandbox off, and what it always appends
  const electron = (noSandbox) => (s) => s === 'allow-file-access-from-files' || (noSandbox && s === 'no-sandbox');
  const run = ({ argv = [], env = {}, noSandbox = false, isPackaged = true, platform = 'win32' } = {}) => startRefusal({
    isPackaged, argv, env, hasSwitch: electron(noSandbox), platform,
  });
  for (const platform of ['win32', 'darwin', 'linux']) {
    const at = (o) => run({ platform, ...o });
    // neither
    assert.equal(at({}), null, platform);
    assert.equal(at({ argv: ['--background', '--lang=en', '--disable-gpu'] }), null, platform);
    // only options
    assert.deepEqual(at({ argv: ['--disable-gpu=1', '--foo'] }), { ...ARGS, params: { switches: '--disable-gpu=1, --foo' } }, platform);
    // only no sandbox: by the variable, by Electron's switch, or both
    assert.deepEqual(at({ env: ENV }), SANDBOX, `${platform} variable`);
    assert.deepEqual(at({ noSandbox: true }), SANDBOX, `${platform} Electron's --no-sandbox`);
    assert.deepEqual(at({ env: ENV, noSandbox: true }), SANDBOX, platform);
    assert.deepEqual(at({ env: ENV, noSandbox: true, argv: ['--background'] }), SANDBOX, `${platform} the extension's start`);
    // both: the options first (review R54 S-5)
    assert.deepEqual(at({ env: ENV, noSandbox: true, argv: ['--foo'] }), { ...ARGS, params: { switches: '--foo' } }, platform);
    assert.deepEqual(at({ env: ENV, argv: ['--no-sandbox'] }), { ...ARGS, params: { switches: '--no-sandbox' } }, platform);
    // development: never
    for (const o of [{ env: ENV }, { noSandbox: true }, { argv: ['--foo'] }, { env: ENV, argv: ['--foo'] }]) {
      assert.equal(at({ ...o, isPackaged: false }), null, `${platform} development ${JSON.stringify(o)}`);
    }
    // the e2e switch: options pass as before, a start without the sandbox does not (review R54 S-3)
    assert.equal(at({ env: E2E, argv: ['--remote-debugging-port=0', '--foo'] }), null, platform);
    assert.deepEqual(at({ env: { ...E2E, ...ENV } }), SANDBOX, platform);
    assert.deepEqual(at({ env: E2E, noSandbox: true }), SANDBOX, platform);
    assert.deepEqual(at({ env: { ...E2E, ...ENV }, argv: ['--foo'] }), SANDBOX, `${platform}: e2e lets --foo through, not the variable`);
    // --no-sandbox written by the person under the e2e switch is theirs, as before: not Electron's
    assert.equal(at({ env: E2E, noSandbox: true, argv: ['--no-sandbox'] }), null, platform);
  }
  // the Windows rule of the variable (an empty value is unset there; Electron agrees, no switch)
  assert.equal(run({ env: { ELECTRON_DISABLE_SANDBOX: '' } }), null);
  assert.deepEqual(run({ env: { electron_disable_sandbox: '0' } }), SANDBOX);
  // elsewhere (v3.3.0 CI on macOS): the all-lower-case name counts by itself, a mixed-case
  // one only when Electron says it turned the sandbox off
  for (const platform of ['darwin', 'linux']) {
    assert.deepEqual(run({ platform, env: { electron_disable_sandbox: '1' } }), SANDBOX, platform);
    assert.equal(run({ platform, env: { Electron_Disable_Sandbox: '1' } }), null, platform);
    assert.deepEqual(run({ platform, env: { Electron_Disable_Sandbox: '1' }, noSandbox: true }), SANDBOX, platform);
  }
  assert.equal(startRefusal(undefined), null);
  // a hasSwitch that throws is no switch
  assert.equal(startRefusal({ isPackaged: true, argv: [], env: {}, hasSwitch: () => { throw new Error('x'); }, platform: 'win32' }), null);
  // the keys exist in every locale
  const read = (l) => JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', 'locales', `${l}.json`), 'utf8'));
  for (const l of ['en', 'zh-CN', 'zh-TW', 'yue-Hant-HK', 'ru', 'hi']) {
    const m = read(l);
    for (const t of Object.values(REFUSAL_TEXT)) for (const k of [t.title, t.body]) assert.ok(m[k], `${l} ${k}`);
  }
});

test('startRefusal asks Chromium\'s parse about the debugging switches and no-sandbox only', () => {
  const { startRefusal, HASSWITCH_SWITCHES } = require('../../src/main/lib/debugflags');
  const asked = [];
  startRefusal({ isPackaged: true, argv: [], env: {}, hasSwitch: (x) => { asked.push(x); return false; }, platform: 'win32' });
  assert.deepEqual(asked.sort(), [...HASSWITCH_SWITCHES, 'no-sandbox'].sort());
});

test('main.js refuses a start without the sandbox with the same box and exit code, before enableSandbox', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const box = src.slice(src.indexOf('function refuseStart'), src.indexOf('function refuseDebugSwitches'));
  assert.match(box, /dialog\.showErrorBox\(title, body\)/);
  assert.match(box, /app\.exit\(2\)/);
  const body = src.slice(src.indexOf('function main() {'));
  assert.ok(body.indexOf('refuseDebugSwitches()') < body.indexOf('app.enableSandbox()'), 'checked before app.enableSandbox()');
  assert.ok(body.indexOf('app.enableSandbox()') > 0);
  const read = (l) => JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', 'locales', `${l}.json`), 'utf8'));
  for (const l of ['en', 'zh-CN', 'zh-TW', 'yue-Hant-HK', 'ru', 'hi']) {
    assert.match(read(l)['startup.sandbox_disabled.body'], /ELECTRON_DISABLE_SANDBOX/, `${l}: names the variable as it is spelled`);
  }
  const en = read('en');
  const zh = read('zh-CN');
  assert.match(en['startup.sandbox_disabled.body'], /environment variable ELECTRON_DISABLE_SANDBOX.*sandbox on.*Remove/s);
  assert.match(zh['startup.sandbox_disabled.body'], /环境变量 ELECTRON_DISABLE_SANDBOX.*沙箱开启.*去掉/s);
  // review R54 S-4: any value counts; a Windows shortcut carries no variables;
  // the terminal or program that starts it keeps the old environment
  assert.match(en['startup.sandbox_disabled.body'], /\b0 or false\b/);
  assert.match(en['startup.sandbox_disabled.body'], /script or terminal/);
  assert.match(en['startup.sandbox_disabled.body'], /reopen the terminal or program/);
  assert.match(zh['startup.sandbox_disabled.body'], /0 或 false/);
  assert.match(zh['startup.sandbox_disabled.body'], /脚本、终端/);
  assert.match(zh['startup.sandbox_disabled.body'], /重新打开.*终端或程序/);
  for (const m of [en, zh]) assert.doesNotMatch(m['startup.sandbox_disabled.body'], /shortcut|快捷方式/);
  // main.js refuseStart shows the box before app.exit(2): "will close", not "has closed"
  for (const k of ['startup.unsupported_args.body', 'startup.sandbox_disabled.body']) assert.doesNotMatch(zh[k], /已关闭/, k);
  // the other locales: language-independent checks that hold before and after
  // translation (translation quality is for the native reviewers). The
  // variable name is checked by the loop above.
  for (const l of ['zh-TW', 'yue-Hant-HK', 'ru', 'hi']) {
    const m = read(l);
    const title = m['startup.sandbox_disabled.title'];
    const body = m['startup.sandbox_disabled.body'];
    for (const s of [title, body]) assert.ok(typeof s === 'string' && s.trim() !== '', `${l}: non-empty`);
    // the values 0 and false as written; no spaces or \b around them (zh, hi)
    assert.match(body, /(^|[^0-9])0([^0-9]|$)/, `${l}: names the value 0`);
    assert.match(body, /false/, `${l}: names the value false`);
    // these two texts take no parameters
    for (const s of [title, body]) assert.doesNotMatch(s, /[{}]/, `${l}: no placeholder`);
  }
});

test('the packaged app drops the development switches of its environment', () => {
  const { devEnvToDrop } = require('../../src/main/lib/debugflags');
  const env = {
    LECTUREFLOW_DEV_EXTENSION: '1',
    LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK: '1',
    lectureflow_dev_power: 'C:\\p.json',
    LECTUREFLOW_FAKE_UPDATER: 'x.js',
    LECTUREFLOW_TEST_SELFUPDATE: '{}',
    LECTUREFLOW_TEST_APPDATA: 'C:\\t',
    LECTUREFLOW_SERVER: 'C:\\s.exe',
    LECTUREFLOW_RESOURCES: 'C:\\r',
    // kept: settings of the product, and the acceptance runs' switches
    LECTUREFLOW_DATA_DIR: 'C:\\data',
    LECTUREFLOW_E2E_ALLOW_DEBUG: '1',
    LECTUREFLOW_NM_ENDPOINT: 'off',
    PATH: 'C:\\Windows',
  };
  assert.deepEqual(devEnvToDrop({ isPackaged: true, env }).sort(), [
    'LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK', 'LECTUREFLOW_DEV_EXTENSION', 'LECTUREFLOW_FAKE_UPDATER', 'LECTUREFLOW_RESOURCES',
    'LECTUREFLOW_SERVER', 'LECTUREFLOW_TEST_APPDATA', 'LECTUREFLOW_TEST_SELFUPDATE', 'lectureflow_dev_power',
  ]);
  assert.deepEqual(devEnvToDrop({ isPackaged: false, env }), [], 'development keeps them');
  assert.deepEqual(devEnvToDrop({ isPackaged: true, env: undefined }), []);
});

test('main.js drops them right after the switch check, before anything reads the environment', () => {
  const src = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'main', 'main.js'), 'utf8');
  const body = src.slice(src.indexOf('function main() {'));
  assert.match(body, /^function main\(\) \{\n {2}if \(refuseDebugSwitches\(\)\) return;\n {2}dropDevEnvironment\(\);\n/);
  const fn = src.slice(src.indexOf('function dropDevEnvironment'), src.indexOf('function main() {'));
  assert.match(fn, /devEnvToDrop\(\{ isPackaged: app\.isPackaged, env: process\.env \}\)/);
  assert.match(fn, /delete process\.env\[k\]/);
});
