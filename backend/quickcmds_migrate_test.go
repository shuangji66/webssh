package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 快捷指令：迁移的加固（第二十四轮）
//
// 三条各自独立、都能单独跑红：
//   ① 文件名过滤：QuickCmdsScopes 只认 global.json 与 <合法会话 id>.json；
//   ② id 重发：同一张表里 id 重复或非法（含冒号）时，迁移给后面的重发并重新加密；
//   ③ 丢失更新：读路径的「读 → 迁移 → 重写」与整表保存互斥，迁移的过期快照不许把
//      刚保存的表盖回去。
// ---------------------------------------------------------------------------

// rawQuickCmdsFiles 在数据目录里造出各种文件名（含只有手工编辑才会出现的怪名字）。
func rawQuickCmdsFiles(t *testing.T, a *testApp, files map[string]string) {
	t.Helper()
	dir := filepath.Join(a.renv.DataDir, "quickcmds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建 quickcmds 目录失败：%v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("写 %s 失败：%v", name, err)
		}
	}
}

// TestQuickCmdsScopesOnlyAcceptsScopeFilenames 覆盖启动扫描的文件名过滤：
// 只有 `global.json` 与 `<合法会话 id>.json` 是表，其余（`.json`、带空格/冒号的名字、
// `WriteJSON` 的 tmp 残渣、同名目录）都要跳过。
//
// `.json` 那条是必须钉住的：旧写法把它削成空串 → 当成**全局表**去迁移，于是这个手工文件被
// 加密重写，而它根本不是一张表。
func TestQuickCmdsScopesOnlyAcceptsScopeFilenames(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	validSID := "abcd1234"

	// 两份合法的表是真 v1 明文（启动扫描必须迁移它们）；两个非法名字的表也让它们带明文，
	// 用来证明扫描**没有**把它们当成表（否则它们会被加密重写成 version 2）。
	rawQuickCmdsFiles(t, a, map[string]string{
		"global.json":             `{"version":1,"commands":[{"id":"g1","name":"全局","content":"echo global","auto":false}]}`,
		validSID + ".json":        `{"version":1,"commands":[{"id":"s1","name":"专属","content":"echo session","auto":false}]}`,
		".json":                   `{"version":1,"commands":[{"id":"d1","name":"名叫点json","content":"echo leak","auto":false}]}`,
		"a b.json":                `{"version":1,"commands":[{"id":"w1","name":"带空格","content":"echo space","auto":false}]}`,
		"a:b.json":                `{"version":1,"commands":[{"id":"c1","name":"带冒号","content":"echo colon","auto":false}]}`,
		"global.json.tmp-1234567": `{"version":1,"commands":[{"id":"t1","name":"残渣","content":"echo tmp","auto":false}]}`,
	})

	scopes, err := a.mux.st.QuickCmdsScopes()
	if err != nil {
		t.Fatalf("列出作用域失败：%v", err)
	}
	want := []string{"", validSID} // sort.Strings：空串（全局）在前
	if len(scopes) != len(want) {
		t.Fatalf("作用域列表 = %q，期望 %q", scopes, want)
	}
	for i := range want {
		if scopes[i] != want[i] {
			t.Fatalf("作用域列表 = %q，期望 %q", scopes, want)
		}
	}

	// 重启 = 跑一次启动迁移：两份合法的表变密文，非法名字那几份**一个字节都不动**。
	b := a.restart(t)
	for _, name := range []string{"global.json", validSID + ".json"} {
		f := quickCmdsFileOfAny(t, b, name)
		if f.Version != 2 || len(f.Commands) != 1 || f.Commands[0].Content == nil {
			t.Errorf("%s 没有被启动迁移加密：%+v", name, f)
		}
	}
	for name, leak := range map[string]string{
		".json":    "echo leak",
		"a b.json": "echo space",
		"a:b.json": "echo colon",
	} {
		raw := readLocal(t, filepath.Join(b.renv.DataDir, "quickcmds", name))
		if !strings.Contains(raw, leak) {
			t.Errorf("%s 不是一张合法的表，启动扫描不该碰它（内容变了）：\n%s", name, raw)
		}
	}
	// tmp 残渣同样不该被迁移（它可能是一份写了一半的表，动它没有任何好处）
	if raw := readLocal(t, filepath.Join(b.renv.DataDir, "quickcmds", "global.json.tmp-1234567")); !strings.Contains(raw, "echo tmp") {
		t.Errorf("tmp 残渣不该被当成表：\n%s", raw)
	}
}

// quickCmdsFileOfAny 按**文件名**读一张表（列表里的名字可能不是合法作用域）。
func quickCmdsFileOfAny(t *testing.T, a *testApp, name string) quickCmdsFile {
	t.Helper()
	var f quickCmdsFile
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "quickcmds", name))
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("解析 %s 失败：%v\n%s", name, err, raw)
	}
	return f
}

