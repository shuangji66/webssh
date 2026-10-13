package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
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

// 下面两个哨兵错误都是「参数问题」（400），不是服务端故障：
//   - errTargetIsDir：界面上点错了入口（比如对着目录点「预览」）；
//   - errKindMismatch：目录删除的 dir 参数与真实类型不一致（此时报错比删掉更安全）。
var (
	errTargetIsDir  = errors.New("目标是目录")
	errKindMismatch = errors.New("dir 参数与真实类型不一致")
	errRefuseRoot   = errors.New("拒绝删除根目录")
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
		return errSFTPUnavailable("远端没有可用的 SFTP 子系统：%v", err)
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
		return errAborted("请求已取消")
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
		return errAborted("请求已取消")
	}
}

// sftpSessionLocked 拿到可用的 SFTP 会话：没有就建，建失败按契约重连一次。
//
// 只对「连接层失败」重试：拨号/认证失败（*apiError 的连接类错误）重试一次不会有不同结果，
// 白白让用户多等一轮超时。
func (r *sessionRuntime) sftpSessionLocked(ctx context.Context) (*sftpSession, error) {
	if r.sftp != nil {
		return r.sftp, nil
	}
	err := r.openSFTPLocked(ctx)
	if err == nil {
		return r.sftp, nil
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return nil, err
	}
	// 第一次失败（多半是上一条连接被对端踢掉、或者 channel 数满了）：丢掉重拨一次。
	r.dropSFTPLocked()
	if err2 := r.openSFTPLocked(ctx); err2 != nil {
		warnf("会话 %s 重建 SFTP 通道失败：%v", r.id, err2)
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
func fsError(action string, err error) *apiError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		// withSFTP 已经给了明确 code 的错误（会话未打开 / SFTP 不可用 / 已取消）原样上抛。
		return apiErr
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// 客户端关页面 / 点取消时，阻塞中的操作会以 ctx 错误返回。那不是服务端故障，
		// 报成 internal 会让日志里全是「内部错误」，而真正的原因（人走了）反而看不见。
		return errAborted("%s：请求已取消", action)
	case errors.Is(err, os.ErrNotExist):
		return errNotFound("%s：%s", action, msg)
	case errors.Is(err, os.ErrPermission):
		return errPermission("%s：%s", action, msg)
	case errors.Is(err, os.ErrExist):
		return errDuplicate("%s：%s", action, msg)
	case errors.Is(err, errTargetIsDir), errors.Is(err, errKindMismatch), errors.Is(err, errRefuseRoot),
		errors.Is(err, errNesting), errors.Is(err, errRootOp), errors.Is(err, errInvalidLocalName):
		return errInvalid("%s：%s", action, msg)
	case strings.Contains(msg, "不允许包含 .."), strings.Contains(msg, "缺少 path"),
		strings.Contains(msg, "上传路径"), strings.Contains(msg, "上传项"):
		return errInvalid("%s：%s", action, msg)
	default:
		if code, ok := sftpStatusCode(err); ok && code == sftpFxFileAlreadyExists {
			return errDuplicate("%s：%s", action, msg)
		}
		return errInternal("%s：%s", action, msg)
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

type fsReq struct {
	SID  string `json:"sid"`
	Path string `json:"path"`
}

// handleFsList 列目录：path 为空时返回家目录，并把 home 一并回给前端（面包屑的「家目录」入口）。
func (m *AdminMux) handleFsList(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true, "path": resolved, "home": home, "entries": entries})
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
func (m *AdminMux) handleFsInfo(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true, "info": info})
}

