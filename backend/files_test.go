package main

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// TestFsInfo 覆盖「信息」接口（/api/fs/info，契约 15.5）：条目自身属性 + 目录直接子项计数。
//
// 三条口径都钉住：
//   - 计数只数**一层**（不是递归）；
//   - 软链接**跟随**（指向目录的算目录、失效的算文件），与列表里的目录/文件图标一致；
//   - 拿不到属性（不存在 / 越权路径 / 会话没了）就按既有的错误码回，不返回半份数据。
func TestFsInfo(t *testing.T) {
	a, sid := fsTestApp(t)

	// 普通文件：属性齐全，没有子项计数
	out := a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "hello.txt")})
	info := out["info"]
	if info == nil {
		t.Fatalf("响应里没有 info：%v", out)
	}
	if strOf(info, "name") != "hello.txt" || info.(map[string]any)["dir"] != false {
		t.Fatalf("hello.txt 的信息不对：%v", info)
	}
	if numOf(info, "size") != 12 {
		t.Errorf("size = %v，期望 12", numOf(info, "size"))
	}
	if mode := strOf(info, "mode"); !strings.HasPrefix(mode, "-") {
		t.Errorf("mode = %q，期望以 - 开头", mode)
	}
	if mt := strOf(info, "mtime"); !strings.HasSuffix(mt, "Z") && !strings.Contains(mt, "+") {
		t.Errorf("mtime 不是 RFC3339：%q", mt)
	}
	if strOf(info, "countError") != "" {
		t.Errorf("文件不该有统计错误：%v", info)
	}
	if numOf(info, "dirCount") != 0 || numOf(info, "fileCount") != 0 {
		t.Errorf("文件不该有子项计数：%v", info)
	}

	// 属主：uid/gid 来自 SFTP attrs，必须与本地 stat 一致；名字是远端 `id` 查出来的
	local, err := os.Stat(filepath.Join(a.sshRoot, "hello.txt"))
	if err != nil {
		t.Fatalf("本地 stat 失败：%v", err)
	}
	if st, ok := local.Sys().(*syscall.Stat_t); ok {
		if int(numOf(info, "uid")) != int(st.Uid) || int(numOf(info, "gid")) != int(st.Gid) {
			t.Errorf("uid/gid 不对：%v，期望 %d:%d", info, st.Uid, st.Gid)
		}
		// 名字能不能查到取决于本机有没有 `id`（测试服务器就跑在本机）：先自己问一遍再断言，
		// 免得在精简环境里把一个「环境不支持」当成产品 bug。
		if out, err := exec.Command("id", "-nu", strconv.Itoa(int(st.Uid))).Output(); err == nil {
			want := strings.TrimSpace(string(out))
			if strOf(info, "owner") != want {
				t.Errorf("owner = %q，期望 %q（info=%v）", strOf(info, "owner"), want, info)
			}
			if strOf(info, "group") == "" {
				t.Errorf("group 不该为空：%v", info)
			}
		} else {
			t.Log("本机没有可用的 id 命令：跳过属主名字断言")
		}
	}

	// 目录：数一层。docs 里只有 readme.md
	out = a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "docs")})
	info = out["info"]
	if info.(map[string]any)["dir"] != true {
		t.Fatalf("docs 应当是目录：%v", info)
	}
	if numOf(info, "dirCount") != 0 || numOf(info, "fileCount") != 1 {
		t.Errorf("docs 内含计数不对（期望 0 目录 / 1 文件）：%v", info)
	}

	// 家目录：docs（目录）+ hello.txt / binary.bin / link.txt / dangling.txt
	// 软链接跟随：link.txt 指向文件 → 文件；dangling.txt 失效 → 也按文件算（与列表一致）
	out = a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": ""})
	info = out["info"]
	if strOf(info, "path") != a.sshRoot {
		t.Errorf("空 path 应当指向家目录：%v", info)
	}
	if numOf(info, "dirCount") != 1 || numOf(info, "fileCount") != 4 {
		t.Errorf("家目录内含计数不对（期望 1 目录 / 4 文件）：%v", info)
	}

	// 指向目录的软链接算目录（口径与列表的 dir 一致）
	if err := os.Symlink("docs", filepath.Join(a.sshRoot, "docs-link")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	out = a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": ""})
	if n := numOf(out["info"], "dirCount"); n != 2 {
		t.Errorf("指向目录的软链接应当算目录（期望 dirCount=2）：%v", n)
	}
	if strOf(out["info"], "countError") != "" {
		t.Errorf("统计失败不该静默：%v", out["info"])
	}

	// 错误路径：.. 逃逸、不存在、非法会话，都和 /api/fs/list 一个口径
	a.fail(t, "/api/fs/info", map[string]any{"sid": sid, "path": a.sshRoot + "/../.."}, "invalid", http.StatusBadRequest)
	a.fail(t, "/api/fs/info", map[string]any{"sid": sid, "path": filepath.Join(a.sshRoot, "nope")}, "not_found", http.StatusNotFound)
	a.fail(t, "/api/fs/info", map[string]any{"sid": "ffffffffffffffffffffffffffffffff", "path": ""}, "session_gone", http.StatusNotFound)
}