// TestQuickCmdsReissuesDupAndInvalidIDs 覆盖表内 id 重复 / 非法时的重发：
// 迁移后 id 互不相同且都合法、**表内第一个出现的 id 保留**、内容与 auto 逐字节不变
// （内容必须跟着新 id 重新加密，否则 AAD 对不上、读出来就是「解密失败」），
// 而且此时两条密文互换必须解不开（这正是重发的目的）。
func TestQuickCmdsReissuesDupAndInvalidIDs(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	// 三条：两条 id 相同（手工文件能造），一条 id 含冒号（`store.ValidID` 不认，而且它会让
	// AAD 里 `quickcmd:<id>:<sid>` 的分隔符产生歧义）。
	writeLegacyQuickCmds(t, a, "",
		legacyEntry("dup1", "甲", "echo 甲", false),
		legacyEntry("dup1", "乙", "echo 乙", true),
		legacyEntry("bad:id", "丙", "echo 丙", false),
	)

	out := a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	cmds := listOf(out, "commands")
	if len(cmds) != 3 {
		t.Fatalf("三条指令应当都在：%v", out)
	}
	want := []struct {
		name    string
		content string
		auto    bool
	}{{"甲", "echo 甲", false}, {"乙", "echo 乙", true}, {"丙", "echo 丙", false}}
	seen := map[string]bool{}
	for i, c := range cmds {
		id := strOf(c, "id")
		if !store.ValidID(id) {
			t.Errorf("第 %d 条的 id %q 仍然非法", i, id)
		}
		if seen[id] {
			t.Errorf("第 %d 条的 id %q 在表里重复", i, id)
		}
		seen[id] = true
		// 顺序不变，名称 / 内容 / auto 逐字节不变
		if strOf(c, "name") != want[i].name || strOf(c, "content") != want[i].content ||
			c.(map[string]any)["auto"] != want[i].auto {
			t.Errorf("第 %d 条被改坏了：%v，期望 %+v", i, c, want[i])
		}
		if i == 0 && id != "dup1" {
			t.Errorf("表里第一个出现的 id 应当保留，实际 %q", id)
		}
		if i > 0 && id == "dup1" {
			t.Errorf("第 %d 条的重复 id 应当被重发", i)
		}
	}

	// 磁盘上是密文，且重启后照样读得出来（说明是**用新 AAD 重新加密**，不是把旧密文挂在
	// 新 id 下面——后者会在重启后变成「解密失败」）。
	raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf("")))
	for _, leak := range []string{"echo 甲", "echo 乙", "echo 丙"} {
		if strings.Contains(raw, leak) {
			t.Errorf("迁移后磁盘上还有明文命令 %q：\n%s", leak, raw)
		}
	}
	b := a.restart(t)
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	cmds = listOf(out, "commands")
	if len(cmds) != 3 {
		t.Fatalf("重启后指令数 = %d", len(cmds))
	}
	for i, c := range cmds {
		if strOf(c, "content") != want[i].content || c.(map[string]any)["auto"] != want[i].auto {
			t.Errorf("重启后第 %d 条内容变了：%v", i, c)
		}
	}

	// 互换任意两条的密文，用它们各自的 id 都必须解不开（id 唯一 + AAD 绑定 id 的直接后果）。
	f := quickCmdsFileOf(t, b, "")
	for i := 0; i < len(f.Commands); i++ {
		for j := i + 1; j < len(f.Commands); j++ {
			left, right := f.Commands[i], f.Commands[j]
			if _, err := decryptQuickCmdContent(b.mux.crypt, "", left); err != nil {
				t.Fatalf("第 %d 条自己的密文应当解得开：%v", i, err)
			}
			// 把 j 的密文塞进 i（id 仍是 i 的 id）：AAD 对不上，必须报错
			swapped := left
			swapped.Content = right.Content
			if _, err := decryptQuickCmdContent(b.mux.crypt, "", swapped); err == nil {
				t.Errorf("第 %d 条（id=%s）与第 %d 条（id=%s）的密文互换后竟然解得开", i, left.ID, j, right.ID)
			}
		}
	}

	// 第二阶段：**已经是密文**的表里出现重复 id（同样只有手工文件/绕过接口才造得出：接口那侧
	// sanitizeQuickCmds 会去重，而 saveQuickCmds 本身不检查）。这条路要先把第二条的密文按旧 id
	// 解开、再按新 id 重新加密——只换 id 不重新加密的话，这条内容立刻变成「解密失败」。
	dup := []quickCmd{
		{ID: "same01", Name: "丁", Content: "echo 丁", Auto: false},
		{ID: "same01", Name: "戊", Content: "echo 戊", Auto: true},
	}
	if err := saveQuickCmds(b.mux.st, b.mux.crypt, "", dup); err != nil {
		t.Fatalf("准备重复 id 的密文表失败：%v", err)
	}
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	cmds = listOf(out, "commands")
	if len(cmds) != 2 {
		t.Fatalf("密文表去重后指令数 = %d", len(cmds))
	}
	if id := strOf(cmds[0], "id"); id != "same01" {
		t.Errorf("密文表里第一个出现的 id 应当保留，实际 %q", id)
	}
	if strOf(cmds[1], "id") == "same01" || !store.ValidID(strOf(cmds[1], "id")) {
		t.Errorf("密文表里的重复 id 应当被重发成合法 id，实际 %q", strOf(cmds[1], "id"))
	}
	dupWant := []struct {
		name    string
		content string
		auto    bool
	}{{"丁", "echo 丁", false}, {"戊", "echo 戊", true}}
	for i, c := range cmds {
		if strOf(c, "name") != dupWant[i].name || strOf(c, "content") != dupWant[i].content ||
			c.(map[string]any)["auto"] != dupWant[i].auto {
			t.Errorf("密文表去重后第 %d 条被改坏了：%v，期望 %+v", i, c, dupWant[i])
		}
	}
	// 重发后的 id 必须与重写后的密文一致：重启（换一次 newAdminMux）后照样读得出同样的内容。
	c := b.restart(t)
	out = c.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	for i, got := range listOf(out, "commands") {
		if strOf(got, "content") != dupWant[i].content {
			t.Errorf("密文表去重、重启后第 %d 条内容 = %q，期望 %q", i, strOf(got, "content"), dupWant[i].content)
		}
	}
}