// resolveOwnerNames 在远端把 uid/gid 查成账号名与组名（`id -nu` / `id -ng`）。
//
// 为什么值得多跑一条命令：SFTP v3 的 attrs 里只有数字 uid/gid，而「这个文件是谁的」几乎总是
// 想知道*名字*（权限问题一半是「属主是 root，所以我写不进去」）。查不到（远端没有 id、
// 账号已删、或这条命令超时）一律返回空串，由界面退回显示数字——**绝不编一个名字**。
//
// 用 `printf` 把两行结果显式分开：`id -nu` 失败时不能把组名当成账号名读进来。
func (r *sessionRuntime) resolveOwnerNames(ctx context.Context, uid, gid uint32) (string, string) {
	if r == nil {
		return "", ""
	}
	cmd := "printf '%s\n' \"$(id -nu " + strconv.FormatUint(uint64(uid), 10) +
		" 2>/dev/null)\" \"$(id -ng " + strconv.FormatUint(uint64(gid), 10) + " 2>/dev/null)\""
	res, err := r.runRemote(ctx, cmd)
	if err != nil {
		// 查名字失败不该让「信息」整个失败：uid/gid 已经有了，界面显示数字就够用
		warnf("会话 %s 查属主名字失败（uid=%d gid=%d）：%v", r.id, uid, gid, err)
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
	return owner, group
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

// handleFsMkdir 新建目录。已存在 → duplicate（前端提示「已存在」，而不是静默成功——
// 静默成功会让用户以为新建的是一个空目录，实际进去看到的是别人的内容）。
func (m *AdminMux) handleFsMkdir(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true})
}

// handleFsCreate 新建空文件（已存在 → duplicate）。
//
// 用 O_EXCL：pkg/sftp 的 Create 是 O_CREATE|O_TRUNC，同名会把用户那份**清成 0 字节**再回
// 一个成功——「新建文件」时手滑打了已有文件名，内容就没了，还没有确认。
func (m *AdminMux) handleFsCreate(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true})
}

// handleFsRename 重命名 / 移动。目标已存在 → duplicate。
//
// SFTP 的 rename 是 POSIX rename：会**直接覆盖**同名目标（pkg/sftp 还提供 PosixRename）。
// 不先查一次存在性的话，「重命名」两个同目录下的文件会把其中一个静默吞掉。
func (m *AdminMux) handleFsRename(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true})
}

// handleFsRemove 删除。dir 必须与真实类型一致：界面把「删除」与「删除目录」分成两个入口，
// 参数传错说明前端状态与实际类型不一致，此时**报错比删掉更安全**（否则可能删掉整棵树）。
func (m *AdminMux) handleFsRemove(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{"ok": true})
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

