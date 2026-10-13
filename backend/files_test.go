package main

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fsTestApp 起一个后端 + SSH 服务器，并在远端根目录里准备好几个文件。
func fsTestApp(t *testing.T) (*testApp, string) {
	t.Helper()
	a := newTestApp(t, appOptions{})
	writeLocal(t, filepath.Join(a.sshRoot, "hello.txt"), "hello world\n")
	if err := os.Mkdir(filepath.Join(a.sshRoot, "docs"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(a.sshRoot, "docs", "readme.md"), "# readme\n")
	writeLocal(t, filepath.Join(a.sshRoot, "binary.bin"), "a\x00b")
	// 软链接：一个指向存在的文件、一个指向不存在的位置（界面要显示「失效链接」）
	if err := os.Symlink("hello.txt", filepath.Join(a.sshRoot, "link.txt")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	if err := os.Symlink("nowhere.txt", filepath.Join(a.sshRoot, "dangling.txt")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	sid := a.newSSHSession(t)
	return a, sid
}

func writeLocal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s 失败：%v", path, err)
	}
}

// TestFsList 覆盖列目录：空路径 = 家目录、字段形态、排序、软链接与失效链接。
func TestFsList(t *testing.T) {
	a, sid := fsTestApp(t)

	out := a.ok(t, "/api/fs/list", map[string]any{"sid": sid, "path": ""})
	if strOf(out, "home") != a.sshRoot {
		t.Errorf("home = %v，期望 %v", out["home"], a.sshRoot)
	}
	if strOf(out, "path") != a.sshRoot {
		t.Errorf("空 path 应当返回家目录：%v", out["path"])
	}
	entries := listOf(out, "entries")
	byName := map[string]map[string]any{}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		byName[strOf(m, "name")] = m
	}
	// 目录排在最前
	if len(entries) == 0 || !listEntryIsDir(t, entries[0]) {
		t.Errorf("目录应当排在前面：%v", entries)
	}
	dir := byName["docs"]
	if dir == nil || dir["dir"] != true {
		t.Fatalf("docs 条目不对：%v", dir)
	}
	if strOf(dir, "path") != filepath.Join(a.sshRoot, "docs") {
		t.Errorf("FsEntry.path 应当是绝对路径：%v", dir["path"])
	}
	if mode := strOf(dir, "mode"); !strings.HasPrefix(mode, "d") {
		t.Errorf("目录 mode = %q，期望以 d 开头（前端直接展示它）", mode)
	}
	file := byName["hello.txt"]
	if file == nil || file["dir"] != false || numOf(file, "size") != 12 {
		t.Fatalf("hello.txt 条目不对：%v", file)
	}
	if mode := strOf(file, "mode"); !strings.HasPrefix(mode, "-") {
		t.Errorf("文件 mode = %q", mode)
	}
	if mtime := strOf(file, "mtime"); !strings.HasSuffix(mtime, "Z") && !strings.Contains(mtime, "+") {
		t.Errorf("mtime 不是 RFC3339：%q", mtime)
	}
	link := byName["link.txt"]
	if link == nil || strOf(link, "symlink") != "hello.txt" || link["dir"] != false || link["broken"] == true {
		t.Fatalf("软链接条目不对：%v", link)
	}
	dangling := byName["dangling.txt"]
	if dangling == nil || dangling["broken"] != true {
		t.Fatalf("失效链接条目不对：%v", dangling)
	}

	// 绝对路径、相对路径都能列
	out = a.ok(t, "/api/fs/list", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "docs")})
	if strOf(out, "path") != filepath.Join(a.sshRoot, "docs") {
		t.Errorf("绝对路径列目录返回 %v", out["path"])
	}
	if len(listOf(out, "entries")) != 1 {
		t.Errorf("docs 里应当只有一个文件：%v", out["entries"])
	}
	out = a.ok(t, "/api/fs/list", map[string]any{"sid": sid, "path": "docs"})
	if len(listOf(out, "entries")) != 1 {
		t.Errorf("相对路径列目录失败：%v", out)
	}

	// 路径里的 .. 必须被拒绝（不能靠 SFTP 服务端兜底）
	a.fail(t, "/api/fs/list", map[string]any{"sid": sid, "path": a.sshRoot + "/../.."}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/list", map[string]any{"sid": sid, "path": "../"}, "invalid", 0)
	// 目录不存在
	a.fail(t, "/api/fs/list", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "nope")}, "not_found", http.StatusNotFound)
	// 会话不存在 / 未打开
	a.fail(t, "/api/fs/list", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "path": ""}, "session_gone", http.StatusNotFound)
	notOpen := a.ok(t, "/api/sessions/save", map[string]any{
		"host": a.ssh.Host(), "port": a.ssh.Port(), "user": "demo2", "authType": "password", "password": "demo",
	})
	a.fail(t, "/api/fs/list", map[string]any{"sid": strOf(notOpen["session"], "id"), "path": ""}, "not_open", http.StatusConflict)
	// 缺 sid
	a.fail(t, "/api/fs/list", map[string]any{"path": ""}, "session_gone", 0)
}

