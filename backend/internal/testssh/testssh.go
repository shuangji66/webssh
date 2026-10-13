// Package testssh 是一个**仅用于测试与本地联调**的进程内 SSH + SFTP 服务器。
//
// 为什么要自己写一个：集成测试必须真连一条 SSH 链路（PTY、认证、SFTP 子系统、主机密钥），
// 而构建机上不一定装着 sshd，更不能要求 root 去改系统配置。这个包把服务器跑在测试进程里，
// 监听 127.0.0.1 的随机端口，测试用 t.Cleanup 关掉即可。
//
// 三件要说清的事：
//   - **主程序绝不引用它**（AGENTS.md：本应用不在本机执行用户命令，唯一例外就是这个测试
//     服务器）。因此它才允许 `os/exec` 起一个真实 shell。
//   - 它提供的 shell 等于把**当前用户**的 shell 暴露给能连上该端口的人。所以默认只监听
//     127.0.0.1，命令行入口 `testsshd` 在绑定非回环地址时要求显式加 `-allow-remote`。
//   - 它用真实 PTY（creack/pty）：只有真 PTY 才能验证「提示符 + 输入回显 + window-change
//     之后换行重排」这类行为；管道只能验证数据流。
//
// x/crypto v0.58 起 **服务端没有 ssh.Session 那套辅助 API**（Pty()/Command()/Subsystem()
// 都没有了），pty-req / window-change / shell / subsystem 这些请求必须自己解包处理，
// 见 session.go 顶部的说明。
package testssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// Options 是启动参数。零值即可用：随机端口、随机主机密钥、demo/demo 之外全空。
type Options struct {
	// Addr 监听地址，默认 127.0.0.1:0（随机端口）。
	Addr string
	// User / Password 是密码认证的凭据。Password 为空则不启用密码认证。
	User     string
	Password string
	// AuthorizedKeys 允许登录的公钥。为空则不启用公钥认证。
	AuthorizedKeys []ssh.PublicKey
	// HostKey 是主机私钥；为空时每次启动生成一把新的 ed25519
	// （指纹每次都不同，正好用来验证 TOFU 的「首次需要信任」）。
	HostKey ssh.Signer
	// Root 是 SFTP 的家目录（也是 shell 的工作目录）。为空时用当前工作目录。
	//
	// 注意：pkg/sftp 的服务端只把 Root 当作**相对路径的基准**，绝对路径直接落到真实
	// 文件系统上（它没有沙箱）。这是测试服务器，只在本机回环上监听。
	Root string
	// Shell 是要启动的 shell，默认 /bin/bash。
	Shell string
	// Env 是额外的环境变量（"K=V"），追加在最后 —— 同名时后面的生效。
	//
	// 用途：模拟一台「缺工具」的远端主机（例如 PATH=/nonexistent，让 tar/unzip 找不到），
	// 用来验证后端的 no_archive_tool 分支。真实 sshd 的 PATH 由 sshd 与登录脚本决定，
	// 这里只能人为覆盖。
	Env []string
	// Logger 收到一条非致命失败时的回调（默认丢弃）。
	Logger func(format string, args ...any)
}

// Server 是一个跑在进程内、监听 127.0.0.1 的 SSH + SFTP 服务器。
type Server struct {
	ln      net.Listener
	cfg     *ssh.ServerConfig
	hostKey ssh.Signer
	root    string
	shell   string
	extraEn []string
	logf    func(format string, args ...any)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// Start 启动服务器并返回句柄。调用方负责 Close。
func Start(opts Options) (*Server, error) {
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	shell := opts.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	root := opts.Root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("取当前工作目录失败：%w", err)
		}
		root = wd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析 Root 失败：%w", err)
	}
	if st, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("Root %s 不可用：%w", root, err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("Root %s 不是目录", root)
	}

	signer := opts.HostKey
	if signer == nil {
		if signer, err = generateSigner(); err != nil {
			return nil, err
		}
	}
	logf := opts.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}

	cfg := &ssh.ServerConfig{
		// 默认 6 次；收到 3 次就断，测试里跑得快一点，也让「认证失败」很快返回。
		MaxAuthTries:  3,
		ServerVersion: "SSH-2.0-webssh-testssh",
	}
	if opts.Password != "" {
		user, password := opts.User, opts.Password
		cfg.PasswordCallback = func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			okUser := user == "" || subtle.ConstantTimeCompare([]byte(conn.User()), []byte(user)) == 1
			okPass := subtle.ConstantTimeCompare(pass, []byte(password)) == 1
			if okUser && okPass {
				return nil, nil
			}
			return nil, fmt.Errorf("认证失败")
		}
	}
	if len(opts.AuthorizedKeys) > 0 {
		allowed := make(map[string]bool, len(opts.AuthorizedKeys))
		for _, k := range opts.AuthorizedKeys {
			allowed[string(k.Marshal())] = true
		}
		cfg.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if allowed[string(key.Marshal())] {
				return nil, nil
			}
			return nil, fmt.Errorf("公钥未被授权")
		}
	}
	if cfg.PasswordCallback == nil && cfg.PublicKeyCallback == nil {
		return nil, fmt.Errorf("至少要配置一种认证方式（Password 或 AuthorizedKeys）")
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{
		ln:      ln,
		cfg:     cfg,
		hostKey: signer,
		root:    root,
		shell:   shell,
		extraEn: opts.Env,
		logf:    logf,
		conns:   map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// StartServer 是测试用入口：启动服务器并在测试结束时自动关闭。
func StartServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Start(opts)
	if err != nil {
		t.Fatalf("启动 testssh 失败：%v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				_ = conn.Close()
			}()
			s.serveConn(conn)
		}()
	}
}

