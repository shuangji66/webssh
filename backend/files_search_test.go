package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"webssh/backend/internal/testssh"
)

// ---------------------------------------------------------------------------
// /api/fs/search（第二十二轮，契约 32）的集成测试。
//
// 全部跑在真链路上（内嵌 SSH/SFTP 服务器 + unix socket HTTP），并且**按行解析**流式响应：
// 记录的形态、顺序、攒批行为都是契约的一部分，不能只看最后一行统计。
// ---------------------------------------------------------------------------

// searchRow 是流式响应里的一行（测试自己解一遍，形状与后端 searchRecord 一致）。
type searchRow struct {
	Type      string           `json:"type"`
	Path      string           `json:"path"`
	Entries   []map[string]any `json:"entries"`
	Scanned   int              `json:"scanned"`
	Dirs      int              `json:"dirs"`
	Skipped   int              `json:"skipped"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
	ElapsedMS int64            `json:"elapsedMs"`
	Code      string           `json:"code"`
	Error     string           `json:"error"`
}

// searchNDJSON 发一次搜索并逐行解析流式响应。
//
// **靠 Content-Type 分流**：开流之后是 application/x-ndjson（一行一条记录）；开流之前的失败
// 是普通的 application/json 错误体（此时返回空记录表，正文用 a.post 单独断言）。
// 这条判据本身也是契约的一部分，别退化成「猜正文像不像 JSON」。
func (a *testApp) searchNDJSON(t *testing.T, sid, path, query string) (*http.Response, []searchRow) {
	t.Helper()
	res, raw := a.do(t, http.MethodPost, "/api/fs/search",
		map[string]any{"sid": sid, "path": path, "query": query}, nil)
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/x-ndjson") {
		return res, nil
	}
	var rows []searchRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row searchRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("响应里有一行不是 JSON（%v）：%q（全文：%s）", err, line, snippetOf(raw))
		}
		rows = append(rows, row)
	}
	return res, rows
}

// searchRowOf 取第一条指定类型的记录（没有就 fail）。
func searchRowOf(t *testing.T, rows []searchRow, kind string) searchRow {
	t.Helper()
	for _, row := range rows {
		if row.Type == kind {
			return row
		}
	}
	t.Fatalf("响应里没有 %q 记录：%+v", kind, rows)
	return searchRow{}
}

// hitPaths 按到达顺序列出所有结果路径。
func hitPaths(rows []searchRow) []string {
	var out []string
	for _, row := range rows {
		if row.Type != "batch" {
			continue
		}
		for _, e := range row.Entries {
			p, _ := e["path"].(string)
			out = append(out, p)
		}
	}
	return out
}

// hitNames 按到达顺序列出所有结果的名字。
func hitNames(rows []searchRow) []string {
	var out []string
	for _, row := range rows {
		if row.Type != "batch" {
			continue
		}
		for _, e := range row.Entries {
			n, _ := e["name"].(string)
			out = append(out, n)
		}
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// searchTestApp 起一个后端 + SSH 服务器，并在远端根目录下铺一棵固定的树：
//
//	root/
//	  .hidden.txt            隐藏文件（不过滤）
//	  hello.txt
//	  other.bin
//	  docs/
//	    readme.md
//	    sub/
//	      deep-app.log
//	  notes/
//	    APP.log
//	  target/
//	    inner.txt
//	  self -> .              指向**起点自己**的软链接（递归进去就是死循环）
func searchTestApp(t *testing.T) (*testApp, string) {
	t.Helper()
	a := newTestApp(t, appOptions{})
	root := a.sshRoot
	writeLocal(t, filepath.Join(root, ".hidden.txt"), "hidden\n")
	writeLocal(t, filepath.Join(root, "hello.txt"), "hello world\n")
	writeLocal(t, filepath.Join(root, "other.bin"), "x\x00y")
	if err := os.MkdirAll(filepath.Join(root, "docs", "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "target"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(root, "docs", "readme.md"), "# readme\n")
	writeLocal(t, filepath.Join(root, "docs", "sub", "deep-app.log"), "deep\n")
	writeLocal(t, filepath.Join(root, "notes", "APP.log"), "app\n")
	writeLocal(t, filepath.Join(root, "target", "inner.txt"), "inner\n")
	if err := os.Symlink(".", filepath.Join(root, "self")); err != nil {
		t.Fatalf("建软链接失败：%v", err)
	}
	sid := a.newSSHSession(t)
	return a, sid
}

// TestFsSearchSubtree 覆盖搜索的核心语义：递归子树、**只按名字匹配**（不匹配路径）、
// 大小写不敏感、结果顺序、目录也能作为结果、隐藏文件不过滤、软链接不递归、以及流式响应的
// 记录形态。
func TestFsSearchSubtree(t *testing.T) {
	a, sid := searchTestApp(t)
	root := a.sshRoot

	// ---- 第一行必须是 start（前端靠它算相对路径、记住这批结果属于哪个目录） ----
	res, rows := a.searchNDJSON(t, sid, root, "app")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("搜索失败：%d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Errorf("Content-Type = %q，期望 application/x-ndjson（流式）", got)
	}
	// 网关（nginx）默认会缓冲整条响应：这行头是「边搜边显示」的前提
	if got := res.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 no（否则中间层会把整条响应缓冲下来）", got)
	}
	if rows[0].Type != "start" {
		t.Fatalf("第一条记录应当是 start：%+v", rows[0])
	}
	if rows[0].Path != root {
		t.Errorf("start.path = %q，期望 %q", rows[0].Path, root)
	}
	if last := rows[len(rows)-1]; last.Type != "done" {
		t.Fatalf("最后一条记录应当是 done：%+v", last)
	}

	// 命中顺序 = BFS 顺序：notes/APP.log（1 层）先于 docs/sub/deep-app.log（2 层）
	if got := hitNames(rows); !equalStrings(got, []string{"APP.log", "deep-app.log"}) {
		t.Errorf("命中 = %v，期望 [APP.log deep-app.log]（先近后远）", got)
	}
	done := searchRowOf(t, rows, "done")
	if done.Total != 2 || done.Truncated {
		t.Errorf("done = %+v，期望 2 条且未截断", done)
	}
	// 扫描数 / 目录数固定：root(7 项) + docs(2) + notes(1) + target(1) + docs/sub(1) = 12 项、
	// 5 个目录。这个断言就是「没有跟着 self 软链接递归进去」的证据——跟进去的话项数会翻倍。
	if done.Scanned != 12 || done.Dirs != 5 {
		t.Errorf("扫描 %d 项 / %d 个目录，期望 12 / 5（软链接不该被递归）", done.Scanned, done.Dirs)
	}
	for _, p := range hitPaths(rows) {
		if strings.Contains(p, "/self/") {
			t.Errorf("结果里出现了穿过软链接的路径（软链接不能被递归）：%s", p)
		}
	}

	// ---- **只匹配名字**，不匹配路径 ----
	// 输入 docs 只命中那个目录自己：`docs/readme.md`、`docs/sub/...` 一个都不许出现
	// （按路径匹配时它们会被一起倒出来，正是用户返工要求去掉的那种「量太大」）。
	_, rows = a.searchNDJSON(t, sid, root, "docs")
	if got := hitNames(rows); !equalStrings(got, []string{"docs"}) {
		t.Errorf("按名字匹配 `docs` 命中 = %v，期望只有目录 docs 自己", got)
	}
	// 目录也要能命中（界面据此「进入该目录」），且带 dir=true
	var sawDir bool
	for _, row := range rows {
		for _, e := range row.Entries {
			if e["name"] == "docs" && e["dir"] == true {
				sawDir = true
			}
		}
	}
	if !sawDir {
		t.Error("结果里的目录必须以 dir=true 出现（否则界面不会把它当目录跳进去）")
	}
	// 名字里不含关键词、只有路径里含：一条都不该有（deep-app.log 的路径是 docs/sub/deep-app.log）
	_, rows = a.searchNDJSON(t, sid, root, "docs/sub")
	if got := hitNames(rows); len(got) != 0 {
		t.Errorf("路径里含关键词、名字里不含的项不该命中：%v", got)
	}
	// 子目录里的项仍能被搜到（递归没变）
	_, rows = a.searchNDJSON(t, sid, root, "readme")
	if got := hitNames(rows); !equalStrings(got, []string{"readme.md"}) {
		t.Errorf("递归命中的名字匹配失败：%v", got)
	}

	// ---- 大小写不敏感 + 隐藏文件不过滤 ----
	_, rows = a.searchNDJSON(t, sid, root, "ReAdMe")
	if got := hitNames(rows); !equalStrings(got, []string{"readme.md"}) {
		t.Errorf("大小写不敏感匹配失败：%v", got)
	}
	_, rows = a.searchNDJSON(t, sid, root, "hidden")
	if got := hitNames(rows); !equalStrings(got, []string{".hidden.txt"}) {
		t.Errorf("隐藏文件应当能搜到：%v", got)
	}

	// ---- 只在这个目录的子树里搜：换个起点就搜不到外面的东西 ----
	_, rows = a.searchNDJSON(t, sid, filepath.Join(root, "docs"), "hello")
	if got := hitNames(rows); len(got) != 0 {
		t.Errorf("在 docs 下搜 hello 不该命中外面的文件：%v", got)
	}
	if done := searchRowOf(t, rows, "done"); done.Total != 0 || done.Scanned == 0 {
		t.Errorf("没命中也要回一份完整统计（界面显示「没找到」）：%+v", done)
	}

	// ---- 起点为空 = 家目录（与 fs/list 同一套解析） ----
	_, rows = a.searchNDJSON(t, sid, "", "deep-app")
	if got := hitNames(rows); !equalStrings(got, []string{"deep-app.log"}) {
		t.Errorf("空 path 应当按家目录搜：%v", got)
	}
	if rows[0].Path != root {
		t.Errorf("空 path 的 start.path = %q，期望家目录 %q", rows[0].Path, root)
	}

	// ---- 相对路径（非绝对）也按家目录解析 ----
	_, rows = a.searchNDJSON(t, sid, "notes", "APP")
	if got := hitNames(rows); !equalStrings(got, []string{"APP.log"}) {
		t.Errorf("相对起点没生效：%v", got)
	}
}

// TestFsSearchStreamsInBatches 断言「搜到一点显示一点」在数据上的形态：结果**分批**到达、
// 不重不漏。150 个命中（> searchBatchSize=50）必须分成多条 batch 记录。
func TestFsSearchStreamsInBatches(t *testing.T) {
	a := newTestApp(t, appOptions{})
	root := a.sshRoot
	names := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		name := "batch-" + pad3(i) + "-hit.txt"
		writeLocal(t, filepath.Join(root, name), "x")
		names = append(names, name)
	}
	// 干扰项：名字里没有关键词，不该出现在结果里
	for i := 0; i < 20; i++ {
		writeLocal(t, filepath.Join(root, "other-"+pad3(i)+".txt"), "y")
	}
	sid := a.newSSHSession(t)

	_, rows := a.searchNDJSON(t, sid, root, "hit")
	var batches int
	for _, row := range rows {
		if row.Type == "batch" {
			batches++
		}
	}
	if batches < 3 {
		t.Fatalf("150 条结果应当分成多条 batch 记录（每批 %d 条），实际 %d 条", searchBatchSize, batches)
	}
	got := hitPaths(rows)
	if len(got) != len(names) {
		t.Fatalf("结果 %d 条，期望 %d 条", len(got), len(names))
	}
	seen := map[string]bool{}
	gotNames := make([]string, 0, len(got))
	for _, p := range got {
		if seen[p] {
			t.Fatalf("结果里有重复项（攒批时重复发送了）：%s", p)
		}
		seen[p] = true
		gotNames = append(gotNames, filepath.Base(p))
	}
	sort.Strings(names)
	if !equalStrings(sortedCopy(gotNames), names) {
		t.Errorf("结果集合不对：%v", sortedCopy(gotNames))
	}
	if done := searchRowOf(t, rows, "done"); done.Total != 150 || done.Truncated {
		t.Errorf("done = %+v，期望 150 条且未截断", done)
	}
}

// TestFsSearchErrors 覆盖「开流之前」的失败：这些必须走普通 JSON 错误（带状态码），
// 前端据此报错；而不是流里塞一行错误。
func TestFsSearchErrors(t *testing.T) {
	a, sid := searchTestApp(t)

	// 关键词为空 / 全空白：两个方向都拦住（前端也会拦，但接口不能只靠前端）
	for _, q := range []string{"", "   ", "\t\n"} {
		res, rows := a.failSearch(t, sid, a.sshRoot, q)
		if len(rows) != 0 {
			t.Errorf("关键词 %q 应当直接以 JSON 错误回，而不是开流：%+v", q, rows)
		}
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("关键词 %q 的状态码 = %d，期望 400", q, res.StatusCode)
		}
	}

	// 路径逃逸
	res, _ := a.failSearch(t, sid, a.sshRoot+"/../etc", "x")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("含 .. 的路径状态码 = %d，期望 400", res.StatusCode)
	}

	// 不存在的会话 / 未打开的会话
	res, _ = a.failSearch(t, "no-such-session", a.sshRoot, "x")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的会话状态码 = %d，期望 404", res.StatusCode)
	}

	// 起点不是目录（拿一个文件当起点）：开流之前失败，界面报「读取目录失败」而不是「没找到」
	res, rows := a.failSearch(t, sid, filepath.Join(a.sshRoot, "hello.txt"), "x")
	if res.StatusCode != http.StatusOK {
		// 有些 SFTP 实现（含 pkg/sftp）在 ReadDir 一个文件时回的是协议错误，映射成 502；
		// 这里只要求「不是 200 的失败」或「流里的 error 行」二者之一，别把实现细节钉死
		if len(rows) != 0 {
			t.Fatalf("既不是成功流也不是普通错误：%d %+v", res.StatusCode, rows)
		}
		return
	}
	if last := rows[len(rows)-1]; last.Type != "error" {
		t.Errorf("拿文件当起点应当报错，实际：%+v", rows)
	}
}

// failSearch 发一次**期望失败**的搜索：返回响应与（若后端开了流）解析出的记录。
func (a *testApp) failSearch(t *testing.T, sid, path, query string) (*http.Response, []searchRow) {
	t.Helper()
	res, rows := a.searchNDJSON(t, sid, path, query)
	if len(rows) == 0 {
		// 没开流：正文必须是标准的 JSON 错误体（ok=false + code + error）
		if res.StatusCode == http.StatusOK {
			t.Fatalf("搜索既没开流又回了 200")
		}
		return res, rows
	}
	// 开了流：最后一行的类型由调用方判断，这里只保证没有 HTTP 5xx 混在里面
	if res.StatusCode != http.StatusOK {
		t.Fatalf("开流的响应状态码应当是 200，实际 %d", res.StatusCode)
	}
	return res, rows
}

// TestFsSearchPermission 覆盖权限的两种表现：
//   - 起点目录读不出来 → 开流之前的 permission（403），界面报错而不是「没找到」；
//   - 子树里的一个目录读不出来 → **跳过并继续**（skipped=1），其余结果照常给。
//
// 权限位在两类环境下不生效，直接 skip（与 TestFsEditPermissionDenied 同一套判据）。
func TestFsSearchPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行：权限位挡不住 ReadDir，跳过")
	}
	dir := permEnforcingDir(t)
	if dir == "" {
		t.Skip("找不到会执行权限位的文件系统：无法模拟「没有权限」，跳过")
	}
	root, err := os.MkdirTemp(dir, "webssh-search-perm-")
	if err != nil {
		t.Fatalf("建临时目录失败：%v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	a := newTestApp(t, appOptions{sshOptions: &testssh.Options{User: "demo", Password: "demo", Root: root}})
	writeLocal(t, filepath.Join(root, "found.log"), "found\n")
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "deep"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	writeLocal(t, filepath.Join(locked, "deep", "hidden-app.log"), "x\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod 失败：%v", err)
	}
	defer os.Chmod(locked, 0o755)
	// 直接确认「ReadDir 真的会被拒」——这才是这条断言的前提（卷可能不执行权限位）
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("这个文件系统拦不住 0000 目录的读取：无法模拟「读不出目录」，跳过")
	}
	sid := a.newSSHSession(t)

	// 起点就是这个没权限的目录：拦在开流之前，前端显示「读取目录失败：权限不足」
	res, rows := a.searchNDJSON(t, sid, locked, "app")
	if len(rows) != 0 {
		t.Fatalf("起点读不出来时不该开流：%+v", rows)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("起点没权限的状态码 = %d，期望 403", res.StatusCode)
	}
	// 正文是标准的 JSON 错误体
	_, out := a.post(t, "/api/fs/search", map[string]any{"sid": sid, "path": locked, "query": "app"})
	if out["ok"] != false || out["code"] != "permission" {
		t.Errorf("错误体 = %v，期望 ok=false / permission", out)
	}

	// 起点是家目录、子树里有一个读不出来的目录：跳过它，别的照常给
	_, rows = a.searchNDJSON(t, sid, root, "log")
	if got := hitNames(rows); !equalStrings(got, []string{"found.log"}) {
		t.Errorf("读不出子目录时应当跳过它并继续：%v", got)
	}
	done := searchRowOf(t, rows, "done")
	if done.Skipped != 1 {
		t.Errorf("done.skipped = %d，期望 1（有一个目录被跳过）", done.Skipped)
	}
	if done.Total != 1 {
		t.Errorf("done.total = %d，期望 1", done.Total)
	}
}

// pad3 把序号补成三位，便于按名字排序时顺序稳定（01、02 … 09、10）。
func pad3(i int) string {
	s := []byte("000")
	for pos := 2; pos >= 0 && i > 0; pos-- {
		s[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(s)
}
