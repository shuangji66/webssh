package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------------------------------------------------------------------------
// SSH 拨号
//
// 认证方式组装、主机密钥校验（TOFU，见 hostkeys.go）、超时与错误归类都在这里。
// 连接类失败统一用 connectError 表达：裸 code（前端的 err_<code> 文案）+ 阶段 +
// 含技术细节的中文 detail。**不做静默降级**：没有凭据就说 no_credential，
// 私钥解不开就说 key_parse，绝不「用空密码再试一次」。
// ---------------------------------------------------------------------------

// 连接类裸码（契约第 4.1 节）。
const (
	codeNoCredential    = "no_credential"
	codeKeyParse        = "key_parse"
	codeDNS             = "dns"
	codeDialTimeout     = "dial_timeout"
	codeDialRefused     = "dial_refused"
	codeDial            = "dial"
	codeHandshake       = "handshake"
	codeHostKeyUnknown  = "hostkey_unknown"
	codeHostKeyMismatch = "hostkey_mismatch"
	codeAuthFailed      = "auth_failed"
	codeAuthUnsupported = "auth_unsupported"
	codeShellFailed     = "shell_failed"
	codeSFTPUnavailable = "sftp_unavailable"
)

// 连接阶段（TestOutcome.stage，前端 TestStage 的取值）。
const (
	stageOK        = "ok"
	stageResolve   = "resolve"
	stageDial      = "dial"
	stageHandshake = "handshake"
	stageHostKey   = "hostkey"
	stageAuth      = "auth"
	stageShell     = "shell"
)

// connectError 是一次连接失败的完整描述。
type connectError struct {
	Code        string
	Stage       string
	Detail      string
	Fingerprint string
	KeyType     string
	Trusted     bool
	Mismatch    bool
	Banner      string
	LatencyMs   int
}

func (e *connectError) Error() string { return e.Detail }

