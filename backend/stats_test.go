package main

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 系统信息的解析（纯函数）：喂字符串，断言算出来的数
// ---------------------------------------------------------------------------

// /proc/stat 的聚合行：cpu user nice system idle iowait irq softirq steal guest guest_nice
const (
	statSample1 = `cpu  1000 20 300 8000 500 30 10 5 0 0
cpu0 500 10 150 4000 250 15 5 2 0 0
intr 12345
`
	statSample2 = `cpu  1100 20 320 8500 520 30 10 5 0 0
cpu0 550 10 160 4250 260 15 5 2 0 0
`
)

func TestParseCPUSampleAndPercent(t *testing.T) {
	before, ok := parseCPUSample(statSample1)
	if !ok {
		t.Fatal("聚合行应当能解析出来")
	}
	after, ok := parseCPUSample(statSample2)
	if !ok {
		t.Fatal("第二份采样应当能解析出来")
	}
	// total 增加 100 + 20 + 520 = 640，idle 增加 500 + 20 = 520 → 忙 120/640 = 18.75%
	pct, ok := cpuPercent(before, after)
	if !ok {
		t.Fatal("两次采样之间计数器动了，应当算出占用率")
	}
	if pct < 18.7 || pct > 18.8 {
		t.Errorf("CPU 占用率 = %v，期望 18.75", pct)
	}

	// 只认聚合行：`cpu0` 那行的数值不能被当成整机
	if before.total == 0 {
		t.Error("聚合行没被算进 total")
	}
	// 采样之间计数器没动（粒度太粗 / 间隔太短）→ 不显示，而不是报一个假的 0%
	if _, ok := cpuPercent(before, before); ok {
		t.Error("两次采样完全相同不该给出占用率")
	}
	// 解析不出来的输入一律 false，不 panic
	if _, ok := parseCPUSample("not a stat file\n"); ok {
		t.Error("没有 cpu 行的输入应当解析失败")
	}
	if _, ok := parseCPUSample("cpu  1 2\n"); ok {
		t.Error("字段不全的 cpu 行应当解析失败")
	}
}

func TestParseMemInfo(t *testing.T) {
	// 新内核：有 MemAvailable（已用 = total − available，不是 total − MemFree）
	used, total, ok := parseMemInfo(`MemTotal:        8000000 kB
MemFree:          200000 kB
MemAvailable:    4000000 kB
Buffers:          100000 kB
Cached:          2000000 kB
`)
	if !ok {
		t.Fatal("正常的 meminfo 应当解析成功")
	}
	if total != 8000000*1024 {
		t.Errorf("MemTotal = %d", total)
	}
	if used != 4000000*1024 {
		t.Errorf("已用内存 = %d，期望 4000000 kB（按 MemAvailable 算）", used)
	}

	// 老内核：没有 MemAvailable → 退到 Free + Buffers + Cached
	used, total, ok = parseMemInfo(`MemTotal:        8000000 kB
MemFree:          200000 kB
Buffers:          100000 kB
Cached:          2000000 kB
`)
	if !ok {
		t.Fatal("缺 MemAvailable 的老内核输出也应当解析成功")
	}
	if want := uint64(8000000-200000-100000-2000000) * 1024; used != want {
		t.Errorf("老内核口径的已用内存 = %d，期望 %d", used, want)
	}
	if total != 8000000*1024 {
		t.Errorf("MemTotal = %d", total)
	}

	// 没有 MemTotal（不是 meminfo）→ 失败，不编数字
	if _, _, ok := parseMemInfo("Whatever: 1 kB\n"); ok {
		t.Error("没有 MemTotal 的输出应当解析失败")
	}
}

func TestParseLoadAvg(t *testing.T) {
	load := parseLoadAvg("0.85 0.81 0.99 1/234 5678\n")
	if len(load) != 3 || load[0] != 0.85 || load[1] != 0.81 || load[2] != 0.99 {
		t.Errorf("负载 = %v，期望 [0.85 0.81 0.99]", load)
	}
	// 只有两个数（某些精简实现）时给两个，不补零
	if got := parseLoadAvg("1.50 2.25\n"); len(got) != 2 {
		t.Errorf("两个数的负载 = %v，期望两个", got)
	}
	if got := parseLoadAvg("\n"); len(got) != 0 {
		t.Errorf("空输入应当是空负载：%v", got)
	}
}

