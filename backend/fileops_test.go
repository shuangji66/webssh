package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"webssh/backend/internal/testssh"
)

// ---------------------------------------------------------------------------
// 复制 / 移动 / 压缩 / 解压 / 导出至飞牛（契约 11.4 / 11.5）的集成测试
//
// 全部走真实链路：真 SSH + 真 SFTP + 真的在「远端」（内嵌服务器跑在本机文件系统上，
// 所以远端路径就是本机路径）跑 tar / unzip。压缩解压走 exec 通道而不申请 PTY，
// 因此不依赖 /dev/ptmx（见 testssh 的 startPipes 说明）。
// ---------------------------------------------------------------------------

// fsOpApp 起后端 + 文件树，返回 (app, sid, 目标目录)。
func fsOpApp(t *testing.T, opts appOptions) (*testApp, string, string) {
	t.Helper()
	a := newTestApp(t, opts)
	root := a.sshRoot
	writeLocal(t, filepath.Join(root, "hello.txt"), "hello world\n")
	if err := os.MkdirAll(filepath.Join(root, "docs", "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(root, "docs", "readme.md"), "# readme\n")
	writeLocal(t, filepath.Join(root, "docs", "sub", "deep.txt"), "deep\n")
	if err := os.Symlink("hello.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("建目标目录失败：%v", err)
	}
	sid := a.newSSHSession(t)
	return a, sid, dest
}

// TestFsCopy 覆盖远端内部复制：递归、软链接、绝不覆盖、同名跳过、参数校验。
func TestFsCopy(t *testing.T) {
	a, sid, dest := fsOpApp(t, appOptions{})
	root := a.sshRoot
	hello := filepath.Join(root, "hello.txt")

	// 单文件
	out := a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": dest})
	files := listOf(out, "files")
	if len(files) != 1 || numOf(out, "skipped") != 0 {
		t.Fatalf("复制结果 = %v", out)
	}
	item, _ := files[0].(map[string]any)
	if strOf(item, "path") != filepath.Join(dest, "hello.txt") || numOf(item, "size") != 12 {
		t.Fatalf("复制出的条目 = %v", item)
	}
	if got := readLocal(t, filepath.Join(dest, "hello.txt")); got != "hello world\n" {
		t.Fatalf("复制的内容不对：%q", got)
	}

	// 同名一律跳过，**绝不覆盖**（先改一下目标文件，再复制一次，内容必须原样）
	writeLocal(t, filepath.Join(dest, "hello.txt"), "changed-by-user")
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": dest})
	if numOf(out, "skipped") != 1 || len(listOf(out, "files")) != 0 {
		t.Fatalf("同名应当跳过：%v", out)
	}
	if got := readLocal(t, filepath.Join(dest, "hello.txt")); got != "changed-by-user" {
		t.Fatalf("同名被覆盖了：%q", got)
	}
	// 复制进源自己所在的目录 = 目标就是自己 → 跳过而不是报错
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": root})
	if numOf(out, "skipped") != 1 {
		t.Fatalf("复制到自己所在目录应当跳过：%v", out)
	}

	// 目录递归：size 是整棵树的字节数
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}, "toDir": dest})
	item, _ = listOf(out, "files")[0].(map[string]any)
	if strOf(item, "path") != filepath.Join(dest, "docs") {
		t.Fatalf("目录复制结果 = %v", item)
	}
	if want := float64(len("# readme\n") + len("deep\n")); numOf(item, "size") != want {
		t.Errorf("目录复制的字节数 = %v，期望 %v", item["size"], want)
	}
	if got := readLocal(t, filepath.Join(dest, "docs", "sub", "deep.txt")); got != "deep\n" {
		t.Errorf("深层文件没复制到：%q", got)
	}
	// 源目录里再复制一次 → 整个目录跳过
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}, "toDir": dest})
	if numOf(out, "skipped") != 1 {
		t.Fatalf("同名目录应当跳过：%v", out)
	}

	// 软链接：复制的是链接本身（不跟随），指向关系保持不变。
	//
	// 目标写成相对或绝对两种形式都接受：pkg/sftp 的**服务端**实现会把相对目标拼成绝对路径
	// （server.go 的 Symlink 对 Targetpath 也调了 toLocalPath），真实 OpenSSH 的
	// sftp-server 则原样传递。这里要验的是「没有跟随链接去复制内容」，两种写法都证明得了。
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "link.txt")}, "toDir": dest})
	if len(listOf(out, "files")) != 1 {
		t.Fatalf("复制软链接结果 = %v", out)
	}
	copiedLink := filepath.Join(dest, "link.txt")
	if fi, err := os.Lstat(copiedLink); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("复制出来的应当仍是软链接：%v %v", fi, err)
	}
	target, err := os.Readlink(copiedLink)
	if err != nil || !(target == "hello.txt" || strings.HasSuffix(target, "/hello.txt")) {
		t.Fatalf("软链接没被原样复制：target=%q err=%v", target, err)
	}

	// 同一路径写两遍只处理一次
	out = a.ok(t, "/api/fs/copy", map[string]any{
		"sid": sid, "paths": []string{filepath.Join(root, "docs", "readme.md"), filepath.Join(root, "docs", "readme.md")}, "toDir": dest,
	})
	if len(listOf(out, "files")) != 1 {
		t.Fatalf("重复路径应当去重：%v", out)
	}

	// 相对路径相对家目录解析
	if err := os.MkdirAll(filepath.Join(root, "dest-link"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	out = a.ok(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{"link.txt"}, "toDir": "dest-link"})
	item, _ = listOf(out, "files")[0].(map[string]any)
	if strOf(item, "path") != filepath.Join(root, "dest-link", "link.txt") {
		t.Fatalf("相对路径没按家目录解析：%v", item)
	}

	// 参数校验
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{}, "toDir": dest}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "nope")}, "toDir": dest}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": ""}, "invalid", 0)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": hello}, "invalid", 0)
	// 「复制到它自己内部」会无限递归，必须挡住
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{root}, "toDir": dest}, "invalid", 0)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{"/"}, "toDir": dest}, "invalid", 0)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{root + "/../etc"}, "toDir": dest}, "invalid", 0)
	// 会话未打开 / 不存在
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	a.fail(t, "/api/fs/copy", map[string]any{"sid": sid, "paths": []string{hello}, "toDir": dest}, "not_open", http.StatusConflict)
	a.fail(t, "/api/fs/copy", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "paths": []string{hello}, "toDir": dest}, "session_gone", http.StatusNotFound)
}

