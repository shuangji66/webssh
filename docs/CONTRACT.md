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

- 进程启动顺序（照 terminal 的教训）：准备数据目录 → 检查 socket 是否已被占用（是则退出）
  → **先绑定 socket 成功再进信号等待**（绑定失败即退出，不要「活着但没有 socket」）→ 收到
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
| `protocol.go` | 终端控制协议常量：OSC 控制帧、close 4001、历史/尺寸上限（与前端一对，见第 6 节） |
| `hostkeys.go` | known_hosts 的 TOFU 记录（读写 + 指纹计算 + 匹配判断） |
| `sshclient.go` | SSH 拨号：认证方式组装、主机密钥回调、超时、`ssh.Client` 生命周期 |
| `sessions.go` | 会话配置的管理：字段校验、保存/删除、测试连接、信任主机密钥 |
| `sessions_runtime.go` | `sessionRuntime`（已打开的会话）：终端 + SFTP 的容器、打开/关闭/列表/回收 |
| `terminal.go` | 单个终端：`ssh.Session` + PTY + 历史文件 + 单挂载点 attach/detach/resize |
| `admin.go` | `AdminMux`、JSON 助手、会话/终端/主机密钥的 HTTP handler |
| `admin_routes.go` | 路由表（路径 → handler）与 WebSocket 升级判定 |
| `static.go` | SPA 回退、缓存头三分类、base href 注入、unix socket 绑定、网关身份 |
| `sftp.go` | SFTP 会话生命周期、远程路径校验、SFTP 错误映射 |
| `files.go` | 文件列表与信息（list / info） |
| `files_write.go` | 新建文件 / 新建目录 / 重命名 / 删除 |
| `files_upload.go` | 上传（本地文件、目录、飞牛文件） |
| `files_download.go` | 下载（含目录打包 zip）与文本读取 |
| `fileops.go` | 复制 / 移动 |
| `archive.go` | 压缩 / 解压 |
| `exportnas.go` | 导出至飞牛共享目录 |
| `quickcmds.go` | 全局 + 会话专属快捷指令的持久化与 API |
| `trimapi.go` | `/api/trim/*` 的 HTTP 入口（客户端在 `internal/trimapi`） |
| `credentials.go` | 「不保存凭据」会话的内存凭据 |
| `remote_exec.go` | 远程命令 helper（exec 通道 + `shellQuote` + 超时 + ctx 取消） |
| `*_test.go` | 单元/集成测试 |

### 2.2 `internal/`（可独立测试的子系统）

| 包 | 职责 |
| --- | --- |
| `internal/apierr/` | API 错误码与 JSON 响应：裸码契约、状态码映射、`ok/code/error` 信封 |
| `internal/logx/` | 极简日志：只记异常/失败（不记例行启动停止） |
| `internal/secret/` | `master.key` 管理与 AES-256-GCM 字段加解密（密码 / 私钥 / 口令） |
| `internal/store/` | 数据目录布局、原子 JSON 读写（tmp + rename）、`Session`/`SessionView`/`SessionInput` |
| `internal/trimapi/` | 飞牛开放 API 客户端（`trim.file.checkUserACL`） |
| `internal/ws/` | WebSocket 帧编解码（含单帧/单消息上限） |
| `internal/testssh/` | **仅测试用**：进程内 SSH + SFTP 服务器（密码/密钥认证、PTY、sftp 子系统），供集成测试与本地联调 |

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
| POST | `/api/fs/compress` | `{sid, paths:[], name?}` | `{ok, archive:{path,size,dir}}`（tar.gz / zip，产物在**远程**归档目录） |
| POST | `/api/fs/extract` | `{sid, path}` | `{ok, dir}` |
| POST | `/api/fs/export-to-nas` | `{sid, path, overwrite?}` | `{ok, export:{path,size,files}}` |
| POST | `/api/fs/upload` | multipart：`sid`、`path`(目标目录)、`overwrite`、`files[]` | `{ok, files:[{path,size}], skipped}` |
| POST | `/api/fs/upload-from-nas` | `{sid, path, source, overwrite}` | `{ok, files, skipped}` |
| POST | `/api/fs/download` | `{sid, path}` | 文件流（`Content-Disposition`；目录打 zip） |
| POST | `/api/fs/read` | `{sid, path}` | `{ok, size, text, tooLarge, content, encoding}`（文本预览，见 25.2） |
| POST | `/api/fs/image` | `{sid, path}` | 图片**原始字节**（`Content-Type` = 按内容嗅探出的类型 + `nosniff`）；不是图片 → `invalid`，超过 4 MiB → `too_large`（见 25.2） |
| POST | `/api/quickcmds/list` | `{sid}`（空 = 全局） | `{ok, scope, commands:[QuickCmd]}` |
| POST | `/api/quickcmds/save` | `{sid, commands:[QuickCmd]}` | `{ok, scope, commands}` |
| GET | `/api/trim/status` | — | `{ok, trim:{available, message, app}}` |
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
             // cpuCount = 在线核数（负载的颜色按 load1 ÷ cpuCount 判，见 23.6）
