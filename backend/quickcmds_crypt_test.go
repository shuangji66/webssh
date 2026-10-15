package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 快捷指令命令内容的落盘加密（quickcmds.go）
//
// 一条贯穿的判据：**磁盘上不许出现命令内容的明文，但名称必须是明文**。内容里的空格与换行
// 是命令的一部分，所以另有一条同样硬的判据：**保存 → 读回 → 重启后读回，内容逐字节相同**。
// ---------------------------------------------------------------------------

// trickyContent 是「加解密不能破坏命令的内容和格式」的判据：前导空格、行尾空格、续行反斜杠
// 与多行换行都必须原样回来（历史上后端在 sanitize 里 trim 过内容，这几项正好是它会改掉的东西）。
const trickyContent = "  mysql -uroot -pS3cret \\\n    -h db.internal \\\n    app_db   \n"

// quickCmdsPathOf 是一个作用域的快捷指令文件路径（sid 为空 = 全局）。
func quickCmdsPathOf(sid string) string {
	if sid == "" {
		return filepath.Join("quickcmds", "global.json")
	}
	return filepath.Join("quickcmds", sid+".json")
}

// quickCmdsFileOf 读回磁盘上的那一份（用后端自己的类型，自动走 UnmarshalJSON）。
func quickCmdsFileOf(t *testing.T, a *testApp, sid string) quickCmdsFile {
	t.Helper()
	var f quickCmdsFile
	raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf(sid)))
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("解析 %s 失败：%v\n%s", quickCmdsPathOf(sid), err, raw)
	}
	return f
}

// rewriteQuickCmdsFile 改完磁盘上的那份文件再写回去（模拟「有人动过数据目录」）。
func rewriteQuickCmdsFile(t *testing.T, a *testApp, sid string, mutate func(f *quickCmdsFile)) {
	t.Helper()
	f := quickCmdsFileOf(t, a, sid)
	mutate(&f)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	path := filepath.Join(a.renv.DataDir, quickCmdsPathOf(sid))
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("写回 %s 失败：%v", path, err)
	}
}

// TestQuickCmdsContentEncryptedAtRest 覆盖全局与专属两张表：内容密文落盘、名称明文落盘、
// 内容原样往返（含重启）。
func TestQuickCmdsContentEncryptedAtRest(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 全局表：内容含密码 + 多行格式
	out := a.ok(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"name": "数据库", "content": trickyContent, "auto": true},
	}})
	if got := strOf(listOf(out, "commands")[0], "content"); got != trickyContent {
		t.Fatalf("保存的响应把内容改了：\n%q\n期望\n%q", got, trickyContent)
	}
	// 专属表
	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": sid, "commands": []any{
		map[string]any{"name": "专属", "content": "  ls -l\n", "auto": false},
	}})

	for _, scope := range []string{"", sid} {
		raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf(scope)))
		// 内容（含其中的密码）绝不能在文件里以明文出现
		for _, leak := range []string{"mysql -uroot", "pS3cret", "ls -l"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s 里出现了明文的命令内容 %q：\n%s", quickCmdsPathOf(scope), leak, raw)
			}
		}
		f := quickCmdsFileOf(t, a, scope)
		if f.Version != 2 {
			t.Errorf("%s 的文件版本 = %d，期望 2", quickCmdsPathOf(scope), f.Version)
		}
		if len(f.Commands) != 1 {
			t.Fatalf("%s 的指令数 = %d", quickCmdsPathOf(scope), len(f.Commands))
		}
		if f.Commands[0].Content == nil || f.Commands[0].Content.V != 1 || len(f.Commands[0].Content.CT) == 0 {
			t.Errorf("%s 的内容不是密文信封：%+v", quickCmdsPathOf(scope), f.Commands[0].Content)
		}
	}
	if !strings.Contains(readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf(""))), "数据库") {
		t.Error("全局表的名称应当是明文")
	}
	if !strings.Contains(readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf(sid))), "专属") {
		t.Error("专属表的名称应当是明文")
	}

	// 内容逐字节往返（同一进程内读回）
	out = a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if got := strOf(listOf(out, "commands")[0], "content"); got != trickyContent {
		t.Fatalf("读回的内容与原内容不一致：\n%q\n期望\n%q", got, trickyContent)
	}

	// 重启（同一把 master.key）后仍能解密，且内容不变
	b := a.restart(t)
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if got := strOf(listOf(out, "commands")[0], "content"); got != trickyContent {
		t.Fatalf("重启后读回的内容不一致：\n%q\n期望\n%q", got, trickyContent)
	}
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": sid})
	if got := strOf(listOf(out, "commands")[0], "content"); got != "  ls -l\n" {
		t.Fatalf("重启后专属表的内容不一致：%q", got)
	}
}

