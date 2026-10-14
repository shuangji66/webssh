package main

import (
	"context"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// SFTP 文件操作 API（契约第 4.2 节的 /api/fs/*）
//
// 三条贯穿全部实现的约束：
//  1. 目标路径一律 path.Clean 并**拒绝 `..`**：SFTP 服务端也会拦，但我们要给出清晰的中文
//     错误，而不是让用户看到一句 "sftp: \"\""。
//  2. 每个会话一条 SFTP 通道、懒加载、失败重连一次，且所有文件操作串行化（见 withSFTP）。
//  3. 客户端取消（关页面 / 点取消）时 r.Context() 结束：正在进行的写入必须能中止，
//     写了一半的文件要删掉。
// ---------------------------------------------------------------------------

type fsReq struct {
	SID  string `json:"sid"`
	Path string `json:"path"`
}

// handleFsList 列目录：path 为空时返回家目录，并把 home 一并回给前端（面包屑的「家目录」入口）。
func (m *AdminMux) handleFsList(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var (
		resolved string
		home     string
		entries  []fsEntry
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		var err error
		home, err = sess.homeDir()
		if err != nil {
			return err
		}
		resolved, err = resolveRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		infos, err := sess.client.ReadDir(resolved)
		if err != nil {
			return err
		}
		entries = make([]fsEntry, 0, len(infos))
		for _, fi := range infos {
			entries = append(entries, buildFsEntry(sess.client, resolved, fi))
		}
		return nil
	})
	if opErr != nil {
		return fsError("读取目录失败", opErr)
	}
	sortEntries(entries)
	return apierr.Respond(w, map[string]any{"ok": true, "path": resolved, "home": home, "entries": entries})
}

// fsInfo 是「信息」弹窗的数据（契约 15.5）：条目自身的属性 + 目录**直接子项**的计数。
//
// 为什么不复用 /api/fs/list：
//  1. 弹窗问的是「**这一个**路径」的属性，而列目录给出的是「它里面的东西」——当前目录自己
//     从来不在自己的列表里；
//  2. 「内含多少目录和文件」在服务端顺手数一遍最省：把整份列表传到浏览器再数一遍是白费流量。
//
// 刻意**不做递归统计**（用户确认只要直接子项）：递归在远程可能要几十分钟，而这个弹窗是
// 一个「点开就想看一眼」的东西，没有进度语义。
type fsInfo struct {
	fsEntry
	// UID / GID 是 SFTP attrs 里的属主（v3 只有数字，没有名字）。
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
	// Owner / Group 是把 UID/GID 在**远端**查出来的账号/组名；查不到就是空串，
	// 界面退回显示数字（绝不编一个名字出来）。见 resolveOwnerNames。
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	// DirCount / FileCount 是直接子项的计数；只在 Dir 为真时有意义。
	//
	// 口径与列表一致：**跟随软链接**（指向目录的软链接算目录、失效链接算文件），
	// 否则同一个目录在列表里显示成目录、在信息里被算成文件，用户会以为是 bug。
	DirCount  int `json:"dirCount"`
	FileCount int `json:"fileCount"`
	// CountError 非空 = 数子项时失败（多半是权限不足）：界面如实说「统计失败：<原因>」，
	// 而**不是**把 0 当成「空目录」显示出去。
	CountError string `json:"countError,omitempty"`
}

// handleFsInfo 取单个路径的信息（文件管理器的「信息」菜单项）。
func (m *AdminMux) handleFsInfo(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var info fsInfo
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		resolved, err := resolveRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		fi, err := sess.client.Lstat(resolved)
		if err != nil {
			return err
		}
		// buildFsEntry 自己拼 path.Join(dir, fi.Name())，所以这里给父目录；根目录特殊但成立：
		// path.Dir("/") == "/" 且 path.Base("/") == "/"，join 回来还是 "/"。
		info = fsInfo{fsEntry: buildFsEntry(sess.client, path.Dir(resolved), fi)}
		if raw, ok := fi.Sys().(*sftp.FileStat); ok {
			info.UID, info.GID = raw.UID, raw.GID
		}
		if !info.Dir {
			return nil
		}
		children, err := sess.client.ReadDir(resolved)
		if err != nil {
			// 读不出子项不是「空目录」：把原因带回去，由界面说清楚。
			info.CountError = err.Error()
			return nil
		}
		for _, child := range children {
			if childIsDir(sess.client, resolved, child) {
				info.DirCount++
			} else {
				info.FileCount++
			}
		}
		return nil
	})
	if opErr != nil {
		return fsError("读取信息失败", opErr)
	}
	// 属主名字在**SFTP 锁之外**查：它要另拨一条 SSH 跑 `id`，没必要占着这条会话的 SFTP 通道
	// （同会话的文件操作是串行的，占住一百多毫秒就是一百多毫秒）。
	info.Owner, info.Group = rt.resolveOwnerNames(r.Context(), info.UID, info.GID)
	return apierr.Respond(w, map[string]any{"ok": true, "info": info})
}

