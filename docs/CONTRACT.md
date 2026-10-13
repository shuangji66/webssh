# webssh 实施契约（前后端接口 / 数据模型 / 分工边界）

> 本文件是**并行开发的唯一接口定义**。任何跨模块的行为都以本文件为准；实现与本文不一致时，
> 要么改实现，要么先改本文并知会所有人。修改冻结文件（见第 7 节）前必须先确认。
>
> 项目：`webssh` —— 飞牛 fnOS 上的 **web 终端**：SSH 直连远程主机（x/crypto/ssh）+ SFTP 文件
> 管理（pkg/sftp），前端 Vue 3 + Pinia + Tailwind 4 + xterm 6，产物是内嵌前端的单文件二进制。
>
> 参考实现（**只读参考，不要修改**）：
> - `../storage/workspace/terminal`（另一 workspace）：终端会话管理、多设备接管、快捷指令、
>   移动端适配、字体切换、单挂载点语义、`bootID` 恢复机制、构建方式（Makefile）。
> - `../fpkstudio`：标签页 + 面包屑 + 文件管理双模式、SFTP/文件操作 API 的形态、飞牛开放 API
>   调用方式（`pickUserFile` / `checkUserACL`）、前端资源缓存头与 baseurl 注入。
> - `/vol1/@appshare/fpkstudio/projects/WebSSH`：fpk 脚手架（已复制进本仓库 `fpk/`）。

---

## 1. 运行形态

- 唯一访问入口是 **unix socket**（`WEBSSH_ADMIN_SOCK`，默认 `$TMPDIR/webssh/app.sock`），
  **不监听任何 TCP 端口**。nginx / 飞牛统一网关按 baseurl 前缀（`WEBSSH_ADMIN_BASEURL`，
  例如 `/app/WebSSH`）把 HTTP 与 WebSocket 反代到该 socket。
- baseurl 构建期未知：后端在响应 `index.html` 时注入 `<base href="<baseurl>/">` 并把
  `./assets/` 改写成 `<baseurl>/assets/`；前端一律基于 `document.baseURI` 解析 API/WS 路径
  （`src/serverapi/index.ts` 的 `runtimeBase()` / `wsUrl()`）。
- **静态资源必须带缓存头**：
  - `*.html` → `no-cache, must-revalidate`（每次响应都会注入 base href，缓存住会把旧页面钉死）
  - `assets/*`（Vite 产物，文件名带内容哈希）→ `public, max-age=31536000, immutable`
  - 其余根级静态文件 → `public, max-age=86400`
- 环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `WEBSSH_ADMIN_SOCK` | `$TMPDIR/webssh/app.sock` | unix socket 路径 |
| `WEBSSH_ADMIN_BASEURL` | 空（根路径） | 前端资源 baseurl 前缀 |
| `WEBSSH_DATA_DIR` | `$TMPDIR/webssh/data` | 数据目录（见第 3 节） |
| `WEBSSH_DIAL_TIMEOUT` | `15s` | SSH 建连超时 |
| `WEBSSH_TRIM_SOCKET` | `/var/run/trim_open_gateway_apiscope.socket` | 飞牛开放 API 网关 |
| `WEBSSH_TRIM_APP` | `WebSSH` | 调开放 API 时带的 appName |

- 进程启动顺序（照 terminal 的教训）：准备数据目录 → 检查 socket 是否已被占用（是则退出）
  → **先绑定 socket 成功再进信号等待**（绑定失败即退出，不要「活着但没有 socket」）→ 收到
  SIGTERM/SIGINT/SIGQUIT 后：先 `srv.Close()` 停入口 → 终止所有终端与 SFTP → 删除 socket
  **但保留数据目录**（会话配置必须活过重启）。
- `bootID`：每次进程启动生成一个随机 id，经 `/api/info` 的 `bootId` 下发。前端据此判断
  「后端换进程了」——换了就说明所有内存态会话都没了，前端要清空会话标签（配置仍在）。

---

## 2. 后端文件布局（`backend/`，Go 1.27，`package main`）

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：环境解析 → 目录准备 → socket 绑定 → 信号等待 → 优雅退出；`appVersion`（`-ldflags` 注入）；`bootID` |
| `config.go` | `RuntimeEnv`（全部来自环境变量）+ 默认值 |
| `logging.go` | 极简日志：只记异常/失败（不记例行启动停止） |
| `store.go` | 数据目录布局、原子 JSON 读写（tmp + rename）、会话配置 CRUD |
| `crypto.go` | `master.key` 管理与 AES-256-GCM 字段加解密（密码 / 私钥 / 口令） |
| `hostkeys.go` | known_hosts 的 TOFU 记录（读写 + 指纹计算 + 匹配判断） |
| `sshclient.go` | SSH 拨号：认证方式组装、主机密钥回调、超时、`ssh.Client` 生命周期 |
| `sessions.go` | `SessionRuntime`（打开的会话）：终端 + SFTP 的容器、打开/关闭/列表/回收 |
| `terminal.go` | 单个终端：`ssh.Session` + PTY + 历史文件 + 单挂载点 attach/detach/resize |
| `ws.go` | WebSocket 帧编解码（照抄 terminal 的实现，含上限与关闭码） |
| `admin.go` | mux、路由、JSON 助手、错误码映射、SPA 与缓存头、base href 注入 |
| `files.go` | SFTP 文件操作 API（list/mkdir/create/rename/remove/upload/upload-from-nas/download/read） |
| `quickcmds.go` | 全局 + 会话专属快捷指令的持久化与 API |
| `trimapi.go` | 飞牛开放 API 客户端（`trim.file.checkUserACL`） |
| `testssh/` | **仅测试用**：进程内 SSH + SFTP 服务器（密码/密钥认证、PTY、sftp 子系统），供集成测试与本地联调 |
| `*_test.go` | 单元/集成测试 |