func (s *Server) serveConn(nc net.Conn) {
	_, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		s.logf("SSH 握手失败：%v", err)
		return
	}
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "只支持 session 通道")
			continue
		}
		ch, chReqs, err := newChannel.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer ch.Close()
			s.serveSession(ch, chReqs)
		}()
	}
}

// Close 关闭监听、断开所有连接并等待处理协程退出。
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
}

// Addr 返回 host:port。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Host 返回监听主机（供 SessionInput.host 使用）。
func (s *Server) Host() string {
	host, _, err := net.SplitHostPort(s.Addr())
	if err != nil {
		return s.Addr()
	}
	return host
}

// Port 返回监听端口（供 SessionInput.port 使用）。
func (s *Server) Port() int {
	_, portStr, err := net.SplitHostPort(s.Addr())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(portStr)
	return port
}

// Fingerprint 返回主机密钥的 SHA256 指纹（与 known_hosts 里存的是同一种写法）。
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

// KeyType 返回主机密钥类型（如 ssh-ed25519）。
func (s *Server) KeyType() string { return s.hostKey.PublicKey().Type() }

// Root 返回 SFTP 的家目录。
func (s *Server) Root() string { return s.root }

// KeyPair 是一对测试用的密钥：既当主机密钥，也当客户端私钥。
type KeyPair struct {
	Signer  ssh.Signer
	priv    ed25519.PrivateKey
	Comment string
}

// NewKeyPair 生成一把 ed25519 密钥。
func NewKeyPair() (*KeyPair, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	return &KeyPair{Signer: signer, priv: priv, Comment: "webssh-test"}, nil
}

// PublicKey 返回可放进 Options.AuthorizedKeys 的公钥。
func (k *KeyPair) PublicKey() ssh.PublicKey { return k.Signer.PublicKey() }

// PEM 导出未加密的 OpenSSH 私钥（可直接填进 SessionInput.privateKey）。
func (k *KeyPair) PEM() (string, error) {
	block, err := ssh.MarshalPrivateKey(k.priv, k.Comment)
	if err != nil {
		return "", err
	}
	return string(pemEncode(block)), nil
}

// PEMWithPassphrase 导出带口令的 OpenSSH 私钥（用于验证「私钥口令」路径）。
func (k *KeyPair) PEMWithPassphrase(passphrase string) (string, error) {
	block, err := ssh.MarshalPrivateKeyWithPassphrase(k.priv, k.Comment, []byte(passphrase))
	if err != nil {
		return "", err
	}
	return string(pemEncode(block)), nil
}

// generateSigner 生成一把 ed25519 主机密钥。
func generateSigner() (ssh.Signer, error) {
	kp, err := NewKeyPair()
	if err != nil {
		return nil, err
	}
	return kp.Signer, nil
}

// pemEncode 把 ssh.MarshalPrivateKey 的结果编成 PEM 文本。
func pemEncode(block *pem.Block) []byte { return pem.EncodeToMemory(block) }

// PTYAvailable 报告当前环境能不能分配 PTY（能否打开 /dev/ptmx）。
//
// 为什么需要它：在受限的沙箱 / 容器里（/dev 只读、禁止打开设备节点），pty.Start 会以
// "open /dev/ptmx: permission denied" 失败，于是所有「要起一个真 PTY」的用例会以
// 「无法启动远程 shell」这种**与真实故障无关**的形态翻红——排查的人会去怀疑 SSH 或
// SFTP 代码。用例开头调它，环境不支持就 t.Skip 并写明原因：跳过的是「环境能力」，
// 不是「代码正确性」，本机与 CI 上照常全跑。
func PTYAvailable() (bool, string) {
	master, slave, err := pty.Open()
	if err != nil {
		return false, "当前环境无法分配 PTY（" + err.Error() + "）：PTY 相关用例已跳过"
	}
	_ = master.Close()
	_ = slave.Close()
	return true, ""
}
