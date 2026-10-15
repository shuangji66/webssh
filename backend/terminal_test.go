package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"webssh/backend/internal/ws"
)

// tidPayloadRe 与前端 serverapi 的 TID_PAYLOAD_RE 一致（服务端下发的终端 id）。
var tidPayloadRe = regexp.MustCompile("\x1b\\]tid;([0-9a-zA-Z_-]+)\x07")

// waitForTID 读新建终端时下发的 `\x1b]tid;<id>\x07` 控制帧。
func waitForTID(t *testing.T, ws *wsClient) string {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		chunk, err := ws.readText(time.Until(deadline))
		if err != nil {
			t.Fatalf("等待终端 id 失败：%v（已收到：%q）", err, acc.String())
		}
		acc.WriteString(chunk)
		if m := tidPayloadRe.FindStringSubmatch(acc.String()); m != nil {
			return m[1]
		}
	}
	t.Fatalf("没有收到 \\x1b]tid;<id>\\x07 控制帧（收到：%q）", acc.String())
	return ""
}

// shellPrompt 是本测试服务器里交互式 bash 的提示符（`bash --norc -i`）。
const shellPrompt = "$ "

// TestTerminalCreateEchoAndResize 覆盖新建终端、PTY 收发、resize 与心跳控制帧。
func TestTerminalCreateEchoAndResize(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil(shellPrompt, testTimeout)

	// 命令回显 + 执行结果（这条链路把 WS 输入 → SSH stdin → PTY → bash → 输出 → 历史 → WS
	// 全部串起来了）。
	ws.writeText("echo marker-one-123\r")
	// 一次是 PTY 回显（用户看到的自己敲的字），一次是命令执行结果。
	ws.mustCollectCount("marker-one-123", 2, testTimeout)

	// 心跳控制帧不能进 PTY。
	ws.writeText("\x1b]ping\x07")
	time.Sleep(100 * time.Millisecond)
	ws.writeText("echo after-ping\r")
	got := ws.mustCollectUntil("after-ping", testTimeout)
	if strings.Contains(got, "]ping") || strings.Contains(got, "\x1b]") {
		t.Errorf("心跳帧被写进了 PTY：%q", got)
	}

	// resize → stty 里能看到新尺寸（证明真的调了 WindowChange 并被对端应用）
	ws.writeText("\x1b]resize;42;24\x07")
	ws.writeText("stty size\r")
	ws.mustCollectUntil("24 42", testTimeout)
	// 越界的尺寸要被忽略（70000 会截断成 4464，把 PTY 设成莫名其妙的尺寸）
	ws.writeText("\x1b]resize;70000;70000\x07")
	ws.writeText("stty size\r")
	ws.mustCollectUntil("24 42", testTimeout)

	// 状态里能看到这个终端
	_, list := a.get(t, "/api/sessions")
	state, _ := listOf(list, "open")[0].(map[string]any)
	terms := listOf(state, "terminals")
	if len(terms) != 1 {
		t.Fatalf("终端列表 = %v", state)
	}
	info, _ := terms[0].(map[string]any)
	if strOf(info, "id") != tid || info["exited"] != false {
		t.Errorf("TerminalInfo 不对：%v", info)
	}
	if _, err := time.Parse(time.RFC3339, strOf(info, "createdAt")); err != nil {
		t.Errorf("createdAt 不是 RFC3339：%v", info)
	}

	// 历史接口能读到内容
	out := a.ok(t, "/api/terminals/history", map[string]any{"sid": sid, "tid": tid})
	if !strings.Contains(strOf(out, "content"), "marker-one-123") {
		t.Errorf("历史里应当有刚才的输出：%v", snippetOf([]byte(strOf(out, "content"))))
	}
	if numOf(out, "size") <= 0 {
		t.Errorf("size = %v", out["size"])
	}
	// 清屏：历史被截断
	a.ok(t, "/api/terminals/clear", map[string]any{"sid": sid, "tid": tid})
	out = a.ok(t, "/api/terminals/history", map[string]any{"sid": sid, "tid": tid})
	if strOf(out, "content") != "" || numOf(out, "size") != 0 {
		t.Errorf("清屏后历史应为空：%v", out)
	}
	// 清屏后仍能继续收发（pump 没被破坏）
	ws.writeText("echo after-clear\r")
	ws.mustCollectUntil("after-clear", testTimeout)

	// 改名
	a.ok(t, "/api/terminals/rename", map[string]any{"sid": sid, "tid": tid, "title": "构建"})
	_, list = a.get(t, "/api/sessions")
	state, _ = listOf(list, "open")[0].(map[string]any)
	if title := strOf(listOf(state, "terminals")[0].(map[string]any), "title"); title != "构建" {
		t.Errorf("标题 = %q", title)
	}
	a.ok(t, "/api/terminals/rename", map[string]any{"sid": sid, "tid": tid, "title": ""})

	// 关闭终端：shell 被终止、终端从状态里消失，再挂载拿到 gone
	a.ok(t, "/api/terminals/close", map[string]any{"sid": sid, "tid": tid})
	_, list = a.get(t, "/api/sessions")
	state, _ = listOf(list, "open")[0].(map[string]any)
	if n := len(listOf(state, "terminals")); n != 0 {
		t.Errorf("关闭后终端列表应为空：%v", state)
	}
	ws2 := a.dialWS(t, sid, tid)
	ws2.mustCollectUntil(oscGone, testTimeout)
}