// TestQuickCmdsMigrationSaveLostUpdate 覆盖「读路径的迁移重写」与「整表保存」的丢失更新：
// load 手里那份**过期快照**（迁移结果）不许把用户刚保存的表整表覆盖。
//
// 判据：每一轮并发跑 { load（会触发迁移） , save（一张全新的表） }，断言**最终磁盘上的表就是
// save 提交的那一张**。两种合法时序都会满足它——load 先跑完（迁移旧的、再被 save 盖掉）、或
// save 先跑完（load 读到的已是新表、无明文可迁移、根本不写）。只有「load 读到旧表 → save 写新表
// → load 把旧表的迁移结果写回去」这一种时序会失败；**去掉 quickCmdsLocks 这把锁就能跑红**
// （这是我本地验证过的：不加锁时几十轮里稳定命中，加锁后 0 失败）。
//
// 迁移的表刻意造到 50 条明文（每条 200 字节）：迁移要做 50 次加密 + 整表序列化 + 重写，落盘
// 窗口越宽，交错越容易命中（复审探针用小表 200 轮才命中 6–9 轮）。
func TestQuickCmdsMigrationSaveLostUpdate(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	path := filepath.Join(a.renv.DataDir, quickCmdsPathOf(""))

	legacy := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		legacy = append(legacy, legacyEntry(fmt.Sprintf("old%02d", i), fmt.Sprintf("旧%d", i),
			"echo "+strings.Repeat("x", 200), true))
	}
	saved := []quickCmd{{ID: "new01", Name: "新表", Content: "echo NEW", Auto: false}}

	const rounds = 40
	for round := 0; round < rounds; round++ {
		writeLegacyQuickCmds(t, a, "", legacy...)

		var wg sync.WaitGroup
		var loadErr error
		// 请求路径上的错误类型是 *apierr.Error（接口返回的形态），用指针接住避免
		// 「typed nil 塞进 error 接口」那个经典陷阱。
		var saveErr *apierr.Error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// 真实链路：读接口 → loadQuickCmdsFile（遇到明文就迁移重写）
			_, loadErr = loadQuickCmdsFile(a.mux.st, a.mux.crypt, "")
		}()
		go func() {
			defer wg.Done()
			<-start
			saveErr = saveQuickCmds(a.mux.st, a.mux.crypt, "", saved)
		}()
		close(start)
		wg.Wait()

		if loadErr != nil {
			t.Fatalf("第 %d 轮读表失败：%v", round, loadErr)
		}
		if saveErr != nil {
			t.Fatalf("第 %d 轮保存失败：%v", round, saveErr)
		}

		f := quickCmdsFileOf(t, a, "")
		if f.Version != 2 || len(f.Commands) != 1 || f.Commands[0].ID != "new01" {
			t.Fatalf("第 %d 轮：save 提交的表被迁移的过期快照覆盖了（磁盘上是 %d 条，首条 id=%q）：\n%s",
				round, len(f.Commands), firstID(f), readLocal(t, path))
		}
		if got, err := decryptQuickCmdContent(a.mux.crypt, "", f.Commands[0]); err != nil || got != "echo NEW" {
			t.Fatalf("第 %d 轮：磁盘上的内容 = %q（err=%v），期望 echo NEW", round, got, err)
		}
	}
}

// firstID 是断言失败时打印用的：磁盘上第一条指令的 id。
func firstID(f quickCmdsFile) string {
	if len(f.Commands) == 0 {
		return ""
	}
	return f.Commands[0].ID
}
