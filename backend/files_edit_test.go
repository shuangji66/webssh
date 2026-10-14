package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"

	"webssh/backend/internal/testssh"
)

// ---------------------------------------------------------------------------
// 第十六轮：文件编辑（/api/fs/edit-load、/api/fs/edit-save，契约第 26 节）
//
// 三条要守住的东西：
//  1. **没权限就进不去编辑页**：edit-load 在读到内容之前先以登录用户的身份打开一次
//     （O_WRONLY），失败就回 permission——界面据此拒绝进入编辑页；
//  2. **原样写回**：UTF-16 还是 UTF-16、带 BOM 还是带 BOM、CRLF 还是 CRLF；
//  3. 不该编辑的东西（目录、设备、二进制、超大文件）一开始就说清原因。
// ---------------------------------------------------------------------------

// editLoad 调 edit-load 并返回响应体。
func editLoad(t *testing.T, a *testApp, sid, path string) map[string]any {
	t.Helper()
	return a.ok(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": path})
}

// editSave 调 edit-save（把 edit-load 拿到的编码元信息原样带回去，模拟前端的用法）。
func editSave(t *testing.T, a *testApp, sid string, loaded map[string]any, content string) map[string]any {
	t.Helper()
	return a.ok(t, "/api/fs/edit-save", map[string]any{
		"sid":      sid,
		"path":     strOf(loaded, "path"),
		"content":  content,
		"encoding": strOf(loaded, "encoding"),
		"bom":      loaded["bom"] == true,
		"eol":      strOf(loaded, "eol"),
	})
}

func readLocalBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	return b
}

// TestFsEditRoundTrip 覆盖「打开 → 改 → 保存」在四种编码/换行下的保真。
func TestFsEditRoundTrip(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	cases := []struct {
		name string
		// 原始字节与它在编辑区里应当显示成什么
		raw, text string
		encoding  string
		bom       bool
		eol       string
	}{
		{"plain.conf", "key=value\n", "key=value\n", encodingUTF8, false, eolLF},
		{"bom.conf", "\xef\xbb\xbfkey=value\n", "key=value\n", encodingUTF8, true, eolLF},
		{"crlf.ini", "[core]\r\nname=value\r\n", "[core]\r\nname=value\r\n", encodingUTF8, false, eolCRLF},
	}
	for _, tc := range cases {
		p := filepath.Join(root, tc.name)
		if err := os.WriteFile(p, []byte(tc.raw), 0o644); err != nil {
			t.Fatalf("写 %s 失败：%v", tc.name, err)
		}
		out := editLoad(t, a, sid, p)
		if strOf(out, "content") != tc.text {
			t.Errorf("%s 的 content = %q，期望 %q", tc.name, strOf(out, "content"), tc.text)
		}
		if strOf(out, "encoding") != tc.encoding || out["bom"] != tc.bom || strOf(out, "eol") != tc.eol {
			t.Errorf("%s 的元信息不对：%v", tc.name, out)
		}
		// 内容里多一行，保存之后字节必须仍然是原来的编码 / BOM / 换行
		edited := strings.Replace(tc.text, "value", "edited", 1)
		if !strings.HasSuffix(edited, "\n") {
			edited += "\n"
		}
		editSave(t, a, sid, out, edited)

		want := []byte(strings.Replace(tc.raw, "value", "edited", 1))
		// CRLF 那一条：编辑区里是 LF，写回时应当还原成 CRLF
		if tc.eol == eolCRLF {
			want = []byte(strings.ReplaceAll(strings.Replace(tc.raw, "value", "edited", 1), "\r\n", "\n"))
			want = []byte(strings.ReplaceAll(string(want), "\n", "\r\n"))
		}
		if got := readLocalBytes(t, p); !bytes.Equal(got, want) {
			t.Errorf("%s 保存后的字节 = %q，期望 %q", tc.name, got, want)
		}
		// 再打开一次：内容应当是刚写进去的那份（编码也还是原样）
		again := editLoad(t, a, sid, p)
		if strOf(again, "content") != edited || strOf(again, "encoding") != tc.encoding || again["bom"] != tc.bom {
			t.Errorf("%s 重新打开 = %v", tc.name, again)
		}
	}
}