func TestParseDf(t *testing.T) {
	// 英文表头
	used, avail, ok := parseDf(`Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/sda1        100000000  46800000  53200000      47% /home/demo
`)
	if !ok {
		t.Fatal("正常的 df 输出应当解析成功")
	}
	if used != 46800000 || avail != 53200000 {
		t.Errorf("已用/可用 = %d/%d", used, avail)
	}

	// 中文表头：**不能**靠匹配表头字样（locale 会变），要按位置找容量那一列
	used, avail, ok = parseDf(`文件系统         1024-块        已用      可用 已用% 挂载点
/dev/sda1        100000000  46800000  53200000   47% /home/demo
`)
	if !ok || used != 46800000 || avail != 53200000 {
		t.Errorf("中文 locale 的 df 输出解析失败：%d/%d ok=%v", used, avail, ok)
	}

	// 设备名很长也不会被折行（df -P 的保证），带 % 的容量列仍然认得出来
	used, avail, ok = parseDf(`Filesystem 1024-blocks Used Available Capacity Mounted on
/dev/mapper/very-long-volume-name-here 204800 102400 102400 50% /data
`)
	if !ok || used != 102400 || avail != 102400 {
		t.Errorf("长设备名的 df 输出解析失败：%d/%d ok=%v", used, avail, ok)
	}

	if _, _, ok := parseDf("df: /nope: No such file or directory\n"); ok {
		t.Error("只有错误信息的输出应当解析失败")
	}
}

func TestParseStatsSections(t *testing.T) {
	out := `@@stat1
cpu  1000 20 300 8000 500 30 10 5 0 0
cpu0 500 10 150 4000 250 15 5 2 0 0
@@mem
MemTotal:        8000000 kB
MemFree:          200000 kB
MemAvailable:    4000000 kB
@@load
0.85 0.81 0.99 1/234 5678
@@df
Filesystem     1024-blocks      Used Available Capacity Mounted on
/dev/sda1        100000000  46800000  53200000      47% /home/demo
@@stat2
cpu  1100 20 320 8500 520 30 10 5 0 0
cpu0 550 10 160 4250 260 15 5 2 0 0
`
	stats := parseStats(out)
	if !stats.Available {
		t.Fatalf("各段都在，应当 available：%+v", stats)
	}
	if stats.CPUPercent == nil || *stats.CPUPercent < 18.7 || *stats.CPUPercent > 18.8 {
		t.Errorf("CPU = %v，期望约 18.75", stats.CPUPercent)
	}
	if stats.MemTotal != 8000000*1024 || stats.MemUsed != 4000000*1024 {
		t.Errorf("内存 = %d/%d", stats.MemUsed, stats.MemTotal)
	}
	if len(stats.Load) != 3 || stats.Load[0] != 0.85 {
		t.Errorf("负载 = %v", stats.Load)
	}
	if stats.DiskPercent == nil || *stats.DiskPercent < 46.7 || *stats.DiskPercent > 46.9 {
		t.Errorf("磁盘占用 = %v，期望约 46.8（自己按 used/(used+avail) 算，不用 df 那个取整的 47%%）", stats.DiskPercent)
	}
	// 核数由 /proc/stat 的 `cpuN` 行数得到（前端按 load1/核数 判负载的颜色）：
	// 上面的 stat1 段里只有 cpu0，所以是 1 核——不能把聚合行 `cpu ` 也算进去。
	if stats.CPUCount != 1 {
		t.Errorf("CPU 核数 = %d，期望 1（只数 cpuN 行）", stats.CPUCount)
	}
}

func TestCountCPUCores(t *testing.T) {
	// 8 核：聚合行 + cpu0..cpu7
	eight := "cpu  1 2 3 4 5 6 7 8\n" + strings.Repeat("cpu0 1 2 3 4\n", 0)
	for i := 0; i < 8; i++ {
		eight += "cpu" + strconv.Itoa(i) + " 1 2 3 4\n"
	}
	if got := countCPUCores(eight); got != 8 {
		t.Errorf("核数 = %d，期望 8", got)
	}
	// 非 Linux（没有 /proc/stat）：0 —— 前端会退回「按 1 核算」，不 panic 也不瞎猜
	if got := countCPUCores(""); got != 0 {
		t.Errorf("空输入核数 = %d，期望 0", got)
	}
	// 干扰行（cpu 后面不是数字）不能算成核
	if got := countCPUCores("cpu  1 2 3 4\ncpuinfo 1\nctxt 5\n"); got != 0 {
		t.Errorf("干扰行被当成核了：%d", got)
	}
}

