package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"webssh/backend/internal/store"
	"webssh/backend/internal/testssh"
	"webssh/backend/internal/ws"
)

// ---------------------------------------------------------------------------
// 集成测试的脚手架
//
// 每个测试都跑在**真实链路**上：进程内 SSH/SFTP 服务器（testssh）+ 真实的 unix socket
// HTTP 服务 + 手写的 WebSocket 客户端。不 mock 任何一层——契约里的坑（PTY 尺寸、
// 单挂载点、历史回放、SFTP 路径语义）只有在真链路上才会暴露。
// ---------------------------------------------------------------------------

const testTimeout = 15 * time.Second

type testApp struct {
	t      *testing.T
	renv   *RuntimeEnv
	mux    *AdminMux
	srv    *http.Server
	client *http.Client

	ssh     *testssh.Server
	sshRoot string
	// sshUser / sshPassword 是内嵌 SSH 服务器的登录凭据（默认 demo/demo）。
	// 有一个用例需要一把**不会在别处出现**的密码（验证「不保存凭据时磁盘上没有明文」），
	// 所以放在字段上而不是由各用例自己再拼一次请求。
	sshUser     string
	sshPassword string
}

type appOptions struct {
	// baseURL 模拟网关前缀（WEBSSH_ADMIN_BASEURL）。
	baseURL string
	// trimSocket 指向一个假的飞牛开放 API 网关 socket（空 = 不存在的路径 → 判定不可用）。
	trimSocket string
	// masterKeyEnv 模拟 WEBSSH_MASTER_KEY。
	masterKeyEnv string
	// noSSH 跳过进程内 SSH 服务器（只测 HTTP 层时省点开销）。
	noSSH bool
	// sshOptions 覆盖 testssh 的启动参数。
	sshOptions *testssh.Options
	// archiveDir / exportRoot 覆盖 RuntimeEnv 的对应字段（空 = 用测试临时目录）。
	// archiveDir 默认不用生产默认值 /tmp/webssh-archive：并行跑的测试会互相盖掉对方的产物，
	// 而且不该往真实 /tmp 里扔垃圾。
	archiveDir string
	exportRoot string
	// sshPassword 覆盖内嵌 SSH 服务器的密码（空 = demo）。
	sshPassword string
}

func newTestApp(t *testing.T, opts appOptions) *testApp {
	t.Helper()
	dir := t.TempDir()
	sshPassword := opts.sshPassword
	if sshPassword == "" {
		sshPassword = "demo"
	}
	archiveDir := opts.archiveDir
	if archiveDir == "" {
		archiveDir = filepath.Join(dir, "archive")
	}
	renv := &RuntimeEnv{
		AdminSock:    filepath.Join(dir, "app.sock"),
		AdminBaseURL: opts.baseURL,
		DataDir:      filepath.Join(dir, "data"),
		DialTimeout:  5 * time.Second,
		TrimSocket:   opts.trimSocket,
		TrimApp:      "WebSSH",
		MasterKeyEnv: opts.masterKeyEnv,
		Version:      "test-version",
		ArchiveDir:   archiveDir,
		ExportRoot:   opts.exportRoot,
	}
	if renv.TrimSocket == "" {
		renv.TrimSocket = filepath.Join(dir, "no-such-trim.sock")
	}
	mux, err := newAdminMux(renv)
	if err != nil {
		t.Fatalf("初始化后端失败：%v", err)
	}
	mux.SetSPA(embeddedFS)

	ln, err := listenAdminSocket(renv)
	if err != nil {
		t.Fatalf("绑定 unix socket 失败：%v", err)
	}
	srv := &http.Server{Handler: mux.buildHandler()}
	go func() { _ = srv.Serve(ln) }()

	a := &testApp{t: t, renv: renv, mux: mux, srv: srv, sshUser: "demo", sshPassword: sshPassword}
	a.client = newUnixClient(renv.AdminSock)
	t.Cleanup(func() {
		_ = srv.Close()
		mux.sessions.closeAll()
		_ = os.Remove(renv.AdminSock)
	})

	if !opts.noSSH {
		a.sshRoot = t.TempDir()
		sshOpts := testssh.Options{User: a.sshUser, Password: sshPassword, Root: a.sshRoot}
		if opts.sshOptions != nil {
			sshOpts = *opts.sshOptions
			if sshOpts.Root == "" {
				sshOpts.Root = a.sshRoot
			}
		}
		a.ssh = testssh.StartServer(t, sshOpts)
	}
	return a
}

