package federator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// AgentStatus is the live status returned by an agent's local control socket.
type AgentStatus struct {
	RuntimeVersion       string   `json:"runtime_version"`
	ProtocolVersion      int      `json:"protocol_version"`
	HostID               string   `json:"host_id"`
	HostName             string   `json:"host_name"`
	Hub                  string   `json:"hub"`
	Connected            bool     `json:"connected"`
	LocalPeers           int      `json:"local_peers"`
	RemotePeers          int      `json:"remote_peers"`
	RemoteHosts          int      `json:"remote_hosts"`
	Capabilities         []string `json:"capabilities,omitempty"`
	RegistryDir          string   `json:"registry_dir"`
	RuntimeDir           string   `json:"runtime_dir"`
	StateDir             string   `json:"state_dir"`
	ClaudeConfigEnvSet   bool     `json:"claude_config_env_set"`
	ClaudeConfigEnvValue string   `json:"claude_config_env_value,omitempty"`
	ClaudeSecureConfig   string   `json:"claude_secure_config,omitempty"`
	ClaudeSecureEnvSet   bool     `json:"claude_secure_env_set"`
}

// ResolvePreferencesRequest applies explicit launch overrides or restores the
// durable values when an override is omitted.
type ResolvePreferencesRequest struct {
	SessionID              string
	Product                string
	Kind                   string
	Groups                 []string
	GroupsSpecified        bool
	ParentSessionID        string
	ParentHostID           string
	ParentGroups           []string
	ParentSpecified        bool
	InheritParentGroups    bool
	InheritGroupsSpecified bool
	AlwaysApprove          bool
	AlwaysApproveSpecified bool
	Qwen                   *QwenSessionMetadata
}

// ResolvedPreferences is the durable preference plus computed effective groups.
type ResolvedPreferences struct {
	Preference      SessionPreferences
	EffectiveGroups []string
}

// ManagedSession is the non-live durable identity needed by a product wrapper
// to resume one exact managed interactive session. Live process details are
// returned only to reject a second attachment.
type ManagedSession struct {
	Preference SessionPreferences
	Name       string
	Live       bool
}

// LookupSessionPreferences returns one durable catalog row without requiring
// the product session to be live. Generic resume uses the stored product to
// select the correct native adapter.
func LookupSessionPreferences(runtimeDir, sessionID string) (ResolvedPreferences, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "session_lookup", Version: GroupProtocolVersion, SessionID: sessionID,
	})
	if err != nil {
		return ResolvedPreferences{}, err
	}
	if response.Type != "session_lookup" || response.Preference == nil {
		return ResolvedPreferences{}, errors.New("agent returned no session catalog row")
	}
	return ResolvedPreferences{Preference: *response.Preference, EffectiveGroups: response.Groups}, nil
}

// LookupManagedSession returns the durable preference/name pair and whether an
// exact local attachment is already live.
func LookupManagedSession(runtimeDir, sessionID string) (ManagedSession, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "session_lookup", Version: GroupProtocolVersion, SessionID: sessionID,
	})
	if err != nil {
		return ManagedSession{}, err
	}
	if response.Type != "session_lookup" || response.Preference == nil {
		return ManagedSession{}, errors.New("agent returned no managed session record")
	}
	return ManagedSession{
		Preference: *response.Preference, Name: response.Name, Live: len(response.Peers) != 0,
	}, nil
}

// ResolveSessionName maps one unique durable product peer name to its stable
// session ID. A unique live registration wins over historical records; all
// other collisions require the caller to use an exact session ID.
func ResolveSessionName(runtimeDir, product, name string) (string, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "session_name_lookup", Version: GroupProtocolVersion, Product: product, Name: name,
	})
	if err != nil {
		return "", err
	}
	if response.Type != "session_name_lookup" || response.SessionID == "" {
		return "", errors.New("agent returned no session name match")
	}
	return response.SessionID, nil
}

// DoctorOptions selects the hub and local registry inspected by Doctor.
type DoctorOptions struct {
	Hub             string
	ClaudeConfigDir string
	RuntimeDir      string
}

