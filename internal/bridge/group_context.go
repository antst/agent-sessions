package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
)

const (
	agentRuntimeDirEnvironment = "AGENT_SESSIONS_AGENT_RUNTIME_DIR"
	peerSessionIDEnvironment   = "AGENT_SESSIONS_SESSION_ID"
	remoteParentEnvironment    = "AGENT_SESSIONS_REMOTE_PARENT_CONTEXT"
)

// laneGroupOptions is the product-neutral portion of every target lane CLI.
// Explicit child groups and parent-group inheritance are separate decisions.
type laneGroupOptions struct {
	groups                 []string
	groupsSpecified        bool
	inheritParentGroups    bool
	inheritGroupsSpecified bool
	parentSessionID        string
	parentHostID           string
	parentAgentRuntimeDir  string
	parentGroups           []string
	parentProduct          string
	parentErr              error
}

func laneCollectionPointer(product, sessionID, parentHostID, _ string, groups []string) string {
	local := fmt.Sprintf("%s-peer-lane wait %s", product, sessionID)
	if parentHostID == "" {
		return local
	}
	suffix := "/" + sessionID
	for _, group := range groups {
		if strings.HasPrefix(group, "session:") && strings.HasSuffix(group, suffix) {
			hostID := strings.TrimSuffix(strings.TrimPrefix(group, "session:"), suffix)
			if hostID != "" && hostID != parentHostID {
				return fmt.Sprintf("agent-sessions lane --host %s --product %s -- wait %s", hostID, product, sessionID)
			}
			break
		}
	}
	return local
}