// TestFsEditUTF16 覆盖 UTF-16 LE / BE（带与不带 BOM）：编辑 Windows 上的 .ini 是最常见的用法。
func TestFsEditUTF16(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot
	const ini = "[core]\r\nname=值\r\n"

	for _, tc := range []struct {
		name      string
		bigEndian bool
		bom       bool
		encoding  string
	}{
		{"le-bom.ini", false, true, encodingUTF16LE},
		{"be-bom.ini", true, true, encodingUTF16BE},
		{"le-nobom.ini", false, false, encodingUTF16LE},
	} {
		p := filepath.Join(root, tc.name)
		if err := os.WriteFile(p, utf16Bytes(t, ini, tc.bigEndian, tc.bom), 0o644); err != nil {
			t.Fatalf("写 %s 失败：%v", tc.name, err)
		}
		out := editLoad(t, a, sid, p)
		if strOf(out, "content") != ini {
			t.Fatalf("%s content = %q，期望 %q", tc.name, strOf(out, "content"), ini)
		}
		if strOf(out, "encoding") != tc.encoding || out["bom"] != tc.bom || strOf(out, "eol") != eolCRLF {
			t.Fatalf("%s 元信息不对：%v", tc.name, out)
		}
		editSave(t, a, sid, out, strings.Replace(ini, "值", "新值", 1))
		want := utf16Bytes(t, strings.Replace(ini, "值", "新值", 1), tc.bigEndian, tc.bom)
		if got := readLocalBytes(t, p); !bytes.Equal(got, want) {
			t.Errorf("%s 保存后 = % x，期望 % x", tc.name, got, want)
		}
	}
}

// TestFsEditCRLFNormalisation 校验「编辑区里是 LF、原文件是 CRLF」时不会写出 `\r\r\n`。
func TestFsEditCRLFNormalisation(t *testing.T) {
	a, sid := fsTestApp(t)
	p := filepath.Join(a.sshRoot, "mixed.conf")
	if err := os.WriteFile(p, []byte("a\r\nb\r\n"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	out := editLoad(t, a, sid, p)
	// 前端 textarea 里粘进来的内容可能两种换行都有：保存时先归一化再按原风格写回
	editSave(t, a, sid, out, "a\r\nb\nc\n")
	if got := string(readLocalBytes(t, p)); got != "a\r\nb\r\nc\r\n" {
		t.Errorf("保存后 = %q，期望 CRLF 且没有多余的回车", got)
	}
}

// permEnforcingDir 找一个**真的会执行权限位**的目录来放测试文件。
//
// 探针必须「先建文件、再 chmod」：在飞牛这类卷上，按创建时的 mode 建的位会被改写
// （实测 WriteFile(…, 0o444) 落地成 0705，属主照样写得进去），只有显式 chmod 才会落到
// mode 位上。所以这里写一个文件、chmod 0444、再以 O_WRONLY 打开一次，被拒才算数；
// 所有候选都探不出来就返回空串，由调用方 skip（**不要**假装通过）。
func permEnforcingDir(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{os.TempDir(), "/tmp", "/var/tmp"} {
		probe := filepath.Join(dir, fmt.Sprintf("webssh-perm-probe-%d", os.Getpid()))
		if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
			continue
		}
		enforced := false
		if err := os.Chmod(probe, 0o444); err == nil {
			f, err := os.OpenFile(probe, os.O_WRONLY, 0)
			enforced = err != nil
			if err == nil {
				_ = f.Close()
			}
		}
		_ = os.Remove(probe)
		if enforced {
			return dir
		}
	}
	return ""
}

// TestFsEditPermissionDenied 是这一轮的核心断言：没有写权限时**进不去编辑页**（permission），
// 而且内容一个字节都不会给出去；保存同样要拦住（不能只靠界面不进入编辑页）。
//
// 权限位在两种环境下不生效，直接 skip 并写明原因（**不要**假装通过）：以 root 跑测试、
// 或文件系统自己改写 mode（飞牛这类卷）。
func TestFsEditPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行：权限位挡不住写入，跳过")
	}
	dir := permEnforcingDir(t)
	if dir == "" {
		t.Skip("找不到会执行权限位的文件系统：无法模拟「没有权限」，跳过")
	}
	root, err := os.MkdirTemp(dir, "webssh-edit-perm-")
	if err != nil {
		t.Fatalf("建临时目录失败：%v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// SSH 服务器的根目录换到这个会执行权限位的地方（其余照旧走内嵌服务器）
	a := newTestApp(t, appOptions{sshOptions: &testssh.Options{User: "demo", Password: "demo", Root: root}})
	sid := a.newSSHSession(t)

	p := filepath.Join(root, "readonly.conf")
	if err := os.WriteFile(p, []byte("key=value\n"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	// 显式 chmod 才落到 mode 位（理由见 permEnforcingDir），并确认这个文件**真的**写不进去：
	// 探针通过不等于这一份也通过（卷可能在别处放宽）。
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatalf("chmod 失败：%v", err)
	}
	if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
		_ = f.Close()
		t.Skip("这个文件仍然可写：无法模拟「没有权限」，跳过")
	}

	out := a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": p}, "permission", http.StatusForbidden)
	if strings.Contains(strOf(out, "error"), "key=value") {
		t.Errorf("被拒绝时不该把文件内容带出来：%v", out)
	}
	a.fail(t, "/api/fs/edit-save", map[string]any{
		"sid": sid, "path": p, "content": "hacked\n", "encoding": encodingUTF8, "eol": eolLF,
	}, "permission", http.StatusForbidden)
	if got := string(readLocalBytes(t, p)); got != "key=value\n" {
		t.Errorf("被拒绝的保存不该改动文件：%q", got)
	}
}

// TestEditErrorMapping 单测错误映射：权限类失败必须回 permission（前端据此「拒绝修改」），
// 而不是 internal。SFTP 的权限拒绝由 pkg/sftp 归一成 os.ErrPermission（client.go 的
// normaliseError），只读文件系统则只剩消息可认——两条路都要认出来。
func TestEditErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"os.ErrPermission", os.ErrPermission, "permission"},
		{"只读文件系统的消息", errors.New("sftp: \"Read-only file system\" (SSH_FX_FAILURE)"), "permission"},
		{"不是普通文件", errEditNotRegular, "invalid"},
		{"不存在", os.ErrNotExist, "not_found"},
	}
	for _, tc := range cases {
		if got := editError("打开编辑器失败", tc.err); got.Code != tc.code {
			t.Errorf("%s → %s，期望 %s", tc.name, got.Code, tc.code)
		}
	}
}

