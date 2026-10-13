package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"
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

// handleFsMkdir 新建目录。已存在 → duplicate（前端提示「已存在」，而不是静默成功——
// 静默成功会让用户以为新建的是一个空目录，实际进去看到的是别人的内容）。
func (m *AdminMux) handleFsMkdir(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if _, err := sess.client.Lstat(target); err == nil {
			return os.ErrExist
		}
		return sess.client.Mkdir(target)
	})
	if opErr != nil {
		return fsError("新建目录失败", opErr)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleFsCreate 新建空文件（已存在 → duplicate）。
//
// 用 O_EXCL：pkg/sftp 的 Create 是 O_CREATE|O_TRUNC，同名会把用户那份**清成 0 字节**再回
// 一个成功——「新建文件」时手滑打了已有文件名，内容就没了，还没有确认。
func (m *AdminMux) handleFsCreate(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if _, err := sess.client.Lstat(target); err == nil {
			return os.ErrExist
		}
		f, err := sess.client.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if opErr != nil {
		return fsError("新建文件失败", opErr)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleFsRename 重命名 / 移动。目标已存在 → duplicate。
//
// SFTP 的 rename 是 POSIX rename：会**直接覆盖**同名目标（pkg/sftp 还提供 PosixRename）。
// 不先查一次存在性的话，「重命名」两个同目录下的文件会把其中一个静默吞掉。
func (m *AdminMux) handleFsRename(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID  string `json:"sid"`
		Path string `json:"path"`
		To   string `json:"to"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		from, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(req.To) == "" {
			return errors.New("缺少 path")
		}
		to, err := resolveRemotePath(sess, req.To)
		if err != nil {
			return err
		}
		if from == to {
			return nil
		}
		if _, err := sess.client.Lstat(to); err == nil {
			return os.ErrExist
		}
		return sess.client.Rename(from, to)
	})
	if opErr != nil {
		return fsError("重命名失败", opErr)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

// handleFsRemove 删除。dir 必须与真实类型一致：界面把「删除」与「删除目录」分成两个入口，
// 参数传错说明前端状态与实际类型不一致，此时**报错比删掉更安全**（否则可能删掉整棵树）。
func (m *AdminMux) handleFsRemove(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID  string `json:"sid"`
		Path string `json:"path"`
		Dir  bool   `json:"dir"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if target == "/" {
			return errRefuseRoot
		}
		fi, err := sess.client.Lstat(target)
		if err != nil {
			return err
		}
		isDir := fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
		if isDir != req.Dir {
			return fmt.Errorf("%w（%s 是%s）", errKindMismatch, target, kindOf(isDir))
		}
		if isDir {
			return removeRemoteTree(sess.client, target)
		}
		return sess.client.Remove(target)
	})
	if opErr != nil {
		return fsError("删除失败", opErr)
	}
	return apierr.Respond(w, map[string]any{"ok": true})
}

func kindOf(dir bool) string {
	if dir {
		return "目录"
	}
	return "文件"
}

// removeRemoteTree 递归删除一个目录（**不跟随符号链接**）。
//
// 刻意不用 pkg/sftp 的 RemoveAll：它的入口是 Stat（跟随软链接），删一个指向目录的软链接
// 会把链接**指向的整棵树**删掉——用户以为删的是一个链接，实际删掉的是别处的一整个目录。
func removeRemoteTree(c *sftp.Client, target string) error {
	fi, err := c.Lstat(target)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return c.Remove(target)
	}
	infos, err := c.ReadDir(target)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if err := removeRemoteTree(c, path.Join(target, info.Name())); err != nil {
			return err
		}
	}
	return c.RemoveDirectory(target)
}
