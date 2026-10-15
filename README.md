# WebSSH — 飞牛 fnOS 上的 web 终端

**SSH 直连远程主机 + SFTP 文件管理**的 web 终端。后端是内嵌前端的单个 Go 二进制，只监听
一个 **unix socket**，由 nginx / 飞牛统一网关按 baseurl 反代；前端是 Vue 3 + TypeScript +
Tailwind 4 + xterm 6 的单页应用。

- 首页（应用标签叫「会话」）添加/管理**远程会话** —— 顶部工具条左侧是「添加会话」，
  右侧是「设置」（两个都是描边按钮；主题、语言、终端字体字号、关于都在设置里；
  飞牛宿主只在不可用时才提示一句）；
  同一个「地址 + 登录名」只算一个会话；
  密码、私钥与**快捷指令的命令内容**都是**加密落盘**的，也可以在表单里关掉「保存凭据」
  ——那样凭据只留在内存里，
  每次连接都要重新输入（编辑里还有**测试连接**，首次连接会让你确认主机密钥指纹；
  指纹没确认过之前按钮显示**「仅保存」**——建议先测试连接并信任指纹，着急也可以先存下来）。
  卡片右上角是三个仅图标按钮（打开/关闭、编辑、删除），与会话名同一行（名字放不下就横向滚动）；
  卡片上不显示认证方式，也没有状态灯与各种时间。**已打开的子终端直接列在卡片下**：点一行就是
  「快捷打开」那一个终端，行尾的 × 单独关掉它（会先确认，因为那会终止远端的 shell）。
- 打开一个会话 = 一个应用级标签；会话内可以开**多个终端子标签**，共用同一套地址与鉴权。
- 子标签栏最左是固定的 `文件管理`，后面是终端标签与 `+`。桌面端**左栏（不显示路径的按钮条 +
  以 `/` 为根的完整目录树，首次进入自动展开到登录用户家目录）与 `文件管理` 子标签是二选一**：
  看终端时显示左栏，切到 `文件管理` 时左栏收起（文件管理器自带面包屑）。
  切回终端用文件管理器工具栏里的「返回终端」（回到你刚才用的那个终端）；左栏能用按钮条里的
  「隐藏面包屑」整条收起，收起后子标签栏左侧会出现「展开面包屑」（点一下左栏回来、按钮自己消失，
  并把目录树重新展开到登录用户的家目录——新打开会话时也会自动展开到那里）；
  左栏宽度用**骑在分隔线上的手柄**拖（平板没有鼠标，1px 的分隔线抓不住）；文件管理在前台时终端工具条（搜索 / 粘贴 /
  清屏 / 重连 / 快捷指令）整条隐藏——它们的作用对象是当前终端，此时没有。
  窄屏（<768px）没有左栏，`文件管理` 就是占据全区域的子页面；宽屏（含平板）按子标签切——
  看终端显左栏、看文件管理收左栏；打开或恢复会话都默认落在终端。
- 文件管理支持：浏览（名称/大小/修改时间**三态排序**，点表头「权限」在**符号 ↔ 八进制**之间切换）、
  空白处右键菜单（新建文件 / 新建文件夹 / 上传 / 信息 / 打开终端 / 复制路径 / 刷新；
  剪贴板里有内容时才会多出「粘贴」）、
  **信息**弹窗（类型 / 名称 / 路径 / 大小（不含子目录内容）/ 修改时间 / 权限（符号 + 八进制）/
  拥有者（账号名 + uid:gid）/ 内含多少目录和文件，只数一层）、
  新建文件/目录、重命名、删除、**复制/剪切/粘贴**（不跨会话；复制/剪切后地址栏下面会常驻一行
  剪贴板，带「粘贴到此处」「取消」两个按钮——这一行的粘贴不清空剪贴板，可以连着粘到好几个目录；
  右键菜单里的「粘贴」仍然粘完就清）、
  **多选**（工具栏「多选」进入选择模式，点行逐个点选，做完复制/剪切/删除/压缩/导出自动退出；
  点工具栏上别的按钮（后退/前进/刷新/上一层/新建/上传/换视图）同样退出多选；
  多选时右键（行上或空白处都一样）直接给这一批的复制/剪切/删除/导出/下载/两个压缩）、
  下载、**导出至飞牛**（落到 `${TRIM_DATA_SHARE_PATHS%%:*}`，即 `runtime.exportRoot`）、
  **压缩为 tar.gz / 压缩为 zip / 解压**（调用远程主机的 `tar` / `zip` / `unzip`，
  压缩包输出到**远程主机**的 `/tmp/webssh-archive`——那个目录不可写就退到同级的
  `/tmp/webssh-archive-<登录用户>`，还不行再退到登录用户家目录的 `~/.webssh-archive`，
  三处都不可写才报错（服务端可用 `WEBSSH_ARCHIVE_DIR` 指定别的目录））、
  上传（本地文件 / 本地目录 / 从飞牛中选择已有的文件）、
  以及「在此目录新建终端」（新终端自动 `cd` 到该目录）。权限不足或任何一步失败都会明确报错，
  不会静默跳过。