约束：

- **不要引入除 `golang.org/x/crypto`、`github.com/pkg/sftp` 之外的第三方依赖**（testify 只允许出现在测试里且需 `go mod tidy` 认可）。
- 单文件大小/请求体/帧大小上限必须有（进程承载全部会话，一个客户端就能打爆内存）：
  上传整体上限 1 GiB（`http.MaxBytesReader`）、JSON 体 1 MiB、WS 单帧 1 MiB / 单消息 4 MiB、
  `resize` 行列 ≤ 1000、历史读取上限 4 MiB。
- 所有远程调用都要带超时或上下文取消（用户关页面/点取消时 `r.Context()` 会结束）。

---

## 3. 数据目录（`WEBSSH_DATA_DIR`）

```
data/
├── master.key                  0600，32 字节随机；AES-256-GCM 主密钥（首次使用时生成）
├── known_hosts.json            已信任的主机密钥（TOFU）
├── sessions/<sid>.json          远程会话配置（凭据字段是密文）
├── history/<sid>/<tid>.log      终端会话历史（会话存活期间；会话关闭时删除整目录）
├── history/<sid>/meta.json      会话级元数据（终端标题等，可选）
└── quickcmds/
    ├── global.json              全局快捷指令
    └── <sid>.json               会话专属快捷指令
```

- **凭据加密**：`password` / `privateKey` / `passphrase` 三个字段以
  `{"v":1,"ct":"<base64(nonce||ciphertext)>"}` 落盘；AAD 绑定 `webssh:v1:<field>:<sid>`，
  避免密文被搬到别的会话/字段后仍能解出。读取时解密失败**必须报错**（不能静默当成空密码，
  否则表现为「密码莫名其妙失效」）。
- **威胁模型要诚实写进 README/注释**：这层加密防的是「拿到磁盘快照/备份的人直接读到明文
  密码」，不防「已经能以同权限读取数据目录的攻击者」（master.key 就在旁边）。设置里可选
  用环境变量 `WEBSSH_MASTER_KEY`（32 字节 base64/hex）外部提供主密钥，避免密钥与密文同放。
- `known_hosts.json` 结构：
  ```json
  { "version": 1,
    "hosts": { "10.0.0.1:22": { "keyType": "ssh-ed25519", "fingerprint": "SHA256:...",
                               "publicKey": "<base64 wire format>", "addedAt": "2026-01-01T00:00:00Z" } } }
  ```
  匹配以 `host:port` 为键；**不匹配就报错，绝不覆盖**（可能是中间人）。
- 会话配置文件示例（`sessions/<sid>.json`）：
  ```json
  { "version": 1,
    "id": "1f0c…", "name": "nas", "host": "10.0.0.1", "port": 22, "user": "root",
    "authType": "password",
    "password": { "v": 1, "ct": "…" },
    "privateKey": null, "passphrase": null,
    "keyFingerprint": "",
    "note": "",
    "createdAt": "…", "updatedAt": "…", "lastUsedAt": "…" }
  ```
  会话身份 = `user + "@" + host + ":" + port`（端口默认 22 参与比较）。**同一身份只允许一个
  会话**：`/api/sessions/save` 撞身份时返回 `duplicate`（HTTP 409），前端提示「已存在」。
- 写盘一律 tmp + rename；并发用一把互斥锁串行化。

---

## 4. HTTP API

### 4.1 统一响应约定

- 成功：`{"ok": true, ...}`。
- 失败：HTTP 4xx/5xx + `{"ok": false, "code": "<bare-code>", "error": "<人类可读，含技术细节>"}`。
- `code` 是**裸码**（不带 `err_` 前缀）；前端把它映射成 i18n 的 `err_<code>`，映射不到就
  原样显示 `error`。稳定码：

  | code | 含义 |
  | --- | --- |
  | `invalid` | 参数不合法（缺字段、路径非法、端口越界） |
  | `not_found` | 对象不存在（会话/终端/路径） |
  | `conflict` | 状态冲突（如会话未打开） |
  | `duplicate` | 会话身份重复 |
  | `permission` | 权限不足（含飞牛 ACL 判定为不可读） |
  | `too_large` | 超过大小上限 |
  | `unavailable` | 依赖不可用（如不在飞牛宿主里） |
  | `internal` | 服务端内部错误 |
  | `no_credential` / `key_parse` / `dns` / `dial_timeout` / `dial_refused` / `dial` / `handshake` / `hostkey_unknown` / `hostkey_mismatch` / `auth_failed` / `auth_unsupported` / `shell_failed` / `sftp_unavailable` | 连接类错误（主要用于连接测试与打开会话） |

- **带参数的接口一律 `POST` + JSON body**（查询串在网关/反代链路上不保证原样送达）。
  仅无参数读取用 `GET`。前端客户端已经按这个约定写好（`src/serverapi/index.ts`），
  路径与字段名以此为准。

### 4.2 端点清单

