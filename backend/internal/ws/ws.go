// Package ws 是服务端用的 WebSocket 帧编解码（RFC 6455 的最小可用子集）。
//
// 为什么不用第三方库：整条链路只需要「服务端发文本帧 / 收客户端掩码帧 + 两个控制帧」，
// 一个库会带进一堆用不到的东西（还有它自己的升级/关闭语义）。这里照抄参考实现
// terminal/backend/terminal.go 的实现，含它踩过的两个坑：
//   - 帧长字段由客户端给（最大 2^63-1），**先校验上限再分配**，否则一条消息就能让进程 OOM；
//   - close 用应用自定区间 4000–4999 的 4001 表示「被其他设备顶掉」（关闭码由调用方传入）。
//
// 本包**只管帧**，不认业务：OSC 控制帧（`\x1b]ready\x07` 之类）与终端语义都在 main 包
// （见 backend/protocol.go）。这样拆的理由是帧层能被单独测（unit_test.go 的
// TestWSHandshakeAndFrames），而业务帧的取值属于前后端契约（CONTRACT 第 6 节）。
package ws

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// OpContinuation / OpText / OpBinary / OpClose / OpPing / OpPong 是 RFC 6455 的 opcode。
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA

	// MaxFrameBytes / MaxMessageBytes 是客户端消息上限：单帧 1 MiB、整条消息（分片累积）4 MiB。
	// 正常输入只有几十字节，只有粘贴才是大消息。没有上限时，一个客户端就能用
	// 「超大长度头 / 无限分片」让这个承载全部会话的进程 OOM。
	MaxFrameBytes   = 1 << 20
	MaxMessageBytes = 4 << 20

	// WriteTimeout 是向单个已挂载连接写数据（实时广播 / 顶号通知）的写超时。
	//
	// 客户端停止读取（挂起的笔记本、静默掉线的网络、被 SIGSTOP 的标签页）时内核发送缓冲
	// 会填满，无超时的 Write 会永久阻塞——而广播是在 terminal 的 histMu 内做的，一旦卡住，
	// 同终端的历史回放（attach）、history/clear 接口会一起挂死，这个终端从此无法再被任何
	// 设备挂载。超时即视为该端已失联：解除挂载。
	WriteTimeout = 5 * time.Second
)

// AcceptKey 计算 Sec-WebSocket-Accept（RFC 6455 的固定 GUID）。
func AcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Frame 构造一个服务端 → 客户端帧（服务端帧一律不掩码）。
func Frame(op byte, payload []byte) []byte {
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

// CloseFrame 构造服务端 close 帧：payload = 2 字节状态码 + UTF-8 reason。
func CloseFrame(code uint16, reason string) []byte {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	return Frame(OpClose, payload)
}

// ReadFrame 读一个客户端帧并解掩码，返回 opcode、FIN 位与载荷。
//
// 传入的是 *bufio.Reader（不是裸 conn）：握手时缓冲里可能已经躺着客户端抢先发来的帧，
// 从裸连接读会把这几个字节丢掉。
func ReadFrame(r io.Reader) (byte, bool, []byte, error) {
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
	if ln > MaxFrameBytes {
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

// WriteFramed 写一帧并带写超时（见 WriteTimeout）。
//
// 失败由调用方决定怎么处理：已挂载连接走 terminal.writeConn（解挂载），挂载过程中的
// 回放直接把错误返回给 WS handler。
func WriteFramed(c net.Conn, frame []byte) error {
	_ = c.SetWriteDeadline(time.Now().Add(WriteTimeout))
	_, err := c.Write(frame)
	return err
}
