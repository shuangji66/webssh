package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInfoResponse 校验 /api/info 的字段名与语义（前端的 RuntimeInfo 逐字对应）。
func TestInfoResponse(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	_, out := a.get(t, "/api/info")
	if out["ok"] != true {
		t.Fatalf("ok 应为 true：%v", out)
	}
	rt, _ := out["runtime"].(map[string]any)
	if rt == nil {
		t.Fatalf("缺少 runtime：%v", out)
	}
	if rt["version"] != "test-version" {
		t.Errorf("version = %v，期望 test-version", rt["version"])
	}
	if boot := strOf(rt, "bootId"); len(boot) != idLen {
		t.Errorf("bootId = %q，期望 %d 位十六进制（前端靠它判断后端是否换进程）", boot, idLen)
	}
	if rt["dataDir"] != a.renv.DataDir {
		t.Errorf("dataDir = %v，期望 %v", rt["dataDir"], a.renv.DataDir)
	}
	if rt["adminSock"] != a.renv.AdminSock {
		t.Errorf("adminSock = %v，期望 %v", rt["adminSock"], a.renv.AdminSock)
	}
	if strOf(rt, "hostname") == "" {
		t.Error("hostname 为空")
	}
	trim, _ := rt["trim"].(map[string]any)
	if trim == nil {
		t.Fatalf("缺少 trim：%v", rt)
	}
	if trim["available"] != false {
		t.Errorf("没有飞牛网关时 trim.available 应为 false：%v", trim)
	}
	if strOf(trim, "message") == "" {
		t.Error("trim 不可用时必须给出原因（message 为空）")
	}
	if trim["app"] != "WebSSH" {
		t.Errorf("trim.app = %v，期望 WebSSH", trim["app"])
	}
	gw, _ := rt["gateway"].(map[string]any)
	if gw == nil {
		t.Fatalf("缺少 gateway：%v", rt)
	}
	if numOf(gw, "uid") != float64(os.Getuid()) {
		t.Errorf("没有网关头时 gateway.uid 应为进程 uid %d：%v", os.Getuid(), gw)
	}
	if gw["fromGateway"] != false {
		t.Errorf("没有网关头时 fromGateway 应为 false：%v", gw)
	}
}

// TestGatewayHeaders 校验统一网关转发头被解析成可信身份。
func TestGatewayHeaders(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	res, raw := a.do(t, http.MethodGet, "/api/info", nil, map[string]string{
		"X-Trim-Userid":   "1000",
		"X-Trim-Username": "alice",
		"X-Trim-Isadmin":  "TRUE",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", res.StatusCode)
	}
	out := decodeJSONBody(t, raw)
	gw, _ := out["runtime"].(map[string]any)
	gw, _ = gw["gateway"].(map[string]any)
	if numOf(gw, "uid") != 1000 || strOf(gw, "username") != "alice" || gw["isAdmin"] != true || gw["fromGateway"] != true {
		t.Fatalf("网关身份解析不对：%v", gw)
	}
}

// TestStaticCacheHeadersAndSPA 校验静态资源的缓存头、SPA 回退与 /api/* 的 404 JSON。
func TestStaticCacheHeadersAndSPA(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})

	// html 不能缓存：后端每次响应都要注入运行期 <base href>，缓存住会把旧页面钉死。
	res, raw := a.do(t, http.MethodGet, "/", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首页状态码 = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("index.html 的 Cache-Control = %q", cc)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index.html 的 Content-Type = %q", ct)
	}
	if len(raw) == 0 {
		t.Error("首页响应为空（embed 里没有 index.html？）")
	}

	// 前端路由地址刷新后要能打开（回退 index.html）。
	res2, _ := a.do(t, http.MethodGet, "/session/1f0c", nil, nil)
	if res2.StatusCode != http.StatusOK {
		t.Errorf("SPA 回退状态码 = %d", res2.StatusCode)
	}
	if cc := res2.Header.Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("SPA 回退的 Cache-Control = %q", cc)
	}

	// /api/* 未命中必须是 404 JSON（回退 HTML 会让前端 JSON.parse 报 "Unexpected token <"）。
	res3, out3 := a.post(t, "/api/does-not-exist", map[string]any{})
	if res3.StatusCode != http.StatusNotFound {
		t.Errorf("未命中接口状态码 = %d，期望 404", res3.StatusCode)
	}
	if out3["ok"] != false || out3["code"] != "not_found" {
		t.Errorf("未命中接口响应 = %v", out3)
	}

	// 方法不对：/api/info 只支持 GET。
	res4, out4 := a.post(t, "/api/info", nil)
	if res4.StatusCode != http.StatusMethodNotAllowed || out4["code"] != "invalid" {
		t.Errorf("POST /api/info 响应 = %d %v", res4.StatusCode, out4)
	}
	// 静态资源只支持 GET。
	res5, out5 := a.post(t, "/index.html", nil)
	if res5.StatusCode != http.StatusMethodNotAllowed || out5["ok"] != false {
		t.Errorf("POST /index.html 响应 = %d %v", res5.StatusCode, out5)
	}

	// 缓存头三分类（函数级断言：assets/ 目录只有真实构建产物里才有）。
	cases := map[string]string{
		"index.html":           "no-cache, must-revalidate",
		"assets/x-abc123.js":   "public, max-age=31536000, immutable",
		"./assets/x-abc12.css": "public, max-age=31536000, immutable",
		"favicon.ico":          "public, max-age=86400",
		"manifest.webmanifest": "public, max-age=86400",
	}
	for name, want := range cases {
		if got := cacheControlFor(name); got != want {
			t.Errorf("cacheControlFor(%q) = %q，期望 %q", name, got, want)
		}
	}
}