| 方法 | 路径 | 请求体 | 响应 |
| --- | --- | --- | --- |
| GET | `/api/info` | — | `{ok, runtime}` |
| GET | `/api/sessions` | — | `{ok, sessions:[SessionView], open:[SessionState]}` |
| POST | `/api/sessions/save` | `SessionInput` | `{ok, session: SessionView}` |
| POST | `/api/sessions/delete` | `{id}` | `{ok}` |
| POST | `/api/sessions/test` | `SessionInput` | `{ok, result: TestOutcome}` |
| POST | `/api/sessions/trust` | `{host, port, fingerprint}` | `{ok}` |
| POST | `/api/sessions/open` | `{id}` | `{ok, state: SessionState}` |
| POST | `/api/sessions/close` | `{id}` | `{ok}` |
| GET | `/api/hostkeys` | — | `{ok, hosts:[HostKeyRecord]}` |
| POST | `/api/hostkeys/forget` | `{host, port}` | `{ok}` |
| POST | `/api/terminals/history` | `{sid, tid}` | `{ok, content, size}` |
| POST | `/api/terminals/clear` | `{sid, tid}` | `{ok}` |
| POST | `/api/terminals/close` | `{sid, tid}` | `{ok}` |
| POST | `/api/terminals/rename` | `{sid, tid, title}` | `{ok}` |
| GET | `/api/terminal/ws?sid=&tid=` | WebSocket | 见 4.4 |
| POST | `/api/fs/list` | `{sid, path}` | `{ok, path, home, entries:[FsEntry]}` |
| POST | `/api/fs/mkdir` | `{sid, path}` | `{ok}` |
| POST | `/api/fs/create` | `{sid, path}` | `{ok}`（空文件；已存在则 `duplicate`） |
| POST | `/api/fs/rename` | `{sid, path, to}` | `{ok}`（目标已存在则 `duplicate`） |
| POST | `/api/fs/remove` | `{sid, path, dir}` | `{ok}`（dir 必须与真实类型一致，防止误删整棵树） |
| POST | `/api/fs/upload` | multipart：`sid`、`path`(目标目录)、`overwrite`、`files[]` | `{ok, files:[{path,size}], skipped}` |
| POST | `/api/fs/upload-from-nas` | `{sid, path, source, overwrite}` | `{ok, files, skipped}` |
| POST | `/api/fs/download` | `{sid, path}` | 文件流（`Content-Disposition`；目录打 zip） |
| POST | `/api/fs/read` | `{sid, path}` | `{ok, size, text, tooLarge, content}` |
| POST | `/api/quickcmds/list` | `{sid}`（空 = 全局） | `{ok, scope, commands:[QuickCmd]}` |
| POST | `/api/quickcmds/save` | `{sid, commands:[QuickCmd]}` | `{ok, scope, commands}` |
| GET | `/api/trim/status` | — | `{ok, trim:{available, message, app}}` |
| POST | `/api/trim/acl` | `{paths:[...]}` | `{ok, list:[AclEntry]}` |

任何未命中的路径回退到 `index.html`（SPA 前端路由），但 `/api/*` 未命中要回 404 JSON。

### 4.3 数据结构（与 `src/serverapi/index.ts` 的类型逐字对应）

```jsonc
SessionView  { id, name, host, port, user, authType: "password"|"key",
               hasPassword: bool, hasPrivateKey: bool, keyFingerprint?: string,
               note, createdAt, updatedAt, lastUsedAt? }          // 时间一律 RFC3339
SessionInput { id?, name?, host, port?, user, authType,
               password?, privateKey?, passphrase?, note? }        // 密钥字段缺省 = 不修改
TestOutcome  { success, code, detail?, stage?, fingerprint?, keyType?,
               trusted?, mismatch?, banner?, latencyMs? }
TerminalInfo { id, title, createdAt, exited }
SessionState { id, open, openedAt?, terminals: [TerminalInfo], sftpOpen }
FsEntry      { name, path, dir, size, mode, mtime, symlink?, broken? }  // path 是绝对路径
QuickCmd     { id, name, content, auto }
HostKeyRecord{ host, port, keyType, fingerprint, addedAt }
AclEntry     { path, readable, writable, deletable }
RuntimeInfo  { version, bootId, dataDir, adminSock, adminBaseURL, hostname,
               trim: {available, message, app}, gateway: {uid, username, isAdmin, fromGateway} }
```

行为要求：

- `SessionInput` 的 `password/privateKey/passphrase` 为 `null`/缺省时**保留已存值**；给空字符串
  表示「清除该字段」（表单里「留空 = 不修改」由前端保证：不修改时字段根本不发）。
- `/api/sessions/open` 幂等：已打开就返回现有 `SessionState`（不要重建终端）。
- `/api/fs/list` 的 `path` 为空时返回**家目录**（`sftp.RealPath(".")`）的内容，并把解析出的
  家目录放在 `home` 里（前端面包屑的「家目录」入口用它）。
- `/api/fs/upload` 的 `files[]` 的 filename 可能是**相对路径**（目录上传，例如 `a/b.txt`）：
  必须按 `/` 切成段逐段校验（拒绝空段、`.`、`..`、绝对路径），在目标目录下按相对路径建目录，
  不能直接把 `path.Base(filename)` 当名字（否则目录上传会拍平）。
- `/api/fs/upload-from-nas`：`source` 是本机（NAS）上的绝对路径，来自前端用
  `@trimjs/web-app` 的 `pickUserFile` 选中的**已授权**路径。服务端在读之前应尽量用
  `trim.file.checkUserACL` 判读权限（飞牛不可用时跳过并在日志里记一句），然后本地读取、
  经 SFTP 写入 `path` 目录下（文件名取 `source` 的 basename）。
