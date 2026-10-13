package main

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 快捷指令（全局 + 会话专属）
//
// 存储：quickcmds/global.json 与 quickcmds/<sid>.json，都是「整表提交 / 整表返回」。
// 前端每次改动（增、删、排序）都把完整列表发回来，我们只做校验与落盘——这样排序、
// 去重这类规则只在前端实现一遍，后端不会多出一份「部分更新」的语义要对齐。
// ---------------------------------------------------------------------------

// maxQuickCmds 是一张表的上限。前端已经把整张表渲染成一列按钮，几千条既没法用，
// 也会把响应体撑大；给一个明确的拒绝比默默接受更好。
const maxQuickCmds = 500

// quickCmd 是 QuickCmd（契约第 4.3 节）。
type quickCmd struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Auto    bool   `json:"auto"`
}

type quickCmdsFile struct {
	Version int `json:"version"`
	// Commands 用指针切片没意义（null 与 [] 在前端都当空处理），统一在读取时归一成 []。
	Commands []quickCmd `json:"commands"`
}

// loadQuickCmds / saveQuickCmds 是 main 包的自由函数（不是 store.Store 的方法）：
// quickCmd 与 apierr.Error 都属于 API 层，store 包只提供通用的 ReadJSON/WriteJSON 与路径。
func loadQuickCmds(st *store.Store, sid string) ([]quickCmd, *apierr.Error) {
	var f quickCmdsFile
	err := st.ReadJSON(st.QuickCmdsPath(sid), &f)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		// 还没有任何指令：正常的空状态，不是错误。
	default:
		return nil, apierr.Internal("读取快捷指令失败：%v", err)
	}
	if f.Commands == nil {
		f.Commands = []quickCmd{}
	}
	return f.Commands, nil
}

func saveQuickCmds(st *store.Store, sid string, cmds []quickCmd) *apierr.Error {
	if err := st.WriteJSON(st.QuickCmdsPath(sid), quickCmdsFile{Version: 1, Commands: cmds}, 0o600); err != nil {
		return apierr.Internal("保存快捷指令失败：%v", err)
	}
	return nil
}

// sanitizeQuickCmds 校验并规整整张表：补齐缺失的 id、去重、去空白。
//
// 自动补 id 而不是拒绝：新增一条指令时前端可能只发 {name, content, auto}，
// 为「没带 id」回一个 400 只会让用户看到一句没头没脑的报错。
func sanitizeQuickCmds(in []quickCmd) ([]quickCmd, *apierr.Error) {
	if len(in) > maxQuickCmds {
		return nil, apierr.TooLarge("快捷指令最多 %d 条（收到 %d 条）", maxQuickCmds, len(in))
	}
	out := make([]quickCmd, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		c.Name = strings.TrimSpace(c.Name)
		c.Content = strings.TrimSpace(c.Content)
		if c.Name == "" || c.Content == "" {
			return nil, apierr.Invalid("快捷指令需要名称与命令内容")
		}
		if c.ID == "" || !store.ValidID(c.ID) {
			c.ID = store.NewID()
		}
		if seen[c.ID] {
			// 同一 id 出现两次：保留先出现的那条（前端按 id 做列表 key，重复 id 会让渲染错乱）。
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out, nil
}

func (m *AdminMux) quickCmdScope(sid string) (*store.Session, *apierr.Error) {
	if sid == "" {
		return nil, nil // 全局
	}
	if !store.ValidID(sid) {
		return nil, errSessionGone("会话不存在")
	}
	sc, ok := m.sessions.getConfig(sid)
	if !ok {
		return nil, errSessionGone("会话不存在（可能已被删除）：%s", sid)
	}
	return sc, nil
}

// handleQuickCmdsList 读取一张表（sid 为空 = 全局）。
func (m *AdminMux) handleQuickCmdsList(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID string `json:"sid"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if _, apiErr := m.quickCmdScope(req.SID); apiErr != nil {
		return apiErr
	}
	cmds, apiErr := loadQuickCmds(m.st, req.SID)
	if apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "scope": scopeOf(req.SID), "commands": cmds})
}

// handleQuickCmdsSave 整表保存。
func (m *AdminMux) handleQuickCmdsSave(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID      string     `json:"sid"`
		Commands []quickCmd `json:"commands"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	if _, apiErr := m.quickCmdScope(req.SID); apiErr != nil {
		return apiErr
	}
	cmds, apiErr := sanitizeQuickCmds(req.Commands)
	if apiErr != nil {
		return apiErr
	}
	if apiErr := saveQuickCmds(m.st, req.SID, cmds); apiErr != nil {
		return apiErr
	}
	return apierr.Respond(w, map[string]any{"ok": true, "scope": scopeOf(req.SID), "commands": cmds})
}

// scopeOf 是响应里的 scope 字段：`global` / `session`（前端只用它区分两张表）。
func scopeOf(sid string) string {
	if sid == "" {
		return "global"
	}
	return "session"
}
