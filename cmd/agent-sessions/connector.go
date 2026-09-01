package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
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
	relay, err := sessiontools.NewMCPRelay(sessiontools.MCPRelayConfig{
		Product: product, Endpoint: endpoint,
		Refresh: func(context.Context) error {
			return refresher.refresh()
		},
		Generation: func(context.Context) (uint64, error) { return 1, nil },
		Attest: func(callCtx context.Context, params json.RawMessage) (sessiontools.ConnectorAttestation, error) {
			return attestConnectorFromState(stateRoot, product, params, os.Getpid())
		},
	})
	if err != nil {
		return err
	}
	return relay.Serve(ctx, os.Stdin, output)
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

//nolint:gocyclo // Connector attestation deliberately fails closed across every product evidence shape.
func attestConnectorFromState(
	stateRoot, product string,
	params json.RawMessage,
	pid int,
) (sessiontools.ConnectorAttestation, error) {
	caller, ancestry, err := sessiontools.CaptureNativeAncestry(pid, 32)
	if err != nil {
		return sessiontools.ConnectorAttestation{}, sessiontools.ErrConnectorInactive
	}
	state, err := daemonpkg.OpenState(stateRoot, 16<<20)
	if err != nil {
		return sessiontools.ConnectorAttestation{}, sessiontools.ErrConnectorInactive
	}
	snapshot, err := state.Read()
	if err != nil {
		return sessiontools.ConnectorAttestation{}, sessiontools.ErrConnectorInactive
	}
	wantedThread := ""
	if product == "codex" {
		wantedThread, _ = sessiontools.StdioMCPThreadID(params)
	}
	matches := make([]daemonpkg.ManagedAttachment, 0, 1)
	for _, attachment := range snapshot.Catalog.Attachments {
		if attachment.Product != product || attachment.State != "attached" ||
			wantedThread != "" && attachment.NativeSessionID != wantedThread {
			continue
		}
		if connectorAncestryMatches(attachment, caller, ancestry) {
			matches = append(matches, attachment)
		}
	}
	if len(matches) != 1 {
		return sessiontools.ConnectorAttestation{}, sessiontools.ErrConnectorInactive
	}
	attachment := matches[0]
	evidence := daemonpkg.NativeEvidence{
		Process: caller, Ancestry: ancestry, ThreadID: attachment.NativeSessionID,
		SocketPath: attachment.Evidence.SocketPath, Executable: attachment.Evidence.Executable,
		RegistryPath: attachment.Evidence.RegistryPath, ArtifactPath: attachment.Evidence.ArtifactPath,
		ArtifactRevision: attachment.Evidence.ArtifactRevision,
	}
	return sessiontools.ConnectorAttestation{AttachmentID: attachment.ID, Evidence: evidence}, nil
}

func connectorAncestryMatches(
	attachment daemonpkg.ManagedAttachment,
	caller procinfo.Identity,
	ancestry []procinfo.Identity,
) bool {
	contains := func(wanted procinfo.Identity) bool {
		if sameProcessIdentity(caller, wanted) {
			return true
		}
		for _, candidate := range ancestry {
			if sameProcessIdentity(candidate, wanted) {
				return true
			}
		}
		return false
	}
	switch attachment.Product {
	case "grok":
		// Grok has shipped both process topologies: plugin MCPs hosted by the
		// private leader and plugin MCPs hosted directly by the attached TUI.
		// Both identities are daemon-corroborated and exact; accept either
		// ancestry without weakening the per-launch attachment boundary.
		return contains(attachment.Evidence.Process) ||
			len(attachment.Evidence.Ancestry) == 1 && contains(attachment.Evidence.Ancestry[0])
	case "codex":
		return len(attachment.Evidence.Ancestry) == 1 && contains(attachment.Evidence.Ancestry[0])
	case "claude", "qwen":
		return contains(attachment.Evidence.Process)
	default:
		return false
	}
}

func sameProcessIdentity(left, right procinfo.Identity) bool {
	return left.PID > 1 && left.PID == right.PID && strings.TrimSpace(left.Start) != "" && left.Start == right.Start &&
		(left.StrongStart == "" || right.StrongStart == "" || left.StrongStart == right.StrongStart)
}