func listEntryIsDir(t *testing.T, v any) bool {
	t.Helper()
	m, _ := v.(map[string]any)
	return m["dir"] == true
}

// TestFsCreateMkdirRenameRemove 覆盖新建文件 / 目录、重命名、删除（含类型校验）。
func TestFsCreateMkdirRenameRemove(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	// mkdir
	a.ok(t, "/api/fs/mkdir", map[string]any{"sid": sid, "path": filepath.Join(root, "newdir")})
	a.fail(t, "/api/fs/mkdir", map[string]any{"sid": sid, "path": filepath.Join(root, "newdir")}, "duplicate", http.StatusConflict)
	if st, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !st.IsDir() {
		t.Fatalf("目录没建出来：%v", err)
	}

	// create（已存在必须报 duplicate：pkg/sftp 的 Create 是 O_TRUNC，会把已有文件清空）
	a.ok(t, "/api/fs/create", map[string]any{"sid": sid, "path": filepath.Join(root, "empty.txt")})
	st, err := os.Stat(filepath.Join(root, "empty.txt"))
	if err != nil || st.Size() != 0 {
		t.Fatalf("空文件没建出来：%v", err)
	}
	a.fail(t, "/api/fs/create", map[string]any{"sid": sid, "path": filepath.Join(root, "empty.txt")}, "duplicate", 0)
	a.ok(t, "/api/fs/create", map[string]any{"sid": sid, "path": filepath.Join(root, "newdir", "in.txt")})

	// rename
	a.ok(t, "/api/fs/rename", map[string]any{
		"sid": sid, "path": filepath.Join(root, "empty.txt"), "to": filepath.Join(root, "renamed.txt"),
	})
	if _, err := os.Stat(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatalf("重命名没生效：%v", err)
	}
	// 目标已存在 → duplicate（SFTP 的 rename 会直接覆盖，必须先查）
	a.fail(t, "/api/fs/rename", map[string]any{
		"sid": sid, "path": filepath.Join(root, "renamed.txt"), "to": filepath.Join(root, "hello.txt"),
	}, "duplicate", http.StatusConflict)
	a.fail(t, "/api/fs/rename", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed.txt"), "to": "../escape"}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/rename", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed.txt")}, "invalid", 0)
	// 相对路径的 to 相对家目录解析
	a.ok(t, "/api/fs/rename", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed.txt"), "to": "renamed2.txt"})
	if _, err := os.Stat(filepath.Join(root, "renamed2.txt")); err != nil {
		t.Fatalf("相对 to 没生效：%v", err)
	}

	// remove：类型必须一致（否则会误删整棵树）
	a.fail(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "newdir"), "dir": false}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed2.txt"), "dir": true}, "invalid", 0)
	// 目录删除是递归的（界面上的删除确认已经说明了「目录会连同其中内容一起删除」）
	a.ok(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "newdir"), "dir": true})
	if _, err := os.Stat(filepath.Join(root, "newdir")); !os.IsNotExist(err) {
		t.Fatalf("目录没删掉：%v", err)
	}
	a.ok(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed2.txt"), "dir": false})
	if _, err := os.Stat(filepath.Join(root, "renamed2.txt")); !os.IsNotExist(err) {
		t.Fatalf("文件没删掉：%v", err)
	}
	a.fail(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "renamed2.txt"), "dir": false}, "not_found", 0)
	a.fail(t, "/api/fs/remove", map[string]any{"sid": sid, "path": "/", "dir": true}, "invalid", 0)
	// 注意用字符串拼接而不是 filepath.Join：Join 会把 .. 规整掉，测的就不是「拒绝 .. 」了。
	a.fail(t, "/api/fs/remove", map[string]any{"sid": sid, "path": root + "/../x", "dir": false}, "invalid", 0)
	// 软链接按「文件」删（不跟随链接）
	a.ok(t, "/api/fs/remove", map[string]any{"sid": sid, "path": filepath.Join(root, "link.txt"), "dir": false})
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); err != nil {
		t.Fatalf("删软链接不该动到目标文件：%v", err)
	}
}

