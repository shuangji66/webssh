package testssh

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// clientConfig 是测试里的客户端配置：接受任意主机密钥（TOFU 是后端的事，
// 这个测试只关心服务器这边能不能正常收发）。
func clientConfig(user string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password("demo")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
}

// TestShellPTYAndResize 验证「真 PTY + 交互 shell」这条链路：
// 提示符、输入回显、命令输出、window-change 生效。
func TestShellPTYAndResize(t *testing.T) {
	srv := StartServer(t, Options{User: "demo", Password: "demo", Root: t.TempDir()})
	conn, err := ssh.Dial("tcp", srv.Addr(), clientConfig("demo"))
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatalf("新建会话失败：%v", err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		t.Fatalf("请求 PTY 失败：%v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("取 stdin 失败：%v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("取 stdout 失败：%v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("启动 shell 失败：%v", err)
	}

	// 等提示符（交互式 shell 一定会打一个），这同时证明 PTY 真的接上了。
	if got := readUntil(t, stdout, "$ ", 5*time.Second); !strings.Contains(got, "$ ") {
		t.Fatalf("没有等到提示符，收到：%q", got)
	}
	// 输入回显：PTY 的 ECHO 打开时，敲进去的字符会原样回显。
	if _, err := io.WriteString(stdin, "echo hello-pty\n"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	got := readUntil(t, stdout, "hello-pty", 5*time.Second)
	if !strings.Contains(got, "hello-pty") {
		t.Fatalf("没有收到命令输出，收到：%q", got)
	}

	// resize：改成很小的宽度，然后问 stty 现在是多少列——只有真的 Setsize 了才会变。
	if err := sess.WindowChange(24, 42); err != nil {
		t.Fatalf("window-change 失败：%v", err)
	}
	io.WriteString(stdin, "stty size\n")
	got = readUntil(t, stdout, "42", 5*time.Second)
	if !strings.Contains(got, "24 42") {
		t.Fatalf("resize 未生效（期望 24 42），收到：%q", got)
	}
}

// TestSFTPSubsystem 验证 SFTP 子系统能列目录、建目录、写文件、改名、删除。
func TestSFTPSubsystem(t *testing.T) {
	root := t.TempDir()
	srv := StartServer(t, Options{User: "demo", Password: "demo", Root: root})
	conn, err := ssh.Dial("tcp", srv.Addr(), clientConfig("demo"))
	if err != nil {
		t.Fatalf("拨号失败：%v", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatalf("新建会话失败：%v", err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("请求 sftp 子系统失败：%v", err)
	}
	stdout, _ := sess.StdoutPipe()
	stdin, _ := sess.StdinPipe()
	c, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		t.Fatalf("初始化 SFTP 客户端失败：%v", err)
	}
	defer c.Close()

	// RealPath(".") 必须是 Root：后端的 fs/list 用空 path 时取的就是它（家目录）。
	home, err := c.RealPath(".")
	if err != nil {
		t.Fatalf("RealPath 失败：%v", err)
	}
	if home != srv.Root() {
		t.Fatalf("家目录 = %q，期望 %q", home, srv.Root())
	}

	if err := c.Mkdir(home + "/dir"); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	f, err := c.Create(home + "/dir/a.txt")
	if err != nil {
		t.Fatalf("建文件失败：%v", err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	f.Close()

	entries, err := c.ReadDir(home + "/dir")
	if err != nil {
		t.Fatalf("列目录失败：%v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("目录内容不对：%+v", entries)
	}
	if err := c.Rename(home+"/dir/a.txt", home+"/dir/b.txt"); err != nil {
		t.Fatalf("改名失败：%v", err)
	}
	if err := c.Remove(home + "/dir/b.txt"); err != nil {
		t.Fatalf("删除文件失败：%v", err)
	}
	if err := c.RemoveDirectory(home + "/dir"); err != nil {
		t.Fatalf("删除目录失败：%v", err)
	}
	if _, err := os.Stat(root + "/dir"); !os.IsNotExist(err) {
		t.Fatalf("目录应已删除，stat 结果：%v", err)
	}
}

// TestPasswordAndPublicKeyAuth 验证两种认证方式都能登录、错误凭据会被拒。
func TestPasswordAndPublicKeyAuth(t *testing.T) {
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	srv := StartServer(t, Options{User: "demo", Password: "demo", AuthorizedKeys: []ssh.PublicKey{kp.PublicKey()}, Root: t.TempDir()})

	// 密码
	if c, err := ssh.Dial("tcp", srv.Addr(), clientConfig("demo")); err != nil {
		t.Fatalf("密码认证失败：%v", err)
	} else {
		c.Close()
	}
	// 公钥
	cfg := &ssh.ClientConfig{
		User:            "demo",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(kp.Signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	if c, err := ssh.Dial("tcp", srv.Addr(), cfg); err != nil {
		t.Fatalf("公钥认证失败：%v", err)
	} else {
		c.Close()
	}
	// 错误密码
	bad := clientConfig("demo")
	bad.Auth = []ssh.AuthMethod{ssh.Password("wrong")}
	if c, err := ssh.Dial("tcp", srv.Addr(), bad); err == nil {
		c.Close()
		t.Fatal("错误密码不应通过认证")
	}
}

// TestPEMRoundTrip 验证导出的私钥 PEM 能被 ssh.ParsePrivateKey 解析（集成测试要把它
// 当作 SessionInput.privateKey 用，这一步错了整条密钥路径都测不了）。
func TestPEMRoundTrip(t *testing.T) {
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	pemText, err := kp.PEM()
	if err != nil {
		t.Fatalf("导出私钥失败：%v", err)
	}
	if !strings.HasPrefix(pemText, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("私钥格式不对：%q", pemText[:40])
	}
	if _, err := ssh.ParsePrivateKey([]byte(pemText)); err != nil {
		t.Fatalf("解析私钥失败：%v", err)
	}
	enc, err := kp.PEMWithPassphrase("secret")
	if err != nil {
		t.Fatalf("导出带口令私钥失败：%v", err)
	}
	if _, err := ssh.ParsePrivateKey([]byte(enc)); err == nil {
		t.Fatal("带口令的私钥不该能无口令解析")
	}
	if _, err := ssh.ParsePrivateKeyWithPassphrase([]byte(enc), []byte("secret")); err != nil {
		t.Fatalf("带口令解析失败：%v", err)
	}
}

// readUntil 读到出现 want 或超时（PTY 输出是流式的，没有固定长度可等）。
func readUntil(t *testing.T, r io.Reader, want string, timeout time.Duration) string {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc []byte
		for {
			n, err := r.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				if strings.Contains(string(acc), want) {
					ch <- result{acc, nil}
					return
				}
			}
			if err != nil {
				ch <- result{acc, err}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		return string(res.data)
	case <-time.After(timeout):
		return ""
	}
}
