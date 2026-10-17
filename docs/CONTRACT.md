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
> - `/vol1/@appshare/fpkstudio/projects/WebSSH`：飞牛应用包（fpk）的打包工程，**不在本仓库**；
>   本仓库只产出 `./webssh`，打包时把二进制放进该工程的 `app/bin/`。

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

- 进程启动顺序（照 terminal 的教训）：**先检查 socket 是否已被占用**（是则直接退出，且这一步刻意
  排在准备数据目录之前：`newAdminMux` 会建 master.key、读写会话配置，让一个注定要退出的实例先碰
  一遍数据目录纯属多余）→ 准备数据目录 → **先绑定 socket 成功再进信号等待**（绑定失败即退出，不要「活着但没有 socket」）→ 收到
  SIGTERM/SIGINT/SIGQUIT 后：先 `srv.Close()` 停入口 → 终止所有终端与 SFTP → 删除 socket
  **但保留数据目录**（会话配置必须活过重启）。
- `bootID`：每次进程启动生成一个随机 id，经 `/api/info` 的 `bootId` 下发。前端据此判断
  「后端换进程了」——换了就说明所有内存态会话都没了，前端要清空会话标签（配置仍在）。

---

## 2. 后端文件布局（`backend/`，Go 1.27）

入口与 HTTP 编排是 `package main`（`backend/*.go`）；可独立测试的子系统在 `backend/internal/` 下。
一个文件一个关注点，文件过大时按职责继续拆（见 AGENTS.md 第 8 节）。

### 2.1 `package main`（`backend/`）

| 文件 | 职责 |
| --- | --- |
| `main.go` | 入口：环境解析 → 目录准备 → socket 绑定 → 信号等待 → 优雅退出；`appVersion`（`-ldflags` 注入）；`bootID` |
| `config.go` | `RuntimeEnv`（全部来自环境变量）+ 默认值 |
| `protocol.go` | 终端控制协议常量：OSC 控制帧、close 4001/4002、历史/尺寸上限（与前端一对，见第 6 节） |
| `hostkeys.go` | known_hosts 的 TOFU 记录（读写 + 指纹计算 + 匹配判断） |
| `sshclient.go` | SSH 拨号：认证方式组装、主机密钥回调、超时、`ssh.Client` 生命周期 |
| `sessions.go` | 会话配置的管理：字段校验、保存/删除、测试连接、信任主机密钥 |
| `sessions_runtime.go` | `sessionRuntime`（已打开的会话）：终端 + SFTP 的容器、打开/关闭/列表/回收 |
| `terminal.go` | 单个终端：`ssh.Session` + PTY + 历史文件 + 单挂载点 attach/detach/resize |
| `admin.go` | `AdminMux`、JSON 助手、会话/终端/主机密钥的 HTTP handler |
| `admin_routes.go` | 路由表（路径 → handler）与 WebSocket 升级判定 |
| `static.go` | SPA 回退、缓存头三分类、base href 注入、unix socket 绑定、网关身份 |
| `sftp.go` | SFTP 会话生命周期、远程路径校验、SFTP 错误映射 |
| `files.go` | 文件列表与信息（list / info，含属主名字的会话内缓存） |
| `files_search.go` | 文件搜索（`/api/fs/search`，流式 NDJSON；上限、取消与写失败语义见第 32、34 节） |
| `files_write.go` | 新建文件 / 新建目录 / 重命名 / 删除 |
| `files_upload.go` | 上传（本地文件、目录、飞牛文件） |
| `files_download.go` | 下载（单文件流 / 目录打包 zip） |
| `files_preview.go` | 预览：`/api/fs/read` 的文本判定（UTF-8 / UTF-16）与 `/api/fs/image` 的图片白名单 |
| `files_edit.go` | 编辑：`/api/fs/edit-load`（写权限探测）与 `/api/fs/edit-save`（按原编码 / BOM / 换行写回） |
| `fileops.go` | 复制 / 移动 |
| `archive.go` | 压缩 / 解压 |
| `exportnas.go` | 导出至飞牛共享目录 |
| `quickcmds.go` | 全局 + 会话专属快捷指令的持久化与 API（命令内容加密 + 明文迁移，见 29） |
| `quickaccess.go` | 快捷访问（`/api/quickaccess/*`，按会话存的远程目录路径，见 30） |
| `trimapi.go` | `/api/trim/*` 的 HTTP 入口（客户端在 `internal/trimapi`） |
| `credentials.go` | 「不保存凭据」会话的内存凭据 |
| `remote_exec.go` | 远程命令 helper（exec 通道 + `shellQuote` + 超时 + ctx 取消） |
| `*_test.go` | 单元/集成测试 |

### 2.2 `internal/`（可独立测试的子系统）

| 包 | 职责 |
| --- | --- |
| `internal/apierr/` | API 错误码与 JSON 响应：裸码契约、状态码映射、`ok/code/error` 信封 |
| `internal/logx/` | 极简日志：只记异常/失败（不记例行启动停止） |
| `internal/secret/` | `master.key` 管理与 AES-256-GCM 字段加解密（密码 / 私钥 / 口令 / 快捷指令内容） |
| `internal/store/` | 数据目录布局、原子 JSON 读写（tmp + rename）、`Session`/`SessionView`/`SessionInput` |
| `internal/trimapi/` | 飞牛开放 API 客户端（`trim.file.checkUserACL`） |
| `internal/ws/` | WebSocket 帧编解码（含单帧/单消息上限） |
| `internal/testssh/` | **仅测试用**：进程内 SSH + SFTP 服务器（密码/密钥认证、PTY、sftp 子系统），供集成测试与本地联调 |

约束：

