# WebSSH — 飞牛 fnOS 上的 web 终端

**SSH 直连远程主机 + SFTP 文件管理**的 web 终端。后端是内嵌前端的单个 Go 二进制，只监听
一个 **unix socket**，由 nginx / 飞牛统一网关按 baseurl 反代；前端是 Vue 3 + TypeScript +
Tailwind 4 + xterm 6 的单页应用。

- 首页添加/管理**远程会话**：同一个「地址 + 登录名」只算一个会话；密码与私钥**加密落盘**，
  支持 SSH 私钥（含口令）；添加时可**测试连接**（首次连接会让你确认主机密钥指纹）。
- 打开一个会话 = 一个应用级标签；会话内可以开**多个终端子标签**，共用同一套地址与鉴权。
- 桌面端打开会话会**自动开一个终端标签**并展开**左栏**（面包屑 + 文件树）；
  左栏与固定的 `文件管理` 子标签都是完整的 SFTP 文件管理器（列表 / 网格双模式）。
- 文件管理支持：浏览、新建文件/目录、重命名、删除、下载、上传（本地文件 / 本地目录 /
  从飞牛中选择已有的文件）。
- 快捷指令分**全局**与**专属**（专属指令随会话保存在它自己的数据目录里）。
- 关掉浏览器/刷新页面**不会**杀掉会话：重新打开页面会把仍开着的会话、终端子标签与历史一起恢复。
- 每个终端都支持**重连**与**多设备接管**（同一终端同时只有一个操作端，被顶掉的设备会收到提示）。

---

## 1. 技术栈

| 层 | 选型 |
| --- | --- |
| 后端 | Go 1.27，`golang.org/x/crypto/ssh`（SSH 客户端）、`github.com/pkg/sftp`（SFTP），标准库 `net/http` + 自实现 WebSocket 帧编解码（无第三方 web 框架） |
| 前端 | Vue 3.5 + Pinia 4 + TypeScript 6 + Tailwind CSS 4 + Vite 8 + xterm.js 6 + `@trimjs/web-app`（飞牛宿主 SDK） |
| 交付 | 前端 `dist` 经 `//go:embed` 打进 Go 二进制 → 项目根目录的 `./webssh`；`fpk/` 是飞牛安装包脚手架 |
| 测试 | 后端集成测试用 `internal/testssh`（进程内 SSH + SFTP 服务器，`github.com/creack/pty` **仅测试用**），不依赖外部 sshd |

> 参考实现：本项目复用/借鉴了 `terminal`（本地 web 终端：会话管理、单挂载点接管、移动端适配、
> 构建方式）与 `fpkstudio`（标签页 + 面包屑 + 文件管理双模式、飞牛开放 API 调用）两个项目的成熟做法。

## 2. 快速开始（本地跑起来）

```bash
# 1) 构建（前端 + 后端一次搞定，产物是 ./webssh）
make                     # dev 构建；make release V=1.0.1 是裁剪符号的发布构建

# 2) 起一个本地 SSH/SFTP 测试服务（仅开发用，见 backend/internal/testssh）
cd backend
go run ./internal/testssh/cmd/testsshd -addr 127.0.0.1:2222 -user demo -password demo -root /tmp/webssh-sftp

# 3) 跑起来（唯一入口是 unix socket，所以要自己搭一层 TCP→socket 的桥来用浏览器访问）
export WEBSSH_ADMIN_SOCK=/tmp/webssh/app.sock
export WEBSSH_ADMIN_BASEURL=/app/WebSSH        # 想直接访问根路径就留空
export WEBSSH_DATA_DIR=/tmp/webssh/data
./webssh &

# 4) 浏览器访问 http://127.0.0.1:18080/app/WebSSH
socat TCP-LISTEN:18080,fork UNIX-CONNECT:/tmp/webssh/app.sock
```

然后在首页「添加远程会话」里填 `127.0.0.1 : 2222 / demo / demo`，点「测试连接」→ 确认指纹 →
保存 → 打开。

## 3. 构建与测试

