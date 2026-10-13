package testssh

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// 客户端请求的线格式结构。
//
// x/crypto v0.58 起服务端不再提供 ssh.Session 的那套辅助 API（Pty/Command/Subsystem
// 全没了），这些结构在 ssh 包里是私有的，只能按 RFC 4254 自己镜像一遍：
// **字段顺序与类型必须与客户端发送的一致**，否则 ssh.Unmarshal 会解出垃圾。
type (
	ptyRequestMsg struct {
		Term     string
		Columns  uint32
		Rows     uint32
		Width    uint32
		Height   uint32
		Modelist string
	}
	ptyWindowChangeMsg struct {
		Columns uint32
		Rows    uint32
		Width   uint32
		Height  uint32
	}
	envRequestMsg struct {
		Name  string
		Value string
	}
	execRequestMsg struct {
		Command string
	}
	subsystemRequestMsg struct {
		Subsystem string
	}
	exitStatusMsg struct {
		Status uint32
	}
)

// serveSession 处理一个 session 通道：pty-req / window-change / env / shell / exec /
// subsystem，直到客户端关闭通道或子系统结束。
func (s *Server) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	ss := &sessionServer{srv: s, ch: ch, term: "xterm-256color", cols: 80, rows: 24}
	ss.run(reqs)
}

type sessionServer struct {
	srv  *Server
	ch   ssh.Channel
	term string
	cols uint32
	rows uint32
	env  []string
	// ptyRequested 记录客户端有没有发过 pty-req。它决定 exec/shell 走真 PTY 还是普通管道
	// ——真实 sshd 也是这样：没收到 pty-req 就不分配终端（见 start 的说明）。
	ptyRequested bool

	cmd       *exec.Cmd
	ptyMaster *os.File
}

func (ss *sessionServer) run(reqs <-chan *ssh.Request) {
	defer ss.cleanup()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var m ptyRequestMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			if ss.cmd != nil {
				// shell 已经起来了：PTY 只能在启动前分配，晚到的 pty-req 直接拒绝
				// （回 true 会让客户端以为尺寸生效了，实际没有）。
				req.Reply(false, nil)
				continue
			}
			ss.term = nonEmpty(m.Term, "xterm-256color")
			ss.ptyRequested = true
			if m.Columns > 0 {
				ss.cols = m.Columns
			}
			if m.Rows > 0 {
				ss.rows = m.Rows
			}
			req.Reply(true, nil)

		case "window-change":
			var m ptyWindowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			if m.Columns > 0 {
				ss.cols = m.Columns
			}
			if m.Rows > 0 {
				ss.rows = m.Rows
			}
			// 真的改 PTY 尺寸：这样测试才能验证「resize 之后换行位置跟着变」，
			// 而不是只看到客户端没报错。
			if ss.ptyMaster != nil {
				if err := pty.Setsize(ss.ptyMaster, &pty.Winsize{Cols: uint16(ss.cols), Rows: uint16(ss.rows)}); err != nil {
					ss.srv.logf("window-change 设置 PTY 尺寸失败：%v", err)
				}
			}
			// RFC 4254 规定 window-change 的 wantReply 通常为 false；客户端不读回复，
			// 但这里必须回（x/crypto 对 wantReply=true 的请求不回会阻塞对端）。
			if req.WantReply {
				req.Reply(true, nil)
			}

		case "env":
			var m envRequestMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			ss.env = append(ss.env, m.Name+"="+m.Value)
			req.Reply(true, nil)

		case "shell":
			if err := ss.start(""); err != nil {
				ss.srv.logf("启动 shell 失败：%v", err)
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)

		case "exec":
			var m execRequestMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			if err := ss.start(m.Command); err != nil {
				ss.srv.logf("执行命令失败：%v", err)
				req.Reply(false, nil)
				return
			}
			req.Reply(true, nil)

		case "subsystem":
			var m subsystemRequestMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			if m.Subsystem != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			ss.serveSFTP()
			return

		default:
			req.Reply(false, nil)
		}
	}
}

// start 起一个真实 shell（或执行一条命令）。
//
// 两条路径：
//   - 客户端发过 pty-req → 走真 PTY（交互式终端要的语义：提示符、输入回显、window-change
//     改尺寸后换行重排）。PTY 不可用时会失败，所以调用方用例要先用 PTYAvailable 守卫。
//   - 没发过 pty-req（通常是 exec 跑一条命令，例如后端调远端 tar/unzip）→ 走普通管道。
//     真实 sshd 也是这个行为（没有 pty-req 就不分配终端），而且它让「只是执行命令」的
//     测试完全不依赖 /dev/ptmx——那些用例验的是压缩解压，不该因为环境没有 PTY 而翻红。
//
// 输入方向一个协程（客户端 → PTY/管道），输出方向另一个协程；输出读尽（子进程退出，
// Linux 上 PTY 表现为 EIO）后回收进程、回 exit-status 并关通道——真实 sshd 也是这个顺序，
// 否则客户端会一直等一个永远不会来的退出状态。
func (ss *sessionServer) start(command string) error {
	if ss.cmd != nil {
		return errors.New("会话已经启动过了")
	}
	if !ss.ptyRequested {
		return ss.startPipes(command)
	}
	shell := ss.srv.shell
	cmd := exec.Command(shell, shellArgs(shell, command)...)
	cmd.Dir = ss.srv.root
	cmd.Env = ss.buildEnv()

	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(ss.cols), Rows: uint16(ss.rows)})
	if err != nil {
		return err
	}
	ss.cmd, ss.ptyMaster = cmd, master

	go func() {
		// 客户端关闭通道时这里会返回；master 关了也会返回。
		_, _ = io.Copy(master, ss.ch)
	}()
	go func() {
		_, _ = io.Copy(ss.ch, master)
		status := ss.waitStatus(cmd.Wait())
		_, _ = ss.ch.SendRequest("exit-status", false, ssh.Marshal(&exitStatusMsg{Status: status}))
		_ = ss.ch.Close()
		_ = master.Close()
	}()
	return nil
}