- **不要引入除 `golang.org/x/crypto`、`github.com/pkg/sftp` 之外的第三方依赖**（testify 只允许出现在测试里且需 `go mod tidy` 认可）。
- 单文件大小/请求体/帧大小上限必须有（进程承载全部会话，一个客户端就能打爆内存）：
  上传整体上限 1 GiB（`http.MaxBytesReader`）、JSON 体 1 MiB（**唯一例外**：`/api/fs/edit-save` 走
  `maxEditBodyBytes = 2×maxReadBytes + 64 KiB`，因为 body 里装的是要写回文件的文本，见 26.2）、
  WS 单帧 1 MiB / 单消息 4 MiB、`resize` 行列 ≤ 1000、历史读取上限 4 MiB、文本/图片预览 4 MiB、
  `upload-from-nas` 单文件 1 GiB、服务端搬运（复制 / 导出）4 GiB、压缩条目数 2000。
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
├── quickcmds/
│   ├── global.json              全局快捷指令（`content` 是密文，其余字段明文）
│   └── <sid>.json               会话专属快捷指令（同上）
└── quickaccess/<sid>.json       会话的快捷访问目录路径（第二十轮，见第 30 节；路径明文）
```

- **凭据加密**：`password` / `privateKey` / `passphrase` 三个字段以
  `{"v":1,"ct":"<base64(nonce||ciphertext)>"}` 落盘；AAD 绑定 `webssh:v1:<field>:<sid>`，
  避免密文被搬到别的会话/字段后仍能解出。读取时解密失败**必须报错**（不能静默当成空密码，
  否则表现为「密码莫名其妙失效」）。
- **快捷指令的命令内容加密**（第十九轮，见第 29 节）：`content` 用同一套信封落盘，AAD 是
  `webssh:v1:quickcmd:<指令 id>:<表作用域>`（全局表的作用域是空串、专属表是会话 id）；
  `id` / `name` / `auto` 仍是明文。第一轮留下的**明文** `content` 会被主动迁移掉（读取时
  就地加密重写 + 启动时扫一遍 `quickcmds/`），磁盘上不再有明文命令内容；内容为空或加密失败
  的条目换成加密的占位文案（29.3）。解密失败同样必须报错（不能静默给一个空内容）。
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
| POST | `/api/sessions/stats` | `{id}` | `{ok, stats: SessionStats}`（远端系统信息，见 23；会话没打开时 `not_open`） |
| GET | `/api/hostkeys` | — | `{ok, hosts:[HostKeyRecord]}` |
| POST | `/api/hostkeys/forget` | `{host, port}` | `{ok}` |
| POST | `/api/terminals/history` | `{sid, tid}` | `{ok, content, size}` |
| POST | `/api/terminals/clear` | `{sid, tid}` | `{ok}` |
| POST | `/api/terminals/close` | `{sid, tid}` | `{ok}` |
| POST | `/api/terminals/rename` | `{sid, tid, title}` | `{ok}` |
| GET | `/api/terminal/ws?sid=&tid=` | WebSocket | 见 4.4 |
| POST | `/api/fs/list` | `{sid, path}` | `{ok, path, home, entries:[FsEntry]}` |
| POST | `/api/fs/info` | `{sid, path}` | `{ok, info: FsInfo}`（单个路径的信息 + 目录直接子项计数，见 15.5） |
| POST | `/api/fs/mkdir` | `{sid, path}` | `{ok}` |
| POST | `/api/fs/create` | `{sid, path}` | `{ok}`（空文件；已存在则 `duplicate`） |
| POST | `/api/fs/rename` | `{sid, path, to}` | `{ok}`（目标已存在则 `duplicate`） |
| POST | `/api/fs/remove` | `{sid, path, dir}` | `{ok}`（dir 必须与真实类型一致，防止误删整棵树） |
| POST | `/api/fs/copy` | `{sid, paths:[], toDir}` | `{ok, files:[{path,size}], skipped}`（同名**跳过**，不覆盖） |
| POST | `/api/fs/move` | `{sid, paths:[], toDir}` | 同上（跨设备无法 rename → `conflict`，绝不静默降级成复制+删除） |
| POST | `/api/fs/compress` | `{sid, paths:[], name?, format?}`（`format` = `tar.gz`(默认) / `zip`） | `{ok, archive:{path,size,dir}}`（产物在**远程**归档目录） |
| POST | `/api/fs/extract` | `{sid, path}` | `{ok, dir}` |
| POST | `/api/fs/export-to-nas` | `{sid, path, overwrite?}` | `{ok, export:{path,size,files}}` |
| POST | `/api/fs/upload` | multipart：`sid`、`path`(目标目录)、`overwrite`、`files[]` | `{ok, files:[{path,size}], skipped}` |
| POST | `/api/fs/upload-from-nas` | `{sid, path, source, overwrite}` | `{ok, files, skipped}` |
| POST | `/api/fs/download` | `{sid, path}` | 文件流（`Content-Disposition`；目录打 zip） |
| POST | `/api/fs/read` | `{sid, path}` | `{ok, size, text, tooLarge, content, encoding}`（文本预览，见 25.2） |
| POST | `/api/fs/image` | `{sid, path}` | 图片**原始字节**（`Content-Type` = 按内容嗅探出的类型 + `nosniff`）；不是图片 → `invalid`，超过 4 MiB → `too_large`（见 25.2） |
| POST | `/api/fs/edit-load` | `{sid, path}` | `{ok, path, size, content, encoding, bom, eol}`（打开编辑器：先验证登录用户可写，见 26.2） |
| POST | `/api/fs/edit-save` | `{sid, path, content, encoding, bom, eol}` | `{ok, size}`（按原编码 / BOM / 换行写回；见 26.2） |
| POST | `/api/fs/search` | `{sid, path, query}` | **流式 NDJSON**（一条记录一行）：`start` → `batch`* → （`progress`*）→ `done`，开流之后出错回一行 `error`；开流**之前**的失败（关键词为空 / 会话没了 / 起点打不开）仍是普通 JSON 错误 + 状态码。见第 32 节 |
| POST | `/api/quickcmds/list` | `{sid}`（空 = 全局） | `{ok, scope, commands:[QuickCmd]}` |
| POST | `/api/quickcmds/save` | `{sid, commands:[QuickCmd]}` | `{ok, scope, commands}` |
| POST | `/api/quickaccess/list` | `{sid}`（**必填**，没有全局表） | `{ok, paths:[string]}`（第二十轮，见第 30 节） |
| POST | `/api/quickaccess/save` | `{sid, paths:[string]}` | `{ok, paths}`（整表覆盖；逐条规整 + 去重） |
| GET | `/api/trim/status` | — | `{ok, trim:{available, message, app}}`（handler 里也校验了方法） |
| POST | `/api/trim/acl` | `{paths:[...]}` | `{ok, list:[AclEntry]}` |

任何未命中的路径回退到 `index.html`（SPA 前端路由），但 `/api/*` 未命中要回 404 JSON。

### 4.3 数据结构（与 `src/serverapi/index.ts` 的类型逐字对应）

```jsonc
SessionView  { id, name, host, port, user, authType: "password"|"key",
               hasPassword: bool, hasPrivateKey: bool, keyFingerprint?: string,
               credentialSaved: bool,                              // false = 该会话不落盘凭据
               note, createdAt, updatedAt, lastUsedAt? }           // 时间一律 RFC3339
SessionInput { id?, name?, host?, port?, user?, authType?,
               password?, privateKey?, passphrase?, note?,
               saveCredential? }                                    // 密钥字段缺省 = 不修改
TestOutcome  { success, code, detail?, stage?, fingerprint?, keyType?,
               trusted?, mismatch?, banner?, latencyMs? }
TerminalInfo { id, title, createdAt, exited }
SessionState { id, open, openedAt?, terminals: [TerminalInfo], sftpOpen }
SessionStats { available, reason?, cpuPercent?, memUsed?, memTotal?, diskPercent?, load?: [n,n,n],
               cpuCount? }
             // 卡片上的系统信息，见 23。available=false = 远端读不出来（不是 Linux），
             // 各字段互相独立（没有 /proc 的机器可能只给 diskPercent）；
             // 单位：内存是字节，两个 percent 是 0–100 的数（不是字符串）；
             // cpuCount = 在线核数（负载的颜色按 load1 ÷ cpuCount 判，见 24.1）
FsEntry      { name, path, dir, size, mode, mtime, symlink?, broken? }  // path 是绝对路径
FsInfo       FsEntry + { uid, gid, owner?, group?, dirCount, fileCount, countError? }
             // 见 15.5 / 17.7：owner/group 是远端 `id` 查出来的名字，查不到就没有（界面显示数字）
FsEditContent { path, size, content, encoding: "utf-8"|"utf-16le"|"utf-16be", bom, eol: "lf"|"crlf" }
             // 编辑页打开时的内容与元信息（见 26.2）：encoding/bom/eol 保存时原样带回去
QuickCmd     { id, name, content, auto }
             // content **明文收发、密文落盘**（见第 3 节 / 29）；内容原样保留，不 trim
HostKeyRecord{ host, port, keyType, fingerprint, addedAt }
AclEntry     { path, readable, writable, deletable }
RuntimeInfo  { version, bootId, dataDir, adminSock, adminBaseURL, hostname,
               archiveDir, exportRoot,                              // 压缩产物目录 / 飞牛导出根
               trim: {available, message, app}, gateway: {uid, username, isAdmin, fromGateway} }
```

行为要求：

- `SessionInput` 的 `password/privateKey/passphrase` 为 `null`/缺省时**保留已存值**；给空字符串
  表示「清除该字段」（表单里「留空 = 不修改」由前端保证：不修改时字段根本不发）。
- `name` 同样是「字段缺省 = 保持原值、**显式空串 = 清除自定义名**」（第六轮修：原来把空串也当成
  「保持原值」，于是用户清空名称后保存、界面上还是旧名字）。清空后展示退回 `user@host`
  （后端 `displayName()` / 前端 `sessionLabel()` 同一口径）。
- `host` / `user` / `authType` 在**更新**时可以省（缺省 = 保留现有值，见
  `resolveSessionFields`）：`home.renameSession` 就只发 `{id, name}`——回传本地缓存的身份字段
  会在「另一台设备刚改过地址」时把它覆盖掉。新增会话时三者必填。
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
- `/api/quickaccess/*`（第二十轮，第 30 节）：`paths` 是**按会话**的远程绝对路径列表，没有
  全局表；`sid` 为空 → `invalid`，会话不存在 → `session_gone`。`save` 是**整表覆盖**，后端
  逐条规整（清重复/末尾斜杠、拒绝相对路径与 `..` 段、按规整后的结果去重）并保留 `paths` 的
  顺序；**不 trim**（以空格结尾的目录名合法，与 `resolveRemotePath` 同一条纪律）。

### 4.4 终端 WebSocket 协议

路径 `/api/terminal/ws?sid=<会话 id>&tid=<终端 id 或省略>`。握手照 terminal 的自实现
（`Sec-WebSocket-Accept`、单端帧编解码、掩码解包、上限、`opText/opBin/opClose/opPing/opPong`）。
握手校验：`Sec-WebSocket-Key` 必须是 16 字节 base64；**不回显** `Sec-WebSocket-Protocol`
（本应用没有子协议）；客户端帧必须掩码，RSV1-3 置位或保留 opcode 一律失败。

- ⚠️ **服务端 → 客户端的终端数据一律用二进制帧（`opBin`），不用文本帧。** 终端输出是任意
  字节流，而文本帧的载荷按 RFC 6455 §8.1 必须是合法 UTF-8：只要有一个非法字节（分帧切在
  多字节字符中间、或远端本来就输出非 UTF-8 字节，如 `cat` 二进制文件、GBK 文件名被 `ls`
  回显），浏览器就会判定协议错误并**直接断开连接**。回放按 32 KiB、实时按 8 KiB 切帧，
  两者都与字符边界无关。前端因此用**流式** `TextDecoder`（`{stream:true}`）解码，跨帧的
  半个字符会被拼回来；非法字节按替换字符显示（真实终端的行为）。改回文本帧 = 含中文的终端
  在重挂载时随机断连，`internal/ws` 与 `protocol.go` 的注释里都写了这一点。
- `tid` 为空 → **新建终端**：先发一帧 `\x1b]tid;<id>\x07`（前端据此回填 id），再 `attach`。
- `tid` 非空 → 挂载已有终端：回放历史（分帧，单帧 ≤ 32 KiB，最多 4 MiB）→ `\x1b]ready\x07`
  → 进入实时流。
- 终端不存在 → 一帧 `\r\n\x1b[31m…\x1b[0m\r\n` + `\x1b]gone\x07` 控制帧，然后关闭。
  ⚠️ 前端只认 `\x1b]gone\x07`，**不能**用人类可读文本判定（终端里 `echo` 出同样文本会误判）。
- **新建终端失败**（没有可用凭据 / 认证失败 / 远端拒绝 PTY…）**不发 `gone`**：一帧原因文本
  （`no_credential` 会追加「请重新打开会话以重新输入凭据」），然后以 close code **4002**
  （`wsCloseCreateFailed`）关闭。前端据此保留标签、把状态点标红并提示可以重连；`gone` 只用
  于「终端/会话在后端不存在」（前端会把标签当成失效）。两者混用会把一个可修复的凭据问题
  说成「会话没了」——用户看到的是「新建一个失败一个」，永远猜不到原因。
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
│   ├── sftp.ts                 [C]    SFTP 文件浏览状态（含目录树的文件截断与总数）
│   ├── sessionStats.ts         [D]    卡片上的系统信息（5 秒轮询，见 23）
│   ├── fileEditor.ts           [C]    文件编辑页状态（按 sid 分片，见 26.3）
│   └── quickAccess.ts          [C]    快捷访问：按会话分片的目录路径 + 弹窗显隐（见 30）
├── trim/sdk.ts                 [C]    @trimjs/web-app 薄封装（选飞牛文件）
├── components/
│   ├── AppTabBar.vue           [D]    顶栏标签条（首页固定 + 会话标签）
│   ├── Toast.vue               [D]
│   ├── ConfirmDialog.vue       [D]    visible/title/message/confirmText/cancelText → confirm|cancel
│   ├── PromptDialog.vue        [D]    visible/title/label/placeholder/initialValue → confirm(value)|cancel
│   ├── SettingsDialog.vue      [D]    主题/语言/字体/字号/关于（已信任主机第十一轮挪到会话弹窗）
│   ├── QuickCmdsDialog.vue     [D]    三页：常用指令 / 全局 / 专属（会话）
│   ├── CredentialPromptDialog.vue [D] 「不保存凭据」的会话在 open 时弹的凭据输入框
│   ├── QuickCmdEditDialog.vue  [D]    新增/编辑单条
│   ├── home/SessionCard.vue    [D]    大号会话卡片
│   ├── home/SessionFormDialog.vue [D] 新增/编辑 + 测试连接
│   ├── terminal/TerminalPane.vue  [B] 一个终端标签（xterm + WS）
│   ├── terminal/KeypadBar.vue     [B] 移动端辅助键条
│   ├── files/FileManager.vue      [C] 文件管理面板（工具栏 + 面包屑 + 列表/网格 + 菜单）
│   ├── files/SearchBar.vue        [C] 搜索栏（输入框 / 「结果：x 项」/ 搜索 / 取消，见 32）
│   ├── files/SearchResults.vue    [C] 结果区（行 / 网格 + 收尾统计 + 空态 + 失败原因，见 32）
│   ├── files/NewEntryDialog.vue   [C] 新建弹窗（目录 / 文件二合一，见 33）
│   ├── files/FileTree.vue         [C] 左栏文件树
│   ├── files/BreadcrumbBar.vue    [C] 左栏按钮条（刷新 / 快捷访问 / 隐藏面包屑）
│   ├── files/QuickAccessDialog.vue [C] 快捷访问弹窗（列表 + 手动输入 + 删除，见 30）
│   ├── files/InfoDialog.vue       [C] 「信息」弹窗（/api/fs/info，见 15.5）
│   ├── files/UploadDialog.vue     [C] 上传弹窗（本地文件 / 本地目录 / 飞牛文件）
│   ├── files/PreviewDialog.vue    [C] 预览：图片走 /api/fs/image、文本走 /api/fs/read（见 25）
│   ├── files/FileEditor.vue       [C] 编辑页（覆盖在文件管理面板上，见 26.3 / 27）
│   ├── files/fileKind.ts          [C] 按扩展名判「预览用哪种界面 / 能不能编辑 / 能不能解压」
│   └── home/SessionCard.vue       [D] 大号会话卡片（含两行系统信息）
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
  openSession(sid: string, title?: string, activate?: boolean): void  // activate=false：只建标签不抢视图（恢复/同步）
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
  load(force?: boolean, quiet?: boolean): Promise<void>   // quiet：后台同步失败不写整页 error
  stateOf(id: string): SessionState | undefined
  isOpen(id: string): boolean
  save(input: SessionInput): Promise<SessionView>
  remove(id: string): Promise<void>
  test(input: SessionInput): Promise<TestOutcome>
  trust(host: string, port: number, fingerprint: string): Promise<void>
  openSession(id: string): Promise<SessionState>
  closeSession(id: string): Promise<void>
  loadInfo(): Promise<void>
  runtime: RuntimeInfo | null             // 运行信息（归档目录 / 导出目标）
  renameSession(id: string, name: string): Promise<SessionView>   // **唯一**改名入口（只发 {id,name}）
  forgetSession(sid: string): void        // 摘掉前端状态（appTabs/terminals/sftp/fileEditor/quickAccess/sessionUi）
  handleTerminalGone(sid: string, uid: string): void   // 后端说这个终端没了（\x1b]gone\x07）
  watchBackendRestart(): void             // WS 掉线后探测后端是否换了进程
  restore(force?: boolean): Promise<string[]>  // 恢复/同步：后端仍在运行的会话 → 标签 + 子终端（**不激活**）；force 重新拉 /api/sessions
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
  add(sid: string, opts?: { initialCommand?: string }): TermTab   // 见 11.1-5
  openTerminalAt(sid: string, dir: string): TermTab              // 「在此目录新建终端」（cd + shellQuote）
  adoptTid(sid: string, uid: string, tid: string, createdAt?: string): void
  setStatus(sid: string, uid: string, status: TermStatus): void
  setTitle(sid: string, uid: string, title: string): void
  titleOf(sid: string, tab: TermTab): string
  setActive(sid: string, uid: string): void       // **切子标签只走它**（同时写 sessionUi，见 15.4）
  lastActiveOf(sid: string): TermTab | null       // 导航目标（切回哪个终端）；不是「指令发给谁」
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
                             mode: PermMode; sort: SortState; multi: boolean;
                             treeChildren: Record<string, FsEntry[]>;
                             treeFileTotal: Record<string, number>;   // 每个目录的文件总数（树只列前 20 个）
                             expanded: string[];
                             treeLoading: Record<string, boolean>; selected: string[];
                             search: SftpSearch }
// SftpSearch（第二十二轮，契约 32）：{ open, query, viewing, running, done, results: FsEntry[],
//   scopeDir, scanned, dirs, skipped, truncated, elapsedMs, error }
//   （`scopeQuery` 在第二十四轮删除：32.7 之后它只写不读，缓存只服务「回到上次结果」）
// PermMode = 'symbolic'|'octal'（权限列显示模式，点表头「权限」切换；localStorage 里存的也是 'octal'）
// SortState = { key: 'name'|'size'|'mtime'|null; dir: 'asc'|'desc' }（null = 不排序，回到服务端顺序）
useSftpStore(): {
  stateOf(sid: string): SftpState
  ensure(sid: string): Promise<void>            // 首次进入：定位家目录并加载
  navigate(sid: string, path: string): Promise<void>   // 记录前进/后退历史
  jumpTo(sid: string, path: string): Promise<void>     // 先探测再切过去（快捷访问的点击，见 30.3）：
                                                       // 列目录失败 → 抛异常且**不动任何展示状态**
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
  // 多选模式（模式而非一次性全选，见 15.6/AGENTS 第 5 节）
  setMulti(sid: string, on: boolean): void; exitMulti(sid: string): void
  // 剪贴板：全局一份、不跨会话；「清不清」由入口决定（keepClipboard）
  copyToClipboard(sid: string, paths: string[]): void
  cutToClipboard(sid: string, paths: string[]): void
  clipboardFor(sid: string): Clipboard | null; clipboardIsForeign(sid: string): boolean
  clearClipboard(): void
  paste(sid: string, toDir: string, keepClipboard: boolean): Promise<PasteOutcome>
  // 远程批量操作与归档
  compress(sid: string, paths: string[], format: 'tar.gz'|'zip', name?: string): Promise<ArchiveInfo>
  extract(sid: string, path: string): Promise<string>
  exportToNas(sid: string, path: string, overwrite: boolean): Promise<ExportInfo>
  // 树 / 运行信息（TREE_FILE_LIMIT = 20：每个目录只列前 20 个文件，见 26.4）
  expandPath(sid: string, path: string): Promise<void>
  refreshTree(sid: string, path: string): Promise<void>
  runtime(sid: string): RuntimeInfo | null; ensureRuntime(sid: string): Promise<void>
  exportRoot(sid: string): string; archiveDir(sid: string): string
  // 权限列模式 / 排序
  toggleMode(sid: string): void; cycleSort(sid: string, key: string): void
  sorted(sid: string): FsEntry[]
  sortedResults(sid: string): FsEntry[]     // 结果区复用同一份排序偏好（32.6；key 为 null 时保持到达顺序）
  // 搜索（第二十二轮，见 32）：openSearch 只是把栏打开（幂等）；只有 closeSearch 退出搜索
  // （关键词与结果一起清掉）。两个动作各有一个控件（32.7）：runSearch = 在当前目录搜一次，
  // showSearchResults = 回到上次那批结果（只切视图、不发请求）
  openSearch(sid: string): void; closeSearch(sid: string): void
  leaveSearchResults(sid: string): void      // 列表区切回目录内容（导航时由 store 自己调）
  runSearch(sid: string): Promise<void>      // 关键词为空 → 抛错（调用方 toast）
  showSearchResults(sid: string): void       // 「结果：x 项」按钮（没搜过时不动）
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

// [C] stores/quickAccess.ts（第二十轮，见 30）
export function normalizeQuickPath(raw: string): string | null   // 与后端 sanitizeQuickPath 同一套规则
useQuickAccessStore(): {
  listOf(sid: string): string[]
  isLoading(sid: string): boolean; isFailed(sid: string): boolean
  ensure(sid: string): Promise<void>              // 幂等；同一个 sid 上并发只发一次请求
  reload(sid: string): Promise<void>   // 强制重拉：弹窗的「重试」走它（失败态由 failed 记，
                                       // 原因文案留在弹窗里）
  add(sid: string, path: string): Promise<'added' | 'exists'>   // 路径非法 → 抛 Error；否则乐观更新 + 失败回滚
  remove(sid: string, path: string): Promise<void>
  isDialogOpen(sid: string): boolean; setDialog(sid: string, open: boolean): void
  forget(sid: string): void                       // home.forgetSession 会调它
}

// [C] stores/fileEditor.ts（契约 26.3 / 27）
useFileEditorStore(): {
  stateOf(sid: string): FileEditorState     // 按 sid 分片；新建分片要「赋值后再从 ref 读一次」，见 18.4
  open(sid: string, path: string): Promise<void>   // 先 edit-load 探写权限，成功才进编辑页
  save(sid: string): Promise<void>; revert(sid: string): Promise<void>
  close(sid: string): void
  isDirty(sid: string): boolean             // 关闭会话标签时的「未保存会丢」提醒（见 2610 行附近）
  forget(sid: string): void                 // home.forgetSession 会调它
}

// [D] stores/settings.ts
export const FONT_FAMILY_KEY: string   // main.ts 用到
useSettingsStore(): { fontSize: number; terminalFont: 'maple'|'system';
                      setFontSize(n: number): void; setTerminalFont(v: 'maple'|'system'): void }

// [D] stores/sessionStats.ts（第十三轮，见 23）
export const STATS_INTERVAL_MS = 5000
export const STATS_THRESHOLDS = { cpu:{warn,danger}, load:{...}, mem:{...}, disk:{...} }
export type StatLevel = 'ok' | 'warn' | 'danger'
cpuLevel(pct): StatLevel; loadLevel(load1, cores): StatLevel
memLevel(used, total): StatLevel; diskLevel(pct): StatLevel
statLevelClass(level: StatLevel): string        // 'text-success' | 'text-warning' | 'text-danger'
useSessionStatsStore(): { statsOf(sid: string): SessionStats | undefined
                          sync(sids: string[], visible: boolean): void
                          // 定时器的生死由调用方（HomeView）负责：sync 是唯一的开表入口，
                          // stop/dispose 是唯一的关表出口（组件卸载时要调，见 23.6）
                          stop(): void; dispose(): void }

// [D] stores/toast.ts
useToastStore(): { toasts: {id:number;msg:string;type:'success'|'error'|'info'}[];
                   show(msg: string, type?: 'success'|'error'|'info'): void; dismiss(id: number): void }

// [冻结] stores/sessionUi.ts
export const FILES_TAB = 'files'
useSessionUiStore(): { setActiveSub(sid, id); activeSubOf(sid): string; isFilesActive(sid): boolean;
                       leftHiddenOf(sid): boolean; setLeftHidden(sid, hidden: boolean); forget(sid) }
// 左栏可见性 = wide && !filesActive && !leftHidden（三者的交集，见 13.1/14.3/16.1）
```

### 6.2 组件接口

| 组件 | props | emits | 说明 |
| --- | --- | --- | --- |
| `TerminalPane.vue` [B] | `{ sid: string; tab: TermTab; active: boolean }` | — | 自己登记 paneControls、自己管 WS 与 xterm；**不负责**子标签栏 |
| `KeypadBar.vue` [B] | `{ ctrl: boolean; alt: boolean; shift: boolean }` | `key(data)`, `toggle('ctrl'\|'alt'\|'shift')` | 与 terminal 项目一致 |
| `FileManager.vue` [C] | `{ sid: string; mobile?: boolean }` | — | 完整文件管理面板；内部自己发起上传/下载/删除等 |
| `FileTree.vue` [C] | `{ sid: string }` | — | 左栏文件树（目录 + 每个目录前 20 个文件）；点目录 = 只更新当前目录并展开（**不切**子标签，见 11.3），点文件 = 预览。只有一个调用方且只在宽屏渲染，所以没有 `mobile` prop |
| `BreadcrumbBar.vue` [C] | `{ sid: string }` | — | 左栏按钮条；「刷新」「快捷访问」（第二十轮，与文件管理工具栏那个书签按钮**同一个**弹窗）「隐藏面包屑」（18.1 删掉了「完整文件管理」） |
| `UploadDialog.vue` [C] | `{ visible: boolean; sid: string; dir: string }` | `update:visible(v)`, `done(UploadResult)` | 三个分组：本地文件 / 本地目录 / 飞牛文件 |
| `PreviewDialog.vue` [C] | `{ visible: boolean; sid: string; path: string; size?: number }` | `update:visible(v)`, `edit(path)` | 图片 / 文本两种模式；`size` 是列表里已知的体积（图片超限时用它把话说清楚）；`edit` 由调用方决定怎么进编辑页 |
| `QuickAccessDialog.vue` [C] | `{ sid: string; visible: boolean }` | `update:visible(v)` | 快捷访问弹窗（第二十轮，见 30）：路径列表 + 手动输入添加 + 逐条删除；点一条 = 先探测再进那个目录。挂在 `SessionView` 上（显隐在 `stores/quickAccess.ts`） |
| `ConfirmDialog.vue` [D] | `{ visible, title, message, confirmText?, cancelText?, danger? }` | `confirm()`, `cancel()`, `update:visible(v)` | 通用确认框 |
| `PromptDialog.vue` [D] | `{ visible, title, label?, placeholder?, initialValue?, confirmText?, cancelText? }` | `confirm(value: string)`, `cancel()`, `update:visible(v)` | 通用输入框（重命名等） |
| `NewEntryDialog.vue` [C] | `{ visible: boolean; label?: string; placeholder?: string }` | `submit(kind: 'file' \| 'dir', value: string)`, `cancel()`, `update:visible(v)` | 新建弹窗（第二十三轮，见 33）：一个输入框 + 「新建文件夹 / 新建文件」两个动作按钮；**提交不自我关闭**（校验在调用方，失败时弹窗留着） |
| `SessionFormDialog.vue` [D] | `{ visible: boolean; session?: SessionView \| null }` | `update:visible(v)`, `saved(s: SessionView)` | 新增/编辑 + 测试连接 + 首次信任指纹 |
| `UploadDialog` 的飞牛分组 | — | — | 用 `trim/sdk.ts` 的 `pickUserFiles`，把路径交给 `sftp.uploadFromNas` |

### 6.3 交互约定（必须一致）

- **应用级标签**：`首页`固定不可关；每个「已打开」的会话一个标签。标签标题默认 `user@host`，
  双击可重命名（本地持久化到 store；会话标签标题同时写入 `appTabs`）。关闭会话标签 =
  `home.closeSession(id)`（终止该会话全部终端 + SFTP）→ 关标签 → `home.forgetSession(sid)`
  （内部依次 `appTabs.close` / `terminals.forget` / `sftp.forget` / `fileEditor.forget` /
  `sessionUi.forget`）。**带会话的标签关闭前必须确认**；编辑页里有未保存的内容时，确认文案
  会追加一句提醒（第十八轮）。
- **启动恢复与进入会话**（第二十五轮，见第 35 节）：`home.restore()` 把后端 `open` 列表投影成
  **应用标签 + 终端子标签**（因此首页卡片上能看到子终端列表），但**不激活**任何会话标签——
  启动后停在首页。`SessionView` 只在用户第一次进入该会话时挂载（`App.vue` 的
  `enteredSessions`），进入过之后保持挂载。应用回到前台 / 切回首页时 `home.restore(true)` 再
  对齐一次后端状态，把别的设备新开的会话补成标签与卡片列表。
- **打开会话**：`home.openSession(id)` → 无条件 `appTabs.openSession(id, label)` →
  `terminals.ensure(sid, state)` → 没有任何终端标签就自动建 1 个：`terminals.add(sid)`。
  **桌面与窄屏一致**（第五轮改的，见 15.3：手机上新开会话也直接落在「终端 1」）。
  新终端挂载时若是激活项，面板自己会聚焦（`TerminalPane` 的 `watch(active, {immediate:true})`），
  用户不用先点一下终端才能打字。
- **子标签栏**（`SessionView`）：
  - 固定段（不参与横向滚动）：`[文件管理]`（始终第一个，`FILES_TAB`）。
  - 滚动段：终端标签（点选激活、双击重命名、显示状态点）+ 末尾 `[+]`；关闭按钮**只在
    `termTabs.length > 1` 时渲染**（22.1）。
  - 切到终端标签 → `sessionUi.setActiveSub(sid, tab.uid)`；切到文件管理 →
    `sessionUi.setActiveSub(sid, FILES_TAB)`。
  - 面板**全部保持挂载**、`absolute inset-0` 叠放，非激活的加 `invisible`（`visibility:hidden`）。
    **不要**用 `v-if`（切标签会销毁 xterm 与 WS）也不用 `v-show`/`display:none`（尺寸为 0，
    FitAddon 量不到、WebGL 反复触发 0 尺寸重排）。例外只有左栏（没有 xterm，且必须真的让出
    横向空间，见 `SessionView.vue` 的 `showLeft`）。这条规则从「会话视图第一次被进入」起生效：
    此前整个 `SessionView` 根本不挂载（第 35 节），一旦进入就不再卸载。
- **左栏**（宽屏且终端在前台且没被手动收起时）：`BreadcrumbBar` + `FileTree`。可见性是
  `wide && !filesActive && !leftHiddenOf(sid)` 的交集（13.1/14.3/16.1）；收起时子标签栏左侧出现
  「展开面包屑」。**没有**「完整文件管理」按钮（13.1/18.1 两次删除）。移动端不渲染左栏
  （`SessionView` 直接把文件管理面板当作子页面）。
- **快捷指令**：工具条入口打开 `QuickCmdsDialog`，弹窗内部是**三页**：`常用指令 | 全局 | 专属`
  （11.6）；「专属」只有会话视图里能切（首页不显示专属分组）。点击命令 → 发给当前激活终端
  （`paneControls.get(sid, activeUid)?.send(...)`），`auto` 为真时在内容后补 `\r`。
  未连接时 toast `qc_not_connected`。
- **快捷访问**（第二十轮，第 30 节）：入口两个——文件管理工具栏「视图」右侧的书签按钮与左栏
  按钮条的「快捷访问」按钮，都打开 `QuickAccessDialog`（挂在 `SessionView` 上，显隐是
  `stores/quickAccess.ts` 的 per-sid 开关）；另有两个「只加不弹窗」的入口：目录的「更多」菜单
  （文件管理与目录树）与文件管理列表/网格的空白处右键菜单。点弹窗里的一条路径 = `sftp.jumpTo`
  先探测再进那个目录（失败只 toast，**不切子标签、不改变当前目录**）。
- **首页卡片**：只平铺（网格，不提供列表/表格视图），卡片做大（`min-w-[22rem]`）。
  卡片显示：会话名 / `user@host:port` / 备注 / 「不保存凭据」提醒 / **内联的子终端列表**
  （点一行 = 快捷打开）。**不再有**认证方式徽标、打开状态灯、打开时间与最近使用时间
  （15.2/16.2 删的，见 16.2）。操作：打开（或关闭）、**进入**（仅已打开时）、编辑、删除。
  已打开时「关闭」按钮要显眼；「进入」是第二十五轮补的入口——后端开着但本机还没有标签
  （另一台设备开的会话、或页面停在旧快照上）时，用户不必先关掉再打开。
  **打开/关闭共用同一个按钮与同一个电源图标**（第二十八轮）：未打开时绿色（`text-success`，
  点击 = 打开会话），已打开时红色（`text-danger`，点击 = 关闭会话）；hover 底色同色系。
  不再用「外链箭头」表示打开——状态只由颜色表达，图标不随状态变。
- **首页排序**（第二十七轮）：工具条「设置」左侧有一个排序切换按钮，**仅当会话数 ≥ 2**
  时显示（0/1 个会话时排序没有意义）。文案 = 当前模式：`排序：默认` / `排序：名称`
  （`home_sort_recent` / `home_sort_name`），点一下在两者间切换，偏好存本设备
  localStorage（`webssh-home-sort`，脏值按 `recent` 处理，不动会话配置）。
  `默认` 沿用原逻辑：`lastUsedAt || updatedAt || createdAt` 降序（最近使用优先）；
  `名称` 固定**升序**、按**展示名**（`sessionLabel`，空名回退 `user@host`）比较，
  `Intl.Collator(locale, { numeric: true, sensitivity: 'base' })`，同名再按默认逻辑排。
  两种模式都只影响卡片展示顺序，与「已打开」无关（没有置顶）。
- **终端面板工具栏**（桌面端在子标签栏右侧，移动端第二行；**没有设置按钮**，设置只在首页工具条上）：
  搜索 / 粘贴 / 清屏 / 重连 /
  快捷指令 / 设置 + 折叠功能名按钮（沿用 terminal 的 `labelsOn` 行为）。
- 移动端：底部辅助键条（`KeypadBar`，仅移动端布局渲染）；虚拟键盘跟随（`--vvh/--vvt`）；
  弹窗用 `.region-center` 定位带。

---

## 7. 冻结文件（本 Lead 维护，其他人只读）

`frontend/package.json`、`vite.config.ts`、`tsconfig.json`、`env.d.ts`、`index.html`、
`src/main.ts`、`src/style.css`、`src/serverapi/index.ts`、`src/i18n/*`、`src/utils/*`、
`src/composables/{useTheme,useViewportHeight,useMobileLayout}.ts`、`src/stores/sessionUi.ts`、
`docs/CONTRACT.md`、`Makefile`、`.gitignore`、`backend/go.mod`（依赖已定）。

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
  ⚠️ 无 PTY 的环境（多数 CI 容器：`open /dev/ptmx: permission denied`）会**静默跳过** 10 个
  PTY 用例（`requirePTY` → `t.Skip`），包括终端收发 / 单挂载点顶号 / 历史回放；本机还会多跳 1 个
  `TestFsInfoCountError`（它依赖「0000 目录读不出子项」这个权限前提，见 34.2）。看到「全绿」
  时要顺带看一眼 skip 数，别把「没跑」当成「跑过了」。
- 前端：`make check`（= `vue-tsc` + `check:i18n`）无错误；`make dev` 能成功产出 `./webssh`。
  **完整门禁是 `make test check`**。
- i18n key 数：**以 `npm run check:i18n` 的实际输出为准（当前 zh/en 各 468）**。下面各轮小节
  括号里的 key 数是**那一轮当时的快照**，有增有删、不保证单调，不要拿它当基准（历史上有
  396/397/398/400/402/438/469/470 几个数字并存，第 24 轮把这一处与各轮小节一起对齐到 468）。
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
5. 快捷指令保存以**后端返回的那一份**为准（后端会补齐 id、去重、trim **名称**；
   命令内容一律原样保留，只把「全是空白」判成空——第 29 节）。

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
`files_open_terminal_here`（在此目录打开终端）**后续轮次已实现**（`sftp.openTerminalAt` +
`terminals.openTerminalAt`，文件管理与目录树的菜单里都在用）。

### 10.4 已知的小瑕疵（不影响使用）

- 会话表单里的密码框不在 `<form>` 内，Chromium 会打印一条 verbose 级提示
  （`Password field is not contained in a form`）；这是浏览器给密码管理器的建议，不是错误。
- `SettingsDialog` / `UploadDialog` 里的若干「label + 按钮组/开关」没有做 `aria-labelledby`
  关联（第 10.2 节第 3 条只覆盖了真正的输入框）。

---

## 11. 第二轮需求的接口变更（本轮实现的唯一依据）

本节列出「首页/会话/文件管理/快捷指令」这一轮改造涉及的全部接口与行为。与前面各节冲突时，
**以本节为准**；实现完成后本节内容会并入第 10 节的实施记录。

### 11.1 会话与凭据

1. **应用级标签改名**：`首页` → `会话`（i18n `app_tab_home` 的值改为「会话」/「Sessions」）。
   首页视图标题保持「远程会话」。
2. 首页去掉**搜索框**与副标题（原 `home_search`/`home_search_empty`/`home_subtitle` 文案作废）。
3. **凭据保存开关**：`SessionInput` 新增 `saveCredential?: boolean`（缺省 = `true`）。
   - `true`：与现在一致——密码/私钥/口令加密落盘。
   - `false`：**不落盘**（三个字段都不写），配置里 `credentialSaved: false`；
     凭据只在本进程内存里保存，用于该会话后续的终端/SFTP 连接。
   - `SessionView` 新增 `credentialSaved: boolean`（前端据此在卡片上标注「不保存凭据」）。
   - `POST /api/sessions/open` 的请求体新增可选的 `password` / `privateKey` / `passphrase`：
     当会话 `credentialSaved == false` 且内存里没有凭据时，open **必须**带凭据，否则返回
     `credential_required`（409）。带凭据时先用它拨号，成功后再把凭据留在内存（供该会话的
     后续终端与 SFTP 复用）。**会话关闭（或进程退出）后内存凭据即失效**，下次 open 会再次
     要求输入——这就是「不保存时每次连接都要输入」的落地方式。
   - **会话开着时把「保存凭据」从开切到关**：当前会话的凭据不会因为这次保存而丢失——后端把
     刚清掉落盘的那份解密后放进内存缓存（只活到 close），新建子终端 / SFTP 断线重连照常
     可用。用户只是关掉了一个持久化开关，不等于要中止当前会话。用户在同一请求里给了新凭据
     时以新凭据为准；认证方式变了则不 carry over（见 12.1.7）。
   - `POST /api/sessions/test` 一直都可以带明文凭据（表单里就是这样用的），无需改动。
   - 编辑一个 `credentialSaved == false` 的会话时，表单里**不显示**「留空 = 不修改」的提示
     （因为没有可保留的东西），打开开关就等于把凭据存下来。
4. **卡片上的终端数量可点击**：点开一个弹窗，列出该会话当前打开的每个终端
   （显示 `终端 N`、可改名、创建时间、单个关闭按钮、新建按钮）。
   → **第六轮把这个弹窗整个删掉，终端列表直接内联在卡片下（带快捷打开），见 16.2。**
   弹窗里的**会话名称**与会话标签名**双向同步**：弹窗改名 → `/api/sessions/save` 更新配置 +
   应用标签标题；应用标签双击改名 → 同样调用 `/api/sessions/save` 更新配置。
   为此前端新增 `home.renameSession(id, name)`；`appTabs.setTitle` 只作为展示层缓存。
   - 改名载荷：**只发 `{id, name}`**（第六轮起）。历史上必须带 `port` 原值的原因是后端把
     缺省的 `port=0` 归一成 22 并判成「改了身份」→ 409 conflict；现在后端按部分更新语义
     把 `port==0` 视为保留原值（`resolveSessionFields`），回传本地缓存的身份字段反而会在
     「另一台设备刚改过地址」时把它覆盖掉。
   - 统一的**部分更新语义**：更新已有会话时，`host`/`user`/`authType`/`port` 为空串或 0 表示
     「保留原值」；`name` 空串同样保留。凭据字段缺省 = 不修改。
5. `terminals` 新增：`add(sid, opts?: { initialCommand?: string })` 与
   `openTerminalAt(sid, dir)`（= 新开一个终端并在其中 `cd` 到该目录）。
   `TermTab` 新增 `initialCommand?: string`；`TerminalPane` 在 WS `onopen` 后**只发一次**
   并清空该字段（用它实现「在此目录新建终端」，避免「先建标签再等 connected 才发命令」的竞态）。

### 11.2 会话视图（桌面 / 移动两种形态）

- **桌面**：子标签栏**不再有常驻的「文件管理」子标签**，只有终端子标签 + `+`。
  左栏（面包屑区 + 目录树）常驻（去掉原来的显隐按钮）。左栏顶部是**按钮条**而不是路径：
  `[家目录] [完整文件管理（图标，切换双模式）] [在此目录新建终端] [刷新]`。
  「完整文件管理」把主区域切换成文件管理器；再点一次（或点子标签栏里的终端标签）切回终端。
  ~~状态放在 `sessionUi.mainMode(sid)`：`'terminal' | 'files'`。~~
  → **这套 `mainMode` 已被删除**（见 13.1、14.3）：主区域显示什么**只有**
  `sessionUi.activeSubOf(sid)` 一个判据，文件管理子标签常驻第一个。下面这段只作历史记录。
- **移动（窄屏）**：子标签栏保留「文件管理」固定子标签 + 终端子标签（没有左栏可用）；
  打开会话默认落在**终端**；面包屑/目录树**不渲染**。
- `sessionUi` 变更：新增 `mainModeOf/setMainMode/toggleMainMode`；**删除** `leftVisible`
  （用户要求去掉折叠按钮）。
- **面板一律保持挂载**：所有 `TerminalPane` / `FileManager` 都用「常挂载 + 非激活加 `.invisible`
  （`visibility: hidden`）」，**不要**用 `v-if`（销毁 xterm 与 WS）也**不要**用 `v-show`
  （`display:none` 下容器尺寸为 0，FitAddon 量不到尺寸、WebGL 会反复 0 尺寸重排）。

### 11.3 左栏（面包屑区）

- **不显示路径**（没有「根目录 / vol1 / 1000 …」那串），只保留按钮条。
- 目录树显示**完整**的远程目录树：以远程 `/` 为根，懒加载展开。
  首次进入会话时**自动展开到登录用户的家目录**并把该节点高亮/滚入视野。
  → **第十六轮起树上也列文件**（每个目录只列前 20 个，超过 20 个时多一行「···更多文件」），
  见 26.4；在此之前它刻意只列目录。
- 「家目录」按钮：全部折叠，只展开登录用户家目录那一条链路。
  → 左栏宽度的拖动方式见 19.1（第九轮起用手柄，不再直接拖分隔线）。
  → **第八轮删了这个按钮，同样的行为改由 FileTree 在两个时机自动做（新开会话 / 重新展开面包屑），
  见 18.1。**
  → **第六轮改了这条件里的按钮组成（去掉「在此目录新建终端」、新增「隐藏面包屑」），见 16.1。**
- **点目录不再跳转文件管理**：只更新「当前目录」（与文件管理器**共用同一个当前目录**）
  并展开/高亮；主区域保持不动。
- 树的右键/「…」菜单补全（目录可用的全部动作）：
  新建 / 上传到此目录 / 在此目录新建终端 / 粘贴 / 重命名 / 删除 /
  压缩为 tar.gz / 导出至飞牛 / 复制完整路径 / 刷新。
  → **第二十三轮把原来的「新建文件夹 / 新建文件」两项并成一项「新建」（打开同一个弹窗，
  在弹窗里选建目录还是建文件），见第 33 节。**

### 11.4 文件管理器

- **排序**：列表视图的「名称 / 大小 / 修改时间」表头可点，循环
  `升序 → 降序 → 取消`（取消 = 回到服务端顺序）。目录始终排在文件前面。
  状态在 `sftp` store 的 `sort: { key: 'name'|'size'|'mtime'|null, dir: 'asc'|'desc' }`，
  持久化到 localStorage。
- **多选**：`SftpState.multi`（第九轮，取代原来的「全选」按钮），详见 19.4。
- **权限显示模式**：工具栏按钮在**符号**（`-rw-r--r--`）与**八进制**（`0644`）之间切换，
  持久化。八进制由前端从符号串换算（`utils/format.ts` 的 `modeToOctal`，含 setuid/setgid/sticky）。
- **更多菜单**：去掉「打开」（行本身仍可点开）；新增
  `复制 / 剪切 / 粘贴 / 压缩为 tar.gz / 解压 / 导出至飞牛 / 在此目录新建终端`。
  菜单顺序：粘贴（有剪贴板时）/ 重命名 / 复制 / 剪切 / 删除 / 下载 / 导出至飞牛 /
  压缩为 tar.gz / 解压（可解压时）/ 复制完整路径 / 在此目录新建终端。
  → **第十六轮**在「粘贴」与「重命名」之间插入「编辑」（只对按扩展名算文本的文件显示），
  顺序变成：粘贴 / 编辑 / 重命名 / 复制 / …（见 26.3）。
- **剪贴板**：`sftp` store 里**全局一份** `clipboard: { sid, op: 'copy'|'cut', paths: string[] } | null`
  （刻意不放进按 sid 分片的 `SftpState`：跨会话判定需要知道「这份剪贴板属于哪个会话」，
  放进分片里这件事在数据结构上就不存在了）。
  - **粘贴一次后清空**（无论成功与否：失败时保留剪贴板并提示原因更友好——本实现取「成功后清空、
    失败保留」并在文档里写明）。→ **第五轮改成「清不清由入口决定」，见 15.6；
    空剪贴板不再显示「粘贴」菜单项，见 19.3。**
  - **不能跨会话粘贴**：`clipboard.sid !== 当前 sid` 时菜单里「粘贴」禁用并提示
    `files_clipboard_cross_session`。
- **导出至飞牛**：把远程文件/目录复制到本机（NAS）共享目录。
  目标目录 = 环境变量 `TRIM_DATA_SHARE_PATHS` 取**第一个冒号之前**的部分
  （等价于 bash 的 `${TRIM_DATA_SHARE_PATHS%%:*}`）；`/api/info` 的 `exportRoot` 下发该值
  （为空/未设置时该菜单项禁用并提示 `files_export_nas_unavailable`）。
  同名冲突返回 `duplicate`（前端提示后可选择覆盖）。
- **压缩 / 解压**（调用远程主机的 `tar` / `unzip`）：
  - 压缩：默认 `tar -czf <输出>/<名字>.tar.gz -C <父目录> <项>`，输出目录固定为
    `/tmp/webssh-archive`（可用环境变量 `WEBSSH_ARCHIVE_DIR` 覆盖，仅服务端可配）。
    完成后前端**跳转到该目录**（因为它在 /tmp 下，用户能立刻看到产物并下载/导出）。
    → **注意：这个目录在「远程主机」上（不是跑本应用的机器）；第六轮起配置目录不可写时会自动
    退到 `<root>-<登录用户名>`，见 16.5。**
  - 解压：在原位解压（`tar -xf` / `unzip -o`，按扩展名识别），同名文件不覆盖（tar 不加 `-k`，
    但先解到临时目录再合并？——简化为：直接用归档工具原样解压，冲突时归档工具自己的行为为准，
    失败/无权限一律回 `permission` / `internal` 并带上命令 stderr 的关键行）。
  - 工具缺失（`tar`/`unzip` 不在）、不支持的后缀、无权限 → 明确错误码 + toast。
- 原「下载」不变：仍然是浏览器下载（`/api/fs/download`）。

### 11.5 新增/变更的后端接口

| 方法 | 路径 | 请求体 | 响应 |
| --- | --- | --- | --- |
| POST | `/api/sessions/open` | `{id, password?, privateKey?, passphrase?}` | `{ok, state}`（无凭据时 `credential_required`） |
| POST | `/api/fs/copy` | `{sid, paths:[], toDir}` | `{ok, files:[{path,size}], skipped}`（同名**跳过**，不覆盖） |
| POST | `/api/fs/move` | `{sid, paths:[], toDir}` | 同上（跨设备无法 rename → `conflict` + 说明，绝不静默降级成复制+删除） |
| POST | `/api/fs/compress` | `{sid, paths:[], name?}` | `{ok, archive:{path,size,dir}}` |
| POST | `/api/fs/extract` | `{sid, path}` | `{ok, dir}` |
| POST | `/api/fs/export-to-nas` | `{sid, path, overwrite?}` | `{ok, export:{path,size,files}}` |

- `/api/info` 的 `runtime` 新增 `archiveDir` 与 `exportRoot`（为空表示不可用）。
- 新错误码：`credential_required`。
- 复制/压缩/导出都要有大小上限与 ctx 取消（沿用现有约束）；所有远程命令参数必须 shell 安全
  引用（单引号转义）。

### 11.6 快捷指令

- ~~会话工具条里的「快捷指令」与「设置」按钮删除（顶栏已有全局入口）。~~
  → **这条被 13.1/13.2 推翻了**：快捷指令按钮**留在会话工具条**（它的作用对象是当前激活终端），
  设置按钮进**首页**工具条；顶栏右侧不再有任何按钮。以 13.1/13.2 为准。
- 弹窗改为**三页**：`常用指令 | 全局 | 专属`。
  - 「常用指令」是内置清单（`frontend/src/data/presetCommands.ts`，按用途分组：系统信息 /
    资源与进程 / 磁盘与目录 / 网络 / 日志 / 服务 / 用户 / Docker），
    每条都标好是否自动执行（只读命令 `auto: true`，会改状态的 `auto: false`）。
    清单里的名字用 i18n key（`qc_p_<id>` 名称、`qc_g_<id>` 分组名）。
    → **第十一轮删掉了「危险操作」分组（`reboot` / `poweroff`）与配套的警示色渲染，
    见 21.2。**
  - 「全局」与「专属」保持现状（增删改 + 上下移动）。
- 弹窗尺寸：**高度固定**为「壳层高 − 应用顶栏（会话标签栏）− 移动端键条」的 **90%**；
  宽度桌面保持现状，窄屏 `95%`；整体居中（用 `.region-center` 定位带）。
  → **第十一轮改成「壳层高 − 应用顶栏」的 90%（不再减底部键条），定位带也不再复用
  `.region-center`，见 21.3；页脚（`发送到 <终端名>` 那一行）同日删除，见 21.4。**

### 11.7 移动端

- 打开会话默认进入终端（桌面与窄屏一致）。→ **第三轮实现曾把窄屏改成「不自动建终端」，
  第五轮改回本节口径，见 15.3。**
- 移动端的输入控件字号统一 **16px**（防 iOS 聚焦自动放大）；xterm 自己的
  `.xterm-helper-textarea` 不在此列（它的字号由终端字号决定，xterm 用内联样式设置）。

---

## 12. 第二轮实施记录与验证（并入本节即可，第 11 节保持为「需求/接口定义」）

### 12.1 后端实现细节（与 11.5 表格的差异都在这）

1. **copy/move 同名不返回 `duplicate`**，而是 `skipped` 计数（与 11.4 正文、前端 `FsOpResult` 一致；
   11.5 表格已同步修正）。copy 是 SFTP 读+写（目录递归），move 优先 `Rename`。
2. **跨设备 move** → `conflict`(409) + 一段说明「可能不在同一文件系统，请改用复制」。
   SFTP 协议层拿不到 errno（sshd 对 EXDEV 只回 SSH_FX_FAILURE），i18n 也没有更合适的码，
   故不自造错误码；**绝不**静默降级成「复制 + 删除」。
3. **压缩**：`mkdir -p <archiveDir> && tar -czf <archiveDir>/<name>.tar.gz …`（`&&` 保证建目录失败
   不会被 tar 的「输出目录不存在」盖掉）；退出码 127 → `no_archive_tool`。自定上限：
   条目数 ≤ 2000（超出 `invalid`，建议改压父目录）、产物 > 4 GiB → 删掉产物并报 `too_large`
   （远端 `/tmp` 通常很小）。同名压缩包按 tar 语义覆盖（默认名带时间戳，实际撞不上）。
4. **解压**：按扩展名识别 tar 家族 / zip，**原位**解压；不支持 → `archive_unsupported`；
   失败信息带上命令 stderr 的关键行。
5. **导出至飞牛**：`TRIM_DATA_SHARE_PATHS` 取第一个冒号之前的部分并 **TrimSpace**（飞牛写这个
   变量可能带空格，带空格路径 stat 必失败且报错像「目录不存在」）；同一会话内的软链接按
   「跳过 + 记日志」处理（链接指向远端路径，落到 NAS 上是死链）。同名且未开 `overwrite` → `duplicate`。
6. **`saveCredential=false` 时私钥指纹（`keyFingerprint`）一并清掉**：指纹由被丢弃的私钥算出，
   留着会让界面显示一把并不存在的密钥。
7. **内存凭据只由「一次成功的 open」写入**：表单里填的凭据不会进内存缓存，否则刚建好
   「不保存凭据」的会话点开就直接连上，与开关的承诺矛盾。`dialRequestForTest` 例外：表单没重填
   凭据时复用内存那份（否则已打开的会话在编辑弹窗里点「测试连接」会报「没有可用凭据」）。
   **第二个例外**（第二十六轮）：会话开着时关掉「保存凭据」，把当前会话正在用的凭据
   carry over 到内存（优先保留 `memCreds` 里那份——它可能是这次 open 当场输入并验证成功的；
   没有才解密旧配置；见 11.1.3 与第 36 节）。这不是「表单凭据入内存」——来源是当前会话
   正在用的那份，且只活到 close；不这么做，新建子终端会立刻报 no_credential。
8. **失败善后用 `context.WithoutCancel` + 30s**：最常见的失败恰恰是「客户端断开」，
   用 `r.Context()` 会永远清不掉半成品文件。
9. **部分更新语义统一**：更新已有会话时 `host`/`user`/`authType` 空串、`port==0`、字段缺省
   一律保留原值（`resolveSessionFields`）。这是修一个真 bug 时统一出来的：`port` 缺省会被
   归一成 22 → 身份判定变化 → 已打开的**非 22 端口**会话改名必回 409 conflict。
10. `internal/testssh` 同时支持「没发 `pty-req` 的 exec 走普通管道」与 `Options.Env`，
    使压缩/解压/「远端缺 tar」这几类用例不依赖 `/dev/ptmx`。
11. 未做：远端磁盘空间预检。

### 12.2 集成阶段发现并修掉的真 bug（第 12.3 节的 e2e 抓到的）

**路径首尾空格被静默裁掉**：`files.go` 的 `resolveRemotePath` 原本对入参 `TrimSpace`。
症状——名字以空格结尾的目录（例如 `evil $(touch x) ' name `）能在 `fs/list` 里正常列出来
（它来自 SFTP 的真实条目），但拿同一个路径去做 list / compress / export / remove **全部**
回 `not_found`：因为查询入口把路径裁成了另一个不存在的名字。裁空格看着「更健壮」，实际是把
合法路径改坏了。现在只把 TrimSpace 用于「整串都是空白」的判断，其余原样交给 SFTP，
并加了回归测试 `TestRemotePathKeepsSurroundingSpaces`（list / export / remove 三个动作）。

### 12.3 端到端验证（`internal/testssh` + 真二进制 + 真 unix socket，58 项断言全过）

夹具特意含病态名字：`dir with space`、`it's a dir`、`evil $(touch x) ' name `（尾空格 + `$()` + 单引号），
用来验证「远程命令里所有插值路径都用单引号引用」这条不是纸面约定。

| 组 | 断言 |
| --- | --- |
| `/api/info` | `archiveDir=/tmp/webssh-archive`、`exportRoot=TRIM_DATA_SHARE_PATHS` |
| 不保存凭据 | `credentialSaved=false` / `hasPassword=false`；**配置文件里没有 password 密文、没有 privateKey/passphrase** |
| 打开 | 无凭据 → 409 `credential_required`；带凭据 → 200 且 `sftpOpen=true`；**内存凭据可复用**（新建终端拿到 tid + `ready`）；**close 后再 open 又要求凭据** |
| 保存凭据 | 缺省 `credentialSaved=true` 且无需凭据即可打开 |
| 部分更新 | 只发 `{id,name}` 改名 200（端口保留）；显式改端口 → 409 conflict；发全字段 → 200 |
| 文件 | list 家目录、mkdir、copy（带空格目录）、同名 copy `skipped=1`、move（带单引号目录）后源消失目标存在 |
| 压缩/解压 | 压缩产物真实存在且 `tar -tzf` 能列出；**压缩病态名字成功且 `$(...)` 没被执行**（`PWNED` 未生成）；tar.gz 与 zip 都原位解压出内容；用普通文件当归档 → `archive_unsupported` |
| 导出至飞牛 | 成功且落在 `exportRoot` 下、文件数与内容正确；同名 → `duplicate`；`overwrite=true` 覆盖成功；单文件导出；病态名字导出成功 |
| 路径校验 | `../` 逃逸被拒；压缩不存在的路径报错 |

`make test`：**84 个用例全过 0 失败**（含 PTY 用例；⚠️ 这是第二轮的快照，当前 119 个 `Test` 函数，
无 PTY 的环境会 skip 11 个，见第 7 节）；`go test -race ./...` 干净；`gofmt -l` / `go vet` 干净。

### 12.4 前端未做「点击级」验证的部分（如实记录）

本轮验证期间浏览器的交互动作被审批策略拒绝，因此以下**只有静态审查 + 类型检查/构建 + 队友的真机冒烟**
（队友在临时实例里跑过 testsshd 并核对了首页/会话视图的渲染与 API 载荷），没有我自己的点击验证：

- 桌面「完整文件管理 ↔ 终端」双向切换；终端列表弹窗 / 凭据输入弹窗 / 快捷指令三页的交互；
- 移动端窄屏形态（浏览器工具无法模拟窄视口与触屏）。

我用**只读**方式确认了渲染结构：首页（顶栏标签「会话」、无搜索框与副标题、卡片含
「不保存凭据」徽标）、会话视图（子标签栏只有终端标签 + `+`、左栏为无路径按钮条 +
以 `/` 为根的完整目录树、工具条只剩 折叠/搜索/粘贴/清屏/重连、文件管理面板的控件
（排序表头 `名称/大小/修改时间`、`权限显示：数字（0644）`、`返回终端`、`全选`、`网格视图`）都在 DOM 里）。
点击级的行为请按第 12.3 节的手测清单人工过一遍。

---

## 13. 第三轮修正（用户对第二轮界面的返工要求）

### 13.1 会话视图（桌面）

- **「完整文件管理」图标按钮取消**：桌面端**恢复固定的「文件管理」子标签**（排在子标签栏最左、
  不参与横向滚动），它与「左栏面包屑/目录树」是**二选一**的关系：
  - 激活的是终端标签 → 显示左栏（面包屑按钮条 + 目录树），主区域是终端；
  - 激活的是 `文件管理` 子标签 → **收起左栏**（文件管理器自带面包屑），主区域是文件管理。
  实现上不需要第二个状态：`sessionUi.activeSubOf(sid) === FILES_TAB` 就是判据
  （`sessionUi` 里那套 `mainMode` 已删除——两套开关会互相打架）。
- 左栏（`BreadcrumbBar`）的按钮条变成 `[家目录] [在此目录新建终端] [刷新]`（不再有路径、
  也不再有「完整文件管理」）。→ **第四轮把家目录简化成纯图标，并加回一个切换按钮（见 14.3）；
  第八轮又把「家目录」与「切换到文件管理」一起删掉，只剩 `[刷新] [隐藏面包屑]`，见 18.1。**
- 文件管理器工具栏里的「返回终端」按钮**删除**（切回终端就点子标签栏里的终端标签）。
  → **第四轮改回，见 14.3。**
- **快捷指令按钮回到会话工具条**（桌面行与移动行都要），作用于当前激活终端——
  用户的原话是「每个子终端标签页都要有快捷指令按钮，否则在弹窗里点指令都不知道输入到了哪个终端」。
  顶栏（app header）里的快捷指令按钮**删除**。

### 13.2 首页

- 删掉「远程会话」标题与「共 N 个会话 / N 个已打开」两个计数徽标。
- 工具条一行：**左对齐** `[添加远程会话] [刷新]`，**右对齐** `[设置]`（从顶栏移过来）。
  → **第五轮把按钮文案改成「添加会话」，见 15.2；第六轮删掉「刷新」、改了两个按钮的样式与
  字号、整栏变矮，见 16.3。**
- 顶栏右侧不再有任何按钮（只剩应用标签）。

### 13.3 会话卡片 / 终端列表弹窗

- **双向同步的是「卡片上的会话名称」与「顶栏标签名」**：所以
  - 编辑表单保存新名字后，要同步把已打开的会话标签标题改掉（`appTabs.syncTitle`）；
  - 顶栏标签双击改名 → 写回配置 → 卡片显示随之更新（已有）。
- `SessionTerminalsDialog` **只做列表与关闭**：显示 `终端 N` + 创建时间 + 单个关闭按钮。
  **去掉**会话名输入框、**去掉**「新建终端」、**去掉**子终端改名。
  → **第六轮把这个弹窗整个删掉，列表直接内联在卡片下（带快捷打开），见 16.2。**

### 13.4 文件管理

- **权限显示模式不再有单独按钮**：点列表视图表头里的「权限」即切换 符号 ↔ 八进制
  （表头其余列照旧是三态排序，两件事别混在一起）。
- **空白处右键菜单（桌面）**：在列表/网格的空白区域右键 → `粘贴 / 打开终端 / 复制路径`。
  → **第四轮在最前面补了「新建文件 / 新建文件夹 / 上传」（见 14.4），第五轮补「信息」（15.5），
  第七轮补「刷新」（17.8）；第二十三轮把前两项并成一项「新建」（见第 33 节）。**
  - `打开终端` = 在当前目录新建终端（`terminals.openTerminalAt(sid, state.dir)`）；
  - `复制路径` = 复制当前目录的完整路径到剪贴板；
  - `粘贴` 在剪贴板为空或来自其它会话时置灰并说明原因（沿用现有规则）。
    → **第九轮改成：剪贴板为空就整项不出现（行菜单与空白菜单统一），只有「来自别的会话」才
    置灰说明原因，见 19.3。**
- **菜单补 `压缩为 zip`**（与「压缩为 tar.gz」并列）；**目录也能复制/剪切/粘贴**
  （后端 copy/move 本来就支持目录，前端不得对目录禁用这几项）。
- **失败必须报出来**：权限不足、压缩失败、解压失败、粘贴失败都要把后端的错误文案
  放进 toast（`describeError`）；不允许静默失败或只留一行控制台。
- **修一个真 bug**：`FileManager` 引用了 `t('files_copy')`，而这个 key 从来不存在 →
  界面上「复制」显示成裸 key `files_copy`（用户反馈的「复制缺少中文」）。
  本轮补上 `files_copy`，并把 `scripts/check-i18n.mjs` 升级为**双向检查**
  （字典之间一一对应 **+** 代码里静态引用的 key 必须存在），这类漏文案以后会被 CI 拦住。

### 13.5 后端

- `POST /api/fs/compress` 新增 `format`（`tar.gz` 缺省 / `zip`）：
  - `zip`：`mkdir -p <archiveDir> && cd <父目录> && zip -qr <out> <名…>`（多个条目分属不同父目录时
    依次 `cd` 追加进同一个包）；远端缺 `zip` → `no_archive_tool`；归档名按 format 补后缀。
  - 权限不足/工具报错（tar/zip 非 0 退出）→ `permission`（含 stderr 关键行）或对应的 existing 码。
- 其它接口不变。

### 13.6 冻结文件（本轮）

`frontend/src/stores/sessionUi.ts`（`mainMode` 已删）、`frontend/src/serverapi/index.ts`
（`fsCompress(..., format?)` + `ArchiveFormat`）、`frontend/src/i18n/*`（新增 `files_copy`、
`files_compress_zip`、`files_open_terminal`）、`frontend/scripts/check-i18n.mjs`（双向检查）。

### 13.7 zip 压缩的实现细节（backend 实测后补记）

- 命令是 **`zip -qry`**，不是契约 13.5 里写的 `-qr`：不加 `-y` 时（Info-ZIP 3.0 实测）
  指向目录的软链接会把**链接指向的整棵树**打进包、失效的软链接被**静默丢弃**（只 warn、退出码 0）、
  只选中一个失效链接时 zip 直接以退出码 12 失败。加 `-y` 后链接按链接本身存，与 tar 侧
  「不跟随链接」的口径一致。
- zip 是**追加/更新**语义，所以同名产物**先删后建**（否则上一次的条目会留在包里）；
  同名目标若恰好是**目录**则拒绝（`invalid`）；失败清理**只删非目录产物**——
  顺手堵掉了「清理半成品时把同名目录递归删掉」这条最不能接受的失败路径。
- 包名自带别的归档后缀时**替换**而不是叠加（`docs.tar.gz` + zip → `docs.zip`）；
  只剩后缀的名字（`.zip`）拒绝（否则产物是个隐藏文件）。
- 条目名取 basename（`cd <父目录>` 的自然结果）：**不同父目录下的同名条目会互相覆盖**
  （zip 按名字更新，以最后一张为准）。tar 侧没有这个问题（多父目录用 `-C /` + 相对路径）。
- 权限不足（tar / zip / unzip 非 0 退出且输出含 permission 字样）→ `permission`(403)；
  缺工具（127/命令不存在）→ `no_archive_tool`(502)；其余 → `internal`(500)，都带输出关键行。
  单测覆盖 tar/zip/mkdir 的中英消息 + 集成用例「archiveDir 不可写」两个格式各一次
  （以 root 跑时该用例会 skip 并写明原因）。
- `format` 只认 `tar.gz` / `zip`（大小写敏感，`tgz`/`ZIP` 一律 `invalid`——不猜，
  否则「前端发错字段值」永远查不出来）。

---

## 14. 第四轮修正（用户对第三轮界面的返工要求）

### 14.1 会话表单：编辑时必须回填已有备注（真 bug）

- `SessionFormDialog` 的 `reset()` 原来把 `note` 写成空串，而备注是**整份提交**的普通字段
  （`buildInput` 里 `input.note = note.value.trim()`，空串 = 清空）。症状：点开一张有备注的会话
  卡片的「编辑」，备注框是空的；此时保存（哪怕只改了个名字）就把配置里的备注清掉了。
- 现改为 `note.value = s?.note ?? ''`。这是**数据丢失级**的坑，改 `reset()` 时不许再把它写死成空。

### 14.2 设置弹窗：删掉「数据目录」一栏

- 删掉 `settings_data_dir` 的标签与路径展示（该 i18n key 两套字典一起删），只保留同一节里的
  **飞牛宿主状态**——它是「飞牛文件」上传与「导出至飞牛」是否可用的唯一自查处。
  顺带删掉同样只服务那一行的 `common_none`（两套字典），不留没人引用的文案。
- `/api/info` 仍然下发 `runtime.dataDir`（后端契约不变），只是前端不再展示。

### 14.3 会话视图：文件管理在前台时隐藏终端工具条；两个方向各一个切换按钮

- **文件管理子标签在前台时**（`filesActive`，判据仍是 `activeSubOf(sid) === FILES_TAB`），
  子标签栏里的**折叠/展开功能名**按钮与**搜索 / 粘贴 / 清屏 / 重连 / 快捷指令**按钮整条隐藏：
  桌面行 `v-if="!filesActive"`，移动端那条工具行整行 `v-if="!filesActive"`。
  理由：这五个动作的作用对象是「当前激活终端」，文件管理在前台时它按设计不存在
  （`activeTermUid === null`），留一排禁用按钮只是噪音。
  这几个按钮没有需要保留的 DOM 状态，所以这里用 `v-if` 是安全的——文件头第 3 条禁 `v-if`
  针对的是**面板**（xterm / WS / FileManager 都要保持挂载）。
- **左栏按钮条**（`BreadcrumbBar`）变为
  `[家目录（纯图标）] [切换到文件管理] [在此目录新建终端] [刷新]`：家目录按钮按要求去掉文字，
  说明挂 `title` / `aria-label`。
- **文件管理器工具栏最左侧新增「返回终端」**（**第八轮删掉左栏那个「切换到文件管理」之后它仍然保留**，
  见 18.1）：切回 `terminals.lastActiveOf(sid)`
  （本会话**上一次激活**的终端，没有就退回第一个标签），桌面端左栏随之回来；
  一个终端都没有时按钮禁用（此时文件管理是唯一能显示的主区域，见 13.1 / SessionView 规则 4）。
- 两个切换按钮共用**同一个状态**：都只写 `sessionUi.setActiveSub`，没有第二套「主区域模式」
  （13.1 的判据不变）。`切换到文件管理` 写 `FILES_TAB`，`返回终端` 写终端 uid。
- store 变更：`terminals` 新增只读 accessor `lastActiveOf(sid): TermTab | null`——**「切回哪个
  终端」这类导航**用它；`activeOf` 仍然是「指令发给谁」的唯一判据（文件管理在前台时必须返回 null）。
- 新 i18n key（两套字典）：`files_switch_to_files`、`files_switch_to_terminal`。

### 14.4 文件管理空白处右键菜单补三项

- 桌面空白处右键菜单顺序改为（用户说的「新建目录」= 界面上的「新建文件夹」，文案沿用工具栏
  那套 `files_new_dir`，不另造一套说法）：
  `新建文件 / 新建文件夹 / 上传 / 粘贴 / 打开终端 / 复制路径`。
- 「新建文件 / 新建文件夹」复用工具栏那套 `newEntry` + `PromptDialog`（父目录 = 当前目录，名字
  校验同前）；「上传」打开工具栏那个 `UploadDialog`（目标目录 = 当前目录）。
- 行内「更多」菜单不变（11.4 / 13.4），粘贴的「置灰 + 说明原因」规则也不变。
  → **第二十三轮把上面那两项并成一项「新建」，`PromptDialog` 换成专门的 `NewEntryDialog`，见
  第 33 节（行内「更多」菜单里从来没有「新建」，不受影响）。**

### 14.5 修复网格视图的「整列悬停高亮」

- 症状（用户上报 + 截图）：网格视图下目录里只有一行条目时，鼠标停在任意格子上，高亮是
  **贯穿上下的整列**，看起来像「整列都被预选中」。
- 原因：网格容器是 `grid min-h-full`（占满滚动区），而网格默认的 `align-content` 会**拉伸自动行**
  去填满容器高度 → 那一行被拉成整屏高，格子的 `hover` 背景与边框（`rounded-md`）也就跟着满高。
- 修法：容器加 `content-start`（`align-content: flex-start`），行只占内容高度、多余空间留在底部。
  **别删这个类**；`min-h-full` 要留着（空白区域要属于网格容器，右键/拖拽都落在它上面）。

### 14.6 本轮的验证方式与结论

- `npm run type-check` + `npm run check:i18n`（392 key 一一对应）+ `make`（前端 dist → embed → 二进制）
  全绿；后端 `go test ./...` 全绿（本轮没动后端，跑一遍确认仓库状态没被带坏）。
- **浏览器实测**（真二进制 + `testsshd` + `socat` 桥，1280×720）：本轮的环境只允许**只读观测**
  （`target.open` / `observe.read` / `observe.screenshot`），点击类动作被策略拒绝。因此下面这些
  界面状态是用**一次性 harness 开关**把页面直接置到目标状态后截屏确认的；开关只改「初始状态」
  （跳过自动建终端、强制网格视图、强制打开某个弹窗、把激活子标签放回文件管理），**不改被验证的
  逻辑**，验完已全部移除（`grep HARNESS-R4 frontend/src` 为空、`git diff` 只含本轮的真改动）：
  - `?edit=1`：编辑弹窗里**备注回填了**「备注：机房 A1 / 机柜 3」（14.1）；
  - `?settings=1`：设置弹窗里**没有「数据目录」一栏**，飞牛状态那行还在（14.2）；
  - `?files2=1`：文件管理子标签在前台 → 桌面工具条（» / 搜索 / 粘贴 / 清屏 / 重连 / 快捷指令）
    **整条消失**，标签栏只剩 `文件管理` + 终端标签 + `+`（14.3）；
  - `?grid=1&blank=1`：文件管理工具栏最左侧的「返回终端」按钮在位（有终端时可点）、
    网格里 7 个条目排成一行且单元格只有内容高度；空白处菜单是
    `新建文件 / 新建文件夹 / 上传 / 粘贴（无剪贴板时置灰）/ 打开终端 / 复制完整路径`（14.4 / 14.5）；
  - `?stretch=1`（负向对照）：把网格的 `align-content` 还原成 `stretch` 并给单元格涂上 hover 那种
    底色 → 单元格变成贯通的整列，与用户上报的截图完全一致；同一次观测里单元格实测
    **112×540**，而正常（`content-start`）是 **112×82**（网格容器 1280×556）。这条对照说明
    「整列高亮」的成因就是行被拉伸，`content-start` 正是解药。
- **没有被自动化点过的部分**：两个切换按钮与空白菜单项的**点击**本身（无法点击）。它们的写入点
  与既有可用路径同源——都是 `sessionUi.setActiveSub`、`sftp.createFile/createDir`、工具栏那套
  `UploadDialog`，且按 `?files2=1` 观测到的界面与「点『切换到文件管理』后应该看到的界面」一致。
  交付前建议手点一遍：左栏 ⇄ → 文件管理；文件管理 ⇄ → 回到刚才那个终端；再点空白处右键的三项。

---

## 15. 第五轮修正（用户对第四轮界面的返工要求）

### 15.1 设置弹窗：飞牛宿主只在「不可用」时提示

- `settings_trim_ok`（「飞牛宿主可用：可直接从飞牛选择文件上传」）整条删掉，两套字典一起删。
- 那一块现在**只在有问题时渲染**（`trimError` 或 `trim.available === false`）：可用时整块不出现，
  加载中也不出现（一次很快的请求，闪一行「加载中」纯属噪音）。
  「飞牛文件」上传与「导出至飞牛」是否可用仍然靠它自查——不可用时**必须**说清原因。

### 15.2 首页：按钮文案与卡片上的状态

- `home_new`：`添加远程会话` → **`添加会话`**（工具条与空态按钮共用这一个 key），
  空态说明里的引用一并跟着改。
- **会话卡片上的状态灯、「未打开/已打开」、打开时间、最近使用时间全部去掉**：打开与否一眼能
  从「打开/关闭」按钮看出来，那两个时间没人看。现在唯一的实时指示是 **`N 个终端` 按钮**
  （会话没打开时连它也不显示）。
- 连带删掉：`SessionCard` 的 `now` prop、`HomeView` 里那个 30s 走字定时器（只为相对时间存在），
  以及随之无引用的 `home_opened_at` / `home_last_used` / `home_never_used` 三个文案。
  `home_opened` / `home_closed` **保留**——顶栏应用标签的 title 还在用（AppTabBar）。

### 15.3 移动端：打开/恢复会话也自动进「终端 1」

- 第三轮的实现里，窄屏打开会话**不**自动建终端（当时的注释是「避免一进会话就占满屏幕」），
  于是手机上打开会话落在**文件管理**子标签上——用户明确要求改掉。
- 现在 `HomeView.openWith` 与 `SessionView` 的 onMounted（用户第一次进入恢复的会话）**都不再判
  `isMobile`**：一个终端都没有就 `terminals.add(sid)`，桌面与窄屏一致（这与 11.7 的原口径一致）。
  第二十五轮起「恢复」不再等于「进入」：启动时只建标签，终端 1 在进入会话时补（见第 35 节）。

### 15.4 「返回终端」必须回到**你刚才用的那个终端**（真 bug）

- 症状（用户上报）：在终端 2 里点「文件管理」，再点「返回终端」，却跳回了终端 3。
- 成因：切终端子标签只写 `sessionUi`（唯一真相），`terminals` 里的 `activeUid` **从来没被更新**，
  它只在 add / ensure / 关标签时被写过——所以它一直停在「最后建出来的那个标签」上，
  而第四轮的 `lastActiveOf` 正是读它。
- 修法两条一起：
  1. `terminals.setActive` 导出去，`SessionView.activateTerm` 改走它（两个 store 一起写，
     单一真相仍然是 sessionUi）；
  2. `lastActiveOf(sid)` 以 `sessionUi` 为准：此刻激活的是终端就用它，激活的是文件管理才退回
     `activeUid` 记下的那一个。
- `activeOf` 的语义**不变**（文件管理在前台时返回 null），它仍然是「指令发给谁」的唯一判据。

### 15.5 「信息」（新增接口 + 弹窗）

- 接口：`POST /api/fs/info`，请求 `{sid, path}`，响应 `{ok, info: FsInfo}`：
  ```jsonc
  FsInfo = FsEntry + { dirCount, fileCount, countError? }   // 见 4.2 / 4.3
  ```
  - **只数直接子项**（用户确认：不要递归）。递归在大目录上要跑很久，而这个弹窗没有进度语义。
  - 子项的「目录/文件」口径与列表**一致**：软链接**跟随**（指向目录的算目录、失效的算文件），
    只为软链接多发一次 Stat，普通文件/目录不花这个往返。
  - 读不出子项（多半是权限）时**不报错**，把 `countError` 带上：界面如实写「统计失败：<原因>」，
    绝不把 0 显示成「这是个空目录」。
  - 条目属性用 `Lstat` + 既有的 `buildFsEntry`（软链接的 `symlink`/`broken` 与列表同一个口径）。
  - 错误码沿用 `/api/fs/list`：`invalid`(400) / `not_found`(404) / `session_gone`(404) / `not_open`(409)。
- 前端：`InfoDialog`（`components/files/`）显示**类型、名称、路径、大小、修改时间、权限、拥有者、内含**
  （拥有者是第七轮加的，见 17.7）。
  - 大小是**条目自身**的大小（目录就是它自己的 inode 大小），旁边标明「不含子目录内容」；
  - 权限**两种写法都显示**（`-rw-r--r--（0644）`，八进制由既有的 `modeToOctal` 换算，认不出来
    就只显示符号串，不编造 0xxx）；
  - **没有「创建时间」**：SFTP v3 的 attrs 只有 mtime/atime，用户确认不要了（不跑远程 `stat`）。
- 入口：行菜单（「···」/右键）的 `信息` 作用于该条目；**空白处右键菜单的 `信息` 作用于当前目录**
  （空白处 = 对这个目录做事，与 13.4 的口径一致）。

### 15.6 剪贴板常驻 + 地址栏下的剪贴板栏

- **清不清由入口决定**（用户明确要求两种入口保持各自现状）→ **第十一轮给「剪切」加了例外，
  见 21.1**：
  - 右键/空白菜单里的「粘贴」（`sftp.paste` 缺省）：成功后清空 —— **现状不变**；
  - 新增的常驻剪贴板栏里的「粘贴到此处」（`paste(sid, toDir, keepClipboard = true)`）：**不清空**，
    只有点「取消」（`sftp.clearClipboard()`）才清。
  - 失败、以及「一项都没贴上」时一律保留剪贴板（原来就是这样）。
- ~~「常驻 + 剪切」多做一步：剪切粘贴是「搬走」，粘完之后剪贴板里的源路径已经不存在，
  下一次粘贴只会报 `not_found`；所以把剪贴板里的路径**映射到新位置**（被同名跳过的那些没搬，
  保持原路径）。这样再粘一次就是「把这东西再搬过去」，语义自洽。~~
  → **第十一轮删除**：剪切粘完直接清空剪贴板，不再重映射路径（见 21.1）。
- 剪贴板栏放在**地址栏下面**（先回答「我在哪」，再回答「手上拿着什么」）：
  `[图标] 已复制 N 项到剪贴板 / 已剪切 N 项到剪贴板 | 名字（截断） | [粘贴到此处] [取消]`。
  - 只在剪贴板属于**本会话**时显示；来自别的会话的不显示（跨会话粘贴是硬约束，
    菜单里那句「不能跨会话粘贴」已经把话说清了，显示一行只能置灰的按钮不如不显示）；
  - 当前目录还没定位到时「粘贴到此处」禁用。
- store 变更：`paste(sid, toDir, keepClipboard?)` 多一个参数、新增 `clearClipboard()`。

### 15.7 移动端：菜单点别处能关掉（真 bug）

- 症状（用户上报）：手机上打开文件/目录的「更多操作」菜单后，点其他地方菜单不消失。
- 成因：关菜单靠的是 `window` 上的 `click` 监听；触屏上 tap 出来的 click 未必会到达 window
  （滚动判定的手指微动、别处的 `stopPropagation` 都会吃掉它）。
- 修法：改成 **`pointerdown` + 捕获阶段**（鼠标与触摸一套逻辑，且先于任何冒泡处理器执行）；
  落在菜单自己身上的 pointerdown 不算「别处」（那一击留给菜单项），因此「新建文件/目录」
  这类会开弹窗的项要**自己显式** `hideMenu()`（原来靠 click 监听顺带关掉）。
  `hideMenu()` 顺手摘掉 window 上的监听，不会一次打开留一个监听器。

### 15.8 冻结文件（本轮）

`frontend/src/serverapi/index.ts`（`FsInfo` + `api.fsInfo`）、`frontend/src/i18n/*`
（新增 `files_info*`、`files_paste_here`、`files_clipboard_cancel`；删 `settings_trim_ok`、
`home_opened_at`、`home_last_used`、`home_never_used`；`home_new` 改文案）、
`frontend/src/stores/sftp.ts`（`paste` 多一个参数、新增 `clearClipboard`）、
`frontend/src/stores/terminals.ts`（导出 `setActive`）。

### 15.9 本轮的验证

- `npm run type-check` + `npm run check:i18n`（400 key 一一对应）+ `make` + `make test` 全绿。
- 后端新增两个用例：
  - `TestFsInfo`：文件/目录/家目录的属性与计数、**软链接跟随**（指向目录的算目录）、
    以及 `invalid` / `not_found` / `session_gone` 三种错误码；
  - `TestFsInfoCountError`：0000 目录 → 带回 `countError`、且计数保持 0（不当「空目录」）。
    这个用例会**先验证前提**（真的 ReadDir 失败），前提不成立就 SKIP 并写明原因：以 root 跑、
    或文件系统不认权限位（飞牛卷实测会把 mode 改写成 0705）时会跳过——`make test` 导出的
    `TMPDIR=$(ROOT)/.tmp` 正好落在那样的卷上，所以走 `make test` 时它是 SKIP，
    在系统 /tmp 上跑 `go test` 时是真跑真过的。
- 浏览器只读实测（真二进制 + testsshd + socat，1280×720），凡是「点下去之后」的状态都用一次性
  harness 开关摆出来看，验完已移除（`grep HARNESS-R5 frontend/src` 为空）：
  - **返回终端回到对的终端**（15.4）：探针跑「建到 3 个终端 → 用新写法激活终端 2
    （`terminals.setActive`，`SessionView.activateTerm` 现在走的那条）→ 切到文件管理」，
    结果是 `back=终端 2 backIsSecond=true lastTab=终端 3`；
    负向对照（`via=legacy`，只写 sessionUi 的旧写法）→ `back=终端 1 backIsSecond=false`，
    也就是「返回终端指向了一个用户没在用的终端」，正是用户上报的现象。
  - **剪贴板语义**（15.6）：`barPaste(count=1)->stillInClipboard=true` +
    `menuPaste(count=1)->clipboardCleared=true`（两次粘贴都是真的调后端复制，远端目录里能查到
    落下的文件）；`clearClipboard()` 之后那行剪贴板栏消失。
  - **菜单点别处关闭**（15.7）：打开行菜单后往 `document.body`（菜单外）派发 pointerdown →
    菜单消失；往菜单自己身上派发 → 菜单留着（这一击要留给菜单项）。
  - **信息弹窗**（15.5）：文件（类型/名称/路径/大小=0 B（不含子目录内容）/修改时间/权限
    `-rwx---r-x（0705）`）与**当前目录**（多一行「内含 2 个目录、2 个文件」，与列表一致）各一张；
    空白处菜单与行菜单里都出现了「信息」；接口直连也验过：目录回 `dirCount/fileCount`、
    文件回 0/0、不存在的路径回 `not_found`。
  - **设置弹窗**（15.1）：把 `/api/trim/status` 的响应临时伪造成「可用」→ 那块整块消失；
    恢复真实响应（本机没有 TRIM_API_TOKEN）→ 「飞牛宿主不可用：…」那一行在。
  - **首页**（15.2）：按钮是「添加会话」，卡片上只剩「1 个终端」这一个指示（没有状态灯、
    没有「已打开/未打开」、没有打开时间与最近使用时间）。
- **没能自动化验证的部分**（如实记录）：
  1. 窄屏/触屏形态（15.3 的「移动端自动进终端 1」与 15.7 的真实手指 tap）：本会话的浏览器没有
     视口/触摸模拟，`act.*`（点击、滚动、宽高调整）一律被策略拒绝。15.3 的改动是「删掉两处
     `!isMobile` 判断」，也就是说窄屏现在与桌面走**同一条**代码路径（桌面那条已实测）；
     15.7 验证的是同一个监听器（pointerdown + 捕获）；
  2. 两个切换按钮、菜单项、剪贴板栏按钮的**点击**本身没有被自动化点过——它们调用的函数都被
     直接调用验过（`lastActiveOf` / `paste(..., true)` / `clearClipboard` / `/api/fs/info`），
     没验的只是「按钮 → 那个函数」这一跳。
- 顺带查清一个**既有现象**（非本轮引入，也没有改）：桌面端进会话时控制台会有一条
  `/api/fs/list` 403。原因不是 bug——目录树首次要「从 `/` 一路展开到家目录」，而本机的
  `/vol1`、`/vol1/1000`（飞牛卷根）对当前账号不可读，`expandPath` 按设计停在拉不到的那一层
  （见 stores/sftp.ts 里那段注释），浏览器只是如实报了这次被容忍的失败。

---

## 16. 第六轮修正（用户对第五轮界面的返工要求）

### 16.1 左栏：去掉「在此目录新建终端」，新增「隐藏面包屑」/「展开面包屑」

- `BreadcrumbBar` 的按钮条变成 `[家目录（仅图标）] [切换到文件管理] [刷新] [隐藏面包屑]`：
  「在此目录新建终端」删掉（文件管理器工具栏与目录树右键菜单里都有，重复）。
- 左栏的可见性现在是**三个条件的交集**（写在 `SessionView` 的 `showLeft` 里）：
  1. 桌面端（移动端没有左栏）；
  2. 激活的是终端标签（`!filesActive`，即 13.1 那条「与文件管理子标签二选一」）；
  3. **用户没有手动收起它**（`sessionUi.leftHiddenOf(sid)`，第六轮新增）。
  这**不是**第二个「主区域模式」：主区域显示什么仍然只由 `activeSub` 决定，这里只是把一个
  「用户偏好」叠加到左栏上（第二轮那版 `mainMode` 被删是因为它自己能决定主区域）。
- 收起之后，**子标签栏最左侧**（`文件管理` 子标签左边）出现一个「展开面包屑」图标按钮：
  点一下左栏回来、按钮自己消失（它只在 `!isMobile && !filesActive && leftHidden` 时渲染）。
- `sessionUi` 新增：`leftHidden`（按 sid）、`leftHiddenOf(sid)`、`setLeftHidden(sid, hidden)`；
  `forget(sid)` 一起清掉。

### 16.2 首页卡片重设（内联子终端列表）

- **右上角三个仅图标按钮**（关闭/打开、编辑、删除）：无边框（`g-btn-ghost` 覆写成 `!h-8 !w-8 !px-0`），
  与**会话名同一行、不换行**；名字放在 `min-w-0 overflow-x-auto whitespace-nowrap` 的容器里，
  放不下就横向滚动（而不是把按钮挤走或换行）。打开/关闭仍是同一格：会话没打开时显示「打开」。
- **不再标注认证方式**（密码/密钥两个徽标删掉）。「未保存凭据」那个提醒徽标保留——它不是
  「密码/密钥」的标注，而是「下次连接要重新输入」这个事实。
- **打开的子终端直接列在卡片下**（不再藏进弹窗）：每行是状态点 + `终端 N（+自定义标题）` +
  创建时间（绝对时间，为了不在首页再养一个 30s 走字定时器）；**整行可点 = 快捷打开**
  （切到该会话的应用标签 → 激活那个子终端 → 把键盘焦点交给它），行尾保留单个关闭按钮
  （先确认：它会终止远端的 shell，沿用 `termlist_close_confirm_*` 两句话）。
- `SessionTerminalsDialog.vue` **删除**；`termlist_title` / `termlist_empty` 两条文案随之删除，
  `termlist_created` 改为内联行用 `formatTime`（不再需要相对时间）。
- 卡片事件：`@open-terminal(uid)` / `@close-terminal(uid)`（HomeView 接线）；`terminal-count`
  prop 换成 `tabs: TermTab[]`。状态点配色抽成 `terminals.termStatusDotClass()` 供卡片与子标签栏
  共用一份（同一个终端在两处不能是两种颜色）。

### 16.3 首页工具条

- 删掉「刷新」按钮（每次进首页都会 `home.load()`，留个按钮只是噪音；加载失败的「重试」还在）。
- 「添加会话」不填充颜色：`g-btn-ghost` + `!text-brand`（品牌色图标 + 文字，无底色无边框）。
- 「设置」补上描边：`g-btn-secondary`。
- 两个按钮字号统一 `text-sm`（14px），高度 `!h-9`，工具条 `py-3 → py-2`（整栏高度随之降下来）。
- 随之无引用的文案：`home_refresh`、`common_refresh` 删除。

### 16.4 「清空会话名称保存后没生效」（真 bug）

- 症状（用户上报）：把会话名称清空后保存，界面上还是旧名字。
- 成因：后端 `save` 里 `if in.Name != nil && TrimSpace(*in.Name) != ""` 把「显式空串」也当成了
  「保持原值」，于是只有「配置里从没起过名字」那个分支才会写入。凭据与备注早就是「显式空串 =
  清除」，名字是唯一的例外。
- 修法：`if in.Name != nil { sc.Name = TrimSpace(*in.Name) }`，只有**新建**且没给名字时才落
  `user@host`；展示统一走 `sessionConfig.displayName()`（空 → `user@host`，与前端
  `sessionLabel` 一致），重复会话的错误文案也用它。
- 同时去掉前端的「缓存写回」：`home.renameSession` 只发 `{id, name}`（原来会把本地缓存的
  host/port/user/authType 一起发回去，可能覆盖别的设备刚做的改动）；`SessionInput` 的
  `host`/`user`/`authType` 因此变成「更新时可省」（见 4.3），新增时仍然必填。

### 16.5 压缩提示「权限不足」是怎么回事（用户提问 + 修复）

- **先回答问题**：压缩包是**在远程主机上**生成的（`tar`/`zip` 由远程 shell 跑），输出目录默认是
  **那台远程主机**的 `/tmp/webssh-archive`——与运行本应用的机器无关。远端 `/tmp` 一般世界可写，
  所以「/tmp 是不是都权限不足」的答案是否定的；最常见的原因是**那个目录已经存在且属于别的账号**
  （典型是历史上以 root 跑过一次），当前登录用户既写不进去、也删不掉。少数主机确实把 `/tmp` 挂得
  很严、或者登录用户被 chroot 在没有 /tmp 的环境里。
- 修法（`resolveArchiveDir`）：打包前先用一次远程探针决定输出目录——
  1. 配置的目录可写 → 用它（现有行为不变）；
  2. 不可写 → 退到同级的 **`<root>-<登录用户名>`**（自己建的目录，不会与别的账号撞车，
     也不会去动别人已有的东西）；响应里的 `archive.dir` 是**实际**用到的目录，前端跳到那儿；
  3. 两个都不行 → `permission` + 一段说得清的文案（点名两个目录、说明是远程、给出
     `WEBSSH_ARCHIVE_DIR` 这个开关，并附上原始输出）。
- 探针**只用 shell 内建**（`command -v` / `[ -w ]` / `printf`）：远端 PATH 坏掉时探针给不出结论，
  这时**不能**顺手报「目录不可写」——那会把真正的 `no_archive_tool`（没有 tar）误报成权限问题，
  所以探针没有结论时按配置目录继续，让后面的打包命令去失败并报告（并落一条 `warnf`）。
- 前端：「站在压缩包输出目录」那条说明现在用**最近一次压缩的实际目录**判断（`sftp` store 新增
  `lastArchiveDir(sid)`），因为实际目录可能不是 `/api/info` 里那个配置值。

### 16.6 冻结文件（本轮）

`frontend/src/stores/sessionUi.ts`（`leftHidden` 一族）、`frontend/src/stores/terminals.ts`
（导出 `setActive` + 新增 `termStatusDotClass`）、`frontend/src/serverapi/index.ts`
（`SessionInput` 的 host/user/authType 更新时可省）、`frontend/src/i18n/*`（删 7 条、加 4 条）、
`frontend/src/utils/*` 未改。

### 16.7 本轮的验证

- `npm run type-check` + `npm run check:i18n`（397 key）+ `make` + `make test` 全绿。
- 后端新增/改写的用例：
  - `TestSessionInputSemantics` 补「name 空串 = 清除」「name 缺省 = 保持原值」两条断言；
  - 新增 `TestFsCompressArchiveDirFallback`：归档目录不可写 → 压缩**成功**且产物落在
    `<root>-<用户>`，原目录不被动过（权限位对 root 无效、或文件系统不认 mode 时 skip 并写明）；
  - `TestFsCompressPermissionDenied` 改成「两个目录都建不出来」的确定性构造
    （归档目录落在一个普通文件下面），断言错误码是 `permission`、文案里点名两个目录 +
    `WEBSSH_ARCHIVE_DIR` + 「远程主机」；
  - `TestFsCompressMissingTool` 保持通过（证明探针没有把「没有 tar」误报成权限问题）。
- 浏览器只读实测（真二进制 + testsshd + socat，1280×720）：凡是「点下去之后」的状态都用一次性
  harness 开关（`?home=1`、`?terms=N`、`?hideleft=1`）摆出来截屏看，验完已移除
  （`grep HARNESS-R6 frontend/src` 为空、`./webssh` 里搜不到 harness 的字符串）：
  - **卡片**：会话名与右上角三个**无边框**图标按钮同一行（电源 / 铅笔 / 垃圾桶），没有密码/密钥
    徽标，也没有状态灯与时间；打开的子终端**内联**成三行（状态点 + `终端 N` + 创建时间 + ×）；
  - **首页工具条**：「添加会话」是品牌色文字+图标（无底色）、右侧「设置」有描边、没有「刷新」，
    两个按钮都是 14px、整栏比上一版矮；
  - **面包屑按钮条**：`[家目录][⇄][刷新][隐藏面包屑]`——「在此目录新建终端」确实没了；
  - **左栏收起**：`?hideleft=1` 时左栏整条消失、子标签栏最左侧（`文件管理` 左边）出现
    「展开面包屑」图标按钮；不收起时那个按钮不出现。
- API 层实测（同一份后端代码）：
  - **压缩目录回退**：`WEBSSH_ARCHIVE_DIR` 指到一个普通文件（无法当目录用）→ 压缩**成功**，
    响应 `archive.dir` = `<root>-demo`，产物确实落在那儿；指向 `/root/webssh-archive`
    （远端两个目录都不可写）→ `permission` + 「远端压缩包输出目录不可写：…（备用的 … 也不行）…
    请给远端登录用户写权限，或用环境变量 WEBSSH_ARCHIVE_DIR 换一个可写目录」，并附原始输出；
  - **清空名称**：`{id, name: ""}` → `name` 变空且 host/port/user/authType 原样保留
    （这正是 `home.renameSession` 现在发的形状）；只发 `{id, note}` → 名字保持为空（不被塞回默认名）；
    再发一个名字又能设回去。
- **没能自动化的部分**：所有按钮/菜单项的**点击**本身（本会话的浏览器只允许只读观测，`act.*`
  一律被策略拒绝）。这些按钮调用的函数都被直接验过（`setLeftHidden` 的效果、`renameSession` 的
  请求形状、`resolveArchiveDir` 的两条分支），没验的只是「按钮 → 那个函数」这一跳；窄屏/触屏
  形态同样没法在本会话模拟。

---

## 17. 第七轮修正（用户对第六轮界面的返工要求）

### 17.1 标签：非激活时常驻灰底 + 关闭按钮常驻

- `style.css` 的 `.app-tab-idle`（顶栏应用标签）与 `.subtab-idle`（会话内子标签）加上**常驻**的
  浅灰底（`bg-black/5` / `dark:bg-white/5`），悬停再深一档（`hover:bg-black/10`）——标签栏里
  一眼能看出有几个标签、边界在哪。激活态仍是品牌色，不变。
- 两个标签的关闭按钮去掉 `.hover-reveal`，**常驻显示**（触屏上本来就常显；桌面端原来只有悬停
  才出现，现在一视同仁）。
- `.hover-reveal` 仍然留给文件管理器行内的「···」（那里的取舍没变）。

### 17.2 卡片：关闭按钮红色

- 首页卡片右上角那个「打开/关闭」共用的图标按钮，在**会话已打开**（即当前动作是关闭）时用
  `!text-danger hover:!bg-danger/10`；未打开（动作是打开）时保持中性色。

### 17.3 首页工具条：「添加会话」补描边

- `g-btn-ghost` → `g-btn-secondary` + 保留 `!text-brand`：有边框、不填色，仍是这一栏的主操作。

### 17.4 编辑弹窗 / 设置弹窗：标题与关闭按钮不参与滚动

- 面板从 `overflow-y-auto p-5` 改成 `flex flex-col`：头部 `shrink-0 border-b px-5 py-3`、
  正文 `min-h-0 flex-1 overflow-y-auto px-5 py-4`。会话表单的**页脚**同样固定
  （`shrink-0 border-t px-5 py-3`）——滚的只有中间那段。
- 这是所有长弹窗的通用写法：**只要弹窗内容可能超过 90% 屏高，就必须这么分层**，否则标题和
  关闭按钮会跟着内容滚出可视区（用户就得先滚回顶部才能关掉）。

### 17.5 编辑弹窗页脚：三个按钮都「有边框、不填色」

- 取消 / 测试连接 / 保存都改成 `g-btn-secondary`（原来取消是 ghost、保存是实心品牌色）。
  主次靠文字与位置区分，不再靠填充色。

### 17.6 Toast：居中浮在标签栏那一条里

- 定位带从「顶栏下方 + 8px」改成**顶栏那一条本身**：`top: var(--vvt)` +
  `height: var(--header-h)` + 竖直/水平居中。键盘弹起、地址栏收缩时跟着顶栏走（变量语义没变）。
- 容器仍是 `pointer-events-none`：提示浮在标签上，但底下的标签照样能点、能拖。

### 17.7 「信息」弹窗加「拥有者」

- 后端 `FsInfo` 新增 `uid` / `gid`（来自 SFTP attrs，v3 只有数字）与 `owner` / `group`
  （在**远端**用 `id -nu` / `id -ng` 查出来的名字，best-effort）。
  - 名字解析放在 SFTP 锁**外面**（另拨一条连接跑 `id`），不占着会话的 SFTP 通道；
  - 查不到（远端没有 `id`、账号已删、命令失败）就返回空串，界面退回显示数字——**绝不编名字**；
  - 查名字失败也不让「信息」整体失败（uid/gid 已经有了）。
- 前端 `InfoDialog` 增加「拥有者」一行：有名有组 → `demo:demo（1000:1000）`；只有数字 → `1000:1000`。

### 17.8 空白处右键菜单加「刷新」

- `FileManager` 空白处菜单末尾加 `刷新`（= 工具栏那个 `retry()`：目录没定位到时重试 `ensure`）。

### 17.9 冻结文件（本轮）

`frontend/src/style.css`（两个 idle 标签类的底色）、`frontend/src/i18n/*`（新增
`files_info_owner`）、`frontend/src/serverapi/index.ts`（`FsInfo` 的 uid/gid/owner/group）。

### 17.10 本轮的验证

- `npm run type-check` + `npm run check:i18n`（398 key）+ `make` + `make test` 全绿。
- 后端 `TestFsInfo` 补属主断言：`uid`/`gid` 必须与本地 `stat` 一致；`owner` 与**测试自己跑
  `id -nu <uid>`** 的结果一致（本机没有 `id` 时只记一条日志、不误判成产品 bug）。
- 浏览器只读实测（harness 开关摆状态、验完已删，`grep HARNESS-R7 frontend/src` 为空）：
  - 顶栏应用标签与会话内子标签：非激活时都是**常驻灰底 + 常驻 ×**（激活态仍是品牌色）；
  - 卡片：右上角「关闭」图标已变红（未打开时那个「打开」仍是中性色）；
  - 工具条：「添加会话」有描边、无填充（品牌色文字），「设置」同样有描边，没有「刷新」；
  - Toast：浮在标签栏那一条里、水平居中；一次两条时**向下排**（不会把上面那条顶出屏幕）；
  - 「信息」弹窗多了「拥有者」一行：实测显示 `Harness:AppUsers（817:823）`（名字 + 数字）；
  - 空白处右键菜单末尾是「刷新」；
  - 两个弹窗的滚动：先把已信任主机灌到 9 条（设置弹窗）让正文真的超屏，再由 harness 把**弹窗正文**
    滚到底——截图里标题与 × 都还在原位、编辑弹窗的页脚三个按钮也固定在底部
    （脚本只滚 `region-center` 里的正文；直接按 class 找会先命中首页那个列表，白滚一场）。
- 老规矩：**按钮的点击本身**没被自动化点过（本会话浏览器只允许只读观测），窄屏/触屏形态也没法
  模拟——相关函数（`retry`、`resolveOwnerNames`、`setLeftHidden`…）都直接验过，没验的只是
  「按钮 → 函数」这一跳。

---

## 18. 第八轮修正（用户对第七轮界面的返工要求）

### 18.1 面包屑按钮条再精简：`[刷新] [隐藏面包屑]`

- **删掉「家目录」按钮**：树本来就会自动展开到登录用户的家目录，再放一个按钮是重复的。
  那个按钮的「展开到 home 那条链路 + 滚进视野」这半件事**没有消失**，改由
  `FileTree.revealHome()` 在两个时机自动做：
  1. **新打开会话**（`state.home` 第一次到手，原来的行为）；
  2. **重新展开面包屑**（`sessionUi.leftHiddenOf` 从 true 变 false）——收起期间用户可能已经
     在别处逛过、链路也被收掉了，重新展开时把视线拉回主场。
  两个入口都要 `await nextTick()` 再滚：左栏是 `v-show` 隐藏的，`display:none` 下
  `scrollIntoView` 一点效果都没有。
- 老按钮的另一半「把**当前目录**切回家目录」**随之取消**（用户明确说这个按钮没用了）：
  展开树不再改当前目录。想回 home 就在树里点一下——这是有意的取舍。
- **删掉「切换到文件管理」按钮**：子标签栏里那个「文件管理」子标签是常驻的，不需要第二个入口。
  **文件管理器工具栏里的「返回终端」保留**（它不是这个按钮的反面：文件管理在前台时左栏本来
  就不显示，而返回终端是「回到我刚才用的那个终端」，两者不是一回事，见 14.3 / 15.4）。
- 文案：`files_switch_to_files` 删除；`files_home` **保留**（目录树里还在用）。17.x 之前说的
  「左栏 ⇄ 与工具栏 返回终端 是一对开关」作废。

### 18.2 关闭按钮：鼠标悬停才变红（触屏常驻红）

- 新增 CSS 工具类 `.close-danger`（`style.css`），外加 `.on-brand` 这一档：
  - **默认就是红的**（触屏没有 hover，这样它常驻红）；
  - 只有 `@media (hover: hover)` 的设备才收成中性色（`.on-brand` 那就是白一点的浅色），
    悬停时再变红（`bg-danger/10 text-danger`）。
  - 反过来写（`hover:text-danger`）在触屏上等于**永远不红**——Tailwind 的 `hover:` 变体自带
    `@media (hover: hover)`，与 `.hover-reveal` 的坑是同一个（见 AGENTS 第 5 节）。
- 用在**三处**：顶栏应用标签、会话内子标签（激活那一个带 `.on-brand`）、卡片里终端行的关闭按钮。
  （第一次只做了用户点名的两处，用户反馈「子终端没变红」后补上——三处保持一致。）
- 卡片右上角那个「打开/关闭」图标按钮**保持**第七轮的做法（已打开时常红）——它是整卡片的
  动作按钮，不是 tab 上的小 ×。

### 18.3 信息弹窗的标题不再带文件名

- 标题从「信息：{name}」改成「信息」（窗口里紧接着第一行就是「名称」，重复没有意义）。
  `files_info_title` 删除，标题直接用 `files_info`。

### 18.4 「新打开会话时面包屑没有展开至家目录」（真 bug，根因在 store）

- 症状（用户上报）：打开（或刷新恢复）一个会话时，左栏目录树**不展开到登录用户的家目录**，
  只剩一个「根目录」。
- 根因不在 FileTree，而在 `stores/sftp.ts` 的 `stateOf()`：
  ```ts
  let st = states.value[sid]
  if (!st) { st = blankState(); states.value[sid] = st }
  return st          // ← 返回的是**原始对象**，不是 ref 里的代理！
  ```
  `blankState()` 造出来的是原始对象，赋值表达式的值也是它——于是**第一个**调用 `stateOf(sid)`
  的组件拿到一层没有响应式的壳：它读得到最新值（直接读属性当然是对的），但它自己的 `computed`
  与 `watch` **永远收不到后续改动**，界面也不刷新。
  谁的「第一次调用」取决于模板渲染顺序——第六轮之前左栏那个「家目录」按钮的 `:disabled="!state.home"`
  会先读一次、顺手把状态建出来，FileTree 拿到的是代理、一切正常；第八轮删掉那个按钮之后，
  FileTree 成了第一个调用方 → 它的 `watch(() => state.home)` 从注册那天起就没触发过。
- 修法：新键**赋值之后再从 ref 上读一次**返回（拿到代理）：
  ```ts
  if (!states.value[sid]) states.value[sid] = blankState()
  return states.value[sid]!
  ```
- 同类写法顺手修掉：`stores/terminals.ts` 的 `ensure` / `add` 里
  `const list = tabs.value[sid] ?? (tabs.value[sid] = [])` 也是同一个坑（首个终端 push 不触发
  响应式，只是被紧跟着的 `setActive` 那次写入掩盖了）。
- `stores/paneControls.ts` 里的同款写法**没改**：那份注册表不参与渲染（只有 `get()` 读它），
  改了也没人受益。

### 18.5 冻结文件（本轮）

`frontend/src/style.css`（`.close-danger`）、`frontend/src/i18n/*`（删 2 条）、
`frontend/src/stores/sftp.ts`（`stateOf` 返回代理——这**不只是**给左栏用的，任何组件都可能在
「第一个调用方」的位置上，写新代码时别退回旧写法）。`stores/sessionUi.ts` 未改。

### 18.6 本轮的验证

- `npm run type-check` + `npm run check:i18n`（396 key）+ `make` + `make test` 全绿。
- 浏览器只读实测（harness 开关摆状态、验完已删，`grep HARNESS-R8 frontend/src` 为空）：
  - 左栏按钮条只剩「刷新 + 隐藏面包屑」（家目录、切换到文件管理都不在了）；
  - **重新展开面包屑会把树拉回家目录**：先把树挪到 `/usr`（harness 调 `expandPath('/usr')`），
    再收起→展开——截图里 `/tmp` 与 `webssh-r8-sftp` 被展开、**home 行带高亮并滚进视野**，
    `/usr` 那条链路被收起。为了让「链路能一路展开」成立，这一步特意换了一台 SFTP 根目录在
    **/tmp**（系统 tmpfs）的测试服务器：本仓库 checkout 落在飞牛卷里，而 `/vol1` 对当前账号
    不可读，`expandPath` 会在那一层就停下（这是 15.9 里记过的既有现象，不是回归）。
  - 关闭按钮的颜色：构建产物里核对过三种形态——默认（触屏）`color: var(--color-danger)`；
    `@media (hover: hover)` 里收成中性色 + `:hover` 变红；`.on-brand`（激活标签）平时白、悬停红。
    截图确认桌面端（可悬停）非激活标签的 × 是灰的、激活标签的是白的——即「悬停才变红」那一支
    生效；触屏那一支没法在本会话模拟（同一个类，靠的就是媒体查询）。
  - 信息弹窗标题只有「信息」两个字（`files_info_title` 已删，代码里不可能再渲染文件名）；
  - **新打开会话时树展开到家目录**：用 console 探针定位到「watcher 从未触发」（store 的 home
    确实写进去了，FileTree 读到的也是新值），换成「没有任何日志」→ 顺着摸到 `stateOf` 返回原始
    对象这个根因；修完再跑一次普通刷新，树里 `/tmp/webssh-r8-sftp` 展开、**home 行高亮并滚进
    视野** ✓（用的是 SFTP 根在 tmpfs 的那台测试服务器，链路每一层都可读）。