// writeLegacyQuickCmds 写一份第一轮形态（`content` 是明文）的表。
func writeLegacyQuickCmds(t *testing.T, a *testApp, sid string, entries ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"version": 1, "commands": entries})
	if err != nil {
		t.Fatalf("构造旧文件失败：%v", err)
	}
	dir := filepath.Join(a.renv.DataDir, "quickcmds")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(a.renv.DataDir, quickCmdsPathOf(sid)), body, 0o600); err != nil {
		t.Fatalf("写旧文件失败：%v", err)
	}
}

// legacyEntry 是一条第一轮形态的指令（明文 content）。
func legacyEntry(id, name, content string, auto bool) map[string]any {
	return map[string]any{"id": id, "name": name, "content": content, "auto": auto}
}

// TestQuickCmdsLegacyPlaintextFile 覆盖第一轮留下的明文文件：**读到就加密重写**，内容与格式不变。
func TestQuickCmdsLegacyPlaintextFile(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	writeLegacyQuickCmds(t, a, "", legacyEntry("legacy01", "旧表", trickyContent, true))

	// 读：老格式照常返回，内容逐字节不变（含前导/行尾空白与多行）
	out := a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	cmds := listOf(out, "commands")
	if len(cmds) != 1 {
		t.Fatalf("旧文件没读出来：%v", out)
	}
	if got := strOf(cmds[0], "content"); got != trickyContent {
		t.Fatalf("旧文件的内容 = %q，期望 %q", got, trickyContent)
	}
	if strOf(cmds[0], "id") != "legacy01" || strOf(cmds[0], "name") != "旧表" ||
		cmds[0].(map[string]any)["auto"] != true {
		t.Fatalf("旧文件的 id / 名称 / auto 被改了：%v", cmds[0])
	}

	// **仅仅读了一次**（没有任何保存）：磁盘上就已经没有明文了
	raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf("")))
	if strings.Contains(raw, "pS3cret") || strings.Contains(raw, "mysql -uroot") {
		t.Fatalf("读一次之后磁盘上还有明文命令：\n%s", raw)
	}
	f := quickCmdsFileOf(t, a, "")
	if f.Version != 2 || len(f.Commands) != 1 || f.Commands[0].ID != "legacy01" ||
		f.Commands[0].Content == nil || f.Commands[0].Auto != true {
		t.Fatalf("迁移后的文件不对：%+v", f)
	}

	// 重启后照样读得出来（密文是用同一把 master.key 写的）
	b := a.restart(t)
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if got := strOf(listOf(out, "commands")[0], "content"); got != trickyContent {
		t.Fatalf("重启后内容 = %q", got)
	}
}

// TestQuickCmdsLegacyMigrationAtStartup 覆盖「启动时扫一遍数据目录」：**没被打开过**的会话的表
// 也要迁移——前端只加载全局表与当前会话的表，只靠读取时迁移的话，别的会话的明文会一直躺着。
func TestQuickCmdsLegacyMigrationAtStartup(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 三张表：全局、一个真实会话的专属表、一个目录里存在但（此刻）没有会话配置的表
	writeLegacyQuickCmds(t, a, "", legacyEntry("g1", "全局旧表", trickyContent, true))
	writeLegacyQuickCmds(t, a, sid, legacyEntry("s1", "专属旧表", "  ls -l\n", true))
	writeLegacyQuickCmds(t, a, "orphan01", legacyEntry("o1", "孤儿表", "echo orphan\n", false))

	// 重启 = 一次新的 newAdminMux（迁移跑在启动路径上）；此刻**还没有任何接口被调用过**
	b := a.restart(t)
	for _, scope := range []string{"", sid, "orphan01"} {
		raw := readLocal(t, filepath.Join(b.renv.DataDir, quickCmdsPathOf(scope)))
		if strings.Contains(raw, "pS3cret") || strings.Contains(raw, "ls -l") || strings.Contains(raw, "echo orphan") {
			t.Errorf("%s 在启动后仍是明文：\n%s", quickCmdsPathOf(scope), raw)
		}
		f := quickCmdsFileOf(t, b, scope)
		if f.Version != 2 || len(f.Commands) != 1 || f.Commands[0].Content == nil {
			t.Errorf("%s 没有被迁移成密文：%+v", quickCmdsPathOf(scope), f)
		}
	}

	// 内容与 auto 逐字节保住（迁移只换信封，不动内容）
	out := b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if got := strOf(listOf(out, "commands")[0], "content"); got != trickyContent {
		t.Fatalf("迁移后全局表的内容 = %q，期望 %q", got, trickyContent)
	}
	if listOf(out, "commands")[0].(map[string]any)["auto"] != true {
		t.Error("迁移不该改掉 auto")
	}
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": sid})
	if got := strOf(listOf(out, "commands")[0], "content"); got != "  ls -l\n" {
		t.Fatalf("迁移后专属表的内容 = %q", got)
	}
}