// TestTerminalReplayAndDetach 覆盖「浏览器断开只解挂载」+ 重新挂载回放历史 + 单挂载点顶号。
func TestTerminalReplayAndDetach(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil(shellPrompt, testTimeout)
	ws.writeText("echo before-detach\r")
	ws.mustCollectUntil("before-detach", testTimeout)

	// 浏览器断开：只解挂载，shell 继续跑、输出继续写历史。
	ws.close()
	time.Sleep(300 * time.Millisecond)
	_, list := a.get(t, "/api/sessions")
	state, _ := listOf(list, "open")[0].(map[string]any)
	if n := len(listOf(state, "terminals")); n != 1 {
		t.Fatalf("断开连接不该杀掉终端：%v", state)
	}

	// 重新挂载：回放历史（含断开期间的输出）+ ready
	ws2 := a.dialWS(t, sid, tid)
	replay := ws2.mustCollectUntil(oscReady, testTimeout)
	if !strings.Contains(replay, "before-detach") {
		t.Errorf("重挂载没有回放历史：%q", replay)
	}
	ws2.writeText("echo after-reattach\r")
	ws2.mustCollectUntil("after-reattach", testTimeout)

	// 单挂载点：第三个连接挂上来，第二个必须收到 detached + close 4001
	ws3 := a.dialWS(t, sid, tid)
	ws3.mustCollectUntil(oscReady, testTimeout)
	detached, ok := ws2.collectUntil(oscDetached, testTimeout)
	if !ok {
		t.Fatalf("被顶掉的连接没有收到 %q（收到：%q）", oscDetached, detached)
	}
	if code := ws2.closeCode(testTimeout); code != wsCloseDetached {
		t.Fatalf("被顶掉的连接关闭码 = %d，期望 %d", code, wsCloseDetached)
	}
	// 新端仍是操作端：能改尺寸（stty 验证）
	ws3.writeText("echo owner-input\r")
	ws3.mustCollectUntil("owner-input", testTimeout)
	ws3.writeText("\x1b]resize;30;12\x07")
	ws3.writeText("stty size\r")
	ws3.mustCollectUntil("12 30", testTimeout)
}

