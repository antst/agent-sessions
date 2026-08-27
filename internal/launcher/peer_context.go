package launcher

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/federator"
)

const (
	agentRuntimeDirEnv = "AGENT_SESSIONS_AGENT_RUNTIME_DIR"
	nativeRuntimeEnv   = "AGENT_SESSIONS_NATIVE_RUNTIME"
	peerSessionIDEnv   = "AGENT_SESSIONS_SESSION_ID"
	peerProductEnv     = "AGENT_SESSIONS_PRODUCT"
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

func persistentRuntimeEnvironment(environment []string) []string {
	blocked := map[string]bool{
		peerSessionIDEnv: true, peerProductEnv: true, remoteParentEnv: true,
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

func extractPeerLaunchContext(args []string, consumesNext func(string) bool) ([]string, peerLaunchContext, error) {
	forwarded := make([]string, 0, len(args))
	var context peerLaunchContext
	remaining := args
	for len(remaining) > 0 {
		argument := remaining[0]
		remaining = remaining[1:]
		if argument == "--" {
			forwarded = append(forwarded, argument)
			forwarded = append(forwarded, remaining...)
			break
		}
		switch {
		case argument == "-g", argument == "--group":
			if len(remaining) == 0 {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			group := remaining[0]
			remaining = remaining[1:]
			if strings.TrimSpace(group) == "" {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			context.groups = append(context.groups, group)
			context.groupsSpecified = true
		case strings.HasPrefix(argument, "-g="), strings.HasPrefix(argument, "--group="):
			_, value, _ := strings.Cut(argument, "=")
			if strings.TrimSpace(value) == "" {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			context.groups = append(context.groups, value)
			context.groupsSpecified = true
		case argument == "--parent-session" || strings.HasPrefix(argument, "--parent-session="):
			return nil, peerLaunchContext{}, usageError("--parent-session is internal; parent membership is assigned by an attested lane launch")
		case argument == "--inherit-groups":
			context.inheritParentGroups, context.inheritGroupsSpecified = true, true
		case argument == "--no-inherit-groups":
			context.inheritParentGroups, context.inheritGroupsSpecified = false, true
		case argument == "--no-yolo":
			context.forceNoYolo = true
		default:
			forwarded = append(forwarded, argument)
			if consumesNext(argument) && len(remaining) > 0 {
				forwarded = append(forwarded, remaining[0])
				remaining = remaining[1:]
			}
		}
	}
	return forwarded, context, nil
}

func peerEnvironment(environment []string, sessionID, product string) []string {
	environment = envutil.Set(environment, agentRuntimeDirEnv, agentRuntimeDir())
	environment = envutil.Set(environment, peerSessionIDEnv, sessionID)
	return envutil.Set(environment, peerProductEnv, product)
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
	return federator.DefaultRuntimeDir()
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func resolvedPeerPreferences(sessionID, product string) (federator.ResolvedPreferences, error) {
	return federator.ResolveSessionPreferences(agentRuntimeDir(), federator.ResolvePreferencesRequest{
		SessionID: sessionID, Product: product, Kind: federation.SessionKindInteractive,
	})
}

func resolvePeerLaunchContext(
	sessionID, product string,
	context peerLaunchContext,
	alwaysApprove, alwaysApproveSpecified bool,
) (federator.ResolvedPreferences, error) {
	return federator.ResolveSessionPreferences(agentRuntimeDir(), peerPreferenceRequest(
		sessionID, product, context, alwaysApprove, alwaysApproveSpecified,
	))
}

func previewPeerLaunchContext(
	sessionID, product string,
	context peerLaunchContext,
	alwaysApprove, alwaysApproveSpecified bool,
) (federator.ResolvedPreferences, federator.ResolvePreferencesRequest, error) {
	request := peerPreferenceRequest(sessionID, product, context, alwaysApprove, alwaysApproveSpecified)
	resolved, err := federator.PreviewSessionPreferences(agentRuntimeDir(), request)
	return resolved, request, err
}

func peerPreferenceRequest(
	sessionID, product string,
	context peerLaunchContext,
	alwaysApprove, alwaysApproveSpecified bool,
) federator.ResolvePreferencesRequest {
	return federator.ResolvePreferencesRequest{
		SessionID: sessionID, Product: product, Kind: federation.SessionKindInteractive,
		Groups: context.groups, GroupsSpecified: context.groupsSpecified,
		ParentSessionID: context.parentSession, ParentSpecified: context.parentSpecified,
		InheritParentGroups: context.inheritParentGroups, InheritGroupsSpecified: context.inheritGroupsSpecified,
		AlwaysApprove: alwaysApprove, AlwaysApproveSpecified: alwaysApproveSpecified,
	}
}
