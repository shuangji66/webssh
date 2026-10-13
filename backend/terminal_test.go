package main

import (
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
	ws := a.dialWS(t, sid, "")
	tid := waitForTID(t, ws)
	ws.mustCollectUntil(shellPrompt, testTimeout)

	// 超过单帧上限（1 MiB）→ 服务端断开这个连接（读取时会按长度字段先拒绝，不分配内存），
	// 但终端不受影响。写这帧本身可能因服务端已断开而拿到 EPIPE，那是预期内的。
	big := strings.Repeat("A", maxFrameBytes+1024)
	_ = ws.tryWriteFrame(opText, []byte(big))
	closed := false
	for i := 0; i < 5 && !closed; i++ {
		op, _, err := ws.readFrame(2 * time.Second)
		if err != nil || op == opClose {
			closed = true
		}
	}
	if !closed {
		t.Fatal("超过单帧上限的连接没有被断开")
	}
	ws.close()

	// 终端还在（只解挂载）
	_, list := a.get(t, "/api/sessions")
	state, _ := listOf(list, "open")[0].(map[string]any)
	if n := len(listOf(state, "terminals")); n != 1 {
		t.Fatalf("超长帧不该杀掉终端：%v", state)
	}

	// 分片消息（text + continuation）要按整条消息处理，不能被截断。
	ws2 := a.dialWS(t, sid, tid)
	ws2.mustCollectUntil(oscReady, testTimeout)
	ws2.writeFragment(opText, []byte("\x1b]resize;7"), false)
	ws2.writeFragment(opCont, []byte("7;33\x07"), true)
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
