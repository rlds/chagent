.PHONY: all build build-all run install uninstall uninstall-all clean test test-lean vet fmt lint lint-docs fix deps update-deps check help

# ---------------------------------------------------------------------------
# Chagent (chess base agent) Makefile
#
# This repository ships a single chess-facing gateway binary. Targets for the
# removed Web launcher, Docker images, Android/macOS bundles, WhatsApp builds and
# the memory benchmark were dropped together with their sources (T7.4).
# ---------------------------------------------------------------------------

BINARY_NAME=chagent
BUILD_DIR=bin
CMD_DIR=cmd/$(BINARY_NAME)
EXT=

# Go toolchain / build settings
GO?=go
CGO_ENABLED?=0
GO_BUILD_TAGS?=goolm,stdjson
GOFLAGS?=-v -tags $(GO_BUILD_TAGS)
GOCACHE?=$(CURDIR)/.cache/go-build
GOMODCACHE?=$(CURDIR)/.cache/go-mod
GOTOOLCHAIN?=local
export CGO_ENABLED
export GOCACHE
export GOMODCACHE
export GOTOOLCHAIN

# Version metadata injected into pkg/config
ifeq ($(OS),Windows_NT)
	VERSION_RAW:=$(strip $(shell git describe --tags --always --dirty 2>NUL))
	GIT_COMMIT_RAW:=$(strip $(shell git rev-parse --short=8 HEAD 2>NUL))
	BUILD_TIME_RAW:=$(strip $(shell powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ssK'"))
	GO_VERSION_RAW:=$(strip $(shell go env GOVERSION 2>NUL))
	EXT=.exe
else
	VERSION_RAW:=$(strip $(shell git describe --tags --always --dirty 2>/dev/null))
	GIT_COMMIT_RAW:=$(strip $(shell git rev-parse --short=8 HEAD 2>/dev/null))
	BUILD_TIME_RAW:=$(strip $(shell date +%FT%T%z))
	GO_VERSION_RAW:=$(strip $(shell go env GOVERSION 2>/dev/null))
endif
VERSION?=$(if $(VERSION_RAW),$(VERSION_RAW),dev)
GIT_COMMIT=$(if $(GIT_COMMIT_RAW),$(GIT_COMMIT_RAW),dev)
BUILD_TIME=$(if $(BUILD_TIME_RAW),$(BUILD_TIME_RAW),dev)
GO_VERSION=$(if $(GO_VERSION_RAW),$(firstword $(GO_VERSION_RAW)),unknown)
CONFIG_PKG=github.com/rlds/chagent/pkg/config
LDFLAGS=-X $(CONFIG_PKG).Version=$(VERSION) -X $(CONFIG_PKG).GitCommit=$(GIT_COMMIT) -X $(CONFIG_PKG).BuildTime=$(BUILD_TIME) -X $(CONFIG_PKG).GoVersion=$(GO_VERSION) -s -w

# Platform detection (used for the build artifact name only)
UNAME_S?=$(shell uname -s)
UNAME_M?=$(shell uname -m)
ifeq ($(UNAME_S),Linux)
	PLATFORM=linux
	ifeq ($(UNAME_M),x86_64)
		ARCH=amd64
	else ifeq ($(UNAME_M),aarch64)
		ARCH=arm64
	else
		ARCH=$(UNAME_M)
	endif
else ifeq ($(UNAME_S),Darwin)
	PLATFORM=darwin
	ifeq ($(UNAME_M),x86_64)
		ARCH=amd64
	else
		ARCH?=arm64
	endif
else ifeq ($(OS),Windows_NT)
	PLATFORM=windows
	ARCH?=amd64
	EXT=.exe
else
	PLATFORM=$(UNAME_S)
	ARCH?=$(UNAME_M)
endif

ifneq ($(strip $(GOOS)),)
	PLATFORM:=$(GOOS)
endif
ifneq ($(strip $(GOARCH)),)
	ARCH:=$(GOARCH)
endif
ifeq ($(PLATFORM),windows)
	EXT=.exe
endif

BINARY_PATH=$(BUILD_DIR)/$(BINARY_NAME)-$(PLATFORM)-$(ARCH)

# Installation and data directories
INSTALL_PREFIX?=$(HOME)/.local
INSTALL_BIN_DIR=$(INSTALL_PREFIX)/bin
INSTALL_TMP_SUFFIX=.new
CHAGENT_HOME?=$(HOME)/.chagent
WORKSPACE_DIR?=$(CHAGENT_HOME)/workspace

## all: Build the chagent binary for the current platform (default)
all: build

## build: Build the chagent binary for the current platform
build:
	@echo "Building $(BINARY_NAME)$(EXT) for $(PLATFORM)/$(ARCH)..."
ifeq ($(OS),Windows_NT)
	@powershell -NoProfile -Command "New-Item -ItemType Directory -Force -Path '$(BUILD_DIR)' | Out-Null"
	@$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_PATH)$(EXT) ./$(CMD_DIR)
	@powershell -NoProfile -Command "Copy-Item -LiteralPath '$(BINARY_PATH)$(EXT)' -Destination '$(BUILD_DIR)/$(BINARY_NAME)$(EXT)' -Force"
else
	@mkdir -p $(BUILD_DIR)
	@GOOS=$(PLATFORM) GOARCH=$(ARCH) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_PATH)$(EXT) ./$(CMD_DIR)
	@ln -sf $(BINARY_NAME)-$(PLATFORM)-$(ARCH)$(EXT) $(BUILD_DIR)/$(BINARY_NAME)$(EXT)