// handleFsUpload 处理本地文件 / 目录上传（multipart：sid、path、overwrite、files[]）。
func (m *AdminMux) handleFsUpload(w http.ResponseWriter, r *http.Request) *apiError {
	// MaxBytesReader 卡整条请求（见 maxUploadBytes 的注释）；它同时让客户端能在超限时
	// 收到明确响应，而不是连接被直接掐断。
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(uploadMemoryLimit); err != nil {
		if tooLargeBody(err) {
			return errTooLarge("上传内容超过上限 %s", humanSize(maxUploadBytes))
		}
		return errInvalid("解析上传内容失败：%v", err)
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
		return errInvalid("没有收到文件（字段名应为 files）")
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
				return errAborted("上传已取消")
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
			size, skip, werr := writeRemote(r.Context(), sess.client, dest, src, overwrite)
			src.Close()
			if werr != nil {
				// 写了一半的文件不能留：它顶掉的可能是同名的那份（overwrite），
				// 留在那儿就是一个看着正常、其实是残渣的文件。
				_ = sess.client.Remove(dest)
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
	return respond(w, map[string]any{"ok": true, "files": written, "skipped": skipped})
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
func decodeFormFilenameEscapes(name string) string {
	replacer := strings.NewReplacer("%22", `"`, "%0D", "\r", "%0A", "\n",
		"%22", `"`, "%0d", "\r", "%0a", "\n")
	return replacer.Replace(name)
}

// writeRemote 边收边写一个上传项（不整份读进内存：放几百 MB 的二进制是常见用法）。
//
// overwrite=false 的语义是「别动已有的文件」：只省掉一次 O_TRUNC 是不够的
// （pkg/sftp 的 Create/OpenFile 本身就是 O_TRUNC），存在就整份跳过并计数。
func writeRemote(ctx context.Context, c *sftp.Client, dest string, src io.Reader, overwrite bool) (int64, bool, error) {
	if !overwrite {
		if _, err := c.Lstat(dest); err == nil {
			return 0, true, nil
		}
	}
	if err := c.MkdirAll(path.Dir(dest)); err != nil {
		return 0, false, err
	}
	f, err := c.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, false, err
	}
	n, err := io.Copy(f, ctxReader{ctx: ctx, r: src})
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return n, false, err
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
func mapTransferError(action string, err error) *apiError {
	if errors.Is(err, errTransferBudget) {
		return errTooLarge("%s：内容超过单次上限 %s", action, humanSize(maxTransferBytes))
	}
	if errors.Is(err, errInvalidLocalName) {
		return errInvalid("%s：文件名不合法（%v）", action, err)
	}
	return fsError(action, err)
}

// handleFsUploadFromNas 把飞牛（本机）上已授权的文件推送到远程主机。
//
// 读之前尽量用 trim.file.checkUserACL 判一次读权限：pickUserFile 只保证「应用用户」
// 拿到了 ACL，不代表当前登录用户也能读这个文件。飞牛不可用时跳过并记日志（这台机器上
// 没有网关，也就没有那层授权），不能因此把功能整个禁掉。
func (m *AdminMux) handleFsUploadFromNas(w http.ResponseWriter, r *http.Request) *apiError {
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
		return errInvalid("缺少 source")
	}
	if !strings.HasPrefix(source, "/") {
		return errInvalid("source 必须是本机绝对路径：%s", source)
	}
	if err := validateRemotePath(source); err != nil {
		return errInvalid("source 不合法：%v", err)
	}
	rt, apiErr := m.sessions.runtimeFor(req.SID)
	if apiErr != nil {
		return apiErr
	}

	// ACL 判定（尽力而为）。
	if m.trim.available(r.Context()) {
		u := gatewayUserOf(r)
		if acls, err := m.trim.checkUserACL(r.Context(), u.UID, []string{source}); err != nil {
			warnf("检查本机文件 %s 的用户权限失败，跳过校验：%v", source, err)
		} else if len(acls) > 0 && !acls[0].Readable {
			return errPermission("当前用户对 %s 没有读权限", source)
		}
	}

	fi, err := os.Stat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNotFound("本机文件不存在：%s", source)
		}
		return errInternal("读取本机文件失败：%v", err)
	}
	if fi.IsDir() {
		// 飞牛文件选择器给的是**文件**（upload_nas_hint 也写明「暂不支持选择目录」）。
		// 真要支持整目录，得连同相对路径逐项推送，属于另一个功能。
		return errInvalid("暂不支持上传目录（%s 是目录）：请改用「本地目录」上传", source)
	}
	if !fi.Mode().IsRegular() {
		// FIFO 没有写者时 open(2) 会永久阻塞，设备节点也不该这么读。
		return errInvalid("只支持普通文件：%s", source)
	}
	if fi.Size() > maxNasFileBytes {
		return errTooLarge("文件超过上限 %s：%s", humanSize(maxNasFileBytes), humanSize(fi.Size()))
	}

	name := path.Base(source)
	segs, verr := validateUploadName(name)
	if verr != nil {
		return errInvalid("文件名不合法：%v", verr)
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
			_ = sess.client.Remove(dest)
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
	return respond(w, map[string]any{"ok": true, "files": written, "skipped": skipped})
}

// handleFsDownload 下载：文件直接流式发正文，目录打成 zip。
//
// 目录**先打进临时文件再发正文**：响应头一旦发出就没法再回一个 JSON 错误，中途失败
// （某个文件读不了——权限位 0000 的文件正是这形态）会留下一个「结构完整、里面是 0 字节
// 条目」的 zip，用户拿到 200 却永远不知道少了东西。代价是下载期间多占一份临时空间。
func (m *AdminMux) handleFsDownload(w http.ResponseWriter, r *http.Request) *apiError {
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
		warnf("下载 %s 中断：%v", req.Path, err)
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
				warnf("打包下载：跳过符号链接 %s", full)
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
func (m *AdminMux) handleFsRead(w http.ResponseWriter, r *http.Request) *apiError {
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
	return respond(w, map[string]any{
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