// TestTerminalGoneFrames 覆盖「终端/会话不存在」必须带 gone 控制帧。
func TestTerminalGoneFrames(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 终端 id 不存在
	ws := a.dialWS(t, sid, "0123456789abcdef0123456789abcdef")
	got := ws.mustCollectUntil(oscGone, testTimeout)
	if !strings.Contains(got, "终端不存在") {
		t.Errorf("gone 之前应当有可读的提示：%q", got)
	}
	_ = ws.closeCode(testTimeout)

	// 会话不存在（后端重启后前端可能拿到旧 sid）
	ws2 := a.dialWS(t, "0123456789abcdef0123456789abcdef", "")
	ws2.mustCollectUntil(oscGone, testTimeout)

	// 会话没打开
	notOpen := a.ok(t, "/api/sessions/save", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo-notopened", "authType": "password", "password": "demo",
	})
	ws3 := a.dialWS(t, strOf(notOpen["session"], "id"), "")
	got3 := ws3.mustCollectUntil(oscGone, testTimeout)
	if !strings.Contains(got3, "尚未打开") {
		t.Errorf("会话未打开时应当明确提示：%q", got3)
	}
}

// TestTerminalShellExitAndReap 覆盖 shell 退出 → exit 控制帧 → 宽限期后回收。
func TestTerminalShellExitAndReap(t *testing.T) {
	requirePTY(t)
	// 把宽限期缩到毫秒级（否则要等 10 分钟）。测试串行跑，改完恢复。
	oldGrace := exitedGrace()
	setExitedGrace(300 * time.Millisecond)
	t.Cleanup(func() { setExitedGrace(oldGrace) })

	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil(shellPrompt, testTimeout)

	// `exit` 让 shell 退出
	ws.writeText("exit\r")
	got := ws.mustCollectUntil(oscExit, testTimeout)
	if !strings.Contains(got, "[process exited]") {
		t.Errorf("退出提示缺失：%q", got)
	}

	// 退出后状态里 exited=true
	_, list := a.get(t, "/api/sessions")
	state, _ := listOf(list, "open")[0].(map[string]any)
	terms := listOf(state, "terminals")
	if len(terms) != 1 || terms[0].(map[string]any)["exited"] != true {
		t.Fatalf("退出后 TerminalInfo.exited 应为 true：%v", state)
	}

	// 宽限期内还能挂载回放（并且补发 exit 控制帧）
	ws2 := a.dialWS(t, sid, tid)
	replay := ws2.mustCollectUntil(oscReady, testTimeout)
	if !strings.Contains(replay, "[process exited]") {
		t.Errorf("退出后的回放应当包含退出提示：%q", replay)
	}
	ws2.mustCollectUntil(oscExit, testTimeout)

	// 宽限期后回收：终端消失、再挂载拿到 gone
	time.Sleep(600 * time.Millisecond)
	_, list = a.get(t, "/api/sessions")
	state, _ = listOf(list, "open")[0].(map[string]any)
	if n := len(listOf(state, "terminals")); n != 0 {
		t.Fatalf("宽限期后终端应当被回收：%v", state)
	}
	ws3 := a.dialWS(t, sid, tid)
	ws3.mustCollectUntil(oscGone, testTimeout)
	// 历史目录里该终端的文件也删掉了
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "history", sid, tid+".log")); !os.IsNotExist(err) {
		t.Errorf("回收后历史文件应当删除：%v", err)
	}
}

