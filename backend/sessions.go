package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 认证方式（SessionView.authType，前端 AuthType 只有这两个取值）。
const (
	authTypePassword = "password"
	authTypeKey      = "key"
)

// ---------------------------------------------------------------------------
// API 数据结构（契约第 4.3 节；字段名与 frontend/src/serverapi/index.ts 逐字对应）
// ---------------------------------------------------------------------------

// sessionView 是「远程会话配置」的对外形态，**不含任何明文凭据**。
type sessionView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	AuthType       string `json:"authType"`
	HasPassword    bool   `json:"hasPassword"`
	HasPrivateKey  bool   `json:"hasPrivateKey"`
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	Note           string `json:"note"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	LastUsedAt     string `json:"lastUsedAt,omitempty"`
}

// sessionInput 是新增 / 编辑会话的输入。
//
// 凭据字段用指针：`null`/缺省 = 保留已存值，空字符串 = 清除该字段。
// 前端保证「不修改时字段根本不发」（见契约第 4.3 节），这里把两种语义分开才能照做。
type sessionInput struct {
	ID         string  `json:"id"`
	Name       *string `json:"name"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	User       string  `json:"user"`
	AuthType   string  `json:"authType"`
	Password   *string `json:"password"`
	PrivateKey *string `json:"privateKey"`
	Passphrase *string `json:"passphrase"`
	Note       *string `json:"note"`
}

// terminalInfo 是一个终端的摘要（契约第 4.3 节 TerminalInfo）。
type terminalInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
	Exited    bool   `json:"exited"`
}

// sessionState 是「已打开会话」的实时状态（纯内存态，进程重启即消失）。
type sessionState struct {
	ID        string         `json:"id"`
	Open      bool           `json:"open"`
	OpenedAt  string         `json:"openedAt,omitempty"`
	Terminals []terminalInfo `json:"terminals"`
	// SFTPOpen 表示这条会话的 SFTP 通道当前是否可用：打开会话时会尝试建立，
	// 断了（或首次建立失败）在下一次文件操作时重连一次。
	SFTPOpen bool `json:"sftpOpen"`
}

// ---------------------------------------------------------------------------
// 会话管理器
// ---------------------------------------------------------------------------

// sessionManager 持有全部会话配置（内存镜像 + 落盘）与已打开的会话。
type sessionManager struct {
	renv    *RuntimeEnv
	st      *store
	crypt   *cryptor
	hks     *hostKeyStore
	pending *pendingKeys

	mu sync.Mutex
	// configs 是会话配置的内存镜像。**更新一律换成新对象**（见 save）：其他协程可能
	// 正拿着旧指针在拨号，就地改字段会数据竞争（`go test -race` 会报）。
	configs  map[string]*sessionConfig
	runtimes map[string]*sessionRuntime
}

func newSessionManager(renv *RuntimeEnv, st *store, crypt *cryptor, hks *hostKeyStore) (*sessionManager, error) {
	m := &sessionManager{
		renv:     renv,
		st:       st,
		crypt:    crypt,
		hks:      hks,
		pending:  newPendingKeys(),
		configs:  map[string]*sessionConfig{},
		runtimes: map[string]*sessionRuntime{},
	}
	// 历史文件没有跨进程的意义（见 purgeStaleHistory 的说明）。
	st.purgeStaleHistory()
	saved, err := st.listSessions()
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败：%w", err)
	}
	for _, sc := range saved {
		m.configs[sc.ID] = sc
	}
	return m, nil
}

// views 返回全部会话配置（供 /api/sessions 的 sessions 字段）。
func (m *sessionManager) views() []sessionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sessionView, 0, len(m.configs))
	for _, sc := range m.configs {
		out = append(out, sc.view())
	}
	sortViews(out)
	return out
}

func (m *sessionManager) getConfig(id string) (*sessionConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.configs[id]
	return sc, ok
}

