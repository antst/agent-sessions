SHELL := /bin/bash

CODEX ?= codex
CLAUDE ?= claude
# Ignore an inherited GROK environment variable: a long-lived peer may have
# pinned its own launcher, but that must not disable discovery for a later
# install. An explicit make command-line GROK=/absolute/path pins one candidate.
GROK_INPUT_ORIGIN := $(origin GROK)
GROK ?=
GROK_PEER ?= $(BIN_DIR)/grok-peer
GROK_PLUGIN_VERIFY ?= $(BIN_DIR)/agent-session-runtime
GROK_PEER_ENV = $(if $(and $(findstring command line,$(GROK_INPUT_ORIGIN)),$(strip $(GROK))),GROK_PEER_GROK_BIN="$(GROK)")
GOLANGCI_LINT ?= golangci-lint
PREFIX ?= $(HOME)/.local
INSTALL_ROOT ?= $(PREFIX)/libexec/agent-sessions
MARKETPLACE ?= agent-sessions
LEGACY_MARKETPLACE ?= codex-messaging
PLUGIN ?= claude-code-peer
LEGACY_PLUGIN ?= $(PLUGIN)@personal
CLAUDE_MARKETPLACE ?= agent-sessions
LEGACY_CLAUDE_MARKETPLACE ?= codex-messaging
CLAUDE_PLUGIN ?= codex-peer
CLAUDE_SCOPE ?= user
CLAUDE_PLUGIN_VERSION := $(shell sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' claude/.claude-plugin/plugin.json | head -1)
CLAUDE_RELEASE_ROOT ?= $(PREFIX)/share/agent-sessions/claude-marketplaces
CLAUDE_STAGED_ROOT := $(CLAUDE_RELEASE_ROOT)/$(CLAUDE_PLUGIN_VERSION)
CLAUDE_MARKETPLACE_ROOT ?= $(CLAUDE_STAGED_ROOT)
GROK_PLUGIN_ROOT ?= $(INSTALL_ROOT)/grok
GROK_PLUGIN_NAME := agent-sessions
GROK_USER_PLUGIN_ROOT ?= $(HOME)/.grok/plugins/$(GROK_PLUGIN_NAME)
START_RUNTIME ?= 1
PEER_FEDERATOR_CONFIG_DIR ?= $(HOME)/.config/peer-federator
PEER_FEDERATOR_DOC_ROOT ?= $(PREFIX)/share/doc/peer-federator
USER_SYSTEMD_DIR ?= $(HOME)/.config/systemd/user
USER_LAUNCHD_DIR ?= $(HOME)/Library/LaunchAgents
PEER_FEDERATOR_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || cat deploy/peer-federator/VERSION)
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
BINARY_NAMES := agent-session-runtime codex-peer codex-peer-lane claude-peer-lane grok-peer peer-federator

.PHONY: all lint test test-race build build-peer-federator install-preflight grok-install-preflight install dev-install reinstall \
	stage-claude validate-claude install-claude dev-install-claude validate-grok install-grok \
	dev-install-grok install-all dev-install-all \
	install-peer-federator install-systemd-user-files install-systemd-user \
	install-launchd-user-files install-launchd-user repair-projection clean

all: lint test build

lint:
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
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(BIN_DIR)/agent-session-runtime" ./cmd/agent-session-runtime; \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(BIN_DIR)/codex-peer" ./cmd/codex-peer; \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(BIN_DIR)/codex-peer-lane" ./cmd/codex-peer-lane; \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(BIN_DIR)/claude-peer-lane" ./cmd/claude-peer-lane; \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='-s -w' -o "$(BIN_DIR)/grok-peer" ./cmd/grok-peer; \
		CGO_ENABLED=0 GOOS="$(GOOS)" GOARCH="$(GOARCH)" go build -trimpath -ldflags='$(PEER_FEDERATOR_LDFLAGS)' -o "$(BIN_DIR)/peer-federator" ./cmd/peer-federator; \
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
				'A managed Grok peer is still running. Exit every grok-peer TUI normally.' \
				'Its private leader and ACP observer stop automatically with that TUI.' \
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
			'A managed Grok peer is still running. Exit every grok-peer TUI normally.' \
			'Its private leader and ACP observer stop automatically with that TUI.' \
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
		"$(INSTALL_ROOT)/scripts" \
		"$(INSTALL_ROOT)/skills"
	cp -R .agents .codex-plugin deploy docs grok hooks scripts skills "$(INSTALL_ROOT)/"
	cp .mcp.json README.md "$(INSTALL_ROOT)/"
	mkdir -p "$(INSTALL_ROOT)/bin/$(PLATFORM)"
	@for binary in $(BINARY_NAMES); do cp "$(BIN_DIR)/$$binary" "$(INSTALL_ROOT)/bin/$(PLATFORM)/$$binary"; done
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/agent-session-runtime" "$(PREFIX)/bin/agent-session-runtime"
	rm -f -- "$(PREFIX)/bin/codex-messaging"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/codex-peer" "$(PREFIX)/bin/codex-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/codex-peer-lane" "$(PREFIX)/bin/codex-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/claude-peer-lane" "$(PREFIX)/bin/claude-peer-lane"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/grok-peer" "$(PREFIX)/bin/grok-peer"
	ln -sfn "$(abspath $(INSTALL_ROOT))/bin/$(PLATFORM)/peer-federator" "$(PREFIX)/bin/peer-federator"
	@if $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(MARKETPLACE)"; \
	fi
	@if [[ "$(MARKETPLACE)" != personal ]] && $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(LEGACY_PLUGIN)"'; then \
		$(CODEX) plugin remove "$(LEGACY_PLUGIN)"; \
	fi
	@if $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(MARKETPLACE)"; \
	fi
	$(CODEX) plugin marketplace add "$(INSTALL_ROOT)"
	$(CODEX) plugin add "$(PLUGIN)@$(MARKETPLACE)"
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(START_RUNTIME)" == "1" ]]; then \
		CODEX_PEER_CODEX_BIN="$(CODEX)" "$(INSTALL_ROOT)/bin/$(PLATFORM)/agent-session-runtime" bootstrap; \
	fi

