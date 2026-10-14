package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// 会话卡片上的「系统信息」：CPU / 负载 / 内存 / 磁盘（契约 23）
//
// 数据全部来自**远端主机自己**的 /proc 与 df：本应用不在本机执行用户命令（AGENTS.md 第 1 节），
// 所以这里跑的仍然是一条交给远端登录用户 shell 的只读命令，走 remote_exec.go 那套约定
// （exec 通道 + `sh -c` + 采集两条流 + 超时）。
//
// 为什么自己养一条常驻连接，而不是复用 r.base、也不是每 5 秒拨一次号：
//  1. 前端每 5 秒问一次，每次新拨一条 SSH（认证 + 密钥交换）对 NAS 这种弱 CPU 的机器是实打实
//     的负担，远端 sshd 的日志也会被刷满；
//  2. r.base 由 sftpMu 保护：一次大文件上传会占住它几分钟，轮询会跟着一起卡死（反过来也一样，
//     一条慢命令会把文件管理堵住）。
// 所以这里懒建一条**只跑这一条命令**的连接，随会话一起关闭（见 sessionRuntime.close）。
// ---------------------------------------------------------------------------

// statsTimeout 是单次取数的超时。
//
// 命令里有 0.5 秒的采样间隔，正常几百毫秒就回来了；10 秒是给「远端很忙 / 网络很慢」留的余量。
// 到点就杀掉远端进程并丢掉那条连接（下次轮询重新拨），不会让一个请求挂住 5 分钟
// （remoteCommandTimeout 是给 tar 这种长命令的，这里不需要）。
const statsTimeout = 10 * time.Second

// statsSampleSeconds 是两次读 /proc/stat 之间隔多久（CPU 占用率只能由两次采样算出来）。
//
// 不缓存在服务端「上一次的采样」再按两次轮询的间隔算：那样第一次轮询没有基线（要回一个空值），
// 而且轮询间隔被网络抖动拉长时会算出一个跨了几十秒的平均值，看着反而不像「现在的占用」。
const statsSampleSeconds = "0.5"

// statsCommand 是一条只读命令：把要用的几份文件打上标记输出，中间隔 statsSampleSeconds
// 再读一次 /proc/stat。
//
// stderr 一律丢掉：非 Linux 远端（macOS / BSD）没有 /proc，报一堆 "No such file" 对我们毫无
// 用处——我们要的是「读到了什么」，不是「哪几个文件不存在」；判断「是不是读不到」由解析结果
// （available）负责，而不是靠命令的退出码。
//
// 磁盘取 $HOME 所在的文件系统（登录用户的家目录，也是文件管理默认进去的地方），
// 家目录不存在或 df 认不出来时退到 /。
const statsCommand = `echo '@@stat1'; cat /proc/stat 2>/dev/null; ` +
	`echo '@@mem'; cat /proc/meminfo 2>/dev/null; ` +
	`echo '@@load'; cat /proc/loadavg 2>/dev/null; ` +
	`echo '@@df'; df -P -k "${HOME:-/}" 2>/dev/null || df -P -k / 2>/dev/null; ` +
	`echo '@@stat2'; sleep ` + statsSampleSeconds + `; cat /proc/stat 2>/dev/null`

