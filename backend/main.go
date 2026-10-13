package main

import (
	"embed"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// embeddedFS 是构建时打进二进制的**前端产物**（Makefile 把 frontend/dist 复制到
// backend/embed 后编译）。构建结束 Makefile 会删掉该目录，因此 `make test` 会先补一个
// 占位 index.html（见 Makefile 的 test 目标）。
//
//go:embed all:embed
var embeddedFS embed.FS

// appVersion 由构建时注入：-ldflags "-X main.appVersion=1.0.0"。默认 dev，
// 前端设置弹窗的「关于」与 /api/info 都会显示它。
var appVersion = "dev"

// bootID 是本次进程实例的随机标识（每次启动都不同），经 /api/info 的 bootId 下发。
//
// 前端把它当作「后端是否换过进程」的唯一判据：进程重启后所有内存态会话（终端 + SFTP）
// 都不复存在，页面必须清空会话标签（配置仍在，可以重新打开），而不是拿旧 id 去挂载
// 一个已经消失的终端。
var bootID = newID()

func main() {
	renv := loadRuntimeEnv()

	// 1) 准备数据目录 + 主密钥 + 已存配置。失败即退出：没有数据目录就没有会话配置，
	//    「活着但一打开就报错」比直接退出更难排查（supervisor 能看见退出码）。
	mux, err := newAdminMux(renv)
	if err != nil {
		warnf("初始化失败：%v", err)
		os.Exit(1)
	}
	mux.SetSPA(embeddedFS)

	// 2) socket 已经被占用时退出：两个实例抢同一个 socket 会互相删对方的文件，
	//    表现为「界面时好时坏」，而两个进程各自都觉得自己正常。
	if _, err := net.Dial("unix", renv.AdminSock); err == nil {
		warnf("admin socket %s 已被占用，退出", renv.AdminSock)
		os.Exit(1)
	}

	// 3) **先绑定成功再进信号等待**：绑定失败就退出（而不是空转）。
	ln, err := listenAdminSocket(renv)
	if err != nil {
		warnf("绑定 admin socket %s 失败：%v", renv.AdminSock, err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: mux.buildHandler()}
	srvErr := make(chan error, 1)
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			srvErr <- serr
		}
	}()

	// 4) 等待退出信号（或 accept 彻底失败：此时已经没有可用入口，按进程退出处理）。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	exitCode := 0
	select {
	case <-sig:
	case serr := <-srvErr:
		warnf("admin socket 服务异常退出：%v", serr)
		exitCode = 1
	}

	// 5) 优雅退出：先停入口（不再接受新请求）→ 终止全部终端与 SFTP 连接 → 删 socket。
	//    **保留数据目录**：会话配置（含加密凭据）必须活过重启，删掉等于让用户每次
	//    重启都要重填密码。
	//
	// 注意 srv.Close() 不会关闭被 hijack 的连接（WebSocket），所以必须先 closeAll()：
	// 终端关闭时会关掉挂载中的连接，WS handler 随之退出。
	_ = srv.Close()
	mux.sessions.closeAll()
	if err := os.Remove(renv.AdminSock); err != nil && !errors.Is(err, os.ErrNotExist) {
		warnf("删除 socket %s 失败：%v", renv.AdminSock, err)
	}
	os.Exit(exitCode)
}