func TestParseStatsPartialAndEmpty(t *testing.T) {
	// 非 Linux：只有 df 能跑（macOS 就是这种情况）→ 只显示磁盘，仍然算 available
	stats := parseStats(`@@df
Filesystem 1024-blocks Used Available Capacity Mounted on
/dev/disk1 1000 400 600 40% /
`)
	if !stats.Available {
		t.Fatal("df 有输出就该显示磁盘")
	}
	if stats.CPUPercent != nil || stats.MemTotal != 0 || len(stats.Load) != 0 {
		t.Errorf("不该凭空造出 CPU / 内存 / 负载：%+v", stats)
	}
	if stats.DiskPercent == nil || *stats.DiskPercent != 40 {
		t.Errorf("磁盘占用 = %v，期望 40", stats.DiskPercent)
	}

	// 一段都没有（不是 Linux，df 也没有）→ available=false + 原因（给日志，前端不显示）
	empty := parseStats("@@stat1\n@@mem\n@@load\n@@df\n@@stat2\n")
	if empty.Available {
		t.Error("什么都读不到时不该说 available")
	}
	if empty.Reason == "" {
		t.Error("读不到时应当给出原因（服务端日志用）")
	}

	// 只有一次采样（命令在 sleep 期间被杀）→ 没有 CPU，但其余照常
	half := parseStats(`@@stat1
cpu  1000 20 300 8000 500 30 10 5 0 0
@@mem
MemTotal: 1000 kB
MemAvailable: 500 kB
`)
	if half.CPUPercent != nil {
		t.Error("只有一次采样时不该给出 CPU 占用率")
	}
	if !half.Available || half.MemTotal != 1000*1024 {
		t.Errorf("内存仍应显示：%+v", half)
	}
}

// ---------------------------------------------------------------------------
// 接口：/api/sessions/stats
// ---------------------------------------------------------------------------

// TestSessionStatsEndpoint 走完整链路（HTTP → SSH → 远端 shell → 解析）。
//
// 断言的是「接口的形状与量纲」，不是具体数值：远端就是跑测试的这台机器，数值本来就该变。
// 在 Linux 上进一步要求真的读到 CPU / 内存 / 负载——否则解析器坏了也会「绿」。
func TestSessionStatsEndpoint(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	out := a.ok(t, "/api/sessions/stats", map[string]any{"id": sid})
	stats, _ := out["stats"].(map[string]any)
	if stats == nil {
		t.Fatalf("响应里没有 stats：%v", out)
	}
	if stats["available"] != true {
		// 非 Linux 的构建机上（macOS/BSD）只有 df 能读，available 仍该是 true；
		// 两者都不是就说明命令或解析真的坏了。
		t.Fatalf("系统信息不可用（原因 %v）：%v", stats["reason"], stats)
	}
	if v, ok := numOK(stats, "cpuPercent"); ok && (v < 0 || v > 100) {
		t.Errorf("cpuPercent = %v，应当在 0–100", v)
	}
	if v, ok := numOK(stats, "diskPercent"); ok && (v < 0 || v > 100) {
		t.Errorf("diskPercent = %v，应当在 0–100", v)
	}
	if used, okU := numOK(stats, "memUsed"); okU {
		total, okT := numOK(stats, "memTotal")
		if !okT || total <= 0 || used < 0 || used > total {
			t.Errorf("内存 = %v/%v，应当 0 ≤ used ≤ total", stats["memUsed"], stats["memTotal"])
		}
	}
	if runtime.GOOS == "linux" {
		if _, ok := numOK(stats, "cpuPercent"); !ok {
			t.Errorf("Linux 上应当读得到 CPU 占用率：%v", stats)
		}
		if _, ok := numOK(stats, "memTotal"); !ok {
			t.Errorf("Linux 上应当读得到内存总量：%v", stats)
		}
		if load := listOf(stats, "load"); len(load) != 3 {
			t.Errorf("Linux 上应当有三个平均负载：%v", stats["load"])
		}
		// 核数：前端按 load1/核数 判负载的颜色，缺了它 8 核上的 1.5 会被当成重载
		if cores, ok := numOK(stats, "cpuCount"); !ok || cores < 1 {
			t.Errorf("Linux 上应当读得到 CPU 核数：%v", stats)
		}
	}

	// 会话没打开 → 409 not_open（前端只对已打开的会话轮询，这里钉住后端的口径）
	closed := a.ok(t, "/api/sessions/save", map[string]any{
		"name": "closed", "host": a.ssh.Host(), "port": a.ssh.Port(),
		"user": a.sshUser + "-other", "authType": "password", "password": a.sshPassword,
	})
	closedSid := strOf(closed["session"], "id")
	a.fail(t, "/api/sessions/stats", map[string]any{"id": closedSid}, "not_open", 409)

	// 关闭之后也不能再取（连接已经没了，不能拿一个假的 0 糊弄过去）
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	a.fail(t, "/api/sessions/stats", map[string]any{"id": sid}, "not_open", 409)

	// 不存在的会话 → 404
	a.fail(t, "/api/sessions/stats", map[string]any{"id": "nosuchid"}, "session_gone", 404)
}

