package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"webssh/backend/internal/trimapi"
)

// ---------------------------------------------------------------------------
// 快捷指令
// ---------------------------------------------------------------------------

func TestQuickCmdsGlobalAndSession(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 空表也要返回 []（不是 null）：前端直接 .map() 它
	out := a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if out["scope"] != "global" {
		t.Errorf("scope = %v，期望 global", out["scope"])
	}
	if cmds := listOf(out, "commands"); cmds == nil || len(cmds) != 0 {
		t.Errorf("空表应当返回 []：%v", out)
	}

	// 保存（第一条不带 id，由服务端补）
	out = a.ok(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"name": "磁盘", "content": "df -h", "auto": true},
		map[string]any{"id": "cmd2", "name": "内存", "content": "free -m", "auto": false},
	}})
	cmds := listOf(out, "commands")
	if len(cmds) != 2 {
		t.Fatalf("命令数 = %d：%v", len(cmds), cmds)
	}
	first, _ := cmds[0].(map[string]any)
	if strOf(first, "id") == "" || len(strOf(first, "id")) != 32 {
		t.Errorf("缺失的 id 应当由服务端补齐：%v", first)
	}
	if strOf(first, "name") != "磁盘" || strOf(first, "content") != "df -h" || first["auto"] != true {
		t.Errorf("QuickCmd 字段不对：%v", first)
	}
	if strOf(cmds[1].(map[string]any), "id") != "cmd2" {
		t.Errorf("已有 id 不该被改：%v", cmds[1])
	}
	// 顺序必须保持（前端用它渲染按钮顺序）
	if strOf(cmds[0].(map[string]any), "content") != "df -h" || strOf(cmds[1].(map[string]any), "content") != "free -m" {
		t.Errorf("顺序被改了：%v", cmds)
	}

	// 会话专属是另一张表
	out = a.ok(t, "/api/quickcmds/list", map[string]any{"sid": sid})
	if out["scope"] != "session" || len(listOf(out, "commands")) != 0 {
		t.Fatalf("会话专属表应当是空的：%v", out)
	}
	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": sid, "commands": []any{
		map[string]any{"name": "专属", "content": "ls -l", "auto": true},
	}})
	out = a.ok(t, "/api/quickcmds/list", map[string]any{"sid": sid})
	if len(listOf(out, "commands")) != 1 {
		t.Fatalf("会话专属表 = %v", out)
	}
	out = a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if len(listOf(out, "commands")) != 2 {
		t.Fatalf("全局表被会话专属污染了：%v", out)
	}

	// 校验：名称 / 内容为空 → invalid；未知会话 → session_gone
	a.fail(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"name": "", "content": "x", "auto": false},
	}}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"name": "n", "content": "  ", "auto": false},
	}}, "invalid", 0)
	a.fail(t, "/api/quickcmds/list", map[string]any{"sid": "ffffffffffffffffffffffffffffffff"}, "session_gone", http.StatusNotFound)
	// 上限
	many := make([]any, maxQuickCmds+1)
	for i := range many {
		many[i] = map[string]any{"name": "n", "content": "c", "auto": false}
	}
	a.fail(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": many}, "too_large", http.StatusRequestEntityTooLarge)
	// 重复 id 去重
	out = a.ok(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"id": "dup", "name": "a", "content": "1", "auto": false},
		map[string]any{"id": "dup", "name": "b", "content": "2", "auto": false},
	}})
	if len(listOf(out, "commands")) != 1 {
		t.Errorf("重复 id 应当被去重：%v", out)
	}

	// 落盘位置与重启后的持久化
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "quickcmds", "global.json")); err != nil {
		t.Errorf("全局快捷指令没有落盘：%v", err)
	}
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "quickcmds", sid+".json")); err != nil {
		t.Errorf("会话快捷指令没有落盘：%v", err)
	}
	b := a.restart(t)
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if len(listOf(out, "commands")) != 1 {
		t.Errorf("重启后全局快捷指令丢了：%v", out)
	}
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": sid})
	if len(listOf(out, "commands")) != 1 {
		t.Errorf("重启后会话快捷指令丢了：%v", out)
	}
}

