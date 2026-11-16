# LectureFlow 桌面外壳（Electron）

`desktop/` 是 LectureFlow 的 Electron 外壳（SPEC-v3 V1、V2）：拉起并守护 Go 服务，把窗口指向 `http://127.0.0.1:{随机端口}/`，提供文件选择、通知、阻止休眠等原生能力。业务逻辑、组件管理器都在 Go 服务里，这里只有外壳。

## 准备

只用项目内的便携 Node（`tools/node/`，不在 PATH 里），不全局安装任何东西。

```bash
# Git Bash，在仓库根目录
export PATH="$PWD/tools/node:$PATH"
cd desktop
npm ci                                   # 依赖按 package-lock.json 安装
node node_modules/electron/install.js    # Electron 44 首次使用前下载自身的二进制（也会在第一次 require 时自动下载）
```

PowerShell：`$env:Path = "$PWD\tools\node;$env:Path"`，其余相同。

## 开发启动

先构建 Go 服务（在仓库根目录）：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File make.ps1 build   # 生成 lectureflow.exe
```

然后在 `desktop/` 里：

```bash
# 建议给开发用一个单独的数据目录，免得写进 %APPDATA%\LectureFlow
export LECTUREFLOW_DATA_DIR="$PWD/../data-desktop"
npm start
```

| 环境变量 | 作用 | 缺省 |
|---|---|---|
| `LECTUREFLOW_DATA_DIR` | 数据目录（优先于 `location.json` 与默认位置；打包后也生效，供验收用） | `%APPDATA%\LectureFlow\`（macOS：`~/Library/Application Support/LectureFlow/`），或其中 `location.json` 指向的目录 |
| `LECTUREFLOW_SERVER` | 服务程序路径；以 `.js` 结尾时用 Electron 自带的 Node 运行（测试用的假服务） | 仓库根目录的 `lectureflow(.exe)` |
| `LECTUREFLOW_RESOURCES` | 资源目录，传给服务的 `-resources`（`worker/`、`bin/ffmpeg` 等） | 仓库根目录 |

`LECTUREFLOW_SERVER`、`LECTUREFLOW_RESOURCES` 只在未打包时有效；打包后固定用 `resources/bin/lectureflow(.exe)` 与 `resources/`。另有两个只给测试用、同样只在未打包时有效的变量：`LECTUREFLOW_TEST_APPDATA`（替换系统的 appData，用来测试 `location.json`）、`LECTUREFLOW_FAKE_UPDATER`（用一个模块代替 electron-updater）。

服务以桌面模式启动：`-desktop -listen 127.0.0.1:PORT -data-dir … -resources … -app-version … -config {数据目录}/config.toml`，数据目录不是来自 `LECTUREFLOW_DATA_DIR` 时另加 `-location-file`。访问令牌写在服务的标准输入第一行，不出现在命令行、日志或磁盘上。高级用户可以把 `config.toml` 放进数据目录；不存在时用默认值。

开发模式下有隐藏菜单里的快捷键：Ctrl+R 重新加载、Ctrl+Shift+I 开发者工具。打包后没有开发者工具。

窗口的会话是持久化的（`persist:lectureflow`，在 userData 即 `{默认数据根目录}/electron/` 下，HTTP 缓存关闭），上次成功使用的端口记在 userData 的 `port.json`，下次优先使用，所以页面的 localStorage 偏好会保留。令牌只在内存里，只经请求头注入，端到端测试会扫描数据目录下的全部文件确认没有令牌。

日志：外壳写 `{数据目录}/logs/desktop.log`，服务写同目录的 `lectureflow.log`；错误页的“查看日志”打开这个目录。

## 测试

```bash
npm test            # 单元测试（node:test）：判定函数、参数校验、数据目录、服务进程管理（用假服务）
npm run test:e2e    # Playwright 端到端（_electron）
```

端到端测试有三个 project：

- `fake`：`test/fixtures/fake-server.js`，按 v3-brief §1–§2 的契约模拟服务，可以让它启动失败、端口被占、以退出码 3 请求重启、崩溃、报告有任务在运行。全部用例都在这里跑。
- `go-build`：`go build -o desktop/.e2e-tmp/bin/lectureflow.exe ./cmd/lectureflow`（需要 PATH 里有 go）。
- `go`：用真实服务跑 `shell.spec.js`（窗口加载、白名单、导航拦截、CSP、无令牌 401、Host 不符 403）。

只跑一部分：`npx playwright test --project=fake`。每个用例使用自己的临时数据目录（`LECTUREFLOW_DATA_DIR`），所以单实例锁互不影响，也不碰真实数据。

## 目录

```
src/main/main.js         入口：单实例、userData、菜单、退出流程
src/main/shell.js        控制器：窗口、服务生命周期、关窗、托盘、阻止休眠
src/main/server.js       服务进程：端口、令牌、就绪检测、端口重试、优雅停止
src/main/websecurity.js  会话与 webContents 的安全设置（令牌注入、CSP、导航拦截、权限）
src/main/ipc.js          preload 白名单背后的 IPC 处理（逐项校验参数与发送方）
src/main/updates.js      应用更新（electron-updater ↔ Go 的 /api/app/update 中转）
src/main/lib/            纯函数（可单测）：security、validate、datadir、policy、serverutil、i18n、menu、pngicon、portmemo、location
scripts/                 构建：stage、dist（`--out DIR` 输出到别的目录）、smoke（`--dist DIR`）、sources（Release 的对应源码附件）、build-ffmpeg-windows.sh、build-ffmpeg-macos.sh，lib/（固定版本下载、签名、大小报告）
packaging/               third-party.json（固定版本的第三方程序）、entitlements.mac.plist
src/preload.js           contextBridge：window.lectureflow（应用页）、window.lectureflowShell（启动/错误页）
src/splash.html, .js     启动画面与错误页（本地文件）
locales/                 外壳自己的文案（zh-CN、en）
test/unit, test/e2e      测试
```

## 构建安装包

```bash
npm run dist        # 在 Windows 上：NSIS 安装包 + 便携版；在 macOS 上：arm64 dmg（+ 更新用的 zip）
node scripts/smoke.js   # 启动打包后的程序，确认服务起来、无令牌请求被拒（401）、更新机制与构建一致，关窗后正常退出
```

需要 PATH 里有 go，第一次构建需要联网（下载 Electron 打包工具，以及下面的固定版本第三方程序）。`scripts/dist.js` 依次：

1. 生成占位图标 `build/icon.png`；
2. `scripts/stage.js` 把随程序分发的文件放进 `build/resources/`（electron-builder 的 extraResources）：
   - `bin/lectureflow(.exe)`（`go build -trimpath -ldflags "-s -w -X main.version=<版本>"`，`CGO_ENABLED=0`）与 `bin/lectureflow-nmhost(.exe)`（浏览器扩展的 Native Messaging 宿主，`./cmd/lectureflow-nmhost`，同样的构建参数）；
   - `bin/ffmpeg`、`bin/ffprobe`（只含 LGPL 部分的精简构建，两个平台都从固定的 ffmpeg-9.0.2.tar.xz 构建：Windows 由 `scripts/build-ffmpeg-windows.sh` 在 Linux 上用 mingw-w64 交叉编译，外部库只有 OpenH264、libvpl（Quick Sync）、NVENC 头文件、zlib 与 dav1d，本机没有现成产物时 stage 经 Docker 自动构建，产物在 `.cache/ffmpeg-windows-<版本>/`；macOS 由 `scripts/build-ffmpeg-macos.sh` 构建，产物在 `.cache/ffmpeg-macos-<版本>/`）；
   - `bin/aria2c(.exe)`（aria2 1.37.0，浏览器扩展的下载队列用：Windows 是上游官方构建，zip 与 aria2c.exe 的 SHA-256 都核对，并与 `packaging/components/components.json` 的 core 组件记录一致；macOS 由 `../packaging/aria2c/build-aria2c-macos.sh` 从固定的源码包构建，产物在 `.cache/aria2c-macos-<版本>/`，见 `packaging/aria2c/README.md`）；
   - `bin/uv`、`worker/` 的运行脚本（不含测试）、`components.json` 与 `locks/`、`sample/`、`LICENSE`、`LICENSE-EXCEPTION.md`、`THIRD_PARTY_NOTICES.md`；
   - `agent/<平台>/lf-agent` 与 `agent/kit.json`（远程服务器（SSH）转写要上传到服务器上的代理程序）**默认不打包**：这个功能还是实验功能（`LECTUREFLOW_EXPERIMENTAL_SSH=1` 才开启），安装包不带它；开关打开而安装包里没有它时，服务器卡片显示 `remote.agent_missing`。构建时设 `LECTUREFLOW_BUNDLE_LF_AGENT=1` 可以把它打进去（`0` 或不设：不打包，其他值构建失败）。功能公开时把 `scripts/stage.js` 的 `BUNDLE_AGENT_DEFAULT` 改成 `true` 即可（`build.test.js` 里断言默认值的那一行同时改）。开发时用仓库根目录的 `make.ps1 agent` 生成 `tools/lf-agent/`，不受影响；
   - `licenses/`：`ffmpeg/`（许可证原文、configure 参数 CONFIGURE.txt、BUILDINFO.txt、SOURCE.txt）、`uv/`、`aria2/`（COPYING、LICENSE.OpenSSL、AUTHORS，Windows 另有 README.mingw 与各静态链入库的许可证 `thirdparty/`，macOS 另有 BUILDINFO.txt 与 wslay 的许可证，以及 SOURCE.txt）。SOURCE.txt 写明对应源码在该版本 Release 的附件里（`LectureFlow-<版本>-source-<程序>-<平台>.tar`，见下文“发布”）。

   第三方程序的地址与 SHA-256 固定在 `packaging/third-party.json`，下载后校验，缓存在 `.cache/`。以下情况构建失败：ffmpeg 的 configure 参数含 `--enable-gpl`、`--enable-nonfree` 或 `--enable-version3`，`ffmpeg -L` 不是 LGPL，`packaging/licenses/binaries.json` 里 ffmpeg 的 bundled 有 GPL 许可的库；aria2c 与固定的 SHA-256 不符，或 `aria2c --version` 不对；`build/resources/` 里任何 Mach-O 文件要求的最低 macOS（`LC_BUILD_VERSION` 的 minos）高于应用声明的最低版本（`third-party.json` 的 `macos.minimum_system_version`，electron-builder 的 `mac.minimumSystemVersion` 读同一个值，目前 13.0：Electron 44 与 Go 1.27 都要求 macOS 13），或者（macOS）没有 arm64 代码。两个 macOS 构建脚本都以这个值为 `MACOSX_DEPLOYMENT_TARGET`；
3. `go run ./scripts/licaudit -check`，有违规时构建失败；
4. electron-builder（配置在 `electron-builder.config.js`，Electron fuses 在打包时写入）；
5. 更新配置检查：没有 `LECTUREFLOW_UPDATE_REPO` 时不得有 `app-update.yml` / `latest*.yml`，有时必须指向该仓库，不一致即失败；
6. 大小报告 `dist/size-report.md` / `.json`：安装包大小（预算 250 MB，超出只报告不失败）、各项资源、app.asar、解包后最大的十个文件。

签名只从环境变量读取，没有就构建未签名版本（见 `electron-builder.config.js` 开头与 `docs/install.md`）：Windows `CSC_LINK`/`CSC_KEY_PASSWORD`（或 `WIN_CSC_LINK`）；macOS `CSC_LINK`/`CSC_KEY_PASSWORD` 或 `CSC_NAME`，公证另需 `APPLE_API_KEY`+`APPLE_API_KEY_ID`+`APPLE_API_ISSUER`（或 `APPLE_ID`+`APPLE_APP_SPECIFIC_PASSWORD`+`APPLE_TEAM_ID`）。macOS 未签名时用临时签名（ad-hoc），Apple 芯片只运行签过名的代码。Windows 签名时 `resources/bin/` 里的 `.exe` 也会被签，唯独 `aria2c.exe` 例外（`win.signExts: ['!aria2c.exe']`，保持与上游官方构建、固定的 SHA-256 逐字节一致；不要再加 `.exe` 之类的正向条目，正向条目优先，会让排除失效）。

更新：electron-updater，GitHub Releases（SPEC-v3 V8 的旧路径）。electron-updater 已不再是 `desktop/` 的依赖，按现在的锁文件构建的安装包里没有它（3.2.0 的锁文件还带着它，3.2.0 安装包里有，只是不加载）：打包后的主进程本来就从不加载它（安全审计 FB1 L-8，`src/main/main.js` 的 `loadUpdater`），这条路径只在开发与端到端测试里由 `LECTUREFLOW_FAKE_UPDATER` 指定的替身运行；下面的构建配置（`app-update.yml`、`latest*.yml`）由 electron-builder 生成，与这个依赖无关，照旧。**更新仓库只从构建环境变量 `LECTUREFLOW_UPDATE_REPO`（`owner/name`，严格校验，格式不对构建失败）读取**；未设置时 publish 为 null，不生成 `app-update.yml` 与 `latest*.yml`，打包后的主进程不加载更新器（`/api/app/update` 保持 none，界面不显示更新）。构建日志与大小报告（随之进入发布说明）的一行同时写明 electron-updater 的更新源与应用内更新：`electron-updater feed: off (…); in-app updates: on (minisign key <key id> built in; …)`，公钥按 `internal/selfupdate/keys.go` 读取（`scripts/lib/updaterepo.js` 的 `builtinUpdateKey`，读不出或不是 minisign 公钥时构建失败）。有 Windows 签名证书时，`LECTUREFLOW_PUBLISHER_NAME` 可指定更新包必须具有的发布者名称（缺省取证书的通用名称），构建后检查 `app-update.yml` 带有 publisherName。设置了仓库时主进程启动时和每 6 小时检查，渠道取 `/api/app/settings` 的 update_channel；不自动下载，经 Go 的 `/api/app/update*` 中转给网页；只在退出流程里、服务停止之后、且用户选了“退出时安装”时安装。以上是服务里没有内置更新公钥时的旧路径；内置了公钥时改走应用内更新（下一段），electron-updater 不加载（`src/main/updatepath.js`），desktop.log 写 `updates: in-app updates (signed update.json) are configured; electron-updater is not loaded`。冒烟测试按同一个 keys.go 判断应当是哪种机制（有公钥：必须是这一行；没有：必须是与 `LECTUREFLOW_UPDATE_REPO` 一致的 electron-updater 那一行），`--expect-updates selfupdate|electron-updater` 可指定；它还检查 `app-update.yml` 的有无与 `LECTUREFLOW_UPDATE_REPO` 一致，并以 `LECTUREFLOW_NM_ENDPOINT=off` 启动（与端到端测试相同），不替这个临时构建登记浏览器扩展的宿主。

应用内更新（`src/main/selfupdate.js`，与 Go 的约定见 `internal/selfupdate/README.md`“The updates directory”）：Go 核对签名与安装包后在 `{数据目录}/updates/` 写 `pending.json`，外壳在下次启动、服务启动之前安装。安装前两道检查：外壳自己核对 `pending.json`（schema、版本比当前新、平台与安装方式、安装包的大小与 SHA-256）；然后用**已安装的旧版服务程序**（`resources/bin/lectureflow(.exe)`）以内置公钥再核对一遍，`lectureflow -selfupdate-verify <updates 目录> -app-version <当前版本>`（`src/main/lib/verifyupdate.js`，30 秒超时）。只有退出码 0、且输出行 `ok version=… kind=… file=… sha256=…` 与 `pending.json` 的版本、类型、文件、SHA-256 完全一致时才备份、改名为 `applying.json` 并安装。其余情况都不安装：

| 结果 | 处理 |
|---|---|
| 退出码 1（`invalid <code>: <原因>`），或退出码 0 但说的是另一个安装包 | 删除 `pending.json`，`result.json` 记 `failed` 与原因（设置页“查看详情”可见）；安装包目录留给服务在启动时清理 |
| 退出码 2（`not_configured`：没有内置公钥的构建）、超时、程序无法启动、输出不合约定 | 保留 `pending.json`（这种构建自己不会写出它，留给能验证的构建或下次启动），`result.json` 记 `failed` 与原因 |

日志与 `result.json` 里的原因去掉了 URL 的查询串。开发模式（未打包）不处理 `pending.json`；端到端测试经 `LECTUREFLOW_TEST_SELFUPDATE` 用假服务（`test/fixtures/fake-server.js` 的 `-selfupdate-verify`，`FAKE_SELFUPDATE_VERIFY` 控制答案）。

持续集成：`.github/workflows/ci.yml`（Windows 与 macOS：go vet/test、pytest、desktop 单元测试、许可证审计；macOS 的 ffmpeg 与 aria2c 构建按脚本与 `third-party.json` 的哈希缓存）与 `release.yml`（打 tag 时两个平台构建、冒烟测试、创建草稿 Release）。

发布：草稿 Release 除了安装包，还附带随包 GPL / LGPL 程序的对应源码（`node scripts/sources.js --out DIR [--version V] [--notes FILE]`，由 release.yml 的 sources 作业运行）：ffmpeg 与 aria2c 每个平台一个 tar，`LectureFlow-<版本>-source-{ffmpeg,aria2c}-{windows-x64,darwin-arm64}.tar`，内含该构建实际用到的上游源码包（按 `third-party.json` 的 SHA-256 校验、原样保存）、我们的构建脚本与 `third-party.json`、README.txt 与 SHA256SUMS；同样的输入得到同样的字节（条目时间取 `SOURCE_DATE_EPOCH`）。四个附件合计约 109 MB（3.1.0 实测 109.1 MB，其中 OpenH264 的源码包占 58 MB）。GCC 与 MinGW-w64 运行库的源码不附带（GCC 运行库例外与 ZPL-2.1 都不要求），SOURCE.txt 与 README.txt 写明原因和来源。
