SHELL := /bin/bash

CODEX ?= codex
CLAUDE ?= claude
QWEN ?= qwen
# Ignore an inherited GROK environment variable: a long-lived peer may have
# pinned its own launcher, but that must not disable discovery for a later
# install. An explicit make command-line GROK=/absolute/path pins one candidate.
GROK_INPUT_ORIGIN := $(origin GROK)
GROK ?=
GROK_CLI = $(if $(strip $(GROK)),$(GROK),grok)
GROK_PEER ?= $(BIN_DIR)/grok-peer
GROK_PLUGIN_VERIFY ?= $(BIN_DIR)/agent-session-runtime
GROK_PEER_ENV = $(if $(and $(findstring command line,$(GROK_INPUT_ORIGIN)),$(strip $(GROK))),GROK_PEER_GROK_BIN="$(GROK)")
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
QWEN_PLUGIN_INSTALLER ?= $(BIN_DIR)/agent-session-runtime
CONNECTOR_INSTALLER ?= $(BIN_DIR)/agent-sessions
CONNECTOR_SOURCE_ROOT ?= $(INSTALL_ROOT)
HOST_INSTALLER ?= $(BIN_DIR)/agent-sessions
HOST_RELEASE_VERSION := $(shell cat deploy/agent-sessions/VERSION)
HOST_LDFLAGS ?= -s -w -X github.com/antst/agent-sessions/internal/daemon.BuildVersion=$(HOST_RELEASE_VERSION)
START_RUNTIME ?= 1
INSTALL_CODEX_INTEGRATION ?= 1
INSTALL_ALL_MAKE ?= $(MAKE)
PEER_FEDERATOR_CONFIG_DIR ?= $(HOME)/.config/peer-federator
PEER_FEDERATOR_DOC_ROOT ?= $(PREFIX)/share/doc/peer-federator
USER_SYSTEMD_DIR ?= $(HOME)/.config/systemd/user
USER_LAUNCHD_DIR ?= $(HOME)/Library/LaunchAgents
PEER_FEDERATOR_VERSION ?= $(shell cat deploy/agent-sessions/VERSION)
PEER_FEDERATOR_LDFLAGS ?= -s -w -X main.version=$(PEER_FEDERATOR_VERSION)

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

.PHONY: all lint lint-tool test test-race build build-peer-federator install-preflight grok-install-preflight install install-codex dev-install reinstall \
	stage-claude validate-claude install-claude dev-install-claude validate-grok install-grok \
	dev-install-grok validate-qwen install-qwen upgrade-qwen remove-qwen dev-install-qwen install-all dev-install-all \
	remove purge-inspect purge \
	install-peer-federator install-systemd-user-files install-systemd-user \
	install-launchd-user-files install-launchd-user repair-projection clean

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
	$(MAKE) build GOOS="$(GOOS)" GOARCH="$(GOARCH)" PEER_FEDERATOR_VERSION="$(RELEASE_VERSION)"
	@mkdir -p "$(TOOLS_BIN_DIR)"
	CGO_ENABLED=0 GOOS="$(HOST_GOOS)" GOARCH="$(HOST_GOARCH)" go build -trimpath -ldflags='-s -w' \
		-o "$(TOOLS_BIN_DIR)/agent-session-runtime-release-packager-$(PLATFORM)" ./cmd/agent-session-runtime
	AGENT_SESSIONS_RELEASE_PACKAGER="$(TOOLS_BIN_DIR)/agent-session-runtime-release-packager-$(PLATFORM)" \
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
			CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags="$$ldflags" -o "$(BIN_DIR)/$$binary" "./cmd/$$binary" || exit; \
		done; \
	else \
		printf 'Go is required because this source tree has no authorized packaged %s binaries\n' "$(PLATFORM)" >&2; \
		exit 127; \
	fi

build-peer-federator: build
	@test -x "$(BIN_DIR)/peer-federator"

install-peer-federator: build-peer-federator
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 0755 "$(BIN_DIR)/peer-federator" "$(DESTDIR)$(PREFIX)/bin/peer-federator"
	install -d "$(DESTDIR)$(PEER_FEDERATOR_DOC_ROOT)"
	cp -R docs/FEDERATION.md docs/federation deploy/peer-federator "$(DESTDIR)$(PEER_FEDERATOR_DOC_ROOT)/"

install-systemd-user-files:
	install -d "$(DESTDIR)$(USER_SYSTEMD_DIR)" "$(DESTDIR)$(PEER_FEDERATOR_CONFIG_DIR)"
	install -m 0644 deploy/peer-federator/systemd/user/*.service "$(DESTDIR)$(USER_SYSTEMD_DIR)/"
	install -m 0644 deploy/peer-federator/systemd/user/*.env.example "$(DESTDIR)$(PEER_FEDERATOR_CONFIG_DIR)/"

install-systemd-user: install-peer-federator install-systemd-user-files
	@printf 'Installed peer-federator user units and templates without replacing active .env files.\n'

install-launchd-user-files:
	install -d "$(DESTDIR)$(USER_LAUNCHD_DIR)"
	install -m 0644 deploy/peer-federator/launchd/*.plist.example "$(DESTDIR)$(USER_LAUNCHD_DIR)/"

install-launchd-user: install-peer-federator install-launchd-user-files
	@printf 'Installed peer-federator launchd templates without replacing or loading active plists.\n'

install-preflight: build
	@if [[ "$(START_RUNTIME)" == "1" ]]; then \
		"$(BIN_DIR)/agent-session-runtime" appserver stopped || { \
			printf '%s\n' \
				'App Server is still running. Exit every Codex client, then stop it with:' \
				'  $(CODEX) app-server daemon stop' \
				'After it stops, run make install again.' >&2; \
			exit 75; \
		}; \
		grok_status=0; \
		"$(BIN_DIR)/agent-session-runtime" grok stopped || grok_status=$$?; \
		if [[ $$grok_status -eq 3 ]]; then \
			printf '%s\n' \
				'A managed Grok peer or lane is still running.' \
				'Exit every grok-peer TUI normally, then list and archive headless lanes with:' \
				'  grok-peer-lane list' \
				'  grok-peer-lane archive SESSION_OR_NAME' \
				'After they stop, run make install again.' >&2; \
			exit 75; \
		elif [[ $$grok_status -ne 0 ]]; then \
			printf 'Cannot verify that managed Grok peers are stopped (inventory exit %s). Resolve the diagnostic above before installing.\n' "$$grok_status" >&2; \
			exit "$$grok_status"; \
		fi; \
	fi

grok-install-preflight: build
	@grok_status=0; \
	"$(BIN_DIR)/agent-session-runtime" grok stopped || grok_status=$$?; \
	if [[ $$grok_status -eq 3 ]]; then \
		printf '%s\n' \
			'A managed Grok peer or lane is still running.' \
			'Exit every grok-peer TUI normally, then list and archive headless lanes with:' \
			'  grok-peer-lane list' \
			'  grok-peer-lane archive SESSION_OR_NAME' \
			'After they stop, run make install-grok again.' >&2; \
		exit 75; \
	elif [[ $$grok_status -ne 0 ]]; then \
		printf 'Cannot verify that managed Grok peers are stopped (inventory exit %s). Resolve the diagnostic above before installing.\n' "$$grok_status" >&2; \
		exit "$$grok_status"; \
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
	$(MAKE) install-qwen QWEN_PLUGIN_ROOT="$(CURDIR)/qwen" QWEN_PLUGIN_INSTALLER="$(BIN_DIR)/agent-session-runtime"

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