// credentials 解密一个会话的凭据（密码 / 私钥 / 口令）。
//
// **不做任何静默降级**：解密失败就把错误带出去（契约第 3 节）——静默当成空密码的表现是
// 「密码莫名其妙失效了」，而真实原因（master.key 换了 / 密文被搬过）永远查不出来。
func (m *sessionManager) credentials(sc *sessionConfig) (password, privateKey, passphrase string, apiErr *apiError) {
	var err error
	if password, err = m.crypt.decrypt(fieldPassword, sc.ID, sc.Password); err != nil {
		return "", "", "", errInternal("会话 %s 的密码解密失败：%v", sc.ID, err)
	}
	if privateKey, err = m.crypt.decrypt(fieldPrivateKey, sc.ID, sc.PrivateKey); err != nil {
		return "", "", "", errInternal("会话 %s 的私钥解密失败：%v", sc.ID, err)
	}
	if passphrase, err = m.crypt.decrypt(fieldPassphrase, sc.ID, sc.Passphrase); err != nil {
		return "", "", "", errInternal("会话 %s 的私钥口令解密失败：%v", sc.ID, err)
	}
	return password, privateKey, passphrase, nil
}

// dialRequestOf 组装一次拨号所需的输入（凭据按需解密）。
func (m *sessionManager) dialRequestOf(sc *sessionConfig) (dialRequest, *apiError) {
	password, privateKey, passphrase, apiErr := m.credentials(sc)
	if apiErr != nil {
		return dialRequest{}, apiErr
	}
	return dialRequest{cfg: sc, password: password, privateKey: privateKey, passphrase: passphrase}, nil
}

// validate 校验会话的身份字段（校验通过后返回规整化的副本）。
func validateSessionFields(host string, port int, user, authType string) (string, string, int, *apiError) {
	host = strings.TrimSpace(host)
	user = strings.TrimSpace(user)
	if host == "" {
		return "", "", 0, errInvalid("缺少地址")
	}
	// host 会被拼成 host:port 交给 net.Dial，含分隔符 / 空白的值只会让人看不懂错误。
	if strings.ContainsAny(host, " \t\r\n/") {
		return "", "", 0, errInvalid("地址不能包含空白或斜杠：%q", host)
	}
	if user == "" {
		return "", "", 0, errInvalid("缺少登录名")
	}
	if strings.ContainsAny(user, " \t\r\n@") {
		return "", "", 0, errInvalid("登录名不能包含空白或 @：%q", user)
	}
	if authType != authTypePassword && authType != authTypeKey {
		return "", "", 0, errInvalid("认证方式只能是 password 或 key：%q", authType)
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", "", 0, errInvalid("端口需在 1–65535 之间：%d", port)
	}
	return host, user, port, nil
}