```bash
make                        # 前端构建 + 内嵌 + go build → ./webssh
make release V=1.0.1        # 发布构建（-s -w，版本号经 -ldflags 注入）
make test                   # 后端单元 + 集成测试（用进程内 SSH/SFTP 服务器，不依赖外部 sshd）
make typecheck              # 前端 vue-tsc 类型检查
make fpk                    # 额外把二进制放进 fpk/app/bin/webssh（打包用）
make clean
```

构建约定：

- 前端必须以**相对 base**（`base: './'`）构建；baseurl 在运行期由后端注入 `<base href>`，
  **不要**把它烧进产物（同一份二进制要能挂在任意前缀下）。
- Go 缓存 / 模块缓存默认放项目本地（`.gocache/`、`.gopath/`、`.gomodcache/`、`.tmp/`）：
  飞牛存储卷上 `/tmp` 与文件模式位都不可靠，构建脚本已经处理（含 `chmod 755` 产物）。
- 每次前端改动后必须重新 `make`；只改源码不重新构建，二进制里还是旧资源。
- `make test` 首次运行需要能访问 Go 模块代理（要拉测试用的 `creack/pty`）；之后走项目本地
  `.gomodcache/`，离线可跑。

## 4. 部署（飞牛 fnOS）

`fpk/` 目录就是应用包骨架（来自 fpkstudio 的 WebSSH 脚手架）：

```
fpk/
├── manifest              应用元信息（appname=WebSSH，display_name=Web终端）
├── building/             打包用（building/manifest、settings.json）
├── cmd/main              生命周期脚本：start/stop/status，负责导出环境变量、启停二进制
├── cmd/*_callback        安装/升级/配置/卸载回调
├── config/privilege      run-as: root
├── config/resource       api-scope: trim.file.userAccess / trim.file.userAcl（选飞牛文件用）
├── app/ui/config         桌面入口：gatewayPrefix=/app/WebSSH，gatewaySocket=app.sock
├── app/ui/images/*       图标
└── wizard/uninstall      卸载向导（选择是否保留数据）
```

- `cmd/main` 会导出 `WEBSSH_ADMIN_SOCK=${TRIM_APPDEST}/app.sock`、
  `WEBSSH_ADMIN_BASEURL=/app/WebSSH`、`WEBSSH_DATA_DIR=${TRIM_PKGVAR}/data`，
  然后启动 `${TRIM_APPDEST}/bin/webssh`。打包时用 `make fpk` 把二进制放进 `fpk/app/bin/webssh`。
- **权限**：`config/privilege` 里是 `run-as: root`。本应用不修改本机文件系统（它只连远程主机），
  之所以仍要 root，是为了读「登录用户用文件选择器授权给本应用」的飞牛文件
  （上传到远程主机时用）——授权给了应用，读取本身仍需具备权限。
- **反代**（不用飞牛统一网关时，例如 nginx）：

  ```nginx
  location /app/WebSSH/ {
      proxy_pass http://unix:/vol1/@appdata/WebSSH/app.sock:/;   # 路径前缀去掉后转发
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;      # 终端是 WebSocket，必须转发 Upgrade
      proxy_set_header Connection "upgrade";
      proxy_read_timeout 3600s;                    # 长时间静默的终端会话别被掐断
  }
  ```

  终端连的是 `/app/WebSSH/api/terminal/ws`（WebSocket）。反代必须支持 `Upgrade`/`Connection`，
  否则只会看到「连接已关闭」。

## 5. 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `WEBSSH_ADMIN_SOCK` | `$TMPDIR/webssh/app.sock` | 唯一的 HTTP/WS 入口（unix socket） |
| `WEBSSH_ADMIN_BASEURL` | 空（根路径） | 前端资源与 API 的路径前缀 |
| `WEBSSH_DATA_DIR` | `$TMPDIR/webssh/data` | 数据目录（会话配置、历史、快捷指令、密钥） |
| `WEBSSH_DIAL_TIMEOUT` | `15s` | SSH 建连超时 |
| `WEBSSH_MASTER_KEY` | 空 | 外部提供的主密钥（32 字节 base64/hex），避免密钥与密文同放 |
| `WEBSSH_TRIM_SOCKET` | `/var/run/trim_open_gateway_apiscope.socket` | 飞牛开放 API 网关 socket |
| `WEBSSH_TRIM_APP` | `WebSSH` | 调开放 API 时的 appName |

