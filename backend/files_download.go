package main

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

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
			if err := zipRemoteTree(sess.client, target, tmp); err != nil {
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
		return fsError("下载失败", opErr)
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

// zipRemoteTree 把一个远程目录递归打进 zip（相对路径保留目录结构）。
//
// 自己走 Lstat 而不是用 pkg/sftp 的 Walk：Walk 基于 kr/fs 的 WalkFS，用的是 Stat
// （跟随软链接），遇到「目录里的软链接指回上层」会无限递归。这里不跟随链接
// （软链接条目跳过并记一条日志——把别人的文件内容打进包更糟）。
func zipRemoteTree(c *sftp.Client, root string, w io.Writer) error {
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
			rel := strings.TrimPrefix(full, base+"/")
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
			_, copyErr := io.Copy(dst, src)
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

// handleFsRead 读取远程文本文件（文件管理器预览用）。
func (m *AdminMux) handleFsRead(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var (
		size     int64
		content  []byte
		tooLarge bool
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		fi, err := sess.client.Stat(target)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return errTargetIsDir
		}
		size = fi.Size()
		if size > maxReadBytes {
			tooLarge = true
			return nil
		}
		f, err := sess.client.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		content, err = io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if int64(len(content)) > maxReadBytes {
			// 读了之后才变大（写日志的文件正是这样）：按超限处理，别把内容塞进 JSON。
			tooLarge = true
			content = nil
		}
		return err
	})
	if opErr != nil {
		return fsError("读取文件失败", opErr)
	}
	text := !tooLarge && utf8.Valid(content) && !bytesContainZero(content)
	out := string(content)
	if !text {
		out = ""
	}
	return apierr.Respond(w, map[string]any{
		"ok":       true,
		"size":     size,
		"text":     text,
		"tooLarge": tooLarge,
		"content":  out,
	})
}

func bytesContainZero(b []byte) bool {
	limit := len(b)
	if limit > 8192 {
		limit = 8192
	}
	for i := 0; i < limit; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
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
