# AGENTS.md — 给 AI 代理与开发者的协作指引

本文档记录本仓库的**硬约束、易踩的坑与必须遵守的规则**。改代码前先读
`docs/CONTRACT.md`（接口与数据模型）与本文件（行为约束）。

---

## 1. 项目本质

- 本仓库是**远程 SSH 的 web 终端**（不是本地终端）：后端用 `x/crypto/ssh` 主动连出去，
  用 `pkg/sftp` 管理远程文件；**不在本机执行用户命令**（唯一例外是内嵌的 SFTP/SSH 测试服务器，
  仅测试用）。
- 产物是**单个自包含 Go 二进制**（`./webssh`）：前端 `dist` 经 `//go:embed all:embed` 打进
  `backend/embed` 编译进二进制。
- 只通过 **unix socket** 提供服务（`WEBSSH_ADMIN_SOCK`），**不监听任何 TCP 端口**；
  baseurl 前缀（`WEBSSH_ADMIN_BASEURL`）在运行期注入 `<base href>`。
- 数据（会话配置、加密凭据、历史、快捷指令、主机密钥）全部落在 `WEBSSH_DATA_DIR`；
  两个进程实例同时用同一个数据目录会互相覆盖，**不要**这么干（用不同目录）。

## 2. 构建约定（重要）

- 前端**必须**用相对 base 构建（`vite.config.ts` 的 `base: './'`）。真实 baseurl 由后端在
  响应 `index.html` 时注入并改写 `./assets/` 前缀；**禁止**把 baseurl 硬编码进产物。
- 前端所有 API/WS 路径基于 `document.baseURI` 解析（`src/serverapi/index.ts` 的
  `runtimeBase()` / `wsUrl()`），不要用构建期的 `import.meta.env.BASE_URL` 拼路径。
- `Makefile` 是唯一构建入口：前端 `dist` → `backend/embed` → `go build -o ./webssh`，
  结束时 `rm -rf backend/embed`（**构建后该目录不存在是正常的**）。
  `make test` 用 `//go:embed` 需要的占位文件补齐 embed 目录——别把占位文件提交进 git。
- 版本号经 `-ldflags -X main.appVersion=` 注入，不要写死代码常量。
- 每次构建都 `go clean -cache`：这是刻意的，保证 embed 资产的变化一定被编进去。
- 改动前端后必须重新 `make`；只改源码不重新构建，二进制里跑的还是旧资源。
- 静态资源缓存头**必须**保持三种分类（见第 4 节），改错会让用户永远拿到旧页面。
- **飞牛应用包（fpk）的脚手架不在本仓库**：打包工程由 fpkstudio 维护
  （`/vol1/@appshare/fpkstudio/projects/WebSSH`），本仓库只产出 `./webssh`，打包时把该二进制
  放进工程的 `app/bin/`。该工程 `manifest` 的两个字段是「飞牛文件」功能的硬前提，改动前先看
  第 4 节：`micro_app=true`（微应用 + JS SDK 宿主桥）、`disable_authorization_path=false`
  （要用文件选择器拿授权）。`disable_authorization_path=true` 会让应用中心把本应用的
  `trim.file.userAccess` 能力状态置成 `-1`，选择器点「确定」时授权接口直接回 `31003`
  （界面「应用鉴权异常」）。

## 3. 并行开发的边界（冻结文件）

以下文件由契约维护者（Lead）冻结，**其他人只读**：
`frontend/package.json`、`vite.config.ts`、`tsconfig.json`、`env.d.ts`、`index.html`、
`src/main.ts`、`src/style.css`、`src/serverapi/index.ts`、`src/i18n/*`、`src/utils/*`、
`src/composables/{useTheme,useViewportHeight,useMobileLayout}.ts`、`src/stores/sessionUi.ts`、
`docs/CONTRACT.md`、`Makefile`、`.gitignore`、`backend/go.mod`（依赖清单）。

需要新 i18n 文案、新 API 字段、改 store 签名时：**先改 `docs/CONTRACT.md` 并知会所有人**，
再改实现。并行编辑同一文件必然互相覆盖。

不引入 `package.json` / `go.mod` 之外的依赖（后端只允许 `x/crypto`、`pkg/sftp`；
`creack/pty` 仅限测试服务器）。不使用 Element Plus、vue-router、dayjs。

## 4. 后端要点与坑

**入口与生命周期（`main.go`）**

- 顺序：检查 socket 是否已被占用（占用即退出）→ 准备数据目录 → **先绑定成功再进信号等待**。
  占用检查刻意排在准备数据目录**之前**：`newAdminMux` 会建 master.key、读写会话配置，让一个
  注定要退出的实例先碰一遍数据目录纯属多余（两个进程共用数据目录会互相覆盖）。
  绑定失败就退出：旧实现（参考项目）在 goroutine 里 bind、失败只打日志，进程会「活着但没有任何
  socket」，supervisor 看不出异常、用户也访问不了。
- `listenAdminSocket` 的顺序是**先直接绑、绑不上再判残留**（`net.DialTimeout` 探活）：只有
  确认没有活着的监听者才删残留 socket 文件重试。旧实现无条件 `os.Remove` 再 bind，两个实例
  同时启动时会把先到者**正在用**的 socket unlink 掉——后到者绑定成功、先到者变成不可达却还在
  读写同一个数据目录。
