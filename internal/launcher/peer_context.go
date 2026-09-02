package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
)

const (
	agentRuntimeDirEnv = "AGENT_SESSIONS_AGENT_RUNTIME_DIR"
	nativeRuntimeEnv   = "AGENT_SESSIONS_NATIVE_RUNTIME"
	peerSessionIDEnv   = "AGENT_SESSIONS_SESSION_ID"
	peerProductEnv     = "AGENT_SESSIONS_PRODUCT"
	peerSessionNameEnv = "AGENT_SESSIONS_SESSION_NAME"
	peerGroupsEnv      = "AGENT_SESSIONS_GROUPS"
	remoteParentEnv    = "AGENT_SESSIONS_REMOTE_PARENT_CONTEXT"
)

// peerLaunchContext is the product-neutral parent layer. Product launchers
// strip these options before invoking their native CLI and pass them to the
// host agent independently of target-specific arguments.
type peerLaunchContext struct {
	groups                 []string
	groupsSpecified        bool
	parentSession          string
	parentSpecified        bool
	inheritParentGroups    bool
	inheritGroupsSpecified bool
	forceNoYolo            bool
}

// extractPeerLaunchContext retains the baseline callback surface until the
// port-map deletion gate permits its removal. New production callers use the
// product-keyed scanner in options.go.
//
//nolint:unused // Required as a named baseline symbol until the deletion gate advances.
func extractPeerLaunchContext(args []string, consumesNext func(string) bool) ([]string, peerLaunchContext, error) {
	return scanPeerWrapperOptionsWithArity(args, consumesNext)
}

func persistentRuntimeEnvironment(environment []string) []string {
	blocked := map[string]bool{
		peerSessionIDEnv: true, peerProductEnv: true, peerSessionNameEnv: true, peerGroupsEnv: true, remoteParentEnv: true,
		"CODEX_THREAD_ID": true, grokLaunchTokenEnv: true, grokSessionIDEnv: true,
	}
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if !blocked[key] {
			result = append(result, value)
		}
	}
	return result
}

func peerEnvironment(environment []string, sessionID, product string) []string {
	environment = envutil.Set(environment, agentRuntimeDirEnv, agentRuntimeDir())
	environment = envutil.Set(environment, peerSessionIDEnv, sessionID)
	return envutil.Set(environment, peerProductEnv, product)
}

// daemonPeerEnvironment publishes only the attachment identity consumed by
// product hooks/connectors. The retired per-host federator runtime pointer is
// intentionally removed.
func daemonPeerEnvironment(environment []string, sessionID, product string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name != agentRuntimeDirEnv && name != peerSessionIDEnv && name != peerProductEnv && name != peerSessionNameEnv && name != peerGroupsEnv {
			filtered = append(filtered, entry)
		}
	}
	filtered = envutil.Set(filtered, peerSessionIDEnv, sessionID)
	return envutil.Set(filtered, peerProductEnv, product)
}

func liveReportEnvironment(environment []string, name string, groups []string) []string {
	encoded, _ := json.Marshal(groups)
	environment = envutil.Set(environment, peerSessionNameEnv, name)
	return envutil.Set(environment, peerGroupsEnv, string(encoded))
}

func (c peerLaunchContext) launchArguments(alwaysApprove bool, alwaysApproveSpecified bool) []string {
	groups := []byte("[]")
	if c.groups != nil {
		groups, _ = json.Marshal(c.groups)
	}
	runtimeDir := agentRuntimeDir()
	args := []string{
		"--agent-runtime-dir", runtimeDir,
		"--groups-json", string(groups),
		"--groups-specified", boolString(c.groupsSpecified),
		"--parent-session", c.parentSession,
		"--parent-specified", boolString(c.parentSpecified),
		"--inherit-parent-groups", boolString(c.inheritParentGroups),
		"--inherit-groups-specified", boolString(c.inheritGroupsSpecified),
		"--always-approve", boolString(alwaysApprove),
		"--always-approve-specified", boolString(alwaysApproveSpecified),
	}
	return args
}

func agentRuntimeDir() string {
	if value := strings.TrimSpace(os.Getenv(agentRuntimeDirEnv)); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		return filepath.Join(value, "agent-sessions")
	}
	return filepath.Join(os.TempDir(), "agent-sessions-"+strconv.Itoa(os.Getuid()))
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