// sessionStats 是一次取数的结果（前端只显示数值，不做图形）。
//
// Available 为 false 时其余字段全空，前端整块不显示：远端不是 Linux、连接断了、权限不够……
// 这些情况在卡片上堆一句错误文案只会盖住会话本身的信息，原因记在服务端日志里（见 stats）。
type sessionStats struct {
	Available bool `json:"available"`
	// Reason 只在 Available 为 false 时给（给日志和排查用，前端不展示）。
	Reason string `json:"reason,omitempty"`
	// CPUPercent 是两次采样之间的 CPU 占用率（0–100，保留一位小数由前端做）。
	CPUPercent *float64 `json:"cpuPercent,omitempty"`
	// MemUsed / MemTotal 单位是字节（used = total − available）。
	MemUsed  uint64 `json:"memUsed,omitempty"`
	MemTotal uint64 `json:"memTotal,omitempty"`
	// DiskPercent 是 $HOME 所在文件系统的占用率。
	DiskPercent *float64 `json:"diskPercent,omitempty"`
	// Load 是 1 / 5 / 15 分钟平均负载（/proc/loadavg 的前三个数）。
	Load []float64 `json:"load,omitempty"`
	// CPUCount 是**在线 CPU 核数**（/proc/stat 里 `cpuN` 的行数）。
	//
	// 给前端折算负载用：负载是「整机排队长度」，1.5 在 2 核机器上是重载、在 32 核上什么都不算，
	// 不除以核数就没法定阈值（前端按 load1 / cpuCount 判绿黄红）。
	// 用 /proc/stat 的行数而不是 `nproc`：负载是内核对**全部在线 CPU** 统计的，行数与它同口径；
	// 而且这份数据本来就已经读回来了，不额外跑命令。
	CPUCount int `json:"cpuCount,omitempty"`
}

// stats 取一次会话所在远端主机的系统信息。
//
// 返回的 error 只表示**基础设施失败**（拨号 / 开会话通道 / 超时）：调用方（前端）拿到错误就
// 不显示这一块。远端读不到 /proc 不算错误，它是 available=false。
func (r *sessionRuntime) stats(ctx context.Context) (sessionStats, *apierr.Error) {
	client, apiErr := r.statsClientFor(ctx)
	if apiErr != nil {
		return sessionStats{}, apiErr
	}
	res, apiErr := r.execOn(ctx, client, statsCommand, statsTimeout)
	if apiErr != nil {
		// 这条连接已经不可用了（超时被关、远端重启、空闲太久被防火墙掐）：丢掉它，
		// 下一次轮询会重新拨一条。不丢的话之后每一次轮询都撞在死连接上。
		r.dropStatsClient(client)
		return sessionStats{}, apiErr
	}
	// Code 为负（-1）表示**对端没回 exit-status**：连接是在命令跑的过程中断的，stdout 是
	// 半截的。这种输出照样能解析出「一半字段」（例如只有 stat1 与 mem，available 仍为 true），
	// 拿它当一次正常读数比不显示更容易骗人，所以按失败处理并丢掉这条连接。
	//
	// 注意**只判负数**：正的非零退出码说明命令跑完了（非 Linux 远端上 `cat /proc/stat`
	// 失败就是 1，而那份输出里的 df 段仍然有用，见 parseStats 的分段口径）。
	if !statsUsable(res) {
		r.dropStatsClient(client)
		return sessionStats{}, apierr.Fail(502, "internal", "读取远端系统信息失败：连接在命令执行中断开")
	}
	out := parseStats(res.Stdout)
	if !out.Available {
		logx.Warnf("会话 %s 读不到远端主机的系统信息：%s", r.id, out.Reason)
	}
	return out, nil
}

// statsUsable 报告一次 stats 命令的输出能不能用来解析。
//
// 判据只有「对端回没回 exit-status」：Code < 0（remoteResult 里记作 -1）意味着连接在命令
// 执行中断开，输出可能是半截的；Code >= 0（含非 0 退出码）意味着命令跑完了，输出完整。
// 单独抽成函数是为了让单测能钉住这条口径——它决定了「半死连接」是被当成一次读数还是失败。
func statsUsable(res remoteResult) bool {
	return res.Code >= 0
}

