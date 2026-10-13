package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"webssh/backend/internal/apierr"
)

// ---------------------------------------------------------------------------
// 批量复制 / 移动（契约 11.4 / 11.5）
//
// 这两个接口是文件管理器的「复制 / 剪切 / 粘贴」的落地：一次请求带多个源路径与一个目标
// 目录。三条贯穿的规则：
//  1. **绝不覆盖**：目标同名一律跳过并计数（前端的粘贴会提示「已存在同名项」）。SFTP 的
//     Rename 是 POSIX rename（直接盖掉），所以移动前必须自己查一次存在性。
//  2. 目录递归，但**不跟随符号链接**：复制软链接时按「重建一个同样的链接」处理，而不是
//     把它指向的内容也搬一份——那会把别人的数据悄悄拷进目标目录（甚至拷出工作区）。
//  3. 失败要收拾现场：半截的目标文件/目录当场删掉（否则用户会看到一份「看起来正常、
//     其实不完整」的副本，而这种副本最难被发现）。
// ---------------------------------------------------------------------------

// 复制 / 移动的两个「参数问题」哨兵错误（都翻成 invalid，见 fsError）：
//   - errNesting：把目录放进它自己内部（复制会无限递归、移动会把树挪进自己肚子里）；
//   - errRootOp：对根目录做整树操作（只会把目标盘写满，或者毁掉整个远程系统）。
var (
	errNesting = errors.New("不能把目录放进它自己内部")
	errRootOp  = errors.New("不能对根目录执行该操作")
)

// fsOpRequest 是 copy / move 的请求体。
type fsOpRequest struct {
	SID   string   `json:"sid"`
	Paths []string `json:"paths"`
	ToDir string   `json:"toDir"`
}

// fsOpResult 是 copy / move 的响应体（files 与上传接口同形：{path,size}）。
//
// files 里一项对应一个**源条目**（而不是每个子孙文件一项）：一个几万文件的目录递归展开
// 成 JSON 会让响应爆掉，而前端的粘贴提示只需要知道「搬了哪几项、跳过几项」。
type fsOpResult struct {
	Files []uploadedFile `json:"files"`
	// Skipped 是「因为目标同名（或就是自己）而跳过的条目数」。
	Skipped int `json:"skipped"`
}

// handleFsCopy 远端内部复制（SFTP 读 + 写）。
func (m *AdminMux) handleFsCopy(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsOpRequest
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var res fsOpResult
	budget := maxTransferBytes
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		toDir, err := requireRemoteDir(sess, req.ToDir)
		if err != nil {
			return err
		}
		sources, err := resolveSources(sess, req.Paths)
		if err != nil {
			return err
		}
		for _, src := range sources {
			if err := r.Context().Err(); err != nil {
				return err
			}
			if err := rejectSelfNesting(src, toDir, "复制"); err != nil {
				return err
			}
			dest := path.Join(toDir, path.Base(src))
			if dest == src {
				// 粘到自己所在的目录：没有可做的事。算「跳过」而不是报错——用户很可能
				// 只是把整个目录选中后粘贴回原处，报错反而莫名其妙。
				res.Skipped++
				continue
			}
			if _, err := sess.client.Lstat(dest); err == nil {
				res.Skipped++
				continue
			}
			size, err := copyRemoteEntry(r.Context(), sess.client, src, dest, &budget)
			if err != nil {
				// 现场清理：半截的副本比没有副本更糟（用户以为复制成功了）。
				_ = removeRemoteTree(sess.client, dest)
				return mapTransferError("复制失败", err)
			}
			res.Files = append(res.Files, uploadedFile{Path: dest, Size: size})
		}
		return nil
	})
	if opErr != nil {
		return mapTransferError("复制失败", opErr)
	}
	if res.Files == nil {
		res.Files = []uploadedFile{}
	}
	return apierr.Respond(w, map[string]any{"ok": true, "files": res.Files, "skipped": res.Skipped})
}

// handleFsMove 远端内部移动（优先 SFTP Rename）。
//
// 跨设备（源与目标在不同挂载卷）时 rename(2) 返回 EXDEV，而 SFTP 协议没有独立的错误码
// （sshd 的 sftp-server 只会回一个笼统的 SSH_FX_FAILURE），所以这里没法精确区分。**绝不**
// 因此静默降级成「复制 + 删除」：那会在中途失败时留下两份数据或一份都没有，而用户以为
// 移动成功了。失败就是把真实原因（尽可能全）报出来，让用户改用复制。
func (m *AdminMux) handleFsMove(w http.ResponseWriter, r *http.Request) *apierr.Error {
	var req fsOpRequest
	if apiErr := m.decodeJSON(w, r, &req); apiErr != nil {
		return apiErr
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}
	var res fsOpResult
	opErr := rt.withSFTP(r.Context(), func(sess *sftpSession) error {
		toDir, err := requireRemoteDir(sess, req.ToDir)
		if err != nil {
			return err
		}
		sources, err := resolveSources(sess, req.Paths)
		if err != nil {
			return err
		}
		for _, src := range sources {
			if err := r.Context().Err(); err != nil {
				return err
			}
			if err := rejectSelfNesting(src, toDir, "移动"); err != nil {
				return err
			}
			dest := path.Join(toDir, path.Base(src))
			if dest == src {
				res.Skipped++
				continue
			}
			if _, err := sess.client.Lstat(dest); err == nil {
				// SFTP 的 Rename 会**直接盖掉**同名目标，所以必须自己先查一次。
				res.Skipped++
				continue
			}
			if err := sess.client.Rename(src, dest); err != nil {
				return moveFailure(src, dest, err)
			}
			// 移动不改内容。尺寸取目标条目自身的大小（文件 = 文件字节数，目录 = 目录项大小）：
			// 刻意**不**递归统计整棵树——移动是一次 rename，为了回一个总字节数就把几万个文件
			// 逐个 stat 一遍不值得（那会让一次瞬间完成的移动变成几秒钟）。
			size := int64(0)
			if fi, err := sess.client.Lstat(dest); err == nil {
				size = fi.Size()
			}
			res.Files = append(res.Files, uploadedFile{Path: dest, Size: size})
		}
		return nil
	})
	if opErr != nil {
		return fsError("移动失败", opErr)
	}
	if res.Files == nil {
		res.Files = []uploadedFile{}
	}
	return apierr.Respond(w, map[string]any{"ok": true, "files": res.Files, "skipped": res.Skipped})
}