// TestTerminalFrameLimits 覆盖单帧上限与分片累积（不能一条消息打爆进程）。
func TestTerminalFrameLimits(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	conn := a.dialWS(t, sid, "")
	tid := waitForTID(t, conn)
	conn.mustCollectUntil(shellPrompt, testTimeout)

	// 超过单帧上限（1 MiB）→ 服务端断开这个连接（读取时会按长度字段先拒绝，不分配内存），
	// 但终端不受影响。写这帧本身可能因服务端已断开而拿到 EPIPE，那是预期内的。
	big := strings.Repeat("A", ws.MaxFrameBytes+1024)
	_ = conn.tryWriteFrame(ws.OpText, []byte(big))
	closed := false
	for i := 0; i < 5 && !closed; i++ {
		op, _, err := conn.readFrame(2 * time.Second)
		if err != nil || op == ws.OpClose {
			closed = true
		}
	}
	if !closed {
		t.Fatal("超过单帧上限的连接没有被断开")
	}
	conn.close()

	// 终端还在（只解挂载）
	_, list := a.get(t, "/api/sessions")
	state, _ := listOf(list, "open")[0].(map[string]any)
	if n := len(listOf(state, "terminals")); n != 1 {
		t.Fatalf("超长帧不该杀掉终端：%v", state)
	}

	// 分片消息（text + continuation）要按整条消息处理，不能被截断。
	ws2 := a.dialWS(t, sid, tid)
	ws2.mustCollectUntil(oscReady, testTimeout)
	ws2.writeFragment(ws.OpText, []byte("\x1b]resize;7"), false)
	ws2.writeFragment(ws.OpContinuation, []byte("7;33\x07"), true)
	ws2.writeText("stty size\r")
	ws2.mustCollectUntil("33 77", testTimeout)
}

// TestTerminalHistorySizeCap 校验历史读取上限（避免把无限增长的日志塞进一次响应）。
func TestTerminalHistorySizeCap(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil(shellPrompt, testTimeout)

	// 往历史文件里塞超过上限的内容（直接写文件最快，等价于跑了很久的终端）。
	histPath := filepath.Join(a.renv.DataDir, "history", sid, tid+".log")
	f, err := os.OpenFile(histPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开历史文件失败：%v", err)
	}
	if _, err := f.Write([]byte(strings.Repeat("x", maxHistoryBytes+4096))); err != nil {
		t.Fatalf("写历史失败：%v", err)
	}
	f.Close()

	out := a.ok(t, "/api/terminals/history", map[string]any{"sid": sid, "tid": tid})
	if size := numOf(out, "size"); int(size) != maxHistoryBytes {
		t.Errorf("历史读取上限没生效：size = %v，期望 %d", size, maxHistoryBytes)
	}
}

// TestTerminalWSRequiresUpgrade 校验非 WebSocket 请求被明确拒绝。
func TestTerminalWSRequiresUpgrade(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	res, out := a.post(t, "/api/terminal/ws?sid="+sid, nil)
	if res.StatusCode != http.StatusBadRequest || out["code"] != "invalid" {
		t.Fatalf("非 WebSocket 请求响应 = %d %v", res.StatusCode, out)
	}
}

// TestTerminalAPINotFound 校验终端类接口对不存在的对象回明确的错误码。
func TestTerminalAPINotFound(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	for _, p := range []string{"/api/terminals/history", "/api/terminals/clear", "/api/terminals/close"} {
		a.fail(t, p, map[string]any{"sid": sid, "tid": "ffffffffffffffffffffffffffffffff"}, "not_found", http.StatusNotFound)
	}
	a.fail(t, "/api/terminals/history", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "tid": "ffffffffffffffffffffffffffffffff"}, "session_gone", http.StatusNotFound)
}

