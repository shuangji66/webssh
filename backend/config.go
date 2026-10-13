package main

import (
	"os"
	"path/filepath"
	"time"
)

// trimDefaultSocket 是飞牛开放 API 网关（trim.file.*）的 socket，只在没有
// WEBSSH_TRIM_SOCKET 时用。里面放的是系统常量而不是应用配置，所以写死在代码里。
const trimDefaultSocket = "/var/run/trim_open_gateway_apiscope.socket"

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
	}
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