// TestFsMove 覆盖远端内部移动：Rename、同名跳过、目录、参数校验。
func TestFsMove(t *testing.T) {
	a, sid, dest := fsOpApp(t, appOptions{})
	root := a.sshRoot

	// 文件移动
	writeLocal(t, filepath.Join(root, "moveme.txt"), "move me")
	out := a.ok(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "moveme.txt")}, "toDir": dest})
	files := listOf(out, "files")
	if len(files) != 1 || numOf(out, "skipped") != 0 {
		t.Fatalf("移动结果 = %v", out)
	}
	item, _ := files[0].(map[string]any)
	if strOf(item, "path") != filepath.Join(dest, "moveme.txt") || numOf(item, "size") != float64(len("move me")) {
		t.Fatalf("移动后的条目 = %v", item)
	}
	if _, err := os.Stat(filepath.Join(root, "moveme.txt")); !os.IsNotExist(err) {
		t.Fatalf("源文件应当已经不在：%v", err)
	}
	if got := readLocal(t, filepath.Join(dest, "moveme.txt")); got != "move me" {
		t.Fatalf("移动后的内容不对：%q", got)
	}
	// 源已经不存在 → not_found
	a.fail(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "moveme.txt")}, "toDir": dest}, "not_found", 0)

	// 同名绝不覆盖：SFTP 的 Rename 是 POSIX rename（会直接盖掉），所以必须由我们查一次
	writeLocal(t, filepath.Join(root, "clash.txt"), "from-source")
	writeLocal(t, filepath.Join(dest, "clash.txt"), "at-dest")
	out = a.ok(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "clash.txt")}, "toDir": dest})
	if numOf(out, "skipped") != 1 || len(listOf(out, "files")) != 0 {
		t.Fatalf("同名应当跳过：%v", out)
	}
	if got := readLocal(t, filepath.Join(dest, "clash.txt")); got != "at-dest" {
		t.Fatalf("同名被覆盖了：%q", got)
	}
	if got := readLocal(t, filepath.Join(root, "clash.txt")); got != "from-source" {
		t.Fatalf("被跳过的源不该被删：%q", got)
	}

	// 目录移动（整棵树换位置）
	out = a.ok(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}, "toDir": dest})
	item, _ = listOf(out, "files")[0].(map[string]any)
	if strOf(item, "path") != filepath.Join(dest, "docs") {
		t.Fatalf("目录移动结果 = %v", item)
	}
	if _, err := os.Stat(filepath.Join(root, "docs")); !os.IsNotExist(err) {
		t.Fatalf("源目录应当已经不在：%v", err)
	}
	if got := readLocal(t, filepath.Join(dest, "docs", "sub", "deep.txt")); got != "deep\n" {
		t.Fatalf("目录移动后内容不对：%q", got)
	}

	// 参数校验
	a.fail(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{}, "toDir": dest}, "invalid", 0)
	// 「移动到它已经在的目录」= 目标就是自己 → 跳过（用户可能只是把整批文件粘回原处）
	out = a.ok(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "hello.txt")}, "toDir": root})
	if numOf(out, "skipped") != 1 {
		t.Fatalf("移动到自身所在目录应当跳过：%v", out)
	}
	a.fail(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{root}, "toDir": dest}, "invalid", 0)
	a.fail(t, "/api/fs/move", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "hello.txt")}, "toDir": filepath.Join(root, "nope")}, "not_found", 0)
}