// TestFsInfoCachesOwnerNames 钉住「属主名字按会话缓存」。
//
// resolveOwnerNames 每次都要**新拨一条 SSH** 跑 `id -nu/-ng`（见 remote_exec 的说明），
// 而「信息」弹窗会被反复打开。缓存后第二次查同一对 uid/gid 不该再拨连接。
func TestFsInfoCachesOwnerNames(t *testing.T) {
	a, sid := fsTestApp(t)
	target := filepath.Join(a.sshRoot, "hello.txt")

	// 本机没有 `id` 时查不到名字（也不写缓存），这条用例就没有意义
	if _, err := exec.Command("id", "-nu", strconv.Itoa(os.Getuid())).Output(); err != nil {
		t.Skip("本机没有可用的 id 命令，属主名字查询不适用")
	}

	before := a.ssh.ConnsAccepted()
	a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": target})
	first := a.ssh.ConnsAccepted()
	if first <= before {
		t.Fatalf("第一次查属主名字应当新拨一条 SSH：%d → %d", before, first)
	}

	// 第二次（同一个文件的同一对 uid/gid）：必须命中缓存
	a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": target})
	if got := a.ssh.ConnsAccepted(); got != first {
		t.Errorf("重复查同一个属主不该再拨 SSH：%d → %d", first, got)
	}
}

