package main

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
	"webssh/backend/internal/store"
	"webssh/backend/internal/ws"
)

// 下面这对是「shell 已退出」的终端在服务端的保留时长（契约第 4.4 节）：这段时间内挂载
// 仍能回放历史，到期自动回收（PTY 已死，输入只会得到 EIO）。
//
// 用 atomic 存纳秒而不是一个普通变量：它会被每个终端的回收协程读，而测试要把它缩到
// 毫秒级（否则验证「回收后重新挂载拿到 gone」要等 10 分钟）。回收协程可能比测试活得久，
// 普通变量会让 `go test -race` 报数据竞争。
var exitedGraceNanos atomic.Int64

func init() { exitedGraceNanos.Store(int64(10 * time.Minute)) }

// exitedGrace 返回当前的保留时长。
func exitedGrace() time.Duration { return time.Duration(exitedGraceNanos.Load()) }

// setExitedGrace 只给测试用：缩短保留时长，避免等 10 分钟。
func setExitedGrace(d time.Duration) { exitedGraceNanos.Store(int64(d)) }

// replayTimeoutNanos 是一整段历史回放的总时限（默认 30s）。
//
// 为什么需要「总」时限：每帧的写超时是 ws.WriteTimeout（5s），而 4 MiB 历史按 32 KiB
// 分帧有 128 帧——一个连上就不再读数据的客户端最坏能让 attach 在 128 × 5s ≈ 10 分钟里
// 一直持有 histMu，期间同终端的实时广播（pump）、/api/terminals/history、清屏、
// 以及另一台设备的顶号通知（kickDetached）全部排队。
//
// 正常回放 4 MiB 是毫秒级（本地 socket + 浏览器读得很快），30s 只在「对端根本不读」
// 时才生效；超时即返回错误 → handler 关掉这条连接并解挂载。
//
// 用 atomic 存纳秒的理由与 exitedGraceNanos 相同：测试要把它缩到毫秒级，而回收协程
// 可能比测试活得久，普通变量会被 -race 判成数据竞争。
var replayTimeoutNanos atomic.Int64

func init() { replayTimeoutNanos.Store(int64(30 * time.Second)) }

// replayTimeout 返回当前的回放总时限。
func replayTimeout() time.Duration { return time.Duration(replayTimeoutNanos.Load()) }

// setReplayTimeout 只给测试用：把总时限缩到毫秒级，避免用例等 30 秒。
func setReplayTimeout(d time.Duration) { replayTimeoutNanos.Store(int64(d)) }

// 新终端建立时的默认 PTY 尺寸。前端拿到 ready 后会按 xterm 的实际尺寸发一次
// `\x1b]resize;…`（握手 URL 里没有尺寸参数，这也是 xterm.js fit 的职责）。
const (
	defaultTermCols = 80
	defaultTermRows = 24
)

// errNotOwner：请求来自已不是操作端的连接（已被其他设备顶掉的旧连接）。
// 上层静默忽略：那只是残留帧，不是错误。
var errNotOwner = errors.New("not session owner")

// terminal 是一个终端：一条独立的 SSH 连接 + 一个 PTY shell + 一个历史文件。
//
// 单挂载点语义：同一时刻只允许一个操作端挂载（能写输入、能改尺寸）。浏览器断开只
// **解挂载**不杀终端——shell 继续跑、输出继续写历史文件，刷新后可重新挂载并回放。
type terminal struct {
	id      string
	runtime *sessionRuntime
	client  *ssh.Client
	sess    *ssh.Session
	stdin   io.WriteCloser
	out     io.ReadCloser
	// outWriter 是 out 的写端（x/crypto 把 PTY 输出写进来）；会话结束时由 watchExit 关闭，
	// 这样 pump 才能从 Read 上醒来（见 start 里的说明）。
	outWriter *io.PipeWriter
	hist      *os.File
	createdAt time.Time

	// histMu 串行化「追加历史 + 向已挂载连接广播」：历史回放与实时追加必须在同一把
	// 锁内完成，否则字节会重复（回放期间的新输出被广播一次、又被回放一次）或丢失。
	histMu sync.Mutex
	connMu sync.Mutex
	conn   net.Conn

	mu        sync.Mutex
	title     string
	cols      uint16
	rows      uint16
	exited    bool
	closed    bool
	closeOnce sync.Once

	// exitedCh 在 pump 结束时关闭（shell 退出、连接断开、终端被关闭都会走到这里），
	// 供到期回收协程等待。
	exitedCh chan struct{}
}