## 6. 数据目录

```
$WEBSSH_DATA_DIR/
├── master.key                0600，凭据加密主密钥（首次使用自动生成；可用 WEBSSH_MASTER_KEY 代替）
├── known_hosts.json          已信任的主机密钥（首次连接确认指纹后才写入）
├── sessions/<sid>.json       远程会话配置（密码/私钥字段是密文）
├── history/<sid>/<tid>.log   终端历史（会话存活期间；关闭会话时删除）
└── quickcmds/{global,<sid>}.json   全局 / 专属快捷指令
```

- **凭据加密**：`password` / `privateKey` / `passphrase` 用 AES-256-GCM 加密后落盘，
  AAD 绑定字段名与会话 id（密文被搬到别处也解不开）。
  **威胁模型说明**：这层加密防的是「拿到磁盘快照 / 备份 / 同步副本的人直接读到明文密码」；
  它**不**防「已经能以同样权限读取数据目录的攻击者」——`master.key` 就在旁边。
  需要更强的隔离就用 `WEBSSH_MASTER_KEY` 把主密钥放到进程环境里（例如只注入给本应用的
  systemd unit），而不是和数据一起躺在同一块盘上。
- 会话身份 = `登录名@地址:端口`，**同一身份只允许一个会话**；重复添加会被拒绝（提示已存在），
  避免了「同一个目标在首页出现两张卡片、各自开一堆终端」。
- 主机密钥按 TOFU（首次使用即信任）处理：未知主机 → 报错并给出指纹，用户确认后写入
  `known_hosts.json`；**指纹不匹配时只报错、绝不覆盖**（可能是中间人）。

## 7. 使用说明

**首页**：远程会话卡片（大卡片平铺）显示地址、登录名、认证方式、打开状态、开了几个终端子标签、
打开时间与最近使用时间；可打开 / 关闭 / 编辑 / 删除，右上角可搜索。

**会话标签**：打开会话后进入会话视图。

- 子标签栏：`文件管理`（固定，不参与横向滚动）+ 若干终端子标签 + `+`（新建终端）。
  终端标签可双击重命名、可关闭（关闭会终止该终端在远程主机上的 shell）。
- 左栏（桌面端）：面包屑（点任意一段跳转）+ 文件树（懒加载）。点目录 = 导航并切到文件管理；
  「完整文件管理」按钮切到固定的文件管理标签。左栏可拖拽调宽、可折叠。
- 工具条：搜索（终端内查找）、粘贴、清屏、重连、快捷指令、设置；功能名可折叠。

**终端**：桌面端鼠标框选自动复制、`Ctrl/Cmd+V` 走浏览器粘贴；移动端长按选词自动复制、
单指拖动滚动、底部辅助键条提供 `ESC/Tab/Ctrl/Alt/Shift/方向键/常用符号`（Shift 是上档锁定）。
字号与字体（内置 Maple Mono / 系统字体）在设置里切换。

**文件管理**：

- 列表 / 网格双模式、后退/前进、刷新、上一层；行内 `···` 菜单：重命名、删除、下载、复制路径。
- **上传**（弹窗，三种来源）：
  1. **本地文件**：从当前设备选文件；
  2. **本地目录**：选整个目录，按原有相对路径上传（浏览器递归读取）；
  3. **飞牛文件**：从飞牛的文件选择器里选**已存在的文件**，由服务端读取后经 SFTP 推送到远程
     主机（暂不支持选目录）。
- 下载走「先取回再落地」的方式，失败会把原因（权限、路径不存在、网关拦截）明确报出来。

**快捷指令**：全局指令对所有会话可用；专属指令属于某一个会话，存在它的数据目录里。
点击即发送到当前激活的终端（`Auto` 会在末尾补一个回车直接执行）；当前在文件管理标签上时，
指令会发到本会话最近使用的那个终端。

> 已知的未实现项（都是可选项）：文件管理器里没有「在此目录打开终端」入口；
> 飞牛文件选择器只支持选**文件**、不支持选目录（按需求暂不做）。