- 收到 SIGTERM/SIGINT/SIGQUIT：先 `srv.Close()` 停入口 → 终止所有终端与 SFTP 连接 →
  删 socket → **保留数据目录**（会话配置必须活过重启）。
- `bootID`（每次启动随机）经 `/api/info` 下发，前端据此判断「后端换进程了 → 内存态会话全没了」。

**路由与静态资源（`admin.go` / `admin_routes.go` / `static.go`）**

- `GET /api/info`、`/api/sessions`、`/api/hostkeys`、`/api/trim/status` 之外，**带参数的接口一律
  POST + JSON body**。原因：「查询串在网关/反代链路上不保证原样送达」是实测过的坑，
  而 body 是端到端可靠的。
- 未命中路径回退 `index.html`（SPA），但 `/api/*` 未命中必须回 **404 JSON**，不能退化成 HTML。
- 缓存头三分类：`*.html` → `no-cache, must-revalidate`（每次响应都注入 base href，
  缓存住会把旧页面钉死）；`assets/*` → `public, max-age=31536000, immutable`（文件名带
  内容哈希）；其余根级文件 → `public, max-age=86400`。
- `index.html` 的 base 注入：`<base href="<baseurl>/">` + 把 `./assets/` 改写成
  `<baseurl>/assets/`。`baseurl` 为空时两者都不做。

**会话与终端（`sessions.go` / `sessions_runtime.go` / `terminal.go`）**

- 会话配置（磁盘）与「已打开的会话」（内存 `SessionRuntime`）是两回事：
  `open` 幂等，`close` 才终止终端 + SFTP，进程重启后只有配置活着。
- 历史文件**必须**以 `O_RDWR|O_CREATE|O_APPEND` 打开：只写打开会让挂载时的回放与
  `/api/terminals/history` 读失败（EBADF）。
- 回放历史 + 实时广播必须在**同一把锁**内完成，否则字节会重复或丢失；向连接的写入要带
  写超时（约 5s），超时即解挂载——一次失联的写不能把锁占死（同会话的挂载/历史接口会被一起堵死）。
- **单挂载点**：一个终端同时只有一个操作端。新挂载成功后给旧连接发 `\x1b]detached\x07`，
  再以 close code **4001** 关闭；旧连接的残留输入在服务端丢弃。
- 关闭顺序：`ssh.Session` → `ssh.Client` → 历史文件。关闭过程中的 `EOF`/`net.ErrClosed`/
  「被信号杀掉」一律是**正常终止**，不要当错误上抛（参考项目在这上面栽过：把 Kill 导致的
  非零退出当失败，DELETE 对运行中的会话回 404）。
- shell 退出：追加 `[process exited]` 与 `\x1b]exit\x07`，历史保留 10 分钟后回收；
  回收后重新挂载会收到 `gone` 控制帧。
- 浏览器断开**只解挂载**：终端继续跑、输出继续写历史文件，刷新后可重新挂载回放。

**WebSocket（`internal/ws`）**

- 自实现帧编解码（握手用 `Sec-WebSocket-Accept`）。**必须有上限**：单帧 1 MiB、整条消息
  4 MiB、`resize` 行列 1–1000。客户端给的长度字段直接决定内存分配，没有上限就是一个
  客户端打爆进程（进程承载全部会话）的入口。
- 控制帧不走 PTY：`\x1b]resize;<cols>;<rows>\x07`、`\x1b]ping\x07`。
- 「会话/终端不存在」要附 `\x1b]gone\x07` 控制帧，前端**只认这个**——
  用人类可读文本（如 `session not found`）判定会被终端里 `echo` 出来的同样文本误判。

**文件（`sftp.go` / `files.go` / `files_write.go` / `files_upload.go` / `files_download.go`）**

- 目标路径一律 `path.Clean` 并拒绝 `..` 逃逸；`remove` 的 `dir` 参数必须与真实类型一致
  （不然会误删整棵树）。
- 上传：`http.MaxBytesReader` 卡住整个请求体（`ParseMultipartForm` 的内存阈值不是总量上限），
  边收边写不落内存；单文件写失败要删掉半截文件；客户端取消（`r.Context()` 结束）要中止写入。
- 上传的 filename 可能是**相对路径**（目录上传）：逐段校验（拒绝空段、`.`、`..`、绝对路径），
  在目标目录下按相对路径建目录。**不要**直接取 `path.Base`（目录上传会被拍平）。
- 目录下载：先打成临时 zip 再发正文——响应头一旦发出就没法再报错，半截 zip 会让用户以为
  下载成功（参考项目的注释里记着这个坑）。
- `upload-from-nas` 的 `source` 是本机路径，来自前端用 `@trimjs/web-app` 选中的**已授权**文件；
  读之前尽量用 `trim.file.checkUserACL` 判读权限（飞牛不可用时跳过并记日志）。

**凭据与远程命令（`credentials.go` / `remote_exec.go` / `fileops.go` / `archive.go` / `exportnas.go`）**

