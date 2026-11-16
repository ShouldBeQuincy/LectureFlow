# Contributing a platform profile / 贡献录播平台对策文件

[English](#english) · [简体中文](#简体中文)

## English

The LectureFlow browser extension finds the audio and video tracks of a lecture recording on any site by itself (generic detection). A **platform profile** teaches it about one recording platform: which pages are lecture players, where the lecture's ID and title are, what each track is called and which ones to tick by default, and whether the stream may be restricted by the lecturer's or university's policy. Support for new platforms comes from profiles contributed by students, so no change to the extension's code is needed.

A profile is a JSON file of declarations only. It cannot contain scripts. It filters, names and describes what the extension finds on the page; the one request a profile can add is `sessionInfo`: after you click to send from a folder list, the extension asks the platform, at the page's own address (same origin), for the details of each lecture being sent, as the platform's own player page does. At worst a wrong profile gives a track the wrong name or fails to read a lecture's details; it cannot make the extension request another site. A profile you import locally (unreviewed) may only use `GET` for `sessionInfo`; `POST` is used only once the profile has been reviewed in a pull request and ships with the extension. Profiles and their fixtures are released under **CC0 1.0**, and a pull request that only contains them needs **no CLA** (see [Licence](#licence)).

### Using an AI coding agent

If you work with an AI coding agent (Claude Code, Codex and the like) that can control your browser, point it at [`extension/AGENTS.md`](extension/AGENTS.md): it tells the agent what to observe on the platform's pages, what it must never record, and how to turn that into a profile and fixtures. You still sign in, allow the site and export the report yourself, and you review everything before the pull request.

### Before you start

You need:

- at least one recording on the platform that you are allowed to watch; two or more is better (one restricted and one not, one with a camera and one without);
- the browser's developer tools (Elements and Network panels);
- the extension installed (see [docs/install.md](docs/install.md#browser-extension)) and LectureFlow running, so you can test the whole path;
- a GitHub account, and about an hour.

Open a recording, **start playback**, then click the extension's icon:

- "N tracks found": carry on. A profile only names and recommends tracks; it does not find them.
- "No tracks found": check that playback has started. If the player sits in an iframe from another site (for example inside a Canvas or Moodle page), allow the player's site too (Settings › Sites › Allow site).
- Tracks marked "Copyright protected (DRM)", with the popup saying "This stream is protected by digital rights management (DRM). LectureFlow does not handle protected content.": the page uses DRM. LectureFlow does not support such platforms; please do not submit a profile.
- "LectureFlow does not work on this site": the site is on the extension's do-not-handle list ([`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js)): **YouTube**, and **MOOCs and online or paid course platforms** (Coursera, edX, Udemy, LinkedIn Learning, 中国大学 MOOC, Stepik, SWAYAM and the like). LectureFlow is only for the lecture recordings your own school gives you. Profiles for these sites are not accepted, and CI refuses them.

### Step 1: export a detection report

In the popup, click **Export detection report**. You get `lectureflow-report-<platform>-<date>.json`.

The report contains **only**:

- the scheme, host and path of addresses. ID-like or token-like parts of a path (UUIDs, long numbers, long letter-and-digit runs, tickets, and shorter runs that do not read as words and numbers) are replaced by fakes of the same shape, the same ID always by the same fake, so your profile's patterns still match; words with numbers such as `Week10Lecture2` or `1920x1080p30` stay;
- the **names** of query parameters that have a value; every value becomes a placeholder such as `{uuid-1}` or `{tok120-3}`; a parameter without a value stays only when it is a common switch (such as `embed` or `download`) and becomes a placeholder otherwise;
- language fields (DASH `lang`, HLS `LANGUAGE`) only when they are language tags in their usual case (`en`, `en-GB`, `zh-Hant-HK`); anything else is reduced to its shape;
- playlists (m3u8, mpd) rebuilt from what the extension understands: addresses as above, numbers and enumerated attributes kept, free text reduced to its shape;
- the structure of the page nodes your profile's selectors hit (tag, id, classes, attribute names), their text reduced to its shape (letters become x / X, digits 0).

The extension **never reads cookies** for a report. If anything in it contains `@` or a field that looks like a name, user name or student number, it refuses to export and tells you what kind of finding it was. **Do not edit a report by hand to get around this**: an edited report fails the replay in CI, and it is easy to miss something. Open an issue instead, saying only what kind of finding it was and where.

After exporting, read the file yourself and search it for your name, student number, e-mail user name, your university's name or abbreviation, course codes and lecturers' names.

### Step 2: find what the profile needs

The complete format is [`extension/schema/profile.schema.json`](extension/schema/profile.schema.json); [`extension/profiles/panopto.json`](extension/profiles/panopto.json) is the worked example. The fields you will use most:

| Field | Where to look | Notes |
|---|---|---|
| `match.hosts` | The host in the address bar | The platform's domains, with `*.` to cover every university and region (`*.example.com` also covers `example.com`). Never a sub-domain only your university uses, never a shared CDN or cloud storage domain (CI rejects `*.cloudfront.net`, `*.amazonaws.com` and the like), never a whole public suffix such as `*.ac.uk` |
| `match.pathPattern` | The player page's path | A regular expression tested against the path only (no query), case-insensitive. Match **single-lecture player pages** only, never course lists or folders |
| `match.mediaHosts` | The host of the media requests in the Network panel | Only when the media is on another domain **and** needs cookies |
| `session.idFrom` | The lecture's ID | `{"query": "id"}` (a query parameter), `{"pathPattern": "^/lesson/([^/]+)"}` (exactly one capture group) or `{"selector": …}`. It must be the same every time the same lecture is opened; never an expiring session token. It is LectureFlow's key for "already imported" |
| `lecture.title`, `lecture.recordedAt`, `lecture.durationSec` | Elements panel: right-click the title › Inspect | A CSS selector, optionally `attr` and a `pattern` (its first capture group is kept); several can be tried in order. Prefer an `id` or a stable class, never generated classes such as `css-1x2y3z`. **These three fields are all there is**: course names, course codes, folders and people are never read, and the schema has no field for them. For `recordedAt`, read the date and time shown to people on the page (the school's local time), not a UTC timestamp: it is kept as written, never converted to the viewer's time zone |
| `tracks[].detect` | Network panel: filter `m3u8`, `mpd`, `mp4`, start playback | `urlPattern` is tested against the full address **without its query**; describe the path, not a CDN's random host name. Add `kind` (audio / video) and `format` (hls / dash / file). Rules apply in order, first match wins |
| `tracks[].recommended` | — | One send takes at most one audio track and two video tracks (“Two cameras”): recommend what transcription needs most |
| `observe` | Network panel: the page's own JSON requests | Only when the information exists nowhere else (Panopto's restriction setting and full MP4 are in a JSON answer). It names a request the page already makes and JSON pointers to single values; the extension keeps only those values. Never point at personal fields |
| `tracks[].detect.fromResponse` | The JSON answer `observe` reads | Names declared under `observe[].pointers` whose values are media addresses (Panopto's full MP4): they become tracks even when the player never loaded them. The extension requests nothing for them |
| `tracks[].supersedes` | — | Roles that lose their recommendation when this rule has a recommended track (Panopto: the full MP4 replaces the separate audio and pictures) |
| `presets` | — | The roles behind the cards' three choices, “Main audio & video”, “Two cameras” and “Audio only”: `full` (a track that is picture and sound in one) and `audio` (the main audio). Without it, the first audio track and the video tracks in list order are used |
| `inpage` | Elements panel on the player page and on the folder list | Where the extension's own “Send to LectureFlow” button goes, on sites you allowed: `player` (a stable anchor in the player page's header) and `list` (the row element, where each row's ID and title are, and where “Update this course in LectureFlow” goes). Check both the platform's own layout and the one inside the LMS iframe |
| `sessionInfo` | Network panel on the player page: the request whose answer holds the lecture's tracks | Only with `inpage.list`, when the folder list itself carries no track information: method, path and form fields (`{sessionId}` is the only placeholder) of a request to the page's own origin. Sent only after the user clicks to send, only for the lectures being sent; its answer is read like the `observe` entry it names. A locally imported (unreviewed) profile may only use `GET`; `POST` needs review and ships with the extension |
| `downloadPolicy.disabledWhen` | Compare a **restricted** lecture with one that is **not** (for example one where the lecturer turned off the platform's own download option) | Only with a reliable marker (a selector or an observed value). A match shows the restriction notice in the popup. If unsure, leave it out: the popup then says nothing |
| `request.cookieUrls` | See below | `[]` or `["track"]` |
| `request.maxConnectionsPerFile` | — | 4 by default; lower it if the platform throttles |

**Does the media need cookies?** In the Network panel, right-click a playlist request › Copy › Copy as cURL, paste it into a terminal on your own computer, **delete the cookie option** (`-b '…'` or `-H 'cookie: …'`) and run it. Still 200: `"cookieUrls": []`. 401 or 403: `"cookieUrls": ["track"]`. The copied command contains your sign-in: clear it when you are done and **never paste it into an issue, a pull request or a chat**.

**Embedded in an LMS and the media needs cookies?** Firefox (and Chrome when third-party cookies are blocked) keeps the cookies a player sets inside another site's page apart, under that site (partitioned cookies). The extension reads that partition too, but only the one of the tab you send from, which it identifies from the site of the top page (the LMS), with the toolbar icon or with the in-page button alike. Send once from inside the LMS page too (the first time, a small confirmation window opens because the button sits in a page embedded in another site; you can trust the LMS site there), and say in the pull request which way you sent.

### Step 3: write the profile

Copy `extension/profiles/panopto.json` to `extension/profiles/<id>.json` and adapt it:

- `id`: lower-case letters, digits and hyphens, starting with a letter; the file name must be `<id>.json`. `generic` is reserved.
- `version`: a new platform starts at `0.1.0`. Later, a selector or pattern fix bumps the patch number; new track rules or fields bump the minor number; a change to `match` or `session` (it affects "already imported") bumps the major number.
- `maintainers`: your GitHub handle, such as `"@your-handle"`.
- `label`: needs at least `en`; add the languages you can.
- `fixtures`: `"fixtures/<id>/"`.
- Not verified on real pages yet? Add `"draft": true`: it is checked but not packaged, and needs no fixtures.

### Step 4: try it in the extension

1. Open the extension's settings, **Platform profiles › Import a local profile**, and pick your file. Errors are listed one per field.
2. Open a recording, start playback, click the icon. The popup uses your track names and marks the profile **"Not reviewed"**. A local import takes precedence over a bundled profile with the same id, so you can improve an existing platform the same way.
3. Check track names and count, recommended ticks, title, recording date and the restriction notice; then try another lecture.
4. Send the same lecture to LectureFlow twice: the second time it says it is already imported, which shows the session ID is stable.
5. Re-import after each change.
6. Finally **export the report again** with your profile imported, once per scenario. These reports are your fixtures: CI replays them.

### Step 5: open a pull request

Add:

- `extension/profiles/<id>.json`
- `extension/fixtures/<id>/<scenario>.json`, the reports from step 4, one per scenario (`camera.report.json`, `restricted.report.json`, …). Name files after the scenario, never after a university.

Open the pull request with the profile template: add `?template=profile.md` to the "Open a pull request" address (or `&template=profile.md` when it already has a `?`), and fill in the checklist. Give the platform's name, the regional domain you tested on (not your university), the scenario of each report, whether cookies are needed and how you confirmed the restriction marker.

### What CI checks

The **Extension** workflow ([`.github/workflows/extension.yml`](.github/workflows/extension.yml)) runs on every pull request:

1. **Profiles and fixtures** ([`scripts/profilecheck/check.mjs`](scripts/profilecheck/check.mjs)), with one readable line per problem and a summary on the run's page:
   - the JSON Schema and the rules a schema cannot express (every regular expression compiles and cannot backtrack out of control: no backreferences or modifier groups, repeated groups of fixed width only, at most two `*` / `+` / `{n,}` never in a row, and few choices from `?`, `{n,m}` and `|` weighted by the width of what they apply to and of what must follow them, as set out in [docs/extension/adding-a-platform.md](docs/extension/adding-a-platform.md), which also describes the runtime guard that stops a profile whose regex is slow in use; one capture group in `session.idFrom.pathPattern`, unique roles), with the same code the extension uses when you import a profile;
   - the file is named after its `id`; a profile that is not a draft has fixtures; hosts are the platform's own;
   - nothing on the do-not-handle list ([`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js): YouTube, MOOCs and online or paid course platforms): no host or media host of those sites, and on a site where only some paths are refused (`linkedin.com/learning`, `bilibili.com/cheese`) a `match.pathPattern` that starts with `^/` and a fixed first path segment other than the refused one, so it can never reach those pages; the extension's "Import local profile" refuses the same;
   - only reports in `extension/fixtures/<id>/`, exported with this profile;
   - a **redaction scan** of every fixture, independent of the extension's own: cookie or authorisation headers, session cookies, `@`, tokens (JWT, bearer, signatures), query values that are not placeholders, real UUIDs, hexadecimal IDs, long numbers and tokens in addresses, fields that name a person. It says where and what kind, never the value.
2. **Fixture replay** (`npm test`): generic detection runs again on what each report recorded and must find the same tracks; then your profile must name and recommend them as the report says.
3. The Chrome and Firefox builds, and the end-to-end tests in a headless Chromium.

Run the same checks before you push (Node.js 24):

    node scripts/profilecheck/check.mjs
    cd extension
    npm ci
    npm test

### Redaction rules (non-negotiable)

- **A fixture that contains cookies, a person's name or a student number is never merged.**
- Not accepted either: e-mail addresses, university account names, lecturers' names, course names or codes, signatures or tokens, screenshots, screen recordings, HAR files.
- Only reports exported by the extension, unedited. A draft profile without fixtures is better than a hand-edited fixture.
- Maintainers check again before merging. If something slips through, you will be asked to export again, and it will be removed from the pull request's history if necessary.

### Licence

Platform profiles (`extension/profiles/*.json`) and fixtures (`extension/fixtures/`) are dedicated to the public domain under **CC0 1.0 Universal** ([`extension/profiles/LICENSE`](extension/profiles/LICENSE)). By opening a pull request with them you agree to release them under CC0 and confirm that they contain nobody else's protected material.

A pull request that **only** adds, changes or removes such files needs no Contributor Licence Agreement: the CLA check sees that and passes. A pull request that also touches anything else (the extension's code, documentation, `extension/profiles/LICENSE`) needs the [CLA](CLA.md) as usual, see [CONTRIBUTING.md](CONTRIBUTING.md). The extension's code stays under the project's AGPL-3.0-or-later.

### What we do not accept

- scripts, `eval`, remote addresses or any "dynamic rules";
- selectors or observed values that read course, folder, lecturer or student information;
- anything that gets around DRM, sign-in or paywalls;
- requests for the extension to fetch addresses the page did not fetch;
- profiles for the sites the extension leaves alone — YouTube, and MOOCs and online or paid course platforms ([`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js); CI refuses them) — or for a whole shared CDN.

### After the merge

A merged profile ships with the next release of the extension. Each profile has its own version number, so a broken change can be reverted on its own. Platforms with a draft written from public sources, waiting for someone with an account to verify them, are in [`docs/extension/drafts/`](docs/extension/drafts/): exporting reports there (steps 1 and 4) and correcting the draft is one of the most useful contributions.

---

## 简体中文

LectureFlow 浏览器扩展在任何网站上都能自己找出录播的音频、视频轨道（通用探测）。**对策文件**（platform profile）让它认识某一个录播平台：哪些页面是单节课的播放页，这节课的 ID 和标题在哪，每条轨道叫什么、默认勾选哪条，这条媒体流是否可能受讲师或大学政策限制。新平台的支持靠同学们提交对策文件完成，不需要改扩展代码。

对策文件是只含声明的 JSON，不能写脚本。它只对扩展在页面上找到的东西做筛选、命名和说明；唯一能让扩展多发的请求是 `sessionInfo`：你在文件夹列表上点了发送之后，扩展为要发送的每节课，向该页同源的平台地址请求讲座信息，与平台自己的播放页发出的请求相同。写错了，最多是轨道名字不对或读不到讲座信息，不会让扩展去请求别的网站。本地导入的（未审核）对策文件，`sessionInfo` 只能用 `GET`；`POST` 要经过 PR 审核、随扩展打包后才会使用。对策文件和 fixtures 以 **CC0 1.0** 发布，只包含它们的 PR **不需要签 CLA**（见[许可](#许可)）。

### 用 AI agent 协助

如果你用能操作浏览器的 AI 编程助手（Claude Code、Codex 等），把 [`extension/AGENTS.md`](extension/AGENTS.md) 交给它：里面写明了要在录播平台页面上观察什么、绝不能记录什么，以及怎样整理成对策文件和 fixtures。登录、授权站点、导出报告仍由你自己点，提 PR 前所有内容由你过目。

### 开始之前

你需要：

- 在这个平台上至少有一节你有权观看的录播，最好两节以上（一节受限、一节不受限；一节有摄像头、一节没有）；
- 浏览器开发者工具（Elements、Network 面板）；
- 已安装扩展（见 [docs/install.md](docs/install.md#浏览器扩展)），并打开 LectureFlow，以便试完整个流程；
- 一个 GitHub 账号，大约一小时。

打开一节录播，**开始播放**，然后点扩展图标：

- 显示“探测到 N 条轨道”：可以继续。对策文件只负责命名和推荐，不负责“找到”。
- 显示“没探测到轨道”：先确认已经开始播放；如果播放器在另一个网站的 iframe 里（例如嵌在 Canvas、Moodle 页面中），在设置 › 站点 › 允许站点里把播放器所在的网站也加上。
- 轨道标着“受版权保护（DRM）”，弹窗显示“这条媒体流受数字版权保护（DRM），LectureFlow 不处理受保护的内容。”：页面用了 DRM。LectureFlow 不支持这种平台，请不要提交对策文件。
- 弹窗显示“这个网站不在 LectureFlow 的支持范围内”：这个网站在扩展的不处理名单里（[`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js)）：**YouTube**，以及 **MOOC 与在线 / 付费课程平台**（Coursera、edX、Udemy、LinkedIn Learning、中国大学 MOOC、Stepik、SWAYAM 等）。LectureFlow 只用于学校给你的课堂录播。不接受这些网站的对策文件，CI 也会拒绝。

### 第 1 步：导出探测报告

在弹窗里点 **导出探测报告**，得到 `lectureflow-report-<平台>-<日期>.json`。

报告**只**包含：

- 地址的协议、域名和路径。路径里像 ID 或令牌的部分（UUID、长数字、长字母数字串、票据，以及读起来不像单词加数字的较短的串）换成形状相同的假值，同一个 ID 总是换成同一个假值，所以对策文件的正则照样能匹配；`Week10Lecture2`、`1920x1080p30` 这类单词加数字的写法保留；
- 有值的查询参数只留**名字**，值一律换成占位符，如 `{uuid-1}`、`{tok120-3}`；没有值的参数只有常见开关（如 `embed`、`download`）原样保留，其余也换成占位符；
- 语言字段（DASH 的 `lang`、HLS 的 `LANGUAGE`）只保留规范写法的语言标签（如 `en`、`en-GB`、`zh-Hant-HK`），其他内容只留形状；
- 播放清单（m3u8、mpd）按扩展能理解的内容重建：地址同上处理，数字和枚举属性保留，自由文本只留形状；
- 对策文件选择器命中的页面节点的结构（标签、id、class、属性名），文字只留形状（字母变 x / X，数字变 0）。

导出报告时扩展**从不读取 Cookie**。报告里只要出现 `@`，或像姓名、用户名、学号的字段，扩展就拒绝导出，并告诉你是哪一类发现。**不要手工改报告来绕过**：改过的报告在 CI 回放时会失败，也很容易漏掉东西。请改为开一个 issue，只说明是哪一类发现、出现在哪里。

导出之后自己再读一遍，在文件里搜索：你的姓名、学号、邮箱用户名、学校名称或缩写、课程代码、讲师姓名。

### 第 2 步：找齐对策文件要的信息

完整格式见 [`extension/schema/profile.schema.json`](extension/schema/profile.schema.json)；[`extension/profiles/panopto.json`](extension/profiles/panopto.json) 是完整的范例。最常用的字段：

| 字段 | 去哪里找 | 注意 |
|---|---|---|
| `match.hosts` | 地址栏的域名 | 写平台的域名，用 `*.` 覆盖所有学校和区域（`*.example.com` 也包含 `example.com` 本身）。不要写只有你们学校用的子域名，不要写公共 CDN 或云存储的域名（CI 会拒绝 `*.cloudfront.net`、`*.amazonaws.com` 等），不要写 `*.ac.uk` 这种整个公共后缀 |
| `match.pathPattern` | 播放页的路径 | 正则，只测路径（不含查询参数），不区分大小写。只匹配**单节课的播放页**，课程列表、文件夹页一律不匹配 |
| `match.mediaHosts` | Network 面板里媒体请求的域名 | 只在媒体在另一个域名、**并且**需要 Cookie 时才写 |
| `session.idFrom` | 这节课的 ID | `{"query": "id"}`（查询参数）、`{"pathPattern": "^/lesson/([^/]+)"}`（恰好一个捕获组）或 `{"selector": …}`。同一节课每次打开都必须一样，不能用会过期的会话令牌。LectureFlow 用它判断“已导入” |
| `lecture.title`、`lecture.recordedAt`、`lecture.durationSec` | Elements 面板：在标题上右键 › 检查 | CSS 选择器，可选 `attr` 与 `pattern`（取第一个捕获组）；可以给多个，按顺序尝试。尽量用 `id` 或稳定的 class，不要用 `css-1x2y3z` 这类自动生成的 class。**只有这三个字段**：课程名、课程代码、文件夹和任何人的信息都不读，schema 里也没有对应字段。`recordedAt` 要读页面上给人看的日期与时间（学校当地时间），不要读 UTC 时间戳：它按所写的原样保存，不换算到看的人所在的时区 |
| `tracks[].detect` | Network 面板：筛选 `m3u8`、`mpd`、`mp4`，开始播放后看请求 | `urlPattern` 测的是**去掉查询参数**的完整地址；按路径规律写，不要写 CDN 的随机域名。配上 `kind`（audio / video）和 `format`（hls / dash / file）。规则按顺序先到先得 |
| `tracks[].recommended` | — | 一次最多送入一条音频、两条视频（双机位）：推荐转写最需要的那条 |
| `observe` | Network 面板：页面自己发出的 JSON 请求 | 只在信息别处都没有时才用（Panopto 的受限设置和完整 MP4 只在一个 JSON 响应里）。写明页面本来就会发出的请求和指向单个值的 JSON 指针，扩展只保留这几个值。不要指向任何个人信息字段 |
| `tracks[].detect.fromResponse` | `observe` 读的那个 JSON 响应 | 写 `observe[].pointers` 里声明过的名字，它们的值是媒体地址（Panopto 的完整 MP4）：即使播放器没加载过，也成为轨道。扩展不为它们发请求 |
| `tracks[].supersedes` | — | 这条规则有推荐的轨道时，这些角色不再推荐（Panopto：有完整 MP4 时，单独的音频和画面不再推荐） |
| `presets` | — | 卡片上三个选项“主要音视频”“双机位”“仅音频”背后的角色：`full`（画面和声音在一起的轨道）与 `audio`（主讲音频）。不写时用第一条音频轨道，画面按列表顺序取 |
| `inpage` | 在播放页和文件夹列表上用 Elements 面板看 | 扩展自己的“发送至 LectureFlow”按钮放在哪（只在你允许的站点上）：`player`（播放页页头里一个稳定的锚点）和 `list`（行元素、每行的 ID 和标题在哪、“更新这门课程到 LectureFlow”放在哪）。平台自己的版式和嵌在 LMS iframe 里的版式都要看 |
| `sessionInfo` | 在播放页的 Network 面板里找响应中带轨道信息的那个请求 | 只在有 `inpage.list`、而文件夹列表本身没有轨道信息时才写：向该页同源地址发的请求的方法、路径与表单字段（只有 `{sessionId}` 一个占位符）。只在用户点了发送之后、只为要发送的那几节发出；响应按它指定的 `observe` 条目读取。本地导入（未审核）的对策文件只能用 `GET`；`POST` 要经过审核、随扩展打包后才使用 |
| `downloadPolicy.disabledWhen` | 比较一节**受限**和一节**不受限**的课（例如讲师关闭了平台自己的下载选项的那一节） | 只有可靠的标记（选择器或观察到的值）时才写；命中时弹窗显示受限提示。拿不准就不写，弹窗不会提示 |
| `request.cookieUrls` | 见下 | `[]` 或 `["track"]` |
| `request.maxConnectionsPerFile` | — | 默认 4；平台限流时调低 |

**媒体要不要 Cookie**：在 Network 面板对一个播放清单请求右键 › Copy › Copy as cURL，粘到你自己电脑的终端里，**删掉带 Cookie 的那一项**（`-b '…'` 或 `-H 'cookie: …'`）再运行。仍然返回 200：写 `"cookieUrls": []`；返回 401 或 403：写 `"cookieUrls": ["track"]`。复制出来的命令含有你的登录凭据，用完立即清掉，**不要贴到 issue、PR 或聊天里**。

**嵌在 LMS 里、媒体又需要 Cookie？** Firefox（以及屏蔽了第三方 Cookie 的 Chrome）会把播放器在别的网站页面里设置的 Cookie 单独存放在那个网站名下（分区 Cookie）。扩展也读这一分区，但只读你发送时所在标签页的那一份，它按顶层页面（LMS）的网站来认这个分区，点工具栏图标和用页内按钮都一样。请在 LMS 页面里也发送一次（第一次会弹出一个小的确认窗口，因为按钮位于嵌在别的网站里的页面中；可以在窗口里信任这个 LMS 网站），并在 PR 里写明是用哪种方式发送的。

### 第 3 步：写对策文件

把 `extension/profiles/panopto.json` 复制为 `extension/profiles/<id>.json` 再修改：

- `id`：小写字母开头，只含小写字母、数字和连字符；文件名必须是 `<id>.json`。`generic` 是保留字。
- `version`：新平台从 `0.1.0` 开始。以后只改选择器或正则修 bug，升补丁号；加轨道规则或字段，升次版本号；改 `match` 或 `session`（会影响“已导入”判断），升主版本号。
- `maintainers`：你的 GitHub 账号，如 `"@your-handle"`。
- `label`：至少要有 `en`，能写几种语言就写几种。
- `fixtures`：`"fixtures/<id>/"`。
- 还没在真实页面上验证完：加 `"draft": true`。草稿照样检查，但不会打包进扩展，也不需要 fixtures。

### 第 4 步：在扩展里试用

1. 打开扩展设置，在 **对策文件 › 导入本地对策文件** 里选你的文件。有错误会按字段逐条列出。
2. 打开录播页、开始播放、点扩展图标。弹窗会用你的轨道名称，并把对策文件标为 **“未审核”**。本地导入的文件优先于扩展自带的同名文件，改进已有平台也用同样的办法。
3. 核对轨道名称和数量、推荐勾选、标题、录制日期、受限提示；换一节课再看一次。
4. 同一节课送进 LectureFlow 两次，第二次应提示已导入，说明会话 ID 是稳定的。
5. 每次修改后重新导入。
6. 最后在导入了你的对策文件的状态下**再导出报告**，每个场景一份。这些报告就是 fixtures，CI 会回放它们。

### 第 5 步：提 PR

放进：

- `extension/profiles/<id>.json`
- `extension/fixtures/<id>/<场景>.json`：第 4 步导出的报告，每个场景一份（`camera.report.json`、`restricted.report.json` ……）。文件名写场景，不写学校。

用对策文件的 PR 模板提交：在“Open a pull request”的地址后面加 `?template=profile.md`（地址里已有 `?` 时加 `&template=profile.md`），然后逐项勾选清单。写明平台名称、你测试的区域域名（不写学校）、每份报告对应的场景、是否需要 Cookie，以及受限标记是怎么确认的。

### CI 检查什么

每个 PR 都会跑 **Extension** 工作流（[`.github/workflows/extension.yml`](.github/workflows/extension.yml)）：

1. **对策文件与 fixtures**（[`scripts/profilecheck/check.mjs`](scripts/profilecheck/check.mjs)），每个问题一行可读的说明，运行页面上另有汇总：
   - JSON Schema，以及 schema 表达不了的规则（每个正则都能编译，而且不会回溯失控：不用反向引用和修饰符组，被重复的分组必须定宽，`*` / `+` / `{n,}` 最多两个且不能相邻，`?`、`{n,m}`、`|` 的选择连同它们作用对象及其后必需内容的宽度不能太多，确切规则见 [docs/extension/adding-a-platform.md](docs/extension/adding-a-platform.md)，那里也说明了使用中正则太慢时停用对策文件的运行时兜底；`session.idFrom.pathPattern` 恰好一个捕获组、role 不重复），用的是扩展导入对策文件时的同一段代码；
   - 文件名与 `id` 一致；不是草稿的对策文件必须有 fixtures；域名是平台自己的；
   - 不碰不处理名单（[`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js)：YouTube、MOOC 与在线 / 付费课程平台）：hosts 与 mediaHosts 不能是这些网站；只屏蔽部分路径的网站（`linkedin.com/learning`、`bilibili.com/cheese`）上，`match.pathPattern` 必须以 `^/` 加一个固定的、不是被屏蔽那一段的第一段路径开头，保证永远匹配不到那些页面；扩展的“导入本地对策文件”同样拒绝；
   - `extension/fixtures/<id>/` 里只有用这个对策文件导出的报告；
   - 对每个 fixture 做**脱敏扫描**（独立于扩展自己的脱敏）：Cookie 或授权请求头、会话 Cookie、`@`、令牌（JWT、Bearer、签名）、不是占位符的查询参数值、真实的 UUID、十六进制 ID、地址里的长数字和长令牌、像在指人的字段名。只报告位置和类别，从不显示值。
2. **fixtures 回放**（`npm test`）：用每份报告记录的输入重跑通用探测，必须找到同样的轨道；再套用你的对策文件，命名和推荐必须和报告一致。
3. Chrome 与 Firefox 构建，以及在无头 Chromium 里的端到端测试。

推送之前可以在本机跑同样的检查（Node.js 24）：

    node scripts/profilecheck/check.mjs
    cd extension
    npm ci
    npm test

### 脱敏要求（硬性）

- **任何包含 Cookie、个人姓名、学号的 fixture 一律不合并。**
- 同样不接受：邮箱、校内账号、讲师姓名、课程名或课程代码、签名或令牌、截图、录屏、HAR 文件。
- 只交扩展导出、未经手改的报告。宁可交一份不带 fixtures 的草稿对策文件，也不要手改 fixture。
- 维护者合并前会再查一遍；发现问题会请你重新导出，必要时从 PR 历史里删除。

### 许可

对策文件（`extension/profiles/*.json`）与 fixtures（`extension/fixtures/`）以 **CC0 1.0 通用**贡献到公有领域（[`extension/profiles/LICENSE`](extension/profiles/LICENSE)）。提交包含它们的 PR，即表示你同意以 CC0 发布它们，并确认其中没有他人受保护的内容。

**只**增加、修改或删除这类文件的 PR 不需要签署贡献者许可协议：CLA 检查会识别并直接通过。同时改动了其他文件（扩展代码、文档、`extension/profiles/LICENSE` 等）的 PR 照常需要签署 [CLA](CLA.md)，见 [CONTRIBUTING.md](CONTRIBUTING.md)。扩展代码本身沿用项目的 AGPL-3.0-or-later。

### 不接受的内容

- 脚本、`eval`、远程地址或任何“动态规则”；
- 读取课程、文件夹、讲师或学生信息的选择器或观察值；
- 绕过 DRM、登录或付费墙的任何做法；
- 要求扩展去请求页面没有请求过的地址；
- 扩展不处理的网站——YouTube，以及 MOOC 与在线 / 付费课程平台（[`extension/src/lib/blocklist.js`](extension/src/lib/blocklist.js)；CI 会拒绝）——或整个公共 CDN 的对策文件。

### 合并之后

合并的对策文件随扩展的下一个版本发布。每份对策文件有自己的版本号，改坏了可以单独回退。根据公开资料写成、等待有账号的人验证的平台草稿在 [`docs/extension/drafts/`](docs/extension/drafts/)：在那些平台上导出报告（第 1、4 步）并修正草稿，是最有用的贡献之一。
