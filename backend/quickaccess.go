package main

import (
	"errors"
	"net/http"
	"os"
	"path"
	"strings"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 快捷访问（第二十轮）
//
// 用户在文件管理/目录树里把常去的目录收进「快捷访问」，之后一次点击就能跳过去。
//
// 存储：`quickaccess/<sid>.json`，与快捷指令一样是**整表提交 / 整表返回**（前端每次改动
// 都发整份列表回来，后端只做校验与落盘）。这样「去重、规整」这类规则只有一处实现，
// 后端不必为「加一条 / 删一条」再定义一套部分更新语义。
//
// **按会话存**、没有全局表：路径是远程主机上的绝对路径，而会话身份（user@host:port）
// 天然去重，所以「按会话」= 「按主机」。第三轮之前前端只用 localStorage 存界面偏好
// （视图 / 排序），那是「这台设备上的人怎么看」，而快捷访问是用户数据，必须跟着数据目录走
// （换设备、清浏览器数据都还在），并且要能被「删除会话」一并清掉。
//
// 这里**不做加密**（与快捷指令的命令内容不同）：目录路径不是秘密，加密只会让它没法排查。
// ---------------------------------------------------------------------------

// maxQuickAccess 是一个会话的快捷访问条数上限。弹窗是一列可点的路径，几百条既没法用，
// 也会把响应体撑大；给一个明确的拒绝比默默接受更好。
const maxQuickAccess = 200

// maxQuickAccessPathLen 是单条路径的长度上限（远程路径本身的上限远小于此，这里只是不让
// 请求体被一条超长字符串打发）。
const maxQuickAccessPathLen = 4096

type quickAccessFile struct {
	Version int      `json:"version"`
	Paths   []string `json:"paths"`
}

// sanitizeQuickPath 规整一条路径：必须是绝对路径、不含 `..` 段，清掉重复斜杠与末尾斜杠。
//
// **不 trim**：以空格结尾的目录名是合法的，而本仓库已经被这条坑过一次（`resolveRemotePath`
// 里那句「不要 TrimSpace」）——把 `/vol1/1000/my dir ` 改成 `/vol1/1000/my dir` 之后，
// 这条快捷访问指向的是一个不存在的目录，症状是「我明明存的就是那个目录，点开说没有」。
// 用户手输时误带的首尾空白由前端在**输入框那一层**处理（见 QuickAccessDialog 的注释）。
func sanitizeQuickPath(raw string) (string, *apierr.Error) {
	if raw == "" {
		return "", apierr.Invalid("快捷访问路径不能为空")
	}
	if len(raw) > maxQuickAccessPathLen {
		return "", apierr.TooLarge("快捷访问路径太长（上限 %d 字符）", maxQuickAccessPathLen)
	}
	if !strings.HasPrefix(raw, "/") {
		return "", apierr.Invalid("快捷访问路径必须是绝对路径（以 / 开头）：%s", raw)
	}
	for _, seg := range strings.Split(raw, "/") {
		// `..` 一律拒绝，而不是靠 path.Clean 折掉：Clean 之后的路径与用户写下的是两个目录，
		// 悄悄改掉比报错更糟（与 fs 系列接口同一个口径）。
		if seg == ".." {
			return "", apierr.Invalid("快捷访问路径不能包含 .. 段：%s", raw)
		}
	}
	return path.Clean(raw), nil
}

// sanitizeQuickPaths 校验并规整整张表：逐条 sanitizeQuickPath + 按规整后的结果去重。
//
// 去重保留**先出现**的那条：前端按路径做列表 key，重复路径会让渲染错乱（点第一行删掉的是
// 第二行）。静默去重而不是报错，是因为「重复添加同一个目录」在界面上是正常操作——前端会
// 先提示「已经在列表里」，但两个设备同时加同一条时后端也必须收敛到一条。
func sanitizeQuickPaths(in []string) ([]string, *apierr.Error) {
	if len(in) > maxQuickAccess {
		return nil, apierr.TooLarge("快捷访问最多 %d 条（收到 %d 条）", maxQuickAccess, len(in))
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		cleaned, apiErr := sanitizeQuickPath(raw)
		if apiErr != nil {
			return nil, apiErr
		}
		if seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	return out, nil
}

// loadQuickAccess 读一个会话的表。文件不存在（还没加过）按空表处理——那是正常状态。
func loadQuickAccess(st *store.Store, sid string) ([]string, *apierr.Error) {
	var f quickAccessFile
	if err := st.ReadJSON(st.QuickAccessPath(sid), &f); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, apierr.Internal("读取快捷访问失败：%v", err)
	}
	// 磁盘上的内容不可信（手工编辑过、旧版本写过别的形状）：逐条重新规整，认不出来就整个
	// 文件报错——静默丢掉一条等于「我的快捷访问自己没了」。
	paths, apiErr := sanitizeQuickPaths(f.Paths)
	if apiErr != nil {
		return nil, apierr.Internal("快捷访问文件 %s 里有非法路径：%v",
			st.QuickAccessPath(sid), apiErr.Error())
	}
	return paths, nil
}

// saveQuickAccess 整表落盘（0600：路径不是秘密，但没必要让同机其他用户读到用户的目录结构）。
func saveQuickAccess(st *store.Store, sid string, paths []string) *apierr.Error {
	if err := st.WriteJSON(st.QuickAccessPath(sid), quickAccessFile{Version: 1, Paths: paths}, 0o600); err != nil {
		return apierr.Internal("保存快捷访问失败：%v", err)
	}
	return nil
}

// quickAccessScope 校验 sid 并确认会话还在。
//
// 与快捷指令不同：这里**没有全局表**，sid 为空是参数错误（不是「全局作用域」）；会话已被
// 删除时回 session_gone（前端会提示「该会话已不存在」）。
func (m *AdminMux) quickAccessScope(sid string) *apierr.Error {
	if sid == "" || !store.ValidID(sid) {
		return apierr.Invalid("缺少会话 id")
	}
	if _, ok := m.sessions.getConfig(sid); !ok {
		return errSessionGone("会话不存在（可能已被删除）：%s", sid)
	}
	return nil
}

// handleQuickAccessList 读一个会话的快捷访问表。
func (m *AdminMux) handleQuickAccessList(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID string `json:"sid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.quickAccessScope(req.SID); apiErr != nil {
		return apiErr
	}
	paths, apiErr := loadQuickAccess(m.st, req.SID)
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "paths": paths})
}

// handleQuickAccessSave 整表保存。
func (m *AdminMux) handleQuickAccessSave(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID   string   `json:"sid"`
		Paths []string `json:"paths"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if apiErr := m.quickAccessScope(req.SID); apiErr != nil {
		return apiErr
	}
	paths, apiErr := sanitizeQuickPaths(req.Paths)
	if apiErr != nil {
		return apiErr
	}
	if apiErr := saveQuickAccess(m.st, req.SID, paths); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "paths": paths})
}