- `saveCredential=false` 的会话**不落盘任何凭据**，只在内存里保留（且只由一次成功的 `open` 写入——
  表单里填的凭据不进内存，否则「不保存凭据」的会话点开就直接连上，与开关承诺矛盾）；
  close/退出即失效，下次 `open` 回 `credential_required`。
- 远程命令一律走 `remote_exec.go` 的 helper：`ssh.Session` 的 exec 通道 + `sh -c` +
  **所有插值路径用单引号引用**（`shellQuote`）+ 采集 stdout/stderr + 超时 + ctx 取消。
  绝不把用户给的路径裸拼进命令。
- **失败善后用 `context.WithoutCancel`**：最常见的失败是「客户端断开」，用 `r.Context()`
  会永远清不掉写了一半的产物。
- `resolveRemotePath` **不要**对路径 `TrimSpace`：名字以空格结尾的目录会被静默改坏
  （症状：`fs/list` 能列出它，但 list/export/remove 全回 `not_found`）。见第 12.2 节。
- 压缩/解压/导出的上限与取舍（条目数 2000、产物 4 GiB、软链接跳过、跨设备 move 报 conflict）
  都写在 `docs/CONTRACT.md` 第 12.1 节，改之前先读。
- 远程命令的**两条路**别用错：`runRemote` 是「拨号 + 跑一条 + 关掉这条连接」（tar 这类一次性
  长命令）；`execOn(ctx, client, cmd, timeout)` 是在**已有的**连接上跑一条，给**高频短命令**用。
  目前只有系统信息轮询走第二条（`stats.go` 的 `statsClient`，5 秒一次）——它要是每次新拨一条
  SSH，NAS 上光认证与密钥交换就够呛；但也不能挂在 `r.base` 上（那被 `sftpMu` 保护，
  一次大文件上传会把轮询堵死）。`execOn` 超时会关掉那条连接，所以缓存 client 的调用方
  必须在失败时丢掉它（见 `dropStatsClient`），否则之后每次轮询都撞在死连接上。

**数据与凭据（`internal/store` / `internal/secret` / `hostkeys.go`）**

- 所有落盘写操作：互斥锁串行 + tmp 文件 + `rename` 原子替换。
- 凭据三字段（password/privateKey/passphrase）用 AES-256-GCM 加密，AAD 绑定
  `webssh:v1:<field>:<sid>`；**解密失败必须报错**，绝不能静默当成空密码（表现为「密码莫名失效」）。
- `master.key` 必须 0600；支持 `WEBSSH_MASTER_KEY` 外部提供。在注释/文档里如实写明威胁模型：
  防的是「拿到磁盘快照的人读到明文」，不防「能读数据目录的攻击者」。
- 主机密钥：未知 → 带上 fingerprint/keyType 报 `hostkey_unknown`（前端会引导用户确认）；
  不匹配 → `hostkey_mismatch`，**绝不覆盖**已记录的值。
- 飞牛开放 API 只能服务端调（token 在 `TRIM_API_TOKEN`，每次从环境变量读，不持久化）；
  飞牛不可用要如实返回 `available:false` + 原因，不要假装成功。

## 5. 前端要点与坑

- 一律 `<script setup lang="ts">` + Composition API；TS 开了 `strict`、`noUnusedLocals`、
  `noUnusedParameters`。
- **不要把会话/终端状态塞进组件**：`appTabs`（应用标签）、`home`（会话配置 + 实时状态）、
  `terminals`（终端子标签）、`sftp`（文件浏览）、`sessionUi`（激活子标签/左栏显隐）各管一段；
  `sessionUi.activeSubOf(sid)` 是「当前激活子标签」的**唯一来源**（终端激活写自己的 uid，
  文件管理写 `FILES_TAB`）。
- 面板一律**保持挂载**、`absolute inset-0` 叠放，非激活的加 `.invisible`（`visibility: hidden`）：
  **不要**用 `v-if`（切标签会销毁 xterm 与 WS，重挂载要重新握手 + 回放历史，用户会看到闪屏与
  丢失的滚动位置），**也不要**用 `v-show`/`display:none`——`display:none` 下容器尺寸为 0，
  FitAddon 量不到尺寸、WebGL 渲染器会反复触发 0 尺寸重排。`visibility:hidden` 保留元素尺寸，
  xterm 完全不知道自己被隐藏，重新可见时不需要重新 fit。
- 终端面板职责边界：`TerminalPane` 只管自己的 xterm + WS + 命令出口（`paneControls`），
  **不负责**子标签栏与文件管理；子标签栏在 `SessionView`。
- 历史回放只有一处：后端在挂载时回放，**前端不要再自己拉 `/api/terminals/history` 写一遍**
  （会重复显示）。`restoring` 只用于「等 ready 前的提示」。
- 订阅媒体查询要用**闭包里的** `MediaQueryList` 注册/移除（模块级共享变量会让 remove 掉别人
  的监听，每挂载一个面板就多一个永久监听）。