// newUnixClient 构造一个只走 unix socket 的 HTTP 客户端。
func newUnixClient(sock string) *http.Client {
	return &http.Client{
		Timeout: testTimeout,
		Transport: &http.Transport{
			// 全部请求都走 unix socket——这个后端**不监听任何 TCP 端口**。
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// restart 用同一个数据目录重建后端（模拟进程重启：bootId 换新、内存态会话消失、配置保留）。
//
// 必须先关掉旧实例（listener + 终端/SFTP）：「重启」= 旧进程消失。不关就相当于两个实例
// 同时监听同一个 socket——新的 listenAdminSocket 会（正确地）拒绝抢一个活着的 socket，
// 而真实重启时旧进程早就没了。
//
// socket 文件**刻意不删**：模拟 kill -9 之后留下的残留文件，验证新实例能自己清掉它再绑。
func (a *testApp) restart(t *testing.T) *testApp {
	t.Helper()
	// 模拟新进程：bootId 是进程级随机标识，重启必然不同。
	bootID = store.NewID()
	// 旧连接池里可能还连着上一个服务实例：必须丢掉（否则请求会打到已经「重启」过的进程上，
	// 表现就是「重启后内存态居然还在」）。
	a.client.CloseIdleConnections()
	_ = a.srv.Close()
	a.mux.sessions.closeAll()

	next := &testApp{t: t, renv: a.renv}
	mux, err := newAdminMux(a.renv)
	if err != nil {
		t.Fatalf("重启后端失败：%v", err)
	}
	mux.SetSPA(embeddedFS)
	ln, err := listenAdminSocket(a.renv)
	if err != nil {
		t.Fatalf("重启后绑定 socket 失败：%v", err)
	}
	srv := &http.Server{Handler: mux.buildHandler()}
	go func() { _ = srv.Serve(ln) }()
	next.mux = mux
	next.srv = srv
	next.client = newUnixClient(a.renv.AdminSock)
	next.ssh = a.ssh
	next.sshRoot = a.sshRoot
	t.Cleanup(func() {
		_ = srv.Close()
		mux.sessions.closeAll()
		_ = os.Remove(a.renv.AdminSock)
	})
	return next
}

// path 把接口路径拼上 baseurl 前缀（模拟网关的转发）。
func (a *testApp) path(p string) string {
	base := strings.TrimRight(a.renv.AdminBaseURL, "/")
	return base + p
}

func (a *testApp) do(t *testing.T, method, p string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	case []byte:
		reader = strings.NewReader(string(v))
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体失败：%v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, "http://webssh"+a.path(p), reader)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败：%v", method, p, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	return res, raw
}

// post 发一个 JSON 请求并解出响应体。
func (a *testApp) post(t *testing.T, p string, body any) (*http.Response, map[string]any) {
	t.Helper()
	res, raw := a.do(t, http.MethodPost, p, body, nil)
	return res, decodeJSONBody(t, raw)
}

func (a *testApp) get(t *testing.T, p string) (*http.Response, map[string]any) {
	t.Helper()
	res, raw := a.do(t, http.MethodGet, p, nil, nil)
	return res, decodeJSONBody(t, raw)
}

func decodeJSONBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（原文：%s）", err, snippetOf(raw))
	}
	return out
}