// TestFsUploadLocal 覆盖本地上传（含相对路径目录上传、覆盖开关、非法文件名）。
func TestFsUploadLocal(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	// 一次传两个文件：一个平铺、一个带相对目录（目录上传绝不能被拍平）
	res, out := a.doUpload(t, sid, root, false, map[string]string{"top.txt": "top", "dir/sub/child.txt": "child"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("上传状态码 = %d：%v", res.StatusCode, out)
	}
	if out["ok"] != true || len(listOf(out, "files")) != 2 || numOf(out, "skipped") != 0 {
		t.Fatalf("上传结果 = %v", out)
	}
	if got := readLocal(t, filepath.Join(root, "dir", "sub", "child.txt")); got != "child" {
		t.Errorf("目录上传被拍平或内容不对：%q", got)
	}
	if got := readLocal(t, filepath.Join(root, "top.txt")); got != "top" {
		t.Errorf("平铺文件内容不对：%q", got)
	}
	// 回给前端的路径必须是绝对路径且与实际落盘一致
	for _, f := range listOf(out, "files") {
		m, _ := f.(map[string]any)
		p := strOf(m, "path")
		if !strings.HasPrefix(p, root+"/") {
			t.Errorf("files[].path 不是绝对路径：%q", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("files[].path 指向的文件不存在：%q", p)
		}
	}

	// overwrite=false：同名整份跳过（不能只省掉 O_TRUNC，那等于开关没接上）
	writeLocal(t, filepath.Join(root, "top.txt"), "changed-by-user")
	res, out = a.doUpload(t, sid, root, false, map[string]string{"top.txt": "overwritten"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("上传状态码 = %d：%v", res.StatusCode, out)
	}
	if numOf(out, "skipped") != 1 || len(listOf(out, "files")) != 0 {
		t.Fatalf("overwrite=false 的结果 = %v", out)
	}
	if got := readLocal(t, filepath.Join(root, "top.txt")); got != "changed-by-user" {
		t.Errorf("overwrite=false 竟然改了文件：%q", got)
	}
	// overwrite=true：覆盖
	if res, out = a.doUpload(t, sid, root, true, map[string]string{"top.txt": "overwritten"}); res.StatusCode != http.StatusOK || numOf(out, "skipped") != 0 {
		t.Fatalf("overwrite=true 的结果 = %d %v", res.StatusCode, out)
	}
	if got := readLocal(t, filepath.Join(root, "top.txt")); got != "overwritten" {
		t.Errorf("overwrite=true 没生效：%q", got)
	}

	// 非法的 filename：绝对路径 / 含 ..
	for _, bad := range []string{"/abs.txt", "../up.txt", "a/../../b.txt"} {
		if res, out = a.doUpload(t, sid, root, true, map[string]string{bad: "x"}); res.StatusCode != http.StatusBadRequest {
			t.Errorf("上传 %q 状态码 = %d（期望 400）：%v", bad, res.StatusCode, out)
		}
	}
	// 没有文件
	if res, _ = a.doUpload(t, sid, root, true, nil); res.StatusCode != http.StatusBadRequest {
		t.Errorf("空上传状态码 = %d", res.StatusCode)
	}
	// 会话未打开 → not_open
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	if res, out = a.doUpload(t, sid, root, true, map[string]string{"a.txt": "x"}); res.StatusCode != http.StatusConflict || out["code"] != "not_open" {
		t.Errorf("会话未打开时上传响应 = %d %v", res.StatusCode, out)
	}
}

// TestFsUploadTooLarge 校验上传整体上限（把上限临时调小，避免真造 1 GiB 请求体）。
func TestFsUploadTooLarge(t *testing.T) {
	old := maxUploadBytes
	maxUploadBytes = 1024
	t.Cleanup(func() { maxUploadBytes = old })

	a, sid := fsTestApp(t)
	res, out := a.doUpload(t, sid, a.sshRoot, true, map[string]string{"big.txt": strings.Repeat("x", 4096)})
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上传状态码 = %d（期望 413）：%v", res.StatusCode, out)
	}
	if out["code"] != "too_large" {
		t.Fatalf("超限上传响应 = %v", out)
	}
}

// uploadBody 返回 multipart 报文与它的 Content-Type（含 boundary）。
func uploadBody(t *testing.T, sid, dir string, overwrite bool, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("sid", sid)
	_ = mw.WriteField("path", dir)
	if overwrite {
		_ = mw.WriteField("overwrite", "true")
	} else {
		_ = mw.WriteField("overwrite", "false")
	}
	// 顺序固定，便于断言
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		// filename 里带 `/` 正是前端的目录上传（form.append('files', file, entry.path)）。
		w, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("构造 multipart 失败：%v", err)
		}
		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatalf("写 multipart 失败：%v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败：%v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// doUpload 发一次 multipart 上传（Content-Type 必须带 boundary，否则服务端解不出来）。
func (a *testApp) doUpload(t *testing.T, sid, dir string, overwrite bool, files map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	body, contentType := uploadBody(t, sid, dir, overwrite, files)
	res, raw := a.do(t, http.MethodPost, "/api/fs/upload", body, map[string]string{"Content-Type": contentType})
	return res, decodeJSONBody(t, raw)
}

func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func readLocal(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	return string(data)
}

// TestFsDownloadAndRead 覆盖下载（文件 / 目录 zip）与文本预览。
func TestFsDownloadAndRead(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot

	// 文件下载
	res, raw := a.do(t, http.MethodPost, "/api/fs/download", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt")}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("下载状态码 = %d：%s", res.StatusCode, snippetOf(raw))
	}
	if got := string(raw); got != "hello world\n" {
		t.Errorf("下载内容 = %q", got)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="hello.txt"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// 目录下载（zip）：先打好临时文件再发正文，所以这里能拿到完整 zip
	res, raw = a.do(t, http.MethodPost, "/api/fs/download", map[string]any{"sid": sid, "path": filepath.Join(root, "docs")}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("目录下载状态码 = %d：%s", res.StatusCode, snippetOf(raw))
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="docs.zip"`) {
		t.Errorf("目录下载的 Content-Disposition = %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("下载的 zip 解不开：%v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["docs/readme.md"] {
		t.Errorf("zip 里没有 docs/readme.md：%v", names)
	}
	// 下载不存在的路径 → not_found；路径非法 → invalid
	a.fail(t, "/api/fs/download", map[string]any{"sid": sid, "path": filepath.Join(root, "nope")}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/download", map[string]any{"sid": sid, "path": root + "/../x"}, "invalid", http.StatusBadRequest)

	// 文本预览
	out := a.ok(t, "/api/fs/read", map[string]any{"sid": sid, "path": filepath.Join(root, "hello.txt")})
	if out["text"] != true || out["tooLarge"] != false || strOf(out, "content") != "hello world\n" || numOf(out, "size") != 12 {
		t.Fatalf("read 结果 = %v", out)
	}
	// 二进制文件：不带内容回来（避免把二进制塞进 JSON）
	out = a.ok(t, "/api/fs/read", map[string]any{"sid": sid, "path": filepath.Join(root, "binary.bin")})
	if out["text"] != false || strOf(out, "content") != "" {
		t.Fatalf("二进制 read 结果 = %v", out)
	}
	// 目录 → invalid
	a.fail(t, "/api/fs/read", map[string]any{"sid": sid, "path": filepath.Join(root, "docs")}, "invalid", http.StatusBadRequest)

	// 超过上限：只回元信息 + tooLarge
	bigPath := filepath.Join(root, "big.txt")
	big, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("建大文件失败：%v", err)
	}
	if _, err := big.Write(bytes.Repeat([]byte("x"), maxReadBytes+1)); err != nil {
		t.Fatalf("写大文件失败：%v", err)
	}
	big.Close()
	out = a.ok(t, "/api/fs/read", map[string]any{"sid": sid, "path": bigPath})
	if out["tooLarge"] != true || numOf(out, "size") != float64(maxReadBytes+1) || strOf(out, "content") != "" {
		t.Fatalf("超限 read 结果 = %v", out)
	}
}

// TestFsUploadFromNas 覆盖「从飞牛（本机）上传」：本地读取 → SFTP 写入。
func TestFsUploadFromNas(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot
	nasDir := t.TempDir()
	nasFile := filepath.Join(nasDir, "from-nas.txt")
	writeLocal(t, nasFile, "nas content")

	out := a.ok(t, "/api/fs/upload-from-nas", map[string]any{
		"sid": sid, "path": root, "source": nasFile, "overwrite": false,
	})
	files := listOf(out, "files")
	if len(files) != 1 || numOf(out, "skipped") != 0 {
		t.Fatalf("upload-from-nas 结果 = %v", out)
	}
	m, _ := files[0].(map[string]any)
	if strOf(m, "path") != filepath.Join(root, "from-nas.txt") {
		t.Errorf("落盘路径 = %v", m["path"])
	}
	if got := readLocal(t, filepath.Join(root, "from-nas.txt")); got != "nas content" {
		t.Errorf("内容 = %q", got)
	}
	if numOf(m, "size") != float64(len("nas content")) {
		t.Errorf("size = %v", m["size"])
	}
	// overwrite=false 且目标已存在 → skipped
	out = a.ok(t, "/api/fs/upload-from-nas", map[string]any{
		"sid": sid, "path": root, "source": nasFile, "overwrite": false,
	})
	if numOf(out, "skipped") != 1 {
		t.Errorf("同名应当跳过：%v", out)
	}

	// 参数校验：相对路径、目录、不存在的文件、特殊文件
	a.fail(t, "/api/fs/upload-from-nas", map[string]any{"sid": sid, "path": root, "source": "rel.txt"}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/upload-from-nas", map[string]any{"sid": sid, "path": root, "source": nasDir}, "invalid", 0)
	a.fail(t, "/api/fs/upload-from-nas", map[string]any{"sid": sid, "path": root, "source": filepath.Join(nasDir, "nope.txt")}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/upload-from-nas", map[string]any{"sid": sid, "path": root, "source": nasDir + "/../rel"}, "invalid", 0)
	// FIFO 会被 open(2) 永久阻塞，必须提前拒绝
	fifo := filepath.Join(nasDir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		a.fail(t, "/api/fs/upload-from-nas", map[string]any{"sid": sid, "path": root, "source": fifo}, "invalid", 0)
	}
}

// TestFsErrorsOnClosedSession 校验会话关闭后所有文件接口都给出明确错误。
func TestFsErrorsOnClosedSession(t *testing.T) {
	a, sid := fsTestApp(t)
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	for _, p := range []string{"/api/fs/list", "/api/fs/mkdir", "/api/fs/create", "/api/fs/rename", "/api/fs/remove", "/api/fs/read", "/api/fs/download", "/api/fs/upload-from-nas"} {
		body := map[string]any{"sid": sid, "path": a.sshRoot, "to": a.sshRoot + "/x"}
		if p == "/api/fs/upload-from-nas" {
			// source 的参数校验发生在会话检查之前，所以这里必须给一个合法的 source。
			body["source"] = "/etc/hostname"
		}
		res, out := a.post(t, p, body)
		if res.StatusCode != http.StatusConflict || out["code"] != "not_open" {
			t.Errorf("%s 关闭会话后的响应 = %d %v", p, res.StatusCode, out)
		}
	}
	// 会话配置不存在 → session_gone
	for _, p := range []string{"/api/fs/list", "/api/fs/mkdir", "/api/fs/create", "/api/fs/read"} {
		res, out := a.post(t, p, map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "path": a.sshRoot})
		if res.StatusCode != http.StatusNotFound || out["code"] != "session_gone" {
			t.Errorf("%s 未知会话的响应 = %d %v", p, res.StatusCode, out)
		}
	}
}

// TestFsCloseSessionRemovesHistoryAndSftp 校验关闭会话时 SFTP 连接也断掉（不只是终端）。
func TestFsCloseSessionRemovesHistoryAndSftp(t *testing.T) {
	a, sid := fsTestApp(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("取运行时失败：%v", apiErr)
	}
	if err := rt.withSFTP(t.Context(), func(sess *sftpSession) error {
		_, err := sess.client.RealPath(".")
		return err
	}); err != nil {
		t.Fatalf("文件操作失败：%v", err)
	}
	if !rt.sftpReady() {
		t.Fatal("SFTP 通道应当已建立")
	}
	a.ok(t, "/api/sessions/close", map[string]any{"id": sid})
	if rt.sftpReady() {
		t.Error("关闭会话后不该还留着 SFTP 通道")
	}
	// 用一条已经不存在的会话再试一次：session_gone（而不是内部错误）
	a.fail(t, "/api/fs/list", map[string]any{"sid": sid, "path": ""}, "not_open", 0)
}