- 触屏上没有 hover：需要「悬停才出现」的按钮用 `.hover-reveal`（父元素 `.group`），
  **不要**用 Tailwind 的 `group-hover:opacity-*`（Tailwind 4 给 hover 变体包了
  `@media (hover: hover)`，触屏上永不生效，按钮等于不存在）。
- 移动端判定用 `useMobileLayout()`（触屏或窄视口），**不要**用 `md:hidden`：iPad 宽度
  ≥768px 会被判成桌面而丢掉底部辅助键条。
- 但**「屏幕大不大」与「有没有键盘/鼠标」是两条判据**，别混用：
  - 触屏专属的东西用 `useMobileLayout()`（辅助键条、`.hover-reveal` 常显、16px 输入框）；
  - **布局宽度**用 `useWideLayout()`（≥md 断点）：会话视图左栏显不显、FileManager 的 `mobile`
    prop（权限列 / 空白右键菜单）都按它——平板是宽屏，左栏和权限列都放得下（第十轮用户
    原话「平板不是窄屏，可以按模式显示面包屑的」）。
- 弹窗定位用 `.region-center` 定位带（`--vvh/--vvt/--header-h/--keypad-h`），不要用
  `fixed inset-0` 居中或 `100vh`（iOS 键盘弹起时会算错、被键盘盖住）。
  **例外只有快捷指令弹窗**：它按用户要求「高度 = 视口 − 应用顶栏 的 90%」，**不**减底部键条，
  所以自带一条定位带（`QuickCmdsDialog` 的 `BAND_STYLE`，与 `PANEL_HEIGHT` 共用同一个常量）。
  带与面板必须同一个算式，否则居中的 flex 会把溢出的一半顶进顶栏里。见 CONTRACT 21.3。
- 列表类操作（删除、重命名、关闭标签）失败必须给出原因（`describeError(e)` + toast），
  不能「点了没反应」。
- **点完按钮要把键盘焦点还给终端**：按钮在 Chrome 上点击后会获得焦点，焦点留在按钮上时用户
  接着敲的空格/回车会去「再按一次按钮」——点一次「+」后按三个空格就多开三个终端（每个都真的
  拨号建 SSH 会话），点「重连」后打字则完全进不了 shell。规则见 `SessionView.vue` 的
  `focusTermSoon()` 与 `runAction()`。
- 弹窗里的 `<label>` 必须用 `id`/`for` 与输入框关联（否则输入框没有可访问名，屏幕阅读器与
  自动化只能猜 placeholder）。
- 内容可能超过 90% 屏高的弹窗（编辑会话、设置）**必须分层**：面板 `flex flex-col` +
  头部 `shrink-0 border-b` + 正文 `min-h-0 flex-1 overflow-y-auto`（+ 页脚 `shrink-0 border-t`）。
  **不要**在面板上直接 `overflow-y-auto`——那样标题与关闭按钮会跟着内容滚出去，用户得先滚回
  顶部才能关掉弹窗（第七轮修过）。
- 标签（顶栏应用标签、会话内子标签）的非激活态带**常驻灰底**（`.app-tab-idle` /
  `.subtab-idle`），关闭按钮**常驻显示**（不要再给它们加 `.hover-reveal`）。`.hover-reveal`
  只留给文件管理器行内的「···」。**唯一例外**：会话只剩一个终端子标签时，那个标签不渲染关闭
  按钮（`SessionView.vue` 的 `v-if="termTabs.length > 1"`，第十二轮）——它是用户唯一在用的
  shell，误触后这个会话一个终端都不剩。顶栏应用标签不受影响（关会话标签是另一回事，且有确认）。
- 关闭按钮想要「悬停才变红」用 `.close-danger`（顶栏标签、卡片终端行）——它**默认就是红的**
  （触屏没有 hover），只有 `@media (hover: hover)` 才收成中性色：别写成 `hover:text-danger`，
  那样触屏上永远不红。
- 左栏目录树的「展开到登录用户家目录」由 `FileTree.revealHome()` 在两个时机做：新开会话
  （home 到手）与**重新展开面包屑**（leftHidden true→false）。左栏是 v-show 隐藏的，
  动手前必须 `await nextTick()`（`display:none` 下 `scrollIntoView` 无效）。
- **按 sid/uid 分片的 store 里，新建分片要「赋值之后再从 ref 上读一次」**：
  `if (!states.value[sid]) states.value[sid] = blankState(); return states.value[sid]!`
  ——`x ?? (x = ...)` 或「先赋给局部变量再 return」都会把**原始对象**交给第一个调用方，
  那个组件的 `computed`/`watch` 从此收不到任何更新（读得到新值、界面不刷新）。
  第八轮的「新打开会话不展开到家目录」就是这么来的，见 `docs/CONTRACT.md` 18.4。
- Toast 浮在**顶栏那一条里**并居中（`top: --vvt` + `height: --header-h`，
  `pointer-events-none` 保证底下的标签照常可点）。
- **移动端输入控件字号固定 16px**（`style.css` 的 `@media (max-width: 767px)`）：iOS 在
  `font-size < 16px` 的输入框上聚焦会自动放大整页且不缩回。xterm 自己的
  `.xterm-helper-textarea` 显式排除（它的字号由终端字号决定，内联样式设置）。