dev-install: install-preflight
	mkdir -p "$(PREFIX)/bin"
	ln -sfn "$(abspath $(BIN_DIR))/agent-session-runtime" "$(PREFIX)/bin/agent-session-runtime"
	rm -f -- "$(PREFIX)/bin/codex-messaging"
	ln -sfn "$(abspath $(BIN_DIR))/codex-peer" "$(PREFIX)/bin/codex-peer"
	ln -sfn "$(abspath $(BIN_DIR))/codex-peer-lane" "$(PREFIX)/bin/codex-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/claude-peer-lane" "$(PREFIX)/bin/claude-peer-lane"
	ln -sfn "$(abspath $(BIN_DIR))/grok-peer" "$(PREFIX)/bin/grok-peer"
	ln -sfn "$(abspath $(BIN_DIR))/peer-federator" "$(PREFIX)/bin/peer-federator"
	@if $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(MARKETPLACE)"; \
	fi
	@if [[ "$(MARKETPLACE)" != personal ]] && $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(LEGACY_PLUGIN)"'; then \
		$(CODEX) plugin remove "$(LEGACY_PLUGIN)"; \
	fi
	@if $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(MARKETPLACE)"; \
	fi
	$(CODEX) plugin marketplace add "$(CURDIR)"
	$(CODEX) plugin add "$(PLUGIN)@$(MARKETPLACE)"
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin list --json | \
		grep -Eq '"pluginId"[[:space:]]*:[[:space:]]*"$(PLUGIN)@$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin remove "$(PLUGIN)@$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(LEGACY_MARKETPLACE)" != "$(MARKETPLACE)" ]] && $(CODEX) plugin marketplace list --json | \
		grep -Eq '"name"[[:space:]]*:[[:space:]]*"$(LEGACY_MARKETPLACE)"'; then \
		$(CODEX) plugin marketplace remove "$(LEGACY_MARKETPLACE)"; \
	fi
	@if [[ "$(START_RUNTIME)" == "1" ]]; then \
		CODEX_PEER_CODEX_BIN="$(CODEX)" "$(BIN_DIR)/agent-session-runtime" bootstrap; \
	fi

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
	@if [[ "$(LEGACY_CLAUDE_MARKETPLACE)" != "$(CLAUDE_MARKETPLACE)" ]] && $(CLAUDE) plugin list --json | \
		grep -Fq '"$(CLAUDE_PLUGIN)@$(LEGACY_CLAUDE_MARKETPLACE)"'; then \
		$(CLAUDE) plugin uninstall --scope "$(CLAUDE_SCOPE)" "$(CLAUDE_PLUGIN)@$(LEGACY_CLAUDE_MARKETPLACE)"; \
	fi
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
	@plugin_list="$$( $(GROK_PEER_ENV) "$(GROK_PEER)" plugin list --json )" || { \
		status=$$?; \
		printf 'Cannot inspect existing Grok plugin registrations (exit %s).\n' "$$status" >&2; \
		exit "$$status"; \
	}; \
	if printf '%s\n' "$$plugin_list" | awk -v name='$(GROK_PLUGIN_NAME)' 'BEGIN { RS = "}" } \
			index($$0, "\"name\"") && index($$0, "\"" name "\"") && index($$0, "\"repo_key\"") { found = 1 } \
			END { exit(found ? 0 : 1) }'; then \
		# Deliberately omit --confirm: fail closed if that name belongs to a \
		# multi-plugin repository instead of deleting its unrelated plugins. \
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

install-all: install
	$(MAKE) install-claude
	$(MAKE) install-grok

dev-install-all: dev-install
	$(MAKE) dev-install-claude
	$(MAKE) dev-install-grok

repair-projection:
	@test -n "$(THREAD_ID)" || { printf 'usage: make repair-projection THREAD_ID=<id> [APPLY=1]\n' >&2; exit 2; }
	./scripts/repair-history-projection $(if $(filter 1,$(APPLY)),--apply,) "$(THREAD_ID)"

clean:
	rm -rf -- "$(CURDIR)/bin" "$(CURDIR)/dist"