func deliverGroupedLaneNotice(sourceSessionID, target, messageID, content string) error {
	if !laneAgentConfigured() {
		return errors.New("host agent is not configured")
	}
	target = strings.TrimPrefix(strings.TrimSpace(target), "session:")
	if target == "" {
		return errors.New("lane notice target is empty")
	}
	if messageID == "" {
		messageID = sessionKey(sourceSessionID + "\x00" + content)
	}
	result, err := routeDaemonAgentFrame(laneAgentRuntimeDir(), sourceSessionID, federation.AgentFrame{
		Version: federation.AgentFrameVersion, Type: "send", MessageID: messageID,
		Targets: []string{target}, Content: content, SentAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil && strings.Contains(err.Error(), "source session is not a live registered peer") {
		result, err = routeDaemonAgentFrame(laneAgentRuntimeDir(), sourceSessionID, federation.AgentFrame{
			Version: federation.AgentFrameVersion, Type: "send", MessageID: messageID,
			Targets: []string{target}, Content: content, SentAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	if err != nil {
		return err
	}
	if len(result.Deliveries) != 1 || result.Deliveries[0].Status != "accepted" {
		if len(result.Deliveries) == 1 && result.Deliveries[0].Error != "" {
			return errors.New(result.Deliveries[0].Error)
		}
		return errors.New("host agent did not accept the lane notice")
	}
	return nil
}

type laneGroupState struct {
	Groups                []string `json:"groups,omitempty"`
	ExplicitGroups        []string `json:"explicitGroups,omitempty"`
	ParentSessionID       string   `json:"parentSessionId,omitempty"`
	ParentHostID          string   `json:"parentHostId,omitempty"`
	ParentAgentRuntimeDir string   `json:"parentAgentRuntimeDir,omitempty"`
	InheritParentGroups   bool     `json:"inheritParentGroups,omitempty"`
}

func laneAgentRuntimeDir() string {
	if value := strings.TrimSpace(os.Getenv(agentRuntimeDirEnvironment)); value != "" {
		return value
	}
	return ""
}

func laneAgentConfigured() bool {
	return daemonControlAvailable()
}

// applyAgentParentContext replaces legacy registry inference when a peer
// launcher exported its stable session identity. The host agent is the sole
// authority for its product, effective groups, and live lifecycle identity.
func applyAgentParentContext(groups laneGroupOptions, owner *laneOwner) laneGroupOptions {
	if body := strings.TrimSpace(os.Getenv(remoteParentEnvironment)); body != "" {
		// Remote parent context is authoritative for the parent layer and never
		// grants a destination-local lifecycle owner. Clear any ancestry hint
		// inherited from the host process before parsing the attested context.
		*owner = laneOwner{}
		var parent federation.ParentContext
		if json.Unmarshal([]byte(body), &parent) != nil || parent.HostID == "" || parent.SessionID == "" {
			groups.parentErr = errors.New("invalid remote lane parent context")
			return groups
		}
		groups.parentSessionID, groups.parentHostID = parent.SessionID, parent.HostID
		groups.parentAgentRuntimeDir = parent.AgentRuntimeDir
		groups.parentGroups = append([]string(nil), parent.Groups...)
		groups.parentProduct = parent.Product
		return groups
	}
	sessionID := strings.TrimSpace(os.Getenv(peerSessionIDEnvironment))
	claimedProduct := strings.TrimSpace(os.Getenv("AGENT_SESSIONS_PRODUCT"))
	// A product-specific ancestry proof is stronger than ambient process
	// environment. In particular, Grok tool shells can retain the outer Codex
	// process's CODEX_THREAD_ID while deliberately exposing only their private
	// Grok launch capability. Do not let that stale ambient ID replace the
	// corroborated immediate parent.
	if owner.SessionID != "" {
		sessionID = owner.SessionID
	} else if nativeThreadID := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); nativeThreadID != "" &&
		(claimedProduct == "" || claimedProduct == "codex") {
		sessionID = nativeThreadID
	}
	if sessionID == "" {
		return groups
	}
	parent, err := resolveDaemonParentContext(laneAgentRuntimeDir(), sessionID)
	if err != nil {
		groups.parentErr = err
		return groups
	}
	if claimedProduct != "" && claimedProduct != parent.Product {
		groups.parentErr = errors.New("host-agent parent product does not match the launch context")
		return groups
	}
	if !corroborateAgentParentContext(parent, *owner, os.Getpid()) {
		groups.parentErr = errors.New("host-agent parent context is not corroborated by this process ancestry")
		return groups
	}
	groups.parentSessionID = parent.SessionID
	groups.parentHostID = parent.HostID
	groups.parentAgentRuntimeDir = parent.AgentRuntimeDir
	groups.parentGroups = append([]string(nil), parent.Groups...)
	groups.parentProduct = parent.Product
	*owner = laneOwner{
		PID: parent.PID, ProcStart: parent.ProcStart, SessionID: parent.SessionID,
		PermissionMode: parent.PermissionMode,
	}
	return groups
}

func corroborateAgentParentContext(parent federation.ParentContext, inferred laneOwner, startPID int) bool {
	if inferred.SessionID != "" {
		return inferred.SessionID == parent.SessionID && inferred.PID == parent.AdapterPID &&
			inferred.ProcStart == parent.AdapterProcStart && processHasAncestor(inferred.PID, parent.PID)
	}
	if parent.Product != "codex" || strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")) != parent.SessionID {
		return false
	}
	hostPID := findCodexHostOwnerPID(startPID)
	if hostPID <= 1 || !processHasAncestor(startPID, hostPID) {
		return false
	}
	state, err := readOwnNativeState(resolveNativePaths(), parent.SessionID)
	return err == nil && intValue(state["pid"]) == parent.AdapterPID &&
		stringValue(state["procStart"]) == parent.AdapterProcStart &&
		samePath(stringValue(state["socketPath"]), parent.AdapterSocket)
}

func resolveLaneGroupState(
	sessionID, product string,
	groups laneGroupOptions,
	alwaysApprove, alwaysApproveSpecified bool,
) (laneGroupState, bool, error) {
	if groups.parentErr != nil {
		return laneGroupState{}, false, groups.parentErr
	}
	if sessionID == "" {
		return laneGroupState{}, false, errors.New("lane session id is empty")
	}
	if !laneAgentConfigured() {
		return laneGroupState{}, false, errors.New("grouped lanes require a running host agent")
	}
	effective := append([]string(nil), groups.groups...)
	if groups.inheritParentGroups {
		effective = append(effective, groups.parentGroups...)
	}
	effective = normalizeDaemonLaneGroups(effective)
	permission := alwaysApprove
	if !alwaysApproveSpecified {
		if managed, err := daemonpkg.LookupManagedAttachment(context.Background(), daemonpkg.AttachmentLookupRequest{Product: product, SessionID: sessionID}); err == nil {
			permission = managed.PermissionMode == "bypassPermissions" || managed.PermissionMode == "dontAsk"
		}
	}
	return laneGroupState{
		Groups:                effective,
		ExplicitGroups:        append([]string(nil), groups.groups...),
		ParentSessionID:       groups.parentSessionID,
		ParentHostID:          groups.parentHostID,
		ParentAgentRuntimeDir: groups.parentAgentRuntimeDir,
		InheritParentGroups:   groups.inheritParentGroups,
	}, permission, nil
}

func normalizeDaemonLaneGroups(groups []string) []string {
	seen := make(map[string]struct{}, len(groups))
	result := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		result = append(result, group)
	}
	sort.Strings(result)
	return result
}

func validateLaneGroupCommand(command string, groups laneGroupOptions) error {
	if (groups.groupsSpecified || groups.inheritGroupsSpecified) &&
		!containsString([]string{"run", "start", "resume"}, command) {
		return errors.New("--group and group inheritance options are valid only for run, start, and resume")
	}
	return nil
}