// DoctorReport describes whether the local host is ready to federate peers.
type DoctorReport struct {
	OK                       bool         `json:"ok"`
	Summary                  string       `json:"summary"`
	RuntimeVersion           string       `json:"runtime_version"`
	ProtocolVersion          int          `json:"protocol_version"`
	Hub                      string       `json:"hub"`
	HubReachable             bool         `json:"hub_reachable"`
	HubCompatible            bool         `json:"hub_compatible"`
	RegistryDir              string       `json:"registry_dir"`
	RegistryReady            bool         `json:"registry_ready"`
	LiveLocalRecords         int          `json:"live_local_records"`
	MessageableLocalPeers    int          `json:"messageable_local_peers"`
	UnmessageableLiveRecords int          `json:"unmessageable_live_records"`
	AgentRunning             bool         `json:"agent_running"`
	Agent                    *AgentStatus `json:"agent,omitempty"`
}

// Doctor checks hub compatibility, the local registry, and an optional running agent.
//
//nolint:gocyclo // Doctor intentionally aggregates independent readiness evidence in one report.
func Doctor(ctx context.Context, options DoctorOptions) DoctorReport {
	if options.ClaudeConfigDir == "" {
		options.ClaudeConfigDir = DefaultClaudeConfigDir()
	}
	if options.RuntimeDir == "" {
		options.RuntimeDir = DefaultRuntimeDir()
	}
	report := DoctorReport{
		RuntimeVersion:  RuntimeVersion,
		ProtocolVersion: ProtocolVersion,
		Hub:             options.Hub,
		RegistryDir:     filepath.Join(options.ClaudeConfigDir, "sessions"),
	}
	report.HubReachable, report.HubCompatible = probeHub(ctx, options.Hub)
	report.RegistryReady, report.LiveLocalRecords, report.MessageableLocalPeers,
		report.UnmessageableLiveRecords = inspectRegistry(report.RegistryDir)
	if status, err := ReadAgentStatus(options.RuntimeDir); err == nil {
		report.AgentRunning = true
		report.Agent = &status
	}
	report.OK = (options.Hub == "" || report.HubCompatible) && report.RegistryReady &&
		report.UnmessageableLiveRecords == 0 && report.AgentRunning
	switch {
	case !report.AgentRunning:
		report.Summary = "host agent is not running"
	case options.Hub != "" && !report.HubReachable:
		report.Summary = "hub is unreachable"
	case options.Hub != "" && !report.HubCompatible:
		report.Summary = "hub protocol is incompatible"
	case !report.RegistryReady:
		report.Summary = "claude session registry is unavailable"
	case report.UnmessageableLiveRecords > 0:
		report.Summary = "live sessions without messaging sockets were found"
	default:
		if options.Hub == "" {
			report.Summary = "ready (local routing only)"
		} else {
			report.Summary = "ready"
		}
	}
	return report
}

