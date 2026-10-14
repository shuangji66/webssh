package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"webssh/backend/internal/logx"
)

type spaHandler struct {
	fsys fs.FS
	base string // 已去掉尾部斜杠
}

func newSPAHandler(fsys fs.FS, baseurl string) http.Handler {
	// 内嵌的是 embed/ 目录本身，取子目录后内部的路径就是 "index.html" / "assets/…"。
	if sub, err := fs.Sub(fsys, "embed"); err == nil {
		fsys = sub
	}
	return &spaHandler{fsys: fsys, base: strings.TrimRight(baseurl, "/")}
}

func (s *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}
	if b, err := fs.ReadFile(s.fsys, reqPath); err == nil {
		s.serve(w, reqPath, b)
		return
	}
	// 不存在的路径回退到 index.html（SPA 前端路由：/session/xxx 这类地址刷新后要能打开）。
	b, err := fs.ReadFile(s.fsys, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serve(w, "index.html", b)
}

func (s *spaHandler) serve(w http.ResponseWriter, name string, b []byte) {
	if strings.HasSuffix(name, ".html") && s.base != "" {
		b = rewriteIndexBase(b, s.base)
	}
	w.Header().Set("Cache-Control", cacheControlFor(name))
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// rewriteIndexBase 注入运行期 baseurl。
//
// baseurl 构建期未知（部署在哪个前缀由网关决定），前端产物用相对 base（./assets/…）
// 构建；这里把 <base href> 塞进 <head> 并把 ./assets/ 改写成 <base>/assets/。
// 注意 <base href> 必须以 `/` 结尾：否则相对路径会基于上一级目录解析。
func rewriteIndexBase(body []byte, base string) []byte {
	s := string(body)
	s = strings.ReplaceAll(s, `src="./assets/`, `src="`+base+`/assets/`)
	s = strings.ReplaceAll(s, `href="./assets/`, `href="`+base+`/assets/`)
	tag := `<base href="` + base + `/">`
	if idx := strings.Index(strings.ToLower(s), "<head"); idx != -1 {
		if closeIdx := strings.Index(s[idx:], ">"); closeIdx != -1 {
			pos := idx + closeIdx + 1
			return []byte(s[:pos] + tag + s[pos:])
		}
	}
	return []byte(tag + s)
}

// cacheControlFor 返回静态资源的 Cache-Control：
//   - `*.html` → no-cache：后端每次响应都要注入运行期 <base href>，缓存住会把旧页面钉死
//     （用户拿到的还是上一个 baseurl，接口全 404）。
//   - `assets/*` → immutable：Vite 产物文件名带内容哈希，内容变了文件名就变。
//   - 其余根级文件（图标 / 字体 / manifest）→ 一天：文件名无哈希，给个折中值。
func cacheControlFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "no-cache, must-revalidate"
	case strings.HasPrefix(path.Clean(name), "assets/"):
		return "public, max-age=31536000, immutable"
	default:
		return "public, max-age=86400"
	}
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".woff"):
		return "font/woff"
	case strings.HasSuffix(name, ".map"):
		return "application/json; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// unix socket 入口
// ---------------------------------------------------------------------------

// listenAdminSocket 绑定唯一访问入口。
//
// 绑定必须**成功**才进信号等待：早先的实现（参考项目）在 goroutine 里 bind、失败只打
// 日志，于是进程会「活着但没有任何 socket」——supervisor 看不出异常，用户访问不了，
// 也查不出原因。
//
// 顺序同样重要：**先直接绑**，绑不上再判断「是残留文件还是真有实例在跑」。旧实现是
// 「无条件 os.Remove 再 Listen」，在「两个实例同时启动」时会把先到者**正在使用**的
// socket 文件 unlink 掉：后到者绑定成功，先到者变成不可达却还在读写同一个数据目录
// （两个进程共用数据目录会互相覆盖，见 AGENTS.md 第 1 节）。现在只有确认「没有活着的
// 监听者」才删残留文件重试，任何情况下都不会 unlink 一个有人用的 socket。
func listenAdminSocket(renv *RuntimeEnv) (net.Listener, error) {
	if dir := filepath.Dir(renv.AdminSock); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", renv.AdminSock)
	if err == nil {
		return setSocketPerm(ln, renv.AdminSock)
	}
	// 绑不上：先探活。能连上说明真有实例在监听（调用方会据此退出）。
	if c, derr := net.DialTimeout("unix", renv.AdminSock, socketProbeTimeout); derr == nil {
		_ = c.Close()
		return nil, fmt.Errorf("admin socket %s 已被占用：%w", renv.AdminSock, err)
	}
	// 连不上：上一次异常退出留下的残留 socket 文件（EADDRINUSE），删掉再绑一次。
	if rerr := os.Remove(renv.AdminSock); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return nil, err
	}
	ln, err = net.Listen("unix", renv.AdminSock)
	if err != nil {
		return nil, err
	}
	return setSocketPerm(ln, renv.AdminSock)
}

// socketProbeTimeout 是「这个 socket 文件背后还有没有活着的监听者」的探活超时：
// 本地 unix socket 要么立刻连上要么立刻被拒，500ms 足够，也不会拖慢启动。
const socketProbeTimeout = 500 * time.Millisecond

// setSocketPerm 把 socket 设成 0660：nginx / 网关与进程同组即可访问，同组之外进不来。
func setSocketPerm(ln net.Listener, sock string) (net.Listener, error) {
	if err := os.Chmod(sock, 0o660); err != nil {
		logx.Warnf("设置 socket 权限失败：%v", err)
	}
	return ln, nil
}

// ---------------------------------------------------------------------------
// 统一网关转发的当前用户
// ---------------------------------------------------------------------------

// gatewayUser 是 RuntimeInfo.gateway（契约第 4.3 节）。
type gatewayUser struct {
	UID         int    `json:"uid"`
	Username    string `json:"username"`
	IsAdmin     bool   `json:"isAdmin"`
	FromGateway bool   `json:"fromGateway"`
}

// gatewayUserOf 解析网关转发的 X-Trim-* 头。
//
// 网关只在校验过登录态之后才转发这组头，所以它可以作为**可信身份**——注意是「可信身份」
// 而不是「已授权」：能不能读某个本机路径仍然要用 uid 去问 trim.file.checkUserACL。
// 没有网关时（直接调 socket / 开发）退化成进程自己的 uid。
func gatewayUserOf(r *http.Request) gatewayUser {
	u := gatewayUser{UID: os.Getuid()}
	if cur, err := user.Current(); err == nil {
		u.Username = cur.Username
	}
	raw := strings.TrimSpace(r.Header.Get("X-Trim-Userid"))
	if raw == "" {
		return u
	}
	uid, err := strconv.Atoi(raw)
	if err != nil || uid < 0 {
		return u
	}
	u.UID = uid
	u.Username = strings.TrimSpace(r.Header.Get("X-Trim-Username"))
	if u.Username == "" {
		if looked, lerr := user.LookupId(raw); lerr == nil {
			u.Username = looked.Username
		}
	}
	u.IsAdmin = strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")), "true")
	u.FromGateway = true
	return u
}