func snippetOf(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// ok 发请求并断言 ok=true，返回响应体。
func (a *testApp) ok(t *testing.T, p string, body any) map[string]any {
	t.Helper()
	res, out := a.post(t, p, body)
	if res.StatusCode != http.StatusOK || out["ok"] != true {
		t.Fatalf("POST %s 期望成功，实际 %d: %v", p, res.StatusCode, out)
	}
	return out
}

// fail 发请求并断言错误码（裸码）与状态码。
func (a *testApp) fail(t *testing.T, p string, body any, wantCode string, wantStatus int) map[string]any {
	t.Helper()
	res, out := a.post(t, p, body)
	if out["ok"] != false {
		t.Fatalf("POST %s 期望失败，实际 %d: %v", p, res.StatusCode, out)
	}
	if out["code"] != wantCode {
		t.Fatalf("POST %s 错误码 = %v，期望 %v（响应：%v）", p, out["code"], wantCode, out)
	}
	if wantStatus != 0 && res.StatusCode != wantStatus {
		t.Fatalf("POST %s 状态码 = %d，期望 %d（响应：%v）", p, res.StatusCode, wantStatus, out)
	}
	if msg, _ := out["error"].(string); strings.TrimSpace(msg) == "" {
		t.Fatalf("POST %s 的错误响应缺少 error 文案：%v", p, out)
	}
	return out
}

// ---------------------------------------------------------------------------
// 会话辅助
// ---------------------------------------------------------------------------

// newSSHSession 建一个指向内嵌 SSH 服务器的会话并打开它，返回 sid。
func (a *testApp) newSSHSession(t *testing.T) string {
	t.Helper()
	out := a.ok(t, "/api/sessions/save", map[string]any{
		"name":     "local",
		"host":     a.ssh.Host(),
		"port":     a.ssh.Port(),
		"user":     a.sshUser,
		"authType": "password",
		"password": a.sshPassword,
	})
	sid := strOf(out["session"], "id")
	if sid == "" {
		t.Fatalf("保存会话没有返回 id：%v", out)
	}
	// 首次连接要信任主机密钥（TOFU）。
	a.trustHost(t, a.ssh)
	a.ok(t, "/api/sessions/open", map[string]any{"id": sid})
	return sid
}

func (a *testApp) trustHost(t *testing.T, srv *testssh.Server) {
	t.Helper()
	res, out := a.post(t, "/api/sessions/test", map[string]any{
		"host": srv.Host(), "port": srv.Port(), "user": a.sshUser, "authType": "password", "password": a.sshPassword,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("连接测试请求失败：%d %v", res.StatusCode, out)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("连接测试没有返回 result：%v", out)
	}
	if result["success"] == true {
		return // 已经信任过了
	}
	if result["code"] != "hostkey_unknown" {
		t.Fatalf("连接测试失败且不是「首次需要信任」：%v", result)
	}
	a.ok(t, "/api/sessions/trust", map[string]any{
		"host": srv.Host(), "port": srv.Port(), "fingerprint": result["fingerprint"],
	})
}

func strOf(v any, key string) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func numOf(v any, key string) float64 {
	m, _ := v.(map[string]any)
	if m == nil {
		return 0
	}
	f, _ := m[key].(float64)
	return f
}

func listOf(v any, key string) []any {
	m, _ := v.(map[string]any)
	if m == nil {
		return nil
	}
	l, _ := m[key].([]any)
	return l
}

// ---------------------------------------------------------------------------
// WebSocket 测试客户端
// ---------------------------------------------------------------------------

type wsClient struct {
	t       *testing.T
	conn    net.Conn
	br      *bufio.Reader
	closed  bool
	closeMu sync.Mutex
}

// dialWS 完成一次真实的 WebSocket 握手（unix socket 上自己写 HTTP 升级请求）。
func (a *testApp) dialWS(t *testing.T, sid, tid string) *wsClient {
	t.Helper()
	conn, err := net.Dial("unix", a.renv.AdminSock)
	if err != nil {
		t.Fatalf("连接 socket 失败：%v", err)
	}
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		t.Fatalf("生成 Sec-WebSocket-Key 失败：%v", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	target := a.path("/api/terminal/ws") + "?sid=" + url.QueryEscape(sid)
	if tid != "" {
		target += "&tid=" + url.QueryEscape(tid)
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: webssh\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", target, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("发送升级请求失败：%v", err)
	}
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(testTimeout))
	res, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取 WebSocket 握手响应失败：%v", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket 握手失败：%s", res.Status)
	}
	if got := res.Header.Get("Sec-WebSocket-Accept"); got != ws.AcceptKey(key) {
		t.Fatalf("Sec-WebSocket-Accept = %q，期望 %q", got, ws.AcceptKey(key))
	}
	w := &wsClient{t: t, conn: conn, br: br}
	t.Cleanup(w.close)
	return w
}

func (w *wsClient) close() {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	_ = w.conn.Close()
}

// writeText 发送一条掩码文本帧（客户端 → 服务端必须掩码）。
func (w *wsClient) writeText(payload string) {
	w.t.Helper()
	w.writeFrame(ws.OpText, []byte(payload))
}

func (w *wsClient) writeFrame(op byte, payload []byte) {
	w.writeFragment(op, payload, true)
}

// writeFragment 发送一帧，fin=false 表示后面还有续帧（ws.OpContinuation）。
func (w *wsClient) writeFragment(op byte, payload []byte, fin bool) {
	w.t.Helper()
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		w.t.Fatalf("连接已关闭，无法写帧")
	}
	frame := maskFrameWith(op, payload, fin)
	_ = w.conn.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := w.conn.Write(frame); err != nil {
		w.t.Fatalf("写 WebSocket 帧失败：%v", err)
	}
}

func maskFrame(op byte, payload []byte) []byte { return maskFrameWith(op, payload, true) }