- `/api/fs/download` 目录打包 zip；**先打进临时文件再发正文**（响应头一旦发出就无法再报错，
  半截 zip 会让用户以为下载成功）。
- `/api/trim/*` 只能服务端调（token 在 `TRIM_API_TOKEN` 环境变量里）；飞牛不可用时
  `/api/trim/status` 返回 `available:false` + 原因，`/api/trim/acl` 返回 `unavailable` 错误。

### 4.4 终端 WebSocket 协议

路径 `/api/terminal/ws?sid=<会话 id>&tid=<终端 id 或省略>`。握手照 terminal 的自实现
（`Sec-WebSocket-Accept`、单端帧编解码、掩码解包、上限、`opText/opBin/opClose/opPing/opPong`）。

- `tid` 为空 → **新建终端**：先发文本帧 `\x1b]tid;<id>\x07`（前端据此回填 id），再 `attach`。
- `tid` 非空 → 挂载已有终端：回放历史（分帧，单帧 ≤ 32 KiB，最多 4 MiB）→ `\x1b]ready\x07`
  → 进入实时流。
- 终端不存在 → 文本帧 `\r\n\x1b[31m…\x1b[0m\r\n` + `\x1b]gone\x07` 控制帧，然后关闭。
  ⚠️ 前端只认 `\x1b]gone\x07`，**不能**用人类可读文本判定（终端里 `echo` 出同样文本会误判）。
- 客户端 → 服务端：普通字节写入远程 PTY；`\x1b]resize;<cols>;<rows>\x07` → `Session.WindowChange`；
  `\x1b]ping\x07` → 心跳，不写 PTY。
- **单挂载点**：一个终端同时只允许一个操作端。新挂载成功后给旧连接发 `\x1b]detached\x07`
  再以 close code **4001** 关闭；旧连接的残留输入在服务端丢弃（`not_session_owner`）。
- shell 退出：写 `\r\n\x1b[31m[process exited]\x1b[0m\r\n` + `\x1b]exit\x07`，并保留历史
  `10 分钟`（`exitedGrace`）后回收（回收后重新挂载会拿到 `gone`）。
- 浏览器断开**只解挂载不杀会话**：终端继续运行、输出继续写历史文件，刷新后可重新挂载回放。

---

## 5. 后端行为细节（踩过的坑）

1. **历史文件必须以 `O_RDWR|O_CREATE|O_APPEND` 打开**（只写打开会让回放/`history` 读失败）。
2. 回放历史 + 实时广播必须在**同一把锁**内完成（不重不漏）；向连接的写要带写超时
   （5s），超时即解挂载，不能让一个失联连接把锁占死。
3. `ssh.Session` 的 PTY：`RequestPty("xterm-256color", rows, cols, modes)`；stdout/stderr
   **接到同一个 pipe**（带 PTY 时 stderr 通常已合并，但扩展数据仍可能走 stderr 通道）；
   stdin 用 `StdinPipe`，或把 `session.Stdin` 设为 `io.PipeReader`（写入端由 WS 侧持有）。
   **必须**把 `TERM` 设成 `xterm-256color`，并在 resize 时调 `WindowChange(rows, cols)`。
4. 关闭顺序：先关 `ssh.Session`（结束 shell）→ 关 `ssh.Client` → 关历史文件（并删除）。
   关闭过程中的 `EOF`/`ErrClosed` 一律视为**正常终止**，不要当错误上抛（terminal 的 `close()`
   就在这上面栽过：把「Kill 导致 Wait 返回非零」当失败，DELETE 对运行中的会话回 404）。
5. SFTP 连接每会话一个、**懒加载**、失败自动重连一次；所有文件操作串行化（`pkg/sftp` 的
   客户端本身支持并发，但我们的会话状态简单些更可控）。
6. 远程路径一律走 `path.Clean` 并拒绝 `..` 逃逸（SFTP 服务端会拦，但我们要给出清晰的中文错误）。
7. 数据目录里的文件模式位在飞牛存储卷上可能不可靠：创建后显式 `chmod`（`master.key` 0600）。
8. 测试：`testssh` 提供进程内 SSH/SFTP 服务器，集成测试必须覆盖：密码/密钥认证、连接测试
   的三种结局（成功 / 首次指纹需信任 / 认证失败）、PTY 收发与 resize、单挂载点接管、
   历史回放、文件 list/upload（含相对路径目录上传）/download/rename/remove、
   凭据加解密往返、身份去重。

---

## 6. 前端部件与职责

