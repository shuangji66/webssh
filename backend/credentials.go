package main

import (
	"strings"
	"webssh/backend/internal/apierr"
	"webssh/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 「不保存凭据」的会话：明文凭据只活在进程内存里（契约 11.1）
//
// saveCredential=false 的会话不把 password / privateKey / passphrase 写进
// sessions/<sid>.json；打开时由用户在请求体里当场提供，拨号成功后留在**内存**里，供该会话
// 后续的终端 / SFTP / 远程命令复用。会话 close 或进程退出即失效——这就是「不保存时每次连接
// 都要输入」的落地方式。
//
// 三条纪律：
//  1. 明文凭据**绝不落盘、绝不进日志、绝不进任何 API 响应**（日志只记失败原因，凭据本身
//     在这份代码里没有任何一条格式化路径）。
//  2. 内存缓存按会话 id 存，且**只在「该会话没有落盘凭据」时被读取**：落盘的那份才是配置的
//     真相，两份同时存在时以落盘的为准，避免「配置里的密码换了、新终端还在用旧密码」。
//  3. 会话 close / 删除 / 进程退出都让缓存失效；保存配置时按下面的规则同步，否则会出现
//     「内存里是旧密码、卡片上写的是新密码」这种看不见的错乱。
// ---------------------------------------------------------------------------

// memoryCredential 是一个会话在内存里的明文凭据。
//
// 刻意用值类型 + 一个方法集很小的结构：它会被复制进 dialRequest，不共享可变状态。
// 注意它**没有 String() 方法**，也不实现 fmt.Stringer——那正是「一不小心就把密码打进
// 日志」的入口（`logx.Warnf("%v", cred)`）。
type memoryCredential struct {
	password   string
	privateKey string
	passphrase string
}

// hasAuthCredential 判断这份凭据够不够按 authType 认证。
//
// 私钥做 trim 判定（粘贴来的 PEM 常常带首尾空白，ssh.ParsePrivateKey 也接受），密码**不做
// trim**：密码里的空格是密码的一部分，trim 掉会表现为「密码莫名不对」。
func (c memoryCredential) hasAuthCredential(authType string) bool {
	if authType == authTypeKey {
		return strings.TrimSpace(c.privateKey) != ""
	}
	return c.password != ""
}

// openCredentials 是 /api/sessions/open 请求体里可选的明文凭据（契约 11.5）。
type openCredentials struct {
	Password   *string `json:"password"`
	PrivateKey *string `json:"privateKey"`
	Passphrase *string `json:"passphrase"`
}

// provided 报告请求里到底带没带凭据。用指针判空而不是判空串：带一个空串是「我明确给你一个
// 空密码」，它同样不构成可用凭据，但语义上属于「带了」（会走 provided 分支并报
// credential_required），而不是「没带」。
func (c openCredentials) provided() bool {
	return c.Password != nil || c.PrivateKey != nil || c.Passphrase != nil
}

// credential 把请求里的凭据转成内存形态。
func (c openCredentials) credential() memoryCredential {
	return memoryCredential{
		password:   derefRaw(c.Password),
		privateKey: derefRaw(c.PrivateKey),
		passphrase: derefRaw(c.Passphrase),
	}
}

// derefRaw 解引用一个可空字符串，**不做 trim**（见 hasAuthCredential 的说明）。
func derefRaw(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------------------------------------------------------------------------
// 内存凭据缓存（挂在 sessionManager 上）
// ---------------------------------------------------------------------------

// cachedCredential 取该会话在内存里的凭据。
func (m *sessionManager) cachedCredential(sid string) (memoryCredential, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mc, ok := m.memCreds[sid]
	if !ok {
		return memoryCredential{}, false
	}
	return *mc, true
}

func (m *sessionManager) setCachedCredential(sid string, mc memoryCredential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCachedCredentialLocked(sid, mc)
}

// setCachedCredentialLocked 的调用方必须持有 m.mu（save 就在锁里改缓存）。
func (m *sessionManager) setCachedCredentialLocked(sid string, mc memoryCredential) {
	if m.memCreds == nil {
		m.memCreds = map[string]*memoryCredential{}
	}
	cp := mc
	m.memCreds[sid] = &cp
}

// dropCachedCredential 忘掉一个会话的内存凭据（close / 删除 / 凭据改为落盘时调用）。
func (m *sessionManager) dropCachedCredential(sid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropCachedCredentialLocked(sid)
}

// dropCachedCredentialLocked 的调用方必须持有 m.mu。
func (m *sessionManager) dropCachedCredentialLocked(sid string) {
	delete(m.memCreds, sid)
}

// dropAllCachedCredentials 在进程退出时清空（进程都不在了，内存里的东西自然作废；
// 显式清掉是为了让测试能验证「重启后必须重新输入」）。
func (m *sessionManager) dropAllCachedCredentials() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memCreds = map[string]*memoryCredential{}
}

// storedCredential 解密落盘的那份凭据。
func (m *sessionManager) storedCredential(sc *store.Session) (memoryCredential, *apierr.Error) {
	p, k, pp, apiErr := m.credentials(sc)
	if apiErr != nil {
		return memoryCredential{}, apiErr
	}
	return memoryCredential{password: p, privateKey: k, passphrase: pp}, nil
}

// resolveCredential 决定一次 open 该用哪份凭据。
//
// 优先级：请求里的（用户当场输入，最权威）→ 落盘的 → 内存缓存 → 都没有就
// `credential_required`（409）。最后这一步正是「不保存凭据时必须每次输入」的判据：
// 前端拿到它就去弹凭据输入框，填完再 open 一次。
func (m *sessionManager) resolveCredential(sc *store.Session, req openCredentials) (memoryCredential, *apierr.Error) {
	if req.provided() {
		mc := req.credential()
		if !mc.hasAuthCredential(sc.AuthType) {
			return memoryCredential{}, apierr.CredentialRequired("请求里带了凭据，但缺少该会话认证方式（%s）需要的那一项", sc.AuthType)
		}
		return mc, nil
	}
	if sc.CredentialsSaved() {
		return m.storedCredential(sc)
	}
	if mc, ok := m.cachedCredential(sc.ID); ok {
		return mc, nil
	}
	return memoryCredential{}, apierr.CredentialRequired("会话「%s」没有保存凭据，请在打开时输入%s", sc.Name, credentialKindName(sc.AuthType))
}

// dialCredential 给「终端 / SFTP / 远程命令」用的凭据解析：没有请求体，凭据只能来自
// 落盘或内存缓存。
//
// 与 resolveCredential 的差别只有一点：这里不会产生 credential_required（那个码是给
// open 用的），拿不到凭据就报 no_credential —— 与「会话存了密码但字段被清空」是同一条路径。
func (m *sessionManager) dialCredential(sc *store.Session) (memoryCredential, *apierr.Error) {
	if sc.CredentialsSaved() {
		return m.storedCredential(sc)
	}
	if mc, ok := m.cachedCredential(sc.ID); ok {
		return mc, nil
	}
	return memoryCredential{}, apierr.NoCredential("会话「%s」没有可用的凭据（它不保存凭据，且本进程内还没有人为它输入过）", sc.Name)
}

// storedOrCachedCredential 给「没有请求体」的场景取一份凭据：先落盘、再内存缓存。
//
// 用在连接测试：编辑一个「不保存凭据」的会话时，表单里不会重填凭据（没有可保留的东西），
// 但会话可能正开着 / 刚开过，内存里有那份凭据——拿它去测比报「没有可用凭据」有用得多。
// 什么都没有时返回空凭据，由调用方按连接失败处理（success=false + no_credential），
// 而不是在这里报 HTTP 错误（连接测试的失败是结果、不是接口错误）。
func (m *sessionManager) storedOrCachedCredential(sc *store.Session) (memoryCredential, *apierr.Error) {
	if sc.CredentialsSaved() {
		return m.storedCredential(sc)
	}
	if mc, ok := m.cachedCredential(sc.ID); ok {
		return mc, nil
	}
	return memoryCredential{}, nil
}

// credentialKindName 给人看的凭据种类（错误文案里用）。
func credentialKindName(authType string) string {
	if authType == authTypeKey {
		return "私钥"
	}
	return "密码"
}
