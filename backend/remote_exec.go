package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---------------------------------------------------------------------------
// 远程命令执行（契约 11.5：压缩 / 解压要调远端主机的 tar / unzip）
//
// 本应用**不在本机执行用户命令**（AGENTS.md 第 1 节），所以这里做的是「把一条命令交给
// 远程主机上登录用户的 shell 去跑」：ssh.Session 的 exec 通道 + `sh -c` + 单引号引用 +
// 采集 stdout/stderr + 退出码 + 超时/取消。
//
// 为什么再拨一条连接而不是复用 r.base（会话打开时那条、SFTP 挂在它上面）：
//  1. r.base 由 sftpMu 保护，一次 5 分钟的 tar 会把整个文件管理界面卡住；
//  2. 命令超时/取消时我们得强杀进程甚至关掉连接，不能把 SFTP 通道一起带走。
// 终端也是这么做的（每条终端一条独立连接，见 sessions.go 的 newTerminal）。
// ---------------------------------------------------------------------------

// remoteCommandTimeout 是单条远程命令的超时。
//
// 5 分钟：压缩一个几十 GB 的目录确实可能更久，但这里没有进度语义（HTTP 请求上没法显示
// 进度，也不该让一个请求无限期挂着）。超时就杀掉远端进程并明确报错，用户可以做小批量重试。
const remoteCommandTimeout = 5 * time.Minute

// remoteOutputLimit 是单条命令 stdout / stderr 的采集上限。
//
// 必须有上限：命令输出由**远端**决定，`tar -cvf` 或一个被攻击者控制的远端可以把输出刷到
// 内存耗尽（这个进程承载全部会话）。超出的部分丢弃（保留前 remoteOutputLimit 字节——
// 错误信息几乎都在开头几行）。
const remoteOutputLimit = 64 << 10

// shellQuote 把一段文本安全地交给 POSIX shell（契约 11.5：所有插值路径必须 shell 安全引用）。
//
// 规则：整体用单引号包住；内部出现的单引号拆成「闭合 → 反斜杠转义的单引号 → 重新打开」
// 三步（就是 replace 里那 4 个字符）。单引号里除了单引号本身没有任何特殊字符，所以
// `$(...)`、反引号、`;`、空格、换行都不会被解释——文件名里带这些字符时命令不会被拆开，
// 也不会被注入。
//
// 空串也必须包成一对单引号：裸露的空串在 `tar -C <parent> <item>` 里会让 tar 把下一个
// 参数当成路径。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteResult 是一条远程命令的执行结果。
//
// Code 为 -1 表示对端没有回 exit-status（x/crypto 的 *ExitMissingError）：这时既不能说
// 成功也不能说失败，交由调用方按「失败但原因未知」处理。
type remoteResult struct {
	Stdout    string
	Stderr    string
	Code      int
	Truncated bool
}

// ok 报告命令是否成功退出（退出码 0）。
func (r remoteResult) ok() bool { return r.Code == 0 }

// diagnostic 汇总命令输出里可用于错误文案的关键行（先 stderr，空了再看 stdout）。
//
// 为什么两条流都要看：真实 sshd 把 stderr 走扩展数据通道，但不少工具（unzip 就是一例）
// 把错误写在 stdout 上；测试用的进程内服务器更是只有一条流。
func (r remoteResult) diagnostic() string {
	if s := keyLines(r.Stderr); s != "" {
		return s
	}
	return keyLines(r.Stdout)
}

// keyLines 取输出里最多 3 行非空内容并压成一行（错误文案要塞进 JSON 响应里）。
func keyLines(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) == 3 {
			break
		}
	}
	joined := strings.Join(lines, " / ")
	// 按字符（rune）截断，避免把一个多字节字符切成两半（JSON 里会出现非法 UTF-8）。
	if runes := []rune(joined); len(runes) > maxErrLinesRunes {
		joined = string(runes[:maxErrLinesRunes])
	}
	return joined
}

// maxErrLinesRunes 是诊断信息进错误文案时的长度上限（字符）。
const maxErrLinesRunes = 300