- 首页卡片：**子终端列表是内联在卡片下的**（`SessionTerminalsDialog` 第六轮已删除），
  点一行是「快捷打开」（切应用标签 + `terminals.setActive` + `panes.get().focus()`），
  关闭单个终端要先确认（会终止远端 shell）。卡片右上角是三个仅图标按钮，不标注认证方式，
  也不显示状态灯/时间。终端状态点的配色只有一份：`terminals.termStatusDotClass()`。
- 卡片上的**系统信息**（第十三轮，见 CONTRACT 23）：已连接的会话在子终端列表**之上**显示两行
  纯数字（CPU + 负载 / 内存 + 磁盘），5 秒刷新一次。**轮询一律走 `stores/sessionStats.ts`，
  由 `HomeView` 用 `sync(openIds, visible)` 驱动**——不要把定时器写进卡片组件（卡片会被反复
  挂载/卸载，会变成「卸载了还在问」或「重挂载多问一份」），也不要漏掉 `visible`：
  首页是 `v-show` 隐藏的，不看首页时必须停表，否则用户在看终端时后端每 5 秒跑一条远程命令。
  读不到（远端不是 Linux / 接口失败）就整块不显示，**不保留旧数字、不弹 toast**。
  四个数值按阈值染绿/黄/红（阈值与理由只在 `STATS_THRESHOLDS` 一处，见 CONTRACT 24）：
  **只染数值、标签保持中性灰**；负载的颜色必须 `load1 ÷ cpuCount`（后端给的核数，
  缺了它 8 核上的 1.5 会被当成重载），核数缺失时按 1 核算。颜色规则别写进组件模板里。
  后端的取数连接与 `r.base` 分开（`sessionRuntime.statsClient`）：`r.base` 被 `sftpMu` 占着，
  一次大文件上传会让轮询卡死；而每 5 秒新拨一条 SSH 对 NAS 是实打实的负担。
- **清空会话名**：后端 `save` 里「显式空串 = 清除、字段缺省 = 保持原值」（与凭据/备注同一套）；
  展示名统一走 `store.Session.DisplayName()` / 前端 `sessionLabel()`（空 → `user@host`）。
  `home.renameSession` **只发 `{id, name}`**，不要把本地缓存的 host/user/authType 回传——
  那会覆盖别的设备刚做的改动。
- **压缩包的输出目录在远程主机上**（`/tmp/webssh-archive`，`WEBSSH_ARCHIVE_DIR` 可换）：
  不可写时 `resolveArchiveDir` 依次退到 `<root>-<登录用户名>`、`$HOME/.webssh-archive`，
  三档都不行才报 permission。探针**只用 shell 内建**，探不出结论时按配置目录继续
  （否则「远端没有 tar」会被误报成权限问题）。
- 文件管理的**多选**是**模式**（`SftpState.multi`）而不是「一次全选」：点工具栏「多选」进入、
  点行逐个选中，做完复制/剪切/删除/压缩/导出后必须 `sftp.exitMulti(sid)` 收回来（进/出都清空
  `selected`）。别退回「全选」那种一次性批量选中。
  多选时：**任何工具栏动作都要先 `leaveMulti()`**（后退/前进/刷新/上一层/新建/上传/换视图），
  **右键菜单换成批量菜单**（行上与空白处同一份：复制/剪切/删除/导出/下载/两个压缩），
  「导出至飞牛」同名冲突一次问、整批覆盖。
- 左栏宽度用**骑在分隔线上的手柄**拖（`SessionView` 里那个 20×32 的按钮，`touch-none`）：
  不要再把它退回「直接拖一条 1px 的线」——平板（触屏）根本抓不住。
- 会话视图的**唯一判据**是 `sessionUi.activeSubOf(sid) === FILES_TAB`：桌面端「固定的文件管理
  子标签」与「左栏面包屑/目录树」**二选一**（看终端时显左栏，看文件管理时收左栏），
  移动端就是全区域子页面。**不要再引入第二个「主区域模式」状态**——第二轮那版 `mainMode`
  与这套判断互相打架，已删除。文件管理器工具栏里的「返回终端」**只是这个状态的写入口**，
  不构成第二套开关（左栏按钮条里那个「切换到文件管理」第八轮已删——`文件管理` 子标签常驻，
  不需要第二个入口）。
  第六轮又叠了一个**用户偏好**：左栏可以被手动收起（`sessionUi.leftHiddenOf`），可见性是
  `wide && !filesActive && !leftHidden` 三者的交集（`wide` = `useWideLayout()`，第十轮起
  按**视口宽度**而不是触屏判定），收起时子标签栏左侧出现「展开面包屑」。
  改之前先看 `docs/CONTRACT.md` 13.1、14.3 与 16.1。
