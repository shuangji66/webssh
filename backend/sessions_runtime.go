package main

import (
	"context"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
	"webssh/backend/internal/store"
)

// open 打开一个会话（幂等）：已打开就直接返回现有运行时，**不重建终端**。
//
// 打开时拨一次号：这样「地址写错 / 密码错 / 指纹没信任」这些错误会在用户点「打开」时
// 立刻以带错误码的响应返回（前端显示「打开失败：认证失败」），而不是等他开终端时
// 只在 WebSocket 里收到一个 gone 控制帧。这条连接同时作为 SFTP 的载体。
//
// 凭据（契约 11.1）：请求体里可以带明文凭据。对「不保存凭据」的会话，这是**唯一**的凭据
// 来源——没带、内存里也没有时返回 credential_required（409），前端据此弹输入框。带凭据时
// 先拨号，成功后把它留在内存里供后续终端 / SFTP / 远程命令复用；会话关闭或进程退出即失效。
func (m *sessionManager) open(ctx context.Context, id string, req openCredentials) (*sessionRuntime, *apierr.Error) {
	if !store.ValidID(id) {
		return nil, apierr.NotFound("会话不存在")
	}
	m.mu.Lock()
	cfg, ok := m.configs[id]
	if !ok {
		m.mu.Unlock()
		return nil, apierr.NotFound("会话不存在：%s", id)
	}
	if rt, isOpen := m.runtimes[id]; isOpen {
		m.mu.Unlock()
		return rt, nil
	}
	m.mu.Unlock()

	cred, apiErr := m.resolveCredential(cfg, req)
	if apiErr != nil {
		return nil, apiErr
	}
	dreq := dialRequest{cfg: cfg, password: cred.password, privateKey: cred.privateKey, passphrase: cred.passphrase}
	rt := newSessionRuntime(m, cfg)
	if apiErr := rt.connect(ctx, dreq); apiErr != nil {
		rt.close()
		return nil, apiErr
	}
	// 拨号成功后才留下请求里的凭据（失败的凭据没有任何复用价值）：写进内存时**只**进
	// memCreds，不碰任何落盘路径，也不进日志与响应（见 credentials.go 的纪律 1）。
	if req.provided() {
		m.setCachedCredential(id, cred)
	}

	m.mu.Lock()
	if existing, isOpen := m.runtimes[id]; isOpen {
		// 并发 open（两个标签页同时点「打开」）：另一路已经建好了，用它。
		m.mu.Unlock()
		rt.close()
		return existing, nil
	}
	m.runtimes[id] = rt
	m.mu.Unlock()

	m.touchLastUsed(id)
	return rt, nil
}

// touchLastUsed 更新 lastUsedAt。**换成新对象**再放回 map（见 configs 的注释）。
func (m *sessionManager) touchLastUsed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.configs[id]
	if !ok {
		return
	}
	cp := *cur
	cp.LastUsedAt = time.Now().Format(time.RFC3339)
	m.configs[id] = &cp
	if err := m.st.SaveSession(&cp); err != nil {
		logx.Warnf("更新会话 %s 的 lastUsedAt 失败：%v", id, err)
	}
}

// closeRuntime 关闭一个已打开的会话：终止它下面全部终端与 SFTP 连接（配置保留）。
//
// 对「未打开的会话」是幂等的 no-op：前端在标签关闭、后端重启后都可能重复调用它，
// 回一个错误除了让界面弹一句没用的提示之外没有任何价值。
func (m *sessionManager) closeRuntime(id string) *apierr.Error {
	if !store.ValidID(id) {
		return apierr.NotFound("会话不存在")
	}
	m.mu.Lock()
	if _, ok := m.configs[id]; !ok {
		m.mu.Unlock()
		return apierr.NotFound("会话不存在：%s", id)
	}
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	// 会话关闭 = 内存凭据失效（契约 11.1）：下次 open 会重新要求输入凭据。
	// 这里就在锁里删，避免与并发的 open 抢出一个「关掉之后还能用」的窗口。
	m.dropCachedCredentialLocked(id)
	m.mu.Unlock()
	if rt != nil {
		rt.close()
	}
	return nil
}