// save 新增或更新会话配置（/api/sessions/save）。
func (m *sessionManager) save(in sessionInput) (*sessionView, *apiError) {
	host, user, port, apiErr := validateSessionFields(in.Host, in.Port, in.User, in.AuthType)
	if apiErr != nil {
		return nil, apiErr
	}
	if in.ID != "" && !validID(in.ID) {
		return nil, errNotFound("会话不存在")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().Format(time.RFC3339)
	created := in.ID == ""
	var sc *sessionConfig
	if in.ID != "" {
		cur, ok := m.configs[in.ID]
		if !ok {
			return nil, errNotFound("会话不存在：%s", in.ID)
		}
		// 复制一份再改：失败路径不污染内存态，成功路径换成一个新对象（见 configs 的注释）。
		cp := *cur
		sc = &cp
		// 身份（user@host:port）是「已打开会话」的连接目标：会话开着的时候改地址/登录名，
		// 运行时还连着旧机器，界面上的卡片却写着新地址——这种不一致必须挡住。
		identityChanged := cur.Host != host || cur.Port != port || cur.User != user
		if identityChanged {
			if _, isOpen := m.runtimes[in.ID]; isOpen {
				return nil, errConflict("会话已打开，请先关闭再修改地址 / 端口 / 登录名")
			}
		}
		sc.Host, sc.User, sc.Port, sc.AuthType = host, user, port, in.AuthType
	} else {
		sc = &sessionConfig{
			Version:   1,
			ID:        newID(),
			Host:      host,
			User:      user,
			Port:      port,
			AuthType:  in.AuthType,
			CreatedAt: now,
		}
	}

	// 会话名称：留空回退 `user@host`（前端的 sessionLabel 也是这么兜底的）。
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		sc.Name = strings.TrimSpace(*in.Name)
	} else if sc.Name == "" {
		sc.Name = defaultSessionName(user, host)
	}
	if in.Note != nil {
		sc.Note = strings.TrimSpace(*in.Note)
	}

	// 身份去重（同一 user@host:port 只允许一个会话）。
	for id, other := range m.configs {
		if id == sc.ID {
			continue
		}
		if other.identity() == sc.identity() {
			return nil, errDuplicate("已存在相同「地址 + 登录名」的会话「%s」", other.Name)
		}
	}

	// 凭据：nil = 不修改，"" = 清除，其余 = 加密后覆盖。
	if in.Password != nil {
		sec, err := m.crypt.encrypt(fieldPassword, sc.ID, *in.Password)
		if err != nil {
			return nil, errInternal("加密密码失败：%v", err)
		}
		sc.Password = sec
	}
	if in.PrivateKey != nil {
		pem := *in.PrivateKey
		if strings.TrimSpace(pem) == "" {
			sc.PrivateKey = nil
			sc.KeyFingerprint = ""
		} else {
			// 本次提交带了新私钥 → 口令以本次提交为准（不带就是「没有口令」）。
			// 沿用已存私钥的场景在 dialRequestForTest 里另外处理。
			passphrase := derefString(in.Passphrase)
			fp, fpErr := keyFingerprint(pem, passphrase)
			if fpErr != nil {
				// 存一个解析不了的私钥等于存一个「永远连不上」的会话，当场报错更省事：
				// 前端的保存流程会把 err_key_parse 显示成「私钥解析失败（内容或口令不对）」。
				return nil, errKeyParse("私钥解析失败：%v", fpErr)
			}
			sec, err := m.crypt.encrypt(fieldPrivateKey, sc.ID, pem)
			if err != nil {
				return nil, errInternal("加密私钥失败：%v", err)
			}
			sc.PrivateKey = sec
			sc.KeyFingerprint = fp
		}
	}
	if in.Passphrase != nil {
		sec, err := m.crypt.encrypt(fieldPassphrase, sc.ID, *in.Passphrase)
		if err != nil {
			return nil, errInternal("加密私钥口令失败：%v", err)
		}
		sc.Passphrase = sec
	}

	// **新建**会话必须给凭据：否则用户会存下一个「打开就报无凭据」的卡片，
	// 而卡片上完全看不出缺什么。
	// 更新时允许把凭据清空（契约第 4.3 节：空串 = 清除该字段）：那是有意的操作，
	// 缺凭据会在连接测试 / 打开会话时给出明确的 no_credential，而不是静默用一个空密码去连。
	if created {
		if sc.AuthType == authTypePassword && sc.Password == nil {
			return nil, errNoCredential("密码认证需要填写登录密码")
		}
		if sc.AuthType == authTypeKey && sc.PrivateKey == nil {
			return nil, errNoCredential("密钥认证需要粘贴私钥内容")
		}
	}

	sc.UpdatedAt = now
	if err := m.st.saveSession(sc); err != nil {
		return nil, errInternal("保存会话失败：%v", err)
	}
	m.configs[sc.ID] = sc
	view := sc.view()
	return &view, nil
}

