SHELL := /bin/bash

CODEX ?= codex
CLAUDE ?= claude
QWEN ?= qwen
# Ignore an inherited GROK environment variable: a long-lived peer may have
# pinned its own launcher, but that must not disable discovery for a later
# install. An explicit make command-line GROK=/absolute/path pins one candidate.
GROK_INPUT_ORIGIN := $(origin GROK)
GROK ?=
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
START_RUNTIME ?= 1
INSTALL_CODEX_INTEGRATION ?= 1
INSTALL_ALL_MAKE ?= $(MAKE)
PEER_FEDERATOR_CONFIG_DIR ?= $(HOME)/.config/peer-federator
PEER_FEDERATOR_DOC_ROOT ?= $(PREFIX)/share/doc/peer-federator
USER_SYSTEMD_DIR ?= $(HOME)/.config/systemd/user
USER_LAUNCHD_DIR ?= $(HOME)/Library/LaunchAgents
PEER_FEDERATOR_VERSION ?= $(shell cat deploy/peer-federator/VERSION)
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

.PHONY: all lint lint-tool test test-race build build-peer-federator install-preflight grok-install-preflight install dev-install reinstall \
	stage-claude validate-claude install-claude dev-install-claude validate-grok install-grok \
	dev-install-grok validate-qwen install-qwen upgrade-qwen remove-qwen dev-install-qwen install-all dev-install-all \
	install-peer-federator install-systemd-user-files install-systemd-user \
	install-launchd-user-files install-launchd-user repair-projection clean

.PHONY: release-inventory build-release-platform

release-inventory:
	@./scripts/release-inventory binaries
	@./scripts/release-inventory plugins