// TestFsEditRejects 覆盖「不该编辑的东西」与参数错误：目录、设备、二进制、超大文件、坏编码。
func TestFsEditRejects(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	// 目录
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": filepath.Join(root, "docs")}, "invalid", http.StatusBadRequest)
	// 不存在
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": filepath.Join(root, "nope.conf")}, "not_found", http.StatusNotFound)
	// 路径逃逸
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": root + "/../etc/passwd"}, "invalid", http.StatusBadRequest)
	// 二进制（GBK 字节：判非文本）
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": filepath.Join(root, "binary.bin")}, "invalid", http.StatusBadRequest)
	// 不是普通文件（FIFO）：以 O_WRONLY 打开它会阻塞，必须在这一道就拦住
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Logf("建 FIFO 失败（跳过这一条）：%v", err)
	} else {
		a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": fifo}, "invalid", http.StatusBadRequest)
	}

	// 超过编辑上限（4 MiB）：一开始就说明原因，而不是让浏览器去扛一个巨大的 textarea
	big := filepath.Join(root, "big.conf")
	if err := os.WriteFile(big, bytes.Repeat([]byte("a"), int(maxReadBytes)+1), 0o644); err != nil {
		t.Fatalf("写大文件失败：%v", err)
	}
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": sid, "path": big}, "too_large", http.StatusRequestEntityTooLarge)

	// 保存：内容超过上限、编码不认识、文件不存在
	small := filepath.Join(root, "small.conf")
	if err := os.WriteFile(small, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	a.fail(t, "/api/fs/edit-save", map[string]any{
		"sid": sid, "path": small, "content": strings.Repeat("a", int(maxReadBytes)+1),
		"encoding": encodingUTF8, "eol": eolLF,
	}, "too_large", http.StatusRequestEntityTooLarge)
	a.fail(t, "/api/fs/edit-save", map[string]any{
		"sid": sid, "path": small, "content": "y\n", "encoding": "gb18030", "eol": eolLF,
	}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/edit-save", map[string]any{
		"sid": sid, "path": filepath.Join(root, "nope.conf"), "content": "y\n",
		"encoding": encodingUTF8, "eol": eolLF,
	}, "not_found", http.StatusNotFound)
	// 缺 path
	a.fail(t, "/api/fs/edit-save", map[string]any{"sid": sid, "content": "y\n"}, "invalid", http.StatusBadRequest)
	// 会话不存在
	a.fail(t, "/api/fs/edit-load", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "path": small}, "session_gone", http.StatusNotFound)
}

