# 安装 LectureFlow

[简体中文](#简体中文) · [English](#english)

## 简体中文

### 下载哪个文件

在项目的 Releases 页面下载：

| 系统 | 文件 | 说明 |
|---|---|---|
| Windows 10 / 11（64 位） | `LectureFlow-Setup-<版本>-x64.exe` | 安装版，推荐。安装到当前用户，不需要管理员权限，可以选择安装位置 |
| Windows 10 / 11（64 位） | `LectureFlow-<版本>-x64-portable.exe` | 便携版，不安装，双击即可运行（每次启动要先解压，稍慢） |
| macOS 13 及以上（Apple 芯片：M1 及更新） | `LectureFlow-<版本>-arm64.dmg` | 打开后把 LectureFlow 拖进“应用程序” |

安装包只包含程序本身。转写所需的运行环境和模型，在第一次启动时按你的电脑配置下载（引导里会说明大小）。

Releases 页面上的其他文件一般不用下载：`update.json`、`update.json.minisig` 和 `LectureFlow-<版本>-arm64-mac.zip` 供应用内更新使用；`LectureFlow-<版本>-source-….tar` 是安装包里 ffmpeg 与 aria2c 的对应源码。

无论安装版还是便携版，数据都保存在：Windows `%APPDATA%\LectureFlow\`，macOS `~/Library/Application Support/LectureFlow/`（可以在设置里改到别的磁盘）。卸载程序不会删除这些数据。

保存的 API key 和 Colab 授权用系统账户的凭据保护加密（Windows 的 DPAPI、macOS 的钥匙串）。把数据目录复制到另一台电脑或另一个系统账户后，需要重新输入 key、重新授权 Colab；设置页会标出需要重新输入的服务商。数据目录最好不要放在 OneDrive、iCloud 云盘等同步文件夹里，也不要放在 FAT / exFAT 格式的磁盘上，诊断页的“数据目录位置”一行会提醒。

### 未签名版本的提示

LectureFlow 的安装包可能没有代码签名证书（是否购买证书由维护者决定）。这种情况下，系统第一次打开时会提示“无法识别的应用”。这是对所有未签名程序的通用提示，不代表发现了问题。如果你是从项目的 Releases 页面下载的，可以按下面的步骤继续。

#### Windows：SmartScreen

1. 双击安装包后，如果出现蓝色窗口“Windows 已保护你的电脑”，点击文字链接 **更多信息**。
2. 窗口里会出现“应用：LectureFlow-Setup-…”和“发布者：未知发布者”，点击 **仍要运行**。
3. 之后按安装向导操作即可。以后启动 LectureFlow 不会再出现这个提示。

![示意图：SmartScreen 提示中先点“更多信息”，再点“仍要运行”](images/install-windows-smartscreen.svg)

*示意图，不是截图；文字以系统实际显示为准。真机截图待补。*

便携版第一次运行时也可能出现同样的提示，处理方法相同。

#### macOS：隐私与安全性

1. 打开 `.dmg`，把 LectureFlow 拖进“应用程序”文件夹（也可以放在外置盘的任意文件夹里）。
2. 双击 LectureFlow。系统提示“未打开 LectureFlow”，点击 **完成**，不要点“移到废纸篓”。

   ![系统提示：未打开“LectureFlow”，按钮“移到废纸篓”与“完成”](images/install-macos-1-not-opened.png)

3. 打开 **系统设置 → 隐私与安全性**，向下滚动到“安全性”一节，可以看到“已阻止 LectureFlow 以保护你的 Mac”，点击旁边的 **仍要打开**。

   ![系统设置 → 隐私与安全性 → 安全性：“仍要打开”](images/install-macos-2-open-anyway.png)

4. 在弹出的对话框里再点击一次 **仍要打开**。

   ![对话框：打开“LectureFlow”？按钮“移到废纸篓”“仍要打开”“完成”](images/install-macos-3-confirm.png)

5. 输入登录密码（或使用触控 ID）确认，LectureFlow 随即打开。

   以后每装一个新版本（包括应用内更新），第一次打开时 macOS 会问 LectureFlow 能否使用钥匙串里的“LectureFlow Safe Storage”：输入登录密码，选 **始终允许**。它用来加密你保存的 API key；回答之前 LectureFlow 停在启动画面。

*截图来自 macOS 27（英文界面），其他版本与语言的文字可能略有不同。*

只需要做一次。macOS 15 起，右键点按“打开”已不能跳过这个提示，请使用“系统设置”。

### 更新

正式发布版每天最多自动检查一次新版本（设置 → 应用 →“自动检查更新”可以关闭，“立即检查”随时可用）。检查时只请求公开的发布地址（GitHub 和你添加的镜像源），不附带任何用户信息。发现新版本时，在设置页与工作台提示，**由你决定**是否下载。

- 下载的安装包要通过签名与 SHA-256 校验，在**下一次启动 LectureFlow 时**安装。上一个版本会保留：新版本启动失败时，自动回到上一个版本，设置页会说明。
- **从文件更新**：没有网络或下载太慢时，从 Releases 页面（或别人拷给你的）取得 `update.json`、`update.json.minisig` 和你的安装包，在设置页“从文件更新”里一起选中（也可以是包含它们的一个 zip）。同样要通过签名校验才会安装。
- **镜像源**：可以在设置页添加镜像地址。镜像上的文件被改动了任何一个字节，都会被拒绝。
- 设置里可以选择稳定版或测试版。
- 便携版更新时直接替换原来的 exe：快捷方式照常可用，但文件名里的版本号不再准确。
- 设置页说明这个版本不在应用内更新时（例如从源代码运行的开发版本），请到 Releases 页面下载新版本覆盖安装，数据不受影响。
- **从 3.1.0 升级到 3.2.0** 请手动下载安装包覆盖安装；之后的版本可以在应用内更新。
- **安装位置**：建议把 LectureFlow 安装在默认位置（`%LOCALAPPDATA%\Programs\LectureFlow`），尤其是这台电脑还有其他账户时。装在别处（例如另一个盘）时，如果安装文件夹或它上面的某一层文件夹其他账户也能修改、删除或改名（在另一个盘上自己建的文件夹通常如此），诊断页从一开始就会给出警告，应用内更新也会停用；即使没有这种情况，安装程序（包括每次应用内更新）也要到解压完才把文件夹收紧为只有你能修改，而应用内更新失败、回退到上一个版本之后，这个权限不会自动恢复，同样会出现警告并停用应用内更新。出现警告时，按诊断页“安装目录”一行的提示处理（例如重新安装到默认位置），处理好之后应用内更新自动恢复。

### 网络代理

LectureFlow 调用模型、检查和下载更新、下载组件、使用 Colab、刮削录播时，只按环境变量 `HTTP_PROXY`、`HTTPS_PROXY` 和 `NO_PROXY`（大小写均可）使用代理：访问 `https://` 地址用 `HTTPS_PROXY`，访问 `http://` 地址用 `HTTP_PROXY`，不会互相代替。所以只设 `HTTP_PROXY` 时，模型服务、更新和绝大多数录播仍然直连。这些连接不使用 `ALL_PROXY`，也不读取系统的代理设置（Windows 与 macOS 都是）。本机地址（`localhost`、`127.0.0.1`）总是直连；局域网里的地址（例如另一台电脑上的模型服务）不想经过代理时，把它写进 `NO_PROXY`。安装组件时，Python 包由 uv 下载，uv 按它自己的规则使用代理，可能与上面不同（例如它的文档说明也会读取 `ALL_PROXY`）。

刮削录播只能使用 `http://` 写法的代理（不写协议的地址，如 `127.0.0.1:7890`，按 `http://` 处理），不支持 SOCKS 代理和 `https://` 写法的代理；代理要认证时，只支持写在地址里的 Basic 认证（`http://用户名:密码@主机:端口`，特殊字符要百分号编码）。某个录播地址要经过这类代理时（包括对应的 `HTTP_PROXY` / `HTTPS_PROXY` 没设、而 `ALL_PROXY` 是 SOCKS 或 `https://` 的情形），刮削会被拒绝并说明原因，不会悄悄直连；`NO_PROXY` 列出的地址照常直连。调用模型、检查和下载更新不受这条限制，可以通过 `HTTP_PROXY` / `HTTPS_PROXY` 使用 SOCKS5（`socks5://`、`socks5h://`）或 `https://` 写法的代理。

改了这些环境变量之后，请完全退出 LectureFlow（包括在后台运行的），再重新打开。

### 浏览器扩展

LectureFlow 浏览器扩展“发送至 LectureFlow”（Chrome、Firefox；Microsoft Edge 从 Chrome 网上应用店安装）把你正在看的录播送进 LectureFlow：在录播页上点一下扩展图标，选择要发送的内容（主要音视频、双机位或仅音频）和课程，刮削就在 LectureFlow 里开始。在 Panopto 上，允许扩展访问该网站后，还可以直接用页面上的“发送至 LectureFlow”按钮。录播嵌在 Canvas 等其他网站的页面里时，点页内按钮后在扩展自己的小窗口里确认发送；把嵌入的网站加入“信任的嵌入网站”后，可以直接在页面上确认（嵌入的播放器太小、放不下确认卡片时仍用小窗口）。扩展只读媒体轨道，不读课程信息；隐私声明见 [extension/privacy.md](extension/privacy.md)。

扩展只用于学校给你的课堂录播：它不处理 YouTube，也不处理 MOOC 与在线 / 付费课程平台（Coursera、edX、Udemy、LinkedIn Learning、中国大学 MOOC 等），在这些网站上弹窗只说明“这个网站不在 LectureFlow 的支持范围内”。

扩展需要同一台电脑上的 LectureFlow 3.2.0 或更新版本：LectureFlow 每次启动时，都会向 Chrome、Edge、Firefox 登记扩展用来和它通信的本机程序（Native Messaging）。所以请**先安装并打开一次 LectureFlow**，再安装扩展。macOS 上只能向已经运行过的浏览器登记：浏览器第一次运行之后，开着的 LectureFlow 会自动补上登记；没有补上时，在诊断页“浏览器扩展连接”一行点“重新登记”。诊断页显示“被 macOS 阻止”时，按提示在“系统设置 › 隐私与安全性 › 完全磁盘访问权限”里允许 LectureFlow。

#### 从扩展商店安装

商店版上架后会在这里给出链接。安装的 LectureFlow（正式版）只允许商店版扩展连接；要用开发版扩展，见下一节。

- Chrome 网上应用店：（待上架）
- Microsoft Edge：从 Chrome 网上应用店安装（Edge 可以安装来自 Chrome 网上应用店的扩展）
- Firefox 附加组件：（待上架）

#### 开发版：从源代码加载

给开发者和[对策文件贡献者](../CONTRIBUTING-profiles.md)用。需要本仓库的源代码和 Node.js 24。

1. 构建（只用 Node 自带的模块，不需要 `npm install`）：

       cd extension
       node scripts/build.mjs

   得到 `extension/dist/chrome`（Chrome 与 Edge 共用）和 `extension/dist/firefox`。
2. **Chrome**：打开 `chrome://extensions`，打开右上角的“开发者模式”，点“加载已解压的扩展程序”，选择 `extension/dist/chrome` 文件夹。
   **Edge**：打开 `edge://extensions`，打开“开发人员模式”，点“加载解压缩的扩展”，选择同一个文件夹。
   开发版的扩展 ID 固定为 `lbfjnfhpnaafingpjkoefobbjnmheehl`（由清单里的公钥决定）。这个公钥是公开的，任何人都能做出同一个 ID 的扩展，所以**安装的 LectureFlow 默认只允许商店版扩展连接**；从源代码运行的开发构建（`make.ps1 run`）默认也允许开发版。要让安装的 LectureFlow 接受开发版扩展，先设置用户环境变量 `LECTUREFLOW_DEV_EXTENSION=1`，再重新启动 LectureFlow 和浏览器：
   - Windows（PowerShell）：`[Environment]::SetEnvironmentVariable('LECTUREFLOW_DEV_EXTENSION', '1', 'User')`
   - macOS（终端）：`launchctl setenv LECTUREFLOW_DEV_EXTENSION 1`（注销或重启后失效，需要再设置一次）

   设置生效后，诊断页“浏览器扩展连接”一行下会写“允许开发版扩展连接”。不再需要时删除这个变量（Windows 把上面命令里的 `'1'` 换成 `$null`；macOS 用 `launchctl unsetenv LECTUREFLOW_DEV_EXTENSION`），再重新启动 LectureFlow。Firefox 的开发版与商店版是同一个附加组件 ID，不需要这一步。
3. **Firefox**（140 或更新）：打开 `about:debugging#/runtime/this-firefox`，点“临时载入附加组件…”，选择 `extension/dist/firefox/manifest.json`。临时载入的扩展在 Firefox 退出后就会移除，下次需要重新载入。
4. 打开 LectureFlow，再打开一节录播并开始播放，点扩展图标。弹窗显示“已连接 LectureFlow”就可以用了；显示“未连接”时，确认 LectureFlow 正在运行，在它的诊断页查看浏览器扩展的登记状态，然后在弹窗里点“重新检测”。
5. 在扩展认识的平台（如 Panopto）上，点弹窗里的“在此网站自动探测”，并在浏览器的权限提示里同意：之后打开录播页时，扩展图标上会自动显示轨道数。

更新开发版：拉取新代码后重新运行 `node scripts/build.mjs`，再在扩展管理页点“重新加载”（Firefox 重新临时载入）。

---

## English

### Which file to download

From the project's Releases page:

| System | File | Notes |
|---|---|---|
| Windows 10 / 11 (64-bit) | `LectureFlow-Setup-<version>-x64.exe` | Installer, recommended. Installs for the current user, no administrator rights needed, installation folder can be changed |
| Windows 10 / 11 (64-bit) | `LectureFlow-<version>-x64-portable.exe` | Portable, nothing to install (unpacks itself on every start, so it starts a little slower) |
| macOS 13 or later (Apple silicon: M1 or newer) | `LectureFlow-<version>-arm64.dmg` | Open it and drag LectureFlow to Applications |

The installer contains the app only. The transcription runtime and models are downloaded on first start, chosen for your computer (the guide shows the sizes).

You don't need the other files on the Releases page: `update.json`, `update.json.minisig` and `LectureFlow-<version>-arm64-mac.zip` are for the in-app update; `LectureFlow-<version>-source-….tar` are the corresponding sources of the ffmpeg and aria2c in the installers.

Your data lives in `%APPDATA%\LectureFlow\` (Windows) or `~/Library/Application Support/LectureFlow/` (macOS); it can be moved to another disk in Settings. Uninstalling does not delete it.

Saved API keys and the Colab authorization are encrypted, protected by your system account (DPAPI on Windows, the Keychain on macOS). After copying the data folder to another computer or another user account, enter the keys and authorize Colab again; Settings marks the providers that need a new key. Better not keep the data folder in a sync folder such as OneDrive or iCloud Drive, or on a FAT / exFAT disk: the "Data folder location" row on the diagnostics page warns about it.

### Unsigned builds

The installers may not be code-signed. Your system then warns that the app is unrecognised the first time you open it. This warning appears for every unsigned program; it does not mean anything was found. If you downloaded the file from the project's Releases page, continue as follows.

**Windows (SmartScreen):** on the blue "Windows protected your PC" window, click **More info**, then **Run anyway**. The warning does not come back afterwards. The portable version may show the same warning.

**macOS:** drag LectureFlow to Applications (or any folder, also on an external drive) and open it. When macOS says "LectureFlow" Not Opened, click **Done** (not "Move to Trash"). Open **System Settings → Privacy & Security**, scroll to "Security", click **Open Anyway** next to "LectureFlow was blocked to protect your Mac", click **Open Anyway** again in the dialog that follows, then confirm with your password or Touch ID; LectureFlow opens. This is needed once. Since macOS 15, Control-clicking "Open" no longer skips the warning. After each new version is installed (in-app updates too), the first start asks whether LectureFlow may use "LectureFlow Safe Storage" in your Keychain: enter your login password and choose **Always Allow**. It encrypts your saved API keys; LectureFlow waits on its start screen until you answer. The macOS screenshots above are from macOS 27; the Windows pictures are schematic.

### Updates

Release builds check for a new version at most once a day (Settings → App → "Check automatically" turns this off; "Check now" works at any time). A check only requests the public release addresses (GitHub and any mirrors you added) and carries no user information. A new version is announced on the Settings page and on the dashboard; **you decide** whether to download it.

- The downloaded package must pass its signature and SHA-256 checks, and is installed **the next time LectureFlow starts**. The previous version is kept: if the new one fails to start, LectureFlow goes back to the previous version and says so in Settings.
- **Update from file**: without a network, or when downloading is too slow, get `update.json`, `update.json.minisig` and your package from the Releases page (or from someone who copied them for you) and select them together under "Update from file" in Settings (a zip holding them works too). They must pass the same signature check.
- **Mirrors**: you can add mirror addresses in Settings. A file on a mirror that differs by a single byte is refused.
- Settings let you choose the stable or the beta channel.
- The portable build replaces its own exe when it updates: shortcuts keep working, but the version in the file name is no longer accurate.
- If Settings says that this version does not update in the app (for example a development build run from source), download the new version from the Releases page and install it over the old one; your data is not affected.
- **From 3.1.0 to 3.2.0**, download and install by hand; later versions can update in the app.
- **Installation folder**: we recommend installing LectureFlow in the default location (`%LOCALAPPDATA%\Programs\LectureFlow`), especially if this computer has other accounts. If you install it elsewhere (for example on another drive) and other accounts can change, delete or rename the installation folder or a folder above it (as they usually can with a folder you made yourself on another drive), the Diagnostics page shows a warning from the start and in-app updates are off. Even when that is not the case, the installer (including the one each in-app update runs) only makes the folder changeable by you alone after unpacking has finished, and after a failed in-app update has been rolled back those permissions are not restored automatically, with the same warning and in-app updates off. When you see the warning, follow the hint in the Installation folder row of the Diagnostics page (for example, install again into the default location); in-app updates resume once it is fixed.

### Network proxy

When LectureFlow calls a model, checks for or downloads updates, downloads components, uses Colab or fetches recordings, it takes its proxy only from the environment variables `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (upper or lower case): `HTTPS_PROXY` for `https://` addresses, `HTTP_PROXY` for `http://` ones, and neither stands in for the other. With only `HTTP_PROXY` set, model providers, updates and most recordings are still reached directly. These connections do not use `ALL_PROXY` and do not read the system's proxy settings (on Windows or macOS). Addresses on this computer (`localhost`, `127.0.0.1`) are always reached directly; to keep an address on your local network (a model server on another computer, for example) off the proxy, add it to `NO_PROXY`. When a component is installed, its Python packages are downloaded by uv, which follows its own proxy rules and may differ from the above (its documentation says it also reads `ALL_PROXY`, for example).

Fetching recordings works only with proxies written as `http://` (an address without a scheme, such as `127.0.0.1:7890`, is treated as `http://`), not with SOCKS proxies or proxies written with `https://`; if the proxy needs a login, only Basic authentication written in the address is supported (`http://user:password@host:port`, special characters percent-encoded). When a recording's address would go through such a proxy (this includes the case where the matching `HTTP_PROXY` / `HTTPS_PROXY` is not set and `ALL_PROXY` is a SOCKS or `https://` one), fetching is refused with an explanation instead of quietly connecting directly; addresses listed in `NO_PROXY` are still reached directly. Model calls and updates are not limited this way: they can use a SOCKS5 (`socks5://`, `socks5h://`) or `https://` proxy set in `HTTP_PROXY` / `HTTPS_PROXY`.

After changing these variables, quit LectureFlow completely (including any copy running in the background) and open it again.

### Browser extension

The LectureFlow browser extension, Send to LectureFlow (Chrome, Firefox; Microsoft Edge installs it from the Chrome Web Store), sends the recording you are watching to LectureFlow: click its icon on a recording page, choose what to send (Main audio & video, Two cameras or Audio only) and the course, and fetching starts in LectureFlow. On Panopto, once you allow the extension on the site, you can also use the "Send to LectureFlow" buttons on the page. When the recording is embedded in another website's page (Canvas, for example), an in-page button asks you to confirm in the extension's own small window; after you add that website to "Trusted embedding sites" you confirm on the page instead (unless the embedded player is too small to hold the confirmation card, which keeps the small window). It reads only the media tracks, never course information; see its [privacy policy](extension/privacy.md).

The extension is only for the lecture recordings your school gives you: it does not handle YouTube, nor MOOCs and online or paid course platforms (Coursera, edX, Udemy, LinkedIn Learning and the like); on those sites the popup only says "LectureFlow does not work on this site".

The extension needs LectureFlow 3.2.0 or later on the same computer: every time LectureFlow starts, it registers with Chrome, Edge and Firefox the local program the extension talks to (Native Messaging). So **install and open LectureFlow once first**, then install the extension. On macOS LectureFlow can register only with browsers that have already run: once a browser has run for the first time, a running LectureFlow adds the registration by itself; if it does not, click **Register again** on the **Browser extension connection** row of the diagnostics page. If that row says **Blocked by macOS**, allow LectureFlow under System Settings › Privacy & Security › Full Disk Access, as it explains.

#### From the extension stores

Links will be added here once the extension is in the stores. An installed (release) LectureFlow lets only the store extension connect; for a developer build of the extension, see the next section.

- Chrome Web Store: (coming)
- Microsoft Edge: install from the Chrome Web Store (Edge can install extensions from it)
- Firefox Add-ons: (coming)

#### Developer build, loaded from source

For developers and [profile contributors](../CONTRIBUTING-profiles.md). You need this repository's source and Node.js 24.

1. Build (Node's own modules only, no `npm install` needed):

       cd extension
       node scripts/build.mjs

   This writes `extension/dist/chrome` (for Chrome and Edge) and `extension/dist/firefox`.
2. **Chrome**: open `chrome://extensions`, turn on **Developer mode** (top right), click **Load unpacked** and choose the `extension/dist/chrome` folder.
   **Edge**: open `edge://extensions`, turn on **Developer mode**, click **Load unpacked** and choose the same folder.
   The developer build's extension ID is always `lbfjnfhpnaafingpjkoefobbjnmheehl` (set by the public key in its manifest). That key is public, so anyone can make an extension with the same ID: **an installed LectureFlow lets only the store extension connect by default**; a development build run from source (`make.ps1 run`) lets the developer build connect too. To let an installed LectureFlow accept the developer build, set the user environment variable `LECTUREFLOW_DEV_EXTENSION=1`, then restart LectureFlow and the browser:
   - Windows (PowerShell): `[Environment]::SetEnvironmentVariable('LECTUREFLOW_DEV_EXTENSION', '1', 'User')`
   - macOS (Terminal): `launchctl setenv LECTUREFLOW_DEV_EXTENSION 1` (lost when you log out or restart; set it again)

   Once it is in effect, the diagnostics page says "The developer build of the extension may connect too" under **Browser extension connection**. When you no longer need it, remove the variable (Windows: the same command with `$null` instead of `'1'`; macOS: `launchctl unsetenv LECTUREFLOW_DEV_EXTENSION`) and restart LectureFlow. Firefox's developer build has the same add-on ID as the store's and needs none of this.
3. **Firefox** (140 or newer): open `about:debugging#/runtime/this-firefox`, click **Load Temporary Add-on…** and choose `extension/dist/firefox/manifest.json`. A temporary add-on is removed when Firefox quits; load it again next time.
4. Open LectureFlow, then a recording, start playback and click the extension's icon. "LectureFlow connected" means you are ready. "Not connected": check that LectureFlow is running, look at the browser extension's registration on its diagnostics page, then click **Check again** in the popup.
5. On a platform the extension knows (such as Panopto), click **Detect automatically on this site** in the popup and allow it in the browser's permission prompt: from then on the icon shows the number of tracks as soon as a recording page opens.

To update a developer build, pull the new source, run `node scripts/build.mjs` again and click **Reload** on the extensions page (Firefox: load it again).
