package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
	"webssh/backend/internal/store"
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

// handleFsUpload 处理本地文件 / 目录上传（multipart：sid、path、overwrite、files[]）。
func (m *AdminMux) handleFsUpload(w http.ResponseWriter, r *http.Request) *apierr.Error {
	// MaxBytesReader 卡整条请求（见 maxUploadBytes 的注释）；它同时让客户端能在超限时
	// 收到明确响应，而不是连接被直接掐断。
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		if tooLargeBody(err) {
			return apierr.TooLarge("上传内容超过上限 %s", humanSize(maxUploadBytes))
		}
		return apierr.Invalid("解析上传内容失败：%v", err)
	}
	sid := r.FormValue("sid")
	target := r.FormValue("path")
	overwrite := r.FormValue("overwrite") == "true"

	rt, apiErr := m.sessions.runtimeFor(sid)
	if apiErr != nil {
		return apiErr
	}
	// 字段名是 `files`（frontend/src/serverapi/index.ts 的 uploadLocal 就是这么 append 的）。
	// 契约第 4.2 节的表格里写的是 `files[]`，两种都收：以后有人照着文档写客户端时不会
	// 拿到一句「没有收到文件」而不知道为什么。
	parts := r.MultipartForm.File["files"]
	if len(parts) == 0 {
		parts = r.MultipartForm.File["files[]"]
	}
	if len(parts) == 0 {
		return apierr.Invalid("没有收到文件（字段名应为 files）")
	}

	var (
		written []uploadedFile
		skipped int
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		dir, err := resolveRemotePath(sess, target)
		if err != nil {
			return err
		}
		if fi, err := sess.client.Stat(dir); err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("目标不是目录：%s", dir)
		}
		for _, part := range parts {
			if err := r.Context().Err(); err != nil {
				return apierr.Aborted("上传已取消")
			}
			name, nerr := rawUploadName(part.Header)
			if nerr != nil {
				return nerr
			}
			segs, verr := validateUploadName(name)
			if verr != nil {
				return verr
			}
			dest := path.Join(append([]string{dir}, segs...)...)
			src, oerr := part.Open()
			if oerr != nil {
				return fmt.Errorf("读取上传内容失败：%w", oerr)
			}
			// 半成品的清理在 writeRemote 里做（它才知道那是临时文件还是本次新建的文件）：
			// 这里绝不能再 Remove(dest)——overwrite 失败时 dest 是**用户原有的文件**。
			size, skip, werr := writeRemote(r.Context(), sess.client, dest, src, overwrite)
			src.Close()
			if werr != nil {
				return werr
			}
			if skip {
				skipped++
				continue
			}
			written = append(written, uploadedFile{Path: dest, Size: size})
		}
		return nil
	})
	if opErr != nil {
		return fsError("上传失败", opErr)
	}
	if written == nil {
		written = []uploadedFile{}
	}
	return apierr.Respond(w, map[string]any{"ok": true, "files": written, "skipped": skipped})
}

// rawUploadName 取上传项的原始 filename。
//
// **不能用 part.FileName()**：Go 的 mime/multipart 按 RFC 7578 第 4.2 节主动把目录信息丢掉
// （`filepath.Base(filename)`），而前端的目录上传正是靠 filename 携带相对路径
// （`form.append('files', file, 'a/b.txt')`，见 serverapi 的 uploadLocal）。用它等于把整棵
// 目录拍平成一层——用户的「上传目录」会变成一个目录里十几个散文件，还可能互相覆盖。
// 所以这里自己从 Content-Disposition 里读原始值。
//
// 另外按 HTML 规范，浏览器会把 filename 里的 `"`、CR、LF 百分号转义（%22/%0D/%0A）
// 再放进表单；这里把这三个还原，否则「名字里带引号的文件」上传后会多出一串 %22。
func rawUploadName(h textproto.MIMEHeader) (string, error) {
	disp := h.Get("Content-Disposition")
	if strings.TrimSpace(disp) == "" {
		return "", errors.New("上传项缺少 Content-Disposition")
	}
	_, params, err := mime.ParseMediaType(disp)
	if err != nil {
		return "", fmt.Errorf("解析上传项的 Content-Disposition 失败：%w", err)
	}
	name := params["filename"]
	if name == "" {
		return "", errors.New("上传项没有文件名")
	}
	return decodeFormFilenameEscapes(name), nil
}