- **快捷指令按钮在会话子标签栏的工具条上**（不在顶栏），它作用于当前激活终端；文件管理子标签在
  前台时没有激活终端，桌面工具条与移动工具行**整条隐藏**（`v-if="!filesActive"`，第四轮用户要求
  「文件管理标签显示时这些按钮不该出现」）。这几个按钮没有要保留的 DOM 状态，用 `v-if` 是安全的
  ——文件头第 3 条禁 `v-if` 只针对面板（xterm / WS / FileManager 必须保持挂载）。
  **弹窗里不再写「发送到 <终端名>」**（第十一轮删掉了整条页脚，用户原话「不会发送错的」）：
  指令发给谁，由那个按钮所在的位置就已经决定，不要再把目标名加回页脚。
  要「切回哪个终端」用 `terminals.lastActiveOf(sid)`（导航目标）；**指令发给谁**只能用
  `terminals.activeOf(sid)`（文件管理在前台时必须返回 null）。
- **切终端子标签必须走 `terminals.setActive`**（它同时写 sessionUi），不要在组件里直接
  `sessionUi.setActiveSub`：那样 `activeUid` 会停在「最后建出来的那个标签」上，
  「返回终端」就会跳到一个陌生的终端（第五轮修过的真 bug，见 CONTRACT 15.4）。
- 剪贴板的清空语义**分两层**：**剪切粘完（真的贴上至少一项）就清**，与入口无关，剪贴板栏随之
  消失——剪切是「搬走」，搬完这一次就结束，不要再去维护「把剪贴板路径重映射到新位置」那套；
  **复制**才由入口决定：右键/空白菜单的「粘贴」粘完就清，地址栏下那行常驻剪贴板里的
  「粘贴到此处」不清（只有「取消」才清，好把同一批粘到好几个目录）。改 `sftp.paste` 时要保留
  `keepClipboard` 这个开关；失败、以及一项都没贴上时**一律保留**剪贴板（那次剪切还没完成）。
- 主机密钥信任的**删除入口在会话弹窗里**（`SessionFormDialog` 底部那一行「已信任的主机密钥」+
  删除按钮，第十一轮从设置页挪过来的，见 CONTRACT 21.5）：**只在当前表单的 `host:port` 已有
  记录时才显示**——「测试连接」不写 known_hosts（只有 `/api/sessions/trust` 写），所以新增会话
  或没信任过的宿主上看不到这一行，点过「信任并继续」才会出现。**同一个判据还管着「保存」**：
  指纹没确认过（新增会话、刚删了信任、地址改成没信任过的主机）时按钮写**「仅保存」**、并在
  三个按钮**之上**单独一行放 `form_trust_first`（「无信任指纹，测试连接并信任密钥指纹后保存」，
  用户明确要求：不要和按钮挤在一行；早先一版是直接藏按钮，把「主机暂时连不上只想先存配置」的
  人卡死了，才改成换文案）；`/api/hostkeys` 读失败时**不变文案**
  （那是我们自己的接口抖动，不该让按钮莫名变成「仅保存」）。不要把它挪回设置页，也不要为了「对称」
  常显一个点了没反应的删除按钮；**删除会话时要一并 `forgetHostKey`**（`home.remove`）。
- 菜单的「点别处关闭」用 **`pointerdown` + 捕获阶段**（`FileManager` 的 `onDocPointerDown`），
  **不要**退回 `window` 上的 `click`：触屏上 tap 的 click 未必到得了 window（第五轮修的真 bug）。
  菜单项自己开的弹窗（新建文件/目录、上传）必须**显式** `hideMenu()`——pointerdown 落在菜单里
  不算「别处」。
- 「信息」走 `POST /api/fs/info`：只数**直接子项**（不要递归），软链接口径与列表一致（跟随），
  读不出子项时回 `countError` 让界面说原因，**绝不能**把 0 当成「空目录」；属主是
  `uid/gid`（SFTP attrs）+ 远端 `id -nu/-ng` 查出来的 `owner/group`（**best-effort**，
  查不到就留空、界面显示数字，不要编名字；查名字要放在 SFTP 锁外面）。
- **预览**（`PreviewDialog` + `backend/files_preview.go`，契约第 25 节）分两条路，别混：
  - **图片**走 `POST /api/fs/image`（原始字节 + 嗅探出的 `Content-Type` + `nosniff`，前端
    `fetchImageBlob` → Blob URL，关闭时 revoke），**上限 4 MiB**（`maxImageBytes`），超限回
    `too_large`、界面**不显示**预览。白名单按**内容**判、不看扩展名；前端的 `IMAGE_EXTS` 与
    后端的 `imageTypes` 是**一对**，加格式要同时改两处。**不含 SVG**——SVG 是能带脚本的 XML，
    而这个接口是唯一把远端文件以 inline 交给浏览器的入口（blob:/同源地址被当页面打开就是
    同源脚本执行），SVG 归文本预览看源码。这是唯一一条「inline 回文件内容」的通道，
    **不要**把它改成「按扩展名回」或给它加别的用途。
  - **文本**走 `POST /api/fs/read`：只认 UTF-8（剥 BOM）与 UTF-16 LE/BE（含无 BOM 的启发式），
    **GBK/GB18030 一律判非文本**（解码要 x/text，本仓库不允许引；硬猜出乱码比明说「不是文本」
    更糟）。响应里的 `encoding` 是给界面标的编码。默认**不换行 + 行号**（行号只在 ≤20000 行时
    给；换行开关是用户偏好，不随文件重置）。