// TestFsCopyTransferLimit 校验单次搬运的总字节上限：超限报 too_large，且**不留半成品**。
func TestFsCopyTransferLimit(t *testing.T) {
	old := maxTransferBytes
	maxTransferBytes = 4 // 比 hello.txt（12 字节）小
	t.Cleanup(func() { maxTransferBytes = old })

	a, sid, dest := fsOpApp(t, appOptions{})
	out := a.fail(t, "/api/fs/copy", map[string]any{
		"sid": sid, "paths": []string{filepath.Join(a.sshRoot, "hello.txt")}, "toDir": dest,
	}, "too_large", http.StatusRequestEntityTooLarge)
	if !strings.Contains(strOf(out, "error"), "上限") {
		t.Errorf("错误文案应当说明上限：%v", out)
	}
	if _, err := os.Stat(filepath.Join(dest, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("超限后不该留下半截文件：%v", err)
	}
}

// TestFsCompressAndExtract 覆盖压缩（产物真落在 archiveDir、能被 tar 列出）与解压。
func TestFsCompressAndExtract(t *testing.T) {
	a, sid, _ := fsOpApp(t, appOptions{})
	root := a.sshRoot
	archiveDir := a.renv.ArchiveDir

	// 1) 单目录压缩：-C <父目录> <名字>，包里的成员就是选中的那个名字
	out := a.ok(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}})
	archive, _ := out["archive"].(map[string]any)
	archivePath := strOf(archive, "path")
	if strOf(archive, "dir") != archiveDir {
		t.Fatalf("archive.dir = %v，期望 %v", archive["dir"], archiveDir)
	}
	if !strings.HasPrefix(archivePath, archiveDir+"/") || !strings.HasSuffix(archivePath, ".tar.gz") {
		t.Fatalf("archive.path = %q", archivePath)
	}
	if !strings.Contains(filepath.Base(archivePath), "webssh-") {
		t.Errorf("缺省名字应当是 webssh-<时间戳>：%q", archivePath)
	}
	if fi, err := os.Stat(archivePath); err != nil {
		t.Fatalf("压缩包不存在：%v", err)
	} else if fi.Size() == 0 || numOf(archive, "size") != float64(fi.Size()) {
		t.Fatalf("size 与实际不符：%v vs %d", archive["size"], fi.Size())
	}
	names := listTarGz(t, archivePath)
	if !containsStr(names, "docs/readme.md") || !containsStr(names, "docs/sub/deep.txt") {
		t.Fatalf("压缩包里的成员不对：%v", names)
	}

	// 2) 多路径 + 多父目录：退回 -C / + 绝对路径（成员名带各自层级）
	other := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(other, "two.txt"), "two\n")
	out = a.ok(t, "/api/fs/compress", map[string]any{
		"sid": sid, "name": "multi",
		"paths": []string{filepath.Join(root, "hello.txt"), filepath.Join(other, "two.txt")},
	})
	archive, _ = out["archive"].(map[string]any)
	multiPath := strOf(archive, "path")
	if filepath.Base(multiPath) != "multi.tar.gz" {
		t.Fatalf("指定的名字没生效：%q", multiPath)
	}
	names = listTarGz(t, multiPath)
	want := []string{strings.TrimPrefix(filepath.Join(root, "hello.txt"), "/"), strings.TrimPrefix(filepath.Join(other, "two.txt"), "/")}
	for _, w := range want {
		if !containsStr(names, w) {
			t.Errorf("压缩包缺少成员 %q（实际 %v）", w, names)
		}
	}

	// 3) 参数校验：名字里的路径分隔符与控制字符、空 paths、不存在的源
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}, "name": "../escape"}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "docs")}, "name": "a/b"}, "invalid", 0)
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{}}, "invalid", 0)
	// 条目数上限（畸形请求：几千条路径靠请求体上限是挡不住的）
	many := make([]string, 0, maxCompressItems+1)
	for i := 0; i <= maxCompressItems; i++ {
		many = append(many, filepath.Join(root, "hello.txt"))
	}
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": many}, "invalid", 0)
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(root, "nope")}}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{"/"}}, "invalid", 0)
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{root + "/../etc"}}, "invalid", 0)

	// 4) 解压 tar.gz：**原位**解压（压缩包所在目录 = archiveDir）
	out = a.ok(t, "/api/fs/extract", map[string]any{"sid": sid, "path": archivePath})
	if strOf(out, "dir") != archiveDir {
		t.Fatalf("解压根目录 = %v，期望 %v", out["dir"], archiveDir)
	}
	if got := readLocal(t, filepath.Join(archiveDir, "docs", "sub", "deep.txt")); got != "deep\n" {
		t.Fatalf("解压出来的内容不对：%q", got)
	}

	// 5) 解压 zip（系统没有 unzip 时跳过这一小段，见 i18n 的 err_no_archive_tool）
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Log("本机没有 unzip，跳过 zip 解压用例")
	} else {
		zipPath := filepath.Join(root, "bundle.zip")
		writeZip(t, zipPath, map[string]string{"inner/one.txt": "one\n"})
		out = a.ok(t, "/api/fs/extract", map[string]any{"sid": sid, "path": zipPath})
		if strOf(out, "dir") != root {
			t.Fatalf("zip 解压根目录 = %v，期望 %v", out["dir"], root)
		}
		if got := readLocal(t, filepath.Join(root, "inner", "one.txt")); got != "one\n" {
			t.Fatalf("zip 解压出来内容不对：%q", got)
		}
	}

	// 5b) 压缩包坏了：tar 的 stderr 关键行必须回到错误文案里，而不是一句「内部错误」
	broken := filepath.Join(root, "broken.tar.gz")
	writeLocal(t, broken, "this is not a gzip stream")
	out = a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": broken}, "internal", http.StatusInternalServerError)
	if !strings.Contains(strOf(out, "error"), "gzip") {
		t.Errorf("损坏压缩包的错误文案应当带上远端命令的输出：%v", out)
	}

	// 6) 不支持的格式 / 目录 / 不存在
	writeLocal(t, filepath.Join(root, "notes.txt"), "not an archive")
	a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": filepath.Join(root, "notes.txt")}, "archive_unsupported", http.StatusBadRequest)
	// .gz 不是 tar.gz：只有 tar 家族与 zip 认
	writeLocal(t, filepath.Join(root, "single.gz"), "x")
	a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": filepath.Join(root, "single.gz")}, "archive_unsupported", 0)
	a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": filepath.Join(root, "elsewhere")}, "invalid", 0)
	a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": filepath.Join(root, "nope.tar.gz")}, "not_found", 0)
}