// ---------------------------------------------------------------------------
// 飞牛开放 API
// ---------------------------------------------------------------------------

// fakeGateway 是一个假的飞牛开放 API 网关（unix socket 上的 HTTP 服务）。
type fakeGateway struct {
	ln      net.Listener
	sock    string
	mu      chan struct{} // 用容量 1 的 channel 当锁，避免额外依赖
	lastReq []byte
	resp    func(req map[string]any) (int, any)
}

func startFakeGateway(t *testing.T, resp func(req map[string]any) (int, any)) *fakeGateway {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "trim.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("绑定假网关 socket 失败：%v", err)
	}
	g := &fakeGateway{ln: ln, sock: sock, mu: make(chan struct{}, 1), resp: resp}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/trimapp" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+os.Getenv(trimapi.EnvToken) {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"code":1000002,"msg":"bad token"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		g.mu <- struct{}{}
		g.lastReq = body
		<-g.mu
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status, out := g.resp(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return g
}

func (g *fakeGateway) last() map[string]any {
	g.mu <- struct{}{}
	defer func() { <-g.mu }()
	var out map[string]any
	_ = json.Unmarshal(g.lastReq, &out)
	return out
}

// TestTrimUnavailable 校验没有飞牛网关时的行为：如实报不可用 + 给出原因。
func TestTrimUnavailable(t *testing.T) {
	a := newTestApp(t, appOptions{})
	_, out := a.get(t, "/api/trim/status")
	if out["ok"] != true {
		t.Fatalf("/api/trim/status 响应 = %v", out)
	}
	trim, _ := out["trim"].(map[string]any)
	if trim["available"] != false || strOf(trim, "message") == "" || strOf(trim, "app") != "WebSSH" {
		t.Fatalf("trim 状态 = %v", trim)
	}
	// ACL 接口返回 unavailable（前端据此把「从飞牛选择」置灰）
	a.fail(t, "/api/trim/acl", map[string]any{"paths": []string{"/vol1/x"}}, "unavailable", http.StatusServiceUnavailable)
}