// maskFrameWith 构造一个客户端帧；fin=false 用来做分片（代理/网关转发时会出现）。
func maskFrameWith(op byte, payload []byte, fin bool) []byte {
	n := len(payload)
	first := op
	if fin {
		first |= 0x80
	}
	var hdr []byte
	switch {
	case n < 126:
		hdr = []byte{first, 0x80 | byte(n)}
	case n < 65536:
		hdr = []byte{first, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		hdr = []byte{first, 0x80 | 127, 0, 0, 0, 0, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	mask := [4]byte{0xA1, 0xB2, 0xC3, 0xD4}
	hdr = append(hdr, mask[:]...)
	out := append(hdr, make([]byte, n)...)
	for i := 0; i < n; i++ {
		out[len(hdr)+i] = payload[i] ^ mask[i%4]
	}
	return out
}

// readFrame 读一帧；超时返回 error。
func (w *wsClient) readFrame(timeout time.Duration) (byte, []byte, error) {
	_ = w.conn.SetReadDeadline(time.Now().Add(timeout))
	op, _, payload, err := ws.ReadFrame(w.br)
	return op, payload, err
}

// readText 读到一帧终端数据（跳过 ping/pong），返回内容。
//
// 顺带钉住一条协议不变量：**服务端 → 客户端的终端数据必须是二进制帧**（见 protocol.go 的
// ⚠️：终端输出是任意字节流，文本帧会被浏览器按 UTF-8 校验并直接断连）。这里的 t.Errorf
// 是「没有浏览器可用」的测试环境里唯一能发现「有人把 opcode 改回文本帧」的地方。
func (w *wsClient) readText(timeout time.Duration) (string, error) {
	for {
		op, payload, err := w.readFrame(timeout)
		if err != nil {
			return "", err
		}
		switch op {
		case ws.OpBinary:
			return string(payload), nil
		case ws.OpText:
			w.t.Errorf("服务端终端数据必须用二进制帧，收到文本帧：%q", snippetOf(payload))
			return string(payload), nil
		case ws.OpClose:
			return "", io.EOF
		}
	}
}

// collectUntil 一直读到累积内容包含 want（或超时），返回累积内容。
func (w *wsClient) collectUntil(want string, timeout time.Duration) (string, bool) {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		chunk, err := w.readText(time.Until(deadline))
		if err != nil {
			return acc.String(), strings.Contains(acc.String(), want)
		}
		acc.WriteString(chunk)
		if strings.Contains(acc.String(), want) {
			return acc.String(), true
		}
	}
	return acc.String(), strings.Contains(acc.String(), want)
}

func (w *wsClient) mustCollectUntil(want string, timeout time.Duration) string {
	w.t.Helper()
	got, ok := w.collectUntil(want, timeout)
	if !ok {
		w.t.Fatalf("等待 %q 超时，收到：%q", want, got)
	}
	return got
}

// mustCollectCount 一直读到 want 出现 times 次（例如「回显一次 + 输出一次」）。
func (w *wsClient) mustCollectCount(want string, times int, timeout time.Duration) string {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		chunk, err := w.readText(time.Until(deadline))
		if err != nil {
			w.t.Fatalf("等待 %q 出现 %d 次失败：%v（已收到：%q）", want, times, err, acc.String())
		}
		acc.WriteString(chunk)
		if strings.Count(acc.String(), want) >= times {
			return acc.String()
		}
	}
	w.t.Fatalf("等待 %q 出现 %d 次超时（已收到：%q）", want, times, acc.String())
	return ""
}

// tryWriteFrame 与 writeFrame 相同，但把错误返回给调用方（用于「服务端可能已经断开」的用例）。
func (w *wsClient) tryWriteFrame(op byte, payload []byte) error {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		return net.ErrClosed
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(testTimeout))
	_, err := w.conn.Write(maskFrame(op, payload))
	return err
}

// closeCode 读关闭帧并返回关闭码（读不到则 t.Fatal）。
func (w *wsClient) closeCode(timeout time.Duration) uint16 {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		op, payload, err := w.readFrame(time.Until(deadline))
		if err != nil {
			w.t.Fatalf("等待关闭帧失败：%v", err)
		}
		if op == ws.OpClose {
			if len(payload) < 2 {
				return 0
			}
			return binary.BigEndian.Uint16(payload[:2])
		}
	}
	w.t.Fatalf("等待关闭帧超时")
	return 0
}

// requirePTY 让「必须真的分配 PTY」的用例在受限环境里明确跳过（见 testssh.PTYAvailable）。
func requirePTY(t *testing.T) {
	t.Helper()
	if ok, why := testssh.PTYAvailable(); !ok {
		t.Skip(why)
	}
}