// TestBaseURLInjection 校验 baseurl 前缀（网关转发）与 <base href> 注入。
func TestBaseURLInjection(t *testing.T) {
	a := newTestApp(t, appOptions{baseURL: "/app/WebSSH", noSSH: true})

	// 带前缀的接口可用（网关转发时就是这个形态）。
	res, raw := a.do(t, http.MethodGet, "/api/info", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("带前缀的 /api/info 状态码 = %d（原文：%s）", res.StatusCode, snippetOf(raw))
	}
	// 首页注入 <base href="<baseurl>/">。
	res, raw = a.do(t, http.MethodGet, "/", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("首页状态码 = %d", res.StatusCode)
	}
	if !strings.Contains(string(raw), `<base href="/app/WebSSH/">`) {
		t.Errorf("首页没有注入 base href：%s", snippetOf(raw))
	}

	// baseurl 为空时**不注入**（部署在根路径下，index.html 保持原样）。
	b := newTestApp(t, appOptions{noSSH: true})
	_, rawB := b.do(t, http.MethodGet, "/", nil, nil)
	if strings.Contains(string(rawB), "<base href=") {
		t.Errorf("baseurl 为空时不该注入 base href：%s", snippetOf(rawB))
	}
}

// TestRewriteIndexBase 用一份 Vite 形态的 index.html 验证路径改写。
func TestRewriteIndexBase(t *testing.T) {
	src := `<!DOCTYPE html><html><head><meta charset="UTF-8"><title>x</title></head>` +
		`<body><div id="app"></div><script type="module" src="./assets/index-a1b2.js"></script>` +
		`<link rel="stylesheet" href="./assets/index-c3d4.css"></body></html>`
	got := string(rewriteIndexBase([]byte(src), "/app/WebSSH"))
	if !strings.Contains(got, `<base href="/app/WebSSH/">`) {
		t.Errorf("缺少 base 标签：%s", got)
	}
	if !strings.Contains(got, `src="/app/WebSSH/assets/index-a1b2.js"`) {
		t.Errorf("js 路径没改写：%s", got)
	}
	if !strings.Contains(got, `href="/app/WebSSH/assets/index-c3d4.css"`) {
		t.Errorf("css 路径没改写：%s", got)
	}
	if strings.Contains(got, `"./assets/`) {
		t.Errorf("还残留相对路径：%s", got)
	}
	// base 标签必须紧跟 <head ...> 之后（浏览器只认第一个 base）。
	headIdx := strings.Index(got, "<head>")
	baseIdx := strings.Index(got, "<base ")
	if headIdx < 0 || baseIdx != headIdx+len("<head>") {
		t.Errorf("base 标签位置不对：%s", got)
	}
}

// TestBodySizeLimits 校验 JSON body 上限（1 MiB）。
func TestBodySizeLimits(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	big := `{"sid":"` + strings.Repeat("a", maxJSONBytes) + `"}`
	res, out := a.post(t, "/api/fs/list", big)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body 状态码 = %d，期望 413（响应：%v）", res.StatusCode, out)
	}
	if out["code"] != "too_large" {
		t.Fatalf("超大 body 错误码 = %v，期望 too_large", out["code"])
	}
	// 空 body 要给出「缺请求体」而不是 500。
	res2, out2 := a.post(t, "/api/fs/list", nil)
	if res2.StatusCode != http.StatusBadRequest || out2["code"] != "invalid" {
		t.Fatalf("空 body 响应 = %d %v", res2.StatusCode, out2)
	}
}

// TestNoTCPListenerInProduction 是一条静态断言：生产代码里**不允许**出现 TCP 监听。
//
// AGENTS/契约都把「只有 unix socket 一个入口」写成了硬约束，而这种约束一旦被破坏
// （有人图省事加一个调试端口）从测试上完全看不出来。这里直接读源码断言，
// 故意把 internal/testssh 排除在外——那个包只用于测试，本来就监听 127.0.0.1。
func TestNoTCPListenerInProduction(t *testing.T) {
	root := "."
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testssh" || d.Name() == "embed" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		body := string(src)
		for _, bad := range []string{`net.Listen("tcp"`, `net.Listen("tcp4"`, `net.Listen("tcp6"`, `http.ListenAndServe`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s 里出现了 TCP 监听（%s）：本应用只能经 unix socket 提供服务", p, bad)
			}
		}
		if strings.Contains(body, "net.Listen(") && !strings.Contains(body, `net.Listen("unix"`) {
			t.Errorf("%s 里有 net.Listen 但不是 unix socket", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败：%v", err)
	}
}

// TestSocketFilePermissions 校验 socket 由 listenAdminSocket 创建并可连。
func TestSocketFilePermissions(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	st, err := os.Stat(a.renv.AdminSock)
	if err != nil {
		t.Fatalf("socket 不存在：%v", err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s 不是 socket（mode %v）", a.renv.AdminSock, st.Mode())
	}
	if perm := st.Mode().Perm(); perm != 0o660 {
		t.Errorf("socket 权限 = %o，期望 660", perm)
	}
}
