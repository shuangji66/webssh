package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 搜索的「取消 / 写不出去」路径（第二十四轮审查 S2、S3、S9 的回归用例）。
//
// 这几条的共同点是：**单元测试跑不到**（要真的中止一次请求、或让写入失败），而它们又都在
// 「客户端走了」这条路上——正式代码里最容易被当成「目录读不出来」糊过去。所以这里直接调
// handler / walk，把判据钉死。
// ---------------------------------------------------------------------------

// countingWriter 记录写入次数，**不真写**（我们只关心「handler 返回之后还有没有人在写」）。
//
// 用 httptest.ResponseRecorder 打底是为了提供 Header()，并让 line() 里的 http.Flusher 断言成立
// （ResponseRecorder 实现了 Flush）。写入计数要能跨协程读（遍历协程写、测试协程读）。
type countingWriter struct {
	rr  *httptest.ResponseRecorder
	mu  sync.Mutex
	cnt int
}

func (c *countingWriter) Header() http.Header  { return c.rr.Header() }
func (c *countingWriter) WriteHeader(code int) { c.rr.WriteHeader(code) }
func (c *countingWriter) Flush()               { c.rr.Flush() }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.cnt++
	c.mu.Unlock()
	// 正文不落盘：这些用例只数次数，不解析内容（内容由 files_search_test.go 覆盖）。
	return len(p), nil
}

func (c *countingWriter) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cnt
}

// failingWriter 让每次写入都失败，模拟「客户端已经走了、连接写不进去」。
type failingWriter struct{ h http.Header }

func (f *failingWriter) Header() http.Header       { return f.h }
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection lost") }

// TestFsSearchStopsWritingAfterCancel 钉住 S2：
// **handler 返回之后，遍历协程不允许再碰 ResponseWriter**（net/http 的硬性要求）。
//
// 修之前 withSFTP 在 ctx 结束时立刻返回、闭包还在写：实测 handler 返回后又写了 41 次 / 453 KiB
// （等于把 2000 条结果全写在一个已经收尾的响应上）。修法是 withSFTPStream 在丢掉连接之后
// 等闭包退出。这个用例的第一半（handler 必须在取消后及时返回）与第二半（返回后写入次数不再
// 增长）缺一不可：只等不返回会被第一半抓住，只返回不等会被第二半抓住。
func TestFsSearchStopsWritingAfterCancel(t *testing.T) {
	a := newTestApp(t, appOptions{})
	root := a.sshRoot
	// 铺一棵「够搜一会儿」的树：30 个目录 × 200 个文件 = 6000 条命中。
	// 太小的话整次搜索会在我们来得及取消之前跑完，用例就退化成「什么也没测」。
	for d := 0; d < 30; d++ {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		for f := 0; f < 200; f++ {
			writeLocal(t, filepath.Join(dir, fmt.Sprintf("hit-%03d.txt", f)), "x")
		}
	}
	sid := a.newSSHSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(fmt.Sprintf(`{"sid":%q,"path":%q,"query":"hit"}`, sid, root))
	req := httptest.NewRequest(http.MethodPost, "/api/fs/search", body).WithContext(ctx)
	w := &countingWriter{rr: httptest.NewRecorder()}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_ = a.mux.handleFsSearch(w, req)
	}()

	// 等到真的写出第一行（start）再取消：取消要发生在**已经开流之后**，测的才是这条路。
	deadline := time.Now().Add(5 * time.Second)
	for w.writes() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("5 秒内没有任何写入：搜索没跑起来，这个用例失去意义")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("取消后 handler 10 秒没返回：withSFTPStream 没把遍历协程等回来（或锁被占死）")
	}

	// 关键断言：handler 已经返回，之后不允许再有写入。
	after := w.writes()
	time.Sleep(300 * time.Millisecond)
	if got := w.writes(); got != after {
		t.Fatalf("handler 返回后又写了 %d 次：net/http 要求 Handler 返回后不得再用 ResponseWriter", got-after)
	}
}