```
frontend/src/
├── main.ts                     [冻结] 入口
├── App.vue                     [D]    壳：顶栏 + 首页/会话视图 + 全局弹窗
├── style.css                   [冻结] 设计令牌与可复用类（见文末）
├── serverapi/index.ts          [冻结] API 客户端 + WS 控制帧 + 上传下载助手 + describeError
├── i18n/{index,zh,en}.ts       [冻结] 文案（**不要新增/改名 key，缺了报给 Lead**）
├── utils/{format,scroll}.ts    [冻结] 时间/体积/路径 + 横向滚动
├── composables/
│   ├── useTheme.ts             [冻结] 主题（light/dark/system）
│   ├── useViewportHeight.ts    [冻结] 虚拟键盘 / --vvh / --vvt / --header-h
│   ├── useMobileLayout.ts      [冻结] 移动端（触屏）判定 + useWideLayout
│   └── useKeypadHeight.ts      [B]    --keypad-h 上报
│   └── useKeypadPage.ts        [B]    键条分页（模块级共享）
├── stores/
│   ├── sessionUi.ts            [冻结] 会话内激活子标签 / 左栏显隐
│   ├── appTabs.ts              [D]    应用级标签（首页 + 会话）
│   ├── home.ts                 [D]    远程会话配置 + 实时状态 + 连接测试
│   ├── quickCmds.ts            [D]    全局 / 专属快捷指令
│   ├── settings.ts             [D]    字号 / 字体（导出 FONT_FAMILY_KEY）
│   ├── toast.ts                [D]    轻提示
│   ├── terminals.ts            [B]    会话内终端子标签
│   ├── paneControls.ts         [B]    终端命令出口（按 sid+uid 登记）
│   └── sftp.ts                 [C]    SFTP 文件浏览状态
├── trim/sdk.ts                 [C]    @trimjs/web-app 薄封装（选飞牛文件）
├── components/
│   ├── AppTabBar.vue           [D]    顶栏标签条（首页固定 + 会话标签）
│   ├── Toast.vue               [D]
│   ├── ConfirmDialog.vue       [D]    visible/title/message/confirmText/cancelText → confirm|cancel
│   ├── PromptDialog.vue        [D]    visible/title/label/placeholder/initialValue → confirm(value)|cancel
│   ├── SettingsDialog.vue      [D]    主题/语言/字体/字号/已信任主机/关于
│   ├── QuickCmdsDialog.vue     [D]    全局 + 专属（会话）两个分组
│   ├── QuickCmdEditDialog.vue  [D]    新增/编辑单条
│   ├── home/SessionCard.vue    [D]    大号会话卡片
│   ├── home/SessionFormDialog.vue [D] 新增/编辑 + 测试连接
│   ├── terminal/TerminalPane.vue  [B] 一个终端标签（xterm + WS）
│   ├── terminal/KeypadBar.vue     [B] 移动端辅助键条
│   ├── files/FileManager.vue      [C] 文件管理面板（工具栏 + 面包屑 + 列表/网格 + 菜单）
│   ├── files/FileTree.vue         [C] 左栏文件树
│   ├── files/BreadcrumbBar.vue    [C] 左栏面包屑（含「完整文件管理」入口）
│   ├── files/UploadDialog.vue     [C] 上传弹窗（本地文件 / 本地目录 / 飞牛文件）
│   └── files/PreviewDialog.vue    [C] 文本预览（可选，用 /api/fs/read）
└── views/
    ├── HomeView.vue            [D]    会话卡片网格（只平铺）
    └── SessionView.vue         [D]    左栏（面包屑 + 文件树）+ 子标签栏 + 面板
```

### 6.1 store 公开接口（跨模块调用面，必须照此实现）

