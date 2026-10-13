package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trimDefaultSocket 是飞牛开放 API 网关（trim.file.*）的 socket，只在没有
// WEBSSH_TRIM_SOCKET 时用。里面放的是系统常量而不是应用配置，所以写死在代码里。
const trimDefaultSocket = "/var/run/trim_open_gateway_apiscope.socket"

// defaultArchiveDir 是远程压缩包的输出目录（契约 11.4：固定 /tmp/webssh-archive）。
//
// 为什么写死在这个位置而不是放到数据目录里：压缩包属于**远程主机**上的产物（tar 在远端
// 打包），数据目录是本机的；放在 /tmp 下是为了让用户能在远端文件管理器里立刻看到并下载它。
// 飞牛的 /tmp 通常可写；实在不可写的环境下用 WEBSSH_ARCHIVE_DIR 换一个目录。
const defaultArchiveDir = "/tmp/webssh-archive"

// RuntimeEnv 是全部解析自环境变量的运行期配置（契约第 1 节）。
//
// 本应用**只能经 unix socket 访问**（不监听任何 TCP 端口）：nginx / 飞牛统一网关按
// WEBSSH_ADMIN_BASEURL 前缀把 HTTP 与 WebSocket 反代到这个 socket。baseurl 构建期
// 未知，所以响应 index.html 时才注入 <base href>（见 admin.go）。
type RuntimeEnv struct {
	AdminSock    string        // WEBSSH_ADMIN_SOCK     唯一访问入口
	AdminBaseURL string        // WEBSSH_ADMIN_BASEURL  前端资源前缀（空 = 根路径）
	DataDir      string        // WEBSSH_DATA_DIR       数据目录（会话配置必须活过重启）
	DialTimeout  time.Duration // WEBSSH_DIAL_TIMEOUT   SSH 建连 + 握手超时
	TrimSocket   string        // WEBSSH_TRIM_SOCKET    飞牛开放 API 网关 socket
	TrimApp      string        // WEBSSH_TRIM_APP       调开放 API 时带的 appName
	MasterKeyEnv string        // WEBSSH_MASTER_KEY     外部提供的主密钥（32 字节 base64/hex）
	Version      string        // -ldflags 注入的版本号
	// ArchiveDir 是远端压缩包的输出目录（WEBSSH_ARCHIVE_DIR，默认 /tmp/webssh-archive）。
	ArchiveDir string
	// ExportRoot 是「导出至飞牛」的目标目录：TRIM_DATA_SHARE_PATHS 里第一个冒号**之前**的
	// 那一段（等价 bash `${TRIM_DATA_SHARE_PATHS%%:*}`），没有该变量时为空串 = 该功能不可用。
	//
	// 为什么只取第一段：这个变量是飞牛给应用用的**共享目录列表**（冒号分隔），前端能选中的
	// 就是第一个共享卷；多段时把整串当路径拼出来必然是个不存在的目录（契约 11.4/11.5）。
	ExportRoot string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultBase 是 socket 与数据目录的默认父目录。
//
// 用 os.TempDir()（Makefile 把它指到项目本地目录）而不是写死 /tmp：飞牛的存储卷上
// 系统 /tmp 的权限受限，而进程自己的 TMPDIR 一定可写。
func defaultBase() string {
	return filepath.Join(os.TempDir(), "webssh")
}

func loadRuntimeEnv() *RuntimeEnv {
	base := defaultBase()
	return &RuntimeEnv{
		AdminSock:    envOr("WEBSSH_ADMIN_SOCK", filepath.Join(base, "app.sock")),
		AdminBaseURL: os.Getenv("WEBSSH_ADMIN_BASEURL"),
		DataDir:      envOr("WEBSSH_DATA_DIR", filepath.Join(base, "data")),
		DialTimeout:  dialTimeoutFromEnv(),
		TrimSocket:   envOr("WEBSSH_TRIM_SOCKET", trimDefaultSocket),
		TrimApp:      envOr("WEBSSH_TRIM_APP", "WebSSH"),
		MasterKeyEnv: os.Getenv("WEBSSH_MASTER_KEY"),
		Version:      appVersion,
		ArchiveDir:   envOr("WEBSSH_ARCHIVE_DIR", defaultArchiveDir),
		ExportRoot:   trimDataShareRoot(os.Getenv("TRIM_DATA_SHARE_PATHS")),
	}
}

// trimDataShareRoot 取 TRIM_DATA_SHARE_PATHS 里第一个冒号之前的那一段（${VAR%%:*}）。
//
// 顺带 trim 掉首尾空白：飞牛写这个变量时可能带空格（"a:b" 或 "a : b"），带尾空格的路径
// 拿去 os.Stat 必然失败，而报错看起来会像「共享目录不存在」，排查方向全歪。
// 未设置 / 全是空白 → 空串（上层据此判定「导出至飞牛不可用」，见 exportnas.go）。
func trimDataShareRoot(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// dialTimeoutFromEnv 解析 WEBSSH_DIAL_TIMEOUT。
//
// 解析失败时回退默认值并记一条日志，而不是拒绝启动：一个写错的环境变量不该让整个
// 应用起不来；但也不能静默吞掉，否则用户会以为自己的设置生效了（于是「怎么还是
// 15 秒超时」永远查不出原因）。
func dialTimeoutFromEnv() time.Duration {
	const def = 15 * time.Second
	raw := os.Getenv("WEBSSH_DIAL_TIMEOUT")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		warnf("WEBSSH_DIAL_TIMEOUT=%q 不是合法的时长（如 15s），已回退默认 %s", raw, def)
		return def
	}
	return d
}
