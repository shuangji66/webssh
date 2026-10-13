package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

// ---------------------------------------------------------------------------
// WebSocket 自实现帧编解码
//
// 为什么不用第三方库：整条链路只需要「服务端发文本帧 / 收客户端掩码帧 + 两个控制帧」，
// 一个库会带进一堆用不到的东西（还有它自己的升级/关闭语义）。这里照抄参考实现
// terminal/backend/terminal.go 的实现，含它踩过的两个坑：
//   - 帧长字段由客户端给（最大 2^63-1），**先校验上限再分配**，否则一条消息就能让进程 OOM；
//   - close 用应用自定区间 4000–4999 的 4001 表示「被其他设备顶掉」。
// ---------------------------------------------------------------------------

const (
	// wsCloseDetached 是「终端被其他设备接管」的关闭码（4000–4999 为应用自定区间）。
	// 前端据此把标签置为 detached：提示用户、不自动重连（否则两台设备会互抢同一个终端）。
	wsCloseDetached = 4001

	// 以下是服务端 → 客户端的 OSC 控制帧（不写入远程 PTY，前端自己解析）。
	// 与 frontend/src/serverapi/index.ts 里的常量是一对，改一处必须改两处。
	oscReady    = "\x1b]ready\x07"    // 历史回放结束，进入实时流
	oscExit     = "\x1b]exit\x07"     // 远程 shell 退出
	oscDetached = "\x1b]detached\x07" // 本端被顶掉（与 close 4001 双保险）
	// oscGone 是「该终端在后端不存在」的控制帧，附在错误文本帧末尾。
	// 前端**只认这个**：用人类可读文本（"terminal not found"）判定会被终端里
	// `echo terminal not found`、grep 结果、日志回显统统误判，把好端端的标签删掉。
	oscGone = "\x1b]gone\x07"

	opText  = 0x1
	opBin   = 0x2
	opCont  = 0x0
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA

	// 客户端消息上限：单帧 1 MiB、整条消息（分片累积）4 MiB。正常输入只有几十字节，
	// 只有粘贴才是大消息。没有上限时，一个客户端就能用「超大长度头 / 无限分片」
	// 让这个承载全部会话的进程 OOM。
	maxFrameBytes   = 1 << 20
	maxMessageBytes = 4 << 20
	// maxTermDimension 是 resize 允许的最大行列数：客户端给的 cols/rows 要转成 uint16，
	// 不设上限时 70000 会被静默截断成 4464，把 PTY 设成一个莫名其妙的尺寸。
	maxTermDimension = 1000

	// wsChunkSize 是历史回放时单帧的最大字节数（与契约第 4.4 节的 32 KiB 一致）。
	wsChunkSize = 32 * 1024
	// maxHistoryBytes 是历史回放与 /api/terminals/history 读取的上限（契约第 2 节）。
	maxHistoryBytes = 4 * 1024 * 1024

	// connWriteTimeout 是向单个已挂载连接写数据（实时广播 / 顶号通知）的写超时。
	//
	// 客户端停止读取（挂起的笔记本、静默掉线的网络、被 SIGSTOP 的标签页）时内核发送缓冲
	// 会填满，无超时的 Write 会永久阻塞——而广播是在 terminal 的 histMu 内做的，一旦卡住，
	// 同终端的历史回放（attach）、history/clear 接口会一起挂死，这个终端从此无法再被任何
	// 设备挂载。超时即视为该端已失联：解除挂载。
	connWriteTimeout = 5 * time.Second
)

// wsAcceptKey 计算 Sec-WebSocket-Accept（RFC 6455 的固定 GUID）。
func wsAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsFrame 构造一个服务端 → 客户端帧（服务端帧一律不掩码）。
func wsFrame(op byte, payload []byte) []byte {
	n := len(payload)
	var hdr []byte
	switch {
	case n < 126:
		hdr = []byte{0x80 | op, byte(n)}
	case n < 65536:
		hdr = []byte{0x80 | op, 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 2, 10)
		hdr[0] = 0x80 | op
		hdr[1] = 127
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	return append(hdr, payload...)
}

// wsCloseFrame 构造服务端 close 帧：payload = 2 字节状态码 + UTF-8 reason。
func wsCloseFrame(code uint16, reason string) []byte {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	return wsFrame(opClose, payload)
}

// wsReadFrame 读一个客户端帧并解掩码，返回 opcode、FIN 位与载荷。
//
// 传入的是 *bufio.Reader（不是裸 conn）：握手时缓冲里可能已经躺着客户端抢先发来的帧，
// 从裸连接读会把这几个字节丢掉。
func wsReadFrame(r io.Reader) (byte, bool, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, false, nil, err
	}
	fin := h[0]&0x80 != 0
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	ln := uint64(h[1] & 0x7f)
	switch ln {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return 0, false, nil, err
		}
		ln = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return 0, false, nil, err
		}
		ln = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, false, nil, err
		}
	}
	// 长度先校验上限再分配：长度字段由客户端给，直接 make 就是一条消息打爆进程的入口。
	if ln > maxFrameBytes {
		return 0, false, nil, fmt.Errorf("websocket 帧过大：%d 字节", ln)
	}
	payload := make([]byte, ln)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, fin, payload, nil
}

// writeFramed 写一帧并带写超时（见 connWriteTimeout）。
//
// 失败由调用方决定怎么处理：已挂载连接走 terminal.writeConn（解挂载），挂载过程中的
// 回放直接把错误返回给 WS handler。
func writeFramed(c net.Conn, frame []byte) error {
	_ = c.SetWriteDeadline(time.Now().Add(connWriteTimeout))
	_, err := c.Write(frame)
	return err
}
