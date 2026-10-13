package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
)

// ---------------------------------------------------------------------------
// 导出至飞牛（契约 11.4 / 11.5）
//
// 方向是「远程主机 → 本机（NAS）共享目录」：经 SFTP 把远程的文件/目录拉回来。
// 目标根目录由环境变量 TRIM_DATA_SHARE_PATHS 的第一个冒号之前那段决定（见 config.go），
// 为空时这个功能整体不可用（no_share_path），前端也会把菜单项禁掉。
//
// 三条要求（契约 11.5 最后一条）：总字节上限、ctx 取消、失败清掉半成品。前两条靠
// budgetReader + withSFTP（客户端断开时它会关掉整条 SFTP 连接，阻塞中的读立刻失败）；
// 第三条靠调用方在错误路径上 os.RemoveAll。
// ---------------------------------------------------------------------------

// handleFsExportToNas 把远程一个条目复制到本机共享目录。
func (m *AdminMux) handleFsExportToNas(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		SID       string `json:"sid"`
		Path      string `json:"path"`
		Overwrite bool   `json:"overwrite"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	root := strings.TrimSpace(m.renv.ExportRoot)
	if root == "" {
		return errNoSharePath("没有配置飞牛共享目录（TRIM_DATA_SHARE_PATHS 为空），无法导出到本机")
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	var (
		local  string
		size   int64
		files  int
		budget = maxTransferBytes
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		remote, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if remote == "/" {
			return errRootOp
		}
		if _, err := sess.client.Lstat(remote); err != nil {
			return err
		}
		name := path.Base(remote)
		if name == "" || name == "/" || name == "." || name == ".." {
			return fmt.Errorf("%w：%q 不能作为本机文件名", errInvalidLocalName, name)
		}
		dest, err := localExportPath(root, name)
		if err != nil {
			return err
		}
		local = dest

		// 同名冲突：默认不覆盖（契约 11.4：前端拿到 duplicate 后再问用户是否覆盖）。
		if _, err := os.Lstat(dest); err == nil {
			if !req.Overwrite {
				return errDuplicate("本机共享目录里已存在同名项：%s（可选择覆盖）", dest)
			}
			// 覆盖 = 先整份删掉再写。不清掉的话，一个目录导出到已存在的同名目录里会变成
			// 「新旧内容混在一起」——用户以为拿到的是远程那份，实际多了别的文件。
			if err := os.RemoveAll(dest); err != nil {
				return fmt.Errorf("清理已存在的 %s 失败：%w", dest, err)
			}
		}
		n, count, err := exportRemoteEntry(r.Context(), sess.client, remote, dest, &budget)
		if err != nil {
			// 失败清掉半成品：一个写了一半的文件/目录留在共享目录里，用户很可能直接把它
			// 当成导出的结果拿走（尤其是目录，少的那几个文件根本看不出来）。
			if rmErr := os.RemoveAll(dest); rmErr != nil {
				warnf("清理导出半成品 %s 失败：%v", dest, rmErr)
			}
			return err
		}
		size, files = n, count
		return nil
	})
	if opErr != nil {
		return mapTransferError("导出至飞牛失败", opErr)
	}
	if files == 0 && size == 0 {
		// 源是个只有软链接的目录之类：什么都没导出。不报错（这不是故障），但要让日志
		// 留一条——用户看到「导出成功 0 个文件」时可以据此排查。
		warnf("导出至飞牛：%s 里没有可导出的普通文件", req.Path)
	}
	return respond(w, map[string]any{
		"ok":     true,
		"export": map[string]any{"path": local, "size": size, "files": files},
	})
}

// errInvalidLocalName 是「远程名字不能当本机文件名用」的哨兵错误（翻成 invalid）。
var errInvalidLocalName = errors.New("文件名不合法")

// localExportPath 拼出本机的目标路径，并确认它确实落在共享目录**里面**。
//
// 多这一道校验是因为：远程条目的名字来自远端（可能带 `..`、可能带奇怪字符），而我们要用
// 它在**本机**建文件。名字本身已经过 path.Base + 显式拒绝 "."/".."，这里再做一次「结果路径
// 是否还在 root 之下」的兜底——本机文件系统的写入权限比远端大得多，值得多查一遍。
func localExportPath(root, name string) (string, error) {
	root = filepath.Clean(root)
	dest := filepath.Join(root, name)
	if dest == root || !strings.HasPrefix(dest, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w：%q 解析出的本机路径 %s 不在共享目录 %s 内", errInvalidLocalName, name, dest, root)
	}
	return dest, nil
}

// exportRemoteEntry 递归把一个远程条目拉回本机，返回（字节数，文件数）。
//
// 软链接**跳过**：它在远端指向的是**远端**的路径，落到本机后是个指向不存在位置（甚至指向
// 本机另一个真实文件）的死链接。悄悄导出会得到一个语义完全变了的链接，比不导出更糟。
func exportRemoteEntry(ctx context.Context, c *sftp.Client, remote, local string, budget *int64) (int64, int, error) {
	fi, err := c.Lstat(remote)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		warnf("导出至飞牛：跳过软链接 %s", remote)
		return 0, 0, nil

	case fi.IsDir():
		// 本机目录用固定模式位（0o755）而不是照搬远端的：共享目录上的读写权限由飞牛的 ACL
		// 管，把远端某个用户/组的特殊权限位搬过来只会出现「自己都进不去」的目录。
		if err := os.MkdirAll(local, 0o755); err != nil {
			return 0, 0, fmt.Errorf("创建本机目录 %s 失败：%w", local, err)
		}
		infos, err := c.ReadDir(remote)
		if err != nil {
			return 0, 0, err
		}
		var (
			size  int64
			files int
		)
		for _, info := range infos {
			// 每进一个条目查一次 ctx：一个几十万文件的目录要能在用户关页面时立刻停下
			// （withSFTP 也会在 ctx 结束时关掉整条连接，把阻塞中的读一并打断）。
			if err := ctx.Err(); err != nil {
				return size, files, err
			}
			n, f, err := exportRemoteEntry(ctx, c, path.Join(remote, info.Name()), filepath.Join(local, info.Name()), budget)
			size += n
			files += f
			if err != nil {
				return size, files, err
			}
		}
		return size, files, nil

	default:
		return exportRemoteFile(ctx, c, remote, local, budget)
	}
}

// exportRemoteFile 拉回一个普通文件。
func exportRemoteFile(ctx context.Context, c *sftp.Client, remote, local string, budget *int64) (int64, int, error) {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return 0, 0, fmt.Errorf("创建本机目录失败：%w", err)
	}
	src, err := c.Open(remote)
	if err != nil {
		return 0, 0, err
	}
	defer src.Close()
	// O_EXCL：上面已经处理过「已存在」的两种情况（跳过 / 先删），这里再挡一次——万一在
	// 这期间有人（另一个用户、另一个实例）放了同名文件，宁可报 duplicate 也不覆盖。
	dst, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, 0, err
	}
	n, copyErr := io.Copy(dst, budgetReader{ctx: ctx, r: src, budget: budget})
	closeErr := dst.Close()
	if copyErr != nil {
		return n, 0, copyErr
	}
	if closeErr != nil {
		return n, 0, closeErr
	}
	// 显式 chmod：飞牛存储卷上 O_CREAT 给的模式位可能被忽略（会落成 0000，连自己都读不回来）。
	if err := os.Chmod(local, 0o644); err != nil {
		warnf("设置 %s 权限失败：%v", local, err)
	}
	return n, 1, nil
}