```ts
// [D] stores/appTabs.ts
export const HOME_TAB_ID = 'home'
useAppTabsStore(): {
  tabs: AppTab[]; activeId: string
  openHome(): void
  openSession(sid: string, title?: string): void
  activate(id: string): void
  close(id: string): void                 // 首页不可关闭
  setTitle(sid: string, title: string): void
  sessionTabs: ComputedRef<AppTab[]>      // kind === 'session'
}

// [D] stores/home.ts
useHomeStore(): {
  sessions: SessionView[]; openStates: Record<string, SessionState>
  loading: boolean; loaded: boolean; error: string
  bootId: string
  load(force?: boolean): Promise<void>
  stateOf(id: string): SessionState | undefined
  isOpen(id: string): boolean
  save(input: SessionInput): Promise<SessionView>
  remove(id: string): Promise<void>
  test(input: SessionInput): Promise<TestOutcome>
  trust(host: string, port: number, fingerprint: string): Promise<void>
  openSession(id: string): Promise<SessionState>
  closeSession(id: string): Promise<void>
  loadInfo(): Promise<void>
  watchBackendRestart(): void             // WS 掉线后探测后端是否换了进程
  restore(): Promise<string[]>            // 启动时：把后端仍在运行的会话恢复成应用标签，返回 sid 列表
}

// [B] stores/terminals.ts
export type TermStatus = 'connecting' | 'open' | 'exited' | 'error' | 'detached'
export interface TermTab { uid: string; tid: string | null; title: string;
                           status: TermStatus; restoring: boolean; createdAt: string }
useTerminalsStore(): {
  tabsOf(sid: string): TermTab[]
  activeOf(sid: string): TermTab | null
  countOf(sid: string): number
  ensure(sid: string, state: SessionState): void   // 从后端实时状态恢复（幂等）
  add(sid: string): TermTab
  adoptTid(sid: string, uid: string, tid: string, createdAt?: string): void
  setStatus(sid: string, uid: string, status: TermStatus): void
  setTitle(sid: string, uid: string, title: string): void
  titleOf(sid: string, tab: TermTab): string
  closeTab(sid: string, uid: string): void
  forget(sid: string): void
}

// [B] stores/paneControls.ts
usePaneControlsStore(): {
  register(sid: string, uid: string, ctl: PaneControls): void
  unregister(sid: string, uid: string): void
  get(sid: string, uid: string | null | undefined): PaneControls | undefined
}
interface PaneControls { paste(): void; clear(): void; reconnect(): void; search(): void;
                         focus(): void; send(data: string): boolean; connected: boolean }

// [C] stores/sftp.ts
export interface SftpState { dir: string; home: string; entries: FsEntry[]; loading: boolean;
                             error: string; back: string[]; forward: string[]; view: 'list'|'grid';
                             treeChildren: Record<string, FsEntry[]>; expanded: string[];
                             treeLoading: Record<string, boolean>; selected: string[] }
useSftpStore(): {
  stateOf(sid: string): SftpState
  ensure(sid: string): Promise<void>            // 首次进入：定位家目录并加载
  navigate(sid: string, path: string): Promise<void>   // 记录前进/后退历史
  back(sid: string): Promise<void>; forward(sid: string): Promise<void>
  refresh(sid: string): Promise<void>
  loadDir(sid: string, path: string, force?: boolean): Promise<void>
  setView(sid: string, v: 'list'|'grid'): void
  toggleExpand(sid: string, path: string): Promise<void>
  createFile(sid: string, dir: string, name: string): Promise<void>
  createDir(sid: string, dir: string, name: string): Promise<void>
  rename(sid: string, path: string, to: string): Promise<void>
  remove(sid: string, path: string, dir: boolean): Promise<void>
  uploadLocal(sid: string, dir: string, entries: UploadEntry[], overwrite: boolean,
              onProgress?: (p: number) => void, signal?: AbortSignal): Promise<UploadResult>
  uploadFromNas(sid: string, dir: string, source: string, overwrite: boolean): Promise<UploadResult>
  download(sid: string, path: string): Promise<void>
  select(sid: string, paths: string[]): void
  clearSelection(sid: string): void
  forget(sid: string): void
}

// [D] stores/quickCmds.ts
useQuickCmdsStore(): {
  global: QuickCmd[]; session: Record<string, QuickCmd[]>
  ensureGlobal(): Promise<void>; ensureSession(sid: string): Promise<void>
  add(sid: string | null, payload: {name, content, auto}): Promise<void>
  update(sid: string | null, cmd: QuickCmd, payload: {name, content, auto}): Promise<void>
  remove(sid: string | null, cmd: QuickCmd): Promise<void>
  reorder(sid: string | null, orderedIds: string[]): Promise<void>
}

// [D] stores/settings.ts
export const FONT_FAMILY_KEY: string   // main.ts 用到
useSettingsStore(): { fontSize: number; terminalFont: 'maple'|'system';
                      setFontSize(n: number): void; setTerminalFont(v: 'maple'|'system'): void }

// [D] stores/toast.ts
useToastStore(): { toasts: {id:number;msg:string;type:'success'|'error'|'info'}[];
                   show(msg: string, type?: 'success'|'error'|'info'): void; dismiss(id: number): void }

// [冻结] stores/sessionUi.ts
export const FILES_TAB = 'files'
useSessionUiStore(): { setActiveSub(sid, id); activeSubOf(sid): string; isFilesActive(sid): boolean;
                       leftIsVisible(sid): boolean; setLeftVisible(sid, v); toggleLeft(sid); forget(sid) }
```

### 6.2 组件接口

| 组件 | props | emits | 说明 |
| --- | --- | --- | --- |
| `TerminalPane.vue` [B] | `{ sid: string; tab: TermTab; active: boolean }` | — | 自己登记 paneControls、自己管 WS 与 xterm；**不负责**子标签栏 |
| `KeypadBar.vue` [B] | `{ ctrl: boolean; alt: boolean; shift: boolean }` | `key(data)`, `toggle('ctrl'\|'alt'\|'shift')` | 与 terminal 项目一致 |
| `FileManager.vue` [C] | `{ sid: string; mobile?: boolean }` | — | 完整文件管理面板；内部自己发起上传/下载/删除等 |
| `FileTree.vue` [C] | `{ sid: string; mobile?: boolean }` | `open-files(path)` | 左栏文件树；点目录 = 导航 + 切到文件管理子标签 |
| `BreadcrumbBar.vue` [C] | `{ sid: string }` | `show-full()` | 左栏面包屑 + 「完整文件管理」按钮 |
| `UploadDialog.vue` [C] | `{ visible: boolean; sid: string; dir: string }` | `update:visible(v)`, `done(UploadResult)` | 三个分组：本地文件 / 本地目录 / 飞牛文件 |
| `ConfirmDialog.vue` [D] | `{ visible, title, message, confirmText?, cancelText?, danger? }` | `confirm()`, `cancel()`, `update:visible(v)` | 通用确认框 |
| `PromptDialog.vue` [D] | `{ visible, title, label?, placeholder?, initialValue?, confirmText?, cancelText? }` | `confirm(value: string)`, `cancel()`, `update:visible(v)` | 通用输入框（重命名 / 新建） |
| `SessionFormDialog.vue` [D] | `{ visible: boolean; session?: SessionView \| null }` | `update:visible(v)`, `saved(s: SessionView)` | 新增/编辑 + 测试连接 + 首次信任指纹 |
| `UploadDialog` 的飞牛分组 | — | — | 用 `trim/sdk.ts` 的 `pickUserFiles`，把路径交给 `sftp.uploadFromNas` |

### 6.3 交互约定（必须一致）

- **应用级标签**：`首页`固定不可关；每个「已打开」的会话一个标签。标签标题默认 `user@host`，
  双击可重命名（本地持久化到 store；会话标签标题同时写入 `appTabs`）。关闭会话标签 =
  `home.closeSession(id)`（终止该会话全部终端 + SFTP）→ 关标签 → `terminals.forget(sid)`、
  `sftp.forget(sid)`、`sessionUi.forget(sid)`。**带会话的标签关闭前必须确认**。
