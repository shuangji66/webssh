package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"webssh/backend/internal/apierr"
	"webssh/backend/internal/logx"
	"webssh/backend/internal/secret"
	"webssh/backend/internal/store"
)

// 认证方式（SessionView.authType，前端 AuthType 只有这两个取值）。
const (
	authTypePassword = "password"
	authTypeKey      = "key"
)

// ---------------------------------------------------------------------------
// API 数据结构（契约第 4.3 节；字段名与 frontend/src/serverapi/index.ts 逐字对应）
// ---------------------------------------------------------------------------

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
	st      *store.Store
	crypt   *secret.Cryptor
	hks     *hostKeyStore
	pending *pendingKeys

	mu sync.Mutex
	// configs 是会话配置的内存镜像。**更新一律换成新对象**（见 save）：其他协程可能
	// 正拿着旧指针在拨号，就地改字段会数据竞争（`go test -race` 会报）。
	configs map[string]*store.Session
	// runtimes 是「已打开」的会话；memCreds 是「不保存凭据」的会话在内存里的明文凭据
	// （按会话 id，见 credentials.go）。两者都只在进程内存里，重启即消失。
	runtimes map[string]*sessionRuntime
	memCreds map[string]*memoryCredential
}

func newSessionManager(renv *RuntimeEnv, st *store.Store, crypt *secret.Cryptor, hks *hostKeyStore) (*sessionManager, error) {
	m := &sessionManager{
		renv:     renv,
		st:       st,
		crypt:    crypt,
		hks:      hks,
		pending:  newPendingKeys(),
		configs:  map[string]*store.Session{},
		runtimes: map[string]*sessionRuntime{},
		memCreds: map[string]*memoryCredential{},
	}
	// 历史文件没有跨进程的意义（见 purgeStaleHistory 的说明）。
	st.PurgeStaleHistory()
	saved, err := st.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败：%w", err)
	}
	for _, sc := range saved {
		m.configs[sc.ID] = sc
	}
	return m, nil
}

// views 返回全部会话配置（供 /api/sessions 的 sessions 字段）。
func (m *sessionManager) views() []store.SessionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.SessionView, 0, len(m.configs))
	for _, sc := range m.configs {
		out = append(out, sc.View())
	}
	sortViews(out)
	return out
}

func (m *sessionManager) getConfig(id string) (*store.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.configs[id]
	return sc, ok
}

// credentials 解密一个会话的凭据（密码 / 私钥 / 口令）。
//
// **不做任何静默降级**：解密失败就把错误带出去（契约第 3 节）——静默当成空密码的表现是
// 「密码莫名其妙失效了」，而真实原因（master.key 换了 / 密文被搬过）永远查不出来。
func (m *sessionManager) credentials(sc *store.Session) (password, privateKey, passphrase string, apiErr *apierr.Error) {
	var err error
	if password, err = m.crypt.Decrypt(secret.FieldPassword, sc.ID, sc.Password); err != nil {
		return "", "", "", apierr.Internal("会话 %s 的密码解密失败：%v", sc.ID, err)
	}
	if privateKey, err = m.crypt.Decrypt(secret.FieldPrivateKey, sc.ID, sc.PrivateKey); err != nil {
		return "", "", "", apierr.Internal("会话 %s 的私钥解密失败：%v", sc.ID, err)
	}
	if passphrase, err = m.crypt.Decrypt(secret.FieldPassphrase, sc.ID, sc.Passphrase); err != nil {
		return "", "", "", apierr.Internal("会话 %s 的私钥口令解密失败：%v", sc.ID, err)
	}
	return password, privateKey, passphrase, nil
}

// dialRequestOf 组装一次拨号所需的输入（凭据按需取：落盘解密或内存缓存，见 dialCredential）。
func (m *sessionManager) dialRequestOf(sc *store.Session) (dialRequest, *apierr.Error) {
	mc, apiErr := m.dialCredential(sc)
	if apiErr != nil {
		return dialRequest{}, apiErr
	}
	return dialRequest{cfg: sc, password: mc.password, privateKey: mc.privateKey, passphrase: mc.passphrase}, nil
}

