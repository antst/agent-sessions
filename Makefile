SHELL := /bin/bash

CODEX ?= codex
CLAUDE ?= claude
QWEN ?= qwen
# Ignore an inherited GROK environment variable: a long-lived peer may have
# pinned its own launcher, but that must not disable discovery for a later
# install. An explicit make command-line GROK=/absolute/path pins one candidate.
GROK ?=
GROK_CLI = $(if $(strip $(GROK)),$(GROK),grok)
GOLANGCI_LINT_VERSION ?= v2.12.2
TOOLS_BIN_DIR ?= $(CURDIR)/bin/tools
GOLANGCI_LINT ?= $(TOOLS_BIN_DIR)/golangci-lint
PREFIX ?= $(HOME)/.local
INSTALL_ROOT ?= $(PREFIX)/libexec/agent-sessions
MARKETPLACE ?= agent-sessions
LEGACY_MARKETPLACE ?= codex-messaging
PLUGIN ?= agent-sessions
LEGACY_CODEX_PLUGIN ?= claude-code-peer
CLAUDE_MARKETPLACE ?= agent-sessions
LEGACY_CLAUDE_MARKETPLACE ?= codex-messaging
CLAUDE_PLUGIN ?= agent-sessions
LEGACY_CLAUDE_PLUGIN ?= codex-peer
CLAUDE_SCOPE ?= user
CLAUDE_PLUGIN_VERSION := $(shell sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' claude/.claude-plugin/plugin.json | head -1)
CLAUDE_RELEASE_ROOT ?= $(PREFIX)/share/agent-sessions/claude-marketplaces
CLAUDE_STAGED_ROOT := $(CLAUDE_RELEASE_ROOT)/$(CLAUDE_PLUGIN_VERSION)
CLAUDE_MARKETPLACE_ROOT ?= $(CLAUDE_STAGED_ROOT)
GROK_PLUGIN_ROOT ?= $(INSTALL_ROOT)/grok
GROK_PLUGIN_NAME := agent-sessions
GROK_USER_PLUGIN_ROOT ?= $(HOME)/.grok/plugins/$(GROK_PLUGIN_NAME)
QWEN_PLUGIN_ROOT ?= $(INSTALL_ROOT)/qwen
QWEN_PLUGIN_VERSION := $(shell sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' qwen/plugin.json | head -1)
CONNECTOR_INSTALLER ?= $(BIN_DIR)/agent-sessions
CONNECTOR_SOURCE_ROOT ?= $(INSTALL_ROOT)
HOST_INSTALLER ?= $(BIN_DIR)/agent-sessions
HOST_RELEASE_VERSION := $(shell cat deploy/agent-sessions/VERSION)
HOST_LDFLAGS ?= -s -w -X github.com/antst/agent-sessions/internal/daemon.BuildVersion=$(HOST_RELEASE_VERSION)
HUB_INSTALLER ?= $(BIN_DIR)/agent-sessions-hub
HUB_RELEASE_VERSION := $(HOST_RELEASE_VERSION)
HUB_LDFLAGS ?= -s -w -X main.version=$(HUB_RELEASE_VERSION)
HUB_LISTEN ?= :7419
INSTALL_ALL_MAKE ?= $(MAKE)

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

.PHONY: all lint lint-tool test test-race vet federation-integration unified-live-contracts \
	build build-hub install install-codex dev-install reinstall \
	stage-claude validate-claude install-claude dev-install-claude validate-grok install-grok \
	dev-install-grok validate-qwen install-qwen upgrade-qwen remove-qwen dev-install-qwen install-all dev-install-all \
	remove purge-inspect purge \
	install-hub remove-hub purge-hub-inspect purge-hub repair-projection clean

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
	@mkdir -p "$(TOOLS_BIN_DIR)"
	CGO_ENABLED=0 GOOS="$(HOST_GOOS)" GOARCH="$(HOST_GOARCH)" go build -trimpath -ldflags='-s -w' \
		-o "$(TOOLS_BIN_DIR)/agent-sessions-release-packager-$(PLATFORM)" ./cmd/agent-sessions
	AGENT_SESSIONS_RELEASE_PACKAGER="$(TOOLS_BIN_DIR)/agent-sessions-release-packager-$(PLATFORM)" \
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

vet:
	go vet ./...

federation-integration:
	./scripts/federation/test

# Closed unified live-contract inventory. Only test-unified-service creates a
# production-shaped daemon, and it does so under its isolated service-manager
# fixture; the remaining contracts use in-process authorities.
unified-live-contracts:
	./scripts/test-unified-peers
	./scripts/test-unified-lane-composition
	./scripts/test-unified-lane-restart
	./scripts/test-unified-migration
	./scripts/test-unified-stress
	./scripts/test-unified-service

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

build-hub:
	mkdir -p "$(BIN_DIR)"
	@if [[ -f "$(PREBUILT_RELEASE_MARKER)" ]]; then \
		[[ -x "$(BIN_DIR)/agent-sessions-hub" ]] || { \
			printf 'This release archive does not contain prebuilt %s agent-sessions-hub\n' "$(PLATFORM)" >&2; \
			exit 127; \
		}; \
		printf 'Using packaged prebuilt hub binary in %s\n' "$(BIN_DIR)"; \
	elif command -v go >/dev/null 2>&1; then \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags="$(HUB_LDFLAGS)" \
			-o "$(BIN_DIR)/agent-sessions-hub" ./cmd/agent-sessions-hub; \
	else \
		printf 'Go is required because this source tree has no authorized packaged %s hub binary\n' "$(PLATFORM)" >&2; \
		exit 127; \
	fi


install: install-all

dev-install: install-all

install-codex: build
	@command -v "$(CODEX)" >/dev/null 2>&1 || { printf 'Codex is required for explicit connector installation: %s\n' "$(CODEX)" >&2; exit 127; }
	"$(CONNECTOR_INSTALLER)" connector install --product codex --source-root "$(abspath $(CONNECTOR_SOURCE_ROOT))" --native "$(CODEX)"

reinstall:
	./scripts/cachebuster
	$(MAKE) install-all

stage-claude:
	@test -n "$(CLAUDE_PLUGIN_VERSION)" || { printf 'Claude plugin version is missing\n' >&2; exit 1; }
	@if [[ "$(abspath $(CLAUDE_MARKETPLACE_ROOT))" != "$(abspath $(CLAUDE_STAGED_ROOT))" ]]; then \
		exit 0; \
	fi; \
	mkdir -p "$(CLAUDE_RELEASE_ROOT)"; \
	if [[ -d "$(CLAUDE_STAGED_ROOT)" ]]; then \
		diff -qr .claude-plugin "$(CLAUDE_STAGED_ROOT)/.claude-plugin" >/dev/null && \
		diff -qr claude "$(CLAUDE_STAGED_ROOT)/claude" >/dev/null || { \
			printf 'Claude plugin version %s already exists with different content; run scripts/cachebuster first\n' "$(CLAUDE_PLUGIN_VERSION)" >&2; \
			exit 1; \
		}; \
	else \
		stage_dir="$$(mktemp -d "$(CLAUDE_RELEASE_ROOT)/.stage.XXXXXX")"; \
		trap 'rm -rf -- "$$stage_dir"' EXIT; \
		cp -R .claude-plugin claude "$$stage_dir/"; \
		mv "$$stage_dir" "$(CLAUDE_STAGED_ROOT)"; \
		trap - EXIT; \
	fi

validate-claude: stage-claude
	@command -v "$(CLAUDE)" >/dev/null 2>&1 || { \
		printf 'Claude Code is required for Claude plugin validation\n' >&2; \
		exit 127; \
	}
	$(CLAUDE) plugin validate "$(CLAUDE_MARKETPLACE_ROOT)" --strict
	$(CLAUDE) plugin validate "$(CLAUDE_MARKETPLACE_ROOT)/claude" --strict

install-claude: validate-claude
	"$(CONNECTOR_INSTALLER)" connector install --product claude --source-root "$(abspath $(CLAUDE_MARKETPLACE_ROOT))" --native "$(CLAUDE)"

dev-install-claude:
	$(MAKE) install-claude CLAUDE_MARKETPLACE_ROOT="$(CURDIR)"

validate-grok: build
	@command -v "$(GROK_CLI)" >/dev/null 2>&1 || { \
		printf 'Grok is required for explicit connector installation: %s\n' "$(GROK_CLI)" >&2; \
		exit 127; \
	}
	"$(GROK_CLI)" plugin validate "$(GROK_PLUGIN_ROOT)"

install-grok: validate-grok
	"$(CONNECTOR_INSTALLER)" connector install --product grok \
		--source-root "$(abspath $(patsubst %/,%,$(dir $(GROK_PLUGIN_ROOT))))" \
		--native "$(GROK_CLI)" --grok-user-root "$(abspath $(GROK_USER_PLUGIN_ROOT))"

dev-install-grok:
	$(MAKE) install-grok GROK_PLUGIN_ROOT="$(CURDIR)/grok"

validate-qwen: build
	@command -v "$(QWEN)" >/dev/null 2>&1 || { \
		printf 'Qwen Code is required for Qwen plugin installation: %s\n' "$(QWEN)" >&2; \
		exit 127; \
	}
	@test -n "$(QWEN_PLUGIN_VERSION)" || { printf 'Qwen plugin version is missing\n' >&2; exit 1; }
	@test -f "$(QWEN_PLUGIN_ROOT)/plugin.json" -a -f "$(QWEN_PLUGIN_ROOT)/mcp.json" || { \
		printf 'Qwen plugin payload is missing at %s\n' "$(QWEN_PLUGIN_ROOT)" >&2; \
		exit 1; \
	}

install-qwen: validate-qwen
	"$(CONNECTOR_INSTALLER)" connector install --product qwen \
		--source-root "$(abspath $(patsubst %/,%,$(dir $(QWEN_PLUGIN_ROOT))))" --native "$(QWEN)"

upgrade-qwen: install-qwen

remove-qwen: build
	@command -v "$(QWEN)" >/dev/null 2>&1 || { \
		printf 'Qwen Code is required for Qwen plugin removal: %s\n' "$(QWEN)" >&2; \
		exit 127; \
	}
	"$(CONNECTOR_INSTALLER)" connector remove --product qwen --native "$(QWEN)"

dev-install-qwen:
	$(MAKE) install-qwen QWEN_PLUGIN_ROOT="$(CURDIR)/qwen"

install-all: build
	@set -euo pipefail; \
		install_root="$(abspath $(INSTALL_ROOT))"; \
		mkdir -p "$$install_root"; \
		stage="$$(mktemp -d "$$install_root/.host-source.XXXXXX")"; \
		trap 'rm -rf -- "$$stage"' EXIT; \
		while IFS= read -r path; do \
			destination="$$stage/$$(dirname -- "$$path")"; \
			mkdir -p "$$destination"; \
			cp -R -- "$$path" "$$destination/"; \
		done < <(./scripts/release-inventory host-package-paths); \
		mkdir -p "$$stage/bin"; \
		cp -- "$(BIN_DIR)/agent-sessions" "$$stage/bin/agent-sessions"; \
		chmod 0755 "$$stage/bin/agent-sessions"; \
		bash ./scripts/stage-release-metadata "$$stage" "$(PLATFORM)" "$(HOST_RELEASE_VERSION)" host; \
		"$(HOST_INSTALLER)" lifecycle install \
			--role host \
			--source-root "$$stage" \
			--prefix "$(abspath $(PREFIX))" \
			--version "$(HOST_RELEASE_VERSION)" \
			--codex "$(CODEX)" \
			--claude "$(CLAUDE)" \
			--grok "$(GROK_CLI)" \
			--qwen "$(QWEN)"

dev-install-all: install-all

remove: build
	"$(HOST_INSTALLER)" lifecycle remove \
		--role host \
		--prefix "$(abspath $(PREFIX))" \
		--codex "$(CODEX)" \
		--claude "$(CLAUDE)" \
		--grok "$(GROK_CLI)" \
		--qwen "$(QWEN)"

install-hub: build-hub
	@set -euo pipefail; \
		install_root="$(abspath $(INSTALL_ROOT))"; \
		mkdir -p "$$install_root"; \
		stage="$$(mktemp -d "$$install_root/.hub-source.XXXXXX")"; \
		trap 'rm -rf -- "$$stage"' EXIT; \
		while IFS= read -r path; do \
			destination="$$stage/$$(dirname -- "$$path")"; \
			mkdir -p "$$destination"; \
			cp -R -- "$$path" "$$destination/"; \
		done < <(./scripts/release-inventory hub-package-paths); \
		mkdir -p "$$stage/bin"; \
		cp -- "$(BIN_DIR)/agent-sessions-hub" "$$stage/bin/agent-sessions-hub"; \
		chmod 0755 "$$stage/bin/agent-sessions-hub"; \
		bash ./scripts/stage-release-metadata "$$stage" "$(PLATFORM)" "$(HUB_RELEASE_VERSION)" hub; \
		"$(HUB_INSTALLER)" lifecycle install \
			--role hub \
			--source-root "$$stage" \
			--prefix "$(abspath $(PREFIX))" \
			--version "$(HUB_RELEASE_VERSION)" \
			--listen "$(HUB_LISTEN)"

remove-hub: build-hub
	"$(HUB_INSTALLER)" lifecycle remove --role hub --prefix "$(abspath $(PREFIX))"

purge-hub-inspect: build-hub
	@test -n "$(PURGE_PLAN)" || { printf 'PURGE_PLAN is required\n' >&2; exit 2; }
	"$(HUB_INSTALLER)" purge inspect --plan "$(abspath $(PURGE_PLAN))"

purge-hub: build-hub
	@test -n "$(PURGE_PLAN)" || { printf 'PURGE_PLAN is required\n' >&2; exit 2; }
	"$(HUB_INSTALLER)" purge apply --plan "$(abspath $(PURGE_PLAN))"

purge-inspect: build
	@test -n "$(PURGE_PLAN)" || { printf 'PURGE_PLAN is required\n' >&2; exit 2; }
	"$(HOST_INSTALLER)" purge inspect --plan "$(abspath $(PURGE_PLAN))"

purge: build
	@test -n "$(PURGE_PLAN)" || { printf 'PURGE_PLAN is required\n' >&2; exit 2; }
	"$(HOST_INSTALLER)" purge apply --plan "$(abspath $(PURGE_PLAN))"

repair-projection:
	@test -n "$(THREAD_ID)" || { printf 'usage: make repair-projection THREAD_ID=<id> [APPLY=1]\n' >&2; exit 2; }
	./scripts/repair-history-projection $(if $(filter 1,$(APPLY)),--apply,) "$(THREAD_ID)"

clean:
	rm -rf -- "$(CURDIR)/bin" "$(CURDIR)/dist"