- 老规矩：**按钮的点击本身**没被自动化点过（本会话浏览器只允许只读观测），窄屏/触屏形态也没法
  模拟——相关函数（`revealHome`、`closeTab`）都直接验过，没验的只是「按钮 → 函数」这一跳。

---

## 19. 第九轮修正（用户对第八轮界面的返工要求）

### 19.1 左栏宽度：分隔线上的拖动手柄（平板必须）

- 原来「直接拖那条 `w-1` 的分隔线」在**平板上根本抓不住**（触屏没有鼠标，手指落点精度差一个
  数量级）——那条 div 已删除，改成**吸附在分隔线上的手柄**：
  - 位置：左栏（`<aside>`）的右上角、居面包屑按钮条右侧，`right-0 -translate-x-1/2` 让它**骑在**
    那条 1px 边框上（一半在左栏、一半在主区域）；
  - 尺寸/手感：20×32 的实体按钮 + `touch-none`（拖动时不会顺手滚动画布）+ `cursor-col-resize`；
  - 桌面端也保留：它同时是「这里可以拖」的可见提示；`pointerdown` 时顺手 `blur()`，免得用户
    拖完按空格又「按到」它。
  - 文案 `session_left_resize`（拖动调整宽度）。

### 19.2 压缩：目录回退变成三档（/tmp → 同级目录 → 家目录）