// ReadAgentStatus reads status from the agent owning runtimeDir.
func ReadAgentStatus(runtimeDir string) (AgentStatus, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return AgentStatus{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := newWireConn(conn).Send(Message{Type: "status"}); err != nil {
		return AgentStatus{}, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return AgentStatus{}, err
	}
	if response.Type != "status" {
		return AgentStatus{}, fmt.Errorf("agent status failed: %s", response.Error)
	}
	var status AgentStatus
	if err := json.Unmarshal(response.Frame, &status); err != nil {
		return AgentStatus{}, err
	}
	return status, nil
}

// ResolveSessionPreferences updates or restores one durable session catalog entry.
func ResolveSessionPreferences(runtimeDir string, request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	return requestSessionPreferences(runtimeDir, "session_preferences", request)
}

// PreviewSessionPreferences applies the same validation and inheritance rules
// without adopting or modifying the durable catalog row.
func PreviewSessionPreferences(runtimeDir string, request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	return requestSessionPreferences(runtimeDir, "session_preferences_preview", request)
}

func requestSessionPreferences(runtimeDir, requestType string, request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return ResolvedPreferences{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := newWireConn(conn).Send(preferenceMessage(requestType, request)); err != nil {
		return ResolvedPreferences{}, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return ResolvedPreferences{}, err
	}
	if response.Type != requestType || response.Preference == nil {
		return ResolvedPreferences{}, fmt.Errorf("resolve session preferences failed: %s", response.Error)
	}
	return ResolvedPreferences{Preference: *response.Preference, EffectiveGroups: response.Groups}, nil
}

func preferenceMessage(requestType string, request ResolvePreferencesRequest) Message {
	return Message{
		Type: requestType, Version: GroupProtocolVersion,
		SessionID: request.SessionID, Product: request.Product,
		SessionKind: request.Kind,
		Groups:      request.Groups, GroupsSpecified: request.GroupsSpecified,
		ParentSessionID: request.ParentSessionID, ParentSpecified: request.ParentSpecified,
		ParentHostID: request.ParentHostID, ParentGroups: request.ParentGroups,
		InheritParentGroups: request.InheritParentGroups, InheritGroupsSpecified: request.InheritGroupsSpecified,
		AlwaysApprove: request.AlwaysApprove, AlwaysApproveSpecified: request.AlwaysApproveSpecified,
		QwenSession: request.Qwen,
	}
}

// RouteAgentFrame sends one product-neutral request through the local host agent.
func RouteAgentFrame(runtimeDir, sourceSessionID string, frame AgentFrame) (AgentFrameResult, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return AgentFrameResult{}, err
	}
	conn, err := net.DialTimeout("unix", filepath.Join(runtimeDir, "agent.sock"), time.Second)
	if err != nil {
		return AgentFrameResult{}, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(wireWriteTimeout + 5*time.Second))
	if err := newWireConn(conn).Send(Message{Type: "agent_frame", SourceSessionID: sourceSessionID, Frame: body}); err != nil {
		return AgentFrameResult{}, err
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return AgentFrameResult{}, err
	}
	if response.Type != "agent_frame_result" {
		return AgentFrameResult{}, fmt.Errorf("agent route failed: %s", response.Error)
	}
	var result AgentFrameResult
	if err := json.Unmarshal(response.Frame, &result); err != nil {
		return AgentFrameResult{}, err
	}
	return result, nil
}

// RouteTerminalNotice routes one durable child-to-parent pointer after the
// child's live adapter has disappeared. The host agent derives and verifies the
// exact parent from the session catalog; callers cannot choose another peer.
func RouteTerminalNotice(runtimeDir, sourceSessionID, target string, frame AgentFrame) (AgentFrameResult, error) {
	if runtimeDir == "" {
		runtimeDir = DefaultRuntimeDir()
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return AgentFrameResult{}, err
	}
	response, err := requestAgentControl(runtimeDir, Message{
		Type: "terminal_notice", Version: GroupProtocolVersion, SourceSessionID: sourceSessionID,
		TargetID: target, Frame: body,
	})
	if err != nil {
		return AgentFrameResult{}, err
	}
	if response.Type != "agent_frame_result" {
		return AgentFrameResult{}, fmt.Errorf("route terminal notice failed: %s", response.Error)
	}
	var result AgentFrameResult
	if err := json.Unmarshal(response.Frame, &result); err != nil {
		return AgentFrameResult{}, err
	}
	return result, nil
}

func probeHub(ctx context.Context, address string) (bool, bool) {
	if address == "" {
		return false, false
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return false, false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := newWireConn(conn).Send(Message{Type: "probe", Version: ProtocolVersion}); err != nil {
		return true, false
	}
	var response Message
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return true, false
	}
	return true, response.Type == "probe_ok" && response.Version == ProtocolVersion
}

func inspectRegistry(registryDir string) (ready bool, live, messageable, unmessageable int) {
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		return false, 0, 0, 0
	}
	for _, entry := range entries {
		pid := parsePID(entry.Name())
		if pid == 0 || !processLive(pid) {
			continue
		}
		// #nosec G304 -- entry.Name came from ReadDir on the configured registry.
		body, err := os.ReadFile(filepath.Join(registryDir, entry.Name()))
		if err != nil {
			continue
		}
		var record registryRecord
		if json.Unmarshal(body, &record) != nil {
			continue
		}
		if record.ProcStart != "" {
			currentStart := processStart(pid)
			if currentStart != "" && currentStart != record.ProcStart {
				continue
			}
		}
		if record.FederatedBy != "" {
			continue
		}
		live++
		if record.MessagingSocketPath != "" && probeUnix(record.MessagingSocketPath, 100*time.Millisecond) {
			messageable++
		} else {
			unmessageable++
		}
	}
	return true, live, messageable, unmessageable
}