// testOutcome 是 /api/sessions/test 的响应里的 result（契约第 4.3 节 TestOutcome）。
type testOutcome struct {
	Success     bool   `json:"success"`
	Code        string `json:"code"`
	Detail      string `json:"detail,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	KeyType     string `json:"keyType,omitempty"`
	Trusted     bool   `json:"trusted,omitempty"`
	Mismatch    bool   `json:"mismatch,omitempty"`
	Banner      string `json:"banner,omitempty"`
	LatencyMs   int    `json:"latencyMs"`
}

// dialRequest 是一次拨号的全部输入。
type dialRequest struct {
	cfg        *sessionConfig
	password   string
	privateKey string
	passphrase string
}

// dialResult 是成功建连的结果。
type dialResult struct {
	client  *ssh.Client
	banner  string
	latency time.Duration
	record  hostKeyRecord
	trusted bool
}

// buildAuthMethods 按会话的 authType 组装认证方式。
//
// 只使用用户选定的那一种（authType 是 password 就只发密码）：OpenSSH 的默认顺序
// 会在密码错时回退到 None 等其它方式，把「密码错了」变成「服务器拒绝所有方式」，
// 用户看到的原因跟真实原因对不上。
func buildAuthMethods(req dialRequest) ([]ssh.AuthMethod, *connectError) {
	switch req.cfg.AuthType {
	case authTypeKey:
		if req.privateKey == "" {
			return nil, &connectError{Code: codeNoCredential, Stage: stageAuth, Detail: "该会话使用密钥认证，但没有可用的私钥"}
		}
		signer, err := parsePrivateKey(req.privateKey, req.passphrase)
		if err != nil {
			return nil, &connectError{Code: codeKeyParse, Stage: stageAuth, Detail: "私钥解析失败：" + err.Error()}
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case authTypePassword:
		if req.password == "" {
			return nil, &connectError{Code: codeNoCredential, Stage: stageAuth, Detail: "该会话使用密码认证，但没有可用的密码"}
		}
		return []ssh.AuthMethod{ssh.Password(req.password)}, nil
	default:
		return nil, &connectError{Code: codeNoCredential, Stage: stageAuth, Detail: fmt.Sprintf("未知的认证方式 %q", req.cfg.AuthType)}
	}
}

// parsePrivateKey 解析私钥。
//
// 顺序是刻意的：**先当未加密的试一次**。用户把明文私钥粘进来、又顺手在「私钥口令」里
// 填了个其实用不到的串时，直接按带口令解析会得到 "ssh: key is not password protected"，
// 表现就是「明明是明文私钥却存不进去/连不上」。明文解析成功就用它；失败（真的是加密私钥）
// 再带口令解析，这样两种输入都能work，错误信息也不会被稀释。
func parsePrivateKey(pem, passphrase string) (ssh.Signer, error) {
	signer, plainErr := ssh.ParsePrivateKey([]byte(pem))
	if plainErr == nil {
		return signer, nil
	}
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase([]byte(pem), []byte(passphrase))
	}
	return nil, plainErr
}

// keyFingerprint 计算私钥对应公钥的 SHA256 指纹（SessionView.keyFingerprint，展示用）。
// 解析不了返回空串 + 错误，由调用方决定是保存失败还是仅放弃指纹。
func keyFingerprint(pem, passphrase string) (string, error) {
	signer, err := parsePrivateKey(pem, passphrase)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// dial 建立一条 SSH 连接（TCP → 握手 → 主机密钥校验 → 认证）。
//
// 刻意**不用 ssh.Dial**：那样拿不到「主机密钥是未知还是被改了」的信息——x/crypto 会把
// 回调里的错误包成 "ssh: handshake failed: …"，指纹与密钥类型全丢了。这里自己拨 TCP
// 再 NewClientConn，主键回调把结果写到闭包变量里，失败时按它构造带指纹的错误。
func (m *sessionManager) dial(ctx context.Context, req dialRequest) (*dialResult, *connectError) {
	hp := hostKeyID(req.cfg.Host, req.cfg.Port)
	started := time.Now()

	auths, cerr := buildAuthMethods(req)
	if cerr != nil {
		cerr.LatencyMs = msSince(started)
		return nil, cerr
	}

	// 主机密钥回调把本次看到的主机密钥与判定结果留在闭包里（见函数注释）。
	var hkFailure *connectError
	var hkRecord hostKeyRecord
	trusted := false
	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		hkRecord = recordOf(key)
		if known, ok := m.hks.lookup(hp); ok {
			if known.Fingerprint == hkRecord.Fingerprint && known.PublicKey == hkRecord.PublicKey {
				trusted = true
				return nil
			}
			hkFailure = &connectError{
				Code:        codeHostKeyMismatch,
				Stage:       stageHostKey,
				Detail:      fmt.Sprintf("主机 %s 的密钥与已记录的不一致（记录 %s %s，本次 %s %s）。可能是服务器重装过，也可能是中间人。确认无误后请在「已信任的主机」里移除旧记录再重连。", hp, known.KeyType, known.Fingerprint, hkRecord.KeyType, hkRecord.Fingerprint),
				Fingerprint: hkRecord.Fingerprint,
				KeyType:     hkRecord.KeyType,
				Mismatch:    true,
			}
			return errors.New("hostkey_mismatch")
		}
		// 未知主机：暂存这次扫到的公钥，等用户确认指纹后再写入 known_hosts
		// （/api/sessions/trust 的请求体里只有指纹，没有公钥）。
		m.pending.put(hp, hkRecord)
		hkFailure = &connectError{
			Code:        codeHostKeyUnknown,
			Stage:       stageHostKey,
			Detail:      fmt.Sprintf("主机 %s 的密钥尚未信任（%s %s）。确认指纹无误后点「信任并继续」。", hp, hkRecord.KeyType, hkRecord.Fingerprint),
			Fingerprint: hkRecord.Fingerprint,
			KeyType:     hkRecord.KeyType,
		}
		return errors.New("hostkey_unknown")
	}

	var banner string
	sshCfg := &ssh.ClientConfig{
		User:            req.cfg.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback,
		BannerCallback: func(message string) error {
			banner = strings.TrimRight(message, "\r\n")
			return nil
		},
		Timeout: m.renv.DialTimeout,
	}

	// 1) TCP 连接：DialContext + 明确的超时，客户端取消（关页面）时立刻退出。
	dialer := net.Dialer{Timeout: m.renv.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", hp)
	if err != nil {
		cerr := classifyDialError(err)
		cerr.LatencyMs = msSince(started)
		return nil, cerr
	}

	// 2) 握手：Conn 上的一次 deadline 覆盖「TCP 已连上但对端不回包」的情况
	//    （防火墙丢包、非 SSH 服务占着 22 端口）。握手完必须清掉 deadline，
	//    否则这条长连接会在 DialTimeout 之后被自己掐断。
	_ = conn.SetDeadline(time.Now().Add(m.renv.DialTimeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, hp, sshCfg)
	if err != nil {
		_ = conn.Close()
		cerr := hkFailure
		if cerr == nil {
			cerr = classifyHandshakeError(err)
		}
		cerr.Fingerprint = firstNonEmpty(cerr.Fingerprint, hkRecord.Fingerprint)
		cerr.KeyType = firstNonEmpty(cerr.KeyType, hkRecord.KeyType)
		cerr.Trusted = trusted
		cerr.Banner = banner
		cerr.LatencyMs = msSince(started)
		return nil, cerr
	}
	_ = conn.SetDeadline(time.Time{})

	return &dialResult{
		client:  ssh.NewClient(sshConn, chans, reqs),
		banner:  banner,
		latency: time.Since(started),
		record:  hkRecord,
		trusted: trusted,
	}, nil
}

// classifyDialError 把 TCP 层失败归类。
func classifyDialError(err error) *connectError {
	detail := "无法连接： " + err.Error()
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return &connectError{Code: codeDNS, Stage: stageResolve, Detail: "域名解析失败：" + err.Error()}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &connectError{Code: codeDialTimeout, Stage: stageDial, Detail: "连接被取消或超时：" + err.Error()}
	case errors.As(err, &netErr) && netErr.Timeout():
		return &connectError{Code: codeDialTimeout, Stage: stageDial, Detail: "连接超时：" + err.Error()}
	case isConnectionRefused(err):
		return &connectError{Code: codeDialRefused, Stage: stageDial, Detail: "目标拒绝连接（端口不对或 SSH 服务未启动）：" + err.Error()}
	default:
		return &connectError{Code: codeDial, Stage: stageDial, Detail: detail}
	}
}

// classifyHandshakeError 把握手/认证失败归类。
//
// x/crypto 把这几类错误都包成 "ssh: handshake failed: …"，只能按措辞区分：
//   - `unable to authenticate, attempted methods [none password], no supported methods remain`
//     → 尝试过的方法都被拒了 → auth_failed
//   - 同一句话里 attempted methods 只有 `none`（我们发的方式服务器一个都不认）
//     → auth_unsupported（用户该换认证方式，而不是怀疑密码）
func classifyHandshakeError(err error) *connectError {
	msg := err.Error()
	var netErr net.Error
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		methods := attemptedMethods(msg)
		if methods == "none" {
			return &connectError{Code: codeAuthUnsupported, Stage: stageAuth, Detail: "服务器不接受当前认证方式：" + msg}
		}
		return &connectError{Code: codeAuthFailed, Stage: stageAuth, Detail: "认证失败（登录名 / 密码 / 密钥不对）：" + msg}
	case strings.Contains(msg, "no supported methods"):
		return &connectError{Code: codeAuthUnsupported, Stage: stageAuth, Detail: "服务器不接受当前认证方式：" + msg}
	case errors.Is(err, context.DeadlineExceeded):
		return &connectError{Code: codeDialTimeout, Stage: stageHandshake, Detail: "握手超时：" + msg}
	case errors.As(err, &netErr) && netErr.Timeout():
		return &connectError{Code: codeDialTimeout, Stage: stageHandshake, Detail: "握手超时：" + msg}
	default:
		return &connectError{Code: codeHandshake, Stage: stageHandshake, Detail: "SSH 握手失败：" + msg}
	}
}

// attemptedMethods 从 "attempted methods [none password]" 里取出方法列表。
func attemptedMethods(msg string) string {
	const marker = "attempted methods ["
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if j := strings.IndexByte(rest, ']'); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return ""
}

// probeSFTP 验证远端确实能开 SFTP 子系统。
//
// 连接测试要连这一步一起判：文件管理是核心功能，如果远端 sshd 没开 sftp 子系统
// （Subsystem sftp 没配 / sftp-server 不存在），用户能开终端但点开文件管理必然失败。
// 这里先给出原因，比让他自己撞上去清楚。
func probeSFTP(client *ssh.Client) *connectError {
	sess, err := client.NewSession()
	if err != nil {
		return &connectError{Code: codeShellFailed, Stage: stageShell, Detail: "无法打开会话通道：" + err.Error()}
	}
	defer sess.Close()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		return &connectError{Code: codeSFTPUnavailable, Stage: stageShell, Detail: "远端没有可用的 SFTP 子系统（sshd 未配置 Subsystem sftp）：" + err.Error()}
	}
	return nil
}

// isConnectionRefused 判断「目标拒绝连接」。单独的 net.OpError 在 Linux 上包着
// syscall.ECONNREFUSED，用 errors.Is 逐层比对（不同平台/包装方式下判定不一致）。
func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// msSince 返回毫秒耗时（LatencyMs 是 int，前端拿它显示「连接成功 · 12ms」）。
func msSince(t time.Time) int {
	ms := time.Since(t).Milliseconds()
	if ms < 0 {
		return 0
	}
	return int(ms)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