- **编辑**（`FileEditor.vue` + `stores/fileEditor.ts` + `backend/files_edit.go`，契约第 26 节）：
  - 三个入口（预览弹窗的编辑按钮 / 文件管理与目录树的行菜单「编辑」）都走 `fileEditor.open()`：
    **先** `POST /api/fs/edit-load`（后端以本会话的**登录用户**身份 `O_WRONLY` 打开一次做写权限
    探测），成功之后才切到文件管理子标签并打开编辑页；失败只 toast、**不切标签页、不进编辑页**。
    没有权限时的表现必须是「拒绝修改」（`permission`），不是一句 internal。
  - 编辑页由 `FileManager` 以 `absolute inset-0` **覆盖**在文件管理页面上（文件管理器保持挂载）；
    进入与退出**都不动文件管理器的状态**（不导航、不换视图）——「退出后回到进入时的状态」
    靠的就是「从头到尾没碰它」，从目录树进来时也不会被带到被编辑文件所在的目录去。
  - 保存**按原样写回**：UTF-16 仍 UTF-16、带 BOM 仍带 BOM、CRLF 仍 CRLF（`encoding`/`bom`/`eol`
    由 edit-load 下发、原样带回，认不出的编码**必须报错**，不许当 UTF-8 写下去）。写入是
    **原地截断再写**（保住 inode/属主/mode/ACL），失败时尽力把原内容写回去。
  - 前端的「能不能编辑」判据只有 `components/files/fileKind.ts` 一处（文本类），图片与目录没有
    「编辑」项；`IMAGE_EXTS` 仍与后端 `imageTypes` 成对。
- **左栏目录树也列文件**（契约 26.4）：每个目录只列**前 `TREE_FILE_LIMIT`（20）个**文件
  （按名字），超过就在该目录末尾加一行「···更多文件」（点击 = 导航到该目录 + 切到文件管理
  子标签）；整棵树没有文件总数上限。`stores/sftp.ts` 里 `treeChildren` 与 `treeFileTotal`
  必须一起维护（三个写入点都过 `treeRows()`），只改一处会出现「更多文件」那行不出现/不消失。
  树里点**文件** = 预览（与文件管理器点一行一致），点**目录**仍然只更新当前目录并展开。
- 文件管理的**权限显示模式**通过点列表表头「权限」列切换（没有独立按钮），与表头的三态排序
  互不干扰；列表空白处右键有独立菜单（新建文件 / 新建文件夹 / 上传 / 信息 / 打开终端 / 复制路径 / 刷新；
  「粘贴」只在剪贴板**有本会话内容**时出现——为空不显示、来自别的会话才置灰说明原因）。
  网格视图容器上的 `content-start` 不能删：网格默认会拉伸自动行，只有一行条目时那一行会被拉成
  整屏高，悬停高亮就变成贯穿上下的一整列（第四轮修的真 bug，见 `docs/CONTRACT.md` 14.5）。
- 会话改名必须走 `home.renameSession`（同时更新配置与顶栏标签，双向同步）；
  **不要**只改标签标题——那样配置里的名字与界面会长期不一致。
- 上传弹窗三种来源（本地文件 / 本地目录 / 飞牛文件）都要有进度与取消；
  飞牛宿主不可用时给明确提示，而不是让用户点了报错。
- 文案全部走 `t()`；缺 key 时 `t()` 会把 **key 本身**当成文案显示（界面上出现 `files_copy`
  这种小写串），而 `npm run check:i18n` 现在是**双向**检查：字典之间一一对应 **+** 代码里静态
  引用的 key 必须存在。新增文案要同时改两套字典；删文案前先确认没有代码引用。
- 图标用内联 SVG（`fill="none" stroke="currentColor" stroke-width="1.5"`，24×24），不引图标库。

## 6. 协议常量（改一处必须改两处）

| 方向 | 帧 | 含义 |
| --- | --- | --- |
| S→C | `\x1b]tid;<id>\x07` | 新建终端时下发终端 id |
| S→C | `\x1b]ready\x07` | 历史回放结束，进入实时流 |
| S→C | `\x1b]exit\x07` | 远程 shell 退出 |
| S→C | `\x1b]detached\x07` | 本端被其他设备顶掉（配合 close 4001） |
| S→C | `\x1b]gone\x07` | 该终端在后端不存在（附在错误文本帧末尾） |
| C→S | `\x1b]resize;<cols>;<rows>\x07` | 调整 PTY 尺寸 |
| C→S | `\x1b]ping\x07` | 心跳（不写 PTY） |

后端 `protocol.go` 的常量与前端 `serverapi/index.ts` 的常量是**一对**，改名/改值必须同步，
否则表现是「终端连上但完全没有输出」或「被顶掉不提示」。