FsEntry      { name, path, dir, size, mode, mtime, symlink?, broken? }  // path 是绝对路径
FsInfo       FsEntry + { uid, gid, owner?, group?, dirCount, fileCount, countError? }
             // 见 15.5 / 17.7：owner/group 是远端 `id` 查出来的名字，查不到就没有（界面显示数字）
QuickCmd     { id, name, content, auto }
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
│   ├── SettingsDialog.vue      [D]    主题/语言/字体/字号/关于（已信任主机第十一轮挪到会话弹窗）
│   ├── QuickCmdsDialog.vue     [D]    三页：常用指令 / 全局 / 专属（会话）
│   ├── CredentialPromptDialog.vue [D] 「不保存凭据」的会话在 open 时弹的凭据输入框
│   ├── QuickCmdEditDialog.vue  [D]    新增/编辑单条
│   ├── home/SessionCard.vue    [D]    大号会话卡片
│   ├── home/SessionFormDialog.vue [D] 新增/编辑 + 测试连接
│   ├── terminal/TerminalPane.vue  [B] 一个终端标签（xterm + WS）
│   ├── terminal/KeypadBar.vue     [B] 移动端辅助键条
│   ├── files/FileManager.vue      [C] 文件管理面板（工具栏 + 面包屑 + 列表/网格 + 菜单）
│   ├── files/FileTree.vue         [C] 左栏文件树
│   ├── files/BreadcrumbBar.vue    [C] 左栏面包屑（只有「刷新」与「隐藏面包屑」两个按钮）
│   ├── files/InfoDialog.vue       [C] 「信息」弹窗（/api/fs/info，见 15.5）
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
  add(sid: string, opts?: { initialCommand?: string }): TermTab   // 见 11.1-5
  openTerminalAt(sid: string, dir: string): TermTab              // 「在此目录新建终端」（cd + shellQuote）
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
                             mode: PermMode; sort: SortState; multi: boolean;
                             treeChildren: Record<string, FsEntry[]>; expanded: string[];
                             treeLoading: Record<string, boolean>; selected: string[] }
// PermMode = 'symbolic'|'numeric'（权限列显示模式，点表头「权限」切换）
// SortState = { key: 'name'|'size'|'mtime'|'mode'; asc: boolean }
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
  // 树 / 运行信息
  expandPath(sid: string, path: string): Promise<void>
  refreshTree(sid: string, path: string): Promise<void>
  runtime(sid: string): RuntimeInfo | null; ensureRuntime(sid: string): Promise<void>
  exportRoot(sid: string): string; archiveDir(sid: string): string
  // 权限列模式 / 排序
  toggleMode(sid: string): void; cycleSort(sid: string, key: string): void
  sorted(sid: string): FsEntry[]
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

// [D] stores/sessionStats.ts（第十三轮，见 23）
export const STATS_INTERVAL_MS = 5000
export const STATS_THRESHOLDS = { cpu:{warn,danger}, load:{...}, mem:{...}, disk:{...} }
export type StatLevel = 'ok' | 'warn' | 'danger'
cpuLevel(pct): StatLevel; loadLevel(load1, cores): StatLevel
memLevel(used, total): StatLevel; diskLevel(pct): StatLevel
statLevelClass(level: StatLevel): string        // 'text-success' | 'text-warning' | 'text-danger'
useSessionStatsStore(): { statsOf(sid: string): SessionStats | undefined
                          sync(sids: string[], visible: boolean): void }

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
| `FileTree.vue` [C] | `{ sid: string; mobile?: boolean }` | — | 左栏文件树；点目录 = 导航（**不切**文件管理子标签，见 11.3）。动作全部直接落 store |
| `BreadcrumbBar.vue` [C] | `{ sid: string }` | — | 左栏面包屑；只有「刷新」「隐藏面包屑」两个按钮（18.1 删掉了「完整文件管理」） |
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
    横向空间，见 `SessionView.vue` 的 `showLeft`）。