build-release-platform:
	@test -n "$(RELEASE_OUTPUT_DIR)" || { printf 'RELEASE_OUTPUT_DIR is required\n' >&2; exit 2; }
	@test -n "$(RELEASE_VERSION)" || { printf 'RELEASE_VERSION is required\n' >&2; exit 2; }
	@test "$(RELEASE_VERSION)" = "$$(cat deploy/peer-federator/VERSION)" || { \
		printf 'release version %s does not match deploy/peer-federator/VERSION\n' "$(RELEASE_VERSION)" >&2; exit 1; \
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
			if [[ "$$binary" == peer-federator ]]; then ldflags='$(PEER_FEDERATOR_LDFLAGS)'; fi; \
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

install: install-preflight
	mkdir -p "$(INSTALL_ROOT)" "$(PREFIX)/bin"
	rm -rf -- \
		"$(INSTALL_ROOT)/.agents" \
		"$(INSTALL_ROOT)/.codex-plugin" \
		"$(INSTALL_ROOT)/bin" \
		"$(INSTALL_ROOT)/deploy" \
		"$(INSTALL_ROOT)/docs" \
		"$(INSTALL_ROOT)/grok" \
		"$(INSTALL_ROOT)/hooks" \
		"$(INSTALL_ROOT)/qwen" \
		"$(INSTALL_ROOT)/scripts" \
		"$(INSTALL_ROOT)/skills"
	cp -R .agents .codex-plugin deploy docs grok hooks qwen scripts skills "$(INSTALL_ROOT)/"
	cp .mcp.json README.md "$(INSTALL_ROOT)/"
	mkdir -p "$(INSTALL_ROOT)/bin/$(PLATFORM)"
	@for binary in $(BINARY_NAMES); do cp "$(BIN_DIR)/$$binary" "$(INSTALL_ROOT)/bin/$(PLATFORM)/$$binary"; done
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/agent-session-runtime" "$(PREFIX)/bin/agent-session-runtime"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/peer" "$(PREFIX)/bin/peer"
	rm -f -- "$(PREFIX)/bin/codex-messaging"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/codex-peer" "$(PREFIX)/bin/codex-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/claude-peer" "$(PREFIX)/bin/claude-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/qwen-peer" "$(PREFIX)/bin/qwen-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/codex-peer-lane" "$(PREFIX)/bin/codex-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/claude-peer-lane" "$(PREFIX)/bin/claude-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/grok-peer" "$(PREFIX)/bin/grok-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/grok-peer-lane" "$(PREFIX)/bin/grok-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/qwen-peer-lane" "$(PREFIX)/bin/qwen-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/peer-federator" "$(PREFIX)/bin/peer-federator"
ifeq ($(INSTALL_CODEX_INTEGRATION),1)
	@if $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(MARKETPLACE)"; \
	fi
	@if $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(MARKETPLACE)"; \
	fi
	$(CODEX) plugin marketplace add "$(INSTALL_ROOT)"
	$(CODEX) plugin add "$(PLUGIN)@$(MARKETPLACE)"
	@$(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"' || { \
		printf 'Codex did not register replacement plugin %s@%s\n' "$(PLUGIN)" "$(MARKETPLACE)" >&2; \
		exit 1; \
	}
	@for legacy_id in \
		"$(LEGACY_CODEX_PLUGIN)@$(MARKETPLACE)" \
		"$(LEGACY_CODEX_PLUGIN)@personal" \
		"$(LEGACY_CODEX_PLUGIN)@$(LEGACY_MARKETPLACE)" \
		"$(PLUGIN)@$(LEGACY_MARKETPLACE)"; do \
		[[ "$$legacy_id" == "$(PLUGIN)@$(MARKETPLACE)" ]] && continue; \
		if $(CODEX) plugin list --json | grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"'"$$legacy_id"'"'; then \
			$(CODEX) plugin remove "$$legacy_id"; \
		fi; \
	done
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(START_RUNTIME)" == "1" ]]; then \
		CODEX_PEER_CODEX_BIN="$(CODEX)" "$(INSTALL_ROOT)/bin/$(PLATFORM)/agent-session-runtime" bootstrap; \
	fi
else
	@printf 'Skipping Codex integration: Codex CLI is not installed.\n'
endif

dev-install: install-preflight
	mkdir -p "$(PREFIX)/bin"
	ln -sfn "$(abspath $(BIN_DIR))/agent-session-runtime" "$(PREFIX)/bin/agent-session-runtime"
	ln -sfn "$(abspath $(BIN_DIR))/peer" "$(PREFIX)/bin/peer"
	rm -f -- "$(PREFIX)/bin/codex-messaging"
	ln -sfn "$(abspath $(BIN_DIR))/codex-peer" "$(PREFIX)/bin/codex-peer"
	ln -sfn "$(abspath $(BIN_DIR))/claude-peer" "$(PREFIX)/bin/claude-peer"
	ln -sfn "$(abspath $(BIN_DIR))/qwen-peer" "$(PREFIX)/bin/qwen-peer"
	ln -sfn "$(abspath $(BIN_DIR))/codex-peer-lane" "$(PREFIX)/bin/codex-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/claude-peer-lane" "$(PREFIX)/bin/claude-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/grok-peer" "$(PREFIX)/bin/grok-peer"
	ln -sfn "$(abspath $(BIN_DIR))/grok-peer-lane" "$(PREFIX)/bin/grok-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/qwen-peer-lane" "$(PREFIX)/bin/qwen-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/peer-federator" "$(PREFIX)/bin/peer-federator"
ifeq ($(INSTALL_CODEX_INTEGRATION),1)
	@if $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(MARKETPLACE)"; \
	fi
	@if $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(MARKETPLACE)"; \
	fi
	$(CODEX) plugin marketplace add "$(CURDIR)"
	$(CODEX) plugin add "$(PLUGIN)@$(MARKETPLACE)"
	@$(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"' || { \
		printf 'Codex did not register replacement plugin %s@%s\n' "$(PLUGIN)" "$(MARKETPLACE)" >&2; \
		exit 1; \
	}
	@for legacy_id in \
		"$(LEGACY_CODEX_PLUGIN)@$(MARKETPLACE)" \
		"$(LEGACY_CODEX_PLUGIN)@personal" \
		"$(LEGACY_CODEX_PLUGIN)@$(LEGACY_MARKETPLACE)" \
		"$(PLUGIN)@$(LEGACY_MARKETPLACE)"; do \
		[[ "$$legacy_id" == "$(PLUGIN)@$(MARKETPLACE)" ]] && continue; \
		if $(CODEX) plugin list --json | grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"'"$$legacy_id"'"'; then \
			$(CODEX) plugin remove "$$legacy_id"; \
		fi; \
	done
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(START_RUNTIME)" == "1" ]]; then \
		CODEX_PEER_CODEX_BIN="$(CODEX)" "$(BIN_DIR)/agent-session-runtime" bootstrap; \
	fi
else
	@printf 'Skipping Codex integration: Codex CLI is not installed.\n'
endif

reinstall: install-preflight
	./scripts/cachebuster
	$(MAKE) install

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
	# Adding the same marketplace at the selected scope is idempotent and updates
	# its source path. Do not remove a working plugin/marketplace before its
	# replacement has been accepted. The install branch is followed by update
	# because Claude can accept an already-installed plugin without updating it.
	$(CLAUDE) plugin marketplace add --scope "$(CLAUDE_SCOPE)" "$(CLAUDE_MARKETPLACE_ROOT)"
	@if $(CLAUDE) plugin list --json | awk \
		-v id='$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)' -v scope='$(CLAUDE_SCOPE)' \
		'index($$0, "\"id\"") { matching = index($$0, "\"" id "\"") > 0 } \
		 matching && index($$0, "\"scope\"") { if (index($$0, "\"" scope "\"") > 0) found = 1; matching = 0 } \
		 END { exit(found ? 0 : 1) }'; then \
		$(CLAUDE) plugin update --scope "$(CLAUDE_SCOPE)" "$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)"; \
	else \
		$(CLAUDE) plugin install --scope "$(CLAUDE_SCOPE)" "$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)"; \
		$(CLAUDE) plugin update --scope "$(CLAUDE_SCOPE)" "$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)"; \
	fi
	@$(CLAUDE) plugin list --json | awk \
		-v id='$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)' -v scope='$(CLAUDE_SCOPE)' \
		'index($$0, "\"id\"") { matching = index($$0, "\"" id "\"") > 0 } \
		 matching && index($$0, "\"scope\"") { if (index($$0, "\"" scope "\"") > 0) found = 1; matching = 0 } \
		 END { exit(found ? 0 : 1) }' || { \
		printf 'Claude did not register replacement plugin %s@%s at scope %s\n' \
			"$(CLAUDE_PLUGIN)" "$(CLAUDE_MARKETPLACE)" "$(CLAUDE_SCOPE)" >&2; \
		exit 1; \
	}
	@for legacy_id in \
		"$(LEGACY_CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)" \
		"$(LEGACY_CLAUDE_PLUGIN)@$(LEGACY_CLAUDE_MARKETPLACE)" \
		"$(CLAUDE_PLUGIN)@$(LEGACY_CLAUDE_MARKETPLACE)"; do \
		[[ "$$legacy_id" == "$(CLAUDE_PLUGIN)@$(CLAUDE_MARKETPLACE)" ]] && continue; \
		if $(CLAUDE) plugin list --json | grep -Fq '"'"$$legacy_id"'"'; then \
			$(CLAUDE) plugin uninstall --scope "$(CLAUDE_SCOPE)" "$$legacy_id"; \
		fi; \
	done
	@if [[ "$(LEGACY_CLAUDE_MARKETPLACE)" != "$(CLAUDE_MARKETPLACE)" ]] && $(CLAUDE) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(LEGACY_CLAUDE_MARKETPLACE)"'; then \
		$(CLAUDE) plugin marketplace remove --scope "$(CLAUDE_SCOPE)" "$(LEGACY_CLAUDE_MARKETPLACE)"; \
	fi

dev-install-claude:
	$(MAKE) install-claude CLAUDE_MARKETPLACE_ROOT="$(CURDIR)"

validate-grok: build
	@test -x "$(GROK_PEER)" || { \
		printf 'The validated grok-peer launcher is required for Grok plugin management: %s\n' "$(GROK_PEER)" >&2; \
		exit 127; \
	}
	# Never execute GROK directly: grok-peer applies the same fail-closed CLI
	# contract probe used for interactive launches, including explicit overrides.
	$(GROK_PEER_ENV) "$(GROK_PEER)" plugin validate "$(GROK_PLUGIN_ROOT)"

install-grok: grok-install-preflight validate-grok
	@plugin_root="$(GROK_USER_PLUGIN_ROOT)"; \
		plugin_parent="$$(dirname -- "$$plugin_root")"; \
		plugin_leaf="$$(basename -- "$$plugin_root")"; \
		if [[ "$$plugin_root" != /* || "$$plugin_root" == "$(HOME)" || \
			"$$plugin_leaf" != "$(GROK_PLUGIN_NAME)" || -z "$$plugin_parent" || \
			"$$plugin_parent" == "." || "$$plugin_parent" == "/" ]]; then \
			printf 'Refusing unsafe GROK_USER_PLUGIN_ROOT (must be a dedicated .../%s directory): %s\n' \
				"$(GROK_PLUGIN_NAME)" "$$plugin_root" >&2; \
			exit 2; \
		fi
	# Grok documents ~/.grok/plugins as its auto-trusted user plugin location.
	# Copying this native MCP payload there is the explicit trust decision.
	# Migrate the older direct-install registry row first; it can be listed as
	# enabled while still being omitted from a live session's MCP inventory.
	# Deliberately omit --confirm below: fail closed if that name belongs to a
	# multi-plugin repository instead of deleting its unrelated plugins.
	@plugin_list="$$( $(GROK_PEER_ENV) "$(GROK_PEER)" plugin list --json )" || { \
		status=$$?; \
		printf 'Cannot inspect existing Grok plugin registrations (exit %s).\n' "$$status" >&2; \
		exit "$$status"; \
	}; \
	if printf '%s\n' "$$plugin_list" | awk -v name='$(GROK_PLUGIN_NAME)' 'BEGIN { RS = "}" } \
			index($$0, "\"name\"") && index($$0, "\"" name "\"") && index($$0, "\"repo_key\"") { found = 1 } \
			END { exit(found ? 0 : 1) }'; then \
		$(GROK_PEER_ENV) "$(GROK_PEER)" plugin uninstall "$(GROK_PLUGIN_NAME)" --keep-data; \
	fi
	@plugin_parent="$$(dirname -- "$(GROK_USER_PLUGIN_ROOT)")"; \
		mkdir -p "$$plugin_parent"; \
		stage_dir="$$(mktemp -d "$$plugin_parent/.agent-sessions.stage.XXXXXX")"; \
		trap 'rm -rf -- "$$stage_dir"' EXIT; \
		cp -R "$(GROK_PLUGIN_ROOT)/." "$$stage_dir/"; \
		rm -rf -- "$(GROK_USER_PLUGIN_ROOT)"; \
		mv "$$stage_dir" "$(GROK_USER_PLUGIN_ROOT)"; \
		trap - EXIT
	# Grok 1.0.4 discovers the live plugin from its user-plugin directory, but
	# only its official installer may safely update the enabled-plugin config.
	# Use a temporary direct registration for that write, remove only that
	# registration while preserving data/config, then verify Grok's resolved
	# runtime view rather than trusting either command's exit status alone.
	$(GROK_PEER_ENV) "$(GROK_PEER)" plugin install "$(GROK_PLUGIN_ROOT)" --trust
	$(GROK_PEER_ENV) "$(GROK_PEER)" plugin uninstall "$(GROK_PLUGIN_NAME)" --keep-data
	set -o pipefail; $(GROK_PEER_ENV) "$(GROK_PEER)" inspect --json | \
		"$(GROK_PLUGIN_VERIFY)" grok-plugin-verify --root "$(GROK_USER_PLUGIN_ROOT)"

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
	"$(QWEN_PLUGIN_INSTALLER)" qwen-plugin-install \
		--qwen "$(QWEN)" \
		--plugin-root "$(abspath $(QWEN_PLUGIN_ROOT))" \
		--version "$(QWEN_PLUGIN_VERSION)"

upgrade-qwen: install-qwen

remove-qwen: build
	@command -v "$(QWEN)" >/dev/null 2>&1 || { \
		printf 'Qwen Code is required for Qwen plugin removal: %s\n' "$(QWEN)" >&2; \
		exit 127; \
	}
	"$(QWEN_PLUGIN_INSTALLER)" qwen-plugin-remove --qwen "$(QWEN)"

dev-install-qwen:
	$(MAKE) install-qwen QWEN_PLUGIN_ROOT="$(CURDIR)/qwen" QWEN_PLUGIN_INSTALLER="$(BIN_DIR)/agent-session-runtime"

install-all:
	+@if command -v "$(CODEX)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) install; \
	else \
		printf 'Skipping Codex integration: Codex CLI is not installed (%s).\n' "$(CODEX)"; \
		$(INSTALL_ALL_MAKE) install INSTALL_CODEX_INTEGRATION=0; \
	fi
	+@if command -v "$(CLAUDE)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) install-claude; \
	else \
		printf 'Skipping Claude integration: Claude Code is not installed (%s).\n' "$(CLAUDE)"; \
	fi
	+@grok_status=0; \
		$(GROK_PEER_ENV) "$(GROK_PEER)" plugin validate "$(GROK_PLUGIN_ROOT)" >/dev/null 2>&1 || grok_status=$$?; \
		if [[ $$grok_status -eq 127 ]]; then \
			printf 'Skipping Grok integration: Grok is not installed.\n'; \
		else \
			$(INSTALL_ALL_MAKE) install-grok; \
		fi
	+@if command -v "$(QWEN)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) install-qwen; \
	else \
		printf 'Skipping Qwen integration: Qwen Code is not installed (%s).\n' "$(QWEN)"; \
	fi

dev-install-all:
	+@if command -v "$(CODEX)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) dev-install; \
	else \
		printf 'Skipping Codex integration: Codex CLI is not installed (%s).\n' "$(CODEX)"; \
		$(INSTALL_ALL_MAKE) dev-install INSTALL_CODEX_INTEGRATION=0; \
	fi
	+@if command -v "$(CLAUDE)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) dev-install-claude; \
	else \
		printf 'Skipping Claude integration: Claude Code is not installed (%s).\n' "$(CLAUDE)"; \
	fi
	+@grok_status=0; \
		$(GROK_PEER_ENV) "$(GROK_PEER)" plugin validate "$(CURDIR)/grok" >/dev/null 2>&1 || grok_status=$$?; \
		if [[ $$grok_status -eq 127 ]]; then \
			printf 'Skipping Grok integration: Grok is not installed.\n'; \
		else \
			$(INSTALL_ALL_MAKE) dev-install-grok; \
		fi
	+@if command -v "$(QWEN)" >/dev/null 2>&1; then \
		$(INSTALL_ALL_MAKE) dev-install-qwen; \
	else \
		printf 'Skipping Qwen integration: Qwen Code is not installed (%s).\n' "$(QWEN)"; \
	fi

repair-projection:
	@test -n "$(THREAD_ID)" || { printf 'usage: make repair-projection THREAD_ID=<id> [APPLY=1]\n' >&2; exit 2; }
	./scripts/repair-history-projection $(if $(filter 1,$(APPLY)),--apply,) "$(THREAD_ID)"

clean:
	rm -rf -- "$(CURDIR)/bin" "$(CURDIR)/dist"
