# clinepass-channel-monitor - CLIProxyAPI plugin
#
# Builds a C ABI shared library. The Go toolchain and its caches live inside the
# repository by default so a build never depends on the ambient environment.

SHELL := /bin/bash

REPO_ROOT  := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
GO_VERSION ?= 1.26.0
GO_BIN     ?= $(REPO_ROOT)/.toolchain/go/bin/go
GOFLAGS    ?=
# Caches stay inside the repository: the host filesystem is not always writable.
export GOCACHE    := $(REPO_ROOT)/.toolchain/gocache
export GOMODCACHE := $(REPO_ROOT)/.toolchain/gomodcache
export GOPATH     := $(REPO_ROOT)/.toolchain/gopath
export GOTMPDIR   := $(REPO_ROOT)/.toolchain/gotmp
export CGO_ENABLED := 1

PLUGIN_ID  := clinepass-channel-monitor
# VERSION is the released version; DEV_BUMP makes each local build a different version
# so that CPA always hot-reloads the plugin instead of keeping the previously loaded
# library (replacement is keyed on the plugin file path, not on its content).
VERSION    ?= $(shell grep -oP 'pluginVersion\s*=\s*"\K[^"]+' $(REPO_ROOT)/plugin.go)
DEV_BUMP   ?= 1
BUMP_FILE  := $(REPO_ROOT)/.toolchain/.dev-build
BUILD_VER  := $(shell if [ "$(DEV_BUMP)" = "1" ]; then \
	base="$(VERSION)"; n=$$(cat $(BUMP_FILE) 2>/dev/null || echo 0); n=$$((n+1)); \
	mkdir -p $(dir $(BUMP_FILE)); echo $$n > $(BUMP_FILE); \
	echo "$${base}-dev.$${n}"; else echo "$(VERSION)"; fi)
GOOS       ?= linux
GOARCH     ?= $(shell $(GO_BIN) env GOARCH 2>/dev/null || echo arm64)
BUILD_DIR  := $(REPO_ROOT)/dist

LIB_NAME   := $(PLUGIN_ID)-v$(BUILD_VER).so
LDFLAGS    := -s -w -X main.pluginVersion=$(BUILD_VER)

# Where the CPA container reads plugins from. Override for your own deployment.
INSTALL_DIR ?= /opt/cpa/plugins/$(GOOS)/$(GOARCH)

.PHONY: all deps build strip test vet fmt lint clean install uninstall tools help

all: build

## deps: download Go modules
deps:
	$(GO_BIN) mod download

## build: build the plugin shared library for GOOS/GOARCH
build: $(BUILD_DIR)/$(LIB_NAME)

$(BUILD_DIR)/$(LIB_NAME): $(wildcard $(REPO_ROOT)/*.go) $(REPO_ROOT)/go.mod $(REPO_ROOT)/cdecl.h
	@mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO_BIN) build $(GOFLAGS) -trimpath \
		-ldflags '$(LDFLAGS)' -buildmode=c-shared -o $@ .
	@rm -f $(BUILD_DIR)/$(PLUGIN_ID).h
	@echo "built $@"

## test: run unit tests
test:
	$(GO_BIN) test ./...

## vet: run go vet
vet:
	$(GO_BIN) vet ./...

## fmt: format Go sources
fmt:
	$(GO_BIN) fmt ./...

## clean: remove build output
clean:
	rm -rf $(BUILD_DIR)

## install: install the plugin into the CPA plugins directory and reload CPA
install: build
	@if [ ! -d "$(INSTALL_DIR)" ]; then \
		echo "install directory $(INSTALL_DIR) does not exist."; \
		echo "create it first, for example:"; \
		echo "  sudo install -d -o \$$(id -un) -g \$$(id -gn) -m 755 $(INSTALL_DIR)"; \
		exit 1; \
	fi
	install -m 0644 $(BUILD_DIR)/$(LIB_NAME) $(INSTALL_DIR)/$(LIB_NAME)
	@echo "installed $(INSTALL_DIR)/$(LIB_NAME)"
	@echo "CPA reloads plugins when its configuration changes; touch the config file to force a rescan."

## uninstall: remove the installed plugin and the copy of the project
uninstall:
	rm -f $(INSTALL_DIR)/$(LIB_NAME)
	@echo "removed $(INSTALL_DIR)/$(LIB_NAME)"

## tools: install the Go toolchain into .toolchain/go (no root required)
tools:
	@bash $(REPO_ROOT)/scripts/install-go.sh $(GO_VERSION)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