// runRemote 在远程主机上执行一条命令并等它结束。
//
// command 是**没有引用过**的命令原文（例如 `mkdir -p '/x' && tar -czf '/y' -C '/x' a`）；
// 这里会把它整体再包一层 `sh -c '<原文>'`。两层引用的原因：exec 通道里的字符串是被
// **登录用户的 shell** 解释的（sshd 用 `$SHELL -c <command>`），显式套一层 `sh -c` 才能
// 保证「解释这条命令的 shell」是我们预期的那个（用户把登录 shell 换成 csh/fish 时，语法
// 差异会变成莫名其妙的行为）；外层引用则保证这层命令原文不会被登录 shell 再展开一次。
//
// 返回的 error 只表示**基础设施失败**（拨号 / 开会话通道 / 超时取消）；远端命令退出码非 0
// 不是 error，它放在 remoteResult.Code 里，由调用方决定怎么翻译成错误码。
func (r *sessionRuntime) runRemote(ctx context.Context, command string) (remoteResult, *apiError) {
	cfg, ok := r.config()
	if !ok {
		return remoteResult{}, errSessionGone("会话不存在：%s", r.id)
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCommandTimeout)
	defer cancel()

	req, apiErr := r.mgr.dialRequestOf(cfg)
	if apiErr != nil {
		return remoteResult{}, apiErr
	}
	res, cerr := r.mgr.dial(ctx, req)
	if cerr != nil {
		return remoteResult{}, errFromConnect(cerr)
	}
	// 这条连接只服务这一条命令：命令结束（或被杀）就整条关掉，不留半死不活的通道。
	defer res.client.Close()

	sess, err := res.client.NewSession()
	if err != nil {
		return remoteResult{}, errShellFailed("无法打开 SSH 会话通道：%v", err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return remoteResult{}, errShellFailed("无法创建 stdout 管道：%v", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return remoteResult{}, errShellFailed("无法创建 stderr 管道：%v", err)
	}

	var outBuf, errBuf limitedBuffer
	var copied sync.WaitGroup
	copied.Add(2)
	go func() { defer copied.Done(); _, _ = io.Copy(&outBuf, stdout) }()
	go func() { defer copied.Done(); _, _ = io.Copy(&errBuf, stderr) }()

	if err := sess.Start("sh -c " + shellQuote(command)); err != nil {
		return remoteResult{}, errShellFailed("无法在远端执行命令：%v", err)
	}

	waited := make(chan error, 1)
	go func() {
		// 先把两条流读干净再取退出状态：否则 Wait 一返回就把通道关了，最后一段输出会丢，
		// 而错误信息往往正是那最后一段。
		copied.Wait()
		waited <- sess.Wait()
	}()

	select {
	case err := <-waited:
		var code int
		switch {
		case err == nil:
			code = 0
		default:
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitStatus()
			} else {
				// *ExitMissingError：远端没回 exit-status（连接被掐、sshd 崩了）。
				// 当成「失败但原因未知」，同时留一条日志，否则这类问题在服务端毫无痕迹。
				code = -1
				warnf("会话 %s 的远程命令没有拿到退出状态：%v", r.id, err)
			}
		}
		return remoteResult{Stdout: outBuf.String(), Stderr: errBuf.String(), Code: code, Truncated: outBuf.truncated || errBuf.truncated}, nil

	case <-ctx.Done():
		// 超时 / 客户端取消：强杀远端进程并关掉连接（关连接保证上面的 Copy 与 Wait 都会返回，
		// 不会漏协程）。SIGKILL 而不是 SIGTERM：tar 收到 TERM 也未必立刻退（它在写大文件）。
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		_ = res.client.Close()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return remoteResult{}, failErr(504, "internal", "远程命令超过 %s 还没结束，已终止", remoteCommandTimeout)
		}
		return remoteResult{}, errAborted("远程命令已取消")
	}
}

// limitedBuffer 是「只保留前 N 字节」的缓冲区：够用（错误信息在前面）且不会因为远端刷
// 输出把内存吃掉。
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := remoteOutputLimit - b.buf.Len(); room > 0 {
		if n > room {
			b.truncated = true
			p = p[:room]
		}
		b.buf.Write(p)
	} else {
		b.truncated = true
	}
	// 必须报「全部消费」：返回短写会让 io.Copy 以为出错，远端进程会收到 SIGPIPE。
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// looksLikePermission 判断命令输出的关键行里有没有「权限不够」的意思。
//
// 中英都要认，两边都要认全：远端可能是英文 locale 的 GNU tar，也可能是中文 locale 或
// busybox 的精简实现。识别错了不会造成破坏（只是错误码从 permission 变成 internal），
// 但认全了能让用户一眼看出「不是 bug，是权限」。
func looksLikePermission(out string) bool {
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"permission denied",
		"operation not permitted",
		"read-only file system",
		"access denied",
		"权限",
		"拒绝访问",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// looksLikeMissingTool 判断输出里有没有「找不到命令」的意思（退出码 127 之外的兜底）。
//
// 只认 shell 自己报「命令不存在」的那几句：像 "no such file or directory" 这种措辞
// **不能**用——`tar: /x/y: Cannot stat: No such file or directory` 说的是源文件不存在，
// 把它当成「缺少工具」会把一个清清楚楚的 not_found 变成一句误导人的错误码。
func looksLikeMissingTool(out string) bool {
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"command not found", // bash: bash: line 1: tar: command not found
		": not found",       // dash: sh: 1: tar: not found
		"executable file not found",
		"cannot exec",
		"找不到命令",
		"未找到命令",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