// startPipes 用普通管道执行一条命令（没有 PTY 的 exec）。
//
// 关于 stderr：x/crypto 的 Channel 接口**不提供**「写扩展数据（stderr）」的能力（服务端
// 那套辅助 API 在 v0.58 已经被移除了），所以这里把 stdout 与 stderr 都写进同一条数据流。
// 真实 sshd 是分两条流发的，因此**生产代码必须两条流都收**（backend/remote_exec.go 的
// runRemote 正是这么做的，诊断信息优先看 stderr、空了再看 stdout）。
func (ss *sessionServer) startPipes(command string) error {
	shell := ss.srv.shell
	cmd := exec.Command(shell, shellArgs(shell, command)...)
	cmd.Dir = ss.srv.root
	cmd.Env = ss.buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	ss.cmd = cmd

	go func() {
		// 命令不读 stdin 时这次 Copy 会一直等着客户端关通道；关掉写端让子进程看到 EOF。
		_, _ = io.Copy(stdin, ss.ch)
		_ = stdin.Close()
	}()
	var copied sync.WaitGroup
	copied.Add(2)
	go func() { defer copied.Done(); _, _ = io.Copy(ss.ch, stdout) }()
	go func() { defer copied.Done(); _, _ = io.Copy(ss.ch, stderr) }()
	go func() {
		// 先把两条流读干净再取退出状态：Wait 会关掉管道，提前调会丢掉最后一段输出。
		copied.Wait()
		status := ss.waitStatus(cmd.Wait())
		_, _ = ss.ch.SendRequest("exit-status", false, ssh.Marshal(&exitStatusMsg{Status: status}))
		_ = ss.ch.Close()
	}()
	return nil
}

// waitStatus 把进程退出结果翻成 SSH 的 exit-status（信号退出用 128+signal）。
func (ss *sessionServer) waitStatus(err error) uint32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	if ws.Signaled() {
		return uint32(128 + int(ws.Signal()))
	}
	return uint32(ws.ExitStatus())
}

func (ss *sessionServer) serveSFTP() {
	srv, err := sftp.NewServer(ss.ch, sftp.WithServerWorkingDirectory(ss.srv.root))
	if err != nil {
		ss.srv.logf("初始化 SFTP 服务端失败：%v", err)
		return
	}
	defer srv.Close()
	if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
		ss.srv.logf("SFTP 会话结束：%v", err)
	}
}

func (ss *sessionServer) buildEnv() []string {
	env := []string{
		"TERM=" + ss.term,
		"HOME=" + ss.srv.root,
		"PWD=" + ss.srv.root,
		"SHELL=" + ss.srv.shell,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	// 客户端用 env 请求带来的变量放最后：同名时后出现的生效，与 sshd 的行为一致。
	env = append(env, ss.env...)
	// 服务器自己的 Env 覆盖（测试模拟「远端缺工具」这类环境差异，必须能盖掉上面的 PATH）。
	return append(env, ss.srv.extraEn...)
}

func (ss *sessionServer) cleanup() {
	if ss.cmd != nil && ss.cmd.Process != nil {
		// 客户端关了通道（我们的后端关闭终端就是这么做的）：进程还在就杀掉，
		// 否则测试里会留下孤儿 bash。
		_ = ss.cmd.Process.Kill()
	}
	if ss.ptyMaster != nil {
		_ = ss.ptyMaster.Close()
	}
}

// shellArgs 决定怎么起 shell。
//
// 交互式会话**不带 rc 文件**（bash 的 --norc/--noprofile）：测试要的是确定性的行为
// （提示符、输入回显、stty 输出），而开发机上的 ~/.bashrc 常常会 echo 东西或改 PS1，
// 让断言变得不可靠。带 command 时走 `-c`（exec 请求）。
func shellArgs(shell, command string) []string {
	base := filepath.Base(shell)
	if command != "" {
		return []string{"-c", command}
	}
	switch {
	case strings.HasPrefix(base, "bash"):
		// 长选项必须排在短选项前面（bash 自己会报 "--: invalid option"）。
		return []string{"--norc", "--noprofile", "-i"}
	case strings.HasPrefix(base, "zsh"):
		return []string{"-i", "-f"}
	default:
		return []string{"-i"}
	}
}

func nonEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