// numOK 取一个数值字段（JSON 解出来是 float64；缺省/非数值时 ok=false）。
func numOK(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// TestParseCPUSampleExcludesGuestTime 钉住 CPU 的分母口径：**不把 guest / guest_nice 计入**。
//
// 内核把 guest 时间已经算进 user、guest_nice 算进 nice（proc(5)），再加一遍等于把分母放大——
// 一台跑着虚拟机的 NAS 上，CPU 占用率会系统性偏低（procps 的 vmstat 同样只取前 8 个字段）。
func TestParseCPUSampleExcludesGuestTime(t *testing.T) {
	// 第 9、10 个字段（guest=700、guest_nice=800）刻意给成非 0：样本里它们是 0 的话，
	// 这条口径根本测不出来。
	block := "cpu  1000 20 300 8000 500 30 10 5 700 800\n" +
		"cpu0 500 10 150 4000 250 15 5 2 350 400\n"

	s, ok := parseCPUSample(block)
	if !ok {
		t.Fatal("聚合行应当能解析出来")
	}
	if want := uint64(1000 + 20 + 300 + 8000 + 500 + 30 + 10 + 5); s.total != want {
		t.Errorf("total = %d，期望 %d（guest / guest_nice 不能计入分母）", s.total, want)
	}
	if want := uint64(8000 + 500); s.idle != want {
		t.Errorf("idle = %d，期望 %d（idle + iowait）", s.idle, want)
	}

	// 两次采样之间 guest 大幅变化时，占用率仍然只由前 8 个字段决定
	after, ok := parseCPUSample("cpu  1400 20 400 9000 600 30 10 5 900 800\n")
	if !ok {
		t.Fatal("第二份采样应当能解析出来")
	}
	pct, ok := cpuPercent(s, after)
	if !ok {
		t.Fatal("计数器动了，应当算出占用率")
	}
	// Δtotal = 400+0+100+1000+100 = 1600，Δidle = 1000+100 = 1100 → 忙 500/1600 = 31.25%
	if pct < 31.2 || pct > 31.3 {
		t.Errorf("占用率 = %v，期望 31.25（guest 不计入分母）", pct)
	}
}

// TestStatsUsable 钉住「半截输出」的判据。
//
// Code 为负表示对端没回 exit-status（连接在命令执行中断开），stdout 可能是半截的：拿它当
// 一次正常读数会给出「只有一半字段但 available=true」的系统信息，比不显示更容易骗人。
// 正的非零退出码说明命令跑完了（非 Linux 远端上 `cat /proc/stat` 失败就是 1，
// 而那份输出里的 df 段仍然有用），必须照常解析。
func TestStatsUsable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{-1, false}, // 对端没回 exit-status：半截输出，不能当读数
		{0, true},
		{1, true},   // 非 Linux：cat /proc/stat 失败，但 df 段可用
		{127, true}, // 命令不存在（例如远端连 sh 都没有）
	}
	for _, tc := range cases {
		if got := statsUsable(remoteResult{Code: tc.code}); got != tc.want {
			t.Errorf("statsUsable(Code=%d) = %v，期望 %v", tc.code, got, tc.want)
		}
	}
}