// requireRemoteDir 解析并校验「目标目录」。
func requireRemoteDir(sess *sftpSession, raw string) (string, error) {
	dir, err := requiredRemotePath(sess, raw)
	if err != nil {
		return "", err
	}
	fi, err := sess.client.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%w：目标不是目录（%s）", errTargetIsDir, dir)
	}
	return dir, nil
}

// resolveSources 解析并校验全部源路径：去重（同一项被选中两次只处理一次）、
// 拒绝根目录（复制整棵树没有意义，只会把目标盘写满）、确认存在。
func resolveSources(sess *sftpSession, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("缺少 paths")
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		p, err := requiredRemotePath(sess, item)
		if err != nil {
			return nil, err
		}
		if p == "/" {
			return nil, errRootOp
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := sess.client.Lstat(p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// rejectSelfNesting 挡住「把目录搬进它自己内部」：那会无限递归下去（复制）或直接把树
// 挪进自己肚子里（移动），最终以各种看不懂的失败告终。
func rejectSelfNesting(src, toDir, action string) error {
	if toDir == src || strings.HasPrefix(toDir, src+"/") {
		return fmt.Errorf("%w：不能把 %s %s 到它自己内部（%s）", errNesting, src, action, toDir)
	}
	return nil
}

// copyRemoteEntry 递归复制一个条目，返回写入的字节数。
//
// 软链接按「重建链接」处理（ReadLink + Symlink）：直接读它的内容会跟随链接把目标数据也
// 拷一份——目标可能在另一个挂载点甚至另一台机器的挂载目录上，用户并不想复制那些。
func copyRemoteEntry(ctx context.Context, c *sftp.Client, src, dest string, budget *int64) (int64, error) {
	fi, err := c.Lstat(src)
	if err != nil {
		return 0, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := c.ReadLink(src)
		if err != nil {
			return 0, err
		}
		return 0, c.Symlink(target, dest)
	}
	if !fi.IsDir() {
		return copyRemoteFile(ctx, c, src, dest, fi, budget)
	}

	if err := c.Mkdir(dest); err != nil {
		return 0, err
	}
	infos, err := c.ReadDir(src)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := copyRemoteEntry(ctx, c, path.Join(src, info.Name()), path.Join(dest, info.Name()), budget)
		total += n
		if err != nil {
			return total, err
		}
	}
	// 目录本身的权限位跟着源走（可执行位、组可写这类信息对用户是有意义的）。
	_ = c.Chmod(dest, fi.Mode().Perm())
	return total, nil
}

// copyRemoteFile 复制一个普通文件。
//
// 目标用 O_EXCL：存在性上面已经查过，但「查完到创建之间」这段时间里别人也可能建出同名
// 文件（同一个会话是串行的，可远端还有别的会话/别的用户）。O_EXCL 保证我们**永远**不会
// 覆盖别人的文件——真撞上就报 duplicate，而不是悄悄盖掉。
func copyRemoteFile(ctx context.Context, c *sftp.Client, src, dest string, fi os.FileInfo, budget *int64) (int64, error) {
	in, err := c.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := c.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, budgetReader{ctx: ctx, r: in, budget: budget})
	closeErr := out.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if closeErr != nil {
		return n, closeErr
	}
	_ = c.Chmod(dest, fi.Mode().Perm())
	return n, nil
}

// moveFailure 把 Rename 的失败翻成错误响应。
//
// 权限类失败直接报 permission（这是最常见、也最需要用户去改权限的一种）；其余一律报
// conflict + 一段把「跨设备」这个最可能的原因写清楚的文案——SFTP 层拿不到 errno，
// 硬猜一个更具体的码只会误导。
func moveFailure(src, dest string, err error) *apierr.Error {
	msg := err.Error()
	if errors.Is(err, os.ErrPermission) || looksLikePermission(msg) {
		return apierr.Permission("移动失败：%s → %s（%v）", src, dest, err)
	}
	return apierr.Conflict("移动失败：%s → %s（%v）。如果两者不在同一个文件系统（例如不同挂载卷），"+
		"SFTP 的 Rename 无法跨设备移动，请改用「复制」再删除源文件。", src, dest, err)
}
