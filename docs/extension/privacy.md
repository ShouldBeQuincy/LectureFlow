# LectureFlow 浏览器扩展隐私声明 / Privacy policy

[简体中文](#简体中文) · [English](#english)

<!-- Internal: this text must match the code (extension/, internal/fetch, internal/nmhost). The extension is listed only on the Chrome Web Store (Microsoft Edge users install it from there, so this policy covers them) and Firefox AMO. privacy-edge.md (Edge-only wording for a future Microsoft Edge Add-ons listing) is not in use but is kept in step: change both together. Store notes: docs/extension/store/ (maintainers' local notes, not published). -->

## 简体中文

最后更新：2026-11-11

本声明说明 LectureFlow 浏览器扩展“发送至 LectureFlow”（英文名 Send to LectureFlow，下称“扩展”）读取哪些数据、什么时候读、交给谁、保存什么。扩展是 LectureFlow 桌面应用（下称“LectureFlow”）的配套部分。两者都是开源软件，源代码在 <https://github.com/ShouldBeQuincy/LectureFlow>。

### 简要

- 扩展只和**同一台电脑上**的 LectureFlow 通信，不连接我们的任何服务器（我们没有服务器）。没有账号，没有遥测，没有统计，没有广告。
- 只有在你发送讲座时，扩展才读取 Cookie，而且只读你选中的轨道所在网站的 Cookie，只交给本机的 LectureFlow。对策文件为平台列出的网站，交出这些轨道地址的 Cookie；其他网站只交出两类：与你所在页面同属一个网站的 Cookie，以及页面里嵌入的播放器亲自请求过这些轨道时、那个播放器所在网站允许跨站发送的 Cookie（Firefox 版只交下面所说的分区 Cookie）。页面只是写出了别的网站的地址时，那个网站的 Cookie 一个也不交。对策文件写明不需要 Cookie 的平台（例如 Panopto）一个也不读。播放器嵌在别的网站页面里（例如 Canvas）时，浏览器会把它的 Cookie 按所在页面的网站分开存放（分区 Cookie）：交给 LectureFlow 的只有你发送时所在页面的那一份，这个网站嵌在其他网站页面里时存下的那份不会交给 LectureFlow。
- 扩展不保存 Cookie。LectureFlow 只把 Cookie 放在内存里，用于这一次刮削，不写入磁盘、数据库或日志。
- 扩展不读课程名、课程代码、文件夹或课件。
- 在你授权过、且有平台对策文件的录播平台上，扩展会在页面里加“发送至 LectureFlow”按钮，并从平台自己的请求结果里只取对策文件声明的几个字段；结果里的其余内容（包括你的姓名、邮箱）不会离开页面。
- 我们（开发者）收不到任何数据。

### 扩展读取什么、什么时候读

| 数据 | 什么时候读 | 用来做什么 |
|---|---|---|
| 页面上的媒体元素（`<video>`、`<audio>`、`<source>` 的地址，包括同源 iframe 里的）、页面已经加载的媒体文件和播放清单的地址、媒体是否受 DRM 保护 | 在你授权过的网站上，录播页打开时；在其他网站上，只在你点击扩展图标之后 | 数出这一页有几条可以刮削的音频、视频轨道 |
| 发往你授权过的网站的网络请求：地址、`Content-Type` 和大小（只读，不拦截、不修改；只留下媒体文件和播放清单，其余忽略） | 浏览器只报告发往你授权过的网站的请求；按标签页分开记在内存里，标签页跳转或关闭就丢弃 | 识别地址里没有扩展名的媒体文件和播放清单 |
| HLS / DASH 播放清单的内容，单个媒体文件的大小 | 探测到清单或媒体文件时 | 把清单拆成音频轨道和视频轨道，估算大小 |
| 讲座标题、录制日期、时长；平台的受限标志（讲师或大学是否限制了这条媒体流）；会话 ID | 当前网址命中某个平台对策文件时，按对策文件里写好的 CSS 选择器读取 | 给轨道和文件命名；媒体流可能受限时显示受限提示（见下文“受限的媒体流与 DRM”）；识别同一节课是否已经送过 |
| 页面自己的标题（浏览器标签页上显示的那一行） | 扩展 0.2.1 起：没有对策文件命中时（通用探测），扩展在这个页面上探测时读取（时机同第一行）；只读正在探测的那个页面或框架的标题，而且在“不处理的网站”检查（见下文）之后才读，名单上的网站和嵌在其中的框架一概不读 | 去掉末尾的站点名（例如“… : Internet Archive”）后作为讲座标题：在弹窗里显示，发送讲座时交给本机的 LectureFlow，扩展不把它发到别处（LectureFlow 之后怎样使用，见下文“LectureFlow 收到之后”）；只剩站点名或“Recording”“Video”之类的占位词时不用，由 LectureFlow 命名。不写进探测报告 |
| 当前页面的地址 | 扩展在这个页面上探测时（时机同第一行） | 匹配平台对策文件；刮削时作为 Referer（与浏览器默认的做法相同：所选轨道都和页面同源，即协议、域名和端口都相同时，是页面地址，不含 `#` 之后的部分；有轨道在其他来源时，只有页面的协议、域名和端口；HTTPS 页面上的普通 HTTP 轨道不带 Referer）；没有对策文件时，用规范化地址的哈希值作为会话 ID |
| 录播页面嵌在哪个网站的页面里（只有协议、域名和端口，浏览器告诉页面里的扩展脚本的，或你允许扩展访问那个网站时的标签页地址） | 录播平台的页面嵌在别的网站页面里（例如 Canvas），你点页内按钮时 | 决定在页面上还是在扩展自己的确认窗口里确认发送（见下文“嵌在其他网站里的按钮”）；读 Cookie 时只取你所在页面的网站那一份 |
| 平台页面自己发出的、对策文件声明的那个请求的 JSON 结果里的几个指定字段（例如 Panopto `DeliveryInfo` 里的完整录像地址、各路媒体地址、是否只有音频、标题、时长；完整录像地址为空即受限标志） | 在你授权过、且对策文件声明了 `observe` 的平台页面上，页面自己发出这个请求时 | 找出完整录像和各条轨道，判断是否受限，给轨道和文件命名 |
| 文件夹列表上每一行的会话 ID；打开“更新这门课程到 LectureFlow”时还读各行的标题 | 在你授权过、且对策文件声明了页内入口的平台列表页上，页面打开时 | 在行上显示“已导入 / 刮削中 / 需要刷新”，批量发送时识别已经导入的课 |
| 你要发送的那几节课的讲座信息（扩展向该页同源的平台地址发出请求，只取声明的字段，同“指定字段”一行） | **只在你点了列表某一行的“发送至 LectureFlow”，或在“更新这门课程到 LectureFlow”里点发送之后**，只针对要发送的那几节 | 列表页不加载每节课的信息；发送时要靠它找出轨道、判断是否受限、命名 |
| 轨道所在网站的 Cookie（播放器嵌在别的网站页面里时，包括浏览器为你发送时所在的页面分开存放的那一份；为其他网站页面存放的那份不会交给 LectureFlow。Firefox 容器标签页里只读这个容器的；开了第一方隔离时只读你所在页面的网站那一份） | **只在你发送讲座**（弹窗或页内卡片里的发送按钮，或为“需要刷新”的任务点“续传”）时；对策文件写明不需要 Cookie 的平台（例如 Panopto）不读 | 让 LectureFlow 以你自己的登录状态，刮削你在这个页面上已经能播放的文件 |

扩展**不读取**：课程名、课程代码、文件夹结构、课件、页面上的其他文字和个人信息、你的浏览历史、其他标签页、表单内容、密码。

“授权过的网站”是指扩展认识的录播平台（例如 Panopto）或你手动添加的网站，并且你在浏览器弹出的权限确认框里点了同意。安装时扩展不获得任何网站的访问权限。弹窗里的“在此网站自动探测”只申请当前页面所在的网站；轨道在其他网站、发送时可能交出那些网站的 Cookie 时（对策文件为平台列出的网站、与页面同属一个网站的，或页面里嵌入的播放器所在网站的），那些网站单独列出，默认不选，由你逐个勾选；列出本身不读也不交任何 Cookie。

### 在录播平台页面里做的事

在你授权过、且有平台对策文件的录播平台（目前是 Panopto，包括嵌在 Canvas 等学习平台里的 Panopto 页面）上，扩展还做下面几件事。没有对策文件的网站上，这些都不会发生。

- **页内按钮**：对策文件声明了页内入口时，扩展在播放页加一个“发送至 LectureFlow”按钮；在文件夹列表的每一行加勾选框和“发送至 LectureFlow”，并加一个“更新这门课程到 LectureFlow”按钮。已经导入或正在刮削的行显示“已导入”或“刮削中”及进度。这些元素放在独立的 shadow DOM 里，不改动页面原有的内容和行为。
- **观察平台自己的请求**：对策文件声明了 `observe` 时，扩展在页面自己的 JavaScript 环境里包装 `fetch` 和 `XMLHttpRequest`。请求和响应原样交还给页面；只有当响应的地址正是对策文件声明的那一个时，扩展才从里面取出声明的几个字段（只取短字符串、数字和布尔值，从不取整段对象或数组），例如 Panopto 的完整录像地址和受限标志。响应的其余内容，包括其中的姓名、邮箱，不保存、不转发，不会离开页面。扩展不为此发出任何请求。
- **讲座信息请求**：文件夹列表不加载每节课的信息。只在你点了某一行的“发送至 LectureFlow”，或在“更新这门课程到 LectureFlow”里点发送之后，扩展才为要发送的那几节、向该页所在的平台（同源地址）逐节请求讲座信息，同样只取声明的字段。浏览器像页面自己请求时一样带上该网站的 Cookie，扩展读不到它们。结果只放在内存里，30 分钟后或标签页关闭时丢弃。
- **嵌在其他网站里的按钮**：录播平台的页面嵌在别的网站页面里时（例如学校的 Canvas），按钮也在。为了防止别的网站把录播页面做成透明、诱导你点击，扩展先看录播页面嵌在哪个网站里：这个网站在你的“信任的嵌入网站”里时，按钮照常在页面上打开卡片，但确认按钮出现后要稍等片刻才能点，在 Chrome 里卡片还必须真正可见；其他网站（或浏览器没告诉扩展是哪个网站）时，确认改在扩展自己的小窗口里完成，网页无法遮挡或伪造这个窗口。窗口里可以勾选“以后在这个网站里直接在页面上确认”，即把这个网站加入信任列表；扩展设置里列出这些网站，可以随时移除。在信任的网站里，嵌入的播放器太小、放不下确认卡片时（例如课程页里的小播放器），同样改在这个小窗口里确认，这时窗口里不显示防诱导点击的提示，也不再询问是否信任。“对这个平台不再提醒”只关掉受限提醒，不影响这里的确认。
- **列表页的状态**：打开文件夹列表时，为了在行上显示“已导入 / 刮削中”，扩展把这一页各行的会话 ID 交给本机的 LectureFlow 查询。只在本机，不联网；LectureFlow 没有运行时，不会为此启动它。列表页只处理当前这一页列出的课，不读文件夹名或其他课程信息，也不抓取其他页。

### 受限的媒体流与 DRM

- 平台对策文件读到受限标志（讲师或大学限制了这条媒体流）时，弹窗标出“可能受政策限制”，并显示受限提示：“检测到该媒体流或受讲师或大学政策限制。LectureFlow 仅将您有权访问的媒体流拉取至本地用于个人无障碍学习（转录/翻译）。请确保您遵守学校规定。发送前会再提醒一次。”点发送时会再确认一次，你确认之后才会发送。批量发送时，可能受限的那几节在发送前统一提醒一次，可以跳过它们。在这次提醒里，你可以勾选“对这个平台不再提醒”：之后这个平台的发送不再弹出提醒，受限提示也不再说“发送前会再提醒一次”，而是写明你已对这个平台关闭提醒。扩展设置里列出这些平台，可以随时恢复提醒。默认不勾选；受数字版权保护（DRM）的媒体流与此无关，照旧不发送。
- 媒体受数字版权保护（DRM）时，弹窗显示：“这条媒体流受数字版权保护（DRM），LectureFlow 不处理受保护的内容。”这样的轨道不能发送。

### 不处理的网站

扩展只用于学校给你的课堂录播。YouTube，以及 MOOC 与在线 / 付费课程平台（例如 Coursera、edX、Udemy、LinkedIn Learning 的 `/learning`、中国大学 MOOC、B 站课堂），在扩展的不处理名单里，所有浏览器的版本都一样：在这些网站上，弹窗只说明“这个网站不在 LectureFlow 的支持范围内”，扩展不读这些页面、不计数、不能授权这些网站，也不显示页内按钮。这些网站嵌在其他网站的页面里时（例如学习平台里的课程工具），扩展跳过嵌入的那一部分和它加载的媒体，不管媒体放在哪个服务器上。LectureFlow 收到这类网站的任务时同样拒绝。

### 交给谁

扩展只把数据交给**同一台电脑上的 LectureFlow**，经浏览器的 Native Messaging 通道传递。浏览器只允许 LectureFlow 登记过的扩展使用这个通道，网页无法访问它。

- 打开弹窗时：扩展版本、浏览器种类、界面语言（用来确认双方版本兼容），以及当前讲座的平台和会话 ID（用来检查这节课是否已经导入）。LectureFlow 回给扩展的是它自己的课程列表、导入状态和刮削进度。
- 打开带页内入口的文件夹列表时：平台和这一页各行的会话 ID，用来在行上显示“已导入 / 刮削中 / 需要刷新”。只查本机的 LectureFlow，不联网；LectureFlow 没有运行时，不会为此启动它。
- 打开“更新这门课程到 LectureFlow”或在其中换课程时：这一页各节的会话 ID、列表上的标题和你选的课程，用来识别已经导入的课。
- 发送讲座时（弹窗或页内卡片里的发送按钮，或批量发送）：你选的轨道地址、当前页面地址（作为 Referer；轨道在其他来源时只有页面的协议、域名和端口，见上表）、轨道所在网站的 Cookie（对策文件写明不需要时不读）、讲座标题（命中对策文件时是按选择器读到的标题；没有对策文件时是页面自己的标题，见上表，可能为空）、录制日期、时长、会话 ID、你选的课程，以及在媒体流可能受限时你是否确认了继续（或已对这个平台关闭提醒）。批量发送时每节一份。
- 点“在 LectureFlow 中打开”时：只有那节讲座或那个刮削任务在 LectureFlow 里的编号。

扩展本身不向任何远程地址发送这些数据。

### 扩展自己发出的网络请求

为了数出轨道、估算大小，扩展会向**轨道所在的服务器**拉取 HLS / DASH 播放清单，并对单个媒体文件发出 `HEAD` 请求；服务器不支持 `HEAD` 或不给出大小时，改发一个只要第一个字节的 `GET` 请求（`Range: bytes=0-0`），收到响应头就中止，不读取文件内容。这些正是网页上的播放器本来就会请求的地址。请求先在页面里、由引用了这个地址的那个页面或框架发出，浏览器像播放时一样带上该网站的 Cookie。页面里请求不成功（或没有拿到文件大小）时，扩展自己再请求，但只对你授权过的网站：只有下面几种情况才带该网站的 Cookie——地址与正在探测的页面同源（协议、域名和端口都相同；页面地址取自浏览器，不取页面自己的说法）；正在探测的页面本身命中平台对策文件，且地址属于对策文件声明的网站；对策文件命中的是页面里嵌入的播放器框架，且地址是这个框架自己引用的、与它同源的地址。其他地址一律不带 Cookie。带 Cookie 的这次请求不跟随跳转；遇到跳转，扩展对原地址改为不带 Cookie 再请求一次，这一次会跟随跳转（跳转后的地址可能不在你授权过的网站里，同样不带 Cookie）。所以扩展自己的这次再请求，同一个地址最多两次。播放清单最多读 2 MB，每个请求从发出到读完最多 10 秒，超出就中止、按失败处理。扩展不在请求里附加任何别的信息。此外只有一种请求：在文件夹列表上发送讲座时，扩展向**该页所在的平台**（同源地址）请求要发送的那几节课的讲座信息（见上文“在录播平台页面里做的事”）。除此之外，扩展不发出任何网络请求；观察平台自己的请求时，扩展不另外发出请求。

### 扩展保存什么、不保存什么

扩展只在浏览器的扩展存储里保存：

- 你的设置；
- 你从本地文件导入的平台对策文件；
- 你上一次选的课程（LectureFlow 里的课程编号），下次默认选中；
- 你选择了“对这个平台不再提醒”的平台：只有平台的标识（例如 panopto）和选择的时间，不含课程或讲座信息；在设置里恢复提醒即删除；
- 每节送入过的讲座用的是哪种内容选择（主要音视频 / 双机位 / 仅音频），只为“续传”时默认选同样的轨道：以平台与会话 ID 的哈希为键，不含标题、地址或课程，最多保留最近 200 条；
- 你信任的嵌入网站（录播页面嵌在这些网站里时，直接在页面上确认发送）：只有网站本身（协议、域名、端口，例如 `https://canvas.example.edu`）和加入的时间，不含页面地址、课程或讲座，最多 100 个；在设置里移除即删除。
- 因为正则太慢而停用的对策文件：只有对策文件的标识与版本、哪一条规则（在文件里的位置，例如 `/tracks/1/detect/urlPattern`）、用了多少毫秒、扩展版本和日期，不含页面地址或页面内容，最多 64 条；重新导入该文件、移除它，或在设置里重新启用内置对策文件即删除。

扩展**不保存** Cookie、轨道地址、页面地址或刮削记录（上面那份内容选择只有哈希和选项名，读不出讲座标题或地址）。探测结果只放在内存里，只为当前标签页服务，标签页关闭或跳转后就丢弃；文件夹列表上取得的讲座信息同样只在内存里，30 分钟后或标签页关闭时丢弃。

站点访问权限由浏览器管理，你可以随时在扩展的设置或浏览器的扩展管理页里撤销。卸载扩展会删除它保存的全部内容。

### LectureFlow 收到之后

- **Cookie 和完整的轨道地址**（其中可能带有签名参数）只存在 LectureFlow 进程的内存里，不写入磁盘、数据库或日志，也不出现在命令行参数里。刮削完成、取消或进入“需要刷新”时立即清除；刮削失败的任务为了能让你点“重试”，会把它们在内存里最多保留 30 分钟，到时清除，任务改为“需要刷新”；你移除这个任务或 LectureFlow 退出时也会清除。LectureFlow 重启后它们就不存在了，未完成的任务会标为“需要刷新”。
- LectureFlow 用内建的刮削工具 aria2c 把文件拉取到本机。Cookie 和 Referer 只通过本机回环地址（127.0.0.1）上带随机密钥的接口交给 aria2c（交出之前，LectureFlow 先确认这个接口确实属于它自己启动的 aria2c），aria2c 带着它们**直接向轨道所在的服务器**请求文件，也就是播放器本来就会访问的服务器。每个 Cookie 只发给它所属的网站。指向这台电脑本身的轨道地址一律拒绝；指向局域网的地址，只在录播页面本身也在局域网里时才刮削。
- LectureFlow 在本机数据库里保存一条刮削记录，用来显示进度和识别重复送入：平台、会话 ID、讲座标题、录制日期、时长、你选的课程、页面的域名（不含路径）、每条轨道的类型、域名和路径的哈希值（**不保存路径和查询参数**：有些平台的媒体路径本身就足以取得文件，签名和令牌通常在查询参数里）、状态和进度，以及在媒体流可能受限时你是否确认了继续。这些记录与 LectureFlow 的其他数据一样，只保存在你的电脑上。失败或已取消的任务可以在课程页移除；删除课程时，它的刮削记录一并删除。已完成的记录保留下来，用于识别重复送入。
- 日志里轨道地址只记域名和路径的哈希值，不记路径和查询参数。
- 刮削完成后，录音成为 LectureFlow 里的一节讲座。之后的转写、修复和翻译遵循 LectureFlow 本身的隐私说明（见 [README 的“隐私”一节](https://github.com/ShouldBeQuincy/LectureFlow#privacy)）：转写在你的电脑上进行；只有在你配置了模型服务商时，转写文字才会发给你选的服务商；扩展交来的讲座标题会成为这节讲座的原始文件名，配置了模型服务商时，LectureFlow 为讲座整理标题，会把这个文件名连同讲座摘要一起发给该服务商（这是 LectureFlow 本身的行为，只在你配置了服务商时发生）；如果你选择用 Google Colab 转写，录音会上传到 Google 的服务器，选择时会明确告诉你。

### 不做的事

- 不收集、不出售、不分享任何数据；没有遥测和统计。
- 不在后台定时抓取：只处理你打开的页面，只刮削你勾选的轨道。
- 不改动网页：包装 `fetch` / `XMLHttpRequest` 时，请求和响应原样交还给页面；页内按钮放在独立的 shadow DOM 里，不改页面原有的内容和行为。
- 不代替你登录，不读取或保存密码；扩展自己不写入、修改或删除 Cookie。在文件夹列表上发送时，扩展发出的讲座信息请求与 Panopto 播放页自己的请求相同，会请平台刷新登录状态；平台是否借此更新它自己的登录 Cookie 由平台决定，浏览器照常保存。
- 不尝试解密受 DRM 保护的媒体，也不录屏。
- 不加载、不执行远程代码。平台对策文件是纯数据（声明式字段和 CSS 选择器），不含脚本。

### 探测报告

弹窗和设置里的“导出探测报告”只在你点击时生成，内容先经过脱敏：去掉 Cookie；网址只保留协议、网站名和路径，路径里较长的编号换成形状相同的假值，查询参数只留**名字**、值换成占位符；页面文字替换为占位符；没有对策文件时读到的页面标题根本不写进报告。报告像浏览器的其他下载一样保存在你的电脑上，扩展不会上传它。如果你自愿把它附在公开的贡献请求（PR）里，它就会公开可见，提交前请自己再检查一遍。

### 你的控制

- 不发送讲座，扩展就不会读取任何 Cookie、不会向平台请求讲座信息，也不会把任何轨道交给 LectureFlow。
- 你可以随时撤销站点权限；撤销后，扩展只在你点图标时探测当前页面，页面里不再出现按钮，也不再观察平台的请求。
- 卸载扩展会删除它保存的全部内容。LectureFlow 的数据保存在你的电脑上（Windows `%APPDATA%\LectureFlow\`，macOS `~/Library/Application Support/LectureFlow/`，或你在设置里选的位置），由你自己管理。
- 我们收不到任何数据，所以也没有可供查询、导出或删除的“你的数据”。

### 变更

数据处理方式有任何变化，我们会先更新本声明，并在扩展的更新说明里写明；需要新的权限时，浏览器会再次请你同意。

### 联系

在 <https://github.com/ShouldBeQuincy/LectureFlow/issues> 提出问题，或发邮件到 <me@qu1ncy.dev>。

### Chrome 网上应用店声明

LectureFlow 浏览器扩展对用户数据的使用遵守 Chrome 网上应用店用户数据政策（Chrome Web Store User Data Policy），包括其中的“有限使用”（Limited Use）要求。

## English

Last updated: 11 November 2026

This policy explains what the LectureFlow browser extension "Send to LectureFlow" ("the extension") reads, when it reads it, who it gives it to, and what it keeps. The extension is a companion to the LectureFlow desktop app ("LectureFlow"). Both are open source; the source code is at <https://github.com/ShouldBeQuincy/LectureFlow>.

### In short

- The extension talks only to LectureFlow **on the same computer**. It does not connect to any server of ours (we don't have one). No account, no telemetry, no analytics, no ads.
- The extension reads cookies only when you send a lecture, and then only the cookies of the website that hosts the tracks you selected. It gives them only to LectureFlow on your computer. For the websites a platform profile lists, it gives the cookies for those track addresses; for any other website, only two kinds: cookies of the same website as the page you are on, and, when a player embedded in the page itself requested those tracks, the cookies that the player's website allows to be sent across sites (in the Firefox version, only the partitioned ones described below). A page that merely names another website's address gets none of that website's cookies. On platforms whose profile says the media needs no cookies (for example Panopto), it reads none at all. When the player is embedded in another website's page (for example Canvas), the browser keeps its cookies apart for each website it is embedded in (partitioned cookies): LectureFlow gets only the ones kept for the page you send from, and the ones that website keeps for other websites' pages are not given to LectureFlow.
- The extension does not keep cookies. LectureFlow keeps them in memory only, for that one fetch, and never writes them to disk, to its database or to a log.
- The extension does not read course names, course codes, folders or slides.
- On recording platforms you have allowed and that have a platform profile, the extension adds "Send to LectureFlow" buttons to the page and takes only a few fields, named in the profile, from the answers to the platform's own requests. The rest of those answers (including your name or e-mail address) never leaves the page.
- We, the developers, receive no data at all.

### What the extension reads, and when

| Data | When | What for |
|---|---|---|
| Media elements on the page (the addresses of `<video>`, `<audio>` and `<source>`, including those in same-origin iframes), the addresses of media files and playlists the page has loaded, and whether the media is DRM-protected | On sites you have allowed: when a recording page opens. On other sites: only after you click the extension's icon | To count how many audio and video tracks on the page can be fetched |
| Network requests to sites you have allowed: address, `Content-Type` and size (read-only; nothing is blocked or changed; only media files and playlists are kept, everything else is ignored) | The browser reports only requests to sites you have allowed; they are kept in memory per tab and dropped when the tab moves to another page or closes | To recognise media files and playlists whose addresses have no file extension |
| The contents of HLS / DASH playlists; the size of single media files | When a playlist or media file is found | To split a playlist into audio and video tracks and estimate their size |
| The lecture's title, recording date and length; the platform's restriction flag (whether the lecturer or university restricts this stream); the session ID | When the page matches a platform profile, using the CSS selectors written in that profile | To name the tracks and the file; to show the restriction notice when the stream may be restricted (see "Restricted streams and DRM" below); to recognise a lecture you have already sent |
| The page's own title (the line the browser shows on the tab) | From extension version 0.2.1: when no platform profile matches (generic detection), whenever the extension looks at that page (same times as the first row). Only the title of the page or frame being looked at, and only after the check for sites LectureFlow does not handle (see below): those sites, and frames inside them, are never read | With the website's name dropped from its end (for example "… : Internet Archive"), as the lecture's title: shown in the popup and, when you send the lecture, given to LectureFlow on your computer; the extension sends it nowhere else (for what LectureFlow then does with it, see "What LectureFlow does with it" below). When only the website's name or a placeholder such as "Recording" or "Video" is left, it is not used and LectureFlow names the lecture. Never written into a detection report |
| The address of the current page | Whenever the extension looks at that page (same times as the first row) | To match a platform profile; sent as the Referer when fetching (as browsers do by default: the page address, without the part after `#`, when all the chosen tracks are on the page's origin — the same scheme, domain and port; only the page's scheme, domain and port when any track is on another origin; no Referer for plain HTTP tracks on an HTTPS page); without a platform profile, a hash of the normalised address is used as the session ID |
| Which website's page a recording page is embedded in (scheme, domain and port only: what the browser tells the extension's script in the page, or the tab's address when you allowed the extension that website) | When a recording platform's page is embedded in another website's page (for example Canvas) and you click an in-page button | To decide whether you confirm on the page or in the extension's own confirmation window (see "Buttons inside other websites" below); when reading cookies, to take only the ones kept for the page you are on |
| A few named fields of the JSON answer to the one request, declared in the platform profile, that the platform's page makes itself (for Panopto's `DeliveryInfo`: the full recording's address, the media addresses, whether it is audio only, the title and the length; an empty full-recording address is the restriction flag) | On platform pages you have allowed whose profile declares `observe`, when the page itself makes that request | To find the full recording and the tracks, to tell whether the stream may be restricted, and to name the tracks and the file |
| On a folder list: the session ID of each row; when you open "Update this course in LectureFlow", also each row's title | On platform list pages you have allowed whose profile declares in-page entries, when the page opens | To show "Imported", fetching progress or "Needs refresh" on the rows, and to recognise lectures already imported when sending several |
| The lecture details of the lectures you send (the extension requests them from the platform at the page's own origin and takes only the declared fields, as in the "named fields" row) | **Only after you click "Send to LectureFlow" on a row of the list, or send from "Update this course in LectureFlow"**, and only for the lectures being sent | A list page does not load each lecture's details; sending needs them to find the tracks, check for restrictions and name the files |
| Cookies of the website that hosts the tracks (when the player is embedded in another website's page, including the ones the browser keeps apart for the page you send from; the ones kept for other websites' pages are not given to LectureFlow. In a Firefox container tab, only that container's; with first-party isolation on, only the ones kept for the website of the page you are on) | **Only when you send a lecture** (the send button in the popup or the in-page card, or "Resume" on a fetch that needs it); none on platforms whose profile says the media needs no cookies (for example Panopto) | So that LectureFlow can fetch, with your own sign-in, the files you can already play on that page |

The extension **does not read** course names, course codes, folder structures, slides, any other text or personal information on the page, your browsing history, other tabs, form contents or passwords.

"Sites you have allowed" means recording platforms the extension knows (for example Panopto) and sites you add yourself, after you have agreed in the browser's own permission prompt. At installation the extension has access to no website at all. "Detect automatically on this site" in the popup asks only for the website of the current page; when tracks are on other websites whose cookies a send may hand over (websites a platform profile lists, websites the page belongs to, or the website of a player embedded in the page), those websites are listed separately, unticked, for you to tick one by one; listing them reads and hands over no cookie.

### What it does on recording platform pages

On recording platforms you have allowed and that have a platform profile (Panopto for now, including Panopto pages embedded in Canvas and other learning platforms), the extension also does the following. On sites without a profile, none of this happens.

- **In-page buttons**: when the profile declares in-page entries, the extension adds a "Send to LectureFlow" button to the player page, and on a folder list a check box and "Send to LectureFlow" on each row plus an "Update this course in LectureFlow" button. Rows already imported or being fetched show "Imported" or the fetching progress. These elements live in their own shadow DOM and do not change the page's own content or behaviour.
- **Watching the platform's own requests**: when the profile declares `observe`, the extension wraps `fetch` and `XMLHttpRequest` in the page's own JavaScript world. Requests and responses are passed to the page unchanged; only when a response comes from the one address the profile declares does the extension take the declared fields from it (short strings, numbers and booleans only, never a whole object or list), for example Panopto's full-recording address and restriction flag. The rest of the response, including any name or e-mail address in it, is neither kept nor passed on and never leaves the page. The extension makes no request of its own for this.
- **Lecture details requests**: a folder list does not load each lecture's details. Only after you click "Send to LectureFlow" on a row, or send from "Update this course in LectureFlow", does the extension request the details of the lectures being sent, one after another, from the platform at the page's own origin, again taking only the declared fields. The browser attaches that website's cookies as it does for the page's own requests; the extension cannot read them. The results are held in memory only and dropped after 30 minutes or when the tab closes.
- **Buttons inside other websites**: when a recording platform's page is embedded in another website's page (your school's Canvas, for example), its buttons are there too. So that another website cannot make the recording page transparent and trick you into clicking, the extension first looks at which website it is embedded in. If that website is on your "Trusted embedding sites", the button opens its card on the page as usual, but the confirming button can be clicked only after a short moment, and in Chrome only while the card is really visible. On any other website (or when the browser does not tell which), you confirm in the extension's own small window instead, which a web page can neither cover nor imitate. In that window you can tick "From now on, confirm on the page itself" for that website, which adds it to the trusted list; the extension's settings list those websites, and you can remove them at any time. On a trusted website, when the embedded player is too small to hold the confirmation card (a small player in a course page, for example), you confirm in that small window as well; it then shows no warning about tricked clicks and does not offer to trust the website. "Don’t remind me again for this platform" only turns off the restriction reminder and does not change this confirmation.
- **Status on a list page**: when a folder list opens, the extension asks LectureFlow on your computer about the session IDs of the rows on that page, to show "Imported" or fetching progress. This stays on your computer and nothing goes online; if LectureFlow is not running, it is not started for this. Only the lectures listed on the current page are handled; folder names and other course information are not read, and other pages are not fetched.

### Restricted streams and DRM

- When a platform profile finds the restriction flag (the lecturer or university restricts this stream), the popup marks the lecture "May be restricted" and shows this notice: "This stream may be restricted by your lecturer’s or university’s policy. LectureFlow only fetches streams you have access to, to this computer, for your personal accessible learning (transcription/translation). Please make sure you follow your school’s rules. You will be reminded once more before sending." You confirm once more when you send, and nothing is sent before you do. When you send several lectures at once, you are reminded once about those that may be restricted and can skip them. In that reminder you can tick "Don’t remind me again for this platform": later sends to that platform go without the reminder, and the notice then says that you turned the reminder off for the platform instead of "You will be reminded once more before sending." The extension's settings list those platforms, and you can turn the reminder back on at any time. It is not ticked unless you tick it. DRM-protected streams are not affected: they are never sent.
- When the media is protected by digital rights management (DRM), the popup shows: "This stream is protected by digital rights management (DRM). LectureFlow does not handle protected content." Such tracks cannot be sent.

### Sites LectureFlow does not handle

The extension is only for the lecture recordings your school gives you. YouTube, and MOOCs and online or paid course platforms (for example Coursera, edX, Udemy, LinkedIn Learning's `/learning`, 中国大学 MOOC, Bilibili's course pages), are on the extension's do-not-handle list, the same in every browser's version: on those sites the popup only says "LectureFlow does not work on this site"; the extension does not read those pages, shows no count, cannot be allowed on them and shows no in-page button. When such a site is embedded in another website's page (for example a course tool inside a learning platform), the extension skips the embedded part and the media it loads, whichever server holds the media. LectureFlow refuses tasks from these sites as well.

### Who receives it

The extension gives data only to **LectureFlow on the same computer**, through the browser's Native Messaging channel. The browser lets only the extensions LectureFlow has registered use that channel; web pages cannot reach it.

- When you open the popup: the extension's version, the kind of browser and the interface language (to check that both sides are compatible), and the platform and session ID of the lecture on the page (to check whether it was already imported). LectureFlow answers with its own course list, the import state and fetch progress.
- When a folder list with in-page entries opens: the platform and the session IDs of the rows on that page, to show "Imported", fetching progress or "Needs refresh" on the rows. This only asks LectureFlow on your computer and nothing goes online; if LectureFlow is not running, it is not started for this.
- When you open "Update this course in LectureFlow" or change its course: the session IDs of the lectures on that page, their titles as listed, and the course you chose, to recognise lectures already imported.
- When you send a lecture (the send button in the popup or the in-page card, or sending several at once): the addresses of the tracks you chose, the address of the current page (as the Referer; only the page's scheme, domain and port when the tracks are on another origin, see the table above), the cookies of the website hosting those tracks (none when the platform profile says they are not needed), the lecture's title (the one a platform profile's selectors read; without a profile, the page's own title, see the table above, which may be empty), recording date and length, the session ID, the course you chose, and whether you confirmed that you want to continue when the stream may be restricted (or turned that reminder off for the platform). When sending several, one such set per lecture.
- When you click "Open in LectureFlow": only LectureFlow's own number for that lecture or fetch.

The extension itself sends none of this to any remote address.

### Network requests the extension makes itself

To count the tracks and estimate their size, the extension fetches HLS / DASH playlists from **the server that hosts the tracks** and sends `HEAD` requests for single media files; when a server does not answer `HEAD` or gives no size, it sends one `GET` request for the first byte only (`Range: bytes=0-0`) and stops as soon as the response headers arrive, without reading the file. These are the same addresses the page's own player requests. The requests are first made from within the page, by the page or frame that refers to the address, and the browser attaches that website's cookies just as it does during playback. When that fails (or gives no file size), the extension requests the address itself, only on websites you have allowed, and attaches that website's cookies only when: the address is on the same origin as the page being looked at (the same scheme, domain and port; the page's address is taken from the browser, not from what the page says); or the page being looked at itself matches a platform profile and the address is on a website that profile declares; or the profile matches a player frame embedded in the page and the address is one that frame refers to itself, on that frame's own origin. Any other address is requested without cookies. The request with cookies does not follow redirects; on a redirect the extension asks for the original address once more without cookies, and that request follows the redirect (possibly to a website you have not allowed, again without cookies). So the extension's own retry takes two requests per address at most. A playlist is read up to 2 MB at most, and each request is given 10 seconds at most from start to the last byte read; beyond either, it is stopped and counts as failed. The extension adds nothing else to these requests. There is one other kind of request: when you send lectures from a folder list, the extension requests the details of those lectures from **the platform the page belongs to** (at the page's own origin; see "What it does on recording platform pages" above). The extension makes no other network requests; watching the platform's own requests involves no request of its own.

### What the extension keeps, and what it doesn't

In the browser's extension storage, the extension keeps only:

- your settings;
- platform profiles you imported from a local file;
- the course you chose last time (its number in LectureFlow), so that it is preselected next time;
- the platforms for which you chose "Don’t remind me again for this platform": only the platform's identifier (for example panopto) and when you chose it, nothing about courses or lectures; turning the reminder back on in the settings deletes it;
- which of the three choices (Main audio & video, Two cameras, Audio only) each lecture you sent was sent with, only so that "Resume" starts with the same tracks: keyed by a hash of the platform and session ID, without title, address or course, the latest 200 at most;
- the embedding websites you trust (when a recording page is embedded in one of them, you confirm sending on the page itself): only the website (scheme, domain and port, for example `https://canvas.example.edu`) and when you added it, no page address, course or lecture, 100 at most; removing it in the settings deletes it.
- profiles stopped because a regular expression was too slow: only the profile's identifier and version, which rule (its place in the file, for example `/tracks/1/detect/urlPattern`), how many milliseconds it took, the extension version and the date, no page address or page content, 64 at most; importing that file again, removing it, or enabling a built-in profile again in the settings deletes it.

The extension **does not keep** cookies, track addresses, page addresses or a fetch history (the choices above are only hashes and option names; no lecture title or address can be read from them). What it finds on a page is held in memory for that tab only and is dropped when the tab closes or moves to another page; lecture details fetched for a folder list are likewise held in memory only, and dropped after 30 minutes or when the tab closes.

Site access is managed by the browser; you can withdraw it at any time in the extension's settings or on the browser's extensions page. Uninstalling the extension deletes everything it kept.

### What LectureFlow does with it

- **Cookies and full track addresses** (which may carry signed parameters) exist only in the memory of the LectureFlow process. They are never written to disk, to the database or to a log, and never appear on a command line. They are cleared as soon as a fetch finishes, is cancelled or needs a refresh. For a fetch that failed, they stay in memory for up to 30 minutes so that you can click "Retry"; then they are cleared and the fetch needs a refresh. Removing that fetch or quitting LectureFlow clears them too. After LectureFlow restarts they are gone, and unfinished fetches are marked "Needs refresh".
- LectureFlow fetches the files to your computer with its built-in fetch tool, aria2c. The cookies and the Referer are handed to aria2c only through an interface on the local loopback address (127.0.0.1) protected by a random secret (after LectureFlow has checked that this interface really belongs to the aria2c it started), and aria2c uses them to request the files **directly from the server that hosts the tracks**, the same server the player uses. Each cookie is sent only to the website it belongs to. Track addresses that point to this computer are refused; addresses on the local network are fetched only when the recording page itself is on the local network.
- LectureFlow keeps a fetch record in its local database, to show progress and recognise a lecture you send twice: the platform, session ID, lecture title, recording date and length, the course you chose, the page's domain (without its path), each track's type, domain and a hash of its path (**neither paths nor query parameters are kept**: on some platforms the media path alone is enough to get the file, and signatures and tokens are usually in query parameters), the state and progress, and whether you confirmed that you want to continue when the stream may be restricted. Like the rest of LectureFlow's data, these records stay on your computer only. Failed or cancelled fetches can be removed on the course page; deleting a course deletes its fetch records. Records of finished fetches are kept to recognise duplicates.
- Its logs record track addresses only as the domain and a hash of the path, without the path or query parameters.
- When fetching is done, the recording becomes a lecture in LectureFlow. Transcription, correction and translation then follow LectureFlow's own privacy terms (see the [Privacy section of the README](https://github.com/ShouldBeQuincy/LectureFlow#privacy)): transcription runs on your computer; transcribed text is sent only to the model provider you set up, if you set one up; the lecture title the extension hands over becomes the lecture's original file name, and when you have set up a model provider, LectureFlow sends that file name together with the lecture's summary to the provider when it works out a title for the lecture (LectureFlow's own behaviour, and only when a provider is set up); if you choose to transcribe with Google Colab, the recording is uploaded to Google's servers, and you are told so when you choose it.

### What it never does

- It does not collect, sell or share any data. No telemetry, no analytics.
- No background crawling: it works only on pages you open and fetches only the tracks you tick.
- It does not change the web page: when it wraps `fetch` / `XMLHttpRequest`, requests and responses are passed to the page unchanged; its in-page buttons live in their own shadow DOM and leave the page's own content and behaviour alone.
- It does not sign in for you, and does not read or store passwords. The extension itself does not write, change or delete cookies. When you send from a folder list, its lecture details request is the same as the one Panopto's own player page makes and asks the platform to refresh your sign-in; whether the platform then updates its own sign-in cookie is up to the platform, and the browser stores it as usual.
- It does not try to decrypt DRM-protected media, and does not record the screen.
- It does not load or run remote code. Platform profiles are plain data (declarative fields and CSS selectors) and contain no scripts.

### Detection reports

"Export detection report" in the popup and in the settings creates a report only when you click it, and redacts it first: cookies are removed; addresses keep only the scheme, the site name, the path (long IDs in it replaced by fakes of the same shape) and the **names** of query parameters (their values replaced by placeholders); text from the page is replaced by placeholders, and the page title read without a platform profile is left out altogether. The report is saved on your computer like any other download, and the extension never uploads it. If you choose to attach it to a public contribution (pull request), it becomes public, so please check it yourself first.

### Your choices

- Unless you send a lecture, the extension reads no cookies, requests no lecture details from the platform and gives no tracks to LectureFlow.
- You can withdraw site access at any time; the extension then looks at a page only when you click its icon, adds no buttons to the page and no longer watches the platform's requests.
- Uninstalling the extension deletes everything it kept. LectureFlow's data stays on your computer (Windows `%APPDATA%\LectureFlow\`, macOS `~/Library/Application Support/LectureFlow/`, or the place you chose in its settings) and is yours to manage.
- Since we receive no data, there is no "your data" for us to look up, export or delete.

### Changes

If the way the extension handles data changes, we will update this policy first and say so in the extension's release notes. If a new permission is needed, the browser will ask for your consent again.

### Contact

Open an issue at <https://github.com/ShouldBeQuincy/LectureFlow/issues> or email <me@qu1ncy.dev>.

### Chrome Web Store statement

The LectureFlow browser extension's use of user data complies with the Chrome Web Store User Data Policy, including the Limited Use requirements.