// validateSessionFields 校验会话的身份字段（校验通过后返回规整化的副本）。
//
// 只负责「校验一组已经定下来的值」；「请求里没给的字段该用什么值」由 resolveSessionFields
// 决定（见那里的说明：更新语义下空值一律保留原值）。
func validateSessionFields(host string, port int, user, authType string) (string, string, int, *apierr.Error) {
	host = strings.TrimSpace(host)
	user = strings.TrimSpace(user)
	if host == "" {
		return "", "", 0, apierr.Invalid("缺少地址")
	}
	// host 会被拼成 host:port 交给 net.Dial，含分隔符 / 空白的值只会让人看不懂错误。
	if strings.ContainsAny(host, " \t\r\n/") {
		return "", "", 0, apierr.Invalid("地址不能包含空白或斜杠：%q", host)
	}
	if user == "" {
		return "", "", 0, apierr.Invalid("缺少登录名")
	}
	if strings.ContainsAny(user, " \t\r\n@") {
		return "", "", 0, apierr.Invalid("登录名不能包含空白或 @：%q", user)
	}
	if authType != authTypePassword && authType != authTypeKey {
		return "", "", 0, apierr.Invalid("认证方式只能是 password 或 key：%q", authType)
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return "", "", 0, apierr.Invalid("端口需在 1–65535 之间：%d", port)
	}
	return host, user, port, nil
}

// resolveSessionFields 按**部分更新**语义把请求里的身份字段合并到现有配置上，再统一校验。
//
// 规则（cur == nil 表示新建）：
//   - 新建：host/user/authType 必填，port 缺省 = 22；
//   - 更新：空串 / 0 / 缺省的字段**一律保留原值**，只有显式给了新值才算改。
//
// 为什么「0 必须当成保留原值」——这是一个真踩过的坑：`port` 的零值就是 0，早期实现把它
// 归一成 22 之后才与现值比较身份，于是「改名时不带 port」被判成「端口从 2222 改成了 22」，
// 已打开的会话直接回 409「请先关闭再修改地址 / 端口 / 登录名」；端口恰好是 22 的会话会蒙对。
// 前端为此被迫每次都发全字段，但这对下一个调用方（脚本 / curl / 新界面）仍然是个陷阱，
// 所以在后端修掉。
//
// host/user/authType 同理：更新时它们为空只能说明「调用方不想改这一项」，不能当成
// 「把它改成空」——那会让用户丢掉一个本来能连的会话配置。
func resolveSessionFields(cur *store.Session, in store.SessionInput) (host, user string, port int, authType string, apiErr *apierr.Error) {
	host = strings.TrimSpace(in.Host)
	user = strings.TrimSpace(in.User)
	port = in.Port
	authType = strings.TrimSpace(in.AuthType)
	if cur != nil {
		if host == "" {
			host = cur.Host
		}
		if user == "" {
			user = cur.User
		}
		if port == 0 {
			port = cur.Port
		}
		if authType == "" {
			authType = cur.AuthType
		}
	}
	host, user, port, apiErr = validateSessionFields(host, port, user, authType)
	return host, user, port, authType, apiErr
}