// TestFsSearchWriteFailureNotCountedAsSkipped 钉住 S3：
// 写响应失败（客户端走了）必须返回 errStreamClosed，**不能**被算成「这个目录读不出来」。
//
// 修之前它走的是 run 里那条 `skipped++` + `logx.Warnf("搜索跳过目录 …：connection lost")`
// 的分支：日志把「没人读了」说成「远端权限问题」，统计里还会凭空多一个被跳过的目录。
func TestFsSearchWriteFailureNotCountedAsSkipped(t *testing.T) {
	a, sid := searchTestApp(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("拿不到会话运行时：%v", apiErr)
	}
	root := a.sshRoot

	if err := rt.withSFTP(context.Background(), func(sess *sftpSession) error {
		infos, err := sess.client.ReadDir(root)
		if err != nil {
			return err
		}
		out := &searchStream{ctx: context.Background(), w: &failingWriter{h: http.Header{}}}
		walk := &searchWalk{
			ctx:     context.Background(),
			sess:    sess,
			out:     out,
			root:    root,
			needle:  "app", // 根目录下的 notes/APP.log 会命中（大小写不敏感）
			started: time.Now(),
		}
		// lastFlush 拨到过去：否则攒批要等够 50 条或 200ms，根目录这一层根本不会触发写入，
		// 用例就测不到「写失败」这条路径了。
		walk.buf = &searchBuf{stream: out, lastFlush: time.Now().Add(-time.Minute)}

		err = walk.run(infos)
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("写失败时 run 返回 %v，期望 errStreamClosed", err)
		}
		if walk.skipped != 0 {
			t.Fatalf("写失败被记成了「跳过 %d 个目录」：日志与收尾统计都会指错方向", walk.skipped)
		}
		if !out.wrote {
			t.Fatal("这次写入没有走到响应头那一行：用例没有覆盖到目标路径")
		}
		return nil
	}); err != nil {
		t.Fatalf("withSFTP 执行失败：%v", err)
	}
}