// TestFsInfoCountError 校验「数不出子项」时如实带原因回来，而不是把 0 当空目录。
//
// 用 0000 权限的目录模拟：SFTP 的 ReadDir 会失败。两种环境里这招不灵，直接 skip 并写明原因
// （**不要**假装通过）：
//   - 以 root 跑测试：root 不受权限位约束；
//   - 文件系统不认权限位（飞牛这类卷会按自己的 ACL 重写 mode——实测量到的是 0705，
//     AGENTS 第 2 节也记了「新文件的模式位不可靠」）。
func TestFsInfoCountError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行：权限位挡不住 ReadDir，跳过")
	}
	a, sid := fsTestApp(t)
	locked := filepath.Join(a.sshRoot, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	defer os.Chmod(locked, 0o755)
	// 直接问「ReadDir 到底会不会失败」——这才是一条权限断言真正需要的前提，
	// 比检查 mode 的数字可靠（文件系统可能改写 mode，却仍然拦得住读取）
	if _, err := os.ReadDir(locked); err == nil {
		fi, _ := os.Stat(locked)
		t.Skipf("这个文件系统拦不住 0000 目录的读取（mode=%v）：无法模拟「读不出子项」，跳过", fi.Mode())
	}

	out := a.ok(t, "/api/fs/info", map[string]any{"sid": sid, "path": locked})
	info := out["info"]
	if info.(map[string]any)["dir"] != true {
		t.Fatalf("locked 应当是目录：%v", info)
	}
	if strOf(info, "countError") == "" {
		t.Errorf("读不出子项时应当带回原因（不能显示成空目录）：%v", info)
	}
	if n := numOf(info, "dirCount") + numOf(info, "fileCount"); n != 0 {
		t.Errorf("统计失败时不该给出计数（会被读成空目录）：%v", info)
	}
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

// TestZipEntryName 钉住目录下载的 zip 条目名（不要带前导 `/`）。
//
// root 是顶层目录（`/home`、`/tmp`）时 base = path.Dir(root) = `/`，旧实现用
// `strings.TrimPrefix(full, base+"/")`（即 `"//"`）一次都匹配不上，条目名成了 `/home/x`——
// 带前导斜杠的绝对路径，Windows 解压会解出奇怪的盘符路径。
func TestZipEntryName(t *testing.T) {
	cases := []struct{ full, base, want string }{
		{"/home/user/docs/a.txt", "/home/user", "docs/a.txt"},
		{"/home/user/docs/sub/b.txt", "/home/user", "docs/sub/b.txt"},
		{"/home/a.txt", "/", "home/a.txt"}, // root = /home：旧实现给 "/home/a.txt"
		{"/tmp/x", "/", "tmp/x"},
		{"/home/userdata/x", "/home", "userdata/x"}, // base 是部分路径段时也不能多切
	}
	for _, c := range cases {
		if got := zipEntryName(c.full, c.base); got != c.want {
			t.Errorf("zipEntryName(%q, %q) = %q，期望 %q", c.full, c.base, got, c.want)
		}
	}
}

// TestFsUploadOverwriteKeepsExistingDirectory 钉住「覆盖上传失败绝不动目标」。
//
// 目标同名且是个目录时，写文件必然失败。旧实现先在目标上 O_TRUNC、失败再 Remove(dest)，
// 会把那个目录直接删掉——用户只是想上传一个同名文件，结果丢了一个目录。
func TestFsUploadOverwriteKeepsExistingDirectory(t *testing.T) {
	a, sid := fsTestApp(t)
	root := a.sshRoot
	dirPath := filepath.Join(root, "same-name")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}

	res, out := a.doUpload(t, sid, root, true, map[string]string{"same-name": "x"})
	if res.StatusCode == http.StatusOK {
		t.Fatalf("往同名目录上覆盖上传竟然成功了：%v", out)
	}
	fi, err := os.Stat(dirPath)
	if err != nil || !fi.IsDir() {
		t.Fatalf("上传失败后同名目录没了（这正是旧实现的数据丢失路径）：%v %v", fi, err)
	}
	// 临时文件必须被清掉，不能留在用户的目录里
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读目标目录失败：%v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".webssh-upload-") {
			t.Errorf("上传失败后残留了临时文件：%s", e.Name())
		}
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
	// 根目录：与 copy / move / export-to-nas 口径一致，直接拒绝——下载 `/` 会把整棵文件系统
	// 打进一个临时 zip（只受远端磁盘限制、用户取消不掉），条目名还会带上前导 `/`。
	a.fail(t, "/api/fs/download", map[string]any{"sid": sid, "path": "/"}, "invalid", http.StatusBadRequest)

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

// TestRemotePathKeepsSurroundingSpaces 回归：路径里的首尾空格**不能**被裁掉。
//
// 症状（e2e 实测）：名字以空格结尾的目录能在 fs/list 里列出来（它来自 SFTP 的真实条目），
// 但拿同一个路径去 list/export/compress 全部回 `not_found`——因为查询入口把路径
// TrimSpace 成了另一个不存在的名字。裁空格看起来「更健壮」，实际是把合法路径改坏了。
func TestRemotePathKeepsSurroundingSpaces(t *testing.T) {
	a := newTestApp(t, appOptions{exportRoot: t.TempDir()})
	sid := a.newSSHSession(t)
	root := a.ssh.Root()

	weird := root + "/trailing "
	if res, out := a.post(t, "/api/fs/mkdir", map[string]any{"sid": sid, "path": weird}); res.StatusCode != 200 {
		t.Fatalf("创建带尾空格的目录失败：%d %v", res.StatusCode, out)
	}

	// list 该目录本身：必须 200（修复前这里会 404）
	res, out := a.post(t, "/api/fs/list", map[string]any{"sid": sid, "path": weird})
	if res.StatusCode != 200 {
		t.Fatalf("list 带尾空格的目录失败：%d %v", res.StatusCode, out)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 0 {
		t.Fatalf("新目录应当是空的，得到 %v", entries)
	}

	// 导出该目录也必须能找到它（走的是同一条路径解析）
	if res, out := a.post(t, "/api/fs/export-to-nas", map[string]any{"sid": sid, "path": weird}); res.StatusCode != 200 {
		t.Fatalf("导出带尾空格的目录失败：%d %v", res.StatusCode, out)
	}

	// 删除时也不能裁
	if res, out := a.post(t, "/api/fs/remove", map[string]any{"sid": sid, "path": weird, "dir": true}); res.StatusCode != 200 {
		t.Fatalf("删除带尾空格的目录失败：%d %v", res.StatusCode, out)
	}
}
