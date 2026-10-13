// testsshd 是 testssh 的命令行入口，**只用于本地联调**（不作为主程序的一部分，
// 主程序不引用它）。例：
//
//	go run ./internal/testssh/cmd/testsshd -addr 127.0.0.1:2222 -user demo -password demo -root /tmp/webssh-demo
//
// 它会打印主机密钥指纹：浏览器里「首次连接需要信任」的那一串应该和这里一致，
// 端到端验证 TOFU 时直接对照即可。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"webssh/backend/internal/testssh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "监听地址（默认只绑回环）")
	user := flag.String("user", "demo", "登录用户名")
	password := flag.String("password", "demo", "登录密码")
	root := flag.String("root", "", "SFTP 根目录（同时是 shell 的工作目录）；留空则在系统临时目录下新建一个")
	shell := flag.String("shell", "/bin/bash", "要启动的 shell")
	allowRemote := flag.Bool("allow-remote", false, "允许绑定非回环地址（危险：等于把当前用户的 shell 暴露到网络上）")
	flag.Parse()

	if !*allowRemote {
		host, _, err := net.SplitHostPort(*addr)
		if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			fmt.Fprintf(os.Stderr, "拒绝绑定 %s：本服务器会给出一个真实 shell，只允许绑回环地址；确实需要请加 -allow-remote\n", *addr)
			os.Exit(2)
		}
	}

	rootDir := *root
	cleanup := func() {}
	if rootDir == "" {
		dir, err := os.MkdirTemp("", "webssh-testssh-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建临时根目录失败：%v\n", err)
			os.Exit(1)
		}
		rootDir, cleanup = dir, func() {}
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "准备根目录 %s 失败：%v\n", rootDir, err)
		os.Exit(1)
	}
	defer cleanup()

	srv, err := testssh.Start(testssh.Options{
		Addr:     *addr,
		User:     *user,
		Password: *password,
		Root:     rootDir,
		Shell:    *shell,
		Logger:   func(format string, args ...any) { log.Printf("[testsshd] "+format, args...) },
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("testsshd 已启动（仅供本地联调）\n")
	fmt.Printf("  地址        : %s\n", srv.Addr())
	fmt.Printf("  用户名/密码 : %s / %s\n", *user, *password)
	fmt.Printf("  shell       : %s（交互式，不走 rc 文件）\n", *shell)
	fmt.Printf("  SFTP 根目录 : %s\n", srv.Root())
	fmt.Printf("  主机密钥    : %s %s\n", srv.KeyType(), srv.Fingerprint())
	fmt.Printf("  （浏览器首次连接时应显示同一串指纹；确认一致后再点「信任并继续」）\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	srv.Close()
	fmt.Printf("testsshd 已退出\n")
}