- **左栏**（宽屏且终端在前台且没被手动收起时）：`BreadcrumbBar` + `FileTree`。可见性是
  `wide && !filesActive && !leftHiddenOf(sid)` 的交集（13.1/14.3/16.1）；收起时子标签栏左侧出现
  「展开面包屑」。**没有**「完整文件管理」按钮（13.1/18.1 两次删除）。移动端不渲染左栏
  （`SessionView` 直接把文件管理面板当作子页面）。
- **快捷指令**：工具条入口打开 `QuickCmdsDialog`，弹窗内部是**三页**：`常用指令 | 全局 | 专属`
  （11.6）；「专属」只有会话视图里能切（首页不显示专属分组）。点击命令 → 发给当前激活终端
  （`paneControls.get(sid, activeUid)?.send(...)`），`auto` 为真时在内容后补 `\r`。
  未连接时 toast `qc_not_connected`。
- **首页卡片**：只平铺（网格，不提供列表/表格视图），卡片做大（`min-w-[22rem]`）。
  卡片显示：会话名 / `user@host:port` / 备注 / 「不保存凭据」提醒 / **内联的子终端列表**
  （点一行 = 快捷打开）。**不再有**认证方式徽标、打开状态灯、打开时间与最近使用时间
  （15.2/16.2 删的，见 16.2）。操作：打开（或关闭）、编辑、删除。已打开时「关闭」按钮要显眼。
- **终端面板工具栏**（桌面端在子标签栏右侧，移动端第二行）：搜索 / 粘贴 / 清屏 / 重连 /
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
  ⚠️ 无 PTY 的环境（多数 CI 容器：`open /dev/ptmx: permission denied`）会**静默跳过** 11 个
  PTY 用例（`requirePTY` → `t.Skip`），包括终端收发 / 单挂载点顶号 / 历史回放。看到「全绿」
  时要顺带看一眼 skip 数，别把「没跑」当成「跑过了」。
- 前端：`make check`（= `vue-tsc` + `check:i18n`）无错误；`make dev` 能成功产出 `./webssh`。
  **完整门禁是 `make test check`**。
- i18n key 数：**以 `npm run check:i18n` 的实际输出为准（当前 zh/en 各 399）**。下面各轮小节
  括号里的 key 数是**那一轮当时的快照**，有增有删、不保证单调，不要拿它当基准（历史上有
  396/397/398/400/402 几个数字并存）。
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
  状态放在 `sessionUi.mainMode(sid)`：`'terminal' | 'files'`。
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
- 「家目录」按钮：全部折叠，只展开登录用户家目录那一条链路。
  → 左栏宽度的拖动方式见 19.1（第九轮起用手柄，不再直接拖分隔线）。
  → **第八轮删了这个按钮，同样的行为改由 FileTree 在两个时机自动做（新开会话 / 重新展开面包屑），
  见 18.1。**
  → **第六轮改了这条件里的按钮组成（去掉「在此目录新建终端」、新增「隐藏面包屑」），见 16.1。**
- **点目录不再跳转文件管理**：只更新「当前目录」（与文件管理器**共用同一个当前目录**）
  并展开/高亮；主区域保持不动。
- 树的右键/「…」菜单补全（目录可用的全部动作）：
  新建文件夹 / 新建文件 / 上传到此目录 / 在此目录新建终端 / 粘贴 / 重命名 / 删除 /
  压缩为 tar.gz / 导出至飞牛 / 复制完整路径 / 刷新。

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

`make test`：**84 个用例全过 0 失败**（含 PTY 用例）；`go test -race ./...` 干净；`gofmt -l` / `go vet` 干净。

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
  第七轮补「刷新」（17.8）。**
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
- 现在 `HomeView.openWith` 与 `App.vue` 的恢复循环**都不再判 `isMobile`**：一个终端都没有就
  `terminals.add(sid)`，桌面与窄屏一致（这与 11.7 的原口径一致）。

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

### 18.5 本轮的验证

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
