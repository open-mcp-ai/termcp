.PHONY: build build-debug clean dist

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN := termcp-$(GOOS)-$(GOARCH)

LDFLAGS_RELEASE := -s -w

# release 模式：-s 去掉符号表，-w 去掉 DWARF 调试信息，-trimpath 去掉本机路径等个人信息
LDFLAGS_RELEASE := -s -w

# 构建目标（默认 release）
build:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS_RELEASE)" -o dist/$(BIN) .

# 调试模式构建（保留符号信息，便于 delve 调试）
build-debug:
	mkdir -p dist
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -gcflags "all=-N -l" -o dist/$(BIN) .

# 清理构建文件
clean:
	rm -rf dist

# 构建并清理旧文件
dist: clean build
