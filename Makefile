SHELL := /bin/bash

CODEX ?= codex
CLAUDE ?= claude
QWEN ?= qwen
# Ignore an inherited GROK environment variable: a long-lived peer may have
# pinned its own launcher, but that must not disable discovery for a later
# install. An explicit make command-line GROK=/absolute/path pins one candidate.
GROK_INPUT_ORIGIN := $(origin GROK)
GROK ?=
GROK_INSTALL_VALUE := $(if $(and $(findstring command line,$(GROK_INPUT_ORIGIN)),$(strip $(GROK))),$(GROK),grok)
GOLANGCI_LINT_VERSION ?= v2.12.2
TOOLS_BIN_DIR ?= $(CURDIR)/bin/tools
GOLANGCI_LINT ?= $(TOOLS_BIN_DIR)/golangci-lint
PREFIX ?= $(HOME)/.local
HOST_RELEASE_VERSION := $(shell cat deploy/agent-sessions/VERSION)
HOST_LDFLAGS ?= -s -w -X main.version=$(HOST_RELEASE_VERSION)
HUB_LDFLAGS ?= -s -w -X main.version=$(HOST_RELEASE_VERSION)

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Linux)
HOST_GOOS := linux
else ifeq ($(UNAME_S),Darwin)
HOST_GOOS := darwin
else
$(error unsupported host OS $(UNAME_S); expected Linux or Darwin)
endif
ifeq ($(UNAME_M),x86_64)
HOST_GOARCH := amd64
else ifeq ($(UNAME_M),amd64)
HOST_GOARCH := amd64
else ifeq ($(UNAME_M),arm64)
HOST_GOARCH := arm64
else ifeq ($(UNAME_M),aarch64)
HOST_GOARCH := arm64
else
$(error unsupported host architecture $(UNAME_M))
endif

GOOS ?= $(HOST_GOOS)
GOARCH ?= $(HOST_GOARCH)

ifeq ($(GOARCH),amd64)
PLATFORM_ARCH := x64
else ifeq ($(GOARCH),arm64)
PLATFORM_ARCH := arm64
else
$(error unsupported GOARCH $(GOARCH); expected amd64 or arm64)
endif

PLATFORM := $(GOOS)-$(PLATFORM_ARCH)
BIN_DIR := $(CURDIR)/bin/$(PLATFORM)
PREBUILT_RELEASE_MARKER := $(CURDIR)/.agent-sessions-prebuilt
BINARY_NAMES := $(shell ./scripts/release-inventory binaries)

.PHONY: all lint lint-tool test test-race build install dev-install reinstall install-all dev-install-all \
	install-hub remove-all remove-hub purge-all repair-projection clean

.PHONY: release-inventory build-release-platform

release-inventory:
	@./scripts/release-inventory binaries
	@./scripts/release-inventory plugins

build-release-platform:
	@test -n "$(RELEASE_OUTPUT_DIR)" || { printf 'RELEASE_OUTPUT_DIR is required\n' >&2; exit 2; }
	@test -n "$(RELEASE_VERSION)" || { printf 'RELEASE_VERSION is required\n' >&2; exit 2; }
	@test "$(RELEASE_VERSION)" = "$$(cat deploy/agent-sessions/VERSION)" || { \
		printf 'release version %s does not match deploy/agent-sessions/VERSION\n' "$(RELEASE_VERSION)" >&2; exit 1; \
	}
	$(MAKE) build GOOS="$(GOOS)" GOARCH="$(GOARCH)"
	CGO_ENABLED=0 GOOS="$(HOST_GOOS)" GOARCH="$(HOST_GOARCH)" \
		./scripts/package-release "$(PLATFORM)" "$(RELEASE_VERSION)" "$(BIN_DIR)" "$(RELEASE_OUTPUT_DIR)"

all: lint test build

lint-tool:
	@if [[ "$(GOLANGCI_LINT)" == "$(TOOLS_BIN_DIR)/golangci-lint" ]]; then \
		if [[ ! -x "$(GOLANGCI_LINT)" ]] || ! "$(GOLANGCI_LINT)" version | grep -Fq 'version $(patsubst v%,%,$(GOLANGCI_LINT_VERSION)) '; then \
			command -v go >/dev/null 2>&1 || { printf 'Go is required to install the repository-managed linter\n' >&2; exit 127; }; \
			mkdir -p "$(TOOLS_BIN_DIR)"; \
			GOBIN="$(TOOLS_BIN_DIR)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		fi; \
	fi

lint: lint-tool
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run

test:
	./scripts/test

test-race:
	RACE=1 ./scripts/test

build:
	mkdir -p "$(BIN_DIR)"
	@if [[ -f "$(PREBUILT_RELEASE_MARKER)" ]]; then \
		for binary in $(BINARY_NAMES); do \
			[[ -x "$(BIN_DIR)/$$binary" ]] || { \
				printf 'This release archive does not contain prebuilt %s/%s\n' "$(PLATFORM)" "$$binary" >&2; \
				exit 127; \
			}; \
		done; \
		printf 'Using packaged prebuilt binaries in %s\n' "$(BIN_DIR)"; \
	elif command -v go >/dev/null 2>&1; then \
		for binary in $(BINARY_NAMES); do \
			ldflags='-s -w'; \
			if [[ "$$binary" == agent-sessions ]]; then ldflags='$(HOST_LDFLAGS)'; fi; \
			if [[ "$$binary" == agent-sessions-hub ]]; then ldflags='$(HUB_LDFLAGS)'; fi; \
			CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags="$$ldflags" -o "$(BIN_DIR)/$$binary" "./cmd/$$binary" || exit; \
		done; \
	else \
		printf 'Go is required because this source tree has no authorized packaged %s binaries\n' "$(PLATFORM)" >&2; \
		exit 127; \
	fi

reinstall:
	./scripts/cachebuster
	$(MAKE) install

install install-all dev-install dev-install-all: build
	CODEX="$(CODEX)" CLAUDE="$(CLAUDE)" GROK="$(GROK_INSTALL_VALUE)" QWEN="$(QWEN)" \
		./scripts/install-host "$(CURDIR)" "$(abspath $(BIN_DIR))/agent-sessions" \
		"$(abspath $(PREFIX))" "$(HOST_RELEASE_VERSION)" "$(PLATFORM)"

install-hub: build
	./scripts/install-hub "$(CURDIR)" "$(abspath $(BIN_DIR))/agent-sessions-hub" \
		"$(abspath $(PREFIX))" "$(HOST_RELEASE_VERSION)" "$(PLATFORM)"

remove-all:
	CODEX="$(CODEX)" CLAUDE="$(CLAUDE)" GROK="$(GROK_INSTALL_VALUE)" QWEN="$(QWEN)" \
		./scripts/remove-host "$(abspath $(PREFIX))"

purge-all:
	CODEX="$(CODEX)" CLAUDE="$(CLAUDE)" GROK="$(GROK_INSTALL_VALUE)" QWEN="$(QWEN)" \
		./scripts/remove-host "$(abspath $(PREFIX))" --purge

remove-hub:
	./scripts/remove-hub "$(abspath $(PREFIX))"

repair-projection:
	@test -n "$(THREAD_ID)" || { printf 'usage: make repair-projection THREAD_ID=<id> [APPLY=1]\n' >&2; exit 2; }
	./scripts/repair-history-projection $(if $(filter 1,$(APPLY)),--apply,) "$(THREAD_ID)"

clean:
	rm -rf -- "$(CURDIR)/bin" "$(CURDIR)/dist"
