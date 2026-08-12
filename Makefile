.PHONY: build clean dist

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN := termcp-$(GOOS)-$(GOARCH)

## 平台特定可执行后缀（Windows 加 .exe）
ifeq ($(GOOS),windows)
BIN := $(BIN).exe
endif

# 构建目标
build:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o dist/$(BIN) .

# 清理构建文件
clean:
	rm -rf dist

# 构建并清理旧文件
dist: clean build
