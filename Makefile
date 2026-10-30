# NetBootX 构建脚本
# 用法：
#   make build     编译出 bin/netbootx
#   make test      跑全部测试
#   make run       编译并启动
#   make clean     清理产物

GO      := ./go
BIN     := bin/netbootx
LDFLAGS := -s -w

.PHONY: all build test test-verbose run clean fmt vet check install help

all: build

## build: 编译二进制
build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .
	@echo "✅ 编译完成: $(BIN) ($$(du -h $(BIN) | cut -f1))"

## test: 跑全部单元测试
test:
	$(GO) test ./... -count=1 -timeout 300s

## test-verbose: 带详细输出
test-verbose:
	$(GO) test ./... -count=1 -v -timeout 300s

## vet: 静态检查
vet:
	$(GO) vet ./...

## fmt: 格式化
fmt:
	$(GO) fmt ./...

## check: vet + test + build 一条龙
check: vet test build

## run: 编译并启动（需要 sudo 才能绑 67/69 端口）
run: build
	sudo ./$(BIN)

## run-nopriv: 用高位端口跑，不需要 sudo（但客户机连不上）
run-nopriv: build
	./$(BIN) -dhcp 16767 -tftp 16969 -http 16000 -web 16080

## clean: 清理
clean:
	rm -rf bin
	@echo "🧹 已清理"

## install: 安装到 /usr/local/bin
install: build
	install -m 0755 $(BIN) /usr/local/bin/netbootx
	@echo "✅ 已安装到 /usr/local/bin/netbootx"

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