// save 新增或更新会话配置（/api/sessions/save）。
func (m *sessionManager) save(in store.SessionInput) (*store.SessionView, *apierr.Error) {
	if in.ID != "" && !store.ValidID(in.ID) {
		return nil, apierr.NotFound("会话不存在")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().Format(time.RFC3339)
	created := in.ID == ""
	var cur *store.Session
	if !created {
		found, ok := m.configs[in.ID]
		if !ok {
			return nil, apierr.NotFound("会话不存在：%s", in.ID)
		}
		cur = found
	}
	// 先按「部分更新」语义把请求字段合并到现有配置上再校验：这样「没发 port」不会被当成
	// 「改成 22」，身份比较与写入用的都是合并后的真实值（见 resolveSessionFields）。
	host, user, port, authType, apiErr := resolveSessionFields(cur, in)
	if apiErr != nil {
		return nil, apiErr
	}

	// 记下改动前的状态：「内存缓存怎么清」要用到「原来保存不保存凭据」「认证方式换没换」
	// 这两个旧值，而下面会把它们覆盖掉。
	//
	// ⚠️ **必须在 sc 被赋值之前读**：sc 是 cur 的副本，`sc.Host, ..., sc.AuthType = ...`
	// 一旦执行，prevAuthType 就恒等于新值、`prevAuthType != sc.AuthType` 永远为假——
	// 「会话不保存凭据 + 改了认证方式」的内存凭据就永远清不掉，下一次 open 会拿着类型
	// 不匹配的旧凭据报出一个看不懂的 no_credential。
	wasSaved := cur != nil && cur.CredentialsSaved()
	prevAuthType := ""
	if cur != nil {
		prevAuthType = cur.AuthType
	}

	var sc *store.Session
	if cur != nil {
		// 复制一份再改：失败路径不污染内存态，成功路径换成一个新对象（见 configs 的注释）。
		cp := *cur
		sc = &cp
		// 身份（user@host:port）是「已打开会话」的连接目标：会话开着的时候改地址/登录名，
		// 运行时还连着旧机器，界面上的卡片却写着新地址——这种不一致必须挡住。
		identityChanged := cur.Host != host || cur.Port != port || cur.User != user
		if identityChanged {
			if _, isOpen := m.runtimes[in.ID]; isOpen {
				return nil, apierr.Conflict("会话已打开，请先关闭再修改地址 / 端口 / 登录名")
			}
		}
		sc.Host, sc.User, sc.Port, sc.AuthType = host, user, port, authType
	} else {
		sc = &store.Session{
			Version:   1,
			ID:        store.NewID(),
			Host:      host,
			User:      user,
			Port:      port,
			AuthType:  authType,
			CreatedAt: now,
		}
	}

	// 会话名称：**显式发空串 = 清除自定义名**（展示时由 displayName / 前端 sessionLabel
	// 退回 `user@host`）。字段缺省 = 保持原值。
	//
	// 原来那版把「空串」也当成「保持原值」，于是用户把名称清空后保存，界面上还是旧名字
	// （第六轮用户报的「清空失败」）。凭据与备注早就是「显式空串 = 清除」的语义，名字不该例外。
	if in.Name != nil {
		sc.Name = strings.TrimSpace(*in.Name)
	} else if created {
		// 新建且没给名字：落盘时就存一份 `user@host`，配置文件自解释
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
		if other.Identity() == sc.Identity() {
			return nil, apierr.Duplicate("已存在相同「地址 + 登录名」的会话「%s」", other.DisplayName())
		}
	}

	// 凭据：nil = 不修改，"" = 清除，其余 = 加密后覆盖。
	//
	// saveCred 决定这批凭据是**落盘**还是**只留在内存**（契约 11.1）：
	//   - 新建时缺省 = true（与第一轮一致）；
	//   - 更新时缺省 = 保持这个会话原来的设置（见 store.SessionInput.SaveCredential 的说明）；
	//   - 关掉开关（false）时把三个字段连同私钥指纹一并清掉：私钥都丢了，留着由它算出来的
	//     指纹只会让界面显示一把已经不存在的密钥。
	saveCred := true
	switch {
	case in.SaveCredential != nil:
		saveCred = *in.SaveCredential
	case !created:
		saveCred = sc.CredentialsSaved()
	}
	// 从「不保存」切到「保存」时，本次请求必须带凭据（见下面 cache 同步之后的检查）。
	planningToStore := saveCred && !created && !wasSaved

	if !saveCred {
		// 只做**校验**，不做任何持久化：带的私钥解析不了要当场报错，否则用户会得到一个
		// 「配置存下了、打开时报 key_parse」的会话，而那次保存明明什么都没存。
		if in.PrivateKey != nil && strings.TrimSpace(*in.PrivateKey) != "" {
			if _, err := parsePrivateKey(*in.PrivateKey, derefRaw(in.Passphrase)); err != nil {
				return nil, apierr.KeyParse("私钥解析失败：%v", err)
			}
		}
		sc.Password, sc.PrivateKey, sc.Passphrase, sc.KeyFingerprint = nil, nil, nil, ""
		sc.CredentialSaved = boolPtr(false)
	} else {
		if in.Password != nil {
			sec, err := m.crypt.Encrypt(secret.FieldPassword, sc.ID, *in.Password)
			if err != nil {
				return nil, apierr.Internal("加密密码失败：%v", err)
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
					return nil, apierr.KeyParse("私钥解析失败：%v", fpErr)
				}
				sec, err := m.crypt.Encrypt(secret.FieldPrivateKey, sc.ID, pem)
				if err != nil {
					return nil, apierr.Internal("加密私钥失败：%v", err)
				}
				sc.PrivateKey = sec
				sc.KeyFingerprint = fp
			}
		}
		if in.Passphrase != nil {
			sec, err := m.crypt.Encrypt(secret.FieldPassphrase, sc.ID, *in.Passphrase)
			if err != nil {
				return nil, apierr.Internal("加密私钥口令失败：%v", err)
			}
			sc.Passphrase = sec
		}
		sc.CredentialSaved = nil
	}

	// **新建**会话必须给凭据：否则用户会存下一个「打开就报无凭据」的卡片，
	// 而卡片上完全看不出缺什么。
	// 更新时允许把凭据清空（契约第 4.3 节：空串 = 清除该字段）：那是有意的操作，
	// 缺凭据会在连接测试 / 打开会话时给出明确的 no_credential，而不是静默用一个空密码去连。
	//
	// 例外：**不保存凭据**的新会话不要求当场填凭据——它本来就不存，打开时会被
	// credential_required 要求输入（这正是这个开关的用途）。表单里填了的那份只用于当场校验。
	if created && saveCred {
		if sc.AuthType == authTypePassword && sc.Password == nil {
			return nil, apierr.NoCredential("密码认证需要填写登录密码")
		}
		if sc.AuthType == authTypeKey && sc.PrivateKey == nil {
			return nil, apierr.NoCredential("密钥认证需要粘贴私钥内容")
		}
	}
	// 从「不保存」切到「保存」：必须在这一刻给出凭据。否则会存下一个声称保存了凭据、
	// 实际一个凭据都没有的会话——下次 open 报的是 no_credential（400）、而不是
	// credential_required（409），前端的凭据弹窗根本不会弹，用户只看到「没有可用凭据」。
	if planningToStore && sc.Password == nil && sc.PrivateKey == nil {
		return nil, apierr.NoCredential("这个会话当前不保存凭据，开启「保存凭据」时需要同时填写%s", credentialKindName(sc.AuthType))
	}

	// 内存凭据缓存与落盘状态保持同步（见 credentials.go 的纪律 3）。
	//
	// 关键取舍：**保存表单里填的凭据不进内存缓存**（除非该会话正开着）。凭据只由一次成功的
	// open 留下（见 open 的注释），也只有它算「验证过、可以用」。否则用户刚建好一个
	// 「不保存凭据」的会话、随手填了个密码保存，卡片看起来是「每次都要输入」的那种，
	// 点开却直接连上了——与这个开关承诺的行为对不上。
	openNow := false
	if _, isOpen := m.runtimes[sc.ID]; isOpen {
		openNow = true
	}
	switch {
	case saveCred:
		// 凭据已经（或改为）落盘：内存里那份多余，留着只会两份不一致。
		m.dropCachedCredentialLocked(sc.ID)
	case in.Password != nil || in.PrivateKey != nil:
		mc := memoryCredential{
			password:   derefRaw(in.Password),
			privateKey: derefRaw(in.PrivateKey),
			passphrase: derefRaw(in.Passphrase),
		}
		switch {
		case !openNow:
			m.dropCachedCredentialLocked(sc.ID)
		case mc.hasAuthCredential(sc.AuthType):
			// 会话正开着：它接下来还要开新终端 / 重连 SFTP，用的必须是用户刚给的这份
			// （旧的那份可能已经过期或被改过）。
			m.setCachedCredentialLocked(sc.ID, mc)
		case prevAuthType != sc.AuthType:
			// 认证方式变了、给的凭据又不匹配新方式：旧缓存类型同样不匹配，留着只会
			// 让下一次拨号报出与原因不符的 no_credential。
			m.dropCachedCredentialLocked(sc.ID)
		default:
			// 会话开着、认证方式没变、这次给的凭据是空的（API 调用方传了空串）：
			// 不要用空值覆盖内存里那份能用的——否则用户只是保存了一次表单，新终端就
			// 全部报 no_credential。前端表单不会发空串（留空 = 不修改）。
		}
	case !created && openNow && cur != nil && cur.CredentialsSaved() && prevAuthType == sc.AuthType:
		// 会话**正开着**时把「保存凭据」从开切到关：sc 里的密文刚被清掉，但当前这条会话
		// 还要用它开新终端 / 在 SFTP 断线后重新拨号。把凭据放进内存缓存（只活到 close，
		// 与 11.1「不保存 = 只在内存里」一致）。
		//
		// 不这样做的话，用户只是关掉了一个持久化开关，回到会话里新建终端却全部报
		// no_credential（前端还会把它显示成「终端不存在」）——「关掉保存」不等于
		// 「中止当前会话」，这两个动作必须分开。
		//
		// 优先保留 memCreds 里已有的那份：它可能是这次 open 时用户**当场输入并验证成功**的
		// （open 对带凭据的请求会写缓存，即使配置里也有旧密文），比旧配置里的更可信。
		// 只有内存里没有时才解密旧配置。
		//
		// 用户在同一请求里给了新凭据时走上面那个 case（新凭据优先）；认证方式变了则不
		// carry over（类型不匹配的缓存会让下一次拨号报出与原因不符的 no_credential）。
		// 旧密文解不开时不 carry over、但保存照常完成，见下面的分支。
		if _, ok := m.memCreds[sc.ID]; !ok {
			mc, credErr := m.storedCredential(cur)
			if credErr != nil {
				// 旧密文解不开（master.key 换过、密文损坏）：**照常完成这次保存**，只是不
				// carry over ——本次保存的目的正是把那份密文清掉，因为「读不出一个马上就要
				// 被丢弃的字段」而返回 500，等于把用户锁死在死角里：开关关不掉，这个会话之后
				// 每次 open 也都会在同一处解密失败（前端只能拿到一句 internal），唯一的出路
				// 是删掉会话重建。
				//
				// 代价说清楚：内存里没有可用的凭据，新建子终端会报 no_credential。这不是静默
				// 失败——终端里那句提示明确要求「重新打开会话以重新输入凭据」，日志里也留了
				// 一条可定位的记录。**不要**把它降级成「空密码去连」：那会把一个数据损坏变成
				// 一个认证失败，原因彻底丢掉。
				logx.Warnf("会话 %s 的旧凭据解密失败，跳过 carry over（本次保存会清掉落盘的那份）：%s",
					sc.ID, credErr.Message)
			} else {
				m.setCachedCredentialLocked(sc.ID, mc)
			}
		}
	case created || prevAuthType != sc.AuthType:
		// 认证方式变了 → 缓存的凭据类型已经不匹配（旧的是密码、新的是私钥），留着只会
		// 让下一次拨号报出一个看不懂的 no_credential。
		m.dropCachedCredentialLocked(sc.ID)
	}

	sc.UpdatedAt = now
	if err := m.st.SaveSession(sc); err != nil {
		return nil, apierr.Internal("保存会话失败：%v", err)
	}
	m.configs[sc.ID] = sc
	view := sc.View()
	return &view, nil
}

