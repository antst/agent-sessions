package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

func runConnector(ctx context.Context, product string, output io.Writer) error {
	resolved, err := resolveConnectorProduct(product, os.Getenv)
	if err != nil {
		return err
	}
	product = resolved
	stateRoot := defaultStateRoot()
	endpoint, err := daemonpkg.ControlEndpoint(stateRoot)
	if err != nil {
		return err
	}
	refresher, err := newConnectorImageRefresher(os.Args, os.Environ())
	if err != nil {
		return fmt.Errorf("track installed connector image: %w", err)
	}
	// Normalize source-tree placeholders and stale pre-upgrade argv before the
	// vendor observes a ready MCP server. A successful refresh replaces this
	// process image and never returns; the replacement sees the exact current
	// identity and continues without another exec.
	if err := refresher.refresh(); err != nil {
		return fmt.Errorf("normalize installed connector image: %w", err)
	}
	if report, ok := connectorLiveReport(product, os.Getenv); ok {
		go maintainLivePresence(ctx, livePresenceEndpoint(stateRoot), report)
	}
	relay, err := sessiontools.NewMCPRelay(sessiontools.MCPRelayConfig{
		Product: product, Endpoint: endpoint,
		Refresh: func(context.Context) error {
			return refresher.refresh()
		},
		Generation: func(context.Context) (uint64, error) { return 1, nil },
		Attest: func(_ context.Context, params json.RawMessage) (sessiontools.ConnectorAttestation, error) {
			return attestConnector(product, params, os.Getenv)
		},
	})
	if err != nil {
		return err
	}
	return relay.Serve(ctx, os.Stdin, output)
}

func connectorLiveReport(product string, getenv func(string) string) (liveSessionReport, bool) {
	uuid := ""
	productSessionEnv := map[string]string{
		laneCandidateCodex: "CODEX_THREAD_ID", laneCandidateClaude: "CLAUDE_CODE_SESSION_ID", laneCandidateGrok: "AGENT_SESSIONS_GROK_SESSION_ID",
	}
	if name := productSessionEnv[product]; name != "" {
		uuid = strings.TrimSpace(getenv(name))
	}
	if uuid == "" {
		uuid = strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_ID"))
	}
	if uuid == "" {
		for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "AGENT_SESSIONS_GROK_SESSION_ID"} {
			if uuid = strings.TrimSpace(getenv(name)); uuid != "" {
				break
			}
		}
	}
	if uuid == "" {
		return liveSessionReport{}, false
	}
	name := strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_NAME"))
	if name == "" {
		name = uuid
	}
	var groups []string
	if encoded := strings.TrimSpace(getenv("AGENT_SESSIONS_GROUPS")); encoded != "" {
		if json.Unmarshal([]byte(encoded), &groups) != nil {
			return liveSessionReport{}, false
		}
	}
	return liveSessionReport{UUID: uuid, Name: name, Groups: uniqueStrings(groups), Product: product}, true
}

// resolveConnectorProduct keeps the repository-root Codex MCP manifest safe
// when another supported product discovers it from the working directory.
// Managed Claude, Grok, Qwen, and lane processes carry the daemon-minted
// product environment; a bare environment is the Codex App Server case. The
// later process/evidence attestation remains authoritative, so this routing
// hint can only select the adapter to prove and can never grant authority.
func resolveConnectorProduct(requested string, getenv func(string) string) (string, error) {
	if requested != "auto" {
		if _, ok := productcatalog.ByID(requested); !ok {
			return "", fmt.Errorf("connector product %q is unsupported", requested)
		}
		return requested, nil
	}
	product := strings.TrimSpace(getenv("AGENT_SESSIONS_PRODUCT"))
	if product == "" && strings.TrimSpace(getenv("AGENT_SESSIONS_GROK_SESSION_ID")) != "" {
		product = "grok"
	}
	if product == "" {
		product = "codex"
	}
	if _, ok := productcatalog.ByID(product); !ok {
		return "", fmt.Errorf("automatic connector product %q is unsupported", product)
	}
	return product, nil
}

func attestConnector(
	product string,
	params json.RawMessage,
	getenv func(string) string,
) (sessiontools.ConnectorAttestation, error) {
	threadID := strings.TrimSpace(getenv("AGENT_SESSIONS_SESSION_ID"))
	if product == "codex" {
		if nativeThread, err := sessiontools.StdioMCPThreadID(params); err == nil {
			threadID = nativeThread
		}
	}
	if threadID == "" {
		return sessiontools.ConnectorAttestation{}, sessiontools.ErrConnectorInactive
	}
	return sessiontools.ConnectorAttestation{
		AttachmentID: threadID,
		Evidence:     daemonpkg.NativeEvidence{ThreadID: threadID},
	}, nil
}