`resolveArchiveDir` 的规则（`docs/CONTRACT.md` 15.5 / 16.5 的后续）：

1. 配置的目录（默认 `/tmp/webssh-archive`）可写 → 用它；
2. 不可写 → 同级的 `<root>-<登录用户名>`；
3. 还不可写 → **`$HOME/.webssh-archive`**（用户要求：远端整个 `/tmp` 都不可写的机器也得能压缩，
   家目录是登录用户唯一保证能写的地方；`$HOME` 为空时跳过这一档）；
4. 三档都不行 → `permission` + 文案里点名**三个**候选目录、说明这是**远程主机**、
   给出 `WEBSSH_ARCHIVE_DIR`，并附原始输出。

实现细节：探针脚本里用一个 `try()` 函数（`mkdir -p` + `[ -d ]` + `[ -w ]`）按顺序试；
仍然只用 shell 内建 + `command -v mkdir` 判断工具在不在——远端 PATH 坏掉时**不能**顺手报
「目录不可写」（那样「远端没有 tar」会被误报成权限问题，见 16.5）。响应里的 `archive.dir`
始终是**实际**目录。

### 19.3 剪贴板为空时，菜单里不再显示「粘贴」

- `pasteItem()` 的 `always` 参数删除：**行菜单与空白处菜单现在同一套判据**——
  - 剪贴板为空 → 整项**不出现**（13.4 那版「空白处总是显示、置灰+说明」作废：一个永远灰着的
    菜单项只是噪音）；
  - 剪贴板来自**别的会话** → 仍然显示但置灰，点一下说明原因（用户需要知道「东西还在，只是不能
    在这儿粘」——跨会话粘贴是硬约束）。

### 19.4 「全选」改成「多选」模式

- 工具栏原来那个「全选」按钮（一次性把当前目录全选上，想选三个文件还得先全选再逐个取消）改成
  **多选模式开关**：
  - 点一下进入多选模式（**不预选任何东西**，`selected` 清空），按钮变高亮，再点一下退出；
  - 多选模式下点行 = 选中/取消选中（不再打开/预览）；
  - 面包屑那一行让位给**选择条**：一个都没选时显示「点选文件或目录（可多选）」这句提示，
    选了之后显示「已选 N 项」，右侧是 `[取消多选]` 与 `[删除所选（N）]`（0 项时禁用）；
  - **做完批量动作就退出多选**：复制 / 剪切 / 删除 / 压缩 / 导出成功之后调 `sftp.exitMulti(sid)`
    （内部同时清空 `selected`）。失败/同名确认框还没走完时**不退出**——那样用户还得重新选一遍。
- store：`SftpState.multi: boolean` + `setMulti(sid, on)`（进/出都清空已选）+ `exitMulti(sid)`。
- 文案：`files_select_all` / `files_clear_selection` 删除，新增 `files_multi_select` /
  `files_multi_exit` / `files_multi_hint`。

### 19.5 冻结文件（本轮）

`frontend/src/stores/sftp.ts`（`multi` 一族）、`frontend/src/i18n/*`（删 2 条、加 4 条）。

### 19.6 本轮的验证

- `npm run type-check` + `npm run check:i18n`（398 key）+ `make` + `make test` 全绿；后端
  `TestFsCompressArchiveDirFallback`（退回同级目录）不变，新增
  `TestFsCompressArchiveDirHomeFallback`（前两档都建不出来 → 落在 `$HOME/.webssh-archive`），
  `TestFsCompressPermissionDenied` 改成「把远端 HOME 也指到不可写处」→ 三档全失败 → `permission`
  且文案点名三个候选目录（`TestFsCompressMissingTool` 继续通过，证明 PATH 坏掉时不会被误报成权限）。
- 浏览器只读实测（harness 摆状态、验完已删，`grep HARNESS-R9 frontend/src` 为空）：
  - **手柄**：`?`（普通加载，终端标签激活 → 左栏可见）截图里那个两竖条的小按钮骑在左栏右边框上、
    位于面包屑按钮条右侧的同一行 ✓；
  - **空剪贴板不显示「粘贴」**：`?files=1&blank=1` 的空白处右键菜单是
    「新建文件 / 新建文件夹 / 上传 / 信息 / 打开终端 / 复制完整路径 / 刷新」——没有「粘贴」 ✓
    （有剪贴板时它出现在「上传」与「信息」之间，第五轮的截图可作对照）；
  - **多选模式**：`?files=1&multi=1` 显示选择条「已选 2 项 / 取消多选 / 删除所选（2）」+ 两行
    高亮 + 工具栏那个按钮变品牌色；`?files=1&multi=copy`（选中两项后执行一次复制）里**选择条
    消失、面包屑回来**（模式退出、高亮清空），同时底部出现剪贴板栏「已复制 2 项到剪贴板
    alpha、beta / 粘贴到此处 / 取消」 ✓。
- 压缩的三档回退由后端用例覆盖（上面那两条），前端只读实测里没有再去动真实 `/tmp`。
- 老规矩：**按钮的点击本身**没被自动化点过（本会话浏览器只允许只读观测），窄屏/触屏形态也没法
  模拟——相关函数（`toggleMulti`、`exitMulti`、`copyEntries`、`resolveArchiveDir`…）都直接验过，
  没验的只是「按钮 → 函数」这一跳。

---

## 20. 第十轮修正（用户对第九轮界面的返工要求）

### 20.1 平板按「宽不宽」显示左栏（触屏判定只管触屏专属的东西）

- 症状（用户原话）：「平板不是窄屏，可以按模式显示面包屑的」——iPad 的 CSS 宽度是
  768/834/1024px，而 `useMobileLayout()` 把**触屏**也算成移动端，于是平板上的左栏（面包屑 +
  目录树）被整个吃掉，只能靠第九轮那个拖动手柄干看着（手柄在，左栏没了）。
- 修法：左栏的显隐判据从「不是触屏」换成「**视口够宽**」——
  `SessionView` 里 `showLeft` / `leftCollapsed` 改用 `useWideLayout()`（= `(min-width: 768px)`，
  与 Tailwind 的 md 断点同一条线，也是键条「两页并排」用的那个判据）；`<FileManager :mobile>`
  同样改成 `!wide`（平板放得下权限列，空白处右键、包括多选时的批量菜单，也该照常弹）。
- **触屏专属的东西仍然按触屏判定**（`useMobileLayout()`）：底部辅助键条、`.hover-reveal`
  的常显、移动端 16px 输入框。两条判据各管一段：**屏幕大不大** vs **有没有键盘/鼠标**。

### 20.2 多选时点工具栏任何按钮都退出多选

- `FileManager` 新增 `leaveMulti()`，加在这些入口上：后退 / 前进 / 刷新 / 上一层 / 面包屑导航 /
  新建文件 / 新建文件夹 / 上传 / 换视图（列表 ↔ 网格）。
- 理由：多选是「选一批 → 做一个动作」的短暂工作流；用户接着去点视图或新建，语义上已经离开那次
  选择了，留着高亮只会让下一次点行「莫名其妙多选了一项」。`leaveMulti` 同时清掉 `selected`。

### 20.3 多选时右键 = 批量菜单（行上与空白处同一份）

- 多选模式下（且确实选了东西）右键——**不管落在行上还是空白处**——菜单固定是这七项：
  `复制 / 剪切 / 删除 / 导出至飞牛 / 下载 / 压缩为 tar.gz / 压缩为 zip`，全部作用于整个选择
  （用户给的原话顺序就是这么排的）。一个都没选时不弹（没有作用对象），退回普通菜单。
- 细节：
  - 「导出至飞牛」在服务端没配共享目录时置灰 + 点一下说明原因（沿用 `exportItem` 的规则）；
  - **批量导出**同名冲突**一次问、整批覆盖**（`files_export_overwrite_n`：`覆盖这批里 N 个同名项？`）
    ——逐个弹窗问会把人烦死，而这一批是同一个选择、同一个意图。其余失败逐个报（带原因）；
  - **批量下载**串行调 N 次单文件下载（`/api/fs/download` 一次只收一个路径），成功只报一条
    `files_download_started_n`，失败报 `files_download_failed_n`（带失败数与最后一个错误）；
  - 删除走 `askDeletePaths` → `removePaths`（沿用「删除选中的 N 项」那句确认文案）。

### 20.4 冻结文件（本轮）

`frontend/src/i18n/*`（新增 `files_download_started_n`、`files_download_failed_n`、
`files_export_nas_done_n`、`files_export_overwrite_n`）。`useMobileLayout.ts` **未改**
（`useWideLayout` 是它既有的导出）。

### 20.5 本轮的验证

- `npm run type-check` + `npm run check:i18n`（402 key）+ `make` + `make test` 全绿；
  最终二进制里 `grep -c 'forcetouch\|HARNESS-R10'` 为 0，`backend/embed` 已删。
- 浏览器只读实测（临时 harness 开关从 `location.search` 读，验完三个文件已还原、重建）：
  1. **平板档 = 触屏 UI 开着 + 视口够宽**：`?forcetouch=1`（强制 `useMobileLayout()` 为真）
     且激活终端子标签时，底部辅助键条（`ESC ↑ Tab Ctrl Alt Shift Insert ← ↓ → …`）在，
     **同时左栏在、分隔线上的拖动手柄在**（20×32 圆角按钮 + grip 双竖线图标，骑在分隔线上），
     目录树也已展开并高亮登录用户家目录 `webssh-r8-sftp`。第九轮这一档是「触屏 → 没有左栏」，
     现在按宽度走 —— 20.1 的核心断言由此坐实。
  2. **文件管理同样按宽度**：`?forcetouch=1&files=1` 下列表带**「权限」列**，
     `?forcetouch=1&files=1&multi=1&blank=1` 下**空白处右键照弹批量菜单** ——
     `mobile=false` 与空白菜单都是宽度判定，不受触屏判定影响。
     （顺带确认视图偏好 `webssh-files-view` 是**每设备**的 localStorage 偏好：上一趟
     `leave=view` 把它写成 `grid`，所以复核权限列时先 `target.close` 开新上下文回到默认列表。）
  3. **多选 + 空白处右键**：`?files=1&multi=1&blank=1` 下弹的就是批量菜单七项
     `复制 / 剪切 / 删除 / 导出至飞牛 / 下载 / 压缩所选（2）/ 压缩为 zip`（截图逐项核对，
     「删除」是红的、「压缩所选（2）」带数量），选择条「已选 2 项 / 取消多选 / 删除所选（2）」
     在，alpha/beta 两行高亮。
  4. **工具栏动作退出多选**：`?files=1&multi=1&leave=view`（脚本调 `toggleView()`）→
     选择条整条消失、行高亮清空、视图切到网格 —— 20.2 的链路（`leaveMulti`）生效。
- 四趟浏览器访问 console 记录均为空（无报错）；验证用的 testsshd 根在 `/tmp/webssh-r8-sftp`
  （每层都可读），避开了 18.4 记的那个 `/vol1` 403 噪声。
- 老规矩：**按钮的点击本身**没被自动化点过（本会话浏览器只允许只读观测，`act.*` 一律被拒），
  窄视口（<768px）形态也没法模拟（没有视口仿真能力）——「窄屏 → 收左栏」这一侧是靠同一个
  `wide` 判据推定的，不是实测的；相关函数（`leaveMulti`、`multiMenuItems`、`downloadPaths`、
  `exportPaths`）都直接读过，没验的只是「按钮 → 函数」这一跳。

---

## 21. 第十一轮修正（用户对第十轮界面的返工要求）

### 21.1 剪切粘完就清空剪贴板（退出粘贴态），复制不受影响

- 用户原话：「剪切和复制的逻辑不同，粘贴后应当清空剪贴板退出粘贴模式」。
- 改前的实现（15.6）把「清不清」**只**挂在入口上（`keepClipboard`）：常驻栏的「粘贴到此处」
  对复制和剪切一视同仁地保留剪贴板；剪切还额外把剪贴板里的路径**重映射**到新位置，好让
  「再粘一次」还能用。问题是剪切本来就不是「可反复粘贴」的操作——它是**搬走**：搬完这一次，
  手上那批东西已经在目的地了，留着剪贴板要么指向已经不存在的源路径，要么变成「把这东西再搬
  一次」，两种都不是用户点「剪切」时想要的。
- 现在的判据（`sftp.paste`，两层）：
  | 操作 | 右键/空白菜单「粘贴」（`keepClipboard=false`） | 常驻栏「粘贴到此处」（`keepClipboard=true`） |
  | --- | --- | --- |
  | **复制** | 粘完清空（第五轮不变） | **保留**，可连着粘到多个目录 |
  | **剪切** | 粘完清空 | **粘完清空**（本轮改动），地址栏下那行剪贴板栏随之消失 |
- 判定点仍是「真的贴上了至少一项」（`outcome.count > 0`）：**失败**、以及**一项都没贴上**
  （全被同名挡下）时一律保留剪贴板——那两种情况下剪切还没完成，剪贴板得留着让用户重试。
  部分成功（有同名被跳过、其余搬走）按「贴上了」处理：剪贴板清空，没搬成的那项留在源目录，
  用户重新剪切一次即可（不再维护「剪贴板里只剩没搬的那几项」这种半吊子状态）。
- 实现上删掉了原来的路径重映射分支（`moved` map），判定简化成
  `if (op === 'cut' || !keepClipboard) clipboard.value = null`。
- 文案无需改动：剪贴板栏的「已剪切 N 项到剪贴板」仍然是**粘贴之前**的状态描述，
  粘完那一行就没了。

### 21.2 「常用指令」删掉重启 / 关机（连带整个「危险操作」分组）

- 用户原话：「常用指令删掉重启和关机，底部那句会改变系统…也删掉」。
- `data/presetCommands.ts` 删掉 `reboot` / `poweroff` 两条，`danger` 分组随之空掉 → 分组类型
  `PresetGroup`、`PRESET_GROUPS` 里的 `'danger'`、`DANGER_GROUPS` 导出、弹窗里的 `isDanger()`
  与那套警示色/三角图标**一并删除**（留一个永远为空的分组就是死代码，AGENTS.md 第 8 节）。
- 清单现在剩 8 组：系统信息 / 资源与进程 / 磁盘与目录 / 网络 / 日志 / 服务 / 用户 / Docker，
  最后一条是 `docker system df`。`auto: false` 的只剩「重启 SSH 服务」一条（它只填命令、
  不替用户回车，这条语义没变）。
- 删掉的 i18n key（zh/en 各 3 条）：`qc_p_reboot`、`qc_p_poweroff`、`qc_g_danger`。

### 21.3 弹窗高度：90% ×（视口 − 应用顶栏），不再减底部辅助键条

- 用户原话：「快捷指令的垂直高度固定为减掉会话标签栏之后的90%」。「会话标签栏」在本契约里
  就是**应用顶栏**（见 11.6），所以差别只在触屏上：旧算式还减了 `--keypad-h`（底部辅助键条），
  弹窗比用户要的矮。
- 新算式（`QuickCmdsDialog.vue`）：定位带 = 顶栏以下到视口底部，面板 = 带的 **90%**，
  带内垂直居中 —— 即 `calc((var(--vvh) - var(--header-h)) * 0.9)`。触屏上比旧版高约 50px，
  底部可能压到辅助键条上（模态盖住键条是正常的）。
- **定位带不再用 `.region-center`**（那条带的高度是 `--vvh − --header-h − --keypad-h`）：
  带与面板必须同一个算式，否则居中的 flex 会把溢出的一半顶进顶栏里。带用内联 `BAND_STYLE`
  写，与 `PANEL_HEIGHT` 共用一个 `BAND_HEIGHT` 常量。
- 桌面（`--keypad-h` 为空）实测数值**不变**：1280×720 → 带 49..720（671）、面板 604px。

### 21.4 删掉弹窗底部的页脚

- 用户原话：「底部的发送到终端x也不需要，不会发送错的」。
- 整条页脚（`shrink-0 border-t` 那一行）删除，两个文案一起下线：
  - `qc_preset_danger`（「会改变系统状态的命令不会自动回车」）——随 21.2 的危险分组一起没了；
  - `qc_send_to` / `qc_send`（「发送到 <终端名>」/「发送到当前终端」）——用户说不需要：
    指令发进哪个终端，由工具条上那个「快捷指令」按钮所在的位置就已经决定了（13.1）。
- 弹窗因此只剩「头部 + 页签 + 列表」三段，列表自己滚（`min-h-0 flex-1 overflow-y-auto`）。
- 只为页脚存在的 `targetLabel` computed 一并删除（否则 `noUnusedLocals` 会报错）。
- 删掉的 i18n key（zh/en 各 3 条）：`qc_preset_danger`、`qc_send`、`qc_send_to`。

### 21.5 「已信任的主机密钥」从设置页挪进会话弹窗，删除会话时一起清掉

- 用户原话：「把设置页删除信任密钥的功能移至各会话卡片弹窗内，只留下标题和删除按钮，不要密钥
  信息，删除会话的时候一起删除信任密钥」。
- **为什么该挪**：那份记录是按 `host:port` 存的，而设置页给的是一份**全局列表**——用户看到
  `10.0.0.1:22` 对不上自己哪张卡片，也就不知道该不该删。挪进会话弹窗（`SessionFormDialog`，
  卡片右上角「编辑」打开的那个）之后，标题旁边就是这台会话的地址与端口。
- 界面：一行，**只有标题 `form_trusted_key`（「已信任的主机密钥」）+ 删除按钮**。不显示
  keyType / 指纹——那两样在同一个弹窗的「测试连接」结果里已经给过了。
- **显示判据只有一条**：当前表单里的 `host:port` 在 `/api/hostkeys` 里**有没有记录**。
  用户追问的三个问题由此一并回答：
  | 场景 | 那一行 |
  | --- | --- |
  | 新增会话、主机还没被信任过 | **不显示**（没有东西可删；留一个点了没反应的按钮更糟） |
  | 「测试连接」成功但主机密钥未信任 | 不显示 —— **测试本身不写 known_hosts**，它只回 `hostkey_unknown` + 指纹 |
  | 点过「信任并继续」（`POST /api/sessions/trust`） | **立刻显示**（`trustAndRetest` 之后重拉一次列表） |
  | 编辑一个已信任的会话 | 一打开就在 |
  | 编辑时把地址/端口改成别的 | 跟着变（按**表单里的值**判断，不是按保存过的 `props.session`） |
  - 端口为空/非法时按 22 处理（与后端 `hostKeyID` 一致）；host 为空时不显示。
- 删除按钮先弹确认（`form_trusted_key_remove_confirm`：下次连接要重新确认指纹）——这条记录
  承担的是「防中间人」那一步，不该一点就没。
- **「保存」按钮的文案与提示也跟这条判据绑定**（同一轮用户两次收敛，最终形态是「仅保存」）：
  - 最初要求「隐藏保存、引导先测试连接」；用户随即指出这会**卡死另一种真实场景**——主机暂时
    连不上（或只是想先把配置存下来）的人什么都没法存。最终改为：按钮**始终在**，文案随信任
    状态切换——没确认过指纹时写 **`form_save_only`（仅保存）**，并把一句
    `form_trust_first`（**「无信任指纹，测试连接并信任密钥指纹后保存」**）放在**三个按钮之上
    单独占一行**（用户明确要求：不要和按钮挤在同一行）；有信任记录（或列表没读到，见下）时
    就是普通的 `common_save`，提示不出现。两个文案走**同一条 `submit()`**，区别只有文案与
    提示——指纹留到第一次连接时再确认（那次本来就会弹 `hostkey_unknown`）。
  - 判据 `needsTrust = hostsLoaded && 当前 host:port 无记录`：**「确认过没有」**才算还没信任；
    读不到列表（还在读 / 读失败）→ 假，按钮保持「保存」、提示不出现——那是我们自己的接口
    出了问题，不该让用户的按钮莫名变文案还多一句没头没脑的提示。与那一行删除按钮的保守方向
    （读不到就不显示）刻意不同：删除点了会改数据，文案只是个说明。
- **删除会话时一并清掉信任记录**：`home.remove()` 先删会话、再 `forgetHostKey(host, port)`。
  代价写在代码注释里：同一台主机同一端口上如果还有别的会话（会话身份是 `user@host:port`，
  所以只发生在换了用户名的场景），它们的信任也会一起没了，下次连接要重新确认指纹。这是用户
  明确要的行为；若要改成「删掉最后一个用它的会话时才删」，判据就加在 `home.remove` 里。
  删信任失败时**不报「删除失败」**（会话确实已经删掉了），而是单独提示
  `home_delete_trust_failed`。
- 删掉的 i18n key（zh/en 各 6 条）：`settings_known_hosts`、`settings_known_hosts_empty`、
  `settings_known_hosts_remove`、`settings_known_hosts_removed`、
  `settings_known_hosts_remove_failed`、`settings_known_hosts_confirm`；
  新增 6 条：`form_trusted_key`、`form_trusted_key_remove`、`form_trusted_key_removed`、
  `form_trusted_key_remove_failed`、`form_trusted_key_remove_confirm`、
  `home_delete_trust_failed`（key 总数不变，仍是 399）。
- 后端**未改**：`GET /api/hostkeys`、`POST /api/hostkeys/forget`（记录不存在时回 `not_found`）
  原样复用——「没有记录就不显示按钮」正是为了不去撞那个 `not_found`。

### 21.6 冻结文件（本轮）

- 剪贴板：`frontend/src/stores/sftp.ts`（`paste` 的清空判定）、
  `frontend/src/components/files/FileManager.vue`（注释同步；行为无需改——它只是把
  `keepClipboard` 传下去）。
- 快捷指令：`frontend/src/data/presetCommands.ts`（删 2 条 + 危险分组）、
  `frontend/src/components/QuickCmdsDialog.vue`（高度 / 页脚 / 危险色渲染）。
- 信任密钥：`frontend/src/components/home/SessionFormDialog.vue`（新增那一行）、
  `frontend/src/components/SettingsDialog.vue`（删掉整栏）、`frontend/src/stores/home.ts`
  （`remove` 一并清记录）、`frontend/src/i18n/*`（改 12 条 key）。
- 后端**未改**（`fs/copy` / `fs/move`、`/api/quickcmds`、`/api/hostkeys*` 的接口与返回结构都不变）。

### 21.7 本轮的验证

- `npm run type-check` + `npm run check:i18n`（402 → **399** key）+ `make`
  + `make test` 全绿（后端本轮未改，跑一遍确认仓库状态没被带坏）。
- **剪贴板**（真二进制 + `internal/testssh` 的 `testsshd` + `socat` 桥，1280×720，baseurl
  `/app/WebSSH`）：本轮 `act.click` 可用，所以菜单项与剪贴板栏按钮都是**真实点击**，不是直接
  调函数。四个场景逐条对照：

  | # | 操作 | 期望 | 实测 |
  | --- | --- | --- | --- |
  | 1 | `a.txt` 行右键 →「剪切」（菜单真实点击），进 `sub`，空白处右键 →「粘贴」 | 搬走 + 剪贴板清空 | `sub/` 里出现 `a.txt`、源目录里没了（磁盘核对）；剪贴板栏整行消失 ✓ |
  | 2 | `b.txt` 行右键 →「复制」，进 `sub`，点剪贴板栏「粘贴到此处」 | 复制 + **剪贴板保留** | `sub/b.txt` 出现；剪贴板栏仍在（「已复制 1 项到剪贴板 b.txt / 粘贴到此处 / 取消」）✓ |
  | 3 | `sub/a.txt` →「剪切」，回上级目录，点剪贴板栏「粘贴到此处」 | **21.1 改动的那条分支**：搬走 + 清空 | `a.txt` 回到上级；剪贴板栏消失（改前这里会留下一条指向新路径的剪贴板）✓ |
  | 4 | `b.txt` →「剪切」，进 `sub`（`sub/b.txt` 已存在），点「粘贴到此处」 | 一项都没贴上 → **保留**剪贴板 | toast「目标已存在同名项「b.txt」」，剪贴板栏仍在、`b.txt` 还留在源目录，可以直接重试 ✓ |

  场景 1 与 3 是「剪切退出粘贴态」的两条入口，场景 2 钉住第五轮的复制语义没被带坏，
  场景 4 钉住 `count > 0` 这个判定点。
- **快捷指令弹窗**（同一套环境，`getBoundingClientRect()` 实测 + 截图）：
  - 尺寸：带 `top=49 / height=671`（= 720 − 49，**没有**再减键条），面板 `top=83 / height=604`
    （= 671 × 0.9，上下各留约 33px）✓；
  - 触屏那一档用**注入 `--keypad-h: 56px`** 验证：带与面板的实测值**一点没变**（671 / 604），
    旧算式此时会变成 0.9 × 615 = 553 —— 21.3 的核心断言由此坐实；
  - 页脚：面板里已经没有带 `border-t` 的子元素，最后一个子元素就是那个
    `min-h-0 flex-1 overflow-y-auto` 的列表；「常用指令」页滚到底，最后一项是
    `Docker 磁盘占用 / docker system df` ✓；
  - 清单：8 个分组标题（系统信息 … Docker）里**没有**「危险操作」，整个弹窗文本里
    `重启主机|关机|危险操作` 一个都搜不到 ✓。
- 环境限制如实记录：本机沙箱不允许 `/dev/ptmx`（`testsshd` 报
  `open /dev/ptmx: permission denied`），所以**终端本身起不来**（面板显示「终端不存在」）。
  本轮改动都在文件管理与弹窗上，不受影响；没有验的部分是「真触屏/窄屏下的弹窗高度」——
  触屏那一档是用注入 CSS 变量的方式验的，不是真机。`act.*` 没有右键动作，剪贴板那两个
  右键是用 `contextmenu` 事件派发出来的（其余点击都是真实点击）。