// start 请求 PTY 并启动远程 shell。
func (t *terminal) start(cols, rows uint16) *apierr.Error {
	// TERM 必须是 xterm-256color：前端是 xterm.js，远端按它决定能力集（颜色 / 括号粘贴 /
	// 光标形状）。设成 vt100 或空会让界面变成 8 色、方向键也不对。
	modes := ssh.TerminalModes{
		ssh.ECHO:          1, // 远端回显：本地不回显（否则会出现双份字符）
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := t.sess.RequestPty("xterm-256color", int(rows), int(cols), modes); err != nil {
		return apierr.ShellFailed("远端拒绝分配 PTY：%v", err)
	}
	stdin, err := t.sess.StdinPipe()
	if err != nil {
		return apierr.ShellFailed("无法创建 stdin 管道：%v", err)
	}
	// stdout 与 stderr 接到**同一个** pipe：带 PTY 时远端一般已把 stderr 合并进 stdout，
	// 但扩展数据（以及没开 PTY 的远端实现）仍可能走 stderr 通道；分两条流收到一起会让
	// 输出顺序错乱，日志与提示符交叉在一起。
	//
	// 这里必须自己握着写端：x/crypto 只会往 sess.Stdout 里写，**不会关闭它**
	// （它内部的 io.Copy 遇到 EOF 直接返回）。如果拿 io.PipeWriter 当 Stdout 又不管它，
	// pump 会永远阻塞在 Read 上——症状是「shell 退出后既看不到 [process exited]，
	// 终端也永不被回收」，而且连接一直挂着。见 watchExit。
	pr, pw := io.Pipe()
	t.sess.Stdout = pw
	t.sess.Stderr = pw
	t.stdin = stdin
	t.out = pr
	if err := t.sess.Shell(); err != nil {
		return apierr.ShellFailed("无法启动远程 shell：%v", err)
	}
	go t.watchExit(pw)
	return nil
}

// watchExit 等会话结束（对端发了 exit-status 并关闭通道，或连接断开），把输出管道的写端关掉。
//
// x/crypto 的 Session.Wait 返回错误是**正常**的：shell 非零退出是 *ExitError、
// 对端没回退出状态是 *ExitMissingError、进程被我们杀掉是连接错误。契约第 5.4 节明确
// 「关闭过程中的 EOF / ErrClosed 一律视为正常终止」，所以这里只在真正意外时报一条日志。
func (t *terminal) watchExit(out *io.PipeWriter) {
	if err := t.sess.Wait(); err != nil && !isNormalSessionEnd(err) {
		logx.Warnf("终端 %s 的 SSH 会话异常结束：%v", t.id, err)
	}
	_ = out.Close()
}

// isNormalSessionEnd 判断「会话结束」的错误是否属于预期内。
func isNormalSessionEnd(err error) bool {
	var exitErr *ssh.ExitError
	var missing *ssh.ExitMissingError
	return errors.As(err, &exitErr) || errors.As(err, &missing) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)
}

// pump 持续读取 shell 输出：追加到历史文件，并向已挂载的连接实时广播。
// 同一把 histMu 保证「文件」与「连接」两路顺序一致。
//
// 帧边界就是这里 8 KiB 读缓冲的边界（也就是远端一次输出的字节数），**可能切在多字节
// UTF-8 字符中间**；所以数据帧必须是二进制帧（见 protocol.go 的 ⚠️），前端用流式
// TextDecoder 把跨帧的半个字符拼回来。改回文本帧会让「cat 中文文件」这类输出直接断连。
func (t *terminal) pump() {
	buf := make([]byte, 8192)
	for {
		n, err := t.out.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			t.histMu.Lock()
			if !t.isClosed() {
				if t.hist != nil {
					_, _ = t.hist.Write(chunk)
				}
				if c := t.mounted(); c != nil {
					t.writeConn(c, ws.Frame(termDataOp, chunk))
				}
			}
			t.histMu.Unlock()
		}
		if err != nil {
			break
		}
	}

	t.mu.Lock()
	t.exited = true
	closed := t.closed
	t.mu.Unlock()

	// shell 退出后补一条提示与退出控制帧（前端只认 \x1b]exit\x07，见 ws.go 的 oscExit）。
	// 提示文本同时写进历史：重新挂载的客户端能看到「这里退出过」。
	t.histMu.Lock()
	if !closed {
		note := []byte("\r\n\x1b[31m[process exited]\x1b[0m\r\n")
		if t.hist != nil {
			_, _ = t.hist.Write(note)
		}
		if c := t.mounted(); c != nil {
			t.writeConn(c, ws.Frame(termDataOp, note))
			t.writeConn(c, ws.Frame(termDataOp, []byte(oscExit)))
		}
	}
	t.histMu.Unlock()
	close(t.exitedCh)
}

