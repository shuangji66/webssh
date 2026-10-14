package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
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

// maxUploadBytes 是整个上传请求体的上限（契约第 2 节）。
//
// 必须用 http.MaxBytesReader 卡住**整个请求体**：ParseMultipartForm 的参数只是
// 「内存阈值」，超出部分会落到进程临时目录并继续接收，总量不设限时一直传下去就能把盘灌满。
//
// var 而不是 const：测试要把它临时调小来验证上限逻辑（真造一个 1 GiB 的请求体太慢）。
var maxUploadBytes int64 = 1 << 30

const (
	// uploadMemoryLimit 是 multipart 在内存里的阈值（超出部分落临时文件，由 MaxBytesReader 兜底）。
	uploadMemoryLimit = 8 << 20
	// maxReadBytes 是 /api/fs/read 的读取上限：超过就只回元信息 + tooLarge，
	// 让界面提示「去下载」而不是把一个几百 MB 的二进制塞进 JSON。
	maxReadBytes = 4 << 20
	// maxImageBytes 是 /api/fs/image（图片预览）的体积上限：超过就回 too_large，界面
	// **不显示**预览（用户要求：图片最大 4 MB）。先 Stat 再读，超限的图片一个字节都不会
	// 真的传过来；单张 4 MiB 常驻内存与 maxReadBytes 同量级，可接受。
	//
	// 与 maxReadBytes 同值但**含义不同**（一个是文本预览、一个是图片预览），所以分成两个
	// 常量：以后要单独调其中一个，不必去猜另一个是不是「同一个东西」。
	maxImageBytes = 4 << 20
	// maxNasFileBytes 是 upload-from-nas 单个文件的体积上限。
	//
	// 这条路径没有请求体可卡（source 是本机路径，服务端自己读），所以单独设一个上限：
	// 一次「上传一个 50GB 的镜像」会占满临时缓冲与远程连接，用户还看不到进度。
	maxNasFileBytes = 1 << 30
)

// maxTransferBytes 是一次「服务端自己搬运数据」的操作（远端复制、导出至飞牛）的总字节上限。
//
// 这类接口同样没有请求体可卡（数据在远端/本机之间流动），没有上限时一次「复制一个 200GB
// 的目录」会把这条会话的 SFTP 通道占住几十分钟：用户看不到任何进度（HTTP 请求上没有进度
// 语义），也取消不掉（前端只能关页面，而那会连带丢掉整个界面状态）。超限即中止，并且把
// 写了一半的东西清掉。
//
// var 而不是 const：测试要把它临时调小来验证上限逻辑（真搬 4 GiB 太慢）。
var maxTransferBytes int64 = 4 << 30

// errTransferBudget 是「搬运总量超过 maxTransferBytes」的哨兵错误，由 budgetReader 返回。
// 单独定义而不是复用 errTooLarge：它要能被 errors.Is 认出来，才能翻成 too_large 的 413。
var errTransferBudget = errors.New("传输内容超过大小上限")

// 下面几个哨兵错误都是「参数问题」（400），不是服务端故障：
//   - errTargetIsDir：界面上点错了入口（比如对着目录点「预览」）；
//   - errKindMismatch：目录删除的 dir 参数与真实类型不一致（此时报错比删掉更安全）；
//   - errNotRegularFile：目标是设备节点 / 管道 / socket，不是普通文件。
var (
	errTargetIsDir  = errors.New("目标是目录")
	errKindMismatch = errors.New("dir 参数与真实类型不一致")
	errRefuseRoot   = errors.New("拒绝删除根目录")
	// errNotRegularFile 是「目标存在、但不是普通文件」。
	//
	// 必须在 Open 之前拦住，而不是打开之后再说：**读**打开一个 FIFO 会阻塞到有写者出现，
	// **写**打开会阻塞到有读者出现（POSIX 语义），而 withSFTP 整段持有 sftpMu——
	// 一次「对 FIFO 点预览」就能把这条会话的全部文件操作卡住，直到客户端断开
	// （前端 fetch 没有超时，用户看到的是文件管理整个卡死）。io.LimitReader 只作用于
	// Read 阶段，救不了 Open。
	errNotRegularFile = errors.New("不是普通文件（设备节点 / 管道 / socket 不能这样打开）")
)