- **信任密钥那一行**（21.5，真二进制 + 同一套环境，每一步都用 `/api/hostkeys` 对账）：

  | # | 操作 | 期望 | 实测 |
  | --- | --- | --- | --- |
  | 1 | 打开「设置」 | 不再有「已信任的主机密钥」 | 栏目只剩 主题 / 语言 / 终端字体 / 终端字号 / 飞牛 / 关于，整段文本里搜不到那六个字 ✓ |
  | 2 | 卡片「编辑」（`127.0.0.1:2222` 已被信任） | 出现那一行，且**没有**密钥信息 | 行的 innerText 正好是「已信任的主机密钥」+ 一个按钮，`SHA256` 搜不到 ✓ |
  | 3 | 点行内删除 → 确认框 → 确认 | 记录真的删掉、行消失 | toast「已移除信任」；`GET /api/hostkeys` 变 `{"hosts":[]}`；那一行从弹窗里消失 ✓ |
  | 4 | 点「测试连接」（此时无记录） | 不产生信任记录、行仍不显示 | 结果块给 `主机密钥未信任` + 指纹；`/api/hostkeys` 仍是空、行仍不显示 ✓（**测试连接不写 known_hosts**，只有 `/api/sessions/trust` 写） |
  | 5 | 点「信任并继续」 | 行立刻出现 | 重测成功（「已在信任列表」）+ 行出现 ✓ |
  | 6 | 「添加会话」里改地址/端口 | 跟随当前 host:port | 空 host → 不显示；`127.0.0.1:2222` → 显示；`127.0.0.9:2222` → 不显示；`127.0.0.1:22` → 不显示 ✓ |
  | 7 | 卡片「删除」→ 确认 | 会话与信任记录一起没 | `/api/sessions` 的 `sessions` 空、`/api/hostkeys` 空、`data/sessions/` 目录空 ✓ |
  | 8 | 编辑已信任的会话 | 按钮是「保存」 | 页脚 `取消 / 测试连接 / 保存`，无提示 ✓ |
  | 9 | 在同一个弹窗里删掉信任 | 按钮变「仅保存」+ 引导 | 页脚 `取消 / 测试连接 / 仅保存` + 黄字「还没确认这台主机的密钥指纹：建议先「测试连接」确认，「仅保存」则留到首次连接时再确认。」；那一行也消失 ✓ |
  | 10 | 接着「测试连接」→「信任并继续」 | 按钮变回「保存」 | 页脚恢复 `取消 / 测试连接 / 保存`、提示消失、信任行回来 ✓ |
  | 11 | 「添加会话」里改 host | 文案跟着信任状态 | 空 host → 「仅保存」+ 提示；`127.0.0.1:2222`（已信任）→ 「保存」；`127.0.0.9` → 「仅保存」+ 提示 ✓ |
  | 12 | 把 `/api/hostkeys` 的 fetch 打成 reject（页内临时改 `window.fetch`）后重开弹窗 | 读失败**不变文案** | 按钮仍是「保存」、提示不出现、信任行不显示（读不到就不给删）✓ |
  | 13 | （21.5 收敛后的核心场景）「添加会话」填一台**连不上**的主机 `127.0.0.1:2299`，点「仅保存」 | 不测试也能存下来 | 卡片出现 `demo@127.0.0.1:2299`；`/api/hostkeys` **没有**新增记录（仅保存不写信任）✓ |
  | 14 | 未信任的弹窗看提示的位置与文案 | 单独一行、在按钮**之上** | 文案逐字是「无信任指纹，测试连接并信任密钥指纹后保存」；`getBoundingClientRect`：提示的 bottom ≤ 按钮行的 top（不在同一行）、宽度撑满（不跟按钮挤）✓ |
  | 15 | 打开一台**可连上但未信任**的会话（第二个 testsshd，`:2223`，卡片「打开」） | toast 带指引 | 「打开失败：主机密钥未信任**，测试连接并信任密钥指纹后再试**」✓ |
  | 16 | 打开一台连不上的会话（`:2299`，`dial_refused`） | toast **不带**指引 | 「打开失败：目标拒绝连接（端口不对或 SSH 服务未启动）」——拨号不通跟信任无关，追加那句是误导 ✓（指引只挂在 `hostkey_unknown` 一种错误码上，见 `openFailedText`） |

- 验证用的环境是 `.tmp/r11/`（临时目录，未提交），验完进程已全部停掉（2222/18082 已释放）。
- 上面这一套（`type-check` / `check:i18n` / `make` / `make test` + 弹窗那几条实测）在**最终
  二进制**上又跑了一遍（`ls -lh ./webssh` 02:15 那一次构建），数值与上面完全一致。

---

## 22. 第十二轮修正

### 22.1 只剩一个终端子标签时不显示关闭按钮

- 用户原话：「会话只有一个子终端标签时，隐藏子终端标签的关闭按钮」。
- 实现只有一处：`SessionView.vue` 子标签栏里那个 `.close-danger` 按钮加 `v-if="termTabs.length > 1"`。
  这是 17.1「子标签的关闭按钮常驻显示」的**唯一例外**，理由不是美观而是误触代价：只剩一个终端
  时它是用户唯一在用的 shell，关掉这个会话就一个终端都不剩（要重新连得再点 `[+]`）。
- 判据用 `termTabs.length`（终端标签数），**不**看「文件管理」那个固定子标签——它本来就没有关闭
  按钮，也没有「关掉文件管理」这回事。
- 只是「标签栏上不给关」，不是「关不掉」：首页卡片里那一行（`SessionCard` 的 `home_term_close`）
  仍可关闭最后一个终端，那里有确认框。同理，顶栏应用标签的关闭按钮不受影响（关会话标签是终止
  整个会话，另一回事）。
- 关掉一个终端后标签数从 2 回到 1，按钮**立刻**消失（`termTabs` 是 computed，无额外状态）。

### 22.2 冻结文件（本轮）

- 无。本轮只动 `frontend/src/views/SessionView.vue`（非冻结）+ 三份文档
  （`AGENTS.md`、`README.md`、本文件）。没碰 i18n、store、`serverapi`、`style.css`、
  `Makefile`、`go.mod`。

### 22.3 本轮的验证

- `npm run type-check` 无错误；`npm run check:i18n` 399 key 一一对应（没增删文案）；
  `make` 构建通过（`./webssh`，`ls -lh` 12:51 那一次）；`make test` 全绿
  （**11 个 PTY 用例照例 skip**：本机 `/dev/ptmx` permission denied，见第 7 节）。
- **真实点击的端到端实测**（`.tmp/r12/`，真二进制 + `internal/testsshd` 的 `testsshd` 起在
  `127.0.0.1:2222` + `socat` 桥到 18082，baseurl `/app/WebSSH`，1280×720）：

  | # | 操作 | 期望 | 实测 |
  | --- | --- | --- | --- |
  | 1 | 添加会话 `127.0.0.1:2222 / demo`，测试连接 → 信任并继续 → 保存 → 打开 | 自动出现 1 个终端标签 | 子标签栏 `文件管理` + `终端 1`（激活）✓ |
  | 2 | 查子标签的 DOM | `终端 1` 里**没有**任何 `button` | `[{text:'文件管理',buttons:[]},{text:'终端 1',buttons:[]}]` ✓（改前这里有一个 `关闭终端标签`） |
  | 3 | 点 `[+]` 新建第二个终端 | 两个标签都出现关闭按钮 | `[{文件管理,[]},{终端 1,[关闭终端标签]},{终端 2,[关闭终端标签]}]` ✓ |
  | 4 | 点 `终端 2` 的 × → 确认框「关闭」 | 回到 1 个标签、按钮又消失 | 只剩 `终端 1`，其 `buttons` 又变回 `[]` ✓（这一条钉住「reactive 跟随标签数」） |
  | 5 | 截图核对 | 标签栏里没有多余的 × | `shot-…-688b8a5b.png`：`文件管理 | 终端 1 | +`，`终端 1` 上无 × ✓ |

- 环境限制如实记录：本机沙箱 `/dev/ptmx` 不可用（`testsshd` 日志
  `启动 shell 失败：open /dev/ptmx: permission denied`），所以**终端本身起不来**（面板显示
  「终端不存在 / 连接已关闭」）。本轮改动只在标签栏的渲染条件上，与 PTY 无关；没验的是
  「真机触屏/窄屏下的标签栏」——移动端用的是同一份 `SessionView` 子标签栏，没有第二套渲染。

---

## 23. 第十三轮：会话卡片上的系统信息

### 23.1 需求与形态

- 用户原话：「会话已连接时，在会话卡片上显示基本信息，cpu和负载一行，内存占用和存储空间一行，
  只需要数值，不需要图形，5秒刷新一次，关闭会话后停止并隐藏，显示子终端列表之上」。
- 卡片上的最终形态（子终端列表**之上**，两行纯文字，等宽字体）：

  ```
  CPU 11.5%   负载 0.88 1.15 1.20
  内存 3.9 GB / 7.6 GB   磁盘 60.8%
  ```

- 数字全部来自**远端主机自己**（不是跑 webssh 的这台机器）：`/proc/stat`（两次采样算 CPU）、
  `/proc/meminfo`、`/proc/loadavg`、`df -P -k`。本应用不在本机执行用户命令（AGENTS.md 第 1 节），
  所以后端跑的仍然是一条交给远端登录用户 shell 的只读命令。
- 取值口径（都是刻意选的，换之前先看这里）：
  - **CPU**：`(1 − Δidle+Δiowait/Δtotal) × 100`，两次读 `/proc/stat` 之间隔 0.5 秒。
    iowait 算「空闲」——不然一台在做大量磁盘 IO 的 NAS 会一直显示 90%+。
  - **内存**：已用 = `MemTotal − MemAvailable`（不是 `MemFree`，否则有缓存的机器会常年显示 90%）。
    老内核（< 3.14）没有 `MemAvailable` 时退到 `Free + Buffers + Cached`。
  - **磁盘**：`df -P -k "$HOME"`（家目录所在的文件系统，也是文件管理默认进去的地方），
    家目录不可用时退到 `/`；占用率自己按 `used/(used+avail)` 算，**不用** df 那个取整的百分比
    （它只给整数，界面上要一位小数）。
  - **负载**：`/proc/loadavg` 的前三个数（1 / 5 / 15 分钟）。
- `available=false`（远端不是 Linux、`/proc` 与 `df` 都读不出来）时**整块不显示**，卡片上不堆
  错误文案；原因记服务端日志（`stats.go` 的 `logx.Warnf`）。各字段互相独立：macOS 那种没有
  `/proc` 但 `df` 能用的机器只显示磁盘那一项。

### 23.2 后端

- `stats.go`：`sessionStats` / `parseStats`（纯函数，全部有单测）+ `statsClientFor` /
  `dropStatsClient`。
- **轮询连接与 `r.base` 分开**（`sessionRuntime.statsClient`，懒建、会话关闭时一起关）：
  1. 前端每 5 秒问一次，每次新拨一条 SSH（认证 + 密钥交换）对 NAS 这种弱 CPU 的机器是实打实的
     负担，远端 sshd 日志也会被刷满；
  2. `r.base` 被 `sftpMu` 保护着——一次大文件上传会占住它几分钟，轮询会跟着卡死（反过来也一样）。
  连接坏掉（超时被杀、远端重启）时 `stats()` 会 `dropStatsClient` 丢掉它，下一次轮询重新拨，
  不会一直撞在死连接上。
- 命令执行的公共部分从 `runRemote` 里抽成 `execOn(ctx, client, command, timeout)`
  （`remote_exec.go`）：`runRemote` 仍然是「拨号 + 跑一条 + 关掉这条连接」，stats 走同一条
  执行/采集/超时路径，只是复用一条常驻连接。超时时 `execOn` 会关掉这条连接（**缓存了 client
  的调用方必须自己丢掉**，见 `dropStatsClient`）。
- 单次取数超时 `statsTimeout = 10s`（不是给 tar 用的 5 分钟）；输出走 `limitedBuffer` 的 64 KiB 上限。
- 接口 `POST /api/sessions/stats {id}`：会话没打开回 `not_open`(409)，会话不存在回
  `session_gone`(404)；「远端读不出来」**不是**错误（`stats.available=false`）。

### 23.3 前端

- 取数与轮询全在 `stores/sessionStats.ts`（组件不自己起定时器：卡片会被反复挂载/卸载，
  定时器放组件里就会出现「卸载了还在问」或「重挂载了多问一份」）。
- `HomeView` 是唯一的驱动方，用 `watch` 把两件事交给 store 的 `sync(openIds, visible)`：
  - `openIds` = 已打开会话的 id（`home.stateOf(id)?.open === true`）；
  - `visible` = `tabs.activeId === HOME_TAB_ID`。**首页是 v-show 隐藏的**，切到终端标签后卡片
    仍然挂载着，不显式传可见性的话，用户在看终端时后端会一直每 5 秒跑一条远程命令。
- 行为：集合或可见性变化时**立刻拉一次**（刚打开的会话马上有数字、切回首页不会先闪旧值），
  之后每 `STATS_INTERVAL_MS`（5000）拉一次；不在首页 / 没有已打开会话 → 停表。
  每个会话各自记「上一请求还在飞」，远端慢的时候不会堆成一串并发请求。
- 拿不到数据（接口失败、`available=false`）时**清掉**该会话的数字（整块消失），不保留上一次的
  旧值——卡片上挂一份不知道多久以前的占用率比不显示更容易误导人；这是 5 秒一次的后台轮询，
  **不弹 toast**。不再「已打开」的会话立刻从 store 里删掉。
- 展示（`SessionCard.vue`）：两行 flex，`font-mono text-xs`，只给数字，没有任何进度条/图形；
  内存用 `humanSize()`（与文件管理同一套口径）；CPU / 磁盘保留一位小数，负载两位。

### 23.4 冻结文件（本轮）

- 无（新增文件：`backend/stats.go`、`backend/stats_test.go`、`frontend/src/stores/sessionStats.ts`）。
  改到 `src/serverapi/index.ts`（加类型与一个 API 函数）与 `src/i18n/*`（4 个 key ×2），
  都是**只增不改**——按第 3 节的要求先写了本节再改实现。

### 23.5 本轮的验证

- `make test` 全绿（新增 7 个解析单测 + 1 个接口集成测试；**11 个 PTY 用例照例 skip**，
  本机 `/dev/ptmx` permission denied，见第 7 节）；`npm run type-check` 无错误；
  `npm run check:i18n` 403 key 一一对应（+4）；`make` 构建通过。
- 解析单测覆盖：CPU 两次采样（含「计数器没动 → 不给假 0%」、缺字段、只有 `cpu0`）、
  meminfo 新旧内核两种口径、loadavg（三数/两数/空）、df（**英文与中文表头**、长设备名、
  只有错误信息）、以及「只有 df 能跑」的部分可用与「什么都读不到」的 available=false。
- 接口集成测试（进程内 testssh + HTTP）：Linux 上必须真的读到 cpuPercent / memTotal / 三个负载，
  且 0 ≤ cpuPercent,diskPercent ≤ 100、memUsed ≤ memTotal；会话没打开 → 409 `not_open`；
  关闭后再取 → 409；不存在的 id → 404 `session_gone`。
- **浏览器实测**（`.tmp/r13/`，真二进制 + `testsshd` + socat 桥到 18083，baseurl `/app/WebSSH`，
  1280×720；数值与宿主机的 `/proc`、`free -b`、`df` 逐项对过）：

  | # | 操作 | 期望 | 实测 |
  | --- | --- | --- | --- |
  | 1 | 会话未打开 | 卡片上没有这一块、0 个 `/api/sessions/stats` 请求 | 卡片只有名字与地址；请求数 0 ✓ |
  | 2 | 点「打开」 | 立刻出现两行数字 | `CPU 27.8% 负载 0.98 1.52 1.30` / `内存 3.8 GB / 7.6 GB 磁盘 60.8%` ✓（宿主机 `free -b` 已用 4074741760 B → 3.8 GB、`df` 172216576/283397632 = 60.8%） |
  | 3 | 量两行数字的位置 | CPU 与负载同一行、内存与磁盘同一行、整体在子终端列表**之上** | `getBoundingClientRect`：CPU(top 193,left 33) 与 负载(top 193,left 114) 同一行；内存(top 211,left 33) 与 磁盘(top 211,left 189) 同一行；数字块 bottom 227 < 终端列表 top 239 ✓ |
  | 4 | 停在首页 12 秒 | 每 5 秒一次 | 请求间隔 `5001, 5000, 5000, 5000, 5000`（ms），CPU 数值每次都在变（27.8 → 10.6 → 10.9）✓ |
  | 5 | 切到会话标签（首页被 v-show 隐藏）12 秒 | **停止轮询** | 间隔里出现一个 132333 ms 的空档；切回首页立刻又拉了一次（CPU 立刻刷新）✓ |
  | 6 | 卡片上「关闭」→ 确认 | 整块消失 + 停止轮询 | 卡片文本只剩名字与地址、这一块从 DOM 里消失；此后 101 秒内 0 个新请求 ✓ |
  | 7 | 重新「打开」 | 数字回来 | `CPU 26.6% 负载 0.96 1.17 1.21 …` ✓ |
  | 8 | 语言切到英文（`localStorage.webssh-language='en'` 后刷新） | 四个标签走英文 | `CPU 31.4% Load 1.03 1.15 1.20 Mem 3.9 GB / 7.6 GB Disk 60.8%` ✓（刷新后恢复的会话也会立刻重新取数） |
  | 9 | 服务端日志 | Linux 远端不该有「读不到系统信息」的告警 | `webssh.log` 空 ✓ |

- 没验的部分（如实记录）：**非 Linux 远端**（没有 `/proc`）在真机上只走了解析单测的
  `available=false` / 只有 df 两条分支，没有真的连过一台 macOS/BSD；**远端在轮询途中断线**
  的那条自愈路径（`dropStatsClient` → 下次重拨）只有代码审查，没有构造出「连接半死」的场景。
- **最终二进制复跑**：把上面那一套（注释里「第十二轮/第十三轮」写错的那两处改对后）重新 `make`
  了一遍（`./webssh` 13:34 那次），在**最终产物**上又量了一遍关键三条：打开后立刻有数字
  （`CPU 39.0% 负载 3.33 2.10 1.53` / `内存 3.9 GB / 7.6 GB 磁盘 60.8%`，与宿主机
  `free -b` 的 4156592128 B、`df` 的 172237712/283396224 = 60.8% 一致）、停在首页时请求间隔
  `5001, 5000, 5000, 5000`（ms）、关闭会话后这一块从卡片上消失且此后不再有新请求。
  环境是 `.tmp/r13b/`（临时目录，未提交；这一轮用 `fetch` 直接调接口建会话，
  其余断言仍是读真实 DOM），验完 2222/18084 已释放。

---

## 24. 第十四轮：四个数值的绿 / 黄 / 红阈值

### 24.1 阈值（**唯一来源是 `stores/sessionStats.ts` 的 `STATS_THRESHOLDS`**，本节只是它的说明）

| 指标 | 绿（正常） | 黄（偏高） | 红（过高） | 为什么这么定 |
| --- | --- | --- | --- | --- |
| CPU | < 60% | 60–85% | ≥ 85% | 短时 100% 也常见（一次解压、一次编译），60% 才开始值得看一眼；85% 以上基本是持续满载 |
| 负载 | < 0.7 | 0.7–1.0 | ≥ 1.0 | **按核数折算**（`load1 ÷ cpuCount`）：1.0 = 每个核上正好排着一个任务，也就是刚好跑满；0.7 起提醒。不折算的话 8 核上的 1.5 会被误判成重载 |
| 内存 | < 70% | 70–90% | ≥ 90% | 内存比 CPU 更早影响使用：开始换页就卡了，所以 70% 就转黄（Linux 的缓存会让「已用」天然偏高，这也是不按 50% 转黄的原因） |
| 磁盘 | < 75% | 75–90% | ≥ 90% | 在 NAS 上后果最重（写满 = 写失败，压缩/上传/日志全挂），所以最早转黄（75%）、90% 转红 |

- 边界口径：`value >= danger` 红、`value >= warn` 黄、其余绿（所以 60% 已经是黄、85% 已经是红）。
- 颜色只加在**数值**上（`CPU 72.0%` 里的 `72.0%`），标签保持中性灰；三档的类名只有一份：
  `statLevelClass()` → `text-success` / `text-warning` / `text-danger`（与
  `terminals.termStatusDotClass()` 同一套做法）。
- 整块带一条 `title`（`home_stat_hint`）把口径与三档阈值写清楚——颜色不能是没有说明的暗号。
- **负载需要核数**，所以后端在 `SessionStats` 里多给一个 `cpuCount`（`/proc/stat` 里 `cpuN` 的
  行数，与内核统计负载的口径一致，且这份数据本来就已经读回来了，不额外跑命令）。核数缺失
  （非 Linux）时前端按 1 核折算——宁可偏严，也不要把 8 核的 1.5 当成正常。

### 24.2 本轮的验证

- 后端：`countCPUCores` 的单测（8 核 / 空输入 / `cpuinfo`、`ctxt` 这类干扰行不能算成核）+
  `parseStats` 的核数断言 + 集成测试在 Linux 上必须拿到 `cpuCount ≥ 1`。
- 前端：`npm run type-check`、`check:i18n`（404 key）无错误；`make` 通过。
- **绿黄红的端到端实测**（真二进制 + `testsshd` + socat 桥到 18085）：用页内临时改写
  `window.fetch`（与 21.5 那次同样的手法）让 `/api/sessions/stats` 返回构造好的数值，
  等**真实的 5 秒轮询**把它渲染出来，再读 DOM 上那四个数值 span 的 class。一份 payload 一次
  轮询，五个 payload 覆盖全部 12 个格子与边界：

  | payload（cpu / load1 / mem / disk） | 期望 | 实测（class） |
  | --- | --- | --- |
  | 30% / 1.60 / 40% / 50% | 绿绿绿绿 | `text-success` ×4 ✓ |
  | 60% / 5.60 / 70% / 75%（**都正好压在下界**） | 黄黄黄黄 | `text-warning` ×4 ✓ |
  | 85% / 8.00 / 90% / 90%（**都正好压在红界**） | 红红红红 | `text-danger` ×4 ✓ |
  | 59.9% / 5.50 / 69.9% / 74.9%（各差一点） | 绿绿绿绿 | `text-success` ×4 ✓ |
  | 99% / 16.0 / 99% / 99% | 红红红红 | `text-danger` ×4 ✓ |

  负载那一列同时钉住了「按核数折算」：payload 里的 `cpuCount` 是 8，5.60/8 = 0.7 → 黄、
  8.00/8 = 1.0 → 红、1.60/8 = 0.2 → 绿。
- 真实数值（未打桩）那一遍：宿主机 8 核、CPU 12–13%、load1 1.3、内存 51%、磁盘 60.8%，
  四个数值全是 `text-success`；截图里 `CPU 72.0%`（黄）/ `负载 9.60 …`（红）/
  `内存 6.3 GB / 7.5 GB`（黄）/ `磁盘 95.0%`（红）四色同框，标签仍是中性灰。
- 环境是 `.tmp/r14/`（临时目录，未提交），验完 2222/18085 已释放。

---

## 25. 第十五轮：文件预览（图片 + 文本）

### 25.1 需求

用户原话：「文件管理的预览为最流行的几个图片格式添加预览，图片最大限制 4MB，超过不显示，
完善文本文件的预览（配置文件，常见代码文件）」。

拆成两件事：
1. **图片预览**：png / jpeg / gif / webp / bmp / ico / avif 七种（浏览器与后端嗅探都认的
   那几个），**上限 4 MiB，超过只说明原因、不显示预览**；
2. **文本预览**：让「配置文件、常见代码文件」真的能读——补上 UTF-16（Windows 上另存的
   .ini / .reg 全是它）与 BOM，加上行号 / 换行开关 / 体积 / 行数 / 编码这些读代码需要的元信息。

### 25.2 后端

**新增 `POST /api/fs/image`**（`backend/files_preview.go`）：

- 成功时回**原始字节**（不是 base64 JSON）：`Content-Type` 用 `http.DetectContentType`
  嗅探出来的类型，另加 `X-Content-Type-Options: nosniff` 与 `Cache-Control: no-store`。
  理由有两条：① 4 MiB 的图片走 base64 会变成 5.4 MiB 的 JSON，还要在前端拼 data: URL；
  ② 这是**唯一**把远端文件内容以 inline 交给浏览器的接口，类型必须由内容决定、且不让浏览器
  再猜（猜出 HTML 就等于给这个地址开了个同源脚本执行入口）。
- 白名单**按内容判，不看扩展名**：`image/png`、`image/jpeg`、`image/gif`、`image/webp`、
  `image/bmp`、`image/x-icon`（标准库嗅探）+ `image/avif`（ISO-BMFF 的 `ftyp` + `avif/avis`
  品牌，标准库的嗅探表里没有它，实测回 `application/octet-stream`）。不在白名单 → `invalid`。
- **不含 SVG**（`image/svg+xml`）：SVG 是能带 `<script>` 的 XML，而 blob:/同源地址一旦被当
  页面打开就是同源脚本执行。SVG 归文本预览（它本来就是 XML 文本）。
- 上限 `maxImageBytes = 4 << 20`（`backend/sftp.go`，与 `maxReadBytes` 同值但含义不同，
  所以是两个常量）。**先 `Stat` 再读**：超限直接 `too_large`（413），一个字节都不会传过来；
  读的过程中变大（正在被写入的文件）也按超限处理。

**`POST /api/fs/read` 的文本判定重写**（`decodeText`，同一个文件）：

- 认三种：UTF-8（**剥 BOM**，留着首行会多一个看不见的字符）、带 BOM 的 UTF-16 LE/BE、
  以及**无 BOM 的 UTF-16**（按 NUL 分布猜字节序：ASCII 的高位字节是 0，所以 NUL 只落在同一
  奇偶性上；长度必须是偶数、解出来不能有 NUL、不能有 U+FFFD、可打印字符 ≥ 95% 才认）。
- **GBK / GB18030 仍判非文本**：解码要 `golang.org/x/text`，而 go.mod 只允许 x/crypto 与
  pkg/sftp（AGENTS 第 3 节）。按字节硬猜只会把乱码当内容展示，比明说「不是文本」更糟——
  前端文案里点了「其它编码」，说的就是这一种。
- 响应多一个 `encoding` 字段（`utf-8` / `utf-16le` / `utf-16be`，认不出为空串）；
  `text=false` 时 `content` 仍是空串（不把二进制塞进 JSON）。
- 空文件算文本（打开一个空配置文件不该看到「不是文本文件」）。

### 25.3 前端

- `PreviewDialog.vue` 两种模式，按**扩展名**选（后端再按内容确认）：
  - 图片：`fetchImageBlob`（`serverapi/index.ts`，POST + JSON body 拿 blob）→
    `URL.createObjectURL` → `<img>`；**关闭/换文件/卸载时 revoke**（几 MB 的东西不留内存里）。
    失败按错误码分三种说法：`too_large`（带上列表里已知的体积与 4 MiB 上限）、`invalid`
    （名字像图片、内容不是）、`<img>` 自己的 `error`（后端放行但浏览器解不开，例如 HEIC）。
  - 文本：默认**不换行 + 行号**（代码/配置文件按行读才对得上；行号列 `sticky left-0`，横向
    滚动时钉在左边；超过 20000 行不给行号，内容照常显示）；「自动换行」开关打开后行号消失
    （换行会让一行占多行，行号就对不上了）。**这个开关是用户偏好，不随文件重置**。
  - 标题右侧一行元信息：体积 · 行数 · 编码（只在不是 UTF-8 时点出来）；图片模式显示体积与
    像素尺寸（`<img>` 的 `naturalWidth/Height`，加载后才有）。
- `FileManager.vue` 把列表里已知的体积传进弹窗（`previewSize`）：图片超限时后端不回内容，
  只有列表知道它多大——弹窗得靠它把「4.6 MB 超过上限 4.0 MB」说清楚。
- 新增 i18n：`files_preview_empty` / `files_preview_lines` / `files_preview_wrap` /
  `files_preview_image_{too_large,too_large_unknown,invalid,unsupported,failed}`；
  `files_preview_binary` 改成「不是 UTF-8 / UTF-16 文本（可能是二进制文件或其它编码）」。

### 25.4 冻结文件（本轮）

`frontend/src/serverapi/index.ts`（新增 `fetchImageBlob`、`fsRead` 的返回类型加 `encoding`）、
`frontend/src/i18n/{zh,en}.ts`、`docs/CONTRACT.md`。后端 `backend/sftp.go` 只加了一个常量。

### 25.5 本轮的验证

- 后端：`make test` 全绿，新增两条用例——
  `TestFsReadEncodings`（空文件 / UTF-8 BOM / UTF-16 LE·BE 带 BOM（含汉字）/ 无 BOM UTF-16LE /
  GBK 字节 / NUL 分布混合的二进制）与 `TestFsImagePreview`（PNG 原样字节 + `Content-Type` +
  `nosniff` + `no-store`；文本文件、目录、不存在、`../` 逃逸各自的错误码；改名成 .png 的文本
  文件按内容判 `invalid`；**正好 4 MiB 能预览、多一个字节 `too_large`**）。
- 前端：`make check`（vue-tsc + i18n 双向）无错误。
- 端到端手测（真二进制 + `testsshd` + socat 桥到 18080/18081，baseurl=`/webssh`）：
  `shot.png` → 图渲染出来，标题右侧 `524 B · 240×120`；`notes.conf` → 行号 1–24 对齐、
  `529 B · 24 行`、切「自动换行」后行号消失且长行折回；`utf16.ini` → `[core] name=值 …` 正常
  显示并标 `UTF-16LE`；`utf16-nobom.conf` → 无 BOM 也认出来；`main.go` → 缩进与行号都对；
  `empty.conf` → 「（空文件）」；`big.png`（4.6 MB）→ 「图片 4.6 MB，超过预览上限 4.0 MB，
  不显示预览」+ 下载按钮，**没有图**；`fake.png` →「不是可预览的图片（内容不是图片，只是名字像）」；
  `blob.bin` →「不是 UTF-8 / UTF-16 文本」。
- 长行（1408 字符）+ 行号的布局用页内 `script.evaluate` 量过：滚动容器 `scrollWidth 10236 /
  clientWidth 894`（确实能横向滚），`scrollLeft = 500` 之后行号列的 `left` 仍是 205（sticky
  钉住），两侧 `<pre>` 的 `line-height` 都是 `19.5px`（行对齐）。
- 环境是 `.tmp/e2e-preview/`（临时目录，未提交，验完已删），端口 2222 / 18080 / 18081 已释放。

---

## 26. 第十六轮：文件编辑（文本文件 + 目录树列文件）

### 26.1 需求

用户原话：「复用文件管理预览的逻辑，为能预览的文件在预览上添加编辑图标按钮和更多菜单加上
编辑功能（面包屑的更多菜单也加上编辑），要验证登录用户是否有权限修改，没权限直接报错拒绝修改
不进入编辑页，修改时在文件管理标签页下进入编辑页，编辑页覆盖文件管理页面，有最上角退出按钮，
右上角还原/保存按钮，退出编辑后回到文件管理进入编辑时的状态，如果是从面包屑来的，回到文件
管理初始状态」。

两处经确认的澄清（都影响实现，记在这里免得日后走样）：

1. **「面包屑的更多菜单」= 左栏目录树（契约 11.3 的「面包屑区」）每行的「···」菜单**。
   那棵树原本**只列目录**，菜单里没有可编辑的对象，所以本轮同时把树改成**也列文件**
   （见 26.4）；用户另外定了两件事：每个目录**只读/显示前 20 个文件（按文件名）**、
   **整棵树显示的文件总数不限**；某个目录的文件超过 20 个时，在它的文件后面加一行
   **「···更多文件」**，点它在文件管理里打开那个目录。
2. **「登录用户有权限修改」= 本会话的 SSH 登录用户**（不是飞牛网关用户的 ACL）。判据见 26.2。

### 26.2 后端

**新增 `POST /api/fs/edit-load`**（`backend/files_edit.go`）→
`{ok, path, size, content, encoding, bom, eol}`：

- **写权限探测**：在读内容**之前**，以本会话登录用户的身份把目标文件 `OpenFile(path, O_WRONLY)`
  打开一次再立刻关掉（不带 `O_TRUNC` / `O_CREATE`，不碰内容也不碰 mtime）。
  为什么不用 mode 位自己算：mode 位看不出 ACL、看不出 root、看不出组关系，而真正保存时走的
  就是这条路——让 SFTP 服务端回答「能不能写」是唯一不会骗人的判据。拒绝时回 **`permission`（403）**，
  且**一个字节的内容都不返回**（界面据此报错并拒绝进入编辑页）。
- 只接受**普通文件**：目录回 `invalid`，设备 / FIFO / socket 回 `invalid`（`errEditNotRegular`）。
  不只是文案问题：以 `O_WRONLY` 打开一个 FIFO 会阻塞到有读者出现，写 `/dev/zero` 这类节点更是
  纯粹的破坏。
- 上限与预览同一条（`maxReadBytes = 4 MiB`）：超过回 `too_large`。判非文本（二进制 / GBK 等）
  回 `invalid`——编辑页只服务预览认得出的那些文件。
- 编码元信息随内容一起回：`encoding`（`utf-8` / `utf-16le` / `utf-16be`）、`bom`（原文件是否以
  BOM 开头）、`eol`（`lf` / `crlf`，按**多数票**判）。保存时前端原样带回来，服务端不猜。

**新增 `POST /api/fs/edit-save`** `{sid, path, content, encoding, bom, eol}` → `{ok, size}`：

- **按原来的样子写回**（用户要求）：UTF-16 还是 UTF-16、带 BOM 还是带 BOM、CRLF 还是 CRLF。
  把 Windows 的 `.ini` / `.reg` 悄悄改成 UTF-8 + LF 是数据损坏，不是「顺手升级」。
  编辑区里的内容一律是 LF（textarea 的 value 按 HTML 规范把 CRLF 规整成 LF），所以 `eol=crlf`
  时后端先归一化再替换（不会写出 `\r\r\n`）。认不出的 `encoding` 回 `invalid`，
  **绝不**当成 UTF-8 写下去。`encoding` 缺省（空串）按 UTF-8——老客户端只发 `content` 也能存。
- 写法是**原地截断再写**（`O_WRONLY|O_TRUNC`），不是「写临时文件再 rename」：rename 会换掉
  inode，把属主、mode、ACL、xattr 全丢掉——在 NAS 上 ACL 恰恰是最要紧的那一层，一个「改一行
  配置」的操作不该顺带重写这些。代价是写入中途失败会留下半截文件，所以先读一份原内容做善后：
  写失败时尽力写回（best-effort，失败只记日志——调用方已经有更重要的错误要报给用户）。
- 目标必须**已经存在**（编辑期间被删掉回 `not_found`，不静默新建）；保存内容超过 `maxReadBytes`
  回 `too_large`；权限不足回 `permission`。

**错误映射**（`editError`）：在 `fsError` 之上补两条——`looksLikePermission`（SFTP 服务端把
「只读文件系统」这类拒绝回成 `SSH_FX_FAILURE`，pkg/sftp 不会归一成 `os.ErrPermission`，只剩
消息可认）与 `errEditNotRegular`（不是普通文件 → `invalid`）。真正的权限拒绝由 pkg/sftp 归一成
`os.ErrPermission`，`fsError` 本来就翻成 `permission`。

### 26.3 前端：编辑页

- **新增 `src/stores/fileEditor.ts`**（按 sid 分片）+ **新增 `src/components/files/FileEditor.vue`**。
  编辑页由 **`FileManager.vue` 以 `absolute inset-0` 覆盖层**渲染（文件管理器保持挂载，退出后
  目录、视图、滚动位置都在）——契约第 5 节「面板一律保持挂载」的规则不因为多了一个页面而改。
- 三个入口都走同一个 store 方法 `fileEditor.open(sid, path)`，它**先**加载（`edit-load`）、
  **成功之后**才把激活子标签切到 `FILES_TAB`（编辑页一定在文件管理标签页下）；
  失败就抛错，由调用方 toast——**不切标签页、不进编辑页**：
  1. 预览弹窗右上角的**编辑图标按钮**（只在文本模式、且内容读出来之后出现；图片没有编辑按钮）；
  2. 文件管理行菜单的**「编辑」**（只对「按扩展名算文本」的文件显示，见 `fileKind.ts`）；
  3. 左栏目录树文件行的菜单（见 26.4）。
- 编辑页形态（用户要求）：**左上角「退出」**、**右上角「还原 / 保存」**，中间一行是文件名 +
  体积 · 行数 · 编码（非 UTF-8 时）· CRLF（原文件是 CRLF 时）+「未保存」标记，正文是一个
  铺满剩余高度的 `<textarea>`（等宽、`spellcheck=false`）。Ctrl/Cmd+S 也保存。
  ⚠️ 第十七轮改过这里：**顶栏不再显示体积与行数**，正文改成「自动换行 + 左侧行号」并加了
  查找/替换两行——那些实现约束动一处就会让行号整体错位，见第 27 节。
- **退出**：有未保存修改时先确认（`ConfirmDialog`），然后 `close()`——**不动文件管理器的任何
  状态**（不导航、不换视图）。这就是「回到进入编辑时的状态」：进入编辑时没有碰它，退出时也不碰。
  从面包屑（左栏）进来时同理——用户要的是「回到文件管理初始状态」，即**别**把文件管理切到
  被编辑文件所在的那个目录去；左栏是从终端视图进来的，编辑页退出后停在文件管理标签页
  （用户原话「退出编辑后回到文件管理…」）。
- **还原**：重新调 `edit-load`（= 丢弃本地修改、回到磁盘上的当前内容），有未保存修改时先确认。
- **保存**：`edit-save` 成功后把 `original` 对齐，并刷新文件所在目录
  （`sftp.refreshTree(sid, parentPath(path))`：文件管理列表与树里展开过的节点都会更新）；
  失败保留编辑区内容并 toast 原因。（回执里的 `size` 从第十七轮起不再取用——顶栏不显示体积。）
- 会话关闭时 `home.forgetSession` 一并 `fileEditor.forget(sid)`。

### 26.4 前端：目录树列文件（「面包屑的更多菜单」的前提）