// TestEncodeForSave 单测编码器：UTF-16 的字节序、BOM、以及「认不出的编码必须报错而不是当 UTF-8 写」。
func TestEncodeForSave(t *testing.T) {
	le, err := encodeForSave("a值\n", encodingUTF16LE, true, eolLF)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	wantLE := []byte{0xff, 0xfe}
	for _, unit := range utf16.Encode([]rune("a值\n")) {
		wantLE = append(wantLE, byte(unit), byte(unit>>8))
	}
	if !bytes.Equal(le, wantLE) {
		t.Errorf("UTF-16LE = % x，期望 % x", le, wantLE)
	}

	be, err := encodeForSave("a值\n", encodingUTF16BE, false, eolLF)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	var wantBE []byte
	for _, unit := range utf16.Encode([]rune("a值\n")) {
		wantBE = append(wantBE, byte(unit>>8), byte(unit))
	}
	if !bytes.Equal(be, wantBE) {
		t.Errorf("UTF-16BE 无 BOM = % x，期望 % x", be, wantBE)
	}

	// CRLF：先归一化再替换，不会出现 \r\r\n
	crlf, err := encodeForSave("a\r\nb\n", encodingUTF8, false, eolCRLF)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if string(crlf) != "a\r\nb\r\n" {
		t.Errorf("CRLF 归一化 = %q", crlf)
	}

	// 空编码（前端没带）= UTF-8：老客户端只发 content 时也应当能存
	plain, err := encodeForSave("a\n", "", false, eolLF)
	if err != nil || string(plain) != "a\n" {
		t.Errorf("空编码 = %q, %v", plain, err)
	}

	if _, err := encodeForSave("a\n", "gb18030", false, eolLF); err == nil {
		t.Error("不认识的编码必须报错，不能当成 UTF-8 写下去")
	}
}

// TestDetectEOL 覆盖换行判定（多数票 + 无换行时的默认值）。
func TestDetectEOL(t *testing.T) {
	cases := map[string]string{
		"a\nb\n":        eolLF,
		"a\r\nb\r\n":    eolCRLF,
		"a\r\nb\r\nc":   eolCRLF,
		"single":        eolLF,
		"a\r\nb\nc\n":   eolLF, // 多数票：LF 2 票
		"a\r\nb\r\nc\n": eolCRLF,
	}
	for text, want := range cases {
		if got := detectEOL(text); got != want {
			t.Errorf("detectEOL(%q) = %q，期望 %q", text, got, want)
		}
	}
}

// TestFsEditLargeSaveRoundTrip 钉住「能打开的就能保存」：1–4 MiB 的文件必须能改完存回去。
//
// 为什么要有这条：「编辑保存」的 body 里装的是编辑区**文本**（JSON 字符串），换行/引号会被
// 转义成两个字节，而它以前和别的接口共用 1 MiB 的 body 上限（maxJSONBytes）——于是
// 2 MiB 的文件能打开、却永远存不回去（用户只看到一句「请求体超过上限」，
// 而 files_edit.go 里那条「保存的内容超过 maxReadBytes」的判断根本没机会执行）。
// 下面这条用例同时还钉住了「超限的错误是从保存内容那一道报出来的」。
func TestFsEditLargeSaveRoundTrip(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	// 2 MiB 的「每行都有内容」文本：既超过旧的 1 MiB body 上限，又远小于 4 MiB 的编辑上限，
	// 而且换行密集（JSON 转义会把它放大到 ~2.1 MB），正是旧实现在第一行就拒绝的形态。
	var sb strings.Builder
	for i := 0; sb.Len() < 2<<20; i++ {
		fmt.Fprintf(&sb, "key%05d=value\n", i)
	}
	content := sb.String()
	big := filepath.Join(root, "large.conf")
	if err := os.WriteFile(big, []byte(content), 0o644); err != nil {
		t.Fatalf("写大文件失败：%v", err)
	}

	loaded := editLoad(t, a, sid, big)
	if strOf(loaded, "content") != content {
		t.Fatalf("大文件读回来的内容不完整：%d 字节 vs %d 字节", len(strOf(loaded, "content")), len(content))
	}
	// 改一行再存：a.ok 内部在失败时直接 Fail，所以这条断言就是「保存真的成功了」
	edited := strings.Replace(content, "key00000=value", "key00000=edited", 1)
	editSave(t, a, sid, loaded, edited)
	if got := string(readLocalBytes(t, big)); got != edited {
		t.Fatalf("大文件保存后的内容不对（长度 %d vs %d）", len(got), len(edited))
	}

	// 超过编辑上限（4 MiB）仍然回 too_large，而且必须是从「保存的内容」这一道报出来的
	// （body 上限已经放到 8 MiB+，所以这次真的走得到那条判断）。
	out := a.fail(t, "/api/fs/edit-save", map[string]any{
		"sid": sid, "path": big, "content": strings.Repeat("a", int(maxReadBytes)+1),
		"encoding": encodingUTF8, "eol": eolLF,
	}, "too_large", http.StatusRequestEntityTooLarge)
	if msg := strOf(out, "error"); !strings.Contains(msg, "保存的内容") {
		t.Errorf("超限错误不是从「保存的内容」这一道报出来的：%q", msg)
	}
}