// resolveOwnerNames 在远端把 uid/gid 查成账号名与组名（`id -nu` / `id -ng`）。
//
// 为什么值得多跑一条命令：SFTP v3 的 attrs 里只有数字 uid/gid，而「这个文件是谁的」几乎总是
// 想知道*名字*（权限问题一半是「属主是 root，所以我写不进去」）。查不到（远端没有 id、
// 账号已删、或这条命令超时）一律返回空串，由界面退回显示数字——**绝不编一个名字**。
//
// 结果按会话缓存（`resolveOwnerNames` 每次都要**新拨一条 SSH** 跑命令，而「信息」弹窗
// 可能被反复打开）：只有查成功才写缓存，失败下次照旧重试（超时不该被永久记住）。
//
// 用 `printf` 把两行结果显式分开：`id -nu` 失败时不能把组名当成账号名读进来。
func (r *sessionRuntime) resolveOwnerNames(ctx context.Context, uid, gid uint32) (string, string) {
	if r == nil {
		return "", ""
	}
	if owner, group, ok := r.cachedOwnerNames(uid, gid); ok {
		return owner, group
	}
	cmd := "printf '%s\n' \"$(id -nu " + strconv.FormatUint(uint64(uid), 10) +
		" 2>/dev/null)\" \"$(id -ng " + strconv.FormatUint(uint64(gid), 10) + " 2>/dev/null)\""
	res, err := r.runRemote(ctx, cmd)
	if err != nil {
		// 查名字失败不该让「信息」整个失败：uid/gid 已经有了，界面显示数字就够用
		logx.Warnf("会话 %s 查属主名字失败（uid=%d gid=%d）：%v", r.id, uid, gid, err)
		return "", ""
	}
	// 命令**没跑成**（非 0 退出：远端没有 id、sh 起不来、连接半死）时不能写缓存：
	// runRemote 只在「基础设施失败」上给 error，命令自己失败是 Code != 0，而那份 stdout
	// 是空的——按「查不到」处理（界面退回数字），但下次还要重试。缓存里记下一次瞬时失败，
	// 这个会话就永远只显示数字了（与文件头「只有查成功才写缓存」的承诺不符）。
	if !res.ok() {
		logx.Warnf("会话 %s 查属主名字的命令退出码 %d（uid=%d gid=%d），本次按查不到处理（不缓存）",
			r.id, res.Code, uid, gid)
		return "", ""
	}
	lines := strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n")
	owner, group := "", ""
	if len(lines) > 0 {
		owner = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		group = strings.TrimSpace(lines[1])
	}
	r.cacheOwnerNames(uid, gid, owner, group)
	return owner, group
}

// cachedOwnerNames 取缓存的属主 / 组名；命中才返回 ok。
func (r *sessionRuntime) cachedOwnerNames(uid, gid uint32) (string, string, bool) {
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()
	pair, ok := r.ownerNames[ownerKey{uid: uid, gid: gid}]
	if !ok {
		return "", "", false
	}
	return pair[0], pair[1], true
}

// cacheOwnerNames 记下一次成功的查询结果。
func (r *sessionRuntime) cacheOwnerNames(uid, gid uint32, owner, group string) {
	r.ownerMu.Lock()
	defer r.ownerMu.Unlock()
	if r.ownerNames == nil {
		r.ownerNames = map[ownerKey][2]string{}
	}
	r.ownerNames[ownerKey{uid: uid, gid: gid}] = [2]string{owner, group}
}

// childIsDir 判断一条 ReadDir 结果是不是目录，规则与 buildFsEntry 保持一致：软链接要跟随一次
// （指向目录的算目录、失效的算文件）。只为软链接多发一次 Stat——普通文件/目录不必花这个往返。
func childIsDir(c *sftp.Client, dir string, fi os.FileInfo) bool {
	if fi.IsDir() {
		return true
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := c.Stat(path.Join(dir, fi.Name()))
	return err == nil && target.IsDir()
}

// buildFsEntry 把一条 ReadDir/Lstat 结果翻成 FsEntry。
//
// 符号链接单独处理：ReadDir / Lstat 拿到的是链接本身（mode 是 Lrwxrwxrwx），而界面要的是
// 「这是个软链接 + 它指向哪 + 是不是失效链接 + 能不能进去」。
func buildFsEntry(c *sftp.Client, dir string, fi os.FileInfo) fsEntry {
	full := path.Join(dir, fi.Name())
	e := fsEntry{
		Name:  fi.Name(),
		Path:  full,
		Dir:   fi.IsDir(),
		Size:  fi.Size(),
		Mode:  fi.Mode().String(),
		MTime: fi.ModTime().Format(time.RFC3339),
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return e
	}
	if target, err := c.ReadLink(full); err == nil {
		e.Symlink = target
	}
	target, err := c.Stat(full)
	if err != nil {
		// 链接指向的目标不存在：界面显示「失效链接」，且不允许进去（进去只会得到错误）。
		e.Broken = true
		e.Dir = false
		return e
	}
	e.Dir = target.IsDir()
	e.Size = target.Size()
	e.MTime = target.ModTime().Format(time.RFC3339)
	return e
}

// sortEntries 目录在前、其余按名称（忽略大小写）排序。
func sortEntries(entries []fsEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		li, lj := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if li != lj {
			return li < lj
		}
		return entries[i].Name < entries[j].Name
	})
}