// statsClient 取（必要时懒建）轮询专用的连接。
func (r *sessionRuntime) statsClientFor(ctx context.Context) (*ssh.Client, *apierr.Error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errSessionGone("会话已关闭：%s", r.id)
	}
	if r.statsClient != nil {
		client := r.statsClient
		r.mu.Unlock()
		return client, nil
	}
	r.mu.Unlock()

	cfg, ok := r.config()
	if !ok {
		return nil, errSessionGone("会话不存在：%s", r.id)
	}
	req, apiErr := r.mgr.dialRequestOf(cfg)
	if apiErr != nil {
		return nil, apiErr
	}
	res, cerr := r.mgr.dial(ctx, req)
	if cerr != nil {
		return nil, errFromConnect(cerr)
	}

	// 拨号期间会话被关了 / 另一个并发轮询先建好了：只留一条，多出来的关掉。
	//
	// 两处 Close 都刻意放在**锁外**（与 sessionRuntime.close 同一个理由：Close 会等对端的
	// 响应，而 r.mu 还护着 terminals / order / closed，卡住它会连带卡住 /api/sessions
	// 与新终端）。
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		res.client.Close()
		return nil, errSessionGone("会话已关闭：%s", r.id)
	}
	if existing := r.statsClient; existing != nil {
		r.mu.Unlock()
		res.client.Close() // 并发轮询刚建好的那条才是缓存，我们这条多余的关掉
		return existing, nil
	}
	r.statsClient = res.client
	r.mu.Unlock()
	return res.client, nil
}

// dropStatsClient 丢掉一条已经不可用的轮询连接（只有它仍是当前缓存的那条时才清空缓存）。
func (r *sessionRuntime) dropStatsClient(client *ssh.Client) {
	r.mu.Lock()
	if r.statsClient == client {
		r.statsClient = nil
	}
	r.mu.Unlock()
	client.Close()
}

// ---------------------------------------------------------------------------
// 解析：全部是纯函数，单测直接喂字符串（见 stats_test.go）
// ---------------------------------------------------------------------------

// parseStats 把 statsCommand 的输出解析成 sessionStats。
//
// 各段互相独立：能读到的就显示（macOS 没有 /proc 但 df 能用，那就只显示磁盘），一段都读不到
// 才算 available=false。
func parseStats(out string) sessionStats {
	sections := splitStatsSections(out)
	stats := sessionStats{}
	stats.CPUCount = countCPUCores(sections["stat1"])

	if before, ok := parseCPUSample(sections["stat1"]); ok {
		if after, ok2 := parseCPUSample(sections["stat2"]); ok2 {
			if pct, ok3 := cpuPercent(before, after); ok3 {
				stats.CPUPercent = &pct
			}
		}
	}
	if used, total, ok := parseMemInfo(sections["mem"]); ok {
		stats.MemUsed, stats.MemTotal = used, total
	}
	if load := parseLoadAvg(sections["load"]); len(load) > 0 {
		stats.Load = load
	}
	if used, avail, ok := parseDf(sections["df"]); ok && used+avail > 0 {
		pct := float64(used) * 100 / float64(used+avail)
		stats.DiskPercent = &pct
	}

	stats.Available = stats.CPUPercent != nil || stats.MemTotal > 0 || stats.DiskPercent != nil || len(stats.Load) > 0
	if !stats.Available {
		stats.Reason = "远端主机没有 /proc/stat、/proc/meminfo、/proc/loadavg，df 也没有输出（不是 Linux？）"
	}
	return stats
}

// splitStatsSections 按 `@@名字` 标记把命令输出切成若干段。
//
// 标记由 statsCommand 自己 echo，所以远端文件的内容里带 `@@` 也不会串段（只认行首的标记行）。
func splitStatsSections(out string) map[string]string {
	sections := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			sections[name] = body.String()
		}
		body.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			name = strings.TrimSpace(strings.TrimPrefix(line, "@@"))
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	return sections
}

// cpuSample 是 /proc/stat 里聚合那一行（`cpu `）的两个累计值。
type cpuSample struct {
	total uint64
	idle  uint64
}

// parseCPUSample 读 /proc/stat 的第一行（`cpu  user nice system idle iowait ...`）。
//
// 只认行首是 `cpu ` 的那一行：`cpu0`、`cpu1`……是每个核的，聚合行才是「整机」。
// 空闲时间算 idle + iowait：iowait 期间 CPU 并没有在干活，不把它算空闲的话，一台在做大量
// 磁盘 IO 的 NAS 会一直显示 90%+ 的占用（而它其实是在等盘）。
//
// **只累加前 8 个字段**（user…steal）：第 9、10 个是 guest / guest_nice，内核已经把
// guest 时间算进 user、guest_nice 算进 nice 了（proc(5) 第 5 节），再加一遍等于把分母
// 放大——跑虚拟机的机器上（NAS 上开 VM 很常见）CPU 占用率会系统性偏低。procps 的 vmstat
// 同样是取前 8 个。
func parseCPUSample(block string) (cpuSample, bool) {
	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 4 {
			return cpuSample{}, false
		}
		var s cpuSample
		for i, f := range fields {
			if i >= 8 { // guest / guest_nice：已含在 user / nice 里，不重复计入分母
				break
			}
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			s.total += v
			if i == 3 || i == 4 { // idle, iowait
				s.idle += v
			}
		}
		return s, true
	}
	return cpuSample{}, false
}