// delete 删除会话：先关掉运行时（终端 + SFTP），再删配置、专属快捷指令与历史目录
// （前端的确认文案就是这么承诺的：「登录数据、专属快捷指令与该会话的历史都会一并删除」）。
func (m *sessionManager) delete(id string) *apiError {
	if !validID(id) {
		return errNotFound("会话不存在")
	}
	m.mu.Lock()
	if _, ok := m.configs[id]; !ok {
		m.mu.Unlock()
		return errNotFound("会话不存在：%s", id)
	}
	delete(m.configs, id)
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	m.mu.Unlock()

	if rt != nil {
		rt.close()
	}
	// 历史可能还在（会话没打开过、或上次没清理干净）
	m.st.removeHistoryDir(id)
	m.st.deleteQuickCmds(id)
	if err := m.st.deleteSession(id); err != nil {
		return errInternal("删除会话失败：%v", err)
	}
	return nil
}

// trust 记录一条用户确认过的主机密钥（/api/sessions/trust）。
//
// 请求体里只有指纹（前端手上只有指纹），完整公钥来自测试连接时暂存的扫描结果；
// 指纹对不上就拒绝——「随手点信任」不该放行一把没确认过的密钥。
func (m *sessionManager) trust(host string, port int, fingerprint string) *apiError {
	host = strings.TrimSpace(host)
	fingerprint = strings.TrimSpace(fingerprint)
	if host == "" {
		return errInvalid("缺少 host")
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return errInvalid("端口需在 1–65535 之间：%d", port)
	}
	if fingerprint == "" {
		return errInvalid("缺少 fingerprint")
	}
	id := hostKeyID(host, port)
	rec, ok := m.pending.take(id, fingerprint)
	if !ok {
		return errNotFound("没有待确认的主机密钥（%s %s）：请先重新测试连接，再点「信任并继续」", id, fingerprint)
	}
	rec.AddedAt = time.Now().Format(time.RFC3339)
	if err := m.hks.add(id, rec); err != nil {
		return errInternal("写入 known_hosts 失败：%v", err)
	}
	return nil
}

// test 执行一次连接测试（不建立持久会话）。
//
// 失败**不是** HTTP 错误：结果放在 TestOutcome 里（success=false + code），前端据此渲染
// 人话（「认证失败（登录名 / 密码 / 密钥不对）」）。只有参数本身不合法才回 4xx。
func (m *sessionManager) test(ctx context.Context, in sessionInput) (*testOutcome, *apiError) {
	req, apiErr := m.dialRequestForTest(in)
	if apiErr != nil {
		return nil, apiErr
	}
	res, cerr := m.dial(ctx, req)
	if cerr != nil {
		return outcomeOf(cerr), nil
	}
	defer res.client.Close()
	if cerr := probeSFTP(res.client); cerr != nil {
		cerr.Banner = res.banner
		cerr.LatencyMs = int(res.latency.Milliseconds())
		out := outcomeOf(cerr)
		out.Trusted = res.trusted
		return out, nil
	}
	return &testOutcome{
		Success:     true,
		Code:        "ok",
		Stage:       stageOK,
		Fingerprint: res.record.Fingerprint,
		KeyType:     res.record.KeyType,
		Trusted:     res.trusted,
		Banner:      res.banner,
		LatencyMs:   int(res.latency.Milliseconds()),
	}, nil
}

// dialRequestForTest 组装测试用的拨号输入：表单里没重填凭据时回落到已存凭据
// （编辑已有会话时点「测试连接」不该要求用户重新输入密码）。
func (m *sessionManager) dialRequestForTest(in sessionInput) (dialRequest, *apiError) {
	host, user, port, apiErr := validateSessionFields(in.Host, in.Port, in.User, in.AuthType)
	if apiErr != nil {
		return dialRequest{}, apiErr
	}
	sc := &sessionConfig{ID: in.ID, Host: host, User: user, Port: port, AuthType: in.AuthType}
	if !validID(in.ID) {
		// 新建会话（还没有 id）：凭据只能来自请求体。
		sc.ID = "pending"
	}

	var stored *sessionConfig
	if in.ID != "" {
		stored, _ = m.getConfig(in.ID)
	}
	password := derefString(in.Password)
	privateKey := derefString(in.PrivateKey)
	passphrase := derefString(in.Passphrase)
	if stored != nil {
		sp, sk, spp, apiErr := m.credentials(stored)
		if apiErr != nil {
			return dialRequest{}, apiErr
		}
		if in.Password == nil {
			password = sp
		}
		if in.PrivateKey == nil {
			privateKey = sk
			// 私钥沿用已存的那份时，口令也一起沿用（新私钥配旧口令几乎必然解析失败）。
			if in.Passphrase == nil {
				passphrase = spp
			}
		}
	}
	return dialRequest{cfg: sc, password: password, privateKey: privateKey, passphrase: passphrase}, nil
}