// mounted 取出当前挂载的连接（调用方需持有 histMu，见 writeConn 的说明）。
func (t *terminal) mounted() net.Conn {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	return t.conn
}

// isOwner 判断该连接是否仍是操作端（被顶掉的旧连接为 false）。
func (t *terminal) isOwner(c net.Conn) bool {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	return t.conn != nil && t.conn == c
}

// writeConn 向已挂载连接写一帧，带写超时（见 ws.WriteTimeout）。调用方必须持有 histMu。
//
// 超时/出错说明该端已不读数据：解除挂载 + 关掉连接（让对端若还活着就立刻看到「连接已
// 关闭」，而不是停在「看着连着、其实没输出」），绝不让一次失联的写把 histMu 长期占住——
// 否则同终端的挂载与 history 接口会被一起堵死。
func (t *terminal) writeConn(c net.Conn, frame []byte) {
	if err := ws.WriteFramed(c, frame); err != nil {
		t.detach(c)
		_ = c.Close()
	}
}

// writeFramedBefore 写一帧，写超时取「单帧超时」与「整段回放的总时限」里更早的那个。
//
// 回放循环用它而不是 ws.WriteFramed：后者每次都把超时重置成 now+5s，帧多了就没有
// 总上限（见 replayTimeout 的说明）。
func writeFramedBefore(c net.Conn, frame []byte, deadline time.Time) error {
	d := time.Now().Add(ws.WriteTimeout)
	if deadline.Before(d) {
		d = deadline
	}
	return ws.WriteFramedDeadline(c, frame, d)
}

// attach 把连接挂到终端上：先回放历史（分帧 ≤32 KiB，最多 4 MiB），再发 `\x1b]ready\x07`，
// 然后接管实时流。整段在 histMu 内完成，保证不重不漏。
//
// 回放的分帧边界是 32 KiB 的字节边界，**会切在多字节字符中间**——所以数据帧是二进制帧
// （见 protocol.go 的 ⚠️），前端流式解码。历史 > 4 MiB 时起点还要落在 size-maxHistoryBytes，
// 同样可能是半个字符，二进制帧同样不受影响。
//
// 返回**被顶掉的旧连接**（无则 nil），由调用方负责通知并关闭（kickDetached）——
// 单挂载点只影响这一个终端，其他终端与其他会话的连接不动。
func (t *terminal) attach(c net.Conn) (net.Conn, error) {
	t.histMu.Lock()
	defer t.histMu.Unlock()

	// 整段回放的总时限（见 replayTimeout 的说明）：没有它，128 帧 × 每帧 5s 的写超时
	// 最坏能把 histMu 占十几分钟，pump / history / kickDetached 全部排队。
	deadline := time.Now().Add(replayTimeout())

	if t.hist != nil {
		if size, err := t.hist.Seek(0, io.SeekEnd); err == nil {
			start := int64(0)
			if size > maxHistoryBytes {
				start = size - maxHistoryBytes
			}
			if _, err := t.hist.Seek(start, io.SeekStart); err == nil {
				chunk := make([]byte, wsChunkSize)
				for {
					n, rerr := t.hist.Read(chunk)
					if n > 0 {
						// 回放同样带写超时：新端不读数据时不能让 histMu 一直被占着。
						if werr := writeFramedBefore(c, ws.Frame(termDataOp, chunk[:n]), deadline); werr != nil {
							return nil, werr
						}
					}
					if rerr != nil {
						break
					}
				}
			}
		}
	}
	if err := writeFramedBefore(c, ws.Frame(termDataOp, []byte(oscReady)), deadline); err != nil {
		return nil, err
	}
	// 已经退出的终端：历史里只有「[process exited]」这段文本，控制帧没写进历史文件。
	// 不补一帧 oscExit 的话，重新挂载的客户端会以为 shell 还活着（看不到退出提示）。
	if t.isExited() {
		if err := writeFramedBefore(c, ws.Frame(termDataOp, []byte(oscExit)), deadline); err != nil {
			return nil, err
		}
	}

	t.connMu.Lock()
	prev := t.conn
	t.conn = c
	t.connMu.Unlock()
	if prev == c {
		prev = nil // 重复挂载同一连接：无需顶号
	}
	return prev, nil
}

