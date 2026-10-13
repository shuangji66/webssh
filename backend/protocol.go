package main

// ---------------------------------------------------------------------------
// 终端控制协议（契约第 6 节）
//
// 服务端 → 客户端的控制帧一律走 OSC 序列（**不写进远程 PTY**，前端自己解析）。
// 这些常量与 frontend/src/serverapi/index.ts 里的是一对：改名/改值必须同步，
// 否则表现是「终端连上但完全没有输出」或「被顶掉不提示」。
// 纯 WebSocket 帧层的常量在 internal/ws 包里。
// ---------------------------------------------------------------------------

const (
	// wsCloseDetached 是「终端被其他设备接管」的关闭码（4000–4999 为应用自定区间）。
	// 前端据此把标签置为 detached：提示用户、不自动重连（否则两台设备会互抢同一个终端）。
	wsCloseDetached = 4001

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
)
