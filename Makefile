# Makefile: 构建 Vue 前端 → 把静态资源 embed 进 Go 后端 → 产出单文件自包含二进制。
#
#   make                   # dev（默认，不裁剪符号）
#   make release V=1.0.1   # release（-s -w 裁剪 + 版本号覆盖）
#   make test              # 后端单元测试（含内嵌 SSH/SFTP 测试服务器）
#   make typecheck         # 前端 vue-tsc 类型检查
#   make clean             # 清理构建产物
#
# 产物是项目根目录的 ./webssh（前端 dist 经 //go:embed 打进 backend/embed 编译进去）。
# 进程只监听 unix socket（WEBSSH_ADMIN_SOCK），由 nginx / 飞牛统一网关按
# WEBSSH_ADMIN_BASEURL 反代；baseurl 由后端在运行时注入 <base href>，不烧进产物。
#
# Go 缓存/模块默认放项目本地目录；环境中已设 GOCACHE/GOPATH 时沿用环境值。

ROOT      := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
BACKEND   := $(ROOT)/backend
FRONTEND  := $(ROOT)/frontend
EMBED     := $(BACKEND)/embed
OUT       := $(ROOT)/webssh

# 版本号：默认 1.0.0，可经 V 覆盖（如 make release V=1.0.1）。
version   ?= 1.0.0
V         ?= $(version)

GOCACHE   ?= $(ROOT)/.gocache
GOPATH    ?= $(ROOT)/.gopath
# 模块缓存也放项目本地：一是与 GOCACHE/GOPATH 同一套约定（环境里已设则沿用），
# 二是飞牛上默认的 $GOPATH/pkg/mod 可能落在别的卷上、权限/模式位不可靠。
GOMODCACHE ?= $(ROOT)/.gomodcache
# go build 的工作目录走 os.TempDir()（$TMPDIR 或 /tmp）。飞牛的存储卷上 /tmp 权限受限时
# 会报 "open /tmp/go-build.../exe/a.out: permission denied"，指到项目本地即可。
TMPDIR    ?= $(ROOT)/.tmp
export GOCACHE GOPATH GOMODCACHE TMPDIR

# 纯 Go，不需要 cgo：产物静态链接（真正的单文件交付），且冷编译不再走 runtime/cgo
# （那一步写的 .o 中间文件在本卷上会被损坏）。环境里已设 CGO_ENABLED 则沿用。
CGO_ENABLED ?= 0
export CGO_ENABLED

# -buildvcs=false：仓库可能没有 .git，或者构建环境不允许读它。
export GOFLAGS := -buildvcs=false $(GOFLAGS)

# fnOS 上 Node 由应用提供，不一定在 PATH 里。取版本号最高的那一个，可用 NODE_BIN 覆盖；
# 一个都没有（别的构建机）就什么都不加——不能退化成 `PATH=:$(PATH)`。
NODE_BIN ?= $(shell ls -d /var/apps/nodejs_v*/target/bin 2>/dev/null | sort -V | tail -1)
ifneq ($(strip $(NODE_BIN)),)
export PATH := $(NODE_BIN):$(PATH)
endif

.PHONY: all dev release build test typecheck clean

all: dev

# 前端产物 / node_modules 已在 ROOT/.gitignore 里；并行执行会让 build 的两步交叉。
.NOTPARALLEL:

dev: ## dev 构建（默认，不裁剪符号）
	$(MAKE) build LDFLAGS="-X main.appVersion=$(V)"

release: ## release 构建（裁剪符号）
	$(MAKE) build LDFLAGS="-s -w -X main.appVersion=$(V)"

build:
	@echo "==> Building frontend..."
	cd "$(FRONTEND)" && npm install --no-audit --no-fund && npm run build
	@echo "==> Copying frontend dist into embed dir..."
	rm -rf "$(EMBED)"
	mkdir -p "$(EMBED)"
	tar -C "$(FRONTEND)/dist" -cf - . | tar -C "$(EMBED)" -xf -
	@echo "==> Building Go binary..."
	mkdir -p "$(GOCACHE)" "$(GOPATH)" "$(GOMODCACHE)" "$(TMPDIR)"
	# 清一次编译缓存：保证代码与 embed 资产的变化一定被编进去。
	go clean -cache
	cd "$(BACKEND)" && go build -trimpath -ldflags "$(LDFLAGS)" -o "$(OUT)" .
	# 飞牛存储卷上新文件的模式位不可靠，显式补执行位（否则可能拿到一个跑不起来的 0000）。
	chmod 755 "$(OUT)"
	@echo "==> Cleaning embed dir..."
	rm -rf "$(EMBED)"
	@echo "==> Done: $(OUT)"
	ls -lh "$(OUT)"

# 后端测试。main.go 用 //go:embed all:embed 打包前端产物，而该目录构建后被清空，
# 因此测试前先补一个占位文件（否则包无法编译）；占位内容不影响测试。
test:
	mkdir -p "$(EMBED)"
	@test -e "$(EMBED)/index.html" || echo "placeholder for //go:embed (see Makefile test target)" > "$(EMBED)/index.html"
	cd "$(BACKEND)" && go test ./...

typecheck:
	cd "$(FRONTEND)" && npm install --no-audit --no-fund && npm run type-check

# 飞牛应用包（fpk）的脚手架**不在本仓库**：打包工程由 fpkstudio 维护
# （`/vol1/@appshare/fpkstudio/projects/WebSSH`）。本仓库只产出 ./webssh，
# 打包时把该二进制放进工程的 app/bin/ 即可；manifest 的硬性字段见 README 第 4 节。
clean:
	rm -rf "$(EMBED)"
	rm -f "$(OUT)"