// detach 解除挂载（不杀终端：shell 继续跑、输出继续写历史文件）。
func (t *terminal) detach(c net.Conn) {
	t.connMu.Lock()
	if t.conn == c {
		t.conn = nil
	}
	t.connMu.Unlock()
}

// kickDetached 通知被顶掉的旧连接：先发 `\x1b]detached\x07`，再发 close(4001)，最后关闭。
//
// 写带 1s 超时：旧连接的内核发送缓冲可能已满，绝不能让这次写阻塞拖住新设备的挂载。
// 持 histMu 是为了不与 pump 的广播对**同一个连接**并发写（两处 write 交错会破坏帧结构）。
func (t *terminal) kickDetached(c net.Conn) {
	t.histMu.Lock()
	defer t.histMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.Write(ws.Frame(termDataOp, []byte(oscDetached)))
	_, _ = c.Write(ws.CloseFrame(wsCloseDetached, "detached"))
	_ = c.Close()
}

// writeInput 把客户端输入写进远程 PTY。**只有当前操作端能写**：被顶掉的旧连接即使还有
// 残留帧抵达也在服务端丢弃——否则两台设备会同时操作同一个 shell（两边都能打字，
// 却只有一边看得到输出）。
func (t *terminal) writeInput(c net.Conn, p []byte) error {
	if !t.isOwner(c) {
		return errNotOwner
	}
	if t.isClosed() || t.isExited() {
		return errNotOwner
	}
	_, err := t.stdin.Write(p)
	return err
}

// resize 调整远端 PTY 尺寸（同样只接受操作端：被顶掉的旧设备随后会触发一次重排，
// 不加限制就会把 PTY 改成它自己的窗口大小）。
func (t *terminal) resize(c net.Conn, cols, rows uint16) error {
	if !t.isOwner(c) {
		return errNotOwner
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.mu.Unlock()
	// x/crypto 的签名是 (h, w)，即先 rows 后 cols（与前端控制帧的 cols;rows 顺序相反）。
	return t.sess.WindowChange(int(rows), int(cols))
}

// history 读取历史文件内容（上限 maxHistoryBytes，取末尾部分）。
func (t *terminal) history() ([]byte, error) {
	t.histMu.Lock()
	defer t.histMu.Unlock()
	if t.hist == nil {
		return []byte{}, nil
	}
	size, err := t.hist.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if size > maxHistoryBytes {
		start = size - maxHistoryBytes
	}
	if _, err := t.hist.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(t.hist)
}

// clearHistory 清空历史文件（前端「清屏」：重连 / 刷新后不再回放旧内容）。
// pump 仍在运行，截断后新输出从 0 继续追加。
func (t *terminal) clearHistory() error {
	t.histMu.Lock()
	defer t.histMu.Unlock()
	if t.hist == nil {
		return nil
	}
	if _, err := t.hist.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return t.hist.Truncate(0)
}

func (t *terminal) setTitle(title string) {
	t.mu.Lock()
	t.title = strings.TrimSpace(title)
	t.mu.Unlock()
}

func (t *terminal) info() terminalInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return terminalInfo{
		ID:        t.id,
		Title:     t.title,
		CreatedAt: t.createdAt.Format(time.RFC3339),
		Exited:    t.exited,
	}
}

func (t *terminal) isExited() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exited
}