// countCPUCores 数 /proc/stat 里 `cpuN` 的行数 = 在线 CPU 核数（给前端折算负载用，见 CPUCount）。
//
// 数不到（不是 Linux）时返回 0：前端会退回「按 1 核算」，那总比把 8 核的 1.5 负载当成重载好。
func countCPUCores(block string) int {
	cores := 0
	for _, line := range strings.Split(block, "\n") {
		if len(line) < 4 || !strings.HasPrefix(line, "cpu") {
			continue
		}
		if line[3] >= '0' && line[3] <= '9' {
			cores++
		}
	}
	return cores
}

// cpuPercent 由两次采样算占用率。
//
// 两次之间计数器没动（采样间隔太短、或者远端时间粒度粗）时返回 false —— 宁可这一次不显示，
// 也不要报一个除零出来的 0% 或 NaN。
func cpuPercent(before, after cpuSample) (float64, bool) {
	if after.total <= before.total || after.idle < before.idle {
		return 0, false
	}
	totalDelta := float64(after.total - before.total)
	idleDelta := float64(after.idle - before.idle)
	pct := (1 - idleDelta/totalDelta) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// parseMemInfo 读 /proc/meminfo，返回「已用 / 总量」（字节）。
//
// 已用按 MemAvailable 算（total − available），不是 total − MemFree：Linux 会把空闲内存
// 拿去做缓存，只看 MemFree 的话一台正常的机器会常年显示「用了 90%」。老内核（< 3.14）没有
// MemAvailable，退到 Free + Buffers + Cached。
func parseMemInfo(block string) (used, total uint64, ok bool) {
	values := map[string]uint64{}
	for _, line := range strings.Split(block, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[strings.TrimSpace(key)] = v * 1024 // 单位是 kB
	}
	total = values["MemTotal"]
	if total == 0 {
		return 0, 0, false
	}
	avail, hasAvail := values["MemAvailable"]
	if !hasAvail {
		avail = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if avail > total {
		avail = total
	}
	return total - avail, total, true
}

// parseLoadAvg 读 /proc/loadavg 的前三个数（1 / 5 / 15 分钟平均负载）。
func parseLoadAvg(block string) []float64 {
	fields := strings.Fields(strings.TrimSpace(block))
	out := make([]float64, 0, 3)
	for _, f := range fields {
		if len(out) == 3 {
			break
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			// 后面是 `1/234 5678` 这种运行队列/进程数，解析不了就说明三个负载已经读完了。
			break
		}
		out = append(out, v)
	}
	return out
}

// parseDf 从 `df -P -k` 的输出里取出已用 / 可用（单位 KiB）。
//
// 不认表头文字：`df -P` 保证一行一个文件系统，但表头本身是**跟着 locale 走**的
// （中文环境是「文件系统 1024-块 已用 可用 已用% 挂载点」）。所以按位置找：容量那一列一定以
// `%` 结尾，它左边两列就是已用与可用——比匹配任何语言的字样都稳。
func parseDf(block string) (used, avail uint64, ok bool) {
	for _, line := range strings.Split(block, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		capIdx := -1
		for i, f := range fields {
			if strings.HasSuffix(f, "%") {
				capIdx = i
				break
			}
		}
		if capIdx < 2 {
			continue
		}
		u, err1 := strconv.ParseUint(fields[capIdx-2], 10, 64)
		a, err2 := strconv.ParseUint(fields[capIdx-1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return u, a, true
	}
	return 0, 0, false
}