- 快捷指令分**全局**与**专属**（专属指令随会话保存在它自己的数据目录里）。
- 关掉浏览器/刷新页面**不会**杀掉会话：重新打开页面会把仍开着的会话、终端子标签与历史一起恢复。
- 每个终端都支持**重连**与**多设备接管**（同一终端同时只有一个操作端，被顶掉的设备会收到提示）。

---

## 1. 技术栈

| 层 | 选型 |
| --- | --- |
| 后端 | Go 1.27，`golang.org/x/crypto/ssh`（SSH 客户端）、`github.com/pkg/sftp`（SFTP），标准库 `net/http` + 自实现 WebSocket 帧编解码（无第三方 web 框架） |
| 前端 | Vue 3.5 + Pinia 4 + TypeScript 6 + Tailwind CSS 4 + Vite 8 + xterm.js 6 + `@trimjs/web-app`（飞牛宿主 SDK） |
| 交付 | 前端 `dist` 经 `//go:embed` 打进 Go 二进制 → 项目根目录的 `./webssh`；飞牛安装包（fpk）的打包工程在仓库外（见第 4 节） |
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

然后在首页「添加会话」里填 `127.0.0.1 : 2222 / demo / demo`，点「测试连接」→ 确认指纹 →
保存 → 打开。

## 3. 构建与测试

