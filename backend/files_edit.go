package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"

	"webssh/backend/internal/logx"
)

// ---------------------------------------------------------------------------
// 编辑（契约第 26 节）：/api/fs/edit-load 与 /api/fs/edit-save
//
// 与预览（第 25 节）刻意分成两组接口，因为「看一眼」与「改一遍」的要求不一样：
//   - 编辑**必须**先确认登录用户能改这个文件（见 handleFsEditLoad 的写权限探测），
//     没权限就直接报 permission，界面不进编辑页；
//   - 保存要把文件**按原来的样子**写回去：UTF-16 还是 UTF-16、带 BOM 还是带 BOM、
//     CRLF 还是 CRLF。把 Windows 的 .ini / .reg 悄悄改成 UTF-8 + LF 是数据损坏，
//     不是「顺手升级」。
//
// 前端拿到的 content 一律是 LF（textarea 的 value 按 HTML 规范把 CRLF 规整成 LF），
// 所以 eol 由后端在保存时换回去；encoding / bom 原样回传，不做任何猜测。
// ---------------------------------------------------------------------------

// 换行风格（/api/fs/edit-load 的 eol 字段）。只区分这两种：孤立的 `\r`（老式 Mac）
// 在 NAS 上的配置文件里见不到，为它多一个分支只会让「保存后变成什么样」更难说清。
const (
	eolLF   = "lf"
	eolCRLF = "crlf"
)

// errEditNotRegular 是「目标不是普通文件」（目录、设备、FIFO、socket）。
//
// 单独拦一道不只是为了文案：以 O_WRONLY 打开一个 FIFO 会**阻塞**到有读者出现，
// 而 /dev/zero 这类设备节点被写回去更是纯粹的破坏。编辑只对普通文件开放。
var errEditNotRegular = errors.New("只能编辑普通文件")