**终端数据（含上面所有控制帧）一律用二进制帧（`opBin`）发送，不要用文本帧。** 终端输出是
任意字节流，而文本帧的载荷按 RFC 6455 §8.1 必须是合法 UTF-8——只要有一个非法字节（回放按
32 KiB、实时按 8 KiB 切帧，边界会落在多字节字符中间；或者远端本来就输出非 UTF-8 字节），
浏览器就会判定协议错误并**直接断开连接**（实测 Chromium：onerror + close 1006，一帧数据都
收不到），表现为「刷新后终端回放到同一位置就断，点几次重连都一样」。前端用**流式**
`TextDecoder`（`{stream:true}`）解码，跨帧的半个字符会拼回来。改这一处要同时看：
`backend/protocol.go` 的 `termDataOp`、`backend/terminal.go` 的三处发帧、`TerminalPane` 的
`decoder`、以及测试客户端的 `readText`（它断言了 opcode，改回文本帧会立刻红）。

握手同样要校验：`Sec-WebSocket-Key` 必须是 16 字节 base64；**不回显** `Sec-WebSocket-Protocol`
（没有子协议，回显等于把客户端输入拼进协议响应）；客户端帧必须掩码；RSV1-3 置位或保留
opcode 一律失败（`internal/ws` 的 `ReadClientFrame`）。

错误码同样是契约：后端返回裸码（`duplicate`、`hostkey_unknown`…），前端映射成 i18n 的
`err_<code>`；新增错误码要在两套字典里都加文案。

## 7. 测试与验收

- 完整门禁是 **`make test check`**（`make check` = 前端 `vue-tsc` + `check:i18n`）。
  `make test` 只跑 Go：构建机上不一定有 node，所以两者刻意不绑在一起。
- ⚠️ 无 PTY 的环境（多数 CI 容器）会**静默跳过** 11 个 PTY 用例（`requirePTY` → `t.Skip`），
  包括 PTY 收发 / 单挂载点顶号 / 历史回放。看到「全绿」时要顺带看一眼 skip 数。
- 后端：`make test` 必须全绿。集成测试用 `internal/testssh` 起进程内 SSH+SFTP 服务器，
  **不允许**依赖外部 sshd（构建机上不一定有）。覆盖：密码/密钥认证、三种连接测试结局、
  PTY 收发与 resize、单挂载点顶号、历史回放、文件全套操作（含相对路径目录上传）、
  hostkey TOFU、凭据加解密往返、会话身份去重。
- 前端：`make typecheck` 无错误。**不要**只跑 `vite build`（Vite 不做类型检查，
  类型错了照样出产物）。
- 端到端（至少覆盖）：建会话 → 测试连接（首次要确认指纹）→ 打开 → 自动出现终端标签 →
  命令有回显 → 新建第二个终端标签 → 文件管理浏览/上传/下载/新建/重命名/删除 →
  全局与专属快捷指令 → 关闭会话标签（终端与 SFTP 全断）→ 刷新页面会话仍在并能恢复。
- 修 bug 时优先补一条能复现该 bug 的测试/说明；没有测试的修复要在提交信息里写清验证方式。
- `internal/testssh` 用真实 pty（`github.com/creack/pty`，**仅测试依赖**）；生产代码不得 import 它。
  `make test` 首次跑要能访问模块代理，之后走本地 `.gomodcache/`。
- 端到端校验脚本（第二轮的功能都在里面，含病态路径名与「命令注入未执行」的断言）：
  `.tmp/e2e2/verify.py`（`python3 verify.py <base_url> <sftp_root> <share_root>`，**58 项断言**，
  ⚠️ `.tmp/` 已被 gitignore：**这个脚本不在仓库里**，clone 出来是拿不到的，别把它当成
  可依赖的资产；它读的是 WebSocket 帧，认 `opText` **也认 `opBin`**（终端数据现在是二进制帧），
  含 tar.gz 与 zip 压缩、多父目录追加、非法 format、病态路径名与「命令注入未执行」）；
  它需要 `TRIM_DATA_SHARE_PATHS` 指向一个可写目录（否则导出一条会按预期报 `no_share_path`）。
- 端到端手测：`go run ./internal/testssh/cmd/testsshd -addr 127.0.0.1:2222 -user demo -password demo -root <可写目录>`
  + `socat TCP-LISTEN:18080,fork UNIX-CONNECT:<WEBSSH_ADMIN_SOCK>`（浏览器访问
  `http://127.0.0.1:18080<WEBSSH_ADMIN_BASEURL>`）。验证清单见 `docs/CONTRACT.md` 第 10.3 节。

## 8. 代码风格

- 注释、日志、错误文案：中文；标识符：英文。
- 注释解释**为什么**，不解释「是什么」；踩过的坑写清症状与原因（参考 `terminal`/`fpkstudio`
  的注释密度——那是本仓库的基线，不要简化成一行）。
- 不为「以后可能用到」写抽象；不留死代码；不留 TODO 而不写清条件。
- 单文件过大时按职责拆分（后端一个文件一个关注点；前端一个组件一个职责）。

## 9. 明确禁止

- 把 baseurl / 主机名 / 端口烧进前端产物或后端二进制。
- 为「方便」监听 TCP 端口（本应用只有 unix socket 一个入口）。
- 静默失败：吞掉错误、把失败当成功、用空值掩盖异常（解密失败、ACL 判定失败、上传中断……）。
- 用人类可读文本判定协议状态（一律用控制帧/错误码）。
- 在日志里打印明文密码、私钥、token。
- 跳过类型检查或测试就说「已完成」。