```bash
make                        # 前端构建 + 内嵌 + go build → ./webssh
make release V=1.0.1        # 发布构建（-s -w，版本号经 -ldflags 注入）
make test                   # 后端单元 + 集成测试（用进程内 SSH/SFTP 服务器，不依赖外部 sshd）
make typecheck              # 前端 vue-tsc 类型检查
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

飞牛应用包（fpk）的脚手架**不在本仓库**，由 fpkstudio 工程维护：
`/vol1/@appshare/fpkstudio/projects/WebSSH`。本仓库只产出 `./webssh`，打包时把二进制放进
工程的 `app/bin/`，再用 fpkstudio 打包。工程目录结构：

```
<fpkstudio 工程>/
├── manifest              应用元信息（appname=WebSSH，display_name=Web终端）
├── building/             打包用（building/manifest、settings.json）
├── cmd/main              生命周期脚本：start/stop/status，负责导出环境变量、启停二进制
├── cmd/*_callback        安装/升级/配置/卸载回调
├── config/privilege      run-as: root
├── config/resource       api-scope: trim.file.userAccess / trim.file.userAcl / trim.file.path
│                         data-share: WebSSH（`TRIM_DATA_SHARE_PATHS` 的来源，「导出至飞牛」用）
├── app/ui/config         桌面入口：gatewayPrefix=/app/WebSSH，gatewaySocket=app.sock
├── app/ui/images/*       图标
├── app/bin/webssh        本仓库 `make` 产出的二进制放这里
└── wizard/uninstall      卸载向导（选择是否保留数据）
```

- `cmd/main` 会导出 `WEBSSH_ADMIN_SOCK=${TRIM_APPDEST}/app.sock`、
  `WEBSSH_ADMIN_BASEURL=/app/WebSSH`、`WEBSSH_DATA_DIR=${TRIM_PKGVAR}/data`，
  然后启动 `${TRIM_APPDEST}/bin/webssh`。
- **权限**：`config/privilege` 里是 `run-as: root`。本应用不修改本机文件系统（它只连远程主机），
  之所以仍要 root，是为了读「登录用户用文件选择器授权给本应用」的飞牛文件
  （上传到远程主机时用）——授权给了应用，读取本身仍需具备权限。
- **manifest 里两个字段决定「飞牛文件」这条路能不能走通**（飞牛开放 API 文档：
  「如果应用需要调用 JS SDK，必须在 manifest 中声明 `micro_app=true`」；
  「`disable_authorization_path` 仅当应用**不需要**用户选择文件或目录访问权限时才用 `true`」）：
  - `micro_app = true`：应用页面按微应用加载，`@trimjs/web-app` 的宿主桥才可用；
  - `disable_authorization_path = false`：本应用要用文件选择器拿用户授权，必须是 `false`。
    写成 `true` 时，应用中心在**安装**（`PackageInfo`）与**每次启动**（`App` 实体）时都会把
    本应用的 `trim.file.userAccess` / `trim.file.sharedAccess` 能力状态强制置为 `-1`，
    于是选择器点「确定」时授权接口回 `31003 ErrCodeAppAuthException`（界面文案「应用鉴权异常」），
    表现为「选好了文件，一确定就失败」。见第 10 节。
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
| `WEBSSH_ARCHIVE_DIR` | `/tmp/webssh-archive` | 远程压缩包的固定输出目录（`/api/info` 的 `archiveDir`） |
| `TRIM_DATA_SHARE_PATHS` | 飞牛注入 | 取**第一个冒号之前**的部分作为「导出至飞牛」的目标目录（`/api/info` 的 `exportRoot`）；未设置时该功能禁用 |

## 6. 数据目录

```
$WEBSSH_DATA_DIR/
├── master.key                0600，凭据加密主密钥（首次使用自动生成；可用 WEBSSH_MASTER_KEY 代替）
├── known_hosts.json          已信任的主机密钥（首次连接确认指纹后才写入）
├── sessions/<sid>.json       远程会话配置（密码/私钥字段是密文）
├── history/<sid>/<tid>.log   终端历史（会话存活期间；关闭会话时删除）
└── quickcmds/{global,<sid>}.json   全局 / 专属快捷指令（命令内容是密文，名称明文）
```

- **凭据加密**：`password` / `privateKey` / `passphrase` 用 AES-256-GCM 加密后落盘，
  AAD 绑定字段名与会话 id（密文被搬到别处也解不开）。
  **威胁模型说明**：这层加密防的是「拿到磁盘快照 / 备份 / 同步副本的人直接读到明文密码」；
  它**不**防「已经能以同样权限读取数据目录的攻击者」——`master.key` 就在旁边。
  需要更强的隔离就用 `WEBSSH_MASTER_KEY` 把主密钥放到进程环境里（例如只注入给本应用的
  systemd unit），而不是和数据一起躺在同一块盘上。
- **快捷指令的命令内容**（全局与专属）用同一套主密钥加密后落盘，AAD 还绑定了**指令 id** 与
  表的作用域（全局 / 某个会话）：密文在同一张表里互换、或把专属表改名成 `global.json` 都解不开。
  `id` / `名称` / `是否自动回车` 仍是明文（名称是给人看的标签，加密它只会让文件没法排查）；
  API 收发的始终是明文内容，命令内容与格式逐字节不变。
  旧版本（第一轮）留下的**明文**文件会被自动迁移：读到时就地加密重写，后端启动时还会扫一遍
  `quickcmds/`（没被打开过的会话的表也要迁移掉）。**迁移失败或内容为空的条目**会被清空成
  加密后的占位文案「命令加密失败已清空，请重新添加」（并把「自动回车」关掉，免得把这句提示
  当命令跑出去）——磁盘上不会再有明文的命令内容。
- 会话身份 = `登录名@地址:端口`，**同一身份只允许一个会话**；重复添加会被拒绝（提示已存在），
  避免了「同一个目标在首页出现两张卡片、各自开一堆终端」。
- 主机密钥按 TOFU（首次使用即信任）处理：未知主机 → 报错并给出指纹，用户确认后写入
  `known_hosts.json`；**指纹不匹配时只报错、绝不覆盖**（可能是中间人）。

## 7. 使用说明

**首页**：远程会话卡片（大卡片平铺）显示地址、登录名、开了几个终端子标签；可打开 / 关闭 / 编辑 /
删除。卡片右上角是三个仅图标按钮；卡片上**不显示**认证方式、状态灯与各种时间（也没有搜索框）。
**会话已连接时，卡片上还会显示远端主机的基本信息**（子终端列表之上，两行纯数字、5 秒刷新一次）：
第一行 CPU 占用率与平均负载，第二行内存占用与磁盘占用；关闭会话即停止并隐藏。
数值按阈值染成绿 / 黄 / 红（CPU 60%/85%、负载按核数折算 0.7/1.0、内存 70%/90%、磁盘 75%/90%，
鼠标悬停有完整说明）；数值来自远端主机自己的 `/proc` 与 `df`，远端读不出来（例如非 Linux）
时这一块不显示。

**会话标签**：打开会话后进入会话视图。

- 子标签栏：`文件管理`（固定，不参与横向滚动）+ 若干终端子标签 + `+`（新建终端）。
  终端标签可双击重命名、可关闭（关闭会终止该终端在远程主机上的 shell）；**只剩一个终端标签时
  不显示关闭按钮**（避免误触后这个会话一个终端都不剩），要关它可从首页卡片那一行关。
- 左栏（桌面端，看终端时显示）：面包屑（点任意一段跳转）+ 文件树（懒加载，目录与每个目录前 20
  个文件）。点目录 = 只更新当前目录并展开（**不切**主区域，文件管理是常驻子标签）；点文件 = 预览。
  宽度用骑在分隔线上的手柄拖，可折叠（没有「完整文件管理」按钮，那个子标签本来就常驻）。
- 工具条：搜索（终端内查找）、粘贴、清屏、重连、快捷指令；功能名可折叠。设置按钮在首页工具条上。

**终端**：桌面端鼠标框选自动复制、`Ctrl/Cmd+V` 走浏览器粘贴；移动端长按选词自动复制、
单指拖动滚动、底部辅助键条提供 `ESC/Tab/Ctrl/Alt/Shift/方向键/常用符号`（Shift 是上档锁定）。
字号与字体（内置 Maple Mono / 系统字体）在设置里切换。

**文件管理**：

- 列表 / 网格双模式、后退/前进/刷新/上一层；行内 `···` 菜单：预览、编辑（文本文件）、重命名、复制、
  剪切、删除、下载、导出至飞牛、压缩为 tar.gz / zip、解压（归档文件）、信息、复制完整路径。
- **上传**（弹窗，三种来源）：
  1. **本地文件**：从当前设备选文件；
  2. **本地目录**：选整个目录，按原有相对路径上传（浏览器递归读取）；
  3. **飞牛文件**：从飞牛的文件选择器里选**已存在的文件**，由服务端读取后经 SFTP 推送到远程
     主机（暂不支持选目录）。
- 下载走「先取回再落地」的方式，失败会把原因（权限、路径不存在、网关拦截）明确报出来。
- **预览 / 编辑**（点文件行 = 预览；图片按内容判类型、上限 4 MiB，文本认 UTF-8 与 UTF-16）：
  预览标题栏的铅笔进编辑页，编辑页**覆盖**在文件管理页面上（退出后目录、视图、滚动都不变）。
  编辑区自动换行、左侧常驻行号（超过 20000 行不画行号）、不出现横向滚动条；
  顶栏「查找」展开两行（查找内容 + 查找 + 数量 + 取消 / 替换为 + 替换 + 全部替换），
  「查找」按顺序循环命中、数量显示 `当前/总数`（不分大小写），可单条替换或一次全部替换。
  保存**按原样写回**（UTF-16 仍 UTF-16、带 BOM 仍带 BOM、CRLF 仍 CRLF）；没有写权限的文件
  会被后端拒绝、不进入编辑页。

**快捷指令**（入口在会话子标签栏的工具条上，作用于**当前激活的终端**）：
弹窗分三页——**常用指令**（内置的服务器管理命令，按用途分组，
只读命令点一下就自动回车执行，会改状态的命令不会自动执行）/ **全局** / **专属**。
全局指令对所有会话可用；专属指令属于某一个会话，存在它的数据目录里。
指令的**内容加密落盘**（名称明文，见第 6 节）。
点击即发送到当前激活的终端（`Auto` 会在末尾补一个回车直接执行）；当前在文件管理标签上时，
指令会发到本会话最近使用的那个终端。

> 已知的未实现项：飞牛文件选择器只支持选**文件**、不支持选目录（按需求暂不做）；
> 压缩包同名按 `tar` 语义覆盖（默认名带时间戳，实际撞不上）；导出至飞牛遇到软链接会跳过
> 并记日志（链接指向远端路径，落到 NAS 上是死链）；未做远端磁盘空间预检。

## 8. 安全

- 单一入口是 unix socket：**不监听任何 TCP 端口**。访问控制交给 socket 文件权限与前置反代。
- 反向代理部署时请自行加认证（飞牛统一网关会做登录态校验）。
- ⚠️ **`X-Trim-Userid` / `X-Trim-Username` / `X-Trim-Isadmin` 是「信任头」，必须由反代覆写**：
  本服务直接采信它们（那是飞牛网关的身份透传约定），任何能直接连到 socket 的客户端都能伪造。
  不要让这些头从外部原样透传进来（反代上应显式 `proxy_set_header` 覆写）。
- 终端 WebSocket 会校验 `Origin` 里的主机名与本次请求的 `Host` 一致（挡跨站 WebSocket 劫持）；
  端口只在 `Host` 这一侧带了端口时才参与比较（飞牛统一网关的 `proxy_set_header Host $host;`
  会把端口丢掉，属于支持的部署）。反代把 `Host` 改写成**别的名字**时请让它带上
  `X-Forwarded-Host`，否则浏览器侧的连接会被拒。
- 「上传飞牛文件」是以**本会话的登录用户**身份读本机路径（飞牛文件选择器里已授权的文件）：
  读之前会尽量用飞牛的 ACL 接口判一次权限，**飞牛不可用时跳过并记日志**——所以进程自身的
  OS 权限是唯一硬边界，**建议以非 root 运行本服务**。
- 主机密钥 TOFU + 指纹展示；密码/私钥加密落盘；会话配置与历史只写本机。
- 连出去的每一条 SSH 都是**从本机发起**的，服务端不会替你执行本机命令（本应用不是本地终端）。

## 9. 开发

```
webssh/
├── Makefile              构建入口（产物 ./webssh）
├── docs/CONTRACT.md      前后端接口 / 数据模型 / 分工边界（改接口先改它）
├── backend/              Go 源码（package main）
│   ├── main.go config.go protocol.go        入口 / 运行环境 / 终端控制协议常量
│   ├── admin.go admin_routes.go static.go   mux + handler / 路由表 / SPA + 缓存头 + socket
│   ├── sessions.go sessions_runtime.go      会话配置管理 / 已打开的会话（终端 + SFTP 容器）
│   ├── terminal.go sftp.go files*.go        终端（PTY+历史+单挂载点）/ SFTP / 文件操作
│   ├── fileops.go archive.go exportnas.go   复制移动 / 压缩解压 / 导出至飞牛
│   ├── sshclient.go hostkeys.go             SSH 拨号 / 主机密钥 TOFU
│   ├── credentials.go remote_exec.go        内存凭据 / 远程命令 helper
│   ├── stats.go                             远端系统信息（CPU/负载/内存/磁盘，会话卡片用）
│   ├── quickcmds.go trimapi.go              快捷指令 / 飞牛 API 的 HTTP 入口
│   └── internal/                            子包：apierr / logx / secret / store / trimapi / ws / testssh
└── frontend/
    ├── src/serverapi/                     API 客户端（契约的代码化）
    ├── src/i18n/                          中英文案（唯一来源）
    ├── src/stores/                        Pinia：appTabs/home/terminals/sftp/quickCmds/settings/toast/sessionUi/sessionStats
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
| 提示「主机密钥不匹配」 | 目标主机重装过，或真的有人在中间。确认无误后到该会话的「编辑」弹窗里点「移除信任」（标题「已信任的主机密钥」右边那个按钮），再重新测试连接 |
| 文件管理报「SFTP 子系统不可用」 | 目标 sshd 的 `Subsystem sftp` 被注释或 `sftp-server` 未安装；终端仍可正常使用 |
| 上传飞牛文件时提示不在宿主里 | 该功能依赖 `@trimjs/web-app` 的宿主桥（只能在飞牛的应用窗口里用），独立浏览器打开时用「本地文件」上传 |
| 「飞牛文件」选好了文件、点确定却报「应用鉴权异常」 | 授权接口（`/app-center/openapi/v1/user/authorization-paths`）被应用中心拒了：**打包工程**的 manifest 里 `disable_authorization_path` 必须是 `false`（见第 4 节）。改成 `false` 后重新打包、重新安装（版本号要升，否则装不上），能力状态才会重建 |
| 提示「该会话没有保存凭据，请先输入凭据」 | 建会话时关掉了「保存凭据」：凭据只在后端内存里，会话关闭或后端重启后就要重新输入。想省事就在编辑里打开开关（会加密落盘） |
| 导出至飞牛是灰的 / 报未配置共享目录 | 进程环境里没有 `TRIM_DATA_SHARE_PATHS`（正常部署由飞牛注入）。该功能的目标目录写死为这个变量的第一个路径 |
| 找不到「设置」 | 设置按钮在**首页**工具条右侧（会话页里不再放它，避免与全局设置重复） |
| 移动端键盘盖住终端 | 本项目用 `visualViewport` 跟随键盘（Safari 不支持 `interactive-widget=resizes-content`）；若仍异常请附浏览器版本反馈 |
| 关闭浏览器后会话没了 | 只有**关闭会话标签**或后端进程退出才会终止会话；刷新/关页面不会。若后端重启过，前端会提示并清空标签（配置仍在，重新打开即可） |
| 终端连上但打字没反应 | 先点一下终端区域。若刚从工具条点了「清屏/重连」，焦点会自动交回终端；真连不上看标签状态点与提示行 |
| 数据目录里 `history/` 是空的 | 正常：终端历史属于「活着的会话」，会话被关闭（或进程退出）时整目录删除；重启进程时也会清掉上次遗留的历史 |

## 11. 许可

Apache-2.0。