// TestFsCompressSizeLimit 校验压缩产物的体积上限：超限要报 too_large 并删掉产物
// （远端 /tmp 通常很小，留一个巨大的包会把整台主机的临时空间吃掉）。
func TestFsCompressSizeLimit(t *testing.T) {
	old := maxTransferBytes
	maxTransferBytes = 4 // 比任何一个 tar.gz 都小
	t.Cleanup(func() { maxTransferBytes = old })

	a, sid, _ := fsOpApp(t, appOptions{})
	out := a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(a.sshRoot, "docs")}}, "too_large", http.StatusRequestEntityTooLarge)
	if !strings.Contains(strOf(out, "error"), "已删除") {
		t.Errorf("超限的错误文案应当说明产物已删除：%v", out)
	}
	entries, err := os.ReadDir(a.renv.ArchiveDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读压缩包目录失败：%v", err)
	}
	if len(entries) != 0 {
		t.Errorf("超限的产物应当被删掉，实际还剩：%v", entries)
	}
}

// TestFsCompressMissingTool 覆盖「远端没有 tar/unzip」：必须回 no_archive_tool。
//
// 用一个 PATH 指向不存在的目录的内嵌服务器来模拟一台缺工具的主机（真实 sshd 的 PATH 由
// 系统决定，没法在会话里改，所以只能由测试服务器提供这个能力）。
func TestFsCompressMissingTool(t *testing.T) {
	a, sid, _ := fsOpApp(t, appOptions{sshOptions: &testssh.Options{
		User: "demo", Password: "demo",
		Env: []string{"PATH=/nonexistent-webssh-test-dir"},
	}})
	a.fail(t, "/api/fs/compress", map[string]any{"sid": sid, "paths": []string{filepath.Join(a.sshRoot, "hello.txt")}}, "no_archive_tool", http.StatusBadGateway)
	// 解压走的是同一个翻译逻辑，也顺带验一下（文件名像压缩包就行：命令都跑不起来）
	writeLocal(t, filepath.Join(a.sshRoot, "fake.tar.gz"), "x")
	a.fail(t, "/api/fs/extract", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "fake.tar.gz")}, "no_archive_tool", http.StatusBadGateway)
}