- `stores/sftp.ts`：`treeChildren` 从「只有目录」改成「**全部目录 + 按名字排序的前 20 个文件**」
  （`TREE_FILE_LIMIT = 20`），并新增 `treeFileTotal: Record<path, number>`（该目录下的文件总数，
  用来判断要不要渲染「更多文件」行）。三处写入点（`ensure` / `loadTreeChildren` / `loadDir` 的同步）
  都过同一个 `treeRows()`，免得某一处漏掉。
- `FileTree.vue`：
  - 文件行与目录行同一个外观（图标不同），点文件行 = **预览**（与文件管理器里点一行一致），
    点目录行照旧只更新共享的当前目录并展开（契约 11.3 的语义不变）；
  - `treeFileTotal > 20` 时在该目录的文件后面加一行 **「···更多文件」**，点击 = 导航到该目录 +
    把激活子标签切到 `FILES_TAB`（「在文件管理打开该目录」）；
  - 文件行的菜单补齐**完整的文件菜单**：预览 / 编辑 / 重命名 / 复制 / 剪切 / 删除 / 下载 /
    导出至飞牛 / 压缩为 tar.gz / 压缩为 zip / 解压（可解压时）/ 信息 / 复制完整路径；
    目录行的菜单保持原样（新建 / 上传 / 粘贴 / 重命名 / 删除 / 压缩 / 导出 / 复制路径 / 刷新 /
    在此目录新建终端）。
- **整棵树的文件数不设上限**（用户明确要求）：每个目录各显示前 20 个，展开多少层就有多少行；
  上限只按「单个目录」算。

### 26.5 冻结文件（本轮）

`frontend/src/serverapi/index.ts`（新增 `fsEditLoad` / `fsEditSave` 与 `FsEditContent`）、
`frontend/src/i18n/{zh,en}.ts`、`docs/CONTRACT.md`。后端只新增 `backend/files_edit.go` 与
两条路由（`admin_routes.go`）。

### 26.6 本轮的验证

**后端**：`make test` 全绿，新增 `backend/files_edit_test.go`：

- `TestFsEditRoundTrip`：UTF-8（无 BOM / 带 BOM）与 CRLF 文件「打开 → 改 → 保存」的字节保真；
- `TestFsEditUTF16`：UTF-16LE / BE（带 BOM）与**无 BOM** 的 UTF-16LE 往返（含汉字）；
- `TestFsEditCRLFNormalisation`：编辑区里 LF 与 CRLF 混着时，写回不会出现 `\r\r\n`；
- `TestFsEditPermissionDenied`（**本轮的核心断言**）：0444 的文件 `edit-load` 回 `permission`
  （403）且不返回内容、`edit-save` 同样被拒且文件一个字节都没变；
- `TestFsEditRejects`：目录 / 不存在 / `..` 逃逸 / 二进制 / **FIFO**（不是普通文件）/ 超过 4 MiB
  各自的错误码；保存侧的超限、坏编码（`gb18030`）、目标不存在、缺 path、会话不存在；
- `TestEncodeForSave` / `TestDetectEOL` / `TestEditErrorMapping`：编码器与错误映射的单测。

⚠️ 权限用例**不是**「随便 chmod 一下就断言」：项目工作卷会按自己的 ACL 重写 mode
（实测 `WriteFile(…, 0o444)` 落地成 0705、属主照样能写），所以用例先探一个「真的执行权限位」的
目录（`permEnforcingDir`：写文件 → chmod 0444 → 以 `O_WRONLY` 打开确认被拒），并把 SSH 根目录
换到那里；以 root 跑、或所有候选都不执行权限位时 `t.Skip` 并写明原因（**不假装通过**）。

**前端**：`make check`（vue-tsc + i18n 双向）无错误，`make` 构建通过。

**端到端手测**（真二进制 + `testsshd` + socat 桥到 18080，baseurl=`/webssh`，环境
`/tmp/webssh-e2e-edit/`，验完已删）：

| # | 操作 | 结果 |
| --- | --- | --- |
| 1 | 文件管理里点 `notes.conf` → 预览 | 预览弹窗，标题栏多出「编辑此文件」按钮 |
| 2 | 点「编辑此文件」 | 预览关闭、切到文件管理子标签、编辑页覆盖文件管理页：左上角「退出」、右上角「还原 / 保存（未改动时禁用）」、标题行 `39 B · 4 行`，textarea 自动聚焦 ✓ |
| 3 | 改内容 → 保存 | 落盘 `key=edited/third=1/added_by_editor=yes`；保存按钮转灰、meta 变成 `39 B · 4 行` |
| 4 | 点「退出」 | 回到文件管理，仍是进入编辑时的那个目录（`root`），列表已刷新出新体积 |
| 5 | 再进编辑页改内容 → 退出 | 弹「有未保存的修改，退出会丢弃这些修改。」；确定后退出，**磁盘内容未变** ✓ |
| 6 | `readonly.conf` → 预览 → 编辑 | toast「打开编辑器失败：权限不足」，**不进编辑页**、子标签页不动、预览留着（用户还在读） ✓ |
| 7 | 左栏目录树 | 目录与文件都列出来了（`main.go / notes.conf / plain.txt / readonly.conf / win.ini …`），目录在前、按名字排 |
| 8 | 展开 `many`（25 个文件） | 20 个文件 + 一行「···更多文件」（title：还有 5 个文件未显示…）；另一处 318 个文件的目录同样只渲染 20 行 |
| 9 | 点「···更多文件」 | 切到文件管理子标签并打开 `many`（26 项）✓ |
| 10 | 树里点文件行 | 打开预览（与文件管理器点一行一致） |
| 11 | 树里文件行的「···」菜单 | 预览 / 编辑 / 重命名 / 复制 / 剪切 / 删除 / 下载 / 导出至飞牛 / 压缩为 tar.gz / 压缩为 zip / 信息 / 复制完整路径 |
| 12 | 树里 `win.ini` → 预览 → 编辑 | 编辑页 meta = `36 B · 3 行 · UTF-16LE · CRLF 换行`；加一行保存后落盘仍是 `\xff\xfe` + UTF-16LE + CRLF ✓ |
| 13 | 从树里进编辑页 → 退出 | 停在文件管理子标签，且**没有**被带到被编辑文件所在目录（回到文件管理自己的状态）✓ |
| 14 | `shot.png`（图片）的行菜单 | **没有**「编辑」项（图片没有编辑器），其余菜单项不变 |
| 15 | 目录行菜单 | 同样没有「编辑」项 |

---

## 27. 第十七轮：编辑页增强（自动换行 + 左侧行号 + 查找替换）

### 27.1 需求

用户原话：「增强文件编辑功能：编辑页不需要显示文件大小和行数，补上左侧的行数显示，自动换行，
不要水平滚动，新增查找按钮，点击之后在编辑区域上加两行（一行输入框+查找+数量+取消，一行
输入框+替换+全部替换），点查找按顺序依次在编辑区域循环显示查找内容，点取消隐藏这两行」。

拆成三件事：
1. **顶栏去掉体积与行数**（编码与 CRLF 标记保留——它们决定保存后文件长什么样，用户看不见）；
2. **编辑区改成「自动换行 + 左侧行号」**：换行后一行会占多行视觉行，行号必须仍贴在**逻辑行**
   的首行上（错位就是纯破坏），且不能出现横向滚动条；
3. **查找 / 替换**：顶栏一个查找按钮展开两行（查找内容 + 查找 + 数量 + 取消 / 替换为 + 替换 +
   全部替换），「查找」按顺序循环，数量显示 `当前/总数`，取消收起两行。

### 27.2 前端：行号怎么长出来的（`FileEditor.vue`）

单个 `<textarea>` 拿不到内部行盒（哪一行折成了几行），所以行号不是「量出来的」，而是让浏览器
再排一遍：编辑区由**同框叠放的两层**组成——

- **镜像层**（`ref="mirror"`，`absolute inset-0` + `aria-hidden` + `pointer-events-none` +
  `visibility: hidden`）：每行一个「幽灵块」（正常排版、不可见），它的高度就是这一行在正文里
  折行后的高度；行号是该行里的绝对定位元素（`right: calc(100% + 8px)`），靠
  `visibility: visible` 穿透父层的 hidden；
- **textarea** 在上面（透明背景），负责输入、选区、IME 与滚动。

两层**共用一份 `editorStyle`**（内边距 / 行高 / `overflow-wrap: break-word`），这是「折行结果
一致」的全部依据。四条硬约束（少一条行号就整体错位，都是实测撞出来的）：

1. **字体 / 字号 / 行高必须完全一致**：外层同一套 `font-mono text-xs max-md:text-[16px]`。
   `style.css` 在 <768px 时把所有输入控件钉到 16px——镜像层的行要是还停在 12px，两边折行
   位置立刻不同，所以镜像层也得跟到 16px（`max-md` 与那条媒体查询是同一个断点）。
2. **两层的 `overflow-y` 都必须是 `scroll`（常驻占位）**：一边 `auto` 一边 `scroll` 时，内容
   不足一屏的那一层会宽出一个滚动条宽度（8px），长行折行位置就变了。两层的 `overflow-x` 都是
   `hidden`，再加上 `wrap="soft"`，所以不会有横向滚动条。
3. **`overflow-wrap: break-word` 两边都要显式写上**：textarea 默认就会撕开超长单词（无空格的
   路径、URL、长 hash），而 `white-space: pre-wrap` 的普通块默认不撕——实测同一行能差好几行。
4. **幽灵块所在的行必须 `min-height: 20px`**：空行在普通块里高度是 0，在 textarea 里占一整行。

行号列宽 = `位数 × ch + 8px`（`ch` 跟着字号走），textarea 的 `padding-left` 用同一个算式，
所以窄屏字号变 16px 时列宽跟着变。行号只在 **≤20000 行**时画（与预览同一个上限），超过就整列
不画（编辑区只留 8px 内边距）——画一半、画错位都是给用户添乱。

滚动同步：编辑区 `scroll` 时把镜像层的 `scrollTop` 对齐（**直接改 DOM，不写响应式状态**，
否则每帧都要 diff 两万行 vnode）。查找的「滚进可视范围」也复用这一层：用 Range 在幽灵行里量出
命中的 y（未滚动坐标系），再决定要不要改 `scrollTop`（行号层被关掉时退化成「逻辑行 × 行高」）。

### 27.3 前端：查找 / 替换语义

- 命中表：**不分大小写**、不重叠、从左到右；查询词或正文一变就重算（`flush: 'sync'`，见下）。
  `toLowerCase` 万一改变长度（`'İ'`）就退回区分大小写，否则下标会与正文错位。
- 「查找」：按顺序走下一个，**走到末尾回到开头**（循环）。还没定位过时从**光标处**往后找第一个
  （读到哪儿就从哪儿往下找），找不到再绕回开头。
- 数量：`当前/总数`（还没定位过时显示 `0/n`），没有命中显示「无结果」，此时三个动作按钮禁用
  （点了不动的按钮比灰按钮更像坏了）。
- 「替换」：换掉当前命中，再把选区移到**插入文本之后**的下一个命中——不回头匹配刚插进去的内容
  （`a` → `aa` 时那样会原地打转）；后面没有了就绕回开头（与「查找」同一条循环规则）。还没定位过
  时先按「查找」的规则定位**并立刻替换它**。
- 「全部替换」：一次换完（不重叠、从左到右），换了几处必须 toast 说出来；换完**恢复光标与滚动
  位置**（见下一条），批量替换后视图被甩到文末只会让人以为出错了。
- ⚠️ **改完正文要动选区必须等 `nextTick`**：v-model 写 textarea 的 value 会把光标甩到末尾，
  同步设置的选区会被这一步覆盖掉（实测「替换」之后选区停在文末）。
- **打开与「还原」之后要把光标与视图归位到开头**（`resetView()`）：同样是 v-model 写值把光标
  甩到末尾，浏览器紧接着把视图滚到文末——打开一个长文件看到的是最后几行，用户根本不知道自己
  「已经在文件末尾」。

### 27.4 其他变更

- `stores/fileEditor.ts`：去掉 `size`（顶栏不再显示体积，留着就是死状态）；`fsEditSave` 的回执
  不再取用。后端**不变**（响应里仍然有 `size`，只是没人用了）。
- i18n 新增 8 条：`files_editor_find` / `_find_ph` / `_find_count` / `_find_none` /
  `_replace` / `_replace_ph` / `_replace_all` / `_replace_all_done`。
- 设置→关于：仓库地址改成 `https://github.com/shuangji66/webssh`，描述简化成
  「基于 Go 与 Vue 3 的 webssh 客户端」（两套字典同时改；这个地址只存在于 i18n，
  组件从 `about_repo_url` 读，见 `SettingsDialog.vue` 的关于块）。
- 冻结文件（本轮）：`frontend/src/i18n/{zh,en}.ts`、`docs/CONTRACT.md`、`AGENTS.md`。

### 27.5 本轮的验证

**镜像层折行一致性**：`.tmp/edtest/mirror3.html`（临时页，未入库）把「同一个 content 分别喂给
幽灵行与 textarea，比较折行行数与文字盒宽度」做成矩阵——**2 种布局（桌面 12px/520px、移动
16px/360px）× 2 种滚动条占位策略（都 `scroll` / 都 `auto`+`scrollbar-gutter: stable`）×
26 个病态用例**（无空格超长行、中文长行、中英混排、行尾空格、tab、长路径、长 URL、emoji、
组合字符、连续空行、全角空格、精确边界 ±1 字符、空内容……）＝ 104 组：

- `overflow-wrap` **两边都不写**时：桌面 8 组、移动 9 组对不上（同一行长能差 21/43 行）；
- 两边都写 `break-word`（或 `word-break: break-word`、`overflow-wrap: anywhere`）时：
  **104 组全部一致**（行数差 0、文字盒宽度差 0、两边都没有横向滚动条）。

**端到端手测**（真二进制 `make` + `testsshd` + socat 桥到 18080，baseurl=`/webssh`，
环境 `.tmp/e2e-edit17/`，验完已删）：

| # | 操作 | 结果 |
| --- | --- | --- |
| 1 | 打开 `many.conf`（120 行） | 顶栏只有「退出 / 文件名 / 查找 / 还原 / 保存」，**没有体积与行数**；左侧行号 1…121；打开即停在**第一行**（光标 0，旧版会滚到文末） |
| 2 | 量两层高度 | `textarea.scrollHeight === 镜像层.scrollHeight`（2432 = 2432）；`scrollWidth === clientWidth`（1280，**没有横向滚动**） |
| 3 | 滚到中部 / 底部 | 镜像层 `scrollTop` 与编辑区逐像素一致（600/1847），行号仍贴在各自行首 |
| 4 | `wrap.conf`（300 字符无空格行 / 60 词长行 / 中文长行 / 长路径 / 长 URL + 40 普通行） | 幽灵行高 40/60/60/60/60（= 2/3/3/3/3 视觉行），两层总高 1112 = 1112，行号贴在**逻辑行首行**上 |
| 5 | 查询 `alpha`（`replace.conf`：alpha/alpha/alpha/ALPHA） | 连点「查找」：1/4→2/4→3/4→4/4（命中大写的 `ALPHA`），第 5 次回到 1/4（**循环**）；选区依次为偏移 7/32/43/76 |
| 6 | 先把光标放到偏移 1000（第 46 行）再点「查找」 | `46/120`，选区 = 1000–1005（从光标处往后找） |
| 7 | 「替换」`alpha`→`ALPHA2` | 第 1 处换成 `ALPHA2`，选区自动落到下一个命中（2/4）；正文里刚插入的 `ALPHA2` **不**被再次匹配 |
| 8 | 「全部替换」 | toast「已替换 4 处」；光标与滚动位置不动 |
| 9 | 「还原」 | 确认后内容回到磁盘版本、视图回到第一行；退出后磁盘文件 md5 未变 |
| 10 | `win.ini`（UTF-16LE + BOM + CRLF） | 顶栏 `UTF-16LE · CRLF 换行`；用「替换」改 `flag=1`→`flag=2` 并保存，落盘仍是 BOM + UTF-16LE + CRLF（46 字节） |
| 11 | `huge.conf`（21000 行） | **不画行号**（镜像层不渲染、编辑区只剩 8px 内边距）；「查找」仍可用（命中末行、视图滚到底） |
| 12 | 查询无命中 | 数量显示「无结果」，查找/替换/全部替换三个按钮禁用，「取消」可用 |
| 13 | 「取消」 | 两行查找区消失、焦点回到编辑区、顶栏查找按钮 `aria-expanded=false` |
| 14 | 构建产物 | `max-md:text-[16px]` 生成 `@media not all and (width >= 48rem)`，与 `style.css` 的 `<768px` 规则同一个断点 |

**门禁**：`make test` 全绿（后端未改动）；`make check`（vue-tsc + i18n 双向）无错误；`make` 构建通过。

---

## 28. 第十八轮：审查问题的修复（27 条）

**来源**：第十五轮审查（只读复核）发现的 1 条严重 + 3 条高 + 6 条中 + 17 条低/记录项，
以及上一轮遗留未处理的两条（`Origin` 校验、`X-Trim-*` 信任边界）。本轮逐条修复，
每条都补了能复现原问题的测试或说明。**没有改任何对外契约的字段名/接口名**，
只有三处口径变化需要记在这里（编辑区内容一律 LF、覆盖上传的 mode/写权限、
预览/下载只接受普通文件）。

### 28.1 严重 / 高（4 条）

1. **编辑页在 CRLF 文件上坐标系错位（严重）** —— `stores/fileEditor.ts` 新增 `toEditorText()`，
   `open()` / `revert()` 载入时把 `\r\n` 与**孤立的 `\r`** 一并归一成 LF 再进 store。
   原因：`<textarea>` 的 `value` 按 HTML 规范会把 CRLF 换成 LF，而 store 里留着 `\r` 就是
   两个坐标系 —— 命中表（`scan()`）的下标、`setSelectionRange` 的选区、`selectionStart`
   与命中表的比较全都差「前面出现过多少个 CRLF」。症状：CRLF 文件里「查找」的高亮每行左偏
   一个字符（第 100 行偏 100 个），最严重的是在错位的选区上打字会改到**另一处字符**，
   保存后文件内容损坏。后端只在保存时按 `eol` 把 LF 换回 CRLF（`encodeForSave`），
   所以「编辑区里一律是 LF」这条口径（26.2/26.3）必须由前端真正做出来。
   **实测**（真二进制 + `testsshd` + 无头 Chromium，CRLF 文件 `flag=1\r\nname=value\r\nflag=2\r\n`）：
   点「查找」定位 `flag=2` → 选区 `18–24`、选中文本 `"flag=2"`（修复前是 `20–26`、选中 `"ag=2\n"`）；
   镜像层与 textarea 的 `scrollHeight` 相等（585 = 585）；改一个字符保存后落盘仍是 CRLF。
2. **目录树里删除文件必定失败（高，第十七轮的回归）** —— `FileTree.vue` 的 `doRemove` 把
   `dir` 参数硬编码成 `true`，而后端按真实类型校验（`isDir != req.Dir` → `invalid`）。
   修法：新增 `entryIsDir(path)`（先查树缓存里这一项的真实类型，查不到再退回菜单记下的 kind），
   `askDelete` 把它算好后一路传到 `sftp.remove`。对照 `FileManager.vue` 用的是 `entry.dir`。
3. **编辑保存被 1 MiB 的 JSON 体上限挡死（高）** —— 新增 `maxEditBodyBytes = 2×maxReadBytes + 64 KiB`
   与 `decodeJSONLimit()`，`/api/fs/edit-save` 单独用它。原因：body 里装的是要写回文件的**文本**，
   换行/引号会被 JSON 转义成两个字节，而 `edit-load` 允许打开到 4 MiB —— 两边共用 1 MiB 的 body
   上限时，**1 MiB 以上的文件能打开却永远存不回去**，而 `files_edit.go` 里那条
   「保存的内容超过 `maxReadBytes`」的判断根本没机会执行（既不可达、也没被测到）。
   现在「能打开」与「能保存」是同一个数。**测试**：`TestFsEditLargeSaveRoundTrip`（2 MiB 往返 +
   4 MiB 超限必须由「保存的内容」那一道报出来）。
4. **覆盖上传：mode 丢失 + 绕过目标文件的写权限（高，本轮回归）** —— `writeRemote` 的
   override 分支现在会：① rename 之前先 `Stat` 目标记下 mode，把临时文件 `Chmod` 成同一个值
   （失败只记日志——宁可保住内容，也不为一次 chmod 让整次上传失败）；② 覆盖前以 `O_WRONLY`
   打开目标一次探写权限（判据与 `edit-load` 的写权限探测一致），没权限就回 `permission`。
   原因：临时文件 + rename 只要求**目录**可写，等于绕过了目标文件自己的权限（0444 也能被替换），
   而且 rename 换 inode 会把 mode / 属主 / ACL / xattr 全丢掉 —— 后者正是 `files_edit.go` 明确
   拒绝 rename 的理由。**属主与 ACL 用 SFTP v3 恢复不了**，这是这条路径的已知取舍（建议以
   非 root 运行本服务，见 README 的安全边界）。**测试**：`TestFsUploadOverwriteKeepsMode`
   （0755 保持 + 0444 被拒且内容未变）。

### 28.2 中（6 条）

5. **预览 / 读取 / 下载 / 复制 / 导出都只接受普通文件** —— 新增哨兵错误 `errNotRegularFile`
   （→ `invalid`）。`fs/read`、`fs/image`、`fs/download` 的单文件分支在 `Stat` 之后显式拒绝
   设备节点 / 管道 / socket；`zipRemoteTree`、`copyRemoteEntry`、`exportRemoteEntry` 对这类条目
   **跳过并记日志**（与软链接同一个口径）。原因：读打开一个 FIFO 会阻塞到有写者出现，
   而 `withSFTP` 整段持 `sftpMu` —— 点一次预览就能让这个会话的文件管理整个卡住。
   **测试**：`TestFsRejectsNonRegularFiles`（四条路径都必须立刻回 `invalid`，且之后列表仍可用）。
6. **保存期间继续输入不再丢改动** —— `fileEditor.save()` 先把待写内容固化成 `payload`，
   请求用它、`original` 也用它；`await` 之后再读 `st.content` 会把 `original` 指向一份
   **没写进磁盘**的内容（界面显示「已保存」，磁盘是旧的，接着点「退出」连确认都不弹）。
7. **`stats()` 不看 `remoteResult.Code`** —— 新增 `statsUsable(res)`：`Code < 0`（对端没回
   exit-status，连接在命令执行中断开、stdout 是半截的）按失败处理并 `dropStatsClient`；
   **正的非零退出码照常解析**（非 Linux 远端 `cat /proc/stat` 失败就是 1，而那份输出里的 df
   段仍然有用）。**测试**：`TestStatsUsable`。
8. **预览的加载可以作废/取消** —— `PreviewDialog` 每次加载自增 `seq` 并建 `AbortController`
   （`reset()` / 卸载 / 换文件即 abort + seq 自增），响应回来先比对 `seq` 再写任何状态。
   原因：迟到的响应会写进**新文件**的状态（标题是新文件名、图是上一个文件的），Blob URL 也会
   拖到下次 reset 才 revoke。`serverapi` 的 `fsRead` / `fetchImageBlob` 增加**可选** `signal?`
   参数（只增不改）。
9. **编辑页顶栏允许折行** —— `flex-wrap` + 文件名/meta 可收缩。原因：窄屏上
   「退出 + 文件名 + 编码/换行 + 未保存 + 查找 + 还原 + 保存」一行放不下，而 meta 恰好只在
   UTF-16 / CRLF 这类文件上出现 —— 不折行时「保存」会被推出可视区（触屏也没有 Ctrl+S）。
   **实测**：360px 宽的面板上 meta + 脏标记 + 查找区同时打开时，`header.scrollWidth == clientWidth == 360`、
   「保存」完整可见（修复前溢出 50–118px）。
10. **关会话时在飞的轮询不再把数字写回** —— `pollOne` 写回前检查 `wanted.includes(sid)`，
    `SessionCard` 的判据叠加 `isOpen`；`sessionStats` 新增 `stop()` / `dispose()` 并在
    `HomeView` 的 `onBeforeUnmount` 里调用（不再把定时器的生死托付给「组件永不卸载」）。

### 28.3 低 / 记录（17 条）

| # | 改动 |
| --- | --- |
| 11 | `statsClientFor` 的两处 `Close()` 移到锁外（与 `sessionRuntime.close` 同一条锁纪律） |
| 12 | `parseCPUSample` **只累加前 8 个字段**：`guest` / `guest_nice` 已含在 `user` / `nice` 里，重复计入会让跑虚拟机的机器上 CPU 占有率系统性偏低（procps 的 vmstat 同样取前 8 个）。`TestParseCPUSampleExcludesGuestTime` 钉住 |
| 13 | 属主名缓存只在命令**真的跑成**（`res.ok()`）时写：非 0 退出时按「查不到」处理并保留重试机会 |
| 14 | 「还原」与「保存」互斥：按钮 `disabled` 与 store 守卫都看 `loading` / `saving`（两个请求并发时，回来的先后会决定「屏幕干净、磁盘是旧正文」） |
| 15 | Ctrl/Cmd+S 的拦截从 textarea 提到**面板根元素**：查找 / 替换输入框里按 Ctrl+S 也会触发浏览器「保存网页」 |
| 16 | 关闭子终端的确认框：**取消 / 点遮罩**后把焦点还给终端（`cancelCloseTerm`）；确认后给邻近标签兜一次（AGENTS 第 5 节的焦点约束） |
| 17 | 首页卡片三个确认框统一 `danger`（关闭会话 / 关闭子终端 / 删除都是破坏性动作，同一个动作在会话视图里本来就是红按钮） |
| 18 | 目录树菜单的「点别处关闭」改用 `pointerdown` + 捕获阶段（与 `FileManager` 一致），并在 `hideMenu` / 卸载时摘掉监听 |
| 19 | 孤立 `\r` 一并归一成 LF（同第 1 条）：镜像层与 textarea 的行数从此一致，行号不再整列下移 |
| 20 | `scan()` 的大小写回退改成**查询词与正文一起退回**区分大小写：只退一半时 `needle` 与查询词长度不同，会把内容切坏 |
| 21 | `FileTree` 去掉从未被传进来的 `mobile` prop（唯一调用方只传 `sid`，值恒为 false） |
| 22 | 「读的过程中变大」的超限文案不再报一个恒等于上限的体积（原来会显示「4.0 MB 超过上限 4.0 MB」）。⚠️ **第十八轮只修了 `/api/fs/read` 那一侧**（它本来就用 `tooLarge` 布尔、不带体积），image 那一处漏了——**第二十四轮才真正改掉**（`files_preview.go` 的文案改成「图片在读的过程中变大，超过预览上限 4.0 MB」，见 34.2） |
| 23 | AVIF 识别改为解析 ftyp 盒的 **compatible_brands**（`avif` 只出现在那里、major brand 是别的，是合法且常见的形态），并容错盒长自述为 0 的情况。`TestSniffImageTypeAVIFCompatibleBrands` 钉住 |
| 24 | 关会话的确认文案在**编辑页有未保存内容**时追加一句（`files_editor_close_warning`，`fileEditor.isDirty` 由此从死代码变成在用） |
| 25 | 编辑区行号幽灵块加 `v-memo="[i, line]"`：打字时只重新打补丁改动过的那一行。**实测**（无头 Chromium，20000 行）：每键 87.1ms → **9.2ms** |
| 26 | `/api/trim/status` 补 `requireMethod(GET)`（文档早就写的是 GET，实现没校验） |
| 27 | WebSocket 握手补 **`Origin` 校验**（防 CSWSH）：Origin 的 `host:port` 必须等于本次请求的 `Host`（缺端口按 scheme 补默认端口），反代改写 Host 时接受 `X-Forwarded-Host`；**没有 Origin 的请求放行**（curl / websocat，入口只有 unix socket + 网关）。`TestOriginAllowed` + `TestTerminalWSRejectsForeignOrigin`。⚠️ **端口判据在第二十一轮改过**（反代丢端口时硬比端口会把终端 WS 全拒，见第 31 节） |

**同时**：`X-Trim-Userid` / `X-Trim-Username` / `X-Trim-Isadmin` 是**信任头**这件事写进了
README 的安全边界（必须由反代覆写、客户端可伪造；`upload-from-nas` 的 `source` 是以登录用户
身份读的本机路径，ACL 预检是 best-effort，**进程自身的 OS 权限是唯一硬边界**——建议非 root 运行）。
「`source` 必须落在共享目录内」这条**刻意没做**：飞牛文件选择器本来就允许选共享目录之外的
文件，硬加约束会让正常用法直接失败，而真正的边界是 ACL + 进程 uid（记录在此，需要时再议）。

### 28.4 本轮的验证

- `go vet ./...` 干净；`go test ./...`：主包 **109 PASS / 10 SKIP / 0 FAIL**（119 个 Test 函数；
  skip 全部由 `requirePTY` 触发，见第 7 节）、`internal/secret` 与 `internal/testssh` 全过。
- `npm run type-check` 0 错误；`npm run check:i18n`：zh/en 各 **438** key 双向一致（+1 条新文案）。
- `make` 构建通过（`./webssh`）。
- 真二进制 + `testsshd` + 无头 Chromium（baseurl `/webssh`）实测了第 1、3、9 条（见各条里的「实测」）。
- 新增/更新的测试：`TestFsEditLargeSaveRoundTrip`、`TestFsUploadOverwriteKeepsMode`、
  `TestFsRejectsNonRegularFiles`、`TestSniffImageTypeAVIFCompatibleBrands`、
  `TestParseCPUSampleExcludesGuestTime`、`TestStatsUsable`、`TestOriginAllowed`、
  `TestTerminalWSRejectsForeignOrigin`。
- 未做：第 1 条的 Firefox / WebKit 复跑（只在 Chromium 上验过）、FIFO 卡死在真 OpenSSH 上的复跑
  （测试环境用的是进程内 SFTP 服务器）。

---

## 29. 第十九轮：快捷指令的命令内容加密落盘

### 29.1 需求

用户原话：「加密落盘用户添加的全局快捷指令和专属快捷指令的命令内容，命令名称不需要，
加解密时不要破坏命令的内容和格式」。

拆成三条：
1. 用户自己添加的**全局**与**专属**快捷指令，`content` 加密落盘；`name`（以及 `id`/`auto`）
   保持明文——名称是给人看的标签，加密它只会让文件没法排查；
2. **加解密不得改动内容**：往返后逐字节相同（前导/行尾空格、tab、`\r\n`、多行、非 UTF-8
   字节都要活下来）；
3. 顺带修掉一个真的会改内容的 bug：后端 `sanitizeQuickCmds` 原本对 `content` 做
   `strings.TrimSpace`（前端 `QuickCmdEditDialog` 早就只 trim 名称、并注释了「命令内容里的
   空格/换行是有意义的」），两套口径不一致的症状是「保存时看到的和重新打开后看到的不一样」。
   现在**只 trim 名称**，内容仅在「全是空白」时判为空（拒绝）。

不做的事：不加密名称、不改 API（收发的 `content` 始终是明文）、不给密文加额外的用途。

**同轮追加**（第 29.3 节）：老文件要**主动迁移**成密文，不允许再存在明文的命令内容——
迁移不掉的那条内容清空成加密的占位文案。

### 29.2 后端（`quickcmds.go`）

- **磁盘形态**（文件版本 2）：
  ```json
  { "version": 2,
    "commands": [ { "id": "…", "name": "数据库", "auto": true,
                    "content": { "v": 1, "ct": "<base64(nonce||ciphertext)>" } } ] }
  ```
  加密走 `internal/secret`（与凭据同一把 `master.key`、同一套 AES-256-GCM 信封与威胁模型），
  但**只在这一个落盘边界**：`loadQuickCmds` 解出明文、`saveQuickCmds` 加密后写。
- **AAD** = `webssh:v1:quickcmd:<指令 id>:<表作用域>`（`secret` 的 `field` 那一格是
  `quickcmd:<id>`，`sid` 那一格放作用域：全局 = 空串、专属 = 会话 id）。于是
  「同一张表里两条指令的密文互换」与「把专属表改名成 `global.json`」都会在解密时失败——
  否则按钮上写着乙的名字、点下去跑的是甲的命令。
  指令 id 在 `sanitizeQuickCmds` 补齐之后才加密，所以 id 是稳定不变的。
  ⚠️ 两处**已知的边角**（第二十四轮审计，见 34.4）：① AAD 用 `:` 拼三段，所以
  `(id="x", 作用域="a:b")` 与 `(id="x:a", 作用域="b")` 会拼出**同一个** AAD 字符串——
  没有改编码是因为改它会让磁盘上已有的密文全部解不开（要另做一次全量重加密），改用
  「两个入口都堵死」替代：非法 id 在迁移里重发、非法作用域文件名在扫描里跳过；
  ② 同一张表里 id 重复时密文仍可互换——迁移现在会把重复/非法的 id 重发掉（见 34.4）。
- **兼容第一轮**：老文件里 `content` 是**字符串**（明文）。`quickCmdStored.UnmarshalJSON`
  按 JSON 类型分流（字符串 = v1 明文、对象 = 信封），两种形态不会歧义，并给明文条目打上
  `legacy` 标记 → 由 29.3 的迁移就地加密重写。读这条路不能因为「格式旧」而失败
  （失败的表现是用户的整个指令列表打不开）。
- **解密失败必须报错**（`internal` + 指令名 + 原因），不能静默返回空内容：那会让用户以为
  指令内容丢了，而他一点保存就把服务端那份覆盖成空的。
- 每次保存都用新 nonce 重新加密（整表重写本来就会换掉整个文件，没有「复用旧密文」的意义）。

### 29.3 明文内容的一次性迁移（同轮追加）

用户追加要求：「向后兼容做一次老文件的迁移尝试，将明文的命令内容加密，迁移失败的命令内容就
清除并替换为加密的『命令加密失败已清空，请重新添加』，反正不得再存在明文的命令内容。」

- **两个触发点，缺一不可**（都走 `migrateQuickCmds` 这一份规则）：
  - `loadQuickCmdsFile`（读一张表时）：读到明文内容就地加密重写（`WriteJSON` = 临时文件 +
    rename，原子替换）。读接口因此带上了写副作用，这是「磁盘上不留明文」必须付的代价。
    ⚠️ 正因为带写副作用，「读 → 迁移 → 重写」与整表保存必须**互斥**（第二十四轮加的
    `quickCmdsLocks`，按作用域一把锁）：否则迁移那份**过期快照**会把用户刚保存的表整表覆盖
    （静默数据丢失；复审探针真并发 200 轮命中 6–9 轮）。`store.WriteJSON` 的锁只串行单次写，
    覆盖不了这个跨步序列，见 34.4。
  - `migrateAllQuickCmds`（`newAdminMux` 启动时，**早于 `srv.Serve`**，所以启动路径上不存在
    与请求的并发）：`store.QuickCmdsScopes()` 扫 `quickcmds/*.json`，逐张表走同一条
    「读 → 迁移 → 需要时重写」。**只有这一步能覆盖「没被打开过的会话」**——前端只加载全局表
    与当前会话的表，别的会话的表可能几个月都不被读一次，明文就会一直躺着。
    `QuickCmdsScopes` 只认 `global.json` 与 `<合法会话 id>.json`（第二十四轮收紧，见 34.4）：
    `.json` / `a b.json` / `a:b.json` 这类名字一律跳过并记日志，`WriteJSON` 的
    `<名字>.json.tmp-*` 残渣仍按扩展名过滤。
  - 启动那一遍失败只记日志（`logx.Warnf`）**不拦启动**：应用起不来比一条指令还是明文更糟；
    用户真的打开那张表时读路径会再迁移一次并报出明确原因。
