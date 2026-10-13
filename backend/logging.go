package main

import (
	"log"
	"os"
)

// 日志纪律（契约第 2 节 + AGENTS.md）：**只记录异常与失败**。
// 启动/停止、每次 API 调用、每次连接成功这类例行信息一律不打——这个进程在飞牛上由
// supervisor 拉起，日志会被收集到应用日志里，例行噪声会把真正的故障淹掉
// （参考实现 terminal/backend/admin.go 的同一个约定）。
//
// log.Logger 自带互斥锁，多协程写是安全的；输出到 stderr（stdout 留给可能的管道）。
var appLog = log.New(os.Stderr, "[WebSSH] ", log.LstdFlags)

// warnf 记一条「失败了/不符合预期」的日志。所有失败路径都应该经它落一条，
// 否则前端只能看到一句「内部错误」，排查方向完全没有。
func warnf(format string, args ...any) {
	appLog.Printf(format, args...)
}