// sftpFxFileAlreadyExists 是 SFTP 协议的 SSH_FX_FILE_ALREADY_EXISTS。
//
// pkg/sftp 没有导出这个码（只导出了 Ok/EOF/NoSuchFile/PermissionDenied/Failure/
// BadMessage/NoConnection/ConnectionLost/OPUnsupported），这里按 draft-ietf-secsh-filexfer-13
// 第 9.1 节写死 11。
const sftpFxFileAlreadyExists = 11

// fsEntry 是目录里的一项（契约第 4.3 节 FsEntry；path 是绝对路径）。
type fsEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	MTime   string `json:"mtime"`
	Symlink string `json:"symlink,omitempty"`
	Broken  bool   `json:"broken,omitempty"`
}

// uploadedFile 是上传结果里的一项。
type uploadedFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ---------------------------------------------------------------------------
// SFTP 通道
// ---------------------------------------------------------------------------

// sftpSession 是一次 SFTP 子系统会话（挂在会话连接的一个 channel 上）。
//
// home 缓存的是登录家目录：它由登录用户决定，一次登录内不会变，而每次 fs/list 都解析
// 一遍要多一个来回。缓存与连接同生命周期（连接重建 = 新会话 = 重新解析）。
type sftpSession struct {
	client *sftp.Client
	sub    *ssh.Session
	home   string
	homeOK bool
}

func (s *sftpSession) homeDir() (string, error) {
	if s.homeOK {
		return s.home, nil
	}
	p, err := s.client.RealPath(".")
	if err != nil {
		return "", err
	}
	s.home, s.homeOK = p, true
	return p, nil
}

// openSFTPLocked 建立 SFTP 通道（调用方必须持有 sftpMu）。
// base 连接没了就先重拨——对端空闲超时踢掉连接是很常见的。
func (r *sessionRuntime) openSFTPLocked(ctx context.Context) error {
	if r.base == nil {
		cfg, ok := r.config()
		if !ok {
			return errSessionGone("会话不存在：%s", r.id)
		}
		req, apiErr := r.mgr.dialRequestOf(cfg)
		if apiErr != nil {
			return apiErr
		}
		res, cerr := r.mgr.dial(ctx, req)
		if cerr != nil {
			return errFromConnect(cerr)
		}
		r.base = res.client
	}
	sub, err := r.base.NewSession()
	if err != nil {
		// 连接已经不可用了：连 SSH 会话通道都开不出来 → 丢掉它，下次重拨。
		_ = r.base.Close()
		r.base = nil
		return fmt.Errorf("打开 SSH 会话通道失败：%w", err)
	}
	if err := sub.RequestSubsystem("sftp"); err != nil {
		_ = sub.Close()
		_ = r.base.Close()
		r.base = nil
		return apierr.SFTPUnavailable("远端没有可用的 SFTP 子系统：%v", err)
	}
	stdout, err := sub.StdoutPipe()
	if err != nil {
		_ = sub.Close()
		return fmt.Errorf("获取 SFTP stdout 失败：%w", err)
	}
	stdin, err := sub.StdinPipe()
	if err != nil {
		_ = sub.Close()
		return fmt.Errorf("获取 SFTP stdin 失败：%w", err)
	}
	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = sub.Close()
		return fmt.Errorf("初始化 SFTP 客户端失败：%w", err)
	}
	r.sftp = &sftpSession{client: client, sub: sub}
	return nil
}

// dropSFTPLocked 关掉 SFTP 通道与会话连接（调用方必须持有 sftpMu）。
func (r *sessionRuntime) dropSFTPLocked() {
	if r.sftp != nil {
		_ = r.sftp.client.Close()
		_ = r.sftp.sub.Close()
		r.sftp = nil
	}
	if r.base != nil {
		_ = r.base.Close()
		r.base = nil
	}
}

// sftpReady 报告 SFTP 通道当前是否可用（SessionState.sftpOpen）。
func (r *sessionRuntime) sftpReady() bool {
	r.sftpMu.Lock()
	defer r.sftpMu.Unlock()
	return r.sftp != nil
}

