package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// 快捷访问（quickaccess.go，第二十轮）
//
// 两条硬判据：
//  1. 路径必须**原样**保存与返回（含末尾空格——那个目录名是合法的，而本仓库已经在
//     `resolveRemotePath` 上被这条坑过一次），只有「重复斜杠 / 末尾斜杠 / `.` 段」会被规整；
//  2. 表按会话存，`/api/quickaccess/*` 只认**存在**的会话 id：sid 为空是参数错误，
//     会话被删之后回 session_gone（前端据此提示「该会话已不存在」）。
// ---------------------------------------------------------------------------

func TestSanitizeQuickPaths(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/", "/", true},
		{"/vol1/1000", "/vol1/1000", true},
		// 重复斜杠 / 末尾斜杠 / `.` 段都规整掉（同一个目录的两种写法不该变成两条）
		{"/vol1//1000/", "/vol1/1000", true},
		{"/vol1/./1000", "/vol1/1000", true},
		// **末尾空格必须留下**：`/vol1/my dir ` 与 `/vol1/my dir` 是两个目录
		{"/vol1/my dir ", "/vol1/my dir ", true},
		// 相对路径 / `..` 一律拒绝（悄悄 Clean 掉会指向另一个目录，比报错更糟）
		{"vol1/1000", "", false},
		{"/vol1/../etc", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, apiErr := sanitizeQuickPath(c.in)
		if c.ok && apiErr != nil {
			t.Fatalf("sanitizeQuickPath(%q) 期望成功，实际报错：%v", c.in, apiErr.Message)
		}
		if !c.ok {
			if apiErr == nil {
				t.Fatalf("sanitizeQuickPath(%q) 期望失败，实际得到 %q", c.in, got)
			}
			if apiErr.Code != "invalid" {
				t.Fatalf("sanitizeQuickPath(%q) 的错误码 = %s，期望 invalid", c.in, apiErr.Code)
			}
		}
		if apiErr == nil && got != c.want {
			t.Fatalf("sanitizeQuickPath(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 整表：顺序保留、按规整后的结果去重（保留先出现的那条）
	got, apiErr := sanitizeQuickPaths([]string{"/b/", "/a//c", "/a", "/a", "/"})
	if apiErr != nil {
		t.Fatalf("sanitizeQuickPaths 期望成功，实际报错：%v", apiErr.Message)
	}
	want := []string{"/b", "/a/c", "/a", "/"}
	if len(got) != len(want) {
		t.Fatalf("sanitizeQuickPaths = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sanitizeQuickPaths = %v，期望 %v", got, want)
		}
	}

	// 上限：多一条就整表拒绝（不是截断——截断会让用户以为自己加上了）
	many := make([]string, maxQuickAccess+1)
	for i := range many {
		many[i] = filepath.Join("/p", string(rune('a'+i%26)))
	}
	if _, apiErr := sanitizeQuickPaths(many); apiErr == nil || apiErr.Code != "too_large" {
		t.Fatalf("超过上限的表期望 too_large，实际：%v", apiErr)
	}
}

// TestQuickAccessApi 覆盖 list / save / 重启后仍在 / 各类错误码 / 删会话时清理。
func TestQuickAccessApi(t *testing.T) {
	a := newTestApp(t, appOptions{})
	sid := a.newSSHSession(t)

	// 还没加过：空表（不是错误）
	out := a.ok(t, "/api/quickaccess/list", map[string]any{"sid": sid})
	if paths, ok := out["paths"].([]any); !ok || len(paths) != 0 {
		t.Fatalf("新会话的快捷访问期望空表，实际：%v", out["paths"])
	}

	// 保存：规整 + 去重；末尾空格必须原样回来
	saved := a.ok(t, "/api/quickaccess/save", map[string]any{"sid": sid, "paths": []any{
		"/vol1/1000/docker/",
		"/",
		a.sshRoot,
		"/vol1/1000/docker",
		a.sshRoot + "/tail ",
	}})
	paths := strList(t, saved["paths"])
	want := []string{"/vol1/1000/docker", "/", a.sshRoot, a.sshRoot + "/tail "}
	if len(paths) != len(want) {
		t.Fatalf("保存后返回的 paths = %v，期望 %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("保存后返回的 paths = %v，期望 %v", paths, want)
		}
	}

	// 磁盘上的形态：quickaccess/<sid>.json，路径明文（不做加密——路径不是秘密）
	var f quickAccessFile
	raw := readLocal(t, filepath.Join(a.renv.DataDir, "quickaccess", sid+".json"))
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("解析 quickaccess/%s.json 失败：%v\n%s", sid, err, raw)
	}
	if f.Version != 1 || len(f.Paths) != len(want) || f.Paths[3] != a.sshRoot+"/tail " {
		t.Fatalf("磁盘上的快捷访问 = %+v，期望 version=1 paths=%v", f, want)
	}

	// 重启后仍在（数据目录才是唯一来源，前端不缓存）
	b := a.restart(t)
	reloaded := strList(t, b.ok(t, "/api/quickaccess/list", map[string]any{"sid": sid})["paths"])
	if len(reloaded) != len(want) {
		t.Fatalf("重启后读回 = %v，期望 %v", reloaded, want)
	}

	// 错误码：sid 为空是参数错误（这里没有全局表）、会话不存在是 session_gone、
	// 路径本身非法是 invalid
	b.fail(t, "/api/quickaccess/list", map[string]any{"sid": ""}, "invalid", 400)
	b.fail(t, "/api/quickaccess/save", map[string]any{"sid": ""}, "invalid", 400)
	b.fail(t, "/api/quickaccess/save", map[string]any{"sid": "no-such-session", "paths": []any{}},
		"session_gone", 404)

	b.fail(t, "/api/quickaccess/save", map[string]any{"sid": sid, "paths": []any{"vol1"}},
		"invalid", 400)
	b.fail(t, "/api/quickaccess/save", map[string]any{"sid": sid, "paths": []any{"/vol1/../etc"}},
		"invalid", 400)
	b.fail(t, "/api/quickaccess/save", map[string]any{"sid": sid, "paths": []any{""}},
		"invalid", 400)

	// 删会话时一并清掉（列表里的路径属于这台远程主机，会话没了它们就没有意义）
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "quickaccess", sid+".json")); err != nil {
		t.Fatalf("删除会话前该文件就应该在：%v", err)
	}
	b.ok(t, "/api/sessions/delete", map[string]any{"id": sid})
	if _, err := os.Stat(filepath.Join(a.renv.DataDir, "quickaccess", sid+".json")); !os.IsNotExist(err) {
		t.Fatalf("删除会话后快捷访问文件应被清掉，实际：%v", err)
	}
	b.fail(t, "/api/quickaccess/list", map[string]any{"sid": sid}, "session_gone", 404)
}

// strList 把响应里的 paths（`[]any`）转成 []string 并断言元素都是字符串。
func strList(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("paths 不是数组：%#v", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("paths 里有非字符串元素：%#v", item)
		}
		out = append(out, s)
	}
	return out
}