// boolPtr 取一个 bool 的地址（CredentialSaved 用指针表达「缺省 = true」）。
func boolPtr(v bool) *bool { return &v }

// delete 删除会话：先关掉运行时（终端 + SFTP），再删配置，**最后**删专属快捷指令、快捷访问与
// 历史目录（前端的确认文案就是这么承诺的：「登录数据、专属快捷指令与该会话的历史都会一并删除」；
// 配置必须排在附属数据之前，理由见函数内注释）。
func (m *sessionManager) delete(id string) *apierr.Error {
	if !store.ValidID(id) {
		return apierr.NotFound("会话不存在")
	}
	m.mu.Lock()
	if _, ok := m.configs[id]; !ok {
		m.mu.Unlock()
		return apierr.NotFound("会话不存在：%s", id)
	}
	delete(m.configs, id)
	delete(m.memCreds, id)
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	m.mu.Unlock()

	if rt != nil {
		rt.close()
	}
	// **先删配置，成功之后再删附属数据**：反过来的顺序（先删历史/快捷指令/快捷访问、再删配置）
	// 在最后一步失败时会留下「会话还在、附属表已经没了」的半删状态——用户看到的会话点开就少了
	// 指令与历史，而且没有任何入口能把它恢复。配置先删掉之后，附属数据即使删不掉也只是孤儿文件
	// （同名会话下次被删时会再碰一次，启动时也会扫到），不产生能观察到的坏状态，所以失败只记日志。
	if err := m.st.DeleteSession(id); err != nil {
		return apierr.Internal("删除会话失败：%v", err)
	}
	// 历史可能还在（会话没打开过、或上次没清理干净）
	m.st.RemoveHistoryDir(id)
	m.st.DeleteQuickCmds(id)
	// 快捷访问的路径属于这台远程主机：会话没了，它们也就没有意义了（列表里的路径会在
	// 别的会话上指向完全不同的目录，留着只会是一条条点了报错的项）。
	m.st.DeleteQuickAccess(id)
	return nil
}