// withSFTP 串行化执行一次文件操作，并处理「连接坏了重连一次」与「客户端取消」。
//
// 取消的处理方式值得说明：文件操作本身不接受 context（pkg/sftp 的 API 没有），所以
// 这里把操作放到协程里跑，ctx 结束时**关掉整条 SFTP 连接**——阻塞中的请求会因连接断开
// 立刻失败返回，协程不会泄漏；代价是这条连接不能再用（下次操作会重拨），换来的是
// 「关页面之后服务端不会一直挂在一个没人要的请求上」。
func (r *sessionRuntime) withSFTP(ctx context.Context, fn func(*sftpSession) error) error {
	r.sftpMu.Lock()
	defer r.sftpMu.Unlock()

	if err := ctx.Err(); err != nil {
		return apierr.Aborted("请求已取消")
	}
	sess, err := r.sftpSessionLocked(ctx)
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- fn(sess) }()
	select {
	case err := <-done:
		if err != nil && isSFTPTransportError(err) {
			// 传输层错误：连接多半已经废了，丢掉它，下次操作重拨。
			r.dropSFTPLocked()
		}
		return err
	case <-ctx.Done():
		r.dropSFTPLocked()
		return apierr.Aborted("请求已取消")
	}
}

// sftpSessionLocked 拿到可用的 SFTP 会话：没有就建，建失败按契约重连一次。
//
// 只对「连接层失败」重试：拨号/认证失败（*apierr.Error 的连接类错误）重试一次不会有不同结果，
// 白白让用户多等一轮超时。
func (r *sessionRuntime) sftpSessionLocked(ctx context.Context) (*sftpSession, error) {
	if r.sftp != nil {
		return r.sftp, nil
	}
	err := r.openSFTPLocked(ctx)
	if err == nil {
		return r.sftp, nil
	}
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return nil, err
	}
	// 第一次失败（多半是上一条连接被对端踢掉、或者 channel 数满了）：丢掉重拨一次。
	r.dropSFTPLocked()
	if err2 := r.openSFTPLocked(ctx); err2 != nil {
		logx.Warnf("会话 %s 重建 SFTP 通道失败：%v", r.id, err2)
		return nil, err2
	}
	return r.sftp, nil
}

// isSFTPTransportError 判断错误是否意味着「这条 SFTP 连接已经不可用」。
//
// 注意区分：*sftp.StatusError 说明协议本身工作正常（服务端明确回了一个业务码，
// 比如「没有这个文件」），那不该把整条连接丢掉。
func isSFTPTransportError(err error) bool {
	if err == nil {
		return false
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		switch uint32(se.FxCode()) {
		case uint32(sftp.ErrSSHFxConnectionLost), uint32(sftp.ErrSSHFxNoConnection):
			return true
		default:
			return false
		}
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE)
}

// ---------------------------------------------------------------------------
// 路径处理
// ---------------------------------------------------------------------------

// validateRemotePath 拒绝含 `..` 段的远程路径。
//
// 先按 `/` 切段检查再 Clean：Clean 会把 `/a/../../b` 规整成 `/b`（逃逸被抹掉，
// 用户看到的是「路径已生效」），而我们要的是一句明确的「路径不允许包含 ..」。
func validateRemotePath(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return errors.New("路径不允许包含 ..")
		}
	}
	return nil
}

// resolveRemotePath 把请求里的 path 解析成绝对路径：空 → 家目录；相对路径 → 相对家目录。
//
// 前端的 path 一律来自 FsEntry.path / home（都是绝对路径），相对路径这条兜底是为了
// curl 手工调试，以及用户手工输入的路径也能用。
func resolveRemotePath(sess *sftpSession, p string) (string, error) {
	// ⚠️ **不要**对 p 本身做 TrimSpace：那会把「名字以空格结尾的目录」静默改成另一个
	// 不存在的路径（e2e 实测：`evil $(touch ` 这种名字在 fs/list 里能列出、后续所有操作
	// 都报 file does not exist，因为查询时被裁成了 `evil $(touch`）。TrimSpace 只用于
	// 判断「整串都是空白」这一种情况，其余一律原样交给 SFTP。
	if strings.TrimSpace(p) == "" {
		return sess.homeDir()
	}
	if err := validateRemotePath(p); err != nil {
		return "", err
	}
	if strings.HasPrefix(p, "/") {
		return path.Clean(p), nil
	}
	home, err := sess.homeDir()
	if err != nil {
		return "", err
	}
	return path.Join(home, p), nil
}