// fsEditSaveReq 是 /api/fs/edit-save 的请求体。
//
// encoding / bom / eol 由 /api/fs/edit-load 回给前端、保存时**原样**带回来，服务端不猜：
// 猜错的后果是把用户那份 UTF-16 的配置文件写成 UTF-8（或反过来），而文件本身没有任何
// 可见的损坏迹象——直到某个只认 UTF-16 的程序读它。
type fsEditSaveReq struct {
	SID      string `json:"sid"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	BOM      bool   `json:"bom"`
	EOL      string `json:"eol"`
}

// handleFsEditLoad 打开编辑器：校验可写 + 读回内容与编码元信息。
//
// 写权限的判据是**以本会话的登录用户身份把文件打开一次**（O_WRONLY，不带 O_TRUNC /
// O_CREATE，打开完立刻关掉，不碰内容也不碰 mtime）。为什么不用 mode 位自己算：
// mode 位看不出 ACL、看不出 root、看不出组关系，而真正保存时走的就是这一条路——
// 让 SFTP 服务端回答「能不能写」是唯一不会骗人的判据。
func (m *AdminMux) handleFsEditLoad(w http.ResponseWriter, r *http.Request) *apierr.Error {
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
		size     int64
		content  []byte
	)
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		target, err := requiredRemotePath(sess, req.Path)
		if err != nil {
			return err
		}
		resolved = target
		fi, err := sess.client.Stat(target)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return errTargetIsDir
		}
		if !fi.Mode().IsRegular() {
			return errEditNotRegular
		}
		size = fi.Size()
		if size > maxReadBytes {
			// 与预览同一条上限（4 MiB）：一个几 MB 的文本塞进 <textarea> 里既卡又改不动，
			// 而「编辑到一半发现打不开」比一开始就说明原因更糟。
			return apierr.TooLarge("文件 %s 超过编辑上限 %s", humanSize(size), humanSize(maxReadBytes))
		}
		// 写权限探测。先探测再读内容：没权限的文件连内容都不该给出去（编辑页根本不会开）。
		wf, err := sess.client.OpenFile(target, os.O_WRONLY)
		if err != nil {
			return err
		}
		if err := wf.Close(); err != nil {
			return err
		}
		f, err := sess.client.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		content, err = io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if int64(len(content)) > maxReadBytes {
			// 读了之后才变大（正在被写入的文件）：按超限处理。
			return apierr.TooLarge("文件 %s 超过编辑上限 %s", humanSize(int64(len(content))), humanSize(maxReadBytes))
		}
		return err
	})
	if opErr != nil {
		return editError("打开编辑器失败", opErr)
	}
	text, encoding, ok := decodeText(content)
	if !ok {
		return apierr.Invalid("这个文件不是 UTF-8 / UTF-16 文本（可能是二进制文件或其它编码），不能编辑")
	}
	return apierr.Respond(w, map[string]any{
		"ok":       true,
		"path":     resolved,
		"size":     size,
		"content":  text,
		"encoding": encoding,
		"bom":      hasBOM(content),
		"eol":      detectEOL(text),
	})
}

// handleFsEditSave 把编辑区的内容写回远程文件。
//
// 写法是**原地截断再写**（O_WRONLY|O_TRUNC），不是「写临时文件再 rename」：rename 会换掉
// inode，把文件的属主、mode、ACL、xattr 全部丢掉——在 NAS 上 ACL 恰恰是最要紧的那一层
// （飞牛的权限模型就靠它），一个「改一行配置」的操作不该顺带重写这些。代价是写入过程中
// 失败会留下半截文件，所以下面先读一份原内容做善后（见 backup）。
func (m *AdminMux) handleFsEditSave(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsEditSaveReq
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	payload, encErr := encodeForSave(req.Content, req.Encoding, req.BOM, req.EOL)
	if encErr != nil {
		return apierr.Invalid("%v", encErr)
	}
	if int64(len(payload)) > maxReadBytes {
		return apierr.TooLarge("保存的内容 %s 超过上限 %s", humanSize(int64(len(payload))), humanSize(maxReadBytes))
	}
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
		if !fi.Mode().IsRegular() {
			return errEditNotRegular
		}
		// 善后用的原内容：写失败（连接断了 / 盘满）时尽量把文件恢复成原样。读不出来就算了
		// ——宁可报错，也不要拿一份不完整的备份去覆盖。
		var backup []byte
		if fi.Size() <= maxReadBytes {
			if bf, err := sess.client.Open(target); err == nil {
				backup, _ = io.ReadAll(io.LimitReader(bf, maxReadBytes+1))
				_ = bf.Close()
			}
		}
		f, err := sess.client.OpenFile(target, os.O_WRONLY|os.O_TRUNC)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(payload)
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			restoreEditedFile(sess.client, target, backup, err)
			return err
		}
		return nil
	})
	if opErr != nil {
		return editError("保存失败", opErr)
	}
	return apierr.Respond(w, map[string]any{"ok": true, "size": int64(len(payload))})
}

// restoreEditedFile 尽力把原内容写回去（best-effort）。
//
// 失败只记日志：调用方已经有一个更重要的错误要报给用户（保存失败），再叠一条「恢复也失败」
// 只会把第一句话挤掉。日志里两个都留着，便于事后判断文件到底成了什么样。
func restoreEditedFile(c *sftp.Client, target string, backup []byte, cause error) {
	if backup == nil {
		logx.Warnf("保存 %s 失败且没有可用的原内容备份：%v", target, cause)
		return
	}
	f, err := c.OpenFile(target, os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		logx.Warnf("保存 %s 失败，恢复原内容也失败：%v（原始错误：%v）", target, err, cause)
		return
	}
	if _, err := f.Write(backup); err != nil {
		_ = f.Close()
		logx.Warnf("保存 %s 失败，恢复原内容写回失败：%v（原始错误：%v）", target, err, cause)
		return
	}
	if err := f.Close(); err != nil {
		logx.Warnf("保存 %s 失败，恢复原内容关闭文件失败：%v（原始错误：%v）", target, err, cause)
		return
	}
	logx.Warnf("保存 %s 失败，已把原内容写回：%v", target, cause)
}

// hasBOM 报告文件是不是以 BOM 开头（UTF-8 或 UTF-16 LE/BE）。
//
// 单独判一次而不是让 decodeText 多回一个值：decodeText 是预览的判据（它只关心
// 「能不能当文本显示」），而 BOM 只在**写回**时才有意义——带 BOM 的 UTF-8 文件被写成
// 不带 BOM 的，多数程序看不出区别，但 Windows 上的记事本会（它靠 BOM 认编码）。
func hasBOM(content []byte) bool {
	return bytes.HasPrefix(content, utf8BOM) ||
		bytes.HasPrefix(content, utf16LEBOM) ||
		bytes.HasPrefix(content, utf16BEBOM)
}

// detectEOL 判断原文件用的是哪种换行。
//
// 多数票而不是「第一个换行」：混用的文件（编辑器保存过、或脚本拼接出来的）本来就说不清，
// 取出现次数多的那一种至少不会把整份文件都改掉。没有换行时回 lf——单行文件写回时用哪种
// 换行都无所谓。
func detectEOL(text string) string {
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	if crlf > lf {
		return eolCRLF
	}
	return eolLF
}

// encodeForSave 按原编码 / BOM / 换行把编辑区的内容编成字节。
//
// 换行先规整再替换：编辑区里可能同时有 LF 与 CRLF（粘贴进来的内容），不先归一化就会
// 写出 `\r\r\n` 这种东西——在配置文件里那是一个多余的回车，肉眼看不见。
func encodeForSave(text, encoding string, bom bool, eol string) ([]byte, error) {
	if eol == eolCRLF {
		text = strings.ReplaceAll(text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	switch encoding {
	case "", encodingUTF8:
		out := []byte(text)
		if bom {
			out = append(append([]byte(nil), utf8BOM...), out...)
		}
		return out, nil
	case encodingUTF16LE, encodingUTF16BE:
		bigEndian := encoding == encodingUTF16BE
		units := utf16.Encode([]rune(text))
		out := make([]byte, 0, len(units)*2+2)
		if bom {
			if bigEndian {
				out = append(out, utf16BEBOM...)
			} else {
				out = append(out, utf16LEBOM...)
			}
		}
		for _, unit := range units {
			if bigEndian {
				out = append(out, byte(unit>>8), byte(unit))
			} else {
				out = append(out, byte(unit), byte(unit>>8))
			}
		}
		return out, nil
	default:
		// 认不出的编码**不能**当成 UTF-8 写下去：那等于替用户做了「把文件转成 UTF-8」的决定。
		return nil, fmt.Errorf("不支持的编码：%q", encoding)
	}
}

// editError 把编辑接口的失败翻成错误响应。
//
// 在 fsError 之上补两条判据，都是这一轮**必须说清**的失败：
//   - `looksLikePermission`：SFTP 服务端把「只读文件系统」之类的拒绝回成 SSH_FX_FAILURE
//     （不是 PERMISSION_DENIED），pkg/sftp 于是不会把它归一成 os.ErrPermission——只剩
//     消息里的「Permission denied / Read-only file system」可认。用户看到的必须是
//     「拒绝修改」，而不是一句 internal。
//   - `errEditNotRegular`：目录 / 设备 / FIFO 不是「服务端故障」，是「这个东西不能编辑」。
//
// 真正的权限拒绝（SSH_FX_PERMISSION_DENIED）由 pkg/sftp 归一成 os.ErrPermission，
// fsError 已经把它翻成 permission，不必在这里重复一遍。
func editError(action string, err error) *apierr.Error {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	if errors.Is(err, errEditNotRegular) {
		return apierr.Invalid("%s：%s", action, err.Error())
	}
	if errors.Is(err, os.ErrPermission) || looksLikePermission(err.Error()) {
		return apierr.Permission("%s：%s", action, err.Error())
	}
	return fsError(action, err)
}