// TestSafeRemoteEntryName 钉住 S9：远端给的名字要能安全地拼进路径。
func TestSafeRemoteEntryName(t *testing.T) {
	for _, name := range []string{"a", "a b", ".hidden", "名字", `a\b`, "a..b", "..a"} {
		if !safeRemoteEntryName(name) {
			t.Errorf("safeRemoteEntryName(%q) = false，期望 true", name)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b", "../x", "/abs", "x/"} {
		if safeRemoteEntryName(name) {
			t.Errorf("safeRemoteEntryName(%q) = true，期望 false（会拼出起点之外的路径）", name)
		}
	}
}

// stepCtx 是一个「前 n 次 Err() 返回 nil、之后返回 context.Canceled」的 context。
//
// 为什么要这种怪东西：取消造成的 `connection lost` 只能**刚好**发生在一次 scanDir 里——真实
// 时序没法在测试里摆出来。用它可以精确构造「循环顶部 ctx 还没结束 → 读目录失败 → 此时 ctx 已
// 结束」这一个交错，而那正是会误记 skipped 的那一格。
type stepCtx struct {
	left int
}

func (c *stepCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *stepCtx) Done() <-chan struct{}       { return nil }
func (c *stepCtx) Value(any) any               { return nil }
func (c *stepCtx) Err() error {
	if c.left <= 0 {
		return context.Canceled
	}
	c.left--
	return nil
}

// fakeDirInfo 是一个只用来「指向不存在的子目录」的 os.FileInfo（名字刻意不命中关键词，
// 因此不会走到 buildFsEntry）。
type fakeDirInfo struct{ name string }

func (f fakeDirInfo) Name() string       { return f.name }
func (f fakeDirInfo) Size() int64        { return 0 }
func (f fakeDirInfo) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (f fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (f fakeDirInfo) IsDir() bool        { return true }
func (f fakeDirInfo) Sys() any           { return nil }

// TestFsSearchCancelInducedReadFailureNotSkipped 钉住「取消时那条 connection lost 不算跳过目录」。
//
// 背景：用户点取消 → withSFTPStream 关掉 SFTP 连接让飞着的 ReadDir 立刻返回 → 那条错误既不是
// 写失败（不是 errStreamClosed），也不是远端权限问题。修之前它会被记成 `skipped` 并在日志里写成
// 「搜索跳过目录 …：connection lost」——排查的人会去远端找权限问题，而实际是我们自己断的。
func TestFsSearchCancelInducedReadFailureNotSkipped(t *testing.T) {
	a, sid := searchTestApp(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("拿不到会话运行时：%v", apiErr)
	}
	root := a.sshRoot

	if err := rt.withSFTP(context.Background(), func(sess *sftpSession) error {
		out := &searchStream{ctx: context.Background(), w: &countingWriter{rr: httptest.NewRecorder()}}
		ctx := &stepCtx{left: 1} // 循环顶部那一次 Err() 返回 nil，之后就是「已取消」
		walk := &searchWalk{
			ctx:     ctx,
			sess:    sess,
			out:     out,
			root:    root,
			needle:  "zzz-不会命中",
			started: time.Now(),
			buf:     &searchBuf{stream: out, lastFlush: time.Now()},
		}
		// 起点目录里有一个**不存在**的子目录：scanDir 会失败，而此刻 ctx 报告已取消。
		err := walk.run([]os.FileInfo{fakeDirInfo{name: "no-such-subdir"}})
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("取消引发的读失败返回 %v，期望 errStreamClosed（不是 errStreamClosed 就会被上面那条分支记成跳过目录）", err)
		}
		if walk.skipped != 0 {
			t.Fatalf("取消引发的读失败被记成了「跳过 %d 个目录」", walk.skipped)
		}
		return nil
	}); err != nil {
		t.Fatalf("withSFTP 执行失败：%v", err)
	}
}

// TestFsSearchReadDirBoundedRespectsDeadline 钉住 S1 的加固：**单个目录的读取**也受
// 「本次搜索的剩余时间」约束——超时用已读到的部分继续，并标 truncated。
//
// 判据的构造方式：把 walk.started 拨到 20 秒前（> searchDeadline），于是派生出的
// ReadDirContext 的 ctx 一开始就是到期的。修之前走的是 `Client.ReadDir`
// （内部 context.Background()），它**根本不看**这个时间；现在必须：
// ① 立刻返回（不真去读那个目录）、② truncated=true、③ 不报错。
func TestFsSearchReadDirBoundedRespectsDeadline(t *testing.T) {
	a, sid := searchTestApp(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("拿不到会话运行时：%v", apiErr)
	}
	if err := rt.withSFTP(context.Background(), func(sess *sftpSession) error {
		walk := &searchWalk{
			ctx:     context.Background(),
			sess:    sess,
			root:    a.sshRoot,
			started: time.Now().Add(-2 * searchDeadline), // 时间已用光
		}
		start := time.Now()
		infos, truncated, err := walk.readDirBounded(a.sshRoot)
		if err != nil {
			t.Fatalf("超时的那次读目录返回了错误：%v（期望「截断」而不是失败）", err)
		}
		if !truncated {
			t.Error("时间已用光时 truncated 应当为 true（界面要说「已截断」）")
		}
		if len(infos) != 0 {
			t.Errorf("ctx 一开始就到期时不该读到条目：%d 条", len(infos))
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("到期之后仍等了 %s：deadline 没有作用到读目录这一步", elapsed)
		}
		return nil
	}); err != nil {
		t.Fatalf("withSFTP 执行失败：%v", err)
	}
}

// TestFsSearchReadDirBoundedReportsClientGone 钉住另一半：客户端已经走了时，读目录返回
// errStreamClosed（而不是一个「远端读不出来」的错误，更不能被记成 skipped）。
func TestFsSearchReadDirBoundedReportsClientGone(t *testing.T) {
	a, sid := searchTestApp(t)
	rt, apiErr := a.mux.sessions.runtimeFor(sid)
	if apiErr != nil {
		t.Fatalf("拿不到会话运行时：%v", apiErr)
	}
	if err := rt.withSFTP(context.Background(), func(sess *sftpSession) error {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 用户点了「取消」
		walk := &searchWalk{ctx: ctx, sess: sess, root: a.sshRoot, started: time.Now()}
		infos, _, err := walk.readDirBounded(a.sshRoot)
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("客户端已取消时返回 %v，期望 errStreamClosed", err)
		}
		if infos != nil {
			t.Errorf("已取消时不该读到条目：%d 条", len(infos))
		}
		return nil
	}); err != nil {
		t.Fatalf("withSFTP 执行失败：%v", err)
	}
}