endif
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)$(EXT)"

## build-all: Cross-compile the chess gateway binary for common platforms
build-all:
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./$(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./$(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./$(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./$(CMD_DIR)
	@echo "Core builds complete"

## run: Build and run the chagent binary (pass ARGS="..." for flags)
run: build
	@$(BUILD_DIR)/$(BINARY_NAME) $(ARGS)

## install: Install the built binary to $(INSTALL_PREFIX)/bin
install: build
	@echo "Installing $(BINARY_NAME)..."
	@mkdir -p $(INSTALL_BIN_DIR)
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@chmod +x $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@mv -f $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX) $(INSTALL_BIN_DIR)/$(BINARY_NAME)
	@echo "Installed binary to $(INSTALL_BIN_DIR)/$(BINARY_NAME)"

## uninstall: Remove the installed binary
uninstall:
	@rm -f $(INSTALL_BIN_DIR)/$(BINARY_NAME)

## uninstall-all: Remove the installed binary and all local data (workspace/config)
uninstall-all: uninstall
	@echo "Removing workspace and config: $(CHAGENT_HOME)"
	@rm -rf $(CHAGENT_HOME)

## clean: Remove build artifacts
clean:
	@echo "Cleaning build artifacts..."
ifeq ($(OS),Windows_NT)
	@powershell -NoProfile -Command "if (Test-Path -LiteralPath '$(BUILD_DIR)') { Remove-Item -LiteralPath '$(BUILD_DIR)' -Recurse -Force }"
else
	@rm -rf $(BUILD_DIR)
endif

## test: Run all Go tests
test:
	@$(GO) test ./... -count=1

## test-lean: Build + run the agent-scoped test packages only
test-lean:
	@./scripts/lean-test.sh

## vet: Run go vet over the repository
vet:
	@$(GO) vet ./...

## fmt: Format Go code with gofmt
fmt:
	@gofmt -w $$(git ls-files -- '*.go')

## lint-docs: Check documentation layout and naming conventions
lint-docs:
	@./scripts/lint-docs.sh

## lint: Run vet and documentation lint
lint: vet lint-docs

## fix: Apply gofmt formatting
fix: fmt

## deps: Download and verify module dependencies
deps:
	@$(GO) mod download
	@$(GO) mod verify

## update-deps: Update dependencies (requires review before commit)
update-deps:
	@$(GO) get -u ./...
	@$(GO) mod tidy

## check: deps + vet + tests + documentation lint
check: deps vet test lint-docs

## help: Show this help message
help:
	@echo "chagent Makefile"
	@echo ""
	@echo "Usage:"
	@echo "  make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sort | awk -F': ' '{printf "  %-16s %s\n", substr($$1, 4), $$2}'
	@echo ""
	@echo "Environment variables:"
	@echo "  INSTALL_PREFIX   # installation prefix (default: ~/.local)"
	@echo "  CHAGENT_HOME    # data root (default: ~/.chagent)"
	@echo "  WORKSPACE_DIR    # workspace (default: \$$CHAGENT_HOME/workspace)"
	@echo "  VERSION          # version string (default: git describe)"
	@echo ""
	@echo "Current configuration:"
	@echo "  Platform: $(PLATFORM)/$(ARCH)"
	@echo "  Binary:   $(BINARY_PATH)"