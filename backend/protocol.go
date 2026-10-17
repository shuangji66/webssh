package main

import "webssh/backend/internal/ws"

// ---------------------------------------------------------------------------
// 终端控制协议（契约第 6 节）
//
// 服务端 → 客户端的控制帧一律走 OSC 序列（**不写进远程 PTY**，前端自己解析）。
// 这些常量与 frontend/src/serverapi/index.ts 里的是一对：改名/改值必须同步，
// 否则表现是「终端连上但完全没有输出」或「被顶掉不提示」。
// 纯 WebSocket 帧层的常量在 internal/ws 包里。
//
// ⚠️ **终端数据（含上面这些 OSC 控制帧）一律用二进制帧发送，不用文本帧。**
// 原因是终端输出是**任意字节流**，而 WebSocket 的文本帧按 RFC 6455 §8.1 必须
// 是合法 UTF-8：载荷里只要出现一个非法字节，浏览器就会判定协议错误并**直接断开
// 连接**（实测 Chromium：onerror + close 1006，一帧数据都收不到）。两种必然踩到的
// 情形：① 分帧边界切在多字节字符中间（回放按 32 KiB、实时按 8 KiB 切，与字符边界
// 无关）；② 远端本来就输出非法 UTF-8（`cat` 二进制文件、GBK/latin1 文件名被 `ls`
// 回显、UTF-16 日志）。改成二进制帧后浏览器不做校验，前端用**流式** TextDecoder
// 解码（`{stream: true}`，见 TerminalPane 的 openSocket），半个字符会跨帧被正确
// 拼回来，非法字节按替换字符显示——这也是真实终端的行为。
// 对应的 e2e / 测试客户端必须认 OpBinary（0x2），只认 0x1 会「终端连上但什么都没有」。
// ---------------------------------------------------------------------------

const (
	// wsCloseDetached 是「终端被其他设备接管」的关闭码（4000–4999 为应用自定区间）。
	// 前端据此把标签置为 detached：提示用户、不自动重连（否则两台设备会互抢同一个终端）。
	wsCloseDetached = 4001

	// wsCloseCreateFailed 是「新建子终端失败」的关闭码（认证失败 / 没有可用凭据 /
	// 远端拒绝 PTY 等）。**不要**把它和 oscGone 混用：gone 的语义是「终端/会话在后端
	// 不存在」，前端收到会把标签标成不存在；创建失败必须保留标签、把真实原因留在终端里，
	// 用户修正凭据后点「重连」就能重试。
	wsCloseCreateFailed = 4002

	// 以下是服务端 → 客户端的 OSC 控制帧（不写入远程 PTY，前端自己解析）。
	oscReady    = "\x1b]ready\x07"    // 历史回放结束，进入实时流
	oscExit     = "\x1b]exit\x07"     // 远程 shell 退出
	oscDetached = "\x1b]detached\x07" // 本端被顶掉（与 close 4001 双保险）
	// oscGone 是「该终端在后端不存在」的控制帧，附在错误文本帧末尾。
	// 前端**只认这个**：用人类可读文本（"terminal not found"）判定会被终端里
	// `echo terminal not found`、grep 结果、日志回显统统误判，把好端端的标签删掉。
	oscGone = "\x1b]gone\x07"

	// maxTermDimension 是 resize 允许的最大行列数：客户端给的 cols/rows 要转成 uint16，
	// 不设上限时 70000 会被静默截断成 4464，把 PTY 设成一个莫名其妙的尺寸。
	maxTermDimension = 1000

	// wsChunkSize 是历史回放时单帧的最大字节数（与契约第 4.4 节的 32 KiB 一致）。
	wsChunkSize = 32 * 1024
	// maxHistoryBytes 是历史回放与 /api/terminals/history 读取的上限（契约第 2 节）。
	maxHistoryBytes = 4 * 1024 * 1024

	// termDataOp 是「终端数据帧」的 opcode：**二进制**，理由见文件头的 ⚠️。
	// 改回 OpText 会让含中文（或多字节字符被分帧切开）的终端在重挂载时随机断连。
	termDataOp = ws.OpBinary
)