func (t *terminal) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// close 终止 shell 并清理资源。
//
// 关闭顺序（契约第 5.4 节）：ssh.Session → ssh.Client → 历史文件（并删除）。
// 关闭过程中的 EOF / net.ErrClosed / 「进程被信号杀掉」一律是**正常终止**，不上抛错误——
// 参考实现曾把 Kill 导致 Wait 返回非零当失败，结果 DELETE 对运行中的会话一律回 404。
func (t *terminal) close() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()

		// 先给 shell 一个 EOF（正常退出），再关会话与连接。
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
		if t.sess != nil {
			_ = t.sess.Close()
		}
		if t.client != nil {
			_ = t.client.Close()
		}
		// 关掉输出管道的读端：卡在 Read 上的 pump 立刻退出（否则它要等下一次 PTY 输出）。
		if t.out != nil {
			_ = t.out.Close()
		}

		// histMu 保护的是「历史文件 + 已挂载连接」，关闭时也要在锁内做：
		// 否则可能正好插在 writeConn 的一次写中间。
		t.histMu.Lock()
		// mounted() 而不是直读 t.conn：conn 由 connMu 保护，而 WS handler 的 detach
		// 可能正在另一边跑（直读会被 -race 判成数据竞争）。
		if c := t.mounted(); c != nil {
			t.detach(c)
			_ = c.Close()
		}
		if t.hist != nil {
			name := t.hist.Name()
			_ = t.hist.Close()
			t.hist = nil
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				logx.Warnf("删除终端历史文件 %s 失败：%v", name, err)
			}
		}
		t.histMu.Unlock()
	})
}

// ---------------------------------------------------------------------------
// 终端 WebSocket
//
// 路径 /api/terminal/ws?sid=<会话 id>&tid=<终端 id 或省略>：
//   - tid 为空 → 新建终端：先发 `\x1b]tid;<id>\x07`（前端据此回填 id），再 attach。
//   - tid 非空 → 挂载已有终端：回放历史（分帧，单帧 ≤32 KiB，最多 4 MiB）→ `\x1b]ready\x07`
//     → 进入实时流。
//   - 终端不存在 → 一帧数据（人可读的说明）+ `\x1b]gone\x07` 控制帧，然后关闭。
//     前端**只认控制帧**（见 ws.go 的 oscGone 注释），不能靠人类可读文本判定。
// 浏览器断开只解挂载：终端继续跑、输出继续写历史文件，刷新后重新挂载即可回放。
// ---------------------------------------------------------------------------

type terminalHandler struct {
	mux *AdminMux
}

