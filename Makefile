# clinepass-channel-monitor - CLIProxyAPI plugin
#
# Builds a C ABI shared library. The Go toolchain and its caches live inside the
# repository by default so a build never depends on the ambient environment.

SHELL := /bin/bash

REPO_ROOT  := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
# Exact toolchain version installed into .toolchain/go by `make tools` and used by
# every other target. Keep it in sync with the toolchain that is actually installed
# (`$(GO_BIN) version`) and with the default in scripts/install-go.sh.
GO_VERSION ?= 1.27.1
GO_BIN     ?= $(REPO_ROOT)/.toolchain/go/bin/go
GOFLAGS    ?=
# Caches stay inside the repository: the host filesystem is not always writable.
export GOCACHE    := $(REPO_ROOT)/.toolchain/gocache
export GOMODCACHE := $(REPO_ROOT)/.toolchain/gomodcache
export GOPATH     := $(REPO_ROOT)/.toolchain/gopath
export GOTMPDIR   := $(REPO_ROOT)/.toolchain/gotmp
export CGO_ENABLED := 1

PLUGIN_ID  := clinepass-channel-monitor
# The version declared in buildinfo.go is the release version; local builds append
# -dev.N so that CPA always hot-reloads the plugin instead of keeping the previously
# loaded library (replacement is keyed on the plugin file path, not on its content).
# sed instead of grep -P keeps this working on macOS/BSD, where the release workflow runs.
DECLARED_VERSION := $(shell sed -n 's/.*Version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' $(REPO_ROOT)/internal/buildinfo/buildinfo.go | head -1)
VERSION    ?= $(if $(DECLARED_VERSION),$(DECLARED_VERSION),0.1.0)
DEV_BUMP   ?= 1
BUMP_FILE  := $(REPO_ROOT)/.toolchain/.dev-build
BUILD_VER  := $(shell if [ "$(DEV_BUMP)" = "1" ]; then \
	base="$(VERSION)"; n=$$(cat $(BUMP_FILE) 2>/dev/null || echo 0); n=$$((n+1)); \
	mkdir -p $(dir $(BUMP_FILE)); echo $$n > $(BUMP_FILE); \
	echo "$${base}-dev.$${n}"; else echo "$(VERSION)"; fi)
GOOS       ?= linux
GOARCH     ?= $(shell $(GO_BIN) env GOARCH 2>/dev/null || echo arm64)
BUILD_DIR  := $(REPO_ROOT)/dist
# Release artifacts (scripts/release.sh): per-platform zips plus checksums.txt.
RELEASE_DIR := $(REPO_ROOT)/release

LIB_NAME   := $(PLUGIN_ID)-v$(BUILD_VER).so
LDFLAGS    := -s -w -X github.com/wkeking/clinepass-channel-monitor/internal/buildinfo.Version=$(BUILD_VER)

# Where the CPA container reads plugins from. Override for your own deployment.
INSTALL_DIR ?= /opt/cpa/plugins/$(GOOS)/$(GOARCH)

.PHONY: all deps build strip test bench vet fmt lint clean clean-cache install uninstall tools help toolchain-dirs

all: build

## toolchain-dirs: create the in-repo Go cache directories that a fresh clone (CI included) does not have
toolchain-dirs:
	@mkdir -p $(GOCACHE) $(GOMODCACHE) $(GOPATH) $(GOTMPDIR)

## deps: download Go modules
deps: toolchain-dirs
	$(GO_BIN) mod download

## build: build the plugin shared library for GOOS/GOARCH
build: $(BUILD_DIR)/$(LIB_NAME)

$(BUILD_DIR)/$(LIB_NAME): $(shell find $(REPO_ROOT)/cmd $(REPO_ROOT)/internal -name '*.go') $(REPO_ROOT)/go.mod $(REPO_ROOT)/cmd/clinepass-channel-monitor/cdecl.h | toolchain-dirs
	@mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO_BIN) build $(GOFLAGS) -trimpath \
		-ldflags '$(LDFLAGS)' -buildmode=c-shared -o $@ ./cmd/clinepass-channel-monitor
	@rm -f $(BUILD_DIR)/$(PLUGIN_ID).h
	@echo "built $@"

## test: run unit tests
test: toolchain-dirs
	$(GO_BIN) test ./...

## bench: run the request-path benchmarks (hook cost, hashing, event marshalling)
bench: toolchain-dirs
	$(GO_BIN) test -run '^$$' -bench . -benchmem -benchtime 200x .

## vet: run go vet
vet: toolchain-dirs
	$(GO_BIN) vet ./...

## fmt: format Go sources
fmt: toolchain-dirs
	$(GO_BIN) fmt ./...

## clean: remove build output and release artifacts
clean:
	rm -rf $(BUILD_DIR) $(RELEASE_DIR)

## clean-cache: empty the Go build cache and temp dir (keeps the toolchain, the module cache and dist/)
clean-cache:
	@freed=$$(du -sm $(GOCACHE) $(GOTMPDIR) 2>/dev/null | awk '{s+=$$1} END {print s+0}'); \
	rm -rf $(GOCACHE) $(GOTMPDIR); \
	mkdir -p $(GOCACHE) $(GOTMPDIR); \
	echo "clean-cache: freed $${freed} MB (.toolchain/gocache, .toolchain/gotmp)"
	@echo "clean-cache: kept $(GO_BIN) and $(GOMODCACHE), so the next build stays offline-capable"

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

## tools: install the pinned Go toolchain (GO_VERSION = 1.27.1) into .toolchain/go (no root required)
tools:
	@bash $(REPO_ROOT)/scripts/install-go.sh $(GO_VERSION)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
