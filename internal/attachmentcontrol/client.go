// Package attachmentcontrol contains typed compatibility projections for
// in-repository callers that are still being reduced onto the unified daemon
// attachment API. It owns no listener, lifecycle, catalog, or state.
package attachmentcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/claudeidentity"
	"github.com/antst/agent-sessions/internal/claudeprofile"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/qwenreadiness"
)

const (
	// GroupProtocolVersion is the compatibility projection of the shared group protocol.
	GroupProtocolVersion = federation.GroupProtocolVersion
	// SessionKindInteractive identifies an interactive managed attachment.
	SessionKindInteractive = federation.SessionKindInteractive
	// SessionKindLane identifies a lane managed attachment.
	SessionKindLane = federation.SessionKindLane
)

// ResolvePreferencesRequest is the legacy caller projection of daemon-owned attachment preferences.
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
	Qwen                   *federation.QwenSessionMetadata
}

// ResolvedPreferences contains the durable preference and its effective group projection.
type ResolvedPreferences struct {
	Preference      federation.SessionPreferences
	EffectiveGroups []string
}

// ManagedSession is a metadata-only view of one daemon-owned attachment.
type ManagedSession struct {
	Preference federation.SessionPreferences
	Name       string
	Live       bool
}

// AgentStatus is the compatibility projection of unified daemon status.
type AgentStatus struct {
	RuntimeVersion       string
	ProtocolVersion      int
	HostID               string
	HostName             string
	Hub                  string
	Connected            bool
	Capabilities         []string
	RegistryDir          string
	RuntimeDir           string
	StateDir             string
	ClaudeConfigEnvSet   bool
	ClaudeConfigEnvValue string
	ClaudeSecureConfig   string
	ClaudeSecureEnvSet   bool
}

// PeerRegistration carries connector observations for an already prepared attachment.
type PeerRegistration struct {
	Version              int
	SessionID            string
	AttachmentID         string
	Product              string
	Name                 string
	Status               string
	PermissionMode       string
	Cwd                  string
	PID                  int
	ProcStart            string
	Socket               string
	LifecyclePID         int
	LifecycleProcStart   string
	LifecycleRoot        string
	ClaudeConfigRoot     string
	ClaudeKeyBaseline    []ClaudeKeyBaselineEntry
	ClaudeKeyBaselineSet bool
	ClaudeSocketPath     string
	ClaudeSocketPathSet  bool
	QwenPreparation      *QwenPreparationPayload
	QwenCapabilityDigest string
	AdapterStrongStart   string
	LifecycleStrongStart string
	StartedAt            int64
}

// QwenPreparationPayload describes Qwen artifacts attested by the daemon adapter.
type QwenPreparationPayload struct {
	Version             int
	Profile             federation.QwenProfileIdentity
	CanonicalCwd        string
	LaunchPreference    string
	InitialModeRequest  string
	Input               qwenreadiness.ArtifactAttestation
	Events              qwenreadiness.ArtifactAttestation
	MCPCapabilityDigest string
}

// QwenArtifactAttestation is the shared Qwen readiness artifact identity.
type QwenArtifactAttestation = qwenreadiness.ArtifactAttestation

// ClaudeKeyBaselineEntry is the shared Claude native-key baseline identity.
type ClaudeKeyBaselineEntry = claudeidentity.KeyBaselineEntry

// LookupSessionPreferences resolves durable preferences by exact session identity.
func LookupSessionPreferences(_ string, sessionID string) (ResolvedPreferences, error) {
	managed, err := daemon.LookupManagedAttachment(context.Background(), daemon.AttachmentLookupRequest{SessionID: sessionID})
	if err != nil {
		return ResolvedPreferences{}, err
	}
	return managedPreferences(managed), nil
}

// LookupManagedSession resolves daemon-owned metadata by exact session identity.
func LookupManagedSession(_ string, sessionID string) (ManagedSession, error) {
	managed, err := daemon.LookupManagedAttachment(context.Background(), daemon.AttachmentLookupRequest{SessionID: sessionID})
	if err != nil {
		return ManagedSession{}, err
	}
	resolved := managedPreferences(managed)
	return ManagedSession{Preference: resolved.Preference, Name: managed.Name, Live: managed.Live}, nil
}

// ResolveSessionName resolves one unambiguous live product-local name.
func ResolveSessionName(_ string, product, name string) (string, error) {
	managed, err := daemon.LookupManagedAttachment(context.Background(), daemon.AttachmentLookupRequest{Product: product, Name: name})
	if err != nil {
		return "", err
	}
	return managed.SessionID, nil
}

// ResolveSessionPreferences returns the preferences fixed during daemon preparation.
func ResolveSessionPreferences(_ string, request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	return resolvePreferences(request)
}

// PreviewSessionPreferences validates preferences without creating attachment state.
func PreviewSessionPreferences(_ string, request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	return resolvePreferences(request)
}

func resolvePreferences(request ResolvePreferencesRequest) (ResolvedPreferences, error) {
	managed, err := daemon.LookupManagedAttachment(context.Background(), daemon.AttachmentLookupRequest{
		Product: request.Product, SessionID: request.SessionID,
	})
	if err != nil {
		return ResolvedPreferences{}, err
	}
	resolved := managedPreferences(managed)
	if request.GroupsSpecified && !equalStrings(request.Groups, resolved.Preference.ExplicitGroups) {
		return ResolvedPreferences{}, errors.New("attachment groups are fixed by daemon launch preparation")
	}
	return resolved, nil
}