// TestTerminalWriteInputDropsNonOwner 是「单挂载点」的底层断言。
//
// WS 层很难稳定复现「被顶掉的连接还有残留帧正在路上」（旧连接已经被关掉了），
// 所以这一条在 server 层直接验：非操作端的输入必须被丢弃、操作端的照常生效。
func TestTerminalWriteInputDropsNonOwner(t *testing.T) {
	requirePTY(t)
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("取会话运行时失败：%v", apiErr)
	}
	term, apiErr := rt.newTerminal(context.Background(), 80, 24)
	if apiErr != nil {
		t.Fatalf("新建终端失败：%v", apiErr)
	}
	defer term.close()

	owner, ownerPeer := net.Pipe()
	defer owner.Close()
	defer ownerPeer.Close()
	go func() { _, _ = io.Copy(io.Discard, ownerPeer) }()
	intruder, intruderPeer := net.Pipe()
	defer intruder.Close()
	defer intruderPeer.Close()
	if _, err := term.attach(owner); err != nil {
		t.Fatalf("挂载失败：%v", err)
	}
	if err := term.writeInput(intruder, []byte("echo ghost-marker\r")); !errors.Is(err, errNotOwner) {
		t.Fatalf("非操作端的输入应当被丢弃（errNotOwner），实际：%v", err)
	}
	if err := term.resize(intruder, 100, 50); !errors.Is(err, errNotOwner) {
		t.Fatalf("非操作端不该能改尺寸，实际：%v", err)
	}
	if err := term.writeInput(owner, []byte("echo owner-marker\r")); err != nil {
		t.Fatalf("操作端的输入应当成功：%v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := term.history()
		if err != nil {
			t.Fatalf("读取历史失败：%v", err)
		}
		if strings.Contains(string(data), "owner-marker") {
			if strings.Contains(string(data), "ghost-marker") {
				t.Fatalf("非操作端的输入进了 PTY：%q", snippetOf(data))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("操作端的输入没有到达 PTY（没等到 owner-marker）")
}

// ---------------------------------------------------------------------------
// 回放分帧的两条不变量（都不依赖 PTY：直接构造带历史文件的 terminal）
// ---------------------------------------------------------------------------

// newHistTerminal 造一个只有历史文件的终端（不建 SSH 连接，所以任何环境都能跑）。
func newHistTerminal(t *testing.T, body []byte) *terminal {
	t.Helper()
	histPath := filepath.Join(t.TempDir(), "hist.log")
	if err := os.WriteFile(histPath, body, 0o600); err != nil {
		t.Fatalf("写历史文件失败：%v", err)
	}
	// 与生产一致：O_RDWR|O_APPEND（只写打开会让回放 EBADF）
	f, err := os.OpenFile(histPath, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开历史文件失败：%v", err)
	}
	t.Cleanup(func() { f.Close() })
	return &terminal{id: "hist-only", hist: f, exitedCh: make(chan struct{})}
}

// TestTerminalReplayFramesAreBinaryAndByteExact 钉住「终端数据用二进制帧」+「回放不丢/不重字节」。
//
// 为什么必须有这条：数据帧曾经是文本帧，而回放按 32 KiB 的**字节**边界切帧（实时广播按
// 8 KiB 同理），边界会落在多字节字符中间；WebSocket 文本帧的载荷按 RFC 6455 §8.1 必须是
// 合法 UTF-8，浏览器收到非法文本帧会**直接断开连接**（Chromium 实测：onerror + close 1006，
// 一帧数据都收不到）。症状是「刷新后终端回放到出问题的那一帧就断，且每次都在同一位置断」。
//
// 这条用例不需要 PTY，所以在没有 /dev/ptmx 的环境（CI 容器）里也会真的跑。
func TestTerminalReplayFramesAreBinaryAndByteExact(t *testing.T) {
	// 32767 个 ASCII + 汉字 + 两个非 UTF-8 字节：第 32768 个字节正好是「中」的第一个字节，
	// 旧实现下第一帧就是一帧非法 UTF-8；末尾的 0xff 0xfe 是 `cat` 二进制文件那种形态。
	body := append(bytes.Repeat([]byte("a"), wsChunkSize-1), []byte("中文测试中文测试")...)
	body = append(body, 0xff, 0xfe, '\n')

	term := newHistTerminal(t, body)
	c1, c2 := net.Pipe()
	defer c2.Close()
	go func() { _, _ = term.attach(c1) }()

	var replayed bytes.Buffer
	frames := 0
	for {
		_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
		op, _, payload, err := ws.ReadFrame(c2)
		if err != nil {
			t.Fatalf("读第 %d 帧失败：%v", frames, err)
		}
		if op != ws.OpBinary {
			t.Fatalf("终端数据帧必须是二进制帧（OpBinary），第 %d 帧的 opcode = 0x%x"+
				"（改回文本帧会让含中文的终端在重挂载时被浏览器断连）", frames, op)
		}
		frames++
		if string(payload) == oscReady {
			break
		}
		replayed.Write(payload)
	}
	if frames < 2 {
		t.Fatalf("回放只有 %d 帧，用例前提不成立（应当被 32 KiB 边界切开）", frames)
	}
	if !bytes.Equal(replayed.Bytes(), body) {
		t.Fatalf("回放字节与历史文件不一致：收到 %d 字节、文件 %d 字节", replayed.Len(), len(body))
	}
}

// TestTerminalReplayTimeoutFreesHistMu 钉住「整段回放有总时限」。
//
// 每帧的写超时是 ws.WriteTimeout（5s），4 MiB 历史按 32 KiB 分帧有 128 帧：一个连上就
// 不再读数据的客户端，最坏能让 attach 在十几分钟里一直持有 histMu（同终端的实时广播、
// /api/terminals/history、清屏、另一台设备的顶号通知全部排队）。
func TestTerminalReplayTimeoutFreesHistMu(t *testing.T) {
	old := replayTimeout()
	setReplayTimeout(200 * time.Millisecond)
	t.Cleanup(func() { setReplayTimeout(old) })

	term := newHistTerminal(t, bytes.Repeat([]byte("x"), maxHistoryBytes))
	c1, c2 := net.Pipe()
	defer c2.Close() // 刻意不读：写一定阻塞

	done := make(chan error, 1)
	go func() {
		_, err := term.attach(c1)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("对端不读数据时，回放应当以超时结束")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("回放没有在总时限内结束（histMu 被慢客户端占住了）")
	}

	// 锁必须已经释放：history() 拿不到锁就会一直等（这就是用户看到的「历史接口一起挂死」）。
	histDone := make(chan struct{})
	go func() {
		_, _ = term.history()
		close(histDone)
	}()
	select {
	case <-histDone:
	case <-time.After(2 * time.Second):
		t.Fatal("回放超时后 histMu 没有释放，history() 被堵住")
	}
}

// TestOriginAllowed 钉住 WebSocket 握手的 Origin 判据（防 CSWSH）。
//
// 规则两条：**Origin 里的主机名必须等于本次请求的 Host**；**端口只在 Host 这一侧带了端口时
// 才要求相等**——`Host: 192.168.5.222` + `Origin: http://192.168.5.222:5666` 放行，因为飞牛
// 统一网关的 `proxy_set_header Host $host;` 给的就是不带端口的主机名（这条链路上曾经整片 403，
// 现场见 CONTRACT 第 31 节）。反代改写 Host 时接受它带过来的 X-Forwarded-Host。
// 没有 Origin 的请求走的是另一条分支（调用方只在 Origin 非空时问这里，curl/websocat 不带它）。
func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		host    string
		fwdHost string
		want    bool
	}{
		{"同主机同端口", "http://nas:8080", "nas:8080", "", true},
		{"https 缺端口 = 443", "https://nas", "nas", "", true},
		{"http 缺端口 = 80", "http://nas", "nas:80", "", true},
		{"大小写不敏感", "http://NAS:8080", "nas:8080", "", true},
		{"端口不同", "http://nas:8080", "nas:9090", "", false},
		{"主机不同", "http://evil.example", "nas:8080", "", false},
		// 飞牛网关丢端口后的现场：192.168.5.222:5666 是 http、:5667 是 https。
		{"反代丢掉了端口（飞牛网关 http）", "http://192.168.5.222:5666", "192.168.5.222", "", true},
		{"反代丢掉了端口（飞牛网关 https）", "https://192.168.5.222:5667", "192.168.5.222", "", true},
		{"反代丢掉了端口但主机不同", "http://evil.example:5666", "192.168.5.222", "", false},
		{"反代改写了 Host，但转发了原 Host", "https://nas", "webssh-internal:80", "nas", true},
		{"X-Forwarded-Host 带了端口并匹配", "https://nas:8443", "internal", "nas:8443", true},
		{"X-Forwarded-Host 丢了端口", "https://nas:8443", "internal", "nas", true},
		{"X-Forwarded-Host 是列表", "https://nas", "internal", "a.example, nas", true},
		{"X-Forwarded-Host 也不匹配", "https://evil.example", "internal", "nas", false},
		{"IPv6 同端口", "http://[::1]:8080", "[::1]:8080", "", true},
		{"IPv6 的 Host 丢了端口", "http://[::1]:8080", "[::1]", "", true},
		{"Origin 不是合法 URL", "null", "nas", "", false},
		{"Origin 没有主机", "file:///etc/passwd", "nas", "", false},
	}
	for _, tc := range cases {
		r := &http.Request{Host: tc.host, Header: http.Header{}}
		if tc.fwdHost != "" {
			r.Header.Set("X-Forwarded-Host", tc.fwdHost)
		}
		if got := originAllowed(tc.origin, r); got != tc.want {
			t.Errorf("%s：originAllowed(%q, Host=%q, XFH=%q) = %v，期望 %v",
				tc.name, tc.origin, tc.host, tc.fwdHost, got, tc.want)
		}
	}
}

// TestTerminalWSRejectsForeignOrigin 覆盖端到端那一条：跨站 Origin 的握手必须被拒（403），
// 而且是在 hijack **之前**就拒掉（不建立 WebSocket 连接、不碰任何会话状态）；
// 同一份请求把 Origin 换成自己的 Host 时不该被这一道挡住（走的是后面「会话不存在」那条路）。
// 另外钉住飞牛网关那一条：反代把 Host 的端口丢掉时（`Host: webssh` + `Origin:
// http://webssh:5666`）必须放行——这条曾经让整条链路上的终端 WS 全部 403。
func TestTerminalWSRejectsForeignOrigin(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	headers := func(origin string) map[string]string {
		return map[string]string{
			"Connection":            "Upgrade",
			"Upgrade":               "websocket",
			"Sec-WebSocket-Key":     "dGhlIHNhbXBsZSBub25jZQ==",
			"Sec-WebSocket-Version": "13",
			"Origin":                origin,
		}
	}
	// 客户端连的是 http://webssh/...（见 harness 的 a.do），所以 Host = webssh（不带端口）
	res, raw := a.do(t, http.MethodGet, "/api/terminal/ws?sid=whatever&tid=whatever", nil, headers("http://evil.example"))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站 Origin 的握手状态码 = %d，期望 403（%s）", res.StatusCode, snippetOf(raw))
	}
	res2, raw2 := a.do(t, http.MethodGet, "/api/terminal/ws?sid=whatever&tid=whatever", nil, headers("http://webssh"))
	if res2.StatusCode == http.StatusForbidden {
		t.Fatalf("同源 Origin 的握手被误拒：%d（%s）", res2.StatusCode, snippetOf(raw2))
	}
	// 反代（飞牛网关 `proxy_set_header Host $host;`）丢掉了端口：Origin 里带着外部端口，
	// Host 里没有。这一条曾经 403。
	res3, raw3 := a.do(t, http.MethodGet, "/api/terminal/ws?sid=whatever&tid=whatever", nil, headers("http://webssh:5666"))
	if res3.StatusCode == http.StatusForbidden {
		t.Fatalf("Host 被反代去掉端口后握手被误拒：%d（%s）", res3.StatusCode, snippetOf(raw3))
	}
	// 丢端口的口子只对「同一主机名」开：别的域名仍然拒。
	res4, raw4 := a.do(t, http.MethodGet, "/api/terminal/ws?sid=whatever&tid=whatever", nil, headers("http://evil.example:5666"))
	if res4.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站 Origin（别的域名 + 端口）状态码 = %d，期望 403（%s）", res4.StatusCode, snippetOf(raw4))
	}
}