- **迁移规则**（`migrateQuickCmds`）：
  - 已经是信封的条目**原样保留**（不重新加密，少一次「解不开」的机会）；
  - 明文内容加密成功 → 换信封：内容与格式**逐字节不变**，`id`/`name`/`auto` 不动；
  - **非法或重复的 id**（第二十四轮）→ 用 `store.NewID()` 重发（同表第一个出现的保留），
    并按新 id 重新加密（AAD 才与新 id 一致）；解不开的密文条目在重发 id 时会**原样保留 + warn**
    ——那条内容本来就取不回来，改 id 只会把「读表报错」变成「内容悄悄错位」。见 34.4。
  - **明文内容为空**（`Encrypt("")` 返回 nil——空内容存不下来）**或加密失败** → 换成
    `quickCmdMigrationPlaceholder`（「命令加密失败已清空，请重新添加」）的**密文**，并把
    `auto` 关掉（一句中文提示不该被「点一下就自动回车」当成命令跑到远端 shell 上）；
  - 连占位文案都加密不了（系统随机数不可用这一级）→ 返回错误、**一个字节都不写**：
    写不出密文，也不能为了「看起来迁移成功」而留下明文。
- **失败可见**：迁移写不进去（数据目录只读等）时读接口回 `internal`（「读取快捷指令失败：
  加密重写 … 失败」），而不是「先把明文内容返回给用户、下次再说」——静默保留明文违背这条
  改动的全部意义。占位替换会写进日志（用户只看到占位文案，原因必须能查）。
- 占位文案是**落盘的数据**而不是界面文案，所以故意不进 i18n 字典：它是被存下来的一个字符串
  值（用户以后把界面切成英文，磁盘上那句话也不该跟着变），字典又是构建期冻结的。

### 29.4 冻结文件（本轮）

`docs/CONTRACT.md`、`AGENTS.md`、`README.md`（只有文档改动；前端零改动——加密边界在落盘那一层，
`src/stores/quickCmds.ts` 与 `QuickCmdsDialog` 完全不需要知道这件事）。

### 29.5 本轮的验证

- `go vet ./...` 干净；`go test -v -count=1 ./...`：**121 PASS / 11 SKIP / 0 FAIL**（132 个 Test
  函数）——主包 126 个（116 PASS / 10 SKIP）、`internal/secret` 2 个、`internal/testssh` 4 个
  （含 1 SKIP）。11 个 skip 里 **10 个是 `requirePTY`** 触发的 PTY 环境跳过（本次环境
  `open /dev/ptmx: permission denied`，见第 7 节的提醒），另一个是 `TestFsInfoCountError`
  （本卷的文件系统拦不住 `0000` 目录的读取，模拟不出「读不出子项」）。本轮新增的用例既不依赖
  PTY、也不依赖那个文件系统特性。
- `make check`（`vue-tsc` + `check:i18n`）无错误：zh/en 各 **438** key 双向一致
  （本轮没有前端改动，也没有新文案）。
- 新增测试 `backend/quickcmds_crypt_test.go`：
  `TestQuickCmdsContentEncryptedAtRest`（内容原样往返 + 磁盘无明文内容 / 名称仍明文 + 信封形态
  （`version: 2`、`content.v == 1`）+ 重启后仍能解密）、
  `TestQuickCmdsLegacyPlaintextFile`（v1 明文文件读一次就被加密重写、内容与 `auto` 逐字节不变、
  重启后可读）、
  `TestQuickCmdsLegacyMigrationAtStartup`（**没有任何接口调用**的情况下重启一次，全局表 /
  真实会话的专属表 / 目录里存在的孤儿表**三张都被迁移**，且内容与 `auto` 不变）、
  `TestQuickCmdsLegacyEmptyContentBecomesPlaceholder`（空内容 → 占位文案的密文 + `auto` 关掉，
  磁盘上连占位文案本身都不是明文）、
  `TestQuickCmdsDecryptFailureIsReported`（字节被改 / 同表两条指令的密文互换 / 专属文件搬成
  `global.json` → 都是带指令名的 `internal` 错误，而不是空内容）。
  原有 `TestQuickCmdsGlobalAndSession` 一行未改地继续通过（含「内容全是空白 → invalid」）。
- 真二进制手测（`make` 出的 `./webssh` + 临时数据目录 + `curl --unix-socket`）：
  ① 存一条含密码与多行格式的指令 → `global.json` 是 `version: 2` + `id`/`name` 明文 +
  `content` 密文信封，磁盘上搜不到 `mysql -uroot` / `pS3cret`；
  ② 读回内容与提交的字符串**逐字节相同**；
  ③ 手工写一份第一轮的明文文件 → 照常读出（行尾换行保留），**这次读取**就把它重写成了密文；
  ④ 把密文改一个字符 → `500 {"code":"internal","error":"解密快捷指令「旧表」（legacy01）的命令内容失败：…"}`
  （不是空内容）；
  ⑤ 迁移（29.3）：手工放一份含「多行命令 + 空内容」的 v1 `global.json` 与一份孤儿表
  `orphan01.json`，**启动后不问任何接口**，两个文件都已变成 `version: 2` 的密文：多行命令的
  内容与 `auto: true` 原样保住，空内容那条换成占位文案的密文 + `auto: false`，
  `grep` 明文内容一无所获；日志里只有一行
  `快捷指令「空内容」（g2，global）的明文内容为空，已换成占位文案`。
  验完已删临时目录与二进制。
- 未做：无头浏览器里点一遍（本轮只动后端落盘格式，界面零改动；接口收发的仍是明文 `content`，
  前端行为不变——`QuickCmdsDialog` 把内容发进终端时本来就把 `\r?\n` 转成 `\r`，多行内容是有意义的）。

---

## 30. 第二十轮：快捷访问（文件管理/面包屑的书签 + 目录菜单）

### 30.1 需求

- 文件管理工具栏「视图」按钮**右侧**加一个书签图标按钮 → 打开「快捷访问」弹窗；
- 弹窗里**不预设任何路径**，支持手动输入添加、支持逐条删除、把路径列出来；
- 目录的「更多操作」菜单（文件管理行菜单 + 左栏目录树的目录菜单）与文件管理**列表/网格的
  空白处右键菜单**加「添加快捷访问」（**文件没有这一项**）；
- 左栏面包屑按钮条也加一个书签按钮，点开**同一个**弹窗；
- 点弹窗里的一条路径 = 在文件管理里进入那个目录；**没权限就报错、并且不打开文件管理 / 不进入
  那个目录**。

### 30.2 后端（`backend/quickaccess.go`）

- 存储 `quickaccess/<sid>.json`（`{version:1, paths:[...]}`，0600）。**按会话**存、没有全局表：
  路径是某台远程主机上的绝对路径，而会话身份（user@host:port）在后端是去重的，所以「按会话」
  就等于「按主机」——把 A 主机的路径挂进 B 会话，点开只能报错。删除会话时一并
  `DeleteQuickAccess`（`sessions.go` 的 `delete`，前端确认文案也加了一句「快捷访问」）。
- 接口（契约 4.2）：`POST /api/quickaccess/list` 与 `save`，**整表提交/整表返回**（与快捷指令
  同一套，去重与规整只实现一遍）。`sid` 为空 → `invalid`（这里没有全局作用域），会话不存在 →
  `session_gone`。条数上限 200（`maxQuickAccess`）、单条长度上限 4096。
- 路径规整 `sanitizeQuickPath`：必须是绝对路径、**拒绝 `..` 段**（而不是靠 `path.Clean` 折掉——
  Clean 之后是另一个目录，悄悄改掉比报错更糟），清掉重复斜杠 / `.` 段 / 末尾斜杠；
  **不 trim**（以空格结尾的目录名合法，与 `resolveRemotePath` 同一条纪律）。整表按规整后的结果
  去重并保留原顺序（先出现的留下）。磁盘上的文件读回时重新规整一遍，认不出就报错（不静默丢条目）。
- **不做加密**（与快捷指令的命令内容不同）：目录路径不是秘密，加密只会让它没法排查。

### 30.3 前端

- `stores/quickAccess.ts`（[C]，契约 6.1）：按 sid 分片 + 乐观更新/失败回滚（与 `quickCmds`
  一致），**但新增/删除都先 `ensure`**——整表覆盖的硬前提是「手上这份就是服务端那份」；同一个
  sid 上并发只发一次请求。路径校验 `normalizeQuickPath`（与后端同一套规则），**不 trim**；用户在
  输入框里手输时误带的首尾空白由**弹窗那一层**去掉（`submit` 里 `trim()`），菜单/树传进来的
  路径一个字都不动。弹窗显隐（per-sid）也放在这里：三个入口分属两个兄弟组件，弹窗本体挂在
  `SessionView` 上，显隐只能有一个共享来源。
- `stores/sftp.ts` 新增 `jumpTo(sid, path)`：**先列一次目录**（`api.fsList`），成功才写状态
  （`applyListing` 是成功路径唯一的写入点，与 `loadDir` 共用），失败**原样抛异常且不动任何展示
  状态**（`entries` / `error` / `back` 全不变）。这就是「没权限不进入目录」的实现：用户可能压根
  没在看文件管理（从面包屑按钮进来），先切过去只会看到一屏错误，还会把后退历史与当前目录一起
  毁掉；探测的那次响应直接当成本次加载的结果，一次点击只发一次请求。
- `components/files/QuickAccessDialog.vue`（挂在 `SessionView`）：头部（标题 + 关闭）/
  正文（列表，`min-h-0 flex-1 overflow-y-auto`）/ 页脚（输入框 + 「添加快捷访问」按钮）三段，
  输入框用 `id`/`for` 与可见 label 关联。加载失败显示原因 + 「重试」，**不显示成「还没有快捷
  访问」**（那会让用户以为自己加的条目被删了）。删除不弹确认框（删一条书签没有破坏性）。
  **打开时不自动聚焦输入框**（用户要求）：打开弹窗更常见的用途是「挑一条已有路径跳过去」，
  抢焦点会在移动端立刻弹起键盘盖住列表；添加成功之后焦点**留在**输入框（连着加几条不用反复点）。
  点一条路径：`jumpTo` 成功 → `sessionUi.setActiveSub(sid, FILES_TAB)` → 关弹窗；失败 →
  toast `files_quick_open_failed`（`describeError` 会把 `permission` 翻成「权限不足」）并**留着
  弹窗**（用户多半想顺手删掉这条失效路径）。
- 两个书签按钮（`FileManager` 工具栏「视图」右侧 / `BreadcrumbBar`）都只写
  `quickAccess.setDialog(sid, true)`；两个「只加不弹窗」的入口（`FileManager` 的空白处菜单与
  目录行菜单、`FileTree` 的目录菜单）走 `quickAccess.add` + toast（已在列表里时提示
  「这个路径已经在快捷访问里了」，不是错误）。
- `SessionView` 在**切走这个会话标签时**关掉弹窗：弹窗 Teleport 到 `body`，而会话视图切标签只是
  `v-show` 隐藏——Teleport 出去的节点不在那棵 `display:none` 的子树里，会一直浮在首页/另一个
  会话上面。
  **同一条规则也用在 `NewEntryDialog` 的两个宿主上**（`FileManager` / `FileTree` 各自 watch
  `appTabs.activeId`，第二十四轮补，见 33.6）：遮罩挡住顶栏只是巧合，不能当成「点不到就不用关」。

### 30.4 冻结文件（本轮）

`backend/admin_routes.go`（两条路由）、`backend/sessions.go`（删会话多清一张表）、
`backend/internal/store/store.go`（新增 `quickaccess/` 目录与两个方法）、
`frontend/src/serverapi/index.ts`（`quickAccess`/`saveQuickAccess` + 抽出 `FsListing`）、
`frontend/src/stores/{sftp,home}.ts`（`jumpTo` / `forgetSession`）、`frontend/src/i18n/*`
（新增 14 条 `files_quick_*`，改 1 条 `home_delete_confirm_msg`）、`docs/CONTRACT.md`。

### 30.5 本轮的验证

- 后端新增 `backend/quickaccess_test.go`：
  `TestSanitizeQuickPaths`（`/a//b/` → `/a/b`、`/a/./b` → `/a/b`、`/a/my dir ` 的**末尾空格保留**、
  相对路径与 `..` → `invalid`、整表顺序保留 + 去重、超过 200 条 → `too_large`）、
  `TestQuickAccessApi`（新会话读回空表、保存后的规整结果、磁盘文件 `quickaccess/<sid>.json` 的
  形态与 0600、**重启后仍在**、`sid` 为空 → `invalid`、会话不存在 → `session_gone`、非法路径 →
  `invalid`、删会话后文件被清掉且再问就是 `session_gone`）。
- `make test`：**123 PASS / 11 SKIP / 0 FAIL**（134 个 Test 函数；11 个 skip 里 10 个是
  `requirePTY` 的 PTY 环境跳过、1 个是 `TestFsInfoCountError`，与上一轮相同）。
- `make check`（`vue-tsc` + `check:i18n`）无错误：zh/en 各 **452** key 双向一致（上轮 438）。
- 真二进制 + 无头浏览器点了一遍（`make` 出的 `./webssh` + `testsshd` + `socat` 转发 unix socket，
  1280×720 宽屏判定，临时数据目录）：① 工具栏「视图」右侧出现书签按钮、左栏按钮条出现
  「快捷访问」；② 弹窗打开是**空列表**（无预设）；③ 手输 `vol1/1000` → toast「路径必须是绝对
  路径（以 / 开头），且不能包含 ..」且列表不变；④ 手输带末尾斜杠的绝对路径 → 存成规整后的路径、
  输入框清空、磁盘出现 `quickaccess/<sid>.json`；⑤ 点该路径 → 文件管理器面包屑落到那个目录、
  弹窗关闭；⑥ 对一个 `chmod 000` 的目录点路径 → toast「打开目录失败：权限不足」、**文件管理
  子标签没被切过去**（面板仍是 `invisible`）、当前目录与列表原样、弹窗留着；⑦ 点删除按钮 →
  列表少一条（磁盘同步）；⑧ 目录行菜单有「添加快捷访问」、**文件行菜单没有**；⑨ 列表与网格的
  空白处右键菜单都有「添加快捷访问」，对已在列表里的当前目录点它 → 「这个路径已经在快捷访问
  里了」；⑩ 左栏目录树的目录菜单有「添加快捷访问」，点 `/tmp` → toast 并落盘；⑪ 刷新整页后
  重新打开弹窗，两条路径都从后端读回来了；⑫ 打开弹窗后切到「会话」标签，弹窗自动关闭。
  验完已停掉进程、删掉临时目录与二进制。
- 未做：移动端（窄视口）点一遍——本轮的入口都在工具栏/左栏/右键菜单里，前两者与桌面同源，
  空白处右键菜单在窄屏本来就不弹（`mobile` prop，见 14.x），没有新增的移动端专属分支。

### 30.6 同轮追加：打开弹窗不自动聚焦输入框

用户追加要求：**弹窗打开时焦点不落在添加输入框上**。改动只有一处——`QuickAccessDialog` 的
`watch(visible)` 里删掉 `nextTick(() => inputEl.focus())`（`nextTick` 导入一并去掉）；
添加成功之后仍然把焦点留在输入框（连着加几条不用反复点，见 `submit`）。`make check` 复跑无错误；
**未再跑无头浏览器复验这一条**（首次验证里打开弹窗那一步已经证明弹窗能正常打开与加载列表，
这次只删掉了聚焦那一句）。

---

## 31. 第二十一轮：反代丢掉 Host 端口时终端 WS 全部 403（Origin 判据修正）

### 31.1 用户报告

原话：「http 环境打开前端时 进入会话文件管理正常 终端提示 ws 连接错误」。真机（飞牛网关，
`http://192.168.5.222:5666/app/WebSSH` 与 `https://…:5667/app/WebSSH` 两条都试过）的后端日志：

```
[WebSSH] 2026/10/16 00:11:00 拒绝来源不明的 WebSocket 握手：Origin="https://192.168.5.222:5667" Host="192.168.5.222" X-Forwarded-Host=""
```