func managedPreferences(managed daemon.ManagedAttachment) ResolvedPreferences {
	permission := managed.PermissionMode == "bypassPermissions" || managed.PermissionMode == "dontAsk"
	preference := federation.SessionPreferences{
		SessionID: managed.SessionID, Product: managed.Product, Kind: managed.Kind,
		ExplicitGroups: append([]string(nil), managed.Groups...), AlwaysApprove: permission,
	}
	return ResolvedPreferences{Preference: preference, EffectiveGroups: append([]string(nil), managed.Groups...)}
}

// ReadAgentStatus projects the already-running unified daemon's metadata-only status.
func ReadAgentStatus(_ string) (AgentStatus, error) {
	body, err := daemon.QueryAdmin(context.Background(), "runtime.status")
	if err != nil {
		return AgentStatus{}, err
	}
	var status daemon.HostStatusProjection
	if err := json.Unmarshal(body, &status); err != nil {
		return AgentStatus{}, err
	}
	result := AgentStatus{RuntimeVersion: status.RuntimeVersion, ProtocolVersion: federation.ProtocolVersion}
	result.RuntimeDir = filepath.Dir(status.Endpoint)
	source, sourceErr := claudeprofile.CurrentSource()
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_PEER_CLAUDE_CONFIG_DIR")); configured != "" {
		source, sourceErr = claudeprofile.SharedSource(configured)
	}
	if sourceErr == nil {
		result.RegistryDir = filepath.Join(source.ConfigRoot, "sessions")
		result.ClaudeConfigEnvSet = source.ConfigEnvSet
		result.ClaudeConfigEnvValue = source.ConfigEnvValue
		result.ClaudeSecureConfig = source.SecureConfig
		result.ClaudeSecureEnvSet = source.SecureEnvSet
	}
	if value, ok := status.Federation["host_id"].(string); ok {
		result.HostID = value
	}
	if value, ok := status.Federation["host_name"].(string); ok {
		result.HostName = value
	}
	if value, ok := status.Federation["hub_address"].(string); ok {
		result.Hub = value
	}
	result.Connected = status.Federation["state"] == string(federation.AgentConnected)
	return result, nil
}

// ResolveParentContext resolves an attested connector's exact parent attachment.
func ResolveParentContext(_ string, sessionID string) (federation.ParentContext, error) {
	return daemon.ResolveManagedParentContext(context.Background(), daemon.InheritedConnectorIdentity(""), sessionID)
}

// RouteAgentFrame delivers one attested connector frame through the unified daemon.
func RouteAgentFrame(_ string, _ string, frame federation.AgentFrame) (federation.AgentFrameResult, error) {
	return daemon.RouteManagedAgentFrame(context.Background(), daemon.InheritedConnectorIdentity(""), frame)
}

// RouteTerminalNotice routes a terminal notice to one exact session target.
func RouteTerminalNotice(_ string, _ string, target string, frame federation.AgentFrame) (federation.AgentFrameResult, error) {
	frame.Targets = []string{strings.TrimPrefix(strings.TrimSpace(target), "session:")}
	return daemon.RouteManagedAgentFrame(context.Background(), daemon.InheritedConnectorIdentity(""), frame)
}

// RegisterPeer verifies that the connector belongs to an already prepared attachment.
func RegisterPeer(_ string, registration PeerRegistration) (federation.Peer, error) {
	identity := daemon.InheritedConnectorIdentity(registration.Product)
	if identity.AttachmentID == "" {
		return federation.Peer{}, errors.New("peer registration requires a daemon-prepared attachment")
	}
	parent, err := daemon.ResolveManagedParentContext(context.Background(), identity, registration.SessionID)
	if err != nil {
		return federation.Peer{}, err
	}
	return federation.Peer{
		ID: parent.HostID + "/" + parent.SessionID, HostID: parent.HostID, SessionID: parent.SessionID,
		InstanceID: parent.InstanceID, Name: registration.Name, Entrypoint: parent.Product,
		Status: registration.Status, Cwd: registration.Cwd, Groups: append([]string(nil), parent.Groups...),
		PermissionMode: parent.PermissionMode, PeerProtocol: federation.GroupProtocolVersion,
	}, nil
}

// UnregisterPeer detaches only the caller's daemon-prepared attachment.
func UnregisterPeer(_ string, registration PeerRegistration) error {
	identity := daemon.InheritedConnectorIdentity(registration.Product)
	if identity.AttachmentID == "" {
		return nil
	}
	return daemon.DetachManagedAttachment(context.Background(), identity.AttachmentID, "connector_exit")
}

// PrepareClaudePeerSelection refuses the superseded connector-owned preparation path.
func PrepareClaudePeerSelection(string, PeerRegistration) error {
	return errors.New("claude preparation is owned by the daemon attachment adapter")
}

// PreparePeerLaunch refuses the superseded connector-owned preparation path.
func PreparePeerLaunch(string, PeerRegistration, ResolvePreferencesRequest, federation.SessionPreferences) (ResolvedPreferences, error) {
	return ResolvedPreferences{}, errors.New("peer preparation is owned by the daemon attachment adapter")
}

// PromoteClaudePeerSelection refuses the superseded connector-owned promotion path.
func PromoteClaudePeerSelection(string, PeerRegistration, ResolvePreferencesRequest, federation.SessionPreferences) (ResolvedPreferences, error) {
	return ResolvedPreferences{}, errors.New("claude selection is owned by the daemon attachment adapter")
}

// CancelPeerPreparation is a no-op because only the daemon can own preparation state.
func CancelPeerPreparation(string, PeerRegistration) error { return nil }

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
