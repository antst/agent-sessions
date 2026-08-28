package bridge

import (
	"context"
	"os"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

func currentDaemonConnectorIdentity() daemonpkg.LocalControlIdentity {
	product := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT"))
	return daemonpkg.InheritedConnectorIdentity(product)
}

func resolveDaemonParentContext(_ string, sessionID string) (federation.ParentContext, error) {
	return daemonpkg.ResolveManagedParentContext(context.Background(), currentDaemonConnectorIdentity(), sessionID)
}

func routeDaemonAgentFrame(_ string, _ string, frame federation.AgentFrame) (federation.AgentFrameResult, error) {
	return daemonpkg.RouteManagedAgentFrame(context.Background(), currentDaemonConnectorIdentity(), frame)
}

func daemonControlAvailable() bool {
	_, err := daemonpkg.QueryAdmin(context.Background(), "runtime.status")
	return err == nil
}