func outcomeOf(cerr *connectError) *testOutcome {
	return &testOutcome{
		Success:     false,
		Code:        cerr.Code,
		Detail:      cerr.Detail,
		Stage:       cerr.Stage,
		Fingerprint: cerr.Fingerprint,
		KeyType:     cerr.KeyType,
		Trusted:     cerr.Trusted,
		Mismatch:    cerr.Mismatch,
		Banner:      cerr.Banner,
		LatencyMs:   cerr.LatencyMs,
	}
}

// open 打开一个会话（幂等）：已打开就直接返回现有运行时，**不重建终端**。
//
// 打开时拨一次号：这样「地址写错 / 密码错 / 指纹没信任」这些错误会在用户点「打开」时
// 立刻以带错误码的响应返回（前端显示「打开失败：认证失败」），而不是等他开终端时
// 只在 WebSocket 里收到一个 gone 控制帧。这条连接同时作为 SFTP 的载体。
func (m *sessionManager) open(ctx context.Context, id string) (*sessionRuntime, *apiError) {
	if !validID(id) {
		return nil, errNotFound("会话不存在")
	}
	m.mu.Lock()
	cfg, ok := m.configs[id]
	if !ok {
		m.mu.Unlock()
		return nil, errNotFound("会话不存在：%s", id)
	}
	if rt, isOpen := m.runtimes[id]; isOpen {
		m.mu.Unlock()
		return rt, nil
	}
	m.mu.Unlock()

	req, apiErr := m.dialRequestOf(cfg)
	if apiErr != nil {
		return nil, apiErr
	}
	rt := newSessionRuntime(m, cfg)
	if apiErr := rt.connect(ctx, req); apiErr != nil {
		rt.close()
		return nil, apiErr
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
	if err := m.st.saveSession(&cp); err != nil {
		warnf("更新会话 %s 的 lastUsedAt 失败：%v", id, err)
	}
}

// closeRuntime 关闭一个已打开的会话：终止它下面全部终端与 SFTP 连接（配置保留）。
//
// 对「未打开的会话」是幂等的 no-op：前端在标签关闭、后端重启后都可能重复调用它，
// 回一个错误除了让界面弹一句没用的提示之外没有任何价值。
func (m *sessionManager) closeRuntime(id string) *apiError {
	if !validID(id) {
		return errNotFound("会话不存在")
	}
	m.mu.Lock()
	if _, ok := m.configs[id]; !ok {
		m.mu.Unlock()
		return errNotFound("会话不存在：%s", id)
	}
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	m.mu.Unlock()
	if rt != nil {
		rt.close()
	}
	return nil
}

// runtimeFor 取出一个已打开的会话运行时（终端 / 文件接口的统一入口）。
func (m *sessionManager) runtimeFor(id string) (*sessionRuntime, *apiError) {
	if m == nil {
		return nil, errInternal("会话管理器未初始化")
	}
	if !validID(id) {
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

// closeAll 在进程退出时终止所有终端与 SFTP 连接。
func (m *sessionManager) closeAll() {
	m.mu.Lock()
	runtimes := make([]*sessionRuntime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		runtimes = append(runtimes, rt)
	}
	m.runtimes = map[string]*sessionRuntime{}
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
}

func newSessionRuntime(mgr *sessionManager, cfg *sessionConfig) *sessionRuntime {
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
func (r *sessionRuntime) connect(ctx context.Context, req dialRequest) *apiError {
	res, cerr := r.mgr.dial(ctx, req)
	if cerr != nil {
		return errFromConnect(cerr)
	}
	r.sftpMu.Lock()
	r.base = res.client
	if err := r.openSFTPLocked(ctx); err != nil {
		warnf("会话 %s 建立 SFTP 通道失败（文件管理会在首次操作时重试）：%v", r.id, err)
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

func (r *sessionRuntime) config() (*sessionConfig, bool) {
	return r.mgr.getConfig(r.id)
}

// newTerminal 新建一个终端：**每条终端一条独立的 SSH 连接**。
//
// 不共用会话连接：一个终端的网络抖动 / shell 退出不该带走同一会话里的其他终端与
// SFTP 通道（用户会在一个标签里跑长命令，另一个标签里编辑文件）。
func (r *sessionRuntime) newTerminal(ctx context.Context, cols, rows uint16) (*terminal, *apiError) {
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
func (r *sessionRuntime) startTerminal(client *ssh.Client, cols, rows uint16) (*terminal, *apiError) {
	tid := newID()
	hist, err := r.mgr.st.createHistoryFile(r.id, tid)
	if err != nil {
		return nil, errInternal("创建终端历史文件失败：%v", err)
	}
	sess, err := client.NewSession()
	if err != nil {
		hist.Close()
		os.Remove(hist.Name())
		return nil, errShellFailed("无法打开 SSH 会话通道：%v", err)
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
func (r *sessionRuntime) closeTerminal(tid string) *apiError {
	t, ok := r.terminal(tid)
	if !ok {
		return errNotFound("终端不存在：%s", tid)
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
	r.mgr.st.removeHistoryDir(r.id)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func (sc *sessionConfig) view() sessionView {
	return sessionView{
		ID:             sc.ID,
		Name:           sc.Name,
		Host:           sc.Host,
		Port:           sc.Port,
		User:           sc.User,
		AuthType:       sc.AuthType,
		HasPassword:    sc.Password != nil,
		HasPrivateKey:  sc.PrivateKey != nil,
		KeyFingerprint: sc.KeyFingerprint,
		Note:           sc.Note,
		CreatedAt:      sc.CreatedAt,
		UpdatedAt:      sc.UpdatedAt,
		LastUsedAt:     sc.LastUsedAt,
	}
}

func defaultSessionName(user, host string) string {
	return user + "@" + host
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// errFromConnect 把连接类失败翻成 HTTP 错误响应。
//
// 状态码的选择：连接类失败意味着「远端有问题、本服务正常」，用 502；主机密钥需要用户
// 介入（未知 / 不匹配）用 409；本地的凭据缺 / 解析不了用 400。前端只用 code 做 i18n，
// 状态码是给日志与中间件看的。
func errFromConnect(cerr *connectError) *apiError {
	status := 502
	switch cerr.Code {
	case codeHostKeyUnknown, codeHostKeyMismatch:
		status = 409
	case codeNoCredential, codeKeyParse:
		status = 400
	}
	return &apiError{Code: cerr.Code, Status: status, Message: cerr.Detail}
}

// sortViews / sortStates 只是为了让响应稳定：map 的遍历顺序是随机的，不稳定的话
// 测试断言与人工排查都要靠猜。前端自己会按时序排卡片。
func sortViews(items []sessionView) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt < items[j].CreatedAt
		}
		return items[i].ID < items[j].ID
	})
}

func sortStates(items []sessionState) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

// errSessionGone / errNotOpen 用 i18n 里已有的 err_session_gone / err_not_open 文案。
// 契约第 4.1 节把「会话未打开」归在 conflict 下，但前端两套字典里已经有更准的
// 两个码，用它比让用户看裸中文更贴合界面（见交付报告里的说明）。
func errNotOpen(format string, args ...any) *apiError {
	return failErr(409, "not_open", format, args...)
}

func errSessionGone(format string, args ...any) *apiError {
	return failErr(404, "session_gone", format, args...)
}