- **打开会话**：`home.openSession(id)` → 无条件 `appTabs.openSession(id, label)` →
  `terminals.ensure(sid, state)` → 桌面端（`useMobileLayout() === false`）若没有任何终端标签则
  自动建 1 个：`terminals.add(sid)`；移动端不自动建，由用户点 +。
- **子标签栏**（`SessionView`）：
  - 固定段（不参与横向滚动）：`[文件管理]`（始终第一个，`FILES_TAB`）。
  - 滚动段：终端标签（点选激活、双击重命名、关闭按钮、显示状态点）+ 末尾 `[+]`。
  - 切到终端标签 → `sessionUi.setActiveSub(sid, tab.uid)`；切到文件管理 →
    `sessionUi.setActiveSub(sid, FILES_TAB)`。
  - 面板用 `v-show` 全部保持挂载（终端靠 visibility 隐藏以保持尺寸与 WS 存活）。
- **左栏**（桌面端）：`BreadcrumbBar` + `FileTree`，顶部有「完整文件管理」按钮（触发 `show-full`
  → 切到文件管理子标签）。移动端不渲染左栏（`SessionView` 直接把文件管理面板当作子页面）。
- **快捷指令**：工具条入口打开 `QuickCmdsDialog`，弹窗内部有「全局 / 专属」两个分组；
  「专属」只有会话视图里能切（首页不显示专属分组）。点击命令 → 发给当前激活终端
  （`paneControls.get(sid, activeUid)?.send(...)`），`auto` 为真时在内容后补 `\r`。
  未连接时 toast `qc_not_connected`。
- **首页卡片**：只平铺（网格，不提供列表/表格视图），卡片做大（`min-w-[22rem]`）。
  卡片必须显示：会话名 / `user@host:port` / 认证方式徽标（密码 / 密钥）/ 打开状态
  （未打开 / 已打开 + 开了几个终端子标签 + 打开于多久之前）/ 最近使用时间；
  操作：打开（或关闭）、编辑、删除。已打开时「关闭」按钮要显眼。
- **终端面板工具栏**（桌面端在子标签栏右侧，移动端第二行）：搜索 / 粘贴 / 清屏 / 重连 /
  快捷指令 / 设置 + 折叠功能名按钮（沿用 terminal 的 `labelsOn` 行为）。
- 移动端：底部辅助键条（`KeypadBar`，仅移动端布局渲染）；虚拟键盘跟随（`--vvh/--vvt`）；
  弹窗用 `.region-center` 定位带。

---

## 7. 冻结文件（本 Lead 维护，其他人只读）

`frontend/package.json`、`vite.config.ts`、`tsconfig.json`、`env.d.ts`、`index.html`、
`src/main.ts`、`src/style.css`、`src/serverapi/index.ts`、`src/i18n/*`、`src/utils/*`、
`src/composables/{useTheme,useViewportHeight,useMobileLayout}.ts`、`src/stores/sessionUi.ts`、
`docs/CONTRACT.md`、`Makefile`、`.gitignore`、`fpk/*`、`backend/go.mod`（依赖已定）。

需要新增 i18n key / 新增 API 字段 / 改 store 接口时：**先告诉 Lead**，不要自行改这些文件。
（并行编辑同一文件必然互相覆盖。）

## 8. 代码风格（两边都遵守）

- 注释、日志、错误文案：**中文**；标识符、类型名：英文。
- 注释解释「为什么」，不解释「是什么」；踩过的坑要写清症状与原因（参考 terminal 项目的注释密度）。
- 不使用 Element Plus / vue-router / dayjs。图标用内联 SVG（24×24，`stroke="currentColor"`，
  与 terminal 项目的写法一致）。
- Vue 一律 `<script setup lang="ts">` + Composition API，TS `strict`（含 `noUnusedLocals`）。
- 不引入未在 `package.json` 里的依赖。
- 不写「万一有用」的抽象；不留下死代码。

## 9. 验收与自测

- 后端：`make test` 全绿（含 `testssh` 集成测试）。
- 前端：`make typecheck` 无错误；`make dev` 能成功产出 `./webssh`。
- 端到端（Lead 负责）：用 `testssh` 起一个本地 SSH/SFTP 服务，走完整流程——建会话 → 测试连接
  （首次要信任指纹）→ 打开会话 → 自动出现 1 个终端标签 → 输入命令有回显 → 新建第 2 个终端
  标签 → 文件管理浏览、上传（本地文件/目录）、下载、新建、重命名、删除 → 快捷指令（全局 +
  专属）→ 关闭会话标签（终端与 SFTP 全断）→ 刷新页面会话仍打开并能恢复 → 移动端视口下键条可用。

---

## 10. 实施记录（构建完成后补记的偏差与实测结论）

以下都是实现阶段与端到端验证（`internal/testssh` 起的进程内 SSH/SFTP + 真实浏览器）中确认的
事实，实现与本文原稿不一致的地方**以这一节为准**。

### 10.1 后端

1. **SFTP 在 `open` 时就建立**（原稿写「懒加载」）：打开会话已经拨了号（为了在「打开」这一步
   就把 hostkey/auth 错误报出来），再开一条 channel 几乎不花钱；`sftpOpen` 因此一开始就是
   `true`。建立失败**不阻断 open**（记日志 + `sftpOpen:false`），首次文件操作会重连一次。
   前端不要把 `sftpOpen` 当「文件管理不可用」的硬门禁。