// TestTrimACL 校验开放 API 调用：token、reqId、uid 来源与结果映射。
func TestTrimACL(t *testing.T) {
	t.Setenv(trimapi.EnvToken, "test-token")
	g := startFakeGateway(t, func(req map[string]any) (int, any) {
		if req["req"] != "trim.file.checkUserACL" {
			return http.StatusOK, map[string]any{"code": 1, "msg": "unexpected req"}
		}
		data, _ := req["data"].(map[string]any)
		paths, _ := data["path"].([]any)
		list := make([]map[string]any, 0, len(paths))
		for _, p := range paths {
			list = append(list, map[string]any{
				"path": p, "readable": true, "writable": p != "/vol1/readonly", "deletable": false,
			})
		}
		return http.StatusOK, map[string]any{"reqId": req["reqId"], "code": 0, "msg": "", "data": list}
	})
	a := newTestApp(t, appOptions{trimSocket: g.sock})

	_, out := a.get(t, "/api/trim/status")
	trim, _ := out["trim"].(map[string]any)
	if trim["available"] != true {
		t.Fatalf("假网关下应当可用：%v", out)
	}
	if trim["app"] != "WebSSH" {
		t.Errorf("app = %v", trim["app"])
	}

	res, raw := a.do(t, http.MethodPost, "/api/trim/acl", map[string]any{"paths": []string{"/vol1/a", "/vol1/readonly"}},
		map[string]string{"X-Trim-Userid": "1000"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ACL 状态码 = %d：%s", res.StatusCode, snippetOf(raw))
	}
	aclOut := decodeJSONBody(t, raw)
	list := listOf(aclOut, "list")
	if len(list) != 2 {
		t.Fatalf("ACL 列表 = %v", aclOut)
	}
	entry, _ := list[0].(map[string]any)
	if strOf(entry, "path") != "/vol1/a" || entry["readable"] != true || entry["writable"] != true || entry["deletable"] != false {
		t.Fatalf("AclEntry 字段不对：%v", entry)
	}
	if list[1].(map[string]any)["writable"] != false {
		t.Errorf("第二条的 writable 解析错了：%v", list[1])
	}
	// uid 必须来自网关头（请求体 / 查询串里的 uid 一律不认）
	sent := g.last()
	data, _ := sent["data"].(map[string]any)
	if data["uid"] != float64(1000) {
		t.Errorf("传给开放 API 的 uid = %v，期望 1000（来自 X-Trim-Userid）", data["uid"])
	}
	if strOf(sent, "appName") != "WebSSH" {
		t.Errorf("appName = %v", sent["appName"])
	}
	if strOf(sent, "reqId") == "" {
		t.Error("reqId 为空")
	}
}

// TestTrimPermissionError 校验 code 1000002 被翻成「权限不足」并带上排查方向。
func TestTrimPermissionError(t *testing.T) {
	t.Setenv(trimapi.EnvToken, "test-token")
	g := startFakeGateway(t, func(req map[string]any) (int, any) {
		return http.StatusForbidden, map[string]any{"code": 1000002, "msg": "管理员权限或 API Scope 不足"}
	})
	a := newTestApp(t, appOptions{trimSocket: g.sock})
	res, raw := a.do(t, http.MethodPost, "/api/trim/acl", map[string]any{"paths": []string{"/vol1/a"}}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d（期望 403）：%s", res.StatusCode, snippetOf(raw))
	}
	out := decodeJSONBody(t, raw)
	if out["code"] != "permission" {
		t.Fatalf("错误码 = %v，期望 permission", out["code"])
	}
	if msg := strOf(out, "error"); !strings.Contains(msg, "api-scope") {
		t.Errorf("错误文案应当提示 api-scope：%q", msg)
	}
}

// TestUploadFromNasACLDenied 校验「用户对本机文件没有读权限」时拒绝上传。
func TestUploadFromNasACLDenied(t *testing.T) {
	t.Setenv(trimapi.EnvToken, "test-token")
	g := startFakeGateway(t, func(req map[string]any) (int, any) {
		data, _ := req["data"].(map[string]any)
		paths, _ := data["path"].([]any)
		list := make([]map[string]any, 0, len(paths))
		for _, p := range paths {
			list = append(list, map[string]any{"path": p, "readable": false, "writable": false, "deletable": false})
		}
		return http.StatusOK, map[string]any{"code": 0, "data": list}
	})
	a := newTestApp(t, appOptions{trimSocket: g.sock})
	sid := a.newSSHSession(t)
	nas := filepath.Join(t.TempDir(), "denied.txt")
	writeLocal(t, nas, "x")

	a.fail(t, "/api/fs/upload-from-nas", map[string]any{
		"sid": sid, "path": a.sshRoot, "source": nas, "overwrite": true,
	}, "permission", http.StatusForbidden)
}

// TestTrimClientErrorMapping 覆盖「缺 token」「响应不是 JSON」两类失败。
func TestTrimClientErrorMapping(t *testing.T) {
	// 缺 token
	g := startFakeGateway(t, func(req map[string]any) (int, any) {
		return http.StatusOK, map[string]any{"code": 0, "data": []any{}}
	})
	a := newTestApp(t, appOptions{trimSocket: g.sock})
	t.Setenv(trimapi.EnvToken, "")
	_, out := a.get(t, "/api/trim/status")
	trim, _ := out["trim"].(map[string]any)
	if trim["available"] != false {
		t.Fatalf("没有 token 时不该判为可用：%v", out)
	}
	a.fail(t, "/api/trim/acl", map[string]any{"paths": []string{"/x"}}, "unavailable", http.StatusServiceUnavailable)

	// 响应不是合法 JSON → internal
	bad := startFakeGateway(t, func(req map[string]any) (int, any) {
		return http.StatusOK, "not-a-json-object"
	})
	_ = bad
	b := newTestApp(t, appOptions{trimSocket: bad.sock})
	client := trimapi.NewClient(b.renv.TrimSocket, b.renv.TrimApp)
	if _, err := client.CheckUserACL(context.Background(), 0, []string{"/x"}); err == nil {
		t.Error("非法 JSON 响应应当返回错误")
	}
}