// TestFsExportToNas 覆盖导出至飞牛：文件 / 目录、同名、覆盖、参数校验、上限。
func TestFsExportToNas(t *testing.T) {
	nas := t.TempDir()
	a, sid, _ := fsOpApp(t, appOptions{exportRoot: nas})
	root := a.sshRoot

	// 1) 文件
	out := a.ok(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt")})
	exp, _ := out["export"].(map[string]any)
	if strOf(exp, "path") != filepath.Join(nas, "hello.txt") {
		t.Fatalf("导出路径 = %v", exp["path"])
	}
	if numOf(exp, "size") != 12 || numOf(exp, "files") != 1 {
		t.Fatalf("导出结果 = %v", exp)
	}
	if got := readLocal(t, filepath.Join(nas, "hello.txt")); got != "hello world\n" {
		t.Fatalf("导出的内容不对：%q", got)
	}

	// 2) 同名 + 未开覆盖 → duplicate；开了覆盖 → 用远程那份替换
	writeLocal(t, filepath.Join(root, "hello.txt"), "remote-v2\n")
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt")}, "duplicate", http.StatusConflict)
	a.ok(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt"), "overwrite": true})
	if got := readLocal(t, filepath.Join(nas, "hello.txt")); got != "remote-v2\n" {
		t.Fatalf("覆盖导出没生效：%q", got)
	}

	// 3) 目录递归 + 软链接跳过 + 覆盖时清掉本机旧内容
	if err := os.MkdirAll(filepath.Join(nas, "docs"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(nas, "docs", "stale.txt"), "stale")
	out = a.ok(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "docs"), "overwrite": true})
	exp, _ = out["export"].(map[string]any)
	if numOf(exp, "files") != 2 {
		t.Errorf("目录导出的文件数 = %v，期望 2", exp["files"])
	}
	if want := float64(len("# readme\n") + len("deep\n")); numOf(exp, "size") != want {
		t.Errorf("目录导出的字节数 = %v，期望 %v", exp["size"], want)
	}
	if got := readLocal(t, filepath.Join(nas, "docs", "sub", "deep.txt")); got != "deep\n" {
		t.Errorf("深层文件没导出：%q", got)
	}
	if _, err := os.Stat(filepath.Join(nas, "docs", "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("覆盖导出应当清掉本机旧内容：%v", err)
	}
	// 远端指向别处的软链接落回本机就是个死链接 → 跳过
	if err := os.Symlink("hello.txt", filepath.Join(root, "docs", "inner-link.txt")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	a.ok(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "docs"), "overwrite": true})
	if _, err := os.Lstat(filepath.Join(nas, "docs", "inner-link.txt")); !os.IsNotExist(err) {
		t.Errorf("软链接不该被导出：%v", err)
	}

	// 4) 参数校验
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "nope")}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": root + "/../etc"}, "invalid", 0)
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": "/"}, "invalid", 0)
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "path": filepath.Join(root, "hello.txt")}, "session_gone", http.StatusNotFound)

	// 5) 没配置共享目录 → no_share_path（模拟没有 TRIM_DATA_SHARE_PATHS 的部署形态）
	a.renv.ExportRoot = ""
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt")}, "no_share_path", http.StatusServiceUnavailable)
}