// trust 记录一条用户确认过的主机密钥（/api/sessions/trust）。
//
// 请求体里只有指纹（前端手上只有指纹），完整公钥来自测试连接时暂存的扫描结果；
// 指纹对不上就拒绝——「随手点信任」不该放行一把没确认过的密钥。
func (m *sessionManager) trust(host string, port int, fingerprint string) *apierr.Error {
	host = strings.TrimSpace(host)
	fingerprint = strings.TrimSpace(fingerprint)
	if host == "" {
		return apierr.Invalid("缺少 host")
	}
	if port == 0 {
		port = 22
	}
	if port < 1 || port > 65535 {
		return apierr.Invalid("端口需在 1–65535 之间：%d", port)
	}
	if fingerprint == "" {
		return apierr.Invalid("缺少 fingerprint")
	}
	id := hostKeyID(host, port)
	rec, ok := m.pending.take(id, fingerprint)
	if !ok {
		return apierr.NotFound("没有待确认的主机密钥（%s %s）：请先重新测试连接，再点「信任并继续」", id, fingerprint)
	}
	rec.AddedAt = time.Now().Format(time.RFC3339)
	if err := m.hks.add(id, rec); err != nil {
		return apierr.Internal("写入 known_hosts 失败：%v", err)
	}
	return nil
}