// decodeFormFilenameEscapes 还原 HTML 规范里对 filename 的百分号转义（只解这三个）。
//
// 大小写两种形态都要解：规范说浏览器转义的是大写十六进制，但别的实现/中间层可能给小写，
// 解错的表现是「文件名里多出一串 %22」。
func decodeFormFilenameEscapes(name string) string {
	replacer := strings.NewReplacer(
		"%22", `"`, "%0D", "\r", "%0A", "\n",
		"%0d", "\r", "%0a", "\n")
	return replacer.Replace(name)
}

// writeRemote 边收边写一个上传项（不整份读进内存：放几百 MB 的二进制是常见用法）。
//
// 两条路径都不碰「用户已有的文件」：
//   - overwrite=false：用 O_EXCL 打开，已存在就整份跳过并计数。**不能**「先 Lstat 再打开」：
//     OpenFile 本身带 O_TRUNC，中间那个窗口足以把别人的文件清成 0 字节。
//     O_EXCL 失败时再 Lstat 一次判「是不是因为已存在」（不依赖服务端把 EEXIST 映射成
//     哪个状态码——不同 SFTP 实现的映射并不一致）。
//   - overwrite=true：先写**同目录**下的临时文件，成功后再 rename 覆盖目标。
//     直接在目标上 O_TRUNC 会让「写到一半失败」等于把用户原来的文件毁掉（旧实现还在
//     失败路径上 Remove(dest)，于是「一个字节没写成功、原文件也没了」）。同目录的 rename
//     是同一文件系统内的原子替换；失败只留一个临时文件，由这里删掉，目标一个字节都不动。
//
// 失败时留下的临时文件（进程被杀这类情况）用 `.webssh-upload-*.part` 命名，可一眼认出。
func writeRemote(ctx context.Context, c *sftp.Client, dest string, src io.Reader, overwrite bool) (int64, bool, error) {
	if err := c.MkdirAll(path.Dir(dest)); err != nil {
		return 0, false, err
	}
	if !overwrite {
		f, err := c.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			if _, lerr := c.Lstat(dest); lerr == nil {
				return 0, true, nil
			}
			return 0, false, err
		}
		n, err := io.Copy(f, ctxReader{ctx: ctx, r: src})
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			// 这个文件是本次 O_EXCL 创建出来的，写坏了就删掉，不留残骸。
			_ = c.Remove(dest)
			return n, false, err
		}
		return n, false, nil
	}

	tmp := path.Join(path.Dir(dest), ".webssh-upload-"+store.NewID()+".part")
	f, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, false, err
	}
	n, err := io.Copy(f, ctxReader{ctx: ctx, r: src})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceRemoteFile(c, tmp, dest)
	}
	if err != nil {
		_ = c.Remove(tmp)
		return n, false, err
	}
	return n, false, nil
}

// replaceRemoteFile 把临时文件换成目标（覆盖已有文件）。
//
// 先试标准 SSH_FXP_RENAME，失败再试 posix-rename@openssh.com 扩展：SFTP v3 对「目标已存在」
// 的语义不确定（draft-ietf-secsh-filexfer-02 §6.5.3 说「已存在即错误」），OpenSSH 专门加了
// posix-rename 扩展来表达「像 rename(2) 一样覆盖」。两个都失败才报错——**不要**退回
// 「先删目标再改名」：那个窗口里进程被杀，用户的文件就真没了（正是这次要修的问题）。
func replaceRemoteFile(c *sftp.Client, tmp, dest string) error {
	firstErr := c.Rename(tmp, dest)
	if firstErr == nil {
		return nil
	}
	if err := c.PosixRename(tmp, dest); err == nil {
		return nil
	}
	return firstErr
}

// ctxReader 让「客户端取消」能中断一次正在进行的写入（见 withSFTP 的说明：
// 只靠「文件之间查一次 ctx」不够，一个几百 MB 的文件会一直写到磁盘上）。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// budgetReader 同时负责「客户端取消」与「总量上限」两件事（见 maxTransferBytes）。
//
// 挂到 io.Copy 的读端而不是写完再统计：这样超限当场停在原地，不会先把几个 GB 搬到目标
// 再回头说「太大了」，半成品也就只有那一小块。
type budgetReader struct {
	ctx    context.Context
	r      io.Reader
	budget *int64
}