// TestFsExportToNasLimit 校验导出的总字节上限，并且失败后不留半成品。
func TestFsExportToNasLimit(t *testing.T) {
	old := maxTransferBytes
	maxTransferBytes = 4
	t.Cleanup(func() { maxTransferBytes = old })

	nas := t.TempDir()
	a, sid, _ := fsOpApp(t, appOptions{exportRoot: nas})
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "hello.txt")}, "too_large", http.StatusRequestEntityTooLarge)
	if _, err := os.Stat(filepath.Join(nas, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("超限后不该留下半成品：%v", err)
	}
	// 目录导出超限时，整个目标目录都要清掉（否则用户会拿到一个内容不全的目录）
	a.fail(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "docs")}, "too_large", 0)
	if _, err := os.Stat(filepath.Join(nas, "docs")); !os.IsNotExist(err) {
		t.Fatalf("超限后不该留下半个目录：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// listTarGz 列出 .tar.gz 里的成员名（用标准库读，不依赖本机的 tar）。
func listTarGz(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开压缩包失败：%v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("压缩包不是 gzip：%v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读压缩包失败：%v", err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

// writeZip 造一个 zip 文件（测试解压用，不依赖本机的 zip 命令）。
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("建 zip 条目失败：%v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("写 zip 条目失败：%v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败：%v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("写 zip 文件失败：%v", err)
	}
}

func containsStr(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