// requiredRemotePath 用于必须有目标的操作（mkdir/create/rename/remove/read/download）。
func requiredRemotePath(sess *sftpSession, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("缺少 path")
	}
	return resolveRemotePath(sess, p)
}

// validateUploadName 校验上传项的 filename 并切成路径段。
//
// 上传的 filename 可能是**相对路径**（目录上传：`a/b.txt`），所以不能只取 path.Base
// ——那样整个目录会被拍平成一层。逐段校验：拒绝空段、`.`、`..` 与绝对路径。
func validateUploadName(name string) ([]string, error) {
	// 浏览器在 Windows 上可能回传反斜杠分隔的相对路径，统一成 `/`。
	cleaned := strings.ReplaceAll(name, "\\", "/")
	if cleaned == "" {
		return nil, errors.New("上传项没有文件名")
	}
	if strings.HasPrefix(cleaned, "/") {
		return nil, fmt.Errorf("上传项不允许是绝对路径：%s", name)
	}
	segs := strings.Split(cleaned, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		switch seg {
		case "":
			return nil, fmt.Errorf("上传路径包含空段：%s", name)
		case ".", "..":
			return nil, fmt.Errorf("上传路径包含 %q：%s", seg, name)
		}
		// 控制字符会把日志与界面排版搅乱，也可能是别的协议层的注入源，直接拒绝。
		if strings.ContainsFunc(seg, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("上传路径包含控制字符：%q", name)
		}
		out = append(out, seg)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

// fsError 把一次文件操作的失败翻成带裸码的错误响应。
//
// 以前（参考实现）这类接口一律 404/500，两个方向都不对：权限、类型错误回 404，而 404
// 在前端往往被当成「这个东西不存在」的正常信号；「路径不合法」这种用户改一下就能过的
// 错回 500，又把真故障淹掉。
func fsError(action string, err error) *apierr.Error {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		// withSFTP 已经给了明确 code 的错误（会话未打开 / SFTP 不可用 / 已取消）原样上抛。
		return apiErr
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 客户端关页面 / 点取消时，阻塞中的操作会以 ctx 错误返回。那不是服务端故障，
		// 报成 internal 会让日志里全是「内部错误」，而真正的原因（人走了）反而看不见。
		return apierr.Aborted("%s：请求已取消", action)
	case errors.Is(err, os.ErrNotExist):
		return apierr.NotFound("%s：%s", action, msg)
	case errors.Is(err, os.ErrPermission):
		return apierr.Permission("%s：%s", action, msg)
	case errors.Is(err, os.ErrExist):
		return apierr.Duplicate("%s：%s", action, msg)
	case errors.Is(err, errTargetIsDir), errors.Is(err, errKindMismatch), errors.Is(err, errRefuseRoot),
		errors.Is(err, errNesting), errors.Is(err, errRootOp), errors.Is(err, errInvalidLocalName),
		errors.Is(err, errNotRegularFile):
		return apierr.Invalid("%s：%s", action, msg)
	case strings.Contains(msg, "不允许包含 .."), strings.Contains(msg, "缺少 path"),
		strings.Contains(msg, "上传路径"), strings.Contains(msg, "上传项"):
		return apierr.Invalid("%s：%s", action, msg)
	default:
		if code, ok := sftpStatusCode(err); ok && code == sftpFxFileAlreadyExists {
			return apierr.Duplicate("%s：%s", action, msg)
		}
		return apierr.Internal("%s：%s", action, msg)
	}
}

func sftpStatusCode(err error) (uint32, bool) {
	var se *sftp.StatusError
	if errors.As(err, &se) {
		return se.Code, true
	}
	return 0, false
}

func tooLargeBody(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// ---------------------------------------------------------------------------
// 处理器：/api/fs/*
// ---------------------------------------------------------------------------