func (b budgetReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if *b.budget <= 0 {
		return 0, errTransferBudget
	}
	if int64(len(p)) > *b.budget {
		p = p[:*b.budget]
	}
	n, err := b.r.Read(p)
	*b.budget -= int64(n)
	return n, err
}

// mapTransferError 把搬运过程中的错误翻成错误码（too_large 要单独认出来，否则会变成
// 一句「内部错误」，用户完全看不出是自己选的目录太大）。
func mapTransferError(action string, err error) *apierr.Error {
	if errors.Is(err, errTransferBudget) {
		return apierr.TooLarge("%s：内容超过单次上限 %s", action, humanSize(maxTransferBytes))
	}
	if errors.Is(err, errInvalidLocalName) {
		return apierr.Invalid("%s：文件名不合法（%v）", action, err)
	}
	return fsError(action, err)
}

// handleFsUploadFromNas 把飞牛（本机）上已授权的文件推送到远程主机。
//
// 读之前尽量用 trim.file.checkUserACL 判一次读权限：pickUserFile 只保证「应用用户」
// 拿到了 ACL，不代表当前登录用户也能读这个文件。飞牛不可用时跳过并记日志（这台机器上
// 没有网关，也就没有那层授权），不能因此把功能整个禁掉。
func (m *AdminMux) handleFsUploadFromNas(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req struct {
		SID       string `json:"sid"`
		Path      string `json:"path"`
		Source    string `json:"source"`
		Overwrite bool   `json:"overwrite"`
	}
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return apierr.Invalid("缺少 source")
	}
	if !strings.HasPrefix(source, "/") {
		return apierr.Invalid("source 必须是本机绝对路径：%s", source)
	}
	if err := validateRemotePath(source); err != nil {
		return apierr.Invalid("source 不合法：%v", err)
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	// ACL 判定（尽力而为）。
	if m.trim.Available(r.Context()) {
		u := gatewayUserOf(r)
		if acls, err := m.trim.CheckUserACL(r.Context(), u.UID, []string{source}); err != nil {
			logx.Warnf("检查本机文件 %s 的用户权限失败，跳过校验：%v", source, err)
		} else if len(acls) > 0 && !acls[0].Readable {
			return apierr.Permission("当前用户对 %s 没有读权限", source)
		}
	}

	fi, err := os.Stat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return apierr.NotFound("本机文件不存在：%s", source)
		}
		return apierr.Internal("读取本机文件失败：%v", err)
	}
	if fi.IsDir() {
		// 飞牛文件选择器给的是**文件**（upload_nas_hint 也写明「暂不支持选择目录」）。
		// 真要支持整目录，得连同相对路径逐项推送，属于另一个功能。
		return apierr.Invalid("暂不支持上传目录（%s 是目录）：请改用「本地目录」上传", source)
	}
	if !fi.Mode().IsRegular() {
		// FIFO 没有写者时 open(2) 会永久阻塞，设备节点也不该这么读。
		return apierr.Invalid("只支持普通文件：%s", source)
	}
	if fi.Size() > maxNasFileBytes {
		return apierr.TooLarge("文件超过上限 %s：%s", humanSize(maxNasFileBytes), humanSize(fi.Size()))
	}

	name := path.Base(source)
	segs, verr := validateUploadName(name)
	if verr != nil {
		return apierr.Invalid("文件名不合法：%v", verr)
	}

	var (
		written []uploadedFile
		skipped int
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		dir, err := resolveRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		if fi, err := sess.client.Stat(dir); err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("目标不是目录：%s", dir)
		}
		dest := path.Join(append([]string{dir}, segs...)...)
		f, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("打开本机文件失败：%w", err)
		}
		defer f.Close()
		size, skip, werr := writeRemote(r.Context(), sess.client, dest, f, req.Overwrite)
		if werr != nil {
			return werr
		}
		if skip {
			skipped = 1
			return nil
		}
		written = append(written, uploadedFile{Path: dest, Size: size})
		return nil
	})
	if opErr != nil {
		return fsError("从飞牛上传失败", opErr)
	}
	if written == nil {
		written = []uploadedFile{}
	}
	return apierr.Respond(w, map[string]any{"ok": true, "files": written, "skipped": skipped})
}