## 8. 安全

- 单一入口是 unix socket：**不监听任何 TCP 端口**。访问控制交给 socket 文件权限与前置反代。
- 反向代理部署时请自行加认证（飞牛统一网关会做登录态校验）。
- 主机密钥 TOFU + 指纹展示；密码/私钥加密落盘；会话配置与历史只写本机。
- 连出去的每一条 SSH 都是**从本机发起**的，服务端不会替你执行本机命令（本应用不是本地终端）。

## 9. 开发

```
webssh/
├── Makefile              构建入口（产物 ./webssh）
├── docs/CONTRACT.md      前后端接口 / 数据模型 / 分工边界（改接口先改它）
├── fpk/                  飞牛应用包脚手架
├── backend/              Go 源码（package main）
│   ├── main.go config.go logging.go      入口 / 配置 / 日志
│   ├── store.go crypto.go hostkeys.go    数据目录 / 凭据加密 / 主机密钥
│   ├── sshclient.go sessions.go terminal.go ssh 拨号 / 会话 / 终端（PTY+历史+单挂载点）
│   ├── ws.go admin.go                    WebSocket 帧编解码 / 路由 + SPA + 缓存头
│   ├── files.go quickcmds.go trimapi.go  SFTP 文件 API / 快捷指令 / 飞牛开放 API
│   └── internal/testssh/                 进程内 SSH+SFTP 测试服务器（仅测试/联调）
└── frontend/
    ├── src/serverapi/                     API 客户端（契约的代码化）
    ├── src/i18n/                          中英文案（唯一来源）
    ├── src/stores/                        Pinia：appTabs/home/terminals/sftp/quickCmds/settings/toast/sessionUi
    ├── src/components/                    AppTabBar / home / terminal / files / 各类弹窗
    └── src/views/                         HomeView / SessionView
```

- **先读 `docs/CONTRACT.md`**：接口、数据结构、store 与组件签名、交互约定都在里面。
- `AGENTS.md` 记录了给 AI/开发者的硬约束与易踩的坑（构建、冻结文件、协议控制帧、缓存头、风格）。
- 提交前请跑 `make test` 与 `make typecheck`。

## 10. 常见问题

| 现象 | 原因 / 处理 |
| --- | --- |
| 页面空白、控制台报 404 | 反代的 location 前缀必须与 `WEBSSH_ADMIN_BASEURL` 一致；本应用不监听 TCP，直接访问 `127.0.0.1:端口` 是连不上的 |
| 终端连不上（一直 connecting） | 反代没转发 `Upgrade`/`Connection` 头；或 SSH 目标不可达（用「测试连接」看具体原因） |
| 提示「主机密钥不匹配」 | 目标主机重装过，或真的有人在中间。确认无误后到 设置 → 已信任的主机密钥 里移除旧记录，再重新测试连接 |
| 文件管理报「SFTP 子系统不可用」 | 目标 sshd 的 `Subsystem sftp` 被注释或 `sftp-server` 未安装；终端仍可正常使用 |
| 上传飞牛文件时提示不在宿主里 | 该功能依赖 `@trimjs/web-app` 的宿主桥（只能在飞牛的应用窗口里用），独立浏览器打开时用「本地文件」上传 |
| 移动端键盘盖住终端 | 本项目用 `visualViewport` 跟随键盘（Safari 不支持 `interactive-widget=resizes-content`）；若仍异常请附浏览器版本反馈 |
| 关闭浏览器后会话没了 | 只有**关闭会话标签**或后端进程退出才会终止会话；刷新/关页面不会。若后端重启过，前端会提示并清空标签（配置仍在，重新打开即可） |
| 终端连上但打字没反应 | 先点一下终端区域。若刚从工具条点了「清屏/重连」，焦点会自动交回终端；真连不上看标签状态点与提示行 |
| 数据目录里 `history/` 是空的 | 正常：终端历史属于「活着的会话」，会话被关闭（或进程退出）时整目录删除；重启进程时也会清掉上次遗留的历史 |

## 11. 许可

MIT。