// TestQuickCmdsLegacyEmptyContentBecomesPlaceholder 覆盖「明文内容没法保留」时的占位替换：
// 空内容存不下来（`Encrypt("")` 返回 nil），迁移会把它换成**加密后**的占位文案并关掉 auto
// ——磁盘上不留空内容、更不留明文；用户在界面上看到这句话，知道要重新填。
func TestQuickCmdsLegacyEmptyContentBecomesPlaceholder(t *testing.T) {
	a := newTestApp(t, appOptions{noSSH: true})
	writeLegacyQuickCmds(t, a, "", legacyEntry("empty01", "空内容", "", true))

	out := a.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	first := listOf(out, "commands")[0].(map[string]any)
	if got := strOf(first, "content"); got != quickCmdMigrationPlaceholder {
		t.Fatalf("空内容的迁移结果 = %q，期望占位文案", got)
	}
	if first["auto"] != false {
		t.Error("占位文案不该保留「点一下就自动回车」（那会把一句中文提示当命令跑掉）")
	}
	if strOf(first, "name") != "空内容" {
		t.Errorf("名称不该被改：%v", first)
	}

	// 磁盘上：有信封、没有明文占位文案
	raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf("")))
	if strings.Contains(raw, "命令加密失败已清空") {
		t.Fatalf("占位文案本身也必须密文落盘：\n%s", raw)
	}
	f := quickCmdsFileOf(t, a, "")
	if f.Commands[0].Content == nil || f.Commands[0].Auto != false {
		t.Fatalf("迁移后的文件不对：%+v", f.Commands[0])
	}

	// 重启后解出来的还是同一句占位（说明它真是密文，不是明文塞进去的）
	b := a.restart(t)
	out = b.ok(t, "/api/quickcmds/list", map[string]any{"sid": ""})
	if got := strOf(listOf(out, "commands")[0], "content"); got != quickCmdMigrationPlaceholder {
		t.Fatalf("重启后内容 = %q", got)
	}
}

// TestQuickCmdsDecryptFailureIsReported 覆盖三类「密文不可信」：字节被改、密文在表内互换、
// 会话专属的文件被搬成全局文件。三种情况都必须是**明确的错误**（internal + 原因），
// 绝不能静默返回空内容——那会让用户以为指令没了，而一保存就把服务端那份覆盖掉。
func TestQuickCmdsDecryptFailureIsReported(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"id": "aaa", "name": "甲", "content": "echo 甲", "auto": false},
		map[string]any{"id": "bbb", "name": "乙", "content": "echo 乙", "auto": false},
	}})

	// ① 密文被改了一个字节（GCM 认证失败）
	rewriteQuickCmdsFile(t, a, "", func(f *quickCmdsFile) {
		ct := []byte(f.Commands[0].Content.CT)
		mid := len(ct) / 2
		if ct[mid] == 'A' {
			ct[mid] = 'B'
		} else {
			ct[mid] = 'A'
		}
		f.Commands[0].Content.CT = string(ct)
	})
	assertQuickCmdsDecryptError(t, a, "", "甲")

	// ② 同一张表里两条指令的密文互换（AAD 绑定了指令 id，所以解不开）
	//    先按正确的内容重写一份，再互换
	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": "", "commands": []any{
		map[string]any{"id": "aaa", "name": "甲", "content": "echo 甲", "auto": false},
		map[string]any{"id": "bbb", "name": "乙", "content": "echo 乙", "auto": false},
	}})
	rewriteQuickCmdsFile(t, a, "", func(f *quickCmdsFile) {
		f.Commands[0].Content, f.Commands[1].Content = f.Commands[1].Content, f.Commands[0].Content
	})
	assertQuickCmdsDecryptError(t, a, "", "甲")

	// ③ 会话专属的表（AAD 的 sid = 会话 id）改名成 global.json：同样解不开
	a.ok(t, "/api/quickcmds/save", map[string]any{"sid": sid, "commands": []any{
		map[string]any{"name": "专属", "content": "echo 专属", "auto": false},
	}})
	os.Remove(filepath.Join(a.renv.DataDir, quickCmdsPathOf("")))
	raw := readLocal(t, filepath.Join(a.renv.DataDir, quickCmdsPathOf(sid)))
	if err := os.WriteFile(filepath.Join(a.renv.DataDir, quickCmdsPathOf("")), []byte(raw), 0o600); err != nil {
		t.Fatalf("搬文件失败：%v", err)
	}
	assertQuickCmdsDecryptError(t, a, "", "专属")
}

// assertQuickCmdsDecryptError 断言读这张表会明确报「解密失败」，并带上出问题的指令名。
func assertQuickCmdsDecryptError(t *testing.T, a *testApp, sid, wantName string) {
	t.Helper()
	out := a.fail(t, "/api/quickcmds/list", map[string]any{"sid": sid}, "internal", 0)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "解密") || !strings.Contains(msg, wantName) {
		t.Fatalf("错误文案没说清是哪条指令解密失败：%q", msg)
	}
}
