package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// SFTP 文件操作 API（契约第 4.2 节的 /api/fs/*）
//
// 本文件只管**下载**（文件流 / 目录打 zip）；两个「读内容给界面看」的接口
// （/api/fs/read 文本预览、/api/fs/image 图片预览）在 files_preview.go。
//
// 三条贯穿全部实现的约束：
//  1. 目标路径一律 path.Clean 并**拒绝 `..`**：SFTP 服务端也会拦，但我们要给出清晰的中文
//     错误，而不是让用户看到一句 "sftp: \"\""。
//  2. 每个会话一条 SFTP 通道、懒加载、失败重连一次，且所有文件操作串行化（见 withSFTP）。
//  3. 客户端取消（关页面 / 点取消）时 r.Context() 结束：正在进行的写入必须能中止，
//     写了一半的文件要删掉。
// ---------------------------------------------------------------------------

// handleFsDownload 下载：文件直接流式发正文，目录打成 zip。
//
// 目录**先打进临时文件再发正文**：响应头一旦发出就没法再回一个 JSON 错误，中途失败
// （某个文件读不了——权限位 0000 的文件正是这形态）会留下一个「结构完整、里面是 0 字节
// 条目」的 zip，用户拿到 200 却永远不知道少了东西。代价是下载期间多占一份临时空间。
func (m *AdminMux) handleFsDownload(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	var (
		isDir bool
		name  string
		size  int64
		tmp   *os.File
		file  *sftp.File
	)
	defer func() {
		if tmp != nil {
			tmpName := tmp.Name()
			tmp.Close()
			os.Remove(tmpName)
		}
		if file != nil {
			file.Close()
		}
	}()

	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		// 与其他「整棵树」的操作（copy / move / export-to-nas）口径一致：拒绝根目录。
		// 下载 `/` 会把整棵文件系统打进一个临时 zip（只受远端磁盘限制、用户也取消不掉），
		// 而且 base = path.Dir("/") = "/" 会让 zip 条目名带上前导 `/`。
		if target == "/" {
			return errRootOp
		}
		fi, err := sess.client.Stat(target)
		if err != nil {
			return err
		}
		base := path.Base(target)
		if base == "" || base == "/" || base == "." {
			base = "download"
		}
		name = base
		if fi.IsDir() {
			isDir = true
			name = base + ".zip"
			tmp, err = os.CreateTemp("", "webssh-download-*.zip")
			if err != nil {
				return fmt.Errorf("创建下载临时文件失败：%w", err)
			}
			budget := maxTransferBytes
			if err := zipRemoteTree(r.Context(), sess.client, target, tmp, &budget); err != nil {
				return fmt.Errorf("打包目录失败：%w", err)
			}
			if err := tmp.Sync(); err != nil {
				return fmt.Errorf("写入下载临时文件失败：%w", err)
			}
			size, err = tmp.Seek(0, io.SeekEnd)
			if err != nil {
				return err
			}
			_, err = tmp.Seek(0, io.SeekStart)
			return err
		}
		// 普通文件：流式发正文（Content-Length 已知，中途断开浏览器会发现文件不完整）。
		file, err = sess.client.Open(target)
		if err != nil {
			return err
		}
		size = fi.Size()
		return nil
	})
	if opErr != nil {
		// mapTransferError 而不是 fsError：打包超限要回 too_large（用户一眼看出是自己选的
		// 目录太大），否则会变成一句「内部错误」。
		return mapTransferError("下载失败", opErr)
	}

	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeHeader(name)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if isDir {
		w.Header().Set("Content-Type", "application/zip")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	src := io.Reader(tmp)
	if !isDir {
		src = file
	}
	if _, err := io.Copy(w, src); err != nil {
		// 正文已经发出去了，再回 JSON 也没用（只会多一条 superfluous WriteHeader）。
		logx.Warnf("下载 %s 中断：%v", req.Path, err)
	}
	return nil
}

// zipEntryName 把绝对路径翻成 zip 里的条目名（相对于 root 的**父目录**，所以包里带一层
// 顶层目录名，例如 /home/user/docs/a.txt → docs/a.txt）。
//
// 先摘 base 再摘前导 `/`：base 为 `/`（root 是顶层目录，例如 /home、/tmp）时 `base+"/"` 是
// `//`，一次 TrimPrefix 都匹配不上，条目名会变成 `/home/...`（带前导斜杠的绝对路径，
// Windows 解压会得到奇怪的盘符路径）。见 TestZipEntryName。
func zipEntryName(full, base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(full, base), "/")
}

// zipRemoteTree 把一个远程目录递归打进 zip（相对路径保留目录结构）。
//
// 自己走 Lstat 而不是用 pkg/sftp 的 Walk：Walk 基于 kr/fs 的 WalkFS，用的是 Stat
// （跟随软链接），遇到「目录里的软链接指回上层」会无限递归。这里不跟随链接
// （软链接条目跳过并记一条日志——把别人的文件内容打进包更糟）。
//
// budget 与另外三条「服务端自己搬数据」的路径同口径（见 maxTransferBytes）：下载一个
// 几百 GB 的目录不能一直占着这条会话的 SFTP 通道把 NAS 临时盘写满。超限返回
// errTransferBudget，由调用方翻成 too_large。
func zipRemoteTree(ctx context.Context, c *sftp.Client, root string, w io.Writer, budget *int64) error {
	zw := zip.NewWriter(w)
	base := path.Dir(root)
	var walk func(dir string) error
	walk = func(dir string) error {
		infos, err := c.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, info := range infos {
			full := path.Join(dir, info.Name())
			rel := zipEntryName(full, base)
			if info.IsDir() {
				if _, err := zw.Create(rel + "/"); err != nil {
					return err
				}
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				logx.Warnf("打包下载：跳过符号链接 %s", full)
				continue
			}
			src, err := c.Open(full)
			if err != nil {
				return fmt.Errorf("%s：%w", full, err)
			}
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				src.Close()
				return err
			}
			hdr.Name = rel
			hdr.Method = zip.Deflate
			dst, err := zw.CreateHeader(hdr)
			if err != nil {
				src.Close()
				return err
			}
			_, copyErr := io.Copy(dst, budgetReader{ctx: ctx, r: src, budget: budget})
			src.Close()
			if copyErr != nil {
				return fmt.Errorf("%s：%w", full, copyErr)
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// sanitizeHeader 去掉 Content-Disposition 文件名里会破坏报文头的字符。
func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, s)
}