// test 执行一次连接测试（不建立持久会话）。
//
// 失败**不是** HTTP 错误：结果放在 TestOutcome 里（success=false + code），前端据此渲染
// 人话（「认证失败（登录名 / 密码 / 密钥不对）」）。只有参数本身不合法才回 4xx。
func (m *sessionManager) test(ctx context.Context, in store.SessionInput) (*testOutcome, *apierr.Error) {
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
func (m *sessionManager) dialRequestForTest(in store.SessionInput) (dialRequest, *apierr.Error) {
	var stored *store.Session
	if in.ID != "" {
		stored, _ = m.getConfig(in.ID)
	}
	// 与 save 用同一套「部分更新」语义：编辑已有会话时表单没发的字段（例如 port）按配置
	// 里的原值测，而不是按零值归一后的 22——否则「测试连接」会去连一个根本不是这个会话的
	// 端口，然后报一句让人摸不着头脑的 dial_refused。
	host, user, port, authType, apiErr := resolveSessionFields(stored, in)
	if apiErr != nil {
		return dialRequest{}, apiErr
	}
	sc := &store.Session{ID: in.ID, Host: host, User: user, Port: port, AuthType: authType}
	if !store.ValidID(in.ID) {
		// 新建会话（还没有 id）：凭据只能来自请求体。
		sc.ID = "pending"
	}
	password := derefString(in.Password)
	privateKey := derefString(in.PrivateKey)
	passphrase := derefString(in.Passphrase)
	if stored != nil {
		mc, apiErr := m.storedOrCachedCredential(stored)
		if apiErr != nil {
			return dialRequest{}, apiErr
		}
		if in.Password == nil {
			password = mc.password
		}
		if in.PrivateKey == nil {
			privateKey = mc.privateKey
			// 私钥沿用已存的那份时，口令也一起沿用（新私钥配旧口令几乎必然解析失败）。
			if in.Passphrase == nil {
				passphrase = mc.passphrase
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
func errFromConnect(cerr *connectError) *apierr.Error {
	status := 502
	switch cerr.Code {
	case codeHostKeyUnknown, codeHostKeyMismatch:
		status = 409
	case apierr.CodeNoCredential, apierr.CodeKeyParse:
		status = 400
	}
	return &apierr.Error{Code: cerr.Code, Status: status, Message: cerr.Detail}
}

// sortViews / sortStates 只是为了让响应稳定：map 的遍历顺序是随机的，不稳定的话
// 测试断言与人工排查都要靠猜。前端自己会按时序排卡片。
func sortViews(items []store.SessionView) {
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
func errNotOpen(format string, args ...any) *apierr.Error {
	return apierr.Fail(409, "not_open", format, args...)
}

func errSessionGone(format string, args ...any) *apierr.Error {
	return apierr.Fail(404, "session_gone", format, args...)
}