2. **错误码补充**：`not_open`（会话未打开）、`session_gone`（会话不存在/已删）、`aborted`
   （客户端取消）、`duplicate`（身份重复）。i18n 里已有对应 `err_*` 文案。
   连接类错误的状态码：远端失败 → 502；hostkey 相关 → 409；参数类 → 400。
3. **`/api/sessions/test` 的失败是 HTTP 200 + `{ok:true, result:{success:false, code,…}}`**；
   只有参数不合法才 4xx。
4. **`SessionInput` 的 `name` 空串 = 保留原值**（新建时回退 `user@host`）；其他字段按原稿
   「空串 = 清除」。
5. **上传同时接受 `files` 与 `files[]` 两个字段名**（冻结的前端用 `files`）。
   上传 filename 从 **Content-Disposition 原文**解析——Go 的 `mime/multipart` 会按 RFC 7578
   主动 `filepath.Base`，用它会把目录上传拍平（已修 + 测试钉住）。
6. **`fs/read` 上限 4 MiB**（与历史读取一致）；超限只回 `size` + `tooLarge: true`。
7. **`fs/remove` 的 `dir=true` 是递归删除**（确认文案已写明），拒绝删 `/`；删符号链接只删链接
   本身（`RemoveAll` 会顺着链接删掉目标整棵树）。
8. **进程启动时清理上次运行留下的 `history/`**（重启后没有任何挂载者，留着只占盘）。
9. **终端初始尺寸 80×24**：握手 URL 里没有尺寸，前端拿到 `ready` 后按 xterm 实际尺寸发一次
   `resize`。重新挂载**已退出**的终端时会补发 `\x1b]exit\x07`（该控制帧不落历史文件）。
10. **依赖**：新增 `github.com/creack/pty v1.1.24`，**只被 `internal/testssh` 使用**（生产代码
   不 import）；测试第一次跑需要能访问模块代理，之后走本地 `.gomodcache`。
11. 一个 x/crypto 的坑：它只往 `sess.Stdout` 写、**不关闭它**，所以用 `io.Pipe` 时 pump 会
    永久阻塞在 `Read` 上（症状：shell 退出后既没有 `[process exited]` 也没有 `\x1b]exit\x07`，
    终端永不回收）。现在由 `sess.Wait()` 返回后关写端来驱动。

### 10.2 前端

1. **面板隐藏必须用 `visibility`（`invisible`）而不是 `v-show`/`display:none`**：
   `display:none` 下容器尺寸为 0，FitAddon 量不到尺寸、WebGL 渲染器会反复触发 0 尺寸重排。
   所有面板都保持挂载、`absolute inset-0` 叠放。
2. **点完按钮要把焦点还给终端**（e2e 抓到的真 bug）：点子标签 / 点「+」之后焦点留在按钮上，
   用户接着敲的空格会去「激活」那个按钮——点一次「+」后按三个空格就多开三个终端；点「重连」
   后打字则完全进不了 shell。规则：`addTerminal()` 与 `activateTerm()` 之后聚焦对应终端面板；
   工具条的 `runAction()`（除 `search`）之后 `blur()` 按钮并聚焦终端。
3. 弹窗里的 `<label>` 必须与输入框用 `id`/`for` 关联（否则输入框没有可访问名，屏幕阅读器与
   自动化只能靠猜 placeholder）。
4. `SessionState.terminals[].title` 默认空串，由前端组合「终端 N / 终端 N：标题」。
5. 快捷指令保存以**后端返回的那一份**为准（后端会补齐 id、去重、trim）。

### 10.3 端到端验证覆盖（已实测通过）

建会话 → 连接测试（首次指纹未信任 → 信任并继续 → 自动重测成功 · 2ms）→ 保存 →
打开（自动 1 个终端）→ 命令回显与 `stty size`（PTY 尺寸 29×105 正确）→ 新建第二个终端
（且键入含空格的命令不会再多开标签）→ 文件管理（浏览 / 新建目录 / 上传本地文件 / 相对路径
目录上传保留层级 / `../` 逃逸被拒 / 同名未覆盖跳过 / 重命名 / 删除 / 单文件下载 / 目录 zip 下载）
→ 快捷指令（全局 + 专属分别落盘 `global.json` 与 `<sid>.json`；在文件管理页执行会兜底发到
最后一个终端并回显）→ 刷新页面（会话恢复、终端历史回放完整）→ 第二设备挂载同一终端（本端置
detached 且不自动重连；「重连」夺回后可继续输入）→ 关闭会话标签（终端与 SFTP 全断、历史目录
被清、配置保留）→ 后端重启（配置存活、打开态清空、前端提示）。

未在本次验证中覆盖（无对应环境，不是未实现）：移动端键盘/辅助键条（浏览器工具无法模拟窄视口
与触屏）、模型化输入法（IME）路径、飞牛宿主内的「选飞牛文件」上传（需要飞牛宿主桥）、
`Shift` 上档锁定等纯前端触屏交互。
`files_open_terminal_here`（在此目录打开终端）**按可选项未实现**（i18n 文案保留未用）。

### 10.4 已知的小瑕疵（不影响使用）

- 会话表单里的密码框不在 `<form>` 内，Chromium 会打印一条 verbose 级提示
  （`Password field is not contained in a form`）；这是浏览器给密码管理器的建议，不是错误。
- `SettingsDialog` / `UploadDialog` 里的若干「label + 按钮组/开关」没有做 `aria-labelledby`
  关联（第 10.2 节第 3 条只覆盖了真正的输入框）。