// runtimeFor 取出一个已打开的会话运行时（终端 / 文件接口的统一入口）。
func (m *sessionManager) runtimeFor(id string) (*sessionRuntime, *apierr.Error) {
	if m == nil {
		return nil, apierr.Internal("会话管理器未初始化")
	}
	if !store.ValidID(id) {
		return nil, errSessionGone("会话不存在")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.configs[id]; !ok {
		return nil, errSessionGone("会话不存在（可能已被删除）：%s", id)
	}
	if rt, ok := m.runtimes[id]; ok {
		return rt, nil
	}
	return nil, errNotOpen("会话尚未打开：%s", id)
}

// states 返回全部已打开会话的实时状态（/api/sessions 的 open 字段）。
func (m *sessionManager) states() []sessionState {
	m.mu.Lock()
	runtimes := make([]*sessionRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		runtimes = append(runtimes, rt)
	}
	m.mu.Unlock()
	out := make([]sessionState, 0, len(runtimes))
	for _, rt := range runtimes {
		out = append(out, rt.state())
	}
	sortStates(out)
	return out
}

// closeAll 在进程退出时终止所有终端与 SFTP 连接，并丢掉全部内存凭据。
func (m *sessionManager) closeAll() {
	m.mu.Lock()
	runtimes := make([]*sessionRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		runtimes = append(runtimes, rt)
	}
	m.runtimes = map[string]*sessionRuntime{}
	m.memCreds = map[string]*memoryCredential{}
	m.mu.Unlock()
	for _, rt := range runtimes {
		rt.close()
	}
}

// ---------------------------------------------------------------------------
// 已打开的会话
// ---------------------------------------------------------------------------

// sessionRuntime 是一次「已打开」的会话：一个 SFTP 通道 + 若干独立终端。
type sessionRuntime struct {
	mgr      *sessionManager
	id       string
	openedAt time.Time

	mu        sync.Mutex
	terminals map[string]*terminal
	order     []string
	closed    bool

	// sftpMu 串行化「SFTP 连接建立 + 全部文件操作」。pkg/sftp 的客户端本身支持并发，
	// 但把会话状态收敛成串行的更可控（契约第 5.5 节），也顺便解决了「连接坏了要重连」
	// 与「并发操作共用一条坏连接」的竞态。
	sftpMu sync.Mutex
	sftp   *sftpSession
	// base 是打开会话时建立的连接：SFTP 通道挂在它上面（终端的连接各自独立）。
	// 远端家目录的缓存在 sftpSession 里（与登录同生命周期）。
	base *ssh.Client

	// ownerMu 保护 ownerNames：{uid,gid} → {账号名, 组名}（见 resolveOwnerNames）。
	// 名字只用于显示（权限判定用的是数字 uid/gid），所以进程内缓存到会话结束就够：
	// 远端改了账号名要重开会话才看得到，这个代价远小于「每点一次『信息』就新拨一条 SSH」。
	// 键用 (uid,gid) 成对：一次查询同时拿两个名字，只命中一半就等于拿到半份错误结果。
	ownerMu    sync.Mutex
	ownerNames map[ownerKey][2]string
}

// ownerKey 是属主名字缓存的键（uid + gid）。
type ownerKey struct {
	uid uint32
	gid uint32
}

func newSessionRuntime(mgr *sessionManager, cfg *store.Session) *sessionRuntime {
	return &sessionRuntime{
		mgr:       mgr,
		id:        cfg.ID,
		openedAt:  time.Now(),
		terminals: map[string]*terminal{},
	}
}

// connect 建立会话连接：拨号（校验主机密钥与认证）+ 尝试打开 SFTP 通道。
//
// SFTP 建立失败**不阻断打开**：没有 SFTP 子系统的主机仍然可以开终端（只是文件管理
// 用不了），把整个会话判为失败反而更差。失败记一条日志，并由下一次文件操作重试。
func (r *sessionRuntime) connect(ctx context.Context, req dialRequest) *apierr.Error {
	res, cerr := r.mgr.dial(ctx, req)
	if cerr != nil {
		return errFromConnect(cerr)
	}
	r.sftpMu.Lock()
	r.base = res.client
	if err := r.openSFTPLocked(ctx); err != nil {
		logx.Warnf("会话 %s 建立 SFTP 通道失败（文件管理会在首次操作时重试）：%v", r.id, err)
	}
	r.sftpMu.Unlock()
	return nil
}

// state 汇总实时状态。
func (r *sessionRuntime) state() sessionState {
	r.mu.Lock()
	terms := make([]terminalInfo, 0, len(r.order))
	for _, tid := range r.order {
		if t, ok := r.terminals[tid]; ok {
			terms = append(terms, t.info())
		}
	}
	r.mu.Unlock()
	return sessionState{
		ID:        r.id,
		Open:      true,
		OpenedAt:  r.openedAt.Format(time.RFC3339),
		Terminals: terms,
		SFTPOpen:  r.sftpReady(),
	}
}

func (r *sessionRuntime) config() (*store.Session, bool) {
	return r.mgr.getConfig(r.id)
}

// newTerminal 新建一个终端：**每条终端一条独立的 SSH 连接**。
//
// 不共用会话连接：一个终端的网络抖动 / shell 退出不该带走同一会话里的其他终端与
// SFTP 通道（用户会在一个标签里跑长命令，另一个标签里编辑文件）。
func (r *sessionRuntime) newTerminal(ctx context.Context, cols, rows uint16) (*terminal, *apierr.Error) {
	cfg, ok := r.config()
	if !ok {
		return nil, errSessionGone("会话不存在：%s", r.id)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errSessionGone("会话已关闭：%s", r.id)
	}
	r.mu.Unlock()

	req, apiErr := r.mgr.dialRequestOf(cfg)
	if apiErr != nil {
		return nil, apiErr
	}
	res, cerr := r.mgr.dial(ctx, req)
	if cerr != nil {
		return nil, errFromConnect(cerr)
	}

	t, apiErr := r.startTerminal(res.client, cols, rows)
	if apiErr != nil {
		res.client.Close()
		return nil, apiErr
	}
	return t, nil
}

// startTerminal 在一条已建立的连接上开 shell 并登记终端（失败时由调用方关连接）。
func (r *sessionRuntime) startTerminal(client *ssh.Client, cols, rows uint16) (*terminal, *apierr.Error) {
	tid := store.NewID()
	hist, err := r.mgr.st.CreateHistoryFile(r.id, tid)
	if err != nil {
		return nil, apierr.Internal("创建终端历史文件失败：%v", err)
	}
	sess, err := client.NewSession()
	if err != nil {
		hist.Close()
		os.Remove(hist.Name())
		return nil, apierr.ShellFailed("无法打开 SSH 会话通道：%v", err)
	}
	t := &terminal{
		id:        tid,
		runtime:   r,
		client:    client,
		sess:      sess,
		hist:      hist,
		createdAt: time.Now(),
		cols:      cols,
		rows:      rows,
		exitedCh:  make(chan struct{}),
	}
	if apiErr := t.start(cols, rows); apiErr != nil {
		t.close()
		return nil, apiErr
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		t.close()
		return nil, errSessionGone("会话已关闭：%s", r.id)
	}
	r.terminals[tid] = t
	r.order = append(r.order, tid)
	r.mu.Unlock()

	go t.pump()
	go r.reapWhenExited(t)
	return t, nil
}

func (r *sessionRuntime) terminal(tid string) (*terminal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.terminals[tid]
	return t, ok
}

func (r *sessionRuntime) forgetTerminal(tid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.terminals, tid)
	for i, id := range r.order {
		if id == tid {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// closeTerminal 关闭一个终端（终止它的 shell 与连接）。
func (r *sessionRuntime) closeTerminal(tid string) *apierr.Error {
	t, ok := r.terminal(tid)
	if !ok {
		return apierr.NotFound("终端不存在：%s", tid)
	}
	t.close()
	r.forgetTerminal(tid)
	return nil
}

// reapWhenExited 在 shell 退出后再等 exitedGrace 就回收终端。
//
// 已退出的终端没有交互价值（PTY 已死，输入只会得到 EIO），不回收就会在进程生命周期内
// 无限堆积（每条终端还占着一个 SSH 连接与一个历史文件）。宽限期内历史仍在，
// 前端此时挂载仍能回放（拿到 ready + 历史 + [process exited]）。
func (r *sessionRuntime) reapWhenExited(t *terminal) {
	// pump 结束时一定会关掉 exitedCh（shell 退出 / 连接断开 / 终端被显式关闭都走同一条路），
	// 所以这里只要等它，不需要第二个信号。
	<-t.exitedCh
	time.Sleep(exitedGrace())
	if cur, ok := r.terminal(t.id); ok && cur == t && !t.isClosed() {
		t.close()
		r.forgetTerminal(t.id)
	}
}

// close 关闭整个会话：终止全部终端 + SFTP 连接，然后删掉历史目录（配置保留）。
func (r *sessionRuntime) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	terms := make([]*terminal, 0, len(r.order))
	for _, tid := range r.order {
		if t, ok := r.terminals[tid]; ok {
			terms = append(terms, t)
		}
	}
	r.terminals = map[string]*terminal{}
	r.order = nil
	r.mu.Unlock()

	for _, t := range terms {
		t.close()
	}
	r.sftpMu.Lock()
	r.dropSFTPLocked()
	r.sftpMu.Unlock()
	r.mgr.st.RemoveHistoryDir(r.id)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------