func (h *terminalHandler) serve(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "当前服务器不支持 hijack", http.StatusInternalServerError)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "缺少 Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	// 按 RFC 6455 §4.1 校验：这个值必须是**16 字节的随机数的 base64**。只判非空等于接受
	// 任意字符串并照它算 Accept，非标准客户端可以完成握手（真实客户端永远满足这条）。
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		http.Error(w, "Sec-WebSocket-Key 不是 16 字节的 base64", http.StatusBadRequest)
		return
	}

	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	// **不回显 Sec-WebSocket-Protocol**：本应用没有子协议，回显只是把客户端给的字符串
	// 原样拼进协议响应行（把客户端输入当协议输出）。浏览器原生 WebSocket 本来就不发这个头，
	// 需要它的客户端（比如某些 websocat 用法）在这里也拿不到任何协商结果——宁可不回，
	// 也不要凭空造一个「协商成功」的假象。
	if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		logx.Warnf("忽略客户端请求的 Sec-WebSocket-Protocol=%q（本应用没有子协议）", proto)
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + ws.AcceptKey(key) + "\r\n\r\n")); err != nil {
		_ = conn.Close()
		return
	}
	_ = brw.Flush()

	sid := r.URL.Query().Get("sid")
	tid := r.URL.Query().Get("tid")

	// gone 分支：人可读的说明给非本前端的客户端也能看懂，末尾的 oscGone 给前端判定用。
	gone := func(detail string) {
		frame := []byte("\r\n\x1b[31m" + detail + "\x1b[0m\r\n" + oscGone)
		_ = ws.WriteFramed(conn, ws.Frame(termDataOp, frame))
		_ = ws.WriteFramed(conn, ws.CloseFrame(1000, "gone"))
		_ = conn.Close()
	}

	rt, apiErr := h.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		gone(apiErr.Message)
		return
	}

	var target *terminal
	if tid == "" {
		t, apiErr := rt.newTerminal(r.Context(), defaultTermCols, defaultTermRows)
		if apiErr != nil {
			gone("新建终端失败：" + apiErr.Message)
			return
		}
		if err := ws.WriteFramed(conn, ws.Frame(termDataOp, []byte("\x1b]tid;"+t.id+"\x07"))); err != nil {
			// 挂载失败（多半是写不出去）：这个终端已经没人能认领了（id 虽然发出过，
			// 但连接已坏、前端拿不到），直接回收，避免表里留一个孤儿终端。
			t.close()
			rt.forgetTerminal(t.id)
			_ = conn.Close()
			return
		}
		if _, err := t.attach(conn); err != nil {
			t.close()
			rt.forgetTerminal(t.id)
			_ = conn.Close()
			return
		}
		target = t
	} else {
		if !store.ValidID(tid) {
			gone("终端不存在")
			return
		}
		t, ok := rt.terminal(tid)
		if !ok {
			gone("终端不存在：" + tid)
			return
		}
		// 单挂载点：挂载成功即成为操作端，旧连接被顶掉并收到「已被接管」通知。
		prev, err := t.attach(conn)
		if err != nil {
			_ = conn.Close()
			return
		}
		if prev != nil {
			t.kickDetached(prev)
		}
		target = t
	}

	// 读取客户端帧：普通数据写入 PTY；resize / ping 是 OSC 控制消息，不进 PTY。
	//
	// 浏览器发的普通消息都是单帧，但网关 / 代理可能分片（op=0 continuation），
	// 按 FIN 位累积成完整消息后再处理，避免长粘贴被截断。
	var msgBuf []byte
	for {
		op, fin, payload, rerr := ws.ReadClientFrame(brw)
		if rerr != nil {
			break
		}
		switch op {
		case ws.OpText, ws.OpBinary, ws.OpContinuation:
			if op != ws.OpContinuation {
				msgBuf = msgBuf[:0]
			}
			// 分片累积也要有上限：否则客户端可以无限续帧，msgBuf 会一直涨到进程 OOM。
			if len(msgBuf)+len(payload) > ws.MaxMessageBytes {
				logx.Warnf("终端 %s 收到超长消息（>%d 字节），断开该连接", target.id, ws.MaxMessageBytes)
				goto done
			}
			msgBuf = append(msgBuf, payload...)
			if !fin {
				continue
			}
			handleClientMessage(target, conn, msgBuf)
		case ws.OpPing:
			_ = ws.WriteFramed(conn, ws.Frame(ws.OpPong, payload))
		case ws.OpClose:
			goto done
		}
	}

done:
	// 只解除挂载，不杀终端（shell 继续运行并写历史文件）。
	target.detach(conn)
	_ = conn.Close()
}

// handleClientMessage 处理一条完整的客户端消息。
func handleClientMessage(t *terminal, c net.Conn, msg []byte) {
	s := string(msg)
	switch {
	case strings.HasPrefix(s, "\x1b]resize;") && strings.HasSuffix(s, "\x07"):
		body := strings.TrimSuffix(strings.TrimPrefix(s, "\x1b]resize;"), "\x07")
		parts := strings.Split(body, ";")
		if len(parts) != 2 {
			return
		}
		cols, err1 := strconv.Atoi(parts[0])
		rows, err2 := strconv.Atoi(parts[1])
		// 行列数直接转 uint16：不设上限时 70000 会被静默截断成 4464，
		// 把 PTY 设成一个莫名其妙的尺寸。
		if err1 != nil || err2 != nil || cols < 1 || cols > maxTermDimension || rows < 1 || rows > maxTermDimension {
			return
		}
		if err := t.resize(c, uint16(cols), uint16(rows)); err != nil && !errors.Is(err, errNotOwner) {
			logx.Warnf("终端 %s 调整尺寸失败：%v", t.id, err)
		}
	case strings.HasPrefix(s, "\x1b]ping") && strings.HasSuffix(s, "\x07"):
		// 心跳：只用来让代理 / NAT 别把空闲连接断掉，不写入 PTY。
	default:
		// 非操作端的残留输入在服务端丢弃（见 writeInput 的说明）。
		if err := t.writeInput(c, msg); err != nil && !errors.Is(err, errNotOwner) {
			logx.Warnf("终端 %s 写入输入失败：%v", t.id, err)
		}
	}
}