前端表现：**文件管理（POST /api/fs/*）一切正常**，一打开会话终端里就打印 `term_ws_error`
（「WebSocket 错误或被飞牛关闭」），浏览器侧是 `Unexpected response code: 403`。

### 31.2 根因

飞牛统一网关把 `/app/` 交给 `trim_http_cgi` 时用的是（`/usr/trim/nginx/conf/conf.d/trim_http_cgi.conf`）：

```nginx
location /app/ {
    proxy_pass http://unix:/var/run/trim_http_cgi.socket:;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;        # $host = **不带端口**的主机名，且不补 X-Forwarded-Host
    proxy_read_timeout 300s;
}
```

第十八轮（第 28 节第 27 条）加的 CSWSH 校验把判据定成「Origin 的 `host:port` == 请求的 `Host`，
缺端口按 scheme 补 80/443」，于是 `Origin: …:5666/5667` 与 `Host: 192.168.5.222`（补成 80/443）
**永远不相等** → 每条终端 WS 都被 403 拒掉。文件管理因为不校验 Origin，所以看起来一切正常。

### 31.3 修复（`backend/terminal.go`）

- 判据拆成两层：**主机名必须一致**（大小写不敏感、IPv6 去方括号，仍是 CSWSH 的主要防线）；
  **端口只在「请求这一侧的权威值带了端口」时才比**——带了就必须相等，没带就只比主机名。
  Origin 那一侧不带端口仍按浏览器语义补默认端口（浏览器只在非默认端口时才写出来）。
  `Host` / `X-Forwarded-Host` 两项都走同一个 `originHostMatches`。
- 函数重命名：`hostPortOf(authority, scheme)` → `splitAuthority(authority)`（**不补默认端口**，
  `port == ""` 就是「这一侧没给端口」这个第三态）+ `defaultPortOf(scheme)`（只用来解释 Origin）。
- 已知取舍：`Host` 不带端口时，「同一主机名、别的端口上的页面」也会被算作同源。代价可接受——
  跨主机的页面（远程站点、别的域名）仍然被主机名比较挡住；而且这类网关后面本来就没有别的端口
  信息可用（浏览器改不了 Origin，服务端也拿不回外部端口）。
- 前端与协议都没动：`wsUrl()` 拼出来的地址本来就是对的，坏在服务端这一道校验。

### 31.4 验证

- 单元测试：`TestOriginAllowed` 新增 6 条（飞牛网关 http/https 两条现场、丢端口但主机不同、
  `X-Forwarded-Host` 丢端口 / 带端口、IPv6 丢端口）；
  `TestTerminalWSRejectsForeignOrigin` 补两条端到端断言——`Host: webssh` + `Origin: http://webssh:5666`
  必须**不再** 403，而 `Origin: http://evil.example:5666` 仍然 403。
- `make test` 全绿（本机有 PTY：主包 **128 PASS / 0 SKIP / 0 FAIL**，内置 SSH/SFTP 服务器与
  secret 包同样全过）；`make check`（`vue-tsc` + `check:i18n`，zh/en 各 452 key）无错误。
- **真二进制 + 无头 Chromium 前后对照**（`.tmp/origin/`：`testsshd` + `proxy.py` 复刻网关行为——
  「把 Host 的端口丢掉、不补 X-Forwarded-Host」+ `playwright` 驱动）：
  - 修复前的二进制（`git show HEAD:backend/terminal.go` 编出来的 `./webssh`）：浏览器报
    `WebSocket connection to 'ws://127.0.0.1:18080/app/WebSSH/api/terminal/ws?sid=…' failed:
    Error during WebSocket handshake: Unexpected response code: 403`，收到 0 字节，后端日志与
    用户现场逐字一致。
  - 修复后：握手 101、终端拿到 `\x1b]tid;…\x07` + `\x1b]ready\x07` + shell 提示符，`echo E2E-OK-…`
    的回显出现在帧里（见截图 `.tmp/origin/shot.png`）。
  - 同一支脚本改 Origin 为 `http://evil.example:5666` 仍在后端被拒（403）。
- 未做：**真机网关没能端到端复验**——线上那份 `0.4.1` 二进制没有这个修复，我也无法覆盖
  `/vol1/@appcenter/WebSSH/bin/webssh`（属主是应用用户，当前登录用户不可写）。需要重新
  `make` + 重新打包/升级 fpk 才能在真机上看到效果。间接证据：该网关的
  `trim_http_cgi` 是 Go 的 `httputil.ReverseProxy`（二进制里有 `handleUpgradeResponse` /
  `upgradeType` 符号），nginx 侧也显式转发了 `Upgrade` / `Connection: upgrade`，即 101 由网关
  中继这一环是有支持的；本轮没在真机上验证的只有这一环。

---

## 32. 第二十二轮：文件管理器的搜索（书签右侧的搜索图标 + 地址栏下的搜索栏）

### 32.1 需求（用户原话拆解）

- 文件管理工具栏**书签按钮右侧**加一个搜索图标按钮，点击后在**目录地址栏下方**显示搜索栏；
- 搜索栏内：输入框 + 数量 + 搜索按钮 + 取消；输入框支持回车（与点「搜索」同一个动作）；
- **点「取消」退出搜索并隐藏搜索栏**；点其他功能按钮**不退出搜索**；
- **仅在当前目录下搜索**（用户确认：**递归当前目录的整棵子树**）；
- **搜到一点显示一点**；**缓存搜索到的结果**；
- 点结果里的**目录**跳进那个目录，**不隐藏搜索栏**；**再点「搜索」快速回到搜索结果**；
- 点结果里的**文件**在搜索结果里预览；**编辑**也是在搜索结果里打开 / 退出编辑页；
- 面包屑（左栏目录树）**不需要**搜索功能。

### 32.2 后端（`backend/files_search.go` + `admin_routes.go` 一条路由）

- `POST /api/fs/search`，请求 `{sid, path, query}`：`path` 空 = 家目录（与 `fs/list` 同一套
  `resolveRemotePath`）；`query` 去首尾空白后为空 → `invalid`（400）。匹配的是条目**自己的
  名字**的不区分大小写**子串**（不含通配符 / 正则 / 内容搜索）。**不匹配路径**（同轮返工：
  按路径匹配时输入 `logs` 会把那个目录下的每一个文件都命中，一个常见的目录名就能刷出几千条
  ——那不是用户要找的东西）；相对路径仍由前端拼出来**显示**（让用户知道命中的项在哪一层），
  只是不参与匹配。隐藏文件不过滤。
- **流式 NDJSON**（`Content-Type: application/x-ndjson`，一条记录一行，写一行 flush 一次）：
  `{"type":"start","path":<起点绝对路径>}` → `{"type":"batch","entries":[...]}`* →
  `{"type":"progress","scanned","dirs"}`*（节流 400ms）→
  `{"type":"done","total","scanned","dirs","skipped","truncated","elapsedMs"}`。
  响应头带 `Cache-Control: no-store` 与 **`X-Accel-Buffering: no`**——网关（nginx）默认会把
  整条响应缓冲下来，不显式关掉的话「边搜边回」到浏览器就变成「搜完一次性到达」。
- **响应头懒写**（`searchStream.wrote`）：起点目录**先读出来**再开流，所以「起点打不开 /
  会话没了 / 关键词为空」这些失败都在写第一行**之前**发生，能走普通的 JSON 错误响应 + 正确的
  状态码（403 permission / 404 / 502），前端那条路是 `ApiError`。开流之后才失败的（遍历途中
  出错、客户端断流）只能作为一行 `{"type":"error","code","error"}` 写回去——裸码照样按
  `fsError` 的口径翻译（`permission` → 界面「权限不足」）。
- 遍历：**BFS**（先近后远，命中也先出现在近处），每层目录内按名字排序（顺序可复现）。
  **软链接不递归进去**：`ReadDir` 给的是 lstat 属性，指向目录的链接 `IsDir()` 为 false，
  所以不会走进死循环（`self -> .`、`/proc/self/root`）；链接自己仍会作为结果出现（口径与列表
  一致，`symlink` / `broken` 由同一个 `buildFsEntry` 填）。
- 上限（任一触顶即停止遍历并 `truncated: true`，界面说「已截断」，不假装搜完）：
  **2000 条结果**（`maxSearchResults`）、**20 万条目**（`maxSearchScanned`）、
  **10 层**（`maxSearchDepth`）、**15 秒**（`searchDeadline`）。
  子树里某个目录读不出来（权限 / 遍历途中被删）→ **跳过并继续**，计入 `skipped`；只有**起点**
  读不出来才算这次搜索失败。
  ⚠️ 第二十四轮的补强：① `collect` 内层**每 512 条**也查一次 ctx 与总时限（原来只有「一个目录」
  这个粒度，单个巨大目录里三道上限都不生效）；② 关键词先 `TrimSpace`，所以**搜不到名字以空格
  结尾的项**（路径侧刻意不 trim，见 12.2）——这是取舍，不是 bug；③ 条目名拼路径前过
  `safeRemoteEntryName`（空串 / `.` / `..` / 含 `/` 一律忽略并记日志），避免远端给的怪名字把
  子目录队列带出起点子树。详见第 34 节。
- 攒批：够 50 条立刻回，或距上次回流超过 200ms 也回（命中稀疏时保证「搜到一点显示一点」）。
- 取消：客户端 abort → `r.Context()` 结束 → 遍历每层（以及每 512 条）看一眼 ctx 主动退出；
  **读目录本身也受约束**（第二十四轮加固，见 34.2 第 3 条）：`readDirBounded` 把本次搜索的
  **剩余时间**做成 `ReadDirContext` 的 deadline、把请求 ctx 传进去——`Client.ReadDir` 内部是
  `context.Background()`，`ReadDirContext` 才在每次 readdir 往返之间看 ctx，超时时还会把
  **已经读到的条目**一起返回（照常处理，只标 `truncated`）。
  **关键纪律（第二十四轮）**：本闭包**直接写响应**，所以必须用
  `withSFTPStream`——它在丢掉连接之后会**等遍历协程退出**再返回；用 `withSFTP` 会让 handler
  返回后协程继续写 ResponseWriter（net/http 明令禁止，实测 abort 后又写了 2000 条结果）。
  写失败（客户端走了）一律返回 `errStreamClosed` 并**立刻终止**，**不**计入 `skipped`、不写
  「跳过目录」日志（那是「客户端走了」，不是「远端权限问题」）；取消引发的 `connection lost`
  同理（是我们自己关的连接）。
- 硬约束不变：目标路径 `path.Clean` + 拒绝 `..`；SFTP 通道仍由 `withSFTP`/`withSFTPStream`
  串行化（一次搜索最多占住这条会话的 SFTP 通道 15 秒，与压缩 / 导出同类）。

### 32.3 前端

- `stores/sftp.ts`：`state.search`（`SftpSearch`，见 6.1）+ `openSearch / closeSearch /
  runSearch / showSearchResults / leaveSearchResults / sortedResults`；中断控制与轮次（`AbortController` + `seq`）
  放在闭包账本里（同 `inited` / `loadedDirs` 的写法），**不进 SftpState**。
  - `runSearch`（搜索栏的「搜索」按钮 / 回车）= **永远在当前目录搜一次**；
    `showSearchResults`（搜索栏的「结果：x 项」按钮）= **回到上一次那批结果**（只切
    `viewing`，一个请求都不发）。两个动作各有一个控件——第三轮返工要求：早先它们挤在「搜索」
    一颗按钮上，要按「现在是不是正在看结果」猜这次点下去是搜还是回，用户明确要求拆开。
  - 缓存 = `results` + `scopeDir`（起点以后端 `start` 记录解析出的绝对路径为准），
    生命周期与「这一次搜索」相同：**只有 `closeSearch`（点取消）会清掉它**。
    （`scopeQuery` 在第二十四轮删除：它只写不读，见 34.5。）
  - **列表区显示结果还是目录**由 `search.viewing` 决定。**目录导航**（`navigate` / `back` /
    `forward` / `jumpTo` / `refresh`）把它置回 false —— 这就是「点结果里的目录跳进去看到的是
    那个目录的内容，而搜索栏与结果缓存都还在」。写操作之后的 `refreshDir` **不**置回
    （用户要求「点别的按钮不退出搜索」；那批结果是那次搜索的快照，想更新再点一次「搜索」）。
  - 结果**边到边追加**（`onBatch` → `results.push`），迟到的轮次靠 `seq` 丢弃（否则新一轮的
    结果会被旧一轮覆盖、或者 `running` 被旧的清掉）。
- `serverapi/index.ts`：新增 `requestStream()`（fetch + `ReadableStream` + 流式 `TextDecoder`，
  按 `\n` 分帧；开流前失败按 `res.ok` / Content-Type 判成 `ApiError`，流里的 `error` 记录也抛
  `ApiError`）与 `api.fsSearch(sid, path, query, {onStart, onBatch, onProgress, signal})`。
- `components/files/SearchBar.vue`：输入框 + **「结果：x 项」按钮** + 搜索 + 取消（左边**没有**
  放大镜图标——第三轮返工删掉，输入框自己就说明了这里能搜）；挂载即聚焦输入框；回车与「搜索」
  同一个动作（`runSearch` 抛错时 toast，比如关键词为空）。「结果：x 项」按钮只在这次搜索开始过
  之后出现（没东西可回时不显示，与剪贴板那一行同一个口径），点它就是回到那批结果（搜索中
  也在长，点它看已搜到的部分）；它的 title 在搜索中是「搜索中…（已扫描 {m} 项）」。失败原因
  在栏里给一行截断的红色文案，完整的在结果区。
- `components/files/SearchResults.vue`：结果区（行 / 网格 + 收尾统计 + 空态 + 失败原因 +
  「结果来自 <别的目录>」的提示）。四条刻意的规矩：
  1. **跟随「视图」偏好**（列表 / 网格，与目录内容同一套形态）：第一列是**名字 + 灰色的所在
     目录**（递归结果散在不同层，只给文件名没有上下文）。名字在前、截断先截目录，所以第一列
     仍叫「名称」——**表头标签与排序都不必为结果单开一套**；
  2. **行内没有「···」菜单**：行菜单的动作（删除 / 重命名 / 压缩 / 导出…）都以「这一项在当前
     目录的列表里」为前提（`removePaths` / `menuItems` 拿 `state.entries` 查表），而结果可能在
     别的目录下——所以结果的交互只有两条：文件 → 预览，目录 → 进入；
  3. **排序复用目录那一份偏好**（`stores/sftp.ts` 的 `sortedResults`，与 `sorted` 共用同一个
     `compareEntries` 与同一个 `sort`）：没点表头时保持**到达顺序**（后端 BFS 先近后远、边搜
     边追加——「搜到一点显示一点」要的就是这个顺序），点了表头就按那一列排（新到的批次也参与）；
  4. 所在目录由 `path` 裁出 `scopeDir` 前缀，**不 trim**（名字以空格结尾合法）。
- `FileManager.vue`：工具栏「快捷访问」右侧的搜索图标（`openSearchBar`：先 `leaveMulti` 再
  `openSearch`，幂等、不做开关——退出搜索的入口只有搜索栏里的「取消」）；搜索栏插在面包屑
  下方、剪贴板栏上方；列表区在 `searchViewing` 时渲染 `SearchResults`。
  **列表的表头只有一份**（在滚动容器**外面**，目录与结果共用）：它不跟着内容滚，三列排序
  （结果同样生效）与权限显示切换在两边行为一致——同轮返工要求「列表视图时标题栏不参与滚动，
  复用排序和权限显示切换」。结果区**不弹空白处右键菜单、不接拖拽上传**（那两个动作的对象都是
  「当前目录」，而结果可能来自别的目录）；`toggleMulti` 进多选前先 `leaveSearchResults`
  （多选是对当前目录挑一批，与结果区不是一回事）。
  点结果的两种走法：目录 → `go(path)`（= 现有导航，顺带切回目录视图）；文件 → 现有的预览弹窗
  （编辑走预览里的按钮 → 编辑页覆盖在文件管理上，进出都不碰文件管理器的状态，所以退出编辑
  回到的正是搜索结果）。
- 文案：zh/en 各新增 19 条 `files_search_*`。左栏 `FileTree` **没有任何搜索入口**（用户要求）。

### 32.4 冻结文件（本轮）

`backend/files_search.go`（新）、`backend/admin_routes.go`（一条路由）、
`frontend/src/serverapi/index.ts`（`requestStream` + `fsSearch` + 三个类型）、
`frontend/src/stores/sftp.ts`、`frontend/src/components/files/{SearchBar,SearchResults}.vue`（新）、
`frontend/src/components/files/FileManager.vue`、`frontend/src/i18n/*`（19 条 `files_search_*`）、
`docs/CONTRACT.md`。

### 32.5 本轮的验证

- 后端 `backend/files_search_test.go`（真链路：进程内 SSH/SFTP + unix socket HTTP）：
  `TestFsSearchSubtree`（递归命中与顺序「先近后远」、相对路径匹配 `docs/`、大小写不敏感、
  隐藏文件能搜到、目录作为结果、软链接**不递归**（扫描数断言 12 项 / 5 个目录，跟进去会翻倍）、
  空 path = 家目录、相对 path 按家目录解析、没命中回完整统计）、
  `TestFsSearchStreamsInBatches`（150 个命中分 ≥3 批、不重不漏）、
  `TestFsSearchErrors`（空关键词 / `..` / 会话不存在 / 拿文件当起点：都是「开流之前」的普通 JSON
  错误）、`TestFsSearchPermission`（起点 0000 → 403 permission 且**不开流**；子树里 0000 →
  跳过并继续，`skipped=1`）。
- `make test` 全绿（主包 **122 PASS / 10 SKIP / 0 FAIL**，本机无 PTY：10 个 PTY 用例按设计跳过，
  `TestFsInfoCountError` 在本机文件系统上拦不住 0000 目录也跳过）。
- `make check`（`vue-tsc` + `check:i18n`）无错误：zh/en 各 **470** key 双向一致。
- **真二进制 + 真 unix socket + socat 转发 + 内嵌 SSH/SFTP 服务器**（`.tmp/e2e_search.py`，
  13 项断言全过）：`Content-Type: application/x-ndjson`、`X-Accel-Buffering: no`、`start` 是
  第一行且 path 正确、命中顺序 `['APP.log','deep-app.log']`、150 个命中分 3 批到达、
  2100 个命中在 2000 条处截断（`truncated=true`）、没命中回完整统计、
  空关键词 400 `invalid`、起点无权限 403 `permission`、子树里一个目录被跳过（`skipped=1`）。
  （脚本在 `.tmp/` 下，gitignore，clone 出来没有——与第 12.3 节的 e2e 脚本同一处境。）
- **真二进制 + 无头 Chromium 点了一遍**（1280×720，`socat` 转发 unix socket，临时数据目录）：
  ① 工具栏「快捷访问」右侧出现搜索图标，点击后**地址栏下方**出现搜索栏（图标 / 输入框 / 「0 项」/
  「搜索」/「取消」）、输入框自动聚焦；② 输入 `app` + **回车** → 结果区出现 `notes/APP.log`、
  `docs/sub/deep-app.log`（相对路径），数量变「2 项」，收尾统计「已扫描 2263 项、5 个目录，
  用时 0.1 秒，1 个目录读取失败已跳过」（那个目录是被 chmod 000 的）；
  ③ 点文件行 → 预览弹窗（内容正确）；弹窗里点「编辑」→ 编辑页覆盖在文件管理上；点「退出」→
  **回到搜索结果的同一屏**；④ 搜 `docs/` → 结果含目录 `docs/sub`；点它 → 面包屑进到
  `…/root/docs/sub`、列表显示该目录内容，**搜索栏与「3 项」都还在**；⑤ 再点「搜索」→
  一次请求都不发就回到那 3 条结果；⑥ 看结果时再点「搜索」→ 在**当前目录**（docs/sub）重搜
  → 0 项 +「没有找到匹配的项」；⑦ 点「上一层」→ 列表切回目录内容、搜索栏仍在；再点「搜索」→
  结果回来并出现「结果来自 …/docs/sub（再点「搜索」在当前目录重新搜索）」（当前目录已不在那棵
  子树里了）；⑧ 点「取消」→ 搜索栏消失、图标不再高亮、列表是目录内容。
  验完已停掉进程、删掉临时目录；`./webssh` 是最后的 `make` 产物（`.gitignore` 里有它，留着方便直接
  拿去部署 / 复验）。
- 浏览器实测抓到并修掉一个真 bug：结果区上方**还画着目录那份表头**（「名称 / 大小 / 修改时间 /
  权限」），与结果的「路径 / 大小 / 修改时间 / 权限」叠成两行——目录表头加 `!searchViewing`。
- 未做：**移动端（窄视口）点一遍**——搜索栏与结果区是桌面同一套 DOM（`mobile` prop 只影响
  权限列），没有移动端专属分支；未验证的是窄屏下「输入框 + 数量 + 两个按钮」那一行的挤压表现。
  另外未验证：**两个设备同时对同一会话搜索**（第二个请求会等第一个跑完，SFTP 串行化的既有
  语义），以及长搜索过程中点「取消」后下一次文件操作的**重拨延迟**（与既有的
  「取消上传 / 关页面」同一条路径）。

### 32.6 同轮返工：结果跟随视图 + 复用表头 + 只按名字匹配

用户两条要求：① 搜索结果**跟随当前的文件管理显示视图**（列表 / 网格），列表视图时**标题栏不参与
滚动**，并**复用排序和权限显示切换**；② 匹配机制改成**只匹配名字**——「只显示含搜索词的文件和
目录，而不是把含搜索词的路径的所有文件和目录都显示，那样量太大了」。

- **后端只匹配名字**：`matchSearch(name, needle)`（`files_search.go`），不再拿相对路径去匹配
  （`relOf` 随之下线，成了死代码）。效果：搜 `docs` 只出目录 `docs` 自己，`docs/readme.md`、
  `docs/sub/…` 一条都不出；路径里含关键词而名字里不含的（`docs/sub/deep-app.log` 搜 `docs/sub`）
  也不再命中。递归范围、顺序（BFS 先近后远）、上限与流式形态都没变。
  测试相应改成断言新口径（`TestFsSearchSubtree`：`docs` 只命中目录本身、`docs/sub` 命中 0 条、
  `readme` 仍能递归命中；以及目录仍以 `dir=true` 出现）。
- **表头一份、且不滚动**：`FileManager` 里那份列表表头重新变回 `v-if="state.view === 'list'"`
  （目录与结果共用），它本来就在滚动容器**外面**，所以「标题栏不参与滚动」是结构上就成立的；
  `SearchResults` 删掉了自己那份表头（原来它画在滚动容器里，会跟着内容滚）。
  第一列的标签**没有**换成「路径」：结果行的第一列改成「**名字** + 灰色所在目录」（名字在前，
  截断时先截目录），所以「名称」这一列名、三列排序与权限切换（`sftp.toggleMode`）全部原样复用。
  行尾补一个 `w-6` 空档对齐表头最后一列（表头那列是行内「···」的位置，结果行没有菜单但列宽
  必须一致，否则 `flex-1` 一宽一窄、后面几列整体错位）。
- **网格视图**：`SearchResults` 增加与目录网格同一套形态的分支（图标 + 名字两行 + 灰色所在目录
  + 大小/目录），`content-start` 照旧不能省（契约 14.5 那个「整列悬停高亮」的坑）。
- **排序复用**：`stores/sftp.ts` 新增 `sortedResults(sid)`——与目录的 `sorted(sid)` 共用
  `state.sort` 与 `compareEntries`；`sort.key === null` 时返回**到达顺序**（不重排），点了表头
  就按那一列排（新到的批次也参与重排，这是用户自己要的）。
- 文案：`files_search_placeholder` 由「名称或路径」改成「名称」（中英各一处），
  `files_search_path`（只给结果表头用的「路径」标签）随之下线，zh/en 回到 **469** key。
- 验证：`make test` 全绿（主包 122 PASS / 10 SKIP / 0 FAIL）；`make check` 无错误（469 key）。
  **真二进制 + 无头 Chromium 又点了一遍**：① 搜 `app` → 结果第一列是「APP.log notes/」这样的
  「名字 + 灰色所在目录」，表头是「名称 / 大小 / 修改时间 / 权限」且**滚动结果列表时标题栏不动**
  （表头在滚动容器外）；② 点表头「大小 升序」→ 结果按大小重排（表头箭头出现）；点「权限」列 →
  结果的权限列在符号 / 八进制之间切换；③ 点工具栏「网格视图」→ 结果变成网格（名字 + 灰色目录 +
  大小/DIR 三行），点回列表视图恢复；④ 搜 `docs` → 只有目录 `docs` 一条（不再把
  `docs/readme.md`、`docs/sub/…` 一起倒出来）。同一支脚本还断言了 `docs/sub` 命中 0 条
  （路径里含关键词、名字里不含）。
  另：点表头「大小」→ `aria-label` 在「升序 / 降序」之间循环（排序对结果生效）；滚动结果列表
  800px 前后表头 `top` 都是 205（**标题栏不跟着滚**）；点「权限」列 → 结果行的权限变成 `0705`
  （符号 ↔ 八进制切换对结果同样生效）。验完停进程、删临时目录。

### 32.7 同轮三轮返工：「回到结果」与「在当前目录重搜」拆成两个控件

用户原话：「进入搜索结果里的目录之后点搜索回到搜索结果的逻辑消失了，把搜索结果的数量改成按钮
形式『结果：x项』，缓存搜索结果，这样进入搜索结果中的目录之后与当前在结果目录中再搜索的逻辑也
就不冲突了；删掉搜索栏左侧的搜索图标」。

- **根因**：上一版把「开搜」和「回到上次的结果」挤在**同一颗「搜索」按钮**上（看结果时点 = 在
  当前目录重搜、没看结果时点 = 缓存命中就秒回），于是用户想「回到结果」时点它，得先判断自己
  此刻在不在看结果——从结果跳进某个目录之后（列表区显示的是那个目录），那颗按钮的含义在用户
  眼里就变成了「在当前目录重搜」，看起来「回到结果的逻辑消失了」。两个意思一颗按钮，怎么调都
  会有一半场景别扭。
- **现在**：`runSearch`（搜索 / 回车）**永远**在当前目录搜一次；`showSearchResults` 是**唯一**的
  「回到结果」入口（不发任何请求）。缓存（`results` + `scopeDir`）依旧在，直到
  点「取消」退出搜索才清掉——「进结果里的目录 → 点『结果：x 项』回结果 → 点『搜索』在当前目录
  重搜」这三步互不干扰。
- **数量变按钮**：`files_search_count` / `files_search_running` / `files_search_rerun` 三条文案
  下线，换成 `files_search_results`（`结果：{n} 项`）、`files_search_results_title`（`回到搜索结果`）、
  `files_search_run_title`（`在当前目录搜索`），`files_search_scanning` 改为同时做「搜索中」的
  title 与结果区空态文案。没有可回的结果（还没搜过）时那个按钮不出现。
- **删掉搜索栏左侧的放大镜图标**（输入框的 `aria-label` 保留，那是可访问名，不是可见装饰）。
- 验证：`make test` 全绿（主包 122 PASS / 10 SKIP / 0 FAIL）；`make check` 无错误
  （zh/en 各 469 key：−3 +3）。**真二进制 + 无头 Chromium**（`performance` 里的
  `/api/fs/search` 条目数就是「发没发请求」的硬证据）：
  ① 刚打开搜索栏时一排只有「输入框 / 搜索 / 取消」（左侧没有图标，也还没有结果按钮）；
  ② 搜 `app` → 出现「结果：2 项」（那一排变成四个控件）；③ 搜 `docs` 后点结果里的目录行
  `docs` → 面包屑进到 `…/root/docs`、列表显示那个目录的内容，**「结果：1 项」按钮还在**；
  ④ 点「结果：1 项」→ 请求数 2 → **2**（一个请求都不发）回到那 1 条结果；
  ⑤ 再点「搜索」→ 请求数 3，在当前目录（`root/docs`）里重搜 `docs` → 0 项 +
  「没有找到匹配的项」，按钮变「结果：0 项」；⑥ 点「结果：0 项」→ 请求数仍是 3，回到那次空结果；
  ⑦ 点「取消」→ 搜索栏消失、工具栏那个搜索图标不再高亮。

---

## 33. 第二十三轮：新建目录 / 新建文件合并成一个「新建」

### 33.1 需求（用户原话拆解）

- 文件管理工具栏原来有**两个**图标（新建文件 / 新建文件夹）→ **只留新建文件那个图标，
  改叫「新建」**；点开的那一个弹窗里，原来的「确定」换成**「新建目录」与「新建文件」两个按钮**，
  名字校验沿用已有的那套。
- 左栏目录树的「…」菜单与空白处右键菜单里的「新建文件夹 / 新建文件」也**并成一项「新建」**，
  点它复用同一个弹窗。

### 33.2 前端

- **新增 `components/files/NewEntryDialog.vue`**（契约 6.2 组件表已登记）：一个输入框 +
  `取消 / 新建文件夹 / 新建文件`。两个动作按钮**本身就是「要建什么」的开关**，不必先选类型
  再点确定。
- **两个入口合并成一个 `openNewEntry()`**，三处调用点共用：
  - `FileManager`：工具栏（父目录 = 当前目录）、空白处右键菜单（父目录 = 当前目录）；
  - `FileTree`：目录菜单（行的「…」与空白处右键共用同一份 `dirMenuItems`，父目录 = **开弹窗
    那一刻**从菜单里快照下来的 `menu.path`——`hideMenu()` 之后菜单状态不再是「用户右键的那一个」，
    而弹窗要一直开着等用户输名字、还可能校验失败重来）。
- **校验逻辑原样复用**：两个组件各自的 `badName()`（非空、不含 `/`、不是 `.` / `..`）一个字
  没改，toast 文案也还是 `files_name_required` / `files_name_invalid`。
- **与 `PromptDialog` 的差别（刻意的，写在组件头注释里）**：`NewEntryDialog` 的提交**不自我
  关闭**——校验在调用方，过不了只 toast 原因、弹窗留着（`PromptDialog` 那套「先关再报错」在这
  个场景下要用户重新打开、重新输一遍）。所以它的 emits 是 `submit(kind, value)`，关不关由
  调用方按校验结果决定。
- 输入框里**回车 = 新建文件**（合并后保留下来的入口就是「新建文件」那个图标，回车给默认动作）；
  Esc / 点遮罩 / 「取消」都是取消。
- `PromptDialog` **仍然保留**（重命名、以及别的模块在用的场景），只是文件管理/目录树不再用它
  来新建。
- 多选态下点「新建」照旧先 `leaveMulti()`（19.4：任何工具栏动作都先收起多选）。
- i18n（两套字典同步改）：
  - 新增 `files_new`（`新建` / `New`）——工具栏 title/aria-label、两处菜单项、弹窗标题都用它；
  - **下线** `files_new_file_title` / `files_new_dir_title`（唯一引用者就是被合并掉的那两个
    弹窗标题）；
  - `files_new_file` / `files_new_dir` 保留，改当弹窗里那两个按钮的文案。**用户说的「新建目录」
    在本界面里一直叫「新建文件夹」**（14.4 起就是这个口径），不另造一套说法。

### 33.3 同轮顺带修正

- `FileTree` 那个 `PromptDialog` 现在**只**服务重命名了，标签从 `files_new_name_prompt`（名称）
  换成 `files_rename_prompt`（新名称）——它原来为兼顾「新建」与「重命名」两个场景写的是「名称」，
  与文件管理器那边的重命名弹窗不一致。

### 33.6 第二十四轮补充：切走应用标签时关掉「新建」弹窗

`NewEntryDialog` 同样 Teleport 到 `body`（在会话视图那棵 `v-show` 子树之外），所以它的两个宿主
（`FileManager.vue` / `FileTree.vue`）各自 `watch(() => useAppTabsStore().activeId, …)`：**切走
这个会话的应用级标签就 `visible = false`**；**同一会话内切子标签不关**（`activeId` 不变，弹窗要
留着）。判据只能是应用级标签——这与 `SessionView` 给 `QuickAccessDialog` 写的那条 watch 是同一条
规则（见 30.3），不要依赖「遮罩盖住了顶栏所以点不到」。

### 33.4 冻结文件（本轮）

- `frontend/src/i18n/zh.ts`、`frontend/src/i18n/en.ts`（新增 1 条、删除 2 条 key，两套字典一一
  对应，见 33.2）。其余冻结文件未动。

### 33.5 本轮的验证

- `make check`：`vue-tsc` 无错误；`check:i18n` 双向 OK（**zh/en 各 468 个 key 一一对应**，
  −2 +1），代码里静态引用的 key 全部存在。
- `make test`：后端全绿（本轮没动后端）。
- `make`：前端 dist → embed → `./webssh` 构建成功。
- **真二进制 + 无头 Chromium**（`testsshd` + `socat` 桥到 127.0.0.1:18080，1280×720，会话用
  `/api/sessions/{test,trust,save}` 建好，远端根目录里预置 `sub/` 与 `existing.txt`）：
  1. 文件管理工具栏上**只有一颗**「新建」图标（原来 `新建文件` + `新建文件夹` 两颗），
     `title`/aria-label 均为「新建」；
  2. 点它 → 弹窗标题「新建」、标签「名称」、三个按钮 `取消 / 新建文件夹 / 新建文件`；
  3. 输入 `bad/name` 点「新建文件夹」→ toast「名称不能包含 /」、**弹窗仍开着且保留输入**
     （磁盘上没有产生任何东西）——校验与「失败不关弹窗」都对；
  4. 改输入 `newdir1` 点「新建文件夹」→ toast「已创建「newdir1」」、弹窗关闭、列表出现
     `newdir1`（`ls` 确认是目录）；
  5. 再打开弹窗输入 `newfile1.txt` **回车** → 关闭并创建文件（`ls` 确认 0 字节普通文件）；
  6. 文件管理**空白处右键** → 菜单是 `新建 / 上传 / 信息 / 打开终端 / 复制完整路径 /
     添加快捷访问 / 刷新`（原来的两项并成了一项），点「新建」打开的就是同一个弹窗；
  7. 左栏目录树的**行「…」菜单**（右键 `/tmp` 行）→ 第一项为单条「新建」，点它 → 同一个弹窗，
     输 `webssh-tree-newdir` 点「新建文件夹」→ `/tmp/webssh-tree-newdir` 真的建出来了；
  8. 目录树**空白处右键**（根菜单）同样只有一条「新建」；点别处菜单正常消失。
- **没有被自动化点过**：网格视图下的空白菜单（与列表视图共用同一个 `blankMenuItems`，第 6 条
  即覆盖它的取数逻辑）、移动端窄屏下弹窗三个按钮的换行表现（面板 `max-w-sm` + 三个按钮合计
  约 252px，360px 宽屏下仍有余量，但没实机截图）。

---

## 34. 第二十四轮：审查问题的修复与取舍（`.tmp/审查报告-第二十四轮-2026-10-16.md`）

**基线** `4cd9b35`。这一轮是「审查 + 修复」：4 路并行复审（搜索后端与协议 / 快捷指令加密与
快捷访问 / 前端新代码 / 上轮结论逐条复核）+ 真二进制运行时探针（38 项断言全过）。
下面只记**改动与取舍**；问题本身的描述留在报告里。

### 34.1 门禁（修完后的实测）

`go vet` 干净 · `make test` 主包 **129 PASS / 10 SKIP / 0 FAIL**（skip 全是 `requirePTY` +
`TestFsInfoCountError`）· `CGO_ENABLED=1 go test -race .` 通过 · `npm run type-check` 0 错误 ·
`check:i18n` zh/en 各 **468** · 前端构建 + `go build` 产出 `./webssh`。

### 34.2 搜索后端（`files_search.go` / `sftp.go` / `files_preview.go`）

1. **`withSFTPStream`（新增）+ handler 与遍历协程 join**：搜索是唯一在 `withSFTP` 闭包里写响应
   的接口。`withSFTP` 在 ctx 结束时立刻返回、闭包还在协程里跑，于是 handler 返回后协程继续写
   `ResponseWriter`（net/http 明令禁止；实测 abort 之后又写了 41 次 / 453 KiB = 全部 2000 条结果）。
   现在 `withSFTPStream` 在丢掉连接后**等闭包退出**（最多 `streamJoinTimeout` = 5s，超时只记
   日志——宁可留一条日志，也不为了等协程把 `sftpMu` 无限期攥住）。`withSFTP` 语义不变
   （它**不等**，正确性依赖「闭包不碰响应」这条纪律），两者共用 `withSFTPJoin`。
   回归用例：`TestFsSearchStopsWritingAfterCancel`。
2. **`errStreamClosed` 哨兵**：闭包里的写失败（客户端走了）与「这个子目录读不出来」必须分开。
   原来任何 error 都走 `skipped++` + `logx.Warnf("搜索跳过目录 …：connection lost")`——把「没人
   读了」说成「远端权限问题」，统计里还多一个目录。现在：写失败 → `errStreamClosed` → 直接终止、
   不记 `skipped`、不写「跳过目录」；取消引发的 `connection lost` 同理（是我们自己 `dropSFTP`
   造成的，见 `run` 里 `w.ctx.Err() != nil` 那一档）。
   回归用例：`TestFsSearchWriteFailureNotCountedAsSkipped`、`TestFsSearchCancelInducedReadFailureNotSkipped`。
3. **目录读取本身也受 ctx/时限约束（`readDirBounded`，同轮加固）**：
   - `collect` 内层每 512 条查一次 ctx 与总时限（原来只有「一个目录」这个粒度）；
   - 读目录改用 **`Client.ReadDirContext(deadlineCtx, dir)`**：`Client.ReadDir` 内部是
     `ReadDirContext(context.Background())`，所以「单个巨大目录」内部既取消不动、也不受 15 秒
     限制（这条请求还占着会话的 `sftpMu`，一个几十万条目的目录会把整个文件管理拖住）。现在
     **剩余时间**就是这次 `ReadDirContext` 的 deadline、**用户的取消**就是它的父 ctx；
     超时时库会把**已经读到的条目**一起返回，我们照常处理这部分并标 `truncated`（界面照实说
     「已截断」）。起点目录走同一条路（起点本身也可能是个巨大目录）。
   - 回归用例：`TestFsSearchReadDirBoundedRespectsDeadline`（时间已用光 → 不真读、不报错、
     `truncated=true`）、`TestFsSearchReadDirBoundedReportsClientGone`（已取消 → `errStreamClosed`）。
   - **仍然存在的边界（如实写）**：`pkg/sftp` v1.13.11 没有分批读目录的接口，所以**内存峰值仍是
     一个目录的完整列表**（现在受时间约束，且读完后立刻按 `maxSearchScanned` 的剩余预算截断）。
     `pkg/sftp` 若将来提供分批 API，这是第一顺位。
4. **`safeRemoteEntryName`**：条目名来自对端，拼路径前挡一道（空串 / `.` / `..` / 含 `/` 忽略并
   记日志），避免远端给的怪名字把子目录队列带出起点子树（ReadDir 正常不会给出这种名字）。
   回归用例：`TestSafeRemoteEntryName`。
5. **图片预览的「读的过程中变大」文案**：上轮 F-22 只修了 `fs/read` 那一侧，image 那一处漏了
   （它报的体积恒等于上限 + 1 字节，于是出现「图片 4.0 MB 超过预览上限 4.0 MB」）。现在改成
   「图片在读的过程中变大，超过预览上限 4.0 MB」。
6. **关键词 `TrimSpace` 的取舍写进 32.2**：搜不到「名字以空格结尾」的项（路径侧不 trim 是刻意的）。

### 34.3 流式接口的两条纪律（写给下一个改搜索的人）

- 在 `withSFTP` 闭包里写响应 → 必须用 **`withSFTPStream`**。
- 闭包里的「写不出去」→ 返回 **`errStreamClosed`**，别混进 `skipped`。
两条都记进了 AGENTS 第 5 节，一条是 net/http 的硬要求，一条是日志/统计的归因正确性。

### 34.4 快捷指令（`quickcmds.go` / `internal/store/store.go`）

1. **读路径与保存的互斥（`quickCmdsLocks`）**：`loadQuickCmdsFile` 的「读 → 迁移 → 重写」带写
   副作用，与 `saveQuickCmds` 的整表写交错时，迁移那份**过期快照**会覆盖用户刚保存的表
   （复审探针真并发 200 轮命中 6–9 轮，静默数据丢失）。现在两者各持同一把**按作用域**的锁
   （`store.WriteJSON` 的锁只串行单次写，覆盖不了这个跨步序列）。`migrateAllQuickCmds` 不自己
   持锁（锁在 `loadQuickCmdsFile` 里取），启动路径与请求路径不会死锁。
   ⚠️ **触达前提要说清**：正常启动时 `migrateAllQuickCmds` 在 `srv.Serve` **之前**跑完，所以
   「升级后第一次读某张表」这条常规路径上并不存在并发；真正会撞上的是「运行期数据目录里又出现
   明文表」（手工从备份恢复 `quickcmds/*.json`、上一次迁移失败留下的重试）。锁仍然值得加：
   它是 10 行，且这类残留在真实运维里出现过。回归用例：`TestQuickCmdsMigrationSaveLostUpdate`。
2. **非法/重复 id 重发**：手工文件里 id 非法（含 `:` 等，`!store.ValidID`）或同表重复时，迁移
   用 `store.NewID()` 重发（表内第一个出现的保留）并按新 id 重新加密；解不开的密文条目在重发
   时**原样保留 + warn**（内容本来就取不回来，改 id 只会把「读表报错」变成「内容悄悄错位」）。
   回归用例：`TestQuickCmdsReissuesDupAndInvalidIDs`。
3. **`QuickCmdsScopes` 收紧**：只认 `global.json` 与 `<合法会话 id>.json`；`.json`（旧写法削成
   空串 → 会被当成**全局**表去迁移）、`a b.json`、`a:b.json` 一律跳过并记日志。
   回归用例：`TestQuickCmdsScopesOnlyAcceptsScopeFilenames`。
4. **AAD 的残余歧义（记录，刻意不改）**：`webssh:v1:quickcmd:<id>:<作用域>` 用 `:` 拼段，
   所以 `(id="x", sid="a:b")` 与 `(id="x:a", sid="b")` 拼出同一个 AAD、密文可互换（实测）。
   **改编码会让磁盘上已有的密文全部解不开**（要另做一次全量重加密），所以本轮改用「把两个入口
   堵死」：非法 id 重发（上一条）+ 非法作用域文件名跳过（上上条）。
5. **删会话顺序**：改为**先删会话配置**，成功后再删历史 / 专属快捷指令 / 快捷访问（附属删除失败
   只记日志）。反过来会在 `DeleteSession` 失败时留下「会话还在、附属表没了」且无法恢复。

### 34.5 前端

- **快捷访问加载失败可见**（`stores/quickAccess.ts`）：`failed` 原来唯一的写入点是 `false`，
  于是 `isFailed` 恒假——`/api/quickaccess/list` 失败时弹窗显示「还没有快捷访问路径」，
  原因与「重试」按钮全是死代码（违反 30.3 与 AGENTS「失败必须给原因」）。现在失败置 `true`
  并保留 `loaded=false`（下次 `ensure` 会再拉），弹窗显示原因 + 可重试。
- **`SftpSearch.scopeQuery` 删除**：32.7 之后没有读取者（缓存只服务「回到上次结果」，只切视图），
  留着是死字段；`stores/sftp.ts` 里那句「与输入框一致才算缓存命中」的注释同步删掉。
- **「新建」弹窗随切走会话标签关闭**：`NewEntryDialog` Teleport 到 body，不会被会话视图的
  `v-show` 隐藏，与 `QuickAccessDialog` 同一条规则（`FileManager` / `FileTree` 各自 watch
  `appTabs.activeId`）。
- **注释纠偏**：`FileManager` 里「表头 / 行 / 空态都在 `SearchResults` 里」与实现相反
  （32.6 起表头只有一份、在滚动容器外）；`stores/sessionStats.ts` 的「见 CONTRACT 23.6」改成
  第 24 节；`serverapi/index.ts` 的搜索注释从「按相对路径匹配」改成「按名字匹配」（32.6）。

### 34.6 本轮**刻意不做**的决定（避免下一轮重复审）

1. **不给 POST 接口加 `Origin` 校验、也不要求 JSON `Content-Type`**。审查里曾把它列为加固项
   （跨站 simple request 在协议层可用；是否成功取决于飞牛网关 cookie 的 SameSite，未能证实）。
   用户明确：**终端 WS 校验 Origin 这件事不是问题，以后不要再验证，也不要尝试改动**。
   → 后续任何一轮审查都不再把这条列为发现。
2. **搜索结果区禁用右键菜单不补反馈**（用户明确：不需要反馈）。`FileManager.onBlankContext`
   在 `searchViewing` 时提前返回，因此自定义菜单与原生菜单都不出现——**这是刻意的**，
   别再当成「点了没反应」的缺陷。
3. **单个大目录的 `ReadDir` 不改**（见 34.2 第 3 条的说明）。

## 35. 第二十五轮：启动恢复停在首页 + 其他设备的会话标签 / 子终端同步

### 35.1 用户报告

1. **移动端进入前端应该先进会话首页**，而不是直接进入恢复的会话中；
2. 当一个会话已经被（另一台设备）打开后，**其他设备打开前端只能看见已连接的会话卡片**，
   没有会话标签和子终端列表，必须先把会话关掉才能重新进入。

### 35.2 根因

- `App.vue` 启动时调用 `home.restore()`，而 `restore()` 用 `openSession()` 逐个恢复标签，
  `openSession` 会写 `activeId`——最后恢复的那个会话成了当前视图。手机上一打开就钻进终端，
  且这个终端会立即 attach（单挂载点语义下会顶掉别的设备）。
- 恢复只发生在 `onMounted` 一次。移动端把页面当 PWA 常驻（切回前台不重新加载）时，本地
  `openStates` 停在旧快照上：卡片可能已经显示为「已打开」，但本机没有应用标签、`terminals`
  里也没有子终端——卡片上唯一的动作是「关闭」，于是只能「关掉再打开」。
- 即使恢复到标签，`v-for` 也会把所有 `SessionView` 立刻挂载，后台标签的终端会直接 attach。
  仅仅打开前端就会抢占其他设备的终端。

### 35.3 修复

- `home.restore(force?)`：先 `load()`（`force` 时强制重拉 `/api/sessions`），把 `open` 列表
  投影成**应用标签 + 终端子标签**，但用 `appTabs.openSession(sid, label, false)` **不激活**。
  返回 sid 列表仍用于启动 toast。`terminals.ensure` 是幂等的，所以后台同步也能把别的设备
  新开的终端补进卡片列表。
- `App.vue`：启动后停在首页。新增 `enteredSessions`（reactive Set），`SessionView` 只渲染
  「进入过的」会话（外加当前激活标签兜底）；进入过一次后保持挂载（v-show），面板规则不变。
  新增 `syncOpenSessions()`：`visibilitychange`（可见时）/ `focus` / 切回首页时
  `home.restore(true)`，单飞执行、页面隐藏时跳过。后台同步的加载是 `quiet` 的
  （`load(force, true)`）：失败保留现有卡片，不把首页换成「加载失败 + 重试」。
- `SessionView.onMounted`：进入时 `terminals.ensure`，仍无终端就 `terminals.add`——
  「进入恢复的会话落在终端 1」这条 15.3 的体验保留，只是时点从「启动」挪到「进入」。
- `terminals.ensure` 不再预置 `restoring: true`：恢复的视图可能不挂载，预置会让卡片状态点
  永远停在「恢复中」的琥珀色；真正的置位在 `TerminalPane.openSocket`。
- 卡片新增「进入」按钮（仅 `state.open === true`）：走 `home.openSession`（后端幂等）→
  激活标签 → `ensure` / `add`。这是「本机没有标签/子终端」时的兜底入口，不必先关再开。
- i18n：新增 `home_enter`（进入会话 / Enter session）。

### 35.4 验证

- `npm run type-check`、`npm run check:i18n` 通过。
- Playwright（mock `/api/info`、`/api/sessions`，移动端 390×844 与桌面 1280×800）：
  1. 启动后激活标签是「会话」，会话标签「测试」存在，卡片出现「终端 1」行；
  2. 先加载「无已打开会话」的页面，再把后端快照改成 open 并触发 `focus`：会话标签与卡片
     子终端列表补上，**当前视图仍是首页**；
  3. 点会话标签 → 进入会话（子标签 `文件管理 / 终端 1`）；回首页后点「进入会话」按钮 →
     切回该会话；
  4. `open` 但 `terminals: []` 的会话：进入时才建「终端 1」，未进入时卡片没有子终端行。
- 未在真机 / 真实 PTY 上重跑：本机沙箱打不开 `/dev/ptmx`（`open /dev/ptmx: permission denied`），
  WebSocket 挂载路径沿用旧逻辑未改；构建后需按第 10.3 节手测一次「另一台设备顶号」。

## 36. 第二十六轮：会话打开期间关闭「保存凭据」/ 修改凭据

### 36.1 用户报告

会话已经登录、正在用时，打开「编辑会话」把「保存凭据」关掉并保存，回到会话里新建的子终端
全部登录失败（看起来像「终端不存在」）。用户建议：切换保存状态 / 修改密码密钥保存后提示
「建议重新打开会话」，或者直接阻止新建子终端并提示。

### 36.2 根因

- `save` 在 `saveCredential=false` 时会把 `sc.Password` / `sc.PrivateKey` / `sc.Passphrase`
  连同指纹一起清掉；但会话**仍然开着**，它的新终端与 SFTP 重连走 `dialCredential`：
  此时 `CredentialsSaved()==false`、`memCreds` 里又没有那份凭据（它来自落盘，不是一次
  「输入凭据的 open」），于是 `no_credential`。
- 更糟的是 WS 新建终端把所有失败都当 `gone` 发：前端把标签标成「终端不存在」并弹
  `term_not_found`，「会话没有可用的凭据」这句真正的原因反而被覆盖。用户每点一次「+」
  就多一个失败标签，完全看不出该改什么。

### 36.3 修复

- **后端 carry over**（`sessions.go` 的 save）：`!saveCred && openNow && cur.CredentialsSaved()`
  且认证方式未变、本次没带新凭据时，把当前会话正在用的凭据放进 `memCreds`（只活到 close）：
  **优先保留已有内存凭据**（可能是这次 open 当场输入并验证成功的，比磁盘上的旧密文更可信），
  没有才 `storedCredential(cur)` 解密旧配置。用户在同一请求里给了新凭据时以新凭据为准；
  认证方式变了则不 carry over（类型不匹配的缓存会让下一次拨号报出与原因不符的
  no_credential）。API 调用方传空密码不会覆盖内存里那份能用的（前端表单不会发空串）。
  「保存凭据」关掉仍只影响持久化，不影响当前已打开会话的可用性。
- **WS 错误分流**（`terminal.go` + `protocol.go` + `serverapi/index.ts`）：
  - 新建终端失败且 `apiErr.Status == 404`（会话/运行时不存在）→ 仍发 `gone`；
  - 其余失败（`no_credential` / `auth_failed` / `hostkey_*` / `shell_failed` / 拨号超时…）→
    发原因文本 + close **4002**（`wsCloseCreateFailed`），**不带** `gone`。
    `no_credential` 的文本追加「请重新打开会话以重新输入凭据，或在会话设置里重新开启
    「保存凭据」并填写凭据」。
  - `TerminalPane` 收到 4002：保留标签、状态置 `error`、补一句 `term_create_failed`
    （修正凭据后可以点「重连」），不再走「终端不存在」的清理分支。
- **表单提示**（`SessionFormDialog`）：会话开着且本次保存涉及凭据时，把普通的
  「已保存」换成：
  - 关闭保存：`form_cred_off_open`——当前会话关闭前仍可用，重新打开要重新输入；
  - 修改密码/密钥（或切换认证方式）：`form_cred_changed_open`——已打开的终端仍是旧连接，
    新终端用新凭据，需要全部生效请重新打开会话。
  只改名/备注/端口时仍是「已保存」。
- i18n 新增 `form_cred_off_open` / `form_cred_changed_open` / `term_create_failed`。

### 36.4 取舍

- **不阻止新建子终端**：会话开着时新建终端是正常操作；问题出在凭据被过早丢弃与错误文案，
  修好这两处后没有阻止的必要。真正拿不到凭据时，终端里会有一句可执行的指引。
- **不自动重连已有终端**：改密码/密钥后已有的 SSH 连接不会断（也不该偷偷断）；提示明确
  说了「已打开的终端仍是旧连接」，需要全部生效时用户自己重新打开会话。
- **不改 `saveCredential=false` 的持久化语义**：carry over 只到 close；关闭/进程退出后
  仍然要求重新输入。

### 36.5 验证

- `TestCredentialSaveSwitchEdits`（扩展）：会话开着时关掉保存开关后，
  `cachedCredential(sid)` 必须存在，丢掉 SFTP 通道后 `/api/fs/list` 仍能重连成功；
  close 后缓存消失、open 回到 `credential_required`。
- `TestCredentialSaveOffKeepsVerifiedMemoryCredential`（新增）：内存里有一份「本次 open
  验证成功」的凭据时，关掉保存开关不能被磁盘上的旧密码覆盖；API 传空密码也不能把它清空。
- `TestTerminalCreateFailureIsNotGone`（新增，不需要 PTY）：抹掉内存凭据后新建终端，
  收到的文本要包含「没有可用的凭据」与「请重新打开会话」，**不能**包含 `\x1b]gone\x07`，
  关闭码必须是 4002。
- `make test`、`make check` 全绿；前端表单提示用 mock 数据在开发服务器上人工点过
  （真实 PTY 受本机 `/dev/ptmx` 限制未跑）。

## 37. 第二十八轮：启动恢复的并发加载竞态（没有会话标签 / 子终端列表）

### 37.1 用户报告

桌面端进入前端时，顶栏只有「会话」标签，卡片上也没有子终端列表（卡片本身、系统信息都正常）；
鼠标随便点一下（窗口获得焦点）之后才补出来。移动端没有复现。

### 37.2 根因

`home.load()` 的并发去重是「有人正在加载就直接 return」，而**不是等那一份结果**：

1. `HomeView.onMounted` 先调 `home.load()`，`loading = true`，`/api/sessions` 在飞；
2. `App.onMounted` 先 `await loadInfo()`，再调 `home.restore()`；`restore()` 里
   `!loaded.value` 成立 → `await load(...)`，但此时 `loading` 为真，`load` 立即返回；
3. `restore()` 继续遍历**还是空的** `openStates`，于是不建标签、不 `terminals.ensure`；
4. 稍后列表请求返回，卡片画出来了（所以看起来「会话是打开的、CPU 也有」），但已经没有
   任何东西再把 `open` 列表投影成标签。要等 `syncOpenSessions()` 被 `focus` /
   `visibilitychange` / 切回首页触发时才补上——桌面端从后台打开时窗口没有焦点，正是用户
   说的「鼠标随便点一下才显示」；移动端启动即有可见性/焦点事件，所以没暴露。

同一根因的另一条路径：启动那一次加载失败（网络/后端暂不可用），首页点「重试」只调
`home.load(true)`——卡片会出来，标签与子终端列表仍然不会。

### 37.3 修复

- `stores/home.ts` 的 `load()`：并发调用改为**共享同一个 in-flight promise**，后到者
  `await` 它（`force`/`quiet` 的差异不值得再打一次请求）；失败时只要**任一**等待者是非
  `quiet` 的（比如用户点重试）就写 `error`，后台同步仍然保持「失败不覆盖卡片」的语义。
- `HomeView` 新增 `retryLoad()`：`await home.load(true)` 成功后再 `await home.restore(true)`。
  `load` 负责把失败原因显示出来（`restore` 的加载是 quiet 的），`restore` 幂等地补齐标签
  与子终端列表；多一次请求只发生在点重试时。

### 37.4 验证

- Playwright（真实后端，构造「已打开会话」+ `page.route` 把 `GET /api/sessions` 延迟 2.5s）：
  - 修复前：启动后 `header .app-tab` = `["会话"]`，卡片正常；派发 `focus` 后变成
    `["会话", "冒烟测试"]`（复现用户描述）；
  - 修复后：延迟期间启动完成，`["会话", "冒烟测试"]` 已就位，`focus` 后不重复、不变化。
- 失败-重试路径：`route.abort()` 所有 `GET /api/sessions` → 错误块 + 只有「会话」标签；
  放行后点「重试」→ 卡片与会话标签同时出现。
- 无延迟的正常启动无回归（标签与卡片都在）。
- `npm run check` 通过；`make test` 全绿（本轮未改后端）。
- 未验证真实子终端列表：本机沙箱打不开 `/dev/ptmx`（`open /dev/ptmx: permission denied`），
  无法通过 WS 建出终端；标签与 `terminals.ensure` 在 `restore()` 的同一个循环里，共享同一竞态。
